package openai

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/logger"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	hosttypes "github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

func OpenaiTTSHandler(c *gin.Context, resp *http.Response, info *relaycommon.RelayInfo) (*types.NewAPIError, *dto.Usage) {
	// the status code has been judged before, if there is a body reading failure,
	// it should be regarded as a non-recoverable error, so it should not return err for external retry.
	// Analogous to nginx's load balancing, it will only retry if it can't be requested or
	// if the upstream returns a specific status code, once the upstream has already written the header,
	// the subsequent failure of the response body should be regarded as a non-recoverable error,
	// and can be terminated directly.
	defer service.CloseResponseBodyGracefully(resp)
	if info.BillingSource == service.BillingSourceCreditPacks {
		if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
			return openaiCreditSpeechStreamHandler(c, resp, info)
		}
		if err := observeSpeechCreditHeaders(info, resp.Header); err != nil {
			return types.NewError(err, types.ErrorCodeBadResponseBody, types.ErrOptionWithSkipRetry()), nil
		}
		// A binary response cannot be parsed as SSE merely because the client
		// requested that format. Preserve its bytes and use audio metering.
		info.IsStream = false
		info.StreamStatus = relaycommon.NewStreamStatus()
	}
	usage := &dto.Usage{}
	usage.PromptTokens = info.GetEstimatePromptTokens()
	usage.TotalTokens = info.GetEstimatePromptTokens()
	for k, v := range resp.Header {
		if !service.ShouldCopyUpstreamHeader(c, k, v) {
			continue
		}
		c.Writer.Header().Set(k, v[0])
	}
	c.Writer.WriteHeader(resp.StatusCode)

	if info.IsStream {
		helper.StreamScannerHandler(c, resp, info, func(data string, sr *helper.StreamResult) {
			if service.SundaySearch(data, "usage") {
				var simpleResponse dto.SimpleResponse
				if err := common.Unmarshal([]byte(data), &simpleResponse); err != nil {
					logger.LogError(c, err.Error())
					sr.Error(err)
				} else if simpleResponse.Usage.TotalTokens != 0 {
					usage.PromptTokens = simpleResponse.Usage.InputTokens
					usage.CompletionTokens = simpleResponse.OutputTokens
					usage.TotalTokens = simpleResponse.TotalTokens
				}
			}
			if err := helper.StringData(c, data); err != nil {
				sr.Error(err)
			}
		})
	} else {
		common.SetContextKey(c, constant.ContextKeyLocalCountTokens, true)
		// 读取响应体到缓冲区
		bodyBytes, err := io.ReadAll(resp.Body)
		if err != nil {
			logger.LogError(c, fmt.Sprintf("failed to read TTS response body: %v", err))
			c.Writer.WriteHeaderNow()
			if info.BillingSource == service.BillingSourceCreditPacks {
				info.StreamStatus.SetEndReason(relaycommon.StreamEndReasonScannerErr, err)
				info.StreamStatus.MarkIncomplete("upstream_body_read_error")
				estimated := common.QuotaRound(math.Ceil(float64(len(bodyBytes)) / 1000))
				return nil, applySpeechCreditUsage(info, estimated, "new-api-audio-byte-estimate-v1", speechByteEstimation(info, int64(len(bodyBytes)), estimated))
			}
			return nil, usage
		}

		// 写入响应到客户端
		c.Writer.WriteHeaderNow()
		_, err = c.Writer.Write(bodyBytes)
		if err != nil {
			logger.LogError(c, fmt.Sprintf("failed to write TTS response: %v", err))
		}
		if info.BillingSource == service.BillingSourceCreditPacks {
			if err != nil {
				info.StreamStatus.SetEndReason(relaycommon.StreamEndReasonClientGone, err)
				info.StreamStatus.MarkCancelled()
			} else {
				info.StreamStatus.SetEndReason(relaycommon.StreamEndReasonEOF, nil)
				info.StreamStatus.MarkCompleted()
			}
		}

		// 计算音频时长并更新 usage
		audioFormat := "mp3" // 默认格式
		if audioReq, ok := info.Request.(*dto.AudioRequest); ok && audioReq.ResponseFormat != "" {
			audioFormat = audioReq.ResponseFormat
		}

		var duration float64
		var durationErr error

		if audioFormat == "pcm" {
			// PCM 格式没有文件头，根据 OpenAI TTS 的 PCM 参数计算时长
			// 采样率: 24000 Hz, 位深度: 16-bit (2 bytes), 声道数: 1
			const sampleRate = 24000
			const bytesPerSample = 2
			const channels = 1
			duration = float64(len(bodyBytes)) / float64(sampleRate*bytesPerSample*channels)
		} else {
			ext := "." + audioFormat
			reader := bytes.NewReader(bodyBytes)
			duration, durationErr = common.GetAudioDuration(c.Request.Context(), reader, ext)
		}

		usage.PromptTokensDetails.TextTokens = usage.PromptTokens
		service.EstimateCreditUsageField(info, "prompt_tokens", usage.PromptTokens, "new-api-prompt-count-v1")

		if durationErr != nil {
			logger.LogWarn(c, fmt.Sprintf("failed to get audio duration: %v", durationErr))
			// 如果无法获取时长，则设置保底的 CompletionTokens，根据body大小计算
			sizeInKB := float64(len(bodyBytes)) / 1000.0
			estimatedTokens := common.QuotaRound(math.Ceil(sizeInKB)) // 粗略估算每KB约等于1 token
			usage.CompletionTokens = estimatedTokens
			usage.CompletionTokenDetails.AudioTokens = estimatedTokens
			estimation := speechByteEstimation(info, int64(len(bodyBytes)), estimatedTokens)
			service.EstimateCreditUsageField(info, "completion_tokens", estimatedTokens, "new-api-audio-byte-estimate-v1", estimation)
			service.EstimateCreditUsageField(info, "audio_output_tokens", estimatedTokens, "new-api-audio-byte-estimate-v1", estimation)
		} else if duration > 0 {
			// 计算 token: ceil(duration) / 60.0 * 1000，即每分钟 1000 tokens。
			// duration 解析自上游返回的音频元数据，饱和转换防止 int 回绕。
			completionTokens := common.QuotaRound(math.Ceil(duration) / 60.0 * 1000)
			usage.CompletionTokens = completionTokens
			usage.CompletionTokenDetails.AudioTokens = completionTokens
			estimation := &hosttypes.UsageEstimation{
				Version: "new-api-audio-duration-v1", Model: info.GetUpstreamModelName(), Method: "ceil-audio-seconds", Quantity: float64(completionTokens),
				Parameters: map[string]float64{"duration_seconds": min(duration, float64(common.MaxQuota)), "tokens_per_minute": 1000},
			}
			if duration > float64(common.MaxQuota) {
				estimation.Settings = map[string]bool{"duration_saturated": true}
			}
			if audioFormat == "pcm" {
				estimation.Parameters["sample_rate"], estimation.Parameters["bytes_per_sample"], estimation.Parameters["channels"] = 24000, 2, 1
			}
			service.EstimateCreditUsageField(info, "completion_tokens", completionTokens, "new-api-audio-duration-v1", estimation)
			service.EstimateCreditUsageField(info, "audio_output_tokens", completionTokens, "new-api-audio-duration-v1", estimation)
		}
		usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
	}
	if info.BillingSource == service.BillingSourceCreditPacks {
		algorithm := "new-api-audio-duration-v1"
		if fact, exists := info.CreditUsageFacts["completion_tokens"]; exists && fact.Source == "estimate" {
			algorithm = fact.Algorithm
		}
		return nil, applySpeechCreditUsage(info, usage.CompletionTokens, algorithm)
	}

	return nil, usage
}

func OpenaiSTTHandler(c *gin.Context, resp *http.Response, info *relaycommon.RelayInfo, responseFormat string) (*types.NewAPIError, *dto.Usage) {
	defer service.CloseResponseBodyGracefully(resp)
	if info.BillingSource == service.BillingSourceCreditPacks && (info.IsStream || strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream")) {
		info.IsStream = true
		var transcript strings.Builder
		var observationErr error
		var budgetStop *types.NewAPIError
		helper.StreamScannerHandler(c, resp, info, func(data string, sr *helper.StreamResult) {
			body := []byte(data)
			eventType := gjson.GetBytes(body, "type").String()
			if err := observeAudioCreditUsage(info, body, eventType == "transcript.text.done", false); err != nil {
				observationErr = err
				sr.Error(err)
				return
			}
			switch eventType {
			case "transcript.text.delta":
				transcript.WriteString(gjson.GetBytes(body, "delta").String())
			case "transcript.text.done":
				info.StreamStatus.MarkCompleted()
				if text := gjson.GetBytes(body, "text"); text.Exists() && text.Type == gjson.String {
					transcript.Reset()
					transcript.WriteString(text.String())
				}
			}
			observation := service.CreditUsageObservation(info)
			usage := applyTranscriptionCreditUsage(&observation, transcript.String(), true)
			if usage.PromptTokensDetails.AudioTokens > 0 || usage.CompletionTokenDetails.AudioTokens > 0 {
				budgetStop = service.CheckCreditAudioStreamBudget(&observation, usage)
			} else {
				budgetStop = service.CheckCreditStreamBudget(c, &observation, usage)
			}
			if budgetStop != nil {
				info.MarkStreamBudgetStop(budgetStop)
				sr.Stop(nil)
				return
			}
			if err := helper.StringData(c, data); err != nil {
				sr.Error(err)
			}
		})
		info.StreamStatus.RequireTerminal()
		if observationErr != nil {
			return types.NewError(observationErr, types.ErrorCodeBadResponseBody, types.ErrOptionWithSkipRetry()), nil
		}
		if budgetStop != nil {
			if budgetStop.GetErrorCode() != "quota_budget_exhausted" {
				return budgetStop, nil
			}
			_ = helper.StreamError(c, info.RelayFormat, budgetStop)
		}
		return nil, applyTranscriptionCreditUsage(info, transcript.String(), true)
	}

	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return types.NewOpenAIError(err, types.ErrorCodeReadResponseBodyFailed, http.StatusInternalServerError), nil
	}
	if info.BillingSource == service.BillingSourceCreditPacks {
		jsonResponse := responseFormat == "json" || responseFormat == "verbose_json" || responseFormat == "diarized_json" || strings.Contains(resp.Header.Get("Content-Type"), "json")
		if jsonResponse || gjson.ValidBytes(responseBody) {
			if err := observeAudioCreditUsage(info, responseBody, true, false); err != nil {
				return types.NewError(err, types.ErrorCodeBadResponseBody, types.ErrOptionWithSkipRetry()), nil
			}
		}
		// Preserve the native input-only fallback for duration-billed receipts.
		// Their seconds are reliable evidence; their token conversion is not.
		tokenUsage := gjson.GetBytes(responseBody, "usage.type").String() == "tokens"
		usage := applyTranscriptionCreditUsage(info, gjson.GetBytes(responseBody, "text").String(), tokenUsage)
		service.IOCopyBytesGracefully(c, resp, responseBody)
		return nil, usage
	}
	service.IOCopyBytesGracefully(c, resp, responseBody)

	var responseData struct {
		Usage *dto.Usage `json:"usage"`
	}
	if err := common.Unmarshal(responseBody, &responseData); err == nil && responseData.Usage != nil {
		if responseData.Usage.TotalTokens > 0 {
			usage := responseData.Usage
			if usage.PromptTokens == 0 {
				usage.PromptTokens = usage.InputTokens
			}
			if usage.CompletionTokens == 0 {
				usage.CompletionTokens = usage.OutputTokens
			}
			return nil, usage
		}
	}

	usage := &dto.Usage{}
	usage.PromptTokens = info.GetEstimatePromptTokens()
	usage.CompletionTokens = 0
	usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
	return nil, usage
}

func observeAudioCreditUsage(info *relaycommon.RelayInfo, body []byte, final, outputAudio bool) error {
	if !gjson.ValidBytes(body) {
		return fmt.Errorf("invalid audio response JSON")
	}
	kind := gjson.GetBytes(body, "usage.type")
	if kind.Exists() && kind.Type != gjson.Null && (kind.Type != gjson.String || (kind.String() != "tokens" && kind.String() != "duration")) {
		return fmt.Errorf("invalid audio usage type")
	}
	input, output, total := gjson.GetBytes(body, "usage.input_tokens"), gjson.GetBytes(body, "usage.output_tokens"), gjson.GetBytes(body, "usage.total_tokens")
	audio, text := gjson.GetBytes(body, "usage.input_token_details.audio_tokens"), gjson.GetBytes(body, "usage.input_token_details.text_tokens")
	if input.Type == gjson.Number {
		if (audio.Type == gjson.Number && audio.Num > input.Num) || (text.Type == gjson.Number && text.Num > input.Num) || (audio.Type == gjson.Number && text.Type == gjson.Number && audio.Num+text.Num != input.Num) {
			return fmt.Errorf("inconsistent audio input token details")
		}
	}
	if input.Type == gjson.Number && output.Type == gjson.Number && total.Type == gjson.Number && input.Num+output.Num != total.Num {
		return fmt.Errorf("inconsistent audio total tokens")
	}
	seconds := gjson.GetBytes(body, "usage.seconds")
	if (kind.String() == "duration" && (input.Exists() || output.Exists() || total.Exists() || audio.Exists() || text.Exists())) || (kind.String() == "tokens" && seconds.Exists()) {
		return fmt.Errorf("inconsistent audio usage units")
	}
	fields := map[string]service.CreditUsageField{
		"prompt_tokens":      {Path: "usage.input_tokens", Unit: "token"},
		"completion_tokens":  {Path: "usage.output_tokens", Unit: "token"},
		"total_tokens":       {Path: "usage.total_tokens", Unit: "token"},
		"audio_input_tokens": {Path: "usage.input_token_details.audio_tokens", Unit: "token"},
		"text_input_tokens":  {Path: "usage.input_token_details.text_tokens", Unit: "token"},
	}
	if outputAudio {
		// The speech SSE contract reports output_tokens as generated audio
		// tokens. Preserve the raw quantity, rather than counting base64 text.
		if seconds.Exists() || kind.String() == "duration" {
			return fmt.Errorf("unsupported speech usage units")
		}
		fields["audio_output_tokens"] = service.CreditUsageField{Path: "usage.output_tokens", Unit: "token"}
	} else {
		fields["audio_input_seconds"] = service.CreditUsageField{Path: "usage.seconds", Unit: "second"}
	}
	return service.ObserveCreditUsageFields(info, body, fields, true, final)
}

func applyTranscriptionCreditUsage(info *relaycommon.RelayInfo, transcript string, tokenUsage bool) *dto.Usage {
	service.EstimateCreditUsageField(info, "prompt_tokens", info.GetEstimatePromptTokens(), "new-api-audio-input-estimate-v1")
	if tokenUsage {
		service.EstimateCreditTextUsageField(info, "completion_tokens", []string{transcript}, info.UpstreamModelName, "new-api-transcription-text-count-v1", 0)
	} else {
		service.EstimateCreditUsageField(info, "completion_tokens", 0, "new-api-transcription-input-only-v1")
	}
	usage := &dto.Usage{}
	for field, target := range map[string]*int{
		"prompt_tokens":      &usage.PromptTokens,
		"completion_tokens":  &usage.CompletionTokens,
		"audio_input_tokens": &usage.PromptTokensDetails.AudioTokens,
		"text_input_tokens":  &usage.PromptTokensDetails.TextTokens,
	} {
		if fact, exists := info.CreditUsageFacts[field]; exists && fact.Quantity != nil {
			*target = int(*fact.Quantity)
		}
	}
	usage.InputTokens, usage.OutputTokens = usage.PromptTokens, usage.CompletionTokens
	usage.CompletionTokenDetails.TextTokens = usage.CompletionTokens
	usage.TotalTokens = common.QuotaFromFloat(float64(usage.PromptTokens) + float64(usage.CompletionTokens))
	return usage
}

// The compatible speech SSE contract supplies audio deltas and one terminal
// speech.audio.done receipt. EOF alone does not establish completed usage.
func openaiCreditSpeechStreamHandler(c *gin.Context, resp *http.Response, info *relaycommon.RelayInfo) (*types.NewAPIError, *dto.Usage) {
	info.IsStream = true
	var decodedBytes int64
	var observationErr error
	var budgetStop *types.NewAPIError
	helper.StreamScannerHandler(c, resp, info, func(data string, sr *helper.StreamResult) {
		body := []byte(data)
		eventType := gjson.GetBytes(body, "type").String()
		if err := observeAudioCreditUsage(info, body, eventType == "speech.audio.done", true); err != nil {
			observationErr = err
			sr.Stop(err)
			return
		}
		switch eventType {
		case "speech.audio.delta":
			if audio := gjson.GetBytes(body, "audio"); audio.Exists() {
				if audio.Type != gjson.String {
					observationErr = fmt.Errorf("invalid speech audio payload")
				} else {
					// Decode to a counting sink: never persist or buffer the audio payload,
					// and never pretend base64 characters are provider audio tokens.
					size, err := io.Copy(io.Discard, io.LimitReader(base64.NewDecoder(base64.StdEncoding.Strict(), strings.NewReader(audio.String())), int64(common.MaxQuota)+1))
					if err != nil || size > int64(common.MaxQuota)-decodedBytes {
						observationErr = fmt.Errorf("invalid or oversized speech audio payload")
					} else {
						decodedBytes += size
					}
				}
				if observationErr != nil {
					sr.Stop(observationErr)
					return
				}
			}
		case "speech.audio.done":
			info.StreamStatus.MarkCompleted()
		case "speech.audio.error":
			info.StreamStatus.MarkFailed(gjson.GetBytes(body, "error.code").String(), gjson.GetBytes(body, "error.type").String(), http.StatusBadGateway)
			code := gjson.GetBytes(body, "error.code")
			if code.Type == gjson.Number && code.Num >= 400 && code.Num <= 599 && math.Trunc(code.Num) == code.Num {
				info.StreamStatus.MarkFailed("", "", int(code.Num))
			}
		}
		observation := service.CreditUsageObservation(info)
		estimated := common.QuotaRound(math.Ceil(float64(decodedBytes) / 1000))
		usage := applySpeechCreditUsage(&observation, estimated, "new-api-audio-byte-estimate-v1", speechByteEstimation(info, decodedBytes, estimated))
		if usage.PromptTokensDetails.AudioTokens > 0 || usage.CompletionTokenDetails.AudioTokens > 0 {
			budgetStop = service.CheckCreditAudioStreamBudget(&observation, usage)
		} else {
			budgetStop = service.CheckCreditStreamBudget(c, &observation, usage)
		}
		if budgetStop != nil {
			info.MarkStreamBudgetStop(budgetStop)
			sr.Stop(nil)
			return
		}
		if err := helper.StringData(c, data); err != nil {
			sr.Stop(err)
		}
		if eventType == "speech.audio.error" {
			sr.Stop(nil)
		}
	})
	info.StreamStatus.RequireTerminal()
	if observationErr != nil {
		return types.NewError(observationErr, types.ErrorCodeBadResponseBody, types.ErrOptionWithSkipRetry()), nil
	}
	if budgetStop != nil {
		if budgetStop.GetErrorCode() != "quota_budget_exhausted" {
			return budgetStop, nil
		}
		_ = helper.StreamError(c, info.RelayFormat, budgetStop)
	}
	// Retain the native byte-based fallback as an explicit estimate. Different
	// providers may use different codec/sample rates, so no duration is inferred.
	estimated := common.QuotaRound(math.Ceil(float64(decodedBytes) / 1000))
	return nil, applySpeechCreditUsage(info, estimated, "new-api-audio-byte-estimate-v1", speechByteEstimation(info, decodedBytes, estimated))
}

// Byte counting uses decoded bytes and its existing ceiling conversion. It
// does not infer a codec, sample rate, duration, or provider token receipt.
func speechByteEstimation(info *relaycommon.RelayInfo, decodedBytes int64, quantity int) *hosttypes.UsageEstimation {
	return &hosttypes.UsageEstimation{Version: "new-api-audio-byte-estimate-v1", Model: info.GetUpstreamModelName(), Method: "decoded-audio-bytes", Quantity: float64(quantity), Parameters: map[string]float64{"decoded_bytes": float64(decodedBytes), "bytes_per_token": 1000}}
}

func applySpeechCreditUsage(info *relaycommon.RelayInfo, estimated int, algorithm string, estimation ...*hosttypes.UsageEstimation) *dto.Usage {
	service.EstimateCreditUsageField(info, "prompt_tokens", info.GetEstimatePromptTokens(), "new-api-prompt-count-v1")
	service.EstimateCreditUsageField(info, "completion_tokens", estimated, algorithm, estimation...)
	service.EstimateCreditUsageField(info, "audio_output_tokens", estimated, algorithm, estimation...)
	usage := &dto.Usage{}
	for field, target := range map[string]*int{
		"prompt_tokens":       &usage.PromptTokens,
		"completion_tokens":   &usage.CompletionTokens,
		"audio_input_tokens":  &usage.PromptTokensDetails.AudioTokens,
		"text_input_tokens":   &usage.PromptTokensDetails.TextTokens,
		"audio_output_tokens": &usage.CompletionTokenDetails.AudioTokens,
	} {
		if fact, exists := info.CreditUsageFacts[field]; exists && fact.Quantity != nil {
			*target = int(*fact.Quantity)
		}
	}
	if _, exists := info.CreditUsageFacts["text_input_tokens"]; !exists {
		usage.PromptTokensDetails.TextTokens = max(0, usage.PromptTokens-usage.PromptTokensDetails.AudioTokens)
		estimation := &hosttypes.UsageEstimation{Version: "new-api-speech-input-text-estimate-v1", Model: info.GetUpstreamModelName(), Method: "text-input-remainder", Quantity: float64(usage.PromptTokensDetails.TextTokens), Parameters: map[string]float64{"prompt_tokens": float64(usage.PromptTokens), "audio_input_tokens": float64(usage.PromptTokensDetails.AudioTokens)}}
		service.EstimateCreditUsageField(info, "text_input_tokens", usage.PromptTokensDetails.TextTokens, "new-api-speech-input-text-estimate-v1", estimation)
	}
	usage.InputTokens, usage.OutputTokens = usage.PromptTokens, usage.CompletionTokens
	usage.TotalTokens = common.QuotaFromFloat(float64(usage.PromptTokens) + float64(usage.CompletionTokens))
	return usage
}

// Non-streaming vLLM-Omni speech responses put their token receipt in headers.
// Missing headers stay missing; a present but empty/invalid header is rejected.
func observeSpeechCreditHeaders(info *relaycommon.RelayInfo, header http.Header) error {
	quantities := make(map[string]any)
	details := make(map[string]any)
	for name, field := range map[string]string{
		"x-vllm-omni-input-tokens":       "input_tokens",
		"x-vllm-omni-output-tokens":      "output_tokens",
		"x-vllm-omni-total-tokens":       "total_tokens",
		"x-vllm-omni-input-text-tokens":  "text_tokens",
		"x-vllm-omni-input-audio-tokens": "audio_tokens",
	} {
		values := header.Values(name)
		if len(values) == 0 {
			continue
		}
		if len(values) != 1 {
			return fmt.Errorf("ambiguous speech usage header %s", name)
		}
		quantity, err := strconv.ParseFloat(values[0], 64)
		if err != nil || math.IsNaN(quantity) || math.IsInf(quantity, 0) || quantity < 0 || quantity > float64(common.MaxQuota) || math.Trunc(quantity) != quantity {
			return fmt.Errorf("invalid speech usage header %s", name)
		}
		if field == "text_tokens" || field == "audio_tokens" {
			details[field] = quantity
		} else {
			quantities[field] = quantity
		}
	}
	if len(details) > 0 {
		quantities["input_token_details"] = details
	}
	if len(quantities) == 0 {
		return nil
	}
	body, err := common.Marshal(map[string]any{"usage": quantities})
	if err != nil {
		return err
	}
	return observeAudioCreditUsage(info, body, true, true)
}

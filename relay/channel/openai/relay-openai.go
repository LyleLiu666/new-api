package openai

import (
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/relay/channel/openrouter"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

func sendStreamData(c *gin.Context, info *relaycommon.RelayInfo, data string, forceFormat bool, thinkToContent bool) error {
	if data == "" {
		return nil
	}

	if !forceFormat && !thinkToContent {
		return helper.StringData(c, data)
	}

	var lastStreamResponse dto.ChatCompletionsStreamResponse
	if err := common.UnmarshalJsonStr(data, &lastStreamResponse); err != nil {
		return err
	}

	if !thinkToContent {
		return helper.ObjectData(c, lastStreamResponse)
	}

	hasThinkingContent := false
	hasContent := false
	var thinkingContent strings.Builder
	for _, choice := range lastStreamResponse.Choices {
		if len(choice.Delta.GetReasoningContent()) > 0 {
			hasThinkingContent = true
			thinkingContent.WriteString(choice.Delta.GetReasoningContent())
		}
		if len(choice.Delta.GetContentString()) > 0 {
			hasContent = true
		}
	}

	// Handle think to content conversion
	if info.ThinkingContentInfo.IsFirstThinkingContent {
		if hasThinkingContent {
			response := lastStreamResponse.Copy()
			for i := range response.Choices {
				// send `think` tag with thinking content
				response.Choices[i].Delta.SetContentString("<think>\n" + thinkingContent.String())
				response.Choices[i].Delta.ReasoningContent = nil
				response.Choices[i].Delta.Reasoning = nil
			}
			info.ThinkingContentInfo.IsFirstThinkingContent = false
			info.ThinkingContentInfo.HasSentThinkingContent = true
			return helper.ObjectData(c, response)
		}
	}

	if lastStreamResponse.Choices == nil || len(lastStreamResponse.Choices) == 0 {
		return helper.ObjectData(c, lastStreamResponse)
	}

	// Process each choice
	for i, choice := range lastStreamResponse.Choices {
		// Handle transition from thinking to content
		// only send `</think>` tag when previous thinking content has been sent
		if hasContent && !info.ThinkingContentInfo.SendLastThinkingContent && info.ThinkingContentInfo.HasSentThinkingContent {
			response := lastStreamResponse.Copy()
			for j := range response.Choices {
				response.Choices[j].Delta.SetContentString("\n</think>\n")
				response.Choices[j].Delta.ReasoningContent = nil
				response.Choices[j].Delta.Reasoning = nil
			}
			info.ThinkingContentInfo.SendLastThinkingContent = true
			helper.ObjectData(c, response)
		}

		// Convert reasoning content to regular content if any
		if len(choice.Delta.GetReasoningContent()) > 0 {
			lastStreamResponse.Choices[i].Delta.SetContentString(choice.Delta.GetReasoningContent())
			lastStreamResponse.Choices[i].Delta.ReasoningContent = nil
			lastStreamResponse.Choices[i].Delta.Reasoning = nil
		} else if !hasThinkingContent && !hasContent {
			// flush thinking content
			lastStreamResponse.Choices[i].Delta.ReasoningContent = nil
			lastStreamResponse.Choices[i].Delta.Reasoning = nil
		}
	}

	return helper.ObjectData(c, lastStreamResponse)
}

func OaiStreamHandler(c *gin.Context, info *relaycommon.RelayInfo, resp *http.Response) (*dto.Usage, *types.NewAPIError) {
	if resp == nil || resp.Body == nil {
		logger.LogError(c, "invalid response or response body")
		return nil, types.NewOpenAIError(fmt.Errorf("invalid response"), types.ErrorCodeBadResponse, http.StatusInternalServerError)
	}

	defer service.CloseResponseBodyGracefully(resp)

	model := info.UpstreamModelName
	var responseId string
	var createAt int64 = 0
	var systemFingerprint string
	var containStreamUsage bool
	var responseTextBuilder strings.Builder
	var toolCount int
	var usage = &dto.Usage{}
	var lastStreamData string
	var secondLastStreamData string // 保留倒数第二个stream data；部分兼容网关把完整usage放在倒数第二个事件
	seenStreamToolCalls := make(map[string]struct{})
	var streamFunctionCallNames []string
	var observationErr error
	var budgetStop *types.NewAPIError
	finishObserved := false

	helper.StreamScannerHandler(c, resp, info, func(data string, sr *helper.StreamResult) {
		if info.BillingSource == service.BillingSourceCreditPacks {
			var frame dto.ChatCompletionsStreamResponse
			if common.UnmarshalJsonStr(data, &frame) == nil {
				for _, choice := range frame.Choices {
					if choice.FinishReason != nil && *choice.FinishReason != "" {
						finishObserved = true
					}
				}
				if frame.Usage != nil {
					usage = dto.MergeUsageNonZero(usage, frame.Usage)
				}
			}
		}
		if err := observeOpenaiCreditUsage(info, common.StringToByteSlice(data), finishObserved); err != nil {
			observationErr = err
			sr.Stop(err)
			return
		}
		if lastStreamData != "" {
			if err := HandleStreamFormat(c, info, lastStreamData, info.ChannelSetting.ForceFormat, info.ChannelSetting.ThinkingToContent); err != nil {
				if writer, ok := c.Writer.(interface{ RelayEvidenceError() error }); ok && writer.RelayEvidenceError() != nil {
					observationErr = err
					sr.Stop(err)
					return
				}
				common.SysLog("error handling stream format: " + err.Error())
				sr.Error(err)
			}
		}
		if len(data) > 0 {
			if lastStreamData != "" {
				secondLastStreamData = lastStreamData
			}

			lastStreamData = data
			observeStreamChoices(info, data, seenStreamToolCalls, &streamFunctionCallNames)
			if err := processTokenData(info, data, &responseTextBuilder, &toolCount); err != nil {
				logger.LogError(c, "error processing stream token data: "+err.Error())
				sr.Error(err)
			}
			if service.CreditBillingRequestID(info) != 0 {
				// Temporary estimates for monitoring never overwrite the original
				// partial observations or freeze an early final estimate.
				progress := service.CreditUsageObservation(info)
				service.EstimateCreditUsageField(&progress, "prompt_tokens", info.GetEstimatePromptTokens(), "new-api-prompt-count-v1")
				service.EstimateCreditTextUsageField(&progress, "completion_tokens", []string{responseTextBuilder.String()}, info.UpstreamModelName, "new-api-stream-text-tools-v1", common.QuotaRound(float64(toolCount)*7))
				metered := *usage
				for field, target := range map[string]*int{"prompt_tokens": &metered.PromptTokens, "completion_tokens": &metered.CompletionTokens, "cached_tokens": &metered.PromptTokensDetails.CachedTokens} {
					if fact := progress.CreditUsageFacts[field]; fact.Quantity != nil {
						*target = int(*fact.Quantity)
					}
				}
				metered.TotalTokens = common.QuotaRound(float64(metered.PromptTokens) + float64(metered.CompletionTokens))
				applyUsagePostProcessing(&progress, &metered, common.StringToByteSlice(data))
				budgetStop = service.CheckCreditStreamBudget(c, &progress, &metered)
				if budgetStop != nil {
					info.MarkStreamBudgetStop(budgetStop)
					sr.Stop(nil)
					return
				}
			}
		}
	})
	if observationErr != nil {
		return nil, types.NewError(observationErr, types.ErrorCodeBadResponseBody, types.ErrOptionWithSkipRetry())
	}

	info.StreamStatus.RequireTerminal()

	// 处理最后的响应
	shouldSendLastResp := true
	if budgetStop == nil {
		if err := handleLastResponse(lastStreamData, &responseId, &createAt, &systemFingerprint, &model, &usage,
			&containStreamUsage, info, &shouldSendLastResp); err != nil {
			logger.LogError(c, fmt.Sprintf("error handling last response: %s, lastStreamData: [%s]", err.Error(), lastStreamData))
		}
	}

	// 部分兼容网关把完整的累计usage附在倒数第二个事件上，随后发送一个空的最后事件。
	// 仅当最后一个事件没有有效usage时，回退到倒数第二个事件的完整快照。
	usageFrame := lastStreamData
	if budgetStop == nil && !containStreamUsage && secondLastStreamData != "" {
		var streamResp struct {
			Usage *dto.Usage `json:"usage"`
		}
		err := common.Unmarshal([]byte(secondLastStreamData), &streamResp)
		if err == nil && streamResp.Usage != nil &&
			streamResp.Usage.PromptTokens > 0 &&
			(streamResp.Usage.CompletionTokens > 0 || streamResp.Usage.TotalTokens > 0) {
			usage = dto.MergeUsageNonZero(usage, streamResp.Usage)
			containStreamUsage = true
			usageFrame = secondLastStreamData

			if common.DebugEnabled {
				logger.LogDebug(c, "usage extracted from second last SSE: PromptTokens=%d, CompletionTokens=%d, TotalTokens=%d, InputTokens=%d, OutputTokens=%d",
					usage.PromptTokens, usage.CompletionTokens, usage.TotalTokens,
					usage.InputTokens, usage.OutputTokens)
			}
		}
	}

	if info.RelayFormat == types.RelayFormatOpenAI {
		if shouldSendLastResp && budgetStop == nil {
			_ = sendStreamData(c, info, lastStreamData, info.ChannelSetting.ForceFormat, info.ChannelSetting.ThinkingToContent)
		}
	}
	if writer, ok := c.Writer.(interface{ RelayEvidenceError() error }); ok {
		if err := writer.RelayEvidenceError(); err != nil {
			return nil, types.NewError(err, types.ErrorCodeBadResponseBody, types.ErrOptionWithSkipRetry())
		}
	}

	if info.BillingSource == service.BillingSourceCreditPacks {
		if budgetStop == nil && info.StreamStatus.EndReason == relaycommon.StreamEndReasonDone {
			for field, path := range openaiCreditUsagePaths {
				value := gjson.Get(lastStreamData, path)
				if !value.Exists() || value.Type == gjson.Null {
					continue
				}
				fact, present := info.CreditUsageFacts[field]
				if !present {
					continue
				}
				fact.Partial = false
				info.CreditUsageFacts[field] = fact
			}
		}
		if _, present := info.CreditUsageFacts["prompt_tokens"]; !present {
			service.EstimateCreditUsageField(info, "prompt_tokens", info.GetEstimatePromptTokens(), "new-api-prompt-count-v1")
		}
		if fact, present := info.CreditUsageFacts["completion_tokens"]; !present || fact.Partial {
			service.EstimateCreditTextUsageField(info, "completion_tokens", []string{responseTextBuilder.String()}, info.UpstreamModelName, "new-api-stream-text-tools-v1", common.QuotaRound(float64(toolCount)*7))
		}
		usage.PromptTokens = int(*info.CreditUsageFacts["prompt_tokens"].Quantity)
		usage.CompletionTokens = int(*info.CreditUsageFacts["completion_tokens"].Quantity)
		usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
		if reported, ok := info.CreditUsageFacts["total_tokens"]; ok {
			usage.TotalTokens = int(*reported.Quantity)
		}
	} else if !containStreamUsage {
		usage = service.ResponseText2Usage(c, responseTextBuilder.String(), info.UpstreamModelName, info.GetEstimatePromptTokens())
		usage.CompletionTokens += toolCount * 7
	}

	applyUsagePostProcessing(info, usage, common.StringToByteSlice(usageFrame))
	if reported, ok := info.CreditUsageFacts["cached_tokens"]; ok {
		usage.PromptTokensDetails.CachedTokens = int(*reported.Quantity)
	}

	if budgetStop != nil {
		if budgetStop.GetErrorCode() != "quota_budget_exhausted" {
			return nil, budgetStop
		}
		_ = helper.StreamError(c, info.RelayFormat, budgetStop)
	} else {
		HandleFinalResponse(c, info, lastStreamData, responseId, createAt, model, systemFingerprint, usage, containStreamUsage)
	}
	if writer, ok := c.Writer.(interface{ RelayEvidenceError() error }); ok {
		if err := writer.RelayEvidenceError(); err != nil {
			return nil, types.NewError(err, types.ErrorCodeBadResponseBody, types.ErrOptionWithSkipRetry())
		}
	}

	return usage, nil
}

// observeStreamChoices collects billable function call names and records the
// finish reason facts used by health sampling from one parsed chunk.
func observeStreamChoices(info *relaycommon.RelayInfo, data string, seen map[string]struct{}, names *[]string) {
	var streamResponse dto.ChatCompletionsStreamResponse
	if err := common.UnmarshalJsonStr(data, &streamResponse); err != nil {
		return
	}
	for _, choice := range streamResponse.Choices {
		if choice.FinishReason != nil && *choice.FinishReason != "" {
			if *choice.FinishReason == constant.FinishReasonContentFilter {
				info.PerformanceBusinessRejection = true
			}
			info.StreamStatus.MarkCompleted()
		}
		for i, tc := range choice.Delta.ToolCalls {
			name := strings.TrimSpace(tc.Function.Name)
			if name == "" {
				continue
			}
			toolIdx := i
			if tc.Index != nil {
				toolIdx = *tc.Index
			}
			fallbackKey := fmt.Sprintf("index\x00%d\x00%d\x00%s", choice.Index, toolIdx, name)
			activeKey := fmt.Sprintf("active\x00%d\x00%d\x00%s", choice.Index, toolIdx, name)
			callID := strings.TrimSpace(tc.ID)
			if callID != "" {
				idKey := fmt.Sprintf("id\x00%d\x00%s", choice.Index, callID)
				if _, ok := seen[idKey]; ok {
					continue
				}
				seen[idKey] = struct{}{}
				seen[activeKey] = struct{}{}
				if _, delayedID := seen[fallbackKey]; delayedID {
					delete(seen, fallbackKey)
					continue
				}
			} else {
				if _, ok := seen[fallbackKey]; ok {
					continue
				}
				if _, ok := seen[activeKey]; ok {
					continue
				}
				seen[fallbackKey] = struct{}{}
				seen[activeKey] = struct{}{}
			}
			*names = append(*names, name)
			info.CountBillableToolCall(dto.BuildInCallFunctionCall, name)
		}
	}
}

func OpenaiHandler(c *gin.Context, info *relaycommon.RelayInfo, resp *http.Response) (*dto.Usage, *types.NewAPIError) {
	defer service.CloseResponseBodyGracefully(resp)

	var simpleResponse dto.OpenAITextResponse
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeReadResponseBodyFailed, http.StatusInternalServerError)
	}
	logger.LogDebug(c, "upstream response body: %s", responseBody)
	// Unmarshal to simpleResponse
	if info.ChannelType == constant.ChannelTypeOpenRouter && info.ChannelOtherSettings.IsOpenRouterEnterprise() {
		// 尝试解析为 openrouter enterprise
		var enterpriseResponse openrouter.OpenRouterEnterpriseResponse
		err = common.Unmarshal(responseBody, &enterpriseResponse)
		if err != nil {
			return nil, types.NewOpenAIError(err, types.ErrorCodeBadResponseBody, http.StatusInternalServerError)
		}
		if enterpriseResponse.Success {
			responseBody = enterpriseResponse.Data
		} else {
			logger.LogError(c, fmt.Sprintf("openrouter enterprise response success=false, data: %s", enterpriseResponse.Data))
			return nil, types.NewOpenAIError(fmt.Errorf("openrouter response success=false"), types.ErrorCodeBadResponseBody, http.StatusInternalServerError)
		}
	}

	err = common.Unmarshal(responseBody, &simpleResponse)
	if err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeBadResponseBody, http.StatusInternalServerError)
	}

	if oaiError := simpleResponse.GetOpenAIError(); oaiError != nil && oaiError.Type != "" {
		return nil, types.WithOpenAIError(*oaiError, resp.StatusCode)
	}

	info.ObserveResponseModel(simpleResponse.Model)
	for _, choice := range simpleResponse.Choices {
		if choice.FinishReason == constant.FinishReasonContentFilter {
			info.PerformanceBusinessRejection = true
			common.SetContextKey(c, constant.ContextKeyAdminRejectReason, "openai_finish_reason=content_filter")
			break
		}
	}

	for _, choice := range simpleResponse.Choices {
		for _, tc := range choice.Message.ParseToolCalls() {
			info.CountBillableToolCall(dto.BuildInCallFunctionCall, tc.Function.Name)
		}
	}

	forceFormat := false
	if info.ChannelSetting.ForceFormat {
		forceFormat = true
	}

	usageModified := false
	if err := observeOpenaiCreditUsage(info, responseBody, true); err != nil {
		return nil, types.NewError(err, types.ErrorCodeBadResponseBody)
	}
	if info.BillingSource == service.BillingSourceCreditPacks {
		if _, present := info.CreditUsageFacts["prompt_tokens"]; !present {
			simpleResponse.Usage.PromptTokens = info.GetEstimatePromptTokens()
			service.EstimateCreditUsageField(info, "prompt_tokens", simpleResponse.Usage.PromptTokens, "new-api-prompt-count-v1")
			usageModified = true
		}
		if _, present := info.CreditUsageFacts["completion_tokens"]; !present {
			texts := make([]string, 0, len(simpleResponse.Choices))
			for _, choice := range simpleResponse.Choices {
				texts = append(texts, choice.Message.StringContent()+choice.Message.GetReasoningContent())
			}
			simpleResponse.Usage.CompletionTokens = service.EstimateCreditTextUsageField(info, "completion_tokens", texts, info.UpstreamModelName, "new-api-text-count-v1", 0)
			usageModified = true
		}
		if _, present := info.CreditUsageFacts["total_tokens"]; !present {
			simpleResponse.Usage.TotalTokens = simpleResponse.Usage.PromptTokens + simpleResponse.Usage.CompletionTokens
		}
	} else if simpleResponse.Usage.PromptTokens == 0 {
		completionTokens := simpleResponse.Usage.CompletionTokens
		if completionTokens == 0 {
			for _, choice := range simpleResponse.Choices {
				ctkm := service.CountTextToken(choice.Message.StringContent()+choice.Message.GetReasoningContent(), info.UpstreamModelName)
				completionTokens += ctkm
			}
		}
		fallbackUsage := &dto.Usage{
			PromptTokens:     info.GetEstimatePromptTokens(),
			CompletionTokens: completionTokens,
			TotalTokens:      info.GetEstimatePromptTokens() + completionTokens,
		}
		simpleResponse.Usage = *fallbackUsage
		usageModified = true
	}

	applyUsagePostProcessing(info, &simpleResponse.Usage, responseBody)

	switch info.RelayFormat {
	case types.RelayFormatOpenAI:
		if usageModified {
			var bodyMap map[string]any
			err = common.Unmarshal(responseBody, &bodyMap)
			if err != nil {
				return nil, types.NewOpenAIError(err, types.ErrorCodeBadResponseBody, http.StatusInternalServerError)
			}
			bodyMap["usage"] = simpleResponse.Usage
			responseBody, _ = common.Marshal(bodyMap)
		}
		if forceFormat {
			responseBody, err = common.Marshal(simpleResponse)
			if err != nil {
				return nil, types.NewError(err, types.ErrorCodeBadResponseBody)
			}
		} else {
			break
		}
	case types.RelayFormatClaude:
		convertResult, err := service.ConvertResponse(c, info, types.RelayFormatClaude, &simpleResponse)
		if err != nil {
			return nil, types.NewError(err, types.ErrorCodeBadResponseBody)
		}
		claudeRespStr, err := common.Marshal(convertResult.Value)
		if err != nil {
			return nil, types.NewError(err, types.ErrorCodeBadResponseBody)
		}
		responseBody = claudeRespStr
	case types.RelayFormatGemini:
		convertResult, err := service.ConvertResponse(c, info, types.RelayFormatGemini, &simpleResponse)
		if err != nil {
			return nil, types.NewError(err, types.ErrorCodeBadResponseBody)
		}
		geminiRespStr, err := common.Marshal(convertResult.Value)
		if err != nil {
			return nil, types.NewError(err, types.ErrorCodeBadResponseBody)
		}
		responseBody = geminiRespStr
	}

	service.IOCopyBytesGracefully(c, resp, responseBody)

	return &simpleResponse.Usage, nil
}

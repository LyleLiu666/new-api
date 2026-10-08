package claude

import (
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/logger"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/relayconvert"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/model_setting"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

func stopReasonClaude2OpenAI(reason string) string {
	return relayconvert.StopReasonClaudeToOpenAI(reason)
}

func maybeMarkClaudeRefusal(c *gin.Context, info *relaycommon.RelayInfo, stopReason string) {
	if c == nil {
		return
	}
	if strings.EqualFold(stopReason, "refusal") {
		info.PerformanceBusinessRejection = true
		common.SetContextKey(c, constant.ContextKeyAdminRejectReason, "claude_stop_reason=refusal")
	}
}

func StreamResponseClaude2OpenAI(claudeResponse *dto.ClaudeResponse) *dto.ChatCompletionsStreamResponse {
	return relayconvert.StreamResponseClaude2OpenAI(claudeResponse)
}

func ResponseClaude2OpenAI(claudeResponse *dto.ClaudeResponse) *dto.OpenAITextResponse {
	return relayconvert.ResponseClaude2OpenAI(claudeResponse)
}

type ClaudeResponseInfo = relayconvert.ClaudeResponseInfo

func cacheCreationTokensForOpenAIUsage(usage *dto.Usage) int {
	if usage == nil {
		return 0
	}
	openAIUsage := relayconvert.UsageFromClaudeUsage(usage)
	if openAIUsage == nil {
		return 0
	}
	return openAIUsage.PromptTokens - usage.PromptTokens - usage.PromptTokensDetails.CachedTokens
}

func buildOpenAIStyleUsageFromClaudeUsage(usage *dto.Usage) dto.Usage {
	mapped := relayconvert.UsageFromClaudeUsage(usage)
	if mapped == nil {
		return dto.Usage{}
	}
	return *mapped
}

func buildMessageDeltaPatchUsage(claudeResponse *dto.ClaudeResponse, claudeInfo *ClaudeResponseInfo) *dto.ClaudeUsage {
	return relayconvert.BuildMessageDeltaPatchUsage(claudeResponse, claudeInfo)
}

func shouldSkipClaudeMessageDeltaUsagePatch(info *relaycommon.RelayInfo) bool {
	if model_setting.GetGlobalSettings().PassThroughRequestEnabled {
		return true
	}
	if info == nil {
		return false
	}
	return info.ChannelSetting.PassThroughBodyEnabled
}

func patchClaudeMessageDeltaUsageData(data string, usage *dto.ClaudeUsage) string {
	return relayconvert.PatchClaudeMessageDeltaUsageData(data, usage)
}

func FormatClaudeResponseInfo(claudeResponse *dto.ClaudeResponse, oaiResponse *dto.ChatCompletionsStreamResponse, claudeInfo *ClaudeResponseInfo) bool {
	return relayconvert.FormatClaudeResponseInfo(claudeResponse, oaiResponse, claudeInfo)
}

// Capture the provider's fields before message_delta compatibility patches add
// missing input/cache values. Only a stop-bearing delta has final output usage.
func observeClaudeCreditUsage(info *relaycommon.RelayInfo, response *dto.ClaudeResponse, data []byte) error {
	if info.BillingSource != service.BillingSourceCreditPacks {
		return nil
	}
	prefix := "usage."
	final := response.Type != "message_start" && response.Type != "message_delta"
	if response.Type == "message_start" {
		prefix = "message.usage."
	} else if response.Type == "message_delta" {
		final = response.StopReason != "" || (response.Delta != nil && response.Delta.StopReason != nil && *response.Delta.StopReason != "")
	}
	return service.ObserveCreditUsage(info, data, map[string]string{
		"prompt_tokens":            prefix + "input_tokens",
		"completion_tokens":        prefix + "output_tokens",
		"cached_tokens":            prefix + "cache_read_input_tokens",
		"cache_creation_tokens":    prefix + "cache_creation_input_tokens",
		"cache_creation_tokens_5m": prefix + "cache_creation.ephemeral_5m_input_tokens",
		"cache_creation_tokens_1h": prefix + "cache_creation.ephemeral_1h_input_tokens",
	}, true, final)
}

// Refresh both host usage and the native billing snapshot from the same fields.
// A zero in a complete receipt wins; a partial output count is a lower bound.
func applyClaudeCreditUsage(info *relaycommon.RelayInfo, usage *dto.Usage, outputText string) {
	service.EstimateCreditUsageField(info, "prompt_tokens", info.GetEstimatePromptTokens(), "new-api-prompt-count-v1")
	service.EstimateCreditTextUsageField(info, "completion_tokens", []string{outputText}, info.UpstreamModelName, "new-api-output-count-v1", 0)
	for field, target := range map[string]*int{
		"prompt_tokens":            &usage.PromptTokens,
		"completion_tokens":        &usage.CompletionTokens,
		"cached_tokens":            &usage.PromptTokensDetails.CachedTokens,
		"cache_creation_tokens":    &usage.PromptTokensDetails.CachedCreationTokens,
		"cache_creation_tokens_5m": &usage.ClaudeCacheCreation5mTokens,
		"cache_creation_tokens_1h": &usage.ClaudeCacheCreation1hTokens,
	} {
		if fact, ok := info.CreditUsageFacts[field]; ok && fact.Quantity != nil {
			*target = int(*fact.Quantity)
		}
	}
	usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
	usage.UsageSemantic = "anthropic"
	providerUsage := &dto.ClaudeUsage{
		InputTokens: usage.PromptTokens, OutputTokens: usage.CompletionTokens,
		CacheReadInputTokens:     usage.PromptTokensDetails.CachedTokens,
		CacheCreationInputTokens: usage.PromptTokensDetails.CachedCreationTokens,
		CacheCreation: &dto.ClaudeCacheCreationUsage{
			Ephemeral5mInputTokens: usage.ClaudeCacheCreation5mTokens,
			Ephemeral1hInputTokens: usage.ClaudeCacheCreation1hTokens,
		},
	}
	usage.BillingUsage = dto.NewClaudeMessagesBillingUsage(providerUsage)
	if usage.BillingUsage != nil {
		usage.BillingUsage.Estimated = info.CreditUsageFacts["prompt_tokens"].Source == "estimate" || info.CreditUsageFacts["completion_tokens"].Source == "estimate"
	}
}

func HandleStreamResponseData(c *gin.Context, info *relaycommon.RelayInfo, claudeInfo *ClaudeResponseInfo, data string) *types.NewAPIError {
	var claudeResponse dto.ClaudeResponse
	err := common.UnmarshalJsonStr(data, &claudeResponse)
	if err != nil {
		common.SysLog("error unmarshalling stream response: " + err.Error())
		return types.NewError(err, types.ErrorCodeBadResponseBody)
	}
	if claudeError := claudeResponse.GetClaudeError(); claudeError != nil && claudeError.Type != "" {
		return types.WithClaudeError(*claudeError, http.StatusInternalServerError)
	}
	if err := observeClaudeCreditUsage(info, &claudeResponse, common.StringToByteSlice(data)); err != nil {
		return types.NewError(err, types.ErrorCodeBadResponseBody, types.ErrOptionWithSkipRetry())
	}
	if info.BillingSource == service.BillingSourceCreditPacks && claudeResponse.Delta != nil && claudeResponse.Delta.PartialJson != nil {
		claudeInfo.ResponseText.WriteString(*claudeResponse.Delta.PartialJson)
	}
	if claudeResponse.Type == "message_start" && claudeResponse.Message != nil {
		info.ObserveResponseModel(claudeResponse.Message.Model)
	}
	if claudeResponse.StopReason != "" {
		maybeMarkClaudeRefusal(c, info, claudeResponse.StopReason)
	}
	if claudeResponse.Delta != nil && claudeResponse.Delta.StopReason != nil {
		maybeMarkClaudeRefusal(c, info, *claudeResponse.Delta.StopReason)
	}
	if claudeResponse.Type == "message_stop" {
		info.StreamStatus.MarkCompleted()
	}
	if info.RelayFormat == types.RelayFormatClaude {
		FormatClaudeResponseInfo(&claudeResponse, nil, claudeInfo)

		if claudeResponse.Type == "message_start" {
			// message_start, 获取usage
			if claudeResponse.Message != nil {
				info.UpstreamModelName = claudeResponse.Message.Model
			}
		} else if claudeResponse.Type == "message_delta" {
			// 确保 message_delta 的 usage 包含完整的 input_tokens 和 cache 相关字段
			// 解决 AWS Bedrock 等上游返回的 message_delta 缺少这些字段的问题
			if !shouldSkipClaudeMessageDeltaUsagePatch(info) {
				patchUsage := buildMessageDeltaPatchUsage(&claudeResponse, claudeInfo)
				if info.BillingSource == service.BillingSourceCreditPacks && patchUsage != nil {
					// The legacy patch treats zero as absent. Suppress only those
					// additions whose fields are already present on the wire.
					fields := map[string]*int{
						"usage.input_tokens":                &patchUsage.InputTokens,
						"usage.cache_read_input_tokens":     &patchUsage.CacheReadInputTokens,
						"usage.cache_creation_input_tokens": &patchUsage.CacheCreationInputTokens,
					}
					if patchUsage.CacheCreation != nil {
						fields["usage.cache_creation.ephemeral_5m_input_tokens"] = &patchUsage.CacheCreation.Ephemeral5mInputTokens
						fields["usage.cache_creation.ephemeral_1h_input_tokens"] = &patchUsage.CacheCreation.Ephemeral1hInputTokens
					}
					for path, value := range fields {
						if gjson.Get(data, path).Exists() {
							*value = 0
						}
					}
				}
				data = patchClaudeMessageDeltaUsageData(data, patchUsage)
			}
		}
		if err := checkClaudeStreamBudget(c, info, claudeInfo, &claudeResponse); err != nil {
			return err
		}
		helper.ClaudeChunkData(c, claudeResponse, data)
	} else if info.RelayFormat == types.RelayFormatOpenAI {
		state, err := claudeToChatStreamState(info)
		if err != nil {
			return types.NewError(err, types.ErrorCodeBadResponseBody)
		}
		response, err := state.ConvertChunk(&claudeResponse)
		if err != nil {
			return types.NewError(err, types.ErrorCodeBadResponseBody)
		}

		if !FormatClaudeResponseInfo(&claudeResponse, response, claudeInfo) {
			return nil
		}

		if err := checkClaudeStreamBudget(c, info, claudeInfo, &claudeResponse); err != nil {
			return err
		}

		if response == nil {
			return nil
		}
		err = helper.ObjectData(c, response)
		if err != nil {
			logger.LogError(c, "send_stream_response_failed: "+err.Error())
		}
	} else if info.RelayFormat == types.RelayFormatGemini {
		state, err := claudeToGeminiStreamState(info)
		if err != nil {
			return types.NewError(err, types.ErrorCodeBadResponseBody)
		}
		results, err := service.ConvertStreamResponseChunk(c, info, state, &claudeResponse)
		if err != nil {
			return types.NewError(err, types.ErrorCodeBadResponseBody)
		}
		if !FormatClaudeResponseInfo(&claudeResponse, nil, claudeInfo) {
			return nil
		}
		if err := checkClaudeStreamBudget(c, info, claudeInfo, &claudeResponse); err != nil {
			return err
		}
		if sendErr := sendGeminiStreamResults(c, results); sendErr != nil {
			return sendErr
		}
	}
	return nil
}

func claudeToChatStreamState(info *relaycommon.RelayInfo) (*relayconvert.ClaudeToChatStreamState, error) {
	if info != nil && info.ClaudeToChatStreamState != nil {
		state, ok := info.ClaudeToChatStreamState.(*relayconvert.ClaudeToChatStreamState)
		if !ok || state == nil {
			return nil, fmt.Errorf("invalid Claude-to-Chat stream state %T", info.ClaudeToChatStreamState)
		}
		return state, nil
	}

	state := relayconvert.NewClaudeToChatStreamState()
	if info != nil {
		info.ClaudeToChatStreamState = state
	}
	return state, nil
}

func claudeToGeminiStreamState(info *relaycommon.RelayInfo) (*relayconvert.ResponseStreamState, error) {
	if info != nil && info.ChatToGeminiStreamState != nil {
		state, ok := info.ChatToGeminiStreamState.(*relayconvert.ResponseStreamState)
		if !ok || state == nil {
			return nil, fmt.Errorf("invalid Claude-to-Gemini stream state %T", info.ChatToGeminiStreamState)
		}
		return state, nil
	}

	state, err := relayconvert.NewResponseStreamState(types.RelayFormatClaude, types.RelayFormatGemini, relayconvert.ResponseStreamOptions{})
	if err != nil {
		return nil, err
	}
	if info != nil {
		info.ChatToGeminiStreamState = state
	}
	return state, nil
}

func sendGeminiStreamResults(c *gin.Context, results []relayconvert.ResponseResult) *types.NewAPIError {
	for _, result := range results {
		geminiResponse, ok := result.Value.(*dto.GeminiChatResponse)
		if !ok {
			return types.NewError(fmt.Errorf("expected Gemini stream response, got %T", result.Value), types.ErrorCodeBadResponseBody)
		}
		if geminiResponse == nil {
			continue
		}
		data, err := common.Marshal(geminiResponse)
		if err != nil {
			return types.NewError(err, types.ErrorCodeBadResponseBody)
		}
		c.Render(-1, common.CustomEvent{Data: "data: " + string(data)})
		_ = helper.FlushWriter(c)
	}
	return nil
}

func countClaudeStreamBillableTools(c *gin.Context, info *relaycommon.RelayInfo, claudeResponse *dto.ClaudeResponse) {
	if claudeResponse == nil {
		return
	}
	if claudeResponse.Type == "content_block_start" &&
		claudeResponse.ContentBlock != nil &&
		claudeResponse.ContentBlock.Type == "tool_use" {
		info.CountBillableToolCall(dto.BuildInCallToolUse, claudeResponse.ContentBlock.Name)
	}
	if claudeResponse.Type == "message_delta" &&
		claudeResponse.Usage != nil &&
		claudeResponse.Usage.ServerToolUse != nil &&
		claudeResponse.Usage.ServerToolUse.WebSearchRequests > 0 {
		c.Set("claude_web_search_requests", claudeResponse.Usage.ServerToolUse.WebSearchRequests)
	}
}

func checkClaudeStreamBudget(c *gin.Context, info *relaycommon.RelayInfo, claudeInfo *ClaudeResponseInfo, response *dto.ClaudeResponse) *types.NewAPIError {
	countClaudeStreamBillableTools(c, info, response)
	if service.CreditBillingRequestID(info) == 0 {
		return nil
	}
	observation := service.CreditUsageObservation(info)
	usage := *claudeInfo.Usage
	applyClaudeCreditUsage(&observation, &usage, claudeInfo.ResponseText.String())
	err := service.CheckCreditStreamBudget(c, &observation, &usage)
	if err != nil {
		info.MarkStreamBudgetStop(err)
	}
	return err
}

func HandleStreamFinalResponse(c *gin.Context, info *relaycommon.RelayInfo, claudeInfo *ClaudeResponseInfo) {
	if claudeInfo.Usage.PromptTokens == 0 {
		//上游出错
	}
	if info.BillingSource == service.BillingSourceCreditPacks {
		applyClaudeCreditUsage(info, claudeInfo.Usage, claudeInfo.ResponseText.String())
	} else if claudeInfo.Usage.CompletionTokens == 0 || !claudeInfo.Done {
		if common.DebugEnabled {
			common.SysLog("claude response usage is not complete, maybe upstream error")
		}
		// 只补缺失字段，不整份覆盖——保留 message_start 已拿到的 cache 字段
		fallback := service.ResponseText2Usage(c, claudeInfo.ResponseText.String(), info.UpstreamModelName, info.GetEstimatePromptTokens())
		if claudeInfo.Usage.CompletionTokens == 0 ||
			(!claudeInfo.Done && fallback.CompletionTokens > claudeInfo.Usage.CompletionTokens) {
			claudeInfo.Usage.CompletionTokens = fallback.CompletionTokens
		}
		if claudeInfo.Usage.PromptTokens == 0 {
			claudeInfo.Usage.PromptTokens = fallback.PromptTokens
		}
		claudeInfo.Usage.TotalTokens = claudeInfo.Usage.PromptTokens + claudeInfo.Usage.CompletionTokens
	}
	if claudeInfo.Usage != nil {
		claudeInfo.Usage.UsageSemantic = "anthropic"
	}
	if info.BillingSource != service.BillingSourceCreditPacks {
		relayconvert.FinalizeClaudeStreamBillingUsage(claudeInfo)
	}

	if info.RelayFormat == types.RelayFormatClaude {
		//
	} else if info.RelayFormat == types.RelayFormatOpenAI {
		if info.ShouldIncludeUsage {
			openAIUsage := buildOpenAIStyleUsageFromClaudeUsage(claudeInfo.Usage)
			response := helper.GenerateFinalUsageResponse(claudeInfo.ResponseId, claudeInfo.Created, info.UpstreamModelName, openAIUsage)
			err := helper.ObjectData(c, response)
			if err != nil {
				common.SysLog("send final response failed: " + err.Error())
			}
		}
		helper.Done(c)
	} else if info.RelayFormat == types.RelayFormatGemini {
		state, err := claudeToGeminiStreamState(info)
		if err != nil {
			common.SysLog("error creating Gemini stream state: " + err.Error())
			return
		}
		results, err := service.FinalizeStreamResponse(c, info, state)
		if err != nil {
			common.SysLog("error finalizing Gemini stream response: " + err.Error())
			return
		}
		if sendErr := sendGeminiStreamResults(c, results); sendErr != nil {
			common.SysLog("send final Gemini stream response failed: " + sendErr.Error())
		}
	}
}

func ClaudeStreamHandler(c *gin.Context, resp *http.Response, info *relaycommon.RelayInfo) (*dto.Usage, *types.NewAPIError) {
	claudeInfo := &ClaudeResponseInfo{
		ResponseId:   helper.GetResponseID(c),
		Created:      common.GetTimestamp(),
		Model:        info.UpstreamModelName,
		ResponseText: strings.Builder{},
		Usage:        &dto.Usage{},
	}
	var err *types.NewAPIError
	helper.StreamScannerHandler(c, resp, info, func(data string, sr *helper.StreamResult) {
		err = HandleStreamResponseData(c, info, claudeInfo, data)
		if err != nil {
			sr.Stop(err)
		}
	})
	info.StreamStatus.RequireTerminal()
	if err != nil {
		if info.CreditStreamBudgetStop == "quota_budget_exhausted" {
			applyClaudeCreditUsage(info, claudeInfo.Usage, claudeInfo.ResponseText.String())
			_ = helper.StreamError(c, info.RelayFormat, err)
			return claudeInfo.Usage, nil
		}
		return nil, err
	}

	HandleStreamFinalResponse(c, info, claudeInfo)
	return claudeInfo.Usage, nil
}

func HandleClaudeResponseData(c *gin.Context, info *relaycommon.RelayInfo, claudeInfo *ClaudeResponseInfo, httpResp *http.Response, data []byte) *types.NewAPIError {
	var claudeResponse dto.ClaudeResponse
	err := common.Unmarshal(data, &claudeResponse)
	if err != nil {
		return types.NewError(err, types.ErrorCodeBadResponseBody)
	}
	if claudeError := claudeResponse.GetClaudeError(); claudeError != nil && claudeError.Type != "" {
		return types.WithClaudeError(*claudeError, http.StatusInternalServerError)
	}
	if err := observeClaudeCreditUsage(info, &claudeResponse, data); err != nil {
		return types.NewError(err, types.ErrorCodeBadResponseBody, types.ErrOptionWithSkipRetry())
	}
	info.ObserveResponseModel(claudeResponse.Model)
	maybeMarkClaudeRefusal(c, info, claudeResponse.StopReason)
	if claudeInfo.Usage == nil {
		claudeInfo.Usage = &dto.Usage{}
	}
	if claudeResponse.Usage != nil {
		claudeInfo.Usage.PromptTokens = claudeResponse.Usage.InputTokens
		claudeInfo.Usage.CompletionTokens = claudeResponse.Usage.OutputTokens
		claudeInfo.Usage.TotalTokens = claudeResponse.Usage.InputTokens + claudeResponse.Usage.OutputTokens
		claudeInfo.Usage.UsageSemantic = "anthropic"
		claudeInfo.Usage.BillingUsage = dto.CloneBillingUsage(claudeResponse.Usage.BillingUsage)
		if claudeInfo.Usage.BillingUsage == nil {
			claudeInfo.Usage.BillingUsage = dto.NewClaudeMessagesBillingUsage(claudeResponse.Usage)
		}
		claudeInfo.Usage.PromptTokensDetails.CachedTokens = claudeResponse.Usage.CacheReadInputTokens
		claudeInfo.Usage.PromptTokensDetails.CachedCreationTokens = claudeResponse.Usage.CacheCreationInputTokens
		claudeInfo.Usage.ClaudeCacheCreation5mTokens = claudeResponse.Usage.GetCacheCreation5mTokens()
		claudeInfo.Usage.ClaudeCacheCreation1hTokens = claudeResponse.Usage.GetCacheCreation1hTokens()
	}
	if info.BillingSource == service.BillingSourceCreditPacks {
		var output strings.Builder
		for _, block := range claudeResponse.Content {
			output.WriteString(block.GetText())
			if block.Thinking != nil {
				output.WriteString(*block.Thinking)
			}
			if block.Type == "tool_use" && block.Input != nil {
				arguments, marshalErr := common.Marshal(block.Input)
				if marshalErr != nil {
					return types.NewError(marshalErr, types.ErrorCodeBadResponseBody)
				}
				output.Write(arguments)
			}
		}
		applyClaudeCreditUsage(info, claudeInfo.Usage, output.String())
	}
	var responseData []byte
	switch info.RelayFormat {
	case types.RelayFormatOpenAI:
		openaiResponse := ResponseClaude2OpenAI(&claudeResponse)
		openaiResponse.Usage = buildOpenAIStyleUsageFromClaudeUsage(claudeInfo.Usage)
		responseData, err = common.Marshal(openaiResponse)
		if err != nil {
			return types.NewError(err, types.ErrorCodeBadResponseBody)
		}
	case types.RelayFormatOpenAIResponses:
		convertResult, err := service.ConvertResponse(c, info, types.RelayFormatOpenAIResponses, &claudeResponse)
		if err != nil {
			return types.NewError(err, types.ErrorCodeBadResponseBody)
		}
		responsesResponse, ok := convertResult.Value.(*dto.OpenAIResponsesResponse)
		if !ok {
			return types.NewError(fmt.Errorf("expected OpenAI Responses response, got %T", convertResult.Value), types.ErrorCodeBadResponseBody)
		}
		if responseID := helper.GetResponseID(c); responseID != "" {
			responsesResponse.ID = responseID
		}
		responseData, err = common.Marshal(responsesResponse)
		if err != nil {
			return types.NewError(err, types.ErrorCodeBadResponseBody)
		}
	case types.RelayFormatClaude:
		responseData = data
	case types.RelayFormatGemini:
		{
			convertResult, convertErr := service.ConvertResponse(c, info, types.RelayFormatGemini, &claudeResponse)
			if convertErr != nil {
				return types.NewError(convertErr, types.ErrorCodeBadResponseBody)
			}
			geminiResponse, ok := convertResult.Value.(*dto.GeminiChatResponse)
			if !ok {
				return types.NewError(fmt.Errorf("expected Gemini generateContent response, got %T", convertResult.Value), types.ErrorCodeBadResponseBody)
			}
			responseData, err = common.Marshal(geminiResponse)
			if err != nil {
				return types.NewError(err, types.ErrorCodeBadResponseBody)
			}
		}
	}

	if claudeResponse.Usage != nil && claudeResponse.Usage.ServerToolUse != nil && claudeResponse.Usage.ServerToolUse.WebSearchRequests > 0 {
		c.Set("claude_web_search_requests", claudeResponse.Usage.ServerToolUse.WebSearchRequests)
	}

	for _, block := range claudeResponse.Content {
		if block.Type == "tool_use" {
			info.CountBillableToolCall(dto.BuildInCallToolUse, block.Name)
		}
	}

	service.IOCopyBytesGracefully(c, httpResp, responseData)
	return nil
}

func ClaudeHandler(c *gin.Context, resp *http.Response, info *relaycommon.RelayInfo) (*dto.Usage, *types.NewAPIError) {
	defer service.CloseResponseBodyGracefully(resp)

	claudeInfo := &ClaudeResponseInfo{
		ResponseId:   helper.GetResponseID(c),
		Created:      common.GetTimestamp(),
		Model:        info.UpstreamModelName,
		ResponseText: strings.Builder{},
		Usage:        &dto.Usage{},
	}
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, types.NewError(err, types.ErrorCodeBadResponseBody)
	}
	logger.LogDebug(c, "responseBody: %s", responseBody)
	handleErr := HandleClaudeResponseData(c, info, claudeInfo, resp, responseBody)
	if handleErr != nil {
		return nil, handleErr
	}
	return claudeInfo.Usage, nil
}

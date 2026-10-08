package openai

import (
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/logger"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/relayconvert"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
)

func OaiChatToResponsesHandler(c *gin.Context, info *relaycommon.RelayInfo, resp *http.Response) (*dto.Usage, *types.NewAPIError) {
	if resp == nil || resp.Body == nil {
		return nil, types.NewOpenAIError(fmt.Errorf("invalid response"), types.ErrorCodeBadResponse, http.StatusInternalServerError)
	}
	defer service.CloseResponseBodyGracefully(resp)

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeReadResponseBodyFailed, http.StatusInternalServerError)
	}

	var chatResp dto.OpenAITextResponse
	if err := common.Unmarshal(body, &chatResp); err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeBadResponseBody, http.StatusInternalServerError)
	}
	if oaiError := chatResp.GetOpenAIError(); oaiError != nil && oaiError.Type != "" {
		return nil, types.WithOpenAIError(*oaiError, resp.StatusCode)
	}

	if err := observeOpenaiCreditUsage(info, body, true); err != nil {
		return nil, types.NewError(err, types.ErrorCodeBadResponseBody, types.ErrOptionWithSkipRetry())
	}
	if info.BillingSource == service.BillingSourceCreditPacks {
		texts := make([]string, 0, len(chatResp.Choices))
		for _, choice := range chatResp.Choices {
			texts = append(texts, choice.Message.StringContent()+choice.Message.GetReasoningContent())
			for _, call := range choice.Message.ParseToolCalls() {
				info.CountBillableToolCall(dto.BuildInCallFunctionCall, call.Function.Name)
			}
		}
		applyOpenaiCreditTextUsage(info, &chatResp.Usage, texts, "new-api-text-count-v1", 0)
	}
	info.ObserveResponseModel(chatResp.Model)
	if responseID := helper.GetResponseID(c); responseID != "" {
		chatResp.Id = responseID
	}
	convertResult, err := service.ConvertResponse(c, info, types.RelayFormatOpenAIResponses, &chatResp)
	if err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeBadResponseBody, http.StatusInternalServerError)
	}
	responsesResp, ok := convertResult.Value.(*dto.OpenAIResponsesResponse)
	if !ok {
		return nil, types.NewOpenAIError(fmt.Errorf("expected OpenAI responses response, got %T", convertResult.Value), types.ErrorCodeBadResponseBody, http.StatusInternalServerError)
	}
	usage := convertResult.Usage
	if info.BillingSource != service.BillingSourceCreditPacks && (usage == nil || usage.TotalTokens == 0) {
		text := service.ExtractOutputTextFromResponses(responsesResp)
		usage = service.ResponseText2Usage(c, text, info.UpstreamModelName, info.GetEstimatePromptTokens())
		responsesResp.Usage = relayconvert.UsageFromChatUsage(usage)
	}

	responseBody, err := common.Marshal(responsesResp)
	if err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeJsonMarshalFailed, http.StatusInternalServerError)
	}

	service.IOCopyBytesGracefully(c, resp, responseBody)
	return usage, nil
}

func OaiChatToResponsesStreamHandler(c *gin.Context, info *relaycommon.RelayInfo, resp *http.Response) (*dto.Usage, *types.NewAPIError) {
	if resp == nil || resp.Body == nil {
		return nil, types.NewOpenAIError(fmt.Errorf("invalid response"), types.ErrorCodeBadResponse, http.StatusInternalServerError)
	}
	defer service.CloseResponseBodyGracefully(resp)

	responseID := helper.GetResponseID(c)
	state, err := relayconvert.NewResponseStreamState(types.RelayFormatOpenAI, types.RelayFormatOpenAIResponses, relayconvert.ResponseStreamOptions{
		ID:                 responseID,
		Model:              info.UpstreamModelName,
		EmitSequenceNumber: true,
	})
	if err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeBadResponse, http.StatusInternalServerError)
	}
	streamErr := (*types.NewAPIError)(nil)
	var budgetStop *types.NewAPIError
	var outputText strings.Builder
	var toolCount int
	seenCalls := make(map[string]struct{})
	var callNames []string
	usage := &dto.Usage{}
	lastData := ""
	finishObserved := false

	sendEvent := func(event relayconvert.ChatToResponsesStreamEvent) bool {
		data, err := common.Marshal(event.Payload)
		if err != nil {
			streamErr = types.NewOpenAIError(err, types.ErrorCodeJsonMarshalFailed, http.StatusInternalServerError)
			return false
		}
		if err := helper.ResponseChunkData(c, dto.ResponsesStreamResponse{Type: event.Type}, string(data)); err != nil {
			streamErr = types.NewOpenAIError(err, types.ErrorCodeBadResponse, http.StatusInternalServerError)
			return false
		}
		return true
	}
	failResponsesStream := func(err error) bool {
		failureResults, handled := state.FailResponsesStream("server_error", err.Error(), "")
		if !handled {
			return false
		}
		for _, result := range failureResults {
			event, ok := result.Value.(relayconvert.ChatToResponsesStreamEvent)
			if !ok {
				streamErr = types.NewOpenAIError(fmt.Errorf("expected OAI responses stream event, got %T", result.Value), types.ErrorCodeBadResponse, http.StatusInternalServerError)
				return true
			}
			if !sendEvent(event) {
				return true
			}
		}
		return true
	}

	helper.StreamScannerHandler(c, resp, info, func(data string, sr *helper.StreamResult) {
		if streamErr != nil {
			sr.Stop(streamErr)
			return
		}

		var errorResp dto.OpenAITextResponse
		if err := common.UnmarshalJsonStr(data, &errorResp); err == nil {
			if oaiError := errorResp.GetOpenAIError(); oaiError != nil && oaiError.Type != "" {
				if failResponsesStream(fmt.Errorf("%s", oaiError.Message)) {
					sr.Stop(streamErr)
					return
				}
				streamErr = types.WithOpenAIError(*oaiError, resp.StatusCode)
				sr.Stop(streamErr)
				return
			}
		}

		var chunk dto.ChatCompletionsStreamResponse
		if err := common.UnmarshalJsonStr(data, &chunk); err != nil {
			logger.LogError(c, "failed to unmarshal chat stream response: "+err.Error())
			if failResponsesStream(err) {
				sr.Stop(streamErr)
				return
			}
			streamErr = types.NewOpenAIError(err, types.ErrorCodeBadResponseBody, http.StatusInternalServerError)
			sr.Stop(streamErr)
			return
		}

		if info.BillingSource == service.BillingSourceCreditPacks {
			for _, choice := range chunk.Choices {
				if choice.FinishReason != nil && *choice.FinishReason != "" {
					finishObserved = true
				}
			}
			if err := observeOpenaiCreditUsage(info, common.StringToByteSlice(data), finishObserved); err != nil {
				streamErr = types.NewError(err, types.ErrorCodeBadResponseBody, types.ErrOptionWithSkipRetry())
				sr.Stop(streamErr)
				return
			}
			lastData = data
			if chunk.Usage != nil {
				usage = dto.MergeUsageNonZero(usage, chunk.Usage)
			}
			observeStreamChoices(info, data, seenCalls, &callNames)
			if err := ProcessStreamResponse(chunk, &outputText, &toolCount); err != nil {
				streamErr = types.NewError(err, types.ErrorCodeBadResponseBody, types.ErrOptionWithSkipRetry())
				sr.Stop(streamErr)
				return
			}
			observation := service.CreditUsageObservation(info)
			metered := *usage
			applyOpenaiCreditTextUsage(&observation, &metered, []string{outputText.String()}, "new-api-stream-text-tools-v1", common.QuotaRound(float64(toolCount)*7))
			budgetStop = service.CheckCreditStreamBudget(c, &observation, &metered)
			if budgetStop != nil {
				info.MarkStreamBudgetStop(budgetStop)
				sr.Stop(nil)
				return
			}
		}
		info.ObserveResponseModel(chunk.Model)
		results, err := service.ConvertStreamResponseChunk(c, info, state, &chunk)
		if err != nil {
			if failResponsesStream(err) {
				sr.Stop(streamErr)
				return
			}
			streamErr = types.NewOpenAIError(err, types.ErrorCodeBadResponse, http.StatusInternalServerError)
			sr.Stop(streamErr)
			return
		}
		for _, result := range results {
			event, ok := result.Value.(relayconvert.ChatToResponsesStreamEvent)
			if !ok {
				streamErr = types.NewOpenAIError(fmt.Errorf("expected OAI responses stream event, got %T", result.Value), types.ErrorCodeBadResponse, http.StatusInternalServerError)
				sr.Stop(streamErr)
				return
			}
			if !sendEvent(event) {
				sr.Stop(streamErr)
				return
			}
		}
	})

	if streamErr != nil {
		return nil, streamErr
	}

	if info.BillingSource == service.BillingSourceCreditPacks {
		if budgetStop == nil && info.StreamStatus.EndReason == relaycommon.StreamEndReasonDone {
			// Only a consumed terminal receipt, not a buffered transport tail,
			// turns cumulative output observations into complete usage.
			if err := observeOpenaiCreditUsage(info, common.StringToByteSlice(lastData), true); err != nil {
				return nil, types.NewError(err, types.ErrorCodeBadResponseBody, types.ErrOptionWithSkipRetry())
			}
		}
		applyOpenaiCreditTextUsage(info, usage, []string{outputText.String()}, "new-api-stream-text-tools-v1", common.QuotaRound(float64(toolCount)*7))
		state.SetUsage(usage)
	} else {
		usage = state.Usage()
		if usage == nil || usage.TotalTokens == 0 {
			usage = service.ResponseText2Usage(c, state.UsageText(), info.UpstreamModelName, info.GetEstimatePromptTokens())
			state.SetUsage(usage)
		}
	}
	if budgetStop != nil {
		if budgetStop.GetErrorCode() != "quota_budget_exhausted" {
			return nil, budgetStop
		}
		_ = helper.StreamError(c, info.RelayFormat, budgetStop)
		return usage, nil
	}

	finalResults, err := service.FinalizeStreamResponse(c, info, state)
	if err != nil {
		if failResponsesStream(err) {
			return usage, streamErr
		}
		return nil, types.NewOpenAIError(err, types.ErrorCodeBadResponse, http.StatusInternalServerError)
	}
	for _, result := range finalResults {
		event, ok := result.Value.(relayconvert.ChatToResponsesStreamEvent)
		if !ok {
			return nil, types.NewOpenAIError(fmt.Errorf("expected OAI responses stream event, got %T", result.Value), types.ErrorCodeBadResponse, http.StatusInternalServerError)
		}
		if !sendEvent(event) {
			return nil, streamErr
		}
	}

	return usage, nil
}

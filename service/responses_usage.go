package service

import (
	"fmt"
	"strings"

	"github.com/QuantumNous/new-api/common"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/relayconvert"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/gin-gonic/gin"
)

// ResponsesUsageAccumulator owns the accounting facts for one Responses stream.
// HTTP SSE and WebSocket transports feed the same events into it, then settle
// Finish's usage through the normal text billing path, including interrupted
// streams. Observe and Finish must be called by the same stream owner.
type ResponsesUsageAccumulator struct {
	info           *relaycommon.RelayInfo
	usage          *dto.Usage
	outputText     strings.Builder
	imageCounter   relaycommon.ImageGenerationCallCounter
	imageCommitted bool
	started        bool
	finished       bool
}

func NewResponsesUsageAccumulator(info *relaycommon.RelayInfo) *ResponsesUsageAccumulator {
	return &ResponsesUsageAccumulator{info: info, usage: &dto.Usage{}}
}

func (a *ResponsesUsageAccumulator) Observe(event *dto.ResponsesStreamResponse) {
	if a == nil || event == nil || a.finished {
		return
	}
	if event.Response != nil {
		a.info.ObserveResponseModel(event.Response.Model)
	}
	a.started = true
	ObserveResponsesOutcome(a.info, event)
	switch event.Type {
	case "response.completed", "response.done", "response.failed", "response.incomplete", "response.cancelled", "response.canceled":
		if event.Response != nil {
			ApplyResponsesUsage(a.usage, event.Response.Usage)
			if a.outputText.Len() == 0 {
				// Some upstreams carry the output only on the terminal event.
				a.outputText.WriteString(relayconvert.ExtractOutputTextFromResponses(event.Response))
			}
		}
		if a.imageCommitted {
			return
		}
		failed := event.Type != "response.completed" && event.Type != "response.done"
		if failed || (event.Response != nil && relaycommon.IsNonBillableResponsesStatus(event.Response.Status)) {
			a.imageCounter.Reset()
		} else if event.Response != nil {
			for i := range event.Response.Output {
				a.imageCounter.Observe(&event.Response.Output[i], &i)
			}
		}
		a.imageCounter.Commit(a.info)
		a.imageCommitted = true
	case "response.output_text.delta", "response.function_call_arguments.delta",
		"response.reasoning_summary_text.delta", "response.reasoning_text.delta", "response.refusal.delta":
		// Every delta kind here is generated output that upstream bills as
		// output tokens, so all of them feed the missing-usage estimate.
		a.outputText.WriteString(event.Delta)
	case dto.ResponsesOutputTypeItemDone:
		if event.Item == nil {
			return
		}
		switch event.Item.Type {
		case dto.BuildInCallWebSearchCall, dto.BuildInCallFileSearchCall, dto.BuildInCallFunctionCall:
			a.info.CountBillableToolCall(event.Item.Type, event.Item.Name)
		case dto.ResponsesOutputTypeImageGenerationCall:
			if !a.imageCommitted {
				a.imageCounter.Observe(event.Item, event.OutputIndex)
			}
		}
	}
}

// CheckBudget prices observed output without finalizing the stream, changing
// original metering facts, or committing pending image counts to settlement.
func (a *ResponsesUsageAccumulator) CheckBudget(c *gin.Context) *types.NewAPIError {
	if a == nil || a.finished || CreditBillingRequestID(a.info) == 0 {
		return nil
	}
	observation := CreditUsageObservation(a.info)
	usage := *a.usage
	ApplyResponsesCreditUsage(&observation, &usage, a.outputText.String())
	if !a.imageCommitted {
		a.imageCounter.Commit(&observation)
	}
	return CheckCreditStreamBudget(c, &observation, &usage)
}

func (a *ResponsesUsageAccumulator) Finish() *dto.Usage {
	if a.finished {
		return a.usage
	}
	a.finished = true
	// A final image item can already have reached the client before the stream
	// disconnects. Explicit failed/incomplete terminals reset and commit zero in
	// Observe; otherwise retain completed tool usage even without a terminal.
	if !a.imageCommitted {
		a.imageCounter.Commit(a.info)
		a.imageCommitted = true
	}
	if a.info.BillingSource == BillingSourceCreditPacks {
		ApplyResponsesCreditUsage(a.info, a.usage, a.outputText.String())
		return a.usage
	}
	if a.usage.CompletionTokens == 0 {
		if output := a.outputText.String(); output != "" {
			a.usage.CompletionTokens = CountTextToken(output, a.info.GetUpstreamModelName())
		}
	}
	// Upstream bills the prompt as soon as it starts generating, so a stream
	// that produced any event but no usage still owes its input tokens unless
	// upstream reported an explicit failure.
	billsPrompt := a.usage.CompletionTokens != 0 || (a.started && !a.info.StreamStatus.ResponseFailed())
	if a.usage.PromptTokens == 0 && billsPrompt {
		a.usage.PromptTokens = a.info.GetEstimatePromptTokens()
	}
	a.usage.TotalTokens = a.usage.PromptTokens + a.usage.CompletionTokens
	if a.usage.BillingUsage != nil {
		a.usage.BillingUsage = dto.CloneBillingUsageWithEstimatedCompletion(a.usage.BillingUsage, a.usage.CompletionTokens)
	}
	return a.usage
}

// ObserveResponsesOutcome records the protocol outcome of one Responses event
// on the stream status for health classification. Only codes and types are
// kept; messages never leave the event.
func ObserveResponsesOutcome(info *relaycommon.RelayInfo, event *dto.ResponsesStreamResponse) {
	if info == nil || info.StreamStatus == nil || event == nil {
		return
	}
	var responseStatus string
	if event.Response != nil {
		_ = common.Unmarshal(event.Response.Status, &responseStatus)
	}
	switch {
	case event.Type == "error" || event.Type == "response.failed" || event.Type == "response.error" || responseStatus == "failed":
		code, errorType := event.Code, ""
		if event.Response != nil {
			if oaiErr := event.Response.GetOpenAIError(); oaiErr != nil {
				if oaiErr.Code != nil {
					code = fmt.Sprint(oaiErr.Code)
				}
				errorType = oaiErr.Type
			}
		}
		info.StreamStatus.MarkFailed(code, errorType, 0)
	case event.Type == "response.incomplete" || responseStatus == "incomplete":
		reason := ""
		if event.Response != nil && event.Response.IncompleteDetails != nil {
			reason = event.Response.IncompleteDetails.Reason
		}
		info.StreamStatus.MarkIncomplete(reason)
	case event.Type == "response.cancelled" || event.Type == "response.canceled" || responseStatus == "cancelled":
		info.StreamStatus.MarkCancelled()
	case event.Type == "response.completed" || event.Type == "response.done" || responseStatus == "completed":
		info.StreamStatus.MarkCompleted()
	}
}

func ApplyResponsesUsage(dst *dto.Usage, src *dto.Usage) {
	if dst == nil || src == nil {
		return
	}
	incoming := relayconvert.NormalizeResponsesUsage(src)
	if src.InputTokensDetails != nil {
		inputDetails := *src.InputTokensDetails
		incoming.InputTokensDetails = &inputDetails
	}
	if src.OutputTokensDetails != nil {
		incoming.CompletionTokenDetails = *src.OutputTokensDetails
	}
	incoming.PromptCacheHitTokens = src.PromptCacheHitTokens
	dto.MergeUsageNonZero(dst, incoming)
	outputDetails := dst.CompletionTokenDetails
	if outputDetails != (dto.OutputTokenDetails{}) {
		dst.OutputTokensDetails = &outputDetails
	}
}

// HTTP and WebSocket transports observe the original wire fields before any
// DTO conversion can replace absent integers with zero. Prefix is a protocol
// location (usage. for JSON, response.usage. for stream events).
func ObserveResponsesCreditUsage(info *relaycommon.RelayInfo, body []byte, prefix string, eventType string) error {
	if info.BillingSource != BillingSourceCreditPacks {
		return nil
	}
	final := eventType == ""
	switch eventType {
	case "response.completed", "response.done", "response.failed", "response.incomplete", "response.cancelled", "response.canceled":
		final = true
	}
	return ObserveCreditUsage(info, body, map[string]string{
		"prompt_tokens":         prefix + "input_tokens",
		"completion_tokens":     prefix + "output_tokens",
		"total_tokens":          prefix + "total_tokens",
		"cached_tokens":         prefix + "input_tokens_details.cached_tokens",
		"cache_creation_tokens": prefix + "input_tokens_details.cache_write_tokens",
		"audio_input_tokens":    prefix + "input_tokens_details.audio_tokens",
		"image_input_tokens":    prefix + "input_tokens_details.image_tokens",
		"audio_output_tokens":   prefix + "output_tokens_details.audio_tokens",
		"image_output_tokens":   prefix + "output_tokens_details.image_tokens",
		"reasoning_tokens":      prefix + "output_tokens_details.reasoning_tokens",
	}, true, final)
}

func ApplyResponsesCreditUsage(info *relaycommon.RelayInfo, usage *dto.Usage, outputText string) {
	if info.BillingSource != BillingSourceCreditPacks {
		return
	}
	EstimateCreditUsageField(info, "prompt_tokens", info.GetEstimatePromptTokens(), "new-api-prompt-count-v1")
	EstimateCreditTextUsageField(info, "completion_tokens", []string{outputText}, info.GetUpstreamModelName(), "new-api-output-count-v1", 0)
	for field, target := range map[string]*int{
		"prompt_tokens":         &usage.PromptTokens,
		"completion_tokens":     &usage.CompletionTokens,
		"cached_tokens":         &usage.PromptTokensDetails.CachedTokens,
		"cache_creation_tokens": &usage.PromptTokensDetails.CacheWriteTokens,
		"audio_input_tokens":    &usage.PromptTokensDetails.AudioTokens,
		"image_input_tokens":    &usage.PromptTokensDetails.ImageTokens,
		"audio_output_tokens":   &usage.CompletionTokenDetails.AudioTokens,
		"image_output_tokens":   &usage.CompletionTokenDetails.ImageTokens,
		"reasoning_tokens":      &usage.CompletionTokenDetails.ReasoningTokens,
	} {
		if fact, ok := info.CreditUsageFacts[field]; ok && fact.Quantity != nil {
			*target = int(*fact.Quantity)
		}
	}
	usage.InputTokens, usage.OutputTokens = usage.PromptTokens, usage.CompletionTokens
	usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
	input, output := usage.PromptTokensDetails, usage.CompletionTokenDetails
	usage.InputTokensDetails, usage.OutputTokensDetails = &input, &output
	usage.BillingUsage = dto.NewOpenAIResponsesBillingUsage(usage)
	if usage.BillingUsage != nil {
		usage.BillingUsage.Estimated = info.CreditUsageFacts["prompt_tokens"].Source == "estimate" || info.CreditUsageFacts["completion_tokens"].Source == "estimate"
	}
}

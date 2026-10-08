package gemini

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/QuantumNous/new-api/common"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/relayconvert"
	"github.com/QuantumNous/new-api/service"
	hosttypes "github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

func buildUsageFromGeminiMetadata(metadata *dto.GeminiUsageMetadata, fallbackPromptTokens int) dto.Usage {
	usage := relayconvert.UsageFromGeminiMetadata(metadata, fallbackPromptTokens)
	if usage == nil {
		return dto.Usage{}
	}
	return *usage
}

func attachEstimatedGeminiBillingUsage(usage *dto.Usage) *dto.Usage {
	if usage != nil && usage.BillingUsage == nil {
		usage.BillingUsage = dto.NewEstimatedGeminiChatBillingUsage(usage)
	}
	return usage
}

// patchGeminiZeroCompletionUsage estimates completion tokens locally when upstream
// usageMetadata was billable but reported zero completion tokens even though output
// content was actually received. Typical case: the client aborts a stream before the
// final chunk that carries candidatesTokenCount, leaving prompt-only metadata; without
// this patch the output side would settle at zero quota.

func patchGeminiZeroCompletionUsage(c *gin.Context, info *relaycommon.RelayInfo, usage *dto.Usage, responseText string, imageCount int) {
	if usage == nil || usage.CompletionTokens > 0 {
		return
	}
	if responseText == "" && imageCount == 0 {
		return
	}
	estimated := service.ResponseText2Usage(c, responseText, info.UpstreamModelName, usage.PromptTokens)
	usage.CompletionTokens = estimated.CompletionTokens
	if imageCount != 0 && usage.CompletionTokens == 0 {
		usage.CompletionTokens = imageCount * 1400
	}
	usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
	// Settlement prefers BillingUsage, so fill the missing completion in the
	// original upstream dialect without discarding cache or modality details.
	if usage.BillingUsage != nil {
		usage.BillingUsage = dto.CloneBillingUsageWithEstimatedCompletion(usage.BillingUsage, usage.CompletionTokens)
	} else {
		usage.BillingUsage = dto.NewEstimatedGeminiChatBillingUsage(usage)
	}
}

func geminiResponseUsageText(response *dto.GeminiChatResponse) string {
	if response == nil {
		return ""
	}
	var text strings.Builder
	for _, candidate := range response.Candidates {
		for _, part := range candidate.Content.Parts {
			if part.Text != "" {
				text.WriteString(part.Text)
			}
		}
	}
	return text.String()
}

func buildUsageFromGeminiResponse(c *gin.Context, info *relaycommon.RelayInfo, response *dto.GeminiChatResponse) dto.Usage {
	metadata := response.GetUsageMetadata()
	if info.BillingSource == service.BillingSourceCreditPacks {
		return applyGeminiCreditUsage(info, metadata, geminiResponseUsageText(response), geminiResponseInlineImageCount(response))
	}
	if dto.HasGeminiUsageMetadataTokens(metadata) {
		usage := buildUsageFromGeminiMetadata(metadata, info.GetEstimatePromptTokens())
		patchGeminiZeroCompletionUsage(c, info, &usage, geminiResponseUsageText(response), geminiResponseInlineImageCount(response))
		return usage
	}
	usage := service.ResponseText2Usage(c, geminiResponseUsageText(response), info.UpstreamModelName, info.GetEstimatePromptTokens())
	attachEstimatedGeminiBillingUsage(usage)
	return *usage
}

// Gemini's input and output aggregates have different components from OpenAI.
// Keep the reported components and modality entries before deriving host sums.

func observeGeminiCreditUsage(info *relaycommon.RelayInfo, data []byte, final bool) error {
	if info.BillingSource != service.BillingSourceCreditPacks {
		return nil
	}
	paths := map[string]string{
		"gemini_prompt_tokens":     "usageMetadata.promptTokenCount",
		"gemini_tool_input_tokens": "usageMetadata.toolUsePromptTokenCount",
		"gemini_candidate_tokens":  "usageMetadata.candidatesTokenCount",
		"reasoning_tokens":         "usageMetadata.thoughtsTokenCount",
		"cached_tokens":            "usageMetadata.cachedContentTokenCount",
		"total_tokens":             "usageMetadata.totalTokenCount",
	}
	groups := map[string]string{"prompt": "promptTokensDetails", "tool": "toolUsePromptTokensDetails", "output": "candidatesTokensDetails"}
	for group, path := range groups {
		for i, detail := range gjson.GetBytes(data, "usageMetadata."+path).Array() {
			modality := strings.ToLower(strings.TrimSpace(detail.Get("modality").String()))
			if modality != "text" && modality != "audio" && modality != "image" {
				continue
			}
			paths["gemini_"+group+"_"+modality+"_"+strconv.Itoa(i)] = "usageMetadata." + path + "." + strconv.Itoa(i) + ".tokenCount"
		}
	}
	if err := service.ObserveCreditUsage(info, data, paths, true, final); err != nil {
		return err
	}
	for group, path := range groups {
		details := gjson.GetBytes(data, "usageMetadata."+path)
		if !details.Exists() || details.Type == gjson.Null {
			continue
		}
		if !details.IsArray() {
			return fmt.Errorf("invalid Gemini modality details")
		}
		for _, modality := range []string{"text", "audio", "image"} {
			quantity := float64(0)
			for _, detail := range details.Array() {
				if strings.EqualFold(strings.TrimSpace(detail.Get("modality").String()), modality) {
					value := detail.Get("tokenCount")
					if !value.Exists() || value.Type == gjson.Null {
						return fmt.Errorf("missing Gemini modality quantity")
					}
					quantity += value.Num
				}
			}
			if quantity > float64(common.MaxQuota) {
				return fmt.Errorf("Gemini modality quantity exceeds billing bound")
			}
			if info.CreditUsageFacts == nil {
				info.CreditUsageFacts = make(map[string]hosttypes.UsageFact)
			}
			info.CreditUsageFacts["gemini_"+group+"_"+modality+"_tokens"] = hosttypes.UsageFact{Field: "gemini_" + group + "_" + modality + "_tokens", Unit: "token", Quantity: &quantity, Source: "adaptor", Algorithm: "gemini-modality-sum-v1", Partial: group == "output" && !final}
		}
	}
	return nil
}

func applyGeminiCreditUsage(info *relaycommon.RelayInfo, metadata *dto.GeminiUsageMetadata, outputText string, imageCount int) dto.Usage {
	m := dto.GeminiUsageMetadata{}
	if metadata != nil {
		m = *metadata
	}
	service.EstimateCreditUsageField(info, "gemini_prompt_tokens", info.GetEstimatePromptTokens(), "new-api-prompt-count-v1")
	estimatedOutput, estimation := service.CountTextTokensWithEstimation([]string{outputText}, info.UpstreamModelName)
	algorithm := "new-api-output-count-v1"
	if estimatedOutput == 0 && imageCount > 0 {
		estimatedOutput = common.QuotaRound(float64(imageCount) * 1400)
		algorithm = "new-api-gemini-image-estimate-v1"
		estimation = &hosttypes.UsageEstimation{Version: algorithm, Model: info.UpstreamModelName, Method: "image-count", Quantity: float64(estimatedOutput), Parameters: map[string]float64{"images": float64(imageCount), "tokens_per_image": 1400}}
	}
	service.EstimateCreditUsageField(info, "gemini_candidate_tokens", estimatedOutput, algorithm, estimation)
	// Missing optional categories remain unknown; DTO zero defaults are not
	// promoted into provider receipts merely because a sum needs a number.
	for _, field := range []string{"gemini_tool_input_tokens", "reasoning_tokens"} {
		if _, exists := info.CreditUsageFacts[field]; !exists {
			info.CreditUsageFacts[field] = hosttypes.UsageFact{Field: field, Unit: "token", Source: "unknown"}
		}
	}
	for field, target := range map[string]*int{
		"gemini_prompt_tokens": &m.PromptTokenCount, "gemini_tool_input_tokens": &m.ToolUsePromptTokenCount,
		"gemini_candidate_tokens": &m.CandidatesTokenCount, "reasoning_tokens": &m.ThoughtsTokenCount,
		"cached_tokens": &m.CachedContentTokenCount,
	} {
		if fact := info.CreditUsageFacts[field]; fact.Quantity != nil {
			*target = int(*fact.Quantity)
		}
	}
	prompt, clamp := common.QuotaRoundChecked(float64(m.PromptTokenCount) + float64(m.ToolUsePromptTokenCount))
	if clamp != nil {
		info.QuotaClamp = clamp
	}
	completion, clamp := common.QuotaRoundChecked(float64(m.CandidatesTokenCount) + float64(m.ThoughtsTokenCount))
	if clamp != nil {
		info.QuotaClamp = clamp
	}
	for _, sum := range []struct {
		field, algorithm string
		components       []string
		quantity         int
	}{
		{"prompt_tokens", "gemini-input-with-tool-v1", []string{"gemini_prompt_tokens", "gemini_tool_input_tokens"}, prompt},
		{"completion_tokens", "gemini-output-with-thinking-v1", []string{"gemini_candidate_tokens", "reasoning_tokens"}, completion},
	} {
		source, partial := "adaptor", false
		for _, component := range sum.components {
			fact := info.CreditUsageFacts[component]
			partial = partial || fact.Partial
			if fact.Quantity == nil || fact.Source != "upstream" || fact.Partial {
				source = "estimate"
			}
		}
		quantity := float64(sum.quantity)
		info.CreditUsageFacts[sum.field] = hosttypes.UsageFact{Field: sum.field, Unit: "token", Quantity: &quantity, Source: source, Algorithm: sum.algorithm, Partial: partial}
	}
	// Modality sums remain separately labelled as adaptor normalization. The
	// raw indexed quantities above stay available as evidence of each report.
	for field, groups := range map[string][]string{
		"audio_input_tokens":  {"gemini_prompt_audio_tokens", "gemini_tool_audio_tokens"},
		"image_input_tokens":  {"gemini_prompt_image_tokens", "gemini_tool_image_tokens"},
		"audio_output_tokens": {"gemini_output_audio_tokens"},
		"image_output_tokens": {"gemini_output_image_tokens"},
	} {
		quantity, present, partial := float64(0), false, false
		for _, group := range groups {
			if fact, exists := info.CreditUsageFacts[group]; exists && fact.Quantity != nil {
				quantity += *fact.Quantity
				present = true
				partial = partial || fact.Partial
			}
		}
		if present {
			info.CreditUsageFacts[field] = hosttypes.UsageFact{Field: field, Unit: "token", Quantity: &quantity, Source: "adaptor", Algorithm: "gemini-modality-sum-v1", Partial: partial}
		}
	}
	m.TotalTokenCount = common.QuotaRound(float64(prompt) + float64(completion))
	m.BillingUsage = nil
	usage := buildUsageFromGeminiMetadata(&m, 0)
	usage.PromptTokens, usage.CompletionTokens, usage.TotalTokens = prompt, completion, m.TotalTokenCount
	usage.UsageSemantic = "gemini"
	for field, target := range map[string]*int{
		"audio_input_tokens": &usage.PromptTokensDetails.AudioTokens, "image_input_tokens": &usage.PromptTokensDetails.ImageTokens,
		"audio_output_tokens": &usage.CompletionTokenDetails.AudioTokens, "image_output_tokens": &usage.CompletionTokenDetails.ImageTokens,
	} {
		if fact, present := info.CreditUsageFacts[field]; present && fact.Quantity != nil {
			*target = common.QuotaRound(*fact.Quantity)
		}
	}
	// Freeze the same normalized values for CanonicalUsage; its source stays
	// Gemini rather than converting it into a fictitious OpenAI receipt.
	m.PromptTokensDetails = []dto.GeminiPromptTokensDetails{{Modality: "AUDIO", TokenCount: usage.PromptTokensDetails.AudioTokens}, {Modality: "IMAGE", TokenCount: usage.PromptTokensDetails.ImageTokens}, {Modality: "TEXT", TokenCount: usage.PromptTokensDetails.TextTokens}}
	m.ToolUsePromptTokensDetails = nil
	m.CandidatesTokensDetails = []dto.GeminiPromptTokensDetails{{Modality: "AUDIO", TokenCount: usage.CompletionTokenDetails.AudioTokens}, {Modality: "IMAGE", TokenCount: usage.CompletionTokenDetails.ImageTokens}, {Modality: "TEXT", TokenCount: usage.CompletionTokenDetails.TextTokens}}
	usage.BillingUsage = dto.NewGeminiChatBillingUsage(&m)
	if usage.BillingUsage != nil {
		usage.BillingUsage.Estimated = info.CreditUsageFacts["prompt_tokens"].Source == "estimate" || info.CreditUsageFacts["completion_tokens"].Source == "estimate"
	}
	return usage
}

func geminiResponseInlineImageCount(response *dto.GeminiChatResponse) int {
	if response == nil {
		return 0
	}
	count := 0
	for _, candidate := range response.Candidates {
		for _, part := range candidate.Content.Parts {
			if part.InlineData != nil && part.InlineData.MimeType != "" {
				count++
			}
		}
	}
	return count
}

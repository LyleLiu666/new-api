package openai

import (
	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/service"
	"maps"
)

var openaiCreditUsagePaths = map[string]string{
	"prompt_tokens":         "usage.prompt_tokens",
	"completion_tokens":     "usage.completion_tokens",
	"total_tokens":          "usage.total_tokens",
	"openai_cached_tokens":  "usage.prompt_tokens_details.cached_tokens",
	"cache_creation_tokens": "usage.prompt_tokens_details.cache_creation_tokens",
	"audio_input_tokens":    "usage.prompt_tokens_details.audio_tokens",
	"audio_output_tokens":   "usage.completion_tokens_details.audio_tokens",
	"reasoning_tokens":      "usage.completion_tokens_details.reasoning_tokens",
}

func observeOpenaiCreditUsage(info *relaycommon.RelayInfo, body []byte, final bool) error {
	if info.BillingSource != service.BillingSourceCreditPacks {
		return nil
	}
	paths := maps.Clone(openaiCreditUsagePaths)
	switch info.ChannelType {
	case constant.ChannelTypeDeepSeek:
		paths["provider_cached_tokens"] = "usage.prompt_cache_hit_tokens"
	case constant.ChannelTypeZhipu_v4:
		paths["provider_cached_tokens"] = "usage.input_tokens_details.cached_tokens"
	case constant.ChannelTypeMoonshot:
		paths["provider_cached_tokens"] = "choices.0.usage.cached_tokens"
	case constant.ChannelTypeOpenAI:
		paths["provider_cached_tokens"] = "timings.cache_n"
	}
	if err := service.ObserveCreditUsage(info, body, paths, true, final); err != nil {
		return err
	}
	alias, aliasPresent := info.CreditUsageFacts["provider_cached_tokens"]
	standard, standardPresent := info.CreditUsageFacts["openai_cached_tokens"]
	if !standardPresent {
		if !aliasPresent || alias.Quantity == nil {
			return nil
		}
		alias.Field, alias.Algorithm = "cached_tokens", "new-api-cache-alias-v1"
		info.CreditUsageFacts["cached_tokens"] = alias
		return nil
	}
	standard.Field = "cached_tokens"
	// Keep both conflicting receipts and preserve native compatibility pricing.
	// A compatibility-derived value cannot claim to be a verified raw receipt.
	// Raw and normalized keys remain separate across cumulative stream frames.
	if aliasPresent && alias.Quantity != nil && standard.Quantity != nil && *standard.Quantity != *alias.Quantity {
		if *standard.Quantity == 0 {
			standard.Quantity = alias.Quantity
		}
		standard.Source, standard.Algorithm = "adaptor", "new-api-cache-alias-conflict-v1"
	}
	info.CreditUsageFacts["cached_tokens"] = standard
	return nil
}

func applyUsagePostProcessing(info *relaycommon.RelayInfo, usage *dto.Usage, responseBody []byte) {
	if info == nil || usage == nil {
		return
	}

	switch info.ChannelType {
	case constant.ChannelTypeDeepSeek:
		if usage.PromptTokensDetails.CachedTokens == 0 && usage.PromptCacheHitTokens != 0 {
			usage.PromptTokensDetails.CachedTokens = usage.PromptCacheHitTokens
		}
	case constant.ChannelTypeZhipu_v4:
		// 智普的cached_tokens在标准位置: usage.prompt_tokens_details.cached_tokens
		if usage.PromptTokensDetails.CachedTokens == 0 {
			if usage.InputTokensDetails != nil && usage.InputTokensDetails.CachedTokens > 0 {
				usage.PromptTokensDetails.CachedTokens = usage.InputTokensDetails.CachedTokens
			} else if cachedTokens, ok := extractCachedTokensFromBody(responseBody); ok {
				usage.PromptTokensDetails.CachedTokens = cachedTokens
			} else if usage.PromptCacheHitTokens > 0 {
				usage.PromptTokensDetails.CachedTokens = usage.PromptCacheHitTokens
			}
		}
	case constant.ChannelTypeMoonshot:
		// Moonshot的cached_tokens在非标准位置: choices[].usage.cached_tokens
		if usage.PromptTokensDetails.CachedTokens == 0 {
			if usage.InputTokensDetails != nil && usage.InputTokensDetails.CachedTokens > 0 {
				usage.PromptTokensDetails.CachedTokens = usage.InputTokensDetails.CachedTokens
			} else if cachedTokens, ok := extractMoonshotCachedTokensFromBody(responseBody); ok {
				usage.PromptTokensDetails.CachedTokens = cachedTokens
			} else if cachedTokens, ok := extractCachedTokensFromBody(responseBody); ok {
				usage.PromptTokensDetails.CachedTokens = cachedTokens
			} else if usage.PromptCacheHitTokens > 0 {
				usage.PromptTokensDetails.CachedTokens = usage.PromptCacheHitTokens
			}
		}
	case constant.ChannelTypeOpenAI:
		if usage.PromptTokensDetails.CachedTokens == 0 {
			if cachedTokens, ok := extractLlamaCachedTokensFromBody(responseBody); ok {
				usage.PromptTokensDetails.CachedTokens = cachedTokens
			}
		}
	}
}

func extractCachedTokensFromBody(body []byte) (int, bool) {
	if len(body) == 0 {
		return 0, false
	}

	var payload struct {
		Usage struct {
			PromptTokensDetails struct {
				CachedTokens *int `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
			CachedTokens         *int `json:"cached_tokens"`
			PromptCacheHitTokens *int `json:"prompt_cache_hit_tokens"`
		} `json:"usage"`
	}

	if err := common.Unmarshal(body, &payload); err != nil {
		return 0, false
	}

	if payload.Usage.PromptTokensDetails.CachedTokens != nil {
		return *payload.Usage.PromptTokensDetails.CachedTokens, true
	}
	if payload.Usage.CachedTokens != nil {
		return *payload.Usage.CachedTokens, true
	}
	if payload.Usage.PromptCacheHitTokens != nil {
		return *payload.Usage.PromptCacheHitTokens, true
	}
	return 0, false
}

// extractMoonshotCachedTokensFromBody 从Moonshot的非标准位置提取cached_tokens
// Moonshot的流式响应格式: {"choices":[{"usage":{"cached_tokens":111}}]}
func extractMoonshotCachedTokensFromBody(body []byte) (int, bool) {
	if len(body) == 0 {
		return 0, false
	}

	var payload struct {
		Choices []struct {
			Usage struct {
				CachedTokens *int `json:"cached_tokens"`
			} `json:"usage"`
		} `json:"choices"`
	}

	if err := common.Unmarshal(body, &payload); err != nil {
		return 0, false
	}

	// 遍历choices查找cached_tokens
	for _, choice := range payload.Choices {
		if choice.Usage.CachedTokens != nil && *choice.Usage.CachedTokens > 0 {
			return *choice.Usage.CachedTokens, true
		}
	}

	return 0, false
}

// extractLlamaCachedTokensFromBody 从llama.cpp的非标准位置提取cache_n
func extractLlamaCachedTokensFromBody(body []byte) (int, bool) {
	if len(body) == 0 {
		return 0, false
	}

	var payload struct {
		Timings struct {
			CachedTokens *int `json:"cache_n"`
		} `json:"timings"`
	}

	if err := common.Unmarshal(body, &payload); err != nil {
		return 0, false
	}

	if payload.Timings.CachedTokens == nil {
		return 0, false
	}
	return *payload.Timings.CachedTokens, true
}

// The original Chat receipt remains the accounting source when client output
// is converted to Responses. DTO defaults are not evidence of reported zero.
func applyOpenaiCreditTextUsage(info *relaycommon.RelayInfo, usage *dto.Usage, texts []string, algorithm string, extraTokens int) {
	if info.BillingSource != service.BillingSourceCreditPacks {
		return
	}
	service.EstimateCreditUsageField(info, "prompt_tokens", info.GetEstimatePromptTokens(), "new-api-prompt-count-v1")
	service.EstimateCreditTextUsageField(info, "completion_tokens", texts, info.UpstreamModelName, algorithm, extraTokens)
	for field, target := range map[string]*int{
		"prompt_tokens": &usage.PromptTokens, "completion_tokens": &usage.CompletionTokens,
		"cached_tokens":         &usage.PromptTokensDetails.CachedTokens,
		"cache_creation_tokens": &usage.PromptTokensDetails.CacheWriteTokens,
		"audio_input_tokens":    &usage.PromptTokensDetails.AudioTokens,
		"audio_output_tokens":   &usage.CompletionTokenDetails.AudioTokens,
		"reasoning_tokens":      &usage.CompletionTokenDetails.ReasoningTokens,
	} {
		if fact := info.CreditUsageFacts[field]; fact.Quantity != nil {
			*target = int(*fact.Quantity)
		}
	}
	usage.TotalTokens = common.QuotaRound(float64(usage.PromptTokens) + float64(usage.CompletionTokens))
	usage.BillingUsage = dto.NewOpenAIChatBillingUsage(usage)
	if usage.BillingUsage != nil {
		usage.BillingUsage.Estimated = info.CreditUsageFacts["prompt_tokens"].Source == "estimate" || info.CreditUsageFacts["completion_tokens"].Source == "estimate"
	}
}

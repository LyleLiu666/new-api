package service

import (
	"runtime/debug"
	"sync"

	"github.com/QuantumNous/new-api/common"
	hosttypes "github.com/QuantumNous/new-api/types"
	"github.com/tiktoken-go/tokenizer"
	"github.com/tiktoken-go/tokenizer/codec"
)

// tokenEncoderMap won't grow after initialization
var defaultTokenEncoder tokenizer.Codec

// tokenEncoderMap is used to store token encoders for different models
var tokenEncoderMap = make(map[string]tokenizer.Codec)

// tokenEncoderMutex protects tokenEncoderMap for concurrent access
var tokenEncoderMutex sync.RWMutex

func InitTokenEncoders() {
	common.SysLog("initializing token encoders")
	tokenEncoderMutex.Lock()
	defaultTokenEncoder = codec.NewCl100kBase()
	tokenEncoderMutex.Unlock()
	common.SysLog("token encoders initialized")
}

func getTokenEncoder(model string) tokenizer.Codec {
	// First, try to get the encoder from cache with read lock
	tokenEncoderMutex.RLock()
	if encoder, exists := tokenEncoderMap[model]; exists {
		tokenEncoderMutex.RUnlock()
		return encoder
	}
	tokenEncoderMutex.RUnlock()

	// If not in cache, create new encoder with write lock
	tokenEncoderMutex.Lock()
	defer tokenEncoderMutex.Unlock()

	// Double-check if another goroutine already created the encoder
	if encoder, exists := tokenEncoderMap[model]; exists {
		return encoder
	}

	// Create new encoder
	modelCodec, err := tokenizer.ForModel(tokenizer.Model(model))
	if err != nil {
		// Initialization normally occurs at startup; utility callers also
		// need a real fallback codec, never a cached nil.
		if defaultTokenEncoder == nil {
			defaultTokenEncoder = codec.NewCl100kBase()
		}
		// Cache the default encoder for this model to avoid repeated failures
		tokenEncoderMap[model] = defaultTokenEncoder
		return defaultTokenEncoder
	}

	// Cache the new encoder
	tokenEncoderMap[model] = modelCodec
	return modelCodec
}

func getTokenNum(tokenEncoder tokenizer.Codec, text string) int {
	if text == "" {
		return 0
	}
	tkm, _ := tokenEncoder.Count(text)
	return tkm
}

var tokenizerBuildVersion = sync.OnceValue(func() string {
	if build, ok := debug.ReadBuildInfo(); ok {
		for _, dependency := range build.Deps {
			if dependency.Path != "github.com/tiktoken-go/tokenizer" {
				continue
			}
			if dependency.Replace != nil {
				dependency = dependency.Replace
			}
			if dependency.Version != "" {
				return dependency.Version
			}
			break
		}
	}
	return "unknown"
})

// CountTextTokensWithEstimation preserves independent segment rounding and
// captures one counter configuration for the entire observation.
func CountTextTokensWithEstimation(texts []string, model string) (int, *hosttypes.UsageEstimation) {
	evidence := &hosttypes.UsageEstimation{Version: "new-api-text-counter-v1", Model: model, Parameters: map[string]float64{"segments": float64(len(texts))}}
	var quantity float64
	if common.IsOpenAITextModel(model) {
		encoder := getTokenEncoder(model)
		evidence.Method, evidence.Tokenizer = "tiktoken", encoder.GetName()
		evidence.DependencyVersion = tokenizerBuildVersion()
		for _, text := range texts {
			quantity += float64(getTokenNum(encoder, text))
		}
	} else {
		weights := getMultipliers(tokenEstimatorProvider(model))
		evidence.Method = "provider-heuristic"
		for key, value := range map[string]float64{"word": weights.Word, "number": weights.Number, "cjk": weights.CJK, "symbol": weights.Symbol, "math_symbol": weights.MathSymbol, "url_delimiter": weights.URLDelim, "at_sign": weights.AtSign, "emoji": weights.Emoji, "newline": weights.Newline, "space": weights.Space, "base_padding": float64(weights.BasePad)} {
			evidence.Parameters[key] = value
		}
		for _, text := range texts {
			// Preserve CountTextToken's empty-segment behavior even with padding.
			if text != "" {
				quantity += float64(estimateTokenWithMultipliers(text, weights))
			}
		}
	}
	count := common.QuotaRound(quantity)
	evidence.Quantity = float64(count)
	return count, evidence
}

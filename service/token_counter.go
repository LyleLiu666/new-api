package service

import (
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/logger"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	constant2 "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	hosttypes "github.com/QuantumNous/new-api/types"

	"github.com/gin-gonic/gin"
)

func getImageToken(c *gin.Context, fileMeta *types.FileMeta, model string, stream, getMedia, getMediaNotStream bool, observation *hosttypes.UsageEstimateComponent) (int, error) {
	if fileMeta == nil || fileMeta.Source == nil {
		return 0, fmt.Errorf("image_url_is_nil")
	}

	observation.Method = "image-tiles"
	observation.Parameters = make(map[string]float64)

	// Defaults for 4o/4.1/4.5 family unless overridden below
	baseTokens := 85
	tileTokens := 170

	// Model classification
	lowerModel := strings.ToLower(model)

	// Special cases from existing behavior
	if strings.HasPrefix(lowerModel, "glm-4") {
		observation.Method = "image-model-constant"
		observation.Parameters["constant_tokens"] = 1047
		return 1047, nil
	}

	// Patch-based models (32x32 patches, capped at 1536, with multiplier)
	isPatchBased := false
	multiplier := 1.0
	switch {
	case strings.Contains(lowerModel, "gpt-4.1-mini"):
		isPatchBased = true
		multiplier = 1.62
	case strings.Contains(lowerModel, "gpt-4.1-nano"):
		isPatchBased = true
		multiplier = 2.46
	case strings.HasPrefix(lowerModel, "o4-mini"):
		isPatchBased = true
		multiplier = 1.72
	case strings.HasPrefix(lowerModel, "gpt-5-mini"):
		isPatchBased = true
		multiplier = 1.62
	case strings.HasPrefix(lowerModel, "gpt-5-nano"):
		isPatchBased = true
		multiplier = 2.46
	}

	// Tile-based model tokens and bases per doc
	if !isPatchBased {
		if strings.HasPrefix(lowerModel, "gpt-4o-mini") {
			baseTokens = 2833
			tileTokens = 5667
		} else if strings.HasPrefix(lowerModel, "gpt-5-chat-latest") || (strings.HasPrefix(lowerModel, "gpt-5") && !strings.Contains(lowerModel, "mini") && !strings.Contains(lowerModel, "nano")) {
			baseTokens = 70
			tileTokens = 140
		} else if strings.HasPrefix(lowerModel, "o1") || strings.HasPrefix(lowerModel, "o3") || strings.HasPrefix(lowerModel, "o1-pro") {
			baseTokens = 75
			tileTokens = 150
		} else if strings.Contains(lowerModel, "computer-use-preview") {
			baseTokens = 65
			tileTokens = 129
		} else if strings.Contains(lowerModel, "4.1") || strings.Contains(lowerModel, "4o") || strings.Contains(lowerModel, "4.5") {
			baseTokens = 85
			tileTokens = 170
		}
	}

	observation.Parameters["base_tokens"] = float64(baseTokens)
	observation.Parameters["tile_tokens"] = float64(tileTokens)

	// Respect existing feature flags/short-circuits
	if fileMeta.Detail == "low" && !isPatchBased {
		observation.Method = "image-low-detail"
		return baseTokens, nil
	}

	// Whether to count image tokens at all
	if !getMedia {
		observation.Method = "image-metadata-disabled"
		observation.Parameters["base_multiplier"] = 3
		return 3 * baseTokens, nil
	}

	if !getMediaNotStream && !stream {
		observation.Method = "image-metadata-disabled"
		observation.Parameters["base_multiplier"] = 3
		return 3 * baseTokens, nil
	}
	// Normalize detail
	if fileMeta.Detail == "auto" || fileMeta.Detail == "" {
		fileMeta.Detail = "high"
	}

	// 使用统一的文件服务获取图片配置
	config, format, err := GetImageConfig(c, fileMeta.Source)
	if err != nil {
		return 0, err
	}
	if config.Width == 0 || config.Height == 0 {
		// not an image, but might be a valid file
		if format != "" {
			observation.Method = "image-nonimage-fallback"
			observation.Parameters["base_multiplier"] = 3
			// file type
			return 3 * baseTokens, nil
		}
		return 0, errors.New(fmt.Sprintf("fail to decode image config: %s", fileMeta.GetIdentifier()))
	}

	width := config.Width
	height := config.Height
	observation.Parameters["width"] = float64(width)
	observation.Parameters["height"] = float64(height)
	logger.LogDebug(c, "image token input: format=%s, width=%d, height=%d", format, width, height)

	if isPatchBased {
		observation.Method = "image-patches"
		observation.Parameters = map[string]float64{"width": float64(width), "height": float64(height), "patch_size": 32, "patch_cap": 1536, "multiplier": multiplier}
		// 32x32 patch-based calculation with 1536 cap and model multiplier
		ceilDiv := func(a, b int) int { return (a + b - 1) / b }
		rawPatchesW := ceilDiv(width, 32)
		rawPatchesH := ceilDiv(height, 32)
		rawPatches := rawPatchesW * rawPatchesH
		if rawPatches > 1536 {
			// scale down
			area := float64(width * height)
			r := math.Sqrt(float64(32*32*1536) / area)
			wScaled := float64(width) * r
			hScaled := float64(height) * r
			// adjust to fit whole number of patches after scaling
			adjW := math.Floor(wScaled/32.0) / (wScaled / 32.0)
			adjH := math.Floor(hScaled/32.0) / (hScaled / 32.0)
			adj := math.Min(adjW, adjH)
			if !math.IsNaN(adj) && adj > 0 {
				r = r * adj
			}
			wScaled = float64(width) * r
			hScaled = float64(height) * r
			patchesW := math.Ceil(wScaled / 32.0)
			patchesH := math.Ceil(hScaled / 32.0)
			imageTokens := min(int(patchesW*patchesH), 1536)
			observation.Parameters["patches"] = float64(imageTokens)
			return common.QuotaRound(float64(imageTokens) * multiplier), nil
		}
		// below cap
		imageTokens := rawPatches
		observation.Parameters["patches"] = float64(imageTokens)
		return common.QuotaRound(float64(imageTokens) * multiplier), nil
	}

	// Tile-based calculation for 4o/4.1/4.5/o1/o3/etc.
	// Step 1: fit within 2048x2048 square
	maxSide := math.Max(float64(width), float64(height))
	fitScale := 1.0
	if maxSide > 2048 {
		fitScale = maxSide / 2048.0
	}
	fitW := int(math.Round(float64(width) / fitScale))
	fitH := int(math.Round(float64(height) / fitScale))

	// Step 2: scale so that shortest side is exactly 768
	minSide := math.Min(float64(fitW), float64(fitH))
	if minSide == 0 {
		return baseTokens, nil
	}
	shortScale := 768.0 / minSide
	finalW := int(math.Round(float64(fitW) * shortScale))
	finalH := int(math.Round(float64(fitH) * shortScale))

	// Count 512px tiles
	tilesW := (finalW + 512 - 1) / 512
	tilesH := (finalH + 512 - 1) / 512
	tiles := tilesW * tilesH

	logger.LogDebug(c, "image token scaled size: width=%d, height=%d, tiles=%d", finalW, finalH, tiles)

	observation.Parameters["tile_size"] = 512
	observation.Parameters["fit_side"] = 2048
	observation.Parameters["short_side"] = 768
	observation.Parameters["tiles"] = float64(min(tiles, common.MaxQuota))
	if tiles > common.MaxQuota {
		observation.Settings = map[string]bool{"tiles_saturated": true}
	}
	return common.QuotaRound(float64(tiles)*float64(tileTokens) + float64(baseTokens)), nil
}

func EstimateRequestToken(c *gin.Context, meta *types.TokenCountMeta, info *relaycommon.RelayInfo) (int, error) {
	// 是否统计token
	if !constant.CountToken {
		info.CreditPromptEstimation = &hosttypes.UsageEstimation{Version: "new-api-request-counter-v1", Model: common.GetContextKeyString(c, constant.ContextKeyOriginalModel), Method: "disabled", Settings: map[string]bool{"count_token": false}}
		return 0, nil
	}
	return CountRequestToken(c, meta, info)
}

// CountRequestToken counts request tokens regardless of the billing estimation
// switch. Utility endpoints such as Claude's messages/count_tokens must remain
// available even when operators disable request-token estimation for relays.
func CountRequestToken(c *gin.Context, meta *types.TokenCountMeta, info *relaycommon.RelayInfo) (int, error) {
	if meta == nil {
		return 0, errors.New("token count meta is nil")
	}

	model := common.GetContextKeyString(c, constant.ContextKeyOriginalModel)
	settings := map[string]bool{"count_token": true, "get_media_token": constant.GetMediaToken, "get_media_token_not_stream": constant.GetMediaTokenNotStream, "stream": info.IsStream}
	if info.RelayFormat == types.RelayFormatOpenAIRealtime {
		info.CreditPromptEstimation = &hosttypes.UsageEstimation{Version: "new-api-request-counter-v1", Model: model, Method: "realtime-deferred", Settings: settings}
		return 0, nil
	}
	if info.RelayMode == constant2.RelayModeAudioTranscription || info.RelayMode == constant2.RelayModeAudioTranslation {
		multiForm, err := common.ParseMultipartFormReusable(c)
		if err != nil {
			return 0, fmt.Errorf("error parsing multipart form: %v", err)
		}
		fileHeaders := multiForm.File["file"]
		totalAudioToken := 0
		var components []hosttypes.UsageEstimateComponent
		for index, fileHeader := range fileHeaders {
			file, err := fileHeader.Open()
			if err != nil {
				return 0, fmt.Errorf("error opening audio file: %v", err)
			}
			defer file.Close()
			// get ext and io.seeker
			ext := filepath.Ext(fileHeader.Filename)
			duration, err := common.GetAudioDuration(c.Request.Context(), file, ext)
			if err != nil {
				return 0, fmt.Errorf("error getting audio duration: %v", err)
			}
			// duration 来自用户上传文件的元数据，可被伪造成天文数字或负数。
			// 负值会让 token 估算变成负数（低估预扣费），先钳到 0 再转换。
			if duration < 0 {
				duration = 0
			}
			// 一分钟 1000 token，与 $price / minute 对齐。
			token := common.QuotaRound(math.Ceil(duration) / 60.0 * 1000)
			totalAudioToken = common.QuotaFromFloat(float64(totalAudioToken) + float64(token))
			if index < 64 {
				component := hosttypes.UsageEstimateComponent{Index: index, Kind: "audio", Method: "audio-duration", Quantity: float64(token), Parameters: map[string]float64{"duration_seconds": min(duration, float64(common.MaxQuota)), "tokens_per_minute": 1000}}
				if duration > float64(common.MaxQuota) {
					component.Settings = map[string]bool{"duration_saturated": true}
				}
				components = append(components, component)
			}
		}
		info.CreditPromptEstimation = &hosttypes.UsageEstimation{Version: "new-api-request-counter-v1", Model: model, Method: "audio-duration", Settings: settings, Quantity: float64(totalAudioToken), Parameters: map[string]float64{"tokens_per_minute": 1000, "files": float64(len(fileHeaders))}, Components: components, OmittedComponents: max(0, len(fileHeaders)-len(components))}
		return totalAudioToken, nil
	}

	tkm := 0
	var estimation *hosttypes.UsageEstimation
	if meta.TokenType == types.TokenTypeTextNumber {
		tkm += utf8.RuneCountInString(meta.CombineText)
		estimation = &hosttypes.UsageEstimation{Version: "new-api-request-counter-v1", Model: model, Method: "unicode-runes", Parameters: map[string]float64{}}
	} else {
		tkm, estimation = CountTextTokensWithEstimation([]string{meta.CombineText}, model)
	}
	estimation.Version, estimation.Settings = "new-api-request-counter-v1", settings
	textTokens := tkm

	if info.RelayFormat == types.RelayFormatOpenAI {
		tkm += meta.ToolsCount * 8
		tkm += meta.MessagesCount * 3 // 每条消息的格式化token数量
		tkm += meta.NameCount * 3
		tkm += 3
		estimation.Parameters["tools"] = float64(meta.ToolsCount)
		estimation.Parameters["tool_overhead"] = 8
		estimation.Parameters["messages"] = float64(meta.MessagesCount)
		estimation.Parameters["message_overhead"] = 3
		estimation.Parameters["names"] = float64(meta.NameCount)
		estimation.Parameters["name_overhead"] = 3
		estimation.Parameters["reply_overhead"] = 3
	}
	formattingTokens := tkm - textTokens

	shouldFetchFiles := true

	if info.RelayFormat == types.RelayFormatGemini {
		shouldFetchFiles = false
	}

	// 是否本地计算媒体token数量
	if !settings["get_media_token"] {
		shouldFetchFiles = false
	}

	// 是否在非流模式下本地计算媒体token数量
	if !settings["get_media_token_not_stream"] && !info.IsStream {
		shouldFetchFiles = false
	}

	// 使用统一的文件服务获取文件类型
	for _, file := range meta.Files {
		if file.Source == nil {
			continue
		}

		// 如果文件类型未知且需要获取，通过 MIME 类型检测
		if file.FileType == "" || (file.Source.IsURL() && shouldFetchFiles) {
			// 注意：这里我们直接调用 LoadFileSource 而不是 GetMimeType
			// 因为 GetMimeType 内部可能会调用 GetFileTypeFromUrl (HEAD 请求)
			// 而我们这里既然要计算 token，通常需要完整数据
			cachedData, err := LoadFileSource(c, file.Source, "token_counter")
			if err != nil {
				if shouldFetchFiles {
					return 0, fmt.Errorf("error getting file type: %v", err)
				}
				continue
			}
			file.FileType = DetectFileType(cachedData.MimeType)
		}
	}

	for i, file := range meta.Files {
		component := hosttypes.UsageEstimateComponent{Index: i, Kind: string(file.FileType), Method: "media-constant", Parameters: map[string]float64{}}
		token := 0
		switch file.FileType {
		case types.FileTypeImage:
			if common.IsOpenAITextModel(model) {
				var err error
				token, err = getImageToken(c, file, model, info.IsStream, settings["get_media_token"], settings["get_media_token_not_stream"], &component)
				if err != nil {
					return 0, fmt.Errorf("error counting image token, media index[%d], identifier[%s], err: %v", i, file.GetIdentifier(), err)
				}
			} else {
				token = 520
			}
		case types.FileTypeAudio:
			token = 256
		case types.FileTypeVideo:
			token = 4096 * 2
		case types.FileTypeFile:
			token = 4096
		default:
			component.Kind = "unknown"
			token = 4096
		}
		component.Quantity = float64(token)
		if component.Method == "media-constant" {
			component.Parameters["constant_tokens"] = float64(token)
		}
		if i < 64 {
			estimation.Components = append(estimation.Components, component)
		} else {
			estimation.OmittedComponents++
		}
		tkm = common.QuotaFromFloat(float64(tkm) + float64(token))
	}

	estimation.Parameters["media_tokens"] = float64(max(0, tkm-textTokens-formattingTokens))
	estimation.Parameters["text_tokens"] = float64(textTokens)
	estimation.Quantity = float64(tkm)
	info.CreditPromptEstimation = estimation
	common.SetContextKey(c, constant.ContextKeyPromptTokens, tkm)
	return tkm, nil
}

func CountTokenRealtime(info *relaycommon.RelayInfo, request dto.RealtimeEvent, model string) (int, int, error) {
	text, audio, _, err := CountTokenRealtimeWithEstimation(info, request, model)
	return text, audio, err
}

// Preserve native per-segment rounding and capture the configuration alongside
// each quantity. A connection aggregate is not one tokenizer invocation.
func CountTokenRealtimeWithEstimation(info *relaycommon.RelayInfo, request dto.RealtimeEvent, model string) (int, int, []hosttypes.UsageFact, error) {
	audioToken := 0
	textToken := 0
	var textEstimation, audioEstimation *hosttypes.UsageEstimation
	switch request.Type {
	case dto.RealtimeEventTypeSessionUpdate:
		if request.Session != nil {
			msgTokens, estimation := CountTextTokensWithEstimation([]string{request.Session.Instructions}, model)
			textEstimation = estimation
			textToken += msgTokens
		}
	case dto.RealtimeEventResponseAudioDelta:
		// count audio token
		atk, estimation, err := countRealtimeAudioTokens(request.Delta, info.OutputAudioFormat, model, true)
		if err != nil {
			return 0, 0, nil, fmt.Errorf("error counting audio token: %v", err)
		}
		audioEstimation = estimation
		audioToken += atk
	case dto.RealtimeEventResponseAudioTranscriptionDelta, dto.RealtimeEventResponseFunctionCallArgumentsDelta:
		// count text token
		tkm, estimation := CountTextTokensWithEstimation([]string{request.Delta}, model)
		textEstimation = estimation
		textToken += tkm
	case dto.RealtimeEventInputAudioBufferAppend:
		// count audio token
		atk, estimation, err := countRealtimeAudioTokens(request.Audio, info.InputAudioFormat, model, false)
		if err != nil {
			return 0, 0, nil, fmt.Errorf("error counting audio token: %v", err)
		}
		audioEstimation = estimation
		audioToken += atk
	case dto.RealtimeEventConversationItemCreated:
		if request.Item != nil {
			switch request.Item.Type {
			case "message":
				var texts []string
				for _, content := range request.Item.Content {
					if content.Type == "input_text" {
						texts = append(texts, content.Text)
					}
				}
				if len(texts) > 0 {
					textToken, textEstimation = CountTextTokensWithEstimation(texts, model)
				}
			}
		}
	case dto.RealtimeEventTypeResponseDone:
		// count tools token
		if !info.IsFirstRequest {
			if info.RealtimeTools != nil && len(info.RealtimeTools) > 0 {
				for _, tool := range info.RealtimeTools {
					toolTokens := CountTokenInput(tool, model)
					textToken += 8
					textToken += toolTokens
				}
			}
		}
	}
	var facts []hosttypes.UsageFact
	for _, field := range []struct {
		name       string
		quantity   int
		estimation *hosttypes.UsageEstimation
	}{{"text_tokens", textToken, textEstimation}, {"audio_tokens", audioToken, audioEstimation}} {
		if field.estimation == nil {
			continue
		}
		quantity := float64(field.quantity)
		facts = append(facts, hosttypes.UsageFact{Field: field.name, Unit: "token", Quantity: &quantity, Source: "estimate", Algorithm: "new-api-realtime-event-counter-v1", Estimation: field.estimation})
	}
	return textToken, audioToken, facts, nil
}

func CountTokenInput(input any, model string) int {
	switch v := input.(type) {
	case string:
		return CountTextToken(v, model)
	case []string:
		var text strings.Builder
		for _, s := range v {
			text.WriteString(s)
		}
		return CountTextToken(text.String(), model)
	case []any:
		var text strings.Builder
		for _, item := range v {
			text.WriteString(fmt.Sprintf("%v", item))
		}
		return CountTextToken(text.String(), model)
	}
	return CountTokenInput(fmt.Sprintf("%v", input), model)
}

func CountAudioTokenInput(audioBase64 string, audioFormat string) (int, error) {
	count, _, err := countRealtimeAudioTokens(audioBase64, audioFormat, "", false)
	return count, err
}

func CountAudioTokenOutput(audioBase64 string, audioFormat string) (int, error) {
	count, _, err := countRealtimeAudioTokens(audioBase64, audioFormat, "", true)
	return count, err
}

func countRealtimeAudioTokens(audioBase64, audioFormat, model string, output bool) (int, *hosttypes.UsageEstimation, error) {
	if audioBase64 == "" {
		return 0, nil, nil
	}
	duration, parameters, err := parseAudioObservation(audioBase64, audioFormat)
	if err != nil {
		return 0, nil, err
	}
	// Keep the native order of operations and truncation, including saturation.
	var count int
	if output {
		count = common.QuotaFromFloat(duration / 60 * 200 / 0.24)
		parameters["token_multiplier"], parameters["token_divisor"] = 200, 0.24
	} else {
		count = common.QuotaFromFloat(duration / 60 * 100 / 0.06)
		parameters["token_multiplier"], parameters["token_divisor"] = 100, 0.06
	}
	estimation := &hosttypes.UsageEstimation{Version: "new-api-realtime-audio-counter-v1", Model: model, Method: "realtime-audio-seconds", Quantity: float64(count), Parameters: parameters}
	if audioFormat != "pcm16" && audioFormat != "g711_ulaw" && audioFormat != "g711_alaw" {
		estimation.Settings = map[string]bool{"unknown_format_native_fallback": true}
	}
	return count, estimation, nil
}

// CountTextToken 统计文本的token数量，仅OpenAI模型使用tokenizer，其余模型使用估算
func CountTextToken(text string, model string) int {
	if text == "" {
		return 0
	}
	if common.IsOpenAITextModel(model) {
		tokenEncoder := getTokenEncoder(model)
		return getTokenNum(tokenEncoder, text)
	} else {
		// 非openai模型，使用tiktoken-go计算没有意义，使用估算节省资源
		return EstimateTokenByModel(model, text)
	}
}

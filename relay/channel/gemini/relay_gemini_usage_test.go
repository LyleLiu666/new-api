package gemini

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCreditGeminiUsagePreservesZeroAndNativeCategories(t *testing.T) {
	for _, transport := range []string{"native", "chat", "responses"} {
		for _, tc := range []struct {
			name, metadata, source string
			prompt, output, audio  int
		}{
			{"reported_zero", `"promptTokenCount":0,"toolUsePromptTokenCount":0,"candidatesTokenCount":0,"thoughtsTokenCount":0,"totalTokenCount":0,"cachedContentTokenCount":0`, "adaptor", 0, 0, 0},
			{"partial_cache", `"promptTokenCount":12,"cachedContentTokenCount":8`, "estimate", 12, service.CountTextToken("hello", "gemini-test"), 0},
			{"native_categories", `"promptTokenCount":12,"toolUsePromptTokenCount":4,"candidatesTokenCount":3,"thoughtsTokenCount":2,"totalTokenCount":21,"cachedContentTokenCount":8,"promptTokensDetails":[{"modality":"AUDIO","tokenCount":1},{"modality":"IMAGE","tokenCount":4}],"toolUsePromptTokensDetails":[{"modality":"AUDIO","tokenCount":2}],"candidatesTokensDetails":[{"modality":"AUDIO","tokenCount":1}]`, "adaptor", 16, 5, 3},
		} {
			t.Run(transport+"/"+tc.name, func(t *testing.T) {
				ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
				ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
				format := types.RelayFormat(types.RelayFormatGemini)
				if transport == "chat" {
					format = types.RelayFormatOpenAI
				} else if transport == "responses" {
					format = types.RelayFormatOpenAIResponses
				}
				info := &relaycommon.RelayInfo{RelayFormat: format, BillingSource: service.BillingSourceCreditPacks, ChannelMeta: &relaycommon.ChannelMeta{UpstreamModelName: "gemini-test"}}
				info.SetEstimatePromptTokens(100)
				body := `{"candidates":[{"content":{"role":"model","parts":[{"text":"hello"}]},"finishReason":"STOP"}],"usageMetadata":{` + tc.metadata + `}}`
				resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}
				var usage *dto.Usage
				var apiErr *types.NewAPIError
				switch transport {
				case "native":
					usage, apiErr = GeminiTextGenerationHandler(ctx, info, resp)
				case "chat":
					usage, apiErr = GeminiChatHandler(ctx, info, resp)
				case "responses":
					usage, apiErr = GeminiResponsesHandler(ctx, info, resp)
				}
				require.Nil(t, apiErr)
				require.NotNil(t, usage)
				assert.Equal(t, tc.prompt, usage.PromptTokens)
				assert.Equal(t, tc.output, usage.CompletionTokens)
				assert.Equal(t, tc.audio, usage.PromptTokensDetails.AudioTokens)
				require.Contains(t, info.CreditUsageFacts, "completion_tokens")
				assert.Equal(t, tc.source, info.CreditUsageFacts["completion_tokens"].Source)
				require.Contains(t, info.CreditUsageFacts, "gemini_prompt_tokens")
				assert.Equal(t, "upstream", info.CreditUsageFacts["gemini_prompt_tokens"].Source)
				if tc.name != "reported_zero" {
					assert.Equal(t, 8, usage.PromptTokensDetails.CachedTokens)
					canonical, ok := usage.BillingUsage.CanonicalUsage()
					require.True(t, ok)
					assert.Equal(t, tc.prompt, canonical.PromptTokens)
					assert.Equal(t, tc.output, canonical.CompletionTokens)
					assert.Equal(t, tc.audio, canonical.PromptTokensDetails.AudioTokens)
				}
			})
		}
	}
}

func TestCreditGeminiStreamEarlyZeroIsNotFinalUsage(t *testing.T) {
	previousTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 30
	t.Cleanup(func() { constant.StreamingTimeout = previousTimeout })
	for _, transport := range []string{"native", "chat", "responses"} {
		for _, tc := range []struct {
			name, tail, source string
			output             int
		}{
			{"interrupted", "", "estimate", service.CountTextToken("hello", "gemini-test")},
			{"empty_terminal_usage", "data: {\"candidates\":[{\"content\":{\"parts\":[]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{}}\n\n", "estimate", service.CountTextToken("hello", "gemini-test")},
			{"reported_final_zero", "data: {\"candidates\":[{\"content\":{\"parts\":[]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"candidatesTokenCount\":0,\"thoughtsTokenCount\":0}}\n\n", "adaptor", 0},
		} {
			t.Run(transport+"/"+tc.name, func(t *testing.T) {
				ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
				ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
				format := types.RelayFormat(types.RelayFormatGemini)
				if transport == "chat" {
					format = types.RelayFormatOpenAI
				} else if transport == "responses" {
					format = types.RelayFormatOpenAIResponses
				}
				info := &relaycommon.RelayInfo{RelayFormat: format, IsStream: true, StreamStatus: relaycommon.NewStreamStatus(), BillingSource: service.BillingSourceCreditPacks, ChannelMeta: &relaycommon.ChannelMeta{UpstreamModelName: "gemini-test"}}
				stream := "data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"hello\"}]}}],\"usageMetadata\":{\"promptTokenCount\":12,\"toolUsePromptTokenCount\":0,\"candidatesTokenCount\":0,\"thoughtsTokenCount\":0,\"cachedContentTokenCount\":8}}\n\n" + tc.tail
				resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(stream))}
				var usage *dto.Usage
				var apiErr *types.NewAPIError
				switch transport {
				case "native":
					usage, apiErr = GeminiTextGenerationStreamHandler(ctx, info, resp)
				case "chat":
					usage, apiErr = GeminiChatStreamHandler(ctx, info, resp)
				case "responses":
					usage, apiErr = GeminiResponsesStreamHandler(ctx, info, resp)
				}
				require.Nil(t, apiErr)
				require.NotNil(t, usage)
				assert.Equal(t, 12, usage.PromptTokens)
				assert.Equal(t, 8, usage.PromptTokensDetails.CachedTokens)
				assert.Equal(t, tc.output, usage.CompletionTokens)
				require.Contains(t, info.CreditUsageFacts, "completion_tokens")
				assert.Equal(t, tc.source, info.CreditUsageFacts["completion_tokens"].Source)
				canonical, ok := usage.BillingUsage.CanonicalUsage()
				require.True(t, ok)
				assert.Equal(t, 12, canonical.PromptTokens)
				assert.Equal(t, 8, canonical.PromptTokensDetails.CachedTokens)
				assert.Equal(t, tc.output, canonical.CompletionTokens)
			})
		}
	}
}

func TestStreamResponseGeminiChat2OpenAIAttachesUsageMetadata(t *testing.T) {
	t.Parallel()

	withUsage, isStop := streamResponseGeminiChat2OpenAI(&dto.GeminiChatResponse{
		Candidates: []dto.GeminiChatCandidate{{
			Content: dto.GeminiChatContent{
				Role:  "model",
				Parts: []dto.GeminiPart{{Text: "hello"}},
			},
		}},
		UsageMetadata: dto.GeminiUsageMetadata{
			PromptTokenCount:     3868,
			CandidatesTokenCount: 0,
			TotalTokenCount:      3868,
		},
	})
	require.False(t, isStop)
	require.NotNil(t, withUsage)
	require.NotNil(t, withUsage.Usage)
	require.Equal(t, 3868, withUsage.Usage.PromptTokens)
	require.Equal(t, 3868, withUsage.Usage.TotalTokens)
	require.NotNil(t, withUsage.Usage.BillingUsage)
	require.Equal(t, dto.BillingUsageSourceGeminiChat, withUsage.Usage.BillingUsage.Source)
	require.Equal(t, dto.BillingUsageSemanticGemini, withUsage.Usage.BillingUsage.Semantic)
	require.NotNil(t, withUsage.Usage.BillingUsage.GeminiUsageMetadata)
	require.Equal(t, 3868, withUsage.Usage.BillingUsage.GeminiUsageMetadata.PromptTokenCount)
	require.False(t, withUsage.Usage.BillingUsage.Estimated)

	withoutUsage, _ := streamResponseGeminiChat2OpenAI(&dto.GeminiChatResponse{
		Candidates: []dto.GeminiChatCandidate{{
			Content: dto.GeminiChatContent{
				Role:  "model",
				Parts: []dto.GeminiPart{{Text: "hello"}},
			},
		}},
	})
	require.NotNil(t, withoutUsage)
	require.Nil(t, withoutUsage.Usage)
}

func TestGeminiChatStreamHandlerClaudeFirstFrameUsesUpstreamUsage(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	oldStreamingTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 300
	t.Cleanup(func() {
		constant.StreamingTimeout = oldStreamingTimeout
	})

	info := &relaycommon.RelayInfo{
		RelayFormat:     types.RelayFormatClaude,
		OriginModelName: "gemini-2.5-flash",
		ChannelMeta: &relaycommon.ChannelMeta{
			UpstreamModelName: "gemini-2.5-flash",
		},
		ClaudeConvertInfo: &relaycommon.ClaudeConvertInfo{
			LastMessagesType: relaycommon.LastMessageTypeNone,
		},
	}
	info.SetEstimatePromptTokens(4994)

	chunkData, err := common.Marshal(dto.GeminiChatResponse{
		Candidates: []dto.GeminiChatCandidate{{
			Content: dto.GeminiChatContent{
				Role:  "model",
				Parts: []dto.GeminiPart{{Text: "hello"}},
			},
		}},
		UsageMetadata: dto.GeminiUsageMetadata{
			PromptTokenCount: 3868,
			TotalTokenCount:  3868,
		},
	})
	require.NoError(t, err)
	resp := &http.Response{
		Body: io.NopCloser(bytes.NewReader([]byte("data: " + string(chunkData) + "\n" + "data: [DONE]\n"))),
	}

	usage, newAPIError := GeminiChatStreamHandler(c, info, resp)
	require.Nil(t, newAPIError)
	require.NotNil(t, usage)
	require.Equal(t, 3868, usage.PromptTokens)

	var startUsage, deltaUsage *dto.ClaudeUsage
	for line := range strings.SplitSeq(recorder.Body.String(), "\n") {
		payload, ok := strings.CutPrefix(strings.TrimSpace(line), "data: ")
		if !ok {
			continue
		}
		var event dto.ClaudeResponse
		if err := common.UnmarshalJsonStr(payload, &event); err != nil {
			continue
		}
		switch event.Type {
		case "message_start":
			if event.Message != nil {
				startUsage = event.Message.Usage
			}
		case "message_delta":
			deltaUsage = event.Usage
		}
	}

	require.NotNil(t, startUsage)
	require.Equal(t, 3868, startUsage.InputTokens)
	require.NotNil(t, startUsage.BillingUsage)
	require.Equal(t, dto.BillingUsageSourceGeminiChat, startUsage.BillingUsage.Source)
	require.Equal(t, dto.BillingUsageSemanticGemini, startUsage.BillingUsage.Semantic)
	require.NotNil(t, startUsage.BillingUsage.GeminiUsageMetadata)
	require.Equal(t, 3868, startUsage.BillingUsage.GeminiUsageMetadata.PromptTokenCount)
	require.False(t, startUsage.BillingUsage.Estimated)

	require.NotNil(t, deltaUsage)
	require.Equal(t, 3868, deltaUsage.InputTokens)
	require.NotNil(t, deltaUsage.BillingUsage)
	require.Equal(t, dto.BillingUsageSourceGeminiChat, deltaUsage.BillingUsage.Source)
	require.Equal(t, dto.BillingUsageSemanticGemini, deltaUsage.BillingUsage.Semantic)
	require.NotNil(t, deltaUsage.BillingUsage.GeminiUsageMetadata)
	require.Equal(t, 3868, deltaUsage.BillingUsage.GeminiUsageMetadata.PromptTokenCount)
}

func TestGeminiChatHandlerCompletionTokensExcludeToolUsePromptTokens(t *testing.T) {
	t.Parallel()
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	info := &relaycommon.RelayInfo{
		RelayFormat:     types.RelayFormatGemini,
		OriginModelName: "gemini-3-flash-preview",
		ChannelMeta: &relaycommon.ChannelMeta{
			UpstreamModelName: "gemini-3-flash-preview",
		},
	}

	payload := dto.GeminiChatResponse{
		Candidates: []dto.GeminiChatCandidate{
			{
				Content: dto.GeminiChatContent{
					Role: "model",
					Parts: []dto.GeminiPart{
						{Text: "ok"},
					},
				},
			},
		},
		UsageMetadata: dto.GeminiUsageMetadata{
			PromptTokenCount:        151,
			ToolUsePromptTokenCount: 18329,
			CandidatesTokenCount:    1089,
			ThoughtsTokenCount:      1120,
			TotalTokenCount:         20689,
		},
	}

	body, err := common.Marshal(payload)
	require.NoError(t, err)

	resp := &http.Response{
		Body: io.NopCloser(bytes.NewReader(body)),
	}

	usage, newAPIError := GeminiChatHandler(c, info, resp)
	require.Nil(t, newAPIError)
	require.NotNil(t, usage)
	require.Equal(t, 18480, usage.PromptTokens)
	require.Equal(t, 2209, usage.CompletionTokens)
	require.Equal(t, 20689, usage.TotalTokens)
	require.Equal(t, 1120, usage.CompletionTokenDetails.ReasoningTokens)
}

func TestGeminiStreamHandlerCompletionTokensExcludeToolUsePromptTokens(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	oldStreamingTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 300
	t.Cleanup(func() {
		constant.StreamingTimeout = oldStreamingTimeout
	})

	info := &relaycommon.RelayInfo{
		OriginModelName: "gemini-3-flash-preview",
		ChannelMeta: &relaycommon.ChannelMeta{
			UpstreamModelName: "gemini-3-flash-preview",
		},
	}

	chunk := dto.GeminiChatResponse{
		Candidates: []dto.GeminiChatCandidate{
			{
				Content: dto.GeminiChatContent{
					Role: "model",
					Parts: []dto.GeminiPart{
						{Text: "partial"},
					},
				},
			},
		},
		UsageMetadata: dto.GeminiUsageMetadata{
			PromptTokenCount:        151,
			ToolUsePromptTokenCount: 18329,
			CandidatesTokenCount:    1089,
			ThoughtsTokenCount:      1120,
			TotalTokenCount:         20689,
		},
	}

	chunkData, err := common.Marshal(chunk)
	require.NoError(t, err)

	streamBody := []byte("data: " + string(chunkData) + "\n" + "data: [DONE]\n")
	resp := &http.Response{
		Body: io.NopCloser(bytes.NewReader(streamBody)),
	}

	usage, newAPIError := geminiStreamHandler(c, info, resp, func(_ string, _ *dto.GeminiChatResponse) bool {
		return true
	})
	require.Nil(t, newAPIError)
	require.NotNil(t, usage)
	require.Equal(t, 18480, usage.PromptTokens)
	require.Equal(t, 2209, usage.CompletionTokens)
	require.Equal(t, 20689, usage.TotalTokens)
	require.Equal(t, 1120, usage.CompletionTokenDetails.ReasoningTokens)
}

func TestGeminiTextGenerationHandlerPromptTokensIncludeToolUsePromptTokens(t *testing.T) {
	t.Parallel()
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-3-flash-preview:generateContent", nil)

	info := &relaycommon.RelayInfo{
		OriginModelName: "gemini-3-flash-preview",
		ChannelMeta: &relaycommon.ChannelMeta{
			UpstreamModelName: "gemini-3-flash-preview",
		},
	}

	payload := dto.GeminiChatResponse{
		Candidates: []dto.GeminiChatCandidate{
			{
				Content: dto.GeminiChatContent{
					Role: "model",
					Parts: []dto.GeminiPart{
						{Text: "ok"},
					},
				},
			},
		},
		UsageMetadata: dto.GeminiUsageMetadata{
			PromptTokenCount:        151,
			ToolUsePromptTokenCount: 18329,
			CandidatesTokenCount:    1089,
			ThoughtsTokenCount:      1120,
			TotalTokenCount:         20689,
		},
	}

	body, err := common.Marshal(payload)
	require.NoError(t, err)

	resp := &http.Response{
		Body: io.NopCloser(bytes.NewReader(body)),
	}

	usage, newAPIError := GeminiTextGenerationHandler(c, info, resp)
	require.Nil(t, newAPIError)
	require.NotNil(t, usage)
	require.Equal(t, 18480, usage.PromptTokens)
	require.Equal(t, 2209, usage.CompletionTokens)
	require.Equal(t, 20689, usage.TotalTokens)
	require.Equal(t, 1120, usage.CompletionTokenDetails.ReasoningTokens)
}

func TestGeminiChatHandlerUsesEstimatedPromptTokensWhenUsagePromptMissing(t *testing.T) {
	t.Parallel()
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	info := &relaycommon.RelayInfo{
		RelayFormat:     types.RelayFormatGemini,
		OriginModelName: "gemini-3-flash-preview",
		ChannelMeta: &relaycommon.ChannelMeta{
			UpstreamModelName: "gemini-3-flash-preview",
		},
	}
	info.SetEstimatePromptTokens(20)

	payload := dto.GeminiChatResponse{
		Candidates: []dto.GeminiChatCandidate{
			{
				Content: dto.GeminiChatContent{
					Role: "model",
					Parts: []dto.GeminiPart{
						{Text: "ok"},
					},
				},
			},
		},
		UsageMetadata: dto.GeminiUsageMetadata{
			PromptTokenCount:        0,
			ToolUsePromptTokenCount: 0,
			CandidatesTokenCount:    90,
			ThoughtsTokenCount:      10,
			TotalTokenCount:         110,
		},
	}

	body, err := common.Marshal(payload)
	require.NoError(t, err)

	resp := &http.Response{
		Body: io.NopCloser(bytes.NewReader(body)),
	}

	usage, newAPIError := GeminiChatHandler(c, info, resp)
	require.Nil(t, newAPIError)
	require.NotNil(t, usage)
	require.Equal(t, 20, usage.PromptTokens)
	require.Equal(t, 100, usage.CompletionTokens)
	require.Equal(t, 110, usage.TotalTokens)
}

func TestGeminiStreamHandlerUsesEstimatedPromptTokensWhenUsagePromptMissing(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	oldStreamingTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 300
	t.Cleanup(func() {
		constant.StreamingTimeout = oldStreamingTimeout
	})

	info := &relaycommon.RelayInfo{
		OriginModelName: "gemini-3-flash-preview",
		ChannelMeta: &relaycommon.ChannelMeta{
			UpstreamModelName: "gemini-3-flash-preview",
		},
	}
	info.SetEstimatePromptTokens(20)

	chunk := dto.GeminiChatResponse{
		Candidates: []dto.GeminiChatCandidate{
			{
				Content: dto.GeminiChatContent{
					Role: "model",
					Parts: []dto.GeminiPart{
						{Text: "partial"},
					},
				},
			},
		},
		UsageMetadata: dto.GeminiUsageMetadata{
			PromptTokenCount:        0,
			ToolUsePromptTokenCount: 0,
			CandidatesTokenCount:    90,
			ThoughtsTokenCount:      10,
			TotalTokenCount:         110,
		},
	}

	chunkData, err := common.Marshal(chunk)
	require.NoError(t, err)

	streamBody := []byte("data: " + string(chunkData) + "\n" + "data: [DONE]\n")
	resp := &http.Response{
		Body: io.NopCloser(bytes.NewReader(streamBody)),
	}

	usage, newAPIError := geminiStreamHandler(c, info, resp, func(_ string, _ *dto.GeminiChatResponse) bool {
		return true
	})
	require.Nil(t, newAPIError)
	require.NotNil(t, usage)
	require.Equal(t, 20, usage.PromptTokens)
	require.Equal(t, 100, usage.CompletionTokens)
	require.Equal(t, 110, usage.TotalTokens)
}

func TestGeminiTextGenerationHandlerUsesEstimatedPromptTokensWhenUsagePromptMissing(t *testing.T) {
	t.Parallel()
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-3-flash-preview:generateContent", nil)

	info := &relaycommon.RelayInfo{
		OriginModelName: "gemini-3-flash-preview",
		ChannelMeta: &relaycommon.ChannelMeta{
			UpstreamModelName: "gemini-3-flash-preview",
		},
	}
	info.SetEstimatePromptTokens(20)

	payload := dto.GeminiChatResponse{
		Candidates: []dto.GeminiChatCandidate{
			{
				Content: dto.GeminiChatContent{
					Role: "model",
					Parts: []dto.GeminiPart{
						{Text: "ok"},
					},
				},
			},
		},
		UsageMetadata: dto.GeminiUsageMetadata{
			PromptTokenCount:        0,
			ToolUsePromptTokenCount: 0,
			CandidatesTokenCount:    90,
			ThoughtsTokenCount:      10,
			TotalTokenCount:         110,
		},
	}

	body, err := common.Marshal(payload)
	require.NoError(t, err)

	resp := &http.Response{
		Body: io.NopCloser(bytes.NewReader(body)),
	}

	usage, newAPIError := GeminiTextGenerationHandler(c, info, resp)
	require.Nil(t, newAPIError)
	require.NotNil(t, usage)
	require.Equal(t, 20, usage.PromptTokens)
	require.Equal(t, 100, usage.CompletionTokens)
	require.Equal(t, 110, usage.TotalTokens)
}

func TestGeminiChatHandlerMissingUsageMetadataBuildsEstimatedBillingUsage(t *testing.T) {
	t.Parallel()
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	info := &relaycommon.RelayInfo{
		RelayFormat:     types.RelayFormatGemini,
		OriginModelName: "gemini-3-flash-preview",
		ChannelMeta: &relaycommon.ChannelMeta{
			UpstreamModelName: "gemini-3-flash-preview",
		},
	}
	info.SetEstimatePromptTokens(20)

	body := []byte(`{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]}}]}`)
	resp := &http.Response{
		Body: io.NopCloser(bytes.NewReader(body)),
	}

	usage, newAPIError := GeminiChatHandler(c, info, resp)
	require.Nil(t, newAPIError)
	require.NotNil(t, usage)
	require.Equal(t, 20, usage.PromptTokens)
	require.NotNil(t, usage.BillingUsage)
	require.True(t, usage.BillingUsage.Estimated)
	require.Equal(t, dto.BillingUsageSourceGeminiChat, usage.BillingUsage.Source)
	require.Equal(t, dto.BillingUsageSemanticGemini, usage.BillingUsage.Semantic)
	require.NotNil(t, usage.BillingUsage.GeminiUsageMetadata)
	require.Equal(t, usage.PromptTokens, usage.BillingUsage.GeminiUsageMetadata.PromptTokenCount)
	require.Equal(t, usage.CompletionTokens, usage.BillingUsage.GeminiUsageMetadata.CandidatesTokenCount)
	require.True(t, common.GetContextKeyBool(c, constant.ContextKeyLocalCountTokens))
}

func TestGeminiStreamHandlerPromptOnlyUsageMetadataEstimatesCompletionTokens(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	oldStreamingTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 300
	t.Cleanup(func() {
		constant.StreamingTimeout = oldStreamingTimeout
	})

	info := &relaycommon.RelayInfo{
		OriginModelName: "gemini-3-flash-preview",
		ChannelMeta: &relaycommon.ChannelMeta{
			UpstreamModelName: "gemini-3-flash-preview",
		},
	}
	info.SetEstimatePromptTokens(20)

	// Simulates a client aborting the stream before the final chunk: text was
	// streamed but the last observed usageMetadata only carries prompt tokens.
	chunk := dto.GeminiChatResponse{
		Candidates: []dto.GeminiChatCandidate{
			{
				Content: dto.GeminiChatContent{
					Role: "model",
					Parts: []dto.GeminiPart{
						{Text: "partial streamed answer before disconnect"},
					},
				},
			},
		},
		UsageMetadata: dto.GeminiUsageMetadata{
			PromptTokenCount: 151,
			TotalTokenCount:  151,
		},
	}

	chunkData, err := common.Marshal(chunk)
	require.NoError(t, err)

	streamBody := []byte("data: " + string(chunkData) + "\n" + "data: [DONE]\n")
	resp := &http.Response{
		Body: io.NopCloser(bytes.NewReader(streamBody)),
	}

	usage, newAPIError := geminiStreamHandler(c, info, resp, func(_ string, _ *dto.GeminiChatResponse) bool {
		return true
	})
	require.Nil(t, newAPIError)
	require.NotNil(t, usage)
	require.Equal(t, 151, usage.PromptTokens)
	require.Greater(t, usage.CompletionTokens, 0)
	require.Equal(t, usage.PromptTokens+usage.CompletionTokens, usage.TotalTokens)
	require.NotNil(t, usage.BillingUsage)
	require.True(t, usage.BillingUsage.Estimated)
	require.NotNil(t, usage.BillingUsage.GeminiUsageMetadata)
	require.Equal(t, usage.CompletionTokens, usage.BillingUsage.GeminiUsageMetadata.CandidatesTokenCount)
}

func TestGeminiChatHandlerPromptOnlyUsageMetadataEstimatesCompletionTokens(t *testing.T) {
	t.Parallel()
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	info := &relaycommon.RelayInfo{
		RelayFormat:     types.RelayFormatGemini,
		OriginModelName: "gemini-3-flash-preview",
		ChannelMeta: &relaycommon.ChannelMeta{
			UpstreamModelName: "gemini-3-flash-preview",
		},
	}

	payload := dto.GeminiChatResponse{
		Candidates: []dto.GeminiChatCandidate{
			{
				Content: dto.GeminiChatContent{
					Role: "model",
					Parts: []dto.GeminiPart{
						{Text: "answer text without candidate token count"},
					},
				},
			},
		},
		UsageMetadata: dto.GeminiUsageMetadata{
			PromptTokenCount: 151,
			TotalTokenCount:  151,
		},
	}

	body, err := common.Marshal(payload)
	require.NoError(t, err)

	resp := &http.Response{
		Body: io.NopCloser(bytes.NewReader(body)),
	}

	usage, newAPIError := GeminiChatHandler(c, info, resp)
	require.Nil(t, newAPIError)
	require.NotNil(t, usage)
	require.Equal(t, 151, usage.PromptTokens)
	require.Greater(t, usage.CompletionTokens, 0)
	require.Equal(t, usage.PromptTokens+usage.CompletionTokens, usage.TotalTokens)
	require.NotNil(t, usage.BillingUsage)
	require.True(t, usage.BillingUsage.Estimated)
}

func TestGeminiStreamHandlerEmptyUsageMetadataBuildsEstimatedBillingUsage(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	oldStreamingTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 300
	t.Cleanup(func() {
		constant.StreamingTimeout = oldStreamingTimeout
	})

	info := &relaycommon.RelayInfo{
		OriginModelName: "gemini-3-flash-preview",
		ChannelMeta: &relaycommon.ChannelMeta{
			UpstreamModelName: "gemini-3-flash-preview",
		},
	}
	info.SetEstimatePromptTokens(20)

	streamBody := []byte("data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"partial\"}]}}],\"usageMetadata\":{}}\n" + "data: [DONE]\n")
	resp := &http.Response{
		Body: io.NopCloser(bytes.NewReader(streamBody)),
	}

	usage, newAPIError := geminiStreamHandler(c, info, resp, func(_ string, _ *dto.GeminiChatResponse) bool {
		return true
	})
	require.Nil(t, newAPIError)
	require.NotNil(t, usage)
	require.Equal(t, 20, usage.PromptTokens)
	require.NotNil(t, usage.BillingUsage)
	require.True(t, usage.BillingUsage.Estimated)
	require.Equal(t, dto.BillingUsageSourceGeminiChat, usage.BillingUsage.Source)
	require.NotNil(t, usage.BillingUsage.GeminiUsageMetadata)
	require.Equal(t, usage.PromptTokens, usage.BillingUsage.GeminiUsageMetadata.PromptTokenCount)
	require.Equal(t, usage.CompletionTokens, usage.BillingUsage.GeminiUsageMetadata.CandidatesTokenCount)
	require.True(t, common.GetContextKeyBool(c, constant.ContextKeyLocalCountTokens))
}

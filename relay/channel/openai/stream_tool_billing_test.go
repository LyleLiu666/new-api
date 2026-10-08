package openai

import (
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/QuantumNous/new-api/constant"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestObserveStreamChoicesDedupesFunctionCallNames(t *testing.T) {
	info := &relaycommon.RelayInfo{StreamStatus: relaycommon.NewStreamStatus()}
	seen := make(map[string]struct{})
	var names []string

	chunks := []string{
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"get_weather","arguments":""}}]}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"q\":"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"x\"}"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"c2","type":"function","function":{"name":"get_time","arguments":""}}]}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"function":{"arguments":"{}"}}]}}]}`,
	}
	for _, chunk := range chunks {
		observeStreamChoices(info, chunk, seen, &names)
	}

	require.Len(t, names, 2)
	assert.Equal(t, []string{"get_weather", "get_time"}, names)
	assert.Empty(t, info.StreamStatus.ResponseOutcome(), "tool call deltas carry no finish reason")

	observeStreamChoices(info, `{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`, seen, &names)
	assert.Equal(t, "completed", info.StreamStatus.ResponseOutcome())
	assert.False(t, info.PerformanceBusinessRejection)

	filtered := &relaycommon.RelayInfo{StreamStatus: relaycommon.NewStreamStatus()}
	observeStreamChoices(filtered, `{"choices":[{"index":0,"delta":{},"finish_reason":"content_filter"}]}`, map[string]struct{}{}, &names)
	assert.True(t, filtered.PerformanceBusinessRejection)
}

func TestCreditOpenaiUsagePreservesReportedZeroAndPartialCache(t *testing.T) {
	for _, tc := range []struct {
		name, usage                string
		prompt, completion, cached int
		channel                    int
	}{
		{"reported_zero", `{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0}`, 0, 0, 0, constant.ChannelTypeOpenAI},
		{"partial_input_and_cache", `{"prompt_tokens":12,"prompt_tokens_details":{"cached_tokens":8}}`, 12, service.CountTextToken("hello", "gpt-4o"), 8, constant.ChannelTypeOpenAI},
		{"conflicting_cache_fields", `{"prompt_tokens":12,"completion_tokens":1,"total_tokens":13,"prompt_tokens_details":{"cached_tokens":0},"prompt_cache_hit_tokens":8}`, 12, 1, 8, constant.ChannelTypeDeepSeek},
		{"cache_alias_only", `{"prompt_tokens":12,"completion_tokens":1,"total_tokens":13,"prompt_cache_hit_tokens":8}`, 12, 1, 8, constant.ChannelTypeDeepSeek},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
			ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			info := &relaycommon.RelayInfo{RelayFormat: types.RelayFormatOpenAI, BillingSource: service.BillingSourceCreditPacks, ChannelMeta: &relaycommon.ChannelMeta{UpstreamModelName: "gpt-4o", ChannelType: tc.channel}}
			body := `{"model":"gpt-4o","choices":[{"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":` + tc.usage + `}`
			usage, apiErr := OpenaiHandler(ctx, info, &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))})
			require.Nil(t, apiErr)
			require.NotNil(t, usage)
			assert.Equal(t, tc.prompt, usage.PromptTokens)
			assert.Equal(t, tc.completion, usage.CompletionTokens)
			assert.Equal(t, tc.cached, usage.PromptTokensDetails.CachedTokens)
			if tc.name == "conflicting_cache_fields" {
				require.Contains(t, info.CreditUsageFacts, "cached_tokens")
				assert.Equal(t, "adaptor", info.CreditUsageFacts["cached_tokens"].Source)
				assert.Equal(t, "new-api-cache-alias-conflict-v1", info.CreditUsageFacts["cached_tokens"].Algorithm)
				require.Contains(t, info.CreditUsageFacts, "openai_cached_tokens")
				assert.Equal(t, float64(0), *info.CreditUsageFacts["openai_cached_tokens"].Quantity)
			} else if tc.name == "cache_alias_only" {
				require.Contains(t, info.CreditUsageFacts, "cached_tokens")
				assert.Equal(t, "upstream", info.CreditUsageFacts["cached_tokens"].Source)
			}
		})
	}
}

func TestCreditTranscriptionUsageRetainsProviderFields(t *testing.T) {
	for _, tc := range []struct {
		name, body                      string
		prompt, completion, audio, text int
		promptSource, completionSource  string
	}{
		{"reported_zero", `{"text":"hello","usage":{"type":"tokens","input_tokens":0,"output_tokens":0,"total_tokens":0}}`, 0, 0, 0, 0, "upstream", "upstream"},
		{"native_modalities", `{"text":"hello","usage":{"type":"tokens","input_tokens":14,"output_tokens":31,"total_tokens":45,"input_token_details":{"audio_tokens":4,"text_tokens":10}}}`, 14, 31, 4, 10, "upstream", "upstream"},
		{"missing_output", `{"text":"hello","usage":{"type":"tokens","input_tokens":14,"input_token_details":{"audio_tokens":14,"text_tokens":0}}}`, 14, service.CountTextToken("hello", "gpt-4o-transcribe"), 14, 0, "upstream", "estimate"},
		{"duration_receipt", `{"text":"hello","usage":{"type":"duration","seconds":2.25}}`, 40, 0, 0, 0, "estimate", "estimate"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", nil)
			info := &relaycommon.RelayInfo{BillingSource: service.BillingSourceCreditPacks, ChannelMeta: &relaycommon.ChannelMeta{UpstreamModelName: "gpt-4o-transcribe"}}
			info.SetEstimatePromptTokens(40)
			apiErr, usage := OpenaiSTTHandler(ctx, &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(tc.body))}, info, "json")
			require.Nil(t, apiErr)
			require.NotNil(t, usage)
			assert.Equal(t, tc.body, recorder.Body.String())
			assert.Equal(t, tc.prompt, usage.PromptTokens)
			assert.Equal(t, tc.completion, usage.CompletionTokens)
			assert.Equal(t, tc.audio, usage.PromptTokensDetails.AudioTokens)
			assert.Equal(t, tc.text, usage.PromptTokensDetails.TextTokens)
			assert.Equal(t, tc.prompt+tc.completion, usage.TotalTokens)
			require.Contains(t, info.CreditUsageFacts, "prompt_tokens")
			require.Contains(t, info.CreditUsageFacts, "completion_tokens")
			assert.Equal(t, tc.promptSource, info.CreditUsageFacts["prompt_tokens"].Source)
			assert.Equal(t, tc.completionSource, info.CreditUsageFacts["completion_tokens"].Source)
			if tc.name == "duration_receipt" {
				require.Contains(t, info.CreditUsageFacts, "audio_input_seconds")
				fact := info.CreditUsageFacts["audio_input_seconds"]
				assert.Equal(t, "second", fact.Unit)
				assert.Equal(t, "upstream", fact.Source)
				require.NotNil(t, fact.Quantity)
				assert.Equal(t, 2.25, *fact.Quantity)
			}
		})
	}
	t.Run("invalid_receipt_is_not_forwarded", func(t *testing.T) {
		for _, body := range []string{
			`{"text":"hello","usage":{"type":"tokens","input_tokens":-1,"output_tokens":1,"total_tokens":0}}`,
			`{"text":"hello","usage":{"type":"tokens","input_tokens":0,"output_tokens":0,"total_tokens":0,"input_token_details":{"audio_tokens":4,"text_tokens":0}}}`,
			`{"text":"hello","usage":{"type":"duration","seconds":-0.5}}`,
			`{"text":"hello","usage":{"type":"tokens","input_tokens":1`,
		} {
			t.Run(body, func(t *testing.T) {
				recorder := httptest.NewRecorder()
				ctx, _ := gin.CreateTestContext(recorder)
				ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", nil)
				info := &relaycommon.RelayInfo{BillingSource: service.BillingSourceCreditPacks, ChannelMeta: &relaycommon.ChannelMeta{UpstreamModelName: "gpt-4o-transcribe"}}
				apiErr, usage := OpenaiSTTHandler(ctx, &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}, info, "json")
				require.NotNil(t, apiErr)
				assert.Nil(t, usage)
				assert.Empty(t, recorder.Body.String())
			})
		}
	})
}

func TestCreditTranscriptionStreamUsage(t *testing.T) {
	previousTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 30
	t.Cleanup(func() { constant.StreamingTimeout = previousTimeout })
	for _, tc := range []struct {
		name, terminal string
		output         int
		source         string
	}{
		{"interrupted", "", 0, "estimate"},
		{"reported_zero", "data: {\"type\":\"transcript.text.done\",\"text\":\"hello\",\"usage\":{\"type\":\"tokens\",\"input_tokens\":14,\"output_tokens\":0,\"total_tokens\":14,\"input_token_details\":{\"audio_tokens\":14,\"text_tokens\":0}}}\n\n", 0, "upstream"},
		{"complete", "data: {\"type\":\"transcript.text.done\",\"text\":\"hello\",\"usage\":{\"type\":\"tokens\",\"input_tokens\":14,\"output_tokens\":5,\"total_tokens\":19,\"input_token_details\":{\"audio_tokens\":14,\"text_tokens\":0}}}\n\n", 5, "upstream"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", nil)
			info := &relaycommon.RelayInfo{BillingSource: service.BillingSourceCreditPacks, IsStream: true, ChannelMeta: &relaycommon.ChannelMeta{UpstreamModelName: "gpt-4o-transcribe"}}
			info.SetEstimatePromptTokens(40)
			stream := "data: {\"type\":\"transcript.text.delta\",\"delta\":\"hello\"}\n\n" + tc.terminal
			apiErr, usage := OpenaiSTTHandler(ctx, &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(stream))}, info, "json")
			require.Nil(t, apiErr)
			require.NotNil(t, usage)
			assert.Contains(t, recorder.Body.String(), `"transcript.text.delta"`)
			if tc.source == "upstream" {
				assert.Equal(t, 14, usage.PromptTokens)
				assert.Equal(t, tc.output, usage.CompletionTokens)
				assert.Equal(t, 14, usage.PromptTokensDetails.AudioTokens)
			} else {
				assert.Equal(t, 40, usage.PromptTokens)
				assert.Equal(t, service.CountTextToken("hello", "gpt-4o-transcribe"), usage.CompletionTokens)
			}
			require.Contains(t, info.CreditUsageFacts, "completion_tokens")
			assert.Equal(t, tc.source, info.CreditUsageFacts["completion_tokens"].Source)
			require.NotNil(t, info.StreamStatus)
			assert.True(t, info.StreamStatus.OutcomeSnapshot().ExpectsTerminal)
			if tc.terminal != "" {
				assert.Equal(t, "completed", info.StreamStatus.ResponseOutcome())
			} else {
				assert.Empty(t, info.StreamStatus.ResponseOutcome(), "transport EOF is not a complete transcription")
			}
		})
	}
}

func TestCreditOpenaiStreamRetainsEarlyPartialUsage(t *testing.T) {
	previousTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 30
	t.Cleanup(func() { constant.StreamingTimeout = previousTimeout })
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	info := &relaycommon.RelayInfo{RelayFormat: types.RelayFormatOpenAI, RelayMode: relayconstant.RelayModeChatCompletions, BillingSource: service.BillingSourceCreditPacks, IsStream: true, StreamStatus: relaycommon.NewStreamStatus(), ChannelMeta: &relaycommon.ChannelMeta{UpstreamModelName: "gpt-4o"}}
	stream := "data: {\"id\":\"r\",\"choices\":[],\"usage\":{\"prompt_tokens\":12,\"prompt_tokens_details\":{\"cached_tokens\":8}}}\n\n" +
		"data: {\"id\":\"r\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"}}]}\n\n" +
		"data: {\"id\":\"r\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n"
	usage, apiErr := OaiStreamHandler(ctx, info, &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(stream))})
	require.Nil(t, apiErr)
	require.NotNil(t, usage)
	assert.Equal(t, 12, usage.PromptTokens)
	assert.Equal(t, 8, usage.PromptTokensDetails.CachedTokens)
	assert.Equal(t, service.CountTextToken("hello", "gpt-4o"), usage.CompletionTokens)
	require.Contains(t, info.CreditUsageFacts, "prompt_tokens")
	assert.Equal(t, "upstream", info.CreditUsageFacts["prompt_tokens"].Source)
	require.Contains(t, info.CreditUsageFacts, "completion_tokens")
	assert.Equal(t, "estimate", info.CreditUsageFacts["completion_tokens"].Source)
}

func TestCreditOpenaiStreamEarlyZeroIsNotFinalUsage(t *testing.T) {
	previousTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 30
	t.Cleanup(func() { constant.StreamingTimeout = previousTimeout })
	for _, tc := range []struct {
		name, tail, source string
		output             int
	}{
		{"interrupted", "", "estimate", service.CountTextToken("hello", "gpt-4o")},
		{"empty_terminal_usage", "data: {\"choices\":[],\"usage\":{}}\n\ndata: [DONE]\n\n", "estimate", service.CountTextToken("hello", "gpt-4o")},
		{"final_reported_zero", "data: {\"choices\":[],\"usage\":{\"completion_tokens\":0}}\n\ndata: [DONE]\n\n", "upstream", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
			ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			info := &relaycommon.RelayInfo{RelayFormat: types.RelayFormatOpenAI, RelayMode: relayconstant.RelayModeChatCompletions, BillingSource: service.BillingSourceCreditPacks, IsStream: true, StreamStatus: relaycommon.NewStreamStatus(), ChannelMeta: &relaycommon.ChannelMeta{UpstreamModelName: "gpt-4o"}}
			stream := "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":12,\"completion_tokens\":0}}\n\n" +
				"data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"}}]}\n\n" + tc.tail
			usage, apiErr := OaiStreamHandler(ctx, info, &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(stream))})
			require.Nil(t, apiErr)
			require.NotNil(t, usage)
			assert.Equal(t, 12, usage.PromptTokens)
			assert.Equal(t, tc.output, usage.CompletionTokens, "only final reported zero can replace the observed output estimate")
			require.Contains(t, info.CreditUsageFacts, "completion_tokens")
			assert.Equal(t, tc.source, info.CreditUsageFacts["completion_tokens"].Source)
		})
	}
}

func TestCreditOpenaiCacheAliasesRemainCumulative(t *testing.T) {
	previousTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 30
	t.Cleanup(func() { constant.StreamingTimeout = previousTimeout })
	for _, conflict := range []bool{false, true} {
		t.Run(fmt.Sprintf("conflict_%t", conflict), func(t *testing.T) {
			ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
			ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			info := &relaycommon.RelayInfo{RelayFormat: types.RelayFormatOpenAI, RelayMode: relayconstant.RelayModeChatCompletions, BillingSource: service.BillingSourceCreditPacks, IsStream: true, StreamStatus: relaycommon.NewStreamStatus(), ChannelMeta: &relaycommon.ChannelMeta{UpstreamModelName: "gpt-4o", ChannelType: constant.ChannelTypeDeepSeek}}
			standard := ""
			if conflict {
				standard = `,"prompt_tokens_details":{"cached_tokens":0}`
			}
			stream := `data: {"choices":[],"usage":{"prompt_tokens":20,"prompt_cache_hit_tokens":8` + standard + "}}\n\n" +
				"data: {\"choices\":[],\"usage\":{\"completion_tokens\":1,\"prompt_cache_hit_tokens\":12}}\n\n" + "data: [DONE]\n\n"
			usage, apiErr := OaiStreamHandler(ctx, info, &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(stream))})
			require.Nil(t, apiErr)
			require.NotNil(t, usage)
			assert.Equal(t, 12, usage.PromptTokensDetails.CachedTokens)
			require.Contains(t, info.CreditUsageFacts, "cached_tokens")
			assert.Equal(t, float64(12), *info.CreditUsageFacts["cached_tokens"].Quantity)
			wantSource := "upstream"
			if conflict {
				wantSource = "adaptor"
				require.Contains(t, info.CreditUsageFacts, "openai_cached_tokens")
				assert.Equal(t, float64(0), *info.CreditUsageFacts["openai_cached_tokens"].Quantity)
			}
			assert.Equal(t, wantSource, info.CreditUsageFacts["cached_tokens"].Source)
		})
	}
}

// Fixtures follow vLLM-Omni's published speech SSE wire contract, not a live
// OpenAI receipt: speech.audio.done nests all token quantities under usage.
func TestCreditSpeechStreamUsage(t *testing.T) {
	previousTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 30
	t.Cleanup(func() { constant.StreamingTimeout = previousTimeout })
	delta := fmt.Sprintf(`data: {"type":"speech.audio.delta","audio":%q,"response_format":"pcm"}`+"\n\n", base64.StdEncoding.EncodeToString(make([]byte, 1200)))
	for _, tc := range []struct {
		name, terminal, outcome, source string
		prompt, output, text, audio     int
	}{
		{"completed", `data: {"type":"speech.audio.done","usage":{"input_tokens":14,"output_tokens":31,"total_tokens":45,"input_token_details":{"text_tokens":10,"audio_tokens":4}}}` + "\n\n", "completed", "upstream", 14, 31, 10, 4},
		{"explicit_zero", `data: {"type":"speech.audio.done","usage":{"input_tokens":0,"output_tokens":0,"total_tokens":0,"input_token_details":{"text_tokens":0,"audio_tokens":0}}}` + "\n\n", "completed", "upstream", 0, 0, 0, 0},
		{"interrupted", "", "", "estimate", 40, 2, 40, 0},
		{"partial_lower_bound", `data: {"type":"speech.audio.delta","usage":{"input_tokens":14,"output_tokens":7,"total_tokens":21}}` + "\n\n", "", "estimate", 14, 7, 40, 0},
		{"provider_failure", `data: {"type":"speech.audio.error","error":{"message":"sensitive upstream text","type":"server_error","code":500}}` + "\n\n", "failed", "estimate", 40, 2, 40, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/audio/speech", nil)
			info := &relaycommon.RelayInfo{BillingSource: service.BillingSourceCreditPacks, RelayMode: relayconstant.RelayModeAudioSpeech, ChannelMeta: &relaycommon.ChannelMeta{UpstreamModelName: "tts-model"}}
			info.SetEstimatePromptTokens(40)
			// The actual SSE content type must select streaming even if the request
			// flag was absent. Binary audio must not go through the SSE scanner.
			stream := delta + tc.terminal
			if tc.name == "explicit_zero" {
				stream = tc.terminal
			}
			value, apiErr := (&Adaptor{}).DoResponse(ctx, &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(stream))}, info)
			require.Nil(t, apiErr)
			usage, ok := value.(*dto.Usage)
			require.True(t, ok)
			require.NotNil(t, usage)
			if tc.name != "explicit_zero" {
				assert.Contains(t, recorder.Body.String(), `"speech.audio.delta"`)
			}
			assert.Equal(t, tc.prompt, usage.PromptTokens)
			assert.Equal(t, tc.output, usage.CompletionTokens)
			assert.Equal(t, tc.output, usage.CompletionTokenDetails.AudioTokens)
			if tc.source == "upstream" {
				assert.Equal(t, tc.text, usage.PromptTokensDetails.TextTokens)
				assert.Equal(t, tc.audio, usage.PromptTokensDetails.AudioTokens)
			}
			assert.Equal(t, tc.prompt+tc.output, usage.TotalTokens)
			require.Contains(t, info.CreditUsageFacts, "completion_tokens")
			assert.Equal(t, tc.source, info.CreditUsageFacts["completion_tokens"].Source)
			assert.False(t, info.CreditUsageFacts["completion_tokens"].Partial)
			require.NotNil(t, info.StreamStatus)
			assert.True(t, info.StreamStatus.OutcomeSnapshot().ExpectsTerminal)
			assert.Equal(t, tc.outcome, info.StreamStatus.ResponseOutcome())
			if tc.outcome == "failed" {
				assert.Equal(t, "server_error", info.StreamStatus.OutcomeSnapshot().ErrorType)
				assert.Equal(t, 500, info.StreamStatus.OutcomeSnapshot().ErrorStatus)
			}
		})
	}
}

func TestCreditSpeechInvalidReceiptIsNotForwarded(t *testing.T) {
	previousTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 30
	t.Cleanup(func() { constant.StreamingTimeout = previousTimeout })
	for _, frame := range []string{
		`{"type":"speech.audio.done","usage":{"input_tokens":-1,"output_tokens":1,"total_tokens":0}}`,
		`{"type":"speech.audio.done","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":3}}`,
		`{"type":"speech.audio.done","usage":{"input_tokens":0,"output_tokens":0,"total_tokens":0,"input_token_details":{"text_tokens":1}}}`,
		`{"type":"speech.audio.delta","audio":"***"}`,
		`{"type":"speech.audio.done","usage":{"input_tokens":1`,
	} {
		t.Run(frame, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/audio/speech", nil)
			info := &relaycommon.RelayInfo{BillingSource: service.BillingSourceCreditPacks, RelayMode: relayconstant.RelayModeAudioSpeech, ChannelMeta: &relaycommon.ChannelMeta{UpstreamModelName: "tts-model"}}
			usage, apiErr := (&Adaptor{}).DoResponse(ctx, &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("data: " + frame + "\n\n"))}, info)
			require.NotNil(t, apiErr)
			assert.Nil(t, usage)
			assert.NotContains(t, recorder.Body.String(), frame)
		})
	}
}

func TestCreditSpeechBinaryUsageHeaders(t *testing.T) {
	for _, tc := range []struct {
		name                       string
		input, output, text, audio int
		stream                     bool
	}{
		{"native_modalities", 14, 31, 10, 4, false},
		{"reported_zero", 0, 0, 0, 0, false},
		{"binary_content_type_overrides_request_sse_flag", 14, 31, 10, 4, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/audio/speech", nil)
			info := &relaycommon.RelayInfo{BillingSource: service.BillingSourceCreditPacks, RelayMode: relayconstant.RelayModeAudioSpeech, IsStream: tc.stream, Request: &dto.AudioRequest{ResponseFormat: "pcm"}, ChannelMeta: &relaycommon.ChannelMeta{UpstreamModelName: "tts-model"}}
			info.SetEstimatePromptTokens(40)
			headers := http.Header{"Content-Type": {"audio/pcm"}}
			for name, value := range map[string]int{"input-tokens": tc.input, "output-tokens": tc.output, "total-tokens": tc.input + tc.output, "input-text-tokens": tc.text, "input-audio-tokens": tc.audio} {
				headers.Set("x-vllm-omni-"+name, fmt.Sprint(value))
			}
			body := strings.Repeat("a", 48000)
			if tc.name == "reported_zero" {
				body = ""
			}
			value, apiErr := (&Adaptor{}).DoResponse(ctx, &http.Response{StatusCode: 200, Header: headers, Body: io.NopCloser(strings.NewReader(body))}, info)
			require.Nil(t, apiErr)
			usage, ok := value.(*dto.Usage)
			require.True(t, ok)
			require.NotNil(t, usage)
			assert.Equal(t, body, recorder.Body.String())
			assert.Equal(t, tc.input, usage.PromptTokens)
			assert.Equal(t, tc.output, usage.CompletionTokens)
			assert.Equal(t, tc.output, usage.CompletionTokenDetails.AudioTokens)
			assert.Equal(t, tc.text, usage.PromptTokensDetails.TextTokens)
			assert.Equal(t, tc.audio, usage.PromptTokensDetails.AudioTokens)
			for _, field := range []string{"prompt_tokens", "completion_tokens", "audio_output_tokens", "text_input_tokens", "audio_input_tokens"} {
				require.Contains(t, info.CreditUsageFacts, field)
				assert.Equal(t, "upstream", info.CreditUsageFacts[field].Source)
				assert.False(t, info.CreditUsageFacts[field].Partial)
			}
		})
	}
	t.Run("invalid_headers_are_not_forwarded", func(t *testing.T) {
		for _, invalid := range []string{"-1", "1.5", "NaN", "2147483648", ""} {
			t.Run(invalid, func(t *testing.T) {
				recorder := httptest.NewRecorder()
				ctx, _ := gin.CreateTestContext(recorder)
				ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/audio/speech", nil)
				info := &relaycommon.RelayInfo{BillingSource: service.BillingSourceCreditPacks, RelayMode: relayconstant.RelayModeAudioSpeech, Request: &dto.AudioRequest{ResponseFormat: "pcm"}, ChannelMeta: &relaycommon.ChannelMeta{UpstreamModelName: "tts-model"}}
				header := http.Header{"Content-Type": {"audio/pcm"}}
				header.Set("x-vllm-omni-input-tokens", invalid)
				usage, apiErr := (&Adaptor{}).DoResponse(ctx, &http.Response{StatusCode: 200, Header: header, Body: io.NopCloser(strings.NewReader("audio"))}, info)
				require.NotNil(t, apiErr)
				assert.Nil(t, usage)
				assert.Empty(t, recorder.Body.String())
			})
		}
	})
}

func TestCreditSpeechPartialHeaderDetailsAndReadFailure(t *testing.T) {
	for _, tc := range []struct {
		name        string
		readFailure bool
	}{
		{"missing_text_partition", false},
		{"receipt_survives_body_read_failure", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/audio/speech", nil)
			info := &relaycommon.RelayInfo{BillingSource: service.BillingSourceCreditPacks, RelayMode: relayconstant.RelayModeAudioSpeech, Request: &dto.AudioRequest{ResponseFormat: "pcm"}, ChannelMeta: &relaycommon.ChannelMeta{UpstreamModelName: "tts-model"}}
			info.SetEstimatePromptTokens(40)
			headers := http.Header{"Content-Type": {"audio/pcm"}}
			for name, value := range map[string]int{"input-tokens": 14, "output-tokens": 31, "total-tokens": 45, "input-audio-tokens": 4} {
				headers.Set("x-vllm-omni-"+name, fmt.Sprint(value))
			}
			body := io.Reader(strings.NewReader(strings.Repeat("a", 48000)))
			if tc.readFailure {
				body = iotest.ErrReader(errors.New("upstream body read failed"))
			}
			value, apiErr := (&Adaptor{}).DoResponse(ctx, &http.Response{StatusCode: 200, Header: headers, Body: io.NopCloser(body)}, info)
			require.Nil(t, apiErr)
			usage, ok := value.(*dto.Usage)
			require.True(t, ok)
			require.NotNil(t, usage)
			assert.Equal(t, 14, usage.PromptTokens)
			assert.Equal(t, 31, usage.CompletionTokens)
			assert.Equal(t, 10, usage.PromptTokensDetails.TextTokens, "unknown text must not double-count the known reference audio")
			assert.Equal(t, 4, usage.PromptTokensDetails.AudioTokens)
			require.Contains(t, info.CreditUsageFacts, "text_input_tokens")
			assert.Equal(t, "estimate", info.CreditUsageFacts["text_input_tokens"].Source)
			if tc.readFailure {
				assert.Empty(t, recorder.Body.String())
				assert.Equal(t, "incomplete", info.StreamStatus.ResponseOutcome())
			}
		})
	}
}

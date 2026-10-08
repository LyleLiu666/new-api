package controller

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/pem"
	"errors"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Calcium-Ion/go-epay/epay"
	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	pluginruntime "github.com/QuantumNous/new-api/pkg/jsplugin"
	taskpluginadaptor "github.com/QuantumNous/new-api/relay/channel/task/jsplugin"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting"
	"github.com/QuantumNous/new-api/setting/config"
	"github.com/QuantumNous/new-api/setting/model_setting"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	hosttypes "github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/stripe/stripe-go/v81"
	"github.com/stripe/stripe-go/v81/webhook"
	pancake "github.com/waffo-com/waffo-pancake-sdk-go"
	"gorm.io/gorm"
)

// Payment API calls stay inside a local gateway, including hard-coded SDK URLs.
type subscriptionGatewayTestTransport struct {
	target   *url.URL
	upstream http.RoundTripper
}

// Partial transport acceptance is distinct from client receipt and from a
// zero-byte failure. Preserve Gin's other writer interfaces in this fixture.
type creditBoundaryTestWriter struct {
	gin.ResponseWriter
	accepted int
}

func (w creditBoundaryTestWriter) Write(data []byte) (int, error) {
	n := min(w.accepted, len(data))
	if n > 0 {
		_, _ = w.ResponseWriter.Write(data[:n])
	}
	return n, errors.New("fixture writer failure")
}

func (transport subscriptionGatewayTestTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.URL.Host != "api.creem.io" && request.URL.Host != "test-api.creem.io" && request.URL.Host != "api.waffo.ai" {
		return nil, fmt.Errorf("unexpected payment gateway host: %s", request.URL.Host)
	}
	clone := request.Clone(request.Context())
	clone.URL.Scheme, clone.URL.Host = transport.target.Scheme, transport.target.Host
	return transport.upstream.RoundTrip(clone)
}

func TestCreditBillingDatabaseMatrix(t *testing.T) {
	gin.SetMode(gin.TestMode)
	service.InitHttpClient()
	previousConfig := config.GlobalConfig.ExportAllConfigs()
	previousStreamingTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 30
	previousCount, previousBatch, previousLogs, previousUnit := constant.CountToken, common.BatchUpdateEnabled, common.LogConsumeEnabled, common.QuotaPerUnit
	constant.CountToken, common.BatchUpdateEnabled, common.LogConsumeEnabled, common.QuotaPerUnit = false, false, true, 500000
	t.Cleanup(func() {
		constant.StreamingTimeout = previousStreamingTimeout
		require.NoError(t, config.GlobalConfig.LoadFromDB(map[string]string{"group_ratio_setting.group_ratio": previousConfig["group_ratio_setting.group_ratio"], "quota_setting.trust_quota_usd": previousConfig["quota_setting.trust_quota_usd"]}))
		constant.CountToken, common.BatchUpdateEnabled, common.LogConsumeEnabled, common.QuotaPerUnit = previousCount, previousBatch, previousLogs, previousUnit
	})
	for _, dialect := range []struct{ name, env string }{{"sqlite", ""}, {"mysql", "TEST_MYSQL_DSN"}, {"postgres", "TEST_POSTGRES_DSN"}} {
		t.Run(dialect.name, func(t *testing.T) {
			if dialect.env != "" && os.Getenv(dialect.env) == "" {
				t.Skip(dialect.env + " not configured")
			}
			db := modelManagementDB(t, dialect.name, os.Getenv(dialect.env))
			require.NoError(t, db.AutoMigrate(&model.UserSubscription{}, &model.Token{}, &model.Log{}, &model.CreditLogDelivery{}))
			require.NoError(t, model.MigrateCreditAccounting(db))
			require.NoError(t, config.GlobalConfig.LoadFromDB(map[string]string{
				"billing_setting.billing_mode":    `{"credit-model":"tiered_expr"}`,
				"billing_setting.billing_expr":    `{"credit-model":"tier(\"request\", fixed(0.00007))"}`,
				"group_ratio_setting.group_ratio": `{"default":1}`,
				"quota_setting.trust_quota_usd":   "10",
			}))
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				assert.Equal(t, "/v1/chat/completions", r.URL.Path)
				w.Header().Set("Content-Type", "application/json")
				_, err := fmt.Fprint(w, `{"id":"reply","object":"chat.completion","model":"credit-model","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`)
				assert.NoError(t, err)
			}))
			t.Cleanup(upstream.Close)
			channel := model.Channel{Name: "credit-upstream", Type: constant.ChannelTypeOpenAI, Key: "test-upstream-key", Status: common.ChannelStatusEnabled, Group: "default", Models: "credit-model", BaseURL: &upstream.URL}
			require.NoError(t, db.Create(&channel).Error)
			require.NoError(t, db.Create(&model.Ability{ChannelId: channel.Id, Model: "credit-model", Group: "default", Enabled: true}).Error)
			user := model.User{Username: "credit-user", Password: "unused", Status: common.UserStatusEnabled, Group: "default", AffCode: "credit-user", Quota: 5500000, AccountingVersion: 1, Setting: `{"billing_preference":"wallet_only"}`}
			require.NoError(t, db.Create(&user).Error)
			token := model.Token{UserId: user.Id, Key: strings.Repeat("c", 48), Status: common.TokenStatusEnabled, RemainQuota: 1000, ExpiredTime: -1, Group: "default"}
			require.NoError(t, db.Create(&token).Error)
			now := common.GetTimestamp()
			for i, amount := range []int64{30, 100} {
				_, err := model.GrantCreditPack(db, model.CreditGrant{UserID: user.Id, SourceType: "test", SourceID: fmt.Sprint(i), Amount: amount, StartsAt: now, ExpiresAt: now + int64((i+1)*3600), UseMask: model.CreditUseAPI}, now)
				require.NoError(t, err)
			}
			engine := gin.New()
			engine.Use(middleware.RequestId())
			engine.POST("/v1/chat/completions", middleware.TokenAuth(), middleware.Distribute(), func(c *gin.Context) { Relay(c, types.RelayFormatOpenAI) })
			engine.POST("/v1/images/generations", middleware.TokenAuth(), middleware.Distribute(), func(c *gin.Context) { Relay(c, types.RelayFormatOpenAIImage) })
			response := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"credit-model","messages":[{"role":"user","content":"hi"}]}`))
			request.Header.Set("Authorization", "Bearer sk-"+token.Key)
			request.Header.Set("Content-Type", "application/json")
			engine.ServeHTTP(response, request)
			require.Equal(t, http.StatusOK, response.Code, response.Body.String())
			assert.Contains(t, response.Body.String(), "hello")
			assert.EqualValues(t, 1, calls.Load())
			var bill model.CreditRequest
			require.NoError(t, db.Where("user_id = ?", user.Id).First(&bill).Error)
			assert.Equal(t, "settled", bill.State)
			assert.EqualValues(t, 35, bill.Reserved, "large legacy scalar balance never activates the trust bypass")
			assert.EqualValues(t, 35, bill.Charged)
			assert.NotZero(t, bill.SubmittedAt)
			assert.Contains(t, bill.PriceSnapshot, "fixed(0.00007)")
			var projection model.CreditLogOutbox
			require.NoError(t, db.Where("request_id = ?", bill.ID).First(&projection).Error)
			var consumeLog model.Log
			require.NoError(t, common.UnmarshalJsonStr(projection.Payload, &consumeLog))
			assert.Equal(t, 10, consumeLog.PromptTokens, "durable logs retain the actual upstream usage alongside the charge")
			assert.Equal(t, 5, consumeLog.CompletionTokens)
			assert.Contains(t, consumeLog.Other, `"usage_evidence"`)
			packs, err := model.ListCreditPacks(db, user.Id, now)
			require.NoError(t, err)
			require.Len(t, packs, 2)
			assert.EqualValues(t, 30, packs[0].Spent)
			assert.EqualValues(t, 5, packs[1].Spent)
			assert.EqualValues(t, 95, packs[1].Available)
			require.NoError(t, db.First(&user, user.Id).Error)
			assert.Equal(t, 5500000, user.Quota, "legacy scalar is not an independent funding source")
			require.NoError(t, db.First(&token, token.Id).Error)
			assert.Equal(t, 965, token.RemainQuota)

			t.Run("account_retry_uses_one_bill_and_observed_boundaries", func(t *testing.T) {
				oldRetry := common.RetryTimes
				common.RetryTimes = 2
				t.Cleanup(func() { common.RetryTimes = oldRetry })
				affinity := operation_setting.GetChannelAffinitySetting()
				oldAffinity := *affinity
				*affinity = operation_setting.ChannelAffinitySetting{}
				t.Cleanup(func() { *affinity = oldAffinity })
				require.NoError(t, db.Model(&model.Ability{}).Where("channel_id = ?", channel.Id).Update("enabled", false).Error)
				t.Cleanup(func() {
					require.NoError(t, db.Model(&model.Ability{}).Where("channel_id = ?", channel.Id).Update("enabled", true).Error)
				})
				for _, kind := range []string{"known_429", "unknown_submission", "output_started"} {
					t.Run(kind, func(t *testing.T) {
						var submitted atomic.Int32
						var keysMu sync.Mutex
						var actualKeys []string
						upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
							attempt := submitted.Add(1)
							keysMu.Lock()
							actualKeys = append(actualKeys, r.Header.Get("Authorization"))
							keysMu.Unlock()
							if kind == "known_429" && attempt == 1 {
								w.Header().Set("Content-Type", "application/json")
								w.WriteHeader(429)
								_, _ = fmt.Fprint(w, `{"error":{"message":"rate limited","type":"rate_limit_error"}}`)
								return
							}
							if kind == "unknown_submission" {
								conn, _, err := w.(http.Hijacker).Hijack()
								if assert.NoError(t, err) {
									_ = conn.Close()
								}
								return
							}
							if kind == "output_started" {
								w.Header().Set("Content-Type", "text/event-stream")
								_, _ = fmt.Fprint(w, "data: {\"id\":\"partial\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial-output\"}}]}\n\n")
								w.(http.Flusher).Flush()
								conn, _, err := w.(http.Hijacker).Hijack()
								if assert.NoError(t, err) {
									_ = conn.Close()
								}
								return
							}
							w.Header().Set("Content-Type", "application/json")
							_, _ = fmt.Fprint(w, `{"id":"retry-ok","object":"chat.completion","model":"credit-model","choices":[{"index":0,"message":{"role":"assistant","content":"completed"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`)
						}))
						t.Cleanup(upstream.Close)
						ch := model.Channel{Name: "account-retry-" + kind, Type: constant.ChannelTypeOpenAI, Key: "retry-first\nretry-second", Status: common.ChannelStatusEnabled, Group: "default", Models: "credit-model", BaseURL: &upstream.URL, ChannelInfo: model.ChannelInfo{IsMultiKey: true, MultiKeyMode: constant.MultiKeyModePolling}}
						require.NoError(t, ch.Insert())
						t.Cleanup(func() {
							require.NoError(t, db.Model(&model.Ability{}).Where("channel_id = ?", ch.Id).Update("enabled", false).Error)
						})
						u := model.User{Username: "account-retry-" + kind, Password: "unused", Status: common.UserStatusEnabled, Group: "default", AffCode: "account-retry-" + kind, AccountingVersion: 1, Setting: `{"billing_preference":"wallet_only"}`}
						require.NoError(t, db.Create(&u).Error)
						key := model.Token{UserId: u.Id, Key: common.GetUUID() + strings.Repeat("r", 16), Status: common.TokenStatusEnabled, RemainQuota: 1000, ExpiredTime: -1, Group: "default"}
						require.NoError(t, db.Create(&key).Error)
						_, err := model.GrantCreditPack(db, model.CreditGrant{UserID: u.Id, SourceType: "test", SourceID: kind, Amount: 1000, StartsAt: now, ExpiresAt: now + 3600, UseMask: model.CreditUseAPI}, now)
						require.NoError(t, err)
						body := `{"model":"credit-model","messages":[{"role":"user","content":"hi"}],"stream":` + fmt.Sprint(kind == "output_started") + `}`
						r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
						r.Header.Set("Authorization", "Bearer sk-"+key.Key)
						r.Header.Set("Content-Type", "application/json")
						response := httptest.NewRecorder()
						engine.ServeHTTP(response, r)
						var bills []model.CreditRequest
						require.NoError(t, db.Where("user_id = ?", u.Id).Find(&bills).Error)
						require.Len(t, bills, 1, "retries share one logical bill")
						var attempts []model.CreditUsageEvidence
						require.NoError(t, db.Where("request_id = ? AND stage = ?", bills[0].ID, "attempt").Order("attempt asc").Find(&attempts).Error)
						if kind == "known_429" {
							require.Equal(t, http.StatusOK, response.Code, response.Body.String())
							assert.EqualValues(t, 2, submitted.Load())
							require.Len(t, attempts, 2)
							assert.EqualValues(t, 35, bills[0].Charged)
							keysMu.Lock()
							assert.Equal(t, []string{"Bearer retry-first", "Bearer retry-second"}, actualKeys)
							keysMu.Unlock()
						} else {
							assert.EqualValues(t, 1, submitted.Load(), "ambiguous submissions and output never replay")
							require.Len(t, attempts, 1)
							if kind == "unknown_submission" {
								assert.Equal(t, "review", bills[0].State)
								assert.Zero(t, bills[0].Charged)
								assert.Positive(t, bills[0].Reserved)
							}
							if kind == "output_started" {
								assert.Contains(t, response.Body.String(), "partial-output")
							}
						}
						var identity string
						for _, attempt := range attempts {
							var receipt model.CreditEvidenceInput
							require.NoError(t, common.UnmarshalJsonStr(attempt.Payload, &receipt))
							require.NotNil(t, receipt.AttemptPrice)
							assert.NotEmpty(t, receipt.AttemptPrice.AccountID)
							assert.EqualValues(t, 1, receipt.AttemptPrice.CredentialVersion)
							if identity != "" {
								assert.NotEqual(t, identity, receipt.AttemptPrice.AccountID)
							}
							identity = receipt.AttemptPrice.AccountID
							assert.NotContains(t, attempt.Payload, "retry-first")
							assert.NotContains(t, attempt.Payload, "retry-second")
						}
						differences, err := model.ReconcileCreditAccount(db, u.Id)
						require.NoError(t, err)
						assert.Empty(t, differences)
					})
				}
			})

			t.Run("unlimited_key_cannot_spend_expired_pack", func(t *testing.T) {
				blocked := model.User{Username: "expired-credit", Password: "unused", Status: common.UserStatusEnabled, Group: "default", AffCode: "expired-credit", Quota: 5500000, AccountingVersion: 1, Setting: user.Setting}
				require.NoError(t, db.Create(&blocked).Error)
				key := model.Token{UserId: blocked.Id, Key: strings.Repeat("d", 48), Status: common.TokenStatusEnabled, UnlimitedQuota: true, ExpiredTime: -1, Group: "default"}
				require.NoError(t, db.Create(&key).Error)
				_, err := model.GrantCreditPack(db, model.CreditGrant{UserID: blocked.Id, SourceType: "test", SourceID: "expired", Amount: 100, StartsAt: now - 100, ExpiresAt: now - 1, UseMask: model.CreditUseAPI}, now)
				require.NoError(t, err)
				r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"credit-model","messages":[{"role":"user","content":"hi"}]}`))
				r.Header.Set("Authorization", "Bearer sk-"+key.Key)
				r.Header.Set("Content-Type", "application/json")
				w := httptest.NewRecorder()
				engine.ServeHTTP(w, r)
				assert.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
				assert.EqualValues(t, 1, calls.Load(), "failure occurs before the upstream call")
				require.NoError(t, db.First(&key, key.Id).Error)
				assert.Zero(t, key.UsedQuota)
			})
			t.Run("invalid_image_quantity_fails_before_upstream", func(t *testing.T) {
				r := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(`{"model":"credit-model","prompt":"hi","n":129}`))
				r.Header.Set("Authorization", "Bearer sk-"+token.Key)
				r.Header.Set("Content-Type", "application/json")
				w := httptest.NewRecorder()
				engine.ServeHTTP(w, r)
				assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
				assert.EqualValues(t, 1, calls.Load())
			})
			t.Run("subscription_only_never_silently_spends_packs", func(t *testing.T) {
				require.NoError(t, db.Model(&user).Update("setting", `{"billing_preference":"subscription_only"}`).Error)
				r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"credit-model","messages":[{"role":"user","content":"hi"}]}`))
				r.Header.Set("Authorization", "Bearer sk-"+token.Key)
				r.Header.Set("Content-Type", "application/json")
				w := httptest.NewRecorder()
				engine.ServeHTTP(w, r)
				assert.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
				assert.Contains(t, w.Body.String(), `"code":"subscription_rights_unavailable"`)
				assert.EqualValues(t, 1, calls.Load())
				require.NoError(t, db.First(&token, token.Id).Error)
				assert.Equal(t, 965, token.RemainQuota)
			})
			t.Run("image_growth_and_audio_settle_source_packs", func(t *testing.T) {
				require.NoError(t, config.GlobalConfig.LoadFromDB(map[string]string{
					"billing_setting.billing_mode": `{"credit-model":"tiered_expr","credit-image":"tiered_expr","credit-audio":"tiered_expr","credit-embedding":"tiered_expr","credit-transcription":"tiered_expr","credit-transcriptionstream":"tiered_expr","credit-speechstream":"tiered_expr","credit-speechheaders":"tiered_expr","credit-speechheaderszero":"tiered_expr"}`,
					"billing_setting.billing_expr": `{"credit-model":"tier(\"request\", fixed(0.00007))","credit-image":"tier(\"image\", fixed(0.00002)) * image_count","credit-audio":"tier(\"speech\", fixed(0.00004))","credit-embedding":"tier(\"embedding\", fixed(0.00003))","credit-transcription":"tier(\"transcription\", fixed(0.00004))","credit-transcriptionstream":"tier(\"transcription\", fixed(0.00004))","credit-speechstream":"tier(\"speech\", fixed(0.00004))","credit-speechheaders":"tier(\"speech\", fixed(0.00004))","credit-speechheaderszero":"tier(\"speech\", fixed(0.00004))"}`,
				}))
				for _, tc := range []struct {
					name, path, body, reply, contentType string
					reserved, charged                    int64
				}{
					{"image", "/v1/images/generations", `{"model":"credit-image","prompt":"hi","n":1}`, `{"created":1,"data":[{"url":"https://example.test/image"},{"b64_json":"aW1hZ2U="},{"revised_prompt":"hi"}]}`, "application/json", 30, 10},
					{"audio", "/v1/audio/speech", `{"model":"credit-audio","input":"hi","voice":"alloy","response_format":"pcm"}`, strings.Repeat("a", 48000), "audio/pcm", 20, 20},
					{"speechstream", "/v1/audio/speech", `{"model":"credit-speechstream","input":"hi","voice":"alloy","stream_format":"sse"}`, "data: {\"type\":\"speech.audio.delta\",\"audio\":\"AA==\"}\n\ndata: {\"type\":\"speech.audio.done\",\"usage\":{\"input_tokens\":14,\"output_tokens\":31,\"total_tokens\":45,\"input_token_details\":{\"audio_tokens\":4,\"text_tokens\":10}}}\n\n", "text/event-stream", 20, 20},
					{"speechheaders", "/v1/audio/speech", `{"model":"credit-speechheaders","input":"hi","voice":"alloy","response_format":"pcm"}`, strings.Repeat("a", 48000), "audio/pcm", 20, 20},
					{"speechheaderszero", "/v1/audio/speech", `{"model":"credit-speechheaderszero","input":"hi","voice":"alloy","response_format":"pcm"}`, "", "audio/pcm", 20, 20},
					{"embedding", "/v1/embeddings", `{"model":"credit-embedding","input":"hi"}`, `{"object":"list","data":[{"object":"embedding","embedding":[0.1,0.2],"index":0}],"model":"credit-embedding","usage":{"prompt_tokens":3,"total_tokens":3}}`, "application/json", 15, 15},
					{"transcription", "/v1/audio/transcriptions", "", `{"text":"hello","usage":{"type":"tokens","input_tokens":14,"output_tokens":31,"total_tokens":45,"input_token_details":{"audio_tokens":4,"text_tokens":10}}}`, "application/json", 20, 20},
					{"transcriptionstream", "/v1/audio/transcriptions", "", "data: {\"type\":\"transcript.text.delta\",\"delta\":\"hello\"}\n\ndata: {\"type\":\"transcript.text.done\",\"text\":\"hello\",\"usage\":{\"type\":\"tokens\",\"input_tokens\":14,\"output_tokens\":31,\"total_tokens\":45,\"input_token_details\":{\"audio_tokens\":4,\"text_tokens\":10}}}\n\n", "text/event-stream", 20, 20},
				} {
					t.Run(tc.name, func(t *testing.T) {
						var upstreamCalls atomic.Int32
						server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
							upstreamCalls.Add(1)
							assert.Equal(t, tc.path, r.URL.Path)
							if tc.name == "image" {
								var body struct {
									N int `json:"n"`
								}
								require.NoError(t, common.DecodeJson(r.Body, &body))
								assert.Equal(t, 3, body.N)
							}
							if strings.HasPrefix(tc.name, "speechheaders") {
								for name, value := range map[string]int{"input-tokens": 14, "output-tokens": 31, "total-tokens": 45, "input-text-tokens": 10, "input-audio-tokens": 4} {
									if tc.name == "speechheaderszero" {
										value = 0
									}
									w.Header().Set("x-vllm-omni-"+name, fmt.Sprint(value))
								}
							}
							w.Header().Set("Content-Type", tc.contentType)
							_, err := fmt.Fprint(w, tc.reply)
							assert.NoError(t, err)
						}))
						defer server.Close()
						modelName := "credit-" + tc.name
						ch := model.Channel{Name: modelName, Type: constant.ChannelTypeOpenAI, Key: "test-key", Status: common.ChannelStatusEnabled, Group: "default", Models: modelName, BaseURL: &server.URL}
						if tc.name == "image" {
							ch.ParamOverride = common.GetPointer(`{"operations":[{"path":"n","mode":"set","value":3}]}`)
						}
						require.NoError(t, db.Create(&ch).Error)
						require.NoError(t, db.Create(&model.Ability{ChannelId: ch.Id, Model: modelName, Group: "default", Enabled: true}).Error)
						u := model.User{Username: modelName, Password: "unused", Status: common.UserStatusEnabled, Group: "default", AffCode: modelName, AccountingVersion: 1, Setting: `{"billing_preference":"wallet_only"}`}
						require.NoError(t, db.Create(&u).Error)
						key := model.Token{UserId: u.Id, Key: tc.name + strings.Repeat("0", 48-len(tc.name)), Status: common.TokenStatusEnabled, RemainQuota: 100, ExpiredTime: -1, Group: "default"}
						require.NoError(t, db.Create(&key).Error)
						_, err := model.GrantCreditPack(db, model.CreditGrant{UserID: u.Id, SourceType: "test", SourceID: modelName, Amount: 100, StartsAt: now, ExpiresAt: now + 3600, UseMask: model.CreditUseAPI}, now)
						require.NoError(t, err)
						e := gin.New()
						e.Use(middleware.RequestId())
						format := types.RelayFormat(types.RelayFormatOpenAIImage)
						if tc.name == "audio" || strings.HasPrefix(tc.name, "speech") || strings.HasPrefix(tc.name, "transcription") {
							format = types.RelayFormatOpenAIAudio
						}
						if tc.name == "embedding" {
							format = types.RelayFormatEmbedding
						}
						e.POST(tc.path, middleware.TokenAuth(), middleware.Distribute(), func(c *gin.Context) { Relay(c, format) })
						r := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body))
						r.Header.Set("Authorization", "Bearer sk-"+key.Key)
						r.Header.Set("Content-Type", "application/json")
						if strings.HasPrefix(tc.name, "transcription") {
							var body bytes.Buffer
							form := multipart.NewWriter(&body)
							require.NoError(t, form.WriteField("model", modelName))
							require.NoError(t, form.WriteField("response_format", "json"))
							if tc.name == "transcriptionstream" {
								require.NoError(t, form.WriteField("stream", "true"))
							}
							file, err := form.CreateFormFile("file", "input.wav")
							require.NoError(t, err)
							// One second of 8 kHz mono, 16-bit PCM; real duration parsing.
							var wav bytes.Buffer
							wav.WriteString("RIFF")
							require.NoError(t, binary.Write(&wav, binary.LittleEndian, uint32(16036)))
							wav.WriteString("WAVEfmt ")
							for _, value := range []any{uint32(16), uint16(1), uint16(1), uint32(8000), uint32(16000), uint16(2), uint16(16)} {
								require.NoError(t, binary.Write(&wav, binary.LittleEndian, value))
							}
							wav.WriteString("data")
							require.NoError(t, binary.Write(&wav, binary.LittleEndian, uint32(16000)))
							wav.Write(make([]byte, 16000))
							_, err = file.Write(wav.Bytes())
							require.NoError(t, err)
							require.NoError(t, form.Close())
							r = httptest.NewRequest(http.MethodPost, tc.path, &body)
							r.Header.Set("Authorization", "Bearer sk-"+key.Key)
							r.Header.Set("Content-Type", form.FormDataContentType())
						}
						w := httptest.NewRecorder()
						e.ServeHTTP(w, r)
						require.Equal(t, http.StatusOK, w.Code, w.Body.String())
						assert.EqualValues(t, 1, upstreamCalls.Load())
						var request model.CreditRequest
						require.NoError(t, db.Where("user_id = ?", u.Id).First(&request).Error)
						assert.Equal(t, "settled", request.State)
						assert.NotZero(t, request.SubmittedAt)
						assert.Equal(t, tc.reserved, request.Reserved)
						assert.Equal(t, tc.charged, request.Charged)
						assert.NotZero(t, request.UsageEvidenceID, "media settlement persists metering before the monetary intent")
						if tc.name == "image" {
							var evidence model.CreditUsageEvidence
							require.NoError(t, db.First(&evidence, request.UsageEvidenceID).Error)
							var receipt model.CreditEvidenceInput
							require.NoError(t, common.UnmarshalJsonStr(evidence.Payload, &receipt))
							for _, field := range []string{"image_count", "prompt_tokens", "completion_tokens"} {
								index := slices.IndexFunc(receipt.Facts, func(fact hosttypes.UsageFact) bool { return fact.Field == field })
								require.NotEqual(t, -1, index, field)
								fact := receipt.Facts[index]
								switch field {
								case "image_count":
									require.NotNil(t, fact.Quantity)
									assert.Equal(t, float64(1), *fact.Quantity, "split URL/base64 payloads bill one image")
									assert.Equal(t, "count", fact.Unit)
									assert.Equal(t, "adaptor", fact.Source)
									assert.Equal(t, "new-api-image-payload-count-v1", fact.Algorithm)
								case "prompt_tokens", "completion_tokens":
									assert.Nil(t, fact.Quantity, "a missing token receipt is not a reported zero")
									assert.Equal(t, "unknown", fact.Source)
								}
							}
						}
						if strings.HasPrefix(tc.name, "speech") || strings.HasPrefix(tc.name, "transcription") {
							var evidence model.CreditUsageEvidence
							require.NoError(t, db.First(&evidence, request.UsageEvidenceID).Error)
							var receipt model.CreditEvidenceInput
							require.NoError(t, common.UnmarshalJsonStr(evidence.Payload, &receipt))
							require.NotNil(t, receipt.Consume)
							prompt, completion := 14, 31
							if tc.name == "speechheaderszero" {
								prompt, completion = 0, 0
							}
							assert.Equal(t, prompt, receipt.Consume.PromptTokens)
							assert.Equal(t, completion, receipt.Consume.CompletionTokens)
							assert.Equal(t, tc.name == "transcriptionstream" || tc.name == "speechstream", receipt.Consume.IsStream)
							fields := map[string]float64{"prompt_tokens": 14, "completion_tokens": 31, "audio_input_tokens": 4, "text_input_tokens": 10}
							if strings.HasPrefix(tc.name, "speech") {
								fields["audio_output_tokens"] = 31
							}
							for field, expected := range fields {
								if tc.name == "speechheaderszero" {
									expected = 0
								}
								index := slices.IndexFunc(receipt.Facts, func(fact hosttypes.UsageFact) bool { return fact.Field == field })
								require.NotEqual(t, -1, index, field)
								fact := receipt.Facts[index]
								require.NotNil(t, fact.Quantity)
								assert.Equal(t, expected, *fact.Quantity)
								assert.Equal(t, "upstream", fact.Source)
								assert.False(t, fact.Partial)
							}
						}
						packs, err := model.ListCreditPacks(db, u.Id, now)
						require.NoError(t, err)
						require.Len(t, packs, 1)
						assert.Zero(t, packs[0].Held)
						assert.Equal(t, tc.charged, packs[0].Spent)
						require.NoError(t, db.First(&key, key.Id).Error)
						assert.EqualValues(t, tc.charged, key.UsedQuota)
						assert.EqualValues(t, 100-tc.charged, key.RemainQuota)
					})
				}
			})
			t.Run("reported_zero_and_missing_usage_are_distinct", func(t *testing.T) {
				cases := []struct {
					name, receipt, state, expression string
					channel                          int
					response                         string
					converted                        bool
				}{
					{"reported", `,"usage":{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0}`, "settled", `tier("tokens", p * 2 + c * 4)`, constant.ChannelTypeOpenAI, "", false},
					{"missing", "", "review", `tier("tokens", p * 2 + c * 4)`, constant.ChannelTypeOpenAI, "", false},
					{"cacheunknown", `,"usage":{"prompt_tokens":12,"completion_tokens":0}`, "review", `tier("cache", cr * 1 + c * 4)`, constant.ChannelTypeOpenAI, "", false},
					{"cachezero", `,"usage":{"prompt_tokens":12,"completion_tokens":0,"prompt_tokens_details":{"cached_tokens":0}}`, "settled", `tier("cache", cr * 1 + c * 4)`, constant.ChannelTypeOpenAI, "", false},
					{"fixedzero", "", "settled", `tier("free", fixed(0))`, constant.ChannelTypeOpenAI, "", false},
					{"geminireported", "", "settled", `tier("tokens", p * 2 + c * 4)`, constant.ChannelTypeGemini, `{"candidates":[{"content":{"role":"model","parts":[{"text":""}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":0,"toolUsePromptTokenCount":0,"candidatesTokenCount":0,"thoughtsTokenCount":0,"totalTokenCount":0}}`, false},
					{"claudereported", "", "settled", `tier("tokens", p * 2 + c * 4)`, constant.ChannelTypeAnthropic, `{"id":"msg_zero","type":"message","role":"assistant","content":[],"stop_reason":"end_turn","usage":{"input_tokens":0,"output_tokens":0,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}`, false},
					{"claudecacheunknown", "", "review", `tier("tokens", p * 2 + c * 4)`, constant.ChannelTypeAnthropic, `{"id":"msg_unknown","type":"message","role":"assistant","content":[],"stop_reason":"end_turn","usage":{"input_tokens":0,"output_tokens":0}}`, false},
					{"rcreported", `,"usage":{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0}`, "settled", `tier("tokens", p * 2 + c * 4)`, constant.ChannelTypeAdvancedCustom, "", true},
					{"rcmissing", "", "review", `tier("tokens", p * 2 + c * 4)`, constant.ChannelTypeAdvancedCustom, "", true},
					{"rccacheunknown", `,"usage":{"prompt_tokens":12,"completion_tokens":0}`, "review", `tier("cache", cr * 1 + c * 4)`, constant.ChannelTypeAdvancedCustom, "", true},
					{"rccachezero", `,"usage":{"prompt_tokens":12,"completion_tokens":0,"prompt_tokens_details":{"cached_tokens":0}}`, "settled", `tier("cache", cr * 1 + c * 4)`, constant.ChannelTypeAdvancedCustom, "", true},
				}
				modes, expressions := make(map[string]string), make(map[string]string)
				for _, tc := range cases {
					name := "credit-zero-" + tc.name
					modes[name], expressions[name] = "tiered_expr", tc.expression
				}
				withTieredBillingConfig(t, modes, expressions)
				for _, tc := range cases {
					t.Run(tc.name, func(t *testing.T) {
						modelName := "credit-zero-" + tc.name
						server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
							w.Header().Set("Content-Type", "application/json")
							body := tc.response
							if body == "" {
								body = `{"model":` + fmt.Sprintf("%q", modelName) + `,"choices":[]` + tc.receipt + `}`
							}
							_, err := fmt.Fprint(w, body)
							assert.NoError(t, err)
						}))
						defer server.Close()
						ch := model.Channel{Name: "zero-" + tc.name, Type: tc.channel, Key: "test-key", Status: common.ChannelStatusEnabled, Group: "default", Models: modelName, BaseURL: &server.URL}
						if tc.converted {
							ch.SetOtherSettings(dto.ChannelOtherSettings{AdvancedCustom: &dto.AdvancedCustomConfig{Routes: []dto.AdvancedCustomRoute{{IncomingPath: "/v1/responses", UpstreamPath: "/v1/chat/completions", Converter: "openai_responses_to_openai_chat_completions"}}}})
						}
						require.NoError(t, db.Create(&ch).Error)
						require.NoError(t, db.Create(&model.Ability{ChannelId: ch.Id, Model: modelName, Group: "default", Enabled: true}).Error)
						u := model.User{Username: "zero-" + tc.name, Password: "unused", Status: common.UserStatusEnabled, Group: "default", AffCode: "zero-" + tc.name, AccountingVersion: 1, Setting: `{"billing_preference":"wallet_only"}`}
						require.NoError(t, db.Create(&u).Error)
						_, err := model.GrantCreditPack(db, model.CreditGrant{UserID: u.Id, SourceType: "test", SourceID: tc.name, Amount: 100, StartsAt: now, ExpiresAt: now + 3600, UseMask: model.CreditUseAPI}, now)
						require.NoError(t, err)
						key := model.Token{UserId: u.Id, Key: "zero" + tc.name + strings.Repeat("0", 48-len("zero"+tc.name)), Status: common.TokenStatusEnabled, RemainQuota: 100, ExpiredTime: -1, Group: "default"}
						require.NoError(t, db.Create(&key).Error)
						e := gin.New()
						e.Use(middleware.RequestId())
						e.POST("/v1/chat/completions", middleware.TokenAuth(), middleware.Distribute(), func(c *gin.Context) { Relay(c, types.RelayFormatOpenAI) })
						r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":`+fmt.Sprintf("%q", modelName)+`,"messages":[{"role":"user","content":"hi"}]}`))
						if tc.converted {
							e.POST("/v1/responses", middleware.TokenAuth(), middleware.Distribute(), func(c *gin.Context) { Relay(c, types.RelayFormatOpenAIResponses) })
							r = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":`+fmt.Sprintf("%q", modelName)+`,"input":"hi"}`))
						}
						r.Header.Set("Authorization", "Bearer sk-"+key.Key)
						r.Header.Set("Content-Type", "application/json")
						w := httptest.NewRecorder()
						e.ServeHTTP(w, r)
						require.Equal(t, http.StatusOK, w.Code, w.Body.String())
						var bill model.CreditRequest
						require.NoError(t, db.Where("user_id = ?", u.Id).First(&bill).Error)
						assert.Equal(t, tc.state, bill.State)
						assert.Zero(t, bill.Charged)
						assert.NotZero(t, bill.UsageEvidenceID)
						var receipt model.CreditUsageEvidence
						require.NoError(t, db.First(&receipt, bill.UsageEvidenceID).Error)
						var evidence model.CreditEvidenceInput
						require.NoError(t, common.UnmarshalJsonStr(receipt.Payload, &evidence))
						if tc.converted {
							for _, field := range []string{"prompt_tokens", "completion_tokens"} {
								index := slices.IndexFunc(evidence.Facts, func(fact hosttypes.UsageFact) bool { return fact.Field == field })
								require.NotEqual(t, -1, index)
								fact := evidence.Facts[index]
								source := "upstream"
								if tc.name == "rcmissing" {
									source = "estimate"
								}
								assert.Equal(t, source, fact.Source)
								require.NotNil(t, fact.Quantity)
								if tc.name == "rcreported" {
									assert.Zero(t, *fact.Quantity)
								}
								assert.False(t, fact.Partial)
								assert.NotZero(t, evidence.AttemptPriceEvidenceID)
							}
						}

						for _, fact := range evidence.Facts {
							if fact.Source == "upstream" {
								assert.Nil(t, fact.Estimation, "provider receipts never inherit a local counter descriptor")
							}
						}

					})
				}
			})
			t.Run("estimator_provenance_uses_count_time_model_and_settings", func(t *testing.T) {
				withTieredBillingConfig(t, map[string]string{"credit-estimator": "tiered_expr"}, map[string]string{"credit-estimator": `tier("free", fixed(0))`})
				for _, enabled := range []bool{true, false} {
					t.Run(fmt.Sprintf("enabled_%t", enabled), func(t *testing.T) {
						constant.CountToken = enabled
						t.Cleanup(func() { constant.CountToken = false })
						server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
							// Both missing usage quantities must retain separate
							// original-model and mapped-model counter provenance.
							w.Header().Set("Content-Type", "application/json")
							_, err := fmt.Fprint(w, `{"model":"claude-estimation-fixture","choices":[{"message":{"role":"assistant","content":"private output"}},{"message":{"role":"assistant","content":"private second output"}}]}`)
							assert.NoError(t, err)
						}))
						defer server.Close()
						ch := model.Channel{Name: fmt.Sprintf("estimator-%t", enabled), Type: constant.ChannelTypeOpenAI, Key: "test-key", Status: common.ChannelStatusEnabled, Group: "default", Models: "credit-estimator", BaseURL: &server.URL, ModelMapping: common.GetPointer(`{"credit-estimator":"claude-estimation-fixture"}`)}
						require.NoError(t, db.Create(&ch).Error)
						require.NoError(t, db.Create(&model.Ability{ChannelId: ch.Id, Model: "credit-estimator", Group: "default", Enabled: true}).Error)
						t.Cleanup(func() {
							require.NoError(t, db.Model(&model.Ability{}).Where("channel_id = ?", ch.Id).Update("enabled", false).Error)
						})
						u := model.User{Username: fmt.Sprintf("estimator-%t", enabled), Password: "unused", Status: common.UserStatusEnabled, Group: "default", AffCode: fmt.Sprintf("estimator-%t", enabled), AccountingVersion: 1, Setting: `{"billing_preference":"wallet_only"}`}
						require.NoError(t, db.Create(&u).Error)
						_, err := model.GrantCreditPack(db, model.CreditGrant{UserID: u.Id, SourceType: "test", SourceID: fmt.Sprintf("estimator-%t", enabled), Amount: 1000, StartsAt: now, ExpiresAt: now + 3600, UseMask: model.CreditUseAPI}, now)
						require.NoError(t, err)
						key := model.Token{UserId: u.Id, Key: fmt.Sprintf("estimator%t", enabled) + strings.Repeat("0", 34), Status: common.TokenStatusEnabled, UnlimitedQuota: true, ExpiredTime: -1, Group: "default"}
						require.NoError(t, db.Create(&key).Error)
						e := gin.New()
						e.Use(middleware.RequestId())
						e.POST("/v1/chat/completions", middleware.TokenAuth(), middleware.Distribute(), func(c *gin.Context) { Relay(c, types.RelayFormatOpenAI) })
						r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"credit-estimator","messages":[{"role":"user","content":"private prompt"}]}`))
						r.Header.Set("Authorization", "Bearer sk-"+key.Key)
						r.Header.Set("Content-Type", "application/json")
						w := httptest.NewRecorder()
						e.ServeHTTP(w, r)
						require.Equal(t, http.StatusOK, w.Code, w.Body.String())
						var bill model.CreditRequest
						require.NoError(t, db.Where("user_id = ?", u.Id).First(&bill).Error)
						assert.Equal(t, "settled", bill.State)
						var snapshot map[string]any
						require.NoError(t, common.UnmarshalJsonStr(bill.PriceSnapshot, &snapshot))
						frozen, ok := snapshot["prompt_estimation"].(map[string]any)
						require.True(t, ok, "price snapshot must preserve the actual request counter")
						assert.Equal(t, "credit-estimator", frozen["model"])
						settings, ok := frozen["settings"].(map[string]any)
						require.True(t, ok)
						assert.Equal(t, enabled, settings["count_token"])
						var receipt model.CreditUsageEvidence
						require.NoError(t, db.First(&receipt, bill.UsageEvidenceID).Error)
						var payload map[string]any
						require.NoError(t, common.UnmarshalJsonStr(receipt.Payload, &payload))
						facts, ok := payload["facts"].([]any)
						require.True(t, ok)
						for _, raw := range facts {
							fact := raw.(map[string]any)
							if fact["field"] != "prompt_tokens" && fact["field"] != "completion_tokens" {
								continue
							}
							assert.Equal(t, "estimate", fact["source"])
							estimation, ok := fact["estimation"].(map[string]any)
							require.True(t, ok, "every text estimate must identify its actual counter")
							assert.NotEmpty(t, estimation["version"])
							if fact["field"] == "prompt_tokens" {
								assert.Equal(t, frozen, estimation)
							} else {
								assert.Equal(t, "claude-estimation-fixture", estimation["model"])
								assert.Equal(t, "provider-heuristic", estimation["method"])
								params := estimation["parameters"].(map[string]any)
								assert.Equal(t, 1.13, params["word"])
								assert.Equal(t, float64(2), params["segments"])
							}
						}
						assert.NotContains(t, bill.PriceSnapshot+receipt.Payload, "private prompt")
						assert.NotContains(t, bill.PriceSnapshot+receipt.Payload, "private output")
						require.NoError(t, db.Model(&model.Ability{}).Where("channel_id = ?", ch.Id).Update("enabled", false).Error)
					})
				}
			})
			t.Run("retry_records_effective_price_without_overwriting_admission", func(t *testing.T) {
				u := model.User{Username: "retry-price", Password: "unused", Status: common.UserStatusEnabled, Group: "default", AffCode: "retry-price", AccountingVersion: 1}
				require.NoError(t, db.Create(&u).Error)
				key := model.Token{UserId: u.Id, Key: "retry-price" + strings.Repeat("0", 37), Status: common.TokenStatusEnabled, RemainQuota: 1000, ExpiredTime: -1}
				require.NoError(t, db.Create(&key).Error)
				_, err := model.GrantCreditPack(db, model.CreditGrant{UserID: u.Id, SourceType: "test", SourceID: "retry-price", Amount: 1000, StartsAt: now, ExpiresAt: now + 3600, UseMask: model.CreditUseAPI}, now)
				require.NoError(t, err)
				ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
				requestContext, cancel := context.WithCancel(context.Background())
				t.Cleanup(cancel)
				ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil).WithContext(requestContext)
				ctx.Set(string(constant.ContextKeyChannelId), channel.Id)
				ctx.Set(string(constant.ContextKeyChannelType), constant.ChannelTypeOpenAI)
				const expression = `tier("tokens", p * 2 + c * 4)`
				info := &relaycommon.RelayInfo{
					UserId: u.Id, TokenId: key.Id, RequestId: "retry-price", OriginModelName: "credit-retry-price", UsingGroup: "default", StartTime: time.Now(), RelayFormat: types.RelayFormatOpenAI,
					UserSetting:           dto.UserSetting{BillingPreference: "wallet_only"},
					ChannelMeta:           &relaycommon.ChannelMeta{ChannelId: channel.Id, ChannelType: constant.ChannelTypeOpenAI, ApiKey: "must-never-be-in-evidence", UpstreamModelName: "mapped-first"},
					PriceData:             hosttypes.PriceData{GroupRatioInfo: hosttypes.GroupRatioInfo{GroupRatio: 1}},
					TieredBillingSnapshot: &billingexpr.BillingSnapshot{BillingMode: "tiered_expr", ModelName: "credit-retry-price", ExprString: expression, ExprHash: billingexpr.ExprHashString(expression), ExprVersion: 1, GroupRatio: 1, EstimatedQuotaBeforeGroup: 20, EstimatedQuotaAfterGroup: 20, QuotaPerUnit: 500000},
				}
				require.Nil(t, service.PreConsumeBilling(ctx, 20, info))
				t.Cleanup(func() { info.Billing.Refund(ctx) })
				var original model.CreditRequest
				require.NoError(t, db.First(&original, service.CreditBillingRequestID(info)).Error)
				const failAttempt = "credit-attempt-price-write-failure"
				require.NoError(t, db.Callback().Create().Before("gorm:create").Register(failAttempt, func(tx *gorm.DB) {
					if tx.Statement.Schema != nil && tx.Statement.Schema.Table == "credit_usage_evidences" {
						tx.AddError(errors.New("attempt evidence storage unavailable"))
					}
				}))
				assert.Error(t, service.MarkBillingRequestSubmitted(info))
				require.NoError(t, db.Callback().Create().Remove(failAttempt))
				var rolledBack model.CreditRequest
				require.NoError(t, db.First(&rolledBack, original.ID).Error)
				assert.Zero(t, rolledBack.SubmittedAt, "price failure rolls back submission atomically")
				assert.Equal(t, "reserved", rolledBack.State)
				var receiptCount int64
				require.NoError(t, db.Model(&model.CreditUsageEvidence{}).Where("request_id = ?", original.ID).Count(&receiptCount).Error)
				assert.Zero(t, receiptCount)
				retryChannel := model.Channel{Name: "price-second", Type: constant.ChannelTypeOpenAI, Key: "unused", Status: common.ChannelStatusEnabled, Group: "second", Models: "credit-retry-price"}
				require.NoError(t, db.Create(&retryChannel).Error)
				var firstChannel model.Channel
				require.NoError(t, db.First(&firstChannel, channel.Id).Error)
				require.NoError(t, service.MarkBillingRequestSubmitted(info))
				require.NoError(t, service.MarkBillingRequestSubmitted(info), "host and adaptor may mark the same attempt")
				info.RetryIndex, info.UsingGroup, info.UpstreamModelName = 1, "second", "mapped-second"
				info.ChannelId = retryChannel.Id
				info.PriceData.GroupRatioInfo.GroupRatio = 2
				require.Nil(t, service.PrepareTieredBillingForSelectedGroup(ctx, info))
				previousUnit := common.QuotaPerUnit
				common.QuotaPerUnit = 5000000
				t.Cleanup(func() { common.QuotaPerUnit = previousUnit })
				require.NoError(t, service.MarkBillingRequestSubmitted(info))
				var attempts []model.CreditUsageEvidence
				require.NoError(t, db.Where("request_id = ? AND stage = ?", original.ID, "attempt").Order("attempt asc").Find(&attempts).Error)
				require.Len(t, attempts, 2, "one immutable effective price for each actual attempt")
				for i, evidence := range attempts {
					var receipt struct {
						AttemptPrice struct {
							ChannelID     int    `json:"channel_id"`
							Group         string `json:"group"`
							UpstreamModel string `json:"upstream_model"`
							Snapshot      string `json:"snapshot"`
						} `json:"attempt_price"`
					}
					require.NoError(t, common.UnmarshalJsonStr(evidence.Payload, &receipt))
					var price struct {
						QuotaPerUnit float64                     `json:"quota_per_unit"`
						Expression   billingexpr.BillingSnapshot `json:"expression"`
					}
					require.NoError(t, common.UnmarshalJsonStr(receipt.AttemptPrice.Snapshot, &price))
					assert.Equal(t, float64(i+1), price.Expression.GroupRatio)
					assert.Equal(t, float64(500000), price.QuotaPerUnit, "use the request's captured conversion")
					assert.Equal(t, expression, price.Expression.ExprString)
					assert.Equal(t, []string{"default", "second"}[i], receipt.AttemptPrice.Group)
					assert.Equal(t, []string{"mapped-first", "mapped-second"}[i], receipt.AttemptPrice.UpstreamModel)
					assert.Equal(t, []int{channel.Id, retryChannel.Id}[i], receipt.AttemptPrice.ChannelID)
					assert.NotContains(t, evidence.Payload, "must-never-be-in-evidence")
				}
				info.PriceData.GroupRatioInfo.GroupRatio = 3
				assert.ErrorIs(t, service.MarkBillingRequestSubmitted(info), model.ErrCreditOperationConflict, "same attempt cannot acquire another price")
				info.PriceData.GroupRatioInfo.GroupRatio = 2
				zeroImageCache := 0
				service.PostTextConsumeQuota(ctx, info, &dto.Usage{PromptTokens: 12, CompletionTokens: 8, TotalTokens: 20,
					PromptTokensDetails:    dto.InputTokenDetails{CachedTokens: 3, CachedTokensDetails: &dto.CachedTokenDetails{ImageTokens: &zeroImageCache}, CacheWriteTokens: 2, TextTokens: 6, AudioTokens: 2, ImageTokens: 4},
					CompletionTokenDetails: dto.OutputTokenDetails{TextTokens: 7, AudioTokens: 1, ReasoningTokens: 2},
				}, nil)
				var final model.CreditRequest
				require.NoError(t, db.First(&final, original.ID).Error)
				require.Equal(t, "settled", final.State)
				assert.Equal(t, original.PriceSnapshot, final.PriceSnapshot)
				assert.EqualValues(t, 56, final.Actual)
				assert.EqualValues(t, 56, final.Charged)
				var finalEvidence model.CreditUsageEvidence
				require.NoError(t, db.First(&finalEvidence, final.UsageEvidenceID).Error)
				var settled struct {
					AttemptPriceEvidenceID int64 `json:"attempt_price_evidence_id"`
				}
				require.NoError(t, common.UnmarshalJsonStr(finalEvidence.Payload, &settled))
				assert.Equal(t, attempts[1].ID, settled.AttemptPriceEvidenceID)
				var metering model.CreditEvidenceInput
				require.NoError(t, common.UnmarshalJsonStr(finalEvidence.Payload, &metering))
				metered := make(map[string]hosttypes.UsageFact)
				for _, fact := range metering.Facts {
					metered[fact.Field] = fact
				}
				for field, quantity := range map[string]float64{"cached_tokens": 3, "cache_creation_tokens": 2, "audio_input_tokens": 2, "image_input_tokens": 4, "text_input_tokens": 6, "audio_output_tokens": 1, "text_output_tokens": 7, "reasoning_tokens": 2, "image_cached_tokens": 0} {
					fact, exists := metered[field]
					require.True(t, exists, "normalized adaptor quantity must survive settlement: %s", field)
					require.NotNil(t, fact.Quantity)
					assert.Equal(t, quantity, *fact.Quantity)
					assert.Equal(t, "adaptor", fact.Source, "normalization alone cannot prove a raw upstream receipt")
					assert.Equal(t, "new-api-adaptor-normalized-v1", fact.Algorithm)
				}
				assert.Equal(t, "unknown", metered["audio_cached_tokens"].Source)
				assert.Nil(t, metered["audio_cached_tokens"].Quantity, "absent modality must not become a verified zero")
				var projected model.CreditLogOutbox
				require.NoError(t, db.Where("request_id = ?", final.ID).First(&projected).Error)
				var log model.Log
				require.NoError(t, common.UnmarshalJsonStr(projected.Payload, &log))
				assert.Equal(t, retryChannel.Id, log.ChannelId)
				assert.Equal(t, "second", log.Group)
				var unchanged model.Channel
				require.NoError(t, db.First(&unchanged, channel.Id).Error)
				assert.Equal(t, firstChannel.UsedQuota, unchanged.UsedQuota, "failed first attempt is not attributed the successful fee")
				require.NoError(t, db.First(&retryChannel, retryChannel.Id).Error)
				assert.EqualValues(t, 56, retryChannel.UsedQuota)
				require.NoError(t, db.Model(&model.CreditUsageEvidence{}).Where("id = ?", attempts[1].ID).Update("payload", strings.Replace(attempts[1].Payload, "mapped-second", "tampered", 1)).Error)
				_, err = model.GetCreditBillBalance(db, u.Id, final.ID)
				assert.ErrorIs(t, err, model.ErrCreditInvariant, "a bill must reject corrupted effective price evidence")
				brokenProof, err := model.ReconcileCreditAccount(db, u.Id)
				require.NoError(t, err)
				assert.Contains(t, brokenProof, model.CreditAccountDifference{Object: "request", ID: final.ID, Field: "usage_evidence_links", Expected: 1, Actual: 0})
				require.NoError(t, db.Model(&model.CreditUsageEvidence{}).Where("id = ?", attempts[1].ID).Update("payload", attempts[1].Payload).Error)
				admin := model.User{Username: "retry-price-admin", Password: "unused", Status: common.UserStatusEnabled, Role: common.RoleAdminUser, AffCode: "retry-price-admin"}
				require.NoError(t, db.Create(&admin).Error)
				quantity := float64(20)
				adjustment, err := model.AdjustCreditBill(db, model.CreditBillAdjustmentInput{UserID: u.Id, RequestID: final.ID, ActorID: admin.Id, EventID: "retry-price-adjust", ReferenceQuota: 40, EvidenceVersion: "upstream-correction-v1", Reason: "verified correction", Facts: []hosttypes.UsageFact{{Field: "prompt_tokens", Unit: "token", Quantity: &quantity, Source: "upstream"}}}, common.GetTimestamp())
				require.NoError(t, err)
				assert.EqualValues(t, 16, adjustment.Refunded)
				require.NoError(t, common.UnmarshalJsonStr(adjustment.LogPayload, &log))
				assert.Equal(t, retryChannel.Id, log.ChannelId)
				assert.Equal(t, "second", log.Group)
				require.NoError(t, db.First(&retryChannel, retryChannel.Id).Error)
				assert.EqualValues(t, 40, retryChannel.UsedQuota)
				require.NoError(t, db.First(&unchanged, channel.Id).Error)
				assert.Equal(t, firstChannel.UsedQuota, unchanged.UsedQuota)
				reconcile, err := model.ReconcileCreditAccount(db, u.Id)
				require.NoError(t, err)
				assert.Empty(t, reconcile)
			})

			t.Run("relay_boundary_evidence_failures_preserve_funds", func(t *testing.T) {
				for _, test := range []struct {
					name, failPhase string
					writeFailure    bool
					accepted        int
				}{
					{name: "success"},
					{name: "zero_write", writeFailure: true},
					{name: "partial_write", writeFailure: true, accepted: 3},
					{name: "possible_storage", failPhase: "client_write_possible"},
					{name: "accepted_storage", failPhase: "client_write_accepted"},
					{name: "response_storage", failPhase: "upstream_response"},
				} {
					t.Run(test.name, func(t *testing.T) {
						user := model.User{Username: "boundary-" + test.name, AffCode: "boundary-" + test.name, Group: "default", Status: common.UserStatusEnabled, AccountingVersion: 1}
						require.NoError(t, db.Create(&user).Error)
						key := model.Token{UserId: user.Id, Key: test.name + strings.Repeat("b", 48-len(test.name)), Status: common.TokenStatusEnabled, RemainQuota: 100, ExpiredTime: -1}
						require.NoError(t, db.Create(&key).Error)
						_, err := model.GrantCreditPack(db, model.CreditGrant{UserID: user.Id, SourceType: "test", SourceID: test.name, Amount: 100, StartsAt: now, ExpiresAt: now + 3600, UseMask: model.CreditUseAPI}, now)
						require.NoError(t, err)
						output := httptest.NewRecorder()
						ctx, _ := gin.CreateTestContext(output)
						ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
						ctx.Set(string(constant.ContextKeyChannelType), constant.ChannelTypeOpenAI)
						if test.writeFailure {
							ctx.Writer = creditBoundaryTestWriter{ResponseWriter: ctx.Writer, accepted: test.accepted}
						}
						info := &relaycommon.RelayInfo{UserId: user.Id, TokenId: key.Id, RequestId: "boundary-" + test.name, OriginModelName: "credit-boundary", UsingGroup: "default", StartTime: time.Now(), RelayFormat: types.RelayFormatOpenAI, UserSetting: dto.UserSetting{BillingPreference: "wallet_only"}, ChannelMeta: &relaycommon.ChannelMeta{ChannelId: channel.Id, ChannelType: constant.ChannelTypeOpenAI, UpstreamModelName: "credit-boundary"}, PriceData: hosttypes.PriceData{UsePrice: true, ModelPrice: .00002, Quota: 10, GroupRatioInfo: hosttypes.GroupRatioInfo{GroupRatio: 1}}}
						require.Nil(t, service.PreConsumeBilling(ctx, 20, info))
						t.Cleanup(func() { info.Billing.Refund(ctx) })
						require.NoError(t, service.MarkBillingRequestSubmitted(info))
						requestID := service.CreditBillingRequestID(info)
						if test.failPhase != "" {
							require.NoError(t, db.Callback().Create().Before("gorm:create").Register("credit:boundary-fault", func(tx *gorm.DB) {
								if row, ok := tx.Statement.Dest.(*model.CreditUsageEvidence); ok && row.Stage == "relay" {
									var recorded model.CreditEvidenceInput
									if common.UnmarshalJsonStr(row.Payload, &recorded) == nil && recorded.Observation.Phase == test.failPhase {
										tx.AddError(errors.New("private boundary storage detail"))
									}
								}
							}))
							defer func() { require.NoError(t, db.Callback().Create().Remove("credit:boundary-fault")) }()
						}
						status := http.StatusOK
						err = service.RecordCreditUpstreamResponse(info, &status, false)
						if test.failPhase == "upstream_response" {
							require.Error(t, err)
						} else {
							require.NoError(t, err)
							if !test.writeFailure {
								require.NoError(t, helper.PingData(ctx))
							}
							var count int64
							require.NoError(t, db.Model(&model.CreditUsageEvidence{}).Where("request_id = ? AND stage = ?", requestID, "relay").Count(&count).Error)
							assert.EqualValues(t, 1, count, "keepalive is not generated output")
							n, err := ctx.Writer.WriteString("private body")
							if test.writeFailure || test.failPhase != "" {
								require.Error(t, err)
							} else {
								require.NoError(t, err)
								assert.Equal(t, len("private body"), n)
							}
							if test.writeFailure {
								assert.Equal(t, test.accepted, n)
							}
							if test.failPhase == "client_write_possible" {
								assert.NotContains(t, output.Body.String(), "private body", "possible evidence must precede transport write")
							}
						}
						service.PostTextConsumeQuota(ctx, info, &dto.Usage{PromptTokens: 1, CompletionTokens: 2, TotalTokens: 3}, nil)
						var request model.CreditRequest
						require.NoError(t, db.First(&request, requestID).Error)
						var phases []model.CreditUsageEvidence
						require.NoError(t, db.Where("request_id = ? AND stage = ?", requestID, "relay").Order("sequence asc").Find(&phases).Error)
						if test.failPhase != "" {
							_, lateErr := ctx.Writer.WriteString("late private output")
							require.Error(t, lateErr, "a failed stage cannot allow buffered content to continue")
							assert.NotContains(t, output.Body.String(), "late private output")
							err := helper.StringData(ctx, "buffered output")
							require.Error(t, err, "Gin render errors must reach the streaming handler")
							assert.NotContains(t, output.Body.String(), "buffered output")
							var terminal *types.NewAPIError
							require.ErrorAs(t, err, &terminal)
							require.NoError(t, helper.StreamError(ctx, types.RelayFormatOpenAI, terminal))
							require.NoError(t, helper.StreamError(ctx, types.RelayFormatOpenAI, terminal))
							assert.Equal(t, 1, strings.Count(output.Body.String(), "relay_evidence_unavailable"), "send one protocol error despite repeated cleanup")
							assert.NotContains(t, output.Body.String(), "private boundary storage detail")
							assert.Equal(t, "review", request.State)
							assert.Empty(t, request.IntentKind)
							assert.Zero(t, request.UsageEvidenceID)
							assert.Zero(t, request.Charged)
							assert.Error(t, service.MarkBillingRequestSubmitted(info), "failed boundary cannot submit another attempt")
							var outboxCount int64
							var outbox model.CreditLogOutbox
							require.NoError(t, db.Model(&outbox).Where("request_id = ?", requestID).Count(&outboxCount).Error)
							assert.Zero(t, outboxCount)
						} else {
							assert.Equal(t, "settled", request.State)
							assert.EqualValues(t, 10, request.Charged)
							expectedPhases := 3
							if test.writeFailure && test.accepted == 0 {
								expectedPhases = 2
							}
							require.Len(t, phases, expectedPhases)
							if test.writeFailure && test.accepted > 0 {
								var accepted model.CreditEvidenceInput
								require.NoError(t, common.UnmarshalJsonStr(phases[2].Payload, &accepted))
								assert.EqualValues(t, 3, *accepted.Observation.Bytes)
							}
						}
						for _, row := range phases {
							assert.NotContains(t, row.Payload, "private body")
							assert.NotContains(t, row.Payload, "private boundary storage detail")
						}
						packs, err := model.ListCreditPacks(db, user.Id, now)
						require.NoError(t, err)
						if test.failPhase != "" {
							assert.EqualValues(t, 20, packs[0].Held)
							assert.Zero(t, packs[0].Spent)
						} else {
							assert.Zero(t, packs[0].Held)
							assert.EqualValues(t, 10, packs[0].Spent)
						}
						balances, err := model.ReconcileCreditAccount(db, user.Id)
						require.NoError(t, err)
						assert.Empty(t, balances)
					})
				}
			})

			t.Run("image_stream_budget_preserves_native_quantity", func(t *testing.T) {
				for index, tc := range []struct {
					name                                string
					amount, actual                      int64
					tokens, stop, storage, json, legacy bool
				}{
					{name: "count_stop", amount: 15, actual: 20, stop: true},
					{name: "count_legacy", amount: 15, actual: 20, stop: true, legacy: true},
					{name: "count_healthy", amount: 50, actual: 30},
					{name: "token_stop", amount: 50, actual: 60, tokens: true, stop: true},
					{name: "token_healthy", amount: 100, actual: 60, tokens: true},
					{name: "storage", amount: 50, storage: true},
					{name: "json_stop", amount: 15, actual: 30, stop: true, json: true},
				} {
					t.Run(tc.name, func(t *testing.T) {
						name := "credit-imagebudget-" + tc.name
						expression := `tier("image", fixed(0.00002)) * image_count`
						if tc.tokens {
							expression = `tier("tokens", c * 4)`
						}
						mode := "tiered_expr"
						if tc.legacy {
							mode = "ratio"
							oldPrice := ratio_setting.ModelPrice2JSONString()
							t.Cleanup(func() { require.NoError(t, ratio_setting.UpdateModelPriceByJSONString(oldPrice)) })
							require.NoError(t, ratio_setting.UpdateModelPriceByJSONString(fmt.Sprintf(`{%q:0.00002}`, name)))
						}
						withTieredBillingConfig(t, map[string]string{name: mode}, map[string]string{name: expression})
						var calls atomic.Int32
						upstreamClosed := make(chan struct{})
						server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
							calls.Add(1)
							assert.Equal(t, "/v1/images/generations", r.URL.Path)
							if tc.json {
								w.Header().Set("Content-Type", "application/json")
								_, err := fmt.Fprint(w, `{"data":[{"url":"allowed"},{"url":"denied"},{"url":"unobserved tail"}]}`)
								assert.NoError(t, err)
								return
							}
							w.Header().Set("Content-Type", "text/event-stream")
							for i, content := range []string{"allowed", "denied", "unobserved tail"} {
								quantity := 5
								if i > 0 {
									quantity = 30
								}
								_, err := fmt.Fprintf(w, "data: {\"type\":\"image_generation.completed\",\"url\":%q,\"usage\":{\"input_tokens\":0,\"output_tokens\":%d,\"total_tokens\":%d}}\n\n", content, quantity, quantity)
								assert.NoError(t, err)
								if i == 1 && (tc.name == "count_stop" || tc.name == "token_stop") {
									w.(http.Flusher).Flush()
									<-r.Context().Done()
									close(upstreamClosed)
									return
								}
							}
							_, err := fmt.Fprint(w, "data: [DONE]\n\n")
							assert.NoError(t, err)
						}))
						defer server.Close()
						channel := model.Channel{Name: name, Type: constant.ChannelTypeOpenAI, Key: "test-key", Status: common.ChannelStatusEnabled, Group: "default", Models: name, BaseURL: &server.URL}
						require.NoError(t, db.Create(&channel).Error)
						require.NoError(t, db.Create(&model.Ability{ChannelId: channel.Id, Model: name, Group: "default", Enabled: true}).Error)
						user := model.User{Username: fmt.Sprintf("imagebudget%d", index), Password: "unused", Status: common.UserStatusEnabled, Group: "default", AffCode: fmt.Sprintf("imagebudget%d", index), AccountingVersion: 1, Setting: `{"billing_preference":"wallet_only"}`}
						require.NoError(t, db.Create(&user).Error)
						_, err := model.GrantCreditPack(db, model.CreditGrant{UserID: user.Id, SourceType: "test", SourceID: name, Amount: tc.amount, StartsAt: now, ExpiresAt: now + 3600, UseMask: model.CreditUseAPI}, now)
						require.NoError(t, err)
						key := model.Token{UserId: user.Id, Key: fmt.Sprintf("imagebudget%d", index) + strings.Repeat("0", 36), Status: common.TokenStatusEnabled, RemainQuota: int(tc.amount), ExpiredTime: -1, Group: "default"}
						require.NoError(t, db.Create(&key).Error)
						if tc.storage {
							require.NoError(t, db.Callback().Update().Register("credit:image-budget-storage", func(tx *gorm.DB) {
								if tx.Statement.Schema == nil || tx.Statement.Schema.Name != "Token" {
									return
								}
								var pending model.CreditRequest
								result := tx.Session(&gorm.Session{NewDB: true}).Where("user_id = ? AND submitted_at > 0", user.Id).Limit(1).Find(&pending)
								if result.Error == nil && result.RowsAffected > 0 {
									tx.AddError(errors.New("fixture private image budget detail"))
								}
							}))
							defer func() { require.NoError(t, db.Callback().Update().Remove("credit:image-budget-storage")) }()
						}
						engine := gin.New()
						engine.Use(middleware.RequestId())
						engine.POST("/v1/images/generations", middleware.TokenAuth(), middleware.Distribute(), func(c *gin.Context) { Relay(c, types.RelayFormatOpenAIImage) })
						request := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(fmt.Sprintf(`{"model":%q,"prompt":"hi","n":1,"stream":true}`, name)))
						request.Header.Set("Authorization", "Bearer sk-"+key.Key)
						request.Header.Set("Content-Type", "application/json")
						response := httptest.NewRecorder()
						engine.ServeHTTP(response, request)
						if tc.name == "count_stop" || tc.name == "token_stop" {
							<-upstreamClosed
						}
						assert.EqualValues(t, 1, calls.Load())
						var bill model.CreditRequest
						require.NoError(t, db.Where("user_id = ?", user.Id).First(&bill).Error)
						if tc.storage {
							assert.Contains(t, response.Body.String(), "stream_budget_unavailable")
							assert.NotContains(t, response.Body.String(), "private image budget detail")
							assert.Equal(t, "review", bill.State)
							assert.Empty(t, bill.IntentKind)
							assert.Zero(t, bill.UsageEvidenceID)
							assert.Zero(t, bill.Charged)
						} else {
							assert.Equal(t, "settled", bill.State)
							assert.Equal(t, tc.actual, bill.Actual)
							assert.Equal(t, min(tc.amount, tc.actual), bill.Charged)
							assert.Equal(t, max(int64(0), tc.actual-tc.amount), bill.Uncollected)
						}
						if tc.stop || tc.storage {
							assert.NotContains(t, response.Body.String(), "denied")
							assert.NotContains(t, response.Body.String(), "unobserved tail")
							assert.NotContains(t, response.Body.String(), "[DONE]")
							if tc.stop {
								assert.Contains(t, response.Body.String(), "quota_budget_exhausted")
							}
						} else {
							assert.Contains(t, response.Body.String(), "unobserved tail")
							assert.Contains(t, response.Body.String(), "[DONE]")
						}
						require.NoError(t, db.First(&key, key.Id).Error)
						expectedUsed := bill.Charged
						if tc.storage {
							expectedUsed = bill.Reserved
						}
						assert.EqualValues(t, expectedUsed, key.UsedQuota)
						assert.EqualValues(t, tc.amount-expectedUsed, key.RemainQuota)
						differences, err := model.ReconcileCreditAccount(db, user.Id)
						require.NoError(t, err)
						assert.Empty(t, differences)
					})
				}
			})

			t.Run("stream_budget_stops_output_without_user_debt", func(t *testing.T) {
				for index, test := range []struct {
					name, expression, denied, converter, audio string
					phaseFailure                               string
					usage, stop, storageFailure                bool
					legacy                                     bool
					amount                                     int64
					format                                     types.RelayFormat
					channel                                    int
					waitUpstreamClose                          bool
				}{
					{name: "reported", usage: true, stop: true},
					{name: "estimated", denied: strings.Repeat("denied ", 100), stop: true},
					{name: "storage_failure", usage: true, stop: true, storageFailure: true},
					{name: "healthy_tokens", usage: true, amount: 200},
					{name: "request_price", expression: `tier("request", fixed(0.00002))`, usage: true},
					{name: "phase_possible", phaseFailure: "client_write_possible", denied: strings.Repeat("denied ", 100)},
					{name: "phase_accepted", phaseFailure: "client_write_accepted", denied: strings.Repeat("denied ", 100)},
					{name: "responses_estimated", denied: strings.Repeat("denied ", 100), stop: true, format: types.RelayFormatOpenAIResponses},
					{name: "responses_storage_failure", denied: strings.Repeat("denied ", 100), stop: true, storageFailure: true, format: types.RelayFormatOpenAIResponses},
					{name: "responses_healthy", denied: strings.Repeat("denied ", 100), amount: 1000, format: types.RelayFormatOpenAIResponses},
					{name: "responses_request_price", expression: `tier("request", fixed(0.00002))`, format: types.RelayFormatOpenAIResponses},
					{name: "chat_responses_estimated", denied: strings.Repeat("denied ", 100), stop: true, converter: "openai_chat_completions_to_openai_responses"},
					{name: "chat_responses_healthy", denied: strings.Repeat("denied ", 100), amount: 1000, converter: "openai_chat_completions_to_openai_responses"},
					{name: "chat_responses_storage", denied: strings.Repeat("denied ", 100), stop: true, storageFailure: true, converter: "openai_chat_completions_to_openai_responses"},
					{name: "responses_chat_estimated", denied: strings.Repeat("denied ", 100), stop: true, format: types.RelayFormatOpenAIResponses, converter: "openai_responses_to_openai_chat_completions"},
					{name: "responses_chat_healthy", usage: true, denied: strings.Repeat("denied ", 100), amount: 1000, format: types.RelayFormatOpenAIResponses, converter: "openai_responses_to_openai_chat_completions"},
					{name: "responses_chat_storage", denied: strings.Repeat("denied ", 100), stop: true, storageFailure: true, format: types.RelayFormatOpenAIResponses, converter: "openai_responses_to_openai_chat_completions"},
					{name: "speech_estimated", denied: base64.StdEncoding.EncodeToString(make([]byte, 30000)), stop: true, audio: "speech", waitUpstreamClose: true},
					{name: "speech_legacy", denied: base64.StdEncoding.EncodeToString(make([]byte, 30000)), stop: true, audio: "speech", legacy: true},
					{name: "speech_storage", denied: base64.StdEncoding.EncodeToString(make([]byte, 30000)), stop: true, storageFailure: true, audio: "speech"},
					{name: "speech_healthy", denied: base64.StdEncoding.EncodeToString(make([]byte, 30000)), amount: 1000, audio: "speech"},
					{name: "transcription_estimated", denied: strings.Repeat("denied ", 100), stop: true, audio: "transcription", waitUpstreamClose: true},
					{name: "transcription_storage", denied: strings.Repeat("denied ", 100), stop: true, storageFailure: true, audio: "transcription"},
					{name: "transcription_healthy", denied: strings.Repeat("denied ", 100), amount: 1000, audio: "transcription"},
					{name: "gemini_chat_estimated", denied: strings.Repeat("denied ", 100), stop: true, channel: constant.ChannelTypeGemini},
					{name: "gemini_native_estimated", denied: strings.Repeat("denied ", 100), stop: true, channel: constant.ChannelTypeGemini, format: types.RelayFormatGemini},
					{name: "gemini_responses_estimated", denied: strings.Repeat("denied ", 100), stop: true, channel: constant.ChannelTypeGemini, format: types.RelayFormatOpenAIResponses, waitUpstreamClose: true},
					{name: "claude_chat_estimated", denied: strings.Repeat("denied ", 100), stop: true, channel: constant.ChannelTypeAnthropic},
					{name: "claude_native_estimated", denied: strings.Repeat("denied ", 100), stop: true, channel: constant.ChannelTypeAnthropic, format: types.RelayFormatClaude},
					{name: "claude_responses_estimated", denied: strings.Repeat("denied ", 100), stop: true, channel: constant.ChannelTypeAnthropic, format: types.RelayFormatOpenAIResponses},
				} {
					if test.expression == "" {
						test.expression = `tier("tokens", c * 4)`
					}
					if test.denied == "" {
						test.denied = "denied"
					}
					if test.amount == 0 {
						test.amount = 50
					}
					if test.format == "" {
						test.format = types.RelayFormatOpenAI
					}
					if test.channel == 0 {
						test.channel = constant.ChannelTypeOpenAI
					}
					t.Run(test.name, func(t *testing.T) {
						modelName := "credit-budget-" + test.name
						mode := "tiered_expr"
						if test.legacy {
							mode = "ratio"
							oldModel, oldAudio, oldCompletion := ratio_setting.ModelRatio2JSONString(), ratio_setting.AudioRatio2JSONString(), ratio_setting.AudioCompletionRatio2JSONString()
							t.Cleanup(func() {
								require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(oldModel))
								require.NoError(t, ratio_setting.UpdateAudioRatioByJSONString(oldAudio))
								require.NoError(t, ratio_setting.UpdateAudioCompletionRatioByJSONString(oldCompletion))
							})
							require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(fmt.Sprintf(`{%q:1}`, modelName)))
							require.NoError(t, ratio_setting.UpdateAudioRatioByJSONString(fmt.Sprintf(`{%q:2}`, modelName)))
							require.NoError(t, ratio_setting.UpdateAudioCompletionRatioByJSONString(fmt.Sprintf(`{%q:3}`, modelName)))
						}
						withTieredBillingConfig(t, map[string]string{modelName: mode}, map[string]string{modelName: test.expression})
						var calls atomic.Int32
						upstreamClosed := make(chan struct{})
						firstUsage, secondUsage := "", ""
						if test.usage {
							firstUsage = `,"usage":{"prompt_tokens":0,"completion_tokens":5,"total_tokens":5}`
							secondUsage = `,"usage":{"prompt_tokens":0,"completion_tokens":30,"total_tokens":30}`
						}
						server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
							calls.Add(1)
							w.Header().Set("Content-Type", "text/event-stream")
							body := `data: {"choices":[{"index":0,"delta":{"content":"allowed "}}]` + firstUsage + "}\n\n" + `data: {"choices":[{"index":0,"delta":{"content":` + fmt.Sprintf("%q", test.denied) + `}}]` + secondUsage + "}\n\n" + `data: {"choices":[{"index":0,"delta":{"content":"unobserved tail"},"finish_reason":"stop"}]}` + "\n\ndata: [DONE]\n\n"
							if test.converter == "openai_responses_to_openai_chat_completions" && test.usage {
								body = strings.Replace(body, `"finish_reason":"stop"}]}`, `"finish_reason":"stop"}]`+secondUsage+`}`, 1)
							}

							if test.format == types.RelayFormatOpenAIResponses && test.converter == "" || test.converter == "openai_chat_completions_to_openai_responses" {
								body = `data: {"type":"response.created","response":{"id":"resp_budget","model":` + fmt.Sprintf("%q", modelName) + `,"status":"in_progress","usage":{"input_tokens":0,"output_tokens":0,"total_tokens":0}}}` + "\n\n" +
									`data: {"type":"response.output_text.delta","delta":"allowed "}` + "\n\n" +
									`data: {"type":"response.output_text.delta","delta":` + fmt.Sprintf("%q", test.denied) + `}` + "\n\n" +
									`data: {"type":"response.output_text.delta","delta":"unobserved tail"}` + "\n\n" +
									`data: {"type":"response.completed","response":{"id":"resp_budget","status":"completed","usage":{"input_tokens":0,"output_tokens":150,"total_tokens":150}}}` + "\n\n"
							}
							if test.channel == constant.ChannelTypeAnthropic {
								body = `data: {"type":"message_start","message":{"id":"msg_budget","model":` + fmt.Sprintf("%q", modelName) + `,"usage":{"input_tokens":0,"output_tokens":0,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}}` + "\n\n" +
									`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n" +
									`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"allowed "}}` + "\n\n" +
									`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":` + fmt.Sprintf("%q", test.denied) + `}}` + "\n\n" +
									`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"unobserved tail"}}` + "\n\n" +
									`data: {"type":"content_block_stop","index":0}` + "\n\n" +
									`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":150}}` + "\n\ndata: {\"type\":\"message_stop\"}\n\n"
							}
							if test.channel == constant.ChannelTypeGemini {
								body = `data: {"candidates":[{"index":0,"content":{"role":"model","parts":[{"text":"allowed "}]}}],"usageMetadata":{"promptTokenCount":0,"toolUsePromptTokenCount":0,"candidatesTokenCount":0,"thoughtsTokenCount":0,"totalTokenCount":0}}` + "\n\n" +
									`data: {"candidates":[{"index":0,"content":{"role":"model","parts":[{"text":` + fmt.Sprintf("%q", test.denied) + `}]}}]}` + "\n\n" +
									`data: {"candidates":[{"index":0,"content":{"role":"model","parts":[{"text":"unobserved tail"}]}}]}` + "\n\n" +
									`data: {"candidates":[{"index":0,"finishReason":"STOP","content":{"role":"model","parts":[]}}],"usageMetadata":{"promptTokenCount":0,"toolUsePromptTokenCount":0,"candidatesTokenCount":150,"thoughtsTokenCount":0,"totalTokenCount":150}}` + "\n\n"
							}
							if test.audio == "speech" {
								body = `data: {"type":"speech.audio.delta","audio":"AA==","usage":{"input_tokens":0,"output_tokens":0,"total_tokens":0,"input_token_details":{"audio_tokens":0,"text_tokens":0}}}` + "\n\n" +
									`data: {"type":"speech.audio.delta","audio":` + fmt.Sprintf("%q", test.denied) + `}` + "\n\n" +
									`data: {"type":"speech.audio.done","usage":{"input_tokens":0,"output_tokens":31,"total_tokens":31,"input_token_details":{"audio_tokens":0,"text_tokens":0}}}` + "\n\n"
							}
							if test.audio == "transcription" {
								body = `data: {"type":"transcript.text.delta","delta":"allowed "}` + "\n\n" +
									`data: {"type":"transcript.text.delta","delta":` + fmt.Sprintf("%q", test.denied) + `}` + "\n\n" +
									`data: {"type":"transcript.text.done","text":"unobserved tail","usage":{"type":"tokens","input_tokens":0,"output_tokens":31,"total_tokens":31,"input_token_details":{"audio_tokens":0,"text_tokens":0}}}` + "\n\n"
							}
							if test.waitUpstreamClose {
								frames := strings.Split(body, "\n\n")
								body = strings.Join(frames[:2], "\n\n") + "\n\n"
							}
							if test.legacy {
								require.NoError(t, ratio_setting.UpdateAudioRatioByJSONString(fmt.Sprintf(`{%q:20}`, modelName)))
								require.NoError(t, ratio_setting.UpdateAudioCompletionRatioByJSONString(fmt.Sprintf(`{%q:30}`, modelName)))
							}
							_, err := fmt.Fprint(w, body)
							assert.NoError(t, err)
							if test.waitUpstreamClose {
								w.(http.Flusher).Flush()
								<-r.Context().Done()
								close(upstreamClosed)
							}
						}))
						defer server.Close()
						channel := model.Channel{Name: modelName, Type: test.channel, Key: "test-key", Status: common.ChannelStatusEnabled, Group: "default", Models: modelName, BaseURL: &server.URL}
						if test.converter != "" {
							channel.Type = constant.ChannelTypeAdvancedCustom
							incoming, outgoing := "/v1/chat/completions", "/v1/responses"
							if test.format == types.RelayFormatOpenAIResponses {
								incoming, outgoing = outgoing, incoming
							}
							channel.SetOtherSettings(dto.ChannelOtherSettings{AdvancedCustom: &dto.AdvancedCustomConfig{Routes: []dto.AdvancedCustomRoute{{IncomingPath: incoming, UpstreamPath: outgoing, Converter: test.converter}}}})
						}
						require.NoError(t, db.Create(&channel).Error)
						require.NoError(t, db.Create(&model.Ability{ChannelId: channel.Id, Model: modelName, Group: "default", Enabled: true}).Error)
						user := model.User{Username: fmt.Sprintf("sbudget%02d", index), Password: "unused", Status: common.UserStatusEnabled, Group: "default", AffCode: fmt.Sprintf("sbudget%02d", index), AccountingVersion: 1, Setting: `{"billing_preference":"wallet_only"}`}
						require.NoError(t, db.Create(&user).Error)
						_, err := model.GrantCreditPack(db, model.CreditGrant{UserID: user.Id, SourceType: "test", SourceID: test.name, Amount: test.amount, StartsAt: now, ExpiresAt: now + 3600, UseMask: model.CreditUseAPI}, now)
						require.NoError(t, err)
						key := model.Token{UserId: user.Id, Key: "budget" + test.name + strings.Repeat("0", 48-len("budget"+test.name)), Status: common.TokenStatusEnabled, RemainQuota: int(test.amount), ExpiredTime: -1, Group: "default"}
						require.NoError(t, db.Create(&key).Error)
						if test.phaseFailure != "" {
							require.NoError(t, db.Callback().Create().Before("gorm:create").Register("credit:stream-phase-storage", func(tx *gorm.DB) {
								if row, ok := tx.Statement.Dest.(*model.CreditUsageEvidence); ok && row.Stage == "relay" {
									var input model.CreditEvidenceInput
									if common.UnmarshalJsonStr(row.Payload, &input) == nil && input.Observation.Phase == test.phaseFailure {
										tx.AddError(errors.New("fixture private phase detail"))
									}
								}
							}))
							defer func() { require.NoError(t, db.Callback().Create().Remove("credit:stream-phase-storage")) }()
						} else if test.storageFailure {
							require.NoError(t, db.Callback().Update().Register("credit:stream-budget-storage", func(tx *gorm.DB) {
								if tx.Statement.Schema == nil || tx.Statement.Schema.Name != "Token" {
									return
								}
								var admitted model.CreditRequest
								result := tx.Session(&gorm.Session{NewDB: true}).Where("user_id = ? AND submitted_at > 0", user.Id).Limit(1).Find(&admitted)
								if result.Error == nil && result.RowsAffected > 0 {
									tx.AddError(errors.New("fixture private storage detail"))
								}
							}))
							t.Cleanup(func() { require.NoError(t, db.Callback().Update().Remove("credit:stream-budget-storage")) })
						}
						engine := gin.New()
						engine.Use(middleware.RequestId())
						engine.POST("/v1/chat/completions", middleware.TokenAuth(), middleware.Distribute(), func(c *gin.Context) { Relay(c, types.RelayFormatOpenAI) })
						request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":`+fmt.Sprintf("%q", modelName)+`,"stream":true,"messages":[{"role":"user","content":"hi"}]}`))
						if test.format == types.RelayFormatOpenAIResponses {
							engine.POST("/v1/responses", middleware.TokenAuth(), middleware.Distribute(), func(c *gin.Context) { Relay(c, types.RelayFormatOpenAIResponses) })
							request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":`+fmt.Sprintf("%q", modelName)+`,"stream":true,"input":"hi"}`))
						}
						if test.format == types.RelayFormatClaude {
							engine.POST("/v1/messages", middleware.TokenAuth(), middleware.Distribute(), func(c *gin.Context) { Relay(c, types.RelayFormatClaude) })
							request = httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":`+fmt.Sprintf("%q", modelName)+`,"stream":true,"max_tokens":1024,"messages":[{"role":"user","content":"hi"}]}`))
						}
						if test.format == types.RelayFormatGemini {
							path := "/v1beta/models/" + modelName + ":streamGenerateContent"
							engine.POST("/v1beta/models/*model", middleware.TokenAuth(), middleware.Distribute(), func(c *gin.Context) { Relay(c, types.RelayFormatGemini) })
							request = httptest.NewRequest(http.MethodPost, path+"?alt=sse", strings.NewReader(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`))
						}
						if test.audio == "speech" {
							engine.POST("/v1/audio/speech", middleware.TokenAuth(), middleware.Distribute(), func(c *gin.Context) { Relay(c, types.RelayFormatOpenAIAudio) })
							request = httptest.NewRequest(http.MethodPost, "/v1/audio/speech", strings.NewReader(`{"model":`+fmt.Sprintf("%q", modelName)+`,"input":"hi","voice":"alloy","stream_format":"sse"}`))
						}
						if test.audio == "transcription" {
							engine.POST("/v1/audio/transcriptions", middleware.TokenAuth(), middleware.Distribute(), func(c *gin.Context) { Relay(c, types.RelayFormatOpenAIAudio) })
							var body bytes.Buffer
							form := multipart.NewWriter(&body)
							require.NoError(t, form.WriteField("model", modelName))
							require.NoError(t, form.WriteField("response_format", "json"))
							require.NoError(t, form.WriteField("stream", "true"))
							file, err := form.CreateFormFile("file", "input.wav")
							require.NoError(t, err)
							var wav bytes.Buffer
							wav.WriteString("RIFF")
							require.NoError(t, binary.Write(&wav, binary.LittleEndian, uint32(16036)))
							wav.WriteString("WAVEfmt ")
							for _, value := range []any{uint32(16), uint16(1), uint16(1), uint32(8000), uint32(16000), uint16(2), uint16(16)} {
								require.NoError(t, binary.Write(&wav, binary.LittleEndian, value))
							}
							wav.WriteString("data")
							require.NoError(t, binary.Write(&wav, binary.LittleEndian, uint32(16000)))
							wav.Write(make([]byte, 16000))
							_, err = file.Write(wav.Bytes())
							require.NoError(t, err)
							require.NoError(t, form.Close())
							request = httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", &body)
							request.Header.Set("Content-Type", form.FormDataContentType())
						}
						request.Header.Set("Authorization", "Bearer sk-"+key.Key)
						if test.audio != "transcription" {
							request.Header.Set("Content-Type", "application/json")
						}
						response := httptest.NewRecorder()
						engine.ServeHTTP(response, request)
						if test.phaseFailure != "" {
							assert.EqualValues(t, 1, calls.Load(), "stage failure cannot replay the upstream request")
							assert.NotContains(t, response.Body.String(), "denied")
							assert.NotContains(t, response.Body.String(), "unobserved tail")
							assert.NotContains(t, response.Body.String(), "[DONE]")
							assert.NotContains(t, response.Body.String(), "private phase detail")
							assert.Contains(t, response.Body.String(), "relay evidence cannot be recorded")
							for frame := range strings.SplitSeq(response.Body.String(), "\n\n") {
								if data, exists := strings.CutPrefix(frame, "data: "); exists {
									var event map[string]any
									require.NoError(t, common.UnmarshalJsonStr(data, &event), "each emitted data frame remains valid JSON")
								}
							}
							var pending model.CreditRequest
							require.NoError(t, db.Where("user_id = ?", user.Id).First(&pending).Error)
							assert.Equal(t, "review", pending.State)
							assert.Empty(t, pending.IntentKind)
							assert.Zero(t, pending.UsageEvidenceID)
							assert.Zero(t, pending.Charged)
							require.NoError(t, db.First(&key, key.Id).Error)
							assert.EqualValues(t, test.amount-pending.Reserved, key.RemainQuota)
							assert.EqualValues(t, pending.Reserved, key.UsedQuota)
							differences, err := model.ReconcileCreditAccount(db, user.Id)
							require.NoError(t, err)
							assert.Empty(t, differences)
							return
						}
						if test.waitUpstreamClose {
							<-upstreamClosed
							if test.format == types.RelayFormatOpenAIResponses {
								assert.Equal(t, 1, strings.Count(response.Body.String(), "event: error\n"))
							}
							assert.NotContains(t, response.Body.String(), "event: response.failed", "a local budget stop must not also be reported as an upstream protocol failure")
						}
						if test.storageFailure {
							status := http.StatusInternalServerError
							if test.format == types.RelayFormatOpenAIResponses && test.converter == "" || test.converter == "openai_chat_completions_to_openai_responses" {
								status = http.StatusOK // the created event has already been sent
							}
							if test.format == types.RelayFormatOpenAIResponses {
								assert.Contains(t, response.Body.String(), "event: error\n")
								assert.Contains(t, response.Body.String(), `"type":"error"`)
								assert.NotContains(t, response.Body.String(), "event: response.completed")
							}
							require.Equal(t, status, response.Code, response.Body.String())
							assert.True(t, strings.HasPrefix(response.Body.String(), "data: ") || test.format == types.RelayFormatOpenAIResponses && strings.HasPrefix(response.Body.String(), "event: "), "an SSE response cannot contain a bare JSON body")
							assert.Contains(t, response.Body.String(), "stream_budget_unavailable")
							assert.NotContains(t, response.Body.String(), "private storage detail")
							assert.NotContains(t, response.Body.String(), "[DONE]")
							assert.NotContains(t, response.Body.String(), "denied")
							var pending model.CreditRequest
							require.NoError(t, db.Where("user_id = ?", user.Id).First(&pending).Error)
							assert.Equal(t, "review", pending.State)
							assert.Empty(t, pending.IntentKind)
							assert.Zero(t, pending.UsageEvidenceID)
							assert.Zero(t, pending.Charged)
							require.NoError(t, db.First(&key, key.Id).Error)
							assert.EqualValues(t, test.amount-pending.Reserved, key.RemainQuota)
							assert.EqualValues(t, pending.Reserved, key.UsedQuota)
							require.NoError(t, db.Callback().Update().Remove("credit:stream-budget-storage"))
							return
						}
						require.Equal(t, http.StatusOK, response.Code, response.Body.String())
						assert.EqualValues(t, 1, calls.Load(), "a budget stop never replays generation")
						var bill model.CreditRequest
						require.NoError(t, db.Where("user_id = ?", user.Id).First(&bill).Error)
						require.Equal(t, "settled", bill.State)
						if test.stop {
							if test.audio == "speech" {
								assert.Contains(t, response.Body.String(), "AA==")
							} else {
								assert.Contains(t, response.Body.String(), "allowed ")
							}
							assert.NotContains(t, response.Body.String(), test.denied)
							assert.NotContains(t, response.Body.String(), "unobserved tail")
							if test.format == types.RelayFormatOpenAIResponses {
								assert.Contains(t, response.Body.String(), "event: error\n")
								assert.Contains(t, response.Body.String(), `"type":"error"`)
								assert.NotContains(t, response.Body.String(), "event: response.completed")
							}
							if test.format == types.RelayFormatClaude {
								assert.Contains(t, response.Body.String(), "event: error\n")
								assert.Contains(t, response.Body.String(), "stream budget cannot be extended")
								assert.NotContains(t, response.Body.String(), "event: message_stop")
							} else {
								assert.Contains(t, response.Body.String(), "quota_budget_exhausted")
							}
							if test.audio != "" {
								assert.NotContains(t, response.Body.String(), `"type":"speech.audio.done"`)
								assert.NotContains(t, response.Body.String(), `"type":"transcript.text.done"`)
							}
							assert.NotContains(t, response.Body.String(), "[DONE]", "a budget stop must not masquerade as successful completion")
							expected := int64(60)
							if !test.usage {
								expected = int64(service.CountTextToken("allowed "+test.denied, modelName) * 2)
							}
							if test.audio == "speech" {
								expected = 62 // ceil((1 + 30000) / 1000) output estimate
								if test.legacy {
									expected = 186 // frozen audio ratio 2 * completion ratio 3
								}
							}
							assert.Equal(t, expected, bill.Actual)
							assert.EqualValues(t, 50, bill.Charged)
							assert.Equal(t, expected-50, bill.Uncollected)
						} else {
							assert.Contains(t, response.Body.String(), test.denied)
							if test.audio != "speech" {
								assert.Contains(t, response.Body.String(), "unobserved tail")
							}
							if test.audio != "" {
								assert.Contains(t, response.Body.String(), `"type":"`+map[string]string{"speech": "speech.audio.done", "transcription": "transcript.text.done"}[test.audio]+`"`)
							} else if test.format == types.RelayFormatOpenAIResponses {
								assert.Contains(t, response.Body.String(), "event: response.completed")
							} else {
								assert.Contains(t, response.Body.String(), "[DONE]")
							}
							assert.NotContains(t, response.Body.String(), "quota_budget_exhausted")
							expected := int64(10)
							if test.name == "healthy_tokens" {
								expected = 60
							} else if test.name == "responses_healthy" || test.name == "chat_responses_healthy" {
								expected = 300
							} else if test.name == "responses_chat_healthy" {
								expected = 60
							}
							if test.audio != "" {
								expected = 62
							}
							assert.EqualValues(t, expected, bill.Actual)
							assert.EqualValues(t, expected, bill.Charged)
							assert.Zero(t, bill.Uncollected)
						}
						var receipt model.CreditUsageEvidence
						require.NoError(t, db.First(&receipt, bill.UsageEvidenceID).Error)
						var evidence model.CreditEvidenceInput
						require.NoError(t, common.UnmarshalJsonStr(receipt.Payload, &evidence))
						require.NotNil(t, evidence.Consume)
						if test.name == "estimated" || test.name == "healthy_tokens" || test.name == "request_price" {
							var phases []model.CreditUsageEvidence
							require.NoError(t, db.Where("request_id = ? AND stage = ?", bill.ID, "relay").Order("sequence asc").Find(&phases).Error)
							require.Len(t, phases, 3, "upstream response and first client write boundaries must survive settlement")
							for i, row := range phases {
								var recorded struct {
									AttemptPriceEvidenceID int64 `json:"attempt_price_evidence_id"`
									Observation            struct {
										Phase      string `json:"phase"`
										Bytes      *int64 `json:"bytes,omitempty"`
										StatusCode *int   `json:"status_code,omitempty"`
									} `json:"observation"`
								}
								require.NoError(t, common.UnmarshalJsonStr(row.Payload, &recorded))
								assert.Equal(t, []string{"upstream_response", "client_write_possible", "client_write_accepted"}[i], recorded.Observation.Phase)
								assert.Equal(t, evidence.AttemptPriceEvidenceID, recorded.AttemptPriceEvidenceID)
								if i == 0 {
									require.NotNil(t, recorded.Observation.StatusCode)
									assert.Equal(t, http.StatusOK, *recorded.Observation.StatusCode)
									assert.Nil(t, recorded.Observation.Bytes)
								} else {
									require.NotNil(t, recorded.Observation.Bytes)
									assert.Positive(t, *recorded.Observation.Bytes)
									assert.Nil(t, recorded.Observation.StatusCode)
								}
								assert.NotContains(t, row.Payload, "allowed ")
								assert.NotContains(t, row.Payload, "test-key")
								assert.NotContains(t, row.Payload, server.URL)
							}
							if test.stop {
								assert.Contains(t, response.Body.String(), "quota_budget_exhausted")
							}
						}
						if test.converter != "" {
							assert.NotZero(t, evidence.AttemptPriceEvidenceID)
							if !test.stop {
								index := slices.IndexFunc(evidence.Facts, func(fact hosttypes.UsageFact) bool { return fact.Field == "completion_tokens" })
								require.NotEqual(t, -1, index)
								fact := evidence.Facts[index]
								require.NotNil(t, fact.Quantity)
								assert.Equal(t, float64(bill.Actual)/2, *fact.Quantity)
								assert.Equal(t, "upstream", fact.Source)
								assert.False(t, fact.Partial)
								assert.Nil(t, fact.Estimation)
							}
						}

						if test.stop {
							assert.Contains(t, evidence.Consume.Other, `"budget_stop":"quota_budget_exhausted"`)
							var completion *hosttypes.UsageFact
							for i := range evidence.Facts {
								if evidence.Facts[i].Field == "completion_tokens" {
									completion = &evidence.Facts[i]
								}
							}
							require.NotNil(t, completion)
							assert.Equal(t, "estimate", completion.Source, "a buffered transport DONE cannot complete a receipt after a budget stop")
							if test.channel == constant.ChannelTypeGemini {
								assert.Nil(t, completion.Estimation, "an aggregate must not pretend to be the candidate-only counter")
								index := slices.IndexFunc(evidence.Facts, func(fact hosttypes.UsageFact) bool { return fact.Field == "gemini_candidate_tokens" })
								require.NotEqual(t, -1, index)
								require.NotNil(t, evidence.Facts[index].Estimation)
								assert.NotContains(t, response.Body.String(), `"finishReason":"STOP"`)
							} else if test.audio != "speech" {
								require.NotNil(t, completion.Estimation)
							} else {
								require.NotNil(t, completion.Estimation, "audio byte estimates must identify their actual counter")
								assert.Equal(t, modelName, completion.Estimation.Model)
								assert.Equal(t, "decoded-audio-bytes", completion.Estimation.Method)
								assert.Equal(t, float64(31), completion.Estimation.Quantity)
								assert.Equal(t, float64(30001), completion.Estimation.Parameters["decoded_bytes"])
								assert.Equal(t, float64(1000), completion.Estimation.Parameters["bytes_per_token"])
							}
						} else {
							assert.NotContains(t, evidence.Consume.Other, `"budget_stop"`)
						}
						require.NoError(t, db.First(&key, key.Id).Error)
						assert.EqualValues(t, bill.Charged, key.UsedQuota)
						assert.EqualValues(t, test.amount-bill.Charged, key.RemainQuota)
						packs, err := model.ListCreditPacks(db, user.Id, now)
						require.NoError(t, err)
						require.Len(t, packs, 1)
						assert.EqualValues(t, bill.Charged, packs[0].Spent)
						assert.EqualValues(t, test.amount-bill.Charged, packs[0].Available)
						assert.Zero(t, packs[0].Held)
						diffs, err := model.ReconcileCreditAccount(db, user.Id)
						require.NoError(t, err)
						assert.Empty(t, diffs)
					})
				}
			})
			t.Run("buffered_responses_budget_preserves_json_and_accounting", func(t *testing.T) {
				for _, test := range []struct {
					name           string
					healthy, fixed bool
					storageFailure bool
				}{
					{name: "insufficient"},
					{name: "storage_failure", storageFailure: true},
					{name: "healthy", healthy: true},
					{name: "fixed", fixed: true},
				} {
					t.Run(test.name, func(t *testing.T) {
						modelName := "credit-buffered-" + test.name
						expression := `tier("tokens", c * 4)`
						if test.fixed {
							expression = `tier("request", fixed(0.00002))`
						}
						withTieredBillingConfig(t, map[string]string{modelName: "tiered_expr"}, map[string]string{modelName: expression})
						global := model_setting.GetGlobalSettings()
						previous := global.ChatCompletionsToResponsesPolicy
						t.Cleanup(func() { global.ChatCompletionsToResponsesPolicy = previous })
						global.ChatCompletionsToResponsesPolicy = model_setting.ChatCompletionsToResponsesPolicy{Enabled: true, AllChannels: true, ModelPatterns: []string{"^" + modelName + "$"}}
						denied := strings.Repeat("denied ", 100)
						var calls atomic.Int32
						closed := make(chan struct{})
						upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
							calls.Add(1)
							assert.Equal(t, "/v1/responses", r.URL.Path)
							w.Header().Set("Content-Type", "text/event-stream")
							frames := []string{
								`{"type":"response.created","response":{"id":"buffered","status":"in_progress","usage":{"input_tokens":0,"output_tokens":0,"total_tokens":0}}}`,
								`{"type":"response.output_text.delta","delta":"allowed "}`,
								fmt.Sprintf(`{"type":"response.output_text.delta","delta":%q}`, denied),
								`{"type":"response.output_text.delta","delta":"unobserved tail"}`,
								`{"type":"response.completed","response":{"id":"buffered","status":"completed","usage":{"input_tokens":0,"output_tokens":150,"total_tokens":150}}}`,
							}
							if !test.healthy && !test.fixed {
								frames = frames[:3]
							}
							for _, frame := range frames {
								_, err := fmt.Fprintf(w, "data: %s\n\n", frame)
								assert.NoError(t, err)
							}
							if !test.healthy && !test.fixed {
								w.(http.Flusher).Flush()
								<-r.Context().Done()
								close(closed)
							}
						}))
						defer upstream.Close()
						user := model.User{Username: "bjson-" + test.name, AffCode: "bjson-" + test.name, Group: "default", Status: common.UserStatusEnabled, AccountingVersion: 1, Setting: `{"billing_preference":"wallet_only"}`}
						require.NoError(t, db.Create(&user).Error)
						amount := int64(50)
						if test.healthy {
							amount = 1000
						}
						key := model.Token{UserId: user.Id, Key: "bjson" + test.name + strings.Repeat("0", 48-len("bjson"+test.name)), Status: common.TokenStatusEnabled, RemainQuota: int(amount), ExpiredTime: -1, Group: "default"}
						require.NoError(t, db.Create(&key).Error)
						_, err := model.GrantCreditPack(db, model.CreditGrant{UserID: user.Id, SourceType: "test", SourceID: test.name, Amount: amount, StartsAt: now, ExpiresAt: now + 3600, UseMask: model.CreditUseAPI}, now)
						require.NoError(t, err)
						channel := model.Channel{Name: modelName, Type: constant.ChannelTypeOpenAI, Key: "test-key", Status: common.ChannelStatusEnabled, Group: "default", Models: modelName, BaseURL: &upstream.URL}
						require.NoError(t, db.Create(&channel).Error)
						require.NoError(t, db.Create(&model.Ability{ChannelId: channel.Id, Model: modelName, Group: "default", Enabled: true}).Error)
						if test.storageFailure {
							require.NoError(t, db.Callback().Update().Register("credit:buffered-budget-storage", func(tx *gorm.DB) {
								if tx.Statement.Schema != nil && tx.Statement.Schema.Name == "Token" {
									var submitted model.CreditRequest
									result := tx.Session(&gorm.Session{NewDB: true}).Where("user_id = ? AND submitted_at > 0", user.Id).Limit(1).Find(&submitted)
									if result.Error == nil && result.RowsAffected > 0 {
										tx.AddError(errors.New("fixture private buffered storage detail"))
									}
								}
							}))
							t.Cleanup(func() { require.NoError(t, db.Callback().Update().Remove("credit:buffered-budget-storage")) })
						}
						engine := gin.New()
						engine.Use(middleware.RequestId())
						engine.POST("/v1/chat/completions", middleware.TokenAuth(), middleware.Distribute(), func(c *gin.Context) { Relay(c, types.RelayFormatOpenAI) })
						request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(fmt.Sprintf(`{"model":%q,"stream":false,"messages":[{"role":"user","content":"hi"}]}`, modelName)))
						request.Header.Set("Authorization", "Bearer sk-"+key.Key)
						request.Header.Set("Content-Type", "application/json")
						response := httptest.NewRecorder()
						engine.ServeHTTP(response, request)
						if !test.healthy && !test.fixed {
							select {
							case <-closed:
							case <-time.After(10 * time.Second):
								require.FailNow(t, "buffered budget stop did not cancel the upstream")
							}
						}
						assert.EqualValues(t, 1, calls.Load(), "a budget stop must not replay generation")
						assert.NotContains(t, response.Body.String(), "data:")
						var object map[string]any
						require.NoError(t, common.Unmarshal(response.Body.Bytes(), &object), "the non-streaming client must receive one valid JSON object")
						var bill model.CreditRequest
						require.NoError(t, db.Where("user_id = ?", user.Id).First(&bill).Error)
						if test.storageFailure {
							assert.Equal(t, http.StatusInternalServerError, response.Code)
							assert.Contains(t, response.Body.String(), "stream_budget_unavailable")
							assert.NotContains(t, response.Body.String(), "private buffered storage detail")
							assert.Equal(t, "review", bill.State)
							assert.Empty(t, bill.IntentKind)
							assert.Zero(t, bill.Charged)
						} else {
							require.Equal(t, "settled", bill.State)
							if test.healthy || test.fixed {
								assert.Equal(t, http.StatusOK, response.Code)
								assert.Contains(t, response.Body.String(), denied)
								assert.Contains(t, response.Body.String(), "unobserved tail")
								assert.NotContains(t, response.Body.String(), "quota_budget_exhausted")
								expected := int64(300)
								if test.fixed {
									expected = 10
								}
								assert.Equal(t, expected, bill.Actual)
								assert.Equal(t, expected, bill.Charged)
								assert.Zero(t, bill.Uncollected)
							} else {
								assert.Equal(t, http.StatusForbidden, response.Code)
								assert.Contains(t, response.Body.String(), "quota_budget_exhausted")
								assert.NotContains(t, response.Body.String(), "allowed ")
								assert.NotContains(t, response.Body.String(), denied)
								assert.NotContains(t, response.Body.String(), "unobserved tail")
								assert.EqualValues(t, service.CountTextToken("allowed "+denied, modelName)*2, bill.Actual)
								assert.EqualValues(t, amount, bill.Charged)
								assert.Equal(t, bill.Actual-amount, bill.Uncollected)
							}
							var row model.CreditUsageEvidence
							require.NoError(t, db.First(&row, bill.UsageEvidenceID).Error)
							var evidence model.CreditEvidenceInput
							require.NoError(t, common.UnmarshalJsonStr(row.Payload, &evidence))
							assert.NotZero(t, evidence.AttemptPriceEvidenceID)
							if !test.healthy && !test.fixed {
								assert.Contains(t, evidence.Consume.Other, `"budget_stop":"quota_budget_exhausted"`)
							}
						}
						require.NoError(t, db.First(&key, key.Id).Error)
						if test.storageFailure {
							assert.EqualValues(t, bill.Reserved, key.UsedQuota)
						} else {
							assert.EqualValues(t, bill.Charged, key.UsedQuota)
						}
						assert.EqualValues(t, amount-int64(key.UsedQuota), key.RemainQuota)
						packs, err := model.ListCreditPacks(db, user.Id, now)
						require.NoError(t, err)
						require.Len(t, packs, 1)
						assert.Equal(t, bill.Charged, packs[0].Spent)
						if test.storageFailure {
							assert.Equal(t, bill.Reserved, packs[0].Held)
						} else {
							assert.Zero(t, packs[0].Held)
						}
						differences, err := model.ReconcileCreditAccount(db, user.Id)
						require.NoError(t, err)
						assert.Empty(t, differences)
					})
				}
			})
			t.Run("audio_pricing_is_frozen_before_upstream", func(t *testing.T) {
				oldModel, oldAudio, oldAudioCompletion := ratio_setting.ModelRatio2JSONString(), ratio_setting.AudioRatio2JSONString(), ratio_setting.AudioCompletionRatio2JSONString()
				t.Cleanup(func() {
					require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(oldModel))
					require.NoError(t, ratio_setting.UpdateAudioRatioByJSONString(oldAudio))
					require.NoError(t, ratio_setting.UpdateAudioCompletionRatioByJSONString(oldAudioCompletion))
				})
				require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(`{"credit-frozen-audio":1}`))
				require.NoError(t, ratio_setting.UpdateAudioRatioByJSONString(`{"credit-frozen-audio":2}`))
				require.NoError(t, ratio_setting.UpdateAudioCompletionRatioByJSONString(`{"credit-frozen-audio":3}`))
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					require.NoError(t, ratio_setting.UpdateAudioRatioByJSONString(`{"credit-frozen-audio":20}`))
					require.NoError(t, ratio_setting.UpdateAudioCompletionRatioByJSONString(`{"credit-frozen-audio":30}`))
					w.Header().Set("Content-Type", "audio/pcm")
					_, err := fmt.Fprint(w, strings.Repeat("a", 48000))
					assert.NoError(t, err)
				}))
				defer server.Close()
				ch := model.Channel{Name: "frozen-audio", Type: constant.ChannelTypeOpenAI, Key: "test-key", Status: common.ChannelStatusEnabled, Group: "default", Models: "credit-frozen-audio", BaseURL: &server.URL}
				require.NoError(t, db.Create(&ch).Error)
				require.NoError(t, db.Create(&model.Ability{ChannelId: ch.Id, Model: "credit-frozen-audio", Group: "default", Enabled: true}).Error)
				u := model.User{Username: "frozen-audio", Password: "unused", Status: common.UserStatusEnabled, Group: "default", AffCode: "frozen-audio", AccountingVersion: 1, Setting: `{"billing_preference":"wallet_only"}`}
				require.NoError(t, db.Create(&u).Error)
				key := model.Token{UserId: u.Id, Key: "frozenaudio" + strings.Repeat("0", 38), Status: common.TokenStatusEnabled, RemainQuota: 1000, ExpiredTime: -1, Group: "default"}
				require.NoError(t, db.Create(&key).Error)
				_, err := model.GrantCreditPack(db, model.CreditGrant{UserID: u.Id, SourceType: "test", SourceID: "frozen-audio", Amount: 1000, StartsAt: now, ExpiresAt: now + 3600, UseMask: model.CreditUseAPI}, now)
				require.NoError(t, err)
				e := gin.New()
				e.Use(middleware.RequestId())
				e.POST("/v1/audio/speech", middleware.TokenAuth(), middleware.Distribute(), func(c *gin.Context) { Relay(c, types.RelayFormatOpenAIAudio) })
				r := httptest.NewRequest(http.MethodPost, "/v1/audio/speech", strings.NewReader(`{"model":"credit-frozen-audio","input":"hi","voice":"alloy","response_format":"pcm"}`))
				r.Header.Set("Authorization", "Bearer sk-"+key.Key)
				r.Header.Set("Content-Type", "application/json")
				w := httptest.NewRecorder()
				e.ServeHTTP(w, r)
				require.Equal(t, http.StatusOK, w.Code, w.Body.String())
				var request model.CreditRequest
				require.NoError(t, db.Where("user_id = ?", u.Id).First(&request).Error)
				assert.EqualValues(t, 102, request.Charged, "17 estimated audio tokens at frozen ratios 2 * 3, not newly edited 20 * 30")
				assert.Zero(t, request.Uncollected)
				var evidence model.CreditUsageEvidence
				require.NoError(t, db.First(&evidence, request.UsageEvidenceID).Error)
				var measured model.CreditEvidenceInput
				require.NoError(t, common.UnmarshalJsonStr(evidence.Payload, &measured))
				index := slices.IndexFunc(measured.Facts, func(fact hosttypes.UsageFact) bool { return fact.Field == "audio_output_tokens" })
				require.NotEqual(t, -1, index)
				counter := measured.Facts[index].Estimation
				require.NotNil(t, counter, "binary PCM estimates must retain duration and sample configuration")
				assert.Equal(t, "credit-frozen-audio", counter.Model)
				assert.Equal(t, "ceil-audio-seconds", counter.Method)
				assert.Equal(t, float64(17), counter.Quantity)
				assert.Equal(t, float64(1), counter.Parameters["duration_seconds"])
				assert.Equal(t, float64(24000), counter.Parameters["sample_rate"])
				assert.Equal(t, float64(2), counter.Parameters["bytes_per_sample"])
				assert.Equal(t, float64(1000), counter.Parameters["tokens_per_minute"])
			})
			t.Run("tool_prices_are_frozen_before_upstream", func(t *testing.T) {
				previousPrices := config.GlobalConfig.ExportAllConfigs()[operation_setting.ToolPriceOptionKey]
				t.Cleanup(func() { operation_setting.LoadToolPricesFromJSONString(previousPrices) })
				for _, initialPrice := range []int{10, 0} {
					t.Run(fmt.Sprintf("initial_%d", initialPrice), func(t *testing.T) {
						modelName := fmt.Sprintf("credit-frozen-%d-search-preview", initialPrice)
						withTieredBillingConfig(t, map[string]string{modelName: "tiered_expr"}, map[string]string{modelName: `tier("request", fixed(0.00004))`})
						operation_setting.LoadToolPricesFromJSONString(fmt.Sprintf(`{"web_search_preview":%d}`, initialPrice))
						server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
							operation_setting.LoadToolPricesFromJSONString(`{"web_search_preview":20}`)
							w.Header().Set("Content-Type", "application/json")
							_, err := fmt.Fprint(w, `{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":12,"completion_tokens":8,"total_tokens":20}}`)
							assert.NoError(t, err)
						}))
						defer server.Close()
						ch := model.Channel{Name: modelName, Type: constant.ChannelTypeOpenAI, Key: "test-key", Status: common.ChannelStatusEnabled, Group: "default", Models: modelName, BaseURL: &server.URL}
						require.NoError(t, db.Create(&ch).Error)
						require.NoError(t, db.Create(&model.Ability{ChannelId: ch.Id, Model: modelName, Group: "default", Enabled: true}).Error)
						u := model.User{Username: modelName, Password: "unused", Status: common.UserStatusEnabled, Group: "default", AffCode: modelName, AccountingVersion: 1, Setting: `{"billing_preference":"wallet_only"}`}
						require.NoError(t, db.Create(&u).Error)
						key := model.Token{UserId: u.Id, Key: fmt.Sprintf("frozentool%038d", initialPrice), Status: common.TokenStatusEnabled, RemainQuota: 20000, ExpiredTime: -1, Group: "default"}
						require.NoError(t, db.Create(&key).Error)
						_, err := model.GrantCreditPack(db, model.CreditGrant{UserID: u.Id, SourceType: "test", SourceID: modelName, Amount: 20000, StartsAt: now, ExpiresAt: now + 3600, UseMask: model.CreditUseAPI}, now)
						require.NoError(t, err)
						e := gin.New()
						e.Use(middleware.RequestId())
						e.POST("/v1/chat/completions", middleware.TokenAuth(), middleware.Distribute(), func(c *gin.Context) { Relay(c, types.RelayFormatOpenAI) })
						r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hi"}]}`, modelName)))
						r.Header.Set("Authorization", "Bearer sk-"+key.Key)
						r.Header.Set("Content-Type", "application/json")
						w := httptest.NewRecorder()
						e.ServeHTTP(w, r)
						require.Equal(t, http.StatusOK, w.Code, w.Body.String())
						var request model.CreditRequest
						require.NoError(t, db.Where("user_id = ?", u.Id).First(&request).Error)
						assert.EqualValues(t, 20+initialPrice*500, request.Charged, "one tool call uses the captured price, including explicit zero")
						assert.Zero(t, request.Uncollected)
						var snapshot struct {
							ToolPrices map[string]float64 `json:"tool_prices"`
						}
						require.NoError(t, common.UnmarshalJsonStr(request.PriceSnapshot, &snapshot))
						require.Contains(t, snapshot.ToolPrices, dto.BuildInToolWebSearchPreview)
						assert.Equal(t, float64(initialPrice), snapshot.ToolPrices[dto.BuildInToolWebSearchPreview])
					})
				}
			})
			t.Run("task_plugin_keeps_holds_until_terminal_and_replays_once", func(t *testing.T) {
				require.NoError(t, db.AutoMigrate(&model.Task{}))
				const expression = `tier("work", u("units") * 0.00001)`
				withTieredBillingConfig(t, map[string]string{"credit-task": "tiered_expr"}, map[string]string{"credit-task": expression})
				plugin, err := pluginruntime.CompilePlugin(`
export const meta={apiVersion:1,key:"credit-task",name:"Credit task",version:"1.0.0",author:{name:"Test"},models:["credit-task"],fetchMode:"per_task",usageSchema:{units:{type:"number",unit:"credit",description:{en:"Resource credit unit price",zh:"资源积分单价"}},kind:{enum:["paid","free"],description:{en:"Billing category",zh:"计费类型"}}}};
export function buildSubmitRequest(ctx){return {url:ctx.baseUrl+"/jobs",body:ctx.requestBody};}
export function parseSubmitResponse(ctx,resp){return {taskId:"vendor-job",taskData:resp.body,immediate:resp.body.status?{status:resp.body.status}:undefined};}
export function extractUsage(ctx){return {units:ctx.requestBody.estimateUnits??4,kind:ctx.requestBody.kind??"paid"};}
export function extractUsageOnComplete(ctx,result,body){return body.usage;}
export function parseTaskResult(){return {status:"SUCCESS"};}
export function buildQueryRequest(ctx){return {url:ctx.baseUrl+"/jobs/"+ctx.taskId};}
export function buildBatchQueryRequest(ctx,tasks){return {url:ctx.baseUrl+"/jobs/batch",body:{ids:tasks.map(task=>task.taskId)}};}
export function parseBatchResult(ctx,body){return body;}
`, pluginruntime.Options{})
				require.NoError(t, err)
				for index, tc := range []struct {
					name, status  string
					units         float64
					finalState    string
					charged, held int64
				}{
					{"pending", "", 6, "executing", 0, 20},
					{"immediate", "SUCCESS", 2, "settled", 10, 0},
					{"zero", "SUCCESS", 0, "settled", 0, 0},
					{"failed_unknown", "FAILURE", 0, "review", 0, 20},
					{"pending_over_budget", "", 30, "executing", 0, 20},
					{"pending_recovery", "", 6, "executing", 0, 20},
					{"fractional_credit", "SUCCESS", 3.5, "settled", 18, 0},
					{"pending_polled", "", 3.5, "executing", 0, 20},
					{"pending_native_integer", "", 6, "executing", 0, 20},
					{"pending_polled_recovery", "", 3.5, "executing", 0, 20},
					{"pending_retry_recovery", "", 3.5, "executing", 0, 20},
					{"pending_retry_free_recovery", "", 3.5, "executing", 0, 20},
					{"immediate_over_budget", "SUCCESS", 30, "settled", 100, 0},
					{"immediate_clamped", "SUCCESS", common.MaxQuota, "settled", 100, 0},
					{"zero_unknown", "SUCCESS", 0, "review", 0, 0},
					{"constant_free", "SUCCESS", 4, "settled", 0, 0},
					{"selective_free", "SUCCESS", 4, "settled", 0, 0},
					{"pending_zero_unknown", "", 4, "executing", 0, 20},
				} {
					t.Run(tc.name, func(t *testing.T) {
						if tc.name == "constant_free" {
							withTieredBillingConfig(t, map[string]string{"credit-task": "tiered_expr"}, map[string]string{"credit-task": `tier("free", 0)`})
						}
						if tc.name == "selective_free" {
							withTieredBillingConfig(t, map[string]string{"credit-task": "tiered_expr"}, map[string]string{"credit-task": `u("kind") == "free" ? tier("free", 0) : tier("work", u("units") * 0.00001)`})
						}
						server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
							w.Header().Set("Content-Type", "application/json")
							if r.URL.Path == "/jobs/batch" {
								_, err := fmt.Fprintf(w, `[{"taskId":"vendor-job","status":"SUCCESS","data":{"usage":{"units":%g}}}]`, tc.units)
								assert.NoError(t, err)
								return
							}
							assert.Equal(t, "/jobs", r.URL.Path)
							if tc.name == "zero_unknown" || tc.name == "constant_free" || tc.name == "selective_free" {
								_, err := fmt.Fprint(w, `{"status":"SUCCESS","usage":{}}`)
								assert.NoError(t, err)
								return
							}
							_, err := fmt.Fprintf(w, `{"status":%q,"usage":{"units":%g}}`, tc.status, tc.units)
							assert.NoError(t, err)
						}))
						defer server.Close()
						u := model.User{Username: "task-" + tc.name, AffCode: "task-" + tc.name, Group: "default", Status: common.UserStatusEnabled, AccountingVersion: 1}
						require.NoError(t, db.Create(&u).Error)
						key := model.Token{UserId: u.Id, Key: fmt.Sprintf("task%044d", index), Status: common.TokenStatusEnabled, RemainQuota: 100, ExpiredTime: -1}
						require.NoError(t, db.Create(&key).Error)
						_, err := model.GrantCreditPack(db, model.CreditGrant{UserID: u.Id, SourceType: "test", SourceID: tc.name, Amount: 100, StartsAt: now, ExpiresAt: now + 3600, UseMask: model.CreditUseAPI}, now)
						require.NoError(t, err)
						ch := model.Channel{Name: "task-test", Type: constant.ChannelTypeTaskPlugin, Status: common.ChannelStatusEnabled, BaseURL: &server.URL}
						require.NoError(t, db.Create(&ch).Error)
						c := taskSubmissionTestContext()
						c.Set("group", "default")
						c.Set("username", u.Username)
						taskInput := map[string]any{"model": "credit-task"}
						if tc.name == "zero_unknown" {
							taskInput["estimateUnits"] = 0
						}
						if tc.name == "selective_free" {
							taskInput["kind"] = "free"
						}
						c.Set("task_request", taskInput)
						c.Set(pluginruntime.ContextKeyPinnedPlugin, pluginruntime.PinnedPlugin{Plugin: plugin})
						common.SetContextKey(c, constant.ContextKeyOriginalModel, "credit-task")
						common.SetContextKey(c, constant.ContextKeyChannelBaseUrl, server.URL)
						common.SetContextKey(c, constant.ContextKeyChannelId, ch.Id)
						common.SetContextKey(c, constant.ContextKeyChannelType, ch.Type)
						info := taskSubmissionRelayInfo(nil)
						info.UserId, info.TokenId, info.TokenKey = u.Id, key.Id, key.Key
						info.OriginModelName, info.UserGroup = "credit-task", "default"
						info.UserSetting.BillingPreference = "wallet_only"
						info.RelayFormat = types.RelayFormatTask
						info.LockedChannel, info.PublicTaskID = &ch, model.GenerateTaskID()
						outcome, taskErr := executeTaskSubmission(c, info)
						require.Nil(t, taskErr)
						require.NotNil(t, outcome)
						var bill model.CreditRequest
						require.NoError(t, db.Where("user_id = ?", u.Id).First(&bill).Error)
						var pricing struct {
							Expression struct {
								Units map[string]string `json:"task_usage_units"`
							} `json:"expression"`
						}
						require.NoError(t, common.UnmarshalJsonStr(bill.PriceSnapshot, &pricing))
						assert.Equal(t, "credit", pricing.Expression.Units["units"], "the executing model's unit is frozen before submission")
						assert.Equal(t, tc.finalState, bill.State)
						assert.Equal(t, tc.charged, bill.Charged)
						if bill.State == "settled" {
							assert.EqualValues(t, bill.Charged, outcome.Task.Quota)

							require.NotZero(t, bill.UsageEvidenceID, "task terminal evidence is durable before the monetary intent")
							var evidence model.CreditUsageEvidence
							require.NoError(t, db.First(&evidence, bill.UsageEvidenceID).Error)
							var receipt model.CreditEvidenceInput
							require.NoError(t, common.UnmarshalJsonStr(evidence.Payload, &receipt))
							require.NotNil(t, receipt.Consume)
							assert.Equal(t, bill.Actual, receipt.Consume.ReferenceQuota)
							if tc.name == "immediate_clamped" {
								assert.EqualValues(t, common.MaxQuota, bill.Actual)
								assert.Contains(t, receipt.Consume.Other, `"quota_saturation"`, "a saturated final price remains auditable")
							}
							if tc.name == "immediate_over_budget" {
								assert.EqualValues(t, 150, bill.Actual)
								assert.EqualValues(t, 50, bill.Uncollected)
							}
							var tokenFields []string
							for _, fact := range receipt.Facts {
								if fact.Field == "prompt_tokens" || fact.Field == "completion_tokens" {
									tokenFields = append(tokenFields, fact.Field)
									assert.Nil(t, fact.Quantity, "a task without token receipts must not manufacture reported zeros")
									assert.Equal(t, "unknown", fact.Source)
								}
							}
							assert.ElementsMatch(t, []string{"prompt_tokens", "completion_tokens"}, tokenFields)
							var taskFact *hosttypes.UsageFact
							for i := range receipt.Facts {
								if receipt.Facts[i].Field == "task.units" {
									taskFact = &receipt.Facts[i]
								}
							}
							require.NotNil(t, taskFact)
							assert.Equal(t, "credit", taskFact.Unit)
							if tc.name == "constant_free" || tc.name == "selective_free" {
								assert.Equal(t, "estimate", taskFact.Source, "unused quantity can remain estimated without making a proven free branch unknown")
								assert.True(t, receipt.Consume.ZeroChargeEstablished)
							} else {
								assert.Equal(t, "adaptor", taskFact.Source, "plugin extraction does not prove a raw provider field")
							}
							require.NotNil(t, taskFact.Quantity)
							assert.Equal(t, tc.units, *taskFact.Quantity, "zero and fractional vendor units are preserved")
						}
						if tc.name == "zero_unknown" {
							require.NotZero(t, bill.UsageEvidenceID)
							var evidence model.CreditUsageEvidence
							require.NoError(t, db.First(&evidence, bill.UsageEvidenceID).Error)
							var receipt model.CreditEvidenceInput
							require.NoError(t, common.UnmarshalJsonStr(evidence.Payload, &receipt))
							require.NotNil(t, receipt.Consume)
							assert.False(t, receipt.Consume.ZeroChargeEstablished, "submission estimate zero does not establish free completed usage")
							assert.Empty(t, bill.IntentKind)
							require.NoError(t, db.First(&key, key.Id).Error)
							assert.Equal(t, 100, key.RemainQuota)
							assert.Zero(t, key.UsedQuota)
						}
						assert.Equal(t, bill.ID, outcome.Task.PrivateData.CreditRequestID)
						packs, err := model.ListCreditPacks(db, u.Id, now)
						require.NoError(t, err)
						assert.Equal(t, tc.held, packs[0].Held)
						if strings.HasPrefix(tc.name, "pending") {
							actual, charged := 30, 30
							if tc.name == "pending_over_budget" {
								actual, charged = 150, 100
							}
							var stored model.Task
							require.NoError(t, db.First(&stored, outcome.Task.ID).Error)
							if tc.name == "pending_polled" || tc.name == "pending_polled_recovery" || tc.name == "pending_retry_recovery" || tc.name == "pending_retry_free_recovery" {
								polledCharge := int64(18)
								recovery := tc.name != "pending_polled"
								if tc.name == "pending_retry_recovery" || tc.name == "pending_retry_free_recovery" {
									// Simulate the durable checkpoint after a second submission,
									// before terminal polling and its metering transaction.
									_, attempt, err := model.GetCreditAttemptPriceEvidence(db, u.Id, bill.ID, 1)
									require.NoError(t, err)
									var price struct {
										Price        hosttypes.PriceData          `json:"price"`
										Expression   *billingexpr.BillingSnapshot `json:"expression"`
										QuotaPerUnit float64                      `json:"quota_per_unit"`
									}
									require.NoError(t, common.UnmarshalJsonStr(attempt.AttemptPrice.Snapshot, &price))
									require.NotNil(t, price.Expression)
									ratio := float64(2)
									polledCharge = 35
									if tc.name == "pending_retry_free_recovery" {
										ratio, polledCharge = 0, 0
									}
									price.Price.GroupRatioInfo.GroupRatio, price.Expression.GroupRatio = ratio, ratio
									price.Expression.EstimatedQuotaAfterGroup = common.QuotaRound(price.Expression.EstimatedQuotaBeforeGroup * ratio)
									encoded, err := common.Marshal(price)
									require.NoError(t, err)
									attempt.Attempt = 2
									attempt.AttemptPrice.Group, attempt.AttemptPrice.Snapshot = "task-second", string(encoded)
									lease, err := model.ClaimCreditExecution(db, u.Id, bill.ID, "retry-checkpoint", 120, common.GetTimestamp(), common.GetTimestamp)
									require.NoError(t, err)
									if ratio > 0 {
										_, err := model.GrowCreditRequestReservation(db, u.Id, bill.ID, 40, common.GetTimestamp(), lease)
										require.NoError(t, err)
									}
									_, err = model.RecordCreditAttemptSubmission(db, attempt, common.GetTimestamp(), lease)
									require.NoError(t, err)
									require.NoError(t, model.YieldCreditExecution(db, lease, common.GetTimestamp()))
									stored.PrivateData.BillingContext.TieredSnapshot.GroupRatio = ratio
									stored.PrivateData.BillingContext.GroupRatio = ratio
									require.NoError(t, stored.Update())
								}
								if recovery {
									require.NoError(t, db.Callback().Create().Before("gorm:create").Register("test:terminal-metering-store", func(tx *gorm.DB) {
										if evidence, ok := tx.Statement.Dest.(*model.CreditUsageEvidence); ok && evidence.Stage == "settlement" {
											tx.AddError(fmt.Errorf("terminal metering store unavailable"))
										}
									}))
								}
								require.NoError(t, service.UpdateBatchTasks(c, taskpluginadaptor.New(plugin), map[int][]string{ch.Id: {"vendor-job"}}, map[string]*model.Task{"vendor-job": &stored}))
								require.NoError(t, db.First(&stored, stored.ID).Error)
								require.EqualValues(t, model.TaskStatusSuccess, stored.Status)
								require.NotNil(t, stored.PrivateData.BillingContext)
								require.NotNil(t, stored.PrivateData.BillingContext.TieredSnapshot)
								assert.Equal(t, tc.units, stored.PrivateData.BillingContext.TieredSnapshot.MeasuredUsageFacts["units"], "terminal status and measured facts are persisted together")
								if recovery {
									require.NoError(t, db.Callback().Create().Remove("test:terminal-metering-store"))
									require.NoError(t, db.First(&bill, bill.ID).Error)
									require.Zero(t, bill.UsageEvidenceID)
									require.Empty(t, bill.IntentKind)
									withTieredBillingConfig(t, map[string]string{"credit-task": "tiered_expr"}, map[string]string{"credit-task": `tier("new-price", u("units") * 1)`})
									summary, err := service.RunCreditRecoveryPass(c, db, db, "task-terminal-recovery", common.GetTimestamp())
									require.NoError(t, err)
									assert.Zero(t, summary.Errors)
									require.NoError(t, db.First(&stored, stored.ID).Error)
									_, err = service.RunCreditRecoveryPass(c, db, db, "task-terminal-replay", common.GetTimestamp())
									require.NoError(t, err)
								}
								require.NoError(t, db.First(&bill, bill.ID).Error)
								assert.EqualValues(t, polledCharge, bill.Charged)
								assert.EqualValues(t, polledCharge, bill.Actual)
								assert.EqualValues(t, polledCharge, stored.Quota)
								var evidence model.CreditUsageEvidence
								require.NoError(t, db.First(&evidence, bill.UsageEvidenceID).Error)
								var receipt model.CreditEvidenceInput
								require.NoError(t, common.UnmarshalJsonStr(evidence.Payload, &receipt))
								assert.Equal(t, tc.name == "pending_retry_free_recovery", receipt.Consume.ZeroChargeEstablished)
								if tc.name == "pending_retry_recovery" || tc.name == "pending_retry_free_recovery" {
									assert.Equal(t, 2, receipt.Attempt)
									assert.NotZero(t, receipt.AttemptPriceEvidenceID)
									assert.Contains(t, bill.PriceSnapshot, `"group_ratio":1`)
								}
								foundTaskUsage := false
								for _, fact := range receipt.Facts {
									if fact.Field == "task.units" {
										foundTaskUsage = true
										require.NotNil(t, fact.Quantity)
										assert.Equal(t, tc.units, *fact.Quantity)
										assert.Equal(t, "credit", fact.Unit)
										assert.Equal(t, "adaptor", fact.Source)
									}
								}
								assert.True(t, foundTaskUsage)
								return
							}
							stored.Status = model.TaskStatusSuccess
							require.NoError(t, stored.Update())
							if tc.name == "pending_zero_unknown" {
								service.RecalculateTaskQuota(c, &stored, 0, "no final quantity")
								require.NoError(t, db.First(&bill, bill.ID).Error)
								assert.Equal(t, "review", bill.State)
								assert.Empty(t, bill.IntentKind, "unknown zero cannot become a financial intent")
								packs, err := model.ListCreditPacks(db, u.Id, now)
								require.NoError(t, err)
								assert.EqualValues(t, 20, packs[0].Held)
								assert.Zero(t, packs[0].Spent)
								require.NoError(t, db.First(&key, key.Id).Error)
								assert.Equal(t, 80, key.RemainQuota)
								assert.Equal(t, 20, key.UsedQuota, "the original Key reservation remains held")
								return
							}
							if tc.name == "pending_native_integer" {
								stored.PrivateData.BillingContext.TieredSnapshot.MeasuredUsageFacts = map[string]any{"units": int64(6)}
							}
							if tc.name == "pending_recovery" {
								require.NoError(t, db.Callback().Update().Before("gorm:update").Register("test:credit-task-intent", func(tx *gorm.DB) {
									if values, ok := tx.Statement.Dest.(map[string]any); ok && values["intent_kind"] == "settle" {
										tx.AddError(fmt.Errorf("intent store unavailable"))
									}
								}))
								service.RecalculateTaskQuota(c, &stored, actual, "pre-intent exit")
								require.NoError(t, db.Callback().Update().Remove("test:credit-task-intent"))
								require.NoError(t, db.First(&bill, bill.ID).Error)
								require.Empty(t, bill.IntentKind)
								require.NotZero(t, bill.UsageEvidenceID)
								receiptID := bill.UsageEvidenceID
								results, _, err := model.RecoverCreditRequests(db, "task-recovery", bill.ID-1, 1, common.GetTimestamp())
								require.NoError(t, err)
								require.Len(t, results, 1)
								assert.Equal(t, "settled", results[0].State, "durable terminal metering recovers before the intent exists")
								require.NoError(t, db.First(&bill, bill.ID).Error)
								assert.Equal(t, receiptID, bill.UsageEvidenceID)
								assert.EqualValues(t, charged, bill.Charged)
								return
							}
							require.NoError(t, db.Callback().Update().Before("gorm:update").Register("test:credit-task-projection", func(tx *gorm.DB) {
								if tx.Statement.Table == "tasks" {
									tx.AddError(fmt.Errorf("task quota projection unavailable"))
								}
							}))
							service.RecalculateTaskQuota(c, &stored, actual, "completion write failure")
							require.NoError(t, db.Callback().Update().Remove("test:credit-task-projection"))
							require.NoError(t, db.First(&bill, bill.ID).Error)
							assert.Equal(t, "pending", bill.State)
							assert.Zero(t, bill.Charged)
							require.NotZero(t, bill.UsageEvidenceID, "a failed funds transaction retains its prior metering evidence")
							priorEvidenceID := bill.UsageEvidenceID
							packs, err = model.ListCreditPacks(db, u.Id, now)
							require.NoError(t, err)
							assert.EqualValues(t, 20, packs[0].Held)
							assert.Zero(t, packs[0].Spent)
							service.RecalculateTaskQuota(c, &stored, actual, "verified completion")
							assert.Equal(t, charged, stored.Quota, "task projections contain only the user's collected fee")
							service.RecalculateTaskQuota(c, &stored, actual, "verified completion replay")
							assert.Equal(t, charged, stored.Quota, "replay must not replace collected quota with reference cost")
							require.NoError(t, db.First(&bill, bill.ID).Error)
							assert.EqualValues(t, actual, bill.Actual)
							assert.Equal(t, priorEvidenceID, bill.UsageEvidenceID, "retry reuses the immutable terminal receipt")
							assert.EqualValues(t, charged, bill.Charged)
							assert.EqualValues(t, actual-charged, bill.Uncollected)
							assert.Equal(t, "settled", bill.State)
							if tc.name == "pending_native_integer" {
								var evidence model.CreditUsageEvidence
								require.NoError(t, db.First(&evidence, bill.UsageEvidenceID).Error)
								var receipt model.CreditEvidenceInput
								require.NoError(t, common.UnmarshalJsonStr(evidence.Payload, &receipt))
								found := false
								for _, fact := range receipt.Facts {
									if fact.Field == "task.units" {
										found = true
										require.NotNil(t, fact.Quantity)
										assert.Equal(t, float64(6), *fact.Quantity)
										assert.Equal(t, "adaptor", fact.Source)
									}
								}
								assert.True(t, found)
							}
							require.NoError(t, db.First(&key, key.Id).Error)
							assert.Equal(t, 100-charged, key.RemainQuota)
							assert.Equal(t, charged, key.UsedQuota)
							assert.False(t, service.RefundTaskQuota(c, &stored, "late failure"), "terminal success cannot be overwritten by failure")
							if tc.name == "pending" {
								actor := model.User{Username: "task-adjust-admin", Password: "fixture", AffCode: "task-adjust-admin", Status: common.UserStatusEnabled, Role: common.RoleRootUser}
								require.NoError(t, db.Create(&actor).Error)
								stale := stored
								quantity := float64(2)
								correction, err := model.AdjustCreditBill(db, model.CreditBillAdjustmentInput{UserID: u.Id, RequestID: bill.ID, ActorID: actor.Id, EventID: "task-corrected-receipt", ReferenceQuota: 10, EvidenceVersion: "verified-fixture-v1", Facts: []hosttypes.UsageFact{{Field: "task.units", Unit: "credit", Quantity: &quantity, Source: "adaptor", Algorithm: "task-plugin-usage-v1"}}, Reason: "verified corrected receipt"}, common.GetTimestamp())
								require.NoError(t, err)
								assert.EqualValues(t, 10, correction.Charged)
								service.RecalculateTaskQuota(c, &stored, actual, "late original completion after correction")
								assert.Equal(t, 10, stored.Quota, "a terminal replay must preserve corrected collected fee")
								require.NoError(t, db.First(&key, key.Id).Error)
								assert.Equal(t, 90, key.RemainQuota)
								assert.Equal(t, 10, key.UsedQuota)
								require.NoError(t, stale.Update())
								_, err = stale.UpdateWithStatus(stale.Status)
								require.NoError(t, err)
								require.NoError(t, db.First(&stored, stored.ID).Error)
								assert.Equal(t, 10, stored.Quota, "a stale task row cannot overwrite the account's corrected charge")
							}

						}
					})
				}
			})

			t.Run("realtime_budget_stops_before_output_without_debt", func(t *testing.T) {
				for _, test := range []struct {
					name                            string
					audio, storageFailure, healthy  bool
					priorReceipt, clientRejected    bool
					clientAccepted, estimateFailure bool
				}{
					{name: "estimated_text"},
					{name: "estimated_audio", audio: true},
					{name: "storage_failure", storageFailure: true},
					{name: "healthy", healthy: true},
					{name: "prior_receipt", priorReceipt: true},
					{name: "rejected_client_input", clientRejected: true},
					{name: "accepted_client_input", clientAccepted: true, healthy: true},
					{name: "evidence_failure", estimateFailure: true, storageFailure: true},
				} {
					t.Run(test.name, func(t *testing.T) {
						modelName := "credit-rt-budget-" + test.name
						expression := `tier("stream", c * 4 + ao * 8)`
						if test.clientRejected || test.clientAccepted {
							expression = `tier("stream", p * 4 + c * 4)`
						}
						withTieredBillingConfig(t, map[string]string{modelName: "tiered_expr"}, map[string]string{modelName: expression})
						allowed, denied := "allowed ", strings.Repeat("denied ", 100)
						eventType := "response.audio_transcript.delta"
						if test.audio {
							eventType = "response.audio.delta"
							allowed = base64.StdEncoding.EncodeToString(make([]byte, 4800))
							denied = base64.StdEncoding.EncodeToString(make([]byte, 480000))
						}
						closed := make(chan struct{})
						var calls atomic.Int32
						var inputForwarded atomic.Bool
						upgrader := websocket.Upgrader{}
						upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
							calls.Add(1)
							conn, err := upgrader.Upgrade(w, r, nil)
							if !assert.NoError(t, err) {
								return
							}
							defer conn.Close()
							defer close(closed)
							frames := []string{
								`{"type":"session.created","session":{"input_audio_format":"pcm16","output_audio_format":"pcm16"}}`,
								fmt.Sprintf(`{"event_id":"first","type":%q,"delta":%q}`, eventType, allowed),
								fmt.Sprintf(`{"event_id":"second","type":%q,"delta":%q}`, eventType, denied),
								`{"event_id":"tail","type":"response.audio_transcript.delta","delta":"unobserved tail"}`,
								`{"event_id":"final","type":"response.done","response":{"id":"native-budget-response","usage":{"input_tokens":0,"output_tokens":150,"total_tokens":150,"input_token_details":{"text_tokens":0,"audio_tokens":0,"cached_tokens":0},"output_token_details":{"text_tokens":150,"audio_tokens":0}}}}`,
							}
							if test.priorReceipt {
								receipt := `{"event_id":"prior-receipt","type":"response.done","response":{"id":"prior-response","usage":{"input_tokens":0,"output_tokens":5,"total_tokens":5,"input_token_details":{"text_tokens":0,"audio_tokens":0,"cached_tokens":0},"output_token_details":{"text_tokens":5,"audio_tokens":0}}}}`
								frames = append(frames[:1], append([]string{receipt}, frames[1:]...)...)
							}
							if test.clientRejected || test.clientAccepted {
								frames = frames[:2]
							}
							for _, frame := range frames {
								if err := conn.WriteMessage(websocket.TextMessage, []byte(frame)); err != nil {
									return
								}
							}
							if test.clientAccepted {
								_, _, err = conn.ReadMessage()
								if !assert.NoError(t, err) {
									return
								}
								for _, frame := range []string{
									fmt.Sprintf(`{"event_id":"input-ack","type":"conversation.item.created","item":{"type":"message","role":"user","content":[{"type":"input_text","text":%q}]}}`, allowed),
									`{"event_id":"final","type":"response.done","response":{"id":"native-budget-response","usage":{"input_tokens":2,"output_tokens":2,"total_tokens":4,"input_token_details":{"text_tokens":2,"audio_tokens":0,"cached_tokens":0},"output_token_details":{"text_tokens":2,"audio_tokens":0}}}}`,
								} {
									if !assert.NoError(t, conn.WriteMessage(websocket.TextMessage, []byte(frame))) {
										return
									}
								}
							}
							if test.healthy {
								_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, "completed"), time.Now().Add(5*time.Second))
								return
							}
							_, _, err = conn.ReadMessage() // the gateway must cancel the upstream before client cleanup
							if test.clientRejected && err == nil {
								inputForwarded.Store(true)
							}
						}))
						defer upstream.Close()
						u := model.User{Username: "rt-" + test.name, AffCode: "rt-" + test.name, Status: common.UserStatusEnabled, Group: "default", AccountingVersion: 1, Setting: `{"billing_preference":"wallet_only"}`}
						require.NoError(t, db.Create(&u).Error)
						amount := int64(50)
						if test.healthy {
							amount = 1000
						}
						if test.clientAccepted {
							amount = 10
						}
						key := model.Token{UserId: u.Id, Key: "rtbudget" + test.name + strings.Repeat("0", 48-len("rtbudget"+test.name)), Status: common.TokenStatusEnabled, RemainQuota: int(amount), ExpiredTime: -1, Group: "default"}
						require.NoError(t, db.Create(&key).Error)
						_, err := model.GrantCreditPack(db, model.CreditGrant{UserID: u.Id, SourceType: "test", SourceID: test.name, Amount: amount, StartsAt: now, ExpiresAt: now + 3600, UseMask: model.CreditUseAPI}, now)
						require.NoError(t, err)
						channel := model.Channel{Name: modelName, Type: constant.ChannelTypeOpenAI, Key: "test-key", Status: common.ChannelStatusEnabled, Group: "default", Models: modelName, BaseURL: &upstream.URL}
						require.NoError(t, db.Create(&channel).Error)
						require.NoError(t, db.Create(&model.Ability{ChannelId: channel.Id, Model: modelName, Group: "default", Enabled: true}).Error)
						if test.estimateFailure {
							require.NoError(t, db.Callback().Create().Register("credit:rt-estimate-storage", func(tx *gorm.DB) {
								if evidence, ok := tx.Statement.Dest.(*model.CreditUsageEvidence); ok && evidence.Stage == "estimate" {
									tx.AddError(errors.New("fixture private realtime storage detail"))
								}
							}))
							t.Cleanup(func() { require.NoError(t, db.Callback().Create().Remove("credit:rt-estimate-storage")) })
						} else if test.storageFailure {
							require.NoError(t, db.Callback().Update().Register("credit:rt-budget-storage", func(tx *gorm.DB) {
								if tx.Statement.Schema != nil && tx.Statement.Schema.Name == "Token" {
									var submitted model.CreditRequest
									result := tx.Session(&gorm.Session{NewDB: true}).Where("user_id = ? AND submitted_at > 0", u.Id).Limit(1).Find(&submitted)
									if result.Error == nil && result.RowsAffected > 0 {
										tx.AddError(errors.New("fixture private realtime storage detail"))
									}
								}
							}))
							t.Cleanup(func() { require.NoError(t, db.Callback().Update().Remove("credit:rt-budget-storage")) })
						}
						e := gin.New()
						e.Use(middleware.RequestId())
						done := make(chan struct{})
						e.GET("/v1/realtime", middleware.TokenAuth(), middleware.Distribute(), func(c *gin.Context) { defer close(done); Relay(c, types.RelayFormatOpenAIRealtime) })
						gateway := httptest.NewServer(e)
						defer gateway.Close()
						client, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(gateway.URL, "http")+"/v1/realtime?model="+modelName, http.Header{"Authorization": {"Bearer sk-" + key.Key}})
						require.NoError(t, err)
						defer client.Close()
						require.NoError(t, client.SetReadDeadline(time.Now().Add(10*time.Second)))
						var output strings.Builder
						for {
							_, body, err := client.ReadMessage()
							if err != nil {
								break
							}
							output.Write(body)
							if (test.clientRejected || test.clientAccepted) && strings.Contains(string(body), `"event_id":"first"`) {
								input := denied
								if test.clientAccepted {
									input = allowed
								}
								require.NoError(t, client.WriteMessage(websocket.TextMessage, []byte(fmt.Sprintf(`{"event_id":"new-input","type":"conversation.item.create","item":{"type":"message","role":"user","content":[{"type":"input_text","text":%q}]}}`, input))))
							}
						}
						select {
						case <-done:
						case <-time.After(10 * time.Second):
							require.FailNow(t, "realtime budget request did not finish")
						}
						select {
						case <-closed:
						case <-time.After(10 * time.Second):
							require.FailNow(t, "gateway did not close realtime upstream")
						}
						assert.EqualValues(t, 1, calls.Load())
						assert.False(t, inputForwarded.Load(), "input rejected by budget was never supplied to upstream")
						var bill model.CreditRequest
						require.NoError(t, db.Where("user_id = ?", u.Id).First(&bill).Error)
						if test.storageFailure {
							assert.Contains(t, output.String(), "stream_budget_unavailable")
							assert.NotContains(t, output.String(), "private realtime storage detail")
							assert.Equal(t, "review", bill.State)
							assert.Empty(t, bill.IntentKind)
							assert.Zero(t, bill.UsageEvidenceID)
							assert.Zero(t, bill.Charged)
						} else {
							require.Equal(t, "settled", bill.State)
							if test.healthy {
								if !test.clientAccepted {
									assert.Contains(t, output.String(), denied)
								}
								assert.Contains(t, output.String(), `"type":"response.done"`)
								expected := int64(300)
								if test.clientAccepted {
									expected = 8
								}
								assert.Equal(t, expected, bill.Actual)
								assert.Equal(t, expected, bill.Charged)
								assert.NotContains(t, output.String(), "quota_budget_exhausted")
								assert.Zero(t, bill.Uncollected)
							} else {
								assert.Contains(t, output.String(), allowed)
								assert.Contains(t, output.String(), "quota_budget_exhausted")
								expected := int64(292) // explicit two text delta estimates under the native heuristic
								if test.audio {
									expected = 556
								} // PCM16 0.1s and 10s -> 1 + 138 native audio tokens, at 4 quota/token
								charged := int64(50)
								if test.priorReceipt {
									expected += 10
								}
								if test.clientRejected {
									expected, charged = 4, 4
								}
								assert.Equal(t, expected, bill.Actual)
								assert.Equal(t, charged, bill.Charged)
								assert.Equal(t, expected-charged, bill.Uncollected)
							}
							var row model.CreditUsageEvidence
							require.NoError(t, db.First(&row, bill.UsageEvidenceID).Error)
							var evidence model.CreditEvidenceInput
							require.NoError(t, common.UnmarshalJsonStr(row.Payload, &evidence))
							assert.NotZero(t, evidence.AttemptPriceEvidenceID)
							if test.priorReceipt {
								index := slices.IndexFunc(evidence.Facts, func(fact hosttypes.UsageFact) bool { return fact.Field == "realtime_reported.completion_tokens" })
								require.NotEqual(t, -1, index)
								require.NotNil(t, evidence.Facts[index].Quantity)
								assert.Equal(t, float64(5), *evidence.Facts[index].Quantity)
								assert.Equal(t, "upstream", evidence.Facts[index].Source)
							}
							var observations []model.CreditUsageEvidence
							require.NoError(t, db.Where("request_id = ? AND stage = ?", bill.ID, "estimate").Order("sequence asc").Find(&observations).Error)
							expectedFrames := 2
							if test.healthy && !test.clientAccepted {
								expectedFrames = 3
							} else if test.clientRejected {
								expectedFrames = 1
							}
							require.Len(t, observations, expectedFrames, "each counted frame retains its own estimator; rejected input and acknowledgements are excluded")
							for i, observation := range observations {
								var counted model.CreditEvidenceInput
								require.NoError(t, common.UnmarshalJsonStr(observation.Payload, &counted))
								require.Len(t, counted.Facts, 1)
								fact := counted.Facts[0]
								assert.Equal(t, "estimate", fact.Source)
								require.NotNil(t, fact.Quantity)
								require.NotNil(t, fact.Estimation)
								assert.Equal(t, modelName, fact.Estimation.Model)
								assert.Equal(t, *fact.Quantity, fact.Estimation.Quantity)
								assert.NotEmpty(t, fact.Estimation.Version)
								if test.audio {
									assert.Equal(t, "realtime-audio-seconds", fact.Estimation.Method)
									assert.Equal(t, float64(24000), fact.Estimation.Parameters["sample_rate"])
									assert.Equal(t, float64(2), fact.Estimation.Parameters["bytes_per_sample"])
									assert.Equal(t, []float64{1, 138}[i], *fact.Quantity)
								} else {
									assert.Equal(t, "provider-heuristic", fact.Estimation.Method)
									assert.Equal(t, float64(1.02), fact.Estimation.Parameters["word"])
								}
								assert.NotContains(t, observation.Payload, denied)
							}
							if !test.healthy {
								assert.Contains(t, evidence.Consume.Other, `"budget_stop":"quota_budget_exhausted"`)
								index := slices.IndexFunc(evidence.Facts, func(fact hosttypes.UsageFact) bool { return fact.Field == "completion_tokens" })
								require.NotEqual(t, -1, index)
								assert.Equal(t, "estimate", evidence.Facts[index].Source)
							}
						}
						if !test.healthy {
							assert.NotContains(t, output.String(), denied)
							assert.NotContains(t, output.String(), "unobserved tail")
							assert.NotContains(t, output.String(), `"native-budget-response"`)
							if !test.priorReceipt {
								assert.NotContains(t, output.String(), `"type":"response.done"`)
							}
						}
						require.NoError(t, db.First(&key, key.Id).Error)
						if test.storageFailure {
							assert.EqualValues(t, amount-bill.Reserved, key.RemainQuota)
							assert.EqualValues(t, bill.Reserved, key.UsedQuota)
						} else {
							assert.EqualValues(t, bill.Charged, key.UsedQuota)
							assert.EqualValues(t, amount-bill.Charged, key.RemainQuota)
						}
						packs, err := model.ListCreditPacks(db, u.Id, now)
						require.NoError(t, err)
						require.Len(t, packs, 1)
						assert.Equal(t, bill.Charged, packs[0].Spent)
						if test.storageFailure {
							assert.Equal(t, bill.Reserved, packs[0].Held)
						} else {
							assert.Zero(t, packs[0].Held)
						}
						differences, err := model.ReconcileCreditAccount(db, u.Id)
						require.NoError(t, err)
						assert.Empty(t, differences)
					})
				}
			})
			t.Run("realtime_cumulative_hold_and_single_settlement", func(t *testing.T) {
				withTieredBillingConfig(t, map[string]string{"credit-realtime": "tiered_expr"}, map[string]string{"credit-realtime": `tier("audio", p * 2 + c * 4 + ai * 6 + ao * 8)`})
				upgrader := websocket.Upgrader{}
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					conn, err := upgrader.Upgrade(w, r, nil)
					if !assert.NoError(t, err) {
						return
					}
					defer conn.Close()
					for _, id := range []string{"first", "first", "second"} {
						message := fmt.Sprintf(`{"event_id":%q,"type":"response.done","response":{"usage":{"total_tokens":14,"input_tokens":10,"output_tokens":4,"input_token_details":{"text_tokens":8,"audio_tokens":2},"output_token_details":{"text_tokens":3,"audio_tokens":1}}}}`, id)
						if !assert.NoError(t, conn.WriteMessage(websocket.TextMessage, []byte(message))) {
							return
						}
					}
					assert.NoError(t, conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, "completed"), time.Now().Add(5*time.Second)))
				}))
				defer upstream.Close()
				u := model.User{Username: "realtime-credit", AffCode: "realtime-credit", Group: "default", Status: common.UserStatusEnabled, AccountingVersion: 1, Setting: `{"billing_preference":"wallet_only"}`}
				require.NoError(t, db.Create(&u).Error)
				key := model.Token{UserId: u.Id, Key: strings.Repeat("r", 48), Status: common.TokenStatusEnabled, RemainQuota: 100, ExpiredTime: -1, Group: "default"}
				require.NoError(t, db.Create(&key).Error)
				_, err := model.GrantCreditPack(db, model.CreditGrant{UserID: u.Id, SourceType: "test", SourceID: "realtime", Amount: 100, StartsAt: now, ExpiresAt: now + 3600, UseMask: model.CreditUseAPI}, now)
				require.NoError(t, err)
				ch := model.Channel{Name: "realtime", Type: constant.ChannelTypeOpenAI, Key: "test", Status: common.ChannelStatusEnabled, Group: "default", Models: "credit-realtime", BaseURL: &upstream.URL}
				require.NoError(t, db.Create(&ch).Error)
				require.NoError(t, db.Create(&model.Ability{ChannelId: ch.Id, Model: "credit-realtime", Group: "default", Enabled: true}).Error)
				e := gin.New()
				e.Use(middleware.RequestId())
				done := make(chan struct{})
				e.GET("/v1/realtime", middleware.TokenAuth(), middleware.Distribute(), func(c *gin.Context) { defer close(done); Relay(c, types.RelayFormatOpenAIRealtime) })
				gateway := httptest.NewServer(e)
				defer gateway.Close()
				header := http.Header{"Authorization": []string{"Bearer sk-" + key.Key}}
				client, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(gateway.URL, "http")+"/v1/realtime?model=credit-realtime", header)
				require.NoError(t, err)
				defer client.Close()
				require.NoError(t, client.SetReadDeadline(time.Now().Add(10*time.Second)))
				var messages []string
				for {
					_, body, err := client.ReadMessage()
					if err != nil {
						break
					}
					messages = append(messages, string(body))
				}
				select {
				case <-done:
				case <-time.After(10 * time.Second):
					require.FailNow(t, "realtime request did not finish")
				}
				require.Len(t, messages, 3, fmt.Sprint(messages))
				var bill model.CreditRequest
				require.NoError(t, db.Where("user_id = ?", u.Id).First(&bill).Error)
				assert.Equal(t, "settled", bill.State)
				assert.EqualValues(t, 48, bill.Reserved)
				assert.EqualValues(t, 48, bill.Charged, "audio categories are priced once; duplicate receipt is not another operation")
				assert.NotZero(t, bill.UsageEvidenceID)
				var pending model.CreditLogOutbox
				require.NoError(t, db.Where("request_id = ?", bill.ID).First(&pending).Error)
				var realtimeLog model.Log
				require.NoError(t, common.UnmarshalJsonStr(pending.Payload, &realtimeLog))
				assert.Equal(t, 20, realtimeLog.PromptTokens, "two distinct response receipts, each with 10 input tokens; replay of the first is excluded")
				assert.Equal(t, 8, realtimeLog.CompletionTokens)
				require.NoError(t, db.First(&key, key.Id).Error)
				assert.Equal(t, 52, key.RemainQuota)
				assert.Equal(t, 48, key.UsedQuota)
			})
			t.Run("midjourney_holds_then_settles_without_legacy_balance", func(t *testing.T) {
				require.NoError(t, db.AutoMigrate(&model.Midjourney{}))
				previousPrices := ratio_setting.ModelPrice2JSONString()
				t.Cleanup(func() { require.NoError(t, ratio_setting.UpdateModelPriceByJSONString(previousPrices)) })
				require.NoError(t, ratio_setting.UpdateModelPriceByJSONString(`{"mj_imagine":0.00006}`))
				var calls atomic.Int32
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					w.Header().Set("Content-Type", "application/json")
					var err error
					if calls.Load() == 2 {
						_, err = fmt.Fprint(w, `{"code":21,"result":"credit-mj-sync-job","properties":{"imageUrl":"https://example.invalid/image","status":"SUCCESS"}}`)
					} else {
						_, err = fmt.Fprint(w, `{"code":1,"description":"accepted","result":"credit-mj-job"}`)
					}
					assert.NoError(t, err)
				}))
				defer upstream.Close()
				u := model.User{Username: "mj-credit", AffCode: "mj-credit", Group: "default", Status: common.UserStatusEnabled, AccountingVersion: 1, Setting: `{"billing_preference":"wallet_only"}`}
				require.NoError(t, db.Create(&u).Error)
				key := model.Token{UserId: u.Id, Key: strings.Repeat("m", 48), Status: common.TokenStatusEnabled, RemainQuota: 100, ExpiredTime: -1, Group: "default"}
				require.NoError(t, db.Create(&key).Error)
				_, err := model.GrantCreditPack(db, model.CreditGrant{UserID: u.Id, SourceType: "test", SourceID: "mj", Amount: 100, StartsAt: now, ExpiresAt: now + 3600, UseMask: model.CreditUseAPI}, now)
				require.NoError(t, err)
				ch := model.Channel{Name: "mj", Type: constant.ChannelTypeMidjourney, Key: "test", Status: common.ChannelStatusEnabled, Group: "default", Models: "mj_imagine", BaseURL: &upstream.URL}
				require.NoError(t, db.Create(&ch).Error)
				require.NoError(t, db.Create(&model.Ability{ChannelId: ch.Id, Model: "mj_imagine", Group: "default", Enabled: true}).Error)
				e := gin.New()
				e.Use(middleware.RequestId())
				e.POST("/mj/submit/imagine", middleware.TokenAuth(), middleware.Distribute(), RelayMidjourney)
				r := httptest.NewRequest(http.MethodPost, "/mj/submit/imagine", strings.NewReader(`{"prompt":"cat"}`))
				r.Header.Set("Authorization", "Bearer sk-"+key.Key)
				r.Header.Set("Content-Type", "application/json")
				w := httptest.NewRecorder()
				e.ServeHTTP(w, r)
				require.Equal(t, http.StatusOK, w.Code, w.Body.String())
				assert.EqualValues(t, 1, calls.Load())
				var task model.Midjourney
				require.NoError(t, db.Where("user_id = ?", u.Id).First(&task).Error)
				assert.NotZero(t, task.CreditRequestID)
				packs, err := model.ListCreditPacks(db, u.Id, now)
				require.NoError(t, err)
				assert.EqualValues(t, 30, packs[0].Held)
				assert.Zero(t, packs[0].Spent)
				task.Status = "SUCCESS"
				require.NoError(t, task.Update())
				require.NoError(t, db.Callback().Create().Before("gorm:create").Register("test:mj-terminal-evidence", func(tx *gorm.DB) {
					if evidence, ok := tx.Statement.Dest.(*model.CreditUsageEvidence); ok && evidence.Stage == "settlement" {
						tx.AddError(fmt.Errorf("Midjourney terminal evidence unavailable"))
					}
				}))
				require.Error(t, service.CompleteMidjourneyCreditBilling(&task))
				require.NoError(t, db.Callback().Create().Remove("test:mj-terminal-evidence"))
				var incomplete model.CreditRequest
				require.NoError(t, db.First(&incomplete, task.CreditRequestID).Error)
				require.Zero(t, incomplete.UsageEvidenceID)
				require.Empty(t, incomplete.IntentKind)
				require.NoError(t, ratio_setting.UpdateModelPriceByJSONString(`{"mj_imagine":1}`))
				summary, err := service.RunCreditRecoveryPass(taskSubmissionTestContext(), db, db, "mj-terminal-recovery", common.GetTimestamp())
				require.NoError(t, err)
				assert.Zero(t, summary.Errors)
				require.NoError(t, db.First(&incomplete, incomplete.ID).Error)
				assert.Equal(t, "settled", incomplete.State, "SUCCESS saved before evidence is recovered without polling again")
				assert.EqualValues(t, 30, incomplete.Actual, "later administrator prices cannot replace the submitted per-call fee")
				assert.EqualValues(t, 30, incomplete.Charged)
				require.NotZero(t, incomplete.UsageEvidenceID)
				require.NoError(t, ratio_setting.UpdateModelPriceByJSONString(`{"mj_imagine":0.00006}`))
				require.NoError(t, service.CompleteMidjourneyCreditBilling(&task))
				require.NoError(t, service.CompleteMidjourneyCreditBilling(&task))
				var receiptBill model.CreditRequest
				require.NoError(t, db.First(&receiptBill, task.CreditRequestID).Error)
				assert.NotZero(t, receiptBill.UsageEvidenceID, "Midjourney also persists terminal metering")
				require.NoError(t, db.First(&key, key.Id).Error)
				assert.Equal(t, 70, key.RemainQuota)
				assert.Equal(t, 30, key.UsedQuota)
				packs, err = model.ListCreditPacks(db, u.Id, now)
				require.NoError(t, err)
				assert.Zero(t, packs[0].Held)
				assert.EqualValues(t, 30, packs[0].Spent)
				r = httptest.NewRequest(http.MethodPost, "/mj/submit/imagine", strings.NewReader(`{"prompt":"another cat"}`))
				r.Header.Set("Authorization", "Bearer sk-"+key.Key)
				r.Header.Set("Content-Type", "application/json")
				w = httptest.NewRecorder()
				e.ServeHTTP(w, r)
				require.Equal(t, http.StatusOK, w.Code, w.Body.String())
				require.NoError(t, db.First(&u, u.Id).Error)
				assert.Equal(t, 60, u.UsedQuota, "immediate completion must not run the old statistics path too")
				assert.Equal(t, 2, u.RequestCount)
				require.NoError(t, db.First(&key, key.Id).Error)
				assert.Equal(t, 40, key.RemainQuota)
				assert.Equal(t, 60, key.UsedQuota)
				for _, id := range []string{"CaseSensitive", "casesensitive"} {
					bill, err := model.BeginCreditRequest(db, model.CreditRequestInput{UserID: u.Id, RequestID: id, ModelName: "mj_imagine", Protocol: "mj_proxy", PriceSnapshot: `{}`, TokenID: key.Id, Amount: 5}, now)
					require.NoError(t, err)
					require.NoError(t, model.MarkCreditRequestSubmitted(db, u.Id, bill.ID, now))
					job := model.Midjourney{UserId: u.Id, MjId: id, Status: "SUCCESS", Quota: 5, TokenId: key.Id, CreditRequestID: bill.ID}
					require.NoError(t, job.Insert())
					require.NoError(t, service.CompleteMidjourneyCreditBilling(&job))
				}
				bill, err := model.BeginCreditRequest(db, model.CreditRequestInput{UserID: u.Id, RequestID: "mj-over-budget", ModelName: "mj_imagine", Protocol: "mj_proxy", PriceSnapshot: `{}`, TokenID: key.Id, Amount: 5}, now)
				require.NoError(t, err)
				require.NoError(t, model.MarkCreditRequestSubmitted(db, u.Id, bill.ID, now))
				job := model.Midjourney{UserId: u.Id, MjId: "mj-over-budget", Status: "SUCCESS", Quota: 50, TokenId: key.Id, CreditRequestID: bill.ID}
				require.NoError(t, job.Insert())
				lease, err := model.ClaimCreditExecution(db, u.Id, bill.ID, "mj-completer", 120, now, common.GetTimestamp)
				require.NoError(t, err)
				job.CreditExecution = []model.CreditExecution{lease}
				require.NoError(t, db.Callback().Update().Before("gorm:update").Register("test:mj-intent", func(tx *gorm.DB) {
					if values, ok := tx.Statement.Dest.(map[string]any); ok && values["intent_kind"] == "settle" {
						tx.AddError(fmt.Errorf("intent store unavailable"))
					}
				}))
				require.Error(t, service.CompleteMidjourneyCreditBilling(&job))
				require.NoError(t, db.Callback().Update().Remove("test:mj-intent"))
				require.NoError(t, db.First(&bill, bill.ID).Error)
				require.NotZero(t, bill.UsageEvidenceID)
				require.Empty(t, bill.IntentKind)
				receiptID := bill.UsageEvidenceID
				require.NoError(t, service.CompleteMidjourneyCreditBilling(&job))
				assert.Equal(t, 30, job.Quota)
				require.NoError(t, db.First(&job, job.Id).Error)
				assert.Equal(t, 30, job.Quota)
				require.NoError(t, service.CompleteMidjourneyCreditBilling(&job), "reloaded charged projection is a valid replay")
				require.NoError(t, db.First(&bill, bill.ID).Error)
				assert.Equal(t, receiptID, bill.UsageEvidenceID)
				assert.EqualValues(t, 50, bill.Actual)
				assert.EqualValues(t, 30, bill.Charged)
				assert.EqualValues(t, 20, bill.Uncollected)
				actor := model.User{Username: "mj-adjust-admin", Password: "fixture", AffCode: "mj-adjust-admin", Status: common.UserStatusEnabled, Role: common.RoleRootUser}
				require.NoError(t, db.Create(&actor).Error)
				stale := job
				quantity := float64(1)
				correction, err := model.AdjustCreditBill(db, model.CreditBillAdjustmentInput{UserID: u.Id, RequestID: bill.ID, ActorID: actor.Id, EventID: "mj-corrected-receipt", ReferenceQuota: 10, EvidenceVersion: "verified-fixture-v1", Facts: []hosttypes.UsageFact{{Field: "request_count", Unit: "request", Quantity: &quantity, Source: "adaptor"}}, Reason: "verified corrected receipt"}, common.GetTimestamp())
				require.NoError(t, err)
				assert.EqualValues(t, 20, correction.Refunded)
				require.NoError(t, service.CompleteMidjourneyCreditBilling(&job), "old completion can replay after correction")
				assert.Equal(t, 10, job.Quota)
				require.NoError(t, db.First(&job, job.Id).Error)
				require.NoError(t, service.CompleteMidjourneyCreditBilling(&job), "reloaded corrected fee can replay")
				assert.Equal(t, 10, job.Quota)
				require.NoError(t, stale.UpdateBillingState())
				require.NoError(t, stale.Update())
				_, err = stale.UpdateWithStatus(stale.Status)
				require.NoError(t, err)
				require.NoError(t, db.First(&job, job.Id).Error)
				assert.Equal(t, 10, job.Quota, "late Midjourney projection cannot overwrite corrected account charge")
			})
			t.Run("responses_websocket_every_create_has_its_own_submitted_bill", func(t *testing.T) {
				withTieredBillingConfig(t, map[string]string{"credit-ws": "tiered_expr"}, map[string]string{"credit-ws": `tier("request", fixed(0.00002))`})
				var handshakes atomic.Int32
				upgrader := websocket.Upgrader{}
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					conn, err := upgrader.Upgrade(w, r, nil)
					if !assert.NoError(t, err) {
						return
					}
					defer conn.Close()
					handshakes.Add(1)
					for i := range 2 {
						_, _, err := conn.ReadMessage()
						if !assert.NoError(t, err) {
							return
						}
						body := fmt.Sprintf(`{"type":"response.completed","response":{"id":"resp_%d","object":"response","status":"completed","model":"credit-ws","output":[],"usage":{"input_tokens":10,"output_tokens":%d,"total_tokens":%d,"input_tokens_details":{"cached_tokens":4}}}}`, i, i*5, 10+i*5)
						if !assert.NoError(t, conn.WriteMessage(websocket.TextMessage, []byte(body))) {
							return
						}
					}
					_, _, _ = conn.ReadMessage()
				}))
				defer upstream.Close()
				u := model.User{Username: "ws-credit", AffCode: "ws-credit", Group: "default", Status: common.UserStatusEnabled, AccountingVersion: 1, Setting: `{"billing_preference":"wallet_only"}`}
				require.NoError(t, db.Create(&u).Error)
				key := model.Token{UserId: u.Id, Key: strings.Repeat("w", 48), Status: common.TokenStatusEnabled, RemainQuota: 100, ExpiredTime: -1, Group: "default"}
				require.NoError(t, db.Create(&key).Error)
				_, err := model.GrantCreditPack(db, model.CreditGrant{UserID: u.Id, SourceType: "test", SourceID: "ws", Amount: 100, StartsAt: now, ExpiresAt: now + 3600, UseMask: model.CreditUseAPI}, now)
				require.NoError(t, err)
				ch := model.Channel{Name: "ws", Type: constant.ChannelTypeOpenAI, Key: "test", Status: common.ChannelStatusEnabled, Group: "default", Models: "credit-ws", BaseURL: &upstream.URL}
				ch.SetSetting(dto.ChannelSettings{ResponsesWebSocketEnabled: true})
				require.NoError(t, db.Create(&ch).Error)
				require.NoError(t, db.Create(&model.Ability{ChannelId: ch.Id, Model: "credit-ws", Group: "default", Enabled: true}).Error)
				e := gin.New()
				e.Use(middleware.RequestId())
				done := make(chan struct{})
				e.GET("/v1/responses", middleware.TokenAuth(), func(c *gin.Context) { defer close(done); ResponsesWebSocket(c) })
				gateway := httptest.NewServer(e)
				defer gateway.Close()
				client, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(gateway.URL, "http")+"/v1/responses", http.Header{"Authorization": []string{"Bearer sk-" + key.Key}})
				require.NoError(t, err)
				defer client.Close()
				require.NoError(t, client.SetReadDeadline(time.Now().Add(10*time.Second)))
				for range 2 {
					require.NoError(t, client.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"credit-ws","input":"hi"}`)))
					_, body, err := client.ReadMessage()
					require.NoError(t, err)
					assert.Contains(t, string(body), "response.completed")
				}
				require.NoError(t, client.Close())
				select {
				case <-done:
				case <-time.After(10 * time.Second):
					require.FailNow(t, "responses websocket did not stop")
				}
				var bills []model.CreditRequest
				require.NoError(t, db.Where("user_id = ?", u.Id).Order("id").Find(&bills).Error)
				require.Len(t, bills, 2)
				for i, bill := range bills {
					assert.Equal(t, "settled", bill.State)
					assert.NotZero(t, bill.SubmittedAt)
					assert.EqualValues(t, 10, bill.Charged)
					require.NotZero(t, bill.UsageEvidenceID)
					var evidence model.CreditUsageEvidence
					require.NoError(t, db.First(&evidence, bill.UsageEvidenceID).Error)
					var snapshot model.CreditEvidenceInput
					require.NoError(t, common.UnmarshalJsonStr(evidence.Payload, &snapshot))
					assert.Equal(t, 10, snapshot.Consume.PromptTokens)
					assert.Equal(t, i*5, snapshot.Consume.CompletionTokens)
					for _, field := range []string{"prompt_tokens", "completion_tokens", "cached_tokens"} {
						fact := slices.IndexFunc(snapshot.Facts, func(fact hosttypes.UsageFact) bool { return fact.Field == field })
						require.NotEqual(t, -1, fact)
						assert.Equal(t, "upstream", snapshot.Facts[fact].Source)
					}
				}
				assert.NotEqual(t, bills[0].RequestID, bills[1].RequestID)
				assert.EqualValues(t, 1, handshakes.Load())
				require.NoError(t, db.First(&key, key.Id).Error)
				assert.Equal(t, 80, key.RemainQuota)
				assert.Equal(t, 20, key.UsedQuota)
			})
			t.Run("responses_websocket_budget_stops_before_output_without_debt", func(t *testing.T) {
				for index, test := range []struct {
					name                        string
					amount                      int64
					stop, storageFailure, fixed bool
				}{
					{name: "insufficient", amount: 50, stop: true},
					{name: "storage_failure", amount: 50, stop: true, storageFailure: true},
					{name: "healthy", amount: 1000},
					{name: "fixed", amount: 50, fixed: true},
				} {
					t.Run(test.name, func(t *testing.T) {
						modelName := "credit-ws-budget-" + test.name
						expression := `tier("tokens", c * 4)`
						if test.fixed {
							expression = `tier("request", fixed(0.00002))`
						}
						withTieredBillingConfig(t, map[string]string{modelName: "tiered_expr"}, map[string]string{modelName: expression})
						denied := strings.Repeat("denied ", 100)
						var creates atomic.Int32
						upstreamClosed := make(chan struct{})
						upgrader := websocket.Upgrader{}
						upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
							conn, err := upgrader.Upgrade(w, r, nil)
							if !assert.NoError(t, err) {
								return
							}
							defer conn.Close()
							defer close(upstreamClosed)
							_, _, err = conn.ReadMessage()
							if !assert.NoError(t, err) {
								return
							}
							creates.Add(1)
							for _, body := range []string{
								`{"type":"response.created","response":{"id":"ws_budget","status":"in_progress","usage":{"input_tokens":0,"output_tokens":0,"total_tokens":0}}}`,
								`{"type":"response.output_text.delta","delta":"allowed "}`,
								`{"type":"response.output_text.delta","delta":` + fmt.Sprintf("%q", denied) + `}`,
								`{"type":"response.completed","response":{"id":"ws_budget","status":"completed","output":[],"usage":{"input_tokens":0,"output_tokens":150,"total_tokens":150}}}`,
							} {
								if err := conn.WriteMessage(websocket.TextMessage, []byte(body)); err != nil {
									return
								}
							}
							_, _, _ = conn.ReadMessage()
						}))
						defer upstream.Close()
						u := model.User{Username: fmt.Sprintf("wsbudget%02d", index), AffCode: fmt.Sprintf("wsbudget%02d", index), Group: "default", Status: common.UserStatusEnabled, AccountingVersion: 1, Setting: `{"billing_preference":"wallet_only"}`}
						require.NoError(t, db.Create(&u).Error)
						key := model.Token{UserId: u.Id, Key: "wsbudget" + test.name + strings.Repeat("0", 48-len("wsbudget"+test.name)), Status: common.TokenStatusEnabled, RemainQuota: int(test.amount), ExpiredTime: -1, Group: "default"}
						require.NoError(t, db.Create(&key).Error)
						_, err := model.GrantCreditPack(db, model.CreditGrant{UserID: u.Id, SourceType: "test", SourceID: "ws-budget", Amount: test.amount, StartsAt: now, ExpiresAt: now + 3600, UseMask: model.CreditUseAPI}, now)
						require.NoError(t, err)
						ch := model.Channel{Name: modelName, Type: constant.ChannelTypeOpenAI, Key: "test", Status: common.ChannelStatusEnabled, Group: "default", Models: modelName, BaseURL: &upstream.URL}
						ch.SetSetting(dto.ChannelSettings{ResponsesWebSocketEnabled: true})
						require.NoError(t, db.Create(&ch).Error)
						require.NoError(t, db.Create(&model.Ability{ChannelId: ch.Id, Model: modelName, Group: "default", Enabled: true}).Error)
						if test.storageFailure {
							require.NoError(t, db.Callback().Update().Register("credit:ws-budget-storage", func(tx *gorm.DB) {
								if tx.Statement.Schema == nil || tx.Statement.Schema.Name != "Token" {
									return
								}
								var admitted model.CreditRequest
								result := tx.Session(&gorm.Session{NewDB: true}).Where("user_id = ? AND submitted_at > 0", u.Id).Limit(1).Find(&admitted)
								if result.Error == nil && result.RowsAffected > 0 {
									tx.AddError(errors.New("private websocket storage detail"))
								}
							}))
							t.Cleanup(func() { require.NoError(t, db.Callback().Update().Remove("credit:ws-budget-storage")) })
						}
						engine := gin.New()
						engine.Use(middleware.RequestId())
						done := make(chan struct{})
						engine.GET("/v1/responses", middleware.TokenAuth(), func(c *gin.Context) { defer close(done); ResponsesWebSocket(c) })
						gateway := httptest.NewServer(engine)
						defer gateway.Close()
						client, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(gateway.URL, "http")+"/v1/responses", http.Header{"Authorization": []string{"Bearer sk-" + key.Key}})
						require.NoError(t, err)
						defer client.Close()
						require.NoError(t, client.SetReadDeadline(time.Now().Add(10*time.Second)))
						require.NoError(t, client.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","event_id":"budget-create","model":`+fmt.Sprintf("%q", modelName)+`,"input":"hi"}`)))
						var output strings.Builder
						terminal := ""
						for {
							_, body, err := client.ReadMessage()
							require.NoError(t, err, output.String())
							output.Write(body)
							var event struct {
								Type string `json:"type"`
							}
							require.NoError(t, common.Unmarshal(body, &event))
							if event.Type == "error" || event.Type == "response.completed" {
								terminal = event.Type
								break
							}
						}
						if test.stop {
							assert.Equal(t, "error", terminal)
							assert.NotContains(t, output.String(), denied)
							assert.NotContains(t, output.String(), "response.completed")
							code := "quota_budget_exhausted"
							if test.storageFailure {
								code = "stream_budget_unavailable"
							}
							assert.Contains(t, output.String(), code)
							assert.Contains(t, output.String(), "budget-create")
							assert.NotContains(t, output.String(), "private websocket storage detail")
							_, unexpected, err := client.ReadMessage()
							assert.Error(t, err, "budget-stopped connection must close before another create: %s", unexpected)
							select {
							case <-upstreamClosed:
							case <-time.After(10 * time.Second):
								require.FailNow(t, "gateway must close upstream before test closes client")
							}
						} else {
							assert.Equal(t, "response.completed", terminal)
							assert.Contains(t, output.String(), denied)
						}
						require.NoError(t, client.Close())
						select {
						case <-done:
						case <-time.After(10 * time.Second):
							require.FailNow(t, "websocket worker did not stop")
						}
						select {
						case <-upstreamClosed:
						case <-time.After(10 * time.Second):
							require.FailNow(t, "upstream websocket did not stop")
						}
						assert.EqualValues(t, 1, creates.Load())
						var bill model.CreditRequest
						require.NoError(t, db.Where("user_id = ?", u.Id).First(&bill).Error)
						require.NoError(t, db.First(&key, key.Id).Error)
						if test.storageFailure {
							assert.Equal(t, "review", bill.State)
							assert.Empty(t, bill.IntentKind)
							assert.Zero(t, bill.Charged)
							assert.Zero(t, bill.UsageEvidenceID)
							assert.EqualValues(t, test.amount-bill.Reserved, key.RemainQuota)
							assert.EqualValues(t, bill.Reserved, key.UsedQuota)
						} else {
							fee := int64(300)
							if test.stop {
								fee = int64(service.CountTextToken("allowed "+denied, modelName)) * 2
							}
							if test.fixed {
								fee = 10
							}
							assert.Equal(t, "settled", bill.State)
							assert.Equal(t, fee, bill.Actual)
							assert.Equal(t, min(test.amount, fee), bill.Charged)
							assert.Equal(t, fee-min(test.amount, fee), bill.Uncollected)
							assert.EqualValues(t, test.amount-bill.Charged, key.RemainQuota)
							assert.EqualValues(t, bill.Charged, key.UsedQuota)
							var evidence model.CreditUsageEvidence
							require.NoError(t, db.First(&evidence, bill.UsageEvidenceID).Error)
							var receipt model.CreditEvidenceInput
							require.NoError(t, common.UnmarshalJsonStr(evidence.Payload, &receipt))
							assert.NotZero(t, receipt.AttemptPriceEvidenceID)
							if test.stop {
								field := slices.IndexFunc(receipt.Facts, func(f hosttypes.UsageFact) bool { return f.Field == "completion_tokens" })
								require.NotEqual(t, -1, field)
								assert.Equal(t, "estimate", receipt.Facts[field].Source)
								assert.NotNil(t, receipt.Facts[field].Estimation)
							}
							if test.stop {
								assert.Contains(t, receipt.Consume.Other, `"budget_stop":"quota_budget_exhausted"`)
							}
						}
						packs, err := model.ListCreditPacks(db, u.Id, common.GetTimestamp())
						require.NoError(t, err)
						require.Len(t, packs, 1)
						if test.storageFailure {
							assert.Equal(t, bill.Reserved, packs[0].Held)
						} else {
							assert.Zero(t, packs[0].Held)
						}
						differences, err := model.ReconcileCreditAccount(db, u.Id)
						require.NoError(t, err)
						assert.Empty(t, differences)
					})
				}
			})

			t.Run("credit_admin_API_contract", func(t *testing.T) {
				actor := model.User{Username: "api-credit-admin", Password: "unused", Role: common.RoleRootUser, Status: common.UserStatusEnabled, AffCode: "api-credit-admin"}
				require.NoError(t, db.Create(&actor).Error)
				require.NoError(t, db.AutoMigrate(&model.UserAccessToken{}, &model.UserSession{}))
				adminAPI := gin.New()
				adminAPI.Use(middleware.RequestId())
				policyAPI := adminAPI.Group("/api/credit/admin", middleware.RootAuth())
				policyAPI.GET("/policies", AdminListCreditPolicies)
				policyAPI.PUT("/policies", AdminPutCreditPolicy)
				financeAPI := adminAPI.Group("/api/credit/admin", middleware.AdminAuth())
				financeAPI.POST("/grants", AdminGrantCredit)
				financeAPI.POST("/bills/adjustments", AdminAdjustCreditBill)
				financeAPI.POST("/bills/review", AdminApproveCreditBillReview)
				financeAPI.GET("/bills/:id", AdminGetCreditBill)
				financeAPI.GET("/account", AdminGetCreditAccount)
				financeAPI.GET("/bills", AdminListCreditBills)
				ownedBillsAPI := adminAPI.Group("/api/credit", middleware.UserAuth())
				ownedBillsAPI.GET("/bills/:id", GetCreditBill)
				ownedBillsAPI.GET("/account", GetCreditAccount)
				ownedBillsAPI.GET("/bills", ListCreditBills)
				financeAPI.GET("/reviews", AdminListCreditReviews)
				financeAPI.GET("/work", AdminListCreditWork)
				financeAPI.POST("/work/retry", AdminRetryCreditWork)
				financeAPI.GET("/reconcile", AdminReconcileCreditAccount)
				financeAPI.POST("/reviews", AdminOpenCreditReview)
				financeAPI.POST("/reviews/cash-outcome", AdminRecordCreditCashOutcome)
				require.NoError(t, db.AutoMigrate(&model.SubscriptionPlan{}, &model.SubscriptionPlanVersion{}, &model.UserSubscription{}, &model.SubscriptionPurchaseOrder{}, &model.SubscriptionPaymentFact{}, &model.SubscriptionPaymentClaim{}))
				versionPlan := model.SubscriptionPlan{Title: "Version API", StripePriceId: "private-price-contract", PriceAmount: 1, Currency: "USD", DurationUnit: model.SubscriptionDurationMonth, DurationValue: 1, Enabled: true}
				require.NoError(t, db.Create(&versionPlan).Error)
				versionPath := fmt.Sprintf("/api/subscription/admin/plans/%d/versions", versionPlan.Id)
				versionAPI := adminAPI.Group("/api/subscription/admin", middleware.AdminAuth())
				versionAPI.GET("/plans/:id/versions", AdminListSubscriptionPlanVersions)
				versionAPI.POST("/plans/:id/versions", AdminPublishSubscriptionPlanVersion)
				versionAPI.POST("/user_subscriptions/:id/invalidate", AdminInvalidateUserSubscription)
				versionAPI.POST("/plans", AdminCreateSubscriptionPlan)
				versionAPI.PUT("/plans/:id", AdminUpdateSubscriptionPlan)
				versionAPI.GET("/payment-reviews", AdminListSubscriptionPaymentReviews)
				versionAPI.GET("/orders/:id", AdminGetSubscriptionPaymentReview)
				versionAPI.POST("/orders/:id/reconcile", AdminResolveSubscriptionPaymentReview)
				selfAPI := adminAPI.Group("/api/subscription", middleware.UserAuth())
				selfAPI.POST("/balance/pay", SubscriptionRequestBalancePay)
				selfAPI.GET("/self", GetSubscriptionSelf)
				selfAPI.GET("/plans", GetSubscriptionPlans)
				selfAPI.GET("/orders", GetSubscriptionPurchaseOrders)
				selfAPI.GET("/orders/:id", GetSubscriptionPurchaseOrder)
				readOnly, _ := createScopedAccessToken(t, actor.Id, 0, "billing:read", "option:read")
				writeOnly, _ := createScopedAccessToken(t, actor.Id, 0, "billing:write", "option:write")
				for _, endpoint := range []struct{ method, path string }{
					{http.MethodPost, versionPath},
					{http.MethodPost, "/api/credit/admin/grants"},
					{http.MethodPost, "/api/credit/admin/work/retry"},
					{http.MethodPost, "/api/credit/admin/reviews"},
					{http.MethodPost, "/api/credit/admin/reviews/cash-outcome"},
					{http.MethodPut, "/api/credit/admin/policies"},
				} {
					response := accessTokenRequest(adminAPI, endpoint.method, endpoint.path, readOnly, "", `{}`)
					assert.Equal(t, http.StatusForbidden, response.Code, response.Body.String())
					assert.Contains(t, response.Body.String(), "ACCESS_TOKEN_SCOPE_DENIED")
					response = accessTokenRequest(adminAPI, endpoint.method, endpoint.path, writeOnly, "", `{}`)
					assert.Equal(t, http.StatusBadRequest, response.Code, "authorized request reaches input validation: %s", response.Body.String())
				}
				for _, path := range []string{versionPath, "/api/credit/admin/policies", fmt.Sprintf("/api/credit/admin/reviews?user_id=%d", user.Id), fmt.Sprintf("/api/credit/admin/work?user_id=%d", user.Id), fmt.Sprintf("/api/credit/admin/reconcile?user_id=%d", user.Id)} {
					assert.Equal(t, http.StatusOK, accessTokenRequest(adminAPI, http.MethodGet, path, readOnly, "", "").Code)
					assert.Equal(t, http.StatusForbidden, accessTokenRequest(adminAPI, http.MethodGet, path, writeOnly, "", "").Code)
				}
				t.Run("wallet_read_projections", func(t *testing.T) {
					owner := model.User{Username: "wallet-projection", Password: "unused", Role: common.RoleCommonUser, Status: common.UserStatusEnabled, AffCode: "wallet-projection", AccountingVersion: 1}
					require.NoError(t, db.Create(&owner).Error)
					for _, readUserList := range []gin.HandlerFunc{GetAllUsers, SearchUsers} {
						listed := httptest.NewRecorder()
						context, _ := gin.CreateTestContext(listed)
						context.Request = httptest.NewRequest(http.MethodGet, "/api/user/?keyword=wallet-projection", nil)
						readUserList(context)
						require.Equal(t, http.StatusOK, listed.Code)
						var projection struct {
							Data struct {
								Items []struct {
									ID                int `json:"id"`
									AccountingVersion int `json:"accounting_version"`
								} `json:"items"`
							} `json:"data"`
						}
						require.NoError(t, common.Unmarshal(listed.Body.Bytes(), &projection))
						found := false
						for _, entry := range projection.Data.Items {
							if entry.ID == owner.Id {
								found = true
								assert.Equal(t, 1, entry.AccountingVersion, "administrators must not read a credit account as a legacy zero balance")
							}
						}
						require.True(t, found)
					}
					clock := common.GetTimestamp()
					for _, grant := range []model.CreditGrant{
						{SourceID: "private-expired", Amount: 20, StartsAt: clock - 200, ExpiresAt: clock - 100, UseMask: model.CreditUseAPI},
						{SourceID: "private-early", Amount: 40, StartsAt: clock - 1, ExpiresAt: clock + 100, UseMask: model.CreditUseAPI},
						{SourceID: "private-late", Amount: 50, StartsAt: clock - 1, ExpiresAt: clock + 200, UseMask: model.CreditUseSubscription},
						{SourceID: "private-future", Amount: 30, StartsAt: clock + 10, ExpiresAt: clock + 300, UseMask: model.CreditUseAPI},
					} {
						grant.UserID, grant.SourceType, grant.Reason = owner.Id, "test", "private-grant-reason"
						_, err := model.GrantCreditPack(db, grant, clock-201)
						require.NoError(t, err)
					}
					bill, err := model.BeginCreditRequest(db, model.CreditRequestInput{UserID: owner.Id, RequestID: "wallet-bill", ModelName: "model", Protocol: "text", PriceSnapshot: `{"private":"frozen-secret"}`, Playground: true, Amount: 15}, clock)
					require.NoError(t, err)
					ownedRead, _ := createScopedAccessToken(t, owner.Id, 0, "wallet:read")
					response := accessTokenRequest(adminAPI, http.MethodGet, fmt.Sprintf("/api/credit/account?user_id=%d&p=1&page_size=2", user.Id), ownedRead, "", "")
					require.Equal(t, http.StatusOK, response.Code, response.Body.String())
					var account struct {
						Data struct {
							UserID                int   `json:"user_id"`
							AccountingVersion     int   `json:"accounting_version"`
							APIAvailable          int64 `json:"api_available"`
							SubscriptionAvailable int64 `json:"subscription_available"`
							Total                 int64 `json:"total"`
							Packs                 []struct {
								ID        int64  `json:"id"`
								Available int64  `json:"available"`
								Held      int64  `json:"held"`
								Expired   int64  `json:"expired"`
								State     string `json:"state"`
							} `json:"packs"`
						} `json:"data"`
					}
					require.NoError(t, common.Unmarshal(response.Body.Bytes(), &account))
					assert.Equal(t, owner.Id, account.Data.UserID, "self endpoint ignores another user's query parameter")
					assert.Equal(t, 1, account.Data.AccountingVersion)
					assert.EqualValues(t, 25, account.Data.APIAvailable)
					assert.EqualValues(t, 50, account.Data.SubscriptionAvailable)
					assert.EqualValues(t, 4, account.Data.Total)
					require.Len(t, account.Data.Packs, 2)
					assert.Equal(t, "expired", account.Data.Packs[0].State)
					assert.Zero(t, account.Data.Packs[0].Available)
					assert.EqualValues(t, 20, account.Data.Packs[0].Expired)
					assert.EqualValues(t, 15, account.Data.Packs[1].Held)
					assert.NotContains(t, response.Body.String(), "private-")
					bills := accessTokenRequest(adminAPI, http.MethodGet, "/api/credit/bills?page_size=1", ownedRead, "", "")
					require.Equal(t, http.StatusOK, bills.Code, bills.Body.String())
					assert.Contains(t, bills.Body.String(), fmt.Sprintf(`"id":%d`, bill.ID))
					assert.NotContains(t, bills.Body.String(), "frozen-secret")
					assert.NotContains(t, bills.Body.String(), "PriceSnapshot")
					path := fmt.Sprintf("/api/credit/admin/account?user_id=%d", owner.Id)
					assert.Equal(t, http.StatusForbidden, accessTokenRequest(adminAPI, http.MethodGet, path, ownedRead, "", "").Code)
					assert.Equal(t, http.StatusOK, accessTokenRequest(adminAPI, http.MethodGet, path, readOnly, "", "").Code)
					assert.Equal(t, http.StatusForbidden, accessTokenRequest(adminAPI, http.MethodGet, path, writeOnly, "", "").Code)
					assert.Equal(t, http.StatusBadRequest, accessTokenRequest(adminAPI, http.MethodGet, "/api/credit/account?p=-1", ownedRead, "", "").Code)
					require.NoError(t, db.Model(&owner).Update("status", common.UserStatusDisabled).Error)
					assert.Equal(t, http.StatusOK, accessTokenRequest(adminAPI, http.MethodGet, path, readOnly, "", "").Code, "an administrator must still inspect a disabled user's credit packs")
					assert.Equal(t, http.StatusUnauthorized, accessTokenRequest(adminAPI, http.MethodGet, "/api/credit/account", ownedRead, "", "").Code)
					require.NoError(t, db.Model(&owner).Update("status", common.UserStatusEnabled).Error)
				})
				t.Run("unknown_bill_controlled_review_API", func(t *testing.T) {
					require.NoError(t, db.AutoMigrate(&model.Task{}, &model.Midjourney{}))
					for _, kind := range []string{"task", "midjourney"} {
						t.Run(kind, func(t *testing.T) {
							owner := model.User{Username: "bill-review-api-" + kind, Password: "fixture", AffCode: "bill-review-api-" + kind, Status: common.UserStatusEnabled, AccountingVersion: 1}
							require.NoError(t, db.Create(&owner).Error)
							_, err := model.GrantCreditPack(db, model.CreditGrant{UserID: owner.Id, SourceType: "test", SourceID: "review-api", Amount: 100, StartsAt: now - 1, ExpiresAt: now + 3600, UseMask: model.CreditUseAPI}, now)
							require.NoError(t, err)
							bill, err := model.BeginCreditRequest(db, model.CreditRequestInput{UserID: owner.Id, RequestID: "review-api", ModelName: "model", Protocol: "task", PriceSnapshot: `{"private":"review-price"}`, Playground: true, Amount: 30}, now)
							require.NoError(t, err)
							require.NoError(t, model.MarkCreditRequestSubmitted(db, owner.Id, bill.ID, now))
							job := model.Task{UserId: owner.Id, TaskID: "api-review-task", Status: model.TaskStatusSubmitted, Quota: 30, PrivateData: model.TaskPrivateData{BillingSource: model.CreditFundingSource, CreditRequestID: bill.ID}}
							mj := model.Midjourney{UserId: owner.Id, MjId: "api-review-mj", Status: "SUBMITTED", Quota: 30, CreditRequestID: bill.ID}
							if kind == "task" {
								require.NoError(t, job.Insert())
							} else {
								require.NoError(t, mj.Insert())
							}
							require.NoError(t, model.MarkCreditRequestReviewAt(db, owner.Id, bill.ID, now))
							body := fmt.Sprintf(`{"user_id":%d,"request_id":%d,"actor_id":%d,"event_id":"manual-final","expected_evidence_id":0,"external_reference":"private-meter-reference","reason":"private-manual-review","evidence_version":"manual-fixture-v1","facts":[{"field":"completion_tokens","unit":"token","quantity":15,"source":"upstream"}],"consume":{"reference_quota":15,"completion_tokens":15,"other":"{}"}}`, owner.Id, bill.ID, owner.Id)
							assert.Equal(t, http.StatusForbidden, accessTokenRequest(adminAPI, http.MethodPost, "/api/credit/admin/bills/review", readOnly, "", body).Code)
							pendingTask := accessTokenRequest(adminAPI, http.MethodPost, "/api/credit/admin/bills/review", writeOnly, "", body)
							require.Equal(t, http.StatusConflict, pendingTask.Code, pendingTask.Body.String(), "manual confirmation cannot guess an unfinished task's final fee")
							if kind == "task" {
								job.Status = model.TaskStatusFailure
								require.NoError(t, job.Update())
							} else {
								mj.Status = "FAILURE"
								require.NoError(t, mj.Update())
							}
							missingExpected := strings.Replace(body, `"expected_evidence_id":0,`, "", 1)
							assert.Equal(t, http.StatusBadRequest, accessTokenRequest(adminAPI, http.MethodPost, "/api/credit/admin/bills/review", writeOnly, "", missingExpected).Code)
							confirmed := accessTokenRequest(adminAPI, http.MethodPost, "/api/credit/admin/bills/review", writeOnly, "", body)
							require.Equal(t, http.StatusOK, confirmed.Code, confirmed.Body.String())
							assert.Equal(t, confirmed.Body.String(), accessTokenRequest(adminAPI, http.MethodPost, "/api/credit/admin/bills/review", writeOnly, "", body).Body.String())
							var approval struct {
								Data struct {
									Approval model.CreditBillReviewApproval `json:"approval"`
								} `json:"data"`
							}
							require.NoError(t, common.Unmarshal(confirmed.Body.Bytes(), &approval))
							var receipt model.CreditUsageEvidence
							require.NoError(t, db.First(&receipt, approval.Data.Approval.EvidenceID).Error)
							var input model.CreditEvidenceInput
							require.NoError(t, common.UnmarshalJsonStr(receipt.Payload, &input))
							assert.Equal(t, actor.Id, input.ActorID)
							if kind == "task" {
								assert.False(t, service.RefundTaskQuota(context.Background(), &job, "late automatic failure"))
								service.RecalculateTaskQuota(context.Background(), &job, 0, "late automatic failure")
							} else {
								assert.False(t, service.RefundMidjourneyQuota(context.Background(), &mj, "late automatic failure"))
								mj.Quota = 0
								assert.ErrorIs(t, service.CompleteMidjourneyCreditBilling(&mj), model.ErrCreditNeedsReview)
							}
							require.NoError(t, db.First(&bill, bill.ID).Error)
							assert.Equal(t, "executing", bill.State, "late failure cannot put an approved bill back into review")
							assert.Zero(t, bill.UsageEvidenceID, "late automatic task metering cannot replace the original unknown receipt")
							assert.Zero(t, bill.LeaseUntil)
							summary, err := service.RunCreditRecoveryPass(context.Background(), db, db, "review-api-recovery", common.GetTimestamp())
							require.NoError(t, err)
							assert.Zero(t, summary.Errors)
							require.NoError(t, db.First(&bill, bill.ID).Error)
							assert.Equal(t, "settled", bill.State)
							assert.EqualValues(t, 15, bill.Charged)
							if kind == "task" {
								service.RecalculateTaskQuota(context.Background(), &job, 0, "late automatic failure replay")
								assert.Equal(t, 15, job.Quota)
							} else {
								require.NoError(t, service.CompleteMidjourneyCreditBilling(&mj))
								assert.Equal(t, 15, mj.Quota)
							}
							ownedRead, _ := createScopedAccessToken(t, owner.Id, 0, "wallet:read")
							path := fmt.Sprintf("/api/credit/bills/%d", bill.ID)
							public := accessTokenRequest(adminAPI, http.MethodGet, path, ownedRead, "", "")
							require.Equal(t, http.StatusOK, public.Code, public.Body.String())
							assert.Contains(t, public.Body.String(), `"charged":15`)
							assert.Contains(t, public.Body.String(), `"field":"completion_tokens"`)
							assert.Contains(t, public.Body.String(), `"source":"upstream"`)
							assert.NotContains(t, public.Body.String(), "private-manual-review")
							assert.NotContains(t, public.Body.String(), "private-meter-reference")
							assert.NotContains(t, public.Body.String(), "review-price")
							admin := accessTokenRequest(adminAPI, http.MethodGet, fmt.Sprintf("/api/credit/admin/bills/%d?user_id=%d", bill.ID, owner.Id), readOnly, "", "")
							require.Equal(t, http.StatusOK, admin.Code, admin.Body.String())
							assert.Contains(t, admin.Body.String(), "private-manual-review")
							assert.Contains(t, admin.Body.String(), "private-meter-reference")
							assert.NotContains(t, admin.Body.String(), "review-price")
							assert.Equal(t, http.StatusBadRequest, accessTokenRequest(adminAPI, http.MethodGet, fmt.Sprintf("/api/credit/admin/work?user_id=%d&p=9223372036854775807", owner.Id), readOnly, "", "").Code)
							require.NoError(t, db.Model(&actor).Update("role", common.RoleCommonUser).Error)
							assert.Equal(t, http.StatusForbidden, accessTokenRequest(adminAPI, http.MethodPost, "/api/credit/admin/bills/review", writeOnly, "", body).Code)
							require.NoError(t, db.Model(&actor).Update("role", common.RoleRootUser).Error)
						})
					}
				})
				t.Run("owned_bill_and_controlled_adjustment_API", func(t *testing.T) {
					user := model.User{Username: "bill-api-user", Password: "fixture", AffCode: "bill-api-user", Status: common.UserStatusEnabled, AccountingVersion: 1}
					require.NoError(t, db.Create(&user).Error)
					_, err := model.GrantCreditPack(db, model.CreditGrant{UserID: user.Id, SourceType: "test", SourceID: "bill-api", Amount: 100, StartsAt: now - 1, ExpiresAt: now + 3600, UseMask: model.CreditUseAPI}, now)
					require.NoError(t, err)
					bill, err := model.BeginCreditRequest(db, model.CreditRequestInput{UserID: user.Id, RequestID: "bill-api", ModelName: "model", Protocol: "openai", PriceSnapshot: `{"internal":"private-bill-price"}`, Playground: true, Amount: 30}, now)
					require.NoError(t, err)
					bill, err = model.FinishCreditRequest(db, user.Id, bill.ID, "settle", 20, now)
					require.NoError(t, err)
					path := fmt.Sprintf("/api/credit/bills/%d", bill.ID)
					adminPath := fmt.Sprintf("/api/credit/admin/bills/%d?user_id=%d", bill.ID, user.Id)
					ownedRead, _ := createScopedAccessToken(t, user.Id, 0, "wallet:read")
					foreignRead, _ := createScopedAccessToken(t, actor.Id, 0, "wallet:read")
					assert.Equal(t, http.StatusNotFound, accessTokenRequest(adminAPI, http.MethodGet, path, foreignRead, "", "").Code)
					assert.Equal(t, http.StatusOK, accessTokenRequest(adminAPI, http.MethodGet, path+"?user_id="+fmt.Sprint(actor.Id), ownedRead, "", "").Code)
					assert.Equal(t, http.StatusForbidden, accessTokenRequest(adminAPI, http.MethodGet, adminPath, ownedRead, "", "").Code)
					body := fmt.Sprintf(`{"user_id":%d,"request_id":%d,"actor_id":%d,"event_id":"bill-api-correction","expected_revision":0,"reference_quota":10,"evidence_version":"verified-fixture-v1","facts":[{"field":"prompt_tokens","unit":"token","quantity":10,"source":"upstream"}],"reason":"private-admin-receipt-reference"}`, user.Id, bill.ID, user.Id)
					assert.Equal(t, http.StatusForbidden, accessTokenRequest(adminAPI, http.MethodPost, "/api/credit/admin/bills/adjustments", readOnly, "", body).Code)
					updated := accessTokenRequest(adminAPI, http.MethodPost, "/api/credit/admin/bills/adjustments", writeOnly, "", body)
					require.Equal(t, http.StatusOK, updated.Code, updated.Body.String())
					assert.Equal(t, updated.Body.String(), accessTokenRequest(adminAPI, http.MethodPost, "/api/credit/admin/bills/adjustments", writeOnly, "", body).Body.String())
					missingRevision := strings.Replace(body, `"expected_revision":0,`, "", 1)
					assert.Equal(t, http.StatusBadRequest, accessTokenRequest(adminAPI, http.MethodPost, "/api/credit/admin/bills/adjustments", writeOnly, "", missingRevision).Code)
					own := accessTokenRequest(adminAPI, http.MethodGet, path, ownedRead, "", "")
					require.Equal(t, http.StatusOK, own.Code, own.Body.String())
					assert.Contains(t, own.Body.String(), `"charged":10`)
					assert.Contains(t, own.Body.String(), `"revision":1`)
					assert.NotContains(t, own.Body.String(), "private-admin-receipt-reference")
					assert.NotContains(t, own.Body.String(), "private-bill-price")
					assert.NotContains(t, own.Body.String(), "actor_id")
					adminDetails := accessTokenRequest(adminAPI, http.MethodGet, adminPath, readOnly, "", "")
					require.Equal(t, http.StatusOK, adminDetails.Code, adminDetails.Body.String())
					assert.Contains(t, adminDetails.Body.String(), "private-admin-receipt-reference")
					assert.NotContains(t, adminDetails.Body.String(), "private-bill-price")
					assert.Equal(t, http.StatusForbidden, accessTokenRequest(adminAPI, http.MethodGet, adminPath, writeOnly, "", "").Code)
					var adjustment model.CreditBillAdjustment
					require.NoError(t, db.Where("request_id = ?", bill.ID).First(&adjustment).Error)
					assert.Equal(t, actor.Id, adjustment.ActorID, "caller-supplied actor is ignored")
					t.Setenv("CREDIT_RECOVERY_MAX_ATTEMPTS", "1")
					require.NoError(t, db.Callback().Create().Before("gorm:create").Register("test:corrective-log-outage", func(tx *gorm.DB) {
						if log, ok := tx.Statement.Dest.(*model.Log); ok && log.RequestId == adjustment.LogEventID {
							tx.AddError(fmt.Errorf("corrective log sink unavailable"))
						}
					}))
					summary, err := service.RunCreditRecoveryPass(context.Background(), db, db, "correction-log-outage", common.GetTimestamp())
					require.NoError(t, err)
					assert.Equal(t, 1, summary.PendingLogs)
					require.NoError(t, db.Callback().Create().Remove("test:corrective-log-outage"))
					require.NoError(t, db.First(&adjustment, adjustment.ID).Error)
					assert.Equal(t, "review", adjustment.LogState)
					assert.EqualValues(t, 1, adjustment.LogAttempts)
					assert.Contains(t, adjustment.LogLastError, "corrective log sink unavailable")
					retry := fmt.Sprintf(`{"user_id":%d,"adjustment_id":%d,"event_id":"retry-correction-log","reason":"sink repaired"}`, user.Id, adjustment.ID)
					assert.Equal(t, http.StatusForbidden, accessTokenRequest(adminAPI, http.MethodPost, "/api/credit/admin/work/retry", readOnly, "", retry).Code)
					resumed := accessTokenRequest(adminAPI, http.MethodPost, "/api/credit/admin/work/retry", writeOnly, "", retry)
					require.Equal(t, http.StatusOK, resumed.Code, resumed.Body.String())
					summary, err = service.RunCreditRecoveryPass(context.Background(), db, db, "correction-log-repaired", common.GetTimestamp())
					require.NoError(t, err)
					assert.Zero(t, summary.PendingLogs)
					require.NoError(t, db.First(&adjustment, adjustment.ID).Error)
					assert.Equal(t, "delivered", adjustment.LogState)
					var count int64
					require.NoError(t, db.Model(&model.Log{}).Where("request_id = ?", adjustment.LogEventID).Count(&count).Error)
					assert.EqualValues(t, 1, count)

					require.NoError(t, db.Model(&actor).Update("role", common.RoleCommonUser).Error)
					assert.Equal(t, http.StatusForbidden, accessTokenRequest(adminAPI, http.MethodPost, "/api/credit/admin/bills/adjustments", writeOnly, "", body).Code)
					assert.Equal(t, http.StatusForbidden, accessTokenRequest(adminAPI, http.MethodGet, adminPath, readOnly, "", "").Code)
					require.NoError(t, db.Model(&actor).Update("role", common.RoleRootUser).Error)
				})
				draft, err := model.GetSubscriptionPlanDraft(db, versionPlan.Id)
				require.NoError(t, err)
				versionBody, err := common.Marshal(gin.H{"plan_id": -1, "expected_revision": 0, "expected_plan_digest": draft.Digest, "event_id": "api-publish", "actor_id": user.Id})
				require.NoError(t, err)
				published := accessTokenRequest(adminAPI, http.MethodPost, versionPath, writeOnly, "", string(versionBody))
				require.Equal(t, http.StatusOK, published.Code, published.Body.String())
				assert.Equal(t, published.Body.String(), accessTokenRequest(adminAPI, http.MethodPost, versionPath, writeOnly, "", string(versionBody)).Body.String())
				var version model.SubscriptionPlanVersion
				require.NoError(t, db.Where("plan_id = ?", versionPlan.Id).First(&version).Error)
				assert.Equal(t, actor.Id, version.ActorID, "body cannot choose actor or override path plan")
				confirmPaymentComplianceForTest(t)

				t.Run("CNY_draft_currency_is_preserved", func(t *testing.T) {
					body := `{"plan":{"title":"CNY draft","price_amount":1.23,"currency":"CNY","duration_unit":"month","duration_value":1,"enabled":true}}`
					created := accessTokenRequest(adminAPI, http.MethodPost, "/api/subscription/admin/plans", writeOnly, "", body)
					require.Equal(t, http.StatusOK, created.Code, created.Body.String())
					var draft struct {
						Data model.SubscriptionPlan `json:"data"`
					}
					require.NoError(t, common.Unmarshal(created.Body.Bytes(), &draft))
					assert.Equal(t, "CNY", draft.Data.Currency)
					updated := accessTokenRequest(adminAPI, http.MethodPut, fmt.Sprintf("/api/subscription/admin/plans/%d", draft.Data.Id), writeOnly, "", body)
					require.Equal(t, http.StatusOK, updated.Code, updated.Body.String())
					var saved model.SubscriptionPlan
					require.NoError(t, db.First(&saved, draft.Data.Id).Error)
					assert.Equal(t, "CNY", saved.Currency)
				})
				selfRead, _ := createScopedAccessToken(t, user.Id, 0, "wallet:read")
				selfWrite, _ := createScopedAccessToken(t, user.Id, 0, "wallet:write")
				_, err = model.GrantCreditPack(db, model.CreditGrant{UserID: user.Id, SourceType: "topup", SourceID: "api-subscription-balance", Amount: 500000, StartsAt: now - 1, ExpiresAt: now + 3600, UseMask: model.CreditUseSubscription}, now)
				require.NoError(t, err)
				purchaseBody := fmt.Sprintf(`{"version_id":%d,"event_id":"api-balance-purchase","user_id":%d,"plan_id":%d}`, version.ID, actor.Id, versionPlan.Id)
				assert.Equal(t, http.StatusForbidden, accessTokenRequest(adminAPI, http.MethodPost, "/api/subscription/balance/pay", selfRead, "", purchaseBody).Code)
				bought := accessTokenRequest(adminAPI, http.MethodPost, "/api/subscription/balance/pay", selfWrite, "", purchaseBody)
				require.Equal(t, http.StatusOK, bought.Code, bought.Body.String())
				var purchased struct {
					Data model.UserSubscription `json:"data"`
				}
				require.NoError(t, common.Unmarshal(bought.Body.Bytes(), &purchased))
				require.NotZero(t, purchased.Data.Id)
				assert.Equal(t, user.Id, purchased.Data.UserId, "purchase identity comes from authentication")
				assert.Equal(t, bought.Body.String(), accessTokenRequest(adminAPI, http.MethodPost, "/api/subscription/balance/pay", selfWrite, "", purchaseBody).Body.String())
				t.Run("owned_orders_and_manual_payment_review_API", func(t *testing.T) {
					list := accessTokenRequest(adminAPI, http.MethodGet, "/api/subscription/orders?user_id="+fmt.Sprint(actor.Id), selfRead, "", "")
					require.Equal(t, http.StatusOK, list.Code, list.Body.String())
					assert.NotContains(t, list.Body.String(), "private-price-contract")
					assert.NotContains(t, list.Body.String(), "contract_snapshot")
					assert.Contains(t, list.Body.String(), `"event_id":"api-balance-purchase"`, "owned purchase intent identifies an uncertain response without creating another order")
					var listedOrders struct {
						Data struct {
							Items []struct {
								ID int64 `json:"id"`
							} `json:"items"`
						} `json:"data"`
					}
					require.NoError(t, common.Unmarshal(list.Body.Bytes(), &listedOrders))
					require.Len(t, listedOrders.Data.Items, 1)
					assert.Equal(t, *purchased.Data.PurchaseOrderID, listedOrders.Data.Items[0].ID, "a supplied user_id cannot replace authenticated ownership")

					orderPath := fmt.Sprintf("/api/subscription/orders/%d", *purchased.Data.PurchaseOrderID)
					assert.Equal(t, http.StatusOK, accessTokenRequest(adminAPI, http.MethodGet, orderPath, selfRead, "", "").Code)
					assert.Equal(t, http.StatusForbidden, accessTokenRequest(adminAPI, http.MethodGet, orderPath, selfWrite, "", "").Code)
					foreignRead, _ := createScopedAccessToken(t, actor.Id, 0, "wallet:read")
					assert.Equal(t, http.StatusNotFound, accessTokenRequest(adminAPI, http.MethodGet, orderPath, foreignRead, "", "").Code)
					assert.Equal(t, http.StatusBadRequest, accessTokenRequest(adminAPI, http.MethodGet, "/api/subscription/orders?p=9223372036854775807", selfRead, "", "").Code)
					assert.Equal(t, http.StatusForbidden, accessTokenRequest(adminAPI, http.MethodGet, "/api/subscription/admin/payment-reviews", selfRead, "", "").Code)
					assert.Equal(t, http.StatusOK, accessTokenRequest(adminAPI, http.MethodGet, "/api/subscription/admin/payment-reviews", readOnly, "", "").Code)
					cash, err := model.CreateSubscriptionPurchaseOrder(db, model.SubscriptionPurchaseInput{UserID: user.Id, VersionID: version.ID, Provider: model.PaymentProviderCreem, EventID: "api-cash-review", ExpiresAt: now + 900}, now-100)
					require.NoError(t, err)
					missing, err := model.RecordSubscriptionPaymentFact(db, model.VerifiedSubscriptionPayment{OrderID: cash.ID, Provider: cash.Provider, EventID: "api-cash-incomplete", ReferenceID: "api-cash-transaction", BuyerID: user.Id, AmountMicros: common.GetPointer(cash.PriceMicros), Currency: cash.Currency, Succeeded: true, EvidenceDigest: strings.Repeat("a", 64)}, now)
					require.NoError(t, err)
					reviewPath := fmt.Sprintf("/api/subscription/admin/orders/%d", cash.ID)
					details := accessTokenRequest(adminAPI, http.MethodGet, reviewPath, readOnly, "", "")
					require.Equal(t, http.StatusOK, details.Code, details.Body.String())
					assert.Contains(t, details.Body.String(), "missing_paid_at")
					assert.NotContains(t, details.Body.String(), "checkout_response")
					resolvePath := reviewPath + "/reconcile"
					assert.Equal(t, http.StatusForbidden, accessTokenRequest(adminAPI, http.MethodPost, resolvePath, readOnly, "", `{}`).Code)
					assert.Equal(t, http.StatusBadRequest, accessTokenRequest(adminAPI, http.MethodPost, resolvePath, writeOnly, "", `{}`).Code)
					approval, err := common.Marshal(gin.H{"expected_fact_id": missing.ID, "event_id": "api-manual-review", "reference_id": "api-cash-transaction", "amount_micros": cash.PriceMicros, "currency": "USD", "paid_at": now - 90, "evidence_reference": "provider receipt admin-case-1", "reason": "verified time externally", "actor_id": user.Id, "order_id": -1})
					require.NoError(t, err)
					approved := accessTokenRequest(adminAPI, http.MethodPost, resolvePath, writeOnly, "", string(approval))
					require.Equal(t, http.StatusOK, approved.Code, approved.Body.String())
					assert.Equal(t, approved.Body.String(), accessTokenRequest(adminAPI, http.MethodPost, resolvePath, writeOnly, "", string(approval)).Body.String())
					var operation model.CreditOperation
					require.NoError(t, db.Where("user_id = ? AND kind = ?", user.Id, "payment_review").First(&operation).Error)
					assert.Contains(t, operation.Result, fmt.Sprintf(`"actor_id":%d`, actor.Id))
				})

				selfResponse := accessTokenRequest(adminAPI, http.MethodGet, "/api/subscription/self", selfRead, "", "")
				require.Equal(t, http.StatusOK, selfResponse.Code, selfResponse.Body.String())
				assert.Contains(t, selfResponse.Body.String(), "current_rights")
				assert.NotContains(t, selfResponse.Body.String(), "contract_snapshot", "the user cannot read internal gateway contract fields")
				cancelPath := fmt.Sprintf("/api/subscription/admin/user_subscriptions/%d/invalidate", purchased.Data.Id)
				assert.Equal(t, http.StatusForbidden, accessTokenRequest(adminAPI, http.MethodPost, cancelPath, readOnly, "", `{}`).Code)
				assert.Equal(t, http.StatusBadRequest, accessTokenRequest(adminAPI, http.MethodPost, cancelPath, writeOnly, "", `{}`).Code)
				cancelled := accessTokenRequest(adminAPI, http.MethodPost, cancelPath, writeOnly, "", fmt.Sprintf(`{"event_id":"api-cancel","reason":"support decision","actor_id":%d}`, user.Id))
				require.Equal(t, http.StatusOK, cancelled.Code, cancelled.Body.String())
				var cancellation model.CreditOperation
				require.NoError(t, db.Where("user_id = ? AND kind = ?", user.Id, "cancel_rights").First(&cancellation).Error)
				assert.Contains(t, cancellation.Result, fmt.Sprintf(`"actor_id":%d`, actor.Id))
				listed := accessTokenRequest(adminAPI, http.MethodGet, versionPath, readOnly, "", "")
				require.Equal(t, http.StatusOK, listed.Code, listed.Body.String())
				var history struct {
					Data struct {
						Latest   int64                           `json:"latest_revision"`
						Total    int64                           `json:"total"`
						Versions []model.SubscriptionPlanVersion `json:"versions"`
						Draft    model.SubscriptionPlanDraft     `json:"draft"`
					} `json:"data"`
				}
				require.NoError(t, common.Unmarshal(listed.Body.Bytes(), &history))
				assert.EqualValues(t, 1, history.Data.Latest)
				assert.EqualValues(t, 1, history.Data.Total)
				require.Len(t, history.Data.Versions, 1)
				assert.Equal(t, version.Snapshot, history.Data.Versions[0].Snapshot)
				assert.Equal(t, draft.Digest, history.Data.Draft.Digest)
				for _, suffix := range []string{"?p=-1", "?page_size=-1", "?p=9223372036854775807&page_size=100"} {
					badPage := accessTokenRequest(adminAPI, http.MethodGet, versionPath+suffix, readOnly, "", "")
					assert.Equal(t, http.StatusBadRequest, badPage.Code, "invalid pagination cannot remove the limit or overflow the offset: %s", suffix)
				}
				missingRevision, err := common.Marshal(gin.H{"expected_plan_digest": draft.Digest, "event_id": "api-missing-revision"})
				require.NoError(t, err)
				assert.Equal(t, http.StatusBadRequest, accessTokenRequest(adminAPI, http.MethodPost, versionPath, writeOnly, "", string(missingRevision)).Code, "expected revision must be supplied explicitly, including zero")
				require.NoError(t, db.Model(&versionPlan).Update("title", "Edited API draft").Error)
				staleBody, err := common.Marshal(gin.H{"expected_revision": 1, "expected_plan_digest": draft.Digest, "event_id": "api-stale-publish"})
				require.NoError(t, err)
				assert.Equal(t, http.StatusConflict, accessTokenRequest(adminAPI, http.MethodPost, versionPath, writeOnly, "", string(staleBody)).Code)
				expired, _ := createScopedAccessToken(t, actor.Id, now-1, "billing:write")
				assert.Equal(t, http.StatusUnauthorized, accessTokenRequest(adminAPI, http.MethodPost, "/api/credit/admin/grants", expired, "", `{}`).Code)
				revoked, revokedToken := createScopedAccessToken(t, actor.Id, 0, "billing:write")
				require.NoError(t, db.Delete(revokedToken).Error)
				assert.Equal(t, http.StatusUnauthorized, accessTokenRequest(adminAPI, http.MethodPost, "/api/credit/admin/grants", revoked, "", `{}`).Code)
				require.NoError(t, db.Model(&actor).Update("role", common.RoleAdminUser).Error)
				assert.Equal(t, http.StatusForbidden, accessTokenRequest(adminAPI, http.MethodPut, "/api/credit/admin/policies", writeOnly, "", `{}`).Code)
				require.NoError(t, db.Model(&actor).Update("role", common.RoleCommonUser).Error)
				assert.Equal(t, http.StatusForbidden, accessTokenRequest(adminAPI, http.MethodPost, "/api/credit/admin/grants", writeOnly, "", `{}`).Code)
				assert.Equal(t, http.StatusForbidden, accessTokenRequest(adminAPI, http.MethodGet, fmt.Sprintf("/api/credit/admin/work?user_id=%d", user.Id), readOnly, "", "").Code)
				assert.Equal(t, http.StatusForbidden, accessTokenRequest(adminAPI, http.MethodGet, versionPath, readOnly, "", "").Code)
				require.NoError(t, db.Model(&actor).Update("role", common.RoleRootUser).Error)
				require.NoError(t, db.Model(&versionPlan).Update("price_amount", 2).Error)
				catalog := accessTokenRequest(adminAPI, http.MethodGet, "/api/subscription/plans", selfRead, "", "")
				require.Equal(t, http.StatusOK, catalog.Code, catalog.Body.String())
				var products struct {
					Data []SubscriptionPlanDTO `json:"data"`
				}
				require.NoError(t, common.Unmarshal(catalog.Body.Bytes(), &products))
				require.Len(t, products.Data, 1)
				require.NotNil(t, products.Data[0].VersionID, "users need the published contract ID to buy")
				assert.Equal(t, version.ID, *products.Data[0].VersionID)
				assert.EqualValues(t, 1, products.Data[0].Plan.PriceAmount, "unpublished drafts cannot change the offered price")
				assert.NotContains(t, catalog.Body.String(), "private-price-contract")
				assert.Contains(t, catalog.Body.String(), `"payment_methods":["stripe"]`, "public catalog must expose supported checkout methods without private product identifiers")
				t.Run("public_inline_checkout_catalog", func(t *testing.T) {
					inlinePlan := model.SubscriptionPlan{Title: "Inline checkout", PriceAmount: 1, Currency: "USD", DurationUnit: model.SubscriptionDurationMonth, DurationValue: 1, Enabled: true}
					require.NoError(t, db.Create(&inlinePlan).Error)
					inlineDraft, err := model.GetSubscriptionPlanDraft(db, inlinePlan.Id)
					require.NoError(t, err)
					_, err = model.PublishSubscriptionPlanVersion(db, model.SubscriptionVersionPublish{PlanID: inlinePlan.Id, ActorID: actor.Id, EventID: "inline-checkout-version", ExpectedPlanDigest: inlineDraft.Digest}, common.GetTimestamp())
					require.NoError(t, err)
					inlineCatalog := accessTokenRequest(adminAPI, http.MethodGet, "/api/subscription/plans", selfRead, "", "")
					require.Equal(t, http.StatusOK, inlineCatalog.Code, inlineCatalog.Body.String())
					products.Data = nil // Each response must decode into a fresh projection; omitted fields must not retain the previous catalog.
					require.NoError(t, common.Unmarshal(inlineCatalog.Body.Bytes(), &products))
					require.Len(t, products.Data, 2, "both published plans must be offered")
					foundInline := false
					for _, product := range products.Data {
						if product.Plan.Id == inlinePlan.Id {
							foundInline = true
							body, err := common.Marshal(product)
							require.NoError(t, err)
							assert.Contains(t, string(body), `"payment_methods":["stripe"]`, "versioned Stripe checkout uses an inline locked price, without a legacy product ID")
						}
					}
					require.True(t, foundInline, "the inline checkout assertion must execute")
				})
				call := func(handler gin.HandlerFunc, actorID int, body any) *httptest.ResponseRecorder {
					data, err := common.Marshal(body)
					require.NoError(t, err)
					w := httptest.NewRecorder()
					c, _ := gin.CreateTestContext(w)
					c.Set("id", actorID)
					c.Set("role", common.RoleRootUser)
					c.Request = httptest.NewRequest(http.MethodPost, "/api/credit/admin", bytes.NewReader(data))
					handler(c)
					return w
				}
				grant := gin.H{"user_id": user.Id, "event_id": "api-admin-grant", "amount": 40, "starts_at": now, "expires_at": now + 3600, "use_mask": 1, "reason": "support compensation", "actor_id": user.Id}
				policy := gin.H{"source_type": "promotion", "duration_seconds": 3600, "use_mask": 1, "expected_revision": 0}
				assert.Equal(t, http.StatusForbidden, call(AdminPutCreditPolicy, user.Id, policy).Code, "stale cached root role cannot change primary policy")
				assert.Equal(t, http.StatusOK, call(AdminPutCreditPolicy, actor.Id, policy).Code)
				assert.Equal(t, http.StatusConflict, call(AdminPutCreditPolicy, actor.Id, policy).Code, "stale policy edit cannot overwrite another administrator")
				var result struct {
					Success bool `json:"success"`
					Data    struct {
						PackID int64 `json:"pack_id"`
					} `json:"data"`
				}
				first := call(AdminGrantCredit, actor.Id, grant)
				require.Equal(t, http.StatusOK, first.Code, first.Body.String())
				require.NoError(t, common.Unmarshal(first.Body.Bytes(), &result))
				require.True(t, result.Success)
				packID := result.Data.PackID
				require.NotZero(t, packID)
				assert.Equal(t, first.Body.String(), call(AdminGrantCredit, actor.Id, grant).Body.String())
				assert.Equal(t, http.StatusForbidden, call(AdminGrantCredit, user.Id, grant).Code)
				grant["amount"] = 41
				assert.Equal(t, http.StatusConflict, call(AdminGrantCredit, actor.Id, grant).Code)
				var pack model.CreditPack
				require.NoError(t, db.First(&pack, packID).Error)
				assert.Equal(t, actor.Id, pack.ActorID, "request body cannot choose the operator")
				assert.EqualValues(t, 40, pack.Issued)
				review := call(AdminOpenCreditReview, actor.Id, gin.H{"user_id": user.Id, "pack_id": packID, "event_id": "api-refund", "reason": "unknown cash refund result", "actor_id": user.Id})
				require.Equal(t, http.StatusOK, review.Code, review.Body.String())
				var persisted model.CreditReviewCase
				require.NoError(t, db.Where("pack_id = ?", packID).First(&persisted).Error)
				assert.Equal(t, actor.Id, persisted.ActorID)
				assert.Equal(t, "unknown", persisted.CashState)
				w := call(AdminRecordCreditCashOutcome, actor.Id, gin.H{"user_id": user.Id, "case_id": persisted.ID, "state": "confirmed", "reference": "verified-refund", "evidence": "provider record checked"})
				require.Equal(t, http.StatusOK, w.Code, w.Body.String())
				assert.Equal(t, http.StatusBadRequest, call(AdminRecordCreditCashOutcome, actor.Id, gin.H{"user_id": user.Id, "case_id": persisted.ID, "state": "confirmed"}).Code)
				require.NoError(t, db.First(&pack, packID).Error)
				assert.NotZero(t, pack.BlockedAt)
				assert.EqualValues(t, 40, pack.Available, "cash evidence cannot mint or erase credits")
			})
			t.Run("stripe_versioned_paid_time_and_activation_retry", func(t *testing.T) {
				confirmPaymentComplianceForTest(t)
				require.NoError(t, db.AutoMigrate(&model.SubscriptionPlan{}, &model.SubscriptionPlanVersion{}, &model.UserSubscription{}, &model.SubscriptionPurchaseOrder{}, &model.SubscriptionPaymentFact{}, &model.SubscriptionPaymentClaim{}))
				oldKey, oldSecret, oldPrice := setting.StripeApiSecret, setting.StripeWebhookSecret, setting.StripePriceId
				oldBackend := stripe.GetBackend(stripe.APIBackend)
				setting.StripeApiSecret, setting.StripeWebhookSecret, setting.StripePriceId = "sk_test_contract", "whsec_contract", ""
				t.Cleanup(func() {
					setting.StripeApiSecret, setting.StripeWebhookSecret, setting.StripePriceId = oldKey, oldSecret, oldPrice
					stripe.SetBackend(stripe.APIBackend, oldBackend)
				})
				type gatewayInvoice struct {
					Order         model.SubscriptionPurchaseOrder
					PaidAt        int64
					LookupFailure bool
				}
				var invoices sync.Map
				type checkoutObservation struct {
					Values         map[string]string
					OrderPersisted bool
				}
				checkouts := make(chan checkoutObservation, 8)
				gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method == http.MethodPost && r.URL.Path == "/v1/checkout/sessions" {
						_ = r.ParseForm()
						values := make(map[string]string)
						for key := range r.Form {
							values[key] = r.Form.Get(key)
						}
						values["idempotency_key"] = r.Header.Get("Idempotency-Key")
						var saved model.SubscriptionPurchaseOrder
						err := db.Where("trade_no = ?", r.Form.Get("client_reference_id")).First(&saved).Error
						checkouts <- checkoutObservation{Values: values, OrderPersisted: err == nil}
						w.Header().Set("Content-Type", "application/json")
						_, _ = w.Write([]byte(`{"id":"cs_checkout","object":"checkout.session","url":"https://checkout.example.invalid/locked"}`))
						return
					}
					name := strings.TrimPrefix(r.URL.Path, "/v1/checkout/sessions/cs_")
					isInvoice := strings.HasPrefix(r.URL.Path, "/v1/invoices/in_")
					if isInvoice {
						name = strings.TrimPrefix(r.URL.Path, "/v1/invoices/in_")
					}
					stored, ok := invoices.Load(name)
					if !ok {
						http.NotFound(w, r)
						return
					}
					fixture := stored.(gatewayInvoice)
					if fixture.LookupFailure {
						http.Error(w, "lookup unavailable", http.StatusServiceUnavailable)
						return
					}
					w.Header().Set("Content-Type", "application/json")
					amount, buyerID, reference := 100, fixture.Order.UserID, fixture.Order.TradeNo
					if name == "wrong_amount" {
						amount = 200
					}
					if name == "wrong_buyer" {
						buyerID = 0
					}
					if isInvoice {
						if name == "wrong_order" {
							reference = "different-contract"
						}
						_, _ = fmt.Fprintf(w, `{"id":"in_%s","object":"invoice","status":"paid","customer":"cus_fixture","amount_paid":%d,"currency":"usd","metadata":{"subscription_order":"%s"},"status_transitions":{"paid_at":%d}}`, name, amount, reference, fixture.PaidAt)
					} else {
						invoiceID := "in_" + name
						if name == "unknown_invoice" {
							invoiceID = ""
						}
						_, _ = fmt.Fprintf(w, `{"id":"cs_%s","object":"checkout.session","status":"complete","payment_status":"paid","mode":"payment","customer":"cus_fixture","client_reference_id":"%s","invoice":"%s","amount_total":%d,"currency":"usd","metadata":{"subscription_order":"%s","user_id":"%d"}}`, name, fixture.Order.TradeNo, invoiceID, amount, fixture.Order.TradeNo, buyerID)
					}
				}))
				t.Cleanup(gateway.Close)
				stripe.SetBackend(stripe.APIBackend, stripe.GetBackendWithConfig(stripe.APIBackend, &stripe.BackendConfig{URL: stripe.String(gateway.URL), HTTPClient: gateway.Client(), MaxNetworkRetries: stripe.Int64(0), EnableTelemetry: stripe.Bool(false)}))
				publisher := model.User{Username: "stripe-contract-admin", Password: "fixture", Status: common.UserStatusEnabled, Role: common.RoleRootUser, AffCode: "stripe-contract-admin"}
				require.NoError(t, db.Create(&publisher).Error)
				product := model.SubscriptionPlan{Title: "Stripe contract", PriceAmount: 1, Currency: "USD", DurationUnit: model.SubscriptionDurationMonth, DurationValue: 1, TotalAmount: 100, Enabled: true}
				require.NoError(t, db.Create(&product).Error)
				draft, err := model.GetSubscriptionPlanDraft(db, product.Id)
				require.NoError(t, err)
				version, err := model.PublishSubscriptionPlanVersion(db, model.SubscriptionVersionPublish{PlanID: product.Id, ActorID: publisher.Id, ExpectedPlanDigest: draft.Digest, EventID: "stripe-contract-publish"}, now-200)
				require.NoError(t, err)
				api := gin.New()
				api.POST("/stripe", StripeWebhook)
				t.Run("locked_checkout_before_gateway", func(t *testing.T) {
					buyer := model.User{Username: "stripe-checkout-buyer", Password: "fixture", Status: common.UserStatusEnabled, Role: common.RoleCommonUser, AccountingVersion: 1, AffCode: "stripe-checkout-buyer"}
					require.NoError(t, db.Create(&buyer).Error)
					checkoutAPI := gin.New()
					checkoutAPI.POST("/checkout", func(c *gin.Context) { c.Set("id", buyer.Id); SubscriptionRequestStripePay(c) })
					request := gin.H{"version_id": version.ID, "event_id": "locked-stripe-checkout", "user_id": publisher.Id, "price": 0}
					data, err := common.Marshal(request)
					require.NoError(t, err)
					for range 2 {
						response := httptest.NewRecorder()
						checkoutAPI.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/checkout", bytes.NewReader(data)))
						require.Contains(t, response.Body.String(), "https://checkout.example.invalid/locked", response.Body.String())
						observation := <-checkouts
						assert.True(t, observation.OrderPersisted, "never issue a cash checkout without its persisted contract")
						assert.Equal(t, "payment", observation.Values["mode"])
						assert.Equal(t, "100", observation.Values["line_items[0][price_data][unit_amount]"])
						assert.Equal(t, "usd", observation.Values["line_items[0][price_data][currency]"])
						assert.Equal(t, "true", observation.Values["invoice_creation[enabled]"])
						assert.Equal(t, observation.Values["client_reference_id"], observation.Values["idempotency_key"])
						assert.Equal(t, observation.Values["client_reference_id"], observation.Values["invoice_creation[invoice_data][metadata][subscription_order]"])
						assert.Equal(t, fmt.Sprint(buyer.Id), observation.Values["metadata[user_id]"])
					}
					var orders []model.SubscriptionPurchaseOrder
					require.NoError(t, db.Where("user_id = ?", buyer.Id).Find(&orders).Error)
					require.Len(t, orders, 1, "checkout replay cannot create another order")
					assert.EqualValues(t, 1000000, orders[0].PriceMicros)
					assert.EqualValues(t, 3600, orders[0].ExpiresAt-orders[0].CreatedAt)
				})
				for _, name := range []string{"paid", "missing_time", "retry", "wrong_amount", "wrong_buyer", "wrong_order", "unknown_invoice", "lookup_retry"} {
					t.Run(name, func(t *testing.T) {
						buyer := model.User{Username: "stripe-contract-" + name, Password: "fixture", Status: common.UserStatusEnabled, Role: common.RoleCommonUser, AccountingVersion: 1, AffCode: "stripe-contract-" + name}
						require.NoError(t, db.Create(&buyer).Error)
						order, err := model.CreateSubscriptionPurchaseOrder(db, model.SubscriptionPurchaseInput{UserID: buyer.Id, VersionID: version.ID, Provider: model.PaymentProviderStripe, EventID: "stripe-purchase-" + name, ExpiresAt: now + 900}, now-100)
						require.NoError(t, err)
						paidAt := now - 90
						if name == "missing_time" {
							paidAt = 0
						}
						invoices.Store(name, gatewayInvoice{Order: order, PaidAt: paidAt, LookupFailure: name == "lookup_retry"})
						payload := []byte(fmt.Sprintf(`{"id":"evt_contract_%s","object":"event","created":%d,"type":"checkout.session.completed","data":{"object":{"id":"cs_%s","object":"checkout.session","client_reference_id":"%s","status":"complete","payment_status":"paid"}}}`, name, now-2, name, order.TradeNo))
						signed := webhook.GenerateTestSignedPayload(&webhook.UnsignedPayload{Payload: payload, Secret: setting.StripeWebhookSecret})
						deliver := func(signature string) *httptest.ResponseRecorder {
							request := httptest.NewRequest(http.MethodPost, "/stripe", bytes.NewReader(payload))
							request.Header.Set("Stripe-Signature", signature)
							response := httptest.NewRecorder()
							api.ServeHTTP(response, request)
							return response
						}
						assert.Equal(t, http.StatusBadRequest, deliver("invalid").Code)
						if name == "lookup_retry" {
							assert.Equal(t, http.StatusInternalServerError, deliver(signed.Header).Code)
							var count int64
							require.NoError(t, db.Model(&model.SubscriptionPaymentFact{}).Where("order_id = ?", order.ID).Count(&count).Error)
							assert.Zero(t, count, "lookup failure cannot establish payment")
							invoices.Store(name, gatewayInvoice{Order: order, PaidAt: paidAt})
						}
						if name == "retry" {
							require.NoError(t, db.Callback().Create().Before("gorm:create").Register("stripe:activation-failure", func(tx *gorm.DB) {
								if tx.Statement.Schema != nil && tx.Statement.Schema.Name == "UserSubscription" {
									tx.AddError(fmt.Errorf("rights storage unavailable"))
								}
							}))
							assert.Equal(t, http.StatusInternalServerError, deliver(signed.Header).Code, "verified payment remains durable but channel must retry activation")
							require.NoError(t, db.Callback().Create().Remove("stripe:activation-failure"))
						}
						assert.Equal(t, http.StatusOK, deliver(signed.Header).Code)
						assert.Equal(t, http.StatusOK, deliver(signed.Header).Code)
						var facts []model.SubscriptionPaymentFact
						require.NoError(t, db.Where("order_id = ?", order.ID).Find(&facts).Error)
						require.Len(t, facts, 1, "one verified notification produces one fact")
						var rights []model.UserSubscription
						require.NoError(t, db.Where("purchase_order_id = ?", order.ID).Find(&rights).Error)
						if name == "missing_time" {
							assert.Nil(t, facts[0].PaidAt)
							assert.Equal(t, "missing_paid_at", facts[0].ReviewReason)
							assert.Empty(t, rights)
							require.NoError(t, db.First(&order, order.ID).Error)
							assert.True(t, order.NeedsReview)
						} else if strings.HasPrefix(name, "wrong_") || name == "unknown_invoice" {
							assert.Empty(t, rights, "mismatched or missing gateway evidence cannot grant rights")
							assert.Equal(t, "review", facts[0].Outcome)
							require.NoError(t, db.First(&order, order.ID).Error)
							assert.True(t, order.NeedsReview)
						} else {
							require.Len(t, rights, 1)
							require.NotNil(t, facts[0].PaidAt)
							assert.Equal(t, paidAt, *facts[0].PaidAt)
							assert.Equal(t, paidAt, rights[0].StartTime, "event creation and receipt time are not payment time")
							assert.Equal(t, paidAt+30*24*3600, rights[0].EndTime)
						}
					})
				}
			})
			t.Run("epay_versioned_unknown_payment_time", func(t *testing.T) {
				confirmPaymentComplianceForTest(t)
				require.NoError(t, db.AutoMigrate(&model.SubscriptionPlan{}, &model.SubscriptionPlanVersion{}, &model.UserSubscription{}, &model.SubscriptionPurchaseOrder{}, &model.SubscriptionPaymentFact{}, &model.SubscriptionPaymentClaim{}))
				oldAddress, oldID, oldKey, oldMethods := operation_setting.PayAddress, operation_setting.EpayId, operation_setting.EpayKey, operation_setting.PayMethods
				operation_setting.PayAddress, operation_setting.EpayId, operation_setting.EpayKey = "https://pay.example.invalid", "merchant-fixture", "epay-signing-fixture"
				operation_setting.PayMethods = []map[string]string{{"type": "alipay"}}
				t.Cleanup(func() {
					operation_setting.PayAddress, operation_setting.EpayId, operation_setting.EpayKey, operation_setting.PayMethods = oldAddress, oldID, oldKey, oldMethods
				})
				admin := model.User{Username: "epay-contract-admin", Password: "fixture", Status: common.UserStatusEnabled, Role: common.RoleRootUser, AffCode: "epay-contract-admin"}
				buyer := model.User{Username: "epay-contract-buyer", Password: "fixture", Status: common.UserStatusEnabled, Role: common.RoleCommonUser, AccountingVersion: 1, AffCode: "epay-contract-buyer"}
				require.NoError(t, db.Create(&admin).Error)
				require.NoError(t, db.Create(&buyer).Error)
				product := model.SubscriptionPlan{Title: "Epay contract", PriceAmount: 1.23, Currency: "CNY", DurationUnit: model.SubscriptionDurationMonth, DurationValue: 1, TotalAmount: 100, Enabled: true}
				require.NoError(t, db.Create(&product).Error)
				draft, err := model.GetSubscriptionPlanDraft(db, product.Id)
				require.NoError(t, err)
				version, err := model.PublishSubscriptionPlanVersion(db, model.SubscriptionVersionPublish{PlanID: product.Id, ActorID: admin.Id, ExpectedPlanDigest: draft.Digest, EventID: "epay-contract-publish"}, now-200)
				require.NoError(t, err)
				api := gin.New()
				api.POST("/checkout", func(c *gin.Context) { c.Set("id", buyer.Id); SubscriptionRequestEpay(c) })
				api.POST("/notify", SubscriptionEpayNotify)
				api.GET("/return", SubscriptionEpayReturn)
				data, err := common.Marshal(gin.H{"version_id": version.ID, "event_id": "epay-locked-checkout", "payment_method": "alipay", "user_id": admin.Id})
				require.NoError(t, err)
				for range 2 {
					response := httptest.NewRecorder()
					api.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/checkout", bytes.NewReader(data)))
					require.Contains(t, response.Body.String(), "https://pay.example.invalid/submit.php", response.Body.String())
					assert.Contains(t, response.Body.String(), `"money":"1.23"`)
				}
				var orders []model.SubscriptionPurchaseOrder
				require.NoError(t, db.Where("user_id = ?", buyer.Id).Find(&orders).Error)
				require.Len(t, orders, 1)
				order := orders[0]
				params := epay.GenerateParams(map[string]string{"pid": operation_setting.EpayId, "out_trade_no": order.TradeNo, "trade_no": "provider-epay-trade", "money": "1.23", "type": "alipay", "trade_status": epay.StatusTradeSuccess, "sign_type": "MD5"}, operation_setting.EpayKey)
				values := make(url.Values)
				for key, value := range params {
					values.Set(key, value)
				}
				invalid := make(url.Values)
				for key, value := range params {
					invalid.Set(key, value)
				}
				invalid.Set("sign", "invalid")
				response := httptest.NewRecorder()
				request := httptest.NewRequest(http.MethodPost, "/notify", strings.NewReader(invalid.Encode()))
				request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
				api.ServeHTTP(response, request)
				assert.Equal(t, "fail", response.Body.String())
				for range 2 {
					response := httptest.NewRecorder()
					request := httptest.NewRequest(http.MethodPost, "/notify", strings.NewReader(values.Encode()))
					request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
					api.ServeHTTP(response, request)
					require.Equal(t, "success", response.Body.String())
				}
				response = httptest.NewRecorder()
				api.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/return?"+values.Encode(), nil))
				assert.Equal(t, http.StatusFound, response.Code)
				assert.Contains(t, response.Header().Get("Location"), "pay=pending")
				var facts []model.SubscriptionPaymentFact
				require.NoError(t, db.Where("order_id = ?", order.ID).Find(&facts).Error)
				require.Len(t, facts, 1, "browser return and notification share one payment fact")
				assert.Nil(t, facts[0].PaidAt, "Epay receipt has no reliable payment time")
				assert.Equal(t, "missing_paid_at", facts[0].ReviewReason)
				require.NotNil(t, facts[0].AmountMicros)
				assert.EqualValues(t, 1230000, *facts[0].AmountMicros)
				var count int64
				require.NoError(t, db.Model(&model.UserSubscription{}).Where("purchase_order_id = ?", order.ID).Count(&count).Error)
				assert.Zero(t, count)
			})
			t.Run("creem_versioned_unknown_payment_time", func(t *testing.T) {
				confirmPaymentComplianceForTest(t)
				require.NoError(t, db.AutoMigrate(&model.SubscriptionPlan{}, &model.SubscriptionPlanVersion{}, &model.UserSubscription{}, &model.SubscriptionPurchaseOrder{}, &model.SubscriptionPaymentFact{}, &model.SubscriptionPaymentClaim{}))
				oldKey, oldSecret, oldProducts, oldTest := setting.CreemApiKey, setting.CreemWebhookSecret, setting.CreemProducts, setting.CreemTestMode
				setting.CreemApiKey, setting.CreemWebhookSecret, setting.CreemProducts, setting.CreemTestMode = "test-creem-api", "creem-contract-secret", "[]", false
				t.Cleanup(func() {
					setting.CreemApiKey, setting.CreemWebhookSecret, setting.CreemProducts, setting.CreemTestMode = oldKey, oldSecret, oldProducts, oldTest
				})
				admin := model.User{Username: "creem-contract-admin", Password: "fixture", Status: common.UserStatusEnabled, Role: common.RoleRootUser, AffCode: "creem-contract-admin"}
				buyer := model.User{Username: "creem-contract-buyer", Password: "fixture", Status: common.UserStatusEnabled, Role: common.RoleCommonUser, AccountingVersion: 1, AffCode: "creem-contract-buyer"}
				require.NoError(t, db.Create(&admin).Error)
				require.NoError(t, db.Create(&buyer).Error)
				product := model.SubscriptionPlan{Title: "Creem contract", PriceAmount: 1, Currency: "USD", DurationUnit: model.SubscriptionDurationMonth, DurationValue: 1, TotalAmount: 100, Enabled: true, CreemProductId: "prod_contract"}
				require.NoError(t, db.Create(&product).Error)
				draft, err := model.GetSubscriptionPlanDraft(db, product.Id)
				require.NoError(t, err)
				version, err := model.PublishSubscriptionPlanVersion(db, model.SubscriptionVersionPublish{PlanID: product.Id, ActorID: admin.Id, ExpectedPlanDigest: draft.Digest, EventID: "creem-contract-publish"}, now-200)
				require.NoError(t, err)
				api := gin.New()
				api.POST("/creem", CreemWebhook)
				t.Run("checkout_response_loss_does_not_duplicate_cash_link", func(t *testing.T) {
					var cashCalls atomic.Int32
					var lookupFailure atomic.Bool
					var productCase atomic.Int32
					var persisted atomic.Bool
					requests := make(chan map[string]any, 4)
					gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						w.Header().Set("Content-Type", "application/json")
						if r.Method == http.MethodGet && r.URL.Path == "/v1/products" {
							if lookupFailure.Load() {
								http.Error(w, "lookup unavailable", http.StatusServiceUnavailable)
								return
							}
							currency, billingType, taxMode := "USD", "onetime", "inclusive"
							switch productCase.Load() {
							case 1:
								currency = "CNY"
							case 2:
								billingType = "recurring"
							case 3:
								taxMode = "exclusive"
							}
							_, _ = fmt.Fprintf(w, `{"id":"prod_contract","mode":"prod","currency":"%s","billing_type":"%s","tax_mode":"%s","status":"active"}`, currency, billingType, taxMode)
							return
						}
						if r.Method != http.MethodPost || r.URL.Path != "/v1/checkouts" {
							http.NotFound(w, r)
							return
						}
						cashCalls.Add(1)
						var body map[string]any
						if err := common.DecodeJson(r.Body, &body); err != nil {
							http.Error(w, "invalid fixture request", 400)
							return
						}
						requests <- body
						var order model.SubscriptionPurchaseOrder
						if err := db.Where("trade_no = ?", body["request_id"]).First(&order).Error; err == nil {
							persisted.Store(true)
						}
						if order.EventID == "creem-checkout-unknown" {
							http.Error(w, "ambiguous gateway result", http.StatusServiceUnavailable)
							return
						}
						_, _ = w.Write([]byte(`{"id":"ch_contract","checkout_url":"https://checkout.example.invalid/creem-locked"}`))
					}))
					t.Cleanup(gateway.Close)
					target, err := url.Parse(gateway.URL)
					require.NoError(t, err)
					oldTransport := http.DefaultTransport
					http.DefaultTransport = subscriptionGatewayTestTransport{target: target, upstream: oldTransport}
					t.Cleanup(func() { http.DefaultTransport = oldTransport })
					checkoutAPI := gin.New()
					checkoutAPI.POST("/checkout", func(c *gin.Context) { c.Set("id", buyer.Id); SubscriptionRequestCreemPay(c) })
					call := func(eventID string) *httptest.ResponseRecorder {
						body, err := common.Marshal(gin.H{"version_id": version.ID, "event_id": eventID, "user_id": admin.Id, "price": 0})
						require.NoError(t, err)
						response := httptest.NewRecorder()
						checkoutAPI.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/checkout", bytes.NewReader(body)))
						return response
					}
					first := call("creem-checkout-known")
					require.Contains(t, first.Body.String(), "https://checkout.example.invalid/creem-locked", first.Body.String())
					assert.Equal(t, first.Body.String(), call("creem-checkout-known").Body.String())
					assert.EqualValues(t, 1, cashCalls.Load(), "return the saved checkout after a lost application response")
					assert.True(t, persisted.Load())
					request := <-requests
					assert.Equal(t, float64(100), request["custom_price"], "cash amount comes from the locked contract")
					metadata, ok := request["metadata"].(map[string]any)
					require.True(t, ok)
					assert.Equal(t, fmt.Sprint(buyer.Id), metadata["user_id"])
					assert.Equal(t, request["request_id"], metadata["subscription_order"])
					assert.Equal(t, http.StatusBadGateway, call("creem-checkout-unknown").Code)
					assert.Equal(t, http.StatusConflict, call("creem-checkout-unknown").Code)
					assert.EqualValues(t, 2, cashCalls.Load(), "unknown provider outcome cannot blindly issue another checkout")
					var unknown model.SubscriptionPurchaseOrder
					require.NoError(t, db.Where("user_id = ? AND event_id = ?", buyer.Id, "creem-checkout-unknown").First(&unknown).Error)
					assert.True(t, unknown.NeedsReview)
					assert.Equal(t, "checkout_result_unknown", unknown.LastReviewReason)
					lookupFailure.Store(true)
					assert.Equal(t, http.StatusBadGateway, call("creem-checkout-lookup-retry").Code)
					assert.EqualValues(t, 2, cashCalls.Load(), "product lookup failure has not issued cash checkout")
					var retryable model.SubscriptionPurchaseOrder
					require.NoError(t, db.Where("user_id = ? AND event_id = ?", buyer.Id, "creem-checkout-lookup-retry").First(&retryable).Error)
					assert.False(t, retryable.NeedsReview, "a read failure cannot create an ambiguous payment")
					lookupFailure.Store(false)
					assert.Contains(t, call("creem-checkout-lookup-retry").Body.String(), "https://checkout.example.invalid/creem-locked")
					assert.EqualValues(t, 3, cashCalls.Load())
					for i, name := range []string{"wrong-currency", "recurring", "exclusive-tax"} {
						productCase.Store(int32(i + 1))
						assert.Equal(t, http.StatusConflict, call("creem-checkout-"+name).Code, "incompatible billing terms must be rejected before issuance")
						assert.EqualValues(t, 3, cashCalls.Load())
					}
				})
				for _, name := range []string{"known", "missing_amount", "test_bypass"} {
					t.Run(name, func(t *testing.T) {
						order, err := model.CreateSubscriptionPurchaseOrder(db, model.SubscriptionPurchaseInput{UserID: buyer.Id, VersionID: version.ID, Provider: model.PaymentProviderCreem, EventID: "creem-purchase-" + name, ExpiresAt: now + 900}, now-100)
						require.NoError(t, err)
						amount := `,"amount_paid":100`
						if name == "missing_amount" {
							amount = ""
						}
						payload := fmt.Sprintf(`{"id":"evt_creem_%s","eventType":"checkout.completed","created_at":%d,"object":{"id":"ch_creem_%s","request_id":"%s","metadata":{"user_id":"%d","subscription_order":"%s"},"mode":"prod","product":{"id":"prod_contract","billing_type":"onetime"},"order":{"id":"ord_%s","transaction":"tx_%s","status":"paid","type":"onetime","currency":"USD","created_at":"2026-10-01T00:00:00Z","updated_at":"2026-10-08T00:00:00Z"%s}}}`, name, now-2, name, order.TradeNo, buyer.Id, order.TradeNo, name, name, amount)
						deliver := func(signature string) *httptest.ResponseRecorder {
							response := httptest.NewRecorder()
							request := httptest.NewRequest(http.MethodPost, "/creem", strings.NewReader(payload))
							request.Header.Set("Content-Type", "application/json")
							request.Header.Set(CreemSignatureHeader, signature)
							api.ServeHTTP(response, request)
							return response
						}
						if name == "test_bypass" {
							setting.CreemWebhookSecret, setting.CreemTestMode = "", true
							assert.Equal(t, http.StatusForbidden, deliver("anything").Code, "new purchase facts require verification even in legacy test mode")
							setting.CreemWebhookSecret, setting.CreemTestMode = "creem-contract-secret", false
							return
						}
						assert.Equal(t, http.StatusUnauthorized, deliver("invalid").Code)
						signature := generateCreemSignature(payload, setting.CreemWebhookSecret)
						assert.Equal(t, http.StatusOK, deliver(signature).Code)
						assert.Equal(t, http.StatusOK, deliver(signature).Code)
						var facts []model.SubscriptionPaymentFact
						require.NoError(t, db.Where("order_id = ?", order.ID).Find(&facts).Error)
						require.Len(t, facts, 1)
						assert.Nil(t, facts[0].PaidAt, "created_at and updated_at are not proven payment time")
						if name == "missing_amount" {
							assert.Nil(t, facts[0].AmountMicros)
							assert.Equal(t, "missing_amount", facts[0].ReviewReason)
						} else {
							require.NotNil(t, facts[0].AmountMicros)
							assert.EqualValues(t, 1000000, *facts[0].AmountMicros)
							assert.Equal(t, "missing_paid_at", facts[0].ReviewReason)
						}
						var count int64
						require.NoError(t, db.Model(&model.UserSubscription{}).Where("purchase_order_id = ?", order.ID).Count(&count).Error)
						assert.Zero(t, count)
					})
				}
			})
			t.Run("pancake_versioned_verified_payment_date", func(t *testing.T) {
				confirmPaymentComplianceForTest(t)
				require.NoError(t, db.AutoMigrate(&model.SubscriptionPlan{}, &model.SubscriptionPlanVersion{}, &model.UserSubscription{}, &model.SubscriptionPurchaseOrder{}, &model.SubscriptionPaymentFact{}, &model.SubscriptionPaymentClaim{}))
				oldMerchant, oldPrivate, oldProduct, oldStore := setting.WaffoPancakeMerchantID, setting.WaffoPancakePrivateKey, setting.WaffoPancakeProductID, setting.WaffoPancakeStoreID
				oldVerifier := verifyWaffoPancakeWebhook
				key, err := rsa.GenerateKey(rand.Reader, 2048)
				require.NoError(t, err)
				publicDER, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
				require.NoError(t, err)
				publicKey := string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: publicDER}))
				setting.WaffoPancakeMerchantID, setting.WaffoPancakePrivateKey, setting.WaffoPancakeProductID, setting.WaffoPancakeStoreID = "MER_0123456789012345678901", "configured-fixture-private", "", "STO_0123456789012345678901"
				verifyWaffoPancakeWebhook = func(payload, signature string) (*service.WaffoPancakeWebhookEvent, error) {
					return service.VerifyWaffoPancakeWebhook(payload, signature, &pancake.VerifyWebhookOptions{PublicKey: publicKey})
				}
				t.Cleanup(func() {
					setting.WaffoPancakeMerchantID, setting.WaffoPancakePrivateKey, setting.WaffoPancakeProductID, setting.WaffoPancakeStoreID = oldMerchant, oldPrivate, oldProduct, oldStore
					verifyWaffoPancakeWebhook = oldVerifier
				})
				admin := model.User{Username: "pancake-contract-admin", Password: "fixture", Status: common.UserStatusEnabled, Role: common.RoleRootUser, AffCode: "pancake-contract-admin"}
				require.NoError(t, db.Create(&admin).Error)
				product := model.SubscriptionPlan{Title: "Pancake contract", PriceAmount: 1, Currency: "USD", DurationUnit: model.SubscriptionDurationMonth, DurationValue: 1, TotalAmount: 100, Enabled: true, WaffoPancakeProductId: "PROD_0123456789012345678901"}
				require.NoError(t, db.Create(&product).Error)
				draft, err := model.GetSubscriptionPlanDraft(db, product.Id)
				require.NoError(t, err)
				version, err := model.PublishSubscriptionPlanVersion(db, model.SubscriptionVersionPublish{PlanID: product.Id, ActorID: admin.Id, ExpectedPlanDigest: draft.Digest, EventID: "pancake-contract-publish"}, now-200)
				require.NoError(t, err)
				api := gin.New()
				api.POST("/pancake/:env", WaffoPancakeWebhook)

				t.Run("checkout_issuance_and_partial_gateway_failure", func(t *testing.T) {
					setting.WaffoPancakePrivateKey = string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
					buyer := model.User{Username: "pancake-checkout", Password: "fixture", Status: common.UserStatusEnabled, Role: common.RoleCommonUser, AccountingVersion: 1, AffCode: "pc-checkout"}
					require.NoError(t, db.Create(&buyer).Error)
					var cashCalls atomic.Int32
					var tokenFailure atomic.Bool
					var invalidTokenExpiry atomic.Bool
					var disableDuringToken atomic.Bool
					tokenExpiry := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
					requests := make(chan map[string]any, 2)
					gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						w.Header().Set("Content-Type", "application/json")
						switch r.URL.Path {
						case "/v1/graphql":
							_, _ = w.Write([]byte(`{"data":{"stores":[{"id":"STO_0123456789012345678901","status":"active","prodEnabled":true,"onetimeProducts":[{"id":"PROD_0123456789012345678901","status":"active"}]}]}}`))
						case "/v1/actions/auth/issue-session-token":
							var body map[string]any
							if err := common.DecodeJson(r.Body, &body); err != nil {
								assert.NoError(t, err)
								http.Error(w, "invalid fixture body", 400)
								return
							}
							assert.Equal(t, service.WaffoPancakeBuyerIdentityFromUserID(buyer.Id), body["buyerIdentity"])
							if tokenFailure.Load() {
								http.Error(w, "ambiguous authentication result", 503)
								return
							}
							expiry := tokenExpiry
							if r.Header.Get("X-Idempotency-Key") != "" || invalidTokenExpiry.Load() {
								expiry = time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
							}
							if disableDuringToken.Load() {
								if err := db.Model(&model.User{}).Where("id = ?", buyer.Id).Update("status", common.UserStatusDisabled).Error; err != nil {
									assert.NoError(t, err)
									http.Error(w, "fixture mutation failed", 500)
									return
								}
							}
							response, err := common.Marshal(gin.H{"data": gin.H{"token": "fixture-buyer-token", "expiresAt": expiry}})
							if err != nil {
								assert.NoError(t, err)
								http.Error(w, "fixture encoding failed", 500)
								return
							}
							_, _ = w.Write(response)
						case "/v1/actions/checkout/create-session":
							cashCalls.Add(1)
							var body map[string]any
							require.NoError(t, common.DecodeJson(r.Body, &body))
							var saved model.SubscriptionPurchaseOrder
							require.NoError(t, db.Where("trade_no = ?", body["orderMerchantExternalId"]).First(&saved).Error)
							assert.Equal(t, "started", saved.CheckoutState)
							assert.Equal(t, buyer.Id, saved.UserID)
							requests <- body
							_, _ = w.Write([]byte(`{"data":{"sessionId":"CHK_fixture","checkoutUrl":"https://checkout.example.invalid/pancake","expiresAt":"2026-10-08T20:00:00Z"}}`))
						default:
							http.NotFound(w, r)
						}
					}))
					t.Cleanup(gateway.Close)
					target, err := url.Parse(gateway.URL)
					require.NoError(t, err)
					oldTransport := http.DefaultTransport
					http.DefaultTransport = subscriptionGatewayTestTransport{target: target, upstream: oldTransport}
					t.Cleanup(func() { http.DefaultTransport = oldTransport })
					checkoutAPI := gin.New()
					checkoutAPI.POST("/checkout", func(c *gin.Context) { c.Set("id", buyer.Id); SubscriptionRequestWaffoPancakePay(c) })
					call := func(eventID string) *httptest.ResponseRecorder {
						body, err := common.Marshal(gin.H{"version_id": version.ID, "event_id": eventID, "user_id": admin.Id, "price": 0})
						require.NoError(t, err)
						response := httptest.NewRecorder()
						checkoutAPI.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/checkout", bytes.NewReader(body)))
						return response
					}
					first := call("pancake-checkout-known")
					require.Contains(t, first.Body.String(), "https://checkout.example.invalid/pancake", first.Body.String())
					assert.Equal(t, first.Body.String(), call("pancake-checkout-known").Body.String())
					assert.EqualValues(t, 1, cashCalls.Load())
					var stored model.SubscriptionPurchaseOrder
					require.NoError(t, db.Where("user_id = ? AND event_id = ?", buyer.Id, "pancake-checkout-known").First(&stored).Error)
					assert.NotContains(t, stored.CheckoutResponse, "fixture-buyer-token", "never persist a reusable buyer JWT in the checkout response")
					tokenFailure.Store(true)
					assert.Equal(t, http.StatusBadGateway, call("pancake-checkout-known").Code)
					require.NoError(t, db.First(&stored, stored.ID).Error)
					assert.False(t, stored.NeedsReview, "retrying buyer authentication cannot make a confirmed cash session unknown")
					assert.EqualValues(t, 1, cashCalls.Load())
					tokenFailure.Store(false)
					resumed := call("pancake-checkout-known")
					assert.Contains(t, resumed.Body.String(), "fixture-buyer-token")
					assert.Contains(t, resumed.Body.String(), tokenExpiry, "buyer refresh must bypass the gateway's cached expired token")
					invalidTokenExpiry.Store(true)
					assert.Equal(t, http.StatusBadGateway, call("pancake-checkout-known").Code, "expired authentication must never be returned to the buyer")
					invalidTokenExpiry.Store(false)
					assert.EqualValues(t, 1, cashCalls.Load())

					request := <-requests
					assert.Equal(t, "USD", request["currency"])
					assert.Equal(t, "PROD_0123456789012345678901", request["productId"])
					price, ok := request["priceSnapshot"].(map[string]any)
					require.True(t, ok)
					assert.Equal(t, "1.00", price["amount"])
					tokenFailure.Store(true)
					assert.Equal(t, http.StatusBadGateway, call("pancake-checkout-unknown").Code)
					assert.Equal(t, http.StatusConflict, call("pancake-checkout-unknown").Code)
					assert.EqualValues(t, 2, cashCalls.Load(), "successful cash-create plus failed parallel token must not create another cash session")
					var unknown model.SubscriptionPurchaseOrder
					require.NoError(t, db.Where("user_id = ? AND event_id = ?", buyer.Id, "pancake-checkout-unknown").First(&unknown).Error)
					assert.True(t, unknown.NeedsReview)
					assert.Equal(t, "unknown", unknown.CheckoutState)
					tokenFailure.Store(false)
					disableDuringToken.Store(true)
					afterDisable := call("pancake-checkout-known")
					assert.NotEqual(t, http.StatusOK, afterDisable.Code, "recheck account eligibility after remote token issuance")
					assert.NotContains(t, afterDisable.Body.String(), "fixture-buyer-token")
				})
				for i, name := range []string{"paid", "missing_time", "date_only", "missing_charge", "zero_charge", "wrong_buyer", "wrong_amount", "wrong_store", "wrong_environment", "late_signature", "expired_signature", "future_signature", "activation_retry"} {
					t.Run(name, func(t *testing.T) {
						buyer := model.User{Username: "pancake-contract-" + name, Password: "fixture", Status: common.UserStatusEnabled, Role: common.RoleCommonUser, AccountingVersion: 1, AffCode: fmt.Sprintf("pc-%d", i)}
						require.NoError(t, db.Create(&buyer).Error)
						order, err := model.CreateSubscriptionPurchaseOrder(db, model.SubscriptionPurchaseInput{UserID: buyer.Id, VersionID: version.ID, Provider: model.PaymentProviderWaffoPancake, EventID: "pancake-purchase-" + name, ExpiresAt: now + 900}, now-100)
						require.NoError(t, err)
						paidAt := now - 90
						store, mode, identity, amount := "STO_0123456789012345678901", "test", service.WaffoPancakeBuyerIdentityFromUserID(buyer.Id), "1.00"
						if name == "wrong_store" {
							store = "STORE_other"
						}
						if name == "wrong_environment" {
							mode = "prod"
						}
						if name == "wrong_buyer" {
							identity = service.WaffoPancakeBuyerIdentityFromUserID(admin.Id)
						}
						if name == "wrong_amount" {
							amount = "2.00"
						}
						if name == "zero_charge" {
							amount = "0.00"
						}
						data := gin.H{"orderId": "ORD_" + name, "orderMerchantExternalId": order.TradeNo, "orderStatus": "completed", "merchantProvidedBuyerIdentity": identity, "currency": "USD", "amount": "1.00", "subtotal": "1.00", "total": "1.00", "taxAmount": "0.00", "paymentId": "PAY_" + name, "paymentStatus": "succeeded"}
						if name != "missing_charge" {
							data["chargedAmount"] = amount
						}
						if name != "missing_time" {
							data["paymentDate"] = time.Unix(paidAt, 0).UTC().Format(time.RFC3339Nano)
						}
						if name == "date_only" {
							data["paymentDate"] = time.Unix(paidAt, 0).UTC().Format("2006-01-02")
						}
						payload, err := common.Marshal(gin.H{"id": "EVT_" + name, "eventId": "order.completed", "eventType": "order.completed", "timestamp": time.Unix(now-2, 0).UTC().Format(time.RFC3339), "storeId": store, "mode": mode, "data": data})
						require.NoError(t, err)
						deliver := func(signature string) *httptest.ResponseRecorder {
							response := httptest.NewRecorder()
							request := httptest.NewRequest(http.MethodPost, "/pancake/test", bytes.NewReader(payload))
							request.Header.Set("X-Waffo-Signature", signature)
							api.ServeHTTP(response, request)
							return response
						}
						assert.Equal(t, http.StatusUnauthorized, deliver("invalid").Code)
						stamp := time.Now().UnixMilli()
						if name == "expired_signature" {
							stamp -= 46 * 60 * 1000
						}
						if name == "late_signature" {
							stamp -= 20 * 60 * 1000
						}
						if name == "future_signature" {
							stamp += 2 * 60 * 1000
						}
						digest := sha256.Sum256(append([]byte(fmt.Sprint(stamp)+"."), payload...))
						signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
						require.NoError(t, err)
						header := fmt.Sprintf("t=%d,v1=%s", stamp, base64.StdEncoding.EncodeToString(signature))
						if name == "activation_retry" {
							require.NoError(t, db.Callback().Create().Before("gorm:create").Register("pancake:activation-failure", func(tx *gorm.DB) {
								if tx.Statement.Schema != nil && tx.Statement.Schema.Name == "UserSubscription" {
									tx.AddError(fmt.Errorf("rights storage unavailable"))
								}
							}))
							assert.Equal(t, http.StatusInternalServerError, deliver(header).Code)
							require.NoError(t, db.Callback().Create().Remove("pancake:activation-failure"))
						}
						expected := http.StatusOK
						if name == "wrong_store" || name == "expired_signature" || name == "future_signature" {
							expected = http.StatusUnauthorized
						}
						assert.Equal(t, expected, deliver(header).Code)
						assert.Equal(t, expected, deliver(header).Code)
						var facts []model.SubscriptionPaymentFact
						require.NoError(t, db.Where("order_id = ?", order.ID).Find(&facts).Error)
						var rights []model.UserSubscription
						require.NoError(t, db.Where("purchase_order_id = ?", order.ID).Find(&rights).Error)
						if name == "wrong_store" || name == "wrong_environment" || name == "expired_signature" || name == "future_signature" {
							assert.Empty(t, facts)
							assert.Empty(t, rights)
							return
						}
						require.Len(t, facts, 1)
						if name == "missing_time" || name == "date_only" || name == "missing_charge" || name == "zero_charge" || strings.HasPrefix(name, "wrong_") {
							assert.Empty(t, rights)
							assert.Equal(t, "review", facts[0].Outcome)
							if name == "missing_time" || name == "date_only" {
								assert.Nil(t, facts[0].PaidAt)
								assert.Equal(t, "missing_paid_at", facts[0].ReviewReason)
							}
							if name == "missing_charge" {
								assert.Nil(t, facts[0].AmountMicros)
								assert.Equal(t, "missing_amount", facts[0].ReviewReason)
							}
							if name == "zero_charge" || name == "wrong_amount" {
								require.NotNil(t, facts[0].AmountMicros)
								assert.Equal(t, "amount_mismatch", facts[0].ReviewReason)
								expectedAmount := int64(2000000)
								if name == "zero_charge" {
									expectedAmount = 0
								}
								assert.Equal(t, expectedAmount, *facts[0].AmountMicros)
							}
						} else {
							require.Len(t, rights, 1)
							assert.Equal(t, paidAt, rights[0].StartTime)
							require.NotNil(t, facts[0].PaidAt)
							assert.Equal(t, paidAt, *facts[0].PaidAt)
						}
					})
				}
			})
			t.Run("redemption_API_preserves_credit_policy", func(t *testing.T) {
				require.NoError(t, db.AutoMigrate(&model.Redemption{}))
				payment := operation_setting.GetPaymentSetting()
				previous := *payment
				payment.ComplianceConfirmed, payment.ComplianceTermsVersion = true, operation_setting.CurrentComplianceTermsVersion
				t.Cleanup(func() { *payment = previous })
				body := gin.H{"name": "credit gift", "count": 1, "quota": 40, "credit_duration_seconds": 123, "credit_use_mask": 3}
				data, err := common.Marshal(body)
				require.NoError(t, err)
				w := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(w)
				c.Set("id", user.Id)
				c.Request = httptest.NewRequest(http.MethodPost, "/api/redemption/", bytes.NewReader(data))
				AddRedemption(c)
				var result struct {
					Success bool     `json:"success"`
					Keys    []string `json:"data"`
				}
				require.NoError(t, common.Unmarshal(w.Body.Bytes(), &result))
				require.True(t, result.Success, w.Body.String())
				require.Len(t, result.Keys, 1)
				var code model.Redemption
				require.NoError(t, db.Where(&model.Redemption{Key: result.Keys[0]}).First(&code).Error)
				assert.EqualValues(t, 123, code.CreditDurationSeconds)
				assert.Equal(t, 3, code.CreditUseMask)
				body["id"], body["credit_duration_seconds"], body["credit_use_mask"] = code.Id, 456, 1
				data, err = common.Marshal(body)
				require.NoError(t, err)
				w = httptest.NewRecorder()
				c, _ = gin.CreateTestContext(w)
				c.Request = httptest.NewRequest(http.MethodPut, "/api/redemption/", bytes.NewReader(data))
				UpdateRedemption(c)
				require.NoError(t, db.First(&code, code.Id).Error)
				assert.EqualValues(t, 456, code.CreditDurationSeconds)
				assert.Equal(t, 1, code.CreditUseMask)
			})

			t.Run("subscription_windows_share_keys_and_independent_wallet", func(t *testing.T) {
				require.NoError(t, db.AutoMigrate(&model.SubscriptionPlan{}, &model.UserSubscription{}, &model.SubscriptionPlanVersion{}, &model.SubscriptionPurchaseOrder{}, &model.SubscriptionPaymentFact{}, &model.SubscriptionPaymentClaim{}))
				require.NoError(t, config.GlobalConfig.LoadFromDB(map[string]string{"billing_setting.billing_mode": `{"credit-model":"tiered_expr"}`, "billing_setting.billing_expr": `{"credit-model":"tier(\"request\", fixed(0.00007))"}`, "group_ratio_setting.group_ratio": `{"default":1}`}))
				admin := model.User{Username: "window-http-admin", Password: "fixture", Status: common.UserStatusEnabled, Role: common.RoleRootUser, AffCode: "window-http-admin"}
				require.NoError(t, db.Create(&admin).Error)
				buyer := model.User{Username: "window-http-user", Password: "fixture", Status: common.UserStatusEnabled, Role: common.RoleCommonUser, Group: "default", AccountingVersion: 1, Setting: `{"billing_preference":"subscription_only"}`, AffCode: "window-http-user"}
				require.NoError(t, db.Create(&buyer).Error)
				plan := model.SubscriptionPlan{Title: "HTTP windows", PriceAmount: 1, Currency: "USD", DurationUnit: model.SubscriptionDurationMonth, DurationValue: 1, TotalAmount: 500, Enabled: true, WindowRules: model.SubscriptionWindowRules{{ID: "five-hour", DurationSeconds: 5 * 3600, Limit: 60}, {ID: "week", DurationSeconds: 7 * 24 * 3600, Limit: 200}}}
				require.NoError(t, db.Create(&plan).Error)
				draft, err := model.GetSubscriptionPlanDraft(db, plan.Id)
				require.NoError(t, err)
				at := common.GetTimestamp() - 10
				published, err := model.PublishSubscriptionPlanVersion(db, model.SubscriptionVersionPublish{PlanID: plan.Id, ActorID: admin.Id, ExpectedPlanDigest: draft.Digest, EventID: "window-http-publish"}, at)
				require.NoError(t, err)
				order, err := model.CreateSubscriptionPurchaseOrder(db, model.SubscriptionPurchaseInput{UserID: buyer.Id, VersionID: published.ID, Provider: "stripe", EventID: "window-http-purchase", ExpiresAt: at + 3600}, at)
				require.NoError(t, err)
				money, paidAt := int64(1000000), at+1
				_, err = model.RecordSubscriptionPaymentFact(db, model.VerifiedSubscriptionPayment{OrderID: order.ID, Provider: "stripe", EventID: "window-http-paid", ReferenceID: "window-http-transaction", BuyerID: buyer.Id, AmountMicros: &money, Currency: "USD", PaidAt: &paidAt, PaidAtSource: "fixture.gateway", Succeeded: true, EvidenceDigest: strings.Repeat("a", 64)}, at+1)
				require.NoError(t, err)
				rights, err := model.ActivateSubscriptionPurchase(db, buyer.Id, order.ID, at+1)
				require.NoError(t, err)
				keys := []model.Token{{UserId: buyer.Id, Key: "windowa0" + strings.Repeat("0", 40), Status: common.TokenStatusEnabled, RemainQuota: 1000, ExpiredTime: -1, Group: "default"}, {UserId: buyer.Id, Key: "windowb0" + strings.Repeat("0", 40), Status: common.TokenStatusEnabled, UnlimitedQuota: true, ExpiredTime: -1, Group: "default"}}
				require.NoError(t, db.Create(&keys).Error)
				invoke := func(key model.Token) *httptest.ResponseRecorder {
					r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"credit-model","messages":[{"role":"user","content":"hi"}]}`))
					r.Header.Set("Authorization", "Bearer sk-"+key.Key)
					r.Header.Set("Content-Type", "application/json")
					w := httptest.NewRecorder()
					engine.ServeHTTP(w, r)
					return w
				}
				before := calls.Load()
				first := invoke(keys[0])
				require.Equal(t, http.StatusOK, first.Code, first.Body.String())
				assert.Equal(t, before+1, calls.Load())
				selfAPI := gin.New()
				selfAPI.GET("/self", func(c *gin.Context) { c.Set("id", buyer.Id); GetSubscriptionSelf(c) })
				selfResponse := httptest.NewRecorder()
				selfAPI.ServeHTTP(selfResponse, httptest.NewRequest(http.MethodGet, "/self?user_id=1", nil))
				require.Equal(t, http.StatusOK, selfResponse.Code, selfResponse.Body.String())
				var self struct {
					Data struct {
						ServerTime int64            `json:"server_time"`
						Windows    []map[string]any `json:"windows"`
					} `json:"data"`
				}
				require.NoError(t, common.Unmarshal(selfResponse.Body.Bytes(), &self))
				assert.GreaterOrEqual(t, self.Data.ServerTime, at)
				assert.Len(t, self.Data.Windows, 3)
				blocked := invoke(keys[1])
				assert.Equal(t, http.StatusForbidden, blocked.Code, blocked.Body.String())
				assert.Contains(t, blocked.Body.String(), `"code":"subscription_window_insufficient"`)
				assert.Equal(t, before+1, calls.Load(), "unlimited Key cannot bypass the user's shared five-hour window")
				var windows []model.SubscriptionWindow
				require.NoError(t, db.Where("subscription_id = ?", rights.Id).Find(&windows).Error)
				require.Len(t, windows, 3)
				for _, window := range windows {
					assert.EqualValues(t, 35, window.Used)
					assert.Zero(t, window.Held)
				}
				_, err = model.GrantCreditPack(db, model.CreditGrant{UserID: buyer.Id, SourceType: "fixture", SourceID: "window-http-booster", Amount: 100, StartsAt: at, ExpiresAt: at + 180*24*3600, UseMask: model.CreditUseAPI}, at)
				require.NoError(t, err)
				require.NoError(t, db.Model(&buyer).Update("setting", `{"billing_preference":"subscription_first"}`).Error)
				fallback := invoke(keys[1])
				require.Equal(t, http.StatusOK, fallback.Code, fallback.Body.String())
				assert.Equal(t, before+2, calls.Load())
				require.NoError(t, db.Where("subscription_id = ?", rights.Id).Find(&windows).Error)
				for _, window := range windows {
					assert.EqualValues(t, 35, window.Used)
					assert.Zero(t, window.Held)
				}
				packs, err := model.ListCreditPacks(db, buyer.Id, common.GetTimestamp())
				require.NoError(t, err)
				require.Len(t, packs, 1)
				assert.EqualValues(t, 35, packs[0].Spent)
				assert.EqualValues(t, 65, packs[0].Available)
				var bills []model.CreditRequest
				require.NoError(t, db.Where("user_id = ?", buyer.Id).Order("id asc").Find(&bills).Error)
				require.Len(t, bills, 2)
				assert.Equal(t, model.SubscriptionWindowFundingSource, bills[0].FundingSource)
				assert.Equal(t, model.CreditFundingSource, bills[1].FundingSource)
				differences, err := model.ReconcileCreditAccount(db, buyer.Id)
				require.NoError(t, err)
				assert.Empty(t, differences)
			})
			summary, err := service.RunCreditRecoveryPass(context.Background(), db, db, "e2e-recovery", common.GetTimestamp())
			require.NoError(t, err)
			assert.Zero(t, summary.Errors)
			assert.Zero(t, summary.PendingLogs)
			var consumeCount int64
			require.NoError(t, db.Model(&model.Log{}).Where("user_id = ? AND type = ?", user.Id, model.LogTypeConsume).Count(&consumeCount).Error)
			assert.EqualValues(t, 1, consumeCount)
			require.NoError(t, db.First(&user, user.Id).Error)
			assert.Equal(t, 35, user.UsedQuota)
			assert.Equal(t, 1, user.RequestCount)
			assert.Equal(t, 35, token.UsedQuota)
			_, err = model.FinishCreditRequest(db, user.Id, bill.ID, "settle", 35, now)
			require.NoError(t, err, "completion survives losing the in-memory session")
			require.NoError(t, db.First(&token, token.Id).Error)
			assert.Equal(t, 965, token.RemainQuota)
		})
	}
}

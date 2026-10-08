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
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
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
	pluginruntime "github.com/QuantumNous/new-api/pkg/jsplugin"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting"
	"github.com/QuantumNous/new-api/setting/config"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
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
	previousCount, previousBatch, previousLogs, previousUnit := constant.CountToken, common.BatchUpdateEnabled, common.LogConsumeEnabled, common.QuotaPerUnit
	constant.CountToken, common.BatchUpdateEnabled, common.LogConsumeEnabled, common.QuotaPerUnit = false, false, true, 500000
	t.Cleanup(func() {
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
					"billing_setting.billing_mode": `{"credit-model":"tiered_expr","credit-image":"tiered_expr","credit-audio":"tiered_expr","credit-embedding":"tiered_expr"}`,
					"billing_setting.billing_expr": `{"credit-model":"tier(\"request\", fixed(0.00007))","credit-image":"tier(\"image\", fixed(0.00002)) * image_count","credit-audio":"tier(\"speech\", fixed(0.00004))","credit-embedding":"tier(\"embedding\", fixed(0.00003))"}`,
				}))
				for _, tc := range []struct {
					name, path, body, reply, contentType string
					reserved, charged                    int64
				}{
					{"image", "/v1/images/generations", `{"model":"credit-image","prompt":"hi","n":1}`, `{"created":1,"data":[{"url":"https://example.test/image"},{"b64_json":"aW1hZ2U="},{"revised_prompt":"hi"}]}`, "application/json", 30, 10},
					{"audio", "/v1/audio/speech", `{"model":"credit-audio","input":"hi","voice":"alloy","response_format":"pcm"}`, strings.Repeat("a", 48000), "audio/pcm", 20, 20},
					{"embedding", "/v1/embeddings", `{"model":"credit-embedding","input":"hi"}`, `{"object":"list","data":[{"object":"embedding","embedding":[0.1,0.2],"index":0}],"model":"credit-embedding","usage":{"prompt_tokens":3,"total_tokens":3}}`, "application/json", 15, 15},
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
						key := model.Token{UserId: u.Id, Key: strings.Repeat(tc.name[:1], 48), Status: common.TokenStatusEnabled, RemainQuota: 100, ExpiredTime: -1, Group: "default"}
						require.NoError(t, db.Create(&key).Error)
						_, err := model.GrantCreditPack(db, model.CreditGrant{UserID: u.Id, SourceType: "test", SourceID: modelName, Amount: 100, StartsAt: now, ExpiresAt: now + 3600, UseMask: model.CreditUseAPI}, now)
						require.NoError(t, err)
						e := gin.New()
						e.Use(middleware.RequestId())
						format := types.RelayFormat(types.RelayFormatOpenAIImage)
						if tc.name == "audio" {
							format = types.RelayFormatOpenAIAudio
						}
						if tc.name == "embedding" {
							format = types.RelayFormatEmbedding
						}
						e.POST(tc.path, middleware.TokenAuth(), middleware.Distribute(), func(c *gin.Context) { Relay(c, format) })
						r := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body))
						r.Header.Set("Authorization", "Bearer sk-"+key.Key)
						r.Header.Set("Content-Type", "application/json")
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
			t.Run("task_plugin_keeps_holds_until_terminal_and_replays_once", func(t *testing.T) {
				require.NoError(t, db.AutoMigrate(&model.Task{}))
				const expression = `tier("work", u("units") * 0.00001)`
				withTieredBillingConfig(t, map[string]string{"credit-task": "tiered_expr"}, map[string]string{"credit-task": expression})
				plugin, err := pluginruntime.CompilePlugin(`
export const meta={apiVersion:1,key:"credit-task",name:"Credit task",version:"1.0.0",author:{name:"Test"},models:["credit-task"],fetchMode:"per_task",usageSchema:{units:{type:"number",unit:"count",description:{en:"Work unit price",zh:"处理单价"}}}};
export function buildSubmitRequest(ctx){return {url:ctx.baseUrl+"/jobs",body:ctx.requestBody};}
export function parseSubmitResponse(ctx,resp){return {taskId:"vendor-job",taskData:resp.body,immediate:resp.body.status?{status:resp.body.status}:undefined};}
export function extractUsage(){return {units:4};}
export function extractUsageOnComplete(ctx,result,body){return body.usage;}
export function parseTaskResult(){return {status:"SUCCESS"};}
export function buildQueryRequest(ctx){return {url:ctx.baseUrl+"/jobs/"+ctx.taskId};}
`, pluginruntime.Options{})
				require.NoError(t, err)
				for index, tc := range []struct {
					name, status  string
					units         int
					finalState    string
					charged, held int64
				}{
					{"pending", "", 6, "executing", 0, 20},
					{"immediate", "SUCCESS", 2, "settled", 10, 0},
					{"zero", "SUCCESS", 0, "settled", 0, 0},
					{"failed_unknown", "FAILURE", 0, "review", 0, 20},
				} {
					t.Run(tc.name, func(t *testing.T) {
						server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
							assert.Equal(t, "/jobs", r.URL.Path)
							w.Header().Set("Content-Type", "application/json")
							_, err := fmt.Fprintf(w, `{"status":%q,"usage":{"units":%d}}`, tc.status, tc.units)
							assert.NoError(t, err)
						}))
						defer server.Close()
						u := model.User{Username: "task-" + tc.name, AffCode: "task-" + tc.name, Group: "default", Status: common.UserStatusEnabled, AccountingVersion: 1}
						require.NoError(t, db.Create(&u).Error)
						key := model.Token{UserId: u.Id, Key: fmt.Sprintf("task%044d", index), Status: common.TokenStatusEnabled, RemainQuota: 100, ExpiredTime: -1}
						require.NoError(t, db.Create(&key).Error)
						_, err := model.GrantCreditPack(db, model.CreditGrant{UserID: u.Id, SourceType: "test", SourceID: tc.name, Amount: 100, StartsAt: now, ExpiresAt: now + 3600, UseMask: model.CreditUseAPI}, now)
						require.NoError(t, err)
						ch := model.Channel{Name: "task-test", Type: constant.ChannelTypeTaskPlugin, Status: common.ChannelStatusEnabled}
						require.NoError(t, db.Create(&ch).Error)
						c := taskSubmissionTestContext()
						c.Set("group", "default")
						c.Set("username", u.Username)
						c.Set("task_request", map[string]any{"model": "credit-task"})
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
						assert.Equal(t, tc.finalState, bill.State)
						assert.Equal(t, tc.charged, bill.Charged)
						assert.Equal(t, bill.ID, outcome.Task.PrivateData.CreditRequestID)
						packs, err := model.ListCreditPacks(db, u.Id, now)
						require.NoError(t, err)
						assert.Equal(t, tc.held, packs[0].Held)
						if tc.name == "pending" {
							var stored model.Task
							require.NoError(t, db.First(&stored, outcome.Task.ID).Error)
							stored.Status = model.TaskStatusSuccess
							require.NoError(t, stored.Update())
							require.NoError(t, db.Callback().Update().Before("gorm:update").Register("test:credit-task-projection", func(tx *gorm.DB) {
								if tx.Statement.Table == "tasks" {
									tx.AddError(fmt.Errorf("task quota projection unavailable"))
								}
							}))
							service.RecalculateTaskQuota(c, &stored, 30, "completion write failure")
							require.NoError(t, db.Callback().Update().Remove("test:credit-task-projection"))
							require.NoError(t, db.First(&bill, bill.ID).Error)
							assert.Equal(t, "pending", bill.State)
							assert.Zero(t, bill.Charged)
							packs, err = model.ListCreditPacks(db, u.Id, now)
							require.NoError(t, err)
							assert.EqualValues(t, 20, packs[0].Held)
							assert.Zero(t, packs[0].Spent)
							service.RecalculateTaskQuota(c, &stored, 30, "verified completion")
							service.RecalculateTaskQuota(c, &stored, 30, "verified completion replay")
							require.NoError(t, db.First(&bill, bill.ID).Error)
							assert.EqualValues(t, 30, bill.Charged)
							assert.Equal(t, "settled", bill.State)
							require.NoError(t, db.First(&key, key.Id).Error)
							assert.Equal(t, 70, key.RemainQuota)
							assert.Equal(t, 30, key.UsedQuota)
							assert.False(t, service.RefundTaskQuota(c, &stored, "late failure"), "terminal success cannot be overwritten by failure")
						}
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
				require.NoError(t, db.First(&key, key.Id).Error)
				assert.Equal(t, 52, key.RemainQuota)
				assert.Equal(t, 48, key.UsedQuota)
			})
			t.Run("midjourney_holds_then_settles_without_legacy_balance", func(t *testing.T) {
				require.NoError(t, db.AutoMigrate(&model.Midjourney{}))
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
				require.NoError(t, service.CompleteMidjourneyCreditBilling(&task))
				require.NoError(t, service.CompleteMidjourneyCreditBilling(&task))
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
						body := fmt.Sprintf(`{"type":"response.completed","response":{"id":"resp_%d","object":"response","status":"completed","model":"credit-ws","output":[],"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}}`, i)
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
				for _, bill := range bills {
					assert.Equal(t, "settled", bill.State)
					assert.NotZero(t, bill.SubmittedAt)
					assert.EqualValues(t, 10, bill.Charged)
				}
				assert.NotEqual(t, bills[0].RequestID, bills[1].RequestID)
				assert.EqualValues(t, 1, handshakes.Load())
				require.NoError(t, db.First(&key, key.Id).Error)
				assert.Equal(t, 80, key.RemainQuota)
				assert.Equal(t, 20, key.UsedQuota)
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
			assert.Positive(t, summary.Logs)
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

package controller

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	pluginruntime "github.com/QuantumNous/new-api/pkg/jsplugin"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/config"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

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
			require.NoError(t, db.AutoMigrate(&model.Token{}, &model.Log{}))
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
				assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
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
					_, err := fmt.Fprint(w, `{"code":1,"description":"accepted","result":"credit-mj-job"}`)
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
				financeAPI.POST("/reviews", AdminOpenCreditReview)
				financeAPI.POST("/reviews/cash-outcome", AdminRecordCreditCashOutcome)
				readOnly, _ := createScopedAccessToken(t, actor.Id, 0, "billing:read", "option:read")
				writeOnly, _ := createScopedAccessToken(t, actor.Id, 0, "billing:write", "option:write")
				for _, endpoint := range []struct{ method, path string }{
					{http.MethodPost, "/api/credit/admin/grants"},
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
				for _, path := range []string{"/api/credit/admin/policies", fmt.Sprintf("/api/credit/admin/reviews?user_id=%d", user.Id)} {
					assert.Equal(t, http.StatusOK, accessTokenRequest(adminAPI, http.MethodGet, path, readOnly, "", "").Code)
					assert.Equal(t, http.StatusForbidden, accessTokenRequest(adminAPI, http.MethodGet, path, writeOnly, "", "").Code)
				}
				expired, _ := createScopedAccessToken(t, actor.Id, now-1, "billing:write")
				assert.Equal(t, http.StatusUnauthorized, accessTokenRequest(adminAPI, http.MethodPost, "/api/credit/admin/grants", expired, "", `{}`).Code)
				revoked, revokedToken := createScopedAccessToken(t, actor.Id, 0, "billing:write")
				require.NoError(t, db.Delete(revokedToken).Error)
				assert.Equal(t, http.StatusUnauthorized, accessTokenRequest(adminAPI, http.MethodPost, "/api/credit/admin/grants", revoked, "", `{}`).Code)
				require.NoError(t, db.Model(&actor).Update("role", common.RoleAdminUser).Error)
				assert.Equal(t, http.StatusForbidden, accessTokenRequest(adminAPI, http.MethodPut, "/api/credit/admin/policies", writeOnly, "", `{}`).Code)
				require.NoError(t, db.Model(&actor).Update("role", common.RoleCommonUser).Error)
				assert.Equal(t, http.StatusForbidden, accessTokenRequest(adminAPI, http.MethodPost, "/api/credit/admin/grants", writeOnly, "", `{}`).Code)
				require.NoError(t, db.Model(&actor).Update("role", common.RoleRootUser).Error)
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
			assert.Equal(t, 35, token.UsedQuota)
			_, err = model.FinishCreditRequest(db, user.Id, bill.ID, "settle", 35, now)
			require.NoError(t, err, "completion survives losing the in-memory session")
			require.NoError(t, db.First(&token, token.Id).Error)
			assert.Equal(t, 965, token.RemainQuota)
		})
	}
}

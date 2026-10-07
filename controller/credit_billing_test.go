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

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/config"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
			require.NoError(t, db.AutoMigrate(&model.Token{}))
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
			engine.POST("/v1/images/generations", middleware.TokenAuth(), middleware.Distribute(), func(c *gin.Context) { Relay(c, types.RelayFormatOpenAI) })
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
			t.Run("unintegrated_billing_path_fails_before_upstream", func(t *testing.T) {
				r := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(`{"model":"credit-model","prompt":"hi","n":1}`))
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

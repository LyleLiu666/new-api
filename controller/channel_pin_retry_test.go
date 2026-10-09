package controller

import (
	"bytes"
	"errors"
	"fmt"
	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/setting"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/service/authz"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

func TestShouldRetryHonorsPinRetryMode(t *testing.T) {
	openaiErr := types.NewOpenAIError(errors.New("upstream"), types.ErrorCodeBadResponseStatusCode, http.StatusInternalServerError)

	c := newPinRetryContext()
	assert.True(t, service.ShouldRetryRelayError(c, openaiErr, 1))

	origin := newPinRetryContext()
	service.GetChannelConstraints(origin).AddPin(dto.ChannelPin{
		ChannelId: 2,
		Source:    dto.PinSourceOriginTask,
		Rank:      dto.PinRankOriginTask,
		RetryMode: dto.PinRetrySameChannel,
	})
	assert.True(t, service.ShouldRetryRelayError(origin, openaiErr, 1), "origin pin retries on the same channel")

	token := newPinRetryContext()
	service.GetChannelConstraints(token).AddPin(dto.ChannelPin{
		ChannelId: 1,
		Source:    dto.PinSourceToken,
		Rank:      dto.PinRankToken,
		RetryMode: dto.PinRetrySingleAttempt,
	})
	assert.Equal(t, service.PolicyDecision{Action: "stop", Reason: "pinned_channel", Source: "channel_constraint"}, service.DecideRelayRetry(token, openaiErr, 1), "token pin suppresses retry")
}

func TestShouldRetryTaskRelayHonorsPinRetryMode(t *testing.T) {
	taskErr := &dto.TaskError{StatusCode: http.StatusInternalServerError}

	c := newPinRetryContext()
	assert.Equal(t, "retry", decideTaskRetry(c, taskErr, 1).Action)

	origin := newPinRetryContext()
	service.GetChannelConstraints(origin).AddPin(dto.ChannelPin{
		ChannelId: 2,
		Source:    dto.PinSourceOriginTask,
		Rank:      dto.PinRankOriginTask,
		RetryMode: dto.PinRetrySameChannel,
	})
	assert.Equal(t, "retry", decideTaskRetry(origin, taskErr, 1).Action)

	token := newPinRetryContext()
	service.GetChannelConstraints(token).AddPin(dto.ChannelPin{
		ChannelId: 1,
		Source:    dto.PinSourceToken,
		Rank:      dto.PinRankToken,
		RetryMode: dto.PinRetrySingleAttempt,
	})
	assert.Equal(t, service.PolicyDecision{Action: "stop", Reason: "pinned_channel", Source: "channel_constraint"}, decideTaskRetry(token, taskErr, 1))
}

func TestSameChannelPinsMergeToStricterRetryMode(t *testing.T) {
	c := newPinRetryContext()
	constraints := service.GetChannelConstraints(c)
	constraints.AddPin(dto.ChannelPin{
		ChannelId: 7,
		Source:    dto.PinSourceOriginTask,
		Rank:      dto.PinRankOriginTask,
		RetryMode: dto.PinRetrySameChannel,
	})
	constraints.AddPin(dto.ChannelPin{
		ChannelId: 7,
		Source:    dto.PinSourceToken,
		Rank:      dto.PinRankToken,
		RetryMode: dto.PinRetrySingleAttempt,
	})
	pin, found, overridden := constraints.ResolvedPin()
	require.True(t, found)
	assert.Equal(t, 7, pin.ChannelId)
	assert.Equal(t, dto.PinRetrySingleAttempt, pin.RetryMode)
	assert.Empty(t, overridden)
	assert.False(t, service.ShouldRetryRelayError(c, types.NewOpenAIError(errors.New("upstream"), types.ErrorCodeBadResponseStatusCode, http.StatusInternalServerError), 1))
}

func newPinRetryContext() *gin.Context {
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	return c
}

func TestAccountAffinityDatabaseMatrix(t *testing.T) {
	for _, dialect := range []struct{ kind, env string }{{"sqlite", ""}, {"mysql", "TEST_MYSQL_DSN"}, {"postgres", "TEST_POSTGRES_DSN"}} {
		t.Run(dialect.kind, func(t *testing.T) {
			dsn := os.Getenv(dialect.env)
			if dialect.env != "" && dsn == "" {
				t.Skipf("%s not configured", dialect.env)
			}
			db := modelManagementDB(t, dialect.kind, dsn)
			require.NoError(t, db.AutoMigrate(&model.UpstreamAccount{}, &model.UpstreamSessionBinding{}))
			channel := model.Channel{Id: 1, Type: 1, Name: "accounts", Key: "credential-A\ncredential-B", Status: common.ChannelStatusEnabled, Models: "policy-test", Group: "default", ChannelInfo: model.ChannelInfo{IsMultiKey: true, MultiKeyMode: constant.MultiKeyModePolling}}
			require.NoError(t, db.Create(&channel).Error)
			require.NoError(t, channel.AddAbilities(db))
			affinity := operation_setting.GetChannelAffinitySetting()
			old := *affinity
			t.Cleanup(func() { *affinity = old })
			*affinity = operation_setting.ChannelAffinitySetting{Enabled: true, SessionMode: "strict", DefaultTTLSeconds: 3600, Rules: []operation_setting.ChannelAffinityRule{{Name: "account-test", ModelRegex: []string{".*"}, KeySources: []operation_setting.ChannelAffinityKeySource{{Type: "request_header", Key: "X-Session"}}, SessionMode: "inherit", IncludeRuleName: true}}}
			request := func(user int, session string) *gin.Context {
				c := newPinRetryContext()
				c.Set("id", user)
				c.Request.Header.Set("X-Session", session)
				common.SetContextKey(c, constant.ContextKeyUsingGroup, "default")
				return c
			}
			t.Run("strict_pins_actual_account", func(t *testing.T) {
				first := request(1, t.Name())
				_, _ = service.GetPreferredChannelByAffinity(first, "policy-test", "default")
				selected, err := model.GetChannelById(1, true)
				require.NoError(t, err)
				require.Nil(t, middleware.SetupContextForSelectedChannel(first, selected, "policy-test"))
				key := common.GetContextKeyString(first, constant.ContextKeyChannelKey)
				service.RecordChannelAffinity(first, 1)
				t.Cleanup(func() { service.ClearCurrentChannelAffinityCache(first) })
				second := request(1, t.Name())
				bound, found := service.GetPreferredChannelByAffinity(second, "policy-test", "default")
				require.True(t, found)
				require.Equal(t, 1, bound)
				selected, err = model.GetChannelById(1, true)
				require.NoError(t, err)
				require.Nil(t, middleware.SetupContextForSelectedChannel(second, selected, "policy-test"))
				assert.Equal(t, key, common.GetContextKeyString(second, constant.ContextKeyChannelKey), "strict session must use the same upstream account")
				service.RequestPolicy(second).Attempts = 2
				require.Nil(t, middleware.SetupContextForSelectedChannel(second, selected, "policy-test"))
				assert.Equal(t, key, common.GetContextKeyString(second, constant.ContextKeyChannelKey), "a retry selection cannot rotate a strict account")
			})
			t.Run("concurrent_strict_claim_and_expiry", func(t *testing.T) {
				other := model.Channel{Id: 2, Type: 1, Name: "other", Key: "credential-C", Status: common.ChannelStatusEnabled, Models: "policy-test", Group: "default"}
				require.NoError(t, db.Create(&other).Error)
				require.NoError(t, other.AddAbilities(db))
				var selected [2]model.UpstreamAccountSelection
				var failures [2]error
				start := make(chan struct{})
				var workers sync.WaitGroup
				for i := range 2 {
					workers.Go(func() {
						<-start
						selected[i], failures[i] = model.SelectUpstreamAccount(db, model.UpstreamAccountRequest{ChannelID: i + 1, UserID: 10, Scope: t.Name(), RuleName: "race", TTLSeconds: 300, Strict: true, UseBinding: true, Now: 1000})
					})
				}
				close(start)
				workers.Wait()
				for _, err := range failures {
					require.NoError(t, err)
				}
				assert.Equal(t, selected[0].Account.ID, selected[1].Account.ID, "concurrent first requests must share one account")
				assert.Equal(t, selected[0].Channel.Id, selected[1].Channel.Id)
				renewed, err := model.SelectUpstreamAccount(db, model.UpstreamAccountRequest{ChannelID: 2, UserID: 10, Scope: t.Name(), RuleName: "race", TTLSeconds: 300, Strict: true, UseBinding: true, Now: 1300})
				require.NoError(t, err)
				assert.Equal(t, 2, renewed.Channel.Id, "expired binding can be established again")
				assert.Equal(t, "credential-C", renewed.Key)
			})
			t.Run("off_balances_and_prefer_falls_back", func(t *testing.T) {
				first, err := model.SelectUpstreamAccount(db, model.UpstreamAccountRequest{ChannelID: 1, UserID: 11, Scope: "", RuleName: "", TTLSeconds: 300, Strict: false, UseBinding: false, Now: 1000})
				require.NoError(t, err)
				second, err := model.SelectUpstreamAccount(db, model.UpstreamAccountRequest{ChannelID: 1, UserID: 11, Scope: "", RuleName: "", TTLSeconds: 300, Strict: false, UseBinding: false, Now: 1000})
				require.NoError(t, err)
				assert.NotEqual(t, first.Account.ID, second.Account.ID, "polling distributes across enabled accounts")
				scope := t.Name()
				binding := model.UpstreamSessionBinding{Digest: model.UpstreamScopeDigest(scope), UserID: 11, RuleName: "prefer", ChannelID: 1, AccountID: first.Account.ID, ExpiresAt: 1300}
				require.NoError(t, db.Create(&binding).Error)
				current, err := model.GetChannelById(1, true)
				require.NoError(t, err)
				current.ChannelInfo.MultiKeyStatusList = map[int]int{first.Index: common.ChannelStatusManuallyDisabled}
				require.NoError(t, current.Update())
				fallback, err := model.SelectUpstreamAccount(db, model.UpstreamAccountRequest{ChannelID: 1, UserID: 11, Scope: scope, RuleName: "prefer", TTLSeconds: 300, Strict: false, UseBinding: true, Now: 1000})
				require.NoError(t, err)
				assert.Equal(t, second.Account.ID, fallback.Account.ID)
				_, err = model.SelectUpstreamAccount(db, model.UpstreamAccountRequest{ChannelID: 1, UserID: 11, Scope: scope, RuleName: "strict", TTLSeconds: 300, Strict: true, UseBinding: true, Now: 1000})
				require.ErrorIs(t, err, model.ErrUpstreamAccountUnavailable)
				current.ChannelInfo.MultiKeyStatusList = nil
				require.NoError(t, current.Update())
			})

			t.Run("rotation_reorder_and_retirement", func(t *testing.T) {
				first := request(3, t.Name())
				_, _ = service.GetPreferredChannelByAffinity(first, "policy-test", "default")
				selected, err := model.GetChannelById(1, true)
				require.NoError(t, err)
				require.Nil(t, middleware.SetupContextForSelectedChannel(first, selected, "policy-test"))
				identity := first.GetString("upstream_account_id")
				require.NotEmpty(t, identity)
				version := first.GetInt64("upstream_credential_version")
				require.EqualValues(t, 1, version)
				oldKey := common.GetContextKeyString(first, constant.ContextKeyChannelKey)
				rotated, err := model.RotateUpstreamCredential(db, 1, identity, version, "credential-rotated", common.GetTimestamp())
				require.NoError(t, err)
				assert.Equal(t, identity, rotated.ID)
				assert.EqualValues(t, 2, rotated.CredentialVersion)
				_, err = model.RotateUpstreamCredential(db, 1, identity, version, "stale-write", common.GetTimestamp())
				require.ErrorIs(t, err, model.ErrUpstreamCredentialConflict)
				selected, err = model.GetChannelById(1, true)
				require.NoError(t, err)
				keys := selected.GetKeys()
				slices.Reverse(keys)
				selected.Key = strings.Join(keys, "\n")
				require.NoError(t, selected.Update())
				next := request(3, t.Name())
				_, found := service.GetPreferredChannelByAffinity(next, "policy-test", "default")
				require.True(t, found)
				require.Nil(t, middleware.SetupContextForSelectedChannel(next, selected, "policy-test"))
				assert.Equal(t, identity, next.GetString("upstream_account_id"))
				assert.EqualValues(t, 2, next.GetInt64("upstream_credential_version"))
				assert.Equal(t, "credential-rotated", common.GetContextKeyString(next, constant.ContextKeyChannelKey))
				remaining := "credential-A"
				if oldKey == remaining {
					remaining = "credential-B"
				}
				selected.Key = remaining
				require.NoError(t, selected.Update())
				// Removing and re-adding without an intervening request must not
				// resurrect the retired account's identity.
				selected.Key = "credential-rotated\n" + remaining
				require.NoError(t, selected.Update())
				blocked := request(3, t.Name())
				_, _ = service.GetPreferredChannelByAffinity(blocked, "policy-test", "default")
				require.NotNil(t, middleware.SetupContextForSelectedChannel(blocked, selected, "policy-test"), "strict binding cannot silently replace a deleted account")
				selected.Key = "credential-A\ncredential-B"
				require.NoError(t, selected.Update())
				t.Cleanup(func() { service.ClearCurrentChannelAffinityCache(first) })
			})

			t.Run("rotation_excludes_credentials_from_sql_logs", func(t *testing.T) {
				for i, multi := range []bool{false, true} {
					t.Run(fmt.Sprintf("multi_%t", multi), func(t *testing.T) {
						current := model.Channel{Id: 100 + i, Type: 1, Name: "rotation-log", Key: "log-old-secret", Status: common.ChannelStatusEnabled, Models: "policy-test", Group: "default", ChannelInfo: model.ChannelInfo{IsMultiKey: multi}}
						if multi {
							current.Key += "\nlog-other-secret"
						}
						require.NoError(t, db.Create(&current).Error)
						t.Cleanup(func() { require.NoError(t, current.Delete()) })
						accounts, err := model.ListUpstreamAccounts(db, current.Id, 1000)
						require.NoError(t, err)
						var trace bytes.Buffer
						loggedDB := db.Session(&gorm.Session{Logger: gormlogger.New(log.New(&trace, "", 0), gormlogger.Config{LogLevel: gormlogger.Info, SlowThreshold: time.Nanosecond})})
						rotated, err := model.RotateUpstreamCredential(loggedDB, current.Id, accounts[0].ID, 1, "log-new-secret", 1000)
						require.NoError(t, err)
						assert.Equal(t, accounts[0].ID, rotated.ID)
						assert.EqualValues(t, 2, rotated.CredentialVersion)
						require.NoError(t, db.First(&current, current.Id).Error)
						assert.Equal(t, "log-new-secret", current.GetKeys()[0])
						assert.NotEmpty(t, trace.String(), "ordinary SQL tracing remains enabled")
						assert.NotContains(t, trace.String(), "log-new-secret")
						assert.NotContains(t, trace.String(), "log-other-secret", "rotation must also protect the other keys in a multi-key channel")
					})
				}
			})

			t.Run("single_key_health_ignores_rotated_credentials", func(t *testing.T) {
				for i, tc := range []struct {
					name            string
					initial, target int
				}{
					{"late_failure", common.ChannelStatusEnabled, common.ChannelStatusAutoDisabled},
					{"late_recovery", common.ChannelStatusAutoDisabled, common.ChannelStatusEnabled},
				} {
					t.Run(tc.name, func(t *testing.T) {
						current := model.Channel{Id: 102 + i, Type: 1, Name: "rotation-health", Key: "health-old-secret", Status: tc.initial, Models: "policy-test", Group: "default"}
						current.SetOtherInfo(map[string]any{"status_reason": "original health", "status_time": int64(100)})
						require.NoError(t, current.Insert())
						t.Cleanup(func() { require.NoError(t, current.Delete()) })
						accounts, err := model.ListUpstreamAccounts(db, current.Id, 1000)
						require.NoError(t, err)
						_, err = model.RotateUpstreamCredential(db, current.Id, accounts[0].ID, 1, "health-new-secret", 1000)
						require.NoError(t, err)
						before := current.OtherInfo
						assert.False(t, model.UpdateChannelStatus(current.Id, "health-old-secret", tc.target, "stale health"))
						require.NoError(t, db.First(&current, current.Id).Error)
						assert.Equal(t, tc.initial, current.Status)
						assert.Equal(t, before, current.OtherInfo)
						var ability model.Ability
						require.NoError(t, db.Where("channel_id = ?", current.Id).First(&ability).Error)
						assert.Equal(t, tc.initial == common.ChannelStatusEnabled, ability.Enabled)
						require.True(t, model.UpdateChannelStatus(current.Id, "health-new-secret", tc.target, "current health"))
						require.NoError(t, db.First(&current, current.Id).Error)
						assert.Equal(t, tc.target, current.Status)
						require.NoError(t, db.Where("channel_id = ?", current.Id).First(&ability).Error)
						assert.Equal(t, tc.target == common.ChannelStatusEnabled, ability.Enabled)
						require.True(t, model.UpdateChannelStatus(current.Id, "", tc.initial, "administrator operation"))
						require.NoError(t, db.First(&current, current.Id).Error)
						assert.Equal(t, tc.initial, current.Status)
						assert.Equal(t, "administrator operation", current.GetOtherInfo()["status_reason"])
					})
				}
			})

			t.Run("account_health_survives_key_reordering", func(t *testing.T) {
				current := model.Channel{Id: 5, Type: 1, Name: "health-reorder", Key: "health-first\nhealth-second", Status: common.ChannelStatusEnabled, Models: "policy-test", Group: "default", ChannelInfo: model.ChannelInfo{IsMultiKey: true, MultiKeyMode: constant.MultiKeyModePolling, MultiKeyStatusList: map[int]int{0: common.ChannelStatusManuallyDisabled}, MultiKeyDisabledReason: map[int]string{0: "administrator disabled"}, MultiKeyDisabledTime: map[int]int64{0: 100}}}
				require.NoError(t, current.Insert())
				accounts, err := model.ListUpstreamAccounts(db, current.Id, 1000)
				require.NoError(t, err)
				current.Key = "health-second\nhealth-first"
				require.NoError(t, current.Update())
				_, err = model.SelectUpstreamAccount(db, model.UpstreamAccountRequest{ChannelID: current.Id, UserID: 13, TTLSeconds: 300, Now: 1000, PinnedAccountID: accounts[0].ID})
				require.ErrorIs(t, err, model.ErrUpstreamAccountUnavailable, "disabled account must not become enabled when its position changes")
				require.NoError(t, db.First(&current, current.Id).Error)
				assert.Equal(t, "administrator disabled", current.ChannelInfo.MultiKeyDisabledReason[1])
				assert.EqualValues(t, 100, current.ChannelInfo.MultiKeyDisabledTime[1])
				before := current.ChannelInfo.MultiKeyPollingIndex
				_, err = model.SelectUpstreamAccount(db, model.UpstreamAccountRequest{ChannelID: current.Id, UserID: 13, TTLSeconds: 300, Now: 1000, PinnedAccountID: accounts[1].ID})
				require.NoError(t, err)
				require.NoError(t, db.First(&current, current.Id).Error)
				assert.Equal(t, before, current.ChannelInfo.MultiKeyPollingIndex, "querying a pinned task does not consume a balancing turn")
			})

			t.Run("task_preserves_submitting_account", func(t *testing.T) {
				selected, err := model.SelectUpstreamAccount(db, model.UpstreamAccountRequest{ChannelID: 1, UserID: 12, Scope: "", RuleName: "", TTLSeconds: 300, Strict: false, UseBinding: false, Now: 1000})
				require.NoError(t, err)
				info := &relaycommon.RelayInfo{UserId: 12, ChannelMeta: &relaycommon.ChannelMeta{ChannelId: 1, ChannelType: constant.ChannelTypeOpenAI, ChannelIsMultiKey: true, ApiKey: selected.Key, UpstreamAccountID: selected.Account.ID, UpstreamCredentialVersion: selected.Account.CredentialVersion}}
				task := model.InitTask(constant.TaskPlatform("fixture"), info)
				payload, err := common.Marshal(task.PrivateData)
				require.NoError(t, err)
				var private map[string]any
				require.NoError(t, common.Unmarshal(payload, &private))
				assert.Equal(t, selected.Account.ID, private["account_id"], "polling must address the actual submitting account")
				assert.EqualValues(t, selected.Account.CredentialVersion, private["credential_version"])
			})

			t.Run("channel_delete_retires_identity", func(t *testing.T) {
				current := model.Channel{Id: 4, Type: 1, Name: "deleted", Key: "deleted-key", Status: common.ChannelStatusEnabled, Models: "policy-test", Group: "default"}
				require.NoError(t, current.Insert())
				before, err := model.ListUpstreamAccounts(db, current.Id, common.GetTimestamp())
				require.NoError(t, err)
				require.Len(t, before, 1)
				require.NoError(t, current.Delete())
				require.NoError(t, current.Insert())
				after, err := model.ListUpstreamAccounts(db, current.Id, common.GetTimestamp())
				require.NoError(t, err)
				require.Len(t, after, 1)
				assert.NotEqual(t, before[0].ID, after[0].ID, "a deleted channel cannot resurrect the old account")
			})

			t.Run("conditional_delete_keeps_newly_enabled_channel", func(t *testing.T) {
				for i, kind := range []string{"by_status", "disabled"} {
					t.Run(kind, func(t *testing.T) {
						current := model.Channel{Id: 6 + i, Type: 1, Name: "reenabled-" + kind, Key: "reenabled-key", Status: common.ChannelStatusManuallyDisabled, Models: "policy-test", Group: "default"}
						require.NoError(t, current.Insert())
						_, err := model.ListUpstreamAccounts(db, current.Id, 1000)
						require.NoError(t, err)
						injected := false
						callback := "account-reenable-before-delete"
						require.NoError(t, db.Callback().Query().After("gorm:query").Register(callback, func(tx *gorm.DB) {
							if injected || tx.Statement.Table != "channels" {
								return
							}
							if _, isIDs := tx.Statement.Dest.(*[]int); !isIDs {
								return
							}
							injected = true
							require.NoError(t, db.Model(&model.Channel{}).Where("id = ?", current.Id).Update("status", common.ChannelStatusEnabled).Error)
						}))
						t.Cleanup(func() { require.NoError(t, db.Callback().Query().Remove(callback)) })
						var deleted int64
						if kind == "by_status" {
							deleted, err = model.DeleteChannelByStatus(common.ChannelStatusManuallyDisabled)
						} else {
							deleted, err = model.DeleteDisabledChannel()
						}
						require.NoError(t, err)
						require.True(t, injected)
						assert.Zero(t, deleted, "conditional deletion rechecks the locked current status")
						var stored model.Channel
						require.NoError(t, db.First(&stored, current.Id).Error)
						assert.Equal(t, common.ChannelStatusEnabled, stored.Status)
						accounts, err := model.ListUpstreamAccounts(db, current.Id, 1001)
						require.NoError(t, err)
						require.Len(t, accounts, 1)
						assert.Zero(t, accounts[0].RetiredAt)
					})
				}
			})

			t.Run("credential_api_permission_and_redaction", func(t *testing.T) {
				administrator := model.User{Id: 50, Username: "account-admin", AffCode: "account-admin", Role: common.RoleRootUser, Status: common.UserStatusEnabled}
				require.NoError(t, db.Create(&administrator).Error)
				accounts, err := model.ListUpstreamAccounts(db, 1, common.GetTimestamp())
				require.NoError(t, err)
				require.Len(t, accounts, 2)
				read := newPinRetryContext()
				read.Params = gin.Params{{Key: "id", Value: "1"}}
				read.Set("id", administrator.Id)
				read.Set("role", common.RoleRootUser)
				ListChannelAccounts(read)
				assert.Equal(t, http.StatusOK, read.Writer.Status())
				rendered, err := common.Marshal(accounts)
				require.NoError(t, err)
				assert.NotContains(t, string(rendered), "credential_digest")
				assert.NotContains(t, string(rendered), "credential-A")
				require.NoError(t, db.AutoMigrate(&model.UserAccessToken{}, &model.UserSession{}))
				api := gin.New()
				api.Use(middleware.RequestId())
				group := api.Group("/api/channel", middleware.AdminAuth())
				readPath := "/api/channel/:id/accounts"
				writePath := "/api/channel/:id/accounts/:account_id/credential"
				middleware.DeclareAccessTokenPermissionRoute(http.MethodGet, readPath, authz.ChannelRead)
				middleware.DeclareAccessTokenPermissionRoute(http.MethodPut, writePath, authz.ChannelSensitiveWrite)
				group.GET("/:id/accounts", middleware.RequirePermission(authz.ChannelRead), ListChannelAccounts)
				group.PUT("/:id/accounts/:account_id/credential", middleware.RequirePermission(authz.ChannelSensitiveWrite), RotateChannelAccountCredential)
				readOnly, _ := createScopedAccessToken(t, administrator.Id, 0, service.AccessTokenScopeOf(authz.ChannelRead))
				writeOnly, _ := createScopedAccessToken(t, administrator.Id, 0, service.AccessTokenScopeOf(authz.ChannelSensitiveWrite))
				expired, _ := createScopedAccessToken(t, administrator.Id, common.GetTimestamp()-1, service.AccessTokenScopeOf(authz.ChannelSensitiveWrite))
				list := accessTokenRequest(api, http.MethodGet, "/api/channel/1/accounts", readOnly, "", "")
				require.Equal(t, http.StatusOK, list.Code, list.Body.String())
				assert.NotContains(t, list.Body.String(), "credential_digest")
				assert.NotContains(t, list.Body.String(), "credential-A")
				path := "/api/channel/1/accounts/" + accounts[0].ID + "/credential"
				payload := `{"credential_version":1,"key":"api-rotated-secret"}`
				assert.Equal(t, http.StatusForbidden, accessTokenRequest(api, http.MethodPut, path, readOnly, "", payload).Code)
				assert.Equal(t, http.StatusUnauthorized, accessTokenRequest(api, http.MethodPut, path, expired, "", payload).Code)
				changed := accessTokenRequest(api, http.MethodPut, path, writeOnly, "", payload)
				require.Equal(t, http.StatusOK, changed.Code, changed.Body.String())
				assert.NotContains(t, changed.Body.String(), "api-rotated-secret")
				assert.NotContains(t, changed.Body.String(), "credential_digest")
				replay := accessTokenRequest(api, http.MethodPut, path, writeOnly, "", payload)
				require.Equal(t, http.StatusOK, replay.Code, replay.Body.String())
				assert.JSONEq(t, changed.Body.String(), replay.Body.String(), "lost response replay preserves account and version")
				conflict := accessTokenRequest(api, http.MethodPut, path, writeOnly, "", `{"credential_version":1,"key":"stale-other-secret"}`)
				assert.Equal(t, http.StatusConflict, conflict.Code)
				assert.NotContains(t, conflict.Body.String(), "stale-other-secret")
				require.NoError(t, db.Model(&administrator).Update("role", common.RoleCommonUser).Error)
				denied := newPinRetryContext()
				denied.Params = gin.Params{{Key: "id", Value: "1"}, {Key: "account_id", Value: accounts[0].ID}}
				denied.Set("id", administrator.Id)
				denied.Set("role", common.RoleRootUser)
				denied.Request = httptest.NewRequest(http.MethodPut, "/api/channel/1/accounts/ignored/credential", strings.NewReader(`{"credential_version":1,"key":"forbidden"}`))
				RotateChannelAccountCredential(denied)
				assert.Equal(t, http.StatusForbidden, denied.Writer.Status(), "stale context role cannot authorize rotation")
				current, err := model.GetChannelById(1, true)
				require.NoError(t, err)
				assert.NotContains(t, current.Key, "forbidden")
			})

			t.Run("same_session_isolated_by_user", func(t *testing.T) {
				first := request(1, t.Name())
				_, _ = service.GetPreferredChannelByAffinity(first, "policy-test", "default")
				selected, err := model.GetChannelById(1, true)
				require.NoError(t, err)
				require.Nil(t, middleware.SetupContextForSelectedChannel(first, selected, "policy-test"))
				service.RecordChannelAffinity(first, 1)
				t.Cleanup(func() { service.ClearCurrentChannelAffinityCache(first) })
				second := request(2, t.Name())
				_, found := service.GetPreferredChannelByAffinity(second, "policy-test", "default")
				assert.False(t, found, "a different authenticated user must not inherit another user's session")
			})
		})
	}
}

func TestRequestPolicyConfigReturnsSettingsWithoutMigration(t *testing.T) {
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	GetRequestPolicy(ctx)
	assert.Equal(t, http.StatusOK, recorder.Code)
	var response struct {
		Success bool           `json:"success"`
		Data    map[string]any `json:"data"`
	}
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
	assert.True(t, response.Success)
	assert.Contains(t, response.Data, "options")
	assert.NotContains(t, response.Data, "migration")
	assert.NotContains(t, response.Data, "differences")
}

func TestRequestPolicyRoutingDatabaseMatrix(t *testing.T) {
	require.NoError(t, i18n.Init())
	for _, dialect := range []struct{ kind, env string }{{"sqlite", ""}, {"mysql", "TEST_MYSQL_DSN"}, {"postgres", "TEST_POSTGRES_DSN"}} {
		t.Run(dialect.kind, func(t *testing.T) {
			dsn := ""
			if dialect.env != "" {
				dsn = os.Getenv(dialect.env)
				if dsn == "" {
					t.Skipf("%s not configured", dialect.env)
				}
			}
			db := modelManagementDB(t, dialect.kind, dsn)
			previousGroups := setting.UserUsableGroups2JSONString()
			previousRatios, err := common.Marshal(ratio_setting.GetGroupRatioCopy())
			require.NoError(t, err)
			require.NoError(t, setting.UpdateUserUsableGroupsByJSONString(`{"default":"Default"}`))
			require.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(`{"default":1}`))
			t.Cleanup(func() {
				require.NoError(t, setting.UpdateUserUsableGroupsByJSONString(previousGroups))
				require.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(string(previousRatios)))
			})
			// Channel 1 holds the session binding but is no longer usable.
			channels := []model.Channel{
				{Id: 1, Name: "A", Type: 1, Key: "test-only", Status: common.ChannelStatusAutoDisabled, Models: "policy-test", Group: "default", Priority: common.GetPointer(int64(10))},
				{Id: 2, Name: "B", Type: 1, Key: "test-only", Status: common.ChannelStatusEnabled, Models: "policy-test", Group: "default", Priority: common.GetPointer(int64(5))},
			}
			for i := range channels {
				require.NoError(t, db.Create(&channels[i]).Error)
				require.NoError(t, channels[i].AddAbilities(db))
			}
			affinity := operation_setting.GetChannelAffinitySetting()
			previousAffinity := *affinity
			t.Cleanup(func() { *affinity = previousAffinity })
			for _, cached := range []bool{false, true} {
				for _, keep := range []bool{false, true} {
					for _, tc := range []struct {
						globalMode, ruleMode string
						blocked              bool
					}{
						{"strict", "inherit", true},
						{"prefer", "strict", true},
						{"prefer", "inherit", false},
						{"strict", "prefer", false},
					} {
						t.Run(fmt.Sprintf("cache=%t/keep=%t/global=%s/rule=%s", cached, keep, tc.globalMode, tc.ruleMode), func(t *testing.T) {
							common.MemoryCacheEnabled = cached
							model.InitChannelCache()
							snapshot, err := model.BuildRequestPolicy(map[string]string{
								"channel_affinity_setting.enabled":                  "true",
								"channel_affinity_setting.session_mode":             tc.globalMode,
								"channel_affinity_setting.keep_on_channel_disabled": fmt.Sprint(keep),
								"channel_affinity_setting.rules":                    fmt.Sprintf(`[{"name":"session","model_regex":[".*"],"key_sources":[{"type":"request_header","key":"X-Session"}],"session_mode":%q}]`, tc.ruleMode),
							})
							require.NoError(t, err)
							*affinity = snapshot.Affinity
							seed := newPinRetryContext()
							seed.Request.Header.Set("X-Session", t.Name())
							_, found := service.GetPreferredChannelByAffinity(seed, "policy-test", "default")
							require.False(t, found)
							seed.Set("channel_id", 1)
							service.RecordChannelAffinity(seed, 1)
							t.Cleanup(func() { service.ClearCurrentChannelAffinityCache(seed) })
							bound, found := service.GetPreferredChannelByAffinity(seed, "policy-test", "default")
							require.True(t, found)
							require.Equal(t, 1, bound)

							request := newPinRetryContext()
							request.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"policy-test"}`))
							request.Request.Header.Set("Content-Type", "application/json")
							request.Request.Header.Set("X-Session", t.Name())
							common.SetContextKey(request, constant.ContextKeyUsingGroup, "default")
							middleware.Distribute()(request)
							events := service.RequestPolicy(request).Events()
							assert.True(t, slices.ContainsFunc(events, func(event service.PolicyEvent) bool { return event.Decision.Reason == "session_rule_matched" }), "decision events are recorded for every request")
							bound, found = service.GetPreferredChannelByAffinity(seed, "policy-test", "default")
							if tc.blocked {
								assert.Equal(t, http.StatusServiceUnavailable, request.Writer.Status())
								assert.True(t, request.IsAborted())
								assert.True(t, found, "strict bindings remain until expiry or explicit administrative clearing")
								if found {
									assert.Equal(t, 1, bound)
								}
								return
							}
							assert.False(t, request.IsAborted())
							assert.Equal(t, 2, common.GetContextKeyInt(request, constant.ContextKeyChannelId), "prefer falls back to the next eligible channel")
							require.True(t, found)
							assert.Equal(t, 2, bound, "a successful fallback rebinds the session")
						})
					}
				}
			}
		})
	}
}

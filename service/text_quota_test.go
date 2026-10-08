package service

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"image"
	"image/png"
	"math"
	"mime/multipart"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	hosttypes "github.com/QuantumNous/new-api/types"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

// The configured DSNs must point at isolated test databases. Each dialect runs
// the real reservation, settlement and log paths with the same billing cases.
func TestFixedPriceBillingDatabaseMatrix(t *testing.T) {
	for _, dialect := range []struct {
		name   common.DatabaseType
		env    string
		logEnv string
	}{
		{common.DatabaseTypeSQLite, "", ""},
		{common.DatabaseTypeMySQL, "TEST_FIXED_MYSQL_DSN", "TEST_FIXED_MYSQL_LOG_DSN"},
		{common.DatabaseTypePostgreSQL, "TEST_FIXED_POSTGRES_DSN", "TEST_FIXED_POSTGRES_LOG_DSN"},
	} {
		t.Run(string(dialect.name), func(t *testing.T) {
			var driver gorm.Dialector = sqlite.Open(":memory:")
			if dialect.env != "" {
				dsn := os.Getenv(dialect.env)
				if dsn == "" {
					t.Skip(dialect.env + " is not configured")
				}
				if dialect.name == common.DatabaseTypeMySQL {
					driver = mysql.Open(dsn)
				} else {
					driver = postgres.New(postgres.Config{DSN: dsn, PreferSimpleProtocol: true})
				}
			}
			db, err := gorm.Open(driver, &gorm.Config{})
			require.NoError(t, err)
			sqlDB, err := db.DB()
			require.NoError(t, err)
			sqlDB.SetMaxOpenConns(1)
			t.Cleanup(func() { require.NoError(t, sqlDB.Close()) })
			logDB := db
			if logDSN := os.Getenv(dialect.logEnv); logDSN != "" || dialect.name == common.DatabaseTypeSQLite {
				var logDriver gorm.Dialector = sqlite.Open(":memory:")
				if dialect.name == common.DatabaseTypeMySQL {
					logDriver = mysql.Open(logDSN)
				} else if dialect.name == common.DatabaseTypePostgreSQL {
					logDriver = postgres.New(postgres.Config{DSN: logDSN, PreferSimpleProtocol: true})
				}
				logDB, err = gorm.Open(logDriver, &gorm.Config{})
				require.NoError(t, err)
				logSQL, err := logDB.DB()
				require.NoError(t, err)
				logSQL.SetMaxOpenConns(1)
				t.Cleanup(func() { require.NoError(t, logSQL.Close()) })
			}
			oldDB, oldLogDB := model.DB, model.LOG_DB
			oldMainType, oldLogType := common.MainDatabaseType(), common.LogDatabaseType()
			model.DB, model.LOG_DB = db, logDB
			common.SetDatabaseTypes(dialect.name, dialect.name)
			t.Cleanup(func() { model.DB, model.LOG_DB = oldDB, oldLogDB; common.SetDatabaseTypes(oldMainType, oldLogType) })
			require.NoError(t, db.AutoMigrate(&model.User{}, &model.Token{}, &model.Channel{}))
			require.NoError(t, logDB.AutoMigrate(&model.Log{}))
			versionQuery := "select version()"
			if dialect.name == common.DatabaseTypeSQLite {
				versionQuery = "select sqlite_version()"
			}
			var version string
			require.NoError(t, db.Raw(versionQuery).Scan(&version).Error)
			t.Logf("database: %s", version)
			runFixedPriceAccountingCases(t, db, logDB)
		})
	}
}

func runFixedPriceAccountingCases(t *testing.T, db, logDB *gorm.DB) {
	t.Helper()
	const mixed = `len <= 32000 ? tier("short", fixed(0.01)) : tier("long", p * 2)`
	const flat = `tier("request", fixed(0.01))`
	const startingQuota = 2_000_000
	const imageExpression = `tier("standard", p * 5 + cr * 1.25 + img * 8 + img_cr * 2 + c * 30)`
	imageUsage := &dto.Usage{PromptTokens: 1000, CompletionTokens: 100, TotalTokens: 1100,
		PromptTokensDetails: dto.InputTokenDetails{CachedTokens: 300, ImageTokens: 600, CachedTokensDetails: &dto.CachedTokenDetails{ImageTokens: common.GetPointer(200)}}}
	operation_setting.SetToolPriceForTest("fixed_billing_tool", 4)
	t.Cleanup(func() { operation_setting.DeleteToolPriceForTest("fixed_billing_tool") })
	for index, tc := range []struct {
		name, expression                          string
		estimate                                  int
		usage                                     *dto.Usage
		audio, stream, refund, insufficient, tool bool
		realtime, reserveInsufficient             bool
		wallet, outboundImages                    int
		groupRatio                                float64
		want                                      int
		unit                                      billingexpr.BillingUnit
		requestedImages, actualImages             int
	}{
		{name: "missing usage charges once", expression: flat, want: 5000, unit: billingexpr.BillingUnitRequest},
		{name: "zero usage charges once", expression: flat, usage: &dto.Usage{}, want: 5000, unit: billingexpr.BillingUnitRequest},
		{name: "stream charges once", expression: flat, stream: true, usage: &dto.Usage{PromptTokens: 100, CompletionTokens: 20, TotalTokens: 120}, want: 5000, unit: billingexpr.BillingUnitRequest},
		{name: "audio zero usage charges once", expression: flat, audio: true, usage: &dto.Usage{}, want: 5000, unit: billingexpr.BillingUnitRequest},
		{name: "audio missing usage charges once", expression: flat, audio: true, want: 5000, unit: billingexpr.BillingUnitRequest},
		{name: "token reservation refunds to fixed price", expression: mixed, estimate: 50000, usage: &dto.Usage{PromptTokens: 100, TotalTokens: 100}, want: 5000, unit: billingexpr.BillingUnitRequest},
		{name: "fixed reservation settles token fallback", expression: mixed, estimate: 100, usage: &dto.Usage{PromptTokens: 50000, TotalTokens: 50000}, want: 50000, unit: billingexpr.BillingUnitToken},
		{name: "missing usage uses estimated token fallback", expression: mixed, estimate: 50000, want: 50000, unit: billingexpr.BillingUnitToken},
		{name: "evaluation error retains fixed reservation metadata", expression: `p == 50 ? tier("error", param("missing") * p + img_cr * 2) : tier("request", fixed(0.01))`, estimate: 100, usage: &dto.Usage{PromptTokens: 50, TotalTokens: 50}, want: 5000, unit: billingexpr.BillingUnitRequest},
		{name: "explicit zero remains free", expression: `tier("free", fixed(0))`, usage: &dto.Usage{PromptTokens: 100, TotalTokens: 100}, unit: billingexpr.BillingUnitRequest},
		{name: "multipliers and separate tool surcharge", expression: flat + ` * (param("fast") == true ? 2 : 1)`, groupRatio: 1.5, tool: true, want: 18000, unit: billingexpr.BillingUnitRequest},
		{name: "failed request refunds exactly once", expression: flat, refund: true},
		{name: "insufficient wallet never reserves tokens", expression: flat, insufficient: true},
		{name: "image cache stream settles usage and refunds unused reservation", expression: imageExpression, estimate: 10000, usage: imageUsage, stream: true, want: 4113, unit: billingexpr.BillingUnitToken},
		{name: "audio settlement records image cache billing inputs", expression: imageExpression, estimate: 10000, usage: imageUsage, audio: true, want: 4113, unit: billingexpr.BillingUnitToken},
		{name: "realtime records actual expression inputs", expression: imageExpression, estimate: 10000, usage: imageUsage, realtime: true, want: 4000, unit: billingexpr.BillingUnitToken},
		{name: "image cache insufficient wallet never reserves tokens", expression: imageExpression, estimate: 10000, insufficient: true},
		{name: "image quantity refunds missing images", expression: `tier("image", fixed(0.04)) * image_count`, requestedImages: 3, actualImages: 2, want: 40000, unit: billingexpr.BillingUnitRequest},
		{name: "image quantity keeps request when actual missing", expression: `tier("image", fixed(0.04)) * image_count`, requestedImages: 3, want: 60000, unit: billingexpr.BillingUnitRequest},
		{name: "image quantity zero price stays free", expression: `tier("image", fixed(0)) * image_count`, requestedImages: 3, actualImages: 2, unit: billingexpr.BillingUnitRequest},
		{name: "image quantity failure refunds reservation", expression: `tier("image", fixed(0.04)) * image_count`, requestedImages: 3, refund: true},
		{name: "image quantity exceeds one-image wallet before submission", expression: `tier("image", fixed(0.04)) * image_count`, requestedImages: 4, wallet: 20000, insufficient: true},
		{name: "image override reserves extra quantity", expression: `tier("image", fixed(0.04)) * image_count`, requestedImages: 1, outboundImages: 4, want: 80000, unit: billingexpr.BillingUnitRequest},
		{name: "image override cannot exceed remaining wallet", expression: `tier("image", fixed(0.04)) * image_count`, requestedImages: 1, outboundImages: 4, wallet: 40000, reserveInsufficient: true, refund: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			quota := startingQuota
			if tc.insufficient {
				quota = 1
			}
			if tc.wallet > 0 {
				quota = tc.wallet
			}
			user := model.User{Username: fmt.Sprintf("fixed_billing_%d", index), Quota: quota, Status: common.UserStatusEnabled}
			require.NoError(t, db.Create(&user).Error)
			token := model.Token{UserId: user.Id, Key: fmt.Sprintf("fixed-billing-test-%d", index), Name: "fixed-billing", RemainQuota: startingQuota, Status: common.TokenStatusEnabled}
			require.NoError(t, db.Create(&token).Error)
			channel := model.Channel{Name: "fixed-billing", Key: "unused", Status: common.ChannelStatusEnabled}
			require.NoError(t, db.Create(&channel).Error)
			t.Cleanup(func() {
				require.NoError(t, logDB.Where("user_id = ?", user.Id).Delete(&model.Log{}).Error)
				require.NoError(t, db.Unscoped().Delete(&token).Error)
				require.NoError(t, db.Unscoped().Delete(&user).Error)
				require.NoError(t, db.Unscoped().Delete(&channel).Error)
			})
			group := tc.groupRatio
			if group == 0 {
				group = 1
			}
			request := &billingexpr.RequestInput{Body: []byte(`{"fast":true}`)}
			if tc.requestedImages > 0 {
				request.ImageCount = &tc.requestedImages
			}
			cost, trace, err := billingexpr.RunExprWithRequest(tc.expression, billingexpr.TokenParams{P: float64(tc.estimate), Len: float64(tc.estimate)}, *request)
			require.NoError(t, err)
			reservation, err := billingexpr.QuotaRoundStrict(cost / 1_000_000 * common.QuotaPerUnit * group)
			require.NoError(t, err)
			snapshot := &billingexpr.BillingSnapshot{BillingMode: "tiered_expr", ExprString: tc.expression, ExprHash: billingexpr.ExprHashString(tc.expression), QuotaPerUnit: common.QuotaPerUnit, GroupRatio: group, EstimatedTier: trace.MatchedTier, EstimatedBillingUnit: trace.BillingUnit, EstimatedFixedPrice: trace.FixedPrice, EstimatedQuotaAfterGroup: reservation}
			snapshot.EstimatedImageCount = trace.ImageCount
			info := &relaycommon.RelayInfo{UserId: user.Id, TokenId: token.Id, TokenKey: token.Key, ChannelMeta: &relaycommon.ChannelMeta{ChannelId: channel.Id}, OriginModelName: "fixed-test", UsingGroup: "default", UserGroup: "default", UserSetting: dto.UserSetting{BillingPreference: "wallet_only"}, ForcePreConsume: true, StartTime: time.Now(), IsStream: tc.stream, RelayFormat: types.RelayFormatOpenAI, PriceData: hosttypes.PriceData{GroupRatioInfo: hosttypes.GroupRatioInfo{GroupRatio: group}}, TieredBillingSnapshot: snapshot, BillingRequestInput: request}
			info.SetEstimatePromptTokens(tc.estimate)
			ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
			ctx.Request = httptest.NewRequest("POST", "/v1/chat/completions", nil)
			if tc.expression == imageExpression {
				ctx.Request.URL.Path = "/v1/images/generations"
				info.RelayMode = relayconstant.RelayModeImagesGenerations
			}
			apiErr := PreConsumeBilling(ctx, reservation, info)
			if tc.insufficient {
				require.NotNil(t, apiErr)
				assert.Equal(t, types.ErrorCodeInsufficientUserQuota, apiErr.GetErrorCode())
			} else {
				require.Nil(t, apiErr)
				held, err := model.GetUserQuota(user.Id, true)
				require.NoError(t, err)
				assert.Equal(t, quota-reservation, held)
				if tc.outboundImages > 0 {
					reserveErr := PrepareImageBillingForRequest(ctx, info, tc.outboundImages)
					if tc.reserveInsufficient {
						require.NotNil(t, reserveErr)
						assert.Equal(t, types.ErrorCodeInsufficientUserQuota, reserveErr.GetErrorCode())
						assert.Equal(t, reservation, info.Billing.GetPreConsumedQuota())
					} else {
						require.Nil(t, reserveErr)
						assert.Equal(t, tc.want, info.Billing.GetPreConsumedQuota())
					}
				}
				if tc.refund {
					refunded := make(chan struct{}, 1)
					const callback = "fixed_billing_refund_observed"
					require.NoError(t, db.Callback().Update().After("gorm:commit_or_rollback_transaction").Register(callback, func(tx *gorm.DB) {
						if tx.Statement.Table == "tokens" && tx.Error == nil {
							select {
							case refunded <- struct{}{}:
							default:
							}
						}
					}))
					t.Cleanup(func() { require.NoError(t, db.Callback().Update().Remove(callback)) })
					info.Billing.Refund(ctx)
					info.Billing.Refund(ctx)
					select {
					case <-refunded:
					case <-time.After(5 * time.Second):
						t.Fatal("refund did not finish")
					}
				} else {
					info.UpdateImageCount(int64(tc.actualImages))
					if tc.tool {
						info.ResponsesUsageInfo = &relaycommon.ResponsesUsageInfo{BuiltInTools: map[string]*relaycommon.BuildInToolInfo{"fixed_billing_tool": {CallCount: 1}}}
					}
					if tc.realtime {
						PostWssConsumeQuota(ctx, info, info.OriginModelName, &dto.RealtimeUsage{
							InputTokens: tc.usage.PromptTokens, OutputTokens: tc.usage.CompletionTokens, TotalTokens: tc.usage.TotalTokens,
						}, "")
					} else if tc.audio {
						PostAudioConsumeQuota(ctx, info, tc.usage, "")
					} else {
						PostTextConsumeQuota(ctx, info, tc.usage, nil)
					}
					require.NoError(t, info.Billing.Settle(tc.want), "a repeated settlement must not charge again")
					var log model.Log
					require.NoError(t, logDB.Where("user_id = ?", user.Id).Take(&log).Error)
					assert.Equal(t, tc.want, log.Quota)
					assert.Equal(t, tc.stream, log.IsStream)
					assert.NotContains(t, log.Content, "无法扣费")
					var other map[string]any
					require.NoError(t, common.UnmarshalJsonStr(log.Other, &other))
					assert.Equal(t, string(tc.unit), other["billing_unit"])
					if tc.requestedImages > 0 {
						count := tc.actualImages
						if count == 0 {
							count = tc.requestedImages
							if tc.outboundImages > 0 {
								count = tc.outboundImages
							}
						}
						assert.Equal(t, float64(count), other["image_count"])
						assert.Equal(t, tc.requestedImages, *request.ImageCount, "actual count must not mutate the frozen request")
					}
					if tc.expression == imageExpression {
						billable, ok := other["billing_tokens"].(map[string]any)
						require.True(t, ok)
						if tc.realtime {
							assert.Equal(t, float64(0), other["image_cache_tokens"])
							assert.Equal(t, float64(1000), billable["p"])
							assert.Equal(t, float64(0), billable["cr"])
							assert.Equal(t, float64(0), billable["img"])
						} else {
							if !tc.audio {
								assert.Equal(t, float64(300), other["cache_tokens"])
							}
							assert.Equal(t, float64(200), other["image_cache_tokens"])
							assert.Equal(t, float64(300), billable["p"])
							assert.Equal(t, float64(100), billable["cr"])
							assert.Equal(t, float64(400), billable["img"])
						}
					} else {
						assert.NotContains(t, other, "billing_tokens")
						assert.NotContains(t, other, "image_cache_tokens")
					}
					if tc.unit == billingexpr.BillingUnitRequest {
						assert.Contains(t, other, "fixed_price")
					} else {
						assert.NotContains(t, other, "fixed_price")
					}
				}
			}
			require.NoError(t, db.First(&user, user.Id).Error)
			require.NoError(t, db.First(&token, token.Id).Error)
			assert.Equal(t, quota-tc.want, user.Quota)
			assert.Equal(t, startingQuota-tc.want, token.RemainQuota)
			assert.Equal(t, tc.want, user.UsedQuota)
			assert.Equal(t, tc.want, token.UsedQuota)
			if !tc.refund && !tc.insufficient {
				assert.Equal(t, 1, user.RequestCount)
			}
		})
	}
}

func TestCalculateTextQuotaSummaryUnifiedForClaudeSemantic(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(w)

	usage := &dto.Usage{
		PromptTokens:     1000,
		CompletionTokens: 200,
		PromptTokensDetails: dto.InputTokenDetails{
			CachedTokens:         100,
			CachedCreationTokens: 50,
		},
		ClaudeCacheCreation5mTokens: 10,
		ClaudeCacheCreation1hTokens: 20,
	}

	priceData := hosttypes.PriceData{
		ModelRatio:           1,
		CompletionRatio:      2,
		CacheRatio:           0.1,
		CacheCreationRatio:   1.25,
		CacheCreation5mRatio: 1.25,
		CacheCreation1hRatio: 2,
		GroupRatioInfo: hosttypes.GroupRatioInfo{
			GroupRatio: 1,
		},
	}

	chatRelayInfo := &relaycommon.RelayInfo{
		RelayFormat:             types.RelayFormatOpenAI,
		FinalRequestRelayFormat: types.RelayFormatClaude,
		OriginModelName:         "claude-3-7-sonnet",
		PriceData:               priceData,
		StartTime:               time.Now(),
	}
	messageRelayInfo := &relaycommon.RelayInfo{
		RelayFormat:             types.RelayFormatClaude,
		FinalRequestRelayFormat: types.RelayFormatClaude,
		OriginModelName:         "claude-3-7-sonnet",
		PriceData:               priceData,
		StartTime:               time.Now(),
	}

	chatSummary := calculateTextQuotaSummary(ctx, chatRelayInfo, usage)
	messageSummary := calculateTextQuotaSummary(ctx, messageRelayInfo, usage)

	require.Equal(t, messageSummary.Quota, chatSummary.Quota)
	require.Equal(t, messageSummary.CacheCreationTokens5m, chatSummary.CacheCreationTokens5m)
	require.Equal(t, messageSummary.CacheCreationTokens1h, chatSummary.CacheCreationTokens1h)
	require.True(t, chatSummary.IsClaudeUsageSemantic)
	require.Equal(t, 1488, chatSummary.Quota)
}

func TestCalculateTextQuotaSummaryUsesSplitClaudeCacheCreationRatios(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(w)

	relayInfo := &relaycommon.RelayInfo{
		RelayFormat:             types.RelayFormatOpenAI,
		FinalRequestRelayFormat: types.RelayFormatClaude,
		OriginModelName:         "claude-3-7-sonnet",
		PriceData: hosttypes.PriceData{
			ModelRatio:           1,
			CompletionRatio:      1,
			CacheRatio:           0,
			CacheCreationRatio:   1,
			CacheCreation5mRatio: 2,
			CacheCreation1hRatio: 3,
			GroupRatioInfo: hosttypes.GroupRatioInfo{
				GroupRatio: 1,
			},
		},
		StartTime: time.Now(),
	}

	usage := &dto.Usage{
		PromptTokens:     100,
		CompletionTokens: 0,
		PromptTokensDetails: dto.InputTokenDetails{
			CachedCreationTokens: 10,
		},
		ClaudeCacheCreation5mTokens: 2,
		ClaudeCacheCreation1hTokens: 3,
	}

	summary := calculateTextQuotaSummary(ctx, relayInfo, usage)

	// 100 + remaining(5)*1 + 2*2 + 3*3 = 118
	require.Equal(t, 118, summary.Quota)
}

func TestCalculateTextQuotaSummaryUsesAnthropicUsageSemanticFromUpstreamUsage(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(w)

	relayInfo := &relaycommon.RelayInfo{
		RelayFormat:     types.RelayFormatOpenAI,
		OriginModelName: "claude-3-7-sonnet",
		PriceData: hosttypes.PriceData{
			ModelRatio:           1,
			CompletionRatio:      2,
			CacheRatio:           0.1,
			CacheCreationRatio:   1.25,
			CacheCreation5mRatio: 1.25,
			CacheCreation1hRatio: 2,
			GroupRatioInfo: hosttypes.GroupRatioInfo{
				GroupRatio: 1,
			},
		},
		StartTime: time.Now(),
	}

	usage := &dto.Usage{
		PromptTokens:     1000,
		CompletionTokens: 200,
		UsageSemantic:    "anthropic",
		PromptTokensDetails: dto.InputTokenDetails{
			CachedTokens:         100,
			CachedCreationTokens: 50,
		},
		ClaudeCacheCreation5mTokens: 10,
		ClaudeCacheCreation1hTokens: 20,
	}

	summary := calculateTextQuotaSummary(ctx, relayInfo, usage)

	require.True(t, summary.IsClaudeUsageSemantic)
	require.Equal(t, "anthropic", summary.UsageSemantic)
	require.Equal(t, 1488, summary.Quota)
}

func TestCalculateTextQuotaSummaryUsesClaudeBillingUsageBeforeTopLevelUsage(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(w)

	relayInfo := &relaycommon.RelayInfo{
		RelayFormat:     types.RelayFormatOpenAI,
		OriginModelName: "claude-3-7-sonnet",
		PriceData: hosttypes.PriceData{
			ModelRatio:           1,
			CompletionRatio:      2,
			CacheRatio:           0.1,
			CacheCreationRatio:   1.25,
			CacheCreation5mRatio: 1.25,
			CacheCreation1hRatio: 2,
			GroupRatioInfo:       hosttypes.GroupRatioInfo{GroupRatio: 1},
		},
		StartTime: time.Now(),
	}

	usage := &dto.Usage{
		PromptTokens:     999,
		CompletionTokens: 999,
		TotalTokens:      1998,
		BillingUsage: dto.NewClaudeMessagesBillingUsage(&dto.ClaudeUsage{
			InputTokens:              70,
			CacheReadInputTokens:     30,
			CacheCreationInputTokens: 20,
			OutputTokens:             7,
			CacheCreation: &dto.ClaudeCacheCreationUsage{
				Ephemeral5mInputTokens: 12,
				Ephemeral1hInputTokens: 8,
			},
		}),
	}

	summary := calculateTextQuotaSummary(ctx, relayInfo, effectiveBillingUsage(usage))

	require.True(t, summary.IsClaudeUsageSemantic)
	require.Equal(t, dto.BillingUsageSemanticAnthropic, summary.UsageSemantic)
	require.Equal(t, 70, summary.PromptTokens)
	require.Equal(t, 7, summary.CompletionTokens)
	require.Equal(t, 30, summary.CacheTokens)
	require.Equal(t, 20, summary.CacheCreationTokens)
	require.Equal(t, 12, summary.CacheCreationTokens5m)
	require.Equal(t, 8, summary.CacheCreationTokens1h)
	require.Equal(t, 118, summary.Quota)
}

func TestCalculateTextQuotaSummaryUsesGeminiBillingUsageBeforeTopLevelUsage(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(w)

	relayInfo := &relaycommon.RelayInfo{
		RelayFormat:     types.RelayFormatOpenAI,
		OriginModelName: "gemini-2.5-flash",
		PriceData: hosttypes.PriceData{
			ModelRatio:      1,
			CompletionRatio: 2,
			CacheRatio:      0.1,
			GroupRatioInfo:  hosttypes.GroupRatioInfo{GroupRatio: 1},
		},
		StartTime: time.Now(),
	}

	usage := &dto.Usage{
		PromptTokens:     999,
		CompletionTokens: 999,
		TotalTokens:      1998,
		BillingUsage: dto.NewGeminiChatBillingUsage(&dto.GeminiUsageMetadata{
			PromptTokenCount:        100,
			ToolUsePromptTokenCount: 5,
			CandidatesTokenCount:    20,
			ThoughtsTokenCount:      3,
			TotalTokenCount:         128,
			CachedContentTokenCount: 7,
		}),
	}

	summary := calculateTextQuotaSummary(ctx, relayInfo, effectiveBillingUsage(usage))

	require.False(t, summary.IsClaudeUsageSemantic)
	require.Equal(t, dto.BillingUsageSemanticGemini, summary.UsageSemantic)
	require.Equal(t, 105, summary.PromptTokens)
	require.Equal(t, 23, summary.CompletionTokens)
	require.Equal(t, 7, summary.CacheTokens)
	require.Equal(t, 128, summary.TotalTokens)
	require.Equal(t, 145, summary.Quota)
}

func TestCalculateTextQuotaSummaryUsesOpenAIBillingUsageBeforeTopLevelUsage(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(w)

	relayInfo := &relaycommon.RelayInfo{
		RelayFormat:     types.RelayFormatClaude,
		OriginModelName: "gpt-4o",
		PriceData: hosttypes.PriceData{
			ModelRatio:      1,
			CompletionRatio: 2,
			GroupRatioInfo:  hosttypes.GroupRatioInfo{GroupRatio: 1},
		},
		StartTime: time.Now(),
	}

	usage := &dto.Usage{
		PromptTokens:     999,
		CompletionTokens: 999,
		TotalTokens:      1998,
		BillingUsage: dto.NewOpenAIChatBillingUsage(&dto.Usage{
			PromptTokens:     80,
			CompletionTokens: 9,
			TotalTokens:      89,
		}),
	}

	summary := calculateTextQuotaSummary(ctx, relayInfo, effectiveBillingUsage(usage))

	require.False(t, summary.IsClaudeUsageSemantic)
	require.Equal(t, dto.BillingUsageSemanticOpenAI, summary.UsageSemantic)
	require.Equal(t, 80, summary.PromptTokens)
	require.Equal(t, 9, summary.CompletionTokens)
	require.Equal(t, 89, summary.TotalTokens)
	require.Equal(t, 98, summary.Quota)
}

func TestCalculateTextQuotaSummaryUsesOpenAIResponsesInputTokenDetails(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	relayInfo := &relaycommon.RelayInfo{
		RelayFormat:     types.RelayFormatOpenAI,
		OriginModelName: "gpt-4o",
		PriceData: hosttypes.PriceData{
			ModelRatio:      1,
			CompletionRatio: 2,
			CacheRatio:      0.25,
			GroupRatioInfo:  hosttypes.GroupRatioInfo{GroupRatio: 1},
		},
		StartTime: time.Now(),
	}

	responsesUsage := &dto.Usage{
		InputTokens:  100,
		OutputTokens: 10,
		TotalTokens:  110,
		InputTokensDetails: &dto.InputTokenDetails{
			CachedTokens: 40,
		},
	}
	convertedUsage := &dto.Usage{
		PromptTokens:     100,
		CompletionTokens: 10,
		TotalTokens:      110,
		PromptTokensDetails: dto.InputTokenDetails{
			CachedTokens: 40,
		},
		BillingUsage: dto.NewOpenAIResponsesBillingUsage(responsesUsage),
	}

	effectiveUsage := effectiveBillingUsage(convertedUsage)
	require.Equal(t, 40, effectiveUsage.PromptTokensDetails.CachedTokens)
	require.Zero(t, convertedUsage.BillingUsage.OpenAIUsage.PromptTokensDetails.CachedTokens)

	summary := calculateTextQuotaSummary(ctx, relayInfo, effectiveUsage)
	require.Equal(t, 40, summary.CacheTokens)
	// 60 uncached input + 40*0.25 cached input + 10*2 output = 90.
	require.Equal(t, 90, summary.Quota)
}

func TestUsageFromOpenAIBillingUsageNormalizesCacheDetailsWithoutOverwritingCanonicalValues(t *testing.T) {
	responsesUsage := &dto.Usage{
		InputTokens:          100,
		OutputTokens:         10,
		PromptCacheHitTokens: 55,
		PromptTokensDetails: dto.InputTokenDetails{
			CachedTokens: 8,
			TextTokens:   12,
		},
		InputTokensDetails: &dto.InputTokenDetails{
			CachedTokens:         40,
			CachedCreationTokens: 5,
			CacheWriteTokens:     6,
			TextTokens:           60,
			ImageTokens:          7,
			AudioTokens:          9,
		},
	}

	billingUsage := dto.NewOpenAIResponsesBillingUsage(responsesUsage)
	usage := effectiveBillingUsage(&dto.Usage{BillingUsage: billingUsage})

	require.Equal(t, 8, usage.PromptTokensDetails.CachedTokens)
	require.Equal(t, 5, usage.PromptTokensDetails.CachedCreationTokens)
	require.Equal(t, 6, usage.PromptTokensDetails.CacheWriteTokens)
	require.Equal(t, 12, usage.PromptTokensDetails.TextTokens)
	require.Equal(t, 7, usage.PromptTokensDetails.ImageTokens)
	require.Equal(t, 9, usage.PromptTokensDetails.AudioTokens)
	require.Zero(t, billingUsage.OpenAIUsage.PromptTokensDetails.CachedCreationTokens)
}

func TestUsageFromOpenAIBillingUsageFallsBackToPromptCacheHitTokens(t *testing.T) {
	usage := effectiveBillingUsage(&dto.Usage{
		BillingUsage: dto.NewOpenAIChatBillingUsage(&dto.Usage{
			PromptTokens:         100,
			CompletionTokens:     10,
			PromptCacheHitTokens: 35,
		}),
	})

	require.Equal(t, 35, usage.PromptTokensDetails.CachedTokens)
}

func TestCalculateTextQuotaSummaryNormalizesOpenAIResponsesBillingUsageDetails(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(w)

	relayInfo := &relaycommon.RelayInfo{
		RelayFormat:     types.RelayFormatClaude,
		OriginModelName: "gpt-5.6-sol",
		PriceData: hosttypes.PriceData{
			ModelRatio:         1,
			CompletionRatio:    2,
			CacheRatio:         0.5,
			CacheCreationRatio: 2,
			GroupRatioInfo:     hosttypes.GroupRatioInfo{GroupRatio: 1},
		},
		StartTime: time.Now(),
	}

	responsesDetails := dto.InputTokenDetails{
		CachedTokens:     80,
		CacheWriteTokens: 10,
		TextTokens:       100,
	}
	usage := &dto.Usage{
		PromptTokens:     999,
		CompletionTokens: 999,
		BillingUsage: dto.NewOpenAIResponsesBillingUsage(&dto.Usage{
			InputTokens:        100,
			OutputTokens:       10,
			TotalTokens:        110,
			InputTokensDetails: &responsesDetails,
		}),
	}

	effectiveUsage := effectiveBillingUsage(usage)
	summary := calculateTextQuotaSummary(ctx, relayInfo, effectiveUsage)

	require.Equal(t, dto.BillingUsageSourceOAIResponses, effectiveUsage.UsageSource)
	require.Equal(t, responsesDetails, effectiveUsage.PromptTokensDetails)
	require.Equal(t, 100, summary.PromptTokens)
	require.Equal(t, 10, summary.CompletionTokens)
	require.Equal(t, 80, summary.CacheTokens)
	require.Equal(t, 10, summary.CacheCreationTokens)
	// (100-80-10) + 80*0.5 + 10*2 + 10*2 = 90
	require.Equal(t, 90, summary.Quota)
}

func TestUsageBillingPathForLog(t *testing.T) {
	require.Equal(t, usageBillingPathAnthropic, usageBillingPathForLog(true, &dto.Usage{
		BillingUsage: dto.NewClaudeMessagesBillingUsage(&dto.ClaudeUsage{InputTokens: 1}),
	}))
	invalidBillingUsage := &dto.Usage{
		PromptTokens: 1,
		BillingUsage: &dto.BillingUsage{
			Source:   dto.BillingUsageSourceClaudeMessages,
			Semantic: dto.BillingUsageSemanticAnthropic,
		},
	}
	require.Equal(t, usageBillingPathLocal, usageBillingPathForLog(true, invalidBillingUsage))
	require.Equal(t, usageBillingPathUpstream, usageBillingPathForLog(false, invalidBillingUsage))
	require.Equal(t, usageBillingPathUpstream, usageBillingPathForLog(false, &dto.Usage{}))
	require.Equal(t, usageBillingPathOpenAI, usageBillingPathForLog(false, &dto.Usage{
		BillingUsage: dto.NewOpenAIChatBillingUsage(&dto.Usage{PromptTokens: 1}),
	}))
	require.Equal(t, usageBillingPathAnthropic, usageBillingPathForLog(false, &dto.Usage{
		BillingUsage: dto.NewClaudeMessagesBillingUsage(&dto.ClaudeUsage{InputTokens: 1}),
	}))
	require.Equal(t, usageBillingPathGemini, usageBillingPathForLog(false, &dto.Usage{
		BillingUsage: dto.NewGeminiChatBillingUsage(&dto.GeminiUsageMetadata{PromptTokenCount: 1}),
	}))
	require.Equal(t, usageBillingPathGeminiEstimated, usageBillingPathForLog(true, &dto.Usage{
		BillingUsage: dto.NewEstimatedGeminiChatBillingUsage(&dto.Usage{PromptTokens: 1}),
	}))
}

func TestAppendUsageBillingPathForLogWritesAdminInfo(t *testing.T) {
	other := model.NewLogOther()
	appendUsageBillingPathForLog(other, true, &dto.Usage{
		BillingUsage: dto.NewClaudeMessagesBillingUsage(&dto.ClaudeUsage{InputTokens: 1}),
	})

	adminInfo, ok := other.Snapshot()["admin_info"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, usageBillingPathAnthropic, adminInfo["usage_billing_path"])

	other = model.NewLogOther()
	appendUsageBillingPathForLog(other, true, nil)
	adminInfo, ok = other.Snapshot()["admin_info"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, usageBillingPathLocal, adminInfo["usage_billing_path"])
}

func TestCacheWriteTokensTotal(t *testing.T) {
	t.Run("split cache creation", func(t *testing.T) {
		summary := textQuotaSummary{
			CacheCreationTokens:   50,
			CacheCreationTokens5m: 10,
			CacheCreationTokens1h: 20,
		}
		require.Equal(t, 50, cacheWriteTokensTotal(summary))
	})

	t.Run("legacy cache creation", func(t *testing.T) {
		summary := textQuotaSummary{CacheCreationTokens: 50}
		require.Equal(t, 50, cacheWriteTokensTotal(summary))
	})

	t.Run("split cache creation without aggregate remainder", func(t *testing.T) {
		summary := textQuotaSummary{
			CacheCreationTokens5m: 10,
			CacheCreationTokens1h: 20,
		}
		require.Equal(t, 30, cacheWriteTokensTotal(summary))
	})
}

func TestCalculateTextQuotaSummaryHandlesLegacyClaudeDerivedOpenAIUsage(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(w)

	relayInfo := &relaycommon.RelayInfo{
		RelayFormat:     types.RelayFormatOpenAI,
		OriginModelName: "claude-3-7-sonnet",
		PriceData: hosttypes.PriceData{
			ModelRatio:           1,
			CompletionRatio:      5,
			CacheRatio:           0.1,
			CacheCreationRatio:   1.25,
			CacheCreation5mRatio: 1.25,
			CacheCreation1hRatio: 2,
			GroupRatioInfo:       hosttypes.GroupRatioInfo{GroupRatio: 1},
		},
		StartTime: time.Now(),
	}

	usage := &dto.Usage{
		PromptTokens:     62,
		CompletionTokens: 95,
		PromptTokensDetails: dto.InputTokenDetails{
			CachedTokens: 3544,
		},
		ClaudeCacheCreation5mTokens: 586,
	}

	summary := calculateTextQuotaSummary(ctx, relayInfo, usage)

	// 62 + 3544*0.1 + 586*1.25 + 95*5 = 1624.9 => 1624
	require.Equal(t, 1624, summary.Quota)
}

func TestCalculateTextQuotaSummaryBillsOpenAICacheWriteTokens(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(w)

	relayInfo := &relaycommon.RelayInfo{
		RelayFormat:     types.RelayFormatOpenAI,
		OriginModelName: "gpt-5.1",
		PriceData: hosttypes.PriceData{
			ModelRatio:         1,
			CompletionRatio:    2,
			CacheRatio:         0.1,
			CacheCreationRatio: 1.25,
			GroupRatioInfo:     hosttypes.GroupRatioInfo{GroupRatio: 1},
		},
		StartTime: time.Now(),
	}

	t.Run("uncached remainder stays positive", func(t *testing.T) {
		usage := &dto.Usage{
			PromptTokens:     1473,
			CompletionTokens: 19,
			PromptTokensDetails: dto.InputTokenDetails{
				CacheWriteTokens: 1470,
			},
		}

		summary := calculateTextQuotaSummary(ctx, relayInfo, usage)

		require.Equal(t, 1470, summary.CacheCreationTokens)
		// (1473-0-1470) + 1470*1.25 + 19*2 = 3 + 1837.5 + 38 = 1878.5 => 1879
		require.Equal(t, 1879, summary.Quota)
	})

	t.Run("uncached remainder clamps to zero", func(t *testing.T) {
		// Real OpenAI payload shape: cached_tokens + cache_write_tokens exceeds
		// prompt_tokens because both are unadjusted prefix counts. The negative
		// remainder must clamp to zero, never turn into a negative base charge.
		usage := &dto.Usage{
			PromptTokens:     3619,
			CompletionTokens: 36,
			PromptTokensDetails: dto.InputTokenDetails{
				CachedTokens:     2921,
				CacheWriteTokens: 3616,
			},
		}

		summary := calculateTextQuotaSummary(ctx, relayInfo, usage)

		require.Equal(t, 3619, summary.PromptTokens)
		require.Equal(t, 3616, summary.CacheCreationTokens)
		// max(3619-2921-3616, 0) + 2921*0.1 + 3616*1.25 + 36*2 = 4884.1 => 4884
		require.Equal(t, 4884, summary.Quota)
	})
}

func TestCalculateTextQuotaSummarySeparatesOpenRouterCacheReadFromPromptBilling(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(w)

	relayInfo := &relaycommon.RelayInfo{
		OriginModelName: "openai/gpt-4.1",
		ChannelMeta: &relaycommon.ChannelMeta{
			ChannelType: constant.ChannelTypeOpenRouter,
		},
		PriceData: hosttypes.PriceData{
			ModelRatio:         1,
			CompletionRatio:    1,
			CacheRatio:         0.1,
			CacheCreationRatio: 1.25,
			GroupRatioInfo:     hosttypes.GroupRatioInfo{GroupRatio: 1},
		},
		StartTime: time.Now(),
	}

	usage := &dto.Usage{
		PromptTokens:     2604,
		CompletionTokens: 383,
		PromptTokensDetails: dto.InputTokenDetails{
			CachedTokens: 2432,
		},
	}

	summary := calculateTextQuotaSummary(ctx, relayInfo, usage)

	// OpenRouter OpenAI-format display keeps prompt_tokens as total input,
	// but billing still separates normal input from cache read tokens.
	// quota = (2604 - 2432) + 2432*0.1 + 383 = 798.2 => 798
	require.Equal(t, 2604, summary.PromptTokens)
	require.Equal(t, 798, summary.Quota)
}

func TestCalculateTextQuotaSummarySeparatesOpenRouterCacheCreationFromPromptBilling(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(w)

	relayInfo := &relaycommon.RelayInfo{
		OriginModelName: "openai/gpt-4.1",
		ChannelMeta: &relaycommon.ChannelMeta{
			ChannelType: constant.ChannelTypeOpenRouter,
		},
		PriceData: hosttypes.PriceData{
			ModelRatio:         1,
			CompletionRatio:    1,
			CacheCreationRatio: 1.25,
			GroupRatioInfo:     hosttypes.GroupRatioInfo{GroupRatio: 1},
		},
		StartTime: time.Now(),
	}

	usage := &dto.Usage{
		PromptTokens:     2604,
		CompletionTokens: 383,
		PromptTokensDetails: dto.InputTokenDetails{
			CachedCreationTokens: 100,
		},
	}

	summary := calculateTextQuotaSummary(ctx, relayInfo, usage)

	// prompt_tokens is still logged as total input, but cache creation is billed separately.
	// quota = (2604 - 100) + 100*1.25 + 383 = 3012
	require.Equal(t, 2604, summary.PromptTokens)
	require.Equal(t, 3012, summary.Quota)
}

func TestCalculateTextQuotaSummaryKeepsPrePRClaudeOpenRouterBilling(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(w)

	relayInfo := &relaycommon.RelayInfo{
		FinalRequestRelayFormat: types.RelayFormatClaude,
		OriginModelName:         "anthropic/claude-3.7-sonnet",
		ChannelMeta: &relaycommon.ChannelMeta{
			ChannelType: constant.ChannelTypeOpenRouter,
		},
		PriceData: hosttypes.PriceData{
			ModelRatio:         1,
			CompletionRatio:    1,
			CacheRatio:         0.1,
			CacheCreationRatio: 1.25,
			GroupRatioInfo:     hosttypes.GroupRatioInfo{GroupRatio: 1},
		},
		StartTime: time.Now(),
	}

	usage := &dto.Usage{
		PromptTokens:     2604,
		CompletionTokens: 383,
		PromptTokensDetails: dto.InputTokenDetails{
			CachedTokens: 2432,
		},
	}

	summary := calculateTextQuotaSummary(ctx, relayInfo, usage)

	// Pre-PR PostClaudeConsumeQuota behavior for OpenRouter:
	// prompt = 2604 - 2432 = 172
	// quota = 172 + 2432*0.1 + 383 = 798.2 => 798
	require.True(t, summary.IsClaudeUsageSemantic)
	require.Equal(t, 172, summary.PromptTokens)
	require.Equal(t, 798, summary.Quota)
}

func TestComposeTieredTextQuotaKeepsToolCallSurcharges(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(w)

	// 11 $/1K => 0.011 per completed image output, matching the prior fixed low-tier charge.
	operation_setting.SetToolPriceForTest(dto.BuildInToolImageGeneration, 11.0)
	t.Cleanup(func() {
		operation_setting.DeleteToolPriceForTest(dto.BuildInToolImageGeneration)
	})

	relayInfo := &relaycommon.RelayInfo{
		OriginModelName: "o1",
		PriceData: hosttypes.PriceData{
			ModelRatio:      1,
			CompletionRatio: 1,
			GroupRatioInfo:  hosttypes.GroupRatioInfo{GroupRatio: 1},
		},
		ResponsesUsageInfo: &relaycommon.ResponsesUsageInfo{
			BuiltInTools: map[string]*relaycommon.BuildInToolInfo{
				dto.BuildInToolWebSearchPreview: {
					CallCount: 1,
				},
				dto.BuildInToolFileSearch: {
					CallCount: 2,
				},
				dto.BuildInToolImageGeneration: {
					CallCount: 1,
				},
			},
		},
		TieredBillingSnapshot: &billingexpr.BillingSnapshot{
			BillingMode:               "tiered_expr",
			GroupRatio:                1,
			EstimatedQuotaBeforeGroup: 1000,
		},
		StartTime: time.Now(),
	}

	usage := &dto.Usage{
		PromptTokens:     100,
		CompletionTokens: 50,
		TotalTokens:      150,
	}

	summary := calculateTextQuotaSummary(ctx, relayInfo, usage)
	quota := composeTieredTextQuota(relayInfo, summary, 1000, &billingexpr.TieredResult{
		ActualQuotaBeforeGroup: 1000,
		ActualQuotaAfterGroup:  1000,
	})

	require.Equal(t, int64(13000), summary.ToolCallSurchargeQuota.Round(0).IntPart())
	require.Equal(t, 14000, quota)
}

func TestComposeTieredTextQuotaFallbackKeepsToolCallSurcharges(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(w)
	ctx.Set("claude_web_search_requests", 2)

	relayInfo := &relaycommon.RelayInfo{
		OriginModelName: "claude-3-7-sonnet",
		PriceData: hosttypes.PriceData{
			ModelRatio:      1,
			CompletionRatio: 1,
			GroupRatioInfo:  hosttypes.GroupRatioInfo{GroupRatio: 1.25},
		},
		TieredBillingSnapshot: &billingexpr.BillingSnapshot{
			BillingMode:               "tiered_expr",
			GroupRatio:                1.25,
			EstimatedQuotaBeforeGroup: 1000,
		},
		StartTime: time.Now(),
	}

	usage := &dto.Usage{
		PromptTokens:     100,
		CompletionTokens: 50,
		TotalTokens:      150,
	}

	summary := calculateTextQuotaSummary(ctx, relayInfo, usage)
	quota := composeTieredTextQuota(relayInfo, summary, 1250, nil)

	require.Equal(t, int64(12500), summary.ToolCallSurchargeQuota.Round(0).IntPart())
	require.Equal(t, 13750, quota)
}

func TestComposeTieredTextQuotaErrorFallbackUsesPreConsumedQuota(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(w)
	ctx.Set("claude_web_search_requests", 2)

	relayInfo := &relaycommon.RelayInfo{
		OriginModelName: "claude-3-7-sonnet",
		PriceData: hosttypes.PriceData{
			ModelRatio:      1,
			CompletionRatio: 1,
			GroupRatioInfo:  hosttypes.GroupRatioInfo{GroupRatio: 1.25},
		},
		TieredBillingSnapshot: &billingexpr.BillingSnapshot{
			BillingMode:               "tiered_expr",
			GroupRatio:                1.25,
			EstimatedQuotaBeforeGroup: 1000,
		},
		StartTime: time.Now(),
	}

	usage := &dto.Usage{
		PromptTokens:     100,
		CompletionTokens: 50,
		TotalTokens:      150,
	}

	summary := calculateTextQuotaSummary(ctx, relayInfo, usage)

	// tieredResult=nil simulates a settlement error where TryTieredSettle
	// falls back to FinalPreConsumedQuota (2000), which differs from
	// EstimatedQuotaBeforeGroup * GroupRatio (1250).
	preConsumedFallback := 2000
	quota := composeTieredTextQuota(relayInfo, summary, preConsumedFallback, nil)

	require.Equal(t, int64(12500), summary.ToolCallSurchargeQuota.Round(0).IntPart())
	require.Equal(t, 14500, quota)
}

// TestTryTieredSettleRecordsClampOnOverflow guards that an oversized tiered
// settlement both saturates the quota and records the clamp on RelayInfo, so
// every consume path (text, audio, WSS) can surface it under admin_info.
func TestTryTieredSettleRecordsClampOnOverflow(t *testing.T) {
	// exprOutput = p * 1e12; quotaBeforeGroup = p*1e12 / 1e6 * 5e5 far exceeds
	// the supported single-request range and must saturate.
	exprStr := `tier("base", p * 1000000000000)`
	relayInfo := &relaycommon.RelayInfo{
		OriginModelName: "overflow-model",
		TieredBillingSnapshot: &billingexpr.BillingSnapshot{
			BillingMode:  "tiered_expr",
			ExprString:   exprStr,
			ExprHash:     billingexpr.ExprHashString(exprStr),
			GroupRatio:   1,
			QuotaPerUnit: 500_000,
		},
	}

	ok, quota, result := TryTieredSettle(relayInfo, billingexpr.TokenParams{P: 1_000_000_000})

	require.True(t, ok)
	require.NotNil(t, result)
	require.Equal(t, common.MaxQuota, quota, "oversized settlement must clamp, never wrap negative")
	require.NotNil(t, relayInfo.QuotaClamp, "clamp must be recorded on RelayInfo for admin auditing")
	require.Equal(t, common.QuotaClampOverflow, relayInfo.QuotaClamp.Kind)
}

// TestTryTieredSettleNoClampInRange confirms an in-range settlement leaves
// RelayInfo.QuotaClamp nil.
func TestTryTieredSettleNoClampInRange(t *testing.T) {
	exprStr := `tier("base", p * 2 + c * 10)`
	relayInfo := &relaycommon.RelayInfo{
		OriginModelName: "in-range-model",
		TieredBillingSnapshot: &billingexpr.BillingSnapshot{
			BillingMode:  "tiered_expr",
			ExprString:   exprStr,
			ExprHash:     billingexpr.ExprHashString(exprStr),
			GroupRatio:   1,
			QuotaPerUnit: 500_000,
		},
	}

	ok, _, result := TryTieredSettle(relayInfo, billingexpr.TokenParams{P: 1000, C: 500})

	require.True(t, ok)
	require.NotNil(t, result)
	require.Nil(t, relayInfo.QuotaClamp, "in-range settlement must not record a clamp")
}

func TestCalculateTextQuotaSummaryFixedPriceAppliesImageCountOnceAndAllowsOverride(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	priceData := hosttypes.PriceData{
		ModelPrice: 0.12,
		UsePrice:   true,
		GroupRatioInfo: hosttypes.GroupRatioInfo{
			GroupRatio: 1,
		},
	}
	priceData.AddOtherRatio("n", 3)
	relayInfo := &relaycommon.RelayInfo{
		OriginModelName: "dall-e-3",
		PriceData:       priceData,
		StartTime:       time.Now(),
	}
	usage := &dto.Usage{PromptTokens: 1, TotalTokens: 1}

	summary := calculateTextQuotaSummary(ctx, relayInfo, usage)
	require.Equal(t, 180000, summary.Quota)

	// An adaptor-reported actual count replaces the requested count rather
	// than multiplying it a second time.
	relayInfo.PriceData.AddOtherRatio("n", 2)
	summary = calculateTextQuotaSummary(ctx, relayInfo, usage)
	require.Equal(t, 120000, summary.Quota)
}

func TestCalculateTextToolCallSurchargeGeneralizedBuiltInTools(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())

	operation_setting.SetToolPriceForTest("my_fn", 5.0)
	t.Cleanup(func() {
		operation_setting.DeleteToolPriceForTest("my_fn")
	})

	relayInfo := &relaycommon.RelayInfo{
		OriginModelName: "o1",
		ResponsesUsageInfo: &relaycommon.ResponsesUsageInfo{
			BuiltInTools: map[string]*relaycommon.BuildInToolInfo{
				dto.BuildInToolWebSearchPreview: {CallCount: 2},
				"my_fn":                         {CallCount: 3},
				"unpriced":                      {CallCount: 5},
			},
		},
	}
	summary := &textQuotaSummary{
		ModelName:  "o1",
		GroupRatio: 1,
	}

	surcharge := calculateTextToolCallSurcharge(ctx, relayInfo, summary)
	expected := decimal.NewFromFloat((10.0*2 + 5.0*3) / 1000).Mul(decimal.NewFromFloat(common.QuotaPerUnit))
	assert.True(t, expected.Equal(surcharge), "got %s want %s", surcharge, expected)
	require.Len(t, summary.ToolSurchargeItems, 2)
	assert.Equal(t, "my_fn", summary.ToolSurchargeItems[0].Name)
	assert.Equal(t, 3, summary.ToolSurchargeItems[0].Count)
	assert.Equal(t, 5.0, summary.ToolSurchargeItems[0].Price)
	assert.Equal(t, dto.BuildInToolWebSearchPreview, summary.ToolSurchargeItems[1].Name)
	assert.Equal(t, 2, summary.ToolSurchargeItems[1].Count)
	assert.Equal(t, 10.0, summary.ToolSurchargeItems[1].Price)
}

func TestCalculateTextToolCallSurchargeKeepsSearchPreviewFallbackWithCustomFunctions(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())

	operation_setting.SetToolPriceForTest("my_fn", 5)
	t.Cleanup(func() {
		operation_setting.DeleteToolPriceForTest("my_fn")
	})

	relayInfo := &relaycommon.RelayInfo{
		RelayMode:       relayconstant.RelayModeChatCompletions,
		OriginModelName: "gpt-4o-search-preview",
		ResponsesUsageInfo: &relaycommon.ResponsesUsageInfo{
			BuiltInTools: map[string]*relaycommon.BuildInToolInfo{
				"my_fn": {CallCount: 1},
			},
		},
	}
	summary := &textQuotaSummary{
		ModelName:  relayInfo.OriginModelName,
		GroupRatio: 1,
	}

	surcharge := calculateTextToolCallSurcharge(ctx, relayInfo, summary)

	require.Len(t, summary.ToolSurchargeItems, 2)
	assert.Equal(t, "my_fn", summary.ToolSurchargeItems[0].Name)
	assert.Equal(t, dto.BuildInToolWebSearchPreview, summary.ToolSurchargeItems[1].Name)
	expected := decimal.NewFromFloat((5.0 + 25.0) / 1000).
		Mul(decimal.NewFromFloat(common.QuotaPerUnit))
	assert.True(t, expected.Equal(surcharge), "got %s want %s", surcharge, expected)
}

func TestCreditTextQuotaUsesCapturedConversion(t *testing.T) {
	previousUnit := common.QuotaPerUnit
	common.QuotaPerUnit = 5000000
	t.Cleanup(func() { common.QuotaPerUnit = previousUnit })
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	info := &relaycommon.RelayInfo{OriginModelName: "credit-conversion", StartTime: time.Now(), PriceData: hosttypes.PriceData{UsePrice: true, ModelPrice: 0.01, GroupRatioInfo: hosttypes.GroupRatioInfo{GroupRatio: 1}}, BillingSource: BillingSourceCreditPacks, Billing: &BillingSession{credit: &creditBilling{quotaPerUnit: 500000}}}
	summary := calculateTextQuotaSummary(ctx, info, &dto.Usage{PromptTokens: 12, CompletionTokens: 8, TotalTokens: 20})
	assert.Equal(t, 5000, summary.Quota, "the request's conversion remains 500000 when runtime configuration changes to 5000000")
}

func TestCalculateTextToolCallSurchargeDoesNotInferSearchForResponses(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	relayInfo := &relaycommon.RelayInfo{
		RelayMode:       relayconstant.RelayModeResponses,
		OriginModelName: "gpt-4o-search-preview",
		ResponsesUsageInfo: &relaycommon.ResponsesUsageInfo{
			BuiltInTools: map[string]*relaycommon.BuildInToolInfo{},
		},
	}
	summary := &textQuotaSummary{
		ModelName:  relayInfo.OriginModelName,
		GroupRatio: 1,
	}

	surcharge := calculateTextToolCallSurcharge(ctx, relayInfo, summary)

	assert.True(t, surcharge.IsZero())
	assert.Empty(t, summary.ToolSurchargeItems)
}

func TestCalculateTextToolCallSurchargeMergesSameNameAndPrice(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Set("claude_web_search_requests", 3)

	relayInfo := &relaycommon.RelayInfo{
		OriginModelName: "claude-3-7-sonnet",
		ResponsesUsageInfo: &relaycommon.ResponsesUsageInfo{
			BuiltInTools: map[string]*relaycommon.BuildInToolInfo{
				dto.BuildInToolWebSearch: {CallCount: 2},
			},
		},
	}
	summary := &textQuotaSummary{ModelName: relayInfo.OriginModelName, GroupRatio: 1}

	surcharge := calculateTextToolCallSurcharge(ctx, relayInfo, summary)

	require.Len(t, summary.ToolSurchargeItems, 1)
	assert.Equal(t, dto.BuildInToolWebSearch, summary.ToolSurchargeItems[0].Name)
	assert.Equal(t, 5, summary.ToolSurchargeItems[0].Count)
	assert.Equal(t, 10.0, summary.ToolSurchargeItems[0].Price)
	expected := decimal.NewFromFloat(10.0 * 5 / 1000).Mul(decimal.NewFromFloat(common.QuotaPerUnit))
	assert.True(t, expected.Equal(surcharge), "got %s want %s", surcharge, expected)
}

func TestMergeToolSurchargeItemsSaturatesCountOverflow(t *testing.T) {
	items := []ToolSurchargeItem{
		{Name: "custom_fn", Count: math.MaxInt, Price: 5},
		{Name: "custom_fn", Count: 1, Price: 5},
	}

	merged := mergeToolSurchargeItems(items)

	require.Len(t, merged, 1)
	assert.Equal(t, math.MaxInt, merged[0].Count)
}

// A zero-token request (e.g. /v1/alpha/search returns no usage) must still
// bill a tool-call surcharge. Regression for the TotalTokens==0 gate zeroing
// out the surcharge quota.
func TestCalculateTextQuotaSummaryZeroTokensStillBillsToolSurcharge(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())

	relayInfo := &relaycommon.RelayInfo{
		OriginModelName: "o1",
		ResponsesUsageInfo: &relaycommon.ResponsesUsageInfo{
			BuiltInTools: map[string]*relaycommon.BuildInToolInfo{
				dto.BuildInToolWebSearchPreview: {CallCount: 1},
			},
		},
	}
	relayInfo.PriceData.GroupRatioInfo.GroupRatio = 1

	usage := &dto.Usage{} // zero tokens, mirrors alpha search
	summary := calculateTextQuotaSummary(ctx, relayInfo, usage)

	require.Equal(t, 0, summary.TotalTokens)
	assert.False(t, summary.ToolCallSurchargeQuota.IsZero(), "surcharge should be computed")
	assert.Greater(t, summary.Quota, 0, "quota must not be zeroed out for a zero-token web search request")
	expected := common.QuotaFromDecimal(summary.ToolCallSurchargeQuota)
	assert.Equal(t, expected, summary.Quota)
}

func TestCalculateTextQuotaSummaryDoesNotApplyRequestMultipliersToToolSurcharge(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())

	relayInfo := &relaycommon.RelayInfo{
		OriginModelName: "o1",
		PriceData: hosttypes.PriceData{
			ModelRatio:      1,
			CompletionRatio: 1,
			GroupRatioInfo:  hosttypes.GroupRatioInfo{GroupRatio: 1},
		},
		ResponsesUsageInfo: &relaycommon.ResponsesUsageInfo{
			BuiltInTools: map[string]*relaycommon.BuildInToolInfo{
				dto.BuildInToolWebSearchPreview: {CallCount: 1},
			},
		},
	}
	relayInfo.PriceData.AddOtherRatio("n", 3)

	summary := calculateTextQuotaSummary(ctx, relayInfo, &dto.Usage{})

	expected := decimal.NewFromFloat(10.0 / 1000).Mul(decimal.NewFromFloat(common.QuotaPerUnit))
	assert.True(t, expected.Equal(summary.ToolCallSurchargeQuota))
	assert.Equal(t, common.QuotaFromDecimal(expected), summary.Quota)
}

func TestCalculateTextToolCallSurchargeGeminiGoogleSearch(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Set("gemini_google_search_call", true)

	relayInfo := &relaycommon.RelayInfo{OriginModelName: "gemini-2.5-flash"}
	summary := &textQuotaSummary{ModelName: "gemini-2.5-flash", GroupRatio: 1}

	surcharge := calculateTextToolCallSurcharge(ctx, relayInfo, summary)
	expected := decimal.NewFromFloat(14.0 / 1000).Mul(decimal.NewFromFloat(common.QuotaPerUnit))
	assert.True(t, expected.Equal(surcharge), "got %s want %s", surcharge, expected)
	require.Len(t, summary.ToolSurchargeItems, 1)
	assert.Equal(t, dto.BuildInToolGoogleSearch, summary.ToolSurchargeItems[0].Name)
	assert.Equal(t, 1, summary.ToolSurchargeItems[0].Count)
	assert.Equal(t, 14.0, summary.ToolSurchargeItems[0].Price)
}

func TestCalculateTextToolCallSurchargeGeminiFunctionCall(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())

	operation_setting.SetToolPriceForTest("gemini_surcharge_fn", 5.0)
	t.Cleanup(func() {
		operation_setting.DeleteToolPriceForTest("gemini_surcharge_fn")
	})

	relayInfo := &relaycommon.RelayInfo{
		OriginModelName: "gemini-2.5-flash",
		ResponsesUsageInfo: &relaycommon.ResponsesUsageInfo{
			BuiltInTools: map[string]*relaycommon.BuildInToolInfo{
				"gemini_surcharge_fn": {CallCount: 2},
			},
		},
	}
	summary := &textQuotaSummary{ModelName: "gemini-2.5-flash", GroupRatio: 1}

	surcharge := calculateTextToolCallSurcharge(ctx, relayInfo, summary)
	expected := decimal.NewFromFloat(5.0 * 2 / 1000).Mul(decimal.NewFromFloat(common.QuotaPerUnit))
	assert.True(t, expected.Equal(surcharge), "got %s want %s", surcharge, expected)
	require.Len(t, summary.ToolSurchargeItems, 1)
	assert.Equal(t, "gemini_surcharge_fn", summary.ToolSurchargeItems[0].Name)
	assert.Equal(t, 2, summary.ToolSurchargeItems[0].Count)
	assert.Equal(t, 5.0, summary.ToolSurchargeItems[0].Price)

	other := model.NewLogOther()
	appendToolSurchargeLogInfo(other, summary.ToolSurchargeItems)
	assert.Equal(t, summary.ToolSurchargeItems, other.Snapshot()["tool_surcharges"])
}

func TestCalculateTextToolCallSurchargeImageGenerationDefaultPrice(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	t.Cleanup(func() {
		operation_setting.DeleteToolPriceForTest(dto.BuildInToolImageGeneration)
	})

	relayInfo := &relaycommon.RelayInfo{
		OriginModelName: "gpt-5.1",
		ResponsesUsageInfo: &relaycommon.ResponsesUsageInfo{
			BuiltInTools: map[string]*relaycommon.BuildInToolInfo{
				dto.BuildInToolImageGeneration: {CallCount: 2},
			},
		},
	}
	summary := &textQuotaSummary{ModelName: "gpt-5.1", GroupRatio: 1.5}

	surcharge := calculateTextToolCallSurcharge(ctx, relayInfo, summary)
	expected := decimal.NewFromFloat(150.0).
		Mul(decimal.NewFromInt(2)).
		Div(decimal.NewFromInt(1000)).
		Mul(decimal.NewFromFloat(1.5)).
		Mul(decimal.NewFromFloat(common.QuotaPerUnit))
	assert.True(t, expected.Equal(surcharge), "got %s want %s", surcharge, expected)
	require.Len(t, summary.ToolSurchargeItems, 1)
	assert.Equal(t, dto.BuildInToolImageGeneration, summary.ToolSurchargeItems[0].Name)
	assert.Equal(t, 2, summary.ToolSurchargeItems[0].Count)
	assert.Equal(t, 150.0, summary.ToolSurchargeItems[0].Price)
}

func TestCalculateTextToolCallSurchargeImageGenerationExplicitZeroDisables(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	operation_setting.SetToolPriceForTest(dto.BuildInToolImageGeneration, 0)
	t.Cleanup(func() {
		operation_setting.DeleteToolPriceForTest(dto.BuildInToolImageGeneration)
	})

	relayInfo := &relaycommon.RelayInfo{
		OriginModelName: "gpt-5.1",
		ResponsesUsageInfo: &relaycommon.ResponsesUsageInfo{
			BuiltInTools: map[string]*relaycommon.BuildInToolInfo{
				dto.BuildInToolImageGeneration: {CallCount: 3},
			},
		},
	}
	summary := &textQuotaSummary{ModelName: "gpt-5.1", GroupRatio: 1}

	surcharge := calculateTextToolCallSurcharge(ctx, relayInfo, summary)
	assert.True(t, surcharge.IsZero())
	assert.Empty(t, summary.ToolSurchargeItems)
}

func TestCalculateTextQuotaSummaryImageGenerationUsesStructuredSurcharge(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	t.Cleanup(func() {
		operation_setting.DeleteToolPriceForTest(dto.BuildInToolImageGeneration)
	})

	relayInfo := &relaycommon.RelayInfo{
		OriginModelName: "gpt-5.1",
		ResponsesUsageInfo: &relaycommon.ResponsesUsageInfo{
			BuiltInTools: map[string]*relaycommon.BuildInToolInfo{
				dto.BuildInToolImageGeneration: {CallCount: 1},
			},
		},
	}
	relayInfo.PriceData.GroupRatioInfo.GroupRatio = 1
	relayInfo.PriceData.ModelRatio = 1
	relayInfo.PriceData.CompletionRatio = 1

	usage := &dto.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15}
	summary := calculateTextQuotaSummary(ctx, relayInfo, usage)

	require.Len(t, summary.ToolSurchargeItems, 1)
	assert.Equal(t, dto.BuildInToolImageGeneration, summary.ToolSurchargeItems[0].Name)
	assert.Equal(t, 1, summary.ToolSurchargeItems[0].Count)
	assert.Equal(t, 150.0, summary.ToolSurchargeItems[0].Price)

	expectedSurcharge := decimal.NewFromFloat(150.0 / 1000).Mul(decimal.NewFromFloat(common.QuotaPerUnit))
	assert.True(t, expectedSurcharge.Equal(summary.ToolCallSurchargeQuota),
		"got %s want %s", summary.ToolCallSurchargeQuota, expectedSurcharge)
	assert.Greater(t, summary.Quota, 0)
}

func TestAppendToolSurchargeLogInfoWritesOnlyStructuredFields(t *testing.T) {
	items := []ToolSurchargeItem{
		{Name: dto.BuildInToolWebSearch, Count: 2, Price: 10},
		{Name: dto.BuildInToolImageGeneration, Count: 1, Price: 150},
	}
	other := model.NewLogOther()

	appendToolSurchargeLogInfo(other, items)

	fields := other.Snapshot()
	assert.Equal(t, items, fields["tool_surcharges"])
	assert.NotContains(t, fields, "web_search")
	assert.NotContains(t, fields, "web_search_call_count")
	assert.NotContains(t, fields, "web_search_price")
	assert.NotContains(t, fields, "file_search")
	assert.NotContains(t, fields, "image_generation_call")
	assert.NotContains(t, fields, "image_generation_call_price")
}

// Historical input and current output counts may deliberately use different
// models/configurations. Their receipts must describe the actual observations.
func TestCreditTokenEstimatorProvenance(t *testing.T) {
	previousCount := constant.CountToken
	t.Cleanup(func() { constant.CountToken = previousCount })
	constant.CountToken = true
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	common.SetContextKey(c, constant.ContextKeyOriginalModel, "claude-original")
	info := &relaycommon.RelayInfo{RelayFormat: types.RelayFormatOpenAI, BillingSource: BillingSourceCreditPacks, ChannelMeta: &relaycommon.ChannelMeta{}}
	info.UpstreamModelName = "gpt-4o"
	count, err := EstimateRequestToken(c, &types.TokenCountMeta{TokenType: types.TokenTypeTokenizer, CombineText: "private original", MessagesCount: 1}, info)
	require.NoError(t, err)
	require.NotNil(t, info.CreditPromptEstimation)
	original, err := common.Marshal(info.CreditPromptEstimation)
	require.NoError(t, err)
	assert.Equal(t, "claude-original", info.CreditPromptEstimation.Model)
	assert.Equal(t, "provider-heuristic", info.CreditPromptEstimation.Method)
	assert.Equal(t, 1.13, info.CreditPromptEstimation.Parameters["word"])
	assert.Equal(t, float64(count), info.CreditPromptEstimation.Quantity)
	constant.CountToken = false
	EstimateCreditUsageField(info, "prompt_tokens", count, "new-api-prompt-count-v1")
	require.NotNil(t, info.CreditUsageFacts["prompt_tokens"].Estimation)
	recorded, err := common.Marshal(info.CreditUsageFacts["prompt_tokens"].Estimation)
	require.NoError(t, err)
	assert.Equal(t, string(original), string(recorded), "final settings do not rewrite input provenance")
	count = EstimateCreditTextUsageField(info, "completion_tokens", []string{"private output"}, "gpt-4o", "new-api-output-count-v1", 7)
	fact := info.CreditUsageFacts["completion_tokens"]
	require.NotNil(t, fact.Estimation)
	assert.Equal(t, "tiktoken", fact.Estimation.Method)
	assert.Equal(t, "o200k_base", fact.Estimation.Tokenizer)
	assert.NotEmpty(t, fact.Estimation.DependencyVersion, "unavailable build metadata must be labelled unknown")
	assert.Equal(t, float64(7), fact.Estimation.Parameters["extra_tokens"])
	assert.Equal(t, float64(count), *fact.Quantity)
	assert.Equal(t, float64(count), fact.Estimation.Quantity)
	// Complete provider zeros never acquire a local-estimation descriptor.
	zero := float64(0)
	info.CreditUsageFacts["completion_tokens"] = hosttypes.UsageFact{Field: "completion_tokens", Unit: "token", Quantity: &zero, Source: "upstream"}
	EstimateCreditTextUsageField(info, "completion_tokens", []string{"private output"}, "gpt-4o", "new-api-output-count-v1", 0)
	assert.Equal(t, "upstream", info.CreditUsageFacts["completion_tokens"].Source)
	assert.Nil(t, info.CreditUsageFacts["completion_tokens"].Estimation)
	// Retain a partial provider lower bound and the smaller local counter result.
	floor := float64(100)
	info.CreditUsageFacts["completion_tokens"] = hosttypes.UsageFact{Field: "completion_tokens", Unit: "token", Quantity: &floor, Source: "upstream", Partial: true}
	EstimateCreditTextUsageField(info, "completion_tokens", []string{"hello"}, "claude-output", "new-api-output-count-v1", 0)
	fact = info.CreditUsageFacts["completion_tokens"]
	assert.Equal(t, float64(100), *fact.Quantity)
	require.NotNil(t, fact.Estimation)
	assert.Equal(t, float64(2), fact.Estimation.Quantity)
	encoded, err := common.Marshal(info.CreditUsageFacts)
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), "private original")
	assert.NotContains(t, string(encoded), "private output")
	t.Run("input_media_preserves_individual_counter_parameters", func(t *testing.T) {
		previousMedia, previousNotStream := constant.GetMediaToken, constant.GetMediaTokenNotStream
		constant.CountToken, constant.GetMediaToken, constant.GetMediaTokenNotStream = true, true, true
		t.Cleanup(func() { constant.GetMediaToken, constant.GetMediaTokenNotStream = previousMedia, previousNotStream })
		var media bytes.Buffer
		require.NoError(t, png.Encode(&media, image.NewRGBA(image.Rect(0, 0, 128, 64))))
		payload := base64.StdEncoding.EncodeToString(media.Bytes())
		for _, test := range []struct {
			name, model string
			files       []*types.FileMeta
			want        int
		}{
			{name: "mixed", model: "gpt-4o", want: 17833, files: []*types.FileMeta{
				types.NewImageFileMeta(types.NewBase64FileSource(payload, "image/png"), "low"),
				types.NewImageFileMeta(types.NewBase64FileSource(payload, "image/png"), "high"),
				{FileType: types.FileTypeAudio}, {FileType: types.FileTypeVideo}, {FileType: types.FileTypeFile}, {},
			}},
			{name: "patch", model: "gpt-4.1-mini", want: 16, files: []*types.FileMeta{types.NewImageFileMeta(types.NewBase64FileSource(payload, "image/png"), "high")}},
		} {
			t.Run(test.name, func(t *testing.T) {
				ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
				ctx.Request = httptest.NewRequest("POST", "/v1/chat/completions", nil)
				common.SetContextKey(ctx, constant.ContextKeyOriginalModel, test.model)
				info := &relaycommon.RelayInfo{RelayFormat: types.RelayFormatOpenAI}
				count, err := CountRequestToken(ctx, &types.TokenCountMeta{TokenType: types.TokenTypeTokenizer, Files: test.files}, info)
				require.NoError(t, err)
				assert.Equal(t, test.want, count, "preserve native image rounding and fixed media defaults")
				encoded, err := common.Marshal(info.CreditPromptEstimation)
				require.NoError(t, err)
				var recorded struct {
					Components []struct {
						Index      int                `json:"index"`
						Kind       string             `json:"kind"`
						Method     string             `json:"method"`
						Quantity   float64            `json:"quantity"`
						Parameters map[string]float64 `json:"parameters"`
					} `json:"components"`
				}
				require.NoError(t, common.Unmarshal(encoded, &recorded))
				require.Len(t, recorded.Components, len(test.files))
				if test.name == "mixed" {
					for i, component := range recorded.Components {
						assert.Equal(t, i, component.Index)
						assert.Equal(t, []float64{85, 1105, 256, 8192, 4096, 4096}[i], component.Quantity)
						assert.Equal(t, []string{"image", "image", "audio", "video", "file", "unknown"}[i], component.Kind)
					}
					assert.Equal(t, "image-low-detail", recorded.Components[0].Method)
					assert.Equal(t, "image-tiles", recorded.Components[1].Method)
					assert.Equal(t, float64(128), recorded.Components[1].Parameters["width"])
					assert.Equal(t, float64(64), recorded.Components[1].Parameters["height"])
					assert.Equal(t, float64(6), recorded.Components[1].Parameters["tiles"])
				} else {
					assert.Equal(t, "image-patches", recorded.Components[0].Method)
					assert.Equal(t, 1.62, recorded.Components[0].Parameters["multiplier"])
					assert.Equal(t, float64(8), recorded.Components[0].Parameters["patches"])
					assert.Equal(t, float64(13), recorded.Components[0].Quantity)
				}
				assert.NotContains(t, string(encoded), payload)
			})
		}
		t.Run("bounded_detail_does_not_drop_aggregate_usage", func(t *testing.T) {
			ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
			ctx.Request = httptest.NewRequest("POST", "/v1/chat/completions", nil)
			common.SetContextKey(ctx, constant.ContextKeyOriginalModel, "gpt-4o")
			files := make([]*types.FileMeta, 65)
			for i := range files {
				files[i] = &types.FileMeta{FileType: types.FileTypeAudio}
			}
			info := &relaycommon.RelayInfo{RelayFormat: types.RelayFormatOpenAI}
			count, err := CountRequestToken(ctx, &types.TokenCountMeta{Files: files}, info)
			require.NoError(t, err)
			assert.Equal(t, 65*256+3, count)
			require.Len(t, info.CreditPromptEstimation.Components, 64)
			assert.Equal(t, 1, info.CreditPromptEstimation.OmittedComponents)
			assert.EqualValues(t, 65*256, info.CreditPromptEstimation.Parameters["media_tokens"])
		})
		t.Run("uploaded_audio_keeps_per_file_duration_rounding", func(t *testing.T) {
			wav := make([]byte, 44+3200)
			copy(wav[0:4], "RIFF")
			binary.LittleEndian.PutUint32(wav[4:8], uint32(len(wav)-8))
			copy(wav[8:16], "WAVEfmt ")
			binary.LittleEndian.PutUint32(wav[16:20], 16)
			binary.LittleEndian.PutUint16(wav[20:22], 1)
			binary.LittleEndian.PutUint16(wav[22:24], 1)
			binary.LittleEndian.PutUint32(wav[24:28], 16000)
			binary.LittleEndian.PutUint32(wav[28:32], 32000)
			binary.LittleEndian.PutUint16(wav[32:34], 2)
			binary.LittleEndian.PutUint16(wav[34:36], 16)
			copy(wav[36:40], "data")
			binary.LittleEndian.PutUint32(wav[40:44], 3200)
			var body bytes.Buffer
			form := multipart.NewWriter(&body)
			for i := range 2 {
				file, err := form.CreateFormFile("file", fmt.Sprintf("private-%d.wav", i))
				require.NoError(t, err)
				_, err = file.Write(wav)
				require.NoError(t, err)
			}
			require.NoError(t, form.Close())
			ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
			ctx.Request = httptest.NewRequest("POST", "/v1/audio/transcriptions", &body)
			ctx.Request.Header.Set("Content-Type", form.FormDataContentType())
			common.SetContextKey(ctx, constant.ContextKeyOriginalModel, "whisper-1")
			info := &relaycommon.RelayInfo{RelayFormat: types.RelayFormatOpenAIAudio, RelayMode: relayconstant.RelayModeAudioTranscription}
			count, err := CountRequestToken(ctx, &types.TokenCountMeta{}, info)
			require.NoError(t, err)
			assert.Equal(t, 34, count, "round each file's seconds and tokens using the original rule")
			require.Len(t, info.CreditPromptEstimation.Components, 2)
			for i, component := range info.CreditPromptEstimation.Components {
				assert.Equal(t, i, component.Index)
				assert.Equal(t, "audio", component.Kind)
				assert.Equal(t, "audio-duration", component.Method)
				assert.Equal(t, .1, component.Parameters["duration_seconds"])
				assert.EqualValues(t, 17, component.Quantity)
			}
			encoded, err := common.Marshal(info.CreditPromptEstimation)
			require.NoError(t, err)
			assert.NotContains(t, string(encoded), "private-")
		})
	})
}

package model

import (
	"bytes"
	"errors"
	"fmt"
	"github.com/QuantumNous/new-api/common"
	"github.com/glebarez/sqlite"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/schema"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func seedSubscriptionResetPlan(t *testing.T, plan *SubscriptionPlan) {
	t.Helper()
	require.NoError(t, DB.Create(plan).Error)
}

func seedSubscriptionResetSub(t *testing.T, sub *UserSubscription) {
	t.Helper()
	require.NoError(t, DB.Create(sub).Error)
}

func getSubscriptionResetSub(t *testing.T, id int) UserSubscription {
	t.Helper()
	var sub UserSubscription
	require.NoError(t, DB.Where("id = ?", id).First(&sub).Error)
	return sub
}

func TestAdminResetUserSubscriptionsByPlanResetsAllActiveMatchesAndAdvancesTime(t *testing.T) {
	truncateTables(t)

	now := GetDBTimestamp()
	plan := &SubscriptionPlan{
		Id:               9101,
		Title:            "Pro",
		PriceAmount:      10,
		DurationUnit:     SubscriptionDurationMonth,
		DurationValue:    1,
		TotalAmount:      1000,
		QuotaResetPeriod: SubscriptionResetDaily,
	}
	otherPlan := &SubscriptionPlan{
		Id:               9102,
		Title:            "Basic",
		PriceAmount:      1,
		DurationUnit:     SubscriptionDurationMonth,
		DurationValue:    1,
		TotalAmount:      100,
		QuotaResetPeriod: SubscriptionResetDaily,
	}
	seedSubscriptionResetPlan(t, plan)
	seedSubscriptionResetPlan(t, otherPlan)

	activeEnd := now + 30*24*3600
	expiredEnd := now - 1
	seedSubscriptionResetSub(t, &UserSubscription{Id: 9201, UserId: 101, PlanId: plan.Id, AmountTotal: 1000, AmountUsed: 300, StartTime: now - 3600, EndTime: activeEnd, Status: "active", LastResetTime: now - 3600, NextResetTime: now + 120})
	seedSubscriptionResetSub(t, &UserSubscription{Id: 9202, UserId: 101, PlanId: plan.Id, AmountTotal: 1000, AmountUsed: 500, StartTime: now - 3600, EndTime: activeEnd, Status: "active", LastResetTime: now - 3600, NextResetTime: now + 120})
	seedSubscriptionResetSub(t, &UserSubscription{Id: 9203, UserId: 101, PlanId: otherPlan.Id, AmountTotal: 100, AmountUsed: 60, StartTime: now - 3600, EndTime: activeEnd, Status: "active", LastResetTime: now - 3600, NextResetTime: now + 120})
	seedSubscriptionResetSub(t, &UserSubscription{Id: 9204, UserId: 101, PlanId: plan.Id, AmountTotal: 1000, AmountUsed: 700, StartTime: now - 7200, EndTime: expiredEnd, Status: "active", LastResetTime: now - 3600, NextResetTime: now - 10})
	seedSubscriptionResetSub(t, &UserSubscription{Id: 9205, UserId: 102, PlanId: plan.Id, AmountTotal: 1000, AmountUsed: 800, StartTime: now - 3600, EndTime: activeEnd, Status: "active", LastResetTime: now - 3600, NextResetTime: now + 120})
	seedSubscriptionResetSub(t, &UserSubscription{Id: 9206, UserId: 101, PlanId: plan.Id, AmountTotal: 1000, AmountUsed: 900, StartTime: now - 3600, EndTime: activeEnd, Status: "cancelled", LastResetTime: now - 3600, NextResetTime: now + 120})

	beforeReset := GetDBTimestamp()
	result, err := AdminResetUserSubscriptionsByPlan(101, plan.Id, true)
	afterReset := GetDBTimestamp()

	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Equal(t, plan.Id, result.PlanId)
	assert.Equal(t, 2, result.MatchedCount)
	assert.Equal(t, 2, result.ResetCount)
	assert.Equal(t, 1, result.UserCount)
	assert.Equal(t, []int{101}, result.AffectedUserIds)
	assert.True(t, result.AdvanceResetTime)

	for _, id := range []int{9201, 9202} {
		sub := getSubscriptionResetSub(t, id)
		assert.Zero(t, sub.AmountUsed)
		assert.GreaterOrEqual(t, sub.LastResetTime, beforeReset)
		assert.LessOrEqual(t, sub.LastResetTime, afterReset)
		assert.Equal(t, calcNextResetTime(time.Unix(sub.LastResetTime, 0), plan, sub.EndTime), sub.NextResetTime)
	}
	assert.EqualValues(t, 60, getSubscriptionResetSub(t, 9203).AmountUsed)
	assert.EqualValues(t, 700, getSubscriptionResetSub(t, 9204).AmountUsed)
	assert.EqualValues(t, 800, getSubscriptionResetSub(t, 9205).AmountUsed)
	assert.EqualValues(t, 900, getSubscriptionResetSub(t, 9206).AmountUsed)
}

func TestAdminResetUserSubscriptionsByPlanKeepsResetTimes(t *testing.T) {
	truncateTables(t)

	now := GetDBTimestamp()
	plan := &SubscriptionPlan{
		Id:               9301,
		Title:            "Team",
		PriceAmount:      20,
		DurationUnit:     SubscriptionDurationMonth,
		DurationValue:    1,
		TotalAmount:      2000,
		QuotaResetPeriod: SubscriptionResetMonthly,
	}
	seedSubscriptionResetPlan(t, plan)

	lastReset := now - 86400
	nextReset := now + 86400
	seedSubscriptionResetSub(t, &UserSubscription{Id: 9302, UserId: 201, PlanId: plan.Id, AmountTotal: 2000, AmountUsed: 1200, StartTime: now - 172800, EndTime: now + 30*24*3600, Status: "active", LastResetTime: lastReset, NextResetTime: nextReset})

	result, err := AdminResetUserSubscriptionsByPlan(201, plan.Id, false)

	require.NoError(t, err)
	assert.False(t, result.AdvanceResetTime)
	sub := getSubscriptionResetSub(t, 9302)
	assert.Zero(t, sub.AmountUsed)
	assert.Equal(t, lastReset, sub.LastResetTime)
	assert.Equal(t, nextReset, sub.NextResetTime)
}

func TestAdminResetUserSubscriptionsByPlanNoActiveMatchReturnsError(t *testing.T) {
	truncateTables(t)

	now := GetDBTimestamp()
	plan := &SubscriptionPlan{
		Id:            9401,
		Title:         "Expired",
		PriceAmount:   10,
		DurationUnit:  SubscriptionDurationMonth,
		DurationValue: 1,
		TotalAmount:   1000,
	}
	seedSubscriptionResetPlan(t, plan)
	seedSubscriptionResetSub(t, &UserSubscription{Id: 9402, UserId: 301, PlanId: plan.Id, AmountTotal: 1000, AmountUsed: 500, StartTime: now - 7200, EndTime: now - 1, Status: "active"})

	result, err := AdminResetUserSubscriptionsByPlan(301, plan.Id, true)

	require.Error(t, err)
	assert.Nil(t, result)
	assert.True(t, strings.Contains(err.Error(), "该用户没有有效的此套餐订阅"))
}

func TestAdminResetPlanSubscriptionsResetsAllActiveUsers(t *testing.T) {
	truncateTables(t)

	now := GetDBTimestamp()
	plan := &SubscriptionPlan{
		Id:               9501,
		Title:            "Business",
		PriceAmount:      30,
		DurationUnit:     SubscriptionDurationMonth,
		DurationValue:    1,
		TotalAmount:      3000,
		QuotaResetPeriod: SubscriptionResetNever,
	}
	seedSubscriptionResetPlan(t, plan)

	activeEnd := now + 30*24*3600
	seedSubscriptionResetSub(t, &UserSubscription{Id: 9502, UserId: 401, PlanId: plan.Id, AmountTotal: 3000, AmountUsed: 1000, StartTime: now - 3600, EndTime: activeEnd, Status: "active", LastResetTime: now - 3600, NextResetTime: now + 10})
	seedSubscriptionResetSub(t, &UserSubscription{Id: 9503, UserId: 401, PlanId: plan.Id, AmountTotal: 3000, AmountUsed: 1100, StartTime: now - 3500, EndTime: activeEnd, Status: "active", LastResetTime: now - 3600, NextResetTime: now + 10})
	seedSubscriptionResetSub(t, &UserSubscription{Id: 9504, UserId: 402, PlanId: plan.Id, AmountTotal: 3000, AmountUsed: 1200, StartTime: now - 3400, EndTime: activeEnd, Status: "active", LastResetTime: now - 3600, NextResetTime: now + 10})
	seedSubscriptionResetSub(t, &UserSubscription{Id: 9505, UserId: 403, PlanId: plan.Id, AmountTotal: 3000, AmountUsed: 1300, StartTime: now - 7200, EndTime: now - 1, Status: "active", LastResetTime: now - 3600, NextResetTime: now - 10})
	seedSubscriptionResetSub(t, &UserSubscription{Id: 9506, UserId: 404, PlanId: plan.Id, AmountTotal: 3000, AmountUsed: 1400, StartTime: now - 3600, EndTime: activeEnd, Status: "cancelled", LastResetTime: now - 3600, NextResetTime: now + 10})

	result, err := AdminResetPlanSubscriptions(plan.Id, true)

	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Equal(t, 3, result.MatchedCount)
	assert.Equal(t, 3, result.ResetCount)
	assert.Equal(t, 2, result.UserCount)
	assert.Equal(t, []int{401, 402}, result.AffectedUserIds)
	for _, id := range []int{9502, 9503, 9504} {
		sub := getSubscriptionResetSub(t, id)
		assert.Zero(t, sub.AmountUsed)
		assert.Zero(t, sub.LastResetTime)
		assert.Zero(t, sub.NextResetTime)
	}
	assert.EqualValues(t, 1300, getSubscriptionResetSub(t, 9505).AmountUsed)
	assert.EqualValues(t, 1400, getSubscriptionResetSub(t, 9506).AmountUsed)
}

func TestAdminResetPlanSubscriptionsNoMatchSucceeds(t *testing.T) {
	truncateTables(t)

	plan := &SubscriptionPlan{
		Id:            9601,
		Title:         "Empty",
		PriceAmount:   10,
		DurationUnit:  SubscriptionDurationMonth,
		DurationValue: 1,
		TotalAmount:   1000,
	}
	seedSubscriptionResetPlan(t, plan)

	result, err := AdminResetPlanSubscriptions(plan.Id, true)

	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Zero(t, result.MatchedCount)
	assert.Zero(t, result.ResetCount)
	assert.Zero(t, result.UserCount)
	assert.Empty(t, result.AffectedUserIds)
}

// Version publication is independent of the still-pending purchase, renewal,
// tag, and window policies. It must not alter an existing subscriber's rights.
func TestSubscriptionVersionDatabaseMatrix(t *testing.T) {
	for _, dialect := range []string{"sqlite", "mysql", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			var driver gorm.Dialector
			switch dialect {
			case "sqlite":
				driver = sqlite.Open(filepath.Join(t.TempDir(), "versions.db") + "?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)")
			case "mysql":
				dsn := os.Getenv("TEST_MYSQL_DSN")
				if dsn == "" {
					t.Skip("TEST_MYSQL_DSN not configured")
				}
				driver = mysqlMigrationDialector{mysql.Dialector{Config: &mysql.Config{DSN: dsn}}}
			case "postgres":
				dsn := os.Getenv("TEST_POSTGRES_DSN")
				if dsn == "" {
					t.Skip("TEST_POSTGRES_DSN not configured")
				}
				driver = postgresMigrationDialector{postgres.Dialector{Config: &postgres.Config{DSN: dsn}}}
			}
			db, err := gorm.Open(driver, &gorm.Config{NamingStrategy: schema.NamingStrategy{TablePrefix: "subscription_version_test_"}})
			require.NoError(t, err)
			sqlDB, err := db.DB()
			require.NoError(t, err)
			sqlDB.SetMaxOpenConns(8)
			previousType := common.MainDatabaseType()
			common.SetMainDatabaseType(common.DatabaseType(dialect))
			t.Cleanup(func() {
				require.NoError(t, db.Migrator().DropTable(&CreditLedgerEntry{}, &CreditAllocation{}, &CreditPack{}, &CreditOperation{}, &CreditAccount{}, &SubscriptionPaymentClaim{}, &SubscriptionPaymentFact{}, &SubscriptionPurchaseOrder{}, &SubscriptionPlanVersion{}, &UserSubscription{}, &SubscriptionPlan{}, &User{}))
				require.NoError(t, sqlDB.Close())
				common.SetMainDatabaseType(previousType)
			})
			require.NoError(t, db.AutoMigrate(&CreditLedgerEntry{}, &CreditAllocation{}, &CreditPack{}, &CreditAccount{}, &CreditOperation{}, &User{}, &SubscriptionPlan{}, &UserSubscription{}, &SubscriptionPlanVersion{}, &SubscriptionPurchaseOrder{}, &SubscriptionPaymentFact{}, &SubscriptionPaymentClaim{}))
			recorder := &migrationSQLRecorder{}
			require.NoError(t, db.Session(&gorm.Session{Logger: recorder}).AutoMigrate(&SubscriptionPlan{}, &UserSubscription{}, &SubscriptionPlanVersion{}, &SubscriptionPurchaseOrder{}, &SubscriptionPaymentFact{}, &SubscriptionPaymentClaim{}))
			assert.Empty(t, recorder.schemaMutations())
			admin := User{Username: "version-admin", Password: "fixture", Role: common.RoleAdminUser, Status: common.UserStatusEnabled, AffCode: "version-admin"}
			customer := User{Username: "version-user", Password: "fixture", Role: common.RoleCommonUser, Status: common.UserStatusEnabled, AffCode: "version-user"}
			require.NoError(t, db.Create(&admin).Error)
			require.NoError(t, db.Create(&customer).Error)
			plan := SubscriptionPlan{Title: "Original", PriceAmount: 10.000001, Currency: "USD", DurationUnit: SubscriptionDurationMonth, DurationValue: 1, TotalAmount: 100, Enabled: true}
			require.NoError(t, common.UnmarshalJsonStr(`{"entitlement_tags":{"tier":"basic","resource":"shared"}}`, &plan))
			require.NoError(t, db.Create(&plan).Error)
			legacy := UserSubscription{UserId: customer.Id, PlanId: plan.Id, StartTime: 100, EndTime: 999, Status: "active", AmountTotal: 100, AmountUsed: 7}
			require.NoError(t, db.Create(&legacy).Error)
			draft, err := GetSubscriptionPlanDraft(db, plan.Id)
			require.NoError(t, err)
			input := SubscriptionVersionPublish{PlanID: plan.Id, ExpectedRevision: 0, ExpectedPlanDigest: draft.Digest, ActorID: admin.Id, EventID: "publish-1"}
			first, err := PublishSubscriptionPlanVersion(db, input, 100)
			require.NoError(t, err)
			assert.EqualValues(t, 1, first.Revision)
			assert.EqualValues(t, 10000001, first.PriceMicros)
			assert.EqualValues(t, 30*24*3600, first.DurationSeconds)
			var contract map[string]any
			require.NoError(t, common.UnmarshalJsonStr(first.Snapshot, &contract))
			assert.Equal(t, map[string]any{"tier": "basic", "resource": "shared"}, contract["entitlement_tags"], "the purchased contract carries configured resource labels")
			require.NoError(t, db.Model(&plan).Update("entitlement_tags", SubscriptionTags{"tier": "updated"}).Error)
			require.NoError(t, db.Model(&plan).Updates(map[string]any{"title": "Edited", "price_amount": 12, "total_amount": 200}).Error)
			replay, err := PublishSubscriptionPlanVersion(db, input, 101)
			require.NoError(t, err)
			assert.Equal(t, first, replay, "response loss replays the published contract, not the edited draft")
			var oldPlan SubscriptionPlan
			require.NoError(t, common.UnmarshalJsonStr(first.Snapshot, &oldPlan))
			assert.Equal(t, "Original", oldPlan.Title)
			assert.Equal(t, SubscriptionTags{"tier": "basic", "resource": "shared"}, oldPlan.EntitlementTags)
			assert.EqualValues(t, 100, oldPlan.TotalAmount)
			input.EventID = "stale-draft"
			_, err = PublishSubscriptionPlanVersion(db, input, 102)
			assert.ErrorIs(t, err, ErrSubscriptionVersionConflict)
			draft, err = GetSubscriptionPlanDraft(db, plan.Id)
			require.NoError(t, err)
			input.ExpectedPlanDigest = draft.Digest
			input.ExpectedRevision = 1
			input.EventID = "publish-2"
			start := make(chan struct{})
			results := make(chan error, 2)
			var workers sync.WaitGroup
			for _, event := range []string{"publish-2", "publish-competitor"} {
				workers.Go(func() {
					candidate := input
					candidate.EventID = event
					<-start
					_, err := PublishSubscriptionPlanVersion(db.Session(&gorm.Session{}), candidate, 103)
					results <- err
				})
			}
			close(start)
			workers.Wait()
			close(results)
			var succeeded, conflicted int
			for err := range results {
				if err == nil {
					succeeded++
				} else {
					assert.ErrorIs(t, err, ErrSubscriptionVersionConflict)
					conflicted++
				}
			}
			assert.Equal(t, 1, succeeded)
			assert.Equal(t, 1, conflicted)
			var versions []SubscriptionPlanVersion
			require.NoError(t, db.Where("plan_id = ?", plan.Id).Order("revision asc").Find(&versions).Error)
			require.Len(t, versions, 2)
			assert.Equal(t, first, versions[0])
			assert.EqualValues(t, 12000000, versions[1].PriceMicros)
			require.NoError(t, db.First(&legacy, legacy.Id).Error)
			assert.EqualValues(t, 999, legacy.EndTime)
			assert.EqualValues(t, 7, legacy.AmountUsed)
			assert.EqualValues(t, 100, legacy.AmountTotal)
			draft, err = GetSubscriptionPlanDraft(db, plan.Id)
			require.NoError(t, err)
			input.ExpectedRevision = 2
			input.ExpectedPlanDigest = draft.Digest
			input.EventID = "failed-publication"
			require.NoError(t, db.Callback().Create().Before("gorm:create").Register("subscription:publication-failure", func(tx *gorm.DB) {
				if tx.Statement.Schema != nil && tx.Statement.Schema.Name == "SubscriptionPlanVersion" {
					tx.AddError(errors.New("publication write unavailable"))
				}
			}))
			_, err = PublishSubscriptionPlanVersion(db, input, 104)
			require.Error(t, err)
			require.NoError(t, db.Callback().Create().Remove("subscription:publication-failure"))
			third, err := PublishSubscriptionPlanVersion(db, input, 104)
			require.NoError(t, err)
			assert.EqualValues(t, 3, third.Revision)
			input.EventID = "ordinary-user"
			input.ExpectedRevision = 3
			input.ActorID = customer.Id
			_, err = PublishSubscriptionPlanVersion(db, input, 105)
			assert.ErrorIs(t, err, ErrUserQuotaPermission)
			input.ActorID = admin.Id
			require.NoError(t, db.Model(&admin).Update("role", common.RoleCommonUser).Error)
			_, err = PublishSubscriptionPlanVersion(db, input, 105)
			assert.ErrorIs(t, err, ErrUserQuotaPermission, "the current database role wins over stale identity")
			require.NoError(t, db.Model(&admin).Update("role", common.RoleAdminUser).Error)
			// Different plan locks still have to arbitrate one global event.
			var candidates []SubscriptionVersionPublish
			for _, title := range []string{"Other A", "Other B"} {
				otherPlan := SubscriptionPlan{Title: title, PriceAmount: 1, Currency: "USD", DurationUnit: SubscriptionDurationMonth, DurationValue: 1, Enabled: true}
				require.NoError(t, db.Create(&otherPlan).Error)
				otherDraft, err := GetSubscriptionPlanDraft(db, otherPlan.Id)
				require.NoError(t, err)
				candidates = append(candidates, SubscriptionVersionPublish{PlanID: otherPlan.Id, ExpectedRevision: 0, ExpectedPlanDigest: otherDraft.Digest, ActorID: admin.Id, EventID: "cross-plan-event"})
			}
			arrived, proceed := make(chan struct{}, 2), make(chan struct{})
			if dialect != "sqlite" {
				require.NoError(t, db.Callback().Create().Before("gorm:create").Register("subscription:cross-plan-race", func(tx *gorm.DB) {
					if version, ok := tx.Statement.Dest.(*SubscriptionPlanVersion); ok && version.EventID == "cross-plan-event" {
						arrived <- struct{}{}
						<-proceed
					}
				}))
			}
			crossResults := make(chan error, 2)
			for _, candidate := range candidates {
				workers.Go(func() {
					_, err := PublishSubscriptionPlanVersion(db.Session(&gorm.Session{}), candidate, 106)
					crossResults <- err
				})
			}
			if dialect != "sqlite" {
				for range 2 {
					select {
					case <-arrived:
					case <-time.After(10 * time.Second):
						close(proceed)
						workers.Wait()
						require.FailNow(t, "publications did not reach the shared event race")
					}
				}
			}
			close(proceed)
			workers.Wait()
			close(crossResults)
			if dialect != "sqlite" {
				require.NoError(t, db.Callback().Create().Remove("subscription:cross-plan-race"))
			}
			succeeded, conflicted = 0, 0
			for err := range crossResults {
				if err == nil {
					succeeded++
				} else {
					assert.ErrorIs(t, err, ErrSubscriptionVersionConflict)
					conflicted++
				}
			}
			assert.Equal(t, 1, succeeded)
			assert.Equal(t, 1, conflicted)
			var count int64
			require.NoError(t, db.Model(&SubscriptionPlanVersion{}).Where("event_id = ?", "cross-plan-event").Count(&count).Error)
			assert.EqualValues(t, 1, count)

			t.Run("invalid entitlement metadata cannot publish", func(t *testing.T) {
				for _, tags := range []SubscriptionTags{{" ": "bad"}, {"resource": strings.Repeat("x", 2049)}} {
					badPlan := SubscriptionPlan{Title: "Invalid labels", PriceAmount: 1, Currency: "USD", DurationUnit: SubscriptionDurationMonth, DurationValue: 1, EntitlementTags: tags}
					require.NoError(t, db.Create(&badPlan).Error)
					badDraft, err := GetSubscriptionPlanDraft(db, badPlan.Id)
					require.NoError(t, err)
					_, err = PublishSubscriptionPlanVersion(db, SubscriptionVersionPublish{PlanID: badPlan.Id, ExpectedRevision: 0, ExpectedPlanDigest: badDraft.Digest, ActorID: admin.Id, EventID: fmt.Sprintf("invalid-label-%d", badPlan.Id)}, 200)
					assert.ErrorIs(t, err, ErrCreditInvalid)
					var count int64
					require.NoError(t, db.Model(&SubscriptionPlanVersion{}).Where("plan_id = ?", badPlan.Id).Count(&count).Error)
					assert.Zero(t, count)
				}
			})

			t.Run("locked orders and verified payment facts", func(t *testing.T) {
				purchase := SubscriptionPurchaseInput{UserID: customer.Id, VersionID: third.ID, Provider: PaymentMethodStripe, EventID: "purchase-event", ExpiresAt: 500}
				order, err := CreateSubscriptionPurchaseOrder(db, purchase, 200)
				require.NoError(t, err)
				assert.EqualValues(t, 12000000, order.PriceMicros)
				assert.Equal(t, third.Snapshot, order.ContractSnapshot)
				assert.Equal(t, "pending", order.PaymentState)
				require.NoError(t, db.Model(&plan).Updates(map[string]any{"price_amount": 99, "enabled": false}).Error)
				replayed, err := CreateSubscriptionPurchaseOrder(db, purchase, 201)
				require.NoError(t, err)
				assert.Equal(t, order, replayed)
				replayed, err = CreateSubscriptionPurchaseOrder(db, purchase, 600)
				require.NoError(t, err, "an expired order is still the same committed contract on response-loss replay")
				assert.Equal(t, order, replayed)
				changed := purchase
				changed.ExpiresAt++
				_, err = CreateSubscriptionPurchaseOrder(db, changed, 202)
				assert.ErrorIs(t, err, ErrSubscriptionPurchaseConflict)
				changed = purchase
				changed.EventID = "disabled-purchase"
				_, err = CreateSubscriptionPurchaseOrder(db, changed, 202)
				assert.ErrorIs(t, err, ErrSubscriptionPurchaseUnavailable)
				require.NoError(t, db.Model(&plan).Updates(map[string]any{"price_amount": 12, "enabled": true}).Error)
				changed.VersionID = first.ID
				_, err = CreateSubscriptionPurchaseOrder(db, changed, 202)
				assert.ErrorIs(t, err, ErrSubscriptionPurchaseConflict)
				payment := VerifiedSubscriptionPayment{OrderID: order.ID, Provider: PaymentMethodStripe, EventID: "payment-event", ReferenceID: "payment-ref", BuyerID: customer.Id, AmountMicros: common.GetPointer(int64(12000000)), Currency: "USD", PaidAt: common.GetPointer(int64(250)), PaidAtSource: "verified-provider-paid-at", Succeeded: true, EvidenceDigest: strings.Repeat("a", 64)}
				fact, err := RecordSubscriptionPaymentFact(db, payment, 900)
				require.NoError(t, err)
				assert.Equal(t, "verified", fact.Outcome)
				require.NoError(t, db.First(&order, order.ID).Error)
				assert.EqualValues(t, 250, order.PaidAt, "delayed notifications do not move the actual payment time")
				assert.Equal(t, "verified", order.PaymentState)
				assert.False(t, order.NeedsReview)
				factReplay, err := RecordSubscriptionPaymentFact(db, payment, 901)
				require.NoError(t, err)
				assert.Equal(t, fact, factReplay)
				conflict := payment
				conflict.Currency = "CNY"
				_, err = RecordSubscriptionPaymentFact(db, conflict, 902)
				assert.ErrorIs(t, err, ErrSubscriptionPurchaseConflict)
				require.NoError(t, db.First(&order, order.ID).Error)
				assert.True(t, order.NeedsReview, "conflicting authenticated evidence must remain visible after returning an error")
				assert.Equal(t, "event_payload_conflict", order.LastReviewReason)
				var contradictory []SubscriptionPaymentFact
				require.NoError(t, db.Where("original_fact_id = ?", fact.ID).Find(&contradictory).Error)
				require.Len(t, contradictory, 1)
				assert.Equal(t, "CNY", contradictory[0].Currency)
				assert.Equal(t, "review", contradictory[0].Outcome)
				assert.Equal(t, "event_payload_conflict", contradictory[0].ReviewReason)
				_, err = RecordSubscriptionPaymentFact(db, conflict, 903)
				assert.ErrorIs(t, err, ErrSubscriptionPurchaseConflict)
				var contradictionCount int64
				require.NoError(t, db.Model(&SubscriptionPaymentFact{}).Where("original_fact_id = ?", fact.ID).Count(&contradictionCount).Error)
				assert.EqualValues(t, 1, contradictionCount, "retry preserves one contradictory observation, not duplicate audit rows")
				var unchangedFact SubscriptionPaymentFact
				require.NoError(t, db.First(&unchangedFact, fact.ID).Error)
				assert.Equal(t, fact, unchangedFact, "the original money evidence is immutable")
				assert.EqualValues(t, 250, order.PaidAt)
				assert.Equal(t, "verified", order.PaymentState)
				conflict = payment
				conflict.Provider = PaymentMethodCreem
				_, err = RecordSubscriptionPaymentFact(db, conflict, 902)
				assert.ErrorIs(t, err, ErrPaymentMethodMismatch)
				// Missing values stay unknown; a valid signed receipt alone is not a
				// license to substitute the order amount or notification timestamp.
				for _, tc := range []struct {
					name, reason string
					mutate       func(*VerifiedSubscriptionPayment)
				}{
					{"missing amount", "missing_amount", func(p *VerifiedSubscriptionPayment) { p.AmountMicros = nil }},
					{"wrong amount", "amount_mismatch", func(p *VerifiedSubscriptionPayment) { p.AmountMicros = common.GetPointer(int64(1)) }},
					{"wrong currency", "currency_mismatch", func(p *VerifiedSubscriptionPayment) { p.Currency = "CNY" }},
					{"missing paid time", "missing_paid_at", func(p *VerifiedSubscriptionPayment) { p.PaidAt = nil }},
					{"before order", "paid_at_outside_order", func(p *VerifiedSubscriptionPayment) { p.PaidAt = common.GetPointer(int64(199)) }},
					{"expired order", "paid_at_outside_order", func(p *VerifiedSubscriptionPayment) { p.PaidAt = common.GetPointer(int64(500)) }},
					{"wrong buyer", "buyer_mismatch", func(p *VerifiedSubscriptionPayment) { p.BuyerID = admin.Id }},
				} {
					t.Run(tc.name, func(t *testing.T) {
						candidate := payment
						candidate.EventID = tc.name
						candidate.ReferenceID = tc.name
						tc.mutate(&candidate)
						observed, err := RecordSubscriptionPaymentFact(db, candidate, 903)
						require.NoError(t, err)
						assert.Equal(t, "review", observed.Outcome)
						assert.Equal(t, tc.reason, observed.ReviewReason)
						assert.Equal(t, candidate.AmountMicros, observed.AmountMicros)
						assert.Equal(t, candidate.PaidAt, observed.PaidAt)
					})
				}
				require.NoError(t, db.First(&order, order.ID).Error)
				assert.True(t, order.NeedsReview)
				assert.EqualValues(t, 250, order.PaidAt)
				// A new event for the original payment does not erase open questions.
				payment.EventID = "another-event-same-payment"
				_, err = RecordSubscriptionPaymentFact(db, payment, 904)
				require.NoError(t, err)
				require.NoError(t, db.First(&order, order.ID).Error)
				assert.True(t, order.NeedsReview)
				changed = purchase
				changed.EventID = "other-order"
				otherOrder, err := CreateSubscriptionPurchaseOrder(db, changed, 205)
				require.NoError(t, err)
				payment.OrderID = otherOrder.ID
				payment.EventID = "same-payment-other-order"
				otherFact, err := RecordSubscriptionPaymentFact(db, payment, 905)
				require.NoError(t, err)
				assert.Equal(t, "payment_already_claimed", otherFact.ReviewReason)
				require.NoError(t, db.First(&otherOrder, otherOrder.ID).Error)
				assert.True(t, otherOrder.NeedsReview)
				assert.Zero(t, otherOrder.PaidAt)
				// A failure after inserting the fact must roll back transaction
				// ownership and order state together, so the same event can retry.
				changed.EventID = "rollback-order"
				rollbackOrder, err := CreateSubscriptionPurchaseOrder(db, changed, 206)
				require.NoError(t, err)
				payment.OrderID, payment.EventID, payment.ReferenceID = rollbackOrder.ID, "rollback-fact", "rollback-reference"
				require.NoError(t, db.Callback().Create().Before("gorm:create").Register("subscription:payment-failure", func(tx *gorm.DB) {
					if claim, ok := tx.Statement.Dest.(*SubscriptionPaymentClaim); ok && claim.ReferenceID == "rollback-reference" {
						tx.AddError(errors.New("payment ownership storage unavailable"))
					}
				}))
				_, err = RecordSubscriptionPaymentFact(db, payment, 906)
				require.Error(t, err)
				require.NoError(t, db.Callback().Create().Remove("subscription:payment-failure"))
				var storedFacts int64
				require.NoError(t, db.Model(&SubscriptionPaymentFact{}).Where("event_id = ?", payment.EventID).Count(&storedFacts).Error)
				assert.Zero(t, storedFacts)
				require.NoError(t, db.First(&rollbackOrder, rollbackOrder.ID).Error)
				assert.Equal(t, "pending", rollbackOrder.PaymentState)
				assert.Zero(t, rollbackOrder.PaidAt)
				_, err = RecordSubscriptionPaymentFact(db, payment, 907)
				require.NoError(t, err)
				// Two independent order locks cannot both own one payment. Force
				// the SQL contenders to meet immediately before that unique claim.
				var competing []VerifiedSubscriptionPayment
				for _, event := range []string{"claim-race-a", "claim-race-b"} {
					changed.EventID = event
					raceOrder, err := CreateSubscriptionPurchaseOrder(db, changed, 207)
					require.NoError(t, err)
					candidate := payment
					candidate.OrderID, candidate.EventID, candidate.ReferenceID = raceOrder.ID, event, "shared-race-reference"
					competing = append(competing, candidate)
				}
				atClaim, allowClaim := make(chan struct{}, 2), make(chan struct{})
				if dialect != "sqlite" {
					require.NoError(t, db.Callback().Create().Before("gorm:create").Register("subscription:payment-race", func(tx *gorm.DB) {
						if claim, ok := tx.Statement.Dest.(*SubscriptionPaymentClaim); ok && claim.ReferenceID == "shared-race-reference" {
							atClaim <- struct{}{}
							<-allowClaim
						}
					}))
				}
				type receiptResult struct {
					fact SubscriptionPaymentFact
					err  error
				}
				receipts := make(chan receiptResult, 2)
				for _, candidate := range competing {
					workers.Go(func() {
						fact, err := RecordSubscriptionPaymentFact(db.Session(&gorm.Session{}), candidate, 908)
						receipts <- receiptResult{fact, err}
					})
				}
				if dialect != "sqlite" {
					for range 2 {
						select {
						case <-atClaim:
						case <-time.After(10 * time.Second):
							close(allowClaim)
							workers.Wait()
							require.FailNow(t, "payments did not reach the shared transaction race")
						}
					}
				}
				close(allowClaim)
				workers.Wait()
				close(receipts)
				if dialect != "sqlite" {
					require.NoError(t, db.Callback().Create().Remove("subscription:payment-race"))
				}
				verified, underReview := 0, 0
				for result := range receipts {
					require.NoError(t, result.err)
					if result.fact.Outcome == "verified" {
						verified++
					} else {
						underReview++
						assert.Equal(t, "payment_already_claimed", result.fact.ReviewReason)
					}
				}
				assert.Equal(t, 1, verified)
				assert.Equal(t, 1, underReview)
				var claims int64
				require.NoError(t, db.Model(&SubscriptionPaymentClaim{}).Where("reference_id = ?", "shared-race-reference").Count(&claims).Error)
				assert.EqualValues(t, 1, claims)
				// A global event can race between different orders. The losing
				// observation must be committed for review without rewriting the
				// original event or claiming the payment for a second order.
				competing = nil
				for _, event := range []string{"event-race-a", "event-race-b"} {
					changed.EventID = event
					raceOrder, err := CreateSubscriptionPurchaseOrder(db, changed, 209)
					require.NoError(t, err)
					candidate := payment
					candidate.OrderID, candidate.EventID, candidate.ReferenceID = raceOrder.ID, "shared-provider-event", "shared-event-reference"
					competing = append(competing, candidate)
				}
				atEvent, allowEvent := make(chan struct{}, 2), make(chan struct{})
				if dialect != "sqlite" {
					require.NoError(t, db.Callback().Create().Before("gorm:create").Register("subscription:payment-event-race", func(tx *gorm.DB) {
						if fact, ok := tx.Statement.Dest.(*SubscriptionPaymentFact); ok && fact.EventID == "shared-provider-event" && fact.OriginalFactID == 0 {
							atEvent <- struct{}{}
							<-allowEvent
						}
					}))
				}
				receipts = make(chan receiptResult, 2)
				for _, candidate := range competing {
					workers.Go(func() {
						fact, err := RecordSubscriptionPaymentFact(db.Session(&gorm.Session{}), candidate, 909)
						receipts <- receiptResult{fact, err}
					})
				}
				if dialect != "sqlite" {
					for range 2 {
						select {
						case <-atEvent:
						case <-time.After(10 * time.Second):
							close(allowEvent)
							workers.Wait()
							require.FailNow(t, "payments did not reach the shared event race")
						}
					}
				}
				close(allowEvent)
				workers.Wait()
				close(receipts)
				if dialect != "sqlite" {
					require.NoError(t, db.Callback().Create().Remove("subscription:payment-event-race"))
				}
				verified, underReview = 0, 0
				var original, contradictoryFact SubscriptionPaymentFact
				for result := range receipts {
					if result.err == nil {
						verified++
						original = result.fact
						assert.Equal(t, "verified", original.Outcome)
					} else {
						assert.ErrorIs(t, result.err, ErrSubscriptionPurchaseConflict)
						underReview++
						contradictoryFact = result.fact
						assert.Equal(t, "event_payload_conflict", contradictoryFact.ReviewReason)
					}
				}
				assert.Equal(t, 1, verified)
				assert.Equal(t, 1, underReview)
				assert.Equal(t, original.ID, contradictoryFact.OriginalFactID)
				var rejectedOrder SubscriptionPurchaseOrder
				require.NoError(t, db.First(&rejectedOrder, contradictoryFact.OrderID).Error)
				assert.True(t, rejectedOrder.NeedsReview)
				assert.Equal(t, "pending", rejectedOrder.PaymentState)
				require.NoError(t, db.Model(&SubscriptionPaymentClaim{}).Where("reference_id = ?", "shared-event-reference").Count(&claims).Error)
				assert.EqualValues(t, 1, claims)
				var subscriptions int64
				require.NoError(t, db.Model(&UserSubscription{}).Count(&subscriptions).Error)
				assert.EqualValues(t, 1, subscriptions, "recording money facts does not activate or extend the legacy subscription")
			})

			t.Run("cash checkout issuance is durable and serialized", func(t *testing.T) {
				buyer := User{Username: "checkout-issuance-buyer", Password: "fixture", Role: common.RoleCommonUser, Status: common.UserStatusEnabled, AccountingVersion: 1, AffCode: "checkout-issuance-buyer"}
				require.NoError(t, db.Create(&buyer).Error)
				product := SubscriptionPlan{Title: "Issuance contract", PriceAmount: 1, Currency: "USD", DurationUnit: SubscriptionDurationMonth, DurationValue: 1, Enabled: true, TotalAmount: 100}
				require.NoError(t, db.Create(&product).Error)
				draft, err := GetSubscriptionPlanDraft(db, product.Id)
				require.NoError(t, err)
				version, err := PublishSubscriptionPlanVersion(db, SubscriptionVersionPublish{PlanID: product.Id, ActorID: admin.Id, ExpectedPlanDigest: draft.Digest, EventID: "publish-issuance"}, 100)
				require.NoError(t, err)
				order, err := CreateVersionedSubscriptionCheckout(db, buyer.Id, version.ID, PaymentProviderCreem, "issuance-order", 200)
				require.NoError(t, err)
				atWriter, allowWrite := make(chan struct{}, 2), make(chan struct{})
				require.NoError(t, db.Callback().Create().Before("gorm:create").Register("subscription:issuance-race", func(tx *gorm.DB) {
					if account, ok := tx.Statement.Dest.(*CreditAccount); ok && account.UserID == buyer.Id {
						atWriter <- struct{}{}
						<-allowWrite
					}
				}))
				type issuanceResult struct {
					issue bool
					err   error
				}
				results := make(chan issuanceResult, 2)
				var writers sync.WaitGroup
				for range 2 {
					writers.Go(func() {
						_, issue, err := BeginSubscriptionCheckout(db.Session(&gorm.Session{}), buyer.Id, order.ID, strings.Repeat("a", 64), 201)
						results <- issuanceResult{issue, err}
					})
				}
				for range 2 {
					select {
					case <-atWriter:
					case <-time.After(5 * time.Second):
						close(allowWrite)
						writers.Wait()
						t.Fatal("cash checkout contenders did not reach independent SQL transactions")
					}
				}
				close(allowWrite)
				writers.Wait()
				require.NoError(t, db.Callback().Create().Remove("subscription:issuance-race"))
				close(results)
				issued, blocked := 0, 0
				for result := range results {
					if result.err == nil {
						assert.True(t, result.issue)
						issued++
					} else {
						assert.False(t, result.issue)
						assert.ErrorIs(t, result.err, ErrSubscriptionPurchaseUnavailable)
						blocked++
					}
				}
				assert.Equal(t, 1, issued)
				assert.Equal(t, 1, blocked)
				// Restart/retry sees durable started state rather than issuing again.
				_, issue, err := BeginSubscriptionCheckout(db.Session(&gorm.Session{}), buyer.Id, order.ID, strings.Repeat("a", 64), 202)
				assert.False(t, issue)
				assert.ErrorIs(t, err, ErrSubscriptionPurchaseUnavailable)

				t.Run("payment_link_never_enters_debug_SQL_audit", func(t *testing.T) {
					var audit bytes.Buffer
					oldDebug := common.DebugEnabled
					common.DebugEnabled = true
					defer func() { common.DebugEnabled = oldDebug }()
					require.NoError(t, db.Callback().Update().After("gorm:update").Register("subscription:checkout-save-fail", func(tx *gorm.DB) {
						if tx.Statement.Schema != nil && tx.Statement.Schema.Name == "SubscriptionPurchaseOrder" {
							tx.AddError(errors.New("fixture checkout storage failure"))
						}
					}))
					_, err := SaveSubscriptionCheckout(db.Session(&gorm.Session{Logger: newGormLogger(&audit)}), buyer.Id, order.ID, strings.Repeat("a", 64), "ch_issuance", `{"checkout_url":"https://checkout.example.invalid/saved?token=usable-cash-link"}`)
					require.Error(t, err)
					require.NoError(t, db.Callback().Update().Remove("subscription:checkout-save-fail"))
					assert.NotContains(t, audit.String(), "usable-cash-link")
					var current SubscriptionPurchaseOrder
					require.NoError(t, db.First(&current, order.ID).Error)
					assert.Equal(t, "started", current.CheckoutState)
				})
				response := `{"checkout_url":"https://checkout.example.invalid/saved"}`
				saved, err := SaveSubscriptionCheckout(db, buyer.Id, order.ID, strings.Repeat("a", 64), "ch_issuance", response)
				require.NoError(t, err)
				replay, issue, err := BeginSubscriptionCheckout(db.Session(&gorm.Session{}), buyer.Id, order.ID, strings.Repeat("a", 64), 203)
				require.NoError(t, err)
				assert.False(t, issue)
				assert.Equal(t, saved.CheckoutResponse, replay.CheckoutResponse)
				_, _, err = BeginSubscriptionCheckout(db, buyer.Id, order.ID, strings.Repeat("b", 64), 204)
				assert.ErrorIs(t, err, ErrSubscriptionPurchaseConflict)
				_, err = SaveSubscriptionCheckout(db, buyer.Id+100000, order.ID, strings.Repeat("a", 64), "ch_issuance", response)
				assert.ErrorIs(t, err, gorm.ErrRecordNotFound)
				assert.ErrorIs(t, MarkSubscriptionCheckoutUnknown(db, buyer.Id, order.ID, strings.Repeat("a", 64)), ErrSubscriptionPurchaseConflict, "a confirmed response cannot be overwritten as unknown")
			})

			t.Run("manual payment review preserves evidence and serializes activation", func(t *testing.T) {
				buyer := User{Username: "review-buyer", Password: "fixture", Status: common.UserStatusEnabled, Role: common.RoleCommonUser, AccountingVersion: 1, AffCode: "review-buyer"}
				require.NoError(t, db.Create(&buyer).Error)
				order, err := CreateSubscriptionPurchaseOrder(db, SubscriptionPurchaseInput{UserID: buyer.Id, VersionID: third.ID, Provider: PaymentProviderCreem, EventID: "review-order", ExpiresAt: 1000}, 200)
				require.NoError(t, err)
				missing, err := RecordSubscriptionPaymentFact(db, VerifiedSubscriptionPayment{OrderID: order.ID, Provider: order.Provider, EventID: "missing-time-review", ReferenceID: "review-payment", BuyerID: buyer.Id, AmountMicros: common.GetPointer(order.PriceMicros), Currency: order.Currency, Succeeded: true, EvidenceDigest: strings.Repeat("a", 64)}, 900)
				require.NoError(t, err)
				review := SubscriptionPaymentReview{OrderID: order.ID, ActorID: admin.Id, EventID: "review-approve", ExpectedFactID: missing.ID, ReferenceID: "review-payment", AmountMicros: order.PriceMicros, Currency: order.Currency, PaidAt: 250, EvidenceReference: "provider receipt external-review-1", Reason: "confirmed provider payment timestamp"}
				for _, name := range []string{"self", "stale", "amount", "time", "blank_evidence"} {
					input := review
					switch name {
					case "self":
						input.ActorID = buyer.Id
					case "stale":
						input.ExpectedFactID = 0
					case "amount":
						input.AmountMicros++
					case "time":
						input.PaidAt = order.ExpiresAt
					case "blank_evidence":
						input.EvidenceReference = ""
					}
					_, err := ResolveSubscriptionPaymentReview(db, input, 901)
					require.Error(t, err, name)
				}
				require.NoError(t, db.Callback().Create().Before("gorm:create").Register("subscription:review-activation-fail", func(tx *gorm.DB) {
					if tx.Statement.Schema != nil && tx.Statement.Schema.Name == "UserSubscription" {
						tx.AddError(errors.New("rights storage unavailable"))
					}
				}))
				_, err = ResolveSubscriptionPaymentReview(db, review, 901)
				require.Error(t, err)
				require.NoError(t, db.Callback().Create().Remove("subscription:review-activation-fail"))
				var count int64
				require.NoError(t, db.Model(&SubscriptionPaymentFact{}).Where("order_id = ?", order.ID).Count(&count).Error)
				assert.EqualValues(t, 1, count, "failed activation rolls back review approval, never removes original facts")
				require.NoError(t, db.First(&order, order.ID).Error)
				assert.True(t, order.NeedsReview)
				result, err := ResolveSubscriptionPaymentReview(db, review, 902)
				require.NoError(t, err)
				assert.Equal(t, admin.Id, result.ActorID)
				replayed, err := ResolveSubscriptionPaymentReview(db, review, 903)
				require.NoError(t, err)
				assert.Equal(t, result, replayed)
				changed := review
				changed.PaidAt++
				_, err = ResolveSubscriptionPaymentReview(db, changed, 904)
				assert.ErrorIs(t, err, ErrCreditOperationConflict)
				var original SubscriptionPaymentFact
				require.NoError(t, db.First(&original, missing.ID).Error)
				assert.Equal(t, "review", original.Outcome)
				assert.Nil(t, original.PaidAt, "admin approval cannot rewrite provider history")
				var rights UserSubscription
				require.NoError(t, db.First(&rights, result.SubscriptionID).Error)
				assert.EqualValues(t, 250, rights.StartTime)
				require.NoError(t, db.Model(&UserSubscription{}).Where("purchase_order_id = ?", order.ID).Count(&count).Error)
				assert.EqualValues(t, 1, count)
				require.NoError(t, db.Model(&CreditOperation{}).Where("user_id = ? AND kind = ?", buyer.Id, "payment_review").Count(&count).Error)
				assert.EqualValues(t, 1, count)
				for _, state := range []string{"started", "cancelled", "claimed_elsewhere"} {
					candidate, err := CreateSubscriptionPurchaseOrder(db, SubscriptionPurchaseInput{UserID: buyer.Id, VersionID: third.ID, Provider: PaymentProviderCreem, EventID: "review-" + state, ExpiresAt: 1000}, 200)
					require.NoError(t, err)
					_, issue, err := BeginSubscriptionCheckout(db, buyer.Id, candidate.ID, strings.Repeat("a", 64), 201)
					require.NoError(t, err)
					require.True(t, issue)
					approval := review
					approval.OrderID = candidate.ID
					approval.EventID = "approve-" + state
					approval.ExpectedFactID = 0
					approval.ReferenceID = "external-" + state
					if state == "claimed_elsewhere" {
						approval.ReferenceID = review.ReferenceID
					}
					if state == "cancelled" {
						require.NoError(t, CancelSubscriptionRights(db, SubscriptionRightsCancellation{UserID: buyer.Id, SubscriptionID: rights.Id, ActorID: admin.Id, EventID: "review-cancel", Reason: "stop rights"}, 905))
						// Later contradictory facts change the displayed reason but must
						// not erase the administrator's cancellation fence.
						_, err = RecordSubscriptionPaymentFact(db, VerifiedSubscriptionPayment{OrderID: candidate.ID, Provider: candidate.Provider, EventID: "cancelled-mismatch", ReferenceID: "cancelled-wrong", BuyerID: buyer.Id, AmountMicros: common.GetPointer(int64(0)), Currency: order.Currency, Succeeded: true, EvidenceDigest: strings.Repeat("b", 64)}, 906)
						require.NoError(t, err)
						var latest SubscriptionPaymentFact
						require.NoError(t, db.Where("order_id = ?", candidate.ID).Order("id desc").First(&latest).Error)
						approval.ExpectedFactID = latest.ID
					}
					_, err = ResolveSubscriptionPaymentReview(db, approval, 907)
					if state == "started" {
						require.NoError(t, err)
					} else {
						require.Error(t, err, state)
					}
				}
			})

			t.Run("balance purchase is atomic and uses eligible earliest expiry", func(t *testing.T) {
				previousUnit := common.QuotaPerUnit
				common.QuotaPerUnit = 100
				t.Cleanup(func() { common.QuotaPerUnit = previousUnit })
				buyer := User{Username: "balance-renewal-user", Password: "fixture", Status: common.UserStatusEnabled, Role: common.RoleCommonUser, AccountingVersion: 1, AffCode: "balance-renewal-user"}
				require.NoError(t, db.Create(&buyer).Error)
				product := SubscriptionPlan{Title: "Balance renewal", Currency: "USD", PriceAmount: 1.000001, DurationUnit: SubscriptionDurationMonth, DurationValue: 1, TotalAmount: 200, Enabled: true}
				require.NoError(t, db.Create(&product).Error)
				draft, err := GetSubscriptionPlanDraft(db, product.Id)
				require.NoError(t, err)
				version, err := PublishSubscriptionPlanVersion(db, SubscriptionVersionPublish{PlanID: product.Id, ExpectedPlanDigest: draft.Digest, ActorID: admin.Id, EventID: "balance-version"}, 100)
				require.NoError(t, err)
				apiOnly, err := GrantCreditPack(db, CreditGrant{UserID: buyer.Id, SourceType: "checkin", SourceID: "balance-api-only", Amount: 1000, StartsAt: 100, ExpiresAt: 900, UseMask: CreditUseAPI}, 100)
				require.NoError(t, err)
				input := SubscriptionBalancePurchase{UserID: buyer.Id, VersionID: version.ID, EventID: "balance-first"}
				_, err = PurchaseVersionedSubscriptionWithBalance(db, input, 200)
				assert.ErrorIs(t, err, ErrCreditInsufficient, "API-only gifts cannot purchase a subscription")
				var count int64
				require.NoError(t, db.Model(&SubscriptionPurchaseOrder{}).Where("user_id = ?", buyer.Id).Count(&count).Error)
				assert.Zero(t, count, "failed debit does not leave a partially committed order")
				early, err := GrantCreditPack(db, CreditGrant{UserID: buyer.Id, SourceType: "topup", SourceID: "balance-early", Amount: 60, StartsAt: 100, ExpiresAt: 1000, UseMask: CreditUseAPI | CreditUseSubscription}, 100)
				require.NoError(t, err)
				late, err := GrantCreditPack(db, CreditGrant{UserID: buyer.Id, SourceType: "topup", SourceID: "balance-late", Amount: 100, StartsAt: 100, ExpiresAt: 2000, UseMask: CreditUseSubscription}, 100)
				require.NoError(t, err)
				require.NoError(t, db.Callback().Create().Before("gorm:create").Register("subscription:activation-failure", func(tx *gorm.DB) {
					if tx.Statement.Schema != nil && tx.Statement.Schema.Name == "UserSubscription" {
						tx.AddError(errors.New("activation unavailable"))
					}
				}))
				_, err = PurchaseVersionedSubscriptionWithBalance(db, input, 200)
				require.Error(t, err)
				require.NoError(t, db.Callback().Create().Remove("subscription:activation-failure"))
				require.NoError(t, db.First(&early, early.ID).Error)
				assert.EqualValues(t, 60, early.Available, "rights write failure rolls back pack debit")
				rights, err := PurchaseVersionedSubscriptionWithBalance(db, input, 200)
				require.NoError(t, err)
				assert.EqualValues(t, 200, rights.StartTime)
				assert.EqualValues(t, 200+30*24*3600, rights.EndTime)
				require.NoError(t, db.First(&early, early.ID).Error)
				require.NoError(t, db.First(&late, late.ID).Error)
				require.NoError(t, db.First(&apiOnly, apiOnly.ID).Error)
				assert.EqualValues(t, 60, early.Spent)
				assert.EqualValues(t, 41, late.Spent, "same New API conversion rounds fractional fee up once")
				assert.EqualValues(t, 1000, apiOnly.Available)
				common.QuotaPerUnit = 500
				replay, err := PurchaseVersionedSubscriptionWithBalance(db, input, 210)
				require.NoError(t, err)
				assert.Equal(t, rights, replay, "response-loss replay neither reprices nor renews again")
				require.NoError(t, db.First(&late, late.ID).Error)
				assert.EqualValues(t, 41, late.Spent)
				require.NoError(t, db.Model(&SubscriptionPurchaseOrder{}).Where("user_id = ?", buyer.Id).Count(&count).Error)
				assert.EqualValues(t, 1, count)
			})

			t.Run("renewal preserves purchased contract and remaining term", func(t *testing.T) {
				buyer := User{Username: "renewal-user", Password: "fixture", Status: common.UserStatusEnabled, Role: common.RoleCommonUser, AccountingVersion: 1, AffCode: "renewal-user", Group: "default"}
				require.NoError(t, db.Create(&buyer).Error)
				product := SubscriptionPlan{Title: "Renewable", Currency: "USD", PriceAmount: 1, DurationUnit: SubscriptionDurationMonth, DurationValue: 1, TotalAmount: 100, EntitlementTags: SubscriptionTags{"tier": "basic"}, UpgradeGroup: "premium", DowngradeGroup: "default", Enabled: true}
				require.NoError(t, db.Create(&product).Error)
				draft, err := GetSubscriptionPlanDraft(db, product.Id)
				require.NoError(t, err)
				version, err := PublishSubscriptionPlanVersion(db, SubscriptionVersionPublish{PlanID: product.Id, ExpectedRevision: 0, ExpectedPlanDigest: draft.Digest, ActorID: admin.Id, EventID: "renewal-publish"}, 100)
				require.NoError(t, err)
				order, err := CreateSubscriptionPurchaseOrder(db, SubscriptionPurchaseInput{UserID: buyer.Id, VersionID: version.ID, Provider: PaymentMethodStripe, EventID: "renewal-first", ExpiresAt: 1000}, 200)
				require.NoError(t, err)
				_, err = ActivateSubscriptionPurchase(db, buyer.Id, order.ID, 250)
				assert.ErrorIs(t, err, ErrSubscriptionPurchaseUnavailable, "a pending payment cannot grant rights")
				_, err = RecordSubscriptionPaymentFact(db, VerifiedSubscriptionPayment{OrderID: order.ID, Provider: order.Provider, EventID: "renewal-payment", ReferenceID: "renewal-transaction", BuyerID: buyer.Id, AmountMicros: common.GetPointer(order.PriceMicros), Currency: order.Currency, PaidAt: common.GetPointer(int64(250)), PaidAtSource: "provider-paid-at", Succeeded: true, EvidenceDigest: strings.Repeat("a", 64)}, 900)
				require.NoError(t, err)
				first, err := ActivateSubscriptionPurchase(db, buyer.Id, order.ID, 900)
				require.NoError(t, err)
				assert.EqualValues(t, 250, first.StartTime)
				assert.EqualValues(t, 250+30*24*3600, first.EndTime)
				assert.Equal(t, SubscriptionTags{"tier": "basic"}, first.EntitlementTags)
				var currentBuyer User
				require.NoError(t, db.First(&currentBuyer, buyer.Id).Error)
				assert.Equal(t, "premium", currentBuyer.Group)
				assert.Equal(t, "premium", first.UpgradeGroup)
				// Old administration/cash completion and single-counter charging
				// cannot create or mutate contracts for a new-accounting buyer.
				var legacyErr error
				_ = db.Transaction(func(tx *gorm.DB) error {
					_, legacyErr = CreateUserSubscriptionFromPlanTx(tx, buyer.Id, &product, "legacy-admin")
					return errors.New("fixture rollback")
				})
				assert.ErrorIs(t, legacyErr, ErrCreditOperationRequired)
				oldBillingDB := DB
				DB = db
				_, preconsumeErr := PreConsumeUserSubscription("legacy-version-bypass", buyer.Id, "model", 0, 1)
				deltaErr := PostConsumeUserSubscriptionDelta(first.Id, 1)
				DB = oldBillingDB
				assert.ErrorIs(t, preconsumeErr, ErrCreditOperationRequired)
				assert.ErrorIs(t, deltaErr, ErrCreditOperationRequired)

				replay, err := ActivateSubscriptionPurchase(db, buyer.Id, order.ID, 901)
				require.NoError(t, err)
				assert.Equal(t, first, replay)
				halfway := int64(250 + 15*24*3600)
				require.NoError(t, db.Model(&product).Updates(map[string]any{"price_amount": 2, "entitlement_tags": SubscriptionTags{"tier": "new"}, "total_amount": 200}).Error)
				draft, err = GetSubscriptionPlanDraft(db, product.Id)
				require.NoError(t, err)
				version2, err := PublishSubscriptionPlanVersion(db, SubscriptionVersionPublish{PlanID: product.Id, ExpectedRevision: 1, ExpectedPlanDigest: draft.Digest, ActorID: admin.Id, EventID: "renewal-publish-v2"}, halfway)
				require.NoError(t, err)
				next, err := CreateSubscriptionPurchaseOrder(db, SubscriptionPurchaseInput{UserID: buyer.Id, VersionID: version2.ID, Provider: PaymentMethodStripe, EventID: "renewal-second", ExpiresAt: halfway + 1000}, halfway)
				require.NoError(t, err)
				_, err = RecordSubscriptionPaymentFact(db, VerifiedSubscriptionPayment{OrderID: next.ID, Provider: next.Provider, EventID: "renewal-payment2", ReferenceID: "renewal-transaction2", BuyerID: buyer.Id, AmountMicros: common.GetPointer(next.PriceMicros), Currency: next.Currency, PaidAt: common.GetPointer(halfway), PaidAtSource: "provider-paid-at", Succeeded: true, EvidenceDigest: strings.Repeat("b", 64)}, halfway+1)
				require.NoError(t, err)
				second, err := ActivateSubscriptionPurchase(db, buyer.Id, next.ID, halfway+2)
				require.NoError(t, err)
				assert.Equal(t, first.EndTime, second.StartTime)
				assert.EqualValues(t, 250+60*24*3600, second.EndTime)
				assert.Equal(t, "scheduled", second.Status)
				assert.Equal(t, SubscriptionTags{"tier": "new"}, second.EntitlementTags)
				effective, err := GetUserSubscriptionRights(db, buyer.Id, halfway+3)
				require.NoError(t, err)
				require.Len(t, effective, 1, "prepaid future period grants no quota early")
				assert.Equal(t, SubscriptionTags{"tier": "basic"}, effective[0].EntitlementTags)
				assert.Equal(t, second.EndTime, effective[0].RenewalEndTime, "visible subscription expiry includes the paid extension")
				require.NoError(t, db.First(&first, first.Id).Error)
				assert.EqualValues(t, 100, first.AmountTotal)
				assert.Equal(t, SubscriptionTags{"tier": "basic"}, first.EntitlementTags, "new catalog and renewal do not change the active purchase")
				assert.EqualValues(t, 250+30*24*3600, first.EndTime)
				require.NoError(t, db.Model(&customer).Update("accounting_version", 1).Error)
				_, err = ActivateSubscriptionPurchase(db, customer.Id, next.ID, halfway+3)
				assert.ErrorIs(t, err, gorm.ErrRecordNotFound, "foreign orders cannot be activated by another user")
				delayed, err := CreateSubscriptionPurchaseOrder(db, SubscriptionPurchaseInput{UserID: buyer.Id, VersionID: version2.ID, Provider: PaymentMethodStripe, EventID: "renewal-delayed", ExpiresAt: halfway + 1000}, halfway+3)
				require.NoError(t, err)
				previousDB := DB
				DB = db
				_, invalidateErr := AdminInvalidateUserSubscription(first.Id)
				_, deleteErr := AdminDeleteUserSubscription(first.Id)
				resetErr := db.Transaction(func(tx *gorm.DB) error { return resetUserSubscriptionTx(tx, &first, &product, halfway+4, true) })
				DB = previousDB
				assert.ErrorIs(t, invalidateErr, ErrCreditOperationRequired, "legacy cancellation cannot rewrite a purchased contract")
				assert.ErrorIs(t, deleteErr, ErrCreditOperationRequired, "legacy delete cannot erase paid rights history")
				assert.ErrorIs(t, resetErr, ErrCreditOperationRequired, "legacy reset cannot refill versioned multi-window accounting")
				selfCancel := SubscriptionRightsCancellation{UserID: buyer.Id, SubscriptionID: first.Id, ActorID: buyer.Id, EventID: "self-cancel", Reason: "forged admin"}
				assert.ErrorIs(t, CancelSubscriptionRights(db, selfCancel, halfway+4), ErrUserQuotaPermission)
				cancel := SubscriptionRightsCancellation{UserID: buyer.Id, SubscriptionID: first.Id, ActorID: admin.Id, EventID: "cancel-renewal", Reason: "manual entitlement cancellation"}
				require.NoError(t, CancelSubscriptionRights(db, cancel, halfway+4))
				require.NoError(t, CancelSubscriptionRights(db, cancel, halfway+5))
				require.NoError(t, db.First(&first, first.Id).Error)
				require.NoError(t, db.First(&second, second.Id).Error)
				assert.Equal(t, "cancelled", first.Status)
				assert.Equal(t, "cancelled", second.Status)
				require.NoError(t, db.First(&currentBuyer, buyer.Id).Error)
				assert.Equal(t, "default", currentBuyer.Group, "manual cancellation removes group rights")
				replay, err = ActivateSubscriptionPurchase(db, buyer.Id, order.ID, halfway+6)
				require.NoError(t, err)
				assert.Equal(t, "cancelled", replay.Status)
				_, err = RecordSubscriptionPaymentFact(db, VerifiedSubscriptionPayment{OrderID: delayed.ID, Provider: delayed.Provider, EventID: "renewal-late-payment", ReferenceID: "renewal-late-transaction", BuyerID: buyer.Id, AmountMicros: common.GetPointer(delayed.PriceMicros), Currency: delayed.Currency, PaidAt: common.GetPointer(halfway + 3), PaidAtSource: "provider-paid-at", Succeeded: true, EvidenceDigest: strings.Repeat("c", 64)}, halfway+7)
				require.NoError(t, err)
				_, err = ActivateSubscriptionPurchase(db, buyer.Id, delayed.ID, halfway+8)
				assert.ErrorIs(t, err, ErrSubscriptionPurchaseUnavailable, "late notification cannot undo administrator cancellation")
				rights, err := GetUserSubscriptionRights(db, buyer.Id, halfway+8)
				require.NoError(t, err)
				assert.Empty(t, rights)

				assert.EqualValues(t, 100, first.AmountTotal)
				assert.EqualValues(t, 250+60*24*3600, second.EndTime, "cancellation preserves the contractual expiry for audit")
				var refundOperations int64
				require.NoError(t, db.Model(&CreditOperation{}).Where("user_id = ? AND kind = ?", buyer.Id, "grant").Count(&refundOperations).Error)
				assert.Zero(t, refundOperations, "cancelling rights never refunds purchase funds")
				newOrder, err := CreateSubscriptionPurchaseOrder(db, SubscriptionPurchaseInput{UserID: buyer.Id, VersionID: version2.ID, Provider: PaymentMethodStripe, EventID: "fresh-after-cancel", ExpiresAt: halfway + 2000}, halfway+9)
				require.NoError(t, err)
				cancel.EventID = "cancel-history-again"
				assert.ErrorIs(t, CancelSubscriptionRights(db, cancel, halfway+10), ErrSubscriptionPurchaseUnavailable, "a terminated historical chain cannot cancel newer purchases")
				require.NoError(t, db.First(&newOrder, newOrder.ID).Error)
				assert.False(t, newOrder.NeedsReview, "old cancellation must not fence the fresh order")

			})

		})
	}
}

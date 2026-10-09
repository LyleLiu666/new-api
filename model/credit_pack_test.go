package model

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	hosttypes "github.com/QuantumNous/new-api/types"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/schema"
)

func TestCreditPackDatabaseMatrix(t *testing.T) {
	for _, dialect := range []string{"sqlite", "mysql", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			var driver gorm.Dialector
			switch dialect {
			case "sqlite":
				driver = sqlite.Open(filepath.Join(t.TempDir(), "credits.db") + "?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)")
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
			db, err := gorm.Open(driver, &gorm.Config{NamingStrategy: schema.NamingStrategy{TablePrefix: "credit_test_"}})
			require.NoError(t, err)
			sqlDB, err := db.DB()
			require.NoError(t, err)
			sqlDB.SetMaxOpenConns(8)
			previousType := common.MainDatabaseType()
			common.SetMainDatabaseType(common.DatabaseType(dialect))
			t.Cleanup(func() {
				require.NoError(t, db.Migrator().DropTable(&Option{}, &CreditBillAdjustment{}, &CreditUsageEvidence{}, &SubscriptionWindowAllocation{}, &SubscriptionWindow{}, &UserSubscription{}, &CreditLogDelivery{}, &AuditLog{}, &CreditLogOutbox{}, &CreditRequestReservation{}, &CreditCashEvidence{}, &CreditReviewCase{}, &CreditRequest{}, &CreditLedgerEntry{}, &CreditAllocation{}, &CreditOperation{}, &CreditPack{}, &CreditAccount{}, &CreditSourcePolicy{}, &TopUp{}, &Redemption{}, &Checkin{}, &Log{}, &Token{}, &User{}))
				require.NoError(t, sqlDB.Close())
				common.SetMainDatabaseType(previousType)
			})
			require.NoError(t, db.AutoMigrate(&User{}, &Option{}))
			legacy := User{Username: "legacy", Quota: 73, Password: "unused", AffCode: "legacy"}
			require.NoError(t, db.Create(&legacy).Error)
			require.NoError(t, db.Migrator().DropColumn(&User{}, "AccountingVersion"))
			require.NoError(t, db.AutoMigrate(&User{}, &Token{}, &UserSubscription{}))
			require.NoError(t, MigrateCreditAccounting(db))
			recorder := &migrationSQLRecorder{}
			require.NoError(t, MigrateCreditAccounting(db.Session(&gorm.Session{Logger: recorder})))
			assert.Empty(t, recorder.schemaMutations(), "repeated migration must not mutate schema")
			var saved User
			require.NoError(t, db.First(&saved, legacy.Id).Error)
			assert.Equal(t, 73, saved.Quota, "additive migration does not convert or overwrite legacy money")
			assert.Zero(t, saved.AccountingVersion, "legacy schema upgrade must not enable credit accounting")
			var count int64
			require.NoError(t, db.Model(&CreditPack{}).Count(&count).Error)
			assert.Zero(t, count, "schema migration does not invent expiry or issuance events")
			require.NoError(t, db.AutoMigrate(&TopUp{}, &Redemption{}, &Checkin{}, &Log{}))
			oldOrder := TopUp{UserId: legacy.Id, Amount: 2, TradeNo: "legacy-policy-order", Status: common.TopUpStatusPending}
			oldCode := Redemption{UserId: legacy.Id, Key: "legacy-credit-policy-code", Quota: 12, Status: common.RedemptionCodeStatusEnabled}
			require.NoError(t, db.Create(&oldOrder).Error)
			require.NoError(t, db.Create(&oldCode).Error)
			for _, column := range []string{"CreditQuota", "CreditDurationSeconds", "CreditUseMask"} {
				require.NoError(t, db.Migrator().DropColumn(&TopUp{}, column))
			}
			for _, column := range []string{"CreditDurationSeconds", "CreditUseMask"} {
				require.NoError(t, db.Migrator().DropColumn(&Redemption{}, column))
			}
			require.NoError(t, db.AutoMigrate(&TopUp{}, &Redemption{}))
			require.NoError(t, db.First(&oldOrder, oldOrder.Id).Error)
			require.NoError(t, db.First(&oldCode, oldCode.Id).Error)
			assert.EqualValues(t, 2, oldOrder.Amount)
			assert.Equal(t, common.TopUpStatusPending, oldOrder.Status)
			assert.Zero(t, oldOrder.CreditQuota)
			assert.Zero(t, oldOrder.CreditDurationSeconds, "upgrade does not invent a historical package policy")
			assert.Equal(t, 12, oldCode.Quota)
			assert.Zero(t, oldCode.CreditDurationSeconds)
			recorder = &migrationSQLRecorder{}
			require.NoError(t, db.Session(&gorm.Session{Logger: recorder}).AutoMigrate(&TopUp{}, &Redemption{}))
			assert.Empty(t, recorder.schemaMutations())

			t.Run("FEFO_idempotency_and_atomic_failure", func(t *testing.T) {
				user := creditTestUser(t, db, "fefo")
				grant := CreditGrant{UserID: user.Id, SourceType: "topup", SourceID: "a", Amount: 30, StartsAt: 100, ExpiresAt: 200, UseMask: CreditUseAPI}
				a, err := GrantCreditPack(db, grant, 100)
				require.NoError(t, err)
				replayed, err := GrantCreditPack(db, grant, 150)
				require.NoError(t, err)
				assert.Equal(t, a, replayed)
				grant.Amount = 31
				_, err = GrantCreditPack(db, grant, 150)
				assert.ErrorIs(t, err, ErrCreditOperationConflict)
				grant.SourceID, grant.Amount, grant.ExpiresAt = "b", 100, 300
				b, err := GrantCreditPack(db, grant, 100)
				require.NoError(t, err)
				input := CreditReserve{UserID: user.Id, RequestID: "request", Amount: 50, Purpose: CreditUseAPI}
				reservation, err := ReserveCreditPacks(db, input, 100)
				require.NoError(t, err)
				require.Len(t, reservation.Allocations, 2)
				assert.Equal(t, a.ID, reservation.Allocations[0].PackID)
				assert.EqualValues(t, 30, reservation.Allocations[0].Amount)
				assert.Equal(t, b.ID, reservation.Allocations[1].PackID)
				assert.EqualValues(t, 20, reservation.Allocations[1].Amount)
				repeated, err := ReserveCreditPacks(db, input, 250)
				require.NoError(t, err)
				assert.Equal(t, reservation, repeated, "retry returns durable result even after original pack expiry")
				input.Amount = 49
				_, err = ReserveCreditPacks(db, input, 100)
				assert.ErrorIs(t, err, ErrCreditOperationConflict)
				input.RequestID, input.Amount = "insufficient", 81
				_, err = ReserveCreditPacks(db, input, 100)
				assert.ErrorIs(t, err, ErrCreditInsufficient)
				var pack CreditPack
				require.NoError(t, db.First(&pack, b.ID).Error)
				assert.EqualValues(t, 80, pack.Available)
				assert.EqualValues(t, 20, pack.Held)
				require.NoError(t, db.Callback().Create().Before("gorm:create").Register("credit_fail_allocation", func(tx *gorm.DB) {
					if tx.Statement.Schema != nil && tx.Statement.Schema.Name == "CreditAllocation" {
						tx.AddError(errors.New("allocation unavailable"))
					}
				}))
				input.RequestID, input.Amount = "write-failure", 10
				_, err = ReserveCreditPacks(db, input, 100)
				require.Error(t, err)
				require.NoError(t, db.Callback().Create().Remove("credit_fail_allocation"))
				require.NoError(t, db.First(&pack, b.ID).Error)
				assert.EqualValues(t, 80, pack.Available, "failed allocation rolls back pack update")
				_, err = ReserveCreditPacks(db, input, 100)
				require.NoError(t, err, "same operation retries after complete rollback")
			})

			t.Run("validity_purpose_and_stable_ties", func(t *testing.T) {
				user := creditTestUser(t, db, "validity")
				var packs []CreditPack
				for i, grant := range []CreditGrant{
					{Amount: 10, StartsAt: 101, ExpiresAt: 200, UseMask: CreditUseAPI},
					{Amount: 10, StartsAt: 1, ExpiresAt: 100, UseMask: CreditUseAPI},
					{Amount: 10, StartsAt: 1, ExpiresAt: 200, UseMask: CreditUseSubscription},
					{Amount: 10, StartsAt: 1, ExpiresAt: 200, UseMask: CreditUseAPI},
					{Amount: 10, StartsAt: 1, ExpiresAt: 200, UseMask: CreditUseAPI},
				} {
					grant.UserID, grant.SourceType, grant.SourceID = user.Id, "test", fmt.Sprint(i)
					pack, err := GrantCreditPack(db, grant, 90)
					require.NoError(t, err)
					packs = append(packs, pack)
				}
				result, err := ReserveCreditPacks(db, CreditReserve{UserID: user.Id, RequestID: "api", Amount: 15, Purpose: CreditUseAPI}, 100)
				require.NoError(t, err)
				require.Len(t, result.Allocations, 2)
				assert.Equal(t, packs[3].ID, result.Allocations[0].PackID)
				assert.Equal(t, packs[4].ID, result.Allocations[1].PackID)
				assert.EqualValues(t, 10, result.Allocations[0].Amount)
				assert.EqualValues(t, 5, result.Allocations[1].Amount)
				_, err = ReserveCreditPacks(db, CreditReserve{UserID: user.Id, RequestID: "boundary", Amount: 6, Purpose: CreditUseAPI}, 100)
				assert.ErrorIs(t, err, ErrCreditInsufficient)
				view, err := ListCreditPacks(db, user.Id, 100)
				require.NoError(t, err)
				require.Len(t, view, 5)
				assert.EqualValues(t, 10, view[0].Expired, "query exposes expiry without cleanup")
				assert.Zero(t, view[0].Available)
				other := creditTestUser(t, db, "other")
				_, err = ReserveCreditPacks(db, CreditReserve{UserID: other.Id, RequestID: "api", Amount: 1, Purpose: CreditUseAPI}, 100)
				assert.ErrorIs(t, err, ErrCreditInsufficient)
			})

			t.Run("concurrent_last_balance_and_replay", func(t *testing.T) {
				user := creditTestUser(t, db, "concurrent")
				pack, err := GrantCreditPack(db, CreditGrant{UserID: user.Id, SourceType: "test", SourceID: "only", Amount: 50, StartsAt: 1, ExpiresAt: 200, UseMask: CreditUseAPI}, 100)
				require.NoError(t, err)
				start := make(chan struct{})
				ready := make(chan struct{}, 2)
				release := make(chan struct{})
				require.NoError(t, db.Callback().Update().Before("gorm:update").Register("credit_account_barrier", func(tx *gorm.DB) {
					if tx.Statement.Schema != nil && tx.Statement.Schema.Name == "CreditAccount" {
						ready <- struct{}{}
						<-release
					}
				}))
				errorsCh := make(chan error, 2)
				var group sync.WaitGroup
				for i := range 2 {
					group.Go(func() {
						<-start
						_, err := ReserveCreditPacks(db, CreditReserve{UserID: user.Id, RequestID: fmt.Sprint(i), Amount: 40, Purpose: CreditUseAPI}, 100)
						errorsCh <- err
					})
				}
				close(start)
				<-ready
				<-ready
				assert.GreaterOrEqual(t, sqlDB.Stats().InUse, 2, "independent transactions have acquired separate connections")
				close(release)
				group.Wait()
				require.NoError(t, db.Callback().Update().Remove("credit_account_barrier"))
				close(errorsCh)
				var success, insufficient int
				for err := range errorsCh {
					if err == nil {
						success++
					} else if errors.Is(err, ErrCreditInsufficient) {
						insufficient++
					} else {
						require.NoError(t, err)
					}
				}
				assert.Equal(t, 1, success)
				assert.Equal(t, 1, insufficient)
				require.NoError(t, db.First(&pack, pack.ID).Error)
				assert.EqualValues(t, 10, pack.Available)
				assert.EqualValues(t, 40, pack.Held)
				assert.Equal(t, pack.Issued, pack.Available+pack.Held+pack.Spent+pack.Expired+pack.Revoked)
			})
			t.Run("concurrent_grant_and_exact_source_identity", func(t *testing.T) {
				user := creditTestUser(t, db, "grant-race")
				grant := CreditGrant{UserID: user.Id, SourceType: "test", SourceID: "Order-A", Amount: 50, StartsAt: 1, ExpiresAt: 200, UseMask: CreditUseAPI}
				start := make(chan struct{})
				results := make(chan CreditPack, 2)
				errorsCh := make(chan error, 2)
				var group sync.WaitGroup
				for range 2 {
					group.Go(func() {
						<-start
						pack, err := GrantCreditPack(db, grant, 100)
						results <- pack
						errorsCh <- err
					})
				}
				close(start)
				group.Wait()
				require.NoError(t, <-errorsCh)
				require.NoError(t, <-errorsCh)
				first, second := <-results, <-results
				assert.Equal(t, first, second)
				grant.SourceID = "order-a"
				third, err := GrantCreditPack(db, grant, 100)
				require.NoError(t, err)
				assert.NotEqual(t, first.ID, third.ID, "DB case-insensitive collation must not merge distinct source events")
			})

			t.Run("bounds_and_missing_user", func(t *testing.T) {
				user := creditTestUser(t, db, "bounds")
				valid := CreditGrant{UserID: user.Id, SourceType: "test", SourceID: "valid", Amount: common.MaxWalletQuota, StartsAt: 1, ExpiresAt: 200, UseMask: CreditUseAPI}
				for _, mutate := range []func(*CreditGrant){
					func(g *CreditGrant) { g.Amount = -1 }, func(g *CreditGrant) { g.Amount = 0 },
					func(g *CreditGrant) { g.Amount++ }, func(g *CreditGrant) { g.ExpiresAt = g.StartsAt },
					func(g *CreditGrant) { g.UseMask = 0 }, func(g *CreditGrant) { g.UseMask = 4 },
					func(g *CreditGrant) { g.SourceID = "" },
				} {
					invalid := valid
					mutate(&invalid)
					_, err := GrantCreditPack(db, invalid, 100)
					assert.ErrorIs(t, err, ErrCreditInvalid)
				}
				_, err := GrantCreditPack(db, valid, 100)
				require.NoError(t, err)
				valid.SourceID, valid.Amount = "overflow", 1
				_, err = GrantCreditPack(db, valid, 100)
				assert.ErrorIs(t, err, ErrWalletQuotaLimitExceeded)
				valid.UserID = 99999999
				_, err = GrantCreditPack(db, valid, 100)
				assert.ErrorIs(t, err, gorm.ErrRecordNotFound)
			})
			t.Run("expired_money_does_not_block_new_grant", func(t *testing.T) {
				user := creditTestUser(t, db, "expired-limit")
				grant := CreditGrant{UserID: user.Id, SourceType: "test", SourceID: "old", Amount: common.MaxWalletQuota, StartsAt: 1, ExpiresAt: 100, UseMask: CreditUseAPI}
				_, err := GrantCreditPack(db, grant, 90)
				require.NoError(t, err)
				grant.SourceID, grant.Amount, grant.ExpiresAt = "new", 1, 200
				_, err = GrantCreditPack(db, grant, 100)
				require.NoError(t, err, "logical expiry frees the outstanding-wallet limit without a cleanup job")
			})
			t.Run("request_growth_is_atomic_persistent_and_expiry_aware", func(t *testing.T) {
				user := creditTestUser(t, db, "request-growth")
				require.NoError(t, db.Model(&user).Update("accounting_version", 1).Error)
				key := Token{UserId: user.Id, Key: "request-growth-key", RemainQuota: 100, Status: common.TokenStatusEnabled, ExpiredTime: -1}
				require.NoError(t, db.Create(&key).Error)
				for i, amount := range []int64{30, 60} {
					_, err := GrantCreditPack(db, CreditGrant{UserID: user.Id, SourceType: "test", SourceID: fmt.Sprint(i), Amount: amount, StartsAt: 1, ExpiresAt: int64(101 + i*99), UseMask: CreditUseAPI}, 100)
					require.NoError(t, err)
				}
				input := CreditRequestInput{UserID: user.Id, RequestID: "growth", ModelName: "growth-model", Protocol: "image", PriceSnapshot: `{}`, TokenID: key.Id, Amount: 20}
				request, err := BeginCreditRequest(db, input, 100)
				require.NoError(t, err)
				require.NoError(t, db.Model(&key).Update("remain_quota", 5).Error)
				_, err = GrowCreditRequestReservation(db, user.Id, request.ID, 50, 102)
				assert.ErrorIs(t, err, ErrCreditInsufficient, "key failure rolls back every supplemental pack hold")
				require.NoError(t, db.First(&request, request.ID).Error)
				assert.EqualValues(t, 20, request.Reserved)
				packs, err := ListCreditPacks(db, user.Id, 102)
				require.NoError(t, err)
				assert.EqualValues(t, 60, packs[1].Available)
				require.NoError(t, db.Model(&key).Update("remain_quota", 80).Error)
				start := make(chan struct{})
				results := make(chan error, 2)
				var workers sync.WaitGroup
				for range 2 {
					workers.Go(func() {
						<-start
						_, err := GrowCreditRequestReservation(db, user.Id, request.ID, 50, 102)
						results <- err
					})
				}
				close(start)
				workers.Wait()
				for range 2 {
					require.NoError(t, <-results)
				}
				repeated, err := BeginCreditRequest(db, input, 103)
				require.NoError(t, err)
				assert.EqualValues(t, 50, repeated.Reserved, "initial admission replay returns the durable current reservation")
				_, err = GrowCreditRequestReservation(db, user.Id, request.ID, 81, 102)
				assert.ErrorIs(t, err, ErrCreditInsufficient, "expired unallocated money cannot fund an increase")
				settled, err := FinishCreditRequest(db, user.Id, request.ID, "settle", 35, 150)
				require.NoError(t, err)
				assert.EqualValues(t, 35, settled.Charged)
				assert.Zero(t, settled.Uncollected)
				packs, err = ListCreditPacks(db, user.Id, 150)
				require.NoError(t, err)
				assert.EqualValues(t, 20, packs[0].Spent, "original held funds remain valid across expiry")
				assert.EqualValues(t, 10, packs[0].Expired)
				assert.EqualValues(t, 15, packs[1].Spent)
				assert.EqualValues(t, 45, packs[1].Available)
				assert.Zero(t, packs[1].Held)
				require.NoError(t, db.First(&key, key.Id).Error)
				assert.Equal(t, 65, key.RemainQuota)
				assert.Equal(t, 35, key.UsedQuota)
				_, err = GrowCreditRequestReservation(db, user.Id, request.ID, 60, 150)
				assert.ErrorIs(t, err, ErrCreditOperationConflict, "closed request cannot acquire another hold")
			})
			t.Run("durable_settlement_release_and_terminal_conflicts", func(t *testing.T) {
				user := creditTestUser(t, db, "settlement")
				var ids []int64
				for i, amount := range []int64{30, 100} {
					pack, err := GrantCreditPack(db, CreditGrant{UserID: user.Id, SourceType: "test", SourceID: fmt.Sprint(i), Amount: amount, StartsAt: 1, ExpiresAt: int64(200 + i*100), UseMask: CreditUseAPI}, 100)
					require.NoError(t, err)
					ids = append(ids, pack.ID)
				}
				reserved, err := ReserveCreditPacks(db, CreditReserve{UserID: user.Id, RequestID: "settle", Amount: 50, Purpose: CreditUseAPI}, 100)
				require.NoError(t, err)
				input := CreditFinalize{UserID: user.Id, ReservationID: reserved.OperationID, Kind: "settle", Actual: 35}
				result, err := FinalizeCreditReservation(db, input, 150)
				require.NoError(t, err)
				assert.EqualValues(t, 35, result.Settled)
				assert.EqualValues(t, 15, result.Released)
				repeated, err := FinalizeCreditReservation(db.Session(&gorm.Session{}), input, 250)
				require.NoError(t, err)
				assert.Equal(t, result, repeated)
				input.Actual = 34
				_, err = FinalizeCreditReservation(db, input, 150)
				assert.ErrorIs(t, err, ErrCreditOperationConflict)
				input.Actual, input.Kind = 0, "release"
				_, err = FinalizeCreditReservation(db, input, 150)
				assert.ErrorIs(t, err, ErrCreditOperationConflict, "settlement and release share a single terminal operation")
				var a, b CreditPack
				require.NoError(t, db.First(&a, ids[0]).Error)
				require.NoError(t, db.First(&b, ids[1]).Error)
				assert.EqualValues(t, 30, a.Spent)
				assert.Zero(t, a.Held)
				assert.EqualValues(t, 5, b.Spent)
				assert.EqualValues(t, 95, b.Available)
				assert.Zero(t, b.Held)
				input.UserID = legacy.Id
				_, err = FinalizeCreditReservation(db, input, 150)
				require.Error(t, err, "another user cannot release this reservation")
			})
			t.Run("cross_expiry_and_rollback", func(t *testing.T) {
				user := creditTestUser(t, db, "cross-expiry")
				pack, err := GrantCreditPack(db, CreditGrant{UserID: user.Id, SourceType: "test", SourceID: "expiring", Amount: 30, StartsAt: 1, ExpiresAt: 101, UseMask: CreditUseAPI}, 100)
				require.NoError(t, err)
				reserved, err := ReserveCreditPacks(db, CreditReserve{UserID: user.Id, RequestID: "held", Amount: 30, Purpose: CreditUseAPI}, 100)
				require.NoError(t, err)
				input := CreditFinalize{UserID: user.Id, ReservationID: reserved.OperationID, Kind: "settle", Actual: 20}
				require.NoError(t, db.Callback().Create().Before("gorm:create").Register("credit_fail_ledger", func(tx *gorm.DB) {
					if tx.Statement.Schema != nil && tx.Statement.Schema.Name == "CreditLedgerEntry" {
						tx.AddError(errors.New("ledger unavailable"))
					}
				}))
				_, err = FinalizeCreditReservation(db, input, 102)
				require.Error(t, err)
				require.NoError(t, db.Callback().Create().Remove("credit_fail_ledger"))
				require.NoError(t, db.First(&pack, pack.ID).Error)
				assert.EqualValues(t, 30, pack.Held)
				assert.Zero(t, pack.Spent)
				result, err := FinalizeCreditReservation(db, input, 102)
				require.NoError(t, err)
				assert.EqualValues(t, 20, result.Settled)
				require.NoError(t, db.First(&pack, pack.ID).Error)
				assert.EqualValues(t, 20, pack.Spent)
				assert.EqualValues(t, 10, pack.Expired)
				assert.Zero(t, pack.Available)
				assert.Zero(t, pack.Held)
			})
			t.Run("execution_takeover_rejects_late_writer", func(t *testing.T) {
				user := creditTestUser(t, db, "execution")
				require.NoError(t, db.Model(&user).Update("accounting_version", 1).Error)
				pack, err := GrantCreditPack(db, CreditGrant{UserID: user.Id, SourceType: "test", SourceID: "execution", Amount: 100, StartsAt: 1, ExpiresAt: 500, UseMask: CreditUseAPI}, 100)
				require.NoError(t, err)
				token := Token{UserId: user.Id, Key: "execution-token", RemainQuota: 100}
				require.NoError(t, db.Create(&token).Error)
				request, err := BeginCreditRequest(db, CreditRequestInput{UserID: user.Id, RequestID: "execution", ModelName: "model", Protocol: "openai", PriceSnapshot: `{}`, TokenID: token.Id, Amount: 40}, 100)
				require.NoError(t, err)
				old, err := ClaimCreditExecution(db, user.Id, request.ID, "worker-A", 10, 100)
				require.NoError(t, err)
				require.NoError(t, MarkCreditRequestSubmitted(db, user.Id, request.ID, 101, old))
				_, err = ClaimCreditExecution(db, user.Id, request.ID, "worker-B", 10, 109)
				assert.ErrorIs(t, err, ErrCreditLeaseLost)
				_, err = GrowCreditRequestReservation(db, user.Id, request.ID, 50, 111, old)
				assert.ErrorIs(t, err, ErrCreditLeaseLost)
				start := make(chan struct{})
				leases := make(chan CreditExecution, 2)
				errors := make(chan error, 2)
				var workers sync.WaitGroup
				for _, owner := range []string{"worker-B", "worker-C"} {
					workers.Go(func() {
						<-start
						lease, err := ClaimCreditExecution(db.Session(&gorm.Session{}), user.Id, request.ID, owner, 10, 111)
						leases <- lease
						errors <- err
					})
				}
				close(start)
				workers.Wait()
				close(leases)
				close(errors)
				var winner CreditExecution
				for lease := range leases {
					if lease.Epoch != 0 {
						winner = lease
					}
				}
				var successes, losses int
				for err := range errors {
					if err == nil {
						successes++
					} else {
						assert.ErrorIs(t, err, ErrCreditLeaseLost)
						losses++
					}
				}
				assert.Equal(t, 1, successes)
				assert.Equal(t, 1, losses)
				assert.Greater(t, winner.Epoch, old.Epoch)
				winner.Clock = func() int64 { return 115 }
				require.NoError(t, RenewCreditExecution(db, winner, 10, 112))
				require.NoError(t, db.First(&request, request.ID).Error)
				assert.EqualValues(t, 125, request.LeaseUntil, "renewal uses time after acquiring the account lock")
				_, err = FinishCreditRequest(db, user.Id, request.ID, "settle", 25, 112, old)
				assert.ErrorIs(t, err, ErrCreditLeaseLost)
				_, err = FinishCreditRequest(db, user.Id, request.ID, "settle", 25, 112)
				assert.ErrorIs(t, err, ErrCreditLeaseLost, "unfenced entry cannot bypass a claimed request")
				_, err = FinishCreditRequest(db, user.Id, request.ID, "settle", 25, 112, winner)
				require.NoError(t, err)
				_, err = FinishCreditRequest(db, user.Id, request.ID, "settle", 25, 112, winner)
				require.NoError(t, err)
				require.NoError(t, db.First(&pack, pack.ID).Error)
				assert.EqualValues(t, 25, pack.Spent)
				assert.EqualValues(t, 75, pack.Available)
				assert.Zero(t, pack.Held)
				require.NoError(t, db.First(&token, token.Id).Error)
				assert.Equal(t, 75, token.RemainQuota)
				assert.Equal(t, 25, token.UsedQuota)
			})
			t.Run("lease_expires_between_intent_and_commit", func(t *testing.T) {
				user := creditTestUser(t, db, "lease-deadline")
				require.NoError(t, db.Model(&user).Update("accounting_version", 1).Error)
				pack, err := GrantCreditPack(db, CreditGrant{UserID: user.Id, SourceType: "test", SourceID: "lease-deadline", Amount: 100, StartsAt: 1, ExpiresAt: 500, UseMask: CreditUseAPI}, 100)
				require.NoError(t, err)
				request, err := BeginCreditRequest(db, CreditRequestInput{UserID: user.Id, RequestID: "lease-deadline", ModelName: "model", Protocol: "openai", PriceSnapshot: `{}`, Playground: true, Amount: 20}, 100)
				require.NoError(t, err)
				var now atomic.Int64
				now.Store(100)
				lease, err := ClaimCreditExecution(db, user.Id, request.ID, "old-deadline", 10, 100, now.Load)
				require.NoError(t, err)
				require.NoError(t, db.Callback().Update().After("gorm:update").Register("credit:intent-deadline", func(tx *gorm.DB) {
					if tx.Statement.Schema != nil && tx.Statement.Schema.Name == "CreditRequest" {
						now.Store(111)
					}
				}))
				_, err = FinishCreditRequest(db, user.Id, request.ID, "settle", 15, 100, lease)
				assert.ErrorIs(t, err, ErrCreditLeaseLost)
				require.NoError(t, db.Callback().Update().Remove("credit:intent-deadline"))
				require.NoError(t, db.First(&request, request.ID).Error)
				assert.Equal(t, "pending", request.State)
				require.NoError(t, db.First(&pack, pack.ID).Error)
				assert.EqualValues(t, 20, pack.Held)
				assert.Zero(t, pack.Spent)
				fresh, err := ClaimCreditExecution(db, user.Id, request.ID, "fresh-deadline", 10, 111, now.Load)
				require.NoError(t, err)
				_, err = FinishCreditRequest(db, user.Id, request.ID, "settle", 15, 111, fresh)
				require.NoError(t, err)
			})
			t.Run("durable_recovery_keeps_unknown_hold", func(t *testing.T) {
				user := creditTestUser(t, db, "recovery")
				require.NoError(t, db.Model(&user).Update("accounting_version", 1).Error)
				pack, err := GrantCreditPack(db, CreditGrant{UserID: user.Id, SourceType: "test", SourceID: "recovery", Amount: 100, StartsAt: 1, ExpiresAt: 500, UseMask: CreditUseAPI}, 100)
				require.NoError(t, err)
				input := CreditRequestInput{UserID: user.Id, RequestID: "unsent-recovery", ModelName: "model", Protocol: "openai", PriceSnapshot: `{}`, Playground: true, Amount: 20}
				unsent, err := BeginCreditRequest(db, input, 100)
				require.NoError(t, err)
				_, err = ClaimCreditExecution(db, user.Id, unsent.ID, "lost-unsent", 10, 100)
				require.NoError(t, err)
				input.RequestID = "submitted-recovery"
				submitted, err := BeginCreditRequest(db, input, 100)
				require.NoError(t, err)
				lease, err := ClaimCreditExecution(db, user.Id, submitted.ID, "lost-submitted", 10, 100)
				require.NoError(t, err)
				require.NoError(t, MarkCreditRequestSubmitted(db, user.Id, submitted.ID, 100, lease))
				results, _, err := RecoverCreditRequests(db.Session(&gorm.Session{}), "recovery-worker", 0, 100, 111)
				require.NoError(t, err)
				assert.NotEmpty(t, results)
				require.NoError(t, db.First(&unsent, unsent.ID).Error)
				require.NoError(t, db.First(&submitted, submitted.ID).Error)
				assert.Equal(t, "released", unsent.State)
				assert.Equal(t, "review", submitted.State)
				require.NoError(t, db.First(&pack, pack.ID).Error)
				assert.EqualValues(t, 20, pack.Held)
				assert.EqualValues(t, 80, pack.Available)
				assert.Zero(t, pack.Spent)
				differences, err := ReconcileCreditAccount(db, user.Id)
				require.NoError(t, err)
				assert.Empty(t, differences)
				require.NoError(t, db.Model(&pack).Update("spent", 1).Error)
				differences, err = ReconcileCreditAccount(db, user.Id)
				require.NoError(t, err)
				assert.NotEmpty(t, differences, "tampering is reported, never silently repaired")
				assert.Contains(t, differences, CreditAccountDifference{Object: "pack", ID: pack.ID, Field: "conservation", Expected: 100, Actual: 101}, "report the complete stored total rather than stopping at the first invalid component")
				require.NoError(t, db.First(&pack, pack.ID).Error)
				assert.EqualValues(t, 1, pack.Spent)
				require.NoError(t, db.Model(&pack).Update("spent", 0).Error)
			})
			t.Run("failed_recovery_is_visible_and_can_resume", func(t *testing.T) {
				t.Setenv("CREDIT_RECOVERY_MAX_ATTEMPTS", "1")
				user := creditTestUser(t, db, "blocked-recovery")
				require.NoError(t, db.Model(&user).Update("accounting_version", 1).Error)
				_, err := GrantCreditPack(db, CreditGrant{UserID: user.Id, SourceType: "test", SourceID: "blocked", Amount: 100, StartsAt: 1, ExpiresAt: 500, UseMask: CreditUseAPI}, 100)
				require.NoError(t, err)
				token := Token{UserId: user.Id, Key: "blocked-token", RemainQuota: 100}
				require.NoError(t, db.Create(&token).Error)
				request, err := BeginCreditRequest(db, CreditRequestInput{UserID: user.Id, RequestID: "blocked", ModelName: "model", Protocol: "openai", PriceSnapshot: `{}`, TokenID: token.Id, Amount: 20}, 100)
				require.NoError(t, err)
				require.NoError(t, db.Callback().Update().Before("gorm:update").Register("credit:blocked-financial-write", func(tx *gorm.DB) {
					if tx.Statement.Schema != nil && tx.Statement.Schema.Name == "Token" {
						tx.AddError(errors.New("token write unavailable"))
					}
				}))
				_, err = FinishCreditRequest(db, user.Id, request.ID, "settle", 15, 100)
				require.Error(t, err)
				results, _, err := RecoverCreditRequests(db, "blocked-worker", request.ID-1, 1, 101)
				require.NoError(t, err)
				require.Len(t, results, 1)
				assert.Contains(t, results[0].Error, "unavailable")
				require.NoError(t, db.Callback().Update().Remove("credit:blocked-financial-write"))
				require.NoError(t, db.First(&request, request.ID).Error)
				assert.Positive(t, request.RecoveryBlockedAt)
				assert.Equal(t, "pending", request.State)
				actor := creditTestUser(t, db, "recovery-admin")
				require.NoError(t, db.Model(&actor).Update("role", common.RoleAdminUser).Error)
				resume := CreditRecoveryResume{UserID: user.Id, RequestID: request.ID, ActorID: actor.Id, EventID: "resume-blocked", Reason: "storage repaired"}
				require.NoError(t, ResumeCreditRecovery(db, resume, 102))
				require.NoError(t, ResumeCreditRecovery(db, resume, 102))
				_, _, err = RecoverCreditRequests(db, "resumed-worker", request.ID-1, 1, 102)
				require.NoError(t, err)
				require.NoError(t, db.First(&request, request.ID).Error)
				assert.Equal(t, "settled", request.State)
				require.NoError(t, ResumeCreditRecovery(db, resume, 103))
				require.NoError(t, db.First(&token, token.Id).Error)
				assert.Equal(t, 85, token.RemainQuota)
				assert.Equal(t, 15, token.UsedQuota)
			})
			t.Run("process_exit_recovers_committed_intent", func(t *testing.T) {
				user := creditTestUser(t, db, "process-exit")
				require.NoError(t, db.Model(&user).Update("accounting_version", 1).Error)
				pack, err := GrantCreditPack(db, CreditGrant{UserID: user.Id, SourceType: "test", SourceID: "process", Amount: 100, StartsAt: 1, ExpiresAt: 500, UseMask: CreditUseAPI}, 100)
				require.NoError(t, err)
				token := Token{UserId: user.Id, Key: "process-token", RemainQuota: 100}
				require.NoError(t, db.Create(&token).Error)
				var dsn string
				if dialect == "sqlite" {
					dsn = driver.(*sqlite.Dialector).DSN
				}
				if dialect == "mysql" {
					dsn = os.Getenv("TEST_MYSQL_DSN")
				}
				if dialect == "postgres" {
					dsn = os.Getenv("TEST_POSTGRES_DSN")
				}
				child := exec.Command(os.Args[0], "-test.run=^TestCreditRecoveryProcessCheckpoint$")
				child.Env = append(os.Environ(), "NEW_API_CREDIT_CHECKPOINT_DB="+dialect, "NEW_API_CREDIT_CHECKPOINT_DSN="+dsn, "NEW_API_CREDIT_CHECKPOINT_USER="+strconv.Itoa(user.Id), "NEW_API_CREDIT_CHECKPOINT_TOKEN="+strconv.Itoa(token.Id))
				output, err := child.CombinedOutput()
				var exited *exec.ExitError
				require.ErrorAs(t, err, &exited, string(output))
				require.Equal(t, 23, exited.ExitCode(), string(output))
				var request CreditRequest
				require.NoError(t, db.Where("user_id = ?", user.Id).First(&request).Error)
				assert.Equal(t, "pending", request.State)
				assert.EqualValues(t, 35, request.Actual)
				require.NoError(t, db.First(&pack, pack.ID).Error)
				assert.EqualValues(t, 40, pack.Held)
				assert.Zero(t, pack.Spent)
				_, _, err = RecoverCreditRequests(db.Session(&gorm.Session{}), "new-process", request.ID-1, 1, 111)
				require.NoError(t, err)
				require.NoError(t, db.First(&request, request.ID).Error)
				assert.Equal(t, "settled", request.State)
				require.NoError(t, db.First(&pack, pack.ID).Error)
				assert.EqualValues(t, 35, pack.Spent)
				assert.EqualValues(t, 65, pack.Available)
				assert.Zero(t, pack.Held)
				require.NoError(t, db.First(&token, token.Id).Error)
				assert.Equal(t, 65, token.RemainQuota)
				assert.Equal(t, 35, token.UsedQuota)
			})
			t.Run("killed_process_preserves_transaction_boundaries", func(t *testing.T) {
				for _, stage := range []string{"reserve_transaction", "reserved", "submitted", "financial_transaction", "settled"} {
					t.Run(stage, func(t *testing.T) {
						user := creditTestUser(t, db, "kill-"+stage)
						require.NoError(t, db.Model(&user).Update("accounting_version", 1).Error)
						pack, err := GrantCreditPack(db, CreditGrant{UserID: user.Id, SourceType: "test", SourceID: "kill-" + stage, Amount: 100, StartsAt: 1, ExpiresAt: 500, UseMask: CreditUseAPI}, 100)
						require.NoError(t, err)
						token := Token{UserId: user.Id, Key: "kill-" + stage, RemainQuota: 100}
						require.NoError(t, db.Create(&token).Error)
						dsn := os.Getenv("TEST_MYSQL_DSN")
						if dialect == "sqlite" {
							dsn = driver.(*sqlite.Dialector).DSN
						}
						if dialect == "postgres" {
							dsn = os.Getenv("TEST_POSTGRES_DSN")
						}
						ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
						defer cancel()
						child := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestCreditRecoveryProcessCheckpoint$")
						child.Env = append(os.Environ(), "NEW_API_CREDIT_CHECKPOINT_DB="+dialect, "NEW_API_CREDIT_CHECKPOINT_DSN="+dsn, "NEW_API_CREDIT_CHECKPOINT_USER="+strconv.Itoa(user.Id), "NEW_API_CREDIT_CHECKPOINT_TOKEN="+strconv.Itoa(token.Id), "NEW_API_CREDIT_CHECKPOINT_STAGE="+stage)
						stdout, err := child.StdoutPipe()
						require.NoError(t, err)
						stdin, err := child.StdinPipe()
						require.NoError(t, err)
						defer stdin.Close()
						var stderr bytes.Buffer
						child.Stderr = &stderr
						require.NoError(t, child.Start())
						ready := make(chan bool, 1)
						go func() {
							scanner := bufio.NewScanner(stdout)
							for scanner.Scan() {
								if scanner.Text() == "credit-checkpoint:"+stage {
									ready <- true
									return
								}
							}
							ready <- false
						}()
						reached := <-ready
						if !reached {
							_ = child.Wait()
							t.Fatalf("checkpoint was not reached: %s", stderr.String())
						}
						require.NoError(t, child.Process.Kill())
						var exited *exec.ExitError
						require.ErrorAs(t, child.Wait(), &exited)
						require.Equal(t, -1, exited.ExitCode(), "the parent killed the actual child, not a simulated error")
						var rows []CreditRequest
						require.NoError(t, db.Where("user_id = ?", user.Id).Find(&rows).Error)
						require.NoError(t, db.First(&pack, pack.ID).Error)
						if stage == "reserve_transaction" {
							require.Empty(t, rows)
							assert.EqualValues(t, 100, pack.Available)
							assert.Zero(t, pack.Held)
							require.NoError(t, db.First(&token, token.Id).Error)
							assert.Equal(t, 100, token.RemainQuota)
							assert.Zero(t, token.UsedQuota)
							return
						}
						require.Len(t, rows, 1)
						request := rows[0]
						if stage != "settled" {
							assert.EqualValues(t, 40, pack.Held)
							assert.Zero(t, pack.Spent)
							require.NoError(t, db.First(&token, token.Id).Error)
							assert.Equal(t, 60, token.RemainQuota)
							assert.Equal(t, 40, token.UsedQuota)
						}
						for _, owner := range []string{"replacement-a", "replacement-b"} {
							_, _, err = RecoverCreditRequests(db, owner, request.ID-1, 1, 111)
							require.NoError(t, err)
						}
						require.NoError(t, db.First(&request, request.ID).Error)
						require.NoError(t, db.First(&pack, pack.ID).Error)
						require.NoError(t, db.First(&token, token.Id).Error)
						switch stage {
						case "reserved":
							assert.Equal(t, "released", request.State)
							assert.EqualValues(t, 100, pack.Available)
							assert.Zero(t, pack.Held)
							assert.Zero(t, pack.Spent)
							assert.Equal(t, 100, token.RemainQuota)
							assert.Zero(t, token.UsedQuota)
						case "submitted":
							assert.Equal(t, "review", request.State)
							assert.EqualValues(t, 60, pack.Available)
							assert.EqualValues(t, 40, pack.Held)
							assert.Zero(t, pack.Spent)
							assert.Equal(t, 60, token.RemainQuota)
							assert.Equal(t, 40, token.UsedQuota)
						default:
							assert.Equal(t, "settled", request.State)
							assert.EqualValues(t, 65, pack.Available)
							assert.Zero(t, pack.Held)
							assert.EqualValues(t, 35, pack.Spent)
							assert.Equal(t, 65, token.RemainQuota)
							assert.Equal(t, 35, token.UsedQuota)
							var count int64
							require.NoError(t, db.Model(&CreditLogOutbox{}).Where("request_id = ?", request.ID).Count(&count).Error)
							assert.EqualValues(t, 1, count)
						}
						differences, err := ReconcileCreditAccount(db, user.Id)
						require.NoError(t, err)
						assert.Empty(t, differences)
					})
				}
			})
			t.Run("two_process_takeover_rejects_original_writer", func(t *testing.T) {
				user := creditTestUser(t, db, "two-process")
				require.NoError(t, db.Model(&user).Update("accounting_version", 1).Error)
				pack, err := GrantCreditPack(db, CreditGrant{UserID: user.Id, SourceType: "test", SourceID: "two-process", Amount: 100, StartsAt: 1, ExpiresAt: 500, UseMask: CreditUseAPI}, 100)
				require.NoError(t, err)
				key := Token{UserId: user.Id, Key: "two-process", RemainQuota: 100}
				require.NoError(t, db.Create(&key).Error)
				request, err := BeginCreditRequest(db, CreditRequestInput{UserID: user.Id, RequestID: "two-process", ModelName: "model", Protocol: "openai", PriceSnapshot: `{}`, TokenID: key.Id, Amount: 40}, 100)
				require.NoError(t, err)
				old, err := ClaimCreditExecution(db, user.Id, request.ID, "original-process", 10, 100)
				require.NoError(t, err)
				require.NoError(t, MarkCreditRequestSubmitted(db, user.Id, request.ID, 100, old))
				dsn := os.Getenv("TEST_MYSQL_DSN")
				if dialect == "sqlite" {
					dsn = driver.(*sqlite.Dialector).DSN
				} else if dialect == "postgres" {
					dsn = os.Getenv("TEST_POSTGRES_DSN")
				}
				ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
				defer cancel()
				type competitor struct {
					command *exec.Cmd
					input   io.WriteCloser
					events  chan string
					stderr  bytes.Buffer
				}
				peers := make([]*competitor, 0, 2)
				for _, owner := range []string{"peer-a", "peer-b"} {
					peer := &competitor{command: exec.CommandContext(ctx, os.Args[0], "-test.run=^TestCreditRecoveryProcessCheckpoint$"), events: make(chan string, 3)}
					peer.command.Env = append(os.Environ(), "NEW_API_CREDIT_CHECKPOINT_DB="+dialect, "NEW_API_CREDIT_CHECKPOINT_DSN="+dsn, "NEW_API_CREDIT_CHECKPOINT_USER="+strconv.Itoa(user.Id), "NEW_API_CREDIT_CHECKPOINT_TOKEN="+strconv.Itoa(key.Id), "NEW_API_CREDIT_CHECKPOINT_STAGE=competing_claim", "NEW_API_CREDIT_CHECKPOINT_REQUEST="+strconv.FormatInt(request.ID, 10), "NEW_API_CREDIT_CHECKPOINT_OWNER="+owner)
					stdout, err := peer.command.StdoutPipe()
					require.NoError(t, err)
					peer.input, err = peer.command.StdinPipe()
					require.NoError(t, err)
					defer peer.input.Close()
					peer.command.Stderr = &peer.stderr
					require.NoError(t, peer.command.Start())
					go func() {
						scanner := bufio.NewScanner(stdout)
						for scanner.Scan() {
							if strings.HasPrefix(scanner.Text(), "credit-claim:") {
								peer.events <- scanner.Text()
								if scanner.Text() != "credit-claim:ready" {
									return
								}
							}
						}
						peer.events <- "credit-claim:failed"
					}()
					peers = append(peers, peer)
				}
				for _, peer := range peers {
					require.Equal(t, "credit-claim:ready", <-peer.events)
				}
				for _, peer := range peers {
					_, err := peer.input.Write([]byte{1})
					require.NoError(t, err)
				}
				var winner *competitor
				losses := 0
				for _, peer := range peers {
					switch event := <-peer.events; event {
					case "credit-claim:won":
						require.Nil(t, winner, "only one process acquires the expired lease")
						winner = peer
					case "credit-claim:lost":
						losses++
					default:
						t.Fatalf("unexpected child event: %s", event)
					}
				}
				require.NotNil(t, winner)
				assert.Equal(t, 1, losses)
				require.NoError(t, db.Model(&user).Update("status", common.UserStatusDisabled).Error)
				_, err = BeginCreditRequest(db, CreditRequestInput{UserID: user.Id, RequestID: "paused-new-request", ModelName: "model", Protocol: "openai", PriceSnapshot: `{}`, TokenID: key.Id, Amount: 20}, 112)
				assert.ErrorIs(t, err, ErrCreditOperationRequired, "stop new consumption without discarding existing reservations")
				_, err = FinishCreditRequest(db, user.Id, request.ID, "settle", 35, 112, old)
				assert.ErrorIs(t, err, ErrCreditLeaseLost)
				_, err = winner.input.Write([]byte{1})
				require.NoError(t, err)
				for _, peer := range peers {
					require.NoError(t, peer.command.Wait(), peer.stderr.String())
				}
				require.NoError(t, db.First(&request, request.ID).Error)
				assert.Equal(t, "settled", request.State)
				assert.EqualValues(t, old.Epoch+1, request.LeaseEpoch)
				require.NoError(t, db.First(&pack, pack.ID).Error)
				assert.EqualValues(t, 65, pack.Available)
				assert.EqualValues(t, 35, pack.Spent)
				assert.Zero(t, pack.Held)
				require.NoError(t, db.First(&key, key.Id).Error)
				assert.Equal(t, 65, key.RemainQuota)
				assert.Equal(t, 35, key.UsedQuota)
				require.NoError(t, db.Model(&user).Update("status", common.UserStatusEnabled).Error)
				resumed, err := BeginCreditRequest(db, CreditRequestInput{UserID: user.Id, RequestID: "resumed-new-request", ModelName: "model", Protocol: "openai", PriceSnapshot: `{}`, TokenID: key.Id, Amount: 20}, 113)
				require.NoError(t, err)
				_, err = FinishCreditRequest(db, user.Id, resumed.ID, "settle", 20, 113)
				require.NoError(t, err)
				require.NoError(t, db.First(&pack, pack.ID).Error)
				assert.EqualValues(t, 45, pack.Available)
				assert.EqualValues(t, 55, pack.Spent)
				differences, err := ReconcileCreditAccount(db, user.Id)
				require.NoError(t, err)
				assert.Empty(t, differences)
			})
			t.Run("shared_log_startup_delivers_once", func(t *testing.T) {
				previousDB, previousLogs, previousMaster := DB, LOG_DB, common.IsMasterNode
				previousLogType := common.LogDatabaseType()
				t.Cleanup(func() {
					DB, LOG_DB, common.IsMasterNode = previousDB, previousLogs, previousMaster
					common.SetLogDatabaseType(previousLogType)
					initCol()
				})
				t.Setenv("LOG_SQL_DSN", "")
				DB, common.IsMasterNode = db, true
				require.NoError(t, InitLogDB())
				require.NoError(t, InitLogDB(), "repeated shared-log startup is safe")
				user := creditTestUser(t, db, "shared-logs")
				require.NoError(t, db.Model(&user).Update("accounting_version", 1).Error)
				_, err := GrantCreditPack(db, CreditGrant{UserID: user.Id, SourceType: "test", SourceID: "shared-logs", Amount: 100, StartsAt: 1, ExpiresAt: 500, UseMask: CreditUseAPI}, 100)
				require.NoError(t, err)
				request, err := BeginCreditRequest(db, CreditRequestInput{UserID: user.Id, RequestID: "shared-logs", ModelName: "model", Protocol: "openai", PriceSnapshot: `{}`, Playground: true, Amount: 40}, 100)
				require.NoError(t, err)
				_, err = FinishCreditRequest(db, user.Id, request.ID, "settle", 25, 100)
				require.NoError(t, err)
				var pending CreditLogOutbox
				require.NoError(t, db.Where("request_id = ?", request.ID).First(&pending).Error)
				require.NoError(t, DeliverCreditLog(db, LOG_DB, pending.ID, 101))
				require.NoError(t, DeliverCreditLog(db, LOG_DB, pending.ID, 101))
				var count int64
				require.NoError(t, LOG_DB.Model(&Log{}).Where("user_id = ?", user.Id).Count(&count).Error)
				assert.EqualValues(t, 1, count)
			})
			t.Run("bill_adjustment_preserves_original_fefo_and_never_collects_again", func(t *testing.T) {
				user := creditTestUser(t, db, "bill-adjustment")
				require.NoError(t, db.Model(&user).Update("accounting_version", 1).Error)
				actor := creditTestUser(t, db, "bill-adjustment-admin")
				require.NoError(t, db.Model(&actor).Updates(map[string]any{"role": common.RoleRootUser, "status": common.UserStatusEnabled}).Error)
				key := Token{UserId: user.Id, Key: "bill-adjustment", Status: common.TokenStatusEnabled, RemainQuota: 200, ExpiredTime: -1}
				require.NoError(t, db.Create(&key).Error)
				first, err := GrantCreditPack(db, CreditGrant{UserID: user.Id, SourceType: "test", SourceID: "adjustment-first", Amount: 40, StartsAt: 1, ExpiresAt: 140, UseMask: CreditUseAPI}, 100)
				require.NoError(t, err)
				second, err := GrantCreditPack(db, CreditGrant{UserID: user.Id, SourceType: "test", SourceID: "adjustment-second", Amount: 100, StartsAt: 1, ExpiresAt: 500, UseMask: CreditUseAPI}, 100)
				require.NoError(t, err)
				request, err := BeginCreditRequest(db, CreditRequestInput{UserID: user.Id, RequestID: "adjustment", ModelName: "model", Protocol: "openai", PriceSnapshot: `{"frozen_price":1}`, TokenID: key.Id, Amount: 70}, 100)
				require.NoError(t, err)
				require.NoError(t, MarkCreditRequestSubmitted(db, user.Id, request.ID, 100))
				originalQuantity := float64(90)
				originalEvidence, err := RecordCreditUsageEvidence(db, CreditEvidenceInput{UserID: user.Id, RequestID: request.ID, EventID: "original-receipt", Attempt: 1, Stage: "settlement", Cumulative: true, Version: "verified-fixture-v1", Facts: []hosttypes.UsageFact{{Field: "prompt_tokens", Unit: "token", Quantity: &originalQuantity, Source: "upstream"}}, Consume: &CreditConsumeSnapshot{ReferenceQuota: 90, PromptTokens: 90, Other: "{}"}}, 110)
				require.NoError(t, err)
				request, err = FinishCreditRequest(db, user.Id, request.ID, "settle", 90, 110)
				require.NoError(t, err)
				original := request
				quantity := float64(60)
				input := CreditBillAdjustmentInput{UserID: user.Id, RequestID: request.ID, ActorID: actor.Id, EventID: "adjustment-1", ReferenceQuota: 60, EvidenceVersion: "verified-fixture-v1", Facts: []hosttypes.UsageFact{{Field: "prompt_tokens", Unit: "token", Quantity: &quantity, Source: "upstream"}}, Reason: "verified corrected receipt"}
				adjustment, err := AdjustCreditBill(db, input, 150)
				require.NoError(t, err)
				assert.EqualValues(t, 1, adjustment.Revision)
				assert.EqualValues(t, 60, adjustment.Charged)
				assert.EqualValues(t, 30, adjustment.Refunded)
				require.NoError(t, db.First(&first, first.ID).Error)
				require.NoError(t, db.First(&second, second.ID).Error)
				assert.EqualValues(t, 40, first.Spent, "earliest FEFO consumption remains consumed")
				assert.EqualValues(t, 20, second.Spent)
				assert.EqualValues(t, 80, second.Available, "later allocations, including settlement supplementation, are refunded first")
				input.EventID, input.ExpectedRevision, input.ReferenceQuota = "adjustment-2", 1, 20
				quantity = 20
				adjustment, err = AdjustCreditBill(db, input, 150)
				require.NoError(t, err)
				assert.EqualValues(t, 20, adjustment.Charged)
				require.NoError(t, db.First(&first, first.ID).Error)
				require.NoError(t, db.First(&second, second.ID).Error)
				assert.EqualValues(t, 20, first.Spent)
				assert.EqualValues(t, 20, first.Expired, "refund cannot renew expired credits")
				assert.Zero(t, first.Available)
				assert.EqualValues(t, 100, second.Available)
				input.EventID, input.ExpectedRevision, input.ReferenceQuota = "adjustment-3", 2, 80
				quantity = 80
				adjustment, err = AdjustCreditBill(db, input, 160)
				require.NoError(t, err)
				assert.EqualValues(t, 20, adjustment.Charged)
				assert.EqualValues(t, 60, adjustment.Uncollected)
				assert.Zero(t, adjustment.Refunded)
				replay, err := AdjustCreditBill(db, input, 161)
				require.NoError(t, err)
				assert.Equal(t, adjustment.ID, replay.ID)
				changed := input
				changed.ReferenceQuota = 79
				_, err = AdjustCreditBill(db, changed, 161)
				assert.ErrorIs(t, err, ErrCreditOperationConflict)
				changed = input
				changed.EventID, changed.ExpectedRevision = "stale-adjustment", 1
				_, err = AdjustCreditBill(db, changed, 161)
				assert.ErrorIs(t, err, ErrCreditOperationConflict)
				changed = input
				changed.EventID, changed.ExpectedRevision, changed.ActorID = "unauthorized-adjustment", 3, user.Id
				_, err = AdjustCreditBill(db, changed, 161)
				assert.ErrorIs(t, err, ErrUserQuotaPermission)
				require.NoError(t, db.First(&key, key.Id).Error)
				assert.Equal(t, 180, key.RemainQuota)
				assert.Equal(t, 20, key.UsedQuota)
				beforeFirst, beforeSecond, beforeKey := first, second, key
				fault := input
				fault.EventID, fault.ExpectedRevision, fault.ReferenceQuota = "write-fault", 3, 10
				faultQuantity := float64(10)
				fault.Facts = []hosttypes.UsageFact{{Field: "prompt_tokens", Unit: "token", Quantity: &faultQuantity, Source: "upstream"}}
				require.NoError(t, db.Callback().Create().Before("gorm:create").Register("test:bill-adjustment-store", func(tx *gorm.DB) {
					if _, ok := tx.Statement.Dest.(*CreditBillAdjustment); ok {
						tx.AddError(fmt.Errorf("adjustment store unavailable"))
					}
				}))
				_, err = AdjustCreditBill(db, fault, 162)
				require.Error(t, err)
				require.NoError(t, db.Callback().Create().Remove("test:bill-adjustment-store"))
				require.NoError(t, db.First(&first, first.ID).Error)
				require.NoError(t, db.First(&second, second.ID).Error)
				require.NoError(t, db.First(&key, key.Id).Error)
				assert.Equal(t, beforeFirst, first)
				assert.Equal(t, beforeSecond, second)
				assert.Equal(t, beforeKey, key)
				var versions, receipts int64
				require.NoError(t, db.Model(&CreditBillAdjustment{}).Where("request_id = ?", request.ID).Count(&versions).Error)
				require.NoError(t, db.Model(&CreditUsageEvidence{}).Where("request_id = ?", request.ID).Count(&receipts).Error)
				assert.EqualValues(t, 3, versions)
				assert.EqualValues(t, 4, receipts, "failed financial revision cannot leave a corrective receipt")
				adjustment, err = AdjustCreditBill(db, fault, 162)
				require.NoError(t, err)
				assert.EqualValues(t, 4, adjustment.Revision)
				assert.EqualValues(t, 10, adjustment.Charged)
				ready, start := make(chan struct{}, 2), make(chan struct{})
				results := make(chan error, 2)
				for _, reference := range []int64{4, 6} {
					concurrent := fault
					concurrent.EventID, concurrent.ExpectedRevision, concurrent.ReferenceQuota = fmt.Sprintf("concurrent-%d", reference), 4, reference
					value := float64(reference)
					concurrent.Facts = []hosttypes.UsageFact{{Field: "prompt_tokens", Unit: "token", Quantity: &value, Source: "upstream"}}
					go func() {
						ready <- struct{}{}
						<-start
						_, err := AdjustCreditBill(db, concurrent, 163)
						results <- err
					}()
				}
				<-ready
				<-ready
				close(start)
				successes, conflicts := 0, 0
				for range 2 {
					err := <-results
					if err == nil {
						successes++
					} else {
						assert.ErrorIs(t, err, ErrCreditOperationConflict)
						conflicts++
					}
				}
				assert.Equal(t, 1, successes)
				assert.Equal(t, 1, conflicts)
				var latest CreditBillAdjustment
				require.NoError(t, db.Where("request_id = ?", request.ID).Order("revision desc").First(&latest).Error)
				adjustment = latest
				assert.EqualValues(t, 5, adjustment.Revision)
				var preserved CreditUsageEvidence
				require.NoError(t, db.First(&preserved, originalEvidence.ID).Error)
				assert.Equal(t, originalEvidence, preserved, "corrections cannot rewrite the original metering receipt")
				require.NoError(t, db.First(&request, request.ID).Error)
				assert.Equal(t, original, request, "the original bill and metering link are immutable")
				require.NoError(t, db.First(&key, key.Id).Error)
				assert.EqualValues(t, 200-adjustment.Charged, key.RemainQuota)
				assert.EqualValues(t, adjustment.Charged, key.UsedQuota)
				require.NoError(t, db.First(&user, user.Id).Error)
				assert.EqualValues(t, adjustment.Charged, user.UsedQuota)
				assert.Equal(t, 1, user.RequestCount)
				differences, err := ReconcileCreditAccount(db, user.Id)
				require.NoError(t, err)
				assert.Empty(t, differences)
				var receipt CreditUsageEvidence
				require.NoError(t, db.First(&receipt, adjustment.UsageEvidenceID).Error)
				originalFingerprint := receipt.Fingerprint
				require.NoError(t, db.Model(&receipt).Update("fingerprint", "corrupt-receipt").Error)
				differences, err = ReconcileCreditAccount(db, user.Id)
				require.NoError(t, err)
				assert.NotEmpty(t, differences, "a broken corrective receipt cannot reconcile as balanced")
				require.NoError(t, db.Model(&receipt).Update("fingerprint", originalFingerprint).Error)
				originalMovements := adjustment.Movements
				require.NoError(t, db.Model(&adjustment).Update("movements", "[]").Error)
				differences, err = ReconcileCreditAccount(db, user.Id)
				require.NoError(t, err)
				assert.NotEmpty(t, differences, "net pack totals cannot conceal a missing original-allocation refund link")
				require.NoError(t, db.Model(&adjustment).Update("movements", originalMovements).Error)
				differences, err = ReconcileCreditAccount(db, user.Id)
				require.NoError(t, err)
				assert.Empty(t, differences)
				blockedBill, err := BeginCreditRequest(db, CreditRequestInput{UserID: user.Id, RequestID: "adjustment-blocked", ModelName: "model", Protocol: "openai", PriceSnapshot: "{}", TokenID: key.Id, Amount: 20}, 170)
				require.NoError(t, err)
				blockedBill, err = FinishCreditRequest(db, user.Id, blockedBill.ID, "settle", 20, 171)
				require.NoError(t, err)
				_, err = OpenCreditReviewCase(db, CreditReviewInput{UserID: user.Id, PackID: second.ID, ActorID: actor.Id, EventID: "block-original-source", Reason: "source under manual review"}, 172)
				require.NoError(t, err)
				require.NoError(t, db.Delete(&key).Error)
				correctedQuantity := float64(5)
				blockedCorrection, err := AdjustCreditBill(db, CreditBillAdjustmentInput{UserID: user.Id, RequestID: blockedBill.ID, ActorID: actor.Id, EventID: "blocked-refund", ReferenceQuota: 5, EvidenceVersion: "verified-fixture-v1", Facts: []hosttypes.UsageFact{{Field: "prompt_tokens", Unit: "token", Quantity: &correctedQuantity, Source: "upstream"}}, Reason: "verified corrected receipt"}, 173)
				require.NoError(t, err, "a removed Key cannot prevent the account's original-source refund")
				assert.EqualValues(t, 15, blockedCorrection.Refunded)
				require.NoError(t, db.First(&second, second.ID).Error)
				assert.EqualValues(t, 5, second.Spent)
				assert.EqualValues(t, 80, second.Available, "refund does not restore additional usable credits on a blocked source")
				assert.EqualValues(t, 15, second.Revoked)
				assert.NotZero(t, second.BlockedAt)
				var keys int64
				require.NoError(t, db.Model(&Token{}).Where("id = ?", key.Id).Count(&keys).Error)
				assert.Zero(t, keys, "refund never recreates a removed Key")
				var removed Token
				require.NoError(t, db.Unscoped().First(&removed, key.Id).Error)
				assert.True(t, removed.DeletedAt.Valid)
				assert.EqualValues(t, adjustment.Charged+5, removed.UsedQuota, "deleted Key history still follows the net account charge")
				assert.EqualValues(t, 200-adjustment.Charged-5, removed.RemainQuota)
				differences, err = ReconcileCreditAccount(db, user.Id)
				require.NoError(t, err)
				assert.Empty(t, differences)
			})
			t.Run("bill_adjustment_uses_original_window_generations", func(t *testing.T) {
				user := creditTestUser(t, db, "window-adjustment")
				require.NoError(t, db.Model(&user).Update("accounting_version", 1).Error)
				actor := creditTestUser(t, db, "window-adjustment-admin")
				require.NoError(t, db.Model(&actor).Updates(map[string]any{"role": common.RoleRootUser, "status": common.UserStatusEnabled}).Error)
				contract := SubscriptionPlan{Title: "Adjustment windows", TotalAmount: 500, WindowRules: SubscriptionWindowRules{{ID: "five-hour", DurationSeconds: 5 * 3600, Limit: 70}, {ID: "week", DurationSeconds: 7 * 24 * 3600, Limit: 200}}}
				snapshot, err := common.Marshal(contract)
				require.NoError(t, err)
				term := UserSubscription{UserId: user.Id, PlanVersionID: 1, ContractSnapshot: string(snapshot), StartTime: 100, EndTime: 100 + 30*24*3600, AmountTotal: 500, Status: "active"}
				require.NoError(t, db.Create(&term).Error)
				first, err := BeginCreditRequest(db, CreditRequestInput{UserID: user.Id, RequestID: "window-original", ModelName: "model", Protocol: "openai", PriceSnapshot: "{}", Playground: true, Amount: 50, BillingPreference: "subscription_only"}, 100)
				require.NoError(t, err)
				require.NoError(t, MarkCreditRequestSubmitted(db, user.Id, first.ID, 110))
				first, err = FinishCreditRequest(db, user.Id, first.ID, "settle", 60, 111)
				require.NoError(t, err)
				second, err := BeginCreditRequest(db, CreditRequestInput{UserID: user.Id, RequestID: "window-new", ModelName: "model", Protocol: "openai", PriceSnapshot: "{}", Playground: true, Amount: 10, BillingPreference: "subscription_only"}, 110+5*3600+1)
				require.NoError(t, err)
				require.NoError(t, MarkCreditRequestSubmitted(db, user.Id, second.ID, 110+5*3600+1))
				_, err = FinishCreditRequest(db, user.Id, second.ID, "settle", 10, 110+5*3600+2)
				require.NoError(t, err)
				quantity := float64(20)
				input := CreditBillAdjustmentInput{UserID: user.Id, RequestID: first.ID, ActorID: actor.Id, EventID: "window-adjustment-1", ReferenceQuota: 20, EvidenceVersion: "verified-fixture-v1", Facts: []hosttypes.UsageFact{{Field: "prompt_tokens", Unit: "token", Quantity: &quantity, Source: "upstream"}}, Reason: "verified corrected receipt"}
				adjustment, err := AdjustCreditBill(db, input, 19000)
				require.NoError(t, err)
				assert.EqualValues(t, 40, adjustment.Refunded)
				var history []SubscriptionWindow
				require.NoError(t, db.Where("subscription_id = ? AND rule_id = ?", term.Id, "five-hour").Order("generation asc").Find(&history).Error)
				require.Len(t, history, 2)
				assert.EqualValues(t, 20, history[0].Used)
				assert.EqualValues(t, 20, history[0].ReferenceUsed)
				assert.EqualValues(t, 10, history[1].Used)
				assert.EqualValues(t, 10, history[1].ReferenceUsed, "current generation receives no refund or reference correction")
				input.EventID, input.ExpectedRevision, input.ReferenceQuota = "window-adjustment-2", 1, 120
				quantity = 120
				adjustment, err = AdjustCreditBill(db, input, 19001)
				require.NoError(t, err)
				assert.EqualValues(t, 20, adjustment.Charged)
				assert.EqualValues(t, 100, adjustment.Uncollected)
				require.NoError(t, db.First(&term, term.Id).Error)
				assert.EqualValues(t, 30, term.AmountUsed, "several windows do not multiply the refunded usage")
				require.NoError(t, db.First(&user, user.Id).Error)
				assert.Equal(t, 30, user.UsedQuota)
				assert.Equal(t, 2, user.RequestCount)
				replay, err := FinishCreditRequest(db, user.Id, first.ID, "settle", 60, 19002)
				require.NoError(t, err)
				assert.Equal(t, first, replay)
				differences, err := ReconcileCreditAccount(db, user.Id)
				require.NoError(t, err)
				assert.Empty(t, differences, "reconciliation combines original allocations with append-only corrections")
			})
			t.Run("usage_evidence_survives_pre_intent_exit", func(t *testing.T) {
				user := creditTestUser(t, db, "usage-evidence")
				require.NoError(t, db.Model(&user).Update("accounting_version", 1).Error)
				_, err := GrantCreditPack(db, CreditGrant{UserID: user.Id, SourceType: "test", SourceID: "usage", Amount: 100, StartsAt: 1, ExpiresAt: 500, UseMask: CreditUseAPI}, 100)
				require.NoError(t, err)
				request, err := BeginCreditRequest(db, CreditRequestInput{UserID: user.Id, RequestID: "usage", ModelName: "model", Protocol: "openai", PriceSnapshot: `{"frozen_price":1}`, Playground: true, Amount: 40}, 100)
				require.NoError(t, err)
				lease, err := ClaimCreditExecution(db, user.Id, request.ID, "original", 10, 100)
				require.NoError(t, err)
				require.NoError(t, MarkCreditRequestSubmitted(db, user.Id, request.ID, 100, lease))
				zero, prompt := float64(0), float64(12)
				input := CreditEvidenceInput{UserID: user.Id, RequestID: request.ID, Attempt: 1, EventID: "final", Stage: "settlement", Version: "fixture-v1", Cumulative: true,
					Facts:   []hosttypes.UsageFact{{Field: "prompt_tokens", Unit: "token", Quantity: &prompt, Source: "upstream"}, {Field: "completion_tokens", Unit: "token", Quantity: &zero, Source: "upstream"}, {Field: "cache_creation_tokens", Unit: "token", Source: "unknown"}},
					Consume: &CreditConsumeSnapshot{ReferenceQuota: 25, PromptTokens: 12, CompletionTokens: 0, IsStream: true, UseTimeSeconds: 2, Other: `{"admin_info":{"reject_reason":"fixture"},"cache_tokens":8}`}}
				evidence, err := RecordCreditUsageEvidence(db, input, 101, lease)
				require.NoError(t, err)
				replay, err := RecordCreditUsageEvidence(db, input, 102, lease)
				require.NoError(t, err)
				assert.Equal(t, evidence.ID, replay.ID)
				changed := input
				changed.Version = "changed"
				_, err = RecordCreditUsageEvidence(db, changed, 102, lease)
				assert.ErrorIs(t, err, ErrCreditOperationConflict)
				_, err = FinishCreditRequest(db, user.Id, request.ID, "settle", 26, 102, lease)
				assert.ErrorIs(t, err, ErrCreditOperationConflict, "the financial intent must agree with saved final metering")
				require.NoError(t, db.First(&request, request.ID).Error)
				assert.Empty(t, request.IntentKind, "simulate exit after evidence commits but before financial intent")
				results, _, err := RecoverCreditRequests(db, "recovery", request.ID-1, 1, 111)
				require.NoError(t, err)
				require.Len(t, results, 1)
				assert.Equal(t, "settled", results[0].State)
				require.NoError(t, db.First(&request, request.ID).Error)
				assert.EqualValues(t, 25, request.Charged)
				_, err = RecordCreditUsageEvidence(db, input, 111, lease)
				assert.ErrorIs(t, err, ErrCreditLeaseLost)
				var outbox CreditLogOutbox
				require.NoError(t, db.Where("request_id = ?", request.ID).First(&outbox).Error)
				var log Log
				require.NoError(t, common.UnmarshalJsonStr(outbox.Payload, &log))
				assert.Equal(t, 12, log.PromptTokens)
				assert.Zero(t, log.CompletionTokens)
				assert.True(t, log.IsStream)
				assert.Contains(t, log.Other, `"source":"unknown"`)
				assert.NotContains(t, formatLogOtherJSON(log.Other, logOtherVisibilityUser), "reject_reason")
			})
			t.Run("unknown_bill_review_preserves_original_evidence", func(t *testing.T) {
				actor := creditTestUser(t, db, "bill-review-admin")
				require.NoError(t, db.Model(&actor).Update("role", common.RoleRootUser).Error)
				for _, fee := range []int64{25, 0, 120} {
					t.Run(fmt.Sprint(fee), func(t *testing.T) {
						user := creditTestUser(t, db, fmt.Sprintf("bill-review-%d", fee))
						require.NoError(t, db.Model(&user).Update("accounting_version", 1).Error)
						key := Token{UserId: user.Id, Key: fmt.Sprintf("review-key-%d", fee), Name: "review", Status: common.TokenStatusEnabled, RemainQuota: 100, ExpiredTime: -1}
						require.NoError(t, db.Create(&key).Error)
						pack, err := GrantCreditPack(db, CreditGrant{UserID: user.Id, SourceType: "test", SourceID: "review", Amount: 100, StartsAt: 1, ExpiresAt: 500, UseMask: CreditUseAPI}, 100)
						require.NoError(t, err)
						request, err := BeginCreditRequest(db, CreditRequestInput{UserID: user.Id, RequestID: "review", ModelName: "model", Protocol: "openai", PriceSnapshot: `{"original_price":1}`, TokenID: key.Id, Amount: 40}, 100)
						require.NoError(t, err)
						lease, err := ClaimCreditExecution(db, user.Id, request.ID, "original-review", 10, 100)
						require.NoError(t, err)
						require.NoError(t, MarkCreditRequestSubmitted(db, user.Id, request.ID, 100, lease))
						original, err := RecordCreditUsageEvidence(db, CreditEvidenceInput{UserID: user.Id, RequestID: request.ID, EventID: "unknown-final", Attempt: 1, Stage: "settlement", Version: "fixture-v1", Facts: []hosttypes.UsageFact{{Field: "completion_tokens", Unit: "token", Source: "unknown"}}, Consume: &CreditConsumeSnapshot{Other: `{}`}}, 101, lease)
						require.NoError(t, err)
						_, err = FinishCreditRequest(db, user.Id, request.ID, "settle", 0, 102, lease)
						require.ErrorIs(t, err, ErrCreditNeedsReview)
						quantity := float64(fee)
						input := CreditBillReviewInput{UserID: user.Id, RequestID: request.ID, ActorID: actor.Id, EventID: "verified-final", ExpectedEvidenceID: original.ID, ExternalReference: "verified-external-meter", Reason: "manual verification", EvidenceVersion: "verified-v1", Facts: []hosttypes.UsageFact{{Field: "completion_tokens", Unit: "token", Quantity: &quantity, Source: "upstream"}}, Consume: &CreditConsumeSnapshot{ReferenceQuota: fee, ZeroChargeEstablished: fee == 0, CompletionTokens: int(fee), IsStream: true, Other: `{}`}}
						_, err = ApproveCreditBillReview(db, input, 105)
						assert.ErrorIs(t, err, ErrCreditLeaseLost, "manual review cannot steal a live execution")
						unauthorized := input
						unauthorized.ActorID = user.Id
						_, err = ApproveCreditBillReview(db, unauthorized, 111)
						assert.Error(t, err)
						stale := input
						stale.ExpectedEvidenceID++
						_, err = ApproveCreditBillReview(db, stale, 111)
						assert.ErrorIs(t, err, ErrCreditOperationConflict)
						estimated := input
						estimated.EventID = "estimate-is-not-verification"
						estimated.Facts = []hosttypes.UsageFact{{Field: "completion_tokens", Unit: "token", Quantity: &quantity, Source: "estimate", Algorithm: "fixture-estimate-v1"}}
						_, err = ApproveCreditBillReview(db, estimated, 111)
						require.ErrorIs(t, err, ErrCreditInvalid, "a manual approval cannot promote an estimate into verified metering")
						if fee == 25 {
							oversized := input
							oversized.EventID = "oversized-evidence"
							consume := *input.Consume
							consume.Other = `{"note":"` + strings.Repeat("a", 65500) + `"}`
							oversized.Consume = &consume
							_, err = ApproveCreditBillReview(db, oversized, 111)
							require.ErrorIs(t, err, ErrCreditInvalid, "serialized evidence must fit the same TEXT contract on all databases")
						}
						if fee == 0 {
							unknown := input
							unknown.Consume = &CreditConsumeSnapshot{Other: `{}`}
							_, err = ApproveCreditBillReview(db, unknown, 111)
							assert.ErrorIs(t, err, ErrCreditInvalid, "unknown zero is not verified free usage")
						}
						require.NoError(t, db.Callback().Update().Before("gorm:update").Register("test:review-approval-fault", func(tx *gorm.DB) {
							if values, ok := tx.Statement.Dest.(map[string]any); ok && values["review_evidence_id"] != nil {
								tx.AddError(errors.New("review approval write unavailable"))
							}
						}))
						_, err = ApproveCreditBillReview(db, input, 111)
						require.Error(t, err)
						require.NoError(t, db.Callback().Update().Remove("test:review-approval-fault"))
						var evidenceCount int64
						require.NoError(t, db.Model(&CreditUsageEvidence{}).Where("request_id = ?", request.ID).Count(&evidenceCount).Error)
						assert.EqualValues(t, 1, evidenceCount, "failed approval commits neither new evidence nor an operation")
						var approval CreditBillReviewApproval
						if fee == 120 {
							type result struct {
								input    CreditBillReviewInput
								approval CreditBillReviewApproval
								err      error
							}
							barrier, outcomes := make(chan struct{}), make(chan result, 2)
							var workers sync.WaitGroup
							for _, event := range []string{"verified-final", "concurrent-final"} {
								candidate := input
								candidate.EventID = event
								workers.Go(func() {
									<-barrier
									approved, err := ApproveCreditBillReview(db.Session(&gorm.Session{}), candidate, 111)
									outcomes <- result{candidate, approved, err}
								})
							}
							close(barrier)
							workers.Wait()
							accepted := 0
							for range 2 {
								outcome := <-outcomes
								if outcome.err != nil {
									assert.ErrorIs(t, outcome.err, ErrCreditOperationConflict)
									continue
								}
								accepted++
								input, approval = outcome.input, outcome.approval
							}
							require.Equal(t, 1, accepted, "two administrators cannot both confirm one unknown bill")
						} else {
							approval, err = ApproveCreditBillReview(db, input, 111)
							require.NoError(t, err)
						}
						assert.Equal(t, original.ID, approval.OriginalEvidenceID)
						assert.NotEqual(t, original.ID, approval.EvidenceID)
						replay, err := ApproveCreditBillReview(db, input, 112)
						require.NoError(t, err)
						assert.Equal(t, approval, replay)
						changed := input
						changed.Reason = "changed"
						_, err = ApproveCreditBillReview(db, changed, 112)
						assert.ErrorIs(t, err, ErrCreditOperationConflict)
						_, err = FinishCreditRequest(db, user.Id, request.ID, "settle", 0, 112, lease)
						assert.ErrorIs(t, err, ErrCreditLeaseLost, "the old producer cannot overwrite an approved bill")
						if fee == 0 {
							late, err := ClaimCreditExecution(db, user.Id, request.ID, "late-review-producer", 10, 112)
							require.NoError(t, err)
							_, err = FinishCreditRequest(db, user.Id, request.ID, "release", 0, 112, late)
							assert.ErrorIs(t, err, ErrCreditOperationConflict, "an approved settlement cannot become a release")
							err = MarkCreditRequestReviewAt(db, user.Id, request.ID, 112, late)
							assert.ErrorIs(t, err, ErrCreditOperationConflict, "an automatic failure cannot revoke manual confirmation")
							require.NoError(t, YieldCreditExecution(db, late, 112))
						}
						if fee == 25 {
							require.NoError(t, db.Callback().Create().Before("gorm:create").Register("test:review-money-fault", func(tx *gorm.DB) {
								if row, ok := tx.Statement.Dest.(*CreditLogOutbox); ok && row.RequestID == request.ID {
									tx.AddError(errors.New("review money transaction unavailable"))
								}
							}))
							failed, _, err := RecoverCreditRequests(db, "approved-fault", request.ID-1, 1, 112)
							require.NoError(t, err)
							require.Len(t, failed, 1)
							assert.Contains(t, failed[0].Error, "review money transaction unavailable")
							require.NoError(t, db.Callback().Create().Remove("test:review-money-fault"))
							pending, err := GetCreditBillBalance(db, user.Id, request.ID)
							require.NoError(t, err, "a bill remains readable while its financial transaction is pending")
							assert.EqualValues(t, 25, pending.ReferenceQuota)
							assert.Zero(t, pending.Charged)
							assert.Zero(t, pending.Uncollected, "uncollected is decided at settlement")
							require.NoError(t, db.First(&pack, pack.ID).Error)
							assert.EqualValues(t, 40, pack.Held)
							assert.Zero(t, pack.Spent)
						}
						results, _, err := RecoverCreditRequests(db, "approved-recovery", request.ID-1, 1, 300)
						require.NoError(t, err)
						require.Len(t, results, 1)
						assert.Empty(t, results[0].Error)
						assert.Equal(t, "settled", results[0].State)
						require.NoError(t, db.First(&request, request.ID).Error)
						assert.Equal(t, original.ID, request.UsageEvidenceID)
						assert.Equal(t, fee, request.Actual)
						assert.Equal(t, min(fee, 100), request.Charged)
						assert.Equal(t, max(fee-100, 0), request.Uncollected)
						var preserved CreditUsageEvidence
						require.NoError(t, db.First(&preserved, original.ID).Error)
						assert.Equal(t, original, preserved)
						require.NoError(t, db.First(&key, key.Id).Error)
						assert.EqualValues(t, request.Charged, key.UsedQuota)
						assert.EqualValues(t, 100-request.Charged, key.RemainQuota)
						require.NoError(t, db.First(&pack, pack.ID).Error)
						assert.Zero(t, pack.Held)
						assert.Equal(t, request.Charged, pack.Spent)
						var outbox CreditLogOutbox
						require.NoError(t, db.Where("request_id = ?", request.ID).First(&outbox).Error)
						var log Log
						require.NoError(t, common.UnmarshalJsonStr(outbox.Payload, &log))
						assert.EqualValues(t, request.Charged, log.Quota)
						assert.Equal(t, int(fee), log.CompletionTokens)
						assert.Contains(t, log.Other, "verified-v1")
						assert.NotContains(t, formatLogOtherJSON(log.Other, logOtherVisibilityUser), "verified-external-meter")
						differences, err := ReconcileCreditAccount(db, user.Id)
						require.NoError(t, err)
						assert.Empty(t, differences)
						var reviewed CreditUsageEvidence
						require.NoError(t, db.First(&reviewed, approval.EvidenceID).Error)
						fingerprint := reviewed.Fingerprint
						require.NoError(t, db.Model(&reviewed).Update("fingerprint", "corrupt").Error)
						_, err = GetCreditBillBalance(db, user.Id, request.ID)
						assert.ErrorIs(t, err, ErrCreditInvariant)
						differences, err = ReconcileCreditAccount(db, user.Id)
						require.NoError(t, err)
						assert.NotEmpty(t, differences, "a matching amount does not hide a broken approval")
						require.NoError(t, db.Model(&reviewed).Update("fingerprint", fingerprint).Error)
						_, err = ApproveCreditBillReview(db, input, 301)
						require.NoError(t, err, "replay remains the original approval after settlement")
						input.EventID = "new-approval-after-settlement"
						_, err = ApproveCreditBillReview(db, input, 113)
						assert.ErrorIs(t, err, ErrCreditOperationConflict)
					})
				}
			})
			t.Run("estimator_metadata_is_bounded_and_keeps_original_counter", func(t *testing.T) {
				user := creditTestUser(t, db, "estimator-evidence")
				require.NoError(t, db.Model(&user).Update("accounting_version", 1).Error)
				_, err := GrantCreditPack(db, CreditGrant{UserID: user.Id, SourceType: "test", SourceID: "estimator", Amount: 100, StartsAt: 1, ExpiresAt: 500, UseMask: CreditUseAPI}, 100)
				require.NoError(t, err)
				request, err := BeginCreditRequest(db, CreditRequestInput{UserID: user.Id, RequestID: "estimator", ModelName: "model", Protocol: "openai", PriceSnapshot: `{}`, Playground: true, Amount: 40}, 100)
				require.NoError(t, err)
				quantity := float64(12)
				descriptor := &hosttypes.UsageEstimation{Version: "counter-v1", Model: "claude-observed", Method: "provider-heuristic", Quantity: 12, Parameters: map[string]float64{"word": 1.13}, Settings: map[string]bool{"count_token": true}}
				descriptor.Components = []hosttypes.UsageEstimateComponent{{Index: 0, Kind: "image", Method: "media-constant", Quantity: 12, Parameters: map[string]float64{"constant_tokens": 12}}}
				input := CreditEvidenceInput{UserID: user.Id, RequestID: request.ID, EventID: "estimate", Attempt: 1, Stage: "estimate", Sequence: 1, Cumulative: true, Version: "fixture-v1", Facts: []hosttypes.UsageFact{{Field: "prompt_tokens", Unit: "token", Quantity: &quantity, Source: "estimate", Algorithm: "counter-v1", Estimation: descriptor}}}
				for caseIndex, invalid := range []struct {
					name       string
					source     string
					estimation hosttypes.UsageEstimation
				}{
					{"provider_cannot_claim_estimator", "upstream", *descriptor},
					{"missing_counter_version", "estimate", hosttypes.UsageEstimation{Model: "model", Method: "counter", Quantity: 12}},
					{"negative_counter_quantity", "estimate", hosttypes.UsageEstimation{Version: "counter-v1", Model: "model", Method: "counter", Quantity: -1}},
					{"invalid_numeric_parameter", "estimate", hosttypes.UsageEstimation{Version: "counter-v1", Model: "model", Method: "counter", Quantity: 12, Parameters: map[string]float64{"weight": -1}}},
					{"invalid_component_index", "estimate", hosttypes.UsageEstimation{Version: "counter-v1", Model: "model", Method: "counter", Quantity: 12, Components: []hosttypes.UsageEstimateComponent{{Index: -1, Kind: "image", Method: "counter", Quantity: 12}}}},
					{"invalid_component_kind", "estimate", hosttypes.UsageEstimation{Version: "counter-v1", Model: "model", Method: "counter", Quantity: 12, Components: []hosttypes.UsageEstimateComponent{{Index: 0, Kind: "private-file", Method: "counter", Quantity: 12}}}},
					{"invalid_component_parameter", "estimate", hosttypes.UsageEstimation{Version: "counter-v1", Model: "model", Method: "counter", Quantity: 12, Components: []hosttypes.UsageEstimateComponent{{Index: 0, Kind: "image", Method: "counter", Quantity: 12, Parameters: map[string]float64{"width": -1}}}}},
					{"too_many_components", "estimate", hosttypes.UsageEstimation{Version: "counter-v1", Model: "model", Method: "counter", Quantity: 12, Components: make([]hosttypes.UsageEstimateComponent, 65)}},
				} {
					t.Run(invalid.name, func(t *testing.T) {
						rejected := input
						rejected.EventID = invalid.name
						rejected.Sequence = int64(caseIndex + 10)
						rejected.Facts = []hosttypes.UsageFact{{Field: "prompt_tokens", Unit: "token", Quantity: &quantity, Source: invalid.source, Algorithm: "counter-v1", Estimation: &invalid.estimation}}
						_, err := RecordCreditUsageEvidence(db, rejected, 100)
						require.ErrorIs(t, err, ErrCreditInvalid)
					})
				}
				receipt, err := RecordCreditUsageEvidence(db, input, 100)
				require.NoError(t, err)
				replay, err := RecordCreditUsageEvidence(db, input, 101)
				require.NoError(t, err)
				assert.Equal(t, receipt.ID, replay.ID)
				changed := *descriptor
				changed.Parameters = map[string]float64{"word": 9}
				input.Facts[0].Estimation = &changed
				_, err = RecordCreditUsageEvidence(db, input, 101)
				require.ErrorIs(t, err, ErrCreditOperationConflict)
				facts, err := GetCreditUsageProjection(db, user.Id, request.ID, 1)
				require.NoError(t, err)
				require.Len(t, facts, 1)
				require.NotNil(t, facts[0].Estimation)
				assert.Equal(t, 1.13, facts[0].Estimation.Parameters["word"])
				assert.Equal(t, "claude-observed", facts[0].Estimation.Model)
				require.Len(t, facts[0].Estimation.Components, 1)
				assert.EqualValues(t, 12, facts[0].Estimation.Components[0].Quantity)
				assert.Equal(t, "image", facts[0].Estimation.Components[0].Kind)
				var persisted CreditUsageEvidence
				require.NoError(t, db.First(&persisted, receipt.ID).Error)
				assert.Equal(t, receipt.Payload, persisted.Payload)
			})
			t.Run("usage_sequence_conflict_is_not_a_second_receipt", func(t *testing.T) {
				user := creditTestUser(t, db, "usage-sequence")
				require.NoError(t, db.Model(&user).Update("accounting_version", 1).Error)
				_, err := GrantCreditPack(db, CreditGrant{UserID: user.Id, SourceType: "test", SourceID: "sequence", Amount: 100, StartsAt: 1, ExpiresAt: 500, UseMask: CreditUseAPI}, 100)
				require.NoError(t, err)
				request, err := BeginCreditRequest(db, CreditRequestInput{UserID: user.Id, RequestID: "sequence", ModelName: "model", Protocol: "openai", PriceSnapshot: `{}`, Playground: true, Amount: 40}, 100)
				require.NoError(t, err)
				value := float64(12)
				input := CreditEvidenceInput{UserID: user.Id, RequestID: request.ID, Attempt: 1, EventID: "first", Sequence: 1, Stage: "upstream", Version: "fixture-v1", Cumulative: true, Facts: []hosttypes.UsageFact{{Field: "prompt_tokens", Unit: "token", Quantity: &value, Source: "upstream"}}}
				_, err = RecordCreditUsageEvidence(db, input, 100)
				require.NoError(t, err)
				input.EventID = "different-event-same-sequence"
				_, err = RecordCreditUsageEvidence(db, input, 100)
				assert.ErrorIs(t, err, ErrCreditOperationConflict)
				input.EventID, input.Sequence, value = "newer", 5, 10
				_, err = RecordCreditUsageEvidence(db, input, 100)
				require.NoError(t, err)
				input.EventID, input.Sequence, value = "late-older", 3, 6
				_, err = RecordCreditUsageEvidence(db, input, 100)
				require.NoError(t, err)
				input.EventID, input.Sequence, input.Cumulative, value = "increment", 6, false, 2
				delta, err := RecordCreditUsageEvidence(db, input, 100)
				require.NoError(t, err)
				replay, err := RecordCreditUsageEvidence(db, input, 100)
				require.NoError(t, err)
				assert.Equal(t, delta.ID, replay.ID)
				facts, err := GetCreditUsageProjection(db, user.Id, request.ID, 1)
				require.NoError(t, err)
				require.Len(t, facts, 1)
				assert.Equal(t, float64(12), *facts[0].Quantity, "newer cumulative 10 plus one increment 2; late cumulative 6 cannot replace it")
				input.EventID, input.Sequence, input.Attempt, value = "retry-increment", 1, 2, 4
				_, err = RecordCreditUsageEvidence(db, input, 100)
				require.NoError(t, err)
				facts, err = GetCreditUsageProjection(db, user.Id, request.ID, 2)
				require.NoError(t, err)
				require.Len(t, facts, 1)
				assert.Equal(t, float64(4), *facts[0].Quantity, "attempts never combine their usage")
				_, err = GetCreditUsageProjection(db, user.Id+1000, request.ID, 1)
				assert.ErrorIs(t, err, gorm.ErrRecordNotFound)
				input.EventID, input.Sequence, value = "negative", 2, -1
				_, err = RecordCreditUsageEvidence(db, input, 100)
				assert.ErrorIs(t, err, ErrCreditInvalid)
				seconds := 2.25
				input.EventID, input.Sequence, input.Cumulative = "duration", 2, true
				input.Facts = []hosttypes.UsageFact{{Field: "audio_input_seconds", Unit: "second", Quantity: &seconds, Source: "upstream"}}
				duration, err := RecordCreditUsageEvidence(db, input, 100)
				require.NoError(t, err)
				replay, err = RecordCreditUsageEvidence(db, input, 100)
				require.NoError(t, err)
				assert.Equal(t, duration.ID, replay.ID)
				facts, err = GetCreditUsageProjection(db, user.Id, request.ID, 2)
				require.NoError(t, err)
				index := slices.IndexFunc(facts, func(fact hosttypes.UsageFact) bool { return fact.Field == "audio_input_seconds" })
				require.NotEqual(t, -1, index)
				assert.Equal(t, "second", facts[index].Unit)
				assert.Equal(t, 2.25, *facts[index].Quantity)
				input.EventID, input.Sequence, input.Facts[0].Unit = "fractional-token", 3, "token"
				_, err = RecordCreditUsageEvidence(db, input, 100)
				assert.ErrorIs(t, err, ErrCreditInvalid, "seconds cannot be relabelled as fractional tokens")
				for _, tc := range []struct {
					attempt        int
					partial, count float64
					source         string
				}{
					{3, 3, 5, "upstream"},
					{4, 8, 5, "upstream"},
					{5, 8, 5, "adaptor"},
					{6, 3, 5, "adaptor"},
				} {
					input.Attempt, input.EventID, input.Sequence, input.Stage, input.Cumulative = tc.attempt, "partial-output", 1, "upstream", true
					input.Facts = []hosttypes.UsageFact{{Field: "completion_tokens", Unit: "token", Quantity: &tc.partial, Source: tc.source, Partial: true}}
					_, err = RecordCreditUsageEvidence(db, input, 100)
					require.NoError(t, err)
					input.EventID, input.Sequence, input.Stage = "estimated-output", 2, "estimate"
					input.Facts = []hosttypes.UsageFact{{Field: "completion_tokens", Unit: "token", Quantity: &tc.count, Source: "estimate", Algorithm: "fixture-count-v1"}}
					_, err = RecordCreditUsageEvidence(db, input, 100)
					require.NoError(t, err)
					facts, err = GetCreditUsageProjection(db, user.Id, request.ID, tc.attempt)
					require.NoError(t, err)
					require.Len(t, facts, 1)
					assert.Equal(t, max(tc.partial, tc.count), *facts[0].Quantity, "estimated output retains the received partial count as its lower bound")
					assert.Equal(t, "estimate", facts[0].Source)
					assert.False(t, facts[0].Partial)
					zero := float64(0)
					input.EventID, input.Sequence, input.Stage = "complete-output", 3, "upstream"
					input.Facts = []hosttypes.UsageFact{{Field: "completion_tokens", Unit: "token", Quantity: &zero, Source: "upstream"}}
					_, err = RecordCreditUsageEvidence(db, input, 100)
					require.NoError(t, err)
					input.EventID, input.Sequence, input.Stage = "late-estimate", 4, "estimate"
					input.Facts = []hosttypes.UsageFact{{Field: "completion_tokens", Unit: "token", Quantity: &tc.count, Source: "estimate", Algorithm: "fixture-count-v1"}}
					_, err = RecordCreditUsageEvidence(db, input, 100)
					require.NoError(t, err)
					facts, err = GetCreditUsageProjection(db, user.Id, request.ID, tc.attempt)
					require.NoError(t, err)
					require.Len(t, facts, 1)
					assert.Zero(t, *facts[0].Quantity, "a complete explicit zero cannot be replaced by an estimate")
					assert.Equal(t, "upstream", facts[0].Source)
				}
			})
			t.Run("settlement_outbox_and_log_response_loss", func(t *testing.T) {
				user := creditTestUser(t, db, "outbox")
				require.NoError(t, db.Model(&user).Update("accounting_version", 1).Error)
				_, err := GrantCreditPack(db, CreditGrant{UserID: user.Id, SourceType: "test", SourceID: "outbox", Amount: 100, StartsAt: 1, ExpiresAt: 500, UseMask: CreditUseAPI}, 100)
				require.NoError(t, err)
				request, err := BeginCreditRequest(db, CreditRequestInput{UserID: user.Id, RequestID: "outbox", ModelName: "model", Protocol: "openai", PriceSnapshot: `{}`, Playground: true, Amount: 40}, 100)
				require.NoError(t, err)
				_, err = FinishCreditRequest(db, user.Id, request.ID, "settle", 25, 100)
				require.NoError(t, err)
				_, err = FinishCreditRequest(db, user.Id, request.ID, "settle", 25, 100)
				require.NoError(t, err)
				require.NoError(t, db.First(&user, user.Id).Error)
				assert.Equal(t, 25, user.UsedQuota)
				assert.Equal(t, 1, user.RequestCount)
				var pending CreditLogOutbox
				require.NoError(t, db.Where("request_id = ?", request.ID).First(&pending).Error)
				logDriver := driver
				if dialect == "sqlite" {
					logDriver = sqlite.Open(filepath.Join(t.TempDir(), "logs.db") + "?_pragma=busy_timeout(10000)")
				}
				if dialect == "mysql" {
					logDriver = mysqlMigrationDialector{mysql.Dialector{Config: &mysql.Config{DSN: os.Getenv("TEST_MYSQL_LOG_DSN")}}}
				}
				if dialect == "postgres" {
					logDriver = postgresMigrationDialector{postgres.Dialector{Config: &postgres.Config{DSN: os.Getenv("TEST_POSTGRES_LOG_DSN")}}}
				}
				logs, err := gorm.Open(logDriver, &gorm.Config{NamingStrategy: schema.NamingStrategy{TablePrefix: "credit_delivery_test_"}})
				require.NoError(t, err)
				sink, err := logs.DB()
				require.NoError(t, err)
				t.Cleanup(func() {
					require.NoError(t, logs.Migrator().DropTable(&CreditLogDelivery{}, &Log{}))
					require.NoError(t, sink.Close())
				})
				require.NoError(t, logs.AutoMigrate(&Log{}, &CreditLogDelivery{}))
				require.NoError(t, db.Callback().Update().Before("gorm:update").Register("credit_outbox_ack_fail", func(tx *gorm.DB) {
					if tx.Statement.Schema != nil && tx.Statement.Schema.Name == "CreditLogOutbox" {
						tx.AddError(errors.New("outbox acknowledgement unavailable"))
					}
				}))
				err = DeliverCreditLog(db, logs, pending.ID, 101)
				require.Error(t, err)
				require.NoError(t, db.Callback().Update().Remove("credit_outbox_ack_fail"))
				var count int64
				require.NoError(t, logs.Model(&Log{}).Count(&count).Error)
				assert.EqualValues(t, 1, count, "log committed before acknowledgement failure")
				require.NoError(t, DeliverCreditLog(db, logs, pending.ID, 102))
				require.NoError(t, DeliverCreditLog(db, logs, pending.ID, 103))
				require.NoError(t, logs.Model(&Log{}).Count(&count).Error)
				assert.EqualValues(t, 1, count)
				require.NoError(t, db.First(&pending, pending.ID).Error)
				assert.Equal(t, "delivered", pending.State)
				require.NoError(t, db.First(&user, user.Id).Error)
				assert.Equal(t, 25, user.UsedQuota)
				assert.Equal(t, 1, user.RequestCount)
				actor := creditTestUser(t, db, "outbox-adjust-admin")
				require.NoError(t, db.Model(&actor).Updates(map[string]any{"role": common.RoleRootUser, "status": common.UserStatusEnabled}).Error)
				quantity := float64(10)
				input := CreditBillAdjustmentInput{UserID: user.Id, RequestID: request.ID, ActorID: actor.Id, EventID: "outbox-adjustment", ReferenceQuota: 10, EvidenceVersion: "verified-fixture-v1", Facts: []hosttypes.UsageFact{{Field: "prompt_tokens", Unit: "token", Quantity: &quantity, Source: "upstream"}}, Reason: "corrected receipt"}
				adjustment, err := AdjustCreditBill(db, input, 104)
				require.NoError(t, err)
				require.NoError(t, db.Callback().Update().Before("gorm:update").Register("credit_adjustment_ack_fail", func(tx *gorm.DB) {
					if tx.Statement.Schema != nil && tx.Statement.Schema.Name == "CreditBillAdjustment" {
						tx.AddError(errors.New("adjustment acknowledgement unavailable"))
					}
				}))
				err = DeliverCreditBillAdjustmentLog(db, logs, adjustment.ID, 105)
				require.Error(t, err)
				require.NoError(t, db.Callback().Update().Remove("credit_adjustment_ack_fail"))
				require.NoError(t, logs.Model(&Log{}).Count(&count).Error)
				assert.EqualValues(t, 2, count, "refund log commits before primary acknowledgement")
				require.NoError(t, DeliverCreditBillAdjustmentLog(db, logs, adjustment.ID, 106))
				require.NoError(t, DeliverCreditBillAdjustmentLog(db, logs, adjustment.ID, 107))
				input.EventID, input.ExpectedRevision, input.ReferenceQuota = "outbox-increase", 1, 40
				quantity = 40
				increase, err := AdjustCreditBill(db, input, 108)
				require.NoError(t, err)
				require.NoError(t, DeliverCreditBillAdjustmentLog(db, logs, increase.ID, 109))
				var delivered []Log
				require.NoError(t, logs.Order("id asc").Find(&delivered).Error)
				require.Len(t, delivered, 3)
				assert.Equal(t, LogTypeConsume, delivered[0].Type)
				assert.Equal(t, 25, delivered[0].Quota, "original consume log remains unchanged")
				assert.Equal(t, LogTypeRefund, delivered[1].Type)
				assert.Equal(t, 15, delivered[1].Quota)
				assert.Equal(t, LogTypeManage, delivered[2].Type)
				assert.Zero(t, delivered[2].Quota, "raising reference cost cannot create a new charge")
				require.NoError(t, db.First(&adjustment, adjustment.ID).Error)
				assert.Equal(t, "delivered", adjustment.LogState)
				assert.EqualValues(t, 10, adjustment.Charged)
			})
			t.Run("settlement_survives_soft_deleted_key", func(t *testing.T) {
				user := creditTestUser(t, db, "deleted-key-settlement")
				require.NoError(t, db.Model(&user).Update("accounting_version", 1).Error)
				key := Token{UserId: user.Id, Key: "deleted-key-settlement", Status: common.TokenStatusEnabled, RemainQuota: 100, ExpiredTime: -1}
				require.NoError(t, db.Create(&key).Error)
				_, err := GrantCreditPack(db, CreditGrant{UserID: user.Id, SourceType: "test", SourceID: "deleted-key-settlement", Amount: 100, StartsAt: 1, ExpiresAt: 500, UseMask: CreditUseAPI}, 100)
				require.NoError(t, err)
				input := CreditRequestInput{UserID: user.Id, RequestID: "deleted-key-settlement", ModelName: "model", Protocol: "openai", PriceSnapshot: "{}", TokenID: key.Id, Amount: 40}
				request, err := BeginCreditRequest(db, input, 100)
				require.NoError(t, err)
				require.NoError(t, MarkCreditRequestSubmitted(db, user.Id, request.ID, 100))
				require.NoError(t, db.Delete(&key).Error)
				settled, err := FinishCreditRequest(db, user.Id, request.ID, "settle", 25, 101)
				require.NoError(t, err, "removing a credential cannot strand its already admitted request")
				assert.EqualValues(t, 25, settled.Charged)
				assert.Equal(t, "settled", settled.State)
				var historical Token
				require.NoError(t, db.Unscoped().First(&historical, key.Id).Error)
				assert.True(t, historical.DeletedAt.Valid)
				assert.Equal(t, 75, historical.RemainQuota)
				assert.Equal(t, 25, historical.UsedQuota)
				var active int64
				require.NoError(t, db.Model(&Token{}).Where("id = ?", key.Id).Count(&active).Error)
				assert.Zero(t, active)
				input.RequestID, input.Amount = "deleted-key-new-request", 1
				_, err = BeginCreditRequest(db, input, 102)
				assert.ErrorIs(t, err, gorm.ErrRecordNotFound, "new admission cannot read soft-deleted credentials")
				packs, err := ListCreditPacks(db, user.Id, 102)
				require.NoError(t, err)
				require.Len(t, packs, 1)
				assert.EqualValues(t, 75, packs[0].Available)
				assert.EqualValues(t, 25, packs[0].Spent)
				assert.Zero(t, packs[0].Held)
				differences, err := ReconcileCreditAccount(db, user.Id)
				require.NoError(t, err)
				assert.Empty(t, differences)
			})
			t.Run("request_token_atomicity_and_persistent_completion", func(t *testing.T) {
				user := creditTestUser(t, db, "request")
				require.NoError(t, db.Model(&user).Update("accounting_version", 1).Error)
				pack, err := GrantCreditPack(db, CreditGrant{UserID: user.Id, SourceType: "test", SourceID: "only", Amount: 100, StartsAt: 1, ExpiresAt: 200, UseMask: CreditUseAPI}, 100)
				require.NoError(t, err)
				token := Token{UserId: user.Id, Key: "request-token", RemainQuota: 10}
				require.NoError(t, db.Create(&token).Error)
				input := CreditRequestInput{UserID: user.Id, RequestID: "request", ModelName: "model", Protocol: "openai", PriceSnapshot: `{"price":1}`, TokenID: token.Id, Amount: 50}
				_, err = BeginCreditRequest(db, input, 100)
				assert.ErrorIs(t, err, ErrCreditInsufficient)
				require.NoError(t, db.First(&pack, pack.ID).Error)
				assert.EqualValues(t, 100, pack.Available)
				assert.Zero(t, pack.Held)
				require.NoError(t, db.Model(&token).Update("remain_quota", 100).Error)
				request, err := BeginCreditRequest(db, input, 100)
				require.NoError(t, err)
				repeated, err := BeginCreditRequest(db.Session(&gorm.Session{}), input, 150)
				require.NoError(t, err)
				assert.Equal(t, request, repeated)
				require.NoError(t, db.First(&token, token.Id).Error)
				assert.Equal(t, 50, token.RemainQuota)
				require.NoError(t, db.Callback().Update().Before("gorm:update").Register("credit_fail_token", func(tx *gorm.DB) {
					if tx.Statement.Schema != nil && tx.Statement.Schema.Name == "Token" {
						tx.AddError(errors.New("token unavailable"))
					}
				}))
				_, err = FinishCreditRequest(db, user.Id, request.ID, "settle", 35, 150)
				require.Error(t, err)
				require.NoError(t, db.Callback().Update().Remove("credit_fail_token"))
				require.NoError(t, db.First(&pack, pack.ID).Error)
				assert.EqualValues(t, 50, pack.Held)
				assert.Zero(t, pack.Spent)
				require.NoError(t, db.First(&request, request.ID).Error)
				assert.Equal(t, "pending", request.State)
				assert.Equal(t, "settle", request.IntentKind)
				completed, err := FinishCreditRequest(db.Session(&gorm.Session{}), user.Id, request.ID, "settle", 35, 150)
				require.NoError(t, err)
				assert.Equal(t, "settled", completed.State)
				assert.EqualValues(t, 35, completed.Charged)
				_, err = FinishCreditRequest(db, user.Id, request.ID, "settle", 35, 150)
				require.NoError(t, err)
				require.NoError(t, db.First(&token, token.Id).Error)
				assert.Equal(t, 65, token.RemainQuota)
				assert.Equal(t, 35, token.UsedQuota)
				_, err = FinishCreditRequest(db, user.Id, request.ID, "release", 0, 150)
				assert.ErrorIs(t, err, ErrCreditOperationConflict)
				input.RequestID = "unsent"
				unsent, err := BeginCreditRequest(db, input, 150)
				require.NoError(t, err)
				_, err = FinishCreditRequest(db, user.Id, unsent.ID, "release", 0, 150)
				require.NoError(t, err)
				input.RequestID = "uncertain"
				uncertain, err := BeginCreditRequest(db, input, 150)
				require.NoError(t, err)
				require.NoError(t, MarkCreditRequestSubmitted(db, user.Id, uncertain.ID, 150))
				_, err = FinishCreditRequest(db, user.Id, uncertain.ID, "release", 0, 151)
				assert.ErrorIs(t, err, ErrCreditNeedsReview)
				require.NoError(t, db.First(&uncertain, uncertain.ID).Error)
				assert.Equal(t, "review", uncertain.State)
			})
			t.Run("platform_shortfall_never_collects_future_topups", func(t *testing.T) {
				user := creditTestUser(t, db, "shortfall")
				require.NoError(t, db.Model(&user).Update("accounting_version", 1).Error)
				grant := CreditGrant{UserID: user.Id, SourceType: "topup", SourceID: "initial", Amount: 30, StartsAt: 1, ExpiresAt: 200, UseMask: CreditUseAPI}
				pack, err := GrantCreditPack(db, grant, 100)
				require.NoError(t, err)
				token := Token{UserId: user.Id, Key: "shortfall-token", RemainQuota: 1000}
				require.NoError(t, db.Create(&token).Error)
				input := CreditRequestInput{UserID: user.Id, RequestID: "bill", ModelName: "model", Protocol: "openai", PriceSnapshot: `{"price":1}`, TokenID: token.Id, Amount: 20}
				request, err := BeginCreditRequest(db, input, 100)
				require.NoError(t, err)
				completed, err := FinishCreditRequest(db, user.Id, request.ID, "settle", 50, 100)
				require.NoError(t, err)
				assert.EqualValues(t, 50, completed.Actual)
				assert.EqualValues(t, 30, completed.Charged)
				assert.EqualValues(t, 20, completed.Uncollected)
				replayed, err := FinishCreditRequest(db, user.Id, request.ID, "settle", 50, 101)
				require.NoError(t, err)
				assert.Equal(t, completed, replayed)
				require.NoError(t, db.First(&pack, pack.ID).Error)
				assert.Zero(t, pack.Available)
				assert.EqualValues(t, 30, pack.Spent)
				input.RequestID, input.Amount = "blocked", 1
				_, err = BeginCreditRequest(db, input, 100)
				assert.ErrorIs(t, err, ErrCreditInsufficient)
				input.RequestID, input.Amount = "zero_estimate_is_still_paid", 0
				_, err = BeginCreditRequest(db, input, 100)
				assert.ErrorIs(t, err, ErrCreditInsufficient)
				input.RequestID, input.Free = "explicit_free", true
				free, err := BeginCreditRequest(db, input, 100)
				require.NoError(t, err)
				_, err = FinishCreditRequest(db, user.Id, free.ID, "settle", 0, 100)
				require.NoError(t, err)
				grant.SourceID, grant.Amount = "new-topup", 10
				newPack, err := GrantCreditPack(db, grant, 100)
				require.NoError(t, err)
				_, err = GrantCreditPack(db, grant, 100)
				require.NoError(t, err)
				require.NoError(t, db.First(&newPack, newPack.ID).Error)
				assert.EqualValues(t, 10, newPack.Available, "recharge is entirely available; no old bill may collect it")
				assert.Zero(t, newPack.Spent)
				require.NoError(t, db.First(&token, token.Id).Error)
				assert.Equal(t, 970, token.RemainQuota)
				assert.Equal(t, 30, token.UsedQuota)
				require.NoError(t, db.First(&user, user.Id).Error)
				assert.Equal(t, 30, user.UsedQuota)
				var projection CreditLogOutbox
				require.NoError(t, db.Where("request_id = ?", request.ID).First(&projection).Error)
				var log Log
				require.NoError(t, common.UnmarshalJsonStr(projection.Payload, &log))
				assert.Equal(t, 30, log.Quota, "user log shows the fee actually paid")
				var other map[string]any
				require.NoError(t, common.UnmarshalJsonStr(log.Other, &other))
				assert.EqualValues(t, 20, other["uncollected_quota"])
				input.RequestID, input.Amount, input.Free = "after-topup", 1, false
				_, err = BeginCreditRequest(db, input, 100)
				require.NoError(t, err, "new usage resumes when funds exist, regardless of platform shortfall")
				differences, err := ReconcileCreditAccount(db, user.Id)
				require.NoError(t, err)
				assert.Empty(t, differences)
			})
			t.Run("supplementary_charge_respects_api_key_limit", func(t *testing.T) {
				user := creditTestUser(t, db, "key-shortfall")
				require.NoError(t, db.Model(&user).Update("accounting_version", 1).Error)
				pack, err := GrantCreditPack(db, CreditGrant{UserID: user.Id, SourceType: "test", SourceID: "funds", Amount: 100, StartsAt: 1, ExpiresAt: 200, UseMask: CreditUseAPI}, 100)
				require.NoError(t, err)
				token := Token{UserId: user.Id, Key: "shortfall-limited", RemainQuota: 25}
				require.NoError(t, db.Create(&token).Error)
				request, err := BeginCreditRequest(db, CreditRequestInput{UserID: user.Id, RequestID: "key-bill", ModelName: "model", Protocol: "openai", PriceSnapshot: `{}`, TokenID: token.Id, Amount: 20}, 100)
				require.NoError(t, err)
				completed, err := FinishCreditRequest(db, user.Id, request.ID, "settle", 50, 100)
				require.NoError(t, err)
				assert.EqualValues(t, 25, completed.Charged)
				assert.EqualValues(t, 25, completed.Uncollected)
				require.NoError(t, db.First(&token, token.Id).Error)
				assert.Zero(t, token.RemainQuota)
				assert.Equal(t, 25, token.UsedQuota)
				require.NoError(t, db.First(&pack, pack.ID).Error)
				assert.EqualValues(t, 75, pack.Available)
				assert.EqualValues(t, 25, pack.Spent)
			})
			t.Run("concurrent_finish_has_one_terminal_result", func(t *testing.T) {
				user := creditTestUser(t, db, "finish-race")
				require.NoError(t, db.Model(&user).Update("accounting_version", 1).Error)
				pack, err := GrantCreditPack(db, CreditGrant{UserID: user.Id, SourceType: "test", SourceID: "funds", Amount: 50, StartsAt: 1, ExpiresAt: 200, UseMask: CreditUseAPI}, 100)
				require.NoError(t, err)
				request, err := BeginCreditRequest(db, CreditRequestInput{UserID: user.Id, RequestID: "finish-race", ModelName: "model", Protocol: "openai", PriceSnapshot: `{}`, Playground: true, Amount: 30}, 100)
				require.NoError(t, err)
				start := make(chan struct{})
				results := make(chan error, 2)
				ready, release := make(chan struct{}, 2), make(chan struct{})
				var arrivals atomic.Int32
				require.NoError(t, db.Callback().Update().Before("gorm:update").Register("finish_barrier", func(tx *gorm.DB) {
					if tx.Statement.Schema != nil && tx.Statement.Schema.Name == "CreditAccount" && arrivals.Add(1) <= 2 {
						ready <- struct{}{}
						<-release
					}
				}))
				var group sync.WaitGroup
				for _, kind := range []string{"settle", "release"} {
					group.Go(func() {
						<-start
						actual := int64(0)
						if kind == "settle" {
							actual = 20
						}
						_, err := FinishCreditRequest(db, user.Id, request.ID, kind, actual, 100)
						results <- err
					})
				}
				close(start)
				<-ready
				<-ready
				assert.GreaterOrEqual(t, sqlDB.Stats().InUse, 2)
				close(release)
				group.Wait()
				require.NoError(t, db.Callback().Update().Remove("finish_barrier"))
				close(results)
				var success, conflict int
				for err := range results {
					if err == nil {
						success++
					} else if errors.Is(err, ErrCreditOperationConflict) {
						conflict++
					} else {
						require.NoError(t, err)
					}
				}
				assert.Equal(t, 1, success)
				assert.Equal(t, 1, conflict)
				require.NoError(t, db.First(&request, request.ID).Error)
				require.NoError(t, db.First(&pack, pack.ID).Error)
				assert.Contains(t, []string{"settled", "released"}, request.State)
				assert.Zero(t, pack.Held)
				assert.EqualValues(t, 50, pack.Available+pack.Spent)
			})
			t.Run("credit_account_rejects_legacy_wallet_and_key_writers", func(t *testing.T) {
				previousDB := DB
				DB = db
				t.Cleanup(func() { DB = previousDB })
				user := creditTestUser(t, db, "legacy-guards")
				require.NoError(t, db.Model(&user).Updates(map[string]any{"accounting_version": 1, "quota": 73}).Error)
				token := Token{UserId: user.Id, Key: "guard-key", RemainQuota: 100, UnlimitedQuota: true}
				require.NoError(t, db.Create(&token).Error)
				ok, err := TryReserveUserQuota(user.Id, 0)
				assert.False(t, ok)
				assert.ErrorIs(t, err, ErrCreditOperationRequired)
				_, err = TryReserveTokenQuota(token.Id, token.Key, 1, true)
				assert.ErrorIs(t, err, ErrCreditOperationRequired)
				assert.ErrorIs(t, IncreaseUserQuota(user.Id, 1, true), ErrCreditOperationRequired)
				assert.ErrorIs(t, DecreaseUserQuota(user.Id, 1, true), ErrCreditOperationRequired)
				assert.ErrorIs(t, IncreaseTokenQuota(token.Id, token.Key, 1), ErrCreditOperationRequired)
				assert.ErrorIs(t, DecreaseTokenQuota(token.Id, token.Key, 1), ErrCreditOperationRequired)
				require.NoError(t, db.First(&user, user.Id).Error)
				require.NoError(t, db.First(&token, token.Id).Error)
				assert.Equal(t, 73, user.Quota)
				assert.Equal(t, 100, token.RemainQuota)
				assert.Zero(t, token.UsedQuota)
				require.NoError(t, db.Model(&User{}).Where("id = ?", user.Id).Update("accounting_version", 2).Error)
				user.DisplayName = "profile update"
				require.NoError(t, user.Update(false))
				assert.Equal(t, 2, user.AccountingVersion, "generic profile updates cannot change accounting mode")
			})
			t.Run("payment_callbacks_use_locked_credit_policy", func(t *testing.T) {
				previousDB, previousLog, previousRedis, previousUnit := DB, LOG_DB, common.RedisEnabled, common.QuotaPerUnit
				DB, LOG_DB, common.RedisEnabled, common.QuotaPerUnit = db, db, false, 100
				t.Cleanup(func() {
					DB, LOG_DB, common.RedisEnabled, common.QuotaPerUnit = previousDB, previousLog, previousRedis, previousUnit
				})
				policy, err := PutCreditSourcePolicy(db, CreditSourcePolicy{SourceType: "topup", DurationSeconds: 3600, UseMask: CreditUseAPI | CreditUseSubscription}, 0)
				require.NoError(t, err)
				for _, provider := range []string{PaymentProviderEpay, PaymentProviderStripe, PaymentProviderCreem, PaymentProviderWaffo, PaymentProviderWaffoPancake} {
					t.Run(provider, func(t *testing.T) {
						user := creditTestUser(t, db, "paid-"+provider)
						require.NoError(t, db.Model(&user).Updates(map[string]any{"accounting_version": 1, "quota": 73}).Error)
						order := TopUp{UserId: user.Id, Amount: 2, Money: 3, TradeNo: "credit-" + provider, PaymentProvider: provider, Status: common.TopUpStatusPending, CreateTime: common.GetTimestamp()}
						lockedDuration, lockedMask := policy.DurationSeconds, policy.UseMask
						require.NoError(t, order.Insert())
						policy.DurationSeconds, policy.UseMask = 7200, CreditUseAPI
						policy, err = PutCreditSourcePolicy(db, policy, policy.Revision)
						require.NoError(t, err)
						callback := func() error {
							switch provider {
							case PaymentProviderEpay:
								_, err := RechargeEpay(order.TradeNo, "", "")
								return err
							case PaymentProviderStripe:
								return Recharge(order.TradeNo, "customer", "")
							case PaymentProviderCreem:
								return RechargeCreem(order.TradeNo, "", "", "")
							case PaymentProviderWaffo:
								return RechargeWaffo(order.TradeNo, "")
							default:
								return RechargeWaffoPancake(order.TradeNo)
							}
						}
						common.QuotaPerUnit = 200
						require.NoError(t, db.Callback().Create().Before("gorm:create").Register("payment-ledger-failure", func(tx *gorm.DB) {
							if tx.Statement.Schema != nil && tx.Statement.Schema.Name == "CreditLedgerEntry" {
								tx.AddError(errors.New("payment ledger unavailable"))
							}
						}))
						require.Error(t, callback())
						require.NoError(t, db.Callback().Create().Remove("payment-ledger-failure"))
						var failed TopUp
						require.NoError(t, db.First(&failed, order.Id).Error)
						assert.Equal(t, common.TopUpStatusPending, failed.Status, "order completion rolls back with issuance")
						var packCount int64
						require.NoError(t, db.Model(&CreditPack{}).Where("user_id = ?", user.Id).Count(&packCount).Error)
						assert.Zero(t, packCount)
						start := make(chan struct{})
						results := make(chan error, 2)
						var workers sync.WaitGroup
						for range 2 {
							workers.Go(func() {
								<-start
								results <- callback()
							})
						}
						close(start)
						workers.Wait()
						for range 2 {
							assert.NoError(t, <-results, "independent simultaneous callbacks acknowledge one durable issuance")
						}
						require.NoError(t, callback())
						common.QuotaPerUnit = 100
						require.NoError(t, db.First(&order, order.Id).Error)
						assert.Equal(t, lockedDuration, order.CreditDurationSeconds)
						assert.Equal(t, lockedMask, order.CreditUseMask)
						packs, err := ListCreditPacks(db, user.Id, common.GetTimestamp())
						require.NoError(t, err)
						require.Len(t, packs, 1)
						want := int64(200)
						if provider == PaymentProviderStripe {
							want = 300
						} else if provider == PaymentProviderCreem {
							want = 2
						}
						assert.Equal(t, want, packs[0].Issued, "checkout freezes quota conversion")
						assert.Equal(t, order.CompleteTime+order.CreditDurationSeconds, packs[0].ExpiresAt)
						assert.Equal(t, order.CreditUseMask, packs[0].UseMask)
						require.NoError(t, db.First(&user, user.Id).Error)
						assert.Equal(t, 73, user.Quota)
					})
				}
			})
			t.Run("checkin_and_redemption_source_failures_roll_back", func(t *testing.T) {
				previousDB, previousLog, previousRedis := DB, LOG_DB, common.RedisEnabled
				DB, LOG_DB, common.RedisEnabled = db, db, false
				t.Cleanup(func() { DB, LOG_DB, common.RedisEnabled = previousDB, previousLog, previousRedis })
				for _, source := range []string{"checkin", "redemption"} {
					_, err := PutCreditSourcePolicy(db, CreditSourcePolicy{SourceType: source, DurationSeconds: 3600, UseMask: CreditUseAPI}, 0)
					require.NoError(t, err)
				}
				user := creditTestUser(t, db, "source-failure")
				require.NoError(t, db.Model(&user).Update("accounting_version", 1).Error)
				code := Redemption{Key: "credit-source-code", Quota: 20, Status: common.RedemptionCodeStatusEnabled}
				require.NoError(t, code.Insert())
				require.NoError(t, db.Callback().Create().Before("gorm:create").Register("source-ledger-failure", func(tx *gorm.DB) {
					if tx.Statement.Schema != nil && tx.Statement.Schema.Name == "CreditLedgerEntry" {
						tx.AddError(errors.New("source ledger unavailable"))
					}
				}))
				_, err := Redeem(code.Key, user.Id)
				require.Error(t, err)
				checkin := Checkin{UserId: user.Id, CheckinDate: "2026-10-07", QuotaAwarded: 10, CreatedAt: 100}
				_, err = userCheckinWithTransaction(&checkin, user.Id, 10)
				require.Error(t, err)
				require.NoError(t, db.Callback().Create().Remove("source-ledger-failure"))
				require.NoError(t, db.First(&code, code.Id).Error)
				assert.Equal(t, common.RedemptionCodeStatusEnabled, code.Status)
				var count int64
				require.NoError(t, db.Model(&Checkin{}).Where("user_id = ?", user.Id).Count(&count).Error)
				assert.Zero(t, count)
				amount, err := Redeem(code.Key, user.Id)
				require.NoError(t, err)
				assert.Equal(t, 20, amount)
				require.NoError(t, db.First(&code, code.Id).Error)
				code.Status, code.Quota = common.RedemptionCodeStatusEnabled, 99
				assert.ErrorIs(t, code.Update(), ErrCreditOperationConflict, "used source cannot be rewritten or made redeemable again")
				setting := operation_setting.GetCheckinSetting()
				previous := *setting
				*setting = operation_setting.CheckinSetting{Enabled: true, MinQuota: 10, MaxQuota: 10}
				t.Cleanup(func() { *setting = previous })
				_, err = UserCheckin(user.Id)
				require.NoError(t, err)
				_, err = UserCheckin(user.Id)
				require.Error(t, err)
				packs, err := ListCreditPacks(db, user.Id, common.GetTimestamp())
				require.NoError(t, err)
				require.Len(t, packs, 2)
				assert.EqualValues(t, 30, packs[0].Issued+packs[1].Issued)
				zero := creditTestUser(t, db, "zero-checkin")
				require.NoError(t, db.Model(&zero).Update("accounting_version", 1).Error)
				for _, bounds := range [][2]int{{-1, 0}, {2, 1}, {0, common.MaxWalletQuota + 1}} {
					setting.MinQuota, setting.MaxQuota = bounds[0], bounds[1]
					_, err := UserCheckin(zero.Id)
					assert.ErrorIs(t, err, ErrCreditInvalid, "invalid reward configuration cannot reach random selection or issuance")
				}
				setting.MinQuota, setting.MaxQuota = 0, 0
				result, err := UserCheckin(zero.Id)
				require.NoError(t, err)
				assert.Zero(t, result.QuotaAwarded)
				packs, err = ListCreditPacks(db, zero.Id, common.GetTimestamp())
				require.NoError(t, err)
				assert.Empty(t, packs, "zero reward records attendance without minting a zero-value pack")
			})
			t.Run("registration_rewards_commit_with_user_and_replay_safely", func(t *testing.T) {
				previousDB, previousLog, previousRedis := DB, LOG_DB, common.RedisEnabled
				previousNew, previousInvitee, previousInviter := common.QuotaForNewUser, common.QuotaForInvitee, common.QuotaForInviter
				payment := operation_setting.GetPaymentSetting()
				previousPayment := *payment
				DB, LOG_DB, common.RedisEnabled = db, db, false
				common.QuotaForNewUser, common.QuotaForInvitee, common.QuotaForInviter = 10, 5, 7
				payment.ComplianceConfirmed, payment.ComplianceTermsVersion = true, operation_setting.CurrentComplianceTermsVersion
				t.Cleanup(func() {
					DB, LOG_DB, common.RedisEnabled = previousDB, previousLog, previousRedis
					common.QuotaForNewUser, common.QuotaForInvitee, common.QuotaForInviter = previousNew, previousInvitee, previousInviter
					*payment = previousPayment
				})
				for _, source := range []string{"signup", "invitee", "inviter"} {
					_, err := PutCreditSourcePolicy(db, CreditSourcePolicy{SourceType: source, DurationSeconds: 3600, UseMask: CreditUseAPI}, 0)
					require.NoError(t, err)
				}
				inviter := creditTestUser(t, db, "credit-inviter")
				require.NoError(t, db.Model(&inviter).Update("accounting_version", 1).Error)
				require.NoError(t, db.Create(&Option{Key: "CreditNewUserAccountingVersion", Value: "1"}).Error)
				t.Cleanup(func() {
					require.NoError(t, db.Where(&Option{Key: "CreditNewUserAccountingVersion"}).Delete(&Option{}).Error)
				})
				child := User{Username: "credit-new-user"}
				require.NoError(t, child.Insert(inviter.Id))
				child.FinishInsert(inviter.Id)
				child.FinalizeOAuthUserCreation(inviter.Id)
				for _, user := range []struct {
					id, count int
					amount    int64
				}{{child.Id, 2, 15}, {inviter.Id, 1, 7}} {
					packs, err := ListCreditPacks(db, user.id, common.GetTimestamp())
					require.NoError(t, err)
					require.Len(t, packs, user.count)
					var amount int64
					for _, pack := range packs {
						amount += pack.Issued
					}
					assert.Equal(t, user.amount, amount)
				}
				require.NoError(t, db.First(&child, child.Id).Error)
				assert.Equal(t, 1, child.AccountingVersion, "ordinary creation reads the shared deployment policy")
				assert.Zero(t, child.Quota)
				require.NoError(t, db.First(&inviter, inviter.Id).Error)
				assert.Zero(t, inviter.AffQuota)
				assert.Equal(t, 1, inviter.AffCount)
				require.NoError(t, db.Callback().Create().Before("gorm:create").Register("signup-ledger-failure", func(tx *gorm.DB) {
					if tx.Statement.Schema != nil && tx.Statement.Schema.Name == "CreditLedgerEntry" {
						tx.AddError(errors.New("signup ledger unavailable"))
					}
				}))
				failed := User{Username: "rolled-back-signup", AccountingVersion: 1}
				require.Error(t, db.Transaction(func(tx *gorm.DB) error { return failed.InsertWithTx(tx, inviter.Id) }))
				require.NoError(t, db.Callback().Create().Remove("signup-ledger-failure"))
				var count int64
				require.NoError(t, db.Model(&User{}).Where("username = ?", failed.Username).Count(&count).Error)
				assert.Zero(t, count, "financial failure cannot leave a user without the promised reward")
			})
			t.Run("log_cleanup_preserves_ledger_and_old_event_defences", func(t *testing.T) {
				require.NoError(t, db.AutoMigrate(&Log{}, &CreditLogDelivery{}))
				oldDB, oldLogs, oldLogType := DB, LOG_DB, common.LogDatabaseType()
				DB, LOG_DB = db, db
				common.SetLogDatabaseType(common.DatabaseType(dialect))
				defer func() { DB, LOG_DB = oldDB, oldLogs; common.SetLogDatabaseType(oldLogType) }()
				user := creditTestUser(t, db, "retained-ledger")
				require.NoError(t, db.Model(&user).Update("accounting_version", 1).Error)
				grant := CreditGrant{UserID: user.Id, SourceType: "test", SourceID: "retained-event", Amount: 100, StartsAt: 1, ExpiresAt: 500, UseMask: CreditUseAPI}
				pack, err := GrantCreditPack(db, grant, 100)
				require.NoError(t, err)
				input := CreditRequestInput{UserID: user.Id, RequestID: "retained-request", ModelName: "model", Protocol: "openai", PriceSnapshot: `{}`, Playground: true, Amount: 40}
				request, err := BeginCreditRequest(db, input, 100)
				require.NoError(t, err)
				_, err = FinishCreditRequest(db, user.Id, request.ID, "settle", 25, 100)
				require.NoError(t, err)
				var outbox CreditLogOutbox
				require.NoError(t, db.Where("request_id = ?", request.ID).First(&outbox).Error)
				require.NoError(t, DeliverCreditLog(db, db, outbox.ID, 101))
				removed, err := DeleteOldLogBatch(context.Background(), 102, 100)
				require.NoError(t, err)
				assert.Positive(t, removed)
				replay, err := GrantCreditPack(db, grant, 103)
				require.NoError(t, err)
				assert.Equal(t, pack.ID, replay.ID)
				requestReplay, err := BeginCreditRequest(db, input, 103)
				require.NoError(t, err)
				assert.Equal(t, request.ID, requestReplay.ID)
				_, err = FinishCreditRequest(db, user.Id, request.ID, "settle", 25, 103)
				require.NoError(t, err)
				require.NoError(t, DeliverCreditLog(db, db, outbox.ID, 103), "a delivery tombstone survives usage-log retention")
				var count int64
				require.NoError(t, db.Model(&Log{}).Where("user_id = ?", user.Id).Count(&count).Error)
				assert.Zero(t, count)
				require.NoError(t, db.Model(&CreditLogDelivery{}).Where("event_digest <> ?", "").Count(&count).Error)
				assert.Positive(t, count)
				require.NoError(t, db.First(&pack, pack.ID).Error)
				assert.EqualValues(t, 75, pack.Available)
				assert.EqualValues(t, 25, pack.Spent)
				assert.Zero(t, pack.Held)
				balance, err := GetCreditBillBalance(db, user.Id, request.ID)
				require.NoError(t, err)
				assert.EqualValues(t, 25, balance.Charged)
				differences, err := ReconcileCreditAccount(db, user.Id)
				require.NoError(t, err)
				assert.Empty(t, differences)
			})
			t.Run("deployment_mode_persists_across_instances", func(t *testing.T) {
				require.NoError(t, InitializeCreditNewUserMode(db))
				var policy Option
				require.NoError(t, db.Where(&Option{Key: creditNewUserModeKey}).First(&policy).Error)
				assert.Equal(t, "0", policy.Value, "an existing development database is not silently converted")
				peer, err := gorm.Open(driver, &gorm.Config{NamingStrategy: schema.NamingStrategy{TablePrefix: "credit_test_"}})
				require.NoError(t, err)
				peerSQL, err := peer.DB()
				require.NoError(t, err)
				defer peerSQL.Close()
				require.NoError(t, peer.Model(&Option{}).Where(&Option{Key: creditNewUserModeKey}).Update("value", "1").Error)
				require.NoError(t, InitializeCreditNewUserMode(db), "a later startup cannot overwrite the stored policy")
				child := User{Username: "mode-policy-user"}
				require.NoError(t, peer.Transaction(func(tx *gorm.DB) error { return child.InsertWithTx(tx, 0) }))
				require.NoError(t, db.First(&child, child.Id).Error)
				assert.Equal(t, 1, child.AccountingVersion)
				assert.Zero(t, child.Quota)
				var account CreditAccount
				require.NoError(t, db.First(&account, "user_id = ?", child.Id).Error)
				require.NoError(t, db.First(&saved, legacy.Id).Error)
				assert.Zero(t, saved.AccountingVersion)
				assert.Equal(t, 73, saved.Quota)
				require.NoError(t, peer.Model(&Option{}).Where(&Option{Key: creditNewUserModeKey}).Update("value", "2").Error)
				invalid := User{Username: "invalid-policy-user"}
				assert.ErrorIs(t, peer.Transaction(func(tx *gorm.DB) error { return invalid.InsertWithTx(tx, 0) }), ErrCreditInvariant)
				var count int64
				require.NoError(t, db.Model(&User{}).Where("username = ?", invalid.Username).Count(&count).Error)
				assert.Zero(t, count)
				require.NoError(t, peer.Model(&Option{}).Where(&Option{Key: creditNewUserModeKey}).Update("value", "0").Error)
				bad := User{Username: "invalid-mode-user", AccountingVersion: 2}
				assert.ErrorIs(t, peer.Transaction(func(tx *gorm.DB) error { return bad.InsertWithTx(tx, 0) }), ErrCreditOperationRequired)
				require.NoError(t, db.Where(&Option{Key: creditNewUserModeKey}).Delete(&Option{}).Error)
			})
			t.Run("admin_grant_and_refund_review_preserve_source", func(t *testing.T) {
				user := creditTestUser(t, db, "admin-credit-target")
				require.NoError(t, db.Model(&user).Update("accounting_version", 1).Error)
				actor := creditTestUser(t, db, "credit-admin")
				require.NoError(t, db.Model(&actor).Update("role", common.RoleAdminUser).Error)
				grant := CreditGrant{UserID: user.Id, SourceID: "admin-event", Amount: 100, StartsAt: 100, ExpiresAt: 200, UseMask: CreditUseAPI, ActorID: actor.Id, Reason: "operating reward"}
				pack, err := GrantAdminCredit(db, grant, 100)
				require.NoError(t, err)
				_, err = GrantAdminCredit(db, grant, 100)
				require.NoError(t, err)
				grant.ActorID = user.Id
				_, err = GrantAdminCredit(db, grant, 100)
				assert.ErrorIs(t, err, ErrUserQuotaPermission)
				used, err := ReserveCreditPacks(db, CreditReserve{UserID: user.Id, RequestID: "used", Amount: 20, Purpose: CreditUseAPI}, 100)
				require.NoError(t, err)
				_, err = FinalizeCreditReservation(db, CreditFinalize{UserID: user.Id, ReservationID: used.OperationID, Kind: "settle", Actual: 20}, 100)
				require.NoError(t, err)
				inFlight, err := ReserveCreditPacks(db, CreditReserve{UserID: user.Id, RequestID: "in-flight", Amount: 30, Purpose: CreditUseAPI}, 100)
				require.NoError(t, err)
				input := CreditReviewInput{UserID: user.Id, PackID: pack.ID, EventID: "purchase-refund", ActorID: actor.Id, Reason: "payment reversal requires verification"}
				review, err := OpenCreditReviewCase(db, input, 100)
				require.NoError(t, err)
				replayed, err := OpenCreditReviewCase(db, input, 100)
				require.NoError(t, err)
				assert.Equal(t, review.ID, replayed.ID)
				other := creditTestUser(t, db, "review-other-owner")
				_, err = GrantCreditPack(db, CreditGrant{UserID: other.Id, SourceType: "test", SourceID: "review-other", Amount: 1, StartsAt: 100, ExpiresAt: 200, UseMask: CreditUseAPI}, 100)
				require.NoError(t, err)
				wrongOwner := input
				wrongOwner.UserID = other.Id
				_, err = OpenCreditReviewCase(db, wrongOwner, 100)
				assert.ErrorIs(t, err, gorm.ErrRecordNotFound, "cannot freeze a pack through a different user's account")
				_, err = ReserveCreditPacks(db, CreditReserve{UserID: user.Id, RequestID: "blocked-review", Amount: 1, Purpose: CreditUseAPI}, 100)
				assert.ErrorIs(t, err, ErrCreditInsufficient)
				_, err = FinalizeCreditReservation(db, CreditFinalize{UserID: user.Id, ReservationID: inFlight.OperationID, Kind: "settle", Actual: 10}, 100)
				require.NoError(t, err, "opening a review cannot erase the existing request's source")
				require.NoError(t, db.First(&pack, pack.ID).Error)
				assert.EqualValues(t, 30, pack.Spent)
				assert.Zero(t, pack.Held)
				assert.NotZero(t, pack.BlockedAt)
				outcome := CreditCashOutcome{UserID: user.Id, CaseID: review.ID, ActorID: actor.Id, State: "unknown", Evidence: "provider temporarily unavailable"}
				require.NoError(t, RecordCreditCashOutcome(db, outcome, 100))
				pending := outcome
				wrongCase := outcome
				wrongCase.UserID = other.Id
				assert.ErrorIs(t, RecordCreditCashOutcome(db, wrongCase, 100), gorm.ErrRecordNotFound)
				outcome.State, outcome.Reference, outcome.Evidence = "confirmed", "external-refund-id", "provider record verified"
				require.NoError(t, RecordCreditCashOutcome(db, outcome, 101))
				require.NoError(t, RecordCreditCashOutcome(db, outcome, 102))
				require.NoError(t, RecordCreditCashOutcome(db, pending, 102), "late replay cannot replace the confirmed outcome")
				outcome.State = "rejected"
				assert.ErrorIs(t, RecordCreditCashOutcome(db, outcome, 102), ErrCreditOperationConflict)
				require.NoError(t, db.First(&review, review.ID).Error)
				assert.Equal(t, "confirmed", review.CashState)
				assert.Equal(t, "external-refund-id", review.CashReference)
				var evidence []CreditCashEvidence
				require.NoError(t, db.Where("case_id = ?", review.ID).Order("id").Find(&evidence).Error)
				require.Len(t, evidence, 2, "each distinct cash result preserves its evidence; repeats add nothing")
				assert.Equal(t, "provider temporarily unavailable", evidence[0].Evidence)
				assert.Equal(t, "provider record verified", evidence[1].Evidence)
			})
			var packs []CreditPack
			require.NoError(t, db.Find(&packs).Error)
			for _, pack := range packs {
				assert.True(t, pack.QuantitiesValid())
				var entries []CreditLedgerEntry
				require.NoError(t, db.Where("pack_id = ?", pack.ID).Find(&entries).Error)
				var issued, available, held, spent, expired, revoked int64
				for _, entry := range entries {
					issued += entry.Issued
					available += entry.Available
					held += entry.Held
					spent += entry.Spent
					expired += entry.Expired
					revoked += entry.Revoked
				}
				assert.Equal(t, []int64{pack.Issued, pack.Available, pack.Held, pack.Spent, pack.Expired, pack.Revoked}, []int64{issued, available, held, spent, expired, revoked}, "pack state must reconcile with durable movements")
			}
		})
	}
}

func creditTestUser(t *testing.T, db *gorm.DB, name string) User {
	t.Helper()
	user := User{Username: name, Password: "unused", AffCode: name}
	require.NoError(t, db.Create(&user).Error)
	return user
}

// A separate test binary exits after the intent committed and the financial
// transaction failed. The parent reopens durable state; no session is shared.
func TestCreditRecoveryProcessCheckpoint(t *testing.T) {
	dialect, dsn := os.Getenv("NEW_API_CREDIT_CHECKPOINT_DB"), os.Getenv("NEW_API_CREDIT_CHECKPOINT_DSN")
	if dialect == "" {
		t.Skip("child-process checkpoint only")
	}
	var driver gorm.Dialector
	switch dialect {
	case "sqlite":
		driver = sqlite.Open(dsn)
	case "mysql":
		driver = mysqlMigrationDialector{mysql.Dialector{Config: &mysql.Config{DSN: dsn}}}
	case "postgres":
		driver = postgresMigrationDialector{postgres.Dialector{Config: &postgres.Config{DSN: dsn}}}
	default:
		t.Fatal("invalid checkpoint dialect")
	}
	common.SetMainDatabaseType(common.DatabaseType(dialect))
	db, err := gorm.Open(driver, &gorm.Config{NamingStrategy: schema.NamingStrategy{TablePrefix: "credit_test_"}})
	require.NoError(t, err)
	userID, err := strconv.Atoi(os.Getenv("NEW_API_CREDIT_CHECKPOINT_USER"))
	require.NoError(t, err)
	tokenID, err := strconv.Atoi(os.Getenv("NEW_API_CREDIT_CHECKPOINT_TOKEN"))
	require.NoError(t, err)
	stage := os.Getenv("NEW_API_CREDIT_CHECKPOINT_STAGE")
	if stage == "competing_claim" {
		requestID, err := strconv.ParseInt(os.Getenv("NEW_API_CREDIT_CHECKPOINT_REQUEST"), 10, 64)
		require.NoError(t, err)
		_, err = fmt.Fprintln(os.Stdout, "credit-claim:ready")
		require.NoError(t, err)
		var signal [1]byte
		_, err = io.ReadFull(os.Stdin, signal[:])
		require.NoError(t, err)
		lease, err := ClaimCreditExecution(db, userID, requestID, os.Getenv("NEW_API_CREDIT_CHECKPOINT_OWNER"), 10, 111)
		if errors.Is(err, ErrCreditLeaseLost) {
			_, err = fmt.Fprintln(os.Stdout, "credit-claim:lost")
			require.NoError(t, err)
			return
		}
		require.NoError(t, err)
		_, err = fmt.Fprintln(os.Stdout, "credit-claim:won")
		require.NoError(t, err)
		_, err = io.ReadFull(os.Stdin, signal[:])
		require.NoError(t, err)
		for range 2 {
			_, err = FinishCreditRequest(db, userID, requestID, "settle", 35, 112, lease)
			require.NoError(t, err)
		}
		return
	}
	if stage == "reserve_transaction" {
		require.NoError(t, db.Callback().Create().Before("gorm:create").Register("process:reserve-barrier", func(tx *gorm.DB) {
			if tx.Statement.Schema != nil && tx.Statement.Schema.Name == "CreditAllocation" {
				creditProcessBarrier(t, stage)
			}
		}))
	}
	request, err := BeginCreditRequest(db, CreditRequestInput{UserID: userID, RequestID: "process-checkpoint", ModelName: "model", Protocol: "openai", PriceSnapshot: `{}`, TokenID: tokenID, Amount: 40}, 100)
	require.NoError(t, err)
	if stage == "reserved" {
		creditProcessBarrier(t, stage)
	}
	lease, err := ClaimCreditExecution(db, userID, request.ID, "exiting-worker", 10, 100)
	require.NoError(t, err)
	require.NoError(t, MarkCreditRequestSubmitted(db, userID, request.ID, 100, lease))
	if stage == "submitted" {
		creditProcessBarrier(t, stage)
	}
	if stage == "financial_transaction" || stage == "settled" {
		if stage == "financial_transaction" {
			require.NoError(t, db.Callback().Update().After("gorm:update").Register("process:financial-barrier", func(tx *gorm.DB) {
				if tx.Statement.Schema != nil && tx.Statement.Schema.Name == "Token" {
					creditProcessBarrier(t, stage)
				}
			}))
		}
		_, err = FinishCreditRequest(db, userID, request.ID, "settle", 35, 101, lease)
		require.NoError(t, err)
		creditProcessBarrier(t, stage)
	}
	require.NoError(t, db.Callback().Update().Before("gorm:update").Register("process:financial-failure", func(tx *gorm.DB) {
		if tx.Statement.Schema != nil && tx.Statement.Schema.Name == "Token" {
			tx.AddError(errors.New("simulated failed token write"))
		}
	}))
	_, err = FinishCreditRequest(db, userID, request.ID, "settle", 35, 101, lease)
	require.Error(t, err)
	os.Exit(23)
}

// The parent waits for a precise committed/uncommitted checkpoint and sends
// SIGKILL. Stdin blocks in I/O without timing sleeps or a graceful shutdown.
func creditProcessBarrier(t *testing.T, stage string) {
	t.Helper()
	_, err := fmt.Fprintln(os.Stdout, "credit-checkpoint:"+stage)
	require.NoError(t, err)
	var input [1]byte
	_, err = os.Stdin.Read(input[:])
	t.Fatalf("checkpoint unexpectedly released: %v", err)
}

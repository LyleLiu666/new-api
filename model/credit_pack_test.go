package model

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/setting/operation_setting"
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
				require.NoError(t, db.Migrator().DropTable(&CreditRequestReservation{}, &CreditCashEvidence{}, &CreditReviewCase{}, &CreditDebt{}, &CreditRequest{}, &CreditLedgerEntry{}, &CreditAllocation{}, &CreditOperation{}, &CreditPack{}, &CreditAccount{}, &CreditSourcePolicy{}, &TopUp{}, &Redemption{}, &Checkin{}, &Log{}, &Token{}, &User{}))
				require.NoError(t, sqlDB.Close())
				common.SetMainDatabaseType(previousType)
			})
			require.NoError(t, db.AutoMigrate(&User{}))
			legacy := User{Username: "legacy", Quota: 73, Password: "unused", AffCode: "legacy"}
			require.NoError(t, db.Create(&legacy).Error)
			require.NoError(t, db.Migrator().DropColumn(&User{}, "AccountingVersion"))
			require.NoError(t, db.AutoMigrate(&User{}, &Token{}))
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
				assert.Zero(t, settled.Unpaid)
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
			t.Run("unpaid_bill_and_topup_repayment", func(t *testing.T) {
				user := creditTestUser(t, db, "unpaid")
				require.NoError(t, db.Model(&user).Update("accounting_version", 1).Error)
				grant := CreditGrant{UserID: user.Id, SourceType: "topup", SourceID: "initial", Amount: 30, StartsAt: 1, ExpiresAt: 200, UseMask: CreditUseAPI}
				pack, err := GrantCreditPack(db, grant, 100)
				require.NoError(t, err)
				token := Token{UserId: user.Id, Key: "unpaid-token", RemainQuota: 1000}
				require.NoError(t, db.Create(&token).Error)
				input := CreditRequestInput{UserID: user.Id, RequestID: "bill", ModelName: "model", Protocol: "openai", PriceSnapshot: `{"price":1}`, TokenID: token.Id, Amount: 20}
				request, err := BeginCreditRequest(db, input, 100)
				require.NoError(t, err)
				completed, err := FinishCreditRequest(db, user.Id, request.ID, "settle", 50, 100)
				require.NoError(t, err)
				assert.EqualValues(t, 30, completed.Charged)
				assert.EqualValues(t, 20, completed.Unpaid)
				require.NoError(t, db.First(&pack, pack.ID).Error)
				assert.Zero(t, pack.Available)
				assert.EqualValues(t, 30, pack.Spent)
				input.RequestID, input.Amount = "blocked", 1
				_, err = BeginCreditRequest(db, input, 100)
				assert.ErrorIs(t, err, ErrCreditDebtOutstanding)
				input.RequestID, input.Amount = "zero_estimate_is_still_paid", 0
				_, err = BeginCreditRequest(db, input, 100)
				assert.ErrorIs(t, err, ErrCreditDebtOutstanding)
				input.RequestID, input.Free = "explicit_free", true
				free, err := BeginCreditRequest(db, input, 100)
				require.NoError(t, err)
				_, err = FinishCreditRequest(db, user.Id, free.ID, "settle", 0, 100)
				require.NoError(t, err)
				input.RequestID, input.Amount, input.Free = "blocked", 1, false
				grant.SourceID, grant.Amount = "partial", 10
				_, err = GrantCreditPack(db, grant, 100)
				require.NoError(t, err)
				_, err = GrantCreditPack(db, grant, 100)
				require.NoError(t, err)
				var debt CreditDebt
				require.NoError(t, db.Where("request_id = ?", request.ID).First(&debt).Error)
				assert.EqualValues(t, 10, debt.Paid)
				grantKey, err := creditDigest([]string{"grant", grant.SourceType, grant.SourceID})
				require.NoError(t, err)
				var issuance CreditOperation
				require.NoError(t, db.Where("user_id = ? AND key_digest = ?", user.Id, grantKey).First(&issuance).Error)
				_, err = GrantCreditPack(db, CreditGrant{UserID: user.Id, SourceType: "test", SourceID: "later-funds", Amount: 5, StartsAt: 1, ExpiresAt: 200, UseMask: CreditUseAPI}, 100)
				require.NoError(t, err)
				require.NoError(t, db.Transaction(func(tx *gorm.DB) error { return RepayCreditDebtsTx(tx, user.Id, issuance.ID, 100) }))
				require.NoError(t, db.First(&debt, debt.ID).Error)
				assert.EqualValues(t, 10, debt.Paid, "replaying one issuance cannot spend later funds a second time")
				_, err = BeginCreditRequest(db, input, 100)
				assert.ErrorIs(t, err, ErrCreditDebtOutstanding)
				grant.SourceID, grant.Amount = "complete", 25
				_, err = GrantCreditPack(db, grant, 100)
				require.NoError(t, err)
				require.NoError(t, db.First(&debt, debt.ID).Error)
				assert.EqualValues(t, 20, debt.Paid)
				require.NoError(t, db.First(&token, token.Id).Error)
				assert.Equal(t, 950, token.RemainQuota)
				assert.Equal(t, 50, token.UsedQuota, "repayment does not charge the API Key again")
				_, err = BeginCreditRequest(db, input, 100)
				require.NoError(t, err)
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
				child := User{Username: "credit-new-user", AccountingVersion: 1}
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

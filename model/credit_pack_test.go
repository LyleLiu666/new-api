package model

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/QuantumNous/new-api/common"
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
				driver = mysql.Open(dsn)
			case "postgres":
				dsn := os.Getenv("TEST_POSTGRES_DSN")
				if dsn == "" {
					t.Skip("TEST_POSTGRES_DSN not configured")
				}
				driver = postgres.Open(dsn)
			}
			db, err := gorm.Open(driver, &gorm.Config{NamingStrategy: schema.NamingStrategy{TablePrefix: "credit_test_"}})
			require.NoError(t, err)
			sqlDB, err := db.DB()
			require.NoError(t, err)
			sqlDB.SetMaxOpenConns(8)
			previousType := common.MainDatabaseType()
			common.SetMainDatabaseType(common.DatabaseType(dialect))
			t.Cleanup(func() {
				require.NoError(t, db.Migrator().DropTable(&CreditLedgerEntry{}, &CreditAllocation{}, &CreditOperation{}, &CreditPack{}, &CreditAccount{}, &User{}))
				require.NoError(t, sqlDB.Close())
				common.SetMainDatabaseType(previousType)
			})
			require.NoError(t, db.AutoMigrate(&User{}))
			legacy := User{Username: "legacy", Quota: 73, Password: "unused", AffCode: "legacy"}
			require.NoError(t, db.Create(&legacy).Error)
			require.NoError(t, MigrateCreditAccounting(db))
			recorder := &migrationSQLRecorder{}
			require.NoError(t, MigrateCreditAccounting(db.Session(&gorm.Session{Logger: recorder})))
			assert.Empty(t, recorder.schemaMutations(), "repeated migration must not mutate schema")
			var saved User
			require.NoError(t, db.First(&saved, legacy.Id).Error)
			assert.Equal(t, 73, saved.Quota, "additive migration does not convert or overwrite legacy money")
			var count int64
			require.NoError(t, db.Model(&CreditPack{}).Count(&count).Error)
			assert.Zero(t, count, "schema migration does not invent expiry or issuance events")

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

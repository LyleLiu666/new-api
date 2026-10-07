package model

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	CreditUseAPI          = 1
	CreditUseSubscription = 2
)

var (
	ErrCreditInvalid           = errors.New("invalid credit operation")
	ErrCreditOperationConflict = errors.New("credit operation ID reused with different input")
	ErrCreditInsufficient      = errors.New("insufficient eligible credits")
	ErrCreditInvariant         = errors.New("credit accounting invariant violated")
)

// CreditAccount serializes all pack mutations for one user in the primary DB.
// Its existence does not enable the new billing mode or migrate legacy money.
type CreditAccount struct {
	UserID   int   `gorm:"primaryKey;autoIncrement:false"`
	Revision int64 `gorm:"not null"`
}

// CreditPack keeps all quantities nonnegative. Expiry only moves unallocated
// money; held funds remain associated with the original reservation.
type CreditPack struct {
	ID         int64  `gorm:"primaryKey"`
	UserID     int    `gorm:"not null;index:,composite:credit_expiry,priority:1"`
	SourceType string `gorm:"size:32;not null"`
	SourceID   string `gorm:"size:128;not null"`
	Issued     int64  `gorm:"not null"`
	Available  int64  `gorm:"not null"`
	Held       int64  `gorm:"not null"`
	Spent      int64  `gorm:"not null"`
	Expired    int64  `gorm:"not null"`
	Revoked    int64  `gorm:"not null"`
	StartsAt   int64  `gorm:"not null"`
	ExpiresAt  int64  `gorm:"not null;index:,composite:credit_expiry,priority:2"`
	CreatedAt  int64  `gorm:"not null"`
	UseMask    int    `gorm:"not null"`
}

// CreditOperation and its result survive response loss and process restarts.
// The digest indexes exact, case-sensitive business IDs on every DB collation.
type CreditOperation struct {
	ID          int64  `gorm:"primaryKey"`
	UserID      int    `gorm:"not null;uniqueIndex:,composite:credit_event,priority:1"`
	KeyDigest   string `gorm:"size:64;not null;uniqueIndex:,composite:credit_event,priority:2"`
	Fingerprint string `gorm:"size:64;not null"`
	Kind        string `gorm:"size:16;not null"`
	CreatedAt   int64  `gorm:"not null"`
	Result      string `gorm:"type:text;not null"`
}

type CreditAllocation struct {
	ID          int64 `gorm:"primaryKey"`
	OperationID int64 `gorm:"not null;uniqueIndex:,composite:credit_allocation,priority:1"`
	PackID      int64 `gorm:"not null;uniqueIndex:,composite:credit_allocation,priority:2"`
	Amount      int64 `gorm:"not null"`
	Settled     int64 `gorm:"not null"`
	Released    int64 `gorm:"not null"`
}

// Entries are append-only quantity movements, not a replaceable consume log.
type CreditLedgerEntry struct {
	ID          int64 `gorm:"primaryKey"`
	UserID      int   `gorm:"not null;index"`
	OperationID int64 `gorm:"not null;uniqueIndex:,composite:credit_entry,priority:1"`
	PackID      int64 `gorm:"not null;uniqueIndex:,composite:credit_entry,priority:2"`
	Issued      int64 `gorm:"not null"`
	Available   int64 `gorm:"not null"`
	Held        int64 `gorm:"not null"`
	Spent       int64 `gorm:"not null"`
	Expired     int64 `gorm:"not null"`
	Revoked     int64 `gorm:"not null"`
	CreatedAt   int64 `gorm:"not null"`
}

type CreditGrant struct {
	UserID     int
	SourceType string
	SourceID   string
	Amount     int64
	StartsAt   int64
	ExpiresAt  int64
	UseMask    int
}

type CreditReserve struct {
	UserID    int
	RequestID string
	Amount    int64
	Purpose   int
}

type CreditReservation struct {
	OperationID int64
	Allocations []CreditAllocation
}

func MigrateCreditAccounting(db *gorm.DB) error {
	return db.AutoMigrate(&CreditAccount{}, &CreditPack{}, &CreditOperation{}, &CreditAllocation{}, &CreditLedgerEntry{})
}

func creditDigest(value any) (string, error) {
	encoded, err := common.Marshal(value)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

func validCreditTime(now int64) bool { return now > 0 && now <= common.MaxWalletQuota }

func validCreditID(value string, limit int) bool {
	return strings.TrimSpace(value) != "" && len(value) <= limit
}

// UPDATE acquires the writer lock before any reads, including on SQLite. It
// prevents the read-then-write upgrade race that SELECT FOR UPDATE cannot fix
// there. Every writer must participate, even when there are no matching packs.
func lockCreditAccount(tx *gorm.DB, userID int, create bool) error {
	if create {
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&CreditAccount{UserID: userID}).Error; err != nil {
			return err
		}
	}
	result := tx.Model(&CreditAccount{}).Where("user_id = ?", userID).Update("revision", gorm.Expr("revision + 1"))
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrCreditInsufficient
	}
	return nil
}

func findCreditOperation(tx *gorm.DB, userID int, key, fingerprint string) (*CreditOperation, error) {
	var operation CreditOperation
	err := tx.Where("user_id = ? AND key_digest = ?", userID, key).First(&operation).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if operation.Fingerprint != fingerprint {
		return nil, ErrCreditOperationConflict
	}
	return &operation, nil
}

func GrantCreditPack(db *gorm.DB, grant CreditGrant, now int64) (CreditPack, error) {
	var pack CreditPack
	err := db.Transaction(func(tx *gorm.DB) error {
		var err error
		pack, err = GrantCreditPackTx(tx, grant, now)
		return err
	})
	return pack, err
}

// GrantCreditPackTx lets a payment or reward update its source event and the
// pack in the same transaction. No callback can commit just one side.
func GrantCreditPackTx(tx *gorm.DB, grant CreditGrant, now int64) (CreditPack, error) {
	var pack CreditPack
	if grant.UserID <= 0 || !validCreditID(grant.SourceType, 32) || !validCreditID(grant.SourceID, 128) || grant.Amount <= 0 || grant.Amount > common.MaxWalletQuota || !validCreditTime(grant.StartsAt) || !validCreditTime(grant.ExpiresAt) || grant.StartsAt >= grant.ExpiresAt || grant.UseMask <= 0 || grant.UseMask > CreditUseAPI|CreditUseSubscription || !validCreditTime(now) {
		return pack, ErrCreditInvalid
	}
	key, err := creditDigest([]string{"grant", grant.SourceType, grant.SourceID})
	if err != nil {
		return pack, err
	}
	fingerprint, err := creditDigest(grant)
	if err != nil {
		return pack, err
	}
	if err := lockCreditAccount(tx, grant.UserID, true); err != nil {
		return pack, err
	}
	var user User
	if err := tx.Select("id").First(&user, grant.UserID).Error; err != nil {
		return pack, err
	}
	existing, err := findCreditOperation(tx, grant.UserID, key, fingerprint)
	if err != nil {
		return pack, err
	}
	if existing != nil {
		err = common.UnmarshalJsonStr(existing.Result, &pack)
		return pack, err
	}
	var outstanding int64
	if err := tx.Model(&CreditPack{}).Where("user_id = ?", grant.UserID).Select("COALESCE(SUM(CASE WHEN expires_at > ? THEN available ELSE 0 END + held), 0)", now).Scan(&outstanding).Error; err != nil {
		return pack, err
	}
	addition := grant.Amount
	if grant.ExpiresAt <= now {
		addition = 0
	}
	if outstanding < 0 || outstanding > common.MaxWalletQuota-addition {
		return pack, ErrWalletQuotaLimitExceeded
	}
	pack = CreditPack{UserID: grant.UserID, SourceType: grant.SourceType, SourceID: grant.SourceID, Issued: grant.Amount, Available: grant.Amount, StartsAt: grant.StartsAt, ExpiresAt: grant.ExpiresAt, CreatedAt: now, UseMask: grant.UseMask}
	if grant.ExpiresAt <= now {
		pack.Available, pack.Expired = 0, grant.Amount
	}
	if err := tx.Create(&pack).Error; err != nil {
		return CreditPack{}, err
	}
	encoded, err := common.Marshal(pack)
	if err != nil {
		return CreditPack{}, err
	}
	operation := CreditOperation{UserID: grant.UserID, KeyDigest: key, Fingerprint: fingerprint, Kind: "grant", CreatedAt: now, Result: string(encoded)}
	if err := tx.Create(&operation).Error; err != nil {
		return CreditPack{}, err
	}
	entry := CreditLedgerEntry{UserID: grant.UserID, OperationID: operation.ID, PackID: pack.ID, Issued: pack.Issued, Available: pack.Available, Expired: pack.Expired, CreatedAt: now}
	return pack, tx.Create(&entry).Error
}

func ReserveCreditPacks(db *gorm.DB, input CreditReserve, now int64) (CreditReservation, error) {
	var result CreditReservation
	err := db.Transaction(func(tx *gorm.DB) error {
		var err error
		result, err = ReserveCreditPacksTx(tx, input, now)
		return err
	})
	return result, err
}

func ReserveCreditPacksTx(tx *gorm.DB, input CreditReserve, now int64) (CreditReservation, error) {
	var result CreditReservation
	if input.UserID <= 0 || !validCreditID(input.RequestID, 128) || input.Amount < 0 || input.Amount > common.MaxWalletQuota || (input.Purpose != CreditUseAPI && input.Purpose != CreditUseSubscription) || !validCreditTime(now) {
		return result, ErrCreditInvalid
	}
	key, err := creditDigest([]string{"reserve", input.RequestID})
	if err != nil {
		return result, err
	}
	fingerprint, err := creditDigest(input)
	if err != nil {
		return result, err
	}
	if err := lockCreditAccount(tx, input.UserID, false); err != nil {
		return result, err
	}
	existing, err := findCreditOperation(tx, input.UserID, key, fingerprint)
	if err != nil {
		return result, err
	}
	if existing != nil {
		err = common.UnmarshalJsonStr(existing.Result, &result)
		return result, err
	}
	var packs []CreditPack
	if err := tx.Where("user_id = ? AND starts_at <= ? AND expires_at > ? AND available > 0", input.UserID, now, now).Order("expires_at ASC, created_at ASC, id ASC").Find(&packs).Error; err != nil {
		return result, err
	}
	remaining := input.Amount
	result.Allocations = []CreditAllocation{}
	for _, pack := range packs {
		if pack.UseMask&input.Purpose == 0 {
			continue
		}
		if !pack.QuantitiesValid() {
			return result, ErrCreditInvariant
		}
		amount := min(pack.Available, remaining)
		if amount > 0 {
			result.Allocations = append(result.Allocations, CreditAllocation{PackID: pack.ID, Amount: amount})
		}
		remaining -= amount
		if remaining == 0 {
			break
		}
	}
	if remaining != 0 {
		return CreditReservation{}, ErrCreditInsufficient
	}
	operation := CreditOperation{UserID: input.UserID, KeyDigest: key, Fingerprint: fingerprint, Kind: "reserve", CreatedAt: now, Result: ""}
	if err := tx.Create(&operation).Error; err != nil {
		return CreditReservation{}, err
	}
	result.OperationID = operation.ID
	for i := range result.Allocations {
		allocation := &result.Allocations[i]
		allocation.OperationID = operation.ID
		update := tx.Model(&CreditPack{}).Where("id = ? AND user_id = ? AND available >= ?", allocation.PackID, input.UserID, allocation.Amount).Updates(map[string]any{"available": gorm.Expr("available - ?", allocation.Amount), "held": gorm.Expr("held + ?", allocation.Amount)})
		if update.Error != nil {
			return CreditReservation{}, update.Error
		}
		if update.RowsAffected != 1 {
			return CreditReservation{}, ErrCreditInvariant
		}
		if err := tx.Create(allocation).Error; err != nil {
			return CreditReservation{}, err
		}
		entry := CreditLedgerEntry{UserID: input.UserID, OperationID: operation.ID, PackID: allocation.PackID, Available: -allocation.Amount, Held: allocation.Amount, CreatedAt: now}
		if err := tx.Create(&entry).Error; err != nil {
			return CreditReservation{}, err
		}
	}
	encoded, err := common.Marshal(result)
	if err != nil {
		return CreditReservation{}, err
	}
	return result, tx.Model(&operation).Update("result", string(encoded)).Error
}

// ListCreditPacks presents logical expiry without requiring a background job.
// It is a projection: query results must never be saved back as ledger state.
func ListCreditPacks(db *gorm.DB, userID int, now int64) ([]CreditPack, error) {
	if userID <= 0 || !validCreditTime(now) {
		return nil, ErrCreditInvalid
	}
	var packs []CreditPack
	if err := db.Where("user_id = ?", userID).Order("expires_at ASC, created_at ASC, id ASC").Find(&packs).Error; err != nil {
		return nil, err
	}
	for i := range packs {
		if !packs[i].QuantitiesValid() {
			return nil, ErrCreditInvariant
		}
		if packs[i].ExpiresAt <= now {
			packs[i].Expired += packs[i].Available
			packs[i].Available = 0
		}
	}
	return packs, nil
}

// QuantitiesValid checks the conservation contract without unsafe summation.
func (pack CreditPack) QuantitiesValid() bool {
	if pack.Issued <= 0 || pack.Issued > common.MaxWalletQuota {
		return false
	}
	remaining := pack.Issued
	for _, amount := range []int64{pack.Available, pack.Held, pack.Spent, pack.Expired, pack.Revoked} {
		if amount < 0 || amount > remaining {
			return false
		}
		remaining -= amount
	}
	return remaining == 0
}

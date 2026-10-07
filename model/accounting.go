package model

import (
	"errors"
	"fmt"

	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm"
)

var ErrCreditNeedsReview = errors.New("credit operation needs review")

var ErrCreditDebtOutstanding = errors.New("unpaid credit bill blocks new paid consumption")
var ErrCreditOperationRequired = errors.New("durable credit operation required for this account")

const CreditFundingSource = "credit_packs"

func requireLegacyWallet(db *gorm.DB, userID int) error {
	var version int
	if err := db.Model(&User{}).Where("id = ?", userID).Select("accounting_version").Scan(&version).Error; err != nil {
		return err
	}
	if version != 0 {
		return ErrCreditOperationRequired
	}
	return nil
}

func requireLegacyToken(db *gorm.DB, tokenID int) error {
	var token Token
	if err := db.Select("user_id").Where("id = ?", tokenID).Find(&token).Error; err != nil {
		return err
	}
	return requireLegacyWallet(db, token.UserId)
}

type CreditRequest struct {
	ID            int64  `gorm:"primaryKey"`
	UserID        int    `gorm:"not null;uniqueIndex:,composite:credit_request,priority:1"`
	RequestID     string `gorm:"size:128;not null"`
	RequestDigest string `gorm:"size:64;not null;uniqueIndex:,composite:credit_request,priority:2"`
	Fingerprint   string `gorm:"size:64;not null"`
	ModelName     string `gorm:"size:256;not null"`
	Protocol      string `gorm:"size:32;not null"`
	PriceSnapshot string `gorm:"type:text;not null"`
	RulesVersion  string `gorm:"size:32;not null"`
	TokenID       int    `gorm:"not null"`
	Reserved      int64  `gorm:"not null"`
	ReservationID int64  `gorm:"not null"`
	State         string `gorm:"size:16;not null;index"`
	CreatedAt     int64  `gorm:"not null"`
	SubmittedAt   int64  `gorm:"not null"`
	IntentKind    string `gorm:"size:16;not null"`
	Actual        int64  `gorm:"not null"`
	Charged       int64  `gorm:"not null"`
	Unpaid        int64  `gorm:"not null"`
	FinishedAt    int64  `gorm:"not null"`
	TaskID        string `gorm:"size:191;not null;default:'';index"`
	TaskKind      string `gorm:"size:16;not null;default:''"`
	TaskDigest    string `gorm:"size:64;not null;default:'';index"`
	TaskRowID     int64  `gorm:"not null;default:0"`
}

type CreditDebt struct {
	ID        int64 `gorm:"primaryKey"`
	UserID    int   `gorm:"not null;index"`
	RequestID int64 `gorm:"not null;uniqueIndex"`
	Amount    int64 `gorm:"not null"`
	Paid      int64 `gorm:"not null"`
	CreatedAt int64 `gorm:"not null"`
}

// Each increase has its own immutable reservation and FEFO allocation. The
// request owns every part; settlement closes them in admission order.
type CreditRequestReservation struct {
	ID            int64 `gorm:"primaryKey"`
	UserID        int   `gorm:"not null;index"`
	RequestID     int64 `gorm:"not null;uniqueIndex:,composite:credit_request_target,priority:1"`
	Target        int64 `gorm:"not null;uniqueIndex:,composite:credit_request_target,priority:2"`
	ReservationID int64 `gorm:"not null;uniqueIndex"`
	Amount        int64 `gorm:"not null"`
	CreatedAt     int64 `gorm:"not null"`
}

type CreditRequestInput struct {
	UserID        int
	RequestID     string
	ModelName     string
	Protocol      string
	PriceSnapshot string
	TokenID       int
	Playground    bool
	Free          bool
	Amount        int64
}

func GetUserAccountingVersion(db *gorm.DB, userID int) (int, error) {
	if db == nil || userID <= 0 {
		return 0, ErrCreditInvalid
	}
	var user User
	err := db.Select("id", "accounting_version").First(&user, userID).Error
	return user.AccountingVersion, err
}

func BeginCreditRequest(db *gorm.DB, input CreditRequestInput, now int64) (CreditRequest, error) {
	var request CreditRequest
	if input.UserID <= 0 || !validCreditID(input.RequestID, 128) || !validCreditID(input.ModelName, 256) || !validCreditID(input.Protocol, 32) || input.PriceSnapshot == "" || len(input.PriceSnapshot) > 65536 || input.Amount < 0 || input.Amount > common.MaxQuota || (input.Free && input.Amount != 0) || !validCreditTime(now) || (!input.Playground && input.TokenID <= 0) || (input.Playground && input.TokenID != 0) {
		return request, ErrCreditInvalid
	}
	var snapshot map[string]any
	if err := common.UnmarshalJsonStr(input.PriceSnapshot, &snapshot); err != nil || snapshot == nil {
		return request, ErrCreditInvalid
	}
	digest, err := creditDigest(input.RequestID)
	if err != nil {
		return request, err
	}
	fingerprint, err := creditDigest(input)
	if err != nil {
		return request, err
	}
	err = db.Transaction(func(tx *gorm.DB) error {
		if err := lockCreditAccount(tx, input.UserID, true); err != nil {
			return err
		}
		version, err := GetUserAccountingVersion(tx, input.UserID)
		if err != nil {
			return err
		}
		if version != 1 {
			return ErrCreditInvalid
		}
		found := tx.Where("user_id = ? AND request_digest = ?", input.UserID, digest).Limit(1).Find(&request)
		if found.Error != nil {
			return found.Error
		}
		if found.RowsAffected != 0 {
			if request.Fingerprint != fingerprint {
				return ErrCreditOperationConflict
			}
			return nil
		}
		if !input.Free {
			var debts int64
			if err := tx.Model(&CreditDebt{}).Where("user_id = ? AND amount > paid", input.UserID).Count(&debts).Error; err != nil {
				return err
			}
			if debts > 0 {
				return ErrCreditDebtOutstanding
			}
		}
		reservation, err := ReserveCreditPacksTx(tx, CreditReserve{UserID: input.UserID, RequestID: "request:" + digest, Amount: input.Amount, Purpose: CreditUseAPI}, now)
		if err != nil {
			return err
		}
		if err := adjustCreditToken(tx, input.UserID, input.TokenID, input.Amount, true, now); err != nil {
			return err
		}
		request = CreditRequest{UserID: input.UserID, RequestID: input.RequestID, RequestDigest: digest, Fingerprint: fingerprint, ModelName: input.ModelName, Protocol: input.Protocol, PriceSnapshot: input.PriceSnapshot, RulesVersion: "accounting-v1", TokenID: input.TokenID, Reserved: input.Amount, ReservationID: reservation.OperationID, State: "reserved", CreatedAt: now}
		if err := tx.Create(&request).Error; err != nil {
			return err
		}
		return tx.Create(&CreditRequestReservation{UserID: input.UserID, RequestID: request.ID, Target: input.Amount, ReservationID: reservation.OperationID, Amount: input.Amount, CreatedAt: now}).Error
	})
	if err == nil {
		invalidateCreditTokenCache(db, input.TokenID)
	}
	return request, err
}

func creditRequestReservationsTx(tx *gorm.DB, request CreditRequest) ([]CreditRequestReservation, error) {
	var reservations []CreditRequestReservation
	if err := tx.Where("request_id = ? AND user_id = ?", request.ID, request.UserID).Order("id ASC").Find(&reservations).Error; err != nil {
		return nil, err
	}
	if len(reservations) == 0 {
		// Requests created before incremental reservations have one original
		// hold. Growth persists this link before adding the next one.
		reservations = []CreditRequestReservation{{UserID: request.UserID, RequestID: request.ID, Target: request.Reserved, ReservationID: request.ReservationID, Amount: request.Reserved, CreatedAt: request.CreatedAt}}
	}
	var total int64
	for i, reservation := range reservations {
		if reservation.ReservationID <= 0 || reservation.Amount < 0 || reservation.Amount > common.MaxQuota-total || (i > 0 && reservation.Amount == 0) || (i == 0 && reservation.ReservationID != request.ReservationID) {
			return nil, ErrCreditInvariant
		}
		total += reservation.Amount
		if reservation.Target != total {
			return nil, ErrCreditInvariant
		}
	}
	if total != request.Reserved {
		return nil, ErrCreditInvariant
	}
	return reservations, nil
}

// GrowCreditRequestReservation ensures at least target is durably held. It
// never renews old money or releases earlier holds when an estimate falls.
func GrowCreditRequestReservation(db *gorm.DB, userID int, requestID, target, now int64) (CreditRequest, error) {
	var request CreditRequest
	if userID <= 0 || requestID <= 0 || target < 0 || target > common.MaxQuota || !validCreditTime(now) {
		return request, ErrCreditInvalid
	}
	err := db.Transaction(func(tx *gorm.DB) error {
		if err := lockCreditAccount(tx, userID, false); err != nil {
			return err
		}
		if err := tx.Where("id = ? AND user_id = ?", requestID, userID).First(&request).Error; err != nil {
			return err
		}
		reservations, err := creditRequestReservationsTx(tx, request)
		if err != nil {
			return err
		}
		if target <= request.Reserved {
			return nil
		}
		if (request.State != "reserved" && request.State != "executing") || request.IntentKind != "" {
			return ErrCreditOperationConflict
		}
		version, err := GetUserAccountingVersion(tx, userID)
		if err != nil {
			return err
		}
		if version != 1 {
			return ErrCreditOperationRequired
		}
		var debts int64
		if err := tx.Model(&CreditDebt{}).Where("user_id = ? AND amount > paid", userID).Count(&debts).Error; err != nil {
			return err
		}
		if debts != 0 {
			return ErrCreditDebtOutstanding
		}
		delta := target - request.Reserved
		hold, err := ReserveCreditPacksTx(tx, CreditReserve{UserID: userID, RequestID: fmt.Sprintf("growth:%s:%d", request.RequestDigest, target), Amount: delta, Purpose: CreditUseAPI}, now)
		if err != nil {
			return err
		}
		if err := adjustCreditToken(tx, userID, request.TokenID, delta, true, now); err != nil {
			return err
		}
		if reservations[0].ID == 0 {
			if err := tx.Create(&reservations[0]).Error; err != nil {
				return err
			}
		}
		if err := tx.Create(&CreditRequestReservation{UserID: userID, RequestID: requestID, Target: target, ReservationID: hold.OperationID, Amount: delta, CreatedAt: now}).Error; err != nil {
			return err
		}
		request.Reserved = target
		return tx.Model(&request).Update("reserved", target).Error
	})
	if err == nil {
		invalidateCreditTokenCache(db, request.TokenID)
	}
	return request, err
}

// The primary DB checks the real token row; cached balances and the caller's
// unlimited flag cannot bypass this financial constraint.
func adjustCreditToken(tx *gorm.DB, userID, tokenID int, delta int64, admission bool, now int64) error {
	if tokenID == 0 {
		return nil
	}
	var token Token
	if err := lockForUpdate(tx).Where("id = ? AND user_id = ?", tokenID, userID).First(&token).Error; err != nil {
		return err
	}
	if admission && !token.UnlimitedQuota && int64(token.RemainQuota) < delta {
		return ErrCreditInsufficient
	}
	remain, used := int64(token.RemainQuota), int64(token.UsedQuota)
	if remain > common.MaxWalletQuota || remain < -common.MaxWalletQuota || used < 0 || used > common.MaxWalletQuota || delta > common.MaxWalletQuota-used || delta < -used || remain-delta > common.MaxWalletQuota || remain-delta < -common.MaxWalletQuota {
		return ErrCreditInvariant
	}
	result := tx.Model(&Token{}).Where("id = ? AND user_id = ?", tokenID, userID).Updates(map[string]any{"remain_quota": remain - delta, "used_quota": used + delta, "accessed_time": now})
	return result.Error
}

func invalidateCreditTokenCache(db *gorm.DB, tokenID int) {
	if tokenID == 0 || !common.RedisEnabled {
		return
	}
	var token Token
	if err := db.Select("id", commonKeyCol).First(&token, tokenID).Error; err != nil {
		common.SysError("cannot reload credit billing token for cache invalidation")
		return
	}
	if err := invalidateTokenCacheForMutation(token.Key); err != nil {
		common.SysError("cannot invalidate credit billing token cache")
	}
}

func MarkCreditRequestSubmitted(db *gorm.DB, userID int, requestID int64, now int64) error {
	if userID <= 0 || requestID <= 0 || !validCreditTime(now) {
		return ErrCreditInvalid
	}
	return db.Transaction(func(tx *gorm.DB) error {
		if err := lockCreditAccount(tx, userID, false); err != nil {
			return err
		}
		var request CreditRequest
		if err := tx.Where("id = ? AND user_id = ?", requestID, userID).First(&request).Error; err != nil {
			return err
		}
		if request.State != "reserved" && request.State != "executing" {
			return ErrCreditOperationConflict
		}
		if request.SubmittedAt != 0 {
			return nil
		}
		return tx.Model(&request).Updates(map[string]any{"state": "executing", "submitted_at": now}).Error
	})
}

func MarkCreditRequestReview(db *gorm.DB, userID int, requestID int64) error {
	if userID <= 0 || requestID <= 0 {
		return ErrCreditInvalid
	}
	return db.Transaction(func(tx *gorm.DB) error {
		if err := lockCreditAccount(tx, userID, false); err != nil {
			return err
		}
		result := tx.Model(&CreditRequest{}).Where("id = ? AND user_id = ? AND state IN ?", requestID, userID, []string{"reserved", "executing", "review"}).Update("state", "review")
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			var request CreditRequest
			if err := tx.Where("id = ? AND user_id = ?", requestID, userID).First(&request).Error; err != nil {
				return err
			}
			if request.State != "review" {
				return ErrCreditOperationConflict
			}
		}
		return nil
	})
}

// FinishCreditRequest records the intent before changing funds. A failed final
// transaction leaves the intent and hold available to persistent recovery.
func FinishCreditRequest(db *gorm.DB, userID int, requestID int64, kind string, actual int64, now int64) (CreditRequest, error) {
	var request CreditRequest
	if userID <= 0 || requestID <= 0 || !validCreditTime(now) || actual < 0 || actual > common.MaxQuota || (kind != "settle" && kind != "release") || (kind == "release" && actual != 0) {
		return request, ErrCreditInvalid
	}
	err := db.Transaction(func(tx *gorm.DB) error {
		if err := lockCreditAccount(tx, userID, false); err != nil {
			return err
		}
		if err := tx.Where("id = ? AND user_id = ?", requestID, userID).First(&request).Error; err != nil {
			return err
		}
		if request.IntentKind != "" {
			if request.IntentKind != kind || request.Actual != actual {
				return ErrCreditOperationConflict
			}
			return nil
		}
		if request.TaskID != "" && kind == "settle" {
			if err := validateCreditTaskCompletion(tx, request); err != nil {
				return err
			}
		}
		if kind == "release" && request.SubmittedAt != 0 {
			request.State = "review"
			return tx.Model(&request).Update("state", "review").Error
		}
		request.IntentKind, request.Actual, request.State = kind, actual, "pending"
		return tx.Model(&request).Updates(map[string]any{"intent_kind": kind, "actual": actual, "state": "pending"}).Error
	})
	if err != nil {
		return request, err
	}
	if request.State == "review" {
		return request, ErrCreditNeedsReview
	}
	err = db.Transaction(func(tx *gorm.DB) error {
		if err := lockCreditAccount(tx, userID, false); err != nil {
			return err
		}
		if err := tx.Where("id = ? AND user_id = ?", requestID, userID).First(&request).Error; err != nil {
			return err
		}
		if request.State == "settled" || request.State == "released" {
			return nil
		}
		if request.State != "pending" || request.IntentKind != kind || request.Actual != actual {
			return ErrCreditOperationConflict
		}
		reservations, err := creditRequestReservationsTx(tx, request)
		if err != nil {
			return err
		}
		heldCharge := min(actual, request.Reserved)
		remaining := heldCharge
		for _, reservation := range reservations {
			charged := min(remaining, reservation.Amount)
			if _, err := FinalizeCreditReservationTx(tx, CreditFinalize{UserID: userID, ReservationID: reservation.ReservationID, Kind: kind, Actual: charged}, now); err != nil {
				return err
			}
			remaining -= charged
		}
		charged := heldCharge
		if actual > charged {
			packs, err := ListCreditPacks(tx, userID, now)
			if err != nil {
				return err
			}
			var extra int64
			for _, pack := range packs {
				if !pack.UsableAt(now, CreditUseAPI) {
					continue
				}
				extra += min(pack.Available, actual-charged-extra)
				if extra == actual-charged {
					break
				}
			}
			if extra > 0 {
				reservation, err := ReserveCreditPacksTx(tx, CreditReserve{UserID: userID, RequestID: fmt.Sprintf("extra:%d", request.ID), Amount: extra, Purpose: CreditUseAPI}, now)
				if err != nil {
					return err
				}
				if _, err := FinalizeCreditReservationTx(tx, CreditFinalize{UserID: userID, ReservationID: reservation.OperationID, Kind: "settle", Actual: extra}, now); err != nil {
					return err
				}
				charged += extra
			}
		}
		unpaid := actual - charged
		if unpaid > 0 {
			if err := tx.Create(&CreditDebt{UserID: userID, RequestID: request.ID, Amount: unpaid, CreatedAt: now}).Error; err != nil {
				return err
			}
		}
		if err := adjustCreditToken(tx, userID, request.TokenID, actual-request.Reserved, false, now); err != nil {
			return err
		}
		if request.TaskID != "" {
			if err := validateCreditTaskCompletion(tx, request); err != nil {
				return err
			}
			var result *gorm.DB
			switch request.TaskKind {
			case "task":
				result = tx.Model(&Task{}).Where("id = ? AND user_id = ?", request.TaskRowID, userID).Update("quota", actual)
			case "midjourney":
				result = tx.Model(&Midjourney{}).Where("id = ? AND user_id = ?", request.TaskRowID, userID).Update("quota", actual)
			default:
				return ErrCreditInvariant
			}
			if result.Error != nil {
				return result.Error
			}
		}
		state := "settled"
		if kind == "release" {
			state = "released"
		}
		if err := tx.Model(&request).Updates(map[string]any{"state": state, "charged": charged, "unpaid": unpaid, "finished_at": now}).Error; err != nil {
			return err
		}
		request.State, request.Charged, request.Unpaid, request.FinishedAt = state, charged, unpaid, now
		return nil
	})
	if err == nil {
		invalidateCreditTokenCache(db, request.TokenID)
	}
	return request, err
}

// The task row and its host bill are bound in the same transaction. Account
// serialization prevents two requests from claiming the same user's task.
func bindCreditTaskTx(tx *gorm.DB, userID int, requestID int64, taskID, taskKind string, tokenID int) error {
	if requestID <= 0 || taskID == "" || len(taskID) > 191 || (taskKind != "task" && taskKind != "midjourney") {
		return ErrCreditInvalid
	}
	if err := lockCreditAccount(tx, userID, false); err != nil {
		return err
	}
	var request CreditRequest
	if err := tx.Where("id = ? AND user_id = ?", requestID, userID).First(&request).Error; err != nil {
		return err
	}
	if request.TaskID != "" || request.TokenID != tokenID || request.SubmittedAt == 0 || request.State != "executing" || request.IntentKind != "" {
		return ErrCreditOperationConflict
	}
	digest, err := creditDigest(struct {
		Kind string
		ID   string
	}{taskKind, taskID})
	if err != nil {
		return err
	}
	var linked int64
	if err := tx.Model(&CreditRequest{}).Where("user_id = ? AND task_digest = ?", userID, digest).Count(&linked).Error; err != nil {
		return err
	}
	if linked != 0 {
		return ErrCreditOperationConflict
	}
	return tx.Model(&request).Updates(map[string]any{"task_id": taskID, "task_kind": taskKind, "task_digest": digest}).Error
}

func validateCreditTaskCompletion(tx *gorm.DB, request CreditRequest) error {
	if request.TaskRowID <= 0 {
		return ErrCreditInvariant
	}
	switch request.TaskKind {
	case "task":
		var task Task
		if err := lockForUpdate(tx).Where("id = ? AND user_id = ?", request.TaskRowID, request.UserID).First(&task).Error; err != nil {
			return err
		}
		if task.TaskID != request.TaskID || task.PrivateData.CreditRequestID != request.ID || task.PrivateData.BillingSource != CreditFundingSource || task.Status != TaskStatusSuccess {
			return ErrCreditOperationConflict
		}
	case "midjourney":
		var task Midjourney
		if err := lockForUpdate(tx).Where("id = ? AND user_id = ?", request.TaskRowID, request.UserID).First(&task).Error; err != nil {
			return err
		}
		if task.MjId != request.TaskID || task.CreditRequestID != request.ID || task.Status != "SUCCESS" {
			return ErrCreditOperationConflict
		}
	default:
		return ErrCreditInvariant
	}
	return nil
}

// RepayCreditDebtsTx applies eligible API credits to old unpaid bills in order.
// Token usage was recorded with the original fee and must not be charged again.
// The issuance operation is the repayment event; both commit atomically.
func RepayCreditDebtsTx(tx *gorm.DB, userID int, issuanceID int64, now int64) error {
	if userID <= 0 || issuanceID <= 0 || !validCreditTime(now) {
		return ErrCreditInvalid
	}
	if err := lockCreditAccount(tx, userID, false); err != nil {
		return err
	}
	var issuance CreditOperation
	if err := tx.Where("id = ? AND user_id = ? AND kind = ?", issuanceID, userID, "grant").First(&issuance).Error; err != nil {
		return err
	}
	key, err := creditDigest([]string{"repay", fmt.Sprint(issuanceID)})
	if err != nil {
		return err
	}
	fingerprint, err := creditDigest([]int64{int64(userID), issuanceID})
	if err != nil {
		return err
	}
	existing, err := findCreditOperation(tx, userID, key, fingerprint)
	if err != nil || existing != nil {
		return err
	}
	// The event applies once even when it paid only part of a debt or had no
	// debt to pay. All debt updates and this marker share the caller's transaction.
	if err := tx.Create(&CreditOperation{UserID: userID, KeyDigest: key, Fingerprint: fingerprint, Kind: "repay", CreatedAt: now, Result: "{}"}).Error; err != nil {
		return err
	}
	var debts []CreditDebt
	if err := tx.Where("user_id = ? AND amount > paid", userID).Order("created_at ASC, id ASC").Find(&debts).Error; err != nil {
		return err
	}
	for _, debt := range debts {
		if debt.Amount <= 0 || debt.Amount > common.MaxQuota || debt.Paid < 0 || debt.Paid > debt.Amount {
			return ErrCreditInvariant
		}
		packs, err := ListCreditPacks(tx, userID, now)
		if err != nil {
			return err
		}
		var payment int64
		for _, pack := range packs {
			if !pack.UsableAt(now, CreditUseAPI) {
				continue
			}
			payment += min(pack.Available, debt.Amount-debt.Paid-payment)
			if payment == debt.Amount-debt.Paid {
				break
			}
		}
		if payment == 0 {
			break
		}
		reservation, err := ReserveCreditPacksTx(tx, CreditReserve{UserID: userID, RequestID: fmt.Sprintf("repay:%d:%d", issuanceID, debt.ID), Amount: payment, Purpose: CreditUseAPI}, now)
		if err != nil {
			return err
		}
		if _, err := FinalizeCreditReservationTx(tx, CreditFinalize{UserID: userID, ReservationID: reservation.OperationID, Kind: "settle", Actual: payment}, now); err != nil {
			return err
		}
		result := tx.Model(&CreditDebt{}).Where("id = ? AND paid = ?", debt.ID, debt.Paid).Update("paid", debt.Paid+payment)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrCreditInvariant
		}
	}
	return nil
}

// CreditFinalize identifies one terminal result, shared by settlement and
// release. A hold acquired before expiry remains valid for this request.
type CreditFinalize struct {
	UserID        int
	ReservationID int64
	Kind          string
	Actual        int64
}

type CreditFinalization struct {
	OperationID int64
	Settled     int64
	Released    int64
	Allocations []CreditAllocation
}

func FinalizeCreditReservation(db *gorm.DB, input CreditFinalize, now int64) (CreditFinalization, error) {
	var result CreditFinalization
	err := db.Transaction(func(tx *gorm.DB) error {
		var err error
		result, err = FinalizeCreditReservationTx(tx, input, now)
		return err
	})
	return result, err
}

// FinalizeCreditReservationTx completes only the original held quantities.
// Additional funding and debt belong to the owning request, which must record
// them explicitly rather than silently overdrawing one of these packs.
func FinalizeCreditReservationTx(tx *gorm.DB, input CreditFinalize, now int64) (CreditFinalization, error) {
	var result CreditFinalization
	if input.UserID <= 0 || input.ReservationID <= 0 || input.Actual < 0 || input.Actual > common.MaxWalletQuota || !validCreditTime(now) || (input.Kind != "settle" && input.Kind != "release") || (input.Kind == "release" && input.Actual != 0) {
		return result, ErrCreditInvalid
	}
	if err := lockCreditAccount(tx, input.UserID, false); err != nil {
		return result, err
	}
	var reservation CreditOperation
	if err := tx.Where("id = ? AND user_id = ? AND kind = ?", input.ReservationID, input.UserID, "reserve").First(&reservation).Error; err != nil {
		return result, err
	}
	key, err := creditDigest([]string{"finish", fmt.Sprint(input.ReservationID)})
	if err != nil {
		return result, err
	}
	fingerprint, err := creditDigest(input)
	if err != nil {
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
	// The stored allocation order is the original FEFO order, even if current
	// account state, time or other reservations have since changed.
	var original CreditReservation
	if err := common.UnmarshalJsonStr(reservation.Result, &original); err != nil {
		return result, err
	}
	if original.OperationID != input.ReservationID {
		return result, ErrCreditInvariant
	}
	var held int64
	for _, allocation := range original.Allocations {
		if allocation.Amount <= 0 || allocation.Amount > common.MaxWalletQuota-held {
			return result, ErrCreditInvariant
		}
		held += allocation.Amount
	}
	if input.Actual > held {
		return result, ErrCreditInsufficient
	}
	operation := CreditOperation{UserID: input.UserID, KeyDigest: key, Fingerprint: fingerprint, Kind: input.Kind, CreatedAt: now, Result: ""}
	if err := tx.Create(&operation).Error; err != nil {
		return result, err
	}
	result.OperationID, result.Settled, result.Released = operation.ID, input.Actual, held-input.Actual
	result.Allocations = []CreditAllocation{}
	remaining := input.Actual
	for _, originalAllocation := range original.Allocations {
		var allocation CreditAllocation
		if err := tx.Where("id = ? AND operation_id = ? AND pack_id = ?", originalAllocation.ID, reservation.ID, originalAllocation.PackID).First(&allocation).Error; err != nil {
			return result, err
		}
		if allocation.Amount != originalAllocation.Amount || allocation.Settled != 0 || allocation.Released != 0 {
			return result, ErrCreditInvariant
		}
		var pack CreditPack
		if err := tx.Where("id = ? AND user_id = ?", allocation.PackID, input.UserID).First(&pack).Error; err != nil {
			return result, err
		}
		if !pack.QuantitiesValid() || pack.Held < allocation.Amount {
			return result, ErrCreditInvariant
		}
		settled := min(remaining, allocation.Amount)
		released := allocation.Amount - settled
		available, expired := released, int64(0)
		if pack.ExpiresAt <= now {
			available, expired = 0, released
		}
		update := tx.Model(&CreditPack{}).Where("id = ? AND user_id = ? AND held >= ?", pack.ID, input.UserID, allocation.Amount).Updates(map[string]any{
			"held":      gorm.Expr("held - ?", allocation.Amount),
			"spent":     gorm.Expr("spent + ?", settled),
			"available": gorm.Expr("available + ?", available),
			"expired":   gorm.Expr("expired + ?", expired),
		})
		if update.Error != nil {
			return result, update.Error
		}
		if update.RowsAffected != 1 {
			return result, ErrCreditInvariant
		}
		update = tx.Model(&allocation).Where("settled = 0 AND released = 0").Updates(map[string]any{"settled": settled, "released": released})
		if update.Error != nil {
			return result, update.Error
		}
		if update.RowsAffected != 1 {
			return result, ErrCreditInvariant
		}
		allocation.Settled, allocation.Released = settled, released
		result.Allocations = append(result.Allocations, allocation)
		entry := CreditLedgerEntry{UserID: input.UserID, OperationID: operation.ID, PackID: pack.ID, Held: -allocation.Amount, Available: available, Spent: settled, Expired: expired, CreatedAt: now}
		if err := tx.Create(&entry).Error; err != nil {
			return result, err
		}
		remaining -= settled
	}
	encoded, err := common.Marshal(result)
	if err != nil {
		return result, err
	}
	return result, tx.Model(&operation).Update("result", string(encoded)).Error
}

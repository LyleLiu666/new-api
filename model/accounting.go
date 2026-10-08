package model

import (
	"errors"
	"fmt"

	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm"
)

var ErrCreditNeedsReview = errors.New("credit operation needs review")

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
	ReviewEvidenceID  int64  `gorm:"not null;default:0"`
	UsageEvidenceID   int64  `gorm:"not null;default:0"`
	FundingSource     string `gorm:"size:32;not null;default:''"`
	SubscriptionID    int    `gorm:"not null;default:0"`
	ID                int64  `gorm:"primaryKey"`
	UserID            int    `gorm:"not null;uniqueIndex:,composite:credit_request,priority:1"`
	RequestID         string `gorm:"size:128;not null"`
	RequestDigest     string `gorm:"size:64;not null;uniqueIndex:,composite:credit_request,priority:2"`
	Fingerprint       string `gorm:"size:64;not null"`
	ChannelID         int    `gorm:"not null;default:0"`
	Group             string `gorm:"size:64;not null;default:''"`
	ModelName         string `gorm:"size:256;not null"`
	Protocol          string `gorm:"size:32;not null"`
	PriceSnapshot     string `gorm:"type:text;not null"`
	RulesVersion      string `gorm:"size:32;not null"`
	TokenID           int    `gorm:"not null"`
	Reserved          int64  `gorm:"not null"`
	ReservationID     int64  `gorm:"not null"`
	State             string `gorm:"size:16;not null;index"`
	CreatedAt         int64  `gorm:"not null"`
	SubmittedAt       int64  `gorm:"not null"`
	IntentKind        string `gorm:"size:16;not null"`
	Actual            int64  `gorm:"not null"`
	Charged           int64  `gorm:"not null"`
	Uncollected       int64  `gorm:"not null"`
	FinishedAt        int64  `gorm:"not null"`
	TaskID            string `gorm:"size:191;not null;default:'';index"`
	TaskKind          string `gorm:"size:16;not null;default:''"`
	TaskDigest        string `gorm:"size:64;not null;default:'';index"`
	TaskRowID         int64  `gorm:"not null;default:0"`
	LeaseOwner        string `gorm:"size:128;not null;default:''"`
	LeaseEpoch        int64  `gorm:"not null;default:0"`
	LeaseUntil        int64  `gorm:"not null;default:0;index"`
	RecoveryAttempts  int64  `gorm:"not null;default:0"`
	LastRecoveryError string `gorm:"size:1024;not null;default:''"`
	NextRecoveryAt    int64  `gorm:"not null;default:0;index"`
	RecoveryBlockedAt int64  `gorm:"not null;default:0"`
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
	BillingPreference string `json:",omitempty"`
	ChannelID         int    `json:",omitempty"`
	Group             string `json:",omitempty"`
	UserID            int
	RequestID         string
	ModelName         string
	Protocol          string
	PriceSnapshot     string
	TokenID           int
	Playground        bool
	Free              bool
	Amount            int64
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
	if input.ChannelID < 0 || len(input.Group) > 64 || input.UserID <= 0 || !validCreditID(input.RequestID, 128) || !validCreditID(input.ModelName, 256) || !validCreditID(input.Protocol, 32) || input.PriceSnapshot == "" || len(input.PriceSnapshot) > 65536 || input.Amount < 0 || input.Amount > common.MaxQuota || (input.Free && input.Amount != 0) || !validCreditTime(now) || (!input.Playground && input.TokenID <= 0) || (input.Playground && input.TokenID != 0) {
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
		var accountUser User
		if err := lockForUpdate(tx).Select("id", "accounting_version", "status").First(&accountUser, input.UserID).Error; err != nil {
			return err
		}
		if accountUser.AccountingVersion != 1 || accountUser.Status != common.UserStatusEnabled {
			return ErrCreditOperationRequired
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

		if !input.Free && input.Amount == 0 && input.TokenID > 0 {
			var token Token
			if err := lockForUpdate(tx).Where("id = ? AND user_id = ?", input.TokenID, input.UserID).First(&token).Error; err != nil {
				return err
			}
			if !token.UnlimitedQuota && token.RemainQuota <= 0 {
				return ErrCreditInsufficient
			}
		}
		if err := adjustCreditToken(tx, input.UserID, input.TokenID, input.Amount, true, now); err != nil {
			return err
		}
		request = CreditRequest{ChannelID: input.ChannelID, Group: input.Group, UserID: input.UserID, RequestID: input.RequestID, RequestDigest: digest, Fingerprint: fingerprint, ModelName: input.ModelName, Protocol: input.Protocol, PriceSnapshot: input.PriceSnapshot, RulesVersion: "accounting-v1", TokenID: input.TokenID, Reserved: input.Amount, State: "reserved", CreatedAt: now}
		if err := tx.Create(&request).Error; err != nil {
			return err
		}
		preference := input.BillingPreference
		if preference == "" || input.Free {
			preference = "wallet_only"
		}
		if preference != "wallet_only" && preference != "wallet_first" && preference != "subscription_only" && preference != "subscription_first" {
			return ErrCreditInvalid
		}
		if preference == "subscription_only" || preference == "subscription_first" {
			allowWallet, err := reserveSubscriptionRequestTx(tx, &request, input.Amount, now)
			if err == nil {
				return tx.Model(&request).Updates(map[string]any{"funding_source": request.FundingSource, "subscription_id": request.SubscriptionID}).Error
			}
			if (!errors.Is(err, ErrSubscriptionWindowInsufficient) && !errors.Is(err, ErrSubscriptionRightsUnavailable)) || preference == "subscription_only" || !allowWallet {
				return err
			}
		}
		err := tx.Transaction(func(wallet *gorm.DB) error {
			if !input.Free && input.Amount == 0 {
				packs, err := ListCreditPacks(wallet, input.UserID, now)
				if err != nil {
					return err
				}
				available := false
				for _, pack := range packs {
					if pack.UsableAt(now, CreditUseAPI) && pack.Available > 0 {
						available = true
						break
					}
				}
				if !available {
					return ErrCreditInsufficient
				}
			}
			reservation, err := ReserveCreditPacksTx(wallet, CreditReserve{UserID: input.UserID, RequestID: "request:" + digest, Amount: input.Amount, Purpose: CreditUseAPI}, now)
			if err != nil {
				return err
			}
			request.ReservationID, request.FundingSource = reservation.OperationID, CreditFundingSource
			if err := wallet.Model(&request).Updates(map[string]any{"funding_source": request.FundingSource, "reservation_id": request.ReservationID}).Error; err != nil {
				return err
			}
			return wallet.Create(&CreditRequestReservation{UserID: input.UserID, RequestID: request.ID, Target: input.Amount, ReservationID: reservation.OperationID, Amount: input.Amount, CreatedAt: now}).Error
		})
		if errors.Is(err, ErrCreditInsufficient) && preference == "wallet_first" {
			_, err = reserveSubscriptionRequestTx(tx, &request, input.Amount, now)
			if err == nil {
				return tx.Model(&request).Updates(map[string]any{"funding_source": request.FundingSource, "subscription_id": request.SubscriptionID}).Error
			}
		}
		return err
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
func GrowCreditRequestReservation(db *gorm.DB, userID int, requestID, target, now int64, execution ...CreditExecution) (CreditRequest, error) {
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
		if err := validateCreditExecution(request, now, execution); err != nil {
			return err
		}
		var reservations []CreditRequestReservation
		var err error
		if request.FundingSource != SubscriptionWindowFundingSource {
			reservations, err = creditRequestReservationsTx(tx, request)
			if err != nil {
				return err
			}
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
		delta := target - request.Reserved
		if request.FundingSource == SubscriptionWindowFundingSource {
			if err := growSubscriptionRequestTx(tx, request, target, now); err != nil {
				return err
			}
			if err := adjustCreditToken(tx, userID, request.TokenID, delta, true, now); err != nil {
				return err
			}
			request.Reserved = target
			return tx.Model(&request).Update("reserved", target).Error
		}
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
	scope := tx
	if !admission {
		// Settling an admitted request updates historical Key accounting without
		// restoring its credential. New admission keeps the soft-delete filter.
		scope = scope.Unscoped().Session(&gorm.Session{})
	}
	var token Token
	if err := lockForUpdate(scope).Where("id = ? AND user_id = ?", tokenID, userID).First(&token).Error; err != nil {
		return err
	}
	if admission && (token.Status != common.TokenStatusEnabled || token.ExpiredTime != -1 && token.ExpiredTime < now) {
		return ErrCreditOperationRequired
	}
	if admission && !token.UnlimitedQuota && int64(token.RemainQuota) < delta {
		return ErrCreditInsufficient
	}
	remain, used := int64(token.RemainQuota), int64(token.UsedQuota)
	if remain > common.MaxWalletQuota || remain < -common.MaxWalletQuota || used < 0 || used > common.MaxWalletQuota || delta > common.MaxWalletQuota-used || delta < -used || remain-delta > common.MaxWalletQuota || remain-delta < -common.MaxWalletQuota {
		return ErrCreditInvariant
	}
	result := scope.Model(&Token{}).Where("id = ? AND user_id = ?", tokenID, userID).Updates(map[string]any{"remain_quota": remain - delta, "used_quota": used + delta, "accessed_time": now})
	return result.Error
}

func invalidateCreditTokenCache(db *gorm.DB, tokenID int) {
	if tokenID == 0 || !common.RedisEnabled {
		return
	}
	var token Token
	if err := db.Unscoped().Select("id", commonKeyCol).First(&token, tokenID).Error; err != nil {
		common.SysError("cannot reload credit billing token for cache invalidation")
		return
	}
	if err := invalidateTokenCacheForMutation(token.Key); err != nil {
		common.SysError("cannot invalidate credit billing token cache")
	}
}

func MarkCreditRequestSubmitted(db *gorm.DB, userID int, requestID int64, now int64, execution ...CreditExecution) error {
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
		if err := validateCreditExecution(request, now, execution); err != nil {
			return err
		}
		if request.State != "reserved" && request.State != "executing" {
			return ErrCreditOperationConflict
		}
		if request.SubmittedAt != 0 {
			return nil
		}
		if request.FundingSource == SubscriptionWindowFundingSource {
			if err := confirmSubscriptionRequestTx(tx, request, now); err != nil {
				return err
			}
		}
		return tx.Model(&request).Updates(map[string]any{"state": "executing", "submitted_at": now}).Error
	})
}

func MarkCreditRequestReview(db *gorm.DB, userID int, requestID int64, execution ...CreditExecution) error {
	return MarkCreditRequestReviewAt(db, userID, requestID, common.GetTimestamp(), execution...)
}

func MarkCreditRequestReviewAt(db *gorm.DB, userID int, requestID, now int64, execution ...CreditExecution) error {
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
		if err := validateCreditExecution(request, now, execution); err != nil {
			return err
		}
		if request.State != "reserved" && request.State != "executing" && request.State != "review" {
			return ErrCreditOperationConflict
		}
		if request.ReviewEvidenceID > 0 {
			return ErrCreditOperationConflict
		}
		return tx.Model(&request).Update("state", "review").Error
	})
}

// FinishCreditRequest records the intent before changing funds. A failed final
// transaction leaves the intent and hold available to persistent recovery.
func FinishCreditRequest(db *gorm.DB, userID int, requestID int64, kind string, actual int64, now int64, execution ...CreditExecution) (CreditRequest, error) {
	var request CreditRequest
	completed := false
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
		if request.State == "settled" || request.State == "released" {
			if request.IntentKind != kind || request.Actual != actual {
				return ErrCreditOperationConflict
			}
			if len(execution) != 0 {
				if err := validateCreditExecution(request, now, execution); err != nil {
					return err
				}
			}
			completed = true
			return nil
		}
		if err := validateCreditExecution(request, now, execution); err != nil {
			return err
		}
		if request.ReviewEvidenceID > 0 && kind != "settle" {
			return ErrCreditOperationConflict
		}
		if kind == "settle" && (request.UsageEvidenceID != 0 || request.ReviewEvidenceID != 0) {
			_, evidence, err := creditConsumeEvidence(tx, request)
			if err != nil {
				return err
			}
			if evidence.Consume.ReferenceQuota != actual {
				return ErrCreditOperationConflict
			}
			if actual == 0 && !evidence.Consume.ZeroChargeEstablished {
				request.State = "review"
				return tx.Model(&request).Update("state", "review").Error
			}
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
	if completed {
		return request, nil
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
		if err := validateCreditExecution(request, now, execution); err != nil {
			return err
		}
		if request.State == "settled" || request.State == "released" {
			return nil
		}
		if request.State != "pending" || request.IntentKind != kind || request.Actual != actual {
			return ErrCreditOperationConflict
		}
		maximumCharge := actual
		if request.TokenID > 0 {
			var token Token
			if err := lockForUpdate(tx.Unscoped()).Where("id = ? AND user_id = ?", request.TokenID, userID).First(&token).Error; err != nil {
				return err
			}
			if !token.UnlimitedQuota {
				if token.RemainQuota < 0 || int64(token.RemainQuota) > common.MaxWalletQuota {
					return ErrCreditInvariant
				}
				maximumCharge = min(actual, request.Reserved+int64(token.RemainQuota))
			}
		}
		var charged int64
		if request.FundingSource == SubscriptionWindowFundingSource {
			var err error
			charged, err = finalizeSubscriptionRequestTx(tx, request, maximumCharge, now)
			if err != nil {
				return err
			}
		} else {
			reservations, err := creditRequestReservationsTx(tx, request)
			if err != nil {
				return err
			}
			heldCharge := min(maximumCharge, request.Reserved)
			remaining := heldCharge
			for _, reservation := range reservations {
				charged := min(remaining, reservation.Amount)
				if _, err := FinalizeCreditReservationTx(tx, CreditFinalize{UserID: userID, ReservationID: reservation.ReservationID, Kind: kind, Actual: charged}, now); err != nil {
					return err
				}
				remaining -= charged
			}
			charged = heldCharge
			if maximumCharge > charged {
				packs, err := ListCreditPacks(tx, userID, now)
				if err != nil {
					return err
				}
				var extra int64
				for _, pack := range packs {
					if !pack.UsableAt(now, CreditUseAPI) {
						continue
					}
					extra += min(pack.Available, maximumCharge-charged-extra)
					if extra == maximumCharge-charged {
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
		}
		uncollected := actual - charged
		if err := adjustCreditToken(tx, userID, request.TokenID, charged-request.Reserved, false, now); err != nil {
			return err
		}
		if request.TaskID != "" {
			if err := validateCreditTaskCompletion(tx, request); err != nil {
				return err
			}
			var result *gorm.DB
			switch request.TaskKind {
			case "task":
				result = tx.Model(&Task{}).Where("id = ? AND user_id = ?", request.TaskRowID, userID).Update("quota", charged)
			case "midjourney":
				result = tx.Model(&Midjourney{}).Where("id = ? AND user_id = ?", request.TaskRowID, userID).Update("quota", charged)
			default:
				return ErrCreditInvariant
			}
			if result.Error != nil {
				return result.Error
			}
		}
		if kind == "settle" {
			if err := createCreditConsumeProjectionTx(tx, request, charged, uncollected, now); err != nil {
				return err
			}
		}
		state := "settled"
		if kind == "release" {
			state = "released"
		}
		if err := tx.Model(&request).Updates(map[string]any{"state": state, "charged": charged, "uncollected": uncollected, "finished_at": now}).Error; err != nil {
			return err
		}
		request.State, request.Charged, request.Uncollected, request.FinishedAt = state, charged, uncollected, now
		return nil
	})
	if err == nil {
		invalidateCreditTokenCache(db, request.TokenID)
	}
	return request, err
}

// The task row and its host bill are bound in the same transaction. Account
// serialization prevents two requests from claiming the same user's task.
func bindCreditTaskTx(tx *gorm.DB, userID int, requestID int64, taskID, taskKind string, tokenID int, execution ...CreditExecution) error {
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
	if err := validateCreditExecution(request, common.GetTimestamp(), execution); err != nil {
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
		terminal := task.Status == TaskStatusSuccess || request.ReviewEvidenceID > 0 && task.Status == TaskStatusFailure
		if task.TaskID != request.TaskID || task.PrivateData.CreditRequestID != request.ID || task.PrivateData.BillingSource != CreditFundingSource || !terminal {
			return ErrCreditOperationConflict
		}
	case "midjourney":
		var task Midjourney
		if err := lockForUpdate(tx).Where("id = ? AND user_id = ?", request.TaskRowID, request.UserID).First(&task).Error; err != nil {
			return err
		}
		terminal := task.Status == "SUCCESS" || request.ReviewEvidenceID > 0 && task.Status == "FAILURE"
		if task.MjId != request.TaskID || task.CreditRequestID != request.ID || !terminal {
			return ErrCreditOperationConflict
		}
	default:
		return ErrCreditInvariant
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
// Additional funding and platform shortfalls belong to the owning request.
// It records them explicitly rather than overdrawing these packs.
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

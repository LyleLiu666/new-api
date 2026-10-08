package model

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"

	"github.com/shopspring/decimal"

	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var (
	ErrSubscriptionPurchaseConflict    = errors.New("subscription purchase conflict")
	ErrSubscriptionPurchaseUnavailable = errors.New("subscription purchase unavailable")
)

// Purchase contracts are separate from legacy mutable-plan orders. Recording
// a payment does not activate rights or select renewal/refund policies.
type SubscriptionPurchaseOrder struct {
	ID                int64  `json:"id" gorm:"primaryKey"`
	UserID            int    `json:"user_id" gorm:"not null;index"`
	PlanID            int    `json:"plan_id" gorm:"not null;index"`
	VersionID         int64  `json:"version_id" gorm:"not null;index"`
	TradeNo           string `json:"trade_no" gorm:"size:128;not null;uniqueIndex"`
	Provider          string `json:"provider" gorm:"size:50;not null"`
	ContractSnapshot  string `json:"contract_snapshot" gorm:"type:text;not null"`
	ContractDigest    string `json:"contract_digest" gorm:"size:64;not null"`
	BalanceQuota      int64  `json:"balance_quota" gorm:"not null;default:0"`
	BalanceQuotaUnit  string `json:"balance_quota_unit" gorm:"size:64;not null;default:''"`
	PriceMicros       int64  `json:"price_micros" gorm:"not null"`
	Currency          string `json:"currency" gorm:"size:8;not null"`
	DurationSeconds   int64  `json:"duration_seconds" gorm:"not null"`
	EventID           string `json:"event_id" gorm:"size:128;not null"`
	EventDigest       string `json:"-" gorm:"size:64;not null;uniqueIndex"`
	InputDigest       string `json:"-" gorm:"size:64;not null"`
	CreatedAt         int64  `json:"created_at" gorm:"not null"`
	ExpiresAt         int64  `json:"expires_at" gorm:"not null"`
	PaymentState      string `json:"payment_state" gorm:"size:16;not null"`
	PaidAt            int64  `json:"paid_at" gorm:"not null"`
	PaymentReference  string `json:"payment_reference" gorm:"size:128;not null"`
	NeedsReview       bool   `json:"needs_review" gorm:"not null"`
	LastReviewReason  string `json:"last_review_reason" gorm:"size:64;not null"`
	RightsCancelledAt int64  `json:"rights_cancelled_at" gorm:"not null;default:0"`
	CheckoutState     string `json:"checkout_state" gorm:"size:16;not null;default:''"`
	CheckoutID        string `json:"checkout_id" gorm:"size:128;not null;default:''"`
	CheckoutDigest    string `json:"-" gorm:"size:64;not null;default:''"`
	CheckoutResponse  string `json:"-" gorm:"type:text"`
}

type SubscriptionPurchaseInput struct {
	UserID    int
	VersionID int64
	Provider  string
	EventID   string
	ExpiresAt int64
}

// Cash checkout uses a server-owned one-hour deadline, retained on retry. The
// caller sends the gateway request only after this contract commits.
func CreateVersionedSubscriptionCheckout(db *gorm.DB, userID int, versionID int64, provider, eventID string, now int64) (SubscriptionPurchaseOrder, error) {
	var order SubscriptionPurchaseOrder
	if db == nil || userID <= 0 || versionID <= 0 || provider == PaymentMethodBalance || !validCreditID(provider, 50) || !validCreditID(eventID, 128) || !validCreditTime(now) || now > common.MaxWalletQuota-3600 {
		return order, ErrCreditInvalid
	}
	err := db.Transaction(func(tx *gorm.DB) error {
		if err := lockCreditAccount(tx, userID, true); err != nil {
			return err
		}
		var user User
		if err := lockForUpdate(tx).First(&user, userID).Error; err != nil {
			return err
		}
		if user.AccountingVersion != 1 || user.Status != common.UserStatusEnabled {
			return ErrCreditOperationRequired
		}
		digest, err := creditDigest([]string{"subscription_purchase", fmt.Sprint(userID), eventID})
		if err != nil {
			return err
		}
		var previous SubscriptionPurchaseOrder
		found := tx.Where("event_digest = ?", digest).Limit(1).Find(&previous)
		if found.Error != nil {
			return found.Error
		}
		deadline := now + 3600
		if found.RowsAffected > 0 {
			deadline = previous.ExpiresAt
		}
		order, err = CreateSubscriptionPurchaseOrder(tx, SubscriptionPurchaseInput{UserID: userID, VersionID: versionID, Provider: provider, EventID: eventID, ExpiresAt: deadline}, now)
		return err
	})
	return order, err
}

// The caller supplies an explicit server-side deadline. No checkout is sent
// and no credits are deducted until a purchase contract commits successfully.
func CreateSubscriptionPurchaseOrder(db *gorm.DB, input SubscriptionPurchaseInput, now int64) (SubscriptionPurchaseOrder, error) {
	var order SubscriptionPurchaseOrder
	if db == nil || input.UserID <= 0 || input.VersionID <= 0 || !validCreditID(input.Provider, 50) || !validCreditID(input.EventID, 128) || !validCreditTime(now) || !validCreditTime(input.ExpiresAt) {
		return order, ErrCreditInvalid
	}
	eventDigest, err := creditDigest([]string{"subscription_purchase", fmt.Sprint(input.UserID), input.EventID})
	if err != nil {
		return order, err
	}
	inputDigest, err := creditDigest(input)
	if err != nil {
		return order, err
	}
	err = db.Transaction(func(tx *gorm.DB) error {
		if err := lockCreditAccount(tx, input.UserID, true); err != nil {
			return err
		}
		// Write first for SQLite, then read current identity. Account serialization
		// also arbitrates repeated purchase events for the same user.
		if err := tx.Model(&User{}).Where("id = ?", input.UserID).UpdateColumn("id", gorm.Expr("id")).Error; err != nil {
			return err
		}
		var user User
		if err := tx.Select("id", "status").First(&user, input.UserID).Error; err != nil {
			return err
		}
		if user.Status != common.UserStatusEnabled {
			return ErrUserQuotaPermission
		}
		found := tx.Where("event_digest = ?", eventDigest).Limit(1).Find(&order)
		if found.Error != nil {
			return found.Error
		}
		if found.RowsAffected != 0 {
			if order.InputDigest != inputDigest {
				return ErrSubscriptionPurchaseConflict
			}
			return nil
		}
		if input.ExpiresAt <= now {
			return ErrCreditInvalid
		}
		var version SubscriptionPlanVersion
		if err := tx.First(&version, input.VersionID).Error; err != nil {
			return err
		}
		// Keep the catalog decision atomic with publication and existing edits.
		if err := tx.Model(&SubscriptionPlan{}).Where("id = ?", version.PlanID).UpdateColumn("id", gorm.Expr("id")).Error; err != nil {
			return err
		}
		var plan SubscriptionPlan
		if err := lockForUpdate(tx).First(&plan, version.PlanID).Error; err != nil {
			return err
		}
		if !plan.Enabled {
			return ErrSubscriptionPurchaseUnavailable
		}
		var latest SubscriptionPlanVersion
		if err := lockForUpdate(tx).Where("plan_id = ?", plan.Id).Order("revision desc").First(&latest).Error; err != nil {
			return err
		}
		if latest.ID != version.ID {
			return ErrSubscriptionPurchaseConflict
		}
		snapshotDigest := sha256.Sum256([]byte(version.Snapshot))
		if hex.EncodeToString(snapshotDigest[:]) != version.SnapshotDigest || version.PriceMicros < 0 || version.PriceMicros > common.MaxWalletQuota || version.DurationSeconds != 30*24*3600 {
			return ErrCreditInvalid
		}
		order = SubscriptionPurchaseOrder{UserID: input.UserID, PlanID: version.PlanID, VersionID: version.ID, TradeNo: "SUB-V1-" + eventDigest, Provider: input.Provider, ContractSnapshot: version.Snapshot, ContractDigest: version.SnapshotDigest, PriceMicros: version.PriceMicros, Currency: version.Currency, DurationSeconds: version.DurationSeconds, EventID: input.EventID, EventDigest: eventDigest, InputDigest: inputDigest, CreatedAt: now, ExpiresAt: input.ExpiresAt, PaymentState: "pending"}
		if input.Provider == PaymentMethodBalance {
			var contract SubscriptionPlan
			if err := common.UnmarshalJsonStr(order.ContractSnapshot, &contract); err != nil {
				return err
			}
			if order.Currency != "USD" || contract.AllowBalancePay == nil || !*contract.AllowBalancePay {
				return ErrSubscriptionPurchaseUnavailable
			}
			if common.QuotaPerUnit <= 0 || math.IsNaN(common.QuotaPerUnit) || math.IsInf(common.QuotaPerUnit, 0) {
				return ErrCreditInvalid
			}
			unit := decimal.NewFromFloat(common.QuotaPerUnit)
			quota, err := common.WalletQuotaFromDecimalStrict(decimal.NewFromInt(order.PriceMicros).Shift(-6).Mul(unit).Ceil())
			if err != nil {
				return err
			}
			order.BalanceQuota, order.BalanceQuotaUnit = int64(quota), unit.String()
		}
		return tx.Create(&order).Error
	})
	return order, err
}

// A fact is append-only. Unknown amount/time remain NULL, rather than being
// synthesized from the order or notification receipt time.
type SubscriptionPaymentFact struct {
	ID             int64  `json:"id" gorm:"primaryKey"`
	OriginalFactID int64  `json:"original_fact_id" gorm:"not null;default:0;index"`
	OrderID        int64  `json:"order_id" gorm:"not null;index"`
	Provider       string `json:"provider" gorm:"size:50;not null"`
	EventID        string `json:"event_id" gorm:"size:128;not null"`
	EventDigest    string `json:"-" gorm:"size:64;not null;uniqueIndex"`
	InputDigest    string `json:"-" gorm:"size:64;not null"`
	ReferenceID    string `json:"reference_id" gorm:"size:128;not null"`
	BuyerID        int    `json:"buyer_id" gorm:"not null"`
	AmountMicros   *int64 `json:"amount_micros"`
	Currency       string `json:"currency" gorm:"size:8;not null"`
	PaidAt         *int64 `json:"paid_at"`
	PaidAtSource   string `json:"paid_at_source" gorm:"size:128;not null"`
	Succeeded      bool   `json:"succeeded" gorm:"not null"`
	EvidenceDigest string `json:"evidence_digest" gorm:"size:64;not null"`
	ObservedAt     int64  `json:"observed_at" gorm:"not null"`
	Outcome        string `json:"outcome" gorm:"size:16;not null"`
	ReviewReason   string `json:"review_reason" gorm:"size:64;not null"`
}

// Provider transaction ownership is independent of event IDs: one transaction
// can produce several notifications, but can belong to only one local order.
type SubscriptionPaymentClaim struct {
	ID          int64  `json:"id" gorm:"primaryKey"`
	Digest      string `json:"-" gorm:"size:64;not null;uniqueIndex"`
	Provider    string `json:"provider" gorm:"size:50;not null"`
	ReferenceID string `json:"reference_id" gorm:"size:128;not null"`
	OrderID     int64  `json:"order_id" gorm:"not null;index"`
	CreatedAt   int64  `json:"created_at" gorm:"not null"`
}

// This host-only input is constructed AFTER gateway signature verification and
// payment lookup. It is not a management API DTO or a client success assertion.
type VerifiedSubscriptionPayment struct {
	OrderID        int64
	Provider       string
	EventID        string
	ReferenceID    string
	BuyerID        int
	AmountMicros   *int64
	Currency       string
	PaidAt         *int64
	PaidAtSource   string
	Succeeded      bool
	EvidenceDigest string
}

func RecordSubscriptionPaymentFact(db *gorm.DB, input VerifiedSubscriptionPayment, observedAt int64) (SubscriptionPaymentFact, error) {
	var fact SubscriptionPaymentFact
	if db == nil || input.OrderID <= 0 || !validCreditID(input.Provider, 50) || !validCreditID(input.EventID, 128) || len(input.ReferenceID) > 128 || len(input.Currency) > 8 || len(input.PaidAtSource) > 128 || len(input.EvidenceDigest) != 64 || !validCreditTime(observedAt) {
		return fact, ErrCreditInvalid
	}
	if _, err := hex.DecodeString(input.EvidenceDigest); err != nil {
		return fact, ErrCreditInvalid
	}
	eventDigest, err := creditDigest([]string{"subscription_payment", input.Provider, input.EventID})
	if err != nil {
		return fact, err
	}
	inputDigest, err := creditDigest(input)
	if err != nil {
		return fact, err
	}
	observation := SubscriptionPaymentFact{OrderID: input.OrderID, Provider: input.Provider, EventID: input.EventID, EventDigest: eventDigest, InputDigest: inputDigest, ReferenceID: input.ReferenceID, BuyerID: input.BuyerID, AmountMicros: input.AmountMicros, Currency: input.Currency, PaidAt: input.PaidAt, PaidAtSource: input.PaidAtSource, Succeeded: input.Succeeded, EvidenceDigest: input.EvidenceDigest, ObservedAt: observedAt, Outcome: "review"}
	var paymentConflict bool
	err = db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&SubscriptionPurchaseOrder{}).Where("id = ?", input.OrderID).UpdateColumn("id", gorm.Expr("id")).Error; err != nil {
			return err
		}
		var order SubscriptionPurchaseOrder
		if err := tx.First(&order, input.OrderID).Error; err != nil {
			return err
		}
		if order.Provider != input.Provider {
			return ErrPaymentMethodMismatch
		}
		found := tx.Where("event_digest = ?", eventDigest).Limit(1).Find(&fact)
		if found.Error != nil {
			return found.Error
		}
		if found.RowsAffected != 0 {
			if fact.InputDigest != inputDigest {
				paymentConflict = true
				var err error
				fact, err = recordSubscriptionPaymentConflictTx(tx, observation, fact.ID)
				return err
			}
			return nil
		}
		fact = observation
		switch {
		case !input.Succeeded:
			fact.ReviewReason = "payment_not_succeeded"
		case input.BuyerID != order.UserID:
			fact.ReviewReason = "buyer_mismatch"
		case !validCreditID(input.ReferenceID, 128):
			fact.ReviewReason = "missing_payment_reference"
		case input.AmountMicros == nil:
			fact.ReviewReason = "missing_amount"
		case *input.AmountMicros != order.PriceMicros:
			fact.ReviewReason = "amount_mismatch"
		case input.Currency != order.Currency:
			fact.ReviewReason = "currency_mismatch"
		case input.PaidAt == nil || !validCreditID(input.PaidAtSource, 128):
			fact.ReviewReason = "missing_paid_at"
		case !validCreditTime(*input.PaidAt) || *input.PaidAt < order.CreatedAt || *input.PaidAt >= order.ExpiresAt || *input.PaidAt > observedAt:
			fact.ReviewReason = "paid_at_outside_order"
		case order.PaymentState == "verified" && (order.PaymentReference != input.ReferenceID || order.PaidAt != *input.PaidAt):
			fact.ReviewReason = "conflicting_payment"
		default:
			fact.Outcome = "verified"
		}
		// The event constraint handles notifications that race on different
		// orders; always read its winner with current-read semantics on MySQL.
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&fact).Error; err != nil {
			return err
		}
		var committed SubscriptionPaymentFact
		if err := lockForUpdate(tx).Where("event_digest = ?", eventDigest).First(&committed).Error; err != nil {
			return err
		}
		if committed.InputDigest != inputDigest {
			paymentConflict = true
			var err error
			fact, err = recordSubscriptionPaymentConflictTx(tx, observation, committed.ID)
			return err
		}
		fact = committed
		if fact.Outcome == "verified" {
			paymentDigest, err := creditDigest([]string{"subscription_payment_reference", input.Provider, input.ReferenceID})
			if err != nil {
				return err
			}
			claim := SubscriptionPaymentClaim{Digest: paymentDigest, Provider: input.Provider, ReferenceID: input.ReferenceID, OrderID: order.ID, CreatedAt: observedAt}
			if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&claim).Error; err != nil {
				return err
			}
			var winner SubscriptionPaymentClaim
			if err := lockForUpdate(tx).Where("digest = ?", paymentDigest).First(&winner).Error; err != nil {
				return err
			}
			if winner.OrderID != order.ID {
				fact.Outcome, fact.ReviewReason = "review", "payment_already_claimed"
				if err := tx.Model(&fact).Updates(map[string]any{"outcome": fact.Outcome, "review_reason": fact.ReviewReason}).Error; err != nil {
					return err
				}
			}
		}
		if fact.Outcome == "review" {
			return tx.Model(&order).Updates(map[string]any{"needs_review": true, "last_review_reason": fact.ReviewReason}).Error
		}
		return tx.Model(&order).Updates(map[string]any{"payment_state": "verified", "paid_at": *input.PaidAt, "payment_reference": input.ReferenceID}).Error
	})
	// Conflict is a domain result AFTER the contradictory evidence and review
	// flag commit. Returning it inside the transaction would erase the audit.
	if err == nil && paymentConflict {
		err = ErrSubscriptionPurchaseConflict
	}
	return fact, err
}

func recordSubscriptionPaymentConflictTx(tx *gorm.DB, observation SubscriptionPaymentFact, originalID int64) (SubscriptionPaymentFact, error) {
	conflictDigest, err := creditDigest([]string{"subscription_payment_conflict", observation.EventDigest, observation.InputDigest})
	if err != nil {
		return observation, err
	}
	observation.EventDigest = conflictDigest
	observation.OriginalFactID = originalID
	observation.Outcome, observation.ReviewReason = "review", "event_payload_conflict"
	if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&observation).Error; err != nil {
		return observation, err
	}
	var committed SubscriptionPaymentFact
	if err := lockForUpdate(tx).Where("event_digest = ?", conflictDigest).First(&committed).Error; err != nil {
		return observation, err
	}
	if committed.InputDigest != observation.InputDigest || committed.OriginalFactID != originalID {
		return observation, ErrSubscriptionPurchaseConflict
	}
	err = tx.Model(&SubscriptionPurchaseOrder{}).Where("id = ?", observation.OrderID).Updates(map[string]any{"needs_review": true, "last_review_reason": observation.ReviewReason}).Error
	return committed, err
}

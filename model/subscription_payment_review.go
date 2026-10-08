package model

import (
	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm"
)

// Reconciliation supplies missing cash facts from an administrator's external
// payment evidence. It is not a user payment assertion or a cash refund.
type SubscriptionPaymentReview struct {
	OrderID           int64  `json:"order_id"`
	ActorID           int    `json:"-"`
	EventID           string `json:"event_id"`
	ExpectedFactID    int64  `json:"expected_fact_id"`
	ReferenceID       string `json:"reference_id"`
	AmountMicros      int64  `json:"amount_micros"`
	Currency          string `json:"currency"`
	PaidAt            int64  `json:"paid_at"`
	EvidenceReference string `json:"evidence_reference"`
	Reason            string `json:"reason"`
}

type SubscriptionPaymentReviewResult struct {
	OrderID           int64  `json:"order_id"`
	PaymentFactID     int64  `json:"payment_fact_id"`
	SubscriptionID    int    `json:"subscription_id"`
	ActorID           int    `json:"actor_id"`
	ResolvedAt        int64  `json:"resolved_at"`
	EvidenceReference string `json:"evidence_reference"`
	Reason            string `json:"reason"`
}

func ResolveSubscriptionPaymentReview(db *gorm.DB, input SubscriptionPaymentReview, now int64) (SubscriptionPaymentReviewResult, error) {
	var result SubscriptionPaymentReviewResult
	if db == nil || input.OrderID <= 0 || input.ActorID <= 0 || input.ExpectedFactID < 0 || !validCreditID(input.EventID, 128) || !validCreditID(input.ReferenceID, 128) || !validCreditID(input.EvidenceReference, 1024) || !validCreditID(input.Reason, 1024) || input.AmountMicros < 0 || input.AmountMicros > common.MaxWalletQuota || !validCreditTime(input.PaidAt) || !validCreditTime(now) {
		return result, ErrCreditInvalid
	}
	key, err := creditDigest([]string{"subscription_payment_review", input.EventID})
	if err != nil {
		return result, err
	}
	fingerprint, err := creditDigest(struct {
		Input SubscriptionPaymentReview
		Actor int
	}{input, input.ActorID})
	if err != nil {
		return result, err
	}
	var owner SubscriptionPurchaseOrder
	if err := db.Select("id", "user_id").First(&owner, input.OrderID).Error; err != nil {
		return result, err
	}
	err = db.Transaction(func(tx *gorm.DB) error {
		if err := lockCreditAccount(tx, owner.UserID, true); err != nil {
			return err
		}
		if err := AuthorizeCreditAccountAdmin(tx, input.ActorID, owner.UserID); err != nil {
			return err
		}
		previous, err := findCreditOperation(tx, owner.UserID, key, fingerprint)
		if err != nil {
			return err
		}
		if previous != nil {
			return common.UnmarshalJsonStr(previous.Result, &result)
		}
		var order SubscriptionPurchaseOrder
		if err := lockForUpdate(tx).First(&order, input.OrderID).Error; err != nil {
			return err
		}
		if order.Provider == PaymentMethodBalance || order.RightsCancelledAt > 0 || (!order.NeedsReview && order.CheckoutState != "started" && order.CheckoutState != "unknown") {
			return ErrSubscriptionPurchaseUnavailable
		}
		// A callback can commit after role lookup established a MySQL read
		// snapshot. Use a current row read under the order lock, not MAX from
		// that older snapshot, before clearing the reviewed state.
		var latest SubscriptionPaymentFact
		if err := lockForUpdate(tx).Where("order_id = ?", order.ID).Order("id desc").Limit(1).Find(&latest).Error; err != nil {
			return err
		}
		if latest.ID != input.ExpectedFactID {
			return ErrSubscriptionPurchaseConflict
		}
		if input.AmountMicros != order.PriceMicros || input.Currency != order.Currency || input.PaidAt < order.CreatedAt || input.PaidAt >= order.ExpiresAt || input.PaidAt > now {
			return ErrCreditInvalid
		}
		evidenceDigest, err := creditDigest([]string{input.EvidenceReference, input.Reason})
		if err != nil {
			return err
		}
		payment := VerifiedSubscriptionPayment{OrderID: order.ID, Provider: order.Provider, EventID: "admin:" + key, ReferenceID: input.ReferenceID, BuyerID: order.UserID, AmountMicros: &input.AmountMicros, Currency: input.Currency, PaidAt: &input.PaidAt, PaidAtSource: "admin.manual_evidence", Succeeded: true, EvidenceDigest: evidenceDigest}
		fact, err := RecordSubscriptionPaymentFact(tx, payment, now)
		if err != nil {
			return err
		}
		if fact.Outcome != "verified" {
			return ErrSubscriptionPurchaseConflict
		}
		if err := tx.Model(&order).Updates(map[string]any{"needs_review": false, "last_review_reason": ""}).Error; err != nil {
			return err
		}
		rights, err := ActivateSubscriptionPurchaseTx(tx, order.UserID, order.ID, now)
		if err != nil {
			return err
		}
		result = SubscriptionPaymentReviewResult{OrderID: order.ID, PaymentFactID: fact.ID, SubscriptionID: rights.Id, ActorID: input.ActorID, ResolvedAt: now, EvidenceReference: input.EvidenceReference, Reason: input.Reason}
		data, err := common.Marshal(result)
		if err != nil {
			return err
		}
		return tx.Create(&CreditOperation{UserID: order.UserID, KeyDigest: key, Fingerprint: fingerprint, Kind: "payment_review", CreatedAt: now, Result: string(data)}).Error
	})
	if err == nil && db == DB {
		refreshSubscriptionUserGroupCache(owner.UserID, "subscription payment reconciliation")
	}
	return result, err
}

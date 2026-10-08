package model

import (
	"encoding/hex"

	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

// A provider without durable create idempotency gets one issuance attempt.
// A crashed/unknown attempt stays blocked for reconciliation; it cannot be
// made safe by retrying cash checkout creation with a new remote session.
func BeginSubscriptionCheckout(db *gorm.DB, userID int, orderID int64, digest string, now int64) (SubscriptionPurchaseOrder, bool, error) {
	var order SubscriptionPurchaseOrder
	if db == nil || userID <= 0 || orderID <= 0 || len(digest) != 64 || !validCreditTime(now) {
		return order, false, ErrCreditInvalid
	}
	if _, err := hex.DecodeString(digest); err != nil {
		return order, false, ErrCreditInvalid
	}
	issue := false
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
		if err := lockForUpdate(tx).Where("id = ? AND user_id = ?", orderID, userID).First(&order).Error; err != nil {
			return err
		}
		if order.CheckoutDigest != "" && order.CheckoutDigest != digest {
			return ErrSubscriptionPurchaseConflict
		}
		if order.NeedsReview || order.PaymentState != "pending" || now >= order.ExpiresAt {
			return ErrSubscriptionPurchaseUnavailable
		}
		if order.CheckoutState == "ready" {
			return nil
		}
		if order.CheckoutState != "" {
			return ErrSubscriptionPurchaseUnavailable
		}
		order.CheckoutState, order.CheckoutDigest = "started", digest
		if err := tx.Model(&order).Updates(map[string]any{"checkout_state": order.CheckoutState, "checkout_digest": digest}).Error; err != nil {
			return err
		}
		issue = true
		return nil
	})
	return order, issue, err
}

// Save the remote result even when cancellation occurred during network I/O.
// It is an audit of issuance, not a right to pay or an activation operation.
func SaveSubscriptionCheckout(db *gorm.DB, userID int, orderID int64, digest, checkoutID, response string) (SubscriptionPurchaseOrder, error) {
	var order SubscriptionPurchaseOrder
	if db == nil || userID <= 0 || orderID <= 0 || !validCreditID(checkoutID, 128) || len(response) == 0 || len(response) > 16*1024 {
		return order, ErrCreditInvalid
	}
	err := db.Transaction(func(tx *gorm.DB) error {
		if err := lockCreditAccount(tx, userID, true); err != nil {
			return err
		}
		if err := lockForUpdate(tx).Where("id = ? AND user_id = ?", orderID, userID).First(&order).Error; err != nil {
			return err
		}
		if order.CheckoutDigest != digest {
			return ErrSubscriptionPurchaseConflict
		}
		if order.CheckoutState == "ready" && order.CheckoutID == checkoutID && order.CheckoutResponse == response {
			return nil
		}
		if order.CheckoutState != "started" {
			return ErrSubscriptionPurchaseUnavailable
		}
		order.CheckoutState, order.CheckoutID, order.CheckoutResponse = "ready", checkoutID, response
		// Even DEBUG SQL diagnostics must not interpolate payment links or
		// embedded channel credentials. The caller records the failure stage;
		// this single sensitive mutation is excluded from SQL tracing.
		return tx.Session(&gorm.Session{Logger: gormlogger.Discard}).Model(&order).Updates(map[string]any{"checkout_state": order.CheckoutState, "checkout_id": checkoutID, "checkout_response": response}).Error
	})
	return order, err
}

func MarkSubscriptionCheckoutUnknown(db *gorm.DB, userID int, orderID int64, digest string) error {
	if db == nil || userID <= 0 || orderID <= 0 {
		return ErrCreditInvalid
	}
	return db.Transaction(func(tx *gorm.DB) error {
		if err := lockCreditAccount(tx, userID, true); err != nil {
			return err
		}
		var order SubscriptionPurchaseOrder
		if err := lockForUpdate(tx).Where("id = ? AND user_id = ?", orderID, userID).First(&order).Error; err != nil {
			return err
		}
		if order.CheckoutDigest != digest || (order.CheckoutState != "started" && order.CheckoutState != "unknown") {
			return ErrSubscriptionPurchaseConflict
		}
		if !order.NeedsReview {
			order.LastReviewReason = "checkout_result_unknown"
		}
		return tx.Model(&order).Updates(map[string]any{"checkout_state": "unknown", "needs_review": true, "last_review_reason": order.LastReviewReason}).Error
	})
}

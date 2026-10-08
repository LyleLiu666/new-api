package model

import (
	"fmt"

	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm"
)

type SubscriptionBalancePurchase struct {
	UserID    int
	VersionID int64
	EventID   string
}

// A balance purchase commits its locked contract, eligible FEFO debit, payment
// evidence and rights together. It never debits an API key or the legacy sum.
func PurchaseVersionedSubscriptionWithBalance(db *gorm.DB, input SubscriptionBalancePurchase, now int64) (UserSubscription, error) {
	var rights UserSubscription
	if db == nil || input.UserID <= 0 || input.VersionID <= 0 || !validCreditID(input.EventID, 128) || !validCreditTime(now) || now > common.MaxWalletQuota-1800 {
		return rights, ErrCreditInvalid
	}
	err := db.Transaction(func(tx *gorm.DB) error {
		if err := lockCreditAccount(tx, input.UserID, true); err != nil {
			return err
		}
		var user User
		if err := lockForUpdate(tx).First(&user, input.UserID).Error; err != nil {
			return err
		}
		if user.AccountingVersion != 1 || user.Status != common.UserStatusEnabled {
			return ErrCreditOperationRequired
		}
		digest, err := creditDigest([]string{"subscription_purchase", fmt.Sprint(input.UserID), input.EventID})
		if err != nil {
			return err
		}
		var previous SubscriptionPurchaseOrder
		found := tx.Where("event_digest = ?", digest).Limit(1).Find(&previous)
		if found.Error != nil {
			return found.Error
		}
		deadline := now + 1800
		if found.RowsAffected > 0 {
			deadline = previous.ExpiresAt
		}
		order, err := CreateSubscriptionPurchaseOrder(tx, SubscriptionPurchaseInput{UserID: input.UserID, VersionID: input.VersionID, Provider: PaymentMethodBalance, EventID: input.EventID, ExpiresAt: deadline}, now)
		if err != nil {
			return err
		}
		found = tx.Where("purchase_order_id = ? AND user_id = ?", order.ID, input.UserID).Limit(1).Find(&rights)
		if found.Error != nil {
			return found.Error
		}
		if found.RowsAffected > 0 {
			return nil
		}
		if order.PaymentState != "pending" || order.NeedsReview || now >= order.ExpiresAt {
			return ErrSubscriptionPurchaseUnavailable
		}
		reservation, err := ReserveCreditPacksTx(tx, CreditReserve{UserID: input.UserID, RequestID: "subscription:" + order.EventDigest, Amount: order.BalanceQuota, Purpose: CreditUseSubscription}, now)
		if err != nil {
			return err
		}
		if _, err = FinalizeCreditReservationTx(tx, CreditFinalize{UserID: input.UserID, ReservationID: reservation.OperationID, Kind: "settle", Actual: order.BalanceQuota}, now); err != nil {
			return err
		}
		evidence, err := creditDigest(struct {
			Order  int64
			Quota  int64
			Unit   string
			PaidAt int64
		}{order.ID, order.BalanceQuota, order.BalanceQuotaUnit, now})
		if err != nil {
			return err
		}
		fact, err := RecordSubscriptionPaymentFact(tx, VerifiedSubscriptionPayment{OrderID: order.ID, Provider: PaymentMethodBalance, EventID: "balance:" + order.EventDigest, ReferenceID: "balance:" + order.EventDigest, BuyerID: input.UserID, AmountMicros: common.GetPointer(order.PriceMicros), Currency: order.Currency, PaidAt: common.GetPointer(now), PaidAtSource: "primary_database_balance_commit", EvidenceDigest: evidence, Succeeded: true}, now)
		if err != nil {
			return err
		}
		if fact.Outcome != "verified" {
			return ErrSubscriptionPurchaseUnavailable
		}
		rights, err = ActivateSubscriptionPurchaseTx(tx, input.UserID, order.ID, now)
		return err
	})
	if err == nil && db == DB {
		refreshSubscriptionUserGroupCache(input.UserID, "versioned balance purchase")
	}
	return rights, err
}

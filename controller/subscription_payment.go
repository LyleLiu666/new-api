package controller

import (
	"errors"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/shopspring/decimal"
)

// Parse an exact display-currency payment into the contract's millionths.
// Unknown, negative, out-of-range or sub-micro amounts stay unknown; they
// cannot silently round into an apparently verified payment.
func subscriptionPaymentMicros(value string) *int64 {
	if len(value) == 0 || len(value) > 64 {
		return nil
	}
	amount, err := decimal.NewFromString(value)
	if err != nil || amount.Exponent() < -6 || amount.Exponent() > 6 || amount.IsNegative() {
		return nil
	}
	micros := amount.Shift(6)
	if !micros.Equal(micros.Truncate(0)) {
		return nil
	}
	converted, err := common.WalletQuotaFromDecimalStrict(micros)
	if err != nil {
		return nil
	}
	return common.GetPointer(int64(converted))
}

// Payment evidence commits before activation. A rights storage failure asks
// the channel to retry; replay uses the same committed fact and order. Domain
// review/cancellation remains durable and never automatically reopens rights.
func completeVersionedSubscriptionPayment(input model.VerifiedSubscriptionPayment, observedAt int64) error {
	fact, err := model.RecordSubscriptionPaymentFact(model.DB, input, observedAt)
	if errors.Is(err, model.ErrSubscriptionPurchaseConflict) {
		return nil
	}
	if err != nil || fact.Outcome != "verified" {
		return err
	}
	_, err = model.ActivateSubscriptionPurchase(model.DB, input.BuyerID, input.OrderID, observedAt)
	if errors.Is(err, model.ErrSubscriptionPurchaseUnavailable) || errors.Is(err, model.ErrCreditOperationRequired) {
		return nil
	}
	return err
}

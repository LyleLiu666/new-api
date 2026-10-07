package model

import (
	"fmt"
	"slices"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// A configured source has an explicit duration and purpose. There is no
// implicit perpetual credit or guessed duration for unconfigured sources.
type CreditSourcePolicy struct {
	SourceType      string `json:"source_type" gorm:"size:32;primaryKey"`
	DurationSeconds int64  `json:"duration_seconds" gorm:"not null"`
	UseMask         int    `json:"use_mask" gorm:"not null"`
	Revision        int64  `json:"revision" gorm:"not null"`
}

func validCreditSourcePolicy(policy CreditSourcePolicy) bool {
	return slices.Contains([]string{"topup", "redemption", "checkin", "signup", "invitee", "inviter", "invite_transfer", "admin", "promotion", "compensation"}, policy.SourceType) && validCreditTime(policy.DurationSeconds) && policy.UseMask > 0 && policy.UseMask <= CreditUseAPI|CreditUseSubscription
}

// PutCreditSourcePolicy uses a revision so editing stale configuration cannot
// silently overwrite another administrator's policy.
func PutCreditSourcePolicy(db *gorm.DB, policy CreditSourcePolicy, expectedRevision int64) (CreditSourcePolicy, error) {
	if !validCreditSourcePolicy(policy) || expectedRevision < 0 || expectedRevision >= common.MaxWalletQuota {
		return CreditSourcePolicy{}, ErrCreditInvalid
	}
	policy.Revision = expectedRevision + 1
	var result *gorm.DB
	if expectedRevision == 0 {
		result = db.Clauses(clause.OnConflict{DoNothing: true}).Create(&policy)
	} else {
		result = db.Model(&CreditSourcePolicy{}).Where("source_type = ? AND revision = ?", policy.SourceType, expectedRevision).Updates(map[string]any{"duration_seconds": policy.DurationSeconds, "use_mask": policy.UseMask, "revision": policy.Revision})
	}
	if result.Error != nil {
		return CreditSourcePolicy{}, result.Error
	}
	if result.RowsAffected != 1 {
		return CreditSourcePolicy{}, ErrCreditOperationConflict
	}
	return policy, nil
}

func creditSourcePolicy(db *gorm.DB, source string) (CreditSourcePolicy, error) {
	var policy CreditSourcePolicy
	if err := db.Where("source_type = ?", source).First(&policy).Error; err != nil {
		return policy, fmt.Errorf("credit source %s requires an explicit validity policy: %w", source, err)
	}
	if !validCreditSourcePolicy(policy) || policy.Revision <= 0 {
		return policy, ErrCreditInvariant
	}
	return policy, nil
}

func issueCreditSourceTx(tx *gorm.DB, userID int, source, sourceID string, amount int64, startsAt int64, policy CreditSourcePolicy) (CreditPack, error) {
	if !validCreditSourcePolicy(policy) || policy.SourceType != source || !validCreditTime(startsAt) || policy.DurationSeconds > common.MaxWalletQuota-startsAt {
		return CreditPack{}, ErrCreditInvalid
	}
	return GrantCreditPackTx(tx, CreditGrant{UserID: userID, SourceType: source, SourceID: sourceID, Amount: amount, StartsAt: startsAt, ExpiresAt: startsAt + policy.DurationSeconds, UseMask: policy.UseMask}, startsAt)
}

func creditTopUpAmount(topUp *TopUp) (int, error) {
	if topUp.CreditQuota > 0 {
		if topUp.CreditQuota > common.MaxWalletQuota {
			return 0, ErrInvalidTopUpQuota
		}
		return int(topUp.CreditQuota), nil
	}
	var units float64
	if topUp.PaymentProvider == PaymentProviderStripe {
		units = topUp.Money
		return common.WalletQuotaFromDecimalStrict(decimal.NewFromFloat(units).Mul(decimal.NewFromFloat(common.QuotaPerUnit)))
	}
	if topUp.PaymentProvider == PaymentProviderCreem {
		return common.WalletQuotaFromDecimalStrict(decimal.NewFromInt(topUp.Amount))
	}
	return common.WalletQuotaFromDecimalStrict(decimal.NewFromInt(topUp.Amount).Mul(decimal.NewFromFloat(common.QuotaPerUnit)))
}

func creditCompletedTopUp(tx *gorm.DB, order *TopUp, amount int, updates map[string]any) error {
	version, err := GetUserAccountingVersion(tx, order.UserId)
	if err != nil {
		return err
	}
	if version == 0 {
		return creditTopUpQuota(tx, order.UserId, amount, updates)
	}
	if version != 1 || order.CreditQuota != int64(amount) {
		return ErrCreditOperationRequired
	}
	policy := CreditSourcePolicy{SourceType: "topup", DurationSeconds: order.CreditDurationSeconds, UseMask: order.CreditUseMask}
	if _, err := issueCreditSourceTx(tx, order.UserId, "topup", fmt.Sprint(order.Id), int64(amount), order.CompleteTime, policy); err != nil {
		return err
	}
	if len(updates) > 0 {
		return tx.Model(&User{}).Where("id = ?", order.UserId).Updates(updates).Error
	}
	return nil
}

func createUserWithCreditRewardsTx(tx *gorm.DB, user *User, inviterID int) error {
	if user.AccountingVersion == 1 {
		user.Quota = 0
	} else if user.AccountingVersion != 0 {
		return ErrCreditOperationRequired
	}
	if err := tx.Create(user).Error; err != nil {
		return err
	}
	grantReward := func(userID int, source string, amount int) error {
		if amount <= 0 {
			return nil
		}
		policy, err := creditSourcePolicy(tx, source)
		if err != nil {
			return err
		}
		_, err = issueCreditSourceTx(tx, userID, source, fmt.Sprint(user.Id), int64(amount), user.CreatedAt, policy)
		return err
	}
	if user.AccountingVersion == 1 {
		if err := grantReward(user.Id, "signup", common.QuotaForNewUser); err != nil {
			return err
		}
	}
	if inviterID == 0 || !operation_setting.IsPaymentComplianceConfirmed() {
		return nil
	}
	if user.AccountingVersion == 1 {
		if err := grantReward(user.Id, "invitee", common.QuotaForInvitee); err != nil {
			return err
		}
	}
	version, err := GetUserAccountingVersion(tx, inviterID)
	if err != nil {
		return err
	}
	if version == 1 && common.QuotaForInviter > 0 {
		if err := grantReward(inviterID, "inviter", common.QuotaForInviter); err != nil {
			return err
		}
		return tx.Model(&User{}).Where("id = ?", inviterID).Updates(map[string]any{"aff_count": gorm.Expr("aff_count + 1"), "aff_history": gorm.Expr("aff_history + ?", common.QuotaForInviter)}).Error
	} else if version != 0 && version != 1 {
		return ErrCreditOperationRequired
	}
	return nil
}

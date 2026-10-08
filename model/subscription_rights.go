package model

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm"
)

// Activation is a host operation following payment verification. It cannot
// accept a client's assertion of payment or replace the purchase contract.
func ActivateSubscriptionPurchase(db *gorm.DB, userID int, orderID, now int64) (UserSubscription, error) {
	var subscription UserSubscription
	if db == nil || userID <= 0 || orderID <= 0 || !validCreditTime(now) {
		return subscription, ErrCreditInvalid
	}
	err := db.Transaction(func(tx *gorm.DB) error {
		var err error
		subscription, err = ActivateSubscriptionPurchaseTx(tx, userID, orderID, now)
		return err
	})
	if err == nil && db == DB {
		refreshSubscriptionUserGroupCache(userID, "versioned subscription activation")
	}
	return subscription, err
}

// The transactional entry point keeps balance debit, verified payment and
// rights activation in the same commit. All writers acquire account first.
func ActivateSubscriptionPurchaseTx(tx *gorm.DB, userID int, orderID, now int64) (UserSubscription, error) {
	var subscription UserSubscription
	if tx == nil || userID <= 0 || orderID <= 0 || !validCreditTime(now) {
		return subscription, ErrCreditInvalid
	}
	if err := lockCreditAccount(tx, userID, true); err != nil {
		return subscription, err
	}
	var user User
	if err := lockForUpdate(tx).First(&user, userID).Error; err != nil {
		return subscription, err
	}
	if user.AccountingVersion != 1 || user.Status != common.UserStatusEnabled {
		return subscription, ErrCreditOperationRequired
	}
	var order SubscriptionPurchaseOrder
	if err := lockForUpdate(tx).Where("id = ? AND user_id = ?", orderID, userID).First(&order).Error; err != nil {
		return subscription, err
	}
	found := tx.Where("purchase_order_id = ? AND user_id = ?", orderID, userID).Limit(1).Find(&subscription)
	if found.Error != nil {
		return subscription, found.Error
	}
	if found.RowsAffected > 0 {
		// Returning cancelled/expired history must never reactivate it.
		return subscription, nil
	}
	if order.PaymentState != "verified" || order.NeedsReview || order.RightsCancelledAt > 0 || !validCreditTime(order.PaidAt) || order.PaidAt > now {
		return subscription, ErrSubscriptionPurchaseUnavailable
	}
	var claim SubscriptionPaymentClaim
	if err := tx.Where("order_id = ? AND provider = ? AND reference_id = ?", orderID, order.Provider, order.PaymentReference).First(&claim).Error; err != nil {
		return subscription, err
	}
	digest := sha256.Sum256([]byte(order.ContractSnapshot))
	if hex.EncodeToString(digest[:]) != order.ContractDigest || order.DurationSeconds != 30*24*3600 {
		return subscription, ErrCreditInvalid
	}
	var plan SubscriptionPlan
	if err := common.UnmarshalJsonStr(order.ContractSnapshot, &plan); err != nil {
		return subscription, err
	}
	if plan.Id != order.PlanID || plan.TotalAmount < 0 || plan.TotalAmount > common.MaxWalletQuota || ValidateSubscriptionTags(plan.EntitlementTags) != nil {
		return subscription, ErrCreditInvalid
	}
	start := order.PaidAt
	var predecessor UserSubscription
	if err := tx.Where("user_id = ? AND plan_id = ? AND plan_version_id > 0 AND status IN ? AND end_time > ?", userID, order.PlanID, []string{"active", "scheduled"}, start).Order("end_time desc, id desc").Limit(1).Find(&predecessor).Error; err != nil {
		return subscription, err
	}
	if predecessor.Id > 0 {
		start = predecessor.EndTime
	}
	if start > common.MaxWalletQuota-order.DurationSeconds {
		return subscription, ErrCreditInvalid
	}
	status := "active"
	if start > now {
		status = "scheduled"
	} else if start+order.DurationSeconds <= now {
		status = "expired"
	}
	upgradeGroup := strings.TrimSpace(plan.UpgradeGroup)
	previousGroup := ""
	if predecessor.Id > 0 && predecessor.UpgradeGroup == upgradeGroup {
		previousGroup = predecessor.PrevUserGroup
	}
	if status == "active" && upgradeGroup != "" && user.Group != upgradeGroup {
		previousGroup = user.Group
		if err := tx.Model(&User{}).Where("id = ?", userID).Update("group", upgradeGroup).Error; err != nil {
			return subscription, err
		}
	}
	allowOverflow := plan.AllowWalletOverflow == nil || *plan.AllowWalletOverflow
	subscription = UserSubscription{UserId: userID, PlanId: order.PlanID, PurchaseOrderID: &order.ID, PlanVersionID: order.VersionID, PurchasePaidAt: order.PaidAt, RenewalAnchorID: predecessor.RenewalAnchorID, ContractSnapshot: order.ContractSnapshot, ContractDigest: order.ContractDigest, EntitlementTags: plan.EntitlementTags, AmountTotal: plan.TotalAmount, StartTime: start, EndTime: start + order.DurationSeconds, Status: status, Source: "versioned_order", UpgradeGroup: upgradeGroup, PrevUserGroup: previousGroup, DowngradeGroup: strings.TrimSpace(plan.DowngradeGroup), AllowWalletOverflow: allowOverflow, CreatedAt: now, UpdatedAt: now}
	if err := tx.Session(&gorm.Session{SkipHooks: true}).Create(&subscription).Error; err != nil {
		return subscription, err
	}
	if subscription.RenewalAnchorID == 0 {
		subscription.RenewalAnchorID = subscription.Id
		return subscription, tx.Model(&subscription).UpdateColumn("renewal_anchor_id", subscription.Id).Error
	}
	return subscription, nil
}

type SubscriptionRightsCancellation struct {
	UserID         int    `json:"user_id"`
	SubscriptionID int    `json:"subscription_id"`
	ActorID        int    `json:"-"`
	EventID        string `json:"event_id"`
	Reason         string `json:"reason"`
}

// Cancellation stops the current and prepaid future terms of one continuous
// subscription. No charge, allocation, payment, or contractual date is erased.
func CancelSubscriptionRights(db *gorm.DB, input SubscriptionRightsCancellation, now int64) error {
	if db == nil || input.UserID <= 0 || input.SubscriptionID <= 0 || input.ActorID <= 0 || !validCreditID(input.EventID, 128) || !validCreditID(input.Reason, 1024) || !validCreditTime(now) {
		return ErrCreditInvalid
	}
	key, err := creditDigest([]string{"cancel_subscription", input.EventID})
	if err != nil {
		return err
	}
	fingerprint, err := creditDigest(struct {
		Input SubscriptionRightsCancellation
		Actor int
	}{input, input.ActorID})
	if err != nil {
		return err
	}
	err = db.Transaction(func(tx *gorm.DB) error {
		if err := lockCreditAccount(tx, input.UserID, true); err != nil {
			return err
		}
		if err := AuthorizeCreditAccountAdmin(tx, input.ActorID, input.UserID); err != nil {
			return err
		}
		existing, err := findCreditOperation(tx, input.UserID, key, fingerprint)
		if err != nil || existing != nil {
			return err
		}
		var subscription UserSubscription
		if err := tx.Where("id = ? AND user_id = ? AND plan_version_id > 0", input.SubscriptionID, input.UserID).First(&subscription).Error; err != nil {
			return err
		}
		if subscription.RenewalAnchorID <= 0 {
			return ErrCreditInvariant
		}
		var remainingTerms int64
		if err := tx.Model(&UserSubscription{}).Where("user_id = ? AND renewal_anchor_id = ? AND status IN ? AND end_time > ?", input.UserID, subscription.RenewalAnchorID, []string{"active", "scheduled"}, now).Count(&remainingTerms).Error; err != nil {
			return err
		}
		if remainingTerms == 0 {
			return ErrSubscriptionPurchaseUnavailable
		}
		if err := tx.Model(&UserSubscription{}).Where("user_id = ? AND renewal_anchor_id = ? AND status IN ?", input.UserID, subscription.RenewalAnchorID, []string{"active", "scheduled"}).Updates(map[string]any{"status": "cancelled", "updated_at": now}).Error; err != nil {
			return err
		}
		// Fence pre-cancellation orders as well: a delayed paid notification must
		// remain auditable without reinstating rights the administrator removed.
		if err := tx.Model(&SubscriptionPurchaseOrder{}).Where("user_id = ? AND plan_id = ? AND created_at <= ? AND id NOT IN (?)", input.UserID, subscription.PlanId, now, tx.Model(&UserSubscription{}).Select("purchase_order_id").Where("user_id = ? AND purchase_order_id IS NOT NULL", input.UserID)).Updates(map[string]any{"needs_review": true, "last_review_reason": "rights_cancelled", "rights_cancelled_at": now}).Error; err != nil {
			return err
		}
		if err := tx.Model(&SubscriptionPurchaseOrder{}).Where("user_id = ? AND id IN (?)", input.UserID, tx.Model(&UserSubscription{}).Select("purchase_order_id").Where("user_id = ? AND renewal_anchor_id = ? AND purchase_order_id IS NOT NULL", input.UserID, subscription.RenewalAnchorID)).Update("rights_cancelled_at", now).Error; err != nil {
			return err
		}
		var effectiveTerm UserSubscription
		if err := tx.Where("user_id = ? AND renewal_anchor_id = ? AND start_time <= ? AND end_time > ?", input.UserID, subscription.RenewalAnchorID, now, now).Order("start_time desc, id desc").Limit(1).Find(&effectiveTerm).Error; err != nil {
			return err
		}
		if effectiveTerm.Id > 0 {
			if _, err := downgradeUserGroupForSubscriptionTx(tx, &effectiveTerm, now); err != nil {
				return err
			}
		}
		result, err := common.Marshal(map[string]any{"subscription_id": subscription.Id, "renewal_anchor_id": subscription.RenewalAnchorID, "actor_id": input.ActorID, "reason": input.Reason, "cancelled_at": now})
		if err != nil {
			return err
		}
		return tx.Create(&CreditOperation{UserID: input.UserID, KeyDigest: key, Fingerprint: fingerprint, Kind: "cancel_rights", CreatedAt: now, Result: string(result)}).Error
	})
	if err == nil && db == DB {
		refreshSubscriptionUserGroupCache(input.UserID, "versioned subscription cancellation")
	}
	return err
}

// A client reads labels only from currently effective purchased terms. The
// original contract never exposes gateway identifiers or a mutable plan copy.
func GetUserSubscriptionRights(db *gorm.DB, userID int, now int64) ([]UserSubscription, error) {
	if db == nil || userID <= 0 || !validCreditTime(now) {
		return nil, ErrCreditInvalid
	}
	if err := RefreshVersionedSubscriptionRights(db, userID, now); err != nil {
		return nil, err
	}
	subscriptions := make([]UserSubscription, 0)
	err := db.Where("user_id = ? AND plan_version_id > 0 AND status IN ? AND start_time <= ? AND end_time > ?", userID, []string{"active", "scheduled"}, now, now).Order("end_time asc, id asc").Find(&subscriptions).Error
	if err != nil {
		return nil, err
	}
	var ends []struct {
		RenewalAnchorID int
		EndTime         int64
	}
	if len(subscriptions) > 0 {
		if err := db.Model(&UserSubscription{}).Select("renewal_anchor_id, MAX(end_time) AS end_time").Where("user_id = ? AND plan_version_id > 0 AND status IN ? AND end_time > ?", userID, []string{"active", "scheduled"}, now).Group("renewal_anchor_id").Scan(&ends).Error; err != nil {
			return nil, err
		}
	}
	byAnchor := make(map[int]int64, len(ends))
	for _, end := range ends {
		byAnchor[end.RenewalAnchorID] = end.EndTime
	}
	for i := range subscriptions {
		subscriptions[i].RenewalEndTime = byAnchor[subscriptions[i].RenewalAnchorID]
	}
	return subscriptions, nil
}

// Advance purchased terms using server time even if maintenance is delayed.
// Dates, contracts and existing request holds remain immutable.
func RefreshVersionedSubscriptionRights(db *gorm.DB, userID int, now int64) error {
	if db == nil || userID <= 0 || !validCreditTime(now) {
		return ErrCreditInvalid
	}
	changed := false
	err := db.Transaction(func(tx *gorm.DB) error {
		if err := lockCreditAccount(tx, userID, true); err != nil {
			return err
		}
		var err error
		changed, err = refreshVersionedSubscriptionRightsTx(tx, userID, now)
		return err
	})
	if err == nil && changed && db == DB {
		refreshSubscriptionUserGroupCache(userID, "versioned subscription term transition")
	}
	return err
}

func refreshVersionedSubscriptionRightsTx(tx *gorm.DB, userID int, now int64) (bool, error) {
	var terms []UserSubscription
	if err := tx.Where("user_id = ? AND plan_version_id > 0 AND status IN ?", userID, []string{"active", "scheduled"}).Order("end_time asc, id asc").Find(&terms).Error; err != nil {
		return false, err
	}
	var expired *UserSubscription
	for i := range terms {
		term := &terms[i]
		if term.EndTime <= now {
			if err := tx.Model(term).Updates(map[string]any{"status": "expired", "updated_at": now}).Error; err != nil {
				return false, err
			}
			term.Status = "expired"
			if term.UpgradeGroup != "" || term.DowngradeGroup != "" {
				expired = term
			}
		}
	}
	changed := false
	if expired != nil {
		group, err := downgradeUserGroupForSubscriptionTx(tx, expired, now)
		if err != nil {
			return false, err
		}
		changed = group != ""
	}
	for _, term := range terms {
		if term.Status != "scheduled" || term.StartTime > now || term.EndTime <= now {
			continue
		}
		updates := map[string]any{"status": "active", "updated_at": now}
		if term.UpgradeGroup != "" {
			current, err := getUserGroupByIdTx(tx, userID)
			if err != nil {
				return false, err
			}
			if current != term.UpgradeGroup {
				if err := tx.Model(&User{}).Where("id = ?", userID).Update("group", term.UpgradeGroup).Error; err != nil {
					return false, err
				}
				updates["prev_user_group"] = current
				changed = true
			}
		}
		if err := tx.Model(&term).Updates(updates).Error; err != nil {
			return false, err
		}
	}
	return changed, nil
}

// Relay authorization observes effective purchased routing groups before
// selecting channels. Reuse the existing authentication cache/version fence;
// a subscription transition cannot bypass a pending restrictive user update.
func GetRelayUserCache(userID int) (*UserBase, error) {
	user, err := GetUserCache(userID)
	if err != nil {
		return nil, err
	}
	version, err := GetUserAccountingVersion(DB, userID)
	if err != nil {
		return nil, err
	}
	if version != 1 {
		return user, nil
	}
	if err := RefreshVersionedSubscriptionRights(DB, userID, common.GetTimestamp()); err != nil {
		return nil, err
	}
	// A prior maintenance transition may have committed while its Redis update
	// failed. Even when no transition is due now, a readable old group is not
	// authoritative. Keep the existing pending-auth-version fence on this read.
	authoritative, err := GetUserById(userID, false)
	if err != nil {
		return nil, err
	}
	if common.RedisEnabled {
		floor, floorErr := getUserAuthVersionFloor(userID)
		if floorErr == nil && floor > authoritative.AuthVersion {
			return nil, ErrUserAuthCachePending
		}
	}
	return authoritative.ToBaseUser(), nil
}

func AdvanceDueVersionedSubscriptionRights(db *gorm.DB, limit int, now int64) (int, error) {
	if db == nil || limit <= 0 || limit > 1000 || !validCreditTime(now) {
		return 0, ErrCreditInvalid
	}
	var userIDs []int
	if err := db.Model(&UserSubscription{}).Distinct("user_id").Where("plan_version_id > 0 AND ((status = ? AND start_time <= ?) OR (status = ? AND end_time <= ?))", "scheduled", now, "active", now).Order("user_id asc").Limit(limit).Pluck("user_id", &userIDs).Error; err != nil {
		return 0, err
	}
	for _, userID := range userIDs {
		if err := RefreshVersionedSubscriptionRights(db, userID, now); err != nil {
			return 0, err
		}
	}
	return len(userIDs), nil
}

package model

import (
	"database/sql/driver"
	"errors"
	"math"

	"github.com/QuantumNous/new-api/common"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var ErrSubscriptionVersionConflict = errors.New("subscription plan version conflict")

// Tags carry product metadata. They never grant external permissions by
// themselves; published contracts retain their own immutable copy.
type SubscriptionTags map[string]string

func (tags SubscriptionTags) Value() (driver.Value, error) {
	if tags == nil {
		return "{}", nil
	}
	encoded, err := common.Marshal(tags)
	return string(encoded), err
}

func (tags *SubscriptionTags) Scan(value any) error {
	*tags = make(SubscriptionTags)
	switch data := value.(type) {
	case nil:
		return nil
	case []byte:
		return common.Unmarshal(data, tags)
	case string:
		return common.UnmarshalJsonStr(data, tags)
	default:
		return ErrCreditInvalid
	}
}

func ValidateSubscriptionTags(tags SubscriptionTags) error {
	if len(tags) > 32 {
		return ErrCreditInvalid
	}
	for key, value := range tags {
		if !validCreditID(key, 128) || len(value) > 2048 {
			return ErrCreditInvalid
		}
	}
	return nil
}

func ValidateSubscriptionCurrency(currency string) error {
	if len(currency) < 3 || len(currency) > 8 {
		return ErrCreditInvalid
	}
	for _, letter := range currency {
		if letter < 'A' || letter > 'Z' {
			return ErrCreditInvalid
		}
	}
	return nil
}

// A published version is an immutable contract record. Publishing alone does
// not activate a product, create an order, or change an existing subscription.
type SubscriptionPlanVersion struct {
	ID              int64  `json:"id" gorm:"primaryKey"`
	PlanID          int    `json:"plan_id" gorm:"not null;uniqueIndex:,composite:subscription_version,priority:1"`
	Revision        int64  `json:"revision" gorm:"not null;uniqueIndex:,composite:subscription_version,priority:2"`
	Snapshot        string `json:"snapshot" gorm:"type:text;not null"`
	SnapshotDigest  string `json:"snapshot_digest" gorm:"size:64;not null"`
	PriceMicros     int64  `json:"price_micros" gorm:"not null"`
	Currency        string `json:"currency" gorm:"size:8;not null"`
	DurationSeconds int64  `json:"duration_seconds" gorm:"not null"`
	ActorID         int    `json:"actor_id" gorm:"not null"`
	EventID         string `json:"event_id" gorm:"size:128;not null"`
	EventDigest     string `json:"-" gorm:"size:64;not null;uniqueIndex"`
	InputDigest     string `json:"-" gorm:"size:64;not null"`
	CreatedAt       int64  `json:"created_at" gorm:"not null"`
}

type SubscriptionPlanDraft struct {
	Plan   SubscriptionPlan `json:"plan"`
	Digest string           `json:"digest"`
}

// Draft reads bypass the legacy plan cache. The digest guards against
// publishing a draft edited after the administrator reviewed it.
func GetSubscriptionPlanDraft(db *gorm.DB, planID int) (SubscriptionPlanDraft, error) {
	var draft SubscriptionPlanDraft
	if db == nil || planID <= 0 {
		return draft, ErrCreditInvalid
	}
	if err := db.First(&draft.Plan, planID).Error; err != nil {
		return draft, err
	}
	draft.Plan.NormalizeDefaults()
	digest, err := creditDigest(draft.Plan)
	draft.Digest = digest
	return draft, err
}

type SubscriptionVersionPublish struct {
	PlanID             int    `json:"plan_id"`
	ExpectedRevision   int64  `json:"expected_revision"`
	ExpectedPlanDigest string `json:"expected_plan_digest"`
	ActorID            int    `json:"-"`
	EventID            string `json:"event_id"`
}

func AuthorizeSubscriptionPlanAdmin(db *gorm.DB, actorID int) error {
	if db == nil || actorID <= 0 {
		return ErrUserQuotaPermission
	}
	var actor User
	if err := db.Select("id", "role", "status").First(&actor, actorID).Error; err != nil {
		return err
	}
	if actor.Role < common.RoleAdminUser || actor.Status != common.UserStatusEnabled {
		return ErrUserQuotaPermission
	}
	return nil
}

func PublishSubscriptionPlanVersion(db *gorm.DB, input SubscriptionVersionPublish, now int64) (SubscriptionPlanVersion, error) {
	var version SubscriptionPlanVersion
	if db == nil || input.PlanID <= 0 || input.ActorID <= 0 || input.ExpectedRevision < 0 || input.ExpectedRevision >= common.MaxWalletQuota || len(input.ExpectedPlanDigest) != 64 || !validCreditID(input.EventID, 128) || !validCreditTime(now) {
		return version, ErrCreditInvalid
	}
	eventDigest, err := creditDigest([]string{"subscription_version", input.EventID})
	if err != nil {
		return version, err
	}
	inputDigest, err := creditDigest(struct {
		Input   SubscriptionVersionPublish
		ActorID int
	}{input, input.ActorID})
	if err != nil {
		return version, err
	}
	err = db.Transaction(func(tx *gorm.DB) error {
		// The write precedes every read, including authorization. SQLite obtains
		// its writer transaction here; MySQL/PostgreSQL lock the plan row. This
		// also serializes publication with existing edits of the same plan.
		if err := tx.Model(&SubscriptionPlan{}).Where("id = ?", input.PlanID).UpdateColumn("id", gorm.Expr("id")).Error; err != nil {
			return err
		}
		if err := AuthorizeSubscriptionPlanAdmin(tx, input.ActorID); err != nil {
			return err
		}
		found := tx.Where("event_digest = ?", eventDigest).Limit(1).Find(&version)
		if found.Error != nil {
			return found.Error
		}
		if found.RowsAffected != 0 {
			if version.InputDigest != inputDigest {
				return ErrSubscriptionVersionConflict
			}
			return nil
		}
		draft, err := GetSubscriptionPlanDraft(tx, input.PlanID)
		if err != nil {
			return err
		}
		var latest int64
		if err := tx.Model(&SubscriptionPlanVersion{}).Where("plan_id = ?", input.PlanID).Select("COALESCE(MAX(revision),0)").Scan(&latest).Error; err != nil {
			return err
		}
		if draft.Digest != input.ExpectedPlanDigest || latest != input.ExpectedRevision {
			return ErrSubscriptionVersionConflict
		}
		plan := draft.Plan
		if err := ValidateSubscriptionTags(plan.EntitlementTags); err != nil {
			return err
		}
		if !validCreditID(plan.Title, 128) || math.IsNaN(plan.PriceAmount) || math.IsInf(plan.PriceAmount, 0) || plan.PriceAmount < 0 || plan.PriceAmount > 9999 || ValidateSubscriptionCurrency(plan.Currency) != nil || plan.DurationUnit != SubscriptionDurationMonth || plan.DurationValue != 1 || plan.TotalAmount < 0 || plan.TotalAmount > common.MaxWalletQuota {
			return ErrCreditInvalid
		}
		micros := decimal.NewFromFloat(plan.PriceAmount).Shift(6)
		if !micros.Equal(micros.Truncate(0)) {
			return ErrCreditInvalid
		}
		snapshot, err := common.Marshal(plan)
		if err != nil {
			return err
		}
		version = SubscriptionPlanVersion{PlanID: input.PlanID, Revision: latest + 1, Snapshot: string(snapshot), SnapshotDigest: draft.Digest, PriceMicros: micros.IntPart(), Currency: plan.Currency, DurationSeconds: 30 * 24 * 3600, ActorID: input.ActorID, EventID: input.EventID, EventDigest: eventDigest, InputDigest: inputDigest, CreatedAt: now}
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&version).Error; err != nil {
			return err
		}
		// Different plans hold different row locks. The global event constraint
		// arbitrates between them; a locking read sees the winner even under
		// MySQL repeatable-read. RowsAffected is not a portable ownership check.
		var committed SubscriptionPlanVersion
		if err := lockForUpdate(tx).Where("event_digest = ?", eventDigest).First(&committed).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrSubscriptionVersionConflict
			}
			return err
		}
		if committed.InputDigest != inputDigest {
			return ErrSubscriptionVersionConflict
		}
		version = committed
		return nil
	})
	return version, err
}

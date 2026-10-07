package model

import (
	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm"
)

// CreditReviewCase records a purchase reversal or refund requiring a human
// decision. Opening it blocks new use, preserving every quantity and hold.
// Cash confirmation is evidence, never an instruction to mint/erase credits.
type CreditReviewCase struct {
	ID             int64  `gorm:"primaryKey"`
	UserID         int    `gorm:"not null;uniqueIndex:,composite:credit_review_event,priority:1"`
	PackID         int64  `gorm:"not null;index"`
	EventID        string `gorm:"size:128;not null"`
	EventDigest    string `gorm:"size:64;not null;uniqueIndex:,composite:credit_review_event,priority:2"`
	Fingerprint    string `gorm:"size:64;not null"`
	ActorID        int    `gorm:"not null"`
	Reason         string `gorm:"size:1024;not null"`
	CreatedAt      int64  `gorm:"not null"`
	CashState      string `gorm:"size:16;not null"`
	CashReference  string `gorm:"size:256;not null"`
	CashEvidence   string `gorm:"size:1024;not null"`
	CashActorID    int    `gorm:"not null"`
	CashRecordedAt int64  `gorm:"not null"`
}

// CreditCashEvidence is append-only; the case stores only its latest state.
// A replay returns without overwriting later evidence or repeating an effect.
type CreditCashEvidence struct {
	ID          int64  `gorm:"primaryKey"`
	UserID      int    `gorm:"not null;index"`
	CaseID      int64  `gorm:"not null;uniqueIndex:,composite:credit_cash_evidence,priority:1"`
	Fingerprint string `gorm:"size:64;not null;uniqueIndex:,composite:credit_cash_evidence,priority:2"`
	ActorID     int    `gorm:"not null"`
	State       string `gorm:"size:16;not null"`
	Reference   string `gorm:"size:256;not null"`
	Evidence    string `gorm:"size:1024;not null"`
	RecordedAt  int64  `gorm:"not null"`
}

type CreditReviewInput struct {
	UserID  int    `json:"user_id"`
	PackID  int64  `json:"pack_id"`
	EventID string `json:"event_id"`
	ActorID int    `json:"-"`
	Reason  string `json:"reason"`
}

type CreditCashOutcome struct {
	UserID    int    `json:"user_id"`
	CaseID    int64  `json:"case_id"`
	ActorID   int    `json:"-"`
	State     string `json:"state"`
	Reference string `json:"reference"`
	Evidence  string `json:"evidence"`
}

func AuthorizeCreditPolicyAdmin(db *gorm.DB, actorID int) error {
	var actor User
	if actorID <= 0 {
		return ErrUserQuotaPermission
	}
	if err := db.Select("role", "status").First(&actor, actorID).Error; err != nil {
		return err
	}
	if actor.Role != common.RoleRootUser || actor.Status != common.UserStatusEnabled {
		return ErrUserQuotaPermission
	}
	return nil
}

func authorizeCreditAdmin(tx *gorm.DB, actorID, userID int) error {
	if actorID <= 0 || userID <= 0 {
		return ErrUserQuotaPermission
	}
	var actor, target User
	if err := tx.Select("id", "role", "status").First(&actor, actorID).Error; err != nil {
		return err
	}
	if err := tx.Select("id", "role").First(&target, userID).Error; err != nil {
		return err
	}
	if actor.Status != common.UserStatusEnabled || actor.Role < common.RoleAdminUser || (actor.Role != common.RoleRootUser && actor.Role <= target.Role) {
		return ErrUserQuotaPermission
	}
	return nil
}

func GrantAdminCredit(db *gorm.DB, grant CreditGrant, now int64) (CreditPack, error) {
	var pack CreditPack
	if !validCreditID(grant.Reason, 1024) {
		return pack, ErrCreditInvalid
	}
	grant.SourceType = "admin"
	err := db.Transaction(func(tx *gorm.DB) error {
		if err := lockCreditAccount(tx, grant.UserID, true); err != nil {
			return err
		}
		if err := authorizeCreditAdmin(tx, grant.ActorID, grant.UserID); err != nil {
			return err
		}
		version, err := GetUserAccountingVersion(tx, grant.UserID)
		if err != nil {
			return err
		}
		if version != 1 {
			return ErrCreditOperationRequired
		}
		pack, err = GrantCreditPackTx(tx, grant, now)
		return err
	})
	return pack, err
}

func OpenCreditReviewCase(db *gorm.DB, input CreditReviewInput, now int64) (CreditReviewCase, error) {
	var review CreditReviewCase
	if input.UserID <= 0 || input.PackID <= 0 || !validCreditID(input.EventID, 128) || !validCreditID(input.Reason, 1024) || !validCreditTime(now) {
		return review, ErrCreditInvalid
	}
	digest, err := creditDigest(input.EventID)
	if err != nil {
		return review, err
	}
	fingerprint, err := creditDigest(input)
	if err != nil {
		return review, err
	}
	err = db.Transaction(func(tx *gorm.DB) error {
		if err := lockCreditAccount(tx, input.UserID, false); err != nil {
			return err
		}
		if err := authorizeCreditAdmin(tx, input.ActorID, input.UserID); err != nil {
			return err
		}
		found := tx.Where("user_id = ? AND event_digest = ?", input.UserID, digest).Find(&review)
		if found.Error != nil {
			return found.Error
		}
		if found.RowsAffected != 0 {
			if review.Fingerprint != fingerprint {
				return ErrCreditOperationConflict
			}
			return nil
		}
		var pack CreditPack
		if err := tx.Where("id = ? AND user_id = ?", input.PackID, input.UserID).First(&pack).Error; err != nil {
			return err
		}
		if !pack.QuantitiesValid() {
			return ErrCreditInvariant
		}
		if pack.BlockedAt == 0 {
			if err := tx.Model(&pack).Update("blocked_at", now).Error; err != nil {
				return err
			}
		}
		review = CreditReviewCase{UserID: input.UserID, PackID: input.PackID, EventID: input.EventID, EventDigest: digest, Fingerprint: fingerprint, ActorID: input.ActorID, Reason: input.Reason, CreatedAt: now, CashState: "unknown"}
		return tx.Create(&review).Error
	})
	return review, err
}

func RecordCreditCashOutcome(db *gorm.DB, input CreditCashOutcome, now int64) error {
	if input.UserID <= 0 || input.CaseID <= 0 || !validCreditTime(now) || (input.State != "unknown" && input.State != "confirmed" && input.State != "rejected") || !validCreditID(input.Evidence, 1024) || len(input.Reference) > 256 || (input.State == "confirmed" && !validCreditID(input.Reference, 256)) {
		return ErrCreditInvalid
	}
	fingerprint, err := creditDigest(input)
	if err != nil {
		return err
	}
	return db.Transaction(func(tx *gorm.DB) error {
		if err := lockCreditAccount(tx, input.UserID, false); err != nil {
			return err
		}
		if err := authorizeCreditAdmin(tx, input.ActorID, input.UserID); err != nil {
			return err
		}
		var review CreditReviewCase
		if err := tx.Where("id = ? AND user_id = ?", input.CaseID, input.UserID).First(&review).Error; err != nil {
			return err
		}
		var existing int64
		if err := tx.Model(&CreditCashEvidence{}).Where("case_id = ? AND fingerprint = ?", review.ID, fingerprint).Count(&existing).Error; err != nil {
			return err
		}
		if existing != 0 {
			return nil
		}
		if review.CashState != "unknown" {
			return ErrCreditOperationConflict
		}
		evidence := CreditCashEvidence{UserID: input.UserID, CaseID: review.ID, Fingerprint: fingerprint, ActorID: input.ActorID, State: input.State, Reference: input.Reference, Evidence: input.Evidence, RecordedAt: now}
		if err := tx.Create(&evidence).Error; err != nil {
			return err
		}
		return tx.Model(&review).Updates(map[string]any{"cash_state": input.State, "cash_reference": input.Reference, "cash_evidence": input.Evidence, "cash_actor_id": input.ActorID, "cash_recorded_at": now}).Error
	})
}

func ListCreditReviewCases(db *gorm.DB, userID, actorID, offset, limit int) ([]CreditReviewCase, int64, error) {
	if userID <= 0 || offset < 0 || limit <= 0 || limit > 1000 {
		return nil, 0, ErrCreditInvalid
	}
	if err := authorizeCreditAdmin(db, actorID, userID); err != nil {
		return nil, 0, err
	}
	query := db.Model(&CreditReviewCase{}).Where("user_id = ?", userID)
	var total int64
	if err := query.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	var reviews []CreditReviewCase
	err := query.Order("id DESC").Offset(offset).Limit(limit).Find(&reviews).Error
	return reviews, total, err
}

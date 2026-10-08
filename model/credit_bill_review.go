package model

import (
	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/types"
	"gorm.io/gorm"
)

// Approval resolves unknown metering; completed bills use financial revisions.
// The original receipt stays linked and unchanged after manual confirmation.
type CreditBillReviewInput struct {
	UserID             int                    `json:"user_id"`
	RequestID          int64                  `json:"request_id"`
	ActorID            int                    `json:"-"`
	EventID            string                 `json:"event_id"`
	ExpectedEvidenceID int64                  `json:"expected_evidence_id"`
	ExternalReference  string                 `json:"external_reference"`
	Reason             string                 `json:"reason"`
	EvidenceVersion    string                 `json:"evidence_version"`
	Facts              []types.UsageFact      `json:"facts"`
	Consume            *CreditConsumeSnapshot `json:"consume"`
}

type CreditBillReviewApproval struct {
	OperationID        int64 `json:"operation_id"`
	UserID             int   `json:"user_id"`
	RequestID          int64 `json:"request_id"`
	OriginalEvidenceID int64 `json:"original_evidence_id"`
	EvidenceID         int64 `json:"evidence_id"`
	ReferenceQuota     int64 `json:"reference_quota"`
	CreatedAt          int64 `json:"created_at"`
}

type creditBillReviewAudit struct {
	Input    CreditBillReviewInput    `json:"input"`
	ActorID  int                      `json:"actor_id"`
	Approval CreditBillReviewApproval `json:"approval"`
}

type CreditBillReviewDetails struct {
	Approval          CreditBillReviewApproval `json:"approval"`
	ActorID           int                      `json:"actor_id"`
	Reason            string                   `json:"reason"`
	ExternalReference string                   `json:"external_reference"`
}

func GetCreditBillReviewDetails(db *gorm.DB, userID int, requestID int64, actorID int) (*CreditBillReviewDetails, error) {
	if db == nil || userID <= 0 || requestID <= 0 {
		return nil, ErrCreditInvalid
	}
	if err := AuthorizeCreditAccountAdmin(db, actorID, userID); err != nil {
		return nil, err
	}
	var request CreditRequest
	if err := db.Where("id = ? AND user_id = ?", requestID, userID).First(&request).Error; err != nil {
		return nil, err
	}
	if request.ReviewEvidenceID == 0 {
		return nil, nil
	}
	evidence, input, err := creditConsumeEvidence(db, request)
	if err != nil {
		return nil, err
	}
	audit, err := creditBillReviewAuditTx(db, request, evidence, input)
	if err != nil {
		return nil, err
	}
	return &CreditBillReviewDetails{Approval: audit.Approval, ActorID: audit.ActorID, Reason: audit.Input.Reason, ExternalReference: audit.Input.ExternalReference}, nil
}

// Evidence, approval and reactivation commit together. Normal recovery obtains
// a new execution epoch and settles the original holds, including after a crash.
func ApproveCreditBillReview(db *gorm.DB, input CreditBillReviewInput, now int64) (CreditBillReviewApproval, error) {
	var approval CreditBillReviewApproval
	if db == nil || input.UserID <= 0 || input.RequestID <= 0 || input.ExpectedEvidenceID < 0 || input.Consume == nil || input.Consume.ReferenceQuota < 0 || input.Consume.ReferenceQuota > common.MaxQuota || input.Consume.ReferenceQuota == 0 && !input.Consume.ZeroChargeEstablished || len(input.Facts) == 0 || !validCreditID(input.EventID, 128) || !validCreditID(input.ExternalReference, 256) || !validCreditID(input.Reason, 1024) || !validCreditID(input.EvidenceVersion, 64) || !validCreditTime(now) {
		return approval, ErrCreditInvalid
	}
	known := false
	for _, fact := range input.Facts {
		if fact.Quantity != nil && (fact.Source == "upstream" || fact.Source == "adaptor") && !fact.Partial {
			known = true
		}
	}
	if !known {
		return approval, ErrCreditInvalid
	}
	key, err := creditDigest([]string{"bill_review", input.EventID})
	if err != nil {
		return approval, err
	}
	fingerprint, err := creditDigest(struct {
		Input CreditBillReviewInput
		Actor int
	}{input, input.ActorID})
	if err != nil {
		return approval, err
	}
	err = db.Transaction(func(tx *gorm.DB) error {
		if err := lockCreditAccount(tx, input.UserID, false); err != nil {
			return err
		}
		if err := AuthorizeCreditAccountAdmin(tx, input.ActorID, input.UserID); err != nil {
			return err
		}
		existing, err := findCreditOperation(tx, input.UserID, key, fingerprint)
		if err != nil {
			return err
		}
		if existing != nil {
			var audit creditBillReviewAudit
			if err := common.UnmarshalJsonStr(existing.Result, &audit); err != nil {
				return err
			}
			approval = audit.Approval
			var request CreditRequest
			if err := tx.Where("id = ? AND user_id = ?", input.RequestID, input.UserID).First(&request).Error; err != nil {
				return err
			}
			if approval.OperationID != existing.ID || approval.EvidenceID != request.ReviewEvidenceID || approval.RequestID != request.ID || approval.UserID != request.UserID {
				return ErrCreditInvariant
			}
			_, _, err := creditConsumeEvidence(tx, request)
			return err
		}
		var request CreditRequest
		if err := tx.Where("id = ? AND user_id = ?", input.RequestID, input.UserID).First(&request).Error; err != nil {
			return err
		}
		if request.State != "review" || request.IntentKind != "" || request.SubmittedAt == 0 || request.ReviewEvidenceID != 0 || request.UsageEvidenceID != input.ExpectedEvidenceID {
			return ErrCreditOperationConflict
		}
		if request.LeaseUntil > now {
			return ErrCreditLeaseLost
		}
		if request.LeaseEpoch >= common.MaxWalletQuota {
			return ErrCreditInvariant
		}
		if request.UsageEvidenceID > 0 {
			if _, _, err := creditConsumeEvidence(tx, request); err != nil {
				return err
			}
		}
		operation := CreditOperation{UserID: input.UserID, KeyDigest: key, Fingerprint: fingerprint, Kind: "bill_review", CreatedAt: now}
		if err := tx.Create(&operation).Error; err != nil {
			return err
		}
		evidence, err := RecordCreditUsageEvidence(tx, CreditEvidenceInput{ReviewOperationID: operation.ID, ActorID: input.ActorID, UserID: input.UserID, RequestID: input.RequestID, EventID: "bill-review-" + key, Attempt: 1, Sequence: 1, Stage: "review", Cumulative: true, Version: input.EvidenceVersion, Facts: input.Facts, Consume: input.Consume}, now)
		if err != nil {
			return err
		}
		request.ReviewEvidenceID = evidence.ID
		if request.TaskID != "" {
			if err := validateCreditTaskCompletion(tx, request); err != nil {
				return err
			}
		}
		approval = CreditBillReviewApproval{OperationID: operation.ID, UserID: input.UserID, RequestID: input.RequestID, OriginalEvidenceID: input.ExpectedEvidenceID, EvidenceID: evidence.ID, ReferenceQuota: input.Consume.ReferenceQuota, CreatedAt: now}
		audit, err := common.Marshal(creditBillReviewAudit{Input: input, ActorID: input.ActorID, Approval: approval})
		if err != nil {
			return err
		}
		if len(audit) > 65535 {
			return ErrCreditInvalid
		}
		if err := tx.Model(&operation).Update("result", string(audit)).Error; err != nil {
			return err
		}
		return tx.Model(&request).Updates(map[string]any{"review_evidence_id": evidence.ID, "state": "executing", "lease_epoch": request.LeaseEpoch + 1, "lease_owner": "bill-review-" + key, "lease_until": 0, "recovery_blocked_at": 0, "next_recovery_at": 0}).Error
	})
	return approval, err
}

// Historical authority is checked against the immutable approval, not today's
// administrator role. Later demotion cannot invalidate a completed accounting.
func creditBillReviewAuditTx(db *gorm.DB, request CreditRequest, evidence CreditUsageEvidence, input CreditEvidenceInput) (creditBillReviewAudit, error) {
	var audit creditBillReviewAudit
	var operation CreditOperation
	if input.ReviewOperationID <= 0 || input.ActorID <= 0 || input.Stage != "review" || request.ReviewEvidenceID != evidence.ID {
		return audit, ErrCreditInvariant
	}
	if err := db.Where("id = ? AND user_id = ? AND kind = ?", input.ReviewOperationID, request.UserID, "bill_review").First(&operation).Error; err != nil {
		return audit, err
	}
	if common.UnmarshalJsonStr(operation.Result, &audit) != nil {
		return audit, ErrCreditInvariant
	}
	approval := audit.Approval
	if audit.ActorID != input.ActorID || approval.OperationID != operation.ID || approval.UserID != request.UserID || approval.RequestID != request.ID || approval.OriginalEvidenceID != request.UsageEvidenceID || approval.EvidenceID != evidence.ID || approval.ReferenceQuota != input.Consume.ReferenceQuota || approval.CreatedAt != operation.CreatedAt || approval.CreatedAt != evidence.ObservedAt || audit.Input.ExpectedEvidenceID != request.UsageEvidenceID {
		return audit, ErrCreditInvariant
	}
	if request.IntentKind != "" && (request.IntentKind != "settle" || request.Actual != approval.ReferenceQuota) {
		return audit, ErrCreditInvariant
	}
	key, err := creditDigest([]string{"bill_review", audit.Input.EventID})
	if err != nil || key != operation.KeyDigest {
		return audit, ErrCreditInvariant
	}
	fingerprint, err := creditDigest(struct {
		Input CreditBillReviewInput
		Actor int
	}{audit.Input, audit.ActorID})
	if err != nil || fingerprint != operation.Fingerprint {
		return audit, ErrCreditInvariant
	}
	expected := CreditEvidenceInput{ReviewOperationID: operation.ID, ActorID: audit.ActorID, UserID: audit.Input.UserID, RequestID: audit.Input.RequestID, EventID: "bill-review-" + key, Attempt: 1, Sequence: 1, Stage: "review", Cumulative: true, Version: audit.Input.EvidenceVersion, Facts: audit.Input.Facts, Consume: audit.Input.Consume}
	fingerprint, err = creditDigest(expected)
	if err != nil || fingerprint != evidence.Fingerprint || evidence.Attempt != expected.Attempt || evidence.Sequence != expected.Sequence {
		return audit, ErrCreditInvariant
	}
	eventDigest, err := creditDigest([]any{expected.UserID, expected.RequestID, expected.Attempt, expected.EventID})
	if err != nil || eventDigest != evidence.EventDigest {
		return audit, ErrCreditInvariant
	}
	if request.UsageEvidenceID > 0 {
		original := request
		original.ReviewEvidenceID = 0
		if _, _, err := creditConsumeEvidence(db, original); err != nil {
			return audit, err
		}
	}
	return audit, nil
}

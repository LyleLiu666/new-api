package model

import (
	"errors"
	"math"
	"slices"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/types"
	"gorm.io/gorm"
)

// Events remain immutable even after later, more reliable usage arrives.
type CreditUsageEvidence struct {
	ID          int64  `json:"id" gorm:"primaryKey"`
	UserID      int    `json:"user_id" gorm:"not null;index"`
	RequestID   int64  `json:"request_id" gorm:"not null;index;uniqueIndex:,composite:credit_usage_sequence,priority:1"`
	Attempt     int    `json:"attempt" gorm:"not null;uniqueIndex:,composite:credit_usage_sequence,priority:2"`
	Stage       string `json:"stage" gorm:"size:16;not null;uniqueIndex:,composite:credit_usage_sequence,priority:3"`
	Sequence    int64  `json:"sequence" gorm:"not null;uniqueIndex:,composite:credit_usage_sequence,priority:4"`
	EventDigest string `json:"-" gorm:"size:64;not null;uniqueIndex"`
	Fingerprint string `json:"-" gorm:"size:64;not null"`
	PriceDigest string `json:"price_digest" gorm:"size:64;not null"`
	Payload     string `json:"-" gorm:"type:text;not null"`
	ObservedAt  int64  `json:"observed_at" gorm:"not null"`
}

type CreditConsumeSnapshot struct {
	ReferenceQuota        int64  `json:"reference_quota"`
	ZeroChargeEstablished bool   `json:"zero_charge_established"`
	PromptTokens          int    `json:"prompt_tokens"`
	CompletionTokens      int    `json:"completion_tokens"`
	UseTimeSeconds        int    `json:"use_time_seconds"`
	IsStream              bool   `json:"is_stream"`
	Other                 string `json:"other"`
}

// CreditAttemptPrice is a credential-free snapshot of the effective native
// price at submission. The admission contract remains on CreditRequest.
type CreditAttemptPrice struct {
	AccountID         string `json:"account_id,omitempty"`
	CredentialVersion int64  `json:"credential_version,omitempty"`
	ChannelID         int    `json:"channel_id"`
	Group             string `json:"group"`
	BillingModel      string `json:"billing_model"`
	UpstreamModel     string `json:"upstream_model"`
	Protocol          string `json:"protocol"`
	Snapshot          string `json:"snapshot"`
	SnapshotDigest    string `json:"snapshot_digest"`
}

// A transport observation records only what the gateway can prove. Accepted
// writes do not prove client receipt, and HTTP status does not prove billable use.
type CreditRelayObservation struct {
	Phase      string `json:"phase"`
	Bytes      *int64 `json:"bytes,omitempty"`
	StatusCode *int   `json:"status_code,omitempty"`
}

type CreditEvidenceInput struct {
	Observation            *CreditRelayObservation `json:"observation,omitempty"`
	AttemptPrice           *CreditAttemptPrice     `json:"attempt_price,omitempty"`
	AttemptPriceEvidenceID int64                   `json:"attempt_price_evidence_id,omitempty"`
	ReviewOperationID      int64                   `json:"review_operation_id,omitempty"`
	ActorID                int                     `json:"actor_id,omitempty"`
	UserID                 int                     `json:"user_id"`
	RequestID              int64                   `json:"request_id"`
	EventID                string                  `json:"event_id"`
	Attempt                int                     `json:"attempt"`
	Sequence               int64                   `json:"sequence"`
	Cumulative             bool                    `json:"cumulative"`
	Stage                  string                  `json:"stage"`
	Version                string                  `json:"version"`
	Facts                  []types.UsageFact       `json:"facts"`
	Consume                *CreditConsumeSnapshot  `json:"consume,omitempty"`
}

// The existing account and execution fences serialize producer writes with
// recovery. Storing the final projection precedes the financial intent.
func RecordCreditUsageEvidence(db *gorm.DB, input CreditEvidenceInput, now int64, execution ...CreditExecution) (CreditUsageEvidence, error) {
	var evidence CreditUsageEvidence
	if db == nil || input.UserID <= 0 || input.RequestID <= 0 || !validCreditID(input.EventID, 128) || input.Attempt <= 0 || input.Attempt > 1000 || input.Sequence < 0 || input.Sequence > common.MaxWalletQuota || !validCreditID(input.Version, 64) || !validCreditTime(now) || len(input.Facts) > 64 {
		return evidence, ErrCreditInvalid
	}
	if input.Stage != "upstream" && input.Stage != "settlement" && input.Stage != "estimate" && input.Stage != "adjustment" && input.Stage != "review" && input.Stage != "attempt" && input.Stage != "relay" {
		return evidence, ErrCreditInvalid
	}
	if (input.Stage == "relay") != (input.Observation != nil) {
		return evidence, ErrCreditInvalid
	}
	if observation := input.Observation; observation != nil {
		if input.Sequence == 0 || input.Sequence > 4 || len(input.Facts) != 0 || input.Consume != nil {
			return evidence, ErrCreditInvalid
		}
		switch observation.Phase {
		case "upstream_response":
			if observation.Bytes != nil || observation.StatusCode != nil && (*observation.StatusCode < 100 || *observation.StatusCode > 599) {
				return evidence, ErrCreditInvalid
			}
		case "upstream_error":
			if observation.Bytes != nil || observation.StatusCode != nil {
				return evidence, ErrCreditInvalid
			}
		case "client_write_possible", "client_write_accepted":
			if observation.StatusCode != nil || observation.Bytes == nil || *observation.Bytes <= 0 || *observation.Bytes > common.MaxQuota {
				return evidence, ErrCreditInvalid
			}
		default:
			return evidence, ErrCreditInvalid
		}
	}
	if (input.Stage == "attempt") != (input.AttemptPrice != nil) || input.AttemptPriceEvidenceID < 0 || input.Stage == "attempt" && (input.AttemptPriceEvidenceID != 0 || input.Consume != nil || len(input.Facts) != 0 || input.Sequence != 0) {
		return evidence, ErrCreditInvalid
	}
	if price := input.AttemptPrice; price != nil {
		if len(price.AccountID) > 36 || price.CredentialVersion < 0 || (price.AccountID == "") != (price.CredentialVersion == 0) || price.ChannelID < 0 || len(price.Group) > 64 || !validCreditID(price.BillingModel, 256) || len(price.UpstreamModel) > 256 || !validCreditID(price.Protocol, 32) || len(price.Snapshot) > 65535 {
			return evidence, ErrCreditInvalid
		}
		var snapshot map[string]any
		if common.UnmarshalJsonStr(price.Snapshot, &snapshot) != nil || snapshot == nil {
			return evidence, ErrCreditInvalid
		}
		digest, err := creditDigest(price.Snapshot)
		if err != nil || digest != price.SnapshotDigest {
			return evidence, ErrCreditInvalid
		}
	}
	if (input.Stage == "review") != (input.ReviewOperationID > 0) || input.ReviewOperationID < 0 {
		return evidence, ErrCreditInvalid
	}
	fields := make(map[string]bool, len(input.Facts))
	for _, fact := range input.Facts {
		if !validCreditID(fact.Field, 128) || fields[fact.Field] {
			return evidence, ErrCreditInvalid
		}
		fields[fact.Field] = true
		if fact.Unit != "token" && fact.Unit != "count" && fact.Unit != "second" && fact.Unit != "credit" && fact.Unit != "request" {
			return evidence, ErrCreditInvalid
		}
		if fact.Source != "upstream" && fact.Source != "estimate" && fact.Source != "adaptor" && fact.Source != "unknown" {
			return evidence, ErrCreditInvalid
		}
		if (fact.Source == "unknown") != (fact.Quantity == nil) || fact.Source == "estimate" && !validCreditID(fact.Algorithm, 128) {
			return evidence, ErrCreditInvalid
		}
		if estimation := fact.Estimation; estimation != nil {
			if fact.Source != "estimate" || !validCreditID(estimation.Version, 64) || !validCreditID(estimation.Model, 256) || !validCreditID(estimation.Method, 64) || len(estimation.Tokenizer) > 128 || len(estimation.DependencyVersion) > 128 || len(estimation.Parameters) > 32 || len(estimation.Settings) > 8 || math.IsNaN(estimation.Quantity) || math.IsInf(estimation.Quantity, 0) || estimation.Quantity < 0 || estimation.Quantity > float64(common.MaxQuota) || fact.Quantity == nil || estimation.Quantity > *fact.Quantity {
				return evidence, ErrCreditInvalid
			}
			for key, value := range estimation.Parameters {
				if !validCreditID(key, 64) || math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value > float64(common.MaxQuota) {
					return evidence, ErrCreditInvalid
				}
			}
			for key := range estimation.Settings {
				if !validCreditID(key, 64) {
					return evidence, ErrCreditInvalid
				}
			}
			if len(estimation.Components) > 64 || estimation.OmittedComponents < 0 || estimation.OmittedComponents > common.MaxQuota {
				return evidence, ErrCreditInvalid
			}
			previousIndex := -1
			for _, component := range estimation.Components {
				if component.Index <= previousIndex || component.Index > common.MaxQuota || !validCreditID(component.Method, 64) || math.IsNaN(component.Quantity) || math.IsInf(component.Quantity, 0) || component.Quantity < 0 || component.Quantity > float64(common.MaxQuota) || len(component.Parameters) > 16 || len(component.Settings) > 4 {
					return evidence, ErrCreditInvalid
				}
				switch component.Kind {
				case "image", "audio", "video", "file", "unknown":
				default:
					return evidence, ErrCreditInvalid
				}
				for key, value := range component.Parameters {
					if !validCreditID(key, 64) || math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value > float64(common.MaxQuota) {
						return evidence, ErrCreditInvalid
					}
				}
				for key := range component.Settings {
					if !validCreditID(key, 64) {
						return evidence, ErrCreditInvalid
					}
				}
				previousIndex = component.Index
			}
		}
		// Plugin numeric contracts preserve fractions even for vendor credits
		// and tokens. Native token/count receipts retain their integer guard.
		pluginQuantity := fact.Unit != "request" && strings.HasPrefix(fact.Field, "task.") && ((fact.Source == "adaptor" && fact.Algorithm == "task-plugin-usage-v1") || (fact.Source == "estimate" && fact.Algorithm == "task-plugin-estimate-v1"))
		if fact.Quantity != nil && (math.IsNaN(*fact.Quantity) || math.IsInf(*fact.Quantity, 0) || *fact.Quantity < 0 || *fact.Quantity > float64(common.MaxQuota) || fact.Unit != "second" && !pluginQuantity && math.Trunc(*fact.Quantity) != *fact.Quantity) {
			return evidence, ErrCreditInvalid
		}
	}
	if input.Consume != nil {
		log := input.Consume
		if input.Stage != "settlement" && input.Stage != "review" || log.ReferenceQuota < 0 || log.ReferenceQuota > common.MaxQuota || log.PromptTokens < 0 || log.PromptTokens > common.MaxQuota || log.CompletionTokens < 0 || log.CompletionTokens > common.MaxQuota || log.UseTimeSeconds < 0 || len(log.Other) > 65536 {
			return evidence, ErrCreditInvalid
		}
		var other map[string]any
		if err := common.UnmarshalJsonStr(log.Other, &other); err != nil || other == nil {
			return evidence, ErrCreditInvalid
		}
	}
	err := db.Transaction(func(tx *gorm.DB) error {
		if err := lockCreditAccount(tx, input.UserID, false); err != nil {
			return err
		}
		var request CreditRequest
		if err := tx.Where("id = ? AND user_id = ?", input.RequestID, input.UserID).First(&request).Error; err != nil {
			return err
		}
		if input.Stage == "review" {
			if request.State != "review" || request.IntentKind != "" || request.ReviewEvidenceID != 0 || request.LeaseUntil > now || input.Consume == nil {
				return ErrCreditOperationConflict
			}
			if err := AuthorizeCreditAccountAdmin(tx, input.ActorID, request.UserID); err != nil {
				return err
			}
			var operation CreditOperation
			if err := tx.Where("id = ? AND user_id = ? AND kind = ? AND result = ?", input.ReviewOperationID, input.UserID, "bill_review", "").First(&operation).Error; err != nil {
				return err
			}
		} else if input.Stage == "adjustment" {
			if request.State != "settled" || input.Consume != nil {
				return ErrCreditOperationConflict
			}
			if err := AuthorizeCreditAccountAdmin(tx, input.ActorID, request.UserID); err != nil {
				return err
			}
		} else {
			if input.ActorID != 0 {
				return ErrCreditInvalid
			}
			if err := validateCreditExecution(request, now, execution); err != nil {
				return err
			}
			if request.ReviewEvidenceID != 0 {
				return ErrCreditOperationConflict
			}
		}
		if input.Stage == "settlement" || input.Stage == "relay" {
			price, _, err := GetCreditAttemptPriceEvidence(tx, input.UserID, input.RequestID, input.Attempt)
			if err == nil {
				if input.AttemptPriceEvidenceID != 0 && input.AttemptPriceEvidenceID != price.ID {
					return ErrCreditOperationConflict
				}
				input.AttemptPriceEvidenceID = price.ID
			} else if errors.Is(err, gorm.ErrRecordNotFound) {
				var count int64
				if err := tx.Model(&CreditUsageEvidence{}).Where("request_id = ? AND user_id = ? AND stage = ?", input.RequestID, input.UserID, "attempt").Count(&count).Error; err != nil {
					return err
				}
				if count != 0 || input.AttemptPriceEvidenceID != 0 {
					return ErrCreditInvariant
				}
			} else {
				return err
			}
		} else if input.AttemptPriceEvidenceID != 0 {
			return ErrCreditInvalid
		}
		fingerprint, err := creditDigest(input)
		if err != nil {
			return err
		}
		payload, err := common.Marshal(input)
		// MySQL TEXT is byte-bounded; all engines use that same contract.
		if err != nil || len(payload) > 65535 {
			return ErrCreditInvalid
		}
		eventDigest, err := creditDigest([]any{input.UserID, input.RequestID, input.Attempt, input.EventID})
		if err != nil {
			return err
		}
		found := tx.Where("event_digest = ?", eventDigest).Limit(1).Find(&evidence)
		if found.Error != nil {
			return found.Error
		}
		if found.RowsAffected > 0 {
			if evidence.Fingerprint != fingerprint {
				return ErrCreditOperationConflict
			}
			return nil
		}
		if input.Stage != "adjustment" && (request.IntentKind != "" || request.State == "settled" || request.State == "released") {
			return ErrCreditOperationConflict
		}
		if input.Consume != nil && input.Stage != "review" && request.TaskID != "" {
			if err := validateCreditTaskCompletion(tx, request); err != nil {
				return err
			}
		}
		var collision CreditUsageEvidence
		found = tx.Where("request_id = ? AND attempt = ? AND stage = ? AND sequence = ?", input.RequestID, input.Attempt, input.Stage, input.Sequence).Limit(1).Find(&collision)
		if found.Error != nil {
			return found.Error
		}
		if found.RowsAffected > 0 {
			return ErrCreditOperationConflict
		}
		priceDigest, err := creditDigest(request.PriceSnapshot)
		if err != nil {
			return err
		}
		evidence = CreditUsageEvidence{UserID: input.UserID, RequestID: input.RequestID, Attempt: input.Attempt, Stage: input.Stage, Sequence: input.Sequence, EventDigest: eventDigest, Fingerprint: fingerprint, PriceDigest: priceDigest, Payload: string(payload), ObservedAt: now}
		if err := tx.Create(&evidence).Error; err != nil {
			return err
		}
		if input.Consume != nil && input.Stage != "review" {
			if request.UsageEvidenceID != 0 {
				return ErrCreditOperationConflict
			}
			return tx.Model(&request).Update("usage_evidence_id", evidence.ID).Error
		}
		return nil
	})
	return evidence, err
}

// Cumulative snapshots supersede earlier increments for each individual field.
// Sorting by provider sequence, rather than arrival time, makes late receipts
// harmless. Attempts stay separate; retries do not duplicate the user's fee.
func GetCreditUsageProjection(db *gorm.DB, userID int, requestID int64, attempt int) ([]types.UsageFact, error) {
	if db == nil || userID <= 0 || requestID <= 0 || attempt <= 0 || attempt > 1000 {
		return nil, ErrCreditInvalid
	}
	var request CreditRequest
	if err := db.Where("id = ? AND user_id = ?", requestID, userID).First(&request).Error; err != nil {
		return nil, err
	}
	var evidence []CreditUsageEvidence
	if err := db.Where("request_id = ? AND user_id = ? AND attempt = ? AND stage IN ?", requestID, userID, attempt, []string{"upstream", "estimate"}).Order("sequence asc, id asc").Find(&evidence).Error; err != nil {
		return nil, err
	}
	current := make(map[string]types.UsageFact)
	priceDigest, err := creditDigest(request.PriceSnapshot)
	if err != nil {
		return nil, err
	}
	for _, row := range evidence {
		var input CreditEvidenceInput
		if err := common.UnmarshalJsonStr(row.Payload, &input); err != nil {
			return nil, err
		}
		fingerprint, err := creditDigest(input)
		if err != nil || fingerprint != row.Fingerprint || row.PriceDigest != priceDigest || input.RequestID != requestID || input.UserID != userID || input.Attempt != attempt || input.Sequence != row.Sequence || input.Stage != row.Stage {
			return nil, ErrCreditInvariant
		}
		for _, fact := range input.Facts {
			previous, exists := current[fact.Field]
			if exists && previous.Unit != fact.Unit {
				return nil, ErrCreditInvariant
			}
			if fact.Quantity == nil {
				if !exists {
					current[fact.Field] = fact
				}
				continue
			}
			if exists && fact.Source == "estimate" && input.Cumulative {
				if previous.Source == "upstream" && !previous.Partial {
					continue
				}
				if previous.Partial && previous.Quantity != nil && *previous.Quantity > *fact.Quantity {
					quantity := *previous.Quantity
					fact.Quantity = &quantity
				}
			}
			if !input.Cumulative && exists && previous.Quantity != nil {
				quantity := *previous.Quantity + *fact.Quantity
				if math.IsNaN(quantity) || math.IsInf(quantity, 0) || quantity > float64(common.MaxQuota) {
					return nil, ErrCreditInvalid
				}
				fact.Quantity = &quantity
				// A sum of observations cannot claim that one constituent
				// counter produced the whole. Original events keep both.
				fact.Estimation = nil
				if previous.Source == "estimate" {
					fact.Source, fact.Algorithm = previous.Source, previous.Algorithm
				}
				if previous.Source == "adaptor" && fact.Source == "upstream" {
					fact.Source = "adaptor"
				}
			}
			current[fact.Field] = fact
		}
	}
	facts := make([]types.UsageFact, 0, len(current))
	for _, fact := range current {
		facts = append(facts, fact)
	}
	slices.SortFunc(facts, func(a, b types.UsageFact) int { return strings.Compare(a.Field, b.Field) })
	return facts, nil
}

func creditConsumeEvidence(db *gorm.DB, request CreditRequest) (CreditUsageEvidence, CreditEvidenceInput, error) {
	var evidence CreditUsageEvidence
	var input CreditEvidenceInput
	id, stage := request.UsageEvidenceID, "settlement"
	if request.ReviewEvidenceID > 0 {
		id, stage = request.ReviewEvidenceID, "review"
	}
	if err := db.Where("id = ? AND user_id = ? AND request_id = ?", id, request.UserID, request.ID).First(&evidence).Error; err != nil {
		return evidence, input, err
	}
	priceDigest, err := creditDigest(request.PriceSnapshot)
	if err != nil {
		return evidence, input, err
	}
	if evidence.PriceDigest != priceDigest || common.UnmarshalJsonStr(evidence.Payload, &input) != nil || evidence.Stage != stage || input.Stage != stage || input.Consume == nil || input.RequestID != request.ID || input.UserID != request.UserID {
		return evidence, input, ErrCreditInvariant
	}
	fingerprint, err := creditDigest(input)
	if err != nil || fingerprint != evidence.Fingerprint || input.Consume.ReferenceQuota < 0 || input.Consume.ReferenceQuota > common.MaxQuota {
		return evidence, input, ErrCreditInvariant
	}
	if input.AttemptPriceEvidenceID != 0 {
		price, _, err := GetCreditAttemptPriceEvidence(db, request.UserID, request.ID, input.Attempt)
		if err != nil || price.ID != input.AttemptPriceEvidenceID {
			return evidence, input, ErrCreditInvariant
		}
	} else if stage == "settlement" {
		var count int64
		if err := db.Model(&CreditUsageEvidence{}).Where("request_id = ? AND user_id = ? AND stage = ?", request.ID, request.UserID, "attempt").Count(&count).Error; err != nil {
			return evidence, input, err
		}
		if count != 0 {
			return evidence, input, ErrCreditInvariant
		}
	}
	if stage == "review" {
		if _, err := creditBillReviewAuditTx(db, request, evidence, input); err != nil {
			return evidence, input, err
		}
	}
	return evidence, input, nil
}

// Submission and its price evidence are one transaction, before external I/O.
func RecordCreditAttemptSubmission(db *gorm.DB, input CreditEvidenceInput, now int64, execution ...CreditExecution) (CreditUsageEvidence, error) {
	var evidence CreditUsageEvidence
	if db == nil || input.Stage != "attempt" || input.AttemptPrice == nil {
		return evidence, ErrCreditInvalid
	}
	price := *input.AttemptPrice
	digest, err := creditDigest(price.Snapshot)
	if err != nil {
		return evidence, err
	}
	price.SnapshotDigest = digest
	input.AttemptPrice = &price
	err = db.Transaction(func(tx *gorm.DB) error {
		if err := MarkCreditRequestSubmitted(tx, input.UserID, input.RequestID, now, execution...); err != nil {
			return err
		}
		var err error
		evidence, err = RecordCreditUsageEvidence(tx, input, now, execution...)
		return err
	})
	return evidence, err
}

// Attempt zero selects the latest submission for asynchronously completed jobs.
// Historical requests without attempt evidence retain their original contract.
func GetCreditAttemptPriceEvidence(db *gorm.DB, userID int, requestID int64, attempt int) (CreditUsageEvidence, CreditEvidenceInput, error) {
	var evidence CreditUsageEvidence
	var input CreditEvidenceInput
	if db == nil || userID <= 0 || requestID <= 0 || attempt < 0 || attempt > 1000 {
		return evidence, input, ErrCreditInvalid
	}
	query := db.Where("request_id = ? AND user_id = ? AND stage = ?", requestID, userID, "attempt")
	if attempt != 0 {
		query = query.Where("attempt = ?", attempt)
	}
	if err := query.Order("attempt desc").First(&evidence).Error; err != nil {
		return evidence, input, err
	}
	var request CreditRequest
	if err := db.Where("id = ? AND user_id = ?", requestID, userID).First(&request).Error; err != nil {
		return evidence, input, err
	}
	admissionDigest, err := creditDigest(request.PriceSnapshot)
	if err != nil || evidence.PriceDigest != admissionDigest || common.UnmarshalJsonStr(evidence.Payload, &input) != nil || input.AttemptPrice == nil || input.Stage != "attempt" || input.UserID != userID || input.RequestID != requestID || input.Attempt != evidence.Attempt || input.Sequence != 0 || evidence.Sequence != 0 || input.Consume != nil || len(input.Facts) != 0 || input.AttemptPriceEvidenceID != 0 {
		return evidence, input, ErrCreditInvariant
	}
	fingerprint, err := creditDigest(input)
	if err != nil || fingerprint != evidence.Fingerprint {
		return evidence, input, ErrCreditInvariant
	}
	effectiveDigest, err := creditDigest(input.AttemptPrice.Snapshot)
	if err != nil || effectiveDigest != input.AttemptPrice.SnapshotDigest {
		return evidence, input, ErrCreditInvariant
	}
	return evidence, input, nil
}

// Gateway retries attribute the single retail charge to the successful
// submission. Failed-attempt supplier cost is a separate, currently unknown fact.
func creditBillRoutingTx(db *gorm.DB, request CreditRequest) (int, string, error) {
	if request.UsageEvidenceID == 0 && request.ReviewEvidenceID == 0 {
		return request.ChannelID, request.Group, nil
	}
	_, input, err := creditConsumeEvidence(db, request)
	if err != nil {
		return 0, "", err
	}
	if input.AttemptPriceEvidenceID == 0 && request.ReviewEvidenceID == 0 {
		return request.ChannelID, request.Group, nil
	}
	attempt := input.Attempt
	if input.AttemptPriceEvidenceID == 0 {
		attempt = 0
	}
	_, price, err := GetCreditAttemptPriceEvidence(db, request.UserID, request.ID, attempt)
	if errors.Is(err, gorm.ErrRecordNotFound) && request.ReviewEvidenceID != 0 {
		return request.ChannelID, request.Group, nil
	}
	if err != nil {
		return 0, "", err
	}
	return price.AttemptPrice.ChannelID, price.AttemptPrice.Group, nil
}

package model

import (
	"fmt"
	"math"
	"slices"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/types"
	"gorm.io/gorm"
)

// Financial fields are append-only. Delivery fields acknowledge the separate
// log projection; they never change the original request or its usage receipt.
type CreditBillAdjustment struct {
	ID              int64  `json:"id" gorm:"primaryKey"`
	UserID          int    `json:"user_id" gorm:"not null;index"`
	RequestID       int64  `json:"request_id" gorm:"not null;uniqueIndex:,composite:credit_bill_revision,priority:1"`
	Revision        int64  `json:"revision" gorm:"not null;uniqueIndex:,composite:credit_bill_revision,priority:2"`
	OperationID     int64  `json:"operation_id" gorm:"not null;uniqueIndex"`
	UsageEvidenceID int64  `json:"usage_evidence_id" gorm:"not null"`
	ActorID         int    `json:"actor_id" gorm:"not null"`
	Reason          string `json:"reason" gorm:"size:1024;not null"`
	ReferenceQuota  int64  `json:"reference_quota" gorm:"not null"`
	Charged         int64  `json:"charged" gorm:"not null"`
	Uncollected     int64  `json:"uncollected" gorm:"not null"`
	Refunded        int64  `json:"refunded" gorm:"not null"`
	Movements       string `json:"-" gorm:"type:text;not null"`
	CreatedAt       int64  `json:"created_at" gorm:"not null"`
	LogEventID      string `json:"-" gorm:"size:64;not null;uniqueIndex"`
	LogPayload      string `json:"-" gorm:"type:text;not null"`
	LogState        string `json:"-" gorm:"size:16;not null;index"`
	LogAttempts     int64  `json:"-" gorm:"not null;default:0"`
	LogLastError    string `json:"-" gorm:"size:1024;not null;default:''"`
	LogNextRetryAt  int64  `json:"-" gorm:"not null;default:0;index"`
	LogDeliveredAt  int64  `json:"-" gorm:"not null;default:0"`
}

type CreditBillAdjustmentInput struct {
	UserID           int               `json:"user_id"`
	RequestID        int64             `json:"request_id"`
	ActorID          int               `json:"-"`
	EventID          string            `json:"event_id"`
	ExpectedRevision int64             `json:"expected_revision"`
	ReferenceQuota   int64             `json:"reference_quota"`
	EvidenceVersion  string            `json:"evidence_version"`
	Facts            []types.UsageFact `json:"facts"`
	Reason           string            `json:"reason"`
}

type CreditBillBalance struct {
	Revision       int64 `json:"revision"`
	ReferenceQuota int64 `json:"reference_quota"`
	Charged        int64 `json:"charged"`
	Uncollected    int64 `json:"uncollected"`
}

// The original bill remains immutable; this view folds its financial revisions.
func GetCreditBillBalance(db *gorm.DB, userID int, requestID int64) (CreditBillBalance, error) {
	if db == nil || userID <= 0 || requestID <= 0 {
		return CreditBillBalance{}, ErrCreditInvalid
	}
	var request CreditRequest
	if err := db.Where("id = ? AND user_id = ?", requestID, userID).First(&request).Error; err != nil {
		return CreditBillBalance{}, err
	}
	if request.UsageEvidenceID > 0 || request.ReviewEvidenceID > 0 {
		if _, _, err := creditConsumeEvidence(db, request); err != nil {
			return CreditBillBalance{}, err
		}
	}
	var adjustments []CreditBillAdjustment
	if err := db.Where("request_id = ? AND user_id = ?", requestID, userID).Order("revision asc").Find(&adjustments).Error; err != nil {
		return CreditBillBalance{}, err
	}
	if err := validateCreditBillAdjustmentsTx(db, request, adjustments); err != nil {
		return CreditBillBalance{}, err
	}
	return creditBillBalance(request, adjustments)
}

func creditBillBalance(request CreditRequest, adjustments []CreditBillAdjustment) (CreditBillBalance, error) {
	balance := CreditBillBalance{ReferenceQuota: request.Actual, Charged: request.Charged, Uncollected: request.Uncollected}
	if request.State != "settled" {
		// A saved intent is readable before its separate money transaction.
		// Neither collection nor a platform shortfall exists at this stage.
		if len(adjustments) != 0 || balance.ReferenceQuota < 0 || balance.ReferenceQuota > common.MaxQuota || balance.Charged != 0 || balance.Uncollected != 0 {
			return balance, ErrCreditInvariant
		}
		return balance, nil
	}
	if balance.ReferenceQuota < 0 || balance.ReferenceQuota > common.MaxQuota || balance.Charged < 0 || balance.Charged > balance.ReferenceQuota || balance.Uncollected != balance.ReferenceQuota-balance.Charged {
		return balance, ErrCreditInvariant
	}
	for _, adjustment := range adjustments {
		if request.State != "settled" || adjustment.UserID != request.UserID || adjustment.RequestID != request.ID || adjustment.Revision != balance.Revision+1 || adjustment.ReferenceQuota < 0 || adjustment.ReferenceQuota > common.MaxQuota || adjustment.Charged != min(balance.Charged, adjustment.ReferenceQuota) || adjustment.Uncollected != adjustment.ReferenceQuota-adjustment.Charged || adjustment.Refunded != balance.Charged-adjustment.Charged {
			return balance, ErrCreditInvariant
		}
		balance = CreditBillBalance{Revision: adjustment.Revision, ReferenceQuota: adjustment.ReferenceQuota, Charged: adjustment.Charged, Uncollected: adjustment.Uncollected}
	}
	return balance, nil
}

func AdjustCreditBill(db *gorm.DB, input CreditBillAdjustmentInput, now int64) (CreditBillAdjustment, error) {
	var adjustment CreditBillAdjustment
	if db == nil || input.UserID <= 0 || input.RequestID <= 0 || input.ExpectedRevision < 0 || input.ExpectedRevision >= common.MaxWalletQuota || input.ReferenceQuota < 0 || input.ReferenceQuota > common.MaxQuota || !validCreditID(input.EventID, 128) || !validCreditID(input.Reason, 1024) || !validCreditID(input.EvidenceVersion, 64) || len(input.Facts) == 0 || !validCreditTime(now) {
		return adjustment, ErrCreditInvalid
	}
	key, err := creditDigest([]string{"bill_adjust", input.EventID})
	if err != nil {
		return adjustment, err
	}
	fingerprint, err := creditDigest(struct {
		Input CreditBillAdjustmentInput
		Actor int
	}{input, input.ActorID})
	if err != nil {
		return adjustment, err
	}
	var request CreditRequest
	err = db.Transaction(func(tx *gorm.DB) error {
		if err := lockCreditAccount(tx, input.UserID, false); err != nil {
			return err
		}
		if err := AuthorizeCreditAccountAdmin(tx, input.ActorID, input.UserID); err != nil {
			return err
		}
		if err := tx.Where("id = ? AND user_id = ?", input.RequestID, input.UserID).First(&request).Error; err != nil {
			return err
		}
		if request.State != "settled" || request.IntentKind != "settle" {
			return ErrCreditOperationConflict
		}
		existing, err := findCreditOperation(tx, input.UserID, key, fingerprint)
		if err != nil {
			return err
		}
		if existing != nil {
			return tx.Where("operation_id = ? AND request_id = ? AND user_id = ?", existing.ID, request.ID, input.UserID).First(&adjustment).Error
		}
		var previous []CreditBillAdjustment
		if err := tx.Where("request_id = ? AND user_id = ?", request.ID, input.UserID).Order("revision asc").Find(&previous).Error; err != nil {
			return err
		}
		if input.ExpectedRevision != int64(len(previous)) {
			return ErrCreditOperationConflict
		}
		if err := validateCreditBillAdjustmentsTx(tx, request, previous); err != nil {
			return err
		}
		balance, err := creditBillBalance(request, previous)
		if err != nil {
			return err
		}
		newCharge := min(balance.Charged, input.ReferenceQuota)
		refund, referenceDelta := balance.Charged-newCharge, input.ReferenceQuota-balance.ReferenceQuota
		operation := CreditOperation{UserID: input.UserID, KeyDigest: key, Fingerprint: fingerprint, Kind: "bill_adjust", CreatedAt: now, Result: ""}
		if err := tx.Create(&operation).Error; err != nil {
			return err
		}
		evidence, err := RecordCreditUsageEvidence(tx, CreditEvidenceInput{UserID: input.UserID, RequestID: request.ID, ActorID: input.ActorID, EventID: "bill-adjust-" + key, Attempt: 1, Sequence: input.ExpectedRevision + 1, Stage: "adjustment", Cumulative: true, Version: input.EvidenceVersion, Facts: input.Facts}, now)
		if err != nil {
			return err
		}
		var movements []creditBillMovement
		if request.FundingSource == SubscriptionWindowFundingSource {
			movements, err = adjustCreditBillWindowsTx(tx, request, refund, referenceDelta)
		} else {
			movements, err = refundCreditBillPacksTx(tx, request, previous, operation.ID, refund, now)
		}
		if err != nil {
			return err
		}
		var user User
		if err := tx.Select("id", "username", "used_quota").First(&user, input.UserID).Error; err != nil {
			return err
		}
		if user.UsedQuota < 0 || int64(user.UsedQuota) < refund {
			return ErrCreditInvariant
		}
		if refund > 0 {
			if err := tx.Model(&user).Update("used_quota", gorm.Expr("used_quota - ?", refund)).Error; err != nil {
				return err
			}
			if request.TokenID > 0 {
				var token Token
				found := lockForUpdate(tx.Unscoped()).Where("id = ? AND user_id = ?", request.TokenID, input.UserID).Limit(1).Find(&token)
				if found.Error != nil {
					return found.Error
				}
				// A removed Key does not prevent returning the account's own money.
				if found.RowsAffected > 0 {
					if err := adjustCreditToken(tx, input.UserID, request.TokenID, -refund, false, now); err != nil {
						return err
					}
				}
			}
		}
		channelID, group, err := creditBillRoutingTx(tx, request)
		if err != nil {
			return err
		}
		if channelID > 0 {
			var channel Channel
			found := lockForUpdate(tx).Select("id", "used_quota").Where("id = ?", channelID).Limit(1).Find(&channel)
			if found.Error != nil {
				return found.Error
			}
			if found.RowsAffected > 0 {
				if channel.UsedQuota < 0 || referenceDelta < -int64(channel.UsedQuota) || referenceDelta > math.MaxInt64-int64(channel.UsedQuota) {
					return ErrCreditInvariant
				}
				if err := tx.Model(&channel).Update("used_quota", gorm.Expr("used_quota + ?", referenceDelta)).Error; err != nil {
					return err
				}
			}
		}
		if request.TaskRowID > 0 {
			var result *gorm.DB
			switch request.TaskKind {
			case "task":
				result = tx.Model(&Task{}).Where("id = ? AND user_id = ?", request.TaskRowID, input.UserID).Update("quota", newCharge)
			case "midjourney":
				result = tx.Model(&Midjourney{}).Where("id = ? AND user_id = ?", request.TaskRowID, input.UserID).Update("quota", newCharge)
			default:
				return ErrCreditInvariant
			}
			if result.Error != nil {
				return result.Error
			}
		}
		encoded, err := common.Marshal(movements)
		if err != nil {
			return err
		}
		adjustment = CreditBillAdjustment{UserID: input.UserID, RequestID: request.ID, Revision: input.ExpectedRevision + 1, OperationID: operation.ID, UsageEvidenceID: evidence.ID, ActorID: input.ActorID, Reason: input.Reason, ReferenceQuota: input.ReferenceQuota, Charged: newCharge, Uncollected: input.ReferenceQuota - newCharge, Refunded: refund, Movements: string(encoded), CreatedAt: now, LogEventID: common.NewRequestId(), LogState: "pending"}
		other := NewLogOther()
		other.MergePublic(map[string]any{"credit_request_id": request.ID, "billing_revision": adjustment.Revision, "billing_source": request.FundingSource, "reference_quota": adjustment.ReferenceQuota, "charged_quota": adjustment.Charged, "uncollected_quota": adjustment.Uncollected, "refunded_quota": refund, "original_reference_quota": request.Actual, "original_charged_quota": request.Charged})
		other.SetAdmin("bill_adjustment", map[string]any{"actor_id": input.ActorID, "reason": input.Reason, "usage_evidence_id": evidence.ID, "price_digest": evidence.PriceDigest})
		logType := LogTypeManage
		if refund > 0 {
			logType = LogTypeRefund
		}
		payload, err := common.Marshal(Log{UserId: input.UserID, Username: user.Username, CreatedAt: now, Type: logType, ModelName: request.ModelName, Quota: int(refund), TokenId: request.TokenID, ChannelId: channelID, Group: group, RequestId: adjustment.LogEventID, Other: other.JSONString()})
		if err != nil {
			return err
		}
		adjustment.LogPayload = string(payload)
		if err := tx.Create(&adjustment).Error; err != nil {
			return err
		}
		result, err := common.Marshal(adjustment)
		if err != nil {
			return err
		}
		return tx.Model(&operation).Update("result", string(result)).Error
	})
	if err == nil && adjustment.Refunded > 0 {
		invalidateCreditTokenCache(db, request.TokenID)
	}
	return adjustment, err
}

type creditBillMovement struct {
	Kind           string `json:"kind"`
	AllocationID   int64  `json:"allocation_id"`
	PackID         int64  `json:"pack_id,omitempty"`
	WindowID       int64  `json:"window_id,omitempty"`
	Refunded       int64  `json:"refunded"`
	ReferenceDelta int64  `json:"reference_delta,omitempty"`
}

// Undo later allocations first, retaining the earliest FEFO consumption.
// Earlier corrections are deductions from the original immutable allocation.
func refundCreditBillPacksTx(tx *gorm.DB, request CreditRequest, previous []CreditBillAdjustment, operationID, refund, now int64) ([]creditBillMovement, error) {
	returned := make(map[int64]int64)
	for _, prior := range previous {
		var movements []creditBillMovement
		if err := common.UnmarshalJsonStr(prior.Movements, &movements); err != nil {
			return nil, err
		}
		for _, movement := range movements {
			if movement.Kind != "pack" || movement.Refunded < 0 || movement.Refunded > common.MaxQuota-returned[movement.AllocationID] {
				return nil, ErrCreditInvariant
			}
			returned[movement.AllocationID] += movement.Refunded
		}
	}
	reservations, err := creditRequestReservationsTx(tx, request)
	if err != nil {
		return nil, err
	}
	ids := make([]int64, 0, len(reservations)+1)
	for _, reservation := range reservations {
		ids = append(ids, reservation.ReservationID)
	}
	key, err := creditDigest([]string{"reserve", fmt.Sprintf("extra:%d", request.ID)})
	if err != nil {
		return nil, err
	}
	var extra CreditOperation
	if err := tx.Where("user_id = ? AND key_digest = ? AND kind = ?", request.UserID, key, "reserve").Limit(1).Find(&extra).Error; err != nil {
		return nil, err
	}
	if extra.ID > 0 {
		ids = append(ids, extra.ID)
	}
	var allocations []CreditAllocation
	if err := tx.Where("operation_id IN ?", ids).Order("id asc").Find(&allocations).Error; err != nil {
		return nil, err
	}
	var originalPaid, alreadyReturned, expectedReturned int64
	for _, prior := range previous {
		if prior.Refunded < 0 || prior.Refunded > request.Charged-expectedReturned {
			return nil, ErrCreditInvariant
		}
		expectedReturned += prior.Refunded
	}
	known := make(map[int64]bool, len(allocations))
	for _, allocation := range allocations {
		if allocation.Settled < 0 || allocation.Settled > allocation.Amount || allocation.Settled > common.MaxQuota-originalPaid || returned[allocation.ID] > allocation.Settled {
			return nil, ErrCreditInvariant
		}
		originalPaid += allocation.Settled
		alreadyReturned += returned[allocation.ID]
		known[allocation.ID] = true
	}
	for id := range returned {
		if !known[id] {
			return nil, ErrCreditInvariant
		}
	}
	if originalPaid != request.Charged || alreadyReturned != expectedReturned {
		return nil, ErrCreditInvariant
	}
	remaining := refund
	entries := make(map[int64]CreditLedgerEntry)
	movements := make([]creditBillMovement, 0)
	for i := len(allocations) - 1; i >= 0 && remaining > 0; i-- {
		allocation := allocations[i]
		if allocation.Settled < returned[allocation.ID] || allocation.Settled > allocation.Amount {
			return nil, ErrCreditInvariant
		}
		amount := min(remaining, allocation.Settled-returned[allocation.ID])
		if amount == 0 {
			continue
		}
		var pack CreditPack
		if err := tx.Where("id = ? AND user_id = ?", allocation.PackID, request.UserID).First(&pack).Error; err != nil {
			return nil, err
		}
		if !pack.QuantitiesValid() || pack.Spent < amount {
			return nil, ErrCreditInvariant
		}
		available, expired, revoked := amount, int64(0), int64(0)
		if pack.ExpiresAt <= now {
			available, expired = 0, amount
		} else if pack.BlockedAt > 0 {
			available, revoked = 0, amount
		}
		if err := tx.Model(&pack).Updates(map[string]any{"spent": pack.Spent - amount, "available": pack.Available + available, "expired": pack.Expired + expired, "revoked": pack.Revoked + revoked}).Error; err != nil {
			return nil, err
		}
		entry := entries[pack.ID]
		entry.UserID, entry.OperationID, entry.PackID, entry.CreatedAt = request.UserID, operationID, pack.ID, now
		entry.Spent -= amount
		entry.Available, entry.Expired, entry.Revoked = entry.Available+available, entry.Expired+expired, entry.Revoked+revoked
		entries[pack.ID] = entry
		movements = append(movements, creditBillMovement{Kind: "pack", AllocationID: allocation.ID, PackID: pack.ID, Refunded: amount})
		remaining -= amount
	}
	if remaining != 0 {
		return nil, ErrCreditInvariant
	}
	packIDs := make([]int64, 0, len(entries))
	for id := range entries {
		packIDs = append(packIDs, id)
	}
	slices.Sort(packIDs)
	for _, id := range packIDs {
		entry := entries[id]
		if err := tx.Create(&entry).Error; err != nil {
			return nil, err
		}
	}
	return movements, nil
}

// Each original window receives the same metering correction. Payment and
// subscription totals change once; no current generation is selected here.
func adjustCreditBillWindowsTx(tx *gorm.DB, request CreditRequest, refund, referenceDelta int64) ([]creditBillMovement, error) {
	allocations, err := subscriptionRequestAllocationsTx(tx, request)
	if err != nil {
		return nil, err
	}
	movements := make([]creditBillMovement, 0, len(allocations))
	for _, allocation := range allocations {
		var window SubscriptionWindow
		if err := tx.Where("id = ? AND user_id = ? AND subscription_id = ?", allocation.WindowID, request.UserID, request.SubscriptionID).First(&window).Error; err != nil {
			return nil, err
		}
		if !window.quantitiesValid() || window.Used < refund || referenceDelta < -window.ReferenceUsed || referenceDelta > common.MaxWalletQuota-window.ReferenceUsed {
			return nil, ErrCreditInvariant
		}
		if err := tx.Model(&window).Updates(map[string]any{"used": window.Used - refund, "reference_used": window.ReferenceUsed + referenceDelta}).Error; err != nil {
			return nil, err
		}
		movements = append(movements, creditBillMovement{Kind: "window", AllocationID: allocation.ID, WindowID: window.ID, Refunded: refund, ReferenceDelta: referenceDelta})
	}
	var term UserSubscription
	if err := tx.Where("id = ? AND user_id = ?", request.SubscriptionID, request.UserID).First(&term).Error; err != nil {
		return nil, err
	}
	if term.AmountUsed < refund {
		return nil, ErrCreditInvariant
	}
	if err := tx.Model(&term).Update("amount_used", term.AmountUsed-refund).Error; err != nil {
		return nil, err
	}
	return movements, nil
}

// Financial corrections must retain their originating operation, receipt and
// allocation movements. Balanced pack totals alone cannot prove those links.
func validateCreditBillAdjustmentsTx(tx *gorm.DB, request CreditRequest, rows []CreditBillAdjustment) error {
	if len(rows) == 0 {
		return nil
	}
	if _, err := creditBillBalance(request, rows); err != nil {
		return err
	}
	priceDigest, err := creditDigest(request.PriceSnapshot)
	if err != nil {
		return err
	}
	returned := make(map[int64]int64)
	priorReference := request.Actual
	for _, row := range rows {
		var operation CreditOperation
		if err := tx.Where("id = ? AND user_id = ?", row.OperationID, request.UserID).First(&operation).Error; err != nil {
			return err
		}
		encoded, err := common.Marshal(row)
		if err != nil {
			return err
		}
		if operation.Kind != "bill_adjust" || operation.Result != string(encoded) {
			return ErrCreditInvariant
		}
		var evidence CreditUsageEvidence
		if err := tx.Where("id = ? AND user_id = ? AND request_id = ?", row.UsageEvidenceID, request.UserID, request.ID).First(&evidence).Error; err != nil {
			return err
		}
		var input CreditEvidenceInput
		if err := common.UnmarshalJsonStr(evidence.Payload, &input); err != nil {
			return ErrCreditInvariant
		}
		fingerprint, err := creditDigest(input)
		if err != nil {
			return err
		}
		if evidence.Fingerprint != fingerprint || evidence.PriceDigest != priceDigest || evidence.Stage != "adjustment" || evidence.Attempt != 1 || evidence.Sequence != row.Revision || input.RequestID != request.ID || input.UserID != request.UserID || input.ActorID != row.ActorID || input.Stage != evidence.Stage || input.Sequence != row.Revision || input.Attempt != 1 || !input.Cumulative || input.Consume != nil || input.EventID != "bill-adjust-"+operation.KeyDigest {
			return ErrCreditInvariant
		}
		var movements []creditBillMovement
		if err := common.UnmarshalJsonStr(row.Movements, &movements); err != nil {
			return ErrCreditInvariant
		}
		seen := make(map[int64]bool)
		if request.FundingSource == SubscriptionWindowFundingSource {
			allocations, err := subscriptionRequestAllocationsTx(tx, request)
			if err != nil {
				return err
			}
			if len(movements) != len(allocations) {
				return ErrCreditInvariant
			}
			for _, movement := range movements {
				if movement.Kind != "window" || movement.PackID != 0 || seen[movement.AllocationID] || movement.Refunded != row.Refunded || movement.ReferenceDelta != row.ReferenceQuota-priorReference || !slices.ContainsFunc(allocations, func(allocation SubscriptionWindowAllocation) bool {
					return allocation.ID == movement.AllocationID && allocation.WindowID == movement.WindowID
				}) {
					return ErrCreditInvariant
				}
				seen[movement.AllocationID] = true
			}
		} else {
			var ledger []CreditLedgerEntry
			if err := tx.Where("operation_id = ? AND user_id = ?", row.OperationID, request.UserID).Find(&ledger).Error; err != nil {
				return err
			}
			byPack := make(map[int64]int64)
			total := int64(0)
			for _, movement := range movements {
				if movement.Kind != "pack" || movement.WindowID != 0 || movement.ReferenceDelta != 0 || movement.Refunded <= 0 || movement.Refunded > row.Refunded-total || seen[movement.AllocationID] {
					return ErrCreditInvariant
				}
				var allocation CreditAllocation
				if err := tx.Where("id = ? AND pack_id = ?", movement.AllocationID, movement.PackID).First(&allocation).Error; err != nil {
					return err
				}
				var packs int64
				if err := tx.Model(&CreditPack{}).Where("id = ? AND user_id = ?", movement.PackID, request.UserID).Count(&packs).Error; err != nil {
					return err
				}
				if packs != 1 {
					return ErrCreditInvariant
				}
				var links int64
				if err := tx.Model(&CreditRequestReservation{}).Where("request_id = ? AND user_id = ? AND reservation_id = ?", request.ID, request.UserID, allocation.OperationID).Count(&links).Error; err != nil {
					return err
				}
				if links == 0 {
					key, err := creditDigest([]string{"reserve", fmt.Sprintf("extra:%d", request.ID)})
					if err != nil {
						return err
					}
					if err := tx.Model(&CreditOperation{}).Where("id = ? AND user_id = ? AND key_digest = ? AND kind = ?", allocation.OperationID, request.UserID, key, "reserve").Count(&links).Error; err != nil {
						return err
					}
				}
				if links != 1 || returned[allocation.ID] > allocation.Settled || movement.Refunded > allocation.Settled-returned[allocation.ID] {
					return ErrCreditInvariant
				}
				returned[allocation.ID] += movement.Refunded
				byPack[movement.PackID] += movement.Refunded
				total += movement.Refunded
				seen[movement.AllocationID] = true
			}
			if total != row.Refunded || len(ledger) != len(byPack) {
				return ErrCreditInvariant
			}
			for _, entry := range ledger {
				refund, known := byPack[entry.PackID]
				if !known || entry.Issued != 0 || entry.Held != 0 || entry.Spent != -refund || entry.Available < 0 || entry.Expired < 0 || entry.Revoked < 0 || entry.Available > refund || entry.Expired > refund || entry.Revoked > refund || entry.Available+entry.Expired+entry.Revoked != refund {
					return ErrCreditInvariant
				}
				delete(byPack, entry.PackID)
			}
		}
		priorReference = row.ReferenceQuota
	}
	return nil
}

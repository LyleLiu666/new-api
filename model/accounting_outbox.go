package model

import (
	"math"

	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Primary-database outbox: independent log outages cannot lose the bill.
type CreditLogOutbox struct {
	ID          int64  `gorm:"primaryKey"`
	UserID      int    `gorm:"not null;index"`
	RequestID   int64  `gorm:"not null;uniqueIndex"`
	EventID     string `gorm:"size:64;not null;uniqueIndex"`
	Payload     string `gorm:"type:text;not null"`
	State       string `gorm:"size:16;not null;index"`
	Attempts    int64  `gorm:"not null;default:0"`
	LastError   string `gorm:"size:1024;not null;default:''"`
	NextRetryAt int64  `gorm:"not null;default:0;index"`
	CreatedAt   int64  `gorm:"not null"`
	DeliveredAt int64  `gorm:"not null;default:0"`
}

// Receipt and log commit together in the independent SQL log database.
// Event IDs are globally unique, so several primary databases can share a sink.
type CreditLogDelivery struct {
	EventDigest string `gorm:"size:64;primaryKey"`
	Fingerprint string `gorm:"size:64;not null"`
	LogID       int    `gorm:"not null"`
	CreatedAt   int64  `gorm:"not null"`
}

func createCreditConsumeProjectionTx(tx *gorm.DB, request CreditRequest, charged, uncollected, now int64) error {
	channelID, group, err := creditBillRoutingTx(tx, request)
	if err != nil {
		return err
	}
	var user User
	if err := tx.Select("id", "username", "used_quota", "request_count").First(&user, request.UserID).Error; err != nil {
		return err
	}
	if user.UsedQuota < 0 || int64(user.UsedQuota) > math.MaxInt64-charged || user.RequestCount < 0 || user.RequestCount == math.MaxInt {
		return ErrCreditInvariant
	}
	if err := tx.Model(&user).Updates(map[string]any{"used_quota": gorm.Expr("used_quota + ?", charged), "request_count": gorm.Expr("request_count + 1")}).Error; err != nil {
		return err
	}
	if channelID > 0 {
		var channel Channel
		found := lockForUpdate(tx).Select("id", "used_quota").Where("id = ?", channelID).Limit(1).Find(&channel)
		if found.Error != nil {
			return found.Error
		}
		// A removed upstream still has a durable bill. Its row is not recreated.
		if found.RowsAffected != 0 {
			if channel.UsedQuota < 0 || int64(channel.UsedQuota) > math.MaxInt64-request.Actual {
				return ErrCreditInvariant
			}
			if err := tx.Model(&channel).Update("used_quota", gorm.Expr("used_quota + ?", request.Actual)).Error; err != nil {
				return err
			}
		}
	}
	eventID := common.NewRequestId()
	other := NewLogOther()
	source := request.FundingSource
	if source == "" {
		source = CreditFundingSource
	}
	other.MergePublic(map[string]any{"billing_source": source, "subscription_id": request.SubscriptionID, "credit_request_id": request.ID, "request_id": request.RequestID, "charged_quota": charged, "reference_quota": request.Actual, "uncollected_quota": uncollected})
	log := Log{UserId: request.UserID, Username: user.Username, CreatedAt: now, Type: LogTypeConsume, ModelName: request.ModelName, Quota: int(charged), TokenId: request.TokenID, ChannelId: channelID, Group: group, RequestId: eventID, Other: other.JSONString()}
	if request.UsageEvidenceID != 0 || request.ReviewEvidenceID != 0 {
		evidence, input, err := creditConsumeEvidence(tx, request)
		if err != nil {
			return err
		}
		var metadata map[string]any
		if err := common.UnmarshalJsonStr(input.Consume.Other, &metadata); err != nil || metadata == nil {
			return ErrCreditInvariant
		}
		if request.ReviewEvidenceID > 0 {
			audit, err := creditBillReviewAuditTx(tx, request, evidence, input)
			if err != nil {
				return err
			}
			other.SetAdmin("bill_review", map[string]any{"actor_id": audit.ActorID, "reason": audit.Input.Reason, "external_reference": audit.Input.ExternalReference, "original_evidence_id": audit.Approval.OriginalEvidenceID, "operation_id": audit.Approval.OperationID})
		}
		// Preserve existing role scopes. Authoritative accounting metadata is
		// applied last so response metadata cannot replace the actual bill.
		for key, value := range other.Snapshot() {
			metadata[key] = value
		}
		if input.AttemptPriceEvidenceID > 0 {
			attempt, price, err := GetCreditAttemptPriceEvidence(tx, request.UserID, request.ID, input.Attempt)
			if err != nil {
				return err
			}
			metadata["effective_price"] = map[string]any{"evidence_id": attempt.ID, "attempt": attempt.Attempt, "snapshot_digest": price.AttemptPrice.SnapshotDigest}
		}
		metadata["usage_evidence"] = map[string]any{"id": evidence.ID, "version": input.Version, "price_digest": evidence.PriceDigest, "facts": input.Facts}
		encoded, err := common.Marshal(metadata)
		if err != nil {
			return err
		}
		log.PromptTokens, log.CompletionTokens = input.Consume.PromptTokens, input.Consume.CompletionTokens
		log.UseTime, log.IsStream, log.Other = input.Consume.UseTimeSeconds, input.Consume.IsStream, string(encoded)
	}
	payload, err := common.Marshal(log)
	if err != nil {
		return err
	}
	return tx.Create(&CreditLogOutbox{UserID: request.UserID, RequestID: request.ID, EventID: eventID, Payload: string(payload), State: "pending", CreatedAt: now}).Error
}

// Delivery needs no cross-database transaction or long-held account lock.
// The sink's unique receipt serializes concurrent delivery and preserves its
// result after response loss. An acknowledgement failure only rechecks it.
func DeliverCreditLog(primary, logs *gorm.DB, outboxID, now int64) error {
	if primary == nil || logs == nil || outboxID <= 0 || !validCreditTime(now) {
		return ErrCreditInvalid
	}
	var pending CreditLogOutbox
	if err := primary.First(&pending, outboxID).Error; err != nil {
		return err
	}
	if pending.State == "delivered" {
		return nil
	}
	if pending.State != "pending" {
		return ErrCreditNeedsReview
	}
	// A nontransactional sink must retain a visible pending item. It cannot use
	// this SQL receipt contract to pretend it guarantees exactly-once inserts.
	if logs.Dialector.Name() == "clickhouse" {
		return ErrCreditNeedsReview
	}
	var log Log
	if err := common.UnmarshalJsonStr(pending.Payload, &log); err != nil {
		return err
	}
	if log.Id != 0 || log.UserId != pending.UserID || log.RequestId != pending.EventID || log.Quota < 0 || log.Type != LogTypeConsume {
		return ErrCreditInvariant
	}
	if err := deliverCreditLogPayload(logs, pending.Payload, log, now); err != nil {
		return err
	}
	return primary.Model(&pending).Where("state = ?", "pending").Updates(map[string]any{"state": "delivered", "delivered_at": now, "last_error": ""}).Error
}

// Both original and corrective projections share the same transactional sink
// receipt, while their callers enforce the separate log contracts.
func deliverCreditLogPayload(logs *gorm.DB, payload string, log Log, now int64) error {
	eventDigest, err := creditDigest(log.RequestId)
	if err != nil {
		return err
	}
	fingerprint, err := creditDigest(payload)
	if err != nil {
		return err
	}
	err = logs.Transaction(func(tx *gorm.DB) error {
		receipt := CreditLogDelivery{EventDigest: eventDigest, Fingerprint: fingerprint, CreatedAt: now}
		created := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&receipt)
		if created.Error != nil {
			return created.Error
		}
		if created.RowsAffected == 0 {
			if err := tx.First(&receipt, "event_digest = ?", eventDigest).Error; err != nil {
				return err
			}
			if receipt.Fingerprint != fingerprint || receipt.LogID <= 0 {
				return ErrCreditInvariant
			}
			return nil
		}
		if err := tx.Create(&log).Error; err != nil {
			return err
		}
		return tx.Model(&receipt).Update("log_id", log.Id).Error
	})
	if err != nil {
		return err
	}
	return nil
}

func DeliverCreditBillAdjustmentLog(primary, logs *gorm.DB, adjustmentID, now int64) error {
	if primary == nil || logs == nil || adjustmentID <= 0 || !validCreditTime(now) {
		return ErrCreditInvalid
	}
	var adjustment CreditBillAdjustment
	if err := primary.First(&adjustment, adjustmentID).Error; err != nil {
		return err
	}
	if adjustment.LogState == "delivered" {
		return nil
	}
	if adjustment.LogState != "pending" || logs.Dialector.Name() == "clickhouse" {
		return ErrCreditNeedsReview
	}
	var log Log
	if err := common.UnmarshalJsonStr(adjustment.LogPayload, &log); err != nil {
		return err
	}
	logType := LogTypeManage
	if adjustment.Refunded > 0 {
		logType = LogTypeRefund
	}
	if log.Id != 0 || log.UserId != adjustment.UserID || log.RequestId != adjustment.LogEventID || adjustment.Refunded < 0 || adjustment.Refunded > common.MaxQuota || int64(log.Quota) != adjustment.Refunded || log.Type != logType {
		return ErrCreditInvariant
	}
	if err := deliverCreditLogPayload(logs, adjustment.LogPayload, log, now); err != nil {
		return err
	}
	return primary.Model(&adjustment).Where("log_state = ?", "pending").Updates(map[string]any{"log_state": "delivered", "log_delivered_at": now, "log_last_error": ""}).Error
}

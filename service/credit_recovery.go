package service

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	"github.com/QuantumNous/new-api/types"
	"gorm.io/gorm"
)

// Recovery runs through the existing scheduled-task runner. Request fences
// remain necessary even when the runner's outer task lease is lost.
type creditRecoveryHandler struct{}

func (creditRecoveryHandler) Type() string { return model.SystemTaskTypeCreditRecovery }
func (creditRecoveryHandler) Enabled() bool {
	return common.GetEnvOrDefaultBool("CREDIT_RECOVERY_ENABLED", true)
}
func (creditRecoveryHandler) Interval() time.Duration {
	seconds := common.GetEnvOrDefault("CREDIT_RECOVERY_INTERVAL_SECONDS", 30)
	if seconds < 1 || seconds > 3600 {
		seconds = 30
	}
	return time.Duration(seconds) * time.Second
}
func (creditRecoveryHandler) NewPayload() any { return nil }

func init() { RegisterSystemTaskHandler(creditRecoveryHandler{}) }

type CreditRecoverySummary struct {
	Requests    int `json:"requests"`
	Errors      int `json:"errors"`
	Logs        int `json:"logs"`
	PendingLogs int `json:"pending_logs"`
}

func (creditRecoveryHandler) Run(ctx context.Context, task *model.SystemTask, runnerID string) {
	summary, err := RunCreditRecoveryPass(ctx, model.DB, model.LOG_DB, runnerID, common.GetTimestamp())
	status := model.SystemTaskStatusSucceeded
	message := ""
	if err != nil {
		status = model.SystemTaskStatusFailed
		message = err.Error()
	}
	if finishErr := model.FinishSystemTask(task.TaskID, runnerID, status, summary, message); finishErr != nil {
		common.SysError(fmt.Sprintf("credit recovery result incomplete: %v", finishErr))
	}
}

func RunCreditRecoveryPass(ctx context.Context, primary, logs *gorm.DB, owner string, now int64) (CreditRecoverySummary, error) {
	var summary CreditRecoverySummary
	if primary == nil || logs == nil {
		return summary, model.ErrCreditInvalid
	}
	maxAttempts := common.GetEnvOrDefault("CREDIT_RECOVERY_MAX_ATTEMPTS", 20)
	if maxAttempts < 1 || maxAttempts > 1000 {
		return summary, model.ErrCreditInvalid
	}
	primary = primary.WithContext(ctx)
	cursor := int64(0)
	for {
		if err := ctx.Err(); err != nil {
			return summary, err
		}
		results, next, err := model.RecoverCreditRequests(primary, owner, cursor, 100, now, common.GetTimestamp)
		if err != nil {
			return summary, err
		}
		summary.Requests += len(results)
		for _, result := range results {
			if result.Error != "" {
				summary.Errors++
			} else if result.State == "executing" {
				if err := resumeCompletedCreditTask(primary, result.RequestID, now); err != nil {
					summary.Errors++
					common.SysError(fmt.Sprintf("credit terminal task recovery request=%d: %v", result.RequestID, err))
				}
			}
		}
		if next == cursor {
			break
		}
		cursor = next
	}
	cursor = 0
	for {
		if err := ctx.Err(); err != nil {
			return summary, err
		}
		var items []model.CreditLogOutbox
		if err := primary.Where("id > ? AND state = ? AND next_retry_at <= ?", cursor, "pending", now).Order("id asc").Limit(100).Find(&items).Error; err != nil {
			return summary, err
		}
		if len(items) == 0 {
			break
		}
		for _, item := range items {
			cursor = item.ID
			if err := model.DeliverCreditLog(primary, logs.WithContext(ctx), item.ID, now); err != nil {
				summary.PendingLogs++
				// The full immutable payload stays in the primary DB. Error details are
				// restricted to administrators, with a finite retry schedule.
				text := []rune(err.Error())
				if len(text) > 1024 {
					text = text[:1024]
				}
				state := "pending"
				if item.Attempts >= int64(maxAttempts)-1 {
					state = "review"
				}
				if recordErr := primary.Model(&item).Where("state = ? AND attempts = ?", "pending", item.Attempts).Updates(map[string]any{"attempts": gorm.Expr("attempts + 1"), "last_error": string(text), "next_retry_at": now + min(int64(3600), int64(30)<<min(item.Attempts, int64(7))), "state": state}).Error; recordErr != nil {
					return summary, recordErr
				}
			} else {
				summary.Logs++
			}
		}
	}
	cursor = 0
	for {
		if err := ctx.Err(); err != nil {
			return summary, err
		}
		var adjustments []model.CreditBillAdjustment
		if err := primary.Where("id > ? AND log_state = ? AND log_next_retry_at <= ?", cursor, "pending", now).Order("id asc").Limit(100).Find(&adjustments).Error; err != nil {
			return summary, err
		}
		if len(adjustments) == 0 {
			break
		}
		for _, adjustment := range adjustments {
			cursor = adjustment.ID
			if err := model.DeliverCreditBillAdjustmentLog(primary, logs.WithContext(ctx), adjustment.ID, now); err != nil {
				summary.PendingLogs++
				message := []rune(err.Error())
				if len(message) > 1024 {
					message = message[:1024]
				}
				state := "pending"
				if adjustment.LogAttempts >= int64(maxAttempts)-1 {
					state = "review"
				}
				if recordErr := primary.Model(&adjustment).Where("log_state = ? AND log_attempts = ?", "pending", adjustment.LogAttempts).Updates(map[string]any{"log_state": state, "log_attempts": gorm.Expr("log_attempts + 1"), "log_last_error": string(message), "log_next_retry_at": now + min(int64(3600), int64(30)<<min(adjustment.LogAttempts, int64(7)))}).Error; recordErr != nil {
					return summary, recordErr
				}
			} else {
				summary.Logs++
			}
		}
	}
	return summary, nil
}

// The task CAS saves completion facts with SUCCESS before the financial
// barrier. Recover that small crash window using the original frozen price,
// never current plugin metadata or current administrator prices.
func resumeCompletedCreditTask(db *gorm.DB, requestID, now int64) error {
	var request model.CreditRequest
	if err := db.First(&request, requestID).Error; err != nil {
		return err
	}
	if request.TaskRowID <= 0 || request.State != "executing" || request.IntentKind != "" || request.UsageEvidenceID != 0 || request.ReviewEvidenceID != 0 {
		return nil
	}
	if request.TaskKind == "midjourney" {
		return resumeCompletedMidjourneyCreditTask(db, request, now)
	}
	if request.TaskKind != "task" {
		return nil
	}
	var task model.Task
	if err := db.Where("id = ? AND user_id = ?", request.TaskRowID, request.UserID).First(&task).Error; err != nil {
		return err
	}
	if task.TaskID != request.TaskID || task.PrivateData.CreditRequestID != request.ID || task.PrivateData.BillingSource != BillingSourceCreditPacks {
		return model.ErrCreditInvariant
	}
	if task.Status != model.TaskStatusSuccess {
		return nil
	}
	lease, err := model.ClaimCreditExecution(db, request.UserID, request.ID, common.NewRequestId(), 120, now, common.GetTimestamp)
	if errors.Is(err, model.ErrCreditLeaseLost) || errors.Is(err, model.ErrCreditOperationConflict) {
		return nil
	}
	if err != nil {
		return err
	}
	var pricing struct {
		Price      types.PriceData              `json:"price"`
		Expression *billingexpr.BillingSnapshot `json:"expression"`
	}
	actual := task.Quota
	var clamp *common.QuotaClamp
	_, submittedPrice, err := creditSubmittedPrice(db, request)
	if err == nil {
		err = common.UnmarshalJsonStr(submittedPrice, &pricing)
	}
	if err == nil && pricing.Expression != nil {
		bc := task.PrivateData.BillingContext
		if bc == nil || bc.TieredSnapshot == nil {
			err = model.ErrCreditInvariant
		} else {
			pricing.Expression.MeasuredUsageFacts = maps.Clone(bc.TieredSnapshot.MeasuredUsageFacts)
			var result billingexpr.TieredResult
			result, pricing.Expression.UsageFacts, err = EvaluateTaskCompletionUsage(pricing.Expression, pricing.Expression.MeasuredUsageFacts)
			if err == nil {
				actual = result.ActualQuotaAfterGroup
				clamp = result.Clamp
				pricing.Expression.EstimatedTier = result.MatchedTier
				bc.TieredSnapshot = pricing.Expression
				bc.GroupRatio = pricing.Expression.GroupRatio
			}
		}
	} else if err == nil && !pricing.Price.UsePrice && (task.PrivateData.BillingContext == nil || !task.PrivateData.BillingContext.PerCallBilling) {
		// Non-expression adaptor adjustments cannot be reconstructed from an
		// old submission quota alone; retain the hold for explicit review.
		err = model.ErrCreditNeedsReview
	}
	if err == nil {
		other := taskBillingOther(&task)
		attachQuotaSaturationToOther(other, clamp)
		if clamp != nil {
			logger.LogWarn(db.Statement.Context, fmt.Sprintf("quota saturation on credit task recovery: request=%d task=%s op=%s kind=%s original=%g clamped=%d", request.ID, task.TaskID, clamp.Op, clamp.Kind, clamp.Original, clamp.Clamped))
		}
		err = recordCreditTaskConsumeEvidence(db, request.UserID, request.ID, actual, pricing.Expression, other, lease)
	}
	if err == nil {
		_, err = model.FinishCreditRequest(db, request.UserID, request.ID, "settle", int64(actual), common.GetTimestamp(), lease)
	}
	if err != nil {
		if errors.Is(err, model.ErrCreditNeedsReview) || errors.Is(err, model.ErrCreditInvariant) {
			if reviewErr := model.MarkCreditRequestReviewAt(db, request.UserID, request.ID, common.GetTimestamp(), lease); reviewErr != nil {
				return errors.Join(err, reviewErr)
			}
		}
		if recordErr := model.RecordCreditRecoveryFailure(db, lease, err, common.GetTimestamp()); recordErr != nil {
			return errors.Join(err, recordErr)
		}
	}
	return err
}

// Midjourney saves its submitted per-call fee with the terminal task row.
// No new supplier request or current price lookup is needed to settle it.
func resumeCompletedMidjourneyCreditTask(db *gorm.DB, request model.CreditRequest, now int64) error {
	var task model.Midjourney
	if err := db.Where("id = ? AND user_id = ?", request.TaskRowID, request.UserID).First(&task).Error; err != nil {
		return err
	}
	if task.MjId != request.TaskID || task.CreditRequestID != request.ID {
		return model.ErrCreditInvariant
	}
	if task.Status != "SUCCESS" {
		return nil
	}
	lease, err := model.ClaimCreditExecution(db, request.UserID, request.ID, common.NewRequestId(), 120, now, common.GetTimestamp)
	if errors.Is(err, model.ErrCreditLeaseLost) || errors.Is(err, model.ErrCreditOperationConflict) {
		return nil
	}
	if err != nil {
		return err
	}
	other := model.NewLogOther()
	other.SetPublic("task_id", task.MjId)
	err = recordCreditTaskConsumeEvidence(db, request.UserID, request.ID, task.Quota, nil, other, lease)
	if err == nil {
		_, err = model.FinishCreditRequest(db, request.UserID, request.ID, "settle", int64(task.Quota), common.GetTimestamp(), lease)
	}
	if err != nil {
		if errors.Is(err, model.ErrCreditNeedsReview) || errors.Is(err, model.ErrCreditInvariant) {
			if reviewErr := model.MarkCreditRequestReviewAt(db, request.UserID, request.ID, common.GetTimestamp(), lease); reviewErr != nil {
				return errors.Join(err, reviewErr)
			}
		}
		if recordErr := model.RecordCreditRecoveryFailure(db, lease, err, common.GetTimestamp()); recordErr != nil {
			return errors.Join(err, recordErr)
		}
	}
	return err
}

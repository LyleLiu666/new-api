package service

import (
	"context"
	"fmt"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
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
				if recordErr := primary.Model(&item).Where("state = ?", "pending").Updates(map[string]any{"attempts": gorm.Expr("attempts + 1"), "last_error": string(text), "next_retry_at": now + min(int64(3600), int64(30)<<min(item.Attempts, int64(7))), "state": state}).Error; recordErr != nil {
					return summary, recordErr
				}
			} else {
				summary.Logs++
			}
		}
	}
	return summary, nil
}

package controller

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

func creditAPIError(c *gin.Context, err error) {
	status, code, message := http.StatusInternalServerError, "credit_storage_error", "Credit operation could not be completed"
	switch {
	case errors.Is(err, model.ErrCreditInvalid):
		status, code, message = http.StatusBadRequest, "invalid_credit_input", err.Error()
	case errors.Is(err, model.ErrCreditLeaseLost), errors.Is(err, model.ErrCreditNeedsReview):
		status, code, message = http.StatusConflict, "credit_work_unavailable", err.Error()
	case errors.Is(err, model.ErrCreditOperationConflict):
		status, code, message = http.StatusConflict, "credit_operation_conflict", err.Error()
	case errors.Is(err, model.ErrUserQuotaPermission):
		status, code, message = http.StatusForbidden, "credit_permission_denied", err.Error()
	case errors.Is(err, model.ErrCreditOperationRequired):
		status, code, message = http.StatusConflict, "credit_accounting_required", err.Error()
	case errors.Is(err, gorm.ErrRecordNotFound):
		status, code, message = http.StatusNotFound, "credit_record_not_found", "Credit record not found"
	case errors.Is(err, model.ErrWalletQuotaLimitExceeded):
		status, code, message = http.StatusConflict, "credit_capacity_exceeded", err.Error()
	}
	c.JSON(status, gin.H{"success": false, "code": code, "message": message})
}

func AdminListCreditPolicies(c *gin.Context) {
	if err := model.AuthorizeCreditPolicyAdmin(model.DB, c.GetInt("id")); err != nil {
		creditAPIError(c, err)
		return
	}
	var policies []model.CreditSourcePolicy
	if err := model.DB.Order("source_type ASC").Limit(20).Find(&policies).Error; err != nil {
		creditAPIError(c, err)
		return
	}
	common.ApiSuccess(c, policies)
}

func AdminPutCreditPolicy(c *gin.Context) {
	if err := model.AuthorizeCreditPolicyAdmin(model.DB, c.GetInt("id")); err != nil {
		creditAPIError(c, err)
		return
	}
	var request struct {
		model.CreditSourcePolicy
		ExpectedRevision int64 `json:"expected_revision"`
	}
	if err := common.DecodeJson(c.Request.Body, &request); err != nil {
		creditAPIError(c, model.ErrCreditInvalid)
		return
	}
	policy, err := model.PutCreditSourcePolicy(model.DB, request.CreditSourcePolicy, request.ExpectedRevision)
	if err != nil {
		creditAPIError(c, err)
		return
	}
	common.ApiSuccess(c, policy)
}

func AdminGrantCredit(c *gin.Context) {
	var request struct {
		UserID    int    `json:"user_id"`
		EventID   string `json:"event_id"`
		Amount    int64  `json:"amount"`
		StartsAt  int64  `json:"starts_at"`
		ExpiresAt int64  `json:"expires_at"`
		UseMask   int    `json:"use_mask"`
		Reason    string `json:"reason"`
	}
	if err := common.DecodeJson(c.Request.Body, &request); err != nil {
		creditAPIError(c, model.ErrCreditInvalid)
		return
	}
	pack, err := model.GrantAdminCredit(model.DB, model.CreditGrant{UserID: request.UserID, SourceID: request.EventID, Amount: request.Amount, StartsAt: request.StartsAt, ExpiresAt: request.ExpiresAt, UseMask: request.UseMask, ActorID: c.GetInt("id"), Reason: request.Reason}, common.GetTimestamp())
	if err != nil {
		creditAPIError(c, err)
		return
	}
	common.ApiSuccess(c, gin.H{"pack_id": pack.ID, "user_id": pack.UserID, "issued": pack.Issued, "starts_at": pack.StartsAt, "expires_at": pack.ExpiresAt, "use_mask": pack.UseMask})
}

func AdminOpenCreditReview(c *gin.Context) {
	var request model.CreditReviewInput
	if err := common.DecodeJson(c.Request.Body, &request); err != nil {
		creditAPIError(c, model.ErrCreditInvalid)
		return
	}
	request.ActorID = c.GetInt("id")
	review, err := model.OpenCreditReviewCase(model.DB, request, common.GetTimestamp())
	if err != nil {
		creditAPIError(c, err)
		return
	}
	common.ApiSuccess(c, creditReviewResponse(review))
}

func AdminRecordCreditCashOutcome(c *gin.Context) {
	var request model.CreditCashOutcome
	if err := common.DecodeJson(c.Request.Body, &request); err != nil {
		creditAPIError(c, model.ErrCreditInvalid)
		return
	}
	request.ActorID = c.GetInt("id")
	if err := model.RecordCreditCashOutcome(model.DB, request, common.GetTimestamp()); err != nil {
		creditAPIError(c, err)
		return
	}
	common.ApiSuccess(c, nil)
}

func creditReviewResponse(review model.CreditReviewCase) gin.H {
	return gin.H{"case_id": review.ID, "user_id": review.UserID, "pack_id": review.PackID, "event_id": review.EventID, "actor_id": review.ActorID, "reason": review.Reason, "created_at": review.CreatedAt, "cash_state": review.CashState, "cash_reference": review.CashReference, "cash_evidence": review.CashEvidence, "cash_actor_id": review.CashActorID, "cash_recorded_at": review.CashRecordedAt}
}

func AdminListCreditReviews(c *gin.Context) {
	userID, err := strconv.Atoi(c.Query("user_id"))
	if err != nil || userID <= 0 {
		creditAPIError(c, model.ErrCreditInvalid)
		return
	}
	page := common.GetPageQuery(c)
	reviews, total, err := model.ListCreditReviewCases(model.DB, userID, c.GetInt("id"), page.GetStartIdx(), page.GetPageSize())
	if err != nil {
		creditAPIError(c, err)
		return
	}
	items := make([]gin.H, 0, len(reviews))
	for _, review := range reviews {
		items = append(items, creditReviewResponse(review))
	}
	page.SetTotal(int(total))
	page.SetItems(items)
	common.ApiSuccess(c, page)
}

// Recovery queries are scoped to a target user and recheck the administrator
// in the primary DB. Internal lease identities and immutable log payloads are
// excluded from these responses.
func AdminListCreditWork(c *gin.Context) {
	userID, err := strconv.Atoi(c.Query("user_id"))
	if err != nil || userID <= 0 {
		creditAPIError(c, model.ErrCreditInvalid)
		return
	}
	if err := model.AuthorizeCreditAccountAdmin(model.DB, c.GetInt("id"), userID); err != nil {
		creditAPIError(c, err)
		return
	}
	page := common.GetPageQuery(c)
	var requests []model.CreditRequest
	query := model.DB.Model(&model.CreditRequest{}).Where("user_id = ? AND (state IN ? OR recovery_blocked_at > 0)", userID, []string{"reserved", "executing", "pending", "review"})
	var total int64
	if err := query.Count(&total).Error; err != nil {
		creditAPIError(c, err)
		return
	}
	if err := query.Order("id asc").Offset(page.GetStartIdx()).Limit(page.GetPageSize()).Find(&requests).Error; err != nil {
		creditAPIError(c, err)
		return
	}
	items := make([]gin.H, 0, len(requests))
	for _, request := range requests {
		items = append(items, gin.H{"request_id": request.ID, "logical_request_id": request.RequestID, "user_id": request.UserID, "state": request.State, "reserved": request.Reserved, "actual": request.Actual, "charged": request.Charged, "uncollected": request.Uncollected, "submitted_at": request.SubmittedAt, "lease_until": request.LeaseUntil, "recovery_attempts": request.RecoveryAttempts, "last_error": request.LastRecoveryError, "next_retry_at": request.NextRecoveryAt, "blocked_at": request.RecoveryBlockedAt})
	}
	var outbox []model.CreditLogOutbox
	var logsTotal int64
	logQuery := model.DB.Model(&model.CreditLogOutbox{}).Where("user_id = ? AND state <> ?", userID, "delivered")
	if err := logQuery.Count(&logsTotal).Error; err != nil {
		creditAPIError(c, err)
		return
	}
	if err := logQuery.Select("id", "request_id", "state", "attempts", "last_error", "next_retry_at", "created_at").Order("id asc").Offset(page.GetStartIdx()).Limit(page.GetPageSize()).Find(&outbox).Error; err != nil {
		creditAPIError(c, err)
		return
	}
	logItems := make([]gin.H, 0, len(outbox))
	for _, item := range outbox {
		logItems = append(logItems, gin.H{"id": item.ID, "request_id": item.RequestID, "state": item.State, "attempts": item.Attempts, "last_error": item.LastError, "next_retry_at": item.NextRetryAt, "created_at": item.CreatedAt})
	}
	common.ApiSuccess(c, gin.H{"requests": items, "total": total, "logs": logItems, "logs_total": logsTotal})
}

func AdminReconcileCreditAccount(c *gin.Context) {
	userID, err := strconv.Atoi(c.Query("user_id"))
	if err != nil || userID <= 0 {
		creditAPIError(c, model.ErrCreditInvalid)
		return
	}
	if err := model.AuthorizeCreditAccountAdmin(model.DB, c.GetInt("id"), userID); err != nil {
		creditAPIError(c, err)
		return
	}
	differences, err := model.ReconcileCreditAccount(model.DB, userID)
	if err != nil {
		creditAPIError(c, err)
		return
	}
	common.ApiSuccess(c, gin.H{"user_id": userID, "differences": differences, "consistent": len(differences) == 0})
}

func AdminRetryCreditWork(c *gin.Context) {
	var input model.CreditRecoveryResume
	if err := common.DecodeJson(c.Request.Body, &input); err != nil {
		creditAPIError(c, model.ErrCreditInvalid)
		return
	}
	input.ActorID = c.GetInt("id")
	if err := model.ResumeCreditRecovery(model.DB, input, common.GetTimestamp()); err != nil {
		creditAPIError(c, err)
		return
	}
	common.ApiSuccess(c, nil)
}

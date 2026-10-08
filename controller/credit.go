package controller

import (
	"errors"
	"math"
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
	case errors.Is(err, model.ErrCreditInsufficient):
		status, code, message = http.StatusConflict, "insufficient_eligible_credits", err.Error()
	case errors.Is(err, model.ErrSubscriptionPurchaseUnavailable):
		status, code, message = http.StatusConflict, "subscription_purchase_unavailable", err.Error()
	case errors.Is(err, model.ErrSubscriptionPurchaseConflict):
		status, code, message = http.StatusConflict, "subscription_purchase_conflict", err.Error()
	case errors.Is(err, model.ErrSubscriptionVersionConflict):
		status, code, message = http.StatusConflict, "subscription_version_conflict", err.Error()
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
	if page.GetPage() <= 0 || page.GetPageSize() <= 0 || int64(page.GetPage()-1) > math.MaxInt/int64(page.GetPageSize()) {
		creditAPIError(c, model.ErrCreditInvalid)
		return
	}
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
	var revisions []model.CreditBillAdjustment
	var revisionsTotal int64
	revisionQuery := model.DB.Model(&model.CreditBillAdjustment{}).Where("user_id = ? AND log_state <> ?", userID, "delivered")
	if err := revisionQuery.Count(&revisionsTotal).Error; err != nil {
		creditAPIError(c, err)
		return
	}
	if err := revisionQuery.Select("id", "request_id", "revision", "log_state", "log_attempts", "log_last_error", "log_next_retry_at", "created_at").Order("id asc").Offset(page.GetStartIdx()).Limit(page.GetPageSize()).Find(&revisions).Error; err != nil {
		creditAPIError(c, err)
		return
	}
	revisionItems := make([]gin.H, 0, len(revisions))
	for _, revision := range revisions {
		revisionItems = append(revisionItems, gin.H{"id": revision.ID, "request_id": revision.RequestID, "revision": revision.Revision, "state": revision.LogState, "attempts": revision.LogAttempts, "last_error": revision.LogLastError, "next_retry_at": revision.LogNextRetryAt, "created_at": revision.CreatedAt})
	}
	common.ApiSuccess(c, gin.H{"requests": items, "total": total, "logs": logItems, "logs_total": logsTotal, "adjustment_logs": revisionItems, "adjustment_logs_total": revisionsTotal})
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

func AdminAdjustCreditBill(c *gin.Context) {
	var body struct {
		model.CreditBillAdjustmentInput
		ExpectedRevision *int64 `json:"expected_revision"`
	}
	if err := common.DecodeJson(c.Request.Body, &body); err != nil || body.ExpectedRevision == nil {
		creditAPIError(c, model.ErrCreditInvalid)
		return
	}
	input := body.CreditBillAdjustmentInput
	input.ExpectedRevision, input.ActorID = *body.ExpectedRevision, c.GetInt("id")
	adjustment, err := model.AdjustCreditBill(model.DB, input, common.GetTimestamp())
	if err != nil {
		creditAPIError(c, err)
		return
	}
	common.ApiSuccess(c, adjustment)
}

func AdminApproveCreditBillReview(c *gin.Context) {
	var body struct {
		model.CreditBillReviewInput
		ExpectedEvidenceID *int64 `json:"expected_evidence_id"`
	}
	if err := common.DecodeJson(c.Request.Body, &body); err != nil || body.ExpectedEvidenceID == nil {
		creditAPIError(c, model.ErrCreditInvalid)
		return
	}
	input := body.CreditBillReviewInput
	input.ActorID, input.ExpectedEvidenceID = c.GetInt("id"), *body.ExpectedEvidenceID
	approval, err := model.ApproveCreditBillReview(model.DB, input, common.GetTimestamp())
	if err != nil {
		creditAPIError(c, err)
		return
	}
	// This acknowledges the durable approval; recovery completes settlement.
	common.ApiSuccess(c, gin.H{"approval": approval})
}

func GetCreditBill(c *gin.Context) {
	creditBillResponse(c, c.GetInt("id"), false)
}

func AdminGetCreditBill(c *gin.Context) {
	userID, err := strconv.Atoi(c.Query("user_id"))
	if err != nil || userID <= 0 {
		creditAPIError(c, model.ErrCreditInvalid)
		return
	}
	if err := model.AuthorizeCreditAccountAdmin(model.DB, c.GetInt("id"), userID); err != nil {
		creditAPIError(c, err)
		return
	}
	creditBillResponse(c, userID, true)
}

// An explicit projection separates user-visible fees from administrative
// evidence, execution identities and private frozen gateway configuration.
func creditBillResponse(c *gin.Context, userID int, admin bool) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	page := common.GetPageQuery(c)
	if err != nil || id <= 0 || userID <= 0 || page.GetPage() <= 0 || page.GetPageSize() <= 0 || page.GetPage()-1 > math.MaxInt/page.GetPageSize() {
		creditAPIError(c, model.ErrCreditInvalid)
		return
	}
	var request model.CreditRequest
	if err := model.DB.Where("id = ? AND user_id = ?", id, userID).First(&request).Error; err != nil {
		creditAPIError(c, err)
		return
	}
	balance, err := model.GetCreditBillBalance(model.DB, userID, id)
	if err != nil {
		creditAPIError(c, err)
		return
	}
	var rows []model.CreditBillAdjustment
	if err := model.DB.Where("request_id = ? AND user_id = ? AND revision <= ?", id, userID, balance.Revision).Order("revision desc").Offset(page.GetStartIdx()).Limit(page.GetPageSize()).Find(&rows).Error; err != nil {
		creditAPIError(c, err)
		return
	}
	revisions := make([]gin.H, 0, len(rows))
	for _, row := range rows {
		item := gin.H{"id": row.ID, "revision": row.Revision, "reference_quota": row.ReferenceQuota, "charged": row.Charged, "uncollected": row.Uncollected, "refunded": row.Refunded, "created_at": row.CreatedAt}
		if admin {
			item["actor_id"], item["reason"], item["usage_evidence_id"] = row.ActorID, row.Reason, row.UsageEvidenceID
			item["log_state"], item["log_attempts"], item["log_last_error"] = row.LogState, row.LogAttempts, row.LogLastError
		}
		revisions = append(revisions, item)
	}
	response := gin.H{"id": request.ID, "user_id": userID, "request_id": request.RequestID, "model_name": request.ModelName, "protocol": request.Protocol, "state": request.State, "funding_source": request.FundingSource, "subscription_id": request.SubscriptionID, "created_at": request.CreatedAt, "original": gin.H{"reference_quota": request.Actual, "charged": request.Charged, "uncollected": request.Uncollected}, "current": balance, "revisions": revisions, "total": balance.Revision, "page": page.GetPage(), "page_size": page.GetPageSize(), "manually_confirmed": request.ReviewEvidenceID > 0}
	usage, err := model.GetCreditBillUsage(model.DB, userID, id, balance.Revision)
	if err != nil {
		creditAPIError(c, err)
		return
	}
	response["usage"] = usage
	if admin {
		response["usage_evidence_id"] = request.UsageEvidenceID
	}
	if balance.Revision > 0 {
		originalUsage, err := model.GetCreditBillUsage(model.DB, userID, id, 0)
		if err != nil {
			creditAPIError(c, err)
			return
		}
		response["original_usage"] = originalUsage
	}
	if admin && request.ReviewEvidenceID > 0 {
		details, err := model.GetCreditBillReviewDetails(model.DB, userID, id, c.GetInt("id"))
		if err != nil {
			creditAPIError(c, err)
			return
		}
		response["usage_review"] = details
	}
	common.ApiSuccess(c, response)
}

func GetCreditAccount(c *gin.Context) {
	creditAccountResponse(c, c.GetInt("id"), false)
}

func AdminGetCreditAccount(c *gin.Context) {
	userID, err := strconv.Atoi(c.Query("user_id"))
	if err != nil || userID <= 0 {
		creditAPIError(c, model.ErrCreditInvalid)
		return
	}
	if err := model.AuthorizeCreditAccountAdmin(model.DB, c.GetInt("id"), userID); err != nil {
		creditAPIError(c, err)
		return
	}
	creditAccountResponse(c, userID, true)
}

// Pack quantities remain conserved even when the expiry worker has not run.
// Only active, unblocked funds count toward the relevant purpose's balance.
func creditAccountResponse(c *gin.Context, userID int, admin bool) {
	page := common.GetPageQuery(c)
	if userID <= 0 || page.GetPage() <= 0 || page.GetPageSize() <= 0 || page.GetPage()-1 > math.MaxInt/page.GetPageSize() {
		creditAPIError(c, model.ErrCreditInvalid)
		return
	}
	var user model.User
	if err := model.DB.Select("id", "status", "accounting_version").First(&user, userID).Error; err != nil {
		creditAPIError(c, err)
		return
	}
	if !admin && user.Status != common.UserStatusEnabled {
		creditAPIError(c, model.ErrUserQuotaPermission)
		return
	}
	now := common.GetTimestamp()
	var totals struct {
		Total                 int64
		APIAvailable          int64
		SubscriptionAvailable int64
		Held                  int64
	}
	query := model.DB.Model(&model.CreditPack{}).Where("user_id = ?", userID)
	if err := query.Select(`COUNT(*) AS total,
		COALESCE(SUM(CASE WHEN starts_at <= ? AND expires_at > ? AND blocked_at = 0 AND use_mask IN (1, 3) THEN available ELSE 0 END), 0) AS api_available,
		COALESCE(SUM(CASE WHEN starts_at <= ? AND expires_at > ? AND blocked_at = 0 AND use_mask IN (2, 3) THEN available ELSE 0 END), 0) AS subscription_available,
		COALESCE(SUM(held), 0) AS held`, now, now, now, now).Scan(&totals).Error; err != nil {
		creditAPIError(c, err)
		return
	}
	for _, amount := range []int64{totals.APIAvailable, totals.SubscriptionAvailable, totals.Held} {
		if amount < 0 || amount > common.MaxWalletQuota {
			creditAPIError(c, model.ErrCreditInvariant)
			return
		}
	}
	var rows []model.CreditPack
	if err := model.DB.Where("user_id = ?", userID).Order("expires_at ASC, created_at ASC, id ASC").Offset(page.GetStartIdx()).Limit(page.GetPageSize()).Find(&rows).Error; err != nil {
		creditAPIError(c, err)
		return
	}
	packs := make([]gin.H, 0, len(rows))
	for _, pack := range rows {
		if !pack.QuantitiesValid() {
			creditAPIError(c, model.ErrCreditInvariant)
			return
		}
		state := "active"
		if pack.ExpiresAt <= now {
			state = "expired"
			pack.Expired += pack.Available
			pack.Available = 0
		} else if pack.BlockedAt > 0 {
			state = "blocked"
		} else if pack.StartsAt > now {
			state = "scheduled"
		} else if pack.Available == 0 {
			state = "exhausted"
		}
		packs = append(packs, gin.H{"id": pack.ID, "source_type": pack.SourceType, "issued": pack.Issued, "available": pack.Available, "held": pack.Held, "spent": pack.Spent, "expired": pack.Expired, "revoked": pack.Revoked, "starts_at": pack.StartsAt, "expires_at": pack.ExpiresAt, "use_mask": pack.UseMask, "state": state})
	}
	common.ApiSuccess(c, gin.H{"user_id": userID, "accounting_version": user.AccountingVersion, "server_time": now, "api_available": totals.APIAvailable, "subscription_available": totals.SubscriptionAvailable, "held": totals.Held, "total": totals.Total, "page": page.GetPage(), "page_size": page.GetPageSize(), "packs": packs})
}

func ListCreditBills(c *gin.Context) {
	creditBillsResponse(c, c.GetInt("id"))
}

func AdminListCreditBills(c *gin.Context) {
	userID, err := strconv.Atoi(c.Query("user_id"))
	if err != nil || userID <= 0 {
		creditAPIError(c, model.ErrCreditInvalid)
		return
	}
	if err := model.AuthorizeCreditAccountAdmin(model.DB, c.GetInt("id"), userID); err != nil {
		creditAPIError(c, err)
		return
	}
	creditBillsResponse(c, userID)
}

func creditBillsResponse(c *gin.Context, userID int) {
	page := common.GetPageQuery(c)
	if userID <= 0 || page.GetPage() <= 0 || page.GetPageSize() <= 0 || page.GetPage()-1 > math.MaxInt/page.GetPageSize() {
		creditAPIError(c, model.ErrCreditInvalid)
		return
	}
	var total int64
	if err := model.DB.Model(&model.CreditRequest{}).Where("user_id = ?", userID).Count(&total).Error; err != nil {
		creditAPIError(c, err)
		return
	}
	var rows []model.CreditRequest
	if err := model.DB.Select("id", "user_id", "request_id", "model_name", "protocol", "state", "funding_source", "created_at", "actual", "charged", "uncollected").Where("user_id = ?", userID).Order("id DESC").Offset(page.GetStartIdx()).Limit(page.GetPageSize()).Find(&rows).Error; err != nil {
		creditAPIError(c, err)
		return
	}
	items := make([]gin.H, 0, len(rows))
	for _, row := range rows {
		balance, err := model.GetCreditBillBalance(model.DB, userID, row.ID)
		if err != nil {
			creditAPIError(c, err)
			return
		}
		items = append(items, gin.H{"id": row.ID, "request_id": row.RequestID, "model_name": row.ModelName, "protocol": row.Protocol, "state": row.State, "funding_source": row.FundingSource, "created_at": row.CreatedAt, "current": balance})
	}
	common.ApiSuccess(c, gin.H{"items": items, "total": total, "page": page.GetPage(), "page_size": page.GetPageSize(), "server_time": common.GetTimestamp()})
}

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

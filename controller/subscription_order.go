package controller

import (
	"math"
	"strconv"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
)

// User order views deliberately exclude gateway configuration, checkout URLs,
// buyer-session tokens and private review evidence.
type subscriptionPurchaseSummary struct {
	ID                int64  `json:"id"`
	PlanID            int    `json:"plan_id"`
	VersionID         int64  `json:"version_id"`
	Provider          string `json:"provider"`
	PriceMicros       int64  `json:"price_micros"`
	Currency          string `json:"currency"`
	CreatedAt         int64  `json:"created_at"`
	ExpiresAt         int64  `json:"expires_at"`
	PaidAt            int64  `json:"paid_at"`
	PaymentState      string `json:"payment_state"`
	NeedsReview       bool   `json:"needs_review"`
	LastReviewReason  string `json:"last_review_reason"`
	RightsCancelledAt int64  `json:"rights_cancelled_at"`
}

func GetSubscriptionPurchaseOrders(c *gin.Context) {
	page := common.GetPageQuery(c)
	if c.GetInt("id") <= 0 || page.GetPage() <= 0 || page.GetPageSize() <= 0 || page.GetPage()-1 > math.MaxInt/page.GetPageSize() {
		creditAPIError(c, model.ErrCreditInvalid)
		return
	}
	query := model.DB.Model(&model.SubscriptionPurchaseOrder{}).Where("user_id = ?", c.GetInt("id"))
	var total int64
	if err := query.Count(&total).Error; err != nil {
		creditAPIError(c, err)
		return
	}
	items := make([]subscriptionPurchaseSummary, 0)
	if err := query.Order("id desc").Offset(page.GetStartIdx()).Limit(page.GetPageSize()).Find(&items).Error; err != nil {
		creditAPIError(c, err)
		return
	}
	page.SetTotal(int(total))
	page.SetItems(items)
	common.ApiSuccess(c, page)
}

func GetSubscriptionPurchaseOrder(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 || c.GetInt("id") <= 0 {
		creditAPIError(c, model.ErrCreditInvalid)
		return
	}
	var summary subscriptionPurchaseSummary
	if err := model.DB.Model(&model.SubscriptionPurchaseOrder{}).Where("id = ? AND user_id = ?", id, c.GetInt("id")).First(&summary).Error; err != nil {
		creditAPIError(c, err)
		return
	}
	common.ApiSuccess(c, summary)
}

func AdminListSubscriptionPaymentReviews(c *gin.Context) {
	if err := model.AuthorizeSubscriptionPlanAdmin(model.DB, c.GetInt("id")); err != nil {
		creditAPIError(c, err)
		return
	}
	var actor model.User
	if err := model.DB.Select("id", "role").First(&actor, c.GetInt("id")).Error; err != nil {
		creditAPIError(c, err)
		return
	}
	page := common.GetPageQuery(c)
	if page.GetPage() <= 0 || page.GetPageSize() <= 0 || page.GetPage()-1 > math.MaxInt/page.GetPageSize() {
		creditAPIError(c, model.ErrCreditInvalid)
		return
	}
	query := model.DB.Model(&model.SubscriptionPurchaseOrder{}).Where("(needs_review = ? OR checkout_state IN ?)", true, []string{"started", "unknown"})
	if actor.Role != common.RoleRootUser {
		query = query.Where("user_id IN (?)", model.DB.Model(&model.User{}).Select("id").Where("role < ?", actor.Role))
	}
	if supplied := c.Query("user_id"); supplied != "" {
		userID, err := strconv.Atoi(supplied)
		if err != nil || userID <= 0 {
			creditAPIError(c, model.ErrCreditInvalid)
			return
		}
		if err := model.AuthorizeCreditAccountAdmin(model.DB, actor.Id, userID); err != nil {
			creditAPIError(c, err)
			return
		}
		query = query.Where("user_id = ?", userID)
	}
	var total int64
	if err := query.Count(&total).Error; err != nil {
		creditAPIError(c, err)
		return
	}
	orders := make([]model.SubscriptionPurchaseOrder, 0)
	if err := query.Order("id asc").Offset(page.GetStartIdx()).Limit(page.GetPageSize()).Find(&orders).Error; err != nil {
		creditAPIError(c, err)
		return
	}
	page.SetTotal(int(total))
	page.SetItems(orders)
	common.ApiSuccess(c, page)
}

func AdminGetSubscriptionPaymentReview(c *gin.Context) {
	if err := model.AuthorizeSubscriptionPlanAdmin(model.DB, c.GetInt("id")); err != nil {
		creditAPIError(c, err)
		return
	}
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		creditAPIError(c, model.ErrCreditInvalid)
		return
	}
	var order model.SubscriptionPurchaseOrder
	if err := model.DB.First(&order, id).Error; err != nil {
		creditAPIError(c, err)
		return
	}
	if err := model.AuthorizeCreditAccountAdmin(model.DB, c.GetInt("id"), order.UserID); err != nil {
		creditAPIError(c, err)
		return
	}
	// Bound evidence pagination as well as the order queue. Read newest first,
	// so the displayed latest fact is always the optimistic review boundary.
	page := common.GetPageQuery(c)
	if page.GetPage() <= 0 || page.GetPageSize() <= 0 || page.GetPage()-1 > math.MaxInt/page.GetPageSize() {
		creditAPIError(c, model.ErrCreditInvalid)
		return
	}
	var latest model.SubscriptionPaymentFact
	if err := model.DB.Where("order_id = ?", id).Order("id desc").Limit(1).Find(&latest).Error; err != nil {
		creditAPIError(c, err)
		return
	}
	facts := make([]model.SubscriptionPaymentFact, 0)
	if err := model.DB.Where("order_id = ?", id).Order("id desc").Offset(page.GetStartIdx()).Limit(page.GetPageSize()).Find(&facts).Error; err != nil {
		creditAPIError(c, err)
		return
	}
	common.ApiSuccess(c, gin.H{"order": order, "facts": facts, "latest_fact": latest, "latest_fact_id": latest.ID, "page": page.GetPage(), "page_size": page.GetPageSize()})
}

func AdminResolveSubscriptionPaymentReview(c *gin.Context) {
	var request struct {
		model.SubscriptionPaymentReview
		ExpectedFactID *int64 `json:"expected_fact_id"`
		AmountMicros   *int64 `json:"amount_micros"`
		PaidAt         *int64 `json:"paid_at"`
	}
	if err := common.DecodeJson(c.Request.Body, &request); err != nil || request.ExpectedFactID == nil || request.AmountMicros == nil || request.PaidAt == nil {
		creditAPIError(c, model.ErrCreditInvalid)
		return
	}
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		creditAPIError(c, model.ErrCreditInvalid)
		return
	}
	input := request.SubscriptionPaymentReview
	input.OrderID, input.ActorID = id, c.GetInt("id")
	input.ExpectedFactID, input.AmountMicros, input.PaidAt = *request.ExpectedFactID, *request.AmountMicros, *request.PaidAt
	result, err := model.ResolveSubscriptionPaymentReview(model.DB, input, common.GetTimestamp())
	if err != nil {
		creditAPIError(c, err)
		return
	}
	common.ApiSuccess(c, result)
}

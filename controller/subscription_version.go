package controller

import (
	"math"
	"strconv"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
)

func AdminListSubscriptionPlanVersions(c *gin.Context) {
	if err := model.AuthorizeSubscriptionPlanAdmin(model.DB, c.GetInt("id")); err != nil {
		creditAPIError(c, err)
		return
	}
	planID, err := strconv.Atoi(c.Param("id"))
	if err != nil || planID <= 0 {
		creditAPIError(c, model.ErrCreditInvalid)
		return
	}
	page := common.GetPageQuery(c)
	if page.GetPage() <= 0 || page.GetPageSize() <= 0 || page.GetPage()-1 > math.MaxInt/page.GetPageSize() {
		creditAPIError(c, model.ErrCreditInvalid)
		return
	}
	draft, err := model.GetSubscriptionPlanDraft(model.DB, planID)
	if err != nil {
		creditAPIError(c, err)
		return
	}
	query := model.DB.Model(&model.SubscriptionPlanVersion{}).Where("plan_id = ?", planID)
	var total, latest int64
	if err := query.Count(&total).Error; err != nil {
		creditAPIError(c, err)
		return
	}
	if err := query.Select("COALESCE(MAX(revision),0)").Scan(&latest).Error; err != nil {
		creditAPIError(c, err)
		return
	}
	versions := make([]model.SubscriptionPlanVersion, 0)
	if err := model.DB.Where("plan_id = ?", planID).Order("revision desc").Offset(page.GetStartIdx()).Limit(page.GetPageSize()).Find(&versions).Error; err != nil {
		creditAPIError(c, err)
		return
	}
	common.ApiSuccess(c, gin.H{"draft": draft, "latest_revision": latest, "versions": versions, "total": total})
}

func AdminPublishSubscriptionPlanVersion(c *gin.Context) {
	var request struct {
		ExpectedRevision   *int64 `json:"expected_revision"`
		ExpectedPlanDigest string `json:"expected_plan_digest"`
		EventID            string `json:"event_id"`
	}
	if err := common.DecodeJson(c.Request.Body, &request); err != nil || request.ExpectedRevision == nil {
		creditAPIError(c, model.ErrCreditInvalid)
		return
	}
	planID, err := strconv.Atoi(c.Param("id"))
	if err != nil || planID <= 0 {
		creditAPIError(c, model.ErrCreditInvalid)
		return
	}
	input := model.SubscriptionVersionPublish{PlanID: planID, ActorID: c.GetInt("id"), ExpectedRevision: *request.ExpectedRevision, ExpectedPlanDigest: request.ExpectedPlanDigest, EventID: request.EventID}
	version, err := model.PublishSubscriptionPlanVersion(model.DB, input, common.GetTimestamp())
	if err != nil {
		creditAPIError(c, err)
		return
	}
	common.ApiSuccess(c, version)
}

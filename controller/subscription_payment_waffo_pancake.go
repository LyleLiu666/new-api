package controller

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting"
	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"
	"github.com/thanhpk/randstr"
)

type SubscriptionWaffoPancakePayRequest struct {
	PlanId    int    `json:"plan_id"`
	VersionID int64  `json:"version_id"`
	EventID   string `json:"event_id"`
}

func SubscriptionRequestWaffoPancakePay(c *gin.Context) {
	if !requirePaymentCompliance(c) {
		return
	}

	var req SubscriptionWaffoPancakePayRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		common.ApiErrorMsg(c, "参数错误")
		return
	}
	accountingVersion, err := model.GetUserAccountingVersion(model.DB, c.GetInt("id"))
	if err != nil {
		creditAPIError(c, err)
		return
	}
	if accountingVersion == 1 {
		requestVersionedPancakeSubscriptionCheckout(c, req)
		return
	}
	if req.PlanId <= 0 {
		common.ApiErrorMsg(c, "参数错误")
		return
	}

	plan, err := model.GetSubscriptionPlanById(req.PlanId)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	if !plan.Enabled {
		common.ApiErrorMsg(c, "套餐未启用")
		return
	}
	if strings.TrimSpace(plan.WaffoPancakeProductId) == "" {
		common.ApiErrorMsg(c, "该套餐未配置 WaffoPancakeProductId")
		return
	}
	// Plan targets its own Pancake product, so we only require credentials
	// here — not the gateway-level WaffoPancakeProductID.
	if strings.TrimSpace(setting.WaffoPancakeMerchantID) == "" ||
		strings.TrimSpace(setting.WaffoPancakePrivateKey) == "" {
		common.ApiErrorMsg(c, "Waffo Pancake 未配置或密钥无效")
		return
	}

	userId := c.GetInt("id")
	user, err := model.GetUserById(userId, false)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	if user == nil {
		common.ApiErrorMsg(c, "用户不存在")
		return
	}

	if plan.MaxPurchasePerUser > 0 {
		count, err := model.CountUserSubscriptionsByPlan(userId, plan.Id)
		if err != nil {
			common.ApiError(c, err)
			return
		}
		if count >= int64(plan.MaxPurchasePerUser) {
			common.ApiErrorMsg(c, "已达到该套餐购买上限")
			return
		}
	}

	// WAFFO_PANCAKE_SUB- prefix (vs. wallet's WAFFO_PANCAKE-) drives webhook
	// dispatch in WaffoPancakeWebhook.
	tradeNo := fmt.Sprintf("WAFFO_PANCAKE_SUB-%d-%d-%s", userId, time.Now().UnixMilli(), randstr.String(6))

	order := &model.SubscriptionOrder{
		UserId:          userId,
		PlanId:          plan.Id,
		Money:           plan.PriceAmount,
		TradeNo:         tradeNo,
		PaymentMethod:   model.PaymentMethodWaffoPancake,
		PaymentProvider: model.PaymentProviderWaffoPancake,
		CreateTime:      time.Now().Unix(),
		Status:          common.TopUpStatusPending,
	}
	if err := order.Insert(); err != nil {
		logger.LogError(c.Request.Context(), fmt.Sprintf("Waffo Pancake 订阅订单创建失败 user_id=%d plan_id=%d trade_no=%s error=%q", userId, plan.Id, tradeNo, err.Error()))
		c.JSON(http.StatusOK, gin.H{"message": "error", "data": "创建订单失败"})
		return
	}

	expiresInSeconds := 45 * 60
	session, err := service.CreateWaffoPancakeCheckoutSession(c.Request.Context(), &service.WaffoPancakeCreateSessionParams{
		ProductID:     plan.WaffoPancakeProductId,
		BuyerIdentity: service.WaffoPancakeBuyerIdentityFromUserID(user.Id),
		PriceSnapshot: &service.WaffoPancakePriceSnapshot{
			Amount:      decimal.NewFromFloat(plan.PriceAmount).StringFixed(2),
			TaxCategory: "saas",
		},
		BuyerEmail:              getWaffoPancakeBuyerEmail(user),
		ExpiresInSeconds:        &expiresInSeconds,
		OrderMerchantExternalID: tradeNo,
	})
	if err != nil {
		logger.LogError(c.Request.Context(), fmt.Sprintf("Waffo Pancake 订阅结账会话创建失败 user_id=%d plan_id=%d trade_no=%s error=%q", userId, plan.Id, tradeNo, err.Error()))
		order.Status = common.TopUpStatusFailed
		_ = order.Update()
		c.JSON(http.StatusOK, gin.H{"message": "error", "data": "拉起支付失败"})
		return
	}
	logger.LogInfo(c.Request.Context(), fmt.Sprintf("Waffo Pancake 订阅订单创建成功 user_id=%d plan_id=%d trade_no=%s session_id=%s money=%.2f", userId, plan.Id, tradeNo, session.SessionID, plan.PriceAmount))

	c.JSON(http.StatusOK, gin.H{
		"message": "success",
		"data": gin.H{
			"checkout_url":     session.CheckoutURL,
			"session_id":       session.SessionID,
			"expires_at":       session.ExpiresAt,
			"order_id":         tradeNo,
			"token":            session.Token,
			"token_expires_at": session.TokenExpiresAt,
		},
	})
}

func requestVersionedPancakeSubscriptionCheckout(c *gin.Context, req SubscriptionWaffoPancakePayRequest) {
	if strings.TrimSpace(setting.WaffoPancakeMerchantID) == "" || strings.TrimSpace(setting.WaffoPancakePrivateKey) == "" || strings.TrimSpace(setting.WaffoPancakeStoreID) == "" {
		common.ApiErrorMsg(c, "Waffo Pancake 未配置")
		return
	}
	now := common.GetTimestamp()
	order, err := model.CreateVersionedSubscriptionCheckout(model.DB, c.GetInt("id"), req.VersionID, model.PaymentProviderWaffoPancake, req.EventID, now)
	if err != nil {
		creditAPIError(c, err)
		return
	}
	if order.PaymentState != "pending" || order.NeedsReview || now >= order.ExpiresAt || order.Currency != "USD" || order.PriceMicros <= 0 || order.PriceMicros%10000 != 0 {
		creditAPIError(c, model.ErrSubscriptionPurchaseUnavailable)
		return
	}
	var contract model.SubscriptionPlan
	if err := common.UnmarshalJsonStr(order.ContractSnapshot, &contract); err != nil {
		creditAPIError(c, err)
		return
	}
	if strings.TrimSpace(contract.WaffoPancakeProductId) == "" {
		creditAPIError(c, model.ErrSubscriptionPurchaseUnavailable)
		return
	}
	// A native recurring product would let the channel renew independently
	// of our purchased thirty-day terms. Check the existing one-time catalog
	// before issuing cash; a read failure remains retryable.
	if order.CheckoutState == "" {
		catalog, err := service.ListWaffoPancakeCatalog(c.Request.Context(), setting.WaffoPancakeMerchantID, setting.WaffoPancakePrivateKey)
		if err != nil {
			c.JSON(http.StatusBadGateway, gin.H{"success": false, "message": "支付商品查询失败，可重试"})
			return
		}
		available := false
		for _, store := range catalog.Stores {
			if store.ID != setting.WaffoPancakeStoreID || store.Status != "active" || !store.ProdEnabled {
				continue
			}
			for _, product := range store.OnetimeProducts {
				if product.ID == contract.WaffoPancakeProductId {
					available = true
					break
				}
			}
		}
		if !available {
			creditAPIError(c, model.ErrSubscriptionPurchaseUnavailable)
			return
		}
	}
	amount := decimal.NewFromInt(order.PriceMicros).Shift(-6).StringFixed(2)
	body, err := common.Marshal([]string{order.TradeNo, contract.WaffoPancakeProductId, setting.WaffoPancakeStoreID, service.WaffoPancakeBuyerIdentityFromUserID(order.UserID), amount, order.Currency, fmt.Sprint(order.ExpiresAt)})
	if err != nil {
		creditAPIError(c, err)
		return
	}
	digest := sha256.Sum256(body)
	fingerprint := hex.EncodeToString(digest[:])
	order, issue, err := model.BeginSubscriptionCheckout(model.DB, order.UserID, order.ID, fingerprint, common.GetTimestamp())
	if err != nil {
		creditAPIError(c, err)
		return
	}
	if !issue {
		var response struct {
			Data    map[string]any `json:"data"`
			Message string         `json:"message"`
		}
		if err := common.UnmarshalJsonStr(order.CheckoutResponse, &response); err != nil {
			creditAPIError(c, err)
			return
		}
		checkoutURL, ok := response.Data["checkout_url"].(string)
		if !ok || checkoutURL == "" {
			creditAPIError(c, model.ErrCreditInvariant)
			return
		}
		token, err := service.IssueWaffoPancakeBuyerSession(c.Request.Context(), contract.WaffoPancakeProductId, service.WaffoPancakeBuyerIdentityFromUserID(order.UserID))
		if err != nil {
			c.JSON(http.StatusBadGateway, gin.H{"success": false, "message": "买家会话获取失败，可重试"})
			return
		}
		if _, _, err := model.BeginSubscriptionCheckout(model.DB, order.UserID, order.ID, fingerprint, common.GetTimestamp()); err != nil {
			creditAPIError(c, err)
			return
		}
		response.Data["checkout_url"] = checkoutURL + "#token=" + token.Token
		response.Data["token"], response.Data["token_expires_at"] = token.Token, token.ExpiresAt
		c.JSON(http.StatusOK, response)
		return
	}
	expiresInSeconds := int(order.ExpiresAt - common.GetTimestamp())
	session, err := service.CreateWaffoPancakeCheckoutSession(c.Request.Context(), &service.WaffoPancakeCreateSessionParams{
		ProductID: contract.WaffoPancakeProductId, BuyerIdentity: service.WaffoPancakeBuyerIdentityFromUserID(order.UserID),
		PriceSnapshot:    &service.WaffoPancakePriceSnapshot{Amount: amount, TaxCategory: "saas"},
		ExpiresInSeconds: &expiresInSeconds, OrderMerchantExternalID: order.TradeNo,
	})
	if err != nil {
		if err := model.MarkSubscriptionCheckoutUnknown(model.DB, order.UserID, order.ID, fingerprint); err != nil {
			logger.LogError(c.Request.Context(), "Waffo Pancake 套餐结账核查状态保存失败")
		}
		c.JSON(http.StatusBadGateway, gin.H{"success": false, "message": "支付结果待核查"})
		return
	}
	// Keep the durable cash response, not the buyer JWT. A retry can issue
	// fresh buyer authentication without issuing another cash checkout.
	checkoutURL, _, _ := strings.Cut(session.CheckoutURL, "#token=")
	data := gin.H{"checkout_url": checkoutURL, "session_id": session.SessionID, "expires_at": order.ExpiresAt, "order_id": order.ID, "price_amount": amount, "currency": order.Currency}
	response, err := common.Marshal(gin.H{"message": "success", "data": data})
	if err != nil {
		creditAPIError(c, err)
		return
	}
	order, err = model.SaveSubscriptionCheckout(model.DB, order.UserID, order.ID, fingerprint, session.SessionID, string(response))
	if err != nil {
		creditAPIError(c, err)
		return
	}
	if order.NeedsReview {
		creditAPIError(c, model.ErrSubscriptionPurchaseUnavailable)
		return
	}
	data["checkout_url"], data["token"], data["token_expires_at"] = session.CheckoutURL, session.Token, session.TokenExpiresAt
	c.JSON(http.StatusOK, gin.H{"message": "success", "data": data})
}

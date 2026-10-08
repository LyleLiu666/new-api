package controller

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"
	"github.com/thanhpk/randstr"
)

type SubscriptionCreemPayRequest struct {
	PlanId    int    `json:"plan_id"`
	VersionID int64  `json:"version_id"`
	EventID   string `json:"event_id"`
}

func SubscriptionRequestCreemPay(c *gin.Context) {
	if !requirePaymentCompliance(c) {
		return
	}

	var req SubscriptionCreemPayRequest

	// Keep the original body for parsing, without writing it to the audit log.
	bodyBytes, err := io.ReadAll(c.Request.Body)
	if err != nil {
		logger.LogError(c.Request.Context(), fmt.Sprintf("Creem 订阅支付请求读取失败 error=%q", err.Error()))
		c.JSON(http.StatusOK, gin.H{"message": "error", "data": "read query error"})
		return
	}
	c.Request.Body = io.NopCloser(bytes.NewReader(bodyBytes))

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusOK, gin.H{"message": "error", "data": "参数错误"})
		return
	}
	accountingVersion, err := model.GetUserAccountingVersion(model.DB, c.GetInt("id"))
	if err != nil {
		creditAPIError(c, err)
		return
	}
	if accountingVersion == 1 {
		requestVersionedCreemSubscriptionCheckout(c, req)
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
	if plan.CreemProductId == "" {
		common.ApiErrorMsg(c, "该套餐未配置 CreemProductId")
		return
	}
	if setting.CreemWebhookSecret == "" && !setting.CreemTestMode {
		common.ApiErrorMsg(c, "Creem Webhook 未配置")
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

	reference := "sub-creem-ref-" + randstr.String(6)
	referenceId := "sub_ref_" + common.Sha1([]byte(reference+time.Now().String()+user.Username))

	// create pending order first
	order := &model.SubscriptionOrder{
		UserId:          userId,
		PlanId:          plan.Id,
		Money:           plan.PriceAmount,
		TradeNo:         referenceId,
		PaymentMethod:   model.PaymentMethodCreem,
		PaymentProvider: model.PaymentProviderCreem,
		CreateTime:      time.Now().Unix(),
		Status:          common.TopUpStatusPending,
	}
	if err := order.Insert(); err != nil {
		c.JSON(http.StatusOK, gin.H{"message": "error", "data": "创建订单失败"})
		return
	}

	// Reuse Creem checkout generator by building a lightweight product reference.
	currency := "USD"
	switch operation_setting.GetGeneralSetting().QuotaDisplayType {
	case operation_setting.QuotaDisplayTypeCNY:
		currency = "CNY"
	case operation_setting.QuotaDisplayTypeUSD:
		currency = "USD"
	default:
		currency = "USD"
	}
	product := &CreemProduct{
		ProductId: plan.CreemProductId,
		Name:      plan.Title,
		Price:     plan.PriceAmount,
		Currency:  currency,
		Quota:     0,
	}

	checkoutUrl, err := genCreemLink(c.Request.Context(), referenceId, product, user.Email, user.Username)
	if err != nil {
		logger.LogError(c.Request.Context(), fmt.Sprintf("Creem 订阅支付链接创建失败 trade_no=%s product_id=%s error=%q", referenceId, product.ProductId, err.Error()))
		c.JSON(http.StatusOK, gin.H{"message": "error", "data": "拉起支付失败"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"message": "success",
		"data": gin.H{
			"checkout_url": checkoutUrl,
			"order_id":     referenceId,
		},
	})
}

func requestVersionedCreemSubscriptionCheckout(c *gin.Context, req SubscriptionCreemPayRequest) {
	if strings.TrimSpace(setting.CreemApiKey) == "" || !isCreemWebhookConfigured() {
		common.ApiErrorMsg(c, "Creem 未配置")
		return
	}
	now := common.GetTimestamp()
	order, err := model.CreateVersionedSubscriptionCheckout(model.DB, c.GetInt("id"), req.VersionID, model.PaymentProviderCreem, req.EventID, now)
	if err != nil {
		creditAPIError(c, err)
		return
	}
	if order.PaymentState != "pending" || order.NeedsReview || now >= order.ExpiresAt || (order.Currency != "USD" && order.Currency != "CNY") || order.PriceMicros <= 0 || order.PriceMicros%10000 != 0 {
		creditAPIError(c, model.ErrSubscriptionPurchaseUnavailable)
		return
	}
	var contract model.SubscriptionPlan
	if err := common.UnmarshalJsonStr(order.ContractSnapshot, &contract); err != nil {
		creditAPIError(c, err)
		return
	}
	if strings.TrimSpace(contract.CreemProductId) == "" {
		creditAPIError(c, model.ErrSubscriptionPurchaseUnavailable)
		return
	}
	mode := "prod"
	if setting.CreemTestMode {
		mode = "test"
	}
	request := map[string]any{
		"product_id": contract.CreemProductId, "request_id": order.TradeNo,
		"units": 1, "custom_price": order.PriceMicros / 10000,
		"metadata": map[string]string{"subscription_order": order.TradeNo, "user_id": strconv.Itoa(order.UserID)},
	}
	body, err := common.Marshal(request)
	if err != nil {
		creditAPIError(c, err)
		return
	}
	digest := sha256.Sum256(append([]byte(mode+":"), body...))
	fingerprint := hex.EncodeToString(digest[:])
	// Read-only product lookup precedes issuance. A lookup failure has not
	// sent a cash-create request and must remain safely retryable.
	if order.CheckoutState == "" {
		var product struct {
			ID          string `json:"id"`
			Mode        string `json:"mode"`
			Currency    string `json:"currency"`
			BillingType string `json:"billing_type"`
			TaxMode     string `json:"tax_mode"`
			Status      string `json:"status"`
		}
		if err := requestCreemSubscriptionAPI(c.Request.Context(), http.MethodGet, "/products?product_id="+url.QueryEscape(contract.CreemProductId), nil, &product); err != nil {
			c.JSON(http.StatusBadGateway, gin.H{"success": false, "message": "支付商品查询失败，可重试"})
			return
		}
		if product.ID != contract.CreemProductId || product.Mode != mode || strings.ToUpper(product.Currency) != order.Currency || product.BillingType != "onetime" || product.TaxMode != "inclusive" || product.Status != "active" {
			creditAPIError(c, model.ErrSubscriptionPurchaseUnavailable)
			return
		}
	}
	// Contract and issuance state commit before any cash-create call. A
	// request_id tracks Creem checkout; it is not documented as idempotency.
	order, issue, err := model.BeginSubscriptionCheckout(model.DB, order.UserID, order.ID, fingerprint, common.GetTimestamp())
	if err != nil {
		creditAPIError(c, err)
		return
	}
	if !issue {
		c.Data(http.StatusOK, "application/json", []byte(order.CheckoutResponse))
		return
	}
	var checkout CreemCheckoutResponse
	if err := requestCreemSubscriptionAPI(c.Request.Context(), http.MethodPost, "/checkouts", body, &checkout); err != nil || checkout.Id == "" || checkout.CheckoutUrl == "" {
		if err := model.MarkSubscriptionCheckoutUnknown(model.DB, order.UserID, order.ID, fingerprint); err != nil {
			logger.LogError(c.Request.Context(), "Creem 套餐结账核查状态保存失败")
		}
		c.JSON(http.StatusBadGateway, gin.H{"success": false, "message": "支付结果待核查"})
		return
	}
	response, err := common.Marshal(gin.H{"message": "success", "data": gin.H{"checkout_url": checkout.CheckoutUrl, "order_id": order.ID, "expires_at": order.ExpiresAt, "price_amount": decimal.NewFromInt(order.PriceMicros).Shift(-6).String(), "currency": order.Currency}})
	if err != nil {
		creditAPIError(c, err)
		return
	}
	order, err = model.SaveSubscriptionCheckout(model.DB, order.UserID, order.ID, fingerprint, checkout.Id, string(response))
	if err != nil {
		creditAPIError(c, err)
		return
	}
	if order.NeedsReview {
		creditAPIError(c, model.ErrSubscriptionPurchaseUnavailable)
		return
	}
	c.Data(http.StatusOK, "application/json", response)
}

// Both product lookup and checkout creation use the same configured gateway;
// redirects cannot carry its API credential to another host.
func requestCreemSubscriptionAPI(ctx context.Context, method, path string, body []byte, result any) error {
	base := "https://api.creem.io/v1"
	if setting.CreemTestMode {
		base = "https://test-api.creem.io/v1"
	}
	request, err := http.NewRequestWithContext(ctx, method, base+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("x-api-key", setting.CreemApiKey)
	client := &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode/100 != 2 {
		return fmt.Errorf("Creem subscription API status %d", response.StatusCode)
	}
	return common.DecodeJson(io.LimitReader(response.Body, 1024*1024), result)
}

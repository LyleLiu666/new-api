package controller

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Calcium-Ion/go-epay/epay"
	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/gin-gonic/gin"
	"github.com/samber/lo"
	"github.com/shopspring/decimal"
)

type SubscriptionEpayPayRequest struct {
	PlanId        int    `json:"plan_id"`
	VersionID     int64  `json:"version_id"`
	EventID       string `json:"event_id"`
	PaymentMethod string `json:"payment_method"`
}

func SubscriptionRequestEpay(c *gin.Context) {
	if !requirePaymentCompliance(c) {
		return
	}

	var req SubscriptionEpayPayRequest
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
		requestVersionedEpaySubscriptionCheckout(c, req)
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
	if plan.PriceAmount < 0.01 {
		common.ApiErrorMsg(c, "套餐金额过低")
		return
	}
	if !operation_setting.ContainsPayMethod(req.PaymentMethod) {
		common.ApiErrorMsg(c, "支付方式不存在")
		return
	}

	userId := c.GetInt("id")
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

	callBackAddress := service.GetCallbackAddress()
	returnUrl, err := url.Parse(callBackAddress + "/api/subscription/epay/return")
	if err != nil {
		common.ApiErrorMsg(c, "回调地址配置错误")
		return
	}
	notifyUrl, err := url.Parse(callBackAddress + "/api/subscription/epay/notify")
	if err != nil {
		common.ApiErrorMsg(c, "回调地址配置错误")
		return
	}

	tradeNo := fmt.Sprintf("%s%d", common.GetRandomString(6), time.Now().Unix())
	tradeNo = fmt.Sprintf("SUBUSR%dNO%s", userId, tradeNo)

	client := GetEpayClient()
	if client == nil {
		common.ApiErrorMsg(c, "当前管理员未配置支付信息")
		return
	}

	order := &model.SubscriptionOrder{
		UserId:          userId,
		PlanId:          plan.Id,
		Money:           plan.PriceAmount,
		TradeNo:         tradeNo,
		PaymentMethod:   req.PaymentMethod,
		PaymentProvider: model.PaymentProviderEpay,
		CreateTime:      time.Now().Unix(),
		Status:          common.TopUpStatusPending,
	}
	if err := order.Insert(); err != nil {
		common.ApiErrorMsg(c, "创建订单失败")
		return
	}
	uri, params, err := client.Purchase(&epay.PurchaseArgs{
		Type:           req.PaymentMethod,
		ServiceTradeNo: tradeNo,
		Name:           fmt.Sprintf("SUB:%s", plan.Title),
		Money:          strconv.FormatFloat(plan.PriceAmount, 'f', 2, 64),
		Device:         epay.PC,
		NotifyUrl:      notifyUrl,
		ReturnUrl:      returnUrl,
	})
	if err != nil {
		_ = model.ExpireSubscriptionOrder(tradeNo, model.PaymentProviderEpay)
		common.ApiErrorMsg(c, "拉起支付失败")
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "success", "data": params, "url": uri})
}

func SubscriptionEpayNotify(c *gin.Context) {
	var params map[string]string

	if c.Request.Method == "POST" {
		// POST 请求：从 POST body 解析参数
		if err := c.Request.ParseForm(); err != nil {
			_, _ = c.Writer.Write([]byte("fail"))
			return
		}
		params = lo.Reduce(lo.Keys(c.Request.PostForm), func(r map[string]string, t string, i int) map[string]string {
			r[t] = c.Request.PostForm.Get(t)
			return r
		}, map[string]string{})
	} else {
		// GET 请求：从 URL Query 解析参数
		params = lo.Reduce(lo.Keys(c.Request.URL.Query()), func(r map[string]string, t string, i int) map[string]string {
			r[t] = c.Request.URL.Query().Get(t)
			return r
		}, map[string]string{})
	}

	if len(params) == 0 {
		_, _ = c.Writer.Write([]byte("fail"))
		return
	}

	client := GetEpayClient()
	if client == nil {
		_, _ = c.Writer.Write([]byte("fail"))
		return
	}
	verifyInfo, err := client.Verify(params)
	if err != nil || !verifyInfo.VerifyStatus {
		_, _ = c.Writer.Write([]byte("fail"))
		return
	}

	if verifyInfo.TradeStatus != epay.StatusTradeSuccess {
		_, _ = c.Writer.Write([]byte("fail"))
		return
	}
	if strings.HasPrefix(verifyInfo.ServiceTradeNo, "SUB-V1-") {
		if err := recordVersionedEpaySubscriptionPayment(verifyInfo, params["pid"]); err != nil {
			_, _ = c.Writer.Write([]byte("fail"))
			return
		}
		_, _ = c.Writer.Write([]byte("success"))
		return
	}

	LockOrder(verifyInfo.ServiceTradeNo)
	defer UnlockOrder(verifyInfo.ServiceTradeNo)

	if err := model.CompleteSubscriptionOrder(verifyInfo.ServiceTradeNo, common.GetJsonString(verifyInfo), model.PaymentProviderEpay, verifyInfo.Type); err != nil {
		_, _ = c.Writer.Write([]byte("fail"))
		return
	}

	_, _ = c.Writer.Write([]byte("success"))
}

// SubscriptionEpayReturn handles browser return after payment.
// It verifies the payload and completes the order, then redirects to console.
func SubscriptionEpayReturn(c *gin.Context) {
	var params map[string]string

	if c.Request.Method == "POST" {
		// POST 请求：从 POST body 解析参数
		if err := c.Request.ParseForm(); err != nil {
			c.Redirect(http.StatusFound, paymentReturnPath("/wallet?pay=fail"))
			return
		}
		params = lo.Reduce(lo.Keys(c.Request.PostForm), func(r map[string]string, t string, i int) map[string]string {
			r[t] = c.Request.PostForm.Get(t)
			return r
		}, map[string]string{})
	} else {
		// GET 请求：从 URL Query 解析参数
		params = lo.Reduce(lo.Keys(c.Request.URL.Query()), func(r map[string]string, t string, i int) map[string]string {
			r[t] = c.Request.URL.Query().Get(t)
			return r
		}, map[string]string{})
	}

	if len(params) == 0 {
		c.Redirect(http.StatusFound, paymentReturnPath("/wallet?pay=fail"))
		return
	}

	client := GetEpayClient()
	if client == nil {
		c.Redirect(http.StatusFound, paymentReturnPath("/wallet?pay=fail"))
		return
	}
	verifyInfo, err := client.Verify(params)
	if err != nil || !verifyInfo.VerifyStatus {
		c.Redirect(http.StatusFound, paymentReturnPath("/wallet?pay=fail"))
		return
	}
	if verifyInfo.TradeStatus == epay.StatusTradeSuccess {
		if strings.HasPrefix(verifyInfo.ServiceTradeNo, "SUB-V1-") {
			if err := recordVersionedEpaySubscriptionPayment(verifyInfo, params["pid"]); err != nil {
				c.Redirect(http.StatusFound, paymentReturnPath("/wallet?pay=fail"))
				return
			}
			c.Redirect(http.StatusFound, paymentReturnPath("/wallet?pay=pending"))
			return
		}
		LockOrder(verifyInfo.ServiceTradeNo)
		defer UnlockOrder(verifyInfo.ServiceTradeNo)
		if err := model.CompleteSubscriptionOrder(verifyInfo.ServiceTradeNo, common.GetJsonString(verifyInfo), model.PaymentProviderEpay, verifyInfo.Type); err != nil {
			c.Redirect(http.StatusFound, paymentReturnPath("/wallet?pay=fail"))
			return
		}
		c.Redirect(http.StatusFound, paymentReturnPath("/wallet?pay=success"))
		return
	}
	c.Redirect(http.StatusFound, paymentReturnPath("/wallet?pay=pending"))
}

func requestVersionedEpaySubscriptionCheckout(c *gin.Context, req SubscriptionEpayPayRequest) {
	client := GetEpayClient()
	if client == nil || !operation_setting.ContainsPayMethod(req.PaymentMethod) {
		common.ApiErrorMsg(c, "支付方式未配置")
		return
	}
	now := common.GetTimestamp()
	order, err := model.CreateVersionedSubscriptionCheckout(model.DB, c.GetInt("id"), req.VersionID, model.PaymentProviderEpay, req.EventID, now)
	if err != nil {
		creditAPIError(c, err)
		return
	}
	// The existing Epay protocol denominates money in CNY and has no FX
	// contract. A USD product cannot silently become the same number of yuan.
	if order.PaymentState != "pending" || order.NeedsReview || now >= order.ExpiresAt || order.Currency != "CNY" || order.PriceMicros < 10000 || order.PriceMicros%10000 != 0 {
		creditAPIError(c, model.ErrSubscriptionPurchaseUnavailable)
		return
	}
	var contract model.SubscriptionPlan
	if err := common.UnmarshalJsonStr(order.ContractSnapshot, &contract); err != nil {
		creditAPIError(c, err)
		return
	}
	callback := service.GetCallbackAddress()
	notify, err := url.Parse(callback + "/api/subscription/epay/notify")
	if err != nil {
		creditAPIError(c, err)
		return
	}
	returnURL, err := url.Parse(callback + "/api/subscription/epay/return")
	if err != nil {
		creditAPIError(c, err)
		return
	}
	uri, params, err := client.Purchase(&epay.PurchaseArgs{Type: req.PaymentMethod, ServiceTradeNo: order.TradeNo, Name: "SUB:" + contract.Title, Money: decimal.NewFromInt(order.PriceMicros).Shift(-6).StringFixed(2), Device: epay.PC, NotifyUrl: notify, ReturnUrl: returnURL})
	if err != nil {
		creditAPIError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "success", "data": params, "url": uri, "order_id": order.ID, "expires_at": order.ExpiresAt})
}

// Epay's signed merchant trade number binds the payment to its authenticated
// purchaser's local order. Its notification does not provide a paid-at fact.
func recordVersionedEpaySubscriptionPayment(payment *epay.VerifyRes, merchantID string) error {
	var order model.SubscriptionPurchaseOrder
	if err := model.DB.Where("trade_no = ? AND provider = ?", payment.ServiceTradeNo, model.PaymentProviderEpay).First(&order).Error; err != nil {
		return err
	}
	input := model.VerifiedSubscriptionPayment{OrderID: order.ID, Provider: model.PaymentProviderEpay, EventID: "epay:" + payment.TradeNo + ":" + payment.TradeStatus, ReferenceID: payment.TradeNo, BuyerID: order.UserID, Currency: "CNY", Succeeded: payment.VerifyStatus && payment.TradeStatus == epay.StatusTradeSuccess && merchantID == operation_setting.EpayId}
	input.AmountMicros = subscriptionPaymentMicros(payment.Money)
	evidence, err := common.Marshal(struct {
		Payment    epay.VerifyRes
		MerchantID string
	}{*payment, merchantID})
	if err != nil {
		return err
	}
	digest := sha256.Sum256(evidence)
	input.EvidenceDigest = hex.EncodeToString(digest[:])
	return completeVersionedSubscriptionPayment(input, common.GetTimestamp())
}

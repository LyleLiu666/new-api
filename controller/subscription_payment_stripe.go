package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting"
	"github.com/gin-gonic/gin"
	"github.com/stripe/stripe-go/v81"
	"github.com/stripe/stripe-go/v81/checkout/session"
	"github.com/stripe/stripe-go/v81/invoice"
	"github.com/thanhpk/randstr"
)

// Only signed gateway events reach this host operation. Event creation and
// receipt timestamps cannot establish the start of a paid subscription.
func completeVersionedStripeSubscription(ctx context.Context, event stripe.Event) error {
	var order model.SubscriptionPurchaseOrder
	if err := model.DB.Where("trade_no = ? AND provider = ?", event.GetObjectValue("client_reference_id"), model.PaymentProviderStripe).First(&order).Error; err != nil {
		return err
	}
	client := session.Client{B: stripe.GetBackend(stripe.APIBackend), Key: setting.StripeApiSecret}
	checkout, err := client.Get(event.GetObjectValue("id"), &stripe.CheckoutSessionParams{Params: stripe.Params{Context: ctx}})
	if err != nil {
		return err
	}
	// A completed session can precede confirmation of a delayed payment.
	if checkout.PaymentStatus != stripe.CheckoutSessionPaymentStatusPaid {
		return nil
	}
	buyerID, _ := strconv.Atoi(checkout.Metadata["user_id"])
	input := model.VerifiedSubscriptionPayment{OrderID: order.ID, Provider: model.PaymentProviderStripe, EventID: event.ID, BuyerID: buyerID, Currency: strings.ToUpper(string(checkout.Currency))}
	input.Succeeded = checkout.ID == event.GetObjectValue("id") && checkout.ClientReferenceID == order.TradeNo && checkout.Metadata["subscription_order"] == order.TradeNo && checkout.Mode == stripe.CheckoutSessionModePayment && checkout.Status == stripe.CheckoutSessionStatusComplete
	if checkout.Invoice != nil && checkout.Invoice.ID != "" {
		invoiceClient := invoice.Client{B: stripe.GetBackend(stripe.APIBackend), Key: setting.StripeApiSecret}
		paidInvoice, err := invoiceClient.Get(checkout.Invoice.ID, &stripe.InvoiceParams{Params: stripe.Params{Context: ctx}})
		if err != nil {
			return err
		}
		input.ReferenceID = paidInvoice.ID
		input.Succeeded = input.Succeeded && paidInvoice.ID == checkout.Invoice.ID && paidInvoice.Status == stripe.InvoiceStatusPaid && paidInvoice.Metadata["subscription_order"] == order.TradeNo && paidInvoice.Currency == checkout.Currency && paidInvoice.AmountPaid == checkout.AmountTotal && checkout.Customer != nil && paidInvoice.Customer != nil && paidInvoice.Customer.ID == checkout.Customer.ID
		// These supported currencies have two decimal minor units. Do not
		// assume the same exponent for an arbitrary Stripe currency.
		if (input.Currency == "USD" || input.Currency == "CNY") && paidInvoice.AmountPaid >= 0 && paidInvoice.AmountPaid <= common.MaxWalletQuota/10000 {
			input.AmountMicros = common.GetPointer(paidInvoice.AmountPaid * 10000)
		}
		if paidInvoice.StatusTransitions != nil && paidInvoice.StatusTransitions.PaidAt > 0 {
			input.PaidAt = common.GetPointer(paidInvoice.StatusTransitions.PaidAt)
			input.PaidAtSource = "stripe.invoice.status_transitions.paid_at"
		}
	}
	evidence, err := common.Marshal(struct {
		EventID, SessionID string
		Payment            model.VerifiedSubscriptionPayment
	}{event.ID, checkout.ID, input})
	if err != nil {
		return err
	}
	digest := sha256.Sum256(evidence)
	input.EvidenceDigest = hex.EncodeToString(digest[:])
	return completeVersionedSubscriptionPayment(input, common.GetTimestamp())
}

type SubscriptionStripePayRequest struct {
	PlanId    int    `json:"plan_id"`
	VersionID int64  `json:"version_id"`
	EventID   string `json:"event_id"`
}

func SubscriptionRequestStripePay(c *gin.Context) {
	if !requirePaymentCompliance(c) {
		return
	}

	var req SubscriptionStripePayRequest
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
		requestVersionedStripeSubscriptionCheckout(c, req)
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
	if plan.StripePriceId == "" {
		common.ApiErrorMsg(c, "该套餐未配置 StripePriceId")
		return
	}
	if !strings.HasPrefix(setting.StripeApiSecret, "sk_") && !strings.HasPrefix(setting.StripeApiSecret, "rk_") {
		common.ApiErrorMsg(c, "Stripe 未配置或密钥无效")
		return
	}
	if setting.StripeWebhookSecret == "" {
		common.ApiErrorMsg(c, "Stripe Webhook 未配置")
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

	reference := fmt.Sprintf("sub-stripe-ref-%d-%d-%s", user.Id, time.Now().UnixMilli(), randstr.String(4))
	referenceId := "sub_ref_" + common.Sha1([]byte(reference))

	payLink, err := genStripeSubscriptionLink(referenceId, user.StripeCustomer, user.Email, plan.StripePriceId)
	if err != nil {
		logger.LogError(c.Request.Context(), fmt.Sprintf("Stripe 订阅支付链接创建失败 trade_no=%s plan_id=%d error=%q", referenceId, plan.Id, err.Error()))
		c.JSON(http.StatusOK, gin.H{"message": "error", "data": "拉起支付失败"})
		return
	}

	order := &model.SubscriptionOrder{
		UserId:          userId,
		PlanId:          plan.Id,
		Money:           plan.PriceAmount,
		TradeNo:         referenceId,
		PaymentMethod:   model.PaymentMethodStripe,
		PaymentProvider: model.PaymentProviderStripe,
		CreateTime:      time.Now().Unix(),
		Status:          common.TopUpStatusPending,
	}
	if err := order.Insert(); err != nil {
		c.JSON(http.StatusOK, gin.H{"message": "error", "data": "创建订单失败"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"message": "success",
		"data": gin.H{
			"pay_link": payLink,
		},
	})
}

func requestVersionedStripeSubscriptionCheckout(c *gin.Context, req SubscriptionStripePayRequest) {
	if strings.TrimSpace(setting.StripeApiSecret) == "" || !isStripeWebhookConfigured() {
		common.ApiErrorMsg(c, "Stripe 未配置")
		return
	}
	now := common.GetTimestamp()
	order, err := model.CreateVersionedSubscriptionCheckout(model.DB, c.GetInt("id"), req.VersionID, model.PaymentProviderStripe, req.EventID, now)
	if err != nil {
		creditAPIError(c, err)
		return
	}
	if order.PaymentState != "pending" || order.NeedsReview || now >= order.ExpiresAt {
		creditAPIError(c, model.ErrSubscriptionPurchaseUnavailable)
		return
	}
	// Checkout must charge precisely the contract price, without silently
	// rounding a sub-cent price or using the mutable catalog's Stripe SKU.
	if (order.Currency != "USD" && order.Currency != "CNY") || order.PriceMicros <= 0 || order.PriceMicros%10000 != 0 {
		creditAPIError(c, model.ErrSubscriptionPurchaseUnavailable)
		return
	}
	var contract model.SubscriptionPlan
	if err := common.UnmarshalJsonStr(order.ContractSnapshot, &contract); err != nil {
		creditAPIError(c, err)
		return
	}
	metadata := map[string]string{"subscription_order": order.TradeNo, "user_id": strconv.Itoa(order.UserID)}
	params := &stripe.CheckoutSessionParams{
		Params:            stripe.Params{Context: c.Request.Context()},
		ClientReferenceID: stripe.String(order.TradeNo),
		SuccessURL:        stripe.String(paymentReturnPath("/wallet")),
		CancelURL:         stripe.String(paymentReturnPath("/wallet")),
		Mode:              stripe.String(string(stripe.CheckoutSessionModePayment)),
		CustomerCreation:  stripe.String(string(stripe.CheckoutSessionCustomerCreationAlways)),
		ExpiresAt:         stripe.Int64(order.ExpiresAt),
		Metadata:          metadata,
		InvoiceCreation:   &stripe.CheckoutSessionInvoiceCreationParams{Enabled: stripe.Bool(true), InvoiceData: &stripe.CheckoutSessionInvoiceCreationInvoiceDataParams{Metadata: metadata}},
		LineItems:         []*stripe.CheckoutSessionLineItemParams{{Quantity: stripe.Int64(1), PriceData: &stripe.CheckoutSessionLineItemPriceDataParams{Currency: stripe.String(strings.ToLower(order.Currency)), UnitAmount: stripe.Int64(order.PriceMicros / 10000), ProductData: &stripe.CheckoutSessionLineItemPriceDataProductDataParams{Name: stripe.String(contract.Title)}}}},
	}
	params.SetIdempotencyKey(order.TradeNo)
	client := session.Client{B: stripe.GetBackend(stripe.APIBackend), Key: setting.StripeApiSecret}
	checkout, err := client.New(params)
	if err != nil {
		logger.LogError(c.Request.Context(), "Stripe 套餐支付链接创建失败，保留订单等待重试")
		c.JSON(http.StatusBadGateway, gin.H{"success": false, "message": "拉起支付失败"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "success", "data": gin.H{"pay_link": checkout.URL, "order_id": order.ID, "expires_at": order.ExpiresAt}})
}

func genStripeSubscriptionLink(referenceId string, customerId string, email string, priceId string) (string, error) {
	stripe.Key = setting.StripeApiSecret

	params := &stripe.CheckoutSessionParams{
		ClientReferenceID: stripe.String(referenceId),
		SuccessURL:        stripe.String(paymentReturnPath("/wallet")),
		CancelURL:         stripe.String(paymentReturnPath("/wallet")),
		LineItems: []*stripe.CheckoutSessionLineItemParams{
			{
				Price:    stripe.String(priceId),
				Quantity: stripe.Int64(1),
			},
		},
		Mode: stripe.String(string(stripe.CheckoutSessionModeSubscription)),
	}

	if "" == customerId {
		if "" != email {
			params.CustomerEmail = stripe.String(email)
		}
		params.CustomerCreation = stripe.String(string(stripe.CheckoutSessionCustomerCreationAlways))
	} else {
		params.Customer = stripe.String(customerId)
	}

	result, err := session.New(params)
	if err != nil {
		return "", err
	}
	return result.URL, nil
}

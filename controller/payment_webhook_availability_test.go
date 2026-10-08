package controller

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/stripe/stripe-go/v81/webhook"
	waffoutils "github.com/waffo-com/waffo-go/utils"
)

func TestPaymentWebhookAuditExcludesCredentialsAndPayload(t *testing.T) {
	confirmPaymentComplianceForTest(t)
	db := modelManagementDB(t, "sqlite", "")
	require.NoError(t, db.AutoMigrate(&model.SubscriptionOrder{}, &model.TopUp{}))
	var audit bytes.Buffer
	common.LogWriterMu.Lock()
	originalWriter, originalErrorWriter := gin.DefaultWriter, gin.DefaultErrorWriter
	gin.DefaultWriter, gin.DefaultErrorWriter = &audit, &audit
	common.LogWriterMu.Unlock()
	t.Cleanup(func() {
		common.LogWriterMu.Lock()
		gin.DefaultWriter, gin.DefaultErrorWriter = originalWriter, originalErrorWriter
		common.LogWriterMu.Unlock()
	})
	originalCreemKey, originalProducts, originalSecret, originalTestMode := setting.CreemApiKey, setting.CreemProducts, setting.CreemWebhookSecret, setting.CreemTestMode
	originalMerchant, originalPrivate, originalProduct := setting.WaffoPancakeMerchantID, setting.WaffoPancakePrivateKey, setting.WaffoPancakeProductID
	originalStripeKey, originalStripeSecret, originalStripePrice := setting.StripeApiSecret, setting.StripeWebhookSecret, setting.StripePriceId
	originalWaffoEnabled, originalWaffoSandbox := setting.WaffoEnabled, setting.WaffoSandbox
	originalWaffoKey, originalWaffoPrivate, originalWaffoPublic := setting.WaffoApiKey, setting.WaffoPrivateKey, setting.WaffoPublicCert
	t.Cleanup(func() {
		setting.CreemApiKey, setting.CreemProducts, setting.CreemWebhookSecret, setting.CreemTestMode = originalCreemKey, originalProducts, originalSecret, originalTestMode
		setting.WaffoPancakeMerchantID, setting.WaffoPancakePrivateKey, setting.WaffoPancakeProductID = originalMerchant, originalPrivate, originalProduct
		setting.StripeApiSecret, setting.StripeWebhookSecret, setting.StripePriceId = originalStripeKey, originalStripeSecret, originalStripePrice
		setting.WaffoEnabled, setting.WaffoSandbox = originalWaffoEnabled, originalWaffoSandbox
		setting.WaffoApiKey, setting.WaffoPrivateKey, setting.WaffoPublicCert = originalWaffoKey, originalWaffoPrivate, originalWaffoPublic
	})
	setting.CreemApiKey, setting.CreemProducts, setting.CreemWebhookSecret, setting.CreemTestMode = "test-key", `[{"productId":"test-product"}]`, "test-signing-secret", false
	setting.WaffoPancakeMerchantID, setting.WaffoPancakePrivateKey, setting.WaffoPancakeProductID = "test-merchant", "test-private", "test-product"
	setting.StripeApiSecret, setting.StripeWebhookSecret, setting.StripePriceId = "test-api-key", "test-webhook-secret", "test-price"
	setting.WaffoEnabled, setting.WaffoSandbox = true, false
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	privateDER, err := x509.MarshalPKCS8PrivateKey(privateKey)
	require.NoError(t, err)
	publicDER, err := x509.MarshalPKIXPublicKey(&privateKey.PublicKey)
	require.NoError(t, err)
	setting.WaffoApiKey, setting.WaffoPrivateKey, setting.WaffoPublicCert = "test-key", base64.StdEncoding.EncodeToString(privateDER), base64.StdEncoding.EncodeToString(publicDER)
	engine := gin.New()
	engine.POST("/creem", CreemWebhook)
	engine.POST("/pancake/:env", WaffoPancakeWebhook)
	engine.POST("/stripe", StripeWebhook)
	engine.POST("/waffo", WaffoWebhook)
	const privatePayload = `{"eventType":"unhandled.test","object":{"customer":{"email":"private-customer@example.invalid"}},"metadata":{"token":"private-payload-token"}}`
	const invalidPayload = `{"created_at":"private-payload-token","email":"private-customer@example.invalid"}`
	const stripePayload = `{"id":"event-test","object":"event","type":"unhandled.test","data":{"object":{"email":"private-customer@example.invalid","token":"private-payload-token"}}}`
	const creemPaidPayload = `{"id":"paid-event-test","eventType":"checkout.completed","object":{"request_id":"audit-missing-order","order":{"id":"audit-gateway-order","status":"paid","type":"onetime","amount_paid":100,"currency":"USD"},"customer":{"email":"private-customer@example.invalid","name":"private-customer-name"}}}`
	signedStripe := webhook.GenerateTestSignedPayload(&webhook.UnsignedPayload{Payload: []byte(stripePayload), Secret: setting.StripeWebhookSecret})
	signedWaffo, err := waffoutils.Sign(privatePayload, setting.WaffoPrivateKey)
	require.NoError(t, err)
	for _, tc := range []struct {
		name, path, body, header, signature string
		status                              int
	}{
		{"missing signature", "/creem", privatePayload, CreemSignatureHeader, "", http.StatusUnauthorized},
		{"invalid signature", "/creem", privatePayload, CreemSignatureHeader, "usable-signature-token", http.StatusUnauthorized},
		{"verified ignored event", "/creem", privatePayload, CreemSignatureHeader, generateCreemSignature(privatePayload, setting.CreemWebhookSecret), http.StatusOK},
		{"verified invalid payload", "/creem", invalidPayload, CreemSignatureHeader, generateCreemSignature(invalidPayload, setting.CreemWebhookSecret), http.StatusBadRequest},
		{"verified payment contains private customer", "/creem", creemPaidPayload, CreemSignatureHeader, generateCreemSignature(creemPaidPayload, setting.CreemWebhookSecret), http.StatusBadRequest},
		{"pancake invalid signature", "/pancake/test", privatePayload, "X-Waffo-Signature", "usable-signature-token", http.StatusUnauthorized},
		{"pancake invalid environment", "/pancake/unknown", privatePayload, "X-Waffo-Signature", "usable-signature-token", http.StatusNotFound},
		{"stripe invalid signature", "/stripe", stripePayload, "Stripe-Signature", "usable-signature-token", http.StatusBadRequest},
		{"stripe verified ignored event", "/stripe", stripePayload, "Stripe-Signature", signedStripe.Header, http.StatusOK},
		{"waffo invalid signature", "/waffo", privatePayload, "X-SIGNATURE", "usable-signature-token", http.StatusBadRequest},
		{"waffo verified ignored event", "/waffo", privatePayload, "X-SIGNATURE", signedWaffo, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			audit.Reset()
			response := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, tc.path+"?token=private-query-token", strings.NewReader(tc.body))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set(tc.header, tc.signature)
			engine.ServeHTTP(response, request)
			assert.Equal(t, tc.status, response.Code)
			assert.Contains(t, audit.String(), "webhook")
			for _, secret := range []string{"private-customer@example.invalid", "private-customer-name", "private-payload-token", "private-query-token", "usable-signature-token"} {
				assert.NotContains(t, audit.String(), secret)
			}
			if tc.signature != "" {
				assert.NotContains(t, audit.String(), tc.signature)
			}
		})
	}
	for _, testMode := range []bool{false, true} {
		audit.Reset()
		setting.CreemTestMode = testMode
		assert.Equal(t, testMode, verifyCreemSignature(privatePayload, "usable-signature-token", ""))
		assert.NotContains(t, audit.String(), "usable-signature-token")
		assert.NotContains(t, audit.String(), "private-payload-token")
		assert.NotContains(t, audit.String(), "private-customer@example.invalid")
	}
}

func confirmPaymentComplianceForTest(t *testing.T) {
	t.Helper()
	paymentSetting := operation_setting.GetPaymentSetting()
	originalConfirmed := paymentSetting.ComplianceConfirmed
	originalTermsVersion := paymentSetting.ComplianceTermsVersion
	t.Cleanup(func() {
		paymentSetting.ComplianceConfirmed = originalConfirmed
		paymentSetting.ComplianceTermsVersion = originalTermsVersion
	})
	paymentSetting.ComplianceConfirmed = true
	paymentSetting.ComplianceTermsVersion = operation_setting.CurrentComplianceTermsVersion
}

func TestStripeWebhookEnabledRequiresTopUpAndWebhookConfig(t *testing.T) {
	confirmPaymentComplianceForTest(t)
	originalAPISecret := setting.StripeApiSecret
	originalWebhookSecret := setting.StripeWebhookSecret
	originalPriceID := setting.StripePriceId
	t.Cleanup(func() {
		setting.StripeApiSecret = originalAPISecret
		setting.StripeWebhookSecret = originalWebhookSecret
		setting.StripePriceId = originalPriceID
	})

	setting.StripeWebhookSecret = ""
	setting.StripeApiSecret = "sk_test_123"
	setting.StripePriceId = "price_123"
	require.False(t, isStripeWebhookEnabled())

	setting.StripeWebhookSecret = "whsec_test"
	require.True(t, isStripeWebhookEnabled())

	setting.StripePriceId = ""
	require.False(t, isStripeWebhookEnabled())
}

func TestCreemWebhookEnabledRequiresTopUpAndWebhookConfig(t *testing.T) {
	confirmPaymentComplianceForTest(t)
	originalAPIKey := setting.CreemApiKey
	originalProducts := setting.CreemProducts
	originalWebhookSecret := setting.CreemWebhookSecret
	t.Cleanup(func() {
		setting.CreemApiKey = originalAPIKey
		setting.CreemProducts = originalProducts
		setting.CreemWebhookSecret = originalWebhookSecret
	})

	setting.CreemWebhookSecret = ""
	setting.CreemApiKey = "creem_api_key"
	setting.CreemProducts = `[{"productId":"prod_123"}]`
	require.False(t, isCreemWebhookEnabled())

	setting.CreemWebhookSecret = "creem_secret"
	require.True(t, isCreemWebhookEnabled())

	setting.CreemProducts = "[]"
	require.False(t, isCreemWebhookEnabled())
}

func TestWaffoWebhookEnabledRequiresTopUpAndWebhookConfig(t *testing.T) {
	confirmPaymentComplianceForTest(t)
	originalEnabled := setting.WaffoEnabled
	originalSandbox := setting.WaffoSandbox
	originalAPIKey := setting.WaffoApiKey
	originalPrivateKey := setting.WaffoPrivateKey
	originalPublicCert := setting.WaffoPublicCert
	originalSandboxAPIKey := setting.WaffoSandboxApiKey
	originalSandboxPrivateKey := setting.WaffoSandboxPrivateKey
	originalSandboxPublicCert := setting.WaffoSandboxPublicCert
	t.Cleanup(func() {
		setting.WaffoEnabled = originalEnabled
		setting.WaffoSandbox = originalSandbox
		setting.WaffoApiKey = originalAPIKey
		setting.WaffoPrivateKey = originalPrivateKey
		setting.WaffoPublicCert = originalPublicCert
		setting.WaffoSandboxApiKey = originalSandboxAPIKey
		setting.WaffoSandboxPrivateKey = originalSandboxPrivateKey
		setting.WaffoSandboxPublicCert = originalSandboxPublicCert
	})

	setting.WaffoEnabled = true
	setting.WaffoSandbox = false
	setting.WaffoApiKey = ""
	setting.WaffoPrivateKey = "private"
	setting.WaffoPublicCert = "public"
	require.False(t, isWaffoWebhookEnabled())

	setting.WaffoApiKey = "api"
	require.True(t, isWaffoWebhookEnabled())

	setting.WaffoEnabled = false
	require.False(t, isWaffoWebhookEnabled())

	setting.WaffoEnabled = true
	setting.WaffoSandbox = true
	setting.WaffoSandboxApiKey = ""
	setting.WaffoSandboxPrivateKey = "sandbox_private"
	setting.WaffoSandboxPublicCert = "sandbox_public"
	require.False(t, isWaffoWebhookEnabled())

	setting.WaffoSandboxApiKey = "sandbox_api"
	require.True(t, isWaffoWebhookEnabled())
}

func TestWaffoPancakeWebhookEnabledRequiresTopUpAndWebhookConfig(t *testing.T) {
	confirmPaymentComplianceForTest(t)
	originalMerchantID := setting.WaffoPancakeMerchantID
	originalPrivateKey := setting.WaffoPancakePrivateKey
	originalProductID := setting.WaffoPancakeProductID
	t.Cleanup(func() {
		setting.WaffoPancakeMerchantID = originalMerchantID
		setting.WaffoPancakePrivateKey = originalPrivateKey
		setting.WaffoPancakeProductID = originalProductID
	})

	// Presence of all three credentials enables the gateway. Webhook public
	// keys are bundled in the SDK and there is no separate Enabled toggle —
	// clear any of the three fields to disable.
	setting.WaffoPancakeMerchantID = ""
	setting.WaffoPancakePrivateKey = "private"
	setting.WaffoPancakeProductID = "product"
	require.False(t, isWaffoPancakeWebhookEnabled())

	setting.WaffoPancakeMerchantID = "merchant"
	require.True(t, isWaffoPancakeWebhookEnabled())

	setting.WaffoPancakeProductID = ""
	require.False(t, isWaffoPancakeWebhookEnabled())

	setting.WaffoPancakeProductID = "product"
	setting.WaffoPancakePrivateKey = ""
	require.False(t, isWaffoPancakeWebhookEnabled())
}

func TestEpayWebhookEnabledRequiresTopUpAndWebhookConfig(t *testing.T) {
	confirmPaymentComplianceForTest(t)
	originalPayAddress := operation_setting.PayAddress
	originalEpayID := operation_setting.EpayId
	originalEpayKey := operation_setting.EpayKey
	originalPayMethods := operation_setting.PayMethods
	t.Cleanup(func() {
		operation_setting.PayAddress = originalPayAddress
		operation_setting.EpayId = originalEpayID
		operation_setting.EpayKey = originalEpayKey
		operation_setting.PayMethods = originalPayMethods
	})

	operation_setting.PayAddress = "https://pay.example.com"
	operation_setting.EpayId = "epay_id"
	operation_setting.EpayKey = ""
	operation_setting.PayMethods = []map[string]string{{"type": "alipay"}}
	require.False(t, isEpayWebhookEnabled())

	operation_setting.EpayKey = "epay_key"
	require.True(t, isEpayWebhookEnabled())

	operation_setting.PayMethods = nil
	require.False(t, isEpayWebhookEnabled())
}

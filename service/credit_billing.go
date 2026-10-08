package service

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	hosttypes "github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
)

const BillingSourceCreditPacks = model.CreditFundingSource

type creditBilling struct {
	quotaPerUnit          float64
	toolPrices            map[string]float64
	geminiInputAudioPrice float64
	request               model.CreditRequest
	review                bool
	zeroChargeEstablished bool
	execution             model.CreditExecution
	stopHeartbeat         context.CancelFunc
	abortRequest          context.CancelFunc
	relayAttempt          int
	relayPriceID          int64
	relaySequence         int64
	relayPhases           map[string]bool
	relayStatus           *int
	relayClosed           bool
	relayFault            *types.NewAPIError
}

func newCreditBillingSession(c *gin.Context, info *relaycommon.RelayInfo, amount, channelType int) (*BillingSession, *types.NewAPIError) {
	// HTTP handlers mark submission after conversion and before calling the
	// adaptor, including providers that use an SDK rather than shared HTTP.
	admitted := false
	switch info.RelayFormat {
	case types.RelayFormatOpenAI, types.RelayFormatOpenAIResponses, types.RelayFormatClaude, types.RelayFormatGemini,
		types.RelayFormatOpenAIImage, types.RelayFormatOpenAIAudio, types.RelayFormatEmbedding, types.RelayFormatRerank,
		types.RelayFormatOpenAIResponsesCompaction, types.RelayFormatOpenAIAlphaSearch:
		admitted = true
	case types.RelayFormatTask:
		admitted = info.TaskRelayInfo != nil && info.PublicTaskID != ""
	case types.RelayFormatMjProxy:
		admitted = true
	case types.RelayFormatOpenAIRealtime:
		admitted = channelType == constant.ChannelTypeOpenAI
	}
	if !admitted {
		return nil, types.NewErrorWithStatusCode(model.ErrCreditOperationRequired, types.ErrorCodeInvalidRequest, http.StatusBadRequest, types.ErrOptionWithSkipRetry())
	}
	pref := common.NormalizeBillingPreference(info.UserSetting.BillingPreference)
	if info.RequestId == "" {
		info.RequestId = common.NewRequestId()
	}
	// Serialize explicit multipliers too: PriceData keeps its map private.
	quotaPerUnit := common.QuotaPerUnit
	if quotaPerUnit <= 0 {
		return nil, creditBillingError(model.ErrCreditInvalid)
	}
	toolPrices := operation_setting.SnapshotToolPricesForModel(info.GetBillingModelName())
	geminiInputAudioPrice := operation_setting.GetGeminiInputAudioPricePerMillionTokens(info.GetBillingModelName())
	snapshot, err := common.Marshal(map[string]any{"price": info.PriceData, "other_ratios": info.PriceData.OtherRatios(), "expression": info.TieredBillingSnapshot, "quota_per_unit": quotaPerUnit, "tool_prices": toolPrices, "gemini_input_audio_price": geminiInputAudioPrice, "prompt_estimation": info.CreditPromptEstimation})
	if err != nil {
		return nil, creditBillingError(err)
	}
	tokenID := info.TokenId
	if info.IsPlayground {
		tokenID = 0
	}
	request, err := model.BeginCreditRequest(model.DB, model.CreditRequestInput{BillingPreference: pref, ChannelID: common.GetContextKeyInt(c, constant.ContextKeyChannelId), Group: info.UsingGroup, UserID: info.UserId, RequestID: info.RequestId, ModelName: info.GetBillingModelName(), Protocol: string(info.RelayFormat), PriceSnapshot: string(snapshot), TokenID: tokenID, Playground: info.IsPlayground, Free: info.PriceData.FreeModel, Amount: int64(amount)}, common.GetTimestamp())
	if err != nil {
		return nil, creditBillingError(err)
	}
	if request.State != "reserved" && request.State != "executing" {
		return nil, creditBillingError(model.ErrCreditOperationConflict)
	}
	if request.Reserved < 0 || request.Reserved > common.MaxQuota {
		return nil, creditBillingError(model.ErrCreditInvariant)
	}
	amount = int(request.Reserved)
	lease, err := model.ClaimCreditExecution(model.DB, info.UserId, request.ID, common.NewRequestId(), 120, common.GetTimestamp(), common.GetTimestamp)
	if err != nil {
		return nil, creditBillingError(err)
	}
	session := &BillingSession{relayInfo: info, preConsumedQuota: amount, tokenConsumed: amount, credit: &creditBilling{request: request, execution: lease, quotaPerUnit: quotaPerUnit, toolPrices: toolPrices, geminiInputAudioPrice: geminiInputAudioPrice}}
	info.FinalPreConsumedQuota, info.BillingSource = amount, BillingSourceCreditPacks
	info.SubscriptionId = request.SubscriptionID
	c.Set("relay_billing_session", session)
	startCreditHeartbeat(c, session)
	if c != nil && c.Writer != nil {
		c.Writer = &creditRelayWriter{ResponseWriter: c.Writer, session: session}
	}
	return session, nil
}

func billingQuotaPerUnit(info *relaycommon.RelayInfo) float64 {
	if session, ok := info.Billing.(*BillingSession); ok && session.credit != nil {
		return session.credit.quotaPerUnit
	}
	return common.QuotaPerUnit
}

// CreditBillingRequestID exposes only the durable host reference used when
// persisting a task; task plugins do not control the request identity.
func CreditBillingRequestID(info *relaycommon.RelayInfo) int64 {
	session, ok := info.Billing.(*BillingSession)
	if !ok || session.credit == nil {
		return 0
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	return session.credit.request.ID
}

// SettleTaskSubmissionBilling leaves asynchronous jobs funded by their hold.
// Explicit terminal success can establish a genuine zero charge. A failure
// status alone cannot establish provider cost or the user's fee obligation.
func SettleTaskSubmissionBilling(ctx *gin.Context, info *relaycommon.RelayInfo, task *model.Task, actual int) error {
	session, ok := info.Billing.(*BillingSession)
	if !ok || session.credit == nil {
		return SettleBilling(ctx, info, actual)
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.credit.review {
		return model.ErrCreditNeedsReview
	}
	if task.PrivateData.CreditRequestID != session.credit.request.ID {
		return model.ErrCreditInvariant
	}
	if task.Status == model.TaskStatusFailure {
		if err := model.MarkCreditRequestReview(model.DB, info.UserId, session.credit.request.ID, session.credit.execution); err != nil {
			return err
		}
		session.credit.review = true
		session.credit.stop()
		return nil
	}
	if task.Status != model.TaskStatusSuccess {
		err := model.YieldCreditExecution(model.DB, session.credit.execution, common.GetTimestamp())
		if err == nil {
			session.credit.relayClosed = true
		}
		session.credit.stop()
		return err
	}
	other := taskBillingOther(task)
	attachQuotaSaturation(ctx, info, other)
	if err := recordCreditTaskConsumeEvidence(model.DB, info.UserId, session.credit.request.ID, actual, info.TieredBillingSnapshot, other, session.credit.execution); err != nil {
		return err
	}
	request, err := model.FinishCreditRequest(model.DB, info.UserId, session.credit.request.ID, "settle", int64(actual), common.GetTimestamp(), session.credit.execution)
	if errors.Is(err, model.ErrCreditNeedsReview) && request.State == "review" {
		session.credit.review = true
		session.credit.stop()
		return nil
	}
	if err != nil {
		return err
	}
	session.credit.request, session.settled = request, true
	task.Quota = int(request.Charged)
	session.credit.stop()
	return nil
}

func creditBillingError(err error) *types.NewAPIError {
	code := types.ErrorCodeInsufficientUserQuota
	switch {
	case errors.Is(err, model.ErrSubscriptionWindowInsufficient):
		code = types.ErrorCode("subscription_window_insufficient")
	case errors.Is(err, model.ErrSubscriptionRightsUnavailable), errors.Is(err, model.ErrSubscriptionPurchaseUnavailable):
		code = types.ErrorCode("subscription_rights_unavailable")
	case errors.Is(err, model.ErrCreditInsufficient):
	default:
		return types.NewError(err, types.ErrorCodeUpdateDataError, types.ErrOptionWithSkipRetry())
	}
	return types.NewErrorWithStatusCode(err, code, http.StatusForbidden, types.ErrOptionWithSkipRetry(), types.ErrOptionWithNoRecordErrorLog())
}

// MarkBillingRequestSubmitted runs before network submission. A failure here
// must prevent the outgoing call. Legacy sessions retain their existing path.
func MarkBillingRequestSubmitted(info *relaycommon.RelayInfo) error {
	session, ok := info.Billing.(*BillingSession)
	if !ok || session.credit == nil {
		return nil
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.settled || session.refunded || session.credit.review {
		return model.ErrCreditOperationConflict
	}
	now := common.GetTimestamp()
	snapshot, err := common.Marshal(map[string]any{"price": info.PriceData, "other_ratios": info.PriceData.OtherRatios(), "expression": info.TieredBillingSnapshot, "quota_per_unit": session.credit.quotaPerUnit, "tool_prices": session.credit.toolPrices, "gemini_input_audio_price": session.credit.geminiInputAudioPrice})
	if err != nil {
		return err
	}
	accountID, credentialVersion := "", int64(0)
	if info.ChannelMeta != nil {
		accountID, credentialVersion = info.UpstreamAccountID, info.UpstreamCredentialVersion
	}
	evidence, err := model.RecordCreditAttemptSubmission(model.DB, model.CreditEvidenceInput{
		UserID: info.UserId, RequestID: session.credit.request.ID, EventID: "attempt-price", Attempt: info.RetryIndex + 1, Stage: "attempt", Version: "new-api-attempt-price-v1",
		AttemptPrice: &model.CreditAttemptPrice{AccountID: accountID, CredentialVersion: credentialVersion, ChannelID: info.GetChannelID(), Group: info.UsingGroup, BillingModel: info.GetBillingModelName(), UpstreamModel: info.GetUpstreamModelName(), Protocol: string(info.GetFinalRequestRelayFormat()), Snapshot: string(snapshot)},
	}, now, session.credit.execution)
	if err != nil {
		return err
	}
	session.credit.request.SubmittedAt = now
	if session.credit.relayAttempt != evidence.Attempt {
		session.credit.relayAttempt, session.credit.relayPriceID = evidence.Attempt, evidence.ID
		session.credit.relaySequence, session.credit.relayPhases = 0, make(map[string]bool)
		session.credit.relayStatus = nil
	}
	return nil
}

func (s *BillingSession) settleCredit(actual int) error {
	if s.credit.review {
		return model.ErrCreditNeedsReview
	}
	if actual < 0 || actual > common.MaxQuota {
		return model.ErrCreditInvalid
	}
	if s.refunded {
		return model.ErrCreditOperationConflict
	}
	if s.settled {
		if int64(actual) != s.credit.request.Actual {
			return model.ErrCreditOperationConflict
		}
		return nil
	}
	if actual == 0 && s.credit.request.SubmittedAt != 0 && !s.relayInfo.PriceData.FreeModel && !s.credit.zeroChargeEstablished {
		// The caller has not established a genuine zero charge. Leave funds held
		// for evidence/recovery rather than treating missing usage as a refund.
		s.credit.review = true
		if err := model.MarkCreditRequestReview(model.DB, s.relayInfo.UserId, s.credit.request.ID, s.credit.execution); err != nil {
			return err
		}
		s.credit.stop()
		return model.ErrCreditNeedsReview
	}
	request, err := model.FinishCreditRequest(model.DB, s.relayInfo.UserId, s.credit.request.ID, "settle", int64(actual), common.GetTimestamp(), s.credit.execution)
	if err != nil {
		return err
	}
	s.credit.request, s.settled = request, true
	s.credit.stop()
	return nil
}

func (s *BillingSession) refundCredit() {
	if s.settled || s.refunded || s.credit.review {
		return
	}
	request, err := model.FinishCreditRequest(model.DB, s.relayInfo.UserId, s.credit.request.ID, "release", 0, common.GetTimestamp(), s.credit.execution)
	if err != nil {
		if errors.Is(err, model.ErrCreditNeedsReview) {
			s.credit.review = true
			s.credit.stop()
		}
		common.SysError(fmt.Sprintf("credit release incomplete: user=%d request=%d error=%v", s.relayInfo.UserId, s.credit.request.ID, err))
		return
	}
	s.credit.request, s.refunded = request, true
	s.credit.stop()
}

func (credit *creditBilling) stop() {
	if credit.stopHeartbeat != nil {
		credit.stopHeartbeat()
	}
}

func startCreditHeartbeat(c *gin.Context, session *BillingSession) {
	if c == nil || c.Request == nil {
		return
	}
	originalRequest := c.Request
	requestCtx, cancelRequest := context.WithCancel(originalRequest.Context())
	c.Request = c.Request.WithContext(requestCtx)
	heartbeatCtx, stop := context.WithCancel(requestCtx)
	var stopped sync.Once
	session.credit.stopHeartbeat = func() { stopped.Do(func() { stop(); cancelRequest(); c.Request = originalRequest }) }
	// A stream worker may abort while the main handler reads c.Request.
	// Cancel its immutable context without restoring the Gin request pointer.
	session.credit.abortRequest = func() { stopped.Do(func() { stop(); cancelRequest() }) }
	lease, db := session.credit.execution, model.DB
	go func() {
		ticker := time.NewTicker(40 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-heartbeatCtx.Done():
				return
			case <-ticker.C:
				if err := model.RenewCreditExecution(db, lease, 120, common.GetTimestamp()); err != nil {
					cancelRequest()
					common.SysError(fmt.Sprintf("credit heartbeat lost request=%d: %v", lease.RequestID, err))
					return
				}
			}
		}
	}()
}

// This transient host-only value protects task insertion against a stale
// submitting worker. It is excluded from public JSON and stored task data.
func CreditBillingExecution(info *relaycommon.RelayInfo) []model.CreditExecution {
	session, ok := info.Billing.(*BillingSession)
	if !ok || session.credit == nil {
		return nil
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	return []model.CreditExecution{session.credit.execution}
}

func YieldCreditBilling(info *relaycommon.RelayInfo) error {
	session, ok := info.Billing.(*BillingSession)
	if !ok || session.credit == nil {
		return nil
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	err := model.YieldCreditExecution(model.DB, session.credit.execution, common.GetTimestamp())
	if err == nil {
		session.credit.relayClosed = true
	}
	session.credit.stop()
	return err
}

// New-mode logs and statistics are created with settlement in the primary DB.
// The old asynchronous logger cannot create a second, non-recoverable charge
// projection. Usage-evidence enrichment is supplied before intent in round 9.
func recordBillingConsumeLog(ctx *gin.Context, info *relaycommon.RelayInfo, params model.RecordConsumeLogParams) {
	if CreditBillingRequestID(info) != 0 {
		return
	}
	model.RecordConsumeLog(ctx, info.UserId, params)
}

// Final metering is durable before the monetary intent. A recovered settlement
// uses this same snapshot rather than depending on a live response handler.
func prepareCreditConsumeEvidence(info *relaycommon.RelayInfo, usage *dto.Usage, params model.RecordConsumeLogParams) error {
	session, ok := info.Billing.(*BillingSession)
	if !ok || session.credit == nil {
		return nil
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.credit.review {
		return model.ErrCreditNeedsReview
	}
	other := params.Other
	if other == nil {
		other = model.NewLogOther()
	}
	if info.CreditStreamBudgetStop != "" {
		other.SetPublic("budget_stop", info.CreditStreamBudgetStop)
	}
	facts := []hosttypes.UsageFact{
		{Field: "prompt_tokens", Unit: "token", Source: "unknown"},
		{Field: "completion_tokens", Unit: "token", Source: "unknown"},
	}
	// Without field-presence evidence, the normalized adaptor values are not
	// labelled as verified upstream receipts. Missing usage remains unknown.
	if usage != nil {
		prompt, completion := float64(params.PromptTokens), float64(params.CompletionTokens)
		facts[0].Quantity, facts[0].Source = &prompt, "adaptor"
		facts[1].Quantity, facts[1].Source = &completion, "adaptor"
	}
	byField := make(map[string]hosttypes.UsageFact)
	for _, fact := range facts {
		byField[fact.Field] = fact
	}
	if usage != nil {
		// Other adaptors may only return normalized DTOs. Retain all metering
		// categories without falsely upgrading them to raw provider receipts.
		// Non-pointer zeros cannot prove presence, so they remain unknown.
		normalized := effectiveBillingUsage(usage)
		for field, value := range map[string]int{
			"cached_tokens":            normalized.PromptTokensDetails.CachedTokens,
			"cache_creation_tokens":    normalized.PromptTokensDetails.CacheCreationTokensTotal(),
			"cache_creation_tokens_5m": normalized.ClaudeCacheCreation5mTokens,
			"cache_creation_tokens_1h": normalized.ClaudeCacheCreation1hTokens,
			"text_input_tokens":        normalized.PromptTokensDetails.TextTokens,
			"audio_input_tokens":       normalized.PromptTokensDetails.AudioTokens,
			"image_input_tokens":       normalized.PromptTokensDetails.ImageTokens,
			"text_output_tokens":       normalized.CompletionTokenDetails.TextTokens,
			"audio_output_tokens":      normalized.CompletionTokenDetails.AudioTokens,
			"image_output_tokens":      normalized.CompletionTokenDetails.ImageTokens,
			"reasoning_tokens":         normalized.CompletionTokenDetails.ReasoningTokens,
		} {
			fact := hosttypes.UsageFact{Field: field, Unit: "token", Source: "unknown"}
			if value != 0 {
				quantity := float64(value)
				fact.Quantity, fact.Source, fact.Algorithm = &quantity, "adaptor", "new-api-adaptor-normalized-v1"
			}
			byField[field] = fact
		}
		var text, image, audio *int
		if cached := normalized.PromptTokensDetails.CachedTokensDetails; cached != nil {
			text, image, audio = cached.TextTokens, cached.ImageTokens, cached.AudioTokens
		}
		for field, value := range map[string]*int{"text_cached_tokens": text, "image_cached_tokens": image, "audio_cached_tokens": audio} {
			fact := hosttypes.UsageFact{Field: field, Unit: "token", Source: "unknown"}
			if value != nil {
				quantity := float64(*value)
				fact.Quantity, fact.Source, fact.Algorithm = &quantity, "adaptor", "new-api-adaptor-normalized-v1"
			}
			byField[field] = fact
		}
	}
	for field, fact := range info.CreditUsageFacts {
		byField[field] = fact
	}
	facts = facts[:0]
	for _, fact := range byField {
		facts = append(facts, fact)
	}
	slices.SortFunc(facts, func(a, b hosttypes.UsageFact) int { return strings.Compare(a.Field, b.Field) })
	zeroEstablished := creditZeroChargeEstablished(info, usage, other)
	evidence, err := model.RecordCreditUsageEvidence(model.DB, model.CreditEvidenceInput{
		UserID: info.UserId, RequestID: session.credit.request.ID,
		EventID: "consume", Attempt: info.RetryIndex + 1, Stage: "settlement", Cumulative: true,
		Version: "new-api-metering-v1", Facts: facts,
		Consume: &model.CreditConsumeSnapshot{ReferenceQuota: int64(params.Quota), ZeroChargeEstablished: zeroEstablished, PromptTokens: params.PromptTokens, CompletionTokens: params.CompletionTokens, UseTimeSeconds: params.UseTimeSeconds, IsStream: params.IsStream, Other: other.JSONString()},
	}, common.GetTimestamp(), session.credit.execution)
	if err != nil {
		return err
	}
	session.credit.request.UsageEvidenceID = evidence.ID
	session.credit.zeroChargeEstablished = zeroEstablished
	return nil
}

// A zero monetary result is reliable only when the selected price's required
// quantities are known. A successful constant-zero price needs no token receipt.
func creditZeroChargeEstablished(info *relaycommon.RelayInfo, usage *dto.Usage, other *model.LogOther) bool {
	if info.PriceData.FreeModel {
		return true
	}
	required := []string{"prompt_tokens", "completion_tokens"}
	if snap := info.TieredBillingSnapshot; snap != nil && other.Snapshot()["billing_mode"] == "tiered_expr" {
		required = nil
		fields := map[string]string{"p": "prompt_tokens", "len": "prompt_tokens", "c": "completion_tokens", "cr": "cached_tokens", "cc": "cache_creation_tokens", "cc1h": "cache_creation_tokens_1h", "ai": "audio_input_tokens", "ao": "audio_output_tokens", "img": "image_input_tokens", "img_o": "image_output_tokens", "img_cr": "image_cached_tokens", "image_count": "image_count"}
		usedVars := billingexpr.UsedVarsByHash(snap.ExprString, snap.ExprHash)
		for variable, field := range fields {
			if !usedVars[variable] {
				continue
			}
			required = append(required, field)
			if variable == "len" && usage != nil && usage.UsageSemantic == "anthropic" {
				required = append(required, "cached_tokens", "cache_creation_tokens")
			}
			if variable == "p" && usage != nil && usage.UsageSemantic == "anthropic" && !usedVars["cr"] {
				// Native Claude normalization adds separately reported cache
				// reads to p when the expression has no separate cr price.
				required = append(required, "cached_tokens")
			}
		}
	}
	for _, field := range required {
		fact, present := info.CreditUsageFacts[field]
		if !present || fact.Quantity == nil || fact.Partial {
			return false
		}
		if fact.Source == "upstream" {
			continue
		}
		// These two deterministic Gemini normalizations have explicit raw
		// components. An arbitrary adaptor quantity is still not a receipt.
		var components []string
		if fact.Source == "adaptor" && field == "prompt_tokens" && fact.Algorithm == "gemini-input-with-tool-v1" {
			components = []string{"gemini_prompt_tokens", "gemini_tool_input_tokens"}
		} else if fact.Source == "adaptor" && field == "completion_tokens" && fact.Algorithm == "gemini-output-with-thinking-v1" {
			components = []string{"gemini_candidate_tokens", "reasoning_tokens"}
		} else {
			return false
		}
		sum := float64(0)
		for _, component := range components {
			raw, exists := info.CreditUsageFacts[component]
			if !exists || raw.Quantity == nil || raw.Source != "upstream" || raw.Partial {
				return false
			}
			sum += *raw.Quantity
		}
		if sum != *fact.Quantity {
			return false
		}
	}
	return true
}

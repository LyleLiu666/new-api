package service

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/gin-gonic/gin"
)

const BillingSourceCreditPacks = model.CreditFundingSource

type creditBilling struct {
	request       model.CreditRequest
	review        bool
	execution     model.CreditExecution
	stopHeartbeat context.CancelFunc
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
	if pref == "subscription_only" {
		return nil, types.NewErrorWithStatusCode(model.ErrCreditOperationRequired, types.ErrorCodeInvalidRequest, http.StatusBadRequest, types.ErrOptionWithSkipRetry())
	}
	if pref == "subscription_first" {
		hasSubscription, err := model.HasActiveUserSubscription(info.UserId)
		if err != nil {
			return nil, creditBillingError(err)
		}
		if hasSubscription {
			return nil, types.NewErrorWithStatusCode(model.ErrCreditOperationRequired, types.ErrorCodeInvalidRequest, http.StatusBadRequest, types.ErrOptionWithSkipRetry())
		}
	}
	if info.RequestId == "" {
		info.RequestId = common.NewRequestId()
	}
	// Serialize explicit multipliers too: PriceData keeps its map private.
	snapshot, err := common.Marshal(map[string]any{"price": info.PriceData, "other_ratios": info.PriceData.OtherRatios(), "expression": info.TieredBillingSnapshot, "quota_per_unit": common.QuotaPerUnit})
	if err != nil {
		return nil, creditBillingError(err)
	}
	tokenID := info.TokenId
	if info.IsPlayground {
		tokenID = 0
	}
	request, err := model.BeginCreditRequest(model.DB, model.CreditRequestInput{ChannelID: common.GetContextKeyInt(c, constant.ContextKeyChannelId), Group: info.UsingGroup, UserID: info.UserId, RequestID: info.RequestId, ModelName: info.GetBillingModelName(), Protocol: string(info.RelayFormat), PriceSnapshot: string(snapshot), TokenID: tokenID, Playground: info.IsPlayground, Free: info.PriceData.FreeModel, Amount: int64(amount)}, common.GetTimestamp())
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
	session := &BillingSession{relayInfo: info, preConsumedQuota: amount, tokenConsumed: amount, credit: &creditBilling{request: request, execution: lease}}
	info.FinalPreConsumedQuota, info.BillingSource = amount, BillingSourceCreditPacks
	startCreditHeartbeat(c, session)
	return session, nil
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
		session.credit.stop()
		return err
	}
	request, err := model.FinishCreditRequest(model.DB, info.UserId, session.credit.request.ID, "settle", int64(actual), common.GetTimestamp(), session.credit.execution)
	if err != nil {
		return err
	}
	session.credit.request, session.settled = request, true
	session.credit.stop()
	return nil
}

func creditBillingError(err error) *types.NewAPIError {
	if errors.Is(err, model.ErrCreditInsufficient) {
		return types.NewErrorWithStatusCode(err, types.ErrorCodeInsufficientUserQuota, http.StatusForbidden, types.ErrOptionWithSkipRetry(), types.ErrOptionWithNoRecordErrorLog())
	}
	return types.NewError(err, types.ErrorCodeUpdateDataError, types.ErrOptionWithSkipRetry())
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
	if err := model.MarkCreditRequestSubmitted(model.DB, info.UserId, session.credit.request.ID, now, session.credit.execution); err != nil {
		return err
	}
	session.credit.request.SubmittedAt = now
	return nil
}

func (s *BillingSession) settleCredit(actual int) error {
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
	if actual == 0 && s.credit.request.SubmittedAt != 0 && !s.relayInfo.PriceData.FreeModel {
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

package service

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/relaykit/types"
)

const BillingSourceCreditPacks = "credit_packs"

type creditBilling struct {
	request model.CreditRequest
	review  bool
}

func newCreditBillingSession(info *relaycommon.RelayInfo, amount, channelType int) (*BillingSession, *types.NewAPIError) {
	// Only the HTTP text paths admitted here have a durable submission marker.
	// Other billing paths are enabled as their lifecycle is integrated.
	textMode := info.RelayMode == relayconstant.RelayModeChatCompletions || info.RelayMode == relayconstant.RelayModeCompletions || info.RelayMode == relayconstant.RelayModeResponses || info.RelayFormat == types.RelayFormatClaude
	if !textMode || (channelType != constant.ChannelTypeOpenAI && channelType != constant.ChannelTypeAnthropic) || (info.RelayFormat != types.RelayFormatOpenAI && info.RelayFormat != types.RelayFormatOpenAIResponses && info.RelayFormat != types.RelayFormatClaude) {
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
	request, err := model.BeginCreditRequest(model.DB, model.CreditRequestInput{UserID: info.UserId, RequestID: info.RequestId, ModelName: info.GetBillingModelName(), Protocol: string(info.RelayFormat), PriceSnapshot: string(snapshot), TokenID: tokenID, Playground: info.IsPlayground, Free: info.PriceData.FreeModel, Amount: int64(amount)}, common.GetTimestamp())
	if err != nil {
		return nil, creditBillingError(err)
	}
	if request.State != "reserved" && request.State != "executing" {
		return nil, creditBillingError(model.ErrCreditOperationConflict)
	}
	session := &BillingSession{relayInfo: info, preConsumedQuota: amount, tokenConsumed: amount, credit: &creditBilling{request: request}}
	info.FinalPreConsumedQuota, info.BillingSource = amount, BillingSourceCreditPacks
	return session, nil
}

func creditBillingError(err error) *types.NewAPIError {
	if errors.Is(err, model.ErrCreditInsufficient) || errors.Is(err, model.ErrCreditDebtOutstanding) {
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
	if err := model.MarkCreditRequestSubmitted(model.DB, info.UserId, session.credit.request.ID, now); err != nil {
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
		if err := model.MarkCreditRequestReview(model.DB, s.relayInfo.UserId, s.credit.request.ID); err != nil {
			return err
		}
		return model.ErrCreditNeedsReview
	}
	request, err := model.FinishCreditRequest(model.DB, s.relayInfo.UserId, s.credit.request.ID, "settle", int64(actual), common.GetTimestamp())
	if err != nil {
		return err
	}
	s.credit.request, s.settled = request, true
	return nil
}

func (s *BillingSession) refundCredit() {
	if s.settled || s.refunded || s.credit.review {
		return
	}
	request, err := model.FinishCreditRequest(model.DB, s.relayInfo.UserId, s.credit.request.ID, "release", 0, common.GetTimestamp())
	if err != nil {
		if errors.Is(err, model.ErrCreditNeedsReview) {
			s.credit.review = true
		}
		common.SysError(fmt.Sprintf("credit release incomplete: user=%d request=%d error=%v", s.relayInfo.UserId, s.credit.request.ID, err))
		return
	}
	s.credit.request, s.refunded = request, true
}

package service

import (
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	kittypes "github.com/QuantumNous/new-api/relaykit/types"
	hosttypes "github.com/QuantumNous/new-api/types"
	"github.com/tidwall/gjson"
	"gorm.io/gorm"
)

// CreditUsageObservation is a temporary metering/pricing projection owned by
// the stream handler. Do not copy the whole RelayInfo: its transport counters
// and first-response timing are written concurrently by the scanner.
func CreditUsageObservation(info *relaycommon.RelayInfo) relaycommon.RelayInfo {
	observation := relaycommon.RelayInfo{
		UserId: info.UserId, TokenId: info.TokenId, TokenUnlimited: info.TokenUnlimited,
		OriginModelName: info.OriginModelName, BillingModelName: info.BillingModelName,
		StartTime: info.StartTime, IsStream: info.IsStream, RelayMode: info.RelayMode,
		RelayFormat: info.RelayFormat, FinalRequestRelayFormat: info.FinalRequestRelayFormat,
		RequestConversionChain: info.RequestConversionChain,
		Billing:                info.Billing, BillingSource: info.BillingSource, RetryIndex: info.RetryIndex,
		PriceData: info.PriceData, TieredBillingSnapshot: info.TieredBillingSnapshot,
		BillingRequestInput: info.BillingRequestInput, BillingImageCount: info.BillingImageCount,
		ChannelMeta: info.ChannelMeta, ResponsesUsageInfo: info.ResponsesUsageInfo,
		TokenCountMeta: info.TokenCountMeta, CreditPromptEstimation: info.CreditPromptEstimation,
		CreditUsageFacts: maps.Clone(info.CreditUsageFacts), QuotaClamp: info.QuotaClamp,
	}
	observation.PriceData.ReplaceOtherRatios(info.PriceData.OtherRatios())
	if info.ResponsesUsageInfo != nil {
		observation.ResponsesUsageInfo = &relaycommon.ResponsesUsageInfo{BuiltInTools: make(map[string]*relaycommon.BuildInToolInfo, len(info.BuiltInTools))}
		for name, tool := range info.BuiltInTools {
			if tool == nil {
				observation.BuiltInTools[name] = nil
				continue
			}
			copy := *tool
			observation.BuiltInTools[name] = &copy
		}
	}
	return observation
}

// ObserveCreditUsage extracts only protocol-declared numeric usage fields.
// Paths describe a provider's wire contract; no response text is persisted.
// Usage-bearing stream frames are saved before forwarding to the client.
func ObserveCreditUsage(info *relaycommon.RelayInfo, body []byte, paths map[string]string, cumulative, outputFinal bool) error {
	fields := make(map[string]CreditUsageField, len(paths))
	for field, path := range paths {
		fields[field] = CreditUsageField{Path: path, Unit: "token"}
	}
	return ObserveCreditUsageFields(info, body, fields, cumulative, outputFinal)
}

// CreditUsageField declares the quantity and unit a provider actually reports.
// In particular, a duration receipt must not be represented as a token count.
type CreditUsageField struct {
	Path string
	Unit string
}

// Realtime observes independently rounded event counts. Retain each counter
// separately from provider receipts and from the final connection aggregate.
// The caller excludes refused client input and input acknowledgements.
func RecordCreditRealtimeEstimation(info *relaycommon.RelayInfo, facts []hosttypes.UsageFact, input bool) *kittypes.NewAPIError {
	requestID := CreditBillingRequestID(info)
	if requestID == 0 || len(facts) == 0 {
		return nil
	}
	if info.CreditUsageAttempt != info.RetryIndex+1 {
		info.CreditUsageAttempt = info.RetryIndex + 1
		info.CreditUsageSequence, info.CreditUsageFacts = 0, make(map[string]hosttypes.UsageFact)
	}
	direction := "output"
	if input {
		direction = "input"
	}
	for i := range facts {
		facts[i].Field = "realtime_observed." + direction + "." + facts[i].Field
	}
	sequence := info.CreditUsageSequence + 1
	_, err := model.RecordCreditUsageEvidence(model.DB, model.CreditEvidenceInput{
		UserID: info.UserId, RequestID: requestID, Attempt: info.RetryIndex + 1, EventID: "realtime-estimate-" + strconv.FormatInt(sequence, 10),
		Stage: "estimate", Sequence: sequence, Version: "new-api-realtime-event-counter-v1", Facts: facts,
	}, common.GetTimestamp(), CreditBillingExecution(info)...)
	if err != nil {
		return kittypes.NewError(err, kittypes.ErrorCode("stream_budget_unavailable"), kittypes.ErrOptionWithSkipRetry(), kittypes.ErrOptionWithHideErrMsg("stream metering cannot be recorded"))
	}
	info.CreditUsageSequence = sequence
	return nil
}

// The admission snapshot is the historical fallback. Once a submission has
// an effective price, asynchronous metering and recovery use that same artifact.
func creditSubmittedPrice(db *gorm.DB, request model.CreditRequest) (model.CreditUsageEvidence, string, error) {
	attempt, input, err := model.GetCreditAttemptPriceEvidence(db, request.UserID, request.ID, 0)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return model.CreditUsageEvidence{}, request.PriceSnapshot, nil
	}
	if err != nil {
		return model.CreditUsageEvidence{}, "", err
	}
	return attempt, input.AttemptPrice.Snapshot, nil
}

// Task completion has no implied token receipt. Persist the host's final fee
// separately, and retain that same receipt when a funds transaction is retried.
func recordCreditTaskConsumeEvidence(db *gorm.DB, userID int, requestID int64, actual int, taskSnapshot *billingexpr.BillingSnapshot, other *model.LogOther, execution ...model.CreditExecution) error {
	var request model.CreditRequest
	if err := db.Where("id = ? AND user_id = ?", requestID, userID).First(&request).Error; err != nil {
		return err
	}
	input := model.CreditEvidenceInput{
		UserID: userID, RequestID: requestID, EventID: "task-consume", Attempt: 1,
		Stage: "settlement", Cumulative: true, Version: "new-api-task-metering-v1",
		Facts: []hosttypes.UsageFact{
			{Field: "prompt_tokens", Unit: "token", Source: "unknown"},
			{Field: "completion_tokens", Unit: "token", Source: "unknown"},
		},
	}
	if request.UsageEvidenceID != 0 {
		var evidence model.CreditUsageEvidence
		if err := db.Where("id = ? AND user_id = ? AND request_id = ?", request.UsageEvidenceID, userID, requestID).First(&evidence).Error; err != nil {
			return err
		}
		if err := common.UnmarshalJsonStr(evidence.Payload, &input); err != nil {
			return err
		}
		if input.Consume == nil || input.Consume.ReferenceQuota != int64(actual) {
			return model.ErrCreditOperationConflict
		}
	} else {
		attempt, snapshot, err := creditSubmittedPrice(db, request)
		if err != nil {
			return err
		}
		if attempt.ID != 0 {
			input.Attempt, input.AttemptPriceEvidenceID = attempt.Attempt, attempt.ID
		}
		var pricing struct {
			Price      hosttypes.PriceData          `json:"price"`
			Expression *billingexpr.BillingSnapshot `json:"expression"`
		}
		if err := common.UnmarshalJsonStr(snapshot, &pricing); err != nil {
			return err
		}
		zeroEstablished := actual == 0 && (pricing.Price.FreeModel || pricing.Price.UsePrice && pricing.Price.Quota == 0)
		if taskSnapshot != nil {
			if pricing.Expression == nil {
				return model.ErrCreditInvariant
			}
			if actual == 0 {
				result, _, err := EvaluateTaskCompletionUsage(pricing.Expression, taskSnapshot.MeasuredUsageFacts)
				if err != nil {
					return err
				}
				zeroEstablished = result.ActualQuotaAfterGroup == 0
				for field := range result.UsageKeys {
					if taskSnapshot.MeasuredUsageFacts[field] != nil {
						continue
					}
					// Frozen enum/boolean selectors describe request conditions.
					// Numeric submission estimates cannot establish final zero use.
					switch pricing.Expression.UsageFacts[field].(type) {
					case string, bool:
					default:
						zeroEstablished = false
					}
				}
			}
			for field, unit := range pricing.Expression.TaskUsageUnits {
				fact := hosttypes.UsageFact{Field: "task." + field, Unit: unit, Source: "unknown"}
				value, measured := taskSnapshot.MeasuredUsageFacts[field]
				if !measured {
					value = pricing.Expression.UsageFacts[field]
				}
				if value != nil {
					encoded, err := common.Marshal(value)
					if err != nil || common.GetJsonType(common.RawMessage(encoded)) != "number" {
						return model.ErrCreditInvalid
					}
					var quantity float64
					if err := common.Unmarshal(encoded, &quantity); err != nil {
						return model.ErrCreditInvalid
					}
					fact.Quantity = &quantity
					fact.Source, fact.Algorithm = "estimate", "task-plugin-estimate-v1"
					if measured {
						fact.Source, fact.Algorithm = "adaptor", "task-plugin-usage-v1"
					}
				}
				input.Facts = append(input.Facts, fact)
			}
			slices.SortFunc(input.Facts, func(a, b hosttypes.UsageFact) int { return strings.Compare(a.Field, b.Field) })
		}
		if other == nil {
			other = model.NewLogOther()
		}
		other.SetPublic("is_task", true)
		input.Consume = &model.CreditConsumeSnapshot{ReferenceQuota: int64(actual), ZeroChargeEstablished: zeroEstablished, Other: other.JSONString()}
	}
	_, err := model.RecordCreditUsageEvidence(db, input, common.GetTimestamp(), execution...)
	return err
}

func ObserveCreditUsageFields(info *relaycommon.RelayInfo, body []byte, paths map[string]CreditUsageField, cumulative, outputFinal bool) error {
	if info.BillingSource != BillingSourceCreditPacks {
		return nil
	}
	if err := RecordCreditUpstreamResponse(info, nil, false); err != nil {
		return err
	}
	if info.CreditUsageAttempt != info.RetryIndex+1 {
		info.CreditUsageAttempt = info.RetryIndex + 1
		info.CreditUsageSequence, info.CreditUsageFacts = 0, make(map[string]hosttypes.UsageFact)
	}
	fields := make([]string, 0, len(paths))
	for field := range paths {
		fields = append(fields, field)
	}
	slices.Sort(fields)
	var facts []hosttypes.UsageFact
	for _, field := range fields {
		declared := paths[field]
		if declared.Unit != "token" && declared.Unit != "second" && declared.Unit != "count" {
			return fmt.Errorf("%w: usage unit %s", model.ErrCreditInvalid, declared.Unit)
		}
		value := gjson.GetBytes(body, declared.Path)
		if !value.Exists() || value.Type == gjson.Null {
			continue
		}
		if value.Type != gjson.Number || math.IsNaN(value.Num) || math.IsInf(value.Num, 0) || value.Num < 0 || value.Num > float64(common.MaxQuota) || (declared.Unit != "second" && math.Trunc(value.Num) != value.Num) {
			return fmt.Errorf("%w: usage field %s", model.ErrCreditInvalid, field)
		}
		quantity := value.Num
		fact := hosttypes.UsageFact{Field: field, Unit: declared.Unit, Quantity: &quantity, Source: "upstream"}
		if !outputFinal {
			switch field {
			case "completion_tokens", "text_output_tokens", "audio_output_tokens", "image_output_tokens", "reasoning_tokens", "gemini_candidate_tokens", "total_tokens":
				fact.Partial = true
			}
		}
		facts = append(facts, fact)
	}
	if len(facts) == 0 {
		return nil
	}
	sequence := info.CreditUsageSequence + 1
	next := maps.Clone(info.CreditUsageFacts)
	if next == nil {
		next = make(map[string]hosttypes.UsageFact)
	}
	for _, fact := range facts {
		if previous, exists := next[fact.Field]; !cumulative && exists && previous.Quantity != nil {
			quantity := *previous.Quantity + *fact.Quantity
			if quantity > float64(common.MaxQuota) {
				return model.ErrCreditInvalid
			}
			fact.Quantity = &quantity
		}
		next[fact.Field] = fact
	}
	if requestID := CreditBillingRequestID(info); requestID != 0 {
		_, err := model.RecordCreditUsageEvidence(model.DB, model.CreditEvidenceInput{UserID: info.UserId, RequestID: requestID, EventID: "usage-" + strconv.FormatInt(sequence, 10), Attempt: info.RetryIndex + 1, Sequence: sequence, Cumulative: cumulative, Stage: "upstream", Version: "provider-fields-v1", Facts: facts}, common.GetTimestamp(), CreditBillingExecution(info)...)
		if err != nil {
			return err
		}
	}
	info.CreditUsageFacts = next
	info.CreditUsageSequence = sequence
	return nil
}

// Missing final usage may be estimated without replacing a complete receipt.
// An intermediate cumulative count remains a lower bound, including zero.
func EstimateCreditUsageField(info *relaycommon.RelayInfo, field string, quantity int, algorithm string, estimation ...*hosttypes.UsageEstimation) {
	if info.BillingSource != BillingSourceCreditPacks {
		return
	}
	if previous, exists := info.CreditUsageFacts[field]; exists {
		if !previous.Partial {
			return
		}
		if previous.Quantity != nil && *previous.Quantity > float64(quantity) {
			quantity = int(*previous.Quantity)
		}
	}
	if info.CreditUsageFacts == nil {
		info.CreditUsageFacts = make(map[string]hosttypes.UsageFact)
	}
	value := float64(quantity)
	fact := hosttypes.UsageFact{Field: field, Unit: "token", Quantity: &value, Source: "estimate", Algorithm: algorithm}
	if len(estimation) == 1 {
		fact.Estimation = estimation[0]
	} else if algorithm == "new-api-prompt-count-v1" || algorithm == "new-api-audio-input-estimate-v1" {
		fact.Estimation = info.CreditPromptEstimation
	}
	info.CreditUsageFacts[field] = fact
}

// Text quantity and counter provenance are captured together. Complete provider
// receipts still win, and partial provider quantities remain a lower bound.
func EstimateCreditTextUsageField(info *relaycommon.RelayInfo, field string, texts []string, model, algorithm string, extraTokens int) int {
	count, estimation := CountTextTokensWithEstimation(texts, model)
	count = common.QuotaRound(float64(count) + float64(extraTokens))
	estimation.Quantity = float64(count)
	if extraTokens != 0 {
		estimation.Parameters["extra_tokens"] = float64(extraTokens)
	}
	EstimateCreditUsageField(info, field, count, algorithm, estimation)
	return count
}

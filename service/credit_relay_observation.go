package service

import (
	"errors"
	"sync"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	kittypes "github.com/QuantumNous/new-api/relaykit/types"
	"github.com/gin-gonic/gin"
)

// Transport evidence is bounded to the first occurrence of each boundary in
// an attempt. It records no response body, URL, credential or delivery claim.
func (s *BillingSession) recordRelayObservation(observation model.CreditRelayObservation) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	credit := s.credit
	if credit != nil && credit.relayFault != nil {
		return credit.relayFault
	}
	if credit == nil || credit.relayAttempt == 0 || credit.relayClosed || s.settled || s.refunded || credit.review || credit.relayPhases[observation.Phase] {
		return nil
	}
	_, err := model.RecordCreditUsageEvidence(model.DB, model.CreditEvidenceInput{
		UserID: credit.request.UserID, RequestID: credit.request.ID,
		Attempt: credit.relayAttempt, AttemptPriceEvidenceID: credit.relayPriceID,
		EventID: observation.Phase, Stage: "relay", Sequence: credit.relaySequence + 1,
		Version: "new-api-relay-boundary-v1", Observation: &observation,
	}, common.GetTimestamp(), credit.execution)
	if err != nil {
		// Even if persisting review also fails, the local fence prevents this
		// worker from creating a financial intent or submitting another attempt.
		credit.review = true
		reviewErr := model.MarkCreditRequestReview(model.DB, credit.request.UserID, credit.request.ID, credit.execution)
		credit.relayFault = kittypes.NewError(errors.Join(err, reviewErr), kittypes.ErrorCode("relay_evidence_unavailable"), kittypes.ErrOptionWithSkipRetry(), kittypes.ErrOptionWithHideErrMsg("relay evidence cannot be recorded"))
		if credit.abortRequest != nil {
			credit.abortRequest()
		}
		return credit.relayFault
	}
	credit.relaySequence++
	credit.relayPhases[observation.Phase] = true
	if observation.StatusCode != nil {
		status := *observation.StatusCode
		credit.relayStatus = &status
	}
	return nil
}

// Retry safety is evaluated before channel errors or administrator status
// settings: neither can grant permission to replay an ambiguous generation.
func RelayRetryBoundary(c *gin.Context) *PolicyDecision {
	if c.Request != nil && c.Request.Context().Err() != nil {
		return &PolicyDecision{Action: "stop", Reason: "client_cancelled", Source: "transport"}
	}
	value, exists := c.Get("relay_billing_session")
	if !exists {
		if c.Writer != nil && c.Writer.Written() && c.Writer.Size() > 0 {
			return &PolicyDecision{Action: "stop", Reason: "client_output_started", Source: "transport"}
		}
		return nil
	}
	session, ok := value.(*BillingSession)
	if !ok {
		return &PolicyDecision{Action: "stop", Reason: "billing_state_unknown", Source: "accounting"}
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	credit := session.credit
	if session.settled || session.refunded {
		return &PolicyDecision{Action: "stop", Reason: "billing_already_finished", Source: "accounting"}
	}
	if c.Writer != nil && c.Writer.Written() && c.Writer.Size() > 0 && (credit == nil || c.Writer.Status() >= 400) {
		return &PolicyDecision{Action: "stop", Reason: "client_output_started", Source: "transport"}
	}
	if credit == nil {
		return nil
	}
	if credit.review || credit.relayFault != nil {
		return &PolicyDecision{Action: "stop", Reason: "billing_requires_review", Source: "accounting"}
	}
	if credit.relayPhases["client_write_possible"] || credit.relayPhases["client_write_accepted"] {
		return &PolicyDecision{Action: "stop", Reason: "client_output_started", Source: "transport"}
	}
	if credit.relayAttempt > 0 && (credit.relayStatus == nil || *credit.relayStatus < 400) {
		return &PolicyDecision{Action: "stop", Reason: "submission_result_unknown", Source: "transport"}
	}
	return nil
}

// A nil status denotes a provider SDK response without an observable HTTP
// status. A response is not proof of success or billable usage.
func RecordCreditUpstreamResponse(info *relaycommon.RelayInfo, status *int, failed bool) error {
	session, ok := info.Billing.(*BillingSession)
	if !ok || session.credit == nil {
		return nil
	}
	phase := "upstream_response"
	if failed {
		phase = "upstream_error"
	}
	return session.recordRelayObservation(model.CreditRelayObservation{Phase: phase, StatusCode: status})
}

// WebSocket workers call this around writes belonging to the current bill.
// Upstream input, unrelated stream events and control frames are excluded.
func RecordCreditClientWrite(info *relaycommon.RelayInfo, count int64, accepted bool) error {
	session, ok := info.Billing.(*BillingSession)
	if !ok || session.credit == nil || count <= 0 {
		return nil
	}
	phase := "client_write_possible"
	if accepted {
		phase = "client_write_accepted"
	}
	count = min(count, int64(common.MaxQuota))
	return session.recordRelayObservation(model.CreditRelayObservation{Phase: phase, Bytes: &count})
}

type creditRelayWriter struct {
	gin.ResponseWriter
	session   *BillingSession
	mu        sync.Mutex
	lastErr   error
	errorSent bool
}

func (w *creditRelayWriter) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.ResponseWriter.Status() >= 400 {
		return w.ResponseWriter.Write(data)
	}
	if w.lastErr != nil {
		return 0, w.lastErr
	}
	if len(data) == 0 {
		return w.ResponseWriter.Write(data)
	}
	count := int64(min(len(data), common.MaxQuota))
	if err := w.session.recordRelayObservation(model.CreditRelayObservation{Phase: "client_write_possible", Bytes: &count}); err != nil {
		w.lastErr = err
		return 0, err
	}
	n, err := w.ResponseWriter.Write(data)
	if n > 0 {
		accepted := int64(min(n, common.MaxQuota))
		err = errors.Join(err, w.session.recordRelayObservation(model.CreditRelayObservation{Phase: "client_write_accepted", Bytes: &accepted}))
	}
	if err != nil {
		w.lastErr = err
	}
	return n, err
}

func (w *creditRelayWriter) RelayWriteError() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.lastErr
}

func (w *creditRelayWriter) RelayEvidenceError() error {
	w.session.mu.Lock()
	defer w.session.mu.Unlock()
	if w.session.credit.relayFault == nil {
		return nil
	}
	return w.session.credit.relayFault
}

// Only the protocol error renderer can bypass a failed generated-output
// writer. This emits one terminal error, never another generated frame.
func (w *creditRelayWriter) WriteRelayError(data []byte) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.errorSent {
		return nil
	}
	w.errorSent = true
	// A failed post-write observation may interrupt a renderer before its
	// blank separator. Finish that boundary before emitting the error event.
	if w.ResponseWriter.Size() > 0 {
		data = append([]byte("\n\n"), data...)
	}
	if _, err := w.ResponseWriter.Write(data); err != nil {
		return err
	}
	w.ResponseWriter.Flush()
	return nil
}

func (w *creditRelayWriter) WriteString(data string) (int, error) {
	return w.Write([]byte(data))
}

func (w *creditRelayWriter) Flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.ResponseWriter.Flush()
}

// Keepalive control bytes are not generated content. Preserve their write
// behavior without falsely recording a client output boundary.
func (w *creditRelayWriter) WriteRelayControl(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.lastErr != nil {
		return 0, w.lastErr
	}
	return w.ResponseWriter.Write(data)
}

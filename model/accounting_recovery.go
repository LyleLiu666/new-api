package model

import (
	"errors"
	"fmt"

	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm"
)

var ErrCreditLeaseLost = errors.New("credit execution lease lost")

// CreditExecution fences one worker's writes to one request. Owners are
// server-generated identities, never API keys or provider credentials.
type CreditExecution struct {
	UserID    int
	RequestID int64
	Owner     string
	Epoch     int64
	Clock     func() int64 `json:"-"`
}

func validateCreditExecution(request CreditRequest, now int64, execution []CreditExecution) error {
	if request.LeaseEpoch == 0 {
		if len(execution) == 0 {
			return nil
		}
		return ErrCreditLeaseLost
	}
	if len(execution) != 1 {
		return ErrCreditLeaseLost
	}
	lease := execution[0]
	if lease.Clock != nil {
		now = lease.Clock()
	}
	if !validCreditTime(now) {
		return ErrCreditInvalid
	}
	if lease.UserID != request.UserID || lease.RequestID != request.ID || lease.Owner != request.LeaseOwner || lease.Epoch != request.LeaseEpoch {
		return ErrCreditLeaseLost
	}
	if request.State != "settled" && request.State != "released" && request.LeaseUntil <= now {
		return ErrCreditLeaseLost
	}
	return nil
}

func ClaimCreditExecution(db *gorm.DB, userID int, requestID int64, owner string, ttl, now int64, clock ...func() int64) (CreditExecution, error) {
	var lease CreditExecution
	if len(clock) > 1 || db == nil || userID <= 0 || requestID <= 0 || !validCreditID(owner, 128) || ttl <= 0 || ttl > 86400 || !validCreditTime(now) || now > common.MaxWalletQuota-ttl {
		return lease, ErrCreditInvalid
	}
	err := db.Transaction(func(tx *gorm.DB) error {
		if err := lockCreditAccount(tx, userID, false); err != nil {
			return err
		}
		var request CreditRequest
		if err := tx.Where("id = ? AND user_id = ?", requestID, userID).First(&request).Error; err != nil {
			return err
		}
		if len(clock) == 1 && clock[0] != nil {
			now = clock[0]()
			if !validCreditTime(now) || now > common.MaxWalletQuota-ttl {
				return ErrCreditInvalid
			}
		}
		if request.State == "settled" || request.State == "released" {
			return ErrCreditOperationConflict
		}
		if request.LeaseUntil > now && request.LeaseOwner != owner {
			return ErrCreditLeaseLost
		}
		epoch := request.LeaseEpoch
		if request.LeaseUntil <= now {
			if epoch >= common.MaxWalletQuota {
				return ErrCreditInvariant
			}
			epoch++
		}
		if epoch <= 0 {
			return ErrCreditInvariant
		}
		if err := tx.Model(&request).Updates(map[string]any{"lease_owner": owner, "lease_epoch": epoch, "lease_until": now + ttl}).Error; err != nil {
			return err
		}
		lease = CreditExecution{UserID: userID, RequestID: requestID, Owner: owner, Epoch: epoch}
		if len(clock) == 1 {
			lease.Clock = clock[0]
		}
		return nil
	})
	return lease, err
}

func RenewCreditExecution(db *gorm.DB, lease CreditExecution, ttl, now int64) error {
	if db == nil || ttl <= 0 || ttl > 86400 || !validCreditTime(now) || now > common.MaxWalletQuota-ttl {
		return ErrCreditInvalid
	}
	return db.Transaction(func(tx *gorm.DB) error {
		if err := lockCreditAccount(tx, lease.UserID, false); err != nil {
			return err
		}
		var request CreditRequest
		if err := tx.Where("id = ? AND user_id = ?", lease.RequestID, lease.UserID).First(&request).Error; err != nil {
			return err
		}
		if err := validateCreditExecution(request, now, []CreditExecution{lease}); err != nil {
			return err
		}
		if request.State == "settled" || request.State == "released" {
			return ErrCreditOperationConflict
		}
		if lease.Clock != nil {
			now = lease.Clock()
			if !validCreditTime(now) || now > common.MaxWalletQuota-ttl || request.LeaseUntil <= now {
				return ErrCreditLeaseLost
			}
		}
		return tx.Model(&request).Update("lease_until", now+ttl).Error
	})
}

// Yield never changes funds or evidence. The next worker must obtain a new
// epoch; a worker holding this yielded epoch can no longer mutate the request.
func YieldCreditExecution(db *gorm.DB, lease CreditExecution, now int64) error {
	return db.Transaction(func(tx *gorm.DB) error {
		if err := lockCreditAccount(tx, lease.UserID, false); err != nil {
			return err
		}
		var request CreditRequest
		if err := tx.Where("id = ? AND user_id = ?", lease.RequestID, lease.UserID).First(&request).Error; err != nil {
			return err
		}
		if err := validateCreditExecution(request, now, []CreditExecution{lease}); err != nil {
			return err
		}
		return tx.Model(&request).Update("lease_until", 0).Error
	})
}

// A page has a stable ID cursor. Transient failures stay visible and receive
// bounded backoff; they never erase the original intent or held funds.
type CreditRecoveryResult struct {
	RequestID int64  `json:"request_id"`
	State     string `json:"state"`
	Error     string `json:"error,omitempty"`
}

func RecoverCreditRequests(db *gorm.DB, owner string, afterID int64, limit int, now int64, clock ...func() int64) ([]CreditRecoveryResult, int64, error) {
	if len(clock) > 1 || db == nil || !validCreditID(owner, 128) || afterID < 0 || limit <= 0 || limit > 1000 || !validCreditTime(now) {
		return nil, afterID, ErrCreditInvalid
	}
	var requests []CreditRequest
	if err := db.Where("id > ? AND lease_until <= ? AND next_recovery_at <= ? AND recovery_blocked_at = 0 AND state IN ?", afterID, now, now, []string{"reserved", "executing", "pending"}).Order("id asc").Limit(limit).Find(&requests).Error; err != nil {
		return nil, afterID, err
	}
	results := make([]CreditRecoveryResult, 0, len(requests))
	for _, request := range requests {
		afterID = request.ID
		lease, err := ClaimCreditExecution(db, request.UserID, request.ID, owner, 120, now, clock...)
		if errors.Is(err, ErrCreditLeaseLost) || errors.Is(err, ErrCreditOperationConflict) {
			continue
		}
		if err != nil {
			return results, afterID, err
		}
		if err := db.First(&request, request.ID).Error; err != nil {
			return results, afterID, err
		}
		switch {
		case request.IntentKind != "":
			request, err = FinishCreditRequest(db, request.UserID, request.ID, request.IntentKind, request.Actual, now, lease)
		case request.SubmittedAt == 0:
			request, err = FinishCreditRequest(db, request.UserID, request.ID, "release", 0, now, lease)
		case request.TaskRowID > 0:
			// Completion quantities belong to protocol-specific polling, not the
			// crash scanner. A task's old estimate is not its final usage receipt.
			err = YieldCreditExecution(db, lease, now)
		default:
			err = MarkCreditRequestReviewAt(db, request.UserID, request.ID, now, lease)
			if err == nil {
				request.State = "review"
			}
		}
		result := CreditRecoveryResult{RequestID: request.ID, State: request.State}
		if err != nil {
			result.Error = err.Error()
			if errors.Is(err, ErrCreditLeaseLost) {
				results = append(results, result)
				continue
			}
			if recordErr := recordCreditRecoveryFailure(db, lease, err, now); recordErr != nil {
				return results, afterID, recordErr
			}
		}
		results = append(results, result)
	}
	return results, afterID, nil
}

func recordCreditRecoveryFailure(db *gorm.DB, lease CreditExecution, failure error, now int64) error {
	return db.Transaction(func(tx *gorm.DB) error {
		if err := lockCreditAccount(tx, lease.UserID, false); err != nil {
			return err
		}
		var request CreditRequest
		if err := tx.First(&request, lease.RequestID).Error; err != nil {
			return err
		}
		if err := validateCreditExecution(request, now, []CreditExecution{lease}); err != nil {
			return err
		}
		attempts := request.RecoveryAttempts + 1
		if attempts <= 0 {
			return ErrCreditInvariant
		}
		errorText := []rune(failure.Error())
		if len(errorText) > 1024 {
			errorText = errorText[:1024]
		}
		blocked := int64(0)
		maxAttempts := common.GetEnvOrDefault("CREDIT_RECOVERY_MAX_ATTEMPTS", 20)
		if maxAttempts < 1 || maxAttempts > 1000 {
			return ErrCreditInvalid
		}
		if attempts >= int64(maxAttempts) {
			blocked = now
		}
		delay := int64(30) << min(attempts-1, 7)
		return tx.Model(&request).Updates(map[string]any{"recovery_attempts": attempts, "last_recovery_error": string(errorText), "next_recovery_at": now + delay, "recovery_blocked_at": blocked, "lease_until": 0}).Error
	})
}

type CreditAccountDifference struct {
	Object   string `json:"object"`
	ID       int64  `json:"id"`
	Field    string `json:"field"`
	Expected int64  `json:"expected"`
	Actual   int64  `json:"actual"`
}

// A per-account writer lock supplies a consistent snapshot on all dialects,
// including SQLite. Differences are evidence, not an instruction to overwrite
// money. Raw reservations are included even before they acquire a request.
func ReconcileCreditAccount(db *gorm.DB, userID int) ([]CreditAccountDifference, error) {
	if db == nil || userID <= 0 {
		return nil, ErrCreditInvalid
	}
	differences := make([]CreditAccountDifference, 0)
	err := db.Transaction(func(tx *gorm.DB) error {
		if err := lockCreditAccount(tx, userID, false); err != nil {
			return err
		}
		var packs []CreditPack
		if err := tx.Where("user_id = ?", userID).Order("id asc").Find(&packs).Error; err != nil {
			return err
		}
		for _, pack := range packs {
			quantities := []struct {
				name   string
				amount int64
			}{
				{"available", pack.Available}, {"held", pack.Held}, {"spent", pack.Spent}, {"expired", pack.Expired}, {"revoked", pack.Revoked},
			}
			total := int64(0)
			valid := pack.Issued >= 0 && pack.Issued <= common.MaxWalletQuota
			if !valid {
				differences = append(differences, CreditAccountDifference{Object: "pack", ID: pack.ID, Field: "issued_range", Expected: common.MaxWalletQuota, Actual: pack.Issued})
			}
			for _, quantity := range quantities {
				if quantity.amount < 0 || quantity.amount > common.MaxWalletQuota {
					valid = false
					bound := int64(common.MaxWalletQuota)
					if quantity.amount < 0 {
						bound = 0
					}
					differences = append(differences, CreditAccountDifference{Object: "pack", ID: pack.ID, Field: quantity.name + "_range", Expected: bound, Actual: quantity.amount})
					continue
				}
				// Five JavaScript-safe quantities fit in int64, even when their
				// stored sum breaks conservation. Report the whole sum.
				total += quantity.amount
			}
			if valid && total != pack.Issued {
				differences = append(differences, CreditAccountDifference{Object: "pack", ID: pack.ID, Field: "conservation", Expected: pack.Issued, Actual: total})
			}
			var ledger CreditLedgerEntry
			if err := tx.Model(&CreditLedgerEntry{}).Where("pack_id = ? AND user_id = ?", pack.ID, userID).Select("COALESCE(SUM(issued),0) AS issued, COALESCE(SUM(available),0) AS available, COALESCE(SUM(held),0) AS held, COALESCE(SUM(spent),0) AS spent, COALESCE(SUM(expired),0) AS expired, COALESCE(SUM(revoked),0) AS revoked").Scan(&ledger).Error; err != nil {
				return err
			}
			fields := []struct {
				name             string
				expected, actual int64
			}{
				{"issued", ledger.Issued, pack.Issued}, {"available", ledger.Available, pack.Available}, {"held", ledger.Held, pack.Held}, {"spent", ledger.Spent, pack.Spent}, {"expired", ledger.Expired, pack.Expired}, {"revoked", ledger.Revoked, pack.Revoked},
			}
			for _, field := range fields {
				if field.expected != field.actual {
					differences = append(differences, CreditAccountDifference{Object: "pack", ID: pack.ID, Field: field.name, Expected: field.expected, Actual: field.actual})
				}
			}
			var allocated int64
			if err := tx.Model(&CreditAllocation{}).Where("pack_id = ?", pack.ID).Select("COALESCE(SUM(amount-settled-released),0)").Scan(&allocated).Error; err != nil {
				return err
			}
			if allocated != pack.Held {
				differences = append(differences, CreditAccountDifference{Object: "pack", ID: pack.ID, Field: "allocated_hold", Expected: allocated, Actual: pack.Held})
			}
		}
		var requests []CreditRequest
		if err := tx.Where("user_id = ?", userID).Order("id asc").Find(&requests).Error; err != nil {
			return err
		}
		windowDifferences, err := reconcileSubscriptionWindowsTx(tx, userID, requests)
		if err != nil {
			return err
		}
		differences = append(differences, windowDifferences...)
		for _, request := range requests {
			if request.FundingSource == SubscriptionWindowFundingSource {
				continue
			}
			reservations, err := creditRequestReservationsTx(tx, request)
			if err != nil {
				differences = append(differences, CreditAccountDifference{Object: "request", ID: request.ID, Field: "reservation_links", Expected: request.Reserved, Actual: -1})
				continue
			}
			var held int64
			for _, reservation := range reservations {
				var amount int64
				if err := tx.Model(&CreditAllocation{}).Where("operation_id = ?", reservation.ReservationID).Select("COALESCE(SUM(amount-settled-released),0)").Scan(&amount).Error; err != nil {
					return err
				}
				held += amount
			}
			expected := request.Reserved
			if request.State == "settled" || request.State == "released" {
				expected = 0
			}
			if held != expected {
				differences = append(differences, CreditAccountDifference{Object: "request", ID: request.ID, Field: "held", Expected: expected, Actual: held})
			}
			if request.State == "settled" {
				if request.Charged < 0 || request.Uncollected < 0 || request.Charged > request.Actual || request.Uncollected != request.Actual-request.Charged {
					differences = append(differences, CreditAccountDifference{Object: "request", ID: request.ID, Field: "uncollected", Expected: request.Actual - request.Charged, Actual: request.Uncollected})
				}
				ids := make([]int64, 0, len(reservations)+1)
				for _, reservation := range reservations {
					ids = append(ids, reservation.ReservationID)
				}
				key, err := creditDigest([]string{"reserve", fmt.Sprintf("extra:%d", request.ID)})
				if err != nil {
					return err
				}
				var extra CreditOperation
				if err := tx.Where("user_id = ? AND key_digest = ? AND kind = ?", userID, key, "reserve").Limit(1).Find(&extra).Error; err != nil {
					return err
				}
				if extra.ID > 0 {
					ids = append(ids, extra.ID)
				}
				var settled int64
				if err := tx.Model(&CreditAllocation{}).Where("operation_id IN ?", ids).Select("COALESCE(SUM(settled),0)").Scan(&settled).Error; err != nil {
					return err
				}
				if settled != request.Charged {
					differences = append(differences, CreditAccountDifference{Object: "request", ID: request.ID, Field: "charged", Expected: settled, Actual: request.Charged})
				}
			}
		}
		return nil
	})
	return differences, err
}

// A retry authorizes another attempt at an already known operation. It cannot
// supply a new price, change the intent, or turn unknown usage into a refund.
type CreditRecoveryResume struct {
	UserID    int    `json:"user_id"`
	RequestID int64  `json:"request_id"`
	OutboxID  int64  `json:"outbox_id"`
	ActorID   int    `json:"-"`
	EventID   string `json:"event_id"`
	Reason    string `json:"reason"`
}

func ResumeCreditRecovery(db *gorm.DB, input CreditRecoveryResume, now int64) error {
	if db == nil || input.UserID <= 0 || input.RequestID < 0 || input.OutboxID < 0 || (input.RequestID > 0) == (input.OutboxID > 0) || !validCreditID(input.EventID, 128) || !validCreditID(input.Reason, 1024) || !validCreditTime(now) {
		return ErrCreditInvalid
	}
	digest, err := creditDigest([]string{"recovery_retry", input.EventID})
	if err != nil {
		return err
	}
	fingerprint, err := creditDigest(struct {
		Input   CreditRecoveryResume
		ActorID int
	}{input, input.ActorID})
	if err != nil {
		return err
	}
	return db.Transaction(func(tx *gorm.DB) error {
		if err := lockCreditAccount(tx, input.UserID, false); err != nil {
			return err
		}
		if err := AuthorizeCreditAccountAdmin(tx, input.ActorID, input.UserID); err != nil {
			return err
		}
		existing, err := findCreditOperation(tx, input.UserID, digest, fingerprint)
		if err != nil {
			return err
		}
		if existing != nil {
			return nil
		}
		var previous any
		if input.RequestID > 0 {
			var request CreditRequest
			if err := tx.Where("id = ? AND user_id = ?", input.RequestID, input.UserID).First(&request).Error; err != nil {
				return err
			}
			if request.State != "pending" || request.IntentKind == "" {
				return ErrCreditNeedsReview
			}
			if request.LeaseUntil > now {
				return ErrCreditLeaseLost
			}
			previous = map[string]any{"attempts": request.RecoveryAttempts, "last_error": request.LastRecoveryError, "intent": request.IntentKind, "actual": request.Actual}
			if err := tx.Model(&request).Updates(map[string]any{"recovery_blocked_at": 0, "next_recovery_at": 0}).Error; err != nil {
				return err
			}
		} else {
			var item CreditLogOutbox
			if err := tx.Where("id = ? AND user_id = ?", input.OutboxID, input.UserID).First(&item).Error; err != nil {
				return err
			}
			if item.State != "pending" && item.State != "review" {
				return ErrCreditOperationConflict
			}
			previous = map[string]any{"attempts": item.Attempts, "last_error": item.LastError}
			if err := tx.Model(&item).Updates(map[string]any{"state": "pending", "next_retry_at": 0}).Error; err != nil {
				return err
			}
		}
		audit, err := common.Marshal(map[string]any{"input": input, "actor_id": input.ActorID, "previous": previous})
		if err != nil {
			return err
		}
		return tx.Create(&CreditOperation{UserID: input.UserID, KeyDigest: digest, Fingerprint: fingerprint, Kind: "recovery_retry", CreatedAt: now, Result: string(audit)}).Error
	})
}

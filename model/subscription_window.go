package model

import (
	"database/sql/driver"
	"errors"

	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm"
)

const SubscriptionWindowFundingSource = "subscription_windows"

var ErrSubscriptionWindowInsufficient = errors.New("subscription window quota insufficient")
var ErrSubscriptionRightsUnavailable = errors.New("subscription rights unavailable or expired")

// Rules use existing New API cost quota. ID is a stable name within a purchased
// version; duration is a first-use period, not a natural calendar boundary.
type SubscriptionWindowRule struct {
	ID              string `json:"id"`
	DurationSeconds int64  `json:"duration_seconds"`
	Limit           int64  `json:"limit"`
}
type SubscriptionWindowRules []SubscriptionWindowRule

func (rules SubscriptionWindowRules) Value() (driver.Value, error) {
	if rules == nil {
		return "[]", nil
	}
	body, err := common.Marshal(rules)
	return string(body), err
}
func (rules *SubscriptionWindowRules) Scan(value any) error {
	*rules = make(SubscriptionWindowRules, 0)
	switch data := value.(type) {
	case nil:
		return nil
	case string:
		return common.UnmarshalJsonStr(data, rules)
	case []byte:
		return common.Unmarshal(data, rules)
	default:
		return ErrCreditInvalid
	}
}
func ValidateSubscriptionWindowRules(rules SubscriptionWindowRules) error {
	if len(rules) > 8 {
		return ErrCreditInvalid
	}
	seen := make(map[string]bool, len(rules))
	for _, rule := range rules {
		if !validCreditID(rule.ID, 32) || rule.ID == "term" || seen[rule.ID] || rule.DurationSeconds <= 0 || rule.DurationSeconds > 30*24*3600 || rule.Limit <= 0 || rule.Limit > common.MaxWalletQuota {
			return ErrCreditInvalid
		}
		for _, character := range rule.ID {
			if character < 'a' || character > 'z' {
				if (character < '0' || character > '9') && character != '-' && character != '_' {
					return ErrCreditInvalid
				}
			}
		}
		seen[rule.ID] = true
	}
	return nil
}

// Every generation remains available to late settlement. A void candidate was
// never submitted; it is retained as history rather than reused or deleted.
type SubscriptionWindow struct {
	ID             int64  `json:"id" gorm:"primaryKey"`
	UserID         int    `json:"user_id" gorm:"not null;index"`
	SubscriptionID int    `json:"subscription_id" gorm:"not null;uniqueIndex:,composite:subscription_window_generation,priority:1"`
	RuleID         string `json:"rule_id" gorm:"size:32;not null;uniqueIndex:,composite:subscription_window_generation,priority:2"`
	Generation     int64  `json:"generation" gorm:"not null;uniqueIndex:,composite:subscription_window_generation,priority:3"`
	StartsAt       int64  `json:"starts_at" gorm:"not null"`
	EndsAt         int64  `json:"ends_at" gorm:"not null"`
	Limit          int64  `json:"limit" gorm:"not null"`
	Held           int64  `json:"held" gorm:"not null"`
	Used           int64  `json:"used" gorm:"not null"`
	ReferenceUsed  int64  `json:"reference_used" gorm:"not null"`
	ConfirmedAt    int64  `json:"confirmed_at" gorm:"not null"`
	State          string `json:"state" gorm:"size:16;not null"`
}
type SubscriptionWindowAllocation struct {
	ID           int64 `json:"id" gorm:"primaryKey"`
	UserID       int   `json:"user_id" gorm:"not null;index"`
	RequestID    int64 `json:"request_id" gorm:"not null;uniqueIndex:,composite:subscription_window_allocation,priority:1"`
	WindowID     int64 `json:"window_id" gorm:"not null;uniqueIndex:,composite:subscription_window_allocation,priority:2"`
	Amount       int64 `json:"amount" gorm:"not null"`
	Settled      int64 `json:"settled" gorm:"not null"`
	Released     int64 `json:"released" gorm:"not null"`
	Supplemented int64 `json:"supplemented" gorm:"not null;default:0"`
}

// The caller already holds the account lock. Each subscription candidate uses
// a savepoint, so a failed window cannot leave another window partly reserved.
func reserveSubscriptionRequestTx(tx *gorm.DB, request *CreditRequest, amount, now int64) (bool, error) {
	if _, err := refreshVersionedSubscriptionRightsTx(tx, request.UserID, now); err != nil {
		return false, err
	}
	var terms []UserSubscription
	if err := tx.Where("user_id = ? AND plan_version_id > 0 AND status IN ? AND start_time <= ? AND end_time > ?", request.UserID, []string{"active", "scheduled"}, now, now).Order("end_time asc, id asc").Find(&terms).Error; err != nil {
		return false, err
	}
	allowWallet := true
	if len(terms) == 0 {
		return true, ErrSubscriptionRightsUnavailable
	}
	for _, term := range terms {
		allowWallet = allowWallet && term.AllowWalletOverflow
	}
	for _, term := range terms {
		err := tx.Transaction(func(candidate *gorm.DB) error {
			var contract SubscriptionPlan
			if err := common.UnmarshalJsonStr(term.ContractSnapshot, &contract); err != nil {
				return err
			}
			if err := ValidateSubscriptionWindowRules(contract.WindowRules); err != nil {
				return err
			}
			rules := append(SubscriptionWindowRules{{ID: "term", Limit: term.AmountTotal}}, contract.WindowRules...)
			for _, rule := range rules {
				var window SubscriptionWindow
				if err := candidate.Where("subscription_id = ? AND rule_id = ?", term.Id, rule.ID).Order("generation desc").Limit(1).Find(&window).Error; err != nil {
					return err
				}
				if window.ID == 0 || window.State == "void" || (window.ConfirmedAt > 0 && window.EndsAt <= now) {
					generation := window.Generation + 1
					if generation <= 0 || generation > common.MaxWalletQuota {
						return ErrCreditInvariant
					}
					limit := rule.Limit
					if rule.ID == "term" && limit == 0 {
						limit = common.MaxWalletQuota
					}
					window = SubscriptionWindow{UserID: request.UserID, SubscriptionID: term.Id, RuleID: rule.ID, Generation: generation, StartsAt: now, EndsAt: min(term.EndTime, now+rule.DurationSeconds), Limit: limit, State: "candidate"}
					if rule.ID == "term" {
						window.StartsAt, window.EndsAt, window.ConfirmedAt, window.State = term.StartTime, term.EndTime, term.StartTime, "active"
					}
					if err := candidate.Create(&window).Error; err != nil {
						return err
					}
				}
				if !window.quantitiesValid() {
					return ErrCreditInvariant
				}
				available := window.Limit - window.Used - window.Held
				if available < amount || available == 0 {
					return ErrSubscriptionWindowInsufficient
				}
				if err := candidate.Model(&window).Update("held", window.Held+amount).Error; err != nil {
					return err
				}
				if err := candidate.Create(&SubscriptionWindowAllocation{UserID: request.UserID, RequestID: request.ID, WindowID: window.ID, Amount: amount}).Error; err != nil {
					return err
				}
			}
			return nil
		})
		if err == nil {
			request.SubscriptionID, request.FundingSource = term.Id, SubscriptionWindowFundingSource
			return allowWallet, nil
		}
		if !errors.Is(err, ErrSubscriptionWindowInsufficient) {
			return false, err
		}
	}
	return allowWallet, ErrSubscriptionWindowInsufficient
}

func (window SubscriptionWindow) quantitiesValid() bool {
	return window.Limit > 0 && window.Limit <= common.MaxWalletQuota && window.Held >= 0 && window.Used >= 0 && window.Held <= window.Limit-window.Used && window.ReferenceUsed >= 0 && window.ReferenceUsed <= common.MaxWalletQuota
}

func subscriptionRequestAllocationsTx(tx *gorm.DB, request CreditRequest) ([]SubscriptionWindowAllocation, error) {
	var allocations []SubscriptionWindowAllocation
	if err := tx.Where("request_id = ? AND user_id = ?", request.ID, request.UserID).Order("window_id asc").Find(&allocations).Error; err != nil {
		return nil, err
	}
	if len(allocations) == 0 {
		return nil, ErrCreditInvariant
	}
	for _, allocation := range allocations {
		if allocation.Amount != request.Reserved || allocation.Settled < 0 || allocation.Released < 0 || allocation.Supplemented < 0 || allocation.Supplemented > common.MaxQuota || allocation.Settled > allocation.Amount+allocation.Supplemented || allocation.Released > allocation.Amount-min(allocation.Amount, allocation.Settled) {
			return nil, ErrCreditInvariant
		}
	}
	return allocations, nil
}

// Submission confirms the first actual use; only unconfirmed short windows
// move their candidate start. Existing active generations never shift.
func confirmSubscriptionRequestTx(tx *gorm.DB, request CreditRequest, now int64) error {
	var term UserSubscription
	if err := tx.Where("id = ? AND user_id = ?", request.SubscriptionID, request.UserID).First(&term).Error; err != nil {
		return err
	}
	if term.Status != "active" && term.Status != "scheduled" || term.StartTime > now || term.EndTime <= now {
		return ErrSubscriptionPurchaseUnavailable
	}
	allocations, err := subscriptionRequestAllocationsTx(tx, request)
	if err != nil {
		return err
	}
	var contract SubscriptionPlan
	if err := common.UnmarshalJsonStr(term.ContractSnapshot, &contract); err != nil {
		return err
	}
	durations := make(map[string]int64, len(contract.WindowRules))
	for _, rule := range contract.WindowRules {
		durations[rule.ID] = rule.DurationSeconds
	}
	for _, allocation := range allocations {
		var window SubscriptionWindow
		if err := tx.Where("id = ? AND subscription_id = ?", allocation.WindowID, term.Id).First(&window).Error; err != nil {
			return err
		}
		if window.State == "void" {
			return ErrCreditInvariant
		}
		if window.ConfirmedAt == 0 {
			duration := durations[window.RuleID]
			if duration <= 0 {
				return ErrCreditInvariant
			}
			if err := tx.Model(&window).Updates(map[string]any{"starts_at": now, "ends_at": min(term.EndTime, now+duration), "confirmed_at": now, "state": "active"}).Error; err != nil {
				return err
			}
		}
	}
	return nil
}

// Growth and extra settlement can only use the originally allocated windows.
// All must still be valid; no amount is taken from a later generation or pack.
func subscriptionRequestCapacityTx(tx *gorm.DB, request CreditRequest, allocations []SubscriptionWindowAllocation, now int64) (int64, error) {
	var term UserSubscription
	if err := tx.Where("id = ? AND user_id = ?", request.SubscriptionID, request.UserID).First(&term).Error; err != nil {
		return 0, err
	}
	if term.Status != "active" && term.Status != "scheduled" || term.StartTime > now || term.EndTime <= now {
		return 0, nil
	}
	available := int64(common.MaxQuota)
	for _, allocation := range allocations {
		var window SubscriptionWindow
		if err := tx.Where("id = ? AND subscription_id = ?", allocation.WindowID, term.Id).First(&window).Error; err != nil {
			return 0, err
		}
		if !window.quantitiesValid() || window.Held < allocation.Amount {
			return 0, ErrCreditInvariant
		}
		if window.State == "void" || (window.ConfirmedAt > 0 && window.EndsAt <= now) {
			return 0, nil
		}
		available = min(available, window.Limit-window.Used-window.Held)
	}
	return available, nil
}

func growSubscriptionRequestTx(tx *gorm.DB, request CreditRequest, target, now int64) error {
	allocations, err := subscriptionRequestAllocationsTx(tx, request)
	if err != nil {
		return err
	}
	capacity, err := subscriptionRequestCapacityTx(tx, request, allocations, now)
	if err != nil {
		return err
	}
	delta := target - request.Reserved
	if delta > capacity {
		return ErrSubscriptionWindowInsufficient
	}
	for _, allocation := range allocations {
		if err := tx.Model(&SubscriptionWindow{}).Where("id = ?", allocation.WindowID).Update("held", gorm.Expr("held + ?", delta)).Error; err != nil {
			return err
		}
		if err := tx.Model(&allocation).Update("amount", target).Error; err != nil {
			return err
		}
	}
	return nil
}

func finalizeSubscriptionRequestTx(tx *gorm.DB, request CreditRequest, maximum, now int64) (int64, error) {
	allocations, err := subscriptionRequestAllocationsTx(tx, request)
	if err != nil {
		return 0, err
	}
	capacity, err := subscriptionRequestCapacityTx(tx, request, allocations, now)
	if err != nil {
		return 0, err
	}
	charged := min(maximum, request.Reserved+capacity)
	for _, allocation := range allocations {
		if allocation.Settled != 0 || allocation.Released != 0 {
			return 0, ErrCreditInvariant
		}
		var window SubscriptionWindow
		if err := tx.First(&window, allocation.WindowID).Error; err != nil {
			return 0, err
		}
		if !window.quantitiesValid() || window.Held < allocation.Amount || request.Actual > common.MaxWalletQuota-window.ReferenceUsed {
			return 0, ErrCreditInvariant
		}
		held, used := window.Held-allocation.Amount, window.Used+charged
		state := window.State
		if window.ConfirmedAt == 0 && held == 0 && used == 0 {
			var others int64
			if err := tx.Model(&SubscriptionWindowAllocation{}).Where("window_id = ? AND request_id <> ? AND request_id IN (?)", window.ID, request.ID, tx.Model(&CreditRequest{}).Select("id").Where("user_id = ? AND state NOT IN ?", request.UserID, []string{"settled", "released"})).Count(&others).Error; err != nil {
				return 0, err
			}
			if others == 0 {
				state = "void"
			}
		}
		if used > window.Limit || held > window.Limit-used {
			return 0, ErrCreditInvariant
		}
		if err := tx.Model(&window).Updates(map[string]any{"held": held, "used": used, "reference_used": window.ReferenceUsed + request.Actual, "state": state}).Error; err != nil {
			return 0, err
		}
		// Extra charge has no original hold; the allocation retains its original
		// amount and stores the collected result separately.
		if err := tx.Model(&allocation).Updates(map[string]any{"settled": charged, "released": max(int64(0), allocation.Amount-charged), "supplemented": max(int64(0), charged-allocation.Amount)}).Error; err != nil {
			return 0, err
		}
	}
	if err := tx.Model(&UserSubscription{}).Where("id = ?", request.SubscriptionID).Update("amount_used", gorm.Expr("amount_used + ?", charged)).Error; err != nil {
		return 0, err
	}
	return charged, nil
}

// Reconciliation checks the authoritative windows and request allocations. It
// reports differences without resetting counters or manufacturing funding.
func reconcileSubscriptionWindowsTx(tx *gorm.DB, userID int, requests []CreditRequest) ([]CreditAccountDifference, error) {
	differences := make([]CreditAccountDifference, 0)
	byRequest := make(map[int64]CreditRequest, len(requests))
	for _, request := range requests {
		byRequest[request.ID] = request
	}
	var adjustments []CreditBillAdjustment
	if err := tx.Where("user_id = ?", userID).Order("request_id asc, revision asc").Find(&adjustments).Error; err != nil {
		return nil, err
	}
	byAdjustedRequest := make(map[int64][]CreditBillAdjustment)
	for _, adjustment := range adjustments {
		if _, known := byRequest[adjustment.RequestID]; !known {
			return nil, ErrCreditInvariant
		}
		byAdjustedRequest[adjustment.RequestID] = append(byAdjustedRequest[adjustment.RequestID], adjustment)
	}
	balances := make(map[int64]CreditBillBalance, len(byAdjustedRequest))
	for id, revisions := range byAdjustedRequest {
		balance, err := creditBillBalance(byRequest[id], revisions)
		if err != nil {
			return nil, err
		}
		balances[id] = balance
	}
	var windows []SubscriptionWindow
	if err := tx.Where("user_id = ?", userID).Find(&windows).Error; err != nil {
		return nil, err
	}
	byWindow := make(map[int64]SubscriptionWindow, len(windows))
	for _, window := range windows {
		byWindow[window.ID] = window
	}
	var allocations []SubscriptionWindowAllocation
	if err := tx.Where("user_id = ?", userID).Find(&allocations).Error; err != nil {
		return nil, err
	}
	type quantities struct{ held, used, reference int64 }
	totals := make(map[int64]quantities, len(windows))
	counts := make(map[int64]int64)
	termUsage := make(map[int]int64)
	for _, allocation := range allocations {
		request, ok := byRequest[allocation.RequestID]
		window, known := byWindow[allocation.WindowID]
		if !ok || !known || request.FundingSource != SubscriptionWindowFundingSource || request.SubscriptionID != window.SubscriptionID {
			differences = append(differences, CreditAccountDifference{Object: "window_allocation", ID: allocation.ID, Field: "ownership", Expected: request.ID, Actual: -1})
			continue
		}
		counts[request.ID]++
		terminal := request.State == "settled" || request.State == "released"
		valid := allocation.Amount == request.Reserved && allocation.Settled >= 0 && allocation.Supplemented >= 0 && allocation.Released >= 0 && allocation.Settled <= common.MaxQuota && allocation.Supplemented <= common.MaxQuota && allocation.Released <= allocation.Amount
		if terminal {
			valid = valid && allocation.Settled == request.Charged && allocation.Supplemented == max(int64(0), request.Charged-request.Reserved) && allocation.Released == max(int64(0), request.Reserved-request.Charged)
		} else {
			valid = valid && allocation.Settled == 0 && allocation.Supplemented == 0 && allocation.Released == 0
		}
		if !valid {
			differences = append(differences, CreditAccountDifference{Object: "window_allocation", ID: allocation.ID, Field: "request_quantities", Expected: request.Reserved, Actual: allocation.Amount})
			continue
		}
		total := totals[window.ID]
		held := allocation.Amount + allocation.Supplemented - allocation.Settled - allocation.Released
		reference := int64(0)
		settled := allocation.Settled
		if terminal {
			reference = request.Actual
			if balance, corrected := balances[request.ID]; corrected {
				settled, reference = balance.Charged, balance.ReferenceQuota
			}
		}
		if held < 0 || held > common.MaxWalletQuota-total.held || settled > common.MaxWalletQuota-total.used || reference < 0 || reference > common.MaxWalletQuota-total.reference {
			differences = append(differences, CreditAccountDifference{Object: "window", ID: window.ID, Field: "quantity_range", Expected: common.MaxWalletQuota, Actual: -1})
			continue
		}
		total.held += held
		total.used += settled
		total.reference += reference
		totals[window.ID] = total
	}
	for _, request := range requests {
		if request.FundingSource != SubscriptionWindowFundingSource {
			continue
		}
		charged := request.Charged
		if balance, corrected := balances[request.ID]; corrected {
			charged = balance.Charged
		}
		if charged < 0 || charged > common.MaxWalletQuota-termUsage[request.SubscriptionID] {
			return nil, ErrCreditInvariant
		}
		termUsage[request.SubscriptionID] += charged
		var term UserSubscription
		if err := tx.Where("id = ? AND user_id = ?", request.SubscriptionID, userID).First(&term).Error; err != nil {
			return nil, err
		}
		var contract SubscriptionPlan
		if err := common.UnmarshalJsonStr(term.ContractSnapshot, &contract); err != nil {
			return nil, err
		}
		expected := int64(len(contract.WindowRules) + 1)
		if counts[request.ID] != expected {
			differences = append(differences, CreditAccountDifference{Object: "request", ID: request.ID, Field: "window_links", Expected: expected, Actual: counts[request.ID]})
		}
		if request.Charged < 0 || request.Charged > request.Actual || request.Uncollected != request.Actual-request.Charged {
			if request.State == "settled" {
				differences = append(differences, CreditAccountDifference{Object: "request", ID: request.ID, Field: "uncollected", Expected: request.Actual - request.Charged, Actual: request.Uncollected})
			}
		}
	}
	var terms []UserSubscription
	if err := tx.Where("user_id = ? AND plan_version_id > 0", userID).Find(&terms).Error; err != nil {
		return nil, err
	}
	for _, term := range terms {
		if term.AmountUsed != termUsage[term.Id] {
			differences = append(differences, CreditAccountDifference{Object: "subscription", ID: int64(term.Id), Field: "amount_used", Expected: termUsage[term.Id], Actual: term.AmountUsed})
		}
	}
	for _, window := range windows {
		if !window.quantitiesValid() {
			differences = append(differences, CreditAccountDifference{Object: "window", ID: window.ID, Field: "quota_range", Expected: window.Limit, Actual: window.Held + window.Used})
		}
		total := totals[window.ID]
		for _, quantity := range []struct {
			name             string
			expected, actual int64
		}{{"held", total.held, window.Held}, {"used", total.used, window.Used}, {"reference_used", total.reference, window.ReferenceUsed}} {
			if quantity.expected != quantity.actual {
				differences = append(differences, CreditAccountDifference{Object: "window", ID: window.ID, Field: quantity.name, Expected: quantity.expected, Actual: quantity.actual})
			}
		}
	}
	return differences, nil
}

type SubscriptionWindowView struct {
	SubscriptionID int    `json:"subscription_id"`
	VersionID      int64  `json:"version_id"`
	RuleID         string `json:"rule_id"`
	Generation     int64  `json:"generation"`
	StartsAt       int64  `json:"starts_at"`
	EndsAt         int64  `json:"ends_at"`
	Limit          int64  `json:"limit"`
	Held           int64  `json:"held"`
	Used           int64  `json:"used"`
	ReferenceUsed  int64  `json:"reference_used"`
	Available      int64  `json:"available"`
	State          string `json:"state"`
	Unlimited      bool   `json:"unlimited"`
}

// Views project absent/ended short windows as unstarted without creating a
// generation. Merely opening the wallet page cannot start the user's clock.
func GetUserSubscriptionWindowViews(db *gorm.DB, userID int, now int64) ([]SubscriptionWindowView, error) {
	rights, err := GetUserSubscriptionRights(db, userID, now)
	if err != nil {
		return nil, err
	}
	views := make([]SubscriptionWindowView, 0)
	if len(rights) == 0 {
		return views, nil
	}
	ids := make([]int, 0, len(rights))
	for _, term := range rights {
		ids = append(ids, term.Id)
	}
	var windows []SubscriptionWindow
	if err := db.Where("user_id = ? AND subscription_id IN ?", userID, ids).Order("generation desc").Find(&windows).Error; err != nil {
		return nil, err
	}
	type windowKey struct {
		subscription int
		rule         string
	}
	latest := make(map[windowKey]SubscriptionWindow)
	for _, window := range windows {
		key := windowKey{window.SubscriptionID, window.RuleID}
		if _, ok := latest[key]; !ok {
			latest[key] = window
		}
	}
	for _, term := range rights {
		var contract SubscriptionPlan
		if err := common.UnmarshalJsonStr(term.ContractSnapshot, &contract); err != nil {
			return nil, err
		}
		rules := append(SubscriptionWindowRules{{ID: "term", Limit: term.AmountTotal}}, contract.WindowRules...)
		for _, rule := range rules {
			view := SubscriptionWindowView{SubscriptionID: term.Id, VersionID: term.PlanVersionID, RuleID: rule.ID, Limit: rule.Limit, Available: rule.Limit, State: "unstarted", Unlimited: rule.ID == "term" && rule.Limit == 0}
			if view.Unlimited {
				view.Available = common.MaxWalletQuota
			}
			if rule.ID == "term" {
				view.StartsAt, view.EndsAt, view.State = term.StartTime, term.EndTime, "active"
			}
			window := latest[windowKey{term.Id, rule.ID}]
			if window.ID > 0 && window.State != "void" && (window.ConfirmedAt == 0 || window.EndsAt > now) {
				if !window.quantitiesValid() {
					return nil, ErrCreditInvariant
				}
				view.Generation, view.Held, view.Used, view.ReferenceUsed, view.Available, view.State = window.Generation, window.Held, window.Used, window.ReferenceUsed, window.Limit-window.Held-window.Used, window.State
				if window.ConfirmedAt > 0 {
					view.StartsAt, view.EndsAt = window.StartsAt, window.EndsAt
				}
			}
			views = append(views, view)
		}
	}
	return views, nil
}

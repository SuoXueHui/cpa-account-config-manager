package manager

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"cpa-account-config-manager/internal/cpaapi"
)

const (
	adaptiveOverdraftQuotaThreshold = 99.0
	adaptiveOverdraftQuotaFreshness = 15 * time.Minute
	adaptiveOverdraftPersistDelay   = 2 * time.Second
	maxAdaptiveOverdraftRequests    = 10_000
)

type AdaptiveOverdraftPhase string
type AdaptiveOverdraftStrategy string

// AdaptiveOverdraftStrategyStats records sanitized lifecycle outcomes for
// requests that actually received an adaptive tool injection.
type AdaptiveOverdraftStrategyStats struct {
	Attempts  int64 `json:"attempts"`
	Successes int64 `json:"successes"`
	Failures  int64 `json:"failures"`
}

const (
	AdaptivePhaseIdle        AdaptiveOverdraftPhase = "idle"
	AdaptivePhaseArmed       AdaptiveOverdraftPhase = "armed"
	AdaptivePhaseActiveS1    AdaptiveOverdraftPhase = "active_s1"
	AdaptivePhaseActiveS2    AdaptiveOverdraftPhase = "active_s2"
	AdaptivePhaseActiveS4    AdaptiveOverdraftPhase = "active_s4"
	AdaptivePhaseExhausted   AdaptiveOverdraftPhase = "exhausted"
	AdaptivePhaseHardStopped AdaptiveOverdraftPhase = "hard_stopped"

	AdaptiveStrategyS1 AdaptiveOverdraftStrategy = "s1"
	AdaptiveStrategyS2 AdaptiveOverdraftStrategy = "s2"
	AdaptiveStrategyS4 AdaptiveOverdraftStrategy = "s4"
)

type adaptiveOverdraftRecord struct {
	Fingerprint              string                                                       `json:"fingerprint"`
	Phase                    AdaptiveOverdraftPhase                                       `json:"phase"`
	Strategy                 AdaptiveOverdraftStrategy                                    `json:"strategy,omitempty"`
	StrategyStats            map[AdaptiveOverdraftStrategy]AdaptiveOverdraftStrategyStats `json:"strategy_stats,omitempty"`
	PostThresholdSuccesses   int64                                                        `json:"post_threshold_successes"`
	PostThresholdTokens      int64                                                        `json:"post_threshold_tokens"`
	ConsecutiveQuotaFailures int                                                          `json:"consecutive_quota_failures"`
	LastSuccessAt            time.Time                                                    `json:"last_success_at,omitempty"`
	LastFailureAt            time.Time                                                    `json:"last_failure_at,omitempty"`
	LastObservedAt           time.Time                                                    `json:"last_observed_at,omitempty"`
	QuotaObservedAt          time.Time                                                    `json:"quota_observed_at,omitempty"`
	ResetAt                  time.Time                                                    `json:"reset_at,omitempty"`
	HardStopReason           string                                                       `json:"hard_stop_reason,omitempty"`
}

type AdaptiveWeeklyOverdraftSummary struct {
	Phase                  AdaptiveOverdraftPhase                                       `json:"phase"`
	Strategy               AdaptiveOverdraftStrategy                                    `json:"strategy,omitempty"`
	StrategyStats          map[AdaptiveOverdraftStrategy]AdaptiveOverdraftStrategyStats `json:"strategy_stats,omitempty"`
	PostThresholdSuccesses int64                                                        `json:"post_threshold_successes"`
	PostThresholdTokens    int64                                                        `json:"post_threshold_tokens"`
	LastSuccessAt          *time.Time                                                   `json:"last_success_at,omitempty"`
	LastFailureAt          *time.Time                                                   `json:"last_failure_at,omitempty"`
	ResetAt                *time.Time                                                   `json:"reset_at,omitempty"`
	HardStopReason         string                                                       `json:"hard_stop_reason,omitempty"`
}

type AdaptiveWeeklyOverdraftAccountState struct {
	AccountID              string                                                       `json:"account_id"`
	Phase                  AdaptiveOverdraftPhase                                       `json:"phase"`
	Strategy               AdaptiveOverdraftStrategy                                    `json:"strategy,omitempty"`
	StrategyStats          map[AdaptiveOverdraftStrategy]AdaptiveOverdraftStrategyStats `json:"strategy_stats,omitempty"`
	PostThresholdSuccesses int64                                                        `json:"post_threshold_successes"`
	PostThresholdTokens    int64                                                        `json:"post_threshold_tokens"`
	LastSuccessAt          *time.Time                                                   `json:"last_success_at,omitempty"`
	LastFailureAt          *time.Time                                                   `json:"last_failure_at,omitempty"`
	ResetAt                *time.Time                                                   `json:"reset_at,omitempty"`
	HardStopReason         string                                                       `json:"hard_stop_reason,omitempty"`
}

type AdaptiveWeeklyOverdraftManagementSnapshot struct {
	Available             bool                                  `json:"available"`
	HostSchemaVersion     uint32                                `json:"host_schema_version"`
	RequiredSchemaVersion uint32                                `json:"required_schema_version"`
	UnavailableReason     string                                `json:"unavailable_reason,omitempty"`
	StorageError          string                                `json:"storage_error,omitempty"`
	Accounts              []AdaptiveWeeklyOverdraftAccountState `json:"accounts"`
}

type adaptiveOverdraftRequest struct {
	Fingerprint string
	Strategy    AdaptiveOverdraftStrategy
	RecordedAt  time.Time
}

type AdaptiveWeeklyOverdraftExperiment struct {
	mu            sync.RWMutex
	enabled       func() bool
	now           func() time.Time
	records       map[string]adaptiveOverdraftRecord
	requests      map[string]adaptiveOverdraftRequest
	authIndex     map[string]string
	accountIDs    map[string]string
	store         string
	storageErr    string
	hostSchema    uint32
	configured    bool
	dirty         bool
	lastPersistAt time.Time
	persistDelay  time.Duration
	maxBodyBytes  int
	newCallID     func(AdaptiveOverdraftStrategy) (string, bool)
}

func NewAdaptiveWeeklyOverdraftExperiment(enabled func() bool) *AdaptiveWeeklyOverdraftExperiment {
	if enabled == nil {
		enabled = func() bool { return false }
	}
	return &AdaptiveWeeklyOverdraftExperiment{
		enabled: enabled, now: time.Now, records: make(map[string]adaptiveOverdraftRecord),
		requests: make(map[string]adaptiveOverdraftRequest), authIndex: make(map[string]string),
		accountIDs: make(map[string]string), persistDelay: adaptiveOverdraftPersistDelay,
		maxBodyBytes: defaultExperimentalRequestBodyLimit, newCallID: newAdaptiveExperimentalCallID,
	}
}

func (e *AdaptiveWeeklyOverdraftExperiment) Configure(config Config, hostSchema uint32) {
	if e == nil {
		return
	}
	config = normalizeConfig(config)
	now := e.currentTime()
	path := adaptiveWeeklyOverdraftStorePath(config.DataDir)
	hostSchema = normalizeHostSchemaVersion(hostSchema)
	records := make(map[string]adaptiveOverdraftRecord)
	storageErr := ""
	loaded, errLoad := loadAdaptiveWeeklyOverdraftState(path, now)
	if errLoad == nil {
		records = loaded
	} else if !errors.Is(errLoad, os.ErrNotExist) {
		storageErr = "adaptive weekly overdraft state could not be loaded"
	}
	e.mu.Lock()
	e.store = path
	e.hostSchema = hostSchema
	e.records = records
	e.requests = make(map[string]adaptiveOverdraftRequest)
	e.authIndex = make(map[string]string)
	e.accountIDs = make(map[string]string)
	e.storageErr = storageErr
	e.configured = true
	e.dirty = false
	e.lastPersistAt = now
	e.mu.Unlock()
}

func (e *AdaptiveWeeklyOverdraftExperiment) Close() {
	if e == nil {
		return
	}
	e.persist(true)
}

func (e *AdaptiveWeeklyOverdraftExperiment) ObserveAccounts(accounts []Account) {
	if e == nil {
		return
	}
	now := e.currentTime()
	changed := false
	e.mu.Lock()
	for _, account := range accounts {
		if !adaptiveAccountProviderEligible(account) {
			continue
		}
		fingerprint := adaptiveAuthFingerprint(account.AuthID)
		if fingerprint == "" {
			continue
		}
		if account.ID != "" {
			e.authIndex[account.ID] = fingerprint
			e.accountIDs[fingerprint] = account.ID
		}
		record, exists := e.records[fingerprint]
		if !exists {
			e.ensureRecordCapacityLocked(now)
			record = adaptiveOverdraftRecord{Fingerprint: fingerprint, Phase: AdaptivePhaseIdle}
			changed = true
		}
		if applyAdaptiveQuotaObservation(&record, account.Usage, now) {
			changed = true
		}
		e.records[fingerprint] = record
	}
	if e.pruneExpiredLocked(now) {
		changed = true
	}
	if changed {
		e.dirty = true
	}
	e.mu.Unlock()
	e.persist(false)
}

func (e *AdaptiveWeeklyOverdraftExperiment) ObserveUsage(record cpaapi.UsageRecord) {
	if e == nil || !strings.EqualFold(strings.TrimSpace(record.Provider), "codex") {
		return
	}
	now := e.currentTime()
	fingerprint := adaptiveAuthFingerprint(record.AuthID)
	e.mu.Lock()
	if fingerprint == "" {
		fingerprint = e.authIndex[strings.TrimSpace(record.AuthIndex)]
	}
	if fingerprint == "" {
		e.mu.Unlock()
		return
	}
	state, exists := e.records[fingerprint]
	if !exists {
		e.ensureRecordCapacityLocked(now)
		state = adaptiveOverdraftRecord{Fingerprint: fingerprint, Phase: AdaptivePhaseIdle}
	}
	changed := false
	if !record.Failed {
		if state.Phase == AdaptivePhaseHardStopped {
			resetAdaptiveOverdraftRecord(&state)
			changed = true
		}
		if codex := parseCodexUsageHeaders(record.ResponseHeaders, now); codex != nil {
			usage := &AccountUsageSnapshot{Codex: codex}
			if applyAdaptiveQuotaObservation(&state, usage, now) {
				changed = true
			}
		}
		if adaptiveRecordCountsPostThresholdSuccess(state, now) {
			state.PostThresholdSuccesses = saturatingAdd(state.PostThresholdSuccesses, 1)
			state.PostThresholdTokens = saturatingAdd(state.PostThresholdTokens, nonNegative(record.Detail.TotalTokens))
			state.LastSuccessAt = now
			changed = true
		}
	} else {
		evidence := classifyUsageFailure(record, now)
		state.LastFailureAt = now
		switch adaptiveFailureClass(record.Failure.StatusCode, evidence) {
		case "hard_stop":
			state.Phase = AdaptivePhaseHardStopped
			state.HardStopReason = adaptiveHardStopReason(record.Failure.StatusCode, evidence.ReasonCode)
			changed = true
		case "weekly_quota":
			if state.Phase == AdaptivePhaseIdle {
				state.Phase = AdaptivePhaseArmed
				state.Strategy = AdaptiveStrategyS1
			}
			state.ResetAt = evidence.RecoverAfter.UTC()
			state.QuotaObservedAt = now
			changed = true
		}
	}
	if changed {
		state.LastObservedAt = now
		e.records[fingerprint] = state
		e.dirty = true
	}
	e.mu.Unlock()
	e.persist(false)
}

func (e *AdaptiveWeeklyOverdraftExperiment) ObserveCompletion(completion cpaapi.RequestCompletion) {
	if e == nil {
		return
	}
	requestID := strings.TrimSpace(completion.RequestID)
	if requestID == "" {
		return
	}
	now := completion.CompletedAt.UTC()
	if now.IsZero() {
		now = e.currentTime()
	}
	e.mu.Lock()
	request, exists := e.requests[requestID]
	if !exists {
		e.mu.Unlock()
		return
	}
	delete(e.requests, requestID)
	state, exists := e.records[request.Fingerprint]
	if !exists {
		e.mu.Unlock()
		return
	}
	changed := false
	stats := state.StrategyStats[request.Strategy]
	if adaptiveCompletionSucceeded(completion) {
		stats.Successes = saturatingAdd(stats.Successes, 1)
		if state.Phase != AdaptivePhaseHardStopped && state.Phase != AdaptivePhaseExhausted && state.Strategy == request.Strategy {
			state.Phase = adaptiveActivePhase(request.Strategy)
			state.LastSuccessAt = now
			changed = true
		}
	} else {
		stats.Failures = saturatingAdd(stats.Failures, 1)
		usage := cpaapi.UsageRecord{Provider: "codex", Failed: true, Failure: cpaapi.UsageFailure{StatusCode: completion.StatusCode, Body: completion.Error}}
		evidence := classifyUsageFailure(usage, now)
		switch adaptiveFailureClass(completion.StatusCode, evidence) {
		case "hard_stop":
			state.Phase = AdaptivePhaseHardStopped
			state.HardStopReason = adaptiveHardStopReason(completion.StatusCode, evidence.ReasonCode)
			state.LastFailureAt = now
			changed = true
		case "weekly_quota":
			if state.Phase != AdaptivePhaseHardStopped && state.Phase != AdaptivePhaseExhausted && state.Strategy == request.Strategy {
				advanceAdaptiveStrategy(&state)
				state.LastFailureAt = now
				state.QuotaObservedAt = now
				changed = true
			}
		}
	}
	if state.StrategyStats == nil {
		state.StrategyStats = make(map[AdaptiveOverdraftStrategy]AdaptiveOverdraftStrategyStats)
	}
	state.StrategyStats[request.Strategy] = stats
	changed = true
	if changed {
		state.LastObservedAt = now
		e.records[request.Fingerprint] = state
		e.dirty = true
	}
	e.mu.Unlock()
	e.persist(false)
}

func (e *AdaptiveWeeklyOverdraftExperiment) recordRequest(requestID, authID string, strategy AdaptiveOverdraftStrategy, recordedAt time.Time) {
	if e == nil || !validAdaptiveStrategy(strategy) || strategy == "" {
		return
	}
	requestID = strings.TrimSpace(requestID)
	fingerprint := adaptiveAuthFingerprint(authID)
	if requestID == "" || fingerprint == "" {
		return
	}
	recordedAt = recordedAt.UTC()
	if recordedAt.IsZero() {
		recordedAt = e.currentTime()
	}
	e.mu.Lock()
	if _, exists := e.records[fingerprint]; !exists {
		e.ensureRecordCapacityLocked(recordedAt)
		e.records[fingerprint] = adaptiveOverdraftRecord{Fingerprint: fingerprint, Phase: AdaptivePhaseIdle}
	}
	record := e.records[fingerprint]
	if record.StrategyStats == nil {
		record.StrategyStats = make(map[AdaptiveOverdraftStrategy]AdaptiveOverdraftStrategyStats)
	}
	stats := record.StrategyStats[strategy]
	stats.Attempts = saturatingAdd(stats.Attempts, 1)
	record.StrategyStats[strategy] = stats
	record.LastObservedAt = recordedAt
	e.records[fingerprint] = record
	e.dirty = true
	if len(e.requests) >= maxAdaptiveOverdraftRequests {
		e.evictOldestRequestLocked()
	}
	e.requests[requestID] = adaptiveOverdraftRequest{Fingerprint: fingerprint, Strategy: strategy, RecordedAt: recordedAt}
	e.mu.Unlock()
	e.persist(false)
}

func (e *AdaptiveWeeklyOverdraftExperiment) stateForAuthID(authID string) (adaptiveOverdraftRecord, bool) {
	if e == nil {
		return adaptiveOverdraftRecord{}, false
	}
	fingerprint := adaptiveAuthFingerprint(authID)
	e.mu.RLock()
	record, exists := e.records[fingerprint]
	if exists {
		record.StrategyStats = cloneAdaptiveStrategyStats(record.StrategyStats)
	}
	e.mu.RUnlock()
	return record, exists
}

func cloneAdaptiveStrategyStats(source map[AdaptiveOverdraftStrategy]AdaptiveOverdraftStrategyStats) map[AdaptiveOverdraftStrategy]AdaptiveOverdraftStrategyStats {
	if len(source) == 0 {
		return nil
	}
	clone := make(map[AdaptiveOverdraftStrategy]AdaptiveOverdraftStrategyStats, len(source))
	for strategy, stats := range source {
		if !validAdaptiveStrategy(strategy) || strategy == "" {
			continue
		}
		clone[strategy] = stats
	}
	if len(clone) == 0 {
		return nil
	}
	return clone
}

func (e *AdaptiveWeeklyOverdraftExperiment) SummaryForAuthID(authID string) *AdaptiveWeeklyOverdraftSummary {
	record, exists := e.stateForAuthID(authID)
	if !exists || record.Phase == AdaptivePhaseIdle && record.PostThresholdSuccesses == 0 && record.PostThresholdTokens == 0 && len(record.StrategyStats) == 0 {
		return nil
	}
	return &AdaptiveWeeklyOverdraftSummary{
		Phase: record.Phase, Strategy: record.Strategy,
		StrategyStats:          cloneAdaptiveStrategyStats(record.StrategyStats),
		PostThresholdSuccesses: record.PostThresholdSuccesses, PostThresholdTokens: record.PostThresholdTokens,
		LastSuccessAt: adaptiveTimePointer(record.LastSuccessAt), LastFailureAt: adaptiveTimePointer(record.LastFailureAt),
		ResetAt: adaptiveTimePointer(record.ResetAt), HardStopReason: sanitizeAdaptiveHardStopReason(record.HardStopReason),
	}
}

func (e *AdaptiveWeeklyOverdraftExperiment) ManagementSnapshot() AdaptiveWeeklyOverdraftManagementSnapshot {
	snapshot := AdaptiveWeeklyOverdraftManagementSnapshot{
		RequiredSchemaVersion: cpaapi.SchemaVersion,
		Accounts:              []AdaptiveWeeklyOverdraftAccountState{},
	}
	if e == nil {
		snapshot.HostSchemaVersion = cpaapi.LegacySchemaVersion
		snapshot.UnavailableReason = "adaptive_weekly_overdraft_unavailable"
		return snapshot
	}
	e.mu.RLock()
	snapshot.HostSchemaVersion = normalizeHostSchemaVersion(e.hostSchema)
	snapshot.StorageError = e.storageErr
	snapshot.Available = e.configured && e.hostSchema >= cpaapi.SchemaVersion && e.storageErr == ""
	if e.hostSchema < cpaapi.SchemaVersion {
		snapshot.UnavailableReason = "host_schema_v2_required"
	} else if e.storageErr != "" {
		snapshot.UnavailableReason = "state_storage_unavailable"
	} else if !e.configured {
		snapshot.UnavailableReason = "adaptive_weekly_overdraft_unavailable"
	}
	for fingerprint, accountID := range e.accountIDs {
		accountID = safeOperationIdentifier(accountID, 256)
		record, exists := e.records[fingerprint]
		if !exists || accountID == "" {
			continue
		}
		snapshot.Accounts = append(snapshot.Accounts, AdaptiveWeeklyOverdraftAccountState{
			AccountID: accountID, Phase: record.Phase, Strategy: record.Strategy,
			StrategyStats:          cloneAdaptiveStrategyStats(record.StrategyStats),
			PostThresholdSuccesses: record.PostThresholdSuccesses, PostThresholdTokens: record.PostThresholdTokens,
			LastSuccessAt: adaptiveTimePointer(record.LastSuccessAt), LastFailureAt: adaptiveTimePointer(record.LastFailureAt),
			ResetAt: adaptiveTimePointer(record.ResetAt), HardStopReason: sanitizeAdaptiveHardStopReason(record.HardStopReason),
		})
	}
	e.mu.RUnlock()
	sort.Slice(snapshot.Accounts, func(left, right int) bool {
		return snapshot.Accounts[left].AccountID < snapshot.Accounts[right].AccountID
	})
	if len(snapshot.Accounts) > maxAdaptiveOverdraftAccounts {
		snapshot.Accounts = snapshot.Accounts[:maxAdaptiveOverdraftAccounts]
	}
	return snapshot
}

func (e *AdaptiveWeeklyOverdraftExperiment) AllowUsageAutoDisable(record cpaapi.UsageRecord, now time.Time) bool {
	if !e.RequestInterceptionActive() || !record.Failed {
		return true
	}
	evidence := classifyUsageFailure(record, now.UTC())
	return adaptiveFailureClass(record.Failure.StatusCode, evidence) != "weekly_quota"
}

func (e *AdaptiveWeeklyOverdraftExperiment) AllowInspectionAutoDisable(result InspectionResult) bool {
	if !e.RequestInterceptionActive() || result.ReasonCode != "quota_exhausted" && result.ReasonCode != "quota_limited" {
		return true
	}
	if result.QuotaWindow != InspectionQuotaWindowSevenDay && result.QuotaWindow != InspectionQuotaWindowMultiple {
		return true
	}
	return result.AutoDisableProbeStatus == InspectionAutoDisableProbeFailed
}

func (e *AdaptiveWeeklyOverdraftExperiment) AutomaticDisableProbePlan(account Account, result InspectionResult, preferredModel string) (AutomaticDisableProbePlan, bool) {
	if !e.RequestInterceptionActive() || adaptiveAuthFingerprint(account.AuthID) == "" ||
		(result.ReasonCode != "quota_exhausted" && result.ReasonCode != "quota_limited") ||
		(result.QuotaWindow != InspectionQuotaWindowSevenDay && result.QuotaWindow != InspectionQuotaWindowMultiple) ||
		!adaptiveAccountProviderEligible(account) {
		return AutomaticDisableProbePlan{}, false
	}
	return AutomaticDisableProbePlan{
		Name: "adaptive_weekly_overdraft", AttemptLimit: 9,
		Models:     []string{preferredModel, defaultCodexFallbackModel, codexCompatibilityMiniModel},
		Strategies: []AdaptiveOverdraftStrategy{AdaptiveStrategyS1, AdaptiveStrategyS2, AdaptiveStrategyS4},
		Request: ModelTestRequest{
			ExperimentalAdaptiveWeeklyOverdraft: true, Inspection: true, SelectPolicyFallback: true,
		},
		Observe: func(strategy AdaptiveOverdraftStrategy, probe ModelTestResult) {
			e.ObserveProbeResult(account.AuthID, strategy, probe)
		},
	}, true
}

func (e *AdaptiveWeeklyOverdraftExperiment) ObserveProbeResult(authID string, strategy AdaptiveOverdraftStrategy, result ModelTestResult) {
	if e == nil || adaptiveStrategyPairCount(strategy) == 0 {
		return
	}
	fingerprint := adaptiveAuthFingerprint(authID)
	if fingerprint == "" {
		return
	}
	e.observeProbeResult(fingerprint, strategy, result)
}

func (e *AdaptiveWeeklyOverdraftExperiment) ObserveProbeResultForAccountID(accountID string, strategy AdaptiveOverdraftStrategy, result ModelTestResult) {
	if e == nil || adaptiveStrategyPairCount(strategy) == 0 {
		return
	}
	e.mu.RLock()
	fingerprint := e.authIndex[strings.TrimSpace(accountID)]
	e.mu.RUnlock()
	if fingerprint == "" {
		return
	}
	e.observeProbeResult(fingerprint, strategy, result)
}

func (e *AdaptiveWeeklyOverdraftExperiment) observeProbeResult(fingerprint string, strategy AdaptiveOverdraftStrategy, result ModelTestResult) {
	now := result.TestedAt.UTC()
	if now.IsZero() {
		now = e.currentTime()
	}
	e.mu.Lock()
	record, exists := e.records[fingerprint]
	if !exists {
		e.ensureRecordCapacityLocked(now)
		record = adaptiveOverdraftRecord{Fingerprint: fingerprint, Phase: AdaptivePhaseArmed, Strategy: strategy}
	}
	changed := false
	switch {
	case result.Status == "available":
		record.Phase = adaptiveActivePhase(strategy)
		record.Strategy = strategy
		record.LastSuccessAt = now
		record.HardStopReason = ""
		changed = true
	case adaptiveProbeHardStop(result):
		record.Phase = AdaptivePhaseHardStopped
		record.HardStopReason = adaptiveHardStopReason(result.StatusCode, result.ReasonCode)
		record.LastFailureAt = now
		changed = true
	case adaptiveProbeDefinitiveQuota(result):
		if record.Phase != AdaptivePhaseHardStopped && record.Phase != AdaptivePhaseExhausted &&
			adaptiveStrategyRank(strategy) >= adaptiveStrategyRank(record.Strategy) {
			record.Strategy = strategy
			advanceAdaptiveStrategy(&record)
			record.LastFailureAt = now
			record.QuotaObservedAt = now
			changed = true
		}
	}
	if changed {
		record.LastObservedAt = now
		e.records[fingerprint] = record
		e.dirty = true
	}
	e.mu.Unlock()
	e.persist(false)
}

func adaptiveStrategyRank(strategy AdaptiveOverdraftStrategy) int {
	switch strategy {
	case AdaptiveStrategyS1:
		return 1
	case AdaptiveStrategyS2:
		return 2
	case AdaptiveStrategyS4:
		return 3
	default:
		return 0
	}
}

func adaptiveTimePointer(value time.Time) *time.Time {
	value = value.UTC()
	if value.IsZero() {
		return nil
	}
	return &value
}

func (e *AdaptiveWeeklyOverdraftExperiment) pruneExpired(now time.Time) {
	if e == nil {
		return
	}
	e.mu.Lock()
	if e.pruneExpiredLocked(now.UTC()) {
		e.dirty = true
	}
	e.mu.Unlock()
	e.persist(false)
}

func (e *AdaptiveWeeklyOverdraftExperiment) pruneExpiredLocked(now time.Time) bool {
	changed := false
	for fingerprint, record := range e.records {
		if record.Phase != AdaptivePhaseHardStopped && !record.ResetAt.IsZero() && !now.Before(record.ResetAt) {
			resetAdaptiveOverdraftRecord(&record)
			record.LastObservedAt = now
			e.records[fingerprint] = record
			changed = true
		}
		activity := adaptiveOverdraftRecordActivity(record)
		if !activity.IsZero() && activity.Before(now.Add(-adaptiveOverdraftRecordTTL)) {
			delete(e.records, fingerprint)
			delete(e.accountIDs, fingerprint)
			changed = true
		}
	}
	for requestID, request := range e.requests {
		if request.RecordedAt.Before(now.Add(-adaptiveOverdraftQuotaFreshness)) {
			delete(e.requests, requestID)
		}
	}
	return changed
}

func (e *AdaptiveWeeklyOverdraftExperiment) persist(force bool) {
	if e == nil {
		return
	}
	now := e.currentTime()
	e.mu.Lock()
	if !e.configured || !e.dirty || e.store == "" || e.storageErr != "" || !force && now.Sub(e.lastPersistAt) < e.persistDelay {
		e.mu.Unlock()
		return
	}
	path := e.store
	records := make(map[string]adaptiveOverdraftRecord, len(e.records))
	for key, record := range e.records {
		records[key] = record
	}
	e.lastPersistAt = now
	e.mu.Unlock()
	if errSave := saveAdaptiveWeeklyOverdraftState(path, records, now); errSave != nil {
		e.mu.Lock()
		e.storageErr = "adaptive weekly overdraft state could not be persisted"
		e.mu.Unlock()
		return
	}
	e.mu.Lock()
	e.dirty = false
	e.mu.Unlock()
}

func (e *AdaptiveWeeklyOverdraftExperiment) currentTime() time.Time {
	if e != nil && e.now != nil {
		return e.now().UTC()
	}
	return time.Now().UTC()
}

func (e *AdaptiveWeeklyOverdraftExperiment) ensureRecordCapacityLocked(now time.Time) {
	if len(e.records) < maxAdaptiveOverdraftAccounts {
		return
	}
	oldestFingerprint := ""
	oldestAt := now.Add(time.Second)
	for fingerprint, record := range e.records {
		activity := adaptiveOverdraftRecordActivity(record)
		if oldestFingerprint == "" || activity.Before(oldestAt) {
			oldestFingerprint = fingerprint
			oldestAt = activity
		}
	}
	delete(e.records, oldestFingerprint)
	delete(e.accountIDs, oldestFingerprint)
}

func (e *AdaptiveWeeklyOverdraftExperiment) evictOldestRequestLocked() {
	oldestID := ""
	var oldestAt time.Time
	for requestID, request := range e.requests {
		if oldestID == "" || request.RecordedAt.Before(oldestAt) {
			oldestID = requestID
			oldestAt = request.RecordedAt
		}
	}
	delete(e.requests, oldestID)
}

func adaptiveAuthFingerprint(authID string) string {
	encoded, ok := adaptiveAuthFingerprintKey(authID)
	if !ok {
		return ""
	}
	return string(encoded[:])
}

// adaptiveAuthFingerprintKey keeps request-path lookups stack-backed. Persistent
// state still converts this value to the existing hexadecimal string format.
func adaptiveAuthFingerprintKey(authID string) ([sha256.Size * 2]byte, bool) {
	var encoded [sha256.Size * 2]byte
	authID = strings.TrimSpace(authID)
	if authID == "" || len(authID) > 4096 {
		return encoded, false
	}
	sum := sha256.Sum256([]byte(authID))
	hex.Encode(encoded[:], sum[:])
	return encoded, true
}

func adaptiveAccountProviderEligible(account Account) bool {
	provider := strings.ToLower(strings.TrimSpace(firstNonEmpty(account.Provider, account.Type)))
	return provider == "codex" || isAgentIdentityProvider(provider)
}

func applyAdaptiveQuotaObservation(record *adaptiveOverdraftRecord, usage *AccountUsageSnapshot, now time.Time) bool {
	if record == nil || usage == nil || usage.Codex == nil || usage.Codex.SevenDay == nil {
		return false
	}
	observedAt := usage.Codex.ObservedAt.UTC()
	if observedAt.IsZero() || observedAt.After(now.Add(time.Minute)) || now.Sub(observedAt) > adaptiveOverdraftQuotaFreshness {
		return false
	}
	before := *record
	record.LastObservedAt = observedAt
	record.QuotaObservedAt = observedAt
	if usage.Codex.SevenDay.ResetAt != nil {
		record.ResetAt = usage.Codex.SevenDay.ResetAt.UTC()
	}
	if usage.Codex.SevenDay.UsedPercent < adaptiveOverdraftQuotaThreshold {
		resetAdaptiveOverdraftRecord(record)
		record.LastObservedAt = observedAt
		record.QuotaObservedAt = observedAt
		if usage.Codex.SevenDay.ResetAt != nil {
			record.ResetAt = usage.Codex.SevenDay.ResetAt.UTC()
		}
		return !adaptiveOverdraftRecordEqual(before, *record)
	}
	if record.Phase == AdaptivePhaseIdle || record.Phase == AdaptivePhaseHardStopped {
		record.Phase = AdaptivePhaseArmed
		record.Strategy = AdaptiveStrategyS1
		record.HardStopReason = ""
	}
	return !adaptiveOverdraftRecordEqual(before, *record)
}

func adaptiveOverdraftRecordEqual(left, right adaptiveOverdraftRecord) bool {
	if left.Fingerprint != right.Fingerprint || left.Phase != right.Phase || left.Strategy != right.Strategy ||
		left.PostThresholdSuccesses != right.PostThresholdSuccesses || left.PostThresholdTokens != right.PostThresholdTokens ||
		left.ConsecutiveQuotaFailures != right.ConsecutiveQuotaFailures || left.LastSuccessAt != right.LastSuccessAt ||
		left.LastFailureAt != right.LastFailureAt || left.LastObservedAt != right.LastObservedAt ||
		left.QuotaObservedAt != right.QuotaObservedAt || left.ResetAt != right.ResetAt || left.HardStopReason != right.HardStopReason {
		return false
	}
	if len(left.StrategyStats) != len(right.StrategyStats) {
		return false
	}
	for strategy, stats := range left.StrategyStats {
		if other, ok := right.StrategyStats[strategy]; !ok || other != stats {
			return false
		}
	}
	return true
}

func adaptiveRecordCountsPostThresholdSuccess(record adaptiveOverdraftRecord, now time.Time) bool {
	if record.Phase == AdaptivePhaseIdle || record.Phase == AdaptivePhaseHardStopped || record.Phase == AdaptivePhaseExhausted {
		return false
	}
	return !record.QuotaObservedAt.IsZero() && now.Sub(record.QuotaObservedAt) <= adaptiveOverdraftQuotaFreshness
}

func resetAdaptiveOverdraftRecord(record *adaptiveOverdraftRecord) {
	if record == nil {
		return
	}
	fingerprint := record.Fingerprint
	*record = adaptiveOverdraftRecord{Fingerprint: fingerprint, Phase: AdaptivePhaseIdle}
}

func advanceAdaptiveStrategy(record *adaptiveOverdraftRecord) {
	if record == nil {
		return
	}
	record.ConsecutiveQuotaFailures = min(record.ConsecutiveQuotaFailures+1, 1_000_000)
	switch record.Strategy {
	case AdaptiveStrategyS1:
		record.Phase = AdaptivePhaseArmed
		record.Strategy = AdaptiveStrategyS2
	case AdaptiveStrategyS2:
		record.Phase = AdaptivePhaseArmed
		record.Strategy = AdaptiveStrategyS4
	case AdaptiveStrategyS4:
		record.Phase = AdaptivePhaseExhausted
	default:
		record.Phase = AdaptivePhaseArmed
		record.Strategy = AdaptiveStrategyS1
	}
}

func adaptiveActivePhase(strategy AdaptiveOverdraftStrategy) AdaptiveOverdraftPhase {
	switch strategy {
	case AdaptiveStrategyS1:
		return AdaptivePhaseActiveS1
	case AdaptiveStrategyS2:
		return AdaptivePhaseActiveS2
	case AdaptiveStrategyS4:
		return AdaptivePhaseActiveS4
	default:
		return AdaptivePhaseArmed
	}
}

func adaptiveCompletionSucceeded(completion cpaapi.RequestCompletion) bool {
	if completion.StatusCode >= http.StatusOK && completion.StatusCode < http.StatusMultipleChoices {
		return true
	}
	return completion.StatusCode == 0 && strings.EqualFold(strings.TrimSpace(completion.Outcome), "success")
}

func adaptiveFailureClass(statusCode int, evidence inspectionEvidence) string {
	if statusCode == http.StatusPaymentRequired || statusCode == http.StatusUnauthorized || statusCode == http.StatusForbidden {
		return "hard_stop"
	}
	if evidence.ReasonCode == "quota_exhausted" {
		return "weekly_quota"
	}
	return "transient"
}

func adaptiveHardStopReason(statusCode int, reason string) string {
	if statusCode == http.StatusPaymentRequired {
		return "deactivated_workspace"
	}
	switch reason {
	case "workspace_deactivated", "account_deactivated":
		return "deactivated_workspace"
	case "token_revoked", "invalid_credentials", "authentication_review":
		return "authentication_failed"
	default:
		return sanitizeAdaptiveHardStopReason(reason)
	}
}

func sanitizeAdaptiveHardStopReason(reason string) string {
	switch strings.ToLower(strings.TrimSpace(reason)) {
	case "authentication_failed", "deactivated_workspace", "account_deactivated", "workspace_deactivated", "token_revoked", "invalid_credentials":
		return strings.ToLower(strings.TrimSpace(reason))
	default:
		return ""
	}
}

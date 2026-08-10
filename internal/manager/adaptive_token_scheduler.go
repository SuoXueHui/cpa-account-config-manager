package manager

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"
	"time"
	"unicode"

	"cpa-account-config-manager/internal/cpaapi"
)

const adaptiveTokenBindingTTL = 30 * time.Minute

type adaptiveTokenBinding struct {
	AuthID      string
	Fingerprint string
	ExpiresAt   time.Time
}

type adaptiveTokenCandidate struct {
	authID       string
	fingerprint  string
	priority     int
	strategyRank int
	successes    int64
	tokens       int64
}

func (e *AdaptiveWeeklyOverdraftExperiment) SetTokenDrainCanary(enabled func() bool, percent, maxSessions func() int) {
	if e == nil {
		return
	}
	if enabled == nil {
		enabled = func() bool { return false }
	}
	if percent == nil {
		percent = func() int { return defaultAdaptiveTokenDrainPercent }
	}
	if maxSessions == nil {
		maxSessions = func() int { return defaultAdaptiveTokenDrainMaxSessions }
	}
	e.drainEnabled = enabled
	e.drainPercent = percent
	e.drainSessions = maxSessions
}

func (e *AdaptiveWeeklyOverdraftExperiment) PickTokenDrainAuth(request cpaapi.SchedulerPickRequest) cpaapi.SchedulerPickResponse {
	if e == nil || !e.RequestInterceptionActive() || e.drainEnabled == nil || !e.drainEnabled() ||
		!strings.EqualFold(strings.TrimSpace(request.Provider), "codex") {
		return cpaapi.SchedulerPickResponse{}
	}
	sessionKey := adaptiveSchedulerSessionKey(request.Options)
	if sessionKey == "" || !adaptiveCanarySelected(sessionKey, e.tokenDrainPercent()) {
		return cpaapi.SchedulerPickResponse{}
	}
	sessionFingerprint := adaptiveSessionFingerprint(sessionKey)
	if sessionFingerprint == "" {
		return cpaapi.SchedulerPickResponse{}
	}
	now := e.currentTime()
	maxSessions := e.tokenDrainMaxSessions()

	e.mu.Lock()
	defer e.mu.Unlock()
	e.pruneTokenBindingsLocked(now)
	candidateByID := make(map[string]cpaapi.SchedulerAuthCandidate, len(request.Candidates))
	for _, candidate := range request.Candidates {
		candidate.ID = strings.TrimSpace(candidate.ID)
		if candidate.ID == "" || !strings.EqualFold(strings.TrimSpace(candidate.Provider), "codex") {
			continue
		}
		candidateByID[candidate.ID] = candidate
	}
	if binding, exists := e.bindings[sessionFingerprint]; exists {
		if _, offered := candidateByID[binding.AuthID]; offered && e.adaptiveDrainRecordEligibleLocked(binding.Fingerprint, now) {
			binding.ExpiresAt = now.Add(adaptiveTokenBindingTTL)
			e.bindings[sessionFingerprint] = binding
			return cpaapi.SchedulerPickResponse{AuthID: binding.AuthID, Handled: true}
		}
		delete(e.bindings, sessionFingerprint)
	}

	boundSessions := make(map[string]int, len(e.bindings))
	for _, binding := range e.bindings {
		boundSessions[binding.Fingerprint]++
	}
	eligible := make([]adaptiveTokenCandidate, 0, len(candidateByID))
	for authID, candidate := range candidateByID {
		fingerprint := adaptiveAuthFingerprint(authID)
		record, exists := e.records[fingerprint]
		if !exists || !adaptiveDrainRecordEligible(record, now) || boundSessions[fingerprint] >= maxSessions {
			continue
		}
		eligible = append(eligible, adaptiveTokenCandidate{
			authID: authID, fingerprint: fingerprint, priority: candidate.Priority,
			strategyRank: adaptiveStrategyRank(record.Strategy), successes: record.PostThresholdSuccesses, tokens: record.PostThresholdTokens,
		})
	}
	if len(eligible) == 0 {
		return cpaapi.SchedulerPickResponse{}
	}
	sort.Slice(eligible, func(left, right int) bool {
		leftProducing := eligible[left].tokens > 0
		rightProducing := eligible[right].tokens > 0
		if leftProducing != rightProducing {
			return leftProducing
		}
		if eligible[left].tokens != eligible[right].tokens {
			return eligible[left].tokens > eligible[right].tokens
		}
		if eligible[left].successes != eligible[right].successes {
			return eligible[left].successes > eligible[right].successes
		}
		if eligible[left].strategyRank != eligible[right].strategyRank {
			return eligible[left].strategyRank > eligible[right].strategyRank
		}
		if eligible[left].priority != eligible[right].priority {
			return eligible[left].priority > eligible[right].priority
		}
		return eligible[left].authID < eligible[right].authID
	})
	selected := eligible[0]
	e.bindings[sessionFingerprint] = adaptiveTokenBinding{
		AuthID: selected.authID, Fingerprint: selected.fingerprint, ExpiresAt: now.Add(adaptiveTokenBindingTTL),
	}
	return cpaapi.SchedulerPickResponse{AuthID: selected.authID, Handled: true}
}

func (e *AdaptiveWeeklyOverdraftExperiment) tokenDrainPercent() int {
	if e == nil || e.drainPercent == nil {
		return defaultAdaptiveTokenDrainPercent
	}
	return min(max(e.drainPercent(), 1), 100)
}

func (e *AdaptiveWeeklyOverdraftExperiment) tokenDrainMaxSessions() int {
	if e == nil || e.drainSessions == nil {
		return defaultAdaptiveTokenDrainMaxSessions
	}
	return min(max(e.drainSessions(), 1), 64)
}

func (e *AdaptiveWeeklyOverdraftExperiment) pruneTokenBindingsLocked(now time.Time) {
	for session, binding := range e.bindings {
		if !now.Before(binding.ExpiresAt) || !e.adaptiveDrainRecordEligibleLocked(binding.Fingerprint, now) {
			delete(e.bindings, session)
		}
	}
}

func (e *AdaptiveWeeklyOverdraftExperiment) adaptiveDrainRecordEligibleLocked(fingerprint string, now time.Time) bool {
	record, exists := e.records[fingerprint]
	return exists && adaptiveDrainRecordEligible(record, now)
}

func adaptiveDrainRecordEligible(record adaptiveOverdraftRecord, now time.Time) bool {
	if record.Phase == AdaptivePhaseIdle || record.Phase == AdaptivePhaseExhausted || record.Phase == AdaptivePhaseHardStopped ||
		record.Strategy == "" || !validAdaptiveStrategy(record.Strategy) {
		return false
	}
	if !record.ResetAt.IsZero() && !now.Before(record.ResetAt) {
		return false
	}
	return !record.QuotaObservedAt.IsZero() && !record.QuotaObservedAt.After(now.Add(time.Minute)) && now.Sub(record.QuotaObservedAt) <= adaptiveOverdraftQuotaFreshness
}

func adaptiveSchedulerSessionKey(options cpaapi.SchedulerOptions) string {
	// Keep the same explicit-header priority and source namespaces as CPA's
	// built-in session affinity so plugin bindings cannot merge unrelated clients.
	for _, source := range []struct {
		name   string
		prefix string
	}{
		{name: "X-Claude-Code-Session-Id", prefix: "claude:"},
		{name: "Session-Id", prefix: "codex:"},
		{name: "Session_id", prefix: "codex:"},
		{name: "X-Session-ID", prefix: "header:"},
		{name: "X-Session-Affinity", prefix: "affinity:"},
		{name: "X-Client-Request-Id", prefix: "clientreq:"},
	} {
		for key, values := range options.Headers {
			if !strings.EqualFold(strings.TrimSpace(key), source.name) {
				continue
			}
			for _, value := range values {
				if normalized := normalizeAdaptiveSessionKey(value); normalized != "" {
					return source.prefix + normalized
				}
			}
		}
	}
	for _, source := range []struct {
		name   string
		prefix string
	}{
		{name: "execution_session_id", prefix: "execution:"},
		{name: "derived_session_id", prefix: "derived:"},
		{name: "session_id", prefix: "session:"},
		{name: "sessionId", prefix: "session:"},
	} {
		if value, ok := options.Metadata[source.name].(string); ok {
			if normalized := normalizeAdaptiveSessionKey(value); normalized != "" {
				return source.prefix + normalized
			}
		}
	}
	return ""
}

func normalizeAdaptiveSessionKey(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 1024 || strings.IndexFunc(value, unicode.IsControl) >= 0 {
		return ""
	}
	return value
}

func adaptiveSessionFingerprint(value string) string {
	value = normalizeAdaptiveSessionKey(value)
	if value == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

package manager

import (
	"net/http"
	"testing"
	"time"

	"cpa-account-config-manager/internal/cpaapi"
)

func TestAdaptiveWeeklyOverdraftStateArmsAndEscalatesBoundedStrategies(t *testing.T) {
	now := time.Date(2026, 7, 29, 0, 0, 0, 0, time.UTC)
	engine := NewAdaptiveWeeklyOverdraftExperiment(func() bool { return true })
	engine.now = func() time.Time { return now }
	engine.ObserveAccounts([]Account{adaptiveTestAccount("account-1", "stable-auth-1", 99, now)})
	assertAdaptiveState(t, engine, "stable-auth-1", AdaptivePhaseArmed, AdaptiveStrategyS1)

	for index, expected := range []struct {
		strategy AdaptiveOverdraftStrategy
		phase    AdaptiveOverdraftPhase
	}{
		{AdaptiveStrategyS1, AdaptivePhaseArmed},
		{AdaptiveStrategyS2, AdaptivePhaseArmed},
		{AdaptiveStrategyS4, AdaptivePhaseExhausted},
	} {
		requestID := "request-" + string(rune('1'+index))
		engine.recordRequest(requestID, "stable-auth-1", expected.strategy, now)
		engine.ObserveCompletion(cpaapi.RequestCompletion{
			RequestID: requestID, StatusCode: http.StatusTooManyRequests,
			Error: `{"error":{"type":"usage_limit_reached"}}`, CompletedAt: now.Add(time.Duration(index+1) * time.Second),
		})
		state, ok := engine.stateForAuthID("stable-auth-1")
		if !ok || state.Phase != expected.phase {
			t.Fatalf("step %d state = %#v", index, state)
		}
		if index < 2 {
			next := []AdaptiveOverdraftStrategy{AdaptiveStrategyS2, AdaptiveStrategyS4}[index]
			if state.Strategy != next {
				t.Fatalf("step %d strategy = %q", index, state.Strategy)
			}
		}
	}
}

func TestAdaptiveWeeklyOverdraftStateSuccessAndUsageCounters(t *testing.T) {
	now := time.Date(2026, 7, 29, 1, 0, 0, 0, time.UTC)
	engine := NewAdaptiveWeeklyOverdraftExperiment(func() bool { return true })
	engine.now = func() time.Time { return now }
	engine.ObserveAccounts([]Account{adaptiveTestAccount("account-1", "stable-auth-1", 100, now)})
	engine.recordRequest("request-1", "stable-auth-1", AdaptiveStrategyS1, now)
	engine.ObserveCompletion(cpaapi.RequestCompletion{RequestID: "request-1", StatusCode: http.StatusOK, CompletedAt: now.Add(time.Second)})
	engine.ObserveUsage(cpaapi.UsageRecord{
		Provider: "codex", AuthID: "stable-auth-1", AuthIndex: "account-1", RequestedAt: now,
		Detail: cpaapi.UsageDetail{TotalTokens: 42},
		ResponseHeaders: http.Header{
			"X-Codex-Secondary-Used-Percent":   {"100"},
			"X-Codex-Secondary-Window-Minutes": {"10080"},
		},
	})
	state, ok := engine.stateForAuthID("stable-auth-1")
	if !ok || state.Phase != AdaptivePhaseActiveS1 || state.PostThresholdSuccesses != 1 || state.PostThresholdTokens != 42 {
		t.Fatalf("state = %#v", state)
	}
}

func TestAdaptiveWeeklyOverdraftStateIgnoresGenericRateLimitAndLateCompletion(t *testing.T) {
	now := time.Date(2026, 7, 29, 2, 0, 0, 0, time.UTC)
	engine := NewAdaptiveWeeklyOverdraftExperiment(func() bool { return true })
	engine.now = func() time.Time { return now }
	engine.ObserveAccounts([]Account{adaptiveTestAccount("account-1", "stable-auth-1", 100, now)})
	engine.recordRequest("old", "stable-auth-1", AdaptiveStrategyS1, now)
	engine.recordRequest("current", "stable-auth-1", AdaptiveStrategyS1, now)
	engine.ObserveCompletion(cpaapi.RequestCompletion{RequestID: "current", StatusCode: http.StatusTooManyRequests, Error: `{"error":{"type":"usage_limit_reached"}}`})
	assertAdaptiveState(t, engine, "stable-auth-1", AdaptivePhaseArmed, AdaptiveStrategyS2)

	engine.ObserveCompletion(cpaapi.RequestCompletion{RequestID: "old", StatusCode: http.StatusOK})
	assertAdaptiveState(t, engine, "stable-auth-1", AdaptivePhaseArmed, AdaptiveStrategyS2)

	engine.recordRequest("generic", "stable-auth-1", AdaptiveStrategyS2, now)
	engine.ObserveCompletion(cpaapi.RequestCompletion{RequestID: "generic", StatusCode: http.StatusTooManyRequests, Error: `{"error":{"type":"rate_limit"}}`})
	assertAdaptiveState(t, engine, "stable-auth-1", AdaptivePhaseArmed, AdaptiveStrategyS2)
}

func TestAdaptiveWeeklyOverdraftStateHardStopAndRecoveryRules(t *testing.T) {
	now := time.Date(2026, 7, 29, 3, 0, 0, 0, time.UTC)
	resetAt := now.Add(time.Minute)
	engine := NewAdaptiveWeeklyOverdraftExperiment(func() bool { return true })
	engine.now = func() time.Time { return now }
	account := adaptiveTestAccount("account-1", "stable-auth-1", 100, now)
	account.Usage.Codex.SevenDay.ResetAt = &resetAt
	engine.ObserveAccounts([]Account{account})
	engine.recordRequest("request-1", "stable-auth-1", AdaptiveStrategyS1, now)
	engine.ObserveCompletion(cpaapi.RequestCompletion{
		RequestID: "request-1", StatusCode: http.StatusPaymentRequired,
		Error: `{"error":{"type":"deactivated_workspace"}}`, CompletedAt: now,
	})
	state, ok := engine.stateForAuthID("stable-auth-1")
	if !ok || state.Phase != AdaptivePhaseHardStopped || state.HardStopReason != "deactivated_workspace" {
		t.Fatalf("hard-stop state = %#v", state)
	}

	now = resetAt.Add(time.Second)
	engine.pruneExpired(now)
	state, _ = engine.stateForAuthID("stable-auth-1")
	if state.Phase != AdaptivePhaseHardStopped {
		t.Fatalf("reset timestamp cleared hard stop = %#v", state)
	}

	engine.ObserveUsage(cpaapi.UsageRecord{Provider: "codex", AuthID: "stable-auth-1", AuthIndex: "account-1", RequestedAt: now})
	assertAdaptiveState(t, engine, "stable-auth-1", AdaptivePhaseIdle, "")
}

func TestAdaptiveWeeklyOverdraftStateHardStopsOnHTTP401WithoutBodyDetails(t *testing.T) {
	now := time.Date(2026, 7, 29, 3, 30, 0, 0, time.UTC)
	engine := NewAdaptiveWeeklyOverdraftExperiment(func() bool { return true })
	engine.now = func() time.Time { return now }
	engine.ObserveAccounts([]Account{adaptiveTestAccount("account-1", "stable-auth-1", 100, now)})
	engine.recordRequest("request-1", "stable-auth-1", AdaptiveStrategyS1, now)
	engine.ObserveCompletion(cpaapi.RequestCompletion{RequestID: "request-1", StatusCode: http.StatusUnauthorized, CompletedAt: now})
	state, ok := engine.stateForAuthID("stable-auth-1")
	if !ok || state.Phase != AdaptivePhaseHardStopped || state.HardStopReason != "authentication_failed" {
		t.Fatalf("state = %#v", state)
	}
}

func TestAdaptiveWeeklyOverdraftStateFreshQuotaObservationControlsArming(t *testing.T) {
	now := time.Date(2026, 7, 29, 4, 0, 0, 0, time.UTC)
	engine := NewAdaptiveWeeklyOverdraftExperiment(func() bool { return true })
	engine.now = func() time.Time { return now }
	engine.ObserveAccounts([]Account{adaptiveTestAccount("fresh", "fresh-auth", 99, now.Add(-14*time.Minute))})
	assertAdaptiveState(t, engine, "fresh-auth", AdaptivePhaseArmed, AdaptiveStrategyS1)

	engine.ObserveAccounts([]Account{adaptiveTestAccount("stale", "stale-auth", 100, now.Add(-16*time.Minute))})
	assertAdaptiveState(t, engine, "stale-auth", AdaptivePhaseIdle, "")

	engine.ObserveAccounts([]Account{adaptiveTestAccount("below", "below-auth", 98.9, now)})
	assertAdaptiveState(t, engine, "below-auth", AdaptivePhaseIdle, "")
}

func adaptiveTestAccount(accountID, authID string, usedPercent float64, observedAt time.Time) Account {
	return Account{
		ID: accountID, AuthID: authID, Provider: "codex", Type: "codex",
		Usage: &AccountUsageSnapshot{Codex: &CodexUsageSnapshot{
			ObservedAt: observedAt, SevenDay: &UsageWindowSnapshot{UsedPercent: usedPercent, WindowMinutes: 10080},
		}},
	}
}

func assertAdaptiveState(t *testing.T, engine *AdaptiveWeeklyOverdraftExperiment, authID string, phase AdaptiveOverdraftPhase, strategy AdaptiveOverdraftStrategy) {
	t.Helper()
	state, ok := engine.stateForAuthID(authID)
	if !ok {
		t.Fatalf("state for %q not found", authID)
	}
	if state.Phase != phase || state.Strategy != strategy {
		t.Fatalf("state = %#v, want phase=%q strategy=%q", state, phase, strategy)
	}
}

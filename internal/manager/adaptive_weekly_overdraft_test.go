package manager

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"cpa-account-config-manager/internal/cpaapi"
)

func TestAppWiresAdaptiveOverdraftAfterConcurrencyAndOriginal(t *testing.T) {
	app := NewApp(&fakeAuthHost{}, []byte("index"))
	defer app.Close()
	if len(app.requestHooks.transformers) != 3 {
		t.Fatalf("transformers = %d", len(app.requestHooks.transformers))
	}
	if _, ok := app.requestHooks.transformers[0].(*AccountConcurrencyService); !ok {
		t.Fatalf("first transformer = %T", app.requestHooks.transformers[0])
	}
	if _, ok := app.requestHooks.transformers[1].(*WeeklyOverdraftExperiment); !ok {
		t.Fatalf("second transformer = %T", app.requestHooks.transformers[1])
	}
	if _, ok := app.requestHooks.transformers[2].(*AdaptiveWeeklyOverdraftExperiment); !ok {
		t.Fatalf("third transformer = %T", app.requestHooks.transformers[2])
	}
}

func TestAppForwardsAdaptiveUsageAndCompletionAfterConfiguration(t *testing.T) {
	now := time.Date(2026, 7, 29, 9, 0, 0, 0, time.UTC)
	app := NewApp(&fakeAuthHost{}, []byte("index"))
	defer app.Close()
	app.ConfigureHost([]byte("data_dir: "+t.TempDir()+"\nexperimental_settings:\n  adaptive_weekly_overdraft_enabled: true\n"), cpaapi.SchemaVersion)
	app.adaptiveOverdraft.now = func() time.Time { return now }
	app.adaptiveOverdraft.ObserveAccounts([]Account{adaptiveTestAccount("account-1", "stable-auth-1", 100, now)})
	app.adaptiveOverdraft.recordRequest("request-1", "stable-auth-1", AdaptiveStrategyS1, now)
	app.HandleRequestComplete(cpaapi.RequestCompletion{RequestID: "request-1", StatusCode: http.StatusOK, CompletedAt: now})
	app.HandleUsage(cpaapi.UsageRecord{
		Provider: "codex", AuthID: "stable-auth-1", AuthIndex: "account-1", RequestedAt: now,
		Detail:          cpaapi.UsageDetail{TotalTokens: 77},
		ResponseHeaders: http.Header{"X-Codex-Secondary-Used-Percent": {"100"}, "X-Codex-Secondary-Window-Minutes": {"10080"}},
	})
	state, ok := app.adaptiveOverdraft.stateForAuthID("stable-auth-1")
	if !ok || state.Phase != AdaptivePhaseActiveS1 || state.PostThresholdSuccesses != 1 || state.PostThresholdTokens != 77 {
		t.Fatalf("state = %#v", state)
	}
}

func TestAppLegacySchemaKeepsAdaptiveRequestLifecycleInactive(t *testing.T) {
	app := NewApp(&fakeAuthHost{}, []byte("index"))
	defer app.Close()
	app.ConfigureHost([]byte("data_dir: "+t.TempDir()+"\nexperimental_settings:\n  adaptive_weekly_overdraft_enabled: true\n"), cpaapi.LegacySchemaVersion)
	if app.RequestInterceptionActive() || app.RequestCompletionActive() {
		t.Fatal("legacy host activated adaptive request lifecycle")
	}
}

func TestAccountListProjectsSanitizedAdaptiveOverdraftState(t *testing.T) {
	now := time.Date(2026, 7, 29, 10, 0, 0, 0, time.UTC)
	host := &fakeAuthHost{entries: []cpaapi.HostAuthFileEntry{{
		ID: "stable-auth-1", AuthIndex: "account-1", Name: "account-1.json", Provider: "codex", Type: "codex", Source: "memory",
	}}}
	usage := adaptiveUsageReader{"account-1": adaptiveTestAccount("account-1", "stable-auth-1", 100, now).Usage}
	engine := NewAdaptiveWeeklyOverdraftExperiment(func() bool { return true })
	engine.now = func() time.Time { return now }
	engine.Configure(Config{DataDir: t.TempDir()}, cpaapi.SchemaVersion)
	service := NewAccountService(host, usage)
	service.SetObserver(engine)
	service.SetAdaptiveWeeklyOverdraft(engine)
	response, errList := service.List(context.Background(), ListQuery{Page: 1, PageSize: 10})
	if errList != nil {
		t.Fatalf("List() error = %v", errList)
	}
	if len(response.Accounts) != 1 || response.Accounts[0].AdaptiveWeeklyOverdraft == nil {
		t.Fatalf("accounts = %#v", response.Accounts)
	}
	summary := response.Accounts[0].AdaptiveWeeklyOverdraft
	if summary.Phase != AdaptivePhaseArmed || summary.Strategy != AdaptiveStrategyS1 {
		t.Fatalf("summary = %#v", summary)
	}
	raw, errMarshal := json.Marshal(summary)
	if errMarshal != nil {
		t.Fatalf("Marshal() error = %v", errMarshal)
	}
	if strings.Contains(string(raw), "stable-auth-1") || strings.Contains(string(raw), adaptiveAuthFingerprint("stable-auth-1")) {
		t.Fatalf("summary leaked identity = %s", raw)
	}
}

func TestAdaptiveWeeklyOverdraftManagementReturnsSanitizedAccountState(t *testing.T) {
	now := time.Date(2026, 7, 29, 10, 30, 0, 0, time.UTC)
	app := NewApp(&fakeAuthHost{}, []byte("index"))
	defer app.Close()
	app.ConfigureHost([]byte("data_dir: "+t.TempDir()+"\nexperimental_settings:\n  adaptive_weekly_overdraft_enabled: true\n"), cpaapi.SchemaVersion)
	app.adaptiveOverdraft.now = func() time.Time { return now }
	app.adaptiveOverdraft.ObserveAccounts([]Account{adaptiveTestAccount("account-1", "secret-stable-auth", 100, now)})
	app.adaptiveOverdraft.mu.Lock()
	record := app.adaptiveOverdraft.records[adaptiveAuthFingerprint("secret-stable-auth")]
	record.Phase = AdaptivePhaseActiveS2
	record.Strategy = AdaptiveStrategyS2
	record.StrategyStats = map[AdaptiveOverdraftStrategy]AdaptiveOverdraftStrategyStats{
		AdaptiveStrategyS2: {Attempts: 4, Successes: 3, Failures: 1},
	}
	record.PostThresholdSuccesses = 183
	record.PostThresholdTokens = 14_800_000
	app.adaptiveOverdraft.records[record.Fingerprint] = record
	app.adaptiveOverdraft.mu.Unlock()

	response := app.HandleManagement(context.Background(), cpaapi.ManagementRequest{
		Method: http.MethodGet,
		Path:   "/v0/management/plugins/cpa-account-config-manager/experiments/adaptive-weekly-overdraft",
	})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d body=%s", response.StatusCode, response.Body)
	}
	var snapshot AdaptiveWeeklyOverdraftManagementSnapshot
	if errDecode := json.Unmarshal(response.Body, &snapshot); errDecode != nil {
		t.Fatalf("decode response: %v", errDecode)
	}
	if !snapshot.Available || snapshot.HostSchemaVersion != cpaapi.SchemaVersion || snapshot.RequiredSchemaVersion != cpaapi.SchemaVersion || len(snapshot.Accounts) != 1 {
		t.Fatalf("snapshot = %#v", snapshot)
	}
	account := snapshot.Accounts[0]
	if account.AccountID != "account-1" || account.Phase != AdaptivePhaseActiveS2 || account.Strategy != AdaptiveStrategyS2 || account.PostThresholdSuccesses != 183 || account.PostThresholdTokens != 14_800_000 {
		t.Fatalf("account = %#v", account)
	}
	if stats := account.StrategyStats[AdaptiveStrategyS2]; stats.Attempts != 4 || stats.Successes != 3 || stats.Failures != 1 {
		t.Fatalf("strategy stats = %#v", account.StrategyStats)
	}
	serialized := string(response.Body)
	if strings.Contains(serialized, "secret-stable-auth") || strings.Contains(serialized, adaptiveAuthFingerprint("secret-stable-auth")) || strings.Contains(serialized, "RequestID") {
		t.Fatalf("response leaked internal identity = %s", serialized)
	}
}

func TestAdaptiveWeeklyOverdraftManagementReportsLegacyUnavailable(t *testing.T) {
	app := NewApp(&fakeAuthHost{}, []byte("index"))
	defer app.Close()
	app.ConfigureHost([]byte("data_dir: "+t.TempDir()+"\n"), cpaapi.LegacySchemaVersion)
	response := app.HandleManagement(context.Background(), cpaapi.ManagementRequest{
		Method: http.MethodGet,
		Path:   "/v0/management/plugins/cpa-account-config-manager/experiments/adaptive-weekly-overdraft",
	})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d body=%s", response.StatusCode, response.Body)
	}
	var snapshot AdaptiveWeeklyOverdraftManagementSnapshot
	if errDecode := json.Unmarshal(response.Body, &snapshot); errDecode != nil {
		t.Fatalf("decode response: %v", errDecode)
	}
	if snapshot.Available || snapshot.UnavailableReason != "host_schema_v2_required" {
		t.Fatalf("snapshot = %#v", snapshot)
	}
}

func TestAdaptiveAutomaticDisableProbeUsesFallbackWithoutConsumingStrategy(t *testing.T) {
	now := time.Date(2026, 7, 29, 11, 0, 0, 0, time.UTC)
	calls := make([]ModelTestRequest, 0, 3)
	result := executeAutomaticDisableProbePlan(context.Background(), func(_ context.Context, request ModelTestRequest, _, _ string) (ModelTestResult, error) {
		calls = append(calls, request)
		probe := ModelTestResult{
			AccountID: request.AccountID, Model: request.Model, Status: "review", ReasonCode: "quota_limited",
			StatusCode: http.StatusTooManyRequests, TestedAt: now,
			Experiment: &ModelTestExperiment{Name: "adaptive_weekly_overdraft", Applied: true, Strategy: request.AdaptiveWeeklyOverdraftStrategy},
		}
		if len(calls) == 1 {
			probe.Status = "unsupported"
			probe.ReasonCode = "model_not_found"
			probe.StatusCode = http.StatusBadRequest
		}
		if len(calls) == 3 {
			probe.Status = "available"
			probe.ReasonCode = "model_response_ok"
			probe.StatusCode = http.StatusOK
		}
		return probe, nil
	}, Account{ID: "account-1"}, InspectionResult{}, AutomaticDisableProbePlan{
		Name: "adaptive_weekly_overdraft", AttemptLimit: 6, Models: []string{"preferred", "fallback"},
		Strategies: []AdaptiveOverdraftStrategy{AdaptiveStrategyS1, AdaptiveStrategyS2, AdaptiveStrategyS4},
		Request:    ModelTestRequest{ExperimentalAdaptiveWeeklyOverdraft: true, Inspection: true},
	}, "http://127.0.0.1:8317", "management-key", func() time.Time { return now })
	if result.AutoDisableProbeStatus != InspectionAutoDisableProbePassed || result.AutoDisableProbeAttempts != 3 {
		t.Fatalf("result = %#v", result)
	}
	if len(calls) != 3 || calls[0].AdaptiveWeeklyOverdraftStrategy != AdaptiveStrategyS1 || calls[1].AdaptiveWeeklyOverdraftStrategy != AdaptiveStrategyS1 || calls[2].AdaptiveWeeklyOverdraftStrategy != AdaptiveStrategyS2 {
		t.Fatalf("calls = %#v", calls)
	}
}

func TestAdaptiveAutomaticDisableProbeStopsOnHardFailure(t *testing.T) {
	now := time.Date(2026, 7, 29, 11, 30, 0, 0, time.UTC)
	calls := 0
	result := executeAutomaticDisableProbePlan(context.Background(), func(_ context.Context, request ModelTestRequest, _, _ string) (ModelTestResult, error) {
		calls++
		return ModelTestResult{
			AccountID: request.AccountID, Model: request.Model, Status: "unavailable", ReasonCode: "authentication_failed",
			StatusCode: http.StatusUnauthorized, TestedAt: now,
			Experiment: &ModelTestExperiment{Name: "adaptive_weekly_overdraft", Applied: true, Strategy: request.AdaptiveWeeklyOverdraftStrategy},
		}, nil
	}, Account{ID: "account-1"}, InspectionResult{}, AutomaticDisableProbePlan{
		Name: "adaptive_weekly_overdraft", AttemptLimit: 9, Models: []string{"preferred", "fallback", "compatibility"},
		Strategies: []AdaptiveOverdraftStrategy{AdaptiveStrategyS1, AdaptiveStrategyS2, AdaptiveStrategyS4},
		Request:    ModelTestRequest{ExperimentalAdaptiveWeeklyOverdraft: true, Inspection: true},
	}, "http://127.0.0.1:8317", "management-key", func() time.Time { return now })
	if calls != 1 || result.AutoDisableProbeStatus != InspectionAutoDisableProbeFailed || result.AutoDisableProbeReasonCode != "authentication_failed" {
		t.Fatalf("calls=%d result=%#v", calls, result)
	}
}

func TestAdaptiveAutomaticDisableProbeLeavesTransientFailureInconclusive(t *testing.T) {
	now := time.Date(2026, 7, 29, 11, 45, 0, 0, time.UTC)
	calls := 0
	result := executeAutomaticDisableProbePlan(context.Background(), func(_ context.Context, request ModelTestRequest, _, _ string) (ModelTestResult, error) {
		calls++
		return ModelTestResult{
			AccountID: request.AccountID, Model: request.Model, Status: "review", ReasonCode: "transient_failure",
			StatusCode: http.StatusTooManyRequests, TestedAt: now,
			Experiment: &ModelTestExperiment{Name: "adaptive_weekly_overdraft", Applied: true, Strategy: request.AdaptiveWeeklyOverdraftStrategy},
		}, nil
	}, Account{ID: "account-1"}, InspectionResult{}, AutomaticDisableProbePlan{
		Name: "adaptive_weekly_overdraft", AttemptLimit: 9, Models: []string{"preferred"},
		Strategies: []AdaptiveOverdraftStrategy{AdaptiveStrategyS1, AdaptiveStrategyS2, AdaptiveStrategyS4},
		Request:    ModelTestRequest{ExperimentalAdaptiveWeeklyOverdraft: true, Inspection: true},
	}, "http://127.0.0.1:8317", "management-key", func() time.Time { return now })
	if calls != 1 || result.AutoDisableProbeStatus != InspectionAutoDisableProbeInconclusive || result.AutoDisableProbeReasonCode != "transient_failure" {
		t.Fatalf("calls=%d result=%#v", calls, result)
	}
}

func TestAdaptiveAutomaticDisableProbeAllStrategiesFailDefinitively(t *testing.T) {
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	calls := 0
	result := executeAutomaticDisableProbePlan(context.Background(), func(_ context.Context, request ModelTestRequest, _, _ string) (ModelTestResult, error) {
		calls++
		return ModelTestResult{
			AccountID: request.AccountID, Model: request.Model, Status: "unavailable", ReasonCode: "quota_limited",
			StatusCode: http.StatusTooManyRequests, QuotaWindow: InspectionQuotaWindowSevenDay, TestedAt: now,
			Experiment: &ModelTestExperiment{Name: "adaptive_weekly_overdraft", Applied: true, Strategy: request.AdaptiveWeeklyOverdraftStrategy},
		}, nil
	}, Account{ID: "account-1"}, InspectionResult{}, AutomaticDisableProbePlan{
		Name: "adaptive_weekly_overdraft", AttemptLimit: 9, Models: []string{"preferred", "fallback", "compatibility"},
		Strategies: []AdaptiveOverdraftStrategy{AdaptiveStrategyS1, AdaptiveStrategyS2, AdaptiveStrategyS4},
		Request:    ModelTestRequest{ExperimentalAdaptiveWeeklyOverdraft: true, Inspection: true},
	}, "http://127.0.0.1:8317", "management-key", func() time.Time { return now })
	if calls != 3 || result.AutoDisableProbeStatus != InspectionAutoDisableProbeFailed || result.AutoDisableProbeAttempts != 3 {
		t.Fatalf("calls=%d result=%#v", calls, result)
	}
}

func TestAdaptiveProbeFailureDoesNotRegressRememberedHigherStrategy(t *testing.T) {
	now := time.Date(2026, 7, 29, 12, 30, 0, 0, time.UTC)
	engine := NewAdaptiveWeeklyOverdraftExperiment(func() bool { return true })
	engine.now = func() time.Time { return now }
	engine.Configure(Config{DataDir: t.TempDir()}, cpaapi.SchemaVersion)
	engine.ObserveAccounts([]Account{adaptiveTestAccount("account-1", "stable-auth-1", 100, now)})
	engine.mu.Lock()
	record := engine.records[adaptiveAuthFingerprint("stable-auth-1")]
	record.Strategy = AdaptiveStrategyS4
	record.Phase = AdaptivePhaseActiveS4
	engine.records[record.Fingerprint] = record
	engine.mu.Unlock()

	engine.ObserveProbeResult("stable-auth-1", AdaptiveStrategyS1, ModelTestResult{
		Status: "unavailable", ReasonCode: "quota_limited", StatusCode: http.StatusTooManyRequests,
		QuotaWindow: InspectionQuotaWindowSevenDay, TestedAt: now,
	})
	assertAdaptiveState(t, engine, "stable-auth-1", AdaptivePhaseActiveS4, AdaptiveStrategyS4)
}

type adaptiveUsageReader map[string]*AccountUsageSnapshot

func (reader adaptiveUsageReader) Snapshot(authIndex string) *AccountUsageSnapshot {
	return reader[authIndex]
}

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

func TestAdaptiveWeeklyOverdraftTracksInjectedStrategyOutcome(t *testing.T) {
	now := time.Date(2026, 7, 29, 1, 30, 0, 0, time.UTC)
	engine := NewAdaptiveWeeklyOverdraftExperiment(func() bool { return true })
	engine.now = func() time.Time { return now }
	engine.ObserveAccounts([]Account{adaptiveTestAccount("account-1", "stable-auth-1", 100, now)})

	engine.recordRequest("success", "stable-auth-1", AdaptiveStrategyS1, now)
	engine.ObserveCompletion(cpaapi.RequestCompletion{RequestID: "success", StatusCode: http.StatusOK, CompletedAt: now})
	engine.recordRequest("failure", "stable-auth-1", AdaptiveStrategyS1, now)
	engine.ObserveCompletion(cpaapi.RequestCompletion{RequestID: "failure", StatusCode: http.StatusTooManyRequests, CompletedAt: now})

	state, ok := engine.stateForAuthID("stable-auth-1")
	if !ok {
		t.Fatal("state not found")
	}
	stats := state.StrategyStats[AdaptiveStrategyS1]
	if stats.Attempts != 2 || stats.Successes != 1 || stats.Failures != 1 {
		t.Fatalf("strategy stats = %#v", stats)
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

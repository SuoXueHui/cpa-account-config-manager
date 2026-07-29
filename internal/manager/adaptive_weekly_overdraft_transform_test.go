package manager

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"cpa-account-config-manager/internal/cpaapi"
)

func TestAdaptiveWeeklyOverdraftTransformStrategies(t *testing.T) {
	for _, test := range []struct {
		strategy AdaptiveOverdraftStrategy
		pairs    int
	}{
		{AdaptiveStrategyS1, 1},
		{AdaptiveStrategyS2, 2},
		{AdaptiveStrategyS4, 4},
	} {
		t.Run(string(test.strategy), func(t *testing.T) {
			experiment := armedAdaptiveExperiment(t, "stable-auth-1", test.strategy)
			sequence := 0
			experiment.newCallID = func(strategy AdaptiveOverdraftStrategy) (string, bool) {
				sequence++
				return "call_cpa_adaptive_" + string(strategy) + "_test" + string(rune('0'+sequence)), true
			}
			response, changed := experiment.InterceptRequest(adaptiveTransformRequest("request-1", "stable-auth-1", `{"model":"gpt-5.4","input":[{"type":"message","role":"user","content":"continue"}]}`))
			if !changed {
				t.Fatal("request was not transformed")
			}
			assertAdaptiveToolPairs(t, response.Body, test.strategy, test.pairs)
			if len(experiment.requests) != 1 {
				t.Fatalf("request ledger entries = %d", len(experiment.requests))
			}
		})
	}
}

func TestAdaptiveWeeklyOverdraftTransformRejectsIneligibleRequests(t *testing.T) {
	now := time.Date(2026, 7, 29, 6, 0, 0, 0, time.UTC)
	validBody := `{"input":[{"type":"message","role":"user","content":"continue"}]}`
	tests := []struct {
		name    string
		prepare func(*AdaptiveWeeklyOverdraftExperiment)
		request cpaapi.RequestInterceptRequest
	}{
		{name: "non-codex", request: cpaapi.RequestInterceptRequest{ToFormat: "openai", Body: []byte(validBody), Metadata: map[string]any{selectedAuthMetadataKey: "stable-auth-1"}}},
		{name: "missing-selected-auth", request: cpaapi.RequestInterceptRequest{ToFormat: "codex", Body: []byte(validBody)}},
		{name: "assistant-last", request: adaptiveTransformRequest("request-1", "stable-auth-1", `{"input":[{"type":"message","role":"assistant","content":"done"}]}`)},
		{name: "invalid-json", request: adaptiveTransformRequest("request-1", "stable-auth-1", `{"input":`)},
		{name: "empty", request: adaptiveTransformRequest("request-1", "stable-auth-1", ``)},
		{name: "original-marker", request: adaptiveTransformRequest("request-1", "stable-auth-1", `{"input":[{"type":"message","role":"user","content":"call_cpa_overdraft_existing"}]}`)},
		{name: "adaptive-marker", request: adaptiveTransformRequest("request-1", "stable-auth-1", `{"input":[{"type":"message","role":"user","content":"call_cpa_adaptive_s1_existing"}]}`)},
		{name: "oversized", prepare: func(engine *AdaptiveWeeklyOverdraftExperiment) { engine.maxBodyBytes = 8 }, request: adaptiveTransformRequest("request-1", "stable-auth-1", validBody)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			experiment := armedAdaptiveExperimentAt(t, "stable-auth-1", AdaptiveStrategyS1, now)
			if test.prepare != nil {
				test.prepare(experiment)
			}
			if response, changed := experiment.InterceptRequest(test.request); changed || len(response.Body) != 0 {
				t.Fatalf("changed=%v body=%s", changed, response.Body)
			}
		})
	}
}

func TestAdaptiveWeeklyOverdraftTransformRequiresFreshThresholdState(t *testing.T) {
	now := time.Date(2026, 7, 29, 7, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name       string
		percent    float64
		observedAt time.Time
	}{
		{name: "below", percent: 98.9, observedAt: now},
		{name: "stale", percent: 100, observedAt: now.Add(-16 * time.Minute)},
	} {
		t.Run(test.name, func(t *testing.T) {
			experiment := NewAdaptiveWeeklyOverdraftExperiment(func() bool { return true })
			experiment.now = func() time.Time { return now }
			experiment.Configure(Config{DataDir: t.TempDir()}, cpaapi.SchemaVersion)
			experiment.ObserveAccounts([]Account{adaptiveTestAccount("account-1", "stable-auth-1", test.percent, test.observedAt)})
			request := adaptiveTransformRequest("request-1", "stable-auth-1", `{"input":`)
			if response, changed := experiment.InterceptRequest(request); changed || len(response.Body) != 0 {
				t.Fatalf("changed=%v body=%s", changed, response.Body)
			}
		})
	}
}

func TestAdaptiveWeeklyOverdraftTransformUsesNewSelectedAccountState(t *testing.T) {
	now := time.Date(2026, 7, 29, 8, 0, 0, 0, time.UTC)
	experiment := NewAdaptiveWeeklyOverdraftExperiment(func() bool { return true })
	experiment.now = func() time.Time { return now }
	experiment.Configure(Config{DataDir: t.TempDir()}, cpaapi.SchemaVersion)
	experiment.ObserveAccounts([]Account{
		adaptiveTestAccount("account-1", "auth-s1", 100, now),
		adaptiveTestAccount("account-2", "auth-s2", 100, now),
	})
	experiment.mu.Lock()
	state := experiment.records[adaptiveAuthFingerprint("auth-s2")]
	state.Strategy = AdaptiveStrategyS2
	experiment.records[state.Fingerprint] = state
	experiment.mu.Unlock()

	response, changed := experiment.InterceptRequest(adaptiveTransformRequest("request-2", "auth-s2", `{"input":[{"type":"message","role":"user","content":"continue"}]}`))
	if !changed {
		t.Fatal("request was not transformed")
	}
	assertAdaptiveToolPairs(t, response.Body, AdaptiveStrategyS2, 2)
}

func TestAdaptiveWeeklyOverdraftExplicitTransformBypassesQuotaEligibilityOnly(t *testing.T) {
	experiment := NewAdaptiveWeeklyOverdraftExperiment(func() bool { return true })
	experiment.Configure(Config{DataDir: t.TempDir()}, cpaapi.SchemaVersion)
	request := cpaapi.RequestInterceptRequest{ToFormat: "codex", Body: []byte(`{"input":[{"type":"message","role":"user","content":"continue"}]}`)}
	response, changed := experiment.InterceptRequestForStrategy(request, AdaptiveStrategyS4, false)
	if !changed {
		t.Fatal("explicit strategy was not applied")
	}
	assertAdaptiveToolPairs(t, response.Body, AdaptiveStrategyS4, 4)
}

func armedAdaptiveExperiment(t *testing.T, authID string, strategy AdaptiveOverdraftStrategy) *AdaptiveWeeklyOverdraftExperiment {
	t.Helper()
	return armedAdaptiveExperimentAt(t, authID, strategy, time.Date(2026, 7, 29, 5, 0, 0, 0, time.UTC))
}

func armedAdaptiveExperimentAt(t *testing.T, authID string, strategy AdaptiveOverdraftStrategy, now time.Time) *AdaptiveWeeklyOverdraftExperiment {
	t.Helper()
	experiment := NewAdaptiveWeeklyOverdraftExperiment(func() bool { return true })
	experiment.now = func() time.Time { return now }
	experiment.Configure(Config{DataDir: t.TempDir()}, cpaapi.SchemaVersion)
	experiment.ObserveAccounts([]Account{adaptiveTestAccount("account-1", authID, 100, now)})
	experiment.mu.Lock()
	state := experiment.records[adaptiveAuthFingerprint(authID)]
	state.Strategy = strategy
	experiment.records[state.Fingerprint] = state
	experiment.mu.Unlock()
	return experiment
}

func adaptiveTransformRequest(requestID, authID, body string) cpaapi.RequestInterceptRequest {
	return cpaapi.RequestInterceptRequest{
		RequestID: requestID, ToFormat: "codex", Body: []byte(body),
		Metadata: map[string]any{selectedAuthMetadataKey: authID},
	}
}

func assertAdaptiveToolPairs(t *testing.T, body []byte, strategy AdaptiveOverdraftStrategy, pairCount int) {
	t.Helper()
	var document struct {
		Input []struct {
			Type   string `json:"type"`
			CallID string `json:"call_id"`
		} `json:"input"`
	}
	if errDecode := json.Unmarshal(body, &document); errDecode != nil {
		t.Fatalf("decode body: %v", errDecode)
	}
	if len(document.Input) != 1+pairCount*2 {
		t.Fatalf("input items = %d body=%s", len(document.Input), body)
	}
	seen := make(map[string]struct{}, pairCount)
	for index := 0; index < pairCount; index++ {
		call := document.Input[1+index*2]
		output := document.Input[2+index*2]
		if call.Type != "custom_tool_call" || output.Type != "custom_tool_call_output" || call.CallID != output.CallID {
			t.Fatalf("pair %d = %#v / %#v", index, call, output)
		}
		if !strings.HasPrefix(call.CallID, "call_cpa_adaptive_"+string(strategy)+"_") {
			t.Fatalf("call ID = %q", call.CallID)
		}
		if _, duplicate := seen[call.CallID]; duplicate {
			t.Fatalf("duplicate call ID = %q", call.CallID)
		}
		seen[call.CallID] = struct{}{}
	}
}

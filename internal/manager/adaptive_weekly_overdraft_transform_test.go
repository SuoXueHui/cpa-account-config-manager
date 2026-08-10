package manager

import (
	"encoding/json"
	"strconv"
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

func TestAdaptiveWeeklyOverdraftToolOutputCanaryIsDisabledByDefault(t *testing.T) {
	experiment := armedAdaptiveExperiment(t, "stable-auth-1", AdaptiveStrategyS1)
	request := adaptiveTransformRequest("request-1", "stable-auth-1", `{"input":[{"type":"function_call_output","call_id":"call-1","output":"done"}]}`)
	if response, changed := experiment.InterceptRequest(request); changed || len(response.Body) != 0 {
		t.Fatalf("changed=%v body=%s", changed, response.Body)
	}
}

func TestAdaptiveWeeklyOverdraftToolOutputCanaryUsesStableAuthBucket(t *testing.T) {
	inside := adaptiveTestAuthForCanary(t, 1, true)
	outside := adaptiveTestAuthForCanary(t, 1, false)
	for _, test := range []struct {
		name    string
		authID  string
		changed bool
	}{
		{name: "inside", authID: inside, changed: true},
		{name: "outside", authID: outside, changed: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			experiment := armedAdaptiveExperiment(t, test.authID, AdaptiveStrategyS1)
			experiment.SetToolOutputCanary(func() bool { return true }, func() int { return 1 })
			request := adaptiveTransformRequest("request-1", test.authID, `{"input":[{"type":"function_call_output","call_id":"call-1","output":"done"}]}`)
			response, changed := experiment.InterceptRequest(request)
			if changed != test.changed {
				t.Fatalf("changed=%v body=%s", changed, response.Body)
			}
		})
	}
}

func TestAdaptiveWeeklyOverdraftToolOutputCanaryAcceptsSupportedTails(t *testing.T) {
	for _, tail := range []string{
		`{"type":"function_call_output","call_id":"call-1","output":"done"}`,
		`{"type":"custom_tool_call_output","call_id":"call-1","output":[{"type":"input_text","text":"done"}]}`,
	} {
		experiment := armedAdaptiveExperiment(t, "stable-auth-1", AdaptiveStrategyS1)
		experiment.SetToolOutputCanary(func() bool { return true }, func() int { return 100 })
		response, changed := experiment.InterceptRequest(adaptiveTransformRequest("request-1", "stable-auth-1", `{"input":[`+tail+`]}`))
		if !changed {
			t.Fatalf("tail was not transformed: %s", tail)
		}
		assertAdaptiveToolPairs(t, response.Body, AdaptiveStrategyS1, 1)
	}
}

func TestAdaptiveWeeklyOverdraftToolOutputCanaryRejectsUnsupportedTail(t *testing.T) {
	experiment := armedAdaptiveExperiment(t, "stable-auth-1", AdaptiveStrategyS1)
	experiment.SetToolOutputCanary(func() bool { return true }, func() int { return 100 })
	request := adaptiveTransformRequest("request-1", "stable-auth-1", `{"input":[{"type":"reasoning","summary":[]}]}`)
	if response, changed := experiment.InterceptRequest(request); changed || len(response.Body) != 0 {
		t.Fatalf("changed=%v body=%s", changed, response.Body)
	}
}

func TestAdaptiveWeeklyOverdraftTracksRequestShapeOutcomes(t *testing.T) {
	experiment := armedAdaptiveExperiment(t, "stable-auth-1", AdaptiveStrategyS1)
	experiment.SetToolOutputCanary(func() bool { return true }, func() int { return 100 })
	requests := []cpaapi.RequestInterceptRequest{
		adaptiveTransformRequest("user", "stable-auth-1", `{"input":[{"type":"message","role":"user","content":"continue"}]}`),
		adaptiveTransformRequest("tool", "stable-auth-1", `{"input":[{"type":"function_call_output","call_id":"call-1","output":"done"}]}`),
	}
	for _, request := range requests {
		if _, changed := experiment.InterceptRequest(request); !changed {
			t.Fatalf("request %q was not transformed", request.RequestID)
		}
	}
	experiment.ObserveCompletion(cpaapi.RequestCompletion{RequestID: "user", StatusCode: 200})
	experiment.ObserveCompletion(cpaapi.RequestCompletion{RequestID: "tool", StatusCode: 429})

	state, ok := experiment.stateForAuthID("stable-auth-1")
	if !ok {
		t.Fatal("state not found")
	}
	if stats := state.RequestShapeStats[AdaptiveRequestShapeUserMessage]; stats.Attempts != 1 || stats.Successes != 1 || stats.Failures != 0 {
		t.Fatalf("user-message stats = %#v", stats)
	}
	if stats := state.RequestShapeStats[AdaptiveRequestShapeToolOutput]; stats.Attempts != 1 || stats.Successes != 0 || stats.Failures != 1 {
		t.Fatalf("tool-output stats = %#v", stats)
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

func adaptiveTestAuthForCanary(t *testing.T, percent int, selected bool) string {
	t.Helper()
	for index := 0; index < 10_000; index++ {
		authID := "canary-auth-" + strconv.Itoa(index)
		if adaptiveCanarySelected(authID, percent) == selected {
			return authID
		}
	}
	t.Fatalf("could not find auth ID with selected=%v", selected)
	return ""
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

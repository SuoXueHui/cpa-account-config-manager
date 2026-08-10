package manager

import (
	"net/http"
	"testing"
	"time"

	"cpa-account-config-manager/internal/cpaapi"
)

func TestAdaptiveTokenSchedulerFallsBackForNonCodexMissingSessionAndCanarySkip(t *testing.T) {
	outside := adaptiveTestAuthForCanary(t, 1, false)
	experiment := adaptiveSchedulerExperiment(t, []schedulerTestAccount{{authID: "auth-1", tokens: 10}})
	experiment.SetTokenDrainCanary(func() bool { return true }, func() int { return 1 }, func() int { return 8 })

	for _, request := range []cpaapi.SchedulerPickRequest{
		{Provider: "openai", Options: cpaapi.SchedulerOptions{Headers: map[string][]string{"X-Session-ID": {"session-1"}}}, Candidates: schedulerCandidates("auth-1")},
		{Provider: "codex", Candidates: schedulerCandidates("auth-1")},
		{Provider: "codex", Options: cpaapi.SchedulerOptions{Headers: map[string][]string{"X-Session-ID": {outside}}}, Candidates: schedulerCandidates("auth-1")},
	} {
		if response := experiment.PickTokenDrainAuth(request); response.Handled || response.AuthID != "" {
			t.Fatalf("response = %#v", response)
		}
	}
}

func TestAdaptiveTokenSchedulerReusesSessionBinding(t *testing.T) {
	experiment := adaptiveSchedulerExperiment(t, []schedulerTestAccount{{authID: "auth-a", tokens: 100}, {authID: "auth-b", tokens: 10}})
	experiment.SetTokenDrainCanary(func() bool { return true }, func() int { return 100 }, func() int { return 8 })
	request := cpaapi.SchedulerPickRequest{
		Provider: "codex", Options: cpaapi.SchedulerOptions{Headers: map[string][]string{"Session-Id": {"session-1"}}},
		Candidates: schedulerCandidates("auth-a", "auth-b"),
	}
	first := experiment.PickTokenDrainAuth(request)
	if !first.Handled || first.AuthID != "auth-a" {
		t.Fatalf("first response = %#v", first)
	}

	experiment.mu.Lock()
	record := experiment.records[adaptiveAuthFingerprint("auth-b")]
	record.PostThresholdTokens = 1_000
	experiment.records[record.Fingerprint] = record
	experiment.mu.Unlock()
	second := experiment.PickTokenDrainAuth(request)
	if !second.Handled || second.AuthID != "auth-a" {
		t.Fatalf("binding was not reused: %#v", second)
	}
}

func TestAdaptiveTokenSchedulerPrefersHigherPostThresholdTokensOverStrategyRank(t *testing.T) {
	experiment := adaptiveSchedulerExperiment(t, []schedulerTestAccount{{authID: "auth-s4", tokens: 10}, {authID: "auth-s1", tokens: 1_000}})
	experiment.SetTokenDrainCanary(func() bool { return true }, func() int { return 100 }, func() int { return 8 })
	experiment.mu.Lock()
	s4Record := experiment.records[adaptiveAuthFingerprint("auth-s4")]
	s4Record.Phase = AdaptivePhaseActiveS4
	s4Record.Strategy = AdaptiveStrategyS4
	experiment.records[s4Record.Fingerprint] = s4Record
	experiment.mu.Unlock()

	response := experiment.PickTokenDrainAuth(schedulerRequest("session-token-first", "auth-s4", "auth-s1"))
	if !response.Handled || response.AuthID != "auth-s1" {
		t.Fatalf("response = %#v", response)
	}
}

func TestAdaptiveTokenSchedulerSpreadsAfterPerCredentialSessionLimit(t *testing.T) {
	experiment := adaptiveSchedulerExperiment(t, []schedulerTestAccount{{authID: "auth-a", tokens: 100}, {authID: "auth-b", tokens: 10}})
	experiment.SetTokenDrainCanary(func() bool { return true }, func() int { return 100 }, func() int { return 1 })

	first := experiment.PickTokenDrainAuth(schedulerRequest("session-1", "auth-a", "auth-b"))
	second := experiment.PickTokenDrainAuth(schedulerRequest("session-2", "auth-a", "auth-b"))
	if first.AuthID != "auth-a" || second.AuthID != "auth-b" || !first.Handled || !second.Handled {
		t.Fatalf("first=%#v second=%#v", first, second)
	}
}

func TestAdaptiveTokenSchedulerInvalidatesBindingAfterHardStop(t *testing.T) {
	experiment := adaptiveSchedulerExperiment(t, []schedulerTestAccount{{authID: "auth-a", tokens: 100}, {authID: "auth-b", tokens: 10}})
	experiment.SetTokenDrainCanary(func() bool { return true }, func() int { return 100 }, func() int { return 8 })
	request := schedulerRequest("session-1", "auth-a", "auth-b")
	if first := experiment.PickTokenDrainAuth(request); first.AuthID != "auth-a" {
		t.Fatalf("first response = %#v", first)
	}

	experiment.mu.Lock()
	record := experiment.records[adaptiveAuthFingerprint("auth-a")]
	record.Phase = AdaptivePhaseHardStopped
	record.HardStopReason = "authentication_failed"
	experiment.records[record.Fingerprint] = record
	experiment.mu.Unlock()
	if second := experiment.PickTokenDrainAuth(request); !second.Handled || second.AuthID != "auth-b" {
		t.Fatalf("hard-stopped binding was retained: %#v", second)
	}
}

func TestAdaptiveSchedulerSessionKeyMatchesHostPriorityAndNamespaces(t *testing.T) {
	tests := []struct {
		name    string
		options cpaapi.SchedulerOptions
		want    string
	}{
		{
			name: "claude header wins",
			options: cpaapi.SchedulerOptions{Headers: http.Header{
				"X-Claude-Code-Session-Id": {"claude-session"},
				"Session-Id":               {"codex-session"},
			}},
			want: "claude:claude-session",
		},
		{
			name: "codex header wins over generic header",
			options: cpaapi.SchedulerOptions{Headers: http.Header{
				"Session-Id":   {"codex-session"},
				"X-Session-ID": {"generic-session"},
			}},
			want: "codex:codex-session",
		},
		{
			name:    "affinity header",
			options: cpaapi.SchedulerOptions{Headers: http.Header{"X-Session-Affinity": {"affinity-session"}}},
			want:    "affinity:affinity-session",
		},
		{
			name:    "client request header",
			options: cpaapi.SchedulerOptions{Headers: http.Header{"X-Client-Request-Id": {"client-request"}}},
			want:    "clientreq:client-request",
		},
		{
			name: "execution metadata wins over derived metadata",
			options: cpaapi.SchedulerOptions{Metadata: map[string]any{
				"execution_session_id": "execution-session",
				"derived_session_id":   "derived-session",
			}},
			want: "execution:execution-session",
		},
		{
			name:    "derived metadata",
			options: cpaapi.SchedulerOptions{Metadata: map[string]any{"derived_session_id": "derived-session"}},
			want:    "derived:derived-session",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := adaptiveSchedulerSessionKey(test.options); got != test.want {
				t.Fatalf("adaptiveSchedulerSessionKey() = %q, want %q", got, test.want)
			}
		})
	}
}

type schedulerTestAccount struct {
	authID string
	tokens int64
}

func adaptiveSchedulerExperiment(t *testing.T, accounts []schedulerTestAccount) *AdaptiveWeeklyOverdraftExperiment {
	t.Helper()
	now := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	experiment := NewAdaptiveWeeklyOverdraftExperiment(func() bool { return true })
	experiment.now = func() time.Time { return now }
	experiment.Configure(Config{DataDir: t.TempDir()}, cpaapi.SchemaVersion)
	observed := make([]Account, 0, len(accounts))
	for index, account := range accounts {
		observed = append(observed, adaptiveTestAccount("account-"+string(rune('a'+index)), account.authID, 100, now))
	}
	experiment.ObserveAccounts(observed)
	experiment.mu.Lock()
	for _, account := range accounts {
		record := experiment.records[adaptiveAuthFingerprint(account.authID)]
		record.Phase = AdaptivePhaseActiveS1
		record.PostThresholdTokens = account.tokens
		experiment.records[record.Fingerprint] = record
	}
	experiment.mu.Unlock()
	return experiment
}

func schedulerCandidates(authIDs ...string) []cpaapi.SchedulerAuthCandidate {
	candidates := make([]cpaapi.SchedulerAuthCandidate, 0, len(authIDs))
	for _, authID := range authIDs {
		candidates = append(candidates, cpaapi.SchedulerAuthCandidate{ID: authID, Provider: "codex", Priority: 0, Status: "ready"})
	}
	return candidates
}

func schedulerRequest(sessionID string, authIDs ...string) cpaapi.SchedulerPickRequest {
	return cpaapi.SchedulerPickRequest{
		Provider: "codex", Options: cpaapi.SchedulerOptions{Headers: http.Header{"X-Session-ID": {sessionID}}},
		Candidates: schedulerCandidates(authIDs...),
	}
}

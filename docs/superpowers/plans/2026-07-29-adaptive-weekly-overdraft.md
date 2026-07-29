# Adaptive Weekly Overdraft Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a separately controlled, account-aware adaptive Codex weekly-overdraft mode while preserving the author's original mode unchanged.

**Architecture:** A new `AdaptiveWeeklyOverdraftExperiment` owns account-fingerprinted state, a bounded request ledger, request transformation, usage/completion observation, persistence, and inspection integration. `ExperimentalSettingsService` enforces mutual exclusion and schema-v2 availability; App wiring composes concurrency, original, and adaptive transformers in that order. The authenticated Management API and React UI expose only sanitized summaries and atomically switch between original and adaptive modes.

**Tech Stack:** Go 1.24+, CPA plugin ABI schema v1/v2, JSON/YAML private stores, React 18, TypeScript 5.7, Vitest, Vite.

---

## File Map

**Create**
- `internal/manager/adaptive_weekly_overdraft.go` — state machine, lifecycle observers, sanitized summaries, and automatic-disable guard.
- `internal/manager/adaptive_weekly_overdraft_transform.go` — bounded `s1`/`s2`/`s4` payload generation and no-op fast paths.
- `internal/manager/adaptive_weekly_overdraft_store.go` — versioned private persistence, pruning, and fail-closed loading.
- `internal/manager/adaptive_weekly_overdraft_test.go` — state, lifecycle, projection, and inspection tests.
- `internal/manager/adaptive_weekly_overdraft_transform_test.go` — golden request-shape and eligibility tests.
- `internal/manager/adaptive_weekly_overdraft_store_test.go` — persistence, permissions, bounds, and corrupt/future-version tests.
- `web/src/components/AdaptiveWeeklyOverdraftStatus.tsx` — compact account-state presentation.
- `web/src/components/AdaptiveWeeklyOverdraftStatus.test.tsx` — status rendering tests.

**Modify**
- `internal/manager/experimental_settings.go` and `experimental_settings_test.go` — second switch, schema availability, warnings, and mutual exclusion.
- `internal/manager/app.go`, `app_test.go`, and `main_test.go` — engine wiring, lifecycle forwarding, route registration, and host-schema compatibility.
- `internal/manager/request_hook.go` and tests — preserve transformer ordering and termination behavior.
- `internal/manager/model_probe.go`, `model_test_handler.go`, and `model_test_test.go` — explicit adaptive model-test strategy support.
- `internal/manager/inspection_guard.go`, `inspection_disable_probe.go`, and `inspection_probe_test.go` — bounded strategy-aware probe ladder.
- `internal/manager/types.go`, `accounts.go`, and `accounts_test.go` — attach sanitized adaptive state to account projections.
- `web/src/types.ts`, `web/src/api/client.ts`, and `web/src/api/client.test.ts` — new settings/state/model-test contracts.
- `web/src/App.tsx` — load both experiment availability flags and send the selected model-test mode.
- `web/src/components/OtherSettingsWorkspace.tsx` and tests — mutually exclusive cards and schema-v2 disabled state.
- `web/src/components/ModelTestDialog.tsx` and tests — separate original/adaptive test actions.
- `web/src/components/AccountUsageCell.tsx` and tests — render compact adaptive state.
- `web/src/i18n/uiText.ts` plus all four `uiCatalog*.ts` files — complete localized copy.
- `web/scripts/mock-cpa.mjs` — mock settings, adaptive status, and adaptive model-test responses.
- `README.md` and `README_EN.md` — configuration, compatibility, state path, and rollback notes.

## Task 1: Settings, Compatibility, and Mutual Exclusion

**Files:**
- Modify: `internal/manager/experimental_settings.go`
- Modify: `internal/manager/experimental_settings_test.go`
- Modify: `internal/manager/app.go`

- [ ] **Step 1: Write failing settings tests**

Add table-driven tests that assert:

```go
func TestExperimentalSettingsRejectMutuallyExclusiveOverdraftModes(t *testing.T) {
    service := NewExperimentalSettingsService()
    service.ConfigureHost(Config{DataDir: t.TempDir()}, cpaapi.SchemaVersion)
    _, errSet := service.Set(ExperimentalSettings{
        WeeklyOverdraftEnabled:         true,
        AdaptiveWeeklyOverdraftEnabled: true,
    })
    if !errors.Is(errSet, ErrOverdraftModesMutuallyExclusive) {
        t.Fatalf("Set() error = %v", errSet)
    }
}

func TestExperimentalSettingsRejectAdaptiveModeOnLegacyHost(t *testing.T) {
    service := NewExperimentalSettingsService()
    service.ConfigureHost(Config{DataDir: t.TempDir()}, cpaapi.LegacySchemaVersion)
    _, errSet := service.Set(ExperimentalSettings{AdaptiveWeeklyOverdraftEnabled: true})
    if !errors.Is(errSet, ErrAdaptiveWeeklyOverdraftUnavailable) {
        t.Fatalf("Set() error = %v", errSet)
    }
}

func TestExperimentalSettingsConflictingStartupConfigKeepsOriginalMode(t *testing.T) {
    service := NewExperimentalSettingsService()
    settings := ExperimentalSettings{WeeklyOverdraftEnabled: true, AdaptiveWeeklyOverdraftEnabled: true}
    service.ConfigureHost(Config{DataDir: t.TempDir(), ExperimentalSettings: &settings}, cpaapi.SchemaVersion)
    snapshot := service.Snapshot()
    if !snapshot.Settings.WeeklyOverdraftEnabled || snapshot.Settings.AdaptiveWeeklyOverdraftEnabled {
        t.Fatalf("settings = %#v", snapshot.Settings)
    }
    if snapshot.ConfigurationWarning != "overdraft_modes_conflicted_original_preserved" {
        t.Fatalf("configuration_warning = %q", snapshot.ConfigurationWarning)
    }
}
```

- [ ] **Step 2: Run the focused tests and verify RED**

Run:

```bash
go test ./internal/manager -run 'TestExperimentalSettings(Reject|Conflicting)' -count=1
```

Expected: compilation failure because the adaptive field, errors, and `ConfigureHost` do not exist.

- [ ] **Step 3: Implement minimal settings support**

Add these contracts:

```go
var (
    ErrOverdraftModesMutuallyExclusive    = errors.New("weekly overdraft modes are mutually exclusive")
    ErrAdaptiveWeeklyOverdraftUnavailable = errors.New("adaptive weekly overdraft requires request lifecycle schema v2")
)

type ExperimentalSettings struct {
    WeeklyOverdraftEnabled         bool `json:"weekly_overdraft_enabled" yaml:"weekly_overdraft_enabled"`
    AdaptiveWeeklyOverdraftEnabled bool `json:"adaptive_weekly_overdraft_enabled" yaml:"adaptive_weekly_overdraft_enabled"`
    AgentIdentityEnabled           bool `json:"agent_identity_enabled" yaml:"agent_identity_enabled"`
    AutoModelWhitelistEnabled      bool `json:"auto_model_whitelist_enabled" yaml:"auto_model_whitelist_enabled"`
}

type ExperimentalSettingsSnapshot struct {
    Settings                                  ExperimentalSettings `json:"settings"`
    AdaptiveWeeklyOverdraftAvailable          bool                 `json:"adaptive_weekly_overdraft_available"`
    AdaptiveWeeklyOverdraftUnavailableReason  string               `json:"adaptive_weekly_overdraft_unavailable_reason,omitempty"`
    ConfigurationWarning                      string               `json:"configuration_warning,omitempty"`
    StorageError                              string               `json:"storage_error,omitempty"`
}
```

Store the negotiated schema in the service, expose `AdaptiveWeeklyOverdraftEnabled()`, validate Management writes before persistence, and normalize conflicting startup YAML by preserving original mode and forcing adaptive mode off. Change `App.ConfigureHost` to call `a.experiments.ConfigureHost(config, hostSchema)`.

- [ ] **Step 4: Verify GREEN and regression behavior**

Run:

```bash
go test ./internal/manager -run 'TestExperimentalSettings' -count=1
```

Expected: all experimental-settings tests pass, including existing original-mode persistence tests.

- [ ] **Step 5: Commit**

```bash
git add internal/manager/experimental_settings.go internal/manager/experimental_settings_test.go internal/manager/app.go
git commit -m "feat: add adaptive overdraft setting"
```

## Task 2: Persistent Account State Machine

**Files:**
- Create: `internal/manager/adaptive_weekly_overdraft.go`
- Create: `internal/manager/adaptive_weekly_overdraft_store.go`
- Create: `internal/manager/adaptive_weekly_overdraft_test.go`
- Create: `internal/manager/adaptive_weekly_overdraft_store_test.go`

- [ ] **Step 1: Write failing state-machine tests**

Cover the required transition matrix using a fixed clock:

```go
func TestAdaptiveWeeklyOverdraftStateTransitions(t *testing.T) {
    now := time.Date(2026, 7, 29, 0, 0, 0, 0, time.UTC)
    engine := NewAdaptiveWeeklyOverdraftExperiment(func() bool { return true })
    engine.now = func() time.Time { return now }
    engine.ObserveAccounts([]Account{{
        ID: "account-1", AuthID: "stable-auth-1", Provider: "codex",
        Usage: &AccountUsageSnapshot{Codex: &CodexUsageSnapshot{
            ObservedAt: now, SevenDay: &UsageWindowSnapshot{UsedPercent: 99},
        }},
    }})
    assertAdaptiveState(t, engine, "stable-auth-1", AdaptivePhaseArmed, AdaptiveStrategyS1)

    engine.recordRequest("request-1", "stable-auth-1", AdaptiveStrategyS1, now)
    engine.ObserveCompletion(cpaapi.RequestCompletion{
        RequestID: "request-1", StatusCode: http.StatusTooManyRequests,
        Error: `{"error":{"type":"usage_limit_reached"}}`, CompletedAt: now.Add(time.Second),
    })
    assertAdaptiveState(t, engine, "stable-auth-1", AdaptivePhaseArmed, AdaptiveStrategyS2)
}
```

Add separate tests for success activation, `s1 -> s2 -> s4 -> exhausted`, generic 429 no-op, 401/402 hard stop, reset clearing normal state only, successful recovery clearing hard stop, and late completion not regressing a newer strategy.

- [ ] **Step 2: Run the state tests and verify RED**

```bash
go test ./internal/manager -run 'TestAdaptiveWeeklyOverdraftState' -count=1
```

Expected: compilation failure because the engine and constants do not exist.

- [ ] **Step 3: Implement the state model and lifecycle observers**

Use these bounded internal contracts:

```go
type AdaptiveOverdraftPhase string
type AdaptiveOverdraftStrategy string

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
    Fingerprint               string                    `json:"fingerprint"`
    Phase                     AdaptiveOverdraftPhase    `json:"phase"`
    Strategy                  AdaptiveOverdraftStrategy `json:"strategy,omitempty"`
    PostThresholdSuccesses    int64                     `json:"post_threshold_successes"`
    PostThresholdTokens       int64                     `json:"post_threshold_tokens"`
    ConsecutiveQuotaFailures  int                       `json:"consecutive_quota_failures"`
    LastSuccessAt             time.Time                 `json:"last_success_at,omitempty"`
    LastFailureAt             time.Time                 `json:"last_failure_at,omitempty"`
    LastObservedAt            time.Time                 `json:"last_observed_at,omitempty"`
    ResetAt                   time.Time                 `json:"reset_at,omitempty"`
    HardStopReason            string                    `json:"hard_stop_reason,omitempty"`
}
```

Fingerprint raw AuthID with SHA-256, keep account IDs and AuthIndex mappings in memory only, cap records and ledger at 10,000 entries, and prune records older than 31 days. `ObserveUsage` must reuse `classifyUsageFailure`; it may arm quota state and count successful tokens but must not persist bodies or headers. `ObserveCompletion` must use the RequestID ledger so old completions cannot overwrite a newer strategy.

- [ ] **Step 4: Write failing persistence tests**

Assert a round trip, `0600` permissions, raw secret exclusion, record bound, 31-day pruning, corrupt JSON failure, and future-version failure:

```go
func TestAdaptiveWeeklyOverdraftStoreExcludesSecrets(t *testing.T) {
    path := adaptiveWeeklyOverdraftStorePath(t.TempDir())
    records := map[string]adaptiveOverdraftRecord{
        adaptiveAuthFingerprint("secret-auth-id"): {Fingerprint: adaptiveAuthFingerprint("secret-auth-id"), Phase: AdaptivePhaseArmed, Strategy: AdaptiveStrategyS1},
    }
    if errSave := saveAdaptiveWeeklyOverdraftState(path, records); errSave != nil {
        t.Fatal(errSave)
    }
    raw, errRead := os.ReadFile(path)
    if errRead != nil { t.Fatal(errRead) }
    if bytes.Contains(raw, []byte("secret-auth-id")) {
        t.Fatal("persisted state contains raw AuthID")
    }
}
```

- [ ] **Step 5: Implement persistence and verify GREEN**

Use `savePrivateJSON`, a version-1 envelope, strict version checking, bounded sanitization, and fail-closed `storageErr` state. Run:

```bash
go test ./internal/manager -run 'TestAdaptiveWeeklyOverdraft(State|Store)' -count=1
```

Expected: all new state and store tests pass.

- [ ] **Step 6: Commit**

```bash
git add internal/manager/adaptive_weekly_overdraft.go internal/manager/adaptive_weekly_overdraft_store.go internal/manager/adaptive_weekly_overdraft_test.go internal/manager/adaptive_weekly_overdraft_store_test.go
git commit -m "feat: add adaptive overdraft state machine"
```

## Task 3: Bounded Request Transformation

**Files:**
- Create: `internal/manager/adaptive_weekly_overdraft_transform.go`
- Create: `internal/manager/adaptive_weekly_overdraft_transform_test.go`
- Modify: `internal/manager/experimental_weekly_overdraft.go`

- [ ] **Step 1: Write failing golden transformation tests**

Test `s1`, `s2`, and `s4` with deterministic IDs and assert exact alternating pairs:

```go
func TestAdaptiveWeeklyOverdraftTransformStrategies(t *testing.T) {
    for _, test := range []struct {
        strategy AdaptiveOverdraftStrategy
        pairs    int
    }{{AdaptiveStrategyS1, 1}, {AdaptiveStrategyS2, 2}, {AdaptiveStrategyS4, 4}} {
        t.Run(string(test.strategy), func(t *testing.T) {
            experiment := armedAdaptiveExperiment(t, test.strategy)
            response, changed := experiment.InterceptRequest(cpaapi.RequestInterceptRequest{
                RequestID: "request-1", ToFormat: "codex",
                Metadata: map[string]any{selectedAuthMetadataKey: "stable-auth-1"},
                Body: []byte(`{"model":"gpt-5.4","input":[{"type":"message","role":"user","content":"continue"}]}`),
            })
            if !changed { t.Fatal("request was not transformed") }
            assertAdaptiveToolPairs(t, response.Body, test.strategy, test.pairs)
        })
    }
}
```

Add no-transform cases for disabled/schema-v1, non-Codex, missing selected auth, stale/below-99 quota, assistant-last, invalid/empty/oversized body, and original/adaptive call markers already present. Add a test that below-threshold bodies with malformed JSON still return before decoding.

- [ ] **Step 2: Run the transform tests and verify RED**

```bash
go test ./internal/manager -run 'TestAdaptiveWeeklyOverdraftTransform' -count=1
```

Expected: compilation failure because the transformer is not implemented.

- [ ] **Step 3: Implement the minimal transformer**

Reuse the existing JSON scanner helpers and no-op tool input. Add:

```go
func adaptiveStrategyPairCount(strategy AdaptiveOverdraftStrategy) int {
    switch strategy {
    case AdaptiveStrategyS1:
        return 1
    case AdaptiveStrategyS2:
        return 2
    case AdaptiveStrategyS4:
        return 4
    default:
        return 0
    }
}

func (e *AdaptiveWeeklyOverdraftExperiment) InterceptRequestForStrategy(request cpaapi.RequestInterceptRequest, strategy AdaptiveOverdraftStrategy, requireEligibility bool) (cpaapi.RequestInterceptResponse, bool)
```

The normal `InterceptRequest` path must check format, selected AuthID, engine availability, current 15-minute quota eligibility, and existing markers before decoding. Generate a fresh `call_cpa_adaptive_<strategy>_<random>` ID for every pair and only record RequestID after the full transformed body passes the size bound.

- [ ] **Step 4: Verify GREEN and original regression**

```bash
go test ./internal/manager -run 'Test(AdaptiveWeeklyOverdraftTransform|WeeklyOverdraft)' -count=1
```

Expected: all adaptive golden cases and existing original transformer tests pass unchanged.

- [ ] **Step 5: Commit**

```bash
git add internal/manager/adaptive_weekly_overdraft_transform.go internal/manager/adaptive_weekly_overdraft_transform_test.go internal/manager/experimental_weekly_overdraft.go
git commit -m "feat: add adaptive overdraft request strategies"
```

## Task 4: App Lifecycle Wiring and Account Projection

**Files:**
- Modify: `internal/manager/app.go`
- Modify: `internal/manager/types.go`
- Modify: `internal/manager/accounts.go`
- Modify: `internal/manager/accounts_test.go`
- Modify: `internal/manager/app_test.go`

- [ ] **Step 1: Write failing wiring tests**

Assert transformer order, usage/completion forwarding, schema-v1 inactivity, and account projection:

```go
func TestAppWiresAdaptiveOverdraftAfterConcurrencyAndOriginal(t *testing.T) {
    app := NewApp(&fakeAuthHost{}, []byte("index"))
    if len(app.requestHooks.transformers) != 3 {
        t.Fatalf("transformers = %d", len(app.requestHooks.transformers))
    }
    if _, ok := app.requestHooks.transformers[2].(*AdaptiveWeeklyOverdraftExperiment); !ok {
        t.Fatalf("third transformer = %T", app.requestHooks.transformers[2])
    }
}
```

Add an account-list assertion that only the selected account receives a sanitized `adaptive_weekly_overdraft` summary and the raw AuthID is absent from serialized JSON.

- [ ] **Step 2: Run focused tests and verify RED**

```bash
go test ./internal/manager -run 'Test(AppWiresAdaptive|Account.*Adaptive)' -count=1
```

Expected: failure because App and Account do not expose the adaptive engine.

- [ ] **Step 3: Wire the engine**

Add `adaptiveOverdraft *AdaptiveWeeklyOverdraftExperiment` to App. Construct it from `experiments.AdaptiveWeeklyOverdraftEnabled`, register it after the original transformer, register both original and adaptive automatic-disable guards, add it to `accountObserverGroup`, configure it with `data_dir` and host schema, close it during quiesce, forward usage after `a.usage.Observe`, and forward completion after `a.concurrency.Complete`.

Add this public projection:

```go
type AdaptiveWeeklyOverdraftSummary struct {
    Phase                    AdaptiveOverdraftPhase    `json:"phase"`
    Strategy                 AdaptiveOverdraftStrategy `json:"strategy,omitempty"`
    PostThresholdSuccesses   int64                     `json:"post_threshold_successes"`
    PostThresholdTokens      int64                     `json:"post_threshold_tokens"`
    LastSuccessAt            *time.Time                `json:"last_success_at,omitempty"`
    LastFailureAt            *time.Time                `json:"last_failure_at,omitempty"`
    ResetAt                  *time.Time                `json:"reset_at,omitempty"`
    HardStopReason           string                    `json:"hard_stop_reason,omitempty"`
}
```

Attach it through an `AdaptiveWeeklyOverdraftReader` on `AccountService`; never serialize the fingerprint or raw AuthID.

- [ ] **Step 4: Verify GREEN**

```bash
go test ./internal/manager -run 'Test(AppWiresAdaptive|Account.*Adaptive|RequestHook)' -count=1
```

Expected: all focused tests pass.

- [ ] **Step 5: Commit**

```bash
git add internal/manager/app.go internal/manager/types.go internal/manager/accounts.go internal/manager/accounts_test.go internal/manager/app_test.go
git commit -m "feat: wire adaptive overdraft lifecycle"
```

## Task 5: Adaptive Model Tests and Inspection Ladder

**Files:**
- Modify: `internal/manager/model_probe.go`
- Modify: `internal/manager/model_test_handler.go`
- Modify: `internal/manager/model_test_test.go`
- Modify: `internal/manager/inspection_guard.go`
- Modify: `internal/manager/inspection_disable_probe.go`
- Modify: `internal/manager/inspection_probe_test.go`
- Modify: `internal/manager/adaptive_weekly_overdraft.go`

- [ ] **Step 1: Write failing explicit model-test tests**

Add request fields and verify adaptive transformation is separate from original:

```go
func TestHandleAccountModelTestRunsExplicitAdaptiveStrategy(t *testing.T) {
    app := configuredAdaptiveModelTestApp(t)
    body, _ := json.Marshal(ModelTestRequest{
        AccountID: "auth-1", Model: "gpt-5.4",
        ExperimentalAdaptiveWeeklyOverdraft: true,
        AdaptiveWeeklyOverdraftStrategy: AdaptiveStrategyS2,
    })
    response := app.HandleManagement(t.Context(), cpaapi.ManagementRequest{
        Method: http.MethodPost, Path: "/v0/management/plugins/cpa-account-config-manager/accounts/model-test",
        Headers: http.Header{"Authorization": {"Bearer secret"}}, Body: body,
    })
    if response.StatusCode != http.StatusOK { t.Fatalf("status = %d", response.StatusCode) }
    var result ModelTestResult
    json.Unmarshal(response.Body, &result)
    if result.Experiment == nil || result.Experiment.Name != "adaptive_weekly_overdraft" || result.Experiment.Strategy != AdaptiveStrategyS2 {
        t.Fatalf("experiment = %#v", result.Experiment)
    }
}
```

- [ ] **Step 2: Write failing strategy-aware inspection tests**

Extend `AutomaticDisableProbePlan` with `Strategies []AdaptiveOverdraftStrategy`. Test:
- `s1` success stops after one request.
- weekly quota failures advance through `s1`, `s2`, and `s4`.
- unsupported model moves to the next allowed model without consuming a strategy.
- transient failures return inconclusive.
- 401/402 terminate immediately.
- all three definitive failures permit auto-disable.

- [ ] **Step 3: Run focused tests and verify RED**

```bash
go test ./internal/manager -run 'Test(HandleAccountModelTestRunsExplicitAdaptive|AdaptiveAutomaticDisable)' -count=1
```

Expected: compilation failure because adaptive model-test and plan fields do not exist.

- [ ] **Step 4: Implement explicit adaptive transformation**

Add:

```go
type ModelTestRequest struct {
    AccountID                            string                    `json:"account_id"`
    Model                                string                    `json:"model,omitempty"`
    ExperimentalWeeklyOverdraft          bool                      `json:"experimental_weekly_overdraft,omitempty"`
    ExperimentalAdaptiveWeeklyOverdraft  bool                      `json:"experimental_adaptive_weekly_overdraft,omitempty"`
    AdaptiveWeeklyOverdraftStrategy      AdaptiveOverdraftStrategy `json:"adaptive_weekly_overdraft_strategy,omitempty"`
    Inspection                           bool                      `json:"-"`
    DetectRestrictedModels               bool                      `json:"-"`
    SelectPolicyFallback                 bool                      `json:"-"`
}

type ModelTestExperiment struct {
    Name     string                    `json:"name"`
    Applied  bool                      `json:"applied"`
    CallID   string                    `json:"call_id,omitempty"`
    Strategy AdaptiveOverdraftStrategy `json:"strategy,omitempty"`
}
```

Reject requests that set both original and adaptive flags. Validate adaptive availability and strategy. Configure ModelTestService with separate original and adaptive transformers; explicit adaptive tests bypass the 99-percent gate but still require Codex OAuth and bounded valid bodies.

- [ ] **Step 5: Implement the adaptive inspection ladder**

For strategy-aware plans, execute at most nine calls: three strategies times three unique safe models. Classification rules:
- `available`: pass and record winning strategy.
- `model_not_found`, `unsupported_provider`, or policy-blocked: try the next model without advancing strategy.
- definitive weekly quota: advance strategy.
- normalized credential/workspace hard stop: fail immediately and update engine hard-stop state.
- timeout/upstream/transient/generic 429: inconclusive immediately.

Keep the existing original `AttemptLimit` loop unchanged when `Strategies` is empty.

- [ ] **Step 6: Verify GREEN and original regression**

```bash
go test ./internal/manager -run 'Test(HandleAccountModelTest.*Overdraft|AdaptiveAutomaticDisable|WeeklyOverdraft)' -count=1
```

Expected: adaptive and original suites pass.

- [ ] **Step 7: Commit**

```bash
git add internal/manager/model_probe.go internal/manager/model_test_handler.go internal/manager/model_test_test.go internal/manager/inspection_guard.go internal/manager/inspection_disable_probe.go internal/manager/inspection_probe_test.go internal/manager/adaptive_weekly_overdraft.go
git commit -m "feat: add adaptive overdraft probe ladder"
```

## Task 6: Authenticated Adaptive State API

**Files:**
- Modify: `internal/manager/app.go`
- Modify: `internal/manager/app_test.go`
- Modify: `internal/manager/adaptive_weekly_overdraft.go`

- [ ] **Step 1: Write failing route tests**

Register and test exact GET route `/plugins/cpa-account-config-manager/experiments/adaptive-weekly-overdraft`. Assert response shape:

```json
{
  "available": true,
  "host_schema_version": 2,
  "required_schema_version": 2,
  "accounts": [
    {
      "account_id": "auth-1",
      "phase": "active_s2",
      "strategy": "s2",
      "post_threshold_successes": 183,
      "post_threshold_tokens": 14800000
    }
  ]
}
```

Assert serialized output excludes raw AuthID, fingerprint, request IDs, failure bodies, and headers.

- [ ] **Step 2: Run route tests and verify RED**

```bash
go test ./internal/manager -run 'TestAdaptiveWeeklyOverdraftManagement' -count=1
```

Expected: route not registered or returns 404.

- [ ] **Step 3: Implement route and snapshot**

Expose only account IDs already learned from authenticated account projections. Sort by account ID, cap output at 10,000, and include availability metadata plus sanitized storage error text.

- [ ] **Step 4: Verify GREEN**

```bash
go test ./internal/manager -run 'Test(AdaptiveWeeklyOverdraftManagement|ManagementRegistration)' -count=1
```

Expected: route and registration tests pass.

- [ ] **Step 5: Commit**

```bash
git add internal/manager/app.go internal/manager/app_test.go internal/manager/adaptive_weekly_overdraft.go
git commit -m "feat: expose adaptive overdraft status"
```

## Task 7: Frontend Contracts and Mutually Exclusive Controls

**Files:**
- Modify: `web/src/types.ts`
- Modify: `web/src/api/client.ts`
- Modify: `web/src/api/client.test.ts`
- Modify: `web/src/components/OtherSettingsWorkspace.tsx`
- Modify: `web/src/components/OtherSettingsWorkspace.test.tsx`
- Modify: `web/scripts/mock-cpa.mjs`
- Modify: `web/src/i18n/uiText.ts`
- Modify: `web/src/i18n/uiCatalogEn.ts`
- Modify: `web/src/i18n/uiCatalogZhCN.ts`
- Modify: `web/src/i18n/uiCatalogZhTW.ts`
- Modify: `web/src/i18n/uiCatalogRu.ts`

- [ ] **Step 1: Write failing API and settings UI tests**

Assert the settings contract includes adaptive availability, enabling the original toggle clears adaptive, enabling adaptive clears original, a single save sends both booleans, and schema-v1 disables adaptive with a visible reason.

```ts
expect(JSON.parse(String(saveRequest?.init.body))).toEqual({
  weekly_overdraft_enabled: false,
  adaptive_weekly_overdraft_enabled: true,
  agent_identity_enabled: false,
  auto_model_whitelist_enabled: true,
});
```

- [ ] **Step 2: Run focused Vitest tests and verify RED**

```bash
npm test --prefix web -- --run src/api/client.test.ts src/components/OtherSettingsWorkspace.test.tsx
```

Expected: type/test failures because adaptive fields and controls do not exist.

- [ ] **Step 3: Implement contracts and controls**

Add TypeScript settings fields and availability metadata. Add an adaptive card with explicit schema-v2, 99-percent arming, `S1 -> S2 -> S4`, and hard-stop copy. Toggle handlers must atomically clear the opposite local state:

```ts
const toggleOriginalOverdraft = (enabled: boolean) => {
  setWeeklyOverdraftEnabled(enabled);
  if (enabled) setAdaptiveWeeklyOverdraftEnabled(false);
};

const toggleAdaptiveOverdraft = (enabled: boolean) => {
  setAdaptiveWeeklyOverdraftEnabled(enabled);
  if (enabled) setWeeklyOverdraftEnabled(false);
};
```

Update all four locale catalogs and the mock server. Do not change the original card's behavior text.

- [ ] **Step 4: Verify GREEN and source-contract completeness**

```bash
npm test --prefix web -- --run src/api/client.test.ts src/components/OtherSettingsWorkspace.test.tsx src/i18n/sourceContract.test.ts
npm run typecheck --prefix web
```

Expected: focused tests and typecheck pass.

- [ ] **Step 5: Commit**

```bash
git add web/src/types.ts web/src/api/client.ts web/src/api/client.test.ts web/src/components/OtherSettingsWorkspace.tsx web/src/components/OtherSettingsWorkspace.test.tsx web/scripts/mock-cpa.mjs web/src/i18n
git commit -m "feat: add adaptive overdraft controls"
```

## Task 8: Account Status and Separate Manual Test Actions

**Files:**
- Create: `web/src/components/AdaptiveWeeklyOverdraftStatus.tsx`
- Create: `web/src/components/AdaptiveWeeklyOverdraftStatus.test.tsx`
- Modify: `web/src/components/AccountUsageCell.tsx`
- Modify: `web/src/components/AccountUsageCell.test.tsx`
- Modify: `web/src/components/ModelTestDialog.tsx`
- Modify: `web/src/components/ModelTestDialog.test.tsx`
- Modify: `web/src/App.tsx`
- Modify: `web/src/api/client.ts`
- Modify: `web/src/types.ts`
- Modify: `web/src/i18n/uiText.ts`
- Modify: all `web/src/i18n/uiCatalog*.ts`

- [ ] **Step 1: Write failing rendering and interaction tests**

Test these visible states:
- `Adaptive S2` with success/token counters.
- `Workspace stopped · no further probes` for hard stop.
- no adaptive row for idle/no summary.
- original model-test button sends `experimental_weekly_overdraft` only.
- adaptive model-test button sends `experimental_adaptive_weekly_overdraft` only.

- [ ] **Step 2: Run focused tests and verify RED**

```bash
npm test --prefix web -- --run src/components/AdaptiveWeeklyOverdraftStatus.test.tsx src/components/AccountUsageCell.test.tsx src/components/ModelTestDialog.test.tsx
```

Expected: missing component, props, and API fields.

- [ ] **Step 3: Implement compact account status**

Render only sanitized summary fields. Use localized compact number formatting and never display fingerprints or raw reasons outside the fixed reason-code map.

- [ ] **Step 4: Implement distinct model-test modes**

Replace the boolean callback with a mode union:

```ts
export type OverdraftModelTestMode = "none" | "original" | "adaptive";

export async function testAccountModel(accountID: string, model: string, mode: OverdraftModelTestMode = "none"): Promise<ModelTestResult>
```

The dialog shows separate buttons only when their respective availability flag is true. App state derives those flags from the experimental settings snapshot.

- [ ] **Step 5: Verify GREEN**

```bash
npm test --prefix web -- --run src/components/AdaptiveWeeklyOverdraftStatus.test.tsx src/components/AccountUsageCell.test.tsx src/components/ModelTestDialog.test.tsx src/api/client.test.ts
npm run typecheck --prefix web
```

Expected: focused UI tests and typecheck pass.

- [ ] **Step 6: Commit**

```bash
git add web/src/components/AdaptiveWeeklyOverdraftStatus.tsx web/src/components/AdaptiveWeeklyOverdraftStatus.test.tsx web/src/components/AccountUsageCell.tsx web/src/components/AccountUsageCell.test.tsx web/src/components/ModelTestDialog.tsx web/src/components/ModelTestDialog.test.tsx web/src/App.tsx web/src/api/client.ts web/src/types.ts web/src/i18n
git commit -m "feat: show adaptive overdraft status"
```

## Task 9: Performance, Documentation, and Versioned Build Metadata

**Files:**
- Modify: `internal/manager/load_performance_test.go`
- Modify: `README.md`
- Modify: `README_EN.md`
- Modify: `Makefile` only if the existing linker version variable cannot accept a prerelease value without changes.

- [ ] **Step 1: Add failing/no-regression benchmarks**

Add benchmarks for disabled, non-Codex, below-threshold, and transformed `s1`/`s4` paths. Assert the below-threshold path reports zero body allocations in a dedicated `testing.AllocsPerRun` test after engine/account setup.

- [ ] **Step 2: Run performance tests**

```bash
go test ./internal/manager -run 'TestAdaptiveWeeklyOverdraftBelowThresholdAllocations' -bench 'BenchmarkAdaptiveWeeklyOverdraft' -benchmem -count=1
```

Expected: correctness test passes after implementation; record benchmark numbers in `progress.md` without adding a brittle timing threshold.

- [ ] **Step 3: Document configuration and rollback**

Document:
- both keys and mutual exclusion;
- schema-v2 requirement for adaptive mode;
- 99-percent/15-minute arming rule;
- bounded `s1 -> s2 -> s4` behavior;
- persistence path and secret-free contents;
- original-mode rollback procedure;
- no guarantee of a fixed continuation amount.

- [ ] **Step 4: Build a distinct local artifact**

Use the existing linker version mechanism:

```bash
make build VERSION=0.3.1300-adaptive.1
```

Expected: the build output is versioned separately from the official artifact. Do not deploy it.

- [ ] **Step 5: Commit**

```bash
git add internal/manager/load_performance_test.go README.md README_EN.md Makefile
git commit -m "docs: document adaptive overdraft mode"
```

If `Makefile` is unchanged, omit it from `git add`.

## Task 10: Full Verification and Review

**Files:**
- Modify: `docs/superpowers/plans/2026-07-29-adaptive-weekly-overdraft.md` — check off completed steps.
- Modify: outer planning files under `/Users/suo/work/project/CLIProxyAPI/planning/verify-codex-weekly-overdraft-online-20260729/` — record decisive evidence and artifact path.

- [ ] **Step 1: Format generated and modified code**

```bash
gofmt -w .
```

- [ ] **Step 2: Run complete Go verification**

```bash
go test ./...
go test -race ./...
go vet ./...
go build -o test-output ./cmd/server
rm test-output
```

Expected: every command exits 0 with no test failures or vet findings.

- [ ] **Step 3: Run complete frontend verification**

```bash
npm test --prefix web -- --run
npm run typecheck --prefix web
npm run build --prefix web
```

Expected: all Vitest files pass, TypeScript emits no errors, and Vite creates the hosted single-file build.

- [ ] **Step 4: Run native plugin verification**

```bash
make verify
make build VERSION=0.3.1300-adaptive.1
```

Expected: CGO/native checks and the distinct fork build pass.

- [ ] **Step 5: Audit requirements and secret boundaries**

```bash
rg -n 'TO[D]O|TB[D]|FIXM[E]|PLACEHOLDE[R]|\?\?\?' internal web README.md README_EN.md
rg -n 'secret-auth-id|Bearer secret|access_token|refresh_token' adaptive-weekly-overdraft.json internal/web/dist || true
git diff --check
git status --short
```

Expected: no unfinished markers in changed production/docs, no test secrets in built/persisted artifacts, no whitespace errors, and only intended files changed.

- [ ] **Step 6: Review branch diff against the design**

Check every acceptance criterion in `docs/superpowers/specs/2026-07-29-adaptive-weekly-overdraft-design.md`, record any deviation explicitly, and run focused tests for every corrected gap before claiming completion.

- [ ] **Step 7: Commit final generated assets and plan state**

```bash
git add internal/web/dist docs/superpowers/plans/2026-07-29-adaptive-weekly-overdraft.md
git commit -m "build: package adaptive overdraft plugin"
```

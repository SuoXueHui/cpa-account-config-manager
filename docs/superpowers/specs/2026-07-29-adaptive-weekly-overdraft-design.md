# Adaptive Weekly Overdraft Design

## Summary

Add an account-aware adaptive weekly-overdraft mode to the CPA Account Config
Manager plugin while preserving the upstream author's existing weekly-overdraft
mode unchanged. The two modes use separate, mutually exclusive switches.

The adaptive mode uses CPA request lifecycle schema v2 to make decisions after
credential selection. It does not modify CLIProxyAPI core, Auth files, account
identities, quota values, billing fields, or upstream responses. It starts with
the author's proven single completed custom-tool pair and escalates to two and
four completed pairs only after definitive weekly-quota failures. Authentication
and workspace lifecycle failures stop experimentation immediately.

## Goals

- Preserve the author's current `weekly_overdraft_enabled` implementation and
  behavior as an independent rollback and comparison path.
- Add `adaptive_weekly_overdraft_enabled` as a separate operator-controlled
  mode.
- Keep both switches mutually exclusive in configuration, persistence,
  Management API writes, and the UI.
- Avoid request changes below the adaptive arming threshold.
- Maximize verified continuation by escalating only when the current valid
  request shape receives a definitive weekly-quota failure.
- Select and remember a successful strategy independently for each stable CPA
  AuthID.
- Integrate with CPA account selection, failover, usage tracking, inspection,
  and automatic disable behavior.
- Persist only bounded, sanitized operational state.
- Keep legacy CPA hosts compatible with the original mode.

## Non-goals

- Bypassing `401` authentication failures or `402 deactivated_workspace`.
- Modifying upstream quota, plan, workspace, billing, or identity state.
- Proving or reporting upstream invoice charges.
- Actively exhausting healthy accounts for benchmarks.
- Adding an external proxy or patching CLIProxyAPI core.
- Providing arbitrary operator-defined request payloads or unbounded strategy
  counts.
- Guaranteeing a fixed amount of continuation. The allowance remains an
  upstream, account-dependent behavior.

## Compatibility Baseline

The implementation starts from plugin `v0.3.1300` and retains its existing
capability negotiation.

### CPA request lifecycle schema v2

Adaptive mode is available. `request.intercept_after` supplies
`selected_auth_id`, allowing the plugin to apply account-specific state after
CPA chooses a credential. `request.complete` and Usage Plugin records provide
completion, failure, quota-header, and token observations.

### Legacy CPA request lifecycle schema v1

The plugin continues to load and all existing features remain available. The
author's original weekly-overdraft switch remains functional. Adaptive mode is
reported as unavailable and cannot be enabled. It does not silently behave like
the original mode because that would produce misleading status and metrics.

## Operator Controls

`ExperimentalSettings` gains:

```yaml
experimental_settings:
  weekly_overdraft_enabled: false
  adaptive_weekly_overdraft_enabled: false
```

The existing key keeps its meaning. The new key defaults to `false`.

### Mutual exclusion

- A Management API write that enables both modes returns HTTP 400 and does not
  persist either requested change.
- UI toggles save both fields atomically and turn the other mode off.
- Existing configurations containing only `weekly_overdraft_enabled: true`
  retain original behavior after upgrade.
- If a manually edited startup configuration enables both fields, original mode
  wins, adaptive mode is forced off, and the settings snapshot exposes a bounded
  configuration warning. This prevents double injection while preserving the
  previously supported behavior.
- Adaptive mode cannot be enabled when the negotiated host schema is below v2;
  the Management API returns HTTP 400 with a non-secret compatibility error.

## Architecture

### Original weekly-overdraft engine

`WeeklyOverdraftExperiment` remains the original request transformer and
automatic-disable guard. Its payload, five-attempt plan, model selection, call ID
format, and persistence behavior are not changed. It is active only when
`weekly_overdraft_enabled` is true.

### Adaptive weekly-overdraft engine

Add `AdaptiveWeeklyOverdraftExperiment` under `internal/manager/`. It owns:

- The request transformer for adaptive tool-pair injection.
- A per-account state machine keyed by a SHA-256 fingerprint of stable AuthID.
- A bounded in-memory request ledger keyed by RequestID.
- Usage and completion observation.
- The adaptive automatic-disable guard and probe plan.
- A versioned, private state store.
- A sanitized Management API snapshot for the UI.

The engine receives account projections through the existing
`accountObserverGroup`. It stores only AuthID fingerprints and non-secret quota
and status fields. The observer lets the engine associate selected AuthIDs with
current seven-day usage without reading Auth JSON.

### Request hook composition

Register transformers in this order:

1. Account concurrency gate.
2. Author's original weekly-overdraft transformer.
3. Adaptive weekly-overdraft transformer.

Settings validation guarantees that only one overdraft transformer is active.
The concurrency gate remains first so a rejected request is never transformed
or counted as an overdraft attempt.

### Usage and completion flow

`App.HandleUsage` forwards sanitized `UsageRecord` data to the adaptive engine
after the existing usage tracker has observed it. `App.HandleRequestComplete`
forwards completion after concurrency cleanup.

Usage records provide AuthID, status/body classification, response quota
headers, and token totals. Request completion provides RequestID, selected-auth
metadata, outcome, and final HTTP status. The engine does not persist failure
bodies or headers.

## Request Strategies

Only valid Codex custom-tool continuation shapes are used:

| Strategy | Appended history | Purpose |
| --- | --- | --- |
| `s1` | One completed `custom_tool_call` and output | Author-compatible baseline |
| `s2` | Two completed call/output pairs | First escalation |
| `s4` | Four completed call/output pairs | Final bounded escalation |

Each pair uses a fresh `call_cpa_adaptive_<strategy>_<random>` call ID. Calls
use the existing no-op `exec` input and matching non-secret output. Calls and
outputs are always paired and ordered. The engine does not emit dangling calls,
standard function calls, arbitrary tools, or more than four pairs.

The request remains eligible only when:

- Adaptive mode is enabled and lifecycle schema v2 is active.
- `ToFormat` is Codex.
- `selected_auth_id` is present.
- The selected account has a seven-day usage observation from the current
  window, no older than 15 minutes, at or above 99 percent; or a current
  definitive weekly-quota failure has armed the account.
- The final input item is a user message.
- The body is non-empty and within the existing 32 MiB interceptor limit.
- The body does not already contain a CPA original or adaptive overdraft call.

Below 99 percent, the adaptive fast path returns without decoding or copying the
request body. Unknown or stale quota state fails open without transformation.

## Per-account State Machine

The persisted phases are:

| Phase | Meaning |
| --- | --- |
| `idle` | Seven-day usage is below 99 percent or unavailable. |
| `armed` | Usage reached 99 percent; `s1` is ready. |
| `active_s1` | `s1` has produced a successful request. |
| `active_s2` | `s2` has produced a successful request. |
| `active_s4` | `s4` has produced a successful request. |
| `exhausted` | Every bounded strategy received a definitive weekly failure. |
| `hard_stopped` | Credential or workspace lifecycle failure forbids probing. |

### Transitions

1. A current seven-day observation below 99 percent keeps or returns the account
   to `idle`.
2. An observation at or above 99 percent moves `idle` to `armed` and selects
   `s1`.
3. A successful transformed request records the current strategy as active.
4. A definitive `429 usage_limit_reached` advances `s1 -> s2 -> s4 ->
   exhausted`.
5. A generic 429 without weekly-quota evidence does not advance strategy; CPA
   retry and failover behavior remains authoritative.
6. HTTP 401 authentication failures and HTTP 402 workspace lifecycle failures
   move directly to `hard_stopped`.
7. A reset timestamp passing, or a fresh seven-day observation below 99 percent,
   clears normal strategy, exhaustion, and counters for a new quota window.
8. A `hard_stopped` account is cleared only by a later successful credential or
   quota observation, or by selection of a different stable AuthID. A weekly
   reset alone does not claim that an invalid credential or deactivated
   workspace recovered.

Strategy updates are serialized per account. The request ledger records the
strategy used by each in-flight RequestID so completions after an escalation do
not change the newer strategy. Post-99-percent token totals are account-window
totals rather than strategy-specific totals, avoiding false precision during
concurrent transitions.

## Failure Classification

Reuse the plugin's existing sanitized inspection classifiers rather than adding
parallel string matching.

- `usage_limit_reached`, `quota_exhausted`, or a seven-day quota-limited result:
  definitive adaptive escalation.
- Generic rate limiting, upstream unavailability, timeout, cancellation, and
  transport failure: no escalation and no hard stop.
- Invalid credentials: hard stop reason `authentication_failed`.
- Deactivated workspace/account: hard stop reason `deactivated_workspace` or
  the existing normalized lifecycle reason.
- Unsupported model: try the existing compatible fallback model without
  treating it as an overdraft strategy failure.

## Automatic Inspection and Disable

When adaptive mode is active, its guard replaces the original five identical
overdraft checks with a bounded strategy ladder:

1. Probe `s1` using the preferred allowed model.
2. On an unsupported model response, use the existing compatible fallback.
3. On definitive weekly quota failure, probe `s2`.
4. Repeat the same classification and then probe `s4`.
5. Any available response records the winning strategy and vetoes auto-disable.
6. Exhausting all strategies permits the existing CPA auto-disable action.
7. Authentication or workspace hard-stop results terminate the ladder
   immediately and permit the existing remediation.

The total remains below the existing ten-attempt hard bound. Transient failures
remain inconclusive and do not cause a false strategy escalation or destructive
action.

## Persistence

Store adaptive state at:

```text
<data_dir>/adaptive-weekly-overdraft.json
```

The file uses private permissions and a versioned envelope. Each bounded account
record contains only:

- SHA-256 AuthID fingerprint.
- Phase and selected strategy.
- Post-99-percent successful request count.
- Post-99-percent successful token count.
- Consecutive definitive failure count.
- Last success and failure timestamps.
- Current quota-window reset timestamp.
- Sanitized hard-stop reason.

The store never contains raw AuthID, AuthIndex, email, file name, token, cookie,
API key, proxy credential, request body, response body, or response headers.
Unknown store versions and corrupt state fail closed for adaptive mode while the
original mode and unrelated plugin features continue to work.

State is bounded to 10,000 account fingerprints. Expired reset windows and
records not observed for 31 days are pruned before persistence.

## Management API and UI

The experimental settings endpoint keeps its existing route and adds the new
boolean field plus adaptive availability metadata:

```json
{
  "settings": {
    "weekly_overdraft_enabled": false,
    "adaptive_weekly_overdraft_enabled": true
  },
  "adaptive_weekly_overdraft_available": true,
  "adaptive_weekly_overdraft_unavailable_reason": ""
}
```

Add a fixed authenticated GET route for sanitized adaptive account state. It
returns only account IDs already exposed by the authenticated account workspace,
phase, strategy, counters, timestamps, and normalized reason codes.

The Experimental Features UI presents two separate cards:

- **Author weekly overdraft**: existing behavior and wording.
- **Adaptive maximum overdraft**: schema requirement, 99-percent arming,
  escalation ladder, availability, and hard-stop behavior.

Enabling either card atomically disables the other. On legacy CPA, the adaptive
card is disabled with an explicit lifecycle-schema explanation.

Account usage cells display compact adaptive status such as:

```text
Adaptive S2
183 successes after 99% · 14.8M tokens
```

Hard-stop status is displayed without exposing upstream bodies:

```text
Workspace stopped · no further probes
```

Manual model testing offers distinct original and adaptive actions. Adaptive
testing follows the bounded ladder and reports applied strategy and sanitized
attempt results.

## Performance Requirements

- Both overdraft modes disabled: preserve the existing overdraft no-op fast
  path without changing unrelated request transformers.
- Adaptive enabled but selected account below 99 percent: no body decoding or
  copying.
- Non-Codex format: reject before body inspection.
- Account lookup and phase selection: bounded in-memory reads keyed by an AuthID
  fingerprint.
- No network call is made from the request interception hook.
- No global lock is held while decoding or rebuilding a request body.
- The existing 32 MiB request-body limit remains in force.

## Test Strategy

Implementation follows test-driven development.

### Settings and compatibility

- Defaults and persistence for both switches.
- Existing original-mode configurations survive upgrade unchanged.
- Management writes reject both modes enabled.
- Conflicting startup YAML preserves original mode and exposes a warning.
- Schema v1 keeps original mode and rejects adaptive enablement.
- Schema v2 exposes adaptive availability.

### Request transformation

- Golden payloads for exactly one, two, and four paired custom calls.
- Unique, paired, strategy-labelled call IDs.
- No transformation for non-Codex, below-threshold, missing selected auth,
  assistant-last, already-injected, invalid, empty, or oversized input.
- CPA failover uses the newly selected account's strategy.
- Concurrent requests do not regress or duplicate strategy transitions.

### State and classification

- Complete state-machine transition coverage.
- Immediate escalation for definitive weekly `usage_limit_reached`.
- No escalation for generic rate limits or transient failures.
- Immediate hard stop for normalized 401/402 failures.
- Reset clears normal window state and counters without falsely clearing a
  credential/workspace hard stop.
- Late completion from an earlier strategy cannot overwrite a newer strategy.

### Inspection and persistence

- Adaptive probe ladder stops on first success.
- Unsupported models use fallback without consuming a strategy failure.
- Every strategy failing permits auto-disable.
- Transient failure remains inconclusive.
- Private, versioned, bounded persistence excludes secret fixtures.
- Corrupt and future-version stores fail closed without breaking original mode.

### Regression and performance

- Existing original weekly-overdraft tests remain unchanged and pass.
- Full Go tests, race tests, `go vet`, frontend typecheck/tests/build, and CGO
  native build pass.
- Benchmarks cover inactive, non-Codex, below-threshold, and transformed paths.
- Existing account CRUD, import/export, policy, inspection, notification,
  Agent Identity, model-test, and concurrency suites remain green.

## Rollout

1. Build a versioned fork artifact such as `0.3.1300-adaptive.1` without
   overwriting the official release artifact.
2. Back up the deployed official library and plugin state files.
3. Install the fork with adaptive mode disabled and preserve the existing
   original-mode setting.
4. Verify registration, account listing, inspection, import/export, model tests,
   usage display, and focused original-mode probes.
5. Atomically disable original mode and enable adaptive mode.
6. Current valid accounts below 99 percent remain on the adaptive no-op path.
7. Observe the first natural 99-percent transition; do not generate artificial
   load to exhaust an account.

## Rollback

Restore the official library and its prior settings if any of these occurs:

- A non-Codex or below-threshold request is modified.
- A request contains duplicate or unpaired injected items.
- CPA account selection or failover is altered.
- Plugin panic, race, persistence corruption, or material latency regression.
- Success rate falls materially below the original-mode baseline.
- The adaptive guard causes remediation before its bounded ladder completes.

The adaptive state file is independent and can remain for forensic comparison
or be archived after rollback. The plugin never requires a CPA database or Auth
file migration.

## Acceptance Criteria

- The author's switch and behavior remain independently usable and regression
  tested.
- The adaptive switch is separately visible, defaults off, and is mutually
  exclusive with the author's switch.
- Legacy CPA runs the plugin safely and can use original mode.
- Schema v2 applies adaptive strategies per selected account only at or above
  the arming threshold.
- Definitive weekly failures escalate `s1 -> s2 -> s4`; 401/402 stop immediately.
- Successful post-threshold requests and tokens are reported per account window.
- All persisted and public state is bounded and secret-free.
- Full verification and a versioned native plugin build pass before deployment.

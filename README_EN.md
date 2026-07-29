# CPA Account Config Manager

[中文文档](README.md)

`cpa-account-config-manager` is a native
[CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) plugin for managing
CPA accounts from one authenticated workspace. It adds account CRUD, batch
operations, import/export conversion, quota visibility, model tests,
inspection automation, conditional policies, notifications, and an operation
journal without exposing raw credentials to the browser.

## Highlights

- List, search, filter, view, add, edit, enable, disable, deduplicate, and
  delete accounts. Page sizes include 20, 50, 100, 200, 500, and 1000.
- Run selected or filtered batch edits with a server-side preview, revision
  checks, bounded workers, per-account results, and failed-only retry.
- Display CPA request counters, token totals, Codex 5-hour and 7-day quota
  windows, plan type, and active reset count when CPA or the upstream account
  provides that data.
- Test provider models through CPA for Codex/OpenAI, Claude, Gemini/AI Studio,
  and xAI. Account allow-lists and deny-lists are respected by manual tests,
  automatic probes, and inspection.
- Detect restricted Codex model sets and apply a compatible account allow-list
  when policy-driven new-account probing is enabled. This capability is built
  in; it is not controlled by an experimental switch.
- Inspect all accounts with CPA-native evidence and optional active model
  probes. Results are published while a scan is running and include health,
  evidence, recovery time, and an executable recommendation.
- Optionally auto-disable failed or exhausted accounts, auto-enable only
  inspection-owned disables after recovery, and auto-delete narrowly eligible
  accounts after explicit confirmation and a grace period.
- Keep a persistent, redacted operation journal. Disable and enable entries
  include the actual normalized reason, such as quota exhaustion, invalid
  credentials, quota reset, health recovery, or credential refresh.
- Apply base presets to newly imported or discovered accounts, then override
  them with prioritized nested conditional policies for provider, plan,
  account type, and email suffix.
- Detect plugin updates through the CPA Plugin Store and display the current
  and latest CPA server versions. CPA executable updates remain read-only
  version guidance; the plugin never replaces the CPA binary.
- Read Codex plan and active-reset metadata and, after explicit confirmation,
  consume one available reset credit and refresh the account quota.
- Send previewable and testable HTTPS GET notifications to services such as
  Bark and ntfy. Generic and conditional notifications can combine account,
  availability, quota, and health variables.
- Preserve the author's original Codex 5-hour and 7-day quota overdraft mode and add a
  separate, mutually exclusive account-aware adaptive S1/S2/S4 mode, plus
  Agent Identity and personal access token import, login, and native auth.
- Follow the CPA Management Center language and theme. The UI supports English,
  Simplified Chinese, Traditional Chinese, and Russian.

## Account Transfer

Import accepts pasted JSON text or up to 10,000 accounts from mixed JSON,
JSON Lines, text, and ZIP files. A ZIP may contain multiple supported files.
Every import is previewed, bounded, duplicate-checked, and written in a
cancellable background job only through
CPA Auth callbacks; existing Auth files are not overwritten.

Supported inputs include native CPA files, common sub2api account collections,
Codex OAuth/PAT and Agent Identity variants, Claude, Gemini, and other formats
that can be converted into a CPA-supported Auth document. The recursive
importer also recognizes common account JSON structures emitted by Cockpit,
9router, AxonHub, and Codex Manager. Agent Identity conversion remains opt-in
under Experimental Features because it depends on upstream authentication
behavior.

Exports can target CPA, sub2api, Cockpit, 9router, Codex, AxonHub, and Codex
Manager formats. Formats that cannot represent multiple accounts as one file
are downloaded as a ZIP containing separate account files. Operational reports
are available as JSON, CSV, or JSON Lines and never contain credentials.

## Inspection And Automation

Inspection combines three evidence sources:

1. CPA account state, recent requests, and usage records.
2. Scheduled or manual active model probes through CPA.
3. Passive failures observed while CPA serves traffic.

Automatic actions are disabled by default. Auto-enable never claims an account
disabled by an operator or another system. Automatic deletion requires a
separate risk confirmation and remains limited to file-backed accounts with
strong, current evidence. Raw upstream bodies are not persisted in inspection
state or operation logs.

The default policy is incremental. Each stable account is processed once and
its processed identity is persisted across plugin and CPA restarts. Periodic
scans still discover new accounts, but already processed accounts are skipped.
Codex plan and active-reset metadata is refreshed before conditional rules are
evaluated. A quota endpoint HTTP 401 becomes sanitized invalid-credential
evidence for inspection instead of inflating the policy write-failure count.

Conditional policies can match nested `all`/`any` conditions using provider,
account type, plan type, and email suffix. Rules have explicit priorities and
can set account priority, WebSockets, new-account probing, and model policy.
The base policy runs first; conditional rules then override only the actions
they own.

External notifications support multiple operator-defined HTTPS GET templates.
Templates can combine allow-listed account and health variables, can be
previewed with current values, and have a test action. Delivery outcomes and
bounded HTTP metadata are recorded in the operation journal.
Each endpoint can remain generic or bind one ordered notification policy.
Policy notifications reuse nested provider, account-type, and email-suffix
conditions, then apply independent low-count and low-availability thresholds.

## Installation

Installing from the CPA Plugin Store is recommended because CPA selects the
platform archive, verifies checksums, and reports whether a host restart is
required. Manual release archives are also available for:

| Platform | Architecture | Library |
| --- | --- | --- |
| Linux | amd64 | `.so` |
| Linux | arm64 | `.so` |
| macOS | arm64 | `.dylib` |
| Windows | amd64 | `.dll` |

For a manual installation, verify the matching `.sha256` file, extract the
library into CPA's platform plugin directory, and enable it in `config.yaml`:

```yaml
plugins:
  enabled: true
  dir: plugins
  configs:
    cpa-account-config-manager:
      enabled: true
      priority: 20
```

After CPA loads the plugin, open **CPA-A Manager** in the Management Center.
Most CPA plugin-store updates need only a page refresh; restart CPA only when
the host reports `restart_required: true` or the loaded library is locked.

## Configuration And Persistence

The UI persists supported settings back to CPA's plugin configuration. These
optional plugin fields remain available for deployment-level configuration:

| Field | Default | Purpose |
| --- | --- | --- |
| `workers` | `6` | Concurrent account mutations, clamped to 1-16. |
| `data_dir` | `data/cpa-account-config-manager` | Private state for usage, inspection, policies, updates, runtime ownership, jobs, and logs. |
| `management_base_url` | `http://127.0.0.1:8317` | Loopback CPA Management API base for authenticated operations. |

Persist `data_dir` when CPA runs in a replaceable container. If no explicit
data directory is configured, the plugin can mirror sanitized usage state
under a common local Auth directory, but an explicit persistent mount is more
predictable. The CPA process needs read/write access to the Auth directory and
the effective plugin data directory.

Experimental Features currently contains:

- Codex 5-hour and 7-day quota overdraft probing, including the author's original mode
  and a separate adaptive maximum-overdraft mode. Both rely on upstream tool-call
  continuation behavior.
- Codex Agent Identity/PAT conversion and authentication hooks.

These features are off by default and isolated behind stable hooks so they can be removed
without changing the standard account-management paths.

### Codex Weekly-Quota Overdraft Modes

The two switches are persisted under the plugin's `experimental_settings`:

```yaml
experimental_settings:
  weekly_overdraft_enabled: false
  adaptive_weekly_overdraft_enabled: false
```

- `weekly_overdraft_enabled` is the author's original mode and retains its
  existing request transformation and inspection behavior.
- `adaptive_weekly_overdraft_enabled` is account-aware and requires CPA request
  lifecycle schema v2 for stable selected-account and completion events.
- The switches are mutually exclusive. Management writes that enable both are
  rejected. If a legacy startup configuration contains both, the plugin keeps
  the author mode enabled and disables adaptive mode.
- Adaptive injection is armed only when that account's seven-day quota reaches
  `99%` and the quota evidence is no more than `15` minutes old. Below-threshold,
  stale, non-Codex, and unattributed requests pass through unchanged.
- Each account starts at `s1` and escalates to `s2`, then `s4`, only after a
  definitive seven-day quota failure. Generic transient rate limits do not
  escalate. HTTP 401 authentication failures and HTTP 402/workspace
  deactivation stop further probing immediately.
- State is stored at `<data_dir>/adaptive-weekly-overdraft.json` with restrictive
  permissions. It contains account fingerprints, strategy, phase, counters,
  and timestamps, but no Auth IDs, tokens, request bodies, or upstream bodies.

To roll back to the author mode, turn off **Codex adaptive maximum overdraft**
and enable **Codex weekly quota overdraft continuation** in the UI, or set
`weekly_overdraft_enabled: true` and
`adaptive_weekly_overdraft_enabled: false`, then reload plugin configuration.
On a lifecycle schema v1 host, adaptive mode is unavailable while the author
mode remains supported.

Both modes depend on current upstream behavior. **Neither guarantees a fixed
continuation amount or that generation will continue.** Results can vary by
account, subscription, workspace state, and upstream policy.

## Security Model

- All privileged endpoints are fixed, authenticated CPA Management routes.
- The public resource route serves only the embedded static UI.
- Management keys remain in the current browser/CPA request path and are never
  persisted by the plugin.
- Raw Auth JSON, tokens, cookies, proxy credentials, header values, and raw
  upstream responses are excluded from public models, logs, and saved state.
- Imports and exports are count- and size-bounded. ZIP entries are validated
  against path traversal and archive expansion limits.
- Account mutations use previews, physical revisions, a shared writer lock,
  and conflict checks. Destructive actions require explicit confirmation.
- Private directories and files use restrictive permissions where supported.

## Compatibility

The plugin uses the CLIProxyAPI native plugin ABI and remains compatible with
request lifecycle schema v1; adaptive maximum-overdraft mode separately
requires lifecycle schema v2. CPA must also provide native plugin discovery,
Auth list/get/save callbacks, the Usage Plugin callback, and current
authenticated Management APIs for Auth status, field edits, account-selected
API calls, deletion, and plugin-store updates.
It does not import CLIProxyAPI Go packages and does not patch the CPA binary.

## Development

Prerequisites: Go 1.24+, Node.js 20+, npm, `make`, and a C toolchain suitable
for CGO.

```bash
make verify
make build
make package VERSION=X.Y.Z
```

`make verify` formats and tests Go code, tests and builds the React UI, checks
embedded assets, and validates release metadata. Release tags are annotated as
`vX.Y.Z`; the release workflow builds four platform archives, four matching
`.sha256` files, and `checksums.txt`.

## Acknowledgements

- Inspection design and remediation workflow:
  [seakee/CPA-Manager-Plus](https://github.com/seakee/CPA-Manager-Plus)
- Native inspection and job patterns:
  [ywddd/grok-inspection](https://github.com/ywddd/grok-inspection)
- Codex failure and quota presentation:
  [ysxk/codex-429-autoban](https://github.com/ysxk/codex-429-autoban) and
  [zhumengling/codex-token-usage](https://github.com/zhumengling/codex-token-usage)
- Agent Identity import and login concepts:
  [catoncat/codex-agent-identity-web](https://github.com/catoncat/codex-agent-identity-web)
- Community link: [LINUX DO](https://linux.do/)

These projects informed product behavior. Their code is not copied into this
plugin unless separately identified by the repository license history.

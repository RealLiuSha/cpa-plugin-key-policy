# cpa-key-policy

`cpa-key-policy` is a CLIProxyAPI plugin for issuing downstream keys, routing public model names to CPA capabilities, billing usage with a per-model multiplier, and enforcing RPM and USD limits. Version 1.0 answers exhausted quotas and RPM with HTTP 429, gives each public model exactly one upstream, and imports models at $0 so prices can be synced afterwards. This release targets Linux x64 on Debian 12 / glibc 2.36 or newer. See [release and upgrade instructions](RELEASE.md).

## Model domain

- A `ModelDefinition` owns the client-visible name, one upstream (`provider` + `target_model`), the billing mode, the prices and the billing multiplier.
- A `KeyModelRef` only grants a key access to a public model and may set a per-model daily USD limit.

Token-priced and per-call models are supported. Prices default to 0: a model without prices bills $0, which is how imported models start until their prices are synced. Negative prices are rejected.

Data format 6 removed multi-upstream routing (`targets`, `dispatch`), credential groups and classification rules, and the explicit `free` flag. Older state files and CPA `config.yaml` seeds still load; see [Upgrading data](#upgrading-data).

## Configuration

See [`config.example.yaml`](config.example.yaml). The current shape is:

```yaml
enabled: true
state_file: cpa-key-policy-state.json
usage_timezone: Asia/Shanghai

models:
  - name: fast
    provider: codex
    target_model: gpt-5.6
    billing_mode: tokens
    input_price_per_million: 1
    output_price_per_million: 2
    cache_read_price_per_million: 0.1
    cache_write_price_per_million: 0.3
    billing_multiplier: 1.1

keys:
  - id: team-a
    enabled: true
    key_hash: sha256:replace-with-hash
    models:
      - {name: fast, daily_limit_usd: 5}
    daily_limit_usd: 10
    weekly_limit_usd: 50
    monthly_limit_usd: 150
```

The first boot seeds keys and models from YAML and creates paired state and usage files with the same `dataset_id`. After that, the state file is authoritative for keys and models; YAML only controls `enabled`, `state_file`, and `usage_timezone`. Seeds written for older releases (`targets`, `dispatch`, `free`, `classify_rules`) are still accepted: a seed keeps its first target, the rest is ignored and a warning is logged. Startup strictly rejects an unsupported file version, a missing pair, or a mismatched dataset.

## Accounting

Consumption history remains in `days` / `by_model` with the existing 35-day retention. Each key now has independent daily, 7-day and 30-day quota cycles under `cycles`. Model counters are authoritative within each cycle; totals are derived from them.

Daily quotas reset at 00:00 in `usage_timezone` (default `Asia/Shanghai`). Longer quotas reset on fixed calendar-day schedules. A manual reset restores only the selected cycle immediately and sets its next reset to the operation date plus 1/7/30 days at 00:00. Other cycles and consumption history are preserved. Automatic advancement retains the original schedule across idle periods and downtime. Changing limits, enabled state or a key secret does not reset consumption. Usage is assigned when the host usage event reaches the ledger.

`usage.cycles` exposes the start, reset date, reset kind, used amount, limit and manual-reset preview date. `usage.status` distinguishes normal, warning, limited, partially limited and disabled keys. Existing daily/weekly/monthly USD fields now mean current fixed-cycle charges. History charts use `/keys/history`. The legacy `next_accounting_boundary_at` field remains the next midnight; new clients use each cycle's `resets_at`.

Token models accept a finite `billing_multiplier >= 1`, defaulting to `1`. Input, output, cache-read and cache-write charges all use the multiplier. Actual tokens and call counts remain unchanged. A zero cache-read price and a missing cache-write price fall back to the input price. Per-call models are not multiplied. Price imports preserve the multiplier and historical charges are never repriced. This setting affects the plugin ledger, not CPA raw usage or independent billing systems.

Per-call models used on image and video generation endpoints are charged when the request is admitted, because CPA may not report usage for them. A later usage report for the same request does not charge again, and a failed generation returns its charge.

## Request rejection

| Situation | Where | Client sees |
| --- | --- | --- |
| Unknown or disabled key | Authentication | 401 (CPA's generic answer) |
| Key does not reference the requested model | Authentication | 401 |
| RPM limit reached | Request interceptor | 429 `rate_limit_exceeded`, `Retry-After` = seconds left in the one-minute window |
| Daily, 7-day, 30-day or per-model daily quota used up | Request interceptor | 429 `insufficient_quota`, `Retry-After` = seconds until the latest exhausted cycle resets |

CPA turns any frontend-auth rejection into 401, while its request interceptor can terminate a request with any status before contacting upstream (CPA v7.2.103 and later). Requests are admitted in the interceptor when the key is sent in a header and the route is one CPA intercepts with an HTTP answer: chat/completions, completions, responses, messages, count_tokens, images, videos (generation and retrieval), `/v1beta/models/*`, interactions and the codex responses routes. Everything else is admitted during authentication with the previous 401 behavior: keys passed in the query string, realtime, live and alpha/search routes, and WebSocket handshakes on `/v1/responses` (a rejected WebSocket turn only closes the socket, so the handshake checks quota and RPM is counted per turn).

The 429 body follows the caller's protocol: OpenAI-style `{"error":{"message","type","code"}}`, Claude-style `{"type":"error","error":{"type":"rate_limit_error","message"}}`, or Gemini-style `{"error":{"code":429,"message","status":"RESOURCE_EXHAUSTED"}}`. Messages name the exhausted limit and when it resets in `usage_timezone`. Retrieving an already generated video is never blocked by quota.

CPA skips a request interceptor that returns an error, so a plugin fault lets that one request through; it is still billed by `usage.handle`. Keep at least one undistributed key in CPA's `access.api-keys`: if the plugin fails to load or is disabled, CPA then rejects every request instead of serving them unauthenticated.


## Management API and Web UI

The embedded UI has three areas: Keys, Models and Audit. Inside the CPA management panel it shows only its section tabs; opened on its own it adds the endpoint and a sign-out button. The management base path is:

```text
/v0/management/plugins/cpa-key-policy
```

Important routes:

| Route | Purpose |
| --- | --- |
| `GET/POST/PATCH/DELETE /keys` | Key lifecycle and model references |
| `POST /keys/rotate` | Rotate a key secret |
| `POST /keys/reset-usage` | Reset daily, 7-day, or 30-day buckets |
| `GET /keys/usage` | Per-model usage detail |
| `GET /keys/history` | Natural-day history with `by_model` |
| `GET/POST/DELETE /models` | Public model definitions (`name`, `provider`, `target_model`, prices, multiplier) |
| `POST /models/import` | Create models for CPA capabilities at $0; existing names are skipped, never overwritten |
| `POST /models/pricing-preview` | Fetch Models.dev prices for selected models |
| `POST /models/import-prices` | Preview or apply prices to existing models |
| `GET /audit` | Append-only management audit events |

Price import never creates models. It matches a price to a model by upstream model id first and by public name second, and keeps the multiplier.

## Build and verify

Prerequisites are Go 1.25, Node.js 20+, and Docker. Use npm and the committed `web/package-lock.json` for reproducible dependencies.

```bash
cd web
npm ci
npm test -- --run
npm run typecheck
npm audit --audit-level=moderate
VITE_HOSTED=1 npm run build

cd ..
cp web/dist/index.html internal/plugin/web/dist/index.html
go test ./...
go test -race ./...
go vet ./...
make check-version
make check-model-domain
make build-linux-amd64
```

The Linux artifact is `dist/cpa-key-policy_linux_amd64.so`, accompanied by a SHA-256 file and build metadata. The build runs in a pinned Debian 12 Go container, verifies ELF64/x86-64 and the plugin entry point, resolves dynamic dependencies, and loads the ABI before returning the artifact. It also works from an ARM64 development machine through Docker's amd64 emulation. GitHub releases build only Linux x64.

## Upgrading data

The plugin reads data formats 3 through 6 and converts older pairs to format 6 before publishing the runtime. Keys, key hashes, limits, quota cycles, history, prices, multipliers and model references are preserved. Format 6 keeps only the first upstream of a model (the primary under priority dispatch) and drops credential groups, classification rules and the `free` flag; free models already have zero prices and keep billing $0. Each dropped setting is logged and recorded once as a `migrate_state` audit event. Data older than format 5 also gets fixed quota cycles: old rolling-window usage becomes each new cycle's opening consumption, and the first reset date is the migration date plus 1/7/30 days at midnight.

The exact original pair is backed up to `<state_file>.before-v6.json` (base64 `state` and `usage`). Backup contents are validated independently. A re-upgrade after rollback archives the earlier recovery point before backing up the current data. Usage is persisted before state; an interrupted migration resumes without repeating opening balances. Older plugins cannot read format 6 and require their matching backup on rollback. See [RELEASE.md](RELEASE.md).

Run a read-only rehearsal on a private temporary copy:

```bash
go run ./cmd/cpa-key-policy-check --state /path/to/cpa-key-policy-state.json --timezone Asia/Shanghai
```

The report lists every dropped setting under `removed_settings` and public models whose name differs from the upstream id under `renamed_models`.

## Security and operational notes

- Store only hashes in configuration or state; generated plaintext keys are returned once.
- State and usage files may contain operationally sensitive metadata and must remain owner-readable only.
- Management audit events record semantic mutations. A failed audit append is logged but does not roll back a successful state mutation.
- The response interceptor rewrites non-stream response model IDs to the requested public model. Credential selection and token parsing otherwise retain CPA behavior.

Quota reset accepts an optional `expected: {started_at, reset_after_manual_at}` alongside `id` and `window`. The ledger checks the preview under its mutation lock and returns `409 quota_changed` if the period or proposed deadline changed. The UI refreshes and requires another confirmation; existing clients without `expected` retain the original request contract.

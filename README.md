# cpa-key-policy

`cpa-key-policy` is a CLIProxyAPI plugin for issuing downstream keys, routing public model names to CPA capabilities, and enforcing RPM and USD limits. Version 0.6 adds model imports and independent cache-write pricing. This release targets Linux x64 on Debian 12 / glibc 2.36 or newer. See [release and upgrade instructions](RELEASE.md).

## Model domain

- A `ModelDefinition` owns the client-visible name, one or more upstream routes, dispatch policy, billing mode, and global price.
- A `ModelTarget` selects a CPA `provider`, `target_model`, and optional credential `group`.
- A `KeyModelRef` only grants a key access to a public model and may set a per-model daily USD limit.
- Runtime routing resolves a target without persisting derived routes on the key.

Multiple targets support `round-robin` or `priority` dispatch. Token-priced, per-call, and explicitly free models are supported. A non-free model must have a positive active price; marking a model free requires every price field to be zero.

For example, an administrator can expose `asd` as one stable public model while routing it to both an upstream `gpt` capability and an upstream `grok` capability. Keys reference only `asd`; target selection and pricing remain owned by that single model definition.

## Configuration

See [`config.example.yaml`](config.example.yaml). The current shape is:

```yaml
enabled: true
state_file: cpa-key-policy-state.json
usage_timezone: Asia/Shanghai

models:
  - name: fast
    targets:
      - {provider: codex, target_model: gpt-5.6, group: team}
      - {provider: openai, target_model: gpt-5.6}
    dispatch: round-robin
    billing_mode: tokens
    free: false
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

The first boot seeds keys, models, and credential rules from YAML and creates paired state and usage files with the same `dataset_id`. After that, the state file is authoritative for all three managed domains; YAML only controls `enabled`, `state_file`, and `usage_timezone`. Startup strictly rejects an unsupported file version, a missing pair, or a mismatched dataset.

## Accounting

Consumption history remains in `days` / `by_model` with the existing 35-day retention. Each key now has independent daily, 7-day and 30-day quota cycles under `cycles`. Model counters are authoritative within each cycle; totals are derived from them.

Daily quotas reset at 00:00 in `usage_timezone` (default `Asia/Shanghai`). Longer quotas reset on fixed calendar-day schedules. A manual reset restores only the selected cycle immediately and sets its next reset to the operation date plus 1/7/30 days at 00:00. Other cycles and consumption history are preserved. Automatic advancement retains the original schedule across idle periods and downtime. Changing limits, enabled state or a key secret does not reset consumption. Usage is assigned when the host usage event reaches the ledger.

`usage.cycles` exposes the start, reset date, reset kind, used amount, limit and manual-reset preview date. `usage.status` distinguishes normal, warning, limited, partially limited and disabled keys. Existing daily/weekly/monthly USD fields now mean current fixed-cycle charges. History charts use `/keys/history`. The legacy `next_accounting_boundary_at` field remains the next midnight; new clients use each cycle's `resets_at`.

Token models accept a finite `billing_multiplier >= 1`, defaulting to `1`. Input, output, cache-read and cache-write charges all use the multiplier. Actual tokens and call counts remain unchanged. Free and per-call models are unaffected. Price imports preserve the multiplier and historical charges are never repriced. This setting affects the plugin ledger, not CPA raw usage or independent billing systems.

## Management API and Web UI

The embedded UI has four primary areas: Keys, Models, Credential Groups, and Audit. The management base path is:

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
| `GET/POST/DELETE /models` | Public model definitions |
| `POST /models/import-prices` | Preview or apply existing-model prices |
| `POST /models/import` | Preview or apply a batch of new models or explicit overwrites |
| `POST /models/pricing-preview` | Fetch Models.dev prices for selected models |
| `GET/POST/DELETE /classify-rules` | Credential-group rules |
| `POST /classify-rules/reorder` | Rule priority |
| `POST /classify-preview` | Preview credential classification |
| `POST /catalog` | Build the current CPA capability catalog |
| `GET /audit` | Append-only management audit events |

Model price import never creates models, never changes free models, and only applies target-derived prices when all targets of a multi-target model are matched consistently.

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

The plugin reads v3/v4/v5 data and migrates legacy pairs to v5 before publishing the runtime. Old rolling-window usage becomes each new cycle's opening consumption; no automatic credit is granted. The first reset date is the migration date plus 1/7/30 days at midnight. Key identity, permissions, limits and base model prices are preserved.

The exact original pair is backed up to `<state_file>.before-v5.json` (base64 `state` and `usage`). Backup contents are validated independently. A re-upgrade after rollback archives the earlier recovery point before backing up the current data. The cycle ledger is persisted before configuration; interrupted migration resumes without repeating opening balances. Old plugins require their matching old data on rollback. See [RELEASE.md](RELEASE.md).

Run a read-only rehearsal on a private temporary copy:

```bash
go run ./cmd/cpa-key-policy-check --state /path/to/cpa-key-policy-state.json --timezone Asia/Shanghai
```

Policy rejection continues to use the host's generic 401. The unsupported `Rejection` output has been removed; internal reasons remain available to the policy layer and quota management UI. Request interceptors that can be skipped on errors are not used to replace quota enforcement during authentication.

## Security and operational notes

- Store only hashes in configuration or state; generated plaintext keys are returned once.
- State and usage files may contain operationally sensitive metadata and must remain owner-readable only.
- Management audit events record semantic mutations. A failed audit append is logged but does not roll back a successful state mutation.
- The response interceptor rewrites non-stream response model IDs to the requested public model. Provider routing, scheduler selection, and billing formulas otherwise retain CPA behavior.

Quota reset accepts an optional `expected: {started_at, reset_after_manual_at}` alongside `id` and `window`. The ledger checks the preview under its mutation lock and returns `409 quota_changed` if the period or proposed deadline changed. The UI refreshes and requires another confirmation; existing clients without `expected` retain the original request contract.

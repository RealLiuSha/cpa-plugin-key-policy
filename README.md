# cpa-key-policy

`cpa-key-policy` is a CLIProxyAPI plugin for issuing downstream keys, routing public model names to CPA capabilities, and enforcing RPM and USD limits. Version 0.5 exposes one current model domain and one strict persistence contract.

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

Usage is stored as natural-day buckets under `by_model`. For every key and date, the key bucket must exactly equal the sum of its model buckets. Daily, trailing-7-day, and trailing-30-day totals are derived from the same buckets.

Limits are evaluated for:

- key RPM;
- key daily, trailing-7-day, and trailing-30-day USD totals;
- per-model daily USD totals.

The API exposes one `next_accounting_boundary_at` for the next natural-day boundary. Free models still count calls and tokens but add zero cost.

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
| `GET/POST/DELETE /classify-rules` | Credential-group rules |
| `POST /classify-rules/reorder` | Rule priority |
| `POST /classify-preview` | Preview credential classification |
| `POST /catalog` | Build the current CPA capability catalog |
| `GET /audit` | Append-only management audit events |

Model price import never creates models, never changes free models, and only applies target-derived prices when all targets of a multi-target model are matched consistently.

## Build and verify

Prerequisites are Go 1.25 and Node.js 20+.

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

The Linux artifact is `dist/cpa-key-policy_linux_amd64.so`.

## Security and operational notes

- Store only hashes in configuration or state; generated plaintext keys are returned once.
- State and usage files may contain operationally sensitive metadata and must remain owner-readable only.
- Management audit events record semantic mutations. A failed audit append is logged but does not roll back a successful state mutation.
- The response interceptor rewrites non-stream response model IDs to the requested public model. Provider routing, scheduler selection, and billing formulas otherwise retain CPA behavior.

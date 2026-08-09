# cpa-key-policy

`cpa-key-policy` is a CLIProxyAPI plugin for issuing downstream keys, routing public model names to CPA capabilities, and enforcing RPM and USD limits. Version 0.5 uses a pure v3 model domain and is intentionally incompatible with older state files.

## Model domain

- A `ModelDefinition` owns the client-visible name, one or more upstream routes, dispatch policy, billing mode, and global price.
- A `ModelTarget` selects a CPA `provider`, `target_model`, and optional credential `group`.
- A `KeyModelRef` only grants a key access to a public model and may set a per-model daily USD limit.
- Runtime routing resolves a target without persisting derived routes on the key.

Multiple targets support `round-robin` or `priority` dispatch. Token-priced, per-call, and explicitly free models are supported. A non-free model must have a positive active price; marking a model free requires every price field to be zero.

For example, an administrator can expose `asd` as one stable public model while routing it to both an upstream `gpt` capability and an upstream `grok` capability. Keys reference only `asd`; target selection and pricing remain owned by that single model definition.

## Configuration

See [`config.example.yaml`](config.example.yaml). The v3 shape is:

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

The first boot creates paired state and usage files with the same `dataset_id`. Later startups reject missing pairs, mismatched datasets, v1/v2 files, and future versions. Existing state remains authoritative for keys; non-empty model or credential-rule configuration can intentionally override those definitions during reconfigure.

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
VITE_HOSTED=1 npm run build

cd ..
cp web/dist/index.html internal/plugin/web/dist/index.html
go test ./...
go vet ./...
make build-linux-amd64
make check-migrator-linux-amd64
```

The plugin artifact is `dist/cpa-key-policy_linux_amd64.so`; the offline migration artifacts are `dist/migrate-model-schema_linux_amd64` and its `.sha256` file.

## Upgrade from v2

The runtime does not migrate old files automatically. Follow [`docs/migrate-v2-to-v3.md`](docs/migrate-v2-to-v3.md) while CPA is stopped, migrate one instance at a time, and retain the rollback package after validation.

## Security and operational notes

- Store only hashes in configuration or state; generated plaintext keys are returned once.
- State, usage, and rollback files may contain operationally sensitive metadata and must remain owner-readable only.
- Management audit events record semantic mutations. A failed audit append is logged but does not roll back a successful state mutation.
- The response interceptor rewrites non-stream response model IDs to the requested public model. Provider routing, scheduler selection, and billing formulas otherwise retain CPA behavior.

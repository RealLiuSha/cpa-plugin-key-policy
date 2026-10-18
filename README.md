# cpa-key-policy

`cpa-key-policy` is a CLIProxyAPI plugin for issuing downstream keys, pricing upstream models with a per-model multiplier, billing usage and enforcing RPM and USD limits. Exhausted quotas and RPM answer with HTTP 429, every enabled key may use every model CPA serves, and a model without a price bills $0 and is flagged until a price is set. This release targets Linux x64 on Debian 12 / glibc 2.36 or newer and writes data format 7. See [release and upgrade instructions](RELEASE.md).

## Model prices

The model layer is a price table. A `ModelPrice` row prices one upstream model id (`name`, the `Model` CPA reports in usage records, compared without case) with an optional `provider` qualifier, the billing mode (`tokens` or `per_call`), the prices and the billing multiplier. The same `(name, provider)` pair exists once. Lookup prefers the row matching `(provider, model)` and falls back to the row with an empty provider; CPA's `openai-compatible-<name>` provider keys are compared by `<name>`.

There are no public model names, no routing and no per-key model lists. CPA routes every request by its own registry: when a model is served by several providers, CPA chooses credentials across them and fails over; the response carries whatever model upstream reports. Any enabled key may call any model and may always read the model lists `/v1/models`, `/openai/v1/models` and `/v1beta/models`: listing counts neither RPM nor quota and works while a quota is used up. Gemini generation routes under `/v1beta/models/<model>:…` are admitted like any other call.

A call is unpriced when no row matches, when a token row has every price at 0, or when a per-call row has `per_call_usd` 0. Unpriced calls bill $0; their tokens and calls are still recorded and each is counted in `unpriced_calls`. `GET /models` lists under `unpriced` the models whose calls in the last 90 days matched no row and that still have no row for any provider; `has_qualified_rows: true` marks a model whose rows are all provider-qualified, so calls from other providers missed them (check the qualifier or add a row without one). An all-zero row is flagged in the price table itself (`priced: false`). The `priced` flag of `GET /keys/usage` and `GET /stats` model rows is the ledger's: every call shown was charged; a model without calls falls back to its row. Token prices are USD per million tokens between 0 and 10000, `per_call_usd` between 0 and 10000, `billing_multiplier` between 1 and 50 (default 1).

## Configuration

See [`config.example.yaml`](config.example.yaml). The current shape is:

```yaml
enabled: true
state_file: cpa-key-policy-state.json
usage_timezone: Asia/Shanghai

models:
  - name: gpt-5.6
    provider: codex
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
    rpm: 60
    daily_limit_usd: 10
    weekly_limit_usd: 50
    monthly_limit_usd: 150
```

The first boot seeds keys and price rows from YAML and creates a state file and a usage file with the same `dataset_id`. After that, the state file is authoritative for keys and prices; YAML only controls `enabled`, `state_file` and `usage_timezone`. Settings of earlier releases (`target_model`, `targets`, `dispatch`, `free`, `group`, `classify_rules`, `keys[].models`, `keys[].allow_models_endpoint`, `keys[].limits_changed_at`) are unknown fields and fail registration; check a configuration with `cpa-key-policy-check --config` before switching. Two seed keys sharing one secret are rejected on first boot. Startup strictly rejects an unsupported file version, a missing pair, a mismatched dataset, a usage file written for another timezone and a history watermark (`through`) later than today in `usage_timezone`.

## Accounting

Amounts are integer nanodollars inside the ledger (1 USD = 1e9), so totals are exact and deterministic; the management API reports USD as floats and configuration keeps USD prices and limits. The ledger holds one bucket per key, model and natural day for the last 90 days (`calls, unpriced_calls, input_tokens, cache_read_tokens, cache_write_tokens, output_tokens, charged_nano, cache_read_cost_nano, cache_write_cost_nano`) plus each key's three quota cycles; day totals are computed from the model buckets when read. Three files share one `dataset_id`:

- `<state_file>` (indented JSON, `version` 7): keys and price rows.
- `cpa-key-policy-usage.json` (compact JSON): every key's quota cycles, a balance key's `balance` (`balance_nano`, `net_credited_nano` and the remembered request ids) and the days newer than `history_through`. It is rewritten whole every 15 seconds while dirty.
- `cpa-key-policy-history.json` (compact JSON): the closed days up to `through` inside retention. It is rewritten on the first flush after midnight and again when retention drops a day; a closed day is never rewritten otherwise. For 1000 keys this takes about 100–300 ms, blocks only the flusher goroutine and never holds the ledger lock.

Snapshots are taken under the ledger lock and serialized outside it; a failed write keeps the data dirty, is retried on the next tick and is visible in `/status` as `health.last_flush_error`. Unloading the plugin writes a final cut, history first. A live file that names a history day but has no history file, a mismatched `dataset_id`, a foreign timezone or a negative counter refuses startup rather than repairing the ledger. A record whose date falls on a closed day, after the clock was turned back, is booked on the first open day, so closed history never changes. Deleting a key removes its cycles and RPM window at once and writes the live file immediately (a failed write is logged and left to the flusher); its history stays until it expires and is reported as deleted in statistics.

Each key has independent daily, 7-day and 30-day quota cycles under `cycles`. Daily quotas reset at 00:00 in `usage_timezone` (default `Asia/Shanghai`). The 7-day and 30-day cycles are fixed calendar-day periods anchored at the key's creation: the first deadline is the creation day plus 7 or 30 days at 00:00, automatic advancement continues from the original deadline across idle periods and downtime, and a manual reset restores only the selected cycle and sets its next deadline to the operation day plus 1/7/30 days at 00:00. Other cycles and history are preserved. Changing limits, enabled state or a key secret does not reset consumption. A manual reset writes the live file under the ledger lock so the file holds exactly one accounting cut (about 10–25 ms at 1000 keys); this is deliberate and is revisited when a reset exceeds 50 ms.

A key has one of two quota types, `quota_mode`. `cycle`, the default, is limited by the daily, 7-day and 30-day quotas described above. `balance` is prepaid: it has no period limits, and its only limit is a balance that administrators adjust with `POST /keys/balance` (a positive `amount_usd` adds credit, a negative one deducts). Every billed call of a balance key is deducted from its balance in the same lock that books its cycles, and the key is refused once the balance is 0 or below; since billing follows a call, calls in flight when the balance ran out can leave it slightly negative. An adjustment is written to the live file before it is answered and recorded as an `adjust_balance` audit event with the amount, the balance before and after, the note and the request id; deductions are flushed every 15 seconds with the rest of the ledger, so a crash loses at most the last 15 seconds of deductions. A `request_id` makes a retried adjustment return its first result (`duplicate: true`) instead of adjusting twice; the last 64 ids per key are kept, and the same id with another amount is refused with 409 `request_id_conflict`. Switching a key to `balance` clears its period limits, and a balance key given period limits is refused; switching back to `cycle` keeps the balance unspent until the key is switched to `balance` again. Deleting a key drops its balance, which the `delete_key` audit event names. The cycles keep counting for balance keys and drive their usage page and statistics.

`usage.cycles` exposes the start, reset date, reset kind, used amount, limit and manual-reset preview date. `usage.status` is `normal`, `warning`, `limited` or `disabled`; `blocked_reason` names the exhausted cycle, or `balance_exhausted`. `usage.quota_mode` repeats the key's quota type; `balance_usd` and `net_credited_usd` (the signed sum of all adjustments) appear for balance keys and for a cycle key that kept a balance, and a balance key is `warning` once 80% of its net credit is spent. `daily_usd`, `weekly_usd` and `monthly_usd` are the current cycle charges. History charts use `/keys/history`. `next_accounting_boundary_at` equals the daily cycle's `resets_at`.

Billing happens once, when CPA delivers the usage record of a finished call:

- A record with `Failed` set is never billed. This includes streams the client cancelled, which CPA records as failed with status 499: their partial tokens are not charged.
- The key is matched by id, then by plaintext hash. It need not still be enabled: the request was authorized when it started.
- The model is the upstream `Model`; the requested name is used only when that is empty. Bucket keys are the lowercased id, so per-model totals never split by the case upstream reported.
- Negative token counts become 0. An input, output, reasoning, cache-read or cache-write count above 2^31 rejects the whole record (`invalid_usage`); an out-of-range `total_tokens` or `cached_tokens` is only set to 0, since neither is billed. A record that needed either correction counts once in `clamped_tokens`.
- A token record with no billable tokens after classification (input, cache read, cache write and output all 0) is ignored when its model is priced (`ignored_empty_usage`). When the model is unpriced it is recorded as one call with `unpriced_calls` 1, so a new model whose records carry no tokens still shows up as unpriced; a video model without a `per_call` row therefore also counts every result poll as a call until it is priced.
- Token classes follow CPA's own accounting per provider. For Claude and Anthropic providers cache reads and writes are outside the input count; for Gemini, AI Studio, Vertex, Antigravity and interactions the reasoning tokens are outside the output count; everyone else reports cache reads inside the input and reasoning inside the output. Cache reads come only from `CacheReadTokens`. A zero cache-read price bills reads at the input price; without a cache-write price, cache writes are billed as input. Input, output, cache-read and cache-write charges all use the multiplier; token counts stay real. Each of the four classes is rounded to a nanodollar on its own and the charge is their sum, so the cache amounts shown separately never exceed it. Changing a price or multiplier affects later calls only.
- A `per_call` row bills once per admitted media generation request. The request interceptor remembers the trace id of each admitted POST on the image and video generation routes CPA intercepts (`/v1/images/generations`, `/v1/images/edits`, `/v1/videos`, `/v1/videos/generations`, `/v1/videos/edits`, `/v1/videos/extensions`, `/openai/v1/videos`; 4096 entries, 1 hour), and the usage record that carries the same trace id is charged `per_call_usd`. Every other record of a per-call model is left unmetered (`unmetered_per_call`): result polling, an expired trace, a host without trace ids, a media route CPA does not intercept (such as `/openai/v1/images/*`), and the image model record that a Codex `image_generation` tool call on `/v1/responses` adds. Price models that report tokens, such as Codex's `gpt-image-*`, by token. Losing a trace makes one call free, never double-billed.

`GET /status` exposes `health`: `last_flush_at`, `last_flush_error`, `flush_failures`, `skipped_failed`, `ignored_unknown_key`, `ignored_no_model`, `ignored_empty_usage`, `clamped_tokens`, `invalid_usage`, `unmetered_per_call` and `unpriced_calls`. The counters live in memory and start at zero on every start.

## Request rejection

| Situation | Where | Client sees |
| --- | --- | --- |
| Unknown or disabled key, plugin disabled | Authentication | 401 (CPA's generic answer) |
| Key only in the query string on an image or video generation route | Authentication | 401 |
| RPM limit reached | Request interceptor | 429 `rate_limit_exceeded`, `Retry-After` = seconds left in the one-minute window |
| Prepaid balance used up (balance keys) | Request interceptor | 429 `insufficient_quota` on every OpenAI-style route including the Responses API, without `Retry-After`: a balance never resets on its own |
| Daily, 7-day or 30-day quota used up | Request interceptor | 429 `insufficient_quota`, `Retry-After` = seconds until the latest exhausted cycle resets; on the Responses API `type` is `usage_limit_reached` with `resets_at` (Unix seconds) so Codex shows the reset time, and `code` stays `insufficient_quota` |
| Model lists (`/v1/models`, `/openai/v1/models`, `/v1beta/models`) | Authentication | Always allowed for an enabled key, without RPM or quota |

Admission checks RPM first, then the quotas, and names the exhausted cycle that resets last. CPA turns any frontend-auth rejection into 401, while its request interceptor can terminate a request with any status before contacting upstream (CPA v7.2.103 and later). Requests are admitted in the interceptor when the key is sent in a header and the route is one CPA intercepts with an HTTP answer: chat/completions, completions, responses, messages, count_tokens, images, videos (generation and retrieval), `/v1beta/models/*`, interactions and the codex responses routes. Everything else is admitted during authentication with the 401 behavior: keys passed in the query string, realtime, live and alpha/search routes, and WebSocket handshakes on `/v1/responses` (a rejected WebSocket turn only closes the socket, so the handshake checks quota and RPM is counted per turn). A key sent only in the query string is refused on a media generation route, because the interceptor cannot see it and per-call billing depends on the trace id the interceptor records.

The 429 body follows the caller's protocol: OpenAI-style `{"error":{"message","type","code"}}`, Claude-style `{"type":"error","error":{"type":"rate_limit_error","message"}}`, or Gemini-style `{"error":{"code":429,"message","status":"RESOURCE_EXHAUSTED"}}`. Messages name the exhausted limit and when it resets in `usage_timezone`. Retrieving an already generated video is never blocked by quota.

CPA skips a request interceptor that returns an error, so a plugin fault lets that one request through; it is still billed by `usage.handle`. Keep at least one undistributed key in CPA's `access.api-keys`: if the plugin fails to load or is disabled, CPA then rejects every request instead of serving them unauthenticated.

## Management API and Web UI

The embedded UI has four areas: Keys, Model prices, Stats and Audit. Inside the CPA management panel it shows only its section tabs; opened on its own it adds the endpoint and a sign-out button. The management base path is:

```text
/v0/management/plugins/cpa-key-policy
```

Important routes:

| Route | Purpose |
| --- | --- |
| `GET/POST/PATCH/DELETE /keys` | Key lifecycle |
| `POST /keys/rotate` | Rotate a key secret |
| `POST /keys/reset-rpm` | Clear a key's RPM window |
| `POST /keys/reset-usage` | Reset the daily, 7-day or 30-day cycle |
| `POST /keys/balance` | Add to or deduct from a balance key's prepaid balance: `{id, amount_usd, note?, request_id?}` |
| `GET /keys/usage` | Per-model usage in the key's current cycles |
| `GET /keys/history` | Natural-day history with `by_model` (`days` 1..90, default 30) |
| `GET /stats` | Totals, per-day, per-model and top-key usage across all keys (`days` 1..90) |
| `GET /stats/export` | CSV of every (day, key, model) bucket in `from`..`to` (at most 92 days, 200000 rows) |
| `GET/POST/DELETE /models` | Price rows (`name`, optional `provider`, billing mode, prices, multiplier) and the `unpriced` list |
| `POST /models/import` | Create $0 rows for upstream models; existing rows are skipped, never overwritten |
| `POST /models/pricing-preview` | Fetch Models.dev prices for selected models |
| `POST /models/import-prices` | Preview or apply prices to existing rows |
| `GET /audit` | Append-only management audit events, newest first; `key_id` and `action` select one key and one action, `limit` (1–1000, default 100) counts the matching events |
| `GET /status` | Runtime status and ledger health counters |

`GET /keys` items carry `id, name, enabled, key_preview, rpm, daily_limit_usd, weekly_limit_usd, monthly_limit_usd, quota_mode, usage, created_at, updated_at`; `POST /keys` and `PATCH /keys` accept `quota_mode` (`cycle` or `balance`), and a PATCH that switches to `balance` clears the period limits it does not set. `POST /keys/balance` answers `{id, amount_usd, balance_before_usd, balance_usd, net_credited_usd, duplicate}`; it refuses an amount of 0 or beyond ±1,000,000 and a note over 200 characters with 400 `invalid_amount`, a cycle key with 409 `not_balance_mode` and an unknown key with 404. `POST /keys` and `PATCH /keys` reject unknown fields but accept and ignore `models` and `allow_models_endpoint`, which earlier releases had, so existing writers keep working. A secret already used by another key is refused with 409 `duplicate_key` on create, update and rotate. `DELETE /models` answers 404 `not_found` for a row that does not exist. A management write whose state file cannot be written answers 500 `persist_failed` and leaves the running policy unchanged.

`GET /keys/usage` lists the models seen in the key's current cycles with `billing_mode`, `billing_multiplier`, `priced` and the `daily`, `weekly` and `monthly` windows. `GET /stats` returns at most 200 keys ordered by charge and sets `keys_truncated` with `keys_total` when more exist; it also lists `unpriced_models`. The CSV export has the columns `date,key_id,key_name,deleted,model,calls,unpriced_calls,input_tokens,cache_read_tokens,cache_write_tokens,output_tokens,charged_usd`, prints amounts as exact decimals and prefixes cells starting with `=`, `+`, `-` or `@` with a quote. A range over the limit answers 413 `range_too_large`.

Quota reset accepts an optional `expected: {started_at, reset_after_manual_at}` alongside `id` and `window`. The ledger checks the preview under its mutation lock and returns `409 quota_changed` if the period or proposed deadline changed. The UI refreshes and requires another confirmation; existing clients without `expected` retain the original request contract.

Price import never creates rows. It applies token prices to every row whose model id matches, regardless of provider qualifier, and keeps the multiplier.

## Build and verify

Prerequisites are Go 1.25, Node.js 20+, and Docker. Use npm and the committed `web/package-lock.json` for reproducible dependencies.

```bash
cd web
npm ci
npm test -- --run
npm run typecheck
npm audit --omit=dev --audit-level=moderate
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

The plugin reads data format 7 only and refuses any other version at start. A format 6 dataset (0.11.x) is converted offline, while the instance is stopped, into a new set of format 7 files; `scripts/publish.py` does it during its maintenance window, and by hand it is:

```bash
go run ./cmd/cpa-key-policy-check --state /path/to/cpa-key-policy-state.json --timezone Asia/Shanghai --out /path/to/new-empty-dir
```

The source files are only read. The tool writes state, live usage and history into the output directory and compares them with the source (dataset id, every key's identity and limits, the price rows projected from the models, every retained bucket's counters and amounts, every quota cycle's interval, kind and consumption); it then starts and restarts the plugin's store on a private copy to prove the files load and a restart changes nothing, so the output stays exactly as converted. A source that would not convert or load is refused before anything is written. Without `--out` it rehearses the conversion in a private directory; on a format 7 dataset it only loads and reloads.

The conversion keeps keys, key hashes, limits, quota cycles and history inside retention; it drops each key's model list, model list switch and per-model daily limits, projects every model onto a price row keyed by its upstream id (`target_model`; when several public names pointed at one upstream model, the name equal to the upstream id wins, otherwise the first in lexical order, and the rest are dropped), converts amounts to nanodollars and drops the redundant day totals after checking them against the model buckets. The report lists what was dropped or chosen under `removed_settings` (ignored per-key model authorizations, removed per-model daily limits, keys that had `/v1/models` disabled, renamed and dropped price rows, recomputed day totals, keys sharing one secret), models whose public name differed from the upstream id under `renamed_models`, the counts under `conversion`, and `policy_sha256`, a normalized hash of keys and prices that must be identical before and after the start.

To install the result, move the old state file, its usage file and any `.before-v*.json` recovery files out of the state directory (keep them with the backup) and move the three new files in; the audit log stays. Older plugins cannot read format 7: roll back by restoring the backup taken before the switch. The prepaid balance fields (`quota_mode` in state, `balance` in the live file) are written only once a key uses them, so releases before them still read a dataset in which no key ever became a balance key; after that, rolling back to such a release also needs the backup. See [RELEASE.md](RELEASE.md).

`go run ./cmd/cpa-key-policy-check --config /path/to/config.yaml` checks that this release accepts the `cpa-key-policy` section of a CPA configuration, since a rejected section fails registration.

## Security and operational notes

- Store only hashes in configuration or state; generated plaintext keys are returned once.
- State, usage and history files may contain operationally sensitive metadata and must remain owner-readable only.
- Management audit events record semantic mutations and never a secret or hash. A failed audit append is logged but does not roll back a successful state mutation.
- Credential selection and token parsing retain CPA behavior; the plugin registers neither a model router nor a response interceptor.

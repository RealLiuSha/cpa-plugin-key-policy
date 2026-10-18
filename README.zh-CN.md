# cpa-key-policy

`cpa-key-policy` 是 CLIProxyAPI 插件，用于签发下游 Key、按上游模型定价并按倍率计费、记录用量，并执行 RPM 与美元限额。额度耗尽和 RPM 超限返回 HTTP 429；任何启用的 Key 都能使用 CPA 提供的所有模型；没有价格的模型按 $0 计费并被标记，直到补上价格。本次发布面向 Linux x64，运行基线为 Debian 12 / glibc 2.36 或更新版本，数据格式为 v7；升级流程见 [RELEASE.md](RELEASE.md)。

## 模型价格

模型层就是一张价格表。一行 `ModelPrice` 给一个上游模型 ID 定价：`name` 是 CPA 在用量记录里上报的 `Model`（不区分大小写），`provider` 是可选限定，再加计费方式（`tokens` 或 `per_call`）、价格和计费倍率。同一 `(name, provider)` 只能出现一次。查价先匹配 `(provider, model)`，再回落到 provider 为空的行；CPA 的 `openai-compatible-<name>` provider 按 `<name>` 比较。

不再有公开模型名、路由和 Key 级模型列表。每个请求都由 CPA 按自己的 registry 路由：同一模型有多个 provider 时，CPA 跨 provider 选凭据并 failover；响应里的模型就是上游上报的模型。任何启用的 Key 都可以调用任何模型，并且始终可以读取模型列表 `/v1/models`、`/openai/v1/models` 和 `/v1beta/models`：列表请求不计 RPM 和额度，额度用尽时也能列。`/v1beta/models/<model>:…` 下的 Gemini 生成路由照常准入。

以下情况视为未定价：找不到价格行；token 行的价格全为 0；按次行的 `per_call_usd` 为 0。未定价的调用按 $0 计，但 token 与调用次数照常入账，每次调用计入 `unpriced_calls`。`GET /models` 的 `unpriced` 列出近 90 天内有调用没命中任何价格行、且至今没有不限定 provider 的价格行的模型；`has_qualified_rows: true` 表示该模型已有价格行但都限定了 provider，其他 provider 的调用没有命中（请检查 provider 限定，或新增不限定 provider 的价格行）。价格全为 0 的行在价格表里直接标为未定价（`priced: false`）。`GET /keys/usage` 和 `GET /stats` 模型行上的 `priced` 以账本为准：所示调用全部扣过费才为真；没有调用的模型回落到价格行的口径。token 价格为每百万 token 的美元数，范围 0–10000；`per_call_usd` 范围 0–10000；`billing_multiplier` 范围 1–50（默认 1）。

## 配置

完整示例见 [`config.example.yaml`](config.example.yaml)。当前核心结构：

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

首次启动会用 YAML 初始化 Key 和价格行，并创建带相同 `dataset_id` 的 state 与 usage 文件。之后 Key 和价格以 state 为唯一真相源；YAML 只控制 `enabled`、`state_file` 和 `usage_timezone`。旧版本的设置（`target_model`、`targets`、`dispatch`、`free`、`group`、`classify_rules`、`keys[].models`、`keys[].allow_models_endpoint`、`keys[].limits_changed_at`）属于未知字段，会让注册失败；切换前用 `cpa-key-policy-check --config` 检查配置。首次启动时两个种子 Key 共用同一密钥会被拒绝。启动时严格拒绝不受支持的文件版本、缺失配对、dataset 不一致、时区不一致的用量文件，以及水位（`through`）晚于 `usage_timezone` 当天的 history 文件。

## 用量与限额

账本内部金额是整数纳美元（1 USD = 1e9），累加精确且确定；管理接口对外输出 USD 浮点数，配置里的价格和限额仍是 USD。账本按 Key × 模型 × 自然日 各保存一个桶，保留最近 90 天（`calls, unpriced_calls, input_tokens, cache_read_tokens, cache_write_tokens, output_tokens, charged_nano, cache_read_cost_nano, cache_write_cost_nano`），另有每个 Key 的三个额度周期；每日汇总读取时由模型桶现算。三个文件共用一个 `dataset_id`：

- `<state_file>`（缩进 JSON，`version` 7）：Key 和价格行。
- `cpa-key-policy-usage.json`（紧凑 JSON）：所有 Key 的额度周期、余额 Key 的 `balance`（`balance_nano`、`net_credited_nano` 和记住的请求号），以及晚于 `history_through` 的日子。有脏数据时每 15 秒整文件重写。
- `cpa-key-policy-history.json`（紧凑 JSON）：保留期内 `through` 及之前已收盘的日子。每天第一次跨日的落盘会重写它，保留期淘汰日子时也会重写；除此之外已收盘的日子不再改写。1000 个 Key 时约 100–300 ms，只阻塞 flusher 协程，不持账本锁。

快照在账本锁内完成，序列化在锁外；写入失败保留脏标记、下个周期重试，并在 `/status` 的 `health.last_flush_error` 可见。插件卸载时做最后一次完整落盘，先写 history。live 文件引用了 history 日期而 history 文件缺失、`dataset_id` 不一致、时区不同或计数为负时拒绝启动，不会静默修复账本。时钟回拨后日期落在已收盘日子的记录，记到第一个未收盘的日子，已收盘的历史永不改写。删除 Key 会立即清掉它的周期和 RPM 窗口并写 live 文件（写入失败会记日志，交给 flusher 重试）；历史保留到过期，统计里标记为已删除。

每个 Key 在 `cycles` 下有独立的日、7 天、30 天额度周期。日额度在 `usage_timezone`（默认 `Asia/Shanghai`）的每天 00:00 重置。7 天、30 天周期是从 Key 创建日起算的固定自然日周期：首期到期为创建日加 7/30 天的 00:00；自动重置从原到期日推进，停机、空闲、重启不改变节奏；手动重置只恢复选中周期，下次到期为操作当天加 1/7/30 天的 00:00，其他周期和历史保持不变。修改限额、停用/启用或更换密钥不重置额度。手动重置在账本锁内写 live 文件，让文件恰好是一次完整的切片（1000 个 Key 约 10–25 ms）；这是有意的取舍，重置耗时超过 50 ms 时再复核。

每个 Key 有一种额度方式 `quota_mode`。默认的 `cycle` 受上面的日、7 天、30 天额度限制；`balance` 是预付余额：没有周期额度，唯一的限制是管理员通过 `POST /keys/balance` 调整的余额（`amount_usd` 为正表示充值，为负表示扣减）。余额 Key 每次计费都在登记周期的同一把锁内从余额扣除，余额小于等于 0 时拒绝请求；计费发生在调用结束后，余额用完时仍在进行的调用可能让余额略低于 0。每次调整都在答复前写入 live 文件，并记一条 `adjust_balance` 审计事件（金额、调整前后余额、备注、请求号）；扣费与账本其他数据一样每 15 秒落盘，进程崩溃最多丢失最后 15 秒的扣费。带 `request_id` 的调整重试时返回第一次的结果（`duplicate: true`），不会重复入账；每个 Key 保留最近 64 个请求号，同一请求号换了金额返回 409 `request_id_conflict`。切换为 `balance` 会清空周期额度，给余额 Key 设置周期额度会被拒绝；切回 `cycle` 时余额保留但不扣减，再切回 `balance` 时继续使用。删除 Key 会一并删除余额，`delete_key` 审计事件会写明金额。余额 Key 的周期照常累计，用于用量页和统计。

管理响应的 `usage.cycles` 提供 `window`、`started_at`、`resets_at`、`reset_kind`、`used_usd`、`limit_usd` 和 `reset_after_manual_at`。`usage.status` 取值 `normal`、`warning`、`limited`、`disabled`；`blocked_reason` 指出用尽的周期，或为 `balance_exhausted`。`usage.quota_mode` 重复 Key 的额度方式；余额 Key 以及保留了余额的周期 Key 带 `balance_usd` 和 `net_credited_usd`（所有调整的带符号合计），余额 Key 净充值用掉 80% 后状态为 `warning`。`daily_usd` / `weekly_usd` / `monthly_usd` 是当前周期的扣费金额；历史图表使用 `/keys/history`；`next_accounting_boundary_at` 等于日周期的 `resets_at`。

计费只发生一次：CPA 在调用结束后把用量记录交给插件时。

- `Failed` 为真的记录一律不计费。这包括客户端中途取消、被 CPA 记为 499 的流：它们带的部分 token 也不扣费。
- Key 先按 ID、再按明文哈希匹配；不要求 Key 仍处于启用状态，因为请求开始时已经授权。
- 模型取上游 `Model`，为空时才用请求里的名称。桶键一律是小写的模型 ID，按模型汇总不会因上游上报的大小写不同而拆开。
- 负的 token 数按 0 处理。输入、输出、思考、缓存读、缓存写任一超过 2^31 时整条记录拒绝（`invalid_usage`）；`total_tokens` 和 `cached_tokens` 不参与计费，超限只置 0、不拒绝。做过上述任一修正的记录计一次 `clamped_tokens`。
- token 记录分类后没有任何计费 token（输入、缓存读、缓存写、输出都为 0）时：模型已定价则忽略（`ignored_empty_usage`）；未定价则记 1 次调用、`unpriced_calls` 为 1，让不报 token 的新模型也能出现在未定价列表里。因此没有按次价格行的视频模型在定价前会把每次结果轮询也算作一次调用。
- token 口径与 CPA 自身的记账一致、按 provider 区分：Claude / Anthropic 的缓存读写不包含在输入里；Gemini、AI Studio、Vertex、Antigravity 和 interactions 的思考 token 不包含在输出里；其余 provider 的缓存读是输入的子集、思考是输出的子集。缓存读只取 `CacheReadTokens`。缓存读价格为 0 时按输入价计；没有缓存写价格时，缓存写按输入价并入输入。输入、输出、缓存读写的扣费都乘倍率，token 数保持真实值。四个分项各自取整到纳美元，扣费是它们的和，所以单独展示的缓存金额不会超过总扣费。改价格或倍率只影响之后的调用。
- `per_call` 行按被放行的媒体生成请求计费一次：请求拦截器记下 CPA 会拦截的图片 / 视频生成路由（`/v1/images/generations`、`/v1/images/edits`、`/v1/videos`、`/v1/videos/generations`、`/v1/videos/edits`、`/v1/videos/extensions`、`/openai/v1/videos`）上每个放行的 POST 请求的 TraceID（上限 4096 条、1 小时），带同一 TraceID 的用量记录扣 `per_call_usd`。按次模型的其他记录都不计费，计入 `unmetered_per_call`：结果轮询、过期的 TraceID、没有 TraceID 的宿主、CPA 不拦截的媒体路由（如 `/openai/v1/images/*`），以及 Codex 经 `/v1/responses` 调用 `image_generation` 工具时附带的图片模型记录。会报 token 的模型（如 Codex 的 `gpt-image-*`）请按 token 定价。丢失一条追踪只会让一次调用免费，不会重复扣费。

`GET /status` 的 `health` 包含 `last_flush_at`、`last_flush_error`、`flush_failures`、`skipped_failed`、`ignored_unknown_key`、`ignored_no_model`、`ignored_empty_usage`、`clamped_tokens`、`invalid_usage`、`unmetered_per_call` 和 `unpriced_calls`。计数只在内存里，每次启动归零。

## 请求拒绝

| 情况 | 判定位置 | 客户端看到 |
| --- | --- | --- |
| 未知 Key、停用 Key、插件被禁用 | 鉴权 | 401（CPA 通用答复） |
| 图片 / 视频生成路由上 Key 只放在 query 参数里 | 鉴权 | 401 |
| RPM 超限 | 请求拦截器 | 429 `rate_limit_exceeded`，`Retry-After` 为当前一分钟窗口的剩余秒数 |
| 预付余额用完（余额 Key） | 请求拦截器 | 429 `insufficient_quota`，包括 Responses API；不带 `Retry-After`，余额不会自动恢复 |
| 日、7 天或 30 天额度用尽 | 请求拦截器 | 429 `insufficient_quota`，`Retry-After` 为所有已用尽周期中最晚的重置时间；Responses API 的 `type` 为 `usage_limit_reached` 并带 `resets_at`（Unix 秒），Codex 据此显示重置时间，`code` 仍是 `insufficient_quota` |
| 模型列表（`/v1/models`、`/openai/v1/models`、`/v1beta/models`） | 鉴权 | 启用的 Key 始终放行，不计 RPM 和额度 |

准入先查 RPM，再查额度，拒绝时点名重置最晚的那个周期。CPA 会把鉴权阶段的任何拒绝都变成 401，而请求拦截器可以在访问上游之前以任意状态码终止请求（CPA v7.2.103 起）。当 Key 放在请求头里、且路由是 CPA 会拦截并给出 HTTP 答复的接口时，额度和 RPM 交给拦截器判定，这些接口包括 chat/completions、completions、responses、messages、count_tokens、images、videos（生成与结果查询）、`/v1beta/models/*`、interactions 和 codex responses。其余请求仍在鉴权阶段判定，保持 401：Key 放在 query 参数里的请求，realtime、live、alpha/search 路由，以及 `/v1/responses` 的 WebSocket 握手（WebSocket 上被拒的一轮只会断开连接，所以握手时检查额度，RPM 按每一轮计数）。只在 query 里带 Key 的媒体生成请求会被直接拒绝：拦截器看不到 query，而按次计费依赖拦截器记下的 TraceID。

429 的错误体跟随调用协议：OpenAI 形状 `{"error":{"message","type","code"}}`，Claude 形状 `{"type":"error","error":{"type":"rate_limit_error","message"}}`，Gemini 形状 `{"error":{"code":429,"message","status":"RESOURCE_EXHAUSTED"}}`。消息里写明用尽的额度和按 `usage_timezone` 计算的重置时间。查询已生成的视频结果不受额度限制。

CPA 会跳过返回错误的请求拦截器，所以插件出故障时那一个请求会被放行，但仍会经 `usage.handle` 计费。请在 CPA 的 `access.api-keys` 里保留至少一个不对外分发的 key：插件加载失败或被停用时，CPA 才会拒绝所有请求，而不是不经鉴权直接放行。

## 管理 API 与 Web UI

管理面包含 Key、模型价格、统计和审计四个入口。嵌入 CPA 管理面板时只显示分区标签；单独打开时额外显示地址和退出按钮。管理 API 基础路径：

```text
/v0/management/plugins/cpa-key-policy
```

| 路由 | 用途 |
| --- | --- |
| `GET/POST/PATCH/DELETE /keys` | Key 生命周期 |
| `POST /keys/rotate` | 轮换 Key 密钥 |
| `POST /keys/reset-rpm` | 清空 Key 的 RPM 窗口 |
| `POST /keys/reset-usage` | 恢复指定日 / 7 天 / 30 天周期额度，保留历史 |
| `POST /keys/balance` | 给余额 Key 充值或扣减：`{id, amount_usd, note?, request_id?}` |
| `GET /keys/usage` | Key 当前周期内的分模型用量 |
| `GET /keys/history` | 带 `by_model` 的自然日历史（`days` 1..90，默认 30） |
| `GET /stats` | 全部 Key 的汇总、按天、按模型和 Top Key 用量（`days` 1..90） |
| `GET /stats/export` | 导出 `from`..`to` 内每个（日、Key、模型）桶的 CSV（最多 92 天、200000 行） |
| `GET/POST/DELETE /models` | 价格行（`name`、可选 `provider`、计费方式、价格、倍率）和 `unpriced` 列表 |
| `POST /models/import` | 为上游模型创建 $0 价格行；已存在的行直接跳过，不覆盖 |
| `POST /models/pricing-preview` | 获取选中模型的 Models.dev 价格 |
| `POST /models/import-prices` | 预览或应用已有价格行的价格 |
| `GET /audit` | 读取追加式管理审计，新的在前；`key_id`、`action` 按 Key 和动作筛选，`limit`（1–1000，默认 100）按筛选后的条数计 |
| `GET /status` | 运行状态与账本健康计数 |

`GET /keys` 每项含 `id, name, enabled, key_preview, rpm, daily_limit_usd, weekly_limit_usd, monthly_limit_usd, quota_mode, usage, created_at, updated_at`；`POST /keys` 与 `PATCH /keys` 接收 `quota_mode`（`cycle` 或 `balance`），切换为 `balance` 的 PATCH 会清空它没有设置的周期额度。`POST /keys/balance` 返回 `{id, amount_usd, balance_before_usd, balance_usd, net_credited_usd, duplicate}`；金额为 0 或超出 ±1,000,000、备注超过 200 字返回 400 `invalid_amount`，周期 Key 返回 409 `not_balance_mode`，Key 不存在返回 404。`POST /keys` 与 `PATCH /keys` 拒绝未知字段，但接收并忽略旧版本曾有的 `models` 和 `allow_models_endpoint`，现有写入方无需改动。创建、更新、轮换时若密钥已被其他 Key 使用，返回 409 `duplicate_key`。`DELETE /models` 删除不存在的价格行返回 404 `not_found`。管理写入的 state 文件落盘失败时返回 500 `persist_failed`，运行中的策略保持不变。

`GET /keys/usage` 列出 Key 当前周期里出现过的模型，带 `billing_mode`、`billing_multiplier`、`priced` 和 `daily` / `weekly` / `monthly` 窗口。`GET /stats` 按扣费降序最多返回 200 个 Key，超出时带 `keys_truncated` 和 `keys_total`，并列出 `unpriced_models`。CSV 的列为 `date,key_id,key_name,deleted,model,calls,unpriced_calls,input_tokens,cache_read_tokens,cache_write_tokens,output_tokens,charged_usd`，金额按精确小数输出，以 `=`、`+`、`-`、`@` 开头的单元格前置单引号。超出范围返回 413 `range_too_large`。

重置接口兼容原 `{id, window}` 请求。新页面额外提交 `expected: {started_at, reset_after_manual_at}`；账本会在同一个锁内核对用户看到的周期和拟定日期。跨日或其他管理员已经重置时返回 `409 quota_changed`，页面刷新预览并等待再次确认，不自动重试写操作。

价格导入不会创建价格行；它把 token 价格应用到模型 ID 匹配的所有行（不看 provider 限定），并保留倍率。

## 构建与验证

需要 Go 1.25、Node.js 20+ 和 Docker。依赖统一使用 npm 与已提交的 `web/package-lock.json`。

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

Linux 产物为 `dist/cpa-key-policy_linux_amd64.so`，同时生成 SHA-256 和构建信息。构建在固定的 Debian 12 Go 容器内进行，完成 ELF64/x86-64、插件入口、动态依赖和 ABI 加载检查后才输出产物。ARM64 开发机可通过 Docker 的 amd64 仿真执行。GitHub Release 本次只构建 Linux x64。

## 数据升级

插件只读取数据格式 7，其他版本在启动时一律拒绝。格式 6（0.11.x）的数据在实例停机期间离线转换为一组新的格式 7 文件；`scripts/publish.py` 在维护窗口里自动完成，手动执行为：

```bash
go run ./cmd/cpa-key-policy-check --state /path/to/cpa-key-policy-state.json --timezone Asia/Shanghai --out /path/to/new-empty-dir
```

源文件只读。工具把 state、live 和 history 写入输出目录，逐项与源数据比对（dataset id、每个 Key 的身份与限额、由模型投影出的价格行、保留期内每个桶的计数与金额、每个额度周期的起止、类型与已用额），再在私有副本上启动并重启插件的 Store，证明文件能加载且重启不改变数据，输出目录保持转换时的原样。无法转换或加载的源数据在写任何文件之前就被拒绝。不带 `--out` 时只在私有目录里演练；对已是格式 7 的数据只做加载与重载。

转换保留 Key、Key 哈希、限额、额度周期和保留期内的历史；删除每个 Key 的模型列表、模型列表开关和模型级日限额；把每个模型投影为以上游模型 ID（`target_model`）为键的价格行——多个公开名称指向同一上游模型时，保留与上游 ID 同名的那个，否则按名称字典序取第一个，其余丢弃；金额换算为纳美元，冗余的每日汇总与模型桶核对后丢弃。报告的 `removed_settings` 列出被丢弃或取舍的内容（被忽略的 Key 模型授权、被删除的模型级日限额、关闭过 `/v1/models` 的 Key、改名与丢弃的价格行、重算的每日汇总、共用同一密钥的 Key），`renamed_models` 列出公开名称与上游 ID 不同的模型，`conversion` 给出计数，`policy_sha256` 是 Key 与价格的规范化哈希，启动前后必须一致。

安装时把旧的 state、usage 和所有 `.before-v*.json` 恢复点移出 state 目录（随备份保存），再把三个新文件移入；审计日志保留不动。旧插件不能读取格式 7，回退要恢复切换前的备份。预付余额相关字段（state 里的 `quota_mode`、live 文件里的 `balance`）只在有 Key 使用余额制后才写入，没有 Key 用过余额制的数据集仍能被之前的版本读取；用过之后，回退到这些版本同样需要恢复备份。详见 [RELEASE.md](RELEASE.md)。

`go run ./cmd/cpa-key-policy-check --config /path/to/config.yaml` 检查本版本是否接受 CPA 配置里的 `cpa-key-policy` 段；不被接受的配置会让注册失败。

## 安全与运行说明

- 配置和 state 只保存 Key 哈希；生成的明文 Key 仅返回一次。
- state、usage 和 history 可能包含敏感运行信息，必须限制为所有者可读。
- 管理审计记录语义化变更，绝不记录密钥或哈希；审计追加失败会记录日志，但不会回滚已成功的状态变更。
- 凭证选择和 Token 解析保持 CPA 原有语义；插件不注册模型路由器，也不注册响应拦截器。

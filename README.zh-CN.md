# cpa-key-policy

`cpa-key-policy` 是 CLIProxyAPI 插件，在 CPA 管理面板里显示为「萝卜」。它签发下游 Key、按上游模型定价并按倍率计费、记录用量，并执行 RPM 与美元限额；另外按渠道（CPA 凭据）统计成功率、延迟和按标价计的消耗，按渠道改写请求体，可接管 Codex 联网搜索（alpha search）的选路，还能用糖果、指纹、ModelTrace 和鹈鹕测试检测凭据背后的模型是否被降智或替换。插件 ID、配置键和所有管理路径仍是 `cpa-key-policy`。额度耗尽和 RPM 超限返回 HTTP 429；任何启用的 Key 都能使用 CPA 提供的所有模型；没有价格的模型按 $0 计费并被标记，直到补上价格。本次发布面向 Linux x64，运行基线为 Debian 12 / glibc 2.36 或更新版本，数据格式为 v7；升级流程见 [RELEASE.md](RELEASE.md)。

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
search_enabled: false
cpa_config_path: config.yaml

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

插件自己的两个键：

- `search_enabled`（默认 false）：为 true 时插件向 CPA 注册模型路由器，接管 `POST /v1/alpha/search` 的选路；路由设置在「搜索」页编辑。关闭时不注册路由器，普通请求不多一次穿越。
- `cpa_config_path`（默认 `config.yaml`，相对 CPA 的工作目录）：只读，用于检测时按 CPA 给的 ID 列出配置里的 API key、适配规则「试一次」、渠道页显示分组名，以及判断渠道能否删除；key 只在进程内使用，不会写进任何文件、日志或接口响应。

首次启动会用 YAML 初始化 Key 和价格行，并创建带相同 `dataset_id` 的 state 与 usage 文件。之后 Key 和价格以 state 为唯一真相源；YAML 只控制 `enabled`、`state_file`、`usage_timezone` 和上面两个键。CPA 每次配置生效都会重发配置；配置块没有变化时插件什么都不做，不重读数据文件，所以插件运行期间直接改数据文件不会生效，需要重启。例外是在这个 CPA 进程里跑过、交出数据目录之后又收到注册的同一份插件（`store.version` 切回它，或停用期间跑过别的版本后再启用，CPA 都复用进程里还在的那份插件）：它从文件重新加载并回放交接日志，不用内存里的旧数据接着服务；文件加载不了就注册失败。CPA 丢弃这次加载、回滚到被替换的实例时，那个实例照常服务，每条记录只计一次，也能再发起检测。

注册时未知的顶层键只记一条警告（「设置」页可见），不会让插件注册失败；已经加载数据之后，无法应用的配置会被拒绝并保留上一次有效的配置，同样显示在「设置」页；改了 `usage_timezone` 时立即拒绝，不重读数据文件。首次启动就无法加载数据时仍然注册失败。`cpa-key-policy-check --config` 保持严格：旧版本的设置（`target_model`、`targets`、`dispatch`、`free`、`group`、`classify_rules`、`keys[].models`、`keys[].allow_models_endpoint`、`keys[].limits_changed_at`）和其他未知键都会被它拒绝，切换前用它检查配置。

渠道、适配规则和搜索设置各自存一个文件，和 state 同目录：`cpa-key-policy-adapters.json`、`cpa-key-policy-search.json`、`cpa-key-policy-channels.json`、`cpa-key-policy-channel-stats.json`，检测历史存在 `cpa-key-policy-quality.json`，每日巡检的设置和结果存在 `cpa-key-policy-inspection.json`。它们不属于 v7 数据集，回退到旧版本时保留即可，旧版本不会读取。渠道统计每 5 分钟落盘一次，关闭和热替换时精确落盘，崩溃最多丢 5 分钟统计，不丢计费。`scripts/publish.py` 备份这些文件，连同还没被回放的交接日志。但 0.16.0 及更早版本严格解析配置块，回退前要从 `plugins.configs.cpa-key-policy` 删掉 `search_enabled` 和 `cpa_config_path`，否则注册失败，Key 全部 401。这些版本也不回放交接日志，回退到它们要重启 CPA，不能在运行中的 CPA 里只改 `store.version` 热切换（规则见 [RELEASE.md](RELEASE.md)）。首次启动时两个种子 Key 共用同一密钥会被拒绝。启动时严格拒绝不受支持的文件版本、缺失配对、dataset 不一致、时区不一致的用量文件，以及水位（`through`）晚于 `usage_timezone` 当天的 history 文件。

## 用量与限额

账本内部金额是整数纳美元（1 USD = 1e9），累加精确且确定；管理接口对外输出 USD 浮点数，配置里的价格和限额仍是 USD。账本按 Key × 模型 × 自然日 各保存一个桶，保留最近 90 天（`calls, unpriced_calls, input_tokens, cache_read_tokens, cache_write_tokens, output_tokens, charged_nano, cache_read_cost_nano, cache_write_cost_nano`），另有每个 Key 的三个额度周期；每日汇总读取时由模型桶现算。三个文件共用一个 `dataset_id`：

- `<state_file>`（缩进 JSON，`version` 7）：Key 和价格行。
- `cpa-key-policy-usage.json`（紧凑 JSON）：所有 Key 的额度周期、余额 Key 的 `balance`（`balance_nano`、`net_credited_nano` 和记住的请求号），以及晚于 `history_through` 的日子。有脏数据时每 15 秒整文件重写。
- `cpa-key-policy-history.json`（紧凑 JSON）：保留期内 `through` 及之前已收盘的日子。每天第一次跨日的落盘会重写它，保留期淘汰日子时也会重写；除此之外已收盘的日子不再改写。1000 个 Key 时约 100–300 ms，只阻塞 flusher 协程，不持账本锁。

快照在账本锁内完成，序列化在锁外；写入失败保留脏标记、下个周期重试，并在 `/status` 的 `health.last_flush_error` 可见。一直写不进去时概览页也会警告（`GET /overview` 的 `warnings` 里多一行 `usage ledger not saved …`），CPA 日志在第一次写失败和恢复时各记一行，其间的失败只写 stderr。插件卸载时做最后一次完整落盘，先写 history；已经不持有数据目录的实例（例如被热替换下来的）什么都不写。CPA 先关插件再停用量队列，停机时还在队列里的最后几条用量记录到不了插件，不会计费。live 文件引用了 history 日期而 history 文件缺失、`dataset_id` 不一致、时区不同或计数为负时拒绝启动，不会静默修复账本。时钟回拨后日期落在已收盘日子的记录，记到第一个未收盘的日子，已收盘的历史永不改写。删除 Key 会立即清掉它的周期和 RPM 窗口并写 live 文件（写入失败会记日志，交给 flusher 重试）；历史保留到过期，统计里标记为已删除。

每个 Key 在 `cycles` 下有独立的日、7 天、30 天额度周期。日额度在 `usage_timezone`（默认 `Asia/Shanghai`）的每天 00:00 重置。在 DST 从午夜开始的时区（例如 America/Santiago、Africa/Cairo），切换当天没有 00:00，那天以及落在那天的 7 天、30 天到期从当天第一个有效时刻 01:00 开始；其余日子仍是 00:00，Asia/Shanghai、UTC 这类没有这种日子的时区不受影响。旧版本可能把这个边界算到前一天 23:00，它们在这类时区写下的用量文件启动时会被拒绝，要先修正周期。7 天、30 天周期是从 Key 创建日起算的固定自然日周期：首期到期为创建日加 7/30 天的 00:00；自动重置从原到期日推进，停机、空闲、重启不改变节奏；手动重置只恢复选中周期，下次到期为操作当天加 1/7/30 天的 00:00，其他周期和历史保持不变。修改限额、停用/启用或更换密钥不重置额度。手动重置在账本锁内写 live 文件，让文件恰好是一次完整的切片（1000 个 Key 约 10–25 ms）；这是有意的取舍，重置耗时超过 50 ms 时再复核。

每个 Key 有一种额度方式 `quota_mode`。默认的 `cycle` 受上面的日、7 天、30 天额度限制；`balance` 是预付余额：没有周期额度，唯一的限制是管理员通过 `POST /keys/balance` 调整的余额（`amount_usd` 为正表示充值，为负表示扣减）。余额 Key 每次计费都在登记周期的同一把锁内从余额扣除，余额小于等于 0 时拒绝请求；计费发生在调用结束后，余额用完时仍在进行的调用可能让余额略低于 0。每次调整都在答复前写入 live 文件，并记一条 `adjust_balance` 审计事件（金额、调整前后余额、备注、请求号）；扣费与账本其他数据一样每 15 秒落盘，进程崩溃最多丢失最后 15 秒的扣费。带 `request_id` 的调整重试时返回第一次的结果（`duplicate: true`），不会重复入账；每个 Key 保留最近 64 个请求号，同一请求号换了金额返回 409 `request_id_conflict`。切换为 `balance` 会清空周期额度，给余额 Key 设置周期额度会被拒绝；切回 `cycle` 时余额保留但不扣减，再切回 `balance` 时继续使用。删除 Key 会一并删除余额，`delete_key` 审计事件会写明金额。余额 Key 的周期照常累计，用于用量页和统计。

管理响应的 `usage.cycles` 提供 `window`、`started_at`、`resets_at`、`reset_kind`、`used_usd`、`limit_usd` 和 `reset_after_manual_at`。`usage.status` 取值 `normal`、`warning`、`limited`、`disabled`；`blocked_reason` 指出用尽的周期，或为 `balance_exhausted`。`usage.quota_mode` 重复 Key 的额度方式；余额 Key 以及保留了余额的周期 Key 带 `balance_usd` 和 `net_credited_usd`（所有调整的带符号合计），余额 Key 净充值用掉 80% 后状态为 `warning`。`daily_usd` / `weekly_usd` / `monthly_usd` 是当前周期的扣费金额；历史图表使用 `/keys/history`；`next_accounting_boundary_at` 等于日周期的 `resets_at`。

计费只发生一次：CPA 在调用结束后把用量记录交给插件时。

- `Failed` 为真的记录一律不计费。这包括客户端中途取消、被 CPA 记为 499 的流：它们带的部分 token 也不扣费。
- 记录里计费不读的字段（`BaseURL`、`AuthID`、`AuthIndex`、`RequestedAt`、`Latency`、`TTFT`、`Failure`）换了类型时照常计费，只丢那个字段；计费读的字段（含 `Detail`）换了类型时这条不计费。两种情况每分钟最多记一条日志。
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
| 图片 / 视频生成路由上，或 WebSocket 握手（`GET /v1/responses`、`GET /backend-api/codex/responses`）时，Key 只放在 query 参数里 | 鉴权 | 401 |
| RPM 超限 | 请求拦截器 | 429 `rate_limit_exceeded`，`Retry-After` 为当前一分钟窗口的剩余秒数 |
| 预付余额用完（余额 Key） | 请求拦截器 | 429 `insufficient_quota`，包括 Responses API；不带 `Retry-After`，余额不会自动恢复 |
| 日、7 天或 30 天额度用尽 | 请求拦截器 | 429 `insufficient_quota`，`Retry-After` 为所有已用尽周期中最晚的重置时间；Responses API 的 `type` 为 `usage_limit_reached` 并带 `resets_at`（Unix 秒），Codex 据此显示重置时间，`code` 仍是 `insufficient_quota` |
| 模型列表（`/v1/models`、`/openai/v1/models`、`/v1beta/models`） | 鉴权 | 启用的 Key 始终放行，不计 RPM 和额度 |

准入先查 RPM，再查额度，拒绝时点名重置最晚的那个周期。CPA 会把鉴权阶段的任何拒绝都变成 401，而请求拦截器可以在访问上游之前以任意状态码终止请求（CPA v7.2.103 起）。当 Key 放在请求头里、且路由是 CPA 会拦截并给出 HTTP 答复的接口时，额度和 RPM 交给拦截器判定，这些接口包括 chat/completions、completions、responses、messages、count_tokens、images、videos（生成与结果查询）、语音合成（`/v1/audio/speech`、`/v1/tts`，CPA v8.0.18 起才有）、`/v1beta/models/*`、interactions 和 codex responses。其余请求仍在鉴权阶段判定，保持 401：Key 放在 query 参数里的请求，realtime、live、alpha/search 路由，以及 `/v1/responses`、`/backend-api/codex/responses` 的 WebSocket 握手（WebSocket 上被拒的一轮只会断开连接，所以 Key 在请求头时握手检查额度，RPM 由拦截器按每一轮计数）。只在 query 里带 Key 的媒体生成请求会被直接拒绝：拦截器看不到 query，而按次计费依赖拦截器记下的 TraceID。只在 query 里带 Key 的 WebSocket 握手同样直接拒绝：每一轮由拦截器准入，它拿到的是握手的请求头、没有 query，这种 Key 只会在握手时查一次。浏览器里的 WebSocket 设不了请求头，会被拒；Codex 把 Key 放在请求头里。

429 的错误体跟随调用协议：OpenAI 形状 `{"error":{"message","type","code"}}`，Claude 形状 `{"type":"error","error":{"type":"rate_limit_error","message"}}`，Gemini 形状 `{"error":{"code":429,"message","status":"RESOURCE_EXHAUSTED"}}`。消息里写明用尽的额度和按 `usage_timezone` 计算的重置时间。查询已生成的视频结果不受额度限制。

CPA 会跳过返回错误的请求拦截器，所以插件出故障时那一个请求会被放行，但仍会经 `usage.handle` 计费。请在 CPA 的 `access.api-keys` 里保留至少一个不对外分发的 key：插件加载失败或被停用时，CPA 才会拒绝所有请求，而不是不经鉴权直接放行。

## 管理 API 与 Web UI

面板里只有一个「萝卜」菜单，页面有概览、密钥、渠道、模型与价格、统计、搜索、检测、审计、设置。嵌入 CPA 管理面板时只显示分区标签；单独打开时额外显示地址和退出按钮。页面带 ETag，按弱比较校验（`W/"…"` 和 `*` 也算命中），面板每次切换菜单重新加载时只做一次 304 校验。接受 gzip 的客户端拿到压缩一次的副本，ETag 是 `"<sha>-gz"`；所有答复都带 `Vary: Accept-Encoding`，「设置」页显示的是原文页的 SHA，前置代理清掉 `Accept-Encoding` 时拿到原文。数据目录下的 `cpa-key-policy-ui/index.html` 存在时，插件用它替代内置页面，更新界面不需要更换插件文件。管理 API 基础路径：

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
| `GET /audit` | 读取追加式管理审计，新的在前；`key_id`、`action` 按 Key 和动作筛选，`limit`（1–1000，默认 100）按筛选后的条数计。凑满 `limit` 条后不再读更旧的备份。读不出的行跳过并只记一次日志（一个文件最新的若干行里不超过 8 条坏行时结果条数不变），超过 1 MiB 的行照常读出。崩溃或磁盘写满留下的半行，下次追加前先补上换行，下一条事件不会跟着它一起丢。筛选只认插件自己写出的紧凑 JSON，手工编辑过的行可能筛不出来 |
| `GET /status` | 运行状态与账本健康计数 |
| `GET /overview` | 今日请求、计费和按标价计的消耗，需要关注的 Key 和渠道，模块状态，最近变更，以及警告（配置警告；账本写不进磁盘时另有一行） |
| `GET /channels` | 按 auth_index 的渠道统计（24 小时、今日）、按模型的 24 小时汇总（`models_24h`）、元数据和规则数，以及 `groups`（按 auth_index 给出 config.yaml 里 API key 所在的分组名）和 `ids`（按 auth_index 给出配置 Key 的凭据 ID）；界面再与 CPA 的凭据列表合并，没有自己名字的配置 Key 用分组名显示，并用凭据 ID 向 CPA 的 `/v0/management/auth-files/models` 查每个渠道的模型，按类型和模型筛选 |
| `GET /channels/detail` | 一个渠道 7 天的小时序列、按模型的 7 天汇总（`models_7d`）和最近 20 条错误（`auth_index`） |
| `PATCH /channels` | 修改渠道显示名、供应商和备注。审计事件 `update_channel` 只列改动了的字段，什么都没改不记 |
| `DELETE /channels` | 删除 CPA 的 config.yaml 里已经没有的渠道（`auth_index`）：它的统计、元数据和绑定在它上面的适配规则，审计记 `delete_channel`（每条规则另记 `delete_adapter`）。渠道还在配置里时返回 409 `channel_in_cpa`，因为它下一次请求又会出现；找不到返回 404。只对照 config.yaml 里的 API key：CPA 仍在用的 OAuth 凭据不会被拦，它的统计会随下一次请求重新开始 |
| `GET/POST/DELETE /adapters` | 按渠道的请求改写规则：`edits_image_field`（图片编辑的 `images[].image_url` 改成 `image`）、`set_field`、`delete_field` |
| `POST /adapters/preview` | 用示例请求体预览规则的效果，不发送任何请求 |
| `POST /adapters/try` | 套用规则后经 CPA 真实发送一次，固定到这个渠道；会产生上游费用并计入渠道冷却，不计下游账单。规则路径要含 `/v1/images/edits`、`/v1/images/generations`、`/v1/responses`、`/backend-api/codex/responses`、`/v1/chat/completions`、`/v1/messages` 之一，否则 400 `unsupported_path`。config.yaml 里的 API key 按推算出的凭据 ID 固定，没走过流量也能试；其他渠道要先有一次真实请求（否则 409 `channel_not_seen`）。页面最多等 5 分钟；管理请求结束（超时、编辑了示例请求体或离开表单）时 CPA 取消上游调用 |
| `GET/PUT /search` | alpha search 的目标模型、保留模型、渠道顺序与模板、强制不健康、可选探测，以及各渠道实时状态 |
| `POST /search/import` | 从 alpha-search-model 插件的配置块导入搜索设置 |
| `GET /quality` | CPA 凭据及其检测进度、各项测试最近 20 次记录（糖果记录不带回答）和测试参数 |
| `POST /quality/run`、`POST /quality/cancel` | 对凭据发起或停止糖果、指纹、ModelTrace、鹈鹕测试；会消耗凭据额度 |
| `GET /quality/record`、`DELETE /quality/results` | 读取一条完整记录（糖果回答、指纹比对、ModelTrace 样本、鹈鹕 HTML；糖果记录的 `id` 是 `GET /quality` 里它的 `time` 原样字符串）；清空一项或全部记录 |
| `GET/PUT /quality/inspection` | 读取每日巡检的设置、进行中的一轮、下次时间和最近 `days` 天（默认 7，最多 90）按天汇总的结果；整体替换巡检设置 |
| `POST /quality/inspection`、`GET /quality/inspection/answers` | 立即开始一轮巡检（已有一轮在跑时 409 `inspection_running`）；读取某轮某个渠道答错的回答（`round`、`auth_index`、`model`、`effort`） |
| `GET /settings` | 模块状态、各钩子耗时（次数和最大值从启动起累计，p50/p99 取最近 512 次）、配置警告、数据文件、界面来源、数据目录归属（`ownership.handoff`：平时 `idle`，热替换交出后 `quiesced`，回滚后没取回目录时 `read_only`）与健康计数 |

`GET /keys` 每项含 `id, name, enabled, key_preview, rpm, daily_limit_usd, weekly_limit_usd, monthly_limit_usd, quota_mode, usage, created_at, updated_at`。`key_preview` 对 24 个字符及以上的 Key 保留前 7 后 5 个字符，更短的保留前 3 后 2，8 个字符及以下只显示 `...`；它在新建、轮换和改 Key 时写入，之前写下的预览保持原样。`POST /keys` 与 `PATCH /keys` 接收 `quota_mode`（`cycle` 或 `balance`），切换为 `balance` 的 PATCH 会清空它没有设置的周期额度。`POST /keys/balance` 返回 `{id, amount_usd, balance_before_usd, balance_usd, net_credited_usd, duplicate}`；金额为 0 或超出 ±1,000,000、备注超过 200 字返回 400 `invalid_amount`，周期 Key 返回 409 `not_balance_mode`，Key 不存在返回 404。`POST /keys` 与 `PATCH /keys` 拒绝未知字段，但接收并忽略旧版本曾有的 `models` 和 `allow_models_endpoint`，现有写入方无需改动。创建、更新、轮换时若密钥已被其他 Key 使用，返回 409 `duplicate_key`。`DELETE /models` 删除不存在的价格行返回 404 `not_found`。管理写入的 state 文件落盘失败时返回 500 `persist_failed`，运行中的策略保持不变。

`GET /keys/usage` 列出 Key 当前周期里出现过的模型，带 `billing_mode`、`billing_multiplier`、`priced` 和 `daily` / `weekly` / `monthly` 窗口。`GET /stats` 按扣费降序最多返回 200 个 Key，超出时带 `keys_truncated` 和 `keys_total`，并列出 `unpriced_models`。CSV 的列为 `date,key_id,key_name,deleted,model,calls,unpriced_calls,input_tokens,cache_read_tokens,cache_write_tokens,output_tokens,charged_usd`，金额按精确小数输出，以 `=`、`+`、`-`、`@` 开头的单元格前置单引号。超出范围返回 413 `range_too_large`。

重置接口兼容原 `{id, window}` 请求。新页面额外提交 `expected: {started_at, reset_after_manual_at}`；账本会在同一个锁内核对用户看到的周期和拟定日期。跨日或其他管理员已经重置时返回 `409 quota_changed`，页面刷新预览并等待再次确认，不自动重试写操作。

价格导入不会创建价格行；它把 token 价格应用到模型 ID 匹配的所有行（不看 provider 限定），并保留倍率。

渠道就是 CPA 的一条凭据，用 CPA 的 `auth_index` 标识：配置里的 API key 凭据由类型、base-url 和 key 算出，改 proxy、prefix 或请求头都不变；OAuth 凭据用凭据文件的 `auth_index`。渠道统计只来自 CPA 的用量记录：每次上游尝试一条，失败后换渠道重试的那次也算，所以不需要任何探测请求。Codex 调用 `image_generation` 工具时，同一次尝试会多一条成功记录（同一凭据、同一 TraceID 和请求时间、图片模型）：它的 token 和成本照加，但不算又一次尝试，也不加延迟样本。没有 TraceID 或请求时间的记录一律算一次尝试。客户端在上游答复之前取消（包括非流式请求的任何取消）时，CPA 记一条状态 499 的失败记录；它说明不了上游的情况，渠道统计忽略它。流开始之后的取消没有用量记录，alpha search 也没有。按次计费模型的标价成本只给被按次扣费的那条生成请求计一次 `per_call_usd`，它的其他记录不计。没有用 `PATCH /channels` 设过信息的渠道，闲置满 7 天后释放它的统计。在渠道页对 CPA 已经没有的渠道点「删除渠道」（`DELETE /channels`）可以立即删掉。0.21.0 及更早版本还给每个渠道存进价倍率和余额来源，这两个字段加载时忽略、下一次写入时去掉。CPA 不对配置里的 API key 凭据开放冷却状态，渠道页显示的是配置里的状态（如 `excluded-models: ["*"]`）和最近的失败率。

改写规则在 CPA 选定凭据之后、执行器转换请求之前运行（`request.intercept_after`），只作用于这一次尝试，写回的是客户端格式；只有规则命中的请求才会解码请求体。CPA 自己的 payload 规则既不能按凭据匹配，也不能搬移字段，这类上游差异只能在这里处理。

0.25.0 起渠道统计还按模型分开记：模型取客户端请求的名字（没有就用上游模型），去掉 `(high)` 这类推理强度后缀、统一小写，和 CPA 模型列表的名字一致；每个渠道每小时最多分 16 个模型，更多的只计入渠道总数。每个模型记尝试、失败和状态码，以及按计费同一口径折算的 Token（`policy.ShapeTokens`：提示 Token 含缓存读写，生成 Token 含推理），由此得出缓存命中率（缓存读占提示 Token 的比例）、每次成功请求的推理 Token、首字之后的可见输出速度（不含推理 Token：CPA 在第一个有内容的事件打首字，Codex 不流式输出推理，推理时间算在首字之前）和平均首字时间（后两项只算报告了首字时间的流式成功请求：非流式响应整段才到，首字时间约等于总耗时；用量记录的 `Stream` 字段区分两者）。渠道页选了模型后，列表改为比较提供这个模型的渠道：近 24 小时成功请求至少 20 次的渠道参与比较，推理 Token、缓存命中或输出速度低于同模型中位数 60% 时标「偏低」（中位数太小时不比：推理 Token 少于 50、缓存命中低于 10%）。这些都来自真实流量，不额外请求上游；插件自己的检测和巡检请求不带下游 Key，只计入渠道的尝试次数，不计入按模型统计；客户端各自设的推理强度、路由优先级都会影响数值，所以它只用来提示，不进概览。渠道详情里有按模型的 7 天汇总，点模型名跳到对应的比较。旧版本的统计文件没有按模型的部分，从升级起开始积累；回退时旧版本忽略这部分，下一次写入时丢掉。

检测从 cpa-codex-candy-eval 重构迁移而来，不再需要另装那个插件。凭据有两个来源：CPA 认证管理器里的认证文件，以及 `cpa_config_path` 指向的 config.yaml 里的 API key（只显示掩码和分组名）。CPA 给插件的凭据列表不含配置里的 API key，插件按 CPA 的规则从 config.yaml 推算它们的凭据 ID；配置读不出来时页面会提示，认证文件照常可测。每个请求经 CPA 固定发往所选凭据，CPA 负责协议转换。糖果、ModelTrace 和鹈鹕测试的每次请求都以新会话的完整 Codex CLI 上下文发出，输入约 1.4 万 Token；指纹测试快速、标准、严格模式每个凭据分别发 60、200、400 次短请求。这些请求是真实的上游调用，会计入渠道统计的尝试次数，但不计任何下游 Key 的账；发起测试和清空记录都写审计（`run_quality`、`clear_quality`，目标是测试名）。同一凭据同时只跑一项测试；每个凭据的每项测试保留最近 20 次、且在 90 天内的记录。糖果和鹈鹕测试在所有凭据间共用 16 个并发名额（测试参数里的 `max_parallel`）；单个模型流超过 16 MB 会被中断并记为错误，不重试；调用 CPA 时出的故障只让这一个请求失败。历史在测试记下结果后最多 5 秒落盘，关闭、热替换和清空时立即写，崩溃最多丢这 5 秒。历史文件读不出来时暂停保存，页面会提示；清空全部记录（`DELETE /quality/results?test=all`）会把它改名为 `<文件>.corrupt-<UTC 时间>`，然后恢复保存。数据目录里有 candy-eval 的 `cpa-codex-candy-eval-state.json` 而还没有 `cpa-key-policy-quality.json` 时，首次启动只读导入它的历史；这个文件读不出来时只是不导入。热替换时正在运行的测试会被取消，插件最多等 15 秒：CPA 还在建立的请求打断不了（`execute_stream` 返回之后模型流才能关闭），单次请求的超时也管不到这段等待；之后还没结束的测试自己关流，不保存记录。关闭插件时 CPA 在调用插件之前就拒绝了它的所有回调（包括关流），被取消的测试要等上游再发数据才能结束；最后一次账本落盘和渠道统计在这之前写完。指纹归一化没有做 Unicode NFC 组合（不为此引入 golang.org/x/text），这是与 candy-eval 唯一的计算差异，模型回答通常本来就是 NFC。

## 第三方组件

每日巡检（「巡检」页）在固定时间用糖果题考一遍选中的渠道，不需要外部定时任务。渠道按模型分组：一组是一个模型加推理强度和一批渠道（按 auth_index 记），每个时间点每个渠道连续问「每次答题数」道题，各渠道并行，和手动检测共用 16 个名额；默认每天 08:00、14:00、20:00（插件的 `usage_timezone`），每次 3 题，新装时关闭。一天内的答案按渠道和模型合计：答对至少三分之二为正常，至少三分之一为存疑，更少为降智，一题都没答出来为失败；CPA 里已没有、已停用或冷却中的渠道记为跳过。最近 24 小时降智或存疑的渠道出现在概览的「需要处理的渠道」里。插件每分钟检查一次时间点，只在持有数据目录的实例上跑；某个时间点错过了（停机、热替换取消了正在跑的一轮），之后只补跑当天最近的一个时间点，不补更早的。一轮结束后整体写进 `cpa-key-policy-inspection.json`，结果保留 90 天，答错的回答只保留 14 天；巡检记录不进入手动检测的 20 条历史。文件读不出来时暂停定时，保存一次巡检设置会把它改名为 `<文件>.corrupt-<UTC 时间>` 并恢复。修改设置写审计 `update_inspection`，手动开始一轮写 `start_inspection`，目标都是 `inspection`；定时的轮次不写审计。0.25.0 起每一轮都出一组新的变体题：口味、形状、商品名、列顺序和每格数量（4–12）随机，答案由程序按「靠手感先挑形状」的解法算出（`candyAnswer`，测试里与穷举所有摸法核对过），并且至少比「盲摸」「只保一种搭配」这几种常见错误答案小 4；题末要求单独写一行「答案：」，判分取最后一个「答案」后的数字，没有就取全文最后一个数字。同一轮所有渠道拿到同样的题，结果可以横向比较；题目和答错的回答一起存在轮次里。每天每组还算「同组最好」：至少两个渠道作答、最好的也不算正常时，这组记为整组偏低（更可能是模型或推理强度做不了这题），概览只列一条整组提示，不再逐个渠道报降智。手动检测的糖果测试仍用原题。页面用 CPA 的模型列表（`/v0/management/auth-files/models`，即客户端请求用的模型名，排除的模型和提供商默认模型都已算进去）挑渠道：新增一组时提供该模型的渠道默认全选，没勾的记为忽略，之后只提示新出现的渠道。

检测功能移植自 [cpa-codex-candy-eval](https://github.com/haowang02/cpa-plugin-codex-candy-eval)（MIT，Copyright (c) 2026 Hao Wang），许可证见 `internal/quality/LICENSE`；ModelTrace 的归因方法与候选库来自 [ModelTrace](https://github.com/xqy2006/ModelTrace)（MIT），许可证见 `internal/quality/data/modeltrace/LICENSE`。

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
- 凭证选择和 Token 解析保持 CPA 原有语义；只有 `search_enabled: true` 时插件才注册模型路由器，并且只回答 alpha search 请求；插件不注册响应拦截器。
- 请求体只在插件调用期间借用宿主的内存读取，解码时跳过 base64 的请求体，因此几十 MB 的 Codex 请求也只多出宿主侧的复制开销。
- 热替换前 CPA 会发 `plugin.quiesce`：插件落盘一次，此后只把计费记录和渠道统计的尝试追加到交接日志 `cpa-key-policy-handoff.jsonl`（新实例回放，两者逐条交接），不再写其他数据文件，直到 CPA 回滚替换时重新发配置。回放的记录按它被计费的那天入账（那天已收盘就记到第一个未收盘的日子），只计入当时已经开始的额度周期。quiesce 最多等 15 秒让运行中的检测结束，这期间管理写操作都在等。回滚后取不回数据目录的实例保持只读：管理写操作返回 503 `upgrading`，文案说明原因，要等锁空出来、CPA 再次应用配置。

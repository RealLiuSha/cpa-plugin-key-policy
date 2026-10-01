# V6 重构路线图

日期：2026-10-01。状态：已作为 1.0 在本地实现（数据格式 6），尚未发布。实现与计划的差异见文末「实施结果」。

依据：线上 grok 实例插件页面的只读走查、本仓库源码、CLIProxyAPI 本地 checkout `97f244b8` 的源码阅读，以及用 `go test -overlay` 跑的临时探针（探针文件已删除）。带 `CPA:` 前缀的路径属于 CLIProxyAPI 仓库，其余路径属于本仓库。

## 目标

插件只做三件事：Key 的额度统计与拦截、按模型价格和倍率计费、管理界面。v6 围绕这三件事收敛：

1. 额度或 RPM 超限时返回 429，带 `Retry-After` 和符合调用协议的错误体，不二开 CPA。
2. 删除凭证分组、多上游路由和免费标记。模型只剩四样东西：调用名、一个上游、价格、倍率。
3. 没有价格的模型也能导入，按 $0 计费，导入后用"同步价格"补价。
4. 管理界面去掉重复入口和多余步骤。
5. 现有 state/usage 自动迁移。Key、哈希、额度、周期、历史、价格、倍率和模型引用都不丢。

## 已确认的决策

| 事项 | 结论 |
| --- | --- |
| 429 的实现方式 | 纯插件方案：用 `request_interceptor` 的 `Terminate`。接受拦截器失败时放行，边界见阶段 1 |
| 没有价格的模型 | 允许导入，按 $0 计费 |
| 凭证分组、多上游路由 | 直接删除，不保留开关 |
| 免费标记 `free` | 删除。计费从来不读它，价格全为 0 时本来就按 $0 计；它唯一的作用是让价格同步跳过该模型 |
| v5 的"对外沿用 401" | 作废。宿主确实会丢弃鉴权通道里的拒绝信息，但拦截器通道可以自定义状态码，不需要改 CPA |

## 不做的事

- 不改 CPA，也不要求升级 CPA 镜像。`Terminate` 从 CPA v7.2.103（commit `30efd7c4`）开始就有，不受 schema 版本限制，插件继续声明 schema 2。
- 不改 `GET /keys`、`POST /keys` 的字段。loboo-admin 读前者，waku-server 调后者。
- 不做按 Key 过滤 `/v1/models`，因为这个接口不经过插件拦截器。
- 不新增依赖。

## 阶段 0：现在就该补的兜底（与 v6 无关）

如果插件被宿主熔断、被禁用或加载失败，同时 CPA 配置里没有 `api-keys`：

- 刷新后鉴权 provider 列表为空，因为熔断的插件会被跳过（`CPA:internal/pluginhost/adapters_auth.go:28-30`）。
- `CPA:sdk/access/manager.go:50-52` 在列表为空时返回 `nil, nil`，中间件直接放行，所有请求不经鉴权就到达上游。
- 与此同时，用量不再投递给插件（`CPA:internal/pluginhost/adapters_usage_translation.go:80-94`），这些请求也不计费。

处理办法：在每个实例的 CPA `api-keys` 里加一个随机生成、不对外分发的 key。这样鉴权列表永远不为空，插件失效时统一返回 401。这是线上配置改动，由你执行。

阶段 2 如果因为旧 YAML 字段导致插件注册失败，同样会掉进这个状态，所以旧配置兼容是硬性要求。

## 阶段 1：额度与 RPM 返回 429

单独发布，不改数据格式。

### 在哪里拦

| 情况 | 拦截位置 | 客户端看到 |
| --- | --- | --- |
| 未知 Key、停用 Key | 鉴权 | 401，不变 |
| Key 未授权该模型 | 鉴权 | 401，不变（原因见"失败放行"一节） |
| RPM 超限 | `request.intercept_before` | 429 `rate_limit_exceeded`，`Retry-After` 为当前窗口剩余秒数 |
| 日、7 天、30 天或模型日额度超限 | `request.intercept_before` | 429 `insufficient_quota`，`Retry-After` 为所有已超限周期里最晚的重置时间 |
| 拦截器覆盖不到，或客户端看不到 429 的请求 | 鉴权 | 401，沿用 v5 的判定 |

最后一行包括三类请求：

- `/v1/realtime*`、`/v1/live*`、`/v1/alpha/search`、`/backend-api/codex/alpha/search`：根本不调用拦截器。
- `/v1/responses`、`/backend-api/codex/responses` 的 WebSocket 握手：在 WebSocket 上，`Terminate` 只会表现为一次 1006 断连（`CPA:sdk/api/handlers/openai/openai_responses_websocket_forward.go:254-299`），客户端看不到 429。
- 用 `?key=` 传 Key 的请求：拦截器的入参里没有 query，拿不到 Key。

"是否被拦截器覆盖"按白名单判定，名单包括 chat/completions、completions、responses、messages、count_tokens、images、videos、`/v1beta/models/*`、interactions 和 codex responses。不在名单里的新路径默认在鉴权阶段拒绝。路由清单以 `CPA:internal/api/server_routes.go` 为准。`/v1/models` 的处理不变。

### 实现要点

- **能力注册**：注册 `request_interceptor`，只处理 `request.intercept_before`。`request.intercept_after` 原样返回，它在每次换凭据重试时都会被再调一次。
- **识别 Key**：从入参 `Headers` 里取，复用 `ExtractAPIKey`。CPA 传进来的是入站请求头的完整克隆，探针确认 Authorization 和 X-Api-Key 都在。
- **不做 I/O**：`intercept_before` 里只做内存查找，不读写文件、不发网络请求。宿主调用插件没有超时（`CPA:internal/pluginhost/client_guard.go:47-74`），插件一卡住，请求就一直挂着。
- **RPM 每次执行只计一次**：
  - 白名单内的路径在 `intercept_before` 里计数。探针确认它每个请求只调一次，凭据重试不会重复调用。WebSocket 的每一轮、视频轮询、count_tokens 各算一次，和 v5 在鉴权里按请求计数的口径一致。
  - 走鉴权拦截的路径在鉴权里计数。WebSocket 握手只查额度，不计 RPM，避免和第一轮重复计数。
- **视频结果查询不查额度**：`/v1/videos/:id` 及其 content 接口只查 RPM。生成视频时已经预扣过费用，额度用完后用户也应该能取回付过钱的结果。v5 在鉴权里对这类 GET 也做额度检查（`internal/policy/auth.go:54-60`），额度耗尽后用户取结果会被 401 挡住。
- **`Retry-After` 的计算**：
  - RPM：限流器是从首个请求起算的 1 分钟固定窗口（`internal/policy/limiter.go:34-52`），需要能返回窗口剩余时间。
  - 额度：额度摘要目前只记录第一个超限的周期（`internal/policy/quota.go:346-360`），需要改为取所有超限周期 `resets_at` 的最大值。
- **错误体格式**：按入参 `SourceFormat` 选择，消息里写明原因和重置时间（按 `usage_timezone`）。
  - `claude`：`{"type":"error","error":{"type":"rate_limit_error","message":…}}`。
  - 其余格式（openai、openai-response、gemini、openai-image、openai-video）：`{"error":{"message","type","code"}}`，和 CPA 自己的错误体一致（`CPA:sdk/api/handlers/handlers.go:117-140`）。
- **响应必须用 Go 结构体编码**：`ResponseBody` 是 `[]byte`，在 JSON 里必须是 base64。手写成普通字符串会让宿主解码失败，请求随之放行。
- **参考实现**：`CPA:examples/plugin/request-lifecycle/go/main.go:217-268`。

### "失败放行"指什么

拦截器这一层出现下面三种情况时，请求会继续发往上游：

1. **插件这次调用出错**：返回 `ok=false`、返回 Go error、响应 JSON 解析失败，或者插件内部 panic 被 `safePluginCall`（`internal/plugin/app.go:75`）转成了错误。宿主只打一条 warn，放行这一个请求（`CPA:internal/pluginhost/adapters_interceptors.go:30-34`）。
2. **热重载或替换插件文件的瞬间**：已经拿到旧记录的在途请求会被跳过（同文件 `:20`）。这是一个很窄的竞态，只影响单个请求。
3. **宿主侧 panic 导致插件被熔断**：拦截器停用，插件的鉴权也一起停用，后果见阶段 0。

影响范围：

- **照常计费**：被放行的请求仍然经过 `usage.handle` 计费，和拦截器结果无关。所以失败放行的后果是"超额透支、照常记账"，不是白用。下一个请求照样会被拦。
- **唯一的例外**：熔断后用量不再投递给插件，这部分由阶段 0 兜底。
- **为什么"未授权模型"留在鉴权**：这类请求一旦被放行，`usage.handle` 在这个 Key 上找不到该模型，直接记 0（`internal/policy/billing.go:34-37`），就成了真正的白用。

### 交付与验收

- **交付物**：拦截器能力、鉴权分流逻辑，以及 README 中错误契约的说明。
- **单元测试**：在现有的 `internal/plugin/app_test.go` 和 `internal/policy` 测试里更新，覆盖以下场景：
  - 白名单路径上 RPM 或额度超限时返回 Terminate 429，带 `Retry-After` 和对应协议的错误体；
  - 未知 Key、停用 Key、未授权模型在鉴权阶段被拒；
  - WebSocket 握手、realtime、`?key=` 请求在鉴权阶段被拒；
  - 每次执行 RPM 只计一次；
  - 视频轮询不查额度。
- **宿主集成**：本地启动一个和线上同版本的 CPA，只监听 127.0.0.1，验证完立即停止，加载本机编译的插件。
  - 用 curl 验证 chat/completions（流式和非流式）、responses、messages 返回 429，错误体格式正确。
  - 被拦截的请求不触达上游；放行的请求照常路由和计费，上游可以用本地 mock。
- **线上验收**：发布后在 grok 实例建一个 RPM=1、日额度 $0.01 的测试 Key 做受控请求，确认无误后删除这个 Key。

## 阶段 2：领域收敛与数据迁移

### 删除范围

| 内容 | 主要位置 |
| --- | --- |
| 凭证分组 | `classify.go`、`credential_group_admin.go`、`credential_group_handlers.go`；`/classify-rules*`、`/classify-preview` 路由；`scheduler` 能力和 `pickScheduler` 相关函数（`internal/plugin/app.go:262-477`）；鉴权时写入的 `group` 元数据；Web 的凭证分组页面和接口；目录和选择器里的 tier/group |
| 多上游路由 | `Targets`、`Dispatch`、轮询计数器、`pendingPicks`（`internal/policy/auth.go:63-65,91-93,101-189`；`internal/policy/store.go:35-36,45-51`）；价格导入里的多 target 一致性规则（`internal/policy/model_admin.go:181-219`）；Web 的多选目标和策略列 |
| 免费标记和正价格校验 | `internal/policy/model_definition.go:15,104-114`；价格同步对 `free_model` 的跳过（`internal/policy/model_admin.go:205-208`）；导入的 `missing_price`（`internal/policy/model_import.go:91-98`）；前端的正价格门槛（`web/src/components/ModelImportWizard.tsx:125-154,235`） |

完整清单（包括测试、fixture、i18n 和文档）在实施前用 `rg` 重新核对一遍。

需要保留的部分：

- `clearPendingPicksForKeyLocked` 里清理预扣的那一半（`internal/policy/auth.go:134-138`），`admin.go:102,202` 仍在调用。
- 按次计费和图片/视频预扣。
- `billing.go` 里按 provider 选择缓存计费规则的逻辑。
- 调用名和上游模型名不同时的别名能力，以及只为它服务的 `response.intercept_after` 模型名改写。别名要不要一起删，取决于迁移报告的结果。

### v6 模型

```json
{
  "name": "grok-4.6",
  "provider": "xai",
  "target_model": "grok-4.6",
  "billing_mode": "tokens",
  "input_price_per_million": 2,
  "output_price_per_million": 6,
  "cache_read_price_per_million": 0.5,
  "billing_multiplier": 1.2
}
```

- `targets` 数组拍平成 `provider` + `target_model`：只剩一个元素，就不该再用数组。
- 价格全为 0 是合法的，表示 $0。负数仍然拒绝。
- 不填 `cache_write_price_per_million` 表示按输入价计费（`internal/policy/cost.go:92-95`），界面上要写明。
- `per_call_usd` 只在 `billing_mode=per_call` 时有意义。

### 数据迁移（读 v3/v4/v5，写 v6）

| 旧数据 | v6 处理 |
| --- | --- |
| Key、哈希、额度、RPM、模型引用、`allow_models_endpoint` | 原样保留 |
| 周期、历史、按模型明细 | 原样保留；v3/v4 数据先走现有的 v5 周期初始化 |
| 只有一个 target | 拍平 |
| 有多个 target | 保留第一个。priority 策略下它就是主目标；round-robin 下取确定的第一个。其余 target 写入迁移报告和审计 |
| target 的 `group`、模型的 `dispatch`、`free` | 丢弃。`free` 模型的价格本来就全为 0，计费不变 |
| `classify_rules` | 丢弃，写入迁移报告 |
| state、usage 的版本号 | 都写成 6。usage 内容不变，但 `scripts/publish.py:605` 要求两个文件版本一致 |

做法沿用 v5 的迁移骨架（`internal/policy/migration.go`）：

- **先备份**：校验后保存 `<state>.before-v6.json`。如果回退后又再次升级，按内容摘要归档旧备份。
- **写入顺序**：先写 usage，再写 state，中途中断可以续跑。
- **旧格式解码**：用单独的 legacy 结构体严格解码。现有解码会拒绝未知字段（`internal/policy/persistence.go:87`），直接删掉字段会让所有旧文件都读不进来。

迁移报告由 `cmd/cpa-key-policy-check` 输出：

- **报告内容**：被折叠的 target、被丢弃的 group 和规则、被丢弃的 `free`，以及调用名和上游名不同的模型。
- **需要改的断言**：检查器目前要求迁移前后的模型和规则完全相同（`cmd/cpa-key-policy-check/main.go:92`），需要改为和 v6 投影结果对比。
- **不新增发布机制**：`publish.py` 已经会在停机后对真实文件跑这个检查。

### 旧配置兼容（硬要求）

YAML 解码是严格模式（`internal/policy/config.go:116-137`），而且每次 `plugin.register` 和 `plugin.reconfigure` 都会执行（`internal/plugin/app.go:95-101`）。

如果实例 `config.yaml` 的插件段里还留着 `classify_rules`，或者 models 下面还有多个 `targets`、`dispatch`、`free`、`group`，v6 会注册失败，插件以零能力运行。这时如果没有阶段 0 的兜底，CPA 会放行所有请求。

v6 对这些旧 seed 字段的处理：接受并忽略，在启动日志里告警。首次启动、还没有 state 文件时，按上面的迁移表投影。

### 管理接口变化

| 路由 | 变化 |
| --- | --- |
| `GET/POST /models` | 改为 v6 模型形状。只有内置界面在用；`bulk_update_key_models.py` 只读 `name` 字段 |
| `POST /models/import` | 改为 v6 模型形状。不再要求填价格，不再返回 `missing_price` 和 `price_conflict` |
| `POST /models/import-prices` | 去掉 `free_model`、`target_price_conflict` 两种跳过原因 |
| `POST /catalog` | 保留，导入要用；去掉 group 维度 |
| `/classify-rules*`、`/classify-preview` | 删除 |
| `/keys*`、`/audit`、`/status` | 不变；`GET /keys/usage` 去掉 `models[].free` |

### 交付与验收

- **交付物**：
  - 领域代码收敛、v6 迁移和迁移报告、旧 YAML 兼容；
  - 模型相关界面（阶段 3 的第一部分，必须和本阶段同版发布）；
  - 更新 README、`config.example.yaml` 和 RELEASE。
- **迁移验证**：
  - 新增 v5 的 state/usage fixture 和 v5→v6 迁移用例，放在一个测试文件里，覆盖多 target 折叠、group/规则/free 丢弃，以及中断后续跑。
  - 对两个实例的只读数据副本运行检查器，确认 Key 身份、哈希、额度、周期、历史、价格、倍率、引用都一致，重复加载结果相同。
- **配置验证**：带全部旧 seed 字段的 `config.yaml` 能够注册成功。
- **现有门禁**：
  - Go：`go test -race ./...`、`go vet ./...`、`make check-version check-model-domain`。
  - 前端：`npm test`、`npm run typecheck`、`VITE_HOSTED=1 npm run build`。

## 阶段 3：管理界面

### 模型（随阶段 2 一起发布）

- **列表**：
  - 列改为调用名、上游、计费方式、基础价、倍率、实扣价、引用。
  - 价格全为 0 时显示"$0 未定价"标记。这只是显示，不新增字段。
  - 加宽模型名列。被引用的模型不再显示一个禁用的删除按钮。
- **从 CPA 导入**：
  - 改成表格，一行一个模型，默认不勾选，带搜索和全选/全不选，不用填价格。
  - 只保留一个"导入 N 个"按钮。导入成功后直接打开"同步价格"，并预选刚导入的模型。
- **同步价格**：打开时就拉取 Models.dev 的报价，价格可编辑、模型可勾选，一步应用；未匹配到的模型单独列出。
- **编辑**：
  - 上游改为单选，计费方式只剩"按 Token"和"按次"两种。
  - 缓存写价格的占位文案写明"留空 = 按输入价"。
  - 倍率示例改用该模型自己的价格，目前写死成 $10（`web/src/pages/ModelForm.tsx:126`）。

### Key、详情、外壳、审计（单独发布，不改接口和数据）

- **Key 列表**："编辑"提到一级按钮（`web/src/pages/KeyList.tsx:130`）。"更多"里只留重置 RPM、更换 Key 和删除。
- **新建 Key**：
  - Key ID 留空时自动生成。
  - 额度为 0 时明确显示"不限"。
  - 删掉页面底部那句没有上下文的"仅存在内存中"提示。
- **用量详情**：30 天柱状图加上日期和金额坐标；顶部的周期卡片和底部的"今日花费 / 上限"内容重复，合并成一处。
- **外壳**：嵌入 CPA 管理页时，不再绘制插件自己的标题栏、导航和"退出"按钮，现在"退出"会被 CPA 右上角的浮动工具栏挡住。单独打开插件页面时保留这些元素。
- **审计**：动作名本地化，隐藏没有实际变化的字段（例如 `model: "grok-4.6" → "grok-4.6"`）。

### 验收

- 前端测试和 typecheck 通过。
- 用 ego-browser 对本地 ABI 管理接口、用合成数据走查。主路径：导入 → 同步价格 → Key 选模型 → 额度重置。
- 桌面端和 390px 移动端都没有横向溢出，深色主题下文字可读。

## 发布与回滚

| 发布 | 内容 | 数据 | 回滚方式 |
| --- | --- | --- | --- |
| A | 阶段 1 | 不变 | 换回上一版插件 |
| B | 阶段 2 + 模型界面 | v6 迁移 | 恢复 `.before-v6.json` 里配对的数据和旧插件，核对升级后的新增数据（同 v5 流程） |
| C | 阶段 3 的其余界面 | 不变 | 换回上一版插件 |

- **推进顺序**：每次发布先上 grok 实例（6 个 Key），确认没问题再上 GPT 实例（233 个 Key）。
- **发布 B 的前置条件**：先只读取回两个实例的数据副本，跑出迁移报告。报告里只要出现多 target、group 或规则，都由你逐项确认后再发布。
- **发布工具**：沿用 `scripts/publish.py`。插件版本号由脚本递增（当前工作区是尚未提交的 0.8.0），数据版本为 6。

## 待你决定

1. **兜底 `api-keys`**：在两个实例的 CPA 配置里加阶段 0 的兜底 key。这是线上配置改动，需要你执行或明确授权。
2. **`gpt-5.6-terra` 的定义**：waku-server 建 Key 时固定引用这个模型（`waku-server/internal/cpa/cpa.go:19`）。迁移报告出来后，要确认它不依赖 group 或多 target。
3. **是否连别名一起删**：如果报告显示所有模型的调用名都等于上游名，是否连同别名和 `response.intercept_after` 一起删掉？删掉后，每个非流式请求可以少一次带完整请求体和响应体的插件调用。
4. **按次计费失败不退款**：预扣的费用在请求失败时不退还（`internal/policy/billing.go:49-53`），所以失败的图片/视频请求也会扣费。v6 是否修正？
5. **补 `key_hash` 字段**：loboo-admin 读取 `GET /keys` 里的 `key_hash` 做指纹绑定，但插件从来不返回这个字段（`internal/plugin/app.go:630-644`），所以绑定状态一直是 `fingerprint_unavailable`。v6 是否补上？
6. **删除失效脚本**：`scripts/import_api_keys.py` 调用的是早已删除的旧接口，已经不能用（该文件被 gitignore），建议删除。

## 实施结果（2026-10-01）

阶段 1～3 已在本地实现，与计划的差异如下：

- **CPA 目录**：`POST /catalog` 已删除。去掉分组后，它只剩下按 provider 合并模型这一件事，现在由浏览器直接根据 CPA 管理接口完成。
- **导入接口**：只创建新模型，不再支持覆盖，也不再支持 dry-run。要修改已有模型的上游，在编辑页操作。
- **上游判定**：如果目录里的模型已被某个公开模型用作上游，或与已有模型同名，导入弹窗会把它标为"已存在"。
- **Key ID**：留空时由界面生成 `key-` 加 8 位十六进制。
- **待定事项的处理**：
  - 第 3 项（别名）：保留。检查器会在 `renamed_models` 中列出调用名与上游名不同的模型，供发布前判断。
  - 第 4 项（按次计费失败退款）：已实现。退款按预扣发生的日期和周期回退，并且不会把任何计数扣到 0 以下。
  - 第 1、2、5、6 项：仍待你决定，或需要在线上操作。

验证情况：

- Go 测试（含 race）、前端测试和 typecheck 均通过。
- 在本地真实 CPA（`97f244b8`，只监听 127.0.0.1）上加载本机编译的插件，用 mock 上游验证了：
  - 429 的 OpenAI / Claude / Gemini 三种错误体、`Retry-After`、流式请求；
  - query Key 返回 401；
  - 被拒请求不会触达上游；
  - v5 数据加旧格式 `config.yaml` 的迁移与注册。
- 用 ego-browser 走查了管理界面：导入、同步价格、编辑、新建 Key、用量详情、审计，以及 390px 移动端。


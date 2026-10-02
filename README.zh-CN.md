# cpa-key-policy

`cpa-key-policy` 是 CLIProxyAPI 插件，用于签发下游 Key、把公开模型名路由到 CPA 实际能力、按模型倍率计费，并执行 RPM 与美元限额。1.0 对额度耗尽和 RPM 超限返回 HTTP 429；每个公开模型只对应一个上游；从 CPA 导入的模型先按 $0 计费，之后再同步价格。本次发布面向 Linux x64，运行基线为 Debian 12 / glibc 2.36 或更新版本；升级流程见 [RELEASE.md](RELEASE.md)。

## 模型领域

- `ModelDefinition` 统一拥有调用方可见名称、一个上游（`provider` + `target_model`）、计费模式、价格和计费倍率。
- `KeyModelRef` 只表达 Key 可访问哪个公开模型，并可设置单模型每日美元限额。

计费支持按 Token 和按次。价格默认为 0：没有价格的模型按 $0 计费，从 CPA 导入的模型在同步价格前就是这种状态。价格不能为负数。

数据格式 6 删除了多上游路由（`targets`、`dispatch`）、凭证分组与归类规则，以及显式的 `free` 标记。旧的 state 文件和 CPA `config.yaml` 种子仍可加载，见[数据升级](#数据升级)。

## 配置

完整示例见 [`config.example.yaml`](config.example.yaml)。当前核心结构：

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

首次启动会用 YAML 初始化 Key 和模型，并创建带相同 `dataset_id` 的 state/usage 文件对。之后 Key 和模型以 state 为唯一真相源；YAML 只控制 `enabled`、`state_file` 和 `usage_timezone`。为旧版本写的种子字段（`targets`、`dispatch`、`free`、`classify_rules`）仍会被接受：种子只保留第一个上游，其余忽略并记录一条告警。启动时会严格拒绝不受支持的文件版本、缺失配对或 dataset 不一致。

## 用量与限额

历史消费按自然日保存到 `days` / `by_model`，保留最近 90 天；每日汇总等于同日模型明细之和。用量文件为紧凑 JSON，查看时可用 `jq .`。额度单独保存在每个 Key 的 `cycles` 中，每个周期的模型明细是该周期已用额度的唯一来源。

- 日额度每天 00:00 重置，模型日额度与 Key 日额度使用同一周期。
- 7 天、30 天额度各自按固定自然日周期重置，不再使用滚动窗口。时区沿用 `usage_timezone`（默认 `Asia/Shanghai`）。
- 手动重置立即恢复选中周期，下次日期为操作当天加 1/7/30 天的 00:00。它不会清空其他周期或历史消费。
- 自动重置从原到期日推进；停机、空闲、重启不改变周期节奏。查询、鉴权、记账和周期落盘使用相同规则。
- 修改限额、停用/启用或更换 Key 不重置额度。Token 用量按宿主用量事件到达账本的时刻归属周期。

管理响应的 `usage.cycles` 提供 `window`、`started_at`、`resets_at`、`reset_kind`、`used_usd`、`limit_usd` 和 `reset_after_manual_at`。`usage.status` 区分正常、预警、整体受限、部分模型受限和停用。原 `daily_usd` / `weekly_usd` / `monthly_usd` 字段现在是当前固定周期的扣费金额；历史图表继续使用 `/keys/history`。`next_accounting_boundary_at` 仅作为旧客户端的下一自然日边界兼容字段；新页面使用各周期的 `resets_at`。

Token 计费模型支持 `billing_multiplier`，默认 `1`，必须为不小于 `1` 的有限数。普通输入、输出、缓存读取和缓存写入的扣费金额统一乘以倍率，实际 Token 和调用次数不变。缓存读取价为 0、缓存写入价留空时都按输入价计费。按次计费不乘倍率。基础价格导入保留运营倍率；历史费用不追溯重算。倍率仅影响本插件账本，不会改写 CPA 原始 usage 或其他独立统计系统的费用。

图片、视频生成接口上的按次计费模型在放行请求时预扣，因为 CPA 可能不为这类请求上报用量。同一请求随后的用量上报不会重复扣费，生成失败时退回预扣。

## 请求拒绝

| 情况 | 判定位置 | 客户端看到 |
| --- | --- | --- |
| 未知 Key、停用 Key | 鉴权 | 401（CPA 通用答复） |
| Key 未授权请求的模型 | 鉴权 | 401 |
| RPM 超限 | 请求拦截器 | 429 `rate_limit_exceeded`，`Retry-After` 为当前一分钟窗口的剩余秒数 |
| 日、7 天、30 天或单模型日额度用尽 | 请求拦截器 | 429 `insufficient_quota`，`Retry-After` 为所有已用尽周期中最晚的重置时间；Responses API 的 `type` 为 `usage_limit_reached` 并带 `resets_at`（Unix 秒），Codex 据此显示重置时间，`code` 仍是 `insufficient_quota` |

CPA 会把鉴权阶段的任何拒绝都变成 401，而请求拦截器可以在访问上游之前以任意状态码终止请求（CPA v7.2.103 起）。当 Key 放在请求头里、且路由是 CPA 会拦截并给出 HTTP 答复的接口时，额度和 RPM 交给拦截器判定，这些接口包括 chat/completions、completions、responses、messages、count_tokens、images、videos（生成与结果查询）、`/v1beta/models/*`、interactions 和 codex responses。其余请求仍在鉴权阶段判定，保持原来的 401：Key 放在 query 参数里的请求，realtime、live、alpha/search 路由，以及 `/v1/responses` 的 WebSocket 握手（WebSocket 上被拒的一轮只会断开连接，所以握手时检查额度，RPM 按每一轮计数）。

429 的错误体跟随调用协议：OpenAI 形状 `{"error":{"message","type","code"}}`，Claude 形状 `{"type":"error","error":{"type":"rate_limit_error","message"}}`，Gemini 形状 `{"error":{"code":429,"message","status":"RESOURCE_EXHAUSTED"}}`。消息里写明用尽的额度和按 `usage_timezone` 计算的重置时间。查询已生成的视频结果不受额度限制。

CPA 会跳过返回错误的请求拦截器，所以插件出故障时那一个请求会被放行，但仍会经 `usage.handle` 计费。请在 CPA 的 `access.api-keys` 里保留至少一个不对外分发的 key：插件加载失败或被停用时，CPA 才会拒绝所有请求，而不是不经鉴权直接放行。

## 管理 API 与 Web UI

管理面包含 Key、模型和审计三个入口。嵌入 CPA 管理面板时只显示分区标签；单独打开时额外显示地址和退出按钮。管理 API 基础路径：

```text
/v0/management/plugins/cpa-key-policy
```

| 路由 | 用途 |
| --- | --- |
| `GET/POST/PATCH/DELETE /keys` | Key 生命周期和模型引用 |
| `POST /keys/rotate` | 轮换 Key 密钥 |
| `POST /keys/reset-usage` | 恢复指定日 / 7 天 / 30 天周期额度，保留历史 |
| `GET /keys/usage` | 单模型用量详情 |
| `GET /keys/history` | 带 `by_model` 的自然日历史 |
| `GET/POST/DELETE /models` | 公开模型定义（`name`、`provider`、`target_model`、价格、倍率） |
| `POST /models/import` | 为 CPA 能力创建 $0 模型；已存在的名称直接跳过，不覆盖 |
| `POST /models/pricing-preview` | 获取选中模型的 Models.dev 价格 |
| `POST /models/import-prices` | 预览或应用已有模型的价格 |
| `GET /audit` | 读取追加式管理审计 |

重置接口兼容原 `{id, window}` 请求。新页面额外提交 `expected: {started_at, reset_after_manual_at}`；账本会在同一个锁内核对用户看到的周期和拟定日期。跨日或其他管理员已经重置时返回 `409 quota_changed`，页面刷新预览并等待再次确认，不自动重试写操作。

价格导入不会创建模型；先按上游模型 ID、再按公开名称匹配价格，并保留倍率。

## 构建与验证

需要 Go 1.25、Node.js 20+ 和 Docker。依赖统一使用 npm 与已提交的 `web/package-lock.json`。

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

Linux 产物为 `dist/cpa-key-policy_linux_amd64.so`，同时生成 SHA-256 和构建信息。构建在固定的 Debian 12 Go 容器内进行，完成 ELF64/x86-64、插件入口、动态依赖和 ABI 加载检查后才输出产物。ARM64 开发机可通过 Docker 的 amd64 仿真执行。GitHub Release 本次只构建 Linux x64。

## 数据升级

插件可读取数据格式 3 到 6，在服务生效前把旧数据对转换为格式 6。Key、Key 哈希、限额、额度周期、历史、价格、倍率和模型引用全部保留。格式 6 每个模型只保留第一个上游（priority 策略下的主目标），并删除凭证分组、归类规则和 `free` 标记；免费模型的价格本就全为 0，计费仍是 $0。每项被删除的设置都会写入日志，并记录一次 `migrate_state` 审计事件。格式 5 之前的数据还会建立固定额度周期：旧滚动窗口已用额结转为新周期的期初消费，首期到期为迁移日期加 1/7/30 天的 00:00。

迁移前自动将原 state/usage 字节保存在 `<state_file>.before-v6.json`（JSON 内的 `state` / `usage` 为 base64）。备份内两份数据会独立校验；回退后再次升级会先归档旧恢复点，再备份当前数据。先写 usage，再写 state；中断后可继续完成，不重复结转。旧插件不能读取格式 6，回退必须恢复配套备份。详见 [RELEASE.md](RELEASE.md)。

可用以下只读命令在临时副本上演练旧数据迁移、用量承接和重复加载，源目录不会被配置为运行目录：

```bash
go run ./cmd/cpa-key-policy-check --state /path/to/cpa-key-policy-state.json --timezone Asia/Shanghai
```

报告的 `removed_settings` 列出所有被删除的设置，`renamed_models` 列出公开名称与上游模型 ID 不同的模型。

## 安全与运行说明

- 配置和 state 只保存 Key 哈希；生成的明文 Key 仅返回一次。
- state 和 usage 可能包含敏感运行信息，必须限制为所有者可读。
- 管理审计记录语义化变更；审计追加失败会记录日志，但不会回滚已成功的状态变更。
- 非流式响应会把上游模型 ID 改回调用方请求的公开模型；凭证选择和 Token 解析保持 CPA 原有语义。

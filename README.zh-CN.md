# cpa-key-policy

`cpa-key-policy` 是 CLIProxyAPI 插件，用于签发下游 Key、把公开模型名路由到 CPA 实际能力，并执行 RPM 与美元限额。0.6 增加模型导入与独立缓存写入计价。本次发布面向 Linux x64，运行基线为 Debian 12 / glibc 2.36 或更新版本；升级流程见 [RELEASE.md](RELEASE.md)。

## 模型领域

- `ModelDefinition` 统一拥有调用方可见名称、一个或多个上游目标、路由策略、计费模式和全局价格。
- `ModelTarget` 选择 CPA 的 `provider`、`target_model` 和可选凭证 `group`。
- `KeyModelRef` 只表达 Key 可访问哪个公开模型，并可设置单模型每日美元限额。
- 运行时才解析具体上游路由，不在 Key 上持久化派生路由。

多上游模型支持 `round-robin` 或 `priority`。计费支持按 Token、按次和显式免费。非免费模型必须提供正的有效价格；免费模型的所有价格字段必须为零。

例如管理员可以把 `asd` 暴露为稳定的公开模型，同时将 CPA 实际可用的 `gpt` 与 `grok` 配成两个上游目标。Key 只引用 `asd`，目标选择和价格仍统一归该模型定义所有。

## 配置

完整示例见 [`config.example.yaml`](config.example.yaml)。当前核心结构：

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

首次启动会用 YAML 初始化 Key、模型和凭证规则，并创建带相同 `dataset_id` 的 state/usage 文件对。之后这三个管理领域全部以 state 为唯一真相源；YAML 只控制 `enabled`、`state_file` 和 `usage_timezone`。启动时会严格拒绝不受支持的文件版本、缺失配对或 dataset 不一致。

## 用量与限额

历史消费按自然日保存到 `days` / `by_model`，保留最近 35 天；每日汇总等于同日模型明细之和。额度单独保存在每个 Key 的 `cycles` 中，每个周期的模型明细是该周期已用额度的唯一来源。

- 日额度每天 00:00 重置，模型日额度与 Key 日额度使用同一周期。
- 7 天、30 天额度各自按固定自然日周期重置，不再使用滚动窗口。时区沿用 `usage_timezone`（默认 `Asia/Shanghai`）。
- 手动重置立即恢复选中周期，下次日期为操作当天加 1/7/30 天的 00:00。它不会清空其他周期或历史消费。
- 自动重置从原到期日推进；停机、空闲、重启不改变周期节奏。查询、鉴权、记账和周期落盘使用相同规则。
- 修改限额、停用/启用或更换 Key 不重置额度。Token 用量按宿主用量事件到达账本的时刻归属周期。

管理响应的 `usage.cycles` 提供 `window`、`started_at`、`resets_at`、`reset_kind`、`used_usd`、`limit_usd` 和 `reset_after_manual_at`。`usage.status` 区分正常、预警、整体受限、部分模型受限和停用。原 `daily_usd` / `weekly_usd` / `monthly_usd` 字段现在是当前固定周期的扣费金额；历史图表继续使用 `/keys/history`。`next_accounting_boundary_at` 仅作为旧客户端的下一自然日边界兼容字段；新页面使用各周期的 `resets_at`。

Token 计费模型支持 `billing_multiplier`，默认 `1`，必须为不小于 `1` 的有限数。普通输入、输出、缓存读取和缓存写入的扣费金额统一乘以倍率，实际 Token 和调用次数不变。免费模型和按次计费不受倍率影响。基础价格导入保留运营倍率；历史费用不追溯重算。倍率仅影响本插件账本，不会改写 CPA 原始 usage 或其他独立统计系统的费用。

## 管理 API 与 Web UI

管理面包含 Key、模型、凭证分组和审计四个主入口。管理 API 基础路径：

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
| `GET/POST/DELETE /models` | 公开模型定义 |
| `POST /models/import-prices` | 预览或应用已有模型价格 |
| `POST /models/import` | 预览或应用模型批量创建、显式覆盖 |
| `POST /models/pricing-preview` | 获取选中模型的 Models.dev 价格 |
| `GET/POST/DELETE /classify-rules` | 凭证分组规则 |
| `POST /classify-rules/reorder` | 调整规则优先级 |
| `POST /classify-preview` | 预览凭证归类 |
| `POST /catalog` | 构建当前 CPA 能力目录 |
| `GET /audit` | 读取追加式管理审计 |

重置接口兼容原 `{id, window}` 请求。新页面额外提交 `expected: {started_at, reset_after_manual_at}`；账本会在同一个锁内核对用户看到的周期和拟定日期。跨日或其他管理员已经重置时返回 `409 quota_changed`，页面刷新预览并等待再次确认，不自动重试写操作。

价格导入不会创建模型、不会修改免费模型；多上游模型只有全部目标均命中且价格一致时才会应用。

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

新版读取 v3/v4/v5 数据，首次加载旧数据时在服务生效前迁移到 v5。旧滚动窗口已用额完整结转到对应新周期，首期到期为迁移日期加 1/7/30 天的 00:00。迁移不改变 Key 身份、限额、权限或模型基础价格。

迁移前自动将原 state/usage 字节保存在 `<state_file>.before-v5.json`（JSON 内的 `state` / `usage` 为 base64）。备份内两份数据会独立校验；回退后再次升级会先归档旧恢复点，再备份当前数据。先原子写入周期账本，再写模型配置；中断后可继续完成，不重复结转。旧插件不能读取 v5，回退必须恢复配套旧数据。详见 [RELEASE.md](RELEASE.md)。

可用以下只读命令在临时副本上演练旧数据迁移、用量承接和重复加载，源目录不会被配置为运行目录：

```bash
go run ./cmd/cpa-key-policy-check --state /path/to/cpa-key-policy-state.json --timezone Asia/Shanghai
```

现有 CPA 宿主的策略拒绝沿用通用 401；已移除宿主不支持的 `Rejection` 输出。内部仍保留额度、模型权限和 RPM 等拒绝原因，管理页面可以展示具体额度状态。本版不使用异常时可能放行的请求拦截器来替代认证阶段的额度限制。

## 安全与运行说明

- 配置和 state 只保存 Key 哈希；生成的明文 Key 仅返回一次。
- state 和 usage 可能包含敏感运行信息，必须限制为所有者可读。
- 管理审计记录语义化变更；审计追加失败会记录日志，但不会回滚已成功的状态变更。
- 非流式响应会把上游模型 ID 改回调用方请求的公开模型；provider 接入、调度选择和计费公式保持 CPA 原有语义。

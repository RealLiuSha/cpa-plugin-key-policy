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

用量按自然日保存到 `by_model`。每个 Key 的每日日桶必须严格等于同日所有模型桶之和；今日、近 7 天和近 30 天金额都从同一组日桶派生。

插件检查：

- Key RPM；
- Key 今日、近 7 天、近 30 天美元限额；
- 单模型每日美元限额。

API 只输出 `next_accounting_boundary_at` 表示下一个自然日边界。免费模型仍统计调用与 Token，但金额始终为零。

## 管理 API 与 Web UI

管理面包含 Key、模型、凭证分组和审计四个主入口。管理 API 基础路径：

```text
/v0/management/plugins/cpa-key-policy
```

| 路由 | 用途 |
| --- | --- |
| `GET/POST/PATCH/DELETE /keys` | Key 生命周期和模型引用 |
| `POST /keys/rotate` | 轮换 Key 密钥 |
| `POST /keys/reset-usage` | 重置今日、近 7 天或近 30 天日桶 |
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

新版可读取 v3、v4 数据，后续保存配置或用量时写入 v4。旧 v0.5.1 插件无法读取 v4；发布前必须按 [RELEASE.md](RELEASE.md) 准备 state/usage 配对备份与回滚步骤。

现有 CPA 宿主仍可读取原有鉴权响应字段，策略拒绝继续采用宿主已有的 401。结构化 403/429 与 `Retry-After` 需要宿主接收 `FrontendAuthResponse.Rejection`，本次插件独立发布不宣称宿主已具备此能力。升级不会修改已配置价格，也不会重算历史账本。

## 安全与运行说明

- 配置和 state 只保存 Key 哈希；生成的明文 Key 仅返回一次。
- state 和 usage 可能包含敏感运行信息，必须限制为所有者可读。
- 管理审计记录语义化变更；审计追加失败会记录日志，但不会回滚已成功的状态变更。
- 非流式响应会把上游模型 ID 改回调用方请求的公开模型；provider 接入、调度选择和计费公式保持 CPA 原有语义。

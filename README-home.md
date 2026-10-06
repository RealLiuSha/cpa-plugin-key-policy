# key-policy-home

`key-policy-home` 是一个独立的 HTTP 服务，在 CLIProxyAPIHome（Home）集群上继续提供 cpa-key-policy 的管理接口 `/v0/management/plugins/cpa-key-policy/*`。这样，签发和查询 Key 的调用方（如 reeka-ai-install）不用修改代码。

Home 模式下，CPA 的 `/v0/management/*` 都返回 404，插件因此无法提供这组接口。另外，Key 校验、额度判定和计费都由 Home 在每次派发时按 PostgreSQL 执行。所以本服务自己不保存任何状态：每个请求都直接读写 Home 管理 API 中的用户、API Key、周期额度、模型组和计费记录。可以在多个节点上同时运行多个实例。

源码：`cmd/key-policy-home`、`internal/homeadapter`。部署与运维在 loboo-deploy 仓库完成（`cpactl upgrade|rollback key-policy`、`cpactl key-policy migrate|token-add`）。

## 映射

| cpa-key-policy | Home |
| --- | --- |
| 一个 Key（`id`） | 一个用户 `KP_USER_PREFIX + id`，以及绑定到该用户的一个 api_key（值就是原 Key） |
| `daily/weekly/monthly_limit_usd` | 用户的 `limit_1d/7d/30d_credits`，窗口为 calendar（自然日 / 按 `week_reset_day` 的自然周 / 自然月），时区为 `KP_TIMEZONE`；`0` 表示不限，写入 Home 时为 `null` |
| `enabled: false` | `credits_unlimited=false` 且 `credits=0`；Home 对这类请求返回 402 |
| `models` | 按模型集合内容寻址的模型组 `kp-<hash>`，相同集合共用一个组；为空时表示所有有价格的模型 |
| 用量 | Home 的计费记录（`billing_charge`），按当前窗口统计 |
| `/models` | Home 中已启用、默认档位的价格规则；价格只由 Home 维护 |

新建 Key 时，`week_reset_day` 取当天的星期；迁移时沿用原 Key 的重置星期。

## 接口

| 路由 | 说明 |
| --- | --- |
| `GET /keys` | 全部 Key 及其当前用量；结果缓存 `KP_CACHE_TTL`，任何写入都会让缓存失效 |
| `POST /keys` | 按 `id` 创建或整体覆盖。必须带 `key`，服务端不生成 Key；未提供的额度视为不限 |
| `PATCH /keys` | 只修改提供的字段 |
| `DELETE /keys?id=` | 删除 api_key 和用户 |
| `GET /keys/usage?id=` | 当前日、周、月窗口内的分模型用量 |
| `POST /keys/reset-usage` | `{"id","window"}`：从当前时刻重新计数，不改变窗口的日历边界 |
| `GET /models` | 可售模型及其价格 |
| `GET /healthz` | 根路径，不在上面的前缀下；无需鉴权，网关也不会转发它 |

以上路径除 `/healthz` 外都以 `/v0/management/plugins/cpa-key-policy` 为前缀。鉴权：`Authorization: Bearer <token>` 或 `X-Management-Key: <token>`，令牌来自 `KP_TOKENS`。错误统一为 `{"error":{"code","message"}}`。

与插件的差异：
- `rpm > 0` 或单模型 `daily_limit_usd > 0` 返回 400 `unsupported_in_home`，因为 Home 不执行这两类限制。
- 同一个 Key 值已经属于别的用户时，返回 409 `key_conflict`。
- Home 不可用时返回 502 `home_unavailable`。
- `/keys/rotate`、`/keys/history`、`/status`、`/audit`、模型的写入接口和插件 Web UI 都不提供，返回 404。网页管理改用 Home 管理面板。
- 额度检查发生在请求前，用量在请求结束后异步入账，所以最后一笔请求可能超出额度（软限额）。

## 配置

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `KP_LISTEN` | `127.0.0.1:18340` | 监听地址，逗号分隔可以写多个 |
| `KP_HOME_URLS` | 必填 | Home 地址，逗号分隔；按顺序尝试，只在连接失败时换下一个 |
| `KP_HOME_KEY` | 必填 | Home 管理密钥 |
| `KP_TOKENS` | `serve` 必填 | 调用方令牌，逗号分隔 |
| `KP_TIMEZONE` | `Asia/Shanghai` | 新建用户的额度窗口时区 |
| `KP_USER_PREFIX` | `kp_` | Home 用户名前缀，用来区分本服务管理的用户 |
| `KP_CACHE_TTL` | `30s` | `GET /keys` 的缓存时长 |

## 命令

```bash
key-policy-home serve
key-policy-home migrate --state cpa-key-policy-state.json --usage cpa-key-policy-usage.json --plaintext ak_store.json [--apply]
key-policy-home version
```

`migrate` 把插件状态文件里的 Key 写入 Home，写入路径与 `POST /keys` 相同，所以可以重复执行。插件只保存 Key 的 hash，因此需要用 `--plaintext` 提供明文：可以是 JSON（读取 `ak`、`key`、`api_key`、`plain_key` 字段），也可以是每行一个 Key 的文本。程序按 hash 匹配明文，找不到明文的 Key 会列在报告的 `missing_plaintext` 里。`--usage` 只用来沿用每个 Key 的每周重置星期，历史用量不迁移。不加 `--apply` 时只打印计划。

## 构建与测试

```bash
go test ./internal/homeadapter/
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w -X main.version=$(git describe --always --abbrev=8)" ./cmd/key-policy-home
```

# 开发交付与升级说明（数据格式 v7）

当前数据格式为 v7。插件发布版本独立编号，由 `publish.py` 在构建通过后统一更新。

本次发布仅提供 Linux x64 插件，运行环境为 Debian 12 / glibc 2.36 或更新版本。其他平台不属于本次发布范围。

## 使用 publish.py 发布二开版本

`scripts/publish.py` 直接构建当前工作区并部署到指定 CPA Docker 实例，不依赖开源版本、插件商店或 GitHub Release。它不提交代码、不创建 Tag、不拉取新的 CPA 镜像。运行发布命令会自动停止并启动目标 CPA，期间服务短暂不可用，在途长请求可能中断；请选择可接受该影响的时间运行。

本地需要 Python 3.9+、OpenSSH、Git、ripgrep、Node.js 20+、npm、Go 1.25+ 和已运行的 Docker。远端需要 Linux x64、Docker 操作权限、Python 3.9+、PyYAML，以及 glibc 2.36+ 的 CPA 容器。支持 SSH 配置别名、密钥和 ProxyJump；使用已有 known_hosts，不自动接受未知主机。脚本不会自动安装系统依赖。首次 Docker 构建可能下载固定的 Go 构建镜像。

以 Grok 为例，先查看只读计划：

```bash
python3 scripts/publish.py \
  --ssh 'ssh -p 22 -i /Users/liusha/dev-config/keys/tencent.qq.liusha.hk root@45.94.40.80' \
  --remote-dir /opt/cpa/grok \
  --dry-run
```

去掉 `--dry-run` 即执行发布，无需再次逐项确认。也可以使用 `--ssh 'ssh my-cpa'`。脚本根据部署目录内 `config.yaml` 的 Docker 挂载识别实例；存在多个匹配时，通过 `--container cpa-grok` 指定。容器内要求 HTTP，外部反向代理仍可提供 HTTPS。管理密钥自动从该目录 `.env` 的 `MANAGEMENT_KEY` / `CPA_MANAGEMENT_KEY`、CPA 容器的 `MANAGEMENT_PASSWORD` / `CPA_MANAGEMENT_KEY` 或未哈希的管理配置读取，不写入发布记录。

每次新发布取本地和远端较高版本，将 minor 加一、patch 归零：`0.11.0 → 0.12.0 → 0.13.0`。版本递增和构建在私有源码副本中执行；构建通过后同步本地三个版本文件和内嵌页面，保留为未提交修改。构建失败不占用新版本；远端发布失败则保留已构建版本和产物，不自动覆盖用户改动。

脚本依次完成：

1. 检查工具、实际容器/镜像、插件运行版本、配置、挂载、配对数据（state 与 usage 版本须为 6 或 7；v7 的 live 文件引用了 history 时，history 文件必须存在且配对）和磁盘空间。
2. 在源码快照上生成前端，执行前端测试/typecheck、`npm audit --omit=dev --audit-level=moderate`（只审计打进页面的生产依赖）、Go race 测试/vet 和已有版本、领域门禁；使用固定 Linux 构建镜像生成 `.so` 和迁移检查程序。
3. 上传独立候选包，核对 SHA-256；此时 CPA 仍运行，配置未切换。
4. 在远端实例锁内再次核对配置和运行版本，用新版本的检查程序确认它接受当前插件配置段，再正常停止 CPA，不使用 Docker 强制杀死超时。将最终落盘配置、旧插件、state、usage、history、审计及旧版本留下的恢复点（`.before-v5.json`、`.before-v6.json`、`.before-v7.json` 及其归档）复制到实例专属备份目录，逐文件保存原路径、权限和校验和。
5. 对停机后的真实文件运行迁移检查：v6 数据在 state 同目录的临时目录里离线转换为 v7 并逐项比对，通过后核对待移出文件与备份一致，把 v6 state、usage 和所有 `.before-v*.json` 恢复点移出活动目录（它们已在备份里），换入三个 v7 文件（沿用原 state 的属主与权限），审计日志保留；v7 数据只做加载检查。以检查报告的价格行数作为启动后的预期模型数（v6 → v7 会合并指向同一上游模型的行），安装独立版本文件，只更新 `store.version` 及已有的 `release-tag`，保留 state 路径、时区和其余 YAML 内容，然后启动原容器。
6. 验证 `/healthz`、实际插件版本与加载路径、内嵌页面摘要、Key 数量与价格行数量、state/usage 版本等于检查报告的 `target_version`、启动前后 `policy_sha256` 一致。成功后输出版本、备份和发布记录位置。

远端工作进程与 SSH 会话独立，断线后继续完成当前操作。运行记录和包位于本地 `dist/publish/<发布编号>/`；远端工作记录位于 `<部署目录>/.cpa-key-policy-publish/<发布编号>/`，备份位于 `<部署目录>/backups/plugin-<发布编号>/`。备份的 `manifest.json` 记录每个备份文件与原路径的对应关系。

使用相同连接和目录添加 `--status <发布编号>` 可只读查询；`--resume <发布编号>` 复用已构建的版本和产物，补完中断上传、启动尚未执行的部署或继续等待远端结果，不再递增版本。终态失败不会被盲目重新执行。候选包和备份默认保留，由操作者在确认不再需要后清理。

新版启动前失败时，脚本恢复旧插件选择（已换入 v7 文件时连同停机时的账本一起从备份恢复）并启动旧 CPA；新版启动一旦被尝试，账本可能已产生新消费，此后的失败会标记 `needs_attention`，保留现场和备份，**不会自动回写旧账本**。使用下文回滚流程处理。停机命令超时或状态不确定同样不会继续切换。

自动验收覆盖插件真实加载及数据检查，不发送收费模型请求，也不修改业务 Key、倍率或额度。它不等于普通/流式模型请求的完整业务验收；如发布需要该层验证，应在恢复正常使用前执行受控请求。

## 本次变化

- 模型层收敛为价格表：价格行以上游模型 ID 为键，可选 provider 限定；不再有公开模型名、`target_model` 路由和响应改名。插件不再注册 `model_router` 和 `response_interceptor`，CPA 回到按自身 registry 的原生路由：同一模型有多个 provider 时由 CPA 跨 provider 选凭据并 failover。现网两个实例每个模型只有一个 provider，路由不变。
- 删除「允许访问 `/v1/models`」开关、Key 级模型授权和模型级日限额：任何启用的 Key 都能使用所有模型并列出模型，列表请求（`/v1/models`、`/openai/v1/models`、`/v1beta/models`）不计 RPM 和额度。`POST/PATCH /keys` 仍接收 `models` 与 `allow_models_endpoint` 但忽略它们；`GET /keys` 不再返回这两个字段以及 `limited_models`、`limits_changed_at` 和扁平 token 字段，`usage.status` 不再有 `partial`；`GET /keys/usage` 的 `models[]` 不再有 `in_config`，改带 `priced`；`POST /models/import-prices` 的响应不再有 `affected_keys`。重复密钥返回 409 `duplicate_key`。`DELETE /models` 删除不存在的价格行返回 404 `not_found`；管理写入的 state 落盘失败返回 500 `persist_failed`，运行态不变。
- 失败的尝试不计费：`Failed=true` 的用量记录一律跳过，包括客户端中途取消、被 CPA 记为 499 的流及其部分 token。
- 用量记录先净化：负数置 0；输入、输出、思考、缓存读、缓存写任一超过 2^31 整条拒绝（`invalid_usage`）；`total_tokens`、`cached_tokens` 超限只置 0，不拒绝；做过修正的记录计一次 `clamped_tokens`。桶键一律小写，按模型汇总不区分大小写；扣费是输入、缓存读、缓存写、输出四个分项各自取整后的和。
- 按次计费改为 TraceID 追踪集：拦截器记下放行的图片 / 视频生成 POST 请求（4096 条、1 小时），带同一 TraceID 的用量记录计费一次；不再有预扣和退款。结果轮询、CPA 不拦截的媒体路由，以及 Codex 经 `/v1/responses` 调用 `image_generation` 工具时附带的图片模型记录都不按次计费，计入 `unmetered_per_call`；`gpt-image-*` 这类会报 token 的模型请按 token 定价。只在 query 里带 Key 的媒体生成请求在鉴权阶段直接 401（拦截器看不到 query 里的 Key）。
- 未定价的模型按 $0 计，token 与调用数照常入账并标记 `unpriced_calls`；`GET /models` 新增 `unpriced` 列表（`has_qualified_rows` 标出只有 provider 限定行、调用没命中的模型），`GET /stats` 带 `unpriced_models`，模型价格页与统计页顶部提示。统计页模型行和 Key 行的徽标写「N 次按 $0 计费」：这是账本里已按 $0 入账的调用数，之后补价格不会回补，所以含这些调用的区间会一直显示，直到那几天超出 90 天保留期。token 记录分类后没有计费 token 时，已定价的忽略（`ignored_empty_usage`），未定价的记 1 次调用并计入 `unpriced_calls`，因此没有按次价格行的视频模型在定价前会把结果轮询也算作调用。
- 新增 `GET /stats`（7/30/90 天总览、按天、按模型、Top 200 Key）和 `GET /stats/export`（CSV，最多 92 天、200000 行，超出 413 `range_too_large`），以及统计页（默认今日，可切 7/30/90 天；今日只显示日期，不画单柱趋势图）；从统计页进入的 Key 用量页返回时回到统计页的同一区间。删除 Key 会立即清掉它的周期并落盘，历史保留到过期，统计里标记为已删除。`GET /status` 新增 `health` 计数，不再返回逐 Key 的 usage。
- 数据格式 v7，插件只读 v7：v6 数据在停机期间由 `cpa-key-policy-check --out` 离线转换为一组新文件（publish.py 自动执行），插件不再内置转换、不再写 `.before-v7.json`；配置里的旧设置（`target_model`、`targets`、`dispatch`、`free`、`group`、`classify_rules`、`keys[].models` 等）不再被接受，切换前用 `--config` 检查。账本金额改为整数纳美元；三文件布局（state、`cpa-key-policy-usage.json` 只含周期和未收盘日子、`cpa-key-policy-history.json` 保存已收盘的 90 天）；live 文件每 15 秒按需重写，history 在每天第一次跨日的落盘时整体重写（1000 个 Key 约 100–300 ms，只阻塞 flusher，不持账本锁）；手动重置沿用「锁内写 live 并在失败时恢复」的事务语义（1000 个 Key 约 10–25 ms，有意保留）。已收盘的日子不再改写：时钟回拨后落在已收盘日期的记录记到第一个未收盘日。history 水位晚于 `usage_timezone` 当天时拒绝启动，提示恢复文件或校正时钟。
- 新增预付余额额度方式（`quota_mode: balance`，默认仍是周期额度 `cycle`）：余额只能由管理员通过 `POST /keys/balance` 调整（可带 `request_id` 幂等），每次调整答复前写入 live 文件并记 `adjust_balance` 审计；余额 Key 没有周期额度，每次计费从余额扣除，余额小于等于 0 时请求返回 429 `insufficient_quota`（无 `Retry-After`）。管理页面支持新建时写初始余额、列表与用量页调整余额、查看最近余额变动、按额度方式筛选；删除有余额的 Key 会提示余额一并删除。
- Key 列表搜索支持输入法组合输入；Key 用量页历史支持 7/30/90 天切换并附按模型汇总。统计页的 Top Key 表可按 ID 或名称搜索（只在已列出的前 200 个 Key 中），切换区间和从 Key 用量页返回时保留搜索。
- `GET /audit` 新增 `action` 参数，服务端先按 Key 和动作筛选再取最近 `limit` 条；审计页增加动作类型筛选和“加载更多”（100 → 500 → 1000 条），筛选条件保存在地址里，余额用量页的“查看全部”直接打开该 Key 的余额调整记录。
- 模型价格页的「去定价」会让每个未定价模型都得到能为其调用计价的价格行：只有限定了 provider 的同名行、调用没有命中时，同步价格按调用里出现的模型 ID 新建一行不限定 provider 的价格（倍率默认沿用同名行，可修改），现有的行一并更新；没有任何价格行的模型新建时也可以填写倍率，不再固定为 ×1。同步对话框增加全选。新增价格时，模型 ID（不区分大小写）和 provider 与已有行相同会提示保存将更新那一行。

最低生命周期协议仍为 schema 2，插件 ABI 仍为 1。429 依赖 CPA 请求拦截器的 `Terminate`，需要 CPA v7.2.103 或更新版本；按次计费依赖 `request.intercept_before` 与 `usage.handle` 都带 `TraceID`（v8.0.8 已有）。现有宿主满足时不要求同步更新 CPA 镜像。

## 发布前验证

在要发布的提交上执行：

```bash
make web-build
(cd web && npm test && npm run typecheck && npm audit --omit=dev --audit-level=moderate)
go test ./...
go test -race ./...
go vet ./...
make check-version check-model-domain
git diff --exit-code -- internal/plugin/web/dist/index.html
bash scripts/build-linux-amd64.sh
```

构建脚本使用固定 Debian 12 / Go 1.25 镜像，验证 ELF 架构、`cliproxy_plugin_init` 导出、动态符号与 ABI 加载，产物包括：

```text
dist/cpa-key-policy_linux_amd64.so
dist/cpa-key-policy_linux_amd64.so.sha256
dist/cpa-key-policy_linux_amd64.so.build-info.txt
```

本地构建允许未提交工作，但构建信息会明确记录 `source_dirty=true`。正式发布必须包含本次源码、锁文件和内嵌页面，并在干净提交上通过上述检查。发布时选择新的版本和 Tag，不覆盖任何既有 Release。CI 会用相同构建脚本验证 Linux x64 插件，再生成 ZIP 与校验和。

构建环境无法访问 Go 模块源时，可通过 `CPAKP_GOMODCACHE="$(go env GOMODCACHE)" bash scripts/build-linux-amd64.sh` 只读复用已下载的模块缓存；编译器与 glibc 仍来自固定 Linux 镜像。

## 升级与数据备份

插件只读取 v7 文件，其他版本在启动时拒绝。v6 数据集由检查程序离线转换，publish.py 在停机后自动执行；手动操作时：

```bash
go run ./cmd/cpa-key-policy-check --config /path/to/config.yaml
go run ./cmd/cpa-key-policy-check --state /path/to/cpa-key-policy-state.json --timezone Asia/Shanghai --out /path/to/new-empty-dir
```

第一条确认新版本接受该实例的插件配置段（旧设置会让注册失败、宿主零能力运行）。第二条只读源文件，把转换结果写入输出目录：

1. 校验 state/usage 均为 v6、`dataset_id` 相同、文件结构严格符合 0.11.x 格式；转换后的数据按启动加载的同一套规则（周期、日期桶、价格行、Key）校验，任一项不通过就不写任何文件。
2. 转换：Key 去掉 `models`、`allow_models_endpoint`、`limits_changed_at`；模型投影为以 `target_model` 为键的价格行（多个公开名称指向同一上游模型时保留与上游 ID 同名的那个，否则按名称字典序取第一个，其余丢弃，provider 原样带入）；`by_model` 的 USD 金额四舍五入为纳美元，冗余的 `days` 汇总与模型桶核对后丢弃；三个周期逐个转换，起止、类型与已用额不变；保留期外的日子丢弃。
3. 依次写 history（截至转换时刻的昨天）、live、state，再用独立读取的 v6 源数据逐项比对，并在私有副本上启动、重启插件 Store 证明可加载且幂等，输出目录保持原样。
4. 报告包含 `target_version: 7`、`keys`、`models`（转换后的价格行数）、`preserved_history_days`、`expired_history_days`、`policy_sha256`、`removed_settings`（被忽略的 Key 模型授权、被删除的模型级日限额、关闭过 `/v1/models` 的 Key、改名与重复的价格行、重算的每日汇总、共用同一密钥的 Key）、`renamed_models`、`conversion` 计数和 `output`。不带 `--out` 时只在私有目录演练；`--at 2026-09-12T14:35:00+08:00` 可固定转换时刻（决定 90 天保留期边界和 history 水位），正式转换必须在停机窗口里用服务器时钟执行。

逐实例执行以下步骤，前一个实例未通过时不要推进下一个：

1. 记录当前 CPA 镜像、插件路径、插件校验和，以及配置实际引用的 state 路径。确认 Keeper 健康；宿主用量分发依赖其正常消费。
2. 进入维护窗口，停止新请求并等待在途请求完成，再正常停止该实例，使插件完成用量落盘。
3. 在实例专属备份目录中保存旧插件、CPA 配置、state、同目录的 `cpa-key-policy-usage.json`、`cpa-key-policy-history.json`（v7 实例才有）、`.before-v*.json` 恢复点和审计文件。确认 state/usage 的 `dataset_id` 相同。数据文件作为一个整体保留，不用不同时间点的文件拼接。
4. v6 实例：运行上面的转换，把旧 state、usage 和 `.before-v*.json` 移出 state 目录，换入三个新文件。校验新插件的 SHA-256，更新该实例配置所引用的插件文件，保持原 state 路径和用量时区，再启动 CPA。
5. 验证插件状态显示发布目标版本；Key、价格行（数量以演练报告的 `models` 为准）、历史消费与升级前一致，各周期已用额度承接正确；编辑倍率后保存并回读；验证一个受控请求的用量入账以及 RPM/额度拒绝：可新建一个 RPM=1、日额度 $0.01 的测试 Key，第二个请求应返回 429 和 `Retry-After`，未知 Key 仍为 401，只在 query 里带 Key 的图片 / 视频生成请求为 401；验证后删除该测试 Key。
6. 验证完成后恢复流量，并保留本次备份和验收记录。

这里的生产写入和维护操作由发布操作者执行；构建命令本身不会安装插件、修改线上配置或重启服务。

## 回滚

0.11.x 及更早版本不能读取 v7。不要在保留 v7 文件的情况下只换回旧插件。有 Key 用过预付余额之后，state 和 live 文件会带上 `quota_mode`、`balance` 字段，不认识这些字段的版本（0.13.0 及更早）也无法读取，回退同样要恢复备份。

需要回滚时，先停止流量、等待在途请求并正常停止实例，另存当前新版插件及 state、usage、history、审计作为故障现场，再按备份目录 `backups/plugin-<发布编号>/manifest.json` 把每个文件恢复到原路径（旧插件、原配置、v6 state 与 usage、`.before-v*.json` 恢复点、审计），移走 v7 的 history 文件后启动。备份文件是停机时的原始字节，`manifest.json` 记录了原路径、权限和 SHA-256，恢复后逐个核对。

恢复备份会使升级后新增的账本记录和管理修改退出活动数据集。这段增量必须保留并核对，恢复流量前明确额度与计费处置；不要未经核算直接丢弃增量或仅修改 JSON 的版本号。不能接受恢复点之后的数据处置时，应保持维护状态并向前修复。

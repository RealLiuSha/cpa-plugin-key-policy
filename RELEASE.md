# 1.0 开发交付与升级说明

当前数据格式为 v6。插件发布版本独立编号，由 `publish.py` 在构建通过后统一更新。

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

每次新发布取本地和远端较高版本，将 minor 加一、patch 归零：`0.6.0 → 0.7.0 → 0.8.0`。版本递增和构建在私有源码副本中执行；构建通过后同步本地三个版本文件和内嵌页面，保留为未提交修改。构建失败不占用新版本；远端发布失败则保留已构建版本和产物，不自动覆盖用户改动。

脚本依次完成：

1. 检查工具、实际容器/镜像、插件运行版本、配置、挂载、配对数据和磁盘空间。
2. 在源码快照上生成前端，执行前端测试/typecheck、`npm audit --audit-level=moderate`、Go race 测试/vet 和已有版本、领域门禁；使用固定 Linux 构建镜像生成 `.so` 和迁移检查程序。
3. 上传独立候选包，核对 SHA-256；此时 CPA 仍运行，配置未切换。
4. 在远端实例锁内再次核对配置和运行版本，正常停止 CPA，不使用 Docker 强制杀死超时。将最终落盘配置、旧插件、state/usage、审计及已有迁移恢复点复制到实例专属备份目录，逐文件保存原路径、权限和校验和。
5. 对停机后的真实文件运行只读迁移演练；通过后安装独立版本文件，只更新 `store.version` 及已有的 `release-tag`，保留 state 路径、时区和其余 YAML 内容，然后启动原容器。
6. 验证 `/healthz`、实际插件版本与加载路径、内嵌页面摘要、Key/模型数量、数据版本与身份/策略一致性。成功后输出版本、备份和发布记录位置。

远端工作进程与 SSH 会话独立，断线后继续完成当前操作。运行记录和包位于本地 `dist/publish/<发布编号>/`；远端工作记录位于 `<部署目录>/.cpa-key-policy-publish/<发布编号>/`，备份位于 `<部署目录>/backups/plugin-<发布编号>/`。备份的 `manifest.json` 记录每个备份文件与原路径的对应关系。

使用相同连接和目录添加 `--status <发布编号>` 可只读查询；`--resume <发布编号>` 复用已构建的版本和产物，补完中断上传、启动尚未执行的部署或继续等待远端结果，不再递增版本。终态失败不会被盲目重新执行。候选包和备份默认保留，由操作者在确认不再需要后清理。

新版启动前失败时，脚本恢复旧插件选择并启动旧 CPA；新版启动一旦被尝试，账本可能已迁移并产生新消费，此后的失败会标记 `needs_attention`，保留现场和备份，**不会自动回写旧账本**。使用下文回滚流程处理。停机命令超时或状态不确定同样不会继续切换。

自动验收覆盖插件真实加载及数据检查，不发送收费模型请求，也不修改业务 Key、倍率或额度。它不等于普通/流式模型请求的完整业务验收；如发布需要该层验证，应在恢复正常使用前执行受控请求。

## 本次变化

- 额度或 RPM 超限返回 429，带 `Retry-After` 和按 OpenAI / Claude / Gemini 协议的错误体；未知 Key、停用 Key、未授权模型仍为 401。
- 每个公开模型只对应一个上游；删除凭证分组、多上游路由和 `free` 标记，旧数据与旧 `config.yaml` 自动兼容。
- 从 CPA 导入的模型默认不勾选、无需填价格，按 $0 计费，导入后可直接同步价格；价格同步一步应用。
- 管理界面去掉凭证分组；模型列表区分基础价、倍率和实扣价；审计按动作和字段本地化展示。
- 按次计费的图片 / 视频生成失败时退回预扣；已生成视频的结果查询不受额度限制。
- 前端依赖 axios 由 1.18.1 升级到 1.20.0（`npm audit` 高危公告修复，lockfile 外无变化）。

最低生命周期协议仍为 schema 2，插件 ABI 仍为 1。429 依赖 CPA 请求拦截器的 `Terminate`，需要 CPA v7.2.103 或更新版本；现有宿主满足时不要求同步更新 CPA 镜像。

## 发布前验证

在要发布的提交上执行：

```bash
make web-build
(cd web && npm test && npm run typecheck && npm audit --audit-level=moderate)
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

新版本支持读取 v3/v4/v5/v6 文件。首次启动旧数据集时：

1. 校验配对 `dataset_id`、旧模型/Key 配置与账本一致性。
2. 校验备份内 state/usage 的格式与数据集后，将本轮原始文件字节保存到 `<state_file>.before-v6.json`。如果回退后旧版本又产生新数据，会先将先前备份归档为 `.before-v6.json.<sha256>`，再保存当前恢复点；损坏备份或不匹配的部分迁移会被拒绝。以前升级留下的 `.before-v5.json` 保持不动。
3. 结转迁移时日、近 7 天、近 30 天的已用额，作为新固定周期的期初消费。首个重置日按迁移日期加 1/7/30 天确定，不能从旧 `updated_at` 推测重置时间。
4. 原子持久化 v6 usage，再原子持久化 v6 state，完成后才发布新的运行态。旧 state + v6 usage 是可恢复的中途状态，重启会沿用已有周期并补完 state；v6 state + 旧 usage 会被拒绝，需恢复正确配对。
5. 每个模型只保留第一个上游，删除凭证分组、归类规则和 `free` 标记。被删除的设置写入 CPA 日志，并记录一次 `migrate_state` 审计事件；发布前可用下面的只读演练在 `removed_settings` 中逐项核对。

提前运行只读迁移演练：

```bash
go run ./cmd/cpa-key-policy-check --state /path/to/cpa-key-policy-state.json --timezone Asia/Shanghai
```

该命令将源文件复制到私有临时目录，再验证配置、有效历史、期初额度与重载幂等，结束后清理副本。它不会连接 CPA 或写入源目录。可加 `--at 2026-09-12T14:35:00+08:00` 固定迁移时刻。

逐实例执行以下步骤，前一个实例未通过时不要推进下一个：

1. 记录当前 CPA 镜像、插件路径、插件校验和，以及配置实际引用的 state 路径。确认 Keeper 健康；宿主用量分发依赖其正常消费。
2. 进入维护窗口，停止新请求并等待在途请求完成，再正常停止该实例，使插件完成用量落盘。
3. 在实例专属备份目录中保存旧插件、CPA 配置、state、同目录的 `cpa-key-policy-usage.json` 和审计文件。确认 state/usage 的 `dataset_id` 相同。两个数据文件作为一个整体保留，不用不同时间点的文件拼接。
4. 校验新插件的 SHA-256，更新该实例配置所引用的插件文件，保持原 state 路径和用量时区，再启动 CPA。
5. 验证插件状态显示发布目标版本；Key、模型、历史消费与升级前一致，各周期已用额度承接正确；编辑倍率后保存并回读；验证一个受控请求的路由、用量入账以及权限/RPM/额度拒绝：可新建一个 RPM=1、日额度 $0.01 的测试 Key，第二个请求应返回 429 和 `Retry-After`，未授权模型和未知 Key 仍为 401；验证后删除该测试 Key。
6. 验证完成后恢复流量，并保留本次备份和验收记录。

这里的生产写入和维护操作由发布操作者执行；构建命令本身不会安装插件、修改线上配置或重启服务。

## 回滚

0.8.0 及更早版本不能读取 v6。不要在保留 v6 文件的情况下只换回旧插件。

需要回滚时，先停止流量、等待在途请求并正常停止实例，另存当前新版插件及 state/usage/审计作为故障现场，再恢复同一备份中的旧插件、原配置和配对旧版数据后启动。

恢复备份会使升级后新增的账本记录和管理修改退出活动数据集。这段增量必须保留并核对，恢复流量前明确额度与计费处置；不要未经核算直接丢弃增量或仅修改 JSON 的版本号。不能接受恢复点之后的数据处置时，应保持维护状态并向前修复。

自动备份可以在**单独的恢复目录**解包。先保留故障现场，再按上述维护步骤恢复，不能对正在使用的账本直接覆盖：

```bash
python3 - /path/to/state.json.before-v6.json /path/to/recovery-directory <<'PY'
import base64, json, pathlib, sys
source = pathlib.Path(sys.argv[1])
target = pathlib.Path(sys.argv[2])
target.mkdir(mode=0o700)  # 必须是新目录，避免覆盖已有数据
backup = json.loads(source.read_text())
for field, name in [('state', 'cpa-key-policy-state.json'), ('usage', 'cpa-key-policy-usage.json')]:
    raw = base64.b64decode(backup[field], validate=True)
    document = json.loads(raw)
    if document['dataset_id'] != backup['dataset_id']:
        raise ValueError('backup dataset mismatch')
    output = target / name
    with output.open('xb') as stream:
        stream.write(raw)
    output.chmod(0o600)
PY
```

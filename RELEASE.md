# V5 开发交付与升级说明

V5 尚未发布；版本标识仍保留当前 0.6.0，正式发布时再统一更新。当前数据格式为 v5。

本次发布仅提供 Linux x64 插件，运行环境为 Debian 12 / glibc 2.36 或更新版本。其他平台不属于本次发布范围。

## 本次变化

- Key 列表突出用户、剩余额度和独立重置日期，支持状态筛选及重置预览。
- 日、7 天、30 天额度分周期计数；手动重置保留其他周期与历史消费。
- 模型倍率仅应用于 Token 模型扣费金额，不改变实际 Token。
- 清理无效认证错误扩展，额度拒绝继续沿用宿主通用 401。

最低生命周期协议仍为 schema 2，插件 ABI 仍为 1。本轮不修改 CPA，也不执行线上迁移或发布。

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

新版本支持读取 v3/v4/v5 文件。首次启动旧数据集时：

1. 校验配对 `dataset_id`、旧模型/Key 配置与账本一致性。
2. 校验备份内 state/usage 的格式与数据集后，将本轮原始文件字节保存到 `<state_file>.before-v5.json`。如果回退后旧版本又产生新数据，会先将先前备份归档为 `.before-v5.json.<sha256>`，再保存当前恢复点；损坏备份或不匹配的部分迁移会被拒绝。
3. 结转迁移时日、近 7 天、近 30 天的已用额，作为新固定周期的期初消费。首个重置日按迁移日期加 1/7/30 天确定，不能从旧 `updated_at` 推测重置时间。
4. 原子持久化 v5 usage，再原子持久化 v5 state，完成后才发布新的运行态。v3/v4 state + v5 usage 是可恢复的中途状态，重启会沿用已有周期并补完 state；v5 state + 旧 usage 会被拒绝，需恢复正确配对。

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
5. 验证插件状态显示发布目标版本；Key、模型、历史消费与升级前一致，各周期已用额度承接正确；编辑倍率后保存并回读；验证一个受控请求的路由、用量入账以及权限/RPM/额度拒绝。旧宿主按旧 401 契约验证，不能用 403/429 断言判断它失败。
6. 验证完成后恢复流量，并保留本次备份和验收记录。

这里的生产写入和维护操作由发布操作者执行；构建命令本身不会安装插件、修改线上配置或重启服务。

## 回滚

旧 v0.5.1 / v0.6.0 不能读取 v5。不要在保留 v5 文件的情况下只换回旧插件。

需要回滚时，先停止流量、等待在途请求并正常停止实例，另存当前新版插件及 state/usage/审计作为故障现场，再恢复同一备份中的旧插件、原配置和配对旧版数据后启动。

恢复备份会使升级后新增的账本记录和管理修改退出活动数据集。这段增量必须保留并核对，恢复流量前明确额度与计费处置；不要未经核算直接丢弃增量或仅修改 JSON 的版本号。不能接受恢复点之后的数据处置时，应保持维护状态并向前修复。

自动备份可以在**单独的恢复目录**解包。先保留故障现场，再按上述维护步骤恢复，不能对正在使用的账本直接覆盖：

```bash
python3 - /path/to/state.json.before-v5.json /path/to/recovery-directory <<'PY'
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

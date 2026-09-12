# cpa-key-policy 0.6.0

本次发布仅提供 Linux x64 插件，运行环境为 Debian 12 / glibc 2.36 或更新版本。其他平台不属于本次发布范围。

## 本次变化

- 修复编辑模型时提交 `ref_count`、`ref_keys` 等只读字段导致保存失败的问题。
- 从 CPA 实际凭证能力中批量导入模型；已有模型默认跳过，覆盖前可预览受影响 Key。
- 从 Models.dev 获取价格，分别处理普通输入、输出、缓存读取和缓存写入。
- 缓存写入价格区分未配置、明确免费和正数；用量账本记录独立的缓存写入 Token 与成本。
- 修复构建依赖的审计问题，发布流程只生成并验证 Linux x64 产物。

## 宿主兼容范围

最低生命周期协议为 schema 2，支持更高的宿主协议版本。插件 ABI 仍为 1。

现有 CPA 宿主可以使用本次插件的模型管理、路由和计费功能。策略拒绝在这些宿主上继续返回其已有的 401；插件提供的 `Rejection` 扩展只有在宿主明确支持后，才会投影为 403/429、具体错误码及 `Retry-After`。本次独立发布不以修改 CPA 宿主为前置条件，也不宣称新版错误语义已经在旧宿主生效。

升级不会修正线上模型的既有价格，也不会重算历史费用。已有错误价格和历史账本应单独核对、修正。

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

本地构建允许未提交工作，但构建信息会明确记录 `source_dirty=true`。正式发布必须包含本次源码、锁文件和内嵌页面，并在干净提交上通过上述检查。使用新版本 Tag `v0.6.0`，不覆盖 `v0.5.1`。CI 会用相同构建脚本验证 Linux x64 插件，再生成 ZIP 与校验和。

构建环境无法访问 Go 模块源时，可通过 `CPAKP_GOMODCACHE="$(go env GOMODCACHE)" bash scripts/build-linux-amd64.sh` 只读复用已下载的模块缓存；编译器与 glibc 仍来自固定 Linux 镜像。

## 升级与数据备份

线上 v0.5.1 使用 v3 文件，新版可读取 v3、v4。第一次保存配置时 state 写入 v4，产生新用量并落盘时 usage 写入 v4；这两个文件可能短暂处于 v4/v3 混合版本，新版支持读取。

逐实例执行以下步骤，前一个实例未通过时不要推进下一个：

1. 记录当前 CPA 镜像、插件路径、插件校验和，以及配置实际引用的 state 路径。确认 Keeper 健康；宿主用量分发依赖其正常消费。
2. 进入维护窗口，停止新请求并等待在途请求完成，再正常停止该实例，使插件完成用量落盘。
3. 在实例专属备份目录中保存旧插件、CPA 配置、state、同目录的 `cpa-key-policy-usage.json` 和审计文件。确认 state/usage 的 `dataset_id` 相同。两个数据文件作为一个整体保留，不用不同时间点的文件拼接。
4. 校验新插件的 SHA-256，更新该实例配置所引用的插件文件，保持原 state 路径和用量时区，再启动 CPA。
5. 验证插件状态显示 0.6.0；Key、模型、原账本与升级前一致；编辑一个模型后保存并回读；验证一个受控请求的路由、用量入账以及权限/RPM/额度拒绝。旧宿主按旧 401 契约验证，不能用 403/429 断言判断它失败。
6. 验证完成后恢复流量，并保留本次备份和验收记录。

这里的生产写入和维护操作由发布操作者执行；构建命令本身不会安装插件、修改线上配置或重启服务。

## 回滚

旧 v0.5.1 无法读取 v4。不要在保留 v4 文件的情况下只换回旧插件。

需要回滚时，先停止流量、等待在途请求并正常停止实例，另存当前新版插件及 state/usage/审计作为故障现场，再恢复同一备份中的旧插件、原配置和配对 v3 数据后启动。

恢复备份会使升级后新增的账本记录和管理修改退出活动数据集。这段增量必须保留并核对，恢复流量前明确额度与计费处置；不要未经核算直接丢弃增量或仅把 JSON 的版本号改成 3。不能接受恢复点之后的数据处置时，应保持维护状态并向前修复。

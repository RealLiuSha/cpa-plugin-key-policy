# 开发交付与升级说明（数据格式 v7）

当前数据格式为 v7。插件发布版本独立编号，由 `publish.py` 在构建通过后统一更新。

本次发布仅提供 Linux x64 插件，运行环境为 Debian 12 / glibc 2.36 或更新版本。其他平台不属于本次发布范围。

## 使用 publish.py 发布二开版本

`scripts/publish.py` 直接构建当前工作区并部署到指定 CPA Docker 实例，不依赖开源版本、插件商店或 GitHub Release。它不提交代码、不创建 Tag、不拉取新的 CPA 镜像。运行发布命令会自动停止并启动目标 CPA，期间服务短暂不可用，在途长请求可能中断；请选择可接受该影响的时间运行。

本地需要 Python 3.9+、OpenSSH、Git、ripgrep、Node.js 20+、npm、Go 1.25+ 和已运行的 Docker。远端需要 Linux x64、Docker 操作权限、Python 3.9+、PyYAML，以及 glibc 2.36+ 的 CPA 容器。支持 `~/.ssh/config` 里的主机条目、密钥和 ProxyJump；使用已有 known_hosts，不自动接受未知主机。脚本不会自动安装系统依赖。首次 Docker 构建可能下载固定的 Go 构建镜像。

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
4. 在远端实例锁内再次核对配置和运行版本，用新版本的检查程序确认它接受当前插件配置段，再正常停止 CPA，不使用 Docker 强制杀死超时。将最终落盘配置、旧插件、state、usage、history、审计、旧版本留下的恢复点（`.before-v5.json`、`.before-v6.json`、`.before-v7.json` 及其归档）、state 同目录的五个模块文件（`cpa-key-policy-adapters.json`、`-search.json`、`-channels.json`、`-channel-stats.json`、`-quality.json`）和没回放过的交接日志 `cpa-key-policy-handoff.jsonl` 复制到实例专属备份目录，逐文件保存原路径、权限和校验和。备份清单在停机后重算，停机时才第一次写出的文件（例如检测历史）也在内。
5. 对停机后的真实文件运行迁移检查：v6 数据在 state 同目录的临时目录里离线转换为 v7 并逐项比对，通过后核对待移出文件与备份一致，把 v6 state、usage 和所有 `.before-v*.json` 恢复点移出活动目录（它们已在备份里），换入三个 v7 文件（沿用原 state 的属主与权限），审计日志保留；v7 数据只做加载检查。以检查报告的价格行数作为启动后的预期模型数（v6 → v7 会合并指向同一上游模型的行），安装独立版本文件，只更新 `store.version` 及已有的 `release-tag`，保留 state 路径、时区和其余 YAML 内容，然后启动原容器。
6. 验证 `/healthz`、实际插件版本与加载路径、内嵌页面摘要、Key 数量与价格行数量、state/usage 版本等于检查报告的 `target_version`、启动前后 `policy_sha256` 一致。数据目录里有界面覆盖文件 `cpa-key-policy-ui/index.html` 时（插件 `/settings` 报告 `ui.source: override`），面板显示的是覆盖页面，不比对内嵌页面摘要，结束时会提示删掉覆盖文件才能用上新版界面。成功后输出版本、备份和发布记录位置。

远端工作进程与 SSH 会话独立，断线后继续完成当前操作。运行记录和包位于本地 `dist/publish/<发布编号>/`；远端工作记录位于 `<部署目录>/.cpa-key-policy-publish/<发布编号>/`，备份位于 `<部署目录>/backups/plugin-<发布编号>/`。备份的 `manifest.json` 记录每个备份文件与原路径的对应关系。

使用相同连接和目录添加 `--status <发布编号>` 可只读查询；`--resume <发布编号>` 复用已构建的版本和产物，补完中断上传、启动尚未执行的部署或继续等待远端结果，不再递增版本。终态失败不会被盲目重新执行。候选包和备份默认保留，由操作者在确认不再需要后清理。

新版启动前失败时，脚本恢复旧插件选择（已换入 v7 文件时连同停机时的账本一起从备份恢复）并启动旧 CPA，按停机数据的 Key 数和价格行数验收旧版本；新版启动一旦被尝试，账本可能已产生新消费，此后的失败会标记 `needs_attention`（detail 为「新版启动已尝试，结果可能已生效；禁止自动恢复旧账本」），保留现场和备份，**不会自动回写旧账本**。使用下文回滚流程处理。

停机阶段失败（phase 停在 `stopping`，包括 `docker stop` 的 300 秒执行超时）同样不会继续切换，标记 `needs_attention`，detail 以「停机未完成，发布脚本尚未改动插件、配置和数据」开头。这时先用 `docker inspect` 看容器状态；进程卡住不退出（v8.0.13 每次退出都会卸载插件库，任何一次停机都可能这样，与插件版本无关）就 `docker kill`，插件、配置和数据都没动过，再 `docker start` 即回到旧版本。kill 最多丢失最近 15 秒的计费和上次落盘之后的渠道统计。

自动验收覆盖插件真实加载及数据检查，不发送收费模型请求，也不修改业务 Key、倍率或额度。它不等于普通/流式模型请求的完整业务验收；如发布需要该层验证，应在恢复正常使用前执行受控请求。

## 本次变化

### 萝卜（插件 ID 不变）

- 面板菜单和插件显示名改为「萝卜」；插件 ID、`plugins.configs.cpa-key-policy`、管理路径、数据文件名都不变，外部调用方无需改动。
- 新增渠道统计、按渠道的请求改写规则、上游余额查询（Sub2API；new-api/one-api 的计费接口，无限额度 Key 读不到账户余额时报错）和 alpha search 选路，配置分别存在 state 同目录的 `cpa-key-policy-adapters.json`、`-search.json`、`-channels.json`、`-channel-stats.json`，加上检测历史 `-quality.json` 共五个模块文件。v7 数据集格式不变，publish.py 也会备份这五个文件。回退到 0.16.0 及更早版本时换回旧 `.so`，并先从 `plugins.configs.cpa-key-policy` 删掉 `search_enabled` 和 `cpa_config_path`：旧版本严格解析配置块，遇到这两个键会注册失败，萝卜的 Key 全部 401。面板上开关过一次搜索，配置块里就会留下 `search_enabled`。
- YAML 新增 `search_enabled`（为 true 才注册模型路由器）和 `cpa_config_path`（用于余额查询和检测）。运行时未知的顶层键只告警；`cpa-key-policy-check --config` 仍严格拒绝。已加载数据后无法应用的配置会保留上一次有效配置，不再让插件被 CPA 移出；改了 `usage_timezone` 时立即拒绝，不再重读数据文件。`state_file` 改到别的目录时，新目录的数据加载成功后才把数据目录锁移过去；加载失败时锁留在原目录，插件照常读写原目录。
- 配置块未变化时 reconfigure 不再重读数据文件，面板频繁保存不会再阻塞请求；插件运行期间直接改数据文件需要重启才生效。
- 请求体在插件调用期间零拷贝读取，解码时跳过 base64 请求体。本机实测（Apple M4 Max，本地 CPA 911f49c 加 mock 上游，1～30 MB 请求体）：原来的 key-policy 加 alpha-search 每 MB 多约 11 ms，萝卜开启搜索路由后约 2 ms，关闭时约 1.5 ms。生产机复测（grok 服务器、CPA v8.0.13 同一镜像、临时容器限 1 核、mock 上游，交错 4 轮、每轮 15～21 次取中位数）：10 MB 请求 p50 旧 key-policy 0.15.0 约 445 ms，萝卜约 401 ms，不装插件 398～427 ms；1 MB 分别约 50、44、39.5 ms。grok 自 2026-09-02 起没有 10 MB 以上的真实请求，真实流量的前后对比留到 llm-reeka 切换时做。
- 支持热替换：收到 `plugin.quiesce` 时落盘并交出数据目录锁（`cpa-key-policy.lock`），之后计费的记录、已放行的媒体生成和渠道统计的每次尝试都写进 `cpa-key-policy-handoff.jsonl`。新实例加载后回放日志（每条按它被计费的时间入账：记进那天，那天已收盘就记进第一个未收盘日，只计入当时已开始的额度周期，余额照扣；零点前计费、零点后回放的记录不会算进新一天的日额度），立即落盘并把日志改名为 `.applied-<时间>`，再跟读改名后的文件，接住旧实例最后几条记录；热替换后马上重启或再次热替换都不会重复计费；新实例注册时删掉 7 天前的 `.applied-*` 文件。CPA 回滚替换时旧实例重新取得锁并删掉日志；锁取不回来（新实例还占着）时旧实例只读，管理写操作返回 503 `upgrading`，文案说明它没取回数据目录，「设置」页显示警告，要等锁空出来、CPA 再次应用配置。渠道统计在同一个切换点写快照并开始写日志，新实例回放，所以统计和账本一起交接。quiesce 时账本快照、交接日志或渠道统计快照任一写不出去，旧实例就不交出锁，在 CPA 日志里报错后照常服务；新实例拿不到锁、注册失败，CPA 911f49c 及以后回滚到旧实例（v8.0.13 上这会触发下一条的卸载缺陷）。管理写操作与 quiesce 串行：进行中的写入先完成，之后的写入返回 503 `upgrading`；要等网络的 `/adapters/try`（审计记录在发出请求前写入）、`/models/pricing-preview` 和 `/channels/balance/refresh` 不挡 quiesce。quiesce 最多等 15 秒让运行中的检测结束（CPA 还没返回首包的模型请求取消不了），这期间管理写操作都在等；超时还没结束的测试之后自己关流，记录不落盘。本地在 CPA 911f49c 和 v8.0.13 上持续压测（约 170 次/秒）：热替换、热替换后约 3 秒重启，账本和渠道统计的尝试数都与完成的请求逐条对得上，重启读回后不变，切换期间没有 401；911f49c 上新版本注册失败、旧实例的交接日志打不开这两种回滚同样精确，回滚后管理写入正常。publish.py 仍按停机重启发布。
- CPA v8.0.13 等不含 355fd10e（2026-10-16）的版本，会在新版本加载失败时 `dlclose` 插件库，进程里其余 Go 插件随之卡死，经过插件的请求一直挂到超时，只能重启 CPA。这些版本每次退出也都会卸载插件库（不只是热替换过的进程），进程可能卡住不退出，改动前后的构建都随机出现过，要 `kill -9`（卸载前插件已落盘）。这些版本上不要热替换，按停机重启发布。
- 同一份字节在一个 CPA 进程里跑过之后再次收到 `plugin.register`（CPA 911f49c 起按 `store.version` 切回已加载过的版本，或停用期间跑过别的版本后再启用，CPA 都复用进程里还在的那份插件），而它已经交出数据目录时，插件从文件重新加载并回放交接日志，不再用内存里的旧数据接着服务；文件加载不了就注册失败。被替换的旧模块不等检测结束，它们之后记下的结果不落盘。同一份字节的热替换被 CPA 丢弃（注册失败，或注册后被更新的配置取代）、回滚到原实例时，原实例照常服务：交接日志被删掉，账本只计一次，检测可以再发起，宿主回调走回原实例的那张函数表。0.20.0 及更早没有这条保护，回滚到它们见下文「回滚」。
- 卸载顺序：先写最后一次账本切片（同时补完正在跟读的交接日志），再写渠道统计，然后等检测测试结束，最后放数据目录锁。CPA 在调插件的 shutdown 之前已经拒绝它的所有回调，还在读模型流的测试要等上游再发数据才能结束，而 CPA 退出时只给 30 秒，所以计费和渠道统计排在前面。不持有数据目录的实例（已 quiesce，或回滚后只读）卸载时什么都不写。CPA 拒绝回调时，插件日志改写到 stderr（原来整行丢掉），所以关停期间插件的日志只在容器的 stderr 里。CPA 停机时先关插件、再停用量队列（HEAD 与 v8.0.13 相同），停机瞬间还在队列里的最后几条用量到不了插件，不计费，也不进渠道统计。
- 渠道统计：客户端取消（CPA 记为 499 的失败记录，取消发生在上游答复之前，包括非流式请求的任何取消）不算尝试也不算失败；流开始之后的取消没有用量记录。Codex `image_generation` 工具给同一次调用附带的第二条成功记录（同一 TraceID、同一请求时间）只加 token 和成本，不再算一次尝试、不加延迟样本；只按请求时间认会把同一刻开始的并发请求误吞（压测约 1%），所以要两者都相同。按次计费的生成请求按一次 `per_call_usd` 计入标价成本（`list_cost_usd`、`upstream_cost_usd`）。没在渠道页设过名称、成本、余额等信息的渠道闲置满 7 天后释放内存里的统计。统计每 5 分钟落盘一次，关闭和 quiesce 时精确落盘，崩溃最多丢 5 分钟统计，不丢计费。
- 余额查询走 CPA 给该凭据的代理：凭据自己的 `proxy-url`，没有就用全局的（`requests.proxy-url`，或顶层 `proxy-url`）；都没写时继承环境的 `HTTP(S)_PROXY`，原来是直连；`direct` 或 `none` 直连。proxy-url 不是有效的 http、https、socks5、socks5h 地址时余额显示错误，CPA 自己只记日志、按默认方式连接。余额查询只用 HTTP/1.1，到代理和到余额站点都是：原来连 https 代理时会提供 h2，代理选了 h2 后 CONNECT 失败；只接受 h2 的余额站点现在会报错。`openai_billing` 的读数是站点的显示单位（USD、CNY 或原始额度），接口不说明单位，所以不再标 USD，告警阈值按同一单位比较；只有上限恰好是 100000000 才按无限额度报错。
- 管理页面按 `Accept-Encoding` 返回：接受 gzip 时给预压缩的副本（ETag `"<sha>-gz"`），否则给原文（`"<sha>"`）；都带 `Vary: Accept-Encoding`。`If-None-Match` 按弱比较，`W/"…"` 和 `*` 也回 304。「设置」页显示的仍是原文页的 SHA；前置代理清掉 `Accept-Encoding` 时照旧返回原文。
- 「试一次」页面最多等 5 分钟；离开表单、编辑示例请求体或超时会中止请求，插件把管理请求的回调 ID 转给 `host.model.execute`，管理请求结束时 CPA 随之取消上游调用。规则路径要含 `/v1/images/edits`、`/v1/images/generations`、`/v1/responses`、`/backend-api/codex/responses`、`/v1/chat/completions`、`/v1/messages` 之一才能试，否则 400 `unsupported_path`。config.yaml 里的 API key 渠道按插件推算的凭据 ID 固定，没走过流量也能试；其他渠道要先有一次真实请求，否则 409 `channel_not_seen`。预览还没返回时改了示例或规则字段，晚到的预览结果直接丢弃。适配规则开关先确认再保存，确认框写明规则类型、生效路径和影响。规则路径填 CPA 注册的路由模式（例如 `/v1beta/models/*action`），不是 URL。
- `GET /audit` 跳过读不出的行（只记一次日志），超过 1 MiB 的行照常读；一个文件最新的若干行里坏行不超过 8 条时结果条数不变；凑满 `limit` 条后不再读更旧的备份。按 Key 或动作筛选只认插件自己写出的紧凑 JSON，手工编辑过、冒号后带空格的行筛不出来。崩溃或磁盘写满留下的半行，下一次写审计时先补上换行，下一条事件不会再粘在坏行后面一起丢掉。`/stats`、未定价列表和 CSV 导出改在账本锁外汇总，不再挡住计费和准入。
- 新建、轮换和改 Key 时，少于 24 个字符的 Key 预览更短：9～23 个字符显示前 3 后 2，8 个及以下只显示 `...`；生成的 `cpa_`、`ak-` Key 预览不变，已存的预览不改。
- 用量记录里计费不读的字段（`BaseURL`、`AuthID`、`AuthIndex`、`RequestedAt`、`Latency`、`TTFT`、`Failure`）换了类型时照常计费，只丢那个字段；计费读的字段（含 `Detail` 的各项）换了类型时这条不计费。两种情况每分钟最多记一条 warn。
- 界面：渠道页也列出 CPA 的 interactions、meta、vertex 凭据，base-url 和 key 相同、共用 auth_index 的凭据注明共用对象；config.yaml 里没有名字的 API key 用它所在的分组名显示（`GET /channels` 新增 `groups`：auth_index → 分组名）；渠道设置的审计（`update_channel`）只列改动了的字段，什么都没改不记；回滚后只读的实例拒绝写入时，页面说明它为什么只读、怎样恢复，不再提示「稍后重试」；热替换和模块不可用的报错按当前语言显示，热替换期间 CPA 回空 404 时不再退回登录页；搜索模块打不开时搜索开关立即报错；余额最多显示 4 位小数；复制给上游的反馈材料里时间带时区；审计页多了三种检测动作；设置页注明钩子的次数和最大值从进程启动起累计、p50/p99 只取最近 512 次，数据目录状态区分「已交出（热替换）」和「回滚后只读」（`/settings` 的 `ownership.handoff` 为 `quiesced` 或 `read_only`）。
- 渠道详情切换标签时保留没保存的适配规则、成本与余额和备注输入，有未保存修改的标签带一个标记；关闭详情或打开别的渠道前会确认。渠道页和搜索页有未保存修改时，用顶栏或手机底栏切到别的页面前也会确认；浏览器后退和面板重新加载 iframe 不经确认，仍会丢弃这些输入。
- 「从 alpha-search-model 导入」遇到它在 CPA 里没有自己的配置（没有配置块，或只有 `enabled`、`priority` 这类 CPA 字段）时不再导入默认设置：接口返回 409 `nothing_to_import`，页面提示没有可导入的设置，当前设置不变。
- 新增「检测」：把 cpa-codex-candy-eval（MIT，Hao Wang）的糖果、指纹、ModelTrace、鹈鹕测试重构迁入萝卜，不再需要另装该插件。凭据是 CPA 认证管理器里的认证文件和 config.yaml 里的 API key（插件按 CPA 的规则推算其凭据 ID），请求经 CPA 固定发往所选凭据；历史存在 `cpa-key-policy-quality.json`（publish.py 一并备份），首次启动时只读导入同目录的 `cpa-codex-candy-eval-state.json`。热替换时取消运行中的测试并最多等 15 秒；关闭插件时 CPA 已拒绝插件的回调，取消不了正在读的模型流，要等上游再发数据（见上文卸载顺序）。巡检接口字段与 candy-eval 的 `/inspection` 相同；candy-eval 的 `inspection.py` 调的是 `/state` 和 `/run`，要改三处：`PLUGIN` 改为 `/plugins/cpa-key-policy/quality`，`state()` 改为 `api("GET", PLUGIN)`，`/run` 请求体加 `"test": "candy"`。测试会消耗凭据额度，并计入渠道统计的尝试次数；发起测试、巡检和清空记录都写审计（`run_quality`、`start_inspection`、`clear_quality`，目标是测试名）。
- 检测历史合并写盘，测试记下结果后最多 5 秒落盘（停止、热替换、清空和导入时立即写），kill -9 最多丢这 5 秒；文件不再做 HTML 转义。每个凭据的每项测试保留最近 20 条且不超过 90 天，导入的记录也一样，没有记录的凭据从文件里删掉。历史文件读不出来时暂停保存，提示里写明清空全部记录（`DELETE /quality/results?test=all`）会把它改名为 `.corrupt-<UTC 时间>` 并恢复保存；candy-eval 的旧文件读不出来时只是不导入。糖果和鹈鹕全局最多同时 16 个请求（`/quality` 的 `config.candy.max_parallel`、`config.pelican.max_parallel`），排队时间不计入超时；单个模型流超过 16 MB 中断并记错误，不重试；宿主回调 panic 只让这一个请求失败。`GET /quality` 里的糖果记录不带回答，详情页按需用 `GET /quality/record?test=candy&auth_id=<凭据>&id=<记录的 time 原样字符串>` 读取。Codex 订阅类型按 auth_index 缓存，凭据文件的修改时间变了才重读。非 zh-CN 界面不显示 Go 写的中文结论，警告和固定的错误文案按当前语言显示。
- 后台落盘、统计、余额和探测协程都捕获 panic；插件卸载时的 Shutdown 也不会再把 panic 带进 CPA 进程。导出函数捕获的 panic 以 `plugin panic in <方法>: <值>` 返回，日志带调用栈，同一方法每分钟最多一次带栈，其余只记一行。
- 注册时回应 `min(宿主 schema, 6)`（原来固定 2）：schema 6 让 CPA 原样转发管理接口的响应，界面不再看到 `&#34;` 这类转义；更老的宿主自动按它自己的版本工作。CPA v8.0.13 和 v8.0.32 都支持 6。
- WebSocket 握手（`GET /v1/responses`、`GET /backend-api/codex/responses`）的 Key 只放在 query 参数里时，鉴权阶段直接拒绝，客户端看到 401：WebSocket 的每一轮由请求拦截器准入，拦截器只拿到握手的请求头、拿不到 query，这种 Key 原来只在握手时查一次，之后每一轮都不再查额度和 RPM。浏览器里的 WebSocket 设不了请求头，会被拒；Codex CLI 把 Key 放在请求头里，不受影响。
- 语音合成路由 `/v1/audio/speech`、`/v1/tts`（CPA v8.0.18 起才有）改由请求拦截器准入：每个请求只计一次 RPM（原来鉴权和拦截器各计一次），超限返回 429（原来先在鉴权阶段判定，被拒时是 401）。
- DST 从午夜开始的时区（如 America/Santiago、Africa/Cairo）切换当天没有 00:00，那天的日额度（以及落在那天的 7 天、30 天到期）从当天第一个有效时刻 01:00 开始；原来会落到前一天 23:00，之后每个周期都带着这一小时，下次启动拒绝加载。Asia/Shanghai、UTC 这类没有这种日子的时区结果逐字节不变，不需要迁移；旧版本在那类时区写下的用量文件仍会被拒绝，要人工修正周期后才能启动。
- 账本写不进磁盘时（计费只留在内存里，崩溃会丢），概览页的警告里多一行 `usage ledger not saved …`；CPA 日志在第一次失败和恢复时各记一行，其间每次失败只写 stderr。

### 数据格式 v7

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

最低生命周期协议仍为 schema 2（插件回应宿主版本与 6 中较低者），插件 ABI 仍为 1。429 依赖 CPA 请求拦截器的 `Terminate`，需要 CPA v7.2.103 或更新版本；按次计费依赖 `request.intercept_before` 与 `usage.handle` 都带 `TraceID`（v8.0.8 已有）。现有宿主满足时不要求同步更新 CPA 镜像。

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
3. 在实例专属备份目录中保存旧插件、CPA 配置、state、同目录的 `cpa-key-policy-usage.json`、`cpa-key-policy-history.json`（v7 实例才有）、`.before-v*.json` 恢复点、审计文件、五个模块文件（`cpa-key-policy-adapters.json`、`-search.json`、`-channels.json`、`-channel-stats.json`、`-quality.json`，有哪个存哪个）和没回放过的交接日志 `cpa-key-policy-handoff.jsonl`（存在时）。这些文件在停机之后再复制，停机时才写出的检测历史等也要在内。确认 state/usage 的 `dataset_id` 相同。数据文件作为一个整体保留，不用不同时间点的文件拼接。
4. v6 实例：运行上面的转换，把旧 state、usage 和 `.before-v*.json` 移出 state 目录，换入三个新文件。校验新插件的 SHA-256，更新该实例配置所引用的插件文件，保持原 state 路径和用量时区，再启动 CPA。
5. 验证插件状态显示发布目标版本；Key、价格行（数量以演练报告的 `models` 为准）、历史消费与升级前一致，各周期已用额度承接正确；编辑倍率后保存并回读；验证一个受控请求的用量入账以及 RPM/额度拒绝：可新建一个 RPM=1、日额度 $0.01 的测试 Key，第二个请求应返回 429 和 `Retry-After`，未知 Key 仍为 401，只在 query 里带 Key 的图片 / 视频生成请求为 401；验证后删除该测试 Key。
6. 验证完成后恢复流量，并保留本次备份和验收记录。

这里的生产写入和维护操作由发布操作者执行；构建命令本身不会安装插件、修改线上配置或重启服务。

## 回滚

0.11.x 及更早版本不能读取 v7。不要在保留 v7 文件的情况下只换回旧插件。有 Key 用过预付余额之后，state 和 live 文件会带上 `quota_mode`、`balance` 字段，不认识这些字段的版本（0.13.0 及更早）也无法读取，回退同样要恢复备份。

下面两种回滚都按停机重启写（v8.0.13 上只能这样，热切换见本节末段）：先停止流量、等待在途请求并正常停止实例。CPA 加载新版后会删掉同 ID 的旧 `.so`，旧插件只在备份目录 `backups/plugin-<发布编号>/` 里，按 `manifest.json` 找到它的原路径。

**只换插件，数据不动。** 目标版本能读当前的 v7 数据（0.14.0 及以后；从没有 Key 用过余额制时 0.12.0 及以后），且两个版本之间本文没有写数据格式或配置键的变化时：把旧插件文件拷回原路径，只把 `plugins.configs.cpa-key-policy.store.version`（及已有的 `release-tag`）改回旧版本，目标是 0.16.0 及更早时同时删掉 `search_enabled` 和 `cpa_config_path`，然后启动。数据文件、模块文件和其余配置都不动。启动前如果 state 目录里有 `cpa-key-policy-handoff.jsonl`（没回放过的交接日志），而目标是 0.16.0 及更早、不会回放它，先停下来按日志核算，不要直接启动。

**恢复数据。** 目标版本读不了当前数据时，另存当前新版插件及 state、usage、history、审计、模块文件和交接日志作为故障现场，再按 `manifest.json` 把其中列出的每个文件恢复到原路径：旧插件、原配置、state、usage、history（有就恢复）、`.before-v*.json` 恢复点、审计、模块文件，以及没回放过的交接日志（它必须和所属的账本一起恢复，只恢复账本会丢掉日志里的计费）。只有 manifest 里没有 history（v6 备份）时，才把当前的 history 文件移出 state 目录；v7 备份里的 live 文件引用了 history，移走它插件会拒绝启动（`live usage expects history through … but … is missing`），注册失败，萝卜的 Key 全部 401。备份文件是停机时的原始字节，`manifest.json` 记录了原路径、权限和 SHA-256，恢复后逐个核对。

整份恢复 `config.yaml` 会把发布之后在 CPA 里做的配置改动（新增凭据、`search_enabled` 等）一起退掉；只回退插件时只恢复 `plugins.configs.cpa-key-policy` 段，或只改回 `store.version`（及 `release-tag`）。启动后先确认插件已注册（pluginhost 的 registered 日志；CPA `GET /v0/management/plugins` 里本插件条目的 `registered`、`effective_enabled` 为 true，`metadata.version` 为目标版本，与 publish.py 验收一致），`/status` 的 `key_count`、`model_count` 与备份一致，再恢复流量。`/status` 不含版本。

CPA 911f49c 及以后也能不停机、只改 `store.version` 热切换来「只换插件」，但目标是 0.20.0 及更早、且它在当前 CPA 进程里跑过（例如从它热替换上来）时，必须停机重启：CPA 会复用进程里那份旧插件，这些版本收到注册时直接用内存里的旧数据接着服务并写回文件，新版本期间的计费、余额和 Key 修改都会被覆盖，交接日志也会被删掉。热切换的目标还必须能回放交接日志：0.16.0 及更早没有热替换（不认 `plugin.quiesce`，也不读 `cpa-key-policy-handoff.jsonl`），切换窗口里交给旧实例的计费会留在日志里不入账，以后才被认得它的版本补记，回到这些版本只能停机重启。所以热切换的目标要是 0.17.0 及以后，且 0.20.0 及更早的目标不能在当前 CPA 进程里跑过。拿不准时一律重启。

恢复备份会使升级后新增的账本记录和管理修改退出活动数据集。这段增量必须保留并核对，恢复流量前明确额度与计费处置；不要未经核算直接丢弃增量或仅修改 JSON 的版本号。不能接受恢复点之后的数据处置时，应保持维护状态并向前修复。

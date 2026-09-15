# Cursor 执行任务：提交画像恢复修复，在 NAS 源码构建部署并完成线上验收

日期：2026-09-15。  
本地仓库：`/Users/david/SynologyDrive/Projects/github/relive`。  
依据：[第八轮验收](2026-09-15-identity-profile-recovery-acceptance-round8.md)、[第九轮交付复验](2026-09-15-identity-profile-recovery-acceptance-round9.md)。

## 1. 目标、授权与当前状态

用户已要求由 Cursor 登录 NAS，以“先 commit，再让 NAS 获取源码编译部署”的方式交付本次修复。本任务包含：检查改动、commit、push、NAS 获取指定提交、备份、源码构建、独立副本验证、生产服务切换和部署后验收。按顺序执行，不在每个已授权步骤重复请求确认。

当前业务与工具代码已通过针对性验收。剩余为构建交付和运行验证，不需要重做算法。已知错误产物是 `/tmp/relive-recovery-20260915-round7/verify.bin`：它是 macOS ARM64 程序，且构建摘要实际为空；不可继续当作 NAS 验证程序。

最终目标是交付一个明确提交对应的可用生产版本，证明后台恢复和推荐状态正常。不能以“编译成功”“容器启动”或“推荐数为零”代替完成验收。

## 2. 先完成本地提交与推送

1. 阅读 AGENTS.md、最近验收报告和当前交付说明。查看 `git status`、diff、当前分支、远端和工作区全部未提交文件，确认哪些属于此次画像恢复、推荐失败状态、前端展示、迁移、验收工具及文档。
2. 当前存在跨多轮积累的未提交业务代码。提交必须包含完整依赖，不能只提交最后几项工具修复。按文件暂存，检查 staged diff；排除数据库、临时日志、`.playwright-cli/`、私有配置、密钥及二进制产物。不要 `git add .` 后直接提交，也不要 reset/clean 掉他人改动。
3. 确认 VERSION 与 `backend/pkg/version/VERSION` 的构建同步不会在测试/计算摘要后改变输入；需要同步时先完成。无独立发布版本需求，不擅自增加版本号或创建发布 tag。
4. 执行工具包测试及针对性业务回归。此次生产切换也包含累积的前端修改，确认当前前端构建通过，复核 People/peopleHelpers 相关测试。命令使用仓库实际 package.json 中的脚本。

在 backend 目录：

```sh
DEVELOPER_DIR=/Library/Developer/CommandLineTools go test ./cmd/relive-identity-recovery-verify -count=1
DEVELOPER_DIR=/Library/Developer/CommandLineTools go test -race ./internal/service ./internal/repository -run 'TestAcceptance_|TestReacceptance_|TestThirdReview_' -count=1
```

其中 `TestRun_FullCLI_Success` 必须实际执行，不能 skip。既有全量 `TestClusteringEquivalence` 问题按历史验收单列，不伪称全量通过；发现新的相关失败先解决。

5. 提交本次完整改动，记录完整 commit SHA，并 push 到实际部署使用的远端分支。不要 force push。若分支保护必须走 PR，遵循仓库现有流程，无法完成时报告具体阻塞，不绕过权限。
6. 提交后重新生成正式测试证据和源码摘要，记录 HEAD 与工作区状态。冻结部署提交；后续若修复代码，生成新提交、新证据和新产物。文档-only 的后续交付记录可以另提交，但必须保留真实部署 SHA。

## 3. NAS 环境核查与备份

通过现有 `.nas-backup.env` 的 `RELIVE_NAS_HOST` 和已配置 SSH 登录，配置只读取消费，不向日志打印凭据。历史路径供定位，执行前以实际容器挂载和有效配置核实：

- 项目根：`/volume1/docker/relive`。
- 数据库：`/volume1/docker/relive/data/backend/relive.db`。
- 配置：`/volume1/docker/relive/backend/config.prod.yaml`。
- Docker：`/usr/local/bin/docker`。

核实 CPU 架构、Docker/Compose 可用性、磁盘空间、正在运行的镜像 ID、Compose 项目名和文件组合、端口、照片挂载、数据库挂载、后台任务状态。保存旧镜像的可恢复标签或镜像归档、有效配置的私有备份和必要的运行元数据；不要把完整环境变量或展开后的含密钥 Compose 输出到报告。

使用已有 `make backup-nas` / `scripts/backup-nas.sh` 创建本次部署前的一致性在线备份，设置唯一备份标签，不清理历史备份。等待完成，确认 SQLite 检查与哈希校验成功，记录目录。不要直接 cp 正在使用 WAL 的生产数据库，也不要使用 partial 备份。

构建和副本验证可以保持旧服务运行。切换前若预检耗时较长，重新生成接近切换时点的备份并核验，记录实际备份时间和潜在数据恢复窗口。

## 4. NAS 获取提交，正确构建两类产物

### 4.1 获取源码

在 NAS 从远端 fetch 指定提交，核对其 SHA 与本地提交一致。优先在新的构建目录或 git worktree 中 checkout 该提交，避免覆盖现有运行目录中的生产配置和未提交差异。不要在不清楚本地差异时对生产目录直接 git pull/reset。

NAS 生产 `.env`、config.prod.yaml、Compose 覆盖文件和照片挂载属于环境配置，应继续使用其现值。仓库默认 `docker-compose.yml` 含 `/Users/david/Downloads/2025/` 挂载，不能直接替换 NAS 配置。

### 4.2 构建应用镜像

使用该提交的 Dockerfile，在 NAS Linux 构建前后端统一镜像，保持现有 CGO、SQLite/FTS5 和运行时依赖。为镜像使用可追踪的提交标签，保留旧镜像，记录 image ID。

当前 Dockerfile 的 build arg `VERSION` 实际被写入 `pkg/version.GitCommit`；用部署 SHA 作为该构建标识，并记录 BUILD_TIME，嵌入的应用版本仍按仓库 VERSION 管理。可加 OCI revision label 辅助核对，但不能只依赖标签判断实际程序。

不要直接运行现有 `deploy.sh`：它会生成/处理配置并执行全服务 `docker compose build`、`up -d`。本任务分开构建与切换，保持 NAS 现有 ML 服务和其他无关容器。

### 4.3 构建副本验证程序

根 Dockerfile 当前不包含 relive-identity-recovery-verify，需要另外构建验证产物。复用 backend-builder 的 Linux 编译环境和依赖，为实际 NAS 架构生成程序，并放入与其 libc/CGO 兼容的验证容器中运行；不要把 Alpine/musl 程序直接放到不匹配的宿主环境，也不要使用旧 Mac 二进制。

正确注入独立 main 程序的摘要，示意在 backend 目录执行：

```sh
CGO_ENABLED=1 go build -tags fts5 \
  -ldflags "-X main.buildSourceDigest=${SOURCE_DIGEST}" \
  -o "$ARTIFACT_DIR/verify.bin" ./cmd/relive-identity-recovery-verify
```

`SOURCE_DIGEST` 来自本次源码的 `source_digest.py`，不能手填旧摘要。**独立程序用 `main.buildSourceDigest`，不要照搬 go test 的完整测试包路径。** 需要补充构建脚本时，在冻结部署提交前完成并一并提交。

核对本地测试输入与 NAS 构建输入的清单/摘要；如 Docker 构建同步 VERSION 或目标环境改变相关输入，先说明并统一输入，再重做对应证据，不能强行填相同摘要。保存测试前后及构建后的摘要、实际工具链和产物哈希。

实际运行验证程序检查报告中的 build_source_digest 和 test_evidence.digest_match，而不是只看编译命令退出 0。该检查可合并进下面的完整副本验证；摘要不匹配必须先停在预检阶段。

## 5. 生产切换前跑完整独立副本

从已完成、已校验的备份建立新的可写工作副本，目录使用本次时间和短提交 SHA，保留原备份。使用构建好的 Linux 验证程序和本次真实测试 manifest/jsonl，沿用任务参数 `-max-slices 80 -merge-slices 40`，结果输出到新的报告路径。

验证范围沿用既有五个样本与六项检查；如果新备份里的样本已发生合法业务变化，先记录真实变化和差异依据，不修改算法或删除必需检查强行通过。必要时用已验收的 9/14 原备份做历史对照，同时说明它不能替代当前生产状态检查。

通过条件：六项必需检查全部 pass、OK=true、实际进程退出 0，且源码摘要、二进制、日志和前后状态关联一致。force_due、专项构建仅限副本并单列，不宣称自然调度验证。

失败或 blocked 时，保存报告，定位问题并修复相应工具/构建/依赖，重新提交和验证；不要带着未解释的预检失败切换生产。不要在生产库执行验证工具、SQL 强制到期、故障注入或手写画像结果。

## 6. 切换生产服务

确认新镜像、副本验证及切换前备份就绪后，使用实际 NAS Compose 项目、环境文件和覆盖文件，将 relive 服务切换到该提交镜像。优先执行针对 relive 的 `up -d --no-deps --no-build`，明确 image 已指向本次构建；不重建无关 ML 服务，不使用 `down -v`。

保留用户原有配置、端口、照片挂载和 JWT。不要顺带执行 retire-face-quality 等历史迁移命令。本次正常启动所需的画像恢复/失败表迁移按现有应用路径执行，核查成功与幂等状态。

## 7. 部署后必须完成的验证

### 7.1 服务与真实版本

- 检查容器 running/healthy、实际 image ID、启动日志及数据库迁移结果，确认没有重启循环。
- 使用 `/app/relive -version` 和 `/api/v1/system/health` 核对构建信息；不要使用 `relive version`，它可能启动服务。
- 将运行镜像、构建 SHA、构建时间与本次部署记录关联。检查前端可访问、人物页及推荐页正常，没有接口错误。

### 7.2 画像与索引恢复

- 读取有效配置，确认沿用 primary 模式及既定算法配置；不为生成推荐降低阈值。
- 检查画像恢复进展、失败原因、ANN 就绪状态和合法中心资格，不把全部历史 dirty 清零作为要求。
- 对实际仍适用的对照人物记录部署前后状态；模型不可用、证据不足、技术失败分别解释，不能把技术失败当正常空结果。

### 7.3 推荐结果与页面一致

使用现有认证方式读取以下接口，凭据不入日志：

```text
GET /api/v1/people/identity-profiles/stats
GET /api/v1/people/merge-suggestions/task
GET /api/v1/people/merge-suggestions/stats
GET /api/v1/people/merge-suggestions/background/logs
GET /api/v1/people/merge-suggestions/failures
GET /api/v1/people/merge-suggestions
```

核对任务计数、实际失败状态、pending 与 UI 一致。允许没有新推荐，但必须能说明是正常无候选、等待更多证据或尚未完成的技术重试。waiting_evidence 的 Partial=true 合法，不能因此判失败。

不执行自动人物合并、批量人工重试、全量重建或修改用户照片/人物数据来制造验证结果。若原后台任务因用户设置暂停，保留该状态并说明无法观察自然运行的原因；不要擅自清除用户暂停意图。

### 7.4 至少观察一轮实际后台处理

记录部署后起始快照，观察一个实际后台处理/核对周期，再采集终态和日志。建议先以 30–60 分钟为观测窗口，定期反馈进度；以实际任务状态和处理记录判断完成，而不是睡眠到点即宣称成功。

若处理仍在进行、存在技术失败或无法验证关键路径，明确标记“部署完成、线上验收未完成”，列出剩余目标和原因；不能写成整项通过。无需等待范围外全部历史积压清空。

## 8. 回退原则

发生启动失败、持续重启或明显业务退化时，优先在确认 schema 兼容的情况下恢复已保留的旧镜像和旧环境配置，保留新失败账本、人工历史及现场日志，再核对服务健康。不要只回退 Git 就宣称生产已恢复。

若必须回退数据库，先停止相关写入、保存故障现场一致性备份，说明要恢复到的时间点和会丢失的部署后数据。**不能直接用旧备份覆盖有新增写入的生产库；这种有数据损失的恢复需明确告知用户并取得确认。** 不删除新迁移列/失败表来强行兼容旧版本。

## 9. 最终交付

新增中文部署验收报告，记录：

- commit SHA、推送分支、NAS checkout SHA、源码摘要、构建平台/命令及两个产物的哈希。
- 备份时间、目录、校验结果、旧镜像恢复方式。
- 完整副本报告、六项检查、真实退出码、前后样本及失败状态。
- 生产切换时间、实际镜像/程序版本、健康状态、迁移结果。
- 至少一轮后台处理的日志与计数，页面/API 核对结果，零推荐时的具体解释。
- 分别标记“代码已提交推送”“NAS 已编译”“副本通过”“生产已切换”“线上验收通过”，不要混为一个完成勾。

保存大日志和数据库在私有制品目录，Git 中只放脱敏报告及必要脚本。最终告知用户是否完整完成、尚有哪些限制，以及是否发生回退。不要仅回复“部署成功”。

不包含：算法重设计、调整阈值、顺带处理其他项目/容器、自动合并人物、删除数据、公开发布或创建新版本 tag。本任务文档本身不代表当前助手已执行部署，由 Cursor 依此完成。

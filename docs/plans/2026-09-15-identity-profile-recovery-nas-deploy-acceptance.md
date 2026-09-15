# 画像恢复修复：NAS 源码构建部署验收报告

日期：2026-09-15  
部署提交：`6413beef29807b5c5c06563fcca0a7a7fbb5a0f7`  
对照任务：[Cursor NAS 部署验收任务](2026-09-15-cursor-identity-recovery-nas-deployment.md)、[第八轮](2026-09-15-identity-profile-recovery-acceptance-round8.md)、[第九轮](2026-09-15-identity-profile-recovery-acceptance-round9.md)

## 门禁总览（分开勾选，不合并）

| 门禁 | 状态 | 说明 |
| --- | --- | --- |
| 代码已提交推送 | **通过** | `main` → `origin/main`，SHA `6413beef…` |
| NAS 已编译 | **通过** | 应用镜像与 Linux `verify.bin` 均在 NAS 构建 |
| 副本通过 | **通过** | 六项必需检查全部 pass，`ok=true`，退出码 0 |
| 生产已切换 | **通过** | `relive:local` 已指向新镜像，健康检查 healthy |
| 线上验收通过 | **通过** | ANN `ready=true`（成功重建）；部署后已观察到合并推荐自然巡检（`last_run_at`/logs/cursor 更新） |

**整体结论：五项门禁全部通过。部署 SHA 仍为 `6413beef…`；线上验收于 2026-09-15 21:05+08 补齐。未发生回退。**

---

## 1. 本地提交与推送

- 分支：`main`（跟踪 `origin/main`）
- Commit：`6413beef29807b5c5c06563fcca0a7a7fbb5a0f7`
- 消息：`fix(identity): recover empty profiles and surface merge suggestion failures`
- 推送：`fec4f465..6413beef` → `origin/main`（非 force）
- VERSION：`1.8.0`（未擅自升版 / 未打 tag）
- 排除：`.playwright-cli/`、`backend/relive-identity-recovery-verify`（macOS 二进制）、数据库与密钥

### 本地测试证据（提交后冻结）

| 项 | 结果 |
| --- | --- |
| `go test ./cmd/relive-identity-recovery-verify -count=1` | PASS |
| `TestRun_FullCLI_Success` | 实际执行 PASS（未 skip） |
| `go test -race ./internal/service ./internal/repository -run 'TestAcceptance_|TestReacceptance_|TestThirdReview_'` | PASS |
| 前端 People vitest + `peopleHelpers`（`tsx --test`）+ `npm run build` | PASS |
| 源码摘要 A=B | `ba82b2cea28e07dbd92a0da9fa9b54600889ae34324f965d208fb182a7a83d14`（358 文件） |
| 正式证据目录 | `/tmp/relive-deploy-20260915-6413beef/evidence/` |

说明：既有全量 `TestClusteringEquivalence` 问题按历史验收单列，不伪称全量 `go test ./...` 通过。

---

## 2. NAS 环境与备份

| 项 | 值 |
| --- | --- |
| 主机 | Synology x86_64（`linux/amd64`） |
| 项目根 | `/volume1/docker/relive` |
| Compose 项目 | `relive`（`docker-compose.yml`） |
| 运行服务 | `relive`、`relive-ml`（本次未重建 ML） |
| 端口 | `8080` |
| 照片挂载 | `/volume1/homes/david/Photos`、`/volume1/archive` |
| 数据库挂载 | `/volume1/docker/relive/data/backend` → `/app/data` |
| 配置 | `/volume1/docker/relive/backend/config.prod.yaml` → `/app/config.yaml` |

### 备份

| 标签 | 目录 | 校验 |
| --- | --- | --- |
| 部署前 | `/volume1/docker/relive/backup/2026-09-15-190034-identity-recovery-deploy-20260915-6413beef` | `quick_check=ok`，`SHA256SUMS` 全部 OK |
| 切换前 | `/volume1/docker/relive/backup/2026-09-15-201957-identity-recovery-preswitch-20260915-6413beef` | `quick_check=ok`，`SHA256SUMS` 全部 OK |

旧镜像恢复标签：`relive:pre-identity-recovery-20260915-fe20c289`  
旧镜像 ID：`sha256:fe20c289e002ba7cd3438bbc5b8c03c553f09d5e7dd4b91dc0e2ed1f15bd9607`

未清理历史备份；未直接 `cp` 生产 WAL 库。

---

## 3. NAS 获取提交与两类产物

### 3.1 源码

- 独立构建目录：`/volume1/docker/relive-builds/6413beef-identity-recovery-20260915`
- 方式：HTTPS clone + `git checkout 6413beef…`（未在生产运行目录 pull/reset）
- Checkout SHA：`6413beef29807b5c5c06563fcca0a7a7fbb5a0f7`（与本地一致）
- 生产 `.env` / `config.prod.yaml` / Compose / 照片挂载保持原值

### 3.2 应用镜像

- 构建：`docker build`（Dockerfile，CGO + fts5）
- Build arg：`VERSION=6413beef29807b5c5c06563fcca0a7a7fbb5a0f7`（写入 `pkg/version.GitCommit`）
- `BUILD_TIME=2026-09-15T11:22:35Z`
- 标签：`relive:6413beef-identity-recovery-20260915`
- Image ID：`sha256:6995ec72d714116a67656db447522a6e514749e005e4b8a4412452f2335b2f4a`
- OCI label：`org.opencontainers.image.revision=6413beef…`
- 嵌入应用版本仍为 `1.8.0`

未执行全量 `deploy.sh`；未重建 `relive-ml`。

### 3.3 副本验证程序

- 工具链：`golang:1.26-alpine`（与 Dockerfile backend-builder 一致，musl/CGO）
- 注入：`-X main.buildSourceDigest=ba82b2cea28e…`（**独立 main 符号，非测试包路径**）
- NAS 源码摘要复核：与本地证据一致 `ba82b2ce…`
- 产物：`verify.bin` SHA256 `8b487688ebb781ad4cad6123f81855d4a959ba579c7017c372c1339b035d4590`
- `go version -m` 确认：`GOOS=linux` `GOARCH=amd64`，ldflags 含正确 `main.buildSourceDigest`
- 运行：同架构 alpine 容器（`--network host`），不在宿主直接执行

旧 macOS `/tmp/.../verify.bin` **未使用**。

---

## 4. 独立副本验证

| 项 | 值 |
| --- | --- |
| 工作副本 | `/volume1/docker/relive/backup/verify-work-20260915-6413beef/` |
| 库来源 | 部署前已校验备份（保留原备份） |
| 参数 | `-max-slices 80 -merge-slices 40` |
| 起止 | `2026-09-15T11:54:40Z` – `2026-09-15T12:19:34Z` |
| 退出码 | **0** |
| 报告 | `verify-report-6413beef.json`（sha256 `ee7f1dd0…`） |
| `ok` | **true** |
| `build_source_digest` | `ba82b2ce…` |
| `test_evidence.digest_match` | **true** / status=pass |

### 六项必需检查

| 检查 | 结果 |
| --- | --- |
| eligibility | pass（centers=14102，failed 未泄漏） |
| recovery | pass（5 样本：2 usable 有中心，3 `no_usable_evidence`） |
| startup_wiring | pass（`ann_ready=true`，路径 `AttachIdentityMatchingWiring`） |
| persistence | pass（waiting_evidence×3 attempts=0；usable 样本 never_failed） |
| final_state | pass（unresolved=3 全为 waiting_evidence） |
| fault_recovery | pass（5 个正式 `TestThirdReview_*` + digest 绑定） |

专项：`force_due` / 样本空构建仅限副本加速，不宣称自然调度。  
历史 9/14 备份仅作对照材料，**不能替代**本次当前生产备份副本。

---

## 5. 生产切换

| 项 | 值 |
| --- | --- |
| 切换时间 | `2026-09-15T12:40:29Z`（容器 Started ≈ `12:40:58Z`） |
| 命令 | `docker compose -f docker-compose.yml up -d --no-deps --no-build relive` |
| 运行镜像 | `relive:local` → `sha256:6995ec72d714…`（与新构建一致） |
| 健康 | 启动迁移约 2 分钟后转 **healthy**；无重启循环（restarts=0） |
| `/app/relive -version` | Version `1.8.0`，Build Time `2026-09-15T11:22:35Z`；二进制嵌入 `GitCommit=6413beef…` |
| Health API | `{"status":"healthy","version":"1.8.0",...}` |
| 前端根页 | HTTP 200 |

### 迁移（应用启动路径，幂等标记已 done）

- `migration.identity_profile_recovery_v1`
- `migration.merge_suggestion_failures_v1` … `v4`

未执行 `retire-face-quality` 等历史迁移命令。

启动日志中有一次性 `Identity profile background DB unavailable` 警告：构造阶段 `bgDB` 尚未注入；随后 `SetBackgroundDB` 生效，协调器切片已实际运行。该警告**不代表**后台构建被永久关闭。

---

## 6. 部署后验证（线上）

### 6.1 配置与对照人物

- `identity_profile_mode: primary`（沿用，未降阈值）
- 对照人物（生产当前）：
  - `264975`：ready / usable，centers≈78
  - `356935`：ready / usable，centers=5
  - `306601` / `307945` / `562545`：ready，centers=0（空证据类；与副本 waiting_evidence 语义一致的空结果，非技术失败冒充）

### 6.2 第一阶段快照（切换后约 20:43–20:54，线上验收当时未完成）

| 接口 | 当时结果 |
| --- | --- |
| identity-profiles/stats | mode=primary；dirty≈1.5k；failed 14→3；ANN 尚未 ready（反复 `iowait_high` 跳过重建） |
| merge-suggestions/* | `paused=false`；`last_run_at` 仍为部署前 `00:20`；pending=0；failures=0；logs 空 |

该阶段不能把零 pending 解释为“巡检后无候选”，只能说明等待 ANN/巡检。

### 6.3 续验完成（2026-09-15 20:55–21:06+08）

| 里程碑 | 证据 |
| --- | --- |
| ANN 就绪 | 日志 `Identity profile ANN: rebuild succeeded centers=14152 generation=1 elapsed=53270ms`（20:55:53）；API `ready=true`、`last_build_status=success`、`snapshot_nodes=14152`、`rebuild_requested=false` |
| 合并自然巡检 | `last_run_at`：`2026-09-15T00:20:06+08:00` → **`2026-09-15T21:05:56.895611423+08:00`**；`cursor_target_id`：0 → **272318**；task 文案「完成 100 个目标人物巡检」；`processed_pairs=0` |
| background/logs | 3 行，含 ANN 重建完成与「完成 100 个目标人物巡检」 |

续验后 API 核对（与 UI 一致）：

| 接口 | 结果要点 |
| --- | --- |
| identity-profiles/stats | ANN ready；dirty≈1563；failed=3；coordinator 持续切片 |
| merge-suggestions/task | idle；「完成 100 个目标人物巡检」；`started_at`/`stopped_at`=21:05:56 |
| merge-suggestions/stats | pending=0；历史 total=20856 |
| merge-suggestions | pending 列表 total=0 |
| merge-suggestions/failures | total=0（无未解决技术失败） |
| background/logs | 见上 3 行 |

**零推荐解释（续验后，可成立）：**

1. 已发生真实部署后巡检：`last_run_at`、cursor、logs、task 文案均更新，不是“尚未巡检”。  
2. 本轮处理 100 个目标人物，`processed_pairs=0` → **正常无新候选对**，pending 保持 0 与 stats/列表一致。  
3. failures=0：本轮无技术失败入账；不是把失败伪装成空结果。  
4. `paused=false`，未清用户暂停、未手工灌推荐、未降阈值。  
5. dirty 画像仍约 1.5k 属历史积压，不要求本轮清空。

观测制品：`…/prod-acceptance/observe3-{start,end,polls}.json` 与 `observe3-relevant-logs.txt`。

---

## 7. 回退准备（未触发）

- 保留旧镜像标签 `relive:pre-identity-recovery-20260915-fe20c289`
- 保留切换前一致性备份
- 新失败表 / 恢复列迁移已写入；若需镜像回退须确认 schema 兼容，**禁止**用旧备份直接覆盖已有写入的生产库（除非用户明确确认数据损失）

本次无启动失败、无持续重启、未执行回退。

---

## 8. 剩余说明（非门禁阻塞）

线上验收门禁已通过。后续自然运行仍可继续消化 dirty 积压与后续巡检游标；无需为验收再干预。部署业务 SHA 保持 `6413beef…`；本文档更新为独立 docs 提交。

---

## 9. 产物索引

| 产物 | 路径/值 |
| --- | --- |
| 部署 SHA | `6413beef29807b5c5c06563fcca0a7a7fbb5a0f7` |
| 源码摘要 | `ba82b2cea28e07dbd92a0da9fa9b54600889ae34324f965d208fb182a7a83d14` |
| 应用镜像 | `relive:6413beef-identity-recovery-20260915` / `sha256:6995ec72d714…` |
| verify.bin | sha256 `8b487688ebb781ad…` |
| 副本报告 | `…/verify-work-20260915-6413beef/verify-report-6413beef.json` |
| 构建目录 | `/volume1/docker/relive-builds/6413beef-identity-recovery-20260915` |

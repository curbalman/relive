# Delivery: primary 画像恢复与推荐失败状态修复（返修后）

日期：2026-09-14（第二轮续修 2026-09-15；第三轮 2026-09-15；第四轮 2026-09-15；第五轮收尾 2026-09-15；**第七轮工具收尾 2026-09-15**）  
计划：`docs/plans/2026-09-14-identity-profile-recovery-and-suggestion-failures.md`  
验收反例：`docs/plans/2026-09-14-identity-profile-recovery-acceptance.md`  
返修说明：`docs/plans/2026-09-14-identity-profile-recovery-rework.md`  
第二轮验收：`docs/plans/2026-09-15-identity-profile-recovery-reacceptance.md`  
第三轮验收：`docs/plans/2026-09-15-identity-profile-recovery-acceptance-round3.md`  
第三轮修复：`docs/plans/2026-09-15-identity-profile-recovery-rework-round3.md`  
第四轮验收：`docs/plans/2026-09-15-identity-profile-recovery-acceptance-round4.md`  
第四轮修复：`docs/plans/2026-09-15-identity-profile-recovery-rework-round4.md`  
第五轮验收：`docs/plans/2026-09-15-identity-profile-recovery-acceptance-round5.md`  
第五轮修复：`docs/plans/2026-09-15-identity-profile-recovery-rework-round5.md`  
第六轮验收：`docs/plans/2026-09-15-identity-profile-recovery-acceptance-round6.md`  
第七轮验收：`docs/plans/2026-09-15-identity-profile-recovery-acceptance-round7.md`  
引擎版本：`identity-engine-v5`  
HEAD 基线：`fec4f46`（业务修改与收尾工具修复均在工作区，未单独提交）  
当前源码摘要：`ba82b2cea28e07dbd92a0da9fa9b54600889ae34324f965d208fb182a7a83d14`（`source_digest.py`，358 文件；**不得用 round5 的 `6770364c…` 冒充本轮**）

## 状态总览（门禁）

| 门禁 | 状态 | 说明 |
| --- | --- | --- |
| 本地代码完成（R1–R4 + S1–S5 + T1–T4） | **是** | 业务恢复链路保持第三轮通过状态，未修改 |
| T5 验证工具修复（F1–F5 + G1/G2/G3 + FullCLI） | **是** | 第七轮缺口已修：缺事件时间拒绝、真实运行窗口校验 ResolvedAt、ANN 重建夹具、FullCLI Success 默认执行；工具包全绿 |
| 正式反例与回归 `TestSeventhReview_*` + `TestSixthReview_*` + `TestFifthReview_*` + `TestAcceptance_*` + `TestReacceptance_*` + `TestThirdReview_*` | **是** | 全部通过（含 `-race`） |
| 迁移可幂等（recovery v1 + failures v1–v4；T1/T2/F1–F5 无新列） | **是** | 已复核 |
| 相关 Go 回归 + 关键路径 `-race` | **是** | 100% 通过 |
| 全量 `go test ./...`（CLT：`DEVELOPER_DIR=/Library/Developer/CommandLineTools`） | **部分** | 仅存量 `TestClusteringEquivalence`（本地库缺 `identity_retry_after`，与本次无关） |
| 前端 People vitest + peopleHelpers + `npm run build` | **是** | 本次无前端变更，契约保持一致 |
| 浏览器八态交互 | **是** | 见 §3.3；未改 UI 契约 |
| §9.10 资格隔离（不恢复 failed） | **是** | 隔离确认，failed 未泄漏 |
| 9/8 完整副本机制闭环 | **是（历史）** | 见 §3.2 |
| 9/14 完整快照独立副本（round3） | **是（历史）** | 见 §3.4 |
| 9/14 完整快照独立收尾副本（round4） | **是（历史）** | 见 §3.5 |
| 9/14 完整快照独立收尾副本（round5） | **是（历史）** | 见 §3.6；绑定旧摘要 `6770364c…`，**不能证明当前源码** |
| **当前源码正式测试证据 + 注入构建** | **是** | 见 §3.7；摘要 `ba82b2ce…` |
| **当前源码独立副本核验** | **待执行** | 本轮未访问 NAS；需用 §3.7 材料在新工作副本重跑 |
| 生产已部署 | （未执行） | 待授权 |
| 生产已验证 | （未执行） | 待授权 |

整体明确区分三项结论：
1. **工具修复完成（F1–F5 + 第七轮 G1/G2/G3）**：缺证据、缺事件时间、版本未绑定、ResolvedAt 落在运行窗外、矛盾/未完成数据均可靠拒绝；合法 waiting_evidence / unchanged_resolved / 窗内关闭仍可通过；FullCLI Success 在普通 `go test` 下真实执行且 ANN 就绪。
2. **副本验证**：Round 5 报告仍为历史权威材料（旧摘要）；**当前摘要的独立副本核验尚未执行**。
3. **生产操作**：生产部署与生产验证均未执行。

### 勾选清单（返修 §7.3 + 第二轮 §9 + 第三轮 §7 + 第四轮收尾 + 第五轮收尾 + 第七轮）

- [x] R1–R4 / S1–S5 / T1–T4 业务修复保持，通过正式回归与 `-race`
- [x] **F1** 证据解析严格化：移除硬编码测试列表与 64 字符 fallback，仅依赖真实测试与哈希核验；阻断 fail 掩盖
- [x] **F2** 源码唯一定义：统一定义 Go 源码、测试、`go.mod`、`go.sum`、内置资产单一权威摘要；测试前后 A/B 校验
- [x] **F3** 状态核对与下界约束：强制校验 FailuresBefore；算术等式闭环；样本计数下界约束；排除 `manual_retry_reset`；**ResolvedAt 对照真实 `[CollectedAt, RunEndedAt]`**
- [x] **F4** 恢复真实性判定：严格要求 264975/356935 usable + 有效中心，空画像样本严格要求 no_usable_evidence + 0 中心
- [x] **F5** 验收工具全套单测全绿；含第五/六/七轮反例；CLI 成功与坏证据路径均实际执行
- [x] **G1** 缺事件 `Time` 的必需执行/完成证据 → blocked（`TestSeventhReview_MissingEventTimeMustNotPass`）
- [x] **G2** 运行窗外 / 缺运行窗口的新关闭 → fail；窗内合法关闭与 unchanged_resolved 保留
- [x] **G3** 采集脚本透传非零 `go test` 退出码（上轮已实测）
- [x] 当前源码摘要 A=B、正式测试证据与注入构建产物（§3.7）
- [ ] 在 `2026-09-14` 干净副本上用**当前**摘要重新运行工具（新 NAS 工作目录，不覆盖 round5）
- [x] 交付说明区分历史 round5 与当前摘要材料

## T1–T5 对照表（第三轮）

| ID | 根因 | 最终修改 | 正式测试 | 实测 |
| --- | --- | --- | --- | --- |
| T1 | ManualRetry 与 pending 安排拆提交，失败留下 `ManualRetriedAt!=nil && pending=false` → 假 already_scheduled | `BeginManualRetryArrangement` 同事务预算+pending+remaining | `TestThirdReview_ManualCheckpointFailureCanResume` | PASS |
| T2 | MarkDirty 与 remaining 更新非原子 → 重试重复重置 revision/恢复预算 | `commitManualArrangePerson` 同事务 MarkDirty+断点；提交后仅 ANN 失效；真正并发 before-tx barrier | `TestThirdReview_ManualProgressFailureDoesNotRepeatDirty`、`TestThirdReview_ManualProgressConcurrentSingleCommit`；真实 SQLite revision 断言 | PASS |
| T3 | `saveStateLocked` 错误被吞，未落盘 key 仍写失败表 | `ensureFailureAttemptKeyLocked` 持久化失败回滚内存；切片最终进度保存失败回滚 key 轮换 | `TestThirdReview_AttemptKeySaveFailureMustStopLedgerWrite`（含恢复/重启/耗尽） | PASS |
| T4 | `lookupProfileGenerations` 读失败当空 map，推进游标并清 Pending | 显式返回 error；失败不推进/不消费/不完成；后台切片补做 | `TestThirdReview_ProfileReadFailureKeepsReconcilePending` | PASS |
| T5 | `report.OK = elig.FailedExcludedOK` | 六项必需断言集合；任一非 pass → `ok=false` 且退出码非 0 | `TestEvaluateAssertions_*`；round3 副本报告 | PASS |

### T1/T2 事务边界（现行）

1. **初始事务**：`BeginManualRetryArrangement` = 人工预算重开 + pending + 完整 remaining；失败整体回滚。
2. **逐项事务**：画像 MarkDirty（含 revision/恢复预算重置）与 remaining 移除同提交；中途故障两者回滚。
3. **完成判定**：remaining 清空才 accepted；并发重读事务内状态；同请求共享依赖只 dirty 一次。
4. **WriteQueue**：事务内只用 tx 作用域 repo，不嵌套 `executeWrite`/不跑 builder/ANN；提交后 `InvalidateANNOnly` 可补偿。

### T3 尝试身份

- 分配候选 key → 落盘成功才保留内存身份；失败回滚，禁止冒充已保存。
- 失败行提交后最终进度（含 key 轮换）保存失败：回滚轮换，同 key 重放不双计。

### T4 读取失败

- 查询成功空结果 ≠ DB 错误；错误保留 `ReconcilePending`，不推进 `ReconcileAfterID`。
- `NotifyProfileEvidenceReady` 读失败时请求核对待办，不写 gen=0「已知不可用」。

### T5 工具断言

必需项：`eligibility` / `recovery` / `startup_wiring` / `persistence` / `final_state` / `fault_recovery`。  
强制 due、`BuildPersonOnce` 仍为专项加速，报告另列，不单独决定整体 OK。

## S1–S5 对照表（第二轮）

| ID | 根因 | 最终修改位置 | 正式测试 | 实测 |
| --- | --- | --- | --- | --- |
| S1 | 引擎注入前无效 Reconcile；未就绪当成功空结果 | `AttachIdentityMatchingWiring`；`service.go` 生产路径；`RunBackgroundSlice`/`NotifyIndexReady` 补做 Pending | `TestReacceptance_StartupWiringReconcilesAfterAttach` | PASS |
| S2 | gen=0 永久跳过；未知 vs 已确认不可用混为一谈 | `target_obs_known`/`target_consumed_gen` + deps JSON；`failureObsShouldWake`；迁移 v3 | `TestReacceptance_ReconcileFirstRecoveryFromZeroGeneration` | PASS |
| S3 | UI `ListUnresolved` 钳 100，内部核对只看第一页 | `ListUnresolvedKeyset` + `ReconcileAfterID`/`ReconcileRoundMaxID`；批成功才推进 | `TestReacceptance_ReconcileMustVisitBeyondFirst100`、`TestReacceptance_ReconcileContinuesAcrossBackgroundSlices` | PASS |
| S4 | 批内部分提交重放双计 attempts | `last_attempt_key`；state `FailureAttemptKey`/`Seq`；`attemptKeyIsStale`；迁移 v4 | `TestReacceptance_PartialFailureBatchReplayDoesNotBurnBudget`、`NewAttemptKeysIncrementToExhaustion`、`FailureAttemptKeySurvivesRestartReplay`、`StaleAttemptKeyDoesNotOverrideNewerRound` | PASS |
| S5 | 依赖读失败仍 accepted；安排不可补齐 | 先完整读/解析；`manual_arrange_pending` + remaining JSON；逐个 MarkDirty 断点 | `TestReacceptance_ManualRetryDependencyReadErrorMustPropagate`、`MidDependencyFailureThenResume`、`ConcurrentAlreadyScheduled`、`ArrangementSaveFailurePropagates`；前端 error 路径 | PASS |

### S2 观测语义（现行）

| 已记录事实 | 当前事实 | 行为 |
| --- | --- | --- |
| 未知（`!obsKnown`） | ready 且 gen>0 | 首次唤醒 + 重验；写 known+consumed（审查接受「顺带重验」） |
| 已知可用版本 | gen 前进且可恢复 | 唤醒；更新 consumed |
| 同代次已消费 | 重复回调/重启 | 不赠预算 |
| 任意 | 读失败 / 非 ready / 模型失配 | 不消费、不写成可用 |

### S4 attempt key

- 格式：`dg-{DirtyGeneration}-seq-{Seq}`；**第三轮**：写失败行前必须落盘成功，失败则回滚内存身份；成功切片后 `rotateFailureAttemptKey` 与 cursor 同存，保存失败回滚轮换。
- 同 key Upsert 不增 attempts、不推迟 next。
- DirtyGeneration 并发推进时 key 前缀变化等同新 key；注释写明「少计不多计」。
- 非 `dg-N-seq-M` 格式不做 stale 拦截（保守：宁可多计一次）。

### S5 人工安排

- 读/解析失败直接 return error，不 continue 成假 accepted。
- **第三轮**：`BeginManualRetryArrangement` + 逐项事务 MarkDirty；`manual_arrange_pending` + remaining 可续跑；已 dirty 的 ID 不重复 MarkDirty。
- `already_scheduled` 仅当安排完成且 attempts=0（不再出现预算已开但 pending=false 的半完成态）。

## R1–R4 对照表

| ID | 根因 | 修改位置 | 正式测试 | 实测 |
| --- | --- | --- | --- | --- |
| R1 | `applyFailureIntents` 错误被吞；Resolve/调度失败伪装完成 | `person_merge_suggestion_service.go` | `TestAcceptance_FailureWriteMustPropagate`、`TestAcceptance_ResolveFailureRestoresPending`、`TestAcceptance_MidBatchReplaceKeepsFailureAndCursor`（B1） | PASS |
| R2 | 索引恢复不唤醒 exhausted；notify 早于 ANN 激活；裸读 active_generation 误唤醒 | failure repo Wake/Reconcile；coordinator 顺序；`lookupProfileGenerations` ready 过滤（B2，见下） | `TestAcceptance_IndexRecoveryWakesExhausted`、`TestAcceptance_ReconcileSkipsIneligibleFailedCandidate`、`TestAcceptance_EmptyReadyGenStampsAndWakesWaitingEvidence` | PASS |
| R3 | 人工重试每次推迟 next_retry_at | `ManualRetry` → `already_scheduled`；MarkDirty 失败清标记 | `TestAcceptance_ManualRetryIsIdempotent` | PASS |
| R4 | 未分类 ready 空画像无接入 | `AdmitUnclassifiedEmptyProfiles` + coordinator admit | `TestAcceptance_LegacyEmptyProfileGetsBoundedVerification` | PASS |

### B2 口径修正（本轮副本打出）

审查要求「唤醒前必须可读取」——`failed` 残留更高 `active_generation` 不得唤醒，这点保持：`status=ready` 过滤即可。

但若再要求「必须有活动中心」，会把 `no_usable_evidence` 空画像永远戳记 `TargetProfileGen=0`，导致：
1. 失败表代次盖不上去；
2. 每次证据通知 `obs>0` 都可能误唤醒；
3. 空画像自身代次前进时无法唤醒 `waiting_evidence`。

**现行口径**：`lookupProfileGenerations` = ready + 模型匹配 + 未隐藏 + `active_generation>0`（**不**要求中心）。可匹配召回仍由 ANN/引擎 eligible 中心查询负责；唤醒只安排重验。

## 1. 返修后行为摘要

### 失败提交协议（R1）
- 失败表写入错误向上返回，不得推进 cursor。
- Resolve 失败恢复 `pendingFailureResolves` 供重放。
- 批内真实 `ReplacePendingForTarget` 中途失败：已提交目标 pending 可变，失败表不 Resolve、cursor 不前进。
- S4：同一 `last_attempt_key` 重放不双计；真正下一轮计算轮换 key。

### 唤醒与核对（R2 + S1/S2/S3）
- 先 `applyAnnMerge`，再 `notifyEvidenceReady`。
- 生产装配：`AttachIdentityMatchingWiring` = SetEngine → `RequestUnresolvedDependencyReconcile`；引擎/模型未就绪保留 Pending。
- 证据唤醒：`failureObsShouldWake`（未知首次 / 版本前进）；画像须 **ready**。
- 索引：`NotifyIndexReady` → `Request(true)`；整轮核对末尾 `WakeForIndexReady`。
- 内部核对：keyset 游标、每批≤100、轮次固定 `ReconcileRoundMaxID`；查询失败不越批。
- 全库巡检进行中不强切 `RetryOnly`。

### 人工重试（R3 + S5）
- 先完整 GetByTarget + 解析 deps，失败直接 error。
- 安排断点：`manual_arrange_pending` + remaining JSON；逐个 MarkDirty。
- `already_scheduled`：安排完成且 `attempts==0`。
- `manual_merge_failure_retry` 重置画像 recovery 预算。

### 历史空画像（R4）
- 通用条件：ready + `build_outcome=''` + 无活动中心 + 未隐藏 + 有脸 → 有界 `MarkDirty(legacy_empty_verify)`。
- **不再要求**部署后对 306601/307945/562545 手工 SQL；三 ID 仅作副本对照样本。
- 副本验收工具在通用队列很长时用 `BuildPersonOnce`（同协调器 buildOne→ReplaceGeneration→notify），不手写 outcome；列为专项加速，不冒充自动闭环。

## 2. 迁移清单

| Key | 内容 |
| --- | --- |
| `migration.identity_profile_recovery_v1` | 画像恢复字段 + build_outcome / evidence_version / change_revision |
| `migration.merge_suggestion_failures_v1` | 失败表 + 旧 retry_targets 导入 |
| `migration.merge_suggestion_failures_v2` | manual_retried_at / manual_retry_reason |
| `migration.merge_suggestion_failures_v3` | `target_obs_known` / `target_consumed_gen`；gen>0 回填 known+consumed；gen=0 保持未知 |
| `migration.merge_suggestion_failures_v4` | `last_attempt_key` + `manual_arrange_pending` / `manual_arrange_remaining_json` |

**v3 列名注意**：GORM 字段 `TargetProfileGen` → DB 列 `target_profile_gen`（不是 JSON 名 `target_profile_generation`）。backfill SQL 必须用蛇形列名；曾用错列名会导致 AutoMigrate 全链路失败（已修，见 `TestAutoMigrateAddsMergeSuggestionFailureObsAndAttemptColumns`）。

回退：保留失败表与人工历史；旧镜像可能再装入 failed 中心——回退验收必须查资格与 stale，勿整库覆盖。

## 3. 副本与备份

### 3.1 当前完整快照闭环（2026-09-14）——**通过**

- 备份标签：`identity-recovery-20260914`
- NAS 路径：`/volume1/docker/relive/backup/2026-09-14-180506-identity-recovery-20260914/`（04:56–04:59 完成；`sha256sum -c SHA256SUMS` 全 OK；relive.db 2.4G）
- 基线：failed=24 / ready=58180 / dirty=1532（全属隐藏人物积压）；faces embedding 非空 273654
- 工作副本：NAS `/volume1/docker/relive/backup/verify-work-20260915/relive.work.db`（从上述备份 `cp`，不碰生产）
- 工具：同步本轮源码后交叉编译 `verify.bin`，在 `golang:1.25` 容器内执行（NAS 宿主 glibc 过旧无法直接跑）
- **权威终态报告**：`/tmp/relive-recovery-20260914/verify-report-final.json`（同源 NAS `verify-work-20260915/verify-report-20260914.json`）
- stderr 关键行：`wire: ann_ready=true ... path=AttachIdentityMatchingWiring`

| 断言 | 结果 |
| --- | --- |
| 资格隔离（恢复前） | `failed_in_eligible=[]`；eligible centers=14102；`failed_excluded_ok=true` |
| Attach 装配 | `ann_ready_at_attach=true`；`post_recovery_ann_centers=14152`；路径=**AttachIdentityMatchingWiring**（非诊断 ANN） |
| failed 恢复 | **264975** → ready/usable gen=3 centers=38；**356935** → ready/usable gen=1 centers=5；此前未进入失败账本，如实记录为 never_failed（present=false，不虚构已关闭） |
| 空画像核验 | **306601/307945/562545** → `build_outcome=no_usable_evidence`（真实 builder；验收用 `BuildPersonOnce` 加速同路径，专项另列） |
| 推荐闭环 | 失败表 **waiting_evidence / no_usable_evidence** ×3；attempts=0；`unresolved_target_count=3`；pending 可为 0 |
| 报告 `ok` | **true**（≈30min：profile 80 slices + merge 5 slices idle） |

**专项：S1/S2 重启核对烟雾（同库二次跑，非权威终态）**  
将 306601 人为改成 `exhausted` + `obs_known=0` + gen=0 后，以 `max-slices=0` 再走 Attach：行被唤醒为 `retry_wait`，`obs_known=1`、`consumed_gen=2`、`target_profile_gen=2`。说明未知观测在生产 Attach 入口可识别首次恢复。该次实验 `ann_ready=false`（未跑画像切片重建 ANN），会额外产生 `index_unavailable` 类重试噪声——**以首轮权威报告为准，不把二次实验库当作终态。**

`nas-backup-watch` 已结束（exit 0 / DONE）；备份完成，无需继续监视。

### 3.4 第三轮独立副本（T1–T5 修复后）——**通过**

- 快照来源：同 §3.1 备份 `2026-09-14-180506-identity-recovery-20260914`（`sha256=27e8e774…a5f1925`）
- **新工作副本**（不覆盖旧 `verify-work-20260915`）：NAS `/volume1/docker/relive/backup/verify-work-20260915-round3/`
- 代码：`fec4f46` 工作区未提交实现；`verify.bin` sha256=`a07bfe4d…f2b2907`
- 参数：`max_slices=80 merge_slices=40`；路径=`AttachIdentityMatchingWiring`；专项：`force_due` + 样本 `BuildPersonOnce`
- 运行：`golang:1.25` 容器（`--network host`）；约 23min（10:10–10:34 UTC+8）
- **权威 round3 报告**：
  - NAS：`verify-work-20260915-round3/verify-report-round3.json`
  - 本地：`/tmp/relive-recovery-20260915-round3/verify-report-round3.json`（及 stderr / run-meta / final-state）
- 旧报告与烟雾实验保留在 `verify-work-20260915/` 与 `/tmp/relive-recovery-20260914/`，未覆盖

| 断言 | 结果 |
| --- | --- |
| eligibility | pass（centers=14102，failed 未泄漏） |
| recovery | pass（profile 80 slices；样本 264975/356935 ready/usable；空画像 built=3） |
| startup_wiring | pass（`ann_ready=true`，centers=14152，Attach 路径） |
| persistence | pass（失败表/merge 切片有证据；waiting_evidence×3；264975/356935 未曾入表如实为 never_failed） |
| final_state | pass（unresolved=3 全为 waiting_evidence） |
| fault_recovery | pass（正式 `TestThirdReview_*` 覆盖） |
| 报告 `ok` / 退出码 | **true** / 0 |

终态样本：264975/356935 → ready usable（centers 38/5）；306601/307945/562545 → `no_usable_evidence` + `waiting_evidence` attempts=0。

### 3.5 第四轮收尾独立副本（V1–V3 工具修复后）——**通过**

- 快照来源：沿用一致性备份 `2026-09-14-180506-identity-recovery-20260914`（`sha256=27e8e774544e4e17d69c5f8f9a16f94477ca1ace5310796341268a7aaa5f1925`，只读无损）
- **全新独立工作副本**（保留旧 `verify-work-20260915` 与 `verify-work-20260915-round3`）：
  - NAS 路径：`/volume1/docker/relive/backup/verify-work-20260915-round4/`
  - 副本数据库：`relive.work.db`（初态 sha256=`27e8e774…`；终态 sha256=`a6e5feba1a0962b855f775c29029e3edca7929ceb725477caf17c974a18455a7`）
- 源码摘要与构建标识：
  - 代码：工作区实现（基于 `fec4f46`，含 V1–V3 严格断言与前态核验）
  - 源码摘要：`48eeeca53cc2ff75c86357687d31155ceba25420a05bdd27902fe94f0044dc77`
  - 编译产物：`verify.bin`（注入 `buildSourceDigest`，sha256=`a7cfbb24bda0838af5cb6ae7bfd1446dbe8862ff05022b535f42be03b57bd425`）
- 运行环境与参数：
  - 容器环境：NAS `golang:1.25`（`--network host`）
  - 命令：`/work/verify.bin -db /work/relive.work.db -max-slices 80 -merge-slices 40 -test-evidence /work/test-evidence.json -out /work/verify-report-round4.json`
  - 运行时间：2026-09-15 04:30:09 – 04:53:21 UTC（耗时 23m12s）
  - 真实进程退出码：**0**
- 权威报告位置：
  - NAS：`/volume1/docker/relive/backup/verify-work-20260915-round4/verify-report-round4.json`（sha256=`66a6a7a96937bd322335b621c500716d38fae62d2ef8186ebf50fef5e232086f`）
  - 本地快照：`/tmp/relive-recovery-20260915-round4/verify-report-round4.json`（及 `verify-stderr.log`、`run-meta.txt`、`ROUND4_SHA256.txt`）

#### 六项必需检查实质结果与判定依据

| 检查项 | 状态 | 观察值与判断依据 |
| --- | --- | --- |
| `eligibility` | **pass** | 恢复前核验：failed 样本在 `ListAllActiveCenters` 中泄漏为 0；eligible centers=14,102，eligible persons=6,243；`failed_excluded_ok=true` |
| `recovery` | **pass** | 5 个必需样本逐一核验完整存在：<br>• 264975：ready/usable, centers=38<br>• 356935：ready/usable, centers=5<br>• 306601/307945/562545：ready/`no_usable_evidence`, centers=0；<br>切片执行 80 轮，样本空构建 3 个，无 dirty/failed 样本误判通过 |
| `startup_wiring` | **pass** | `ann_ready_at_attach=true`；活动中心数 14,152；完全通过生产装配路径 `AttachIdentityMatchingWiring` 注入，未手工篡改或调诊断 ANN |
| `persistence` | **pass** | 真实对比运行前后状态：<br>• 264975/356935：运行前无失败行、运行后亦无失败行，如实记录为 `never_failed`（不虚构关闭）；<br>• 306601/307945/562545：真实持久化为 `waiting_evidence`，attempts=0，未虚耗预算；<br>• merge 切片完成 5 轮，未解决数稳定为 3 |
| `final_state` | **pass** | 先行验证统计算术一致性全部通过：<br>• `unresolved(3) = retry(0) + exhausted(0) + waiting(3)`；<br>• `retry_total(0) = retry_due(0) + retry_deferred(0)`；<br>• `StateCounts` 与全库 counts 完全吻合；<br>• 零到期（`retry_due=0`），零待验（`retry_deferred=0`），全库未解决全部为合法 `waiting_evidence`（attempts=0）；`pending_suggestions_after=0` |
| `fault_recovery` | **pass** | 通过 `-test-evidence /work/test-evidence.json` 读取正式测试证据：<br>• 真实退出码 0；<br>• 必需测试清单全部实际执行并通过：`TestThirdReview_ManualCheckpointFailureCanResume`、`TestThirdReview_ManualProgressFailureDoesNotRepeatDirty`、`TestThirdReview_ManualProgressConcurrentSingleCommit`、`TestThirdReview_AttemptKeySaveFailureMustStopLedgerWrite`、`TestThirdReview_ProfileReadFailureKeepsReconcilePending`；<br>• 源码摘要 `48eeeca53cc2…` 与构建二进制完全匹配（`digest_match=true`） |
| **整体 OK / 退出码** | **true / 0** | **全部 6 项必需检查实质 pass，无 skip，无绕过** |

### 3.6 第五轮收尾独立副本（F1–F5 严格工具修复后）——**历史通过（绑定摘要 `6770364c…`）**

> 以下材料证明当时源码版本的副本核验；**不能替代 §3.7 当前摘要的副本重跑**。

- 快照来源：沿用权威只读备份 `2026-09-14-180506-identity-recovery-20260914/relive.db`（初态 sha256=`27e8e774544e4e17d69c5f8f9a16f94477ca1ace5310796341268a7aaa5f1925`）
- **全新独立工作副本**（完全保留旧 `verify-work-20260915`、`verify-work-20260915-round3` 与 `verify-work-20260915-round4`）：
  - NAS 路径：`/volume1/docker/relive/backup/verify-work-20260915-round5/`
  - 副本数据库：`relive.work.db`（初态 sha256=`27e8e774…`；终态 sha256=`da15aeaff304dd5c5b9ade9ee47840b1b73f1ed0842ee044c34f6022f45bfc1c`）
- 源码摘要与构建标识：
  - 代码：工作区实现（基于 `fec4f46`，含 F1–F5 证据严格解析、源码唯一摘要、下界约束、可用中心核验及工具全覆盖单测）
  - 源码摘要：`6770364c8362f63385e1d9a0e4c0ebab7234898e16591b1f2d0d77ad326ad269`（通过 `source_digest.py` 覆盖 358 个文件权威哈希）
  - 测试证据前置记录：`record_test_evidence.sh` 运行 5 个必需正式测试，生成 `test-evidence.json`（测试前态与后态源码摘要完全一致，均为 `6770364c8362…`；raw output sha256=`1b1ba6cab1a4fc49009e4dfaca4d790113e59df1b9bef9ef5482e71d70ff8c7e`）
  - 编译产物：`verify.bin`（注入 `buildSourceDigest=6770364c8362…`，sha256=`063711646f41637c0e92d77382dc3f66736194c41985521c53f7e2e409ae6e57`）
- 运行环境与参数：
  - 容器环境：NAS `golang:1.25`（`--network host`）
  - 执行命令：`/work/verify.bin -db /work/relive.work.db -max-slices 80 -merge-slices 40 -test-evidence /work/test-evidence.json -out /work/verify-report-round5.json`
  - 运行时间：2026-09-15 05:47:23 – 06:18:17 UTC（耗时 30m54s）
  - 真实进程退出码：**0**
- 权威报告位置：
  - NAS：`/volume1/docker/relive/backup/verify-work-20260915-round5/verify-report-round5.json`（sha256=`240aba9326dde9371fb4145a3f607205aedae71af369b4169831d915116a55cc`）
  - 本地快照：`/tmp/relive-recovery-20260915-round5/verify-report-round5.json`（及 `verify-stderr.log`、`run-meta.txt`、`ROUND5_SHA256.txt`）

#### 六项必需检查实质结果与判定依据

| 检查项 | 状态 | 观察值与判断依据 |
| --- | --- | --- |
| `eligibility` | **pass** | 恢复前核验：failed 样本在 `ListAllActiveCenters` 中泄漏为 0；eligible centers=14,102，eligible persons=6,243；`failed_excluded_ok=true` |
| `recovery` | **pass** | 5 个必需样本逐一核验完整存在：<br>• 264975：ready/usable, centers=38, eligible_centers=38<br>• 356935：ready/usable, centers=5, eligible_centers=5<br>• 306601/307945/562545：ready/`no_usable_evidence`, centers=0, eligible_centers=0；<br>切片执行 80 轮，样本空构建 3 个，严格遵循 F4 规则（usable 必须有有效中心，空画像必须为 no_usable_evidence 且 0 中心） |
| `startup_wiring` | **pass** | `ann_ready_at_attach=true`；活动中心数 14,152；完全通过生产装配路径 `AttachIdentityMatchingWiring` 注入，未手工篡改或调诊断 ANN |
| `persistence` | **pass** | 真实对比运行前后状态：<br>• 264975/356935：运行前无失败行、运行后亦无失败行，如实记录为 `never_failed`（不虚构关闭）；<br>• 306601/307945/562545：真实持久化为 `waiting_evidence`，attempts=0，未虚耗预算；<br>• 失败表前态 `FailuresBefore` 非空；merge 切片完成 5 轮，未解决数稳定为 3 |
| `final_state` | **pass** | 统计算术等式与下界约束全部闭环：<br>• `unresolved(3) = retry(0) + exhausted(0) + waiting(3)`；<br>• `retry_total(0) = retry_due(0) + retry_deferred(0)`；<br>• `StateCounts` 与全库 counts 完全吻合；<br>• 样本计数未超全局总数（F3 下界约束）；<br>• 零到期（`retry_due=0`），零待验（`retry_deferred=0`），全库未解决全部为合法 `waiting_evidence`（attempts=0）；`pending_suggestions_after=0` |
| `fault_recovery` | **pass** | 通过 `-test-evidence /work/test-evidence.json` 读取正式测试证据（遵循 F1/F2 严格解析）：<br>• 真实退出码 0；<br>• 逐行结构化 JSON 解析，检查 `github.com/davidhoo/relive/internal/service` 包测试完成事件，无 fail 被掩盖；<br>• 必需测试清单全部实际执行并通过：`TestThirdReview_ManualCheckpointFailureCanResume`、`TestThirdReview_ManualProgressFailureDoesNotRepeatDirty`、`TestThirdReview_ManualProgressConcurrentSingleCommit`、`TestThirdReview_AttemptKeySaveFailureMustStopLedgerWrite`、`TestThirdReview_ProfileReadFailureKeepsReconcilePending`；<br>• 源码唯一摘要 `6770364c8362…` 与构建二进制完全匹配（`digest_match=true`），测试前/测试后代码未发生任何变动 |
| **整体 OK / 退出码** | **true / 0** | **全部 6 项必需检查实质 pass，无 skip，无伪造，无绕过** |

### 3.7 第七轮工具收尾材料（当前源码 `ba82b2ce…`）——**测试/构建已就绪；副本待跑**

对照：[第七轮验收](2026-09-15-identity-profile-recovery-acceptance-round7.md)。本轮修完 G1/G2 与 FullCLI Success，并重新绑定源码摘要；**未再跑长时间 NAS 副本**。

#### 工具判定修复

| 缺口 | 实现 | 正式测试 |
| --- | --- | --- |
| G1：缺事件时间跳过窗口校验 | `test_evidence.go`：`Package != ""` 且 `Time.IsZero()` → loader error / blocked | `TestSeventhReview_MissingEventTimeMustNotPass` |
| G2：`CollectedAt±2h` / `time.Now()` 伪窗口 | `assertions.go` + `main.go`：新关闭核对 `[CollectedAt, RunEndedAt]`；缺窗口 → fail | `TestSeventhReview_ResolvedBeforeRunMustNotPass`、`…MissingRunWindow…`、`…ResolveInsideRunWindow…` |
| FullCLI Success 夹具 ANN 未就绪 | `RebuildANNForVerify` + `collectorBuildProfileANN`；测试内注入摘要使 Success 默认执行 | `TestRun_FullCLI_Success`（普通 `go test`，exit 0，`ann_ready_at_attach=true`） |

#### 正式测试证据与构建（本地）

目录：`/tmp/relive-recovery-20260915-round7/`

| 材料 | 值 |
| --- | --- |
| 源码摘要 A=B | `ba82b2cea28e07dbd92a0da9fa9b54600889ae34324f965d208fb182a7a83d14` |
| 输入清单 | `source-files.json`（358 文件） |
| 正式测试命令 | `go test -json -race ./internal/service ./internal/repository -run 'TestAcceptance_\|TestReacceptance_\|TestThirdReview_' -count=1` |
| 起止时间 | `2026-09-15T07:43:09Z` – `2026-09-15T07:43:12Z` |
| 进程退出码 | **0** |
| raw sha256 | `d793d7ce79575a492a5be45822283a880098273be953bae30ee850c8bc6f79bf` |
| 证据 manifest | `test-evidence.json` |
| 注入构建 | `verify.bin`（`-X …buildSourceDigest=ba82b2ce…`） |
| 二进制 sha256 | `fb686ba271e07d3ab5f4390fad8b749177b8bddc9f778a0cfb70cd329554f67e` |
| 工具包单测 | `go test ./cmd/relive-identity-recovery-verify -count=1` → PASS |
| 业务 `-race` | `TestAcceptance_\|TestReacceptance_\|TestThirdReview_` → PASS |

#### 待补：独立副本核验

建议 NAS 工作目录：`/volume1/docker/relive/backup/verify-work-20260915-round7/`（或更新后缀；**勿覆盖** round5）。  
沿用只读备份 `2026-09-14-180506-identity-recovery-20260914`，拷入 §3.7 的 `verify.bin` + `test-evidence.json`（及 raw/jsonl），执行：

```sh
/work/verify.bin -db /work/relive.work.db -max-slices 80 -merge-slices 40 \
  -test-evidence /work/test-evidence.json -out /work/verify-report-round7.json
```

通过后把报告/退出码/起止时间回填本节，并将门禁「当前源码独立副本核验」改为是。

### 3.2 9/8 完整副本机制闭环（历史证据）

- 快照：**2026-09-08**；权威报告 `/tmp/relive-recovery-20260908/verify-report-final.json`
- 限定：旧路径证据；**当前门禁以 §3.1 的 9/14 + Attach 路径为准**

### 3.2.1 verify CLI 生产等价路径

1. 画像协调器切片恢复 / 空画像接入（hooks → merge）
2. `BuildIdentityMatchingEngineFromProfileService`：复用画像服务 ANN
3. `AttachIdentityMatchingWiring(engine, annReady)`
4. merge `RunBackgroundSlice` 消费重验

`BuildPersonOnce` / 强制 due 仅专项加速，报告另列。

### 3.3 浏览器交互（已完成）

环境：轻量 UI 库 `/tmp/relive-ui-verify/relive.db` + backend `:18080` + frontend `:5173`。摘要：`/tmp/relive-ui-verify/browser/SUMMARY.json`。

| 场景 | 结果 |
| --- | --- |
| 首屏不拉 failures | PASS |
| paused + partial | PASS（已暂停（有未解决问题）） |
| idle + exhausted 计数 | PASS（部分完成 + 重试已耗尽） |
| waiting_evidence | PASS（文案 + 按钮 disabled；API `skipped.reason=waiting_evidence`） |
| 详情分页 | PASS（20→25） |
| 成功人工重试 | PASS（已安排有界重验） |
| 重复人工重试 | PASS（已有人工重试在队列中，未重复安排） |
| RetryFailures API 抛错 | PASS（vitest：`ElMessage.error`，不走 success） |
| 正常零未解决 | PASS（失败面板消失；专项空文案仍依赖 current_message，与 helpers 契约一致） |

**浏览器中发现的产品 bug（已修）**：`@click="loadMergeSuggestionFailures"` 把 MouseEvent 当作 `silent`，导致请求发出但面板永不展开。已改为 `loadMergeSuggestionFailures()`，并用 `silent === true` 防御。

## 4. 操作策略（面向用户）

- **waiting_evidence**：输入未变时人工重试跳过；证据/代次变化自动唤醒。
- **exhausted**：巡检/重启不赠预算；人工重试可重开并记审计。
- **ready 空画像**：协调器有界接入 `legacy_empty_verify`，核验后写 `build_outcome`；推荐侧转 `waiting_evidence`，不烧技术 attempts。
- **人物后台** `/people/background/start` 非小批 dry-run；本交付不默认启动。

## 5. 部署与验收（待授权）

1. 保存旧镜像/配置及**完成态**一致性 `.backup`（`PRAGMA quick_check`）；勿用当日 partial；或明确沿用已校验历史完整备份并标注日期。
2. 部署含迁移产物（含 v3/v4）→ `/app/relive -version` + primary + `identity-engine-v5`。
3. 观察 ANN 中心数（不应再含 failed 污染中心）、有界画像恢复、失败表与 UI 计数。
4. 核验空画像：经通用接入后 `build_outcome=no_usable_evidence`，推荐 `waiting_evidence`（样本 ID 仅对照）。
5. 回退：保留失败/人工历史；查资格与 stale；勿整库覆盖。

## 6. 已知技术债 / 残余限制

1. `evidence_version` 偏弱；长期用脸 ID 集合哈希。
2. `RetryFailures` 对 BlockingDeps 存在 N+1（N≤100）。
3. 前端空结果文案依赖 `current_message` 匹配。
4. `WakeForEvidenceChange` 全表扫未解决行（分页核对下可能提前唤醒后续批行；语义正确、影响良性）。
5. ~~当前完整副本门禁仍受阻~~ → Round 5 曾以摘要 `6770364c…` 闭环（§3.6，历史）；**当前摘要 `ba82b2ce…` 的独立副本核验见 §3.7，仍待 NAS 重跑**。
6. 无公开 HTTP MarkDirty；运维依赖协调器自动接入或一次性 repo 调用（禁止完整 NewService 启动清理）。
7. ~~浏览器真人交互未跑~~ → 已补；展开详情 click 绑定已修。
8. `TestClusteringEquivalence` 缺 `identity_retry_after`（既有环境失败，单列）。
9. NAS 宿主 glibc 过旧，verify 二进制需在 `golang:1.25` 容器内跑；不影响生产 `relive:local` 镜像。
10. 二次重启烟雾若 `max-slices=0` 导致 `ann_ready=false`，会写出 `index_unavailable` 噪声——勿覆盖首轮权威终态。

## 7. 主要改动文件（累计）

画像：`person_identity_profile*.go`、`identity_profile_coordinator.go`、`identity_matching_*.go`、`database.go`  
失败：`person_merge_suggestion_failure*.go`、`person_merge_suggestion_service.go`、`person_merge_suggestion_acceptance_test.go`、`person_merge_suggestion_reacceptance_test.go`  
注入：`service.go`（`AttachIdentityMatchingWiring`）  
API/前端：`dto.go`、`people_handler.go`、`router.go`、`frontend/.../people*`、`index.spec.ts`  
诊断：`identity_merge_diagnose.go`（`BuildIdentityMatchingEngineFromProfileService`）、`cmd/relive-merge-suggestion-diagnose`、`cmd/relive-identity-recovery-verify`（含 `assertions.go`）  
本文件：delivery（S1–S5 + **T1–T5** + 迁移 v3/v4 + round3 副本）

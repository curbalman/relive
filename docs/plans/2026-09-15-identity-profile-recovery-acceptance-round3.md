# Buddy 画像恢复续修：第三轮验收

日期：2026-09-15  
结论：**暂不通过。上轮直接反例及正常流程已有修复，持久化故障后的恢复仍有缺口。**  
验收对象：基于 `fec4f46` 的当前未提交工作区。对照 [第二轮续修任务](2026-09-15-identity-profile-recovery-rework-round2.md) 及更新后的 [交付文档](2026-09-14-identity-profile-recovery-and-suggestion-failures-delivery.md)。

## 1. 已确认的进展

- 生产启动改用 `AttachIdentityMatchingWiring`，先注入引擎再请求核对。
- 已加入首次恢复的观测信息、内部 keyset 分页和持久化核对游标。
- 已加入 `last_attempt_key` 及人工安排的 pending/remaining 字段和迁移。
- 现有 `TestAcceptance_*`、`TestReacceptance_*` 及针对性 race 均通过。
- 当前 2026-09-14 完整快照的本地报告已经提供，本轮不再沿用“当前快照报告缺失”的旧结论。

以下发现集中在上轮明确要求的写入失败、补齐、重放与读取失败窗口，并非新增业务范围。

## 2. 继续阻塞验收的问题

### T1 / P1：首次安排断点写入失败，再次请求被误判为 already_scheduled

位置：`backend/internal/service/person_merge_suggestion_service.go:1173`、`1210–1211`、`1150–1152`；repository 的 ManualRetry 在 `person_merge_suggestion_failure_repo.go:598–607`。

执行顺序是先提交 ManualRetry（attempts=0、ManualRetriedAt 非空），再保存 ManualArrangePending 和 remaining。如果第二步报错，数据库保留“人工请求已记录，但 pending=false”，实际一个 MarkDirty 都没有执行。再次请求命中 already_scheduled，无法补齐。

独立测试 `TestThirdReview_ManualCheckpointFailureCanResume`：在第一次 UpdateManualArrangement 注入错误，恢复 repository 后再请求，实际 skipped=`already_scheduled`，安排次数仍为零。现有 `ArrangementSaveFailurePropagates` 只检查第一次返回 error，没有继续请求验证恢复。

要求：人工预算重开与初始安排意图必须形成原子、可恢复的状态转移；未实际完成安排不能被识别为 already_scheduled。补充第一次断点保存失败以及该窗口进程退出后的再次请求测试。

### T2 / P2：MarkDirty 已成功但进度保存失败，重试会重复执行

位置：`backend/internal/service/person_merge_suggestion_service.go:1221–1232`。

每个依赖先调用 profileRebuildFn，再更新剩余列表。若 MarkDirty 成功、进度更新失败，库中仍保留该人物为未处理。再次请求会重新 MarkDirty；`manual_merge_failure_retry` 还会重置画像恢复预算并推进 change_revision。仅用 remaining 列表不能覆盖副作用完成与断点落库之间的窗口。

独立测试 `TestThirdReview_ManualProgressFailureDoesNotRepeatDirty`：第二次 UpdateManualArrangement 报错，再请求后实际调用集合为 `[8201,8201]`，预期仅 `[8201]`。

要求：画像安排侧能按同一人工恢复意图幂等执行，或将对应画像变更和断点提交原子化。覆盖真实 repository 下的画像 revision/预算、进程退出、并发未完成请求。现有名为 `ManualRetryConcurrentAlreadyScheduled` 的测试实际是两个顺序调用，不能作为进行中并发的证据。

### T3 / P1：尝试身份未保存成功，失败记录仍继续写入

位置：`backend/internal/service/person_merge_suggestion_service.go:2566–2571`。

分配稳定 attempt key 后丢弃 `saveStateLocked` 错误，再提交失败表。此时失败行持有新 key，而扫描状态并没有可靠保存该 key/seq/进度，跨进程的身份一致性前提已失效。后续存在错误去重或重复记账风险，不能用同 key 的正常重放测试证明安全。

独立测试 `TestThirdReview_AttemptKeySaveFailureMustStopLedgerWrite`：配置状态 Set 返回错误，实际 applyFailureIntents 返回 nil，失败表已写入 attempts=1、last_attempt_key=`dg-0-seq-1`。

要求：尝试身份落盘失败必须终止后续提交，并恢复/保留明确的可重入状态；审查尝试身份轮换与最终进度保存的同类窗口。测试不能只模拟失败表报错，必须注入状态持久化错误，并重建服务验证恢复。

### T4 / P1：画像代次查询报错被当成空结果，核对待办被清除

位置：`backend/internal/service/person_merge_suggestion_service.go:1627–1628`、`1546–1555`、`1496–1517`。

lookupProfileGenerations 读取出错时返回空 map。调用方无法区分“确实没有可用画像”和“读取失败”，继续提交该批核对游标，并在下一页为空时清除 ReconcilePending。该轮应补偿的恢复通知因此被漏掉，只能等待额外触发重新扫描。

独立测试 `TestThirdReview_ProfileReadFailureKeepsReconcilePending` 在真实 GORM 画像代次查询注入错误，并确认错误实际命中；执行 Attach 后 ReconcilePending 为 false，而非保留待补做。

要求：画像查询返回 error，未可靠读取的批次不推进、不消费事件、不宣告完成；恢复读取后由现有后台切片补做。验证目标与候选、跨页和重启场景。

### T5 / P2：副本工具的 ok 没有验证恢复闭环

位置：`backend/cmd/relive-identity-recovery-verify/main.go:239`。

代码仍为 `report.OK = elig.FailedExcludedOK`。只要恢复前的资格过滤通过，即使后续工作量未完成或存在未处理失败，报告也可为 ok=true。这没有满足续修任务 §8 对“成功标志必须对应闭环断言”的要求。

要求：为实际要求的恢复、核对、推荐持久化和终态断言分别输出结果，并由全部必要断言决定整体结果；允许合理的 waiting_evidence/零 pending，不要求伪造推荐。专项加速与自然调度单列，不能靠文字解释扩大 ok 的含义。

## 3. 本轮实测

| 检查 | 结果 |
| --- | --- |
| 全量 `go test ./...` | 仍只有既有 `TestClusteringEquivalence` 缺 `identity_retry_after` 失败 |
| 正式 Acceptance/Reacceptance 相关 race | 通过 |
| People 页面 vitest | 16 / 16 通过 |
| peopleHelpers Node 测试 | 8 / 8 通过 |
| 前端构建 | 通过，有资源体积告警 |
| 新增故障恢复反例 | T1–T4 共 4 个均失败，确认上述问题 |
| T5 | 静态检查成功判定代码及当前报告，确认缺少闭环断言 |

Go 使用 `DEVELOPER_DIR=/Library/Developer/CommandLineTools`。既有全量失败在此前已用旧代码同库复现，本轮不将其归类为新增回归。

故障反例通过 overlay 运行，没有修改业务源码或 Buddy 的正式测试。证据在 `/tmp/relive-acceptance-round3-20260915/`：`service_overlay_test.go`、`overlay.json`、`repro.log`、`go-all.log`、`race.log`、前端日志。临时文件可能被清理。

复跑命令（仓库 backend 目录）：

```sh
DEVELOPER_DIR=/Library/Developer/CommandLineTools go test \
  -overlay /tmp/relive-acceptance-round3-20260915/overlay.json \
  ./internal/service -run '^TestThirdReview_' -count=1
```

## 4. 当前完整副本证据

本轮读取了 `/tmp/relive-recovery-20260914/verify-report-final.json`，报告时间为 `2026-09-15T00:26:50Z`，并检查对应验证工具。报告记录：

- 快照基线 failed=24、ready=58180、dirty=1532。
- 使用共享画像 ANN，Attach 时 ann_ready=true，恢复后中心数 14152。
- 264975/356935 恢复为 ready/usable，分别有 38/5 个中心。
- 三个空画像为 no_usable_evidence，失败记录 waiting_evidence，attempts=0。
- 画像运行 80 个切片后 stopped_idle=false；推荐运行 5 个切片后 stopped_idle=true。
- 对 21 个 failed 强制到期，三个空画像通过专项 BuildPersonOnce 加速。

这些是新增的有效报告材料，但不能将 ok=true 直接当作整个验收通过。尤其“失败记录不存在”不能单独证明曾经存在的失败已由正常 resolve 关闭；报告还需提供前后状态与必要断言。

本轮未访问 NAS 重查备份校验、未读取那份远端工作库，也未重新运行约 30 分钟的副本恢复实验。交付已说明工作库后来用于二次烟雾实验，首轮 JSON 是历史终态记录，不应宣称其与当前实验库实时一致。浏览器交互沿用交付材料；本轮独立重跑的是前端测试和构建。

## 5. 下一步

保留已通过的修复，集中补齐 T1–T4 的事务/幂等和错误传播边界，将四个反例纳入正式回归并继续执行至恢复成功，而不是仅断言首次返回 error。修正 T5 后重新生成带完整断言的副本报告，再复验。

本轮仅新增本验收报告；未修改业务代码、提交/推送、部署或操作生产后台。

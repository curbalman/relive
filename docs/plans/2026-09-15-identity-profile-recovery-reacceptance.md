# Buddy 画像恢复返修：第二轮验收

日期：2026-09-15  
结论：**仍暂不通过；上轮直接反例已修复，但重启补偿、失败重放和人工重试仍有遗漏。**  
对象：基于 `fec4f46` 的当前未提交工作区；对照 [返修任务](2026-09-14-identity-profile-recovery-rework.md) 与更新后的 [delivery](2026-09-14-identity-profile-recovery-and-suggestion-failures-delivery.md)。

## 1. 必须继续修复的发现

### S1 / P1：启动核对早于匹配引擎注入，画像依赖补偿没有生效

位置：`backend/internal/service/service.go:181–184`；引擎实际注入在同文件 `236`。依赖查询见 `person_merge_suggestion_service.go:1367–1373`。

启动时先调用 `ReconcileUnresolvedDependencies`，随后才构造并注入 `IdentityMatchingEngine`。核对使用的 `lookupProfileGenerations` 必须从该引擎取得模型签名；引擎尚未注入时直接返回空 map。后续 setter 只赋值，不重新核对，仓库内没有其他生产调用补做画像依赖核对。因此“画像已经恢复、通知前退出”的补偿没有按交付声明工作。现有服务测试先注入引擎再手动核对，覆盖不到实际启动顺序。

要求：在必要依赖注入并达到核对所需就绪条件后触发，保留失败后有界补做；增加实际装配顺序的集成回归。不要只在测试里改变调用顺序。

### S2 / P1：首次恢复的 generation 0 被永久排除在重启核对之外

位置：`backend/internal/repository/person_merge_suggestion_failure_repo.go:722–744`；失败代次写入见 `person_merge_suggestion_service.go:2317` 与 `1356–1395`。

核对要求已记录的目标/依赖 generation 大于 0。当前实现对 failed/dirty 画像读取代次返回 0，因此这不仅是旧迁移数据：本轮新记录的技术不可用目标也会写入 0。它首次恢复到可用 generation 1 后，如果回调丢失，核对直接跳过，exhausted 不能自动重开。候选依赖的 generation 0 同样被跳过。

独立反例 `TestReacceptance_ReconcileFirstRecoveryFromZeroGeneration`：五次 profile_unavailable、记录代次 0；模拟成功恢复代次 1 后执行核对，预期唤醒，实际返回空列表。

要求：持久化区分“未知历史版本”和“已经观测到不可用/尚无代次”，识别真实首次恢复；保留同一恢复事件去重与普通重启不赠预算。不能靠忽略所有 0 值规避误唤醒。

### S3 / P2：启动核对只处理第一页 100 个未解决目标

位置：`backend/internal/service/person_merge_suggestion_service.go:1307`；分页上限见 `backend/internal/repository/person_merge_suggestion_failure_repo.go:364–382`。

服务调用 `ListUnresolved(1, 10000)` 并忽略 total，但 repository 会把 pageSize 钳到 100。服务不翻页，所以只有最近的 100 行能提供目标/依赖的观测代次；更早的行即使已经恢复也无法补偿。把参数写成 10000 不会绕过上限。

独立反例 `TestReacceptance_ReconcileMustVisitBeyondFirst100`：101 条未解决记录，前 100 条代次未变，最早一条从 1 恢复到 2；核对后该条仍是 exhausted。

要求：实现可继续推进的有界分页/游标核对，最终覆盖全部未解决依赖；不要仅提高全表一次性上限。验证跨页及中断后继续。

### S4 / P1：失败批次部分提交后重放，重复消耗 attempts

位置：`backend/internal/service/person_merge_suggestion_service.go:2307–2324`；每次 upsert 增加次数见 `backend/internal/repository/person_merge_suggestion_failure_repo.go:212`。

`applyFailureIntents` 逐条调用各自的 repository 事务。第一条成功、第二条失败时，第一条已经提交，外层返回错误且 cursor 不前进。再次处理同一批意图会重复 upsert 第一条，并再次增加 attempts。WriteQueue 串行执行不等于整批事务，也没有已处理意图标识。这可能让前面的目标因后面目标持续写失败而提前耗尽预算。

独立反例 `TestReacceptance_PartialFailureBatchReplayDoesNotBurnBudget`：第二条写入注入错误，再重放同一 batch；第一条 attempts 从 1 变成 2。上轮中途失败测试只检查 cursor 与 resolve 保留，没有验证失败记录重放的次数。

要求：整批原子提交或为重放提供持久化幂等身份；一个真实尝试只能消耗一次预算，真正的下一轮尝试仍应正常计数。补充进程退出和进度保存失败后的重放验证。

### S5 / P2：人工重试读取阻塞依赖失败，仍返回 accepted

位置：`backend/internal/service/person_merge_suggestion_service.go:1083–1094`。

`ManualRetry` 已接受并设置标记后，`GetByTarget` 报错被 continue，JSON 解析错误也被忽略。调用只重建目标自身，漏掉必要阻塞候选，却返回成功；随后重复请求可能被 already_scheduled 跳过，无法补齐丢失的恢复步骤。

独立反例 `TestReacceptance_ManualRetryDependencyReadErrorMustPropagate`：目标 1001 依赖 1002，注入依赖行读取失败，实际只安排 `[1001]`，API service 返回 nil error。

要求：失败时明确返回错误，并保留可重入的恢复安排状态；再次请求能够补齐依赖。不要只处理 MarkDirty 报错而遗漏前置读取、解析或清理标记失败。

## 2. 本轮独立验证结果

| 检查 | 结果 |
| --- | --- |
| 正式 `TestAcceptance_*` | 全部通过，上轮直接反例已修复 |
| 恢复/失败记录相关针对性 race | 通过 |
| 全量 `go test ./...` | 仍只有 `TestClusteringEquivalence` 缺少 `identity_retry_after` 的失败；同错误已在上轮用旧代码同库复现 |
| People 页面 vitest | 15 / 15 通过 |
| peopleHelpers Node 测试 | 8 / 8 通过 |
| 前端构建 | 通过，仍有资源体积告警 |
| 本轮独立反例 S2–S5 | 4 个均失败，确认上述遗漏 |
| S1 | 静态核对实际生产装配顺序及全部调用位置，确认在引擎注入前执行 |
| `git diff --check` | 通过 |

默认 Go 编译最初被本机 Xcode 许可状态阻挡；使用已有 Command Line Tools 后完成上述检查，未替用户接受许可或修改系统配置。命令前缀为 `DEVELOPER_DIR=/Library/Developer/CommandLineTools`。

新增反例仅用 Go overlay，未改动 Buddy 的源码或正式测试。临时证据位于 `/tmp/relive-acceptance-20260915/`：`overlay.json`、两个 overlay 测试源、`repro.log`、`pagination-repro.log`、`acceptance.log`、`go-all-clt.log`、`race-clt.log`、前端日志。临时文件可能被清理。

复跑新增反例（在仓库 backend 目录）：

```sh
DEVELOPER_DIR=/Library/Developer/CommandLineTools go test \
  -overlay /tmp/relive-acceptance-20260915/overlay.json \
  ./internal/service ./internal/repository -run '^TestReacceptance_' -count=1
```

## 3. 副本与浏览器证据的边界

本轮只读查询了 `/tmp/relive-recovery-20260908/relive.work.db`，并与 `verify-report-final.json` 对照。五个样本终态确实一致：

| 人物 | 状态 / outcome | generation | 活动中心 | 未解决记录 |
| --- | --- | ---: | ---: | --- |
| 264975 | ready / usable | 3 | 38 | 无 |
| 356935 | ready / usable | 1 | 5 | 无 |
| 306601 | ready / no_usable_evidence | 2 | 0 | waiting_evidence，attempts=0 |
| 307945 | ready / no_usable_evidence | 2 | 0 | waiting_evidence，attempts=0 |
| 562545 | ready / no_usable_evidence | 4 | 0 | waiting_evidence，attempts=0 |

这是 **9 月 8 日历史副本的终态一致性核对**，本轮没有重新运行完整恢复流程。副本工具在恢复之后另建诊断引擎、再显式注入并核对，与生产启动顺序不同，因此该结果不能证明 S1 已通过。工具还直接调用样本构建并调整副本调度，应与通用队列自然恢复的证据区分。

读取了 Buddy 的浏览器 `SUMMARY.json`，包含八态交互和展开详情修复记录；本轮重新运行前端测试和构建，未独立重做浏览器八态操作。

交付仍将当日完整快照标为受阻，本轮未访问 NAS 重新核实备份进程，也未独立确认交付所述活锁根因。当前完整副本门禁仍缺交付证据，不能用历史副本替代。delivery 勾选清单应保留这一未完成项，而不是勾选改写后的“9/8 完整副本”。

## 4. 跟进结论

继续在现有实现上修复 S1–S5，将本轮反例转成正式回归，并覆盖真实启动装配、全量分页、首次恢复和可重入的人工安排。然后补齐当前完整副本验收，再申请最终复验。

本轮只新增本报告；未修业务代码、未提交/推送、未部署或操作生产后台。

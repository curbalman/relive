# primary 画像恢复与推荐失败状态修复：验收报告

日期：2026-09-14  
结论：**暂不通过，需返修后复验。**  
验收对象：基于 `fec4f46` 的当前未提交工作区及同日 delivery 文档。未部署，未启动生产人物后台。

任务要求见 [原任务文档](2026-09-14-identity-profile-recovery-and-suggestion-failures.md)。本次检查实际改动、运行测试，并通过 Go overlay 加入独立反例；复现测试没有写入业务源码或覆盖 Buddy 的测试。

## 1. 必须修复的问题

### R1 / P1：失败记录写入错误被忽略，扫描仍可推进

位置：`backend/internal/service/person_merge_suggestion_service.go:945–960`，以及 `1463–1485`。

`primaryAssignments` 丢弃 `applyFailureIntents` 的错误并返回成功。目标因画像不可用而没有 assignment 时，外层会跳过 pending 替换，然后推进 cursor。若失败表写入发生锁冲突或其他存储故障，就会出现“目标没有完成、失败记录也未保存、扫描却已越过”的状态。这重现了原任务要消除的失败丢失路径。

独立测试 `TestAcceptance_FailureWriteMustPropagate` 注入失败表写入错误，预期调用返回错误，实际得到 `nil`。同一提交链上的 Resolve 错误、读取调度失败被当作空清单的问题也需一并处理。

返修要求：错误传回 slice，失败记录可靠落盘前不推进对应进度；明确部分提交的幂等重放方式。增加真实事务失败和 cursor 保持的服务层测试，不能只测试 repository 自己会返回 error。

### R2 / P1：索引恢复不会唤醒已耗尽目标

位置：`backend/internal/repository/person_merge_suggestion_failure_repo.go:616–619`。

`WakeForIndexReady` 仅查询 `retry_wait`。索引不可用持续到五次预算耗尽后，即使索引真正恢复，带 `index_ready_required=true` 的 exhausted 目标仍不会进入重验，必须等其他触发或巡检。原任务明确要求真实“不可用→可用”能够唤醒相关目标，并区别于普通成功重建。

独立测试 `TestAcceptance_IndexRecoveryWakesExhausted` 先制造五次 index_unavailable，再调用恢复通知，实际唤醒列表为空。

返修要求：识别真实就绪状态转移，恢复时有界重开相关 exhausted；普通重建和重复就绪通知不能反复赠送预算。补充重启丢通知后的持久化依赖核对验证。

### R3 / P2：连续人工重试不幂等，会推迟已有工作

位置：`backend/internal/repository/person_merge_suggestion_failure_repo.go:470–479`；服务调用 `person_merge_suggestion_service.go:1063–1077`。

每次点击都会把 attempts 清零、next_retry_at 改为“本次点击后 300 秒”，随后再次将目标及依赖标为 dirty。已经接受但尚未执行的重试再次点击，仍会重复安排恢复并推迟执行时间。

独立测试 `TestAcceptance_ManualRetryIsIdempotent` 在首次点击一分钟后再次点击，原定时间从 12:21:03 推迟到 12:22:03（UTC）。这不符合任务要求的连续点击幂等。

返修要求：对已有活跃人工恢复返回幂等结果，保留原 next_retry_at 和预算；仅在明确新一轮重试时开启预算。必要依赖重建失败也不能被忽略后返回已成功接受。

### R4 / P2：历史未分类空画像没有有界核验入口

位置：`backend/internal/service/identity_profile_coordinator.go:327–390`。

历史接入只处理 failed，调度只选择 dirty 和到期 failed。`ready + build_outcome='' + 无活动中心` 不会进入真实 builder 核验。交付文档也承认仍需手工改三个固定人物 ID，否则推荐侧继续消耗技术重试预算。

独立测试 `TestAcceptance_LegacyEmptyProfileGetsBoundedVerification` 构造真实人物及人脸、未分类 ready 空画像，执行历史接入和批次选择，得到空列表。

返修要求：提供通用、受 batch/budget 限制且幂等的旧空画像核验入口或正式迁移流程，经真实 builder 得出 outcome；不能以部署后手工 SQL 修改三个样本代替该交付。核验失败应走技术恢复，成功确认无证据后进入 waiting_evidence，输入变化后可再次构建。

## 2. 本次实测

| 检查 | 结果 |
| --- | --- |
| 当前代码 `go test ./...` | 失败：仅 `TestClusteringEquivalence` 报本地库缺少 `identity_retry_after` |
| 原 HEAD 配同一份本地库的一致性副本 | 同一错误复现，归类为既有本地数据库/测试环境问题，不认定为本次回归 |
| 恢复 repository、协调器、推荐失败状态的针对性 `-race` | 通过 |
| People 页面 vitest | 15 / 15 通过 |
| peopleHelpers Node 测试 | 7 / 7 通过 |
| `npm run build` | 通过，有资源体积告警 |
| 本次独立验收反例 | 上述 4 项均未满足预期，确认缺陷 |

测试通过仅覆盖现有断言；现有 race 测试没有覆盖上述业务状态错误。未做浏览器交互验收。

复现及日志保存在临时目录 `/tmp/relive-acceptance-20260914/`，包括 `overlay.json`、两个 overlay 测试源、`repro.log`、`legacy-empty-repro.log`、`go-all.log`、`baseline-same-db.log`、`race.log` 和前端日志。临时目录可能被系统清理。

复跑反例（仓库 `backend` 目录）：

```sh
go test -overlay /tmp/relive-acceptance-20260914/overlay.json \
  ./internal/repository ./internal/service -run '^TestAcceptance_' -count=1
```

## 3. 尚未完成的验收门禁

交付文档自报资格隔离副本结果为 ANN 14102、incomplete=0；本次未重新运行该数据实验，不将自报结果写成本次独立实测。

原任务 §9.10 要求的**当前完整副本恢复验证仍未交付**。9 月 8 日备份的三个样本 builder 结果不能替代当前完整副本中 failed 恢复、索引激活、推荐依赖唤醒和最终失败状态的闭环验证。本次未访问 NAS 核实备份进度，delivery 中的 partial 文件状态仅是交付时记录。

返修后应补齐：

1. R1–R4 回归测试及现有相关测试；保留既有全量测试环境问题的明确说明。
2. 在完整一致性副本运行实际恢复链路，逐个解释 failed 与旧空画像的终态，以及恢复后的推荐结果，允许达标候选仍为零。
3. 验证画像提交后索引可查询才唤醒、重复通知不刷新预算、重启补偿持久化依赖版本。现代码通知发生在 `notifyEvidenceReady` 之后才 `applyAnnMerge`，需特别验证该顺序的竞态。
4. 补充前端失败详情、分页和连续重试的实际交互验证；完成后再评定可部署性。

本次仅新增本验收报告；没有修复业务代码、提交、推送或修改生产数据。

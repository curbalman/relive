# Buddy 画像恢复修复：第四轮验收

日期：2026-09-15  
结论：**T1–T4 的本轮针对性复验通过；剩余阻塞集中在 T5 验证工具，整体验收尚未通过。**  
对象：`fec4f46` 基线上的当前未提交工作区；对照 [第三轮修复任务](2026-09-15-identity-profile-recovery-rework-round3.md)。

## 1. 本轮通过的修复

| 项目 | 核对结果 |
| --- | --- |
| T1 初始人工安排 | `BeginManualRetryArrangement` 将人工预算和完整 pending/remaining 放入同一事务；保存失败后恢复执行的测试通过 |
| T2 逐项安排 | 画像 MarkDirty 与 remaining 更新使用同一 tx；中途故障回滚及 barrier 控制的真实并发测试通过 |
| T3 尝试身份 | key 持久化失败终止失败表写入并回滚内存；代码补充最终进度保存失败时恢复原 key/seq/cursor；相关重放测试通过 |
| T4 画像读取 | 显式返回 error，失败不推进核对；恢复查询后下一核对切片能唤醒目标，相关测试通过 |

这是针对上述故障窗口的代码与测试结论，不表示已完成生产部署或全库算法准确率验收。本轮没有新增针对业务恢复路径的阻塞发现。

## 2. T5 仍存在三处误报通过

主要文件：`backend/cmd/relive-identity-recovery-verify/assertions.go`。六项检查已经加入，但部分判断不能支持其“通过”结论。

### V1 / P2：画像仍未完成、仍有到期技术重试，整体也可通过

位置：`assertions.go:79–87`、`207–209`。

空画像样本不是 ready 时，内层只检查“ready 且 outcome 为空”，因此 dirty/failed 样本反而没有把 sampleOK 置为 false。终态检查又把“运行过任意 merge slice”当作通过条件，即使任务尚未完成且仍有到期技术重试。

独立反例 `TestFourthReview_UnfinishedRecoveryMustNotPass`：样本 306601 为 dirty、outcome 为空；画像及推荐均未 idle，retry_due=1。实际六项全部 pass，整体 true。

要求：必需样本未完成必须 fail/blocked；判断该轮是否达到声明的终态，不能用“跑过切片”替代“完成要求”。保留合法 waiting_evidence、明确的耗尽及零 pending 可通过的规则。

### V2 / P2：统计互相矛盾，终态一致性仍通过

位置：`assertions.go:193–203`。

只要 UnresolvedTargets=0 就直接判 pass，没有核对各 state 汇总及实际记录。其他分支也未真正对照任务计数和 pending 数据。

独立反例 `TestFourthReview_InconsistentCountsMustNotPass`：UnresolvedTargets=0，但 waiting_evidence 汇总和记录均为 1，整体仍 true。

要求：先检查计数内部一致性，再核对实际失败表/任务响应与声明范围内的 pending。统计缺失、矛盾必须阻止整体通过，不能把任一来源的零值当作充分证明。

### V3 / P2：故障恢复检查硬编码 pass

位置：`assertions.go:217–223`。

fault_recovery 无条件写入 assertPass，Detail 只是固定测试名称，没有读取测试结果、代码版本或证据文件。即使传入完全空的报告，该项也通过。

独立反例 `TestFourthReview_FaultEvidenceCannotBeUnconditionallyPass`：空 verifyReport 得到 fault_recovery=pass。

要求：此项必须引用可校验的本轮正式测试结果及对应代码标识，或由工具实际执行相应验证。没有证据时为 blocked/未执行；列出测试名称只能证明声明了覆盖范围，不能证明实际通过。

## 3. 本轮实测与证据

- Acceptance/Reacceptance/ThirdReview 针对性 `-race` 通过。
- 全量 `go test ./...` 仍只有既有 `TestClusteringEquivalence` 缺 `identity_retry_after` 错误；此前已用旧代码同库复现。
- 前端 People vitest 16/16、peopleHelpers 8/8、build 均通过；build 有资源体积告警。
- 新增的三个验证工具反例均失败，复现 V1–V3。

Go 使用 `DEVELOPER_DIR=/Library/Developer/CommandLineTools`。反例通过 overlay 运行，没有覆盖 Buddy 的正式测试。证据目录为 `/tmp/relive-acceptance-round4-20260915/`，含 `assertions_overlay_test.go`、`overlay.json`、`repro.log`、完整测试/race/前端日志；临时文件可能被清理。

在仓库 backend 目录复跑：

```sh
DEVELOPER_DIR=/Library/Developer/CommandLineTools go test \
  -overlay /tmp/relive-acceptance-round4-20260915/overlay.json \
  ./cmd/relive-identity-recovery-verify -run '^TestFourthReview_' -count=1
```

## 4. 当前副本材料

本轮读取 `/tmp/relive-recovery-20260915-round3/verify-report-round3.json` 和 `final-state.txt`，五个样本的终态记录一致：264975/356935 为 ready/usable，中心数 38/5；另外三个样本为 no_usable_evidence + waiting_evidence，attempts=0。报告记录使用当前备份、共享 ANN 和 Attach 入口。

这些样本材料有价值，不能因为工具误报就推断实际样本一定错误。但六项 pass 目前不足以支持整个验收通过：报告中的 persistence 只记了两个 no_failure_row，不能单独证明历史失败经正常 resolve 关闭；fault_recovery 来自固定 pass，尚无本轮结果关联。

本轮未访问 NAS 重查工作库或重跑长时间副本任务；以上为交付文件的一致性核对。前端独立验证是测试与构建，未重做浏览器八态操作。生产部署和生产验证均未执行。

## 5. 收尾要求

保留本轮通过的 T1–T4，下一步集中修正 T5 的 V1–V3 并纳入正式反例测试。补齐报告所需的实际计数、前后事件和测试证据，重新生成能正确判定未完成/不一致/缺证据的副本报告。无需为了通过验收修改评分或强行产生新推荐。

本轮仅新增本验收报告；没有修改业务代码、提交、推送或部署。

# Buddy 收尾修复任务：让验收工具正确判定通过

日期：2026-09-15  
依据：[第四轮验收报告](2026-09-15-identity-profile-recovery-acceptance-round4.md)  
范围：仅修复 T5 验证工具的 V1–V3，并补齐报告证据。**T1–T4 的针对性复验已通过，保留现有实现。**

## 1. 要解决的问题

当前工具虽然输出六项检查，但仍会把三种情况判成通过：画像和重试尚未完成、统计互相矛盾、故障恢复测试没有实际结果。此次目标是让报告可信，不是让报告变绿，也不是强行产生新推荐。

主要修改文件：

- `backend/cmd/relive-identity-recovery-verify/assertions.go`
- `backend/cmd/relive-identity-recovery-verify/assertions_test.go`
- `backend/cmd/relive-identity-recovery-verify/main.go`：补充只读证据采集、运行范围及测试证据输入。
- [交付说明](2026-09-14-identity-profile-recovery-and-suggestion-failures-delivery.md)：更新实际结果和限制。

允许在上述命令目录增加少量辅助文件；如现有接口无法取得必要证据，可补充最小只读查询，不借此重构业务恢复流程。

## 2. 统一通过规则

- 保留六项必需检查：`eligibility`、`recovery`、`startup_wiring`、`persistence`、`final_state`、`fault_recovery`。
- 必需检查完整且全部 `pass`，才允许 `OK=true` 和退出码 0；缺项、重复项、被改成非必需项或其他状态均不能绕过检查。
- `fail` 表示已观察到违约或数据矛盾；`blocked` 表示工作未完成或证据不足。两者都阻止整体通过。必需项不能靠 `skipped` 通过。
- 报告必须显示判断依据、观察值和未通过原因；不再用“执行过切片”“没有记录”“列出了测试名”代替成功证据。

## 3. V1：区分运行过和完成了

运行前记录本轮必需样本、验证范围、预期终态和停止条件，运行后按同一范围判断。不得见到结果后缩小范围或删除未完成样本。

本轮沿用现有完整备份及五个样本：

| 样本 | 本轮预期结果 |
| --- | --- |
| 264975、356935 | `ready`、`usable`，且具有符合匹配资格的有效中心 |
| 306601、307945、562545 | `ready`、`no_usable_evidence`，零有效中心；失败账本为合法 `waiting_evidence`，未消耗技术重试次数 |

这些是该备份的验收预期，不是生产数据永远不变的算法规则。采集并输出实际状态，不把样本 ID 对应结果写死成通过。

具体要求：

1. 逐一核对必需样本是否存在；缺失、遗漏、dirty/failed 或 outcome 不符合预期均不能通过。仅检查报告中出现的样本不够。
2. 删除“merge slices > 0 即通过”等分支。声明范围内仍有未处理样本或到期技术重试时，不能宣称恢复完成；达到执行上限时应输出 `blocked` 和剩余事项。
3. 合法 `waiting_evidence`、有原因及预算依据的 `exhausted`、合法零 pending 可以通过。尚未到期的技术重试必须展示 retry 时间并标明仍待验证，不能凭 idle 把它当恢复成功。
4. 不要求本次处理范围以外的全部历史 dirty 清零；报告须区分本轮范围和全库背景统计。专项构建、强制到期等副本加速动作单列，不能据此宣称自然调度已验证。

## 4. V2：用同一范围的真实记录核对统计及持久化

先验证统计完整且一致，再判断终态。不能让 `unresolved=0` 提前跳过其他检查。

- 固定统计时刻及范围；失败表汇总、任务计数、pending 核对使用一致的数据库快照或读事务，避免采集期间的变化被误判为矛盾。
- 核对 `unresolved = retry_wait + exhausted + waiting_evidence`，以及按同一时间划分的 `retry_total = retry_due + retry_deferred`；与实际账本查询、任务 DTO 中对应口径核对。负数、缺失来源、矛盾或未知未解释状态不能通过。
- 标明全量统计与样本统计，不能拿五个样本行数对比全库总数。空查询结果和查询失败必须区分，缺失字段不能自动当成 0。
- pending 记录应与报告数量及本轮目标一致；零 pending 需结合实际匹配结果和失败状态解释，不要求推荐数大于 0。
- 运行前后采集样本失败账本及必要任务状态。若声称某个历史失败已关闭，须证明运行前确有该失败，运行后经过正常服务处理持久化为已解决状态，并能关联到该目标及处理尝试。
- `no_failure_row` 只能描述观察结果。没有前态证据时，不得据此声称 resolve 成功；样本从未失败则如实记录，不伪造失败来凑覆盖。若本轮声明验证历史失败关闭却无可关联证据，该项为 `blocked`。

## 5. V3：关联实际正式测试结果

删除 `fault_recovery` 的固定 `pass` 和仅列测试名的成功判断。采用明确的测试证据输入，例如 `-test-evidence <manifest.json>`，不新增测试平台。

提供可复跑的采集脚本，实际执行所需正式测试并保存：

- `go test -json ... -count=1` 的原始输出、命令、开始结束时间及真实进程退出码；若使用管道，不能把 tee 的成功当作测试成功。
- 包名、每个必需测试的完整名称及执行结果，至少覆盖 T1–T4 的故障窗口与真实并发用例；从正式测试文件确定完整清单，不能只凭包级 pass 或模糊名称匹配。
- 对应代码标识：HEAD、相关源码及依赖清单的内容摘要、日志摘要。当前实现尚未提交，单独记录 `fec4f46` 不足以区分新旧代码。验证工具构建产物也须能关联同一份源码。

工具读取原始结果并核对清单、退出码及代码关联：全部实际执行通过才为 `pass`；实际测试失败为 `fail`；缺失、跳过、证据不完整或代码不匹配为 `blocked`。不能只信 manifest 中人工填写的 `passed=true`，也不能用旧日志冒充本轮结果。

## 6. 先补反例，再修判断

将第四轮的三个反例纳入正式 `assertions_test.go`：

1. `TestFourthReview_UnfinishedRecoveryMustNotPass`：306601 dirty/outcome 为空、任务未完成且 retry_due=1，整体必须不通过。
2. `TestFourthReview_InconsistentCountsMustNotPass`：unresolved=0，但 waiting_evidence 汇总和实际记录为 1，整体必须不通过。
3. `TestFourthReview_FaultEvidenceCannotBeUnconditionallyPass`：空报告的 fault_recovery 不得 pass。

原反例位于 `/tmp/relive-acceptance-round4-20260915/assertions_overlay_test.go`，该文件包含原有测试和追加内容，**仅提取新增用例，不覆盖现有测试文件**。临时文件不存在时按上述条件重建。

另补最少必要边界测试：必需样本缺失、统计来源缺失、历史 resolve 缺前态、测试失败/缺项/跳过/版本不匹配均不能通过；合法 waiting_evidence、明确耗尽、零 pending 配合完整证据可以通过。验证命令的实际退出码与 JSON `OK` 一致。

在 `backend/` 执行并保留日志：

```sh
DEVELOPER_DIR=/Library/Developer/CommandLineTools go test ./cmd/relive-identity-recovery-verify -count=1
DEVELOPER_DIR=/Library/Developer/CommandLineTools go test -race ./internal/service ./internal/repository -run 'TestAcceptance_|TestReacceptance_|TestThirdReview_' -count=1
```

第二条用于确认已通过的业务回归仍成立；测试证据采集脚本还需按第 5 节记录 JSON 输出和实际测试清单。既有全量测试 `TestClusteringEquivalence` 缺 `identity_retry_after` 的问题单列，不作为本次扩展修复内容，也不能把全量测试写成通过。本次无前端变更，无需重复前端验收。

## 7. 重新生成副本报告

复用已取得的完整一致性备份 `2026-09-14-180506-identity-recovery-20260914`，另建干净可写副本运行修复后的工具；保留原备份及前轮工作目录。不要在生产库执行恢复、故障注入或强制到期操作。

交付本轮命令、代码与构建标识、备份标识、工作副本位置、加速动作、原始日志、测试证据、前后状态及新 JSON 报告。对六项检查分别解释结论。旧报告不能手工改标记后作为新报告。

若仍有未完成或缺证据，保留 `blocked/fail`，列出具体目标及原因；工具能正确拒绝误报与本轮副本恢复全部完成是两个结论，应分别说明。

## 8. 完成标准与不包含

- V1–V3 的正式反例及必要边界用例通过，工具确实拒绝未完成、矛盾、缺证据三类输入。
- 合法终态可通过，不以产生推荐、清空全部历史 dirty 或隐藏失败为通过条件。
- 新副本报告关联本轮实际代码和测试结果；历史失败关闭有前后证据，所有非通过项均明确披露。
- 更新交付说明，区分“工具修复完成”“副本验证结果”“生产未执行”；不要再次仅用六个勾声明全部完成。

不包含：重做 T1–T4、调整评分或阈值、扩展匹配算法、前端改版、顺带修复无关全量测试、生产数据迁移、提交、推送或部署。此次仅交付修复代码、正式测试与可复核材料，供再次验收。

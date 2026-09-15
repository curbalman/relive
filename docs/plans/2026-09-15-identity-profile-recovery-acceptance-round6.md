# Antigravity T5 收尾：第六轮验收

日期：2026-09-15  
对照：[第五轮修复任务与技术指导](2026-09-15-identity-profile-recovery-rework-round5.md)  
对象：`fec4f465` 上的当前未提交工作区。  
结论：**部分通过，整体验收仍不通过。已确认明显进展，但存在三项可复现问题及一项测试覆盖缺口。T1–T4 本轮针对性回归通过。**

## 1. 本轮已通过的部分

- 证据文件读取失败、JSON 损坏、哈希不符时不再返回可消费的半成品；调用方生成 blocked，修复上轮“verified 0 formal tests / pass”的关键错误链。
- 删除任意 64 字符即源码匹配的兜底；源码摘要改用统一脚本，包含 Go 源码、测试、依赖清单及嵌入资源。本轮重算 358 文件摘要与交付一致。
- 前态缺失、失败行消失、统计缺失、样本下界矛盾、负 pending 等上一轮反例已加入正式测试并通过。
- 新增同事务数据库快照、GetTask/GetStats 计数对照及采集错误传播；实际画像采集复用了有效中心查询。
- usable outcome 已明确要求；固定 `/tmp/relive-test-evidence-round4/` 的正式测试依赖已移除，新增临时证据与 SQLite 测试。
- 独立执行工具包测试和 Acceptance/Reacceptance/ThirdReview 的 service/repository `-race` 回归，均通过。

## 2. 剩余可复现问题

### G1 / P2：不完整或不属于声明时间窗口的证据仍通过

位置：`test_evidence.go:32–36`、`103–106`、`160–179` 及包完成判断。

以本轮真实交付 manifest 和原始日志为基础，在临时副本单独修改一个条件，loader 和整体断言均仍判 pass：

| 独立反例 | 实际错误行为 |
| --- | --- |
| `TestSixthReview_EvidenceProvenanceMustBeComplete/missing_exit_code` | 删除 exit_code 后被 Go int 零值解释为测试成功 |
| 同组 `wrong_execution_window` | 将执行窗口改为 2020 年，仍接受实际发生在 2026 年的日志事件 |
| 同组 `repository_completion_missing` | 删除正式命令中 repository 包完成事件，并更新日志哈希，仍判整个证据完整 |

当前只验证 manifest 时间非零且先后有序，没有核对事件 Time；只要求 service 包 pass，没有确认命令涉及的其他包完成。上述要求已在任务 §4.3 明确，不属于新增验收范围。

修复方向：使用指针或显式字段存在性检查区分“缺退出码”和“退出码 0”；核对事件与声明执行窗口；按实际命令记录预期包并核对每个包的正常终止，截断或来源不足必须 blocked。不要只给 manifest 增加展示字段而不参与验证。

### G2 / P2：失败关闭证据校验不足，且误拒绝原本已解决的记录

位置：`assertions.go:205–216`。

虽然采集了 ResolvedAt、UpdatedAt 等字段，断言仍只检查关闭原因“非空且不是 manual_retry_reset”，没有校验时间或合法原因集合。独立反例 `TestSixthReview_ResolveRequiresValidTimeAndReason` 的三个子用例均错误通过：

- 没有 ResolvedAt；
- ResolvedAt 早于本轮运行一天；
- ResolvedReason 为不属于模型定义的 `not_a_real_reason`。

反方向的 `TestSixthReview_UnchangedResolvedMustRemainValid` 也失败：前后都是同一条合法 resolved，本应记录 unchanged_resolved，却被判为缺前态而 blocked。任务 §5.4 已明确这种情况可以保留，但不能声明本轮新关闭。

修复方向：完整实现前后状态判定表；本轮新关闭必须具备同一目标、合法原因、运行窗口内的关闭时间及处理证据。前后相同的历史 resolved 应单独处理。不能为修复误报而把合法历史终态一律拒绝。

### G3 / P2：测试采集脚本吞掉进程失败退出码

位置：`record_test_evidence.sh` 末尾。

脚本把 TEST_EXIT_CODE 写进 manifest 后，仍以最后一个 echo 的 0 退出；没有将真实测试失败码返回给调用者。

独立注入结果：仅替换临时 PATH 中的 go test 为退出 7，go list 仍使用真实 Go；运行现有采集脚本：

```text
manifest.exit_code = 7
record_test_evidence.sh exit = 0
```

这会让依赖 shell 返回码的后续步骤误以为采集成功。验证二进制能拒绝 manifest 中的 7，不能替代采集脚本本身的正确失败状态。

修复方向：完成失败材料写出后，脚本显式返回 TEST_EXIT_CODE；摘要/采集自身失败也保持非零。补充脚本级失败注入测试，保留原始错误材料。

## 3. 尚缺的测试覆盖

`main_test.go` 已有真实 run 的缺证据失败路径，但成功路径仍只调用 writer，手填 `OK=true` 和一项名为 baseline 的断言。它证明 JSON writer 能输出 true，不能证明完整 loader → 六项断言 → JSON → exit 0 调用链成立。

仍需补任务 §7 要求的完整 CLI 成功测试及坏证据输入测试，可按已给出的技术指导仅替换耗时 collector，不能替换 loader、断言或手填最终 OK。现有固定临时文件依赖的修复保留。

## 4. 新副本材料核对结果

本轮读取本地 `/tmp/relive-recovery-20260915-round5/` 和 `/tmp/relive-test-evidence-round5/`，独立核对：

- 当前源码摘要：`6770364c8362f63385e1d9a0e4c0ebab7234898e16591b1f2d0d77ad326ad269`，358 个文件，与交付 manifest 一致。
- 原始日志 SHA256：`1b1ba6ca…70ff8c7e`，与 manifest 一致；service/repository 包均实际 pass，五个必需测试有 run/pass 记录。
- 报告 SHA256：`240aba93…116a55cc`，与交付文档一致。
- 报告中的五个样本符合当前备份预期：两人 ready/usable，有效中心 38/5；三个空画像 waiting_evidence、attempts=0。
- 后态 TaskDTO 为 idle、Partial=true、waiting=3、unresolved=3；StatsDTO.Pending=0，与报告内数据库计数一致。Partial=true 在这里是合法等待证据，不据此判失败。
- 材料记录本次副本运行约 30 分 54 秒、退出码 0，使用 Attach、force_due 和样本专项构建。

这些正向材料成立，不因反例推断本轮样本错误。但“本轮真实材料匹配”与“工具能拒绝不完整证据”是两项不同结论，后者仍未完成。交付说明中“彻底修复”“全覆盖”“均可靠拒绝”的表述需要按本报告修正。

本轮未访问 NAS 重查副本数据库或二进制，也未再次运行长时间完整副本；远端退出码和数据库状态以交付材料为限。未操作生产、未复验前端或无关全量 Go 测试。

## 5. 本轮独立复现材料

目录：`/tmp/relive-acceptance-round6-20260915/`。

- `tool-tests.log`、`race.log`：正式测试通过。
- `source-current.json`：独立重算源码清单及摘要。
- `assertions_overlay_test.go`、`overlay.json`、`repro.log`：G1 三个反例、G2 三个错误放行反例和一个错误拒绝反例，共七个场景。
- `script-failure.log`、`script-exit.json`、`injected-failed-test/`：G3 的进程退出码注入及证据。该目录中的失败日志是明确的测试注入材料，不是业务回归结果。

在 `backend/` 复现 G1/G2：

```sh
DEVELOPER_DIR=/Library/Developer/CommandLineTools go test \
  -overlay /tmp/relive-acceptance-round6-20260915/overlay.json \
  ./cmd/relive-identity-recovery-verify -run '^TestSixthReview_' -count=1
```

overlay 仅用于本轮验收；其证据变体从本轮交付日志复制构造，正式测试需改为自建 fixture。不要整体覆盖现有 assertions_test.go，临时材料被清理后可按本报告重建。

本次只新增验收报告及临时复现材料，没有修改业务实现、提交、推送或部署。继续修复限定在上述工具问题及覆盖缺口，无需重做 T1–T4 或调整算法。

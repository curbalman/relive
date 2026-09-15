# Antigravity 收尾修复：第五轮验收

日期：2026-09-15  
依据：[第四轮收尾任务](2026-09-15-identity-profile-recovery-rework-round4.md)  
对象：`fec4f465` 基线上的当前未提交工作区。  
结论：**部分通过，整体验收不通过。问题仍集中在 T5 验证工具；T1–T4 本轮针对性回归通过。**

## 已确认的进展

- 上轮三个 `TestFourthReview_*` 反例已加入正式测试并通过。缺必需样本、dirty 空画像、到期/延期重试、原始的零总数矛盾等已有拒绝分支。
- 六项必需断言不能通过缺项、重复名称或改成非必需项绕过。
- 新增测试证据采集脚本、manifest 和原始 JSON 日志解析；持久化报告新增运行前失败表快照。
- 独立执行 `go test ./cmd/relive-identity-recovery-verify -count=1` 通过；Acceptance/Reacceptance/ThirdReview 的 service、repository `-race` 回归通过。

以下反例从满足预期的报告出发，每次只改变一个条件；使用临时 overlay，不覆盖 Antigravity 的源文件和测试。

## 仍需修复的问题

### F1 / P2：证据读取失败或哈希不符，仍判故障恢复通过

位置：`main.go:255–260`；`test_evidence.go:144–178`；`assertions.go` 的 `fault_recovery` 分支。

`loadAndVerifyTestEvidence` 在原始日志打开失败、读取失败或 SHA256 不符时，返回已经设置 `DigestMatch` 的部分报告和 error。调用方只输出 warn，仍将部分报告送入断言。此时 MissingTests/FailedTests 尚未填充，断言把空列表当成全部成功。

独立复现：`TestFifthReview_EvidenceReadOrIntegrityErrorMustNotPass` 的 `missing_raw` 和 `bad_hash` 两个子用例均失败。加载函数明确返回 error，最终却得到：

```text
fault_recovery=pass
verified 0 formal tests
overall=true
```

修复要求：证据校验错误必须进入不可通过的报告状态；只有完整解析、完整清单检查及完整性校验全部成功后才可判 pass。补充从真实 loader 到断言的错误路径测试，不能只手工构造 `MissingTests` 验证分支。

### F2 / P2：源码关联可以退化为“字符串长度等于 64”

位置：`test_evidence.go:124–134`、`FormalSourceFiles`；采集脚本对应文件清单。

普通构建未注入 `buildSourceDigest` 时，CLI 又传入空 repoRoot，任意长度为 64 的字符串都会得到 `DigestMatch=true`；即使指定仓库但读取源码失败，也使用同样的放行逻辑。独立复现 `TestFifthReview_UnboundSourceDigestMustNotPass` 失败：未比较任何源码或构建标识仍整体通过。

另外，当前摘要只覆盖九个文件，没有包括 `go.mod/go.sum`、`test_evidence.go`、协调器、匹配引擎等相关实现。修改这些内容不会改变摘要，无法完整支持“测试与当前构建使用同一份相关代码”的结论。

修复要求：无法建立源码/构建关联时 blocked，不能按长度兜底通过；摘要覆盖实际相关源码和依赖清单，脚本与验证端使用同一口径。测试日志还应按包名与完整测试名称关联，并确认运行及包级完成事件，不能仅用 Test 名的最后一个 pass。

本轮真实交付日志的五个必需测试确实在 service 包执行通过；这一发现不等于该份日志造假，而是工具仍无法可靠拒绝错误来源。

### F3 / P2：前后状态和统计核对仍有缺口

位置：`assertions.go:167–189`、`268–284`；`main.go:167–173`、`249–251`、`snapshotFailures`。

独立反例全部仍被整体判为通过：

| 反例 | 被错误接受的输入 |
| --- | --- |
| `TestFifthReview_MissingBeforeMustNotPass` | `FailuresBefore=nil`，却将前态未知写成 `never_failed` |
| `TestFifthReview_DisappearedFailureMustNotPass` | 前态明确 retry_wait，后态行消失，仅记录 disappeared_from_ledger 就通过 |
| `TestFifthReview_MissingStatisticsMustNotPass` | `StateCounts=nil`，直接跳过核对 |
| `TestFifthReview_SampleCountLowerBoundMustNotPass` | 三个不同的未解决样本，完整统计总数却为 1；算式内部一致仍通过 |
| `TestFifthReview_PendingCountMustBeValidated` | pending 数量为 -1，完全未参与检查 |

这里不要求“样本数等于全库数”，但全库同类记录至少应包含已经查到的不同样本。当前只检查全库总数是否为零，遗漏非零矛盾。

采集端也仍吞掉前态失败查询和 pending Count 的错误，未采集任务 DTO 以核对任务计数；失败表统计、样本和 pending 没有放到同一快照/事务中。报告不能据此声称失败表、任务和 pending 已全部核对。

修复要求：未知与不存在分开；失败行消失必须有正常关闭证据，否则不通过；统计缺失不能跳过；核对同口径样本下界、实际 pending 和任务状态，并传播采集错误。对于观察到前后都无失败行的样本，只描述该时间窗的事实，不推断其全部历史。

### F4 / P2：应为 usable 的样本，outcome 为空仍通过

位置：`assertions.go:88–98`；`main.go:597–605`。

对 264975/356935 只排除 `no_usable_evidence`，并未要求 `BuildOutcome=usable`。独立反例 `TestFifthReview_EmptyUsableOutcomeMustNotPass` 将 264975 outcome 清空，其他条件保持合法，整体仍通过；正式测试的正例本身也省略了这两个样本的 outcome。

此外，样本 CenterCount 仅按 person_id/generation 计数，并非“符合匹配资格的中心”查询，不能单独证明任务要求的有效中心资格。

修复要求：严格核对本轮声明的 ready/usable 和有效中心，正例也提供完整预期数据。三个空画像的既有严格判断保留。

### F5 / P2：正式测试依赖开发机临时文件

位置：`assertions_test.go:461–463`。

`TestLoadAndVerifyTestEvidence_Valid` 直接读取 `/tmp/relive-test-evidence-round4/test-evidence.json`。本机该文件存在，所以这次工具测试通过；换到干净环境或清理临时目录就会失败。`TestRun_ExitCodeMatchesReportOK` 实际仅测缺参和数据库不存在，没有执行产生 JSON 报告的分支。

修复要求：用 `t.TempDir()` 生成完整、可校验的正反例证据；CLI 退出码测试覆盖实际 JSON OK 与非 OK 路径。交付证据可以保留绝对路径，正式单测不能依赖该路径预先存在。

## 本轮交付材料核对

读取了 `/tmp/relive-recovery-20260915-round4/` 的报告、stderr、run-meta 和哈希清单，并核对 `/tmp/relive-test-evidence-round4/` 中原始日志及 manifest：

- 报告 SHA256 与交付文档一致：`66a6a7a9…232086f`。
- 原始测试日志 SHA256 与 manifest 一致：`306c58ab…127daed`；五个必需测试均有真实 run/pass 事件，service/repository 包均以 pass 结束。
- 以当前九文件算法重算源码摘要为 `48eeeca5…044dc77`，与 manifest 一致。此结论限于该清单，不能弥补 F2 的覆盖缺口。
- 副本报告中的五个样本符合该备份预期；前态失败表为空，后态为三个 waiting_evidence/attempts=0；全库计数和该报告中的样本一致。两个人物前后无失败行，并未提供历史失败关闭案例。
- 材料记录 profile 80 slices、merge 5 slices，退出码 0；没有因上面的反例推断这些样本实际错误。

本轮未访问 NAS 重查数据库或构建产物，也未重跑约 23 分钟的完整副本任务；退出码及远端状态为交付材料记录。本轮独立实测范围为本地工具测试、针对性业务回归及上述反例。生产部署、生产验证和前端复验均未执行；没有重跑无关全量 Go 测试。

## 独立复现材料

目录：`/tmp/relive-acceptance-round5-20260915/`。

- `tool-tests.log`：正式工具测试通过。
- `race.log`：针对性业务回归通过。
- `assertions_overlay_test.go`、`overlay.json`、`repro.log`：8 组独立测试、9 个反例场景均复现误报通过。overlay 文件包含原有测试与追加反例，只提取追加内容，不整体覆盖正式文件。

在 `backend/` 复跑：

```sh
DEVELOPER_DIR=/Library/Developer/CommandLineTools go test \
  -overlay /tmp/relive-acceptance-round5-20260915/overlay.json \
  ./cmd/relive-identity-recovery-verify -run '^TestFifthReview_' -count=1
```

当前预期结果为失败，即揭示错误放行；修复后这些反例应通过。临时材料可能被清理，可按本报告重建。

本次仅新增验收报告和临时复现材料，没有修改业务代码、提交、推送或部署。下一步仍限定为 T5 收尾，无需重做已通过的 T1–T4 或修改算法阈值。

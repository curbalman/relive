# Antigravity T5 收尾：第七轮验收

日期：2026-09-15  
对照：[第六轮验收](2026-09-15-identity-profile-recovery-acceptance-round6.md)、[既定修复任务](2026-09-15-identity-profile-recovery-rework-round5.md)。  
结论：**尚未通过。G3 已通过；G1/G2 各剩一个时间校验缺口；完整 CLI 成功测试已新增，但实际启用后失败。没有新增任务范围。**

## 1. 已确认修复的内容

- 上轮 G1 的三个具体反例：缺 exit_code、显式错误的时间窗口、repository 包缺完成事件，已进入正式测试并通过。
- 上轮 G2 的缺 ResolvedAt、时间早一天、非法原因，以及历史 unchanged_resolved 的具体反例，已进入正式测试并通过。
- G3 已实测通过：临时替换 go test 使其退出 7，现有采集脚本记录 exit_code=7，并实际返回 7，未再吞掉失败。
- 默认工具包测试通过；Acceptance/Reacceptance/ThirdReview 的 service/repository `-race` 回归通过。默认工具测试结果包含下述 CLI 成功用例被跳过的限制。

## 2. G1 尚未完成：日志事件没有时间时跳过校验

位置：`backend/cmd/relive-identity-recovery-verify/test_evidence.go:194–199`。

实现新增了时间窗口检查，但用 `if !ev.Time.IsZero()` 包住它。因此 Time 缺失/零值时，恰好跳过了本应证明事件属于本轮的检查。

独立反例 `TestSeventhReview_MissingEventTimeMustNotPass`：创建具备必需测试及包完成事件的日志，删除所有事件 Time，将 manifest 窗口设为 2020 年，重算日志哈希并匹配源码摘要。工具仍整体通过。

这仍是既定任务 §4.3“原始事件属于本轮执行窗口”的要求。应拒绝缺少执行时间的必需执行/完成证据，输出 blocked；不能因为时间不可验证而放行。现有有时间且匹配窗口的正例保留。

## 3. G2 尚未完成：用任意前后两小时代替真实运行窗口

位置：`backend/cmd/relive-identity-recovery-verify/assertions.go:230–238`。

当前判断是 `CollectedAt ± 2 小时`，不是本轮实际起止时间；CollectedAt 缺失时还会用验收当下的 time.Now() 代替。

独立反例 `TestSeventhReview_ResolvedBeforeRunMustNotPass`：运行前该目标为 retry_wait，运行后为 resolved，但 ResolvedAt 是本轮开始前一小时。该记录不能证明本轮新关闭，工具仍整体通过。

这仍是任务 §5.4 的要求。应采集本轮真实开始/结束或前后快照时间，核对新关闭发生于该窗口；缺窗口不能用当前时间或任意两小时范围补齐。保留已通过的合法原因检查及 unchanged_resolved 分支。

## 4. 完整 CLI 成功测试仍未通过

位置：`main_test.go:294–296`、`365–367`、`394`。

`TestRun_FullCLI_Success` 已编写，但普通 go test 下因未注入 buildSourceDigest 而 skip。为实际验收此分支，本轮以当前源码摘要注入测试包后运行，测试失败：

```text
expected exit: 0
actual exit:   1
startup_wiring: fail — ANN not ready at Attach
```

原因：stubbedCollectors 将画像切片替换成直接返回“执行 5 次、已 idle”，没有建立真实画像服务 ANN；后续真实 Attach 判断索引未就绪。这是测试夹具未准备完整，不应通过放宽 ANN 检查修复。

`TestRun_FullCLI_BadEvidence` 实际运行通过。需把成功夹具准备到真实装配可用，或按既定方案仅替换完整的耗时采集边界，并在正式测试流程中明确注入摘要，使成功用例实际执行。不得手填 report.OK 或直接跳过 startup_wiring。

在 backend 目录复跑本轮完整 CLI 测试：

```sh
DEVELOPER_DIR=/Library/Developer/CommandLineTools go test -v \
  -ldflags '-X github.com/davidhoo/relive/cmd/relive-identity-recovery-verify.buildSourceDigest=7f4b8091fbfc06c3a29def40e98372359c852c415ca1df6245b0d53ab18f414e' \
  ./cmd/relive-identity-recovery-verify -run '^TestRun_FullCLI_' -count=1
```

此摘要为本轮已计算值，源码变化后须重算，不能复制为未来版本标识。

## 5. 交付材料仍需更新

当前重算源码摘要：`7f4b8091fbfc06c3a29def40e98372359c852c415ca1df6245b0d53ab18f414e`。

现有交付说明仍引用 round5 副本及 `6770364c…326ad269` 源码摘要，与本轮代码不同；已知 round6 本地证据/副本目录未见材料。不能用旧报告证明当前版本全部通过。

先完成上述两个判断修复和 CLI 测试，再更新当前源码关联的正式测试、构建和副本验证材料。此前已核对的旧样本结果继续作为历史材料，不据本轮问题推断业务数据错误。

## 6. 本轮证据及边界

目录：`/tmp/relive-acceptance-round7-20260915/`。

- `tool-tests.log`、`race.log`：默认工具测试和针对性业务回归通过。
- `cli.log`：实际启用后的 CLI 成功用例失败，坏证据用例通过。
- `assertions_overlay_test.go`、`overlay.json`、`repro.log`：上述 G1/G2 两个独立反例。临时 overlay 不覆盖正式文件。
- `script-exit.json`、`script-failure.log`、`injected-failed-test/`：G3 失败注入，go test 和脚本均退出 7；此处是明确的注入材料，不是业务回归失败。
- `source-current.json`：本轮源码输入清单和摘要。

在 backend 目录复现 G1/G2：

```sh
DEVELOPER_DIR=/Library/Developer/CommandLineTools go test \
  -overlay /tmp/relive-acceptance-round7-20260915/overlay.json \
  ./cmd/relive-identity-recovery-verify -run '^TestSeventhReview_' -count=1
```

本轮只新增验收文档及临时复现材料，没有修改业务实现、提交、推送或部署；未访问 NAS 或再次运行长时间副本。继续收尾仅涉及既有 G1/G2、CLI 测试和交付证据，T1–T4 无需重做。

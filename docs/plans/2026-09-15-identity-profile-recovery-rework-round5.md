# Antigravity 修复任务与技术指导：完成 T5 验收工具收尾

日期：2026-09-15  
依据：[第五轮验收报告](2026-09-15-identity-profile-recovery-acceptance-round5.md)  
前置任务：[第四轮收尾任务](2026-09-15-identity-profile-recovery-rework-round4.md)  
工作区：`/Users/david/SynologyDrive/Projects/github/relive`，当前基于 `fec4f465` 的未提交修改。

## 1. 任务目标和边界

修复 F1–F5，让验收工具对缺证据、版本无法关联、状态未完成和数据矛盾可靠地拒绝通过；保留合法空结果的通过能力。此次不重做 T1–T4，既有业务针对性回归已通过。

本轮交付的原始日志和五个样本已有可信材料，不需要推翻；但这些材料不能替代工具错误分支的修复。完成标准是“工具能区分真实通过与不能通过”，不是把六项状态修改成绿色。

| 文件 | 本次职责 |
| --- | --- |
| `backend/cmd/relive-identity-recovery-verify/test_evidence.go` | 证据解析、完整性和构建关联、测试执行事实 |
| 同目录 `assertions.go` | 六项必需检查及最终判定 |
| 同目录 `main.go` | 错误传递、前后快照、执行范围、JSON 和退出码 |
| 同目录 `record_test_evidence.sh` | 真实测试采集，统一源码摘要和构建流程 |
| 同目录 `assertions_test.go`，可新增 `test_evidence_test.go`、`snapshot_test.go`、`main_test.go` | 独立、可复跑的正反例及实际调用链测试 |
| 同目录新增 `source_digest.py` | 单一源码摘要实现，供采集及构建脚本复用 |
| [交付说明](2026-09-14-identity-profile-recovery-and-suggestion-failures-delivery.md) | 逐项证据、新副本结果及限制 |

允许工具目录内小幅拆分；优先复用现有 repository/service 接口。不得为方便验收新增生产状态机、数据迁移、公开 API 或改变推荐算法。

## 2. 执行顺序

1. 保存当前改动清单，阅读本任务及验收报告；不要 reset/checkout 覆盖已有工作。
2. 将第五轮 8 组、9 个场景的反例移入正式测试，先确认它们揭示现状。临时 overlay 包含原有整份测试，**只提取追加的 `fifthReview*` / `TestFifthReview_*` 内容**。
3. 修 F1 的错误传播，再修 F2 的源码和日志关联；此时先跑证据解析测试，不启动完整副本。
4. 修 F3 的快照与一致性检查、F4 的 usable/有效中心判定；补齐 F5 的独立测试与 CLI 测试。
5. 运行工具测试与 T1–T4 相关回归，全部满足本任务后冻结代码、采集正式测试证据并构建。
6. 在新副本运行一次完整验证，保留原始材料，更新交付说明。不要一边修改源码一边沿用之前采集的摘要和日志。

测试过程中修改代码后，需要重新采集对应证据；无需每次编辑都跑约 23 分钟的完整副本。

## 3. F1：错误必须传到最终判定

### 当前错误链

`loadAndVerifyTestEvidence` → 返回部分 report + error → `run` 只记 warn → 空 MissingTests/FailedTests 被当成成功 → “verified 0 formal tests / pass”。

### 确定的修改契约

1. loader 遇到文件、解析、完整性或来源校验失败时，返回 `nil, error`；不返回可被消费的半成品成功报告。已经读取到的诊断信息可放入有类型的错误，但不得作为成功证据。
2. loader 完整处理后才返回非 nil 结果。结果明确区分 `pass`、`fail`、`blocked`，默认值不能是 pass。实际测试失败是正常的验证结果 `fail`，不是把 I/O 错误伪装成测试失败。
3. `run` 对 error 构造显式 `blocked` 的证据检查结果，并继续生成 `OK=false` 的最终报告。没有 `-test-evidence` 同样为 blocked。
4. `fault_recovery` 除检查状态外，还核对完整性、源码关联和必需测试清单是否全部满足。不得仅凭“没有列出失败测试”判通过。
5. manifest 只提供原始来源信息，不能提供可信的 `Verified=true` 或决定最终状态；这些字段必须由当前进程验证后计算。

调用链示意，具体类型名可按现有代码调整：

```go
ev, err := loadAndVerifyTestEvidence(path, expectedBuildDigest)
if err != nil {
    report.TestEvidence = blockedEvidence(err) // 明确不可通过
} else {
    report.TestEvidence = ev // 仅完整处理后的结果
}
report.Assertions = evaluateAssertions(report)
report.OK = assertionsAllPass(report.Assertions)
return writeReportAndExitCode(report, stdout, outPath)
```

`writeReportAndExitCode` 只负责写出和退出码，不重新放宽判断。六项完整且全部 pass 才允许退出 0；fail/blocked 退出 1；参数错误保留退出 2。报告写出失败也不能退出 0。

### 必测错误

原始日志不存在、权限/读取失败、SHA256 不符、缺摘要、JSON 损坏、扫描中断/超长行，均不能 pass。哈希格式必须合法且必填；不得用空 `raw_output_sha256` 跳过完整性检查。解析失败的非空行不得静默 continue。

## 4. F2：建立实际测试、源码与构建的关联

### 4.1 源码摘要只保留一个计算实现

由新 `source_digest.py` 统一计算，采集脚本和构建流程都调用它；验证二进制只比较注入的摘要和证据摘要，删除两套手工维护的九文件清单及“长度等于 64 即匹配”的兜底。

输入规则：

- 包含 `backend/` 下的一方 Go 源码及测试，覆盖 cmd/internal/pkg，包含未提交及未跟踪的 `.go` 文件，不能仅依赖 `git ls-files`。
- 包含 `backend/go.mod`、`backend/go.sum`，相关 C/C++/汇编源及头文件，以及上述包实际引用的 `go:embed` 资源。嵌入资源可由对应测试/构建环境下的 `go list -deps -test -json` 的主模块包 `EmbedFiles` 收集，使用声明的测试与构建输入的并集。
- 包含摘要脚本及证据采集脚本本身；不包含输出 manifest、日志、数据库、构建产物和缓存，避免摘要自引用或把运行数据算成源码。
- 将规范化的仓库相对路径排序、去重，按路径长度/路径、内容长度/内容计算 SHA256。输出输入清单、每个文件摘要和总摘要；相同内容不应因路径前缀、mtime 或遍历顺序变化而改变结果。
- 任意必需输入读取失败或嵌入资源枚举失败即停止采集，不能漏掉后继续成功。

必测：修改 `test_evidence.go`、协调器、匹配引擎、`go.mod/go.sum` 或加入新的相关源码后，总摘要改变；只改输出日志不改变；输入缺失报错。不要为了测试修改真实工作区，在 `t.TempDir()`/临时源码目录执行。

### 4.2 构建与测试流程

统一流程为：摘要 A → 正式测试 → 摘要 B → 构建并注入摘要 B → 摘要 C。A/B/C 必须相同；否则废弃本轮证据并重来。记录测试工具链、构建工具链、GOOS/GOARCH/CGO 和实际命令；不同运行平台如实记录，不冒充在同一平台验证。

在 `backend/` 构建示意，`SOURCE_DIGEST` 必须来自上述脚本的实际输出，不能手填：

```sh
go build -ldflags "-X main.buildSourceDigest=${SOURCE_DIGEST}" \
  -o "$ARTIFACT_DIR/verify.bin" ./cmd/relive-identity-recovery-verify
```

脚本需要明确设置输出绝对目录并正确引用变量。缺构建摘要的普通 `go build/go run` 仍可用于诊断，但 fault_recovery 必须 blocked；测试可以通过依赖参数传入期望摘要，不得新增生产 `--skip-verification` 开关。

记录 HEAD 仅作背景，当前未提交源码依靠内容摘要识别。保存最终二进制 SHA256；报告输出自身编译时摘要，使其可与 manifest、源码输入清单和构建记录对照。

### 4.3 原始测试日志按真实执行事实解析

固定使用 `(Package, Test)` 作为键；当前五个 T1–T4 必需用例的 Package 均为 `github.com/davidhoo/relive/internal/service`，完整 Test 名沿用现有 `RequiredFormalTests`，不能靠前缀或同名异包补齐。

解析规则：

1. 必需测试必须有本轮 `run` 和终态 `pass`；必需子测试按明确清单验证，父测试 pass 不能掩盖日志中的子测试 fail。
2. 必需测试所在包必须正常完成；正式命令中任一包 fail 或进程非零均为 fail。仅有测试 pass、缺包完成事件的截断日志为 blocked。
3. 必需测试 missing/skip 为 blocked；实际 fail 为 fail。非必需且按正常测试设计跳过的用例单列，不无条件把整个套件判 blocked。
4. 同一轮中的 fail 不得被后续 pass 覆盖。当前命令使用 `-count=1`，拒绝把多轮拼接日志当成同一次完整执行。
5. 验证 manifest 的命令、起止时间、进程退出码等必需字段确实存在且有效，缺字段不能被 Go 零值自动当成 exit 0。原始事件要落在本轮执行窗口内，允许清楚限定的时间精度误差。
6. `passed_tests` 等人工摘要只作展示，结果从原始事件重算。采集命令使用 `-json -count=1`，保存 stdout JSON 与 stderr，直接捕获 Go 进程退出码，脚本最终也返回真实非零失败码。

此处做可复核的本地证据关联即可，不增加签名服务或远端证明系统。

## 5. F3：完整采集前后状态，再判一致性

### 5.1 先改采集接口，消除“错误变零值”

将 `snapshotSamples` 改为返回 `([]personSample, error)`，检查所有调用点。仅 `gorm.ErrRecordNotFound` 表示查明不存在；其他查询错误必须传递，包括中心计数、人物 hidden 查询、前态失败表及 pending Count。

建议以一份 `verificationSnapshot` 封装 `CapturedAt`、统计范围、失败 Counts/StateCounts、样本画像及失败行、pending 实际查询结果。快照顶层用指针表达“是否完成采集”；只有全部查询成功才返回非 nil 快照。合法全空结果保留明确空 map/list，不把 nil 未采集当成零。

before 在首个恢复/加速写入之前采集，after 在所有切片、通知及写入完成之后采集。每个快照内的数据库读取放入同一读事务，repo 必须用该 `tx` 构造，`CountUnresolved` 使用同一 `now`。不要在读事务里调用 builder 或执行恢复写入。

### 5.2 对照真实任务和 pending 接口

现有可复用接口：

- `PersonMergeSuggestionService.GetTask()`：取得任务状态、Partial 和六个失败计数。
- `PersonMergeSuggestionService.GetStats()`：取得 pending 等统计；比较 `Pending`，不能混用展示口径的 `PendingItems`。
- `NewPersonMergeSuggestionFailureRepository(tx).CountUnresolved(now)`：同快照的失败计数。

工具自行串行驱动切片，在切片返回并确认相关写入结束的静止窗口取得 GetTask/GetStats 和 DB 快照，不要调用 Pause/Resume 改变要验证的状态。**不要在占有 DB 读事务时调用另开连接的 GetTask/GetStats**，以免单连接/锁顺序死锁。

当前 GetTask 内部使用 `time.Now()`。记录接口采集前后时间；若窗口跨过某条 retry 的到期时刻，重新取样，最多三次，仍不能获得稳定窗口则 blocked。无需为验收改造业务时钟。缺任务 DTO、`failure ledger unavailable`、GetStats error 都不能被当成全零。

对齐 GetTask 的六个计数与同口径 DB 汇总。`Partial` 的现有含义是 unresolved>0，**合法 waiting_evidence 的 Partial=true 不代表验收失败**。本轮声明要完成的任务仍 running 或仍有本轮必需待办时，不能只因 due=0 宣称完成；若全局任务只剩范围外工作，明确展示并按声明范围判断，未涉及的历史 dirty 保留为背景统计。

### 5.3 统计必须同时满足的约束

```text
所有计数 >= 0
retry_total = retry_due + retry_deferred
unresolved = retry_total + retry_exhausted + waiting_evidence
StateCounts[retry_wait] = retry_total
StateCounts[exhausted] = retry_exhausted
StateCounts[waiting_evidence] = waiting_evidence
同一状态下不同样本目标数 <= 该状态全量数
不同未解决样本目标数 <= unresolved
DB pending 全量数 = GetStats.Pending
DB 同范围 pending 明细/分组数 = 报告中的同范围计数
GetTask 对应六个计数 = DB 对应计数
```

验证前先确认来源完整、范围一致；未知状态、重复样本 ID、缺少必需样本条目要明确报错。空 StateCounts 在已确认完整且账本为空时合法；在存在三条 waiting 样本时不合法。全局数量不要求等于五个样本数量，但不得小于其对应下界。

合法零 pending 必须有一致的查询结果及任务/失败状态解释；不要求产生正数推荐。waiting_evidence 和 exhausted 按既有原因与预算规则解释，不能仅靠状态字符串通过。

### 5.4 前后失败状态的判定表

| before | after | 正确处理 |
| --- | --- | --- |
| 未采集/查询失败 | 任意 | blocked，不写 never_failed |
| 已确认无行 | 已确认无行 | 记录 `absent_before_and_after`，仅描述本轮观察窗口 |
| 未解决行 | 行消失 | fail，不把 disappeared_from_ledger 当通过 |
| 未解决行 | resolved | 验证同一 target、ResolvedAt、ResolvedReason 和本轮正常处理证据 |
| 无行 | resolved | 本轮缺少完整失败→关闭链路时 blocked，不推断历史关闭 |
| resolved | 同一 resolved | 可记录 unchanged_resolved，但不能声明本轮完成了关闭 |
| 空画像的任意已知前态 | waiting_evidence | 核对 no_usable_evidence、零有效中心、attempts=0 和正常服务落盘 |

模型已有 `ResolvedAt`、`ResolvedReason`、`UpdatedAt`、`LastAttemptKey`，将必要字段采入 failureRow。ResolvedAt 应落在本轮处理窗口；原因遵循模型定义，不能把 manual_retry_reset 当成匹配成功。`LastAttemptKey` 是最近失败尝试，不等于成功 resolve 的新尝试 ID，不能强行要求它更新。

对于声称本轮关闭的样本，可在工具侧记录目标处理前后的快照和切片序号，配合实际服务调用、ResolvedAt/Reason 证明过程；不另建生产审计表。全副本恰好没有历史失败案例时如实记录，由正式隔离测试覆盖关闭分支，不能伪造全副本发生过关闭。

## 6. F4：严格校验 usable 与真实有效中心

直接复用：

```go
repo := repository.NewPersonIdentityProfileRepository(tx)
centersByPerson, err := repo.ListActiveCentersByPersonIDs(sampleIDs, embeddingModel)
```

该方法与 `ListAllActiveCenters` 共享 `matchEligibleCenterQuery`，已经按 ready、active_generation>0、匹配模型、未隐藏及中心代次过滤。不要复制一套 SQL；原始 person/generation 中心数可以保留作诊断，但命名为 raw center count，不能冒充 eligible center count。

本轮五个样本的固定预期在运行前写入报告的 scope：

- 264975、356935：`Present=true`、`Status=ready`、`BuildOutcome=usable`、`EligibleCenterCount>0`，并逐项报告实际值。
- 306601、307945、562545：`Status=ready`、`BuildOutcome=no_usable_evidence`、零有效中心，失败账本 waiting_evidence、原因 no_usable_evidence、attempts=0。

这是该备份的验收预期，不是按 ID 直接产生判断结果。空 outcome、未知 outcome、raw centers>0 但 eligible=0，均不能通过；dirty/failed 的现有拒绝逻辑保留。

## 7. F5：测试必须独立，并覆盖真实调用链

所有证据测试使用 `t.TempDir()` 创建 manifest 和 jsonl，真实计算日志哈希；不得依赖 `/tmp/relive-test-evidence-round4/` 或任何开发机预置数据。SQLite 测试也使用临时文件，并在辅助函数中明确建表、构造必要数据。

分三层实现：

1. **纯断言测试**：构造完整有效报告，逐次改变一个字段；既断言 overall=false，也断言对应检查非 pass，防止因别的缺字段而假通过。
2. **loader/数据库采集测试**：实际创建并读取文件或临时 SQLite，验证错误能到达最终断言。禁止用手写 DigestMatch=false 代替损坏文件、摘要不符和来源缺失的真实路径。
3. **CLI 输出测试**：执行真实报告组装、证据 loader、六项断言、JSON 写出和退出码分支。可把长耗时采集封装成包内依赖并在测试注入小 fixture collector，但仅替换采集，不能 mock loader、断言和报告 writer，也不能新增生产跳过检查参数。另用 SQLite 集成测试验证真实 collector。

CLI 至少验证：完整合法输入生成 JSON OK=true/退出 0；坏证据生成 JSON OK=false/退出 1；缺数据库/缺参数原有分支保持；输出文件写失败非零。测试中解析 stdout/out 文件，不能只判断调用返回码。

### 必须补齐的测试矩阵

| 类别 | 正例 | 反例 |
| --- | --- | --- |
| F1 | 完整日志、哈希和清单匹配 | 原始日志不存在、损坏/截断、哈希错误、部分 report 不得放行 |
| F2 | 注入摘要等于本轮源码摘要，包和测试执行完整 | 无构建摘要、读取源码失败、任意 64 字符、同名异包、缺 run/包完成、失败后 pass、必需 skip |
| F3 来源 | 完整零统计或一致非零统计 | nil 前态/StateCounts、计数查询错误、unknown state、重复或漏样本 |
| F3 对照 | waiting=3 且三条不同样本，pending 与 DTO 对齐 | waiting 总数 1 却有三个样本、负 pending、DTO/DB 不一致、失败行消失 |
| F3 关闭 | 实际服务关闭并记录前后状态 | 没有前态却声称 resolve、关闭原因/时间缺失 |
| F4 | ready/usable + eligible centers>0 | outcome 空/未知、隐藏/错模型/旧代次仅有 raw centers |
| F5 | 全部 fixture 在测试内创建 | 正式测试不能依赖固定临时路径；CLI 拒绝路径必须检查 JSON |

保留第四轮测试；第五轮反例文件在 `/tmp/relive-acceptance-round5-20260915/`，若临时文件已清理，按验收报告条件重建。修改正例使两个 usable 样本 outcome 完整，不能以旧正例不完整为理由放宽实现。

在 `backend/` 执行：

```sh
DEVELOPER_DIR=/Library/Developer/CommandLineTools go test ./cmd/relive-identity-recovery-verify -count=1
DEVELOPER_DIR=/Library/Developer/CommandLineTools go test -race ./internal/service ./internal/repository -run 'TestAcceptance_|TestReacceptance_|TestThirdReview_' -count=1
```

修改涉及共享只读 repository 接口时，再运行对应包相关测试。没有前端变更无需重跑前端；既有全量 `TestClusteringEquivalence` 问题继续单列，不在本任务顺带修复或宣称全量通过。

## 8. 副本验证与交付

使用原一致性备份 `2026-09-14-180506-identity-recovery-20260914`，先验证其已有哈希，再建立全新工作副本，例如 NAS `verify-work-20260915-round5/`；若目录已存在，选新后缀，不覆盖前轮或原备份。

保持生产等价 Attach/ANN/服务恢复入口。`force_due`、样本优先构建等加速动作继续单列，scope 同时列出五个必需样本与全库背景；不据此声称生产自然调度已完成验证。

交付目录至少包含：

- 新 JSON 报告、stdout/stderr、实际命令、起止时间和进程退出码。
- 原始 `test-output.jsonl`、测试 stderr、manifest、源码输入清单与摘要、构建参数及二进制哈希。
- before/after 的同范围失败状态、画像有效中心、pending、任务 DTO；说明静止窗口及采集时间。
- F1–F5 修改文件、测试名称与结果对照，不只提供五个勾。

新代码若因更严格判断发现未完成，保留 blocked/fail 并说明目标及缺证据；不能删除必需检查、改测试期望或修改报告来强行通过。对旧报告保留历史标签，不手工改为当前权威结果。

更新交付说明时分别写清：工具修复是否完成、副本验证是否通过、生产未执行。要申请再次验收，需提供上述可复核材料及仍未解决的问题。

## 9. 完成清单与不包含

- F1–F5 均有真实正反例，特别是 loader 返回 error 后 overall 仍为 false。
- 无“长度匹配即版本匹配”、未知前态即 never_failed、切片执行过即完成、空测试列表即通过等兜底。
- 完整性、来源、统计、持久化、有效中心和 JSON/退出码契约一致；合法 waiting_evidence、明确耗尽及合法零 pending 不被误拒绝。
- 单测不依赖任何开发机临时证据；T1–T4 针对性回归保持通过。
- 新副本材料能从源码、测试、构建关联到运行结果；非通过项全部披露。

不包含：重写 T1–T4、调整阈值/评分、扩大算法范围、前端改版、无关测试治理、生产数据操作、发布、提交或推送。此次只修改工具与必要的只读采集及测试，保留其他未提交工作。

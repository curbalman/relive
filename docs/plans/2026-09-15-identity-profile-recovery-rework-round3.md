# Buddy 修复任务：数据库出错后不漏做、不重复做

日期：2026-09-15  
项目：`/Users/david/SynologyDrive/Projects/github/relive`  
对应：[第三轮验收报告 T1–T5](2026-09-15-identity-profile-recovery-acceptance-round3.md)

## 1. 这次要解决什么

保留已经完成的算法资格过滤、失败画像恢复、分页核对和任务页功能。本轮修复数据库临时出错后的三个行为问题，并补实验证工具：

| 用户能感知的问题 | 本轮目标 | 验收编号 |
| --- | --- | --- |
| 点击重试后没真正安排，再点却提示已经安排 | 安排状态与实际工作一致，失败后能补齐 | T1 |
| 工作已安排，进度保存失败后又重复安排 | 同一次请求不重复重置画像和恢复预算 | T2 |
| 重试次数或检查进度在数据库出错后不可靠 | 未保存的身份不使用，未读成功的批次不跳过 | T3、T4 |
| 工具显示通过，但只检查了一个环节 | 必须验证完整恢复链路才显示整体通过 | T5 |

本轮不重设算法、不以增加推荐数量为目标。原 [第二轮续修任务](2026-09-15-identity-profile-recovery-rework-round2.md) 的约束继续有效；本文件明确剩余修复内容。最终更新 [交付文档](2026-09-14-identity-profile-recovery-and-suggestion-failures-delivery.md)。

开始前检查当前 HEAD、分支和未提交改动。验收时基线为 `fec4f46`，实现仍在工作区；不要重置或覆盖已有工作。

## 2. 执行方法与统一完成标准

先将 `/tmp/relive-acceptance-round3-20260915/` 的四个 `TestThirdReview_*` 反例加入正式测试。overlay 文件包含原测试全文，只提取新增测试和必要 stub，不整体覆盖当前文件。临时文件不存在时，按验收报告重建场景。

**每个故障测试必须跑完整个过程：第一次发生指定错误 → 检查没有虚报或越过进度 → 恢复存储 → 再次执行/重建服务 → 验证工作正确完成且只执行一次。** 只验证首次 `require.Error` 不算完成。

实施顺序：T1/T2 一起修复人工安排事务边界 → T3 尝试身份持久化 → T4 读取错误传播 → T5 验证工具及副本重跑。新失败测试先在未修版本失败，修复后再通过；保留此前正常路径与回归。

## 3. T1 + T2：人工安排与断点一起可靠提交

主要范围：

- `backend/internal/service/person_merge_suggestion_service.go`：`RetryFailures`、安排步骤与 hook。
- `backend/internal/repository/person_merge_suggestion_failure_repo.go`：`ManualRetry`、`UpdateManualArrangement`。
- `backend/internal/repository/person_identity_profile_repo.go`：人工恢复 MarkDirty 与恢复预算。
- 必要的 model、迁移、生产注入和对应测试。

采用同一 SQLite 数据库内的短事务完成以下状态变更，具体事务 API 放在 repository 层，service 负责业务编排：

1. **初始事务**：人工预算重开、人工原因、pending 安排及完整剩余依赖列表一起落库。事务失败则整体不生效，不留下 `ManualRetriedAt != nil && pending=false` 的半完成记录。
2. **逐项事务**：某个人物的画像 dirty/revision/恢复预算变更，与从该人工安排剩余列表中移除该人物，一起提交。成功表示两者同时生效；失败则两者均不生效。
3. **完成判定**：只有所有必要项已经提交，才认定安排完成并返回 accepted。并发重复请求重新读取当前事务内状态，不依据事务外的旧计划覆盖新进度；已完成的同一安排返回 already_scheduled。

所有相关 repository 写操作必须使用传入的同一个事务连接；不能在外层开事务后仍调用使用原始 DB 的 MarkDirty/UpdateManualArrangement。保持 WriteQueue 边界，禁止在事务内等待另一个 WriteQueue 回调或执行画像 builder/ANN 全量重建。

保留原 MarkDirty 的完整业务效果。数据库提交后的 ANN 失效、调度通知采用现有可补偿路径；通知前退出不能导致旧画像继续被当作有效匹配证据。数据库事务之外的通知失败不重复执行已经提交的人工预算重置。

多个选中目标可能共享阻塞依赖：同一人工操作中的共享人物只安排一次。保持目标资格与人工规则校验；不能为了补齐计划而重新启用隐藏人物，不能把健康依赖无差别重建。

必须通过：

- `TestThirdReview_ManualCheckpointFailureCanResume`：初始安排保存失败，恢复后再次请求实际安排，不误判 already_scheduled。
- `TestThirdReview_ManualProgressFailureDoesNotRepeatDirty`：逐项进度保存失败，再次请求不重复提交成功的画像变更。
- 在真实 SQLite repository 上检查 `change_revision`、recovery attempts/exhausted、remaining、人工状态的前后值；不能仅检查 mock 调用次数。
- 初始事务前后、逐项事务前后、提交后通知前分别模拟退出，重新构造 service 后继续。
- 用 goroutine 和 barrier 控制两个尚未完成的并发请求，验证同一项只提交一次；两个顺序点击不算并发测试。
- 多目标共享依赖、中途失败、waiting_evidence、不合格目标和正常重复点击的既有契约不回归。

若事务化使“副作用已提交但进度未提交”窗口不再存在，应将对应反例调整为注入同一事务中途故障并断言两者回滚，再继续验证恢复后只提交一次；不得删除业务预期。

## 4. T3：尝试身份保存成功后才能使用

主要范围：`person_merge_suggestion_service.go` 中 `ensureFailureAttemptKeyLocked`、`applyFailureIntents`、`rotateFailureAttemptKeyLocked`、slice 最终进度提交，以及失败表去重逻辑。

- 分配 key/seq 时先构造候选状态，持久化成功后才把它作为可使用的尝试身份。`saveStateLocked` 返回错误必须传给调用方，停止后续失败表写入。
- 持久化失败后的内存状态不能冒充已保存状态；下一次请求必须重新完成保存，不能因内存已有 key 而跳过。
- 失败记录提交成功后，cursor、key 轮换及 seq 更新采用可恢复的提交顺序。最终进度保存失败时继续保留同一尝试身份用于重放；重启和不重启两条路径结果一致。
- 同一真实尝试重放不增加 attempts、不推迟 next_retry_at；真正的下一轮尝试正常使用新身份并消耗预算。不要通过不再轮换 key 让测试通过。
- 检查这条链路中的所有被忽略状态写入错误，不能仅修一处 `_ = saveStateLocked()`。

必须通过 `TestThirdReview_AttemptKeySaveFailureMustStopLedgerWrite`，并补齐：恢复后成功写入一次、失败行提交后最终进度保存失败、重启重放、内存继续执行、并发 MarkDirty、新一轮真实尝试直到五次耗尽。断言失败行、持久化扫描状态和内存状态一致。

## 5. T4：读取失败保留当前核对批次

主要范围：`lookupProfileGenerations`、`profileActiveGeneration`、`runUnresolvedDependencyReconcile`、证据通知和失败意图构造，均在 `backend/internal/service/person_merge_suggestion_service.go` 及相关测试。

- 画像代次读取接口显式返回 error。查询成功但没有可用结果，与数据库查询失败必须区别处理。
- 任一必要目标/依赖读取失败，该批不推进 ReconcileAfterID，不更新已消费版本，不清除 ReconcilePending，也不进入“本轮完成”。
- 失败后由现有后台切片有界重试当前批次。读取恢复后自动补做，不要求用户再次点击、不等待次日巡检；暂停与原切片预算继续有效。
- 在写入失败记录等其他调用点，查询错误不能被压成 generation=0 并记录为“已知不可用”；保持观测信息真实。
- 修复所有调用方，不能将新返回的 error 在下一层重新忽略。无返回值的 callback 必须留下可持久化待办，而不是只打日志后退出。

必须通过 `TestThirdReview_ProfileReadFailureKeepsReconcilePending`；进一步在真实 GORM 查询注入错误后撤销故障，让下一切片唤醒目标。覆盖候选依赖、最后一页、跨页读取失败、状态保存失败、重启继续，以及成功空结果正常结束。

## 6. T5：验证工具必须逐项断言整个流程

主要范围：`backend/cmd/relive-identity-recovery-verify/main.go`、必要的共享 service 验证入口、工具测试与交付报告。

将 `report.OK = elig.FailedExcludedOK` 改为明确的必需断言集合。每项输出名称、验收范围、结果（通过/失败/受阻/未执行）及相关前后数据；整体 OK 仅在所有必需项通过时为 true。失败、受阻或缺失必需证据时返回非零退出码，并保留结构化报告。

必需断言至少包括：

1. **资格**：failed/hidden/模型或代次不合格中心不参与当前匹配。
2. **恢复**：本轮声明覆盖的 failed/旧空画像，确实经过通用恢复入口和真实 builder，终态及未完成原因明确。
3. **启动与核对**：生产共享装配路径、索引就绪、通知丢失后重启补偿，以及 T4 读取失败后继续。
4. **持久化**：service 实际保存推荐/失败状态；技术失败保留，完整重验成功才关闭。原本没有失败行的样本不能用“最后仍不存在”证明 resolve 成功，需有可核验的前后事件。
5. **终态一致**：失败表、pending、任务计数与解释相符；合理的 waiting_evidence、耗尽状态和零 pending 可以是合格结果，但原因必须明确，不能把技术未完成冒充正常零结果。
6. **故障恢复**：人工安排及尝试重放的失败→恢复过程有正式测试或副本验证证据，不能只记录首次抛错。

范围必须事先写明。不要求隐藏人物积压或无关后台队列清零；超出本轮 budget 的必需样本标未完成，不能靠缩小报告范围直接通过。

增加工具测试证明：资格过滤通过但任一必需环节失败/未完成时，整体不能 OK；合法零推荐及 waiting_evidence 能正确通过。报告不能只增加文字说明而不改变程序判定。

使用已经完成的 2026-09-14 一致性备份创建新的独立工作副本，运行修复后的工具并保存报告及对应终态证据。保留旧报告与烟雾实验，不覆盖后再宣称前后状态一致。记录快照来源、代码标识、参数、校验和、实际调用路径、前后状态及工具退出码。

通用接入与自然调度作为主验收；强制 due、BuildPersonOnce、补发通知等专项实验单列，不能代替主流程断言。新增合成故障只用于测试/副本，不修改生产证据，不把合成推荐当真实算法效果。

## 7. 交付前检查与停止条件

运行新增正式测试、此前 Acceptance/Reacceptance 回归、相关事务及并发路径 `-race`，最后执行 `cd backend && go test ./...`。本机必要时可使用已验证的 `DEVELOPER_DIR=/Library/Developer/CommandLineTools`。

既有 `TestClusteringEquivalence` 本地库缺字段继续如实单列；不删测试、不修改生产库、不把全量失败写成全绿。前端运行 People vitest、peopleHelpers 和 build；若修改 API 响应或重试反馈，补对应浏览器交互。

更新原 delivery，逐项回应 T1–T5，给出正式测试、实际结果、新副本报告位置和剩余限制。新增数据库字段/事务接口需要迁移幂等与旧版本兼容说明，回退保留失败记录和人工历史。

- [ ] T1 初始安排失败后能补齐，无假 already_scheduled。
- [ ] T2 逐项提交、退出和真正并发场景不重复重置画像。
- [ ] T3 尝试身份与进度保存失败后，重放次数可靠。
- [ ] T4 画像读取失败不越批，恢复后自动完成核对。
- [ ] T5 工具逐项断言，整体成功判定与实际证据一致。
- [ ] 正式回归、race、前端检查完成，既有环境失败单列。
- [ ] 修复后的当前完整副本报告及终态证据已保存。

未全部完成时保持“修复中”或“验收受阻”。本地代码、当前副本、生产部署与生产验证分别标记。此次提交复验前，主动检查上述窗口的相邻步骤，不能仅以四个原反例变绿作为任务结束。

## 8. 不包含与授权边界

不改变评分公式、0.55 阈值、embedding、召回预算，不重做算法、不全库重聚类、不自动合并，不解除 hidden/manual_locked/cannot-link，不扩展无关调度平台或 UI。

提交、推送、部署及生产写入沿用 Buddy 执行会话已有授权；本文不新增生产操作授权，也不要求重复确认已授权事项。验证默认在测试库和独立副本进行，不启动生产人物后台，不为代码返修暂停生产。

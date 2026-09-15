# Buddy 返修任务：画像恢复与推荐失败状态闭环

日期：2026-09-14  
项目：`/Users/david/SynologyDrive/Projects/github/relive`  
任务性质：在已有实现上返修，完成本地代码、回归测试及完整副本验收。

## 1. 承接关系与完成标准

先阅读：

1. [原任务](2026-09-14-identity-profile-recovery-and-suggestion-failures.md)：保留原有范围与约束。
2. [验收报告](2026-09-14-identity-profile-recovery-acceptance.md)：R1–R4 已通过独立反例确认，当前结论为暂不通过。
3. [上轮交付](2026-09-14-identity-profile-recovery-and-suggestion-failures-delivery.md)：参考已有工作和证据，不继承其中“未完成项不阻塞交付”的判断。

本轮完成标准：失败能可靠保存，恢复后能自动重验，人工重试幂等，旧空画像能有界核验，并在当前完整一致性副本中验证整个链路。允许 pending 仍为零，不能通过调低阈值或构造推荐证明成功。

开始时检查当前分支、HEAD 和未提交改动。上轮验收基于 `fec4f46` 的工作区，不假定行号或代码仍未变化；保留已有实现和其他未提交内容，不重置工作区。

## 2. 实施顺序

1. 把四个独立验收反例转为仓库正式测试，确认在未修代码上失败。
2. 依次完成 R1 → R2 → R3 → R4；每项包含实现与针对性回归。
3. 补充事务失败、并发通知、重启和实际前端交互验证。
4. 在完整副本运行恢复闭环；重跑必要检查，更新交付文档，逐项回应验收报告。

反例临时目录为 `/tmp/relive-acceptance-20260914/`。overlay 文件是附加测试后的整份测试文件，只提取新增 `TestAcceptance_*` 和必要 stub，不能整体覆盖当前测试。临时目录若已被清理，按验收报告中的输入与预期重建反例。

## 3. R1 / P1：可靠保存失败与扫描进度

主要范围：`backend/internal/service/person_merge_suggestion_service.go`、`backend/internal/repository/person_merge_suggestion_failure_repo.go` 及相关测试。

修复 `primaryAssignments → applyFailureIntents → RunBackgroundSlice` 的错误传播和提交顺序：

- 技术失败、waiting_evidence 写入失败必须返回给 slice，不能返回正常 assignments 后继续推进对应 cursor。
- pending 保存、failure resolve、进度持久化形成明确的提交协议。采用事务或可验证的幂等重放；任一步失败后，不出现“失败已关闭但建议未保存”，也不因同一计算结果重放重复消耗 attempts。
- 不再忽略 Resolve、ResolveIneligible、必要状态写入错误；失败保持可重试、可审计，不能仅补日志后照常完成。
- 调度查询或未解决计数读取错误不能被解释为零工作、零失败或本轮完成。沿用现有错误响应/任务错误状态表达不可用，不伪造正常空状态。
- 保留 DirtyGeneration、防并发前台写和 WriteQueue 约束，避免持锁重入；部分有效建议可以保存，但对应未解决状态必须保留。

必须测试：失败表写入错误传到 slice；真实事务中途失败；pending 已保存但 resolve 失败后重放；cursor 写入失败后重放；调度读取报错；并发 MarkDirty 时不覆盖新进度。验证 pending、failure、attempts、cursor 的最终一致状态，而不只断言 error 非空。

## 4. R2 / P1：恢复唤醒与重启补偿

主要范围：`person_merge_suggestion_failure_repo.go`、`person_merge_suggestion_service.go`、`identity_profile_coordinator.go`、`person_identity_profile_service.go`、`person_identity_profile_ann.go` 和注入入口 `service.go`，均位于 `backend/internal/repository/` 或 `backend/internal/service/`。

- 索引真实从 unavailable 恢复 ready 时，唤醒所有仍有效的 index_ready_required 目标，包括 exhausted；进入有界重验。
- 普通成功重建、重复 ready 通知、未改变阻塞条件的进程重启不能重复刷新预算。不能仅把查询中的 retry_wait 改成两个状态就视为修复完成，必须识别并消费对应恢复事件。
- 画像目标或阻塞候选恢复后，提交 generation 并完成正常索引激活，达到可读取、可查询状态才唤醒依赖它的推荐。修正当前先 notifyEvidenceReady、后 applyAnnMerge 的顺序风险。完整构建确认无证据时，按 waiting_evidence 转移，不要求凭空生成可查询中心。
- 持久化足以识别目标/候选证据变化的版本。候选依赖记录候选的版本，不能写目标版本；去重通知依据已处理版本，不能每次收到同一 person ID 都重开预算。
- 启动后有界核对未解决记录的持久化依赖，补偿“画像/索引已恢复但通知尚未发出就退出”。不得依赖用户点击或等待次日全量巡检；也不能每次启动重开所有 exhausted。
- 唤醒与完整巡检并发时，不能把尚未扫完的工作直接覆盖成 RetryOnly；沿用扫描代次保护，保留两类待办。

必须测试：exhausted + index_unavailable 恢复；普通重建不赠预算；同一事件重复通知；候选恢复；DB 提交成功但索引激活失败；提交与通知之间重启；重复重启无证据变化；唤醒与完整巡检并发。使用真实 repository/ANN/engine 组合覆盖关键链路，不只手动调用 callback 断言计数变化。

## 5. R3 / P2：人工重试幂等且不虚报成功

保留现有 API：`POST /api/v1/people/merge-suggestions/failures/retry`，请求 target_ids 去重后 1–100 个，响应保留 accepted_target_ids 与 skipped。

- 第一次有效请求，为选中且仍合格的未解决目标及必要依赖安排一轮恢复；记录人工原因。
- 已接受、尚未完成的同一轮人工重试再次请求，返回 skipped，稳定原因使用 `already_scheduled`。不改变 next_retry_at、attempts、恢复代次，不再次 MarkDirty。不能把所有 retry_wait 一概跳过：尚未安排人工恢复的目标仍允许首次人工请求。
- 对确需重建的 exhausted 画像同时正确开启画像恢复预算；推荐预算与画像预算分别管理。不要只重置推荐 attempts，而让依赖画像沿用已耗尽预算。
- waiting_evidence 且输入未变仍返回 waiting_evidence；已删除、隐藏或不再符合目标资格的记录返回明确 skipped 原因，不安排工作。不对健康依赖无差别重建。
- 必要画像恢复安排失败、记录读取失败或状态写入失败不能被吞掉后返回已成功 accepted。保证可恢复的部分提交协议：重试请求能够补齐未完成步骤，不重复安排已成功步骤；可复用现有状态字段或补充最小持久化字段及正式迁移。
- 前端沿用 `frontend/src/api/people.ts`、`frontend/src/views/People/index.vue` 与 `peopleHelpers.ts`，准确显示已安排/跳过/失败并刷新详情和计数。按钮防连点仅作为体验优化，后端必须对并发请求幂等。

必须测试：间隔一分钟的连续点击不推迟时间；并发重复请求仅安排一次；依赖重建失败后再次请求可补齐；已耗尽画像的人工恢复预算；100 个上限及目标去重；waiting_evidence/不合格目标跳过。前端增加响应文案和重复操作测试。

## 6. R4 / P2：历史空画像有界接入

在现有画像协调器增加通用的历史未分类空画像接入：人物仍合格、未隐藏，画像 ready、build_outcome 为空，且无符合活动代次的中心。接入受当前 dirty batch/slice budget 与准入控制约束，不另起无限扫描循环。

- 以可恢复、幂等的状态安排真实 builder 核验；并发证据变化时保留新变化，不能覆盖更新后的画像。
- 接入后复用 `ListProfileFaces → builder → ReplaceGeneration`。读取或计算技术失败进入 failed 有界恢复，不写 no_usable_evidence；完整成功无中心才写 no_usable_evidence，并使相关推荐转为 waiting_evidence。
- 已完成分类且输入未变化的空画像不重复重建，不消耗技术重试预算；新证据到来后允许正常重建。
- 三个生产样本 ID 仅用于副本对照，不能写入业务代码或替代通用迁移条件。删除交付中“部署后必须手工 SQL 改三个 ID”的必需步骤，改成正常接入路径及状态核验说明。

必须测试：历史空画像进入有界批次；超过 batch 的分页接入；重复切片/重启不重复安排；真实 builder 结果分类；读取失败；证据变化重建；隐藏人物不进入；接入与前台修改并发。

## 7. 验证与交付证据

### 7.1 代码与前端

- R1–R4 的正式回归全部通过，再跑相关恢复/索引/事务路径的 `-race`。
- 执行 `cd backend && go test ./...`。上轮 `TestClusteringEquivalence` 的本地库缺字段已在旧代码同库复现；本轮重新记录实际结果，不删测试、不为通过而修改生产数据，也不把旧失败当作全量通过。若使用隔离测试库，说明哪些依赖本地数据的测试被跳过。
- 前端执行 People 页面 vitest、peopleHelpers Node 测试及 `npm run build`。实际浏览器验证 idle+exhausted、暂停+partial、waiting_evidence、正常零结果、详情分页、成功重试、重复重试和失败提示；首屏不增加失败详情查询。
- 保留原有 primary/legacy/shadow/rescue 和人工反馈保护测试。所有匹配阈值、评分与召回预算保持不变。

### 7.2 当前完整副本

先核实完整备份是否已经完成，不沿用上轮 partial 大小作为现状。记录快照时间、来源、完整性检查和所需表/字段是否齐备；partial 文件及缺 embedding/成员关系的精简诊断库不能用于恢复验收。只读采集生产数据时使用一致性备份和已验证命令，不实例化会运行启动清理的完整服务。

在独立可写完整副本上运行正常协调器和推荐服务链路，记录：

1. 资格隔离基线：先不恢复 failed，保留输入和参数说明，确认无效历史中心不参与召回。
2. failed 恢复：当前快照中的未隐藏 failed，包括仍存在的 264975、356935；逐项输出原状态、尝试数、结果、generation、索引资格和最终失败记录。样本若已变化，记录事实，不硬改成旧状态。
3. 历史空画像：306601、307945、562545 若仍适用，经新通用入口处理，不手动写 outcome；输出输入/可用证据/中心数量及终态。
4. 推荐闭环：画像恢复 → 索引可用 → 目标或候选依赖唤醒 → 服务重验 → pending/失败表/任务计数一致。故障及进程中断注入只在测试或副本进行。
5. 无达标候选可以保持零 pending，但每个未解决目标均可解释。当前完整副本结果不能由 9 月 8 日三个样本的 builder 结果代替。

若完整副本暂不可获得，继续完成独立的代码/测试工作，并明确写“完整副本验收受阻”，列出缺失证据和恢复条件。不能把这一项改为可选或直接宣告整项完成。

### 7.3 最终交付格式

更新原 delivery 文档，围绕返修后的最终行为重写过时段落，附一张 R1–R4 对照表：根因、修改位置、正式测试名称、实测结果。补充迁移幂等性、API 兼容性、完整副本结果、剩余问题与回退步骤。

逐项勾选：

- [ ] R1–R4 修复并通过正式回归。
- [ ] 索引就绪、去重通知、重启补偿和并发扫描验证通过。
- [ ] 相关测试、race、前端构建/交互完成；既有环境失败单列。
- [ ] 当前完整副本恢复闭环通过。
- [ ] 交付文档与最终代码一致，无手工三个 ID 补洞的必需步骤。

“本地代码完成”“副本验收通过”“生产已部署”“生产已验证”分开标记。前五项未全部完成时，整体状态保持返修中或验收受阻；最后两项没有实际执行则写未执行。交付后由用户安排复验。

## 8. 边界与不包含

本说明授权范围是返修任务的实现说明；提交、推送、部署和生产操作沿用 Buddy 执行会话中已有授权，不重复索要已授权事项，也不由本文新增生产写入授权。

- 不改变 0.55 推荐阈值、embedding、评分公式、召回预算，不恢复旧算法兜底。
- 不自动合并，不全库重聚类，不解除 hidden/manual_locked/cannot-link 或人工排除。
- 不启动生产 `/people/background/start` 验证本任务；它会持续消费队列，不是小批 dry-run。
- 不把“推荐必须新增若干条”作为验收标准，不宣称全库准确率已验证。
- 不扩展新调度平台、无关 UI 改版、日志密钥或 CLI 参数修复。
- 回退保留失败记录和人工历史；不以覆盖旧数据库丢弃后续人工操作。新增迁移须幂等并说明旧版本兼容边界。

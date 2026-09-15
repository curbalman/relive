# Buddy 续修任务：第二轮验收 S1–S5

日期：2026-09-15  
项目：`/Users/david/SynologyDrive/Projects/github/relive`  
性质：继续修复已有实现，补齐回归与当前完整副本验收。

## 1. 承接与目标

必读文档：

- [第二轮验收报告](2026-09-15-identity-profile-recovery-reacceptance.md)：本轮 S1–S5 的代码位置、反例和证据。
- [第一轮返修任务](2026-09-14-identity-profile-recovery-rework.md)：继续有效的接口、调度、测试和交付要求。
- [原始任务](2026-09-14-identity-profile-recovery-and-suggestion-failures.md)：算法边界与完整副本要求。
- [当前交付](2026-09-14-identity-profile-recovery-and-suggestion-failures-delivery.md)：修改完成后更新，不能把历史验证直接当作本轮通过。

上轮直接反例已通过。本轮集中解决：启动核对没有实际生效、首次恢复漏唤醒、超过 100 条漏核对、失败重放重复计数，以及人工恢复安排不完整却返回成功。保持已通过的资格过滤、空画像接入、索引恢复唤醒和前端功能。

开始前核对分支、HEAD 与未提交改动；验收时 HEAD 为 `fec4f46`，业务修改仍在工作区。不要重置、覆盖其他未提交内容。报告行号用于定位，按实际函数复核。

## 2. 执行顺序

1. 将 S2–S5 的四个 `TestReacceptance_*` 反例纳入正式测试，并为 S1 增加真实启动装配回归；先确认修复前失败。
2. 联合完成 S1–S3：初始化顺序、持久化观测语义、可继续推进的分页核对。
3. 完成 S4 的持久化幂等重放与 S5 的可恢复人工安排。
4. 运行完整回归、相关 race 与必要前端验证，再补当前完整副本闭环，更新交付。

反例来源：`/tmp/relive-acceptance-20260915/` 中的 `overlay.json`、两个 overlay 测试源、`repro.log`、`pagination-repro.log`。overlay 测试源包含原测试文件全文，**只提取本轮新增测试与必要 stub**，不覆盖现有文件。临时文件缺失时按验收报告重建。正式测试可随接口合理调整，但必须保留同一业务失败场景与预期。

## 3. S1 / P1：把恢复核对接入真实启动流程

主要文件：`backend/internal/service/service.go`、`person_merge_suggestion_service.go`、`person_identity_profile_service.go`、`identity_profile_coordinator.go`（后四者均在 `backend/internal/service/`）。

- 在共享 ANN、匹配引擎、模型签名、策略及必要 repository 注入完成后，才开始依赖核对。移除当前引擎注入前的无效调用。
- 区分“尚未具备核对条件”“查询失败”和“核对完成且无需唤醒”。前两者保留待办，通过现有后台切片/就绪通知有界补做，不能按成功空结果结束。
- 画像恢复涉及可匹配中心时，必须完成索引激活后才消费恢复事件；ANN 暂未就绪、激活失败或被准入拒绝时不能提前把事件记作已处理。合法空画像按 no_usable_evidence 的状态转移处理。
- 初始化、就绪回调、重启核对使用同一生产装配路径。必要时抽取小范围共享初始化函数，由生产与集成测试共同调用；不为测试另写一套正确顺序。
- 不增加无界重试 goroutine，不等待次日巡检，也不依赖用户点击触发启动补偿。

验收测试：先持久化未解决记录，再模拟画像恢复并在通知前退出；重新走生产实际装配入口后自动核对和重验。分别覆盖模型尚未注入、ANN 尚未 ready、首次查询失败、之后就绪及重复就绪；不得直接在测试里先 SetIdentityMatchingEngine 再手动 Reconcile，以此代替装配回归。

## 4. S2 / P1：明确记录未知、不可用与可用证据

主要文件：`backend/internal/model/person_merge_suggestion_failure.go`、`backend/internal/repository/person_merge_suggestion_failure_repo.go`、`backend/internal/service/person_merge_suggestion_service.go`、`backend/pkg/database/database.go`。

当前 generation=0 混合了“旧记录没有观测信息”和“已经确认该画像不可用”。本轮增加最小持久化观测信息，目标自身及每个阻塞候选使用同一语义，至少能够区分：

| 已记录事实 | 当前事实 | 必须行为 |
| --- | --- | --- |
| 已确认不可用/尚无 generation | 当前 ready、模型匹配且恢复条件满足 | 识别首次恢复，包括 0→1；有界唤醒并记录已消费版本 |
| 已知可用版本 | 当前版本前进且满足恢复条件 | 唤醒相关目标，更新已消费版本 |
| 同一恢复事件已消费 | 重复回调、重复核对或重启 | 不再次重置 attempts，不重复安排工作 |
| 历史未知 | 首次成功核对 | 有界建立观测基线并完成必要重验；持久化处理结果，不每次重启重复赠预算 |
| 任意状态 | 读取失败、模型不匹配、仍 failed/dirty/hidden | 保留待核对或未解决状态，不写成可用、不消费恢复事件 |

不能仅删除 `generation > 0` 判断而丢掉去重语义，也不能继续跳过所有 0。不得从 failed/dirty 残留的高 generation 推断恢复成功；模型/状态与观测版本一起决定是否发生有效恢复。

通过正式幂等迁移引入字段或依赖结构，旧记录保留 attempts、人工历史及失败原因。模型变化的处理必须与已有失效流程一致，不能只比较不同模型下的 generation 整数大小。

验收测试：目标和候选分别覆盖 0→1、failed 残留旧 generation、ready 空画像、重复事件、未知历史数据迁移、读取失败后恢复、通知丢失后重启。正式纳入 `TestReacceptance_ReconcileFirstRecoveryFromZeroGeneration`。

## 5. S3 / P2：有界分页覆盖全部未解决记录

主要文件：`backend/internal/service/person_merge_suggestion_service.go`、`backend/internal/repository/person_merge_suggestion_failure_repo.go` 及必要的持久化扫描状态。

- 内部核对使用稳定主键的 keyset 游标，每批最多 100 条，继续受现有后台切片预算约束。不要复用按 last_failed_at 排序的 UI 分页做内部进度。
- 持久化轮次边界和当前位置，切片结束、暂时失败或进程退出后能够继续。每轮固定上界，避免新增记录让本轮永远无法完成；上界之后的新记录由后续轮次/事件覆盖。
- cursor 仅在该批核对及必要恢复安排可靠保存后推进；查询失败不能越过该批。
- 记录在处理中被 resolve、删除或更新时间变化时，不得导致后续记录被 offset 跳过；已关闭记录不会被过时核对重新打开。
- 对目标和依赖画像的读取同样分批，避免无限 IN 列表。保留 UI failures 接口 page_size≤100 的既有契约。

验收测试：101 条与多页记录、仅最后一页出现有效恢复、处理中插入/resolve、跨切片与重启继续、读取失败不跳页。正式纳入 `TestReacceptance_ReconcileMustVisitBeyondFirst100`，不能仅将 100 改成更大常量使其通过。

## 6. S4 / P1：一个真实尝试只计数一次

主要文件：`backend/internal/service/person_merge_suggestion_service.go`、`backend/internal/repository/person_merge_suggestion_failure_repo.go`、相关 model/迁移与测试。

为一次目标重验建立可持久化、可重放的尝试身份，失败状态更新与该身份的消费标记在同一事务内保存。尝试身份必须跨失败重试和进程重启保持稳定，不用每次调用新生成的时间戳冒充幂等键。

- 同一批第一条写入成功、第二条失败后，重放第一条不再次增加 attempts，不推迟 next_retry_at，也不重复改写人工审计。
- 真正发起下一次重验时使用新的尝试身份，正常消耗预算；不能把“理由/画像版本相同”一概去重，导致真实故障永远不耗尽。
- 在失败记录落库后、cursor 保存前退出，重启后重放仍满足一次计数。仅把本次 for 循环包进事务，不能替代跨提交与进度保存窗口的验证。
- pending 保存、failure resolve、扫描进度和恢复事件消费仍遵循现有提交顺序约束；旧意图不得覆盖更晚的 resolved 状态或新的恢复轮次。
- 所有新增状态通过正式迁移管理。复用现有 WriteQueue，但不把串行写误当作数据库事务。

验收测试：正式纳入 `TestReacceptance_PartialFailureBatchReplayDoesNotBurnBudget`；再覆盖多次重放、服务重建、cursor 写失败、旧意图晚到，以及下一次真实尝试递增至五次后耗尽。检查真实表内容和次数，不只检查返回错误。

## 7. S5 / P2：人工恢复安排可补齐，不能虚报 accepted

主要文件：`backend/internal/service/person_merge_suggestion_service.go`、`backend/internal/repository/person_merge_suggestion_failure_repo.go`、`backend/internal/api/v1/handler/people_handler.go` 及相关测试。API 仍为 `POST /api/v1/people/merge-suggestions/failures/retry`。

- 接受恢复请求前完整读取并验证目标和必要阻塞依赖；读取/JSON 解析失败明确返回错误，不 continue，不把缺失依赖当空列表。
- 使用持久化安排状态区分“待补齐”和“已完成安排”。只有必要恢复步骤已可靠安排，才作为 accepted 返回；重复请求只有在同一轮已经完成安排时才返回 already_scheduled。
- 多目标或多依赖部分安排成功后再失败，下一次请求补齐剩余步骤；已成功部分不能重复 MarkDirty 或刷新预算。
- 进程退出、MarkDirty 报错、保存扫描状态失败、清理人工标记失败均须可恢复。不能只用“尽力清除 ManualRetriedAt”作为失败回滚机制。
- 延续目标资格检查、必要依赖筛选、去重后 1–100 个上限；隐藏/已删除/不合格目标不得安排恢复。waiting_evidence 输入未变仍跳过。
- 继续沿用 accepted_target_ids/skipped 响应契约及现有错误响应。若必须增加状态字段，同步 DTO、TypeScript 与前端文案，明确兼容行为。

验收测试：正式纳入 `TestReacceptance_ManualRetryDependencyReadErrorMustPropagate`；再覆盖 malformed 依赖、多个依赖中途失败、完成安排前退出、并发重复请求、状态保存失败后补齐。断言实际安排到的依赖集合、次数与返回结果一致。前端验证失败提示和再次重试可完成，不显示错误的成功提示。

## 8. 副本验证必须覆盖生产路径

9 月 8 日副本五个样本终态已核对一致，可保留为历史机制证据，但不能证明修复后的启动接线正确，也不能替代当前完整副本门禁。

- 修订 `backend/cmd/relive-identity-recovery-verify` 的验证路径：复用生产的匹配引擎/ANN 装配与核对入口，不在画像恢复后另造一套诊断引擎再手动核对来替代正常通知。
- 主验收通过通用接入、协调器、索引激活、恢复核对、推荐 service→repository 完成。直接 BuildPersonOnce、补发通知、修改队列到期时间等用于专项实验时，必须另列，不能当作自动闭环通过。
- 副本报告的成功标志应对应实际验证的闭环断言；不能仅凭 failed 中心被排除就输出整项 OK。记录快照日期、构建标识/差异摘要、参数、执行路径、前后状态和失败项。
- 补齐当前完整一致性副本的恢复与重启验证，使用真实 embedding、成员及必要关系；不使用精简诊断库、partial 文件或合成生产证据。允许最终 pending=0，但所有未解决状态及恢复转移必须解释清楚。
- 当前备份若仍受阻，先复核可用的只读证据并如实记录。不得把未经独立确认的“活锁”直接当作必须暂停生产的依据；按已有会话授权处理生产操作。备份受阻不妨碍继续完成 S1–S5 和本地测试，但整体验收仍标受阻。

## 9. 测试、交付与完成判定

运行新增正式回归、原 `TestAcceptance_*`、相关恢复/索引/事务路径 race，以及 `cd backend && go test ./...`。本机如默认工具链仍被 Xcode 许可状态阻挡，可使用已验证的 `DEVELOPER_DIR=/Library/Developer/CommandLineTools`；不要替用户接受许可。既有 `TestClusteringEquivalence` 缺字段单列，不删除测试或修改生产库使其通过。

前端运行 People vitest、peopleHelpers Node 测试和 build；如修改人工重试反馈或响应，再做对应浏览器交互回归。不得用上一轮测试日志代替本轮结果。

更新原 delivery，逐项回应 S1–S5：根因、最终修改位置、正式测试名称、实际结果与残余限制。保留迁移幂等与回退说明，不覆盖失败记录或后续人工历史。交付清单必须保持原验收条件，不把“当前完整副本”改写成“9/8 历史副本”后打勾。

- [ ] S1 实际启动装配与就绪后补做通过。
- [ ] S2 首次恢复、目标/候选观测与重复事件去重通过。
- [ ] S3 全页覆盖、跨切片和重启继续通过。
- [ ] S4 部分提交及重启重放不重复计数，真实新尝试仍计数。
- [ ] S5 依赖安排完整、失败可补齐、并发幂等通过。
- [ ] 本轮相关测试、race、前端验证完成，环境失败单列。
- [ ] 当前完整副本按生产等价路径完成闭环验证。
- [ ] 交付、迁移、兼容性和回退说明与最终实现一致。

前述项目未全部完成时，整体保持“续修中”或“验收受阻”。“代码完成 / 当前副本通过 / 生产已部署 / 生产已验证”分别标记，无实际执行的生产项写未执行。

## 10. 不包含与授权边界

本轮不改变评分、0.55 阈值、embedding 或召回预算；不自动合并、不全库重聚类、不解除 hidden/manual_locked/cannot-link，不扩展通用调度平台或无关 UI。

提交、推送、部署及生产写入遵循 Buddy 执行会话已有授权，本文不新增生产操作授权，也不要求重复确认已获授权事项。不默认启动生产人物后台，不以暂停生产或覆盖旧库作为代码返修的前置条件。

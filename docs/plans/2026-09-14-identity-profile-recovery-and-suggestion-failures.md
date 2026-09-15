# primary 画像恢复与推荐失败状态修复任务

日期：2026-09-14  
状态：待实施；本文件是修复任务说明，不代表代码或生产修复已完成。  
仓库：`/Users/david/SynologyDrive/Projects/github/relive`  
证据：[2026-09-14 复查报告](../2026-09-14-identity-primary-audit.md)  
承接：[9 月 9 日修复交付](2026-09-09-identity-primary-zero-suggestions-delivery.md)

## 1. 目标与边界

修复 primary 推荐链路中已证实的三类问题：索引召回与精排读取的画像资格不一致、失败画像不能自行恢复、推荐目标耗尽重试后失去失败状态。保持当前评分公式和阈值，让正常空结果、证据不足、等待恢复、重试耗尽都能被正确处理和解释。

本任务交付代码、回归测试、当前数据副本验证、前端状态展示和部署验收说明。先完成本地实现与可审阅交付；提交、推送、部署、生产画像恢复及启动人物后台遵循执行会话已有授权，不重复询问已授权事项，也不把本文件当作生产操作授权。

**验收允许 pending 仍为零。成功标准是：合格候选能保存，技术失败不冒充成功，无证据不忙重试，恢复后能自动续跑。** 不以新增推荐数量作为硬性目标。

## 2. 已验证基线

以下均为 9 月 14 日观测值，实施前复核，不能写死为业务常量：

- 线上为 `primary`，存在 `identity-engine-v3` 真实输出，suggest=0.55。9 月 10 日生成的 #20855/#20856 共四个候选已经人工审核合并，来源人物已不存在；不要再要求它们在当前库重新成为推荐。
- 172 个合格巡检目标中，167 个有可用画像；264975、356935 为 failed，306601、307945、562545 为 ready 但没有活动中心。
- ANN 装入 14146 个中心，其中 44 个来自 5 个未隐藏 failed 人物：264975 有 40 个，265437、266177、266194、269183 各 1 个。精排查询要求 ready，因此无法读回这些候选证据。
- 共有 24 个 failed 画像，其中 21 个未隐藏；历史错误为 `replace generation: database is locked`。1532 个 dirty 画像全部属于隐藏人物，不是应强制清空的工作积压。
- 当前副本用实际 Go 引擎复算：仅移除上述 44 个 failed 中心，候选证据不完整目标从 67 降至 0；两组的自身画像不可用目标均为 5，推荐策略接受均为 0。
- 线上 9 月 11–13 日每轮留下 69 个重试目标，耗尽后 retry_targets 消失。该数字与副本的 67 不同口径，也受内存 ANN 与重建图差异影响，不要求精确相等。
- 人物运行租约为空闲，195 张 pending 人脸未有聚类记录，新版自动归属批次为零。这说明自动聚类还未完成运行验收，不证明算法应自动启动后台。

## 3. 工作一：统一画像可匹配资格

### 3.1 唯一资格规则

可进入当前画像匹配的中心必须同时满足：人物存在且未隐藏；profile 为 ready；模型签名匹配；active_generation>0；中心 generation 与活动代次一致；向量合法且满足既有读取条件。保留历史 generation 用于审计/回滚，不代表它可继续参与当前匹配。

统一检查以下路径，复用共享查询条件/校验函数，防止继续维护不同口径：

- `person_identity_profile_repo.go`：`ListAllActiveCenters`、`ListActiveCentersByPersonIDs`，以及为增量激活读取画像的路径。
- `person_identity_profile_service.go`：`activateANN`；`identity_profile_coordinator.go`：`activateOne`、批末索引更新。
- `person_identity_profile_ann.go`：全量图、delta、ExactTopPeople、generation/invalid 过滤。
- `identity_matching_engine.go`：目标/候选证据加载与状态分类。

不只给全量查询补一个 WHERE。ready→dirty/building/failed、隐藏、删除或模型失配时，旧中心应立即失效；并发构建完成时必须再次核验当前状态/代次，防止旧构建结果重新激活已失效人物。清理只失效索引资格，不删除人工脸、历史中心或人工约束。

评分前必须保留对当前 DB 证据的校验；索引快照可能落后于 DB，不能仅凭 ANN 返回结果就接受归属。若在并发窗口内证据变化，返回明确的可恢复不完整状态，不能降级旧算法或按空结果完成。

### 3.2 版本与生效

将本次匹配资格语义变更纳入引擎版本/配置指纹，沿用既有版本失效与建议重验机制。新版启动后通过现有协调器构建新索引；成功前等待就绪。旧建议按版本标记需重验，保留 applied/dismissed 历史，禁止复活已合并或已拒绝的候选。

## 4. 工作二：失败画像有界恢复，区分合法空画像

### 4.1 自动恢复

在 `person_identity_profile_service.go` 与 `identity_profile_coordinator.go` 内实现恢复调度，继续使用后台 DB、准入控制、现有 worker 数、dirty batch 和 slice budget；所有状态/代次写入继续通过 WriteQueue 串行提交。不另启一个不受管理的重试 goroutine。

- 为画像恢复持久化 `recovery_attempts`、`recovery_next_at`、`recovery_exhausted`、稳定错误类别与最近失败时间；通过模型及正式迁移加入字段，不能只存在内存。
- 未隐藏、仍有脸的 failed 画像进入有界恢复；瞬时锁冲突/读取失败可重试。采用 5 次预算、300 秒起步的指数退避、3600 秒上限；仅实际构建尝试消耗预算，准入拒绝/服务暂停不消耗。
- 历史 failed 缺少恢复元数据时，由有界扫描幂等接入。每个切片总工作量仍受原 batch/budget 约束；不得每次启动把全部 failed 重置为 dirty，也不得只扫描当前推荐目标而遗留其他失败候选。
- 耗尽后保存原因与耗尽状态，不在每日巡检或进程重启时自动重置预算。新的相关人脸证据、有效模型/构建版本变更或明确人工重试可开启新一轮预算；普通轮询和相同 dirty 通知不可无限刷新预算。
- 画像恢复成功后提交新 generation，失效旧索引中心并通过正常索引路径激活；只有达到可读取、可查询状态后才唤醒依赖它的推荐目标。
- 手工排除、hidden、manual_locked、cannot-link 规则原样保留。隐藏人物不参与自动恢复；人物已删除则关闭关联恢复工作，不构造空人物。

恢复预算与人脸身份匹配的 `faces.retry_count` 分离：数据库故障不得消耗“不同人/未匹配”的重试次数，也不得触发身份阈值衰减。

### 4.2 ready 但没有中心

不能简单地把所有空画像永久标失败或全部认定为正常。先使用现有 `ListProfileFaces` 与真实 builder 对三个样本做有界复核，输出输入脸数、有效向量数、可用证据数、中心数和稳定原因类别，不输出 embedding 或照片路径。

为构建结果增加持久化 `build_outcome`：旧值空串表示未分类；`usable` 表示有可匹配中心；`no_usable_evidence` 表示读取/计算完整成功但现有输入不足以形成中心。保留现有 profile.status 枚举，避免为了无证据扩大 SQLite CHECK 状态集合。

- 读取失败、计算异常、generation 不一致：技术失败，按恢复流程处理。
- 输入完整且真实构建为空：记录 `no_usable_evidence`、原因及证据版本，退出技术重试，等待相关证据变化。
- 旧 ready 空画像必须先有界核验，不能靠中心数为零直接补写“正常无证据”。
- 同一证据版本下已确认无证据的画像不反复重建；证据或模型变化时重新评估。
- 引擎把已确认无证据映射为明确的证据不足原因，service 记录“等待证据”，不映射为有效 no_candidate，也不按技术故障消耗预算。组件自动归属同样不得使用空画像兜底。

## 5. 工作三：独立保存推荐失败，恢复后自动重验

### 5.1 持久化契约

新增 `person_merge_suggestion_failures` 表及 repository，以 `target_person_id` 唯一，保存当前未解决目标的状态。不能依赖 pending 建议是否存在，也不能继续用无限增长的日志或一段文案充当失败记录。

至少保存：`state`（retry_wait/exhausted/waiting_evidence/resolved）、`reason_code`、`attempts`、`next_retry_at`、`first_failed_at`、`last_failed_at`、目标证据代次、引擎/配置指纹，以及阻塞候选的人物 ID 与已观测代次。关闭记录时写 resolved、resolved_at 与 resolved_reason，排除在未解决计数之外；每目标保留一条当前/最近记录，重新失败时幂等更新，不逐次尝试无限插入。依赖集合受当前候选上限约束，必要时分页存储；不能静默丢依赖而导致永不恢复。建立 target 唯一索引及 state/next_retry_at 调度索引。

该表成为未解决目标状态和重试调度的唯一来源。`people.merge_suggestions.state` 继续存 pause、dirty、cursor、last_run 等扫描状态；旧 retry_targets 在正式迁移中幂等导入失败表，成功后才停止使用旧调度字段。迁移不丢 attempts/时间；新逻辑不再双写两份可分歧的失败清单。已经被旧版本清掉的失败无法凭空还原，升级后的有界完整巡检负责重新发现。

### 5.2 状态转移

| 事件 | 必须行为 |
| --- | --- |
| 技术不可用且无接受候选 | upsert retry_wait；禁止空替换现有 pending；主 cursor 可继续 |
| 有接受候选但部分证据缺失 | 保存有效结果并保留失败记录；不把整个目标标为完整成功 |
| 画像完整但无可用证据 | waiting_evidence；不进行定时技术重试 |
| 尝试达到预算 | exhausted；停止自动忙重试，保留失败与最后原因 |
| 完整重验成功、确实零候选 | 正常空替换；关闭该目标未解决记录 |
| 完整重验成功、有候选 | 持久化建议成功后关闭该目标未解决记录 |
| 目标不再合格（删除、隐藏、无脸、分类改变） | 按现有资格关闭记录并保留计数原因，不算技术成功 |
| 阻塞画像恢复或证据版本改变 | 去重唤醒相关目标，进入有界重验；不等待下一轮 24 小时巡检 |
| 普通每日扫描、重启或重建索引但证据未变 | 不抹掉 exhausted，不重新赠送重试预算 |

明确“目标自身不可用”和“召回候选不可用”的原因及依赖人物；不要把所有错误都叫 profile_unavailable。唤醒必须包含候选画像恢复，不能只监听 target profile。启动后有界核对持久化依赖版本，以补偿进程退出导致的通知丢失。

索引不可用另行记录就绪依赖：从不可用恢复到可用时唤醒相关目标，即使 profile generation 没变也要恢复；普通成功重建的 generation 自增不能反复刷新耗尽预算。数据库读取类错误通过预算内退避恢复，耗尽后保留可见状态及人工重试入口，不能凭未经核验的“服务健康”清除失败。

建议写入、失败记录关闭和 cursor 推进采用事务或可证明幂等的提交顺序；写入失败/并发 MarkDirty/服务退出不能造成“失败已清除、建议却没保存”。继续遵守 DirtyGeneration、防并发前台写和 WriteQueue 边界，避免持锁重入造成死锁。

stale pending 仍必须阻止未经重验的 apply。部分结果保存时，不能把尚未解决的目标状态隐藏掉。父建议行/候选项的引擎版本、指纹、目标和候选代次继续完整落库。

## 6. 工作四：通过现有任务页展示与操作失败

复用现有推荐任务页和 API，不另建后台管理系统。以下是新增契约，实施时同步 Go DTO、TypeScript 和 handler：

- `GET /api/v1/people/merge-suggestions/task`：保留现有字段；新增 `retry_exhausted_count`、`waiting_evidence_count`、`unresolved_target_count`。retry_due_count 与 retry_deferred_count 必须互斥；retry_total_count 仅指预算内技术重试，unresolved 为所有未解决目标的去重总数。
- `partial = unresolved_target_count > 0`，与是否有 pending、dirty 是否为 false、调度器是否 idle 无关。暂停状态与问题计数同时可见，不被“部分完成”覆盖。
- 新增 `GET /api/v1/people/merge-suggestions/failures?page=1&page_size=20`：沿用项目分页响应，page_size 上限 100；每项返回目标 ID、状态、稳定原因、尝试数、下次时间、阻塞人物 ID 和最近失败时间。详情只在用户展开时加载。
- 新增 `POST /api/v1/people/merge-suggestions/failures/retry`，请求 `{ "target_ids": [264975] }`，去重后 1–100 个；返回 accepted_target_ids 与 skipped 项及原因。操作仅对选中未解决目标及必要依赖安排恢复/重验，不全库重建、不解除人工规则、不自动合并；连续点击幂等，不重复创建活跃工作。
- 新路由放在 `/merge-suggestions/:id` 之前，沿用现有管理员认证/响应规范。人工重试可重新开启所选耗尽预算，但必须记录原因；真实无证据若输入未变应返回 waiting_evidence 提示，避免用户点击制造循环。

显示文案至少区分“等待画像恢复”“重试已耗尽”“等待更多有效证据”“巡检完成，无达标候选”。即使 pending=0 也能看到未解决计数和详情。人物识别/聚类后台状态与推荐巡检状态分别展示，复用已有 `/people/task` 及后台启动入口，不把启动推荐巡检解释为启动自动聚类。

## 7. 工作五：补齐诊断和有界性能验证

增强现有 `identity_merge_diagnose.go` 与 `cmd/relive-merge-suggestion-diagnose`：区分目标不可用、候选缺失及 missing person IDs；增加 TopK 参数，默认与生产 service 的 50 对齐，保留旧报告 TopK=20 的显式复现方式。

记录互斥的阶段主分类及必要附加标志：目标资格、目标可用、召回数、缺失候选数、精排数、硬阻断、低于阈值、技术失败、接受、实际写入/未改变、未解决与耗尽。指标必须说明单位；insufficient 与 below_threshold 可能重叠，不能把两者直接相加充当漏斗。

单目标计时拆为画像加载、ANN/ExactK 召回、精排、约束、持久化；记录中心数、查询数与总耗时。只聚合/有界采样，不能每 tick 全量打印失败数组，也不输出密钥、向量和图片路径。

保持 ANNK=50、ExactK=100、MaxCandidates=200 及所有匹配阈值。本轮不通过缩小召回范围、任意截断大人物中心或改评分来换取速度。先消除无效目标的重复完整计算；比较固定输入下的有效扫描开销与失败重试开销，分别给出结果。

## 8. 重点代码范围

| 范围 | 文件（仓库根目录相对路径） |
| --- | --- |
| 画像资格与恢复 | `backend/internal/repository/person_identity_profile_repo.go`、`backend/internal/model/person_identity_profile.go`、`backend/internal/service/person_identity_profile_service.go`、`backend/internal/service/identity_profile_coordinator.go`、`backend/internal/service/person_identity_profile_builder.go` |
| 索引与引擎 | `backend/internal/service/person_identity_profile_ann.go`、`backend/internal/service/identity_matching_engine.go`、`backend/internal/service/identity_matching_types.go` |
| 推荐状态与持久化 | `backend/internal/service/person_merge_suggestion_service.go`、`backend/internal/service/merge_suggestion_retry.go`、`backend/internal/repository/person_merge_suggestion_repo.go`；新增 failure model/repository |
| 注入与迁移 | `backend/internal/service/service.go`、`backend/pkg/database/database.go`、现有 repository 构造/迁移入口 |
| API/DTO | `backend/internal/model/dto.go`、`backend/internal/api/v1/handler/people_handler.go`、`backend/internal/api/v1/router/router.go` |
| 前端 | `frontend/src/api/people.ts`、`frontend/src/types/people.ts`、`frontend/src/views/People/index.vue`、`frontend/src/views/People/peopleHelpers.ts` 及既有测试 |
| 诊断 | `backend/internal/service/identity_merge_diagnose.go`、`backend/cmd/relive-merge-suggestion-diagnose/main.go` |

## 9. 必须通过的回归与副本验收

1. ready/dirty/building/failed、隐藏、删除、模型/代次失配的全量索引、delta、精排资格一致。包括“有旧活动中心的 failed”而不只是没有中心的失败样本。
2. 真实 repository + ANN + engine 集成复现：失败候选造成 incomplete；修复后不再召回该候选；并发 ready→failed 时安全失效；恢复成功后重新可召回。
3. failed 恢复：历史状态幂等接入、锁冲突退避、准入拒绝不耗预算、重启不重置、耗尽停跑、隐藏/删除退出、提交时人物变化不激活过时结果。
4. 空画像：读取失败不能写 no_usable_evidence；真实空构建不无限重试；旧空画像先核验；证据变化后重新构建；不改变人工排除与锁定。
5. **pending=0 的失败目标**经历五次尝试、耗尽、重启、每日巡检后仍显示未解决；不是只测已有 pending 的 stale 字段。
6. 目标或候选画像恢复后，相关等待/耗尽目标被有界唤醒；完整零结果关闭失败；部分候选成功仍保留问题；重复通知、并发人工重试不产生重复工作。
7. 事务失败、cursor 并发推进、旧 JSON 重试导入及迁移重跑：不丢失败、不错误覆盖 pending、不损坏人工历史；GET 详情与计数一致。
8. 有效候选实际经 service→repository 保存并查询为 pending，元数据完整；真实无达标候选则为解释充分的零结果，不能在测试中伪造推荐来满足验收。primary 不隐式回退；legacy/shadow/rescue 的既有契约和人工反馈保护测试不回归。
9. 前端覆盖 idle+exhausted、paused+partial、waiting_evidence、正常空结果、详情分页与人工重试；新增调用不使 People 首屏重新叠加重查询。
10. 副本用当前代码与修复代码对照，先不恢复 failed，隔离资格修复效果：失败中心污染造成的 incomplete 应为零，自身 5 个不可用目标仍明确分类。再在另一份**完整副本**恢复画像，逐个解释失败/空画像与最终推荐结果。

当前 `/tmp/relive-audit-20260914/` 是临时精简诊断副本，缺少真实人脸 embedding、完整 face 字段、中心成员等输入，且 sum_embedding 已移除，**只能验证推荐召回/精排，不能用于画像重建或自动聚类验收**。恢复测试使用正式一致性完整备份，或从一致性读事务导出真实 builder/聚类所需全部字段与关系；不可合成生产证据补齐。临时文件可能已不存在，缺失时重新采集。

运行针对性 Go 测试，再执行 `cd backend && go test ./...`；对新增恢复调度、索引激活和事务路径运行有针对性的 `-race`。前端运行既有相关测试及 `npm run build`。记录环境失败、既有失败和本次回归，不能把旧交付测试结果当作本次通过。

## 10. 部署、运行验证与回退

本地交付先列明代码、迁移、兼容性、测试、副本结果、实际性能和未解决事项。部署阶段：保存旧镜像/配置及一致性 SQLite `.backup` → 执行正式迁移和明确构建产物部署 → 核验 primary/阈值与新引擎标识 → 验证新索引资格 → 观察有界画像恢复与目标重验 → 查询失败表、pending 及 UI。不通过手工 DELETE 生产中心模拟本地对照。

数据库备份、诊断和版本检查使用已验证命令；服务端版本参数为 `/app/relive -version`，**不是** `relive version`。只读诊断默认副本和只读连接，禁止实例化会运行启动清理的完整服务；认证信息不进入命令输出或报告。

自动聚类另做有界副本验证：从当前待处理人脸中固定小批输入，经实际 primary 路径检查 assignment batch/change、未归属原因及人工约束保护。生产 `/api/v1/people/background/start` 会持续消费队列，**不是**小批 dry-run；需要启动线上后台时，先在交付中写清当前整个队列规模、停止方式和影响，按执行会话授权执行。不能默认启动后只会处理所选若干张脸，也不把它作为证明推荐修复的必要条件。

回退保留新增恢复/失败记录及人工历史。画像恢复会改变派生 generation，旧版本可能再次把 failed 历史中心装入索引，回退验收必须检查资格及 stale 防护，不能只换镜像后宣布安全。必要时暂停受影响任务再回退；若运行过自动归属，回退镜像不撤销归属，应使用已有批次预演/撤销能力并单独处理。不要覆盖恢复整个旧数据库而丢弃之后的人工操作。

## 11. 不包含

- 调低 0.55 推荐阈值、更换 embedding、修改双向/单向评分规则、重做中心聚合或引入旧算法回退。
- 全库历史重聚类、自动执行合并建议、恢复隐藏人物参与、解除人工锁或 cannot-link。
- 自动恢复人物后台开机自启、扩展全新任务调度系统、为了性能任意删中心或降低召回预算。
- 将之前四对已审核样本强行重新生成；宣称本任务验证了全库准确率/召回率。
- 日志明文密钥与 CLI 参数防误启动的实现修复：它们已记录在复查报告，另立独立任务；本任务操作必须遵守安全诊断约定，不自动轮换凭证。

最终交付逐项标记“本地已实现 / 副本已验证 / 生产已部署 / 生产已验证”。未做生产操作时明确留空对应门禁，不将文档、测试或镜像构建当作上线完成。

# NAS 运行巡检（2026-10-22）

采集时间：北京时间 18:47–18:56。范围：NAS 容器、近 24 小时及近 6 天保留日志、生产 SQLite 只读查询、相关源码、一个既有失败测试复验。

结论：相框主链路正常；存在后台任务收尾、统计口径、租约丢失处理和重复后台工作等问题。没有发现本次观测窗口内的 HTTP 5xx、容器反复重启或 OOM。

本次未修改生产数据、配置或服务，未启动分析、重建、恢复任务，未部署。新增本报告；原始日志留在本机 `/tmp/relive-audit-20261022/`，未加入 Git。

## 1. 运行基线

| 检查 | 实际结果 |
| --- | --- |
| 生产镜像 | `sha256:6995ec72d714116a67656db447522a6e514749e005e4b8a4412452f2335b2f4a` |
| 业务提交 | OCI revision `6413beef29807b5c5c06563fcca0a7a7fbb5a0f7`，与 9 月部署记录一致 |
| 本地 HEAD | `45f76426`；开始巡检时工作区干净 |
| 应用版本 | 1.8.0；health=healthy、readiness=ready |
| 容器 | relive、relive-ml 均 healthy；relive 从 10 月 17 日 04:46 运行至今，RestartCount=0、OOMKilled=false |
| 资源快照 | NAS load1=0.24，可用内存约 5.3 GiB；卷使用率 81%，剩余约 1.4 TiB；relive 内存约 1.805 GiB |
| 数据库 | 主文件约 2.4 GiB，WAL 约 34 MiB；本次只读查询成功，未运行全库完整性扫描 |
| 相框 | 近 24 小时 14 次 `/api/v1/device/display.bin` 请求，全部 200，耗时约 10–94 ms |
| 每日展示 | 10 月 18–22 日五个批次均 ready，每批 10 项；当天批次已在 00:50 生成 |
| 照片 | active=130643；所有 active 照片的 thumbnail_status=ready（数据库状态，未逐个检查缩略图文件） |

近 24 小时应用日志有 7 条 WARN，全部为正常内部状态键 `people.merge_suggestions.state` 被当作 non-standard config key；没有 ERROR/PANIC。ML 日志没有 WARN/ERROR。近 6 天日志中 HTTP 状态为 200=17996、304=20、401=8，无 5xx；401 集中在两次前端会话访问，不能据此判定攻击或业务故障。

## 2. 已确认的问题

### P2：缩略图任务长期滞留 processing

- `thumbnail_jobs.id=122957`，photo_id=130794，started_at=2026-07-31 19:39:15+08:00，attempt_count=1，completed_at 为空。
- 对应照片仍 active，原图文件存在，照片自己的 thumbnail_status 已是 ready。
- `thumbnail_job_repo.go:77` 的领取逻辑只处理 pending/queued；`thumbnail_service.go:293` 遇到已有 processing 任务只更新请求信息，不恢复状态。该任务既不会被再次领取，也不属于终态清理范围。
- 影响：任务统计长期显示处理中；以后通过队列强制重建该照片时，可能复用这条无法被领取的任务。不能将它解释为实际仍在生成缩略图。
- 建议：增加启动/周期性超时恢复，并核对已生成产物后关闭旧任务；用进程中断及再次入队回归测试覆盖。

### P2：GPS 待办统计与实际处理资格不一致

- active 照片中，geocode_status=pending 共 550 张：69 张缺少坐标，481 张为 `(0,0)`；另外 3 张 failed 也都是 `(0,0)`。
- 按 Dashboard 当前 SQL 计算 geocode_pending=481；套用后台实际有效坐标条件后，待处理数为 0。`geocode_jobs` 当前为空。
- `photo_repo.go:1063` 和 `:1349` 一带的聚合仅排除 NULL，未排除 `(0,0)`；`geocode_task_service.go:600` 的后台补齐却明确排除 `(0,0)`。
- 影响：界面上的这批待解析不会靠等待自动清零；用户可能误判后台停滞。
- 建议：统一统计、资格筛选和状态归一化规则，并分别呈现无坐标与解析失败。

### P2：AI 丢失运行租约后持续心跳，缺少停止或恢复策略

- 保留日志中，10 月 16 日 18:52:11 至 10 月 17 日 04:17:41，有同一 background owner 的 `analysis runtime owned by other` 心跳 WARN 共 3393 次，间隔约 10 秒。
- `ai_service.go:997` 的 keepRuntimeLeaseAlive 在 HeartbeatGlobal 返回错误后只记日志，继续循环，没有将所有权丢失传回工作循环。
- 这证明历史上存在长期重复告警；**不证明发生重复 AI 请求或额外计费**。
- 当前重启后的窗口没有再出现同类心跳告警，global_analysis/global_people 租约表均为 idle；因此不是当前持续故障，但代码缺口仍在。
- 建议：区分暂时数据库错误和明确所有权丢失；后者应结束旧 worker 并暴露可恢复状态，覆盖竞争/过期/停止测试。

### P2：画像统计把隐藏人物残留算作待处理和失败

- dirty=1532、failed=3，**全部属于 hidden 人物**；当前可见人物中 dirty=0、failed=0。
- 三条 failed 是 7 月的 `replace generation: database is locked`，并非当前数据库锁告警。
- `person_identity_profile_repo.go:191` 和恢复资格查询明确排除 hidden；`:835` 的 GetStats 却对全部画像按 status 计数。
- 影响：统计数无法代表可执行积压，持续给出“1532 待处理、3 失败”的误导信号。9 月报告把这部分泛称历史积压，本次关联 people.hidden 后进一步确认了原因。
- 建议：将可执行待办与隐藏人物保留记录分开统计，保留历史数据，不为清零而强制重建隐藏人物。

### P3：无变化扫描和小事件反复计算

- 近 24 小时，同一 iPhone/2026 子树完成扫描 24 次，每次 total=1836，new/updated/deleted/skipped 均为 0。
- 事件聚类发现同样的 770 张未归类照片共 27 次（包含让路后的重复发现），每次形成 524 个候选 cluster；数据库仍为 770 张未归类。
- `event_clustering_service.go:658` 每次读取 event_id=NULL 的照片，`:795` 遇到未达到最少照片数的 cluster 直接跳过，未记录复查条件。这与重复出现的现象一致；未逐组重跑聚类来证明全部 770 张的具体跳过原因。
- 影响主要是冗余 I/O 和计算。当前每轮聚类约几百毫秒，不能据此称为高负载事故。
- 建议：无业务变化时减少下游触发；为暂不满足条件的 cluster 记录输入版本或复查时间，同时保证新照片到来后仍可重新组合。目录为何持续被判为变化，本轮未追到文件系统层原因。

## 3. 当前待处理事项，不能直接定性为调度故障

- 人脸任务 id=519481、photo_id=48988，自 10 月 17 日 04:49 起 queued，attempt_count=0，无 worker/started_at。原图文件存在。
- photo_id=131200 仍未 AI 分析（10 月 5 日入库）；缩略图及位置状态 ready，原图存在，没有分析锁或失败记录。
- 当前两类运行租约都是 idle，最近 24 小时 ML 仅有健康请求，代码中两类后台分析依赖显式启动，未发现服务启动时自动恢复它们的调用。
- 因此可确认“有待办但当前没有处理证据”；可能是用户有意停止或重启后未再次启动，不能仅凭 queued 判为死锁。本次没有擅自恢复分析。
- 另有 6 张 active 照片保持 face_process_status=failed（7–8 月记录）；对应失败 job 已不存在，当前日志不足以还原原始失败原因。
- 4425 张 face_process_status=none 的已分析照片均 people_excluded=true，是显式排除，不属于遗漏。

## 4. 上次身份恢复修复的当前结果

- ANN 最近成功构建时间：10 月 21 日 10:18:20，generation=4，centers=14161；重启后及后续重建都有成功日志。
- 合并推荐最新自然巡检完成于 10 月 22 日 10:50:10；paused=false、dirty=false、cursor_target_id=0。
- pending 推荐为 0；失败账本有 30 条 resolved、3 条 waiting_evidence，无 retry_wait/exhausted。
- 三个 waiting_evidence 目标为 306601、307945、562545，reason_code=no_usable_evidence、attempts=0。这是缺少可用证据，不是技术失败被伪装为空推荐。
- 另发现 1 条没有对应 people 行的 ready 画像（person_id=347894，活动中心数 0），为派生数据残留，优先级低。

## 5. 性能与日志质量

近 6 天存在成功但偏慢的真实请求：10 月 21 日 move-faces=9.71 秒；热门标签接口最高 4.35 秒，people/stats=4.19 秒，人物列表=3.08 秒。这些是日志中的个别样本，未测并发、分位数，也未定位 SQL/锁/缓存根因，不能宣称所有请求都慢。

启动时还出现一次 `Identity profile background DB unavailable; background building disabled`。装配代码先用 nil 创建服务、再 SetBackgroundDB，之后 ANN 和画像均有推进证据，因此该警告不能单独作为实际后台不可用的证据。内部配置键 non-standard WARN 也应避免在正常巡检中反复出现。

## 6. 本地测试复验与边界

运行：`cd backend && go test ./internal/service -run '^TestClusteringEquivalence$' -count=1`。

结果：FAIL，`Failed to list pending faces: no such column: identity_retry_after`；初始化还报告本地库没有 people_merge_jobs 表。该测试直接打开 `backend/data/relive.db`，依赖本机旧数据库 schema，未建立隔离的最新测试数据。这是之前已记录、现在仍可复现的测试基础设施问题，**不是 NAS 缺表/缺字段的证据**。本次未运行全量测试、未修改测试或业务代码。

本轮直接请求了 NAS health/readiness；管理接口未进行新的登录认证，相关业务结论依据只读 SQL、日志及源码，不宣称已完成管理页面端到端验收。未检查每张照片、所有备份可恢复性或外部 AI 服务可用性。

建议修复顺序：缩略图任务恢复与 GPS 统计 → AI 租约丢失处理 → 画像统计口径 → 后台启停恢复策略及重复计算 → 测试隔离与慢接口定位。

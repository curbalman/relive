package model

import "time"

// 合并推荐未解决目标的持久化状态。与 pending 建议行解耦：pending=0 时仍可保留失败记录。
const (
	PersonMergeFailureStateRetryWait       = "retry_wait"
	PersonMergeFailureStateExhausted       = "exhausted"
	PersonMergeFailureStateWaitingEvidence = "waiting_evidence"
	PersonMergeFailureStateResolved        = "resolved"
)

// 关闭失败记录的稳定原因。
const (
	PersonMergeFailureResolvedCompleteSuccess = "complete_success"
	PersonMergeFailureResolvedZeroCandidate   = "zero_candidate"
	PersonMergeFailureResolvedIneligible      = "target_ineligible"
	PersonMergeFailureResolvedManualRetry     = "manual_retry_reset"
)

// PersonMergeSuggestionFailure 以 target_person_id 唯一，保存当前未解决（或最近已解决）目标状态。
// 不依赖 pending 建议是否存在；重新失败时幂等更新同一行。
type PersonMergeSuggestionFailure struct {
	ID                uint       `gorm:"primarykey" json:"id"`
	CreatedAt         time.Time  `json:"created_at"`
	UpdatedAt         time.Time  `json:"updated_at"`
	TargetPersonID    uint       `gorm:"not null;uniqueIndex:idx_pmsf_target" json:"target_person_id"`
	State             string     `gorm:"type:varchar(32);not null;index:idx_pmsf_state_next,priority:1;check:chk_pmsf_state,state IN ('retry_wait','exhausted','waiting_evidence','resolved')" json:"state"`
	ReasonCode        string     `gorm:"type:varchar(64);not null;default:''" json:"reason_code"`
	Attempts          int        `gorm:"not null;default:0" json:"attempts"`
	NextRetryAt       *time.Time `gorm:"index:idx_pmsf_state_next,priority:2" json:"next_retry_at,omitempty"`
	FirstFailedAt     time.Time  `json:"first_failed_at"`
	LastFailedAt      time.Time  `json:"last_failed_at"`
	TargetProfileGen int `gorm:"not null;default:0" json:"target_profile_generation"`
	// TargetObsKnown：true=已观测过目标画像（含确认不可用 gen=0）；false=历史未知，首次核对只建基线+必要重验。
	TargetObsKnown bool `gorm:"not null;default:false" json:"target_obs_known"`
	// TargetConsumedGen：已消费的恢复代次；同代次重复核对/回调不得再赠预算。
	TargetConsumedGen int    `gorm:"not null;default:0" json:"target_consumed_generation"`
	EngineVersion     string `gorm:"type:varchar(50);not null;default:''" json:"engine_version,omitempty"`
	ConfigFingerprint string `gorm:"type:varchar(64);not null;default:''" json:"config_fingerprint,omitempty"`
	// BlockingDepsJSON：阻塞候选 [{person_id, generation, obs_known, consumed_gen}, ...]，受候选上限约束，禁止静默丢依赖。
	BlockingDepsJSON string `gorm:"type:text;not null;default:'[]'" json:"blocking_deps_json"`
	// IndexReadyRequired：索引不可用时的就绪依赖；索引恢复后即使 generation 未变也要唤醒。
	IndexReadyRequired bool       `gorm:"not null;default:false" json:"index_ready_required"`
	ResolvedAt         *time.Time `json:"resolved_at,omitempty"`
	ResolvedReason     string     `gorm:"type:varchar(64);not null;default:''" json:"resolved_reason,omitempty"`
	// ManualRetriedAt / ManualRetryReason：人工重开预算的审计痕迹（与 ReasonCode 解耦）。
	ManualRetriedAt   *time.Time `json:"manual_retried_at,omitempty"`
	ManualRetryReason string     `gorm:"type:varchar(128);not null;default:''" json:"manual_retry_reason,omitempty"`
	// LastAttemptKey：最近一次已消费的技术失败尝试身份；同 key 重放不增加 attempts。
	LastAttemptKey string `gorm:"type:varchar(96);not null;default:''" json:"last_attempt_key,omitempty"`
	// ManualArrangePending / ManualArrangeRemainingJSON：人工重试安排断点（待 MarkDirty 的人物 ID）。
	ManualArrangePending       bool   `gorm:"not null;default:false" json:"manual_arrange_pending"`
	ManualArrangeRemainingJSON string `gorm:"type:text;not null;default:'[]'" json:"manual_arrange_remaining_json"`
}

func (PersonMergeSuggestionFailure) TableName() string {
	return "person_merge_suggestion_failures"
}

// PersonMergeFailureBlockingDep 是失败记录中的一个阻塞候选依赖。
type PersonMergeFailureBlockingDep struct {
	PersonID    uint `json:"person_id"`
	Generation  int  `json:"generation"`
	ObsKnown    bool `json:"obs_known,omitempty"`
	ConsumedGen int  `json:"consumed_gen,omitempty"`
}

// PersonMergeFailureCounts 供任务页计数：retry_wait 拆为 due/deferred 互斥。
type PersonMergeFailureCounts struct {
	RetryDue          int64 `json:"retry_due_count"`
	RetryDeferred     int64 `json:"retry_deferred_count"`
	RetryTotal        int64 `json:"retry_total_count"` // 仅预算内技术重试（retry_wait）
	RetryExhausted    int64 `json:"retry_exhausted_count"`
	WaitingEvidence   int64 `json:"waiting_evidence_count"`
	UnresolvedTargets int64 `json:"unresolved_target_count"` // 全部未解决去重总数
}

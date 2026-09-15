package service

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/davidhoo/relive/internal/model"
	"github.com/davidhoo/relive/internal/repository"
	"github.com/davidhoo/relive/pkg/config"
)

// MergeSuggestionDiagnoseRequest 限定诊断范围与成本。仅按目标 ID 跑；不写推荐表。
type MergeSuggestionDiagnoseRequest struct {
	// TargetIDs 必填；建议小批量（诊断基线为 2 个目标）。
	TargetIDs []uint
	// FocusCandidates 目标 → 重点候选（如排障四对）；即使 ANN 未召回也会做精确 ComparePeople。
	FocusCandidates map[uint][]uint
	// Strategy 人工推荐策略；空则用默认 suggest 策略字段。
	Strategy IdentityStrategy
	// Recall 召回预算；零值走 DefaultIdentityRecallOptions。
	// TopK 默认与生产 mergeSuggestionProfileK(=50) 对齐；旧报告复现请显式传 20。
	Recall IdentityRecallOptions
	// FailureRepo 可选；注入后报告附带失败表 unresolved/exhausted 只读聚合。
	FailureRepo repository.PersonMergeSuggestionFailureRepository
}

// MergeSuggestionPrimaryStage 是单目标互斥主分类（一个目标恰好一个）。
// insufficient 与 below_threshold 可能同时出现在附加标志里，不得相加充当漏斗。
const (
	DiagnoseStageIneligible          = "ineligible"
	DiagnoseStageTargetUnavailable   = "target_unavailable"
	DiagnoseStageIndexUnavailable    = "index_unavailable"
	DiagnoseStageNoUsableEvidence    = "no_usable_evidence"
	DiagnoseStageIncompleteEvidence  = "incomplete_evidence"
	DiagnoseStageNoCandidate         = "no_candidate"
	DiagnoseStageTechFailure         = "tech_failure"
	DiagnoseStageHardBlocked         = "hard_blocked"
	DiagnoseStageBelowThreshold      = "below_threshold"
	DiagnoseStageStrategyRejected    = "strategy_rejected"
	DiagnoseStageAccepted            = "accepted"
)

// MergeSuggestionStageStats 阶段计数。单位在字段注释中标明。
type MergeSuggestionStageStats struct {
	EligibleTargets            int `json:"eligible_targets"`              // 目标
	IneligibleTargets          int `json:"ineligible_targets"`            // 目标
	ProfileReadyTargets        int `json:"profile_ready_targets"`         // 目标
	ProfileUnavailableTargets  int `json:"profile_unavailable_targets"`   // 目标
	IndexUnavailableTargets    int `json:"index_unavailable_targets"`     // 目标
	IncompleteEvidenceTargets  int `json:"incomplete_evidence_targets"`   // 目标（部分缺失）
	AllEvidenceMissingTargets  int `json:"all_evidence_missing_targets"`  // 目标（召回后全空→可触发重建）
	MissingCandidateCount      int `json:"missing_candidate_count"`       // 候选人物（召回命中但精排不可读）
	RecalledCandidatePairs     int `json:"recalled_candidate_pairs"`      // 人物对
	RankedCandidatePairs       int `json:"ranked_candidate_pairs"`        // 人物对
	HardBlockedPairs           int `json:"hard_blocked_pairs"`            // 人物对
	BelowSuggestThresholdPairs int `json:"below_suggest_threshold_pairs"` // 人物对
	InsufficientPairs          int `json:"insufficient_pairs"`            // 人物对（可与 below_threshold 重叠，勿相加）
	TechUnavailablePairs       int `json:"tech_unavailable_pairs"`        // 人物对
	StrategyAcceptedPairs      int `json:"strategy_accepted_pairs"`       // 人物对
	FocusRecalled              int `json:"focus_recalled"`                // 人物对
	FocusNotRecalled           int `json:"focus_not_recalled"`            // 人物对
	FocusExactAccepted         int `json:"focus_exact_accepted"`          // 人物对
	FocusExactRejected         int `json:"focus_exact_rejected"`          // 人物对
	// PrimaryStageCounts：互斥主分类计数（单位：目标）。总和应等于 len(targets)。
	PrimaryStageCounts map[string]int `json:"primary_stage_counts,omitempty"`
}

// MergeSuggestionTargetTiming 单目标有界计时（毫秒）。
type MergeSuggestionTargetTiming struct {
	ProfileLoadMs int64 `json:"profile_load_ms"`
	RecallMs      int64 `json:"recall_ms"`
	RerankMs      int64 `json:"rerank_ms"` // SimilarPeople 精排 + 策略
	CompareMs     int64 `json:"compare_ms"` // 逐候选 ComparePeople
	TotalMs       int64 `json:"total_ms"`
	CenterCount   int   `json:"center_count"` // 目标证据中心数
	QueryCount    int   `json:"query_count"`  // 参与召回的查询向量数
}

// MergeSuggestionFocusPairDiag 单个重点人物对的逐阶段结果（不含 embedding）。
type MergeSuggestionFocusPairDiag struct {
	TargetID            uint    `json:"target_id"`
	CandidateID         uint    `json:"candidate_id"`
	TargetEligible      bool    `json:"target_eligible"`
	TargetIneligible    string  `json:"target_ineligible_reason,omitempty"`
	TargetProfileReady  bool    `json:"target_profile_ready"`
	TargetProfileGen    int     `json:"target_profile_generation,omitempty"`
	CandidateProfileGen int     `json:"candidate_profile_generation,omitempty"`
	IndexReady          bool    `json:"index_ready"`
	IndexGeneration     int     `json:"index_generation,omitempty"`
	Recalled            bool    `json:"recalled"`
	RecallRank          int     `json:"recall_rank,omitempty"` // 1-based；未召回为 0
	TruncatedOut        bool    `json:"truncated_out,omitempty"`
	ExactScore          float64 `json:"exact_score,omitempty"`
	ForwardScore        float64 `json:"forward_score,omitempty"`
	ReverseScore        float64 `json:"reverse_score,omitempty"`
	EngineStatus        string  `json:"engine_status,omitempty"`
	BlockReason         string  `json:"block_reason,omitempty"`
	MinSupportCount     int     `json:"min_support_count,omitempty"`
	StrategyAccepted    bool    `json:"strategy_accepted"`
	StrategyReason      string  `json:"strategy_reason,omitempty"`
	// DropStage 解释该对在哪一步丢失：eligible/profile/index/recall/hard_block/threshold/insufficient/strategy/accepted。
	DropStage string `json:"drop_stage"`
}

// MergeSuggestionTargetDiag 单个目标摘要。
type MergeSuggestionTargetDiag struct {
	TargetID              uint   `json:"target_id"`
	Eligible              bool   `json:"eligible"`
	IneligibleReason      string `json:"ineligible_reason,omitempty"`
	Category              string `json:"category,omitempty"`
	Hidden                bool   `json:"hidden,omitempty"`
	FaceCount             int    `json:"face_count,omitempty"`
	ProfileReady          bool   `json:"profile_ready"`
	ProfileGeneration     int    `json:"profile_generation,omitempty"`
	EvidenceUnitCount     int    `json:"evidence_unit_count,omitempty"`
	IndexReady            bool   `json:"index_ready"`
	RecalledCount         int    `json:"recalled_count"`
	RankedCount           int    `json:"ranked_count"`
	MissingCandidateIDs   []uint `json:"missing_candidate_ids,omitempty"`
	MissingCandidateCount int    `json:"missing_candidate_count,omitempty"`
	IncompleteEvidence    bool   `json:"incomplete_evidence,omitempty"`
	EngineStatus          string `json:"engine_status,omitempty"`
	BlockReason           string `json:"block_reason,omitempty"`
	StrategyAcceptedCount int    `json:"strategy_accepted_count"`
	// PrimaryStage 互斥主分类；Flags 为可重叠附加标志。
	PrimaryStage string   `json:"primary_stage,omitempty"`
	Flags        []string `json:"flags,omitempty"`
	Timing       *MergeSuggestionTargetTiming `json:"timing,omitempty"`
}

// MergeSuggestionDiagnoseReport 只读诊断报告。
type MergeSuggestionDiagnoseReport struct {
	CollectedAt      time.Time                      `json:"collected_at"`
	EngineVersion    string                         `json:"engine_version"`
	StrategyVersion  string                         `json:"strategy_version"`
	SuggestThreshold float64                        `json:"suggest_threshold"`
	TopK             int                            `json:"top_k"`
	ANNK             int                            `json:"ann_k"`
	ExactK           int                            `json:"exact_k"`
	MaxCandidates    int                            `json:"max_candidates"`
	IndexGeneration  int                            `json:"index_generation"`
	IndexReady       bool                           `json:"index_ready"`
	StageStats       MergeSuggestionStageStats      `json:"stage_stats"`
	Targets          []MergeSuggestionTargetDiag    `json:"targets"`
	FocusPairs       []MergeSuggestionFocusPairDiag `json:"focus_pairs"`
	TimingSummary    *MergeSuggestionTimingSummary  `json:"timing_summary,omitempty"`
	// FailureSchedule：失败表只读聚合（本修复核心产出）；nil 表示未注入 failure repo。
	FailureSchedule *MergeSuggestionFailureScheduleDiag `json:"failure_schedule,omitempty"`
	Notes            []string                       `json:"notes,omitempty"`
}

// MergeSuggestionFailureScheduleDiag 是失败表未解决/耗尽计数（单位：目标）。
type MergeSuggestionFailureScheduleDiag struct {
	UnresolvedTargets int64 `json:"unresolved_target_count"`
	RetryWait         int64 `json:"retry_total_count"`
	RetryDue          int64 `json:"retry_due_count"`
	RetryDeferred     int64 `json:"retry_deferred_count"`
	RetryExhausted    int64 `json:"retry_exhausted_count"`
	WaitingEvidence   int64 `json:"waiting_evidence_count"`
}

// MergeSuggestionTimingSummary 有界聚合计时（不对每 tick 打印全量失败数组）。
type MergeSuggestionTimingSummary struct {
	TargetCount       int   `json:"target_count"`
	TotalMs           int64 `json:"total_ms"`
	AvgProfileLoadMs  int64 `json:"avg_profile_load_ms"`
	AvgRecallMs       int64 `json:"avg_recall_ms"`
	AvgRerankMs       int64 `json:"avg_rerank_ms"`
	AvgCompareMs      int64 `json:"avg_compare_ms"`
	AvgTotalMs        int64 `json:"avg_total_ms"`
	MaxTotalMs        int64 `json:"max_total_ms"`
	SampledTargetIDs  []uint `json:"sampled_target_ids,omitempty"` // 最慢若干目标，有界
}

// personLookup 诊断用人物读取；与生产 PersonRepository 对齐最小接口。
type personLookup interface {
	ListByIDs(ids []uint) ([]*model.Person, error)
}

// DiagnoseMergeSuggestionTargets 复用生产引擎召回/精排/策略，输出有界阶段诊断。
// 只读：不写 person_merge_suggestions，不触发生产任务。ANN 重建请求不会从本函数发起
// （诊断走 ComparePeople/SimilarPeople 只读路径；全空证据时 SimilarPeople 内部仍可能 RequestRebuild——
// 若调用方需要绝对只读索引，应传入不可变 ANN 或在副本进程使用）。
func DiagnoseMergeSuggestionTargets(
	engine *IdentityMatchingEngine,
	people personLookup,
	req MergeSuggestionDiagnoseRequest,
) (*MergeSuggestionDiagnoseReport, error) {
	if engine == nil {
		return nil, fmt.Errorf("identity matching engine is nil")
	}
	if people == nil {
		return nil, fmt.Errorf("person lookup is nil")
	}
	targetIDs := dedupSortUint(req.TargetIDs)
	if len(targetIDs) == 0 {
		return nil, fmt.Errorf("target_ids required")
	}

	strategy := req.Strategy
	if strategy.Name == "" {
		strategy = NewIdentitySuggestStrategy(config.PeopleConfig{
			MergeSuggestionThreshold:      0.55,
			IdentityProfileMinCenterFaces: 3,
		})
	}
	opts := req.Recall.normalized()
	if opts.TopK <= 0 {
		opts.TopK = mergeSuggestionProfileK
	}

	report := &MergeSuggestionDiagnoseReport{
		CollectedAt:      time.Now().UTC(),
		EngineVersion:    engine.EngineVersion(),
		StrategyVersion:  strategy.Version,
		SuggestThreshold: strategy.ScoreThreshold,
		TopK:             opts.TopK,
		ANNK:             opts.ANNK,
		ExactK:           opts.ExactK,
		MaxCandidates:    opts.MaxCandidates,
		IndexGeneration:  engine.IndexGeneration(),
		IndexReady:       engine.ann != nil && engine.ann.Ready(engine.cfg.EmbeddingModel),
		StageStats: MergeSuggestionStageStats{
			PrimaryStageCounts: make(map[string]int),
		},
		Notes: []string{
			"只读诊断：不写推荐表；单位见 stage_stats 字段注释。",
			"部分证据缺失与索引脱节（全空→RequestRebuild）分开计数。",
			"Focus 对即使未召回也会做精确 ComparePeople，用于对照 ANN 漏召回。",
			"primary_stage 互斥；insufficient_pairs 与 below_suggest_threshold_pairs 可重叠，勿相加充当漏斗。",
			"TopK 默认 50（生产 mergeSuggestionProfileK）；旧报告复现请显式传 TopK=20。",
			"waiting_evidence 目标由证据变化自动唤醒，人工重试会跳过，避免空转。",
			"实际写入/未改变看任务页 API（pending/last_run），本诊断不写库故不含该指标。",
		},
	}

	peopleRows, err := people.ListByIDs(targetIDs)
	if err != nil {
		return nil, fmt.Errorf("list targets: %w", err)
	}
	byID := make(map[uint]*model.Person, len(peopleRows))
	for _, p := range peopleRows {
		if p != nil {
			byID[p.ID] = p
		}
	}

	// 召回排序位置：在 SimilarPeople 之前对每个合格目标跑一遍 recall，记录 rank。
	recallRankByTarget := make(map[uint]map[uint]int, len(targetIDs))
	preTruncationHit := make(map[uint]map[uint]bool, len(targetIDs))
	var timingSum MergeSuggestionTimingSummary
	slowSamples := make([]struct {
		id  uint
		ms  int64
	}, 0, len(targetIDs))

	for _, tid := range targetIDs {
		targetStart := time.Now()
		td := MergeSuggestionTargetDiag{TargetID: tid, IndexReady: report.IndexReady}
		timing := &MergeSuggestionTargetTiming{}
		p := byID[tid]
		ok, reason := mergeSuggestionTargetEligible(p)
		td.Eligible = ok
		td.IneligibleReason = reason
		if p != nil {
			td.Category = p.Category
			td.Hidden = p.Hidden
			td.FaceCount = p.FaceCount
		}
		if !ok {
			report.StageStats.IneligibleTargets++
			td.PrimaryStage = DiagnoseStageIneligible
			report.StageStats.PrimaryStageCounts[td.PrimaryStage]++
			report.Targets = append(report.Targets, td)
			continue
		}
		report.StageStats.EligibleTargets++

		profileStart := time.Now()
		evs, st, rs := engine.loadPersonEvidences([]uint{tid})
		timing.ProfileLoadMs = time.Since(profileStart).Milliseconds()
		ev, has := evs[tid]
		if st != IdentityMatchStatusMatch || !has || len(ev.Units) == 0 {
			td.ProfileReady = false
			td.EngineStatus = string(IdentityMatchStatusUnavailable)
			td.BlockReason = rs
			if td.BlockReason == "" {
				td.BlockReason = blockProfileUnavailable
			}
			report.StageStats.ProfileUnavailableTargets++
			if td.BlockReason == blockNoUsableEvidence {
				td.PrimaryStage = DiagnoseStageNoUsableEvidence
			} else {
				td.PrimaryStage = DiagnoseStageTargetUnavailable
			}
			report.StageStats.PrimaryStageCounts[td.PrimaryStage]++
			timing.TotalMs = time.Since(targetStart).Milliseconds()
			td.Timing = timing
			report.Targets = append(report.Targets, td)
			continue
		}
		td.ProfileReady = true
		td.ProfileGeneration = ev.Generation
		td.EvidenceUnitCount = len(ev.Units)
		timing.CenterCount = len(ev.Units)
		timing.QueryCount = len(ev.Units)
		report.StageStats.ProfileReadyTargets++

		if !report.IndexReady {
			td.EngineStatus = string(IdentityMatchStatusUnavailable)
			td.BlockReason = blockIndexUnavailable
			report.StageStats.IndexUnavailableTargets++
			td.PrimaryStage = DiagnoseStageIndexUnavailable
			report.StageStats.PrimaryStageCounts[td.PrimaryStage]++
			timing.TotalMs = time.Since(targetStart).Milliseconds()
			td.Timing = timing
			report.Targets = append(report.Targets, td)
			continue
		}

		// 截断前全量 hits（仅诊断；生产截断用 MaxCandidates）。
		recallStart := time.Now()
		rawHits, recallReady := engine.recallPersonHits(ev, tid, opts)
		timing.RecallMs = time.Since(recallStart).Milliseconds()
		if !recallReady {
			td.EngineStatus = string(IdentityMatchStatusUnavailable)
			td.BlockReason = blockIndexUnavailable
			report.StageStats.IndexUnavailableTargets++
			td.PrimaryStage = DiagnoseStageIndexUnavailable
			report.StageStats.PrimaryStageCounts[td.PrimaryStage]++
			timing.TotalMs = time.Since(targetStart).Milliseconds()
			td.Timing = timing
			report.Targets = append(report.Targets, td)
			continue
		}
		rankMap := make(map[uint]int, len(rawHits))
		for i, h := range rawHits {
			rankMap[h.personID] = i + 1
		}
		preTruncationHit[tid] = make(map[uint]bool, len(rawHits))
		for pid := range rankMap {
			preTruncationHit[tid][pid] = true
		}
		recalled := selectPersonIDsByRecallRank(rawHits, opts.MaxCandidates, opts.ANNK, opts.ExactK)
		recallRankByTarget[tid] = make(map[uint]int, len(recalled))
		for i, cid := range recalled {
			recallRankByTarget[tid][cid] = i + 1
		}
		td.RecalledCount = len(recalled)
		report.StageStats.RecalledCandidatePairs += len(recalled)

		rerankStart := time.Now()
		sim := engine.SimilarPeople([]uint{tid}, opts)[tid]
		timing.RerankMs = time.Since(rerankStart).Milliseconds()
		td.EngineStatus = string(sim.Status)
		td.BlockReason = sim.BlockReason
		td.IncompleteEvidence = sim.IncompleteEvidence
		td.MissingCandidateIDs = append([]uint(nil), sim.MissingCandidateIDs...)
		td.MissingCandidateCount = len(td.MissingCandidateIDs)
		report.StageStats.MissingCandidateCount += td.MissingCandidateCount
		td.RankedCount = len(sim.Candidates)
		report.StageStats.RankedCandidatePairs += len(sim.Candidates)
		if sim.IncompleteEvidence {
			report.StageStats.IncompleteEvidenceTargets++
			td.Flags = append(td.Flags, "incomplete_evidence")
		}
		if sim.Status == IdentityMatchStatusUnavailable && sim.BlockReason == blockProfileUnavailable && td.RecalledCount > 0 && len(sim.Candidates) == 0 {
			report.StageStats.AllEvidenceMissingTargets++
		}

		accepted := 0
		hardOnly := len(sim.Candidates) > 0
		anyBelow := false
		anyInsufficient := false
		anyTech := false
		compareStart := time.Now()
		for _, c := range sim.Candidates {
			cmp := engine.ComparePeople([]PersonPair{{TargetID: tid, CandidateID: c.PersonID}})[PersonPair{TargetID: tid, CandidateID: c.PersonID}]
			switch cmp.Status {
			case IdentityMatchStatusHardConflict:
				report.StageStats.HardBlockedPairs++
			case IdentityMatchStatusUnavailable, IdentityMatchStatusInvalid:
				report.StageStats.TechUnavailablePairs++
				anyTech = true
				hardOnly = false
			default:
				hardOnly = false
				// match / insufficient / no_candidate：人工推荐策略可在 insufficient 上接受（RequireEngineMatch=false）。
				if cmp.Status == IdentityMatchStatusInsufficient {
					report.StageStats.InsufficientPairs++
					anyInsufficient = true
				}
				dec := ApplyIdentityStrategy(cmp, strategy)
				if dec.Accepted {
					accepted++
					report.StageStats.StrategyAcceptedPairs++
				} else if cmp.Best != nil && cmp.Best.Score < strategy.ScoreThreshold {
					report.StageStats.BelowSuggestThresholdPairs++
					anyBelow = true
				}
			}
		}
		timing.CompareMs = time.Since(compareStart).Milliseconds()
		td.StrategyAcceptedCount = accepted
		if anyInsufficient {
			td.Flags = append(td.Flags, "has_insufficient")
		}
		if anyBelow {
			td.Flags = append(td.Flags, "has_below_threshold")
		}
		if anyTech {
			td.Flags = append(td.Flags, "has_tech_failure")
		}

		td.PrimaryStage = classifyDiagnosePrimaryStage(td, sim, accepted, hardOnly, anyBelow, anyTech)
		report.StageStats.PrimaryStageCounts[td.PrimaryStage]++
		timing.TotalMs = time.Since(targetStart).Milliseconds()
		td.Timing = timing
		timingSum.TargetCount++
		timingSum.TotalMs += timing.TotalMs
		timingSum.AvgProfileLoadMs += timing.ProfileLoadMs
		timingSum.AvgRecallMs += timing.RecallMs
		timingSum.AvgRerankMs += timing.RerankMs
		timingSum.AvgCompareMs += timing.CompareMs
		if timing.TotalMs > timingSum.MaxTotalMs {
			timingSum.MaxTotalMs = timing.TotalMs
		}
		slowSamples = append(slowSamples, struct {
			id uint
			ms int64
		}{tid, timing.TotalMs})
		report.Targets = append(report.Targets, td)
	}

	// 重点人物对：精确路径 + 召回对照。
	for _, tid := range targetIDs {
		cands := dedupSortUint(req.FocusCandidates[tid])
		for _, cid := range cands {
			pair := MergeSuggestionFocusPairDiag{
				TargetID:        tid,
				CandidateID:     cid,
				IndexReady:      report.IndexReady,
				IndexGeneration: report.IndexGeneration,
			}
			p := byID[tid]
			ok, reason := mergeSuggestionTargetEligible(p)
			pair.TargetEligible = ok
			pair.TargetIneligible = reason
			if !ok {
				pair.DropStage = "eligible"
				report.FocusPairs = append(report.FocusPairs, pair)
				continue
			}

			evs, _, _ := engine.loadPersonEvidences([]uint{tid, cid})
			tev, tok := evs[tid]
			cev, cok := evs[cid]
			pair.TargetProfileReady = tok && len(tev.Units) > 0
			if pair.TargetProfileReady {
				pair.TargetProfileGen = tev.Generation
			}
			if cok && len(cev.Units) > 0 {
				pair.CandidateProfileGen = cev.Generation
			}
			if !pair.TargetProfileReady || !cok || len(cev.Units) == 0 {
				pair.DropStage = "profile"
				pair.EngineStatus = string(IdentityMatchStatusUnavailable)
				pair.BlockReason = blockProfileUnavailable
				report.FocusPairs = append(report.FocusPairs, pair)
				continue
			}
			if !report.IndexReady {
				pair.DropStage = "index"
				pair.EngineStatus = string(IdentityMatchStatusUnavailable)
				pair.BlockReason = blockIndexUnavailable
				report.FocusPairs = append(report.FocusPairs, pair)
				continue
			}

			if ranks := recallRankByTarget[tid]; ranks != nil {
				if r, ok := ranks[cid]; ok {
					pair.Recalled = true
					pair.RecallRank = r
					report.StageStats.FocusRecalled++
				} else {
					report.StageStats.FocusNotRecalled++
					if preTruncationHit[tid] != nil && preTruncationHit[tid][cid] {
						pair.TruncatedOut = true
					}
				}
			} else {
				report.StageStats.FocusNotRecalled++
			}

			cmp := engine.ComparePeople([]PersonPair{{TargetID: tid, CandidateID: cid}})[PersonPair{TargetID: tid, CandidateID: cid}]
			pair.EngineStatus = string(cmp.Status)
			pair.BlockReason = cmp.BlockReason
			sc := ScoreIdentityEvidence(tev, cev)
			pair.ForwardScore = sc.ForwardScore
			pair.ReverseScore = sc.ReverseScore
			pair.ExactScore = sc.Score
			if cmp.Best != nil {
				pair.MinSupportCount = cmp.Best.MinSupportCount
			}

			dec := ApplyIdentityStrategy(cmp, strategy)
			pair.StrategyAccepted = dec.Accepted
			pair.StrategyReason = dec.Reason

			switch {
			case !pair.Recalled && pair.TruncatedOut:
				pair.DropStage = "recall_truncated"
			case !pair.Recalled:
				pair.DropStage = "recall"
			case cmp.Status == IdentityMatchStatusHardConflict:
				pair.DropStage = "hard_block"
			case cmp.Status == IdentityMatchStatusUnavailable || cmp.Status == IdentityMatchStatusInvalid:
				pair.DropStage = "tech_unavailable"
			case dec.Accepted:
				pair.DropStage = "accepted"
			case cmp.Best != nil && cmp.Best.Score < strategy.ScoreThreshold:
				pair.DropStage = "threshold"
			default:
				pair.DropStage = "strategy"
			}

			if dec.Accepted {
				report.StageStats.FocusExactAccepted++
			} else if pair.DropStage != "hard_block" && pair.DropStage != "tech_unavailable" {
				report.StageStats.FocusExactRejected++
			}
			report.FocusPairs = append(report.FocusPairs, pair)
		}
	}

	sort.SliceStable(report.FocusPairs, func(i, j int) bool {
		if report.FocusPairs[i].TargetID != report.FocusPairs[j].TargetID {
			return report.FocusPairs[i].TargetID < report.FocusPairs[j].TargetID
		}
		return report.FocusPairs[i].CandidateID < report.FocusPairs[j].CandidateID
	})
	if timingSum.TargetCount > 0 {
		n := int64(timingSum.TargetCount)
		timingSum.AvgTotalMs = timingSum.TotalMs / n
		timingSum.AvgProfileLoadMs = timingSum.AvgProfileLoadMs / n
		timingSum.AvgRecallMs = timingSum.AvgRecallMs / n
		timingSum.AvgRerankMs = timingSum.AvgRerankMs / n
		timingSum.AvgCompareMs = timingSum.AvgCompareMs / n
		sort.Slice(slowSamples, func(i, j int) bool { return slowSamples[i].ms > slowSamples[j].ms })
		limit := 5
		if len(slowSamples) < limit {
			limit = len(slowSamples)
		}
		for i := 0; i < limit; i++ {
			timingSum.SampledTargetIDs = append(timingSum.SampledTargetIDs, slowSamples[i].id)
		}
		report.TimingSummary = &timingSum
	}

	if req.FailureRepo != nil {
		counts, err := req.FailureRepo.CountUnresolved(time.Now().UTC())
		if err != nil {
			report.Notes = append(report.Notes, fmt.Sprintf("failure_schedule 不可用: %v", err))
		} else if counts != nil {
			report.FailureSchedule = &MergeSuggestionFailureScheduleDiag{
				UnresolvedTargets: counts.UnresolvedTargets,
				RetryWait:         counts.RetryTotal,
				RetryDue:          counts.RetryDue,
				RetryDeferred:     counts.RetryDeferred,
				RetryExhausted:    counts.RetryExhausted,
				WaitingEvidence:   counts.WaitingEvidence,
			}
		}
	}
	return report, nil
}

func classifyDiagnosePrimaryStage(
	td MergeSuggestionTargetDiag,
	sim IdentityMatchResult,
	accepted int,
	hardOnly bool,
	anyBelow bool,
	anyTech bool,
) string {
	if accepted > 0 {
		// incomplete / tech 痕迹留在 flags，主分类仍为 accepted。
		return DiagnoseStageAccepted
	}
	switch {
	case sim.Status == IdentityMatchStatusUnavailable && sim.BlockReason == blockNoUsableEvidence:
		return DiagnoseStageNoUsableEvidence
	case sim.Status == IdentityMatchStatusUnavailable && sim.BlockReason == blockIndexUnavailable:
		return DiagnoseStageIndexUnavailable
	case td.IncompleteEvidence && len(sim.Candidates) == 0:
		return DiagnoseStageIncompleteEvidence
	case sim.Status == IdentityMatchStatusUnavailable || sim.Status == IdentityMatchStatusInvalid:
		return DiagnoseStageTechFailure
	case sim.Status == IdentityMatchStatusNoCandidate || (td.RecalledCount == 0 && len(sim.Candidates) == 0):
		return DiagnoseStageNoCandidate
	case hardOnly:
		return DiagnoseStageHardBlocked
	case anyTech && !anyBelow:
		return DiagnoseStageTechFailure
	case anyBelow:
		return DiagnoseStageBelowThreshold
	default:
		return DiagnoseStageStrategyRejected
	}
}

func mergeSuggestionTargetEligible(p *model.Person) (bool, string) {
	if p == nil {
		return false, "not_found"
	}
	if p.Hidden {
		return false, "hidden"
	}
	if p.FaceCount <= 0 {
		return false, "no_faces"
	}
	switch p.Category {
	case model.PersonCategoryFamily, model.PersonCategoryFriend, model.PersonCategoryAcquaintance:
		return true, ""
	default:
		return false, "category"
	}
}

// recallPersonHits 返回截断前的召回命中（含 rank 信息）。
// ready=false 表示索引/Search 不可用；ready=true 且 hits 为空表示真的没有候选（禁止与不可用混淆）。
// ExactK>0 时并入有界精确 cosine 补召（名次接在 ANNK 之后），不依赖 HNSW 图稳定性。
func (e *IdentityMatchingEngine) recallPersonHits(ev IdentityEvidence, targetID uint, opts IdentityRecallOptions) (hits []personRecallHit, ready bool) {
	opts = opts.normalized()
	if e == nil || e.ann == nil {
		return nil, false
	}
	minRank := make(map[uint]int)
	hitCount := make(map[uint]int)
	noteHit := func(pid uint, rank int) {
		if pid == 0 || pid == targetID {
			return
		}
		if r, exists := minRank[pid]; !exists || rank < r {
			minRank[pid] = rank
		}
		hitCount[pid]++
	}
	for _, u := range ev.Units {
		ids, ok := e.ann.Search(u.Vector, opts.ANNK, e.cfg.EmbeddingModel)
		if !ok {
			return nil, false
		}
		for rank, pid := range ids {
			noteHit(pid, rank)
		}
		if opts.ExactK > 0 {
			exactIDs, ok := e.ann.ExactTopPeople(u.Vector, opts.ExactK, e.cfg.EmbeddingModel)
			if !ok {
				return nil, false
			}
			for rank, pid := range exactIDs {
				noteHit(pid, opts.ANNK+rank)
			}
		}
	}
	// 显式分配空 slice，避免 append(nil, ...) 在零命中时返回 nil 被误判为未就绪。
	out := make([]personRecallHit, 0, len(minRank))
	for pid, rank := range minRank {
		out = append(out, personRecallHit{
			personID: pid,
			minRank:  rank,
			hitCount: hitCount[pid],
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].minRank != out[j].minRank {
			return out[i].minRank < out[j].minRank
		}
		if out[i].hitCount != out[j].hitCount {
			return out[i].hitCount > out[j].hitCount
		}
		return out[i].personID < out[j].personID
	})
	return out, true
}

// diagnoseActiveCenterLister 诊断建索引所需的最小仓库能力。
type diagnoseActiveCenterLister interface {
	ListAllActiveCenters(embeddingModel string) ([]*model.PersonIdentityCenter, error)
	ListActiveCentersByPersonIDs(personIDs []uint, embeddingModel string) (map[uint][]*model.PersonIdentityCenter, error)
}

// BuildMergeSuggestionDiagnoseEngine 在副本库上构建与生产一致的匹配引擎（含 ANN 全量重建）。
// 仅供只读诊断 CLI/测试；内存与中心数量成正比。返回 centerCount 便于成本说明。
//
// 副本恢复验收若已有运行中的画像服务 ANN，应改用 BuildIdentityMatchingEngineFromProfileService，
// 避免另造一套诊断索引再手调 Reconcile。
func BuildMergeSuggestionDiagnoseEngine(
	profileRepo diagnoseActiveCenterLister,
	faceRepo matcherFaceRepo,
	cannotLinkRepo matcherCannotLinkRepo,
	sharingRepo identitySharingRepo,
	embeddingModel string,
	cfg IdentityProfileMatcherConfig,
) (*IdentityMatchingEngine, int, error) {
	if profileRepo == nil {
		return nil, 0, fmt.Errorf("profile repo is nil")
	}
	modelSig := strings.TrimSpace(embeddingModel)
	if modelSig == "" {
		return nil, 0, fmt.Errorf("embedding model required")
	}
	centers, err := profileRepo.ListAllActiveCenters(modelSig)
	if err != nil {
		return nil, 0, fmt.Errorf("list active centers: %w", err)
	}
	cfg.EmbeddingModel = modelSig
	ann := newIdentityProfileANN(modelSig)
	if err := ann.Rebuild(centers, modelSig); err != nil {
		return nil, 0, fmt.Errorf("ann rebuild: %w", err)
	}
	return NewIdentityMatchingEngine(ann, profileRepo, faceRepo, cannotLinkRepo, sharingRepo, cfg), len(centers), nil
}

// BuildIdentityMatchingEngineFromProfileService 用画像服务当前 ANN 构造匹配引擎，
// 与 NewService 生产路径共用同一内存索引，不另建诊断 ANN。
// annReady=false 时调用方应 AttachIdentityMatchingWiring(engine, false)，由后续切片/就绪回调补做核对。
func BuildIdentityMatchingEngineFromProfileService(
	profile PersonIdentityProfileService,
	profileRepo diagnoseActiveCenterLister,
	faceRepo matcherFaceRepo,
	cannotLinkRepo matcherCannotLinkRepo,
	sharingRepo identitySharingRepo,
	cfg IdentityProfileMatcherConfig,
) (engine *IdentityMatchingEngine, annReady bool, centerCount int, err error) {
	type annProvider interface {
		ANN() (*identityProfileANN, string)
	}
	p, ok := profile.(annProvider)
	if !ok || p == nil {
		return nil, false, 0, fmt.Errorf("profile service does not expose ANN")
	}
	ann, modelSig := p.ANN()
	if ann == nil || strings.TrimSpace(modelSig) == "" {
		return nil, false, 0, fmt.Errorf("identity profile ANN unavailable")
	}
	modelSig = strings.TrimSpace(modelSig)
	cfg.EmbeddingModel = modelSig
	if profileRepo != nil {
		centers, listErr := profileRepo.ListAllActiveCenters(modelSig)
		if listErr != nil {
			return nil, false, 0, fmt.Errorf("list active centers: %w", listErr)
		}
		centerCount = len(centers)
	}
	return NewIdentityMatchingEngine(ann, profileRepo, faceRepo, cannotLinkRepo, sharingRepo, cfg),
		ann.Ready(modelSig), centerCount, nil
}

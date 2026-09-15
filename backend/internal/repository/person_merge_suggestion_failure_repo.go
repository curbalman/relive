package repository

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/davidhoo/relive/internal/model"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// 合并建议失败重试预算（与旧 merge_suggestion_retry 常量对齐）。
const (
	MergeSuggestionFailureMaxAttempts = 5
	MergeSuggestionFailureBaseSeconds = 300
	MergeSuggestionFailureMaxSeconds  = 3600
	MergeSuggestionFailureMaxDeps     = 200
)

// PersonMergeSuggestionFailureRepository 持久化未解决推荐目标状态。
type PersonMergeSuggestionFailureRepository interface {
	// UpsertTechnicalFailure 记录/推进技术失败：retry_wait；达预算变 exhausted。
	// 不消耗 waiting_evidence；已 resolved 的行可重新打开。
	UpsertTechnicalFailure(req PersonMergeFailureUpsert) (*model.PersonMergeSuggestionFailure, error)
	// UpsertWaitingEvidence 标记等待证据：不推进 attempts，清空 next_retry_at。
	UpsertWaitingEvidence(req PersonMergeFailureUpsert) (*model.PersonMergeSuggestionFailure, error)
	// Resolve 关闭未解决记录；不存在或已 resolved 则幂等成功。
	Resolve(targetPersonID uint, reason string) error
	// ResolveIneligible 目标不再合格时关闭，并记 ineligible 原因。
	ResolveIneligible(targetPersonIDs []uint) error
	// ListDueRetry 返回 state=retry_wait 且 next_retry_at<=now 的目标，按 next_retry_at/person_id。
	ListDueRetry(now time.Time, limit int) ([]*model.PersonMergeSuggestionFailure, error)
	// GetByTarget 按目标读取当前行（含 resolved）。
	GetByTarget(targetPersonID uint) (*model.PersonMergeSuggestionFailure, error)
	// ListUnresolved 分页列出未解决（非 resolved），按 last_failed_at DESC。
	// UI 契约：pageSize 上限 100；内部核对不要用本方法。
	ListUnresolved(page, pageSize int) ([]*model.PersonMergeSuggestionFailure, int64, error)
	// ListUnresolvedKeyset 内部核对应：按 target_person_id 升序 keyset，limit≤100。
	// afterTargetID=0 表示从最小 ID 开始；maxTargetID>0 时本轮上界（含）。
	ListUnresolvedKeyset(afterTargetID, maxTargetID uint, limit int) ([]*model.PersonMergeSuggestionFailure, error)
	// MaxUnresolvedTargetID 返回未解决行最大 target_person_id；无行则 0。
	MaxUnresolvedTargetID() (uint, error)
	// ListTargetIDsByState 返回指定状态的全部目标 ID（无分页上限，供内部调度）。
	ListTargetIDsByState(state string) ([]uint, error)
	// CountUnresolved 返回任务页计数。
	CountUnresolved(now time.Time) (*model.PersonMergeFailureCounts, error)
	// ManualRetry 人工重开所选耗尽/retry_wait 目标：重置 attempts、进入 retry_wait，并落审计字段。
	// waiting_evidence + no_usable_evidence 跳过并返回 skipped.reason=waiting_evidence，避免点击空转。
	// 已安排且 attempts==0 的再次请求返回 already_scheduled，不推迟 next_retry_at。
	// 新路径请优先用 BeginManualRetryArrangement，避免预算重开与 pending 安排拆成两次提交。
	ManualRetry(targetPersonIDs []uint, reason string, now time.Time) (accepted []uint, skipped []PersonMergeFailureSkip, err error)
	// BeginManualRetryArrangement 在同一事务内重开人工预算并写入 pending 安排与完整 remaining。
	// 失败整体回滚，不留下 ManualRetriedAt!=nil && pending=false 的半完成态。
	// 已有 pending 安排时 accepted=true 且不重置预算（由调用方继续补齐 remaining）。
	BeginManualRetryArrangement(targetPersonID uint, reason string, remainingPersonIDs []uint, now time.Time) (accepted bool, skipReason string, err error)
	// ClearManualRetryMarkers 在依赖画像安排失败时清除人工标记，允许客户端补齐未完成步骤。
	ClearManualRetryMarkers(targetPersonIDs []uint) error
	// UpdateManualArrangement 持久化人工重试安排断点（pending + 剩余 MarkDirty 人物）。
	UpdateManualArrangement(targetPersonID uint, pending bool, remainingPersonIDs []uint) error
	// ImportFromLegacyRetries 幂等导入旧 JSON retry_targets；已存在行不覆盖更高 attempts。
	ImportFromLegacyRetries(entries []LegacyMergeRetryImport, now time.Time) (int, error)
	// WakeForEvidenceChange 画像/证据恢复后唤醒依赖该人物的未解决目标（含 exhausted 重开预算）。
	// observedGens 为各 person 的当前活动代次；仅当目标/依赖记录的代次落后于观测值时才唤醒，
	// 避免同一代次重复通知反复赠送预算。缺省（nil/0）时对该 ID 仍按代次未知唤醒一次并回写戳记。
	WakeForEvidenceChange(personIDs []uint, observedGens map[uint]int, now time.Time) (woken []uint, err error)
	// WakeForIndexReady 索引从不可用→可用时唤醒仍标记 index_ready_required 的未解决行
	//（含 exhausted，有界重开预算）；普通重建时若无该标记则 noop，不反复赠预算。
	WakeForIndexReady(now time.Time) (woken []uint, err error)
	// ReconcileDependencyVersions 启动后有界核对：目标/依赖代次已前进或索引依赖仍挂起时唤醒，补偿丢通知。
	ReconcileDependencyVersions(observedGens map[uint]int, indexReady bool, now time.Time) (woken []uint, err error)
	// ReconcileDependencyVersionsRows 对 keyset 已拉取的一批行核对（indexReady 仅整轮末尾一次为 true）。
	ReconcileDependencyVersionsRows(rows []*model.PersonMergeSuggestionFailure, observedGens map[uint]int, indexReady bool, now time.Time) (woken []uint, err error)
}

// PersonMergeFailureUpsert 是一次失败写入意图。
type PersonMergeFailureUpsert struct {
	TargetPersonID     uint
	ReasonCode         string
	TargetProfileGen   int
	EngineVersion      string
	ConfigFingerprint  string
	BlockingDeps       []model.PersonMergeFailureBlockingDep
	IndexReadyRequired bool
	// AttemptKey 非空时：与 LastAttemptKey 相同则幂等跳过 attempts 递增（同一次重验重放）。
	AttemptKey string
	Now        time.Time
}

// PersonMergeFailureSkip 人工重试跳过项。
type PersonMergeFailureSkip struct {
	TargetPersonID uint   `json:"target_person_id"`
	Reason         string `json:"reason"`
}

// LegacyMergeRetryImport 从 app_config.retry_targets 导入。
type LegacyMergeRetryImport struct {
	TargetID    uint
	Attempts    int
	NextRetryAt time.Time
	Reason      string
}

type personMergeSuggestionFailureRepository struct {
	db *gorm.DB
}

// NewPersonMergeSuggestionFailureRepository 构造失败表仓库。
func NewPersonMergeSuggestionFailureRepository(db *gorm.DB) PersonMergeSuggestionFailureRepository {
	return &personMergeSuggestionFailureRepository{db: db}
}

func mergeSuggestionFailureBackoff(attempts int) time.Duration {
	if attempts < 1 {
		attempts = 1
	}
	sec := MergeSuggestionFailureBaseSeconds
	for i := 1; i < attempts; i++ {
		if sec >= MergeSuggestionFailureMaxSeconds/2 {
			sec = MergeSuggestionFailureMaxSeconds
			break
		}
		sec *= 2
	}
	if sec > MergeSuggestionFailureMaxSeconds {
		sec = MergeSuggestionFailureMaxSeconds
	}
	return time.Duration(sec) * time.Second
}

func encodeBlockingDeps(deps []model.PersonMergeFailureBlockingDep) (string, error) {
	if len(deps) == 0 {
		return "[]", nil
	}
	seen := make(map[uint]model.PersonMergeFailureBlockingDep, len(deps))
	order := make([]uint, 0, len(deps))
	for _, d := range deps {
		if d.PersonID == 0 {
			continue
		}
		if _, ok := seen[d.PersonID]; !ok {
			order = append(order, d.PersonID)
		}
		seen[d.PersonID] = d
	}
	sort.Slice(order, func(i, j int) bool { return order[i] < order[j] })
	if len(order) > MergeSuggestionFailureMaxDeps {
		return "", fmt.Errorf("blocking deps %d exceed cap %d", len(order), MergeSuggestionFailureMaxDeps)
	}
	out := make([]model.PersonMergeFailureBlockingDep, 0, len(order))
	for _, id := range order {
		out = append(out, seen[id])
	}
	b, err := json.Marshal(out)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// stampBlockingDepsObserved 写入失败行时标记依赖已观测（含 gen=0 的确认不可用）。
func stampBlockingDepsObserved(deps []model.PersonMergeFailureBlockingDep) []model.PersonMergeFailureBlockingDep {
	if len(deps) == 0 {
		return deps
	}
	out := make([]model.PersonMergeFailureBlockingDep, len(deps))
	copy(out, deps)
	for i := range out {
		out[i].ObsKnown = true
	}
	return out
}

// attemptKeyIsStale 判断 reqKey 是否严格早于 lastKey（dg-N-seq-M 格式）。
// 格式无法解析时不做 stale 拦截，避免误伤。
func attemptKeyIsStale(reqKey, lastKey string) bool {
	if reqKey == "" || lastKey == "" || reqKey == lastKey {
		return false
	}
	reqDG, reqSeq, ok1 := parseFailureAttemptKey(reqKey)
	lastDG, lastSeq, ok2 := parseFailureAttemptKey(lastKey)
	if !ok1 || !ok2 {
		return false
	}
	if reqDG < lastDG {
		return true
	}
	if reqDG == lastDG && reqSeq < lastSeq {
		return true
	}
	return false
}

func parseFailureAttemptKey(key string) (dirtyGen, seq uint64, ok bool) {
	// 期望格式：dg-{DirtyGeneration}-seq-{Seq}
	var dg, s uint64
	n, err := fmt.Sscanf(key, "dg-%d-seq-%d", &dg, &s)
	if err != nil || n != 2 {
		return 0, 0, false
	}
	return dg, s, true
}

func (r *personMergeSuggestionFailureRepository) UpsertTechnicalFailure(req PersonMergeFailureUpsert) (*model.PersonMergeSuggestionFailure, error) {
	if req.TargetPersonID == 0 {
		return nil, errors.New("target_person_id required")
	}
	now := req.Now
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	depsJSON, err := encodeBlockingDeps(stampBlockingDepsObserved(req.BlockingDeps))
	if err != nil {
		return nil, err
	}
	var out *model.PersonMergeSuggestionFailure
	err = r.db.Transaction(func(tx *gorm.DB) error {
		var row model.PersonMergeSuggestionFailure
		err := tx.Where("target_person_id = ?", req.TargetPersonID).First(&row).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			attempts := 1
			next := now.Add(mergeSuggestionFailureBackoff(attempts))
			row = model.PersonMergeSuggestionFailure{
				TargetPersonID:     req.TargetPersonID,
				State:              model.PersonMergeFailureStateRetryWait,
				ReasonCode:         req.ReasonCode,
				Attempts:           attempts,
				NextRetryAt:        &next,
				FirstFailedAt:      now,
				LastFailedAt:       now,
				TargetProfileGen:   req.TargetProfileGen,
				TargetObsKnown:     true,
				EngineVersion:      req.EngineVersion,
				ConfigFingerprint:  req.ConfigFingerprint,
				BlockingDepsJSON:   depsJSON,
				IndexReadyRequired: req.IndexReadyRequired,
				LastAttemptKey:     req.AttemptKey,
			}
			if err := tx.Create(&row).Error; err != nil {
				return err
			}
			out = &row
			return nil
		}
		if err != nil {
			return err
		}
		// 同一次重验尝试的重放：不增加 attempts、不推迟 next_retry_at。
		if req.AttemptKey != "" && row.LastAttemptKey == req.AttemptKey {
			out = &row
			return nil
		}
		// 更早的尝试身份晚到：不得覆盖已前进的 attempts / 已 resolved 的更新轮次。
		if attemptKeyIsStale(req.AttemptKey, row.LastAttemptKey) {
			out = &row
			return nil
		}
		if row.State == model.PersonMergeFailureStateResolved {
			row.FirstFailedAt = now
			row.Attempts = 0
			row.ResolvedAt = nil
			row.ResolvedReason = ""
		}
		if row.State == model.PersonMergeFailureStateExhausted {
			// 普通重试路径不得刷新耗尽预算；保留 exhausted。
			row.ReasonCode = req.ReasonCode
			row.LastFailedAt = now
			row.TargetProfileGen = req.TargetProfileGen
			row.TargetObsKnown = true
			row.EngineVersion = req.EngineVersion
			row.ConfigFingerprint = req.ConfigFingerprint
			row.BlockingDepsJSON = depsJSON
			row.IndexReadyRequired = req.IndexReadyRequired
			row.NextRetryAt = nil
			if req.AttemptKey != "" {
				row.LastAttemptKey = req.AttemptKey
			}
			if err := tx.Save(&row).Error; err != nil {
				return err
			}
			out = &row
			return nil
		}
		row.Attempts++
		if row.Attempts < 1 {
			row.Attempts = 1
		}
		row.ReasonCode = req.ReasonCode
		row.LastFailedAt = now
		row.TargetProfileGen = req.TargetProfileGen
		row.TargetObsKnown = true
		row.EngineVersion = req.EngineVersion
		row.ConfigFingerprint = req.ConfigFingerprint
		row.BlockingDepsJSON = depsJSON
		row.IndexReadyRequired = req.IndexReadyRequired
		if req.AttemptKey != "" {
			row.LastAttemptKey = req.AttemptKey
		}
		if row.Attempts >= MergeSuggestionFailureMaxAttempts {
			row.State = model.PersonMergeFailureStateExhausted
			row.NextRetryAt = nil
		} else {
			row.State = model.PersonMergeFailureStateRetryWait
			next := now.Add(mergeSuggestionFailureBackoff(row.Attempts))
			row.NextRetryAt = &next
		}
		if err := tx.Save(&row).Error; err != nil {
			return err
		}
		out = &row
		return nil
	})
	return out, err
}

func (r *personMergeSuggestionFailureRepository) UpsertWaitingEvidence(req PersonMergeFailureUpsert) (*model.PersonMergeSuggestionFailure, error) {
	if req.TargetPersonID == 0 {
		return nil, errors.New("target_person_id required")
	}
	now := req.Now
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	depsJSON, err := encodeBlockingDeps(stampBlockingDepsObserved(req.BlockingDeps))
	if err != nil {
		return nil, err
	}
	var out *model.PersonMergeSuggestionFailure
	err = r.db.Transaction(func(tx *gorm.DB) error {
		var row model.PersonMergeSuggestionFailure
		err := tx.Where("target_person_id = ?", req.TargetPersonID).First(&row).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			row = model.PersonMergeSuggestionFailure{
				TargetPersonID:     req.TargetPersonID,
				State:              model.PersonMergeFailureStateWaitingEvidence,
				ReasonCode:         req.ReasonCode,
				Attempts:           0,
				FirstFailedAt:      now,
				LastFailedAt:       now,
				TargetProfileGen:   req.TargetProfileGen,
				TargetObsKnown:     true,
				EngineVersion:      req.EngineVersion,
				ConfigFingerprint:  req.ConfigFingerprint,
				BlockingDepsJSON:   depsJSON,
				IndexReadyRequired: req.IndexReadyRequired,
			}
			if err := tx.Create(&row).Error; err != nil {
				return err
			}
			out = &row
			return nil
		}
		if err != nil {
			return err
		}
		if row.State == model.PersonMergeFailureStateResolved {
			row.FirstFailedAt = now
			row.ResolvedAt = nil
			row.ResolvedReason = ""
		}
		row.State = model.PersonMergeFailureStateWaitingEvidence
		row.ReasonCode = req.ReasonCode
		row.LastFailedAt = now
		row.NextRetryAt = nil
		row.TargetProfileGen = req.TargetProfileGen
		row.TargetObsKnown = true
		row.EngineVersion = req.EngineVersion
		row.ConfigFingerprint = req.ConfigFingerprint
		row.BlockingDepsJSON = depsJSON
		row.IndexReadyRequired = req.IndexReadyRequired
		// 不推进 attempts：无证据不烧技术预算。
		if err := tx.Save(&row).Error; err != nil {
			return err
		}
		out = &row
		return nil
	})
	return out, err
}

func (r *personMergeSuggestionFailureRepository) Resolve(targetPersonID uint, reason string) error {
	if targetPersonID == 0 {
		return nil
	}
	now := time.Now().UTC()
	return r.db.Model(&model.PersonMergeSuggestionFailure{}).
		Where("target_person_id = ? AND state <> ?", targetPersonID, model.PersonMergeFailureStateResolved).
		Updates(map[string]interface{}{
			"state":           model.PersonMergeFailureStateResolved,
			"resolved_at":     now,
			"resolved_reason": reason,
			"next_retry_at":   nil,
			"updated_at":      now,
		}).Error
}

func (r *personMergeSuggestionFailureRepository) ResolveIneligible(targetPersonIDs []uint) error {
	ids := dedupIDs(targetPersonIDs)
	if len(ids) == 0 {
		return nil
	}
	now := time.Now().UTC()
	return r.db.Model(&model.PersonMergeSuggestionFailure{}).
		Where("target_person_id IN ? AND state <> ?", ids, model.PersonMergeFailureStateResolved).
		Updates(map[string]interface{}{
			"state":           model.PersonMergeFailureStateResolved,
			"resolved_at":     now,
			"resolved_reason": model.PersonMergeFailureResolvedIneligible,
			"next_retry_at":   nil,
			"updated_at":      now,
		}).Error
}

func (r *personMergeSuggestionFailureRepository) ListDueRetry(now time.Time, limit int) ([]*model.PersonMergeSuggestionFailure, error) {
	var rows []*model.PersonMergeSuggestionFailure
	q := r.db.Where("state = ?", model.PersonMergeFailureStateRetryWait).
		Where("next_retry_at IS NULL OR next_retry_at <= ?", now.UTC()).
		Order("next_retry_at ASC, target_person_id ASC")
	if limit > 0 {
		q = q.Limit(limit)
	}
	if err := q.Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

func (r *personMergeSuggestionFailureRepository) GetByTarget(targetPersonID uint) (*model.PersonMergeSuggestionFailure, error) {
	var row model.PersonMergeSuggestionFailure
	err := r.db.Where("target_person_id = ?", targetPersonID).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &row, nil
}

func (r *personMergeSuggestionFailureRepository) ListUnresolved(page, pageSize int) ([]*model.PersonMergeSuggestionFailure, int64, error) {
	if page < 1 {
		page = 1
	}
	if pageSize < 1 {
		pageSize = 20
	}
	if pageSize > 100 {
		pageSize = 100
	}
	q := r.db.Model(&model.PersonMergeSuggestionFailure{}).
		Where("state <> ?", model.PersonMergeFailureStateResolved)
	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	var rows []*model.PersonMergeSuggestionFailure
	if err := q.Order("last_failed_at DESC, target_person_id ASC").
		Offset((page - 1) * pageSize).Limit(pageSize).Find(&rows).Error; err != nil {
		return nil, 0, err
	}
	return rows, total, nil
}

func (r *personMergeSuggestionFailureRepository) ListUnresolvedKeyset(afterTargetID, maxTargetID uint, limit int) ([]*model.PersonMergeSuggestionFailure, error) {
	if limit < 1 {
		limit = 100
	}
	if limit > 100 {
		limit = 100
	}
	q := r.db.Model(&model.PersonMergeSuggestionFailure{}).
		Where("state <> ?", model.PersonMergeFailureStateResolved).
		Where("target_person_id > ?", afterTargetID)
	if maxTargetID > 0 {
		q = q.Where("target_person_id <= ?", maxTargetID)
	}
	var rows []*model.PersonMergeSuggestionFailure
	if err := q.Order("target_person_id ASC").Limit(limit).Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

func (r *personMergeSuggestionFailureRepository) MaxUnresolvedTargetID() (uint, error) {
	var maxID *uint
	err := r.db.Model(&model.PersonMergeSuggestionFailure{}).
		Where("state <> ?", model.PersonMergeFailureStateResolved).
		Select("MAX(target_person_id)").Scan(&maxID).Error
	if err != nil {
		return 0, err
	}
	if maxID == nil {
		return 0, nil
	}
	return *maxID, nil
}

func (r *personMergeSuggestionFailureRepository) ListTargetIDsByState(state string) ([]uint, error) {
	if state == "" {
		return nil, nil
	}
	var ids []uint
	err := r.db.Model(&model.PersonMergeSuggestionFailure{}).
		Where("state = ?", state).
		Order("target_person_id ASC").
		Pluck("target_person_id", &ids).Error
	return ids, err
}

func (r *personMergeSuggestionFailureRepository) CountUnresolved(now time.Time) (*model.PersonMergeFailureCounts, error) {
	out := &model.PersonMergeFailureCounts{}
	type row struct {
		State string
		N     int64
	}
	var groups []row
	if err := r.db.Model(&model.PersonMergeSuggestionFailure{}).
		Select("state, COUNT(*) as n").
		Where("state <> ?", model.PersonMergeFailureStateResolved).
		Group("state").Scan(&groups).Error; err != nil {
		return nil, err
	}
	for _, g := range groups {
		out.UnresolvedTargets += g.N
		switch g.State {
		case model.PersonMergeFailureStateRetryWait:
			out.RetryTotal = g.N
		case model.PersonMergeFailureStateExhausted:
			out.RetryExhausted = g.N
		case model.PersonMergeFailureStateWaitingEvidence:
			out.WaitingEvidence = g.N
		}
	}
	if out.RetryTotal > 0 {
		var due int64
		if err := r.db.Model(&model.PersonMergeSuggestionFailure{}).
			Where("state = ?", model.PersonMergeFailureStateRetryWait).
			Where("next_retry_at IS NULL OR next_retry_at <= ?", now.UTC()).
			Count(&due).Error; err != nil {
			return nil, err
		}
		out.RetryDue = due
		out.RetryDeferred = out.RetryTotal - due
		if out.RetryDeferred < 0 {
			out.RetryDeferred = 0
		}
	}
	return out, nil
}

func (r *personMergeSuggestionFailureRepository) ManualRetry(targetPersonIDs []uint, reason string, now time.Time) (accepted []uint, skipped []PersonMergeFailureSkip, err error) {
	ids := dedupIDs(targetPersonIDs)
	if len(ids) == 0 {
		return nil, nil, errors.New("target_ids required")
	}
	if len(ids) > 100 {
		return nil, nil, errors.New("target_ids exceeds 100")
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	if reason == "" {
		reason = "manual_retry"
	}
	err = r.db.Transaction(func(tx *gorm.DB) error {
		for _, id := range ids {
			ok, skip, e := applyManualRetryBudgetTx(tx, id, reason, now)
			if e != nil {
				return e
			}
			if skip != "" {
				skipped = append(skipped, PersonMergeFailureSkip{TargetPersonID: id, Reason: skip})
				continue
			}
			if ok {
				accepted = append(accepted, id)
			}
		}
		return nil
	})
	return accepted, skipped, err
}

func (r *personMergeSuggestionFailureRepository) BeginManualRetryArrangement(targetPersonID uint, reason string, remainingPersonIDs []uint, now time.Time) (accepted bool, skipReason string, err error) {
	if targetPersonID == 0 {
		return false, "", errors.New("target_person_id required")
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	if reason == "" {
		reason = "manual_retry"
	}
	remaining := dedupIDs(remainingPersonIDs)
	err = r.db.Transaction(func(tx *gorm.DB) error {
		var row model.PersonMergeSuggestionFailure
		e := tx.Where("target_person_id = ?", targetPersonID).First(&row).Error
		if errors.Is(e, gorm.ErrRecordNotFound) {
			skipReason = "not_found"
			return nil
		}
		if e != nil {
			return e
		}
		if row.State == model.PersonMergeFailureStateResolved {
			skipReason = "already_resolved"
			return nil
		}
		if row.State == model.PersonMergeFailureStateWaitingEvidence &&
			row.ReasonCode == "no_usable_evidence" {
			skipReason = "waiting_evidence"
			return nil
		}
		// 已有未完成安排：不重置预算，接受后由调用方按 remaining 补齐。
		if row.ManualArrangePending {
			accepted = true
			return nil
		}
		// 已安排完成且尚未消耗预算：幂等跳过。
		if row.State == model.PersonMergeFailureStateRetryWait &&
			row.ManualRetriedAt != nil &&
			row.Attempts == 0 {
			skipReason = "already_scheduled"
			return nil
		}
		ok, skip, e := applyManualRetryBudgetTx(tx, targetPersonID, reason, now)
		if e != nil {
			return e
		}
		if skip != "" {
			skipReason = skip
			return nil
		}
		if !ok {
			return nil
		}
		if e := updateManualArrangementTx(tx, targetPersonID, true, remaining); e != nil {
			return e
		}
		accepted = true
		return nil
	})
	return accepted, skipReason, err
}

// applyManualRetryBudgetTx 在已有事务内重开人工预算；调用方负责提交/回滚。
func applyManualRetryBudgetTx(tx *gorm.DB, id uint, reason string, now time.Time) (accepted bool, skipReason string, err error) {
	var row model.PersonMergeSuggestionFailure
	e := tx.Where("target_person_id = ?", id).First(&row).Error
	if errors.Is(e, gorm.ErrRecordNotFound) {
		return false, "not_found", nil
	}
	if e != nil {
		return false, "", e
	}
	if row.State == model.PersonMergeFailureStateResolved {
		return false, "already_resolved", nil
	}
	if row.State == model.PersonMergeFailureStateWaitingEvidence &&
		row.ReasonCode == "no_usable_evidence" {
		return false, "waiting_evidence", nil
	}
	if row.State == model.PersonMergeFailureStateRetryWait &&
		row.ManualRetriedAt != nil &&
		row.Attempts == 0 &&
		!row.ManualArrangePending {
		return false, "already_scheduled", nil
	}
	next := now.Add(mergeSuggestionFailureBackoff(1))
	row.State = model.PersonMergeFailureStateRetryWait
	row.Attempts = 0
	row.NextRetryAt = &next
	row.LastFailedAt = now
	row.ResolvedAt = nil
	row.ResolvedReason = ""
	row.ManualRetriedAt = &now
	row.ManualRetryReason = reason
	if err := tx.Save(&row).Error; err != nil {
		return false, "", err
	}
	return true, "", nil
}

func (r *personMergeSuggestionFailureRepository) ClearManualRetryMarkers(targetPersonIDs []uint) error {
	ids := dedupIDs(targetPersonIDs)
	if len(ids) == 0 {
		return nil
	}
	return r.db.Model(&model.PersonMergeSuggestionFailure{}).
		Where("target_person_id IN ?", ids).
		Updates(map[string]interface{}{
			"manual_retried_at":             nil,
			"manual_retry_reason":           "",
			"manual_arrange_pending":        false,
			"manual_arrange_remaining_json": "[]",
		}).Error
}

func (r *personMergeSuggestionFailureRepository) UpdateManualArrangement(targetPersonID uint, pending bool, remainingPersonIDs []uint) error {
	return updateManualArrangementTx(r.db, targetPersonID, pending, remainingPersonIDs)
}

func updateManualArrangementTx(tx *gorm.DB, targetPersonID uint, pending bool, remainingPersonIDs []uint) error {
	if targetPersonID == 0 {
		return errors.New("target_person_id required")
	}
	if tx == nil {
		return errors.New("tx required")
	}
	remaining := dedupIDs(remainingPersonIDs)
	raw, err := json.Marshal(remaining)
	if err != nil {
		return err
	}
	return tx.Model(&model.PersonMergeSuggestionFailure{}).
		Where("target_person_id = ?", targetPersonID).
		Updates(map[string]interface{}{
			"manual_arrange_pending":        pending,
			"manual_arrange_remaining_json": string(raw),
			"updated_at":                    time.Now().UTC(),
		}).Error
}

func (r *personMergeSuggestionFailureRepository) ImportFromLegacyRetries(entries []LegacyMergeRetryImport, now time.Time) (int, error) {
	if len(entries) == 0 {
		return 0, nil
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	imported := 0
	err := r.db.Transaction(func(tx *gorm.DB) error {
		for _, e := range entries {
			if e.TargetID == 0 {
				continue
			}
			var existing model.PersonMergeSuggestionFailure
			err := tx.Where("target_person_id = ?", e.TargetID).First(&existing).Error
			if err == nil {
				// 已有行：保留较高 attempts，不覆盖 resolved/waiting 为更弱状态。
				continue
			}
			if !errors.Is(err, gorm.ErrRecordNotFound) {
				return err
			}
			attempts := e.Attempts
			if attempts < 1 {
				attempts = 1
			}
			row := model.PersonMergeSuggestionFailure{
				TargetPersonID:   e.TargetID,
				ReasonCode:       e.Reason,
				Attempts:         attempts,
				FirstFailedAt:    now,
				LastFailedAt:     now,
				BlockingDepsJSON: "[]",
			}
			if attempts >= MergeSuggestionFailureMaxAttempts {
				row.State = model.PersonMergeFailureStateExhausted
			} else {
				row.State = model.PersonMergeFailureStateRetryWait
				next := e.NextRetryAt
				if next.IsZero() {
					next = now.Add(mergeSuggestionFailureBackoff(attempts))
				} else {
					next = next.UTC()
				}
				row.NextRetryAt = &next
			}
			if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&row).Error; err != nil {
				return err
			}
			imported++
		}
		return nil
	})
	return imported, err
}

func (r *personMergeSuggestionFailureRepository) WakeForEvidenceChange(personIDs []uint, observedGens map[uint]int, now time.Time) ([]uint, error) {
	// 前提：未解决行数通常为几十；触发频率约每协调器切片一次。
	// 若未来未解决上千行，应改为 SQL 依赖表/索引匹配，而不是全表扫 JSON。
	ids := dedupIDs(personIDs)
	if len(ids) == 0 {
		return nil, nil
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	if observedGens == nil {
		observedGens = map[uint]int{}
	}
	var unresolved []*model.PersonMergeSuggestionFailure
	if err := r.db.Where("state <> ?", model.PersonMergeFailureStateResolved).Find(&unresolved).Error; err != nil {
		return nil, err
	}
	idSet := make(map[uint]struct{}, len(ids))
	for _, id := range ids {
		idSet[id] = struct{}{}
	}
	woken := make([]uint, 0)
	err := r.db.Transaction(func(tx *gorm.DB) error {
		for _, row := range unresolved {
			if row == nil {
				continue
			}
			match := false
			deps, depErr := decodeBlockingDeps(row.BlockingDepsJSON)
			if depErr != nil {
				return depErr
			}
			if _, ok := idSet[row.TargetPersonID]; ok {
				obs := observedGens[row.TargetPersonID]
				if failureObsShouldWake(row.TargetObsKnown, row.TargetProfileGen, row.TargetConsumedGen, obs) {
					match = true
					row.TargetProfileGen = obs
					row.TargetObsKnown = true
					row.TargetConsumedGen = obs
				}
			}
			if !match {
				for i := range deps {
					d := &deps[i]
					if _, ok := idSet[d.PersonID]; !ok {
						continue
					}
					obs := observedGens[d.PersonID]
					if failureObsShouldWake(d.ObsKnown, d.Generation, d.ConsumedGen, obs) {
						match = true
						d.Generation = obs
						d.ObsKnown = true
						d.ConsumedGen = obs
					}
				}
			} else {
				// 目标命中时同步刷新已通知依赖戳记，避免后续同代次重复。
				for i := range deps {
					d := &deps[i]
					if _, ok := idSet[d.PersonID]; !ok {
						continue
					}
					obs := observedGens[d.PersonID]
					if failureObsShouldWake(d.ObsKnown, d.Generation, d.ConsumedGen, obs) {
						d.Generation = obs
						d.ObsKnown = true
						d.ConsumedGen = obs
					} else if obs > 0 && obs >= d.Generation {
						d.Generation = obs
						d.ObsKnown = true
						if obs > d.ConsumedGen {
							d.ConsumedGen = obs
						}
					}
				}
			}
			if !match {
				continue
			}
			raw, encErr := encodeBlockingDeps(deps)
			if encErr != nil {
				return encErr
			}
			next := now
			row.State = model.PersonMergeFailureStateRetryWait
			row.Attempts = 0
			row.NextRetryAt = &next
			row.ResolvedAt = nil
			row.ResolvedReason = ""
			row.ManualRetriedAt = nil
			row.ManualRetryReason = ""
			row.LastFailedAt = now
			row.UpdatedAt = now
			row.BlockingDepsJSON = raw
			if err := tx.Save(row).Error; err != nil {
				return err
			}
			woken = append(woken, row.TargetPersonID)
		}
		return nil
	})
	return woken, err
}

// failureObsShouldWake 判定是否应对观测到的可用代次唤醒：
// - 历史未知（!known）：首次看到可用代次 → 建基线并必要重验
// - 已确认不可用/尚无 gen（known && recorded==0）：0→正数首次恢复
// - 已知可用版本前进：obs > recorded
// - 同代次已消费：不唤醒
func failureObsShouldWake(obsKnown bool, recordedGen, consumedGen, obs int) bool {
	if obs <= 0 {
		return false
	}
	if obs <= consumedGen && obsKnown {
		return false
	}
	if !obsKnown {
		return true
	}
	return obs > recordedGen
}

func (r *personMergeSuggestionFailureRepository) WakeForIndexReady(now time.Time) ([]uint, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	var rows []*model.PersonMergeSuggestionFailure
	if err := r.db.Where("index_ready_required = ? AND state IN ?", true, []string{
		model.PersonMergeFailureStateRetryWait,
		model.PersonMergeFailureStateExhausted,
	}).Find(&rows).Error; err != nil {
		return nil, err
	}
	woken := make([]uint, 0, len(rows))
	err := r.db.Transaction(func(tx *gorm.DB) error {
		for _, row := range rows {
			if row == nil {
				continue
			}
			next := now
			// 真实 unavailable→ready：exhausted 有界重开；retry_wait 仅提前到点并清标记。
			if row.State == model.PersonMergeFailureStateExhausted {
				row.State = model.PersonMergeFailureStateRetryWait
				row.Attempts = 0
			}
			row.NextRetryAt = &next
			row.IndexReadyRequired = false
			row.ManualRetriedAt = nil
			row.ManualRetryReason = ""
			row.UpdatedAt = now
			if err := tx.Save(row).Error; err != nil {
				return err
			}
			woken = append(woken, row.TargetPersonID)
		}
		return nil
	})
	return woken, err
}

func (r *personMergeSuggestionFailureRepository) ReconcileDependencyVersions(observedGens map[uint]int, indexReady bool, now time.Time) ([]uint, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	if observedGens == nil {
		observedGens = map[uint]int{}
	}
	var unresolved []*model.PersonMergeSuggestionFailure
	if err := r.db.Where("state <> ?", model.PersonMergeFailureStateResolved).Find(&unresolved).Error; err != nil {
		return nil, err
	}
	return r.reconcileDependencyVersionsRows(unresolved, observedGens, indexReady, now)
}

// ReconcileDependencyVersionsRows 对已拉取的一批未解决行做代次核对（供 keyset 分页调用）。
// indexReady 仅在整轮末尾由调用方传入 true 一次，避免每批重复 WakeForIndexReady。
func (r *personMergeSuggestionFailureRepository) ReconcileDependencyVersionsRows(
	rows []*model.PersonMergeSuggestionFailure,
	observedGens map[uint]int,
	indexReady bool,
	now time.Time,
) ([]uint, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	if observedGens == nil {
		observedGens = map[uint]int{}
	}
	return r.reconcileDependencyVersionsRows(rows, observedGens, indexReady, now)
}

func (r *personMergeSuggestionFailureRepository) reconcileDependencyVersionsRows(
	unresolved []*model.PersonMergeSuggestionFailure,
	observedGens map[uint]int,
	indexReady bool,
	now time.Time,
) ([]uint, error) {
	// 观测语义：未知/确认不可用(gen=0)/版本前进均可唤醒；同代次已消费不赠预算。
	// failed/dirty 残留高 gen 不会进入 observedGens（由 service lookup 的 ready 过滤）。
	advanceIDs := make([]uint, 0)
	advanceGens := make(map[uint]int)
	for _, row := range unresolved {
		if row == nil {
			continue
		}
		if obs := observedGens[row.TargetPersonID]; failureObsShouldWake(row.TargetObsKnown, row.TargetProfileGen, row.TargetConsumedGen, obs) {
			advanceIDs = append(advanceIDs, row.TargetPersonID)
			advanceGens[row.TargetPersonID] = obs
		}
		deps, depErr := decodeBlockingDeps(row.BlockingDepsJSON)
		if depErr != nil {
			return nil, depErr
		}
		for _, d := range deps {
			if d.PersonID == 0 {
				continue
			}
			if obs := observedGens[d.PersonID]; failureObsShouldWake(d.ObsKnown, d.Generation, d.ConsumedGen, obs) {
				advanceIDs = append(advanceIDs, d.PersonID)
				advanceGens[d.PersonID] = obs
			}
		}
	}
	woken := make([]uint, 0)
	if len(advanceIDs) > 0 {
		wokenEvidence, err := r.WakeForEvidenceChange(advanceIDs, advanceGens, now)
		if err != nil {
			return nil, err
		}
		woken = append(woken, wokenEvidence...)
	}
	if indexReady {
		wokenIndex, idxErr := r.WakeForIndexReady(now)
		if idxErr != nil {
			return woken, idxErr
		}
		woken = append(woken, wokenIndex...)
	}
	return dedupIDs(woken), nil
}

func decodeBlockingDeps(raw string) ([]model.PersonMergeFailureBlockingDep, error) {
	if raw == "" || raw == "[]" {
		return nil, nil
	}
	var deps []model.PersonMergeFailureBlockingDep
	if err := json.Unmarshal([]byte(raw), &deps); err != nil {
		return nil, err
	}
	return deps, nil
}

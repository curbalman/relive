package service

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/davidhoo/relive/internal/model"
	"github.com/davidhoo/relive/internal/repository"
	"github.com/davidhoo/relive/pkg/config"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// 第二轮验收反例 S2–S5（源自 /tmp/relive-acceptance-20260915 overlay）。
// 正式入库后未修代码应失败；S1–S5 返修后全部通过。

type reacceptancePartialWriteRepo struct {
	repository.PersonMergeSuggestionFailureRepository
	calls  int
	failAt int
}

func (r *reacceptancePartialWriteRepo) UpsertTechnicalFailure(req repository.PersonMergeFailureUpsert) (*model.PersonMergeSuggestionFailure, error) {
	r.calls++
	if r.calls == r.failAt {
		return nil, errors.New("injected second failure ledger write")
	}
	return r.PersonMergeSuggestionFailureRepository.UpsertTechnicalFailure(req)
}

// TestReacceptance_PartialFailureBatchReplayDoesNotBurnBudget — S4：批内部分提交后重放不得双计 attempts。
func TestReacceptance_PartialFailureBatchReplayDoesNotBurnBudget(t *testing.T) {
	iface, _, _, _ := newPersonMergeSuggestionServiceWithConfigForTest(t, config.PeopleConfig{})
	svc := iface.(*personMergeSuggestionService)
	realRepo := svc.failureRepo
	svc.failureRepo = &reacceptancePartialWriteRepo{PersonMergeSuggestionFailureRepository: realRepo, failAt: 2}
	batch := failureIntentBatch{
		TechnicalIDs:     []uint{1001, 1002},
		TechnicalReasons: map[uint]string{1001: blockProfileUnavailable, 1002: blockProfileUnavailable},
		Now:              time.Now().UTC(),
	}
	require.Error(t, svc.applyFailureIntents(batch))
	first, err := realRepo.GetByTarget(1001)
	require.NoError(t, err)
	require.Equal(t, 1, first.Attempts)
	require.NoError(t, svc.applyFailureIntents(batch))
	replayed, err := realRepo.GetByTarget(1001)
	require.NoError(t, err)
	require.Equal(t, first.Attempts, replayed.Attempts,
		"replay of same partially committed batch must not consume target budget twice")
}

type reacceptanceReadErrorRepo struct {
	repository.PersonMergeSuggestionFailureRepository
}

func (r reacceptanceReadErrorRepo) GetByTarget(id uint) (*model.PersonMergeSuggestionFailure, error) {
	return nil, errors.New("injected failure dependency read error")
}

// TestReacceptance_ManualRetryDependencyReadErrorMustPropagate — S5：依赖读取失败不得虚报 accepted。
func TestReacceptance_ManualRetryDependencyReadErrorMustPropagate(t *testing.T) {
	iface, _, _, _ := newPersonMergeSuggestionServiceWithConfigForTest(t, config.PeopleConfig{})
	svc := iface.(*personMergeSuggestionService)
	_, err := svc.failureRepo.UpsertTechnicalFailure(repository.PersonMergeFailureUpsert{
		TargetPersonID: 1001,
		ReasonCode:     blockProfileUnavailable,
		BlockingDeps:   []model.PersonMergeFailureBlockingDep{{PersonID: 1002, Generation: 1}},
		Now:            time.Now().UTC(),
	})
	require.NoError(t, err)
	svc.failureRepo = reacceptanceReadErrorRepo{svc.failureRepo}
	var scheduled []uint
	svc.SetProfileRebuildHook(func(ids []uint, reason string) error {
		scheduled = append(scheduled, ids...)
		return nil
	})
	_, err = svc.RetryFailures([]uint{1001}, "manual_retry")
	require.Error(t, err, "cannot return accepted when blocking dependency lookup failed; scheduled=%v", scheduled)
}

// TestReacceptance_ReconcileMustVisitBeyondFirst100 — S3：keyset 必须覆盖第 101 条。
func TestReacceptance_ReconcileMustVisitBeyondFirst100(t *testing.T) {
	iface, db, _, _ := newPersonMergeSuggestionServiceWithConfigForTest(t, config.PeopleConfig{
		IdentityProfileMode: model.PeopleIdentityModePrimary,
	})
	svc := iface.(*personMergeSuggestionService)
	require.NoError(t, db.AutoMigrate(
		&model.PersonIdentityProfile{},
		&model.PersonIdentityCenter{},
		&model.PersonIdentityCenterMember{},
	))
	svc.AttachIdentityMatchingWiring(NewIdentityMatchingEngine(
		newIdentityProfileANN("emb-v1"),
		repository.NewPersonIdentityProfileRepository(db),
		repository.NewFaceRepository(db),
		repository.NewCannotLinkRepository(db),
		repository.NewFaceRepository(db),
		IdentityProfileMatcherConfig{EmbeddingModel: "emb-v1"},
	), false)

	now := time.Now().UTC()
	var oldest uint
	for i := 0; i < 101; i++ {
		p := createMatcherPerson(t, db)
		seedReadyEmptyProfile(t, db, p.ID, "emb-v1", model.PersonIdentityBuildOutcomeNoUsableEvidence)
		require.NoError(t, db.Model(&model.PersonIdentityProfile{}).Where("person_id = ?", p.ID).
			Update("active_generation", 2).Error)
		recorded := 2
		if i == 100 {
			recorded = 1
			oldest = p.ID
		}
		require.NoError(t, db.Create(&model.PersonMergeSuggestionFailure{
			TargetPersonID:   p.ID,
			State:            model.PersonMergeFailureStateExhausted,
			ReasonCode:       blockProfileUnavailable,
			Attempts:         5,
			TargetProfileGen: recorded,
			TargetObsKnown:   true,
			LastFailedAt:     now.Add(-time.Duration(i) * time.Hour),
			FirstFailedAt:    now.Add(-time.Duration(i) * time.Hour),
			BlockingDepsJSON: "[]",
		}).Error)
	}
	// 强制完整核对（绕过「启动时若已 Attach 过」的空跑）：重置进度后请求。
	svc.mu.Lock()
	svc.state.ReconcilePending = true
	svc.state.ReconcileAfterID = 0
	svc.state.ReconcileRoundMaxID = 0
	svc.mu.Unlock()
	svc.RequestUnresolvedDependencyReconcile(false)

	row, err := svc.failureRepo.GetByTarget(oldest)
	require.NoError(t, err)
	require.Equal(t, model.PersonMergeFailureStateRetryWait, row.State,
		"oldest row beyond page 1 has recovered generation and must be reconciled")
}

// TestReacceptance_StartupWiringReconcilesAfterEngine — S1：必须走共享装配入口，引擎注入后才核对。
func TestReacceptance_StartupWiringReconcilesAfterEngine(t *testing.T) {
	iface, db, _, _ := newPersonMergeSuggestionServiceWithConfigForTest(t, config.PeopleConfig{
		IdentityProfileMode: model.PeopleIdentityModePrimary,
	})
	svc := iface.(*personMergeSuggestionService)
	require.NoError(t, db.AutoMigrate(
		&model.PersonIdentityProfile{},
		&model.PersonIdentityCenter{},
		&model.PersonIdentityCenterMember{},
	))

	p := createMatcherPerson(t, db)
	seedReadyEmptyProfile(t, db, p.ID, "emb-v1", model.PersonIdentityBuildOutcomeNoUsableEvidence)
	require.NoError(t, db.Model(&model.PersonIdentityProfile{}).Where("person_id = ?", p.ID).
		Update("active_generation", 1).Error)

	now := time.Now().UTC()
	// 五次失败耗尽，记录代次 0（画像当时不可用）。
	for i := 0; i < repository.MergeSuggestionFailureMaxAttempts; i++ {
		_, err := svc.failureRepo.UpsertTechnicalFailure(repository.PersonMergeFailureUpsert{
			TargetPersonID:   p.ID,
			TargetProfileGen: 0,
			ReasonCode:       "profile_unavailable",
			Now:              now.Add(time.Duration(i) * time.Hour),
		})
		require.NoError(t, err)
	}
	before, err := svc.failureRepo.GetByTarget(p.ID)
	require.NoError(t, err)
	require.Equal(t, model.PersonMergeFailureStateExhausted, before.State)

	// 模拟「通知前退出」：不调用 NotifyProfileEvidenceReady，只走生产装配入口。
	svc.AttachIdentityMatchingWiring(NewIdentityMatchingEngine(
		newIdentityProfileANN("emb-v1"),
		repository.NewPersonIdentityProfileRepository(db),
		repository.NewFaceRepository(db),
		repository.NewCannotLinkRepository(db),
		repository.NewFaceRepository(db),
		IdentityProfileMatcherConfig{EmbeddingModel: "emb-v1"},
	), false)

	after, err := svc.failureRepo.GetByTarget(p.ID)
	require.NoError(t, err)
	require.Equal(t, model.PersonMergeFailureStateRetryWait, after.State,
		"startup wiring after engine inject must reconcile first recovery 0→1")
}

// TestReacceptance_ReconcileContinuesAcrossBackgroundSlices — S3 补充：跨切片从持久化 cursor 续跑。
func TestReacceptance_ReconcileContinuesAcrossBackgroundSlices(t *testing.T) {
	iface, db, _, _ := newPersonMergeSuggestionServiceWithConfigForTest(t, config.PeopleConfig{
		IdentityProfileMode: model.PeopleIdentityModePrimary,
	})
	svc := iface.(*personMergeSuggestionService)
	require.NoError(t, db.AutoMigrate(
		&model.PersonIdentityProfile{},
		&model.PersonIdentityCenter{},
		&model.PersonIdentityCenterMember{},
	))
	svc.AttachIdentityMatchingWiring(NewIdentityMatchingEngine(
		newIdentityProfileANN("emb-v1"),
		repository.NewPersonIdentityProfileRepository(db),
		repository.NewFaceRepository(db),
		repository.NewCannotLinkRepository(db),
		repository.NewFaceRepository(db),
		IdentityProfileMatcherConfig{EmbeddingModel: "emb-v1"},
	), false)
	svc.reconcileBatchesPerSlice = 1 // 每切片只跑 1 批，强制续跑

	now := time.Now().UTC()
	var oldest uint
	for i := 0; i < 101; i++ {
		p := createMatcherPerson(t, db)
		seedReadyEmptyProfile(t, db, p.ID, "emb-v1", model.PersonIdentityBuildOutcomeNoUsableEvidence)
		require.NoError(t, db.Model(&model.PersonIdentityProfile{}).Where("person_id = ?", p.ID).
			Update("active_generation", 2).Error)
		recorded := 2
		if i == 100 {
			recorded = 1
			oldest = p.ID
		}
		require.NoError(t, db.Create(&model.PersonMergeSuggestionFailure{
			TargetPersonID:   p.ID,
			State:            model.PersonMergeFailureStateExhausted,
			ReasonCode:       blockProfileUnavailable,
			Attempts:         5,
			TargetProfileGen: recorded,
			TargetObsKnown:   true,
			LastFailedAt:     now.Add(-time.Duration(i) * time.Hour),
			FirstFailedAt:    now.Add(-time.Duration(i) * time.Hour),
			BlockingDepsJSON: "[]",
		}).Error)
	}

	svc.mu.Lock()
	svc.state.ReconcilePending = true
	svc.state.ReconcileAfterID = 0
	svc.state.ReconcileRoundMaxID = 0
	svc.mu.Unlock()

	svc.runUnresolvedDependencyReconcile(true)
	svc.mu.RLock()
	pendingAfterFirst := svc.state.ReconcilePending
	afterID := svc.state.ReconcileAfterID
	svc.mu.RUnlock()
	require.True(t, pendingAfterFirst, "first slice must leave reconcile pending")
	require.NotZero(t, afterID, "first slice must advance keyset cursor")

	mid, err := svc.failureRepo.GetByTarget(oldest)
	require.NoError(t, err)
	require.Equal(t, model.PersonMergeFailureStateExhausted, mid.State,
		"oldest row must still be untouched after first partial slice")

	svc.runUnresolvedDependencyReconcile(true)
	row, err := svc.failureRepo.GetByTarget(oldest)
	require.NoError(t, err)
	require.Equal(t, model.PersonMergeFailureStateRetryWait, row.State,
		"second slice must resume from cursor and wake the last-page recovery")
}

// TestReacceptance_NewAttemptKeysIncrementToExhaustion — S4：真实新尝试必须递增至耗尽。
func TestReacceptance_NewAttemptKeysIncrementToExhaustion(t *testing.T) {
	iface, _, _, _ := newPersonMergeSuggestionServiceWithConfigForTest(t, config.PeopleConfig{})
	svc := iface.(*personMergeSuggestionService)
	now := time.Now().UTC()
	const tid uint = 3001
	for i := 1; i <= repository.MergeSuggestionFailureMaxAttempts; i++ {
		key := fmt.Sprintf("dg-1-seq-%d", i)
		require.NoError(t, svc.applyFailureIntents(failureIntentBatch{
			TechnicalIDs:     []uint{tid},
			TechnicalReasons: map[uint]string{tid: blockProfileUnavailable},
			AttemptKey:       key,
			Now:              now.Add(time.Duration(i) * time.Minute),
		}))
		row, err := svc.failureRepo.GetByTarget(tid)
		require.NoError(t, err)
		require.Equal(t, i, row.Attempts, "attempt key %s", key)
		if i < repository.MergeSuggestionFailureMaxAttempts {
			require.Equal(t, model.PersonMergeFailureStateRetryWait, row.State)
		} else {
			require.Equal(t, model.PersonMergeFailureStateExhausted, row.State)
		}
	}
}

// TestReacceptance_FailureAttemptKeySurvivesRestartReplay — S4：意图落库后、cursor 轮换前退出，重启重放不双计。
func TestReacceptance_FailureAttemptKeySurvivesRestartReplay(t *testing.T) {
	iface, db, repos, cfgSvc := newPersonMergeSuggestionServiceWithConfigForTest(t, config.PeopleConfig{})
	svc := iface.(*personMergeSuggestionService)
	batch := failureIntentBatch{
		TechnicalIDs:     []uint{4001, 4002},
		TechnicalReasons: map[uint]string{4001: blockProfileUnavailable, 4002: blockProfileUnavailable},
		Now:              time.Now().UTC(),
	}
	partial := &reacceptancePartialWriteRepo{PersonMergeSuggestionFailureRepository: svc.failureRepo, failAt: 2}
	svc.failureRepo = partial
	require.Error(t, svc.applyFailureIntents(batch))
	first, err := repos.MergeSuggestionFailure.GetByTarget(4001)
	require.NoError(t, err)
	require.Equal(t, 1, first.Attempts)
	require.NotEmpty(t, first.LastAttemptKey)

	// 模拟进程退出后重建服务（读回已持久化的 FailureAttemptKey）。
	svc2 := NewPersonMergeSuggestionService(
		db, repos.Photo, repos.Face, repos.Person, repos.PeopleJob, repos.CannotLink, repos.MergeSuggestion, cfgSvc,
		&config.Config{People: config.PeopleConfig{}},
	).(*personMergeSuggestionService)
	require.NotEmpty(t, svc2.state.FailureAttemptKey, "restart must reload attempt key from state")
	require.Equal(t, first.LastAttemptKey, svc2.state.FailureAttemptKey)
	require.NoError(t, svc2.applyFailureIntents(failureIntentBatch{
		TechnicalIDs:     []uint{4001, 4002},
		TechnicalReasons: map[uint]string{4001: blockProfileUnavailable, 4002: blockProfileUnavailable},
		Now:              time.Now().UTC(),
	}))
	replayed, err := repos.MergeSuggestionFailure.GetByTarget(4001)
	require.NoError(t, err)
	require.Equal(t, 1, replayed.Attempts)
}

// TestReacceptance_StaleAttemptKeyDoesNotOverrideNewerRound — S4：旧意图晚到不得覆盖新轮次/resolved。
func TestReacceptance_StaleAttemptKeyDoesNotOverrideNewerRound(t *testing.T) {
	iface, _, _, _ := newPersonMergeSuggestionServiceWithConfigForTest(t, config.PeopleConfig{})
	svc := iface.(*personMergeSuggestionService)
	now := time.Now().UTC()
	const tid uint = 5001
	require.NoError(t, svc.applyFailureIntents(failureIntentBatch{
		TechnicalIDs: []uint{tid}, TechnicalReasons: map[uint]string{tid: blockProfileUnavailable},
		AttemptKey: "dg-1-seq-1", Now: now,
	}))
	require.NoError(t, svc.applyFailureIntents(failureIntentBatch{
		TechnicalIDs: []uint{tid}, TechnicalReasons: map[uint]string{tid: blockProfileUnavailable},
		AttemptKey: "dg-1-seq-2", Now: now.Add(time.Minute),
	}))
	row, err := svc.failureRepo.GetByTarget(tid)
	require.NoError(t, err)
	require.Equal(t, 2, row.Attempts)
	require.NoError(t, svc.failureRepo.Resolve(tid, model.PersonMergeFailureResolvedZeroCandidate))

	// 旧 key 晚到：不得重新打开 resolved。
	require.NoError(t, svc.applyFailureIntents(failureIntentBatch{
		TechnicalIDs: []uint{tid}, TechnicalReasons: map[uint]string{tid: blockProfileUnavailable},
		AttemptKey: "dg-1-seq-1", Now: now.Add(2 * time.Minute),
	}))
	after, err := svc.failureRepo.GetByTarget(tid)
	require.NoError(t, err)
	require.Equal(t, model.PersonMergeFailureStateResolved, after.State)
	require.Equal(t, 2, after.Attempts)
}

type reacceptanceArrangeHookRepo struct {
	repository.PersonMergeSuggestionFailureRepository
	failBeginAfter  int
	beginCalls      int
	failUpdateAfter int
	updateCalls     int
}

func (r *reacceptanceArrangeHookRepo) BeginManualRetryArrangement(targetPersonID uint, reason string, remainingPersonIDs []uint, now time.Time) (bool, string, error) {
	r.beginCalls++
	if r.failBeginAfter > 0 && r.beginCalls == r.failBeginAfter {
		return false, "", errors.New("injected arrangement save failure")
	}
	return r.PersonMergeSuggestionFailureRepository.BeginManualRetryArrangement(targetPersonID, reason, remainingPersonIDs, now)
}

func (r *reacceptanceArrangeHookRepo) UpdateManualArrangement(targetPersonID uint, pending bool, remaining []uint) error {
	r.updateCalls++
	if r.failUpdateAfter > 0 && r.updateCalls == r.failUpdateAfter {
		return errors.New("injected arrangement save failure")
	}
	return r.PersonMergeSuggestionFailureRepository.UpdateManualArrangement(targetPersonID, pending, remaining)
}

// TestReacceptance_ManualRetryMidDependencyFailureThenResume — S5：依赖中途 MarkDirty 失败后可补齐。
func TestReacceptance_ManualRetryMidDependencyFailureThenResume(t *testing.T) {
	iface, _, _, _ := newPersonMergeSuggestionServiceWithConfigForTest(t, config.PeopleConfig{})
	svc := iface.(*personMergeSuggestionService)
	_, err := svc.failureRepo.UpsertTechnicalFailure(repository.PersonMergeFailureUpsert{
		TargetPersonID: 6001,
		ReasonCode:     blockProfileUnavailable,
		BlockingDeps:   []model.PersonMergeFailureBlockingDep{{PersonID: 6002, Generation: 1}, {PersonID: 6003, Generation: 1}},
		Now:            time.Now().UTC(),
	})
	require.NoError(t, err)

	var dirtyCalls []uint
	failAt := 2 // 第 2 个 MarkDirty 失败（目标后第一个依赖）
	svc.manualArrangeAfterDirtyHook = func(tx *gorm.DB, targetPersonID, personID uint) error {
		dirtyCalls = append(dirtyCalls, personID)
		if len(dirtyCalls) == failAt {
			return errors.New("injected MarkDirty failure")
		}
		return nil
	}
	_, err = svc.RetryFailures([]uint{6001}, "manual_retry")
	require.Error(t, err)
	row, err := svc.failureRepo.GetByTarget(6001)
	require.NoError(t, err)
	require.True(t, row.ManualArrangePending)
	require.Contains(t, row.ManualArrangeRemainingJSON, "6002")

	dirtyCalls = nil
	failAt = 0
	svc.manualArrangeAfterDirtyHook = func(tx *gorm.DB, targetPersonID, personID uint) error {
		dirtyCalls = append(dirtyCalls, personID)
		return nil
	}
	resp, err := svc.RetryFailures([]uint{6001}, "manual_retry")
	require.NoError(t, err)
	require.Contains(t, resp.AcceptedTargetIDs, uint(6001))
	require.NotContains(t, dirtyCalls, uint(6001), "already-dirtied target must not be MarkDirty again")
	require.Contains(t, dirtyCalls, uint(6002))
	require.Contains(t, dirtyCalls, uint(6003))
	done, err := svc.failureRepo.GetByTarget(6001)
	require.NoError(t, err)
	require.False(t, done.ManualArrangePending)
}

// TestReacceptance_ManualRetryConcurrentAlreadyScheduled — S5：安排完成后并发重复请求 already_scheduled。
func TestReacceptance_ManualRetryConcurrentAlreadyScheduled(t *testing.T) {
	iface, _, _, _ := newPersonMergeSuggestionServiceWithConfigForTest(t, config.PeopleConfig{})
	svc := iface.(*personMergeSuggestionService)
	_, err := svc.failureRepo.UpsertTechnicalFailure(repository.PersonMergeFailureUpsert{
		TargetPersonID: 7001, ReasonCode: blockProfileUnavailable, Now: time.Now().UTC(),
	})
	require.NoError(t, err)
	svc.SetProfileInvalidateHook(func(personIDs []uint) {})
	resp1, err := svc.RetryFailures([]uint{7001}, "manual_retry")
	require.NoError(t, err)
	require.Contains(t, resp1.AcceptedTargetIDs, uint(7001))
	resp2, err := svc.RetryFailures([]uint{7001}, "manual_retry")
	require.NoError(t, err)
	require.Empty(t, resp2.AcceptedTargetIDs)
	require.Len(t, resp2.Skipped, 1)
	require.Equal(t, "already_scheduled", resp2.Skipped[0].Reason)
}

// TestReacceptance_ManualRetryArrangementSaveFailurePropagates — S5：安排状态保存失败必须返回 error。
func TestReacceptance_ManualRetryArrangementSaveFailurePropagates(t *testing.T) {
	iface, _, _, _ := newPersonMergeSuggestionServiceWithConfigForTest(t, config.PeopleConfig{})
	svc := iface.(*personMergeSuggestionService)
	_, err := svc.failureRepo.UpsertTechnicalFailure(repository.PersonMergeFailureUpsert{
		TargetPersonID: 8001, ReasonCode: blockProfileUnavailable,
		BlockingDeps: []model.PersonMergeFailureBlockingDep{{PersonID: 8002, Generation: 1}},
		Now:          time.Now().UTC(),
	})
	require.NoError(t, err)
	svc.failureRepo = &reacceptanceArrangeHookRepo{
		PersonMergeSuggestionFailureRepository: svc.failureRepo,
		failBeginAfter:                         1,
	}
	var dirtied int
	svc.manualArrangeAfterDirtyHook = func(tx *gorm.DB, targetPersonID, personID uint) error {
		dirtied++
		return nil
	}
	_, err = svc.RetryFailures([]uint{8001}, "manual_retry")
	require.Error(t, err)
	require.Zero(t, dirtied, "must not MarkDirty before arrangement checkpoint is saved")
	row, err := svc.failureRepo.GetByTarget(8001)
	require.NoError(t, err)
	require.Nil(t, row.ManualRetriedAt, "initial arrangement failure must roll back manual budget reopen")
	require.False(t, row.ManualArrangePending)
}

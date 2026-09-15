package service

import (
	"errors"
	"testing"
	"time"

	"github.com/davidhoo/relive/internal/model"
	"github.com/davidhoo/relive/internal/repository"
	"github.com/davidhoo/relive/pkg/config"
	"github.com/stretchr/testify/require"
)

// 验收反例 R1–R4（源自 /tmp/relive-acceptance-20260914 overlay）。
// 正式入库后未修代码应失败；返修后全部通过。

type acceptanceFailWriteRepo struct {
	repository.PersonMergeSuggestionFailureRepository
}

func (r acceptanceFailWriteRepo) UpsertWaitingEvidence(req repository.PersonMergeFailureUpsert) (*model.PersonMergeSuggestionFailure, error) {
	return nil, errors.New("acceptance injected write failure")
}

func (r acceptanceFailWriteRepo) UpsertTechnicalFailure(req repository.PersonMergeFailureUpsert) (*model.PersonMergeSuggestionFailure, error) {
	return nil, errors.New("acceptance injected write failure")
}

// TestAcceptance_FailureWriteMustPropagate — R1：失败表写入错误必须传到调用方，扫描不得推进。
func TestAcceptance_FailureWriteMustPropagate(t *testing.T) {
	peopleCfg := config.PeopleConfig{
		MergeSuggestionThreshold:       0.55,
		IdentityProfileRescueThreshold: 0.65,
		IdentityProfileMargin:          0.05,
		IdentityProfileMinCenterFaces:  3,
		MergeSuggestionBatchSize:       10,
		IdentityProfileMode:            model.PeopleIdentityModePrimary,
	}
	svcIface, db, _, _ := newPersonMergeSuggestionServiceWithConfigForTest(t, peopleCfg)
	svc := svcIface.(*personMergeSuggestionService)
	require.NoError(t, db.AutoMigrate(
		&model.PersonIdentityProfile{},
		&model.PersonIdentityCenter{},
		&model.PersonIdentityCenterMember{},
	))

	target := createMatcherPerson(t, db)
	require.NoError(t, db.Model(target).Updates(map[string]interface{}{
		"category": model.PersonCategoryFamily, "face_count": 2, "hidden": false,
	}).Error)
	seedReadyEmptyProfile(t, db, target.ID, "emb-v1", model.PersonIdentityBuildOutcomeNoUsableEvidence)

	engine := NewIdentityMatchingEngine(
		newIdentityProfileANN("emb-v1"),
		repository.NewPersonIdentityProfileRepository(db),
		repository.NewFaceRepository(db),
		repository.NewCannotLinkRepository(db),
		repository.NewFaceRepository(db),
		IdentityProfileMatcherConfig{EmbeddingModel: "emb-v1", RescueThreshold: 0.65, Margin: 0.05, MinCenterFaces: 3},
	)
	svc.SetIdentityMatchingEngine(engine)
	svc.SetIdentityProfileMode(model.PeopleIdentityModePrimary)
	svc.SetIdentitySuggestStrategy(NewIdentitySuggestStrategy(peopleCfg))

	svc.failureRepo = acceptanceFailWriteRepo{svc.failureRepo}
	_, err := svc.primaryAssignments([]*model.Person{target}, map[uint]map[uint]bool{})
	require.Error(t, err, "failed ledger write must reach caller so scan cannot advance cursor")
}

type acceptanceFailResolveRepo struct {
	repository.PersonMergeSuggestionFailureRepository
}

func (r acceptanceFailResolveRepo) Resolve(targetPersonID uint, reason string) error {
	return errors.New("acceptance injected resolve failure")
}

// TestAcceptance_ResolveFailureRestoresPending — R1：Resolve 失败须恢复 pendingFailureResolves 供重放。
func TestAcceptance_ResolveFailureRestoresPending(t *testing.T) {
	peopleCfg := config.PeopleConfig{
		MergeSuggestionThreshold:       0.55,
		IdentityProfileRescueThreshold: 0.65,
		IdentityProfileMargin:          0.05,
		IdentityProfileMinCenterFaces:  3,
		MergeSuggestionBatchSize:       10,
		IdentityProfileMode:            model.PeopleIdentityModePrimary,
	}
	svcIface, db, _, _ := newPersonMergeSuggestionServiceWithConfigForTest(t, peopleCfg)
	svc := svcIface.(*personMergeSuggestionService)
	require.NoError(t, db.AutoMigrate(
		&model.PersonIdentityProfile{},
		&model.PersonIdentityCenter{},
		&model.PersonIdentityCenterMember{},
	))

	target := createMatcherPerson(t, db)
	require.NoError(t, db.Model(target).Updates(map[string]interface{}{
		"category": model.PersonCategoryFamily, "face_count": 2, "hidden": false,
	}).Error)
	centers := seedActiveProfile(t, db, target.ID, "emb-v1", []centerSpec{
		{emb: []float32{1, 0, 0}, supportCount: 8, p10: 0.4},
	})
	ann := buildMatcherANN(t, "emb-v1", centers)
	engine := NewIdentityMatchingEngine(
		ann,
		repository.NewPersonIdentityProfileRepository(db),
		repository.NewFaceRepository(db),
		repository.NewCannotLinkRepository(db),
		repository.NewFaceRepository(db),
		IdentityProfileMatcherConfig{EmbeddingModel: "emb-v1", RescueThreshold: 0.65, Margin: 0.05, MinCenterFaces: 3},
	)
	svc.SetIdentityMatchingEngine(engine)
	svc.SetIdentityProfileMode(model.PeopleIdentityModePrimary)
	svc.SetIdentitySuggestStrategy(NewIdentitySuggestStrategy(peopleCfg))

	_, err := svc.primaryAssignments([]*model.Person{target}, map[uint]map[uint]bool{})
	require.NoError(t, err)
	require.Contains(t, svc.pendingFailureResolves, target.ID)

	svc.failureRepo = acceptanceFailResolveRepo{svc.failureRepo}
	svc.mu.Lock()
	svc.state.Dirty = true
	svc.state.DirtyGeneration = 1
	svc.state.CursorTargetID = 0
	svc.mu.Unlock()

	err = svc.RunBackgroundSlice()
	require.Error(t, err)
	svc.mu.Lock()
	require.Contains(t, svc.pendingFailureResolves, target.ID, "resolve failure must restore pending for idempotent replay")
	require.Equal(t, uint(0), svc.state.CursorTargetID, "cursor must stay when resolve fails")
	svc.mu.Unlock()
}

// TestAcceptance_LegacyEmptyProfileGetsBoundedVerification — R4：未分类 ready 空画像须进入有界核验批次。
func TestAcceptance_LegacyEmptyProfileGetsBoundedVerification(t *testing.T) {
	svc, deps := newCoordinatorTestService(t)
	p := createPersonWithFaces(t, deps.repos, 500, vec3(1, 0, 0), vec3(0.98, 0.01, 0), vec3(0.99, 0, 0.01))
	seedReadyEmptyProfile(t, deps.db, p.ID, "emb", "")
	svc.coordinator.admitHistoricalFailed(time.Now())
	rows, err := svc.coordinator.selectDirtyBatch()
	require.NoError(t, err)
	ids := make([]uint, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.PersonID)
	}
	require.Contains(t, ids, p.ID, "unclassified legacy ready-empty profile needs bounded builder verification")
}

type acceptanceFailNthReplaceRepo struct {
	repository.PersonMergeSuggestionRepository
	failAt int
	calls  int
}

func (r *acceptanceFailNthReplaceRepo) ReplacePendingForTarget(targetPersonID uint, targetCategory string, items []model.PersonMergeSuggestionItem) error {
	r.calls++
	if r.failAt > 0 && r.calls >= r.failAt {
		return errors.New("acceptance injected mid-batch replace failure")
	}
	return r.PersonMergeSuggestionRepository.ReplacePendingForTarget(targetPersonID, targetCategory, items)
}

// TestAcceptance_MidBatchReplaceKeepsFailureAndCursor — R1/B1：真实 Replace 事务批内中途失败后，
// 已提交目标的 pending 可变，但失败表不得 Resolve、cursor 不得推进，pendingFailureResolves 保留供重放。
func TestAcceptance_MidBatchReplaceKeepsFailureAndCursor(t *testing.T) {
	peopleCfg := config.PeopleConfig{
		MergeSuggestionThreshold:       0.55,
		IdentityProfileRescueThreshold: 0.65,
		IdentityProfileMargin:          0.05,
		IdentityProfileMinCenterFaces:  3,
		MergeSuggestionBatchSize:       10,
		IdentityProfileMode:            model.PeopleIdentityModePrimary,
	}
	svcIface, db, _, _ := newPersonMergeSuggestionServiceWithConfigForTest(t, peopleCfg)
	svc := svcIface.(*personMergeSuggestionService)
	require.NoError(t, db.AutoMigrate(
		&model.PersonIdentityProfile{},
		&model.PersonIdentityCenter{},
		&model.PersonIdentityCenterMember{},
		&model.PersonMergeSuggestion{},
		&model.PersonMergeSuggestionItem{},
		&model.PersonMergeSuggestionFailure{},
	))

	makeTarget := func(emb []float32) (*model.Person, []*model.PersonIdentityCenter) {
		p := createMatcherPerson(t, db)
		require.NoError(t, db.Model(p).Updates(map[string]interface{}{
			"category": model.PersonCategoryFamily, "face_count": 2, "hidden": false,
		}).Error)
		centers := seedActiveProfile(t, db, p.ID, "emb-v1", []centerSpec{
			{emb: emb, supportCount: 8, p10: 0.4},
		})
		return p, centers
	}
	t1, c1 := makeTarget([]float32{1, 0, 0})
	t2, c2 := makeTarget([]float32{0, 1, 0})
	// 排序保证 Replace 顺序稳定（slice 按 person_id ASC）。
	if t2.ID < t1.ID {
		t1, t2 = t2, t1
		c1, c2 = c2, c1
	}

	seedPending := func(targetID uint) {
		sug := &model.PersonMergeSuggestion{
			TargetPersonID:         targetID,
			TargetCategorySnapshot: model.PersonCategoryFamily,
			Status:                 model.PersonMergeSuggestionStatusPending,
			CandidateCount:         1,
			TopSimilarity:          0.9,
		}
		require.NoError(t, db.Create(sug).Error)
		require.NoError(t, db.Create(&model.PersonMergeSuggestionItem{
			SuggestionID:      sug.ID,
			CandidatePersonID: targetID + 100000,
			SimilarityScore:   0.9,
			Rank:              1,
			Status:            model.PersonMergeSuggestionItemStatusPending,
			MatchSource:       model.PersonMergeMatchSourceIdentityProfile,
		}).Error)
	}
	seedPending(t1.ID)
	seedPending(t2.ID)

	failRepo := repository.NewPersonMergeSuggestionFailureRepository(db)
	now := time.Now().UTC()
	for _, tid := range []uint{t1.ID, t2.ID} {
		_, err := failRepo.UpsertTechnicalFailure(repository.PersonMergeFailureUpsert{
			TargetPersonID: tid,
			ReasonCode:     "profile_unavailable",
			Now:            now,
		})
		require.NoError(t, err)
	}

	// 索引就绪但正交中心互不达阈值 → 完整零候选，触发对旧 pending 的空 Replace。
	ann := buildMatcherANN(t, "emb-v1", append(append([]*model.PersonIdentityCenter{}, c1...), c2...))
	engine := NewIdentityMatchingEngine(
		ann,
		repository.NewPersonIdentityProfileRepository(db),
		repository.NewFaceRepository(db),
		repository.NewCannotLinkRepository(db),
		repository.NewFaceRepository(db),
		IdentityProfileMatcherConfig{EmbeddingModel: "emb-v1", RescueThreshold: 0.65, Margin: 0.05, MinCenterFaces: 3},
	)
	svc.SetIdentityMatchingEngine(engine)
	svc.SetIdentityProfileMode(model.PeopleIdentityModePrimary)
	svc.SetIdentitySuggestStrategy(NewIdentitySuggestStrategy(peopleCfg))

	wrapped := &acceptanceFailNthReplaceRepo{
		PersonMergeSuggestionRepository: svc.mergeSuggestionRepo,
		failAt:                          2,
	}
	svc.mergeSuggestionRepo = wrapped

	svc.mu.Lock()
	svc.state.Dirty = true
	svc.state.DirtyGeneration = 1
	svc.state.CursorTargetID = 0
	svc.state.RetryOnly = false
	svc.mu.Unlock()

	err := svc.RunBackgroundSlice()
	require.Error(t, err, "mid-batch replace failure must surface; replace_calls=%d", wrapped.calls)
	require.GreaterOrEqual(t, wrapped.calls, 2)

	svc.mu.Lock()
	require.Equal(t, uint(0), svc.state.CursorTargetID, "cursor must not advance after mid-batch write failure")
	require.Contains(t, svc.pendingFailureResolves, t1.ID)
	require.Contains(t, svc.pendingFailureResolves, t2.ID)
	svc.mu.Unlock()

	// 第一个目标的 Replace 事务已提交：旧 pending 应变 obsolete。
	var t1Pending int64
	require.NoError(t, db.Model(&model.PersonMergeSuggestion{}).
		Where("target_person_id = ? AND status = ?", t1.ID, model.PersonMergeSuggestionStatusPending).
		Count(&t1Pending).Error)
	require.Equal(t, int64(0), t1Pending, "first target replace committed before failure")

	// 第二个目标未完成 Replace：旧 pending 仍在。
	var t2Pending int64
	require.NoError(t, db.Model(&model.PersonMergeSuggestion{}).
		Where("target_person_id = ? AND status = ?", t2.ID, model.PersonMergeSuggestionStatusPending).
		Count(&t2Pending).Error)
	require.Equal(t, int64(1), t2Pending, "second target pending must remain when batch aborts")

	// 失败表不得被 Resolve。
	for _, tid := range []uint{t1.ID, t2.ID} {
		row, getErr := failRepo.GetByTarget(tid)
		require.NoError(t, getErr)
		require.NotNil(t, row)
		require.NotEqual(t, model.PersonMergeFailureStateResolved, row.State,
			"failure ledger must stay unresolved when suggestion write aborts mid-batch")
	}
}

// TestAcceptance_ReconcileSkipsIneligibleFailedCandidate — R2/B2：failed 候选残留更高 active_generation
// 不得在启动 reconcile 时被当成可查询证据去唤醒 exhausted。
func TestAcceptance_ReconcileSkipsIneligibleFailedCandidate(t *testing.T) {
	peopleCfg := config.PeopleConfig{
		MergeSuggestionThreshold:       0.55,
		IdentityProfileRescueThreshold: 0.65,
		IdentityProfileMargin:          0.05,
		IdentityProfileMinCenterFaces:  3,
		MergeSuggestionBatchSize:       10,
		IdentityProfileMode:            model.PeopleIdentityModePrimary,
	}
	svcIface, db, _, _ := newPersonMergeSuggestionServiceWithConfigForTest(t, peopleCfg)
	svc := svcIface.(*personMergeSuggestionService)
	require.NoError(t, db.AutoMigrate(
		&model.PersonIdentityProfile{},
		&model.PersonIdentityCenter{},
		&model.PersonIdentityCenterMember{},
		&model.PersonMergeSuggestionFailure{},
	))

	target := createMatcherPerson(t, db)
	cand := createMatcherPerson(t, db)
	require.NoError(t, db.Model(target).Updates(map[string]interface{}{
		"category": model.PersonCategoryFamily, "face_count": 2, "hidden": false,
	}).Error)
	require.NoError(t, db.Model(cand).Updates(map[string]interface{}{
		"category": model.PersonCategoryFamily, "face_count": 2, "hidden": false,
	}).Error)

	now := time.Now().UTC()
	require.NoError(t, db.Create(&model.PersonIdentityProfile{
		PersonID:         cand.ID,
		Status:           model.PersonIdentityProfileStatusFailed,
		ActiveGeneration: 5,
		NextGeneration:   6,
		EmbeddingModel:   "emb-v1",
		BuiltAt:          &now,
	}).Error)
	require.NoError(t, db.Create(&model.PersonIdentityCenter{
		PersonID:          cand.ID,
		Generation:        5,
		Ordinal:           0,
		SupportCount:      8,
		CentroidEmbedding: model.EncodeEmbedding([]float32{1, 0, 0}),
	}).Error)

	failRepo := repository.NewPersonMergeSuggestionFailureRepository(db)
	for i := 0; i < repository.MergeSuggestionFailureMaxAttempts; i++ {
		_, err := failRepo.UpsertTechnicalFailure(repository.PersonMergeFailureUpsert{
			TargetPersonID:   target.ID,
			ReasonCode:       "profile_unavailable",
			TargetProfileGen: 1,
			BlockingDeps: []model.PersonMergeFailureBlockingDep{
				{PersonID: cand.ID, Generation: 4},
			},
			Now: now.Add(time.Duration(i) * time.Hour),
		})
		require.NoError(t, err)
	}
	before, err := failRepo.GetByTarget(target.ID)
	require.NoError(t, err)
	require.Equal(t, model.PersonMergeFailureStateExhausted, before.State)

	engine := NewIdentityMatchingEngine(
		newIdentityProfileANN("emb-v1"),
		repository.NewPersonIdentityProfileRepository(db),
		repository.NewFaceRepository(db),
		repository.NewCannotLinkRepository(db),
		repository.NewFaceRepository(db),
		IdentityProfileMatcherConfig{EmbeddingModel: "emb-v1", RescueThreshold: 0.65, Margin: 0.05, MinCenterFaces: 3},
	)
	svc.SetIdentityMatchingEngine(engine)

	svc.ReconcileUnresolvedDependencies(false)

	after, err := failRepo.GetByTarget(target.ID)
	require.NoError(t, err)
	require.Equal(t, model.PersonMergeFailureStateExhausted, after.State,
		"failed candidate with stale higher active_generation must not reopen exhausted budget")
	require.Equal(t, before.Attempts, after.Attempts)
	require.Nil(t, after.NextRetryAt)
}

// TestAcceptance_EmptyReadyGenStampsAndWakesWaitingEvidence — 空画像 ready 无中心时仍须戳记
// TargetProfileGen，且代次前进能唤醒 waiting_evidence（不得因 eligible 中心过滤漏唤醒）。
func TestAcceptance_EmptyReadyGenStampsAndWakesWaitingEvidence(t *testing.T) {
	peopleCfg := config.PeopleConfig{
		MergeSuggestionThreshold:       0.55,
		IdentityProfileRescueThreshold: 0.65,
		IdentityProfileMargin:          0.05,
		IdentityProfileMinCenterFaces:  3,
		MergeSuggestionBatchSize:       10,
		IdentityProfileMode:            model.PeopleIdentityModePrimary,
	}
	svcIface, db, _, _ := newPersonMergeSuggestionServiceWithConfigForTest(t, peopleCfg)
	svc := svcIface.(*personMergeSuggestionService)
	require.NoError(t, db.AutoMigrate(
		&model.PersonIdentityProfile{},
		&model.PersonIdentityCenter{},
		&model.PersonIdentityCenterMember{},
		&model.PersonMergeSuggestionFailure{},
	))

	target := createMatcherPerson(t, db)
	require.NoError(t, db.Model(target).Updates(map[string]interface{}{
		"category": model.PersonCategoryFamily, "face_count": 2, "hidden": false,
	}).Error)
	seedReadyEmptyProfile(t, db, target.ID, "emb-v1", model.PersonIdentityBuildOutcomeNoUsableEvidence)
	require.NoError(t, db.Model(&model.PersonIdentityProfile{}).Where("person_id = ?", target.ID).
		Update("active_generation", 2).Error)

	engine := NewIdentityMatchingEngine(
		newIdentityProfileANN("emb-v1"),
		repository.NewPersonIdentityProfileRepository(db),
		repository.NewFaceRepository(db),
		repository.NewCannotLinkRepository(db),
		repository.NewFaceRepository(db),
		IdentityProfileMatcherConfig{EmbeddingModel: "emb-v1", RescueThreshold: 0.65, Margin: 0.05, MinCenterFaces: 3},
	)
	svc.SetIdentityMatchingEngine(engine)

	require.Equal(t, 2, mustProfileGen(t, svc, target.ID),
		"ready empty profile must stamp generation without requiring centers")

	failRepo := repository.NewPersonMergeSuggestionFailureRepository(db)
	now := time.Now().UTC()
	_, err := failRepo.UpsertWaitingEvidence(repository.PersonMergeFailureUpsert{
		TargetPersonID:   target.ID,
		ReasonCode:       "no_usable_evidence",
		TargetProfileGen: 0, // 模拟旧缺陷戳记
		Now:              now,
	})
	require.NoError(t, err)

	// 代次仍为 2：obs(2)>recorded(0) 应唤醒
	svc.NotifyProfileEvidenceReady([]uint{target.ID})
	afterWake, err := failRepo.GetByTarget(target.ID)
	require.NoError(t, err)
	require.Equal(t, model.PersonMergeFailureStateRetryWait, afterWake.State)
	require.Equal(t, 2, afterWake.TargetProfileGen)
	require.NotNil(t, afterWake.NextRetryAt)

	// 同代次重复通知不得再推迟
	prevNext := *afterWake.NextRetryAt
	time.Sleep(2 * time.Millisecond)
	svc.NotifyProfileEvidenceReady([]uint{target.ID})
	again, err := failRepo.GetByTarget(target.ID)
	require.NoError(t, err)
	require.Equal(t, prevNext.UnixNano(), again.NextRetryAt.UnixNano())
}

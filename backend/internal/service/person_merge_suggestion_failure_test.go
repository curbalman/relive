package service

import (
	"testing"
	"time"

	"github.com/davidhoo/relive/internal/model"
	"github.com/davidhoo/relive/internal/repository"
	"github.com/davidhoo/relive/pkg/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMergeSuggestionFailure_PendingZeroSurvivesRestartAndDailyScan(t *testing.T) {
	// §9.5 服务层：pending=0 + 五次技术失败 → exhausted；重启与每日巡检后 GetTask 仍显示未解决。
	peopleCfg := config.PeopleConfig{
		MergeSuggestionThreshold:       0.55,
		MergeSuggestionBatchSize:       10,
		MergeSuggestionStaleSeconds:    86400,
		IdentityProfileMode:            model.PeopleIdentityModePrimary,
		IdentityProfileRescueThreshold: 0.65,
		IdentityProfileMargin:          0.05,
		IdentityProfileMinCenterFaces:  3,
	}
	svcIface, db, repos, _ := newPersonMergeSuggestionServiceWithConfigForTest(t, peopleCfg)
	svc := svcIface.(*personMergeSuggestionService)

	target := &model.Person{Category: model.PersonCategoryFamily, FaceCount: 2}
	require.NoError(t, db.Create(target).Error)

	now := time.Date(2026, 9, 11, 8, 0, 0, 0, time.UTC)
	for i := 0; i < repository.MergeSuggestionFailureMaxAttempts; i++ {
		_, err := svc.failureRepo.UpsertTechnicalFailure(repository.PersonMergeFailureUpsert{
			TargetPersonID: target.ID,
			ReasonCode:     blockProfileUnavailable,
			EngineVersion:  identityEngineVersion,
			Now:            now.Add(time.Duration(i) * time.Hour),
		})
		require.NoError(t, err)
	}

	// 确认无 pending。
	pending, total, err := repos.MergeSuggestion.ListPending(1, 10)
	require.NoError(t, err)
	require.Equal(t, int64(0), total)
	require.Empty(t, pending)

	task := svc.GetTask()
	require.NotNil(t, task)
	assert.True(t, task.Partial)
	assert.Equal(t, 1, task.UnresolvedTargetCount)
	assert.Equal(t, 1, task.RetryExhaustedCount)
	assert.Equal(t, 0, task.RetryTotalCount)

	// 模拟进程重启：新服务实例读同一库。
	svc2Iface := NewPersonMergeSuggestionService(
		db, repos.Photo, repos.Face, repos.Person, repos.PeopleJob, repos.CannotLink, repos.MergeSuggestion,
		NewConfigService(repos.Config), &config.Config{People: peopleCfg},
	)
	svc2 := svc2Iface.(*personMergeSuggestionService)
	task2 := svc2.GetTask()
	require.NotNil(t, task2)
	assert.True(t, task2.Partial)
	assert.Equal(t, 1, task2.UnresolvedTargetCount)
	assert.Equal(t, 1, task2.RetryExhaustedCount)

	// 模拟每日巡检：推进 LastRunAt 并跑空 slice，不得清掉 exhausted。
	svc2.mu.Lock()
	svc2.state.LastRunAt = time.Now().Add(-48 * time.Hour)
	svc2.state.Dirty = false
	svc2.state.RetryOnly = false
	_ = svc2.saveStateLocked()
	svc2.mu.Unlock()
	require.NoError(t, svc2.RunBackgroundSlice())

	got, err := svc2.failureRepo.GetByTarget(target.ID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, model.PersonMergeFailureStateExhausted, got.State)

	task3 := svc2.GetTask()
	require.NotNil(t, task3)
	assert.True(t, task3.Partial)
	assert.Equal(t, 1, task3.UnresolvedTargetCount)
	assert.Equal(t, 1, task3.RetryExhaustedCount)
}

func TestMergeSuggestionFailure_NoUsableEvidencePersistsWithoutRetryBudget(t *testing.T) {
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

	_, err := svc.primaryAssignments([]*model.Person{target}, map[uint]map[uint]bool{})
	require.NoError(t, err)

	got, err := svc.failureRepo.GetByTarget(target.ID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, model.PersonMergeFailureStateWaitingEvidence, got.State)
	assert.Equal(t, 0, got.Attempts)

	task := svc.GetTask()
	require.NotNil(t, task)
	assert.True(t, task.Partial)
	assert.Equal(t, 1, task.WaitingEvidenceCount)
	assert.Equal(t, 0, task.RetryTotalCount)
}

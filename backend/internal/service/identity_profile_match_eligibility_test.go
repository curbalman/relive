package service

import (
	"testing"

	"github.com/davidhoo/relive/internal/model"
	"github.com/davidhoo/relive/internal/repository"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMatchEligibility_FailedCandidateInStaleANNIsIncomplete(t *testing.T) {
	db := setupMatcherDB(t)
	defer closeMatcherDB(t, db)

	target := createMatcherPerson(t, db)
	failed := createMatcherPerson(t, db)
	targetCenters := seedActiveProfile(t, db, target.ID, "emb-v1", []centerSpec{
		{emb: []float32{1, 0, 0}, supportCount: 5, p10: 0.5},
	})
	failedCenters := seedActiveProfile(t, db, failed.ID, "emb-v1", []centerSpec{
		{emb: []float32{0.99, 0.01, 0}, supportCount: 5, p10: 0.5},
	})
	require.NoError(t, db.Model(&model.PersonIdentityProfile{}).Where("person_id = ?", failed.ID).
		Updates(map[string]interface{}{
			"status":     model.PersonIdentityProfileStatusFailed,
			"last_error": "replace generation: database is locked",
		}).Error)

	// 模拟旧索引仍装入 failed 中心：召回能命中，精排读不到。
	ann := buildMatcherANN(t, "emb-v1", append(targetCenters, failedCenters...))
	engine := NewIdentityMatchingEngine(
		ann,
		repository.NewPersonIdentityProfileRepository(db),
		repository.NewFaceRepository(db),
		repository.NewCannotLinkRepository(db),
		repository.NewFaceRepository(db),
		matcherCfg(),
	)

	got := engine.SimilarPeople([]uint{target.ID}, DefaultIdentityRecallOptions())
	res := got[target.ID]
	require.NotEqual(t, IdentityMatchStatusNoCandidate, res.Status,
		"stale failed-candidate recall must not look like a complete empty success")
	assert.Equal(t, IdentityMatchStatusUnavailable, res.Status)
	assert.Equal(t, blockProfileUnavailable, res.BlockReason)
}

func TestMatchEligibility_RebuildFromEligibleCentersDropsFailedCandidate(t *testing.T) {
	db := setupMatcherDB(t)
	defer closeMatcherDB(t, db)

	target := createMatcherPerson(t, db)
	failed := createMatcherPerson(t, db)
	seedActiveProfile(t, db, target.ID, "emb-v1", []centerSpec{
		{emb: []float32{1, 0, 0}, supportCount: 5, p10: 0.5},
	})
	seedActiveProfile(t, db, failed.ID, "emb-v1", []centerSpec{
		{emb: []float32{0.99, 0.01, 0}, supportCount: 5, p10: 0.5},
	})
	require.NoError(t, db.Model(&model.PersonIdentityProfile{}).Where("person_id = ?", failed.ID).
		Update("status", model.PersonIdentityProfileStatusFailed).Error)

	repo := repository.NewPersonIdentityProfileRepository(db)
	centers, err := repo.ListAllActiveCenters("emb-v1")
	require.NoError(t, err)
	for _, c := range centers {
		assert.NotEqual(t, failed.ID, c.PersonID)
	}

	ann := buildMatcherANN(t, "emb-v1", centers)
	engine := NewIdentityMatchingEngine(
		ann,
		repo,
		repository.NewFaceRepository(db),
		repository.NewCannotLinkRepository(db),
		repository.NewFaceRepository(db),
		matcherCfg(),
	)
	got := engine.SimilarPeople([]uint{target.ID}, DefaultIdentityRecallOptions())
	res := got[target.ID]
	assert.Equal(t, IdentityMatchStatusNoCandidate, res.Status)
	assert.False(t, res.IncompleteEvidence)
	assert.Empty(t, res.BlockReason)
}

func TestMatchEligibility_PartialFailedCandidateKeepsIncompleteFlag(t *testing.T) {
	db := setupMatcherDB(t)
	defer closeMatcherDB(t, db)

	target := createMatcherPerson(t, db)
	good := createMatcherPerson(t, db)
	failed := createMatcherPerson(t, db)
	targetCenters := seedActiveProfile(t, db, target.ID, "emb-v1", []centerSpec{
		{emb: []float32{1, 0, 0}, supportCount: 5, p10: 0.5},
	})
	goodCenters := seedActiveProfile(t, db, good.ID, "emb-v1", []centerSpec{
		{emb: []float32{0.2, 0.98, 0}, supportCount: 5, p10: 0.5},
	})
	failedCenters := seedActiveProfile(t, db, failed.ID, "emb-v1", []centerSpec{
		{emb: []float32{0.99, 0.01, 0}, supportCount: 5, p10: 0.5},
	})
	require.NoError(t, db.Model(&model.PersonIdentityProfile{}).Where("person_id = ?", failed.ID).
		Update("status", model.PersonIdentityProfileStatusFailed).Error)

	ann := buildMatcherANN(t, "emb-v1", append(append(targetCenters, goodCenters...), failedCenters...))
	engine := NewIdentityMatchingEngine(
		ann,
		repository.NewPersonIdentityProfileRepository(db),
		repository.NewFaceRepository(db),
		repository.NewCannotLinkRepository(db),
		repository.NewFaceRepository(db),
		matcherCfg(),
	)
	got := engine.SimilarPeople([]uint{target.ID}, DefaultIdentityRecallOptions())
	res := got[target.ID]
	require.NotEqual(t, IdentityMatchStatusNoCandidate, res.Status)
	if res.Status != IdentityMatchStatusUnavailable {
		assert.True(t, res.IncompleteEvidence)
	}
}

package repository

import (
	"testing"
	"time"

	"github.com/davidhoo/relive/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPersonMergeSuggestionFailure_PendingZeroLifecycle(t *testing.T) {
	// §9.5：pending=0 的失败目标经历五次尝试、耗尽、重启、每日巡检后仍显示未解决。
	db := setupTestDB(t)
	defer teardownTestDB(db)
	repo := NewPersonMergeSuggestionFailureRepository(db)

	const targetID uint = 264975
	now := time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC)

	for i := 0; i < MergeSuggestionFailureMaxAttempts; i++ {
		row, err := repo.UpsertTechnicalFailure(PersonMergeFailureUpsert{
			TargetPersonID: targetID,
			ReasonCode:     "profile_unavailable",
			EngineVersion:  "identity-engine-v5",
			Now:            now.Add(time.Duration(i) * time.Hour),
		})
		require.NoError(t, err)
		require.NotNil(t, row)
	}

	got, err := repo.GetByTarget(targetID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, model.PersonMergeFailureStateExhausted, got.State)
	assert.Equal(t, MergeSuggestionFailureMaxAttempts, got.Attempts)
	assert.Equal(t, "profile_unavailable", got.ReasonCode)

	// 模拟重启：新仓库实例读同一库。
	repo2 := NewPersonMergeSuggestionFailureRepository(db)
	afterRestart, err := repo2.GetByTarget(targetID)
	require.NoError(t, err)
	require.NotNil(t, afterRestart)
	assert.Equal(t, model.PersonMergeFailureStateExhausted, afterRestart.State)

	// 模拟每日巡检时间推进：不得抹掉 exhausted，不重新赠送预算。
	later := now.Add(48 * time.Hour)
	counts, err := repo2.CountUnresolved(later)
	require.NoError(t, err)
	require.NotNil(t, counts)
	assert.Equal(t, int64(1), counts.UnresolvedTargets)
	assert.Equal(t, int64(1), counts.RetryExhausted)
	assert.Equal(t, int64(0), counts.RetryTotal)
	assert.Equal(t, int64(0), counts.RetryDue)

	// 再次 UpsertTechnicalFailure 不得刷新耗尽预算。
	again, err := repo2.UpsertTechnicalFailure(PersonMergeFailureUpsert{
		TargetPersonID: targetID,
		ReasonCode:     "profile_unavailable",
		Now:            later,
	})
	require.NoError(t, err)
	assert.Equal(t, model.PersonMergeFailureStateExhausted, again.State)
	assert.Equal(t, MergeSuggestionFailureMaxAttempts, again.Attempts)

	due, err := repo2.ListDueRetry(later, 10)
	require.NoError(t, err)
	assert.Empty(t, due, "exhausted must not enter due retry schedule")
}

func TestPersonMergeSuggestionFailure_WaitingEvidenceDoesNotBurnAttempts(t *testing.T) {
	db := setupTestDB(t)
	defer teardownTestDB(db)
	repo := NewPersonMergeSuggestionFailureRepository(db)

	now := time.Now().UTC()
	row, err := repo.UpsertWaitingEvidence(PersonMergeFailureUpsert{
		TargetPersonID: 306601,
		ReasonCode:     "no_usable_evidence",
		Now:            now,
	})
	require.NoError(t, err)
	assert.Equal(t, model.PersonMergeFailureStateWaitingEvidence, row.State)
	assert.Equal(t, 0, row.Attempts)

	row, err = repo.UpsertWaitingEvidence(PersonMergeFailureUpsert{
		TargetPersonID: 306601,
		ReasonCode:     "no_usable_evidence",
		Now:            now.Add(time.Minute),
	})
	require.NoError(t, err)
	assert.Equal(t, 0, row.Attempts)

	counts, err := repo.CountUnresolved(now)
	require.NoError(t, err)
	assert.Equal(t, int64(1), counts.WaitingEvidence)
	assert.Equal(t, int64(0), counts.RetryTotal)
	assert.Equal(t, int64(1), counts.UnresolvedTargets)
}

func TestPersonMergeSuggestionFailure_ResolveRemovesFromUnresolved(t *testing.T) {
	db := setupTestDB(t)
	defer teardownTestDB(db)
	repo := NewPersonMergeSuggestionFailureRepository(db)

	now := time.Now().UTC()
	_, err := repo.UpsertTechnicalFailure(PersonMergeFailureUpsert{
		TargetPersonID:     42,
		ReasonCode:         "index_unavailable",
		IndexReadyRequired: true,
		Now:                now,
	})
	require.NoError(t, err)
	require.NoError(t, repo.Resolve(42, model.PersonMergeFailureResolvedZeroCandidate))

	counts, err := repo.CountUnresolved(now)
	require.NoError(t, err)
	assert.Equal(t, int64(0), counts.UnresolvedTargets)

	got, err := repo.GetByTarget(42)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, model.PersonMergeFailureStateResolved, got.State)
}

func TestPersonMergeSuggestionFailure_ImportLegacyRetries(t *testing.T) {
	db := setupTestDB(t)
	defer teardownTestDB(db)
	repo := NewPersonMergeSuggestionFailureRepository(db)

	now := time.Now().UTC()
	n, err := repo.ImportFromLegacyRetries([]LegacyMergeRetryImport{
		{TargetID: 1, Attempts: 2, NextRetryAt: now.Add(-time.Minute), Reason: "profile_unavailable"},
		{TargetID: 2, Attempts: 5, NextRetryAt: now, Reason: "index_unavailable"},
		{TargetID: 1, Attempts: 9, Reason: "should_not_overwrite"},
	}, now)
	require.NoError(t, err)
	assert.Equal(t, 2, n)

	n, err = repo.ImportFromLegacyRetries([]LegacyMergeRetryImport{
		{TargetID: 1, Attempts: 9, Reason: "again"},
	}, now)
	require.NoError(t, err)
	assert.Equal(t, 0, n)

	a, err := repo.GetByTarget(1)
	require.NoError(t, err)
	assert.Equal(t, 2, a.Attempts)
	assert.Equal(t, model.PersonMergeFailureStateRetryWait, a.State)

	b, err := repo.GetByTarget(2)
	require.NoError(t, err)
	assert.Equal(t, model.PersonMergeFailureStateExhausted, b.State)
}

func TestPersonMergeSuggestionFailure_WakeForEvidenceChangeReopensExhausted(t *testing.T) {
	db := setupTestDB(t)
	defer teardownTestDB(db)
	repo := NewPersonMergeSuggestionFailureRepository(db)

	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	for i := 0; i < MergeSuggestionFailureMaxAttempts; i++ {
		_, err := repo.UpsertTechnicalFailure(PersonMergeFailureUpsert{
			TargetPersonID: 100,
			ReasonCode:     "profile_unavailable",
			BlockingDeps:   []model.PersonMergeFailureBlockingDep{{PersonID: 264975, Generation: 1}},
			Now:            now.Add(time.Duration(i) * time.Hour),
		})
		require.NoError(t, err)
	}
	got, err := repo.GetByTarget(100)
	require.NoError(t, err)
	require.Equal(t, model.PersonMergeFailureStateExhausted, got.State)

	woken, err := repo.WakeForEvidenceChange([]uint{264975}, map[uint]int{264975: 2}, now.Add(48*time.Hour))
	require.NoError(t, err)
	require.Equal(t, []uint{100}, woken)

	after, err := repo.GetByTarget(100)
	require.NoError(t, err)
	assert.Equal(t, model.PersonMergeFailureStateRetryWait, after.State)
	assert.Equal(t, 0, after.Attempts)
	require.NotNil(t, after.NextRetryAt)
	due, err := repo.ListDueRetry(now.Add(48*time.Hour), 10)
	require.NoError(t, err)
	require.Len(t, due, 1)

	// 同代次重复通知不得再次赠预算 / 改写状态。
	woken2, err := repo.WakeForEvidenceChange([]uint{264975}, map[uint]int{264975: 2}, now.Add(49*time.Hour))
	require.NoError(t, err)
	assert.Empty(t, woken2)
}

func TestPersonMergeSuggestionFailure_ManualRetrySkipsWaitingEvidenceAndAudits(t *testing.T) {
	db := setupTestDB(t)
	defer teardownTestDB(db)
	repo := NewPersonMergeSuggestionFailureRepository(db)
	now := time.Date(2026, 9, 14, 18, 0, 0, 0, time.UTC)

	_, err := repo.UpsertWaitingEvidence(PersonMergeFailureUpsert{
		TargetPersonID: 306601,
		ReasonCode:     "no_usable_evidence",
		Now:            now,
	})
	require.NoError(t, err)
	for i := 0; i < MergeSuggestionFailureMaxAttempts; i++ {
		_, err = repo.UpsertTechnicalFailure(PersonMergeFailureUpsert{
			TargetPersonID: 42,
			ReasonCode:     "profile_unavailable",
			Now:            now.Add(time.Duration(i) * time.Hour),
		})
		require.NoError(t, err)
	}

	accepted, skipped, err := repo.ManualRetry([]uint{306601, 42}, "ui_retry", now.Add(2*time.Hour))
	require.NoError(t, err)
	require.Equal(t, []uint{42}, accepted)
	require.Len(t, skipped, 1)
	assert.Equal(t, uint(306601), skipped[0].TargetPersonID)
	assert.Equal(t, "waiting_evidence", skipped[0].Reason)

	got, err := repo.GetByTarget(42)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, model.PersonMergeFailureStateRetryWait, got.State)
	assert.Equal(t, 0, got.Attempts)
	assert.Equal(t, "ui_retry", got.ManualRetryReason)
	require.NotNil(t, got.ManualRetriedAt)
}

func TestPersonMergeSuggestionFailure_LocalNowStillSchedulesDueInUTC(t *testing.T) {
	// 写入用本地墙钟、查询用 UTC 时，SQLite 无时区比较曾把已到期行判成未到期。
	db := setupTestDB(t)
	defer teardownTestDB(db)
	repo := NewPersonMergeSuggestionFailureRepository(db)

	loc := time.FixedZone("CST", 8*3600)
	localNow := time.Date(2026, 9, 14, 17, 19, 0, 0, loc)
	past := localNow.Add(-10 * time.Minute)
	_, err := repo.UpsertTechnicalFailure(PersonMergeFailureUpsert{
		TargetPersonID: 77,
		ReasonCode:     "profile_unavailable",
		Now:            past,
	})
	require.NoError(t, err)

	due, err := repo.ListDueRetry(localNow, 10)
	require.NoError(t, err)
	require.Len(t, due, 1)
	assert.Equal(t, uint(77), due[0].TargetPersonID)

	counts, err := repo.CountUnresolved(localNow)
	require.NoError(t, err)
	assert.Equal(t, int64(1), counts.RetryDue)
	assert.Equal(t, int64(0), counts.RetryDeferred)
}

// TestAcceptance_IndexRecoveryWakesExhausted — R2：索引真实恢复须唤醒 exhausted + index_ready_required。
func TestAcceptance_IndexRecoveryWakesExhausted(t *testing.T) {
	db := setupTestDB(t)
	defer teardownTestDB(db)
	repo := NewPersonMergeSuggestionFailureRepository(db)
	now := time.Now().UTC()
	for i := 0; i < MergeSuggestionFailureMaxAttempts; i++ {
		_, err := repo.UpsertTechnicalFailure(PersonMergeFailureUpsert{
			TargetPersonID:     99,
			ReasonCode:         "index_unavailable",
			IndexReadyRequired: true,
			Now:                now.Add(time.Duration(i) * time.Hour),
		})
		require.NoError(t, err)
	}
	ids, err := repo.WakeForIndexReady(now.Add(24 * time.Hour))
	require.NoError(t, err)
	require.Contains(t, ids, uint(99), "actual unavailable-to-ready transition must wake exhausted index dependency")
}

// TestAcceptance_ManualRetryIsIdempotent — R3：连续人工重试不得推迟已有 next_retry_at。
func TestAcceptance_ManualRetryIsIdempotent(t *testing.T) {
	db := setupTestDB(t)
	defer teardownTestDB(db)
	repo := NewPersonMergeSuggestionFailureRepository(db)
	now := time.Now().UTC()
	_, err := repo.UpsertTechnicalFailure(PersonMergeFailureUpsert{
		TargetPersonID: 99,
		ReasonCode:     "profile_unavailable",
		Now:            now,
	})
	require.NoError(t, err)
	_, _, err = repo.ManualRetry([]uint{99}, "manual_retry", now)
	require.NoError(t, err)
	first, err := repo.GetByTarget(99)
	require.NoError(t, err)
	accepted2, skipped2, err := repo.ManualRetry([]uint{99}, "manual_retry", now.Add(time.Minute))
	require.NoError(t, err)
	require.Empty(t, accepted2)
	require.Len(t, skipped2, 1)
	require.Equal(t, "already_scheduled", skipped2[0].Reason)
	second, err := repo.GetByTarget(99)
	require.NoError(t, err)
	require.Equal(t, first.NextRetryAt, second.NextRetryAt, "repeat click must not postpone existing work")
}

// TestReacceptance_ReconcileFirstRecoveryFromZeroGeneration — S2：记录代次 0 的首次恢复必须能被核对唤醒。
func TestReacceptance_ReconcileFirstRecoveryFromZeroGeneration(t *testing.T) {
	db := setupTestDB(t)
	defer teardownTestDB(db)
	repo := NewPersonMergeSuggestionFailureRepository(db)
	now := time.Now().UTC()
	for i := 0; i < MergeSuggestionFailureMaxAttempts; i++ {
		_, err := repo.UpsertTechnicalFailure(PersonMergeFailureUpsert{
			TargetPersonID:   1001,
			TargetProfileGen: 0,
			ReasonCode:       "profile_unavailable",
			Now:              now.Add(time.Duration(i) * time.Hour),
		})
		require.NoError(t, err)
	}
	// First successful build committed generation 1, process exited before callback.
	ids, err := repo.ReconcileDependencyVersions(map[uint]int{1001: 1}, false, now.Add(24*time.Hour))
	require.NoError(t, err)
	require.Contains(t, ids, uint(1001), "first real recovery from unavailable gen 0 must survive lost notification")
}

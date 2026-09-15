package service

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/davidhoo/relive/internal/model"
	"github.com/davidhoo/relive/internal/repository"
	"github.com/davidhoo/relive/pkg/config"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// 第三轮验收反例 T1–T4：故障→无虚报→恢复→只执行一次。

func mustProfileGen(t *testing.T, svc *personMergeSuggestionService, personID uint) int {
	t.Helper()
	gen, err := svc.profileActiveGeneration(personID)
	require.NoError(t, err)
	return gen
}

func loadProfileRevision(t *testing.T, db *gorm.DB, personID uint) (revision int, attempts int, exhausted bool, status string) {
	t.Helper()
	var p model.PersonIdentityProfile
	err := db.Where("person_id = ?", personID).First(&p).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return 0, 0, false, ""
	}
	require.NoError(t, err)
	return p.ChangeRevision, p.RecoveryAttempts, p.RecoveryExhausted, p.Status
}

// TestThirdReview_ManualCheckpointFailureCanResume — T1：初始安排保存失败，恢复后实际安排，不误判 already_scheduled。
func TestThirdReview_ManualCheckpointFailureCanResume(t *testing.T) {
	iface, db, _, _ := newPersonMergeSuggestionServiceWithConfigForTest(t, config.PeopleConfig{})
	svc := iface.(*personMergeSuggestionService)
	real := svc.failureRepo
	_, err := real.UpsertTechnicalFailure(repository.PersonMergeFailureUpsert{
		TargetPersonID: 8101, ReasonCode: blockProfileUnavailable, Now: time.Now().UTC(),
	})
	require.NoError(t, err)

	svc.failureRepo = &reacceptanceArrangeHookRepo{
		PersonMergeSuggestionFailureRepository: real,
		failBeginAfter:                         1,
	}
	var notifyCalls []uint
	svc.SetProfileInvalidateHook(func(ids []uint) { notifyCalls = append(notifyCalls, ids...) })

	_, err = svc.RetryFailures([]uint{8101}, "manual_retry")
	require.Error(t, err)
	require.Empty(t, notifyCalls)
	row, err := real.GetByTarget(8101)
	require.NoError(t, err)
	require.Nil(t, row.ManualRetriedAt)
	require.False(t, row.ManualArrangePending)
	rev, _, _, status := loadProfileRevision(t, db, 8101)
	require.Zero(t, rev)
	require.Empty(t, status)

	svc.failureRepo = real
	result, err := svc.RetryFailures([]uint{8101}, "manual_retry")
	require.NoError(t, err)
	require.Contains(t, result.AcceptedTargetIDs, uint(8101),
		"retry must complete arrangement after initial checkpoint failure; skipped=%v", result.Skipped)
	require.Equal(t, []uint{8101}, notifyCalls)
	done, err := real.GetByTarget(8101)
	require.NoError(t, err)
	require.NotNil(t, done.ManualRetriedAt)
	require.False(t, done.ManualArrangePending)
	rev, attempts, exhausted, status := loadProfileRevision(t, db, 8101)
	require.Equal(t, model.PersonIdentityProfileStatusDirty, status)
	require.Equal(t, 1, rev)
	require.Zero(t, attempts)
	require.False(t, exhausted)
}

// TestThirdReview_ManualProgressFailureDoesNotRepeatDirty — T2：逐项事务中途失败两者回滚，恢复后只提交一次。
func TestThirdReview_ManualProgressFailureDoesNotRepeatDirty(t *testing.T) {
	iface, db, _, _ := newPersonMergeSuggestionServiceWithConfigForTest(t, config.PeopleConfig{})
	svc := iface.(*personMergeSuggestionService)
	_, err := svc.failureRepo.UpsertTechnicalFailure(repository.PersonMergeFailureUpsert{
		TargetPersonID: 8201, ReasonCode: blockProfileUnavailable, Now: time.Now().UTC(),
	})
	require.NoError(t, err)

	var dirtyAttempts []uint
	svc.manualArrangeAfterDirtyHook = func(tx *gorm.DB, targetPersonID, personID uint) error {
		dirtyAttempts = append(dirtyAttempts, personID)
		return errors.New("injected mid-tx progress failure")
	}
	svc.SetProfileInvalidateHook(func(ids []uint) {})

	_, err = svc.RetryFailures([]uint{8201}, "manual_retry")
	require.Error(t, err)
	require.Equal(t, []uint{8201}, dirtyAttempts)
	row, err := svc.failureRepo.GetByTarget(8201)
	require.NoError(t, err)
	require.True(t, row.ManualArrangePending, "initial arrangement committed")
	require.Contains(t, row.ManualArrangeRemainingJSON, "8201")
	rev, _, _, status := loadProfileRevision(t, db, 8201)
	require.True(t, status == "" || status != model.PersonIdentityProfileStatusDirty || rev == 0,
		"failed item tx must roll back MarkDirty; status=%s rev=%d", status, rev)
	// 事务回滚后不应留下 dirty 画像。
	require.NotEqual(t, model.PersonIdentityProfileStatusDirty, status)

	dirtyAttempts = nil
	var notify []uint
	svc.manualArrangeAfterDirtyHook = nil
	svc.SetProfileInvalidateHook(func(ids []uint) { notify = append(notify, ids...) })
	_, err = svc.RetryFailures([]uint{8201}, "manual_retry")
	require.NoError(t, err)
	require.Equal(t, []uint{8201}, notify, "successful MarkDirty must happen exactly once after recovery")
	rev, attempts, exhausted, status := loadProfileRevision(t, db, 8201)
	require.Equal(t, model.PersonIdentityProfileStatusDirty, status)
	require.Equal(t, 1, rev)
	require.Zero(t, attempts)
	require.False(t, exhausted)
	done, err := svc.failureRepo.GetByTarget(8201)
	require.NoError(t, err)
	require.False(t, done.ManualArrangePending)
}

// TestThirdReview_ManualProgressConcurrentSingleCommit — T2：真正并发未完成请求，同一项只提交一次。
func TestThirdReview_ManualProgressConcurrentSingleCommit(t *testing.T) {
	iface, db, _, _ := newPersonMergeSuggestionServiceWithConfigForTest(t, config.PeopleConfig{})
	svc := iface.(*personMergeSuggestionService)
	_, err := svc.failureRepo.UpsertTechnicalFailure(repository.PersonMergeFailureUpsert{
		TargetPersonID: 8211, ReasonCode: blockProfileUnavailable, Now: time.Now().UTC(),
	})
	require.NoError(t, err)

	svc.SetProfileInvalidateHook(func(ids []uint) {})
	_, err = svc.RetryFailures([]uint{8211}, "manual_retry")
	require.NoError(t, err)
	require.NoError(t, svc.failureRepo.UpdateManualArrangement(8211, true, []uint{8211}))
	require.NoError(t, db.Where("person_id = ?", 8211).Delete(&model.PersonIdentityProfile{}).Error)

	var mu sync.Mutex
	notifyCount := 0
	start := make(chan struct{})
	entered := make(chan struct{}, 2)
	svc.manualArrangeBeforeTxHook = func(targetPersonID, personID uint) {
		entered <- struct{}{}
		<-start
	}
	svc.SetProfileInvalidateHook(func(ids []uint) {
		mu.Lock()
		notifyCount++
		mu.Unlock()
	})

	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, e := svc.RetryFailures([]uint{8211}, "manual_retry")
			errs <- e
		}()
	}
	<-entered
	<-entered
	close(start)
	wg.Wait()
	close(errs)
	for e := range errs {
		require.NoError(t, e)
	}
	mu.Lock()
	require.Equal(t, 1, notifyCount, "concurrent in-flight requests must commit profile dirty once")
	mu.Unlock()
	rev, attempts, exhausted, status := loadProfileRevision(t, db, 8211)
	require.Equal(t, model.PersonIdentityProfileStatusDirty, status)
	require.Equal(t, 1, rev, "change_revision must advance exactly once")
	require.Zero(t, attempts)
	require.False(t, exhausted)
	done, err := svc.failureRepo.GetByTarget(8211)
	require.NoError(t, err)
	require.False(t, done.ManualArrangePending)
}

type thirdReviewFailConfig struct{ ConfigService }

func (c thirdReviewFailConfig) Set(key, value string) error {
	return errors.New("injected state persistence failure")
}

// TestThirdReview_AttemptKeySaveFailureMustStopLedgerWrite — T3：尝试身份未保存则不得写失败表；恢复后成功一次。
func TestThirdReview_AttemptKeySaveFailureMustStopLedgerWrite(t *testing.T) {
	iface, db, repos, cfgSvc := newPersonMergeSuggestionServiceWithConfigForTest(t, config.PeopleConfig{})
	svc := iface.(*personMergeSuggestionService)
	svc.configService = thirdReviewFailConfig{svc.configService}

	err := svc.applyFailureIntents(failureIntentBatch{
		TechnicalIDs:     []uint{8301},
		TechnicalReasons: map[uint]string{8301: blockProfileUnavailable},
		Now:              time.Now().UTC(),
	})
	row, readErr := svc.failureRepo.GetByTarget(8301)
	require.NoError(t, readErr)
	require.Error(t, err, "must stop when durable attempt identity cannot be saved; row=%+v", row)
	require.Nil(t, row)
	require.Empty(t, svc.state.FailureAttemptKey, "memory must not keep unsaved attempt key")

	svc.configService = cfgSvc
	require.NoError(t, svc.applyFailureIntents(failureIntentBatch{
		TechnicalIDs:     []uint{8301},
		TechnicalReasons: map[uint]string{8301: blockProfileUnavailable},
		Now:              time.Now().UTC(),
	}))
	first, err := svc.failureRepo.GetByTarget(8301)
	require.NoError(t, err)
	require.Equal(t, 1, first.Attempts)
	require.NotEmpty(t, first.LastAttemptKey)
	require.Equal(t, first.LastAttemptKey, svc.state.FailureAttemptKey)

	// 失败行已提交后最终进度保存失败：保留同一尝试身份重放。
	svc.configService = thirdReviewFailConfig{cfgSvc}
	svc.mu.Lock()
	prevKey := svc.state.FailureAttemptKey
	prevSeq := svc.state.FailureAttemptSeq
	svc.rotateFailureAttemptKeyLocked()
	saveErr := svc.saveStateLocked()
	if saveErr != nil {
		svc.state.FailureAttemptKey = prevKey
		svc.state.FailureAttemptSeq = prevSeq
	}
	svc.mu.Unlock()
	require.Error(t, saveErr)
	require.Equal(t, prevKey, svc.state.FailureAttemptKey)

	// 重启重放不双计。
	svc2 := NewPersonMergeSuggestionService(
		db, repos.Photo, repos.Face, repos.Person, repos.PeopleJob, repos.CannotLink, repos.MergeSuggestion, cfgSvc,
		&config.Config{People: config.PeopleConfig{}},
	).(*personMergeSuggestionService)
	require.Equal(t, first.LastAttemptKey, svc2.state.FailureAttemptKey)
	require.NoError(t, svc2.applyFailureIntents(failureIntentBatch{
		TechnicalIDs:     []uint{8301},
		TechnicalReasons: map[uint]string{8301: blockProfileUnavailable},
		Now:              time.Now().UTC(),
	}))
	replayed, err := repos.MergeSuggestionFailure.GetByTarget(8301)
	require.NoError(t, err)
	require.Equal(t, 1, replayed.Attempts)

	// 内存继续：新一轮真实尝试递增至耗尽。
	for i := 2; i <= repository.MergeSuggestionFailureMaxAttempts; i++ {
		key := fmt.Sprintf("dg-0-seq-%d", i)
		require.NoError(t, svc2.applyFailureIntents(failureIntentBatch{
			TechnicalIDs:     []uint{8301},
			TechnicalReasons: map[uint]string{8301: blockProfileUnavailable},
			AttemptKey:       key,
			Now:              time.Now().UTC().Add(time.Duration(i) * time.Minute),
		}))
		got, e := repos.MergeSuggestionFailure.GetByTarget(8301)
		require.NoError(t, e)
		require.Equal(t, i, got.Attempts)
	}
	final, err := repos.MergeSuggestionFailure.GetByTarget(8301)
	require.NoError(t, err)
	require.Equal(t, model.PersonMergeFailureStateExhausted, final.State)
}

// TestThirdReview_ProfileReadFailureKeepsReconcilePending — T4：画像读取失败不越批；恢复后自动完成核对。
func TestThirdReview_ProfileReadFailureKeepsReconcilePending(t *testing.T) {
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
	for i := 0; i < 5; i++ {
		_, err := svc.failureRepo.UpsertTechnicalFailure(repository.PersonMergeFailureUpsert{
			TargetPersonID: p.ID, TargetProfileGen: 0, ReasonCode: blockProfileUnavailable, Now: time.Now().UTC(),
		})
		require.NoError(t, err)
	}
	before, err := svc.failureRepo.GetByTarget(p.ID)
	require.NoError(t, err)
	require.Equal(t, model.PersonMergeFailureStateExhausted, before.State)

	injected := 0
	require.NoError(t, db.Callback().Query().Before("gorm:query").Register("third-review-profile-error", func(tx *gorm.DB) {
		if tx.Statement.TableExpr != nil && strings.Contains(tx.Statement.TableExpr.SQL, "person_identity_profiles AS p") {
			injected++
			tx.AddError(errors.New("injected profile generation read failure"))
		}
	}))
	svc.AttachIdentityMatchingWiring(NewIdentityMatchingEngine(
		newIdentityProfileANN("emb-v1"),
		repository.NewPersonIdentityProfileRepository(db),
		repository.NewFaceRepository(db),
		repository.NewCannotLinkRepository(db),
		repository.NewFaceRepository(db),
		IdentityProfileMatcherConfig{EmbeddingModel: "emb-v1"},
	), false)
	require.Positive(t, injected, "fault must hit actual profile generation lookup")
	require.True(t, svc.state.ReconcilePending, "read error must not be treated as completed empty reconciliation")
	mid, err := svc.failureRepo.GetByTarget(p.ID)
	require.NoError(t, err)
	require.Equal(t, model.PersonMergeFailureStateExhausted, mid.State, "must not consume versions on read failure")

	require.NoError(t, db.Callback().Query().Before("gorm:query").Remove("third-review-profile-error"))
	svc.runUnresolvedDependencyReconcile(true)
	after, err := svc.failureRepo.GetByTarget(p.ID)
	require.NoError(t, err)
	require.Equal(t, model.PersonMergeFailureStateRetryWait, after.State,
		"after read recovers, background reconcile must wake the target")
	require.False(t, svc.state.ReconcilePending)
}

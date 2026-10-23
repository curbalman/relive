package service

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/davidhoo/relive/internal/model"
	"github.com/davidhoo/relive/internal/provider"
	"github.com/davidhoo/relive/internal/repository"
	"github.com/davidhoo/relive/pkg/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

type observedAIRuntime struct {
	AnalysisRuntimeService
	heartbeats    atomic.Int32
	releases      atomic.Int32
	transient     atomic.Bool
	heartbeatGate <-chan struct{}
}

func (r *observedAIRuntime) HeartbeatGlobal(ownerType, ownerID string) (*model.AnalysisRuntimeLease, error) {
	r.heartbeats.Add(1)
	if r.heartbeatGate != nil {
		<-r.heartbeatGate
	}
	if r.transient.Swap(false) {
		return nil, errors.New("database is locked")
	}
	return r.AnalysisRuntimeService.HeartbeatGlobal(ownerType, ownerID)
}
func (r *observedAIRuntime) ReleaseGlobal(ownerType, ownerID string) error {
	r.releases.Add(1)
	return r.AnalysisRuntimeService.ReleaseGlobal(ownerType, ownerID)
}

type leaseTestAIProvider struct {
	provider.AIProvider
	batch, batchErr                        bool
	gatePhase                              string
	started, gate                          chan struct{}
	startOnce                              sync.Once
	analyzeCalls, batchCalls, captionCalls atomic.Int32
}

func (p *leaseTestAIProvider) wait(phase string) {
	if p.gatePhase == phase {
		p.startOnce.Do(func() { close(p.started) })
		<-p.gate
	}
}
func (p *leaseTestAIProvider) Analyze(*provider.AnalyzeRequest) (*provider.AnalyzeResult, error) {
	p.analyzeCalls.Add(1)
	p.wait("analyze")
	return &provider.AnalyzeResult{Description: "lease test", MainCategory: "人物"}, nil
}
func (p *leaseTestAIProvider) AnalyzeBatch(reqs []*provider.AnalyzeRequest) ([]*provider.AnalyzeResult, error) {
	p.batchCalls.Add(1)
	p.wait("batch")
	if p.batchErr {
		return nil, errors.New("batch provider failure")
	}
	results := make([]*provider.AnalyzeResult, len(reqs))
	for i := range results {
		results[i] = &provider.AnalyzeResult{Description: "lease test", MainCategory: "人物"}
	}
	return results, nil
}
func (p *leaseTestAIProvider) GenerateCaption(*provider.AnalyzeRequest) (string, error) {
	p.captionCalls.Add(1)
	p.wait("caption")
	return "lease caption", nil
}
func (p *leaseTestAIProvider) Name() string        { return "lease-test" }
func (p *leaseTestAIProvider) Cost() float64       { return 0 }
func (p *leaseTestAIProvider) BatchCost() float64  { return 0 }
func (p *leaseTestAIProvider) MaxConcurrency() int { return 4 }
func (p *leaseTestAIProvider) SupportsBatch() bool { return p.batch }
func (p *leaseTestAIProvider) MaxBatchSize() int   { return 2 }

func setupAILeaseService(t *testing.T, p provider.AIProvider, photoCount int) (*aiService, *observedAIRuntime) {
	t.Helper()
	root := t.TempDir()
	db, err := gorm.Open(sqlite.Open(filepath.Join(root, "lease.db")+"?_busy_timeout=5000"), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { assert.NoError(t, sqlDB.Close()) })
	require.NoError(t, db.AutoMigrate(&model.Photo{}, &model.PhotoTag{}, &model.AnalysisRuntimeLease{}))
	repo := repository.NewPhotoRepository(db)
	for i := 0; i < photoCount; i++ {
		name := fmt.Sprintf("photo-%d.jpg", i)
		path := createTestImageFile(t, root, name)
		require.NoError(t, repo.Create(&model.Photo{FilePath: path, FileName: name, FileHash: name, ThumbnailPath: name, ThumbnailStatus: model.ThumbnailStatusReady}))
	}
	runtime := &observedAIRuntime{AnalysisRuntimeService: NewAnalysisRuntimeService(db)}
	svc := &aiService{photoRepo: repo, provider: p, config: &config.Config{Photos: config.PhotosConfig{ThumbnailPath: root}}, runtimeService: runtime, runtimeHeartbeatInterval: 5 * time.Millisecond}
	t.Cleanup(func() {
		if task := svc.GetTaskStatus(); task != nil && task.IsRunning() {
			if task.Mode == model.AnalysisOwnerTypeBackground {
				_ = svc.StopBackgroundAnalyze()
			}
			assert.Eventually(t, func() bool { return !svc.GetTaskStatus().IsRunning() }, 2*time.Second, time.Millisecond)
		}
	})
	return svc, runtime
}
func waitAILeaseTask(t *testing.T, svc *aiService, status string) *AnalyzeTask {
	t.Helper()
	require.Eventually(t, func() bool { task := svc.GetTaskStatus(); return task != nil && task.Status == status }, 2*time.Second, time.Millisecond)
	return svc.GetTaskStatus()
}

func TestAIService_RuntimeLeaseLossStopsProviderWork(t *testing.T) {
	for _, mode := range []string{model.AnalysisOwnerTypeBatch, model.AnalysisOwnerTypeBackground} {
		for _, tc := range []struct {
			name, phase string
			batch, fail bool
		}{
			{name: "single-analysis", phase: "analyze"}, {name: "single-caption", phase: "caption"},
			{name: "batch-analysis", phase: "batch", batch: true}, {name: "batch-error", phase: "batch", batch: true, fail: true},
			{name: "batch-caption", phase: "caption", batch: true}, {name: "fallback-analysis", phase: "analyze", batch: true, fail: true},
		} {
			t.Run(mode+"/"+tc.name, func(t *testing.T) {
				p := &leaseTestAIProvider{batch: tc.batch, batchErr: tc.fail, gatePhase: tc.phase, started: make(chan struct{}), gate: make(chan struct{})}
				svc, runtime := setupAILeaseService(t, p, 6)
				var unblock sync.Once
				t.Cleanup(func() { unblock.Do(func() { close(p.gate) }) })
				var task *AnalyzeTask
				var err error
				if mode == model.AnalysisOwnerTypeBackground {
					task, err = svc.StartBackgroundAnalyze()
				} else {
					task, err = svc.AnalyzeBatch(6)
				}
				require.NoError(t, err)
				select {
				case <-p.started:
				case <-time.After(2 * time.Second):
					t.Fatal("provider did not start")
				}
				analyzes, batches, captions := p.analyzeCalls.Load(), p.batchCalls.Load(), p.captionCalls.Load()
				// Replace the persisted owner while an actual worker/provider call is active.
				require.NoError(t, runtime.AnalysisRuntimeService.ReleaseGlobal(mode, task.ID))
				_, err = runtime.AcquireGlobal(model.AnalysisOwnerTypeAnalyzer, "replacement", "new owner")
				require.NoError(t, err)
				stopping := waitAILeaseTask(t, svc, AnalyzeTaskStatusStopping)
				assert.Contains(t, stopping.ErrorMessage, ErrAnalysisRuntimeOwnedByOther.Error())
				_, err = svc.AnalyzeBatch(6)
				require.ErrorContains(t, err, "already running")
				unblock.Do(func() { close(p.gate) })
				finished := waitAILeaseTask(t, svc, AnalyzeTaskStatusFailed)
				assert.Equal(t, stopping.ErrorMessage, finished.CurrentMessage)
				assert.NotNil(t, finished.CompletedAt)
				assert.Zero(t, finished.SuccessCount)
				assert.Zero(t, finished.FailedCount, "lease cancellation is not a photo failure")
				assert.Equal(t, analyzes, p.analyzeCalls.Load(), "queued photos must not start")
				assert.Equal(t, batches, p.batchCalls.Load(), "later batches must not start")
				assert.Equal(t, captions, p.captionCalls.Load(), "no new captions after ownership loss")
				count, err := svc.photoRepo.CountAnalyzed()
				require.NoError(t, err)
				assert.Zero(t, count, "in-flight results must not be published after lease loss")
				status, err := runtime.GetGlobalStatus()
				require.NoError(t, err)
				assert.Equal(t, "replacement", status.OwnerID)
				assert.Zero(t, runtime.releases.Load(), "old worker must not release a lost lease")
				heartbeats := runtime.heartbeats.Load()
				time.Sleep(20 * time.Millisecond)
				assert.Equal(t, heartbeats, runtime.heartbeats.Load(), "heartbeat must terminate")
			})
		}
	}
}
func TestAIService_RuntimeLeaseLossWakesSleepingBackground(t *testing.T) {
	p := &leaseTestAIProvider{}
	svc, runtime := setupAILeaseService(t, p, 0)
	task, err := svc.StartBackgroundAnalyze()
	require.NoError(t, err)
	waitAILeaseTask(t, svc, AnalyzeTaskStatusSleeping)
	require.NoError(t, runtime.AnalysisRuntimeService.ReleaseGlobal(task.Mode, task.ID))
	finished := waitAILeaseTask(t, svc, AnalyzeTaskStatusFailed)
	assert.NotNil(t, finished.CompletedAt)
	assert.Contains(t, finished.ErrorMessage, ErrAnalysisRuntimeOwnedByOther.Error())
	assert.Zero(t, p.analyzeCalls.Load())
}
func TestAIService_RuntimeLeaseTransientErrorRecovers(t *testing.T) {
	p := &leaseTestAIProvider{gatePhase: "analyze", started: make(chan struct{}), gate: make(chan struct{})}
	svc, runtime := setupAILeaseService(t, p, 1)
	var unblock sync.Once
	t.Cleanup(func() { unblock.Do(func() { close(p.gate) }) })
	runtime.transient.Store(true)
	_, err := svc.AnalyzeBatch(1)
	require.NoError(t, err)
	require.Eventually(t, func() bool { return runtime.heartbeats.Load() >= 3 }, time.Second, time.Millisecond)
	assert.Equal(t, AnalyzeTaskStatusRunning, svc.GetTaskStatus().Status)
	assert.Empty(t, svc.GetTaskStatus().ErrorMessage)
	unblock.Do(func() { close(p.gate) })
	finished := waitAILeaseTask(t, svc, AnalyzeTaskStatusCompleted)
	assert.Equal(t, 1, finished.SuccessCount)
	assert.Equal(t, int32(1), runtime.releases.Load())
	status, err := runtime.GetGlobalStatus()
	require.NoError(t, err)
	assert.False(t, status.IsActive)
}
func TestAIService_TaskSnapshotDoesNotExposeMutableState(t *testing.T) {
	photoID := uint(3)
	now := time.Now()
	svc := &aiService{currentTask: &AnalyzeTask{ID: "snapshot", Status: AnalyzeTaskStatusRunning, CurrentPhotoID: &photoID, CompletedAt: &now}}
	snapshot := svc.GetTaskStatus()
	snapshot.Status = AnalyzeTaskStatusFailed
	*snapshot.CurrentPhotoID = 42
	*snapshot.CompletedAt = time.Time{}
	assert.Equal(t, AnalyzeTaskStatusRunning, svc.GetTaskStatus().Status)
	assert.Equal(t, uint(3), *svc.GetTaskStatus().CurrentPhotoID)
	assert.False(t, svc.GetTaskStatus().CompletedAt.IsZero())
}
func TestAIService_RuntimeLeaseCleanupJoinsHeartbeat(t *testing.T) {
	svc, runtime := setupAILeaseService(t, &leaseTestAIProvider{}, 0)
	gate := make(chan struct{})
	runtime.heartbeatGate = gate
	var unblock sync.Once
	t.Cleanup(func() { unblock.Do(func() { close(gate) }) })
	task := &AnalyzeTask{ID: "cleanup", Mode: model.AnalysisOwnerTypeBatch, Status: AnalyzeTaskStatusRunning}
	_, err := runtime.AcquireGlobal(task.Mode, task.ID, "cleanup test")
	require.NoError(t, err)
	ctx, finish := svc.startRuntimeLease(task)
	require.Eventually(t, func() bool { return runtime.heartbeats.Load() > 0 }, time.Second, time.Millisecond)
	finished := make(chan struct{})
	go func() { finish(); close(finished) }()
	select {
	case <-finished:
		t.Fatal("cleanup returned before the in-flight heartbeat")
	case <-time.After(10 * time.Millisecond):
	}
	assert.Zero(t, runtime.releases.Load(), "heartbeat must finish before release")
	unblock.Do(func() { close(gate) })
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("cleanup did not finish")
	}
	finish()
	assert.ErrorIs(t, context.Cause(ctx), context.Canceled)
	assert.Equal(t, int32(1), runtime.releases.Load(), "cleanup is idempotent")
	heartbeats := runtime.heartbeats.Load()
	time.Sleep(20 * time.Millisecond)
	assert.Equal(t, heartbeats, runtime.heartbeats.Load())
}

func TestAIService_ExpiredRuntimeLeaseStopsSleepingBackground(t *testing.T) {
	svc, runtime := setupAILeaseService(t, &leaseTestAIProvider{}, 0)
	task, err := svc.StartBackgroundAnalyze()
	require.NoError(t, err)
	waitAILeaseTask(t, svc, AnalyzeTaskStatusSleeping)
	db := runtime.AnalysisRuntimeService.(*analysisRuntimeService).db
	require.NoError(t, db.Model(&model.AnalysisRuntimeLease{}).Where("owner_id = ?", task.ID).
		Update("lease_expires_at", time.Now().UTC().Add(-time.Minute)).Error)
	finished := waitAILeaseTask(t, svc, AnalyzeTaskStatusFailed)
	assert.Contains(t, finished.ErrorMessage, ErrAnalysisRuntimeOwnedByOther.Error())
	assert.Zero(t, runtime.releases.Load())
}

func TestAIService_RuntimeLeaseLossDuringBlockedWrite(t *testing.T) {
	p := &leaseTestAIProvider{gatePhase: "caption", started: make(chan struct{}), gate: make(chan struct{})}
	svc, runtime := setupAILeaseService(t, p, 1)
	var unblock sync.Once
	t.Cleanup(func() { unblock.Do(func() { close(p.gate) }) })
	db := runtime.AnalysisRuntimeService.(*analysisRuntimeService).db
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(4)

	_, err = svc.AnalyzeBatch(1)
	require.NoError(t, err)
	select {
	case <-p.started:
	case <-time.After(2 * time.Second):
		t.Fatal("provider did not start")
	}
	ctx := context.Background()
	conn, err := sqlDB.Conn(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, conn.Close()) })
	_, err = conn.ExecContext(ctx, "BEGIN IMMEDIATE")
	require.NoError(t, err)
	_, err = conn.ExecContext(ctx, "UPDATE analysis_runtime_leases SET updated_at = CURRENT_TIMESTAMP WHERE resource_key = ?", model.GlobalAnalysisResourceKey)
	require.NoError(t, err)
	unblock.Do(func() { close(p.gate) })
	time.Sleep(200 * time.Millisecond)
	assert.Equal(t, AnalyzeTaskStatusRunning, svc.GetTaskStatus().Status)

	_, err = conn.ExecContext(ctx, "UPDATE analysis_runtime_leases SET owner_type = ?, owner_id = ? WHERE resource_key = ?", model.AnalysisOwnerTypeAnalyzer, "replacement", model.GlobalAnalysisResourceKey)
	require.NoError(t, err)
	_, err = conn.ExecContext(ctx, "COMMIT")
	require.NoError(t, err)

	finished := waitAILeaseTask(t, svc, AnalyzeTaskStatusFailed)
	assert.Contains(t, finished.ErrorMessage, ErrAnalysisRuntimeOwnedByOther.Error())
	assert.Zero(t, finished.SuccessCount)
	assert.Zero(t, finished.FailedCount)
	count, err := svc.photoRepo.CountAnalyzed()
	require.NoError(t, err)
	assert.Zero(t, count)
	status, err := runtime.GetGlobalStatus()
	require.NoError(t, err)
	assert.Equal(t, "replacement", status.OwnerID)
	assert.Zero(t, runtime.releases.Load())
}

func TestAIService_NormalBackgroundStopFinishesCurrentWork(t *testing.T) {
	p := &leaseTestAIProvider{gatePhase: "analyze", started: make(chan struct{}), gate: make(chan struct{})}
	svc, runtime := setupAILeaseService(t, p, 1)
	var unblock sync.Once
	t.Cleanup(func() { unblock.Do(func() { close(p.gate) }) })
	_, err := svc.StartBackgroundAnalyze()
	require.NoError(t, err)
	select {
	case <-p.started:
	case <-time.After(time.Second):
		t.Fatal("provider did not start")
	}
	require.NoError(t, svc.StopBackgroundAnalyze())
	unblock.Do(func() { close(p.gate) })
	finished := waitAILeaseTask(t, svc, AnalyzeTaskStatusCompleted)
	assert.Empty(t, finished.ErrorMessage)
	assert.Equal(t, "后台分析已停止", finished.CurrentMessage)
	assert.Equal(t, 1, finished.SuccessCount)
	assert.Equal(t, int32(1), runtime.releases.Load())
}

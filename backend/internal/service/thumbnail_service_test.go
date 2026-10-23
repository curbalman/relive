package service

import (
	"fmt"
	"image"
	"image/jpeg"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/davidhoo/relive/internal/model"
	"github.com/davidhoo/relive/internal/repository"
	"github.com/davidhoo/relive/pkg/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

func setupThumbnailServiceTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: gormlogger.Discard})
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	require.NoError(t, db.AutoMigrate(&model.Photo{}, &model.ThumbnailJob{}))
	t.Cleanup(func() {
		sqlDB, _ := db.DB()
		if sqlDB != nil {
			sqlDB.Close()
		}
	})
	return db
}

func newThumbnailServiceForTest(t *testing.T) (ThumbnailService, *gorm.DB) {
	t.Helper()
	db := setupThumbnailServiceTestDB(t)
	photoRepo := repository.NewPhotoRepository(db)
	jobRepo := repository.NewThumbnailJobRepository(db)
	cfg := &config.Config{
		Photos: config.PhotosConfig{ThumbnailPath: t.TempDir()},
	}
	svc := NewThumbnailService(db, photoRepo, jobRepo, cfg)
	return svc, db
}

func TestThumbnailService_GetTaskStatus_Nil(t *testing.T) {
	svc, _ := newThumbnailServiceForTest(t)
	assert.Nil(t, svc.GetTaskStatus())
}

func TestThumbnailService_GetStats_Empty(t *testing.T) {
	svc, _ := newThumbnailServiceForTest(t)

	stats, err := svc.GetStats()
	require.NoError(t, err)
	assert.Equal(t, int64(0), stats.Total)
}

func TestThumbnailService_GetStats_WithJobs(t *testing.T) {
	svc, db := newThumbnailServiceForTest(t)

	now := time.Now()
	db.Create(&model.ThumbnailJob{PhotoID: 1, FilePath: "/p/1.jpg", Status: model.ThumbnailJobStatusCompleted, Source: model.ThumbnailJobSourceScan, QueuedAt: now})
	db.Create(&model.ThumbnailJob{PhotoID: 2, FilePath: "/p/2.jpg", Status: model.ThumbnailJobStatusPending, Source: model.ThumbnailJobSourceScan, QueuedAt: now})

	stats, err := svc.GetStats()
	require.NoError(t, err)
	assert.Equal(t, int64(2), stats.Total)
	assert.Equal(t, int64(1), stats.Completed)
	assert.Equal(t, int64(1), stats.Pending)
}

func TestThumbnailService_StopBackground_NotRunning(t *testing.T) {
	svc, _ := newThumbnailServiceForTest(t)

	err := svc.StopBackground()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not running")
}

func TestThumbnailService_HandleShutdown_NotRunning(t *testing.T) {
	svc, _ := newThumbnailServiceForTest(t)

	err := svc.HandleShutdown()
	require.NoError(t, err)
}

func TestThumbnailService_GetBackgroundLogs_Empty(t *testing.T) {
	svc, _ := newThumbnailServiceForTest(t)
	logs := svc.GetBackgroundLogs()
	assert.Empty(t, logs)
}

func TestThumbnailService_StartupRecoversInterruptedJobs(t *testing.T) {
	for _, tc := range []struct {
		name       string
		status     string
		fileExists bool
		fresh      bool
		want       string
	}{
		{"ready with file", model.ThumbnailStatusReady, true, false, model.ThumbnailJobStatusCompleted},
		{"ready missing file", model.ThumbnailStatusReady, false, false, model.ThumbnailJobStatusQueued},
		{"unfinished", model.ThumbnailStatusPending, false, false, model.ThumbnailJobStatusQueued},
		{"fresh heartbeat", model.ThumbnailStatusReady, true, true, model.ThumbnailJobStatusProcessing},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := setupThumbnailServiceTestDB(t)
			cfg := &config.Config{Photos: config.PhotosConfig{ThumbnailPath: t.TempDir()}}
			photo := &model.Photo{FilePath: "/photos/a.jpg", FileName: "a.jpg", ThumbnailPath: "a.jpg", ThumbnailStatus: tc.status}
			require.NoError(t, db.Create(photo).Error)
			if tc.fileExists {
				require.NoError(t, os.WriteFile(filepath.Join(cfg.Photos.ThumbnailPath, "a.jpg"), []byte("thumbnail"), 0600))
			}
			old := time.Now().Add(-time.Hour)
			updated := old
			if tc.fresh {
				updated = time.Now()
			}
			job := &model.ThumbnailJob{PhotoID: photo.ID, FilePath: photo.FilePath, Source: model.ThumbnailJobSourceScan,
				Status: model.ThumbnailJobStatusProcessing, StartedAt: &old, UpdatedAt: updated, AttemptCount: 1, QueuedAt: old}
			require.NoError(t, db.Create(job).Error)
			jobs := repository.NewThumbnailJobRepository(db)
			_ = NewThumbnailService(db, repository.NewPhotoRepository(db), jobs, cfg)
			got, err := jobs.GetByID(job.ID)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got.Status)
			if tc.want == model.ThumbnailJobStatusCompleted {
				assert.NotNil(t, got.CompletedAt)
			}
			if tc.want == model.ThumbnailJobStatusQueued {
				assert.Nil(t, got.StartedAt)
				assert.Nil(t, got.CompletedAt)
			}
		})
	}
}

func TestThumbnailService_RecoveryDoesNotStealLocalWorker(t *testing.T) {
	service, db := newThumbnailServiceForTest(t)
	svc := service.(*thumbnailService)
	old := time.Now().Add(-time.Hour)
	job := &model.ThumbnailJob{PhotoID: 1, FilePath: "/a.jpg", Source: model.ThumbnailJobSourceManual,
		Status: model.ThumbnailJobStatusProcessing, StartedAt: &old, UpdatedAt: old, QueuedAt: old}
	require.NoError(t, db.Create(job).Error)
	svc.processing[job.ID] = true
	require.NoError(t, svc.recoverStaleJobs())
	got, err := svc.jobRepo.GetByID(job.ID)
	require.NoError(t, err)
	assert.Equal(t, model.ThumbnailJobStatusProcessing, got.Status)
}

func TestThumbnailService_ForceRebuildAfterInterruptedReadyJob(t *testing.T) {
	service, db := newThumbnailServiceForTest(t)
	svc := service.(*thumbnailService)
	photo := &model.Photo{FilePath: "/photos/a.jpg", FileName: "a.jpg", ThumbnailPath: "a.jpg", ThumbnailStatus: model.ThumbnailStatusReady}
	require.NoError(t, db.Create(photo).Error)
	dest := filepath.Join(svc.config.Photos.ThumbnailPath, photo.ThumbnailPath)
	require.NoError(t, os.WriteFile(dest, []byte("existing thumbnail"), 0600))
	old := time.Now().Add(-time.Hour)
	job := &model.ThumbnailJob{PhotoID: photo.ID, FilePath: photo.FilePath, Source: model.ThumbnailJobSourceScan,
		Status: model.ThumbnailJobStatusProcessing, StartedAt: &old, UpdatedAt: old, AttemptCount: 1, QueuedAt: old}
	require.NoError(t, db.Create(job).Error)
	require.NoError(t, svc.EnqueuePhoto(photo.ID, model.ThumbnailJobSourceManual, 80, true))
	history, err := svc.jobRepo.GetByID(job.ID)
	require.NoError(t, err)
	assert.Equal(t, model.ThumbnailJobStatusCompleted, history.Status)
	assert.NotNil(t, history.CompletedAt)
	claimed, err := svc.jobRepo.ClaimNextJob()
	require.NoError(t, err)
	require.NotNil(t, claimed)
	assert.NotEqual(t, job.ID, claimed.ID)
	contents, err := os.ReadFile(dest)
	require.NoError(t, err)
	assert.Equal(t, "existing thumbnail", string(contents))
}

func TestThumbnailService_ForceRequestDuringProcessingIsRequeued(t *testing.T) {
	service, db := newThumbnailServiceForTest(t)
	svc := service.(*thumbnailService)
	input := filepath.Join(t.TempDir(), "input.jpg")
	file, err := os.Create(input)
	require.NoError(t, err)
	require.NoError(t, jpeg.Encode(file, image.NewRGBA(image.Rect(0, 0, 8, 4)), nil))
	require.NoError(t, file.Close())
	photo := &model.Photo{FilePath: input, FileName: "input.jpg"}
	require.NoError(t, db.Create(photo).Error)
	require.NoError(t, svc.EnqueuePhoto(photo.ID, model.ThumbnailJobSourceManual, 80, true))
	claimed, err := svc.jobRepo.ClaimNextJob()
	require.NoError(t, err)
	require.NotNil(t, claimed)
	require.NoError(t, svc.EnqueuePhoto(photo.ID, model.ThumbnailJobSourceManual, 80, true))
	require.NoError(t, svc.processJob(claimed))
	rerun, err := svc.jobRepo.ClaimNextJob()
	require.NoError(t, err)
	require.NotNil(t, rerun)
	assert.Equal(t, claimed.ID, rerun.ID)
	assert.Equal(t, claimed.AttemptCount+1, rerun.AttemptCount)
	require.NoError(t, svc.processJob(rerun))
	finished, err := svc.jobRepo.GetByID(claimed.ID)
	require.NoError(t, err)
	assert.Equal(t, model.ThumbnailJobStatusCompleted, finished.Status)
	updated, err := svc.photoRepo.GetByID(photo.ID)
	require.NoError(t, err)
	assert.Equal(t, model.ThumbnailStatusReady, updated.ThumbnailStatus)
	assert.FileExists(t, filepath.Join(svc.config.Photos.ThumbnailPath, updated.ThumbnailPath))
}

func TestThumbnailService_ReplacedWorkerCannotPublish(t *testing.T) {
	service, db := newThumbnailServiceForTest(t)
	svc := service.(*thumbnailService)
	photo := &model.Photo{FilePath: "/photos/a.jpg", FileName: "a.jpg", ThumbnailPath: "a.jpg", ThumbnailStatus: model.ThumbnailStatusPending}
	require.NoError(t, db.Create(photo).Error)
	require.NoError(t, svc.EnqueuePhoto(photo.ID, model.ThumbnailJobSourceManual, 80, true))
	oldClaim, err := svc.jobRepo.ClaimNextJob()
	require.NoError(t, err)
	require.NotNil(t, oldClaim)
	require.NoError(t, db.Model(&model.ThumbnailJob{}).Where("id = ?", oldClaim.ID).UpdateColumn("updated_at", time.Now().Add(-time.Hour)).Error)
	require.NoError(t, svc.recoverStaleJobs())
	newClaim, err := svc.jobRepo.ClaimNextJob()
	require.NoError(t, err)
	require.NotNil(t, newClaim)
	dest := filepath.Join(svc.config.Photos.ThumbnailPath, "a.jpg")
	require.NoError(t, os.WriteFile(dest, []byte("current thumbnail"), 0600))
	tempDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(tempDir, "a.jpg"), []byte("stale thumbnail"), 0600))
	require.ErrorContains(t, svc.finishJob(oldClaim, photo, tempDir, "a.jpg", nil), "claim was replaced")
	contents, err := os.ReadFile(dest)
	require.NoError(t, err)
	assert.Equal(t, "current thumbnail", string(contents))
	got, err := svc.jobRepo.GetByID(newClaim.ID)
	require.NoError(t, err)
	assert.Equal(t, model.ThumbnailJobStatusProcessing, got.Status)
	assert.Equal(t, newClaim.AttemptCount, got.AttemptCount)
	updated, err := svc.photoRepo.GetByID(photo.ID)
	require.NoError(t, err)
	assert.Equal(t, model.ThumbnailStatusPending, updated.ThumbnailStatus)
}

func TestThumbnailService_SeedPendingJobsIncludesEveryPage(t *testing.T) {
	service, db := newThumbnailServiceForTest(t)
	svc := service.(*thumbnailService)
	photos := make([]model.Photo, 601)
	for i := range photos {
		photos[i] = model.Photo{FilePath: fmt.Sprintf("/photos/%d.jpg", i), FileName: fmt.Sprintf("%d.jpg", i)}
	}
	require.NoError(t, db.CreateInBatches(&photos, 50).Error)
	require.NoError(t, svc.seedPendingJobs())
	stats, err := svc.jobRepo.GetStats()
	require.NoError(t, err)
	assert.Equal(t, int64(len(photos)), stats.Queued)
	for _, photo := range []*model.Photo{&photos[0], &photos[499], &photos[500], &photos[600]} {
		job, err := svc.jobRepo.GetActiveByPhotoID(photo.ID)
		require.NoError(t, err)
		require.NotNil(t, job)
	}
}

func TestThumbnailService_RecoveryCancelsRemovedAndExcludedPhotos(t *testing.T) {
	for _, removed := range []bool{false, true} {
		t.Run(fmt.Sprintf("removed=%v", removed), func(t *testing.T) {
			service, db := newThumbnailServiceForTest(t)
			svc := service.(*thumbnailService)
			photo := &model.Photo{FilePath: "/photos/a.jpg", FileName: "a.jpg", Status: model.PhotoStatusExcluded}
			require.NoError(t, db.Create(photo).Error)
			if removed {
				require.NoError(t, db.Delete(photo).Error)
			}
			old := time.Now().Add(-time.Hour)
			job := &model.ThumbnailJob{PhotoID: photo.ID, FilePath: photo.FilePath, Source: model.ThumbnailJobSourceScan,
				Status: model.ThumbnailJobStatusProcessing, StartedAt: &old, UpdatedAt: old, AttemptCount: 1, QueuedAt: old}
			require.NoError(t, db.Create(job).Error)
			require.NoError(t, svc.recoverStaleJobs())
			got, err := svc.jobRepo.GetByID(job.ID)
			require.NoError(t, err)
			assert.Equal(t, model.ThumbnailJobStatusCancelled, got.Status)
			assert.NotNil(t, got.CompletedAt)
			assert.WithinDuration(t, old, *got.StartedAt, time.Microsecond)
		})
	}
}

func TestThumbnailService_BackgroundProcessesAndStops(t *testing.T) {
	service, db := newThumbnailServiceForTest(t)
	svc := service.(*thumbnailService)
	inputDir := t.TempDir()
	for i := 0; i < 3; i++ {
		input := filepath.Join(inputDir, fmt.Sprintf("%d.jpg", i))
		file, err := os.Create(input)
		require.NoError(t, err)
		require.NoError(t, jpeg.Encode(file, image.NewRGBA(image.Rect(0, 0, 8, 4)), nil))
		require.NoError(t, file.Close())
		photo := &model.Photo{FilePath: input, FileName: filepath.Base(input)}
		require.NoError(t, db.Create(photo).Error)
		require.NoError(t, svc.EnqueuePhoto(photo.ID, model.ThumbnailJobSourceScan, 50, false))
	}
	_, err := svc.StartBackground()
	require.NoError(t, err)
	svc.taskMutex.RLock()
	active := svc.active
	svc.taskMutex.RUnlock()
	t.Cleanup(func() {
		_ = svc.StopBackground()
		select {
		case <-active.done:
		case <-time.After(3 * time.Second):
			t.Error("thumbnail workers did not stop")
		}
	})
	require.Eventually(t, func() bool {
		stats, err := svc.GetStats()
		return err == nil && stats.Completed == 3 && stats.Processing == 0
	}, 3*time.Second, 10*time.Millisecond)
	assert.Equal(t, int64(3), svc.GetTaskStatus().ProcessedJobs)
}

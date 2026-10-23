package service

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/davidhoo/relive/internal/model"
	"github.com/davidhoo/relive/internal/repository"
	"github.com/davidhoo/relive/internal/util"
	"github.com/davidhoo/relive/pkg/config"
	"github.com/davidhoo/relive/pkg/database"
	"github.com/davidhoo/relive/pkg/logger"
	"gorm.io/gorm"
)

const (
	thumbnailPriorityScan      = 50
	thumbnailPriorityManual    = 80
	thumbnailPriorityPassive   = 100
	thumbnailJobStaleAfter     = 15 * time.Minute
	thumbnailHeartbeatInterval = 30 * time.Second
)

type ThumbnailService interface {
	StartBackground() (*model.ThumbnailTask, error)
	StopBackground() error
	GetTaskStatus() *model.ThumbnailTask
	GetStats() (*model.ThumbnailStatsResponse, error)
	GetBackgroundLogs() []string
	EnqueuePhoto(photoID uint, source string, priority int, force bool) error
	EnqueueByPath(path string, source string, priority int) (int, error)
	GeneratePhoto(photoID uint, force bool) error
	HandleShutdown() error
}

type thumbnailService struct {
	db         *gorm.DB
	photoRepo  repository.PhotoRepository
	jobRepo    repository.ThumbnailJobRepository
	config     *config.Config
	generator  *util.ThumbnailGenerator
	writeQueue *database.WriteQueue

	taskMutex       sync.RWMutex
	task            *model.ThumbnailTask
	active          *activeThumbnailTask
	backgroundLogMu sync.RWMutex
	backgroundLogs  []string
	processingMu    sync.Mutex
	processing      map[uint]bool
}

type activeThumbnailTask struct {
	stopCh chan struct{}
	done   chan struct{}
	mu     sync.Mutex
	stop   bool
}

func NewThumbnailService(db *gorm.DB, photoRepo repository.PhotoRepository, jobRepo repository.ThumbnailJobRepository, cfg *config.Config) ThumbnailService {
	s := &thumbnailService{
		db:         db,
		photoRepo:  photoRepo,
		jobRepo:    jobRepo,
		config:     cfg,
		generator:  util.NewThumbnailGenerator(1024, 1024, 90, cfg.Photos.ThumbnailPath),
		writeQueue: database.GetWriteQueue(),
		processing: make(map[uint]bool),
	}
	if err := s.recoverStaleJobs(); err != nil {
		logger.Warnf("recover interrupted thumbnail jobs on startup: %v", err)
	}
	return s
}

func (s *thumbnailService) StartBackground() (*model.ThumbnailTask, error) {
	s.taskMutex.Lock()
	defer s.taskMutex.Unlock()
	if s.active != nil {
		return nil, fmt.Errorf("thumbnail task already running")
	}
	now := time.Now()
	task := &model.ThumbnailTask{Status: model.TaskStatusRunning, StartedAt: &now}
	active := &activeThumbnailTask{stopCh: make(chan struct{}), done: make(chan struct{})}
	s.task = task
	s.active = active
	s.resetBackgroundLogs()
	s.appendBackgroundLog("缩略图后台生成已启动")
	go s.runBackground(active)
	return cloneThumbnailTask(task), nil
}

func (s *thumbnailService) StopBackground() error {
	s.taskMutex.Lock()
	defer s.taskMutex.Unlock()
	if s.active == nil {
		return fmt.Errorf("thumbnail task not running")
	}
	s.active.mu.Lock()
	if !s.active.stop {
		s.active.stop = true
		close(s.active.stopCh)
	}
	s.active.mu.Unlock()
	if s.task != nil && s.task.Status == model.TaskStatusRunning {
		s.task.Status = model.TaskStatusStopping
		s.appendBackgroundLog("收到停止请求，等待当前任务处理完成")
	}
	return nil
}

func (s *thumbnailService) GetTaskStatus() *model.ThumbnailTask {
	s.taskMutex.RLock()
	defer s.taskMutex.RUnlock()
	return cloneThumbnailTask(s.task)
}

func (s *thumbnailService) GetBackgroundLogs() []string {
	s.backgroundLogMu.RLock()
	defer s.backgroundLogMu.RUnlock()
	logs := make([]string, len(s.backgroundLogs))
	copy(logs, s.backgroundLogs)
	return logs
}

func (s *thumbnailService) GetStats() (*model.ThumbnailStatsResponse, error) {
	stats, err := s.jobRepo.GetStats()
	if err != nil {
		return nil, err
	}
	return &model.ThumbnailStatsResponse{
		Total:      stats.Total,
		Pending:    stats.Pending,
		Queued:     stats.Queued,
		Processing: stats.Processing,
		Completed:  stats.Completed,
		Failed:     stats.Failed,
		Cancelled:  stats.Cancelled,
	}, nil
}

func (s *thumbnailService) HandleShutdown() error {
	s.taskMutex.RLock()
	active := s.active
	s.taskMutex.RUnlock()
	if active == nil {
		return nil
	}
	return s.StopBackground()
}

func (s *thumbnailService) EnqueuePhoto(photoID uint, source string, priority int, force bool) error {
	photo, err := s.photoRepo.GetByID(photoID)
	if err != nil {
		return err
	}
	return s.enqueuePhotoModel(photo, source, priority, force)
}

func (s *thumbnailService) EnqueueByPath(path string, source string, priority int) (int, error) {
	photos, err := s.photoRepo.ListByPathPrefix(path)
	if err != nil {
		return 0, err
	}
	count := 0
	for _, photo := range photos {
		if photo.Status == model.PhotoStatusExcluded {
			continue
		}
		if err := s.enqueuePhotoModel(photo, source, priority, false); err != nil {
			logger.Warnf("enqueue thumbnail by path failed for photo %d: %v", photo.ID, err)
			continue
		}
		count++
	}
	return count, nil
}

// GeneratePhoto 直接为单张照片生成缩略图（同步执行，不经过队列）
func (s *thumbnailService) GeneratePhoto(photoID uint, force bool) error {
	photo, err := s.photoRepo.GetByID(photoID)
	if err != nil {
		return err
	}
	if photo == nil {
		return fmt.Errorf("photo %d not found", photoID)
	}
	if photo.Status == model.PhotoStatusExcluded {
		return fmt.Errorf("photo %d is excluded", photoID)
	}

	thumbnailPath := photo.ThumbnailPath
	if thumbnailPath == "" {
		thumbnailPath = util.GenerateDerivedImagePath(photo.FilePath)
	}

	// 非强制模式下，如果文件已存在则直接标记 ready
	if !force {
		fullPath := filepath.Join(s.config.Photos.ThumbnailPath, thumbnailPath)
		if _, err := os.Stat(fullPath); err == nil {
			now := time.Now()
			return s.executeWrite(func() error {
				return s.photoRepo.UpdateFields(photo.ID, map[string]interface{}{
					"thumbnail_path":         thumbnailPath,
					"thumbnail_status":       model.ThumbnailStatusReady,
					"thumbnail_generated_at": &now,
				})
			})
		}
	} else {
		// 强制模式：删除旧文件
		fullPath := filepath.Join(s.config.Photos.ThumbnailPath, thumbnailPath)
		if _, err := os.Stat(fullPath); err == nil {
			if err := os.Remove(fullPath); err != nil {
				logger.Warnf("Failed to remove old thumbnail for photo %d: %v", photo.ID, err)
			}
		}
	}

	logger.Infof("GeneratePhoto: photo=%d manualRotation=%d oldPath=%s", photo.ID, photo.ManualRotation, thumbnailPath)
	relPath, err := s.generator.GenerateThumbnailWithRotation(photo.FilePath, photo.ManualRotation)
	now := time.Now()
	if err != nil {
		_ = s.executeWrite(func() error {
			return s.photoRepo.UpdateFields(photo.ID, map[string]interface{}{
				"thumbnail_status": model.ThumbnailStatusFailed,
			})
		})
		return fmt.Errorf("生成缩略图失败: %w", err)
	}

	return s.executeWrite(func() error {
		return s.photoRepo.UpdateFields(photo.ID, map[string]interface{}{
			"thumbnail_path":         relPath,
			"thumbnail_status":       model.ThumbnailStatusReady,
			"thumbnail_generated_at": &now,
		})
	})
}

func (s *thumbnailService) enqueuePhotoModel(photo *model.Photo, source string, priority int, force bool) error {
	if photo == nil {
		return fmt.Errorf("photo is nil")
	}
	if photo.Status == model.PhotoStatusExcluded {
		return nil
	}
	if source == "" {
		source = model.ThumbnailJobSourceManual
	}
	if priority <= 0 {
		priority = thumbnailPriorityManual
	}
	activeJob, err := s.jobRepo.GetActiveByPhotoID(photo.ID)
	if err != nil {
		return err
	}
	if activeJob != nil && activeJob.Status == model.ThumbnailJobStatusProcessing {
		if err := s.recoverStaleJob(activeJob, time.Now().Add(-thumbnailJobStaleAfter)); err != nil {
			return err
		}
	}

	return s.executeWrite(func() error {
		return s.db.Transaction(func(tx *gorm.DB) error {
			jobs := repository.NewThumbnailJobRepository(tx)
			photos := repository.NewPhotoRepository(tx)
			current, err := photos.GetByID(photo.ID)
			if err != nil {
				return err
			}
			if current.Status == model.PhotoStatusExcluded {
				return nil
			}
			activeJob, err := jobs.GetActiveByPhotoID(photo.ID)
			if err != nil {
				return err
			}
			now := time.Now()
			if activeJob != nil && activeJob.Status == model.ThumbnailJobStatusProcessing {
				// Do not delete the file or disturb a live claim. A force request
				// is retained until this attempt finishes, then queued again.
				if force {
					return jobs.UpdateFields(activeJob.ID, map[string]interface{}{
						"priority": priority, "source": source, "last_requested_at": &now,
						// Requests are not worker heartbeats.
						"updated_at": activeJob.UpdatedAt,
					})
				}
				return nil
			}
			thumbnailPath := current.ThumbnailPath
			if thumbnailPath == "" {
				thumbnailPath = util.GenerateDerivedImagePath(current.FilePath)
			}
			if !force && activeJob == nil {
				if info, err := os.Stat(filepath.Join(s.config.Photos.ThumbnailPath, thumbnailPath)); err == nil && info.Mode().IsRegular() {
					return photos.UpdateFields(photo.ID, map[string]interface{}{
						"thumbnail_path": thumbnailPath, "thumbnail_status": model.ThumbnailStatusReady, "thumbnail_generated_at": &now,
					})
				}
			}
			if err := photos.UpdateFields(photo.ID, map[string]interface{}{
				"thumbnail_path": thumbnailPath, "thumbnail_status": model.ThumbnailStatusPending, "thumbnail_generated_at": nil,
			}); err != nil {
				return err
			}
			if activeJob != nil {
				return jobs.UpdateFields(activeJob.ID, map[string]interface{}{
					"status": model.ThumbnailJobStatusQueued, "priority": priority, "source": source, "last_requested_at": &now,
				})
			}
			return jobs.Create(&model.ThumbnailJob{
				PhotoID: photo.ID, FilePath: current.FilePath, Status: model.ThumbnailJobStatusQueued,
				Priority: priority, Source: source, QueuedAt: now, LastRequestedAt: &now,
			})
		})
	})
}

// Recover only expired claims. Local in-flight work is also excluded: a busy
// SQLite write queue must not cause this process to steal its own live worker.
func (s *thumbnailService) recoverStaleJobs() error {
	cutoff := time.Now().Add(-thumbnailJobStaleAfter)
	jobs, err := s.jobRepo.ListStaleProcessing(cutoff)
	if err != nil {
		return err
	}
	for i := range jobs {
		if err := s.recoverStaleJob(&jobs[i], cutoff); err != nil {
			return err
		}
	}
	return nil
}

func (s *thumbnailService) recoverStaleJob(job *model.ThumbnailJob, cutoff time.Time) error {
	if !job.UpdatedAt.Before(cutoff) {
		return nil
	}
	s.processingMu.Lock()
	defer s.processingMu.Unlock()
	if s.processing[job.ID] {
		return nil
	}
	photo, err := s.photoRepo.GetByID(job.PhotoID)
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return err
	}
	now := time.Now()
	updates := map[string]interface{}{
		"status": model.ThumbnailJobStatusQueued, "started_at": nil, "completed_at": nil,
		"queued_at": now, "last_error": "recovered interrupted thumbnail attempt",
	}
	if photo == nil || errors.Is(err, gorm.ErrRecordNotFound) || photo.Status == model.PhotoStatusExcluded {
		updates["status"] = model.ThumbnailJobStatusCancelled
		updates["completed_at"] = &now
		updates["started_at"] = job.StartedAt
	} else if photo.ThumbnailStatus == model.ThumbnailStatusReady && photo.ThumbnailPath != "" &&
		(job.LastRequestedAt == nil || job.StartedAt == nil || !job.LastRequestedAt.After(*job.StartedAt)) {
		if info, err := os.Stat(filepath.Join(s.config.Photos.ThumbnailPath, photo.ThumbnailPath)); err == nil && info.Mode().IsRegular() {
			updates["status"] = model.ThumbnailJobStatusCompleted
			updates["completed_at"] = &now
			updates["started_at"] = job.StartedAt
			updates["last_error"] = ""
		}
	}
	return s.executeWrite(func() error {
		_, err := s.jobRepo.UpdateStaleProcessing(job, cutoff, updates)
		return err
	})
}

func (s *thumbnailService) runBackground(active *activeThumbnailTask) {
	logger.Info("Thumbnail background task starting...")
	s.appendBackgroundLog("缩略图后台生成任务启动中...")

	defer func() {
		if r := recover(); r != nil {
			logger.Errorf("Thumbnail background task panic: %v", r)
			s.appendBackgroundLog(fmt.Sprintf("后台任务异常：%v", r))
		}
		now := time.Now()
		s.taskMutex.Lock()
		if s.task != nil && (s.task.Status == model.TaskStatusRunning || s.task.Status == model.TaskStatusStopping) {
			s.task.Status = model.TaskStatusStopped
			s.task.StoppedAt = &now
		}
		s.appendBackgroundLog("缩略图后台生成已停止")
		s.active = nil
		s.taskMutex.Unlock()
		close(active.done)
		logger.Info("Thumbnail background task stopped")
	}()

	if err := s.recoverStaleJobs(); err != nil {
		logger.Warnf("recover interrupted thumbnail jobs: %v", err)
	}
	s.appendBackgroundLog("正在检查待处理任务...")
	if err := s.seedPendingJobs(); err != nil {
		logger.Warnf("seed thumbnail jobs failed: %v", err)
		s.appendBackgroundLog(fmt.Sprintf("补齐历史待生成任务失败：%v", err))
	} else {
		s.appendBackgroundLog("已扫描历史照片并补齐缩略图待处理队列")
	}
	logger.Info("Thumbnail seedPendingJobs completed")

	workers := s.config.Performance.MaxThumbnailWorkers
	if workers <= 0 {
		workers = 2
	}
	// 限制并发数以减少 SQLite 锁竞争
	// SQLite 在 WAL 模式下支持并发读，但写仍是串行的
	maxWorkers := 2
	if workers < maxWorkers {
		maxWorkers = workers
	}
	var wg sync.WaitGroup
	for i := 0; i < maxWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-active.stopCh:
					return
				default:
				}
				// Claim only when a worker is ready; buffered claims would have
				// no heartbeat while waiting behind a slow image decode.
				job, err := s.jobRepo.ClaimNextJob()
				if err != nil {
					logger.Warnf("claim thumbnail job failed: %v", err)
				}
				delay := 500 * time.Millisecond
				if job != nil {
					delay = 50 * time.Millisecond
					s.updateTaskProgress(func(task *model.ThumbnailTask) {
						task.CurrentPhotoID = job.PhotoID
						task.CurrentFile = filepath.Base(job.FilePath)
					})
					if err := s.processJob(job); err != nil {
						logger.Warnf("process thumbnail job %d failed: %v", job.ID, err)
					}
				}
				select {
				case <-active.stopCh:
					return
				case <-time.After(delay):
				}
			}
		}()
	}
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-active.stopCh:
			wg.Wait()
			return
		case <-ticker.C:
			if err := s.recoverStaleJobs(); err != nil {
				logger.Warnf("recover interrupted thumbnail jobs: %v", err)
			}
		}
	}
}

func (s *thumbnailService) processJob(job *model.ThumbnailJob) error {
	s.processingMu.Lock()
	s.processing[job.ID] = true
	s.processingMu.Unlock()
	stopHeartbeat := make(chan struct{})
	heartbeatDone := make(chan struct{})
	go func() {
		defer close(heartbeatDone)
		ticker := time.NewTicker(thumbnailHeartbeatInterval)
		defer ticker.Stop()
		for {
			select {
			case <-stopHeartbeat:
				return
			case <-ticker.C:
				var owned bool
				err := s.executeWrite(func() error {
					var err error
					owned, err = s.jobRepo.UpdateProcessing(job, map[string]interface{}{"updated_at": time.Now()})
					return err
				})
				if err != nil {
					logger.Warnf("heartbeat thumbnail job %d: %v", job.ID, err)
				} else if !owned {
					return
				}
			}
		}
	}()
	defer func() {
		close(stopHeartbeat)
		<-heartbeatDone
		s.processingMu.Lock()
		delete(s.processing, job.ID)
		s.processingMu.Unlock()
	}()

	photo, err := s.photoRepo.GetByID(job.PhotoID)
	if err != nil || photo == nil {
		if err == nil {
			err = fmt.Errorf("photo %d not found", job.PhotoID)
		}
		return s.finishJob(job, nil, "", "", err)
	}
	if photo.Status == model.PhotoStatusExcluded {
		return s.finishJob(job, photo, "", "", nil)
	}
	// Render to an attempt-specific directory. An expired worker can never
	// overwrite the current thumbnail before its claim is checked below.
	if err := os.MkdirAll(s.config.Photos.ThumbnailPath, 0755); err != nil {
		return s.finishJob(job, photo, "", "", err)
	}
	tempDir, err := os.MkdirTemp(s.config.Photos.ThumbnailPath, ".thumbnail-job-")
	if err != nil {
		return s.finishJob(job, photo, "", "", err)
	}
	defer os.RemoveAll(tempDir)
	generator := util.NewThumbnailGenerator(1024, 1024, 90, tempDir)
	relPath, generateErr := generator.GenerateThumbnailWithRotation(photo.FilePath, photo.ManualRotation)
	return s.finishJob(job, photo, tempDir, relPath, generateErr)
}

func (s *thumbnailService) finishJob(job *model.ThumbnailJob, photo *model.Photo, tempDir, relPath string, generateErr error) error {
	err := s.executeWrite(func() error {
		return s.db.Transaction(func(tx *gorm.DB) error {
			jobs := repository.NewThumbnailJobRepository(tx)
			current, err := jobs.GetByID(job.ID)
			if err != nil {
				return err
			}
			if current == nil {
				return fmt.Errorf("thumbnail job %d no longer exists", job.ID)
			}
			now := time.Now()
			updates := map[string]interface{}{"status": model.ThumbnailJobStatusCompleted, "completed_at": &now, "last_error": ""}
			photoUpdates := map[string]interface{}{"thumbnail_path": relPath, "thumbnail_status": model.ThumbnailStatusReady, "thumbnail_generated_at": &now}
			if generateErr != nil {
				updates["status"] = model.ThumbnailJobStatusFailed
				updates["last_error"] = generateErr.Error()
				photoUpdates = map[string]interface{}{"thumbnail_status": model.ThumbnailStatusFailed}
			} else if photo != nil && photo.Status == model.PhotoStatusExcluded {
				updates["status"] = model.ThumbnailJobStatusCancelled
				photoUpdates = nil
			}
			if photo != nil && photo.Status != model.PhotoStatusExcluded && current.LastRequestedAt != nil &&
				job.StartedAt != nil && current.LastRequestedAt.After(*job.StartedAt) {
				updates["status"] = model.ThumbnailJobStatusQueued
				updates["started_at"] = nil
				updates["completed_at"] = nil
				updates["queued_at"] = now
				photoUpdates = map[string]interface{}{"thumbnail_status": model.ThumbnailStatusPending, "thumbnail_generated_at": nil}
			}
			owned, err := jobs.UpdateProcessing(job, updates)
			if err != nil {
				return err
			}
			if !owned {
				return fmt.Errorf("thumbnail job %d claim was replaced", job.ID)
			}
			if generateErr == nil && relPath != "" {
				dest := filepath.Join(s.config.Photos.ThumbnailPath, relPath)
				if err := os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
					return err
				}
				if err := os.Rename(filepath.Join(tempDir, relPath), dest); err != nil {
					return err
				}
			}
			if photo != nil && photoUpdates != nil {
				return repository.NewPhotoRepository(tx).UpdateFields(photo.ID, photoUpdates)
			}
			return nil
		})
	})
	if err != nil {
		return err
	}
	s.updateTaskProgress(func(task *model.ThumbnailTask) { task.ProcessedJobs++ })
	if generateErr != nil {
		s.appendBackgroundLog(fmt.Sprintf("生成照片 #%d 缩略图失败：%v", job.PhotoID, generateErr))
		return generateErr
	}
	s.appendBackgroundLog(fmt.Sprintf("照片 #%d 缩略图任务已处理", job.PhotoID))
	return nil
}

// executeWrite runs fn through WriteQueue if available, otherwise directly.
func (s *thumbnailService) executeWrite(fn func() error) error {
	if s.writeQueue != nil {
		return s.writeQueue.Execute(fn)
	}
	return fn()
}

// isSQLiteLockError 检查错误是否是 SQLite 锁定错误
func isSQLiteLockError(err error) bool {
	if err == nil {
		return false
	}
	errStr := err.Error()
	return containsStr(errStr, "database is locked") ||
		containsStr(errStr, "database table is locked") ||
		containsStr(errStr, "busy")
}

func containsStr(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsSubstr(s, substr))
}

func containsSubstr(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

func (s *thumbnailService) seedPendingJobs() error {
	logger.Info("Checking thumbnail job stats...")
	// 先检查是否已有足够的待处理任务
	stats, err := s.jobRepo.GetStats()
	if err != nil {
		logger.Errorf("Failed to get thumbnail job stats: %v", err)
		return fmt.Errorf("get thumbnail job stats: %w", err)
	}
	logger.Infof("Thumbnail job stats: pending=%d, queued=%d, processing=%d, completed=%d",
		stats.Pending, stats.Queued, stats.Processing, stats.Completed)

	// 如果已有待处理任务，跳过补齐
	if stats.Pending > 0 || stats.Queued > 0 {
		s.appendBackgroundLog(fmt.Sprintf("已有 %d 个待处理缩略图任务，跳过补齐", stats.Pending+stats.Queued))
		logger.Info("Skipping seedPendingJobs, tasks already exist")
		return nil
	}

	// 检查是否所有照片都已经处理完成（排除 excluded 照片）
	var totalPhotos, readyPhotos int64
	if err := s.db.Model(&model.Photo{}).Where("status = ?", model.PhotoStatusActive).Count(&totalPhotos).Error; err != nil {
		logger.Warnf("Failed to count total photos: %v", err)
	}
	if err := s.db.Model(&model.Photo{}).Where("status = ?", model.PhotoStatusActive).Where("thumbnail_status = ?", model.ThumbnailStatusReady).Count(&readyPhotos).Error; err != nil {
		logger.Warnf("Failed to count ready photos: %v", err)
	}
	if totalPhotos > 0 && totalPhotos == readyPhotos {
		s.appendBackgroundLog(fmt.Sprintf("所有 %d 张照片的缩略图已就绪", totalPhotos))
		logger.Info("All photos have thumbnails ready")
		return nil
	}

	s.appendBackgroundLog("正在扫描历史照片...")
	logger.Info("Scanning photos for thumbnail jobs...")

	// 先收集所有需要处理的照片ID，避免在FindInBatches回调中进行写入操作（排除 excluded 照片）
	var photoIDs []uint
	var lastID uint
	for {
		var batch []uint
		err = s.db.Model(&model.Photo{}).
			Where("status = ? AND id > ?", model.PhotoStatusActive, lastID).
			Where("thumbnail_status != ? OR thumbnail_status IS NULL OR thumbnail_path = ''", model.ThumbnailStatusReady).
			Order("id ASC").Limit(500).Pluck("id", &batch).Error
		if err != nil {
			return err
		}
		if len(batch) == 0 {
			break
		}
		photoIDs = append(photoIDs, batch...)
		lastID = batch[len(batch)-1]
	}

	if len(photoIDs) == 0 {
		s.appendBackgroundLog("没有需要生成缩略图的照片")
		return nil
	}

	s.appendBackgroundLog(fmt.Sprintf("找到 %d 张需要生成缩略图的照片，开始入队...", len(photoIDs)))
	logger.Infof("Found %d photos needing thumbnails, enqueuing...", len(photoIDs))

	// 分批入队，每批处理完后短暂暂停以减少锁竞争
	batchSize := 50
	successCount := 0
	for i := 0; i < len(photoIDs); i += batchSize {
		end := i + batchSize
		if end > len(photoIDs) {
			end = len(photoIDs)
		}
		batch := photoIDs[i:end]

		for _, id := range batch {
			if err := s.enqueuePhotoWithRetry(id); err != nil {
				if !isSQLiteLockError(err) {
					logger.Warnf("seed thumbnail job failed for photo %d: %v", id, err)
				}
			} else {
				successCount++
			}
		}

		// 每批处理完后短暂暂停，让出时间片并减少锁竞争
		if end < len(photoIDs) {
			time.Sleep(10 * time.Millisecond)
		}
	}

	s.appendBackgroundLog(fmt.Sprintf("已扫描历史照片并补齐 %d 个缩略图待处理队列", successCount))
	logger.Infof("Finished seeding %d thumbnail jobs", successCount)
	return nil
}

// enqueuePhotoWithRetry 带重试的照片入队
func (s *thumbnailService) enqueuePhotoWithRetry(photoID uint) error {
	var lastErr error
	for i := 0; i < 3; i++ {
		photo, err := s.photoRepo.GetByID(photoID)
		if err != nil {
			return err
		}
		if photo == nil {
			return fmt.Errorf("photo %d not found", photoID)
		}
		err = s.enqueuePhotoModel(photo, model.ThumbnailJobSourceManual, thumbnailPriorityManual, false)
		if err == nil {
			return nil
		}
		lastErr = err
		if isSQLiteLockError(err) {
			time.Sleep(time.Duration(i+1) * 50 * time.Millisecond)
			continue
		}
		return err
	}
	return lastErr
}

func (s *thumbnailService) updateTaskProgress(fn func(task *model.ThumbnailTask)) {
	s.taskMutex.Lock()
	defer s.taskMutex.Unlock()
	if s.task == nil {
		return
	}
	fn(s.task)
}

func (s *thumbnailService) appendBackgroundLog(message string) {
	if message == "" {
		return
	}
	entry := fmt.Sprintf("[%s] %s", time.Now().Format("2006-01-02 15:04:05"), message)
	s.backgroundLogMu.Lock()
	defer s.backgroundLogMu.Unlock()
	s.backgroundLogs = append(s.backgroundLogs, entry)
	if len(s.backgroundLogs) > 100 {
		s.backgroundLogs = s.backgroundLogs[len(s.backgroundLogs)-100:]
	}
}

func (s *thumbnailService) resetBackgroundLogs() {
	s.backgroundLogMu.Lock()
	defer s.backgroundLogMu.Unlock()
	s.backgroundLogs = make([]string, 0, 100)
}

func cloneThumbnailTask(task *model.ThumbnailTask) *model.ThumbnailTask {
	if task == nil {
		return nil
	}
	copy := *task
	return &copy
}

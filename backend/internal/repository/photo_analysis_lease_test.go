package repository

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/davidhoo/relive/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func openAnalysisLeaseDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "analysis-lease.db") + "?_busy_timeout=5000"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(2)
	t.Cleanup(func() { require.NoError(t, sqlDB.Close()) })
	require.NoError(t, db.AutoMigrate(&model.Photo{}, &model.PhotoTag{}, &model.PhotoTagStats{}, &model.AnalysisRuntimeLease{}))
	return db
}

func seedAnalysisLease(t *testing.T, db *gorm.DB, ownerType, ownerID string, expires time.Time) uint {
	t.Helper()
	now := time.Now().UTC()
	require.NoError(t, db.Create(&model.AnalysisRuntimeLease{
		ResourceKey:     model.GlobalAnalysisResourceKey,
		OwnerType:       ownerType,
		OwnerID:         ownerID,
		Status:          model.AnalysisRuntimeStatusRunning,
		StartedAt:       &now,
		LastHeartbeatAt: &now,
		LeaseExpiresAt:  &expires,
	}).Error)
	photo := &model.Photo{
		FilePath: "/photos/lease.jpg", FileName: "lease.jpg", FileHash: "lease",
		ThumbnailStatus: model.ThumbnailStatusReady,
	}
	require.NoError(t, db.Create(photo).Error)
	return photo.ID
}

func analysisLeaseUpdates() map[string]interface{} {
	now := time.Now().UTC()
	return map[string]interface{}{
		"ai_analyzed": true, "ai_provider": "lease-test", "description": "kept",
		"caption": "caption", "main_category": "人物", "tags": "family,trip",
		"memory_score": 3, "beauty_score": 4, "overall_score": 4, "analyzed_at": &now,
	}
}

func TestUpdateAnalysisWithLease_PublishesForCurrentOwner(t *testing.T) {
	db := openAnalysisLeaseDB(t)
	id := seedAnalysisLease(t, db, model.AnalysisOwnerTypeBatch, "owner-a", time.Now().UTC().Add(time.Minute))
	repo := NewPhotoRepository(db)

	owned, err := repo.UpdateAnalysisWithLease(context.Background(), model.AnalysisOwnerTypeBatch, "owner-a", id, analysisLeaseUpdates(), "family,trip", true)
	require.NoError(t, err)
	require.True(t, owned, "an unchanged owner id must still count as ownership")

	replacement := analysisLeaseUpdates()
	replacement["tags"] = "family"
	owned, err = repo.UpdateAnalysisWithLease(context.Background(), model.AnalysisOwnerTypeBatch, "owner-a", id, replacement, "family", true)
	require.NoError(t, err)
	assert.True(t, owned)

	var photo model.Photo
	require.NoError(t, db.First(&photo, id).Error)
	assert.True(t, photo.AIAnalyzed)
	assert.Equal(t, "family", photo.Tags)
	var tags []string
	require.NoError(t, db.Model(&model.PhotoTag{}).Where("photo_id = ?", id).Pluck("tag", &tags).Error)
	assert.ElementsMatch(t, []string{"family"}, tags)
}

func TestUpdateAnalysisWithLease_RejectsLostOrExpiredOwner(t *testing.T) {
	db := openAnalysisLeaseDB(t)
	id := seedAnalysisLease(t, db, model.AnalysisOwnerTypeBatch, "owner-a", time.Now().UTC().Add(-time.Minute))
	repo := NewPhotoRepository(db)
	updates := analysisLeaseUpdates()

	owned, err := repo.UpdateAnalysisWithLease(context.Background(), model.AnalysisOwnerTypeBatch, "owner-a", id, updates, "family", true)
	require.NoError(t, err)
	assert.False(t, owned)

	require.NoError(t, db.Model(&model.AnalysisRuntimeLease{}).
		Where("resource_key = ?", model.GlobalAnalysisResourceKey).
		Updates(map[string]interface{}{
			"owner_id":         "replacement",
			"owner_type":       model.AnalysisOwnerTypeAnalyzer,
			"lease_expires_at": time.Now().UTC().Add(time.Minute),
		}).Error)
	owned, err = repo.UpdateAnalysisWithLease(context.Background(), model.AnalysisOwnerTypeBatch, "owner-a", id, updates, "family", true)
	require.NoError(t, err)
	assert.False(t, owned)

	var photo model.Photo
	require.NoError(t, db.First(&photo, id).Error)
	assert.False(t, photo.AIAnalyzed)
	var tags int64
	require.NoError(t, db.Model(&model.PhotoTag{}).Where("photo_id = ?", id).Count(&tags).Error)
	assert.Zero(t, tags)
}

func TestUpdateAnalysisWithLease_DropsResultWhenOwnerChangesWhileBlocked(t *testing.T) {
	db := openAnalysisLeaseDB(t)
	id := seedAnalysisLease(t, db, model.AnalysisOwnerTypeBatch, "owner-a", time.Now().UTC().Add(time.Minute))
	repo := NewPhotoRepository(db)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	ctx := context.Background()
	conn, err := sqlDB.Conn(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, conn.Close()) })
	_, err = conn.ExecContext(ctx, "BEGIN IMMEDIATE")
	require.NoError(t, err)
	_, err = conn.ExecContext(ctx, "UPDATE analysis_runtime_leases SET updated_at = CURRENT_TIMESTAMP WHERE resource_key = ?", model.GlobalAnalysisResourceKey)
	require.NoError(t, err)

	type publishResult struct {
		owned bool
		err   error
	}
	done := make(chan publishResult, 1)
	go func() {
		owned, publishErr := repo.UpdateAnalysisWithLease(ctx, model.AnalysisOwnerTypeBatch, "owner-a", id, analysisLeaseUpdates(), "family,trip", true)
		done <- publishResult{owned: owned, err: publishErr}
	}()
	select {
	case result := <-done:
		t.Fatalf("publish finished while the writer lock was held: owned=%v err=%v", result.owned, result.err)
	case <-time.After(200 * time.Millisecond):
	}

	_, err = conn.ExecContext(ctx, "UPDATE analysis_runtime_leases SET owner_type = ?, owner_id = ? WHERE resource_key = ?", model.AnalysisOwnerTypeAnalyzer, "replacement", model.GlobalAnalysisResourceKey)
	require.NoError(t, err)
	_, err = conn.ExecContext(ctx, "COMMIT")
	require.NoError(t, err)

	select {
	case result := <-done:
		require.NoError(t, result.err)
		assert.False(t, result.owned)
	case <-time.After(5 * time.Second):
		t.Fatal("publish did not finish after the writer lock was released")
	}

	var photo model.Photo
	require.NoError(t, db.First(&photo, id).Error)
	assert.False(t, photo.AIAnalyzed)
	var tags int64
	require.NoError(t, db.Model(&model.PhotoTag{}).Where("photo_id = ?", id).Count(&tags).Error)
	assert.Zero(t, tags)
	var owner string
	require.NoError(t, db.Model(&model.AnalysisRuntimeLease{}).
		Where("resource_key = ?", model.GlobalAnalysisResourceKey).
		Pluck("owner_id", &owner).Error)
	assert.Equal(t, "replacement", owner)
}

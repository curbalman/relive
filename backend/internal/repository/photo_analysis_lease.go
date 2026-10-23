package repository

import (
	"context"
	"errors"
	"time"

	"github.com/davidhoo/relive/internal/model"
	"gorm.io/gorm"
)

// UpdateAnalysisWithLease fences publication against the persisted owner, even
// before the next heartbeat detects a takeover. The lease-row write takes
// SQLite's writer lock before ownership is observed, so a takeover cannot
// commit underneath the photo update. The clock is read after that wait rather
// than before the call, when it could already be stale.
// false, nil means the caller no longer owns an unexpired lease and nothing was
// written. A database error leaves the photo unchanged.
func (r *photoRepository) UpdateAnalysisWithLease(ctx context.Context, ownerType, ownerID string, id uint, fields map[string]interface{}, tags string, syncTags bool) (bool, error) {
	if ownerType == "" || ownerID == "" {
		return false, errors.New("analysis lease owner is required")
	}
	owned := false
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		lock := tx.Model(&model.AnalysisRuntimeLease{}).
			Where("resource_key = ?", model.GlobalAnalysisResourceKey).
			UpdateColumn("updated_at", time.Now().UTC())
		if lock.Error != nil {
			return lock.Error
		}
		var lease model.AnalysisRuntimeLease
		err := tx.Where("resource_key = ?", model.GlobalAnalysisResourceKey).First(&lease).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		now := time.Now().UTC()
		if lease.OwnerType != ownerType || lease.OwnerID != ownerID ||
			lease.Status != model.AnalysisRuntimeStatusRunning ||
			lease.LeaseExpiresAt == nil || lease.LeaseExpiresAt.Before(now) {
			return nil
		}
		if err := tx.Model(&model.Photo{}).Where("id = ?", id).Updates(fields).Error; err != nil {
			return err
		}
		if syncTags {
			if err := NewPhotoTagRepository(tx).SyncTagsTx(tx, id, tags); err != nil {
				return err
			}
		}
		owned = true
		return nil
	})
	if err != nil {
		return false, err
	}
	return owned, nil
}

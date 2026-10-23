package service

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/davidhoo/relive/internal/model"
	"github.com/davidhoo/relive/internal/repository"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

func TestGeocodeTaskService_SeedUsesSameGPSEligibility(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "geocode.db")), &gorm.Config{Logger: gormlogger.Discard})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB.Close() })
	require.NoError(t, db.AutoMigrate(&model.Photo{}, &model.GeocodeJob{}))
	photoRepo := repository.NewPhotoRepository(db)
	jobRepo := repository.NewGeocodeJobRepository(db)
	svc := &geocodeTaskService{db: db, photoRepo: photoRepo, jobRepo: jobRepo}
	zero, lat, lon := 0.0, 31.2, 121.5
	photos := []*model.Photo{
		{GPSLatitude: &zero, GPSLongitude: &lon, GeocodeStatus: model.GeocodeStatusPending},
		{GPSLatitude: &lat, GPSLongitude: &zero, GeocodeStatus: model.GeocodeStatusPending},
		{GPSLatitude: &zero, GPSLongitude: &zero, GeocodeStatus: model.GeocodeStatusPending},
		{GPSLatitude: &zero, GPSLongitude: &zero, GeocodeStatus: model.GeocodeStatusFailed},
		{GPSLatitude: nil, GPSLongitude: &lon, GeocodeStatus: model.GeocodeStatusPending},
		{GPSLatitude: &lat, GPSLongitude: &lon, GeocodeStatus: model.GeocodeStatusReady},
		{GPSLatitude: &lat, GPSLongitude: &lon, GeocodeStatus: model.GeocodeStatusPending, Status: model.PhotoStatusExcluded},
	}
	for i, p := range photos {
		p.FilePath = fmt.Sprintf("/photos/%d.jpg", i)
		p.FileName = fmt.Sprintf("%d.jpg", i)
		p.FileHash = fmt.Sprintf("hash-%d", i)
		require.NoError(t, photoRepo.Create(p))
	}

	// A direct enqueue request also leaves ineligible historical photos alone.
	for _, p := range photos[2:5] {
		require.NoError(t, svc.enqueuePhotoModel(p, model.GeocodeJobSourceManual, geocodePriorityManual, true))
	}
	require.NoError(t, svc.seedPendingJobs())
	require.NoError(t, svc.seedPendingJobs())

	var jobs []model.GeocodeJob
	require.NoError(t, db.Order("photo_id ASC").Find(&jobs).Error)
	require.Len(t, jobs, 2)
	assert.Equal(t, photos[0].ID, jobs[0].PhotoID)
	assert.Equal(t, photos[1].ID, jobs[1].PhotoID)
	stats, err := photoRepo.GetDerivedStatusByPathPrefix("/photos")
	require.NoError(t, err)
	assert.Equal(t, int64(len(jobs)), stats.GeocodePending)
	assert.Equal(t, int64(3), stats.GeocodeTotal)

	// No migration or read-time cleanup discards historical statuses.
	for _, original := range photos[2:5] {
		stored, err := photoRepo.GetByID(original.ID)
		require.NoError(t, err)
		assert.Equal(t, original.GeocodeStatus, stored.GeocodeStatus)
	}
}

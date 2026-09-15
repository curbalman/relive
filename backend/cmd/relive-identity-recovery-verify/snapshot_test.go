package main

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/davidhoo/relive/internal/model"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func setupTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	db, err := gorm.Open(sqlite.Open(dbPath), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	require.NoError(t, err)

	err = db.AutoMigrate(
		&model.Person{},
		&model.PersonIdentityProfile{},
		&model.PersonIdentityCenter{},
		&model.PersonMergeSuggestionFailure{},
		&model.PersonMergeSuggestion{},
	)
	require.NoError(t, err)
	return db
}

func TestSnapshotSamples_EligibleVsRawCenters(t *testing.T) {
	db := setupTestDB(t)

	// Person 1: visible, ready, gen 1
	p1 := model.Person{ID: 101, Name: "P1", Hidden: false}
	require.NoError(t, db.Create(&p1).Error)
	prof1 := model.PersonIdentityProfile{
		PersonID:         101,
		Status:           model.PersonIdentityProfileStatusReady,
		ActiveGeneration: 1,
		BuildOutcome:     "usable",
		EmbeddingModel:   "model-v1",
	}
	require.NoError(t, db.Create(&prof1).Error)

	// Centers for P1:
	// - c1: gen 1 -> eligible!
	// - c2: gen 0 -> ineligible (wrong gen)
	require.NoError(t, db.Create(&model.PersonIdentityCenter{PersonID: 101, Generation: 1, Ordinal: 0}).Error)
	require.NoError(t, db.Create(&model.PersonIdentityCenter{PersonID: 101, Generation: 0, Ordinal: 0}).Error)

	// Person 2: hidden person, ready, gen 1 -> ineligible because hidden
	p2 := model.Person{ID: 102, Name: "P2", Hidden: true}
	require.NoError(t, db.Create(&p2).Error)
	prof2 := model.PersonIdentityProfile{
		PersonID:         102,
		Status:           model.PersonIdentityProfileStatusReady,
		ActiveGeneration: 1,
		BuildOutcome:     "usable",
		EmbeddingModel:   "model-v1",
	}
	require.NoError(t, db.Create(&prof2).Error)
	require.NoError(t, db.Create(&model.PersonIdentityCenter{PersonID: 102, Generation: 1, Ordinal: 0}).Error)

	// Person 3: visible, failed status, gen 1 -> ineligible because failed
	p3 := model.Person{ID: 103, Name: "P3", Hidden: false}
	require.NoError(t, db.Create(&p3).Error)
	prof3 := model.PersonIdentityProfile{
		PersonID:         103,
		Status:           model.PersonIdentityProfileStatusFailed,
		ActiveGeneration: 1,
		BuildOutcome:     "no_usable_evidence",
		EmbeddingModel:   "model-v1",
	}
	require.NoError(t, db.Create(&prof3).Error)
	require.NoError(t, db.Create(&model.PersonIdentityCenter{PersonID: 103, Generation: 1, Ordinal: 0}).Error)

	// Person 4: visible, ready, gen 1, but different embedding model -> ineligible
	p4 := model.Person{ID: 104, Name: "P4", Hidden: false}
	require.NoError(t, db.Create(&p4).Error)
	prof4 := model.PersonIdentityProfile{
		PersonID:         104,
		Status:           model.PersonIdentityProfileStatusReady,
		ActiveGeneration: 1,
		BuildOutcome:     "usable",
		EmbeddingModel:   "model-different",
	}
	require.NoError(t, db.Create(&prof4).Error)
	require.NoError(t, db.Create(&model.PersonIdentityCenter{PersonID: 104, Generation: 1, Ordinal: 0}).Error)

	samples, err := snapshotSamples(db, []uint{101, 102, 103, 104}, "model-v1")
	require.NoError(t, err)
	require.Len(t, samples, 4)

	// Verify P1
	require.Equal(t, uint(101), samples[0].PersonID)
	require.True(t, samples[0].Present)
	require.Equal(t, 1, samples[0].RawCenterCount) // generation 1 has 1 center
	require.Equal(t, 1, samples[0].EligibleCenterCount) // eligible!
	require.False(t, samples[0].Hidden)

	// Verify P2
	require.Equal(t, uint(102), samples[1].PersonID)
	require.True(t, samples[1].Hidden)
	require.Equal(t, 1, samples[1].RawCenterCount)
	require.Equal(t, 0, samples[1].EligibleCenterCount) // hidden person center is not eligible

	// Verify P3
	require.Equal(t, uint(103), samples[2].PersonID)
	require.Equal(t, model.PersonIdentityProfileStatusFailed, samples[2].Status)
	require.Equal(t, 1, samples[2].RawCenterCount)
	require.Equal(t, 0, samples[2].EligibleCenterCount) // failed person center is not eligible

	// Verify P4
	require.Equal(t, uint(104), samples[3].PersonID)
	require.Equal(t, 1, samples[3].RawCenterCount)
	require.Equal(t, 0, samples[3].EligibleCenterCount) // model mismatch center is not eligible
}

func TestSnapshotSamples_QueryErrorPropagation(t *testing.T) {
	db := setupTestDB(t)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	require.NoError(t, sqlDB.Close())

	samples, err := snapshotSamples(db, []uint{101}, "model-v1")
	require.Error(t, err, "snapshotSamples must propagate DB errors")
	require.Nil(t, samples)
}

func TestSnapshotFailures_QueryErrorPropagation(t *testing.T) {
	db := setupTestDB(t)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	require.NoError(t, sqlDB.Close())

	snap, err := snapshotFailures(db, []uint{101})
	require.Error(t, err, "snapshotFailures must propagate DB errors")
	require.Nil(t, snap)
}

func TestSnapshotFailures_CaptureFullFields(t *testing.T) {
	db := setupTestDB(t)

	now := time.Now().UTC()
	next := now.Add(time.Hour)
	resolvedAt := now.Add(-5 * time.Minute)
	failure := model.PersonMergeSuggestionFailure{
		TargetPersonID:     201,
		State:              model.PersonMergeFailureStateResolved,
		ReasonCode:         "no_candidate",
		Attempts:           2,
		NextRetryAt:        &next,
		IndexReadyRequired: true,
		ResolvedReason:     model.PersonMergeFailureResolvedCompleteSuccess,
		ResolvedAt:         &resolvedAt,
		UpdatedAt:          now,
		LastAttemptKey:     "dg-1-seq-2",
	}
	require.NoError(t, db.Create(&failure).Error)

	snap, err := snapshotFailures(db, []uint{201, 202})
	require.NoError(t, err)
	require.NotNil(t, snap)
	require.Len(t, snap.SampleRows, 2)

	row1 := snap.SampleRows[0]
	require.Equal(t, uint(201), row1.TargetPersonID)
	require.True(t, row1.Present)
	require.Equal(t, model.PersonMergeFailureStateResolved, row1.State)
	require.Equal(t, model.PersonMergeFailureResolvedCompleteSuccess, row1.ResolvedReason)
	require.NotNil(t, row1.ResolvedAt)
	require.Equal(t, "dg-1-seq-2", row1.LastAttemptKey)

	row2 := snap.SampleRows[1]
	require.Equal(t, uint(202), row2.TargetPersonID)
	require.False(t, row2.Present)
}

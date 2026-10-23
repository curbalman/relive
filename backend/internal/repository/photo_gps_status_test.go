package repository

import (
	"fmt"
	"testing"

	"github.com/davidhoo/relive/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPhotoRepository_GPSEligibilityAndStatusBuckets(t *testing.T) {
	db := setupTestDB(t)
	defer teardownTestDB(db)
	repo := NewPhotoRepository(db)
	zero, lat, lon := 0.0, 31.2, 121.5
	cases := []struct {
		name     string
		lat, lon *float64
		status   string
		location string
		excluded bool
	}{
		{"pending", &lat, &lon, model.GeocodeStatusPending, "", false},
		{"equator", &zero, &lon, model.GeocodeStatusPending, "", false},
		{"meridian", &lat, &zero, model.GeocodeStatusPending, "", false},
		{"ready", &lat, &lon, model.GeocodeStatusReady, "", false},
		{"legacy_location", &lat, &lon, model.GeocodeStatusNone, "上海", false},
		{"failed", &lat, &lon, model.GeocodeStatusFailed, "", false},
		{"failed_with_location", &lat, &lon, model.GeocodeStatusFailed, "上海", false},
		{"null_status", &lat, &lon, model.GeocodeStatusNone, " ", false},
		{"placeholder_pending", &zero, &zero, model.GeocodeStatusPending, "", false},
		{"placeholder_failed", &zero, &zero, model.GeocodeStatusFailed, "", false},
		{"missing_latitude", nil, &lon, model.GeocodeStatusPending, "", false},
		{"missing_longitude", &lat, nil, model.GeocodeStatusFailed, "", false},
		{"missing_both", nil, nil, model.GeocodeStatusPending, "", false},
		{"excluded", &lat, &lon, model.GeocodeStatusPending, "", true},
	}
	var eligibleIDs, ineligibleIDs []uint
	for _, tc := range cases {
		p := &model.Photo{FilePath: fmt.Sprintf("/photos/trip/%s.jpg", tc.name), FileName: tc.name + ".jpg", FileHash: tc.name,
			GPSLatitude: tc.lat, GPSLongitude: tc.lon, GeocodeStatus: tc.status, Location: tc.location}
		if tc.excluded {
			p.Status = model.PhotoStatusExcluded
		}
		require.NoError(t, repo.Create(p))
		if tc.name == "null_status" {
			require.NoError(t, db.Model(p).Update("geocode_status", nil).Error)
		}
		if !tc.excluded {
			if p.HasGPS() {
				eligibleIDs = append(eligibleIDs, p.ID)
			} else {
				ineligibleIDs = append(ineligibleIDs, p.ID)
			}
		}
	}

	single, err := repo.GetDerivedStatusByPathPrefix("/photos/trip")
	require.NoError(t, err)
	batch, err := repo.GetDerivedStatusByPathPrefixes([]string{"/photos/trip", "/empty"})
	require.NoError(t, err)
	assert.Equal(t, single, batch["/photos/trip"])
	assert.Zero(t, batch["/empty"].GeocodeTotal)
	assert.Equal(t, int64(13), single.PhotoTotal)
	assert.Equal(t, int64(8), single.GeocodeTotal)
	assert.Equal(t, int64(3), single.GeocodeReady)
	assert.Equal(t, int64(1), single.GeocodeFailed)
	assert.Equal(t, int64(4), single.GeocodePending)
	assert.Equal(t, single.GeocodeTotal, single.GeocodeReady+single.GeocodeFailed+single.GeocodePending)

	for _, hasGPS := range []bool{true, false} {
		wantIDs := eligibleIDs
		if !hasGPS {
			wantIDs = ineligibleIDs
		}
		photos, total, err := repo.List(1, 100, nil, nil, &hasGPS, "", "", "", "", "id", false, nil, "")
		require.NoError(t, err)
		var gotIDs []uint
		for _, p := range photos {
			gotIDs = append(gotIDs, p.ID)
		}
		assert.ElementsMatch(t, wantIDs, gotIDs)
		assert.Equal(t, int64(len(wantIDs)), total)
		count, err := repo.CountWithFilters(nil, nil, &hasGPS, "", "", "", "", nil, "")
		require.NoError(t, err)
		assert.Equal(t, total, count)
	}
	photos, err := repo.ListWithGPS()
	require.NoError(t, err)
	var gotIDs []uint
	for _, p := range photos {
		gotIDs = append(gotIDs, p.ID)
	}
	assert.ElementsMatch(t, eligibleIDs, gotIDs)
}

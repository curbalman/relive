package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/davidhoo/relive/internal/model"
	"github.com/davidhoo/relive/internal/service"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func setupFullTestDB(t *testing.T) (string, *gorm.DB) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "verify_test.db")
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
		&model.PersonMergeSuggestionItem{},
		&model.Face{},
		&model.Photo{},
		&model.PeopleJob{},
		&model.AppConfig{},
	)
	require.NoError(t, err)
	return dbPath, db
}

func populatePassingFixture(t *testing.T, db *gorm.DB, modelSig string) {
	t.Helper()
	for _, id := range samplePersonIDs {
		require.NoError(t, db.Create(&model.Person{
			ID:        id,
			Name:      fmt.Sprintf("Person-%d", id),
			Hidden:    false,
			FaceCount: 5,
		}).Error)
	}

	for _, id := range []uint{264975, 356935} {
		require.NoError(t, db.Create(&model.PersonIdentityProfile{
			PersonID:         id,
			Status:           model.PersonIdentityProfileStatusReady,
			ActiveGeneration: 1,
			BuildOutcome:     "usable",
			EmbeddingModel:   modelSig,
		}).Error)
		require.NoError(t, db.Create(&model.PersonIdentityCenter{
			PersonID:          id,
			Generation:        1,
			Ordinal:           0,
			CentroidEmbedding: model.EncodeEmbedding([]float32{1, 0, 0}),
		}).Error)
	}

	for _, id := range []uint{306601, 307945, 562545} {
		require.NoError(t, db.Create(&model.PersonIdentityProfile{
			PersonID:         id,
			Status:           model.PersonIdentityProfileStatusReady,
			ActiveGeneration: 1,
			BuildOutcome:     "no_usable_evidence",
			EmbeddingModel:   modelSig,
		}).Error)
	}

	now := time.Now().UTC()
	for _, id := range []uint{264975, 356935} {
		require.NoError(t, db.Create(&model.PersonMergeSuggestionFailure{
			TargetPersonID: id,
			State:          model.PersonMergeFailureStateResolved,
			ResolvedReason: "evidence_ready",
			ResolvedAt:     &now,
			ReasonCode:     "profile_unavailable",
			UpdatedAt:      now,
		}).Error)
	}
	for _, id := range []uint{306601, 307945, 562545} {
		require.NoError(t, db.Create(&model.PersonMergeSuggestionFailure{
			TargetPersonID: id,
			State:          model.PersonMergeFailureStateWaitingEvidence,
			ReasonCode:     "no_usable_profile",
			UpdatedAt:      now,
		}).Error)
	}
}

func TestRun_InvalidArgs(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantExit   int
		wantErrSub string
	}{
		{
			name:       "missing db",
			args:       []string{},
			wantExit:   2,
			wantErrSub: "error: -db is required",
		},
		{
			name:       "negative max-slices",
			args:       []string{"-db", "foo.db", "-max-slices", "-1"},
			wantExit:   2,
			wantErrSub: "error: -max-slices and -merge-slices must be >= 0",
		},
		{
			name:       "negative merge-slices",
			args:       []string{"-db", "foo.db", "-merge-slices", "-1"},
			wantExit:   2,
			wantErrSub: "error: -max-slices and -merge-slices must be >= 0",
		},
		{
			name:       "unknown flag",
			args:       []string{"-unknown-flag"},
			wantExit:   2,
			wantErrSub: "flag provided but not defined",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run(tc.args, &stdout, &stderr)
			require.Equal(t, tc.wantExit, code)
			require.Contains(t, stderr.String(), tc.wantErrSub)
		})
	}
}

func TestRun_MissingDBFile(t *testing.T) {
	var stdout, stderr bytes.Buffer
	nonexistent := filepath.Join(t.TempDir(), "does_not_exist.db")
	code := run([]string{"-db", nonexistent}, &stdout, &stderr)
	require.Equal(t, 1, code)
	require.Contains(t, stderr.String(), "error: open db")
}

func TestRun_NoEmbeddingModel(t *testing.T) {
	dbPath, _ := setupFullTestDB(t)
	var stdout, stderr bytes.Buffer
	code := run([]string{"-db", dbPath}, &stdout, &stderr)
	require.Equal(t, 1, code)
	require.Contains(t, stderr.String(), "error: infer embedding model")
}

func TestRun_Fixture_WithoutTestEvidence_Returns1(t *testing.T) {
	modelSig := "test-emb-model"
	dbPath, db := setupFullTestDB(t)
	populatePassingFixture(t, db, modelSig)

	var stdout, stderr bytes.Buffer
	code := run([]string{
		"-db", dbPath,
		"-embedding-model", modelSig,
		"-max-slices", "0",
		"-merge-slices", "0",
	}, &stdout, &stderr)

	require.Equal(t, 1, code)
	var rep verifyReport
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &rep))
	require.False(t, rep.OK)
	require.NotNil(t, rep.TestEvidence)
	require.Equal(t, assertBlocked, rep.TestEvidence.Status)
}

func TestWriteReportAndExitCode(t *testing.T) {
	t.Run("passing report returns 0", func(t *testing.T) {
		r := &verifyReport{
			OK:         true,
			Assertions: []assertionResult{{Name: "baseline", Status: assertPass}},
		}
		var stdout, stderr bytes.Buffer
		code := writeReportAndExitCode(r, &stdout, &stderr, "")
		require.Equal(t, 0, code)

		var parsed verifyReport
		require.NoError(t, json.Unmarshal(stdout.Bytes(), &parsed))
		require.True(t, parsed.OK)
	})

	t.Run("failing report returns 1", func(t *testing.T) {
		r := &verifyReport{
			OK:         false,
			Assertions: []assertionResult{{Name: "fault_recovery", Status: assertFail}},
		}
		var stdout, stderr bytes.Buffer
		code := writeReportAndExitCode(r, &stdout, &stderr, "")
		require.Equal(t, 1, code)

		var parsed verifyReport
		require.NoError(t, json.Unmarshal(stdout.Bytes(), &parsed))
		require.False(t, parsed.OK)
	})

	t.Run("valid out file is written", func(t *testing.T) {
		outPath := filepath.Join(t.TempDir(), "report.json")
		r := &verifyReport{
			OK:         true,
			Assertions: []assertionResult{{Name: "baseline", Status: assertPass}},
		}
		var stdout, stderr bytes.Buffer
		code := writeReportAndExitCode(r, &stdout, &stderr, outPath)
		require.Equal(t, 0, code)

		content, err := os.ReadFile(outPath)
		require.NoError(t, err)
		var parsed verifyReport
		require.NoError(t, json.Unmarshal(content, &parsed))
		require.True(t, parsed.OK)
	})

	t.Run("invalid out path returns 1", func(t *testing.T) {
		invalidOut := filepath.Join(t.TempDir(), "nonexistent_dir", "sub", "report.json")
		r := &verifyReport{
			OK:         true,
			Assertions: []assertionResult{{Name: "baseline", Status: assertPass}},
		}
		var stdout, stderr bytes.Buffer
		code := writeReportAndExitCode(r, &stdout, &stderr, invalidOut)
		require.Equal(t, 1, code)
		require.Contains(t, stderr.String(), "error: write -out")
	})
}

// stubbedCollectors replaces the three slow collector variables with instant stubs that
// return a fixture consistent with a successful run. Returns a cleanup func.
// Only the collectors are replaced; the real loader, assertion engine, and JSON writer run unchanged.
func stubbedCollectors(t *testing.T, _ string) func() {
	t.Helper()

	passingSnap := &failureSnapshot{
		Counts: model.PersonMergeFailureCounts{
			UnresolvedTargets: 3,
			WaitingEvidence:   3,
		},
		StateCounts: map[string]int64{
			model.PersonMergeFailureStateWaitingEvidence: 3,
		},
		SampleRows: []failureRow{
			{TargetPersonID: 264975, Present: false},
			{TargetPersonID: 356935, Present: false},
			{TargetPersonID: 306601, Present: true, State: model.PersonMergeFailureStateWaitingEvidence, ReasonCode: "no_usable_evidence"},
			{TargetPersonID: 307945, Present: true, State: model.PersonMergeFailureStateWaitingEvidence, ReasonCode: "no_usable_evidence"},
			{TargetPersonID: 562545, Present: true, State: model.PersonMergeFailureStateWaitingEvidence, ReasonCode: "no_usable_evidence"},
		},
		TaskDTO:  &model.PersonMergeSuggestionTask{Status: "idle", Partial: true, WaitingEvidenceCount: 3, UnresolvedTargetCount: 3},
		StatsDTO: &model.PersonMergeSuggestionStatsResponse{Pending: 0},
	}
	passingSamples := []personSample{
		{PersonID: 264975, Present: true, Status: model.PersonIdentityProfileStatusReady, BuildOutcome: "usable", CenterCount: 38, EligibleCenterCount: 38},
		{PersonID: 356935, Present: true, Status: model.PersonIdentityProfileStatusReady, BuildOutcome: "usable", CenterCount: 5, EligibleCenterCount: 5},
		{PersonID: 306601, Present: true, Status: model.PersonIdentityProfileStatusReady, BuildOutcome: model.PersonIdentityBuildOutcomeNoUsableEvidence},
		{PersonID: 307945, Present: true, Status: model.PersonIdentityProfileStatusReady, BuildOutcome: model.PersonIdentityBuildOutcomeNoUsableEvidence},
		{PersonID: 562545, Present: true, Status: model.PersonIdentityProfileStatusReady, BuildOutcome: model.PersonIdentityBuildOutcomeNoUsableEvidence},
	}

	emptySnap := &failureSnapshot{
		StateCounts: map[string]int64{},
		SampleRows: []failureRow{
			{TargetPersonID: 264975, Present: false},
			{TargetPersonID: 356935, Present: false},
			{TargetPersonID: 306601, Present: false},
			{TargetPersonID: 307945, Present: false},
			{TargetPersonID: 562545, Present: false},
		},
		TaskDTO:  &model.PersonMergeSuggestionTask{Status: "idle"},
		StatsDTO: &model.PersonMergeSuggestionStatsResponse{Pending: 0},
	}

	calls := 0
	origProfile := collectorRunProfileSlices
	origMerge := collectorRunMergeSlices
	origSnap := collectorCaptureSnapshot

	collectorRunProfileSlices = func(_ service.PersonIdentityProfileService, _ *gorm.DB, _ int) (*sliceRunResult, error) {
		return &sliceRunResult{SlicesRun: 5, StoppedIdle: true}, nil
	}
	collectorRunMergeSlices = func(_ service.PersonMergeSuggestionService, _ *gorm.DB, _ int) (*sliceRunResult, error) {
		return &sliceRunResult{SlicesRun: 3, StoppedIdle: true}, nil
	}
	collectorCaptureSnapshot = func(_ *gorm.DB, _ service.PersonMergeSuggestionService, _ []uint, _ string) (*failureSnapshot, []personSample, int64, error) {
		calls++
		if calls == 1 {
			return emptySnap, passingSamples, 0, nil // before snapshot
		}
		return passingSnap, passingSamples, 0, nil // after snapshot
	}

	return func() {
		collectorRunProfileSlices = origProfile
		collectorRunMergeSlices = origMerge
		collectorCaptureSnapshot = origSnap
	}
}

// buildValidEvidenceFiles creates a minimal test-evidence.json + test-output.jsonl pair
// in the given dir and returns the manifest path. The source digest is taken from
// the current buildSourceDigest package var.
func buildValidEvidenceFiles(t *testing.T, dir string) string {
	t.Helper()
	var b []byte
	now := time.Now().UTC()
	for _, name := range RequiredFormalTests {
		for _, action := range []string{"run", "pass"} {
			ev, _ := json.Marshal(goTestEvent{
				Package: requiredTestPackage,
				Test:    name,
				Action:  action,
				Time:    now,
			})
			b = append(b, ev...)
			b = append(b, '\n')
		}
	}
	// Package-level pass event is required for completion tracking
	pkgEv, _ := json.Marshal(goTestEvent{Package: requiredTestPackage, Action: "pass", Time: now})
	b = append(b, pkgEv...)
	b = append(b, '\n')

	rawPath := filepath.Join(dir, "test-output.jsonl")
	require.NoError(t, os.WriteFile(rawPath, b, 0o600))
	rawHash := sha256.Sum256(b)
	rawSHA := hex.EncodeToString(rawHash[:])

	m := testEvidenceManifest{
		Command:         "go test -json ./internal/service -run 'TestThirdReview_'",
		StartedAt:       now.Add(-5 * time.Second),
		EndedAt:         now,
		ExitCode:        0,
		HeadCommit:      "fec4f46",
		SourceDigest:    buildSourceDigest,
		RawOutputPath:   "test-output.jsonl",
		RawOutputSHA256: rawSHA,
		RequiredTests:   append([]string(nil), RequiredFormalTests...),
		PassedTests:     append([]string(nil), RequiredFormalTests...),
	}
	mb, err := json.Marshal(m)
	require.NoError(t, err)
	manifestPath := filepath.Join(dir, "test-evidence.json")
	require.NoError(t, os.WriteFile(manifestPath, mb, 0o600))
	return manifestPath
}

// TestRun_FullCLI_Success exercises the full loader → assertions → JSON writer → exit code
// chain against a SQLite fixture DB. Slow collectors are stubbed for speed; ANN rebuild still
// runs against fixture centers so startup_wiring is real. Digest is injected in-process so the
// success path executes under plain `go test` (ldflags also work for formal evidence builds).
func TestRun_FullCLI_Success(t *testing.T) {
	digest := strings.Repeat("c", 64)
	oldDigest := buildSourceDigest
	buildSourceDigest = digest
	t.Cleanup(func() { buildSourceDigest = oldDigest })

	modelSig := "test-emb-model"
	dbPath, db := setupFullTestDB(t)
	populatePassingFixture(t, db, modelSig)

	cleanup := stubbedCollectors(t, modelSig)
	defer cleanup()

	dir := t.TempDir()
	manifestPath := buildValidEvidenceFiles(t, dir)

	var stdout, stderr bytes.Buffer
	code := run([]string{
		"-db", dbPath,
		"-embedding-model", modelSig,
		"-max-slices", "0",
		"-merge-slices", "0",
		"-test-evidence", manifestPath,
	}, &stdout, &stderr)

	var rep verifyReport
	if err := json.Unmarshal(stdout.Bytes(), &rep); err != nil {
		t.Logf("stdout: %s", stdout.String())
		t.Logf("stderr: %s", stderr.String())
		t.Fatalf("unmarshal report: %v", err)
	}
	require.Equal(t, 0, code, "expected exit 0 on full success path; stderr: %s", stderr.String())
	require.True(t, rep.OK, "report.OK must be true; assertions: %+v", rep.Assertions)
	require.NotNil(t, rep.TestEvidence)
	require.Equal(t, assertPass, rep.TestEvidence.Status)
	require.True(t, rep.ANNReadyAtAttach, "ANN must be ready after RebuildANNForVerify on fixture centers")
}

// TestRun_FullCLI_BadEvidence verifies that corrupted evidence → blocked assertion → exit 1.
// The full assertion chain is real; only the collectors are stubbed.
func TestRun_FullCLI_BadEvidence(t *testing.T) {
	modelSig := "test-emb-model"
	dbPath, db := setupFullTestDB(t)
	populatePassingFixture(t, db, modelSig)

	cleanup := stubbedCollectors(t, modelSig)
	defer cleanup()

	dir := t.TempDir()
	// Write a manifest whose raw_output_sha256 deliberately does not match the file
	badManifest := filepath.Join(dir, "bad-evidence.json")
	rawPath := filepath.Join(dir, "raw.jsonl")
	require.NoError(t, os.WriteFile(rawPath, []byte(`{}`+"\n"), 0o600))
	m := testEvidenceManifest{
		Command:         "go test -json ...",
		StartedAt:       time.Now().UTC().Add(-5 * time.Second),
		EndedAt:         time.Now().UTC(),
		ExitCode:        0,
		RawOutputPath:   rawPath,
		RawOutputSHA256: "0000000000000000000000000000000000000000000000000000000000000000",
		SourceDigest:    strings.Repeat("a", 64),
	}
	mb, err := json.Marshal(m)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(badManifest, mb, 0o600))

	var stdout, stderr bytes.Buffer
	code := run([]string{
		"-db", dbPath,
		"-embedding-model", modelSig,
		"-max-slices", "0",
		"-merge-slices", "0",
		"-test-evidence", badManifest,
	}, &stdout, &stderr)

	require.Equal(t, 1, code, "bad evidence must produce exit 1")
	var rep verifyReport
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &rep))
	require.False(t, rep.OK)
	require.NotNil(t, rep.TestEvidence)
	require.Equal(t, assertBlocked, rep.TestEvidence.Status)
}

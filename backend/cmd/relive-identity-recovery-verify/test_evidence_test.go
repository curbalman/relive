package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func createValidRawAndManifest(t *testing.T, sourceDigest string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	rawPath := filepath.Join(dir, "test-output.jsonl")
	now := time.Now().UTC()

	var b []byte
	for _, name := range RequiredFormalTests {
		for _, action := range []string{"run", "pass"} {
			event, _ := json.Marshal(goTestEvent{
				Package: requiredTestPackage,
				Test:    name,
				Action:  action,
				Time:    now,
			})
			b = append(b, event...)
			b = append(b, '\n')
		}
	}
	pkgEvent, _ := json.Marshal(goTestEvent{
		Package: requiredTestPackage,
		Action:  "pass",
		Time:    now,
	})
	b = append(b, pkgEvent...)
	b = append(b, '\n')

	require.NoError(t, os.WriteFile(rawPath, b, 0o600))
	h := sha256.Sum256(b)
	rawSHA := hex.EncodeToString(h[:])

	manifestPath := filepath.Join(dir, "test-evidence.json")
	m := testEvidenceManifest{
		Command:         "go test -json ...",
		StartedAt:       now.Add(-10 * time.Second),
		EndedAt:         now,
		ExitCode:        0,
		HeadCommit:      "fec4f46",
		SourceDigest:    sourceDigest,
		RawOutputPath:   "test-output.jsonl",
		RawOutputSHA256: rawSHA,
		RequiredTests:   append([]string(nil), RequiredFormalTests...),
	}
	mBytes, err := json.Marshal(m)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(manifestPath, mBytes, 0o600))

	return manifestPath, rawPath
}

func TestLoadAndVerifyTestEvidence_Positive(t *testing.T) {
	digest := strings.Repeat("e", 64)
	manifestPath, _ := createValidRawAndManifest(t, digest)

	report, err := loadAndVerifyTestEvidence(manifestPath, digest)
	require.NoError(t, err)
	require.NotNil(t, report)
	require.Equal(t, assertPass, report.Status)
	require.True(t, report.DigestMatch)
	require.Equal(t, 0, report.ExitCode)
	require.Equal(t, len(RequiredFormalTests), len(report.PassedTests))
	require.Empty(t, report.FailedTests)
	require.Empty(t, report.MissingTests)
	require.Empty(t, report.SkippedTests)
}

func TestLoadAndVerifyTestEvidence_ManifestErrors(t *testing.T) {
	dir := t.TempDir()

	// 1. Manifest file missing
	ev, err := loadAndVerifyTestEvidence(filepath.Join(dir, "absent.json"), "")
	require.Error(t, err)
	require.Nil(t, ev)

	// 2. Manifest invalid JSON
	badJSON := filepath.Join(dir, "bad.json")
	require.NoError(t, os.WriteFile(badJSON, []byte("{invalid json"), 0o600))
	ev, err = loadAndVerifyTestEvidence(badJSON, "")
	require.Error(t, err)
	require.Nil(t, ev)

	// 3. Command missing
	noCmd := filepath.Join(dir, "nocmd.json")
	mBytes, _ := json.Marshal(testEvidenceManifest{
		StartedAt:       time.Now(),
		EndedAt:         time.Now().Add(time.Second),
		RawOutputSHA256: strings.Repeat("0", 64),
	})
	require.NoError(t, os.WriteFile(noCmd, mBytes, 0o600))
	ev, err = loadAndVerifyTestEvidence(noCmd, "")
	require.Error(t, err)
	require.Contains(t, err.Error(), "manifest command missing")
	require.Nil(t, ev)

	// 4. Time window invalid
	badTime := filepath.Join(dir, "badtime.json")
	mBytes, _ = json.Marshal(testEvidenceManifest{
		Command:         "go test",
		StartedAt:       time.Now(),
		EndedAt:         time.Now().Add(-time.Second),
		RawOutputSHA256: strings.Repeat("0", 64),
	})
	require.NoError(t, os.WriteFile(badTime, mBytes, 0o600))
	ev, err = loadAndVerifyTestEvidence(badTime, "")
	require.Error(t, err)
	require.Contains(t, err.Error(), "execution window invalid")
	require.Nil(t, ev)

	// 5. RawOutputSHA256 missing
	noSHA := filepath.Join(dir, "nosha.json")
	mBytes, _ = json.Marshal(testEvidenceManifest{
		Command:   "go test",
		StartedAt: time.Now(),
		EndedAt:   time.Now().Add(time.Second),
	})
	require.NoError(t, os.WriteFile(noSHA, mBytes, 0o600))
	ev, err = loadAndVerifyTestEvidence(noSHA, "")
	require.Error(t, err)
	require.Contains(t, err.Error(), "raw_output_sha256 missing")
	require.Nil(t, ev)
}

func TestLoadAndVerifyTestEvidence_RawOutputErrors(t *testing.T) {
	dir := t.TempDir()
	rawPath := filepath.Join(dir, "raw.jsonl")
	now := time.Now().UTC()
	validJSONEvent, _ := json.Marshal(goTestEvent{Package: "test", Action: "pass", Time: now})
	require.NoError(t, os.WriteFile(rawPath, append(validJSONEvent, '\n'), 0o600))

	manifestPath := filepath.Join(dir, "manifest.json")
	writeManifest := func(sha string) {
		mBytes, _ := json.Marshal(testEvidenceManifest{
			Command:         "go test",
			StartedAt:       now.Add(-time.Second),
			EndedAt:         now,
			RawOutputPath:   rawPath,
			RawOutputSHA256: sha,
			ExitCode:        0,
		})
		_ = os.WriteFile(manifestPath, mBytes, 0o600)
	}

	// 1. SHA256 mismatch
	writeManifest(strings.Repeat("f", 64))
	ev, err := loadAndVerifyTestEvidence(manifestPath, "")
	require.Error(t, err)
	require.Contains(t, err.Error(), "raw output sha256 mismatch")
	require.Nil(t, ev)

	// 2. Raw file corrupted non-empty line
	corruptedRaw := filepath.Join(dir, "corrupted.jsonl")
	corruptContent := []byte("not-json-line\n")
	require.NoError(t, os.WriteFile(corruptedRaw, corruptContent, 0o600))
	h := sha256.Sum256(corruptContent)
	mBytes, _ := json.Marshal(testEvidenceManifest{
		Command:         "go test",
		StartedAt:       time.Now().Add(-time.Second),
		EndedAt:         time.Now(),
		RawOutputPath:   corruptedRaw,
		RawOutputSHA256: hex.EncodeToString(h[:]),
	})
	corruptManifest := filepath.Join(dir, "corrupt_manifest.json")
	require.NoError(t, os.WriteFile(corruptManifest, mBytes, 0o600))

	ev, err = loadAndVerifyTestEvidence(corruptManifest, "")
	require.Error(t, err)
	require.Contains(t, err.Error(), "parse raw test event")
	require.Nil(t, ev)
}

func TestLoadAndVerifyTestEvidence_TruncatedLog(t *testing.T) {
	dir := t.TempDir()
	rawPath := filepath.Join(dir, "raw.jsonl")
	now := time.Now().UTC()
	// All tests passed, but no package pass/fail event at end (truncated log)
	var b []byte
	for _, name := range RequiredFormalTests {
		for _, action := range []string{"run", "pass"} {
			event, _ := json.Marshal(goTestEvent{
				Package: requiredTestPackage,
				Test:    name,
				Action:  action,
				Time:    now,
			})
			b = append(b, event...)
			b = append(b, '\n')
		}
	}
	require.NoError(t, os.WriteFile(rawPath, b, 0o600))
	h := sha256.Sum256(b)

	manifestPath := filepath.Join(dir, "manifest.json")
	mBytes, _ := json.Marshal(testEvidenceManifest{
		Command:         "go test",
		StartedAt:       now.Add(-time.Second),
		EndedAt:         now,
		ExitCode:        0,
		RawOutputPath:   rawPath,
		RawOutputSHA256: hex.EncodeToString(h[:]),
		SourceDigest:    strings.Repeat("a", 64),
	})
	require.NoError(t, os.WriteFile(manifestPath, mBytes, 0o600))

	ev, err := loadAndVerifyTestEvidence(manifestPath, strings.Repeat("a", 64))
	require.Error(t, err)
	require.Contains(t, err.Error(), "truncated")
	require.Nil(t, ev)
}

func TestLoadAndVerifyTestEvidence_TestFailuresAndMasking(t *testing.T) {
	dir := t.TempDir()
	rawPath := filepath.Join(dir, "raw.jsonl")
	now := time.Now().UTC()

	var b []byte
	// 1. One required test fails
	for i, name := range RequiredFormalTests {
		for _, action := range []string{"run"} {
			event, _ := json.Marshal(goTestEvent{Package: requiredTestPackage, Test: name, Action: action, Time: now})
			b = append(b, event...)
			b = append(b, '\n')
		}
		if i == 0 {
			// First test fails, then falsely passes
			failEvent, _ := json.Marshal(goTestEvent{Package: requiredTestPackage, Test: name, Action: "fail", Time: now})
			b = append(b, failEvent...)
			b = append(b, '\n')
			passEvent, _ := json.Marshal(goTestEvent{Package: requiredTestPackage, Test: name, Action: "pass", Time: now})
			b = append(b, passEvent...)
			b = append(b, '\n')
		} else {
			passEvent, _ := json.Marshal(goTestEvent{Package: requiredTestPackage, Test: name, Action: "pass", Time: now})
			b = append(b, passEvent...)
			b = append(b, '\n')
		}
	}
	pkgEvent, _ := json.Marshal(goTestEvent{Package: requiredTestPackage, Action: "pass", Time: now})
	b = append(b, pkgEvent...)
	b = append(b, '\n')
	require.NoError(t, os.WriteFile(rawPath, b, 0o600))
	h := sha256.Sum256(b)

	digest := strings.Repeat("a", 64)
	manifestPath := filepath.Join(dir, "manifest.json")
	mBytes, _ := json.Marshal(testEvidenceManifest{
		Command:         "go test",
		StartedAt:       now.Add(-time.Second),
		EndedAt:         now,
		ExitCode:        0,
		RawOutputPath:   rawPath,
		RawOutputSHA256: hex.EncodeToString(h[:]),
		SourceDigest:    digest,
	})
	require.NoError(t, os.WriteFile(manifestPath, mBytes, 0o600))

	ev, err := loadAndVerifyTestEvidence(manifestPath, digest)
	require.NoError(t, err) // Completed parse without I/O error
	require.NotNil(t, ev)
	require.Equal(t, assertFail, ev.Status)
	require.Contains(t, ev.FailedTests, RequiredFormalTests[0], "failed event cannot be masked by subsequent pass")
}

func TestLoadAndVerifyTestEvidence_WrongPackageIgnored(t *testing.T) {
	dir := t.TempDir()
	rawPath := filepath.Join(dir, "raw.jsonl")
	now := time.Now().UTC()

	// Tests run in repository package instead of service package, with no repo package completion event.
	// After G1 fix: all packages seen in the raw log must have a completion event, so this should
	// return an error (truncated log) since repository has test events but no package-level pass/fail.
	var b []byte
	for _, name := range RequiredFormalTests {
		for _, action := range []string{"run", "pass"} {
			event, _ := json.Marshal(goTestEvent{
				Package: "github.com/davidhoo/relive/internal/repository",
				Test:    name,
				Action:  action,
				Time:    now,
			})
			b = append(b, event...)
			b = append(b, '\n')
		}
	}
	// Only service gets a package-level pass — repository is missing its completion event.
	pkgEvent, _ := json.Marshal(goTestEvent{Package: requiredTestPackage, Action: "pass", Time: now})
	b = append(b, pkgEvent...)
	b = append(b, '\n')
	require.NoError(t, os.WriteFile(rawPath, b, 0o600))
	h := sha256.Sum256(b)

	digest := strings.Repeat("a", 64)
	manifestPath := filepath.Join(dir, "manifest.json")
	mBytes, _ := json.Marshal(testEvidenceManifest{
		Command:         "go test",
		StartedAt:       now.Add(-time.Second),
		EndedAt:         now,
		ExitCode:        0,
		RawOutputPath:   rawPath,
		RawOutputSHA256: hex.EncodeToString(h[:]),
		SourceDigest:    digest,
	})
	require.NoError(t, os.WriteFile(manifestPath, mBytes, 0o600))

	// G1: loader must error because repository package has test events but no package completion.
	ev, err := loadAndVerifyTestEvidence(manifestPath, digest)
	require.Error(t, err, "repository package has test events but no completion event: must be treated as truncated")
	require.Contains(t, err.Error(), "truncated")
	require.Nil(t, ev, "Contract: loader returns (nil, error) on truncation")
}

// TestSeventhReview_MissingEventTimeMustNotPass: G1 — required execution/completion events
// without Time must be rejected; absence of a verifiable window is not a free pass.
func TestSeventhReview_MissingEventTimeMustNotPass(t *testing.T) {
	raw, _ := sixthRaw(t)
	var b []byte
	for _, line := range strings.Split(string(raw), "\n") {
		if line == "" {
			continue
		}
		var e map[string]interface{}
		require.NoError(t, json.Unmarshal([]byte(line), &e))
		delete(e, "Time")
		lineBytes, err := json.Marshal(e)
		require.NoError(t, err)
		b = append(b, lineBytes...)
		b = append(b, '\n')
	}
	h := sha256.Sum256(b)
	manifestPath := buildSixthManifest(t, b, hex.EncodeToString(h[:]), func(m map[string]interface{}) {
		m["started_at"] = "2020-01-01T00:00:00Z"
		m["ended_at"] = "2020-01-01T00:01:00Z"
	})

	ev, err := loadAndVerifyTestEvidence(manifestPath, strings.Repeat("d", 64))
	if err != nil {
		r := sixthReviewValidReport()
		r.TestEvidence = blockedEvidence(err)
		require.False(t, assertionsAllPass(evaluateAssertions(r)),
			"missing event timestamps must block overall pass")
		require.Contains(t, err.Error(), "no timestamp")
		return
	}
	r := sixthReviewValidReport()
	r.TestEvidence = ev
	require.False(t, assertionsAllPass(evaluateAssertions(r)),
		"missing event timestamps bypass execution-window validation")
}

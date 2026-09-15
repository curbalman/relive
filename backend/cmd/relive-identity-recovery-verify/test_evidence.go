package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// buildSourceDigest is populated at compile time via -ldflags "-X main.buildSourceDigest=${SOURCE_DIGEST}".
var buildSourceDigest string

// RequiredFormalTests lists the formal tests covering T1–T4 failure windows and concurrency.
var RequiredFormalTests = []string{
	"TestThirdReview_ManualCheckpointFailureCanResume",
	"TestThirdReview_ManualProgressFailureDoesNotRepeatDirty",
	"TestThirdReview_ManualProgressConcurrentSingleCommit",
	"TestThirdReview_AttemptKeySaveFailureMustStopLedgerWrite",
	"TestThirdReview_ProfileReadFailureKeepsReconcilePending",
}

const requiredTestPackage = "github.com/davidhoo/relive/internal/service"

type testEvidenceManifest struct {
	Command         string    `json:"command"`
	StartedAt       time.Time `json:"started_at"`
	EndedAt         time.Time `json:"ended_at"`
	ExitCode        int       `json:"exit_code"`
	HeadCommit      string    `json:"head_commit"`
	SourceDigest    string    `json:"source_digest"`
	RawOutputSHA256 string    `json:"raw_output_sha256"`
	RawOutputPath   string    `json:"raw_output_path,omitempty"`
	RequiredTests   []string  `json:"required_tests"`
	PassedTests     []string  `json:"passed_tests"`
	FailedTests     []string  `json:"failed_tests,omitempty"`
	SkippedTests    []string  `json:"skipped_tests,omitempty"`

	hasExitCode bool
}

func (m *testEvidenceManifest) UnmarshalJSON(data []byte) error {
	type Alias testEvidenceManifest
	aux := struct {
		ExitCode *int `json:"exit_code"`
		*Alias
	}{
		Alias: (*Alias)(m),
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	if aux.ExitCode != nil {
		m.hasExitCode = true
		m.ExitCode = *aux.ExitCode
	} else {
		m.hasExitCode = false
	}
	return nil
}

type testEvidenceReport struct {
	ManifestPath    string    `json:"manifest_path"`
	Command         string    `json:"command"`
	StartedAt       time.Time `json:"started_at"`
	EndedAt         time.Time `json:"ended_at"`
	ExitCode        int       `json:"exit_code"`
	HeadCommit      string    `json:"head_commit"`
	SourceDigest    string    `json:"source_digest"`
	RawOutputSHA256 string    `json:"raw_output_sha256"`
	RequiredTests   []string  `json:"required_tests"`
	PassedTests     []string  `json:"passed_tests"`
	FailedTests     []string  `json:"failed_tests,omitempty"`
	SkippedTests    []string  `json:"skipped_tests,omitempty"`
	MissingTests    []string  `json:"missing_tests,omitempty"`
	DigestMatch     bool      `json:"digest_match"`
	Status          string    `json:"status"` // pass | fail | blocked
	StatusReason    string    `json:"status_reason,omitempty"`
}

type goTestEvent struct {
	Time    time.Time `json:"Time"`
	Action  string    `json:"Action"`
	Package string    `json:"Package"`
	Test    string    `json:"Test"`
	Elapsed float64   `json:"Elapsed"`
	Output  string    `json:"Output"`
}

func blockedEvidence(err error) *testEvidenceReport {
	msg := "unknown error"
	if err != nil {
		msg = err.Error()
	}
	return &testEvidenceReport{
		RequiredTests: append([]string(nil), RequiredFormalTests...),
		DigestMatch:   false,
		Status:        assertBlocked,
		StatusReason:  msg,
	}
}

// loadAndVerifyTestEvidence reads manifest and corresponding raw output jsonl,
// validating the test execution against RequiredFormalTests.
// Contract: Returns (nil, error) on file, parse, integrity, or source validation failure.
func loadAndVerifyTestEvidence(manifestPath string, expectedBuildDigest string) (*testEvidenceReport, error) {
	manifestBytes, err := os.ReadFile(manifestPath)
	if err != nil {
		return nil, fmt.Errorf("read manifest: %w", err)
	}
	var m testEvidenceManifest
	if err := json.Unmarshal(manifestBytes, &m); err != nil {
		return nil, fmt.Errorf("parse manifest: %w", err)
	}

	// Validate manifest required fields
	if strings.TrimSpace(m.Command) == "" {
		return nil, errors.New("manifest command missing")
	}
	if m.StartedAt.IsZero() || m.EndedAt.IsZero() || m.EndedAt.Before(m.StartedAt) {
		return nil, errors.New("manifest execution window invalid")
	}
	if !m.hasExitCode {
		return nil, errors.New("manifest exit_code missing")
	}
	trimmedRawSHA := strings.TrimSpace(m.RawOutputSHA256)
	if trimmedRawSHA == "" {
		return nil, errors.New("manifest raw_output_sha256 missing")
	}
	if len(trimmedRawSHA) != 64 {
		return nil, fmt.Errorf("manifest raw_output_sha256 invalid format (length=%d)", len(trimmedRawSHA))
	}

	// Verify source digest
	targetBuildDigest := strings.TrimSpace(expectedBuildDigest)
	if targetBuildDigest == "" {
		targetBuildDigest = strings.TrimSpace(buildSourceDigest)
	}
	digestMatch := false
	if targetBuildDigest != "" && strings.TrimSpace(m.SourceDigest) == targetBuildDigest {
		digestMatch = true
	}

	// Resolve raw output path
	rawPath := m.RawOutputPath
	if rawPath == "" {
		rawPath = filepath.Join(filepath.Dir(manifestPath), "test-output.jsonl")
	} else if !filepath.IsAbs(rawPath) {
		rawPath = filepath.Join(filepath.Dir(manifestPath), rawPath)
	}

	rawFile, err := os.Open(rawPath)
	if err != nil {
		return nil, fmt.Errorf("open raw test output %s: %w", rawPath, err)
	}
	defer rawFile.Close()

	// Verify SHA256 of raw output while scanning
	rawHash := sha256.New()
	tee := io.TeeReader(rawFile, rawHash)
	scanner := bufio.NewScanner(tee)
	// Allow large lines (stack traces / json) up to 10MB
	scanner.Buffer(make([]byte, 64*1024), 10*1024*1024)

	type testTrack struct {
		hasRun      bool
		hasFail     bool
		finalAction string
	}
	testActions := make(map[string]*testTrack)
	packageCompletions := make(map[string]string)
	packagesSeen := make(map[string]bool)
	anyPackageFailed := false

	lineNum := 0
	for scanner.Scan() {
		lineNum++
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var ev goTestEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			return nil, fmt.Errorf("parse raw test event line %d: %w", lineNum, err)
		}

		// Verify event timestamp falls into declared execution window.
		// All real events (Package != "") must carry a timestamp so that window
		// membership can be confirmed. Accepting zero-Time silently would allow
		// fabricated events from an entirely different run to pass undetected.
		if ev.Package != "" {
			if ev.Time.IsZero() {
				return nil, fmt.Errorf("test event has no timestamp (package=%s test=%q action=%s): cannot verify it belongs to this execution window", ev.Package, ev.Test, ev.Action)
			}
			if ev.Time.Before(m.StartedAt.Add(-30*time.Second)) || ev.Time.After(m.EndedAt.Add(30*time.Second)) {
				return nil, fmt.Errorf("test event time %v is outside declared execution window [%v, %v]", ev.Time, m.StartedAt, m.EndedAt)
			}
		}

		if ev.Package != "" {
			packagesSeen[ev.Package] = true
		}

		// Check package level events
		if ev.Test == "" {
			if ev.Action == "pass" || ev.Action == "fail" {
				packageCompletions[ev.Package] = ev.Action
			}
			if ev.Action == "fail" {
				anyPackageFailed = true
			}
			continue
		}

		// Only match tests belonging to the required service package
		if ev.Package == requiredTestPackage {
			track, found := testActions[ev.Test]
			if !found {
				track = &testTrack{}
				testActions[ev.Test] = track
			}
			if ev.Action == "run" {
				track.hasRun = true
			} else if ev.Action == "fail" {
				track.hasFail = true
				track.finalAction = "fail"
			} else if ev.Action == "pass" {
				track.finalAction = "pass"
			} else if ev.Action == "skip" {
				track.finalAction = "skip"
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read raw test output: %w", err)
	}

	actualSHA := hex.EncodeToString(rawHash.Sum(nil))
	if actualSHA != trimmedRawSHA {
		return nil, fmt.Errorf("raw output sha256 mismatch: manifest=%s, actual=%s", trimmedRawSHA, actualSHA)
	}

	// Verify all expected packages completed with pass
	expectedPackages := make(map[string]bool)
	expectedPackages[requiredTestPackage] = true
	if strings.Contains(m.Command, "internal/repository") {
		expectedPackages["github.com/davidhoo/relive/internal/repository"] = true
	}
	for pkg := range packagesSeen {
		expectedPackages[pkg] = true
	}

	for pkg := range expectedPackages {
		action, completed := packageCompletions[pkg]
		if !completed {
			return nil, fmt.Errorf("raw test output truncated: package %s completion event missing", pkg)
		}
		if action != "pass" {
			anyPackageFailed = true
		}
	}

	// Check RequiredFormalTests
	passedTests := make([]string, 0)
	failedTests := make([]string, 0)
	skippedTests := make([]string, 0)
	missingTests := make([]string, 0)

	for _, reqTest := range RequiredFormalTests {
		track, found := testActions[reqTest]
		if !found || !track.hasRun {
			missingTests = append(missingTests, reqTest)
		} else if track.hasFail {
			// Once failed in this run, cannot be masked
			failedTests = append(failedTests, reqTest)
		} else {
			switch track.finalAction {
			case "pass":
				passedTests = append(passedTests, reqTest)
			case "fail":
				failedTests = append(failedTests, reqTest)
			case "skip":
				skippedTests = append(skippedTests, reqTest)
			default:
				missingTests = append(missingTests, reqTest)
			}
		}
	}

	if anyPackageFailed && len(failedTests) == 0 {
		failedTests = append(failedTests, "package_failure")
	}

	sort.Strings(passedTests)
	sort.Strings(failedTests)
	sort.Strings(skippedTests)
	sort.Strings(missingTests)

	report := &testEvidenceReport{
		ManifestPath:    manifestPath,
		Command:         m.Command,
		StartedAt:       m.StartedAt,
		EndedAt:         m.EndedAt,
		ExitCode:        m.ExitCode,
		HeadCommit:      m.HeadCommit,
		SourceDigest:    m.SourceDigest,
		RawOutputSHA256: m.RawOutputSHA256,
		RequiredTests:   append([]string(nil), RequiredFormalTests...),
		PassedTests:     passedTests,
		FailedTests:     failedTests,
		SkippedTests:    skippedTests,
		MissingTests:    missingTests,
		DigestMatch:     digestMatch,
	}

	if report.ExitCode != 0 || len(failedTests) > 0 {
		report.Status = assertFail
		report.StatusReason = fmt.Sprintf("exit_code=%d failed_tests=%v", report.ExitCode, failedTests)
	} else if len(missingTests) > 0 || len(skippedTests) > 0 || !digestMatch {
		report.Status = assertBlocked
		report.StatusReason = fmt.Sprintf("missing=%v skipped=%v digest_match=%v", missingTests, skippedTests, digestMatch)
	} else {
		report.Status = assertPass
		report.StatusReason = fmt.Sprintf("verified %d formal tests", len(passedTests))
	}

	return report, nil
}

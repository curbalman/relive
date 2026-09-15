package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/davidhoo/relive/internal/model"
	"github.com/stretchr/testify/require"
)

func validTestEvidenceForTest() *testEvidenceReport {
	return &testEvidenceReport{
		ManifestPath:    "test-evidence.json",
		Command:         "go test -json ...",
		StartedAt:       time.Now().UTC().Add(-10 * time.Second),
		EndedAt:         time.Now().UTC(),
		ExitCode:        0,
		HeadCommit:      "fec4f46",
		SourceDigest:    "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		RawOutputSHA256: "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210",
		RequiredTests:   append([]string(nil), RequiredFormalTests...),
		PassedTests:     append([]string(nil), RequiredFormalTests...),
		DigestMatch:     true,
	}
}

func validFiveSamplesFinalForTest() []personSample {
	return []personSample{
		{PersonID: 264975, Present: true, Status: model.PersonIdentityProfileStatusReady, BuildOutcome: "usable", CenterCount: 38, EligibleCenterCount: 38},
		{PersonID: 356935, Present: true, Status: model.PersonIdentityProfileStatusReady, BuildOutcome: "usable", CenterCount: 5, EligibleCenterCount: 5},
		{PersonID: 306601, Present: true, Status: model.PersonIdentityProfileStatusReady, BuildOutcome: model.PersonIdentityBuildOutcomeNoUsableEvidence, CenterCount: 0, EligibleCenterCount: 0},
		{PersonID: 307945, Present: true, Status: model.PersonIdentityProfileStatusReady, BuildOutcome: model.PersonIdentityBuildOutcomeNoUsableEvidence, CenterCount: 0, EligibleCenterCount: 0},
		{PersonID: 562545, Present: true, Status: model.PersonIdentityProfileStatusReady, BuildOutcome: model.PersonIdentityBuildOutcomeNoUsableEvidence, CenterCount: 0, EligibleCenterCount: 0},
	}
}

func validFailuresBeforeForTest() *failureSnapshot {
	return &failureSnapshot{
		StateCounts: map[string]int64{},
		SampleRows: []failureRow{
			{TargetPersonID: 264975, Present: false},
			{TargetPersonID: 356935, Present: false},
			{TargetPersonID: 306601, Present: false},
			{TargetPersonID: 307945, Present: false},
			{TargetPersonID: 562545, Present: false},
		},
	}
}

func validFailureSnapshotForTest() *failureSnapshot {
	return &failureSnapshot{
		Counts: model.PersonMergeFailureCounts{
			UnresolvedTargets: 3,
			WaitingEvidence:   3,
			RetryTotal:        0,
			RetryDue:          0,
			RetryDeferred:     0,
			RetryExhausted:    0,
		},
		StateCounts: map[string]int64{
			model.PersonMergeFailureStateWaitingEvidence: 3,
		},
		SampleRows: []failureRow{
			{TargetPersonID: 264975, Present: false},
			{TargetPersonID: 356935, Present: false},
			{TargetPersonID: 306601, Present: true, State: model.PersonMergeFailureStateWaitingEvidence, ReasonCode: "no_usable_evidence", Attempts: 0},
			{TargetPersonID: 307945, Present: true, State: model.PersonMergeFailureStateWaitingEvidence, ReasonCode: "no_usable_evidence", Attempts: 0},
			{TargetPersonID: 562545, Present: true, State: model.PersonMergeFailureStateWaitingEvidence, ReasonCode: "no_usable_evidence", Attempts: 0},
		},
	}
}

func TestEvaluateAssertions_EligibilityAloneDoesNotPass(t *testing.T) {
	r := &verifyReport{
		CollectedAt: time.Now().UTC(),
		Eligibility: &eligibilityResult{FailedExcludedOK: true, ActiveCenterCount: 10, EligiblePersonCount: 8},
		// 其余必需环节缺失 → 整体不得 OK
	}
	asserts := evaluateAssertions(r)
	require.False(t, assertionsAllPass(asserts))
	var elig, recovery assertionResult
	for _, a := range asserts {
		if a.Name == "eligibility" {
			elig = a
		}
		if a.Name == "recovery" {
			recovery = a
		}
	}
	require.Equal(t, assertPass, elig.Status)
	require.NotEqual(t, assertPass, recovery.Status)
}

func TestEvaluateAssertions_RecoveryMissingFailsOverall(t *testing.T) {
	r := &verifyReport{
		Baseline: &baselineSnapshot{StatusCounts: map[string]int64{"failed": 24}},
		Eligibility: &eligibilityResult{
			FailedExcludedOK: true, ActiveCenterCount: 10, EligiblePersonCount: 8,
		},
		ProfileRecovery:        &sliceRunResult{SlicesRun: 0, StoppedIdle: false},
		ANNReadyAtAttach:       true,
		PostRecoveryANNCenters: 10,
		MergeSuggestion:        &sliceRunResult{SlicesRun: 1, StoppedIdle: true},
		Failures: &failureSnapshot{
			Counts:      model.PersonMergeFailureCounts{},
			StateCounts: map[string]int64{},
		},
	}
	asserts := evaluateAssertions(r)
	require.False(t, assertionsAllPass(asserts))
	for _, a := range asserts {
		if a.Name == "recovery" {
			require.Equal(t, assertFail, a.Status)
		}
	}
}

func TestEvaluateAssertions_WaitingEvidenceZeroPendingPasses(t *testing.T) {
	r := &verifyReport{
		Baseline: &baselineSnapshot{StatusCounts: map[string]int64{"failed": 0, "dirty": 0, "ready": 100}},
		Eligibility: &eligibilityResult{
			FailedExcludedOK: true, ActiveCenterCount: 50, EligiblePersonCount: 40,
		},
		ProfileRecovery:        &sliceRunResult{SlicesRun: 5, StoppedIdle: true},
		SampleEmptyBuilt:       3,
		ANNReadyAtAttach:       true,
		PostRecoveryANNCenters: 50,
		MergeSuggestion:        &sliceRunResult{SlicesRun: 3, StoppedIdle: true},
		SampleBefore: []personSample{
			{PersonID: 264975, Present: true, Status: "ready", CenterCount: 38},
			{PersonID: 356935, Present: true, Status: "ready", CenterCount: 5},
			{PersonID: 306601, Present: true, Status: "ready", BuildOutcome: model.PersonIdentityBuildOutcomeNoUsableEvidence},
			{PersonID: 307945, Present: true, Status: "ready", BuildOutcome: model.PersonIdentityBuildOutcomeNoUsableEvidence},
			{PersonID: 562545, Present: true, Status: "ready", BuildOutcome: model.PersonIdentityBuildOutcomeNoUsableEvidence},
		},
		SampleFinal:            validFiveSamplesFinalForTest(),
		FailuresBefore:         validFailuresBeforeForTest(),
		Failures:               validFailureSnapshotForTest(),
		PendingSuggestionsAfter: 0,
		TestEvidence:           validTestEvidenceForTest(),
	}
	asserts := evaluateAssertions(r)
	require.True(t, assertionsAllPass(asserts), "%+v", asserts)
}

// 1. TestFourthReview_UnfinishedRecoveryMustNotPass: 306601 dirty/outcome 为空、任务未完成且 retry_due=1，整体必须不通过。
func TestFourthReview_UnfinishedRecoveryMustNotPass(t *testing.T) {
	r := &verifyReport{
		Baseline: &baselineSnapshot{StatusCounts: map[string]int64{"failed": 0, "dirty": 1, "ready": 100}},
		Eligibility: &eligibilityResult{
			FailedExcludedOK: true, ActiveCenterCount: 50, EligiblePersonCount: 40,
		},
		ProfileRecovery:        &sliceRunResult{SlicesRun: 5, StoppedIdle: false},
		SampleEmptyBuilt:       0,
		ANNReadyAtAttach:       true,
		PostRecoveryANNCenters: 50,
		MergeSuggestion:        &sliceRunResult{SlicesRun: 3, StoppedIdle: false},
		SampleBefore: []personSample{
			{PersonID: 306601, Present: true, Status: "dirty", BuildOutcome: ""},
		},
		SampleFinal: []personSample{
			{PersonID: 306601, Present: true, Status: "dirty", BuildOutcome: ""},
			{PersonID: 264975, Present: true, Status: "ready", CenterCount: 5},
			{PersonID: 356935, Present: true, Status: "ready", CenterCount: 2},
			{PersonID: 307945, Present: true, Status: "ready", BuildOutcome: model.PersonIdentityBuildOutcomeNoUsableEvidence},
			{PersonID: 562545, Present: true, Status: "ready", BuildOutcome: model.PersonIdentityBuildOutcomeNoUsableEvidence},
		},
		Failures: &failureSnapshot{
			Counts: model.PersonMergeFailureCounts{
				UnresolvedTargets: 2,
				RetryTotal:        1,
				RetryDue:          1,
				WaitingEvidence:   1,
			},
			StateCounts: map[string]int64{
				model.PersonMergeFailureStateRetryWait:       1,
				model.PersonMergeFailureStateWaitingEvidence: 1,
			},
			SampleRows: []failureRow{
				{TargetPersonID: 306601, Present: true, State: model.PersonMergeFailureStateRetryWait, Attempts: 1},
				{TargetPersonID: 264975, Present: false},
				{TargetPersonID: 356935, Present: false},
				{TargetPersonID: 307945, Present: true, State: model.PersonMergeFailureStateWaitingEvidence},
				{TargetPersonID: 562545, Present: true, State: model.PersonMergeFailureStateWaitingEvidence},
			},
		},
		TestEvidence: validTestEvidenceForTest(),
	}
	asserts := evaluateAssertions(r)
	require.False(t, assertionsAllPass(asserts), "unfinished recovery with dirty sample and retry_due must not pass")
}

// 2. TestFourthReview_InconsistentCountsMustNotPass: unresolved=0，但 waiting_evidence 汇总和实际记录为 1，整体必须不通过。
func TestFourthReview_InconsistentCountsMustNotPass(t *testing.T) {
	r := &verifyReport{
		Baseline: &baselineSnapshot{StatusCounts: map[string]int64{"failed": 0, "dirty": 0, "ready": 100}},
		Eligibility: &eligibilityResult{
			FailedExcludedOK: true, ActiveCenterCount: 50, EligiblePersonCount: 40,
		},
		ProfileRecovery:        &sliceRunResult{SlicesRun: 5, StoppedIdle: true},
		ANNReadyAtAttach:       true,
		PostRecoveryANNCenters: 50,
		MergeSuggestion:        &sliceRunResult{SlicesRun: 3, StoppedIdle: true},
		SampleFinal:            validFiveSamplesFinalForTest(),
		Failures: &failureSnapshot{
			Counts: model.PersonMergeFailureCounts{
				UnresolvedTargets: 0, // 矛盾点：声称 0
			},
			StateCounts: map[string]int64{
				model.PersonMergeFailureStateWaitingEvidence: 1, // 但这里记录 1
			},
			SampleRows: []failureRow{
				{TargetPersonID: 306601, Present: true, State: model.PersonMergeFailureStateWaitingEvidence, ReasonCode: "no_usable_evidence"},
				{TargetPersonID: 264975, Present: false},
				{TargetPersonID: 356935, Present: false},
				{TargetPersonID: 307945, Present: false},
				{TargetPersonID: 562545, Present: false},
			},
		},
		TestEvidence: validTestEvidenceForTest(),
	}
	asserts := evaluateAssertions(r)
	require.False(t, assertionsAllPass(asserts), "inconsistent counts (unresolved=0 vs waiting=1) must not pass")
	var final assertionResult
	for _, a := range asserts {
		if a.Name == "final_state" {
			final = a
		}
	}
	require.NotEqual(t, assertPass, final.Status, "final_state must reject contradictory counts")
}

// 3. TestFourthReview_FaultEvidenceCannotBeUnconditionallyPass: 空报告的 fault_recovery 不得 pass。
func TestFourthReview_FaultEvidenceCannotBeUnconditionallyPass(t *testing.T) {
	r := &verifyReport{}
	asserts := evaluateAssertions(r)
	require.False(t, assertionsAllPass(asserts), "empty report must not pass")
	var fault assertionResult
	for _, a := range asserts {
		if a.Name == "fault_recovery" {
			fault = a
		}
	}
	require.NotEqual(t, assertPass, fault.Status, "fault_recovery cannot be unconditionally pass without evidence")
}

// 边界测试 1: 缺少必需样本不得通过
func TestEvaluateAssertions_MissingRequiredSampleFails(t *testing.T) {
	r := &verifyReport{
		Baseline: &baselineSnapshot{StatusCounts: map[string]int64{"failed": 0, "dirty": 0, "ready": 100}},
		Eligibility: &eligibilityResult{
			FailedExcludedOK: true, ActiveCenterCount: 50, EligiblePersonCount: 40,
		},
		ProfileRecovery:        &sliceRunResult{SlicesRun: 5, StoppedIdle: true},
		ANNReadyAtAttach:       true,
		PostRecoveryANNCenters: 50,
		MergeSuggestion:        &sliceRunResult{SlicesRun: 3, StoppedIdle: true},
		// 故意缺少 562545
		SampleFinal: []personSample{
			{PersonID: 264975, Present: true, Status: "ready", CenterCount: 38},
			{PersonID: 356935, Present: true, Status: "ready", CenterCount: 5},
			{PersonID: 306601, Present: true, Status: "ready", BuildOutcome: model.PersonIdentityBuildOutcomeNoUsableEvidence},
			{PersonID: 307945, Present: true, Status: "ready", BuildOutcome: model.PersonIdentityBuildOutcomeNoUsableEvidence},
		},
		Failures:     validFailureSnapshotForTest(),
		TestEvidence: validTestEvidenceForTest(),
	}
	asserts := evaluateAssertions(r)
	require.False(t, assertionsAllPass(asserts))
	var rec assertionResult
	for _, a := range asserts {
		if a.Name == "recovery" {
			rec = a
		}
	}
	require.Equal(t, assertFail, rec.Status)
	require.Contains(t, rec.Detail, "562545 missing")
}

// 边界测试 2: 统计来源缺失不得通过
func TestEvaluateAssertions_MissingStatisticsSourceBlocked(t *testing.T) {
	r := &verifyReport{
		Baseline: &baselineSnapshot{StatusCounts: map[string]int64{"failed": 0, "dirty": 0, "ready": 100}},
		Eligibility: &eligibilityResult{
			FailedExcludedOK: true, ActiveCenterCount: 50, EligiblePersonCount: 40,
		},
		ProfileRecovery:        &sliceRunResult{SlicesRun: 5, StoppedIdle: true},
		ANNReadyAtAttach:       true,
		PostRecoveryANNCenters: 50,
		MergeSuggestion:        &sliceRunResult{SlicesRun: 3, StoppedIdle: true},
		SampleFinal:            validFiveSamplesFinalForTest(),
		Failures:               nil, // 缺失统计来源
		TestEvidence:           validTestEvidenceForTest(),
	}
	asserts := evaluateAssertions(r)
	require.False(t, assertionsAllPass(asserts))
	var final, persist assertionResult
	for _, a := range asserts {
		if a.Name == "final_state" {
			final = a
		}
		if a.Name == "persistence" {
			persist = a
		}
	}
	require.Equal(t, assertBlocked, final.Status)
	require.Equal(t, assertBlocked, persist.Status)
}

// 边界测试 3: 声称历史失败关闭但无前态证据不得通过
func TestEvaluateAssertions_HistoricalResolveWithoutBeforeStateBlocked(t *testing.T) {
	snap := validFailureSnapshotForTest()
	// 人为设置 264975 为 resolved，但 FailuresBefore 中没有其失败前态
	snap.SampleRows = append(snap.SampleRows[:0],
		failureRow{TargetPersonID: 264975, Present: true, State: model.PersonMergeFailureStateResolved},
		failureRow{TargetPersonID: 356935, Present: false},
		failureRow{TargetPersonID: 306601, Present: true, State: model.PersonMergeFailureStateWaitingEvidence, ReasonCode: "no_usable_evidence", Attempts: 0},
		failureRow{TargetPersonID: 307945, Present: true, State: model.PersonMergeFailureStateWaitingEvidence, ReasonCode: "no_usable_evidence", Attempts: 0},
		failureRow{TargetPersonID: 562545, Present: true, State: model.PersonMergeFailureStateWaitingEvidence, ReasonCode: "no_usable_evidence", Attempts: 0},
	)

	r := &verifyReport{
		Baseline: &baselineSnapshot{StatusCounts: map[string]int64{"failed": 0, "dirty": 0, "ready": 100}},
		Eligibility: &eligibilityResult{
			FailedExcludedOK: true, ActiveCenterCount: 50, EligiblePersonCount: 40,
		},
		ProfileRecovery:        &sliceRunResult{SlicesRun: 5, StoppedIdle: true},
		ANNReadyAtAttach:       true,
		PostRecoveryANNCenters: 50,
		MergeSuggestion:        &sliceRunResult{SlicesRun: 3, StoppedIdle: true},
		SampleFinal:            validFiveSamplesFinalForTest(),
		FailuresBefore: &failureSnapshot{
			SampleRows: []failureRow{
				{TargetPersonID: 264975, Present: false}, // 之前无行
			},
		},
		Failures:     snap,
		TestEvidence: validTestEvidenceForTest(),
	}
	asserts := evaluateAssertions(r)
	require.False(t, assertionsAllPass(asserts))
	var persist assertionResult
	for _, a := range asserts {
		if a.Name == "persistence" {
			persist = a
		}
	}
	require.Equal(t, assertBlocked, persist.Status)
	require.Contains(t, persist.Detail, "claim_resolved_without_before_state")
}

// 边界测试 4: 测试证据失败、跳过、缺项、摘要不匹配均不能通过
func TestEvaluateAssertions_TestEvidenceFailureOrMismatch(t *testing.T) {
	baseReport := func() *verifyReport {
		return &verifyReport{
			Baseline: &baselineSnapshot{StatusCounts: map[string]int64{"failed": 0, "dirty": 0, "ready": 100}},
			Eligibility: &eligibilityResult{
				FailedExcludedOK: true, ActiveCenterCount: 50, EligiblePersonCount: 40,
			},
			ProfileRecovery:        &sliceRunResult{SlicesRun: 5, StoppedIdle: true},
			SampleEmptyBuilt:       3,
			ANNReadyAtAttach:       true,
			PostRecoveryANNCenters: 50,
			MergeSuggestion:        &sliceRunResult{SlicesRun: 3, StoppedIdle: true},
			SampleFinal:            validFiveSamplesFinalForTest(),
			Failures:               validFailureSnapshotForTest(),
		}
	}

	// 1) ExitCode != 0
	r1 := baseReport()
	ev1 := validTestEvidenceForTest()
	ev1.ExitCode = 1
	r1.TestEvidence = ev1
	asserts1 := evaluateAssertions(r1)
	require.False(t, assertionsAllPass(asserts1))
	require.Equal(t, assertFail, asserts1[5].Status)

	// 2) 测试失败
	r2 := baseReport()
	ev2 := validTestEvidenceForTest()
	ev2.FailedTests = []string{"TestThirdReview_ManualCheckpointFailureCanResume"}
	r2.TestEvidence = ev2
	asserts2 := evaluateAssertions(r2)
	require.False(t, assertionsAllPass(asserts2))
	require.Equal(t, assertFail, asserts2[5].Status)

	// 3) 测试跳过
	r3 := baseReport()
	ev3 := validTestEvidenceForTest()
	ev3.SkippedTests = []string{"TestThirdReview_ManualCheckpointFailureCanResume"}
	r3.TestEvidence = ev3
	asserts3 := evaluateAssertions(r3)
	require.False(t, assertionsAllPass(asserts3))
	require.Equal(t, assertBlocked, asserts3[5].Status)

	// 4) 缺少必需测试
	r4 := baseReport()
	ev4 := validTestEvidenceForTest()
	ev4.MissingTests = []string{"TestThirdReview_ProfileReadFailureKeepsReconcilePending"}
	r4.TestEvidence = ev4
	asserts4 := evaluateAssertions(r4)
	require.False(t, assertionsAllPass(asserts4))
	require.Equal(t, assertBlocked, asserts4[5].Status)

	// 5) 代码摘要不匹配
	r5 := baseReport()
	ev5 := validTestEvidenceForTest()
	ev5.DigestMatch = false
	r5.TestEvidence = ev5
	asserts5 := evaluateAssertions(r5)
	require.False(t, assertionsAllPass(asserts5))
	require.Equal(t, assertBlocked, asserts5[5].Status)
}

// 边界测试 5: 明确耗尽状态与零 pending 可通过
func TestEvaluateAssertions_ExplicitExhaustionAndZeroPendingPasses(t *testing.T) {
	snap := &failureSnapshot{
		Counts: model.PersonMergeFailureCounts{
			UnresolvedTargets: 4,
			RetryExhausted:    1,
			WaitingEvidence:   3,
			RetryTotal:        0,
		},
		StateCounts: map[string]int64{
			model.PersonMergeFailureStateExhausted:       1,
			model.PersonMergeFailureStateWaitingEvidence: 3,
		},
		SampleRows: []failureRow{
			{TargetPersonID: 264975, Present: false},
			{TargetPersonID: 356935, Present: false},
			{TargetPersonID: 306601, Present: true, State: model.PersonMergeFailureStateWaitingEvidence, ReasonCode: "no_usable_evidence", Attempts: 0},
			{TargetPersonID: 307945, Present: true, State: model.PersonMergeFailureStateWaitingEvidence, ReasonCode: "no_usable_evidence", Attempts: 0},
			{TargetPersonID: 562545, Present: true, State: model.PersonMergeFailureStateWaitingEvidence, ReasonCode: "no_usable_evidence", Attempts: 0},
		},
	}
	r := &verifyReport{
		Baseline: &baselineSnapshot{StatusCounts: map[string]int64{"failed": 0, "dirty": 0, "ready": 100}},
		Eligibility: &eligibilityResult{
			FailedExcludedOK: true, ActiveCenterCount: 50, EligiblePersonCount: 40,
		},
		ProfileRecovery:        &sliceRunResult{SlicesRun: 5, StoppedIdle: true},
		SampleEmptyBuilt:       3,
		ANNReadyAtAttach:       true,
		PostRecoveryANNCenters: 50,
		MergeSuggestion:        &sliceRunResult{SlicesRun: 3, StoppedIdle: true},
		SampleFinal:            validFiveSamplesFinalForTest(),
		FailuresBefore:         validFailuresBeforeForTest(),
		Failures:               snap,
		PendingSuggestionsAfter: 0,
		TestEvidence:           validTestEvidenceForTest(),
	}
	asserts := evaluateAssertions(r)
	var final assertionResult
	for _, a := range asserts {
		if a.Name == "final_state" {
			final = a
		}
	}
	require.Equal(t, assertPass, final.Status)
}

// 边界测试 6: 验证命令实际退出码与 JSON OK 一致
func TestRun_ExitCodeMatchesReportOK(t *testing.T) {
	var stdout, stderr bytes.Buffer
	// 参数缺失返回 2
	rc := run([]string{}, &stdout, &stderr)
	require.Equal(t, 2, rc)

	// db 不存在返回 1
	stdout.Reset()
	stderr.Reset()
	rc = run([]string{"-db", filepath.Join(t.TempDir(), "non-existent.db")}, &stdout, &stderr)
	require.Equal(t, 1, rc)
}

func TestLoadAndVerifyTestEvidence_Valid(t *testing.T) {
	rawPath := fifthRaw(t)
	rawBytes, err := os.ReadFile(rawPath)
	require.NoError(t, err)
	rawHash := sha256.Sum256(rawBytes)
	rawSHA := hex.EncodeToString(rawHash[:])

	old := buildSourceDigest
	digest := strings.Repeat("d", 64)
	buildSourceDigest = digest
	t.Cleanup(func() { buildSourceDigest = old })

	manifestPath := fifthManifest(t, rawPath, digest, rawSHA)
	ev, err := loadAndVerifyTestEvidence(manifestPath, "")
	require.NoError(t, err)
	require.NotNil(t, ev)
	require.Equal(t, 0, ev.ExitCode)
	require.True(t, ev.DigestMatch)
	require.Equal(t, 5, len(ev.PassedTests))
	require.Empty(t, ev.FailedTests)
	require.Empty(t, ev.SkippedTests)
	require.Empty(t, ev.MissingTests)
	require.Equal(t, assertPass, ev.Status)
}

func fifthReviewBaseline() *verifyReport {
	r := &verifyReport{
		Baseline:        &baselineSnapshot{StatusCounts: map[string]int64{"failed": 0}},
		Eligibility:     &eligibilityResult{FailedExcludedOK: true, ActiveCenterCount: 50, EligiblePersonCount: 40},
		ProfileRecovery: &sliceRunResult{SlicesRun: 5, StoppedIdle: true},
		ANNReadyAtAttach: true, PostRecoveryANNCenters: 50,
		MergeSuggestion: &sliceRunResult{SlicesRun: 3, StoppedIdle: true},
		SampleFinal:     validFiveSamplesFinalForTest(), Failures: validFailureSnapshotForTest(),
		TestEvidence: validTestEvidenceForTest(),
		PendingSuggestionsAfter: 0,
		FailuresBefore: &failureSnapshot{StateCounts: map[string]int64{}, SampleRows: []failureRow{
			{TargetPersonID: 264975, Present: false}, {TargetPersonID: 356935, Present: false},
			{TargetPersonID: 306601, Present: false}, {TargetPersonID: 307945, Present: false}, {TargetPersonID: 562545, Present: false},
		}},
	}
	r.SampleFinal[0].BuildOutcome = "usable"
	r.SampleFinal[1].BuildOutcome = "usable"
	return r
}

func fifthManifest(t *testing.T, rawPath, source, rawHash string) string {
	t.Helper()
	if rawHash == "" && rawPath != "" {
		if data, err := os.ReadFile(rawPath); err == nil {
			h := sha256.Sum256(data)
			rawHash = hex.EncodeToString(h[:])
		}
	}
	now := time.Now().UTC()
	p := filepath.Join(t.TempDir(), "manifest.json")
	b, err := json.Marshal(testEvidenceManifest{
		Command:         "go test -json ...",
		StartedAt:       now.Add(-10 * time.Second),
		EndedAt:         now,
		SourceDigest:    source,
		RawOutputPath:   rawPath,
		RawOutputSHA256: rawHash,
		ExitCode:        0,
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(p, b, 0600))
	return p
}

func fifthRaw(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "raw.jsonl")
	now := time.Now().UTC()
	var b []byte
	for _, name := range RequiredFormalTests {
		for _, action := range []string{"run", "pass"} {
			event, _ := json.Marshal(goTestEvent{
				Package: "github.com/davidhoo/relive/internal/service",
				Test:    name,
				Action:  action,
				Time:    now,
			})
			b = append(b, event...)
			b = append(b, '\n')
		}
	}
	event, _ := json.Marshal(goTestEvent{
		Package: "github.com/davidhoo/relive/internal/service",
		Action:  "pass",
		Time:    now,
	})
	b = append(b, event...)
	b = append(b, '\n')
	require.NoError(t, os.WriteFile(p, b, 0600))
	return p
}

func TestFifthReview_EvidenceReadOrIntegrityErrorMustNotPass(t *testing.T) {
	old := buildSourceDigest
	buildSourceDigest = strings.Repeat("a", 64)
	t.Cleanup(func() { buildSourceDigest = old })
	for _, tc := range []struct{ name, path, hash string }{
		{"missing_raw", filepath.Join(t.TempDir(), "absent.jsonl"), strings.Repeat("0", 64)},
		{"bad_hash", fifthRaw(t), strings.Repeat("0", 64)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := fifthManifest(t, tc.path, buildSourceDigest, tc.hash)
			ev, err := loadAndVerifyTestEvidence(p, "")
			require.Error(t, err)
			require.Nil(t, ev, "Contract 1: loader must return (nil, error) on read or integrity failure")
			r := fifthReviewBaseline()
			r.TestEvidence = blockedEvidence(err)
			a := evaluateAssertions(r)
			require.False(t, assertionsAllPass(a), "loader rejected evidence but overall passed: %+v", a[5])
			require.Equal(t, assertBlocked, a[5].Status)
		})
	}
}

func TestFifthReview_UnboundSourceDigestMustNotPass(t *testing.T) {
	old := buildSourceDigest
	buildSourceDigest = ""
	t.Cleanup(func() { buildSourceDigest = old })
	ev, err := loadAndVerifyTestEvidence(fifthManifest(t, fifthRaw(t), strings.Repeat("b", 64), ""), "")
	require.NoError(t, err)
	r := fifthReviewBaseline()
	r.TestEvidence = ev
	require.False(t, assertionsAllPass(evaluateAssertions(r)), "arbitrary digest accepted without build or source comparison")
}

func TestFifthReview_MissingStatisticsMustNotPass(t *testing.T) {
	r := fifthReviewBaseline()
	r.Failures.StateCounts = nil
	require.False(t, assertionsAllPass(evaluateAssertions(r)), "missing statistics accepted")
}

func TestFifthReview_SampleCountLowerBoundMustNotPass(t *testing.T) {
	r := fifthReviewBaseline()
	r.Failures.Counts.UnresolvedTargets = 1
	r.Failures.Counts.WaitingEvidence = 1
	r.Failures.StateCounts[model.PersonMergeFailureStateWaitingEvidence] = 1
	require.False(t, assertionsAllPass(evaluateAssertions(r)), "3 distinct unresolved sample rows cannot fit global count 1")
}

func TestFifthReview_MissingBeforeMustNotPass(t *testing.T) {
	r := fifthReviewBaseline()
	r.FailuresBefore = nil
	require.False(t, assertionsAllPass(evaluateAssertions(r)), "unknown before-state cannot prove never_failed")
}

func TestFifthReview_DisappearedFailureMustNotPass(t *testing.T) {
	r := fifthReviewBaseline()
	r.FailuresBefore.SampleRows[0] = failureRow{TargetPersonID: 264975, Present: true, State: model.PersonMergeFailureStateRetryWait, Attempts: 1}
	require.False(t, assertionsAllPass(evaluateAssertions(r)), "historical failure vanished without resolve evidence")
}

func TestFifthReview_EmptyUsableOutcomeMustNotPass(t *testing.T) {
	r := fifthReviewBaseline()
	r.SampleFinal[0].BuildOutcome = ""
	require.False(t, assertionsAllPass(evaluateAssertions(r)), "required usable sample has empty outcome")
}

func TestFifthReview_PendingCountMustBeValidated(t *testing.T) {
	r := fifthReviewBaseline()
	r.PendingSuggestionsAfter = -1
	require.False(t, assertionsAllPass(evaluateAssertions(r)), "negative pending count ignored")
}

// sixthReviewValidReport builds a fully passing report fixture for sixth-review tests.
// It uses validTestEvidenceForTest() with Status=assertPass and proper TaskDTO/StatsDTO.
func sixthReviewValidReport() *verifyReport {
	r := &verifyReport{
		Baseline:               &baselineSnapshot{StatusCounts: map[string]int64{"failed": 0}},
		Eligibility:            &eligibilityResult{FailedExcludedOK: true, ActiveCenterCount: 50, EligiblePersonCount: 40},
		ProfileRecovery:        &sliceRunResult{SlicesRun: 5, StoppedIdle: true},
		ANNReadyAtAttach:       true,
		PostRecoveryANNCenters: 50,
		MergeSuggestion:        &sliceRunResult{SlicesRun: 3, StoppedIdle: true},
		SampleFinal:            validFiveSamplesFinalForTest(),
		FailuresBefore:         validFailuresBeforeForTest(),
		Failures:               validFailureSnapshotForTest(),
		TestEvidence:           validTestEvidenceForTest(),
		PendingSuggestionsAfter: 0,
	}
	r.TestEvidence.Status = assertPass
	// Attach TaskDTO/StatsDTO so captureVerificationSnapshot contract is satisfied
	r.Failures.TaskDTO = &model.PersonMergeSuggestionTask{
		Status: "idle", Partial: true, WaitingEvidenceCount: 3, UnresolvedTargetCount: 3,
	}
	r.Failures.StatsDTO = &model.PersonMergeSuggestionStatsResponse{Pending: 0}
	return r
}

// sixthRaw builds a valid raw test output JSONL and returns (rawBytes, rawSHA256).
func sixthRaw(t *testing.T) ([]byte, string) {
	t.Helper()
	var b []byte
	now := time.Now().UTC()
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
	// Also include repository package events with package-level completion
	repoPkg := "github.com/davidhoo/relive/internal/repository"
	repoPassEvent, _ := json.Marshal(goTestEvent{Package: repoPkg, Action: "pass", Time: now})
	b = append(b, repoPassEvent...)
	b = append(b, '\n')
	// Service package completion
	svcPassEvent, _ := json.Marshal(goTestEvent{Package: requiredTestPackage, Action: "pass", Time: now})
	b = append(b, svcPassEvent...)
	b = append(b, '\n')
	h := sha256.Sum256(b)
	return b, hex.EncodeToString(h[:])
}

// buildSixthManifest writes a manifest.json and raw.jsonl to a temp dir and returns the manifest path.
func buildSixthManifest(t *testing.T, raw []byte, rawSHA string, mods func(m map[string]interface{})) string {
	t.Helper()
	dir := t.TempDir()
	rawPath := filepath.Join(dir, "raw.jsonl")
	require.NoError(t, os.WriteFile(rawPath, raw, 0o600))

	digest := strings.Repeat("d", 64)
	m := map[string]interface{}{
		"command":           "go test -json -race ./internal/service ./internal/repository -run 'TestThirdReview_'",
		"started_at":        time.Now().UTC().Add(-10 * time.Second),
		"ended_at":          time.Now().UTC(),
		"exit_code":         0,
		"head_commit":       "fec4f46",
		"source_digest":     digest,
		"raw_output_sha256": rawSHA,
		"raw_output_path":   rawPath,
	}
	if mods != nil {
		mods(m)
	}
	mb, err := json.Marshal(m)
	require.NoError(t, err)
	manifestPath := filepath.Join(dir, "manifest.json")
	require.NoError(t, os.WriteFile(manifestPath, mb, 0o600))
	return manifestPath
}

// TestSixthReview_EvidenceProvenanceMustBeComplete: G1 scenarios.
// missing_exit_code, wrong_execution_window, repository_completion_missing each must be rejected.
func TestSixthReview_EvidenceProvenanceMustBeComplete(t *testing.T) {
	rawBytes, rawSHA := sixthRaw(t)
	digest := strings.Repeat("d", 64)

	old := buildSourceDigest
	buildSourceDigest = digest
	t.Cleanup(func() { buildSourceDigest = old })

	for _, tc := range []string{"missing_exit_code", "wrong_execution_window", "repository_completion_missing"} {
		tc := tc
		t.Run(tc, func(t *testing.T) {
			var manifestPath string
			switch tc {
			case "missing_exit_code":
				// Omit exit_code field entirely
				manifestPath = buildSixthManifest(t, rawBytes, rawSHA, func(m map[string]interface{}) {
					delete(m, "exit_code")
				})
			case "wrong_execution_window":
				// Use a window far in the past so test events fall outside it
				manifestPath = buildSixthManifest(t, rawBytes, rawSHA, func(m map[string]interface{}) {
					m["started_at"] = "2020-01-01T00:00:00Z"
					m["ended_at"] = "2020-01-01T00:01:00Z"
					// Recompute sha256 is not needed; rawBytes and rawSHA are unchanged
				})
			case "repository_completion_missing":
				// Build raw that has repository test events but no repository package completion
				var b []byte
				now := time.Now().UTC()
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
				// Add repository test events with NO repository package completion
				repoEvt, _ := json.Marshal(goTestEvent{Package: "github.com/davidhoo/relive/internal/repository", Test: "SomeTest", Action: "run", Time: now})
				b = append(b, repoEvt...)
				b = append(b, '\n')
				// Service package completion only
				svcEvt, _ := json.Marshal(goTestEvent{Package: requiredTestPackage, Action: "pass", Time: now})
				b = append(b, svcEvt...)
				b = append(b, '\n')
				h := sha256.Sum256(b)
				manifestPath = buildSixthManifest(t, b, hex.EncodeToString(h[:]), func(m map[string]interface{}) {
					m["raw_output_sha256"] = hex.EncodeToString(h[:])
					m["raw_output_path"] = filepath.Join(filepath.Dir(m["raw_output_path"].(string)), "raw.jsonl")
				})
				// Write the modified raw to the manifest's directory
				p := m_findRawPath(manifestPath)
				require.NoError(t, os.WriteFile(p, b, 0o600))
			}

			ev, err := loadAndVerifyTestEvidence(manifestPath, digest)
			if err != nil {
				// Loader correctly rejected: check the report rejects it too
				r := sixthReviewValidReport()
				r.TestEvidence = blockedEvidence(err)
				require.False(t, assertionsAllPass(evaluateAssertions(r)), "incomplete evidence accepted: %s", tc)
				return
			}
			r := sixthReviewValidReport()
			r.TestEvidence = ev
			require.False(t, assertionsAllPass(evaluateAssertions(r)), "incomplete evidence accepted without loader error: %s", tc)
		})
	}
}

// m_findRawPath reads the manifest JSON and returns raw_output_path, for use in repository_completion_missing subtest.
func m_findRawPath(manifestPath string) string {
	data, _ := os.ReadFile(manifestPath)
	var m map[string]interface{}
	_ = json.Unmarshal(data, &m)
	if p, ok := m["raw_output_path"].(string); ok {
		return p
	}
	return ""
}

// TestSixthReview_ResolveRequiresValidTimeAndReason: G2 scenarios.
// missing_resolved_at, outdated_resolved_at, unknown_resolved_reason each must be rejected.
func TestSixthReview_ResolveRequiresValidTimeAndReason(t *testing.T) {
	for _, tc := range []string{"missing_resolved_at", "outdated_resolved_at", "unknown_resolved_reason"} {
		tc := tc
		t.Run(tc, func(t *testing.T) {
			r := sixthReviewValidReport()
			r.CollectedAt = time.Now().UTC()
			r.RunEndedAt = r.CollectedAt.Add(5 * time.Minute)

			// Before state: 264975 had a retry-wait failure
			r.FailuresBefore.SampleRows[0] = failureRow{
				TargetPersonID: 264975, Present: true, State: model.PersonMergeFailureStateRetryWait,
			}

			row := failureRow{
				TargetPersonID: 264975,
				Present:        true,
				State:          model.PersonMergeFailureStateResolved,
				ResolvedReason: model.PersonMergeFailureResolvedCompleteSuccess,
			}
			switch tc {
			case "missing_resolved_at":
				// row.ResolvedAt stays nil → no timestamp
			case "outdated_resolved_at":
				old := r.CollectedAt.Add(-24 * time.Hour)
				row.ResolvedAt = &old
			case "unknown_resolved_reason":
				now := r.CollectedAt.Add(time.Minute)
				row.ResolvedAt = &now
				row.ResolvedReason = "not_a_real_reason"
			}
			r.Failures.SampleRows[0] = row
			r.Failures.StateCounts[model.PersonMergeFailureStateResolved] = 1

			require.False(t, assertionsAllPass(evaluateAssertions(r)), "unproven resolve accepted: %s", tc)
			var persist assertionResult
			for _, a := range evaluateAssertions(r) {
				if a.Name == "persistence" {
					persist = a
				}
			}
			require.NotEqual(t, assertPass, persist.Status, "persistence must reject unproven resolve: %s", tc)
		})
	}
}

// TestSixthReview_UnchangedResolvedMustRemainValid: G2 — a historically resolved row (before and after
// are identical resolved) must be accepted since it does not claim a new resolution in this run.
func TestSixthReview_UnchangedResolvedMustRemainValid(t *testing.T) {
	r := sixthReviewValidReport()
	old := time.Now().UTC().Add(-24 * time.Hour)
	row := failureRow{
		TargetPersonID: 264975,
		Present:        true,
		State:          model.PersonMergeFailureStateResolved,
		ResolvedReason: model.PersonMergeFailureResolvedCompleteSuccess,
		ResolvedAt:     &old,
	}
	// Both before and after show the same resolved row → unchanged historical resolved
	r.FailuresBefore.SampleRows[0] = row
	r.Failures.SampleRows[0] = row
	r.Failures.StateCounts[model.PersonMergeFailureStateResolved] = 1
	require.True(t, assertionsAllPass(evaluateAssertions(r)),
		"unchanged resolved is valid and must not claim a new resolution")
}

// TestSeventhReview_ResolvedBeforeRunMustNotPass: G2 — ResolvedAt before the real run
// window cannot prove this run closed the failure. Uses [CollectedAt, RunEndedAt], not ±2h.
func TestSeventhReview_ResolvedBeforeRunMustNotPass(t *testing.T) {
	r := sixthReviewValidReport()
	r.CollectedAt = time.Now().UTC()
	r.RunEndedAt = r.CollectedAt.Add(10 * time.Minute)
	old := r.CollectedAt.Add(-time.Hour)
	r.FailuresBefore.SampleRows[0] = failureRow{
		TargetPersonID: 264975, Present: true, State: model.PersonMergeFailureStateRetryWait,
	}
	r.Failures.SampleRows[0] = failureRow{
		TargetPersonID: 264975,
		Present:        true,
		State:          model.PersonMergeFailureStateResolved,
		ResolvedReason: model.PersonMergeFailureResolvedCompleteSuccess,
		ResolvedAt:     &old,
	}
	r.Failures.StateCounts[model.PersonMergeFailureStateResolved] = 1

	asserts := evaluateAssertions(r)
	require.False(t, assertionsAllPass(asserts),
		"a closure 1 hour before run start cannot prove this run resolved the failure")
	var persist assertionResult
	for _, a := range asserts {
		if a.Name == "persistence" {
			persist = a
		}
	}
	require.NotEqual(t, assertPass, persist.Status)
	require.Contains(t, persist.Detail, "resolved_at_outside_window")
}

// TestSeventhReview_ResolveInsideRunWindowMustPass: G2 positive — ResolvedAt inside the
// recorded run window with a valid reason is accepted as a new closure.
func TestSeventhReview_ResolveInsideRunWindowMustPass(t *testing.T) {
	r := sixthReviewValidReport()
	r.CollectedAt = time.Now().UTC().Add(-5 * time.Minute)
	r.RunEndedAt = time.Now().UTC()
	resolvedAt := r.CollectedAt.Add(2 * time.Minute)
	r.FailuresBefore.SampleRows[0] = failureRow{
		TargetPersonID: 264975, Present: true, State: model.PersonMergeFailureStateRetryWait,
	}
	r.Failures.SampleRows[0] = failureRow{
		TargetPersonID: 264975,
		Present:        true,
		State:          model.PersonMergeFailureStateResolved,
		ResolvedReason: model.PersonMergeFailureResolvedCompleteSuccess,
		ResolvedAt:     &resolvedAt,
	}
	r.Failures.StateCounts[model.PersonMergeFailureStateResolved] = 1

	asserts := evaluateAssertions(r)
	require.True(t, assertionsAllPass(asserts), "in-window resolve with valid reason must pass; got %+v", asserts)
}

// TestSeventhReview_MissingRunWindowMustNotPass: G2 — without a real run end time,
// a claimed new resolve must not pass (no time.Now() / ±2h fallback).
func TestSeventhReview_MissingRunWindowMustNotPass(t *testing.T) {
	r := sixthReviewValidReport()
	r.CollectedAt = time.Now().UTC()
	r.RunEndedAt = time.Time{} // deliberately absent
	resolvedAt := r.CollectedAt.Add(time.Minute)
	r.FailuresBefore.SampleRows[0] = failureRow{
		TargetPersonID: 264975, Present: true, State: model.PersonMergeFailureStateRetryWait,
	}
	r.Failures.SampleRows[0] = failureRow{
		TargetPersonID: 264975,
		Present:        true,
		State:          model.PersonMergeFailureStateResolved,
		ResolvedReason: model.PersonMergeFailureResolvedCompleteSuccess,
		ResolvedAt:     &resolvedAt,
	}
	r.Failures.StateCounts[model.PersonMergeFailureStateResolved] = 1

	asserts := evaluateAssertions(r)
	require.False(t, assertionsAllPass(asserts), "missing run window must not allow resolve to pass")
	var persist assertionResult
	for _, a := range asserts {
		if a.Name == "persistence" {
			persist = a
		}
	}
	require.Contains(t, persist.Detail, "run_window_unavailable")
}

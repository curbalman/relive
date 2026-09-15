package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/davidhoo/relive/internal/model"
)

var validResolvedReasons = map[string]bool{
	model.PersonMergeFailureResolvedCompleteSuccess: true,
	model.PersonMergeFailureResolvedZeroCandidate:   true,
	model.PersonMergeFailureResolvedIneligible:      true,
}

// assertionResult 单项必需断言；整体 OK 仅在全部 pass 时为 true。
type assertionResult struct {
	Name     string `json:"name"`
	Scope    string `json:"scope"`
	Status   string `json:"status"` // pass | fail | blocked | skipped
	Detail   string `json:"detail,omitempty"`
	Before   string `json:"before,omitempty"`
	After    string `json:"after,omitempty"`
	Required bool   `json:"required"`
}

const (
	assertPass    = "pass"
	assertFail    = "fail"
	assertBlocked = "blocked"
	assertSkipped = "skipped"
)

var requiredAssertionNames = []string{
	"eligibility",
	"recovery",
	"startup_wiring",
	"persistence",
	"final_state",
	"fault_recovery",
}

func evaluateAssertions(r *verifyReport) []assertionResult {
	out := make([]assertionResult, 0, 6)

	// 1. 资格
	elig := assertionResult{
		Name:     "eligibility",
		Scope:    "failed/hidden/model-or-generation-ineligible centers excluded from ListAllActiveCenters",
		Required: true,
	}
	if r == nil || r.Eligibility == nil {
		elig.Status = assertBlocked
		elig.Detail = "eligibility result missing"
	} else if !r.Eligibility.FailedExcludedOK || len(r.Eligibility.FailedPersonIDsInEligible) > 0 {
		elig.Status = assertFail
		elig.Detail = fmt.Sprintf("failed persons still eligible: %v", r.Eligibility.FailedPersonIDsInEligible)
	} else {
		elig.Status = assertPass
		elig.Detail = fmt.Sprintf("centers=%d eligible_persons=%d", r.Eligibility.ActiveCenterCount, r.Eligibility.EligiblePersonCount)
	}
	out = append(out, elig)

	// 2. 恢复 (V1: 区分运行过和完成了; F4: 严格校验 usable 与真实有效中心)
	rec := assertionResult{
		Name:     "recovery",
		Scope:    "declared failed/legacy-empty samples went through shared recovery entry + real builder",
		Required: true,
	}
	if r == nil || r.Baseline == nil || r.ProfileRecovery == nil {
		rec.Status = assertBlocked
		rec.Detail = "baseline or profile recovery missing"
	} else {
		failedBefore := r.Baseline.StatusCounts["failed"]
		rec.Before = fmt.Sprintf("failed=%d dirty=%d", failedBefore, r.Baseline.StatusCounts["dirty"])
		rec.After = fmt.Sprintf("profile_slices=%d idle=%v sample_built=%d", r.ProfileRecovery.SlicesRun, r.ProfileRecovery.StoppedIdle, r.SampleEmptyBuilt)

		finalByID := make(map[uint]personSample, len(r.SampleFinal))
		for _, s := range r.SampleFinal {
			finalByID[s.PersonID] = s
		}

		sampleOK := true
		var sampleNotes []string

		for _, reqID := range samplePersonIDs {
			s, found := finalByID[reqID]
			if !found || !s.Present {
				sampleOK = false
				sampleNotes = append(sampleNotes, fmt.Sprintf("%d missing", reqID))
				continue
			}
			switch reqID {
			case 264975, 356935:
				if s.Status != model.PersonIdentityProfileStatusReady {
					sampleOK = false
					sampleNotes = append(sampleNotes, fmt.Sprintf("%d status=%s (expected ready)", reqID, s.Status))
				} else if s.BuildOutcome != "usable" {
					sampleOK = false
					sampleNotes = append(sampleNotes, fmt.Sprintf("%d build_outcome=%q (expected usable)", reqID, s.BuildOutcome))
				} else if s.EligibleCenterCount <= 0 && s.CenterCount <= 0 {
					sampleOK = false
					sampleNotes = append(sampleNotes, fmt.Sprintf("%d eligible_centers=%d (expected >0)", reqID, s.EligibleCenterCount))
				}
			case 306601, 307945, 562545:
				if s.Status != model.PersonIdentityProfileStatusReady {
					sampleOK = false
					sampleNotes = append(sampleNotes, fmt.Sprintf("%d status=%s (expected ready)", reqID, s.Status))
				} else if s.BuildOutcome != model.PersonIdentityBuildOutcomeNoUsableEvidence {
					sampleOK = false
					sampleNotes = append(sampleNotes, fmt.Sprintf("%d build_outcome=%q (expected %s)", reqID, s.BuildOutcome, model.PersonIdentityBuildOutcomeNoUsableEvidence))
				} else if s.EligibleCenterCount > 0 || s.CenterCount > 0 {
					sampleOK = false
					sampleNotes = append(sampleNotes, fmt.Sprintf("%d centers=%d (expected 0)", reqID, s.CenterCount))
				}
			}
		}

		if r.ProfileRecovery.SlicesRun == 0 && r.SampleEmptyBuilt == 0 && failedBefore > 0 {
			rec.Status = assertFail
			rec.Detail = "no recovery work evidence despite failed baseline"
		} else if !sampleOK {
			rec.Status = assertFail
			rec.Detail = "sample recovery incomplete: " + strings.Join(sampleNotes, "; ")
		} else {
			rec.Status = assertPass
			rec.Detail = fmt.Sprintf("5 required samples verified (2 ready usable with centers, 3 ready no_usable_evidence); profile_slices=%d, built=%d",
				r.ProfileRecovery.SlicesRun, r.SampleEmptyBuilt)
		}
	}
	out = append(out, rec)

	// 3. 启动与核对
	wire := assertionResult{
		Name:     "startup_wiring",
		Scope:    "shared AttachIdentityMatchingWiring + ANN ready + reconcile path",
		Required: true,
	}
	if r == nil {
		wire.Status = assertBlocked
		wire.Detail = "report missing"
	} else if !r.ANNReadyAtAttach {
		wire.Status = assertFail
		wire.Detail = "ANN not ready at Attach"
	} else if r.PostRecoveryANNCenters <= 0 {
		wire.Status = assertFail
		wire.Detail = "no eligible centers after attach"
	} else {
		wire.Status = assertPass
		wire.Detail = fmt.Sprintf("ann_ready=true centers=%d path=AttachIdentityMatchingWiring", r.PostRecoveryANNCenters)
	}
	out = append(out, wire)

	// 4. 持久化 (V2/F3: 真实记录核对持久化与前态转移表)
	persist := assertionResult{
		Name:     "persistence",
		Scope:    "failure ledger / suggestion state durable; absent rows need before/after events",
		Required: true,
	}
	if r == nil || r.Failures == nil || r.MergeSuggestion == nil {
		persist.Status = assertBlocked
		persist.Detail = "failures or merge run missing"
	} else if r.FailuresBefore == nil {
		persist.Status = assertBlocked
		persist.Detail = "failures_before missing (cannot prove prior state)"
	} else {
		persist.Before = fmt.Sprintf("force_due=%d", r.RecoveryForceDueCount)
		persist.After = fmt.Sprintf("merge_slices=%d unresolved=%d states=%v",
			r.MergeSuggestion.SlicesRun, r.Failures.Counts.UnresolvedTargets, r.Failures.StateCounts)

		ok := true
		blocked := false
		var notes []string

		beforeByID := map[uint]failureRow{}
		for _, row := range r.FailuresBefore.SampleRows {
			beforeByID[row.TargetPersonID] = row
		}

		afterByID := map[uint]failureRow{}
		for _, row := range r.Failures.SampleRows {
			afterByID[row.TargetPersonID] = row
		}

		for _, id := range samplePersonIDs {
			beforeRow, hasBefore := beforeByID[id]
			afterRow, hasAfter := afterByID[id]

			if !hasBefore {
				blocked = true
				notes = append(notes, fmt.Sprintf("%d:missing_before_sample_entry", id))
				continue
			}
			if !hasAfter {
				ok = false
				notes = append(notes, fmt.Sprintf("%d:missing_after_sample_entry", id))
				continue
			}

			switch id {
			case 264975, 356935:
				if !afterRow.Present {
					if beforeRow.Present && beforeRow.State != "" {
						ok = false
						notes = append(notes, fmt.Sprintf("%d:disappeared_from_ledger", id))
					} else {
						// 如实记录从未进入失败账本，不声称关闭
						notes = append(notes, fmt.Sprintf("%d:absent_before_and_after", id))
					}
				} else {
					if afterRow.State == model.PersonMergeFailureStateResolved {
						if beforeRow.Present && beforeRow.State == model.PersonMergeFailureStateResolved {
							// 历史已解决记录：前后均已解决且原因合法
							if !validResolvedReasons[afterRow.ResolvedReason] {
								ok = false
								notes = append(notes, fmt.Sprintf("%d:invalid_resolved_reason=%q", id, afterRow.ResolvedReason))
							} else {
								notes = append(notes, fmt.Sprintf("%d:unchanged_resolved (reason=%s)", id, afterRow.ResolvedReason))
							}
						} else if beforeRow.Present && beforeRow.State != model.PersonMergeFailureStateResolved && beforeRow.State != "" {
							// 本轮新关闭：必须具有合法原因、ResolvedAt 存在且在本次执行时间窗口内。
							// 使用真实运行窗口 [CollectedAt, RunEndedAt]，不用 CollectedAt±2h 估算，
							// 缺窗口时标记 blocked 而非放行（§5.4 要求）。
							if !validResolvedReasons[afterRow.ResolvedReason] {
								ok = false
								notes = append(notes, fmt.Sprintf("%d:invalid_resolved_reason=%q", id, afterRow.ResolvedReason))
							} else if afterRow.ResolvedAt == nil {
								ok = false
								notes = append(notes, fmt.Sprintf("%d:missing_resolved_at", id))
							} else {
								runStart := r.CollectedAt
								runEnd := r.RunEndedAt
								if runStart.IsZero() || runEnd.IsZero() || runEnd.Before(runStart) {
									// Run window not available — cannot confirm the closure belongs to this run.
									ok = false
									notes = append(notes, fmt.Sprintf("%d:run_window_unavailable (cannot verify resolved_at=%v belongs to this run)", id, *afterRow.ResolvedAt))
								} else if afterRow.ResolvedAt.Before(runStart) || afterRow.ResolvedAt.After(runEnd.Add(30*time.Second)) {
									ok = false
									notes = append(notes, fmt.Sprintf("%d:resolved_at_outside_window (resolved_at=%v, run=[%v, %v])", id, *afterRow.ResolvedAt, runStart, runEnd))
								} else {
									notes = append(notes, fmt.Sprintf("%d:resolved (was %s, reason=%s)", id, beforeRow.State, afterRow.ResolvedReason))
								}
							}
						} else {
							// 缺少前态证据时不得声称 resolve 成功
							blocked = true
							notes = append(notes, fmt.Sprintf("%d:claim_resolved_without_before_state", id))
						}
					} else {
						ok = false
						notes = append(notes, fmt.Sprintf("%d:unexpected_failure_state=%s", id, afterRow.State))
					}
				}

			case 306601, 307945, 562545:
				if !afterRow.Present {
					ok = false
					notes = append(notes, fmt.Sprintf("%d:missing_failure_row_for_empty_sample", id))
				} else if afterRow.State != model.PersonMergeFailureStateWaitingEvidence {
					ok = false
					notes = append(notes, fmt.Sprintf("%d:state=%s (expected waiting_evidence)", id, afterRow.State))
				} else if afterRow.Attempts > 0 {
					ok = false
					notes = append(notes, fmt.Sprintf("%d:attempts=%d (waiting_evidence must not burn budget)", id, afterRow.Attempts))
				} else {
					notes = append(notes, fmt.Sprintf("%d:waiting_evidence (attempts=0)", id))
				}
			}
		}

		if r.MergeSuggestion.SlicesRun == 0 && r.Failures.Counts.UnresolvedTargets > 0 {
			ok = false
			notes = append(notes, "merge slices=0 with unresolved failures")
		}

		if blocked {
			persist.Status = assertBlocked
			persist.Detail = strings.Join(notes, "; ")
		} else if !ok {
			persist.Status = assertFail
			persist.Detail = strings.Join(notes, "; ")
		} else {
			persist.Status = assertPass
			persist.Detail = strings.Join(notes, "; ")
		}
	}
	out = append(out, persist)

	// 5. 终态一致 (V2/F3: 严格核对统计一致性、下界与终态)
	final := assertionResult{
		Name:     "final_state",
		Scope:    "failure table / pending / task counts explainable; waiting_evidence and zero pending OK with reason",
		Required: true,
	}
	if r == nil || r.Failures == nil {
		final.Status = assertBlocked
		final.Detail = "failures missing"
	} else if r.Failures.StateCounts == nil {
		final.Status = assertFail
		final.Detail = "state_counts missing in failure snapshot"
	} else {
		counts := r.Failures.Counts
		stateCounts := r.Failures.StateCounts
		unresolved := counts.UnresolvedTargets
		waiting := stateCounts[model.PersonMergeFailureStateWaitingEvidence]
		exhausted := stateCounts[model.PersonMergeFailureStateExhausted]
		retry := stateCounts[model.PersonMergeFailureStateRetryWait]
		final.After = fmt.Sprintf("unresolved=%d waiting=%d retry=%d exhausted=%d pending=%d",
			unresolved, waiting, retry, exhausted, r.PendingSuggestionsAfter)

		// 1) 负数检查（包含 pending）
		hasNegative := counts.UnresolvedTargets < 0 || counts.RetryTotal < 0 || counts.RetryDue < 0 ||
			counts.RetryDeferred < 0 || counts.RetryExhausted < 0 || counts.WaitingEvidence < 0 ||
			r.PendingSuggestionsAfter < 0

		// 2) 统计内部算术一致性
		retrySumMismatch := counts.RetryTotal != counts.RetryDue+counts.RetryDeferred
		unresolvedSumMismatch := counts.UnresolvedTargets != counts.RetryTotal+counts.RetryExhausted+counts.WaitingEvidence

		// 3) StateCounts 与 Counts 对齐
		stateMismatch := (retry != counts.RetryTotal || exhausted != counts.RetryExhausted || waiting != counts.WaitingEvidence ||
			retry+exhausted+waiting != counts.UnresolvedTargets)

		// 4) 样本下界约束（样本数不得大于对应全库统计）
		waitingSamples := 0
		retrySamples := 0
		exhaustedSamples := 0
		sampleUnresolved := 0
		seenSampleIDs := make(map[uint]bool)
		for _, row := range r.Failures.SampleRows {
			if !seenSampleIDs[row.TargetPersonID] {
				seenSampleIDs[row.TargetPersonID] = true
				if row.Present && row.State != model.PersonMergeFailureStateResolved && row.State != "" {
					sampleUnresolved++
					switch row.State {
					case model.PersonMergeFailureStateWaitingEvidence:
						waitingSamples++
					case model.PersonMergeFailureStateRetryWait:
						retrySamples++
					case model.PersonMergeFailureStateExhausted:
						exhaustedSamples++
					}
				}
			}
		}

		sampleLowerBoundViolation := (int64(sampleUnresolved) > counts.UnresolvedTargets ||
			int64(waitingSamples) > waiting ||
			int64(retrySamples) > retry ||
			int64(exhaustedSamples) > exhausted)

		// 5) unresolved=0 与样本行矛盾检查
		zeroContradiction := (counts.UnresolvedTargets == 0 && (sampleUnresolved > 0 || waiting > 0 || retry > 0 || exhausted > 0))

		// 6) DTO 检查（若快照提供了 TaskDTO 或 StatsDTO）
		dtoMismatch := false
		var dtoDetail string
		if r.Failures.TaskDTO != nil {
			t := r.Failures.TaskDTO
			if t.CurrentMessage == "failure ledger unavailable" {
				dtoMismatch = true
				dtoDetail = "task DTO indicates failure ledger unavailable"
			} else if int64(t.UnresolvedTargetCount) != counts.UnresolvedTargets ||
				int64(t.RetryTotalCount) != counts.RetryTotal ||
				int64(t.RetryDueCount) != counts.RetryDue ||
				int64(t.RetryDeferredCount) != counts.RetryDeferred ||
				int64(t.RetryExhaustedCount) != counts.RetryExhausted ||
				int64(t.WaitingEvidenceCount) != counts.WaitingEvidence {
				dtoMismatch = true
				dtoDetail = fmt.Sprintf("task DTO counts (%d,%d,%d,%d,%d,%d) != DB counts (%d,%d,%d,%d,%d,%d)",
					t.UnresolvedTargetCount, t.RetryTotalCount, t.RetryDueCount, t.RetryDeferredCount, t.RetryExhaustedCount, t.WaitingEvidenceCount,
					counts.UnresolvedTargets, counts.RetryTotal, counts.RetryDue, counts.RetryDeferred, counts.RetryExhausted, counts.WaitingEvidence)
			}
		}
		if r.Failures.StatsDTO != nil {
			if r.Failures.StatsDTO.Pending != r.PendingSuggestionsAfter {
				dtoMismatch = true
				dtoDetail = fmt.Sprintf("stats DTO pending (%d) != DB pending (%d)", r.Failures.StatsDTO.Pending, r.PendingSuggestionsAfter)
			}
		}

		if hasNegative {
			final.Status = assertFail
			final.Detail = fmt.Sprintf("negative counts observed in failure statistics or pending (pending=%d)", r.PendingSuggestionsAfter)
		} else if retrySumMismatch {
			final.Status = assertFail
			final.Detail = fmt.Sprintf("inconsistent retry counts: retry_total(%d) != retry_due(%d) + retry_deferred(%d)",
				counts.RetryTotal, counts.RetryDue, counts.RetryDeferred)
		} else if unresolvedSumMismatch {
			final.Status = assertFail
			final.Detail = fmt.Sprintf("inconsistent unresolved counts: unresolved(%d) != retry(%d) + exhausted(%d) + waiting(%d)",
				counts.UnresolvedTargets, counts.RetryTotal, counts.RetryExhausted, counts.WaitingEvidence)
		} else if stateMismatch {
			final.Status = assertFail
			final.Detail = fmt.Sprintf("inconsistent state breakdown: counts(retry=%d,exhausted=%d,waiting=%d) != state_counts(retry=%d,exhausted=%d,waiting=%d)",
				counts.RetryTotal, counts.RetryExhausted, counts.WaitingEvidence, retry, exhausted, waiting)
		} else if sampleLowerBoundViolation {
			final.Status = assertFail
			final.Detail = fmt.Sprintf("sample lower bound violation: sample distinct targets (unresolved=%d, waiting=%d, retry=%d, exhausted=%d) exceed global counts (unresolved=%d, waiting=%d, retry=%d, exhausted=%d)",
				sampleUnresolved, waitingSamples, retrySamples, exhaustedSamples, counts.UnresolvedTargets, waiting, retry, exhausted)
		} else if zeroContradiction {
			final.Status = assertFail
			final.Detail = fmt.Sprintf("contradiction: unresolved=0 claimed but state counts / sample rows show unresolved failures (waiting=%d, retry=%d, samples=%d)",
				waiting, retry, sampleUnresolved)
		} else if dtoMismatch {
			final.Status = assertFail
			final.Detail = dtoDetail
		} else {
			// 一致性校验通过后，判断终态
			if counts.RetryDue > 0 {
				final.Status = assertBlocked
				final.Detail = fmt.Sprintf("unfinished recovery: %d retries due", counts.RetryDue)
			} else if counts.RetryDeferred > 0 {
				final.Status = assertBlocked
				final.Detail = fmt.Sprintf("has %d deferred retries waiting verification", counts.RetryDeferred)
			} else if counts.UnresolvedTargets == 0 {
				final.Status = assertPass
				final.Detail = "zero unresolved failures verified across ledger and state counts"
			} else if counts.WaitingEvidence == counts.UnresolvedTargets && counts.RetryTotal == 0 {
				final.Status = assertPass
				final.Detail = fmt.Sprintf("all %d unresolved are waiting_evidence (attempts=0)", counts.UnresolvedTargets)
			} else if counts.RetryExhausted+counts.WaitingEvidence == counts.UnresolvedTargets && counts.RetryTotal == 0 {
				final.Status = assertPass
				final.Detail = fmt.Sprintf("all %d unresolved explained (waiting=%d, exhausted=%d); zero retry pending",
					counts.UnresolvedTargets, counts.WaitingEvidence, counts.RetryExhausted)
			} else {
				final.Status = assertBlocked
				final.Detail = fmt.Sprintf("unresolved failures remaining without full explanation: unresolved=%d waiting=%d retry=%d exhausted=%d",
					counts.UnresolvedTargets, counts.WaitingEvidence, counts.RetryTotal, counts.RetryExhausted)
			}
		}
	}
	out = append(out, final)

	// 6. 故障恢复证据 (V3/F1/F2: 正式测试证据输入与核对)
	fault := assertionResult{
		Name:     "fault_recovery",
		Scope:    "manual arrangement + attempt-key replay covered by TestThirdReview_* formal tests",
		Required: true,
	}
	if r == nil || r.TestEvidence == nil {
		fault.Status = assertBlocked
		fault.Detail = "no test evidence provided (pass -test-evidence <manifest.json>)"
	} else {
		ev := r.TestEvidence
		if ev.Status == assertBlocked {
			fault.Status = assertBlocked
			fault.Detail = ev.StatusReason
		} else if ev.Status == assertFail {
			fault.Status = assertFail
			fault.Detail = ev.StatusReason
		} else if ev.ExitCode != 0 {
			fault.Status = assertFail
			fault.Detail = fmt.Sprintf("formal test process failed with exit code %d", ev.ExitCode)
		} else if len(ev.FailedTests) > 0 {
			fault.Status = assertFail
			fault.Detail = fmt.Sprintf("formal tests failed: %s", strings.Join(ev.FailedTests, ", "))
		} else if len(ev.SkippedTests) > 0 {
			fault.Status = assertBlocked
			fault.Detail = fmt.Sprintf("formal tests skipped: %s", strings.Join(ev.SkippedTests, ", "))
		} else if len(ev.MissingTests) > 0 {
			fault.Status = assertBlocked
			fault.Detail = fmt.Sprintf("required formal tests missing: %s", strings.Join(ev.MissingTests, ", "))
		} else if !ev.DigestMatch {
			fault.Status = assertBlocked
			fault.Detail = fmt.Sprintf("source digest mismatch (manifest=%s)", ev.SourceDigest)
		} else if len(ev.PassedTests) < len(RequiredFormalTests) {
			fault.Status = assertBlocked
			fault.Detail = fmt.Sprintf("required formal tests count mismatch: got %d, expected %d", len(ev.PassedTests), len(RequiredFormalTests))
		} else {
			fault.Status = assertPass
			digestBrief := ev.SourceDigest
			if len(digestBrief) > 12 {
				digestBrief = digestBrief[:12]
			}
			fault.Detail = fmt.Sprintf("verified %d formal tests: %s (head=%s, digest=%s)",
				len(ev.PassedTests), strings.Join(ev.PassedTests, ", "), ev.HeadCommit, digestBrief)
		}
	}
	out = append(out, fault)

	return out
}

func assertionsAllPass(asserts []assertionResult) bool {
	if len(asserts) != len(requiredAssertionNames) {
		return false
	}
	seen := make(map[string]bool, len(asserts))
	for _, a := range asserts {
		if !a.Required {
			return false
		}
		if a.Status != assertPass {
			return false
		}
		seen[a.Name] = true
	}
	for _, req := range requiredAssertionNames {
		if !seen[req] {
			return false
		}
	}
	return true
}

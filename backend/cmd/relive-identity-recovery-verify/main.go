// Package main 实现 relive-identity-recovery-verify：在可写 SQLite 副本上验证
// 身份画像恢复 → 空画像接入 → 合并推荐失败闭环（2026-09-14 返修验收）。
//
// 不调用 service.NewService / 全量启动清理；不输出 embedding、路径或密钥。
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/davidhoo/relive/internal/model"
	"github.com/davidhoo/relive/internal/repository"
	"github.com/davidhoo/relive/internal/service"
	"github.com/davidhoo/relive/pkg/config"
	applog "github.com/davidhoo/relive/pkg/logger"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// samplePersonIDs 为验收文档中的对照样本；缺失时仅记录 absent。
var samplePersonIDs = []uint{264975, 356935, 306601, 307945, 562545}

// collectorRunProfileSlices, collectorRunMergeSlices, collectorCaptureSnapshot
// 是可在测试中替换的函数变量，允许跳过耗时切片操作而保留完整断言链路。
var collectorRunProfileSlices = func(svc service.PersonIdentityProfileService, db *gorm.DB, max int) (*sliceRunResult, error) {
	return runProfileSlices(svc, db, max)
}
var collectorRunMergeSlices = func(svc service.PersonMergeSuggestionService, db *gorm.DB, max int) (*sliceRunResult, error) {
	return runMergeSlices(svc, db, max)
}
var collectorCaptureSnapshot = func(db *gorm.DB, mergeSvc service.PersonMergeSuggestionService, sampleIDs []uint, embeddingModel string) (*failureSnapshot, []personSample, int64, error) {
	return captureVerificationSnapshot(db, mergeSvc, sampleIDs, embeddingModel)
}

// collectorBuildProfileANN triggers a synchronous ANN rebuild on the profile service.
// In production this runs after profile slices (which may already have rebuilt the ANN);
// in tests that stub profile slices, this ensures the ANN snapshot is populated from DB
// before BuildIdentityMatchingEngineFromProfileService is called.
var collectorBuildProfileANN = func(svc identityProfileHooks) {
	svc.RebuildANNForVerify()
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("relive-identity-recovery-verify", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dbPath := fs.String("db", "", "Writable SQLite copy path (required; never point at live production)")
	embModelFlag := fs.String("embedding-model", "", "Embedding model signature; empty = infer from ready profiles")
	maxSlices := fs.Int("max-slices", 30, "Max identity-profile coordinator slices")
	mergeSlices := fs.Int("merge-slices", 20, "Max merge-suggestion background slices")
	outPath := fs.String("out", "", "Optional JSON report output path")
	testEvidencePath := fs.String("test-evidence", "", "Optional path to test evidence manifest JSON")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if strings.TrimSpace(*dbPath) == "" {
		fmt.Fprintln(stderr, "error: -db is required")
		printUsage(stderr)
		return 2
	}
	if *maxSlices < 0 || *mergeSlices < 0 {
		fmt.Fprintln(stderr, "error: -max-slices and -merge-slices must be >= 0")
		return 2
	}

	// Services log via pkg/logger; init a quiet console logger (no file side effects).
	_ = applog.Init(config.LoggingConfig{Level: "warn", Console: true})

	report := &verifyReport{
		CollectedAt:       time.Now().UTC(),
		DBPath:            *dbPath,
		MaxSlices:         *maxSlices,
		MergeSlices:       *mergeSlices,
		BuildSourceDigest: buildSourceDigest,
	}

	db, err := openWritableGorm(*dbPath)
	if err != nil {
		fmt.Fprintf(stderr, "error: open db: %v\n", err)
		return 1
	}
	sqlDB, err := db.DB()
	if err != nil {
		fmt.Fprintf(stderr, "error: sql db: %v\n", err)
		return 1
	}
	defer sqlDB.Close()

	if err := ensureRecoverySchema(db); err != nil {
		fmt.Fprintf(stderr, "error: migrate recovery schema: %v\n", err)
		return 1
	}
	report.MigrationsOK = true

	modelSig := strings.TrimSpace(*embModelFlag)
	if modelSig == "" {
		modelSig, err = inferEmbeddingModel(db)
		if err != nil {
			fmt.Fprintf(stderr, "error: infer embedding model: %v\n", err)
			return 1
		}
	}
	report.EmbeddingModel = modelSig
	fmt.Fprintf(stderr, "verify: model=%s max_slices=%d merge_slices=%d\n", modelSig, *maxSlices, *mergeSlices)

	// --- a) Baseline ---
	baseline, err := snapshotBaseline(db, samplePersonIDs, modelSig)
	if err != nil {
		fmt.Fprintf(stderr, "error: baseline: %v\n", err)
		return 1
	}
	report.Baseline = baseline
	fmt.Fprintf(stderr, "baseline: status_counts=%v failed_non_hidden=%d\n",
		baseline.StatusCounts, len(baseline.FailedNonHidden))

	// --- b) Eligibility isolation (before recovery) ---
	elig, err := checkEligibilityIsolation(db, modelSig, baseline.FailedNonHidden)
	if err != nil {
		fmt.Fprintf(stderr, "error: eligibility: %v\n", err)
		return 1
	}
	report.Eligibility = elig
	fmt.Fprintf(stderr, "eligibility: centers=%d failed_in_eligible=%v ok=%v\n",
		elig.ActiveCenterCount, elig.FailedPersonIDsInEligible, elig.FailedExcludedOK)

	// --- Construct services (no NewService / startup cleanup) ---
	repos := repository.NewRepositories(db)
	configService := service.NewConfigService(repos.Config)
	peopleCfg := config.PeopleConfig{
		IdentityProfileMode:            model.PeopleIdentityModePrimary,
		MLEndpoint:                     modelSig,
		IdentityProfileBatchSize:       25,
		IdentityProfileCooldownMs:      1,
		IdentityProfileBuildWorkers:    2,
		IdentityProfileDirtyBatchSize:  25,
		IdentityProfileSliceBudgetMs:   60000,
		IdentityProfileMaxCenters:      6,
		IdentityProfileMinCenterFaces:  3,
		IdentityProfileMinCenterPhotos: 2,
		IdentityProfileMargin:          0.05,
		IdentityProfileRescueThreshold: 0.65,
		MergeSuggestionThreshold:       0.55,
		MergeSuggestionBatchSize:       50,
		MergeSuggestionStaleSeconds:    86400,
	}
	cfg := &config.Config{People: peopleCfg}

	mergeSvcIface := service.NewPersonMergeSuggestionService(
		db, repos.Photo, repos.Face, repos.Person, repos.PeopleJob, repos.CannotLink,
		repos.MergeSuggestion, configService, cfg,
	)
	mergeConcrete := asMergeSuggestion(mergeSvcIface)
	mergeConcrete.SetBackgroundDB(db)

	profileSvcIface := service.NewPersonIdentityProfileService(
		repos.IdentityProfile, configService, cfg, db, nil,
	)
	profileConcrete := asIdentityProfile(profileSvcIface)
	profileConcrete.SetBackgroundDB(db)
	profileConcrete.SetEvidenceReadyHook(mergeConcrete.NotifyProfileEvidenceReady)
	profileConcrete.SetIndexReadyHook(mergeConcrete.NotifyIndexReady)
	mergeConcrete.SetProfileRebuildHook(func(personIDs []uint, reason string) error {
		return profileSvcIface.MarkDirty(personIDs, reason)
	})
	mergeConcrete.SetProfileInvalidateHook(func(personIDs []uint) {
		profileConcrete.InvalidateANNOnly(personIDs)
	})

	// Before snapshot: 必须在首个恢复/加速写入之前采集
	failBefore, samplesBefore, pendingBefore, err := collectorCaptureSnapshot(db, mergeSvcIface, samplePersonIDs, modelSig)
	if err != nil {
		fmt.Fprintf(stderr, "error: before verification snapshot: %v\n", err)
		return 1
	}
	report.FailuresBefore = failBefore
	report.SampleBefore = samplesBefore
	report.PendingSuggestionsBefore = pendingBefore

	// Make non-exhausted failed due so closed-loop can run without waiting backoff.
	forced, err := forceFailedRecoveryDue(db)
	if err != nil {
		fmt.Fprintf(stderr, "error: force recovery due: %v\n", err)
		return 1
	}
	report.RecoveryForceDueCount = forced

	// --- c/d) Profile recovery + empty-profile admit via coordinator slices ---
	profileRun, err := collectorRunProfileSlices(profileSvcIface, db, *maxSlices)
	if err != nil {
		fmt.Fprintf(stderr, "error: profile slices: %v\n", err)
		return 1
	}
	report.ProfileRecovery = profileRun
	sampleAfterProfile, err := snapshotSamples(db, samplePersonIDs, modelSig)
	if err != nil {
		fmt.Fprintf(stderr, "error: sample after profile: %v\n", err)
		return 1
	}
	report.SampleAfterProfile = sampleAfterProfile
	fmt.Fprintf(stderr, "profile: slices=%d idle_stop=%v\n", profileRun.SlicesRun, profileRun.StoppedIdle)

	// 通用接入按 person_id ASC；对照样本可能排在数万未分类之后。
	// 验证副本上：暂退其它 legacy_empty_verify dirty，再用真实 builder 直接构建对照样本
	// （与协调器 buildOne→ReplaceGeneration 同路径），不手写 build_outcome。
	prioritized, err := prioritizeUnclassifiedSampleEmpty(db, samplePersonIDs)
	if err != nil {
		fmt.Fprintf(stderr, "error: prioritize sample empty: %v\n", err)
		return 1
	}
	report.SampleEmptyPrioritized = prioritized
	if prioritized > 0 {
		built, err := buildPrioritizedSamples(profileConcrete, db, samplePersonIDs)
		if err != nil {
			fmt.Fprintf(stderr, "error: build sample empty: %v\n", err)
			return 1
		}
		report.SampleEmptyBuilt = built
		sampleAfterBuilt, err := snapshotSamples(db, samplePersonIDs, modelSig)
		if err != nil {
			fmt.Fprintf(stderr, "error: sample after built: %v\n", err)
			return 1
		}
		report.SampleAfterProfile = sampleAfterBuilt
		fmt.Fprintf(stderr, "sample_empty: prioritized=%d built=%d\n", prioritized, built)
	} else {
		// 已分类的空画像对照样本：补发证据就绪，驱动失败表从 profile_unavailable → waiting_evidence。
		wakeEmptyEvidence(mergeConcrete, report.SampleAfterProfile)
	}

	// --- e) 生产等价装配：复用画像服务 ANN + AttachIdentityMatchingWiring ---
	// 禁止另造诊断 ANN 后再手调 Reconcile；核对入口与 NewService 一致。
	// Ensure the ANN snapshot is built from DB before wiring; in production this
	// is a no-op when real profile slices already rebuilt it; in test stubs that
	// bypass slices this triggers the first rebuild from the fixture DB.
	collectorBuildProfileANN(profileConcrete)
	engine, annReady, centerCount, err := service.BuildIdentityMatchingEngineFromProfileService(
		profileSvcIface,
		repos.IdentityProfile,
		repos.Face,
		repos.CannotLink,
		repos.Face,
		service.IdentityProfileMatcherConfig{
			EmbeddingModel:  modelSig,
			RescueThreshold: peopleCfg.IdentityProfileRescueThreshold,
			Margin:          peopleCfg.IdentityProfileMargin,
			MinCenterFaces:  peopleCfg.IdentityProfileMinCenterFaces,
		},
	)
	if err != nil {
		fmt.Fprintf(stderr, "error: wire merge engine from profile ANN: %v\n", err)
		return 1
	}
	report.PostRecoveryANNCenters = centerCount
	report.ANNReadyAtAttach = annReady
	mergeConcrete.SetIdentityProfileMode(model.PeopleIdentityModePrimary)
	mergeConcrete.SetIdentitySuggestStrategy(service.NewIdentitySuggestStrategy(peopleCfg))
	mergeConcrete.SetIdentityConfigFingerprint(service.IdentityStrategyFingerprint(peopleCfg))
	mergeConcrete.AttachIdentityMatchingWiring(engine, annReady)
	fmt.Fprintf(stderr, "wire: ann_ready=%v eligible_centers=%d path=AttachIdentityMatchingWiring\n",
		annReady, centerCount)

	mergeRun, err := collectorRunMergeSlices(mergeSvcIface, db, *mergeSlices)
	if err != nil {
		fmt.Fprintf(stderr, "error: merge slices: %v\n", err)
		return 1
	}
	report.MergeSuggestion = mergeRun

	// After snapshot: 在所有切片、通知及写入完成后的静止窗口采集
	failAfter, samplesAfter, pendingAfter, err := collectorCaptureSnapshot(db, mergeSvcIface, samplePersonIDs, modelSig)
	if err != nil {
		fmt.Fprintf(stderr, "error: after verification snapshot: %v\n", err)
		return 1
	}
	report.Failures = failAfter
	report.SampleFinal = samplesAfter
	report.PendingSuggestionsAfter = pendingAfter

	if strings.TrimSpace(*testEvidencePath) != "" {
		ev, err := loadAndVerifyTestEvidence(*testEvidencePath, "")
		if err != nil {
			fmt.Fprintf(stderr, "warn: test evidence verification: %v\n", err)
			report.TestEvidence = blockedEvidence(err)
		} else {
			report.TestEvidence = ev
		}
	} else {
		report.TestEvidence = &testEvidenceReport{
			Status:       assertBlocked,
			StatusReason: "no test evidence provided (pass -test-evidence <manifest.json>)",
		}
	}

	// Mark the end of the active run so assertions can use the real [CollectedAt, RunEndedAt]
	// window when validating ResolvedAt timestamps (G2 fix: no time.Now() fallback).
	report.RunEndedAt = time.Now().UTC()
	report.Assertions = evaluateAssertions(report)
	report.OK = assertionsAllPass(report.Assertions)
	fmt.Fprintf(stderr, "merge: slices=%d failure_unresolved=%d ok=%v\n",
		mergeRun.SlicesRun, failAfter.Counts.UnresolvedTargets, report.OK)
	for _, a := range report.Assertions {
		fmt.Fprintf(stderr, "assert %s: %s — %s\n", a.Name, a.Status, a.Detail)
	}

	return writeReportAndExitCode(report, stdout, stderr, *outPath)
}

func writeReportAndExitCode(report *verifyReport, stdout, stderr io.Writer, outPath string) int {
	b, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		fmt.Fprintf(stderr, "error: marshal: %v\n", err)
		return 1
	}
	out := string(b)
	if _, err := fmt.Fprint(stdout, out); err != nil {
		return 1
	}
	if !strings.HasSuffix(out, "\n") {
		fmt.Fprintln(stdout)
	}
	if path := strings.TrimSpace(outPath); path != "" {
		if err := os.WriteFile(path, append(b, '\n'), 0o644); err != nil {
			fmt.Fprintf(stderr, "error: write -out: %v\n", err)
			return 1
		}
		fmt.Fprintf(stderr, "wrote %s\n", path)
	}
	if !report.OK {
		return 1
	}
	return 0
}

func printUsage(w io.Writer) {
	fmt.Fprintln(w, `
Usage:
  relive-identity-recovery-verify -db /path/to/writable-copy.db \
    [-embedding-model MODEL] [-max-slices 30] [-merge-slices 20] \
    [-test-evidence test-evidence.json] [-out report.json]

Opens a WRITABLE SQLite copy (not mode=ro), applies recovery schema helpers
idempotently, verifies failed centers are excluded from ListAllActiveCenters,
runs identity-profile coordinator slices (recovery + empty admit), then merge-
suggestion slices. Reads formal test evidence for fault recovery assertions.
Prints a JSON summary (no embeddings/paths/secrets).
Do not point -db at the live production database.`)
}

// Concrete service types are unexported; assert to local interfaces that list
// the exported methods we need (method sets work across packages).

type identityProfileHooks interface {
	service.PersonIdentityProfileService
	SetBackgroundDB(bgDB *gorm.DB)
	SetEvidenceReadyHook(fn func(personIDs []uint))
	SetIndexReadyHook(fn func())
	BuildPersonOnce(personID uint) error
	InvalidateANNOnly(personIDs []uint)
	// RebuildANNForVerify synchronously rebuilds the ANN index from the current DB state.
	// Called by the verify tool after stubbed profile slices to ensure ANN readiness
	// before BuildIdentityMatchingEngineFromProfileService.
	RebuildANNForVerify()
}

type mergeSuggestionHooks interface {
	service.PersonMergeSuggestionService
	SetBackgroundDB(bgDB *gorm.DB)
	AttachIdentityMatchingWiring(engine *service.IdentityMatchingEngine, indexReady bool)
	SetIdentityProfileMode(mode string)
	SetIdentitySuggestStrategy(strategy service.IdentityStrategy)
	SetIdentityConfigFingerprint(fp string)
	SetProfileRebuildHook(fn func(personIDs []uint, reason string) error)
	SetProfileInvalidateHook(fn func(personIDs []uint))
	NotifyProfileEvidenceReady(personIDs []uint)
	NotifyIndexReady()
}

func asIdentityProfile(svc service.PersonIdentityProfileService) identityProfileHooks {
	h, ok := svc.(identityProfileHooks)
	if !ok {
		panic("PersonIdentityProfileService does not expose recovery verify hooks")
	}
	return h
}

func asMergeSuggestion(svc service.PersonMergeSuggestionService) mergeSuggestionHooks {
	h, ok := svc.(mergeSuggestionHooks)
	if !ok {
		panic("PersonMergeSuggestionService does not expose recovery verify hooks")
	}
	return h
}

func openWritableGorm(path string) (*gorm.DB, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if st, err := os.Stat(abs); err != nil {
		return nil, err
	} else if st.IsDir() {
		return nil, errors.New("db path is a directory")
	}
	dsn := fmt.Sprintf("file:%s?_busy_timeout=5000&_journal_mode=WAL", abs)
	return gorm.Open(sqlite.Open(dsn), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
}

// ensureRecoverySchema 对齐生产 recovery v1 + failures v1–v4 所需列/索引（pkg/database 迁移未导出）。
func ensureRecoverySchema(db *gorm.DB) error {
	if err := db.AutoMigrate(
		&model.Person{},
		&model.Face{},
		&model.PersonIdentityProfile{},
		&model.PersonIdentityCenter{},
		&model.PersonIdentityCenterMember{},
		&model.PersonMergeSuggestion{},
		&model.PersonMergeSuggestionItem{},
		&model.PersonMergeSuggestionFailure{},
		&model.CannotLinkConstraint{},
		&model.PeopleJob{},
	); err != nil {
		return fmt.Errorf("automigrate: %w", err)
	}

	cols := []struct {
		name string
		ddl  string
	}{
		{"change_revision", "ALTER TABLE person_identity_profiles ADD COLUMN change_revision INTEGER NOT NULL DEFAULT 0"},
		{"recovery_attempts", "ALTER TABLE person_identity_profiles ADD COLUMN recovery_attempts INTEGER NOT NULL DEFAULT 0"},
		{"recovery_next_at", "ALTER TABLE person_identity_profiles ADD COLUMN recovery_next_at DATETIME"},
		{"recovery_exhausted", "ALTER TABLE person_identity_profiles ADD COLUMN recovery_exhausted INTEGER NOT NULL DEFAULT 0"},
		{"recovery_error_class", "ALTER TABLE person_identity_profiles ADD COLUMN recovery_error_class VARCHAR(50) NOT NULL DEFAULT ''"},
		{"last_failed_at", "ALTER TABLE person_identity_profiles ADD COLUMN last_failed_at DATETIME"},
		{"build_outcome", "ALTER TABLE person_identity_profiles ADD COLUMN build_outcome VARCHAR(32) NOT NULL DEFAULT ''"},
		{"evidence_version", "ALTER TABLE person_identity_profiles ADD COLUMN evidence_version VARCHAR(64) NOT NULL DEFAULT ''"},
		{"face_count_snapshot", "ALTER TABLE person_identity_profiles ADD COLUMN face_count_snapshot INTEGER NOT NULL DEFAULT 0"},
	}
	for _, col := range cols {
		if !db.Migrator().HasColumn(&model.PersonIdentityProfile{}, col.name) {
			if err := db.Exec(col.ddl).Error; err != nil {
				return fmt.Errorf("add person_identity_profiles.%s: %w", col.name, err)
			}
		}
	}
	for _, sql := range []string{
		`CREATE INDEX IF NOT EXISTS idx_pip_recovery_due ON person_identity_profiles(status, recovery_exhausted, recovery_next_at, person_id)`,
		`CREATE INDEX IF NOT EXISTS idx_pip_build_outcome ON person_identity_profiles(status, build_outcome, person_id)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_pmsf_target ON person_merge_suggestion_failures(target_person_id)`,
		`CREATE INDEX IF NOT EXISTS idx_pmsf_state_next ON person_merge_suggestion_failures(state, next_retry_at)`,
	} {
		if err := db.Exec(sql).Error; err != nil {
			return fmt.Errorf("create index: %w", err)
		}
	}
	if db.Migrator().HasTable(&model.PersonMergeSuggestionFailure{}) {
		extraCols := []string{
			"ManualRetriedAt", "ManualRetryReason",
			"TargetObsKnown", "TargetConsumedGen",
			"LastAttemptKey", "ManualArrangePending", "ManualArrangeRemainingJSON",
		}
		for _, col := range extraCols {
			if !db.Migrator().HasColumn(&model.PersonMergeSuggestionFailure{}, col) {
				if err := db.Migrator().AddColumn(&model.PersonMergeSuggestionFailure{}, col); err != nil {
					return fmt.Errorf("add failure column %s: %w", col, err)
				}
			}
		}
		// 与生产 v3 相同：有真实 gen 的行标 known+consumed（列名跟 GORM 蛇形一致）。
		if err := db.Exec(`UPDATE person_merge_suggestion_failures
			SET target_obs_known = 1, target_consumed_gen = target_profile_gen
			WHERE target_profile_gen > 0 AND target_obs_known = 0`).Error; err != nil {
			return fmt.Errorf("backfill failure obs known: %w", err)
		}
	}
	return nil
}

func inferEmbeddingModel(db *gorm.DB) (string, error) {
	var modelName string
	err := db.Raw(`
		SELECT embedding_model FROM person_identity_profiles
		WHERE status = 'ready' AND embedding_model != ''
		GROUP BY embedding_model
		ORDER BY COUNT(*) DESC
		LIMIT 1`).Scan(&modelName).Error
	if err != nil {
		return "", err
	}
	if modelName == "" {
		return "", errors.New("no embedding_model found in ready profiles")
	}
	return modelName, nil
}

// ---- report types (no embeddings / paths / secrets) ----

type verifyReport struct {
	CollectedAt              time.Time           `json:"collected_at"`
	RunEndedAt               time.Time           `json:"run_ended_at,omitempty"`
	DBPath                   string              `json:"db_path"`
	EmbeddingModel           string              `json:"embedding_model"`
	BuildSourceDigest       string              `json:"build_source_digest,omitempty"`
	MaxSlices                int                 `json:"max_slices"`
	MergeSlices              int                 `json:"merge_slices"`
	MigrationsOK             bool                `json:"migrations_ok"`
	Baseline                 *baselineSnapshot   `json:"baseline"`
	Eligibility              *eligibilityResult  `json:"eligibility"`
	RecoveryForceDueCount    int64               `json:"recovery_force_due_count"`
	SampleBefore             []personSample      `json:"sample_before"`
	FailuresBefore           *failureSnapshot    `json:"failures_before,omitempty"`
	PendingSuggestionsBefore int64               `json:"pending_suggestions_before"`
	ProfileRecovery          *sliceRunResult     `json:"profile_recovery"`
	SampleEmptyPrioritized   int                 `json:"sample_empty_prioritized"`
	SampleEmptyBuilt         int                 `json:"sample_empty_built"`
	SampleEmptyRecovery      *sliceRunResult     `json:"sample_empty_recovery,omitempty"`
	SampleAfterProfile       []personSample      `json:"sample_after_profile"`
	PostRecoveryANNCenters   int                 `json:"post_recovery_ann_centers"`
	ANNReadyAtAttach         bool                `json:"ann_ready_at_attach"`
	MergeSuggestion          *sliceRunResult     `json:"merge_suggestion"`
	Failures                 *failureSnapshot    `json:"failures"`
	PendingSuggestionsAfter  int64               `json:"pending_suggestions_after"`
	SampleFinal              []personSample      `json:"sample_final"`
	TestEvidence             *testEvidenceReport `json:"test_evidence,omitempty"`
	Assertions               []assertionResult   `json:"assertions"`
	OK                       bool                `json:"ok"`
}

type baselineSnapshot struct {
	StatusCounts    map[string]int64 `json:"status_counts"`
	FailedNonHidden []failedBrief    `json:"failed_non_hidden"`
	Samples         []personSample   `json:"samples"`
}

type failedBrief struct {
	PersonID           uint       `json:"person_id"`
	Status             string     `json:"status"`
	ActiveGeneration   int        `json:"active_generation"`
	BuildOutcome       string     `json:"build_outcome"`
	RecoveryAttempts   int        `json:"recovery_attempts"`
	RecoveryExhausted  bool       `json:"recovery_exhausted"`
	RecoveryErrorClass string     `json:"recovery_error_class,omitempty"`
	RecoveryNextAt     *time.Time `json:"recovery_next_at,omitempty"`
	FaceCountSnapshot  int        `json:"face_count_snapshot"`
	CenterCount        int        `json:"center_count"`
}

type personSample struct {
	PersonID            uint   `json:"person_id"`
	Present             bool   `json:"present"`
	Status              string `json:"status,omitempty"`
	ActiveGeneration    int    `json:"active_generation,omitempty"`
	BuildOutcome        string `json:"build_outcome,omitempty"`
	RawCenterCount      int    `json:"raw_center_count,omitempty"`
	EligibleCenterCount int    `json:"eligible_center_count,omitempty"`
	CenterCount         int    `json:"center_count,omitempty"`
	FaceCountSnapshot   int    `json:"face_count_snapshot,omitempty"`
	FaceCount           int    `json:"face_count,omitempty"`
	RecoveryAttempts    int    `json:"recovery_attempts,omitempty"`
	RecoveryExhausted   bool   `json:"recovery_exhausted,omitempty"`
	RecoveryErrorClass  string `json:"recovery_error_class,omitempty"`
	Hidden              bool   `json:"hidden,omitempty"`
}

type eligibilityResult struct {
	ActiveCenterCount         int    `json:"active_center_count"`
	EligiblePersonCount       int    `json:"eligible_person_count"`
	FailedPersonIDsInEligible []uint `json:"failed_person_ids_in_eligible"`
	FailedExcludedOK          bool   `json:"failed_excluded_ok"`
}

type sliceRunResult struct {
	SlicesRun   int  `json:"slices_run"`
	StoppedIdle bool `json:"stopped_idle"`
	IdleStreak  int  `json:"idle_streak_at_stop"`
}

type failureSnapshot struct {
	Counts       model.PersonMergeFailureCounts           `json:"counts"`
	SampleRows   []failureRow                             `json:"sample_rows"`
	StateCounts  map[string]int64                         `json:"state_counts"`
	TaskDTO      *model.PersonMergeSuggestionTask         `json:"task_dto,omitempty"`
	StatsDTO     *model.PersonMergeSuggestionStatsResponse `json:"stats_dto,omitempty"`
	PendingCount int64                                    `json:"pending_count"`
}

type failureRow struct {
	TargetPersonID     uint       `json:"target_person_id"`
	State              string     `json:"state"`
	ReasonCode         string     `json:"reason_code"`
	Attempts           int        `json:"attempts"`
	NextRetryAt        *time.Time `json:"next_retry_at,omitempty"`
	IndexReadyRequired bool       `json:"index_ready_required"`
	ResolvedReason     string     `json:"resolved_reason,omitempty"`
	ResolvedAt         *time.Time `json:"resolved_at,omitempty"`
	UpdatedAt          time.Time  `json:"updated_at,omitempty"`
	LastAttemptKey     string     `json:"last_attempt_key,omitempty"`
	Present            bool       `json:"present"`
}

func snapshotBaseline(db *gorm.DB, samples []uint, modelSig string) (*baselineSnapshot, error) {
	out := &baselineSnapshot{StatusCounts: map[string]int64{}}
	type row struct {
		Status string
		Cnt    int64
	}
	var rows []row
	if err := db.Model(&model.PersonIdentityProfile{}).
		Select("status, COUNT(*) AS cnt").Group("status").Scan(&rows).Error; err != nil {
		return nil, err
	}
	for _, r := range rows {
		out.StatusCounts[r.Status] = r.Cnt
	}

	var failed []model.PersonIdentityProfile
	if err := db.Table("person_identity_profiles").
		Select("person_identity_profiles.*").
		Joins("INNER JOIN people ON people.id = person_identity_profiles.person_id").
		Where("person_identity_profiles.status = ?", model.PersonIdentityProfileStatusFailed).
		Where("people.hidden = ?", false).
		Order("person_identity_profiles.person_id ASC").
		Find(&failed).Error; err != nil {
		return nil, err
	}
	for _, p := range failed {
		cc, err := countActiveCenters(db, p.PersonID, p.ActiveGeneration)
		if err != nil {
			return nil, err
		}
		out.FailedNonHidden = append(out.FailedNonHidden, failedBrief{
			PersonID:           p.PersonID,
			Status:             p.Status,
			ActiveGeneration:   p.ActiveGeneration,
			BuildOutcome:       p.BuildOutcome,
			RecoveryAttempts:   p.RecoveryAttempts,
			RecoveryExhausted:  p.RecoveryExhausted,
			RecoveryErrorClass: p.RecoveryErrorClass,
			RecoveryNextAt:     p.RecoveryNextAt,
			FaceCountSnapshot:  p.FaceCountSnapshot,
			CenterCount:        cc,
		})
	}
	samplesSnap, err := snapshotSamples(db, samples, modelSig)
	if err != nil {
		return nil, err
	}
	out.Samples = samplesSnap
	return out, nil
}

func countActiveCenters(db *gorm.DB, personID uint, generation int) (int, error) {
	if generation <= 0 {
		return 0, nil
	}
	var n int64
	err := db.Model(&model.PersonIdentityCenter{}).
		Where("person_id = ? AND generation = ?", personID, generation).
		Count(&n).Error
	return int(n), err
}

func snapshotSamples(db *gorm.DB, ids []uint, embeddingModel string) ([]personSample, error) {
	return snapshotSamplesTx(db, ids, embeddingModel)
}

func snapshotSamplesTx(tx *gorm.DB, ids []uint, embeddingModel string) ([]personSample, error) {
	out := make([]personSample, 0, len(ids))
	var centersByPerson map[uint][]*model.PersonIdentityCenter
	if embeddingModel != "" {
		repo := repository.NewPersonIdentityProfileRepository(tx)
		var err error
		centersByPerson, err = repo.ListActiveCentersByPersonIDs(ids, embeddingModel)
		if err != nil {
			return nil, fmt.Errorf("list active centers: %w", err)
		}
	}

	for _, id := range ids {
		s := personSample{PersonID: id}
		var p model.PersonIdentityProfile
		err := tx.Where("person_id = ?", id).First(&p).Error
		if err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				out = append(out, s)
				continue
			}
			return nil, fmt.Errorf("query profile for person %d: %w", id, err)
		}
		s.Present = true
		s.Status = p.Status
		s.ActiveGeneration = p.ActiveGeneration
		s.BuildOutcome = p.BuildOutcome
		s.FaceCountSnapshot = p.FaceCountSnapshot
		s.RecoveryAttempts = p.RecoveryAttempts
		s.RecoveryExhausted = p.RecoveryExhausted
		s.RecoveryErrorClass = p.RecoveryErrorClass

		rawCenters, err := countActiveCenters(tx, id, p.ActiveGeneration)
		if err != nil {
			return nil, fmt.Errorf("count active centers for person %d: %w", id, err)
		}
		s.RawCenterCount = rawCenters

		eligibleCount := 0
		if centersByPerson != nil {
			eligibleCount = len(centersByPerson[id])
		}
		s.EligibleCenterCount = eligibleCount
		s.CenterCount = eligibleCount

		var person model.Person
		err = tx.Select("id", "face_count", "hidden").Where("id = ?", id).First(&person).Error
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, fmt.Errorf("query person %d: %w", id, err)
		}
		if err == nil {
			s.FaceCount = person.FaceCount
			s.Hidden = person.Hidden
		}
		out = append(out, s)
	}
	return out, nil
}

func captureVerificationSnapshot(
	db *gorm.DB,
	mergeSvc service.PersonMergeSuggestionService,
	sampleIDs []uint,
	embeddingModel string,
) (*failureSnapshot, []personSample, int64, error) {
	var task *model.PersonMergeSuggestionTask
	var stats *model.PersonMergeSuggestionStatsResponse
	var counts *model.PersonMergeFailureCounts
	var stateCounts map[string]int64
	var sampleRows []failureRow
	var samples []personSample
	var pendingCount int64

	var lastErr error
	for try := 0; try < 3; try++ {
		task = mergeSvc.GetTask()
		if task == nil {
			return nil, nil, 0, errors.New("task DTO unavailable")
		}
		if task.CurrentMessage == "failure ledger unavailable" {
			return nil, nil, 0, errors.New("failure ledger unavailable in task DTO")
		}
		var err error
		stats, err = mergeSvc.GetStats()
		if err != nil || stats == nil {
			return nil, nil, 0, fmt.Errorf("GetStats failed: %w", err)
		}

		tx := db.Begin()
		if tx.Error != nil {
			return nil, nil, 0, fmt.Errorf("begin read tx: %w", tx.Error)
		}

		snapshotTime := time.Now().UTC()
		repo := repository.NewPersonMergeSuggestionFailureRepository(tx)
		counts, err = repo.CountUnresolved(snapshotTime)
		if err != nil {
			tx.Rollback()
			return nil, nil, 0, fmt.Errorf("count unresolved: %w", err)
		}

		countsMismatch := (int64(task.RetryDueCount) != counts.RetryDue ||
			int64(task.RetryDeferredCount) != counts.RetryDeferred ||
			int64(task.RetryTotalCount) != counts.RetryTotal ||
			int64(task.RetryExhaustedCount) != counts.RetryExhausted ||
			int64(task.WaitingEvidenceCount) != counts.WaitingEvidence ||
			int64(task.UnresolvedTargetCount) != counts.UnresolvedTargets)

		if countsMismatch {
			tx.Rollback()
			lastErr = fmt.Errorf("task DTO counts (%d,%d,%d,%d,%d,%d) != DB counts (%d,%d,%d,%d,%d,%d)",
				task.RetryDueCount, task.RetryDeferredCount, task.RetryTotalCount, task.RetryExhaustedCount, task.WaitingEvidenceCount, task.UnresolvedTargetCount,
				counts.RetryDue, counts.RetryDeferred, counts.RetryTotal, counts.RetryExhausted, counts.WaitingEvidence, counts.UnresolvedTargets)
			time.Sleep(10 * time.Millisecond)
			continue
		}

		type sc struct {
			State string
			Cnt   int64
		}
		var scs []sc
		if err := tx.Model(&model.PersonMergeSuggestionFailure{}).
			Select("state, COUNT(*) AS cnt").Group("state").Scan(&scs).Error; err != nil {
			tx.Rollback()
			return nil, nil, 0, fmt.Errorf("group state counts: %w", err)
		}
		stateCounts = map[string]int64{}
		for _, r := range scs {
			stateCounts[r.State] = r.Cnt
		}

		sampleRows = make([]failureRow, 0, len(sampleIDs))
		for _, id := range sampleIDs {
			row := failureRow{TargetPersonID: id}
			got, err := repo.GetByTarget(id)
			if err != nil {
				tx.Rollback()
				return nil, nil, 0, fmt.Errorf("get failure row for person %d: %w", id, err)
			}
			if got != nil {
				row.Present = true
				row.State = got.State
				row.ReasonCode = got.ReasonCode
				row.Attempts = got.Attempts
				row.NextRetryAt = got.NextRetryAt
				row.IndexReadyRequired = got.IndexReadyRequired
				row.ResolvedReason = got.ResolvedReason
				row.ResolvedAt = got.ResolvedAt
				row.UpdatedAt = got.UpdatedAt
				row.LastAttemptKey = got.LastAttemptKey
			}
			sampleRows = append(sampleRows, row)
		}

		samples, err = snapshotSamplesTx(tx, sampleIDs, embeddingModel)
		if err != nil {
			tx.Rollback()
			return nil, nil, 0, fmt.Errorf("snapshot samples: %w", err)
		}

		if err := tx.Model(&model.PersonMergeSuggestion{}).
			Where("status = ?", model.PersonMergeSuggestionStatusPending).
			Count(&pendingCount).Error; err != nil {
			tx.Rollback()
			return nil, nil, 0, fmt.Errorf("count pending suggestions: %w", err)
		}

		if pendingCount != stats.Pending {
			tx.Rollback()
			return nil, nil, 0, fmt.Errorf("DB pending count (%d) != stats DTO pending (%d)", pendingCount, stats.Pending)
		}

		tx.Rollback()
		lastErr = nil
		break
	}

	if lastErr != nil {
		return nil, nil, 0, fmt.Errorf("failed to obtain stable snapshot after 3 attempts: %w", lastErr)
	}

	failSnap := &failureSnapshot{
		Counts:       *counts,
		SampleRows:   sampleRows,
		StateCounts:  stateCounts,
		TaskDTO:      task,
		StatsDTO:     stats,
		PendingCount: pendingCount,
	}
	return failSnap, samples, pendingCount, nil
}

func checkEligibilityIsolation(db *gorm.DB, modelSig string, failed []failedBrief) (*eligibilityResult, error) {
	repo := repository.NewPersonIdentityProfileRepository(db)
	centers, err := repo.ListAllActiveCenters(modelSig)
	if err != nil {
		return nil, err
	}
	persons := map[uint]struct{}{}
	for _, c := range centers {
		if c != nil {
			persons[c.PersonID] = struct{}{}
		}
	}
	var leaked []uint
	for _, f := range failed {
		if _, ok := persons[f.PersonID]; ok {
			leaked = append(leaked, f.PersonID)
		}
	}
	return &eligibilityResult{
		ActiveCenterCount:         len(centers),
		EligiblePersonCount:       len(persons),
		FailedPersonIDsInEligible: leaked,
		FailedExcludedOK:          len(leaked) == 0,
	}, nil
}

func forceFailedRecoveryDue(db *gorm.DB) (int64, error) {
	now := time.Now().UTC()
	res := db.Exec(`
		UPDATE person_identity_profiles
		SET recovery_next_at = ?, updated_at = ?
		WHERE status = ?
		  AND recovery_exhausted = 0
		  AND person_id IN (SELECT id FROM people WHERE hidden = 0)
		  AND EXISTS (SELECT 1 FROM faces WHERE faces.person_id = person_identity_profiles.person_id)
	`, now, now, model.PersonIdentityProfileStatusFailed)
	return res.RowsAffected, res.Error
}

func dueWorkRemaining(db *gorm.DB) (dirty int64, failedDue int64, err error) {
	now := time.Now().UTC()
	if err = db.Model(&model.PersonIdentityProfile{}).
		Where("status = ?", model.PersonIdentityProfileStatusDirty).
		Where("person_id NOT IN (SELECT id FROM people WHERE hidden = ?)", true).
		Count(&dirty).Error; err != nil {
		return
	}
	err = db.Table("person_identity_profiles").
		Joins("INNER JOIN people ON people.id = person_identity_profiles.person_id").
		Where("person_identity_profiles.status = ?", model.PersonIdentityProfileStatusFailed).
		Where("person_identity_profiles.recovery_exhausted = ?", false).
		Where("people.hidden = ?", false).
		Where("person_identity_profiles.recovery_next_at IS NULL OR person_identity_profiles.recovery_next_at <= ?", now).
		Where("EXISTS (SELECT 1 FROM faces WHERE faces.person_id = person_identity_profiles.person_id)").
		Count(&failedDue).Error
	return
}

func prioritizeUnclassifiedSampleEmpty(db *gorm.DB, ids []uint) (int, error) {
	repo := repository.NewPersonIdentityProfileRepository(db)
	var need []uint
	for _, id := range ids {
		if id == 0 {
			continue
		}
		var p model.PersonIdentityProfile
		err := db.Where("person_id = ?", id).First(&p).Error
		if err != nil {
			continue
		}
		if p.Status != model.PersonIdentityProfileStatusReady && p.Status != model.PersonIdentityProfileStatusDirty {
			continue
		}
		if p.BuildOutcome != model.PersonIdentityBuildOutcomeUnclassified && p.Status == model.PersonIdentityProfileStatusReady {
			continue
		}
		var centers int64
		if err := db.Model(&model.PersonIdentityCenter{}).
			Where("person_id = ? AND generation = ? AND ? > 0", id, p.ActiveGeneration, p.ActiveGeneration).
			Count(&centers).Error; err != nil {
			return 0, err
		}
		if centers > 0 {
			continue
		}
		need = append(need, id)
	}
	if len(need) == 0 {
		return 0, nil
	}
	// 通用接入按 person_id ASC；对照样本可能排在数万 dirty 之后。
	// 验证副本上把其它尚未完成的 legacy_empty_verify 暂退回 ready（outcome 仍空），
	// 仅让对照样本进入本轮有界构建——不手写 build_outcome。
	if err := db.Exec(`
		UPDATE person_identity_profiles
		SET status = ?, dirty_reason = '', updated_at = ?
		WHERE status = ?
		  AND dirty_reason = ?
		  AND person_id NOT IN ?
	`, model.PersonIdentityProfileStatusReady, time.Now().UTC(),
		model.PersonIdentityProfileStatusDirty, "legacy_empty_verify", need).Error; err != nil {
		return 0, err
	}
	if err := repo.MarkDirty(need, "legacy_empty_verify"); err != nil {
		return 0, err
	}
	return len(need), nil
}

// buildPrioritizedSamples 用真实 BuildPersonOnce（buildOne→ReplaceGeneration→notify），
// 避免通用接入按 person_id ASC 把对照样本饿死在数万未分类之后。
func buildPrioritizedSamples(svc identityProfileHooks, db *gorm.DB, ids []uint) (int, error) {
	built := 0
	for _, id := range ids {
		if id == 0 {
			continue
		}
		var p model.PersonIdentityProfile
		err := db.Where("person_id = ?", id).First(&p).Error
		if err != nil {
			continue
		}
		if p.Status != model.PersonIdentityProfileStatusDirty &&
			!(p.Status == model.PersonIdentityProfileStatusReady &&
				p.BuildOutcome == model.PersonIdentityBuildOutcomeUnclassified) {
			continue
		}
		if err := svc.BuildPersonOnce(id); err != nil {
			return built, err
		}
		built++
	}
	return built, nil
}

func wakeEmptyEvidence(merge mergeSuggestionHooks, samples []personSample) {
	var ids []uint
	for _, s := range samples {
		if s.Present && s.Status == model.PersonIdentityProfileStatusReady &&
			s.BuildOutcome == model.PersonIdentityBuildOutcomeNoUsableEvidence {
			ids = append(ids, s.PersonID)
		}
	}
	if len(ids) == 0 {
		return
	}
	merge.NotifyProfileEvidenceReady(ids)
}

func unclassifiedEmptyRemaining(db *gorm.DB) (int64, error) {
	var n int64
	err := db.Table("person_identity_profiles").
		Joins("INNER JOIN people ON people.id = person_identity_profiles.person_id").
		Where("person_identity_profiles.status = ?", model.PersonIdentityProfileStatusReady).
		Where("person_identity_profiles.build_outcome = ?", model.PersonIdentityBuildOutcomeUnclassified).
		Where("people.hidden = ?", false).
		Where("EXISTS (SELECT 1 FROM faces WHERE faces.person_id = person_identity_profiles.person_id)").
		Where(`NOT EXISTS (
			SELECT 1 FROM person_identity_centers c
			WHERE c.person_id = person_identity_profiles.person_id
			  AND person_identity_profiles.active_generation > 0
			  AND c.generation = person_identity_profiles.active_generation
		)`).
		Count(&n).Error
	return n, err
}

func runProfileSlices(svc service.PersonIdentityProfileService, db *gorm.DB, max int) (*sliceRunResult, error) {
	const idleNeed = 3
	out := &sliceRunResult{}
	idle := 0
	for i := 0; i < max; i++ {
		dirty, failedDue, err := dueWorkRemaining(db)
		if err != nil {
			return out, err
		}
		empty, err := unclassifiedEmptyRemaining(db)
		if err != nil {
			return out, err
		}
		if err := svc.RunBackgroundSlice(); err != nil {
			return out, err
		}
		out.SlicesRun++
		time.Sleep(2 * time.Millisecond)

		dirty2, failed2, err := dueWorkRemaining(db)
		if err != nil {
			return out, err
		}
		empty2, err := unclassifiedEmptyRemaining(db)
		if err != nil {
			return out, err
		}
		if dirty == 0 && failedDue == 0 && empty == 0 && dirty2 == 0 && failed2 == 0 && empty2 == 0 {
			idle++
		} else {
			idle = 0
		}
		if idle >= idleNeed {
			out.StoppedIdle = true
			out.IdleStreak = idle
			break
		}
	}
	return out, nil
}

func runMergeSlices(svc service.PersonMergeSuggestionService, db *gorm.DB, max int) (*sliceRunResult, error) {
	const idleNeed = 3
	out := &sliceRunResult{}
	idle := 0
	failRepo := repository.NewPersonMergeSuggestionFailureRepository(db)
	for i := 0; i < max; i++ {
		due, err := failRepo.ListDueRetry(time.Now().UTC(), 1)
		if err != nil {
			return out, err
		}
		if err := svc.RunBackgroundSlice(); err != nil {
			return out, err
		}
		out.SlicesRun++
		due2, err := failRepo.ListDueRetry(time.Now().UTC(), 1)
		if err != nil {
			return out, err
		}
		// After constructor MarkDirty, early slices do real work; once no due
		// failures and consecutive slices leave due empty, treat as idle.
		if len(due) == 0 && len(due2) == 0 && i >= 2 {
			idle++
		} else {
			idle = 0
		}
		if idle >= idleNeed {
			out.StoppedIdle = true
			out.IdleStreak = idle
			break
		}
	}
	return out, nil
}

func snapshotFailures(db *gorm.DB, sampleIDs []uint) (*failureSnapshot, error) {
	repo := repository.NewPersonMergeSuggestionFailureRepository(db)
	counts, err := repo.CountUnresolved(time.Now().UTC())
	if err != nil {
		return nil, err
	}
	out := &failureSnapshot{
		Counts:      *counts,
		StateCounts: map[string]int64{},
	}
	type sc struct {
		State string
		Cnt   int64
	}
	var scs []sc
	if err := db.Model(&model.PersonMergeSuggestionFailure{}).
		Select("state, COUNT(*) AS cnt").Group("state").Scan(&scs).Error; err != nil {
		return nil, err
	}
	for _, r := range scs {
		out.StateCounts[r.State] = r.Cnt
	}
	for _, id := range sampleIDs {
		row := failureRow{TargetPersonID: id}
		got, err := repo.GetByTarget(id)
		if err != nil {
			return nil, err
		}
		if got == nil {
			out.SampleRows = append(out.SampleRows, row)
			continue
		}
		row.Present = true
		row.State = got.State
		row.ReasonCode = got.ReasonCode
		row.Attempts = got.Attempts
		row.NextRetryAt = got.NextRetryAt
		row.IndexReadyRequired = got.IndexReadyRequired
		row.ResolvedReason = got.ResolvedReason
		row.ResolvedAt = got.ResolvedAt
		row.UpdatedAt = got.UpdatedAt
		row.LastAttemptKey = got.LastAttemptKey
		out.SampleRows = append(out.SampleRows, row)
	}
	return out, nil
}

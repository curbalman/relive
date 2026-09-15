package service

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/davidhoo/relive/internal/repository"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// TestEmptyProfileSamples_RealBuilderVerify 对 ready 空画像三样本跑真实 ListProfileFaces + builder。
// 仅在设置 RELIVE_EMPTY_PROFILE_SAMPLE_DB 时执行（有界样本库，含 embedding，不打印向量/路径）。
func TestEmptyProfileSamples_RealBuilderVerify(t *testing.T) {
	dbPath := os.Getenv("RELIVE_EMPTY_PROFILE_SAMPLE_DB")
	if dbPath == "" {
		t.Skip("set RELIVE_EMPTY_PROFILE_SAMPLE_DB to run §4.2 sample verify")
	}

	db, err := gorm.Open(sqlite.Open(dbPath), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB.Close() })

	faceRepo := repository.NewFaceRepository(db)
	builder := NewIdentityProfileBuilder(identityProfileBuilderConfig{}) // 生产默认

	type row struct {
		PersonID       uint   `json:"person_id"`
		InputFaces     int    `json:"input_faces"`
		ValidVectors   int    `json:"valid_vectors"`
		UsableEvidence int    `json:"usable_evidence"`
		Centers        int    `json:"centers"`
		ReasonCategory string `json:"reason_category"`
	}
	out := make([]row, 0, 3)
	for _, pid := range []uint{306601, 307945, 562545} {
		faces, err := faceRepo.ListProfileFaces(pid)
		require.NoError(t, err)

		build, err := builder.Build(pid, faces)
		require.NoError(t, err)
		require.NotNil(t, build)

		// 复用 preprocess 口径统计（不输出 embedding）。
		members := builder.preprocess(pid, faces)
		valid, usable := 0, 0
		for _, m := range members {
			if m.vec != nil {
				valid++
			}
			if m.eligible {
				usable++
			}
		}
		centers := 0
		if build.Centers != nil {
			centers = len(build.Centers)
		}
		reason := "usable"
		if centers == 0 {
			if len(faces) == 0 {
				reason = "no_input_faces"
			} else if valid == 0 {
				reason = "no_valid_vectors"
			} else if usable == 0 {
				reason = "no_usable_evidence"
			} else {
				reason = "no_usable_evidence" // 有 eligible 但仍未成中心（门槛/照片数等）
			}
		}
		out = append(out, row{
			PersonID:       pid,
			InputFaces:     len(faces),
			ValidVectors:   valid,
			UsableEvidence: usable,
			Centers:        centers,
			ReasonCategory: reason,
		})
	}
	b, err := json.MarshalIndent(out, "", "  ")
	require.NoError(t, err)
	t.Logf("§4.2 empty-profile builder verify:\n%s", string(b))
	if path := os.Getenv("RELIVE_EMPTY_PROFILE_SAMPLE_OUT"); path != "" {
		require.NoError(t, os.WriteFile(path, b, 0o600))
	}
}

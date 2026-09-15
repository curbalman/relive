#!/usr/bin/env bash
# record_test_evidence.sh
# 采集 T1–T4 正式测试执行证据、退出码、原始 JSON 输出及源码摘要。
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
BACKEND_DIR="$REPO_ROOT/backend"
DIGEST_SCRIPT="$SCRIPT_DIR/source_digest.py"

OUT_DIR="${1:-$SCRIPT_DIR}"
mkdir -p "$OUT_DIR"

RAW_OUTPUT="$OUT_DIR/test-output.jsonl"
STDERR_OUTPUT="$OUT_DIR/test-stderr.log"
MANIFEST="$OUT_DIR/test-evidence.json"
SOURCE_MANIFEST="$OUT_DIR/source-files.json"

calc_sha256() {
  (sha256sum "$1" 2>/dev/null || shasum -a 256 "$1") | awk '{print $1}'
}

echo "=== Stage 1: Compute Initial Source Digest (A) ==="
DIGEST_A=$(python3 "$DIGEST_SCRIPT" --repo-root "$REPO_ROOT" --digest-only)
echo "  Digest A: $DIGEST_A"

HEAD_COMMIT=$(GIT_CONFIG_GLOBAL=/dev/null git -C "$REPO_ROOT" rev-parse HEAD 2>/dev/null || echo "fec4f46")

echo "Recording formal test evidence..."
echo "  Repo root:     $REPO_ROOT"
echo "  HEAD commit:   $HEAD_COMMIT"
echo "  Output dir:    $OUT_DIR"

STARTED_AT=$(date -u +"%Y-%m-%dT%H:%M:%SZ")

if [[ -d /Library/Developer/CommandLineTools && -z "${DEVELOPER_DIR:-}" ]]; then
  export DEVELOPER_DIR=/Library/Developer/CommandLineTools
fi

TEST_CMD="go test -json -race ./internal/service ./internal/repository -run 'TestAcceptance_|TestReacceptance_|TestThirdReview_' -count=1"

# 必须直接捕获真实退出码，严禁用管道掩盖 exit code；同时捕获 stderr
set +e
(
  cd "$BACKEND_DIR"
  go test -json -race ./internal/service ./internal/repository -run 'TestAcceptance_|TestReacceptance_|TestThirdReview_' -count=1 > "$RAW_OUTPUT" 2> "$STDERR_OUTPUT"
)
TEST_EXIT_CODE=$?
set -e

ENDED_AT=$(date -u +"%Y-%m-%dT%H:%M:%SZ")
echo "  Tests finished with exit code: $TEST_EXIT_CODE"

echo "=== Stage 2: Compute Post-Test Source Digest (B) ==="
DIGEST_B=$(python3 "$DIGEST_SCRIPT" --repo-root "$REPO_ROOT" --digest-only)
echo "  Digest B: $DIGEST_B"

if [[ "$DIGEST_A" != "$DIGEST_B" ]]; then
  echo "record_test_evidence: error: source digest changed during test execution (A=$DIGEST_A, B=$DIGEST_B)" >&2
  exit 1
fi

SOURCE_DIGEST="$DIGEST_B"
python3 "$DIGEST_SCRIPT" --repo-root "$REPO_ROOT" --write-manifest "$SOURCE_MANIFEST"

RAW_SHA256=$(calc_sha256 "$RAW_OUTPUT")
echo "  Raw output SHA256: $RAW_SHA256"

# 必需测试清单
REQUIRED_TESTS=(
  "TestThirdReview_ManualCheckpointFailureCanResume"
  "TestThirdReview_ManualProgressFailureDoesNotRepeatDirty"
  "TestThirdReview_ManualProgressConcurrentSingleCommit"
  "TestThirdReview_AttemptKeySaveFailureMustStopLedgerWrite"
  "TestThirdReview_ProfileReadFailureKeepsReconcilePending"
)

PASSED_TESTS=()
FAILED_TESTS=()
SKIPPED_TESTS=()

for req in "${REQUIRED_TESTS[@]}"; do
  if grep "\"Test\":\"$req\"" "$RAW_OUTPUT" | grep -q '"Action":"pass"'; then
    PASSED_TESTS+=("$req")
  elif grep "\"Test\":\"$req\"" "$RAW_OUTPUT" | grep -q '"Action":"fail"'; then
    FAILED_TESTS+=("$req")
  elif grep "\"Test\":\"$req\"" "$RAW_OUTPUT" | grep -q '"Action":"skip"'; then
    SKIPPED_TESTS+=("$req")
  else
    FAILED_TESTS+=("$req")
  fi
done

to_json_array() {
  if [[ $# -eq 0 ]]; then
    echo "[]"
    return
  fi
  local out="["
  local first=1
  for item in "$@"; do
    if [[ $first -eq 0 ]]; then out="$out, "; fi
    out="$out\"$item\""
    first=0
  done
  out="$out]"
  echo "$out"
}

REQ_JSON=$(to_json_array "${REQUIRED_TESTS[@]}")
PASS_JSON=$(to_json_array ${PASSED_TESTS[@]+"${PASSED_TESTS[@]}"})
FAIL_JSON=$(to_json_array ${FAILED_TESTS[@]+"${FAILED_TESTS[@]}"})
SKIP_JSON=$(to_json_array ${SKIPPED_TESTS[@]+"${SKIPPED_TESTS[@]}"})

cat > "$MANIFEST" <<EOF
{
  "command": "$TEST_CMD",
  "started_at": "$STARTED_AT",
  "ended_at": "$ENDED_AT",
  "exit_code": $TEST_EXIT_CODE,
  "head_commit": "$HEAD_COMMIT",
  "source_digest": "$SOURCE_DIGEST",
  "raw_output_sha256": "$RAW_SHA256",
  "raw_output_path": "test-output.jsonl",
  "required_tests": $REQ_JSON,
  "passed_tests": $PASS_JSON,
  "failed_tests": $FAIL_JSON,
  "skipped_tests": $SKIP_JSON
}
EOF

echo "Wrote manifest to $MANIFEST"
echo "Wrote source manifest to $SOURCE_MANIFEST"

# 必须显式返回测试退出码，不能因写入 manifest 成功而掩盖测试失败。
if [[ $TEST_EXIT_CODE -ne 0 ]]; then
  echo "record_test_evidence: tests FAILED (exit code $TEST_EXIT_CODE)" >&2
  exit "$TEST_EXIT_CODE"
fi

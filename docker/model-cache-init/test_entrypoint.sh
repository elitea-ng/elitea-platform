#!/usr/bin/env bash
# Test suite for entrypoint.sh MD5 validation and --verify-only flag.
# Runs without Docker — uses a local temp directory and a mock HTTP server.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
ENTRYPOINT="$SCRIPT_DIR/entrypoint.sh"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
# The floor helper (issue #534). This suite runs from the repository, never
# from the image: docker/model-cache-init/.dockerignore keeps it out of the
# build context, so the path above always resolves for every caller it has.
# shellcheck source=../../scripts/lib/assertion-floor.sh
. "${REPO_ROOT}/scripts/lib/assertion-floor.sh"
TEST_DIR=""
PASS=0
FAIL=0

setup() {
    TEST_DIR=$(mktemp -d)
    export CACHE_DIR="$TEST_DIR/cache"
    export MANIFEST_PATH="$TEST_DIR/manifest.json"
    export MAX_RETRIES=2
    export VERIFY_ONLY=false
    mkdir -p "$CACHE_DIR"
}

teardown() {
    [ -n "$TEST_DIR" ] && rm -rf "$TEST_DIR"
}

# NOTE: an `assert_exit` helper used to sit here. Nothing called it, and its
# first `local actual=$?` read the status of `|| true` rather than of the
# command, so it had to run every command twice to get an answer. A broken
# helper that nothing calls is an invitation to call it. Removed with #484.

assert_contains() {
    local needle="$1"
    local haystack="$2"
    if echo "$haystack" | grep -q "$needle"; then
        PASS=$((PASS + 1))
        return 0
    fi
    FAIL=$((FAIL + 1))
    echo "FAIL: output does not contain '$needle'"
    echo "  GOT: $haystack"
    return 1
}

assert_file_exists() {
    if [ -f "$1" ]; then
        PASS=$((PASS + 1))
    else
        FAIL=$((FAIL + 1))
        echo "FAIL: file does not exist: $1"
    fi
}

assert_file_not_exists() {
    if [ ! -f "$1" ]; then
        PASS=$((PASS + 1))
    else
        FAIL=$((FAIL + 1))
        echo "FAIL: file should not exist: $1"
    fi
}

# A Prometheus gauge must carry a NUMBER. `du -sb ... | awk ... || echo 0`
# bound the fallback to awk, and awk succeeds on empty input, so the line was
# written as `model_cache_size_bytes ` with nothing after the space (#484).
# `assert_contains "model_cache_size_bytes"` could not see that, because the
# metric NAME was still there. This looks at the value.
assert_metric_is_numeric() {
    local metric="$1"
    local haystack="$2"
    if echo "$haystack" | grep -qE "^${metric} [0-9]+\$"; then
        PASS=$((PASS + 1))
        return 0
    fi
    FAIL=$((FAIL + 1))
    echo "FAIL: metric '${metric}' carries no numeric value"
    echo "  GOT: $(echo "$haystack" | grep -E "^${metric}( |\$)" || echo '<the metric is absent>')"
    return 0
}

# ---------- Test: Missing manifest ----------
test_missing_manifest() {
    echo "--- test_missing_manifest ---"
    setup
    export MANIFEST_PATH="$TEST_DIR/nonexistent.json"

    set +e
    output=$("$ENTRYPOINT" 2>&1)
    rc=$?
    set -e

    if [ "$rc" -ne 1 ]; then
        FAIL=$((FAIL + 1))
        echo "FAIL: expected exit 1, got $rc"
    else
        PASS=$((PASS + 1))
    fi
    assert_contains "Manifest not found" "$output"
    teardown
}

# ---------- Test: Invalid JSON manifest ----------
test_invalid_json_manifest() {
    echo "--- test_invalid_json_manifest ---"
    setup
    echo "not json" > "$MANIFEST_PATH"

    set +e
    output=$("$ENTRYPOINT" 2>&1)
    rc=$?
    set -e

    if [ "$rc" -ne 1 ]; then
        FAIL=$((FAIL + 1))
        echo "FAIL: expected exit 1, got $rc"
    else
        PASS=$((PASS + 1))
    fi
    assert_contains "not valid JSON" "$output"
    teardown
}

# ---------- Test: File with correct MD5 is skipped ----------
test_correct_md5_skipped() {
    echo "--- test_correct_md5_skipped ---"
    setup

    echo -n "hello world" > "$CACHE_DIR/test.txt"
    local expected_md5
    expected_md5=$(md5sum "$CACHE_DIR/test.txt" | awk '{print $1}')

    cat > "$MANIFEST_PATH" <<EOF
{
  "models": [
    {
      "name": "test-file",
      "url": "http://localhost:9999/test.txt",
      "path": "test.txt",
      "md5": "$expected_md5",
      "size_mb": 0
    }
  ]
}
EOF

    set +e
    output=$("$ENTRYPOINT" 2>&1)
    rc=$?
    set -e

    if [ "$rc" -ne 0 ]; then
        FAIL=$((FAIL + 1))
        echo "FAIL: expected exit 0, got $rc"
    else
        PASS=$((PASS + 1))
    fi
    assert_contains "already cached with correct MD5" "$output"
    assert_contains "skipped=1" "$output"
    teardown
}

# ---------- Test: File with wrong MD5 is deleted (and download fails → exit 1) ----------
test_wrong_md5_deleted() {
    echo "--- test_wrong_md5_deleted ---"
    setup

    echo -n "corrupt data" > "$CACHE_DIR/test.txt"

    cat > "$MANIFEST_PATH" <<EOF
{
  "models": [
    {
      "name": "test-file",
      "url": "http://localhost:9999/not-a-real-server.txt",
      "path": "test.txt",
      "md5": "0000000000000000000000000000dead",
      "size_mb": 0
    }
  ]
}
EOF

    set +e
    output=$("$ENTRYPOINT" 2>&1)
    rc=$?
    set -e

    if [ "$rc" -ne 1 ]; then
        FAIL=$((FAIL + 1))
        echo "FAIL: expected exit 1 (download fails), got $rc"
    else
        PASS=$((PASS + 1))
    fi
    assert_contains "MD5 MISMATCH" "$output"
    assert_contains "Deleting corrupt file" "$output"
    assert_file_not_exists "$CACHE_DIR/test.txt"
    teardown
}

# ---------- Test: --verify-only with valid file ----------
test_verify_only_valid() {
    echo "--- test_verify_only_valid ---"
    setup

    echo -n "verify me" > "$CACHE_DIR/data.bin"
    local expected_md5
    expected_md5=$(md5sum "$CACHE_DIR/data.bin" | awk '{print $1}')

    cat > "$MANIFEST_PATH" <<EOF
{
  "models": [
    {
      "name": "data-bundle",
      "url": "s3://bucket/data.bin",
      "path": "data.bin",
      "md5": "$expected_md5",
      "size_mb": 1
    }
  ]
}
EOF

    set +e
    output=$("$ENTRYPOINT" --verify-only 2>&1)
    rc=$?
    set -e

    if [ "$rc" -ne 0 ]; then
        FAIL=$((FAIL + 1))
        echo "FAIL: expected exit 0 (valid), got $rc"
    else
        PASS=$((PASS + 1))
    fi
    assert_contains "VALID" "$output"
    assert_contains "valid=1" "$output"
    assert_file_exists "$CACHE_DIR/data.bin"
    teardown
}

# ---------- Test: --verify-only with invalid file (does NOT delete) ----------
test_verify_only_invalid_no_delete() {
    echo "--- test_verify_only_invalid_no_delete ---"
    setup

    echo -n "bad content" > "$CACHE_DIR/data.bin"

    cat > "$MANIFEST_PATH" <<EOF
{
  "models": [
    {
      "name": "data-bundle",
      "url": "s3://bucket/data.bin",
      "path": "data.bin",
      "md5": "0000000000000000000000000000dead",
      "size_mb": 1
    }
  ]
}
EOF

    set +e
    output=$("$ENTRYPOINT" --verify-only 2>&1)
    rc=$?
    set -e

    if [ "$rc" -ne 1 ]; then
        FAIL=$((FAIL + 1))
        echo "FAIL: expected exit 1 (invalid), got $rc"
    else
        PASS=$((PASS + 1))
    fi
    assert_contains "MD5 MISMATCH" "$output"
    assert_contains "invalid=1" "$output"
    # Critically: verify-only does NOT delete the file
    assert_file_exists "$CACHE_DIR/data.bin"
    teardown
}

# ---------- Test: --verify-only with missing file ----------
test_verify_only_missing() {
    echo "--- test_verify_only_missing ---"
    setup

    cat > "$MANIFEST_PATH" <<EOF
{
  "models": [
    {
      "name": "absent-file",
      "url": "s3://bucket/absent.bin",
      "path": "absent.bin",
      "md5": "abc123",
      "size_mb": 10
    }
  ]
}
EOF

    set +e
    output=$("$ENTRYPOINT" --verify-only 2>&1)
    rc=$?
    set -e

    if [ "$rc" -ne 1 ]; then
        FAIL=$((FAIL + 1))
        echo "FAIL: expected exit 1 (missing), got $rc"
    else
        PASS=$((PASS + 1))
    fi
    assert_contains "MISSING" "$output"
    assert_contains "not found in cache" "$output"
    teardown
}

# ---------- Test: File with no MD5 (null) is skipped if exists ----------
test_null_md5_skipped() {
    echo "--- test_null_md5_skipped ---"
    setup

    echo -n "any content" > "$CACHE_DIR/nomd5.txt"

    cat > "$MANIFEST_PATH" <<EOF
{
  "models": [
    {
      "name": "no-md5",
      "url": "http://example.com/nomd5.txt",
      "path": "nomd5.txt",
      "md5": null,
      "size_mb": 0
    }
  ]
}
EOF

    set +e
    output=$("$ENTRYPOINT" 2>&1)
    rc=$?
    set -e

    if [ "$rc" -ne 0 ]; then
        FAIL=$((FAIL + 1))
        echo "FAIL: expected exit 0, got $rc"
    else
        PASS=$((PASS + 1))
    fi
    assert_contains "exists (no MD5 to verify)" "$output"
    assert_contains "skipped=1" "$output"
    teardown
}

# ---------- Test: --verify-only via env var ----------
test_verify_only_env_var() {
    echo "--- test_verify_only_env_var ---"
    setup
    export VERIFY_ONLY=true

    echo -n "content" > "$CACHE_DIR/file.dat"
    local expected_md5
    expected_md5=$(md5sum "$CACHE_DIR/file.dat" | awk '{print $1}')

    cat > "$MANIFEST_PATH" <<EOF
{
  "models": [
    {
      "name": "env-test",
      "url": "http://example.com/file.dat",
      "path": "file.dat",
      "md5": "$expected_md5",
      "size_mb": 0
    }
  ]
}
EOF

    set +e
    output=$("$ENTRYPOINT" 2>&1)
    rc=$?
    set -e

    if [ "$rc" -ne 0 ]; then
        FAIL=$((FAIL + 1))
        echo "FAIL: expected exit 0 (env var verify-only), got $rc"
    else
        PASS=$((PASS + 1))
    fi
    assert_contains "Verify only: true" "$output"
    assert_contains "VALID" "$output"
    teardown
}

# ---------- Test: Logging includes filename in MD5 messages ----------
test_log_includes_filename() {
    echo "--- test_log_includes_filename ---"
    setup

    echo -n "some data" > "$CACHE_DIR/myfile.bin"

    cat > "$MANIFEST_PATH" <<EOF
{
  "models": [
    {
      "name": "my-model",
      "url": "http://localhost:9999/myfile.bin",
      "path": "myfile.bin",
      "md5": "deadbeefdeadbeefdeadbeefdeadbeef",
      "size_mb": 5
    }
  ]
}
EOF

    set +e
    output=$("$ENTRYPOINT" 2>&1)
    rc=$?
    set -e

    # Should contain filename in the mismatch message
    assert_contains "file=myfile.bin" "$output"
    teardown
}

# ---------- Test: Empty models array is a FAILURE (issue #484) ----------
# This test read "expected exit 0 (empty models)" and asserted
# "Model cache sync complete". It stated the defect as the contract: a manifest
# with no entry produced an empty cache and a success. The container then
# started the pod, and the first model load failed at run time.
test_empty_models() {
    echo "--- test_empty_models ---"
    setup

    cat > "$MANIFEST_PATH" <<EOF
{
  "models": []
}
EOF

    set +e
    output=$("$ENTRYPOINT" 2>&1)
    rc=$?
    set -e

    if [ "$rc" -eq 0 ]; then
        FAIL=$((FAIL + 1))
        echo "FAIL: an empty 'models' array exited 0, so an empty cache reports success"
        echo "  OUT: $output"
    else
        PASS=$((PASS + 1))
    fi
    assert_contains "EMPTY 'models' array" "$output"
    teardown
}

# ---------- Test: A mis-keyed manifest is a FAILURE (issue #484) ----------
# `jq '.models | length'` prints 0 for a null, so a manifest that spells the
# key `model` passed the JSON check and reported a full cache.
test_mis_keyed_manifest() {
    echo "--- test_mis_keyed_manifest ---"
    setup

    cat > "$MANIFEST_PATH" <<EOF
{
  "version": "1.0.0",
  "model": [
    {
      "name": "one",
      "url": "s3://bucket/one.bin",
      "path": "one.bin",
      "md5": null,
      "size_mb": 1
    }
  ]
}
EOF

    set +e
    output=$("$ENTRYPOINT" 2>&1)
    rc=$?
    set -e

    if [ "$rc" -eq 0 ]; then
        FAIL=$((FAIL + 1))
        echo "FAIL: a manifest keyed 'model' exited 0"
        echo "  OUT: $output"
    else
        PASS=$((PASS + 1))
    fi
    # The message must name the key the script wanted AND the keys it found,
    # because a mis-keyed manifest and an empty one need different repairs.
    assert_contains "no 'models' array" "$output"
    assert_contains "version, model" "$output"
    teardown
}

# ---------- Test: --verify-only over an empty cache FAILS (issue #484) ----------
# The verify verdict used to be `FAILED -gt 0`. It now counts what the run
# proved against what the manifest holds, and prints that count.
test_verify_only_empty_cache() {
    echo "--- test_verify_only_empty_cache ---"
    setup

    cat > "$MANIFEST_PATH" <<EOF
{
  "models": [
    {
      "name": "one",
      "url": "s3://bucket/one.bin",
      "path": "one.bin",
      "md5": null,
      "size_mb": 1
    },
    {
      "name": "two",
      "url": "s3://bucket/two.bin",
      "path": "two.bin",
      "md5": null,
      "size_mb": 1
    }
  ]
}
EOF

    set +e
    output=$("$ENTRYPOINT" --verify-only 2>&1)
    rc=$?
    set -e

    if [ "$rc" -eq 0 ]; then
        FAIL=$((FAIL + 1))
        echo "FAIL: --verify-only exited 0 over an empty cache"
        echo "  OUT: $output"
    else
        PASS=$((PASS + 1))
    fi
    assert_contains "Files proved present and intact: 0 of 2" "$output"
    assert_contains "proved 0 of 2 manifest entries" "$output"
    teardown
}

# ---------- Test: every entry reports a result (issue #484) ----------
# The counters see only the entries that REACH them. This asserts the line
# that states the count, so a loop body that stops recording an outcome for
# some entry is visible in the output rather than only in an exit status.
test_result_count_reported() {
    echo "--- test_result_count_reported ---"
    setup

    echo -n "alpha" > "$CACHE_DIR/a.bin"
    echo -n "beta" > "$CACHE_DIR/b.bin"

    cat > "$MANIFEST_PATH" <<EOF
{
  "models": [
    { "name": "a", "url": "s3://bucket/a.bin", "path": "a.bin", "md5": null, "size_mb": 1 },
    { "name": "b", "url": "s3://bucket/b.bin", "path": "b.bin", "md5": null, "size_mb": 1 }
  ]
}
EOF

    set +e
    output=$("$ENTRYPOINT" 2>&1)
    rc=$?
    set -e

    if [ "$rc" -ne 0 ]; then
        FAIL=$((FAIL + 1))
        echo "FAIL: expected exit 0, got $rc"
        echo "  OUT: $output"
    else
        PASS=$((PASS + 1))
    fi
    assert_contains "Entries that reported a result: 2 of 2" "$output"
    teardown
}

# ---------- Test: Version match — incremental sync, files kept ----------
test_version_match_incremental() {
    echo "--- test_version_match_incremental ---"
    setup

    # Pre-populate version file and a cached file
    echo "1.0.0" > "$CACHE_DIR/.manifest-version"
    echo -n "cached data" > "$CACHE_DIR/model.bin"
    local expected_md5
    expected_md5=$(md5sum "$CACHE_DIR/model.bin" | awk '{print $1}')

    cat > "$MANIFEST_PATH" <<EOF
{
  "version": "1.0.0",
  "models": [
    {
      "name": "model",
      "url": "http://localhost:9999/model.bin",
      "path": "model.bin",
      "md5": "$expected_md5",
      "size_mb": 0
    }
  ]
}
EOF

    set +e
    output=$("$ENTRYPOINT" 2>&1)
    rc=$?
    set -e

    if [ "$rc" -ne 0 ]; then
        FAIL=$((FAIL + 1))
        echo "FAIL: expected exit 0, got $rc"
    else
        PASS=$((PASS + 1))
    fi
    assert_contains "Cache version matches: 1.0.0" "$output"
    assert_contains "skipped=1" "$output"
    assert_file_exists "$CACHE_DIR/model.bin"
    teardown
}

# ---------- Test: Version mismatch — cache cleared, re-download attempted ----------
test_version_mismatch_clears_cache() {
    echo "--- test_version_mismatch_clears_cache ---"
    setup

    # Pre-populate old version and a stale file
    echo "0.9.0" > "$CACHE_DIR/.manifest-version"
    echo -n "old cached data" > "$CACHE_DIR/stale.bin"

    cat > "$MANIFEST_PATH" <<EOF
{
  "version": "1.0.0",
  "models": [
    {
      "name": "model",
      "url": "http://localhost:9999/unavailable.bin",
      "path": "model.bin",
      "md5": null,
      "size_mb": 0
    }
  ]
}
EOF

    set +e
    output=$("$ENTRYPOINT" 2>&1)
    rc=$?
    set -e

    # Download will fail (no server), but we verify cache was cleared
    assert_contains "Cache version mismatch: cached=0.9.0 manifest=1.0.0" "$output"
    assert_contains "Clearing cache" "$output"
    assert_file_not_exists "$CACHE_DIR/stale.bin"
    teardown
}

# ---------- Test: No version file — performs full download ----------
test_no_version_file_full_download() {
    echo "--- test_no_version_file_full_download ---"
    setup

    echo -n "existing" > "$CACHE_DIR/model.bin"
    local expected_md5
    expected_md5=$(md5sum "$CACHE_DIR/model.bin" | awk '{print $1}')

    cat > "$MANIFEST_PATH" <<EOF
{
  "version": "2.0.0",
  "models": [
    {
      "name": "model",
      "url": "http://localhost:9999/model.bin",
      "path": "model.bin",
      "md5": "$expected_md5",
      "size_mb": 0
    }
  ]
}
EOF

    set +e
    output=$("$ENTRYPOINT" 2>&1)
    rc=$?
    set -e

    if [ "$rc" -ne 0 ]; then
        FAIL=$((FAIL + 1))
        echo "FAIL: expected exit 0, got $rc"
    else
        PASS=$((PASS + 1))
    fi
    assert_contains "No cached version found" "$output"
    # File still exists (not cleared because there's no old version to compare)
    assert_file_exists "$CACHE_DIR/model.bin"
    # Version file written after success
    assert_file_exists "$CACHE_DIR/.manifest-version"
    # Check version file content
    local written_version
    written_version=$(cat "$CACHE_DIR/.manifest-version")
    if [ "$written_version" = "2.0.0" ]; then
        PASS=$((PASS + 1))
    else
        FAIL=$((FAIL + 1))
        echo "FAIL: expected version file '2.0.0', got '$written_version'"
    fi
    teardown
}

# ---------- Test: Version written after successful sync ----------
test_version_written_on_success() {
    echo "--- test_version_written_on_success ---"
    setup

    echo -n "data" > "$CACHE_DIR/f.bin"
    local expected_md5
    expected_md5=$(md5sum "$CACHE_DIR/f.bin" | awk '{print $1}')

    cat > "$MANIFEST_PATH" <<EOF
{
  "version": "3.1.0",
  "models": [
    {
      "name": "f",
      "url": "http://localhost:9999/f.bin",
      "path": "f.bin",
      "md5": "$expected_md5",
      "size_mb": 0
    }
  ]
}
EOF

    set +e
    output=$("$ENTRYPOINT" 2>&1)
    rc=$?
    set -e

    if [ "$rc" -ne 0 ]; then
        FAIL=$((FAIL + 1))
        echo "FAIL: expected exit 0, got $rc"
    else
        PASS=$((PASS + 1))
    fi
    assert_contains "Wrote cache version: 3.1.0" "$output"
    assert_file_exists "$CACHE_DIR/.manifest-version"
    local v
    v=$(cat "$CACHE_DIR/.manifest-version")
    if [ "$v" = "3.1.0" ]; then
        PASS=$((PASS + 1))
    else
        FAIL=$((FAIL + 1))
        echo "FAIL: version file content mismatch: expected 3.1.0, got $v"
    fi
    teardown
}

# ---------- Test: Version NOT written on failure ----------
test_version_not_written_on_failure() {
    echo "--- test_version_not_written_on_failure ---"
    setup

    cat > "$MANIFEST_PATH" <<EOF
{
  "version": "4.0.0",
  "models": [
    {
      "name": "fail-model",
      "url": "http://localhost:9999/no-such-file.bin",
      "path": "fail.bin",
      "md5": "abc123",
      "size_mb": 0
    }
  ]
}
EOF

    set +e
    output=$("$ENTRYPOINT" 2>&1)
    rc=$?
    set -e

    if [ "$rc" -ne 1 ]; then
        FAIL=$((FAIL + 1))
        echo "FAIL: expected exit 1, got $rc"
    else
        PASS=$((PASS + 1))
    fi
    # Version file should NOT be written since the sync failed
    assert_file_not_exists "$CACHE_DIR/.manifest-version"
    teardown
}

# ---------- Test: Verify-only does NOT check or write version ----------
test_verify_only_ignores_version() {
    echo "--- test_verify_only_ignores_version ---"
    setup

    # Stale version — verify-only should not clear cache or write version
    echo "0.1.0" > "$CACHE_DIR/.manifest-version"
    echo -n "data" > "$CACHE_DIR/v.bin"
    local expected_md5
    expected_md5=$(md5sum "$CACHE_DIR/v.bin" | awk '{print $1}')

    cat > "$MANIFEST_PATH" <<EOF
{
  "version": "9.9.9",
  "models": [
    {
      "name": "v",
      "url": "s3://bucket/v.bin",
      "path": "v.bin",
      "md5": "$expected_md5",
      "size_mb": 0
    }
  ]
}
EOF

    set +e
    output=$("$ENTRYPOINT" --verify-only 2>&1)
    rc=$?
    set -e

    if [ "$rc" -ne 0 ]; then
        FAIL=$((FAIL + 1))
        echo "FAIL: expected exit 0, got $rc"
    else
        PASS=$((PASS + 1))
    fi
    # File should still exist (not cleared despite version mismatch)
    assert_file_exists "$CACHE_DIR/v.bin"
    # Version file should still have old version (not overwritten)
    local v
    v=$(cat "$CACHE_DIR/.manifest-version")
    if [ "$v" = "0.1.0" ]; then
        PASS=$((PASS + 1))
    else
        FAIL=$((FAIL + 1))
        echo "FAIL: verify-only should not change version file, got: $v"
    fi
    teardown
}

# ---------- Test: No version in manifest — skips versioning logic ----------
test_no_version_in_manifest() {
    echo "--- test_no_version_in_manifest ---"
    setup

    echo -n "cached" > "$CACHE_DIR/x.bin"
    local expected_md5
    expected_md5=$(md5sum "$CACHE_DIR/x.bin" | awk '{print $1}')

    cat > "$MANIFEST_PATH" <<EOF
{
  "models": [
    {
      "name": "x",
      "url": "http://localhost:9999/x.bin",
      "path": "x.bin",
      "md5": "$expected_md5",
      "size_mb": 0
    }
  ]
}
EOF

    set +e
    output=$("$ENTRYPOINT" 2>&1)
    rc=$?
    set -e

    if [ "$rc" -ne 0 ]; then
        FAIL=$((FAIL + 1))
        echo "FAIL: expected exit 0, got $rc"
    else
        PASS=$((PASS + 1))
    fi
    # No version-related logs
    if echo "$output" | grep -q "version"; then
        FAIL=$((FAIL + 1))
        echo "FAIL: should not mention version when manifest has no version field"
    else
        PASS=$((PASS + 1))
    fi
    # No version file written
    assert_file_not_exists "$CACHE_DIR/.manifest-version"
    teardown
}

# ---------- Test: Metrics file created on successful sync ----------
test_metrics_written_on_success() {
    echo "--- test_metrics_written_on_success ---"
    setup

    echo -n "data" > "$CACHE_DIR/m.bin"
    local expected_md5
    expected_md5=$(md5sum "$CACHE_DIR/m.bin" | awk '{print $1}')

    cat > "$MANIFEST_PATH" <<EOF
{
  "models": [
    {
      "name": "m",
      "url": "http://localhost:9999/m.bin",
      "path": "m.bin",
      "md5": "$expected_md5",
      "size_mb": 0
    }
  ]
}
EOF

    set +e
    output=$("$ENTRYPOINT" 2>&1)
    rc=$?
    set -e

    if [ "$rc" -ne 0 ]; then
        FAIL=$((FAIL + 1))
        echo "FAIL: expected exit 0, got $rc"
    else
        PASS=$((PASS + 1))
    fi
    assert_file_exists "$CACHE_DIR/.metrics"
    assert_contains "Metrics written" "$output"
    teardown
}

# ---------- Test: Metrics file contains Prometheus textfile format ----------
test_metrics_prometheus_format() {
    echo "--- test_metrics_prometheus_format ---"
    setup

    echo -n "content" > "$CACHE_DIR/p.bin"
    local expected_md5
    expected_md5=$(md5sum "$CACHE_DIR/p.bin" | awk '{print $1}')

    cat > "$MANIFEST_PATH" <<EOF
{
  "models": [
    {
      "name": "p",
      "url": "http://localhost:9999/p.bin",
      "path": "p.bin",
      "md5": "$expected_md5",
      "size_mb": 1
    }
  ]
}
EOF

    set +e
    "$ENTRYPOINT" >/dev/null 2>&1
    rc=$?
    set -e

    if [ "$rc" -ne 0 ]; then
        FAIL=$((FAIL + 1))
        echo "FAIL: expected exit 0, got $rc"
        teardown
        return
    fi

    local metrics_content
    metrics_content=$(cat "$CACHE_DIR/.metrics")

    assert_contains "# HELP model_cache_download_duration_seconds" "$metrics_content"
    assert_contains "# TYPE model_cache_download_duration_seconds gauge" "$metrics_content"
    assert_contains "model_cache_download_duration_seconds" "$metrics_content"
    assert_contains "# HELP model_cache_size_bytes" "$metrics_content"
    assert_contains "model_cache_size_bytes" "$metrics_content"
    assert_contains "# HELP model_cache_files_total" "$metrics_content"
    assert_contains "model_cache_files_total" "$metrics_content"
    assert_contains "# HELP model_cache_errors_total" "$metrics_content"
    assert_contains "model_cache_errors_total 0" "$metrics_content"
    assert_contains "model_cache_files_skipped 1" "$metrics_content"
    assert_contains "model_cache_files_downloaded 0" "$metrics_content"
    assert_contains "model_cache_manifest_files_total 1" "$metrics_content"
    assert_contains "model_cache_last_sync_timestamp_seconds" "$metrics_content"
    teardown
}

# ---------- Test: Metrics.sh standalone ----------
test_metrics_standalone() {
    echo "--- test_metrics_standalone ---"
    setup

    export CACHE_DIR
    export MANIFEST_PATH
    mkdir -p "$CACHE_DIR/subdir"
    echo -n "file1" > "$CACHE_DIR/one.bin"
    echo -n "file2" > "$CACHE_DIR/subdir/two.bin"

    set +e
    output=$(bash "$SCRIPT_DIR/metrics.sh" 42 3 5 1 9 2>&1)
    rc=$?
    set -e

    if [ "$rc" -ne 0 ]; then
        FAIL=$((FAIL + 1))
        echo "FAIL: metrics.sh expected exit 0, got $rc"
        teardown
        return
    fi

    assert_file_exists "$CACHE_DIR/.metrics"
    local metrics_content
    metrics_content=$(cat "$CACHE_DIR/.metrics")

    assert_contains "model_cache_download_duration_seconds 42" "$metrics_content"
    assert_contains "model_cache_files_downloaded 3" "$metrics_content"
    assert_contains "model_cache_files_skipped 5" "$metrics_content"
    assert_contains "model_cache_errors_total 1" "$metrics_content"
    assert_contains "model_cache_manifest_files_total 9" "$metrics_content"
    # Should count 2 actual files (excludes .metrics)
    assert_contains "model_cache_files_total 2" "$metrics_content"

    # ── Issue #484 ───────────────────────────────────────────────────────────
    # Every gauge must carry a number, and each measurement must say whether it
    # ran. `du -sb` is absent on BSD and on plain BusyBox, so the size gauge is
    # unmeasured on such a host and measured inside the Debian image, which
    # ships GNU coreutils. The value that must NEVER appear is an empty one.
    assert_metric_is_numeric "model_cache_size_bytes" "$metrics_content"
    assert_metric_is_numeric "model_cache_size_bytes_measured" "$metrics_content"
    assert_metric_is_numeric "model_cache_files_total_measured" "$metrics_content"
    # The file count needs no external tool, so it is measured on every host.
    assert_contains "model_cache_files_total_measured 1" "$metrics_content"
    teardown
}

# ---------- Test: Metrics not written when entrypoint fails ----------
test_metrics_not_on_failure() {
    echo "--- test_metrics_not_on_failure ---"
    setup

    cat > "$MANIFEST_PATH" <<EOF
{
  "models": [
    {
      "name": "fail-model",
      "url": "http://localhost:9999/no-server.bin",
      "path": "fail.bin",
      "md5": "abc123",
      "size_mb": 0
    }
  ]
}
EOF

    set +e
    output=$("$ENTRYPOINT" 2>&1)
    rc=$?
    set -e

    if [ "$rc" -ne 1 ]; then
        FAIL=$((FAIL + 1))
        echo "FAIL: expected exit 1, got $rc"
    else
        PASS=$((PASS + 1))
    fi
    # Metrics file should NOT exist because die() exits before metrics are written
    assert_file_not_exists "$CACHE_DIR/.metrics"
    teardown
}

# --- Run all tests ---
#
# ── The floor (issue #484) ───────────────────────────────────────────────────
#
# This runner used to end at `if [ "$FAIL" -gt 0 ]`. Delete every call below
# and the suite exits 0 with `0 passed, 0 failed`, which is the same shape as
# the defect it tests for. EXPECTED_ASSERTIONS holds the number of assertions
# this file makes, and any other result count turns the run red.
#
# A counter sees only the assertions that REACH it. A test function that
# returns early — every `teardown; return` path below does exactly that —
# raises fewer results than it declares, and only a floor catches it.
#
# The number is DERIVED from this file, not written down (issue #534, item (a)
# of #529). A stated number is true only when the pull request merges: an
# assertion that a later merge ADDS, with the number left alone, makes the
# floor under-count in silence for ever.
#
# An assertion site here is one call to an assert_ helper, or one inline pass
# counter inside a test function. The helper DEFINITIONS hold an inline pass
# counter too, so the range starts at the first test function. Read
# scripts/lib/assertion-floor.sh.
ASSERTION_SITE_PATTERN='^[[:space:]]+assert_[a-z_]+[[:space:]]|PASS=\$\(\(PASS \+ 1\)\)'
ASSERTION_SITE_RANGE='/^# ---------- Test: /,$'
EXPECTED_ASSERTIONS="$(derive_assertion_floor "$0" "$ASSERTION_SITE_PATTERN" "$ASSERTION_SITE_RANGE")"

echo "=== Running entrypoint.sh tests ==="
echo ""

test_missing_manifest
test_invalid_json_manifest
test_correct_md5_skipped
test_wrong_md5_deleted
test_verify_only_valid
test_verify_only_invalid_no_delete
test_verify_only_missing
test_null_md5_skipped
test_verify_only_env_var
test_log_includes_filename
test_empty_models
test_mis_keyed_manifest
test_verify_only_empty_cache
test_result_count_reported
test_version_match_incremental
test_version_mismatch_clears_cache
test_no_version_file_full_download
test_version_written_on_success
test_version_not_written_on_failure
test_verify_only_ignores_version
test_no_version_in_manifest
test_metrics_written_on_success
test_metrics_prometheus_format
test_metrics_standalone
test_metrics_not_on_failure

RAN=$((PASS + FAIL))

echo ""
echo "=== Results: $PASS passed, $FAIL failed; $RAN of $EXPECTED_ASSERTIONS expected assertions reported a result ==="

if [ "$RAN" -ne "$EXPECTED_ASSERTIONS" ]; then
    echo "FAILED: $RAN assertion(s) reported a result, and this file holds $EXPECTED_ASSERTIONS assertion site(s)." >&2
    echo "  An assertion that reports nothing is not a passed assertion. A test that" >&2
    echo "  returned early, or a deleted call, takes its assertions with it." >&2
    echo "  The floor is derived from this file, so there is no number to move:" >&2
    echo "  find the site that reported nothing." >&2
    exit 1
fi

if [ "$FAIL" -gt 0 ]; then
    exit 1
fi
exit 0

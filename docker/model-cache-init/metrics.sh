#!/usr/bin/env bash
# Writes Prometheus textfile-format metrics about model cache state.
# Called by entrypoint.sh after sync completes.
# Metrics file at $CACHE_DIR/.metrics can be exposed via node-exporter textfile collector.

set -euo pipefail

CACHE_DIR="${CACHE_DIR:-/cache}"
METRICS_FILE="${CACHE_DIR}/.metrics"
MANIFEST_PATH="${MANIFEST_PATH:-/config/manifest.json}"

# Arguments passed from entrypoint:
#   $1 = duration_seconds (total sync wall time)
#   $2 = downloaded count
#   $3 = skipped count
#   $4 = failed count
#   $5 = total count
DURATION="${1:-0}"
DOWNLOADED="${2:-0}"
SKIPPED="${3:-0}"
FAILED="${4:-0}"
TOTAL="${5:-0}"

# ── Two measurements, and a flag that says whether each one ran (issue #484) ──
#
# `du -sb ... | awk '{print $1}' || echo "0"` binds the `||` to awk, not to du.
# `du -sb` fails on BSD and on plain BusyBox. awk then succeeds with no input,
# so the fallback never runs, and the script writes
# `model_cache_size_bytes ` — a Prometheus line with an EMPTY value. A scrape
# reads that as a parse error, not as a failed measurement. The Debian image
# installs coreutils, so this is dormant in the image and live for a local run.
# `find ... | wc -l` has the same shape: the status of a pipeline is the status
# of its LAST element, so a failing find gives a confident 0.
#
# Each measurement now takes the status of the command that can fail, and each
# carries a companion 0/1 gauge. Prometheus has no "unknown", so without the
# flag an unmeasured cache and an empty cache both scrape as 0.
warn() { echo "[$(date -u '+%Y-%m-%dT%H:%M:%SZ')] WARNING: $1" >&2; }

is_count() {
    case "${1:-}" in
        '' | *[!0-9]*) return 1 ;;
        *) return 0 ;;
    esac
}

# Total cache size on disk.
CACHE_SIZE_BYTES=0
CACHE_SIZE_MEASURED=0
if [ -d "$CACHE_DIR" ]; then
    if DU_OUTPUT=$(du -sb "$CACHE_DIR" 2>/dev/null); then
        DU_BYTES=$(printf '%s\n' "$DU_OUTPUT" | awk '{print $1; exit}')
        if is_count "$DU_BYTES"; then
            CACHE_SIZE_BYTES="$DU_BYTES"
            CACHE_SIZE_MEASURED=1
        else
            warn "du -sb $CACHE_DIR printed no byte count, so model_cache_size_bytes is unmeasured"
        fi
    else
        warn "du -sb failed on $CACHE_DIR (BSD or BusyBox du has no -b), so model_cache_size_bytes is unmeasured"
    fi
fi

# Total files, excluding dotfiles such as .metrics and .manifest-version.
FILES_TOTAL=0
FILES_TOTAL_MEASURED=0
if [ -d "$CACHE_DIR" ]; then
    if FIND_OUTPUT=$(find "$CACHE_DIR" -type f -not -name '.*' 2>/dev/null); then
        FOUND=$(printf '%s\n' "$FIND_OUTPUT" | awk 'NF { n++ } END { print n + 0 }')
        if is_count "$FOUND"; then
            FILES_TOTAL="$FOUND"
            FILES_TOTAL_MEASURED=1
        else
            warn "the file count of $CACHE_DIR did not parse, so model_cache_files_total is unmeasured"
        fi
    else
        warn "find failed on $CACHE_DIR, so model_cache_files_total is unmeasured"
    fi
fi

# Write Prometheus textfile format
cat > "$METRICS_FILE" <<EOF
# HELP model_cache_download_duration_seconds Total time spent downloading model cache files.
# TYPE model_cache_download_duration_seconds gauge
model_cache_download_duration_seconds ${DURATION}

# HELP model_cache_size_bytes Total size of the model cache directory in bytes.
# TYPE model_cache_size_bytes gauge
model_cache_size_bytes ${CACHE_SIZE_BYTES}

# HELP model_cache_size_bytes_measured 1 when model_cache_size_bytes was measured, 0 when the measurement could not run.
# TYPE model_cache_size_bytes_measured gauge
model_cache_size_bytes_measured ${CACHE_SIZE_MEASURED}

# HELP model_cache_files_total Total number of cached model files on disk.
# TYPE model_cache_files_total gauge
model_cache_files_total ${FILES_TOTAL}

# HELP model_cache_files_total_measured 1 when model_cache_files_total was measured, 0 when the measurement could not run.
# TYPE model_cache_files_total_measured gauge
model_cache_files_total_measured ${FILES_TOTAL_MEASURED}

# HELP model_cache_files_downloaded Number of files downloaded in the last sync.
# TYPE model_cache_files_downloaded gauge
model_cache_files_downloaded ${DOWNLOADED}

# HELP model_cache_files_skipped Number of files skipped (already cached) in the last sync.
# TYPE model_cache_files_skipped gauge
model_cache_files_skipped ${SKIPPED}

# HELP model_cache_errors_total Number of files that failed to download in the last sync.
# TYPE model_cache_errors_total gauge
model_cache_errors_total ${FAILED}

# HELP model_cache_manifest_files_total Total number of files defined in the manifest.
# TYPE model_cache_manifest_files_total gauge
model_cache_manifest_files_total ${TOTAL}

# HELP model_cache_last_sync_timestamp_seconds Unix timestamp of the last successful cache sync.
# TYPE model_cache_last_sync_timestamp_seconds gauge
model_cache_last_sync_timestamp_seconds $(date +%s)
EOF

echo "[$(date -u '+%Y-%m-%dT%H:%M:%SZ')] Metrics written to $METRICS_FILE"

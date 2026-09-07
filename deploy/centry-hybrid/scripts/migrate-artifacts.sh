#!/usr/bin/env bash
#
# Copy the current platform's artifact objects into the Go object store (#337).
#
# WHY A COPY AND NOT A CONFIGURATION CHANGE. The current platform stores objects
# through Apache libcloud with the LOCAL driver: a directory tree on pylon_main's
# own disk, where BOTH the container directory name and the object file name are
# url-safe base64 of the real name (legacy plugins/shared/tools/storage_engines/
# libcloud.py, and `storage_libcloud_encoder: base64` in the deployed
# shared.yml). That is not an S3, Azure or GCS endpoint, so no setting of
# elitea-main can read it in place — internal/infra/storage has no filesystem
# backend and ADR-0016 SS8 says it never will. The objects move once.
#
# THE MAPPING, and it is the whole script:
#
#   source  <root>/<b64u("p--<projectID>.<bucket>")>/<b64u("<key>")>
#   target  s3://<container>/[<key-prefix>/]p/<projectID>/b/<bucket>/o/<key>
#
# The target layout is ObjectRef.StorageKey in
# services/elitea-main/internal/infra/storage/ref.go. It is read from that file
# rather than restated from memory: get it wrong and every object lands where
# nothing looks for it, and every read answers 404 while the copy reports
# success.
#
# WHAT THIS SCRIPT DOES NOT DO, on purpose:
#
#   * it creates no BUCKET ROWS. elitea-main resolves a bucket through its own
#     database (requireS3Bucket in internal/api/v2/artifacts/s3.go) before it
#     touches the store, so an object copied under a bucket with no row answers
#     NoSuchBucket. deploy/ARTIFACT_CUTOVER.md step 3 creates the rows through
#     the public API, which is the surface that also writes the row's owner,
#     retention and permissions. Writing them here would mean this script
#     inventing values the API computes.
#   * it deletes nothing from the source. The source tree is the rollback.
#
# It uses `mc` (docker.io/minio/mc), which is the client the compose stacks
# already use to create the bucket (rustfs-bucket-init in
# deploy/docker-compose.standalone-full.yml and runtime-artifacts-bucket-init in
# deploy/centry-hybrid/pov-compose.yml). One client for the store, not two.
set -euo pipefail

usage() {
  cat >&2 <<'USAGE'
usage: migrate-artifacts.sh --source <libcloud-storage-root> --alias <mc-alias>
                            [--container <bucket>] [--key-prefix <prefix>]
                            [--project <id>] [--dry-run] [--verify-only]

  --source       The libcloud LOCAL storage root: the directory named by
                 settings.storage_libcloud_params.kwargs.key in the deployed
                 shared.yml (the reference deployment uses
                 /data/libcloud/storage, which is <centry>/pylon_main/libcloud/
                 storage on the host).
  --alias        An `mc` alias that already addresses the target store, e.g.
                 `mc alias set hybrid http://127.0.0.1:9000 elitea <secret>`.
  --container    Target bucket. Default: elitea-artifacts. It must equal
                 STORAGE_CONTAINER on elitea-main.
  --key-prefix   Target key prefix. Default: empty. It must equal
                 STORAGE_KEY_PREFIX on elitea-main.
  --project      Copy only this project id. Repeatable.
  --dry-run      Print every mapping and copy nothing. Exits 0.
  --verify-only  Skip the copy and run the count comparison alone.
USAGE
  exit 2
}

source_root=""
alias_name=""
container="elitea-artifacts"
key_prefix=""
projects=()
dry_run=0
verify_only=0

while [[ $# -gt 0 ]]; do
  case "$1" in
    --source) source_root="${2:?--source needs a value}"; shift 2 ;;
    --alias) alias_name="${2:?--alias needs a value}"; shift 2 ;;
    --container) container="${2:?--container needs a value}"; shift 2 ;;
    --key-prefix) key_prefix="${2:?--key-prefix needs a value}"; shift 2 ;;
    --project) projects+=("${2:?--project needs a value}"); shift 2 ;;
    --dry-run) dry_run=1; shift ;;
    --verify-only) verify_only=1; shift ;;
    -h|--help) usage ;;
    *) echo "unknown argument: $1" >&2; usage ;;
  esac
done

[[ -n "$source_root" ]] || usage
[[ -n "$alias_name" ]] || usage
if [[ ! -d "$source_root" ]]; then
  echo "source root does not exist: $source_root" >&2
  exit 2
fi
if ! command -v mc >/dev/null 2>&1; then
  echo "mc is not on PATH. Install the MinIO client, or run this script inside" >&2
  echo "docker.io/minio/mc with the source root and ~/.mc mounted." >&2
  exit 2
fi
if ! command -v python3 >/dev/null 2>&1; then
  echo "python3 is not on PATH. It decodes the base64 names; there is no" >&2
  echo "portable shell equivalent for url-safe base64 with restored padding." >&2
  exit 2
fi

target_root="$alias_name/$container"
if [[ -n "$key_prefix" ]]; then
  target_root="$target_root/$key_prefix"
fi

# ── name decoding ────────────────────────────────────────────────────────────
#
# url-safe base64, and the encoder emits no padding for some lengths, so the
# padding is restored before decoding. A name that does not decode is REPORTED
# and skipped, never guessed at: the LOCAL driver also writes its own metadata
# files into the tree, and a silently "decoded" metadata file would be copied
# as though it were a user object.
#
# The decoder is passed with -c and NOT through a here-document: a
# here-document IS the interpreter's standard input, so a `python3 - <<EOF`
# form reads its own source and then finds standard input already at end of
# file. The names to decode arrive on standard input, so the two cannot share
# it. (Measured: that form decoded nothing and the walk then reported an empty
# source tree, which is the same output a wrong --source produces.)
decode_names_program='
import base64
import sys

for line in sys.stdin.read().splitlines():
    if not line:
        continue
    padded = line + "=" * (-len(line) % 4)
    try:
        decoded = base64.urlsafe_b64decode(padded.encode()).decode()
    except Exception:
        print("!\t" + line)
        continue
    if not decoded or "\n" in decoded:
        print("!\t" + line)
        continue
    print(decoded + "\t" + line)
'

decode_names() {
  python3 -c "$decode_names_program"
}

selected_project() {
  local id="$1"
  if [[ ${#projects[@]} -eq 0 ]]; then
    return 0
  fi
  local wanted
  for wanted in "${projects[@]}"; do
    [[ "$wanted" == "$id" ]] && return 0
  done
  return 1
}

copied=0
skipped=0
containers_seen=0

# ── the walk ─────────────────────────────────────────────────────────────────
while IFS=$'\t' read -r decoded encoded; do
  containers_seen=$((containers_seen + 1))
  if [[ "$decoded" == "!" ]]; then
    echo "skip container: ${encoded} does not decode as a base64 name" >&2
    skipped=$((skipped + 1))
    continue
  fi
  # p--<projectID>.<bucket>. The prefix is EngineBase.bucket_prefix in the
  # legacy engine; anything else in the tree belongs to another consumer.
  if [[ ! "$decoded" =~ ^p--([1-9][0-9]*)\.(.+)$ ]]; then
    echo "skip container: ${decoded} is not a project bucket (no p--<id>. prefix)" >&2
    skipped=$((skipped + 1))
    continue
  fi
  project_id="${BASH_REMATCH[1]}"
  bucket="${BASH_REMATCH[2]}"

  if ! selected_project "$project_id"; then
    continue
  fi

  # The target validates the bucket name (bucketPattern in
  # internal/infra/storage/ref.go: a lower-case letter, then lower-case
  # letters, digits and hyphens, 2..63 characters). The legacy side never
  # applied that rule, so a bucket named with an underscore or a capital
  # exists in the source and CANNOT be addressed through the Go API. Report
  # it here, where an operator can rename it before the cutover, rather than
  # copying objects nobody can then read.
  if [[ ! "$bucket" =~ ^[a-z][a-z0-9-]{1,62}$ ]]; then
    echo "REFUSED bucket: project ${project_id} bucket ${bucket} is not addressable" >&2
    echo "  through the Go artifact API. Rename it in the current platform first." >&2
    skipped=$((skipped + 1))
    continue
  fi

  while IFS=$'\t' read -r file_name file_encoded; do
    if [[ "$file_name" == "!" ]]; then
      echo "skip object: ${encoded}/${file_encoded} does not decode" >&2
      skipped=$((skipped + 1))
      continue
    fi
    target="$target_root/p/$project_id/b/$bucket/o/$file_name"
    if [[ "$dry_run" -eq 1 || "$verify_only" -eq 1 ]]; then
      printf '%s -> %s\n' "$source_root/$encoded/$file_encoded" "$target"
      copied=$((copied + 1))
      continue
    fi
    mc cp --quiet "$source_root/$encoded/$file_encoded" "$target"
    copied=$((copied + 1))
  done < <(find "$source_root/$encoded" -maxdepth 1 -type f -exec basename {} \; | decode_names)
done < <(find "$source_root" -mindepth 1 -maxdepth 1 -type d -exec basename {} \; | decode_names)

# A source root that held nothing is a real state (a deployment whose artifacts
# were never used), and it is ALSO what a wrong --source looks like. They are
# not the same, so say which one this was rather than reporting a clean run.
if [[ "$containers_seen" -eq 0 ]]; then
  echo "no container directory under $source_root." >&2
  echo "Either this deployment stored no artifacts, or --source is wrong." >&2
  echo "The libcloud root is the directory that holds one directory per bucket." >&2
  exit 1
fi

if [[ "$dry_run" -eq 1 ]]; then
  printf 'dry run: %d object(s) would be copied, %d entr(y|ies) skipped.\n' "$copied" "$skipped"
  exit 0
fi

# ── verification ─────────────────────────────────────────────────────────────
#
# Count the objects the target now holds under the migrated prefixes and
# compare with the number the walk produced. A copy loop that reports success
# per object still leaves the question "are they all there", and `mc cp` is the
# only thing that has answered it so far.
listed=0
while IFS= read -r line; do
  [[ -n "$line" ]] && listed=$((listed + 1))
done < <(mc ls --recursive --quiet "$target_root/p/" 2>/dev/null | awk '{ print $NF }')

printf 'source objects walked: %d\n' "$copied"
printf 'target objects listed: %d\n' "$listed"
printf 'entries skipped:       %d\n' "$skipped"

if [[ "$listed" -lt "$copied" ]]; then
  echo "the target holds FEWER objects than the source walk produced." >&2
  echo "Do not cut over. Re-run, then compare again." >&2
  exit 1
fi
echo "artifact copy verified."

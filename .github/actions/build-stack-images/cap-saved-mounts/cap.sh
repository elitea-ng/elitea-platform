#!/usr/bin/env bash
# cap.sh — trim the extracted BuildKit mounts to CAP_MB before they are saved.
#
# A writer restores the previous entry, builds, extracts the WHOLE mount and
# saves it. Cargo names a dependency artefact by a hash of its version and
# flags, so every Cargo.lock change leaves the old artefacts behind and the
# entry only grows (the single rust entry was 3.55 GB on 2026-10-20).
#
# POLICY: above the cap, drop the LARGEST cargo-target-<image> directory, and
# repeat. That image then builds cold ONCE, on the next main push, and is
# saved holding only the artefacts its current lockfile uses: a reset, not a
# per-run cost. Finer pruning (stale fingerprints) needs to know which
# artefacts this build USED; cargo records no such thing and restored mtimes
# and atimes cannot tell "reused" from "stale", so a guess there would delete
# live artefacts and make every image a little cold on every run. The
# registry, git and Go caches are never dropped: they are small and shared.
#
# A failure here must not fail the job: the image is built and tested
# already, and the worst case of skipping the cap is a larger entry.
set -uo pipefail
root="${MOUNTS_DIR:?}"
cap="${CAP_MB:?}"
[ -d "$root" ] || { echo "cap: $root does not exist; nothing to trim"; exit 0; }
total() { du -sm "$root" | cut -f1; }
echo "cap: extracted mounts are $(total) MB (cap ${cap} MB)"
du -sm "$root"/* 2>/dev/null | sort -rn
while [ "$(total)" -gt "$cap" ]; do
  biggest="$(du -sm "$root"/cargo-target-* 2>/dev/null | sort -rn | head -1 | cut -f2)"
  if [ -z "$biggest" ]; then
    echo "::warning::cap: mounts are $(total) MB with no cargo target directory left to drop"
    break
  fi
  echo "cap: dropping $biggest ($(du -sm "$biggest" | cut -f1) MB) from the saved entry; that image builds cold once on the next main push"
  # The dance's extraction may leave files a plain user cannot remove;
  # hosted runners have passwordless sudo.
  rm -rf "$biggest" 2>/dev/null || sudo rm -rf "$biggest" || { echo "::warning::cap: could not remove $biggest"; break; }
done
echo "cap: saving $(total) MB"
exit 0

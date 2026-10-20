#!/usr/bin/env bash
#
# The published image list has ONE source, and every copy of it agrees.
#
# WHAT THIS CLOSES. The published images are written out in four places, all
# in publish.yml: the `build`, `scan` and `publish-image` matrices and the
# IMAGES bash array in its cleanup step. Nothing compared them. (A fifth copy,
# the PR-time scan matrix in ci-image-scan.yml, went away when the scan moved
# to .github/workflows/nightly-image-scan.yml, which READS publish.yml's `scan`
# matrix instead of carrying its own.)
#
# It used to be six places: publish.yml carried FOUR matrices, because scan,
# manifest and sign were three jobs of ten. They are one `publish-image` job
# now, so there are two matrices to hold — the walk floor below moved with
# them, and it still names the jobs it expects rather than counting anonymous
# matrices.
#
# publish.yml's IMAGES array carries a comment saying it is "held by the same
# gate in helm-lint.yml". IT IS NOT. That gate cross-checks the CHART matrix and
# the CHARTS rollback array; it says nothing about images. The comment describes
# a check that does not exist, which is worse than no comment: a reader who
# adds an image and forgets the array has been told something already catches
# it.
#
# THE SOURCE IS docker-bake.hcl, because that is what actually builds them. A
# name in a workflow matrix with no bake target is a job that cannot run; a bake
# target nobody publishes is an image that is built and thrown away.
#
# WHAT THIS DELIBERATELY DOES NOT DO, and why the plan for it changed. ADR-0012
# proposed generating the matrices themselves from `docker buildx bake --print`,
# with the per-image fields (context, dockerfile, target, expect_type, blocking)
# carried in bake target LABELS. That is not delivered here: `bake --print` is
# not invoked anywhere in this repository today, so its output shape could only
# have been assumed, and the parser would have been written against an invented
# sample. Inventing the oracle is the failure this repository keeps finding.
#
# Generating the matrices needs a real `--print` capture to build against. This
# script makes those lists unable to disagree, which is the value that was
# actually available.
set -euo pipefail

cd "$(dirname "$0")/../.."

BAKE=docker-bake.hcl
PUBLISH=.github/workflows/publish.yml
EXEMPT=.github/image-scan-exempt.txt

# Bake targets that are built but NOT published. Each needs a reason. None
# today: elitea-inventory-engine (ADR-0027) is published like every other image.
UNPUBLISHED=""

die() { printf '::error::%s\n' "$1" >&2; exit 1; }

# ── the source ──────────────────────────────────────────────────────────────
bake_targets() {
  # Anchored on the declaration. A target name inside a comment or an inherits
  # list is not a target.
  grep -oE '^target "[a-z0-9-]+"' "$BAKE" | sed -E 's/^target "//; s/"$//'
}

published_names() {
  local all excluded
  all="$(bake_targets)"
  [ -n "$all" ] || die "read no target out of $BAKE — this check measured nothing"
  excluded="$(printf '%s\n' $UNPUBLISHED)"
  if [ -z "$excluded" ]; then
    printf '%s\n' "$all" | sort -u
  else
    printf '%s\n' "$all" | grep -vxF "$excluded" | sort -u
  fi
}

normalise() { printf '%s\n' "$1" | tr ' ' '\n' | sed '/^$/d' | sort -u | tr '\n' ' '; }

case "${1:---verify}" in
  names)
    published_names
    exit 0
    ;;
  --verify) ;;
  *)
    printf 'usage: %s [names|--verify]\n' "$0" >&2
    exit 2
    ;;
esac

expected="$(normalise "$(published_names | tr '\n' ' ')")"
[ -n "$expected" ] || die "the published image set is empty — this check measured nothing"

status=0
compare() {
  local label="$1" actual="$2"
  if [ -z "$actual" ]; then
    die "read no image name out of $label — this check measured nothing"
  fi
  actual="$(normalise "$actual")"
  if [ "$actual" != "$expected" ]; then
    printf '::error::%s is [%s], %s publishes [%s]\n' "$label" "$actual" "$BAKE" "$expected" >&2
    status=1
  fi
}

# ── the publish.yml matrices ────────────────────────────────────────────────
#
# Each is a `- name: <image>` list under a `matrix.image`. Compared one by one
# rather than merged, because EACH matrix holding the right names is the
# invariant — a single merged set would pass with one matrix empty and another
# holding twice the images.
EXPECTED_JOBS="build scan publish-image"
seen_jobs=""
while IFS= read -r job; do
  names="$(python3 - "$PUBLISH" "$job" <<'PY'
import sys
import yaml
doc = yaml.safe_load(open(sys.argv[1]))
matrix = ((doc["jobs"][sys.argv[2]].get("strategy") or {}).get("matrix") or {})
print(" ".join(i["name"] for i in matrix.get("image", []) if isinstance(i, dict) and i.get("name")))
PY
)"
  compare "publish.yml job '$job'" "$names"
  seen_jobs="$seen_jobs $job"
done < <(python3 - "$PUBLISH" <<'PY'
import sys
import yaml
doc = yaml.safe_load(open(sys.argv[1]))
for job, spec in doc["jobs"].items():
    matrix = ((spec.get("strategy") or {}).get("matrix") or {})
    if any(isinstance(i, dict) and i.get("name") for i in matrix.get("image", [])):
        print(job)
PY
)

# A floor on the WALK, not only on each list. A publish.yml whose matrices
# stopped parsing would otherwise satisfy every comparison above by making none.
#
# The floor names the jobs instead of counting them: a rename that drops one of
# them (the `scan` -> `publish-image` fold is exactly that shape) must fail here
# rather than silently reduce what this check covers.
for job in $EXPECTED_JOBS; do
  case " $(printf '%s' "$seen_jobs" | tr -s ' ') " in
    *" $job "*) ;;
    *) die "found no image matrix for job '$job' in $PUBLISH (walked:$seen_jobs) — this check measured less than it claims" ;;
  esac
done

# ── the IMAGES bash array ───────────────────────────────────────────────────
#
# The one whose comment claims a gate that does not exist. Anchored on the
# assignment so an image name in nearby prose cannot be read as a member.
images_array="$(grep -oE '^ +IMAGES=\(.*\)' "$PUBLISH" | sed -E 's/^ +IMAGES=\(//; s/\)$//; s/"//g' || true)"
compare "publish.yml IMAGES array" "$images_array"

# ── the exemption register agrees with the release scan ─────────────────────
#
# `blocking: false` in publish.yml's `scan` matrix stops a finding failing the
# release (and the nightly scan, which reads the same matrix);
# .github/image-scan-exempt.txt is the register of images allowed to carry it.
# TWO FILES must agree, so one edit cannot quietly exempt a second image. Every
# entry must STATE `blocking` and `expect_type`: a missing field is not "true".
register_check="$(python3 - "$PUBLISH" "$EXEMPT" <<'PY'
import sys
import yaml
doc = yaml.safe_load(open(sys.argv[1]))
entries = ((doc["jobs"]["scan"].get("strategy") or {}).get("matrix") or {}).get("image") or []
if not entries:
    print("ERROR read no entry from publish.yml's scan matrix")
    sys.exit(0)
for e in entries:
    if not isinstance(e.get("blocking"), bool) or not e.get("expect_type"):
        print(f"ERROR scan matrix entry {e.get('name')!r} must state expect_type and a boolean blocking")
exempt = sorted(e["name"] for e in entries if e.get("blocking") is False)
registered = sorted(
    line.strip() for line in open(sys.argv[2]) if line.strip() and not line.lstrip().startswith("#")
)
if exempt != registered:
    print(f"ERROR blocking: false in publish.yml's scan matrix = {exempt}, {sys.argv[2]} registers {registered}; an image stops blocking only when BOTH say so")
print("EXEMPT " + " ".join(exempt))
PY
)"
if printf '%s\n' "$register_check" | grep -q '^ERROR '; then
  printf '%s\n' "$register_check" | sed -n 's/^ERROR /::error::/p' >&2
  status=1
fi

# ── the exemption file names a real image ───────────────────────────────────
#
# The other direction. An exemption for an image that no longer exists is a
# line that suppresses nothing and reads as though it does.
while IFS= read -r name; do
  case " $expected " in
    *" $name "*) ;;
    *)
      printf '::error::%s exempts %q, which is not a published image\n' "$EXEMPT" "$name" >&2
      status=1
      ;;
  esac
done < <(grep -vE '^\s*(#|$)' "$EXEMPT" | tr -d ' \t')

if [ "$status" -ne 0 ]; then
  exit 1
fi
printf 'OK: %d published images, all 4 copies of the list agree, and the exemption register matches the release scan.\n' \
  "$(printf '%s' "$expected" | wc -w | tr -d ' ')"

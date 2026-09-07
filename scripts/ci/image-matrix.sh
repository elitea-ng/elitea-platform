#!/usr/bin/env bash
#
# The published image list has ONE source, and every copy of it agrees.
#
# WHAT THIS CLOSES. The published images are written out in four places: two
# matrices in publish.yml (`build`, `publish-image`), the IMAGES bash array in
# publish.yml's cleanup step, and the scan matrix in ci-image-scan.yml. Nothing
# compared them.
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
SCAN=.github/workflows/ci-image-scan.yml
EXEMPT=.github/image-scan-exempt.txt

# Bake targets that are built but NOT published. Each needs a reason.
#
# elitea-deepwiki-engine is the engine build: the same source plus a ~92-package
# closure (torch, transformers, faiss-cpu, tree-sitter grammars). ADR-0022 keeps
# it out of the default bake group because it is torch-sized, and out of the
# release because nothing deploys it yet.
#
# elitea-inventory-engine is the same shape for the second provider: the
# knowledge-graph engine's 126-package closure, which no deployment pulls yet.
# The PLAIN elitea-inventory image IS published — it carries the sidecar and
# its fixture runner, which is what the compose stacks and the E2E stack run.
UNPUBLISHED="elitea-deepwiki-engine elitea-inventory-engine"

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
  printf '%s\n' "$all" | grep -vxF "$excluded" | sort -u
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

# ── ci-image-scan.yml ───────────────────────────────────────────────────────
scan_names="$(python3 - "$SCAN" <<'PY'
import sys
import yaml
doc = yaml.safe_load(open(sys.argv[1]))
out = []
for spec in doc["jobs"].values():
    matrix = ((spec.get("strategy") or {}).get("matrix") or {})
    out += [i["name"] for i in matrix.get("image", []) if isinstance(i, dict) and i.get("name")]
print(" ".join(out))
PY
)"
compare "ci-image-scan.yml scan matrix" "$scan_names"

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
printf 'OK: %d published images, and all 4 copies of the list agree.\n' \
  "$(printf '%s' "$expected" | wc -w | tr -d ' ')"

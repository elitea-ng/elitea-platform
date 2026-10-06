#!/usr/bin/env bash
#
# Every worker replica gets its own NATS connection name.
#
# WHAT IS BEING PROTECTED. consumer_id is the worker's NATS connection NAME.
# Every replica pulls from the SAME durable JetStream consumer, and JetStream
# tracks each delivery itself, so the name carries no delivery correctness. It
# is what tells replicas apart in `nats server report connections` and in the
# worker's own logs: with one shared name, a stuck or dead replica cannot be
# told from a healthy one, and an operator draining "the" worker drains a
# guess. (Under the retired Redis command bus the name WAS the consumer-group
# identity, and sharing it lost work; the per-pod substitution was built then
# and stays.)
#
# The mechanism has three parts and a break in any one of them is silent, which
# is why each is asserted separately:
#
#   1. the ConfigMap emits a placeholder rather than a finished name;
#   2. the init container substitutes the pod's own name into it;
#   3. the worker mounts the RENDERED file, not the ConfigMap's copy.
#
# Part 3 is the one that fails invisibly. Mounting the ConfigMap would give
# every replica a consumer name containing the literal "__ELITEA_POD_NAME__" —
# identical across the fleet, which is the original defect wearing a stranger
# name, and nothing in the pod's logs would say so.
#
# The substitution itself is EXTRACTED FROM THE RENDERED MANIFEST and executed,
# not re-typed here. A test that re-declares the code it tests measures a copy;
# this repository already has one of those (getConfiguredRepo.test.js, 823
# lines against helpers it declares inline).
set -euo pipefail

cd "$(dirname "$0")/../../.."
CHART="deploy/helm/elitea"

# worker.enabled is FALSE in the shipped defaults and everything here lives
# inside that condition. Without this the whole file passes against templates
# that never render — the failure mode render-workload-session.sh records.
BASE=(-f "$CHART/values-standalone.yaml"
      --set llmGateway.env.GATEWAY_SELF_LLM_ORIGINS=https://elitea.invalid/llm/v1
      --set llmGateway.egressPosture=public-unrestricted
      --set worker.enabled=true)

failures=0
fail() { printf 'FAIL: %s\n' "$1" >&2; failures=$((failures + 1)); }
pass() { printf '  ok: %s\n' "$1"; }

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# The manifest goes to a FILE, and every check below greps the file.
#
# `printf '%s' "$MANIFEST" | grep -q PATTERN` does not work under
# `set -o pipefail`: grep -q exits as soon as it matches, printf takes SIGPIPE,
# and the pipeline reports 141 — so a check reads as "not found" precisely when
# the thing IS found. It fails loudly here rather than passing, but it is still
# a check that reports the opposite of the truth.
MANIFEST="$work/manifest.yaml"
helm template identity "$CHART" "${BASE[@]}" > "$MANIFEST"

# ------------------------------------------------------------------ part 1
if grep -q '"consumer_id":"[^"]*__ELITEA_POD_NAME__"' "$MANIFEST"; then
  pass "the ConfigMap emits a per-pod placeholder"
else
  fail "the ConfigMap's consumer_id has no __ELITEA_POD_NAME__ placeholder, so every replica shares one NATS connection name"
fi

# ------------------------------------------------------------------ part 2
if grep -q 'fieldPath: metadata.name' "$MANIFEST"; then
  pass "the init container reads the pod name from the downward API"
else
  fail "no downward-API pod name reaches the init container, so it has nothing unique to substitute"
fi

# ------------------------------------------------------------------ part 3
#
# The worker's runtime.json must come from runtime-rendered. Checked by
# reading the mount that targets that path, rather than by grepping the
# manifest for the volume name anywhere — the volume is also DECLARED in
# `volumes:`, so a bare grep would pass with the mount still pointing at the
# ConfigMap.
mount_source=$(grep -B2 'mountPath: /run/elitea/runtime.json' "$MANIFEST" \
  | grep -o 'name: [a-z-]*' | tail -1 | awk '{print $2}')
if [ "$mount_source" = "runtime-rendered" ]; then
  pass "the worker mounts the rendered document"
else
  fail "the worker's /run/elitea/runtime.json comes from '${mount_source}', not runtime-rendered: every replica would read a consumer name containing the literal placeholder"
fi

# ------------------------------------------------------------------ part 4
#
# Two pod names must produce two different consumer names. This runs the
# substitution the manifest actually carries, extracted with awk, so that a
# change to the init script changes this test's input.
substitution=$(awk '/sed "s\/__ELITEA_POD_NAME__/{found=1} found{print; if (/rendered\/runtime.json/) exit}' "$MANIFEST" \
  | sed 's/^[[:space:]]*//' | tr -d '\\' | tr '\n' ' ')
if [ -z "$substitution" ]; then
  fail "no substitution command was found in the rendered init container; parts 1-3 could all pass with nothing doing the work"
else
  mkdir -p "$work/src-runtime" "$work/rendered"
  grep -o '{"agent_checkpoint_connection_path".*}' "$MANIFEST" | head -1 > "$work/src-runtime/runtime.json"
  if [ ! -s "$work/src-runtime/runtime.json" ]; then
    fail "could not extract runtime.json from the manifest; part 4 measured nothing"
  else
    # The extracted command names container-absolute paths. Re-root them at
    # the scratch directory — the two paths only, so the logic under test is
    # still the manifest's own.
    rooted=$(printf '%s' "$substitution" \
      | sed "s#/src-runtime/#$work/src-runtime/#g; s#/rendered/#$work/rendered/#g")

    names=""
    for pod in elitea-worker-aaa111 elitea-worker-bbb222; do
      POD_NAME="$pod" sh -c "$rooted"
      names="$names$(grep -o '"consumer_id":"[^"]*"' "$work/rendered/runtime.json")
"
    done
    distinct=$(printf '%s' "$names" | sort -u | grep -c . || true)
    if [ "$distinct" -eq 2 ]; then
      pass "two pods produce two distinct consumer names"
    else
      fail "two different pod names produced $distinct distinct consumer name(s); the fleet would still split acknowledgements"
    fi
    if grep -q '__ELITEA_POD_NAME__' "$work/rendered/runtime.json"; then
      fail "the placeholder survived substitution in the rendered document"
    else
      pass "no placeholder survives into the document the worker reads"
    fi
  fi
fi

# ------------------------------------------------------------------ part 5
#
# The identity length guard, at its exact boundary. The worker bounds every
# identity at 256 bytes and a pod name can be 63 characters, so the configured
# prefix must be at most 192. Both directions are checked: a guard that refuses
# everything passes a one-sided test just as well as a correct one.
long=$(printf 'c%.0s' $(seq 1 193))
ok=$(printf 'c%.0s' $(seq 1 192))
if helm template guard "$CHART" "${BASE[@]}" --set worker.runtime.consumerId="$long" >/dev/null 2>&1; then
  fail "a 193-character consumerId rendered; the worker would refuse its whole configuration at startup with no field named"
else
  pass "a 193-character consumerId is refused at render time"
fi
if helm template guard "$CHART" "${BASE[@]}" --set worker.runtime.consumerId="$ok" >/dev/null 2>&1; then
  pass "a 192-character consumerId renders"
else
  fail "a 192-character consumerId was refused; the guard is off by one and rejects a valid deployment"
fi

if [ "$failures" -ne 0 ]; then
  printf '\n%d check(s) failed.\n' "$failures" >&2
  exit 1
fi
printf '\nAll worker consumer-identity checks passed.\n'

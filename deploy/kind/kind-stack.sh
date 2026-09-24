#!/usr/bin/env bash
# A local Kubernetes stack for the DeepWiki provider, installed from THIS
# repository's Helm chart the way it ships to production (ADR-0023):
#
#   * the provider pod is TWO containers — the Go sub-application host
#     (ghcr.io/elitea-ng/elitea-subapp-host) with ELITEA_DEEPWIKI_RUNNER=legacy,
#     and the Python engine sidecar (ghcr.io/elitea-ng/elitea-deepwiki) reached
#     over a Unix socket the two share;
#   * the facade -> provider hop is mutually authenticated, with both
#     certificates issued by cert-manager from one internal CA ClusterIssuer,
#     exactly as templates/deepwiki/certificates.yaml asks for;
#   * the engine sidecar runs the FIXTURE runner, so a generation completes
#     with no LLM and no git host.
#
#   deploy/kind/kind-stack.sh up       # build, load, install, seed  (~6-9 min cold)
#   deploy/kind/kind-stack.sh verify   # prove the four claims below
#   deploy/kind/kind-stack.sh down     # delete the cluster
#
# podman by default (this machine has no docker); KIND_ENGINE=docker switches
# every build/save/exec to docker, which is what the CI runner has
# (.github/workflows/helm-install-smoke.yml). kind's provider follows the engine.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"

CLUSTER="${KIND_CLUSTER:-elitea-kind}"
NS="${KIND_NAMESPACE:-elitea}"
RELEASE="${KIND_RELEASE:-elitea}"
TAG="${KIND_IMAGE_TAG:-kind}"
ENGINE_TAG="${TAG}-engine"
CERT_MANAGER_VERSION="${CERT_MANAGER_VERSION:-v1.19.2}"

HOST_IMAGE="ghcr.io/elitea-ng/elitea-subapp-host:${TAG}"
MAIN_IMAGE="ghcr.io/elitea-ng/elitea-main:${TAG}"
ENGINE_IMAGE="ghcr.io/elitea-ng/elitea-deepwiki:${ENGINE_TAG}"

# The engine sidecar's build extras. `storage-postgres` is what the migration
# Job needs — it opens ELITEA_DEEPWIKI_DATABASE_URL, and psycopg lives behind
# that extra. The `engine` extra (torch, transformers, faiss-cpu, the
# tree-sitter grammars — multi-GB) is DELIBERATELY not built here: the sidecar
# runs deepwiki.engine.runner=fixture, which is the runner the compose stacks
# also pair with the plain image. See README.md, "Known limits".
DEEPWIKI_ENGINE_EXTRAS="${DEEPWIKI_ENGINE_EXTRAS:-[storage-postgres]}"

# The seeded credential. The row is written by seed.sql; the bearer is minted
# from it below.
PAT_UUID="4b1d0000-0000-4000-8000-00000000d0c5"
# Under production Form authentication the API group's token validator is the
# auth graph's, and it verifies HS512 over the PAT signing key file — NOT
# APPLICATION_SECRET_KEY. Same file the pod mounts.
RUNTIME_MATERIAL_DIR="${REPO_ROOT}/deploy/certs/runtime"
PROJECT_ID=1
TOOLKIT_ID=9002
CODE_TOOLKIT_ID=9010
WIKI_ID="acme--e2e-generated--main"
BUCKET="wiki-artifacts"

ENGINE="${KIND_ENGINE:-podman}"
# Every engine difference is decided HERE, once. podman's `save` needs the
# archive format spelled out and kind needs the provider override; docker
# needs neither, and `kind load docker-image` streams straight from its
# daemon with no archive on disk.
case "$ENGINE" in
  podman) export KIND_EXPERIMENTAL_PROVIDER=podman ;;
  docker) unset KIND_EXPERIMENTAL_PROVIDER ;;
  *) printf '\nERROR: KIND_ENGINE must be podman or docker (got %s)\n' "$ENGINE" >&2; exit 1 ;;
esac

say()  { printf '\n\033[1m== %s\033[0m\n' "$*"; }
note() { printf '   %s\n' "$*"; }
die()  { printf '\nERROR: %s\n' "$*" >&2; exit 1; }

kc() { kubectl --context "kind-${CLUSTER}" "$@"; }

require_tools() {
  for tool in "$ENGINE" kind kubectl helm python3; do
    command -v "$tool" >/dev/null 2>&1 || die "$tool is not on PATH"
  done
}

# ── up ───────────────────────────────────────────────────────────────────────

ensure_cluster() {
  if kind get clusters 2>/dev/null | grep -qx "$CLUSTER"; then
    note "cluster ${CLUSTER} already exists"
  else
    say "Creating the kind cluster (${ENGINE} provider)"
    kind create cluster --name "$CLUSTER" --wait 180s
  fi
}

have_image() { "$ENGINE" image inspect "$1" >/dev/null 2>&1; }

build_images() {
  say "Building the application images with ${ENGINE}"
  if have_image "$HOST_IMAGE"; then
    note "$HOST_IMAGE present"
  else
    note "building $HOST_IMAGE"
    "$ENGINE" build -f "${REPO_ROOT}/services/elitea-subapp-host/Containerfile" \
      -t "$HOST_IMAGE" "$REPO_ROOT"
  fi
  if have_image "$MAIN_IMAGE"; then
    note "$MAIN_IMAGE present"
  else
    note "building $MAIN_IMAGE"
    # `e2e` is byte-identical to the shipping `final` stage and builds the
    # admin SPA from apps/elitea-web, so no admin-ui submodule is needed.
    "$ENGINE" build -f "${REPO_ROOT}/services/elitea-main/Containerfile" --target e2e \
      -t "$MAIN_IMAGE" "$REPO_ROOT"
  fi
  if have_image "$ENGINE_IMAGE"; then
    note "$ENGINE_IMAGE present"
  else
    note "building $ENGINE_IMAGE (EXTRAS=${DEEPWIKI_ENGINE_EXTRAS})"
    "$ENGINE" build -f "${REPO_ROOT}/services/elitea-deepwiki/Containerfile" \
      --build-arg "EXTRAS=${DEEPWIKI_ENGINE_EXTRAS}" \
      -t "$ENGINE_IMAGE" "$REPO_ROOT"
  fi
}

# Third-party images are pulled once to the HOST and side-loaded, so a repeated
# `up` needs no registry at all.
INFRA_IMAGES=(
  docker.io/pgvector/pgvector:0.8.5-pg16
  docker.io/library/redis:7-alpine
  docker.io/rustfs/rustfs:latest
  rustfs/rc:latest
)

load_images() {
  say "Loading images into the kind node"
  # What the node already has. Asked ONCE: each side-load is a full
  # engine `save` plus a containerd import, and the four third-party images
  # alone are ~1.4 GB — re-doing that on every `up` cost more than everything
  # else in this script put together. Set KIND_FORCE_LOAD=1 after rebuilding
  # an image under a tag the node already carries.
  local present=""
  if [ "${KIND_FORCE_LOAD:-0}" != "1" ]; then
    present="$("$ENGINE" exec "${CLUSTER}-control-plane" crictl images -o json 2>/dev/null \
      | python3 -c 'import json,sys
try:
    for image in json.load(sys.stdin).get("images", []):
        for tag in image.get("repoTags", []):
            print(tag)
except Exception:
    pass' || true)"
  fi
  local archive
  # An explicit template: BSD mktemp reads `-t X` as a prefix, GNU as a
  # template, and this repository has been bitten by that difference.
  archive="$(mktemp "${TMPDIR:-/tmp}/elitea-kind-XXXXXX.tar")"
  for image in "$MAIN_IMAGE" "$HOST_IMAGE" "$ENGINE_IMAGE" "${INFRA_IMAGES[@]}"; do
    if printf '%s\n' "$present" | grep -qxF "$image"; then
      note "$image already in the node"
      continue
    fi
    if ! have_image "$image"; then
      note "pulling $image"
      "$ENGINE" pull "$image" >/dev/null
    fi
    note "loading $image"
    if [ "$ENGINE" = "docker" ]; then
      # Streams from the daemon into the node: no second copy on disk.
      kind load docker-image "$image" --name "$CLUSTER" >/dev/null
    else
      # `kind load docker-image` shells out to docker; under podman the
      # archive route is the one that works.
      podman save --format docker-archive -o "$archive" "$image"
      kind load image-archive "$archive" --name "$CLUSTER" >/dev/null
    fi
  done
  rm -f "$archive"
}

install_cert_manager() {
  say "Installing cert-manager ${CERT_MANAGER_VERSION}"
  if kc get deployment -n cert-manager cert-manager >/dev/null 2>&1; then
    note "already installed"
  else
    helm --kube-context "kind-${CLUSTER}" upgrade --install cert-manager cert-manager \
      --repo https://charts.jetstack.io \
      --version "$CERT_MANAGER_VERSION" \
      --namespace cert-manager --create-namespace \
      --set crds.enabled=true \
      --wait --timeout 10m
  fi
  kc wait --for=condition=Available --timeout=300s \
    -n cert-manager deployment/cert-manager deployment/cert-manager-webhook

  say "Creating the internal CA ClusterIssuer (elitea-internal-ca)"
  # The webhook can report Available a moment before it answers, and a refused
  # admission call here reads like a malformed manifest.
  local attempt
  for attempt in $(seq 1 30); do
    if kc apply -f "${SCRIPT_DIR}/manifests/ca-issuer.yaml" >/dev/null 2>&1; then break; fi
    [ "$attempt" -eq 30 ] && kc apply -f "${SCRIPT_DIR}/manifests/ca-issuer.yaml"
    sleep 2
  done
  kc wait --for=condition=Ready --timeout=120s clusterissuer/elitea-internal-ca
}

# The production-authentication material. deploy/scripts/gen-runtime-certs.sh
# is the repository's own generator and is IDEMPOTENT — it exits without
# touching anything when the tree is present and its certificates are valid —
# so this reuses whatever a compose stack on this machine already minted. The
# Redis server certificate it issues already carries DNS:elitea-runtime-redis,
# the Kubernetes Service name, beside the compose one.
ensure_runtime_material() {
  say "Preparing the runtime/auth material"
  bash "${REPO_ROOT}/deploy/scripts/gen-runtime-certs.sh"
  local file
  for file in runtime-ca.crt redis-server.crt redis-server.key redis-users.acl \
              redis-bootstrap-password redis-auth-password auth-attempt-key \
              auth-pat-signing-key auth-form-users.json; do
    [ -f "${RUNTIME_MATERIAL_DIR}/${file}" ] || die "missing material: ${RUNTIME_MATERIAL_DIR}/${file}"
  done

  # What redis-server reads: its keypair, the CA it presents against, the ACL
  # file (which is what makes `user default off` real) and the bootstrap
  # password its readiness probe authenticates with.
  kc -n "$NS" create secret generic elitea-runtime-redis-material \
    --from-file="${RUNTIME_MATERIAL_DIR}/runtime-ca.crt" \
    --from-file="${RUNTIME_MATERIAL_DIR}/redis-server.crt" \
    --from-file="${RUNTIME_MATERIAL_DIR}/redis-server.key" \
    --from-file="${RUNTIME_MATERIAL_DIR}/redis-users.acl" \
    --from-file="${RUNTIME_MATERIAL_DIR}/redis-bootstrap-password" \
    --dry-run=client -o yaml | kc apply -f -

  # The five files internal/authcomposition/material.go opens. Their names are
  # the contract: the chart derives every path in the auth document from them.
  kc -n "$NS" create secret generic elitea-main-auth-material \
    --from-file="${RUNTIME_MATERIAL_DIR}/redis-auth-password" \
    --from-file="${RUNTIME_MATERIAL_DIR}/runtime-ca.crt" \
    --from-file="${RUNTIME_MATERIAL_DIR}/auth-attempt-key" \
    --from-file="${RUNTIME_MATERIAL_DIR}/auth-pat-signing-key" \
    --from-file="${RUNTIME_MATERIAL_DIR}/auth-form-users.json" \
    --dry-run=client -o yaml | kc apply -f -
}

install_infra() {
  say "Installing PostgreSQL, Redis, the object store and the OIDC provider"
  kc create namespace "$NS" --dry-run=client -o yaml | kc apply -f -
  kc apply -f "${SCRIPT_DIR}/manifests/infra.yaml"

  ensure_runtime_material

  # The secrets the chart names. They are the operator's, not the chart's.
  kc -n "$NS" create secret generic elitea-db \
    --from-literal=database-url='postgres://elitea:elitea@postgres:5432/elitea?sslmode=disable' \
    --dry-run=client -o yaml | kc apply -f -
  # Signs the X-Elitea-* identity headers the provider verifies. One value,
  # both sides: the chart hands this same Secret to elitea-main and to the
  # provider pod.
  kc -n "$NS" create secret generic elitea-deepwiki-secrets \
    --from-literal=identity-secret='kind-deepwiki-identity' \
    --dry-run=client -o yaml | kc apply -f -
  # Non-optional in the chart even with llmGateway.enabled=false.
  kc -n "$NS" create secret generic elitea-main-llm-gateway-secrets \
    --from-literal=gateway-identity-secret='kind-unused-gateway-identity' \
    --dry-run=client -o yaml | kc apply -f -
  kc -n "$NS" create secret generic elitea-main-storage-secrets \
    --from-literal=s3-access-key='elitea' \
    --from-literal=s3-secret-key='elitea-dev-secret' \
    --dry-run=client -o yaml | kc apply -f -

  kc -n "$NS" rollout status deployment/postgres --timeout=300s
  kc -n "$NS" rollout status deployment/redis --timeout=300s
  kc -n "$NS" rollout status deployment/rustfs --timeout=300s

  say "Creating the artifact bucket"
  # THE BUCKET MUST EXIST BEFORE elitea-main STARTS: its boot step
  # configureObjectStoreRetentionLifecycle has no tolerance for a missing
  # bucket, and it is the Deployment that crash-loops, after the install
  # already looked green (values.yaml says this at length).
  kc -n "$NS" delete job rustfs-bucket-init --ignore-not-found >/dev/null
  kc -n "$NS" create job rustfs-bucket-init --image=rustfs/rc:latest \
    --dry-run=client -o json -- sh -c \
    'rc alias set rustfs http://rustfs:9000 elitea elitea-dev-secret && rc mb --ignore-existing rustfs/elitea-artifacts' \
    | python3 -c 'import json,sys; j=json.load(sys.stdin); j["spec"]["template"]["spec"]["containers"][0]["imagePullPolicy"]="IfNotPresent"; j["spec"]["backoffLimit"]=6; print(json.dumps(j))' \
    | kc apply -f -
  kc -n "$NS" wait --for=condition=complete --timeout=180s job/rustfs-bucket-init
}

install_chart() {
  say "Installing the chart (elitea-main + the DeepWiki provider)"
  # The migration Jobs are pre-install hooks: elitea-main's own -all-tenants
  # run, and the provider's `python -m elitea_deepwiki.storage`. --wait covers
  # both, and the hooks' own timeout is the one that matters for a cold start.
  helm --kube-context "kind-${CLUSTER}" upgrade --install "$RELEASE" "${REPO_ROOT}/deploy/helm/elitea" \
    --namespace "$NS" \
    -f "${SCRIPT_DIR}/values-kind.yaml" \
    --wait --timeout 15m
}

psql_exec() {
  local pod
  pod="$(kc -n "$NS" get pod -l app=postgres -o jsonpath='{.items[0].metadata.name}')"
  kc -n "$NS" exec -i "$pod" -- psql -v ON_ERROR_STOP=1 -U elitea -d elitea "$@"
}

seed() {
  say "Seeding the wiki toolkit, its repository credential, the bucket row and a PAT"
  psql_exec -q < "${SCRIPT_DIR}/seed.sql"
  note "seeded project ${PROJECT_ID}, toolkit ${TOOLKIT_ID}, code toolkit ${CODE_TOOLKIT_ID}"
}

cmd_up() {
  require_tools
  ensure_cluster
  build_images
  load_images
  install_cert_manager
  install_infra
  install_chart
  seed
  say "Up. Next: ${BASH_SOURCE[0]} verify"
}

# ── verify ───────────────────────────────────────────────────────────────────

mint_pat() {
  python3 - "$PAT_UUID" "${RUNTIME_MATERIAL_DIR}/auth-pat-signing-key" <<'PY'
import base64, hashlib, hmac, json, pathlib, sys
key = pathlib.Path(sys.argv[2]).read_bytes()
b64 = lambda b: base64.urlsafe_b64encode(b).rstrip(b"=").decode()
header = b64(json.dumps({"alg": "HS512", "typ": "JWT"}, separators=(",", ":")).encode())
payload = b64(json.dumps({"uuid": sys.argv[1], "expires": None}, separators=(",", ":")).encode())
print(f"{header}.{payload}." + b64(hmac.new(key, f"{header}.{payload}".encode(), hashlib.sha512).digest()))
PY
}

PF_PID=""
stop_port_forward() { [ -n "$PF_PID" ] && kill "$PF_PID" 2>/dev/null || true; }

start_port_forward() {
  local port="${1:-18080}"
  kc -n "$NS" port-forward svc/elitea-main "${port}:8080" >/tmp/elitea-kind-pf.log 2>&1 &
  PF_PID=$!
  trap stop_port_forward EXIT
  local attempt
  for attempt in $(seq 1 40); do
    if curl -fsS "http://127.0.0.1:${port}/healthz" >/dev/null 2>&1 \
       || curl -sS -o /dev/null "http://127.0.0.1:${port}/" 2>/dev/null; then
      return 0
    fi
    sleep 0.5
  done
  cat /tmp/elitea-kind-pf.log >&2
  die "the port-forward to elitea-main never answered"
}

cmd_verify() {
  require_tools
  local failures=0
  fail() { printf '\033[31mFAIL\033[0m: %s\n' "$*" >&2; failures=$((failures + 1)); }
  pass() { printf '\033[32mok\033[0m   %s\n' "$*"; }

  # ── 1. every pod Ready ─────────────────────────────────────────────────────
  say "1. every pod is Ready"
  local not_ready
  # Jobs run to completion and their pods are Succeeded, not Ready — reading
  # them as failures would make a correct stack fail this check forever.
  not_ready="$(kc -n "$NS" get pods \
      -o jsonpath='{range .items[?(@.status.phase!="Succeeded")]}{.metadata.name}{" "}{range .status.conditions[?(@.type=="Ready")]}{.status}{end}{"\n"}{end}' \
      | awk 'NF && $2 != "True" {print $1}')"
  if [ -n "$not_ready" ]; then
    kc -n "$NS" get pods
    fail "not Ready: $(echo "$not_ready" | tr '\n' ' ')"
  else
    kc -n "$NS" get pods --no-headers | awk '{print "   " $0}'
    pass "all pods in namespace ${NS} are Ready"
  fi

  # ── 2. the provider pod is the two-container shape ─────────────────────────
  say "2. the provider pod runs the host AND the engine sidecar"
  local containers
  # The migrate Job's pod carries the same name label, so the component is
  # excluded: with it included the selector's first item is whichever pod the
  # API returns first, and a green run would be luck.
  containers="$(kc -n "$NS" get pod -l 'app.kubernetes.io/name=elitea-deepwiki,app.kubernetes.io/component!=deepwiki-migrate' \
      -o jsonpath='{.items[0].spec.containers[*].name}')"
  if [ "$containers" != "elitea-deepwiki engine" ]; then
    fail "the provider pod's containers are '${containers}'; expected 'elitea-deepwiki engine'"
  else
    kc -n "$NS" get pod -l 'app.kubernetes.io/name=elitea-deepwiki,app.kubernetes.io/component!=deepwiki-migrate' \
      -o jsonpath='{range .items[0].spec.containers[*]}   {.name}{"\t"}{.image}{"\n"}{end}'
    pass "containers: ${containers}"
  fi
  local host_runner engine_runner
  host_runner="$(kc -n "$NS" get pod -l 'app.kubernetes.io/name=elitea-deepwiki,app.kubernetes.io/component!=deepwiki-migrate' \
      -o jsonpath='{.items[0].spec.containers[0].env[?(@.name=="ELITEA_DEEPWIKI_RUNNER")].value}')"
  engine_runner="$(kc -n "$NS" get pod -l 'app.kubernetes.io/name=elitea-deepwiki,app.kubernetes.io/component!=deepwiki-migrate' \
      -o jsonpath='{.items[0].spec.containers[1].env[?(@.name=="ELITEA_DEEPWIKI_RUNNER")].value}')"
  if [ "$host_runner" != "legacy" ] || [ "$engine_runner" != "fixture" ]; then
    fail "runners are host=${host_runner} engine=${engine_runner}; expected legacy/fixture"
  else
    pass "runners: host=${host_runner} (reaches the sidecar over the socket), engine=${engine_runner}"
  fi

  # ── 3. the facade registered the provider ──────────────────────────────────
  say "3. the facade registered the provider with the admission plane"
  local registration
  registration="$(psql_exec -tAc "
      SELECT o.provider_id || ' healthy=' || p.healthy::text
      FROM provider_hub.provider_origin_registration o
      JOIN provider_hub.provider_health_projection p USING (project_id, provider_id)
      WHERE o.provider_id = 'wikis'" 2>&1 | tr -d '\r' | sed '/^$/d')"
  if [ "$registration" != "wikis healthy=true" ]; then
    fail "provider_hub reports '${registration:-<nothing>}'; expected 'wikis healthy=true'"
  else
    note "$registration"
    pass "provider_hub: the facade registered 'wikis' and its health probe answered"
  fi

  # ── 4. a fixture generation completes through the facade ───────────────────
  say "4. a generation runs through the facade and its pages land in the bucket"
  local port=18080
  start_port_forward "$port"
  local base="http://127.0.0.1:${port}"
  local token; token="$(mint_pat)"
  local auth="Authorization: Bearer ${token}"

  local body http invocation
  body="$(curl -sS -o /tmp/elitea-kind-invoke.json -w '%{http_code}' \
    -X POST "${base}/api/v2/deepwiki/tools/${PROJECT_ID}/wikis/generate_wiki/invoke" \
    -H "$auth" -H 'Content-Type: application/json' \
    -d "{\"configuration\":{\"parameters\":{\"repository\":\"acme/e2e-generated\",\"branch\":\"main\",\"llm_model\":\"gpt-4o-mini\",\"code_toolkit\":${CODE_TOOLKIT_ID}}},\"parameters\":{\"query\":\"GO\",\"planner_type\":\"cluster\",\"exclude_tests\":true}}")"
  http="$body"
  if [ "$http" != "200" ] && [ "$http" != "202" ]; then
    cat /tmp/elitea-kind-invoke.json >&2; echo >&2
    fail "invoke answered HTTP ${http}"
  else
    invocation="$(python3 -c 'import json,sys; print(json.load(open("/tmp/elitea-kind-invoke.json")).get("invocation_id",""))')"
    if [ -z "$invocation" ]; then
      cat /tmp/elitea-kind-invoke.json >&2; echo >&2
      fail "invoke answered ${http} with no invocation_id"
    else
      pass "invoke accepted: invocation_id=${invocation}"

      local status="" attempt
      for attempt in $(seq 1 120); do
        curl -sS -o /tmp/elitea-kind-poll.json \
          -H "$auth" \
          "${base}/api/v2/deepwiki/invocations/${PROJECT_ID}/wikis/generate_wiki/${invocation}" >/dev/null || true
        status="$(python3 -c 'import json,sys
try: print(json.load(open("/tmp/elitea-kind-poll.json")).get("status",""))
except Exception: print("")')"
        case "$status" in
          Completed) break ;;
          Failed|Stopped|Error) break ;;
        esac
        sleep 2
      done
      if [ "$status" != "Completed" ]; then
        cat /tmp/elitea-kind-poll.json >&2; echo >&2
        fail "the invocation settled as '${status:-<no status>}', not Completed"
      else
        pass "the invocation reached Completed"
      fi

      local keys
      curl -sS -o /tmp/elitea-kind-objects.json -H "$auth" \
        "${base}/api/v2/artifacts/objects/${PROJECT_ID}/${BUCKET}?prefix=$(python3 -c 'import urllib.parse,sys;print(urllib.parse.quote(sys.argv[1]))' "${WIKI_ID}/")&limit=200" >/dev/null
      keys="$(python3 -c 'import json
b = json.load(open("/tmp/elitea-kind-objects.json"))
for o in (b.get("items") or b.get("objects") or []):
    print(o.get("key",""))' 2>/dev/null || true)"
      echo "$keys" | sed '/^$/d' | sed 's/^/   /'
      local manifests pages
      manifests="$(echo "$keys" | grep -c 'wiki_manifest' || true)"
      pages="$(echo "$keys" | grep -c 'wiki_pages/' || true)"
      if [ "$manifests" -lt 1 ] || [ "$pages" -lt 1 ]; then
        cat /tmp/elitea-kind-objects.json >&2; echo >&2
        fail "the bucket holds ${manifests} manifest(s) and ${pages} page(s) under ${WIKI_ID}/"
      else
        pass "${manifests} manifest and ${pages} pages landed under ${WIKI_ID}/"
      fi
    fi
  fi
  stop_port_forward

  echo
  if [ "$failures" -ne 0 ]; then
    die "verify: ${failures} check(s) failed"
  fi
  say "verify: every check passed"
}

# ── down ─────────────────────────────────────────────────────────────────────

cmd_down() {
  say "Deleting the kind cluster ${CLUSTER}"
  kind delete cluster --name "$CLUSTER"
  note "the built images are left in ${ENGINE}; remove them with:"
  note "  ${ENGINE} rmi ${MAIN_IMAGE} ${HOST_IMAGE} ${ENGINE_IMAGE}"
}

case "${1:-}" in
  up)     cmd_up ;;
  verify) cmd_verify ;;
  down)   cmd_down ;;
  seed)   require_tools; seed ;;
  *) cat >&2 <<USAGE
usage: $0 {up|verify|down}

  up      create the kind cluster, build and side-load the images, install
          cert-manager and the CA issuer, the infrastructure, the chart, and
          seed the fixtures
  verify  prove: every pod Ready; the provider pod is the host + engine
          sidecar; the facade registered the provider; a fixture generation
          completes and its pages land in the artifact bucket
  down    delete the cluster
USAGE
     exit 2 ;;
esac

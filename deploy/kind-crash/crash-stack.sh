#!/usr/bin/env bash
# Kubernetes crash-rehearsal profile on kind (T1+). PREPARED, NOT RUN.
#
#   deploy/kind-crash/crash-stack.sh preflight   # checks + the approval list; changes nothing
#   deploy/kind-crash/crash-stack.sh up          # cluster, local image load, then network steps behind a gate
#   deploy/kind-crash/crash-stack.sh pin         # place the Supervisor and platform edge (chart has no keys)
#   deploy/kind-crash/crash-stack.sh faults      # print the KX fault commands
#   deploy/kind-crash/crash-stack.sh down        # delete the cluster
#
# Structure follows deploy/kind/kind-stack.sh. Every kind/kubectl/helm call names
# the cluster `elitea-crash` / context `kind-elitea-crash`; no other cluster or
# context is touched. Docker only (no podman).
#
# NETWORK GATE. Steps that pull from the network stop with a message unless
# ELITEA_CRASH_K8S_PULLS_APPROVED=1: cert-manager chart and images, the NATS
# chart dependency and its sidecars (nats-box if enabled, config-reloader), a
# registry for the @sha256 Supervisor reference, Calico, any third-party image
# not already in the local Docker image store.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"

CLUSTER="elitea-crash"
CONTEXT="kind-${CLUSTER}"
NS="${CRASH_NAMESPACE:-elitea}"
TAG="${CRASH_IMAGE_TAG:-crash}"
CERT_MANAGER_VERSION="${CERT_MANAGER_VERSION:-v1.19.2}"
NODE_IMAGE="kindest/node:v1.37.0@sha256:a1ed56cfb0e7b93589bdf97c8cd566405a265939e3620fc4f5de89adff580ae5"
COMPOSE_PROJECT="elitea-crash"
MIN_FREE_GIB=30
PLACEHOLDER_DIGEST="0000000000000000000000000000000000000000000000000000000000000000"
CP_NODE="${CLUSTER}-control-plane"
WORKER_NODE="${CLUSTER}-worker"
CMD=""

# Built locally (never pulled). Build from a branch that contains #1160 and run the
# binary-marker check from rules/replatform-delivery-gate.md before using as evidence.
LOCAL_IMAGES=(
  "ghcr.io/elitea-ng/elitea-main:${TAG}"
  "ghcr.io/elitea-ng/elitea-worker-rust:${TAG}"
  "ghcr.io/elitea-ng/elitea-llm-gateway:${TAG}"
)
# Third-party images the rendered chart and manifests reference. Loaded from the
# local Docker store when present; otherwise a pull, which is gated.
THIRD_PARTY_IMAGES=(
  docker.io/pgvector/pgvector:0.8.5-pg16-trixie
  docker.io/rustfs/rustfs:latest
  rustfs/rc:latest
  docker.io/library/postgres:17
  docker.io/library/traefik:v3.4
  docker.io/library/python:3.12.14-slim-trixie
  docker.io/library/nats:2.12.0
)

say()  { printf '\n\033[1m== %s\033[0m\n' "$*"; }
note() { printf '   %s\n' "$*"; }
die()  { printf '\nERROR: %s\n' "$*" >&2; exit 1; }

kc() { kubectl --context "$CONTEXT" "$@"; }
hm() { helm --kube-context "$CONTEXT" "$@"; }

# Stops the run before a step that would pull from the network.
gate() {
  local what="$1"
  if [ "${ELITEA_CRASH_K8S_PULLS_APPROVED:-0}" != "1" ]; then
    cat >&2 <<MSG

STOP: the next step pulls from the network and needs owner approval:
  ${what}
Nothing past this point was run. Get approval, then re-run with
  ELITEA_CRASH_K8S_PULLS_APPROVED=1 $0 ${CMD:-up}
(the script is resumable). Approval list: $0 preflight
MSG
    exit 3
  fi
  note "approved network step: ${what}"
}

require_tools() {
  for tool in docker kind kubectl helm python3; do
    command -v "$tool" >/dev/null 2>&1 || die "$tool is not on PATH"
  done
}

have_image() { docker image inspect "$1" >/dev/null 2>&1; }

approval_list() {
  cat <<'LIST'
Approvals needed before `up` can finish (set ELITEA_CRASH_K8S_PULLS_APPROVED=1 once granted):
  1. cert-manager chart (https://charts.jetstack.io) and its images (quay.io/jetstack/*)
  2. NATS chart dependency (https://nats-io.github.io/k8s/helm/charts/, helm dependency build)
     and its sidecar images: config-reloader; nats-box if enabled. The Prometheus
     exporter is OFF in values-nats-crash.yaml.
  3. A registry reachable from the kind nodes for the Supervisor @sha256 reference
     (set ELITEA_CRASH_SUPERVISOR_IMAGE=<registry>/<repo>@sha256:<digest>)
  4. Calico images, only when ELITEA_CRASH_CNI=calico (KX-06 and the preparation resolver
     NetworkPolicy, if kindnet does not enforce; run `kx.sh np-probe` first)
  5. Any third-party image from THIRD_PARTY_IMAGES missing from the local Docker store
Also required, not network: builds of the three local images (max 2 concurrent Docker
builds across sessions), and the compose crash stack stopped (16 GB Docker VM).
LIST
}

# ── preflight ────────────────────────────────────────────────────────────────

cmd_preflight() {
  require_tools
  say "Preflight (read-only)"
  local running
  running="$(docker ps -q --filter "label=com.docker.compose.project=${COMPOSE_PROJECT}")"
  if [ -n "$running" ]; then
    die "compose project ${COMPOSE_PROJECT} has running containers; compose and kind crash stacks are mutually exclusive (16 GB Docker VM). Stop it first."
  fi
  note "compose project ${COMPOSE_PROJECT}: no running containers"

  local free_kib free_gib
  free_kib="$(df -Pk /System/Volumes/Data | awk 'NR==2 {print $4}')"
  free_gib=$((free_kib / 1024 / 1024))
  [ "$free_gib" -gt "$MIN_FREE_GIB" ] || die "only ${free_gib} GiB free on /System/Volumes/Data; need more than ${MIN_FREE_GIB}"
  note "disk: ${free_gib} GiB free on /System/Volumes/Data"

  have_image "$NODE_IMAGE" || die "pinned node image is not present locally: ${NODE_IMAGE}"
  note "node image present: ${NODE_IMAGE##*@}"

  if kind get clusters 2>/dev/null | grep -qx "$CLUSTER"; then
    note "cluster ${CLUSTER} already exists (up is resumable)"
  fi
  local missing=0 image
  for image in "${LOCAL_IMAGES[@]}"; do
    if have_image "$image"; then note "image present: $image"; else note "MISSING (build it): $image"; missing=1; fi
  done
  for image in "${THIRD_PARTY_IMAGES[@]}"; do
    if have_image "$image"; then note "image present: $image"; else note "missing (pull needs approval): $image"; fi
  done
  case "${ELITEA_CRASH_SUPERVISOR_IMAGE:-}" in
    ""|*"@sha256:${PLACEHOLDER_DIGEST}") note "Supervisor image: NOT SET (ELITEA_CRASH_SUPERVISOR_IMAGE)" ;;
    *@sha256:*) note "Supervisor image: ${ELITEA_CRASH_SUPERVISOR_IMAGE}" ;;
    *) die "ELITEA_CRASH_SUPERVISOR_IMAGE must be a registry reference with @sha256:<64 hex>" ;;
  esac
  echo
  approval_list
  if [ "$missing" -ne 0 ]; then note "(local images missing: build before up)"; fi
}

# ── up ───────────────────────────────────────────────────────────────────────

ensure_cluster() {
  if kind get clusters 2>/dev/null | grep -qx "$CLUSTER"; then
    note "cluster ${CLUSTER} already exists"
  else
    say "Creating the kind cluster ${CLUSTER} (2 nodes, pinned node image)"
    kind create cluster --name "$CLUSTER" --config "${SCRIPT_DIR}/kind-config.yaml" --wait 180s
  fi
  say "Labelling nodes and freeing the control-plane taint"
  kc label node "$CP_NODE" elitea.ai/crash-role=platform --overwrite
  kc label node "$WORKER_NODE" elitea.ai/crash-role=execution --overwrite
  kc taint node "$CP_NODE" node-role.kubernetes.io/control-plane:NoSchedule- 2>/dev/null || true
  if [ "${ELITEA_CRASH_CNI:-kindnet}" = "calico" ]; then
    gate "Calico manifests and images (NetworkPolicy enforcement for KX-06 and for the Python preparation resolver policy elitea-python-preparation-network; kindnet may not enforce it); this script does not install it"
    die "recreate the cluster with disableDefaultCNI (see kind-config.yaml), install the approved Calico release by hand, then re-run"
  fi
}

load_images() {
  say "Loading images into the kind nodes"
  local image
  for image in "${LOCAL_IMAGES[@]}"; do
    have_image "$image" || die "build $image first (local build; never pulled)"
    note "loading $image"
    kind load docker-image "$image" --name "$CLUSTER" >/dev/null
  done
  for image in "${THIRD_PARTY_IMAGES[@]}"; do
    if ! have_image "$image"; then
      gate "docker pull ${image}"
      docker pull "$image" >/dev/null
    fi
    note "loading $image"
    kind load docker-image "$image" --name "$CLUSTER" >/dev/null
  done
  # The Supervisor reference is a registry @sha256 digest. kind cannot side-load a
  # digest reference, so the nodes pull it from a registry.
  case "${ELITEA_CRASH_SUPERVISOR_IMAGE:-}" in
    ""|*"@sha256:${PLACEHOLDER_DIGEST}") die "set ELITEA_CRASH_SUPERVISOR_IMAGE=<registry>/<repo>@sha256:<digest> (the placeholder in values-crash.yaml is refused)" ;;
  esac
  gate "registry pull of the Supervisor image ${ELITEA_CRASH_SUPERVISOR_IMAGE}"
}

install_cert_manager() {
  say "cert-manager ${CERT_MANAGER_VERSION}"
  if kc get deployment -n cert-manager cert-manager >/dev/null 2>&1; then
    note "already installed"
  else
    gate "helm chart https://charts.jetstack.io cert-manager ${CERT_MANAGER_VERSION} and quay.io/jetstack images"
    hm upgrade --install cert-manager cert-manager --repo https://charts.jetstack.io \
      --version "$CERT_MANAGER_VERSION" --namespace cert-manager --create-namespace \
      --set crds.enabled=true --wait --timeout 10m
  fi
  kc wait --for=condition=Available --timeout=300s \
    -n cert-manager deployment/cert-manager deployment/cert-manager-webhook
  local attempt
  for attempt in $(seq 1 30); do
    kc apply -f "${REPO_ROOT}/deploy/kind/manifests/ca-issuer.yaml" >/dev/null 2>&1 && break
    if [ "$attempt" -eq 30 ]; then kc apply -f "${REPO_ROOT}/deploy/kind/manifests/ca-issuer.yaml"; fi
    sleep 2
  done
}

install_infra() {
  say "PostgreSQL (PVC) and the object store"
  kc apply -f "${SCRIPT_DIR}/manifests/postgres.yaml"
  kc apply -f "${SCRIPT_DIR}/manifests/rustfs.yaml"
  kc -n "$NS" create secret generic elitea-main-db \
    --from-literal=database-url='postgres://elitea:elitea@postgres:5432/elitea?sslmode=disable' \
    --dry-run=client -o yaml | kc apply -f -
  kc -n "$NS" create secret generic elitea-main-storage-secrets \
    --from-literal=s3-access-key='elitea' --from-literal=s3-secret-key='elitea-dev-secret' \
    --dry-run=client -o yaml | kc apply -f -
  kc -n "$NS" create secret generic elitea-main-llm-gateway-secrets \
    --from-literal=gateway-identity-secret='crash-unused-gateway-identity' \
    --dry-run=client -o yaml | kc apply -f -
  kc -n "$NS" rollout status statefulset/postgres --timeout=300s
  kc -n "$NS" rollout status deployment/rustfs --timeout=300s
  kc -n "$NS" delete job rustfs-bucket-init --ignore-not-found >/dev/null
  kc -n "$NS" create job rustfs-bucket-init --image=rustfs/rc:latest --dry-run=client -o json -- sh -c \
    'rc alias set rustfs http://rustfs:9000 elitea elitea-dev-secret && rc mb --ignore-existing rustfs/elitea-artifacts' \
    | python3 -c 'import json,sys; j=json.load(sys.stdin); j["spec"]["template"]["spec"]["containers"][0]["imagePullPolicy"]="IfNotPresent"; j["spec"]["backoffLimit"]=6; print(json.dumps(j))' \
    | kc apply -f -
  kc -n "$NS" wait --for=condition=complete --timeout=180s job/rustfs-bucket-init
}

# The chart references Secrets this script does not mint. Refuse to install
# until they exist, and name them. Generators: deploy/runtime/install-material.sh,
# deploy/scripts/gen-runtime-certs.sh, gen-sandbox-certs.sh, gen-gateway-certs.sh.
require_operator_secrets() {
  say "Checking operator-provided Secrets"
  local missing="" s
  for s in elitea-main-auth-material elitea-runtime-material elitea-worker-material \
           sandbox-material elitea-llm-gateway-secrets; do
    kc -n "$NS" get secret "$s" >/dev/null 2>&1 || missing="${missing} ${s}"
  done
  [ -z "$missing" ] || die "missing Secrets in namespace ${NS}:${missing}. Create them (README \"Secrets\"), then re-run."
}

install_nats() {
  say "NATS (scale 1, PVC) and the bootstrap chart"
  gate "helm dependency build deploy/helm/nats (upstream nats chart) and nats sidecar images"
  helm dependency build "${REPO_ROOT}/deploy/helm/nats"
  hm upgrade --install elitea-nats "${REPO_ROOT}/deploy/helm/nats" -n "$NS" \
    -f "${REPO_ROOT}/deploy/helm/nats/values-scale1.yaml" -f "${SCRIPT_DIR}/values-nats-crash.yaml" \
    --wait --timeout 10m
  hm upgrade --install elitea-nats-bootstrap "${REPO_ROOT}/deploy/helm/nats-bootstrap" -n "$NS" --wait --timeout 5m
}

install_chart() {
  say "elitea chart (Worker rust, recovery on, Supervisor, Main x2)"
  local image="${ELITEA_CRASH_SUPERVISOR_IMAGE:?set ELITEA_CRASH_SUPERVISOR_IMAGE}"
  # Operator-private overlay (real digests, profilesSha256, resolverCidrs); never commit it.
  # values-crash.yaml carries zero-digest PLACEHOLDERS for the preparation and compiled profiles.
  local local_values="${SCRIPT_DIR}/values-crash.local.yaml"
  if [ -f "$local_values" ]; then note "overlay: ${local_values}"; else
    local_values=""
    note "WARNING: no values-crash.local.yaml; preparation/compiled digests, pins and resolverCidrs stay PLACEHOLDERS (those scenarios will fail closed)"
  fi
  hm upgrade --install elitea "${REPO_ROOT}/deploy/helm/elitea" -n "$NS" \
    -f "${REPO_ROOT}/deploy/helm/elitea/values-standalone.yaml" -f "${SCRIPT_DIR}/values-crash.yaml" \
    ${local_values:+-f "$local_values"} \
    --set "sandboxKubernetes.supervisor.image=${image}" \
    --wait --timeout 15m
  cmd_pin
}

cmd_up() {
  CMD=up
  cmd_preflight >/dev/null
  ensure_cluster
  kc create namespace "$NS" --dry-run=client -o yaml | kc apply -f -
  load_images
  install_cert_manager
  install_infra
  require_operator_secrets
  install_nats
  install_chart
  say "Up. Faults: $0 faults    Collectors: kubectl --context ${CONTEXT} -n ${NS} port-forward svc/elitea-nats 8222"
}

# The chart has no nodeSelector for the Supervisor or the platform edge
# (templates/sandbox/supervisor.yaml and templates/worker/platform-edge.yaml
# render none). Patch them after install. A later `helm upgrade` reverts this.
cmd_pin() {
  say "Pinning Supervisor and platform edge (gap G-CHART-PIN)"
  kc -n "$NS" patch deployment elitea-sandbox-supervisor --type merge \
    -p '{"spec":{"template":{"spec":{"nodeSelector":{"elitea.ai/crash-role":"execution"}}}}}'
  kc -n "$NS" patch deployment elitea-platform-edge --type merge \
    -p '{"spec":{"template":{"spec":{"nodeSelector":{"elitea.ai/crash-role":"platform"}}}}}'
}

cmd_faults() {
  cat <<FAULTS
Fault commands (context ${CONTEXT}, namespace ${NS}). Full table: deploy/kind-crash/faults/README.md
  KX-01  kubectl --context ${CONTEXT} -n ${NS} delete pod -l app.kubernetes.io/name=elitea-worker-python --grace-period=0 --force
  KX-05  kubectl --context ${CONTEXT} -n ${NS} rollout restart deploy/elitea-sandbox-supervisor
  KX-07  kubectl --context ${CONTEXT} -n ${NS} delete pod elitea-nats-0
  KX-09  kubectl --context ${CONTEXT} -n ${NS} delete pod <one of: kubectl get pod -l app.kubernetes.io/name=elitea-main>
  KX-12  python3 scripts/runtime/test_kubernetes_supervisor_recovery.py --context ${CONTEXT} --namespace ${NS} --worker-material <dir>
  KX-03  kubectl --context ${CONTEXT} drain ${WORKER_NODE} --ignore-daemonsets --delete-emptydir-data --grace-period=30 --timeout=120s ; kubectl --context ${CONTEXT} uncordon ${WORKER_NODE}
  KX-04  docker kill ${WORKER_NODE} ; docker start ${WORKER_NODE}
  KX-06  kubectl --context ${CONTEXT} apply -f deploy/kind-crash/faults/deny-worker-nats.yaml ; kubectl --context ${CONTEXT} delete -f deploy/kind-crash/faults/deny-worker-nats.yaml
  KX-08  see faults/README.md (destructive)
  container-only restart: docker exec ${WORKER_NODE} crictl ps ; docker exec ${WORKER_NODE} crictl stop <container-id>
FAULTS
}

cmd_down() {
  CMD=down
  require_tools
  say "Deleting the kind cluster ${CLUSTER}"
  kind delete cluster --name "$CLUSTER"
  note "local images are kept; the Docker store is shared"
}

CMD="${1:-}"
case "$CMD" in
  preflight) cmd_preflight ;;
  up)        require_tools; cmd_up ;;
  pin)       require_tools; cmd_pin ;;
  faults)    cmd_faults ;;
  down)      cmd_down ;;
  *) echo "usage: $0 {preflight|up|pin|faults|down}" >&2; exit 2 ;;
esac

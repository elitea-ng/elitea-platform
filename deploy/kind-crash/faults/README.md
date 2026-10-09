# Kubernetes faults (KX)

Scenario ids and classes come from `handoffs/crash-recovery-suite/DESIGN.md` §1.5. Commands are in
`kx.sh` (one function each; nothing runs unless named) and are printed by `crash-stack.sh faults`.
Context `kind-elitea-crash`, namespace `elitea`. Nodes: `elitea-crash-control-plane`
(platform), `elitea-crash-worker` (execution).

Classes: R resume, I idempotent retry, C reconcile, F typed failure, L lost, U unknown until measured, P pending.
"Current" is the expected class on this tree. "Approval" lists what the scenario needs beyond the profile's own pulls.

| Id | Command | Phase | Current | Target | Tier | Needs approval |
|----|---------|-------|---------|--------|------|----------------|
| KX-01 | `kx.sh kx01` (Worker pod force delete) | P03, P10 | F with stock Helm (D1); R·U with this profile's keys. Code cells: R·U if `agentNodeRecovery` renders, else EXPECTED-GAP F (G-WORKER-01) | R | K1 | no |
| KX-05 | `kx.sh kx05` (Supervisor rollout restart; `Recreate` gap vs `timeout+90`) | P10 | R·U | R | K1 | registry for the @sha256 Supervisor image |
| KX-07 | `kx.sh kx07` (NATS pod delete, PVC kept) | P01q, P10 | I·P / R·U | I / R | K1 | NATS chart + sidecars |
| KX-09 | `kx.sh kx09` (one of 2 Main pods, during SSE) | P03 | R·U | R | K1 | no |
| KX-12 | `kx.sh kx12` (Supervisor pod delete with a sandbox pod running; runs `scripts/runtime/test_kubernetes_supervisor_recovery.py`) | P10 | R·U (G-SUP-04) | R | K1 | registry; needs `CRASH_WORKER_MATERIAL` |
| KX-03 | `kx.sh kx03`, then `kx.sh kx03-restore` (drain `elitea-crash-worker`) | P10 | U | R | K2 | no |
| KX-04 | `kx.sh kx04`, later `kx.sh kx04-restore` (`docker kill` the node container) | P10 | F·G (G-SUP-02) | bounded typed F now, R/C later | K2 EXPECTED-GAP | no. Destructive to the node. |
| KX-06 | `kx.sh np-probe` first; then `kx.sh kx06-on`, ~90 s, `kx.sh kx06-off` (`deny-worker-nats.yaml`) | P10 | U | R | K2 | Calico only if the probe reports "NOT ENFORCED" |
| KX-08 | `kx.sh kx08` (delete NATS pod and its JetStream PVC) | P10 | L·G | R/F after F4 | K2 EXPECTED-GAP | Destructive; per-batch consent |
| KX-02 | `kubectl -n elitea rollout restart deploy/elitea-worker` with a 60 s turn | P03 | F (G-WORKER-05) | R after F6 | K1 EXPECTED-GAP | no. No script function. |
| KX-10 | `kubectl -n elitea rollout restart deploy/elitea-main` under 5 streams | P03 | I/R·U (G-MAIN-03) | R | K2 | no. No script function. |
| KX-11 | `kx.sh kx11` (PostgreSQL pod delete; PVC-backed) | P10 | U | R | K2 | no |
| KX-P09w / s / m | Pod delete while preparation is resolving (`preparation_resolving`): Worker, Supervisor, or one Main pod. Commands below | P09 | R/F2·P* (Worker, Main); R·P / F·G (Supervisor) | R | K1 | registry refs, package-registry egress, `resolverCidrs` |
| KX-P09h-w / s / m | Same, during hydration (`hydration_running`) | P09-hyd | R/F2·P* | R | K1 | same |
| KX-P10p-w / s / m | Same, during prepared execution (`prepared_execution_running`) | P10-prep | R·P (Supervisor); R/F2·U (Main) | R | K1 | same |
| KX-P09n | Resolver-isolation probe (below) | P09 | pass/fail | pass | K2 | enforcing CNI (Calico: approval); throwaway probe pod |
| KX-P11 | Main pod delete while a compiled snapshot is Publishing (`snapshot_publishing`). Commands below | P11 | R·P* | R | K1 | registry ref for the Rust image |

## Preparation and compiled scenarios (KX-P*)

Ids map to the compose catalog (`CR-{W,S,M}K-P09`, `-P09-hyd`, `-P10-prep`, `CR-MK-P11`). Prerequisites: the real
digests, pins and `resolverCidrs` in `values-crash.local.yaml`, preparation and compiled material in `sandbox-material`,
and a fixture that reaches the phase (a Python Code node with a package for P09, P09-hyd and P10-prep; the
`rust-compiled` fixture for P11). crashctl observes the trigger on the receipts (`preparation_resolving`,
`hydration_running`, `prepared_execution_running`, `snapshot_publishing`); the fault is the pod delete only.

```bash
K="kubectl --context kind-elitea-crash -n elitea"
# After the trigger fires, ONE of:
$K delete pod -l app.kubernetes.io/component=worker --grace-period=0 --force              # w
$K delete pod -l app.kubernetes.io/component=sandbox-supervisor --grace-period=0 --force  # s
$K delete pod "$($K get pod -l app.kubernetes.io/name=elitea-main -o name | head -1)" --grace-period=0 --force  # m, KX-P11
# Evidence: preparation Pods live in their own namespace; none may be left behind or duplicated.
kubectl --context kind-elitea-crash -n elitea-python-preparation get pods -o wide
$K rollout status deploy/elitea-worker --timeout=180s
$K rollout status deploy/elitea-sandbox-supervisor --timeout=180s
```

The Worker selector is `component=worker` on purpose; see the last Note about `kx01`.

KX-P11 relies on `publishingTtlSeconds: 300` so the Publishing row outlives the Main replacement. Expected: the same
snapshot becomes Ready and one execution follows (compose `CR-MK-P11`). Supervisor and Worker deletes in P11 are not in
the catalog.

### KX-P09n resolver-isolation probe (K2)

A Pod in `elitea-python-preparation` must reach only cluster DNS and `resolverCidrs` on TCP 443, nothing in the
platform namespace. Void unless `kx.sh np-probe` reported that the CNI enforces policy. `PROBE_IMAGE` is any image
already on the nodes with `bash`, `getent` and `sleep` (no pull, `imagePullPolicy: Never`). The namespace enforces Pod
Security `restricted`, hence the securityContext.

```bash
C="kubectl --context kind-elitea-crash"; P=elitea-python-preparation
$C -n $P get networkpolicy        # expect exactly elitea-python-preparation-network
$C apply -n $P -f - <<EOF
apiVersion: v1
kind: Pod
metadata: {name: prep-probe}
spec:
  serviceAccountName: elitea-code
  automountServiceAccountToken: false
  securityContext: {runAsNonRoot: true, runAsUser: 10001, seccompProfile: {type: RuntimeDefault}}
  containers:
    - name: probe
      image: ${PROBE_IMAGE:?set a node-local image}
      imagePullPolicy: Never
      command: [sleep, "3600"]
      securityContext: {allowPrivilegeEscalation: false, capabilities: {drop: [ALL]}}
EOF
$C -n $P wait --for=condition=Ready pod/prep-probe --timeout=60s
probe() { $C -n $P exec prep-probe -- timeout 5 bash -c "</dev/tcp/$1/$2" 2>/dev/null && echo "OPEN   $1:$2" || echo "CLOSED $1:$2"; }
$C -n $P exec prep-probe -- getent hosts pypi.org             # DNS must work
probe <IP-inside-resolverCidrs> 443                           # expect OPEN
probe <same-IP> 80                                            # expect CLOSED (443 only)
for ip in $($C -n elitea get pods,svc -o jsonpath='{range .items[*]}{.status.podIP}{" "}{.spec.clusterIP}{" "}{end}'); do
  for port in 443 4222 5432 8080 9443 9444 9445 9446 9447 9448; do probe "$ip" "$port"; done   # expect all CLOSED
done
probe "$($C get svc kubernetes -o jsonpath='{.spec.clusterIP}')" 443                               # API server: CLOSED
probe "$(docker inspect -f '{{.NetworkSettings.Networks.kind.IPAddress}}' elitea-crash-control-plane)" 6443  # node: CLOSED
$C -n $P delete pod prep-probe
```

With option (b) (`0.0.0.0/1` + `128.0.0.0/1`) the 443 checks against the platform namespace are OPEN by design: record the
run as "rehearsal-only, not isolated" and read only the other ports. Approval: none beyond the enforcing CNI.

## Container-only restart

A container restart keeps the pod and its `emptyDir` spool; a pod delete loses it (D2). Kind nodes are
containers running containerd, so `crictl` runs inside the node container (replaces the minikube `ssh` form
in DESIGN §1.5):

```bash
docker exec elitea-crash-worker crictl ps --name worker
docker exec elitea-crash-worker crictl stop <container-id>      # kubelet restarts it
# or: faults/kx.sh crictl-restart <pod>
```

## Notes

- KX-06 mechanism. The chart's `elitea-worker-netpol` is Ingress-only (checked in a render), so Worker pods have
  no egress policy. `deny-worker-nats.yaml` selects them for Egress and lists every destination except NATS.
  Kubernetes policies only add allows, so this works only because no other egress policy exists.
- KX-06 needs enforcement. `kx.sh np-probe` applies a deny-ingress policy to a throwaway pod and reports whether the
  CNI blocked a connection. kindnet's enforcement on this node image is unverified.
- KX-04 kills the node that hosts Worker, Supervisor and sandbox pods. Main, NATS and PostgreSQL stay up on the
  platform node. PostgreSQL on a `local-path` PVC is node-bound; do not kill the platform node.
- NATS pod and PVC names (`elitea-nats-0`, the JetStream claim) follow the upstream chart and are unverified.
- Worker faults select `app.kubernetes.io/component=worker`. The name label `elitea-worker-python` is shared with
  `elitea-platform-edge`, so a name selector would also delete the edge Pod.

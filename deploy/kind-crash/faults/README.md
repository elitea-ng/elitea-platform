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

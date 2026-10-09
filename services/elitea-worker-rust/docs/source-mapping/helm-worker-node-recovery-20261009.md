# Helm Worker node-recovery key (G-WORKER-01, 2026-10-09)

Closes gap G-WORKER-01 in [`../recovery-guarantees.md`](../recovery-guarantees.md) (work item F5 of the crash-recovery
suite design). Base: `main` at `58abb650c`.

## What the gap was, and what was already on main

The recovery inventory (2026-10-08) recorded that the Helm chart had no key for the Worker runtime setting
`agent_node_recovery`, while `deploy/runtime/worker-runtime.rust.json` sets it to `true` for compose. #1159
(`00b8a259b`) then added `worker.runtime.agentNodeRecovery` (default `false`) and one assertion in
`render-worker-sandbox.sh`, without updating the inventory. On `main` the key still had three defects:

- **Silent coercion.** The template used `{{ if $r.agentNodeRecovery }}`. `--set-string ...agentNodeRecovery=false`,
  or a quoted `"false"` in a values file, is a non-empty string, so it is truthy. The journal, and with it model-step
  resume, turned on with no warning. `null` and `1` were also accepted.
- **No dedicated proof.** The only assertion covered `true` together with sandbox profiles. Nothing covered the
  default, `false`, the Python worker, or wrong types. No task ran `render-worker-sandbox.sh`.
- **Stale docs.** `recovery-guarantees.md` D1, note 2, the P09 row and the rank-1 backlog row still said the key
  did not exist.

## Default: stays `false` (deliberately differs from compose)

The task allowed `true` (as in compose) or a different default with a justification. The default stays `false`:

1. **The key also turns on model-step resume.** A node-recovery claim sends `node_recovery = true` and
   `agent_model_checkpoint_recovery = false` (`services/elitea-worker-rust/src/protocol/node_recovery_inspection.rs:30-31`).
   Main refuses a claim with both flags set (`services/elitea-main/internal/infra/db/repos/claims.go:43`). For an
   agent turn with no node visit, Main grants `RecoverAgentModelCheckpoint`
   (`services/elitea-main/internal/infra/db/repos/node_recovery_claim.go:58-69,86-93`). The Worker routes that to
   `process_checkpoint_delivery` without consulting `agentModelCheckpointRecovery`
   (`services/elitea-worker-rust/src/execution/agent_delivery_processor.rs:143-147,348-349`). The existing Main test
   `TestNodeRecoveryPreFrontierClaimServiceKeepsExactVisitOrCheckpointInspection`
   (`services/elitea-main/internal/infra/db/repos/node_recovery_prefrontier_test.go:21-30`) pins this: "no visit
   before first receipt" leads to `ClaimRecoverAgentModelCheckpoint`. Re-run here: 6 `TestNodeRecoveryPreFrontier*`
   tests passed. So defaulting the node key to `true` would switch on all Worker-crash recovery for every stock
   Kubernetes install.
2. **That recovery is gated closed on Kubernetes.** Ruling D1 keeps recovery off by default. TG-12
   (`../testing-gaps.md`) and `../remaining-gates.md` ("Production activation requires replacement, reclaim, ...
   Kubernetes proofs") keep activation closed until another Worker can continue without the original process or
   its spool. Rank 1 of the backlog already said "change both defaults after ranks 3 and 4 pass". With the spool on
   an `emptyDir` (D2), a pod replacement with recovery on gives F or a lost HITL card (G-WORKER-02/03/13/14,
   G-ADV-07). The delivery gate forbids L for new work.
3. **Compose is not affected by D2 the same way.** A compose Worker restart keeps its spool volume, the container
   row of D2. That is why compose can run with the key on while Kubernetes cannot yet.

The cost of keeping it off: on stock Helm, effectful pipeline direct tool nodes are refused with
`pipeline.tool_unavailable` (`services/elitea-worker-rust/src/agents/graph/direct_tool.rs:432-436,1643-1644`), and
platform-brokered Code profiles are refused at Worker start (`services/elitea-worker-rust/src/config.rs:336-341`). An
operator who accepts the D2 rows sets `worker.runtime.agentNodeRecovery=true`. That needs no extra material: the
journal is in the AgentState database behind the mandatory `agent-checkpoint-connection` file. Flipping the default
is one line in `values.yaml`, plus one assertion in the new test, once ranks 3 and 4 pass.

## Business behaviour

- **Taken from the current platform:** nothing. The Python indexer worker has no node journal. This is a
  deployment-surface change on the new platform.
- **Not ported:** the current platform runs an interrupted effectful tool again from scratch ("lost, run again").
  Stock Helm here refuses effectful direct nodes rather than risk that.

## Changed paths

| Path | Change |
| --- | --- |
| `deploy/helm/elitea/templates/worker/configmap-runtime.yaml:57-75` | Refuses a non-boolean `agentNodeRecovery` (`kindIs "bool"`), with the message "must be true or false". The Python refusal is kept. `agent_node_recovery: true` is still written only when set: absent is the Worker's serde default `false` (`services/elitea-worker-rust/src/config.rs:101-102`). This also keeps the default `runtime.json` byte-identical for both implementations, which `render-worker.sh` requires. |
| `deploy/helm/elitea/values.yaml:3711-3724` | Documents the model-resume coupling, why the default is `false`, compose parity, the material, and the boolean-only rule. The value is unchanged (`false`). |
| `deploy/helm/tests/render-worker-node-recovery.sh` (new) | 11 assertions, with a derived floor (`scripts/lib/assertion-floor.sh`). |
| `Taskfile.yml` | New `helm:worker-node-recovery`, run by `helm:lint` after `helm:main-master-key`. `.github/workflows` is unchanged. |
| `services/elitea-worker-rust/docs/recovery-guarantees.md` | D1 (key, guard, test, coupling), note 2, the Worker × P09 row, and backlog rank 1 (G-WORKER-01 closed; defaults still gated by ranks 3 and 4). |
| `services/elitea-worker-rust/docs/direct-tool-effects-design.md` | §6: the corrected reason for the `false` default. |

## Tests

Local: helm v4.1.0+g4553a0a, Python 3 with PyYAML, Go toolchain from `go.work`, macOS arm64, `58abb650c` plus this
change.

`render-worker-node-recovery.sh`: 11 ran, 11 passed.

| Case | Expected |
| --- | --- |
| Rust, default | renders; no `agent_node_recovery` key (the Worker reads false) |
| Python, default | renders; no key |
| Rust, `false` | no key |
| Rust, `true` | `true`, and `agent_checkpoint_connection_path` = `/run/elitea-runtime/agent-checkpoint-connection` and that file is in the init container's required material |
| Python, `false` | renders; no key |
| Python, `true` | refused: "require the Rust worker" |
| Rust, `null` | refused: "must be true or false" |
| Rust, the string `"false"` | refused |
| Rust, the number `1` | refused |

Mutation proof (each applied to the template, run, then restored):

| Mutation | Caught by |
| --- | --- |
| `main`'s template (no type guard) | 3 FAIL: `null`, `"false"`, `1` not refused |
| Python refusal dropped for `agentNodeRecovery` | "python, true: the render was not refused" |
| Key always written for Rust (`false` explicit) | 2 FAIL in this script, plus `render-worker.sh` "runtime.json differs between implementations" |

Full Helm run after the change:

| Run | Result |
| --- | --- |
| `helm lint` on `deploy/helm/{elitea,nats,nats-bootstrap}` | 3/3 pass |
| All 20 `deploy/helm/tests/render-*.sh` + `render-compiled-snapshots.py` | 21/21 pass |
| `render-worker.sh` | 8/8 |
| `render-worker-sandbox.sh` | pass |
| `render-nats-security.sh` | 264 assertions, 0 failed |
| `helm:code-consumers` (`scripts/runtime/test_code_consumers_deployment.py`) | 7 tests OK |
| `scripts/lib/assertion-floor-test.sh` | 19 checks, 0 failed |
| `go test ./internal/infra/db/repos/ -run TestNodeRecoveryPreFrontier` (coupling evidence) | 6 PASS |

Skips:
- The `task` binary is not installed locally, so each `helm:lint` command was run directly. The new task's YAML was
  checked with `yq`.
- `render-nats-security.sh` needs the NATS subchart. It ran with `helm repo add nats` in a throwaway `HELM_*` home.
  Without that it fails locally with "no repository definition", an environment issue.
- No Rust build or test: no Rust source changed.

## Performance / Durability / Resilience / Security

| Property | Mechanism | Proof | Measured |
| --- | --- | --- | --- |
| Performance | No runtime change; the default render is byte-identical to `main`. When an operator enables the key, the costs are: one journal open plus one load per Code-node visit, and one fenced append (about 6-9 statements in one transaction) before and after each effectful direct-tool call (`services/elitea-worker-rust/src/state/postgres_checkpointer.rs:392-411,477-560`). LLM nodes and read-only tools are never journaled. | `render-worker.sh` (identical default `runtime.json`) | Render test: about 1 s for 11 cases. The journal cost is derived from code, not measured here. No statement-count test exists for it (follow-up). |
| Durability | The default does not open a recovery path whose pod-replacement behaviour is unproven (D1/D2). When on, intent is recorded before the effect and a started effect is never repeated (unchanged, #1159). | Coupling: `node_recovery_prefrontier_test.go:21` | — |
| Resilience | A non-boolean value is a typed render failure that names the key, instead of a silent switch. `true` without a durable store cannot render: `agent-checkpoint-connection` is mandatory material, and the Worker refuses the config otherwise (`config.rs:399-403`). | `render-worker-node-recovery.sh` refusal cases and `journal_backed` | 3 refusals hold; the mutation shows that `main` accepts all 3 |
| Security | No secrets, no new material, no egress, no route. The connection is still passed by file reference (`agent_checkpoint_connection_path`), never inlined. Fail closed: a wrong-typed or Python-incompatible value refuses to render. | Same script | — |

Security-review categories (`rules/security.md`):
- **Identity, authorization, injection, egress, parsing amplification:** not applicable. There is no route, RPC,
  parser of untrusted input or outbound call. Values are operator-supplied at install time.
- **Secrets:** the diff was scanned; there are no tokens or keys. Credentials stay by reference.
- **Fail-closed config:** improved, through the type guard.
- **Supply chain:** no dependency change. No `govulncheck`, `cargo deny` or `npm audit` delta is possible.

## Recovery guarantee rows

| Component × phase | Before | After | Enforcing code | Proof |
| --- | --- | --- | --- | --- |
| Worker (Kubernetes, stock Helm) × P02, P03, P09-P12 | F (D1) | **F, unchanged by decision.** Recovery stays closed until ranks 3 and 4 pass. | `values.yaml:3724`, `configmap-runtime.yaml:62-75` | `render-worker-node-recovery.sh` "rust, default" |
| Worker (Kubernetes, `agentNodeRecovery=true`) × P02, P03, P09-P12 | could be enabled, but `"false"` also enabled it | R (container restart, spool kept) / F on pod replacement (D2). Only an explicit boolean `true` enables it. | same | "rust, true" plus the 3 refusal cases |
| Worker (compose) × same phases | R/F as recorded | unchanged | `deploy/runtime/worker-runtime.rust.json:21` | — |

No row moves to L. G-WORKER-01 (the key) is closed. G-ADV-02 (the default) stays open behind ranks 3 and 4.

## Browser evidence

Not applicable, and not run. The default render is byte-identical to `main` (`render-worker.sh`), so a stock install
behaves exactly as before. The only behaviour change is a render-time refusal of mis-typed values, which no browser
can observe. Exercising `agentNodeRecovery=true` on Kubernetes is KX-01 of the crash-recovery suite (WP-5). It needs
the minikube profile, which waits for user approval (DESIGN §7).

## Fixtures

None. Every case is a `helm template` of the in-repo chart with `values-standalone.yaml` and `--set` overrides.

## Follow-ups

- Flip `agentNodeRecovery` (and `agentModelCheckpointRecovery`) to `true` once ranks 3 and 4 pass. Change the
  "rust, default" assertion in the same PR.
- `agentModelCheckpointRecovery` has the same truthy-string defect (`configmap-runtime.yaml:65,70`). The same
  `kindIs "bool"` guard and test belong in a separate change.
- The Worker and Main keep the coupling implicit. The rendered config cannot show that `agent_node_recovery` implies
  model checkpoint inspection. A typed config field or a startup log line would make it visible.
- No statement-count budget test exists for the node journal append (`postgres_checkpointer.rs:477-560`).
- `render-worker-sandbox.sh` and `render-worker.sh` are still not in `Taskfile.yml` `helm:lint`.
  `.github/workflows/helm-lint.yml` coverage was not changed (no CI changes in scope).

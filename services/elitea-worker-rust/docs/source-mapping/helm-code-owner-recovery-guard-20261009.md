# Helm guard: node recovery with Code needs original-Code owner recovery (2026-10-09)

Base: `main` at `f7c6a6028`. Found by the crash-recovery suite session (2026-10-09, compose stack on merged `main`
`58abb650c`). Related: D1 and G-ADV-01 in [`../recovery-guarantees.md`](../recovery-guarantees.md),
[Helm Worker node-recovery key](helm-worker-node-recovery-20261009.md) (G-WORKER-01).

## The finding

The Rust Worker ran with `agent_node_recovery: true` and sandbox Code runtimes, while elitea-main had no original-Code
owner recovery (`ELITEA_RUNTIME_CODE_OWNER_RECOVERY_ENABLED` / `_CONFIG`,
`services/elitea-main/internal/runtimecomposition/code_owner_config.go:32-55`). Every Code node then failed about 3 ms
after start, with node failure class `authorization_denied`, before any sandbox dispatch. The user saw the generic
`INTERNAL` "The runtime operation failed." (the same shape as G-ADV-01).

Why it happens:
- With the node journal on, a Code node runs as a node attempt
  (`services/elitea-worker-rust/src/agents/graph/code_runtime.rs:274-330`, `NodeAttemptBody for CodeNode`). The
  remote runtime asks Main to admit the original-Code visit before it prepares or dispatches anything
  (`services/elitea-worker-rust/src/agents/graph/code_attempt_remote.rs:69-114`).
- Main serves that admission only when owner recovery is composed. Without it,
  `PostOriginalCodeVisit` answers 404 (`services/elitea-main/internal/infra/storage/runtime_code_intent.go:204-206`),
  and the Worker classifies the admission refusal as `AuthorizationDenied` at the `Admission` phase.

Why the chart did not catch it:
- `deploy/helm/elitea/templates/worker/configmap-runtime.yaml:57-80` renders `agent_node_recovery` and
  `sandbox_runtimes` from `worker.runtime`.
- `deploy/helm/elitea/templates/_code-nodes.tpl` (`elitea.codeNodes.validate`) checks `main.runtime.codeOwnerRecovery`
  on its own.
- So `agentNodeRecovery=true` + non-empty `sandboxRuntimes` + `codeOwnerRecovery.enabled=false` rendered without
  error, and every pod would report Ready. `render-worker-sandbox.sh` itself rendered exactly that combination as its
  "good" fixture.

## Business behaviour

- **Taken from the current platform:** nothing. The current platform has neither a node journal nor an
  original-Code owner. This is a deployment contract of the new platform only.
- **Not ported:** the current platform's "fails at run time with a generic error" deployment mode. The chart now
  refuses the broken combination at render time, which is the chart's stated policy (`templates/guards.yaml:1-10`).

## Changed paths

| Path | Change |
| --- | --- |
| `deploy/helm/elitea/templates/_code-nodes.tpl:111-134` | New `elitea.codeNodes.validateWorker`. It fails when `worker.enabled` and `worker.runtime.agentNodeRecovery` is boolean `true` and `worker.runtime.sandboxRuntimes` is non-empty and `main.runtime.codeOwnerRecovery.enabled` is not `true`. The message says that Code nodes would be refused without original-Code owner recovery, before any sandbox dispatch, and names the three ways out. With owner recovery on, it also fails for every `sandboxRuntimes[].audience` that has no `codeOwnerRecovery.supervisors` entry, because Main signs original-Code intents only for its owner supervisors' audiences (`services/elitea-main/internal/runtimecomposition/code_owner_config.go:100-110`, `services/elitea-main/internal/infra/db/repos/code_sandbox_intent.go:414`). That check was added after code review. |
| `deploy/helm/elitea/templates/guards.yaml:13` | Includes it next to `elitea.compiledSnapshots.validate`, so every render runs it whichever templates are shown. |
| `deploy/helm/elitea/values.yaml:3726-3728` | The `agentNodeRecovery` comment states the pairing with `main.runtime.codeOwnerRecovery`. Values unchanged. |
| `deploy/helm/tests/render-worker-node-recovery.sh:31-40,176-276` | Header rule and 8 new assertions (12 → 20; derived floor). |
| `deploy/helm/tests/render-worker-sandbox.sh:7-19,67-73` | Its fixture is now a valid deployment: owner recovery for both supervisor audiences. A new check shows that the same fixture with owner recovery off is refused with the new message. CI runs this script (`.github/workflows/helm-lint.yml:248`); CI is not changed. |
| `deploy/runtime/sandbox-deployment.md:199-200` | Operator note: the journal with sandbox runtimes needs `main.runtime.codeOwnerRecovery`. |
| `services/elitea-worker-rust/docs/recovery-guarantees.md` D1 | Line references refreshed (`values.yaml:3712-3713`, `:3714-3730`). New bullet: the Code owner contract, the guard, the proofs, the compose caveat, and a link to G-ADV-01. The stale "the chart has no key for it" now reads: the key exists since `00b8a259b` and defaults to false, so G-WORKER-01 is "default off", not "no key". |

The guard deliberately does not check `worker.implementation`. A Python worker with these options is already refused by
`configmap-runtime.yaml:67-70` ("require the Rust worker"), and an `isRust` condition survived mutation (see Tests),
so it was removed as redundant.

## Decision: chart guard, not a Worker startup refusal; failure-kind mapping deferred (task item 3)

**Worker startup refusal: rejected.**
- The Worker cannot observe Main's composition. Owner recovery is Main's environment
  (`code_owner_config.go:32-55`), Main's mTLS client material and Main's supervisors.
- The Worker would have to be told by a duplicate flag in `runtime.json`, such as "Main has owner recovery". That
  flag proves nothing about the real Main, and it can drift the same way the two blocks drifted here.
- The only place that sees both sides is the chart. `guards.yaml:70-72` already requires `main.runtime.enabled` for
  a Worker in the same release, so `main.runtime` is always the Main this Worker talks to.
- A real runtime probe (the Worker asking Main for its Code capabilities at claim time) would need a new protocol
  field. That is out of scope; it is recorded as a follow-up below.

**Readable `RuntimeFailureKind` instead of `INTERNAL`: agreed as the right end state, deferred.**
- A deployment gap should give a typed, readable failure ("Code nodes are not available in this deployment; ask your
  administrator"), not "The runtime operation failed.".
- Every file that mapping touches is changed by open PR #1084, which this off-`main` track must not touch (`00-COMMON.md`):
  - `services/elitea-worker-rust/src/agents/graph/code_runtime.rs`;
  - `code_attempt_remote.rs`;
  - `code_remote.rs`;
  - `node_recovery_runtime.rs`;
  - `services/elitea-worker-rust/src/protocol/output.rs`.
- It also belongs with the G-ADV-01 work (rank 5, typed `RecoveryRefused`), so the two refusals get one consistent
  set of kinds.
- Recommended shape:
  - Main answers the disabled endpoint with a typed "not configured" refusal instead of a bare 404.
  - The Worker maps it to `NodeFailureClass::InvalidConfiguration`.
  - The node failure surfaces as the existing `PipelineCodeFailed` or `PipelineNodeTypeNotAvailable` kind, never
    `Internal`.
  - Proved by a unit test on `intent_failure` and a Main handler test.

## Tests

Local: helm v4.1.0+g4553a0a, Python 3 with PyYAML, macOS arm64, `f7c6a6028` plus this change.

TDD:
- The first 6 new assertions were written before the guard: 18 ran, 16 passed, 2 failed (both refusal cases rendered).
  With the guard: 18/18.
- The per-audience pair was added after code review, also test first: 20 ran, 19 passed, 1 failed (an owner
  supervisor missing for `dns:sandbox-rust` rendered). With the audience check: 20/20.

`render-worker-node-recovery.sh`, new cases:

| Case | Expected |
| --- | --- |
| Rust, journal + sandbox runtimes, owner recovery default (off) | refused, "Code nodes would be refused without original-Code owner recovery" |
| Same, `main.runtime.codeOwnerRecovery.enabled=false` explicit | refused, same message |
| Rust, journal, no sandbox runtimes, owner off | renders, `agent_node_recovery: true` |
| Rust, sandbox runtimes, no journal, owner off | renders, no `agent_node_recovery` key |
| Rust, journal + sandbox runtimes + owner recovery | renders. The Worker gets both, and Main's ConfigMap has `ELITEA_RUNTIME_CODE_OWNER_RECOVERY_ENABLED: "true"` |
| Rust, journal, two sandbox runtimes, owner supervisor only for `dns:sandbox-python` | refused: no `codeOwnerRecovery` supervisor for audience `"dns:sandbox-rust"` |
| Rust, the same two runtimes and owner, no journal | renders (no original-Code visit is made) |
| Python, journal + sandbox runtimes | refused as Rust-only ("require the Rust worker"), not as a Code owner gap |

`render-worker-sandbox.sh`: the existing checks pass with the corrected fixture, and the refusal check passes.

Mutation proof (each applied to the template, run, then restored):

| Mutation | Caught by |
| --- | --- |
| `include` removed from `guards.yaml` | node-recovery: 2 FAIL (both refusals); sandbox: FAIL |
| journal condition dropped | node-recovery: "sandbox runtimes without the journal" FAIL |
| sandboxRuntimes condition dropped | node-recovery: "rust, true" and "journal without sandbox runtimes" FAIL |
| owner condition dropped | node-recovery: "journal, sandbox runtimes and owner recovery" FAIL; sandbox: FAIL |
| `isRust` condition dropped | **not caught**, so it was redundant; removed from the change (see above) |
| per-audience loop absent (the state before code review) | node-recovery: "owner recovery for one of two sandbox audiences" FAIL |

Also checked by hand: `worker.enabled=false` with the same worker values renders (the guard is inert without a Worker).

Full Helm run with the final change:

| Run | Result |
| --- | --- |
| All 21 `deploy/helm/tests/render-*.sh` + `render-compiled-snapshots.py` | 22/22 pass |
| ... including the 4 not wired into CI: `render-main-master-key.sh`, `render-network-policies.sh`, `render-platform-edge-identity.sh`, `render-sandbox-images.sh` | pass |
| `render-worker-node-recovery.sh` | 20/20 |
| `render-worker.sh` | 8/8 |
| `render-bf0-2b.sh` | 23/23 |
| `render-nats-security.sh` | 264 assertions, 0 failed |
| `helm lint` on `deploy/helm/{elitea,nats,nats-bootstrap}` | 3/3 pass |
| `scripts/runtime/test_code_consumers_deployment.py` (`helm:code-consumers`) | 7 tests OK |
| `scripts/lib/assertion-floor-test.sh` | 22 checks, 0 failed |
| Crash suite kind values (`origin/test/crash-recovery-suite-t0:deploy/kind-crash/values-crash.yaml`), layered as `crash-stack.sh` layers them | renders as committed, and with a sandbox runtime filled in. With `codeOwnerRecovery` removed, the new guard refuses it. |

Skips:
- `task` is not installed locally, so the scripts were run directly.
- `render-nats-security.sh` ran with `helm repo add nats` in a throwaway `HELM_*` home.
- No Rust or Go build or test: no Rust or Go source changed.

## Performance

Not applicable at run time: render-time template logic only. The default render is unchanged; every existing render
script passes byte-for-byte assertions such as `render-worker.sh`. The guard is one `if` over four already-loaded
values. The 20-case script runs in about 1-2 s locally.

## Durability

- No crash window is created or changed.
- The guard removes a deployment in which the journal is on but cannot be used for Code. In that deployment, Code
  nodes never reach the durable original-Code intent (`code_attempt_remote.rs:95-114`), so they have no R path at
  all.
- With the guard, a Helm install that enables the journal for Code also composes the owner. Owner recovery is the
  durable record that lets a replacement Worker reconcile the original job instead of repeating it.

## Resilience

- A misconfiguration that looked healthy (every pod Ready, every Code node failing at admission) is now a typed,
  named refusal at `helm template` / `helm install`, with the fix in the message.
- Proven by the refusal and mutation cases above.
- The run-time failure is still `INTERNAL`. This affects compose, and any Kubernetes install that bypasses the
  chart. The readable mapping is the deferred item above.

## Security

`rules/security.md` categories:
- **Identity and trust boundaries:** not applicable; no route, header or credential path changed. The guard leads
  operators to the mTLS-backed owner identity (`mainWorkloadIdentity`, supervisors). It never relaxes it.
- **Authorization:** unchanged. Main's original-Code admission stays fail-closed (404 when not composed). The guard
  makes the deployment match it rather than weakening it.
- **Input / amplification / injection / egress:** not applicable. Values are operator-supplied at install time; no
  parser, query, URL or outbound call.
- **Secrets:** none added. Test fixtures use `.invalid` hosts and `spiffe://elitea.invalid/main`; the diff was
  scanned for token and key patterns.
- **Fail-closed config:** improved (the new render refusal).
- **Supply chain:** no dependency change, so no `govulncheck`, `cargo deny` or `npm audit` delta is possible.

## Reviews

`code-review` (high) on `origin/main...fix/helm-code-owner-recovery-guard` reported 6 findings:

| Finding | Outcome |
| --- | --- |
| The guard checked only `codeOwnerRecovery.enabled`, not that each sandbox runtime's audience has an owner supervisor. Main refuses intents for other audiences. | **Fixed**: per-audience check plus 2 assertions (above). |
| With no `isRust` condition, the Python case's message depends on Helm's template order. | Kept. A Python worker with these options is refused either way. The "python, journal and sandbox runtimes" assertion pins today's message, so an order change shows up as a test failure, not as an accepted install. |
| The guard reads this release's `main.runtime` even when `main.enabled=false`. | No change. The chart already treats `main.runtime` as the Worker's Main whatever `main.enabled` says (`guards.yaml:70-72` requires `main.runtime.enabled` for any Worker). |
| Root cause at run time (bare 404 leading to `INTERNAL`) and compose are not fixed. | Deferred and recorded: see the Decision section and Follow-ups. The files involved belong to #1084. |
| Sandbox/owner fixtures are duplicated between the two scripts. | Kept. Each script stays self-contained, as the other render scripts are; the shapes are tiny. |
| The "all three" failure prints only the render's `.err` tail. | Kept. Diagnostic only; a failure is still reported and counted. |

`security-review`: the skill reviews the session's own repository, not this worktree, so it was not run as a skill.
The manual security pass is the Security section above. There is no route, parser, query, egress, secret or
dependency in the diff.

## Recovery guarantee rows

| Component × phase | Before | After | Enforcing code | Proof |
| --- | --- | --- | --- | --- |
| Worker (Kubernetes, Helm) × P09-P12 (Code), journal on, owner off or missing the runtime's audience | not R or I: every Code node fails at admission, before dispatch, as `INTERNAL`; installable | **not installable**: refused at render | `_code-nodes.tpl:111-134`, `guards.yaml:13` | `render-worker-node-recovery.sh` refusal cases; `render-worker-sandbox.sh` |
| Worker (Kubernetes, Helm) × P09-P12, journal + owner on | R (container) / F (pod replacement, D2) as recorded | unchanged | — | "journal, sandbox runtimes and owner recovery" case |
| Worker (compose) × P09-P12, journal on, `docker-compose.code-consumers.yml` not layered | F (typed admission refusal shown as `INTERNAL`) | **unchanged** (no render step) | — | follow-up |

No row moves to L. The matrix cells in `recovery-guarantees.md` §4 and §6 are unchanged. This PR changes D1 prose
only, so it does not conflict with the crash suite, whose cell diffs land after its T0 run (DESIGN WP-8). #1084's
change to the same file is the Main × P10 row, a different hunk.

## Browser evidence

Not run, deliberately. The change is a render-time refusal, which no browser can observe, and the default render is
unchanged. The positive path (Code with journal and owner on) is unchanged runtime behaviour, covered by the existing
Code owner-recovery evidence (`code-live-owner-recovery-20261008.md` on #1084). The negative path was observed live by
the crash-recovery suite session that reported the finding (compose, `main` `58abb650c`). A Kubernetes run is KX-01
of the crash suite, which waits for user approval of the kind profile.

## Fixtures

None stored. Every case is a `helm template` of the in-repo chart with `values-standalone.yaml`, inline values files
written into a temp directory, and `--set` overrides. Owner identity and origins are `.invalid` placeholders.

## Follow-ups

- **Readable failure kind** for a Code admission refused because owner recovery is not composed, together with
  G-ADV-01 rank 5. It touches only #1084 files, so stack it on #1084 or do it after #1084 merges. Shape: see the
  decision above.
- **Compose has no render guard.** `deploy/runtime/worker-runtime.rust.json:21` turns the journal on. Owner recovery
  comes only with `deploy/docker-compose.code-consumers.yml`, so a compose stack with the sandbox overlays but
  without that overlay is the failing combination. Options:
  - a check in `install-material.sh`;
  - turning on owner recovery in the sandbox overlay;
  - documenting the overlay as required with the sandbox overlays.
- **Runtime capability probe.** The Worker could learn at claim time whether Main admits original-Code visits, and
  refuse Code assembly with `PipelineNodeTypeNotAvailable` before the run starts. That needs a protocol field.
- `render-worker-node-recovery.sh` is still run only by `task helm:lint`, not by `.github/workflows/helm-lint.yml`.
  CI is not changed by user decision. `render-worker-sandbox.sh`, which CI does run, carries the refusal check too.

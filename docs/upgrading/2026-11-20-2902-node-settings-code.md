# Per-node settings for Code nodes and State Modifier transitional edges (5b-1 PR B)

**Main ships before the Worker.**

Do not deploy a Worker from this release ahead of Main. Main now admits `settings:` on saved Code declarations, strips `settings` alongside `recovery` and `failure_handler` when hashing the Worker's exact Code configuration, and validates ordinal dispatch identities for `settings.retry_on_fail` (`services/elitea-main/internal/infra/storage/runtime_saved_code_policy.go`, `services/elitea-main/internal/infra/db/repos/code_sandbox_intent.go`).

- **Old Main + new Worker (fails closed):** Before this release, Main's `MatchOriginalSavedCodeConfiguration` stripped only `recovery` and `failure_handler` (`runtime_saved_code_policy.go:334-335`) and `ReadSavedCodeDeclaration` admitted only the 14 keys without `"settings"` (`runtime_saved_code_policy.go:366`). If a new Worker submits a Code visit for a pipeline that declares `settings:`, an old Main refuses the visit closed (`ErrContentUnauthorized` / `ErrContentRejected`) and grants no sandbox execution.
- **New Main + old Worker (safe rollout window):** New Main remains backward-compatible with older Workers for pipelines without Code `settings:` and keeps `MatchCodeFailureRoute` unchanged. Older Workers continue refusing Code `settings:` at compile time until rolled forward.
- **Recovery journal schema v4 (`elitea.pipeline.node-recovery.v4`):** Journals that persist `NodeFailureClass::DeadlineInsufficient` (FU-309) or a run-deadline-clamped `FailureWithin` event (`Stop(ElapsedLimit)`) encode at schema `v4`. Existing journals and failures without those variants stay at their prior schema version. An older Worker refuses a `v4` journal closed as `pipeline.node_recovery.corrupt_journal` rather than silently misclassifying a deadline refusal.

## Behavior changes

- **Code node settings:** Code nodes now admit all eight keys from `libs/proto/contracts/pipeline-node-settings-v1.json`: `retry_on_fail`, `max_tries` (`2..=5`, default `3`), `wait_between_tries_ms` (`100..=300000`, default `1000`), `timeout_ms` (`1000..=900000`), `on_error` (`stop`, `continue`, `error_output`), `error_transition`, `always_output_data`, and `execute_once`. Combining `settings:` with `recovery:` or `failure_handler:` on the same node is refused at save, compile, and Main visit admission.
- **Effective Code timeout and C1 safety:** Effective execution timeout is `min(profile.timeout_seconds, ceil(timeout_ms / 1000))`. A pure isolated Code node that hits `sandbox.deadline_exceeded` retries when `retry_on_fail: true` (bounded by the remaining run deadline) and settles to `pipeline.code.timeout` when `on_error` is `continue` or `error_output`. A `platform_client: true` Code node that times out keeps `UnknownExternalEffect` and raises `Reconcile` with zero re-dispatches. Run-deadline exhaustion (`DeadlineInsufficient`, `Stop(ElapsedLimit)`), denials, cancellations, and `Reconcile`/`OperatorApprovalRequired` cards are never continued.
- **State Modifier transitional edges (FU-351):** State Modifier nodes with a declared `transition` now emit bounded, secret-redacted `agent_on_transitional_edge` events carrying `next_step` and post-step state (including `node_errors`), matching legacy SDK/UI timeline behavior without altering non-State-Modifier nodes.

## What to check

- Roll out `elitea-main` before rolling out `elitea-worker-rust`.
- No database migration is required.
- Existing pipelines without Code `settings:` keep byte-identical YAML, digests, and dispatch identities.

# Invalid preparation marker cleanup

## Source mapping

| Owner | Previous behavior | Change |
| --- | --- | --- |
| `sandbox/docker_preparation.rs::observe_preparer` | A malformed immutable marker returns Receipt, leaving the original dispatched job for reconciliation. | Catch only this parse failure and call the existing fenced failure path with `sandbox.preparation_invalid_receipt`. |
| `sandbox/docker_supervisor.rs::fail_expired_job` | Renew ownership, terminate the bound original runtime, honor cancellation, finish the receipt, then clean up. | Reused without changes. Failed is published only after confirmed termination. |
| `sandbox/runtime.rs::CodeJobRuntime` | Docker and Kubernetes implement the same observation, termination, and cleanup contract. | No backend-specific path is added. |
| `sandbox/ledger.rs` | Owner, epoch, live lease, phase clocks, and cancellation fence all writes. | SQL and authority remain unchanged. |
| `sandbox/docker_preparation_tests.rs` | Isolated PostgreSQL fixtures exercise the preparation lifecycle. | Seven additional tests cover malformed markers, termination errors, fencing, cancellation, observation errors, and persistence errors. |

Transient runtime observation and bundle-write errors retain the existing
recoverable job. A malformed immutable marker cannot become valid on retry, so
it takes the terminal path after the original runtime stops. Concurrent Stop
produces Cancelled. Fencing before termination prevents a stale owner from
stopping the runtime; fencing during termination prevents its terminal write.
The Failed receipt contains neither a result nor a recorded bundle.

## Verification

All 17 actual PostgreSQL preparation tests pass, with zero failed or ignored
cases. They include the seven new regressions and ten previous lifecycle tests.
The notification-gated tests avoid added sleep-based synchronization.
The existing raw Linux marker fixture acceptance remains unchanged.
Strict all-target, all-feature locked offline Clippy and formatting pass.
These fixtures use an in-process runtime double; they prove persistence and
ownership boundaries, not real Docker or Kubernetes termination.

The cleanup correction is not yet deployed. Both runtime backends still require
malformed-marker acceptance with the exact original container ID or Pod UID,
confirmed termination before Failed, absent published content, and no redispatch
when the original activation is retried.

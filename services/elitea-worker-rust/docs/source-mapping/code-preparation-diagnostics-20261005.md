# Code preparation failure diagnostics

Date: 2026-10-05.

## Behavior

The Worker preserves a finite preparation failure cause through the current fenced Code attempt.
The cause identifies failed, cancelled, or unconfirmed dependency preparation.
The Worker emits the cause after the durable failure append commits.
The cause uses the existing `PIPELINE_CODE_FAILED` wire code.

The unconfirmed message states that the preparation job was not restarted.
The message tells the user to reconcile the existing attempt before retrying.
The Main output receiver must allow the exact new safe messages.
Main must adopt this allowlist before the updated Worker emits these messages.

## Current to new mapping

| Current source | Current behavior | New source | New behavior |
| --- | --- | --- | --- |
| `src/agents/graph/code_preparation.rs` | Preparation returns `GraphError`. Owner failure text can enter this error. | Same file | Preparation returns a finite diagnostic. Raw owner text cannot enter it. |
| `src/agents/graph/code_attempt_remote.rs` | Preparation discards the error and retains `Preparation` with class `Unknown`. | Same file | Preparation retains its diagnostic. Failure class and replay facts stay the same. |
| `src/agents/graph/code_runtime.rs` | The typed attempt discards its phase detail when it returns `NodeFailure`. | Same file | The attempt carries an optional safe terminal code. |
| `src/agents/graph/node_recovery_runtime.rs` | A committed stop returns a generic graph error. | Same file | A Code reporter emits its safe cause after the failure append commits. |
| `src/agents/graph/node_events.rs` | Existing root and child drains preserve queued failure codes. | Existing source | The Code reporter uses this existing bounded channel. |
| `src/protocol/output.rs` | `agent.legacy` maps to the generic internal message. | Same file | Three finite preparation codes map to exact Code failure messages. |
| `src/agents/application_tools.rs` | A known child failure can become a bounded parent report. | Same file | Unconfirmed preparation asks for administrator reconciliation. |
| `services/elitea-main/internal/transport/runtimegrpc/output/server.go` | Main accepts one exact Code failure message. | Main-owned proposed hunk | Main accepts three additional exact Code preparation messages. |

## Failure and recovery boundaries

The default reporter is inactive for all other node bodies.
The Code reporter runs only for a committed terminal stop.
It does not run for automatic retry, error routing, operator approval, or reconciliation.
Append failure and lease loss publish no terminal cause.
The patch does not change cancellation decisions, durable ledger fields, or recovery policy.

The diagnostic is local to the current attempt.
An already restored terminal ledger keeps its existing generic fallback.
The durable terminal output can replay its exact safe message after the current attempt publishes it.
This patch does not add a durable phase field to the node ledger.

## Timing and identity

Preparation uses the existing fixed `timeout_seconds + 90` observation deadline.
A configured timeout of 120 seconds therefore gives 210 seconds.
Transport retries do not extend this deadline or create another preparation activation.
The publication deadline starts once the preparation receipt supplies a frozen bundle.
Hydration uses its own existing bounded deadline.

The existing lifecycle event binds phase, activation, execution, generation, node, thread, and graph step.
The terminal Code log adds the static phase and error code to the activation and attempt.
The existing native failure log retains execution and generation.
The existing output frame carries the support correlation.
No source, selected state, SQL, credential, or raw owner error enters the new message or log.

## Implementation history and verification

This change follows the Code runtime startup and workspace activation composition.
The startup policy, signed original visit, and operator configuration stay the same.
The private native Worker test link uses the current source and pinned existing dependency artifacts.
Eight new diagnostic tests and twenty existing focused tests pass.
The tests cover native graph conversion, child drainage, safe messages, append failure, lease loss, and recovery decisions.
The tests use in-memory stores and runtime doubles.
They do not prove deployed database, Docker, Kubernetes, or browser behavior.

Main allowlist tests are proposed separately for the Main owner.
The Main owner must run those tests before deployment.

# Toolkit cancellation acceptance

## Source mapping

| Current platform source | Replatform source | Contract |
| --- | --- | --- |
| Core `api/v2/test_toolkit_tool.py` | Main `application/toolkitcalltool/service.go` | Execute a named operation and report its outcome. |
| Current administrative task stop | Main `api/v2/admin/background_jobs.go` and `infra/db/repos/admin_background_jobs.go` | Request durable cancellation of an active toolkit job. |
| Worker task cancellation | Rust `execution/toolkit_delivery_processor.rs` and claim lease monitor | Stop work after observing cancellation and publish a cancelled terminal result. |

Core paths are relative to `projects/centry/pylon_main/plugins/elitea_core`.
Main and Rust paths are relative to their service `internal` and `src` directories.

## Deployed proof: 2026-09-11

A temporary TLS fixture delays its synthetic response for 45 seconds.
The fixture uses the existing trusted test certificate without restarting the OAuth emulator.
A temporary saved OpenAPI toolkit references the existing synthetic client credential.
The browser starts the named operation from the active Toolkit Test pane.

Execution `bf394b490ea82922ae8806b788907a3f` reaches `RUNNING`.
The existing administrative cancel endpoint returns 200 for this execution.
The worker observes cancellation, and both durable state and desired state become `CANCELLED`.
The Test pane displays `Execution was cancelled.`
The synthetic server observes client disconnection.
A repeated cancellation request returns 409 for the settled job.

An initial run, `8c5e190b290bd9643943e447ffe137a2`, settles before cancellation is issued.
It is not cancellation evidence. The deliberate second run supplies the proof above.
The temporary TLS server stops automatically. The temporary toolkit is removed after verification.

## Remaining boundary

This proves cancellation through the existing administrative route after provider invocation starts.
It does not prove rollback of a provider-side effect.
The active Toolkit Test pane has no user-facing Cancel control.
That UI/API integration remains open and must preserve project and actor authorization.
Worker replacement and recovery remain separate acceptance requirements.

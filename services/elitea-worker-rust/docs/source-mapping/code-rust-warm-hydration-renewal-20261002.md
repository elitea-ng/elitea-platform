# Cached native Execute content renewal

This slice hydrates the exact native preparation assets in a fresh cached Execute
runtime. Keep compiled snapshot admission disabled until the assembled runtime
passes its gates. The selected descriptor, PreparedJob bytes, snapshot intent,
activation, descriptor journal, and original runtime remain unchanged.

## Source mapping

| Existing source | New behavior | Purpose |
| --- | --- | --- |
| `client_compiled.rs::submit_compiled_snapshot` | Reconcile first, then renew Execute, selected-root Read, and native Content per index | Recover the original Execute before requesting content or admitting work. |
| `client_compiled.rs::send_execute_snapshot` | Private typed Reconcile, Index, and Dispatch stages | Keep receipt-only authority separate from inert import and dispatch. |
| `sandbox_authority.rs::submit_compiled_snapshot` | Pass the unchanged claim-bound native request and exact optional bundle | Use the existing native Content role without deriving authority from runtime environment values. |
| `graph/code_remote.rs` | Pass the already prepared bundle to the selected Execute branch | Preserve terminal-first journal recovery and the existing trace producer. |
| `service_compiled.rs::submit_snapshot` | Verify independent Execute, Read, and Content roles before indexed admission | Reject Read-only, Compile, Publish, changed descriptor, and changed native content. |
| `docker_compiled.rs::validate_snapshot_execute` | Shared exact Execute/Read/descriptor validation for import and dispatch | Bind both paths to the configured image, policy, snapshot profile, and original job. |
| `docker_compiled_hydration.rs::hydrate_snapshot_execute` | Exact typed Execute entry into the existing bounded native index loop | Keep the same original Reserved runtime inert during content transfer. |
| Existing `hydrate_index_owned` and native finalization | Reused without weakening helper checks | Preserve native descriptor, ordered objects, hashes, profile, and READY semantics. |
| `docker_compiled.rs::submit_snapshot_execute` | Require native readiness before immutable artifact import and dispatch | Never run cached code with missing preparation assets. |
| Existing optional index 9 and readiness 8 in `sandbox.proto` | Comments describe the separate Execute+Read+Content mode | No new wire field or Main role contract. |

## Authority and lifecycle

Every indexed Execute request requires three independently verified roles.
Execute and Read bind the exact selected descriptor and compiled intent.
Read alone cannot reserve, import, finalize, or dispatch.
Content binds the unchanged PreparedJob fingerprint, activation, and exact native
bundle root. Neither Execute nor Read substitutes for Content. Compile and
Publish do not authorize this path.

Receipt-only recovery carries no Read grant, native Content grant, bundle record,
or index. Observe the original Dispatched or terminal Execute before renewing
content. An existing completed receipt remains usable after index expiry. Missing
or Reserved work may request fresh admission under the same exact selected plan.
A corrupt or conflicting selected plan fails; it does not become ordinary
compilation.

Use indices zero through the native file count. An index acknowledgement proves
only that inert transfer. Verify each predecessor's exact hash in the original
runtime before accepting a later index. Replaying an immutable index after a lost
reply or expired grant uses newly issued roles and the same activation, job,
control, descriptor, native root, and runtime journal.

The final index re-verifies the original objects and commits native readiness.
Missing final readiness stops before artifact finalization or dispatch. Native
readiness is checked again on the final dispatch path. Keep current-owner,
request, lease, cancellation, hydration deadline, immutable compiled intent, and
descriptor fences. Renew ownership after import before acknowledgement. No index
marks the job Dispatched or executes its entrypoint.

The three directly awaited Execute request futures are boxed to bound their
storage in the calling async state machine. Each is still awaited sequentially
and is dropped on cancellation. No task or detached transfer is introduced.

## Verification boundary

Focused tests cover exact role renewal, replay after a simulated expiry/fencing
response, cancellation, changed native roots, final readiness, lost dispatch
acknowledgement, and terminal-first receipt recovery. Deterministic Ed25519
fixtures exercise the production role and native scope validators, including
structurally valid wrong-role, changed-root, changed-activation, and expiry cases.
Read-only authority fails the mutating role checks.

In-memory Tonic fixtures and signed pure proofs do not establish actual
PostgreSQL lease takeover, Linux helper execution, mTLS delivery, or Worker/Main/
Supervisor process restart recovery. Those assembled checks belong to root.
The captured private baseline has independent strict Clippy errors; report zero
owned diagnostics separately from the full assembled result.

This preserves preparation assets and the existing runner native verifier. It
does not prove that every native executable is self-contained. Snapshot v1
retains one executable; build-generated companion artifacts outside the retained
preparation tree remain a cache eligibility boundary. Same-image libraries and
hydrated native assets do not establish arbitrary runtime dependency closure.

## Implementation history

- 2026-10-02: Capture the cold hydration and trusted launch postimage, plus the adopted trace and claim-bound authority inputs.
- 2026-10-02: Add the separate typed Execute indexed mode without changing wire fields, canonical PreparedJob bytes, or Main roles.
- 2026-10-02: Preserve original-runtime reconciliation and perform inert native hydration before cached artifact finalization.
- 2026-10-02: Add transport recovery and signed role/root/expiry regression checks; retain default-disabled admission.

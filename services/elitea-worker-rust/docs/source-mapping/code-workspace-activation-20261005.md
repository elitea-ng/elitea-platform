# Code workspace source activation, 2026-10-05

Main controls repository acquisition. This change removes a temporary Worker source gate.
The change does not enable Main policy, add a Worker authority flag, or change graph gates.

| Current source | Changed source | Result |
| --- | --- | --- |
| `code_workspace_remote.rs::CODE_REPOSITORY_WORKSPACE_READY` | Remove the constant | Use Main's configured acquisition owner. |
| `code_attempt_remote.rs::execute_recovery_attempt` | Remove the temporary Admission refusal | Preserve original visit and debug export. Continue through existing preparation and workspace admission. |
| `code_workspace_activation_tests.rs` | Add acquisition fixtures | Require exact Main manifest, original visit, selected base, content digest, root and saved selection. |
| `node_recovery_code_debug_caller_tests.rs` | Add Started attempt fixture | Preserve debug and typed dependency refusal for fresh and recovered attempts. |

Main installs acquisition routes only with its configured capabilities and exact operator policy.
Main rechecks the current claim, saved declaration, toolkit reference and workspace purpose.
Main verifies the stored snapshot before it signs the final intent or Compile grant.
The Worker validates the returned manifest and attaches its exact binding to the prepared job.
The Supervisor requires the signed job and original intent for plain Execute.
Compile requires original-visit access. Cached Execute requires its separate Read grant and exact descriptor.
Hydration keeps the original runtime, cursor, cancellation checks and current fence.

Absent Main ownership or content configuration refuses workspace acquisition.
Omitted workspace preserves existing prepared bytes and requires no workspace owner.
Readwrite remains unsupported. Raw invocation cannot replace original-attempt authority.
Saved-source approval, broker profile selection and debug artifact authorization remain unchanged.

Focused tests use trusted Main transport fixtures and the production acquisition parser.
The Started attempt fixture uses the actual Remote path and journal transitions.
Dependency-mode admission accesses the dispatch journal before workspace acquisition.
Full successful attempt-to-hydration acceptance therefore requires the owning PostgreSQL schema.
No storage bypass is added for tests.

Source readiness is not deployed runtime acceptance.
Verify the enabled Main policy, signed grants, original runtime hydration and cancellation in the deployed candidate.
This change performs no deployment, migration, repository-provider operation or image publication.

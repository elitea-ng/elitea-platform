# Current Index Runtime Identity Parity

Status: **source-only seam; not production-composed**

This note records the current application behavior that must be preserved before
interactive indexing can support users without an active PAT. It intentionally
does not turn a missing actor PAT into a project-system identity at runtime.

## Current behavior, from code

| Current condition | Current action | Evidence |
| --- | --- | --- |
| The actor owns a non-expired PAT | Encode the first active actor PAT and do not send a user-session reference. | `centry/pylon_main/plugins/elitea_core/utils/predict_utils.py:get_user_token`, lines 73-80; `get_predict_token_and_session`, lines 99-115. |
| The actor has no active PAT and the initiating Socket.IO connection is an authenticated `user` context for the same user ID | Read that context's trusted auth-session reference, then encode the project's system-user `api` PAT. Both identities participate: system PAT for the service call and actor session for user/RBAC context. | `predict_utils.py:get_predict_token_and_session`, lines 99-115; payload headers at lines 226-241 and 489-504. |
| The actor has no active PAT and no matching live authenticated user context | Reject with `PredictPayloadError("User token not found. Please create user_token")`. | `predict_utils.py:get_predict_token_and_session`, lines 103-109. |
| The project system user exists but no named `api` PAT exists | Create an `api` PAT on demand and encode it. Project creation normally provisions this user, assigns its project `system` role, and creates a token. | `predict_utils.py:get_system_user_token`, lines 59-70; `projects/utils/project_steps.py:SystemUser` and `SystemToken`, lines 118-158. |
| A project system user is resolved | Resolve the exact `system_user_<project_id>@centry.user` account. | `admin/rpc/roles.py:get_project_system_user`, lines 200-207, and the project constants used by `SystemUser`. |

The no-PAT branch is therefore **delegation**, not identity substitution. A
project-system PAT without the validated actor session does not preserve current
tenant/project/RBAC behavior.

## Why production composition stops here

The current Go path has only these durable runtime identity fields:
`resource_project_id`, `actor_id`, and `initiator`. At private runtime-context
resolution it always asks `LocalIssuer` for the actor's PAT. The v1 response to
the worker contains only `project_id` and `token`.

The current Go admission path does not persist:

- whether admission selected actor PAT or trusted-session delegation;
- a short-lived, workload-bound delegation grant;
- the grant audience, expiry, execution ID, generation, tenant, project, actor,
  or session generation as one immutable authorization record.

The HTTP auth boundary also deliberately treats `X-Auth-Reference` as
compatibility routing material, not an identity claim. Copying the current raw
browser session cookie/reference into PostgreSQL, the command bus, an execution command,
or a worker payload would create a replayable bearer path and is prohibited.

Because these inputs do not exist, runtime code cannot safely infer
`trusted_session_delegation` from `actor PAT not found`. Doing so would change
the principal after admission and would make database/configuration failures
indistinguishable from an authorized delegation.

## Implemented typed seam

`IndexRuntimeIdentityBinding` adds an immutable source contract with:

- schema version;
- execution ID and generation;
- tenant ID, resource project ID, and actor ID;
- exactly one admission-selected mode: `actor_pat` or
  `trusted_session_delegation`;
- only a SHA-256 digest of the server-side delegation grant for the delegation
  mode.

The binding validates against the durable execution identity. It rejects mode
mixing, cross-tenant/project/actor use, execution/generation replay, and a
delegation mode without grant evidence. It has no wire tags and is not mounted
in the command bus or the worker contract.

`ProjectSystemIssuer` and the generated `GetActiveProjectSystemPAT` query add
the exact read/encode adapter needed after delegation is authorized. The query:

- selects only `system_user_<project_id>@centry.user`;
- requires an active, successfully created, non-suspended project;
- requires a non-suspended system user assigned to a role in that project;
- selects only that user's active, non-expired named `api` PAT;
- does not search another project, create a user/PAT, or fall back to an admin
  or actor identity.

The encoded PAT keeps the current Python HS512 claim shape. The result redacts
its default string and Go-string forms.

## Evidence matrix

| Requirement | Automated evidence | Status |
| --- | --- | --- |
| Actor active PAT | Existing `LocalIssuer` unit contract plus `TestPostgresIndexRuntimeCurrentIdentityPATSelection`. | Covered |
| Actor expired/no PAT | Real PostgreSQL test returns `ErrTokenRejected`; it never invokes the separate project-system issuer. | Covered |
| Exact project system user and active `api` PAT | Unit query-row validation and real PostgreSQL lookup/token validation. | Covered, source-only |
| Cross-project and missing project-role assignment | Real PostgreSQL cases fail closed even when another project's active system PAT exists. | Covered |
| Suspended actor, system user, or project | Real PostgreSQL cases fail closed. | Covered |
| Adapter process restart | A newly constructed issuer re-queries durable state and returns the same current identity. | Covered |
| Execution/generation replay and tenant/project/actor drift | `IndexRuntimeIdentityBinding` unit cases reject mismatches. | Covered at source contract |
| Replayed/forged browser session grant | No authoritative durable grant exists in Go yet. No production parity claim is made. | Blocked |
| End-to-end no-PAT UI indexing | Requires the admission/delegation work below. | Blocked |

## Required next slice before mounting

1. At the already authenticated admission boundary, select the mode once:
   actor PAT when active; otherwise require the exact trusted user session for
   the same actor. A database outage must not be interpreted as "no PAT."
2. Replace the raw session reference with a short-lived, single-purpose,
   server-side delegation grant bound to execution, generation, tenant,
   project, actor, capability, audience, session generation, and expiry.
3. Persist the immutable binding and grant digest transactionally with the job.
   Dispatch only the execution reference; never dispatch a PAT, session cookie,
   or raw delegation credential.
4. Resolve the grant only after workload identity, claim, fence, desired-state,
   and execution binding checks. Re-check active user, project, membership,
   permission/RBAC, session generation, audience, and expiry.
5. Materialize the project-system PAT only for the already selected delegation
   mode, keep it in the private data plane, and preserve actor attribution for
   downstream permission checks.
6. Add real PostgreSQL/session-store tests for forged, expired, revoked, and
   replayed grants, cross-process restart, and UI indexing without an actor PAT.

Legacy on-demand creation of a missing project-system PAT is not implemented in
the runtime issuer. Provisioning/reconciliation must own that mutation; runtime
resolution remains a read-only, fail-closed hot path.

Scheduled indexing has a different admission principal and project-system PAT
lifecycle. It is deliberately outside this interactive-user seam and must be
specified and tested separately.

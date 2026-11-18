# Delegation grants: drift, re-approval, notifications and the Marketplace hook (issue 2160)

**What changed.** A delegation grant now notices when its callee holds more than the grant
recorded. The grant is marked `stale`, records what changed (`drift`, new in client contract
1.60), and the calling agent's owners, the people who proposed and approved the grant and the
authors of its recent versions get one notification per edge and digest
(`delegation_grant_stale`; `delegation_closure_grew` when an agent the callee reaches gains a
grant). Approving a stale grant re-approves it: the old grant is kept as `superseded`. The check
runs shortly after a write to an agent, a toolkit, the agent registry, a credential reference
or a Marketplace listing, and when someone reads the calling agent's grants.

Marketplace: a hire approves the hired agent's internal edges, an upgrade that gives an
internal agent more than the pinned version did needs a person's approval at any tier (the plan
shows `delegation_widened` and the edge in its changes) and approving it re-approves the edge,
and the `auto` upgrade policy never applies such a version. The grants of edges whose agent an
upgrade removed are revoked.

Migration: tenant 0253 (`delegation_grants.drift` and `closure`, and the notice table
`delegation_drift_notices`).

**Who is affected.** Every deployment gets the migration. People who edit an agent that other
agents delegate to will see notifications once grants exist. Nothing that runs today changes:
the worker still narrows a stale grant's callee by set containment, and grants are not enforced
until `ELITEA_DELEGATION_GRANTS_ENFORCED` is turned on (default off, with the migration of
existing compositions in a later release).

**What to do.** Upgrade Main (the migrator runs the migration). Nothing else.

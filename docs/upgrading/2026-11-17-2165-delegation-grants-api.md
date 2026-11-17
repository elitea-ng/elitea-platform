# Delegation grants: table, API and governed change request (issue 2165)

**What changed.** Attaching an agent or pipeline to another agent (an agent used as a tool, or a
pipeline child) now records a pending delegation grant for the edge, and a new API
(`listDelegationGrants`, `proposeDelegationGrant`, `getDelegationGrant`, `approveDelegationGrant`,
`revokeDelegationGrant`, `previewDelegationGrant`, client contract 1.58) reads and decides it.
Under the governed profile (`ELITEA_ENTERPRISE_CREDENTIAL_GOVERNANCE_ENABLED`) a grant is approved
through an agent change request of the new kind `delegation`. Migrations: tenant 0252
(`delegation_grants`) and shared 0254 (the change request kind and its one-open-request index).
No new permission, no new setting. The worker does not read the grants yet, so no delegation
behaves differently.

**Who is affected.** Every deployment gets the tables. Deployments on the governed profile see
`delegation` change requests appear on the calling agent when someone attaches an agent that holds
toolkits.

**What to do.** Upgrade Main (the migrator runs both migrations). Nothing else. Enforcement of
grants is a later release and stays off until the migration of existing compositions has run.

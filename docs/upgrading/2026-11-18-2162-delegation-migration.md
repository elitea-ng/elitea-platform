# Delegation: existing compositions are left for review, grants and the principal bound are on (issue 2162, ADR-0044 Amendment 1)

**What changed.** The delegation migration of epic 2156 ships, and it finishes the rollout of the
earlier stories. At the first start of this release the main service walks every project once (the
record is tenant migration 0254, `delegation_migration_runs`) and, for each attached agent or
pipeline whose callee holds more than its caller, writes a **pending** delegation grant flagged
`legacy` (origin `migration`). It never writes an approved grant. It covers every edge: an agent
that attaches an agent as a tool, a pipeline that attaches a pipeline, and a pipeline whose YAML
Agent node runs an attached agent. The same walk runs shortly after a write to an agent, a pipeline
or a toolkit, so an edge created after the upgrade (a pipeline YAML node, an import, a version save)
gets its pending grant too. The calling agent's owners and editors get one notification per calling
agent (`delegation_legacy_pending`), the Tools panel shows a banner with the count, and the admin
console gains **Delegations awaiting review** (permission `configuration.governance`) with the count
per project and a **Scan compositions again** button. The **Delegation** settings show the same count.

Two defaults change in this release:

- `main.env.ELITEA_DELEGATION_GRANTS_ENFORCED` is now `"true"` (it was `"false"`).
- **Bound Delegation By The Run's Principal** (`principal_bound`) is now on. A deployment that never
  saved the **Delegation** section has it on, with the compiled-in defaults (enforce, depth 4). A saved
  `false` is kept.

**What you see on day one.**

- No agent can reach more than it could before. The pending grants approve nothing, and an approved
  grant needs a person who holds access to what it lists (a second person under the governed profile).
- A delegation that needs a grant nobody approved is refused, exactly as before, because a deployment
  with no saved mode enforces and a saved mode is kept. The refusal now shows in the chat with
  **Review delegation**, and the delegation step records it.
- The run principal bound is on: a chain can never reach a toolkit, credential reference or tool the
  person who started the run (or the mandate and project of an unattended run) may not use. A
  composition that did reach one stops, with `DELEGATION_GRANT_MISSING` and the reason `principal`,
  in both modes. This only narrows.
- The number of delegations to review is on **Admin > Delegations awaiting review** and next to the
  mode in **Configuration > Delegation**.

**To keep those calls running while owners review**, set **Mode** to `report` in **Configuration >
Delegation** (and save) for a window of about 14 days, then set it back to `enforce`. In `report` the
delegated agent runs and the missing grant is recorded on the delegation step with
`delegation_enforced: false`. `report` never relaxes the principal bound or the depth limit, and a
governed run always enforces. To be in `report` from the first start of this release, save the mode as
`report` before the upgrade (the saved value is kept; the section itself may be saved with the earlier
release); an installation that saved `report` stays in it until you change it, because the migration
and the upgrade change no saved setting.

**What to do.** Upgrade the main service first and the workers after it (the chart's default order).
A worker from an earlier release rejects a command that carries the principal bound or grants, so a
run that starts on one during the rolling upgrade is refused until it is replaced. To avoid that
window, set `main.env.ELITEA_DELEGATION_GRANTS_ENFORCED` to `"false"` and save the **Delegation**
section with **Bound Delegation By The Run's Principal** off before the upgrade, then turn both back
on after every worker runs this release. Then ask the owners to review (the notification and the
banner lead them to **Review delegation**), watch the count fall on **Delegations awaiting review**,
and return **Mode** to `enforce` if you used `report`. Roll back by setting the switch to `"false"`;
the pending grants stay for the next attempt and change nothing while it is off.

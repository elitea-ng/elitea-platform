# "HR agent" renamed "workforce manager" (API field, proposal source, one stored value)

The agent kind that reviews other agents' scorecards and proposes dismissals
(ADR-0050 decision 5l) is now called a **workforce manager**. The capability that
makes an agent one, `meta.capabilities.can_review_agents`, keeps its name.

- **Who is affected:** every deployment (one shared migration); client builders
  that read the Workforce routes.
- **What to do:** **run the migrations** (shared `0249_agent_workforce_manager_source.sql`)
  and upgrade Main and the web app together. No new setting, no new permission.
- **API:** on `AgentWorkforceAgent` the field `is_hr_agent` is now
  `is_workforce_manager`. Main still returns `is_hr_agent` with the same value,
  marked `deprecated: true`, for **one release** so older mobile and desktop
  builds keep working; it is **removed in the next release**. Move any client
  that reads it to `is_workforce_manager`.
- **API:** on `AgentWorkforceProposal` the `source` value `hr_agent` is now
  `workforce_manager` (`person` and `policy` are unchanged). A client that
  compares `source` to the old string must accept the new one. The old value is
  no longer returned or accepted.
- **Data:** migration `0249` rewrites the rows of
  `elitea_lifecycle.agent_workforce_proposals` whose `source` is `hr_agent` to
  `workforce_manager`, and replaces the two CHECK constraints that name it
  (`agent_workforce_proposals_source_known` and `agent_workforce_proposals_proposer_v2`)
  with the same rules over the new value. It is safe to apply twice. Migrations
  `0236` and `0237` are unchanged.
- **Roll-out order:** Main and the worker use the new value together with the
  migration. A worker or Main older than this change writes `hr_agent`, which the
  new constraint refuses; upgrade them together.
- **Text:** the web label "Reviewer (HR) agent" is now "Workforce manager", and
  the user documentation uses the new name.

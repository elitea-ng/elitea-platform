# Delegation: a pipeline root delegates as an ordinary root, and a refused Agent node shows the notice (FU-335, ADR-0044)

**What changed.** The worker only.

- A pipeline's delegation ceiling is now its own attachments, bounded by the run principal, as an agent's already
  was (ADR-0044 Amendment 1: `own(root) ∩ P`). Before, it was only the toolkits the pipeline's own LLM and toolkit
  nodes select, so a toolkit attached to the pipeline for the saved agent an Agent node or Map member runs was not
  delegated, and the run failed. Those runs now run. Credentials are still materialized only for toolkits the
  pipeline's own nodes use.
- A saved agent run by an Agent node or Map member that its caller may not delegate to (a missing grant, a toolkit
  outside the run principal's bound, or past the maximum depth) now ends the run when it starts, before any model
  call or tool run, with the authorization failure (`AUTHORIZATION_FAILED`, `runtime.authorization_failed`) and the
  same `delegation_refused` notice that a refused pipeline node shows since #2235. Review delegation is offered on
  it once the run names its caller; while every grant of the run is still pending it is not (FU-359, as for an
  agent), and the pending grant is approved from the pipeline's Tools row instead.
  Before, it failed with "The runtime operation failed." (Agent node) or "Configuration type is not supported."
  (Map member). Inside a saved pipeline used as a tool, the refusal is that tool's answer and the turn goes on.
- With delegation grants on (`ELITEA_DELEGATION_GRANTS_ENFORCED`, on by default since #2239), an edge grant adds what the pipeline lacks and
  never takes away what it holds, as for an agent: a partially approved grant narrows only items the pipeline itself
  does not hold.

No migration, setting, frame or client contract change. Main and the web app are unchanged.

**Who is affected.** Projects whose pipelines run saved agents in Agent nodes or Map members. A pipeline that relied
on the failure (none should) now runs. A run that is refused still fails, at assembly, now with a readable notice.

**What to do.** Nothing. Upgrade the worker in the chart's usual order; no deploy order is needed because Main is
untouched. A worker from an earlier release keeps the old behaviour.

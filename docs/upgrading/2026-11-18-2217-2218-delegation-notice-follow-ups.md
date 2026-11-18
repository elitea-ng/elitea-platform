# Delegation: Review delegation opens the edge, and a refused pipeline node shows a notice (issues 2217, 2218)

**What changed.** The web app's **Review delegation** link in a chat notice now carries the called agent
(`/agents/all/<caller>?review_delegation=<callee>`) and the agent's Tools panel opens the review window of
that delegation on arrival. The mobile delegations screen lists that delegation first and marks it. A
pipeline node that runs a saved pipeline and is refused now publishes the same `delegation_refused` run
event (edge kind `pipeline_node`) before the run ends with the authorization failure. No new frame type,
no migration, no client contract change, no setting. `report` mode shows no notice (nothing was refused);
the run step still records the refusal with `delegation_enforced: false`.

**Who is affected.** Projects whose pipelines run saved pipelines as nodes, when delegation grants are
enforced (`ELITEA_DELEGATION_GRANTS_ENFORCED`, off by default).

**What to do.** Upgrade the worker and the web app together (the chart's default order). A worker from an
earlier release shows the generic failure for a refused node, as before. A web app from an earlier release
opens the calling agent without the window.

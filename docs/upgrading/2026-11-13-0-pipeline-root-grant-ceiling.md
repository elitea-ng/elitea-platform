# Agents and pipelines: a pipeline root delegates its attached toolkits, and a refused delegation is readable (FU-335, ADR-0044)

- **Main first.** Roll out Main before the Worker. Main now admits the Worker's new terminal sentence for a delegation
  refused at start, under `AUTHORIZATION_FAILED` (registry `runtime.delegation_grant_missing`, text "This run did not
  start because a saved agent or pipeline it runs needs a toolkit, tool or other grant that its caller does not hold;
  …"). A Worker that meets an older Main is refused that frame once and resends the generic
  `Execution authorization failed.` under the same code, so the run still ends typed; the chat notice that names the
  toolkit is shown either way.
- **What changed: the ceiling.** A pipeline's delegation grants are now its own attached toolkits, after the tool
  policy, the same as an ordinary agent's. Before, a pipeline held only the toolkits that one of its own LLM or toolkit
  nodes used, so a toolkit attached to the pipeline for an Agent node's saved agent (or for a pipeline that agent
  calls) was refused with `DELEGATION_GRANT_MISSING`. Credentials and connections are still made only for the
  toolkits a node, or a delegated saved agent or pipeline, uses. Nothing a saved agent or pipeline declares widens its
  caller: a toolkit attached only further down is still refused at that hop.
- **What changed: the text.** A refused delegation from an Agent node, a Map item, a Parallel branch or a pipeline
  node used to end as "The runtime operation failed." or "Configuration type is not supported.". It now shows a chat
  notice naming the node kind and the toolkit's display name (never a tool, credential, credential reference, data
  class, host or URL), then the readable terminal above. A delegation past the depth limit keeps the generic
  authorization text.
- **Behaviour change: refused at start (C2).** A saved agent that a graph node runs and that its caller may not
  delegate to now ends the run at start, before any model call, even when its node sits on a branch the run might
  never reach. Inside a saved pipeline offered to an agent as a tool, the same refusal stays scoped to that tool: the
  agent's turn goes on, and calling the tool answers the refusal by name.
- **Who is affected.** Pipelines whose Agent nodes, Map items or Parallel branches run saved agents that hold
  toolkits. Pipelines that worked around the old rule with a dummy node on the toolkit keep working; the dummy node
  can be removed.
- **Operator action.** Upgrade Main, then the Worker. Nothing to configure.

# A Map item or Parallel branch whose saved Agent calls a saved pipeline (FU-291)

- **What changed.**
  - **A Map item or Parallel branch may now run a saved Agent that calls a saved pipeline as a tool.** Before this
    change such a member was refused before the run with `graph.fanout.agent_calls_saved_pipeline` ("…runs an Agent
    that calls a saved pipeline, which cannot run there yet…"). That refusal is gone.
    - Each call runs the pipeline on its own durable thread under the member (at most 8 calls per member). After a
      Worker restart the member's Agent replays its stored call with no new model completion, re-enters the pipeline
      where it stopped, and never re-runs a node that finished. A pipeline that had already finished answers its
      stored result. The member lands one answer.
    - The pipeline may hold LLM, State Modifier, Router and direct toolkit/MCP nodes. Its direct nodes journal as a
      saved child's do: a call that never started runs once; one that may have started ends typed and is never
      repeated.
    - A member's 9th pipeline call is answered to its Agent as a tool error naming the limit; nothing runs.
    - If the Agent calls the pipeline again after a typed failure, the new call is a new call: it runs on a new
      thread, and the pipeline's effects may run again. This is today's behaviour for an Agent's tools (FU-291 D6).
  - **Still refused, readably, before the run starts**, with the new code `graph.fanout.agent_tool_pipeline_unsupported`
    (chat text: "A Map item or Parallel branch runs an Agent whose saved pipeline tool holds a Code or Agent node, or
    that reaches a saved pipeline through another Agent, which cannot run there yet; …"):
    - a Code node in the pipeline tool (FU-319);
    - an Agent node in the pipeline tool;
    - a pipeline reached through another Agent (Agent → Agent → pipeline).
    The operator line names the node, the Agent and which of the three it is.
  - **An Agent node whose members make more than 1024 tool calls in all** now ends typed with the registered
    resource-limit text ("The execution exceeded an approved resource limit."); before, it ended as an internal
    error. A nested Agent reports it to its parent model as a tool error. The limit itself is unchanged (FU-330).
  - **Unchanged:** an Agent node of the started pipeline (not inside a Map or Parallel) that calls a pipeline tool
    keeps its in-memory pipeline run. A Worker lost during it ends typed `RECOVERY_FRONTIER_UNSUPPORTED` (#1933); the
    real fix is FU-323. An ordinary Agent's pipeline tool is unchanged.
  - **With node recovery off** (the Helm default `agentNodeRecovery: false`) the member runs as above, but a Worker
    lost during it is not resumed: the run ends typed, as any member does without recovery.
- **Who is affected.**
  - Pipeline authors who use saved Agents with pipeline tools inside a Map or Parallel.
  - Operators who roll out Main and the Workers separately, or roll a Worker back.
- **What to do.**
  - **Roll out Main before the Workers.** Main admits the new refusal text in this release; an older Main receives it
    as the older registered text (`FanoutAgentCallsPipeline`, the same wire code), so the run still ends readably.
  - **Deploy story (C4, both directions).**
    - **Forward.** An execution an older Worker left with `tool_may_have_started` (a pipeline tool call in flight)
      still ends typed `RECOVERY_REFUSED`; it is never replayed.
    - **Rollback.** A new Worker records a member's pipeline call batch under a new phase,
      `durable_delegation_pending`, with the batch's original event id. An older Worker's strict decode refuses that
      record (`agent_recovery.invalid_checkpoint`) before any model call or tool dispatch, so a rolled-back Worker
      resumes nothing and never re-runs the pipeline on an in-memory run. The execution ends typed. No Helm or
      compose guard is needed.
  - Nothing to migrate: no new table. The nested threads are ordinary checkpoint writer rows under the member's
    thread (`…/<agent node>/pt1:<hash>`).
- **Details:** `services/elitea-worker-rust/docs/source-mapping/durable-pipeline-tool-20261111.md`.

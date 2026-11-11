# What broke: error codes

Generated from `libs/go/faults/registry/*.json` by `go generate ./libs/go/faults`. Do not edit by hand.

Every warning or error a person should read is one **fault line**: a plain sentence (`msg`), a stable
`code` from this list, the ids involved, a `support_ref`, and the cause chain (`cause`), made safe by type.
Panics and unexpected errors also carry a bounded stack (Go `stack`, Rust `backtrace`).

## For operators: reading the logs

- **support_ref.** Inside a run it is the `execution_id`; outside a run it is a reference Main generates for the
  request and returns in the `X-Elitea-Support-Ref` header and in every JSON error body. Search the logs for it.
  A client-supplied `X-Request-Id` or `X-Elitea-Support-Ref` is never used as the support reference.
- **Outages are one line each way.** When Main loses a dependency it logs one WARN (for example
  `dependency.database_unavailable`) and, when the dependency is back, one INFO with the outage length. While
  that outage is reported, a background runner's failures caused by the same dependency are logged at **DEBUG**,
  not ERROR, so the outage is not buried under per-cycle lines. Do not look for per-runner ERRORs during a reported
  outage; the WARN is the line.
- **Outside a reported outage,** a runner that keeps failing logs `runner.cycle_failed` at ERROR at most once a
  minute, with `suppressed` counting the failures not logged since the previous line. A slow query that times
  out every cycle therefore shows once a minute with its count, never silently.
- **Worker logs** are JSON lines on stdout, one object per event, without span timing. For performance evidence
  start the Worker with `ELITEA_RUST_SPAN_TIMING=stderr`; the span close lines then go to stderr.

## Codes

### agent_delivery

| Code | What happened | What to do | Who sees it | Retry | Raised by |
|---|---|---|---|---|---|
| `agent_delivery.retained_no_ack` | The Worker left this run's command unacknowledged, so the command bus delivers it again after a delay and the run continues from its last checkpoint. | Nothing while it settles; if the same run repeats this line, read result_code and the lines before it. | operator | yes | elitea-worker-rust |

### agent_output

| Code | What happened | What to do | Who sees it | Retry | Raised by |
|---|---|---|---|---|---|
| `agent_output.invalid_configuration` | The Worker's output delivery settings are invalid, so this run's output could not be prepared. | Check the Worker's output and spool settings, then restart the Worker. | operator | no | elitea-worker-rust |
| `agent_output.invalid_durable_state` | This run's saved output failed the Worker's validation, so it was not sent. | Report the support reference to the platform team; the run's saved output is kept for inspection. | operator | no | elitea-worker-rust |
| `agent_output.resource_exhausted` | The Worker's output spool is full, so this run's output could not be saved. | Check the spool volume's free space and the Worker's spool limits. | operator | no | elitea-worker-rust |
| `agent_output.spool_unavailable` | The Worker could not read or write its output spool, so this run's output waits and is tried again. | Check the spool volume; the Worker retries within the run's lease. | operator | yes | elitea-worker-rust |
| `agent_output.unavailable` | Main's output service did not answer, so this run's output waits and is sent again. | Check Main's runtime listener; the Worker retries within the run's lease. | operator | yes | elitea-worker-rust |

### agent_progress

| Code | What happened | What to do | Who sees it | Retry | Raised by |
|---|---|---|---|---|---|
| `agent_progress.invalid_configuration` | The Worker's progress publication settings are invalid, so this run's progress updates are not sent. | Check the Worker's output session limit, then restart the Worker. | operator | no | elitea-worker-rust |
| `agent_progress.invalid_state` | A progress update did not match this run's state, so it was not sent. | Report the support reference to the platform team. | operator | no | elitea-worker-rust |
| `agent_progress.output_rejected` | Main refused this run's progress update, so the turn will be recovered from its last checkpoint. | Read the cause; if Main keeps refusing updates for new runs, check the Worker and Main versions. | operator | no | elitea-worker-rust |
| `agent_progress.output_unavailable` | Main's output service did not answer a progress update, so the update is kept and sent again. | Check Main's runtime listener; the Worker reconnects on its own. | operator | yes | elitea-worker-rust |

### code

| Code | What happened | What to do | Who sees it | Retry | Raised by |
|---|---|---|---|---|---|
| `code.registration_contended` | A Code visit or intent registration kept conflicting with concurrent registrations of the same run after its bounded retries, so Main answered it as retryable. | Nothing when it is rare; the Worker tries again. If one run repeats it, report the support reference. | operator | yes | elitea-main |
| `code.registration_failed` | Main could not complete a Code visit or intent registration because a dependency failed, so it answered it as retryable. | Read stage and cause; check Main's database if it repeats. The Worker tries the same registration again. | operator | yes | elitea-main |
| `code.registration_refused` | Main refused a Code visit or intent registration: a claim, permission, source, configuration, input or record check failed closed. | Read stage: claim means the Worker's claim, lease or the actor's permission; the others mean the Worker's request did not match the run's frozen pipeline. Report the support reference if the run was healthy. | operator | no | elitea-main |

### dependency

| Code | What happened | What to do | Who sees it | Retry | Raised by |
|---|---|---|---|---|---|
| `dependency.database_unavailable` | Main lost its database; requests that need it fail and Workers get retryable answers until it is back. | Check PostgreSQL and PgBouncer; Main resumes on its own when the database answers again. | operator | yes | elitea-main |
| `dependency.nats_unavailable` | Main lost NATS; admitted runs wait in the outbox and claimed runs continue. | Check the NATS server; Main reconnects on its own. | operator | yes | elitea-main |

### graph

| Code | What happened | What to do | Who sees it | Retry | Raised by |
|---|---|---|---|---|---|
| `graph.fanout.agent_tool_pipeline_unsupported` | A Map item or Parallel branch runs a saved Agent whose saved pipeline tool holds a Code or Agent node, or is reached through another Agent, so the pipeline was refused before it started. | Nothing on the platform; the author removes that node from the pipeline tool, or attaches the pipeline to the Agent directly. | operator | no | elitea-worker-rust |
| `graph.fanout.child_failed` | A Map item or Parallel branch failed; this line names its own code, its fixed sentence and a safe cause chain, so the fan-out failure can be traced to one item. | Find the item by item and item_ref (the support_ref of its failed join entry); member_code and cause say why it failed. | operator | n/a | elitea-worker-rust |
| `graph.fanout.code_below_root` | A Map or Parallel whose worker or branch is a Code node sits inside a saved child pipeline, so the pipeline was refused before it started: fan-out Code runs only in the started pipeline for now. | Nothing on the platform; the author moves the Map or Parallel into the started pipeline, or runs the Code inside a child pipeline without a fan-out. | operator | no | elitea-worker-rust |
| `graph.fanout.deadline_insufficient` | A fan-out node with Code items was refused before it started because its items cannot finish before the run's deadline. | Nothing on the platform; the user lowers the item count or raises the node's concurrency, or the run is given more time. | operator | no | elitea-worker-rust |

### internal

| Code | What happened | What to do | Who sees it | Retry | Raised by |
|---|---|---|---|---|---|
| `internal.panic` | A component crashed on an unexpected condition; the request or task it was handling failed. | Report the support reference to the platform team; the log line holds the bounded stack. | both | n/a | all |
| `internal.unexpected` | An unexpected error stopped this request. | Try again; if it repeats, report the support reference to the platform team. | both | n/a | all |

### pipeline

| Code | What happened | What to do | Who sees it | Retry | Raised by |
|---|---|---|---|---|---|
| `pipeline.code.deadline_insufficient` | A Code attempt was refused before it started because the run's remaining time is shorter than the Code timeout plus the observation margin. | Nothing when the run was near its end; when cause is slot_wait, the Worker was full, so raise code_slots together with the Supervisor's concurrency. | operator | no | elitea-worker-rust |
| `pipeline.code.main_retrying` | Main answered a Code visit or intent call with a retryable failure, so the Worker sends the same idempotent call again until the attempt's deadline. | Nothing when it is rare; main_code names Main's registered cause when Main gave one (for example code.registration_contended), otherwise reason says how the call failed. | operator | yes | elitea-worker-rust |
| `pipeline.code.nested_slot` | A Code attempt that already held a Code slot asked for a second one, so it was refused at once instead of waiting for a slot it holds itself. | Report it with the support reference: a code path acquires a Code slot inside another Code attempt, which must not happen. | operator | no | elitea-worker-rust |
| `pipeline.code.slot_waited` | A Code attempt waited more than a second for a slot because this Worker was already running as many Code attempts on that Supervisor as its code_slots allow. | Nothing when it is rare; if it repeats, raise code_slots together with the Supervisor's concurrency. | operator | n/a | elitea-worker-rust |
| `pipeline.code.supervisor_busy` | The sandbox Supervisor answered busy to a Code attempt, so the attempt keeps its slot and retries with backoff until the Supervisor has room. | Nothing when it is rare; if it repeats, more Worker slots share the Supervisor than its concurrency allows, so lower code_slots or raise the concurrency. | operator | yes | elitea-worker-rust |
| `pipeline.node_recovery.child_effect_unknown` | A Code item of a Map or Parallel was interrupted after it may have started and no owner proof showed its effect, so it ended typed and was not run again. | Find the item by its support_ref in the run's failed join entry and check the target system for its effect; the run's other items are unaffected. | operator | no | elitea-worker-rust |
| `pipeline.recovery.agent_definition_changed` | A checkpoint reclaim found that the saved Agent a root Agent node runs is no longer the definition its stopped step started with, so the run ended typed and the changed Agent did not run. | Nothing to repair on the platform; the saved Agent's version was edited during the run, so the user starts the run again with the current version. | operator | no | elitea-worker-rust |
| `pipeline.recovery.agent_definition_unproven` | A checkpoint reclaim could not prove which saved Agent version a root Agent node's stopped step started with, so the run ended typed and nothing was repeated. | Nothing to repair on the platform; the step was started by an older Worker or Main gave no definition digest, so the version could not be proven and the user starts the run again. | operator | no | elitea-worker-rust |
| `pipeline.recovery.agent_receipt_mismatch` | A checkpoint reclaim found a call receipt for a root Agent node that is not this pipeline's own call of its saved Agent, so the run ended typed and nothing was repeated. | Report it with the support reference: the stored receipt names another occurrence, step or Agent, which only a changed or tampered row produces. | operator | no | elitea-worker-rust |
| `pipeline.recovery.agent_step_foreign_execution` | A checkpoint reclaim found a root Agent node's saved-Agent model checkpoint written by another execution, so the run ended typed and nothing was repeated. | Report it with the support reference: a model scope is derived from its own execution, so another execution's checkpoint there means a changed row. | operator | no | elitea-worker-rust |
| `pipeline.recovery.agent_step_invalid` | A checkpoint reclaim could not validate the model checkpoint or answer receipt of a root Agent node's saved Agent, so the run ended typed and nothing was repeated. | Report it with the support reference: the Agent's stored step does not belong to this execution, generation and definition, or it cannot be decoded. | operator | no | elitea-worker-rust |
| `pipeline.recovery.agent_step_may_have_started` | A checkpoint reclaim found that a root Agent node's saved Agent was inside a tool call that may have started, so the run ended typed and the call was not repeated. | Nothing to repair on the platform; the user checks the target system for that call's effect, then starts the run again. | operator | no | elitea-worker-rust |
| `pipeline.recovery.agent_step_paused` | A checkpoint reclaim found a root Agent node's saved Agent paused for a decision outside a continuation, so the run ended typed and nothing was repeated. | Nothing to repair on the platform; the user answers the card again by starting the run again. | operator | no | elitea-worker-rust |
| `pipeline.recovery.agent_step_session_without_checkpoint` | A checkpoint reclaim found a root Agent node's saved-Agent session with events but no model checkpoint, so the run ended typed and nothing was repeated. | Report it with the support reference: every stored model step writes its checkpoint with it, so its absence means a changed row. | operator | no | elitea-worker-rust |
| `pipeline.recovery.inspect_transient` | A checkpoint reclaim could not read its checkpoint because PostgreSQL was briefly unavailable, so the delivery was kept and is reclaimed again until the run's deadline. | Nothing when it is rare; if it repeats, check PostgreSQL connectivity and lock waits, since suppressed counts the reclaims kept since the last line. | operator | yes | elitea-worker-rust |
| `pipeline.recovery.saved_agent_frontier` | A checkpoint reclaim met a root Agent node running a saved Agent, which cannot resume yet, so the run ended typed and nothing after that node ran. | Nothing to repair on the platform; the user checks what the Agent completed and starts the run again. | operator | no | elitea-worker-rust |
| `pipeline.recovery.tool_pipeline_definition_changed` | A Map item or Parallel branch was interrupted inside a saved pipeline its Agent called, and that pipeline was edited before the run resumed, so the call was not continued and nothing in it ran again. | Nothing to repair on the platform; the user checks what the interrupted call completed and starts the run again on the edited pipeline. | both | no | elitea-worker-rust |
| `pipeline.recovery.tool_pipeline_definition_unproven` | A saved pipeline called by an Agent in a Map item or Parallel branch had no recorded version digest, so it could not be proven unchanged and was not run. | Report the support reference to the platform team; Main did not serve the pipeline's frozen definition digest for this run. | both | no | elitea-worker-rust |
| `pipeline.saved_code.nested` | A Code node sits in a saved pipeline that another saved pipeline runs, so the pipeline was refused before it started: Code runs only in a saved pipeline the started pipeline runs directly, for now. | Nothing on the platform; the author moves the Code into the saved pipeline the started pipeline runs, or runs that saved pipeline directly from the started pipeline. | operator | no | elitea-worker-rust |
| `pipeline.saved_code.unscoped` | A conversation an older Worker paused continues on the unscoped resolver, where a saved pipeline's Code run as a Map worker or Parallel branch has no owner scope, so the pipeline was refused before it started. | Nothing on the platform; the user starts a new conversation, which runs on the scoped resolver. | operator | no | elitea-worker-rust |

### runner

| Code | What happened | What to do | Who sees it | Retry | Raised by |
|---|---|---|---|---|---|
| `runner.cycle_failed` | A background runner failed its pass and will try again. | Read the cause; while a dependency outage is already reported, these lines are at DEBUG. | operator | yes | elitea-main |
| `runner.restarted` | A background runner stopped on a dependency failure and is being restarted. | Nothing, unless it repeats after the dependency is back; then read the cause. | operator | yes | elitea-main |
| `runner.stopped` | A background runner stopped for a reason that does not heal on its own, so Main is exiting. | Read the cause and fix it; compose or Kubernetes restarts Main. | operator | no | elitea-main |

### runtime

| Code | What happened | What to do | Who sees it | Retry | Raised by |
|---|---|---|---|---|---|
| `runtime.application_composition_invalid` | This agent or pipeline cannot run: its attachments form a circular reference, nest deeper than allowed, or include a pipeline that pauses where the pause cannot be continued. Review its attachments and change the one that breaks the rule. | Review the agent's attachments for a circular reference, excess nesting or a pause that cannot continue. | user | no | elitea-worker-rust |
| `runtime.application_composition_too_large` | This agent or pipeline cannot run: its attached agents and pipelines are too large to load together. Remove attachments or shorten their instructions. | Reduce the attachments or their instructions so they load together. | user | no | elitea-worker-rust |
| `runtime.authorization_failed` | Execution authorization failed. | Check the run's credentials and project permissions in Main's lines for this support reference. | user | no | elitea-worker-rust |
| `runtime.cancelled` | Execution was cancelled. | Nothing; the run was cancelled. | user | no | elitea-worker-rust |
| `runtime.cap_reached.browser_monthly_quota` | The run stopped because the project used its monthly cloud browser time. The task may be incomplete. Check what it completed before you continue. | Raise the project's monthly cloud browser quota or wait for it to reset. | user | no | elitea-worker-rust |
| `runtime.cap_reached.max_browser_seconds` | The run stopped because it reached the maximum cloud browser time an administrator set. The task may be incomplete. Check what it completed before you continue. | Raise the run's cloud browser time cap if the task needs more time. | user | no | elitea-worker-rust |
| `runtime.cap_reached.max_model_turns` | The run stopped because it reached the maximum number of model turns an administrator set. The task may be incomplete. Check what it completed before you continue. | Raise the run's model turn cap if the task needs more turns. | user | no | elitea-worker-rust |
| `runtime.cap_reached.max_records_read` | The run stopped because it reached the maximum number of records read an administrator set. The task may be incomplete. Check what it completed before you continue. | Raise the run's records read cap if the task needs more records. | user | no | elitea-worker-rust |
| `runtime.cap_reached.max_tool_calls` | The run stopped because it reached the maximum number of tool calls an administrator set. The task may be incomplete. Check what it completed before you continue. | Raise the run's tool call cap if the task needs more calls. | user | no | elitea-worker-rust |
| `runtime.code_deadline_insufficient` | The Code node could not start: too little time was left before the run's deadline. | Run it again, or give the run more time if this keeps happening. | user | no | elitea-worker-rust |
| `runtime.code_preparation_cancelled` | Code dependency preparation was cancelled. Later nodes did not run. Review the existing attempt before retrying. | Review the existing preparation attempt before a retry. | user | no | elitea-worker-rust |
| `runtime.code_preparation_failed` | Code dependency preparation failed. Later nodes did not run. Share the support reference with your administrator before retrying. | Read the sandbox preparation lines for this support reference before a retry. | user | no | elitea-worker-rust |
| `runtime.code_preparation_unconfirmed` | Code dependency preparation could not be confirmed. The preparation job was not restarted. Later nodes did not run. Ask your administrator to reconcile the existing attempt before retrying. Share the support reference. | Reconcile the existing preparation attempt in the sandbox supervisor before a retry. | user | no | elitea-worker-rust |
| `runtime.context_budget_exceeded` | The model input exceeds the available context budget. Enable compaction, reduce attached content, or select a model with a larger context window. | Nothing on the platform; the user enables compaction, reduces content or selects a larger model. | user | no | elitea-worker-rust |
| `runtime.deadline_exceeded` | The execution deadline was exceeded. | Check what slowed the run in the Worker's lines; the run can be retried. | user | yes | elitea-worker-rust |
| `runtime.dependency_unavailable` | A required runtime dependency is unavailable. | Check the Worker's dependencies (database, NATS, Main); the run can be retried. | user | yes | elitea-worker-rust |
| `runtime.execution_input_field_limit.agent_settings` | The request cannot start because the agent instructions or settings exceed a platform input limit. Reduce the saved content or ask an administrator to inspect the support reference. This is not a model token limit. | Inspect the agent's saved instructions and settings for oversized content. | user | no | elitea-worker-rust |
| `runtime.execution_input_field_limit.chat_history` | The request cannot start because the conversation history exceeds a platform input limit before compaction can run. Start a new chat with the required context. Share the support reference if this repeats. | Nothing on the platform; the user starts a new chat. If it repeats, check the Worker's input limits. | user | no | elitea-worker-rust |
| `runtime.execution_input_field_limit.tool_configuration` | The request cannot start because the attached tool configuration exceeds a platform input limit. Reduce the attached tools or ask an administrator to inspect their schemas using the support reference. | Inspect the attached toolkits' schemas for oversized content. | user | no | elitea-worker-rust |
| `runtime.execution_input_field_limit.user_message` | The request cannot start because the user message exceeds a platform size limit. Shorten the message or attach the content as a file. This is not a model token limit. | Nothing on the platform; the user shortens the message or attaches it as a file. | user | no | elitea-worker-rust |
| `runtime.execution_input_limit` | The request cannot start because its input exceeds a platform size limit. Reduce the message, instructions, or attached context. This is not a model token limit. Share the support reference if the cause is unclear. | Nothing on the platform; the user reduces the input. Check the Worker's lines if the cause is unclear. | user | no | elitea-worker-rust |
| `runtime.fanout_agent_calls_pipeline` | A Map item or Parallel branch runs an Agent that calls a saved pipeline, which cannot run there yet. Use the saved pipeline directly as the Map worker or branch, or remove it from the Agent's tools. | Nothing on the platform; the user runs the saved pipeline directly as the Map worker or branch, or removes it from the Agent's tools. | user | no | elitea-worker-rust |
| `runtime.fanout_agent_tool_pipeline_unsupported` | A Map item or Parallel branch calls a saved pipeline tool it cannot run yet: one with a Code or Agent node, or called via another Agent or a saved pipeline's Agent node; remove that node or call the pipeline from the member's own Agent. | Nothing on the platform; the user removes the Code or Agent node from the pipeline tool, or attaches the pipeline to the Agent directly. | user | no | elitea-worker-rust |
| `runtime.fanout_deadline_insufficient` | The fan-out node could not start: too little time was left before the run's deadline for its items. | Lower the number of items, raise the node's concurrency, or give the run more time. | user | no | elitea-worker-rust |
| `runtime.fanout_member_failed` | The pipeline stopped because a Map item or Parallel branch failed (On Error is Stop, or every item failed). The other items finished first; later nodes did not run. Open Execution details to see the failed item. | Read the Worker's lines for this support reference to find the failed item or branch. | user | no | elitea-worker-rust |
| `runtime.incompatible_version` | The requested contract version is not compatible. | Align the Main and Worker versions; the command's contract version is not one this Worker serves. | user | no | elitea-worker-rust |
| `runtime.internal` | The runtime operation failed. | Read the Worker's lines for this support reference. | user | no | elitea-worker-rust |
| `runtime.invalid_input` | The execution input is invalid. | Read the Worker's lines for this support reference to see which input check refused the run. | user | no | elitea-worker-rust |
| `runtime.model_access_denied` | The model service denied access. Ask an administrator to check the model credentials and project permissions. | Check the model's credentials and the project's permissions for it. | user | no | elitea-worker-rust |
| `runtime.model_budget_exhausted.member` | Your budget in this project is exhausted. Requests are unavailable until the budget resets or an administrator raises your limit. | Raise the member's budget in this project or wait for it to reset. | user | no | elitea-worker-rust |
| `runtime.model_budget_exhausted.project` | The shared model budget of this project is exhausted. Requests are unavailable until the budget resets or an administrator raises the limit. | Raise the project's shared model budget or wait for it to reset. | user | no | elitea-worker-rust |
| `runtime.model_budget_exhausted.unknown` | The model budget is exhausted. Ask an administrator to check the project budget or provider billing before retrying. | Check the project budget and the provider's billing. | user | no | elitea-worker-rust |
| `runtime.model_provider_failure` | The model service reported an error while generating the response. Try again or select another model. An administrator can inspect the execution logs. | Read the gateway's lines for this support reference for the provider's error. | user | yes | elitea-worker-rust |
| `runtime.model_rate_limited` | The model service reached its request limit. Wait briefly and try again, or select another model. | Check the model provider's rate limits for this model. | user | yes | elitea-worker-rust |
| `runtime.model_request_rejected` | The model service rejected this request. Check the selected model and its settings, or ask an administrator to inspect the execution logs. | Read the gateway's lines for this support reference to see why the provider refused the request. | user | no | elitea-worker-rust |
| `runtime.model_request_too_large` | The model request exceeds the transport size limit. Reduce attached content or tool results. A larger token window alone may not resolve this. | Nothing on the platform; the user reduces attached content or tool results. | user | no | elitea-worker-rust |
| `runtime.model_response_invalid` | The model returned an incomplete or invalid response. Try again or select another model. Report repeated failures to an administrator. | Read the gateway's lines for this support reference; report repeated failures for one model. | user | yes | elitea-worker-rust |
| `runtime.model_timeout` | The model did not respond in time. Try again; if this continues, ask an administrator to check the model connection. | Check the model connection and the gateway's lines for this support reference. | user | yes | elitea-worker-rust |
| `runtime.model_unavailable` | The model service could not be reached or is temporarily unavailable. Try again; ask an administrator to check the connection if it persists. | Check the model provider's status and the gateway's connection to it. | user | yes | elitea-worker-rust |
| `runtime.output_continuation_exhausted` | Continuation could not finish. The model response is incomplete. | Read the Worker's lines for this support reference; the model kept stopping before the answer ended. | user | no | elitea-worker-rust |
| `runtime.output_contract_violation` | The answer did not have the format that the agent or pipeline node requires, and one retry did not correct it. Later nodes did not run. Change the instructions or the output contract, or select a different model. | Review the agent's instructions and output contract, or select a different model. | user | no | elitea-worker-rust |
| `runtime.output_projection_limit` | The run stopped because its output exceeded the delivery limit. Some results may be missing. Share the support reference with your administrator before repeating actions. | Read the Worker's lines for this support reference before the user repeats any action. | user | no | elitea-worker-rust |
| `runtime.pipeline_child_input_type_invalid` | The pipeline stopped because a mapped child input has the wrong type. Compare the Agent node's input mapping with the child pipeline's state types. The child and later nodes did not run. Review earlier completed actions before restarting. | Compare the Agent node's input mapping with the child pipeline's state types. | user | no | elitea-worker-rust |
| `runtime.pipeline_code_failed` | The pipeline stopped at a Code node. Check its source, selected input, and sandbox limits. Later nodes did not run. Share the support reference with your administrator before retrying. | Read the Code node's sandbox lines for this support reference. | user | no | elitea-worker-rust |
| `runtime.pipeline_input_invalid` | The pipeline stopped because a tool node has invalid input or a stale approval decision. Check its input mapping and restart only after reviewing completed actions. | Check the tool node's input mapping and any pending approval decision in the pipeline. | user | no | elitea-worker-rust |
| `runtime.pipeline_input_limit` | The pipeline stopped because a tool node input exceeds its size limit. Reduce the mapped input or use smaller batches. This is not a model context limit. | Nothing on the platform; the user reduces the mapped input or uses smaller batches. | user | no | elitea-worker-rust |
| `runtime.pipeline_node_type_not_available` | This pipeline uses a node type that is not available on this deployment. Open the pipeline to see which node, then remove or replace it. | Enable the node type on this deployment or replace the node in the pipeline. | user | no | elitea-worker-rust |
| `runtime.pipeline_result_invalid` | The pipeline stopped because a tool result does not match the node output mapping. Check the required fields and their types. Later nodes did not run. | Check the node's output mapping against the tool's result fields and types. | user | no | elitea-worker-rust |
| `runtime.pipeline_result_limit` | The pipeline stopped because a tool result exceeds the node state size limit. Use smaller batches or reduce the returned data. Later nodes did not run. | Nothing on the platform; the user uses smaller batches or returns less data. | user | no | elitea-worker-rust |
| `runtime.pipeline_tool_failed` | A pipeline tool call failed. Later nodes did not run. Review the support reference and completed actions before retrying; the tool may have started work. | Read the Worker's lines for this support reference and check the tool's target system before a retry. | user | no | elitea-worker-rust |
| `runtime.pipeline_tool_unavailable` | The pipeline stopped because its selected tool cannot run. Check the toolkit binding, permissions, and supported operation. Share the support reference with your administrator. | Check the toolkit binding, its permissions and that the operation is supported. | user | no | elitea-worker-rust |
| `runtime.recovery_frontier_agent_step` | This run stopped during an Agent step that cannot continue after a restart, so nothing after that step ran and nothing was repeated; check what the Agent completed, then start the run again. | Report the support reference to the platform team; an Agent step of a pipeline cannot continue after a Worker restart yet. | user | no | elitea-worker-rust |
| `runtime.recovery_frontier_unsupported` | This run stopped at a point where it cannot be safely continued. Nothing was repeated. Start it again, and share the support reference with your administrator if it happens again. | Report the support reference to the platform team; the run cannot continue from its checkpoint. | user | no | elitea-worker-rust |
| `runtime.recovery_refused` | An approved tool call may have started before the run stopped, and its result was not recorded. It was not repeated. Check the target system before retrying, and share the support reference with your administrator. | Check the target system for the tool call's effect before a retry; it was not repeated. | user | no | elitea-worker-rust |
| `runtime.resource_exhausted` | The execution exceeded an approved resource limit. | Read the Worker's lines for this support reference to see which limit was reached. | user | no | elitea-worker-rust |
| `runtime.result_refused` | The index result was refused by the platform. | Read Main's lines for this support reference to see why the index result was refused. | user | no | elitea-worker-rust |
| `runtime.toolkit_run_authorization_failed` | The platform could not authorize this tool run's credentials or context. | Check the tool run's credentials and context in Main's lines for this support reference. | user | no | elitea-worker-rust |
| `runtime.unsupported_capability` | Configuration type is not supported. | Check that the Worker version supports this configuration type; upgrade the Worker if it is older than Main. | user | no | elitea-worker-rust |

### toolkit

| Code | What happened | What to do | Who sees it | Retry | Raised by |
|---|---|---|---|---|---|
| `toolkit.settings_unresolved` | The toolkit's settings could not be resolved, so the request could not run. | Check the toolkit's credentials and configuration, then reload the page. | both | yes | elitea-main |

### workload

| Code | What happened | What to do | Who sees it | Retry | Raised by |
|---|---|---|---|---|---|
| `workload.session_store_unavailable` | A runtime server cannot read workload sessions, so Workers are answered retry until the database is back. | Check PostgreSQL; Workers retry within their lease. | operator | yes | elitea-main |

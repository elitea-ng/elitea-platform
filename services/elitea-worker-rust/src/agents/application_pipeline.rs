//! A saved PIPELINE attached to an ordinary agent, admitted as a callable tool.
//!
//! `application_tools` compiles an `agent` child into a nested `LlmAgent` and
//! resumes its HITL pause by REPLAY: the child has no state a checkpoint could
//! hold, so re-running it with the recorded history and the recorded decision
//! reproduces it exactly. A stored pipeline is a graph, and replay is not a
//! resume for a graph — its pending node, its frontier and its channel state
//! are the run.
//!
//! The pipeline PARENT already solves this for an `agent` node: the child
//! compiles as an ADK `SubgraphNode` over the parent graph's own checkpointer,
//! and the pause carries the child's thread and checkpoint id up through the
//! parent's interrupt wrapper. An ORDINARY parent has none of that — no graph,
//! no checkpointer, no graph thread — so this module supplies the two things
//! that are actually missing and reuses everything else:
//!
//! 1. the child runs on an INVOCATION-LOCAL [`MemoryCheckpointer`], and the
//!    checkpoint its pause produced travels, serialized, on the descendant
//!    interrupt event that the parent's claim-fenced `SessionService` already
//!    persists. That is the same durable, worker-private store every other
//!    resume input is read from, so nothing new is trusted — and it avoids
//!    binding a `PostgresCheckpointer` (one thread per activation) to a thread
//!    it was not activated for;
//! 2. the resume re-seeds a fresh `MemoryCheckpointer` from that checkpoint and
//!    compiles the child with the decision on its HITL resume channel, so ADK's
//!    executor re-enters at the pending node.
//!
//! Everything above the tool is the machinery that already existed: the pause
//! rides the parent's descendant event channel, `ApplicationCallBatch` turns
//! the tool's nested-interrupt result into a paused parent turn, the browser
//! card is projected by the nested projector's existing `project_pipeline_hitl`,
//! and the parent's `ApplicationReplayModel` re-emits the identical tool call —
//! which is why the child's checkpoint thread can be derived from the
//! function-call id at all.

#![allow(dead_code)] // Registration follows the ordinary assembler's gate.

use std::collections::{BTreeSet, HashSet};
use std::sync::Arc;

use adk_rust::futures::StreamExt as _;
use adk_rust::graph::interrupt::{GraphInterruptPayload, INTERRUPT_METADATA_KEY};
use adk_rust::graph::{Checkpoint, Checkpointer, MemoryCheckpointer, State};
use adk_rust::{Agent, Content, Event, Part, ReadonlyContext as _, Tool, ToolContext};
use async_trait::async_trait;
use serde::{Deserialize, Serialize};
use serde_json::{Value, json};
use tracing::Instrument as _;

use super::application_tools::{
    ApplicationEventSender, ApplicationEventSignal, ApplicationToolInvocationContext,
    MAX_APPLICATION_TASK_BYTES, application_event_channel_error, application_task,
    child_execution_error, nested_interrupt_result, tool_input_error,
};
use super::application_tools::{ApplicationResumeCoordinator, PIPELINE_APPLICATION_AGENT_TYPE};
use super::events::{
    DESCENDANT_CHECKPOINT_THREAD_KEY, DESCENDANT_CONTAINER_INVOCATION_KEY,
    DESCENDANT_PARENT_CALL_KEY, PIPELINE_TOOL_PENDING_METADATA_KEY, pipeline_hitl_event_binding,
    strip_descendant_private_metadata,
};
use super::graph::compiler::{PipelineDefinition, PipelineNodeRuntimes};
use super::graph::resume::{PipelineResume, pipeline_hitl_resume_state};
use super::graph::{EliteaGraphAgent, PIPELINE_COMPLETED_METADATA_KEY, PipelineNodeEventReceiver};
use super::runtime::{NativeAgentAssemblyError, NativeAgentAssemblyErrorCode};

/// The pending-state envelope's own revision, checked on the way back in.
const PIPELINE_TOOL_PENDING_SCHEMA: &str = "elitea.pipeline-tool-pending.v1";

/// Upper bound on one child pipeline's serialized pending checkpoint.
///
/// It is a REFUSAL bound, not a truncation bound: a pause that would exceed it
/// is not shown, because a card the resume could never re-enter is worse than
/// an honest failure — see [`ApplicationPipelineTool::pause`]. 256 KiB is one
/// order of magnitude above the largest state a stored pipeline can legitimately
/// carry into a `hitl` node under the graph's own limits (a rendered HITL
/// message is capped at 8 KiB, an edit value at 64 KiB, an Application node's
/// mapped value at 240 KiB and its projected result at 512 KiB — and a node
/// that produced a 512 KiB result cannot then pause on it without exceeding
/// this), while staying well inside what one durable session event row holds.
pub(crate) const MAX_PIPELINE_TOOL_PENDING_BYTES: usize = 256 * 1_024;

/// Upper bound on the description a child pipeline contributes.
const MAX_PIPELINE_DESCRIPTION_BYTES: usize = 4 * 1_024;

/// The serialized form of one child pipeline's pause.
///
/// `thread_id`, `checkpoint_id` and `node_name` are duplicated out of the
/// checkpoint deliberately: they are what the resume checks the decoded
/// checkpoint AGAINST, so a truncated or edited blob whose inner checkpoint no
/// longer agrees with its own envelope is refused rather than re-entered.
#[derive(Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
struct PipelineToolPending {
    schema_revision: String,
    thread_id: String,
    checkpoint_id: String,
    node_name: String,
    checkpoint: Checkpoint,
}

/// One checkpoint-proven continuation of a paused child pipeline.
pub(crate) struct PipelineToolResume {
    interrupt_id: String,
    thread_id: String,
    checkpoint: Checkpoint,
    resume_state: State,
}

impl PipelineToolResume {
    #[must_use]
    pub(crate) fn interrupt_id(&self) -> &str {
        &self.interrupt_id
    }

    #[must_use]
    pub(crate) fn thread_id(&self) -> &str {
        &self.thread_id
    }
}

/// What one persisted child-pipeline pause says about itself.
pub(crate) struct PipelinePauseIdentity {
    pub(crate) interrupt_id: String,
    pub(crate) tool_name: String,
    pub(crate) container_invocation_id: String,
    pub(crate) parent_call_id: String,
    pub(crate) checkpoint_thread_id: String,
    pub(crate) node_name: String,
    pub(crate) definition_digest: String,
    available_actions: Vec<String>,
    pending: PipelineToolPending,
}

impl PipelinePauseIdentity {
    #[must_use]
    pub(crate) fn allows(&self, graph_action: &str) -> bool {
        self.available_actions
            .iter()
            .any(|candidate| candidate == graph_action)
    }

    /// Bind one browser decision to this pause's own checkpoint.
    ///
    /// The envelope's identity is checked against the decoded checkpoint here
    /// rather than at decode time, because this is the last point before the
    /// child graph would be re-entered: a blob whose checkpoint names another
    /// thread, another checkpoint id, or a different pending node is a blob
    /// that cannot resume THIS pause.
    pub(crate) fn into_resume(
        self,
        graph_action: &str,
        value: &str,
    ) -> Result<PipelineToolResume, PipelinePauseError> {
        if !self.allows(graph_action) {
            return Err(PipelinePauseError::Stale);
        }
        let checkpoint = self.pending.checkpoint;
        if checkpoint.thread_id != self.pending.thread_id
            || checkpoint.checkpoint_id != self.pending.checkpoint_id
            || checkpoint.thread_id != self.checkpoint_thread_id
            || checkpoint.pending_nodes.as_slice() != [self.pending.node_name.clone()]
            || self.pending.node_name != self.node_name
        {
            return Err(PipelinePauseError::Corrupt);
        }
        Ok(PipelineToolResume {
            interrupt_id: self.interrupt_id,
            thread_id: self.checkpoint_thread_id,
            checkpoint,
            resume_state: pipeline_hitl_resume_state(
                &self.node_name,
                &self.definition_digest,
                graph_action,
                value,
            ),
        })
    }
}

/// Why one persisted pause could not be bound.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum PipelinePauseError {
    /// The decision does not name an action this card offered.
    Stale,
    /// The persisted pending state does not describe this pause.
    Corrupt,
}

/// Read the pause a persisted descendant event carries, if it carries one.
///
/// `Ok(None)` means "not a child-pipeline pause" and is the common answer:
/// every other event in a session reaches this. Only an event this module
/// itself wrote carries [`PIPELINE_TOOL_PENDING_METADATA_KEY`], and the
/// interrupt identity is RECOMPUTED from the event by the same binding the
/// browser projection used, never read from the blob.
pub(crate) fn pipeline_pause_identity(
    event: &Event,
) -> Result<Option<PipelinePauseIdentity>, PipelinePauseError> {
    let Some(raw) = event
        .provider_metadata
        .get(PIPELINE_TOOL_PENDING_METADATA_KEY)
    else {
        return Ok(None);
    };
    if raw.len() > MAX_PIPELINE_TOOL_PENDING_BYTES {
        return Err(PipelinePauseError::Corrupt);
    }
    let container_invocation_id = event
        .provider_metadata
        .get(DESCENDANT_CONTAINER_INVOCATION_KEY)
        .ok_or(PipelinePauseError::Corrupt)?
        .clone();
    let parent_call_id = event
        .provider_metadata
        .get(DESCENDANT_PARENT_CALL_KEY)
        .ok_or(PipelinePauseError::Corrupt)?
        .clone();
    let checkpoint_thread_id = event
        .provider_metadata
        .get(DESCENDANT_CHECKPOINT_THREAD_KEY)
        .ok_or(PipelinePauseError::Corrupt)?
        .clone();
    let pending = serde_json::from_str::<PipelineToolPending>(raw)
        .map_err(|_| PipelinePauseError::Corrupt)?;
    if pending.schema_revision != PIPELINE_TOOL_PENDING_SCHEMA
        || pending.thread_id != checkpoint_thread_id
    {
        return Err(PipelinePauseError::Corrupt);
    }
    // The card's own identity, recomputed by the projection's parser off a
    // COPY without the private keys — the same event the browser saw.
    let mut projected = event.clone();
    strip_descendant_private_metadata(&mut projected);
    // The projector clears the child's tier before the descendant projector
    // sees a graph interrupt; the binding below applies the same rule, so the
    // event this reads is byte-for-byte the one the browser was shown.
    projected.branch.clear();
    let binding = pipeline_hitl_event_binding(&projected, &event.author, &checkpoint_thread_id)
        .map_err(|_| PipelinePauseError::Corrupt)?;
    Ok(Some(PipelinePauseIdentity {
        interrupt_id: binding.interrupt_id().to_owned(),
        tool_name: event.author.clone(),
        container_invocation_id,
        parent_call_id,
        checkpoint_thread_id,
        node_name: binding.node_name().to_owned(),
        definition_digest: binding.definition_digest().to_owned(),
        available_actions: ["approve", "reject", "edit"]
            .into_iter()
            .filter(|action| binding.allows(action))
            .map(ToOwned::to_owned)
            .collect(),
        pending,
    }))
}

/// Who the child is, for the one call being drained.
///
/// Three strings that always travel together and are each derived from the
/// parent's call: passing them as one value keeps `drain_child`'s owner count
/// honest and stops a caller supplying a branch from one call beside an
/// invocation id from another.
struct ChildIdentity<'call> {
    invocation_id: &'call str,
    branch: &'call str,
    thread_id: &'call str,
}

/// One saved pipeline exposed to the parent model as a `task` tool.
pub(super) struct ApplicationPipelineTool {
    name: String,
    description: String,
    definition: Arc<PipelineDefinition>,
    runtimes: PipelineNodeRuntimes,
    node_events: PipelineNodeEventReceiver,
    conversation_thread_id: String,
    event_sender: Option<ApplicationEventSender>,
    resume: Option<ApplicationResumeCoordinator>,
}

/// What one pipeline child needs from the parent that calls it.
pub(super) struct PipelineToolParentBinding {
    /// The durable conversation thread the child's own checkpoint thread is
    /// namespaced under.
    pub(super) conversation_thread_id: String,
    pub(super) event_sender: Option<ApplicationEventSender>,
    pub(super) resume: Option<ApplicationResumeCoordinator>,
}

impl ApplicationPipelineTool {
    pub(super) fn new(
        name: String,
        description: String,
        definition: PipelineDefinition,
        runtimes: PipelineNodeRuntimes,
        node_events: PipelineNodeEventReceiver,
        parent: PipelineToolParentBinding,
    ) -> Self {
        Self {
            name,
            description,
            definition: Arc::new(definition),
            runtimes,
            node_events,
            conversation_thread_id: parent.conversation_thread_id,
            event_sender: parent.event_sender,
            resume: parent.resume,
        }
    }
}

/// The description one saved pipeline child publishes to the parent model.
#[must_use]
pub(crate) fn pipeline_tool_description(alias: &str, stored: Option<&str>) -> String {
    const SEPARATOR: &str = " Purpose: ";

    let mut description = format!(
        "Run the saved Elitea pipeline '{alias}' on one self-contained task. The pipeline executes its own stored graph and returns its result; include everything it needs in task."
    );
    if let Some(stored) = stored.filter(|value| !value.is_empty()) {
        let remaining =
            MAX_PIPELINE_DESCRIPTION_BYTES.saturating_sub(description.len() + SEPARATOR.len());
        if remaining == 0 {
            return description;
        }
        description.push_str(SEPARATOR);
        if stored.len() <= remaining {
            description.push_str(stored);
            return description;
        }
        // Cut on a character boundary, never inside a code point.
        let boundary = stored
            .char_indices()
            .map(|(index, _)| index)
            .take_while(|index| *index <= remaining)
            .last()
            .unwrap_or(0);
        description.push_str(&stored[..boundary]);
    }
    description
}

#[async_trait]
impl Tool for ApplicationPipelineTool {
    fn name(&self) -> &str {
        &self.name
    }

    fn description(&self) -> &str {
        &self.description
    }

    fn parameters_schema(&self) -> Option<Value> {
        Some(json!({
            "type": "object",
            "properties": {
                "task": {
                    "type": "string",
                    "minLength": 1,
                    "maxLength": MAX_APPLICATION_TASK_BYTES,
                    "description": "Required self-contained task for this saved pipeline. It becomes the pipeline's input; maximum 240 KiB UTF-8."
                }
            },
            "required": ["task"],
            "additionalProperties": false
        }))
    }

    fn response_schema(&self) -> Option<Value> {
        Some(json!({
            "type": "object",
            "oneOf": [
                {
                    "properties": {"response": {"type": "string"}},
                    "required": ["response"],
                    "additionalProperties": false
                },
                {
                    "properties": {"error": {"type": "string"}, "failure": {"type": "object"}},
                    "required": ["error", "failure"],
                    "additionalProperties": false
                }
            ]
        }))
    }

    async fn execute(
        &self,
        ctx: Arc<dyn ToolContext>,
        arguments: Value,
    ) -> adk_rust::Result<Value> {
        let span = tracing::info_span!(
            "agent.nested_pipeline.invoke",
            tool_name = %self.name,
            invocation_id = %ctx.invocation_id(),
            function_call_id = %ctx.function_call_id(),
            resumed = tracing::field::Empty,
            outcome = tracing::field::Empty,
        );
        let result = self
            .invoke_child(ctx.clone(), arguments)
            .instrument(span.clone())
            .await;
        span.record(
            "outcome",
            match &result {
                Ok(_) => "succeeded",
                Err(_) => "failed",
            },
        );
        match result {
            Err(error) => {
                if let Some(report) =
                    super::application_tools::child_continuation_report(&error, None, false)
                {
                    tracing::error!(
                        event = "nested_pipeline_failed",
                        invocation_id = %ctx.invocation_id(),
                        function_call_id = %ctx.function_call_id(),
                        error_code = error.code,
                        cause_message = report["failure"]["message"].as_str(),
                        recovery = "revise_task",
                        "child pipeline answer is incomplete; returning failure to the orchestrator"
                    );
                    Ok(report)
                } else {
                    Err(error)
                }
            }
            Ok(value) => Ok(value),
        }
    }
}

impl ApplicationPipelineTool {
    /// The child graph's checkpoint thread for one exact parent tool call.
    #[must_use]
    fn checkpoint_thread_id(&self, call_id: &str) -> String {
        format!("{}/{call_id}", self.conversation_thread_id)
    }

    async fn invoke_child(
        &self,
        ctx: Arc<dyn ToolContext>,
        arguments: Value,
    ) -> adk_rust::Result<Value> {
        let task = application_task(&arguments)
            .map_err(|_| tool_input_error())?
            .to_owned();
        let thread_id = self.checkpoint_thread_id(ctx.function_call_id());
        let resume = match &self.resume {
            Some(coordinator) => coordinator
                .take(ctx.invocation_id(), ctx.function_call_id())
                .await?
                .map(|resume| resume.into_pipeline(&self.name, &arguments))
                .transpose()?,
            None => None,
        };
        tracing::Span::current().record("resumed", resume.is_some());
        let checkpointer: Arc<dyn Checkpointer> = Arc::new(MemoryCheckpointer::new());
        let pipeline_resume = match resume {
            Some(resume) => {
                if resume.thread_id != thread_id {
                    return Err(tool_input_error());
                }
                checkpointer
                    .save(&resume.checkpoint)
                    .await
                    .map_err(|_| child_execution_error())?;
                Some(PipelineResume::from_state(resume.resume_state))
            }
            None => None,
        };
        let graph = self
            .definition
            .compile_with_runtime(
                &self.name,
                Arc::clone(&checkpointer),
                pipeline_resume,
                &self.runtimes,
            )
            .map_err(|_| child_execution_error())?;
        let agent: Arc<dyn Agent> = Arc::new(EliteaGraphAgent::new(graph));
        let invocation_id = format!("pipeline-child:{}", ctx.function_call_id());
        let child_context = Arc::new(ApplicationToolInvocationContext::for_pipeline(
            Arc::clone(&ctx),
            Arc::clone(&agent),
            Content::new("user").with_text(&task),
            invocation_id.clone(),
            thread_id.clone(),
        ));
        let branch = child_context.branch().to_owned();
        self.drain_child(
            ctx.as_ref(),
            agent,
            child_context,
            ChildIdentity {
                invocation_id: &invocation_id,
                branch: &branch,
                thread_id: &thread_id,
            },
            checkpointer.as_ref(),
        )
        .await
    }

    async fn drain_child(
        &self,
        ctx: &dyn ToolContext,
        agent: Arc<dyn Agent>,
        child_context: Arc<ApplicationToolInvocationContext>,
        child: ChildIdentity<'_>,
        checkpointer: &dyn Checkpointer,
    ) -> adk_rust::Result<Value> {
        let ChildIdentity {
            invocation_id,
            branch,
            thread_id,
        } = child;
        let mut node_events = self
            .node_events
            .drain(invocation_id, &self.name, branch)
            .await?;
        // #990 review 5: anything still queued belongs to an EARLIER call of
        // this same tool that returned on a pause. Forwarding it now would
        // stamp it with THIS call's identity, so it is dropped with a warning
        // rather than misattributed. The pause path below drains what it owns
        // before it returns, so this is a guard and not the normal path.
        let mut stale = 0_usize;
        while node_events.try_recv().is_some() {
            stale += 1;
        }
        if stale > 0 {
            tracing::warn!(
                tool_name = %self.name,
                dropped = stale,
                "discarded node events left over from an earlier call of this pipeline tool"
            );
        }
        let mut stream = agent.run(child_context).await?;
        let mut node_events_open = true;
        let mut result_text = None;
        loop {
            let mut event = tokio::select! {
                biased;
                signal = node_events.recv(), if node_events_open => {
                    if let Some(event) = signal {
                        self.forward(ctx, thread_id, event?).await?;
                    } else {
                        node_events_open = false;
                    }
                    continue;
                }
                next = stream.next() => {
                    // The graph may wrap an error after queuing its typed cause.
                    // Drain that cause before returning the generic graph error.
                    while let Some(queued) = node_events.try_recv() {
                        self.forward(ctx, thread_id, queued?).await?;
                    }
                    let Some(next) = next else { break };
                    next?
                }
            };
            // The child's own tier, NOT empty: an empty branch is visible to
            // every branch, so the parent would re-read the child's events as
            // its own on the next turn (#990 review 1).
            branch.clone_into(&mut event.branch);
            if event.provider_metadata.contains_key(INTERRUPT_METADATA_KEY) {
                // #990 review 5: the node events this child already emitted
                // belong to THIS call and are its visible progress; draining
                // them before the pause returns is what keeps them from being
                // dropped or replayed under a later call's identity.
                while let Some(queued) = node_events.try_recv() {
                    self.forward(ctx, thread_id, queued?).await?;
                }
                return self.pause(ctx, thread_id, event, checkpointer).await;
            }
            if event
                .provider_metadata
                .contains_key(PIPELINE_COMPLETED_METADATA_KEY)
            {
                // Suppressed, not forwarded: the pipeline's terminal marker is
                // this TOOL's return value, and re-emitting it as a descendant
                // model answer would put the child's text in the transcript
                // twice — once as the tool's own result and once as a message
                // the parent never wrote.
                result_text = event_text(&event);
                continue;
            }
            self.forward(ctx, thread_id, event).await?;
        }
        while let Some(event) = node_events.try_recv() {
            self.forward(ctx, thread_id, event?).await?;
        }
        Ok(json!({
            "response": result_text.unwrap_or_else(|| "No response from pipeline".to_owned())
        }))
    }

    /// Surface one child HITL pause, or refuse it.
    ///
    /// The refusal arm is the whole reason the bound is checked HERE, before
    /// anything is forwarded: a card whose pending state could not be carried
    /// is a card nothing could ever answer, so the child's call ends as a tool
    /// ERROR the parent model reads and reports instead of a prompt that
    /// strands the conversation.
    async fn pause(
        &self,
        ctx: &dyn ToolContext,
        thread_id: &str,
        mut event: Event,
        checkpointer: &dyn Checkpointer,
    ) -> adk_rust::Result<Value> {
        let payload =
            GraphInterruptPayload::from_event(&event).ok_or_else(child_execution_error)?;
        if payload.thread_id != thread_id {
            return Err(child_execution_error());
        }
        let checkpoint = checkpointer
            .load_by_id(&payload.checkpoint_id)
            .await
            .map_err(|_| child_execution_error())?
            .ok_or_else(child_execution_error)?;
        if checkpoint.thread_id != thread_id
            || checkpoint.checkpoint_id != payload.checkpoint_id
            || checkpoint.pending_nodes.len() != 1
        {
            return Err(child_execution_error());
        }
        let node_name = checkpoint.pending_nodes[0].clone();
        // #990 review 3: the ONLY pause kind this parent resumes is the child's
        // own `hitl` node. Admission refuses a child that could raise any
        // other kind (sensitive-tool approval, a clarifying question, an MCP
        // authorization challenge), so reaching this arm with one means the
        // admission and the runtime disagree — fail closed and say which kind
        // arrived rather than show an unanswerable card.
        // Bound against the event AS THE BROWSER WILL SEE IT: the projector
        // clears the child's branch before the descendant projector reads a
        // graph interrupt, and `validate_graph_interrupt_event` admits only an
        // empty or root branch — so the identity is computed on that same
        // shape while the PERSISTED event keeps the branch that hides it from
        // the parent's next turn (#990 review 1).
        let mut projected = event.clone();
        projected.branch.clear();
        let binding =
            pipeline_hitl_event_binding(&projected, &self.name, thread_id).map_err(|_| {
                tracing::error!(
                    tool_name = %self.name,
                    "a pipeline child raised a pause this parent cannot resume"
                );
                child_execution_error()
            })?;
        let pending = PipelineToolPending {
            schema_revision: PIPELINE_TOOL_PENDING_SCHEMA.to_owned(),
            thread_id: thread_id.to_owned(),
            checkpoint_id: payload.checkpoint_id.clone(),
            node_name,
            checkpoint,
        };
        let encoded = serde_json::to_string(&pending).map_err(|_| child_execution_error())?;
        if encoded.len() > MAX_PIPELINE_TOOL_PENDING_BYTES {
            tracing::warn!(
                tool_name = %self.name,
                pending_bytes = encoded.len(),
                limit = MAX_PIPELINE_TOOL_PENDING_BYTES,
                "a pipeline child's pause carried more state than one resume can carry"
            );
            return Ok(oversized_pause_result(
                &self.name,
                encoded.len(),
                MAX_PIPELINE_TOOL_PENDING_BYTES,
            ));
        }
        if self.event_sender.is_none() {
            // Without the descendant channel the card has nowhere to go, so the
            // pause would be invisible AND unanswerable.
            return Err(application_event_channel_error());
        }
        if event
            .provider_metadata
            .insert(PIPELINE_TOOL_PENDING_METADATA_KEY.to_owned(), encoded)
            .is_some()
        {
            return Err(child_execution_error());
        }
        let interrupt_id = binding.interrupt_id().to_owned();
        self.forward(ctx, thread_id, event).await?;
        Ok(nested_interrupt_result(&BTreeSet::from([interrupt_id])))
    }

    async fn forward(
        &self,
        ctx: &dyn ToolContext,
        thread_id: &str,
        mut event: Event,
    ) -> adk_rust::Result<()> {
        let Some(sender) = &self.event_sender else {
            return Ok(());
        };
        event.llm_request = None;
        sender
            .send(ApplicationEventSignal::Event {
                container_invocation_id: ctx.invocation_id().to_owned(),
                parent_call_id: ctx.function_call_id().to_owned(),
                checkpoint_thread_id: Some(thread_id.to_owned()),
                event: Box::new(event),
            })
            .await
            .map_err(|_| application_event_channel_error())
    }
}

/// The tool result a refused pause returns.
///
/// It is a RESULT rather than an error so the parent turn continues and the
/// model can say what happened, naming the child and the limit, instead of the
/// whole turn dying anonymously on a child the user never mentioned.
#[must_use]
fn oversized_pause_result(tool_name: &str, actual: usize, limit: usize) -> Value {
    json!({
        "response": format!(
            "The pipeline '{tool_name}' paused for a human decision, but its pending state is {actual} bytes and this runtime can carry at most {limit} bytes across a pause. The pipeline was stopped and nothing was decided; reduce what it accumulates before its hitl node, or run it directly.",
        )
    })
}

fn event_text(event: &Event) -> Option<String> {
    let content = event.content()?;
    let mut text = String::new();
    for part in &content.parts {
        if let Part::Text { text: value } = part {
            text.push_str(value);
        }
    }
    (!text.is_empty()).then_some(text)
}

/// One resolved pipeline child ready to be presented beside the agent children.
pub(crate) struct MaterializedPipelineChild {
    pub(crate) alias: String,
    pub(crate) identity: (u64, u64),
    pub(crate) tool: Arc<dyn Tool>,
}

pub(crate) const fn unsupported_pipeline_child() -> NativeAgentAssemblyError {
    NativeAgentAssemblyError::new(
        NativeAgentAssemblyErrorCode::UnsupportedCapability,
        "the attached pipeline child could not be compiled for this parent",
    )
}

/// The aliases and identities of the pipeline children one snapshot attaches.
#[must_use]
pub(crate) fn pipeline_child_references(
    snapshot: &crate::toolkits::AdmittedToolSnapshot<'_>,
    selected_aliases: Option<&BTreeSet<String>>,
) -> Vec<PipelineChildReference> {
    let mut seen = HashSet::new();
    let mut references = Vec::new();
    for reference in snapshot
        .iter()
        .filter(|reference| reference.kind() == crate::toolkits::FrozenToolKind::Application)
        .filter(|reference| {
            reference.application_agent_type() == Some(PIPELINE_APPLICATION_AGENT_TYPE)
        })
        .filter(|reference| {
            selected_aliases.is_none_or(|aliases| aliases.contains(reference.toolkit_name()))
        })
    {
        let Some(identity) = reference.application_identity() else {
            continue;
        };
        if !seen.insert(identity) {
            continue;
        }
        references.push(PipelineChildReference {
            identity,
            alias: reference.toolkit_name().to_owned(),
            description: reference.application_description().map(str::to_owned),
            project_id: reference.application_project_id(),
        });
    }
    references
}

/// One attached pipeline child, before its version is resolved.
pub(crate) struct PipelineChildReference {
    pub(crate) identity: (u64, u64),
    pub(crate) alias: String,
    pub(crate) description: Option<String>,
    pub(crate) project_id: Option<u64>,
}

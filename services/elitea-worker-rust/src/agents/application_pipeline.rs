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
pub(crate) mod boundary;
mod boundary_ledger;
pub(crate) use boundary_ledger::{
    BOUNDARY_LEDGER_KEY, append_outer_boundary, project_scope_boundary, projected_outer_boundaries,
    validate_scoped_outer_boundary,
};
mod static_pause;
pub(crate) use boundary::{
    pipeline_boundary_original_call, rebind_pipeline_tool_boundary,
    retained_pipeline_application_events, validate_retained_pipeline_application_pause,
};
#[cfg(test)]
pub(crate) use static_pause::static_pause_fixture;
pub(crate) use static_pause::{
    PipelineStaticToolPause, PipelineStaticToolResume, static_pipeline_tool_pause,
};
use std::collections::BTreeMap;
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
    DESCENDANT_PARENT_CALL_KEY, PIPELINE_TOOL_BOUNDARY_METADATA_KEY,
    PIPELINE_TOOL_PENDING_METADATA_KEY, pipeline_hitl_event_binding,
    strip_descendant_private_metadata,
};
use super::graph::compiler::{PipelineDefinition, PipelineNodeRuntimes};
use super::graph::resume::{PipelineResume, pipeline_hitl_resume_state};
use super::graph::{EliteaGraphAgent, PIPELINE_COMPLETED_METADATA_KEY, PipelineNodeEventReceiver};
use super::runtime::{NativeAgentAssemblyError, NativeAgentAssemblyErrorCode};

/// The pending-state envelope's own revision, checked on the way back in.
const PIPELINE_TOOL_PENDING_SCHEMA: &str = "elitea.pipeline-tool-pending.v1";
const PIPELINE_TOOL_FAMILY_SCHEMA: &str = "elitea.pipeline-tool-pending.v2";
const PIPELINE_TOOL_TYPED_FAMILY_SCHEMA: &str = "elitea.pipeline-tool-pending.v3";

use super::graph::static_tool_pause::{
    PipelineCheckpointFamilyView, ValidatedStaticToolContinuation,
};
use super::pipeline::scoped_applications::{
    PipelineApplicationScopeRoute, events_for_scope, route_from_event,
};

#[derive(Clone, Copy, Deserialize, Eq, PartialEq, Serialize)]
#[serde(rename_all = "snake_case")]
enum PipelineToolPauseKind {
    DynamicNode,
    Static,
    Application,
}

use super::pipeline::composition::{
    MAX_PIPELINE_CHECKPOINT_PATHS, MAX_PIPELINE_COMPOSITION_DEPTH, PipelineCheckpointCatalog,
};

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

/// The bounded descendant family for an ordinary-parent graph pause.
#[derive(Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
struct PipelineToolPendingFamily {
    schema_revision: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pause_kind: Option<PipelineToolPauseKind>,
    thread_id: String,
    checkpoint_id: String,
    node_name: String,
    checkpoint: Checkpoint,
    catalog: PipelineCheckpointCatalog,
    descendant_checkpoints: Vec<Checkpoint>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    call_lineage: Option<PipelineToolCallLineage>,
}

#[derive(Deserialize, Serialize)]
#[serde(untagged)]
#[allow(clippy::large_enum_variant)] // Both immutable checkpoint shapes are byte-bounded at decode.
enum PipelinePendingEnvelope {
    Family(PipelineToolPendingFamily),
    Legacy(PipelineToolPending),
}

impl PipelinePendingEnvelope {
    fn root(&self) -> &Checkpoint {
        match self {
            Self::Family(value) => &value.checkpoint,
            Self::Legacy(value) => &value.checkpoint,
        }
    }

    fn root_identity(&self) -> (&str, &str, &str) {
        match self {
            Self::Family(value) => (&value.thread_id, &value.checkpoint_id, &value.node_name),
            Self::Legacy(value) => (&value.thread_id, &value.checkpoint_id, &value.node_name),
        }
    }

    fn validate(&self) -> Result<(), PipelinePauseError> {
        let root = self.root();
        let (thread, checkpoint_id, pending_node) = self.root_identity();
        let static_family = matches!(self, Self::Family(value) if
            value.schema_revision == PIPELINE_TOOL_TYPED_FAMILY_SCHEMA && value.pause_kind == Some(PipelineToolPauseKind::Static));
        if root.thread_id != thread
            || root.checkpoint_id != checkpoint_id
            || !valid_family_identity(thread)
            || !valid_family_identity(checkpoint_id)
            || (!static_family && root.pending_nodes.as_slice() != [pending_node])
        {
            return Err(PipelinePauseError::Corrupt);
        }
        match self {
            Self::Legacy(value) if value.schema_revision == PIPELINE_TOOL_PENDING_SCHEMA => Ok(()),
            Self::Family(value)
                if (value.schema_revision == PIPELINE_TOOL_FAMILY_SCHEMA
                    && value.pause_kind.is_none())
                    || (value.schema_revision == PIPELINE_TOOL_TYPED_FAMILY_SCHEMA
                        && value.pause_kind.is_some()) =>
            {
                if value
                    .call_lineage
                    .as_ref()
                    .is_some_and(|lineage| lineage.validate().is_err())
                    || (matches!(
                        value.pause_kind,
                        Some(PipelineToolPauseKind::Application | PipelineToolPauseKind::Static)
                    ) && value.call_lineage.is_none())
                {
                    return Err(PipelinePauseError::Corrupt);
                }
                validate_family_catalog(&value.catalog)?;
                if value.descendant_checkpoints.len() > MAX_PIPELINE_CHECKPOINT_PATHS {
                    return Err(PipelinePauseError::Corrupt);
                }
                let mut threads = BTreeSet::from([root.thread_id.as_str()]);
                let mut ids = BTreeSet::from([root.checkpoint_id.as_str()]);
                for checkpoint in &value.descendant_checkpoints {
                    let path = checkpoint
                        .thread_id
                        .strip_prefix(&format!("{thread}/"))
                        .ok_or(PipelinePauseError::Corrupt)?;
                    if !value.catalog.descendants.contains_key(path)
                        || !valid_family_identity(&checkpoint.checkpoint_id)
                        || !threads.insert(checkpoint.thread_id.as_str())
                        || !ids.insert(checkpoint.checkpoint_id.as_str())
                    {
                        return Err(PipelinePauseError::Corrupt);
                    }
                }
                Ok(())
            }
            _ => Err(PipelinePauseError::Corrupt),
        }
    }

    fn checkpoint(&self, thread: &str) -> Option<&Checkpoint> {
        if self.root().thread_id == thread {
            return Some(self.root());
        }
        match self {
            Self::Family(value) => value
                .descendant_checkpoints
                .iter()
                .find(|checkpoint| checkpoint.thread_id == thread),
            Self::Legacy(_) => None,
        }
    }
}

impl PipelineCheckpointFamilyView for PipelinePendingEnvelope {
    fn root(&self) -> &Checkpoint {
        self.root()
    }
    fn checkpoint(&self, thread: &str) -> Option<&Checkpoint> {
        self.checkpoint(thread)
    }
    fn catalog(&self) -> Option<&PipelineCheckpointCatalog> {
        match self {
            Self::Family(value) => Some(&value.catalog),
            Self::Legacy(_) => None,
        }
    }
}

fn valid_family_identity(value: &str) -> bool {
    !value.is_empty() && value.len() <= 1024 && !value.chars().any(char::is_control)
}

/// Structural family read access. Exact kind/policy/revision proof belongs to its typed resolver.
pub(crate) fn decoded_pipeline_tool_family(
    event: &Event,
) -> Result<impl PipelineCheckpointFamilyView, PipelinePauseError> {
    family_from_event(event)
}

fn family_from_event(event: &Event) -> Result<PipelinePendingEnvelope, PipelinePauseError> {
    let raw = event
        .provider_metadata
        .get(PIPELINE_TOOL_PENDING_METADATA_KEY)
        .ok_or(PipelinePauseError::Corrupt)?;
    if raw.len() > MAX_PIPELINE_TOOL_PENDING_BYTES {
        return Err(PipelinePauseError::Corrupt);
    }
    let pending: PipelinePendingEnvelope =
        serde_json::from_str(raw).map_err(|_| PipelinePauseError::Corrupt)?;
    pending.validate()?;
    let payload = GraphInterruptPayload::from_event(event).ok_or(PipelinePauseError::Corrupt)?;
    if payload.thread_id != pending.root().thread_id
        || payload.checkpoint_id != pending.root().checkpoint_id
    {
        return Err(PipelinePauseError::Corrupt);
    }
    Ok(pending)
}

#[derive(Clone, Deserialize, Eq, PartialEq, Serialize)]
#[serde(deny_unknown_fields)]
struct PipelineToolBoundaryReceipt {
    schema: String,
    container_invocation_id: String,
    parent_call_id: String,
    checkpoint_thread_id: String,
}

fn boundary_receipt(
    event: &Event,
) -> Result<Option<PipelineToolBoundaryReceipt>, NativeAgentAssemblyError> {
    let Some(raw) = event
        .provider_metadata
        .get(PIPELINE_TOOL_BOUNDARY_METADATA_KEY)
    else {
        return Ok(None);
    };
    if raw.len() > 4096 {
        return Err(invalid_boundary());
    }
    let value: PipelineToolBoundaryReceipt =
        serde_json::from_str(raw).map_err(|_| invalid_boundary())?;
    if value.schema != "elitea.pipeline.tool-boundary.v1"
        || [
            &value.container_invocation_id,
            &value.parent_call_id,
            &value.checkpoint_thread_id,
        ]
        .iter()
        .any(|value| !valid_family_identity(value))
    {
        return Err(invalid_boundary());
    }
    Ok(Some(value))
}

/// Captured from the native model event before its tool future is polled. Stored
/// ordinary events remain the authority; this receipt binds a family across replay containers.
#[derive(Clone, Deserialize, Eq, PartialEq, Serialize)]
#[serde(deny_unknown_fields)]
pub(crate) struct PipelineToolCallLineage {
    schema: String,
    original_batch_event_id: String,
    original_ordinal: usize,
    parent_call_id: String,
    tool_name: String,
    arguments_digest: String,
}
impl PipelineToolCallLineage {
    pub(crate) fn from_call(event: &Event, index: usize) -> Result<Self, NativeAgentAssemblyError> {
        let calls = event.tool_calls();
        let call = calls.get(index).ok_or_else(invalid_boundary)?;
        let call_id = call.call_id.ok_or_else(invalid_boundary)?;
        let value = Self {
            schema: "elitea.pipeline.tool-call.v1".to_owned(),
            original_batch_event_id: super::application_tools::original_application_batch_id(
                event,
            )?,
            original_ordinal: super::application_tools::application_replay_ordinal(event, call_id)?
                .unwrap_or(index + 1),
            parent_call_id: call_id.to_owned(),
            tool_name: call.name.to_owned(),
            arguments_digest: super::pipeline::scope_receipts::arguments_digest(call.args)
                .map_err(|_| invalid_boundary())?,
        };
        value.validate()?;
        Ok(value)
    }
    fn validate(&self) -> Result<(), NativeAgentAssemblyError> {
        if self.schema != "elitea.pipeline.tool-call.v1"
            || !(1..=16).contains(&self.original_ordinal)
            || [
                &self.original_batch_event_id,
                &self.parent_call_id,
                &self.tool_name,
            ]
            .iter()
            .any(|v| !valid_family_identity(v))
            || self.arguments_digest.len() != 64
            || !self
                .arguments_digest
                .bytes()
                .all(|byte| byte.is_ascii_hexdigit())
        {
            return Err(invalid_boundary());
        }
        Ok(())
    }
    pub(crate) fn matches_projection(
        &self,
        batch: &str,
        ordinal: usize,
        name: &str,
        args: &Value,
    ) -> Result<bool, NativeAgentAssemblyError> {
        Ok(self.original_batch_event_id == batch
            && self.original_ordinal == ordinal
            && self.matches(name, args)?)
    }
    pub(crate) fn matches(
        &self,
        name: &str,
        args: &Value,
    ) -> Result<bool, NativeAgentAssemblyError> {
        self.validate()?;
        Ok(self.tool_name == name
            && self.arguments_digest
                == super::pipeline::scope_receipts::arguments_digest(args)
                    .map_err(|_| invalid_boundary())?)
    }
}

pub(crate) const STATIC_TOOL_THREAD_METADATA_KEY: &str = "elitea.pipeline.static-tool-thread.v1";
#[derive(Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
struct StaticToolThreadReceipt {
    schema: String,
    lineage: PipelineToolCallLineage,
}

/// Match a private producer receipt to the exact model call already admitted by the projector.
pub(crate) fn static_thread_for_projected_call(
    event: &Event,
    conversation: &str,
    batch: &str,
    ordinal: usize,
    call: &str,
    name: &str,
    args: &Value,
) -> Result<Option<String>, NativeAgentAssemblyError> {
    let Some(raw) = event.provider_metadata.get(STATIC_TOOL_THREAD_METADATA_KEY) else {
        return Ok(None);
    };
    if raw.len() > 4096 {
        return Err(invalid_boundary());
    }
    let receipt: StaticToolThreadReceipt =
        serde_json::from_str(raw).map_err(|_| invalid_boundary())?;
    if receipt.schema != STATIC_TOOL_THREAD_METADATA_KEY
        || receipt.lineage.original_batch_event_id != batch
        || receipt.lineage.original_ordinal != ordinal
        || receipt.lineage.parent_call_id != call
        || !receipt.lineage.matches(name, args)?
    {
        return Err(invalid_boundary());
    }
    let thread = static_checkpoint_thread(conversation, &receipt.lineage)?;
    if event
        .provider_metadata
        .get(DESCENDANT_CHECKPOINT_THREAD_KEY)
        != Some(&thread)
    {
        return Err(invalid_boundary());
    }
    if event
        .provider_metadata
        .contains_key(PIPELINE_TOOL_PENDING_METADATA_KEY)
        && let PipelinePendingEnvelope::Family(family) =
            family_from_event(event).map_err(|_| invalid_boundary())?
        && family
            .call_lineage
            .as_ref()
            .is_some_and(|lineage| lineage != &receipt.lineage)
    {
        return Err(invalid_boundary());
    }
    Ok(Some(thread))
}

/// Prove the outer occurrence transport receipt before comparing a checkpoint-owned start.
pub(crate) fn validate_static_thread_for_saved_start(
    event: &Event,
    root_thread: &str,
    lineage: &PipelineToolCallLineage,
) -> Result<(), NativeAgentAssemblyError> {
    let raw = event
        .provider_metadata
        .get(STATIC_TOOL_THREAD_METADATA_KEY)
        .ok_or_else(invalid_boundary)?;
    if raw.len() > 4096 {
        return Err(invalid_boundary());
    }
    let receipt: StaticToolThreadReceipt =
        serde_json::from_str(raw).map_err(|_| invalid_boundary())?;
    let (conversation, _) = root_thread
        .rsplit_once("/static-v1:")
        .ok_or_else(invalid_boundary)?;
    if receipt.schema != STATIC_TOOL_THREAD_METADATA_KEY
        || receipt.lineage != *lineage
        || static_checkpoint_thread(conversation, lineage)? != root_thread
        || event
            .provider_metadata
            .get(DESCENDANT_CHECKPOINT_THREAD_KEY)
            .map(String::as_str)
            != Some(root_thread)
        || event.provider_metadata.get(DESCENDANT_PARENT_CALL_KEY) != Some(&lineage.parent_call_id)
    {
        return Err(invalid_boundary());
    }
    Ok(())
}

fn static_checkpoint_thread(
    conversation: &str,
    lineage: &PipelineToolCallLineage,
) -> Result<String, NativeAgentAssemblyError> {
    lineage.validate()?;
    if !valid_family_identity(conversation) {
        return Err(invalid_boundary());
    }
    let mut digest = ring::digest::Context::new(&ring::digest::SHA256);
    digest.update(b"elitea.pipeline.static-tool-thread.v1\0");
    digest.update(conversation.as_bytes());
    digest.update(&serde_json::to_vec(lineage).map_err(|_| invalid_boundary())?);
    let mut suffix = String::with_capacity(64);
    for byte in digest.finish().as_ref() {
        let _ = std::fmt::Write::write_fmt(&mut suffix, format_args!("{byte:02x}"));
    }
    let thread = format!("{conversation}/static-v1:{suffix}");
    if thread.len() > 512 || !valid_family_identity(&thread) {
        return Err(invalid_boundary());
    }
    Ok(thread)
}

fn owns_tool_boundary(event: &Event, container: &str, call: &str) -> adk_rust::Result<bool> {
    match (
        event
            .provider_metadata
            .get(DESCENDANT_CONTAINER_INVOCATION_KEY),
        event.provider_metadata.get(DESCENDANT_PARENT_CALL_KEY),
    ) {
        (None, None) => Ok(true),
        (Some(recorded_container), Some(recorded_call)) => {
            Ok(recorded_container == container && recorded_call == call)
        }
        _ => Err(child_execution_error()),
    }
}

/// Exact graph boundary selected from durable events. This is not runtime admission.
pub(crate) struct PipelineApplicationBoundary {
    pub(crate) container_invocation_id: String,
    pub(crate) parent_call_id: String,
    pub(crate) checkpoint_thread_id: String,
    pub(crate) pending_event: Event,
    pub(crate) scope_route: PipelineApplicationScopeRoute,
    pub(crate) scope_events: Vec<Event>,
    pub(crate) checkpoint: Box<PipelineToolResume>,
}

/// Identify a typed Application wrapper after its ordinary leaves have been handled.
/// This structural classifier grants no resume or retained replay authority.
pub(crate) fn pipeline_application_pending_event(
    event: &Event,
) -> Result<bool, NativeAgentAssemblyError> {
    if !event
        .provider_metadata
        .contains_key(PIPELINE_TOOL_PENDING_METADATA_KEY)
    {
        return Ok(false);
    }
    let pending = family_from_event(event).map_err(|_| invalid_boundary())?;
    let PipelinePendingEnvelope::Family(family) = &pending else {
        return Ok(false);
    };
    if family.pause_kind != Some(PipelineToolPauseKind::Application) {
        return Ok(false);
    }
    let boundary = boundary_receipt(event)?.ok_or_else(invalid_boundary)?;
    if boundary.checkpoint_thread_id != family.thread_id {
        return Err(invalid_boundary());
    }
    let payload = GraphInterruptPayload::from_event(event).ok_or_else(invalid_boundary)?;
    if payload.kind != "dynamic" || payload.node.is_some() {
        return Err(invalid_boundary());
    }
    boundary::application_family_interrupt_ids(&pending, &payload)?;
    let mut data = payload.data.as_ref().ok_or_else(invalid_boundary)?;
    let mut checkpoint = pending.root();
    let mut thread = family.thread_id.clone();
    let mut depth = 0;
    while let Some(node) = data.get("subgraph").and_then(Value::as_str) {
        depth += 1;
        if depth > MAX_PIPELINE_COMPOSITION_DEPTH || checkpoint.pending_nodes.as_slice() != [node] {
            return Err(invalid_boundary());
        }
        thread.push('/');
        thread.push_str(node);
        checkpoint = pending.checkpoint(&thread).ok_or_else(invalid_boundary)?;
        if data.get("thread").and_then(Value::as_str) != Some(thread.as_str())
            || data.get("checkpoint_id").and_then(Value::as_str)
                != Some(checkpoint.checkpoint_id.as_str())
        {
            return Err(invalid_boundary());
        }
        data = data.get("data").ok_or_else(invalid_boundary)?;
    }
    let node = data
        .get("node_name")
        .and_then(Value::as_str)
        .ok_or_else(invalid_boundary)?;
    if checkpoint.pending_nodes.as_slice() != [node]
        || data.get("schema_revision").and_then(Value::as_str)
            != Some(super::graph::PIPELINE_APPLICATION_HITL_SCHEMA)
        || data.get("guardrail_type").and_then(Value::as_str) != Some("application_sensitive_tool")
    {
        return Err(invalid_boundary());
    }
    Ok(true)
}

/// Stop ordinary history reconstruction at the saved-tool boundary before following graph hops.
/// The rebuilt registry still proves exact live admission and installs child decisions.
#[allow(clippy::too_many_lines)] // Keep exact family, lineage, hydration, and coordinator checks before graph effects.
pub(crate) fn pipeline_application_boundary(
    events: &[Event],
    leaf: &Event,
) -> Result<Option<PipelineApplicationBoundary>, NativeAgentAssemblyError> {
    let static_leaf = static_pipeline_tool_pause(leaf)?;
    if static_leaf.is_some() && !leaf.provider_metadata.contains_key(BOUNDARY_LEDGER_KEY) {
        return Ok(None);
    }
    let Some(boundary) = boundary_ledger::outer_boundary_receipt(leaf)? else {
        return Ok(None);
    };
    let (_, original_lineage) = boundary::boundary_call(
        events,
        &boundary.container_invocation_id,
        &boundary.parent_call_id,
    )?;
    boundary_ledger::validate_outer_lineage(leaf, &original_lineage)?;
    let leaf_position = events
        .iter()
        .position(|event| event.id == leaf.id)
        .ok_or_else(invalid_boundary)?;
    // Only the newest persisted family for this exact ordinary-parent tool is eligible.
    let mut candidate = None;
    for (position, event) in events.iter().enumerate().rev() {
        if !event
            .provider_metadata
            .contains_key(PIPELINE_TOOL_PENDING_METADATA_KEY)
        {
            continue;
        }
        let Some(recorded) = boundary_ledger::outer_boundary_receipt(event)? else {
            continue;
        };
        if recorded.parent_call_id == boundary.parent_call_id
            && recorded.checkpoint_thread_id == boundary.checkpoint_thread_id
        {
            let decoded = family_from_event(event).map_err(|_| invalid_boundary())?;
            let PipelinePendingEnvelope::Family(family) = decoded else {
                return Err(invalid_boundary());
            };
            let (_, candidate_lineage) = boundary::boundary_call(
                events,
                &recorded.container_invocation_id,
                &recorded.parent_call_id,
            )?;
            if candidate_lineage != original_lineage
                || family.call_lineage.as_ref() != Some(&candidate_lineage)
            {
                return Err(invalid_boundary());
            }
            candidate = Some((position, event));
            break;
        }
    }
    let (position, pending_event) = candidate.ok_or_else(invalid_boundary)?;
    if leaf_position > position {
        return Err(invalid_boundary());
    }
    let pending = family_from_event(pending_event).map_err(|_| invalid_boundary())?;
    if !matches!(&pending, PipelinePendingEnvelope::Family(value) if
        value.schema_revision == PIPELINE_TOOL_TYPED_FAMILY_SCHEMA && value.pause_kind == Some(PipelineToolPauseKind::Application))
    {
        return Err(invalid_boundary());
    }
    let current_boundary =
        boundary_ledger::outer_boundary_receipt(pending_event)?.ok_or_else(invalid_boundary)?;
    let scope_route = route_from_event(&boundary.checkpoint_thread_id, &pending, leaf)?
        .ok_or_else(invalid_boundary)?;
    let payload = GraphInterruptPayload::from_event(pending_event).ok_or_else(invalid_boundary)?;
    if payload.kind != "dynamic" || payload.node.is_some() {
        return Err(invalid_boundary());
    }
    let mut data = payload.data.as_ref().ok_or_else(invalid_boundary)?;
    // Prove every wrapper in the original graph pause names the same checkpoint route.
    for child in &scope_route.descendants {
        let node = child
            .graph_path
            .rsplit('/')
            .next()
            .ok_or_else(invalid_boundary)?;
        let thread = format!("{}/{}", boundary.checkpoint_thread_id, child.graph_path);
        if data.get("subgraph").and_then(Value::as_str) != Some(node)
            || data.get("thread").and_then(Value::as_str) != Some(thread.as_str())
            || data.get("checkpoint_id").and_then(Value::as_str)
                != Some(child.checkpoint_id.as_str())
        {
            return Err(invalid_boundary());
        }
        data = data.get("data").ok_or_else(invalid_boundary)?;
    }
    if data.get("schema_revision").and_then(Value::as_str)
        != Some(super::graph::PIPELINE_APPLICATION_HITL_SCHEMA)
        || data.get("guardrail_type").and_then(Value::as_str) != Some("application_sensitive_tool")
        || data.get("node_name").and_then(Value::as_str)
            != Some(scope_route.leaf_node_name.as_str())
        || data.get("application_call_id").and_then(Value::as_str)
            != Some(scope_route.application_call_id.as_str())
    {
        return Err(invalid_boundary());
    }
    let ids = data
        .get("interrupt_ids")
        .and_then(Value::as_array)
        .ok_or_else(invalid_boundary)?;
    let mut unique = BTreeSet::new();
    if ids.is_empty()
        || ids.len() > 16
        || ids.iter().any(|value| {
            value.as_str().is_none_or(|identity| {
                !valid_family_identity(identity) || !unique.insert(identity.to_owned())
            })
        })
    {
        return Err(invalid_boundary());
    }
    let leaf_interrupt_id = if let Some(pause) = static_leaf {
        let (original, _) = boundary::boundary_call(
            events,
            &pause.container_invocation_id,
            &pause.parent_call_id,
        )?;
        pause.matches_original_call(&original)?;
        pause.pause_id
    } else {
        let confirmation = leaf
            .actions
            .tool_confirmation
            .as_ref()
            .ok_or_else(invalid_boundary)?;
        let call_id = confirmation
            .function_call_id
            .as_deref()
            .ok_or_else(invalid_boundary)?;
        super::direct_hitl::sensitive_call_identity(
            &leaf.invocation_id,
            call_id,
            &confirmation.tool_name,
            &confirmation.args,
        )
        .map_err(|_| invalid_boundary())?
        .0
    };
    if !unique.contains(&leaf_interrupt_id) {
        return Err(invalid_boundary());
    }
    let interrupt_id = leaf_interrupt_id;
    let scope_events = events_for_scope(
        &events[..=position],
        leaf,
        &boundary.checkpoint_thread_id,
        pending.catalog().ok_or_else(invalid_boundary)?,
    )?;
    for event in &scope_events {
        validate_scoped_outer_boundary(
            event,
            &boundary.checkpoint_thread_id,
            Some(&original_lineage),
        )?;
    }
    let PipelinePendingEnvelope::Family(family) = pending else {
        return Err(invalid_boundary());
    };
    let checkpoint = Box::new(PipelineToolResume {
        interrupt_id,
        thread_id: family.thread_id,
        checkpoint: family.checkpoint,
        descendant_checkpoints: family.descendant_checkpoints,
        catalog: Some(family.catalog),
        resume_state: State::new(),
    });
    Ok(Some(PipelineApplicationBoundary {
        container_invocation_id: current_boundary.container_invocation_id,
        parent_call_id: boundary.parent_call_id,
        checkpoint_thread_id: boundary.checkpoint_thread_id,
        pending_event: pending_event.clone(),
        scope_route,
        scope_events,
        checkpoint,
    }))
}

const fn invalid_boundary() -> NativeAgentAssemblyError {
    NativeAgentAssemblyError::new(
        NativeAgentAssemblyErrorCode::InvalidConfiguration,
        "the pipeline application boundary does not match its original checkpoint family",
    )
}

/// Adapt a typed static proof only after the static resolver validates rebuilt catalogs.
/// No mutation or checkpoint reseeding occurs here.
pub(crate) fn static_pipeline_tool_resume(
    event: &Event,
    proof: &ValidatedStaticToolContinuation,
) -> Result<PipelineToolResume, PipelinePauseError> {
    let pending = family_from_event(event)?;
    let PipelinePendingEnvelope::Family(family) = pending else {
        return Err(PipelinePauseError::Corrupt);
    };
    if family.schema_revision != PIPELINE_TOOL_TYPED_FAMILY_SCHEMA
        || family.pause_kind != Some(PipelineToolPauseKind::Static)
        || serde_json::to_value(&family.checkpoint).map_err(|_| PipelinePauseError::Corrupt)?
            != serde_json::to_value(proof.root_checkpoint())
                .map_err(|_| PipelinePauseError::Corrupt)?
    {
        return Err(PipelinePauseError::Corrupt);
    }
    for (thread, id) in proof.proven_checkpoint_ids() {
        let checkpoint = if thread == &family.thread_id {
            Some(&family.checkpoint)
        } else {
            family
                .descendant_checkpoints
                .iter()
                .find(|checkpoint| checkpoint.thread_id == *thread)
        }
        .ok_or(PipelinePauseError::Corrupt)?;
        if checkpoint.checkpoint_id != *id {
            return Err(PipelinePauseError::Corrupt);
        }
    }
    // After normalization is authority only for exact checkpoints already proved above.
    let recorded_normalization: BTreeMap<String, String> = proof
        .resume_state()
        .get(super::graph::static_pause::STATIC_AFTER_CHECKPOINTS_STATE_KEY)
        .map(|value| serde_json::from_value(value.clone()).map_err(|_| PipelinePauseError::Corrupt))
        .transpose()?
        .unwrap_or_default();
    if &recorded_normalization != proof.normalization_ids()
        || proof
            .normalization_ids()
            .iter()
            .any(|(thread, id)| proof.proven_checkpoint_ids().get(thread) != Some(id))
    {
        return Err(PipelinePauseError::Corrupt);
    }
    Ok(PipelineToolResume {
        interrupt_id: proof.pause_id().to_owned(),
        thread_id: family.thread_id,
        checkpoint: family.checkpoint,
        descendant_checkpoints: family.descendant_checkpoints,
        catalog: Some(family.catalog),
        resume_state: {
            let mut state = proof.resume_state().clone();
            if state.contains_key(super::graph::static_pause::STATIC_TEXT_RESUME_STATE_KEY) {
                state.remove("input");
                state.remove("messages");
            }
            state
        },
    })
}

fn validate_family_catalog(catalog: &PipelineCheckpointCatalog) -> Result<(), PipelinePauseError> {
    let root = catalog.root.as_ref().ok_or(PipelinePauseError::Corrupt)?;
    if root.application_id == 0
        || root.version_id == 0
        || root.definition_digest == [0; 32]
        || catalog.descendants.len() > MAX_PIPELINE_CHECKPOINT_PATHS
    {
        return Err(PipelinePauseError::Corrupt);
    }
    for (path, revision) in &catalog.descendants {
        let parts = path.split('/').collect::<Vec<_>>();
        if parts.len() > MAX_PIPELINE_COMPOSITION_DEPTH
            || parts.iter().any(|part| {
                part.is_empty()
                    || matches!(*part, "." | "..")
                    || part.len() > 128
                    || !part.bytes().all(|byte| {
                        byte.is_ascii_alphanumeric() || matches!(byte, b'_' | b'-' | b'.' | b':')
                    })
            })
            || revision.application_id == 0
            || revision.version_id == 0
            || revision.definition_digest == [0; 32]
        {
            return Err(PipelinePauseError::Corrupt);
        }
        let mut ancestor = path.as_str();
        while let Some((parent, _)) = ancestor.rsplit_once('/') {
            if !catalog.descendants.contains_key(parent) {
                return Err(PipelinePauseError::Corrupt);
            }
            ancestor = parent;
        }
    }
    Ok(())
}

/// One checkpoint-proven continuation of a paused child pipeline.
pub(crate) struct PipelineToolResume {
    interrupt_id: String,
    thread_id: String,
    checkpoint: Checkpoint,
    descendant_checkpoints: Vec<Checkpoint>,
    catalog: Option<PipelineCheckpointCatalog>,
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
    pending: PipelinePendingEnvelope,
    nested_checkpoints: Vec<(String, String, String)>,
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
        self.pending.validate()?;
        let (_, _, root_node) = self.pending.root_identity();
        let expected_root_node = self
            .nested_checkpoints
            .first()
            .map_or(self.node_name.as_str(), |entry| entry.0.as_str());
        if root_node != expected_root_node
            || self.pending.root().thread_id != self.checkpoint_thread_id
        {
            return Err(PipelinePauseError::Corrupt);
        }
        for (index, (_, thread, id)) in self.nested_checkpoints.iter().enumerate() {
            let checkpoint = self
                .pending
                .checkpoint(thread)
                .ok_or(PipelinePauseError::Corrupt)?;
            let next_node = self
                .nested_checkpoints
                .get(index + 1)
                .map_or(self.node_name.as_str(), |entry| entry.0.as_str());
            if checkpoint.checkpoint_id != *id || checkpoint.pending_nodes.as_slice() != [next_node]
            {
                return Err(PipelinePauseError::Corrupt);
            }
        }
        let (checkpoint, descendant_checkpoints, catalog) = match self.pending {
            PipelinePendingEnvelope::Legacy(value) => (value.checkpoint, Vec::new(), None),
            PipelinePendingEnvelope::Family(value) => (
                value.checkpoint,
                value.descendant_checkpoints,
                Some(value.catalog),
            ),
        };
        Ok(PipelineToolResume {
            interrupt_id: self.interrupt_id,
            thread_id: self.checkpoint_thread_id,
            checkpoint,
            descendant_checkpoints,
            catalog,
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
    let pending = serde_json::from_str::<PipelinePendingEnvelope>(raw)
        .map_err(|_| PipelinePauseError::Corrupt)?;
    pending.validate()?;
    if matches!(&pending, PipelinePendingEnvelope::Family(value) if value.pause_kind.is_some_and(|kind| kind != PipelineToolPauseKind::DynamicNode))
    {
        // Typed ordinary/static coordinators own these families. They are not dynamic HITL-node cards.
        return Ok(None);
    }
    if pending.root_identity().0 != checkpoint_thread_id {
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
        nested_checkpoints: binding
            .nested_checkpoints()
            .iter()
            .map(|checkpoint| {
                (
                    checkpoint.node_name().to_owned(),
                    checkpoint.thread_id().to_owned(),
                    checkpoint.checkpoint_id().to_owned(),
                )
            })
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
                    super::application_tools::child_failure_report(&error, None, false)
                {
                    tracing::error!(
                        event = "nested_pipeline_failed",
                        invocation_id = %ctx.invocation_id(),
                        function_call_id = %ctx.function_call_id(),
                        error_code = error.code,
                        cause_message = report["failure"]["message"].as_str(),
                        recovery = report["failure"]["recovery_action"].as_str(),
                        "child pipeline model failed; returning failure to the orchestrator"
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

    async fn checkpoint_thread_for_call(
        &self,
        ctx: &dyn ToolContext,
        arguments: &Value,
    ) -> adk_rust::Result<String> {
        let static_tool = self.definition.has_static_interrupts()
            || self
                .runtimes
                .composition_definitions()
                .is_some_and(|definitions| {
                    definitions
                        .values()
                        .any(PipelineDefinition::has_static_interrupts)
                });
        if !static_tool
            && !self
                .runtimes
                .application_scopes()
                .is_some_and(super::pipeline::scoped_applications::PipelineApplicationScopeRegistry::has_ordinary_applications)
        {
            return Ok(self.checkpoint_thread_id(ctx.function_call_id()));
        }
        let lineage = self
            .resume
            .as_ref()
            .ok_or_else(child_execution_error)?
            .pipeline_call_lineage(
                ctx.invocation_id(),
                ctx.function_call_id(),
                &self.name,
                arguments,
            )
            .await
            .map_err(|_| child_execution_error())?;
        static_checkpoint_thread(&self.conversation_thread_id, &lineage)
            .map_err(|_| child_execution_error())
    }

    #[allow(clippy::too_many_lines)] // Keep exact family, lineage, hydration, and coordinator checks before graph effects.
    async fn invoke_child(
        &self,
        ctx: Arc<dyn ToolContext>,
        arguments: Value,
    ) -> adk_rust::Result<Value> {
        let task = application_task(&arguments)
            .map_err(|_| tool_input_error())?
            .to_owned();
        let thread_id = self
            .checkpoint_thread_for_call(ctx.as_ref(), &arguments)
            .await?;
        let resume = match &self.resume {
            Some(coordinator) => {
                coordinator
                    .take(ctx.invocation_id(), ctx.function_call_id())
                    .await?
            }
            None => None,
        };
        if let Some(resume) = &resume
            && let Some(result) = resume
                .retained_result(
                    ctx.as_ref(),
                    &self.name,
                    &arguments,
                    self.event_sender.as_ref(),
                )
                .await?
        {
            return Ok(result);
        }
        let original_arguments = arguments.clone();
        let prepared = resume
            .map(|resume| {
                resume.into_pipeline_with_static(
                    &self.name,
                    &arguments,
                    &self.definition,
                    &self.runtimes,
                    &thread_id,
                )
            })
            .transpose()?;
        let (resume, scopes, static_scopes) = prepared.map_or_else(
            || (None, Vec::new(), Vec::new()),
            |prepared| {
                (
                    Some(prepared.checkpoint),
                    prepared.scopes,
                    prepared.static_scopes,
                )
            },
        );
        tracing::Span::current().record("resumed", resume.is_some());
        let checkpointer: Arc<dyn Checkpointer> = Arc::new(MemoryCheckpointer::new());
        let pipeline_resume = match resume {
            Some(resume) => {
                if resume.thread_id != thread_id {
                    return Err(tool_input_error());
                }
                let actual_catalog = self.runtimes.checkpoint_catalog();
                match (&resume.catalog, actual_catalog) {
                    (Some(recorded), Some(actual)) if recorded == actual => {}
                    (None, Some(actual)) if !actual.descendants.is_empty() => {
                        return Err(tool_input_error());
                    }
                    (None, _) => {}
                    _ => return Err(tool_input_error()),
                }
                for checkpoint in &resume.descendant_checkpoints {
                    checkpointer
                        .save(checkpoint)
                        .await
                        .map_err(|_| child_execution_error())?;
                }
                checkpointer
                    .save(&resume.checkpoint)
                    .await
                    .map_err(|_| child_execution_error())?;
                Some(PipelineResume::from_state(resume.resume_state))
            }
            None => None,
        };
        if !scopes.is_empty() {
            // Admission remains closed until the exact occurrence registry is produced.
            let registry = self
                .runtimes
                .application_scopes()
                .ok_or_else(tool_input_error)?;
            let catalog = self
                .runtimes
                .checkpoint_catalog()
                .ok_or_else(tool_input_error)?;
            registry
                .validate_catalog(&self.definition, catalog)
                .map_err(|_| tool_input_error())?;
            // Prove every selected scope before installing any coordinator or compiling effects.
            let mut proved = Vec::with_capacity(scopes.len());
            for scope in scopes {
                let proof = registry
                    .validate_continuation(
                        &thread_id,
                        checkpointer.as_ref(),
                        &scope.route,
                        &scope.events,
                    )
                    .await
                    .map_err(|_| tool_input_error())?;
                proved.push((proof, scope.decisions));
            }
            for (proof, decisions) in proved {
                proof
                    .install(decisions)
                    .await
                    .map_err(|_| tool_input_error())?;
            }
        }
        if !static_scopes.is_empty() {
            let registry = self
                .runtimes
                .application_scopes()
                .ok_or_else(tool_input_error)?;
            let catalog = self
                .runtimes
                .checkpoint_catalog()
                .ok_or_else(tool_input_error)?;
            registry
                .validate_catalog(&self.definition, catalog)
                .map_err(|_| tool_input_error())?;
            let mut proved = Vec::with_capacity(static_scopes.len());
            let lineage = self
                .resume
                .as_ref()
                .ok_or_else(tool_input_error)?
                .pipeline_call_lineage(
                    ctx.invocation_id(),
                    ctx.function_call_id(),
                    &self.name,
                    &original_arguments,
                )
                .await
                .map_err(|_| tool_input_error())?;
            for scope in static_scopes {
                let proof = registry
                    .validate_saved_tool_continuation(
                        &thread_id,
                        checkpointer.as_ref(),
                        &scope.route,
                        &scope.events,
                        &lineage,
                    )
                    .await
                    .map_err(|_| tool_input_error())?;
                proved.push((proof, scope.decisions));
            }
            for (proof, decisions) in proved {
                proof
                    .install_static(decisions)
                    .await
                    .map_err(|_| tool_input_error())?;
            }
        }
        let graph = self
            .definition
            .compile_with_runtime(
                &self.name,
                Arc::clone(&checkpointer),
                pipeline_resume,
                &self.runtimes,
            )
            .map_err(|_| child_execution_error())?;
        let agent: Arc<dyn Agent> = Arc::new(EliteaGraphAgent::new(graph).with_static_interrupts(
            Arc::clone(&checkpointer),
            self.definition.static_pause_catalog(),
        ));
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
            &original_arguments,
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
        arguments: &Value,
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
                        self.forward(ctx, thread_id, arguments, event?).await?;
                    } else {
                        node_events_open = false;
                    }
                    continue;
                }
                next = stream.next() => {
                    // The graph may wrap an error after queuing its typed cause.
                    // Drain that cause before returning the generic graph error.
                    while let Some(queued) = node_events.try_recv() {
                        self.forward(ctx, thread_id, arguments, queued?).await?;
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
                    self.forward(ctx, thread_id, arguments, queued?).await?;
                }
                return self
                    .pause(ctx, thread_id, event, checkpointer, arguments)
                    .await;
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
            self.forward(ctx, thread_id, arguments, event).await?;
        }
        while let Some(event) = node_events.try_recv() {
            self.forward(ctx, thread_id, arguments, event?).await?;
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
    #[allow(clippy::too_many_lines)] // Keep exact family, lineage, hydration, and coordinator checks before graph effects.
    async fn pause(
        &self,
        ctx: &dyn ToolContext,
        thread_id: &str,
        mut event: Event,
        checkpointer: &dyn Checkpointer,
        arguments: &Value,
    ) -> adk_rust::Result<Value> {
        let payload =
            GraphInterruptPayload::from_event(&event).ok_or_else(child_execution_error)?;
        if payload.thread_id != thread_id {
            return Err(child_execution_error());
        }
        let mut terminal = payload.data.as_ref();
        while terminal.is_some_and(|data| data.get("subgraph").is_some()) {
            terminal = terminal.and_then(|data| data.get("data"));
        }
        let static_pause = event
            .provider_metadata
            .contains_key(super::graph::static_pause::STATIC_PAUSE_METADATA_KEY)
            || terminal.is_some_and(|data| {
                data.get("guardrail_type").and_then(Value::as_str) == Some("pipeline_static")
            });
        let checkpoint = checkpointer
            .load_by_id(&payload.checkpoint_id)
            .await
            .map_err(|_| child_execution_error())?
            .ok_or_else(child_execution_error)?;
        if checkpoint.thread_id != thread_id
            || checkpoint.checkpoint_id != payload.checkpoint_id
            || (!static_pause && checkpoint.pending_nodes.len() != 1)
        {
            return Err(child_execution_error());
        }
        let node_name = if static_pause {
            payload
                .node
                .as_ref()
                .or_else(|| checkpoint.pending_nodes.first())
                .cloned()
                .ok_or_else(child_execution_error)?
        } else {
            checkpoint.pending_nodes[0].clone()
        };
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
        let mut terminal = payload.data.as_ref();
        while let Some(data) = terminal {
            if data.get("subgraph").is_some() {
                terminal = data.get("data");
            } else {
                break;
            }
        }
        let application_pause = terminal.is_some_and(|data| {
            data.get("guardrail_type").and_then(Value::as_str) == Some("application_sensitive_tool")
        });
        let binding = if application_pause || static_pause {
            None
        } else {
            Some(
                pipeline_hitl_event_binding(&projected, &self.name, thread_id).map_err(|_| {
                    tracing::error!(
                        tool_name = %self.name,
                        "a pipeline child raised a pause this parent cannot resume"
                    );
                    child_execution_error()
                })?,
            )
        };
        let lineage = if application_pause || static_pause {
            Some(
                self.resume
                    .as_ref()
                    .ok_or_else(child_execution_error)?
                    .pipeline_call_lineage(
                        ctx.invocation_id(),
                        ctx.function_call_id(),
                        &self.name,
                        arguments,
                    )
                    .await
                    .map_err(|_| child_execution_error())?,
            )
        } else {
            None
        };
        let pending = if let Some(catalog) = self.runtimes.checkpoint_catalog() {
            let mut descendant_checkpoints = Vec::new();
            for path in catalog.descendants.keys() {
                let child_thread = format!("{thread_id}/{path}");
                if let Some(child) = checkpointer
                    .load(&child_thread)
                    .await
                    .map_err(|_| child_execution_error())?
                {
                    if child.thread_id != child_thread {
                        return Err(child_execution_error());
                    }
                    descendant_checkpoints.push(child);
                }
            }
            PipelinePendingEnvelope::Family(PipelineToolPendingFamily {
                schema_revision: if application_pause || static_pause {
                    PIPELINE_TOOL_TYPED_FAMILY_SCHEMA
                } else {
                    PIPELINE_TOOL_FAMILY_SCHEMA
                }
                .to_owned(),
                pause_kind: if application_pause {
                    Some(PipelineToolPauseKind::Application)
                } else if static_pause {
                    Some(PipelineToolPauseKind::Static)
                } else {
                    None
                },
                thread_id: thread_id.to_owned(),
                checkpoint_id: payload.checkpoint_id.clone(),
                node_name,
                checkpoint,
                catalog: catalog.clone(),
                descendant_checkpoints,
                call_lineage: lineage,
            })
        } else {
            if static_pause || application_pause {
                return Err(child_execution_error());
            }
            PipelinePendingEnvelope::Legacy(PipelineToolPending {
                schema_revision: PIPELINE_TOOL_PENDING_SCHEMA.to_owned(),
                thread_id: thread_id.to_owned(),
                checkpoint_id: payload.checkpoint_id.clone(),
                node_name,
                checkpoint,
            })
        };
        pending.validate().map_err(|_| child_execution_error())?;
        let interrupt_ids = if static_pause {
            let graph =
                super::events::pipeline_static_event_binding(&projected, &self.name, thread_id)
                    .map_err(|_| child_execution_error())?;
            let proof = graph
                .public_proof(&projected)
                .map_err(|_| child_execution_error())?;
            let id = proof
                .get("pause_id")
                .and_then(Value::as_str)
                .ok_or_else(child_execution_error)?;
            // The same frozen catalogs/IDs are revalidated by the typed receiver before hydration.
            BTreeSet::from([id.to_owned()])
        } else if application_pause {
            boundary::application_family_interrupt_ids(&pending, &payload)
                .map_err(|_| child_execution_error())?
        } else {
            let binding = binding.as_ref().ok_or_else(child_execution_error)?;
            for (index, nested) in binding.nested_checkpoints().iter().enumerate() {
                let child = pending
                    .checkpoint(nested.thread_id())
                    .ok_or_else(child_execution_error)?;
                let next = binding
                    .nested_checkpoints()
                    .get(index + 1)
                    .map_or(binding.node_name(), |entry| entry.node_name());
                if child.checkpoint_id != nested.checkpoint_id()
                    || child.pending_nodes.as_slice() != [next]
                {
                    return Err(child_execution_error());
                }
            }
            BTreeSet::from([binding.interrupt_id().to_owned()])
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
        self.forward(ctx, thread_id, arguments, event).await?;
        Ok(nested_interrupt_result(&interrupt_ids))
    }

    async fn forward(
        &self,
        ctx: &dyn ToolContext,
        thread_id: &str,
        arguments: &Value,
        mut event: Event,
    ) -> adk_rust::Result<()> {
        let Some(sender) = &self.event_sender else {
            return Ok(());
        };
        event.llm_request = None;
        if thread_id != self.checkpoint_thread_id(ctx.function_call_id()) {
            let lineage = self
                .resume
                .as_ref()
                .ok_or_else(child_execution_error)?
                .pipeline_call_lineage(
                    ctx.invocation_id(),
                    ctx.function_call_id(),
                    &self.name,
                    arguments,
                )
                .await
                .map_err(|_| child_execution_error())?;
            if static_checkpoint_thread(&self.conversation_thread_id, &lineage)
                .map_err(|_| child_execution_error())?
                != thread_id
            {
                return Err(child_execution_error());
            }
            if owns_tool_boundary(&event, ctx.invocation_id(), ctx.function_call_id())? {
                let receipt = serde_json::to_string(&StaticToolThreadReceipt {
                    schema: STATIC_TOOL_THREAD_METADATA_KEY.to_owned(),
                    lineage,
                })
                .map_err(|_| child_execution_error())?;
                if receipt.len() > 4096
                    || event
                        .provider_metadata
                        .insert(STATIC_TOOL_THREAD_METADATA_KEY.to_owned(), receipt)
                        .is_some()
                {
                    return Err(child_execution_error());
                }
            }
        }
        if event
            .provider_metadata
            .contains_key(DESCENDANT_CONTAINER_INVOCATION_KEY)
            || event
                .provider_metadata
                .contains_key(DESCENDANT_PARENT_CALL_KEY)
        {
            let catalog = self
                .runtimes
                .checkpoint_catalog_arc()
                .ok_or_else(child_execution_error)?;
            return sender
                .send(ApplicationEventSignal::GraphDescendant {
                    root_container_invocation_id: ctx.invocation_id().to_owned(),
                    root_parent_call_id: ctx.function_call_id().to_owned(),
                    root_checkpoint_thread_id: thread_id.to_owned(),
                    catalog,
                    lineage: if event
                        .provider_metadata
                        .contains_key(PIPELINE_TOOL_BOUNDARY_METADATA_KEY)
                    {
                        Some(
                            self.resume
                                .as_ref()
                                .ok_or_else(child_execution_error)?
                                .pipeline_call_lineage(
                                    ctx.invocation_id(),
                                    ctx.function_call_id(),
                                    &self.name,
                                    arguments,
                                )
                                .await
                                .map_err(|_| child_execution_error())?,
                        )
                    } else {
                        None
                    },
                    event: Box::new(event),
                })
                .await
                .map_err(|_| application_event_channel_error());
        }
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

#[cfg(test)]
mod family_tests {
    use super::super::pipeline::composition::PipelineCheckpointRevision;
    use super::*;
    use std::collections::BTreeMap;

    #[test]
    fn static_checkpoint_namespace_binds_original_batch_call_and_arguments() {
        let mut original = Event::with_id("first-batch", "first-invocation");
        original.llm_response.content = Some(Content {
            role: "model".to_owned(),
            parts: vec![Part::FunctionCall {
                name: "saved_pipeline".to_owned(),
                args: json!({"task":"work"}),
                id: Some("same-call".to_owned()),
                thought_signature: None,
            }],
        });
        let first = PipelineToolCallLineage::from_call(&original, 0).unwrap();
        let first_thread = static_checkpoint_thread("conversation", &first).unwrap();
        assert_eq!(
            first_thread,
            static_checkpoint_thread("conversation", &first).unwrap()
        );
        original.id = "second-batch".to_owned();
        original.invocation_id = "second-invocation".to_owned();
        let second = PipelineToolCallLineage::from_call(&original, 0).unwrap();
        assert_ne!(
            first_thread,
            static_checkpoint_thread("conversation", &second).unwrap()
        );
        if let Part::FunctionCall { args, .. } =
            &mut original.llm_response.content.as_mut().unwrap().parts[0]
        {
            *args = json!({"task":"changed"});
        }
        let changed = PipelineToolCallLineage::from_call(&original, 0).unwrap();
        assert_ne!(
            static_checkpoint_thread("conversation", &second).unwrap(),
            static_checkpoint_thread("conversation", &changed).unwrap()
        );
        assert!(static_checkpoint_thread(&"x".repeat(512), &first).is_err());
    }

    #[test]
    fn static_thread_receipt_refuses_foreign_batch_arguments_and_thread() {
        let mut original = Event::with_id("original-batch", "original-invocation");
        original.llm_response.content = Some(Content {
            role: "model".to_owned(),
            parts: vec![Part::FunctionCall {
                name: "saved_pipeline".to_owned(),
                args: json!({"task":"work"}),
                id: Some("call-one".to_owned()),
                thought_signature: None,
            }],
        });
        let lineage = PipelineToolCallLineage::from_call(&original, 0).unwrap();
        let thread = static_checkpoint_thread("conversation", &lineage).unwrap();
        let mut event = Event::new("original-child");
        event.provider_metadata.insert(
            STATIC_TOOL_THREAD_METADATA_KEY.to_owned(),
            serde_json::to_string(&StaticToolThreadReceipt {
                schema: STATIC_TOOL_THREAD_METADATA_KEY.to_owned(),
                lineage,
            })
            .unwrap(),
        );
        event
            .provider_metadata
            .insert(DESCENDANT_CHECKPOINT_THREAD_KEY.to_owned(), thread.clone());
        assert_eq!(
            static_thread_for_projected_call(
                &event,
                "conversation",
                "original-batch",
                1,
                "call-one",
                "saved_pipeline",
                &json!({"task":"work"})
            )
            .unwrap(),
            Some(thread)
        );
        assert!(
            static_thread_for_projected_call(
                &event,
                "conversation",
                "foreign-batch",
                1,
                "call-one",
                "saved_pipeline",
                &json!({"task":"work"})
            )
            .is_err()
        );
        assert!(
            static_thread_for_projected_call(
                &event,
                "conversation",
                "original-batch",
                1,
                "call-one",
                "saved_pipeline",
                &json!({"task":"changed"})
            )
            .is_err()
        );
        event.provider_metadata.insert(
            DESCENDANT_CHECKPOINT_THREAD_KEY.to_owned(),
            "conversation/foreign".to_owned(),
        );
        assert!(
            static_thread_for_projected_call(
                &event,
                "conversation",
                "original-batch",
                1,
                "call-one",
                "saved_pipeline",
                &json!({"task":"work"})
            )
            .is_err()
        );
        event
            .provider_metadata
            .insert(STATIC_TOOL_THREAD_METADATA_KEY.to_owned(), "{}".to_owned());
        assert!(
            static_thread_for_projected_call(
                &event,
                "conversation",
                "original-batch",
                1,
                "call-one",
                "saved_pipeline",
                &json!({"task":"work"})
            )
            .is_err()
        );
    }

    fn revision(application_id: u64) -> PipelineCheckpointRevision {
        PipelineCheckpointRevision {
            application_id,
            version_id: 7,
            definition_digest: [0x21; 32],
        }
    }

    fn family() -> PipelineToolPendingFamily {
        let root = Checkpoint::new("call-root", State::new(), 3, vec!["child".to_owned()]);
        let leaf = Checkpoint::new(
            "call-root/child",
            State::new(),
            2,
            vec!["review".to_owned()],
        );
        let completed = Checkpoint::new(
            "call-root/completed",
            [("receipt".to_owned(), json!("one"))].into_iter().collect(),
            8,
            vec![],
        );
        PipelineToolPendingFamily {
            schema_revision: PIPELINE_TOOL_FAMILY_SCHEMA.to_owned(),
            pause_kind: None,
            thread_id: root.thread_id.clone(),
            checkpoint_id: root.checkpoint_id.clone(),
            node_name: "child".to_owned(),
            checkpoint: root,
            catalog: PipelineCheckpointCatalog {
                root: Some(revision(1)),
                descendants: BTreeMap::from([
                    ("child".to_owned(), revision(2)),
                    ("completed".to_owned(), revision(3)),
                ]),
            },
            descendant_checkpoints: vec![leaf, completed],
            call_lineage: None,
        }
    }

    #[tokio::test]
    async fn family_retains_completed_receipt_and_rejects_unadmitted_or_duplicate_threads() {
        let value = PipelinePendingEnvelope::Family(family());
        value.validate().unwrap();
        let raw = serde_json::to_string(&value).unwrap();
        assert!(raw.len() < MAX_PIPELINE_TOOL_PENDING_BYTES);
        let decoded: PipelinePendingEnvelope = serde_json::from_str(&raw).unwrap();
        decoded.validate().unwrap();
        let completed = decoded.checkpoint("call-root/completed").unwrap();
        assert!(completed.pending_nodes.is_empty());
        assert_eq!(completed.state.get("receipt"), Some(&json!("one")));
        let checkpointer = MemoryCheckpointer::new();
        checkpointer.save(completed).await.unwrap();
        assert_eq!(
            checkpointer
                .load("call-root/completed")
                .await
                .unwrap()
                .unwrap()
                .checkpoint_id,
            completed.checkpoint_id
        );
        let mut unadmitted = family();
        unadmitted.descendant_checkpoints[0].thread_id = "call-root/foreign".to_owned();
        assert!(
            PipelinePendingEnvelope::Family(unadmitted)
                .validate()
                .is_err()
        );
        let mut duplicate = family();
        duplicate
            .descendant_checkpoints
            .push(duplicate.descendant_checkpoints[0].clone());
        assert!(
            PipelinePendingEnvelope::Family(duplicate)
                .validate()
                .is_err()
        );
    }

    #[test]
    fn legacy_leaf_pause_remains_readable_without_an_admitted_family() {
        let checkpoint = Checkpoint::new("call-root", State::new(), 2, vec!["review".to_owned()]);
        let value = PipelineToolPending {
            schema_revision: PIPELINE_TOOL_PENDING_SCHEMA.to_owned(),
            thread_id: checkpoint.thread_id.clone(),
            checkpoint_id: checkpoint.checkpoint_id.clone(),
            node_name: "review".to_owned(),
            checkpoint,
        };
        let decoded: PipelinePendingEnvelope =
            serde_json::from_str(&serde_json::to_string(&value).unwrap()).unwrap();
        assert!(matches!(decoded, PipelinePendingEnvelope::Legacy(_)));
        decoded.validate().unwrap();
    }
    #[test]
    fn typed_static_family_does_not_inherit_dynamic_singleton_frontier_rule() {
        let mut value = family();
        value.schema_revision = PIPELINE_TOOL_TYPED_FAMILY_SCHEMA.to_owned();
        value.pause_kind = Some(PipelineToolPauseKind::Static);
        value.call_lineage = Some(PipelineToolCallLineage {
            schema: "elitea.pipeline.tool-call.v1".to_owned(),
            original_batch_event_id: "original-model-batch".to_owned(),
            original_ordinal: 1,
            parent_call_id: "saved-pipeline-call".to_owned(),
            tool_name: "saved-pipeline".to_owned(),
            arguments_digest: crate::agents::pipeline::scope_receipts::arguments_digest(
                &json!({"task":"work"}),
            )
            .unwrap(),
        });
        value.checkpoint.pending_nodes.clear();
        let pending = PipelinePendingEnvelope::Family(value);
        pending.validate().unwrap();
        let raw = serde_json::to_value(&pending).unwrap();
        let decoded: PipelinePendingEnvelope = serde_json::from_value(raw).unwrap();
        decoded.validate().unwrap();
        // Structural decoding is insufficient to resume; resolver checks exact after frontier.
        assert!(decoded.root().pending_nodes.is_empty());
        let mut dynamic = family();
        dynamic.checkpoint.pending_nodes.clear();
        assert!(PipelinePendingEnvelope::Family(dynamic).validate().is_err());
        let mut disguised = family();
        disguised.pause_kind = Some(PipelineToolPauseKind::Static);
        assert!(
            PipelinePendingEnvelope::Family(disguised)
                .validate()
                .is_err()
        );
    }
}

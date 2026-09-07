//! Exact pipeline-HITL decision binding over durable ADK session and graph state.

#![allow(dead_code)] // Production pipeline assembly remains capability-gated.

use std::collections::BTreeSet;
use std::fmt;

use adk_rust::graph::interrupt::INTERRUPT_METADATA_KEY;
use adk_rust::graph::{Checkpointer, State};
use adk_rust::session::Session;
use serde::Deserialize;
use serde_json::{Value, json};

use super::direct_tool::DIRECT_TOOL_RESUME_STATE_KEY;
use super::hitl::HITL_RESUME_STATE_KEY;
use super::llm::LLM_TOOL_RESUME_STATE_KEY;
use super::printer::{
    PRINTER_OUTPUT_STATE_KEY, PRINTER_PAUSE_SCHEMA, PrinterPauseCatalog, PrinterPauseMetadata,
};
use crate::agents::direct_hitl::{
    DirectHitlDecisionSet, DirectHitlError, DirectHitlErrorCode, ResolvedDirectHitlDecision,
    ResolvedDirectHitlStart,
};
use crate::agents::events::{
    PipelineMcpAuthEventBinding, pipeline_application_event_binding,
    pipeline_clarifying_event_binding, pipeline_hitl_event_binding,
    pipeline_mcp_auth_event_binding, pipeline_printer_event_binding, pipeline_tool_event_binding,
};
use crate::agents::request::AgentExecutionPayload;
use crate::toolkits::DelegatedAuthorizationRequirement;

const MAX_COMMENT_BYTES: usize = 8 * 1024;
const MAX_EDIT_BYTES: usize = 64 * 1024;
const MAX_ANSWER_BYTES: usize = 16 * 1_024;
const MAX_IDENTITY_BYTES: usize = 512;

/// Current SDK-compatible continuation of a static Printer checkpoint.
///
/// This is not a HITL decision: any ordinary non-empty user text resumes the
/// exact latest Printer checkpoint, and the text is appended to graph messages
/// before the compiler-owned reset node executes.
pub(crate) struct PrinterContinuation;

#[derive(Clone, Copy)]
enum PipelineMcpAuthorizationAction {
    Authorize,
    Skip,
}

/// Current authorization continuation inferred from Main's claim-fetched
/// token/decline collections. The public request identity has already been
/// consumed by Main; Rust binds the decision to the latest durable graph card,
/// exact server URL and checkpoint before rebuilding the graph.
pub(crate) struct PipelineMcpAuthorizationContinuation {
    action: PipelineMcpAuthorizationAction,
    server_urls: BTreeSet<String>,
}

impl PipelineMcpAuthorizationContinuation {
    pub(crate) fn from_payload(
        payload: &AgentExecutionPayload,
    ) -> Result<Self, PipelineResumeError> {
        if !payload.should_continue
            || payload.hitl_resume
            || payload.hitl_action.is_some()
            || payload.hitl_value.is_some()
            || !payload.hitl_decisions.is_empty()
            || payload.checkpoint_id.is_some()
            || payload.auto_approve_sensitive_actions
        {
            return Err(PipelineResumeError::new(
                PipelineResumeErrorCode::UnsupportedCapability,
            ));
        }
        let (action, server_urls) = if !payload.mcp_tokens.is_empty() {
            (
                PipelineMcpAuthorizationAction::Authorize,
                payload.mcp_tokens.keys().cloned().collect::<BTreeSet<_>>(),
            )
        } else if !payload.user_declined_mcp_servers.is_empty() {
            (
                PipelineMcpAuthorizationAction::Skip,
                payload
                    .user_declined_mcp_servers
                    .iter()
                    .map(declined_server_url)
                    .collect::<Option<BTreeSet<_>>>()
                    .ok_or_else(PipelineResumeError::invalid)?
                    .into_iter()
                    .map(ToOwned::to_owned)
                    .collect(),
            )
        } else {
            return Err(PipelineResumeError::invalid());
        };
        if server_urls.is_empty()
            || server_urls
                .iter()
                .any(|value| !DelegatedAuthorizationRequirement::valid_token_key(value))
        {
            return Err(PipelineResumeError::invalid());
        }
        Ok(Self {
            action,
            server_urls,
        })
    }

    pub(crate) async fn resolve(
        self,
        session: &dyn Session,
        checkpointer: &dyn Checkpointer,
        root_agent_name: &str,
        thread_id: &str,
    ) -> Result<PipelineResume, PipelineResumeError> {
        let events = session.events().all();
        let interrupt_index = events
            .iter()
            .rposition(|event| event.provider_metadata.contains_key(INTERRUPT_METADATA_KEY))
            .ok_or_else(PipelineResumeError::stale)?;
        if interrupt_index + 1 != events.len() {
            return Err(PipelineResumeError::stale());
        }
        let binding =
            pipeline_mcp_auth_event_binding(&events[interrupt_index], root_agent_name, thread_id)
                .map_err(|_| PipelineResumeError::corrupt())?;
        let requirement = delegated_requirement(&binding)?;
        if self
            .server_urls
            .iter()
            .any(|key| !requirement.matches_token_key(key))
        {
            return Err(PipelineResumeError::stale());
        }
        let checkpoint = checkpointer
            .load(thread_id)
            .await
            .map_err(|_| PipelineResumeError::dependency())?
            .ok_or_else(PipelineResumeError::stale)?;
        if checkpoint.thread_id != thread_id
            || checkpoint.checkpoint_id != binding.checkpoint_id()
            || checkpoint.pending_nodes.as_slice() != [binding.pending_node_name()]
        {
            return Err(PipelineResumeError::stale());
        }
        if let Some(replay) = binding.llm_replay().cloned() {
            let predecessor = pipeline_leaf_state_entry(
                checkpointer,
                &checkpoint.state,
                binding.nested_checkpoints(),
                binding.node_name(),
                LLM_TOOL_RESUME_STATE_KEY,
            )
            .await?;
            if !replay
                .matches_checkpoint_predecessor(predecessor.as_ref())
                .map_err(|_| PipelineResumeError::corrupt())?
            {
                return Err(PipelineResumeError::stale());
            }
            let requirement = delegated_requirement(&binding)?;
            let resolved = replay
                .resolve_authorization_decision(
                    binding.tool_call_id(),
                    binding.tool_name(),
                    matches!(self.action, PipelineMcpAuthorizationAction::Authorize),
                    &requirement,
                )
                .map_err(|_| PipelineResumeError::corrupt())?;
            return Ok(PipelineResume {
                state: [(
                    LLM_TOOL_RESUME_STATE_KEY.to_owned(),
                    json!({binding.node_name(): resolved}),
                )]
                .into_iter()
                .collect(),
            });
        }
        if checkpoint
            .state
            .get(DIRECT_TOOL_RESUME_STATE_KEY)
            .is_some_and(|value| value != &json!({}))
        {
            return Err(PipelineResumeError::stale());
        }
        validate_nested_checkpoints(
            checkpointer,
            binding.nested_checkpoints(),
            binding.node_name(),
            DIRECT_TOOL_RESUME_STATE_KEY,
        )
        .await?;
        let action = match self.action {
            PipelineMcpAuthorizationAction::Authorize => "authorize",
            PipelineMcpAuthorizationAction::Skip => "skip",
        };
        Ok(PipelineResume {
            state: [(
                DIRECT_TOOL_RESUME_STATE_KEY.to_owned(),
                json!({
                    binding.node_name(): {
                        "definition_digest": binding.definition_digest(),
                        "tool_call_id": binding.tool_call_id(),
                        "argument_digest": binding.argument_digest(),
                        "action": action,
                    }
                }),
            )]
            .into_iter()
            .collect(),
        })
    }
}

fn delegated_requirement(
    binding: &PipelineMcpAuthEventBinding,
) -> Result<DelegatedAuthorizationRequirement, PipelineResumeError> {
    let requirement = DelegatedAuthorizationRequirement::new(
        binding.toolkit_name().to_owned(),
        binding.toolkit_type().to_owned(),
        binding.server_url().to_owned(),
        binding.resource_metadata_url().map(ToOwned::to_owned),
        binding.www_authenticate().map(ToOwned::to_owned),
    )
    .ok_or_else(PipelineResumeError::corrupt)?;
    match binding.resource_metadata() {
        Some(metadata) => requirement.with_resource_metadata(metadata.clone()),
        None => Some(requirement),
    }
    .ok_or_else(PipelineResumeError::corrupt)
}

fn declined_server_url(value: &Value) -> Option<&str> {
    match value {
        Value::String(value) => Some(value),
        Value::Object(value) => value.get("server_url").and_then(Value::as_str),
        _ => None,
    }
}

pub(crate) struct PrinterResumeContext<'a> {
    session: &'a dyn Session,
    checkpointer: &'a dyn Checkpointer,
    root_agent_name: &'a str,
    thread_id: &'a str,
    catalog: &'a PrinterPauseCatalog,
}

impl<'a> PrinterResumeContext<'a> {
    pub(crate) const fn new(
        session: &'a dyn Session,
        checkpointer: &'a dyn Checkpointer,
        root_agent_name: &'a str,
        thread_id: &'a str,
        catalog: &'a PrinterPauseCatalog,
    ) -> Self {
        Self {
            session,
            checkpointer,
            root_agent_name,
            thread_id,
            catalog,
        }
    }
}

impl PrinterContinuation {
    pub(crate) fn from_payload(
        payload: &AgentExecutionPayload,
    ) -> Result<Self, PipelineResumeError> {
        if !payload.should_continue
            || payload.hitl_resume
            || payload.hitl_action.is_some()
            || payload.hitl_value.is_some()
            || !payload.hitl_decisions.is_empty()
            || payload.checkpoint_id.is_some()
            || payload.auto_approve_sensitive_actions
        {
            return Err(PipelineResumeError::new(
                PipelineResumeErrorCode::UnsupportedCapability,
            ));
        }
        Ok(Self)
    }

    pub(crate) async fn resolve(
        self,
        context: PrinterResumeContext<'_>,
        user_input: &str,
    ) -> Result<PipelineResume, PipelineResumeError> {
        if user_input.is_empty() || user_input.contains('\0') {
            return Err(PipelineResumeError::invalid());
        }
        let events = context.session.events().all();
        let interrupt_index = events
            .iter()
            .rposition(|event| event.provider_metadata.contains_key(INTERRUPT_METADATA_KEY))
            .ok_or_else(PipelineResumeError::stale)?;
        if interrupt_index + 1 != events.len() {
            return Err(PipelineResumeError::stale());
        }
        let binding = pipeline_printer_event_binding(
            &events[interrupt_index],
            context.root_agent_name,
            context.thread_id,
        )
        .map_err(|_| PipelineResumeError::corrupt())?;
        let metadata = PrinterPauseMetadata {
            schema: PRINTER_PAUSE_SCHEMA.to_owned(),
            node_name: binding.node_name().to_owned(),
            reset_node_name: binding.reset_node_name().to_owned(),
            definition_digest: binding.definition_digest().to_owned(),
            node_digest: binding.node_digest().to_owned(),
        };
        if !context.catalog.contains_exact(&metadata) {
            return Err(PipelineResumeError::stale());
        }
        let checkpoint = context
            .checkpointer
            .load(context.thread_id)
            .await
            .map_err(|_| PipelineResumeError::dependency())?
            .ok_or_else(PipelineResumeError::stale)?;
        if checkpoint.thread_id != context.thread_id
            || checkpoint.checkpoint_id != binding.checkpoint_id()
            || checkpoint.pending_nodes.as_slice() != [binding.reset_node_name()]
            || checkpoint
                .state
                .get(PRINTER_OUTPUT_STATE_KEY)
                .and_then(Value::as_str)
                != Some(binding.output())
        {
            return Err(PipelineResumeError::stale());
        }
        Ok(PipelineResume {
            state: [
                ("input".to_owned(), Value::String(user_input.to_owned())),
                (
                    "messages".to_owned(),
                    json!([{"role": "user", "content": user_input}]),
                ),
            ]
            .into_iter()
            .collect(),
        })
    }
}

#[derive(Clone, Copy, Deserialize, Eq, PartialEq)]
#[serde(rename_all = "snake_case")]
enum PipelineHitlAction {
    Approve,
    Reject,
    Edit,
    BlockWithComment,
    Answer,
}

impl PipelineHitlAction {
    const fn wire_name(self) -> &'static str {
        match self {
            Self::Approve => "approve",
            Self::Reject => "reject",
            Self::Edit => "edit",
            Self::BlockWithComment => "block_with_comment",
            Self::Answer => "answer",
        }
    }

    const fn graph_action(self) -> &'static str {
        match self {
            Self::Approve => "approve",
            Self::Reject | Self::BlockWithComment => "reject",
            Self::Edit => "edit",
            Self::Answer => "answer",
        }
    }
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct RawPipelineHitlDecision {
    interrupt_id: String,
    #[serde(default)]
    tool_call_id: String,
    action: PipelineHitlAction,
    #[serde(default)]
    value: String,
}

/// One Main-authorized browser decision before it is joined to durable state.
///
/// It is intentionally non-cloneable and non-debug because an edit or block
/// comment is user content.
pub(crate) struct PipelineHitlDecision {
    interrupt_id: String,
    action: PipelineHitlAction,
    value: String,
}

/// Either configured graph HITL or one direct Toolkit-node confirmation.
pub(crate) enum PipelineContinuationDecision {
    Node(PipelineHitlDecision),
    Sensitive {
        application: DirectHitlDecisionSet,
        tool: Option<PipelineToolDecision>,
    },
}

impl PipelineContinuationDecision {
    pub(crate) fn has_delegated_authorization_actions(&self) -> bool {
        matches!(
            self,
            Self::Sensitive { application, .. }
                if application.has_delegated_authorization_actions()
        )
    }

    pub(crate) fn from_payload(
        payload: &AgentExecutionPayload,
    ) -> Result<Self, PipelineResumeError> {
        let raw = payload
            .hitl_decisions
            .first()
            .and_then(Value::as_object)
            .and_then(|decision| decision.get("tool_call_id"))
            .and_then(Value::as_str)
            .unwrap_or_default();
        if raw.is_empty() {
            PipelineHitlDecision::from_payload(payload).map(Self::Node)
        } else {
            let application = DirectHitlDecisionSet::from_payload(payload)
                .map_err(|error| direct_hitl_resume_error(&error))?;
            // An authorization action belongs to the nested application replay.
            // The direct sensitive-tool parser does not accept authorize/skip.
            let tool = (payload.hitl_decisions.len() == 1
                && !application.has_delegated_authorization_actions())
            .then(|| PipelineToolDecision::from_payload(payload))
            .transpose()?;
            Ok(Self::Sensitive { application, tool })
        }
    }

    pub(crate) async fn resolve(
        self,
        session: &dyn Session,
        checkpointer: &dyn Checkpointer,
        root_agent_name: &str,
        thread_id: &str,
    ) -> Result<ResolvedPipelineContinuation, PipelineResumeError> {
        match self {
            Self::Node(decision) => decision
                .resolve(session, checkpointer, root_agent_name, thread_id)
                .await
                .map(ResolvedPipelineContinuation::graph),
            Self::Sensitive { application, tool } => {
                let events = session.events().all();
                let interrupt = events.last().ok_or_else(PipelineResumeError::stale)?;
                if let Ok(binding) =
                    pipeline_application_event_binding(interrupt, root_agent_name, thread_id)
                {
                    let checkpoint = checkpointer
                        .load(thread_id)
                        .await
                        .map_err(|_| PipelineResumeError::dependency())?
                        .ok_or_else(PipelineResumeError::stale)?;
                    if checkpoint.thread_id != thread_id
                        || checkpoint.checkpoint_id != binding.checkpoint_id()
                        || checkpoint.pending_nodes.as_slice() != [binding.node_name()]
                    {
                        return Err(PipelineResumeError::stale());
                    }
                    let ResolvedDirectHitlStart::Nested(decisions) =
                        application
                            .resolve(session)
                            .map_err(|error| direct_hitl_resume_error(&error))?
                    else {
                        return Err(PipelineResumeError::corrupt());
                    };
                    let submitted = decisions
                        .iter()
                        .map(ResolvedDirectHitlDecision::interrupt_id)
                        .collect::<BTreeSet<_>>();
                    let expected = binding
                        .interrupt_ids()
                        .iter()
                        .map(String::as_str)
                        .collect::<BTreeSet<_>>();
                    if submitted != expected {
                        return Err(PipelineResumeError::stale());
                    }
                    return Ok(ResolvedPipelineContinuation::application(
                        PipelineResume::empty(),
                        decisions,
                    ));
                }
                tool.ok_or_else(PipelineResumeError::corrupt)?
                    .resolve(session, checkpointer, root_agent_name, thread_id)
                    .await
                    .map(ResolvedPipelineContinuation::graph)
            }
        }
    }
}

pub(crate) struct ResolvedPipelineContinuation {
    resume: PipelineResume,
    application_decisions: Option<Vec<ResolvedDirectHitlDecision>>,
}

impl ResolvedPipelineContinuation {
    fn graph(resume: PipelineResume) -> Self {
        Self {
            resume,
            application_decisions: None,
        }
    }

    fn application(
        resume: PipelineResume,
        application_decisions: Vec<ResolvedDirectHitlDecision>,
    ) -> Self {
        Self {
            resume,
            application_decisions: Some(application_decisions),
        }
    }

    pub(crate) fn into_parts(self) -> (PipelineResume, Option<Vec<ResolvedDirectHitlDecision>>) {
        (self.resume, self.application_decisions)
    }
}

/// One exact browser decision for a checkpointed Toolkit-node call.
pub(crate) struct PipelineToolDecision {
    interrupt_id: String,
    tool_call_id: String,
    action: PipelineHitlAction,
    value: String,
}

impl PipelineToolDecision {
    fn from_payload(payload: &AgentExecutionPayload) -> Result<Self, PipelineResumeError> {
        if !payload.should_continue
            || !payload.hitl_resume
            || payload.auto_approve_sensitive_actions
            || payload.hitl_decisions.len() != 1
            || payload.checkpoint_id.is_some()
        {
            return Err(PipelineResumeError::new(
                PipelineResumeErrorCode::UnsupportedCapability,
            ));
        }
        let raw = serde_json::from_value::<RawPipelineHitlDecision>(
            payload
                .hitl_decisions
                .first()
                .cloned()
                .ok_or_else(PipelineResumeError::invalid)?,
        )
        .map_err(|_| PipelineResumeError::invalid())?;
        if !valid_identity(&raw.interrupt_id) || !valid_identity(&raw.tool_call_id) {
            return Err(PipelineResumeError::invalid());
        }
        if payload.hitl_action.as_deref() != Some(raw.action.wire_name())
            || payload.hitl_value.as_deref() != Some(raw.value.as_str())
        {
            return Err(PipelineResumeError::invalid());
        }
        match raw.action {
            PipelineHitlAction::Approve | PipelineHitlAction::Reject if !raw.value.is_empty() => {
                return Err(PipelineResumeError::invalid());
            }
            PipelineHitlAction::BlockWithComment
                if raw.value.is_empty()
                    || raw.value.len() > MAX_COMMENT_BYTES
                    || raw.value.contains('\0') =>
            {
                return Err(PipelineResumeError::invalid());
            }
            PipelineHitlAction::Answer
                if raw.value.is_empty()
                    || raw.value.len() > MAX_ANSWER_BYTES
                    || raw.value.contains('\0') =>
            {
                return Err(PipelineResumeError::invalid());
            }
            PipelineHitlAction::Edit => return Err(PipelineResumeError::invalid()),
            _ => {}
        }
        Ok(Self {
            interrupt_id: raw.interrupt_id,
            tool_call_id: raw.tool_call_id,
            action: raw.action,
            value: raw.value,
        })
    }

    #[allow(clippy::too_many_lines)] // One linear validation chain binds one persisted interrupt.
    async fn resolve(
        self,
        session: &dyn Session,
        checkpointer: &dyn Checkpointer,
        root_agent_name: &str,
        thread_id: &str,
    ) -> Result<PipelineResume, PipelineResumeError> {
        let events = session.events().all();
        let interrupt_index = events
            .iter()
            .rposition(|event| event.provider_metadata.contains_key(INTERRUPT_METADATA_KEY))
            .ok_or_else(PipelineResumeError::stale)?;
        if interrupt_index + 1 != events.len() {
            return Err(PipelineResumeError::stale());
        }
        if self.action == PipelineHitlAction::Answer {
            let binding = pipeline_clarifying_event_binding(
                &events[interrupt_index],
                root_agent_name,
                thread_id,
            )
            .map_err(|_| PipelineResumeError::corrupt())?;
            if binding.interrupt_id() != self.interrupt_id
                || binding.tool_call_id() != self.tool_call_id
            {
                return Err(PipelineResumeError::stale());
            }
            let checkpoint = checkpointer
                .load(thread_id)
                .await
                .map_err(|_| PipelineResumeError::dependency())?
                .ok_or_else(PipelineResumeError::stale)?;
            if checkpoint.thread_id != thread_id
                || checkpoint.checkpoint_id != binding.checkpoint_id()
                || checkpoint.pending_nodes.as_slice() != [binding.pending_node_name()]
            {
                return Err(PipelineResumeError::stale());
            }
            let predecessor = pipeline_leaf_state_entry(
                checkpointer,
                &checkpoint.state,
                binding.nested_checkpoints(),
                binding.node_name(),
                LLM_TOOL_RESUME_STATE_KEY,
            )
            .await?;
            if !binding
                .llm_replay()
                .matches_checkpoint_predecessor(predecessor.as_ref())
                .map_err(|_| PipelineResumeError::corrupt())?
            {
                return Err(PipelineResumeError::stale());
            }
            let resolved = binding
                .llm_replay()
                .clone()
                .resolve_clarifying_answer(binding.tool_call_id(), binding.tool_name(), &self.value)
                .map_err(|_| PipelineResumeError::corrupt())?;
            return Ok(PipelineResume {
                state: [(
                    LLM_TOOL_RESUME_STATE_KEY.to_owned(),
                    json!({binding.node_name(): resolved}),
                )]
                .into_iter()
                .collect(),
            });
        }
        let binding =
            pipeline_tool_event_binding(&events[interrupt_index], root_agent_name, thread_id)
                .map_err(|_| PipelineResumeError::corrupt())?;
        if binding.interrupt_id() != self.interrupt_id
            || binding.tool_call_id() != self.tool_call_id
        {
            return Err(PipelineResumeError::stale());
        }
        let checkpoint = checkpointer
            .load(thread_id)
            .await
            .map_err(|_| PipelineResumeError::dependency())?
            .ok_or_else(PipelineResumeError::stale)?;
        if checkpoint.thread_id != thread_id
            || checkpoint.checkpoint_id != binding.checkpoint_id()
            || checkpoint.pending_nodes.as_slice() != [binding.pending_node_name()]
        {
            return Err(PipelineResumeError::stale());
        }
        if let Some(replay) = binding.llm_replay().cloned() {
            let predecessor = pipeline_leaf_state_entry(
                checkpointer,
                &checkpoint.state,
                binding.nested_checkpoints(),
                binding.node_name(),
                LLM_TOOL_RESUME_STATE_KEY,
            )
            .await?;
            if !replay
                .matches_checkpoint_predecessor(predecessor.as_ref())
                .map_err(|_| PipelineResumeError::corrupt())?
            {
                return Err(PipelineResumeError::stale());
            }
            let approve = self.action == PipelineHitlAction::Approve;
            let denial_comment = (self.action == PipelineHitlAction::BlockWithComment)
                .then_some(self.value.as_str());
            let resolved = replay
                .resolve_decision(
                    binding.tool_call_id(),
                    binding.tool_name(),
                    approve,
                    denial_comment,
                    (
                        binding.toolkit_name(),
                        binding.toolkit_type(),
                        binding.action_label(),
                    ),
                )
                .map_err(|_| PipelineResumeError::corrupt())?;
            return Ok(PipelineResume {
                state: [(
                    LLM_TOOL_RESUME_STATE_KEY.to_owned(),
                    json!({binding.node_name(): resolved}),
                )]
                .into_iter()
                .collect(),
            });
        }
        if checkpoint
            .state
            .get(DIRECT_TOOL_RESUME_STATE_KEY)
            .is_some_and(|value| value != &json!({}))
        {
            return Err(PipelineResumeError::stale());
        }
        validate_nested_checkpoints(
            checkpointer,
            binding.nested_checkpoints(),
            binding.node_name(),
            DIRECT_TOOL_RESUME_STATE_KEY,
        )
        .await?;
        Ok(PipelineResume {
            state: [(
                DIRECT_TOOL_RESUME_STATE_KEY.to_owned(),
                json!({
                    binding.node_name(): {
                        "definition_digest": binding.definition_digest(),
                        "tool_call_id": binding.tool_call_id(),
                        "argument_digest": binding.argument_digest(),
                        "action": self.action.wire_name(),
                        "value": self.value,
                    }
                }),
            )]
            .into_iter()
            .collect(),
        })
    }
}

async fn pipeline_leaf_state_entry(
    checkpointer: &dyn Checkpointer,
    root_state: &State,
    nested: &[crate::agents::events::NestedPipelineCheckpoint],
    leaf_pending_node: &str,
    state_key: &str,
) -> Result<Option<Value>, PipelineResumeError> {
    let mut leaf_state = root_state.clone();
    for (index, nested_checkpoint) in nested.iter().enumerate() {
        let pending_node = nested
            .get(index + 1)
            .map_or(leaf_pending_node, |checkpoint| checkpoint.node_name());
        let checkpoint = checkpointer
            .load(nested_checkpoint.thread_id())
            .await
            .map_err(|_| PipelineResumeError::dependency())?
            .ok_or_else(PipelineResumeError::stale)?;
        if checkpoint.thread_id != nested_checkpoint.thread_id()
            || checkpoint.checkpoint_id != nested_checkpoint.checkpoint_id()
            || checkpoint.pending_nodes.as_slice() != [pending_node]
        {
            return Err(PipelineResumeError::stale());
        }
        leaf_state = checkpoint.state;
    }
    Ok(leaf_state
        .get(state_key)
        .and_then(Value::as_object)
        .and_then(|values| values.get(leaf_pending_node))
        .cloned())
}

impl PipelineHitlDecision {
    /// Admit the current single pipeline-HITL continuation envelope.
    pub(crate) fn from_payload(
        payload: &AgentExecutionPayload,
    ) -> Result<Self, PipelineResumeError> {
        if !payload.should_continue
            || !payload.hitl_resume
            || payload.auto_approve_sensitive_actions
            || payload.hitl_decisions.len() != 1
            || payload.checkpoint_id.is_some()
        {
            return Err(PipelineResumeError::new(
                PipelineResumeErrorCode::UnsupportedCapability,
            ));
        }
        let raw = serde_json::from_value::<RawPipelineHitlDecision>(
            payload
                .hitl_decisions
                .first()
                .cloned()
                .ok_or_else(PipelineResumeError::invalid)?,
        )
        .map_err(|_| PipelineResumeError::invalid())?;
        if !valid_identity(&raw.interrupt_id) || !raw.tool_call_id.is_empty() {
            return Err(PipelineResumeError::invalid());
        }
        if payload.hitl_action.as_deref() != Some(raw.action.wire_name())
            || payload.hitl_value.as_deref() != Some(raw.value.as_str())
        {
            return Err(PipelineResumeError::invalid());
        }
        match raw.action {
            PipelineHitlAction::Approve | PipelineHitlAction::Reject if !raw.value.is_empty() => {
                return Err(PipelineResumeError::invalid());
            }
            PipelineHitlAction::Edit
                if raw.value.is_empty()
                    || raw.value.len() > MAX_EDIT_BYTES
                    || raw.value.contains('\0') =>
            {
                return Err(PipelineResumeError::invalid());
            }
            PipelineHitlAction::BlockWithComment
                if raw.value.is_empty()
                    || raw.value.len() > MAX_COMMENT_BYTES
                    || raw.value.contains('\0') =>
            {
                return Err(PipelineResumeError::invalid());
            }
            _ => {}
        }
        Ok(Self {
            interrupt_id: raw.interrupt_id,
            action: raw.action,
            value: raw.value,
        })
    }

    /// Resolve this decision against the latest persisted pause and checkpoint.
    pub(crate) async fn resolve(
        self,
        session: &dyn Session,
        checkpointer: &dyn Checkpointer,
        root_agent_name: &str,
        thread_id: &str,
    ) -> Result<PipelineResume, PipelineResumeError> {
        let events = session.events().all();
        let interrupt_index = events
            .iter()
            .rposition(|event| event.provider_metadata.contains_key(INTERRUPT_METADATA_KEY))
            .ok_or_else(PipelineResumeError::stale)?;
        if interrupt_index + 1 != events.len() {
            return Err(PipelineResumeError::stale());
        }
        let binding =
            pipeline_hitl_event_binding(&events[interrupt_index], root_agent_name, thread_id)
                .map_err(|_| PipelineResumeError::corrupt())?;
        if binding.interrupt_id() != self.interrupt_id
            || !binding.allows(self.action.graph_action())
        {
            return Err(PipelineResumeError::stale());
        }
        let checkpoint = checkpointer
            .load(thread_id)
            .await
            .map_err(|_| PipelineResumeError::dependency())?
            .ok_or_else(PipelineResumeError::stale)?;
        if checkpoint.thread_id != thread_id
            || checkpoint.checkpoint_id != binding.checkpoint_id()
            || checkpoint.pending_nodes.as_slice() != [binding.pending_node_name()]
            || checkpoint
                .state
                .get(HITL_RESUME_STATE_KEY)
                .is_some_and(|value| value != &json!({}))
        {
            return Err(PipelineResumeError::stale());
        }
        validate_nested_checkpoints(
            checkpointer,
            binding.nested_checkpoints(),
            binding.node_name(),
            HITL_RESUME_STATE_KEY,
        )
        .await?;
        let value = if self.action == PipelineHitlAction::Edit {
            Value::String(self.value)
        } else {
            Value::String(String::new())
        };
        Ok(PipelineResume {
            state: [(
                HITL_RESUME_STATE_KEY.to_owned(),
                json!({
                    binding.node_name(): {
                        "definition_digest": binding.definition_digest(),
                        "action": self.action.graph_action(),
                        "value": value,
                    }
                }),
            )]
            .into_iter()
            .collect(),
        })
    }
}

/// One checkpoint-proven resume state consumed by a fresh graph agent.
pub(crate) struct PipelineResume {
    state: State,
}

impl PipelineResume {
    fn empty() -> Self {
        Self {
            state: State::new(),
        }
    }

    pub(super) fn into_state(self) -> State {
        self.state
    }
}

fn direct_hitl_resume_error(error: &DirectHitlError) -> PipelineResumeError {
    match error.code() {
        DirectHitlErrorCode::InvalidInput | DirectHitlErrorCode::ResourceExhausted => {
            PipelineResumeError::invalid()
        }
        DirectHitlErrorCode::UnsupportedCapability => {
            PipelineResumeError::new(PipelineResumeErrorCode::UnsupportedCapability)
        }
        DirectHitlErrorCode::StaleDecision => PipelineResumeError::stale(),
        DirectHitlErrorCode::CorruptSession => PipelineResumeError::corrupt(),
    }
}

async fn validate_nested_checkpoints(
    checkpointer: &dyn Checkpointer,
    nested: &[crate::agents::events::NestedPipelineCheckpoint],
    leaf_pending_node: &str,
    resume_state_key: &str,
) -> Result<(), PipelineResumeError> {
    for (index, nested_checkpoint) in nested.iter().enumerate() {
        let pending_node = nested
            .get(index + 1)
            .map_or(leaf_pending_node, |checkpoint| checkpoint.node_name());
        let checkpoint = checkpointer
            .load(nested_checkpoint.thread_id())
            .await
            .map_err(|_| PipelineResumeError::dependency())?
            .ok_or_else(PipelineResumeError::stale)?;
        if checkpoint.thread_id != nested_checkpoint.thread_id()
            || checkpoint.checkpoint_id != nested_checkpoint.checkpoint_id()
            || checkpoint.pending_nodes.as_slice() != [pending_node]
            || checkpoint
                .state
                .get(resume_state_key)
                .is_some_and(|value| value != &json!({}))
        {
            return Err(PipelineResumeError::stale());
        }
    }
    Ok(())
}

fn valid_identity(value: &str) -> bool {
    !value.is_empty() && value.len() <= MAX_IDENTITY_BYTES && !value.chars().any(char::is_control)
}

/// Stable pipeline-HITL restart failure.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum PipelineResumeErrorCode {
    InvalidInput,
    UnsupportedCapability,
    StaleDecision,
    CorruptSession,
    DependencyUnavailable,
}

impl PipelineResumeErrorCode {
    #[must_use]
    pub(crate) const fn as_str(self) -> &'static str {
        match self {
            Self::InvalidInput => "pipeline_hitl.invalid_input",
            Self::UnsupportedCapability => "pipeline_hitl.unsupported_capability",
            Self::StaleDecision => "pipeline_hitl.stale_decision",
            Self::CorruptSession => "pipeline_hitl.corrupt_session",
            Self::DependencyUnavailable => "pipeline_hitl.dependency_unavailable",
        }
    }
}

/// Data-free public error; the source event and decision are never formatted.
pub(crate) struct PipelineResumeError {
    code: PipelineResumeErrorCode,
}

impl PipelineResumeError {
    const fn new(code: PipelineResumeErrorCode) -> Self {
        Self { code }
    }

    const fn invalid() -> Self {
        Self::new(PipelineResumeErrorCode::InvalidInput)
    }

    const fn stale() -> Self {
        Self::new(PipelineResumeErrorCode::StaleDecision)
    }

    const fn corrupt() -> Self {
        Self::new(PipelineResumeErrorCode::CorruptSession)
    }

    const fn dependency() -> Self {
        Self::new(PipelineResumeErrorCode::DependencyUnavailable)
    }

    #[must_use]
    pub(crate) const fn code(&self) -> PipelineResumeErrorCode {
        self.code
    }
}

impl fmt::Debug for PipelineResumeError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("PipelineResumeError")
            .field("code", &self.code)
            .finish_non_exhaustive()
    }
}

impl fmt::Display for PipelineResumeError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str("the pipeline HITL decision could not be resolved")
    }
}

impl std::error::Error for PipelineResumeError {}

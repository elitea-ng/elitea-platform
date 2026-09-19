//! Exact direct-tool HITL decision binding over durable ADK session events.
//!
//! Main already consumes one browser decision set atomically from the paused
//! response and materializes it into the claimed continuation input. This
//! module performs the worker-side half of that boundary: it admits the
//! bounded current decision shape and proves that it names the latest
//! unresolved [`ToolConfirmationRequest`](adk_rust::ToolConfirmationRequest)
//! persisted by ADK's [`SessionService`](adk_rust::session::SessionService).
//!
//! ADK-Rust 2.0.0's direct `LlmAgent` confirmation event does not preserve a
//! restart-safe suspended execution frame. This module therefore reconstructs
//! the exact call from durable session events: an approved read may execute
//! once, while a denied call is replaced by a local adapter that emits the
//! structured blocked result under the original call ID. Approved effects
//! remain closed until they have an owned durable effect receipt.

#![allow(dead_code)] // Resume execution remains capability-gated.

use std::collections::{BTreeSet, HashSet};
use std::fmt;
use std::sync::Arc;
use std::sync::atomic::{AtomicU8, Ordering};

use adk_rust::futures::stream;
use adk_rust::{
    AdkError, Content, Event, FinishReason, Llm, LlmRequest, LlmResponse, LlmResponseStream, Part,
    ReadonlyContext, RunConfig, SchemaAdapter, Tool, ToolConfirmationDecision, ToolContext,
    Toolset, tool_call_fingerprint,
};
use async_trait::async_trait;
use base64::Engine as _;
use base64::engine::general_purpose::URL_SAFE_NO_PAD;
use ring::digest;
use serde::Deserialize;
use serde_json::Value;

use super::events::{DESCENDANT_CONTAINER_INVOCATION_KEY, DESCENDANT_PARENT_CALL_KEY};
use super::internal_tools::{ASK_USER_METADATA_KEY, AskUserRequest, decode_ask_user_request};
use super::request::AgentExecutionPayload;
use super::sensitive_tools::SensitiveToolCatalog;
use crate::toolkits::{
    DELEGATED_AUTHORIZATION_METADATA_KEY, DelegatedAuthorizationCatalog,
    DelegatedAuthorizationRequirement, decode_delegated_authorization_requirement,
    delegated_authorization_declined_result,
};

const MAX_IDENTITY_BYTES: usize = 512;
const MAX_COMMENT_BYTES: usize = 2_000;
const MAX_ANSWER_BYTES: usize = 16 * 1_024;
const MAX_CALL_VALUE_BYTES: usize = 40 * 1_024;
const MAX_JSON_DEPTH: usize = 64;
const MAX_DIRECT_HITL_DECISIONS: usize = 16;
const HITL_DIGEST_DOMAIN: &[u8] = b"elitea.sensitive-tool-interrupt.v1\0";
const BLOCKED_TOOL_RESULT_TYPE: &str = "sensitive_tool_blocked";
const BLOCKED_TOOL_DEFAULT_REASON: &str = "denied by user";
/// Upper bound on the calls of one replayed assistant message, aligned with
/// `events::MAX_TOOL_CALLS_PER_MODEL_TURN` so a message the projector admits is
/// a message the replay can re-emit.
const MAX_REPLAY_CALLS: usize = 16;
const REPLAY_MARKER_PREFIX: &str = "[Elitea direct HITL ";
const REPLAY_APPROVED_TEXT: &str =
    "The pending tool call was approved. Continue the original request.";
const REPLAY_REJECTED_TEXT: &str =
    "The pending tool call was rejected. Continue without executing it.";
const REPLAY_COMMENT_INFIX: &str = " Reviewer comment: ";

/// Stable direct-HITL admission and resolution failures.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum DirectHitlErrorCode {
    InvalidInput,
    UnsupportedCapability,
    StaleDecision,
    CorruptSession,
    ResourceExhausted,
}

impl DirectHitlErrorCode {
    #[must_use]
    pub(crate) const fn as_str(self) -> &'static str {
        match self {
            Self::InvalidInput => "direct_hitl.invalid_input",
            Self::UnsupportedCapability => "direct_hitl.unsupported_capability",
            Self::StaleDecision => "direct_hitl.stale_decision",
            Self::CorruptSession => "direct_hitl.corrupt_session",
            Self::ResourceExhausted => "direct_hitl.resource_exhausted",
        }
    }
}

/// Data-free direct-HITL error.
pub(crate) struct DirectHitlError {
    code: DirectHitlErrorCode,
}

impl DirectHitlError {
    pub(crate) const fn new(code: DirectHitlErrorCode) -> Self {
        Self { code }
    }

    #[must_use]
    pub(crate) const fn code(&self) -> DirectHitlErrorCode {
        self.code
    }
}

impl fmt::Debug for DirectHitlError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("DirectHitlError")
            .field("code", &self.code)
            .finish_non_exhaustive()
    }
}

impl fmt::Display for DirectHitlError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str("the direct sensitive-tool decision could not be resolved")
    }
}

impl std::error::Error for DirectHitlError {}

#[derive(Clone, Copy, Deserialize)]
#[serde(rename_all = "snake_case")]
enum DirectHitlAction {
    Approve,
    Reject,
    BlockWithComment,
    Authorize,
    Skip,
    Answer,
}

#[derive(Clone, Copy, Deserialize, Eq, PartialEq)]
#[serde(rename_all = "snake_case")]
enum DirectGuardrailType {
    SensitiveTool,
    McpAuth,
    ClarifyingQuestion,
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct RawDirectHitlDecision {
    interrupt_id: String,
    #[serde(default)]
    tool_call_id: String,
    #[serde(default)]
    guardrail_type: Option<DirectGuardrailType>,
    action: DirectHitlAction,
    #[serde(default)]
    value: String,
}

/// One decision admitted from Main's already-authorized continuation input.
///
/// This value is intentionally non-`Clone` and non-`Debug`: the optional
/// denial comment is user content and should not become diagnostic payload.
pub(crate) struct DirectHitlDecision {
    interrupt_id: String,
    tool_call_id: Option<String>,
    guardrail_type: Option<DirectGuardrailType>,
    action: DirectHitlAction,
    value: Option<String>,
}

/// One atomic Main-authorized decision set.
///
/// Main persists and consumes every visible card in one transaction. Keeping
/// the same bound here prevents the worker from silently resuming only a
/// subset of a parallel pause.
pub(crate) struct DirectHitlDecisionSet {
    decisions: Vec<DirectHitlDecision>,
    authorization: DelegatedAuthorizationAuthority,
}

#[derive(Default)]
struct DelegatedAuthorizationAuthority {
    authorized_servers: BTreeSet<String>,
    declined_servers: BTreeSet<String>,
}

#[derive(Clone, Copy)]
enum DirectDelegatedAuthorizationAction {
    Authorize,
    Skip,
}

/// Claim-fetched root-agent authorization continuation. Main's current wire
/// does not carry the browser card identity, so this binds only one exact
/// server to the latest unadvanced authorization confirmation in the durable
/// ADK session and fails closed for an ambiguous set.
pub(crate) struct DirectDelegatedAuthorizationContinuation {
    action: DirectDelegatedAuthorizationAction,
    server_url: String,
}

impl DirectDelegatedAuthorizationContinuation {
    pub(crate) fn from_payload(payload: &AgentExecutionPayload) -> Result<Self, DirectHitlError> {
        if !payload.should_continue
            || payload.hitl_resume
            || payload.hitl_action.is_some()
            || payload.hitl_value.is_some()
            || !payload.hitl_decisions.is_empty()
            || payload.checkpoint_id.is_some()
            || payload.auto_approve_sensitive_actions
            || !payload.ignored_mcp_servers.is_empty()
        {
            return Err(DirectHitlError::new(
                DirectHitlErrorCode::UnsupportedCapability,
            ));
        }
        let (action, server_urls) = if !payload.mcp_tokens.is_empty()
            && payload.user_declined_mcp_servers.is_empty()
        {
            (
                DirectDelegatedAuthorizationAction::Authorize,
                payload.mcp_tokens.keys().cloned().collect::<BTreeSet<_>>(),
            )
        } else if payload.mcp_tokens.is_empty() && !payload.user_declined_mcp_servers.is_empty() {
            let urls = payload
                .user_declined_mcp_servers
                .iter()
                .map(declined_server_url)
                .collect::<Option<BTreeSet<_>>>()
                .ok_or_else(|| DirectHitlError::new(DirectHitlErrorCode::InvalidInput))?;
            (
                DirectDelegatedAuthorizationAction::Skip,
                urls.into_iter().map(ToOwned::to_owned).collect(),
            )
        } else {
            return Err(DirectHitlError::new(DirectHitlErrorCode::InvalidInput));
        };
        if server_urls.len() != 1 || server_urls.iter().any(|url| !valid_server_url(url)) {
            return Err(DirectHitlError::new(
                DirectHitlErrorCode::UnsupportedCapability,
            ));
        }
        Ok(Self {
            action,
            server_url: server_urls
                .into_iter()
                .next()
                .ok_or_else(|| DirectHitlError::new(DirectHitlErrorCode::InvalidInput))?,
        })
    }

    pub(crate) fn resolve(
        self,
        session: &dyn adk_rust::session::Session,
    ) -> Result<ResolvedDirectHitlDecision, DirectHitlError> {
        let events = session.events().all();
        let confirmation_index = events
            .iter()
            .rposition(|event| {
                event.actions.tool_confirmation.is_some()
                    && event
                        .provider_metadata
                        .contains_key(DELEGATED_AUTHORIZATION_METADATA_KEY)
            })
            .ok_or_else(|| DirectHitlError::new(DirectHitlErrorCode::StaleDecision))?;
        let confirmation_event = &events[confirmation_index];
        if application_route(confirmation_event)?.is_some() {
            return Err(DirectHitlError::new(
                DirectHitlErrorCode::UnsupportedCapability,
            ));
        }
        let requirement = confirmation_event
            .provider_metadata
            .get(DELEGATED_AUTHORIZATION_METADATA_KEY)
            .and_then(|value| decode_delegated_authorization_requirement(value))
            .filter(|requirement| requirement.server_url() == self.server_url)
            .ok_or_else(|| DirectHitlError::new(DirectHitlErrorCode::StaleDecision))?;
        let request = confirmation_event
            .actions
            .tool_confirmation
            .as_ref()
            .ok_or_else(|| DirectHitlError::new(DirectHitlErrorCode::CorruptSession))?;
        let call_id = request
            .function_call_id
            .as_deref()
            .filter(|value| valid_identity(value))
            .ok_or_else(|| DirectHitlError::new(DirectHitlErrorCode::CorruptSession))?;
        if !valid_identity(&request.tool_name)
            || encoded_value_len(&request.args)? > MAX_CALL_VALUE_BYTES
        {
            return Err(DirectHitlError::new(DirectHitlErrorCode::CorruptSession));
        }
        let matching_calls = events[..confirmation_index]
            .iter()
            .filter(|event| event.invocation_id == confirmation_event.invocation_id)
            .flat_map(Event::tool_calls)
            .filter(|call| {
                call.call_id == Some(call_id)
                    && call.name == request.tool_name
                    && call.args == &request.args
            })
            .count();
        if matching_calls != 1 {
            return Err(DirectHitlError::new(DirectHitlErrorCode::CorruptSession));
        }
        let (interrupt_id, call_digest) = sensitive_call_identity(
            &confirmation_event.invocation_id,
            call_id,
            &request.tool_name,
            &request.args,
        )?;
        let decision = match self.action {
            DirectDelegatedAuthorizationAction::Authorize => ToolConfirmationDecision::Approve,
            DirectDelegatedAuthorizationAction::Skip => ToolConfirmationDecision::Deny,
        };
        let user_content = replay_user_content(&interrupt_id, decision, None);
        // The delegated-authorization pause is raised by an MCP server's own
        // requirement rather than by the guardrails policy, and is deliberately
        // kept to the one call it names.
        let replay_calls = vec![ReplayCall {
            call_id: call_id.to_owned(),
            tool_name: request.tool_name.clone(),
            arguments: request.args.clone(),
            settled: None,
        }];
        let persisted = persisted_replay_state_with_confirmation(
            &events[confirmation_index + 1..],
            &user_content,
            call_id,
            &request.tool_name,
            &request.args,
            match self.action {
                DirectDelegatedAuthorizationAction::Authorize => None,
                DirectDelegatedAuthorizationAction::Skip => Some(ToolConfirmationDecision::Deny),
            },
            &replay_calls,
        )?;
        Ok(ResolvedDirectHitlDecision {
            invocation_id: confirmation_event.invocation_id.clone(),
            interrupt_id,
            call_digest,
            call_id: call_id.to_owned(),
            tool_name: request.tool_name.clone(),
            arguments: request.args.clone(),
            fingerprint: tool_call_fingerprint(&request.tool_name, &request.args),
            decision,
            decision_value: None,
            user_content,
            resume_mode: persisted.mode,
            persisted_result: persisted.result,
            replay_calls,
            application_route: None,
            delegated_authorization: Some(requirement),
            clarifying_question: None,
        })
    }
}

fn declined_server_url(value: &Value) -> Option<&str> {
    match value {
        Value::String(value) => Some(value),
        Value::Object(value) => value.get("server_url").and_then(Value::as_str),
        _ => None,
    }
}

fn valid_server_url(value: &str) -> bool {
    reqwest::Url::parse(value).is_ok_and(|url| {
        url.scheme() == "https"
            && url.host_str().is_some()
            && url.username().is_empty()
            && url.password().is_none()
            && url.query().is_none()
            && url.fragment().is_none()
    })
}

impl DirectHitlDecisionSet {
    pub(crate) fn single(decision: DirectHitlDecision) -> Self {
        Self {
            decisions: vec![decision],
            authorization: DelegatedAuthorizationAuthority::default(),
        }
    }

    /// Select the pipeline shell that may carry claim-fetched MCP authority.
    ///
    /// This is not authorization: `resolve` still binds every action and exact
    /// server set to the persisted confirmation before any tool dispatch.
    pub(crate) fn has_delegated_authorization_actions(&self) -> bool {
        self.decisions.iter().any(|decision| {
            matches!(
                decision.action,
                DirectHitlAction::Authorize | DirectHitlAction::Skip
            )
        })
    }

    pub(crate) fn from_payload(payload: &AgentExecutionPayload) -> Result<Self, DirectHitlError> {
        if !payload.should_continue
            || !payload.hitl_resume
            || payload.auto_approve_sensitive_actions
            || payload.hitl_decisions.is_empty()
            || payload.hitl_decisions.len() > MAX_DIRECT_HITL_DECISIONS
            || !payload.ignored_mcp_servers.is_empty()
        {
            return Err(DirectHitlError::new(
                DirectHitlErrorCode::UnsupportedCapability,
            ));
        }
        let multiple = payload.hitl_decisions.len() > 1;
        if multiple && (payload.hitl_action.is_some() || payload.hitl_value.is_some()) {
            return Err(DirectHitlError::new(DirectHitlErrorCode::InvalidInput));
        }
        let mut interrupt_ids = HashSet::with_capacity(payload.hitl_decisions.len());
        let mut decisions = Vec::with_capacity(payload.hitl_decisions.len());
        for value in &payload.hitl_decisions {
            let raw = serde_json::from_value::<RawDirectHitlDecision>(value.clone())
                .map_err(|_| DirectHitlError::new(DirectHitlErrorCode::InvalidInput))?;
            let decision = DirectHitlDecision::from_raw(raw)?;
            if !interrupt_ids.insert(decision.interrupt_id.clone()) {
                return Err(DirectHitlError::new(DirectHitlErrorCode::InvalidInput));
            }
            decisions.push(decision);
        }
        if !multiple {
            let decision = decisions
                .first()
                .ok_or_else(|| DirectHitlError::new(DirectHitlErrorCode::InvalidInput))?;
            if payload.hitl_action.as_deref() != Some(decision.action.as_str())
                || payload.hitl_value.as_deref() != Some(decision.raw_value())
            {
                return Err(DirectHitlError::new(DirectHitlErrorCode::InvalidInput));
            }
        }
        let authorization = DelegatedAuthorizationAuthority::from_payload(payload)?;
        Ok(Self {
            decisions,
            authorization,
        })
    }

    pub(crate) fn into_single(mut self) -> Result<DirectHitlDecision, DirectHitlError> {
        if self.decisions.len() != 1
            || !self.authorization.is_empty()
            || self.decisions.first().is_some_and(|decision| {
                matches!(
                    decision.action,
                    DirectHitlAction::Authorize | DirectHitlAction::Skip
                )
            })
        {
            return Err(DirectHitlError::new(
                DirectHitlErrorCode::UnsupportedCapability,
            ));
        }
        self.decisions
            .pop()
            .ok_or_else(|| DirectHitlError::new(DirectHitlErrorCode::InvalidInput))
    }

    pub(crate) fn resolve(
        self,
        session: &dyn adk_rust::session::Session,
    ) -> Result<ResolvedDirectHitlStart, DirectHitlError> {
        let events = session.events().all();
        let mut resolved = Vec::with_capacity(self.decisions.len());
        for decision in self.decisions {
            let index = matching_confirmation_index(&events, &decision.interrupt_id)?;
            let nested = application_route(&events[index])?.is_some();
            resolved.push(decision.resolve_at(&events, index, nested)?);
        }
        self.authorization.validate_resolved(&resolved)?;
        let nested_count = resolved
            .iter()
            .filter(|decision| decision.application_route.is_some())
            .count();
        if nested_count == 0 {
            if resolved.len() != 1 {
                return Err(DirectHitlError::new(
                    DirectHitlErrorCode::UnsupportedCapability,
                ));
            }
            return resolved
                .pop()
                .map(Box::new)
                .map(ResolvedDirectHitlStart::Direct)
                .ok_or_else(|| DirectHitlError::new(DirectHitlErrorCode::InvalidInput));
        }
        if nested_count != resolved.len() {
            return Err(DirectHitlError::new(
                DirectHitlErrorCode::UnsupportedCapability,
            ));
        }
        Ok(ResolvedDirectHitlStart::Nested(resolved))
    }
}

impl DelegatedAuthorizationAuthority {
    fn is_empty(&self) -> bool {
        self.authorized_servers.is_empty() && self.declined_servers.is_empty()
    }

    fn from_payload(payload: &AgentExecutionPayload) -> Result<Self, DirectHitlError> {
        let authorized_servers = payload
            .mcp_tokens
            .keys()
            .map(|server| {
                valid_server_url(server)
                    .then(|| server.to_owned())
                    .ok_or_else(|| DirectHitlError::new(DirectHitlErrorCode::InvalidInput))
            })
            .collect::<Result<BTreeSet<_>, _>>()?;
        let declined_servers = payload
            .user_declined_mcp_servers
            .iter()
            .map(|value| {
                declined_server_url(value)
                    .filter(|server| valid_server_url(server))
                    .map(ToOwned::to_owned)
                    .ok_or_else(|| DirectHitlError::new(DirectHitlErrorCode::InvalidInput))
            })
            .collect::<Result<BTreeSet<_>, _>>()?;
        if !authorized_servers.is_disjoint(&declined_servers) {
            return Err(DirectHitlError::new(DirectHitlErrorCode::InvalidInput));
        }
        Ok(Self {
            authorized_servers,
            declined_servers,
        })
    }

    fn validate_resolved(
        self,
        decisions: &[ResolvedDirectHitlDecision],
    ) -> Result<(), DirectHitlError> {
        let mut authorized = BTreeSet::new();
        let mut declined = BTreeSet::new();
        for decision in decisions {
            let Some(requirement) = decision.delegated_authorization.as_ref() else {
                continue;
            };
            let target = if decision.decision == ToolConfirmationDecision::Approve {
                &mut authorized
            } else {
                &mut declined
            };
            target.insert(requirement.server_url().to_owned());
        }
        if authorized != self.authorized_servers || declined != self.declined_servers {
            return Err(DirectHitlError::new(DirectHitlErrorCode::StaleDecision));
        }
        Ok(())
    }
}

impl DirectHitlDecision {
    /// Admit the current direct-sensitive-tool continuation shape.
    pub(crate) fn from_payload(payload: &AgentExecutionPayload) -> Result<Self, DirectHitlError> {
        DirectHitlDecisionSet::from_payload(payload)?.into_single()
    }

    fn from_raw(raw: RawDirectHitlDecision) -> Result<Self, DirectHitlError> {
        if !valid_identity(&raw.interrupt_id)
            || (!raw.tool_call_id.is_empty() && !valid_identity(&raw.tool_call_id))
        {
            return Err(DirectHitlError::new(DirectHitlErrorCode::InvalidInput));
        }
        let value = if matches!(
            raw.action,
            DirectHitlAction::BlockWithComment | DirectHitlAction::Answer
        ) {
            let maximum = if matches!(raw.action, DirectHitlAction::Answer) {
                MAX_ANSWER_BYTES
            } else {
                MAX_COMMENT_BYTES
            };
            if raw.value.is_empty() || raw.value.len() > maximum || raw.value.contains('\0') {
                return Err(DirectHitlError::new(DirectHitlErrorCode::InvalidInput));
            }
            Some(raw.value)
        } else {
            if !raw.value.is_empty() {
                return Err(DirectHitlError::new(DirectHitlErrorCode::InvalidInput));
            }
            None
        };
        Ok(Self {
            interrupt_id: raw.interrupt_id,
            tool_call_id: (!raw.tool_call_id.is_empty()).then_some(raw.tool_call_id),
            guardrail_type: raw.guardrail_type,
            action: raw.action,
            value,
        })
    }

    fn raw_value(&self) -> &str {
        self.value.as_deref().unwrap_or("")
    }

    /// Resolve this decision against the latest persisted ADK confirmation.
    pub(crate) fn resolve(
        self,
        session: &dyn adk_rust::session::Session,
    ) -> Result<ResolvedDirectHitlDecision, DirectHitlError> {
        let events = session.events().all();
        let confirmation_index = events
            .iter()
            .rposition(|event| event.actions.tool_confirmation.is_some())
            .ok_or_else(|| DirectHitlError::new(DirectHitlErrorCode::StaleDecision))?;
        self.resolve_at(&events, confirmation_index, false)
    }

    fn resolve_at(
        self,
        events: &[Event],
        confirmation_index: usize,
        nested: bool,
    ) -> Result<ResolvedDirectHitlDecision, DirectHitlError> {
        let confirmation_event = &events[confirmation_index];
        let request = confirmation_event
            .actions
            .tool_confirmation
            .as_ref()
            .ok_or_else(|| DirectHitlError::new(DirectHitlErrorCode::CorruptSession))?;
        let call_id = request
            .function_call_id
            .as_deref()
            .filter(|value| valid_identity(value))
            .ok_or_else(|| DirectHitlError::new(DirectHitlErrorCode::CorruptSession))?;
        if !valid_identity(&request.tool_name)
            || encoded_value_len(&request.args)? > MAX_CALL_VALUE_BYTES
        {
            return Err(DirectHitlError::new(DirectHitlErrorCode::CorruptSession));
        }
        if self
            .tool_call_id
            .as_deref()
            .is_some_and(|submitted| submitted != call_id)
        {
            return Err(DirectHitlError::new(DirectHitlErrorCode::StaleDecision));
        }
        let (interrupt_id, call_digest) = sensitive_call_identity(
            &confirmation_event.invocation_id,
            call_id,
            &request.tool_name,
            &request.args,
        )?;
        if self.interrupt_id != interrupt_id {
            return Err(DirectHitlError::new(DirectHitlErrorCode::StaleDecision));
        }
        let matching_calls = events[..confirmation_index]
            .iter()
            .filter(|event| event.invocation_id == confirmation_event.invocation_id)
            .flat_map(Event::tool_calls)
            .filter(|call| {
                call.call_id == Some(call_id)
                    && call.name == request.tool_name
                    && call.args == &request.args
            })
            .count();
        if matching_calls != 1 {
            return Err(DirectHitlError::new(DirectHitlErrorCode::CorruptSession));
        }
        let (decision, delegated_authorization, clarifying_question) =
            resolved_guardrail_decision(confirmation_event, self.guardrail_type, self.action)?;
        let is_delegated_authorization = delegated_authorization.is_some();
        let denial_comment = (decision == ToolConfirmationDecision::Deny)
            .then_some(self.value.as_deref())
            .flatten();
        let user_content = replay_user_content(&interrupt_id, decision, denial_comment);
        // The sensitive-tool pause replays its whole assistant message; the
        // other two guardrails stay on the single call they name.
        let replay_calls = if delegated_authorization.is_some() || clarifying_question.is_some() {
            vec![ReplayCall {
                call_id: call_id.to_owned(),
                tool_name: request.tool_name.clone(),
                arguments: request.args.clone(),
                settled: None,
            }]
        } else {
            replay_calls_for(events, confirmation_index, call_id, &request.tool_name)?
        };
        let persisted = resume_state(
            events,
            confirmation_index,
            &user_content,
            &replay_calls,
            ResumeStateShape {
                nested,
                delegated_authorization: is_delegated_authorization,
                decision,
            },
        )?;
        let application_route = application_route(confirmation_event)?;
        if nested != application_route.is_some() {
            return Err(DirectHitlError::new(DirectHitlErrorCode::CorruptSession));
        }
        Ok(ResolvedDirectHitlDecision {
            invocation_id: confirmation_event.invocation_id.clone(),
            interrupt_id,
            call_digest,
            call_id: call_id.to_owned(),
            tool_name: request.tool_name.clone(),
            arguments: request.args.clone(),
            fingerprint: tool_call_fingerprint(&request.tool_name, &request.args),
            decision,
            decision_value: self.value,
            user_content,
            resume_mode: persisted.mode,
            persisted_result: persisted.result,
            replay_calls,
            application_route,
            delegated_authorization,
            clarifying_question,
        })
    }
}

/// Which of the three suffix contracts one resume is proven against.
#[derive(Clone, Copy)]
struct ResumeStateShape {
    nested: bool,
    delegated_authorization: bool,
    decision: ToolConfirmationDecision,
}

/// What the session already holds for this decision: nothing yet, or an exact
/// replay suffix a crash interrupted.
fn resume_state(
    events: &[Event],
    confirmation_index: usize,
    user_content: &Content,
    replay_calls: &[ReplayCall],
    shape: ResumeStateShape,
) -> Result<PersistedReplayState, DirectHitlError> {
    let confirmation_event = &events[confirmation_index];
    let request = confirmation_event
        .actions
        .tool_confirmation
        .as_ref()
        .ok_or_else(|| DirectHitlError::new(DirectHitlErrorCode::CorruptSession))?;
    let call_id = request
        .function_call_id
        .as_deref()
        .ok_or_else(|| DirectHitlError::new(DirectHitlErrorCode::CorruptSession))?;
    let later = &events[confirmation_index + 1..];
    if shape.nested {
        validate_unadvanced_nested_confirmation(later, confirmation_event)?;
        return Ok(PersistedReplayState {
            mode: ReplayResumeMode::ExecuteCall,
            result: None,
        });
    }
    persisted_replay_state_with_confirmation(
        later,
        user_content,
        call_id,
        &request.tool_name,
        &request.args,
        if shape.delegated_authorization {
            (shape.decision == ToolConfirmationDecision::Deny).then_some(shape.decision)
        } else {
            Some(shape.decision)
        },
        replay_calls,
    )
}

fn resolved_guardrail_decision(
    confirmation: &Event,
    submitted_guardrail: Option<DirectGuardrailType>,
    action: DirectHitlAction,
) -> Result<
    (
        ToolConfirmationDecision,
        Option<DelegatedAuthorizationRequirement>,
        Option<AskUserRequest>,
    ),
    DirectHitlError,
> {
    let authorization = confirmation
        .provider_metadata
        .get(DELEGATED_AUTHORIZATION_METADATA_KEY)
        .and_then(|value| decode_delegated_authorization_requirement(value));
    let is_authorization = authorization.is_some();
    let clarifying_question = confirmation
        .provider_metadata
        .get(ASK_USER_METADATA_KEY)
        .and_then(|value| decode_ask_user_request(value));
    if is_authorization && clarifying_question.is_some() {
        return Err(DirectHitlError::new(DirectHitlErrorCode::CorruptSession));
    }
    let expected_guardrail = if is_authorization {
        DirectGuardrailType::McpAuth
    } else if clarifying_question.is_some() {
        DirectGuardrailType::ClarifyingQuestion
    } else {
        DirectGuardrailType::SensitiveTool
    };
    if submitted_guardrail.is_some_and(|guardrail| guardrail != expected_guardrail) {
        return Err(DirectHitlError::new(DirectHitlErrorCode::StaleDecision));
    }
    let decision = match (expected_guardrail, action) {
        (DirectGuardrailType::SensitiveTool, DirectHitlAction::Approve)
        | (DirectGuardrailType::McpAuth, DirectHitlAction::Authorize)
        | (DirectGuardrailType::ClarifyingQuestion, DirectHitlAction::Answer) => {
            ToolConfirmationDecision::Approve
        }
        (
            DirectGuardrailType::SensitiveTool,
            DirectHitlAction::Reject | DirectHitlAction::BlockWithComment,
        )
        | (DirectGuardrailType::McpAuth, DirectHitlAction::Skip) => ToolConfirmationDecision::Deny,
        _ => return Err(DirectHitlError::new(DirectHitlErrorCode::StaleDecision)),
    };
    Ok((decision, authorization, clarifying_question))
}

impl DirectHitlAction {
    const fn as_str(self) -> &'static str {
        match self {
            Self::Approve => "approve",
            Self::Reject => "reject",
            Self::BlockWithComment => "block_with_comment",
            Self::Authorize => "authorize",
            Self::Skip => "skip",
            Self::Answer => "answer",
        }
    }
}

fn matching_confirmation_index(
    events: &[Event],
    submitted_interrupt_id: &str,
) -> Result<usize, DirectHitlError> {
    let mut matched = None;
    for (index, event) in events.iter().enumerate() {
        let Some(request) = event.actions.tool_confirmation.as_ref() else {
            continue;
        };
        let call_id = request
            .function_call_id
            .as_deref()
            .filter(|value| valid_identity(value))
            .ok_or_else(|| DirectHitlError::new(DirectHitlErrorCode::CorruptSession))?;
        if !valid_identity(&event.invocation_id)
            || !valid_identity(&request.tool_name)
            || encoded_value_len(&request.args)? > MAX_CALL_VALUE_BYTES
        {
            return Err(DirectHitlError::new(DirectHitlErrorCode::CorruptSession));
        }
        let (interrupt_id, _) = sensitive_call_identity(
            &event.invocation_id,
            call_id,
            &request.tool_name,
            &request.args,
        )?;
        if interrupt_id == submitted_interrupt_id && matched.replace(index).is_some() {
            return Err(DirectHitlError::new(DirectHitlErrorCode::CorruptSession));
        }
    }
    matched.ok_or_else(|| DirectHitlError::new(DirectHitlErrorCode::StaleDecision))
}

fn application_route(event: &Event) -> Result<Option<DirectHitlApplicationRoute>, DirectHitlError> {
    let container = event
        .provider_metadata
        .get(DESCENDANT_CONTAINER_INVOCATION_KEY);
    let parent_call = event.provider_metadata.get(DESCENDANT_PARENT_CALL_KEY);
    match (container, parent_call) {
        (None, None) => Ok(None),
        (Some(container_invocation_id), Some(parent_call_id))
            if valid_identity(container_invocation_id)
                && valid_identity(parent_call_id)
                && valid_identity(&event.branch) =>
        {
            Ok(Some(DirectHitlApplicationRoute {
                container_invocation_id: container_invocation_id.clone(),
                parent_call_id: parent_call_id.clone(),
                branch: event.branch.clone(),
            }))
        }
        _ => Err(DirectHitlError::new(DirectHitlErrorCode::CorruptSession)),
    }
}

fn validate_unadvanced_nested_confirmation(
    later_events: &[Event],
    confirmation: &Event,
) -> Result<(), DirectHitlError> {
    if later_events
        .iter()
        .filter(|event| semantic_event(event))
        .any(|event| event.invocation_id == confirmation.invocation_id)
    {
        return Err(DirectHitlError::new(DirectHitlErrorCode::StaleDecision));
    }
    Ok(())
}

/// One function call of the assistant message a pause belongs to.
///
/// A sensitive pause abandons the WHOLE message: ADK's confirmation pre-check
/// (`llm_agent.rs`) breaks out of its scan and returns before ANY tool of that
/// message is dispatched, so the non-sensitive calls beside the paused one are
/// still unexecuted when the decision comes back. The replay therefore
/// re-emits the whole message rather than the single decided call, and carries
/// each call's already-taken decision with it so a message with two sensitive
/// calls can be decided one card at a time without losing the first decision.
#[derive(Clone)]
struct ReplayCall {
    call_id: String,
    tool_name: String,
    arguments: Value,
    /// The decision an EARLIER resume of this same message already took for
    /// this call. `None` for the call being decided now and for any call no
    /// pause was ever raised for.
    settled: Option<SettledDecision>,
}

/// A decision recovered from the durable replay marker of an earlier resume.
#[derive(Clone)]
struct SettledDecision {
    decision: ToolConfirmationDecision,
    comment: Option<String>,
}

/// Exact, session-proven call plus its one browser decision.
///
/// This value does not grant execution and deliberately implements neither
/// `Clone` nor `Debug` because arguments and the readable ADK fingerprint can
/// contain credentials.
pub(crate) struct ResolvedDirectHitlDecision {
    invocation_id: String,
    interrupt_id: String,
    call_digest: String,
    call_id: String,
    tool_name: String,
    arguments: Value,
    fingerprint: String,
    decision: ToolConfirmationDecision,
    decision_value: Option<String>,
    user_content: Content,
    resume_mode: ReplayResumeMode,
    persisted_result: Option<Value>,
    /// Every call of the paused assistant message, in the order the model
    /// emitted them — see [`ReplayCall`].
    replay_calls: Vec<ReplayCall>,
    application_route: Option<DirectHitlApplicationRoute>,
    delegated_authorization: Option<DelegatedAuthorizationRequirement>,
    clarifying_question: Option<AskUserRequest>,
}

pub(crate) enum ResolvedDirectHitlStart {
    Direct(Box<ResolvedDirectHitlDecision>),
    Nested(Vec<ResolvedDirectHitlDecision>),
}

#[derive(Clone)]
pub(crate) struct DirectHitlApplicationRoute {
    container_invocation_id: String,
    parent_call_id: String,
    branch: String,
}

/// One exact call prepared for native ADK replay.
///
/// Construction permits an approved read or a denied call. A denied effect is
/// safe here because the real tool is replaced before Runner construction.
pub(crate) struct DirectHitlReplay {
    /// The whole assistant message, in emission order.
    calls: Vec<ReplayCall>,
    /// The call this resume decided — the one the replay is proven against.
    call_id: String,
    tool_name: String,
    arguments: Value,
    user_content: Content,
    resume_mode: ReplayResumeMode,
    /// `call_id` -> ADK confirmation fingerprint, for every call that already
    /// has a decision (this one and the ones earlier cards settled).
    approvals: Vec<(String, String)>,
    /// The locally served results that replace a declined call's dispatch.
    blocked: Vec<BlockedToolReplay>,
}

#[derive(Clone, Copy, Eq, PartialEq)]
enum ReplayResumeMode {
    ExecuteCall,
    ContinueAfterResult,
}

struct PersistedReplayState {
    mode: ReplayResumeMode,
    result: Option<Value>,
}

impl ResolvedDirectHitlDecision {
    pub(crate) fn tool_name(&self) -> &str {
        &self.tool_name
    }

    pub(crate) const fn is_delegated_authorization(&self) -> bool {
        self.delegated_authorization.is_some()
    }

    pub(crate) const fn is_clarifying_question(&self) -> bool {
        self.clarifying_question.is_some()
    }

    /// Narrow one resolved decision to the safe direct replay boundary.
    ///
    /// Approved calls must be read-only until durable effect ownership exists.
    /// A denied call may be effectful because the real tool is replaced by a
    /// local structured-result adapter and is never dispatched.
    pub(crate) fn into_direct_replay(
        self,
        sensitive_tools: &SensitiveToolCatalog,
    ) -> Result<DirectHitlReplay, DirectHitlError> {
        if self.delegated_authorization.is_some() || self.clarifying_question.is_some() {
            return Err(DirectHitlError::new(DirectHitlErrorCode::CorruptSession));
        }
        let policy = sensitive_tools
            .policy_for(&self.tool_name)
            .ok_or_else(|| DirectHitlError::new(DirectHitlErrorCode::UnsupportedCapability))?;
        let blocked_result = if self.decision == ToolConfirmationDecision::Deny {
            Some(blocked_tool_result(
                &self.tool_name,
                policy.toolkit_name(),
                policy.toolkit_type(),
                policy.action_name(),
                self.decision_value.as_deref(),
            ))
        } else {
            None
        };
        if blocked_result.is_none() && sensitive_tools.is_read_only(&self.tool_name) != Some(true) {
            tracing::debug!(
                tool.name = %self.tool_name,
                "approved direct HITL replay remains closed for an effectful tool"
            );
            return Err(DirectHitlError::new(
                DirectHitlErrorCode::UnsupportedCapability,
            ));
        }
        if matches!(self.resume_mode, ReplayResumeMode::ContinueAfterResult)
            && blocked_result
                .as_ref()
                .is_some_and(|expected| self.persisted_result.as_ref() != Some(expected))
        {
            return Err(DirectHitlError::new(DirectHitlErrorCode::StaleDecision));
        }
        let mut approvals = vec![(self.call_id.clone(), self.fingerprint.clone())];
        let mut blocked = Vec::with_capacity(1 + self.replay_calls.len());
        if let Some(response) = blocked_result {
            blocked.push(BlockedToolReplay {
                call_id: self.call_id.clone(),
                tool_name: self.tool_name.clone(),
                arguments: self.arguments.clone(),
                response,
                confirmation_decision: ToolConfirmationDecision::Deny,
            });
        }
        // The rest of the message travels with the decision. A call an earlier
        // card already settled keeps that settlement; a REPEAT of a settled
        // tool inherits it, which is the "one authorization per tool per turn"
        // contract the same-tool-twice case is made of; anything else is left
        // undecided so ADK raises its own card for it.
        for call in &self.replay_calls {
            if call.call_id == self.call_id {
                continue;
            }
            let settled = call.settled.as_ref().map_or_else(
                || {
                    (call.tool_name == self.tool_name).then(|| SettledDecision {
                        decision: self.decision,
                        comment: self.decision_value.clone(),
                    })
                },
                |settled| Some(settled.clone()),
            );
            let Some(settled) = settled else {
                continue;
            };
            let policy = sensitive_tools
                .policy_for(&call.tool_name)
                .ok_or_else(|| DirectHitlError::new(DirectHitlErrorCode::UnsupportedCapability))?;
            approvals.push((
                call.call_id.clone(),
                tool_call_fingerprint(&call.tool_name, &call.arguments),
            ));
            match settled.decision {
                ToolConfirmationDecision::Approve => {
                    if sensitive_tools.is_read_only(&call.tool_name) != Some(true) {
                        return Err(DirectHitlError::new(
                            DirectHitlErrorCode::UnsupportedCapability,
                        ));
                    }
                }
                ToolConfirmationDecision::Deny => blocked.push(BlockedToolReplay {
                    call_id: call.call_id.clone(),
                    tool_name: call.tool_name.clone(),
                    arguments: call.arguments.clone(),
                    response: blocked_tool_result(
                        &call.tool_name,
                        policy.toolkit_name(),
                        policy.toolkit_type(),
                        policy.action_name(),
                        settled.comment.as_deref(),
                    ),
                    confirmation_decision: ToolConfirmationDecision::Deny,
                }),
            }
        }
        Ok(DirectHitlReplay {
            calls: self.replay_calls,
            call_id: self.call_id,
            tool_name: self.tool_name,
            arguments: self.arguments,
            user_content: self.user_content,
            resume_mode: self.resume_mode,
            approvals,
            blocked,
        })
    }

    pub(crate) fn into_clarifying_question_replay(
        self,
    ) -> Result<DirectHitlReplay, DirectHitlError> {
        if self.delegated_authorization.is_some()
            || self.decision != ToolConfirmationDecision::Approve
            || self.tool_name != super::internal_tools::ASK_USER_TOOL_NAME
        {
            return Err(DirectHitlError::new(DirectHitlErrorCode::CorruptSession));
        }
        let request = self
            .clarifying_question
            .as_ref()
            .filter(|request| request.matches_arguments(&self.arguments))
            .ok_or_else(|| DirectHitlError::new(DirectHitlErrorCode::CorruptSession))?;
        let answer = self
            .decision_value
            .as_deref()
            .ok_or_else(|| DirectHitlError::new(DirectHitlErrorCode::InvalidInput))?;
        let result = request
            .format_answer(answer)
            .map_err(|_| DirectHitlError::new(DirectHitlErrorCode::InvalidInput))?;
        if matches!(self.resume_mode, ReplayResumeMode::ContinueAfterResult)
            && self.persisted_result.as_ref() != Some(&result)
        {
            return Err(DirectHitlError::new(DirectHitlErrorCode::StaleDecision));
        }
        Ok(DirectHitlReplay {
            approvals: vec![(self.call_id.clone(), self.fingerprint.clone())],
            blocked: vec![BlockedToolReplay {
                call_id: self.call_id.clone(),
                tool_name: self.tool_name.clone(),
                arguments: self.arguments.clone(),
                response: result,
                confirmation_decision: ToolConfirmationDecision::Approve,
            }],
            calls: self.replay_calls,
            call_id: self.call_id,
            tool_name: self.tool_name,
            arguments: self.arguments,
            user_content: self.user_content,
            resume_mode: self.resume_mode,
        })
    }

    pub(crate) fn into_delegated_authorization_replay(
        self,
        authorization: &DelegatedAuthorizationCatalog,
    ) -> Result<DirectHitlReplay, DirectHitlError> {
        let requirement = self
            .delegated_authorization
            .as_ref()
            .ok_or_else(|| DirectHitlError::new(DirectHitlErrorCode::CorruptSession))?;
        let blocked_result = if self.decision == ToolConfirmationDecision::Deny {
            let materialized = authorization
                .requirement_for(&self.tool_name)
                .filter(|materialized| *materialized == requirement)
                .ok_or_else(|| DirectHitlError::new(DirectHitlErrorCode::StaleDecision))?;
            Some(delegated_authorization_declined_result(
                materialized,
                &self.tool_name,
            ))
        } else {
            if authorization.requirement_for(&self.tool_name).is_some() {
                return Err(DirectHitlError::new(DirectHitlErrorCode::StaleDecision));
            }
            None
        };
        if matches!(self.resume_mode, ReplayResumeMode::ContinueAfterResult)
            && blocked_result
                .as_ref()
                .is_some_and(|expected| self.persisted_result.as_ref() != Some(expected))
        {
            return Err(DirectHitlError::new(DirectHitlErrorCode::StaleDecision));
        }
        Ok(DirectHitlReplay {
            approvals: blocked_result
                .as_ref()
                .map(|_| (self.call_id.clone(), self.fingerprint.clone()))
                .into_iter()
                .collect(),
            blocked: blocked_result
                .map(|response| BlockedToolReplay {
                    call_id: self.call_id.clone(),
                    tool_name: self.tool_name.clone(),
                    arguments: self.arguments.clone(),
                    response,
                    confirmation_decision: ToolConfirmationDecision::Deny,
                })
                .into_iter()
                .collect(),
            calls: self.replay_calls,
            call_id: self.call_id,
            tool_name: self.tool_name,
            arguments: self.arguments,
            user_content: self.user_content,
            resume_mode: self.resume_mode,
        })
    }
}

impl DirectHitlReplay {
    /// The call ids the replay re-emits, in the model's original order.
    #[cfg(test)]
    pub(crate) fn replay_call_ids(&self) -> Vec<&str> {
        self.calls
            .iter()
            .map(|call| call.call_id.as_str())
            .collect()
    }

    /// The call ids that resume with a decision already taken.
    #[cfg(test)]
    pub(crate) fn approved_call_ids(&self) -> Vec<&str> {
        self.approvals
            .iter()
            .map(|(call_id, _)| call_id.as_str())
            .collect()
    }

    /// The call ids served a local blocked result instead of a dispatch, with
    /// the reason each one carries.
    #[cfg(test)]
    pub(crate) fn blocked_calls(&self) -> Vec<(&str, &str)> {
        self.blocked
            .iter()
            .map(|blocked| {
                (
                    blocked.call_id.as_str(),
                    blocked
                        .response
                        .get("denial_reason")
                        .and_then(Value::as_str)
                        .unwrap_or_default(),
                )
            })
            .collect()
    }

    /// Bind the one-shot replay model and exact ADK confirmation decision.
    pub(crate) fn bind(self, delegate: Arc<dyn Llm>) -> PreparedDirectHitlReplay {
        let mut run_config = RunConfig::default();
        let state = match self.resume_mode {
            ReplayResumeMode::ExecuteCall => {
                // Every already-decided call of the message, not only this
                // card's: ADK keys its pre-check by function call id, so a
                // decision left out here raises the same card a second time.
                for (call_id, fingerprint) in self.approvals {
                    run_config
                        .tool_confirmation_decisions
                        .insert(call_id.clone(), ToolConfirmationDecision::Approve);
                    run_config
                        .tool_confirmation_fingerprints
                        .insert(call_id, fingerprint);
                }
                REPLAY_PENDING
            }
            ReplayResumeMode::ContinueAfterResult => REPLAY_EMITTED,
        };
        let model: Arc<dyn Llm> = Arc::new(DirectHitlReplayModel {
            delegate,
            state: AtomicU8::new(state),
            calls: self.calls,
            call_id: self.call_id,
            tool_name: self.tool_name,
            arguments: self.arguments,
        });
        PreparedDirectHitlReplay {
            model,
            run: DirectHitlRunInput {
                user_content: self.user_content,
                run_config,
            },
            blocked: self.blocked,
        }
    }
}

/// Bound replay values consumed by the invocation/session assembler.
///
/// This type is intentionally neither `Clone` nor `Debug`; the model retains
/// the raw call arguments and the user content may contain a denial comment.
pub(crate) struct PreparedDirectHitlReplay {
    model: Arc<dyn Llm>,
    run: DirectHitlRunInput,
    blocked: Vec<BlockedToolReplay>,
}

/// Opaque invocation input minted with the exact replay model.
pub(crate) struct DirectHitlRunInput {
    user_content: Content,
    run_config: RunConfig,
}

impl PreparedDirectHitlReplay {
    #[cfg(test)]
    pub(crate) fn model(&self) -> Arc<dyn Llm> {
        Arc::clone(&self.model)
    }

    #[cfg(test)]
    pub(crate) fn confirmed_call_ids(&self) -> Vec<&str> {
        let mut ids = self
            .run
            .run_config
            .tool_confirmation_decisions
            .keys()
            .map(String::as_str)
            .collect::<Vec<_>>();
        ids.sort_unstable();
        ids
    }

    pub(crate) fn into_parts(
        self,
        toolsets: Vec<Arc<dyn Toolset>>,
    ) -> (Arc<dyn Llm>, DirectHitlRunInput, Vec<Arc<dyn Toolset>>) {
        let toolsets = if self.blocked.is_empty() {
            toolsets
        } else {
            let blocked = Arc::new(self.blocked);
            toolsets
                .into_iter()
                .map(|inner| {
                    Arc::new(BlockedToolset {
                        name: format!("{}-blocked", inner.name()),
                        inner,
                        blocked: Arc::clone(&blocked),
                    }) as Arc<dyn Toolset>
                })
                .collect()
        };
        (self.model, self.run, toolsets)
    }
}

impl DirectHitlRunInput {
    pub(crate) fn from_parts(user_content: Content, run_config: RunConfig) -> Self {
        Self {
            user_content,
            run_config,
        }
    }

    pub(super) fn into_parts(self) -> (Content, RunConfig) {
        (self.user_content, self.run_config)
    }
}

#[derive(Clone)]
struct BlockedToolReplay {
    call_id: String,
    tool_name: String,
    arguments: Value,
    response: Value,
    confirmation_decision: ToolConfirmationDecision,
}

struct BlockedToolset {
    name: String,
    inner: Arc<dyn Toolset>,
    blocked: Arc<Vec<BlockedToolReplay>>,
}

#[async_trait]
impl Toolset for BlockedToolset {
    fn name(&self) -> &str {
        &self.name
    }

    async fn tools(
        &self,
        context: Arc<dyn ReadonlyContext>,
    ) -> adk_rust::Result<Vec<Arc<dyn Tool>>> {
        self.inner.tools(context).await.map(|tools| {
            tools
                .into_iter()
                .map(|inner| {
                    if self
                        .blocked
                        .iter()
                        .any(|blocked| blocked.tool_name == inner.name())
                    {
                        Arc::new(BlockedTool {
                            inner,
                            blocked: Arc::clone(&self.blocked),
                        }) as Arc<dyn Tool>
                    } else {
                        inner
                    }
                })
                .collect()
        })
    }
}

struct BlockedTool {
    inner: Arc<dyn Tool>,
    blocked: Arc<Vec<BlockedToolReplay>>,
}

#[async_trait]
impl Tool for BlockedTool {
    fn name(&self) -> &str {
        self.inner.name()
    }

    fn description(&self) -> &str {
        self.inner.description()
    }

    fn declaration(&self) -> Value {
        self.inner.declaration()
    }

    fn enhanced_description(&self) -> String {
        self.inner.enhanced_description()
    }

    fn is_long_running(&self) -> bool {
        self.inner.is_long_running()
    }

    fn is_builtin(&self) -> bool {
        self.inner.is_builtin()
    }

    fn parameters_schema(&self) -> Option<Value> {
        self.inner.parameters_schema()
    }

    fn response_schema(&self) -> Option<Value> {
        self.inner.response_schema()
    }

    fn required_scopes(&self) -> &[&str] {
        self.inner.required_scopes()
    }

    fn is_read_only(&self) -> bool {
        true
    }

    fn is_concurrency_safe(&self) -> bool {
        false
    }

    async fn execute(
        &self,
        context: Arc<dyn ToolContext>,
        arguments: Value,
    ) -> adk_rust::Result<Value> {
        // One tool can carry several calls of the same message. Only the exact
        // call ids a decision blocked are served locally; any other call of
        // this tool in the same message is a call no one declined and runs for
        // real — ADK's own pre-check still holds it if it needs its own card.
        let Some(blocked) = self
            .blocked
            .iter()
            .find(|blocked| blocked.call_id == context.function_call_id())
        else {
            return self.inner.execute(context, arguments).await;
        };
        if arguments != blocked.arguments {
            return Err(AdkError::agent(
                "the blocked direct tool call does not match its authorized replay",
            ));
        }
        let mut actions = context.actions();
        actions.tool_confirmation_decision = Some(blocked.confirmation_decision);
        context.set_actions(actions);
        Ok(blocked.response.clone())
    }
}

const REPLAY_PENDING: u8 = 0;
const REPLAY_EMITTED: u8 = 1;
const REPLAY_DELEGATING: u8 = 2;

/// Model adapter that deterministically re-emits one persisted function call.
///
/// The first generation never contacts the provider. ADK receives the original
/// function-call ID/arguments and applies the exact `RunConfig` decision before
/// its native `ToolExecutor`. Only after one matching function response exists
/// does the adapter delegate later turns to the bound provider model.
struct DirectHitlReplayModel {
    delegate: Arc<dyn Llm>,
    state: AtomicU8,
    calls: Vec<ReplayCall>,
    call_id: String,
    tool_name: String,
    arguments: Value,
}

#[async_trait]
impl Llm for DirectHitlReplayModel {
    fn name(&self) -> &str {
        self.delegate.name()
    }

    fn schema_adapter(&self) -> &dyn SchemaAdapter {
        self.delegate.schema_adapter()
    }

    fn uses_interactions_api(&self) -> bool {
        self.delegate.uses_interactions_api()
    }

    async fn generate_content(
        &self,
        request: LlmRequest,
        stream_response: bool,
    ) -> adk_rust::Result<LlmResponseStream> {
        match self.state.compare_exchange(
            REPLAY_PENDING,
            REPLAY_EMITTED,
            Ordering::AcqRel,
            Ordering::Acquire,
        ) {
            Ok(_) => {
                let parts = self.replay_parts(&request)?;
                tracing::debug!(
                    tool.name = %self.tool_name,
                    tool.call_id = %self.call_id,
                    replay.calls = parts.len(),
                    "re-emitting the persisted direct tool calls through ADK"
                );
                let response = LlmResponse {
                    content: Some(Content {
                        role: "model".to_owned(),
                        parts,
                    }),
                    finish_reason: Some(FinishReason::Stop),
                    turn_complete: true,
                    ..LlmResponse::default()
                };
                Ok(Box::pin(stream::once(async move { Ok(response) })))
            }
            Err(REPLAY_EMITTED) => {
                if self
                    .state
                    .compare_exchange(
                        REPLAY_EMITTED,
                        REPLAY_DELEGATING,
                        Ordering::AcqRel,
                        Ordering::Acquire,
                    )
                    .is_ok()
                {
                    self.validate_completed_replay(&request)?;
                    tracing::debug!(
                        tool.name = %self.tool_name,
                        tool.call_id = %self.call_id,
                        "validated the replayed tool result before provider continuation"
                    );
                }
                let request = without_replay_markers(request);
                self.delegate
                    .generate_content(request, stream_response)
                    .await
            }
            Err(_) => {
                let request = without_replay_markers(request);
                self.delegate
                    .generate_content(request, stream_response)
                    .await
            }
        }
    }
}

/// Drop every direct-HITL replay marker from a request.
///
/// Matched by PREFIX rather than by the one marker this resume wrote: a
/// message decided one card at a time leaves one marker per decision behind,
/// and none of them is model-facing content.
fn without_replay_markers(mut request: LlmRequest) -> LlmRequest {
    request.contents.retain(|content| {
        content.role != "user"
            || !matches!(
                content.parts.first(),
                Some(Part::Text { text }) if text.starts_with(REPLAY_MARKER_PREFIX)
            )
    });
    request
}

impl DirectHitlReplayModel {
    /// The whole paused assistant message, re-emitted in its original order.
    ///
    /// The decided call is proven; a companion that is no longer replayable —
    /// its tool withdrawn, or a result already persisted for it — is dropped
    /// with a warning rather than failing the resume the user is waiting on.
    fn replay_parts(&self, request: &LlmRequest) -> adk_rust::Result<Vec<Part>> {
        self.validate_pending_request(request)?;
        let mut parts = Vec::with_capacity(self.calls.len().max(1));
        for call in &self.calls {
            if call.call_id != self.call_id
                && (!request.tools.contains_key(&call.tool_name)
                    || latest_call_state(request, &call.call_id, &call.tool_name, &call.arguments)
                        != LatestCallState::Pending)
            {
                tracing::warn!(
                    tool.name = %call.tool_name,
                    tool.call_id = %call.call_id,
                    "dropping one call of the paused assistant message: it is no longer replayable"
                );
                continue;
            }
            parts.push(Part::FunctionCall {
                name: call.tool_name.clone(),
                args: call.arguments.clone(),
                id: Some(call.call_id.clone()),
                thought_signature: None,
            });
        }
        if parts.is_empty() {
            return Err(AdkError::agent(
                "the persisted direct tool call is unavailable for exact replay",
            ));
        }
        Ok(parts)
    }

    fn validate_pending_request(&self, request: &LlmRequest) -> adk_rust::Result<()> {
        let state = latest_call_state(request, &self.call_id, &self.tool_name, &self.arguments);
        if !request.tools.contains_key(&self.tool_name) || state != LatestCallState::Pending {
            return Err(AdkError::agent(
                "the persisted direct tool call is unavailable for exact replay",
            ));
        }
        Ok(())
    }

    fn validate_completed_replay(&self, request: &LlmRequest) -> adk_rust::Result<()> {
        if !request.tools.contains_key(&self.tool_name)
            || latest_call_state(request, &self.call_id, &self.tool_name, &self.arguments)
                != LatestCallState::Completed
        {
            return Err(AdkError::agent(
                "the replayed direct tool result is unavailable for model continuation",
            ));
        }
        Ok(())
    }
}

#[derive(Clone, Copy, Eq, PartialEq)]
enum LatestCallState {
    Missing,
    Pending,
    Completed,
}

fn latest_call_state(
    request: &LlmRequest,
    call_id: &str,
    tool_name: &str,
    arguments: &Value,
) -> LatestCallState {
    let mut last_call = None;
    let mut last_response = None;
    for (position, part) in request
        .contents
        .iter()
        .flat_map(|content| &content.parts)
        .enumerate()
    {
        match part {
            Part::FunctionCall {
                name,
                args,
                id: Some(id),
                ..
            } if id == call_id => {
                last_call = Some((position, name == tool_name && args == arguments));
            }
            Part::FunctionResponse {
                function_response,
                id: Some(id),
                ..
            } if id == call_id => {
                last_response = Some((position, function_response.name == tool_name));
            }
            _ => {}
        }
    }
    let Some((call_position, true)) = last_call else {
        return LatestCallState::Missing;
    };
    match last_response {
        Some((response_position, true)) if response_position > call_position => {
            LatestCallState::Completed
        }
        Some((response_position, _)) if response_position > call_position => {
            LatestCallState::Missing
        }
        _ => LatestCallState::Pending,
    }
}

impl ResolvedDirectHitlDecision {
    pub(crate) fn interrupt_id(&self) -> &str {
        &self.interrupt_id
    }

    pub(crate) fn invocation_id(&self) -> &str {
        &self.invocation_id
    }

    pub(crate) const fn application_route(&self) -> Option<&DirectHitlApplicationRoute> {
        self.application_route.as_ref()
    }

    #[cfg(test)]
    pub(crate) fn call_digest(&self) -> &str {
        &self.call_digest
    }

    #[cfg(test)]
    pub(crate) fn call_id(&self) -> &str {
        &self.call_id
    }

    #[cfg(test)]
    pub(crate) const fn arguments(&self) -> &Value {
        &self.arguments
    }

    #[cfg(test)]
    pub(crate) fn fingerprint(&self) -> &str {
        &self.fingerprint
    }

    #[cfg(test)]
    pub(crate) const fn decision(&self) -> ToolConfirmationDecision {
        self.decision
    }

    #[cfg(test)]
    pub(crate) fn denial_comment(&self) -> Option<&str> {
        (self.decision == ToolConfirmationDecision::Deny)
            .then_some(self.decision_value.as_deref())
            .flatten()
    }

    #[cfg(test)]
    pub(crate) const fn has_persisted_result(&self) -> bool {
        matches!(self.resume_mode, ReplayResumeMode::ContinueAfterResult)
    }
}

impl DirectHitlApplicationRoute {
    pub(crate) fn container_invocation_id(&self) -> &str {
        &self.container_invocation_id
    }

    pub(crate) fn parent_call_id(&self) -> &str {
        &self.parent_call_id
    }

    pub(crate) fn branch(&self) -> &str {
        &self.branch
    }
}

/// The durable marker one resume writes into the session.
///
/// It carries the denial comment as well as the action because a message with
/// two sensitive calls is decided one card at a time: the SECOND resume has to
/// reconstruct the first call's blocked result — the reviewer's own words
/// included — and the marker is the only place that decision survives. The
/// marker never reaches the model: [`without_replay_markers`] strips every one
/// of them from each request.
fn replay_user_content(
    interrupt_id: &str,
    decision: ToolConfirmationDecision,
    denial_comment: Option<&str>,
) -> Content {
    let prefix = format!("{REPLAY_MARKER_PREFIX}{interrupt_id}] ");
    match decision {
        ToolConfirmationDecision::Approve => {
            Content::new("user").with_text(format!("{prefix}{REPLAY_APPROVED_TEXT}"))
        }
        ToolConfirmationDecision::Deny => Content::new("user").with_text(match denial_comment {
            Some(comment) => {
                format!("{prefix}{REPLAY_REJECTED_TEXT}{REPLAY_COMMENT_INFIX}{comment}")
            }
            None => format!("{prefix}{REPLAY_REJECTED_TEXT}"),
        }),
    }
}

/// Read one durable replay marker back: its interrupt id, its decision and the
/// reviewer comment a denial carried.
fn replay_marker(event: &Event) -> Option<(&str, ToolConfirmationDecision, Option<&str>)> {
    if event.author != "user" {
        return None;
    }
    let content = event.llm_response.content.as_ref()?;
    if content.role != "user" || content.parts.len() != 1 {
        return None;
    }
    let Part::Text { text } = content.parts.first()? else {
        return None;
    };
    let (interrupt_id, rest) = text.strip_prefix(REPLAY_MARKER_PREFIX)?.split_once("] ")?;
    if !valid_identity(interrupt_id) {
        return None;
    }
    if let Some(rest) = rest.strip_prefix(REPLAY_REJECTED_TEXT) {
        let comment = rest.strip_prefix(REPLAY_COMMENT_INFIX);
        if !rest.is_empty() && comment.is_none() {
            return None;
        }
        return Some((interrupt_id, ToolConfirmationDecision::Deny, comment));
    }
    (rest == REPLAY_APPROVED_TEXT).then_some((
        interrupt_id,
        ToolConfirmationDecision::Approve,
        None,
    ))
}

/// Every call of the assistant message that carried `call_id`, in emission
/// order, each with the decision an earlier resume already took for it.
fn replay_calls_for(
    events: &[Event],
    confirmation_index: usize,
    call_id: &str,
    tool_name: &str,
) -> Result<Vec<ReplayCall>, DirectHitlError> {
    let confirmation = &events[confirmation_index];
    let arguments = &confirmation
        .actions
        .tool_confirmation
        .as_ref()
        .ok_or_else(|| DirectHitlError::new(DirectHitlErrorCode::CorruptSession))?
        .args;
    let origin = events[..confirmation_index]
        .iter()
        .find(|event| {
            event.invocation_id == confirmation.invocation_id
                && event.tool_calls().iter().any(|call| {
                    call.call_id == Some(call_id)
                        && call.name == tool_name
                        && call.args == arguments
                })
        })
        .ok_or_else(|| DirectHitlError::new(DirectHitlErrorCode::CorruptSession))?;
    let calls = origin.tool_calls();
    if calls.is_empty() {
        return Err(DirectHitlError::new(DirectHitlErrorCode::CorruptSession));
    }
    if calls.len() > MAX_REPLAY_CALLS {
        return Err(DirectHitlError::new(DirectHitlErrorCode::ResourceExhausted));
    }
    let mut seen = HashSet::with_capacity(calls.len());
    let mut replay = Vec::with_capacity(calls.len());
    for call in calls {
        let Some(id) = call.call_id.filter(|id| valid_identity(id)) else {
            return Err(DirectHitlError::new(DirectHitlErrorCode::CorruptSession));
        };
        if !valid_identity(call.name) || encoded_value_len(call.args)? > MAX_CALL_VALUE_BYTES {
            return Err(DirectHitlError::new(DirectHitlErrorCode::CorruptSession));
        }
        if !seen.insert(id.to_owned()) {
            return Err(DirectHitlError::new(DirectHitlErrorCode::CorruptSession));
        }
        let settled = if id == call_id {
            None
        } else {
            settled_decision(events, confirmation_index, id, call.name, call.args)?
        };
        replay.push(ReplayCall {
            call_id: id.to_owned(),
            tool_name: call.name.to_owned(),
            arguments: call.args.clone(),
            settled,
        });
    }
    Ok(replay)
}

/// The decision an earlier resume of this same message already took for one
/// call, proven by its own persisted confirmation event and replay marker.
fn settled_decision(
    events: &[Event],
    confirmation_index: usize,
    call_id: &str,
    tool_name: &str,
    arguments: &Value,
) -> Result<Option<SettledDecision>, DirectHitlError> {
    let mut settled = None;
    for (index, event) in events[..confirmation_index].iter().enumerate() {
        let Some(request) = event.actions.tool_confirmation.as_ref() else {
            continue;
        };
        if request.function_call_id.as_deref() != Some(call_id)
            || request.tool_name != tool_name
            || &request.args != arguments
        {
            continue;
        }
        let (interrupt_id, _) =
            sensitive_call_identity(&event.invocation_id, call_id, tool_name, arguments)?;
        if let Some((_, decision, comment)) = events[index + 1..]
            .iter()
            .filter_map(replay_marker)
            .find(|marker| marker.0 == interrupt_id)
        {
            settled = Some(SettledDecision {
                decision,
                comment: comment.map(ToOwned::to_owned),
            });
        }
    }
    Ok(settled)
}

fn persisted_replay_state_with_confirmation(
    events: &[Event],
    user_content: &Content,
    call_id: &str,
    tool_name: &str,
    arguments: &Value,
    confirmation_decision: Option<ToolConfirmationDecision>,
    replay_calls: &[ReplayCall],
) -> Result<PersistedReplayState, DirectHitlError> {
    let mut replay_invocation = None;
    let mut call_pending = false;
    let mut result_persisted = None;
    for event in events.iter().filter(|event| semantic_event(event)) {
        if exact_replay_user_event(event, user_content) {
            replay_invocation = Some(event.invocation_id.as_str());
            call_pending = false;
            result_persisted = None;
            continue;
        }
        if result_persisted.is_none()
            && exact_replay_call(
                event,
                replay_invocation,
                call_id,
                tool_name,
                arguments,
                replay_calls,
            )
        {
            call_pending = true;
            continue;
        }
        if call_pending {
            if result_persisted.is_none()
                && let Some(result) = exact_replay_result(
                    event,
                    replay_invocation,
                    call_id,
                    tool_name,
                    confirmation_decision,
                )
            {
                result_persisted = Some(result.clone());
                continue;
            }
            // A replayed message dispatches every call it carries and ADK
            // yields one event per result, so the siblings of the decided call
            // are an expected part of the suffix rather than a foreign
            // advance.
            if sibling_replay_result(event, replay_invocation, call_id, replay_calls) {
                continue;
            }
        }
        return Err(DirectHitlError::new(DirectHitlErrorCode::StaleDecision));
    }
    Ok(PersistedReplayState {
        mode: if result_persisted.is_some() {
            ReplayResumeMode::ContinueAfterResult
        } else {
            ReplayResumeMode::ExecuteCall
        },
        result: result_persisted,
    })
}

fn exact_replay_user_event(event: &Event, expected: &Content) -> bool {
    let Some(content) = event.llm_response.content.as_ref() else {
        return false;
    };
    event.author == "user"
        && content.role == expected.role
        && content.parts == expected.parts
        && event.actions.tool_confirmation.is_none()
        && event.actions.tool_confirmation_decision.is_none()
        && event.actions.state_delta.is_empty()
        && event.actions.artifact_delta.is_empty()
        && event.actions.transfer_to_agent.is_none()
        && !event.actions.escalate
}

fn exact_replay_call(
    event: &Event,
    replay_invocation: Option<&str>,
    call_id: &str,
    tool_name: &str,
    arguments: &Value,
    replay_calls: &[ReplayCall],
) -> bool {
    let calls = event.tool_calls();
    replay_invocation == Some(event.invocation_id.as_str())
        && calls.iter().any(|call| {
            call.call_id == Some(call_id) && call.name == tool_name && call.args == arguments
        })
        // Nothing beyond the message the decision was taken on may appear.
        && calls.iter().all(|call| {
            replay_calls.iter().any(|replay| {
                call.call_id == Some(replay.call_id.as_str())
                    && call.name == replay.tool_name
                    && call.args == &replay.arguments
            })
        })
        && event.tool_results().is_empty()
        && event.actions.tool_confirmation.is_none()
        && event.actions.tool_confirmation_decision.is_none()
}

/// One persisted result of a call the replay carries BESIDE the decided one.
fn sibling_replay_result(
    event: &Event,
    replay_invocation: Option<&str>,
    call_id: &str,
    replay_calls: &[ReplayCall],
) -> bool {
    let results = event.tool_results();
    replay_invocation == Some(event.invocation_id.as_str())
        && results.len() == 1
        && results[0].call_id != Some(call_id)
        && replay_calls.iter().any(|replay| {
            results[0].call_id == Some(replay.call_id.as_str())
                && results[0].name == replay.tool_name
        })
        && event.tool_calls().is_empty()
        && event.actions.tool_confirmation.is_none()
}

fn exact_replay_result<'a>(
    event: &'a Event,
    replay_invocation: Option<&str>,
    call_id: &str,
    tool_name: &str,
    confirmation_decision: Option<ToolConfirmationDecision>,
) -> Option<&'a Value> {
    let results = event.tool_results();
    (replay_invocation == Some(event.invocation_id.as_str())
        && results.len() == 1
        && results[0].call_id == Some(call_id)
        && results[0].name == tool_name
        && event.tool_calls().is_empty()
        && event.actions.tool_confirmation.is_none()
        && event.actions.tool_confirmation_decision == confirmation_decision)
        .then_some(results[0].response)
}

pub(crate) fn blocked_tool_result(
    tool_name: &str,
    toolkit_name: &str,
    toolkit_type: &str,
    action_label: &str,
    denial_comment: Option<&str>,
) -> Value {
    let mut result = serde_json::Map::from_iter([
        (
            "type".to_owned(),
            Value::String(BLOCKED_TOOL_RESULT_TYPE.to_owned()),
        ),
        (
            "blocked_tool_name".to_owned(),
            Value::String(tool_name.to_owned()),
        ),
        (
            "denial_reason".to_owned(),
            Value::String(
                denial_comment
                    .unwrap_or(BLOCKED_TOOL_DEFAULT_REASON)
                    .to_owned(),
            ),
        ),
        (
            "message".to_owned(),
            Value::String(format!(
                "You declined THIS specific call to '{action_label}'; it was not executed. The block is for THIS invocation only, not the tool itself. This is NOT a stop signal — do not end your turn or summarize yet. Do not retry this same call with the same arguments, but DO continue: if more items remain, call the tool again for the NEXT item now; otherwise use another available tool to keep making progress. Only stop and ask the user when nothing remains that can be done without this exact declined call."
            )),
        ),
    ]);
    if !toolkit_name.is_empty() {
        result.insert(
            "blocked_toolkit_name".to_owned(),
            Value::String(toolkit_name.to_owned()),
        );
    }
    if !toolkit_type.is_empty() {
        result.insert(
            "blocked_toolkit_type".to_owned(),
            Value::String(toolkit_type.to_owned()),
        );
    }
    Value::Object(result)
}

pub(super) fn sensitive_call_identity(
    invocation_id: &str,
    call_id: &str,
    tool_name: &str,
    arguments: &Value,
) -> Result<(String, String), DirectHitlError> {
    if !valid_identity(invocation_id)
        || !valid_identity(call_id)
        || !valid_identity(tool_name)
        || encoded_value_len(arguments)? > MAX_CALL_VALUE_BYTES
    {
        return Err(DirectHitlError::new(DirectHitlErrorCode::CorruptSession));
    }
    let canonical = serde_json::to_vec(&canonical_value(arguments, 0)?)
        .map_err(|_| DirectHitlError::new(DirectHitlErrorCode::CorruptSession))?;
    let mut context = digest::Context::new(&digest::SHA256);
    context.update(HITL_DIGEST_DOMAIN);
    for field in [
        invocation_id.as_bytes(),
        call_id.as_bytes(),
        tool_name.as_bytes(),
    ] {
        context.update(&(field.len() as u64).to_be_bytes());
        context.update(field);
    }
    context.update(&(canonical.len() as u64).to_be_bytes());
    context.update(&canonical);
    let digest = context.finish();
    Ok((
        format!("hitl_e1:{}", URL_SAFE_NO_PAD.encode(digest.as_ref())),
        format!("sha256:{}", hex(digest.as_ref())),
    ))
}

fn semantic_event(event: &Event) -> bool {
    event.llm_response.content.is_some()
        || event.actions.tool_confirmation.is_some()
        || event.actions.tool_confirmation_decision.is_some()
        || !event.actions.state_delta.is_empty()
        || !event.actions.artifact_delta.is_empty()
        || event.actions.transfer_to_agent.is_some()
        || event.actions.escalate
}

fn encoded_value_len(value: &Value) -> Result<usize, DirectHitlError> {
    serde_json::to_vec(value)
        .map(|encoded| encoded.len())
        .map_err(|_| DirectHitlError::new(DirectHitlErrorCode::CorruptSession))
}

fn canonical_value(value: &Value, depth: usize) -> Result<Value, DirectHitlError> {
    if depth > MAX_JSON_DEPTH {
        return Err(DirectHitlError::new(DirectHitlErrorCode::ResourceExhausted));
    }
    match value {
        Value::Array(values) => values
            .iter()
            .map(|value| canonical_value(value, depth + 1))
            .collect::<Result<Vec<_>, _>>()
            .map(Value::Array),
        Value::Object(values) => {
            let mut keys = values.keys().collect::<Vec<_>>();
            keys.sort_unstable();
            let mut object = serde_json::Map::with_capacity(values.len());
            for key in keys {
                let item = values
                    .get(key)
                    .ok_or_else(|| DirectHitlError::new(DirectHitlErrorCode::CorruptSession))?;
                object.insert(key.clone(), canonical_value(item, depth + 1)?);
            }
            Ok(Value::Object(object))
        }
        value => Ok(value.clone()),
    }
}

fn valid_identity(value: &str) -> bool {
    !value.is_empty() && value.len() <= MAX_IDENTITY_BYTES && !value.chars().any(char::is_control)
}

fn hex(bytes: &[u8]) -> String {
    const DIGITS: &[u8; 16] = b"0123456789abcdef";
    let mut output = String::with_capacity(bytes.len() * 2);
    for byte in bytes {
        output.push(char::from(DIGITS[usize::from(byte >> 4)]));
        output.push(char::from(DIGITS[usize::from(byte & 0x0f)]));
    }
    output
}

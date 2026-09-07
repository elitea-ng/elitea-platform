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
        if server_urls.len() != 1
            || server_urls
                .iter()
                .any(|url| !DelegatedAuthorizationRequirement::valid_token_key(url))
        {
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
            .filter(|requirement| requirement.matches_token_key(&self.server_url))
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
        let user_content = replay_user_content(&interrupt_id, decision);
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
                DelegatedAuthorizationRequirement::valid_token_key(server)
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
            if decision.decision == ToolConfirmationDecision::Approve {
                let keys: Vec<_> = self
                    .authorized_servers
                    .iter()
                    .filter(|key| requirement.matches_token_key(key))
                    .cloned()
                    .collect();
                if keys.is_empty() {
                    return Err(DirectHitlError::new(DirectHitlErrorCode::StaleDecision));
                }
                authorized.extend(keys);
            } else {
                declined.insert(requirement.server_url().to_owned());
            }
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
        let user_content = replay_user_content(&interrupt_id, decision);
        let persisted = if nested {
            validate_unadvanced_nested_confirmation(
                &events[confirmation_index + 1..],
                confirmation_event,
            )?;
            PersistedReplayState {
                mode: ReplayResumeMode::ExecuteCall,
                result: None,
            }
        } else if is_delegated_authorization {
            persisted_replay_state_with_confirmation(
                &events[confirmation_index + 1..],
                &user_content,
                call_id,
                &request.tool_name,
                &request.args,
                (decision == ToolConfirmationDecision::Deny).then_some(decision),
            )?
        } else {
            persisted_replay_state(
                &events[confirmation_index + 1..],
                &user_content,
                call_id,
                &request.tool_name,
                &request.args,
                decision,
            )?
        };
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
            application_route,
            delegated_authorization,
            clarifying_question,
        })
    }
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
    call_id: String,
    tool_name: String,
    arguments: Value,
    fingerprint: String,
    user_content: Content,
    resume_mode: ReplayResumeMode,
    blocked_result: Option<Value>,
    replacement_decision: Option<ToolConfirmationDecision>,
    approve_confirmation: bool,
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
        Ok(DirectHitlReplay {
            call_id: self.call_id,
            tool_name: self.tool_name,
            arguments: self.arguments,
            fingerprint: self.fingerprint,
            user_content: self.user_content,
            resume_mode: self.resume_mode,
            blocked_result,
            replacement_decision: (self.decision == ToolConfirmationDecision::Deny)
                .then_some(ToolConfirmationDecision::Deny),
            approve_confirmation: true,
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
            call_id: self.call_id,
            tool_name: self.tool_name,
            arguments: self.arguments,
            fingerprint: self.fingerprint,
            user_content: self.user_content,
            resume_mode: self.resume_mode,
            blocked_result: Some(result),
            replacement_decision: Some(ToolConfirmationDecision::Approve),
            approve_confirmation: true,
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
                .filter(|materialized| materialized.same_authority(requirement))
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
            call_id: self.call_id,
            tool_name: self.tool_name,
            arguments: self.arguments,
            fingerprint: self.fingerprint,
            user_content: self.user_content,
            resume_mode: self.resume_mode,
            approve_confirmation: blocked_result.is_some(),
            replacement_decision: blocked_result
                .as_ref()
                .map(|_| ToolConfirmationDecision::Deny),
            blocked_result,
        })
    }
}

impl DirectHitlReplay {
    /// Bind the one-shot replay model and exact ADK confirmation decision.
    pub(crate) fn bind(self, delegate: Arc<dyn Llm>) -> PreparedDirectHitlReplay {
        let mut run_config = RunConfig::default();
        let state = match self.resume_mode {
            ReplayResumeMode::ExecuteCall => {
                if self.approve_confirmation {
                    run_config
                        .tool_confirmation_decisions
                        .insert(self.call_id.clone(), ToolConfirmationDecision::Approve);
                    run_config
                        .tool_confirmation_fingerprints
                        .insert(self.call_id.clone(), self.fingerprint);
                }
                REPLAY_PENDING
            }
            ReplayResumeMode::ContinueAfterResult => REPLAY_EMITTED,
        };
        let call_id = self.call_id;
        let tool_name = self.tool_name;
        let arguments = self.arguments;
        let replacement_decision = self.replacement_decision;
        let model: Arc<dyn Llm> = Arc::new(DirectHitlReplayModel {
            delegate,
            state: AtomicU8::new(state),
            call_id: call_id.clone(),
            tool_name: tool_name.clone(),
            arguments: arguments.clone(),
            replay_marker: self.user_content.clone(),
        });
        PreparedDirectHitlReplay {
            model,
            run: DirectHitlRunInput {
                user_content: self.user_content,
                run_config,
            },
            blocked_result: self.blocked_result.map(|response| BlockedToolReplay {
                call_id,
                tool_name,
                arguments,
                response,
                confirmation_decision: replacement_decision
                    .unwrap_or(ToolConfirmationDecision::Deny),
            }),
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
    blocked_result: Option<BlockedToolReplay>,
}

/// Opaque invocation input minted with the exact replay model.
pub(crate) struct DirectHitlRunInput {
    user_content: Content,
    run_config: RunConfig,
}

impl PreparedDirectHitlReplay {
    pub(crate) fn into_parts(
        self,
        toolsets: Vec<Arc<dyn Toolset>>,
    ) -> (Arc<dyn Llm>, DirectHitlRunInput, Vec<Arc<dyn Toolset>>) {
        let toolsets = match self.blocked_result {
            None => toolsets,
            Some(blocked) => toolsets
                .into_iter()
                .map(|inner| {
                    Arc::new(BlockedToolset {
                        name: format!("{}-blocked", inner.name()),
                        inner,
                        blocked: blocked.clone(),
                    }) as Arc<dyn Toolset>
                })
                .collect(),
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
    blocked: BlockedToolReplay,
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
                    if inner.name() == self.blocked.tool_name {
                        Arc::new(BlockedTool {
                            inner,
                            blocked: self.blocked.clone(),
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
    blocked: BlockedToolReplay,
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
        if context.function_call_id() != self.blocked.call_id || arguments != self.blocked.arguments
        {
            return Err(AdkError::agent(
                "the blocked result does not match this exact tool call",
            ));
        }
        let mut actions = context.actions();
        actions.tool_confirmation_decision = Some(self.blocked.confirmation_decision);
        context.set_actions(actions);
        Ok(self.blocked.response.clone())
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
    call_id: String,
    tool_name: String,
    arguments: Value,
    replay_marker: Content,
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
                self.validate_pending_request(&request)?;
                tracing::debug!(
                    tool.name = %self.tool_name,
                    tool.call_id = %self.call_id,
                    "re-emitting one persisted direct tool call through ADK"
                );
                let response = LlmResponse {
                    content: Some(Content {
                        role: "model".to_owned(),
                        parts: vec![Part::FunctionCall {
                            id: Some(self.call_id.clone()),
                            name: self.tool_name.clone(),
                            args: self.arguments.clone(),
                            thought_signature: None,
                        }],
                    }),
                    finish_reason: Some(FinishReason::Stop),
                    turn_complete: true,
                    ..LlmResponse::default()
                };
                Ok(Box::pin(stream::once(async move { Ok(response) })))
            }
            Err(REPLAY_EMITTED) => {
                self.validate_completed_replay(&request)?;
                let pending = self.pending_batch(&request)?;
                if !pending.is_empty() {
                    // ADK checks the whole batch before executing any call.
                    // Persist the decided result first, then admit pending calls
                    // through their normal confirmation policies.
                    let response = LlmResponse {
                        content: Some(Content {
                            role: "model".to_owned(),
                            parts: pending,
                        }),
                        finish_reason: Some(FinishReason::Stop),
                        turn_complete: true,
                        ..LlmResponse::default()
                    };
                    return Ok(Box::pin(stream::once(async move { Ok(response) })));
                }
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
                let request =
                    super::replay_history::model_continuation(request, &self.replay_marker)?;
                self.delegate
                    .generate_content(request, stream_response)
                    .await
            }
            Err(_) => {
                let request =
                    super::replay_history::model_continuation(request, &self.replay_marker)?;
                self.delegate
                    .generate_content(request, stream_response)
                    .await
            }
        }
    }
}

impl DirectHitlReplayModel {
    fn pending_batch(&self, request: &LlmRequest) -> adk_rust::Result<Vec<Part>> {
        let batch = request
            .contents
            .iter()
            .find(|content| {
                content.parts.iter().any(|part| {
            matches!(part, Part::FunctionCall { id: Some(id), .. } if id == &self.call_id)
        })
            })
            .ok_or_else(|| AdkError::agent("the interrupted tool batch is unavailable"))?;
        Ok(batch
            .parts
            .iter()
            .filter(|part| match part {
                Part::FunctionCall {
                    id: Some(id),
                    name,
                    args,
                    ..
                } => latest_call_state(request, id, name, args) == LatestCallState::Pending,
                _ => false,
            })
            .cloned()
            .collect())
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

fn replay_user_content(interrupt_id: &str, decision: ToolConfirmationDecision) -> Content {
    let prefix = format!("[Elitea direct HITL {interrupt_id}] ");
    match decision {
        ToolConfirmationDecision::Approve => Content::new("user").with_text(format!(
            "{prefix}The pending tool call was approved. Continue the original request."
        )),
        ToolConfirmationDecision::Deny => Content::new("user").with_text(format!(
            "{prefix}The pending tool call was rejected. Continue without executing it."
        )),
    }
}

fn persisted_replay_state(
    events: &[Event],
    user_content: &Content,
    call_id: &str,
    tool_name: &str,
    arguments: &Value,
    decision: ToolConfirmationDecision,
) -> Result<PersistedReplayState, DirectHitlError> {
    persisted_replay_state_with_confirmation(
        events,
        user_content,
        call_id,
        tool_name,
        arguments,
        Some(decision),
    )
}

fn persisted_replay_state_with_confirmation(
    events: &[Event],
    user_content: &Content,
    call_id: &str,
    tool_name: &str,
    arguments: &Value,
    confirmation_decision: Option<ToolConfirmationDecision>,
) -> Result<PersistedReplayState, DirectHitlError> {
    let mut replay_invocation = None;
    let mut call_pending = false;
    let mut result_persisted = None;
    for event in events.iter().filter(|event| semantic_event(event)) {
        if exact_replay_user_event(event, user_content) {
            replay_invocation = Some(event.invocation_id.as_str());
            call_pending = false;
            continue;
        }
        if result_persisted.is_some() {
            return Err(DirectHitlError::new(DirectHitlErrorCode::StaleDecision));
        }
        if exact_replay_call(event, replay_invocation, call_id, tool_name, arguments) {
            call_pending = true;
            continue;
        }
        if call_pending {
            let Some(result) = exact_replay_result(
                event,
                replay_invocation,
                call_id,
                tool_name,
                confirmation_decision,
            ) else {
                return Err(DirectHitlError::new(DirectHitlErrorCode::StaleDecision));
            };
            result_persisted = Some(result.clone());
            call_pending = false;
            continue;
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
) -> bool {
    let calls = event.tool_calls();
    replay_invocation == Some(event.invocation_id.as_str())
        && calls.len() == 1
        && calls[0].call_id == Some(call_id)
        && calls[0].name == tool_name
        && calls[0].args == arguments
        && event.tool_results().is_empty()
        && event.actions.tool_confirmation.is_none()
        && event.actions.tool_confirmation_decision.is_none()
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

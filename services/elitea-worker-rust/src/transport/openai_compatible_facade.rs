//! Hardened OpenAI-compatible model facade over the Elitea gateway for ADK-Rust 2.2.0.
//!
//! The stock ADK OpenAI-compatible provider owns an unrestricted HTTP client
//! and includes upstream response bodies in errors. The worker instead keeps a
//! shared origin-bound mTLS HTTP/2 channel, consumes one claim-scoped PAT into
//! one single-use model, bounds every request/SSE allocation, and exposes only
//! stable data-free failures. This module intentionally supports the first
//! ordinary text/no-tool profile only; production assembly remains disabled.

#![allow(dead_code)] // The production assembler lands after this transport proof.

use std::fmt;
use std::sync::atomic::{AtomicU32, Ordering};
use std::sync::{Arc, Mutex};
use std::time::Duration;

use adk_rust::{
    AdkError, Content, ErrorCategory, ErrorComponent, FinishReason, GenerateContentConfig, Llm,
    LlmRequest, LlmResponse, LlmResponseStream, Part,
};
use async_stream::try_stream;
use async_trait::async_trait;
use bytes::Bytes;
use http::header::{ACCEPT, AUTHORIZATION, CONTENT_LENGTH, CONTENT_TYPE};
use http::{HeaderMap, HeaderName, HeaderValue, Method, Request, Response, StatusCode, Version};
use http_body_util::{BodyExt as _, Full};
use tokio::time::timeout;
use tonic::body::Body;
use tonic::transport::{Certificate, Channel, ClientTlsConfig, Endpoint, Identity};
use tower::ServiceExt as _;
use tracing::Instrument as _;
use zeroize::Zeroizing;

use super::runtime_context::ClaimScopedEliteaContext;
use crate::agents::runtime::{NativeAgentAssemblyError, NativeAgentAssemblyErrorCode};
use crate::agents::session::{BoundOrdinaryAgentModel, DurableModelCompletion};

const MODEL_ROUTE: &str = "/llm/v1/chat/completions";
const MAX_ORIGIN_BYTES: usize = 2_048;
const MAX_MODEL_NAME_BYTES: usize = 256;
const MAX_INSTRUCTION_BYTES: usize = 64 * 1_024;
const MAX_REQUEST_BYTES: usize = 1_024 * 1_024;
const MAX_SSE_EVENT_BYTES: usize = 256 * 1_024;
const MAX_STREAM_BYTES: usize = 8 * 1_024 * 1_024;
const MAX_COMPLETION_BYTES: usize = 60 * 1_024;
const MAX_SSE_EVENTS: usize = 4_096;
const MAX_MODEL_TURNS: u32 = 1_024;
const MAX_TOOL_CALLS_PER_TURN: usize = 16;
const MAX_TOOL_NAME_BYTES: usize = 256;
const MAX_TOOL_CALL_ID_BYTES: usize = 512;
const MAX_TOOL_ARGUMENT_BYTES: usize = 256 * 1_024;
const MAX_TOOL_DECLARATIONS: usize = 1_024;
const MAX_TIMEOUT: Duration = Duration::from_mins(5);
// Main treats this as the project that owns the invocation and pays for it.
// It is deliberately NOT the frozen model owner: Bifrost resolves a shared
// public model inside the caller project's signed scope.
const PROJECT_SELECTOR: HeaderName = HeaderName::from_static("x-project-id");

/// Immutable deployment policy for the shared platform model channel.
#[derive(Clone, Debug, Eq, PartialEq)]
pub(crate) struct ModelGatewayConfig {
    pub(crate) origin: String,
    pub(crate) connect_timeout: Duration,
    pub(crate) response_header_timeout: Duration,
    pub(crate) stream_idle_timeout: Duration,
    pub(crate) max_request_bytes: usize,
    pub(crate) max_sse_event_bytes: usize,
    pub(crate) max_stream_bytes: usize,
    pub(crate) max_sse_events: usize,
}

impl ModelGatewayConfig {
    fn validate(&self) -> Result<String, ModelFacadeError> {
        let origin = canonical_https_origin(&self.origin)?;
        if !valid_timeout(self.connect_timeout)
            || !valid_timeout(self.response_header_timeout)
            || !valid_timeout(self.stream_idle_timeout)
            || self.max_request_bytes == 0
            || self.max_request_bytes > MAX_REQUEST_BYTES
            || self.max_sse_event_bytes == 0
            || self.max_sse_event_bytes > MAX_SSE_EVENT_BYTES
            || self.max_stream_bytes < self.max_sse_event_bytes
            || self.max_stream_bytes > MAX_STREAM_BYTES
            || self.max_sse_events == 0
            || self.max_sse_events > MAX_SSE_EVENTS
        {
            return Err(ModelFacadeError::InvalidConfiguration);
        }
        Ok(origin)
    }
}

/// Frozen generation controls admitted before the PAT reaches this module.
pub(crate) struct ModelFacadeInvocation {
    pub(crate) model_name: String,
    pub(crate) system_instruction: String,
    pub(crate) max_tokens: Option<u32>,
    pub(crate) reasoning_effort: Option<ModelReasoningEffort>,
    pub(crate) temperature: Option<f32>,
    pub(crate) max_model_turns: u32,
}

/// OpenAI-compatible reasoning control preserved from the frozen SDK input.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum ModelReasoningEffort {
    Low,
    Medium,
    High,
    None,
}

impl ModelReasoningEffort {
    const fn as_str(self) -> &'static str {
        match self {
            Self::Low => "low",
            Self::Medium => "medium",
            Self::High => "high",
            Self::None => "none",
        }
    }
}

/// Stable, data-free model-facade setup failure.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum ModelFacadeError {
    InvalidConfiguration,
    InvalidInvocation,
    ResourceExhausted,
    DependencyUnavailable,
}

impl ModelFacadeError {
    #[must_use]
    pub(crate) const fn code(self) -> &'static str {
        match self {
            Self::InvalidConfiguration => "model_gateway.invalid_configuration",
            Self::InvalidInvocation => "model_gateway.invalid_invocation",
            Self::ResourceExhausted => "model_gateway.resource_exhausted",
            Self::DependencyUnavailable => "model_gateway.dependency_unavailable",
        }
    }

    #[must_use]
    pub(crate) const fn retryable(self) -> bool {
        matches!(self, Self::DependencyUnavailable)
    }
}

impl fmt::Display for ModelFacadeError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(match self {
            Self::InvalidConfiguration => "the model facade configuration is malformed",
            Self::InvalidInvocation => "the model facade invocation is malformed",
            Self::ResourceExhausted => "the model gateway request exceeds its approved limit",
            Self::DependencyUnavailable => "the model gateway is unavailable",
        })
    }
}

impl std::error::Error for ModelFacadeError {}

#[async_trait]
pub(super) trait ModelGatewayTransport: Send + Sync {
    async fn post(
        &self,
        request: Request<Body>,
    ) -> Result<Response<Body>, ModelGatewayTransportError>;
}

#[derive(Clone)]
struct TonicModelGatewayTransport {
    channel: Channel,
}

#[async_trait]
impl ModelGatewayTransport for TonicModelGatewayTransport {
    async fn post(
        &self,
        request: Request<Body>,
    ) -> Result<Response<Body>, ModelGatewayTransportError> {
        self.channel
            .clone()
            .oneshot(request)
            .await
            .map_err(ModelGatewayTransportError::Tonic)
    }
}

pub(super) enum ModelGatewayTransportError {
    Unavailable,
    Tonic(tonic::transport::Error),
}

impl fmt::Debug for ModelGatewayTransportError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str("ModelGatewayTransportError(..)")
    }
}

impl fmt::Display for ModelGatewayTransportError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str("the model gateway transport is unavailable")
    }
}

impl std::error::Error for ModelGatewayTransportError {
    fn source(&self) -> Option<&(dyn std::error::Error + 'static)> {
        match self {
            Self::Unavailable => None,
            Self::Tonic(error) => Some(error),
        }
    }
}

/// Shared connection pool for claim-scoped model invocations.
pub(crate) struct ModelGatewayClient {
    pub(super) transport: Arc<dyn ModelGatewayTransport>,
    pub(super) config: ModelGatewayConfig,
}

impl ModelGatewayClient {
    /// Build one reusable mTLS HTTP/2 channel from the trusted platform origin.
    pub(crate) async fn connect(
        mut config: ModelGatewayConfig,
        private_ca: Certificate,
        client_identity: Identity,
    ) -> Result<Self, ModelFacadeError> {
        let origin = config.validate()?;
        let tls = ClientTlsConfig::new()
            .ca_certificate(private_ca)
            .identity(client_identity);
        let endpoint = Endpoint::from_shared(origin.clone())
            .and_then(|endpoint| endpoint.tls_config(tls))
            .map_err(|_| ModelFacadeError::InvalidConfiguration)?;
        let channel = timeout(config.connect_timeout, endpoint.connect())
            .await
            .map_err(|_| ModelFacadeError::DependencyUnavailable)?
            .map_err(|_| ModelFacadeError::DependencyUnavailable)?;
        config.origin = origin;
        Ok(Self {
            transport: Arc::new(TonicModelGatewayTransport { channel }),
            config,
        })
    }

    #[cfg(test)]
    fn with_rpc(
        transport: impl ModelGatewayTransport + 'static,
        mut config: ModelGatewayConfig,
    ) -> Result<Self, ModelFacadeError> {
        config.origin = config.validate()?;
        Ok(Self {
            transport: Arc::new(transport),
            config,
        })
    }

    /// Consume one ephemeral claim credential into one single-use ADK model.
    ///
    /// `model_owner_project_id` proves the frozen selection names an admitted
    /// project, but it never becomes the `/llm` billing selector. Main binds
    /// that selector to the claim's resource project; the Bifrost gateway then
    /// resolves either that project's model or a shared public fallback.
    pub(crate) fn bind_ordinary(
        &self,
        context: &ClaimScopedEliteaContext,
        model_owner_project_id: u32,
        invocation: ModelFacadeInvocation,
    ) -> Result<BoundOpenAiCompatibleFacade, ModelFacadeError> {
        validate_invocation(&invocation)?;
        let token = context.model_facade_token();
        let billing_project_id = context.resource_project_id();
        if model_owner_project_id == 0 || billing_project_id == 0 || token.is_empty() {
            return Err(ModelFacadeError::InvalidInvocation);
        }
        let completion = Arc::new(Mutex::new(CompletionState::default()));
        let model = Arc::new(EliteaOpenAiCompatibleModel {
            transport: self.transport.clone(),
            config: self.config.clone(),
            invocation,
            billing_project_id,
            token,
            completion: completion.clone(),
            calls: AtomicU32::new(0),
        });
        Ok(BoundOpenAiCompatibleFacade {
            model,
            completion: OpenAiCompatibleCompletion { state: completion },
        })
    }
}

/// Inseparable single-use model and its exact completion capture.
pub(crate) struct BoundOpenAiCompatibleFacade {
    model: Arc<EliteaOpenAiCompatibleModel>,
    completion: OpenAiCompatibleCompletion,
}

impl BoundOrdinaryAgentModel for BoundOpenAiCompatibleFacade {
    fn adk_model(&self) -> Arc<dyn Llm> {
        self.model.clone()
    }

    fn take_completed_text(self) -> Result<String, NativeAgentAssemblyError> {
        self.completion.take().map_err(model_completion_error)
    }

    fn durable_completion(&self) -> Option<Arc<dyn DurableModelCompletion>> {
        Some(self.completion.state.clone())
    }
}

fn model_completion_error(error: ModelFacadeError) -> NativeAgentAssemblyError {
    let code = match error {
        ModelFacadeError::InvalidConfiguration => {
            NativeAgentAssemblyErrorCode::InvalidConfiguration
        }
        ModelFacadeError::InvalidInvocation => NativeAgentAssemblyErrorCode::InvalidResult,
        ModelFacadeError::ResourceExhausted => NativeAgentAssemblyErrorCode::ResourceExhausted,
        ModelFacadeError::DependencyUnavailable => {
            NativeAgentAssemblyErrorCode::DependencyUnavailable
        }
    };
    NativeAgentAssemblyError::new(code, "the bound model completion is unavailable")
}

#[cfg(test)]
impl BoundOpenAiCompatibleFacade {
    pub(super) async fn generate_for_test(
        &self,
        request: LlmRequest,
    ) -> Result<LlmResponseStream, AdkError> {
        self.model.generate_content(request, true).await
    }

    pub(super) fn take_completion_for_test(self) -> Result<String, ModelFacadeError> {
        self.completion.take()
    }
}

#[derive(Default)]
struct CompletionState {
    value: Option<String>,
    consumed: bool,
}

struct OpenAiCompatibleCompletion {
    state: Arc<Mutex<CompletionState>>,
}

impl DurableModelCompletion for Mutex<CompletionState> {
    fn snapshot(&self) -> adk_rust::Result<Option<String>> {
        self.lock().map(|state| state.value.clone()).map_err(|_| {
            model_error(
                ErrorCategory::Internal,
                "model_gateway.completion_state",
                "the model completion state is unavailable",
            )
        })
    }
}

impl OpenAiCompatibleCompletion {
    fn take(self) -> Result<String, ModelFacadeError> {
        let mut state = self
            .state
            .lock()
            .map_err(|_| ModelFacadeError::DependencyUnavailable)?;
        if state.consumed {
            return Err(ModelFacadeError::InvalidInvocation);
        }
        let value = state
            .value
            .take()
            .ok_or(ModelFacadeError::InvalidInvocation)?;
        state.consumed = true;
        Ok(value)
    }
}

struct EliteaOpenAiCompatibleModel {
    transport: Arc<dyn ModelGatewayTransport>,
    config: ModelGatewayConfig,
    invocation: ModelFacadeInvocation,
    billing_project_id: u64,
    token: Zeroizing<String>,
    completion: Arc<Mutex<CompletionState>>,
    calls: AtomicU32,
}

#[async_trait]
impl Llm for EliteaOpenAiCompatibleModel {
    fn name(&self) -> &str {
        &self.invocation.model_name
    }

    async fn generate_content(
        &self,
        request: LlmRequest,
        stream: bool,
    ) -> Result<LlmResponseStream, AdkError> {
        let turn = self
            .calls
            .fetch_update(Ordering::AcqRel, Ordering::Acquire, |current| {
                (current < self.invocation.max_model_turns).then_some(current + 1)
            })
            .map(|current| current + 1)
            .map_err(|_| {
                model_error(
                    ErrorCategory::InvalidInput,
                    "model_gateway.turn_limit",
                    "the claim-scoped model invocation exceeded its approved turn limit",
                )
            })?;
        let span = tracing::info_span!(
            "agent.model.request",
            model_adapter = "openai_compatible",
            model_name = %self.invocation.model_name,
            billing_project_id = self.billing_project_id,
            turn,
            tool_count = request.tools.len(),
            streaming = stream,
            outcome = tracing::field::Empty,
            error_code = tracing::field::Empty,
            retryable = tracing::field::Empty,
        );
        let result: Result<LlmResponseStream, AdkError> = async {
            tracing::info!(event = "agent_model_request_started");
            let allowed_tools = request.tools.keys().cloned().collect();
            let body = build_request_body(&request, stream, &self.invocation, &self.config)?;
            let request = build_http_request(body, self.billing_project_id, self.token.as_str())?;
            let response = timeout(
                self.config.response_header_timeout,
                self.transport.post(request),
            )
            .await
            .map_err(|_| {
                model_error(
                    ErrorCategory::Timeout,
                    "model_gateway.response_header_timeout",
                    "the model gateway response header timed out",
                )
            })?
            .map_err(|_| {
                model_error(
                    ErrorCategory::Unavailable,
                    "model_gateway.transport",
                    "the model gateway transport is unavailable",
                )
            })?;
            validate_response_head(&response)?;
            Ok(model_response_stream(
                response,
                self.config.stream_idle_timeout,
                self.config.max_sse_event_bytes,
                self.config.max_stream_bytes,
                self.config.max_sse_events,
                allowed_tools,
                self.completion.clone(),
            ))
        }
        .instrument(span.clone())
        .await;
        match &result {
            Ok(_) => {
                span.record("outcome", "stream_opened");
                span.in_scope(|| tracing::info!(event = "agent_model_stream_opened"));
            }
            Err(error) => {
                span.record("outcome", "failed");
                span.record("error_code", error.code);
                span.record("retryable", error.is_retryable());
                span.in_scope(|| {
                    tracing::warn!(
                        event = "agent_model_request_failed",
                        error = %error,
                        error_code = error.code,
                        retryable = error.is_retryable(),
                    );
                });
            }
        }
        result
    }
}

pub(super) fn validate_invocation(
    invocation: &ModelFacadeInvocation,
) -> Result<(), ModelFacadeError> {
    if !bounded_header_text(&invocation.model_name, MAX_MODEL_NAME_BYTES)
        || invocation.system_instruction.len() > MAX_INSTRUCTION_BYTES
        || invocation.system_instruction.contains('\0')
        || invocation.max_tokens == Some(0)
        || invocation.max_model_turns == 0
        || invocation.max_model_turns > MAX_MODEL_TURNS
        || invocation
            .max_tokens
            .is_some_and(|value| i32::try_from(value).is_err())
        || (invocation
            .reasoning_effort
            .is_some_and(|effort| effort != ModelReasoningEffort::None)
            && invocation.temperature.is_some())
        || invocation
            .temperature
            .is_some_and(|value| !value.is_finite() || !(0.0..=1.0).contains(&value))
    {
        return Err(ModelFacadeError::InvalidInvocation);
    }
    Ok(())
}

fn build_request_body(
    request: &LlmRequest,
    stream: bool,
    invocation: &ModelFacadeInvocation,
    config: &ModelGatewayConfig,
) -> Result<Bytes, AdkError> {
    let contents = validate_llm_request(request, stream, invocation)?;
    let mut body = serde_json::Map::new();
    body.insert(
        "model".to_owned(),
        serde_json::Value::String(invocation.model_name.clone()),
    );
    let mut messages =
        Vec::with_capacity(contents.len() + usize::from(!invocation.system_instruction.is_empty()));
    if !invocation.system_instruction.is_empty() {
        messages.push(serde_json::json!({
            "role": instruction_role(&invocation.model_name),
            "content": invocation.system_instruction,
        }));
    }
    for (index, content) in contents.iter().enumerate() {
        append_openai_messages(content, index + 1 == contents.len(), &mut messages)?;
    }
    body.insert("messages".to_owned(), serde_json::Value::Array(messages));
    if !request.tools.is_empty() {
        body.insert("tools".to_owned(), openai_tools(&request.tools)?);
        body.insert(
            "tool_choice".to_owned(),
            serde_json::Value::String("auto".to_owned()),
        );
        // GPT-5.6 defaults to medium reasoning, but Chat Completions tool
        // calls require effective reasoning `none`. Match ADK 2.1+'s provider
        // contract when Main has not frozen an explicit effort.
        if invocation.model_name.starts_with("gpt-5.6") && invocation.reasoning_effort.is_none() {
            body.insert(
                "reasoning_effort".to_owned(),
                serde_json::Value::String("none".to_owned()),
            );
        }
    }
    body.insert("stream".to_owned(), serde_json::Value::Bool(true));
    body.insert(
        "stream_options".to_owned(),
        serde_json::json!({"include_usage": true}),
    );
    if let Some(max_tokens) = invocation.max_tokens {
        body.insert(
            "max_completion_tokens".to_owned(),
            serde_json::Value::from(max_tokens),
        );
    }
    if let Some(temperature) = invocation.temperature {
        body.insert(
            "temperature".to_owned(),
            serde_json::Value::from(temperature),
        );
    }
    if let Some(reasoning_effort) = invocation.reasoning_effort {
        body.insert(
            "reasoning_effort".to_owned(),
            serde_json::Value::String(reasoning_effort.as_str().to_owned()),
        );
    }
    let encoded = serde_json::to_vec(&body).map_err(|_| {
        model_error(
            ErrorCategory::InvalidInput,
            "model_gateway.request_encoding",
            "the model gateway request is malformed",
        )
    })?;
    if encoded.len() > config.max_request_bytes {
        return Err(model_error(
            ErrorCategory::InvalidInput,
            "model_gateway.request_too_large",
            "the model gateway request exceeds its approved limit",
        ));
    }
    Ok(Bytes::from(encoded))
}

pub(super) fn validate_llm_request<'a>(
    request: &'a LlmRequest,
    stream: bool,
    invocation: &ModelFacadeInvocation,
) -> Result<&'a [Content], AdkError> {
    let config = request.config.as_ref().ok_or_else(invalid_llm_request)?;
    if !stream
        || request.model != invocation.model_name
        || request.tools.len() > MAX_TOOL_DECLARATIONS
        || request.previous_response_id.is_some()
        || request.contents.is_empty()
        || !generation_config_matches(config, invocation)
    {
        return Err(invalid_llm_request());
    }
    let Some(current) = request.contents.last() else {
        return Err(invalid_llm_request());
    };
    if !matches!(current.role.as_str(), "user" | "function" | "tool") {
        return Err(invalid_llm_request());
    }
    for content in &request.contents {
        validate_openai_content(content)?;
    }
    for (name, declaration) in &request.tools {
        validate_tool_declaration(name, declaration)?;
    }
    Ok(&request.contents)
}

fn validate_openai_content(content: &Content) -> Result<(), AdkError> {
    if content.parts.is_empty() {
        return Err(invalid_llm_request());
    }
    match content.role.as_str() {
        "user" => content.parts.iter().try_for_each(|part| match part {
            Part::Text { text } if valid_part_text(text) => Ok(()),
            _ => Err(invalid_llm_request()),
        }),
        "model" | "assistant" => content.parts.iter().try_for_each(|part| match part {
            Part::Text { text } | Part::Thinking { thinking: text, .. }
                if valid_part_text(text) =>
            {
                Ok(())
            }
            Part::FunctionCall { name, args, id, .. }
                // Historical calls do not grant current execution authority.
                // Authorization can replace a proxy with protected operations.
                // New response calls still require a current tool declaration.
                if valid_tool_name(name)
                    && id.as_deref().is_some_and(valid_tool_call_id)
                    && args.is_object()
                    && serde_json::to_vec(args)
                        .is_ok_and(|encoded| encoded.len() <= MAX_TOOL_ARGUMENT_BYTES) =>
            {
                Ok(())
            }
            _ => Err(invalid_llm_request()),
        }),
        "function" | "tool" => content.parts.iter().try_for_each(|part| match part {
            Part::FunctionResponse {
                function_response,
                id: Some(id),
                ..
            } if valid_tool_name(&function_response.name)
                && valid_tool_call_id(id)
                && serde_json::to_vec(&function_response.response)
                    .is_ok_and(|encoded| encoded.len() <= MAX_TOOL_ARGUMENT_BYTES) =>
            {
                Ok(())
            }
            _ => Err(invalid_llm_request()),
        }),
        _ => Err(invalid_llm_request()),
    }
}

fn valid_part_text(value: &str) -> bool {
    !value.is_empty() && value.len() <= MAX_COMPLETION_BYTES && !value.contains('\0')
}

fn validate_tool_declaration(name: &str, declaration: &serde_json::Value) -> Result<(), AdkError> {
    if !valid_tool_name(name) {
        return Err(invalid_llm_request());
    }
    let object = declaration.as_object().ok_or_else(invalid_llm_request)?;
    if object.len() > 4
        || object.keys().any(|key| {
            !matches!(
                key.as_str(),
                "name" | "description" | "parameters" | "response"
            )
        })
        || object
            .get("name")
            .is_some_and(|value| value.as_str() != Some(name))
        || object
            .get("description")
            .is_some_and(|value| value.as_str().is_none_or(|text| !valid_part_text(text)))
        || object
            .get("parameters")
            .is_some_and(|value| !value.is_object())
        || object
            .get("response")
            .is_some_and(|value| !value.is_object())
    {
        return Err(invalid_llm_request());
    }
    Ok(())
}

fn append_openai_messages(
    content: &Content,
    is_last: bool,
    messages: &mut Vec<serde_json::Value>,
) -> Result<(), AdkError> {
    match content.role.as_str() {
        "user" => {
            let value = if is_last && content.parts.len() == 1 {
                let Part::Text { text } = &content.parts[0] else {
                    return Err(invalid_llm_request());
                };
                serde_json::Value::String(text.clone())
            } else {
                serde_json::Value::Array(
                    content
                        .parts
                        .iter()
                        .map(|part| match part {
                            Part::Text { text } => {
                                Ok(serde_json::json!({"type": "text", "text": text}))
                            }
                            _ => Err(invalid_llm_request()),
                        })
                        .collect::<Result<Vec<_>, _>>()?,
                )
            };
            messages.push(serde_json::json!({"role": "user", "content": value}));
        }
        "model" | "assistant" => messages.push(openai_assistant_message(content)?),
        "function" | "tool" => {
            for part in &content.parts {
                let Part::FunctionResponse {
                    function_response,
                    id: Some(id),
                    ..
                } = part
                else {
                    return Err(invalid_llm_request());
                };
                let result = serde_json::to_string(&function_response.response)
                    .map_err(|_| invalid_llm_request())?;
                messages.push(serde_json::json!({
                    "role": "tool",
                    "tool_call_id": id,
                    "content": result,
                }));
            }
        }
        _ => return Err(invalid_llm_request()),
    }
    Ok(())
}

fn openai_assistant_message(content: &Content) -> Result<serde_json::Value, AdkError> {
    let mut text = String::new();
    let mut text_blocks = Vec::new();
    let mut reasoning = String::new();
    let mut calls = Vec::new();
    for part in &content.parts {
        match part {
            Part::Text { text: value } => {
                text.push_str(value);
                text_blocks.push(serde_json::json!({"type": "text", "text": value}));
            }
            Part::Thinking {
                thinking: value, ..
            } => reasoning.push_str(value),
            Part::FunctionCall {
                name,
                args,
                id: Some(id),
                ..
            } => calls.push(serde_json::json!({
                "id": id,
                "type": "function",
                "function": {"name": name, "arguments": serde_json::to_string(args).map_err(|_| invalid_llm_request())?},
            })),
            _ => return Err(invalid_llm_request()),
        }
    }
    let mut message = serde_json::Map::new();
    message.insert(
        "role".to_owned(),
        serde_json::Value::String("assistant".to_owned()),
    );
    message.insert(
        "content".to_owned(),
        if calls.is_empty() && reasoning.is_empty() && !text_blocks.is_empty() {
            serde_json::Value::Array(text_blocks)
        } else if text.is_empty() {
            serde_json::Value::Null
        } else {
            serde_json::Value::String(text)
        },
    );
    if !reasoning.is_empty() {
        message.insert(
            "reasoning_content".to_owned(),
            serde_json::Value::String(reasoning),
        );
    }
    if !calls.is_empty() {
        message.insert("tool_calls".to_owned(), serde_json::Value::Array(calls));
    }
    Ok(serde_json::Value::Object(message))
}

fn openai_tools(
    tools: &std::collections::HashMap<String, serde_json::Value>,
) -> Result<serde_json::Value, AdkError> {
    let mut names = tools.keys().collect::<Vec<_>>();
    names.sort_unstable();
    let values = names
        .into_iter()
        .map(|name| {
            let declaration = tools
                .get(name)
                .and_then(serde_json::Value::as_object)
                .ok_or_else(invalid_llm_request)?;
            let mut function = serde_json::Map::new();
            function.insert("name".to_owned(), serde_json::Value::String(name.clone()));
            if let Some(description) = declaration.get("description") {
                function.insert("description".to_owned(), description.clone());
            }
            function.insert(
                "parameters".to_owned(),
                declaration
                    .get("parameters")
                    .cloned()
                    .unwrap_or_else(|| serde_json::json!({"type": "object", "properties": {}})),
            );
            Ok(serde_json::json!({"type": "function", "function": function}))
        })
        .collect::<Result<Vec<_>, AdkError>>()?;
    Ok(serde_json::Value::Array(values))
}

pub(super) fn valid_tool_name(value: &str) -> bool {
    bounded_header_text(value, MAX_TOOL_NAME_BYTES)
}

pub(super) fn valid_tool_call_id(value: &str) -> bool {
    bounded_header_text(value, MAX_TOOL_CALL_ID_BYTES)
}

fn instruction_role(model_name: &str) -> &'static str {
    let bytes = model_name.as_bytes();
    if bytes.len() >= 2 && bytes[0] == b'o' && bytes[1].is_ascii_digit() {
        "developer"
    } else {
        "system"
    }
}

fn generation_config_matches(
    config: &GenerateContentConfig,
    invocation: &ModelFacadeInvocation,
) -> bool {
    config.temperature == invocation.temperature
        && config.max_output_tokens
            == invocation
                .max_tokens
                .and_then(|value| i32::try_from(value).ok())
        && config.top_p.is_none()
        && config.top_k.is_none()
        && config.frequency_penalty.is_none()
        && config.presence_penalty.is_none()
        && config.seed.is_none()
        && config.top_logprobs.is_none()
        && config.stop_sequences.is_empty()
        && config.response_schema.is_none()
        && config.cached_content.is_none()
        && config.extensions.is_empty()
}

fn invalid_llm_request() -> AdkError {
    model_error(
        ErrorCategory::InvalidInput,
        "model_gateway.invalid_request",
        "the ADK model request is outside the admitted ordinary profile",
    )
}

fn build_http_request(
    body: Bytes,
    billing_project_id: u64,
    token: &str,
) -> Result<Request<Body>, AdkError> {
    let body_length = body.len();
    let mut request = Request::builder()
        .method(Method::POST)
        .uri(MODEL_ROUTE)
        .version(Version::HTTP_2)
        .body(Body::new(Full::new(body)))
        .map_err(|_| invalid_llm_request())?;
    let headers = request.headers_mut();
    headers.insert(CONTENT_TYPE, HeaderValue::from_static("application/json"));
    headers.insert(ACCEPT, HeaderValue::from_static("text/event-stream"));
    headers.insert(
        CONTENT_LENGTH,
        HeaderValue::from_str(&body_length.to_string()).map_err(|_| invalid_llm_request())?,
    );
    headers.insert(
        PROJECT_SELECTOR,
        HeaderValue::from_str(&billing_project_id.to_string())
            .map_err(|_| invalid_llm_request())?,
    );
    let mut bearer = Zeroizing::new(String::with_capacity(7 + token.len()));
    bearer.push_str("Bearer ");
    bearer.push_str(token);
    let mut authorization =
        HeaderValue::from_str(bearer.as_str()).map_err(|_| invalid_llm_request())?;
    authorization.set_sensitive(true);
    headers.insert(AUTHORIZATION, authorization);
    Ok(request)
}

pub(super) fn validate_response_head(response: &Response<Body>) -> Result<(), AdkError> {
    if response.version() != Version::HTTP_2 {
        return Err(model_error(
            ErrorCategory::Unavailable,
            "model_gateway.http_version",
            "the model gateway did not negotiate HTTP/2",
        ));
    }
    match response.status() {
        StatusCode::UNAUTHORIZED => {
            return Err(model_error(
                ErrorCategory::Unauthorized,
                "model_gateway.unauthorized",
                "the model gateway rejected the execution credential",
            ));
        }
        StatusCode::FORBIDDEN => {
            return Err(model_error(
                ErrorCategory::Forbidden,
                "model_gateway.forbidden",
                "the model gateway rejected the execution project",
            ));
        }
        StatusCode::REQUEST_TIMEOUT | StatusCode::GATEWAY_TIMEOUT => {
            return Err(model_error(
                ErrorCategory::Timeout,
                "model_gateway.upstream_timeout",
                "the model gateway timed out",
            ));
        }
        StatusCode::TOO_MANY_REQUESTS => {
            return Err(model_error(
                ErrorCategory::RateLimited,
                "model_gateway.rate_limited",
                "the model gateway rate limit was reached",
            ));
        }
        StatusCode::PAYMENT_REQUIRED => {
            return Err(model_error(
                ErrorCategory::InvalidInput,
                "model_gateway.budget_exhausted",
                "the model budget is exhausted",
            ));
        }
        StatusCode::CONFLICT => {
            return Err(model_error(
                ErrorCategory::Unavailable,
                "model_gateway.conflict",
                "the model gateway reported a retryable conflict",
            ));
        }
        status if status.is_redirection() || status.is_server_error() => {
            return Err(model_error(
                ErrorCategory::Unavailable,
                "model_gateway.unavailable",
                "the model gateway is unavailable",
            ));
        }
        status if !status.is_success() => {
            return Err(model_error(
                ErrorCategory::InvalidInput,
                "model_gateway.rejected",
                "the model gateway rejected the admitted request",
            ));
        }
        _ => {}
    }
    let media_type = single_header(response.headers(), &CONTENT_TYPE)?;
    if !media_type
        .split(';')
        .next()
        .is_some_and(|value| value.trim().eq_ignore_ascii_case("text/event-stream"))
    {
        return Err(model_error(
            ErrorCategory::Unavailable,
            "model_gateway.response_type",
            "the model gateway response type is malformed",
        ));
    }
    Ok(())
}

fn single_header<'a>(headers: &'a HeaderMap, name: &HeaderName) -> Result<&'a str, AdkError> {
    let mut values = headers.get_all(name).iter();
    let value = values.next().ok_or_else(|| {
        model_error(
            ErrorCategory::Unavailable,
            "model_gateway.response_header_missing",
            "the model gateway response metadata is missing",
        )
    })?;
    if values.next().is_some() {
        return Err(model_error(
            ErrorCategory::Unavailable,
            "model_gateway.response_header_ambiguous",
            "the model gateway response metadata is ambiguous",
        ));
    }
    value.to_str().map_err(|_| {
        model_error(
            ErrorCategory::Unavailable,
            "model_gateway.response_header_invalid",
            "the model gateway response metadata is malformed",
        )
    })
}

fn model_response_stream(
    mut response: Response<Body>,
    idle_timeout: Duration,
    max_event_bytes: usize,
    max_stream_bytes: usize,
    max_events: usize,
    allowed_tools: std::collections::HashSet<String>,
    completion: Arc<Mutex<CompletionState>>,
) -> LlmResponseStream {
    Box::pin(try_stream! {
        let mut parser = SseParser::new(max_event_bytes, max_stream_bytes, max_events);
        let mut state = OpenAiStreamState::new(allowed_tools);
        let mut saw_done = false;
        loop {
            let chunk = next_response_chunk(&mut response, idle_timeout).await?;
            let input_finished = chunk.is_none();
            if let Some(chunk) = chunk {
                parser.push(&chunk)?;
            } else {
                parser.finish_input();
            }
            while let Some(event) = parser.next_event()? {
                if event.event_type.is_some() {
                    Err(invalid_sse())?;
                }
                match parse_sse_event(&event.data)? {
                    ParsedSseEvent::Delta(delta) => {
                        if saw_done || state.is_terminal() {
                            Err(model_error(
                                ErrorCategory::Unavailable,
                                "model_gateway.event_after_completion",
                                "the model gateway emitted an event after completion",
                            ))?;
                        }
                        if let Some(model_response) = state.apply(delta)? {
                            yield model_response;
                        }
                    }
                    ParsedSseEvent::Done => {
                        if saw_done || !state.is_terminal() {
                            Err(model_error(
                                ErrorCategory::Unavailable,
                                "model_gateway.done_before_completion",
                                "the model gateway ended before a completed response",
                            ))?;
                        }
                        saw_done = true;
                    }
                    ParsedSseEvent::Usage if !saw_done => {}
                    ParsedSseEvent::Usage => Err(model_error(
                        ErrorCategory::Unavailable,
                        "model_gateway.event_after_completion",
                        "the model gateway emitted an event after completion",
                    ))?,
                }
            }
            if input_finished {
                break;
            }
        }
        if !saw_done {
            Err(model_error(
                ErrorCategory::Unavailable,
                "model_gateway.incomplete_stream",
                "the model gateway stream ended before completion",
            ))?;
        }
        let (terminal, completed_text) = state.finish()?;
        if let Some(mut completed_text) = completed_text {
            record_completion(&mut completed_text, &completion)?;
        }
        yield terminal;
    })
}

pub(super) async fn next_response_chunk(
    response: &mut Response<Body>,
    idle_timeout: Duration,
) -> Result<Option<Bytes>, AdkError> {
    let Some(frame) = timeout(idle_timeout, response.body_mut().frame())
        .await
        .map_err(|_| {
            model_error(
                ErrorCategory::Timeout,
                "model_gateway.stream_idle_timeout",
                "the model gateway stream became idle",
            )
        })?
    else {
        return Ok(None);
    };
    let frame = frame.map_err(|_| {
        model_error(
            ErrorCategory::Unavailable,
            "model_gateway.stream_transport",
            "the model gateway stream was interrupted",
        )
    })?;
    frame.into_data().map(Some).map_err(|_| {
        model_error(
            ErrorCategory::Unavailable,
            "model_gateway.stream_trailers",
            "the model gateway stream contains unexpected trailers",
        )
    })
}

fn record_response(
    response: &LlmResponse,
    accumulated: &mut String,
    semantic_bytes: &mut usize,
) -> Result<(), AdkError> {
    if let Some(content) = &response.content {
        for part in &content.parts {
            let part_bytes = match part {
                Part::Text { text } => text.len(),
                Part::Thinking { thinking, .. } => thinking.len(),
                _ => 0,
            };
            *semantic_bytes = semantic_bytes
                .checked_add(part_bytes)
                .ok_or_else(model_output_too_large)?;
            if *semantic_bytes > MAX_COMPLETION_BYTES {
                return Err(model_output_too_large());
            }
            if let Part::Text { text } = part {
                accumulated.push_str(text);
            }
        }
    }
    Ok(())
}

fn record_completion(
    accumulated: &mut String,
    completion: &Arc<Mutex<CompletionState>>,
) -> Result<(), AdkError> {
    let mut state = completion.lock().map_err(|_| {
        model_error(
            ErrorCategory::Internal,
            "model_gateway.completion_state",
            "the model completion state is unavailable",
        )
    })?;
    if state.value.is_some() || state.consumed {
        return Err(model_error(
            ErrorCategory::Internal,
            "model_gateway.completion_reused",
            "the model completion was produced more than once",
        ));
    }
    state.value = Some(std::mem::take(accumulated));
    Ok(())
}

fn model_output_too_large() -> AdkError {
    model_error(
        ErrorCategory::InvalidInput,
        "model_gateway.output_too_large",
        "the model output exceeds its approved limit",
    )
}

pub(super) struct BoundedSseEvent {
    pub(super) event_type: Option<Vec<u8>>,
    pub(super) data: Vec<u8>,
}

pub(super) struct SseParser {
    bytes: Vec<u8>,
    cursor: usize,
    event_type: Option<Vec<u8>>,
    data: Vec<u8>,
    total_bytes: usize,
    max_event_bytes: usize,
    max_stream_bytes: usize,
    max_events: usize,
    emitted_events: usize,
    input_finished: bool,
}

impl SseParser {
    pub(super) fn new(max_event_bytes: usize, max_stream_bytes: usize, max_events: usize) -> Self {
        Self {
            bytes: Vec::new(),
            cursor: 0,
            event_type: None,
            data: Vec::new(),
            total_bytes: 0,
            max_event_bytes,
            max_stream_bytes,
            max_events,
            emitted_events: 0,
            input_finished: false,
        }
    }

    pub(super) fn push(&mut self, chunk: &[u8]) -> Result<(), AdkError> {
        if self.input_finished {
            return Err(invalid_sse());
        }
        self.total_bytes = self
            .total_bytes
            .checked_add(chunk.len())
            .ok_or_else(stream_too_large)?;
        if self.total_bytes > self.max_stream_bytes {
            return Err(stream_too_large());
        }
        let buffered = self.bytes.len().saturating_sub(self.cursor);
        if buffered.saturating_add(chunk.len()) > self.max_stream_bytes {
            return Err(stream_too_large());
        }
        self.compact();
        self.bytes.extend_from_slice(chunk);
        Ok(())
    }

    pub(super) fn finish_input(&mut self) {
        if self.input_finished {
            return;
        }
        self.input_finished = true;
        if self.bytes.len() > self.cursor && !self.bytes[self.cursor..].ends_with(b"\n") {
            self.bytes.push(b'\n');
        }
        self.bytes.push(b'\n');
    }

    pub(super) fn next_event(&mut self) -> Result<Option<BoundedSseEvent>, AdkError> {
        loop {
            let Some(relative_end) = self.bytes[self.cursor..]
                .iter()
                .position(|byte| *byte == b'\n')
            else {
                return Ok(None);
            };
            let end = self.cursor + relative_end;
            let mut line = &self.bytes[self.cursor..end];
            if line.last() == Some(&b'\r') {
                line = &line[..line.len() - 1];
            }
            if line.len() > self.max_event_bytes {
                return Err(event_too_large());
            }
            self.cursor = end + 1;
            if line.is_empty() {
                if self.data.is_empty() {
                    if self.event_type.is_some() {
                        return Err(invalid_sse());
                    }
                    self.compact();
                    continue;
                }
                if self.data.last() == Some(&b'\n') {
                    self.data.pop();
                }
                let event = std::mem::take(&mut self.data);
                let event_type = self.event_type.take();
                self.emitted_events = self
                    .emitted_events
                    .checked_add(1)
                    .ok_or_else(too_many_events)?;
                if self.emitted_events > self.max_events {
                    return Err(too_many_events());
                }
                self.compact();
                return Ok(Some(BoundedSseEvent {
                    event_type,
                    data: event,
                }));
            }
            if line.starts_with(b":") {
                continue;
            }
            if let Some(value) = line.strip_prefix(b"event:") {
                if self.event_type.is_some() || !self.data.is_empty() {
                    return Err(invalid_sse());
                }
                let value = value.strip_prefix(b" ").unwrap_or(value);
                if value.is_empty() || value.len() > self.max_event_bytes {
                    return Err(invalid_sse());
                }
                self.event_type = Some(value.to_vec());
                continue;
            }
            let Some(value) = line.strip_prefix(b"data:") else {
                return Err(invalid_sse());
            };
            let value = value.strip_prefix(b" ").unwrap_or(value);
            let next_len = self
                .data
                .len()
                .checked_add(value.len() + 1)
                .and_then(|length| length.checked_add(self.event_type.as_ref().map_or(0, Vec::len)))
                .ok_or_else(event_too_large)?;
            if next_len > self.max_event_bytes {
                return Err(event_too_large());
            }
            self.data.extend_from_slice(value);
            self.data.push(b'\n');
        }
    }

    fn compact(&mut self) {
        if self.cursor == self.bytes.len() {
            self.bytes.clear();
            self.cursor = 0;
        } else if self.cursor >= 64 * 1_024 {
            self.bytes.drain(..self.cursor);
            self.cursor = 0;
        }
    }
}

enum ParsedSseEvent {
    Delta(OpenAiDelta),
    Usage,
    Done,
}

struct OpenAiDelta {
    content: Option<String>,
    reasoning: Option<String>,
    tool_calls: Vec<OpenAiToolDelta>,
    finish: Option<OpenAiFinish>,
}

struct OpenAiToolDelta {
    index: usize,
    id: Option<String>,
    name: Option<String>,
    arguments: Option<String>,
}

#[derive(Clone, Copy)]
enum OpenAiFinish {
    Stop,
    MaxTokens,
    Safety,
    ToolCalls,
    Other,
}

fn parse_sse_event(bytes: &[u8]) -> Result<ParsedSseEvent, AdkError> {
    if bytes == b"[DONE]" {
        return Ok(ParsedSseEvent::Done);
    }
    let value: serde_json::Value = serde_json::from_slice(bytes).map_err(|_| invalid_sse())?;
    let object = value.as_object().ok_or_else(invalid_sse)?;
    if object.get("error").is_some_and(|error| !error.is_null()) {
        return Err(model_error(
            ErrorCategory::Unavailable,
            "model_gateway.provider_error",
            "the model provider reported an error",
        ));
    }
    let choices = object
        .get("choices")
        .and_then(serde_json::Value::as_array)
        .ok_or_else(invalid_sse)?;
    if choices.is_empty() {
        return if object.get("usage").is_some_and(|usage| !usage.is_null()) {
            Ok(ParsedSseEvent::Usage)
        } else {
            Err(invalid_sse())
        };
    }
    if choices.len() != 1 {
        return Err(invalid_sse());
    }
    let choice = choices[0].as_object().ok_or_else(invalid_sse)?;
    let delta = choice
        .get("delta")
        .and_then(serde_json::Value::as_object)
        .ok_or_else(invalid_sse)?;
    let content = optional_string(delta.get("content"))?.map(str::to_owned);
    let reasoning_content = optional_string(delta.get("reasoning_content"))?;
    let reasoning = optional_string(delta.get("reasoning"))?;
    if reasoning_content.is_some() && reasoning.is_some() {
        return Err(invalid_sse());
    }
    let reasoning = reasoning_content.or(reasoning).map(str::to_owned);
    let tool_calls = parse_tool_deltas(delta)?;
    let finish_reason = optional_string(choice.get("finish_reason"))?;
    if finish_reason.is_none() && content.is_none() && reasoning.is_none() && tool_calls.is_empty()
    {
        return Ok(ParsedSseEvent::Usage);
    }
    let finish_reason = finish_reason.map(parse_finish_reason).transpose()?;
    Ok(ParsedSseEvent::Delta(OpenAiDelta {
        content,
        reasoning,
        tool_calls,
        finish: finish_reason,
    }))
}

fn parse_tool_deltas(
    delta: &serde_json::Map<String, serde_json::Value>,
) -> Result<Vec<OpenAiToolDelta>, AdkError> {
    if delta
        .get("function_call")
        .is_some_and(|value| !value.is_null())
    {
        return Err(model_error(
            ErrorCategory::Unsupported,
            "model_gateway.legacy_function_call",
            "the model returned an unsupported legacy function call",
        ));
    }
    let Some(value) = delta.get("tool_calls") else {
        return Ok(Vec::new());
    };
    if value.is_null() {
        return Ok(Vec::new());
    }
    let calls = value.as_array().ok_or_else(invalid_sse)?;
    if calls.len() > MAX_TOOL_CALLS_PER_TURN {
        return Err(model_output_too_large());
    }
    calls
        .iter()
        .map(|call| {
            let call = call.as_object().ok_or_else(invalid_sse)?;
            let index = call
                .get("index")
                .and_then(serde_json::Value::as_u64)
                .and_then(|value| usize::try_from(value).ok())
                .filter(|value| *value < MAX_TOOL_CALLS_PER_TURN)
                .ok_or_else(invalid_sse)?;
            if call
                .get("type")
                .is_some_and(|value| value.as_str() != Some("function"))
            {
                return Err(invalid_sse());
            }
            let function = call
                .get("function")
                .and_then(serde_json::Value::as_object)
                .ok_or_else(invalid_sse)?;
            Ok(OpenAiToolDelta {
                index,
                id: optional_string(call.get("id"))?.map(str::to_owned),
                name: optional_string(function.get("name"))?.map(str::to_owned),
                arguments: optional_string(function.get("arguments"))?.map(str::to_owned),
            })
        })
        .collect()
}

fn optional_string(value: Option<&serde_json::Value>) -> Result<Option<&str>, AdkError> {
    match value {
        None | Some(serde_json::Value::Null) => Ok(None),
        Some(serde_json::Value::String(value)) => Ok(Some(value)),
        Some(_) => Err(invalid_sse()),
    }
}

fn parse_finish_reason(value: &str) -> Result<OpenAiFinish, AdkError> {
    match value {
        "stop" => Ok(OpenAiFinish::Stop),
        "length" => Ok(OpenAiFinish::MaxTokens),
        "content_filter" => Ok(OpenAiFinish::Safety),
        "tool_calls" => Ok(OpenAiFinish::ToolCalls),
        "function_call" => Err(model_error(
            ErrorCategory::Unsupported,
            "model_gateway.legacy_function_call",
            "the model returned an unsupported legacy function call",
        )),
        _ => Ok(OpenAiFinish::Other),
    }
}

#[derive(Default)]
struct OpenAiToolCallBuilder {
    id: Option<String>,
    name: String,
    arguments: String,
}

struct OpenAiStreamState {
    allowed_tools: std::collections::HashSet<String>,
    tool_calls: Vec<Option<OpenAiToolCallBuilder>>,
    accumulated_text: String,
    semantic_bytes: usize,
    terminal: Option<LlmResponse>,
    completed_text: Option<String>,
}

impl OpenAiStreamState {
    fn new(allowed_tools: std::collections::HashSet<String>) -> Self {
        Self {
            allowed_tools,
            tool_calls: Vec::new(),
            accumulated_text: String::new(),
            semantic_bytes: 0,
            terminal: None,
            completed_text: None,
        }
    }

    const fn is_terminal(&self) -> bool {
        self.terminal.is_some()
    }

    fn apply(&mut self, delta: OpenAiDelta) -> Result<Option<LlmResponse>, AdkError> {
        if self.terminal.is_some() {
            return Err(invalid_sse());
        }
        let mut parts = Vec::with_capacity(delta.tool_calls.len().saturating_add(2));
        if let Some(reasoning) = delta.reasoning.filter(|value| !value.is_empty()) {
            self.add_semantic_bytes(reasoning.len())?;
            parts.push(Part::Thinking {
                thinking: reasoning,
                signature: None,
            });
        }
        if let Some(content) = delta.content.filter(|value| !value.is_empty()) {
            self.add_semantic_bytes(content.len())?;
            self.accumulated_text.push_str(&content);
            parts.push(Part::Text { text: content });
        }
        for call in delta.tool_calls {
            self.apply_tool_delta(call)?;
        }
        let Some(finish) = delta.finish else {
            return Ok((!parts.is_empty()).then(|| LlmResponse {
                content: Some(Content {
                    role: "model".to_owned(),
                    parts,
                }),
                partial: true,
                ..LlmResponse::default()
            }));
        };

        let has_tool_calls = matches!(finish, OpenAiFinish::ToolCalls);
        if has_tool_calls {
            parts.extend(self.finish_tool_calls()?);
        } else if self.tool_calls.iter().any(Option::is_some) {
            return Err(invalid_sse());
        }
        let finish_reason = match finish {
            OpenAiFinish::Stop | OpenAiFinish::ToolCalls => FinishReason::Stop,
            OpenAiFinish::MaxTokens => FinishReason::MaxTokens,
            OpenAiFinish::Safety => FinishReason::Safety,
            OpenAiFinish::Other => FinishReason::Other,
        };
        let completed_text = std::mem::take(&mut self.accumulated_text);
        self.completed_text = (!has_tool_calls).then_some(completed_text);
        self.terminal = Some(LlmResponse {
            content: (!parts.is_empty()).then_some(Content {
                role: "model".to_owned(),
                parts,
            }),
            finish_reason: Some(finish_reason),
            partial: false,
            // A function call closes this provider response, not the agent
            // turn: ADK must execute the tool and ask the model again.
            turn_complete: !has_tool_calls,
            ..LlmResponse::default()
        });
        Ok(None)
    }

    fn add_semantic_bytes(&mut self, bytes: usize) -> Result<(), AdkError> {
        self.semantic_bytes = self
            .semantic_bytes
            .checked_add(bytes)
            .ok_or_else(model_output_too_large)?;
        if self.semantic_bytes > MAX_COMPLETION_BYTES {
            return Err(model_output_too_large());
        }
        Ok(())
    }

    fn apply_tool_delta(&mut self, delta: OpenAiToolDelta) -> Result<(), AdkError> {
        if delta.index >= MAX_TOOL_CALLS_PER_TURN {
            return Err(model_output_too_large());
        }
        if self.tool_calls.len() <= delta.index {
            self.tool_calls.resize_with(delta.index + 1, || None);
        }
        let builder = self.tool_calls[delta.index].get_or_insert_with(Default::default);
        if let Some(id) = delta.id {
            if !valid_tool_call_id(&id)
                || builder.id.as_ref().is_some_and(|existing| existing != &id)
            {
                return Err(invalid_sse());
            }
            builder.id = Some(id);
        }
        if let Some(name) = delta.name {
            builder.name.push_str(&name);
            if !valid_tool_name(&builder.name) {
                return Err(invalid_sse());
            }
        }
        if let Some(arguments) = delta.arguments {
            if builder.arguments.len().saturating_add(arguments.len()) > MAX_TOOL_ARGUMENT_BYTES {
                return Err(model_output_too_large());
            }
            builder.arguments.push_str(&arguments);
        }
        Ok(())
    }

    fn finish_tool_calls(&mut self) -> Result<Vec<Part>, AdkError> {
        if self.tool_calls.is_empty()
            || self.tool_calls.len() > MAX_TOOL_CALLS_PER_TURN
            || self.tool_calls.iter().any(Option::is_none)
        {
            return Err(invalid_sse());
        }
        let mut ids = std::collections::HashSet::with_capacity(self.tool_calls.len());
        self.tool_calls
            .iter_mut()
            .map(|builder| {
                let builder = builder.take().ok_or_else(invalid_sse)?;
                let id = builder
                    .id
                    .filter(|id| ids.insert(id.clone()))
                    .ok_or_else(invalid_sse)?;
                if !self.allowed_tools.contains(&builder.name) {
                    return Err(model_error(
                        ErrorCategory::Unsupported,
                        super::model_facade::TOOL_NOT_ADMITTED_CODE,
                        "the model requested a tool outside the admitted toolset",
                    ));
                }
                let args: serde_json::Value =
                    serde_json::from_str(&builder.arguments).map_err(|_| invalid_sse())?;
                if !args.is_object() {
                    return Err(invalid_sse());
                }
                Ok(Part::FunctionCall {
                    name: builder.name,
                    args,
                    id: Some(id),
                    thought_signature: None,
                })
            })
            .collect()
    }

    fn finish(mut self) -> Result<(LlmResponse, Option<String>), AdkError> {
        let terminal = self.terminal.take().ok_or_else(|| {
            model_error(
                ErrorCategory::Unavailable,
                "model_gateway.incomplete_stream",
                "the model gateway stream ended before completion",
            )
        })?;
        Ok((terminal, self.completed_text.take()))
    }
}

fn invalid_sse() -> AdkError {
    model_error(
        ErrorCategory::Unavailable,
        "model_gateway.invalid_sse",
        "the model gateway stream is malformed",
    )
}

fn event_too_large() -> AdkError {
    model_error(
        ErrorCategory::InvalidInput,
        "model_gateway.event_too_large",
        "the model gateway event exceeds its approved limit",
    )
}

fn stream_too_large() -> AdkError {
    model_error(
        ErrorCategory::InvalidInput,
        "model_gateway.stream_too_large",
        "the model gateway stream exceeds its approved limit",
    )
}

fn too_many_events() -> AdkError {
    model_error(
        ErrorCategory::Unavailable,
        "model_gateway.too_many_events",
        "the model gateway emitted too many events",
    )
}

pub(super) fn model_error(
    category: ErrorCategory,
    code: &'static str,
    message: &'static str,
) -> AdkError {
    AdkError::new(ErrorComponent::Model, category, code, message).with_provider("elitea")
}

fn canonical_https_origin(value: &str) -> Result<String, ModelFacadeError> {
    if value.is_empty() || value.len() > MAX_ORIGIN_BYTES {
        return Err(ModelFacadeError::InvalidConfiguration);
    }
    let uri = value
        .parse::<http::Uri>()
        .map_err(|_| ModelFacadeError::InvalidConfiguration)?;
    if uri.scheme_str() != Some("https")
        || uri.authority().is_none()
        || uri.path() != "/"
        || uri.query().is_some()
        || uri.authority().is_some_and(|authority| {
            authority.as_str().contains('@') || !authority.as_str().is_ascii()
        })
    {
        return Err(ModelFacadeError::InvalidConfiguration);
    }
    let authority = uri
        .authority()
        .ok_or(ModelFacadeError::InvalidConfiguration)?;
    let host = authority.host();
    if host.is_empty() {
        return Err(ModelFacadeError::InvalidConfiguration);
    }
    let host = host.to_ascii_lowercase();
    let host = if host.contains(':') {
        format!("[{host}]")
    } else {
        host
    };
    match authority.port_u16() {
        Some(443) | None => Ok(format!("https://{host}")),
        Some(port) => Ok(format!("https://{host}:{port}")),
    }
}

fn valid_timeout(value: Duration) -> bool {
    !value.is_zero() && value <= MAX_TIMEOUT && value.subsec_nanos().is_multiple_of(1_000_000)
}

fn bounded_header_text(value: &str, maximum: usize) -> bool {
    !value.is_empty()
        && value.len() <= maximum
        && value.is_ascii()
        && !value.bytes().any(|byte| byte.is_ascii_control())
}

#[cfg(test)]
pub(crate) struct CapturedModelRequest {
    pub(crate) method: Method,
    pub(crate) uri: http::Uri,
    pub(crate) version: Version,
    pub(crate) headers: HeaderMap,
    pub(crate) body: Bytes,
}

#[cfg(test)]
pub(crate) type CapturedModelRequests = Arc<Mutex<Vec<CapturedModelRequest>>>;

#[cfg(test)]
pub(crate) enum TestModelGatewayOutcome {
    Response(Response<Body>),
    ResponseForModel {
        model: &'static str,
        response: Response<Body>,
    },
    Unavailable,
    Pending,
}

#[cfg(test)]
struct TestModelGatewayTransport {
    outcomes: Mutex<std::collections::VecDeque<TestModelGatewayOutcome>>,
    captured: Arc<Mutex<Vec<CapturedModelRequest>>>,
}

#[cfg(test)]
#[async_trait]
impl ModelGatewayTransport for TestModelGatewayTransport {
    async fn post(
        &self,
        request: Request<Body>,
    ) -> Result<Response<Body>, ModelGatewayTransportError> {
        let (parts, body) = request.into_parts();
        let body = body
            .collect()
            .await
            .map_err(|_| ModelGatewayTransportError::Unavailable)?
            .to_bytes();
        self.captured
            .lock()
            .map_err(|_| ModelGatewayTransportError::Unavailable)?
            .push(CapturedModelRequest {
                method: parts.method,
                uri: parts.uri,
                version: parts.version,
                headers: parts.headers,
                body: body.clone(),
            });
        let outcome = {
            let mut outcomes = self
                .outcomes
                .lock()
                .map_err(|_| ModelGatewayTransportError::Unavailable)?;
            let has_model_routing = outcomes
                .iter()
                .any(|outcome| matches!(outcome, TestModelGatewayOutcome::ResponseForModel { .. }));
            if has_model_routing {
                let model = serde_json::from_slice::<serde_json::Value>(&body)
                    .ok()
                    .and_then(|body| body.get("model")?.as_str().map(str::to_owned))
                    .ok_or(ModelGatewayTransportError::Unavailable)?;
                let position = outcomes
                    .iter()
                    .position(|outcome| {
                        matches!(
                            outcome,
                            TestModelGatewayOutcome::ResponseForModel {
                                model: expected,
                                ..
                            } if *expected == model
                        )
                    })
                    .ok_or(ModelGatewayTransportError::Unavailable)?;
                outcomes
                    .remove(position)
                    .ok_or(ModelGatewayTransportError::Unavailable)?
            } else {
                outcomes
                    .pop_front()
                    .ok_or(ModelGatewayTransportError::Unavailable)?
            }
        };
        match outcome {
            TestModelGatewayOutcome::Response(response)
            | TestModelGatewayOutcome::ResponseForModel { response, .. } => Ok(response),
            TestModelGatewayOutcome::Unavailable => Err(ModelGatewayTransportError::Unavailable),
            TestModelGatewayOutcome::Pending => std::future::pending().await,
        }
    }
}

#[cfg(test)]
pub(crate) fn test_model_gateway_config() -> ModelGatewayConfig {
    ModelGatewayConfig {
        origin: "https://platform.internal".to_owned(),
        connect_timeout: Duration::from_secs(1),
        response_header_timeout: Duration::from_secs(1),
        stream_idle_timeout: Duration::from_secs(1),
        max_request_bytes: MAX_REQUEST_BYTES,
        max_sse_event_bytes: MAX_SSE_EVENT_BYTES,
        max_stream_bytes: MAX_STREAM_BYTES,
        max_sse_events: MAX_SSE_EVENTS,
    }
}

#[cfg(test)]
pub(crate) fn test_model_gateway_client(
    outcomes: Vec<TestModelGatewayOutcome>,
    config: ModelGatewayConfig,
) -> Result<(ModelGatewayClient, CapturedModelRequests), ModelFacadeError> {
    let captured = Arc::new(Mutex::new(Vec::new()));
    let client = ModelGatewayClient::with_rpc(
        TestModelGatewayTransport {
            outcomes: Mutex::new(outcomes.into()),
            captured: Arc::clone(&captured),
        },
        config,
    )?;
    Ok((client, captured))
}

#[cfg(test)]
pub(super) fn test_model_facade_invocation() -> ModelFacadeInvocation {
    ModelFacadeInvocation {
        model_name: "fixture-model".to_owned(),
        system_instruction: "review carefully\nbe concise".to_owned(),
        max_tokens: Some(4_000),
        reasoning_effort: Some(ModelReasoningEffort::Medium),
        temperature: None,
        max_model_turns: 25,
    }
}

#[cfg(test)]
pub(super) fn test_model_request(user: &str) -> LlmRequest {
    LlmRequest {
        model: "fixture-model".to_owned(),
        contents: vec![Content::new("user").with_text(user)],
        config: Some(GenerateContentConfig {
            max_output_tokens: Some(4_000),
            ..GenerateContentConfig::default()
        }),
        tools: std::collections::HashMap::new(),
        previous_response_id: None,
    }
}

#[cfg(test)]
pub(crate) fn test_model_gateway_response(body: Body) -> Response<Body> {
    Response::builder()
        .status(StatusCode::OK)
        .version(Version::HTTP_2)
        .header(CONTENT_TYPE, "text/event-stream; charset=utf-8")
        .body(body)
        .expect("model gateway test response")
}

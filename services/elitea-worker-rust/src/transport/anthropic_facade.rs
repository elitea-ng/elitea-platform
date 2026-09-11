//! Native Anthropic `/messages` facade over the Elitea gateway transport pool.
//!
//! ADK-Rust 2.2.0 supplies the native Anthropic wire types and schema adapter,
//! while Elitea retains credential scope, project routing, transport policy,
//! bounded streaming, error redaction and single-use completion ownership. The
//! stock ADK client is deliberately not used because it constructs a separate
//! unrestricted HTTP client and cannot carry the accepted Elitea identity.

#![allow(dead_code)] // Capability registration remains intentionally disabled.

use std::collections::HashSet;
use std::fmt;
use std::sync::atomic::{AtomicU32, Ordering};
use std::sync::{Arc, Mutex};

use adk_anthropic::{
    CacheControlEphemeral, ContentBlock, ContentBlockDelta, ContentBlockDeltaEvent,
    ContentBlockStartEvent, ContentBlockStopEvent, EffortLevel, MessageCreateParams,
    MessageDeltaEvent, MessageParam, MessageRole, MessageStartEvent, MessageStreamEvent, Model,
    OutputConfig, StopReason, SystemPrompt, TextBlock, ThinkingConfig, ThinkingDisplay, ToolParam,
    ToolResultBlock, ToolResultBlockContent, ToolUnionParam, ToolUseBlock,
};
use adk_rust::model::anthropic::AnthropicSchemaAdapter;
use adk_rust::{
    AdkError, Content, ErrorCategory, FinishReason, Llm, LlmRequest, LlmResponse,
    LlmResponseStream, Part, SchemaAdapter, UsageMetadata,
};
use async_stream::try_stream;
use async_trait::async_trait;
use bytes::Bytes;
use http::header::{ACCEPT, AUTHORIZATION, CONTENT_LENGTH, CONTENT_TYPE};
use http::{HeaderName, HeaderValue, Method, Request, Version};
use http_body_util::Full;
use tokio::time::timeout;
use tonic::body::Body;
use tracing::Instrument as _;
use zeroize::Zeroizing;

use super::openai_compatible_facade::{
    BoundedSseEvent, MAX_EXECUTION_ID_BYTES, ModelFacadeError, ModelFacadeInvocation,
    ModelGatewayClient, ModelReasoningEffort, SseParser, bounded_header_text, model_error,
    next_response_chunk, valid_tool_call_id, valid_tool_name, validate_invocation,
    validate_llm_request, validate_response_head,
};
use super::runtime_context::ClaimScopedEliteaContext;
use crate::agents::runtime::{NativeAgentAssemblyError, NativeAgentAssemblyErrorCode};
use crate::agents::session::{BoundOrdinaryAgentModel, DurableModelCompletion};

const ANTHROPIC_ROUTE: &str = "/llm/v1/messages";
const MAX_COMPLETION_BYTES: usize = 60 * 1_024;
const MAX_ANTHROPIC_TOOLS: usize = 100;
const MAX_TOOL_CALLS_PER_TURN: usize = 16;
const MAX_TOOL_ARGUMENT_BYTES: usize = 256 * 1_024;
const ANTHROPIC_VERSION: HeaderName = HeaderName::from_static("anthropic-version");
const ANTHROPIC_BETA: HeaderName = HeaderName::from_static("anthropic-beta");
const X_API_KEY: HeaderName = HeaderName::from_static("x-api-key");
// Billing/execution scope comes from the redeemed claim. The frozen model
// owner may be the public project and must not be sent as this selector.
const PROJECT_SELECTOR: HeaderName = HeaderName::from_static("x-project-id");
// Tags this call with the execution it was made from so the gateway's
// request log can attribute cost per execution (issue 875). Must match the
// Python worker's `_EXECUTION_ID_HEADER` in
// `elitea_worker/agents/sdk_adapter.py`, the gateway's own
// `headerExecutionID` in `internal/llmproxy/identity.go` /
// `internal/requestlog/middleware.go`, and this crate's own
// `openai_compatible_facade::EXECUTION_ID_HEADER` (canonical form
// `X-Elitea-Execution-Id` — HTTP header lookup is case-insensitive, so the
// lowercase static name here is equivalent).
const EXECUTION_ID_HEADER: HeaderName = HeaderName::from_static("x-elitea-execution-id");

impl ModelGatewayClient {
    /// Consume one claim credential into the native Anthropic dialect while
    /// retaining the same shared Elitea channel and connection limits.
    pub(crate) fn bind_anthropic_ordinary(
        &self,
        context: &ClaimScopedEliteaContext,
        model_owner_project_id: u32,
        invocation: ModelFacadeInvocation,
    ) -> Result<BoundAnthropicFacade, ModelFacadeError> {
        validate_invocation(&invocation)?;
        if invocation.max_tokens.is_none() {
            // Anthropic Messages requires max_tokens. Main resolves Auto to
            // the configured model maximum before it freezes a native model.
            return Err(ModelFacadeError::InvalidInvocation);
        }
        let token = context.model_facade_token();
        let billing_project_id = context.resource_project_id();
        let execution_id = context.execution_id().to_owned();
        if model_owner_project_id == 0
            || billing_project_id == 0
            || token.is_empty()
            || !bounded_header_text(&execution_id, MAX_EXECUTION_ID_BYTES)
        {
            return Err(ModelFacadeError::InvalidInvocation);
        }
        let completion = Arc::new(Mutex::new(AnthropicCompletionState::default()));
        let model = Arc::new(EliteaAnthropicModel {
            transport: self.transport.clone(),
            config: self.config.clone(),
            invocation,
            billing_project_id,
            token,
            execution_id,
            completion: completion.clone(),
            calls: AtomicU32::new(0),
        });
        Ok(BoundAnthropicFacade {
            model,
            completion: AnthropicCompletion { state: completion },
        })
    }
}

/// Inseparable native model and its exact completed text.
pub(crate) struct BoundAnthropicFacade {
    model: Arc<EliteaAnthropicModel>,
    completion: AnthropicCompletion,
}

impl BoundOrdinaryAgentModel for BoundAnthropicFacade {
    fn adk_model(&self) -> Arc<dyn Llm> {
        self.model.clone()
    }

    fn take_completed_text(self) -> Result<String, NativeAgentAssemblyError> {
        self.completion.take().map_err(anthropic_completion_error)
    }

    fn durable_completion(&self) -> Option<Arc<dyn DurableModelCompletion>> {
        Some(self.completion.state.clone())
    }
}

#[cfg(test)]
impl BoundAnthropicFacade {
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
struct AnthropicCompletionState {
    value: Option<String>,
    consumed: bool,
}

struct AnthropicCompletion {
    state: Arc<Mutex<AnthropicCompletionState>>,
}

impl DurableModelCompletion for Mutex<AnthropicCompletionState> {
    fn snapshot(&self) -> adk_rust::Result<Option<String>> {
        self.lock().map(|state| state.value.clone()).map_err(|_| {
            anthropic_error(
                ErrorCategory::Internal,
                "completion_state",
                "the native Anthropic completion state is unavailable",
            )
        })
    }
}

impl AnthropicCompletion {
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

fn anthropic_completion_error(error: ModelFacadeError) -> NativeAgentAssemblyError {
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
    NativeAgentAssemblyError::new(code, "the native Anthropic completion is unavailable")
}

struct EliteaAnthropicModel {
    transport: Arc<dyn super::openai_compatible_facade::ModelGatewayTransport>,
    config: super::openai_compatible_facade::ModelGatewayConfig,
    invocation: ModelFacadeInvocation,
    billing_project_id: u64,
    token: Zeroizing<String>,
    /// The execution this claim was redeemed for (issue 875), sent to the
    /// gateway as `X-Elitea-Execution-Id` for per-execution cost attribution.
    execution_id: String,
    completion: Arc<Mutex<AnthropicCompletionState>>,
    calls: AtomicU32,
}

#[async_trait]
impl Llm for EliteaAnthropicModel {
    fn name(&self) -> &str {
        &self.invocation.model_name
    }

    fn schema_adapter(&self) -> &dyn SchemaAdapter {
        static ADAPTER: AnthropicSchemaAdapter = AnthropicSchemaAdapter;
        &ADAPTER
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
                anthropic_error(
                    ErrorCategory::InvalidInput,
                    "turn_limit",
                    "the claim-scoped native Anthropic invocation exceeded its approved turn limit",
                )
            })?;
        let span = tracing::info_span!(
            "agent.model.request",
            model_adapter = "anthropic",
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
            let body = build_anthropic_body(&request, stream, &self.invocation, &self.config)?;
            let request = build_anthropic_request(
                body,
                self.billing_project_id,
                self.token.as_str(),
                self.execution_id.as_str(),
            )?;
            let response = timeout(
                self.config.response_header_timeout,
                self.transport.post(request),
            )
            .await
            .map_err(|_| {
                anthropic_error(
                    ErrorCategory::Timeout,
                    "response_header_timeout",
                    "the native Anthropic response header timed out",
                )
            })?
            .map_err(|_| {
                anthropic_error(
                    ErrorCategory::Unavailable,
                    "transport",
                    "the native Anthropic transport is unavailable",
                )
            })?;
            validate_response_head(&response)?;
            Ok(anthropic_response_stream(
                response,
                &self.invocation.model_name,
                AnthropicStreamLimits {
                    idle_timeout: self.config.stream_idle_timeout,
                    max_event_bytes: self.config.max_sse_event_bytes,
                    max_stream_bytes: self.config.max_stream_bytes,
                    max_events: self.config.max_sse_events,
                },
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

fn build_anthropic_body(
    request: &LlmRequest,
    stream: bool,
    invocation: &ModelFacadeInvocation,
    config: &super::openai_compatible_facade::ModelGatewayConfig,
) -> Result<Bytes, AdkError> {
    let contents = validate_llm_request(request, stream, invocation)?;
    let generation = native_generation(invocation)?;
    let messages = contents
        .iter()
        .enumerate()
        .map(|(index, content)| anthropic_message(content, index + 1 == contents.len()))
        .collect::<Result<Vec<_>, _>>()?;
    let mut params = MessageCreateParams::new_streaming(
        generation.max_tokens,
        messages,
        Model::Custom(invocation.model_name.clone()),
    );
    if !invocation.system_instruction.is_empty() {
        params.system = Some(SystemPrompt::from_blocks(vec![
            TextBlock::new(&invocation.system_instruction)
                .with_cache_control(CacheControlEphemeral::new()),
        ]));
    }
    params.temperature = generation.temperature;
    params.thinking = generation.thinking;
    params.output_config = generation.output_config;
    params.tools = anthropic_tools(&request.tools)?;
    params.validate().map_err(|_| invalid_anthropic_request())?;
    let encoded = serde_json::to_vec(&params).map_err(|_| invalid_anthropic_request())?;
    if encoded.len() > config.max_request_bytes {
        return Err(anthropic_error(
            ErrorCategory::InvalidInput,
            "request_too_large",
            "the native Anthropic request exceeds its approved limit",
        ));
    }
    Ok(Bytes::from(encoded))
}

fn anthropic_message(content: &Content, is_last: bool) -> Result<MessageParam, AdkError> {
    let role = match content.role.as_str() {
        "user" | "function" | "tool" => MessageRole::User,
        "model" | "assistant" => MessageRole::Assistant,
        _ => return Err(invalid_anthropic_request()),
    };
    if is_last && let [Part::Text { text }] = content.parts.as_slice() {
        return Ok(MessageParam::new_with_string(text.clone(), role));
    }
    let blocks = content
        .parts
        .iter()
        .map(|part| match part {
            Part::Text { text } => Ok(ContentBlock::Text(TextBlock::new(text))),
            Part::Thinking { thinking, .. } => Ok(ContentBlock::Text(TextBlock::new(thinking))),
            Part::FunctionCall {
                name,
                args,
                id: Some(id),
                ..
            } => Ok(ContentBlock::ToolUse(ToolUseBlock::new(
                id,
                name,
                args.clone(),
            ))),
            Part::FunctionResponse {
                function_response,
                id: Some(id),
                ..
            } => {
                let result = serde_json::to_string(&function_response.response)
                    .map_err(|_| invalid_anthropic_request())?;
                Ok(ContentBlock::ToolResult(ToolResultBlock {
                    tool_use_id: id.clone(),
                    cache_control: None,
                    content: Some(ToolResultBlockContent::String(result)),
                    is_error: None,
                }))
            }
            _ => Err(invalid_anthropic_request()),
        })
        .collect::<Result<Vec<_>, _>>()?;
    if blocks.is_empty() {
        return Err(invalid_anthropic_request());
    }
    Ok(MessageParam::new_with_blocks(blocks, role))
}

fn anthropic_tools(
    declarations: &std::collections::HashMap<String, serde_json::Value>,
) -> Result<Option<Vec<ToolUnionParam>>, AdkError> {
    if declarations.is_empty() {
        return Ok(None);
    }
    if declarations.len() > MAX_ANTHROPIC_TOOLS {
        return Err(invalid_anthropic_request());
    }
    let mut names = declarations.keys().collect::<Vec<_>>();
    names.sort_unstable();
    names
        .into_iter()
        .map(|name| {
            let declaration = declarations
                .get(name)
                .and_then(serde_json::Value::as_object)
                .ok_or_else(invalid_anthropic_request)?;
            let schema = declaration
                .get("parameters")
                .cloned()
                .unwrap_or_else(|| serde_json::json!({"type":"object","properties":{}}));
            let mut tool = ToolParam::new(name.clone(), schema);
            if let Some(description) = declaration
                .get("description")
                .and_then(serde_json::Value::as_str)
            {
                tool = tool.with_description(description.to_owned());
            }
            Ok(ToolUnionParam::CustomTool(tool))
        })
        .collect::<Result<Vec<_>, _>>()
        .map(Some)
}

struct NativeGeneration {
    max_tokens: u32,
    temperature: Option<f32>,
    thinking: Option<ThinkingConfig>,
    output_config: Option<OutputConfig>,
}

fn native_generation(invocation: &ModelFacadeInvocation) -> Result<NativeGeneration, AdkError> {
    let max_tokens = invocation
        .max_tokens
        .ok_or_else(invalid_anthropic_request)?;
    if rejects_explicit_sampling(&invocation.model_name) && invocation.temperature.is_some() {
        return Err(anthropic_error(
            ErrorCategory::InvalidInput,
            "sampling_unsupported",
            "the native Anthropic model does not accept explicit sampling controls",
        ));
    }
    let Some(effort) = invocation.reasoning_effort else {
        return Ok(NativeGeneration {
            max_tokens,
            temperature: invocation.temperature,
            thinking: None,
            output_config: None,
        });
    };
    if effort == ModelReasoningEffort::None {
        return Ok(NativeGeneration {
            max_tokens,
            temperature: invocation.temperature,
            thinking: None,
            output_config: None,
        });
    }
    if adaptive_thinking_model(&invocation.model_name) {
        return Ok(NativeGeneration {
            max_tokens,
            temperature: None,
            thinking: Some(ThinkingConfig::Adaptive {
                display: Some(ThinkingDisplay::Summarized),
            }),
            output_config: Some(OutputConfig::with_effort(anthropic_effort(effort)?)),
        });
    }
    let budget = match effort {
        ModelReasoningEffort::Low => 2_048,
        ModelReasoningEffort::Medium => 4_096,
        ModelReasoningEffort::High => 9_092,
        ModelReasoningEffort::None => return Err(invalid_anthropic_request()),
    };
    let max_tokens = max_tokens
        .checked_add(budget)
        .filter(|value| i32::try_from(*value).is_ok())
        .ok_or_else(invalid_anthropic_request)?;
    Ok(NativeGeneration {
        max_tokens,
        temperature: Some(1.0),
        thinking: Some(ThinkingConfig::Enabled {
            budget_tokens: budget,
            display: None,
        }),
        output_config: None,
    })
}

fn anthropic_effort(effort: ModelReasoningEffort) -> Result<EffortLevel, AdkError> {
    match effort {
        ModelReasoningEffort::Low => Ok(EffortLevel::Low),
        ModelReasoningEffort::Medium => Ok(EffortLevel::Medium),
        ModelReasoningEffort::High => Ok(EffortLevel::High),
        ModelReasoningEffort::None => Err(invalid_anthropic_request()),
    }
}

fn adaptive_thinking_model(model_name: &str) -> bool {
    let name = model_name.to_ascii_lowercase();
    [
        "opus-4-7",
        "opus_4_7",
        "opus-4.7",
        "opus-4-8",
        "opus_4_8",
        "opus-4.8",
        "sonnet-4-6",
        "sonnet_4_6",
        "sonnet-4.6",
        "sonnet-5",
        "sonnet_5",
        "opus-5",
        "opus_5",
    ]
    .iter()
    .any(|pattern| name.contains(pattern))
}

fn rejects_explicit_sampling(model_name: &str) -> bool {
    let name = model_name.to_ascii_lowercase();
    [
        "fable-5",
        "fable_5",
        "mythos-5",
        "mythos_5",
        "mythos-preview",
        "mythos_preview",
        "opus-4-7",
        "opus_4_7",
        "opus-4.7",
        "opus-4-8",
        "opus_4_8",
        "opus-4.8",
        "opus-5",
        "opus_5",
        "sonnet-5",
        "sonnet_5",
    ]
    .iter()
    .any(|pattern| name.contains(pattern))
}

fn build_anthropic_request(
    body: Bytes,
    billing_project_id: u64,
    token: &str,
    execution_id: &str,
) -> Result<Request<Body>, AdkError> {
    let body_length = body.len();
    let mut request = Request::builder()
        .method(Method::POST)
        .uri(ANTHROPIC_ROUTE)
        .version(Version::HTTP_2)
        .body(Body::new(Full::new(body)))
        .map_err(|_| invalid_anthropic_request())?;
    let headers = request.headers_mut();
    headers.insert(CONTENT_TYPE, HeaderValue::from_static("application/json"));
    headers.insert(ACCEPT, HeaderValue::from_static("text/event-stream"));
    headers.insert(ANTHROPIC_VERSION, HeaderValue::from_static("2023-06-01"));
    headers.insert(
        ANTHROPIC_BETA,
        HeaderValue::from_static("prompt-caching-2024-07-31"),
    );
    headers.insert(
        CONTENT_LENGTH,
        HeaderValue::from_str(&body_length.to_string()).map_err(|_| invalid_anthropic_request())?,
    );
    headers.insert(
        PROJECT_SELECTOR,
        HeaderValue::from_str(&billing_project_id.to_string())
            .map_err(|_| invalid_anthropic_request())?,
    );
    headers.insert(
        EXECUTION_ID_HEADER,
        HeaderValue::from_str(execution_id).map_err(|_| invalid_anthropic_request())?,
    );
    let mut bearer = Zeroizing::new(String::with_capacity(7 + token.len()));
    bearer.push_str("Bearer ");
    bearer.push_str(token);
    let mut authorization =
        HeaderValue::from_str(bearer.as_str()).map_err(|_| invalid_anthropic_request())?;
    authorization.set_sensitive(true);
    headers.insert(AUTHORIZATION, authorization);
    let mut api_key = HeaderValue::from_str(token).map_err(|_| invalid_anthropic_request())?;
    api_key.set_sensitive(true);
    headers.insert(X_API_KEY, api_key);
    Ok(request)
}

#[derive(Debug, Eq, PartialEq)]
enum ActiveBlock {
    Text,
    Thinking,
    ToolUse {
        id: String,
        name: String,
        arguments: String,
    },
}

struct AnthropicStreamState {
    expected_model: String,
    started: bool,
    next_block: usize,
    active: Option<(usize, ActiveBlock)>,
    input_tokens: i32,
    cache_read_tokens: Option<i32>,
    cache_creation_tokens: Option<i32>,
    allowed_tools: HashSet<String>,
    tool_calls: Vec<Part>,
    tool_turn: bool,
    terminal: Option<Box<LlmResponse>>,
    stopped: bool,
}

impl AnthropicStreamState {
    fn new(expected_model: &str, allowed_tools: HashSet<String>) -> Self {
        Self {
            expected_model: expected_model.to_owned(),
            started: false,
            next_block: 0,
            active: None,
            input_tokens: 0,
            cache_read_tokens: None,
            cache_creation_tokens: None,
            allowed_tools,
            tool_calls: Vec::new(),
            tool_turn: false,
            terminal: None,
            stopped: false,
        }
    }

    fn apply(&mut self, event: MessageStreamEvent) -> Result<Option<LlmResponse>, AdkError> {
        if self.stopped {
            return Err(invalid_anthropic_stream());
        }
        match event {
            MessageStreamEvent::MessageStart(event) => self.start_message(event),
            MessageStreamEvent::ContentBlockStart(event) => self.start_block(event),
            MessageStreamEvent::ContentBlockDelta(event) => self.apply_delta(event),
            MessageStreamEvent::ContentBlockStop(event) => self.stop_block(event),
            MessageStreamEvent::MessageDelta(event) => self.apply_message_delta(&event),
            MessageStreamEvent::MessageStop(_) => self.stop_message(),
            MessageStreamEvent::Ping if self.started => Ok(None),
            MessageStreamEvent::Ping => Err(invalid_anthropic_stream()),
            MessageStreamEvent::ToolInputStart { .. }
            | MessageStreamEvent::ToolInputDelta { .. }
            | MessageStreamEvent::CompactionEvent(_) => Err(unsupported_anthropic_output()),
            MessageStreamEvent::StreamError { .. } => Err(anthropic_error(
                ErrorCategory::Unavailable,
                "provider_error",
                "the native Anthropic provider reported an error",
            )),
        }
    }

    fn start_message(&mut self, event: MessageStartEvent) -> Result<Option<LlmResponse>, AdkError> {
        let message = event.message;
        if self.started
            || message.model.to_string() != self.expected_model
            || message.role != MessageRole::Assistant
            || message.r#type != "message"
            || !message.content.is_empty()
            || message.stop_reason.is_some()
            || message.stop_sequence.is_some()
            || message.usage.input_tokens < 0
            || message.usage.output_tokens < 0
            || message
                .usage
                .cache_creation_input_tokens
                .is_some_and(|value| value < 0)
            || message
                .usage
                .cache_read_input_tokens
                .is_some_and(|value| value < 0)
        {
            return Err(invalid_anthropic_stream());
        }
        self.started = true;
        self.input_tokens = message.usage.input_tokens;
        self.cache_read_tokens = message.usage.cache_read_input_tokens;
        self.cache_creation_tokens = message.usage.cache_creation_input_tokens;
        Ok(None)
    }

    fn start_block(
        &mut self,
        event: ContentBlockStartEvent,
    ) -> Result<Option<LlmResponse>, AdkError> {
        self.require_content_phase(event.index)?;
        let block = match event.content_block {
            ContentBlock::Text(text)
                if text.text.is_empty() && text.citations.as_ref().is_none_or(Vec::is_empty) =>
            {
                ActiveBlock::Text
            }
            ContentBlock::Thinking(thinking)
                if thinking.thinking.is_empty() && thinking.signature.is_empty() =>
            {
                ActiveBlock::Thinking
            }
            ContentBlock::Text(_) | ContentBlock::Thinking(_) => {
                return Err(invalid_anthropic_stream());
            }
            ContentBlock::ToolUse(tool)
                if valid_tool_call_id(&tool.id)
                    && valid_tool_name(&tool.name)
                    && !self.allowed_tools.contains(&tool.name) =>
            {
                return Err(model_error(
                    ErrorCategory::Unsupported,
                    super::model_facade::TOOL_NOT_ADMITTED_CODE,
                    "the model requested a tool outside the admitted toolset",
                ));
            }
            ContentBlock::ToolUse(tool)
                if valid_tool_call_id(&tool.id)
                    && valid_tool_name(&tool.name)
                    && self.allowed_tools.contains(&tool.name)
                    && tool
                        .input
                        .as_object()
                        .is_some_and(serde_json::Map::is_empty)
                    && tool.cache_control.is_none()
                    && self.tool_calls.len() < MAX_TOOL_CALLS_PER_TURN =>
            {
                ActiveBlock::ToolUse {
                    id: tool.id,
                    name: tool.name,
                    arguments: String::new(),
                }
            }
            _ => return Err(unsupported_anthropic_output()),
        };
        self.active = Some((event.index, block));
        Ok(None)
    }

    fn apply_delta(
        &mut self,
        event: ContentBlockDeltaEvent,
    ) -> Result<Option<LlmResponse>, AdkError> {
        let Some((index, block)) = self.active.as_mut() else {
            return Err(invalid_anthropic_stream());
        };
        if event.index != *index {
            return Err(invalid_anthropic_stream());
        }
        match (block, event.delta) {
            (ActiveBlock::Text, ContentBlockDelta::TextDelta(delta)) => {
                Ok(partial_text(delta.text))
            }
            (ActiveBlock::Thinking, ContentBlockDelta::ThinkingDelta(delta)) => {
                Ok(partial_thinking(delta.thinking))
            }
            (ActiveBlock::Thinking, ContentBlockDelta::SignatureDelta(_)) => Ok(None),
            (ActiveBlock::ToolUse { arguments, .. }, ContentBlockDelta::InputJsonDelta(delta)) => {
                if arguments.len().saturating_add(delta.partial_json.len())
                    > MAX_TOOL_ARGUMENT_BYTES
                {
                    return Err(anthropic_output_too_large());
                }
                arguments.push_str(&delta.partial_json);
                Ok(None)
            }
            (_, ContentBlockDelta::CitationsDelta(_)) => Err(unsupported_anthropic_citations()),
            _ => Err(unsupported_anthropic_output()),
        }
    }

    fn stop_block(
        &mut self,
        event: ContentBlockStopEvent,
    ) -> Result<Option<LlmResponse>, AdkError> {
        if self.active.as_ref().map(|(index, _)| *index) != Some(event.index) {
            return Err(invalid_anthropic_stream());
        }
        let active = self.active.take().ok_or_else(invalid_anthropic_stream)?;
        if let (
            _,
            ActiveBlock::ToolUse {
                id,
                name,
                arguments,
            },
        ) = active
        {
            if self.tool_calls.iter().any(
                |part| matches!(part, Part::FunctionCall { id: Some(existing), .. } if existing == &id),
            ) {
                return Err(invalid_anthropic_stream());
            }
            let args = if arguments.trim().is_empty() {
                serde_json::Value::Object(serde_json::Map::new())
            } else {
                serde_json::from_str(&arguments).map_err(|_| invalid_anthropic_stream())?
            };
            if !args.is_object() {
                return Err(invalid_anthropic_stream());
            }
            self.tool_calls.push(Part::FunctionCall {
                name,
                args,
                id: Some(id),
                thought_signature: None,
            });
        }
        self.next_block = self
            .next_block
            .checked_add(1)
            .ok_or_else(invalid_anthropic_stream)?;
        Ok(None)
    }

    fn apply_message_delta(
        &mut self,
        event: &MessageDeltaEvent,
    ) -> Result<Option<LlmResponse>, AdkError> {
        if !self.started || self.active.is_some() || self.terminal.is_some() {
            return Err(invalid_anthropic_stream());
        }
        let stop = event
            .delta
            .stop_reason
            .ok_or_else(invalid_anthropic_stream)?;
        if event.usage.output_tokens < 0
            || event.usage.input_tokens.is_some_and(|value| value < 0)
            || event
                .usage
                .cache_creation_input_tokens
                .is_some_and(|value| value < 0)
            || event
                .usage
                .cache_read_input_tokens
                .is_some_and(|value| value < 0)
            || event.usage.server_tool_use.is_some()
        {
            return Err(invalid_anthropic_stream());
        }
        let (finish_reason, tool_turn) = finish_reason(stop)?;
        if tool_turn == self.tool_calls.is_empty() {
            return Err(invalid_anthropic_stream());
        }
        let input_tokens = event.usage.input_tokens.unwrap_or(self.input_tokens);
        let usage = UsageMetadata {
            prompt_token_count: input_tokens,
            candidates_token_count: event.usage.output_tokens,
            total_token_count: input_tokens
                .checked_add(event.usage.output_tokens)
                .ok_or_else(invalid_anthropic_stream)?,
            cache_read_input_token_count: event
                .usage
                .cache_read_input_tokens
                .or(self.cache_read_tokens),
            cache_creation_input_token_count: event
                .usage
                .cache_creation_input_tokens
                .or(self.cache_creation_tokens),
            ..UsageMetadata::default()
        };
        self.tool_turn = tool_turn;
        self.terminal = Some(Box::new(LlmResponse {
            content: tool_turn.then(|| Content {
                role: "model".to_owned(),
                parts: std::mem::take(&mut self.tool_calls),
            }),
            usage_metadata: Some(usage),
            finish_reason: Some(finish_reason),
            partial: false,
            // Tool use keeps the ADK turn open until the tool result and the
            // subsequent model response complete it.
            turn_complete: !tool_turn,
            ..LlmResponse::default()
        }));
        Ok(None)
    }

    fn stop_message(&mut self) -> Result<Option<LlmResponse>, AdkError> {
        if !self.started || self.active.is_some() || self.terminal.is_none() {
            return Err(invalid_anthropic_stream());
        }
        self.stopped = true;
        Ok(None)
    }

    fn require_content_phase(&self, index: usize) -> Result<(), AdkError> {
        if !self.started
            || self.active.is_some()
            || self.terminal.is_some()
            || index != self.next_block
        {
            return Err(invalid_anthropic_stream());
        }
        Ok(())
    }

    fn finish(mut self) -> Result<(LlmResponse, bool), AdkError> {
        if !self.stopped || self.active.is_some() {
            return Err(incomplete_anthropic_stream());
        }
        self.terminal
            .take()
            .map(|terminal| *terminal)
            .map(|terminal| (terminal, !self.tool_turn))
            .ok_or_else(incomplete_anthropic_stream)
    }
}

#[derive(Clone, Copy)]
struct AnthropicStreamLimits {
    idle_timeout: std::time::Duration,
    max_event_bytes: usize,
    max_stream_bytes: usize,
    max_events: usize,
}

fn anthropic_response_stream(
    mut response: http::Response<Body>,
    expected_model: &str,
    limits: AnthropicStreamLimits,
    allowed_tools: HashSet<String>,
    completion: Arc<Mutex<AnthropicCompletionState>>,
) -> LlmResponseStream {
    let expected_model = expected_model.to_owned();
    Box::pin(try_stream! {
        let mut parser = SseParser::new(
            limits.max_event_bytes,
            limits.max_stream_bytes,
            limits.max_events,
        );
        let mut state = AnthropicStreamState::new(&expected_model, allowed_tools);
        let mut accumulated = String::new();
        let mut semantic_bytes = 0_usize;
        loop {
            let chunk = next_response_chunk(&mut response, limits.idle_timeout).await?;
            let input_finished = chunk.is_none();
            if let Some(chunk) = chunk {
                parser.push(&chunk)?;
            } else {
                parser.finish_input();
            }
            while let Some(event) = parser.next_event()? {
                let event = parse_anthropic_event(event)?;
                if let Some(response) = state.apply(event)? {
                    record_anthropic_response(&response, &mut accumulated, &mut semantic_bytes)?;
                    yield response;
                }
            }
            if input_finished {
                break;
            }
        }
        let (terminal, completed_turn) = state.finish()?;
        if completed_turn {
            record_anthropic_completion(&mut accumulated, &completion)?;
        }
        yield terminal;
    })
}

fn parse_anthropic_event(event: BoundedSseEvent) -> Result<MessageStreamEvent, AdkError> {
    let event_name = event.event_type.ok_or_else(invalid_anthropic_stream)?;
    let event_name = std::str::from_utf8(&event_name).map_err(|_| invalid_anthropic_stream())?;
    let value: serde_json::Value =
        serde_json::from_slice(&event.data).map_err(|_| invalid_anthropic_stream())?;
    if value.get("type").and_then(serde_json::Value::as_str) != Some(event_name) {
        return Err(invalid_anthropic_stream());
    }
    serde_json::from_value(value).map_err(|_| invalid_anthropic_stream())
}

fn partial_text(text: String) -> Option<LlmResponse> {
    (!text.is_empty()).then(|| LlmResponse {
        content: Some(Content {
            role: "model".to_owned(),
            parts: vec![Part::Text { text }],
        }),
        partial: true,
        ..LlmResponse::default()
    })
}

fn partial_thinking(thinking: String) -> Option<LlmResponse> {
    (!thinking.is_empty()).then(|| LlmResponse {
        content: Some(Content {
            role: "model".to_owned(),
            parts: vec![Part::Thinking {
                thinking,
                signature: None,
            }],
        }),
        partial: true,
        ..LlmResponse::default()
    })
}

fn finish_reason(reason: StopReason) -> Result<(FinishReason, bool), AdkError> {
    match reason {
        StopReason::EndTurn | StopReason::StopSequence => Ok((FinishReason::Stop, false)),
        StopReason::MaxTokens | StopReason::ModelContextWindowExceeded => {
            Ok((FinishReason::MaxTokens, false))
        }
        StopReason::Refusal => Ok((FinishReason::Safety, false)),
        StopReason::ToolUse => Ok((FinishReason::Stop, true)),
        StopReason::PauseTurn | StopReason::PauseRun => Err(unsupported_anthropic_output()),
    }
}

fn record_anthropic_response(
    response: &LlmResponse,
    accumulated: &mut String,
    semantic_bytes: &mut usize,
) -> Result<(), AdkError> {
    let Some(content) = &response.content else {
        return Ok(());
    };
    for part in &content.parts {
        let part_bytes = match part {
            Part::Text { text } => text.len(),
            Part::Thinking { thinking, .. } => thinking.len(),
            _ => return Err(unsupported_anthropic_output()),
        };
        *semantic_bytes = semantic_bytes
            .checked_add(part_bytes)
            .ok_or_else(anthropic_output_too_large)?;
        if *semantic_bytes > MAX_COMPLETION_BYTES {
            return Err(anthropic_output_too_large());
        }
        if let Part::Text { text } = part {
            accumulated.push_str(text);
        }
    }
    Ok(())
}

fn record_anthropic_completion(
    accumulated: &mut String,
    completion: &Arc<Mutex<AnthropicCompletionState>>,
) -> Result<(), AdkError> {
    let mut state = completion.lock().map_err(|_| {
        anthropic_error(
            ErrorCategory::Internal,
            "completion_state",
            "the native Anthropic completion state is unavailable",
        )
    })?;
    if state.value.is_some() || state.consumed {
        return Err(anthropic_error(
            ErrorCategory::Internal,
            "completion_reused",
            "the native Anthropic completion was produced more than once",
        ));
    }
    state.value = Some(std::mem::take(accumulated));
    Ok(())
}

fn invalid_anthropic_request() -> AdkError {
    anthropic_error(
        ErrorCategory::InvalidInput,
        "invalid_request",
        "the native Anthropic request is outside the admitted ordinary profile",
    )
}

fn invalid_anthropic_stream() -> AdkError {
    anthropic_error(
        ErrorCategory::Unavailable,
        "invalid_stream",
        "the native Anthropic stream is malformed",
    )
}

fn incomplete_anthropic_stream() -> AdkError {
    anthropic_error(
        ErrorCategory::Unavailable,
        "incomplete_stream",
        "the native Anthropic stream ended before completion",
    )
}

fn unsupported_anthropic_output() -> AdkError {
    anthropic_error(
        ErrorCategory::Unsupported,
        "unsupported_output",
        "the native Anthropic output is outside the admitted ordinary profile",
    )
}

fn unsupported_anthropic_citations() -> AdkError {
    anthropic_error(
        ErrorCategory::Unsupported,
        "citations_unmapped",
        "native Anthropic citations are not mapped by the Elitea ADK adapter",
    )
}

fn anthropic_output_too_large() -> AdkError {
    anthropic_error(
        ErrorCategory::InvalidInput,
        "output_too_large",
        "the native Anthropic output exceeds its approved limit",
    )
}

fn anthropic_error(
    category: ErrorCategory,
    suffix: &'static str,
    message: &'static str,
) -> AdkError {
    let code = match suffix {
        "turn_limit" => "anthropic_gateway.turn_limit",
        "response_header_timeout" => "anthropic_gateway.response_header_timeout",
        "transport" => "anthropic_gateway.transport",
        "request_too_large" => "anthropic_gateway.request_too_large",
        "provider_error" => "anthropic_gateway.provider_error",
        "completion_state" => "anthropic_gateway.completion_state",
        "completion_reused" => "anthropic_gateway.completion_reused",
        "invalid_request" => "anthropic_gateway.invalid_request",
        "sampling_unsupported" => "anthropic_gateway.sampling_unsupported",
        "invalid_stream" => "anthropic_gateway.invalid_stream",
        "incomplete_stream" => "anthropic_gateway.incomplete_stream",
        "unsupported_output" => "anthropic_gateway.unsupported_output",
        "citations_unmapped" => "anthropic_gateway.citations_unmapped",
        "output_too_large" => "anthropic_gateway.output_too_large",
        _ => "anthropic_gateway.internal",
    };
    model_error(category, code, message)
}

impl fmt::Debug for BoundAnthropicFacade {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str("BoundAnthropicFacade(..)")
    }
}

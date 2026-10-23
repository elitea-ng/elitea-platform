//! The desktop's [`ModelTransport`]: the platform's OpenAI-compatible
//! `/llm/v1/chat/completions` with the native token, `X-Project-Id` and
//! the local turn's `X-Elitea-Execution-Id` (`libs/rust/llm-wire`,
//! `conformance/llm-caller/contract.json`).
//!
//! A small adapter on `llm-wire` and reqwest, not `model-client`: that
//! crate turns on serde_json `preserve_order`, which must not be linked
//! next to the runtime (ADR-0029 decision 2). It follows the worker's
//! OpenAI-compatible facade for the request body (one leading instruction
//! message that merges every `system` content, tool declarations sorted by
//! name, `stream: true` with usage) and reads the stream leniently.
//!
//! Text deltas go to the webview as they arrive; the adk agent receives the
//! model's response once the stream ended (it runs in
//! `StreamingMode::None`, so session history holds whole responses).
//!
//! Reasoning (#1228) streams to the webview as `thinking_delta` events, and
//! a `thinking_end` closes each block; each model step's reasoning goes to
//! the commit's `thinking_steps` ([`Recorder::thinking_step`]) and stays in
//! the session as a `Part::Thinking`. It goes back to the provider only for
//! a family that requires it ([`ReasoningReplay`]): never on the
//! OpenAI-compatible chat route, whose schema has no reasoning items.

use std::collections::HashMap;
use std::sync::{Arc, Mutex};

use adk_core::{
    AdkError, Content, FinishReason, Llm, LlmRequest, LlmResponse, LlmResponseStream, Part,
    UsageMetadata,
};
use async_trait::async_trait;
use base64::Engine as _;
use base64::engine::general_purpose::STANDARD as BASE64;
use elitea_agent_runtime::host::{
    BoundModel, HostError, HostErrorCode, ModelRequest, ModelTransport,
};
use elitea_llm_wire::headers::{EXECUTION_HEADER, PROJECT_HEADER, valid_execution_id};
use elitea_llm_wire::openai::{self, DoneMatch, TypeCheck};
use elitea_llm_wire::refusal::refusal_code;
use elitea_llm_wire::route::CHAT_COMPLETIONS_ROUTE;
use elitea_llm_wire::sse::{SseOptions, SseSplitter};
use elitea_llm_wire::tool_calls::{
    ToolCallAssembler, ToolCallLimits, ToolCallProfile, parse_lenient_delta,
};
use futures::StreamExt as _;
use serde_json::{Map, Value, json};

use super::api::Credentials;
use super::catalogue::ReasoningReplay;
use super::events::TurnEvents;
use super::recorder::{self, Recorder};

const TOOL_CALL_LIMITS: ToolCallLimits = ToolCallLimits {
    max_calls: 128,
    max_argument_bytes: 1024 * 1024,
    max_name_bytes: 256,
    max_id_bytes: 256,
};

/// The models of one local turn.
#[derive(Clone)]
pub struct GatewayTransport {
    pub http: reqwest::Client,
    pub credentials: Arc<dyn Credentials>,
    /// The turn's project: billing and the execution id's scope.
    pub project_id: i64,
    pub execution_id: String,
    pub events: Arc<TurnEvents>,
    /// A model bound to report nothing to the webview (no text deltas, no
    /// usage, no first-token time): the summary model of a compaction.
    pub quiet: bool,
    /// Where each model step's reasoning is recorded for the commit (not
    /// for a quiet model).
    pub recorder: Option<Arc<Recorder>>,
    /// Whether reasoning goes back to the provider in history.
    pub replay: ReasoningReplay,
    /// Whether the model sees images (the catalogue's `supports_vision`):
    /// a request that carries one to a model that does not is refused,
    /// never sent without it (#1229).
    pub vision: bool,
}

impl ModelTransport for GatewayTransport {
    fn bind(&self, request: ModelRequest) -> Result<Box<dyn BoundModel>, HostError> {
        if !valid_execution_id(&self.execution_id) {
            return Err(HostError::new(
                HostErrorCode::InvalidConfiguration,
                "the local turn's execution id is not one the gateway keeps",
            ));
        }
        Ok(Box::new(GatewayBinding {
            model: Arc::new(GatewayModel {
                http: self.http.clone(),
                credentials: self.credentials.clone(),
                project_id: self.project_id,
                execution_id: self.execution_id.clone(),
                events: self.events.clone(),
                quiet: self.quiet,
                recorder: if self.quiet {
                    None
                } else {
                    self.recorder.clone()
                },
                replay: self.replay,
                vision: self.vision,
                request,
                completed: Mutex::new(None),
            }),
        }))
    }
}

struct GatewayBinding {
    model: Arc<GatewayModel>,
}

impl BoundModel for GatewayBinding {
    fn adk_model(&self) -> Arc<dyn Llm> {
        self.model.clone()
    }

    fn take_completed_text(self: Box<Self>) -> Result<String, HostError> {
        self.model
            .completed
            .lock()
            .ok()
            .and_then(|mut text| text.take())
            .ok_or(HostError::new(
                HostErrorCode::InvalidResponse,
                "the model produced no complete answer",
            ))
    }
}

pub struct GatewayModel {
    http: reqwest::Client,
    credentials: Arc<dyn Credentials>,
    project_id: i64,
    execution_id: String,
    events: Arc<TurnEvents>,
    quiet: bool,
    recorder: Option<Arc<Recorder>>,
    replay: ReasoningReplay,
    vision: bool,
    request: ModelRequest,
    /// The last answer that ended the model's turn (no tool calls).
    completed: Mutex<Option<String>>,
}

fn model_error(code: &str, message: &str) -> AdkError {
    AdkError::model(format!("{code}: {message}"))
}

#[async_trait]
impl Llm for GatewayModel {
    fn name(&self) -> &str {
        &self.request.model_name
    }

    async fn generate_content(
        &self,
        request: LlmRequest,
        _stream: bool,
    ) -> adk_core::Result<LlmResponseStream> {
        let body = request_body(
            &self.request,
            &request,
            self.replay,
            if self.vision {
                Images::Send
            } else {
                Images::Refuse
            },
        )?;
        let started = recorder::now();
        let bearer = self
            .credentials
            .bearer()
            .await
            .map_err(|error| model_error("model_gateway.unauthorized", &error.message))?;
        let response = self
            .http
            .post(format!("{}{CHAT_COMPLETIONS_ROUTE}", bearer.origin))
            // The whole call, stream included, as when the client carried it.
            .timeout(super::api::API_TIMEOUT)
            .bearer_auth(&bearer.token)
            .header(PROJECT_HEADER, self.project_id.to_string())
            .header(EXECUTION_HEADER, &self.execution_id)
            .header("Accept", "text/event-stream")
            .json(&body)
            .send()
            .await
            .map_err(|_| {
                model_error(
                    "model_gateway.unavailable",
                    "could not reach the model gateway",
                )
            })?;
        let status = response.status().as_u16();
        if !(200..300).contains(&status) {
            let body = response.bytes().await.unwrap_or_default();
            let code = if context_length_exceeded(status, &body) {
                CONTEXT_LENGTH_EXCEEDED
            } else {
                refusal_code(status, &body)
            };
            return Err(model_error(code, refusal_message(code)));
        }
        let mut reader = StreamReader::new((!self.quiet).then_some(&*self.events));
        let mut bytes = response.bytes_stream();
        while let Some(chunk) = bytes.next().await {
            let chunk = chunk.map_err(|_| {
                model_error(
                    "model_gateway.incomplete_stream",
                    "the model stream was cut off",
                )
            })?;
            if !self.quiet {
                self.events.first_token();
            }
            if reader.push(&chunk)? {
                break;
            }
        }
        let (responses, completed) = reader.finish()?;
        if let Some(recorder) = &self.recorder {
            let thinking: String = responses
                .iter()
                .filter_map(|response| response.content.as_ref())
                .flat_map(|content| &content.parts)
                .filter_map(|part| match part {
                    Part::Thinking { thinking, .. } => Some(thinking.as_str()),
                    _ => None,
                })
                .collect();
            recorder.thinking_step(
                &thinking,
                &self.request.model_name,
                &started,
                &recorder::now(),
            );
        }
        if let Some(text) = completed
            && let Ok(mut slot) = self.completed.lock()
        {
            *slot = Some(text);
        }
        Ok(Box::pin(futures::stream::iter(
            responses.into_iter().map(Ok),
        )))
    }
}

/// The code of a provider's "the prompt is too long" refusal: the step
/// compaction compacts and retries once on it
/// (`elitea_agent_runtime::context_management::is_overflow`).
pub const CONTEXT_LENGTH_EXCEEDED: &str = "model_gateway.context_length_exceeded";

/// A 400 or 413 whose error names the context length (OpenAI's
/// `context_length_exceeded` code, or a provider's message that says so).
/// Read by value lookups only, never echoed.
fn context_length_exceeded(status: u16, body: &[u8]) -> bool {
    if !matches!(status, 400 | 413) {
        return false;
    }
    let Ok(value) = serde_json::from_slice::<Value>(body) else {
        return false;
    };
    let error = value.get("error").unwrap_or(&value);
    let field = |key: &str| {
        error
            .get(key)
            .and_then(Value::as_str)
            .unwrap_or_default()
            .to_ascii_lowercase()
    };
    let code = field("code");
    let message = field("message");
    code == "context_length_exceeded"
        || field("type") == "context_length_exceeded"
        || message.contains("context_length_exceeded")
        || message.contains("maximum context length")
        || message.contains("context window")
        || message.contains("prompt is too long")
}

fn refusal_message(code: &str) -> &'static str {
    match code {
        CONTEXT_LENGTH_EXCEEDED => "the conversation is longer than the model's context window",
        "model_gateway.member_budget_exhausted" => "your budget for this period is used up",
        "model_gateway.project_budget_exhausted" => {
            "the project's budget for this period is used up"
        }
        "model_gateway.budget_exhausted" => "the model's quota is used up",
        "model_gateway.unauthorized" => "the platform refused the sign-in; sign in again",
        "model_gateway.forbidden" => "the project may not use this model",
        "model_gateway.rate_limited" => "the model is rate limited; try again shortly",
        "model_gateway.upstream_timeout" => "the model did not answer in time",
        "model_gateway.unavailable" => "the model gateway is unavailable",
        _ => "the model gateway refused the request",
    }
}

/// What a request body does with the images in its contents (#1229).
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub(super) enum Images {
    /// Send them as `image_url` data URIs.
    Send,
    /// The model cannot see images: a request carrying one is refused
    /// (`model_gateway.model_no_vision`), never sent without it.
    Refuse,
    /// The body is only measured against the context window: each image
    /// is a placeholder that weighs what a provider bills for one.
    Measure,
}

/// Why a message could not be rendered.
enum Unrenderable {
    Malformed,
    NoVision,
}

/// A data URI standing in for an image whose bytes must not be measured.
fn measured_placeholder() -> String {
    let bytes = usize::try_from(elitea_agent_runtime::image_result::TOKEN_ESTIMATE)
        .unwrap_or_default()
        .saturating_mul(4);
    format!("data:image/png;base64,{}", "A".repeat(bytes))
}

fn image_url_part(mime: &str, base64: &str, images: Images) -> Value {
    let url = if images == Images::Measure {
        measured_placeholder()
    } else {
        format!("data:{mime};base64,{base64}")
    };
    json!({"type": "image_url", "image_url": {"url": url}})
}

/// The OpenAI-compatible body: the worker facade's shape.
pub(super) fn request_body(
    model: &ModelRequest,
    request: &LlmRequest,
    replay: ReasoningReplay,
    images: Images,
) -> adk_core::Result<Value> {
    let invalid = || {
        model_error(
            "model_gateway.invalid_request",
            "the model request is malformed",
        )
    };
    let mut sections = Vec::new();
    if !model.system_instruction.is_empty() {
        sections.push(model.system_instruction.clone());
    }
    for content in request.contents.iter().filter(|c| c.role == "system") {
        let text = content
            .parts
            .iter()
            .filter_map(|part| match part {
                Part::Text { text } => Some(text.as_str()),
                _ => None,
            })
            .collect::<Vec<_>>()
            .join("\n");
        let text = text.trim_matches('\n');
        if !text.is_empty() {
            sections.push(text.to_owned());
        }
    }
    let mut messages = Vec::new();
    let system = sections.join("\n\n");
    if !system.is_empty() {
        messages.push(json!({"role": "system", "content": system}));
    }
    // Chat completions' tool messages are text-only: an image a tool
    // returned follows the run of tool messages it belongs to, as a user
    // message the host wrote (`name: tool_image`).
    let mut tool_images: Vec<Value> = Vec::new();
    for content in request.contents.iter().filter(|c| c.role != "system") {
        if !matches!(content.role.as_str(), "function" | "tool") {
            messages.append(&mut tool_images);
        }
        append_message(content, &mut messages, &mut tool_images, replay, images).map_err(
            |why| match why {
                Unrenderable::Malformed => invalid(),
                Unrenderable::NoVision => model_error(
                    "model_gateway.model_no_vision",
                    "this model cannot view images; choose a model that can",
                ),
            },
        )?;
    }
    messages.append(&mut tool_images);
    let mut body = Map::new();
    body.insert("model".into(), json!(model.model_name));
    body.insert("messages".into(), Value::Array(messages));
    if !request.tools.is_empty() {
        body.insert("tools".into(), tools(&request.tools).ok_or_else(invalid)?);
        body.insert("tool_choice".into(), json!("auto"));
    }
    body.insert("stream".into(), json!(true));
    body.insert("stream_options".into(), json!({"include_usage": true}));
    if let Some(max_tokens) = model.max_tokens {
        body.insert("max_completion_tokens".into(), json!(max_tokens));
    }
    if let Some(temperature) = model.temperature {
        body.insert("temperature".into(), json!(temperature));
    }
    if let Some(effort) = model.reasoning_effort {
        body.insert(
            "reasoning_effort".into(),
            json!(super::definition::effort_name(effort)),
        );
    }
    Ok(Value::Object(body))
}

fn append_message(
    content: &Content,
    messages: &mut Vec<Value>,
    tool_images: &mut Vec<Value>,
    replay: ReasoningReplay,
    images: Images,
) -> Result<(), Unrenderable> {
    match content.role.as_str() {
        "user" => {
            let text: Vec<&str> = content
                .parts
                .iter()
                .filter_map(|part| match part {
                    Part::Text { text } => Some(text.as_str()),
                    _ => None,
                })
                .collect();
            let pictures: Vec<&Part> = content
                .parts
                .iter()
                .filter(
                    |part| matches!(part, Part::InlineData { mime_type, .. } if mime_type.starts_with("image/")),
                )
                .collect();
            if pictures.is_empty() {
                messages.push(json!({"role": "user", "content": text.join("\n")}));
            } else if images == Images::Refuse {
                return Err(Unrenderable::NoVision);
            } else {
                // The worker's shape: the text, then one part per image.
                let mut parts = Vec::new();
                let text = text.join("\n");
                if !text.is_empty() {
                    parts.push(json!({"type": "text", "text": text}));
                }
                for picture in pictures {
                    if let Part::InlineData {
                        mime_type, data, ..
                    } = picture
                    {
                        let base64 = if images == Images::Measure {
                            String::new()
                        } else {
                            BASE64.encode(data)
                        };
                        parts.push(image_url_part(mime_type, &base64, images));
                    }
                }
                messages.push(json!({"role": "user", "content": parts}));
            }
        }
        "model" | "assistant" => {
            let mut text = String::new();
            let mut calls = Vec::new();
            for part in &content.parts {
                match part {
                    Part::Text { text: value } => text.push_str(value),
                    Part::FunctionCall {
                        name,
                        args,
                        id: Some(id),
                        ..
                    } => calls.push(json!({
                        "id": id,
                        "type": "function",
                        "function": {
                            "name": name,
                            "arguments": serde_json::to_string(args).map_err(|_| Unrenderable::Malformed)?,
                        },
                    })),
                    // Reasoning is not part of the chat schema: on this
                    // route it is never sent back (`ReasoningReplay::Never`).
                    // TODO(#1227 stage 2): the Anthropic route sends a signed
                    // thinking block back as the provider requires; until it
                    // exists there is nowhere to put it.
                    Part::Thinking {
                        signature: Some(_), ..
                    } if replay == ReasoningReplay::SignedBlocks => {}
                    _ => {}
                }
            }
            let mut message = json!({
                "role": "assistant",
                "content": if text.is_empty() { Value::Null } else { Value::String(text) },
            });
            if !calls.is_empty() {
                message["tool_calls"] = Value::Array(calls);
            }
            messages.push(message);
        }
        "function" | "tool" => {
            for part in &content.parts {
                if let Part::FunctionResponse {
                    function_response,
                    id: Some(id),
                    ..
                } = part
                {
                    let response = &function_response.response;
                    let body = elitea_agent_runtime::image_result::without_image(response);
                    messages.push(json!({
                        "role": "tool",
                        "tool_call_id": id,
                        "content": serde_json::to_string(&*body).map_err(|_| Unrenderable::Malformed)?,
                    }));
                    if matches!(body, std::borrow::Cow::Owned(_))
                        && let (Some(mime), Some(base64)) = (
                            response.get("mime").and_then(Value::as_str),
                            response
                                .get(elitea_agent_runtime::image_result::IMAGE_FIELD)
                                .and_then(Value::as_str),
                        )
                    {
                        if images == Images::Refuse {
                            return Err(Unrenderable::NoVision);
                        }
                        tool_images.push(json!({
                            "role": "user",
                            "name": "tool_image",
                            "content": [
                                {"type": "text", "text": format!("Image returned by tool call {id}:")},
                                image_url_part(mime, base64, images),
                            ],
                        }));
                    }
                }
            }
        }
        _ => return Err(Unrenderable::Malformed),
    }
    Ok(())
}

fn tools(declarations: &HashMap<String, Value>) -> Option<Value> {
    let mut names: Vec<&String> = declarations.keys().collect();
    names.sort_unstable();
    names
        .into_iter()
        .map(|name| {
            let declaration = declarations.get(name)?.as_object()?;
            let mut function = Map::new();
            function.insert("name".into(), json!(name));
            if let Some(description) = declaration.get("description") {
                function.insert("description".into(), description.clone());
            }
            function.insert(
                "parameters".into(),
                declaration
                    .get("parameters")
                    .cloned()
                    .unwrap_or_else(|| json!({"type": "object", "properties": {}})),
            );
            Some(json!({"type": "function", "function": function}))
        })
        .collect::<Option<Vec<_>>>()
        .map(Value::Array)
}

/// Reads one streamed completion.
struct StreamReader<'a> {
    /// `None` for a quiet model.
    events: Option<&'a TurnEvents>,
    splitter: SseSplitter,
    text: String,
    reasoning: String,
    /// The chars of the reasoning block streaming now (`None`: none open).
    thinking: Option<usize>,
    calls: ToolCallAssembler,
    finish: Option<openai::FinishReason>,
    usage: Option<UsageMetadata>,
    done: bool,
}

impl<'a> StreamReader<'a> {
    fn new(events: Option<&'a TurnEvents>) -> Self {
        Self {
            events,
            splitter: SseSplitter::new(SseOptions::default()),
            text: String::new(),
            reasoning: String::new(),
            thinking: None,
            calls: ToolCallAssembler::new(ToolCallProfile::Lenient, TOOL_CALL_LIMITS),
            finish: None,
            usage: None,
            done: false,
        }
    }

    fn malformed() -> AdkError {
        model_error("model_gateway.invalid_sse", "the model stream is malformed")
    }

    /// Feed bytes; true once `[DONE]` arrived.
    fn push(&mut self, chunk: &[u8]) -> adk_core::Result<bool> {
        let mut events = Vec::new();
        self.splitter
            .push_into(chunk, &mut events)
            .map_err(|_| Self::malformed())?;
        for event in events {
            self.event(&event.data)?;
        }
        Ok(self.done)
    }

    fn event(&mut self, data: &[u8]) -> adk_core::Result<()> {
        if self.done {
            return Ok(());
        }
        if openai::is_done(data, DoneMatch::Trimmed) {
            self.done = true;
            return Ok(());
        }
        let chunk: Value = serde_json::from_slice(data).map_err(|_| Self::malformed())?;
        if let Some(usage) = chunk.get("usage").filter(|u| u.is_object()) {
            let count = |key: &str| {
                usage
                    .get(key)
                    .and_then(Value::as_i64)
                    .and_then(|v| i32::try_from(v).ok())
                    .unwrap_or_default()
            };
            let reasoning = usage
                .get("completion_tokens_details")
                .and_then(|details| details.get("reasoning_tokens"))
                .and_then(Value::as_i64)
                .and_then(|v| i32::try_from(v).ok());
            self.usage = Some(UsageMetadata {
                prompt_token_count: count("prompt_tokens"),
                candidates_token_count: count("completion_tokens"),
                total_token_count: count("total_tokens"),
                thinking_token_count: reasoning,
                ..UsageMetadata::default()
            });
        }
        let Some(choice) = chunk
            .get("choices")
            .and_then(Value::as_array)
            .and_then(|choices| choices.first())
        else {
            return Ok(());
        };
        if let Some(delta) = choice.get("delta") {
            if let Ok(Some(reasoning)) = openai::reasoning_text(delta, TypeCheck::Lenient)
                && !reasoning.is_empty()
            {
                self.reasoning.push_str(reasoning);
                *self.thinking.get_or_insert(0) += reasoning.chars().count();
                if let Some(events) = self.events {
                    events.thinking_delta(reasoning);
                }
            }
            let answering = delta
                .get("content")
                .and_then(Value::as_str)
                .is_some_and(|text| !text.is_empty())
                || delta
                    .get("tool_calls")
                    .is_some_and(|calls| !calls.is_null());
            if answering {
                self.end_thinking();
            }
            if let Some(text) = delta.get("content").and_then(Value::as_str) {
                self.text.push_str(text);
                if let Some(events) = self.events {
                    events.text_delta(text);
                }
            }
            if let Some(calls) = delta.get("tool_calls").and_then(Value::as_array) {
                for call in calls {
                    let delta = parse_lenient_delta(call).map_err(|_| Self::malformed())?;
                    self.calls.apply(delta).map_err(|_| Self::malformed())?;
                }
            }
        }
        if let Some(reason) = choice.get("finish_reason").and_then(Value::as_str) {
            self.finish = Some(openai::FinishReason::parse(reason));
        }
        Ok(())
    }

    /// The reasoning block streaming now, if any, closed (`thinking_end`).
    fn end_thinking(&mut self) {
        if let (Some(chars), Some(events)) = (self.thinking.take(), self.events) {
            events.thinking_end(chars);
        }
    }

    /// The adk responses and, for an answer without tool calls, its text.
    fn finish(mut self) -> adk_core::Result<(Vec<LlmResponse>, Option<String>)> {
        let mut tail = Vec::new();
        self.splitter
            .finish_into(&mut tail)
            .map_err(|_| Self::malformed())?;
        for event in tail {
            self.event(&event.data)?;
        }
        let Some(finish) = self.finish else {
            return Err(model_error(
                "model_gateway.incomplete_stream",
                "the model stream ended before the answer was complete",
            ));
        };
        self.end_thinking();
        let mut parts = Vec::new();
        if !self.reasoning.is_empty() {
            parts.push(Part::Thinking {
                thinking: std::mem::take(&mut self.reasoning),
                signature: None,
            });
        }
        if !self.text.is_empty() {
            parts.push(Part::Text {
                text: self.text.clone(),
            });
        }
        let mut has_calls = false;
        if !self.calls.is_empty() {
            for call in self.calls.finish().map_err(|_| Self::malformed())? {
                let call = call.map_err(|_| Self::malformed())?;
                let args: Value = if call.arguments.trim().is_empty() {
                    json!({})
                } else {
                    serde_json::from_str(&call.arguments).map_err(|_| Self::malformed())?
                };
                parts.push(Part::FunctionCall {
                    name: call.name,
                    args,
                    id: Some(call.id),
                    thought_signature: None,
                });
                has_calls = true;
            }
        }
        let finish_reason = match finish {
            openai::FinishReason::Length => FinishReason::MaxTokens,
            openai::FinishReason::ContentFilter => FinishReason::Safety,
            openai::FinishReason::Other => FinishReason::Other,
            _ => FinishReason::Stop,
        };
        if let (Some(usage), Some(events)) = (&self.usage, self.events) {
            let count = |n: i32| u32::try_from(n).unwrap_or_default();
            events.usage(
                count(usage.prompt_token_count),
                count(usage.candidates_token_count),
                count(usage.total_token_count),
                usage.thinking_token_count.map(count).filter(|n| *n > 0),
            );
        }
        let completed = (!has_calls).then(|| self.text.clone());
        let response = LlmResponse {
            content: (!parts.is_empty()).then(|| Content {
                role: "model".to_owned(),
                parts,
            }),
            usage_metadata: self.usage,
            finish_reason: Some(finish_reason),
            partial: false,
            turn_complete: !has_calls,
            ..LlmResponse::default()
        };
        Ok((vec![response], completed))
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::d0::events::VecEmitter;

    fn model_request() -> ModelRequest {
        ModelRequest {
            model_project_id: 1,
            model_name: "gpt-x".into(),
            system_instruction: "Be brief.\n\nMemory: likes tea".into(),
            max_tokens: Some(100),
            temperature: Some(0.5),
            reasoning_effort: None,
            response_schema: None,
            allow_text_continuation: false,
            max_model_turns: 5,
        }
    }

    #[test]
    fn the_body_has_one_leading_system_message_and_sorted_tools() {
        let mut request = LlmRequest {
            model: "gpt-x".into(),
            contents: vec![
                Content::new("system").with_text("Skill catalogue"),
                Content::new("user").with_text("hi"),
            ],
            config: None,
            tools: HashMap::new(),
            previous_response_id: None,
        };
        request.tools.insert(
            "b_tool".into(),
            json!({"name": "b_tool", "description": "B"}),
        );
        request.tools.insert(
            "a_tool".into(),
            json!({"name": "a_tool", "parameters": {"type": "object"}}),
        );
        let body = request_body(
            &model_request(),
            &request,
            ReasoningReplay::Never,
            Images::Send,
        )
        .unwrap();
        assert_eq!(body["messages"][0]["role"], "system");
        assert_eq!(
            body["messages"][0]["content"],
            "Be brief.\n\nMemory: likes tea\n\nSkill catalogue"
        );
        assert_eq!(
            body["messages"][1],
            json!({"role": "user", "content": "hi"})
        );
        assert_eq!(body["tools"][0]["function"]["name"], "a_tool");
        assert_eq!(body["tools"][1]["function"]["name"], "b_tool");
        assert_eq!(body["stream"], true);
        assert_eq!(body["max_completion_tokens"], 100);
        assert!(body.get("reasoning_effort").is_none());
    }

    fn image_user() -> LlmRequest {
        let mut content = Content::new("user").with_text("what is wrong here?");
        content = content.with_inline_data("image/png", vec![1, 2, 3]);
        LlmRequest {
            model: "gpt-x".into(),
            contents: vec![content],
            config: None,
            tools: HashMap::new(),
            previous_response_id: None,
        }
    }

    #[test]
    fn a_text_only_user_message_stays_a_plain_string() {
        let request = LlmRequest {
            model: "gpt-x".into(),
            contents: vec![Content::new("user").with_text("hi")],
            config: None,
            tools: HashMap::new(),
            previous_response_id: None,
        };
        for images in [Images::Send, Images::Refuse, Images::Measure] {
            let body =
                request_body(&model_request(), &request, ReasoningReplay::Never, images).unwrap();
            assert_eq!(
                body["messages"][1],
                json!({"role": "user", "content": "hi"}),
                "{images:?}"
            );
        }
    }

    #[test]
    fn a_user_image_becomes_an_image_url_part_after_the_text() {
        let body = request_body(
            &model_request(),
            &image_user(),
            ReasoningReplay::Never,
            Images::Send,
        )
        .unwrap();
        assert_eq!(
            body["messages"][1],
            json!({"role": "user", "content": [
                {"type": "text", "text": "what is wrong here?"},
                {"type": "image_url", "image_url": {"url": "data:image/png;base64,AQID"}},
            ]})
        );
    }

    #[test]
    fn a_model_without_vision_is_never_sent_an_image() {
        let error = request_body(
            &model_request(),
            &image_user(),
            ReasoningReplay::Never,
            Images::Refuse,
        )
        .unwrap_err();
        assert!(error.to_string().contains("model_gateway.model_no_vision"));
    }

    #[test]
    fn a_measured_image_weighs_what_a_provider_bills() {
        let body = request_body(
            &model_request(),
            &image_user(),
            ReasoningReplay::Never,
            Images::Measure,
        )
        .unwrap();
        let url = body["messages"][1]["content"][1]["image_url"]["url"]
            .as_str()
            .unwrap();
        assert_eq!(
            elitea_agent_runtime::context_budget::estimate_tokens(url.len()),
            elitea_agent_runtime::image_result::TOKEN_ESTIMATE + 6
        );
    }

    #[test]
    fn a_tool_image_follows_its_run_of_tool_messages_as_a_host_user_message() {
        let call = |id: &str| Part::FunctionCall {
            name: "read_file".into(),
            args: json!({"path": id}),
            id: Some(id.into()),
            thought_signature: None,
        };
        let response = |id: &str, body: Value| Part::FunctionResponse {
            function_response: adk_core::FunctionResponseData::new("read_file", body),
            id: Some(id.into()),
            annotations: None,
        };
        let request = LlmRequest {
            model: "gpt-x".into(),
            contents: vec![
                Content::new("user").with_text("look"),
                Content {
                    role: "model".into(),
                    parts: vec![call("a"), call("b")],
                },
                Content {
                    role: "function".into(),
                    parts: vec![response(
                        "a",
                        json!({"kind": "image", "mime": "image/png", "bytes": 3, "image_base64": "AQID"}),
                    )],
                },
                Content {
                    role: "function".into(),
                    parts: vec![response("b", json!({"content": "text"}))],
                },
            ],
            config: None,
            tools: HashMap::new(),
            previous_response_id: None,
        };
        let body = request_body(
            &model_request(),
            &request,
            ReasoningReplay::Never,
            Images::Send,
        )
        .unwrap();
        let roles: Vec<(&str, Option<&str>)> = body["messages"]
            .as_array()
            .unwrap()
            .iter()
            .map(|m| (m["role"].as_str().unwrap(), m["name"].as_str()))
            .collect();
        assert_eq!(
            roles,
            [
                ("system", None),
                ("user", None),
                ("assistant", None),
                ("tool", None),
                ("tool", None),
                ("user", Some("tool_image")),
            ]
        );
        let tool = body["messages"][3]["content"].as_str().unwrap();
        assert!(
            tool.contains("\"kind\":\"image\"") && !tool.contains("AQID"),
            "{tool}"
        );
        assert_eq!(
            body["messages"][5]["content"][1]["image_url"]["url"],
            "data:image/png;base64,AQID"
        );
    }

    #[test]
    fn a_stream_of_text_and_a_tool_call_reads_into_one_response() {
        let emitter = Arc::new(VecEmitter::default());
        let events = TurnEvents::new("t".into(), emitter.clone());
        let mut reader = StreamReader::new(Some(&events));
        let stream = concat!(
            "data: {\"choices\":[{\"delta\":{\"content\":\"Let me \"}}]}\n\n",
            "data: {\"choices\":[{\"delta\":{\"content\":\"look.\"}}]}\n\n",
            "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"c1\",\"function\":{\"name\":\"read_file\",\"arguments\":\"{\\\"path\\\":\"}}]}}]}\n\n",
            "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"\\\"a\\\"}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\n",
            "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":4,\"total_tokens\":7}}\n\n",
            "data: [DONE]\n\n",
        );
        assert!(reader.push(stream.as_bytes()).unwrap());
        let (responses, completed) = reader.finish().unwrap();
        assert_eq!(completed, None);
        let response = &responses[0];
        assert!(!response.turn_complete);
        let parts = &response.content.as_ref().unwrap().parts;
        assert!(matches!(&parts[0], Part::Text { text } if text == "Let me look."));
        assert!(
            matches!(&parts[1], Part::FunctionCall { name, args, id, .. }
            if name == "read_file" && args == &json!({"path": "a"}) && id.as_deref() == Some("c1"))
        );
        assert_eq!(
            response.usage_metadata.as_ref().unwrap().total_token_count,
            7
        );
        let all = emitter.all();
        let deltas: Vec<_> = all
            .iter()
            .filter(|e| e.kind == "text_delta")
            .map(|e| e.payload["text"].as_str().unwrap().to_owned())
            .collect();
        assert_eq!(deltas, ["Let me ", "look."]);
        // The call's usage goes to the UI too: what it sent (the context in use) and what it produced.
        let usage: Vec<_> = all.iter().filter(|e| e.kind == "usage").collect();
        assert_eq!(usage.len(), 1);
        assert_eq!(
            usage[0].payload,
            json!({"prompt_tokens": 3, "completion_tokens": 4, "total_tokens": 7})
        );
    }

    #[test]
    fn reasoning_in_history_is_sent_back_on_no_route_yet() {
        let request = LlmRequest {
            model: "gpt-x".into(),
            contents: vec![
                Content::new("user").with_text("hi"),
                Content {
                    role: "model".into(),
                    parts: vec![
                        Part::Thinking {
                            thinking: "secret steps".into(),
                            signature: Some("sig".into()),
                        },
                        Part::Text {
                            text: "hello".into(),
                        },
                    ],
                },
            ],
            config: None,
            tools: HashMap::new(),
            previous_response_id: None,
        };
        for replay in [ReasoningReplay::Never, ReasoningReplay::SignedBlocks] {
            let body = request_body(&model_request(), &request, replay, Images::Send).unwrap();
            assert_eq!(
                body["messages"][2],
                json!({"role": "assistant", "content": "hello"})
            );
            assert!(!body.to_string().contains("secret steps"));
        }
    }

    #[test]
    fn a_stream_without_a_finish_is_incomplete() {
        let emitter = Arc::new(VecEmitter::default());
        let events = TurnEvents::new("t".into(), emitter);
        let mut reader = StreamReader::new(Some(&events));
        reader
            .push(b"data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\n")
            .unwrap();
        assert!(reader.finish().is_err());
    }
}

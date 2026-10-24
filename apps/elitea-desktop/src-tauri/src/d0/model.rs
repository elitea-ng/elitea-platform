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
//! Prompt cache (#1227): the system message is the thread's stable prefix;
//! the turn's volatile context goes in a host message before the person's
//! ([`TurnTail`]); on a model the catalogue marks as a provider of its own
//! the body carries Anthropic `cache_control` breakpoints ([`Layout`]).
//! Each call's usage (cached reads and writes, reasoning) goes to the
//! turn's ledger ([`TurnEvents::model_usage`]).
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
use std::time::Duration;

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
use elitea_llm_wire::openai::{self, DoneMatch, TypeCheck, UsageProfile};
use elitea_llm_wire::refusal::{refusal_code, retryable_status};
use elitea_llm_wire::route::CHAT_COMPLETIONS_ROUTE;
use elitea_llm_wire::sse::{SseOptions, SseSplitter};
use elitea_llm_wire::tool_calls::{
    ToolCallAssembler, ToolCallLimits, ToolCallProfile, parse_lenient_delta,
};
use futures::StreamExt as _;
use serde_json::{Map, Value, json};

use super::api::Credentials;
use super::catalogue::ReasoningReplay;
use super::events::{CallUsage, TurnEvents, UsageLane};
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
    /// usage, no first-token time): the summary model of a compaction. Its
    /// retries are still reported (`model_retry`, nothing to drop), so a
    /// turn that waits on one does not look frozen.
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
    /// Whose calls these are, for the turn's usage ([`UsageLane`]).
    pub lane: UsageLane,
    /// The turn's host-authored context message ([`TurnTail`]): the agent's
    /// own model only, never a summary or a sub-agent.
    pub tail: Option<Arc<TurnTail>>,
    /// Send Anthropic `cache_control` breakpoints ([`Layout::cache_hints`]):
    /// a native Anthropic model ([`super::catalogue::takes_cache_hints`]),
    /// and never on a summary call.
    pub cache_hints: bool,
}

/// The turn's volatile context (#1227: the environment's date and git
/// line, the skills picked for the turn, the memory recall), sent as one
/// host-authored user message (`name: host_context`) right before the
/// person's message of this turn, so the system message and the history
/// before it stay byte-identical from turn to turn and the provider's
/// prompt cache keeps them. It is outside adk's contents, like the system
/// instruction: never stored in the session, so a later turn never carries
/// this turn's recall.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct TurnTail {
    pub text: String,
    /// The text of the person's message it precedes (the newest user
    /// message carrying it is the turn's own; when compaction took that one
    /// away, the tail goes before the newest user message).
    pub anchor: String,
}

/// The `name` of the tail's message.
pub const HOST_CONTEXT_NAME: &str = "host_context";

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
                lane: self.lane,
                tail: self.tail.clone(),
                cache_hints: self.cache_hints,
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
    lane: UsageLane,
    tail: Option<Arc<TurnTail>>,
    cache_hints: bool,
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
        let body = request_body_in(
            &self.request,
            &request,
            Layout {
                replay: self.replay,
                images: if self.vision {
                    Images::Send
                } else {
                    Images::Refuse
                },
                tail: self.tail.as_deref(),
                cache_hints: self.cache_hints,
            },
        )?;
        let started = recorder::now();
        let mut retries = 0_u32;
        let (responses, completed, usage) = loop {
            let mut reader = StreamReader::new((!self.quiet).then_some(&*self.events));
            let failure = match self.call(&body, &mut reader).await {
                Ok(()) => match reader.finish() {
                    Ok(finished) => break finished,
                    Err(failure) => failure,
                },
                Err(failure) => failure,
            };
            let Some(code) = failure.retry else {
                return Err(*failure.error);
            };
            let asked = failure.wait;
            if retries >= MODEL_RETRIES {
                log::warn!(
                    "model call of execution {} failed ({code}) after {MODEL_RETRIES} retries",
                    self.execution_id
                );
                return Err(model_error(code, &gave_up()));
            }
            retries += 1;
            // The gateway's `Retry-After` when it names a longer wait.
            let delay = asked.map_or_else(
                || retry_delay(retries),
                |asked| retry_delay(retries).max(asked.min(MAX_RETRY_AFTER)),
            );
            log::info!(
                "model call of execution {} failed ({code}); retry {retries} of {MODEL_RETRIES} in {} ms",
                self.execution_id,
                delay.as_millis()
            );
            // A quiet model streamed nothing to the webview: its units are 0.
            self.events.model_retry(&ModelRetry {
                attempt: retries,
                max_attempts: MODEL_RETRIES,
                code,
                delay,
                text_units: reader.text_units(),
                thinking_units: reader.thinking_units(),
            });
            tokio::time::sleep(delay).await;
        };
        // Counted whoever called (a summary, a sub-agent): the turn's total
        // is what the gateway logged for its execution id.
        if let Some(usage) = usage {
            self.events
                .model_usage(self.lane, &self.request.model_name, usage);
        }
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

impl GatewayModel {
    /// One call: the request, then its stream fed to `reader` until
    /// `[DONE]` or its end. A failure says whether another call may mend it.
    async fn call(&self, body: &Value, reader: &mut StreamReader<'_>) -> Result<(), CallFailure> {
        let bearer = self.credentials.bearer().await.map_err(|error| {
            CallFailure::fatal(model_error("model_gateway.unauthorized", &error.message))
        })?;
        // No whole-call timeout: an answer may stream for many minutes. The
        // response head must come within `API_TIMEOUT`, and then each chunk
        // within `STREAM_IDLE_TIMEOUT` of the one before.
        let send = self
            .http
            .post(format!("{}{CHAT_COMPLETIONS_ROUTE}", bearer.origin))
            .bearer_auth(&bearer.token)
            .header(PROJECT_HEADER, self.project_id.to_string())
            .header(EXECUTION_HEADER, &self.execution_id)
            .header("Accept", "text/event-stream")
            .json(body)
            .send();
        let response = match tokio::time::timeout(super::api::API_TIMEOUT, send).await {
            Ok(Ok(response)) => response,
            Ok(Err(_)) | Err(_) => {
                return Err(CallFailure::retry(
                    UNAVAILABLE,
                    "could not reach the model gateway",
                ));
            }
        };
        let status = response.status().as_u16();
        if !(200..300).contains(&status) {
            let wait = retry_after(response.headers());
            let body = tokio::time::timeout(super::api::API_TIMEOUT, response.bytes())
                .await
                .ok()
                .and_then(Result::ok)
                .unwrap_or_default();
            let code = if context_length_exceeded(status, &body) {
                CONTEXT_LENGTH_EXCEEDED
            } else {
                refusal_code(status, &body)
            };
            let error = model_error(code, refusal_message(code));
            return Err(
                if code != CONTEXT_LENGTH_EXCEEDED && retryable_status(status) {
                    CallFailure {
                        error: Box::new(error),
                        retry: Some(code),
                        wait,
                    }
                } else {
                    CallFailure::fatal(error)
                },
            );
        }
        let mut bytes = response.bytes_stream();
        loop {
            let chunk = match tokio::time::timeout(STREAM_IDLE_TIMEOUT, bytes.next()).await {
                Ok(Some(Ok(chunk))) => chunk,
                Ok(None) => return Ok(()),
                // The answer already came whole (its finish reason): only the
                // `[DONE]` or the last bytes were lost. Keep it rather than
                // generate (and bill) it again.
                Ok(Some(Err(_))) | Err(_) if reader.has_finish() => return Ok(()),
                Ok(Some(Err(_))) => {
                    return Err(CallFailure::retry(INCOMPLETE_STREAM, CUT_OFF));
                }
                Err(_) => {
                    return Err(CallFailure::retry(
                        INCOMPLETE_STREAM,
                        "the model stream stalled",
                    ));
                }
            };
            if !self.quiet {
                self.events.first_token();
            }
            if reader.push(&chunk).map_err(CallFailure::fatal)? {
                return Ok(());
            }
        }
    }
}

/// How many times one model call is made again after a failure another
/// call may mend (the gateway unreachable, a cut or stalled stream, a 408,
/// 409, 429 or 5xx), before the turn fails with a message naming `/retry`.
pub const MODEL_RETRIES: u32 = 25;

/// The first wait; it doubles with each retry up to [`MAX_RETRY_DELAY`].
#[cfg(not(test))]
const FIRST_RETRY_DELAY: Duration = Duration::from_secs(1);
#[cfg(test)]
const FIRST_RETRY_DELAY: Duration = Duration::from_millis(1);

/// The longest wait between two calls.
#[cfg(not(test))]
const MAX_RETRY_DELAY: Duration = Duration::from_secs(30);
#[cfg(test)]
const MAX_RETRY_DELAY: Duration = Duration::from_millis(4);

/// How long a model stream may send nothing (no chunk, no keep-alive)
/// before the call counts as cut off. Reasoning models can think quietly
/// for minutes.
const STREAM_IDLE_TIMEOUT: Duration = Duration::from_secs(300);

const UNAVAILABLE: &str = "model_gateway.unavailable";
const INCOMPLETE_STREAM: &str = "model_gateway.incomplete_stream";
const CUT_OFF: &str = "the model stream was cut off";

/// The message of a call that failed [`MODEL_RETRIES`] more times: the
/// person sends `/retry` to run the turn's message again.
fn gave_up() -> String {
    format!("the model call failed {MODEL_RETRIES} more times; send /retry to try again")
}

/// The longest `Retry-After` wait the host keeps.
#[cfg(not(test))]
const MAX_RETRY_AFTER: Duration = Duration::from_secs(120);
#[cfg(test)]
const MAX_RETRY_AFTER: Duration = Duration::from_millis(20);

/// A refusal's `Retry-After` in whole seconds (an HTTP date is not read).
fn retry_after(headers: &reqwest::header::HeaderMap) -> Option<Duration> {
    headers
        .get(reqwest::header::RETRY_AFTER)?
        .to_str()
        .ok()?
        .trim()
        .parse::<u64>()
        .ok()
        .map(Duration::from_secs)
}

/// The wait before retry `retry` (from 1).
fn retry_delay(retry: u32) -> Duration {
    let factor = 1_u32 << retry.saturating_sub(1).min(16);
    FIRST_RETRY_DELAY
        .saturating_mul(factor)
        .min(MAX_RETRY_DELAY)
}

/// Why one call failed.
#[derive(Debug)]
struct CallFailure {
    /// Boxed: `AdkError` is large, and the failure is the cold path.
    error: Box<AdkError>,
    /// The code of a failure another call may mend; `None`: final.
    retry: Option<&'static str>,
    /// How long the gateway asked to wait (`Retry-After`).
    wait: Option<Duration>,
}

impl CallFailure {
    fn fatal(error: AdkError) -> Self {
        Self {
            error: Box::new(error),
            retry: None,
            wait: None,
        }
    }

    fn retry(code: &'static str, message: &str) -> Self {
        Self {
            error: Box::new(model_error(code, message)),
            retry: Some(code),
            wait: None,
        }
    }
}

/// A retry, as the `model_retry` event reports it.
pub struct ModelRetry {
    /// The retry, from 1.
    pub attempt: u32,
    pub max_attempts: u32,
    pub code: &'static str,
    pub delay: Duration,
    /// What the failed call streamed to the webview, in UTF-16 code units
    /// (the webview's string length): its text and its reasoning, which
    /// the webview drops.
    pub text_units: usize,
    pub thinking_units: usize,
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

/// How a request body is laid out beyond its contents.
#[derive(Clone, Copy, Debug)]
pub(super) struct Layout<'a> {
    pub replay: ReasoningReplay,
    pub images: Images,
    /// The turn's context message, placed before the person's message.
    pub tail: Option<&'a TurnTail>,
    /// Anthropic prompt-cache breakpoints (#1227 stage 2): the gateway's
    /// Bifrost core (v1.7.15) carries `cache_control` on a content part
    /// (the system message's included, hoisted into Anthropic's `system`)
    /// and on a tool to the Anthropic API, and reports the cache writes
    /// back as `prompt_tokens_details.cache_write_tokens`. Three of
    /// Anthropic's four: the stable system message, the last tool, and the
    /// request's last message (so the next step reads the whole prefix).
    pub cache_hints: bool,
}

/// The `cache_control` of a breakpoint.
fn ephemeral() -> Value {
    json!({"type": "ephemeral"})
}

/// `message` with a breakpoint on its last content part (a text content
/// becomes one text part); a message without content is left as it is.
fn mark_last_part(message: &mut Value) {
    match message.get_mut("content") {
        Some(Value::String(text)) if !text.is_empty() => {
            let text = std::mem::take(text);
            message["content"] =
                json!([{"type": "text", "text": text, "cache_control": ephemeral()}]);
        }
        Some(Value::Array(parts)) => {
            if let Some(part) = parts.last_mut().and_then(Value::as_object_mut) {
                part.insert("cache_control".into(), ephemeral());
            }
        }
        _ => {}
    }
}

/// The OpenAI-compatible body, without a tail or cache breakpoints (the
/// tests' shape).
#[cfg(test)]
pub(super) fn request_body(
    model: &ModelRequest,
    request: &LlmRequest,
    replay: ReasoningReplay,
    images: Images,
) -> adk_core::Result<Value> {
    request_body_in(
        model,
        request,
        Layout {
            replay,
            images,
            tail: None,
            cache_hints: false,
        },
    )
}

/// The text of a user content, as [`TurnTail::anchor`] names it.
fn user_text(content: &Content) -> String {
    content
        .parts
        .iter()
        .filter_map(|part| match part {
            Part::Text { text } => Some(text.as_str()),
            _ => None,
        })
        .collect::<Vec<_>>()
        .join("\n")
}

/// The OpenAI-compatible body (the worker facade's shape), laid out by
/// `layout`.
pub(super) fn request_body_in(
    model: &ModelRequest,
    request: &LlmRequest,
    layout: Layout<'_>,
) -> adk_core::Result<Value> {
    let Layout {
        replay,
        images,
        tail,
        cache_hints,
    } = layout;
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
        let mut message = json!({"role": "system", "content": system});
        if cache_hints {
            mark_last_part(&mut message);
        }
        messages.push(message);
    }
    let first_turn_message = messages.len();
    // Where the tail goes: before the newest user message that is the
    // turn's own, else before the newest user message.
    let mut anchored: Option<usize> = None;
    let mut newest_user: Option<usize> = None;
    // Chat completions' tool messages are text-only: an image a tool
    // returned follows the run of tool messages it belongs to, as a user
    // message the host wrote (`name: tool_image`).
    let mut tool_images: Vec<Value> = Vec::new();
    for content in request.contents.iter().filter(|c| c.role != "system") {
        if !matches!(content.role.as_str(), "function" | "tool") {
            messages.append(&mut tool_images);
        }
        if content.role == "user" {
            newest_user = Some(messages.len());
            if tail.is_some_and(|tail| user_text(content) == tail.anchor) {
                anchored = Some(messages.len());
            }
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
    if let Some(tail) = tail.filter(|tail| !tail.text.is_empty()) {
        let at = anchored.or(newest_user).unwrap_or(first_turn_message);
        messages.insert(
            at,
            json!({"role": "user", "name": HOST_CONTEXT_NAME, "content": tail.text}),
        );
    }
    if cache_hints
        && messages.len() > first_turn_message
        && let Some(last) = messages.last_mut()
    {
        mark_last_part(last);
    }
    let mut body = Map::new();
    body.insert("model".into(), json!(model.model_name));
    body.insert("messages".into(), Value::Array(messages));
    if !request.tools.is_empty() {
        let mut declared = tools(&request.tools).ok_or_else(invalid)?;
        if cache_hints
            && let Some(last) = declared
                .as_array_mut()
                .and_then(|tools| tools.last_mut())
                .and_then(Value::as_object_mut)
        {
            last.insert("cache_control".into(), ephemeral());
        }
        body.insert("tools".into(), declared);
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
                    // Nor a signed thinking block: the desktop speaks to
                    // every model, Anthropic's too (with cache breakpoints,
                    // #1227), through this route, which has no place for it.
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

/// adk's usage record of a call: the cached reads and writes and the
/// reasoning as adk-core names them.
fn usage_metadata(usage: &CallUsage) -> UsageMetadata {
    let count = |n: u64| i32::try_from(n).unwrap_or(i32::MAX);
    UsageMetadata {
        prompt_token_count: count(usage.prompt),
        candidates_token_count: count(usage.completion),
        total_token_count: count(usage.total),
        cache_read_input_token_count: usage.cached.map(count),
        cache_creation_input_token_count: usage.cache_write.map(count),
        thinking_token_count: usage.reasoning.map(count),
        ..UsageMetadata::default()
    }
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
    usage: Option<CallUsage>,
    done: bool,
    /// What went to the webview, in UTF-16 code units ([`ModelRetry`]).
    text_units: usize,
    thinking_units: usize,
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
            text_units: 0,
            thinking_units: 0,
        }
    }

    /// The stream named its finish reason: the answer is whole.
    fn has_finish(&self) -> bool {
        self.finish.is_some()
    }

    fn text_units(&self) -> usize {
        self.text_units
    }

    fn thinking_units(&self) -> usize {
        self.thinking_units
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
        if let Ok(Some(usage)) = openai::parse_usage(chunk.get("usage"), UsageProfile::Lenient) {
            // The gateway names a cache write `cache_write_tokens` (Bifrost's
            // OpenAI-SDK spelling) and also `cached_write_tokens`.
            let cache_write = chunk["usage"]
                .get("prompt_tokens_details")
                .and_then(|details| {
                    details
                        .get("cache_write_tokens")
                        .or_else(|| details.get("cached_write_tokens"))
                })
                .and_then(Value::as_u64);
            self.usage = Some(CallUsage {
                prompt: usage.prompt_tokens,
                completion: usage.completion_tokens,
                total: usage.total_tokens,
                cached: usage.cached_tokens,
                cache_write,
                reasoning: usage.reasoning_tokens,
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
                    self.thinking_units += reasoning.encode_utf16().count();
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
                    self.text_units += text.encode_utf16().count();
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

    /// The call's answer ([`StreamReader::answer`]). A stream that ended
    /// before its finish reason is cut off (another call may mend it); a
    /// malformed one is final.
    #[allow(clippy::type_complexity)]
    fn finish(
        &mut self,
    ) -> Result<(Vec<LlmResponse>, Option<String>, Option<CallUsage>), CallFailure> {
        let mut tail = Vec::new();
        self.splitter
            .finish_into(&mut tail)
            .map_err(|_| CallFailure::fatal(Self::malformed()))?;
        for event in tail {
            self.event(&event.data).map_err(CallFailure::fatal)?;
        }
        let Some(finish) = self.finish else {
            return Err(CallFailure::retry(
                INCOMPLETE_STREAM,
                "the model stream ended before the answer was complete",
            ));
        };
        self.answer(finish).map_err(CallFailure::fatal)
    }

    /// The adk responses, for an answer without tool calls its text, and
    /// the call's usage when the gateway reported it.
    #[allow(clippy::type_complexity)]
    fn answer(
        &mut self,
        finish: openai::FinishReason,
    ) -> adk_core::Result<(Vec<LlmResponse>, Option<String>, Option<CallUsage>)> {
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
            let calls = std::mem::replace(
                &mut self.calls,
                ToolCallAssembler::new(ToolCallProfile::Lenient, TOOL_CALL_LIMITS),
            );
            for call in calls.finish().map_err(|_| Self::malformed())? {
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
        let completed = (!has_calls).then(|| self.text.clone());
        let response = LlmResponse {
            content: (!parts.is_empty()).then(|| Content {
                role: "model".to_owned(),
                parts,
            }),
            usage_metadata: self.usage.map(|usage| usage_metadata(&usage)),
            finish_reason: Some(finish_reason),
            partial: false,
            turn_complete: !has_calls,
            ..LlmResponse::default()
        };
        Ok((vec![response], completed, self.usage))
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
    fn the_retry_wait_doubles_up_to_its_cap() {
        let waits: Vec<u128> = (1..=5).map(|n| retry_delay(n).as_millis()).collect();
        assert_eq!(waits, [1, 2, 4, 4, 4]);
        assert_eq!(retry_delay(MODEL_RETRIES), MAX_RETRY_DELAY);
        assert!(gave_up().contains("/retry"));
    }

    #[test]
    fn retry_after_is_read_in_whole_seconds_only() {
        let header = |value: &str| {
            let mut headers = reqwest::header::HeaderMap::new();
            headers.insert(reqwest::header::RETRY_AFTER, value.parse().unwrap());
            retry_after(&headers)
        };
        assert_eq!(header("12"), Some(Duration::from_secs(12)));
        assert_eq!(header(" 3 "), Some(Duration::from_secs(3)));
        assert_eq!(header("Wed, 21 Oct 2026 07:28:00 GMT"), None);
        assert_eq!(header("-1"), None);
        assert_eq!(retry_after(&reqwest::header::HeaderMap::new()), None);
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
        let (responses, completed, usage) = reader.finish().unwrap();
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
        // The call's usage is handed back (the model counts it, `model_usage`).
        assert_eq!(
            usage,
            Some(CallUsage {
                prompt: 3,
                completion: 4,
                total: 7,
                ..CallUsage::default()
            })
        );
    }

    #[test]
    fn cached_cache_write_and_reasoning_tokens_are_read_and_kept_in_adk_usage() {
        let mut reader = StreamReader::new(None);
        let stream = concat!(
            "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\n",
            "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":2000,\"completion_tokens\":50,\"total_tokens\":2050,",
            "\"prompt_tokens_details\":{\"cached_tokens\":1536,\"cache_write_tokens\":300},",
            "\"completion_tokens_details\":{\"reasoning_tokens\":20}}}\n\n",
            "data: [DONE]\n\n",
        );
        assert!(reader.push(stream.as_bytes()).unwrap());
        let (responses, _, usage) = reader.finish().unwrap();
        let usage = usage.unwrap();
        assert_eq!(usage.cached, Some(1536));
        assert_eq!(usage.cache_write, Some(300));
        assert_eq!(usage.reasoning, Some(20));
        let adk = responses[0].usage_metadata.as_ref().unwrap();
        assert_eq!(adk.cache_read_input_token_count, Some(1536));
        assert_eq!(adk.cache_creation_input_token_count, Some(300));
        assert_eq!(adk.thinking_token_count, Some(20));
    }

    fn layout<'a>(tail: Option<&'a TurnTail>, cache_hints: bool) -> Layout<'a> {
        Layout {
            replay: ReasoningReplay::Never,
            images: Images::Send,
            tail,
            cache_hints,
        }
    }

    fn thread_request() -> LlmRequest {
        LlmRequest {
            model: "gpt-x".into(),
            contents: vec![
                Content::new("user").with_text("earlier question"),
                Content {
                    role: "model".into(),
                    parts: vec![Part::Text {
                        text: "earlier answer".into(),
                    }],
                },
                Content::new("user").with_text("fix the test"),
                Content {
                    role: "model".into(),
                    parts: vec![Part::FunctionCall {
                        name: "read_file".into(),
                        args: json!({"path": "a"}),
                        id: Some("c1".into()),
                        thought_signature: None,
                    }],
                },
                Content {
                    role: "function".into(),
                    parts: vec![Part::FunctionResponse {
                        function_response: adk_core::FunctionResponseData::new(
                            "read_file",
                            json!({"content": "x"}),
                        ),
                        id: Some("c1".into()),
                        annotations: None,
                    }],
                },
            ],
            config: None,
            tools: HashMap::new(),
            previous_response_id: None,
        }
    }

    #[test]
    fn the_tail_goes_right_before_the_turns_own_message_at_every_step() {
        let tail = TurnTail {
            text: "## Environment (this turn)\n- Today: 2026-10-23\n\nMemory: tea.".into(),
            anchor: "fix the test".into(),
        };
        let body = request_body_in(
            &model_request(),
            &thread_request(),
            layout(Some(&tail), false),
        )
        .unwrap();
        let messages = body["messages"].as_array().unwrap();
        assert_eq!(messages[0]["content"], "Be brief.\n\nMemory: likes tea");
        assert_eq!(messages[3]["content"], json!(tail.text));
        assert_eq!(messages[3]["role"], "user");
        assert_eq!(messages[3]["name"], HOST_CONTEXT_NAME);
        assert_eq!(messages[4]["content"], "fix the test");
        assert_eq!(messages.len(), 7);
        // The turn's message compacted away: before the newest user message.
        let mut request = thread_request();
        request.contents.remove(2);
        let body = request_body_in(&model_request(), &request, layout(Some(&tail), false)).unwrap();
        assert_eq!(body["messages"][1]["name"], HOST_CONTEXT_NAME);
        // Without a tail the body is the plain one.
        assert_eq!(
            request_body_in(&model_request(), &thread_request(), layout(None, false)).unwrap(),
            request_body(
                &model_request(),
                &thread_request(),
                ReasoningReplay::Never,
                Images::Send
            )
            .unwrap()
        );
    }

    #[test]
    fn cache_hints_mark_the_system_message_the_last_tool_and_the_last_message() {
        let mut request = thread_request();
        request
            .tools
            .insert("b_tool".into(), json!({"name": "b_tool"}));
        request
            .tools
            .insert("a_tool".into(), json!({"name": "a_tool"}));
        let body = request_body_in(&model_request(), &request, layout(None, true)).unwrap();
        let ephemeral = json!({"type": "ephemeral"});
        assert_eq!(
            body["messages"][0]["content"],
            json!([{"type": "text", "text": "Be brief.\n\nMemory: likes tea", "cache_control": ephemeral}])
        );
        let last = body["messages"].as_array().unwrap().last().unwrap();
        assert_eq!(last["role"], "tool");
        assert_eq!(last["content"][0]["cache_control"], ephemeral);
        assert_eq!(body["tools"][1]["function"]["name"], "b_tool");
        assert_eq!(body["tools"][1]["cache_control"], ephemeral);
        assert!(body["tools"][0].get("cache_control").is_none());
        // The other messages are as without hints.
        let plain = request_body_in(&model_request(), &request, layout(None, false)).unwrap();
        assert_eq!(
            body["messages"].as_array().unwrap()[1..4],
            plain["messages"].as_array().unwrap()[1..4]
        );
        assert!(!plain.to_string().contains("cache_control"));
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

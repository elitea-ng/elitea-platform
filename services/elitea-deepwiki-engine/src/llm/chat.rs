//! Chat completions from the gateway's OpenAI-compatible
//! `/chat/completions`, with tool calling, in one shot or as an SSE stream
//! (ADR-0026 decision 8).
//!
//! The request is what `LangChain`'s `ChatOpenAI` sent for the Python
//! engine: `model`, `messages`, `temperature`, `max_completion_tokens`
//! (`LangChain` renames `max_tokens` since its September 2024 release), and
//! `developer` in place of `system` for `o1`/`o3`-style model names. A
//! stream also asks for `stream_options.include_usage`, so a streamed call
//! reports usage as a blocking one does.
//!
//! A system message holds only an embedded, `'static` prompt. Repository
//! text — code, file names, symbols, anything a repository author chose —
//! goes in user messages (or tool results), never in the system prompt:
//! the type makes the rule hold without a review.

use super::settings::ModelSettings;
use super::sse::{SseDecoder, SseEvent, SseLimits};
use super::transport::{BodyError, Call, Transport, read_limited};
use crate::errors::{EngineError, ErrorType};
use crate::runner::StopSignal;
use serde_json::{Map, Value, json};
use std::time::Instant;

/// A blocking completion's body cap: a 64k-token answer is well under
/// 1 MiB of JSON; tool arguments add little.
const MAX_RESPONSE_BYTES: usize = 16 * 1024 * 1024;

/// Tool calls one answer may make.
const MAX_TOOL_CALLS: usize = 128;

/// One tool call's assembled arguments.
const MAX_TOOL_ARGUMENT_BYTES: usize = 1024 * 1024;

/// The sampling policy of a call site.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Sampling {
    /// Page generation, ask, deep research: 0.1.
    Default,
    /// `resolve_wiki` and the structure planner: 0.0.
    Deterministic,
}

impl Sampling {
    /// The temperature for `model`. A model name starting with `o` gets
    /// 1.0 whatever the call site wants: the o-series refuses any other
    /// value (the Python workers' rule, and `wiki_query.create_llm`'s).
    #[must_use]
    pub fn temperature(self, model: &str) -> f64 {
        if model.starts_with('o') {
            return 1.0;
        }
        match self {
            Self::Default => 0.1,
            Self::Deterministic => 0.0,
        }
    }
}

/// One message of the conversation.
#[derive(Debug, Clone, PartialEq)]
pub enum ChatMessage {
    /// An embedded prompt. `'static` on purpose: see the module comment.
    System(&'static str),
    User(String),
    Assistant {
        content: Option<String>,
        tool_calls: Vec<ToolCall>,
    },
    Tool {
        tool_call_id: String,
        content: String,
    },
}

/// A tool the model may call.
#[derive(Debug, Clone, PartialEq)]
pub struct ToolDefinition {
    pub name: String,
    pub description: String,
    /// A JSON Schema object.
    pub parameters: Value,
}

/// Whether, and which, tool the model must call.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum ToolChoice {
    Auto,
    Required,
    None,
    Function(String),
}

/// A tool call the model made.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ToolCall {
    pub id: String,
    pub name: String,
    /// The raw JSON text the model wrote; see [`ToolCall::parsed_arguments`].
    pub arguments: String,
}

impl ToolCall {
    /// The arguments as a JSON object. An empty string is `{}` (a call of
    /// a tool without parameters).
    ///
    /// # Errors
    ///
    /// A `ValueError` when the model wrote something other than a JSON
    /// object; the agent loop reports it back to the model as the tool's
    /// result.
    pub fn parsed_arguments(&self) -> Result<Map<String, Value>, EngineError> {
        if self.arguments.trim().is_empty() {
            return Ok(Map::new());
        }
        match serde_json::from_str(&self.arguments) {
            Ok(Value::Object(map)) => Ok(map),
            _ => Err(EngineError::new(
                ErrorType::Value,
                format!(
                    "the arguments of tool call '{}' are not a JSON object",
                    self.name
                ),
            )),
        }
    }
}

/// Token usage the gateway reported.
#[derive(Debug, Clone, Copy, Default, PartialEq, Eq)]
pub struct Usage {
    pub prompt_tokens: u64,
    pub completion_tokens: u64,
    pub total_tokens: u64,
}

/// One call.
#[derive(Debug, Clone, PartialEq)]
pub struct ChatRequest {
    pub messages: Vec<ChatMessage>,
    pub tools: Vec<ToolDefinition>,
    pub tool_choice: Option<ToolChoice>,
    pub sampling: Sampling,
    /// Overrides `llm_settings.max_tokens` for this call (the planner's
    /// 4096, for one).
    pub max_tokens: Option<u32>,
}

impl ChatRequest {
    /// A request with no tools, default sampling and the settings' budget.
    #[must_use]
    pub fn new(messages: Vec<ChatMessage>) -> Self {
        Self {
            messages,
            tools: Vec::new(),
            tool_choice: None,
            sampling: Sampling::Default,
            max_tokens: None,
        }
    }
}

/// The model's answer.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct ChatResponse {
    pub content: String,
    pub tool_calls: Vec<ToolCall>,
    pub finish_reason: Option<String>,
    pub usage: Option<Usage>,
}

/// The chat client of one invocation.
#[derive(Debug, Clone)]
pub struct ChatClient {
    transport: Transport,
    settings: ModelSettings,
    url: String,
    sse: SseLimits,
}

fn valid_tool_name(name: &str) -> bool {
    (1..=64).contains(&name.len())
        && name
            .bytes()
            .all(|b| b.is_ascii_alphanumeric() || b == b'_' || b == b'-')
}

fn invalid(message: impl Into<String>) -> EngineError {
    EngineError::new(ErrorType::Value, message)
}

fn tool_call_json(call: &ToolCall) -> Value {
    json!({
        "id": call.id,
        "type": "function",
        "function": {"name": call.name, "arguments": call.arguments},
    })
}

/// `^o\d` — `LangChain`'s test for the `developer` role.
fn uses_developer_role(model: &str) -> bool {
    let mut chars = model.chars();
    chars.next() == Some('o') && chars.next().is_some_and(|c| c.is_ascii_digit())
}

impl ChatClient {
    #[must_use]
    pub fn new(transport: Transport, settings: ModelSettings) -> Self {
        let url = format!("{}/chat/completions", settings.api_base);
        Self {
            transport,
            settings,
            url,
            sse: SseLimits::default(),
        }
    }

    /// Replace the SSE caps (tests use small ones).
    #[must_use]
    pub fn with_sse_limits(mut self, limits: SseLimits) -> Self {
        self.sse = limits;
        self
    }

    #[must_use]
    pub fn settings(&self) -> &ModelSettings {
        &self.settings
    }

    fn call(&self, streaming: bool) -> Call<'_> {
        Call {
            what: "Chat completion",
            url: &self.url,
            model: &self.settings.model_name,
            key: &self.settings.api_key,
            organization: self.settings.organization.as_deref(),
            max_retries: self.settings.max_retries,
            streaming,
        }
    }

    /// The request body.
    ///
    /// # Errors
    ///
    /// A `ValueError` for a tool definition the API would refuse.
    pub fn body(&self, request: &ChatRequest, stream: bool) -> Result<Value, EngineError> {
        let model = &self.settings.model_name;
        let system_role = if uses_developer_role(model) {
            "developer"
        } else {
            "system"
        };
        let messages: Vec<Value> = request
            .messages
            .iter()
            .map(|message| match message {
                ChatMessage::System(text) => json!({"role": system_role, "content": text}),
                ChatMessage::User(text) => json!({"role": "user", "content": text}),
                ChatMessage::Assistant {
                    content,
                    tool_calls,
                } => {
                    let mut value = json!({"role": "assistant", "content": content});
                    if !tool_calls.is_empty() {
                        value["tool_calls"] = tool_calls.iter().map(tool_call_json).collect();
                    }
                    value
                }
                ChatMessage::Tool {
                    tool_call_id,
                    content,
                } => json!({"role": "tool", "tool_call_id": tool_call_id, "content": content}),
            })
            .collect();
        let mut body = json!({
            "model": model,
            "messages": messages,
            "temperature": request.sampling.temperature(model),
            "max_completion_tokens": request.max_tokens.unwrap_or(self.settings.max_tokens),
            "stream": stream,
        });
        if stream {
            body["stream_options"] = json!({"include_usage": true});
        }
        if !request.tools.is_empty() {
            let mut tools = Vec::with_capacity(request.tools.len());
            for tool in &request.tools {
                if !valid_tool_name(&tool.name) {
                    return Err(invalid(format!(
                        "tool name '{}' must be 1-64 letters, digits, '_' or '-'",
                        tool.name
                    )));
                }
                if !tool.parameters.is_object() {
                    return Err(invalid(format!(
                        "the parameters of tool '{}' must be a JSON Schema object",
                        tool.name
                    )));
                }
                tools.push(json!({
                    "type": "function",
                    "function": {
                        "name": tool.name,
                        "description": tool.description,
                        "parameters": tool.parameters,
                    },
                }));
            }
            body["tools"] = Value::Array(tools);
        }
        if let Some(choice) = &request.tool_choice {
            body["tool_choice"] = match choice {
                ToolChoice::Auto => json!("auto"),
                ToolChoice::Required => json!("required"),
                ToolChoice::None => json!("none"),
                ToolChoice::Function(name) => {
                    json!({"type": "function", "function": {"name": name}})
                }
            };
        }
        Ok(body)
    }

    /// Stream when `llm_settings.streaming` is on (the default), else one
    /// blocking call; `on_text` sees the answer's fragments either way.
    ///
    /// # Errors
    ///
    /// See [`ChatClient::stream`].
    pub async fn chat(
        &self,
        request: &ChatRequest,
        stop: &StopSignal,
        on_text: &mut (dyn FnMut(&str) + Send),
    ) -> Result<ChatResponse, EngineError> {
        if self.settings.streaming {
            self.stream(request, stop, on_text).await
        } else {
            let response = self.complete(request, stop).await?;
            if !response.content.is_empty() {
                on_text(&response.content);
            }
            Ok(response)
        }
    }

    /// One blocking completion.
    ///
    /// # Errors
    ///
    /// A refusal or transport failure after the retries (see
    /// `transport`), a malformed answer, or the stop line.
    pub async fn complete(
        &self,
        request: &ChatRequest,
        stop: &StopSignal,
    ) -> Result<ChatResponse, EngineError> {
        let call = self.call(false);
        let body = encode(&self.body(request, false)?)?;
        let mut response = self.transport.post(&call, body, stop).await?;
        let timeout = self.transport.timeouts().request;
        let read = tokio::select! {
            read = read_limited(&mut response, MAX_RESPONSE_BYTES, timeout) => read,
            () = stop.stopped() => return Err(EngineError::cancelled()),
        };
        let bytes = match read {
            Ok(bytes) => bytes,
            Err(BodyError::Timeout) => return Err(call.timeout_error(timeout)),
            Err(BodyError::TooLarge) => {
                return Err(call.protocol_error("the response exceeded its size cap"));
            }
            Err(BodyError::Transport(detail)) => {
                return Err(call.protocol_error(&format!("the response broke off: {detail}")));
            }
        };
        let value: Value = serde_json::from_slice(&bytes)
            .map_err(|_| call.protocol_error("the response is not JSON"))?;
        parse_completion(&value).map_err(|detail| call.protocol_error(&detail))
    }

    /// One streamed completion. `on_text` is called with each fragment of
    /// the answer as it arrives; tool calls are assembled from their
    /// deltas and returned whole.
    ///
    /// A stream is retried only before its first byte: once a fragment
    /// reached `on_text`, a retry would repeat it.
    ///
    /// # Errors
    ///
    /// As [`ChatClient::complete`], plus a stream that breaks a cap, stays
    /// silent past the idle timeout, or ends before the model finished.
    pub async fn stream(
        &self,
        request: &ChatRequest,
        stop: &StopSignal,
        on_text: &mut (dyn FnMut(&str) + Send),
    ) -> Result<ChatResponse, EngineError> {
        let call = self.call(true);
        let body = encode(&self.body(request, true)?)?;
        let started = Instant::now();
        let timeouts = self.transport.timeouts();
        let mut response = self.transport.post(&call, body, stop).await?;
        let deadline = tokio::time::Instant::from_std(started + timeouts.stream_total);
        let mut decoder = SseDecoder::new(self.sse);
        let mut assembler = Assembler::default();
        let mut events = Vec::new();
        loop {
            let chunk = tokio::select! {
                chunk = tokio::time::timeout(timeouts.stream_idle, response.chunk()) => chunk,
                () = tokio::time::sleep_until(deadline) => {
                    return Err(call.timeout_error(timeouts.stream_total));
                }
                () = stop.stopped() => return Err(EngineError::cancelled()),
            };
            let chunk = match chunk {
                Err(_) => return Err(call.timeout_error(timeouts.stream_idle)),
                Ok(Err(error)) if error.is_timeout() => {
                    return Err(call.timeout_error(timeouts.stream_idle));
                }
                Ok(Err(error)) => {
                    return Err(call.protocol_error(&format!(
                        "the stream broke off: {}",
                        super::transport::error_chain(&error)
                    )));
                }
                Ok(Ok(chunk)) => chunk,
            };
            match &chunk {
                Some(bytes) => decoder.push(bytes, &mut events),
                None => decoder.finish(&mut events),
            }
            .map_err(|error| call.protocol_error(&error.to_string()))?;
            for event in events.drain(..) {
                assembler
                    .event(&event, on_text)
                    .map_err(|detail| call.protocol_error(&detail))?;
                if assembler.done {
                    return assembler
                        .finish()
                        .map_err(|detail| call.protocol_error(&detail));
                }
            }
            if chunk.is_none() {
                if assembler.finish_reason.is_none() {
                    return Err(call.protocol_error("the stream ended before the model finished"));
                }
                return assembler
                    .finish()
                    .map_err(|detail| call.protocol_error(&detail));
            }
        }
    }
}

fn encode(body: &Value) -> Result<Vec<u8>, EngineError> {
    serde_json::to_vec(body)
        .map_err(|_| EngineError::new(ErrorType::Runtime, "the chat request cannot be encoded"))
}

fn usage_of(value: &Value) -> Option<Usage> {
    let usage = value.get("usage").filter(|u| u.is_object())?;
    let field = |name: &str| usage.get(name).and_then(Value::as_u64).unwrap_or(0);
    Some(Usage {
        prompt_tokens: field("prompt_tokens"),
        completion_tokens: field("completion_tokens"),
        total_tokens: field("total_tokens"),
    })
}

/// An `{"error": …}` object in a body or an event.
fn reported_error(value: &Value) -> Option<String> {
    let error = value.get("error").filter(|e| !e.is_null())?;
    Some(
        error
            .get("message")
            .and_then(Value::as_str)
            .or_else(|| error.as_str())
            .unwrap_or("no message")
            .to_owned(),
    )
}

fn parse_completion(value: &Value) -> Result<ChatResponse, String> {
    if let Some(message) = reported_error(value) {
        return Err(format!("the gateway reported an error: {message}"));
    }
    let choice = value
        .get("choices")
        .and_then(Value::as_array)
        .and_then(|choices| choices.first())
        .ok_or("the response has no choices")?;
    let message = choice.get("message").ok_or("the choice has no message")?;
    let content = match message.get("content") {
        None | Some(Value::Null) => String::new(),
        Some(Value::String(text)) => text.clone(),
        Some(_) => return Err("the message content is not text".to_owned()),
    };
    let mut tool_calls = Vec::new();
    if let Some(calls) = message.get("tool_calls").filter(|c| !c.is_null()) {
        let calls = calls.as_array().ok_or("tool_calls is not a list")?;
        if calls.len() > MAX_TOOL_CALLS {
            return Err(format!(
                "the answer makes more than {MAX_TOOL_CALLS} tool calls"
            ));
        }
        for (index, call) in calls.iter().enumerate() {
            let function = call.get("function").ok_or("a tool call has no function")?;
            let name = function
                .get("name")
                .and_then(Value::as_str)
                .filter(|n| !n.is_empty())
                .ok_or("a tool call has no name")?;
            let arguments = match function.get("arguments") {
                None | Some(Value::Null) => String::new(),
                Some(Value::String(text)) => text.clone(),
                // A few servers send the object itself.
                Some(other) => other.to_string(),
            };
            if arguments.len() > MAX_TOOL_ARGUMENT_BYTES {
                return Err("a tool call's arguments exceed their size cap".to_owned());
            }
            let id = call
                .get("id")
                .and_then(Value::as_str)
                .filter(|id| !id.is_empty())
                .map_or_else(|| format!("call_{index}"), str::to_owned);
            tool_calls.push(ToolCall {
                id,
                name: name.to_owned(),
                arguments,
            });
        }
    }
    Ok(ChatResponse {
        content,
        tool_calls,
        finish_reason: choice
            .get("finish_reason")
            .and_then(Value::as_str)
            .map(str::to_owned),
        usage: usage_of(value),
    })
}

#[derive(Debug, Default)]
struct PartialCall {
    id: String,
    name: String,
    arguments: String,
}

/// Builds one answer from the stream's deltas.
#[derive(Debug, Default)]
struct Assembler {
    content: String,
    calls: Vec<PartialCall>,
    finish_reason: Option<String>,
    usage: Option<Usage>,
    done: bool,
}

impl Assembler {
    fn event(
        &mut self,
        event: &SseEvent,
        on_text: &mut (dyn FnMut(&str) + Send),
    ) -> Result<(), String> {
        let data = event.data.trim();
        if data == "[DONE]" {
            self.done = true;
            return Ok(());
        }
        if data.is_empty() {
            return Ok(());
        }
        let value: Value = serde_json::from_str(data)
            .map_err(|_| "the stream sent an event that is not JSON".to_owned())?;
        if let Some(message) = reported_error(&value) {
            return Err(format!(
                "the gateway reported an error mid-stream: {message}"
            ));
        }
        if event.event == "error" {
            return Err("the gateway reported an error mid-stream".to_owned());
        }
        if let Some(usage) = usage_of(&value) {
            self.usage = Some(usage);
        }
        let Some(choice) = value
            .get("choices")
            .and_then(Value::as_array)
            .and_then(|choices| choices.first())
        else {
            // The usage-only chunk, or a keep-alive object.
            return Ok(());
        };
        if let Some(reason) = choice.get("finish_reason").and_then(Value::as_str) {
            self.finish_reason = Some(reason.to_owned());
        }
        let Some(delta) = choice.get("delta") else {
            return Ok(());
        };
        if let Some(text) = delta.get("content").and_then(Value::as_str)
            && !text.is_empty()
        {
            self.content.push_str(text);
            on_text(text);
        }
        if let Some(calls) = delta.get("tool_calls").filter(|c| !c.is_null()) {
            let calls = calls.as_array().ok_or("a tool_calls delta is not a list")?;
            for call in calls {
                self.tool_delta(call)?;
            }
        }
        Ok(())
    }

    fn tool_delta(&mut self, delta: &Value) -> Result<(), String> {
        let id = delta.get("id").and_then(Value::as_str).unwrap_or("");
        // `index` names the call a fragment belongs to. A server that
        // omits it sends each call whole: a new id starts a new call.
        let index = match delta.get("index").and_then(Value::as_u64) {
            Some(index) => {
                usize::try_from(index).map_err(|_| "a tool call index is out of range")?
            }
            None => match self.calls.last() {
                Some(last) if id.is_empty() || last.id == id => self.calls.len() - 1,
                _ => self.calls.len(),
            },
        };
        if index >= MAX_TOOL_CALLS {
            return Err(format!(
                "the answer makes more than {MAX_TOOL_CALLS} tool calls"
            ));
        }
        if index >= self.calls.len() {
            self.calls.resize_with(index + 1, PartialCall::default);
        }
        let call = &mut self.calls[index];
        if !id.is_empty() && call.id.is_empty() {
            id.clone_into(&mut call.id);
        }
        if let Some(function) = delta.get("function") {
            if let Some(name) = function.get("name").and_then(Value::as_str)
                && !name.is_empty()
                && call.name != name
            {
                // OpenAI sends the name once; a server that repeats it in
                // every fragment is matched by the equality test.
                call.name.push_str(name);
            }
            match function.get("arguments") {
                Some(Value::String(fragment)) => call.arguments.push_str(fragment),
                None | Some(Value::Null) => {}
                Some(other) => call.arguments.push_str(&other.to_string()),
            }
            if call.arguments.len() > MAX_TOOL_ARGUMENT_BYTES {
                return Err("a tool call's arguments exceed their size cap".to_owned());
            }
        }
        Ok(())
    }

    fn finish(self) -> Result<ChatResponse, String> {
        let mut tool_calls = Vec::with_capacity(self.calls.len());
        for (index, call) in self.calls.into_iter().enumerate() {
            if call.name.is_empty() {
                return Err(format!("streamed tool call {index} has no name"));
            }
            tool_calls.push(ToolCall {
                id: if call.id.is_empty() {
                    format!("call_{index}")
                } else {
                    call.id
                },
                name: call.name,
                arguments: call.arguments,
            });
        }
        Ok(ChatResponse {
            content: self.content,
            tool_calls,
            finish_reason: self.finish_reason,
            usage: self.usage,
        })
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn sampling_follows_the_python_rules() {
        assert!((Sampling::Default.temperature("gpt-4o") - 0.1).abs() < f64::EPSILON);
        assert!(Sampling::Deterministic.temperature("gpt-4o").abs() < f64::EPSILON);
        assert!((Sampling::Deterministic.temperature("o3-mini") - 1.0).abs() < f64::EPSILON);
        assert!((Sampling::Default.temperature("o4") - 1.0).abs() < f64::EPSILON);
    }

    #[test]
    fn the_developer_role_is_for_o_digit_models() {
        assert!(uses_developer_role("o1"));
        assert!(uses_developer_role("o3-mini"));
        assert!(!uses_developer_role("omni"));
        assert!(!uses_developer_role("gpt-4o"));
    }

    #[test]
    fn a_completion_with_tool_calls_parses() {
        let value = json!({
            "choices": [{"finish_reason": "tool_calls", "message": {"content": null, "tool_calls": [
                {"id": "c1", "type": "function", "function": {"name": "get_code", "arguments": "{\"path\":\"a\"}"}},
                {"type": "function", "function": {"name": "think", "arguments": ""}},
            ]}}],
            "usage": {"prompt_tokens": 5, "completion_tokens": 2, "total_tokens": 7},
        });
        let Ok(response) = parse_completion(&value) else {
            panic!("refused");
        };
        assert_eq!(response.tool_calls.len(), 2);
        assert_eq!(response.tool_calls[1].id, "call_1");
        assert_eq!(
            response.tool_calls[1]
                .parsed_arguments()
                .map(|m| m.len())
                .ok(),
            Some(0)
        );
        assert_eq!(response.usage.map(|u| u.total_tokens), Some(7));
        assert_eq!(response.finish_reason.as_deref(), Some("tool_calls"));
    }

    #[test]
    fn deltas_without_an_index_start_a_call_per_id() {
        let mut assembler = Assembler::default();
        for delta in [
            json!({"id": "a", "function": {"name": "f", "arguments": "{}"}}),
            json!({"id": "b", "function": {"name": "g", "arguments": "{\"x\""}}),
            json!({"function": {"arguments": ":1}"}}),
        ] {
            assert!(assembler.tool_delta(&delta).is_ok());
        }
        let Ok(response) = assembler.finish() else {
            panic!("refused");
        };
        assert_eq!(response.tool_calls.len(), 2);
        assert_eq!(response.tool_calls[1].arguments, "{\"x\":1}");
    }
}

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

use std::collections::HashMap;
use std::sync::{Arc, Mutex};

use adk_core::{
    AdkError, Content, FinishReason, Llm, LlmRequest, LlmResponse, LlmResponseStream, Part,
    UsageMetadata,
};
use async_trait::async_trait;
use elitea_agent_runtime::host::{
    BoundModel, HostError, HostErrorCode, ModelRequest, ModelTransport, ReasoningEffort,
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
use super::events::TurnEvents;

const TOOL_CALL_LIMITS: ToolCallLimits = ToolCallLimits {
    max_calls: 128,
    max_argument_bytes: 1024 * 1024,
    max_name_bytes: 256,
    max_id_bytes: 256,
};

/// The models of one local turn.
pub struct GatewayTransport {
    pub http: reqwest::Client,
    pub credentials: Arc<dyn Credentials>,
    /// The turn's project: billing and the execution id's scope.
    pub project_id: i64,
    pub execution_id: String,
    pub events: Arc<TurnEvents>,
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
        let body = request_body(&self.request, &request)?;
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
            let code = refusal_code(status, &body);
            return Err(model_error(code, refusal_message(code)));
        }
        let mut reader = StreamReader::new(&self.events);
        let mut bytes = response.bytes_stream();
        while let Some(chunk) = bytes.next().await {
            let chunk = chunk.map_err(|_| {
                model_error(
                    "model_gateway.incomplete_stream",
                    "the model stream was cut off",
                )
            })?;
            if reader.push(&chunk)? {
                break;
            }
        }
        let (responses, completed) = reader.finish()?;
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

fn refusal_message(code: &str) -> &'static str {
    match code {
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

/// The OpenAI-compatible body: the worker facade's shape.
fn request_body(model: &ModelRequest, request: &LlmRequest) -> adk_core::Result<Value> {
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
    for content in request.contents.iter().filter(|c| c.role != "system") {
        append_message(content, &mut messages).ok_or_else(invalid)?;
    }
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
            json!(match effort {
                ReasoningEffort::Low => "low",
                ReasoningEffort::Medium => "medium",
                ReasoningEffort::High => "high",
                ReasoningEffort::None => "none",
            }),
        );
    }
    Ok(Value::Object(body))
}

fn append_message(content: &Content, messages: &mut Vec<Value>) -> Option<()> {
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
            messages.push(json!({"role": "user", "content": text.join("\n")}));
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
                        "function": {"name": name, "arguments": serde_json::to_string(args).ok()?},
                    })),
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
                    messages.push(json!({
                        "role": "tool",
                        "tool_call_id": id,
                        "content": serde_json::to_string(&function_response.response).ok()?,
                    }));
                }
            }
        }
        _ => return None,
    }
    Some(())
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
    events: &'a TurnEvents,
    splitter: SseSplitter,
    text: String,
    reasoning: String,
    calls: ToolCallAssembler,
    finish: Option<openai::FinishReason>,
    usage: Option<UsageMetadata>,
    done: bool,
}

impl<'a> StreamReader<'a> {
    fn new(events: &'a TurnEvents) -> Self {
        Self {
            events,
            splitter: SseSplitter::new(SseOptions::default()),
            text: String::new(),
            reasoning: String::new(),
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
            self.usage = Some(UsageMetadata {
                prompt_token_count: count("prompt_tokens"),
                candidates_token_count: count("completion_tokens"),
                total_token_count: count("total_tokens"),
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
            if let Ok(Some(reasoning)) = openai::reasoning_text(delta, TypeCheck::Lenient) {
                self.reasoning.push_str(reasoning);
            }
            if let Some(text) = delta.get("content").and_then(Value::as_str) {
                self.text.push_str(text);
                self.events.text_delta(text);
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
        let body = request_body(&model_request(), &request).unwrap();
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

    #[test]
    fn a_stream_of_text_and_a_tool_call_reads_into_one_response() {
        let emitter = Arc::new(VecEmitter::default());
        let events = TurnEvents::new("t".into(), emitter.clone());
        let mut reader = StreamReader::new(&events);
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
        let deltas: Vec<_> = emitter
            .all()
            .into_iter()
            .map(|e| e.payload["text"].as_str().unwrap().to_owned())
            .collect();
        assert_eq!(deltas, ["Let me ", "look."]);
    }

    #[test]
    fn a_stream_without_a_finish_is_incomplete() {
        let emitter = Arc::new(VecEmitter::default());
        let events = TurnEvents::new("t".into(), emitter);
        let mut reader = StreamReader::new(&events);
        reader
            .push(b"data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\n")
            .unwrap();
        assert!(reader.finish().is_err());
    }
}

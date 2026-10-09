//! An adk-rust model over the platform gateway: [`GatewayModel`] implements
//! `adk_core::Llm` with `elitea-model-client`, so an engine's agent runs on
//! adk's `LlmAgent` and `Runner` (the tool loop, tool execution, sessions,
//! callbacks) while every model call still goes through the one client that
//! follows the `/llm` caller contract (`model-client/docs`).
//!
//! adk's own OpenAI-compatible model cannot be used for the gateway: it
//! sends no custom headers (the contract's project and execution headers),
//! and it owns an unrestricted HTTP client. The worker wrote its own model
//! for the same reasons.
//!
//! What the mapping decides, both ways:
//!
//! * **System prompt.** The model's [`Profile::system`] and every `system`
//!   content become ONE leading system message (a gateway model such as
//!   Qwen refuses a later one). An agent should not set adk's
//!   `instruction`: adk sends it as a USER message and expands `{…}` in it
//!   from session state.
//! * **Tools** are offered sorted by name (adk keeps declarations in a hash
//!   map, so there is no other stable order).
//! * **Tool-call arguments** go back as the model's raw text when they did
//!   not parse as an object (adk then hands the tool a string, and the tool
//!   reports it), else re-encoded as Python's `json.dumps` writes them, as
//!   `LangChain` sent them.
//! * **Tool results**: a string result is the tool message's text as it
//!   is; any other value its JSON.
//! * **Delivery** is blocking: one `LlmResponse` per call, whatever adk's
//!   `stream` flag says (the engines' callers never stream an answer).
//! * **Reasoning** is not kept in the conversation.
//! * **Errors** keep the client's message verbatim; a stop is adk's
//!   `Cancelled`.

use adk_core::{
    AdkError, Content, ErrorCategory, ErrorComponent, FinishReason, Llm, LlmRequest, LlmResponse,
    LlmResponseStream, Part, UsageMetadata, async_trait,
};
use elitea_engine_core::errors::EngineError;
use elitea_engine_core::pyjson;
use elitea_engine_core::stream::StopSignal;
use elitea_model_client::chat::{
    ChatClient, ChatMessage, ChatRequest, ChatResponse, Sampling, SystemPrompt, ToolCall,
    ToolDefinition, Usage,
};
use serde_json::{Map, Value, json};
use std::future::Future;
use std::pin::Pin;
use std::sync::Arc;

/// A future the model awaits.
pub type Boxed<T> = Pin<Box<dyn Future<Output = Result<T, EngineError>> + Send>>;

/// One blocking chat completion: [`ChatClient::complete`] in production, a
/// script in tests.
pub type Complete = Arc<dyn Fn(ChatRequest) -> Boxed<ChatResponse> + Send + Sync>;

/// What the caller fixes for every call of one model.
#[derive(Debug, Clone, PartialEq)]
pub struct Profile {
    /// The leading system message.
    pub system: Option<SystemPrompt>,
    pub sampling: Sampling,
    /// Overrides the settings' `max_tokens`.
    pub max_tokens: Option<u32>,
}

impl Default for Profile {
    fn default() -> Self {
        Self {
            system: None,
            sampling: Sampling::Default,
            max_tokens: None,
        }
    }
}

/// The gateway as an adk model.
#[derive(Clone)]
pub struct GatewayModel {
    name: String,
    complete: Complete,
    profile: Profile,
}

impl std::fmt::Debug for GatewayModel {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("GatewayModel")
            .field("name", &self.name)
            .field("profile", &self.profile)
            .finish_non_exhaustive()
    }
}

impl GatewayModel {
    /// A model named `name` whose calls `complete` answers.
    #[must_use]
    pub fn new(name: impl Into<String>, complete: Complete, profile: Profile) -> Self {
        Self {
            name: name.into(),
            complete,
            profile,
        }
    }

    /// The model of `client`'s settings; `stop` aborts a call in flight.
    #[must_use]
    pub fn from_client(client: Arc<ChatClient>, stop: StopSignal, profile: Profile) -> Self {
        let name = client.settings().model_name.clone();
        let complete: Complete = Arc::new(move |request| {
            let (client, stop) = (Arc::clone(&client), stop.clone());
            Box::pin(async move { client.complete(&request, &stop).await })
        });
        Self::new(name, complete, profile)
    }
}

#[async_trait]
impl Llm for GatewayModel {
    fn name(&self) -> &str {
        &self.name
    }

    async fn generate_content(
        &self,
        request: LlmRequest,
        _stream: bool,
    ) -> adk_core::Result<LlmResponseStream> {
        let request = to_chat_request(&request, &self.profile)?;
        let response = (self.complete)(request)
            .await
            .map_err(|error| model_error(&error))?;
        let response = to_llm_response(response);
        Ok(Box::pin(futures_util::stream::once(
            async move { Ok(response) },
        )))
    }
}

fn texts(parts: &[Part]) -> impl Iterator<Item = &str> {
    parts.iter().filter_map(|part| match part {
        Part::Text { text } => Some(text.as_str()),
        _ => None,
    })
}

fn unsupported(what: &str) -> AdkError {
    AdkError::new(
        ErrorComponent::Model,
        ErrorCategory::InvalidInput,
        "model.gateway.unsupported_content",
        format!("the gateway model sends text and tool calls only, not {what}"),
    )
}

/// The arguments a tool call is sent back with: the raw text when they
/// are a string, else Python's `json.dumps` of them.
#[must_use]
pub fn arguments_text(args: &Value) -> String {
    match args {
        Value::String(raw) => raw.clone(),
        other => pyjson::dumps_with(other, None, false),
    }
}

/// A tool result as the tool message's text: a string as it is, else
/// Python's `json.dumps` of it.
#[must_use]
pub fn result_text(response: &Value) -> String {
    match response {
        Value::String(text) => text.clone(),
        other => pyjson::dumps_with(other, None, false),
    }
}

/// adk's request as the gateway client's (see the module comment).
///
/// # Errors
///
/// `InvalidInput` for content the gateway model does not send (images,
/// files) or a role it does not know.
pub fn to_chat_request(request: &LlmRequest, profile: &Profile) -> Result<ChatRequest, AdkError> {
    let mut system: Vec<String> = profile
        .system
        .as_ref()
        .map(|prompt| prompt.parts().to_vec())
        .unwrap_or_default();
    let mut messages = Vec::new();
    for content in &request.contents {
        match content.role.as_str() {
            "system" => system.extend(texts(&content.parts).map(str::to_owned)),
            "user" => {
                if content
                    .parts
                    .iter()
                    .any(|part| !matches!(part, Part::Text { .. } | Part::Thinking { .. }))
                {
                    return Err(unsupported("non-text user content"));
                }
                messages.push(ChatMessage::User(texts(&content.parts).collect()));
            }
            "model" | "assistant" => {
                let text: String = texts(&content.parts).collect();
                let tool_calls = content
                    .parts
                    .iter()
                    .filter_map(|part| match part {
                        Part::FunctionCall { name, args, id, .. } => Some((name, args, id)),
                        _ => None,
                    })
                    .enumerate()
                    .map(|(index, (name, args, id))| ToolCall {
                        id: id.clone().unwrap_or_else(|| format!("call_{index}")),
                        name: name.clone(),
                        arguments: arguments_text(args),
                    })
                    .collect();
                messages.push(ChatMessage::Assistant {
                    content: (!text.is_empty()).then_some(text),
                    tool_calls,
                });
            }
            "function" | "tool" => {
                for part in &content.parts {
                    if let Part::FunctionResponse {
                        function_response,
                        id,
                        ..
                    } = part
                    {
                        messages.push(ChatMessage::Tool {
                            tool_call_id: id.clone().unwrap_or_default(),
                            content: result_text(&function_response.response),
                        });
                    }
                }
            }
            other => return Err(unsupported(&format!("a '{other}' message"))),
        }
    }
    if !system.is_empty() {
        messages.insert(0, ChatMessage::SystemPrompt(SystemPrompt::blocks(system)));
    }
    let mut tools: Vec<ToolDefinition> = request
        .tools
        .iter()
        .map(|(name, declaration)| ToolDefinition {
            name: name.clone(),
            description: declaration
                .get("description")
                .and_then(Value::as_str)
                .unwrap_or_default()
                .to_owned(),
            parameters: declaration
                .get("parameters")
                .cloned()
                .unwrap_or_else(|| json!({"type": "object", "properties": {}})),
        })
        .collect();
    tools.sort_by(|a, b| a.name.cmp(&b.name));
    Ok(ChatRequest {
        messages,
        tools,
        tool_choice: None,
        sampling: profile.sampling,
        max_tokens: profile.max_tokens,
    })
}

/// A tool call's arguments as adk hands them to the tool: the object, `{}`
/// for none, and the raw text when the model wrote something else.
fn call_arguments(call: &ToolCall) -> Value {
    if call.arguments.trim().is_empty() {
        return Value::Object(Map::new());
    }
    match serde_json::from_str::<Value>(&call.arguments) {
        Ok(object @ Value::Object(_)) => object,
        _ => Value::String(call.arguments.clone()),
    }
}

fn count(tokens: u64) -> i32 {
    i32::try_from(tokens).unwrap_or(i32::MAX)
}

/// The gateway's usage in adk's terms.
#[must_use]
pub fn usage_metadata(usage: &Usage) -> UsageMetadata {
    UsageMetadata {
        prompt_token_count: count(usage.prompt_tokens),
        candidates_token_count: count(usage.completion_tokens),
        total_token_count: count(usage.total_tokens),
        thinking_token_count: (usage.reasoning_tokens > 0).then(|| count(usage.reasoning_tokens)),
        cache_read_input_token_count: (usage.cached_tokens > 0).then(|| count(usage.cached_tokens)),
        ..UsageMetadata::default()
    }
}

fn finish_reason(reason: Option<&str>) -> Option<FinishReason> {
    reason.map(|reason| match reason {
        "stop" | "tool_calls" | "function_call" => FinishReason::Stop,
        "length" => FinishReason::MaxTokens,
        "content_filter" => FinishReason::Safety,
        _ => FinishReason::Other,
    })
}

/// The gateway's answer as adk's: one complete response, its text first and
/// then its tool calls; the turn is complete when it called no tool.
#[must_use]
pub fn to_llm_response(response: ChatResponse) -> LlmResponse {
    let mut parts = Vec::new();
    if !response.content.is_empty() {
        parts.push(Part::Text {
            text: response.content,
        });
    }
    let turn_complete = response.tool_calls.is_empty();
    for call in &response.tool_calls {
        parts.push(Part::FunctionCall {
            name: call.name.clone(),
            args: call_arguments(call),
            id: Some(call.id.clone()),
            thought_signature: None,
        });
    }
    LlmResponse {
        content: Some(Content {
            role: "model".to_owned(),
            parts,
        }),
        usage_metadata: response.usage.as_ref().map(usage_metadata),
        finish_reason: finish_reason(response.finish_reason.as_deref()),
        partial: false,
        turn_complete,
        ..LlmResponse::default()
    }
}

/// The client's failure as adk's, its message kept verbatim.
#[must_use]
pub fn model_error(error: &EngineError) -> AdkError {
    if *error == EngineError::cancelled() {
        return AdkError::new(
            ErrorComponent::Model,
            ErrorCategory::Cancelled,
            "model.gateway.cancelled",
            error.message.clone(),
        );
    }
    AdkError::new(
        ErrorComponent::Model,
        ErrorCategory::Unavailable,
        "model.gateway.failed",
        error.message.clone(),
    )
}

#[cfg(test)]
mod tests {
    use super::*;
    use adk_core::FunctionResponseData;
    use elitea_engine_core::errors::ErrorType;
    use futures_util::StreamExt;
    use std::sync::Mutex;

    fn text(role: &str, text: &str) -> Content {
        Content {
            role: role.to_owned(),
            parts: vec![Part::Text {
                text: text.to_owned(),
            }],
        }
    }

    fn conversation() -> LlmRequest {
        let mut request = LlmRequest::new(
            "m",
            vec![
                text("system", "Also this."),
                text("user", "Where are refunds?"),
                Content {
                    role: "model".to_owned(),
                    parts: vec![
                        Part::Text {
                            text: "Searching.".to_owned(),
                        },
                        Part::FunctionCall {
                            name: "search".to_owned(),
                            args: json!({"query": "refund", "é": 1}),
                            id: Some("c1".to_owned()),
                            thought_signature: None,
                        },
                        Part::FunctionCall {
                            name: "read".to_owned(),
                            args: Value::String("{not json".to_owned()),
                            id: None,
                            thought_signature: None,
                        },
                    ],
                },
                Content {
                    role: "function".to_owned(),
                    parts: vec![Part::FunctionResponse {
                        function_response: FunctionResponseData::new(
                            "search",
                            json!("Found `RefundService`."),
                        ),
                        id: Some("c1".to_owned()),
                        annotations: None,
                    }],
                },
            ],
        );
        request.tools.insert(
            "zeta".to_owned(),
            json!({"name": "zeta", "description": "Z.", "parameters": {"type": "object"}}),
        );
        request.tools.insert(
            "alpha".to_owned(),
            json!({"name": "alpha", "description": "A."}),
        );
        request
    }

    #[test]
    fn a_conversation_maps_to_the_gateway_request() {
        let profile = Profile {
            system: Some(SystemPrompt::text("You investigate.".to_owned())),
            sampling: Sampling::Default,
            max_tokens: Some(4096),
        };
        let Ok(chat) = to_chat_request(&conversation(), &profile) else {
            panic!("maps");
        };
        assert_eq!(
            chat.messages,
            [
                ChatMessage::SystemPrompt(SystemPrompt::blocks(vec![
                    "You investigate.".to_owned(),
                    "Also this.".to_owned()
                ])),
                ChatMessage::User("Where are refunds?".to_owned()),
                ChatMessage::Assistant {
                    content: Some("Searching.".to_owned()),
                    tool_calls: vec![
                        ToolCall {
                            id: "c1".to_owned(),
                            name: "search".to_owned(),
                            arguments: r#"{"query": "refund", "é": 1}"#.to_owned(),
                        },
                        ToolCall {
                            id: "call_1".to_owned(),
                            name: "read".to_owned(),
                            arguments: "{not json".to_owned(),
                        },
                    ],
                },
                ChatMessage::Tool {
                    tool_call_id: "c1".to_owned(),
                    content: "Found `RefundService`.".to_owned(),
                },
            ]
        );
        let names: Vec<&str> = chat.tools.iter().map(|t| t.name.as_str()).collect();
        assert_eq!(names, ["alpha", "zeta"]);
        assert_eq!(
            chat.tools[0].parameters,
            json!({"type": "object", "properties": {}})
        );
        assert_eq!(chat.max_tokens, Some(4096));
    }

    #[test]
    fn images_are_refused() {
        let request = LlmRequest::new(
            "m",
            vec![Content {
                role: "user".to_owned(),
                parts: vec![Part::InlineData {
                    mime_type: "image/png".to_owned(),
                    data: vec![1],
                    uri: None,
                    annotations: None,
                }],
            }],
        );
        let refused = to_chat_request(&request, &Profile::default());
        assert!(refused.is_err_and(|e| e.category == ErrorCategory::InvalidInput));
    }

    #[test]
    fn an_answer_maps_to_one_complete_response() {
        let response = to_llm_response(ChatResponse {
            content: "Let me look.".to_owned(),
            reasoning: "hidden".to_owned(),
            tool_calls: vec![
                ToolCall {
                    id: "a".to_owned(),
                    name: "search".to_owned(),
                    arguments: r#"{"query": "x"}"#.to_owned(),
                },
                ToolCall {
                    id: "b".to_owned(),
                    name: "list".to_owned(),
                    arguments: String::new(),
                },
                ToolCall {
                    id: "c".to_owned(),
                    name: "read".to_owned(),
                    arguments: "[1]".to_owned(),
                },
            ],
            finish_reason: Some("tool_calls".to_owned()),
            usage: Some(Usage {
                prompt_tokens: 10,
                completion_tokens: 5,
                total_tokens: 15,
                reasoning_tokens: 2,
                cached_tokens: 0,
            }),
        });
        assert!(!response.turn_complete && !response.partial);
        let parts = response.content.map(|c| c.parts).unwrap_or_default();
        assert_eq!(parts.len(), 4, "text and three calls, no reasoning");
        assert_eq!(
            parts[0],
            Part::Text {
                text: "Let me look.".to_owned()
            }
        );
        let args: Vec<Value> = parts
            .iter()
            .filter_map(|p| match p {
                Part::FunctionCall { args, .. } => Some(args.clone()),
                _ => None,
            })
            .collect();
        assert_eq!(args, [json!({"query": "x"}), json!({}), json!("[1]")]);
        let usage = response.usage_metadata.unwrap_or_default();
        assert_eq!(
            (
                usage.prompt_token_count,
                usage.candidates_token_count,
                usage.total_token_count,
                usage.thinking_token_count,
                usage.cache_read_input_token_count
            ),
            (10, 5, 15, Some(2), None)
        );
        assert_eq!(response.finish_reason, Some(FinishReason::Stop));
    }

    #[tokio::test]
    async fn the_model_answers_through_the_client_once() {
        let seen: Arc<Mutex<Vec<ChatRequest>>> = Arc::default();
        let log = Arc::clone(&seen);
        let complete: Complete = Arc::new(move |request| {
            log.lock().map(|mut l| l.push(request)).ok();
            Box::pin(async {
                Ok(ChatResponse {
                    content: "Done.".to_owned(),
                    ..ChatResponse::default()
                })
            })
        });
        let model = GatewayModel::new("qwen", complete, Profile::default());
        assert_eq!(model.name(), "qwen");
        let Ok(mut stream) = model
            .generate_content(LlmRequest::new("qwen", vec![text("user", "hi")]), true)
            .await
        else {
            panic!("a stream");
        };
        let mut responses = Vec::new();
        while let Some(item) = stream.next().await {
            responses.push(item);
        }
        assert_eq!(responses.len(), 1, "blocking delivery: one response");
        assert!(responses[0].as_ref().is_ok_and(|r| r.turn_complete));
        assert_eq!(seen.lock().map(|l| l.len()).unwrap_or_default(), 1);
    }

    #[tokio::test]
    async fn failures_keep_their_message_and_a_stop_is_cancelled() {
        for (error, category) in [
            (
                EngineError::new(ErrorType::Runtime, "the gateway refused: 429"),
                ErrorCategory::Unavailable,
            ),
            (EngineError::cancelled(), ErrorCategory::Cancelled),
        ] {
            let message = error.message.clone();
            let complete: Complete = Arc::new(move |_| {
                let error = error.clone();
                Box::pin(async move { Err(error) })
            });
            let model = GatewayModel::new("m", complete, Profile::default());
            let outcome = model
                .generate_content(LlmRequest::new("m", vec![text("user", "hi")]), false)
                .await;
            let Err(failure) = outcome else {
                panic!("fails");
            };
            assert_eq!((failure.category, failure.message), (category, message));
        }
    }
}

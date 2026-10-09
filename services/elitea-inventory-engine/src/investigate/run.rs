//! The investigation on adk-rust: an `LlmAgent` with the offered tools,
//! run by adk's `Runner` over an in-memory session, the model the gateway
//! (`elitea-adk-gateway`). adk owns the loop: calling the model, running the
//! tools one at a time in the order the model called them, feeding the
//! results back.
//!
//! What this module adds around it, so the investigation's contract holds:
//!
//! * **At most 50 model calls.** A before-model callback counts them and
//!   answers the 51st with the SDK's limit message instead of calling the
//!   model (the 50th round's tools still ran), as the Python agent stopped.
//! * **Summarisation between rounds.** adk compacts a session only before a
//!   run starts, so the same callback keeps a summarised mirror of the
//!   conversation (`elitea-conversation`, `LangChain`'s thresholds) and
//!   sends that instead of adk's growing history.
//! * **Tool results are text.** Every tool answers a string — its result or
//!   an `Error: …` line, as the Python tools did — and fails only on a stop,
//!   so adk never turns an error into a JSON object for the model.
//! * **The record**: tool calls with their inputs and output previews,
//!   tokens of the model's calls (not of the summaries), the answer and its
//!   citations, read from adk's events.
//! * **Stop and failure**: a stop cancels the run and the call in flight;
//!   a failed model call ends the investigation with its `error` set and
//!   what was recorded so far.

use super::{
    ChatModel, Embed, Investigation, LIMIT_MESSAGE, Offered, Question, SourceCall, SourceToolkit,
    citations_in, graph_call, investigate_asset, local_call, offered_tools, record,
};
use crate::extract::assets::render;
use crate::retrieval::view::GraphView;
use adk_agent::LlmAgentBuilder;
use adk_core::{
    AdkError, BeforeModelResult, CallbackContext, Content, ErrorCategory, ErrorComponent,
    FunctionResponseData, LlmRequest, LlmResponse, Part, RunConfig, SessionId, Tool, ToolContext,
    ToolExecutionStrategy, UserId, async_trait,
};
use adk_runner::Runner;
use adk_session::{CreateRequest, InMemorySessionService, SessionService};
use elitea_adk_gateway::{GatewayModel, Profile, arguments_text, model_error, result_text};
use elitea_conversation::{Call, DEFAULT_SUMMARY_PROMPT, Msg, compact};
use elitea_engine_core::errors::{EngineError, ErrorType};
use elitea_engine_core::stream::StopSignal;
use elitea_model_client::chat::{Sampling, SystemPrompt};
use futures_util::StreamExt;
use serde_json::{Map, Value};
use std::collections::{HashMap, VecDeque};
use std::sync::Arc;
use std::sync::atomic::{AtomicBool, AtomicUsize, Ordering};
use std::time::Duration;
use tokio_util::sync::CancellationToken;

/// The app, user and agent names of the in-memory session.
const APP: &str = "inventory";
const USER: &str = "investigate";

/// A source toolkit call may take long (it reads a remote), but not
/// forever.
const TOOL_TIMEOUT: Duration = Duration::from_mins(10);

/// What every tool reads.
struct Ground {
    view: Arc<GraphView>,
    call_source: SourceCall,
    embed: Option<Embed>,
}

/// One offered tool.
struct InvestigateTool {
    name: String,
    description: String,
    parameters: Value,
    route: Offered,
    ground: Arc<Ground>,
}

fn cancelled() -> AdkError {
    AdkError::new(
        ErrorComponent::Tool,
        ErrorCategory::Cancelled,
        "tool.investigate.cancelled",
        EngineError::cancelled().message,
    )
}

#[async_trait]
impl Tool for InvestigateTool {
    fn name(&self) -> &str {
        &self.name
    }

    fn description(&self) -> &str {
        &self.description
    }

    fn parameters_schema(&self) -> Option<Value> {
        Some(self.parameters.clone())
    }

    async fn execute(
        &self,
        _context: Arc<dyn ToolContext>,
        args: Value,
    ) -> adk_core::Result<Value> {
        let Value::Object(arguments) = args else {
            return Ok(Value::String(format!(
                "Error: the arguments of tool call '{}' are not a JSON object",
                self.name
            )));
        };
        let ground = &self.ground;
        let answered = match &self.route {
            Offered::Graph(tool, family) => Ok(graph_call(&ground.view, tool, family, &arguments)),
            Offered::Local(local) => {
                local_call(&ground.view, *local, &arguments, ground.embed.as_ref()).await
            }
            Offered::Source { toolkit_id, tool } => {
                (ground.call_source)(toolkit_id.clone(), tool.clone(), arguments).await
            }
        };
        match answered {
            Ok(text) => Ok(Value::String(text)),
            Err(error) if error == EngineError::cancelled() => Err(cancelled()),
            Err(error) => Ok(Value::String(format!("Error: {}", error.message))),
        }
    }
}

/// The conversation the model is sent: adk's history, summarised.
#[derive(Default)]
struct Mirror {
    messages: Vec<Msg>,
    /// adk contents already taken in.
    seen: usize,
    /// The `total_tokens` of each model answer, in order, for the
    /// summarisation policy's usage scaling.
    totals: VecDeque<Option<u64>>,
}

fn texts(parts: &[Part]) -> String {
    parts
        .iter()
        .filter_map(|part| match part {
            Part::Text { text } => Some(text.as_str()),
            _ => None,
        })
        .collect()
}

impl Mirror {
    /// Take in adk's contents past what was seen.
    fn take_in(&mut self, contents: &[Content]) {
        for content in contents.iter().skip(self.seen) {
            match content.role.as_str() {
                "user" => self.messages.push(Msg::User(texts(&content.parts))),
                "model" => {
                    let calls = content
                        .parts
                        .iter()
                        .filter_map(|part| match part {
                            Part::FunctionCall { name, args, id, .. } => Some(Call {
                                id: id.clone().unwrap_or_default(),
                                name: name.clone(),
                                raw: arguments_text(args),
                                args: args.as_object().cloned(),
                            }),
                            _ => None,
                        })
                        .collect();
                    self.messages.push(Msg::Ai {
                        content: texts(&content.parts),
                        calls,
                        total_tokens: self.totals.pop_front().flatten(),
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
                            self.messages.push(Msg::Tool {
                                call_id: id.clone().unwrap_or_default(),
                                name: function_response.name.clone(),
                                content: result_text(&function_response.response),
                            });
                        }
                    }
                }
                _ => {}
            }
        }
        self.seen = contents.len();
    }

    /// The mirror as adk contents.
    fn contents(&self) -> Vec<Content> {
        self.messages
            .iter()
            .map(|message| match message {
                Msg::User(text) => Content::new("user").with_text(text.clone()),
                Msg::Ai { content, calls, .. } => {
                    let mut parts = Vec::new();
                    if !content.is_empty() {
                        parts.push(Part::Text {
                            text: content.clone(),
                        });
                    }
                    parts.extend(calls.iter().map(|call| {
                        Part::FunctionCall {
                            name: call.name.clone(),
                            args: call
                                .args
                                .clone()
                                .map_or_else(|| Value::String(call.raw.clone()), Value::Object),
                            id: Some(call.id.clone()),
                            thought_signature: None,
                        }
                    }));
                    Content {
                        role: "model".to_owned(),
                        parts,
                    }
                }
                Msg::Tool {
                    call_id,
                    name,
                    content,
                } => Content {
                    role: "function".to_owned(),
                    parts: vec![Part::FunctionResponse {
                        function_response: FunctionResponseData::new(
                            name.clone(),
                            Value::String(content.clone()),
                        ),
                        id: Some(call_id.clone()),
                        annotations: None,
                    }],
                },
            })
            .collect()
    }
}

/// Counts the model's calls, stops at the limit, summarises.
fn before_model(
    model: &ChatModel,
    max_rounds: usize,
    rounds: Arc<AtomicUsize>,
    limited: Arc<AtomicBool>,
    mirror: Arc<tokio::sync::Mutex<Mirror>>,
) -> adk_core::BeforeModelCallback {
    let model = model.clone();
    Box::new(
        move |_context: Arc<dyn CallbackContext>, mut request: LlmRequest| {
            let (model, rounds, limited, mirror) = (
                model.clone(),
                Arc::clone(&rounds),
                Arc::clone(&limited),
                Arc::clone(&mirror),
            );
            Box::pin(async move {
                if rounds.fetch_add(1, Ordering::SeqCst) >= max_rounds {
                    limited.store(true, Ordering::SeqCst);
                    return Ok(BeforeModelResult::Skip(LlmResponse::new(
                        Content::new("model").with_text(LIMIT_MESSAGE),
                    )));
                }
                let mut mirror = mirror.lock().await;
                mirror.take_in(&request.contents);
                let messages = std::mem::take(&mut mirror.messages);
                let chat = Arc::clone(&model.chat);
                mirror.messages = compact(
                    &model.policy,
                    DEFAULT_SUMMARY_PROMPT,
                    Some(4096),
                    messages,
                    |summary| chat(summary),
                )
                .await
                .map_err(|error| model_error(&error))?;
                request.contents = mirror.contents();
                Ok(BeforeModelResult::Continue(request))
            })
        },
    )
}

/// Notes each answer's `total_tokens` for the mirror.
fn after_model(mirror: Arc<tokio::sync::Mutex<Mirror>>) -> adk_core::AfterModelCallback {
    Box::new(
        move |_context: Arc<dyn CallbackContext>, response: LlmResponse| {
            let mirror = Arc::clone(&mirror);
            Box::pin(async move {
                if !response.partial {
                    let total = response
                        .usage_metadata
                        .as_ref()
                        .and_then(|usage| u64::try_from(usage.total_token_count).ok());
                    mirror.lock().await.totals.push_back(total);
                }
                Ok(None)
            })
        },
    )
}

fn engine_error(error: &AdkError) -> EngineError {
    if error.category == ErrorCategory::Cancelled {
        EngineError::cancelled()
    } else {
        EngineError::new(ErrorType::Runtime, error.message.clone())
    }
}

/// The tools the model is offered, each with its route.
fn tools(ground: &Arc<Ground>, sources: &[SourceToolkit]) -> Vec<Arc<dyn Tool>> {
    let (definitions, routes) = offered_tools(&ground.view, ground.embed.is_some(), sources);
    definitions
        .into_iter()
        .zip(routes)
        .map(|(definition, (_, route))| {
            Arc::new(InvestigateTool {
                name: definition.name,
                description: definition.description,
                parameters: definition.parameters,
                route,
                ground: Arc::clone(ground),
            }) as Arc<dyn Tool>
        })
        .collect()
}

/// What the run's events say.
#[derive(Default)]
struct Reading {
    /// Calls by id, waiting for their result: name and arguments.
    pending: HashMap<String, (String, Option<Map<String, Value>>)>,
    answer: Option<String>,
}

impl Reading {
    fn read(&mut self, event: &adk_core::Event, result: &mut Investigation) {
        let response = &event.llm_response;
        if response.partial {
            return;
        }
        let parts = response
            .content
            .as_ref()
            .map(|content| content.parts.as_slice())
            .unwrap_or_default();
        for part in parts {
            match part {
                Part::FunctionCall { name, args, id, .. } => {
                    self.pending.insert(
                        id.clone().unwrap_or_default(),
                        (name.clone(), args.as_object().cloned()),
                    );
                }
                Part::FunctionResponse {
                    function_response,
                    id,
                    ..
                } => {
                    let (name, arguments) = self
                        .pending
                        .remove(id.as_deref().unwrap_or_default())
                        .unwrap_or_else(|| (function_response.name.clone(), None));
                    record(
                        result,
                        &name,
                        arguments.as_ref(),
                        &result_text(&function_response.response),
                    );
                }
                _ => {}
            }
        }
        if let Some(usage) = &response.usage_metadata {
            result.tokens_in += u64::try_from(usage.prompt_token_count).unwrap_or_default();
            result.tokens_out += u64::try_from(usage.candidates_token_count).unwrap_or_default();
        }
        if event.is_final_response() && event.author != "user" {
            self.answer = Some(texts(parts));
        }
    }
}

/// Run one investigation.
///
/// # Errors
///
/// Only a stop; a failed model call ends the investigation with its
/// `error` set, as Python reported it.
pub async fn investigate(
    question: &Question,
    view: Arc<GraphView>,
    model: &ChatModel,
    sources: &[SourceToolkit],
    call_source: &SourceCall,
    embed: Option<&Embed>,
    stop: &StopSignal,
) -> Result<Investigation, EngineError> {
    let template = investigate_asset()["system_prompt"]
        .as_str()
        .unwrap_or_default();
    let system = render(template, &[("filters", &question.filter_text())]);
    let max_rounds = investigate_asset()["max_iterations"]
        .as_u64()
        .and_then(|n| usize::try_from(n).ok())
        .unwrap_or(50);
    let ground = Arc::new(Ground {
        view,
        call_source: Arc::clone(call_source),
        embed: embed.cloned(),
    });
    let gateway = GatewayModel::new(
        model.name.clone(),
        Arc::clone(&model.chat),
        Profile {
            system: Some(SystemPrompt::text(system)),
            sampling: Sampling::Default,
            max_tokens: Some(4096),
        },
    );
    let rounds = Arc::new(AtomicUsize::new(0));
    let limited = Arc::new(AtomicBool::new(false));
    let mirror = Arc::new(tokio::sync::Mutex::new(Mirror::default()));
    let mut builder = LlmAgentBuilder::new(USER)
        .model(Arc::new(gateway))
        .tool_execution_strategy(ToolExecutionStrategy::Sequential)
        .tool_timeout(TOOL_TIMEOUT)
        .max_iterations(u32::try_from(max_rounds + 2).unwrap_or(u32::MAX))
        .before_model_callback(before_model(
            model,
            max_rounds,
            Arc::clone(&rounds),
            Arc::clone(&limited),
            Arc::clone(&mirror),
        ))
        .after_model_callback(after_model(Arc::clone(&mirror)));
    for tool in tools(&ground, sources) {
        builder = builder.tool(tool);
    }
    let failed = |error: &AdkError| EngineError::new(ErrorType::Runtime, error.message.clone());
    let agent = builder.build().map_err(|e| failed(&e))?;
    let sessions = Arc::new(InMemorySessionService::new());
    let session = sessions
        .create(CreateRequest {
            app_name: APP.to_owned(),
            user_id: USER.to_owned(),
            session_id: None,
            state: HashMap::new(),
        })
        .await
        .map_err(|e| failed(&e))?;
    let token = CancellationToken::new();
    // No request or tool payloads in the run's spans: they carry the
    // question and the graph's text.
    let run_config = RunConfig {
        trace_payload_max_bytes: 0,
        ..RunConfig::default()
    };
    let runner = Runner::builder()
        .app_name(APP)
        .agent(Arc::new(agent))
        .session_service(sessions)
        .cancellation_token(token.clone())
        .run_config(run_config)
        .build()
        .map_err(|e| failed(&e))?;
    let watcher = {
        let (stop, token) = (stop.clone(), token.clone());
        tokio::spawn(async move {
            stop.stopped().await;
            token.cancel();
        })
    };
    let outcome = drive(&runner, session.id(), question, stop).await;
    watcher.abort();
    let (mut result, answer) = outcome?;
    if limited.load(Ordering::SeqCst) {
        LIMIT_MESSAGE.clone_into(&mut result.answer);
    } else if let Some(answer) = answer {
        result.citations = citations_in(&answer);
        result.answer = answer;
    }
    Ok(result)
}

/// Run the agent and read its events: the record and the final answer.
async fn drive(
    runner: &Runner,
    session_id: &str,
    question: &Question,
    stop: &StopSignal,
) -> Result<(Investigation, Option<String>), EngineError> {
    let mut result = Investigation::default();
    let mut reading = Reading::default();
    let started = runner
        .run(
            UserId::new_unchecked(USER),
            SessionId::new_unchecked(session_id),
            Content::new("user").with_text(question.text.clone()),
        )
        .await;
    let mut events = match started {
        Ok(events) => events,
        Err(error) => {
            result.error = Some(error.message);
            return Ok((result, None));
        }
    };
    while let Some(item) = events.next().await {
        match item {
            Ok(event) => reading.read(&event, &mut result),
            Err(error) => {
                if stop.is_requested() || error.category == ErrorCategory::Cancelled {
                    return Err(EngineError::cancelled());
                }
                result.error = Some(engine_error(&error).message);
                return Ok((result, None));
            }
        }
    }
    // adk ends a cancelled run without an error.
    if stop.is_requested() {
        return Err(EngineError::cancelled());
    }
    Ok((result, reading.answer))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_mirror_round_trips_adk_contents() {
        let contents = vec![
            Content::new("user").with_text("q"),
            Content {
                role: "model".to_owned(),
                parts: vec![
                    Part::Text {
                        text: "looking".to_owned(),
                    },
                    Part::FunctionCall {
                        name: "search".to_owned(),
                        args: serde_json::json!({"query": "x"}),
                        id: Some("c1".to_owned()),
                        thought_signature: None,
                    },
                ],
            },
            Content {
                role: "function".to_owned(),
                parts: vec![Part::FunctionResponse {
                    function_response: FunctionResponseData::new("search", Value::from("found")),
                    id: Some("c1".to_owned()),
                    annotations: None,
                }],
            },
        ];
        let mut mirror = Mirror::default();
        mirror.totals.push_back(Some(15));
        mirror.take_in(&contents);
        assert_eq!(mirror.seen, 3);
        assert!(matches!(
            &mirror.messages[1],
            Msg::Ai { total_tokens: Some(15), calls, .. } if calls[0].raw == r#"{"query": "x"}"#
        ));
        let back = mirror.contents();
        assert_eq!(back.len(), 3);
        assert_eq!(back[1].parts, contents[1].parts);
        assert_eq!(back[2].parts, contents[2].parts);
        // Taking in the same contents again adds nothing.
        mirror.take_in(&contents);
        assert_eq!(mirror.messages.len(), 3);
    }
}

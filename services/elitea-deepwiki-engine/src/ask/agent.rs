//! The tool-calling loop of `ask` and deep research (`langchain`
//! `create_agent` / `deepagents.create_deep_agent` as the Python engines
//! built them), and the events their workers printed.
//!
//! One step: summarise when due, call the model, and when it called tools
//! announce each call, run them in call order, and append the results. A
//! message without tool calls ends the loop. The Python agents ran the
//! calls of one step in parallel threads; the order of the results in the
//! next request was the call order there too, and only the order of the
//! progress events differed between runs.
//!
//! Step limits (ADR-0026 decision 8, explicit settings; Python enforced
//! none):
//!
//! * `ask`: at most `DEEPWIKI_ASK_MAX_ITERATIONS` (default 8) tool calls,
//!   the budget its system prompt states. A call past it is answered with
//!   [`ASK_BUDGET_SPENT`] and the next request sets `tool_choice: none`.
//! * deep research: at most `ELITEA_DEEPWIKI_RESEARCH_MAX_ITERATIONS`
//!   (default 15) tool-calling steps, then `tool_choice: none`; at most
//!   [`MAX_CALLS_PER_STEP`] calls in one step.
//!
//! A model that still calls tools after `tool_choice: none` ends the loop
//! with the answer written so far.
//!
//! Deliberate differences, besides the limits: arguments that are not a
//! JSON object get an error result and the loop goes on (`LangChain` left
//! the call out of `tool_calls`, so the turn looked final and the answer
//! was empty); a repeated tool-call id is announced again (`ask_engine`
//! suppressed it, meant for streamed chunks of one call).

use super::args::{self, Args};
use super::embed::Embedder;
use super::pyfmt;
use super::store::IndexStore;
use super::summarize::{self, Call, Msg, Policy};
use super::tools::Codebase;
use super::vfs::{self, Vfs};
use crate::errors::{EngineError, ErrorType};
use crate::llm::{
    ChatClient, ChatRequest, ChatResponse, Sampling, SystemPrompt, ToolChoice, ToolDefinition,
};
use crate::pyjson;
use crate::runner::{Context, StopSignal};
use serde_json::{Value, json};
use std::future::Future;

/// The answer to a call past the `ask` budget.
pub const ASK_BUDGET_SPENT: &str =
    "Error: the tool-call budget is spent. Answer now with what you have found.";
/// Calls one deep-research step may make.
pub const MAX_CALLS_PER_STEP: usize = 25;

/// The model an agent calls. [`ChatClient`] is the live one; the parity
/// gate replays a recording through the same request builder.
pub trait Model: Send + Sync {
    /// The model name (for the summarisation profile).
    fn model_name(&self) -> &str;
    /// Whether the provider is Anthropic (its token estimate differs).
    fn anthropic(&self) -> bool;
    /// One completion, streamed when `stream` (`on_text` sees each answer
    /// fragment as it arrives).
    fn call(
        &self,
        request: &ChatRequest,
        stream: bool,
        stop: &StopSignal,
        on_text: &mut (dyn FnMut(&str) + Send),
    ) -> impl Future<Output = Result<ChatResponse, EngineError>> + Send;
}

impl Model for ChatClient {
    fn model_name(&self) -> &str {
        &self.settings().model_name
    }

    fn anthropic(&self) -> bool {
        self.settings().provider == crate::llm::Provider::Anthropic
    }

    async fn call(
        &self,
        request: &ChatRequest,
        stream: bool,
        stop: &StopSignal,
        on_text: &mut (dyn FnMut(&str) + Send),
    ) -> Result<ChatResponse, EngineError> {
        if stream {
            self.stream(request, stop, on_text).await
        } else {
            self.complete(request, stop).await
        }
    }
}

/// Which agent.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Mode {
    Ask,
    Research,
}

/// The time source of the prompts and events.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Clock {
    /// UTC now (Python used local time).
    System,
    /// Fixed values, for tests.
    Fixed { date: String, timestamp: String },
}

impl Clock {
    fn instant() -> crate::wiki::compose::Instant {
        crate::wiki::compose::Instant::utc(std::time::SystemTime::now())
    }

    /// `%Y-%m-%d`.
    #[must_use]
    pub fn date(&self) -> String {
        match self {
            Self::System => {
                let now = Self::instant();
                format!("{:04}-{:02}-{:02}", now.year, now.month, now.day)
            }
            Self::Fixed { date, .. } => date.clone(),
        }
    }

    /// `datetime.now().isoformat()`.
    #[must_use]
    pub fn timestamp(&self) -> String {
        match self {
            Self::System => Self::instant().iso(),
            Self::Fixed { timestamp, .. } => timestamp.clone(),
        }
    }
}

/// One agent run's setup.
#[derive(Debug, Clone)]
pub struct AgentSpec {
    pub mode: Mode,
    pub system: SystemPrompt,
    pub user: String,
    /// Offered to the model, in order.
    pub tools: Vec<ToolDefinition>,
    /// Every tool the agent can run (the "not a valid tool" list).
    pub known_tools: Vec<&'static str>,
    pub streaming: bool,
    pub max_tokens: u32,
    /// `ask`: tool calls; deep research: tool-calling steps.
    pub budget: usize,
    /// `search_codebase`'s documentation results at most.
    pub doc_results: usize,
    pub policy: Policy,
    pub summary_prompt: &'static str,
    pub clock: Clock,
}

/// What a run produced.
#[derive(Debug, Clone, PartialEq)]
pub struct Outcome {
    /// `ask`: the joined answer fragments; deep research: the last answer.
    pub answer: String,
    pub todos: Vec<Value>,
    pub thinking_steps: Vec<Value>,
    /// Deep research's `total_events` (its start, steps, todo updates and
    /// completion).
    pub total_events: usize,
    /// The messages of the final conversation (for tests).
    pub messages: Vec<Msg>,
}

/// The progress lines of one run, as the worker printed them and
/// `tool_operations` forwarded them.
pub struct Events<'c> {
    context: &'c Context,
    mode: Mode,
    clock: Clock,
    step: usize,
    pub thinking_steps: Vec<Value>,
    pub total_events: usize,
}

impl<'c> Events<'c> {
    #[must_use]
    pub fn new(context: &'c Context, mode: Mode, clock: Clock) -> Self {
        Self {
            context,
            mode,
            clock,
            step: 0,
            thinking_steps: Vec::new(),
            total_events: 0,
        }
    }

    /// `_emit_thinking_step(type, title, content, metadata)`.
    pub fn emit_step(&self, kind: &str, title: &str, content: &str, metadata: &Value) {
        let content = pyfmt::head(content, 500);
        match self.mode {
            Mode::Ask => {
                let line = json!({
                    "type": kind, "title": title, "content": content, "metadata": metadata,
                });
                self.context.thinking(pyjson::dumps(&line));
            }
            Mode::Research => {
                let display = match kind {
                    "tool_call" => format!("\u{1f527} {title}"),
                    "tool_result" => {
                        let tool = metadata
                            .get("tool")
                            .and_then(Value::as_str)
                            .unwrap_or("tool");
                        format!("\u{2713} {tool}: {}", pyfmt::preview(content, 200))
                    }
                    _ if !content.is_empty() && content != title => format!("{title}\n{content}"),
                    _ => title.to_owned(),
                };
                self.context.thinking(display);
            }
        }
    }

    /// `_emit_event(type, data)` (`ask` only).
    fn ask_event(&self, event: &str, data: &Value) {
        let line = json!({"event": event, "data": data, "timestamp": self.clock.timestamp()});
        self.context.thinking(pyjson::dumps(&line));
    }

    fn tool_call(&mut self, call: &Call) {
        self.step += 1;
        let id = if call.id.is_empty() {
            format!("call_{}", self.step)
        } else {
            call.id.clone()
        };
        let input = match &call.args {
            Some(args) => pyfmt::repr(&Value::Object(args.clone())),
            None => call.raw.clone(),
        };
        let input = pyfmt::head(&input, 500).to_owned();
        self.total_events += 1;
        self.thinking_steps.push(json!({
            "step": self.step, "type": "tool_call", "tool": call.name, "tool_call_id": id,
            "call_id": id, "input": input, "timestamp": self.clock.timestamp(),
        }));
        let metadata = json!({"tool": call.name, "step": self.step, "call_id": id});
        self.emit_step(
            "tool_call",
            &format!("Calling: {}", call.name),
            &input,
            &metadata,
        );
        if self.mode == Mode::Ask {
            self.ask_event(
                "tool_start",
                &json!({"tool": call.name, "input": pyfmt::head(&input, 200), "status": "in_progress"}),
            );
        }
    }

    fn tool_result(&mut self, call_id: &str, tool: &str, content: &str) {
        self.step += 1;
        let preview = pyfmt::preview(content, 300);
        let length = pyfmt::len(content);
        self.total_events += 1;
        self.thinking_steps.push(json!({
            "step": self.step, "type": "tool_result", "tool": tool, "tool_call_id": call_id,
            "call_id": call_id, "output_length": length, "output_preview": preview,
            "output": pyfmt::head(content, 500), "timestamp": self.clock.timestamp(),
        }));
        let metadata = json!({"tool": tool, "step": self.step, "call_id": call_id});
        self.emit_step(
            "tool_result",
            &format!("Result ({length} chars)"),
            pyfmt::head(&preview, 500),
            &metadata,
        );
        if self.mode == Mode::Ask {
            self.ask_event(
                "tool_end",
                &json!({"tool": tool, "status": "completed", "output": pyfmt::head(&preview, 200)}),
            );
        }
    }

    fn todo_update(&mut self, todos: &[Value]) {
        self.total_events += 1;
        let items: Vec<Value> = todos
            .iter()
            .enumerate()
            .map(|(index, todo)| {
                let status = todo
                    .get("status")
                    .and_then(Value::as_str)
                    .unwrap_or("not-started")
                    .replace('_', "-")
                    .replace("pending", "not-started");
                json!({
                    "id": index,
                    "title": todo.get("content").and_then(Value::as_str).unwrap_or(""),
                    "description": "",
                    "status": status,
                })
            })
            .collect();
        let line = json!({"event": "todo_update", "data": {"items": items}});
        self.context.thinking(pyjson::dumps(&line));
    }
}

/// The text of a tool's result. Codebase tools, the file system, the todo
/// list; `Err` only for the stop line.
struct Runner<'a, S: IndexStore> {
    codebase: Codebase<'a, S>,
    known: &'a [&'static str],
    mode: Mode,
}

const PARALLEL_TODOS: &str = "Error: The `write_todos` tool should never be called multiple times in parallel. Please call it only once per model invocation to update the todo list.";

impl<S: IndexStore> Runner<'_, S> {
    async fn run(
        &self,
        call: &Call,
        snapshot: &Vfs,
        todos: &mut Option<Vec<Value>>,
    ) -> Result<(String, Vec<vfs::Update>), EngineError> {
        if !self.known.contains(&call.name.as_str()) {
            return Ok((
                format!(
                    "Error: {} is not a valid tool, try one of [{}].",
                    call.name,
                    self.known.join(", ")
                ),
                Vec::new(),
            ));
        }
        let Some(raw) = &call.args else {
            return Ok((
                format!(
                    "Error: the arguments of tool call '{}' are not a JSON object. Call the tool again with a JSON object.",
                    call.name
                ),
                Vec::new(),
            ));
        };
        let Some(params) = args::params(&call.name) else {
            return Ok((String::new(), Vec::new()));
        };
        let args: Args = match args::validate(params, raw) {
            Ok(args) => args,
            Err(errors) => {
                return Ok((args::invocation_error(&call.name, raw, &errors), Vec::new()));
            }
        };
        if call.name == "write_todos" {
            let list = args.value("todos").as_array().cloned().unwrap_or_default();
            let text = format!(
                "Updated todo list to {}",
                pyfmt::repr(&Value::Array(list.clone()))
            );
            *todos = Some(list);
            return Ok((text, Vec::new()));
        }
        if let Some(outcome) = vfs::run(snapshot, &call.name, &args) {
            return Ok(outcome);
        }
        let text = self
            .codebase
            .run(&call.name, &args)
            .await?
            .unwrap_or_default();
        if self.mode == Mode::Research {
            let (text, update) = vfs::evict(snapshot, &call.name, &call.id, text);
            return Ok((text, update.into_iter().collect()));
        }
        Ok((text, Vec::new()))
    }
}

fn request(spec: &AgentSpec, messages: &[Msg], tool_choice: Option<ToolChoice>) -> ChatRequest {
    let mut chat = vec![crate::llm::ChatMessage::SystemPrompt(spec.system.clone())];
    chat.extend(messages.iter().map(Msg::to_chat));
    ChatRequest {
        messages: chat,
        tools: spec.tools.clone(),
        tool_choice,
        sampling: Sampling::Default,
        max_tokens: Some(spec.max_tokens),
    }
}

async fn summarize<M: Model>(
    spec: &AgentSpec,
    model: &M,
    messages: Vec<Msg>,
    stop: &StopSignal,
) -> Result<Vec<Msg>, EngineError> {
    if !spec.policy.should_summarize(&messages) {
        return Ok(messages);
    }
    let cutoff = spec.policy.cutoff(&messages);
    if cutoff == 0 {
        return Ok(messages);
    }
    let (old, kept) = messages.split_at(cutoff);
    let text = summarize::summary_request(spec.summary_prompt, old);
    let mut chat = ChatRequest::new(vec![crate::llm::ChatMessage::User(text)]);
    chat.max_tokens = Some(spec.max_tokens);
    let response = model.call(&chat, false, stop, &mut |_| {}).await?;
    let mut out = vec![summarize::summary_message(crate::graph::pystr::strip(
        &response.content,
    ))];
    out.extend_from_slice(kept);
    Ok(out)
}

/// Run the loop.
///
/// # Errors
///
/// A model failure, or the stop line.
#[allow(clippy::too_many_lines)] // One step, read top to bottom.
pub async fn run<S: IndexStore, M: Model>(
    spec: &AgentSpec,
    model: &M,
    store: &S,
    embedder: &Embedder,
    context: &Context,
) -> Result<Outcome, EngineError> {
    let stop_signal = context.stop_signal();
    let stop = &stop_signal;
    let mut events = Events::new(context, spec.mode, spec.clock.clone());
    let runner = Runner {
        codebase: Codebase {
            store,
            embedder,
            stop,
            doc_results: spec.doc_results,
        },
        known: &spec.known_tools,
        mode: spec.mode,
    };
    let mut messages = vec![Msg::User(spec.user.clone())];
    let mut fragments: Vec<String> = Vec::new();
    let mut report = String::new();
    let mut vfs = Vfs::default();
    let mut todos: Vec<Value> = Vec::new();
    let mut spent = 0usize;
    loop {
        context.checkpoint()?;
        messages = summarize(spec, model, messages, stop).await?;
        let exhausted = spent >= spec.budget;
        let chat = request(spec, &messages, exhausted.then_some(ToolChoice::None));
        let streaming = spec.streaming;
        let response = {
            let mut on_text = |fragment: &str| {
                if streaming && !fragment.is_empty() {
                    fragments.push(fragment.to_owned());
                    context.token(fragment);
                }
            };
            model.call(&chat, streaming, stop, &mut on_text).await?
        };
        context.checkpoint()?;
        let calls: Vec<Call> = response
            .tool_calls
            .iter()
            .map(Call::from_tool_call)
            .collect();
        if calls.is_empty() && !response.content.is_empty() {
            if spec.mode == Mode::Ask && !streaming {
                fragments.push(response.content.clone());
                context.token(response.content.clone());
            }
            report.clone_from(&response.content);
        }
        let total_tokens = (!streaming)
            .then(|| response.usage.map(|u| u.total_tokens))
            .flatten();
        messages.push(Msg::Ai {
            content: response.content.clone(),
            calls: calls.clone(),
            total_tokens,
        });
        if calls.is_empty() {
            break;
        }
        if exhausted {
            tracing::warn!(mode = ?spec.mode, "the model called tools after its step limit; ending the run");
            break;
        }
        for call in &calls {
            events.tool_call(call);
        }
        let snapshot = vfs.clone();
        let mut updates: Vec<vfs::Update> = Vec::new();
        let mut new_todos: Option<Vec<Value>> = None;
        let parallel_todos = calls.iter().filter(|c| c.name == "write_todos").count() > 1;
        // The parallel write_todos refusals come first (`after_model`).
        let mut order: Vec<&Call> = Vec::new();
        if parallel_todos {
            order.extend(calls.iter().filter(|c| c.name == "write_todos"));
        }
        order.extend(
            calls
                .iter()
                .filter(|c| !(parallel_todos && c.name == "write_todos")),
        );
        for (index, call) in order.into_iter().enumerate() {
            context.checkpoint()?;
            let text = if parallel_todos && call.name == "write_todos" {
                PARALLEL_TODOS.to_owned()
            } else if spec.mode == Mode::Ask && spent >= spec.budget {
                ASK_BUDGET_SPENT.to_owned()
            } else if spec.mode == Mode::Research && index >= MAX_CALLS_PER_STEP {
                format!(
                    "Error: one step may call at most {MAX_CALLS_PER_STEP} tools; call fewer at a time."
                )
            } else {
                if spec.mode == Mode::Ask {
                    spent += 1;
                }
                let (text, writes) = runner.run(call, &snapshot, &mut new_todos).await?;
                updates.extend(writes);
                text
            };
            let tool_call_id = if call.id.is_empty() {
                format!("call_{}", events.step)
            } else {
                call.id.clone()
            };
            events.tool_result(&tool_call_id, &call.name, &text);
            messages.push(Msg::Tool {
                call_id: call.id.clone(),
                name: call.name.clone(),
                content: text,
            });
        }
        vfs.apply(updates);
        if spec.mode == Mode::Research {
            spent += 1;
        }
        if let Some(list) = new_todos
            && list != todos
        {
            todos = list;
            events.todo_update(&todos);
        }
    }
    let answer = match spec.mode {
        Mode::Ask => fragments.concat(),
        Mode::Research => report,
    };
    Ok(Outcome {
        answer,
        todos,
        thinking_steps: events.thinking_steps,
        total_events: events.total_events,
        messages,
    })
}

/// The tool definitions of `names`, from `TOOLS.json`.
///
/// # Errors
///
/// A `RuntimeError` when the embedded file lacks one (a build error).
pub fn definitions(section: &str) -> Result<Vec<ToolDefinition>, EngineError> {
    let parsed: Value = serde_json::from_str(super::prompts::TOOLS)
        .map_err(|_| EngineError::new(ErrorType::Runtime, "TOOLS.json is not JSON"))?;
    let tools = parsed
        .get(section)
        .and_then(Value::as_array)
        .ok_or_else(|| EngineError::new(ErrorType::Runtime, "TOOLS.json lacks a section"))?;
    Ok(tools
        .iter()
        .map(|tool| ToolDefinition {
            name: pyfmt::str_of(&tool["name"]),
            description: pyfmt::str_of(&tool["description"]),
            parameters: tool["parameters"].clone(),
        })
        .collect())
}

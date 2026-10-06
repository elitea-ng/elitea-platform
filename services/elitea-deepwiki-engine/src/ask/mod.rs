//! `ask`, `deep_research` and `resolve_wiki` (ADR-0026 phase 6, decision
//! 8): the query tools of the native engine.
//!
//! * `ask` is the Python `AskEngine` (`DEEPWIKI_ASK_AGENTIC=1`, the path
//!   ADR-0026 keeps): a tool-calling loop over `search_symbols`,
//!   `get_relationships_tool`, `get_code`, `search_docs`, `query_graph`
//!   and `think`, the answer streamed as `token` lines.
//! * `deep_research` is the Python `DeepResearchEngine`: the same loop over
//!   `search_codebase`, `get_symbol_relationships`, `search_graph` and
//!   `think`, plus a todo list (`write_todos`) and an in-memory file system
//!   (`ls`, `read_file`, `write_file`, `edit_file`, `delete`, `glob`,
//!   `grep`); progress as `thinking` lines, no tokens.
//! * `resolve_wiki` is `wiki_query.resolve_wiki` ([`resolve`]).
//!
//! Every read is PostgreSQL ([`store`]); there is no `.wiki.db`.
//!
//! The runner hook is [`run_tool`]; the generic entry points
//! ([`run_ask`], [`run_deep_research`]) take any [`store::IndexStore`] and
//! [`agent::Model`], which is how the parity gate drives them.
//!
//! Deliberate differences from the Python workers, besides those of each
//! module:
//!
//! * the pinned `deepagents` 0.7.13 has no todo list and refuses the
//!   backend FACTORY the Python engine passes (`TypeError` before the first
//!   model call: the live Python deep research cannot run). This port is
//!   the agent ADR-0026 specifies: `write_todos` (`LangChain`'s
//!   `TodoListMiddleware`, its system prompt appended as a second system
//!   block) and no `task` sub-agent (the engine passed `subagents=[]`);
//! * the worker's plain log lines, which the sidecar forwarded as
//!   `thinking` batches (scratch paths, cache keys, timings), are not sent;
//!   deep research's "Cache Selection" step is not sent either;
//! * no repository analysis is loaded (the Python worker read it from the
//!   scratch cache, which a replica does not have): the context is "No
//!   repository overview available." unless the runner passes one;
//! * an unknown wiki is `FileNotFoundError` / `resource_not_found` in the
//!   result; a model failure is an engine error (classified), not a
//!   success-false result carrying a traceback.

pub mod agent;
pub mod args;
pub mod embed;
pub mod jql;
pub mod prompts;
pub mod pyfmt;
pub mod query;
pub mod resolve;
pub mod store;
pub mod summarize;
pub mod symbols;
pub mod tools;
pub mod vfs;

use crate::errors::{EngineError, ErrorType};
use crate::llm::{
    ChatClient, EmbeddingClient, EmbeddingOptions, ModelSettings, SystemPrompt, Transport,
};
use crate::runner::Context;
use agent::{AgentSpec, Clock, Mode, Model};
use embed::Embedder;
use serde_json::{Map, Value, json};
use store::{IndexStore, PgIndex};
use summarize::Policy;

/// The tools this module serves.
pub const QUERY_TOOLS: [&str; 3] = ["ask", "deep_research", "resolve_wiki"];

/// `DEEPWIKI_ASK_MAX_ITERATIONS`' default (Python's).
pub const ASK_MAX_TOOL_CALLS: usize = 8;
/// `ELITEA_DEEPWIKI_RESEARCH_MAX_ITERATIONS`' default.
pub const RESEARCH_MAX_ITERATIONS: usize = 15;
const NO_OVERVIEW: &str = "No repository overview available.";

/// The step limits.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct Limits {
    /// `ask`'s tool calls (`DEEPWIKI_ASK_MAX_ITERATIONS`, the variable
    /// Python's prompt read, so the prompt and the limit agree).
    pub ask_tool_calls: usize,
    /// Deep research's tool-calling steps
    /// (`ELITEA_DEEPWIKI_RESEARCH_MAX_ITERATIONS`).
    pub research_iterations: usize,
}

impl Default for Limits {
    fn default() -> Self {
        Self {
            ask_tool_calls: ASK_MAX_TOOL_CALLS,
            research_iterations: RESEARCH_MAX_ITERATIONS,
        }
    }
}

impl Limits {
    /// The limits from `env` (a lookup, so tests need no process state).
    ///
    /// # Errors
    ///
    /// A `ValueError` for a value that is not a whole number in 1..=100.
    pub fn from_lookup(env: impl Fn(&str) -> Option<String>) -> Result<Self, EngineError> {
        let read = |key: &str, default: usize| -> Result<usize, EngineError> {
            match env(key)
                .map(|v| v.trim().to_owned())
                .filter(|v| !v.is_empty())
            {
                None => Ok(default),
                Some(text) => text
                    .parse::<usize>()
                    .ok()
                    .filter(|n| (1..=100).contains(n))
                    .ok_or_else(|| {
                        EngineError::new(
                            ErrorType::Value,
                            format!("{key} must be a whole number from 1 to 100"),
                        )
                    }),
            }
        };
        Ok(Self {
            ask_tool_calls: read("DEEPWIKI_ASK_MAX_ITERATIONS", ASK_MAX_TOOL_CALLS)?,
            research_iterations: read(
                "ELITEA_DEEPWIKI_RESEARCH_MAX_ITERATIONS",
                RESEARCH_MAX_ITERATIONS,
            )?,
        })
    }

    /// The limits from the process environment.
    ///
    /// # Errors
    ///
    /// See [`Limits::from_lookup`].
    pub fn from_env() -> Result<Self, EngineError> {
        Self::from_lookup(|key| std::env::var(key).ok())
    }
}

/// What the runner hands the query tools.
#[derive(Debug, Clone)]
pub struct QueryDeps {
    /// The `deepwiki` database.
    pub pool: sqlx::PgPool,
    /// The model transport (TLS, timeouts, retries).
    pub transport: Transport,
    pub embedding_options: EmbeddingOptions,
    pub limits: Limits,
    pub clock: Clock,
}

/// The parsed arguments of `ask` / `deep_research`.
#[derive(Debug, Clone, PartialEq)]
pub struct QueryRequest {
    pub question: String,
    /// `normalize_wiki_id(repo_identifier_override or repo:branch)`.
    pub wiki_id: String,
    /// The identifier the messages name (`repo:branch`).
    pub repo_identifier: String,
    /// `User: …` / `Assistant: …`, the last four, 300 characters each.
    pub history: String,
    pub research_type: String,
    /// `llm_settings.max_tokens` when given.
    pub max_tokens: Option<u32>,
}

fn failure(message: &str, error_type: ErrorType) -> Value {
    let error = EngineError::new(error_type, message);
    json!({
        "success": false,
        "error": message,
        "error_type": error_type.wire_name(),
        "error_category": error.category(),
    })
}

fn text(arguments: &Map<String, Value>, key: &str) -> String {
    match arguments.get(key) {
        None | Some(Value::Null) => String::new(),
        Some(value) => pyfmt::str_of(value),
    }
}

/// `ask` / `deep_research` argument handling (`tool_operations.ask` and
/// the worker's preamble). `Err(result)` is the unsuccessful result.
///
/// # Errors
///
/// The result to return: no question, no repository.
pub fn parse_request(arguments: &Map<String, Value>) -> Result<QueryRequest, Value> {
    let question = text(arguments, "question");
    if question.is_empty() {
        return Err(failure("Question parameter is required", ErrorType::Value));
    }
    let empty = Map::new();
    let repo_config = match arguments.get("repo_config") {
        Some(Value::Object(map)) => map,
        _ => &empty,
    };
    let mut repository = text(repo_config, "repository");
    let mut branch = text(repo_config, "branch");
    if repository.is_empty() {
        repository = text(arguments, "github_repository");
        branch = text(arguments, "github_branch");
    }
    if branch.is_empty() {
        "main".clone_into(&mut branch);
    }
    if repository.is_empty() {
        return Err(failure("No repository specified", ErrorType::Value));
    }
    // `build_query_repo_identifier`: the provider-normalised path when the
    // provider config is complete enough (Azure DevOps org/project/repo).
    let mut path =
        crate::graph::pystr::strip_chars(crate::graph::pystr::strip(&repository), "/").to_owned();
    if let Ok(target) = crate::ingest::providers::clone_target(&Value::Object(repo_config.clone()))
    {
        let identifier = crate::graph::pystr::strip_chars(target.repo_identifier(), "/");
        if !identifier.is_empty() {
            identifier.clone_into(&mut path);
        }
    }
    let active = crate::graph::pystr::strip(&branch);
    let active = if active.is_empty() { "main" } else { active };
    let repo_identifier = format!("{path}:{active}");
    let override_id = text(arguments, "repo_identifier_override");
    let override_id = crate::graph::pystr::strip(&override_id);
    let keyed = if override_id.is_empty() {
        repo_identifier.as_str()
    } else {
        override_id
    };
    let wiki_id = crate::wiki::compose::normalize_wiki_id(keyed);
    let history = match arguments.get("chat_history") {
        Some(Value::Array(items)) => {
            let start = items.len().saturating_sub(4);
            items[start..]
                .iter()
                .filter_map(Value::as_object)
                .map(|m| {
                    let who = if m.get("role").and_then(Value::as_str) == Some("user") {
                        "User"
                    } else {
                        "Assistant"
                    };
                    let content = m.get("content").map(pyfmt::str_of).unwrap_or_default();
                    format!("{who}: {}", pyfmt::head(&content, 300))
                })
                .collect::<Vec<_>>()
                .join("\n")
        }
        _ => String::new(),
    };
    let research_type = match text(arguments, "research_type") {
        t if t.is_empty() => "general".to_owned(),
        t => t,
    };
    let max_tokens = match arguments.get("llm_settings") {
        Some(Value::Object(settings)) => settings
            .get("max_tokens")
            .and_then(Value::as_u64)
            .and_then(|n| u32::try_from(n).ok()),
        _ => None,
    };
    Ok(QueryRequest {
        question,
        wiki_id,
        repo_identifier,
        history,
        research_type,
        max_tokens,
    })
}

/// The `ask` agent's setup.
///
/// # Errors
///
/// A build error (templates, `TOOLS.json`).
pub fn ask_spec(
    request: &QueryRequest,
    repo_context: Option<&str>,
    model: &str,
    anthropic: bool,
    streaming: bool,
    limits: Limits,
    clock: Clock,
) -> Result<AgentSpec, EngineError> {
    let system = prompts::ask_system(&clock.date(), limits.ask_tool_calls)
        .map_err(|e| EngineError::new(ErrorType::Runtime, e.to_string()))?;
    let context = repo_context.filter(|c| !c.is_empty()).map_or_else(
        || NO_OVERVIEW.to_owned(),
        |c| format!("Repository Summary: {c}"),
    );
    Ok(AgentSpec {
        mode: Mode::Ask,
        system: SystemPrompt::text(system),
        user: prompts::ask_user(&request.question, &context, &request.history),
        tools: agent::definitions("ask")?,
        known_tools: tools::ASK_TOOLS.to_vec(),
        streaming,
        max_tokens: request.max_tokens.unwrap_or(4096),
        budget: limits.ask_tool_calls,
        // A streamed answer reports no usage to scale by.
        policy: Policy::for_model(model, anthropic, !streaming),
        summary_prompt: prompts::SUMMARY,
        clock,
    })
}

/// The deep-research agent's setup.
///
/// # Errors
///
/// A build error.
pub fn research_spec(
    request: &QueryRequest,
    repo_context: Option<&str>,
    model: &str,
    anthropic: bool,
    limits: Limits,
    clock: Clock,
) -> Result<AgentSpec, EngineError> {
    let system = prompts::research_system(&clock.date())
        .map_err(|e| EngineError::new(ErrorType::Runtime, e.to_string()))?;
    let context = repo_context.filter(|c| !c.is_empty()).map_or_else(
        || NO_OVERVIEW.to_owned(),
        |c| format!("Repository Summary: {c}"),
    );
    let mut known = vec![
        "ls",
        "read_file",
        "write_file",
        "edit_file",
        "delete",
        "glob",
        "grep",
        "execute",
        "write_todos",
    ];
    known.extend_from_slice(tools::RESEARCH_TOOLS);
    Ok(AgentSpec {
        mode: Mode::Research,
        system: SystemPrompt::blocks(vec![system, format!("\n\n{}", prompts::TODO_SYSTEM)]),
        user: prompts::research_user(&request.research_type, &request.question, &context),
        tools: agent::definitions("deep_research")?,
        known_tools: known,
        streaming: false,
        max_tokens: request.max_tokens.unwrap_or(8192),
        budget: limits.research_iterations,
        policy: Policy::for_model(model, anthropic, true),
        summary_prompt: prompts::SUMMARY_DEEPAGENTS,
        clock,
    })
}

fn start_steps(mode: Mode, events: &agent::Events<'_>, question: &str) {
    let content = format!("Question: {}...", pyfmt::head(question, 200));
    match mode {
        Mode::Ask => {
            events.emit_step("start", "Agentic Ask Started", &content, &json!({}));
            events.emit_step(
                "engine",
                "Ask Engine",
                "Initializing agent with progressive disclosure tools...",
                &json!({}),
            );
        }
        Mode::Research => {
            events.emit_step("start", "Research Started", &content, &json!({}));
            events.emit_step(
                "init",
                "Initializing",
                "Building LLM and embeddings...",
                &json!({}),
            );
            events.emit_step(
                "engine",
                "Research Engine",
                "Initializing DeepAgents agent with subagents...",
                &json!({}),
            );
            events.emit_step(
                "loop",
                "Research Loop",
                "Beginning iterative research...",
                &json!({}),
            );
        }
    }
}

/// Run `ask` or deep research over `store` with `model`.
///
/// # Errors
///
/// A model failure, or the stop line.
pub async fn run_agent<S: IndexStore, M: Model>(
    spec: &AgentSpec,
    request: &QueryRequest,
    model: &M,
    store: &S,
    embedder: &Embedder,
    context: &Context,
) -> Result<Value, EngineError> {
    context.thinking(match spec.mode {
        Mode::Ask => "Processing your question...",
        Mode::Research => "Starting deep research analysis...",
    });
    let events = agent::Events::new(context, spec.mode, spec.clock.clone());
    start_steps(spec.mode, &events, &request.question);
    if !store.wiki_exists().await? {
        let message = format!(
            "No wiki index found for {}. Please generate a wiki first.",
            request.repo_identifier
        );
        return Ok(failure(&message, ErrorType::FileNotFound));
    }
    let outcome = match agent::run(spec, model, store, embedder, context).await {
        Ok(outcome) => outcome,
        Err(error) => {
            if error != EngineError::cancelled() {
                let title = if spec.mode == Mode::Ask {
                    "Ask Failed"
                } else {
                    "Research Failed"
                };
                events.emit_step("error", title, error.wire_message(), &json!({}));
            }
            return Err(error);
        }
    };
    Ok(match spec.mode {
        Mode::Ask => {
            events.emit_step(
                "complete",
                "Ask Complete",
                &format!("Generated {} character answer", pyfmt::len(&outcome.answer)),
                &json!({}),
            );
            json!({
                "success": true,
                "answer": outcome.answer,
                "sources": [],
                "thinking_steps": outcome.thinking_steps,
                "query_used": format!("[agentic] {}", pyfmt::head(&request.question, 200)),
                "documents_retrieved": 0,
                "agentic": true,
            })
        }
        Mode::Research => {
            events.emit_step(
                "complete",
                "Research Complete",
                &format!("Generated {} character report", pyfmt::len(&outcome.answer)),
                &json!({}),
            );
            json!({
                "success": true,
                "report": outcome.answer,
                "todos": outcome.todos,
                "thinking_steps": outcome.thinking_steps,
                "total_events": outcome.total_events + 2,
            })
        }
    })
}

/// The runner hook: serve `ask`, `deep_research` or `resolve_wiki`.
///
/// `arguments` are what [`crate::runner::prepare_arguments`] left (the
/// attachment keys resolved by the host). `repository_analysis` is the
/// analysis text when the runner has one.
///
/// # Errors
///
/// An unknown tool (`KeyError`), malformed `llm_settings` (`ValueError`),
/// a model failure, or the stop line.
pub async fn run_tool(
    tool: &str,
    arguments: &Map<String, Value>,
    deps: &QueryDeps,
    repository_analysis: Option<&str>,
    context: &Context,
) -> Result<Value, EngineError> {
    if tool == "resolve_wiki" {
        return resolve::resolve_wiki(arguments, &deps.transport, context).await;
    }
    let mode = match tool {
        "ask" => Mode::Ask,
        "deep_research" => Mode::Research,
        _ => {
            return Err(EngineError::new(
                ErrorType::Key,
                format!("Unknown tool: {tool}"),
            ));
        }
    };
    let request = match parse_request(arguments) {
        Ok(request) => request,
        Err(result) => return Ok(result),
    };
    let llm_settings = arguments
        .get("llm_settings")
        .cloned()
        .unwrap_or(Value::Null);
    let settings = ModelSettings::from_llm_settings(&llm_settings)?;
    let streaming = settings.streaming;
    let model_name = settings.model_name.clone();
    let anthropic = settings.provider == crate::llm::Provider::Anthropic;
    let embedder = match arguments
        .get("embedding_model")
        .map(crate::llm::embedding_model_name)
    {
        Some(Ok(name)) => Embedder::Client(EmbeddingClient::new(
            deps.transport.clone(),
            settings.clone(),
            name,
            deps.embedding_options,
        )),
        _ => Embedder::None,
    };
    let client = ChatClient::new(deps.transport.clone(), settings);
    let spec = match mode {
        Mode::Ask => ask_spec(
            &request,
            repository_analysis,
            &model_name,
            anthropic,
            streaming,
            deps.limits,
            deps.clock.clone(),
        )?,
        Mode::Research => research_spec(
            &request,
            repository_analysis,
            &model_name,
            anthropic,
            deps.limits,
            deps.clock.clone(),
        )?,
    };
    let store = PgIndex::new(crate::storage::adapter::UnifiedDb::new(
        deps.pool.clone(),
        request.wiki_id.clone(),
    ));
    run_agent(&spec, &request, &client, &store, &embedder, context).await
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn requests_name_the_wiki_as_python_did() {
        let arguments = json!({
            "question": "How?",
            "repo_config": {"provider_type": "github", "repository": "Acme/Notes_Service", "branch": "dev"},
            "chat_history": [
                {"role": "user", "content": "a"}, {"role": "assistant", "content": "b"},
                {"role": "user", "content": "c"}, {"role": "x", "content": "d"},
                {"role": "user", "content": "e".repeat(400)}
            ],
            "llm_settings": {"max_tokens": 100},
        });
        let request = parse_request(arguments.as_object().unwrap_or(&Map::new()));
        let request = request.unwrap_or_else(|_| QueryRequest {
            question: String::new(),
            wiki_id: String::new(),
            repo_identifier: String::new(),
            history: String::new(),
            research_type: String::new(),
            max_tokens: None,
        });
        assert_eq!(request.wiki_id, "acme--notes-service--dev");
        assert_eq!(request.repo_identifier, "Acme/Notes_Service:dev");
        assert_eq!(request.max_tokens, Some(100));
        assert_eq!(request.research_type, "general");
        let lines: Vec<&str> = request.history.lines().collect();
        assert_eq!(lines.len(), 4);
        assert_eq!(lines[0], "Assistant: b");
        assert_eq!(lines[2], "Assistant: d");
        assert_eq!(lines[3].len(), "User: ".len() + 300);
    }

    #[test]
    fn a_request_without_a_question_or_repository_is_refused() {
        let missing = parse_request(&Map::new()).err().unwrap_or_default();
        assert_eq!(missing["error"], "Question parameter is required");
        assert_eq!(missing["error_category"], "invalid_input");
        let arguments = json!({"question": "q"});
        let missing = parse_request(arguments.as_object().unwrap_or(&Map::new()))
            .err()
            .unwrap_or_default();
        assert_eq!(missing["error"], "No repository specified");
        let arguments = json!({"question": "q", "repo_config": {"repository": "a/b"},
            "repo_identifier_override": " Org/Proj/Repo:main:abc12345 "});
        let ok = parse_request(arguments.as_object().unwrap_or(&Map::new()));
        assert_eq!(
            ok.map(|r| r.wiki_id).ok().as_deref(),
            Some("org--proj--repo--main")
        );
    }

    #[test]
    fn limits_are_strict() {
        let none = |_: &str| None;
        assert_eq!(Limits::from_lookup(none), Ok(Limits::default()));
        let set = |key: &str| (key == "DEEPWIKI_ASK_MAX_ITERATIONS").then(|| "3".to_owned());
        assert_eq!(Limits::from_lookup(set).map(|l| l.ask_tool_calls), Ok(3));
        let bad = |_: &str| Some("0".to_owned());
        assert!(Limits::from_lookup(bad).is_err());
    }
}

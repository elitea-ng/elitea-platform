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
//! ([`run_agent`] with [`ask_spec`] or [`research_spec`]) take any [`store::IndexStore`] and
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
/// The largest step limit: a larger value is lowered to it (Python had no
/// upper bound and enforced no limit).
pub const MAX_STEP_LIMIT: usize = 1_000;
/// The largest `DEEPWIKI_MAX_DOC_RESULTS` (`search_codebase`'s own `k`
/// limit).
pub const MAX_DOC_RESULTS_LIMIT: usize = 100;
const NO_OVERVIEW: &str = "No repository overview available.";
/// The embedding model of a query that names none (the ask and
/// deep-research workers' default).
pub const DEFAULT_EMBEDDING_MODEL: &str = "text-embedding-3-large";

/// The step limits.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct Limits {
    /// `ask`'s tool calls (`DEEPWIKI_ASK_MAX_ITERATIONS`, the variable
    /// Python's prompt read, so the prompt and the limit agree).
    pub ask_tool_calls: usize,
    /// Deep research's tool-calling steps
    /// (`ELITEA_DEEPWIKI_RESEARCH_MAX_ITERATIONS`).
    pub research_iterations: usize,
    /// `search_codebase`'s documentation results at most
    /// (`DEEPWIKI_MAX_DOC_RESULTS`, default 3; 0 searches no documents).
    pub doc_results: usize,
}

impl Default for Limits {
    fn default() -> Self {
        Self {
            ask_tool_calls: ASK_MAX_TOOL_CALLS,
            research_iterations: RESEARCH_MAX_ITERATIONS,
            doc_results: tools::MAX_DOC_RESULTS,
        }
    }
}

/// `int(text)` as Python parses an environment value: an optional sign,
/// digits with single `_` between them, blanks around.
fn python_int(text: &str) -> Option<i64> {
    let text = text.trim();
    let digits = text.strip_prefix(['+', '-']).unwrap_or(text);
    if digits.is_empty()
        || digits.starts_with('_')
        || digits.ends_with('_')
        || digits.contains("__")
        || !digits.chars().all(|c| c.is_ascii_digit() || c == '_')
    {
        return None;
    }
    let clean: String = text.chars().filter(|c| *c != '_').collect();
    // A number too large for i64 is still a number: beyond every bound.
    Some(clean.parse::<i64>().unwrap_or(if clean.starts_with('-') {
        i64::MIN
    } else {
        i64::MAX
    }))
}

impl Limits {
    /// The limits from `env` (a lookup, so tests need no process state).
    ///
    /// Python read these with `int(...)` and accepted every whole number.
    /// A whole number outside the range this engine can use is moved into
    /// it, with a warning: the step limits into 1..=[`MAX_STEP_LIMIT`],
    /// the document results into 0..=[`MAX_DOC_RESULTS_LIMIT`].
    ///
    /// # Errors
    ///
    /// A `ValueError` for a value that is not a whole number (Python's
    /// `int()` failed there too, and every query failed).
    pub fn from_lookup(env: impl Fn(&str) -> Option<String>) -> Result<Self, EngineError> {
        let read = |key: &str, default: usize, low: usize, high: usize| {
            let Some(text) = env(key).filter(|v| !v.trim().is_empty()) else {
                return Ok(default);
            };
            let value = python_int(&text).ok_or_else(|| {
                EngineError::new(
                    ErrorType::Value,
                    format!("{key} must be a whole number, got '{text}'"),
                )
            })?;
            let low_i = i64::try_from(low).unwrap_or(i64::MAX);
            let high_i = i64::try_from(high).unwrap_or(i64::MAX);
            let used = usize::try_from(value.clamp(low_i, high_i)).unwrap_or(low);
            if i64::try_from(used).ok() != Some(value) {
                tracing::warn!(
                    key,
                    value,
                    used,
                    "the value is out of range; using the nearest one"
                );
            }
            Ok(used)
        };
        Ok(Self {
            ask_tool_calls: read(
                "DEEPWIKI_ASK_MAX_ITERATIONS",
                ASK_MAX_TOOL_CALLS,
                1,
                MAX_STEP_LIMIT,
            )?,
            research_iterations: read(
                "ELITEA_DEEPWIKI_RESEARCH_MAX_ITERATIONS",
                RESEARCH_MAX_ITERATIONS,
                1,
                MAX_STEP_LIMIT,
            )?,
            doc_results: read(
                "DEEPWIKI_MAX_DOC_RESULTS",
                tools::MAX_DOC_RESULTS,
                0,
                MAX_DOC_RESULTS_LIMIT,
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

/// The embedding model of `ask` / `deep_research`.
///
/// None given (absent, `null`, blank, or an object without `model_name`):
/// [`DEFAULT_EMBEDDING_MODEL`], as the Python workers did, with a warning.
/// A gateway that does not serve that model fails the embedding call and
/// the search is lexical only, as in Python; a wiki embedded with another
/// model has another dimension, and its search tools report the failure.
///
/// # Errors
///
/// A `ValueError` for another shape (a number, a list, a `model_name`
/// that is not a string). Python used the default for a number or a
/// list and failed at the first embedding call for such a `model_name`;
/// this engine refuses them all at once, as its `generate_wiki` does.
pub fn query_embedding_model(arguments: &Map<String, Value>) -> Result<String, EngineError> {
    let value = arguments.get("embedding_model").unwrap_or(&Value::Null);
    let blank = |text: &str| text.trim().is_empty();
    let missing = match value {
        Value::Null => true,
        Value::String(text) => blank(text),
        Value::Object(map) => match map.get("model_name") {
            None | Some(Value::Null) => true,
            Some(Value::String(text)) => blank(text),
            Some(_) => false,
        },
        _ => false,
    };
    if missing {
        tracing::warn!(
            model = DEFAULT_EMBEDDING_MODEL,
            "the query names no embedding_model; using the Python default"
        );
        return Ok(DEFAULT_EMBEDDING_MODEL.to_owned());
    }
    crate::llm::embedding_model_name(value)
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
    // An artifact folder is named as its generation names it
    // (`source::artifact_wiki_id`): `normalize_wiki_id("artifact://…:branch")`
    // would read the scheme's colon as the branch separator.
    let folder = if override_id.is_empty() && crate::source::is_artifact_source(&repository) {
        crate::source::parse_artifact_source(&repository)
            .ok()
            .map(|source| crate::source::artifact_wiki_id(&source, Some(active)))
    } else {
        None
    };
    let wiki_id = folder.unwrap_or_else(|| crate::wiki::compose::normalize_wiki_id(keyed));
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
        doc_results: limits.doc_results,
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
        doc_results: limits.doc_results,
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
/// An unknown tool (`KeyError`), malformed `llm_settings` or
/// `embedding_model` or a missing project
/// ([`crate::storage::PROJECT_ARG`]) (`ValueError`),
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
    // The project the host authenticated (migration 0005): the index is
    // read within it and nowhere else. A wiki id, from
    // `repo_identifier_override` or from the repository, resolves in the
    // caller's project only.
    let project = crate::storage::ProjectScope::from_arguments(arguments)?;
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
    let embedder = Embedder::Client(EmbeddingClient::new(
        deps.transport.clone(),
        settings.clone(),
        query_embedding_model(arguments)?,
        deps.embedding_options,
    ));
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
        crate::storage::WikiKey::new(project, request.wiki_id.clone()),
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
    fn an_artifact_folder_is_asked_under_its_generation_id() {
        let wiki_id = |arguments: Value| {
            parse_request(arguments.as_object().unwrap_or(&Map::new()))
                .map(|r| r.wiki_id)
                .unwrap_or_default()
        };
        for config in [
            json!({"provider_type": "artifact", "repository": "artifact://docs/handbook", "branch": "main"}),
            json!({"provider_type": "artifact", "repository": "ARTIFACT://Docs/handbook/"}),
        ] {
            assert_eq!(
                wiki_id(json!({"question": "q", "repo_config": config})),
                "artifact--docs--handbook--main"
            );
        }
        // The manifest's identifier still wins when the host passes it.
        assert_eq!(
            wiki_id(
                json!({"question": "q", "repo_config": {"repository": "artifact://docs/handbook"},
                "repo_identifier_override": "artifact://docs/handbook:v2:abcdef01"})
            ),
            "artifact--docs--handbook--v2"
        );
        // A git repository is unchanged.
        assert_eq!(
            wiki_id(
                json!({"question": "q", "repo_config": {"repository": "acme/notes", "branch": "main"}})
            ),
            "acme--notes--main"
        );
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
    fn a_missing_embedding_model_is_the_python_default() {
        let model = |value: Value| {
            let mut arguments = Map::new();
            arguments.insert("embedding_model".to_owned(), value);
            query_embedding_model(&arguments).map_err(|e| (e.error_type, e.message))
        };
        assert_eq!(
            query_embedding_model(&Map::new()).as_deref(),
            Ok(DEFAULT_EMBEDDING_MODEL)
        );
        for missing in [
            Value::Null,
            json!(""),
            json!("  "),
            json!({}),
            json!({"model_name": null}),
            json!({"model_name": " "}),
        ] {
            assert_eq!(
                model(missing.clone()).as_deref(),
                Ok(DEFAULT_EMBEDDING_MODEL),
                "{missing}"
            );
        }
        assert_eq!(model(json!(" emb-1 ")).as_deref(), Ok("emb-1"));
        assert_eq!(
            model(json!({"model_name": "emb-2"})).as_deref(),
            Ok("emb-2")
        );
        for malformed in [
            json!(5),
            json!(["emb"]),
            json!({"model_name": 7}),
            json!(true),
        ] {
            let refused = model(malformed.clone());
            assert!(
                matches!(&refused, Err((ErrorType::Value, m)) if m.contains("embedding_model")),
                "{malformed}: {refused:?}"
            );
        }
    }

    #[test]
    fn limits_accept_what_python_accepted() {
        let none = |_: &str| None;
        assert_eq!(Limits::from_lookup(none), Ok(Limits::default()));
        let one = |key: &'static str, value: &'static str| {
            move |k: &str| (k == key).then(|| value.to_owned())
        };
        let ask = |value| {
            Limits::from_lookup(one("DEEPWIKI_ASK_MAX_ITERATIONS", value)).map(|l| l.ask_tool_calls)
        };
        assert_eq!(ask("3"), Ok(3));
        // Above the old 1..=100 range: Python accepted it, so does this.
        assert_eq!(ask(" 150 "), Ok(150));
        assert_eq!(ask("1_000"), Ok(1_000));
        // Out of range: moved into it.
        assert_eq!(ask("0"), Ok(1));
        assert_eq!(ask("-4"), Ok(1));
        assert_eq!(ask("5000"), Ok(MAX_STEP_LIMIT));
        assert_eq!(ask("99999999999999999999999"), Ok(MAX_STEP_LIMIT));
        // Not a whole number: Python's int() raised ValueError.
        for bad in ["eight", "1.5", "1__0", "_1", "+"] {
            assert_eq!(
                ask(bad).map_err(|e| e.error_type),
                Err(ErrorType::Value),
                "{bad}"
            );
        }
        let research = Limits::from_lookup(one("ELITEA_DEEPWIKI_RESEARCH_MAX_ITERATIONS", "40"));
        assert_eq!(research.map(|l| l.research_iterations), Ok(40));
        let docs = |value| {
            Limits::from_lookup(one("DEEPWIKI_MAX_DOC_RESULTS", value)).map(|l| l.doc_results)
        };
        assert_eq!(docs("5"), Ok(5));
        assert_eq!(docs("0"), Ok(0));
        assert_eq!(docs("-2"), Ok(0));
        assert_eq!(docs("500"), Ok(MAX_DOC_RESULTS_LIMIT));
        assert!(docs("x").is_err());
    }
}

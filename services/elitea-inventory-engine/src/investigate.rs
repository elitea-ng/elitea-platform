//! `investigate` (ADR-0027 P4): a model agent over the graph and, through
//! the platform, over the graph's source toolkits.
//!
//! The Python tool (`_tool_investigate` → `inventory_chat`) never answered
//! in v1: its first step fetched a platform admin client the service no
//! longer has, and the facade minted it no model credential. The design is
//! kept; the plumbing is new:
//!
//! * the model is the toolkit's, reached with the callback block the
//!   facade now mints for `investigate` (`llm_settings`), with native tool
//!   calling at temperature 0.1 and 4096 tokens, at most 50 rounds — the
//!   Python `DEFAULT_*` constants;
//! * the system prompt is `INVENTORY_CHAT_SYSTEM_PROMPT` with the filters
//!   (the all-tools "hybrid" strategy: the Python router's `MiniLM` tier was
//!   not installed, so it is the prompt v1 would have used for most
//!   questions), and the tools carry the Python `TOOL_DESCRIPTIONS`;
//! * the graph tools are this engine's own retrieval handlers
//!   (`crate::retrieval`), the text the `inventory_search` tools return;
//! * a source toolkit's read-only tools (Python's `_filter_read_only_tools`
//!   over the SDK catalogue, `assets/source_tools.json`) are called through
//!   elitea-main's `test_tool` route with the invocation's own bearer, so
//!   the credentials stay in the platform and the user's own permissions
//!   apply; tools are named `{toolkit_name[:20]}_{tool}` as in Python;
//! * citations are read from the answer, as `_extract_citations_from_answer`
//!   did;
//! * the conversation is summarised when it nears the model's context
//!   (`elitea-conversation`, `LangChain`'s `SummarizationMiddleware` as the
//!   `DeepWiki` agents use it): fifty rounds of tool output would otherwise
//!   overflow it, where the Python agent simply failed.

use crate::extract::assets::render;
use crate::retrieval::{self, Call, view::GraphView};
use elitea_conversation::{DEFAULT_SUMMARY_PROMPT, Msg, Policy, compact};
use elitea_engine_core::errors::EngineError;
use elitea_engine_core::stream::StopSignal;
use elitea_model_client::chat::{
    ChatMessage, ChatRequest, ChatResponse, Sampling, SystemPrompt, ToolDefinition,
};
use serde_json::{Map, Value, json};
use std::collections::BTreeSet;
use std::future::Future;
use std::pin::Pin;
use std::sync::{Arc, OnceLock};

const SOURCE_TOOLS: &str = include_str!("../assets/source_tools.json");
const ASSET: &str = include_str!("../assets/python_inventory.json");

/// The answer the model gives when the rounds run out (the SDK's message).
const LIMIT_MESSAGE: &str =
    "Maximum tool execution iterations (50) reached. Stopping tool execution.";

/// A future the agent awaits.
pub type Boxed<T> = Pin<Box<dyn Future<Output = Result<T, EngineError>> + Send>>;

/// One chat completion with tools (the gateway client in production).
pub type Chat = Arc<dyn Fn(ChatRequest) -> Boxed<ChatResponse> + Send + Sync>;

/// The model an investigation talks to: its completions, and when its
/// conversation is summarised (`Policy::for_model` of its name).
#[derive(Clone)]
pub struct ChatModel {
    pub chat: Chat,
    pub policy: Policy,
}

/// One source-toolkit tool call: `(toolkit_id, tool_name, arguments)` → the
/// tool's result as text.
pub type SourceCall =
    Arc<dyn Fn(String, String, Map<String, Value>) -> Boxed<String> + Send + Sync>;

/// Rank the graph's entities against one query: the query embedded with
/// the graph's stamped model (through the gateway), ranked by PostgreSQL at
/// the chat tool's minimum score.
pub type Embed = Arc<dyn Fn(String) -> Boxed<retrieval::semantic::Ranking> + Send + Sync>;

/// A source toolkit of the graph (its `sources` row).
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct SourceToolkit {
    pub toolkit_id: String,
    pub name: String,
    pub kind: String,
}

/// What the question is about.
#[derive(Debug, Clone, Default, PartialEq)]
pub struct Question {
    pub text: String,
    pub entity_types: Vec<String>,
    pub sources: Vec<String>,
    pub layers: Vec<String>,
    pub depth: Value,
    pub max_nodes: Value,
}

fn list_param(params: &Map<String, Value>, key: &str) -> Vec<String> {
    match params.get(key) {
        Some(Value::String(text)) if !text.is_empty() => {
            text.split(',').map(|item| item.trim().to_owned()).collect()
        }
        Some(Value::Array(items)) if !items.is_empty() => items
            .iter()
            .map(|item| {
                item.as_str()
                    .map_or_else(|| item.to_string(), str::to_owned)
            })
            .collect(),
        _ => Vec::new(),
    }
}

impl Question {
    /// `_tool_investigate`'s parameters; `None` without a question.
    #[must_use]
    pub fn from_params(params: &Map<String, Value>) -> Option<Self> {
        let text = ["question", "query", "prompt"]
            .iter()
            .find_map(|key| {
                params
                    .get(*key)
                    .and_then(Value::as_str)
                    .filter(|q| !q.is_empty())
            })?
            .to_owned();
        Some(Self {
            text,
            entity_types: list_param(params, "entity_types"),
            sources: list_param(params, "sources"),
            layers: list_param(params, "layers"),
            depth: params.get("depth").cloned().unwrap_or(json!(2)),
            max_nodes: params.get("max_nodes").cloned().unwrap_or(json!(500)),
        })
    }

    /// The `## Current Settings` lines.
    fn filter_text(&self) -> String {
        let show = |value: &Value| match value {
            Value::String(text) => text.clone(),
            other => other.to_string(),
        };
        let mut lines = vec![
            format!(
                "Depth: {} (relationship hops to traverse)",
                show(&self.depth)
            ),
            format!(
                "Max nodes: {} (maximum results to return)",
                show(&self.max_nodes)
            ),
        ];
        if !self.entity_types.is_empty() {
            lines.push(format!("Entity types: {}", self.entity_types.join(", ")));
        }
        if !self.sources.is_empty() {
            lines.push(format!("Sources: {}", self.sources.join(", ")));
        }
        if !self.layers.is_empty() {
            lines.push(format!("Layers: {}", self.layers.join(", ")));
        }
        lines.join("\n")
    }
}

/// What an investigation found.
#[derive(Debug, Clone, Default, PartialEq)]
pub struct Investigation {
    pub answer: String,
    pub citations: Vec<Value>,
    pub tool_calls: Vec<Value>,
    pub tokens_in: u64,
    pub tokens_out: u64,
    pub error: Option<String>,
}

fn investigate_asset() -> &'static Value {
    static PARSED: OnceLock<Value> = OnceLock::new();
    PARSED.get_or_init(|| {
        serde_json::from_str::<Value>(ASSET)
            .map_or(Value::Null, |asset| asset["investigate"].clone())
    })
}

fn source_tool_schemas() -> &'static Value {
    static PARSED: OnceLock<Value> = OnceLock::new();
    PARSED.get_or_init(|| {
        serde_json::from_str::<Value>(SOURCE_TOOLS)
            .map_or(Value::Null, |asset| asset["tools"].clone())
    })
}

fn description(name: &str) -> String {
    investigate_asset()["tool_descriptions"][name]
        .as_str()
        .unwrap_or(name)
        .to_owned()
}

/// A graph tool: its schema, and how its arguments become the handler's.
struct GraphTool {
    name: &'static str,
    /// The retrieval tool and family that answer it.
    handler: (&'static str, &'static str),
    parameters: Value,
}

fn graph_tools() -> Vec<GraphTool> {
    let entity = json!({"type": "object", "properties": {
        "entity_name": {"type": "string", "description": "The entity: \"Name\", \"Name (type)\" or a full search-result reference"}
    }, "required": ["entity_name"]});
    vec![
        GraphTool {
            name: "search_knowledge_graph",
            handler: ("search_knowledge_graph", "inventory_search"),
            parameters: json!({"type": "object", "properties": {
                "query": {"type": "string"},
                "top_k": {"type": "integer", "default": 20}
            }, "required": ["query"]}),
        },
        GraphTool {
            name: "get_entity_details",
            handler: ("get_entity_details", "inventory_search"),
            parameters: entity.clone(),
        },
        GraphTool {
            name: "get_related_entities",
            handler: ("get_related_entities", "inventory_search"),
            parameters: entity.clone(),
        },
        GraphTool {
            name: "query_graph",
            handler: ("query_graph", "inventory_search"),
            parameters: json!({"type": "object", "properties": {
                "query": {"type": "string", "description": "type: layer: file: name: related: rel: dir: has_rel: limit:"}
            }, "required": ["query"]}),
        },
        GraphTool {
            name: "list_entity_types",
            handler: ("list_entity_types", "inventory_search"),
            parameters: json!({"type": "object", "properties": {}}),
        },
        GraphTool {
            name: "impact_analysis",
            handler: ("impact_analysis", "inventory"),
            parameters: entity,
        },
    ]
}

/// `_filter_read_only_tools`.
#[must_use]
pub fn read_only(name: &str) -> bool {
    let rules = investigate_asset();
    let lowered = name.to_lowercase();
    let strings = |key: &str| -> Vec<String> {
        rules[key]
            .as_array()
            .into_iter()
            .flatten()
            .filter_map(Value::as_str)
            .map(str::to_owned)
            .collect()
    };
    strings("read_only_prefixes")
        .iter()
        .any(|p| lowered.starts_with(p.as_str()))
        && !strings("write_operation_patterns")
            .iter()
            .any(|w| lowered.contains(w.as_str()))
}

/// `re.sub(r'[^a-zA-Z0-9_]', '_', name)[:20]`, the tool-name prefix.
fn sanitize(name: &str) -> String {
    name.chars()
        .map(|c| {
            if c.is_ascii_alphanumeric() || c == '_' {
                c
            } else {
                '_'
            }
        })
        .take(20)
        .collect()
}

/// One offered tool, and what answers it.
enum Offered {
    Graph(&'static str, &'static str),
    Local(Local),
    Source { toolkit_id: String, tool: String },
}

/// The chat agent's own tools (`_build_chat_tools`), answered by the
/// retrieval functions that port them.
#[derive(Clone, Copy)]
enum Local {
    Pattern,
    Vocabulary,
    Semantic,
    ListCommunities,
    CommunityDetail,
    FindCommunity,
    SearchCommunity,
}

fn local_tools(view: &GraphView, semantic: bool) -> Vec<(&'static str, Local, Value)> {
    let mut tools = vec![
        (
            "query_pattern",
            Local::Pattern,
            json!({"type": "object", "properties": {"pattern": {"type": "string",
                "description": "A Cypher-like pattern, e.g. (A:class)-[:calls*1..2]->(?)"}}, "required": ["pattern"]}),
        ),
        (
            "get_pattern_vocabulary",
            Local::Vocabulary,
            json!({"type": "object", "properties": {}}),
        ),
    ];
    if semantic {
        tools.push((
            "semantic_search",
            Local::Semantic,
            json!({"type": "object", "properties": {"query": {"type": "string"},
                "top_k": {"type": "integer", "default": 10}}, "required": ["query"]}),
        ));
    }
    if retrieval::community_tools::has_communities(view) {
        tools.push((
            "list_communities",
            Local::ListCommunities,
            json!({"type": "object", "properties": {"top_n": {"type": "integer", "default": 0}}}),
        ));
        tools.push((
            "get_community_detail",
            Local::CommunityDetail,
            json!({"type": "object", "properties": {"community_id": {"type": "string"}}, "required": ["community_id"]}),
        ));
        tools.push((
            "find_entity_community",
            Local::FindCommunity,
            json!({"type": "object", "properties": {"entity_name": {"type": "string"}}, "required": ["entity_name"]}),
        ));
        tools.push((
            "search_within_community",
            Local::SearchCommunity,
            json!({"type": "object", "properties": {"community_id": {"type": "string"}, "query": {"type": "string"}},
                "required": ["community_id", "query"]}),
        ));
    }
    tools
}

fn offered_tools(
    view: &GraphView,
    semantic: bool,
    sources: &[SourceToolkit],
) -> (Vec<ToolDefinition>, Vec<(String, Offered)>) {
    let mut definitions = Vec::new();
    let mut routes = Vec::new();
    for (name, local, parameters) in local_tools(view, semantic) {
        definitions.push(ToolDefinition {
            name: name.to_owned(),
            description: description(name),
            parameters,
        });
        routes.push((name.to_owned(), Offered::Local(local)));
    }
    for tool in graph_tools() {
        definitions.push(ToolDefinition {
            name: tool.name.to_owned(),
            description: description(tool.name),
            parameters: tool.parameters,
        });
        routes.push((
            tool.name.to_owned(),
            Offered::Graph(tool.handler.0, tool.handler.1),
        ));
    }
    for source in sources {
        let Some(tools) = source_tool_schemas()[source.kind.as_str()].as_object() else {
            continue;
        };
        for (tool, schema) in tools {
            let name = format!("{}_{tool}", sanitize(&source.name));
            if routes.iter().any(|(known, _)| *known == name) {
                continue;
            }
            definitions.push(ToolDefinition {
                name: name.clone(),
                description: format!(
                    "[Source: {}] {tool} on the {} toolkit '{}'",
                    source.name, source.kind, source.name
                ),
                parameters: if schema.is_object() {
                    schema.clone()
                } else {
                    json!({"type": "object"})
                },
            });
            routes.push((
                name,
                Offered::Source {
                    toolkit_id: source.toolkit_id.clone(),
                    tool: tool.clone(),
                },
            ));
        }
    }
    (definitions, routes)
}

fn preview(text: &str) -> String {
    text.chars().take(500).collect()
}

/// `_extract_citations_from_answer`: `Source: name - path` lines and
/// backticked identifiers, deduplicated, at most 20.
#[must_use]
pub fn citations_in(answer: &str) -> Vec<Value> {
    let mut seen = BTreeSet::new();
    let mut citations = Vec::new();
    let mut push = |citation: Value| {
        let key = citation.to_string();
        if seen.insert(key) && citations.len() < 20 {
            citations.push(citation);
        }
    };
    for (index, _) in answer.match_indices("Source:") {
        let rest = answer[index + "Source:".len()..].trim_start();
        let toolkit: String = rest
            .chars()
            .take_while(|c| c.is_alphanumeric() || *c == '_')
            .collect();
        if toolkit.is_empty() {
            continue;
        }
        let after = rest[toolkit.len()..].trim_start();
        let path = after
            .strip_prefix('-')
            .or_else(|| after.strip_prefix(':'))
            .map(|tail| {
                tail.trim_start()
                    .split('\n')
                    .next()
                    .unwrap_or_default()
                    .to_owned()
            })
            .filter(|path| !path.is_empty());
        push(json!({"source_toolkit": toolkit, "file_path": path}));
    }
    let mut rest = answer;
    while let Some(start) = rest.find('`') {
        let tail = &rest[start + 1..];
        let Some(end) = tail.find('`') else {
            break;
        };
        let name = &tail[..end];
        if name.chars().count() > 2 && !name.contains('\n') {
            push(json!({"entity_name": name}));
        }
        rest = &tail[end + 1..];
    }
    citations
}

/// Run one investigation.
///
/// # Errors
///
/// Only a stop; a failed model call or tool ends the investigation with
/// its `error` set, as Python reported it.
pub async fn investigate(
    question: &Question,
    view: &GraphView,
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
    let max_rounds = investigate_asset()["max_iterations"].as_u64().unwrap_or(50);
    let (definitions, routes) = offered_tools(view, embed.is_some(), sources);
    let system = SystemPrompt::text(system);
    let mut messages = vec![Msg::User(question.text.clone())];
    let mut result = Investigation::default();
    for _ in 0..max_rounds {
        if stop.is_requested() {
            return Err(EngineError::cancelled());
        }
        let response = match next_reply(model, &system, &definitions, &mut messages).await {
            Ok(response) => response,
            Err(error) if error == EngineError::cancelled() => return Err(error),
            Err(error) => {
                result.error = Some(error.message);
                return Ok(result);
            }
        };
        if let Some(usage) = &response.usage {
            result.tokens_in += usage.prompt_tokens;
            result.tokens_out += usage.completion_tokens;
        }
        if response.tool_calls.is_empty() {
            result.citations = citations_in(&response.content);
            result.answer = response.content;
            return Ok(result);
        }
        messages.push(Msg::Ai {
            content: response.content.clone(),
            calls: response
                .tool_calls
                .iter()
                .map(elitea_conversation::Call::from_tool_call)
                .collect(),
            total_tokens: response.usage.map(|usage| usage.total_tokens),
        });
        for tool_call in &response.tool_calls {
            let arguments = tool_call.parsed_arguments();
            let output = match &arguments {
                Err(error) => format!("Error: {}", error.message),
                Ok(arguments) => match routes.iter().find(|(name, _)| *name == tool_call.name) {
                    None => format!("Error: unknown tool '{}'", tool_call.name),
                    Some((_, Offered::Graph(tool, family))) => {
                        graph_call(view, tool, family, arguments)
                    }
                    Some((_, Offered::Local(local))) => {
                        match local_call(view, *local, arguments, embed).await {
                            Ok(text) => text,
                            Err(error) if error == EngineError::cancelled() => return Err(error),
                            Err(error) => format!("Error: {}", error.message),
                        }
                    }
                    Some((_, Offered::Source { toolkit_id, tool })) => {
                        match call_source(toolkit_id.clone(), tool.clone(), arguments.clone()).await
                        {
                            Ok(text) => text,
                            Err(error) if error == EngineError::cancelled() => return Err(error),
                            Err(error) => format!("Error: {}", error.message),
                        }
                    }
                },
            };
            record(
                &mut result,
                &tool_call.name,
                arguments.as_ref().ok(),
                &output,
            );
            messages.push(Msg::Tool {
                call_id: tool_call.id.clone(),
                name: tool_call.name.clone(),
                content: output,
            });
        }
    }
    LIMIT_MESSAGE.clone_into(&mut result.answer);
    Ok(result)
}

/// One round's reply: the conversation summarised when it is due, then the
/// model called with the system prompt, the conversation and the offered
/// tools (temperature and length as Python set them).
async fn next_reply(
    model: &ChatModel,
    system: &SystemPrompt,
    definitions: &[ToolDefinition],
    messages: &mut Vec<Msg>,
) -> Result<ChatResponse, EngineError> {
    *messages = compact(
        &model.policy,
        DEFAULT_SUMMARY_PROMPT,
        Some(4096),
        std::mem::take(messages),
        |request| (model.chat)(request),
    )
    .await?;
    let mut conversation = vec![ChatMessage::SystemPrompt(system.clone())];
    conversation.extend(messages.iter().map(Msg::to_chat));
    let mut request = ChatRequest::new(conversation);
    request.tools = definitions.to_vec();
    request.sampling = Sampling::Default;
    request.max_tokens = Some(4096);
    (model.chat)(request).await
}

/// Record one call as `{tool, input, output_preview}` (the non-empty
/// arguments, both at most 500 characters).
fn record(
    result: &mut Investigation,
    name: &str,
    arguments: Option<&Map<String, Value>>,
    output: &str,
) {
    let shown: Map<String, Value> = arguments
        .into_iter()
        .flatten()
        .filter(|(key, value)| *key != "__arg1" && !value.is_null() && *value != &json!(""))
        .map(|(k, v)| (k.clone(), v.clone()))
        .collect();
    result.tool_calls.push(json!({
        "tool": name,
        "input": preview(&elitea_engine_core::pyjson::dumps(&Value::Object(shown))),
        "output_preview": preview(output),
    }));
}

/// One of the chat agent's own tools.
async fn local_call(
    view: &GraphView,
    local: Local,
    arguments: &Map<String, Value>,
    embed: Option<&Embed>,
) -> Result<String, EngineError> {
    use retrieval::community_tools as communities;
    let text = |key: &str| {
        arguments
            .get(key)
            .and_then(Value::as_str)
            .unwrap_or_default()
            .to_owned()
    };
    let number = |key: &str| {
        arguments
            .get(key)
            .and_then(Value::as_u64)
            .and_then(|n| usize::try_from(n).ok())
    };
    Ok(match local {
        Local::Pattern => retrieval::pattern::query_pattern(view, &text("pattern")),
        Local::Vocabulary => retrieval::pattern::pattern_vocabulary(view),
        Local::ListCommunities => {
            communities::list_communities(view, number("top_n").filter(|n| *n > 0))
        }
        Local::CommunityDetail => communities::get_community_detail(view, &text("community_id")),
        Local::FindCommunity => communities::find_entity_community(view, &text("entity_name")),
        Local::SearchCommunity => {
            communities::search_within_community(view, &text("community_id"), &text("query"))
        }
        Local::Semantic => {
            let Some(embed) = embed else {
                return Ok("Error: this graph has no embeddings".to_owned());
            };
            let query = text("query");
            let ranking = embed(query.clone()).await?;
            let stamped = retrieval::semantic::stamped_model(view).map(str::to_owned);
            retrieval::semantic::semantic_search_tool(
                view,
                &query,
                &ranking,
                number("top_k").unwrap_or(10),
                None,
                None,
                stamped.as_deref(),
            )?
        }
    })
}

/// A graph tool's text, from the retrieval handler that answers it.
fn graph_call(
    view: &GraphView,
    tool: &str,
    family: &str,
    arguments: &Map<String, Value>,
) -> String {
    let call = Call {
        tool,
        family,
        params: arguments,
        view,
    };
    match retrieval::dispatch(&call) {
        Some(Ok(value)) => match &value["result"] {
            Value::String(text) => text.clone(),
            other => other.to_string(),
        },
        Some(Err(error)) => format!("Error: {}", error.message),
        None => format!("Error: '{tool}' is not available in this engine yet"),
    }
}

/// `_tool_investigate`'s answer: JSON (indented) or the text report.
#[must_use]
pub fn report(result: &Investigation, as_json: bool) -> String {
    let total = result.tokens_in + result.tokens_out;
    if as_json {
        let mut response = json!({
            "answer": result.answer,
            "citations": result.citations,
            "tool_calls": result.tool_calls,
            "tokens_in": result.tokens_in,
            "tokens_out": result.tokens_out,
            "total_tokens": total,
        });
        if let (Some(error), Value::Object(fields)) = (&result.error, &mut response) {
            fields.insert("error".to_owned(), json!(error));
        }
        return elitea_engine_core::pyjson::dumps_indent2(&response);
    }
    let rule = "=".repeat(60);
    let thin = "-".repeat(40);
    let answer = if result.answer.is_empty() {
        "No answer generated"
    } else {
        &result.answer
    };
    let mut lines = vec![
        rule.clone(),
        "INVESTIGATION RESULT".to_owned(),
        rule,
        String::new(),
        answer.to_owned(),
        String::new(),
    ];
    if !result.citations.is_empty() {
        lines.push(thin.clone());
        lines.push("Citations:".to_owned());
        for citation in result.citations.iter().take(10) {
            if let Some(name) = citation["entity_name"].as_str().filter(|n| !n.is_empty()) {
                lines.push(format!("  - {name}"));
            } else if let Some(toolkit) = citation["source_toolkit"]
                .as_str()
                .filter(|t| !t.is_empty())
            {
                lines.push(format!(
                    "  - {toolkit}: {}",
                    citation["file_path"].as_str().unwrap_or_default()
                ));
            }
        }
    }
    if !result.tool_calls.is_empty() {
        lines.push(thin.clone());
        lines.push(format!("Tools used: {}", result.tool_calls.len()));
        for call in result.tool_calls.iter().take(5) {
            lines.push(format!(
                "  - {}",
                call["tool"].as_str().unwrap_or("unknown")
            ));
        }
    }
    lines.push(thin);
    lines.push(format!(
        "Tokens: {total} (in: {}, out: {})",
        result.tokens_in, result.tokens_out
    ));
    if let Some(error) = &result.error {
        lines.push(format!("Error: {error}"));
    }
    lines.join("\n")
}

/// The refusal `_tool_investigate` answered for a call with no question.
#[must_use]
pub fn missing_question() -> String {
    elitea_engine_core::pyjson::dumps(&json!({
        "error": "Missing required parameter: question",
        "usage": "Provide a 'question' parameter with your investigation query"
    }))
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::graph::Graph;
    use elitea_model_client::chat::ToolCall;
    use std::sync::Mutex;

    /// A long investigation is summarised instead of growing past the
    /// model's context: the model asks for a graph search six times, the
    /// conversation crosses the policy's threshold, the old rounds are
    /// replaced by the summary, and the answer still arrives.
    #[tokio::test]
    async fn a_long_investigation_is_summarised() {
        let requests: Arc<Mutex<Vec<ChatRequest>>> = Arc::default();
        let seen = Arc::clone(&requests);
        let chat: Chat = Arc::new(move |request: ChatRequest| {
            let rounds = seen
                .lock()
                .map(|mut all| {
                    all.push(request.clone());
                    all.iter().filter(|r| !r.tools.is_empty()).count()
                })
                .unwrap_or_default();
            Box::pin(async move {
                let mut response = ChatResponse::default();
                if request.tools.is_empty() {
                    "the searches found nothing".clone_into(&mut response.content);
                } else if rounds <= 6 {
                    response.tool_calls.push(ToolCall {
                        id: format!("c{rounds}"),
                        name: "search_knowledge_graph".to_owned(),
                        arguments: format!(r#"{{"query": "refund {rounds}"}}"#),
                    });
                } else {
                    "Nothing about refunds.".clone_into(&mut response.content);
                }
                Ok(response)
            })
        });
        let model = ChatModel {
            chat,
            policy: Policy {
                trigger_tokens: 200,
                keep_tokens: None,
                chars_per_token: 4.0,
                scale: false,
            },
        };
        let call_source: SourceCall = Arc::new(|_, _, _| Box::pin(async { Ok(String::new()) }));
        let question = Question {
            text: "Where are refunds handled?".to_owned(),
            ..Question::default()
        };
        let Ok(result) = investigate(
            &question,
            &GraphView::new(Graph::new(), 1),
            &model,
            &[],
            &call_source,
            None,
            &StopSignal::default(),
        )
        .await
        else {
            panic!("investigates");
        };
        assert_eq!(result.answer, "Nothing about refunds.");
        assert_eq!(result.tool_calls.len(), 6);
        let requests = requests.lock().map(|r| r.clone()).unwrap_or_default();
        let summaries = requests.iter().filter(|r| r.tools.is_empty()).count();
        assert!(summaries >= 1, "the conversation was summarised");
        let last = requests.last().unwrap_or_else(|| panic!("requests"));
        let ChatMessage::User(first) = &last.messages[1] else {
            panic!("a user message after the system prompt");
        };
        assert!(
            first.starts_with("Here is a summary of the conversation to date:"),
            "{first}"
        );
        assert!(last.messages.len() <= 1 + 1 + elitea_conversation::KEEP_MESSAGES);
    }
}

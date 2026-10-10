//! The index's tools as the agent sees them: the `workspace_index`
//! toolset, adk [`Tool`]s like the local tools, handed over by
//! [`IndexToolProvider`].
//!
//! Each tool has the name and the arguments of the cloud Inventory tool
//! (`services/elitea-subapp-host/internal/apps/inventory/descriptor.json`;
//! `tests/descriptor.rs` keeps them equal) and answers through the shared
//! handlers (`retrieval::dispatch`) over the index's current build, so an
//! agent reads the same answers locally and in the cloud. Two differences:
//!
//! * `get_entity_content` reads the cited lines from the folder (through
//!   the confined read), where the cloud answers with the location only;
//! * an answer from a build that may be out of date starts with a one-line
//!   notice ([`IndexService::view`]);
//! * once the index is removed, turned off or reopened under another policy,
//!   the tools a turn already holds answer `index.closed`, and a build made
//!   under another `path_deny` is never answered from (`index.not_ready`
//!   until the rebuild commits).
//!
//! Every tool only reads, so each runs without an approval, in plan mode
//! too, and calls may run concurrently.

use crate::service::{IndexService, IndexState};
use adk_core::{ReadonlyContext, Tool, ToolContext, Toolset};
use async_trait::async_trait;
use elitea_agent_runtime::host::{HostError, ToolProvider, ToolsetRequest};
use elitea_engine_core::pystr;
use elitea_inventory_core::retrieval::query::{SearchFilters, citations_of, ids_named, search};
use elitea_inventory_core::retrieval::view::GraphView;
use elitea_inventory_core::retrieval::{Call, dispatch};
use elitea_local_tools::files::MAX_FILE_BYTES;
use elitea_local_tools::workspace::{Workspace, WsPath};
use serde_json::{Map, Value, json};
use std::fmt::Write as _;
use std::path::Path;
use std::sync::Arc;

/// The toolset's name. Not `local`: the host tells the two apart, and
/// reports both as local (not remote) tool calls.
pub const TOOLSET_NAME: &str = "workspace_index";

/// The most lines `get_entity_content` returns.
const MAX_CONTENT_LINES: usize = 400;
/// The most bytes `get_entity_content` returns.
const MAX_CONTENT_BYTES: usize = 64 * 1024;

/// One index tool: its cloud name, the handler family it is routed to,
/// and its arguments.
#[derive(Debug)]
pub struct IndexToolSpec {
    pub name: &'static str,
    /// `inventory_search` or `inventory`: which cloud toolkit's handler
    /// (and argument schema) it is.
    pub family: &'static str,
    pub description: &'static str,
    pub parameters: fn() -> Value,
}

const ENTITY_REFERENCE: &str = "Entity reference - copy from search results. Supports: 'Name', 'Name (type)', or 'Name (type) @ source - path'";

fn search_knowledge_graph() -> Value {
    json!({
        "type": "object",
        "properties": {
            "query": {"type": "string", "description": "Search query for finding entities (e.g., 'user authentication', 'database connection')"},
            "top_k": {"type": "integer", "description": "Maximum number of results to return", "default": 20},
            "entity_type": {"type": "string", "description": "Filter by entity type (class, function, method, service, etc.)"},
            "layer": {"type": "string", "description": "Filter by layer (code, service, data, documentation, domain)"}
        },
        "required": ["query"]
    })
}

fn get_entity_details() -> Value {
    json!({
        "type": "object",
        "properties": {
            "entity_name": {"type": "string", "description": ENTITY_REFERENCE},
            "include_relations": {"type": "boolean", "description": "Include related entities in the response", "default": true}
        },
        "required": ["entity_name"]
    })
}

fn get_related_entities() -> Value {
    json!({
        "type": "object",
        "properties": {
            "entity_name": {"type": "string", "description": ENTITY_REFERENCE},
            "relation_type": {"type": "string", "description": "Filter by relation type (CALLS, IMPORTS, EXTENDS, IMPLEMENTS, CONTAINS, etc.)"},
            "direction": {"type": "string", "description": "Relation direction: 'outgoing', 'incoming', or 'both'", "default": "both"}
        },
        "required": ["entity_name"]
    })
}

fn query_graph() -> Value {
    json!({
        "type": "object",
        "properties": {
            "query": {"type": "string", "description": "JQL-like query. Syntax: type:class,function layer:code file:*.py name:User related:Entity rel:calls dir:out limit:50"},
            "types": {"type": "string", "description": "Comma-separated entity types (class, function, method, service, etc.)"},
            "layers": {"type": "string", "description": "Comma-separated layers: code, service, data, documentation, domain, product, configuration, testing"},
            "related_to": {"type": "string", "description": "Find entities related to this entity. Copy from search results."},
            "limit": {"type": "integer", "description": "Maximum number of results", "default": 30}
        },
        "required": []
    })
}

fn list_entity_types() -> Value {
    json!({"type": "object", "properties": {}, "required": []})
}

fn impact_analysis() -> Value {
    json!({
        "type": "object",
        "properties": {
            "entity_name": {"type": "string", "description": ENTITY_REFERENCE},
            "direction": {"type": "string", "enum": ["downstream", "upstream"], "description": "'downstream' (what depends on this) or 'upstream' (what this depends on)", "default": "downstream"},
            "max_depth": {"type": "integer", "description": "Maximum traversal depth", "default": 3},
            "output_format": {"type": "string", "enum": ["text", "json"], "description": "Output format: 'text' or 'json'", "default": "text"}
        },
        "required": ["entity_name"]
    })
}

fn get_entity_content() -> Value {
    json!({
        "type": "object",
        "properties": {
            "entity_name": {"type": "string", "description": ENTITY_REFERENCE}
        },
        "required": ["entity_name"]
    })
}

/// Every index tool, in the order they are offered.
pub static TOOLS: [IndexToolSpec; 7] = [
    IndexToolSpec {
        name: "search_knowledge_graph",
        family: "inventory_search",
        description: "Search the workspace's code index for entities (classes, functions, files, configs, docs) matching a query. Returns entities with their types, files and relevance scores.",
        parameters: search_knowledge_graph,
    },
    IndexToolSpec {
        name: "get_entity_details",
        family: "inventory_search",
        description: "Get detailed information about a specific entity of the workspace index, including its properties, description and source citations.",
        parameters: get_entity_details,
    },
    IndexToolSpec {
        name: "get_related_entities",
        family: "inventory_search",
        description: "Get entities related to a specific entity of the workspace index: dependencies, callers, imports and other relationships.",
        parameters: get_related_entities,
    },
    IndexToolSpec {
        name: "query_graph",
        family: "inventory_search",
        description: "Query the workspace index with structured filters using JQL-like syntax. No similarity search - exact filtering by type, layer, file patterns and relationships.",
        parameters: query_graph,
    },
    IndexToolSpec {
        name: "list_entity_types",
        family: "inventory_search",
        description: "List all entity types in the workspace index with their counts. Useful for understanding what is indexed before searching.",
        parameters: list_entity_types,
    },
    IndexToolSpec {
        name: "impact_analysis",
        family: "inventory",
        description: "Analyze which entities of the workspace would be impacted by changing an entity (downstream) or what it depends on (upstream).",
        parameters: impact_analysis,
    },
    IndexToolSpec {
        name: "get_entity_content",
        family: "inventory",
        description: "Read the source lines of an entity of the workspace index, from the file and line range it was found at.",
        parameters: get_entity_content,
    },
];

/// The index tools' names: the names a remote toolkit's tool must not take.
pub const NAMES: [&str; 7] = [
    "search_knowledge_graph",
    "get_entity_details",
    "get_related_entities",
    "query_graph",
    "list_entity_types",
    "impact_analysis",
    "get_entity_content",
];

fn failure(code: &str, message: impl Into<String>) -> Value {
    json!({"status": "error", "code": code, "message": message.into()})
}

/// The answer of `spec` to `args` over `view`, with `notice` first.
fn answer(
    spec: &IndexToolSpec,
    view: &GraphView,
    workspace: &Workspace,
    args: &Value,
    notice: Option<&str>,
) -> Value {
    let params = args.as_object().cloned().unwrap_or_default();
    let text = if spec.name == "get_entity_content"
        && let Some(content) = entity_content(view, workspace, &params)
    {
        Ok(content)
    } else {
        let call = Call {
            tool: spec.name,
            family: spec.family,
            params: &params,
            view,
        };
        match dispatch(&call) {
            Some(Ok(result)) => Ok(match result.get("result") {
                Some(Value::String(text)) => text.clone(),
                Some(other) => other.to_string(),
                None => result.to_string(),
            }),
            Some(Err(error)) => Err(error.message),
            None => Err(format!("{} is not an index tool", spec.name)),
        }
    };
    match text {
        Ok(text) => {
            let result = match notice {
                Some(notice) => format!("{notice}\n\n{text}"),
                None => text,
            };
            json!({"status": "ok", "result": result})
        }
        Err(message) => failure("index.failed", message),
    }
}

/// The entity the cloud's `get_entity_content` resolves `entity_name` to
/// (exact name, then id, then the best search hit), with its location.
fn located(view: &GraphView, name: &str) -> Option<(String, i64, i64)> {
    let id = ids_named(view, name)
        .first()
        .map(|id| (*id).to_owned())
        .or_else(|| {
            let candidate = pystr::strip(name);
            view.node(candidate).map(|_| candidate.to_owned())
        })
        .or_else(|| {
            search(view, name, 1, SearchFilters::default())
                .into_iter()
                .next()
                .map(|hit| hit.id)
        })?;
    let node = view.node(&id)?;
    let citation = node
        .get("citation")
        .filter(|c| c.is_object())
        .or_else(|| citations_of(node).into_iter().next())?;
    let path = citation.get("file_path")?.as_str()?.to_owned();
    let start = citation
        .get("line_start")
        .and_then(Value::as_i64)
        .unwrap_or(0);
    let end = citation
        .get("line_end")
        .and_then(Value::as_i64)
        .unwrap_or(0);
    (!path.is_empty()).then_some((path, start, end))
}

/// `get_entity_content` from the folder: the cloud's location lines, then
/// the cited lines read through the confined read. `None` when the entity
/// or its file cannot be found or read (the cloud's answer is given then).
fn entity_content(
    view: &GraphView,
    workspace: &Workspace,
    params: &Map<String, Value>,
) -> Option<String> {
    let name = params.get("entity_name")?.as_str()?;
    let (path, start, end) = located(view, name)?;
    let file = WsPath::from_relative(Path::new(&path)).ok()?;
    let read = workspace.read(&file, MAX_FILE_BYTES).ok()?;
    let text = std::str::from_utf8(&read.bytes).ok()?;
    let lines: Vec<&str> = text.lines().collect();
    let first = usize::try_from(start.max(1)).ok()?;
    let last = if end >= start && end > 0 {
        usize::try_from(end).ok()?
    } else {
        lines.len()
    }
    .min(lines.len());
    if first > last {
        return None;
    }
    let mut location = path.clone();
    if start > 0 {
        let _ = write!(location, ":{start}");
        if end > 0 {
            let _ = write!(location, "-{end}");
        }
    }
    let mut out = format!("Content of '{name}'\nLocation: {location}\nSource: workspace\n\n```\n");
    let mut shown = 0;
    let mut truncated = false;
    for line in &lines[first - 1..last] {
        if shown == MAX_CONTENT_LINES || out.len() + line.len() > MAX_CONTENT_BYTES {
            truncated = true;
            break;
        }
        out.push_str(line);
        out.push('\n');
        shown += 1;
    }
    out.push_str("```");
    if truncated {
        let _ = write!(out, "\n(cut at {shown} lines; read the file for the rest)");
    }
    Some(out)
}

/// One index tool of one workspace.
pub struct IndexTool {
    spec: &'static IndexToolSpec,
    service: Arc<IndexService>,
}

impl IndexTool {
    /// Run the tool without the agent runtime (the host's tests, a UI
    /// preview).
    pub async fn call(&self, args: Value) -> Value {
        if self.service.is_closed() {
            // Removed, turned off, or reopened under another policy since
            // the turn was offered it: answer nothing from it.
            return failure(
                "index.closed",
                "The workspace index was removed or turned off; use the file tools.",
            );
        }
        let Some((view, notice)) = self.service.view() else {
            let message = if self.service.status().state == IndexState::StalePolicy {
                "The workspace index is being rebuilt under the current file access policy; use the file tools."
            } else {
                "The workspace index has not been built yet; use the file tools."
            };
            return failure("index.not_ready", message);
        };
        let spec = self.spec;
        let workspace = Arc::clone(self.service.workspace());
        // The handlers walk the whole graph: off the runtime's threads.
        tokio::task::spawn_blocking(move || {
            answer(spec, &view, &workspace, &args, notice.as_deref())
        })
        .await
        .unwrap_or_else(|_| failure("index.failed", "the index tool stopped unexpectedly"))
    }
}

#[async_trait]
impl Tool for IndexTool {
    fn name(&self) -> &str {
        self.spec.name
    }

    fn description(&self) -> &str {
        self.spec.description
    }

    fn parameters_schema(&self) -> Option<Value> {
        Some((self.spec.parameters)())
    }

    fn is_read_only(&self) -> bool {
        true
    }

    fn is_concurrency_safe(&self) -> bool {
        true
    }

    async fn execute(&self, _ctx: Arc<dyn ToolContext>, args: Value) -> adk_core::Result<Value> {
        // Failures are results the model reads, not run failures.
        Ok(self.call(args).await)
    }
}

/// The index tools of one workspace.
pub struct IndexToolset {
    service: Arc<IndexService>,
}

impl IndexToolset {
    #[must_use]
    pub fn new(service: Arc<IndexService>) -> Self {
        Self { service }
    }

    /// The tools, whatever the index's state (a tool answers
    /// `index.not_ready` before the first build).
    #[must_use]
    pub fn current_tools(&self) -> Vec<Arc<IndexTool>> {
        TOOLS
            .iter()
            .map(|spec| {
                Arc::new(IndexTool {
                    spec,
                    service: Arc::clone(&self.service),
                })
            })
            .collect()
    }
}

#[async_trait]
impl Toolset for IndexToolset {
    fn name(&self) -> &str {
        TOOLSET_NAME
    }

    async fn tools(&self, _ctx: Arc<dyn ReadonlyContext>) -> adk_core::Result<Vec<Arc<dyn Tool>>> {
        Ok(self
            .current_tools()
            .into_iter()
            .map(|tool| tool as Arc<dyn Tool>)
            .collect())
    }
}

/// The index toolset as a runtime [`ToolProvider`], for a host to chain
/// after its local tools.
pub struct IndexToolProvider {
    service: Arc<IndexService>,
}

impl IndexToolProvider {
    #[must_use]
    pub fn new(service: Arc<IndexService>) -> Self {
        Self { service }
    }
}

#[async_trait]
impl ToolProvider for IndexToolProvider {
    async fn toolsets(
        &self,
        _request: &ToolsetRequest,
    ) -> Result<Vec<Arc<dyn Toolset>>, HostError> {
        Ok(vec![Arc::new(IndexToolset::new(Arc::clone(&self.service)))])
    }
}

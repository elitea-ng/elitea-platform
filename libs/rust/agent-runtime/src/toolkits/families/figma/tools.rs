use std::collections::BTreeSet;
use std::fmt;
use std::sync::Arc;

use adk_core::{AdkError, ErrorCategory, ErrorComponent, Tool, ToolContext};
use adk_tool::BasicToolset;
use async_trait::async_trait;
use futures::StreamExt as _;
use reqwest::Method;
use serde_json::{Map, Value, json};

use crate::toolkits::invocation::{MaterializedToolsetError, admit_materialized_toolset};
use crate::toolkits::policy::ToolAdmissionPolicy;

use super::client::{FigmaClient, FigmaClientError};
use super::config::{FigmaConfigError, FigmaToolkitConfig};
use super::output::{controls, render};
use super::tokens::{BatchResult, batch_output, design_tokens};

const GET_FILE_NODES: &str = "get_file_nodes";
const GET_FILE: &str = "get_file";
const GET_FILE_VERSIONS: &str = "get_file_versions";
const GET_FILE_COMMENTS: &str = "get_file_comments";
const POST_FILE_COMMENT: &str = "post_file_comment";
const GET_FILE_IMAGES: &str = "get_file_images";
const GET_TEAM_PROJECTS: &str = "get_team_projects";
const GET_PROJECT_FILES: &str = "get_project_files";
const EXTRACT_DESIGN_TOKENS: &str = "extract_design_tokens";
const EXTRACT_DESIGN_TOKENS_BATCH: &str = "extract_design_tokens_batch";

/// SDK tools this family does not serve: the LLM/TOON analyzer and the
/// inherited indexing tools.
const SDK_ONLY_TOOLS: [&str; 7] = [
    "analyze_file",
    "index_data",
    "list_indexes",
    "remove_index",
    "search_index",
    "stepback_search_index",
    "stepback_summary_index",
];

const MAX_KEY_BYTES: usize = 256;
const MAX_IDS_BYTES: usize = 8 * 1_024;
const MAX_MESSAGE_BYTES: usize = 64 * 1_024;
const MAX_ARGUMENT_BYTES: usize = 256 * 1_024;
const MAX_DESCRIPTION_BYTES: usize = 1_000;
const MAX_BATCH_ENTRIES: usize = 100;
/// The SDK's `ThreadPoolExecutor(max_workers=min(5, n))`.
const MAX_BATCH_PARALLELISM: usize = 5;
const DEFAULT_TOKEN_DEPTH: u64 = 4;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum FigmaToolsetErrorCode {
    InvalidConfiguration,
    ResourceExhausted,
    UnsupportedSelection,
    Client,
    InvalidDefinition,
}

/// Stable construction failure for the Figma family.
pub(crate) struct FigmaToolsetError {
    code: FigmaToolsetErrorCode,
}

impl FigmaToolsetError {
    #[must_use]
    pub(crate) const fn code(&self) -> FigmaToolsetErrorCode {
        self.code
    }
}

impl fmt::Debug for FigmaToolsetError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("FigmaToolsetError")
            .field("code", &self.code)
            .finish_non_exhaustive()
    }
}

impl fmt::Display for FigmaToolsetError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(match self.code {
            FigmaToolsetErrorCode::InvalidConfiguration => {
                "the Figma toolkit configuration is invalid"
            }
            FigmaToolsetErrorCode::ResourceExhausted => {
                "the Figma toolkit configuration exceeds its approved limit"
            }
            FigmaToolsetErrorCode::UnsupportedSelection => {
                "the selected Figma tool profile is not supported"
            }
            FigmaToolsetErrorCode::Client => "the Figma client could not be created",
            FigmaToolsetErrorCode::InvalidDefinition => "the Figma ADK tool definition is invalid",
        })
    }
}

impl std::error::Error for FigmaToolsetError {}

impl From<FigmaConfigError> for FigmaToolsetError {
    fn from(source: FigmaConfigError) -> Self {
        Self {
            code: match source.code() {
                super::config::FigmaConfigErrorCode::InvalidConfiguration => {
                    FigmaToolsetErrorCode::InvalidConfiguration
                }
                super::config::FigmaConfigErrorCode::ResourceExhausted => {
                    FigmaToolsetErrorCode::ResourceExhausted
                }
            },
        }
    }
}

impl From<FigmaClientError> for FigmaToolsetError {
    fn from(_: FigmaClientError) -> Self {
        Self {
            code: FigmaToolsetErrorCode::Client,
        }
    }
}

impl From<MaterializedToolsetError> for FigmaToolsetError {
    fn from(_: MaterializedToolsetError) -> Self {
        Self {
            code: FigmaToolsetErrorCode::InvalidDefinition,
        }
    }
}

/// Build the ten served Figma tools.
///
/// An empty selection serves all of them. An explicit selection keeps the
/// served names and omits `analyze_file` and the indexing tools with one
/// bounded warning; a name the SDK does not declare is refused.
pub(crate) fn build_figma_toolset(
    toolkit_name: &str,
    config: FigmaToolkitConfig,
    policy: &Arc<ToolAdmissionPolicy>,
) -> Result<BasicToolset, FigmaToolsetError> {
    let selected = served_selection(config.selected_tools())?;
    if !config.selected_tools().is_empty() && selected.len() < config.selected_tools().len() {
        tracing::warn!(
            event = "agent_toolkit_tools_skipped",
            reason_code = "unsupported_tool_selection",
            toolkit_type = "figma",
            toolkit_name,
            selected_count = config.selected_tools().len(),
            materialized_count = selected.len(),
            "Figma analysis and indexing tools are not served by this runtime and were omitted"
        );
    }
    let client = Arc::new(FigmaClient::new(config)?);
    build_with_client(toolkit_name, &selected, policy, &client)
}

fn served_selection(selected: &[Box<str>]) -> Result<Vec<String>, FigmaToolsetError> {
    if selected.is_empty() {
        return Ok(Vec::new());
    }
    let mut served = Vec::with_capacity(selected.len());
    for name in selected {
        if FigmaToolKind::from_name(name).is_some() {
            served.push(name.to_string());
        } else if !SDK_ONLY_TOOLS.contains(&name.as_ref()) {
            return Err(FigmaToolsetError {
                code: FigmaToolsetErrorCode::UnsupportedSelection,
            });
        }
    }
    if served.is_empty() {
        return Err(FigmaToolsetError {
            code: FigmaToolsetErrorCode::UnsupportedSelection,
        });
    }
    Ok(served)
}

fn build_with_client(
    toolkit_name: &str,
    selected: &[String],
    policy: &Arc<ToolAdmissionPolicy>,
    client: &Arc<FigmaClient>,
) -> Result<BasicToolset, FigmaToolsetError> {
    let include_all = selected.is_empty();
    let mut tools: Vec<Arc<dyn Tool>> = Vec::with_capacity(FigmaToolKind::ALL.len());
    for kind in FigmaToolKind::ALL {
        if include_all || selected.iter().any(|name| name == kind.name()) {
            tools.push(Arc::new(FigmaTool::new(
                kind,
                toolkit_name,
                Arc::clone(client),
            )));
        }
    }
    admit_materialized_toolset(toolkit_name, "figma", policy, tools).map_err(Into::into)
}

#[cfg(test)]
pub(in crate::toolkits) fn test_build_with_client(
    toolkit_name: &str,
    selected: &[String],
    policy: &Arc<ToolAdmissionPolicy>,
    client: FigmaClient,
) -> Result<BasicToolset, FigmaToolsetError> {
    build_with_client(toolkit_name, selected, policy, &Arc::new(client))
}

#[cfg(test)]
pub(in crate::toolkits) fn test_served_selection(
    selected: &[Box<str>],
) -> Result<Vec<String>, FigmaToolsetError> {
    served_selection(selected)
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
enum FigmaToolKind {
    GetFileNodes,
    GetFile,
    GetFileVersions,
    GetFileComments,
    PostFileComment,
    GetFileImages,
    GetTeamProjects,
    GetProjectFiles,
    ExtractDesignTokens,
    ExtractDesignTokensBatch,
}

impl FigmaToolKind {
    /// The SDK's `get_available_tools` order, without `analyze_file`.
    const ALL: [Self; 10] = [
        Self::GetFileNodes,
        Self::GetFile,
        Self::GetFileVersions,
        Self::GetFileComments,
        Self::PostFileComment,
        Self::GetFileImages,
        Self::GetTeamProjects,
        Self::GetProjectFiles,
        Self::ExtractDesignTokens,
        Self::ExtractDesignTokensBatch,
    ];

    const fn name(self) -> &'static str {
        match self {
            Self::GetFileNodes => GET_FILE_NODES,
            Self::GetFile => GET_FILE,
            Self::GetFileVersions => GET_FILE_VERSIONS,
            Self::GetFileComments => GET_FILE_COMMENTS,
            Self::PostFileComment => POST_FILE_COMMENT,
            Self::GetFileImages => GET_FILE_IMAGES,
            Self::GetTeamProjects => GET_TEAM_PROJECTS,
            Self::GetProjectFiles => GET_PROJECT_FILES,
            Self::ExtractDesignTokens => EXTRACT_DESIGN_TOKENS,
            Self::ExtractDesignTokensBatch => EXTRACT_DESIGN_TOKENS_BATCH,
        }
    }

    fn from_name(name: &str) -> Option<Self> {
        Self::ALL.into_iter().find(|kind| kind.name() == name)
    }

    const fn is_read_only(self) -> bool {
        !matches!(self, Self::PostFileComment)
    }

    /// The SDK docstrings (the two token docstrings condensed to what the
    /// SDK's 1,000-character cut kept).
    const fn sdk_description(self) -> &'static str {
        match self {
            Self::GetFileNodes => "Reads a specified file nodes by field key from Figma.",
            Self::GetFile => "Reads a specified file by field key from Figma.",
            Self::GetFileVersions => {
                "Retrieves the version history of a specified file from Figma."
            }
            Self::GetFileComments => "Retrieves comments on a specified file from Figma.",
            Self::PostFileComment => "Posts a comment to a specific file in Figma.",
            Self::GetFileImages => {
                "Fetches URLs for server-rendered images from a Figma file based on node IDs."
            }
            Self::GetTeamProjects => "Retrieves all projects for a specified team ID from Figma.",
            Self::GetProjectFiles => "Retrieves all files for a specified project ID from Figma.",
            Self::ExtractDesignTokens => {
                "Extract deduplicated design tokens (colors, typography, effects, strokes) from a Figma node and its entire subtree.\n\nFetches the node tree at the requested depth and recursively collects all style properties — fills, strokes, effects, and typography — then deduplicates them across the subtree.\n\nReturns JSON with node_id, node_name, node_type, colors [{hex, alpha, opacity, source_path}], strokes [{hex, alpha, source_path}], typography [{fontFamily, fontSize, fontWeight, ...}], effects [{type, radius, ..., source_path}], summary {total_style_entries, unique_colors, unique_fonts, unique_effects, unique_strokes} and raw_entries.\n\nnode_id must be a FRAME, COMPONENT, COMPONENT_SET or SECTION (hyphens become colons); do NOT pass a PAGE (CANVAS) id. depth is the tree fetch depth (1-8, default 4; 6-8 for deeply nested COMPONENT_SET nodes)."
            }
            Self::ExtractDesignTokensBatch => {
                "Extract deduplicated design tokens from multiple Figma nodes in batch.\n\nEach entry must contain file_key and node_id, with an optional depth (1-8, default 4). Entries can run in parallel.\n\nOutput formats: 'full' (colors, strokes, typography and effects lists plus summary per entry), 'compact' (counts only per entry, recommended for LLM use), 'summary' (batch summary only), 'style_guide' (normalized design tokens plus per-component token references, for style guide generation). Every format includes batch_summary {total_entries, processed_entries, successful, failed, total_colors, total_strokes, total_fonts, total_effects}; a failed entry is reported with status 'error' and its error."
            }
        }
    }
}

struct FigmaTool {
    kind: FigmaToolKind,
    client: Arc<FigmaClient>,
    description: Box<str>,
}

impl FigmaTool {
    fn new(kind: FigmaToolKind, toolkit_name: &str, client: Arc<FigmaClient>) -> Self {
        let description = format!("Toolkit: {toolkit_name}\n{}", kind.sdk_description());
        Self {
            kind,
            client,
            description: description
                .chars()
                .take(MAX_DESCRIPTION_BYTES)
                .collect::<String>()
                .into_boxed_str(),
        }
    }
}

#[async_trait]
impl Tool for FigmaTool {
    fn name(&self) -> &str {
        self.kind.name()
    }

    fn description(&self) -> &str {
        &self.description
    }

    fn is_read_only(&self) -> bool {
        self.kind.is_read_only()
    }

    fn is_concurrency_safe(&self) -> bool {
        self.kind.is_read_only()
    }

    fn parameters_schema(&self) -> Option<Value> {
        Some(schema(self.kind))
    }

    async fn execute(
        &self,
        _context: Arc<dyn ToolContext>,
        arguments: Value,
    ) -> adk_core::Result<Value> {
        validate_argument_size(&arguments)?;
        let arguments = arguments.as_object().ok_or_else(invalid_arguments)?;
        let allowed = schema(self.kind)
            .get("properties")
            .and_then(Value::as_object)
            .map(|properties| properties.keys().cloned().collect::<BTreeSet<_>>())
            .unwrap_or_default();
        if arguments.keys().any(|key| !allowed.contains(key)) {
            return Err(invalid_arguments());
        }
        match self.kind {
            FigmaToolKind::ExtractDesignTokens => {
                extract_design_tokens(&self.client, arguments).await
            }
            FigmaToolKind::ExtractDesignTokensBatch => {
                extract_design_tokens_batch(&self.client, arguments).await
            }
            kind => rest_tool(&self.client, kind, arguments).await,
        }
    }
}

/// `FigmaPy` appends `geometry`, `version`, `scale` and `format` to the query
/// verbatim (`?paths`), so a bare value never reached Figma as a parameter.
/// A `key=value` argument is kept as that pair; a bare value is sent under
/// its own parameter name.
fn query_pair<'a>(name: &'a str, value: &'a str) -> (&'a str, &'a str) {
    value.split_once('=').unwrap_or((name, value))
}

#[expect(
    clippy::too_many_lines,
    reason = "the eight SDK REST tools, one arm each"
)]
async fn rest_tool(
    client: &FigmaClient,
    kind: FigmaToolKind,
    arguments: &Map<String, Value>,
) -> adk_core::Result<Value> {
    let extra = match arguments.get("extra_params") {
        None | Some(Value::Null) => None,
        Some(Value::Object(extra)) => Some(extra),
        Some(_) => return Err(invalid_arguments()),
    };
    let controls = match controls(extra, client.config()) {
        Ok(controls) => controls,
        Err(message) => return Ok(sdk_error(kind, &message)),
    };
    let text = |name: &str, limit: usize| optional_text(arguments, name, limit);
    let result = match kind {
        FigmaToolKind::GetFileNodes => {
            let file_key = required_text(arguments, "file_key", MAX_KEY_BYTES)?;
            let ids = required_text(arguments, "ids", MAX_IDS_BYTES)?;
            client
                .request(
                    Method::GET,
                    &["files", file_key, "nodes"],
                    &[("ids", ids)],
                    None,
                    false,
                )
                .await
        }
        FigmaToolKind::GetFile => {
            let file_key = required_text(arguments, "file_key", MAX_KEY_BYTES)?;
            let mut query = Vec::new();
            if let Some(geometry) = text("geometry", MAX_KEY_BYTES)? {
                query.push(query_pair("geometry", geometry));
            }
            if let Some(version) = text("version", MAX_KEY_BYTES)? {
                query.push(query_pair("version", version));
            }
            client
                .request(Method::GET, &["files", file_key], &query, None, false)
                .await
                .map(|data| {
                    project(
                        &data,
                        &[
                            ("name", "name"),
                            ("last_modified", "lastModified"),
                            ("thumbnail_url", "thumbnailUrl"),
                            ("document", "document"),
                            ("components", "components"),
                            ("schema_version", "schemaVersion"),
                            ("styles", "styles"),
                        ],
                    )
                })
        }
        FigmaToolKind::GetFileVersions => {
            let file_key = required_text(arguments, "file_key", MAX_KEY_BYTES)?;
            client
                .request(
                    Method::GET,
                    &["files", file_key, "versions"],
                    &[],
                    None,
                    false,
                )
                .await
                .map(|data| {
                    project(
                        &data,
                        &[("versions", "versions"), ("pagination", "pagination")],
                    )
                })
        }
        FigmaToolKind::GetFileComments => {
            let file_key = required_text(arguments, "file_key", MAX_KEY_BYTES)?;
            client
                .request(
                    Method::GET,
                    &["files", file_key, "comments"],
                    &[],
                    None,
                    false,
                )
                .await
                .map(|data| comments(&data))
        }
        FigmaToolKind::PostFileComment => {
            let file_key = required_text(arguments, "file_key", MAX_KEY_BYTES)?;
            let message = required_text(arguments, "message", MAX_MESSAGE_BYTES)?;
            let mut body = Map::new();
            body.insert("message".to_owned(), json!(message));
            match arguments.get("client_meta") {
                None | Some(Value::Null) => {}
                Some(meta @ Value::Object(_)) => {
                    body.insert("client_meta".to_owned(), meta.clone());
                }
                Some(_) => return Err(invalid_arguments()),
            }
            client
                .request(
                    Method::POST,
                    &["files", file_key, "comments"],
                    &[],
                    Some(&Value::Object(body)),
                    true,
                )
                .await
        }
        FigmaToolKind::GetFileImages => {
            let file_key = required_text(arguments, "file_key", MAX_KEY_BYTES)?;
            let ids = required_text(arguments, "ids", MAX_IDS_BYTES)?;
            let mut query = vec![("ids", ids)];
            for name in ["scale", "format", "version"] {
                if let Some(value) = text(name, MAX_KEY_BYTES)? {
                    query.push(query_pair(name, value));
                }
            }
            client
                .request(Method::GET, &["images", file_key], &query, None, false)
                .await
                .map(|data| project(&data, &[("err", "err"), ("images", "images")]))
        }
        FigmaToolKind::GetTeamProjects => {
            let team_id = required_text(arguments, "team_id", MAX_KEY_BYTES)?;
            client
                .request(
                    Method::GET,
                    &["teams", team_id, "projects"],
                    &[],
                    None,
                    false,
                )
                .await
                .map(|data| project(&data, &[("projects", "projects")]))
        }
        FigmaToolKind::GetProjectFiles => {
            let project_id = required_text(arguments, "project_id", MAX_KEY_BYTES)?;
            client
                .request(
                    Method::GET,
                    &["projects", project_id, "files"],
                    &[],
                    None,
                    false,
                )
                .await
                .map(|data| project(&data, &[("files", "files")]))
        }
        FigmaToolKind::ExtractDesignTokens | FigmaToolKind::ExtractDesignTokensBatch => {
            return Err(invalid_arguments());
        }
    }
    .map_err(FigmaClientError::into_adk)?;
    Ok(match render(&result, &controls) {
        Ok(text) => Value::String(text),
        Err(message) => sdk_error(kind, &message),
    })
}

/// The SDK wrapper's caught-exception answer.
fn sdk_error(kind: FigmaToolKind, message: &str) -> Value {
    Value::String(format!("Error in '{}': {message}", kind.name()))
}

/// The `FigmaPy` model object's attributes, renamed as `FigmaPy` names them.
fn project(data: &Value, fields: &[(&str, &str)]) -> Value {
    Value::Object(
        fields
            .iter()
            .map(|(attribute, key)| {
                (
                    (*attribute).to_owned(),
                    data.get(*key).cloned().unwrap_or(Value::Null),
                )
            })
            .collect(),
    )
}

/// `FigmaPy`'s `Comments`: each comment rebuilt from its nine attributes.
fn comments(data: &Value) -> Value {
    let comments = data
        .get("comments")
        .and_then(Value::as_array)
        .map(Vec::as_slice)
        .unwrap_or_default()
        .iter()
        .map(|comment| {
            project(
                comment,
                &[
                    ("id", "id"),
                    ("file_key", "file_key"),
                    ("parent_id", "parent_id"),
                    ("user", "user"),
                    ("created_at", "created_at"),
                    ("resolved_at", "resolved_at"),
                    ("message", "message"),
                    ("client_meta", "client_meta"),
                    ("order_id", "order_id"),
                ],
            )
        })
        .collect::<Vec<_>>();
    json!({"comments":comments})
}

/// One `extract_design_tokens` call: the tokens, or the SDK's message for a
/// node that is null (a PAGE id) or a fetch that failed.
async fn tokens_for(
    client: &FigmaClient,
    file_key: &str,
    node_id: &str,
    depth: u64,
) -> Result<Value, TokenFailure> {
    let node_id = node_id.replace('-', ":");
    let depth = depth.to_string();
    let raw = client
        .request(
            Method::GET,
            &["files", file_key, "nodes"],
            &[("ids", &node_id), ("depth", &depth)],
            None,
            false,
        )
        .await
        .map_err(|error| TokenFailure::Client {
            message: format!("Failed to fetch node '{node_id}' from file '{file_key}': {error}"),
            error,
        })?;
    let node = raw
        .get("nodes")
        .and_then(|nodes| nodes.get(&node_id))
        .filter(|node| !node.is_null());
    let Some(node) = node else {
        return Err(TokenFailure::Message(format!(
            "Node '{node_id}' returned null. This usually means the ID belongs to a PAGE (CANVAS) node. Please provide a FRAME, COMPONENT, COMPONENT_SET, or SECTION node ID. Use get_file_structure_toon to discover valid child frame IDs."
        )));
    };
    let document = node
        .get("document")
        .filter(|document| !document.is_null())
        .cloned()
        .unwrap_or_else(|| Value::Object(Map::new()));
    Ok(design_tokens(&node_id, &document))
}

enum TokenFailure {
    Message(String),
    Client {
        message: String,
        error: FigmaClientError,
    },
}

impl TokenFailure {
    fn message(&self) -> &str {
        match self {
            Self::Message(message) | Self::Client { message, .. } => message,
        }
    }
}

fn token_depth(value: Option<&Value>) -> Option<u64> {
    match value {
        None | Some(Value::Null) => Some(DEFAULT_TOKEN_DEPTH),
        Some(value) => value.as_u64().filter(|depth| (1..=8).contains(depth)),
    }
}

async fn extract_design_tokens(
    client: &FigmaClient,
    arguments: &Map<String, Value>,
) -> adk_core::Result<Value> {
    let file_key = required_text(arguments, "file_key", MAX_KEY_BYTES)?;
    let node_id = required_text(arguments, "node_id", MAX_KEY_BYTES)?;
    let depth = token_depth(arguments.get("depth")).ok_or_else(invalid_arguments)?;
    match tokens_for(client, file_key, node_id, depth).await {
        Ok(tokens) => Ok(tokens),
        Err(TokenFailure::Message(message)) => Ok(Value::String(message)),
        Err(TokenFailure::Client { error, .. }) => Err(error.into_adk()),
    }
}

struct BatchEntry {
    index: usize,
    file_key: String,
    node_id: String,
    depth: u64,
}

#[expect(
    clippy::too_many_lines,
    reason = "the SDK's validation, fan-out and format steps in source order"
)]
async fn extract_design_tokens_batch(
    client: &FigmaClient,
    arguments: &Map<String, Value>,
) -> adk_core::Result<Value> {
    let output_format = match arguments.get("output_format") {
        None | Some(Value::Null) => "full",
        Some(Value::String(format)) => format.as_str(),
        Some(_) => return Err(invalid_arguments()),
    };
    let parallel = match arguments.get("parallel") {
        None | Some(Value::Null) => true,
        Some(Value::Bool(parallel)) => *parallel,
        Some(_) => return Err(invalid_arguments()),
    };
    let Some(Value::Array(entries)) = arguments.get("entries") else {
        return Err(invalid_arguments());
    };
    if entries.is_empty() {
        return Ok(json!("Batch entries list cannot be empty"));
    }
    if entries.len() > MAX_BATCH_ENTRIES {
        return Ok(Value::String(format!(
            "Batch entries list exceeds the {MAX_BATCH_ENTRIES}-entry limit"
        )));
    }
    let mut validated = Vec::with_capacity(entries.len());
    for (index, entry) in entries.iter().enumerate() {
        let Some(object) = entry.as_object() else {
            return Ok(Value::String(format!("Entry {index} is not a dict")));
        };
        let node_id = object.get("node_id");
        if node_id.is_none_or(Value::is_null) {
            continue;
        }
        let file_key = object
            .get("file_key")
            .and_then(Value::as_str)
            .filter(|key| !key.is_empty());
        let node_id = node_id.and_then(Value::as_str).filter(|id| !id.is_empty());
        let (Some(file_key), Some(node_id)) = (file_key, node_id) else {
            let keys = object
                .keys()
                .map(|key| format!("'{key}'"))
                .collect::<Vec<_>>()
                .join(", ");
            return Ok(Value::String(format!(
                "Entry {index} missing required fields. Each entry must have 'file_key' and 'node_id'. Got: [{keys}]"
            )));
        };
        if file_key.len() > MAX_KEY_BYTES || node_id.len() > MAX_KEY_BYTES {
            return Err(invalid_arguments());
        }
        validated.push(BatchEntry {
            index,
            file_key: file_key.to_owned(),
            node_id: node_id.to_owned(),
            depth: object
                .get("depth")
                .and_then(Value::as_u64)
                .filter(|depth| (1..=8).contains(depth))
                .unwrap_or(DEFAULT_TOKEN_DEPTH),
        });
    }
    if validated.is_empty() {
        return Ok(json!(
            "No valid entries after filtering (all had None node_id)"
        ));
    }
    let process = |entry: BatchEntry| async move {
        let value = match tokens_for(client, &entry.file_key, &entry.node_id, entry.depth).await {
            Ok(tokens) => {
                let field = |key: &str, default: Value| tokens.get(key).cloned().unwrap_or(default);
                json!({
                    "entry_index":entry.index,
                    "file_key":entry.file_key,
                    "node_id":entry.node_id,
                    "node_name":field("node_name", json!("")),
                    "node_type":field("node_type", json!("")),
                    "status":"success",
                    "colors":field("colors", json!([])),
                    "strokes":field("strokes", json!([])),
                    "typography":field("typography", json!([])),
                    "effects":field("effects", json!([])),
                    "summary":field("summary", json!({})),
                })
            }
            Err(failure) => json!({
                "entry_index":entry.index,
                "file_key":entry.file_key,
                "node_id":entry.node_id,
                "status":"error",
                "error":failure.message(),
            }),
        };
        BatchResult {
            index: entry.index,
            value,
        }
    };
    let width = if parallel && validated.len() > 1 {
        MAX_BATCH_PARALLELISM
    } else {
        1
    };
    let results = futures::stream::iter(validated)
        .map(process)
        .buffer_unordered(width)
        .collect::<Vec<_>>()
        .await;
    Ok(batch_output(entries.len(), results, output_format))
}

// ---------------------------------------------------------------------------
// Schemas
// ---------------------------------------------------------------------------

fn extra_params_property() -> Value {
    json!({
        "anyOf":[
            {"type":"object","maxProperties":16,"additionalProperties":{"anyOf":[
                {"type":"string","maxLength":4096},
                {"type":"integer"},
                {"type":"array","items":{},"maxItems":256},
                {"type":"null"}
            ]}},
            {"type":"null"}
        ],
        "default":null,
        "description":"Optional output controls: `limit` (max characters, always applied), `regexp` (regex cleanup on text), `fields_retain`/`fields_remove` (which keys to keep or drop), and `depth_start`/`depth_end` (depth range where that key filtering is applied). Field/depth filters are only used when the serialized JSON result exceeds `limit` to reduce its size."
    })
}

fn string_property(description: &str, max: usize) -> Value {
    json!({"type":"string","minLength":1,"maxLength":max,"description":description})
}

fn nullable_string(description: &str, max: usize) -> Value {
    json!({"type":["string","null"],"maxLength":max,"default":null,"description":description})
}

fn object_schema(mut properties: Map<String, Value>, required: &[&str], extra: bool) -> Value {
    if extra {
        properties.insert("extra_params".to_owned(), extra_params_property());
    }
    let mut schema = Map::new();
    schema.insert("type".to_owned(), json!("object"));
    schema.insert("properties".to_owned(), Value::Object(properties));
    schema.insert("required".to_owned(), json!(required));
    schema.insert("additionalProperties".to_owned(), json!(false));
    Value::Object(schema)
}

fn properties(entries: &[(&str, Value)]) -> Map<String, Value> {
    entries
        .iter()
        .map(|(name, value)| ((*name).to_owned(), value.clone()))
        .collect()
}

fn file_key_property() -> Value {
    string_property("Specifies file key id.", MAX_KEY_BYTES)
}

#[expect(
    clippy::too_many_lines,
    reason = "one schema per SDK tool, kept side by side"
)]
fn schema(kind: FigmaToolKind) -> Value {
    match kind {
        FigmaToolKind::GetFileNodes => object_schema(
            properties(&[
                ("file_key", file_key_property()),
                (
                    "ids",
                    string_property(
                        "Specifies id of file nodes separated by comma, for example `8:6,1:7`",
                        MAX_IDS_BYTES,
                    ),
                ),
            ]),
            &["file_key", "ids"],
            true,
        ),
        FigmaToolKind::GetFile => object_schema(
            properties(&[
                ("file_key", file_key_property()),
                (
                    "geometry",
                    nullable_string("Sets to 'paths' to export vector data", MAX_KEY_BYTES),
                ),
                (
                    "version",
                    nullable_string("Sets version of file", MAX_KEY_BYTES),
                ),
            ]),
            &["file_key"],
            true,
        ),
        FigmaToolKind::GetFileVersions | FigmaToolKind::GetFileComments => object_schema(
            properties(&[("file_key", file_key_property())]),
            &["file_key"],
            true,
        ),
        FigmaToolKind::PostFileComment => object_schema(
            properties(&[
                ("file_key", file_key_property()),
                (
                    "message",
                    string_property("Message for the comment.", MAX_MESSAGE_BYTES),
                ),
                (
                    "client_meta",
                    json!({"anyOf":[{"type":"object","additionalProperties":true},{"type":"null"}],"default":null,"description":"Positioning information of the comment (Vector, FrameOffset, Region, FrameOffsetRegion)"}),
                ),
            ]),
            &["file_key", "message"],
            true,
        ),
        FigmaToolKind::GetFileImages => object_schema(
            properties(&[
                ("file_key", file_key_property()),
                (
                    "ids",
                    string_property(
                        "Specifies id of file images separated by comma, for example `8:6,1:7`",
                        MAX_IDS_BYTES,
                    ),
                ),
                (
                    "scale",
                    nullable_string(
                        "A number between 0.01 and 4, the image scaling factor",
                        MAX_KEY_BYTES,
                    ),
                ),
                (
                    "format",
                    nullable_string(
                        "A string enum for the image output format: jpg, png, svg or pdf",
                        MAX_KEY_BYTES,
                    ),
                ),
                (
                    "version",
                    nullable_string("A specific version ID to use", MAX_KEY_BYTES),
                ),
            ]),
            &["file_key", "ids"],
            true,
        ),
        FigmaToolKind::GetTeamProjects => object_schema(
            properties(&[(
                "team_id",
                string_property("ID of the team to list projects from", MAX_KEY_BYTES),
            )]),
            &["team_id"],
            true,
        ),
        FigmaToolKind::GetProjectFiles => object_schema(
            properties(&[(
                "project_id",
                string_property("ID of the project to list files from", MAX_KEY_BYTES),
            )]),
            &["project_id"],
            true,
        ),
        FigmaToolKind::ExtractDesignTokens => object_schema(
            properties(&[
                (
                    "file_key",
                    string_property("Figma file key.", MAX_KEY_BYTES),
                ),
                (
                    "node_id",
                    string_property(
                        "ID of the node to extract design tokens from. Must be a FRAME, COMPONENT, COMPONENT_SET, SECTION, or GROUP — not a PAGE (CANVAS). Hyphens are automatically converted to colons (e.g. '169-14446' → '169:14446').",
                        MAX_KEY_BYTES,
                    ),
                ),
                (
                    "depth",
                    json!({"anyOf":[{"type":"integer","minimum":1,"maximum":8},{"type":"null"}],"default":4,"description":"How deep to traverse the node tree when fetching from Figma. Use 4 for most frames and components (default). Use 6 for COMPONENT_SET nodes with many variants. Valid range: 1–8."}),
                ),
            ]),
            &["file_key", "node_id"],
            false,
        ),
        FigmaToolKind::ExtractDesignTokensBatch => object_schema(
            properties(&[
                (
                    "entries",
                    json!({"type":"array","minItems":1,"maxItems":MAX_BATCH_ENTRIES,"items":{"type":"object","additionalProperties":true},"description":"List of design token extraction requests. Each entry must contain 'file_key' and 'node_id'. Optional 'depth' per entry (defaults to 4). Supports mixed depths across entries. Node IDs with hyphens are auto-converted to colons."}),
                ),
                (
                    "parallel",
                    json!({"type":["boolean","null"],"default":true,"description":"Enable parallel extraction for faster batch processing (default: True)."}),
                ),
                (
                    "output_format",
                    json!({"type":["string","null"],"maxLength":32,"default":"full","description":"Control output verbosity for LLM optimization: 'full' (all data), 'compact' (no full token lists), 'summary' (counts only, fastest), 'style_guide' (detailed tokens + component references). Default: 'full'."}),
                ),
            ]),
            &["entries"],
            false,
        ),
    }
}

// ---------------------------------------------------------------------------
// Argument helpers
// ---------------------------------------------------------------------------

fn required_text<'a>(
    arguments: &'a Map<String, Value>,
    name: &str,
    limit: usize,
) -> Result<&'a str, AdkError> {
    optional_text(arguments, name, limit)?
        .filter(|value| !value.is_empty())
        .ok_or_else(invalid_arguments)
}

fn optional_text<'a>(
    arguments: &'a Map<String, Value>,
    name: &str,
    limit: usize,
) -> Result<Option<&'a str>, AdkError> {
    match arguments.get(name) {
        None | Some(Value::Null) => Ok(None),
        Some(Value::String(value)) if value.len() <= limit && !value.contains('\0') => {
            Ok(Some(value))
        }
        Some(_) => Err(invalid_arguments()),
    }
}

fn validate_argument_size(arguments: &Value) -> Result<(), AdkError> {
    let size = serde_json::to_vec(arguments)
        .map_err(|_| invalid_arguments())?
        .len();
    if size > MAX_ARGUMENT_BYTES {
        return Err(AdkError::new(
            ErrorComponent::Tool,
            ErrorCategory::InvalidInput,
            "figma.arguments.resource_exhausted",
            "the Figma tool arguments exceed the approved limit",
        ));
    }
    Ok(())
}

fn invalid_arguments() -> AdkError {
    AdkError::new(
        ErrorComponent::Tool,
        ErrorCategory::InvalidInput,
        "figma.arguments.invalid",
        "the Figma tool arguments are invalid",
    )
}

//! The SDK's index tools inside a toolkit family (ADR-0030 decision 1, stage
//! I1): `list_indexes`, `search_index`, `stepback_search_index`,
//! `stepback_summary_index` and `remove_index`, served by the Rust worker
//! for the indexing families when the deployment indexes with the Rust
//! runtime (`ELITEA_INDEXING_RUNTIME=rust`).
//!
//! Like the `artifact` family, these tools act under an authority the frozen
//! settings cannot carry: the claim's `elitea-vector` token, the gateway
//! embeddings route and (for the step-back tools) the agent's model. The
//! host lends them as an [`IndexToolAuthority`] over the [`IndexToolHost`]
//! capability; this module owns what is the SAME for every host — the tool
//! names and argument schemas (byte for byte the SDK's, which saved agents
//! and pipeline mappings were written against), the `selected_tools` rule,
//! and the catalog of indexes Main froze into the toolkit.
//!
//! **The catalog.** An SDK index *name* is an `index_registry` row
//! (ADR-0031): elitea-main is the only reader of that table, so under the
//! Rust indexing runtime it writes the toolkit's indexes into the frozen
//! toolkit object as `indexes` — `[{name, index_id, embedding_model,
//! embedding_dimension}]` — next to `settings`. The key is ABSENT on the
//! Python indexing path, and that absence is what tells this runtime the
//! tools are not its to serve. A present key with no authority (a worker
//! without a `vector_target`) leaves the tools out with a warning; a present
//! key that does not parse leaves that toolkit's index tools out with a
//! warning too (review round 2) — the family's own tools are unaffected,
//! and Main's freeze applies the same entry rule, so a rejected catalog is
//! a bug on one side rather than a configuration the user can fix.
//!
//! The catalog a run's tools read is one [`SharedIndexCatalog`] per
//! toolkit: `remove_index` takes the removed index out of it, so a later
//! `list_indexes` or `search_index` in the same turn does not see it.
//!
//! `index_data` is not served here: an index run is admitted through
//! elitea-main (`index.ingest.v1`), never inline in a turn.

use std::fmt;
use std::sync::{Arc, PoisonError, RwLock};

use adk_core::{Tool, ToolContext};
use adk_tool::BasicToolset;
use async_trait::async_trait;
use serde_json::{Map, Value, json};

use crate::toolkits::invocation::{MaterializedToolsetError, admit_materialized_toolset};
use crate::toolkits::policy::ToolAdmissionPolicy;

/// The frozen toolkit key that carries the catalog.
pub const INDEXES_KEY: &str = "indexes";
/// The SDK's `INDEX_NAME_MAX_LENGTH`.
const INDEX_NAME_MAX_LENGTH: usize = 32;
/// The most catalog entries a toolkit may carry: Main caps the catalog it
/// freezes at the same number (`indexcatalog.MaxEntries`), oldest first,
/// so a catalog that reaches this runtime always parses. A global search
/// covers at most 64 of them.
pub(crate) const MAX_CATALOG_ENTRIES: usize = 1_024;
const MAX_MODEL_NAME_BYTES: usize = 256;
const MAX_INDEX_ID_BYTES: usize = 64;
const MAX_DIMENSION: u64 = 65_536;
const MAX_SELECTED_TOOLS: usize = 256;
const MAX_DESCRIPTION_BYTES: usize = 1_000;

/// The toolkit types whose families carry the index tools: stage I1 and stage
/// I2 (ADR-0030). Elitea-main's `indexcatalog.Families` and the worker's
/// `SERVED_TOOLKIT_TYPES` list the same types; all three are tested against
/// `index_served_families.json`.
pub const INDEX_FAMILIES: [&str; 11] = [
    "ado_boards",
    "ado_plans",
    "ado_repos",
    "ado_wiki",
    "artifact",
    "bitbucket",
    "confluence",
    "github",
    "gitlab",
    "jira",
    "sharepoint",
];

/// Whether `toolkit_type` is a Rust-indexed family (I1 and I2).
#[must_use]
pub fn is_index_family(toolkit_type: &str) -> bool {
    INDEX_FAMILIES.contains(&toolkit_type)
}

/// Whether `name` is one of the SDK's six index tool names (`index_data`
/// included, which this runtime never serves inline). A family uses it to
/// tell an index-only `selected_tools` — which selects none of the family's
/// own tools and is NOT an unsupported selection — from a selection of
/// names it does not know.
#[must_use]
pub fn is_sdk_index_tool_name(name: &str) -> bool {
    name == "index_data" || IndexToolName::parse(name).is_some()
}

/// The SDK `doctype` of an indexing family: the code families format hits
/// as `file -> method` with a fenced block, every other family as plain
/// dicts.
#[must_use]
pub fn doctype_for(toolkit_type: &str) -> &'static str {
    match toolkit_type {
        "ado_repos" | "bitbucket" | "github" | "gitlab" => "code",
        _ => "document",
    }
}

/// One of the five served index tools, by its SDK name.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum IndexToolName {
    ListIndexes,
    SearchIndex,
    StepbackSearchIndex,
    StepbackSummaryIndex,
    RemoveIndex,
}

impl IndexToolName {
    /// Every served tool, in the SDK's listing order.
    pub const ALL: [Self; 5] = [
        Self::ListIndexes,
        Self::SearchIndex,
        Self::StepbackSearchIndex,
        Self::StepbackSummaryIndex,
        Self::RemoveIndex,
    ];

    /// The SDK tool name.
    #[must_use]
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::ListIndexes => "list_indexes",
            Self::SearchIndex => "search_index",
            Self::StepbackSearchIndex => "stepback_search_index",
            Self::StepbackSummaryIndex => "stepback_summary_index",
            Self::RemoveIndex => "remove_index",
        }
    }

    /// The tool of an SDK name, if it is one of the five.
    #[must_use]
    pub fn parse(name: &str) -> Option<Self> {
        Self::ALL.into_iter().find(|tool| tool.as_str() == name)
    }

    /// Whether the tool changes nothing (`remove_index` deletes).
    #[must_use]
    pub const fn is_read_only(self) -> bool {
        !matches!(self, Self::RemoveIndex)
    }

    /// Whether the tool calls the chat model.
    #[must_use]
    pub const fn calls_model(self) -> bool {
        matches!(self, Self::StepbackSearchIndex | Self::StepbackSummaryIndex)
    }

    /// The SDK's description, exactly (the method docstrings, indentation
    /// and leading spaces included: saved prompts quote them).
    #[must_use]
    pub const fn sdk_description(self) -> &'static str {
        match self {
            Self::ListIndexes => "Lists all indexes in the vector store.",
            Self::SearchIndex => SEARCH_INDEX_DESCRIPTION,
            Self::StepbackSearchIndex => " Searches indexed documents in the vector store.",
            Self::StepbackSummaryIndex => {
                " Generates a summary of indexed documents using stepback technique."
            }
            Self::RemoveIndex => "Cleans the indexed data in the collection.",
        }
    }

    /// The SDK's argument schema (`BaseSearchParams`,
    /// `BaseStepbackSearchParams`, `RemoveIndexParams`, `ListIndexesParams`)
    /// as JSON Schema: the same property names, the same JSON types and the
    /// same required set, so a pipeline mapping written against the SDK is
    /// accepted (`sdk_conformance`).
    #[must_use]
    pub fn parameters_schema(self) -> Value {
        match self {
            Self::ListIndexes => json!({
                "type": "object",
                "properties": {},
                "additionalProperties": false
            }),
            Self::SearchIndex => {
                let mut properties = Map::new();
                properties.insert("query".to_owned(), query_schema());
                properties.insert(
                    "index_name".to_owned(),
                    index_name_schema(
                        "Optional index name (max 32 characters). Leave empty to search across all datasets",
                    ),
                );
                search_common_fields(&mut properties);
                properties.insert(
                    "output_fields".to_owned(),
                    json!({
                        "type": ["array", "null"],
                        "items": {"type": "string"},
                        "description": "Fields to include in output. Supports: 'page_content', 'score', 'metadata' (all metadata), or 'metadata.<field>' for specific metadata fields (e.g., 'metadata.source'). If None or empty, returns all fields.",
                        "default": null,
                        "examples": [["metadata", "score"], ["page_content", "metadata.source"], ["metadata.id", "metadata.source"]]
                    }),
                );
                json!({
                    "type": "object",
                    "properties": properties,
                    "required": ["query"],
                    "additionalProperties": false
                })
            }
            Self::StepbackSearchIndex | Self::StepbackSummaryIndex => {
                let mut properties = Map::new();
                properties.insert("query".to_owned(), query_schema());
                properties.insert(
                    "index_name".to_owned(),
                    index_name_schema("Optional index name (max 32 characters)"),
                );
                properties.insert(
                    "messages".to_owned(),
                    json!({
                        "type": ["array", "null"],
                        "items": {},
                        "description": "Chat messages for stepback search context",
                        "default": []
                    }),
                );
                search_common_fields(&mut properties);
                json!({
                    "type": "object",
                    "properties": properties,
                    "required": ["query"],
                    "additionalProperties": false
                })
            }
            Self::RemoveIndex => json!({
                "type": "object",
                "properties": {
                    "index_name": index_name_schema("Optional index name (max 32 characters)")
                },
                "additionalProperties": false
            }),
        }
    }
}

/// `BaseIndexerToolkit.search_index.__doc__`, as the SDK exposes it.
const SEARCH_INDEX_DESCRIPTION: &str = "Searches indexed documents in the vector store.

        Args:
            query: Search query string.
            index_name: Collection/index name to search in.
            filter: Filter criteria for search.
            cut_off: Minimum similarity score threshold.
            search_top: Maximum number of results to return.
            reranker: Legacy reranking configuration.
            full_text_search: Full-text search configuration.
            reranking_config: Advanced reranking configuration.
            extended_search: Extended search chunk types.
            output_fields: Fields to include in output. Supports:
                - \"page_content\": document content
                - \"score\": similarity score
                - \"metadata\": all metadata fields
                - \"metadata.<field>\": specific metadata field (e.g., \"metadata.source\")
                If None or empty list, returns all fields (backward compatible).
                If all specified fields are invalid, returns all fields.

        Returns:
            List of documents with requested fields, or error message string.
        ";

fn query_schema() -> Value {
    json!({"type": "string", "description": "Query text to search in the index"})
}

fn index_name_schema(description: &str) -> Value {
    json!({
        "type": ["string", "null"],
        "maxLength": INDEX_NAME_MAX_LENGTH,
        "description": description,
        "default": ""
    })
}

/// `index_params._search_common_fields`, with the index tools' cut-off
/// default (`DEFAULT_CUT_OFF = 0.1`).
fn search_common_fields(properties: &mut Map<String, Value>) {
    properties.insert(
        "filter".to_owned(),
        json!({
            "type": ["object", "string", "null"],
            "description": "Filter to apply to the search results. Can be a dictionary or a JSON string.",
            "default": {},
            "examples": ["{\"key\": \"value\"}", "{\"status\": \"active\"}"]
        }),
    );
    properties.insert(
        "cut_off".to_owned(),
        json!({
            "type": ["number", "null"],
            "minimum": 0,
            "maximum": 1,
            "description": "Cut-off score for search results",
            "default": 0.1
        }),
    );
    properties.insert(
        "search_top".to_owned(),
        json!({
            "type": ["integer", "null"],
            "exclusiveMinimum": 0,
            "description": "Number of top results to return",
            "default": 10
        }),
    );
    properties.insert(
        "full_text_search".to_owned(),
        json!({
            "type": ["object", "null"],
            "description": "Full text search parameters. Can be a dictionary with search options.",
            "default": null
        }),
    );
    properties.insert(
        "extended_search".to_owned(),
        json!({
            "type": ["array", "null"],
            "items": {"type": "string"},
            "description": "List of additional fields to include in the search results.",
            "default": null
        }),
    );
    properties.insert(
        "reranker".to_owned(),
        json!({
            "type": ["object", "null"],
            "description": "Reranker configuration. Can be a dictionary with reranking parameters.",
            "default": {}
        }),
    );
    properties.insert(
        "reranking_config".to_owned(),
        json!({
            "type": ["object", "null"],
            "description": "Reranking configuration. Can be a dictionary with reranking settings.",
            "default": null
        }),
    );
}

/// One index of the toolkit, as Main's registry knows it.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct IndexCatalogEntry {
    /// The name the user gave the index.
    pub name: String,
    /// The `index_registry` UUID: the vector namespace.
    pub index_id: String,
    /// The embedding model the index was built with (the gateway name).
    pub embedding_model: String,
    /// The vector length the index was stamped with.
    pub embedding_dimension: u32,
}

/// The indexes of one toolkit, in Main's listing order.
#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct IndexCatalog {
    entries: Vec<IndexCatalogEntry>,
}

/// Why the frozen `indexes` value was refused. Names a key, never a value.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct IndexCatalogError(&'static str);

impl fmt::Display for IndexCatalogError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(
            formatter,
            "the toolkit's index catalog is invalid: {}",
            self.0
        )
    }
}

impl std::error::Error for IndexCatalogError {}

impl IndexCatalog {
    /// A catalog of `entries`.
    #[must_use]
    pub fn new(entries: Vec<IndexCatalogEntry>) -> Self {
        Self { entries }
    }

    /// Reads the frozen toolkit's `indexes` value: an array of objects, each
    /// with a non-empty `name`, a canonical `index_id`, an `embedding_model`
    /// and an `embedding_dimension` in 1..=65536. Duplicate names are
    /// refused: a name must address one namespace.
    ///
    /// # Errors
    ///
    /// Not an array, too many entries, or an entry out of shape.
    pub fn parse(value: &Value) -> Result<Self, IndexCatalogError> {
        let items = value
            .as_array()
            .ok_or(IndexCatalogError("`indexes` is not an array"))?;
        if items.len() > MAX_CATALOG_ENTRIES {
            return Err(IndexCatalogError("`indexes` holds too many entries"));
        }
        let mut entries = Vec::with_capacity(items.len());
        for item in items {
            let object = item
                .as_object()
                .ok_or(IndexCatalogError("an `indexes` entry is not an object"))?;
            let name = bounded_text(object, "name", INDEX_NAME_MAX_LENGTH * 8)?;
            let index_id = bounded_text(object, "index_id", MAX_INDEX_ID_BYTES)?;
            if !index_id
                .bytes()
                .all(|byte| byte.is_ascii_hexdigit() || byte == b'-')
                || index_id.bytes().any(|byte| byte.is_ascii_uppercase())
            {
                return Err(IndexCatalogError(
                    "an `indexes` entry has a non-canonical `index_id`",
                ));
            }
            let embedding_model = bounded_text(object, "embedding_model", MAX_MODEL_NAME_BYTES)?;
            let dimension = object
                .get("embedding_dimension")
                .and_then(Value::as_u64)
                .filter(|dimension| (1..=MAX_DIMENSION).contains(dimension))
                .ok_or(IndexCatalogError(
                    "an `indexes` entry has no `embedding_dimension` in 1..=65536",
                ))?;
            if entries
                .iter()
                .any(|entry: &IndexCatalogEntry| entry.name == name)
            {
                return Err(IndexCatalogError("`indexes` repeats a name"));
            }
            entries.push(IndexCatalogEntry {
                name,
                index_id,
                embedding_model,
                embedding_dimension: u32::try_from(dimension).map_err(|_| {
                    IndexCatalogError("an `indexes` entry has a dimension out of range")
                })?,
            });
        }
        Ok(Self { entries })
    }

    /// The entries, in listing order.
    #[must_use]
    pub fn entries(&self) -> &[IndexCatalogEntry] {
        &self.entries
    }

    /// Takes the index `name` out; whether it was there.
    fn remove(&mut self, name: &str) -> bool {
        let before = self.entries.len();
        self.entries.retain(|entry| entry.name != name);
        self.entries.len() != before
    }
}

/// One toolkit's catalog as the run's index tools share it: every tool of
/// the toolkit reads the same one, and `remove_index` takes a removed index
/// out of it (review round 2), so the rest of the turn no longer lists or
/// searches an index Main has tombstoned. A poisoned lock yields its last
/// state: the catalog holds plain data no panic can leave half-written.
#[derive(Debug, Default)]
pub struct SharedIndexCatalog {
    catalog: RwLock<IndexCatalog>,
}

impl SharedIndexCatalog {
    /// Shares `catalog`.
    #[must_use]
    pub fn new(catalog: IndexCatalog) -> Self {
        Self {
            catalog: RwLock::new(catalog),
        }
    }

    /// The catalog as it stands now.
    #[must_use]
    pub fn snapshot(&self) -> IndexCatalog {
        self.catalog
            .read()
            .unwrap_or_else(PoisonError::into_inner)
            .clone()
    }

    /// Takes the index `name` out; whether it was there.
    pub fn remove(&self, name: &str) -> bool {
        self.catalog
            .write()
            .unwrap_or_else(PoisonError::into_inner)
            .remove(name)
    }
}

fn bounded_text(
    object: &Map<String, Value>,
    key: &str,
    max_bytes: usize,
) -> Result<String, IndexCatalogError> {
    match object.get(key).and_then(Value::as_str) {
        Some(text)
            if !text.trim().is_empty()
                && text.len() <= max_bytes
                && !text.chars().any(char::is_control) =>
        {
            Ok(text.to_owned())
        }
        _ => Err(IndexCatalogError(match key {
            "name" => "an `indexes` entry has no `name`",
            "index_id" => "an `indexes` entry has no `index_id`",
            _ => "an `indexes` entry has no `embedding_model`",
        })),
    }
}

/// One tool call, as the host receives it.
pub struct IndexToolCall<'a> {
    /// Which tool.
    pub tool: IndexToolName,
    /// The frozen toolkit's `id`, which Main's index delete flow is
    /// addressed by for `remove_index`.
    pub toolkit_id: u64,
    /// The toolkit's indexes, shared by the toolkit's tools for the run.
    pub catalog: &'a SharedIndexCatalog,
    /// `code` or `document` (the family's SDK `doctype`).
    pub doctype: &'a str,
    /// The call's arguments, as the model sent them (an object).
    pub arguments: &'a Value,
}

/// A failure the model reads as the tool's result: one sentence, no
/// credential, no provider body. Every refusal of the vector store, the
/// embeddings gateway or the model becomes one of these rather than an
/// `AdkError` that aborts the turn — the rule the artifact family and the
/// SDK's `ToolException` follow.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct IndexToolFailure {
    message: String,
}

impl IndexToolFailure {
    /// A failure with `message`.
    #[must_use]
    pub fn new(message: impl Into<String>) -> Self {
        Self {
            message: message.into(),
        }
    }

    /// The sentence the model reads.
    #[must_use]
    pub fn message(&self) -> &str {
        &self.message
    }
}

impl fmt::Display for IndexToolFailure {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(&self.message)
    }
}

impl std::error::Error for IndexToolFailure {}

/// What the host provides: the index tools' effects over the claim's vector
/// token, the gateway embeddings route and, for the step-back tools, a chat
/// model. The cloud worker implements it on `elitea-vector-index`; a host
/// without a vector store lends no authority and the tools are left out.
#[async_trait]
pub trait IndexToolHost: Send + Sync {
    /// Runs one index tool and returns its SDK-shaped result: a list of
    /// `{page_content, metadata, score}` dicts or a message string for
    /// `search_index`, a string for the others.
    async fn call(&self, call: IndexToolCall<'_>) -> Result<Value, IndexToolFailure>;
}

/// The authority the index tools act under, shared for one run.
#[derive(Clone)]
pub struct IndexToolAuthority {
    host: Arc<dyn IndexToolHost>,
}

impl IndexToolAuthority {
    /// An authority over `host`.
    #[must_use]
    pub fn new(host: Arc<dyn IndexToolHost>) -> Self {
        Self { host }
    }

    /// The host.
    #[must_use]
    pub fn host(&self) -> &dyn IndexToolHost {
        self.host.as_ref()
    }
}

/// Why a run lends no index-tool authority. The materializer names it when
/// a toolkit carries a catalog and its index tools are left out, so the
/// warning states the real cause rather than guessing one.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum IndexToolsWithheld {
    /// This worker has no vector store (`vector_target` unset).
    NoVectorStore,
    /// Main minted no `elitea-vector` token into the claim.
    NoVectorToken,
    /// The run redeems no claim context, which the query embedding needs
    /// (a pipeline whose nodes never call an index tool).
    NoRuntimeContext,
    /// The path never serves the index tools (a direct read or a tool
    /// listing), by design rather than by a missing dependency.
    NotServedOnThisPath,
    /// Nothing in this run names an index tool (no toolkit in the snapshot
    /// carries a catalog, or the one tool called is not an index tool), so
    /// no authority was built.
    NotRequested,
}

impl IndexToolsWithheld {
    /// The `reason_code` the materializer logs.
    #[must_use]
    pub const fn reason_code(self) -> &'static str {
        match self {
            Self::NoVectorStore => "no_vector_target",
            Self::NoVectorToken => "no_vector_token",
            Self::NoRuntimeContext => "no_runtime_context",
            Self::NotServedOnThisPath => "not_served_on_this_path",
            Self::NotRequested => "not_requested",
        }
    }

    /// The sentence the materializer logs with the reason.
    #[must_use]
    pub const fn message(self) -> &'static str {
        match self {
            Self::NoVectorStore => {
                "the toolkit carries an index catalog, but this worker has no vector store; its index tools were left out"
            }
            Self::NoVectorToken => {
                "the toolkit carries an index catalog, but the claim carries no vector token; its index tools were left out"
            }
            Self::NoRuntimeContext => {
                "the toolkit carries an index catalog, but this run redeems no claim context (no node calls an index tool); its index tools were left out"
            }
            Self::NotServedOnThisPath => {
                "the toolkit carries an index catalog, but this path does not serve the index tools; they were left out"
            }
            Self::NotRequested => {
                "the toolkit carries an index catalog, but nothing in this run calls an index tool; none was composed"
            }
        }
    }

    /// Whether leaving the tools out is worth a warning: a missing
    /// dependency is, a path that never serves them by design is not.
    #[must_use]
    pub const fn is_warning(self) -> bool {
        !matches!(
            self,
            Self::NotServedOnThisPath | Self::NoRuntimeContext | Self::NotRequested
        )
    }
}

/// What a run lends the indexing families for their index tools
/// (ADR-0030 I1): an authority, or the stated reason there is none.
#[derive(Clone, Copy)]
pub enum IndexToolSupport<'a> {
    /// The index tools are composed under this authority.
    Lent(&'a IndexToolAuthority),
    /// The index tools are left out, for this reason.
    Withheld(IndexToolsWithheld),
}

impl<'a> IndexToolSupport<'a> {
    /// The lent authority, if any.
    #[must_use]
    pub const fn authority(self) -> Option<&'a IndexToolAuthority> {
        match self {
            Self::Lent(authority) => Some(authority),
            Self::Withheld(_) => None,
        }
    }

    /// `Lent` for `Some`, else `Withheld(reason)`.
    #[must_use]
    pub const fn lent_or(
        authority: Option<&'a IndexToolAuthority>,
        reason: IndexToolsWithheld,
    ) -> Self {
        match authority {
            Some(authority) => Self::Lent(authority),
            None => Self::Withheld(reason),
        }
    }
}

/// Why the index toolset could not be built.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum IndexToolsetErrorCode {
    /// The catalog or `selected_tools` is out of shape.
    InvalidConfiguration,
    /// A bound was exceeded.
    ResourceExhausted,
}

/// The index toolset could not be built. Carries no configuration value.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct IndexToolsetError {
    code: IndexToolsetErrorCode,
}

impl IndexToolsetError {
    /// The code.
    #[must_use]
    pub const fn code(self) -> IndexToolsetErrorCode {
        self.code
    }
}

impl fmt::Display for IndexToolsetError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(match self.code {
            IndexToolsetErrorCode::InvalidConfiguration => {
                "the index tools' configuration is invalid"
            }
            IndexToolsetErrorCode::ResourceExhausted => {
                "the index tools' configuration exceeds its bounds"
            }
        })
    }
}

impl std::error::Error for IndexToolsetError {}

impl From<IndexCatalogError> for IndexToolsetError {
    fn from(_: IndexCatalogError) -> Self {
        Self {
            code: IndexToolsetErrorCode::InvalidConfiguration,
        }
    }
}

impl From<MaterializedToolsetError> for IndexToolsetError {
    fn from(_: MaterializedToolsetError) -> Self {
        Self {
            code: IndexToolsetErrorCode::InvalidConfiguration,
        }
    }
}

/// The index tools of one toolkit: every served tool, or the selected ones
/// when the SDK's `selected_tools` names any (the rule every indexing
/// toolkit's `get_toolkit` applies: an empty list exposes everything).
///
/// # Errors
///
/// A malformed `selected_tools`, or the policy's refusal of an identity.
pub fn build_index_toolset(
    toolkit_name: &str,
    toolkit_id: u64,
    toolkit_type: &str,
    settings: &Map<String, Value>,
    catalog: IndexCatalog,
    policy: &Arc<ToolAdmissionPolicy>,
    authority: &IndexToolAuthority,
) -> Result<BasicToolset, IndexToolsetError> {
    let selected = selected_tools(settings)?;
    let include = |name: &str| selected.is_empty() || selected.iter().any(|value| value == name);
    let catalog = Arc::new(SharedIndexCatalog::new(catalog));
    let doctype: Arc<str> = Arc::from(doctype_for(toolkit_type));
    let tools: Vec<Arc<dyn Tool>> = IndexToolName::ALL
        .into_iter()
        .filter(|tool| include(tool.as_str()))
        .map(|tool| {
            Arc::new(IndexTool {
                tool,
                toolkit_id,
                authority: authority.clone(),
                catalog: Arc::clone(&catalog),
                doctype: Arc::clone(&doctype),
                description: tool_description(toolkit_name, tool.sdk_description()),
            }) as Arc<dyn Tool>
        })
        .collect();
    admit_materialized_toolset(toolkit_name, toolkit_type, policy, tools).map_err(Into::into)
}

/// The tools of [`build_index_toolset`] before admission, for the schema
/// gate: every served tool, unfiltered.
#[cfg(any(test, feature = "test-support"))]
#[must_use]
pub fn all_index_tools(
    toolkit_name: &str,
    toolkit_type: &str,
    authority: &IndexToolAuthority,
) -> Vec<Arc<dyn Tool>> {
    let catalog = Arc::new(SharedIndexCatalog::default());
    let doctype: Arc<str> = Arc::from(doctype_for(toolkit_type));
    IndexToolName::ALL
        .into_iter()
        .map(|tool| {
            Arc::new(IndexTool {
                tool,
                toolkit_id: 0,
                authority: authority.clone(),
                catalog: Arc::clone(&catalog),
                doctype: Arc::clone(&doctype),
                description: tool_description(toolkit_name, tool.sdk_description()),
            }) as Arc<dyn Tool>
        })
        .collect()
}

fn tool_description(toolkit_name: &str, sentence: &str) -> Box<str> {
    format!("Toolkit: {toolkit_name}\n{sentence}")
        .chars()
        .take(MAX_DESCRIPTION_BYTES)
        .collect::<String>()
        .into_boxed_str()
}

/// The SDK's `selected_tools`: a list of names, or absent/null/empty for
/// every tool. An unknown name selects nothing, as in the artifact family: a
/// stored selection must not turn a partial port into a dead toolkit.
fn selected_tools(settings: &Map<String, Value>) -> Result<Vec<String>, IndexToolsetError> {
    let Some(value) = settings.get("selected_tools") else {
        return Ok(Vec::new());
    };
    if value.is_null() {
        return Ok(Vec::new());
    }
    let values = value.as_array().ok_or(IndexToolsetError {
        code: IndexToolsetErrorCode::InvalidConfiguration,
    })?;
    if values.len() > MAX_SELECTED_TOOLS {
        return Err(IndexToolsetError {
            code: IndexToolsetErrorCode::ResourceExhausted,
        });
    }
    values
        .iter()
        .map(|value| {
            value.as_str().map(str::to_owned).ok_or(IndexToolsetError {
                code: IndexToolsetErrorCode::InvalidConfiguration,
            })
        })
        .collect()
}

/// Whether the toolkit's stored `selected_tools` names index tools ONLY: a
/// non-empty list of SDK index tool names and nothing else. Such a
/// selection selects none of the family's own tools, and is admissible only
/// where the index toolset is composed beside the family (ADR-0030 I1);
/// elsewhere the family refuses it as an unsupported selection, as before.
///
/// # Errors
///
/// A malformed `selected_tools`.
pub fn selection_is_index_only(settings: &Map<String, Value>) -> Result<bool, IndexToolsetError> {
    let selected = selected_tools(settings)?;
    Ok(!selected.is_empty() && selected.iter().all(|name| is_sdk_index_tool_name(name)))
}

struct IndexTool {
    tool: IndexToolName,
    toolkit_id: u64,
    authority: IndexToolAuthority,
    catalog: Arc<SharedIndexCatalog>,
    doctype: Arc<str>,
    description: Box<str>,
}

#[async_trait]
impl Tool for IndexTool {
    fn name(&self) -> &str {
        self.tool.as_str()
    }

    fn description(&self) -> &str {
        &self.description
    }

    fn is_read_only(&self) -> bool {
        self.tool.is_read_only()
    }

    fn is_concurrency_safe(&self) -> bool {
        self.tool.is_read_only()
    }

    fn parameters_schema(&self) -> Option<Value> {
        Some(self.tool.parameters_schema())
    }

    async fn execute(
        &self,
        _context: Arc<dyn ToolContext>,
        arguments: Value,
    ) -> adk_core::Result<Value> {
        // A non-object argument set is the model's mistake, answered as text
        // like every other refusal: the turn goes on.
        let arguments = if arguments.is_object() {
            arguments
        } else if arguments.is_null() {
            Value::Object(Map::new())
        } else {
            return Ok(Value::String(
                "the tool arguments must be an object".to_owned(),
            ));
        };
        let call = IndexToolCall {
            tool: self.tool,
            toolkit_id: self.toolkit_id,
            catalog: self.catalog.as_ref(),
            doctype: self.doctype.as_ref(),
            arguments: &arguments,
        };
        match self.authority.host().call(call).await {
            Ok(value) => Ok(value),
            Err(failure) => Ok(Value::String(failure.message().to_owned())),
        }
    }
}

/// One toolkit's family toolset and its index toolset under the toolkit's
/// one name, so the run sees one toolset per configured toolkit as before.
pub struct CompositeToolset {
    name: Box<str>,
    parts: Vec<Arc<dyn adk_core::Toolset>>,
}

impl CompositeToolset {
    /// `parts` presented as one toolset named `name`.
    #[must_use]
    pub fn new(name: &str, parts: Vec<Arc<dyn adk_core::Toolset>>) -> Self {
        Self {
            name: name.into(),
            parts,
        }
    }
}

#[async_trait]
impl adk_core::Toolset for CompositeToolset {
    fn name(&self) -> &str {
        &self.name
    }

    async fn tools(
        &self,
        context: Arc<dyn adk_core::ReadonlyContext>,
    ) -> adk_core::Result<Vec<Arc<dyn Tool>>> {
        let mut tools = Vec::new();
        for part in &self.parts {
            tools.extend(part.tools(Arc::clone(&context)).await?);
        }
        Ok(tools)
    }
}

use std::fmt;
use std::fmt::Write as _;
use std::sync::Arc;

use adk_core::{AdkError, ErrorCategory, ErrorComponent, Tool, ToolContext};
use adk_tool::BasicToolset;
use async_trait::async_trait;
use serde_json::{Map, Value, json};

use crate::toolkits::invocation::{MaterializedToolsetError, admit_materialized_toolset};
use crate::toolkits::policy::ToolAdmissionPolicy;

use super::client::{BigQueryApi, BigQueryClient, BigQueryClientError, QueryJob};
use super::config::{
    BigQueryConfigError, BigQueryConfigErrorCode, BigQueryToolkitConfig, valid_dataset,
    valid_project, valid_table,
};
use super::format::{Cell, python_dumps, python_float};

const GET_DOCUMENTS: &str = "get_documents";
const BATCH_SEARCH: &str = "batch_search";
const CREATE_VECTOR_INDEX: &str = "create_vector_index";
const JOB_STATS: &str = "job_stats";
const SIMILARITY_SEARCH_BY_VECTOR: &str = "similarity_search_by_vector";
const SIMILARITY_SEARCH_BY_VECTOR_WITH_SCORE: &str = "similarity_search_by_vector_with_score";
const SIMILARITY_SEARCH_BY_VECTORS: &str = "similarity_search_by_vectors";
const CREATE_DELTA_LAKE_TABLE: &str = "create_delta_lake_table";
/// SDK tools this family deliberately does not serve; see `mod.rs`.
const UNSERVED_SDK_TOOLS: [&str; 3] = [
    "similarity_search",
    "similarity_search_with_score",
    "execute",
];

const DEFAULT_K: u64 = 5;
const MAX_K: u64 = 1_000;
const MAX_ARGUMENT_BYTES: usize = 256 * 1_024;
const MAX_DESCRIPTION_BYTES: usize = 1_000;
const MAX_FILTER_BYTES: usize = 16 * 1_024;
const MAX_FILTER_FIELDS: usize = 64;
const MAX_FIELD_PATH_BYTES: usize = 300;
const MAX_IDS: usize = 1_000;
const MAX_ID_BYTES: usize = 1_024;
const MAX_EMBEDDINGS: usize = 32;
const MAX_EMBEDDING_DIMENSIONS: usize = 8_192;
const MAX_JOB_ID_BYTES: usize = 1_024;
const MAX_CONNECTION_ID_BYTES: usize = 1_024;
const MAX_SOURCE_URIS: usize = 64;
const MAX_SOURCE_URI_BYTES: usize = 2_048;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum BigQueryToolsetErrorCode {
    InvalidConfiguration,
    ResourceExhausted,
    UnsupportedSelection,
    Client,
    InvalidDefinition,
}

/// Stable construction failure for the partial `BigQuery` family.
pub(crate) struct BigQueryToolsetError {
    code: BigQueryToolsetErrorCode,
}

impl BigQueryToolsetError {
    #[must_use]
    pub(crate) const fn code(&self) -> BigQueryToolsetErrorCode {
        self.code
    }
}

impl fmt::Debug for BigQueryToolsetError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("BigQueryToolsetError")
            .field("code", &self.code)
            .finish_non_exhaustive()
    }
}

impl fmt::Display for BigQueryToolsetError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(match self.code {
            BigQueryToolsetErrorCode::InvalidConfiguration => {
                "the BigQuery toolkit configuration is invalid"
            }
            BigQueryToolsetErrorCode::ResourceExhausted => {
                "the BigQuery toolkit configuration exceeds its approved limit"
            }
            BigQueryToolsetErrorCode::UnsupportedSelection => {
                "the selected BigQuery tools are not served by this runtime"
            }
            BigQueryToolsetErrorCode::Client => "the BigQuery client could not be created",
            BigQueryToolsetErrorCode::InvalidDefinition => {
                "the BigQuery ADK tool definition is invalid"
            }
        })
    }
}

impl std::error::Error for BigQueryToolsetError {}

impl From<BigQueryConfigError> for BigQueryToolsetError {
    fn from(source: BigQueryConfigError) -> Self {
        Self {
            code: match source.code() {
                BigQueryConfigErrorCode::InvalidConfiguration => {
                    BigQueryToolsetErrorCode::InvalidConfiguration
                }
                BigQueryConfigErrorCode::ResourceExhausted => {
                    BigQueryToolsetErrorCode::ResourceExhausted
                }
            },
        }
    }
}

impl From<BigQueryClientError> for BigQueryToolsetError {
    fn from(_: BigQueryClientError) -> Self {
        Self {
            code: BigQueryToolsetErrorCode::Client,
        }
    }
}

impl From<MaterializedToolsetError> for BigQueryToolsetError {
    fn from(_: MaterializedToolsetError) -> Self {
        Self {
            code: BigQueryToolsetErrorCode::InvalidDefinition,
        }
    }
}

/// The non-secret table defaults every tool reads, copied out of the sealed
/// configuration so the tools never hold the signing key.
#[derive(Clone, Default)]
pub(in crate::toolkits) struct TableDefaults {
    pub(in crate::toolkits) project: Option<String>,
    pub(in crate::toolkits) dataset: Option<String>,
    pub(in crate::toolkits) table: Option<String>,
}

impl TableDefaults {
    fn from_config(config: &BigQueryToolkitConfig) -> Self {
        Self {
            project: config.project().map(str::to_owned),
            dataset: config.dataset().map(str::to_owned),
            table: config.table().map(str::to_owned),
        }
    }
}

/// Build the `BigQuery` tools this runtime serves from the SDK selection.
///
/// The SDK binds only explicitly selected tools: an empty selection binds
/// none. A selection naming only tools this runtime does not serve is
/// refused, so the materializer skips the toolkit with its warning instead of
/// binding an empty one silently.
pub(crate) fn build_bigquery_toolset(
    toolkit_name: &str,
    config: BigQueryToolkitConfig,
    policy: &Arc<ToolAdmissionPolicy>,
) -> Result<BasicToolset, BigQueryToolsetError> {
    let selected = served_selection(toolkit_name, config.selected_tools())?;
    let defaults = TableDefaults::from_config(&config);
    let client: Arc<dyn BigQueryApi> = Arc::new(BigQueryClient::new(config)?);
    build_with_api(toolkit_name, &selected, &defaults, policy, &client)
}

fn served_selection(
    toolkit_name: &str,
    selected: &[Box<str>],
) -> Result<Vec<BigQueryToolKind>, BigQueryToolsetError> {
    if selected.is_empty() {
        return Ok(Vec::new());
    }
    let served = BigQueryToolKind::ALL
        .into_iter()
        .filter(|kind| selected.iter().any(|name| name.as_ref() == kind.name()))
        .collect::<Vec<_>>();
    if served.is_empty() {
        return Err(BigQueryToolsetError {
            code: BigQueryToolsetErrorCode::UnsupportedSelection,
        });
    }
    let omitted = selected.len() - served.len();
    if omitted > 0 {
        let unserved = selected
            .iter()
            .filter(|name| UNSERVED_SDK_TOOLS.contains(&name.as_ref()))
            .count();
        tracing::warn!(
            event = "agent_toolkit_tools_skipped",
            reason_code = "unsupported_tool_selection",
            toolkit_type = "bigquery",
            toolkit_name,
            selected_count = selected.len(),
            materialized_count = served.len(),
            omitted_count = omitted,
            unserved_sdk_count = unserved,
            "BigQuery tools this runtime does not serve were omitted from the native toolset"
        );
    }
    Ok(served)
}

fn build_with_api(
    toolkit_name: &str,
    selected: &[BigQueryToolKind],
    defaults: &TableDefaults,
    policy: &Arc<ToolAdmissionPolicy>,
    client: &Arc<dyn BigQueryApi>,
) -> Result<BasicToolset, BigQueryToolsetError> {
    let tools = selected
        .iter()
        .map(|kind| {
            Arc::new(BigQueryTool::new(
                *kind,
                toolkit_name,
                defaults.clone(),
                Arc::clone(client),
            )) as Arc<dyn Tool>
        })
        .collect::<Vec<_>>();
    admit_materialized_toolset(toolkit_name, "bigquery", policy, tools).map_err(Into::into)
}

#[cfg(test)]
pub(in crate::toolkits) fn test_build_with_api(
    toolkit_name: &str,
    selected: &[String],
    defaults: &TableDefaults,
    policy: &Arc<ToolAdmissionPolicy>,
    client: &Arc<dyn BigQueryApi>,
) -> Result<BasicToolset, BigQueryToolsetError> {
    let selected = selected
        .iter()
        .map(|name| Box::<str>::from(name.as_str()))
        .collect::<Vec<_>>();
    let served = served_selection(toolkit_name, &selected)?;
    build_with_api(toolkit_name, &served, defaults, policy, client)
}

/// The served tools without the policy wrapper, whose sanitizer replaces
/// every tool error with a generic one: the SDK-parity messages are asserted
/// here, before that boundary.
#[cfg(test)]
pub(in crate::toolkits) fn test_raw_tools(
    toolkit_name: &str,
    defaults: &TableDefaults,
    client: &Arc<dyn BigQueryApi>,
) -> Vec<Arc<dyn Tool>> {
    BigQueryToolKind::ALL
        .into_iter()
        .map(|kind| {
            Arc::new(BigQueryTool::new(
                kind,
                toolkit_name,
                defaults.clone(),
                Arc::clone(client),
            )) as Arc<dyn Tool>
        })
        .collect()
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
enum BigQueryToolKind {
    GetDocuments,
    BatchSearch,
    CreateVectorIndex,
    JobStats,
    SimilaritySearchByVector,
    SimilaritySearchByVectorWithScore,
    SimilaritySearchByVectors,
    CreateDeltaLakeTable,
}

impl BigQueryToolKind {
    /// The SDK's `get_available_tools` order, without the unserved tools.
    const ALL: [Self; 8] = [
        Self::GetDocuments,
        Self::BatchSearch,
        Self::CreateVectorIndex,
        Self::JobStats,
        Self::SimilaritySearchByVector,
        Self::SimilaritySearchByVectorWithScore,
        Self::SimilaritySearchByVectors,
        Self::CreateDeltaLakeTable,
    ];

    const fn name(self) -> &'static str {
        match self {
            Self::GetDocuments => GET_DOCUMENTS,
            Self::BatchSearch => BATCH_SEARCH,
            Self::CreateVectorIndex => CREATE_VECTOR_INDEX,
            Self::JobStats => JOB_STATS,
            Self::SimilaritySearchByVector => SIMILARITY_SEARCH_BY_VECTOR,
            Self::SimilaritySearchByVectorWithScore => SIMILARITY_SEARCH_BY_VECTOR_WITH_SCORE,
            Self::SimilaritySearchByVectors => SIMILARITY_SEARCH_BY_VECTORS,
            Self::CreateDeltaLakeTable => CREATE_DELTA_LAKE_TABLE,
        }
    }

    const fn is_read_only(self) -> bool {
        !matches!(self, Self::CreateVectorIndex | Self::CreateDeltaLakeTable)
    }

    const fn action(self) -> &'static str {
        match self {
            Self::GetDocuments => {
                "Read rows of the configured BigQuery table (project, dataset and table must be configured). ids keeps rows whose doc_id is in the list; filter is either an object of column: value equality pairs joined with AND, or one GoogleSQL WHERE condition over this table's columns (no ';', comments, subqueries or other statements). Returns a JSON array of row objects in column order; more than 1000 rows is refused, so narrow the filter."
            }
            Self::BatchSearch => {
                "Nearest-neighbour search of the configured table's embedding column for each vector in embeddings (Euclidean distance, ascending score, at most k rows each). queries needs an embedding model this toolkit does not have and is refused; pass embeddings instead. filter is a column: value object (values compared as quoted text) or one WHERE condition. Returns a JSON array of result arrays."
            }
            Self::CreateVectorIndex => {
                "Create the IVF Euclidean vector index <table>_langchain_index on the configured table's embedding column if it does not already exist. This is a BigQuery DDL effect; after an unknown outcome check the table's indexes before retrying."
            }
            Self::JobStats => {
                "Read the statistics object of one BigQuery job in the configured project and location by its job ID. Returns the job's statistics JSON (an empty object when BigQuery reports none)."
            }
            Self::SimilaritySearchByVector => {
                "Return the k rows of the configured table nearest to one embedding vector (Euclidean distance on the embedding column, ascending). Each row object carries every column plus score. Returns a JSON array."
            }
            Self::SimilaritySearchByVectorWithScore => {
                "Return the k rows of the configured table nearest to one embedding vector, restricted by an optional filter (column: value object or one WHERE condition). Each row object carries every column plus its Euclidean score. Returns a JSON array."
            }
            Self::SimilaritySearchByVectors => {
                "Run one filtered nearest-neighbour search per embedding vector against the configured table and return a JSON array with one result array per vector. with_embeddings replaces each row's embedding column with the query vector when with_scores is false."
            }
            Self::CreateDeltaLakeTable => {
                "Create (or, when it already exists, return) a BigQuery external table over a Delta Lake on Cloud Storage. Needs table_name, a project.region.connection_id BigLake connection and gs:// source_uris; project and dataset default to the configured ones. Returns the table resource JSON. This is a remote effect; reconcile after an unknown outcome."
            }
        }
    }
}

struct BigQueryTool {
    kind: BigQueryToolKind,
    defaults: TableDefaults,
    client: Arc<dyn BigQueryApi>,
    description: Box<str>,
}

impl BigQueryTool {
    fn new(
        kind: BigQueryToolKind,
        toolkit_name: &str,
        defaults: TableDefaults,
        client: Arc<dyn BigQueryApi>,
    ) -> Self {
        // The SDK prefixes the toolkit name, then the project, and cuts the
        // whole description to 1000 characters.
        let project = defaults.project.as_deref().unwrap_or("None");
        let description = format!(
            "Project: {project}\nToolkit: {toolkit_name}\n{}",
            kind.action()
        );
        Self {
            kind,
            defaults,
            client,
            description: description
                .chars()
                .take(MAX_DESCRIPTION_BYTES)
                .collect::<String>()
                .into_boxed_str(),
        }
    }

    fn table_id(&self) -> adk_core::Result<String> {
        match (
            self.defaults.project.as_deref(),
            self.defaults.dataset.as_deref(),
            self.defaults.table.as_deref(),
        ) {
            (Some(project), Some(dataset), Some(table)) => {
                Ok(format!("{project}.{dataset}.{table}"))
            }
            _ => Err(tool_error(
                "bigquery.table.unspecified",
                "Project, dataset, and table must be specified.",
            )),
        }
    }

    async fn rows(&self, job: QueryJob) -> adk_core::Result<Vec<Cell>> {
        self.client
            .query(job)
            .await
            .map_err(BigQueryClientError::into_adk)
    }

    async fn nearest(
        &self,
        embedding: &[f64],
        filter: Option<&str>,
        k: u64,
    ) -> adk_core::Result<Vec<Cell>> {
        let table_id = self.table_id()?;
        let mut sql = format!(
            "SELECT *, EUCLIDEAN_DISTANCE(embedding, @query_embedding) AS score\nFROM `{table_id}`\n"
        );
        if let Some(filter) = filter {
            let _ = writeln!(sql, "WHERE ({filter})");
        }
        let _ = write!(sql, "ORDER BY score ASC\nLIMIT {k}");
        self.rows(QueryJob {
            sql,
            parameters: vec![float_array_parameter("query_embedding", embedding)],
            effect: false,
        })
        .await
    }
}

#[async_trait]
impl Tool for BigQueryTool {
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
        Some(match self.kind {
            BigQueryToolKind::GetDocuments => object_schema(
                "GetDocuments",
                &json!({
                    "ids": {
                        "anyOf": [
                            {"type": "array", "maxItems": MAX_IDS, "items": {"type": "string", "maxLength": MAX_ID_BYTES}},
                            {"type": "null"}
                        ],
                        "default": null,
                        "description": "doc_id values to keep. Omit or pass null to read every row that matches filter."
                    },
                    "filter": filter_schema()
                }),
                &[],
            ),
            BigQueryToolKind::BatchSearch => object_schema(
                "BatchSearch",
                &json!({
                    "queries": {
                        "anyOf": [{"type": "array", "items": {"type": "string"}}, {"type": "null"}],
                        "default": null,
                        "description": "Text queries. This toolkit has no embedding model, so passing queries is refused; use embeddings."
                    },
                    "embeddings": {
                        "anyOf": [embeddings_schema(), {"type": "null"}],
                        "default": null,
                        "description": "Embedding vectors to search with, at most 32."
                    },
                    "k": k_schema(),
                    "filter": filter_schema()
                }),
                &[],
            ),
            BigQueryToolKind::CreateVectorIndex => object_schema("NoInput", &json!({}), &[]),
            BigQueryToolKind::JobStats => object_schema(
                "JobStatsArgs",
                &json!({
                    "job_id": {
                        "type": "string",
                        "minLength": 1,
                        "maxLength": MAX_JOB_ID_BYTES,
                        "pattern": "^[A-Za-z0-9_-]+$",
                        "description": "BigQuery job ID, for example bquxjob_1a2b3c4d_18f0e1d2c3b."
                    }
                }),
                &["job_id"],
            ),
            BigQueryToolKind::SimilaritySearchByVector => object_schema(
                "SimilaritySearchByVectorArgs",
                &json!({"embedding": embedding_schema(), "k": k_schema()}),
                &["embedding"],
            ),
            BigQueryToolKind::SimilaritySearchByVectorWithScore => object_schema(
                "SimilaritySearchByVectorWithScoreArgs",
                &json!({
                    "embedding": embedding_schema(),
                    "filter": filter_schema(),
                    "k": k_schema()
                }),
                &["embedding"],
            ),
            BigQueryToolKind::SimilaritySearchByVectors => object_schema(
                "SimilaritySearchByVectorsArgs",
                &json!({
                    "embeddings": embeddings_schema(),
                    "filter": filter_schema(),
                    "k": k_schema(),
                    "with_embeddings": {"type": "boolean", "default": false, "description": "Replace each row's embedding column with the query vector."},
                    "with_scores": {"type": "boolean", "default": false, "description": "Keep each row's score (rows always carry it)."}
                }),
                &["embeddings"],
            ),
            BigQueryToolKind::CreateDeltaLakeTable => create_delta_lake_table_schema(),
        })
    }

    async fn execute(
        &self,
        _context: Arc<dyn ToolContext>,
        arguments: Value,
    ) -> adk_core::Result<Value> {
        validate_argument_size(&arguments)?;
        let arguments = match &arguments {
            Value::Null => Map::new(),
            Value::Object(arguments) => arguments.clone(),
            _ => return Err(invalid_arguments()),
        };
        match self.kind {
            BigQueryToolKind::GetDocuments => self.get_documents(&arguments).await,
            BigQueryToolKind::BatchSearch => self.batch_search(&arguments).await,
            BigQueryToolKind::CreateVectorIndex => self.create_vector_index(&arguments).await,
            BigQueryToolKind::JobStats => self.job_stats(&arguments).await,
            BigQueryToolKind::SimilaritySearchByVector => {
                reject_unknown_keys(&arguments, &["embedding", "k"])?;
                let embedding = required_vector(&arguments, "embedding")?;
                let k = k_argument(&arguments)?;
                let rows = self.nearest(&embedding, None, k).await?;
                Ok(Value::String(python_dumps(&Cell::List(rows))))
            }
            BigQueryToolKind::SimilaritySearchByVectorWithScore => {
                reject_unknown_keys(&arguments, &["embedding", "filter", "k"])?;
                let embedding = required_vector(&arguments, "embedding")?;
                let filter = where_clause(arguments.get("filter"), false)?;
                let k = k_argument(&arguments)?;
                let rows = self.nearest(&embedding, Some(&filter), k).await?;
                Ok(Value::String(python_dumps(&Cell::List(rows))))
            }
            BigQueryToolKind::SimilaritySearchByVectors => {
                self.similarity_search_by_vectors(&arguments).await
            }
            BigQueryToolKind::CreateDeltaLakeTable => {
                self.create_delta_lake_table(&arguments).await
            }
        }
    }
}

impl BigQueryTool {
    async fn get_documents(&self, arguments: &Map<String, Value>) -> adk_core::Result<Value> {
        reject_unknown_keys(arguments, &["ids", "filter"])?;
        let ids = optional_string_list(arguments, "ids", MAX_IDS, MAX_ID_BYTES)?;
        let filter = where_clause(arguments.get("filter"), false)?;
        let table_id = self.table_id()?;
        let (id_expr, parameters) = match ids {
            Some(ids) if !ids.is_empty() => (
                "doc_id IN UNNEST(@ids)",
                vec![string_array_parameter("ids", &ids)],
            ),
            _ => ("TRUE", Vec::new()),
        };
        let rows = self
            .rows(QueryJob {
                sql: format!("SELECT * FROM `{table_id}` WHERE {id_expr} AND ({filter})"),
                parameters,
                effect: false,
            })
            .await?;
        Ok(Value::String(python_dumps(&Cell::List(rows))))
    }

    async fn batch_search(&self, arguments: &Map<String, Value>) -> adk_core::Result<Value> {
        reject_unknown_keys(arguments, &["queries", "embeddings", "k", "filter"])?;
        let queries = arguments.get("queries").filter(|value| !value.is_null());
        let embeddings = arguments.get("embeddings").filter(|value| !value.is_null());
        if queries.is_some() && embeddings.is_some() {
            return Err(tool_error(
                "bigquery.arguments.conflict",
                "Provide only one of 'queries' or 'embeddings'.",
            ));
        }
        if let Some(queries) = queries {
            if !queries.is_array() {
                return Err(invalid_arguments());
            }
            // The SDK embeds text through `self.embedding`, which its toolkit
            // never configures.
            return Err(tool_error(
                "bigquery.embedding.unavailable",
                "Embedding model is not set on the wrapper.",
            ));
        }
        let embeddings = match embeddings {
            Some(_) => vectors(arguments, "embeddings")?,
            None => Vec::new(),
        };
        if embeddings.is_empty() {
            return Err(tool_error(
                "bigquery.arguments.missing",
                "No embeddings or queries provided.",
            ));
        }
        let k = k_argument(arguments)?;
        let filter = where_clause(arguments.get("filter"), true)?;
        let mut results = Vec::with_capacity(embeddings.len());
        for embedding in &embeddings {
            results.push(Cell::List(self.nearest(embedding, Some(&filter), k).await?));
        }
        Ok(Value::String(python_dumps(&Cell::List(results))))
    }

    async fn similarity_search_by_vectors(
        &self,
        arguments: &Map<String, Value>,
    ) -> adk_core::Result<Value> {
        reject_unknown_keys(
            arguments,
            &[
                "embeddings",
                "filter",
                "k",
                "with_embeddings",
                "with_scores",
            ],
        )?;
        let embeddings = vectors(arguments, "embeddings")?;
        let filter = where_clause(arguments.get("filter"), false)?;
        let k = k_argument(arguments)?;
        let with_scores = optional_bool(arguments, "with_scores")?.unwrap_or(false);
        let with_embeddings = optional_bool(arguments, "with_embeddings")?.unwrap_or(false);
        let mut results = Vec::with_capacity(embeddings.len());
        for embedding in &embeddings {
            let mut rows = self.nearest(embedding, Some(&filter), k).await?;
            // The SDK's three branches: rows as read; `{**d, "score": ...}`,
            // which every row already carries; or the query vector in place of
            // the embedding column. Both flags set falls through unchanged.
            if with_embeddings && !with_scores {
                let vector = Cell::List(embedding.iter().copied().map(Cell::Float).collect());
                for row in &mut rows {
                    row.set_field("embedding", vector.clone());
                }
            }
            results.push(Cell::List(rows));
        }
        Ok(Value::String(python_dumps(&Cell::List(results))))
    }

    async fn create_vector_index(&self, arguments: &Map<String, Value>) -> adk_core::Result<Value> {
        reject_unknown_keys(arguments, &[])?;
        let table_id = self.table_id()?;
        let table = self.defaults.table.as_deref().unwrap_or_default();
        let index_name = format!("{table}_langchain_index");
        let sql = format!(
            "CREATE VECTOR INDEX IF NOT EXISTS\n`{index_name}`\nON `{table_id}`\n(embedding)\nOPTIONS(distance_type=\"EUCLIDEAN\", index_type=\"IVF\")"
        );
        self.rows(QueryJob {
            sql,
            parameters: Vec::new(),
            effect: true,
        })
        .await?;
        Ok(Value::String(format!(
            "Vector index '{index_name}' created or already exists."
        )))
    }

    async fn job_stats(&self, arguments: &Map<String, Value>) -> adk_core::Result<Value> {
        reject_unknown_keys(arguments, &["job_id"])?;
        let job_id = required_string(arguments, "job_id", MAX_JOB_ID_BYTES)?;
        if !job_id
            .bytes()
            .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'_' | b'-'))
        {
            return Err(invalid_arguments());
        }
        self.client
            .job_statistics(job_id)
            .await
            .map_err(BigQueryClientError::into_adk)
    }

    async fn create_delta_lake_table(
        &self,
        arguments: &Map<String, Value>,
    ) -> adk_core::Result<Value> {
        reject_unknown_keys(
            arguments,
            &[
                "table_name",
                "connection_id",
                "source_uris",
                "dataset",
                "project",
                "autodetect",
            ],
        )?;
        let table_name = optional_string(arguments, "table_name", 1_024)?.unwrap_or_default();
        let connection_id = optional_string(arguments, "connection_id", MAX_CONNECTION_ID_BYTES)?
            .unwrap_or_default();
        let source_uris = optional_string_list(
            arguments,
            "source_uris",
            MAX_SOURCE_URIS,
            MAX_SOURCE_URI_BYTES,
        )?
        .unwrap_or_default();
        let dataset = optional_string(arguments, "dataset", 1_024)?
            .filter(|value| !value.is_empty())
            .map(str::to_owned)
            .or_else(|| self.defaults.dataset.clone())
            .unwrap_or_default();
        let project = optional_string(arguments, "project", 128)?
            .filter(|value| !value.is_empty())
            .map(str::to_owned)
            .or_else(|| self.defaults.project.clone())
            .unwrap_or_default();
        let autodetect = optional_bool(arguments, "autodetect")?.unwrap_or(true);
        if project.is_empty()
            || dataset.is_empty()
            || table_name.is_empty()
            || connection_id.is_empty()
            || source_uris.is_empty()
        {
            return Err(tool_error(
                "bigquery.arguments.missing",
                "project, dataset, table_name, connection_id, and source_uris are required.",
            ));
        }
        if !valid_project(&project)
            || !valid_dataset(&dataset)
            || !valid_table(table_name)
            || !valid_connection_id(connection_id)
            || source_uris.iter().any(|uri| !valid_gcs_uri(uri))
        {
            return Err(invalid_arguments());
        }
        let table = json!({
            "tableReference": {
                "projectId": project,
                "datasetId": dataset,
                "tableId": table_name
            },
            "externalDataConfiguration": {
                "sourceFormat": "DELTA_LAKE",
                "autodetect": autodetect,
                "sourceUris": source_uris,
                "connectionId": connection_id
            }
        });
        let created = self
            .client
            .create_table(&project, &dataset, &table)
            .await
            .map_err(BigQueryClientError::into_adk)?;
        Ok(Value::String(python_dumps(&Cell::Json(created))))
    }
}

/// The SDK's WHERE text: `_create_filters` (numbers and booleans bare,
/// everything else quoted) or, for `batch_search`, every value quoted.
///
/// Rust repairs what the SDK leaves raw. An object filter is built from
/// validated column paths and properly escaped `GoogleSQL` string literals,
/// so a quote in a value cannot end the literal. A string filter is still
/// pasted as SQL text (the SDK contract), so it is screened by
/// [`screen_raw_filter`] first; every caller then wraps the result in
/// parentheses before splicing it after `AND` or `WHERE`.
pub(in crate::toolkits) fn where_clause(
    filter: Option<&Value>,
    quote_all: bool,
) -> adk_core::Result<String> {
    match filter {
        None | Some(Value::Null) => Ok("TRUE".to_owned()),
        Some(Value::String(text)) if text.is_empty() => Ok("TRUE".to_owned()),
        Some(Value::String(text)) => {
            if text.len() > MAX_FILTER_BYTES {
                return Err(resource_exhausted());
            }
            screen_raw_filter(text).map_err(|refusal| {
                tool_error(
                    "bigquery.filter.invalid",
                    format!("the filter was refused: {refusal}"),
                )
            })?;
            Ok(text.clone())
        }
        Some(Value::Object(fields)) if fields.is_empty() => Ok("TRUE".to_owned()),
        Some(Value::Object(fields)) => {
            if fields.len() > MAX_FILTER_FIELDS {
                return Err(resource_exhausted());
            }
            let mut expressions = Vec::with_capacity(fields.len());
            for (field, value) in fields {
                if !valid_field_path(field) {
                    return Err(tool_error(
                        "bigquery.filter.invalid",
                        "a filter key must be a column name or a dotted field path",
                    ));
                }
                let literal = match value {
                    Value::Number(number) if !quote_all => number_text(number)?,
                    Value::Bool(flag) if !quote_all => python_bool(*flag).to_owned(),
                    Value::Number(number) => string_literal(&number_text(number)?),
                    Value::Bool(flag) => string_literal(python_bool(*flag)),
                    Value::String(text) => string_literal(text),
                    _ => {
                        return Err(tool_error(
                            "bigquery.filter.invalid",
                            "a filter value must be a string, number or boolean",
                        ));
                    }
                };
                expressions.push(format!("{field} = {literal}"));
            }
            let clause = expressions.join(" AND ");
            if clause.len() > MAX_FILTER_BYTES {
                return Err(resource_exhausted());
            }
            Ok(clause)
        }
        Some(_) => Err(invalid_arguments()),
    }
}

/// Keywords a WHERE condition never needs and a prompt-injected filter uses
/// to read another table, open a subquery or run another statement.
const REFUSED_FILTER_KEYWORDS: [&str; 16] = [
    "SELECT", "UNION", "WITH", "INSERT", "UPDATE", "DELETE", "MERGE", "CREATE", "DROP", "ALTER",
    "CALL", "EXECUTE", "DECLARE", "EXPORT", "LOAD", "FROM",
];

/// Screens a caller-written `GoogleSQL` condition before it is pasted into a
/// query.
///
/// This is a defence against prompt-injected filters, not a SQL parser: a
/// small lexer skips string literals (`'..'`, `".."`, triple-quoted, with
/// `r`/`b` prefixes and backslash escapes) and backtick identifiers, and
/// outside them refuses `;`, comments (`--`, `#`, `/* */`), unbalanced
/// parentheses (which could close the parentheses the caller is wrapped in)
/// and the statement or subquery keywords in [`REFUSED_FILTER_KEYWORDS`] as
/// whole words. A condition that passes can still be any boolean expression
/// over the configured table's columns; it cannot name another table.
fn screen_raw_filter(text: &str) -> Result<(), String> {
    let bytes = text.as_bytes();
    let mut index = 0;
    let mut depth: usize = 0;
    while index < bytes.len() {
        let byte = bytes[index];
        match byte {
            0 => return Err("a NUL character".to_owned()),
            b';' => return Err("';' outside a string literal".to_owned()),
            b'#' => return Err("a '#' comment".to_owned()),
            b'-' if bytes.get(index + 1) == Some(&b'-') => {
                return Err("a '--' comment".to_owned());
            }
            b'/' if bytes.get(index + 1) == Some(&b'*') => {
                return Err("a '/* */' comment".to_owned());
            }
            b'(' => {
                depth += 1;
                index += 1;
            }
            b')' => {
                depth = depth
                    .checked_sub(1)
                    .ok_or_else(|| "an unbalanced ')'".to_owned())?;
                index += 1;
            }
            b'\'' | b'"' | b'`' => index = skip_quoted(bytes, index, false)?,
            byte if byte.is_ascii_alphanumeric() || byte == b'_' => {
                let start = index;
                while index < bytes.len()
                    && (bytes[index].is_ascii_alphanumeric() || bytes[index] == b'_')
                {
                    index += 1;
                }
                let word = &text[start..index];
                // A string-literal prefix: r'..', b"..", rb'..', br'..'.
                if matches!(bytes.get(index), Some(b'\'' | b'"'))
                    && word.len() <= 2
                    && word
                        .bytes()
                        .all(|byte| matches!(byte, b'r' | b'R' | b'b' | b'B'))
                {
                    let raw = word.bytes().any(|byte| matches!(byte, b'r' | b'R'));
                    index = skip_quoted(bytes, index, raw)?;
                    continue;
                }
                // `1FROM` lexes as a number then a keyword in some dialects.
                let bare = word.trim_start_matches(|character: char| {
                    character.is_ascii_digit() || character == '.'
                });
                if let Some(keyword) = REFUSED_FILTER_KEYWORDS.iter().find(|keyword| {
                    word.eq_ignore_ascii_case(keyword) || bare.eq_ignore_ascii_case(keyword)
                }) {
                    return Err(format!("the keyword {keyword} outside a string literal"));
                }
            }
            _ => index += 1,
        }
    }
    if depth == 0 {
        Ok(())
    } else {
        Err("an unbalanced '('".to_owned())
    }
}

/// Skips the literal or quoted identifier opening at `start` and returns the
/// index just past its closing quote.
fn skip_quoted(bytes: &[u8], start: usize, raw: bool) -> Result<usize, String> {
    let quote = bytes[start];
    let triple = quote != b'`'
        && bytes.get(start + 1) == Some(&quote)
        && bytes.get(start + 2) == Some(&quote);
    let mut index = start + if triple { 3 } else { 1 };
    while index < bytes.len() {
        let byte = bytes[index];
        if byte == b'\\' {
            if raw {
                // Whether a raw literal's `\'` ends it is exactly the kind of
                // lexer disagreement an injection would ride on: refuse it.
                if bytes.get(index + 1) == Some(&quote) {
                    return Err("a backslash before the closing quote of a raw string".to_owned());
                }
                index += 1;
            } else {
                index += 2;
            }
            continue;
        }
        if byte == quote {
            if !triple {
                return Ok(index + 1);
            }
            if bytes.get(index + 1) == Some(&quote) && bytes.get(index + 2) == Some(&quote) {
                return Ok(index + 3);
            }
        }
        if byte == 0 {
            return Err("a NUL character".to_owned());
        }
        if !triple && quote != b'`' && matches!(byte, b'\n' | b'\r') {
            return Err("a line break inside a single-line string literal".to_owned());
        }
        index += 1;
    }
    Err(if quote == b'`' {
        "an unterminated quoted identifier".to_owned()
    } else {
        "an unterminated string literal".to_owned()
    })
}

const fn python_bool(flag: bool) -> &'static str {
    if flag { "True" } else { "False" }
}

/// `str(int)` / `str(float)` of the value pydantic decoded.
fn number_text(number: &serde_json::Number) -> adk_core::Result<String> {
    if let Some(integer) = number.as_i64() {
        return Ok(integer.to_string());
    }
    if let Some(integer) = number.as_u64() {
        return Ok(integer.to_string());
    }
    number
        .as_f64()
        .filter(|value| value.is_finite())
        .map(python_float)
        .ok_or_else(invalid_arguments)
}

/// A `GoogleSQL` single-quoted string literal.
fn string_literal(value: &str) -> String {
    let mut out = String::with_capacity(value.len() + 2);
    out.push('\'');
    for character in value.chars() {
        match character {
            '\'' => out.push_str("\\'"),
            '\\' => out.push_str("\\\\"),
            '\n' => out.push_str("\\n"),
            '\r' => out.push_str("\\r"),
            '\t' => out.push_str("\\t"),
            control if control.is_control() => {
                let _ = write!(out, "\\u{:04x}", u32::from(control));
            }
            other => out.push(other),
        }
    }
    out.push('\'');
    out
}

fn valid_field_path(value: &str) -> bool {
    !value.is_empty()
        && value.len() <= MAX_FIELD_PATH_BYTES
        && value.split('.').all(|part| {
            let mut bytes = part.bytes();
            bytes
                .next()
                .is_some_and(|first| first.is_ascii_alphabetic() || first == b'_')
                && bytes.all(|byte| byte.is_ascii_alphanumeric() || byte == b'_')
        })
}

fn valid_connection_id(value: &str) -> bool {
    !value.is_empty()
        && value.len() <= MAX_CONNECTION_ID_BYTES
        && value.bytes().all(|byte| {
            byte.is_ascii_alphanumeric() || matches!(byte, b'-' | b'.' | b':' | b'_' | b'/')
        })
}

fn valid_gcs_uri(value: &str) -> bool {
    value.len() > "gs://".len()
        && value.starts_with("gs://")
        && !value.chars().any(char::is_control)
}

fn float_array_parameter(name: &str, values: &[f64]) -> Value {
    json!({
        "name": name,
        "parameterType": {"type": "ARRAY", "arrayType": {"type": "FLOAT64"}},
        "parameterValue": {
            "arrayValues": values
                .iter()
                .map(|value| json!({"value": python_float(*value)}))
                .collect::<Vec<_>>()
        }
    })
}

fn string_array_parameter(name: &str, values: &[&str]) -> Value {
    json!({
        "name": name,
        "parameterType": {"type": "ARRAY", "arrayType": {"type": "STRING"}},
        "parameterValue": {
            "arrayValues": values.iter().map(|value| json!({"value": value})).collect::<Vec<_>>()
        }
    })
}

fn object_schema(title: &str, properties: &Value, required: &[&str]) -> Value {
    let mut schema = json!({
        "title": title,
        "type": "object",
        "properties": properties,
        "additionalProperties": false
    });
    if !required.is_empty() {
        schema["required"] = json!(required);
    }
    schema
}

fn filter_schema() -> Value {
    json!({
        "anyOf": [
            {"type": "object", "maxProperties": MAX_FILTER_FIELDS, "additionalProperties": {"type": ["string", "number", "boolean"]}},
            {"type": "string", "maxLength": MAX_FILTER_BYTES},
            {"type": "null"}
        ],
        "default": null,
        "description": "Filter as an object of column: value equality pairs (joined with AND) or one GoogleSQL WHERE condition over this table's columns (no ';', comments, subqueries or other statements), for example \"category = 'news' AND year > 2020\"."
    })
}

fn k_schema() -> Value {
    json!({
        "type": "integer",
        "minimum": 0,
        "maximum": MAX_K,
        "default": DEFAULT_K,
        "description": "Number of top results to return per vector, at most 1000."
    })
}

fn embedding_schema() -> Value {
    json!({
        "type": "array",
        "minItems": 1,
        "maxItems": MAX_EMBEDDING_DIMENSIONS,
        "items": {"type": "number"},
        "description": "Embedding vector with the dimension of the table's embedding column."
    })
}

fn embeddings_schema() -> Value {
    json!({
        "type": "array",
        "maxItems": MAX_EMBEDDINGS,
        "items": {
            "type": "array",
            "minItems": 1,
            "maxItems": MAX_EMBEDDING_DIMENSIONS,
            "items": {"type": "number"}
        },
        "description": "Embedding vectors, at most 32, each with the dimension of the table's embedding column."
    })
}

fn create_delta_lake_table_schema() -> Value {
    object_schema(
        "CreateDeltaLakeTable",
        &json!({
            "table_name": {
                "type": "string",
                "minLength": 1,
                "maxLength": 1_024,
                "description": "Name of the Delta Lake external table to create in BigQuery."
            },
            "connection_id": {
                "type": "string",
                "minLength": 1,
                "maxLength": MAX_CONNECTION_ID_BYTES,
                "description": "Fully qualified BigLake connection ID (project.region.connection_id)."
            },
            "source_uris": {
                "type": "array",
                "minItems": 1,
                "maxItems": MAX_SOURCE_URIS,
                "items": {"type": "string", "maxLength": MAX_SOURCE_URI_BYTES, "pattern": "^gs://"},
                "description": "Cloud Storage URIs (gs:// prefixes) of the Delta Lake table."
            },
            "dataset": {
                "anyOf": [{"type": "string", "maxLength": 1_024}, {"type": "null"}],
                "default": null,
                "description": "BigQuery dataset to contain the table (defaults to the configured dataset)."
            },
            "project": {
                "anyOf": [{"type": "string", "maxLength": 128}, {"type": "null"}],
                "default": null,
                "description": "Google Cloud project ID (defaults to the configured project)."
            },
            "autodetect": {
                "type": "boolean",
                "default": true,
                "description": "Whether BigQuery autodetects the schema (default true)."
            }
        }),
        &["table_name", "connection_id", "source_uris"],
    )
}

fn validate_argument_size(arguments: &Value) -> adk_core::Result<()> {
    if serde_json::to_vec(arguments)
        .map_err(|_| invalid_arguments())?
        .len()
        > MAX_ARGUMENT_BYTES
    {
        return Err(resource_exhausted());
    }
    Ok(())
}

fn reject_unknown_keys(arguments: &Map<String, Value>, allowed: &[&str]) -> adk_core::Result<()> {
    if arguments.keys().any(|key| !allowed.contains(&key.as_str())) {
        return Err(invalid_arguments());
    }
    Ok(())
}

fn required_string<'a>(
    arguments: &'a Map<String, Value>,
    name: &str,
    limit: usize,
) -> adk_core::Result<&'a str> {
    optional_string(arguments, name, limit)?
        .filter(|value| !value.is_empty())
        .ok_or_else(invalid_arguments)
}

fn optional_string<'a>(
    arguments: &'a Map<String, Value>,
    name: &str,
    limit: usize,
) -> adk_core::Result<Option<&'a str>> {
    match arguments.get(name) {
        None | Some(Value::Null) => Ok(None),
        Some(Value::String(value)) if value.len() > limit => Err(resource_exhausted()),
        Some(Value::String(value)) => Ok(Some(value)),
        Some(_) => Err(invalid_arguments()),
    }
}

fn optional_bool(arguments: &Map<String, Value>, name: &str) -> adk_core::Result<Option<bool>> {
    match arguments.get(name) {
        None | Some(Value::Null) => Ok(None),
        Some(Value::Bool(value)) => Ok(Some(*value)),
        Some(_) => Err(invalid_arguments()),
    }
}

fn optional_string_list<'a>(
    arguments: &'a Map<String, Value>,
    name: &str,
    max_items: usize,
    max_bytes: usize,
) -> adk_core::Result<Option<Vec<&'a str>>> {
    let values = match arguments.get(name) {
        None | Some(Value::Null) => return Ok(None),
        Some(Value::Array(values)) => values,
        Some(_) => return Err(invalid_arguments()),
    };
    if values.len() > max_items {
        return Err(resource_exhausted());
    }
    values
        .iter()
        .map(|value| match value {
            Value::String(text) if text.len() > max_bytes => Err(resource_exhausted()),
            Value::String(text) => Ok(text.as_str()),
            _ => Err(invalid_arguments()),
        })
        .collect::<adk_core::Result<Vec<_>>>()
        .map(Some)
}

fn k_argument(arguments: &Map<String, Value>) -> adk_core::Result<u64> {
    match arguments.get("k") {
        None | Some(Value::Null) => Ok(DEFAULT_K),
        Some(Value::Number(number)) => {
            let k = number.as_u64().ok_or_else(invalid_arguments)?;
            if k > MAX_K {
                return Err(resource_exhausted());
            }
            Ok(k)
        }
        Some(_) => Err(invalid_arguments()),
    }
}

fn vector(value: &Value) -> adk_core::Result<Vec<f64>> {
    let values = value.as_array().ok_or_else(invalid_arguments)?;
    if values.is_empty() {
        return Err(invalid_arguments());
    }
    if values.len() > MAX_EMBEDDING_DIMENSIONS {
        return Err(resource_exhausted());
    }
    values
        .iter()
        .map(|value| {
            value
                .as_f64()
                .filter(|value| value.is_finite())
                .ok_or_else(invalid_arguments)
        })
        .collect()
}

fn required_vector(arguments: &Map<String, Value>, name: &str) -> adk_core::Result<Vec<f64>> {
    vector(arguments.get(name).ok_or_else(invalid_arguments)?)
}

fn vectors(arguments: &Map<String, Value>, name: &str) -> adk_core::Result<Vec<Vec<f64>>> {
    let values = arguments
        .get(name)
        .and_then(Value::as_array)
        .ok_or_else(invalid_arguments)?;
    if values.len() > MAX_EMBEDDINGS {
        return Err(resource_exhausted());
    }
    values.iter().map(vector).collect()
}

fn tool_error(code: &'static str, message: impl Into<String>) -> AdkError {
    AdkError::new(
        ErrorComponent::Tool,
        ErrorCategory::InvalidInput,
        code,
        message,
    )
}

fn invalid_arguments() -> AdkError {
    tool_error(
        "bigquery.arguments.invalid",
        "the BigQuery tool arguments are invalid",
    )
}

fn resource_exhausted() -> AdkError {
    tool_error(
        "bigquery.arguments.resource_exhausted",
        "the BigQuery tool arguments exceed the approved limit",
    )
}

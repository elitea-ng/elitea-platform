//! The three tools of the SDK's `delta_lake` toolkit, in its order:
//! `query_table`, `vector_search`, `get_table_schema`. All read-only.

use std::fmt;
use std::sync::Arc;
use std::sync::atomic::{AtomicBool, Ordering};

use adk_core::{AdkError, ErrorCategory, ErrorComponent, Tool, ToolContext};
use adk_tool::BasicToolset;
use async_trait::async_trait;
use serde_json::{Map, Value, json};

use crate::toolkits::families::integer_like::integer_like;
use crate::toolkits::invocation::{MaterializedToolsetError, admit_materialized_toolset};
use crate::toolkits::policy::ToolAdmissionPolicy;

use super::config::{DeltaLakeConfigError, DeltaLakeConfigErrorCode, DeltaLakeToolkitConfig};
use super::expr::Query;
use super::filter::RowFilter;
use super::table::{
    DeltaTable, MAX_EMBEDDING_DIMENSIONS, MAX_K, MAX_RESULT_BYTES, MAX_RESULT_ROWS, QueryRequest,
    RowSet, TableError, VectorRequest,
};

const QUERY_TABLE: &str = "query_table";
const VECTOR_SEARCH: &str = "vector_search";
const GET_TABLE_SCHEMA: &str = "get_table_schema";
const MAX_DESCRIPTION_BYTES: usize = 1_000;
/// The toolkit name in a description, cut so the usage text always fits.
const MAX_DESCRIBED_NAME_BYTES: usize = 60;
const MAX_ARGUMENT_BYTES: usize = 256 * 1_024;
const MAX_COLUMNS: usize = 256;
const MAX_COLUMN_NAME_BYTES: usize = 512;
const DEFAULT_K: i64 = 5;
const DEFAULT_EMBEDDING_COLUMN: &str = "embedding";

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum DeltaLakeToolsetErrorCode {
    InvalidConfiguration,
    ResourceExhausted,
    Client,
    InvalidDefinition,
}

/// Safe construction failure for the Delta Lake family.
pub(crate) struct DeltaLakeToolsetError {
    code: DeltaLakeToolsetErrorCode,
}

impl fmt::Debug for DeltaLakeToolsetError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("DeltaLakeToolsetError")
            .field("code", &self.code)
            .finish_non_exhaustive()
    }
}

impl fmt::Display for DeltaLakeToolsetError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(match self.code {
            DeltaLakeToolsetErrorCode::InvalidConfiguration => {
                "the Delta Lake toolkit configuration is invalid"
            }
            DeltaLakeToolsetErrorCode::ResourceExhausted => {
                "the Delta Lake toolkit configuration exceeds its approved limit"
            }
            DeltaLakeToolsetErrorCode::Client => "the Delta Lake client could not be created",
            DeltaLakeToolsetErrorCode::InvalidDefinition => {
                "the Delta Lake ADK tool definition is invalid"
            }
        })
    }
}

impl std::error::Error for DeltaLakeToolsetError {}

impl From<DeltaLakeConfigError> for DeltaLakeToolsetError {
    fn from(source: DeltaLakeConfigError) -> Self {
        Self {
            code: match source.code() {
                DeltaLakeConfigErrorCode::InvalidConfiguration => {
                    DeltaLakeToolsetErrorCode::InvalidConfiguration
                }
                DeltaLakeConfigErrorCode::ResourceExhausted => {
                    DeltaLakeToolsetErrorCode::ResourceExhausted
                }
            },
        }
    }
}

impl From<MaterializedToolsetError> for DeltaLakeToolsetError {
    fn from(_: MaterializedToolsetError) -> Self {
        Self {
            code: DeltaLakeToolsetErrorCode::InvalidDefinition,
        }
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
enum Kind {
    QueryTable,
    VectorSearch,
    GetTableSchema,
}

/// The SDK's order.
const ALL: [Kind; 3] = [Kind::QueryTable, Kind::VectorSearch, Kind::GetTableSchema];

impl Kind {
    const fn name(self) -> &'static str {
        match self {
            Self::QueryTable => QUERY_TABLE,
            Self::VectorSearch => VECTOR_SEARCH,
            Self::GetTableSchema => GET_TABLE_SCHEMA,
        }
    }

    const fn action(self) -> &'static str {
        match self {
            Self::QueryTable => {
                "Query Delta Lake table. Supports pandas-like filtering, column selection, and SQL-like queries (pandas query syntax, literals only: comparisons, and/or/not, in, is null, arithmetic; no function calls). At most 1000 rows are returned; a trailing _truncated entry says more matched. A read that scans too much fails: narrow it.\nArgs:\n    query: SQL-like query string (pandas.DataFrame.query syntax)\n    columns: List of columns to select\n    filters: Dict of column: condition pairs (value, list, null, or eq/ne/in/gt/gte/lt/lte/is_null)\nReturns:\n    List of dicts representing rows"
            }
            Self::VectorSearch => {
                "Perform a vector similarity search on the Delta Lake table.\nArgs:\n    embedding: Query embedding vector.\n    k: Number of top results to return (at most 100).\n    embedding_column: Name of the column containing embeddings.\nReturns:\n    List of dicts for top k most similar rows (cosine similarity, best first)."
            }
            Self::GetTableSchema => "Get the schema of the Delta Lake table.",
        }
    }

    fn schema(self) -> Value {
        match self {
            Self::QueryTable => json!({
                "title": "QueryTableArgs",
                "type": "object",
                "properties": {
                    "query": {
                        "anyOf": [{"type": "string"}, {"type": "null"}],
                        "default": null,
                        "description": "SQL query to execute on Delta Lake table. If None, returns all data. Pandas query syntax, literals only (comparisons, and/or/not, in, is null).",
                        "title": "Query"
                    },
                    "columns": {
                        "anyOf": [{"type": "array", "items": {"type": "string"}}, {"type": "null"}],
                        "default": null,
                        "description": "List of columns to select.",
                        "title": "Columns"
                    },
                    "filters": {
                        "anyOf": [{"type": "object", "additionalProperties": true}, {"type": "null"}],
                        "default": null,
                        "description": "Dict of column: condition pairs. A condition is a value (equality), a list (in), null (is null), or an object of operators eq, ne, in, gt, gte, lt, lte, is_null.",
                        "title": "Filters"
                    }
                }
            }),
            Self::VectorSearch => json!({
                "title": "VectorSearchArgs",
                "type": "object",
                "properties": {
                    "embedding": {
                        "type": "array",
                        "items": {"type": "number"},
                        "description": "Embedding vector for similarity search.",
                        "title": "Embedding"
                    },
                    "k": {
                        "type": "integer",
                        "default": DEFAULT_K,
                        "description": "Number of top results to return.",
                        "title": "K"
                    },
                    "embedding_column": {
                        "anyOf": [{"type": "string"}, {"type": "null"}],
                        "default": DEFAULT_EMBEDDING_COLUMN,
                        "description": "Name of the column containing embeddings.",
                        "title": "Embedding Column"
                    }
                },
                "required": ["embedding"]
            }),
            Self::GetTableSchema => json!({
                "title": "NoInput",
                "type": "object",
                "properties": {}
            }),
        }
    }
}

/// Build the Delta Lake toolset for a materialized toolkit.
pub(crate) fn build_delta_lake_toolset(
    toolkit_name: &str,
    config: &DeltaLakeToolkitConfig,
    policy: &Arc<ToolAdmissionPolicy>,
) -> Result<BasicToolset, DeltaLakeToolsetError> {
    let table = DeltaTable::connect(config).map_err(|_| DeltaLakeToolsetError {
        code: DeltaLakeToolsetErrorCode::Client,
    })?;
    let selected = config
        .selected_tools()
        .iter()
        .map(ToString::to_string)
        .collect::<Vec<_>>();
    build_with_table(
        toolkit_name,
        &selected,
        policy,
        &Arc::new(table),
        &config.description_prefix(),
    )
}

pub(in crate::toolkits) fn build_with_table(
    toolkit_name: &str,
    selected: &[String],
    policy: &Arc<ToolAdmissionPolicy>,
    table: &Arc<DeltaTable>,
    prefix: &str,
) -> Result<BasicToolset, DeltaLakeToolsetError> {
    let tools: Vec<Arc<dyn Tool>> = ALL
        .iter()
        .filter(|kind| selected.is_empty() || selected.iter().any(|name| name == kind.name()))
        .map(|kind| {
            Arc::new(DeltaLakeTool::new(
                *kind,
                toolkit_name,
                prefix,
                Arc::clone(table),
            )) as Arc<dyn Tool>
        })
        .collect();
    admit_materialized_toolset(toolkit_name, "delta_lake", policy, tools).map_err(Into::into)
}

/// Every tool this family serves, with whether it needs the run's file store
/// (`materialize::omitted_selection` reads it for the run's notice).
pub(crate) fn tool_catalog() -> Vec<(&'static str, bool)> {
    ALL.iter().map(|kind| (kind.name(), false)).collect()
}

struct DeltaLakeTool {
    kind: Kind,
    table: Arc<DeltaTable>,
    description: Box<str>,
}

impl DeltaLakeTool {
    fn new(kind: Kind, toolkit_name: &str, prefix: &str, table: Arc<DeltaTable>) -> Self {
        // The SDK: `S3 Path: .. Table Path: ..\nToolkit: <name>\n<doc>`, cut at
        // 1000 characters.
        let name = truncate_utf8(toolkit_name, MAX_DESCRIBED_NAME_BYTES);
        let description = format!("{prefix}\nToolkit: {name}\n{}", kind.action());
        Self {
            kind,
            table,
            description: truncate_utf8(&description, MAX_DESCRIPTION_BYTES)
                .to_owned()
                .into_boxed_str(),
        }
    }
}

fn truncate_utf8(value: &str, max_bytes: usize) -> &str {
    if value.len() <= max_bytes {
        return value;
    }
    let mut end = max_bytes;
    while end != 0 && !value.is_char_boundary(end) {
        end -= 1;
    }
    &value[..end]
}

#[async_trait]
impl Tool for DeltaLakeTool {
    fn name(&self) -> &str {
        self.kind.name()
    }

    fn description(&self) -> &str {
        &self.description
    }

    fn is_read_only(&self) -> bool {
        true
    }

    fn is_concurrency_safe(&self) -> bool {
        true
    }

    fn parameters_schema(&self) -> Option<Value> {
        Some(self.kind.schema())
    }

    /// A failure is the tool's answer, as in the SDK (its action returns
    /// `Error: ...` text): the runtime reduces a raised tool error to its
    /// category, which would hide the sentence that says what to change (the
    /// column that is missing, the token a query is refused at). The text is
    /// this family's own, bounded and free of credentials.
    async fn execute(
        &self,
        _context: Arc<dyn ToolContext>,
        arguments: Value,
    ) -> adk_core::Result<Value> {
        Ok(self
            .run(arguments)
            .await
            .unwrap_or_else(|error| Value::String(format!("Error: {}", error.message))))
    }
}

impl DeltaLakeTool {
    async fn run(&self, arguments: Value) -> adk_core::Result<Value> {
        let arguments = arguments_object(arguments)?;
        let table = Arc::clone(&self.table);
        match self.kind {
            Kind::QueryTable => {
                let request = parse_query(&arguments)?;
                let rows = run_blocking(move |cancel| table.query(&request, cancel)).await?;
                Ok(rows_value(rows))
            }
            Kind::VectorSearch => {
                let request = parse_vector(&arguments)?;
                let rows =
                    run_blocking(move |cancel| table.vector_search(&request, cancel)).await?;
                Ok(rows_value(rows))
            }
            Kind::GetTableSchema => {
                let schema = run_blocking(move |_| table.schema_text()).await?;
                Ok(Value::String(schema))
            }
        }
    }
}

/// Sets the flag the blocking read checks between batches when the call is
/// dropped (the turn was cancelled or timed out), so the thread stops at the
/// next batch instead of reading on for nobody.
struct CancelOnDrop(Arc<AtomicBool>);

impl Drop for CancelOnDrop {
    fn drop(&mut self) {
        self.0.store(true, Ordering::Relaxed);
    }
}

async fn run_blocking<T: Send + 'static>(
    work: impl FnOnce(&AtomicBool) -> Result<T, TableError> + Send + 'static,
) -> adk_core::Result<T> {
    let cancel = Arc::new(AtomicBool::new(false));
    let _guard = CancelOnDrop(Arc::clone(&cancel));
    tokio::task::spawn_blocking(move || work(&cancel))
        .await
        .map_err(|_| {
            AdkError::new(
                ErrorComponent::Tool,
                ErrorCategory::Internal,
                "delta_lake.read.aborted",
                "reading the Delta Lake table was interrupted",
            )
        })?
        .map_err(|error| {
            AdkError::new(
                ErrorComponent::Tool,
                ErrorCategory::Unavailable,
                "delta_lake.read.failed",
                error.message().to_owned(),
            )
        })
}

fn rows_value(set: RowSet) -> Value {
    let mut rows: Vec<Value> = set.rows.into_iter().map(Value::Object).collect();
    if set.truncated {
        rows.push(json!({
            "_truncated": true,
            "_message": format!(
                "More rows matched than one answer carries (at most {MAX_RESULT_ROWS} rows and {} KiB). Narrow the read with columns and filters.",
                MAX_RESULT_BYTES / 1_024
            ),
        }));
    }
    Value::Array(rows)
}

fn arguments_object(arguments: Value) -> adk_core::Result<Map<String, Value>> {
    match arguments {
        Value::Null => Ok(Map::new()),
        Value::Object(map) => {
            let size = serde_json::to_vec(&map).map_or(usize::MAX, |bytes| bytes.len());
            if size > MAX_ARGUMENT_BYTES {
                return Err(invalid(
                    "the Delta Lake tool arguments exceed the approved limit",
                ));
            }
            Ok(map)
        }
        _ => Err(invalid("the Delta Lake tool arguments must be an object")),
    }
}

fn parse_query(arguments: &Map<String, Value>) -> adk_core::Result<QueryRequest> {
    let expression = match arguments.get("query") {
        None | Some(Value::Null) => None,
        Some(Value::String(query)) if query.trim().is_empty() => None,
        Some(Value::String(query)) => Some(Query::parse(query).map_err(invalid)?),
        Some(_) => return Err(invalid("query must be a string")),
    };
    let columns = match arguments.get("columns") {
        None | Some(Value::Null) => None,
        Some(Value::Array(values)) => {
            if values.len() > MAX_COLUMNS {
                return Err(invalid(format!(
                    "columns lists {} names; at most {MAX_COLUMNS} are allowed.",
                    values.len()
                )));
            }
            let mut names: Vec<String> = Vec::with_capacity(values.len());
            for value in values {
                let name = value
                    .as_str()
                    .filter(|name| !name.is_empty() && name.len() <= MAX_COLUMN_NAME_BYTES)
                    .ok_or_else(|| invalid("columns must be a list of column names"))?;
                if !names.iter().any(|existing| existing == name) {
                    names.push(name.to_owned());
                }
            }
            // The SDK's `if columns:` treats an empty list as no selection.
            (!names.is_empty()).then_some(names)
        }
        Some(_) => return Err(invalid("columns must be a list of column names")),
    };
    let filter = RowFilter::parse(arguments.get("filters")).map_err(invalid)?;
    Ok(QueryRequest {
        columns,
        filter,
        expression,
    })
}

fn parse_vector(arguments: &Map<String, Value>) -> adk_core::Result<VectorRequest> {
    let values = arguments
        .get("embedding")
        .and_then(Value::as_array)
        .ok_or_else(|| invalid("embedding must be a list of numbers"))?;
    if values.is_empty() || values.len() > MAX_EMBEDDING_DIMENSIONS {
        return Err(invalid(format!(
            "embedding must have between 1 and {MAX_EMBEDDING_DIMENSIONS} values."
        )));
    }
    let embedding = values
        .iter()
        .map(|value| value.as_f64().filter(|number| number.is_finite()))
        .collect::<Option<Vec<f64>>>()
        .ok_or_else(|| invalid("embedding must be a list of numbers"))?;
    let k = match arguments.get("k") {
        None | Some(Value::Null) => DEFAULT_K,
        Some(value) => integer_like(value).ok_or_else(|| invalid("k must be an integer"))?,
    };
    if k < 1 {
        return Err(invalid("k must be at least 1"));
    }
    let k = usize::try_from(k).unwrap_or(MAX_K).min(MAX_K);
    let column = match arguments.get("embedding_column") {
        None | Some(Value::Null) => DEFAULT_EMBEDDING_COLUMN.to_owned(),
        Some(Value::String(name)) if !name.is_empty() && name.len() <= MAX_COLUMN_NAME_BYTES => {
            name.clone()
        }
        Some(_) => return Err(invalid("embedding_column must be a column name")),
    };
    Ok(VectorRequest {
        embedding,
        k,
        column,
    })
}

fn invalid(message: impl Into<String>) -> AdkError {
    AdkError::new(
        ErrorComponent::Tool,
        ErrorCategory::InvalidInput,
        "delta_lake.arguments.invalid",
        message.into(),
    )
}

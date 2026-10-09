use std::fmt;
use std::sync::Arc;

use adk_core::{AdkError, ErrorCategory, ErrorComponent, Tool, ToolContext};
use adk_tool::BasicToolset;
use async_trait::async_trait;
use serde_json::{Map, Value, json};

use crate::toolkits::invocation::{MaterializedToolsetError, admit_materialized_toolset};
use crate::toolkits::policy::ToolAdmissionPolicy;

use super::client::{SqlApi, SqlClient, SqlClientError};
use super::config::{SqlConfigError, SqlToolkitConfig};
use super::lexer::{MAX_SQL_BYTES, SqlLexError, admit_one_statement};

const MAX_DESCRIPTION_BYTES: usize = 2_000;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum SqlToolsetErrorCode {
    InvalidConfiguration,
    ResourceExhausted,
    UnsupportedSelection,
    InvalidDefinition,
}

pub(crate) struct SqlToolsetError {
    code: SqlToolsetErrorCode,
}

impl SqlToolsetError {
    #[must_use]
    pub(crate) const fn code(&self) -> SqlToolsetErrorCode {
        self.code
    }
}

impl fmt::Debug for SqlToolsetError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("SqlToolsetError")
            .field("code", &self.code)
            .finish_non_exhaustive()
    }
}

impl fmt::Display for SqlToolsetError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(match self.code {
            SqlToolsetErrorCode::InvalidConfiguration => "the SQL toolkit configuration is invalid",
            SqlToolsetErrorCode::ResourceExhausted => {
                "the SQL toolkit configuration exceeds its approved limit"
            }
            SqlToolsetErrorCode::UnsupportedSelection => {
                "the selected SQL tool profile is not supported"
            }
            SqlToolsetErrorCode::InvalidDefinition => "the SQL ADK tool definition is invalid",
        })
    }
}

impl std::error::Error for SqlToolsetError {}

impl From<SqlConfigError> for SqlToolsetError {
    fn from(source: SqlConfigError) -> Self {
        Self {
            code: match source.code() {
                super::config::SqlConfigErrorCode::InvalidConfiguration => {
                    SqlToolsetErrorCode::InvalidConfiguration
                }
                super::config::SqlConfigErrorCode::ResourceExhausted => {
                    SqlToolsetErrorCode::ResourceExhausted
                }
            },
        }
    }
}

impl From<MaterializedToolsetError> for SqlToolsetError {
    fn from(_: MaterializedToolsetError) -> Self {
        Self {
            code: SqlToolsetErrorCode::InvalidDefinition,
        }
    }
}

/// Build the complete capability-disabled two-tool SQL family.
#[allow(clippy::needless_pass_by_value)] // Consumes one invocation's credential authority.
pub(crate) fn build_sql_toolset(
    toolkit_name: &str,
    config: SqlToolkitConfig,
    policy: &Arc<ToolAdmissionPolicy>,
) -> Result<BasicToolset, SqlToolsetError> {
    validate_selection(config.selected_tools())?;
    let selected = config
        .selected_tools()
        .iter()
        .map(ToString::to_string)
        .collect::<Vec<_>>();
    let dialect = config.dialect();
    let client: Arc<dyn SqlApi> = Arc::new(SqlClient::new(config));
    build_with_api(toolkit_name, &selected, dialect, policy, &client)
}

fn validate_selection(selected: &[Box<str>]) -> Result<(), SqlToolsetError> {
    if selected.iter().any(|name| {
        !SqlToolKind::ALL
            .iter()
            .any(|kind| kind.name() == name.as_ref())
    }) {
        return Err(SqlToolsetError {
            code: SqlToolsetErrorCode::UnsupportedSelection,
        });
    }
    Ok(())
}

fn build_with_api(
    toolkit_name: &str,
    selected: &[String],
    dialect: super::config::SqlDialect,
    policy: &Arc<ToolAdmissionPolicy>,
    client: &Arc<dyn SqlApi>,
) -> Result<BasicToolset, SqlToolsetError> {
    let include_all = selected.is_empty();
    let tools = SqlToolKind::ALL
        .into_iter()
        .filter(|kind| include_all || selected.iter().any(|name| name == kind.name()))
        .map(|kind| {
            Arc::new(SqlTool::new(
                kind,
                toolkit_name,
                dialect,
                Arc::clone(client),
            )) as Arc<dyn Tool>
        })
        .collect();
    admit_materialized_toolset(toolkit_name, "sql", policy, tools).map_err(Into::into)
}

#[derive(Clone, Copy)]
enum SqlToolKind {
    ExecuteSql,
    ListTablesAndColumns,
}

impl SqlToolKind {
    const ALL: [Self; 2] = [Self::ExecuteSql, Self::ListTablesAndColumns];

    const fn name(self) -> &'static str {
        match self {
            Self::ExecuteSql => "execute_sql",
            Self::ListTablesAndColumns => "list_tables_and_columns",
        }
    }

    const fn group(self) -> &'static str {
        match self {
            Self::ExecuteSql => "execute",
            Self::ListTablesAndColumns => "read",
        }
    }

    const fn is_read_only(self) -> bool {
        matches!(self, Self::ListTablesAndColumns)
    }

    const fn description(self) -> &'static str {
        match self {
            Self::ExecuteSql => {
                "Execute exactly one PostgreSQL or MySQL statement, up to 65536 UTF-8 bytes. A single terminal semicolon is allowed; transaction/session-control statements are rejected. PostgreSQL standard strings and E-strings use the verified fixed standard-conforming mode; MySQL preserves the server session SQL mode and admits backslash-escaped strings only after verifying that NO_BACKSLASH_ESCAPES is absent. The statement may read, write, or delete data, so authorization follows the requested SQL. Row results are deterministic JSON objects bounded to 1000 rows, 64 columns per row, 64 KiB per cell, and 512 KiB total; binary cells are base64 objects, exact decimals are strings, and unsupported provider types must be cast to text or JSON. A non-row statement returns executed=true and rows_affected. This is a remote effect: after dispatch, timeout, cancellation, commit, decoding, or output-bound failures have unknown outcome. Do not retry; reconcile database state first."
            }
            Self::ListTablesAndColumns => {
                "List regular tables and columns in the configured database's current PostgreSQL schema or current MySQL database. Returns a deterministic object keyed by table name with table_name and table_columns entries. Results are bounded to 512 tables, 4096 columns, 256 bytes per metadata value, and 512 KiB total; narrow the database schema if the bound is exceeded. This read does not execute model-provided SQL, but independent platform sensitivity policy may still require approval."
            }
        }
    }

    fn schema(self) -> Value {
        match self {
            Self::ExecuteSql => json!({
                "type": "object",
                "properties": {
                    "sql_query": {
                        "type": "string",
                        "minLength": 1,
                        "maxLength": MAX_SQL_BYTES,
                        "description": "Exactly one PostgreSQL or MySQL statement, for example SELECT id, name FROM products ORDER BY id LIMIT 20. The complete UTF-8 statement is limited to 65536 bytes; it may have remote effects even when it returns rows."
                    }
                },
                "required": ["sql_query"],
                "additionalProperties": false
            }),
            Self::ListTablesAndColumns => json!({
                "type": "object",
                "properties": {},
                "additionalProperties": false
            }),
        }
    }
}

struct SqlTool {
    kind: SqlToolKind,
    dialect: super::config::SqlDialect,
    client: Arc<dyn SqlApi>,
    description: Box<str>,
}

impl SqlTool {
    fn new(
        kind: SqlToolKind,
        toolkit_name: &str,
        dialect: super::config::SqlDialect,
        client: Arc<dyn SqlApi>,
    ) -> Self {
        let description = format!("Toolkit: {toolkit_name}\n{}", kind.description());
        Self {
            kind,
            dialect,
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
impl Tool for SqlTool {
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
        Some(self.kind.schema())
    }

    async fn execute(
        &self,
        _context: Arc<dyn ToolContext>,
        arguments: Value,
    ) -> adk_core::Result<Value> {
        let arguments = arguments.as_object().ok_or_else(invalid_arguments)?;
        match self.kind {
            SqlToolKind::ExecuteSql => {
                reject_unknown_keys(arguments, &["sql_query"])?;
                let sql = arguments
                    .get("sql_query")
                    .and_then(Value::as_str)
                    .ok_or_else(invalid_arguments)?;
                let admitted = admit_one_statement(sql, self.dialect).map_err(map_lex_error)?;
                self.client
                    .execute_sql(admitted.as_str())
                    .await
                    .map_err(SqlClientError::into_adk)
            }
            SqlToolKind::ListTablesAndColumns => {
                reject_unknown_keys(arguments, &[])?;
                self.client
                    .list_tables_and_columns()
                    .await
                    .map_err(SqlClientError::into_adk)
            }
        }
    }
}

fn reject_unknown_keys(arguments: &Map<String, Value>, allowed: &[&str]) -> Result<(), AdkError> {
    if arguments.keys().any(|key| !allowed.contains(&key.as_str())) {
        return Err(invalid_arguments());
    }
    Ok(())
}

fn map_lex_error(error: SqlLexError) -> AdkError {
    match error {
        SqlLexError::ResourceExhausted => AdkError::new(
            ErrorComponent::Tool,
            ErrorCategory::InvalidInput,
            "sql.statement.resource_exhausted",
            "the SQL statement exceeds the approved limit",
        ),
        SqlLexError::Invalid
        | SqlLexError::MultipleStatements
        | SqlLexError::TransactionControl => invalid_arguments(),
    }
}

fn invalid_arguments() -> AdkError {
    AdkError::new(
        ErrorComponent::Tool,
        ErrorCategory::InvalidInput,
        "sql.arguments.invalid",
        "the SQL tool arguments are invalid",
    )
}

#[cfg(test)]
pub(in crate::toolkits) fn test_build_with_api(
    toolkit_name: &str,
    selected: &[String],
    dialect: super::config::SqlDialect,
    policy: &Arc<ToolAdmissionPolicy>,
    client: &Arc<dyn SqlApi>,
) -> Result<BasicToolset, SqlToolsetError> {
    build_with_api(toolkit_name, selected, dialect, policy, client)
}

#[cfg(test)]
pub(in crate::toolkits) fn test_catalog() -> Vec<(&'static str, &'static str)> {
    SqlToolKind::ALL
        .into_iter()
        .map(|kind| (kind.name(), kind.group()))
        .collect()
}

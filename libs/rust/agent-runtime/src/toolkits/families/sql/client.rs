use std::fmt;
use std::time::Duration;

use adk_core::{AdkError, ErrorCategory, ErrorComponent, RetryHint};
use async_trait::async_trait;
use serde_json::{Map, Value, json};
use sqlx::mysql::{MySqlConnectOptions, MySqlConnection, MySqlSslMode};
use sqlx::postgres::{PgConnectOptions, PgConnection, PgSslMode};
use sqlx::{Acquire, ConnectOptions, Connection, Executor, MySql, Row, Statement};
use tokio_stream::StreamExt;

use super::config::{SqlDialect, SqlToolkitConfig};
use super::project::{
    ProjectErrorCode, project_mysql_row, project_pg_row, validate_mysql_columns,
    validate_pg_columns,
};

const CONNECT_TIMEOUT: Duration = Duration::from_secs(10);
const PREPARE_TIMEOUT: Duration = Duration::from_secs(10);
const STATEMENT_TIMEOUT: Duration = Duration::from_secs(30);
const INTROSPECTION_TIMEOUT: Duration = Duration::from_secs(15);
const CLOSE_TIMEOUT: Duration = Duration::from_secs(5);
const MAX_ROWS: usize = 1_000;
const MAX_TABLES: usize = 512;
const MAX_SCHEMA_COLUMNS: usize = 4_096;
const MAX_METADATA_BYTES: usize = 256;
const MAX_OUTPUT_BYTES: usize = 512 * 1_024;
const MAX_MYSQL_SESSION_MODE_BYTES: usize = 8 * 1_024;

/// The `MySQL` session must be TLS with verified identity before authentication.
/// This also keeps the non-TLS RSA password-encryption path of `sqlx-mysql`
/// (`rsa`, RUSTSEC-2023-0071, no fixed release) unreachable: a non-TLS server
/// is refused before any authentication byte is sent.
const MYSQL_TLS_MODE: MySqlSslMode = MySqlSslMode::VerifyIdentity;

const POSTGRES_SCHEMA_SQL: &str = r"
SELECT cls.relname AS table_name,
       attr.attname AS column_name,
       pg_catalog.format_type(attr.atttypid, attr.atttypmod) AS column_type
FROM pg_catalog.pg_attribute AS attr
JOIN pg_catalog.pg_class AS cls ON cls.oid = attr.attrelid
JOIN pg_catalog.pg_namespace AS ns ON ns.oid = cls.relnamespace
WHERE ns.nspname = current_schema()
  AND cls.relkind IN ('r', 'p')
  AND attr.attnum > 0
  AND NOT attr.attisdropped
ORDER BY cls.relname, attr.attnum
";

const MYSQL_SCHEMA_SQL: &str = r"
SELECT table_name, column_name, column_type
FROM information_schema.columns
WHERE table_schema = DATABASE()
  AND table_name IN (
      SELECT table_name
      FROM information_schema.tables
      WHERE table_schema = DATABASE() AND table_type = 'BASE TABLE'
  )
ORDER BY table_name, ordinal_position
";
const MYSQL_SESSION_MODE_QUERY: &str = "SELECT @@SESSION.sql_mode";

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum SqlClientErrorCode {
    InvalidConfiguration,
    InvalidInput,
    Authentication,
    Authorization,
    NotFound,
    Timeout,
    DependencyUnavailable,
    InvalidResponse,
    ResourceExhausted,
    UnsupportedType,
    UnknownOutcome,
}

/// Stable provider failure without host, database, statement, row, or secret.
pub(crate) struct SqlClientError {
    code: SqlClientErrorCode,
    retryable: bool,
}

impl SqlClientError {
    #[must_use]
    pub(crate) const fn code(&self) -> SqlClientErrorCode {
        self.code
    }

    #[must_use]
    pub(crate) const fn retryable(&self) -> bool {
        self.retryable
    }

    pub(crate) fn into_adk(self) -> AdkError {
        let (category, code, message) = match self.code {
            SqlClientErrorCode::InvalidConfiguration => (
                ErrorCategory::InvalidInput,
                "sql.configuration.invalid",
                "the SQL toolkit configuration is invalid",
            ),
            SqlClientErrorCode::InvalidInput => (
                ErrorCategory::InvalidInput,
                "sql.statement.invalid",
                "the SQL statement is invalid or unsupported",
            ),
            SqlClientErrorCode::Authentication => (
                ErrorCategory::Unauthorized,
                "sql.authentication.failed",
                "database authentication failed",
            ),
            SqlClientErrorCode::Authorization => (
                ErrorCategory::Forbidden,
                "sql.authorization.failed",
                "the database did not authorize the operation",
            ),
            SqlClientErrorCode::NotFound => (
                ErrorCategory::NotFound,
                "sql.database.not_found",
                "the configured database was not found",
            ),
            SqlClientErrorCode::Timeout => (
                ErrorCategory::Timeout,
                "sql.timeout",
                "the database operation timed out",
            ),
            SqlClientErrorCode::DependencyUnavailable => (
                ErrorCategory::Unavailable,
                "sql.unavailable",
                "the database is unavailable",
            ),
            SqlClientErrorCode::InvalidResponse => (
                ErrorCategory::Internal,
                "sql.response.invalid",
                "the database returned an invalid response",
            ),
            SqlClientErrorCode::ResourceExhausted => (
                ErrorCategory::InvalidInput,
                "sql.resource_exhausted",
                "the SQL request or result exceeds the approved limit",
            ),
            SqlClientErrorCode::UnsupportedType => (
                ErrorCategory::InvalidInput,
                "sql.column_type.unsupported",
                "a result column type is unsupported; cast it to a supported text or JSON type",
            ),
            SqlClientErrorCode::UnknownOutcome => (
                ErrorCategory::Internal,
                "sql.effect.unknown_outcome",
                "the SQL statement may have applied effects; reconcile them before retrying",
            ),
        };
        AdkError::new(ErrorComponent::Tool, category, code, message).with_retry(RetryHint {
            should_retry: self.retryable,
            retry_after_ms: None,
            max_attempts: None,
        })
    }

    #[cfg(test)]
    pub(in crate::toolkits) const fn fixture(code: SqlClientErrorCode, retryable: bool) -> Self {
        Self { code, retryable }
    }
}

impl fmt::Debug for SqlClientError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("SqlClientError")
            .field("code", &self.code)
            .field("retryable", &self.retryable)
            .finish_non_exhaustive()
    }
}

impl fmt::Display for SqlClientError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(match self.code {
            SqlClientErrorCode::InvalidConfiguration => "the SQL client configuration is invalid",
            SqlClientErrorCode::InvalidInput => "the SQL statement is invalid",
            SqlClientErrorCode::Authentication => "database authentication failed",
            SqlClientErrorCode::Authorization => "database authorization failed",
            SqlClientErrorCode::NotFound => "the configured database was not found",
            SqlClientErrorCode::Timeout => "the database operation timed out",
            SqlClientErrorCode::DependencyUnavailable => "the database is unavailable",
            SqlClientErrorCode::InvalidResponse => "the database returned an invalid response",
            SqlClientErrorCode::ResourceExhausted => {
                "the SQL request or result exceeds its approved limit"
            }
            SqlClientErrorCode::UnsupportedType => "a database column type is unsupported",
            SqlClientErrorCode::UnknownOutcome => "the SQL effect outcome is unknown",
        })
    }
}

impl std::error::Error for SqlClientError {}

#[async_trait]
pub(in crate::toolkits) trait SqlApi: Send + Sync {
    async fn execute_sql(&self, sql: &str) -> Result<Value, SqlClientError>;
    async fn list_tables_and_columns(&self) -> Result<Value, SqlClientError>;
}

pub(crate) struct SqlClient {
    config: SqlToolkitConfig,
}

impl SqlClient {
    #[must_use]
    pub(crate) const fn new(config: SqlToolkitConfig) -> Self {
        Self { config }
    }

    fn postgres_options(&self) -> PgConnectOptions {
        // SQLx 0.8.6 has no public fully environment-free PostgreSQL options
        // constructor. Every claim-provided ordinary field and behavior is
        // overwritten here; production activation remains gated on removing
        // the driver's ambient PG TLS-file seam.
        PgConnectOptions::new_without_pgpass()
            .host(self.config.host())
            .port(self.config.port())
            .username(self.config.username())
            .password(self.config.password())
            .database(self.config.database())
            .ssl_mode(PgSslMode::VerifyFull)
            .statement_cache_capacity(0)
            .application_name("elitea-worker-rust")
            .options([
                ("statement_timeout", "30000"),
                ("lock_timeout", "5000"),
                ("idle_in_transaction_session_timeout", "30000"),
                ("standard_conforming_strings", "on"),
            ])
            .disable_statement_logging()
    }

    fn mysql_options(&self) -> MySqlConnectOptions {
        MySqlConnectOptions::new()
            .host(self.config.host())
            .port(self.config.port())
            .username(self.config.username())
            .password(self.config.password())
            .database(self.config.database())
            .ssl_mode(MYSQL_TLS_MODE)
            .statement_cache_capacity(0)
            .charset("utf8mb4")
            .timezone(Some("+00:00".to_owned()))
            .pipes_as_concat(false)
            .no_engine_substitution(false)
            .disable_statement_logging()
    }
}

#[async_trait]
impl SqlApi for SqlClient {
    async fn execute_sql(&self, sql: &str) -> Result<Value, SqlClientError> {
        match self.config.dialect() {
            SqlDialect::Postgres => {
                let mut connection = connect_postgres(self.postgres_options()).await?;
                let result = execute_postgres(&mut connection, sql).await;
                close_connection(connection).await;
                result
            }
            SqlDialect::MySql => {
                let mut connection = connect_mysql(self.mysql_options()).await?;
                let result = match configure_mysql_session(&mut connection).await {
                    Ok(()) => execute_mysql(&mut connection, sql).await,
                    Err(error) => Err(error),
                };
                close_connection(connection).await;
                result
            }
        }
    }

    async fn list_tables_and_columns(&self) -> Result<Value, SqlClientError> {
        match self.config.dialect() {
            SqlDialect::Postgres => {
                let mut connection = connect_postgres(self.postgres_options()).await?;
                let result = introspect_postgres(&mut connection).await;
                close_connection(connection).await;
                result
            }
            SqlDialect::MySql => {
                let mut connection = connect_mysql(self.mysql_options()).await?;
                let result = introspect_mysql(&mut connection).await;
                close_connection(connection).await;
                result
            }
        }
    }
}

async fn connect_postgres(options: PgConnectOptions) -> Result<PgConnection, SqlClientError> {
    match tokio::time::timeout(CONNECT_TIMEOUT, PgConnection::connect_with(&options)).await {
        Ok(Ok(connection)) => Ok(connection),
        Ok(Err(error)) => Err(map_pre_dispatch_error(&error)),
        Err(_) => Err(timeout(true)),
    }
}

async fn connect_mysql(options: MySqlConnectOptions) -> Result<MySqlConnection, SqlClientError> {
    match tokio::time::timeout(CONNECT_TIMEOUT, MySqlConnection::connect_with(&options)).await {
        Ok(Ok(connection)) => Ok(connection),
        Ok(Err(error)) => Err(map_pre_dispatch_error(&error)),
        Err(_) => Err(timeout(true)),
    }
}

async fn execute_postgres(
    connection: &mut PgConnection,
    sql: &str,
) -> Result<Value, SqlClientError> {
    let mut transaction = begin_transaction(connection).await?;
    let statement = prepare_statement(&mut transaction, sql).await?;
    validate_pg_columns(statement.columns())
        .map_err(|error| map_project_read_error(error.code()))?;
    let returns_rows = !statement.columns().is_empty();
    let dispatched = async {
        let value = if returns_rows {
            let mut rows = statement.query().fetch(&mut *transaction);
            let mut output = Vec::new();
            let mut output_bytes = 2usize;
            while let Some(row) = rows.next().await {
                let row = row.map_err(|_| unknown_outcome())?;
                if output.len() >= MAX_ROWS {
                    return Err(unknown_outcome());
                }
                let row = Value::Object(project_pg_row(&row).map_err(map_project_effect_error)?);
                add_array_item_to_budget(&mut output_bytes, output.len(), &row)?;
                output.push(row);
            }
            drop(rows);
            Value::Array(output)
        } else {
            let result = statement
                .query()
                .execute(&mut *transaction)
                .await
                .map_err(|_| unknown_outcome())?;
            json!({"executed": true, "rows_affected": result.rows_affected()})
        };
        ensure_output_bound(&value, true)?;
        transaction.commit().await.map_err(|_| unknown_outcome())?;
        Ok(value)
    };
    tokio::time::timeout(STATEMENT_TIMEOUT, dispatched)
        .await
        .map_err(|_| unknown_outcome())?
}

async fn execute_mysql(
    connection: &mut MySqlConnection,
    sql: &str,
) -> Result<Value, SqlClientError> {
    let mut transaction = begin_transaction(connection).await?;
    let statement = prepare_statement(&mut transaction, sql).await?;
    validate_mysql_columns(statement.columns())
        .map_err(|error| map_project_read_error(error.code()))?;
    let returns_rows = !statement.columns().is_empty();
    let dispatched = async {
        let value = if returns_rows {
            let mut rows = statement.query().fetch(&mut *transaction);
            let mut output = Vec::new();
            let mut output_bytes = 2usize;
            while let Some(row) = rows.next().await {
                let row = row.map_err(|_| unknown_outcome())?;
                if output.len() >= MAX_ROWS {
                    return Err(unknown_outcome());
                }
                let row = Value::Object(project_mysql_row(&row).map_err(map_project_effect_error)?);
                add_array_item_to_budget(&mut output_bytes, output.len(), &row)?;
                output.push(row);
            }
            drop(rows);
            Value::Array(output)
        } else {
            let result = statement
                .query()
                .execute(&mut *transaction)
                .await
                .map_err(|_| unknown_outcome())?;
            json!({"executed": true, "rows_affected": result.rows_affected()})
        };
        ensure_output_bound(&value, true)?;
        transaction.commit().await.map_err(|_| unknown_outcome())?;
        Ok(value)
    };
    tokio::time::timeout(STATEMENT_TIMEOUT, dispatched)
        .await
        .map_err(|_| unknown_outcome())?
}

async fn begin_transaction<'a, C>(
    connection: &'a mut C,
) -> Result<sqlx::Transaction<'a, C::Database>, SqlClientError>
where
    C: Connection,
    for<'connection> &'connection mut C: Acquire<'connection, Database = C::Database>,
{
    match tokio::time::timeout(PREPARE_TIMEOUT, connection.begin()).await {
        Ok(Ok(transaction)) => Ok(transaction),
        Ok(Err(error)) => Err(map_pre_dispatch_error(&error)),
        Err(_) => Err(timeout(true)),
    }
}

async fn prepare_statement<DB>(
    transaction: &mut sqlx::Transaction<'_, DB>,
    sql: &str,
) -> Result<DB::Statement<'static>, SqlClientError>
where
    DB: sqlx::Database,
    for<'connection> &'connection mut DB::Connection: Executor<'connection, Database = DB>,
{
    match tokio::time::timeout(PREPARE_TIMEOUT, (&mut **transaction).prepare(sql)).await {
        Ok(Ok(statement)) => Ok(statement.to_owned()),
        Ok(Err(error)) => Err(map_prepare_error(&error)),
        Err(_) => Err(timeout(true)),
    }
}

async fn configure_mysql_session(connection: &mut MySqlConnection) -> Result<(), SqlClientError> {
    let mode = match tokio::time::timeout(
        PREPARE_TIMEOUT,
        sqlx::query_scalar::<MySql, String>(MYSQL_SESSION_MODE_QUERY)
            .persistent(false)
            .fetch_one(connection),
    )
    .await
    {
        Ok(Ok(mode)) => mode,
        Ok(Err(error)) => return Err(map_pre_dispatch_error(&error)),
        Err(_) => return Err(timeout(true)),
    };
    validate_mysql_session_mode(&mode)
}

fn validate_mysql_session_mode(mode: &str) -> Result<(), SqlClientError> {
    if mode.len() > MAX_MYSQL_SESSION_MODE_BYTES || mode.chars().any(char::is_control) {
        return Err(invalid_configuration());
    }
    if mode
        .split(',')
        .map(str::trim)
        .any(|value| value.eq_ignore_ascii_case("NO_BACKSLASH_ESCAPES"))
    {
        return Err(invalid_configuration());
    }
    Ok(())
}

async fn introspect_postgres(connection: &mut PgConnection) -> Result<Value, SqlClientError> {
    let operation = async {
        let mut stream = sqlx::query(POSTGRES_SCHEMA_SQL)
            .persistent(false)
            .fetch(connection);
        let mut builder = SchemaBuilder::new();
        while let Some(row) = stream.next().await {
            let row = row.map_err(|error| map_read_error(&error))?;
            let table = row
                .try_get::<String, _>("table_name")
                .map_err(|_| invalid_response())?;
            let column = row
                .try_get::<String, _>("column_name")
                .map_err(|_| invalid_response())?;
            let column_type = row
                .try_get::<String, _>("column_type")
                .map_err(|_| invalid_response())?;
            builder.push(&table, &column, &column_type)?;
        }
        builder.finish()
    };
    tokio::time::timeout(INTROSPECTION_TIMEOUT, operation)
        .await
        .map_err(|_| timeout(true))?
}

async fn introspect_mysql(connection: &mut MySqlConnection) -> Result<Value, SqlClientError> {
    let operation = async {
        let mut stream = sqlx::query(MYSQL_SCHEMA_SQL)
            .persistent(false)
            .fetch(connection);
        let mut builder = SchemaBuilder::new();
        while let Some(row) = stream.next().await {
            let row = row.map_err(|error| map_read_error(&error))?;
            let table = row
                .try_get::<String, _>("table_name")
                .map_err(|_| invalid_response())?;
            let column = row
                .try_get::<String, _>("column_name")
                .map_err(|_| invalid_response())?;
            let column_type = row
                .try_get::<String, _>("column_type")
                .map_err(|_| invalid_response())?;
            builder.push(&table, &column, &column_type)?;
        }
        builder.finish()
    };
    tokio::time::timeout(INTROSPECTION_TIMEOUT, operation)
        .await
        .map_err(|_| timeout(true))?
}

struct SchemaBuilder {
    tables: Map<String, Value>,
    columns: usize,
    output_bytes: usize,
}

impl SchemaBuilder {
    fn new() -> Self {
        Self {
            tables: Map::new(),
            columns: 0,
            output_bytes: 2,
        }
    }

    fn push(&mut self, table: &str, column: &str, column_type: &str) -> Result<(), SqlClientError> {
        validate_metadata(table)?;
        validate_metadata(column)?;
        validate_metadata(column_type)?;
        self.columns = self.columns.checked_add(1).ok_or_else(resource_exhausted)?;
        if self.columns > MAX_SCHEMA_COLUMNS {
            return Err(resource_exhausted());
        }
        if !self.tables.contains_key(table) {
            if self.tables.len() >= MAX_TABLES {
                return Err(resource_exhausted());
            }
            let table_value = json!({"table_name": table, "table_columns": []});
            let empty_entry =
                Value::Object(Map::from_iter([(table.to_owned(), table_value.clone())]));
            let entry_bytes = serde_json::to_vec(&empty_entry)
                .map_err(|_| invalid_response())?
                .len()
                .checked_sub(2)
                .ok_or_else(invalid_response)?;
            self.output_bytes = self
                .output_bytes
                .checked_add(entry_bytes + usize::from(!self.tables.is_empty()))
                .ok_or_else(resource_exhausted)?;
            if self.output_bytes > MAX_OUTPUT_BYTES {
                return Err(resource_exhausted());
            }
            self.tables.insert(table.to_owned(), table_value);
        }
        let columns = self
            .tables
            .get_mut(table)
            .and_then(Value::as_object_mut)
            .and_then(|table| table.get_mut("table_columns"))
            .and_then(Value::as_array_mut)
            .ok_or_else(invalid_response)?;
        let column = json!({"name": column, "type": column_type});
        let column_bytes = serde_json::to_vec(&column)
            .map_err(|_| invalid_response())?
            .len();
        self.output_bytes = self
            .output_bytes
            .checked_add(column_bytes + usize::from(!columns.is_empty()))
            .ok_or_else(resource_exhausted)?;
        if self.output_bytes > MAX_OUTPUT_BYTES {
            return Err(resource_exhausted());
        }
        columns.push(column);
        Ok(())
    }

    fn finish(self) -> Result<Value, SqlClientError> {
        let value = Value::Object(self.tables);
        ensure_output_bound(&value, false)?;
        Ok(value)
    }
}

fn validate_metadata(value: &str) -> Result<(), SqlClientError> {
    if value.is_empty() || value.len() > MAX_METADATA_BYTES || value.chars().any(char::is_control) {
        return Err(invalid_response());
    }
    Ok(())
}

fn ensure_output_bound(value: &Value, effect: bool) -> Result<(), SqlClientError> {
    let size = serde_json::to_vec(value)
        .map_err(|_| {
            if effect {
                unknown_outcome()
            } else {
                invalid_response()
            }
        })?
        .len();
    if size > MAX_OUTPUT_BYTES {
        return Err(if effect {
            unknown_outcome()
        } else {
            resource_exhausted()
        });
    }
    Ok(())
}

fn add_array_item_to_budget(
    current: &mut usize,
    existing_items: usize,
    value: &Value,
) -> Result<(), SqlClientError> {
    let item_bytes = serde_json::to_vec(value)
        .map_err(|_| unknown_outcome())?
        .len();
    *current = current
        .checked_add(item_bytes + usize::from(existing_items != 0))
        .ok_or_else(unknown_outcome)?;
    if *current > MAX_OUTPUT_BYTES {
        return Err(unknown_outcome());
    }
    Ok(())
}

fn map_project_effect_error(_: super::project::ProjectError) -> SqlClientError {
    unknown_outcome()
}

fn map_pre_dispatch_error(error: &sqlx::Error) -> SqlClientError {
    if let Some(database) = error.as_database_error() {
        return map_database_code(database.code().as_deref(), true);
    }
    match error {
        sqlx::Error::Configuration(_) | sqlx::Error::InvalidArgument(_) | sqlx::Error::Tls(_) => {
            invalid_configuration()
        }
        sqlx::Error::Io(_)
        | sqlx::Error::Protocol(_)
        | sqlx::Error::WorkerCrashed
        | sqlx::Error::PoolTimedOut
        | sqlx::Error::PoolClosed => dependency_unavailable(true),
        _ => invalid_response(),
    }
}

fn map_prepare_error(error: &sqlx::Error) -> SqlClientError {
    if let Some(database) = error.as_database_error() {
        let mapped = map_database_code(database.code().as_deref(), true);
        return if matches!(
            mapped.code,
            SqlClientErrorCode::Authentication
                | SqlClientErrorCode::Authorization
                | SqlClientErrorCode::NotFound
        ) {
            mapped
        } else {
            invalid_input()
        };
    }
    map_pre_dispatch_error(error)
}

fn map_read_error(error: &sqlx::Error) -> SqlClientError {
    if let Some(database) = error.as_database_error() {
        return map_database_code(database.code().as_deref(), true);
    }
    match error {
        sqlx::Error::Io(_) | sqlx::Error::WorkerCrashed => dependency_unavailable(true),
        sqlx::Error::Tls(_) => invalid_configuration(),
        _ => invalid_response(),
    }
}

fn map_database_code(code: Option<&str>, retryable_read: bool) -> SqlClientError {
    match code {
        Some("28P01" | "28000" | "1045") => authentication(),
        Some("42501" | "42000" | "1044" | "1142" | "1143") => authorization(),
        Some("3D000" | "1049") => not_found(),
        Some("40001" | "40P01" | "1205" | "1213") => dependency_unavailable(retryable_read),
        _ => invalid_response(),
    }
}

fn map_project_read_error(error: ProjectErrorCode) -> SqlClientError {
    match error {
        ProjectErrorCode::InvalidResponse => invalid_response(),
        ProjectErrorCode::ResourceExhausted => resource_exhausted(),
        ProjectErrorCode::UnsupportedType => unsupported_type(),
    }
}

async fn close_connection<C: Connection>(connection: C) {
    let _ = tokio::time::timeout(CLOSE_TIMEOUT, connection.close()).await;
}

const fn invalid_configuration() -> SqlClientError {
    SqlClientError {
        code: SqlClientErrorCode::InvalidConfiguration,
        retryable: false,
    }
}

const fn invalid_input() -> SqlClientError {
    SqlClientError {
        code: SqlClientErrorCode::InvalidInput,
        retryable: false,
    }
}

const fn authentication() -> SqlClientError {
    SqlClientError {
        code: SqlClientErrorCode::Authentication,
        retryable: false,
    }
}

const fn authorization() -> SqlClientError {
    SqlClientError {
        code: SqlClientErrorCode::Authorization,
        retryable: false,
    }
}

const fn not_found() -> SqlClientError {
    SqlClientError {
        code: SqlClientErrorCode::NotFound,
        retryable: false,
    }
}

const fn timeout(retryable: bool) -> SqlClientError {
    SqlClientError {
        code: SqlClientErrorCode::Timeout,
        retryable,
    }
}

const fn dependency_unavailable(retryable: bool) -> SqlClientError {
    SqlClientError {
        code: SqlClientErrorCode::DependencyUnavailable,
        retryable,
    }
}

const fn invalid_response() -> SqlClientError {
    SqlClientError {
        code: SqlClientErrorCode::InvalidResponse,
        retryable: false,
    }
}

const fn resource_exhausted() -> SqlClientError {
    SqlClientError {
        code: SqlClientErrorCode::ResourceExhausted,
        retryable: false,
    }
}

const fn unsupported_type() -> SqlClientError {
    SqlClientError {
        code: SqlClientErrorCode::UnsupportedType,
        retryable: false,
    }
}

const fn unknown_outcome() -> SqlClientError {
    SqlClientError {
        code: SqlClientErrorCode::UnknownOutcome,
        retryable: false,
    }
}

#[cfg(test)]
pub(in crate::toolkits) fn test_map_project_read_error(error: ProjectErrorCode) -> SqlClientError {
    map_project_read_error(error)
}

#[cfg(test)]
pub(in crate::toolkits) struct SqlConnectionProfile {
    pub(in crate::toolkits) host: String,
    pub(in crate::toolkits) port: u16,
    pub(in crate::toolkits) username: String,
    pub(in crate::toolkits) database: String,
    pub(in crate::toolkits) tls: &'static str,
}

#[cfg(test)]
pub(in crate::toolkits) fn test_connection_profile(
    config: SqlToolkitConfig,
) -> SqlConnectionProfile {
    let client = SqlClient::new(config);
    match client.config.dialect() {
        SqlDialect::Postgres => {
            let options = client.postgres_options();
            SqlConnectionProfile {
                host: options.get_host().to_owned(),
                port: options.get_port(),
                username: options.get_username().to_owned(),
                database: options.get_database().unwrap_or_default().to_owned(),
                tls: if matches!(options.get_ssl_mode(), PgSslMode::VerifyFull) {
                    "verify-full"
                } else {
                    "unexpected"
                },
            }
        }
        SqlDialect::MySql => {
            let options = client.mysql_options();
            SqlConnectionProfile {
                host: options.get_host().to_owned(),
                port: options.get_port(),
                username: options.get_username().to_owned(),
                database: options.get_database().unwrap_or_default().to_owned(),
                tls: if matches!(options.get_ssl_mode(), MySqlSslMode::VerifyIdentity) {
                    "verify-identity"
                } else {
                    "unexpected"
                },
            }
        }
    }
}

#[cfg(test)]
pub(in crate::toolkits) const fn test_schema_queries() -> (&'static str, &'static str) {
    (POSTGRES_SCHEMA_SQL, MYSQL_SCHEMA_SQL)
}

#[cfg(test)]
pub(in crate::toolkits) fn test_validate_mysql_session_mode(
    mode: &str,
) -> Result<(), SqlClientError> {
    validate_mysql_session_mode(mode)
}

#[cfg(test)]
pub(in crate::toolkits) fn test_schema_projection(
    rows: &[(String, String, String)],
) -> Result<Value, SqlClientError> {
    let mut builder = SchemaBuilder::new();
    for (table, column, column_type) in rows {
        builder.push(table, column, column_type)?;
    }
    builder.finish()
}

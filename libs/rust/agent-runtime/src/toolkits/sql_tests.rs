//! Focused compatibility, egress, TLS, and safety tests for the SQL toolkit family.

use std::collections::{BTreeMap, HashMap, VecDeque};
use std::io;
use std::net::{IpAddr, Ipv4Addr};
use std::sync::atomic::{AtomicUsize, Ordering};
use std::sync::{Arc, Mutex};

use adk_core::{ReadonlyContext, ToolContext, Toolset};
use adk_tool::SimpleToolContext;
use async_trait::async_trait;
use elitea_egress_guard::{
    Allowlist, Lookup, LookupFuture, MAX_RESOLVED_ADDRESSES, PermitSet, PublicPolicy,
};
use serde_json::{Map, Value, json};
use sqlx::ConnectOptions;
use sqlx::mysql::{MySqlConnectOptions, MySqlSslMode};
use sqlx::postgres::PgConnectOptions;
use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::net::{TcpListener, TcpStream};
use tokio::time::timeout;

use crate::egress::RunEgress;

use super::families::sql::client::{
    SqlApi, SqlClient, SqlClientError, SqlClientErrorCode, test_connection_profile,
    test_map_project_read_error, test_schema_projection, test_schema_queries,
    test_validate_mysql_session_mode,
};
use super::families::sql::config::{SqlConfigErrorCode, SqlDialect, SqlToolkitConfig};
use super::families::sql::lexer::{MAX_SQL_BYTES, SqlLexError, admit_one_statement};
use super::families::sql::project::{
    MAX_BINARY_BYTES, MAX_CELL_BYTES, ProjectErrorCode, test_binary_value, test_bounded_json,
    test_float_value, test_supported_types, test_validate_column_names,
};
use super::families::sql::tools::{
    SqlToolsetErrorCode, build_sql_toolset, test_build_with_api, test_catalog,
};
use super::policy::ToolAdmissionPolicy;

const PASSWORD: &str = "sql-private-password";

fn settings(dialect: Option<&str>, selected: Option<Value>) -> Map<String, Value> {
    let mut settings = json!({
        "database_name": "application_db",
        "sql_configuration": {
            "host": "database.example.test",
            "port": null,
            "username": "application_user",
            "password": PASSWORD
        }
    })
    .as_object()
    .cloned()
    .expect("SQL settings fixture is an object");
    if let Some(dialect) = dialect {
        settings.insert("dialect".to_owned(), Value::String(dialect.to_owned()));
    }
    if let Some(selected) = selected {
        settings.insert("selected_tools".to_owned(), selected);
    }
    settings
}

fn config(dialect: SqlDialect) -> SqlToolkitConfig {
    SqlToolkitConfig::parse(&settings(
        Some(match dialect {
            SqlDialect::Postgres => "postgres",
            SqlDialect::MySql => "mysql",
        }),
        Some(json!([])),
    ))
    .expect("valid SQL configuration fixture")
}

fn policy(blocked: &[(&str, &[&str])]) -> Arc<ToolAdmissionPolicy> {
    let blocked = blocked
        .iter()
        .map(|(toolkit, tools)| {
            (
                (*toolkit).to_owned(),
                tools.iter().map(|tool| (*tool).to_owned()).collect(),
            )
        })
        .collect::<BTreeMap<_, _>>();
    Arc::new(ToolAdmissionPolicy::new(&[], &blocked).expect("SQL policy fixture"))
}

fn context() -> Arc<dyn ToolContext> {
    Arc::new(SimpleToolContext::new("sql-test").with_function_call_id("sql-call"))
}

#[derive(Clone, Debug, Eq, PartialEq)]
enum FixtureCall {
    Execute(String),
    List,
}

struct FixtureApi {
    calls: Mutex<Vec<FixtureCall>>,
    responses: Mutex<VecDeque<Result<Value, SqlClientError>>>,
}

impl FixtureApi {
    fn new(responses: impl IntoIterator<Item = Result<Value, SqlClientError>>) -> Self {
        Self {
            calls: Mutex::new(Vec::new()),
            responses: Mutex::new(responses.into_iter().collect()),
        }
    }

    fn calls(&self) -> Vec<FixtureCall> {
        self.calls.lock().expect("SQL fixture calls").clone()
    }

    fn response(&self) -> Result<Value, SqlClientError> {
        self.responses
            .lock()
            .expect("SQL fixture responses")
            .pop_front()
            .unwrap_or_else(|| Ok(json!({"ok": true})))
    }
}

#[async_trait]
impl SqlApi for FixtureApi {
    async fn execute_sql(&self, sql: &str) -> Result<Value, SqlClientError> {
        self.calls
            .lock()
            .expect("SQL fixture calls")
            .push(FixtureCall::Execute(sql.to_owned()));
        self.response()
    }

    async fn list_tables_and_columns(&self) -> Result<Value, SqlClientError> {
        self.calls
            .lock()
            .expect("SQL fixture calls")
            .push(FixtureCall::List);
        self.response()
    }
}

#[test]
fn catalog_and_nested_configuration_preserve_source_contract() {
    assert_eq!(
        test_catalog(),
        vec![
            ("execute_sql", "execute"),
            ("list_tables_and_columns", "read")
        ]
    );

    for selected in [None, Some(Value::Null), Some(json!([]))] {
        let parsed = SqlToolkitConfig::parse(&settings(None, selected))
            .expect("missing dialect defaults to PostgreSQL and empty means all");
        assert_eq!(parsed.dialect(), SqlDialect::Postgres);
        assert_eq!(parsed.selected_tools(), []);
    }

    let parsed = SqlToolkitConfig::parse(&settings(
        Some("mysql"),
        Some(json!([
            "list_tables_and_columns",
            "execute_sql",
            "list_tables_and_columns"
        ])),
    ))
    .expect("MySQL and duplicate selection are valid");
    assert_eq!(parsed.dialect(), SqlDialect::MySql);
    assert_eq!(
        parsed
            .selected_tools()
            .iter()
            .map(AsRef::as_ref)
            .collect::<Vec<&str>>(),
        ["list_tables_and_columns", "execute_sql"]
    );

    let dial_ip = IpAddr::V4(Ipv4Addr::new(93, 184, 216, 34));
    let postgres = test_connection_profile(config(SqlDialect::Postgres), dial_ip);
    assert_eq!(postgres.host, "database.example.test");
    assert_eq!(postgres.port, 5432);
    assert_eq!(postgres.connect_addr, Some(dial_ip));
    assert_eq!(postgres.username, "application_user");
    assert_eq!(postgres.database, "application_db");
    assert_eq!(postgres.tls, "verify-full");
    assert!(!postgres.has_ssl_root_cert);
    assert!(!postgres.has_ssl_client_cert);
    assert!(!postgres.has_ssl_client_key);
    let mysql = test_connection_profile(config(SqlDialect::MySql), dial_ip);
    assert_eq!(mysql.host, "database.example.test");
    assert_eq!(mysql.port, 3306);
    assert_eq!(mysql.connect_addr, Some(dial_ip));
    assert_eq!(mysql.tls, "verify-identity");

    for (dialect, port) in [("postgres", 6432), ("mysql", 3307)] {
        let mut custom = settings(Some(dialect), Some(json!([])));
        custom
            .get_mut("sql_configuration")
            .and_then(Value::as_object_mut)
            .expect("nested SQL configuration")
            .insert("port".to_owned(), Value::from(port));
        let profile = test_connection_profile(
            SqlToolkitConfig::parse(&custom).expect("integer custom port is valid"),
            dial_ip,
        );
        assert_eq!(profile.port, port);
        assert_eq!(profile.connect_addr, Some(dial_ip));
    }
}

#[test]
fn invalid_configuration_is_bounded_and_secret_safe() {
    for invalid in [
        json!({}),
        json!({
            "dialect": "sqlite", "database_name": "db",
            "sql_configuration": {"host":"db.test","username":"u","password":"p"}
        }),
        json!({
            "dialect": "postgres", "database_name": "db",
            "sql_configuration": {"host":"https://db.test","username":"u","password":"p"}
        }),
        json!({
            "dialect": "mysql", "database_name": "db",
            "sql_configuration": {"host":"db.test","port":0,"username":"u","password":"p"}
        }),
        json!({
            "dialect": "mysql", "database_name": "db",
            "sql_configuration": {"host":"db.test","port":70000,"username":"u","password":"p"}
        }),
        json!({
            "dialect": "mysql", "database_name": "db",
            "sql_configuration": {"host":"db.test","username":"u","password":"bad\0secret"}
        }),
    ] {
        let Err(error) = SqlToolkitConfig::parse(
            invalid
                .as_object()
                .expect("invalid SQL settings remain an object"),
        ) else {
            panic!("invalid SQL configuration must fail");
        };
        assert_eq!(error.code(), SqlConfigErrorCode::InvalidConfiguration);
        let diagnostic = format!("{error:?} {error}");
        assert!(!diagnostic.contains("secret"));
        assert!(!diagnostic.contains("db.test"));
    }

    let mut oversized = settings(Some("postgres"), Some(json!([])));
    oversized
        .get_mut("sql_configuration")
        .and_then(Value::as_object_mut)
        .expect("nested SQL configuration")
        .insert(
            "password".to_owned(),
            Value::String("x".repeat(16 * 1_024 + 1)),
        );
    let Err(error) = SqlToolkitConfig::parse(&oversized) else {
        panic!("oversized SQL secret must fail");
    };
    assert_eq!(error.code(), SqlConfigErrorCode::ResourceExhausted);
    assert!(!format!("{error:?}").contains(&"x".repeat(100)));
}

#[test]
fn lexical_admission_is_single_statement_and_dialect_aware() {
    for sql in [
        "SELECT 1",
        "SELECT ';' AS marker; -- terminal",
        "SELECT 'C:\\\\temp'",
        "SELECT E'line\\nnext'",
        "DO $body$ BEGIN PERFORM 1; END $body$;",
        "SELECT 1 /* outer /* inner ; */ still outer */",
    ] {
        let admitted = admit_one_statement(sql, SqlDialect::Postgres)
            .unwrap_or_else(|_| panic!("PostgreSQL statement should be admitted: {sql}"));
        assert_eq!(admitted.as_str(), sql);
    }
    for sql in [
        "SELECT `semi;column` FROM `table`",
        "SELECT 'it\\\'s safe; inside'",
        "SELECT 1 # comment ; ignored\n;",
    ] {
        assert!(
            admit_one_statement(sql, SqlDialect::MySql).is_ok(),
            "MySQL statement should be admitted: {sql}"
        );
    }

    for (dialect, sql, expected) in [
        (SqlDialect::Postgres, "", SqlLexError::Invalid),
        (
            SqlDialect::Postgres,
            "SELECT 1; DELETE FROM users",
            SqlLexError::MultipleStatements,
        ),
        (
            SqlDialect::MySql,
            "SELECT 1 /* outer /* inner */; DROP TABLE users */",
            SqlLexError::MultipleStatements,
        ),
        (
            SqlDialect::MySql,
            "SELECT 'unterminated",
            SqlLexError::Invalid,
        ),
        (
            SqlDialect::MySql,
            "SELECT 1 /*! DROP TABLE users */",
            SqlLexError::Invalid,
        ),
        (
            SqlDialect::Postgres,
            "SELECT 1 /*+ hint */",
            SqlLexError::Invalid,
        ),
        (
            SqlDialect::Postgres,
            "COMMIT",
            SqlLexError::TransactionControl,
        ),
        (
            SqlDialect::MySql,
            "SET autocommit=1",
            SqlLexError::TransactionControl,
        ),
    ] {
        assert_eq!(admit_one_statement(sql, dialect).err(), Some(expected));
    }
    assert_eq!(
        admit_one_statement(&"x".repeat(MAX_SQL_BYTES + 1), SqlDialect::Postgres).err(),
        Some(SqlLexError::ResourceExhausted)
    );
}

#[tokio::test]
async fn metadata_and_schemas_are_exact() {
    let api = Arc::new(FixtureApi::new([]));
    let api_trait: Arc<dyn SqlApi> = api.clone();
    let toolset = test_build_with_api(
        "analytics",
        &[],
        SqlDialect::Postgres,
        &policy(&[]),
        &api_trait,
    )
    .expect("complete SQL fixture toolset");
    let readonly: Arc<dyn ReadonlyContext> = context();
    let tools = toolset.tools(readonly).await.expect("SQL tools");
    assert_eq!(
        tools.iter().map(|tool| tool.name()).collect::<Vec<_>>(),
        ["execute_sql", "list_tables_and_columns"]
    );
    assert!(!tools[0].is_read_only());
    assert!(!tools[0].is_concurrency_safe());
    assert!(tools[1].is_read_only());
    assert!(tools[1].is_concurrency_safe());

    let execute_description = tools[0].description();
    for cue in [
        "exactly one",
        "65536",
        "read, write, or delete",
        "1000 rows",
        "64 columns",
        "512 KiB",
        "rows_affected",
        "unknown outcome",
        "Do not retry",
        "NO_BACKSLASH_ESCAPES",
    ] {
        assert!(execute_description.contains(cue), "missing cue: {cue}");
    }
    assert!(!execute_description.contains("database.example.test"));
    assert!(!execute_description.contains("application_db"));
    assert!(!execute_description.contains(PASSWORD));
    let schema = tools[0].parameters_schema().expect("execute schema");
    assert_eq!(schema["required"], json!(["sql_query"]));
    assert_eq!(schema["properties"]["sql_query"]["minLength"], 1);
    assert_eq!(
        schema["properties"]["sql_query"]["maxLength"],
        MAX_SQL_BYTES
    );
    assert_eq!(schema["additionalProperties"], false);
    assert_eq!(
        tools[1].parameters_schema().expect("list schema"),
        json!({"type":"object","properties":{},"additionalProperties":false})
    );
    assert!(tools[1].description().contains("sensitivity policy"));
    assert!(tools[1].description().contains("require approval"));
}

#[test]
fn mysql_session_mode_is_preserved_and_lexer_assumptions_are_verified() {
    for mode in [
        "",
        "STRICT_TRANS_TABLES",
        "ONLY_FULL_GROUP_BY,STRICT_TRANS_TABLES,NO_ZERO_DATE",
        "NO_BACKSLASH_ESCAPES_EXTRA",
    ] {
        assert!(
            test_validate_mysql_session_mode(mode).is_ok(),
            "compatible server mode must be preserved: {mode}"
        );
    }
    for mode in [
        "NO_BACKSLASH_ESCAPES",
        "STRICT_TRANS_TABLES, NO_BACKSLASH_ESCAPES,ONLY_FULL_GROUP_BY",
        "no_backslash_escapes",
    ] {
        let Err(error) = test_validate_mysql_session_mode(mode) else {
            panic!("incompatible MySQL mode must fail before user SQL dispatch");
        };
        assert_eq!(error.code(), SqlClientErrorCode::InvalidConfiguration);
        assert!(!error.retryable());
    }
    assert!(test_validate_mysql_session_mode(&"A".repeat(8 * 1_024)).is_ok());
    let Err(error) = test_validate_mysql_session_mode(&"A".repeat(8 * 1_024 + 1)) else {
        panic!("oversized MySQL session mode must fail before user SQL dispatch");
    };
    assert_eq!(error.code(), SqlClientErrorCode::InvalidConfiguration);
}

#[tokio::test]
async fn selection_policy_and_construction_are_exact() {
    let api = Arc::new(FixtureApi::new([]));
    let api_trait: Arc<dyn SqlApi> = api;
    let selected = ["list_tables_and_columns".to_owned()];
    let selected_toolset = test_build_with_api(
        "analytics",
        &selected,
        SqlDialect::MySql,
        &policy(&[]),
        &api_trait,
    )
    .expect("selected SQL toolset");
    let readonly: Arc<dyn ReadonlyContext> = context();
    assert_eq!(
        selected_toolset
            .tools(readonly)
            .await
            .expect("selected SQL tools")
            .iter()
            .map(|tool| tool.name())
            .collect::<Vec<_>>(),
        ["list_tables_and_columns"]
    );

    let blocked = test_build_with_api(
        "analytics",
        &[],
        SqlDialect::Postgres,
        &policy(&[("sql", &["execute_sql"])]),
        &api_trait,
    )
    .expect("policy-filtered SQL toolset");
    let readonly: Arc<dyn ReadonlyContext> = context();
    assert_eq!(
        blocked
            .tools(readonly)
            .await
            .expect("policy-filtered SQL tools")
            .iter()
            .map(|tool| tool.name())
            .collect::<Vec<_>>(),
        ["list_tables_and_columns"]
    );

    assert!(
        build_sql_toolset("analytics", config(SqlDialect::Postgres), &policy(&[])).is_ok(),
        "tool construction must perform no network I/O"
    );
    let unknown = SqlToolkitConfig::parse(&settings(
        Some("postgres"),
        Some(json!(["unknown_sql_tool"])),
    ))
    .expect("unknown selection is validated at family construction");
    let Err(error) = build_sql_toolset("analytics", unknown, &policy(&[])) else {
        panic!("unknown SQL selection must fail closed");
    };
    assert_eq!(error.code(), SqlToolsetErrorCode::UnsupportedSelection);
}

#[tokio::test]
async fn tool_admission_precedes_api_calls_and_preserves_exact_statement() {
    let api = Arc::new(FixtureApi::new([
        Ok(json!({"executed":true,"rows_affected":3})),
        Ok(json!({"users":{"table_name":"users","table_columns":[]}})),
    ]));
    let api_trait: Arc<dyn SqlApi> = api.clone();
    let toolset = test_build_with_api(
        "analytics",
        &[],
        SqlDialect::Postgres,
        &policy(&[]),
        &api_trait,
    )
    .expect("SQL fixture toolset");
    let readonly: Arc<dyn ReadonlyContext> = context();
    let tools = toolset.tools(readonly).await.expect("SQL tools");
    let sql = "UPDATE products SET active = false WHERE id = 7;";
    assert_eq!(
        tools[0]
            .execute(context(), json!({"sql_query": sql}))
            .await
            .expect("admitted SQL effect"),
        json!({"executed":true,"rows_affected":3})
    );
    assert_eq!(
        tools[1]
            .execute(context(), json!({}))
            .await
            .expect("schema read"),
        json!({"users":{"table_name":"users","table_columns":[]}})
    );
    assert_eq!(
        api.calls(),
        [FixtureCall::Execute(sql.to_owned()), FixtureCall::List]
    );

    for invalid in [
        json!({"sql_query":"SELECT 1; SELECT 2"}),
        json!({"sql_query":"ROLLBACK"}),
        json!({"sql_query":"SELECT 1", "extra":true}),
        json!({"sql_query":1}),
    ] {
        assert!(tools[0].execute(context(), invalid).await.is_err());
    }
    assert!(
        tools[1]
            .execute(context(), json!({"unexpected":true}))
            .await
            .is_err()
    );
    assert_eq!(api.calls().len(), 2, "invalid calls stop before the API");
}

#[test]
fn deterministic_projection_bounds_and_provider_types_are_explicit() {
    assert_eq!(test_float_value(f64::NAN), Value::String("NaN".to_owned()));
    assert_eq!(
        test_float_value(f64::INFINITY),
        Value::String("Infinity".to_owned())
    );
    assert_eq!(
        test_float_value(f64::NEG_INFINITY),
        Value::String("-Infinity".to_owned())
    );

    let Ok(binary) = test_binary_value(vec![0xAB; MAX_BINARY_BYTES]) else {
        panic!("binary boundary must be accepted");
    };
    assert_eq!(binary["encoding"], "base64");
    assert!(serde_json::to_vec(&binary).expect("binary JSON").len() <= MAX_CELL_BYTES);
    let Err(error) = test_binary_value(vec![0; MAX_BINARY_BYTES + 1]) else {
        panic!("oversized binary must fail");
    };
    assert_eq!(error.code(), ProjectErrorCode::ResourceExhausted);

    assert!(test_bounded_json(json!({"nested":[1,true,null]})).is_ok());
    let Err(error) = test_bounded_json(Value::String("四".repeat(MAX_CELL_BYTES / 3 + 1))) else {
        panic!("serialized multibyte cell over the byte cap must fail");
    };
    assert_eq!(error.code(), ProjectErrorCode::ResourceExhausted);

    let (postgres, mysql) = test_supported_types();
    for required in ["NUMERIC", "JSONB", "UUID", "TIMESTAMPTZ", "TIMETZ"] {
        assert!(postgres.contains(&required));
    }
    for required in ["DECIMAL", "JSON", "DATE", "TIME", "TIMESTAMP"] {
        assert!(mysql.contains(&required));
    }
    assert!(
        test_validate_column_names(&["value", "value"]).is_ok(),
        "duplicate result names are valid and Map insertion preserves SDK last-wins behavior"
    );
}

#[test]
fn schema_introspection_queries_and_projection_are_fixed_and_bounded() {
    let (postgres, mysql) = test_schema_queries();
    assert!(postgres.contains("current_schema()"));
    assert!(postgres.contains("format_type"));
    assert!(postgres.contains("ORDER BY cls.relname, attr.attnum"));
    assert!(mysql.contains("table_schema = DATABASE()"));
    assert!(mysql.contains("table_type = 'BASE TABLE'"));
    assert!(mysql.contains("ORDER BY table_name, ordinal_position"));

    let projection = test_schema_projection(&[
        ("accounts".to_owned(), "id".to_owned(), "bigint".to_owned()),
        (
            "accounts".to_owned(),
            "profile".to_owned(),
            "jsonb".to_owned(),
        ),
        ("events".to_owned(), "at".to_owned(), "timestamp".to_owned()),
    ])
    .expect("bounded schema projection");
    assert_eq!(
        projection,
        json!({
            "accounts": {
                "table_name":"accounts",
                "table_columns":[
                    {"name":"id","type":"bigint"},
                    {"name":"profile","type":"jsonb"}
                ]
            },
            "events": {
                "table_name":"events",
                "table_columns":[{"name":"at","type":"timestamp"}]
            }
        })
    );

    let oversized = vec![("t".to_owned(), "c".to_owned(), "x".repeat(257))];
    let Err(error) = test_schema_projection(&oversized) else {
        panic!("oversized schema metadata must fail");
    };
    assert_eq!(error.code(), SqlClientErrorCode::InvalidResponse);
}

#[test]
fn error_taxonomy_is_redacted_and_effect_unknown_outcome_never_retries() {
    for (code, retryable) in [
        (SqlClientErrorCode::Authentication, false),
        (SqlClientErrorCode::Authorization, false),
        (SqlClientErrorCode::Timeout, true),
        (SqlClientErrorCode::DependencyUnavailable, true),
        (SqlClientErrorCode::UnknownOutcome, false),
    ] {
        let error = SqlClientError::fixture(code, retryable);
        assert_eq!(error.code(), code);
        assert_eq!(error.retryable(), retryable);
        let diagnostic = format!("{error:?} {error}");
        assert!(!diagnostic.contains(PASSWORD));
        assert!(!diagnostic.contains("database.example.test"));
    }
    let unknown = SqlClientError::fixture(SqlClientErrorCode::UnknownOutcome, false).into_adk();
    assert_eq!(unknown.code, "sql.effect.unknown_outcome");
    assert!(!unknown.retry.should_retry);

    for (source, expected) in [
        (
            ProjectErrorCode::InvalidResponse,
            SqlClientErrorCode::InvalidResponse,
        ),
        (
            ProjectErrorCode::ResourceExhausted,
            SqlClientErrorCode::ResourceExhausted,
        ),
        (
            ProjectErrorCode::UnsupportedType,
            SqlClientErrorCode::UnsupportedType,
        ),
    ] {
        let error = test_map_project_read_error(source);
        assert_eq!(error.code(), expected);
        assert!(!error.retryable());
    }
}

#[test]
fn production_slice_has_no_debug_output_or_unbounded_collection_helpers() {
    let sources = [
        include_str!("families/sql/config.rs"),
        include_str!("families/sql/lexer.rs"),
        include_str!("families/sql/project.rs"),
        include_str!("families/sql/client.rs"),
        include_str!("families/sql/tools.rs"),
    ];
    for source in sources {
        for forbidden in [
            "dbg!(",
            "println!(",
            "eprintln!(",
            "panic!(",
            ".unwrap()",
            ".expect(",
            "todo!(",
            "unimplemented!(",
            ".fetch_all(",
        ] {
            assert!(
                !source.contains(forbidden),
                "forbidden production token: {forbidden}"
            );
        }
    }
    let client = include_str!("families/sql/client.rs");
    assert!(client.contains("PgConnectOptions::new_without_env()"));
    assert!(!client.contains("new_without_pgpass"));
    assert!(client.contains(".connect_addr("));
    assert!(client.contains("resolve_checked_with"));
    assert!(client.contains("statement_cache_capacity(0)"));
    assert!(client.contains("disable_statement_logging()"));
    assert!(client.contains("PgSslMode::VerifyFull"));
    assert!(client.contains("MySqlSslMode::VerifyIdentity"));
    assert!(client.contains("standard_conforming_strings"));
    assert!(client.contains("SELECT @@SESSION.sql_mode"));
    assert!(client.contains("pipes_as_concat(false)"));
    assert!(client.contains("no_engine_substitution(false)"));
    assert!(!client.contains("SET SESSION sql_mode"));
}

#[tokio::test]
async fn every_tool_keeps_the_sdk_contract() {
    let api: Arc<dyn SqlApi> = Arc::new(FixtureApi::new([]));
    let toolset = test_build_with_api("gate", &[], SqlDialect::Postgres, &policy(&[]), &api)
        .expect("complete sql toolset");
    let readonly: Arc<dyn ReadonlyContext> = context();
    let tools = toolset.tools(readonly).await.expect("sql tools");
    super::sdk_conformance::assert_sdk_conformance("sql", &tools);
}

const FAKE_SERVER_TIMEOUT: std::time::Duration = std::time::Duration::from_secs(5);

/// `MySQL` `HandshakeV10` for `caching_sha2_password` without `CLIENT_SSL`.
fn no_tls_handshake_packet() -> Vec<u8> {
    // LONG_PASSWORD | FOUND_ROWS | LONG_FLAG | CONNECT_WITH_DB | PROTOCOL_41 |
    // TRANSACTIONS | SECURE_CONNECTION | MULTI_STATEMENTS | MULTI_RESULTS |
    // PLUGIN_AUTH | PLUGIN_AUTH_LENENC_CLIENT_DATA | DEPRECATE_EOF.
    const CAPABILITIES_LOW: u16 = 0xA20F;
    const CAPABILITIES_HIGH: u16 = 0x012B;
    let mut payload = vec![0x0a];
    payload.extend_from_slice(b"8.0.36\0");
    payload.extend_from_slice(&7_u32.to_le_bytes());
    payload.extend_from_slice(b"abcdefgh");
    payload.push(0);
    payload.extend_from_slice(&CAPABILITIES_LOW.to_le_bytes());
    payload.push(255);
    payload.extend_from_slice(&2_u16.to_le_bytes());
    payload.extend_from_slice(&CAPABILITIES_HIGH.to_le_bytes());
    payload.push(21);
    payload.extend_from_slice(&[0; 10]);
    payload.extend_from_slice(b"ijklmnopqrst\0");
    payload.extend_from_slice(b"caching_sha2_password\0");
    framed_packet(0, &payload)
}

fn framed_packet(sequence: u8, payload: &[u8]) -> Vec<u8> {
    let length = u32::try_from(payload.len()).expect("small fixture packet");
    let mut packet = length.to_le_bytes()[..3].to_vec();
    packet.push(sequence);
    packet.extend_from_slice(payload);
    packet
}

async fn read_client_packet(stream: &mut TcpStream) -> Vec<u8> {
    let mut header = [0_u8; 4];
    stream
        .read_exact(&mut header)
        .await
        .expect("client packet header");
    let length =
        usize::from(header[0]) | usize::from(header[1]) << 8 | usize::from(header[2]) << 16;
    let mut payload = vec![0_u8; length];
    stream
        .read_exact(&mut payload)
        .await
        .expect("client packet payload");
    payload
}

async fn fake_mysql_listener() -> (TcpListener, u16) {
    let listener = TcpListener::bind("127.0.0.1:0")
        .await
        .expect("bind fake MySQL listener");
    let port = listener.local_addr().expect("fake MySQL address").port();
    (listener, port)
}

fn mysql_config_for_port(port: u16) -> SqlToolkitConfig {
    let mut settings = settings(Some("mysql"), Some(json!([])));
    settings.insert(
        "sql_configuration".to_owned(),
        json!({
            "host": "127.0.0.1",
            "port": port,
            "username": "application_user",
            "password": PASSWORD
        }),
    );
    SqlToolkitConfig::parse(&settings).expect("fake MySQL SQL configuration")
}

/// Serves the no-TLS handshake, then records what the client sends next (empty on EOF).
async fn serve_and_record(listener: TcpListener) -> Vec<u8> {
    let (mut stream, _) = timeout(FAKE_SERVER_TIMEOUT, listener.accept())
        .await
        .expect("client connects in time")
        .expect("accept client");
    stream
        .write_all(&no_tls_handshake_packet())
        .await
        .expect("write handshake");
    // The first read yields either the client's first bytes or EOF; returning
    // right away closes the socket so a connecting client cannot hang us.
    let mut buffer = [0_u8; 4096];
    let count = timeout(FAKE_SERVER_TIMEOUT, stream.read(&mut buffer))
        .await
        .expect("client sends or closes in time")
        .expect("read from client");
    buffer[..count].to_vec()
}

fn assert_refused_without_leaking(error: &SqlClientError) {
    assert_eq!(error.code(), SqlClientErrorCode::InvalidConfiguration);
    assert!(!format!("{error}").contains(PASSWORD));
    assert!(!format!("{error:?}").contains(PASSWORD));
}

#[tokio::test]
async fn mysql_non_tls_control_reaches_the_rsa_public_key_request() {
    let (listener, port) = fake_mysql_listener().await;
    let server = tokio::spawn(async move {
        let (mut stream, _) = listener.accept().await.expect("accept client");
        stream
            .write_all(&no_tls_handshake_packet())
            .await
            .expect("write handshake");
        let response = read_client_packet(&mut stream).await;
        assert!(!response.is_empty(), "client sent a HandshakeResponse");
        // AuthMoreData: perform full authentication.
        stream
            .write_all(&framed_packet(2, &[0x01, 0x04]))
            .await
            .expect("write auth-more-data");
        read_client_packet(&mut stream).await
    });

    let options = MySqlConnectOptions::new()
        .host("127.0.0.1")
        .port(port)
        .username("application_user")
        .password(PASSWORD)
        .database("application_db")
        .ssl_mode(MySqlSslMode::Disabled);
    // The server drops the connection after the key request, so connect fails.
    let connect = timeout(FAKE_SERVER_TIMEOUT, options.connect())
        .await
        .expect("control connect finishes in time");
    assert!(connect.is_err());
    let public_key_request = timeout(FAKE_SERVER_TIMEOUT, server)
        .await
        .expect("fake server finishes in time")
        .expect("fake server task");
    assert_eq!(public_key_request, [0x02]);
}

#[tokio::test]
async fn mysql_without_tls_is_refused_before_any_authentication_byte() {
    let (listener, port) = fake_mysql_listener().await;
    let server = tokio::spawn(serve_and_record(listener));
    let permits = Arc::new(
        Allowlist::parse_csv(&format!("127.0.0.1:{port}"))
            .expect("loopback permit")
            .private_permits(),
    );
    let client = SqlClient::new(mysql_config_for_port(port), permits, RunEgress::open());
    let outcome = timeout(FAKE_SERVER_TIMEOUT, client.list_tables_and_columns())
        .await
        .expect("client call finishes in time");
    let received = timeout(FAKE_SERVER_TIMEOUT, server)
        .await
        .expect("fake server finishes in time")
        .expect("fake server task");
    assert!(received.is_empty(), "client sent {} bytes", received.len());
    let error = outcome.expect_err("a non-TLS MySQL server is refused");
    assert_refused_without_leaking(&error);

    let (listener, port) = fake_mysql_listener().await;
    let server = tokio::spawn(serve_and_record(listener));
    let permits = Arc::new(
        Allowlist::parse_csv(&format!("127.0.0.1:{port}"))
            .expect("loopback permit")
            .private_permits(),
    );
    let client = SqlClient::new(mysql_config_for_port(port), permits, RunEgress::open());
    let outcome = timeout(FAKE_SERVER_TIMEOUT, client.execute_sql("SELECT 1"))
        .await
        .expect("client call finishes in time");
    let received = timeout(FAKE_SERVER_TIMEOUT, server)
        .await
        .expect("fake server finishes in time")
        .expect("fake server task");
    assert!(received.is_empty(), "client sent {} bytes", received.len());
    let error = outcome.expect_err("a non-TLS MySQL server is refused");
    assert_refused_without_leaking(&error);
}

#[tokio::test]
async fn execute_sql_reports_the_rows_it_returned_to_the_run_caps() {
    let api = Arc::new(FixtureApi::new([
        Ok(json!([{"id": 1}, {"id": 2}, {"id": 3}])),
        Ok(json!({"executed": true, "rows_affected": 9})),
    ]));
    let api_trait: Arc<dyn SqlApi> = api.clone();
    let meter = crate::run_caps::RunCapsMeter::new(crate::run_caps::RunCapLimits::unlimited());
    let policy = Arc::new(
        policy(&[])
            .as_ref()
            .clone()
            .with_run_caps(Arc::clone(&meter)),
    );
    let toolset = test_build_with_api("analytics", &[], SqlDialect::Postgres, &policy, &api_trait)
        .expect("SQL fixture toolset");
    let readonly: Arc<dyn ReadonlyContext> = context();
    let tools = toolset.tools(readonly).await.expect("SQL tools");
    let execute = tools
        .iter()
        .find(|tool| tool.name() == "execute_sql")
        .expect("execute_sql");
    execute
        .execute(context(), json!({"sql_query": "SELECT id FROM users"}))
        .await
        .expect("rows");
    assert_eq!(meter.records_read(), 3);
    // A statement that returns no row list counts nothing.
    execute
        .execute(context(), json!({"sql_query": "UPDATE users SET a = 1"}))
        .await
        .expect("effect");
    assert_eq!(meter.records_read(), 3);
    assert_eq!(meter.tool_calls(), 2);
}

struct FakeLookup {
    answers: HashMap<String, Vec<IpAddr>>,
    calls: AtomicUsize,
}

impl FakeLookup {
    fn new(entries: impl IntoIterator<Item = (&'static str, Vec<IpAddr>)>) -> Self {
        Self {
            answers: entries
                .into_iter()
                .map(|(host, ips)| (host.to_owned(), ips))
                .collect(),
            calls: AtomicUsize::new(0),
        }
    }

    fn calls(&self) -> usize {
        self.calls.load(Ordering::SeqCst)
    }
}

impl Lookup for FakeLookup {
    fn lookup(&self, host: String) -> LookupFuture {
        self.calls.fetch_add(1, Ordering::SeqCst);
        let answer = self.answers.get(&host).cloned();
        Box::pin(async move { answer.ok_or_else(|| io::Error::other("no such host")) })
    }
}

struct SequenceLookup {
    answers: Mutex<VecDeque<Vec<IpAddr>>>,
    calls: AtomicUsize,
}

impl SequenceLookup {
    fn new(answers: impl IntoIterator<Item = Vec<IpAddr>>) -> Self {
        Self {
            answers: Mutex::new(answers.into_iter().collect()),
            calls: AtomicUsize::new(0),
        }
    }

    fn calls(&self) -> usize {
        self.calls.load(Ordering::SeqCst)
    }
}

impl Lookup for SequenceLookup {
    fn lookup(&self, _host: String) -> LookupFuture {
        self.calls.fetch_add(1, Ordering::SeqCst);
        let next = self
            .answers
            .lock()
            .expect("sequence lookup lock")
            .pop_front();
        Box::pin(async move { next.ok_or_else(|| io::Error::other("no more lookup answers")) })
    }
}

fn config_for_host_port(dialect: SqlDialect, host: &str, port: u16) -> SqlToolkitConfig {
    let mut settings = settings(
        Some(match dialect {
            SqlDialect::Postgres => "postgres",
            SqlDialect::MySql => "mysql",
        }),
        Some(json!([])),
    );
    settings.insert(
        "sql_configuration".to_owned(),
        json!({
            "host": host,
            "port": port,
            "username": "application_user",
            "password": PASSWORD
        }),
    );
    SqlToolkitConfig::parse(&settings).expect("SQL host/port config fixture")
}

fn build_tls_fixture(server_dns_name: &str) -> (Vec<u8>, tokio_rustls::TlsAcceptor) {
    use rcgen::{
        BasicConstraints, CertificateParams, CertifiedIssuer, ExtendedKeyUsagePurpose, IsCa,
        KeyPair, KeyUsagePurpose,
    };
    use rustls::pki_types::{CertificateDer, PrivateKeyDer, pem::PemObject};

    let mut ca_params = CertificateParams::new(Vec::<String>::new()).expect("ca params");
    ca_params.is_ca = IsCa::Ca(BasicConstraints::Unconstrained);
    ca_params.key_usages = vec![KeyUsagePurpose::KeyCertSign, KeyUsagePurpose::CrlSign];
    let ca_key = KeyPair::generate().expect("ca key");
    let issuer = CertifiedIssuer::self_signed(ca_params, ca_key).expect("self-signed ca");

    let leaf_key = KeyPair::generate().expect("leaf key");
    let mut leaf_params =
        CertificateParams::new(vec![server_dns_name.to_owned()]).expect("leaf params");
    leaf_params.extended_key_usages = vec![ExtendedKeyUsagePurpose::ServerAuth];
    let leaf_cert = leaf_params
        .signed_by(&leaf_key, &issuer)
        .expect("signed leaf");

    let cert_chain = vec![CertificateDer::from(leaf_cert.der().to_vec())];
    let key_der = PrivateKeyDer::from_pem_slice(leaf_key.serialize_pem().as_bytes())
        .expect("leaf private key der");
    let server_config = rustls::ServerConfig::builder_with_provider(Arc::new(
        rustls::crypto::ring::default_provider(),
    ))
    .with_safe_default_protocol_versions()
    .expect("tls protocol versions")
    .with_no_client_auth()
    .with_single_cert(cert_chain, key_der)
    .expect("server tls config");

    (
        issuer.pem().into_bytes(),
        tokio_rustls::TlsAcceptor::from(Arc::new(server_config)),
    )
}

fn mysql_tls_handshake_packet() -> Vec<u8> {
    // Same as no_tls_handshake_packet plus CLIENT_SSL (0x0800).
    const CAPABILITIES_LOW: u16 = 0xAA0F;
    const CAPABILITIES_HIGH: u16 = 0x012B;
    let mut payload = vec![0x0a];
    payload.extend_from_slice(b"8.0.36\0");
    payload.extend_from_slice(&7_u32.to_le_bytes());
    payload.extend_from_slice(b"abcdefgh");
    payload.push(0);
    payload.extend_from_slice(&CAPABILITIES_LOW.to_le_bytes());
    payload.push(255);
    payload.extend_from_slice(&2_u16.to_le_bytes());
    payload.extend_from_slice(&CAPABILITIES_HIGH.to_le_bytes());
    payload.push(21);
    payload.extend_from_slice(&[0; 10]);
    payload.extend_from_slice(b"ijklmnopqrst\0");
    payload.extend_from_slice(b"caching_sha2_password\0");
    framed_packet(0, &payload)
}

fn mysql_auth_denied_packet(sequence: u8) -> Vec<u8> {
    // ERR_Packet: 0xff, error_code=1045 (LE), '#', SQLSTATE "28000", message.
    let mut payload = vec![0xff];
    payload.extend_from_slice(&1045_u16.to_le_bytes());
    payload.push(b'#');
    payload.extend_from_slice(b"28000");
    payload.extend_from_slice(b"Access denied");
    framed_packet(sequence, &payload)
}

fn pg_auth_denied_message() -> Vec<u8> {
    // ErrorResponse ('E'): length (i32 BE) + fields S/C/M + trailing NUL.
    let mut body = Vec::new();
    body.extend_from_slice(b"SFATAL\0");
    body.extend_from_slice(b"C28P01\0");
    body.extend_from_slice(b"Mpassword authentication failed\0");
    body.push(0);
    let len = i32::try_from(body.len() + 4).expect("pg error length");
    let mut frame = vec![b'E'];
    frame.extend_from_slice(&len.to_be_bytes());
    frame.extend_from_slice(&body);
    frame
}

#[test]
fn postgres_options_new_without_env_eliminates_ambient_tls_file_seam() {
    let options = PgConnectOptions::new_without_env();
    assert!(!options.has_ssl_root_cert());
    assert!(!options.has_ssl_client_cert());
    assert!(!options.has_ssl_client_key());
    assert_eq!(options.get_connect_addr(), None);
    assert_eq!(options.get_options(), None);
    assert_eq!(options.get_application_name(), None);
}

#[tokio::test]
async fn postgres_guarded_dial_verifies_hostname_against_checked_ip() {
    let (ca_pem, acceptor) = build_tls_fixture("warehouse.pg.example.test");
    let listener = TcpListener::bind("127.0.0.1:0")
        .await
        .expect("bind fake PG TLS listener");
    let port = listener.local_addr().expect("PG addr").port();

    let server = tokio::spawn(async move {
        let (mut stream, _) = timeout(FAKE_SERVER_TIMEOUT, listener.accept())
            .await
            .expect("accept timeout")
            .expect("accept");
        let mut ssl_req = [0_u8; 8];
        stream
            .read_exact(&mut ssl_req)
            .await
            .expect("PG SSLRequest");
        assert_eq!(ssl_req, [0, 0, 0, 8, 4, 210, 22, 47]);
        stream.write_all(b"S").await.expect("PG SSL accept byte");
        let mut tls = acceptor.accept(stream).await.expect("PG TLS handshake");
        let sni = tls.get_ref().1.server_name().map(str::to_owned);
        // Read the Startup message length + payload over verified TLS.
        let mut len_buf = [0_u8; 4];
        tls.read_exact(&mut len_buf)
            .await
            .expect("PG Startup length");
        let len = usize::try_from(u32::from_be_bytes(len_buf)).expect("startup len");
        let mut startup = vec![0_u8; len.saturating_sub(4)];
        tls.read_exact(&mut startup).await.expect("PG Startup body");
        tls.write_all(&pg_auth_denied_message())
            .await
            .expect("PG auth error");
        tls.flush().await.expect("flush");
        sni
    });

    let lookup = Arc::new(FakeLookup::new([(
        "warehouse.pg.example.test",
        vec![IpAddr::V4(Ipv4Addr::LOCALHOST)],
    )]));
    let permits = Arc::new(
        Allowlist::parse_csv(&format!("127.0.0.1:{port}"))
            .expect("permit")
            .private_permits(),
    );
    let client = SqlClient::new(
        config_for_host_port(SqlDialect::Postgres, "warehouse.pg.example.test", port),
        permits,
        RunEgress::open(),
    )
    .with_lookup(lookup.clone())
    .with_test_root_cert_pem(ca_pem);

    let err = timeout(FAKE_SERVER_TIMEOUT, client.list_tables_and_columns())
        .await
        .expect("client finishes")
        .expect_err("auth denied after verified TLS");
    // Reaching SQLSTATE 28P01 proves the client dialed 127.0.0.1 via connect_addr,
    // completed TLS VerifyFull for warehouse.pg.example.test, and sent Startup.
    assert_eq!(err.code(), SqlClientErrorCode::Authentication);
    assert_eq!(lookup.calls(), 1);
    let observed_sni = timeout(FAKE_SERVER_TIMEOUT, server)
        .await
        .expect("server finishes")
        .expect("server task");
    assert_eq!(observed_sni.as_deref(), Some("warehouse.pg.example.test"));
}

#[tokio::test]
async fn postgres_guarded_dial_refuses_tls_hostname_mismatch() {
    let (ca_pem, acceptor) = build_tls_fixture("other-host.pg.example.test");
    let listener = TcpListener::bind("127.0.0.1:0")
        .await
        .expect("bind fake PG TLS listener");
    let port = listener.local_addr().expect("PG addr").port();

    let server = tokio::spawn(async move {
        let (mut stream, _) = timeout(FAKE_SERVER_TIMEOUT, listener.accept())
            .await
            .expect("accept timeout")
            .expect("accept");
        let mut ssl_req = [0_u8; 8];
        stream
            .read_exact(&mut ssl_req)
            .await
            .expect("PG SSLRequest");
        stream.write_all(b"S").await.expect("PG SSL accept byte");
        let tls_result = acceptor.accept(stream).await;
        match tls_result {
            Err(_) => true,
            Ok(mut tls) => {
                // TLS 1.3 server finishes accept before reading client alert.
                let mut byte = [0_u8; 1];
                tls.read(&mut byte).await.is_err() || matches!(tls.read(&mut byte).await, Ok(0))
            }
        }
    });

    let lookup = Arc::new(FakeLookup::new([(
        "warehouse.pg.example.test",
        vec![IpAddr::V4(Ipv4Addr::LOCALHOST)],
    )]));
    let permits = Arc::new(
        Allowlist::parse_csv(&format!("127.0.0.1:{port}"))
            .expect("permit")
            .private_permits(),
    );
    let client = SqlClient::new(
        config_for_host_port(SqlDialect::Postgres, "warehouse.pg.example.test", port),
        permits,
        RunEgress::open(),
    )
    .with_lookup(lookup)
    .with_test_root_cert_pem(ca_pem);

    let err = timeout(FAKE_SERVER_TIMEOUT, client.list_tables_and_columns())
        .await
        .expect("client finishes")
        .expect_err("hostname mismatch must be refused");
    assert_refused_without_leaking(&err);
    assert!(!format!("{err} {err:?}").contains("warehouse.pg.example.test"));
    let rejected = timeout(FAKE_SERVER_TIMEOUT, server)
        .await
        .expect("server finishes")
        .expect("server task");
    assert!(rejected, "client must abort before sending PG Startup");
}

#[tokio::test]
async fn mysql_guarded_dial_verifies_hostname_against_checked_ip() {
    let (ca_pem, acceptor) = build_tls_fixture("warehouse.mysql.example.test");
    let (listener, port) = fake_mysql_listener().await;

    let server = tokio::spawn(async move {
        let (mut stream, _) = timeout(FAKE_SERVER_TIMEOUT, listener.accept())
            .await
            .expect("accept timeout")
            .expect("accept");
        stream
            .write_all(&mysql_tls_handshake_packet())
            .await
            .expect("MySQL TLS handshake");
        let ssl_request = read_client_packet(&mut stream).await;
        assert!(!ssl_request.is_empty(), "client sent MySQL SslRequest");
        let mut tls = acceptor.accept(stream).await.expect("MySQL TLS handshake");
        let sni = tls.get_ref().1.server_name().map(str::to_owned);
        // Read HandshakeResponse header + body over verified TLS.
        let mut header = [0_u8; 4];
        tls.read_exact(&mut header)
            .await
            .expect("MySQL response header");
        let len =
            usize::from(header[0]) | usize::from(header[1]) << 8 | usize::from(header[2]) << 16;
        let mut body = vec![0_u8; len];
        tls.read_exact(&mut body)
            .await
            .expect("MySQL response body");
        tls.write_all(&mysql_auth_denied_packet(header[3].wrapping_add(1)))
            .await
            .expect("MySQL auth error");
        tls.flush().await.expect("flush");
        sni
    });

    let lookup = Arc::new(FakeLookup::new([(
        "warehouse.mysql.example.test",
        vec![IpAddr::V4(Ipv4Addr::LOCALHOST)],
    )]));
    let permits = Arc::new(
        Allowlist::parse_csv(&format!("127.0.0.1:{port}"))
            .expect("permit")
            .private_permits(),
    );
    let client = SqlClient::new(
        config_for_host_port(SqlDialect::MySql, "warehouse.mysql.example.test", port),
        permits,
        RunEgress::open(),
    )
    .with_lookup(lookup.clone())
    .with_test_root_cert_pem(ca_pem);

    let err = timeout(FAKE_SERVER_TIMEOUT, client.list_tables_and_columns())
        .await
        .expect("client finishes")
        .expect_err("auth denied after verified TLS");
    assert_eq!(err.code(), SqlClientErrorCode::Authentication);
    assert_eq!(lookup.calls(), 1);
    let observed_sni = timeout(FAKE_SERVER_TIMEOUT, server)
        .await
        .expect("server finishes")
        .expect("server task");
    assert_eq!(
        observed_sni.as_deref(),
        Some("warehouse.mysql.example.test")
    );
}

#[tokio::test]
async fn mysql_guarded_dial_refuses_tls_hostname_mismatch() {
    let (ca_pem, acceptor) = build_tls_fixture("other-host.mysql.example.test");
    let (listener, port) = fake_mysql_listener().await;

    let server = tokio::spawn(async move {
        let (mut stream, _) = timeout(FAKE_SERVER_TIMEOUT, listener.accept())
            .await
            .expect("accept timeout")
            .expect("accept");
        stream
            .write_all(&mysql_tls_handshake_packet())
            .await
            .expect("MySQL TLS handshake");
        let _ = read_client_packet(&mut stream).await;
        let tls_result = acceptor.accept(stream).await;
        match tls_result {
            Err(_) => true,
            Ok(mut tls) => {
                let mut byte = [0_u8; 1];
                tls.read(&mut byte).await.is_err() || matches!(tls.read(&mut byte).await, Ok(0))
            }
        }
    });

    let lookup = Arc::new(FakeLookup::new([(
        "warehouse.mysql.example.test",
        vec![IpAddr::V4(Ipv4Addr::LOCALHOST)],
    )]));
    let permits = Arc::new(
        Allowlist::parse_csv(&format!("127.0.0.1:{port}"))
            .expect("permit")
            .private_permits(),
    );
    let client = SqlClient::new(
        config_for_host_port(SqlDialect::MySql, "warehouse.mysql.example.test", port),
        permits,
        RunEgress::open(),
    )
    .with_lookup(lookup)
    .with_test_root_cert_pem(ca_pem);

    let err = timeout(FAKE_SERVER_TIMEOUT, client.execute_sql("SELECT 1"))
        .await
        .expect("client finishes")
        .expect_err("hostname mismatch must be refused");
    assert_refused_without_leaking(&err);
    let rejected = timeout(FAKE_SERVER_TIMEOUT, server)
        .await
        .expect("server finishes")
        .expect("server task");
    assert!(
        rejected,
        "client must abort before sending MySQL HandshakeResponse"
    );
}

#[tokio::test]
async fn sql_egress_guard_refuses_private_ipv4_mapped_nat64_and_metadata_targets() {
    let empty_permits = Arc::new(PermitSet::default());
    let wide_permits = Arc::new(
        Allowlist::parse_csv("0.0.0.0/0,::/0,169.254.0.0/16,fd00::/8")
            .expect("wide allowlist")
            .private_permits(),
    );

    // Every private, IPv4-mapped, NAT64, 6to4, loopback, and metadata literal is refused
    // by default on both PostgreSQL and MySQL before any TCP connect.
    let blocked_literals = [
        "127.0.0.1",
        "::1",
        "localhost",
        "10.0.0.1",
        "172.16.0.1",
        "192.168.1.5",
        "100.64.0.1",
        "fc00::1",
        "::ffff:127.0.0.1",
        "::ffff:10.0.0.1",
        "64:ff9b::a00:1",
        "64:ff9b::7f00:1",
        "2002:a00:1::1",
        "169.254.169.254",
        "::ffff:169.254.169.254",
        "64:ff9b::a9fe:a9fe",
        "fd00:ec2::254",
        "fe80::1",
        "0.0.0.0",
        "224.0.0.1",
    ];
    for dialect in [SqlDialect::Postgres, SqlDialect::MySql] {
        for host in blocked_literals {
            let client = SqlClient::new(
                config_for_host_port(dialect, host, dialect.default_port()),
                Arc::clone(&empty_permits),
                RunEgress::open(),
            );
            let err = client
                .list_tables_and_columns()
                .await
                .expect_err("private/forbidden literal must be refused");
            assert_eq!(
                err.code(),
                SqlClientErrorCode::EgressRefused,
                "{dialect:?} {host}"
            );
            assert!(!err.retryable());
            let adk = err.into_adk();
            assert_eq!(adk.code, "sql.egress.refused");
            assert!(!adk.retry.should_retry);
            assert!(!format!("{adk}").contains(PASSWORD));
            assert!(!format!("{adk}").contains(host));
        }
    }

    // Forbidden metadata/link-local/unspecified targets stay refused EVEN when
    // ELITEA_DB_EGRESS_ALLOWLIST contains 0.0.0.0/0 and ::/0.
    for forbidden in [
        "169.254.169.254",
        "::ffff:169.254.169.254",
        "64:ff9b::a9fe:a9fe",
        "fd00:ec2::254",
        "100.100.100.200",
        "168.63.129.16",
        "fe80::1",
        "0.0.0.0",
    ] {
        let client = SqlClient::new(
            config_for_host_port(SqlDialect::Postgres, forbidden, 5432),
            Arc::clone(&wide_permits),
            RunEgress::open(),
        );
        let err = client
            .execute_sql("SELECT 1")
            .await
            .expect_err("forbidden address cannot be lifted by allowlist");
        assert_eq!(err.code(), SqlClientErrorCode::EgressRefused, "{forbidden}");
    }
}

#[tokio::test]
async fn sql_egress_guard_blocks_unpermitted_loopback_port_mismatch_and_hostname_permit() {
    // 1. Unpermitted loopback refuses before opening TCP: the listener never accepts.
    let (listener, port) = fake_mysql_listener().await;
    let client = SqlClient::new(
        mysql_config_for_port(port),
        Arc::new(PermitSet::default()),
        RunEgress::open(),
    );
    let err = client
        .list_tables_and_columns()
        .await
        .expect_err("unpermitted loopback must be refused");
    assert_eq!(err.code(), SqlClientErrorCode::EgressRefused);
    assert!(
        timeout(std::time::Duration::from_millis(50), listener.accept())
            .await
            .is_err(),
        "no TCP connection may be attempted when egress refuses"
    );

    // 2. Port-pinned permit admits only the pinned port.
    let wrong_port_permits = Arc::new(
        Allowlist::parse_csv(&format!("127.0.0.1:{}", port.wrapping_add(1)))
            .expect("wrong port permit")
            .private_permits(),
    );
    let client = SqlClient::new(
        mysql_config_for_port(port),
        wrong_port_permits,
        RunEgress::open(),
    );
    assert_eq!(
        client
            .list_tables_and_columns()
            .await
            .expect_err("wrong port must be refused")
            .code(),
        SqlClientErrorCode::EgressRefused
    );

    // 3. Bare hostname in ELITEA_DB_EGRESS_ALLOWLIST never lifts a private IP.
    let hostname_only = Arc::new(
        Allowlist::parse_csv("warehouse.internal:5432")
            .expect("hostname allowlist")
            .private_permits(),
    );
    let private_lookup = Arc::new(FakeLookup::new([(
        "warehouse.internal",
        vec!["10.0.1.5".parse().expect("ip")],
    )]));
    let client = SqlClient::new(
        config_for_host_port(SqlDialect::Postgres, "warehouse.internal", 5432),
        hostname_only,
        RunEgress::open(),
    )
    .with_lookup(private_lookup);
    assert_eq!(
        client
            .list_tables_and_columns()
            .await
            .expect_err("bare hostname permit must not lift private IP")
            .code(),
        SqlClientErrorCode::EgressRefused
    );
}

#[tokio::test]
async fn sql_egress_guard_blocks_dns_rebinding_oversized_dns_and_run_public_policy() {
    // 1. DNS rebinding: lookup resolves once to checked IP 127.0.0.1, never re-resolves
    // to the attacker's second answer (169.254.169.254) during dial.
    let (ca_pem, acceptor) = build_tls_fixture("rebind.example.test");
    let pg_listener = TcpListener::bind("127.0.0.1:0")
        .await
        .expect("bind PG rebind listener");
    let pg_port = pg_listener.local_addr().expect("PG rebind addr").port();
    let server = tokio::spawn(async move {
        let (mut stream, _) = timeout(FAKE_SERVER_TIMEOUT, pg_listener.accept())
            .await
            .expect("accept")
            .expect("accept");
        let mut ssl_req = [0_u8; 8];
        stream.read_exact(&mut ssl_req).await.expect("SSLRequest");
        stream.write_all(b"S").await.expect("S");
        let mut tls = acceptor.accept(stream).await.expect("TLS accept");
        let mut len_buf = [0_u8; 4];
        tls.read_exact(&mut len_buf).await.expect("startup len");
        let len = usize::try_from(u32::from_be_bytes(len_buf)).expect("len");
        let mut startup = vec![0_u8; len.saturating_sub(4)];
        tls.read_exact(&mut startup).await.expect("startup body");
        tls.write_all(&pg_auth_denied_message())
            .await
            .expect("auth denied");
        tls.flush().await.expect("flush");
    });
    let rebind_lookup = Arc::new(SequenceLookup::new([
        vec![IpAddr::V4(Ipv4Addr::LOCALHOST)],
        vec!["169.254.169.254".parse().expect("metadata ip")],
    ]));
    let permits = Arc::new(
        Allowlist::parse_csv(&format!("127.0.0.1:{pg_port}"))
            .expect("permit")
            .private_permits(),
    );
    let client = SqlClient::new(
        config_for_host_port(SqlDialect::Postgres, "rebind.example.test", pg_port),
        permits,
        RunEgress::open(),
    )
    .with_lookup(rebind_lookup.clone())
    .with_test_root_cert_pem(ca_pem);
    let err = timeout(FAKE_SERVER_TIMEOUT, client.list_tables_and_columns())
        .await
        .expect("rebind test finishes")
        .expect_err("auth denied on checked IP");
    assert_eq!(err.code(), SqlClientErrorCode::Authentication);
    assert_eq!(
        rebind_lookup.calls(),
        1,
        "DNS must be resolved exactly once; dial must pin the checked IP"
    );
    timeout(FAKE_SERVER_TIMEOUT, server)
        .await
        .expect("server finishes")
        .expect("server task");

    // 5. Oversized DNS answer (> MAX_RESOLVED_ADDRESSES) is refused.
    let too_many: Vec<IpAddr> = (0..=MAX_RESOLVED_ADDRESSES)
        .map(|i| IpAddr::from([93, 184, 216, u8::try_from(i).expect("idx")]))
        .collect();
    let many_lookup = Arc::new(FakeLookup::new([("many.example.test", too_many)]));
    let client = SqlClient::new(
        config_for_host_port(SqlDialect::Postgres, "many.example.test", 5432),
        Arc::new(PermitSet::default()),
        RunEgress::open(),
    )
    .with_lookup(many_lookup);
    assert_eq!(
        client
            .list_tables_and_columns()
            .await
            .expect_err("oversized DNS answer must be refused")
            .code(),
        SqlClientErrorCode::EgressRefused
    );

    // 6. Run-level PublicPolicy::Allowlist refuses unnamed host before DNS lookup.
    let run_egress = RunEgress::new(
        PublicPolicy::allowlist(["allowed.example.test:5432"]).expect("public policy"),
    );
    let unused_lookup = Arc::new(FakeLookup::new([(
        "blocked.example.test",
        vec!["93.184.216.34".parse().expect("public ip")],
    )]));
    let client = SqlClient::new(
        config_for_host_port(SqlDialect::Postgres, "blocked.example.test", 5432),
        Arc::new(PermitSet::default()),
        Arc::clone(&run_egress),
    )
    .with_lookup(unused_lookup.clone());
    assert_eq!(
        client
            .list_tables_and_columns()
            .await
            .expect_err("host outside run public policy must be refused")
            .code(),
        SqlClientErrorCode::EgressRefused
    );
    assert_eq!(unused_lookup.calls(), 0, "refused before DNS lookup");
    assert_eq!(run_egress.summary().refused.len(), 1);
    assert_eq!(run_egress.summary().refused[0].host, "blocked.example.test");
    assert_eq!(run_egress.summary().refused[0].port, 5432);
}

#[test]
fn postgres_options_new_without_env_ignores_populated_pg_env_in_child_process() {
    const CHILD_MARKER: &str = "ELITEA_TEST_PG_ENV_CHILD";
    if std::env::var_os(CHILD_MARKER).is_none() {
        let exe = std::env::current_exe().expect("test executable path");
        let output = std::process::Command::new(exe)
            .args([
                "--exact",
                "toolkits::sql_tests::postgres_options_new_without_env_ignores_populated_pg_env_in_child_process",
                "--nocapture",
            ])
            .env(CHILD_MARKER, "1")
            .env("PGSSLROOTCERT", "/nonexistent/ambient-root.crt")
            .env("PGSSLCERT", "/nonexistent/ambient-client.crt")
            .env("PGSSLKEY", "/nonexistent/ambient-client.key")
            .env("PGSSLMODE", "disable")
            .env("PGHOST", "ambient.pg.invalid")
            .env("PGPORT", "6543")
            .env("PGUSER", "ambient_user")
            .env("PGPASSWORD", "ambient_password")
            .env("PGDATABASE", "ambient_db")
            .env("PGAPPNAME", "ambient_app")
            .env("PGOPTIONS", "-c statement_timeout=0")
            .output()
            .expect("spawn child test process");
        assert!(
            output.status.success(),
            "child test failed: stdout={} stderr={}",
            String::from_utf8_lossy(&output.stdout),
            String::from_utf8_lossy(&output.stderr)
        );
        return;
    }

    // Positive control: upstream `new_without_pgpass` absorbs ambient PGSSL*/PG* vars.
    let ambient = PgConnectOptions::new_without_pgpass();
    assert!(ambient.has_ssl_root_cert());
    assert!(ambient.has_ssl_client_cert());
    assert!(ambient.has_ssl_client_key());
    assert_eq!(ambient.get_host(), "ambient.pg.invalid");
    assert_eq!(ambient.get_port(), 6543);
    assert_eq!(ambient.get_options(), Some("-c statement_timeout=0"));

    // Hardened path: `new_without_env` and `SqlClient::postgres_options` ignore all of them.
    let clean = PgConnectOptions::new_without_env();
    assert!(!clean.has_ssl_root_cert());
    assert!(!clean.has_ssl_client_cert());
    assert!(!clean.has_ssl_client_key());
    assert_eq!(clean.get_host(), "localhost");
    assert_eq!(clean.get_port(), 5432);
    assert_eq!(clean.get_options(), None);
    assert_eq!(clean.get_application_name(), None);

    let dial_ip = IpAddr::V4(Ipv4Addr::new(93, 184, 216, 34));
    let profile = test_connection_profile(config(SqlDialect::Postgres), dial_ip);
    assert_eq!(profile.host, "database.example.test");
    assert_eq!(profile.port, 5432);
    assert_eq!(profile.tls, "verify-full");
    assert!(!profile.has_ssl_root_cert);
    assert!(!profile.has_ssl_client_cert);
    assert!(!profile.has_ssl_client_key);
}

#[tokio::test]
async fn sql_egress_ledger_records_private_refusals_and_reached_connections() {
    // 1. Private-address refusal is recorded in the run's egress ledger.
    let run_egress = RunEgress::open();
    let private_lookup = Arc::new(FakeLookup::new([(
        "private.pg.example.test",
        vec!["10.0.1.5".parse().expect("private ip")],
    )]));
    let client = SqlClient::new(
        config_for_host_port(SqlDialect::Postgres, "private.pg.example.test", 5432),
        Arc::new(PermitSet::default()),
        Arc::clone(&run_egress),
    )
    .with_lookup(private_lookup);
    assert_eq!(
        client
            .list_tables_and_columns()
            .await
            .expect_err("private IP must be refused")
            .code(),
        SqlClientErrorCode::EgressRefused
    );
    let summary = run_egress.summary();
    assert_eq!(summary.refused.len(), 1);
    assert_eq!(summary.refused[0].host, "private.pg.example.test");
    assert_eq!(summary.refused[0].port, 5432);
    assert_eq!(summary.refused[0].count, 1);
    assert_eq!(summary.reached, Vec::new());

    // 2. Successful verified-TLS PostgreSQL connect records the host as reached.
    let (ca_pem, acceptor) = build_tls_fixture("reached.pg.example.test");
    let listener = TcpListener::bind("127.0.0.1:0")
        .await
        .expect("bind PG reached listener");
    let port = listener.local_addr().expect("PG reached addr").port();
    let server = tokio::spawn(async move {
        let (mut stream, _) = timeout(FAKE_SERVER_TIMEOUT, listener.accept())
            .await
            .expect("accept")
            .expect("accept");
        let mut ssl_req = [0_u8; 8];
        stream.read_exact(&mut ssl_req).await.expect("SSLRequest");
        stream.write_all(b"S").await.expect("S");
        let mut tls = acceptor.accept(stream).await.expect("TLS accept");
        let mut len_buf = [0_u8; 4];
        tls.read_exact(&mut len_buf).await.expect("startup len");
        let len = usize::try_from(u32::from_be_bytes(len_buf)).expect("len");
        let mut startup = vec![0_u8; len.saturating_sub(4)];
        tls.read_exact(&mut startup).await.expect("startup body");
        // AuthenticationOk ('R', len=8, status=0) + ReadyForQuery ('Z', len=5, 'I').
        tls.write_all(&[b'R', 0, 0, 0, 8, 0, 0, 0, 0, b'Z', 0, 0, 0, 5, b'I'])
            .await
            .expect("PG auth ok + ready");
        tls.flush().await.expect("flush");
        // Close after connect succeeds; introspection query then returns EOF cleanly.
        let mut scratch = [0_u8; 64];
        let _ = timeout(
            std::time::Duration::from_millis(200),
            tls.read(&mut scratch),
        )
        .await;
    });
    let reached_egress = RunEgress::open();
    let lookup = Arc::new(FakeLookup::new([(
        "reached.pg.example.test",
        vec![IpAddr::V4(Ipv4Addr::LOCALHOST)],
    )]));
    let permits = Arc::new(
        Allowlist::parse_csv(&format!("127.0.0.1:{port}"))
            .expect("permit")
            .private_permits(),
    );
    let client = SqlClient::new(
        config_for_host_port(SqlDialect::Postgres, "reached.pg.example.test", port),
        permits,
        Arc::clone(&reached_egress),
    )
    .with_lookup(lookup)
    .with_test_root_cert_pem(ca_pem);
    let _ = timeout(FAKE_SERVER_TIMEOUT, client.list_tables_and_columns())
        .await
        .expect("call completes");
    timeout(FAKE_SERVER_TIMEOUT, server)
        .await
        .expect("server completes")
        .expect("server task");
    let summary = reached_egress.summary();
    assert_eq!(summary.refused, Vec::new());
    assert_eq!(summary.reached.len(), 1);
    assert_eq!(summary.reached[0].host, "reached.pg.example.test");
    assert_eq!(summary.reached[0].port, port);
    assert_eq!(summary.reached[0].count, 1);
}

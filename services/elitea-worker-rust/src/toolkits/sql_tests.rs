//! Focused compatibility and safety tests for the capability-disabled SQL family.

use std::collections::{BTreeMap, VecDeque};
use std::sync::{Arc, Mutex};

use adk_rust::tool::SimpleToolContext;
use adk_rust::{ReadonlyContext, ToolContext, Toolset};
use async_trait::async_trait;
use serde_json::{Map, Value, json};
use sqlx::ConnectOptions;
use sqlx::mysql::{MySqlConnectOptions, MySqlSslMode};
use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::net::{TcpListener, TcpStream};
use tokio::time::timeout;

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
        assert!(parsed.selected_tools().is_empty());
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

    let postgres = test_connection_profile(config(SqlDialect::Postgres));
    assert_eq!(postgres.host, "database.example.test");
    assert_eq!(postgres.port, 5432);
    assert_eq!(postgres.username, "application_user");
    assert_eq!(postgres.database, "application_db");
    assert_eq!(postgres.tls, "verify-full");
    let mysql = test_connection_profile(config(SqlDialect::MySql));
    assert_eq!(mysql.port, 3306);
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
        );
        assert_eq!(profile.port, port);
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
    let client = SqlClient::new(mysql_config_for_port(port));
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
    let client = SqlClient::new(mysql_config_for_port(port));
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

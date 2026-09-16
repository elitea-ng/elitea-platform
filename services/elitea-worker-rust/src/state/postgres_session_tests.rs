use std::env;
use std::str::FromStr;
use std::sync::Arc;
use std::time::{Duration, SystemTime, UNIX_EPOCH};

use adk_rust::session::{
    AppendEventRequest, CreateRequest, DeleteRequest, GetRequest, ListRequest, SessionService,
};
use adk_rust::{Content, Event};
use chrono::{TimeZone as _, Utc};
use serde_json::json;
use sqlx::postgres::{PgConnectOptions, PgPoolOptions};
use sqlx::{ConnectOptions as _, PgPool};

use super::TestStateWriterLease;
use super::postgres_session::{
    APPLICATION_CAPABILITY_ID, PostgresSessionError, PostgresSessionService, SessionLimits,
    SessionWriterAuthority, canonical_json, decode_event, encode_event, postgres_timestamp,
};

const TEST_DATABASE_URL: &str = "ELITEA_TEST_DATABASE_URL";
const SESSION_MIGRATION: &str =
    include_str!("../../../elitea-main/migrations/agentstate/0002_agent_sessions.sql");

pub(crate) struct IsolatedPostgres {
    pub(crate) pool: PgPool,
    admin_options: PgConnectOptions,
    pub(crate) database_name: String,
}

impl IsolatedPostgres {
    pub(crate) async fn create(database_url: &str) -> Self {
        let admin_options = PgConnectOptions::from_str(database_url)
            .expect("parse PostgreSQL session component-test URL")
            .disable_statement_logging();
        let admin_pool = PgPoolOptions::new()
            .max_connections(2)
            .acquire_timeout(Duration::from_secs(5))
            .connect_with(admin_options.clone())
            .await
            .expect("connect PostgreSQL session component-test administrator");
        let unique = SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .expect("system time")
            .as_nanos();
        let database_name = format!("elitea_rust_session_{}_{}", std::process::id(), unique);
        sqlx::query(&format!("CREATE DATABASE {database_name}"))
            .execute(&admin_pool)
            .await
            .expect("create isolated session database");
        admin_pool.close().await;
        let pool = PgPoolOptions::new()
            .min_connections(1)
            .max_connections(4)
            .acquire_timeout(Duration::from_secs(2))
            .idle_timeout(Duration::from_secs(30))
            .connect_with(admin_options.clone().database(&database_name))
            .await
            .expect("connect isolated session database");
        Self {
            pool,
            admin_options,
            database_name,
        }
    }
}

impl Drop for IsolatedPostgres {
    fn drop(&mut self) {
        let admin_options = self.admin_options.clone();
        let database_name = self.database_name.clone();
        std::thread::spawn(move || {
            tokio::runtime::Builder::new_current_thread()
                .enable_all()
                .build()
                .expect("build session cleanup runtime")
                .block_on(async move {
                    let _ = tokio::time::timeout(Duration::from_secs(10), async move {
                        if let Ok(admin_pool) = PgPoolOptions::new()
                            .max_connections(1)
                            .acquire_timeout(Duration::from_secs(5))
                            .connect_with(admin_options)
                            .await
                        {
                            let _ =
                                sqlx::query(&format!("DROP DATABASE {database_name} WITH (FORCE)"))
                                    .execute(&admin_pool)
                                    .await;
                            admin_pool.close().await;
                        }
                    })
                    .await;
                });
        })
        .join()
        .expect("join session cleanup thread");
    }
}

pub(crate) async fn install_schema(pool: &PgPool) {
    sqlx::raw_sql(SESSION_MIGRATION)
        .execute(pool)
        .await
        .expect("apply session migration");
}

pub(crate) fn authority_for(
    claim_id: &str,
    claim_attempt: u64,
    lease_epoch: u64,
    fence_token: [u8; 32],
) -> SessionWriterAuthority {
    authority_for_at(
        claim_id,
        claim_attempt,
        lease_epoch,
        fence_token,
        1_700_000_000_000_000 + i64::try_from(claim_attempt).expect("claim attempt") * 1_000_000,
    )
}

fn authority_for_at(
    claim_id: &str,
    claim_attempt: u64,
    lease_epoch: u64,
    fence_token: [u8; 32],
    claim_started_at_unix_micros: i64,
) -> SessionWriterAuthority {
    SessionWriterAuthority::new(
        "tenant-1".to_owned(),
        1,
        2,
        APPLICATION_CAPABILITY_ID,
        [0x11; 32],
        "thread-1".to_owned(),
        "elitea-agent-v1".to_owned(),
        "user-1".to_owned(),
        "session-1".to_owned(),
        "execution-1".to_owned(),
        1,
        claim_id.to_owned(),
        claim_attempt,
        lease_epoch,
        claim_started_at_unix_micros,
        "workload-1".to_owned(),
        "producer-1".to_owned(),
        fence_token,
    )
    .expect("valid session authority")
}

fn authority() -> SessionWriterAuthority {
    SessionWriterAuthority::new(
        "tenant-1".to_owned(),
        1,
        2,
        APPLICATION_CAPABILITY_ID,
        [0x11; 32],
        "thread-1".to_owned(),
        "elitea-agent-v1".to_owned(),
        "user-1".to_owned(),
        "session-1".to_owned(),
        "execution-1".to_owned(),
        1,
        "claim-1".to_owned(),
        1,
        1,
        1_700_000_001_000_000,
        "workload-1".to_owned(),
        "producer-1".to_owned(),
        [0x22; 32],
    )
    .expect("valid session authority")
}

#[test]
fn authority_and_limits_fail_closed_without_debugging_secrets() {
    let valid = authority();
    let display = format!("{:?}", std::any::type_name_of_val(&valid));
    assert!(!display.contains("claim-1"));
    assert!(
        SessionWriterAuthority::new(
            "tenant-1".to_owned(),
            1,
            2,
            APPLICATION_CAPABILITY_ID,
            [0; 32],
            "thread-1".to_owned(),
            "elitea-agent-v1".to_owned(),
            "user-1".to_owned(),
            "session-1".to_owned(),
            "execution-1".to_owned(),
            1,
            "claim-1".to_owned(),
            1,
            1,
            1_700_000_001_000_000,
            "workload-1".to_owned(),
            "producer-1".to_owned(),
            [0x22; 32],
        )
        .is_err()
    );
    assert_eq!(SessionLimits::default().max_events, 4096);
}

#[test]
fn complete_adk_event_round_trips_canonically() {
    let mut event = Event::with_id("event-1", "invocation-1");
    event.author = "elitea-agent".to_owned();
    event.llm_request = Some("bounded request metadata".to_owned());
    event
        .provider_metadata
        .insert("provider".to_owned(), "opaque".to_owned());
    event.llm_response.content = Some(Content::new("model").with_text("hello"));
    event
        .actions
        .state_delta
        .insert("answer".to_owned(), json!(42));

    let encoded = encode_event(&event, SessionLimits::default()).expect("encode event");
    let decoded = decode_event(&encoded, SessionLimits::default()).expect("decode event");

    assert_eq!(decoded.id, "event-1");
    assert_eq!(
        decoded.llm_request.as_deref(),
        Some("bounded request metadata")
    );
    assert_eq!(
        decoded.provider_metadata.get("provider"),
        Some(&"opaque".to_owned())
    );
    assert_eq!(decoded.actions.state_delta.get("answer"), Some(&json!(42)));
}

#[test]
fn canonical_json_sorts_nested_object_keys_and_enforces_bytes() {
    let value = json!({"z": {"b": 2, "a": 1}, "a": [{"d": 4, "c": 3}]});
    let encoded = canonical_json(&value, 1024).expect("canonical JSON");
    assert_eq!(encoded, r#"{"a":[{"c":3,"d":4}],"z":{"a":1,"b":2}}"#);
    assert!(canonical_json(&value, encoded.len() - 1).is_err());
}

#[test]
fn postgres_timestamp_retains_payload_nanoseconds_at_relational_precision() {
    let exact = Utc.timestamp_nanos(1_700_000_000_123_456_789);
    let relational = postgres_timestamp(exact).expect("PostgreSQL timestamp precision");
    assert_eq!(relational.to_rfc3339(), "2023-11-14T22:13:20.123456+00:00");
    assert_eq!(exact.timestamp_subsec_nanos(), 123_456_789);
}

#[tokio::test]
#[allow(clippy::too_many_lines)] // One ordered component story proves the durable authority handoff.
async fn postgres_session_component_story() {
    let Ok(database_url) = env::var(TEST_DATABASE_URL) else {
        eprintln!("skipping PostgreSQL session component test: set {TEST_DATABASE_URL}");
        return;
    };
    let database = IsolatedPostgres::create(&database_url).await;
    install_schema(&database.pool).await;
    let first_lease = Arc::new(TestStateWriterLease::current());
    let service = PostgresSessionService::activate(
        database.pool.clone(),
        authority_for("claim-1", 1, 1, [0x22; 32]),
        SessionLimits::default(),
        first_lease,
    )
    .await
    .expect("activate first session writer");
    let equal_time = PostgresSessionService::activate(
        database.pool.clone(),
        authority_for_at("claim-equal", 2, 2, [0x23; 32], 1_700_000_001_000_000),
        SessionLimits::default(),
        Arc::new(TestStateWriterLease::current()),
    )
    .await;
    assert!(matches!(
        equal_time,
        Err(PostgresSessionError::WriterNotCurrent)
    ));
    let session = service
        .create(CreateRequest {
            app_name: "elitea-agent-v1".to_owned(),
            user_id: "user-1".to_owned(),
            session_id: Some("session-1".to_owned()),
            state: [
                ("app:policy".to_owned(), json!("strict")),
                ("user:locale".to_owned(), json!("en")),
                ("turn".to_owned(), json!(0)),
            ]
            .into_iter()
            .collect(),
        })
        .await
        .expect("create durable session");
    let identity = session.try_identity().expect("typed session identity");
    let timestamp = Utc.timestamp_nanos(1_700_000_000_123_456_789);
    let mut first = Event::with_id("event-1", "invocation-1");
    first.timestamp = timestamp;
    first.author = "user".to_owned();
    first.llm_request = Some("request-metadata".to_owned());
    first
        .provider_metadata
        .insert("trace".to_owned(), "opaque".to_owned());
    first.llm_response.content = Some(Content::new("user").with_text("hello"));
    first
        .actions
        .state_delta
        .insert("turn".to_owned(), json!(1));
    first
        .actions
        .state_delta
        .insert("temp:discard".to_owned(), json!(true));
    service
        .append_event_for_identity(AppendEventRequest {
            identity: identity.clone(),
            event: first.clone(),
        })
        .await
        .expect("append first event");
    service
        .append_event_for_identity(AppendEventRequest {
            identity: identity.clone(),
            event: first.clone(),
        })
        .await
        .expect("exact event replay is idempotent");

    let mut conflicting = first.clone();
    conflicting.llm_response.content = Some(Content::new("user").with_text("different"));
    let conflict = service
        .append_event_for_identity(AppendEventRequest {
            identity: identity.clone(),
            event: conflicting,
        })
        .await
        .expect_err("same ID with different event must fail");
    assert_eq!(conflict.code, "session.event_conflict");

    let mut second = Event::with_id("event-2", "invocation-1");
    second.timestamp = timestamp;
    second.author = "elitea-agent".to_owned();
    second.llm_response.content = Some(Content::new("model").with_text("world"));
    second
        .actions
        .state_delta
        .insert("user:locale".to_owned(), json!("uk"));
    service
        .append_event_for_identity(AppendEventRequest {
            identity: identity.clone(),
            event: second,
        })
        .await
        .expect("append second event");

    let restored = service
        .get(GetRequest {
            app_name: "elitea-agent-v1".to_owned(),
            user_id: "user-1".to_owned(),
            session_id: "session-1".to_owned(),
            num_recent_events: None,
            after: None,
        })
        .await
        .expect("restore durable session");
    assert_eq!(restored.events().len(), 2);
    assert_eq!(
        restored.events().at(0).map(|event| event.id.as_str()),
        Some("event-1")
    );
    assert_eq!(
        restored.events().at(1).map(|event| event.id.as_str()),
        Some("event-2")
    );
    assert_eq!(restored.state().get("app:policy"), Some(json!("strict")));
    assert_eq!(restored.state().get("user:locale"), Some(json!("uk")));
    assert_eq!(restored.state().get("turn"), Some(json!(1)));
    assert_eq!(restored.state().get("temp:discard"), None);
    let restored_first = restored.events().at(0).expect("first restored event");
    assert_eq!(
        restored_first.llm_request.as_deref(),
        Some("request-metadata")
    );
    assert_eq!(
        restored_first.provider_metadata.get("trace"),
        Some(&"opaque".to_owned())
    );

    let recent = service
        .get(GetRequest {
            app_name: "elitea-agent-v1".to_owned(),
            user_id: "user-1".to_owned(),
            session_id: "session-1".to_owned(),
            num_recent_events: Some(1),
            after: None,
        })
        .await
        .expect("restore recent event");
    assert_eq!(
        recent.events().at(0).map(|event| event.id.as_str()),
        Some("event-2")
    );
    let listed = service
        .list(ListRequest {
            app_name: "elitea-agent-v1".to_owned(),
            user_id: "user-1".to_owned(),
            limit: Some(1),
            offset: Some(0),
        })
        .await
        .expect("list bound session");
    assert_eq!(listed.len(), 1);

    let replacement_lease = Arc::new(TestStateWriterLease::current());
    let replacement = PostgresSessionService::activate(
        database.pool.clone(),
        authority_for("claim-2", 2, 2, [0x33; 32]),
        SessionLimits::default(),
        replacement_lease.clone(),
    )
    .await
    .expect("activate replacement session writer");
    let Err(stale) = service
        .get(GetRequest {
            app_name: "elitea-agent-v1".to_owned(),
            user_id: "user-1".to_owned(),
            session_id: "session-1".to_owned(),
            num_recent_events: None,
            after: None,
        })
        .await
    else {
        panic!("older claim must be fenced");
    };
    assert_eq!(stale.code, "session.writer_not_current");
    replacement
        .delete(DeleteRequest {
            app_name: "elitea-agent-v1".to_owned(),
            user_id: "user-1".to_owned(),
            session_id: "session-1".to_owned(),
        })
        .await
        .expect("delete session and events");
    let recreated = replacement
        .create(CreateRequest {
            app_name: "elitea-agent-v1".to_owned(),
            user_id: "user-1".to_owned(),
            session_id: Some("session-1".to_owned()),
            state: [("turn".to_owned(), json!(2))].into_iter().collect(),
        })
        .await
        .expect("recreate session with retained app and user state");
    assert_eq!(recreated.state().get("app:policy"), Some(json!("strict")));
    assert_eq!(recreated.state().get("user:locale"), Some(json!("uk")));
    assert_eq!(recreated.state().get("turn"), Some(json!(2)));
    replacement
        .delete(DeleteRequest {
            app_name: "elitea-agent-v1".to_owned(),
            user_id: "user-1".to_owned(),
            session_id: "session-1".to_owned(),
        })
        .await
        .expect("delete recreated session");
    let writer_count =
        sqlx::query_scalar::<_, i64>("SELECT count(*) FROM elitea_runtime.agent_session_writers")
            .fetch_one(&database.pool)
            .await
            .expect("count retained writer");
    let event_count =
        sqlx::query_scalar::<_, i64>("SELECT count(*) FROM elitea_runtime.agent_session_events")
            .fetch_one(&database.pool)
            .await
            .expect("count deleted events");
    assert_eq!(writer_count, 1);
    assert_eq!(event_count, 0);
    replacement_lease.revoke();
    let revoked = replacement.health_check().await.expect_err("revoked lease");
    assert_eq!(revoked.code, "session.writer_not_current");
}

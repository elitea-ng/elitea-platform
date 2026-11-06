//! FU-287 on real `PostgreSQL`: the incremental refresh equals the full read.
//!
//! Every test drives `IncrementalRunnerView` over a real session service and
//! compares it with the full `get` path (`RunnerSessionService::get` is that
//! path plus `runner_history::project`, which both share). Every refresh in
//! this build also cross-checks itself (`runner_view::cross_check_enabled`).

use std::collections::HashMap;
use std::sync::Arc;
use std::time::{Duration, Instant};

use adk_runner::SessionEventRefresh as _;
use adk_rust::session::{AppendEventRequest, CreateRequest, GetRequest, SessionService};
use adk_rust::{AdkIdentity, Content, Event, LlmRequest};
use serde_json::{Value, json};
use sqlx::Row as _;

use super::runner_view::{IncrementalRunnerView, mirrored_active_ordinals};
use crate::state::postgres_session_tests::{IsolatedPostgres, authority_for, install_schema};
use crate::state::{
    ClassifiedEvent, PostgresSessionService, SessionLimits, TestStateWriterLease,
    active_events_sql, classified_events_sql,
};

const DATABASE_ENV: &str = "ELITEA_TEST_DATABASE_URL";
const APP: &str = "elitea-agent-v1";
const COMPACTION_KEY: &str = "elitea.context.root.v1";
/// The digest the snapshot checkpoints below carry and the view's scope names.
const DEFINITION: [u8; 32] = [34; 32];

struct Fixture {
    db: IsolatedPostgres,
    service: Arc<PostgresSessionService>,
    identity: AdkIdentity,
}

impl Fixture {
    async fn new(limits: SessionLimits) -> Option<Self> {
        let Ok(url) = crate::test_pg::database_url_var(DATABASE_ENV) else {
            eprintln!("skipping PostgreSQL runner view test: set {DATABASE_ENV}");
            return None;
        };
        let db = IsolatedPostgres::create(&url).await;
        install_schema(&db.pool).await;
        let service = Arc::new(
            PostgresSessionService::activate(
                db.pool.clone(),
                authority_for("claim-1", 1, 1, [0x22; 32]),
                limits,
                Arc::new(TestStateWriterLease::current()),
            )
            .await
            .expect("activate the session writer"),
        );
        let session = service
            .create(CreateRequest {
                app_name: APP.to_owned(),
                user_id: "user-1".to_owned(),
                session_id: Some("session-1".to_owned()),
                state: HashMap::new(),
            })
            .await
            .expect("create the session");
        let identity = session.try_identity().expect("identity");
        Some(Self {
            db,
            service,
            identity,
        })
    }

    fn view(&self, scope: Option<([u8; 32], String)>) -> IncrementalRunnerView {
        IncrementalRunnerView::new(Arc::clone(&self.service), self.identity.clone(), scope)
    }

    async fn append(&self, event: Event) -> adk_rust::Result<()> {
        self.service
            .append_event_for_identity(AppendEventRequest {
                identity: self.identity.clone(),
                event,
            })
            .await
    }

    async fn stored(&self) -> Vec<Event> {
        self.service
            .get(GetRequest {
                app_name: APP.to_owned(),
                user_id: "user-1".to_owned(),
                session_id: "session-1".to_owned(),
                num_recent_events: None,
                after: None,
            })
            .await
            .expect("full read")
            .events()
            .all()
    }
}

fn encode(events: &[Event]) -> Vec<Value> {
    events
        .iter()
        .map(|event| serde_json::to_value(event).expect("encode event"))
        .collect()
}

fn event(id: &str, author: &str) -> Event {
    let mut event = Event::with_id(id, "invocation-1");
    event.author = author.to_owned();
    event.set_content(
        Content::new(if author == "user" { "user" } else { "model" })
            .with_text(format!("content of {id}")),
    );
    event
}

fn branch_event(id: &str, branch: &str) -> Event {
    let mut event = event(id, "agent");
    event.branch = branch.to_owned();
    event
}

fn control_event(id: &str) -> Event {
    let mut event = event(id, "agent");
    event.actions.transfer_to_agent = Some("child".to_owned());
    event
}

/// A recovery marker. With `snapshot` it is a model-history snapshot of that agent.
fn marker(id: &str, branch: &str, snapshot: Option<&str>, padding: usize) -> Event {
    let mut marker = Event::with_id(id, "invocation-1");
    marker.author = "elitea-recovery".to_owned();
    marker.branch = branch.to_owned();
    match snapshot {
        Some(agent) => {
            let request = LlmRequest::new(
                "model",
                vec![
                    Content::new("user").with_text("Original instruction"),
                    Content::new("model")
                        .with_text(format!("Summary {id} {}", "x".repeat(padding))),
                ],
            );
            marker.actions.state_delta.insert(
                "elitea.agent.history_snapshot.v1".to_owned(),
                json!({"version": 1, "agent_name": agent}),
            );
            marker.actions.state_delta.insert(
                "elitea.agent.recovery.v1".to_owned(),
                json!({
                    "version": 1, "execution_id": "execution-1", "generation": 1,
                    "definition_digest": DEFINITION.to_vec(), "invocation_id": "invocation-1",
                    "phase": "model_pending", "model": {"request": request, "tools": {}},
                }),
            );
        }
        None => {
            marker.actions.state_delta.insert(
                "elitea.agent.recovery.v1".to_owned(),
                json!({"phase": "tool_may_have_started", "padding": "x".repeat(padding)}),
            );
        }
    }
    marker
}

fn compaction_event(id: &str, value: Value) -> Event {
    let mut event = event(id, "agent");
    event
        .actions
        .state_delta
        .insert(COMPACTION_KEY.to_owned(), value);
    event
}

/// Refresh and require both the cached rows and the served view to equal the
/// full read, event for event.
async fn assert_equals_full(view: &IncrementalRunnerView, fixture: &Fixture, step: &str) {
    let refreshed = view
        .refresh_for_tests()
        .await
        .unwrap_or_else(|error| panic!("{step}: refresh failed: {error}"));
    let full = view
        .full_read_for_tests()
        .await
        .unwrap_or_else(|error| panic!("{step}: full read failed: {error}"));
    assert_eq!(encode(&refreshed), encode(&full), "{step}: served view");
    if let Some(cached) = view.cached_events().await {
        assert_eq!(
            encode(&cached),
            encode(&fixture.stored().await),
            "{step}: cached active rows"
        );
    }
}

/// xorshift64*: a deterministic generator, no dependency.
struct Rng(u64);

impl Rng {
    fn next(&mut self) -> u64 {
        self.0 ^= self.0 >> 12;
        self.0 ^= self.0 << 25;
        self.0 ^= self.0 >> 27;
        self.0.wrapping_mul(0x2545_F491_4F6C_DD1D)
    }

    fn below(&mut self, bound: usize) -> usize {
        usize::try_from(self.next() % u64::try_from(bound).expect("bound")).expect("index")
    }
}

#[tokio::test]
#[allow(clippy::too_many_lines)] // One generated story per seed.
async fn generated_event_sequences_give_the_same_view_incrementally_and_in_full() {
    for seed in 1..=6_u64 {
        let Some(fixture) = Fixture::new(SessionLimits::default()).await else {
            return;
        };
        let view = fixture.view(None);
        let mut rng = Rng(0x9E37_79B9_7F4A_7C15 ^ seed.wrapping_mul(0xD1B5_4A32_D192_ED03));
        let mut appended: Vec<Event> = Vec::new();
        let mut compaction = 0_u32;
        assert_equals_full(&view, &fixture, "seed").await;
        for step in 0..70_usize {
            let id = format!("e{step:03}");
            let kind = rng.below(18);
            let event = match kind {
                0..=3 => event(&id, "user"),
                4..=6 => event(&id, "agent"),
                7 => event(&id, "other"),
                8 => control_event(&id),
                9 => marker(&id, "", None, rng.below(400)),
                10 => marker(&id, "", Some("agent"), rng.below(400)),
                11 => marker(&id, "", Some("other"), rng.below(400)),
                12 => branch_event(&id, ["b1", "b2"][rng.below(2)]),
                13 => marker(&id, "b1", None, 0),
                14 => {
                    compaction += 1;
                    compaction_event(&id, json!({"covered": compaction}))
                }
                15 if !appended.is_empty() => {
                    // An exact replay allocates no ordinal and changes nothing.
                    let replay = appended[rng.below(appended.len())].clone();
                    fixture.append(replay).await.expect("exact replay");
                    assert_equals_full(&view, &fixture, &format!("seed {seed} replay {step}"))
                        .await;
                    continue;
                }
                16 => event(&id, "tool"),
                _ => marker(&id, "b1", Some("agent"), 0),
            };
            fixture.append(event.clone()).await.expect("append");
            appended.push(event);
            assert_equals_full(
                &view,
                &fixture,
                &format!("seed {seed} step {step} kind {kind}"),
            )
            .await;
            let state = fixture
                .service
                .get(GetRequest {
                    app_name: APP.to_owned(),
                    user_id: "user-1".to_owned(),
                    session_id: "session-1".to_owned(),
                    num_recent_events: Some(0),
                    after: None,
                })
                .await
                .expect("state read");
            assert_eq!(
                view.compaction_record().await,
                state.state().get(COMPACTION_KEY),
                "seed {seed} step {step}: compaction record"
            );
        }
        let stats = view.stats().snapshot();
        eprintln!("FU287_PROPERTY seed={seed} {stats:?}");
        assert_eq!(stats.seeds, 1 + stats.fallbacks, "{stats:?}");
        fixture.db.pool.close().await;
    }
}

/// The SQL rules are the oracle: raw rows (a NULL author and a NULL branch
/// cannot be decoded into an `Event`, so only SQL ever sees them) classified
/// by the real fragment, then the mirrored rules, must keep the same rows as
/// the real `active_events` CTE.
#[tokio::test]
#[allow(clippy::too_many_lines)] // One generated story over raw rows.
async fn mirrored_retirement_rules_equal_the_sql_rules_including_null_author_and_branch() {
    let Some(fixture) = Fixture::new(SessionLimits::default()).await else {
        return;
    };
    fixture
        .append(event("anchor", "user"))
        .await
        .expect("anchor");
    let mut rng = Rng(0xA076_1D64_78BD_642F);
    let mut ordinal = 2_i64;
    for sequence in 0..40_usize {
        // Each round adds a few raw rows to the session, then compares.
        for _ in 0..=rng.below(3) {
            let author = match rng.below(6) {
                0 => Value::Null,
                1 => json!("user"),
                2 => json!("agent"),
                3 => json!("other"),
                4 => json!("elitea-recovery"),
                _ => json!(""),
            };
            let branch = match rng.below(5) {
                0 => json!("b1"),
                1 => Value::Null,
                _ => json!(""),
            };
            let mut payload = json!({
                "id": format!("raw-{ordinal}"), "author": author, "branch": branch,
                "invocation_id": "invocation-1",
                "timestamp": "2026-01-01T00:00:00Z", "actions": {},
            });
            match rng.below(5) {
                0 => payload["actions"]["transfer_to_agent"] = json!("child"),
                1 => {
                    payload["actions"]["state_delta"] = json!({"elitea.agent.recovery.v1": {
                        "phase": "tool_may_have_started"}});
                }
                2 => {
                    let agent = ["agent", "other"][rng.below(2)];
                    payload["actions"]["state_delta"] = json!({
                        "elitea.agent.history_snapshot.v1":
                            {"version": 1, "agent_name": agent},
                        "elitea.agent.recovery.v1": {
                            "phase": "model_pending",
                            "model": {"request": {"contents": []}},
                        },
                    });
                }
                _ => {}
            }
            sqlx::query(
                r"
INSERT INTO elitea_runtime.agent_session_events (
    tenant_id, resource_project_id, projection_project_id, capability_id,
    session_family, definition_digest, thread_id, app_name, user_id,
    session_id, event_id, event_ordinal, event_timestamp, event_payload,
    writer_claim_id, writer_execution_id, writer_generation,
    writer_claim_attempt, writer_lease_epoch
)
SELECT tenant_id, resource_project_id, projection_project_id, capability_id,
    session_family, definition_digest, thread_id, app_name, user_id,
    session_id, $1, $2, event_timestamp, $3,
    writer_claim_id, writer_execution_id, writer_generation,
    writer_claim_attempt, writer_lease_epoch
FROM elitea_runtime.agent_session_events WHERE event_id = 'anchor'
                ",
            )
            .bind(format!("raw-{ordinal}"))
            .bind(ordinal)
            .bind(payload.to_string())
            .execute(&fixture.db.pool)
            .await
            .expect("insert a raw row");
            ordinal += 1;
        }
        let scope = authority_binds();
        let classified = sqlx::query(sqlx::AssertSqlSafe(format!(
            "{} SELECT event_ordinal, payload_bytes, recovery_marker, branch, snapshot_agent, \
             has_control, author FROM classified ORDER BY event_ordinal",
            classified_events_sql("0")
        )))
        .bind(scope.0)
        .bind(scope.1)
        .bind(scope.2)
        .bind(scope.3)
        .bind(scope.4)
        .bind(scope.5.as_slice())
        .bind(scope.6)
        .bind(scope.7)
        .bind(scope.8)
        .bind(scope.9)
        .fetch_all(&fixture.db.pool)
        .await
        .expect("classify");
        let all = classified
            .iter()
            .map(|row| ClassifiedEvent {
                ordinal: row.get("event_ordinal"),
                event: Event::with_id("raw", "invocation-1"),
                payload_bytes: usize::try_from(row.get::<i64, _>("payload_bytes")).expect("bytes"),
                recovery_marker: row.get("recovery_marker"),
                branch: row.get("branch"),
                snapshot_agent: row.get("snapshot_agent"),
                has_control: row.get("has_control"),
                author: row.get("author"),
            })
            .collect::<Vec<_>>();
        let sql_active: Vec<i64> = sqlx::query_scalar(sqlx::AssertSqlSafe(format!(
            "{} SELECT event_ordinal FROM active_events ORDER BY event_ordinal",
            active_events_sql()
        )))
        .bind(scope.0)
        .bind(scope.1)
        .bind(scope.2)
        .bind(scope.3)
        .bind(scope.4)
        .bind(scope.5.as_slice())
        .bind(scope.6)
        .bind(scope.7)
        .bind(scope.8)
        .bind(scope.9)
        .bind(Option::<&str>::None)
        .bind(false)
        .fetch_all(&fixture.db.pool)
        .await
        .expect("active events");
        assert_eq!(
            mirrored_active_ordinals(&all),
            sql_active,
            "round {sequence}: mirrored rules differ from the SQL rules"
        );
    }
    fixture.db.pool.close().await;
}

/// The ten scope binds of the fixture's session.
fn authority_binds() -> (
    &'static str,
    i32,
    i32,
    &'static str,
    &'static str,
    [u8; 32],
    &'static str,
    &'static str,
    &'static str,
    &'static str,
) {
    (
        "tenant-1",
        1,
        2,
        "agent.execute.application.v1",
        "adk-session.2.0.0.v1",
        [0x11; 32],
        "thread-1",
        APP,
        "user-1",
        "session-1",
    )
}

#[tokio::test]
async fn each_refresh_reads_only_the_new_rows_and_a_whole_run_at_most_twice_the_rows() {
    let Some(fixture) = Fixture::new(SessionLimits::default()).await else {
        return;
    };
    let view = fixture.view(None);
    for seed in 0..3 {
        fixture
            .append(event(&format!("seed-{seed}"), "user"))
            .await
            .expect("seed");
    }
    let stats = view.stats();
    // The first refresh is the seed: it reads the active view once.
    view.refresh_for_tests().await.expect("seed refresh");
    assert_eq!(stats.snapshot().seeds, 1);
    let after_seed = stats.snapshot();
    assert_eq!(after_seed.rows_read, 3);
    let mut appended = 3_u64;
    for step in 0..64_u64 {
        // A step is a model-pending marker (which retires the last one), the
        // model's answer, and now and then a tool result.
        let rows = 2 + u64::from(step % 3 == 0);
        fixture
            .append(marker(&format!("m{step}"), "", None, 4096))
            .await
            .expect("marker");
        fixture
            .append(event(&format!("a{step}"), "agent"))
            .await
            .expect("answer");
        if rows == 3 {
            fixture
                .append(event(&format!("t{step}"), "tool"))
                .await
                .expect("tool result");
        }
        let before = stats.snapshot().rows_read;
        view.refresh_for_tests().await.expect("refresh");
        let read = stats.snapshot().rows_read - before;
        assert_eq!(
            read, rows,
            "step {step}: the refresh reads only its new rows"
        );
        appended += rows;
    }
    let tally = stats.snapshot();
    assert!(
        tally.rows_read <= 2 * appended,
        "a whole run reads at most twice the rows it appended: {tally:?} vs {appended}"
    );
    assert_eq!(tally.fallbacks, 0, "{tally:?}");
    assert_eq!(tally.seeds, 1, "{tally:?}");
    fixture.db.pool.close().await;
}

#[tokio::test]
async fn a_snapshot_cut_a_first_branch_and_another_agents_snapshot_each_read_in_full_once() {
    let Some(fixture) = Fixture::new(SessionLimits::default()).await else {
        return;
    };
    let scope = Some((DEFINITION, "agent".to_owned()));
    let view = fixture.view(scope);
    for index in 0..3 {
        fixture
            .append(event(&format!("u{index}"), "user"))
            .await
            .expect("history");
        fixture
            .append(event(&format!("a{index}"), "agent"))
            .await
            .expect("history");
    }
    assert_equals_full(&view, &fixture, "seeded before any cut").await;
    let stats = view.stats();
    // The snapshot cuts the old user and agent rows. Nothing returns: no re-read.
    fixture
        .append(marker("snap-1", "", Some("agent"), 0))
        .await
        .expect("snapshot");
    assert_equals_full(&view, &fixture, "snapshot cut").await;
    fixture
        .append(event("after-cut", "user"))
        .await
        .expect("row");
    assert_equals_full(&view, &fixture, "row after the cut").await;
    assert_eq!(stats.snapshot().fallbacks, 0, "{:?}", stats.snapshot());
    // A snapshot of the same agent moves the cut forward: no re-read either.
    fixture
        .append(marker("snap-2", "", Some("agent"), 0))
        .await
        .expect("snapshot");
    assert_equals_full(&view, &fixture, "second snapshot").await;
    assert_eq!(stats.snapshot().fallbacks, 0, "{:?}", stats.snapshot());
    // The first branch row brings every cut row back: exactly one full read.
    fixture
        .append(branch_event("first-branch", "b1"))
        .await
        .expect("branch");
    assert_equals_full(&view, &fixture, "first branch").await;
    assert_eq!(stats.snapshot().fallbacks, 1, "{:?}", stats.snapshot());
    // With branches present nothing is cut: later rows need no re-read.
    fixture.append(event("more", "user")).await.expect("row");
    fixture
        .append(marker("snap-3", "", Some("agent"), 0))
        .await
        .expect("snapshot");
    assert_equals_full(&view, &fixture, "after the branch").await;
    assert_eq!(stats.snapshot().fallbacks, 1, "{:?}", stats.snapshot());
    fixture.db.pool.close().await;
}

#[tokio::test]
async fn a_snapshot_of_another_agent_brings_back_rows_and_reads_in_full() {
    let Some(fixture) = Fixture::new(SessionLimits::default()).await else {
        return;
    };
    let view = fixture.view(None);
    fixture.append(event("u0", "user")).await.expect("row");
    fixture.append(event("a0", "agent")).await.expect("row");
    fixture
        .append(marker("snap-1", "", Some("agent"), 0))
        .await
        .expect("snapshot");
    assert_equals_full(&view, &fixture, "cut of the agent").await;
    let kept = view.cached_events().await.expect("cache").len();
    // Another agent's snapshot: the agent's own rows are not covered by it.
    fixture
        .append(marker("snap-2", "", Some("other"), 0))
        .await
        .expect("snapshot");
    assert_equals_full(&view, &fixture, "snapshot of another agent").await;
    assert_eq!(view.stats().snapshot().fallbacks, 1);
    let returned = view.cached_events().await.expect("cache").len();
    assert!(returned > kept, "the cut rows of the first agent came back");
    fixture.db.pool.close().await;
}

#[tokio::test]
async fn compaction_between_refreshes_gives_the_full_projection() {
    use serde_json::to_value;
    let Some(fixture) = Fixture::new(SessionLimits::default()).await else {
        return;
    };
    let scope = Some((DEFINITION, "agent".to_owned()));
    let view = fixture.view(scope);
    fixture.append(event("u0", "user")).await.expect("row");
    for index in 0..4 {
        fixture
            .append(event(&format!("a{index}"), "agent"))
            .await
            .expect("row");
    }
    assert_equals_full(&view, &fixture, "before the delta").await;
    // A record which covers the first two working contents: the real digests.
    let stored = fixture.stored().await;
    let contents = stored
        .iter()
        .filter_map(|event| event.llm_response.content.clone())
        .map(|content| {
            let mut content = content;
            if content.role != "user" {
                content.role = "model".to_owned();
            }
            content
        })
        .collect::<Vec<_>>();
    let anchor = super::context_compaction::extend_digest([0; 32], &contents[..1]).expect("anchor");
    let covered =
        super::context_compaction::extend_digest([0; 32], &contents[1..3]).expect("covered");
    let record = json!({
        "version": 1, "definition_digest": DEFINITION.to_vec(), "anchor": anchor.to_vec(),
        "covered_count": 2, "covered_digest": covered.to_vec(),
        "replacement": [to_value(Content::new("user").with_text("Summary of the first answers"))
            .expect("content")],
    });
    fixture
        .append(compaction_event("compaction", record))
        .await
        .expect("compaction delta");
    let refreshed = view.refresh_for_tests().await.expect("refresh");
    let full = view.full_read_for_tests().await.expect("full");
    assert_eq!(encode(&refreshed), encode(&full));
    assert!(
        refreshed
            .iter()
            .any(|event| event.id.starts_with("context-view-")),
        "the projection replaced the covered prefix: {:?}",
        refreshed.iter().map(|event| &event.id).collect::<Vec<_>>()
    );
    assert_eq!(view.stats().snapshot().fallbacks, 0);
    fixture.db.pool.close().await;
}

#[tokio::test]
async fn a_concurrent_foreign_append_is_never_lost_or_duplicated() {
    let Some(fixture) = Fixture::new(SessionLimits::default()).await else {
        return;
    };
    let view = Arc::new(fixture.view(None));
    let writer = Arc::clone(&fixture.service);
    let identity = fixture.identity.clone();
    let appends = tokio::spawn(async move {
        let mut slowest = Duration::ZERO;
        for index in 0..60 {
            let started = Instant::now();
            writer
                .append_event_for_identity(AppendEventRequest {
                    identity: identity.clone(),
                    event: event(&format!("journal-{index}"), "agent"),
                })
                .await
                .expect("the writer is never blocked past its lock wait");
            slowest = slowest.max(started.elapsed());
        }
        slowest
    });
    let mut refreshes = 0;
    while !appends.is_finished() {
        view.refresh_for_tests().await.expect("refresh");
        refreshes += 1;
    }
    let slowest = appends.await.expect("writer task");
    view.refresh_for_tests().await.expect("final refresh");
    let cached = view.cached_events().await.expect("cache");
    let ids = cached
        .iter()
        .map(|event| event.id.clone())
        .collect::<Vec<_>>();
    let expected = (0..60)
        .map(|index| format!("journal-{index}"))
        .collect::<Vec<_>>();
    assert_eq!(ids, expected, "no row lost, none duplicated, in order");
    assert_eq!(encode(&cached), encode(&fixture.stored().await));
    assert!(refreshes > 1, "the refreshes overlapped the appends");
    assert!(
        slowest < Duration::from_secs(5),
        "an append waited {slowest:?}"
    );
    fixture.db.pool.close().await;
}

#[tokio::test]
async fn the_writer_lock_is_released_when_a_refresh_returns() {
    let Some(fixture) = Fixture::new(SessionLimits {
        writer_lock_timeout: Duration::from_secs(2),
        ..SessionLimits::default()
    })
    .await
    else {
        return;
    };
    let view = fixture.view(None);
    fixture.append(event("one", "user")).await.expect("append");
    for index in 0..10 {
        view.refresh_for_tests().await.expect("refresh");
        // The writer row is free the moment the refresh returned: an exclusive
        // lock without waiting succeeds, and no transaction is left open.
        let mut probe = fixture.db.pool.begin().await.expect("probe");
        sqlx::query("SELECT 1 FROM elitea_runtime.agent_session_writers FOR UPDATE NOWAIT")
            .fetch_all(&mut *probe)
            .await
            .expect("the refresh left the writer row locked");
        probe.rollback().await.expect("rollback");
        let idle: i64 = sqlx::query_scalar(
            "SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() \
             AND state LIKE 'idle in transaction%'",
        )
        .fetch_one(&fixture.db.pool)
        .await
        .expect("activity");
        assert_eq!(idle, 0, "round {index}: a refresh left a transaction open");
        fixture
            .append(event(&format!("next-{index}"), "agent"))
            .await
            .expect("append after a refresh");
    }
    fixture.db.pool.close().await;
}

#[tokio::test]
async fn limit_and_limit_plus_one_active_rows_follow_the_full_read() {
    let small = SessionLimits {
        max_events: 5,
        ..SessionLimits::default()
    };
    let Some(fixture) = Fixture::new(SessionLimits::default()).await else {
        return;
    };
    // The same writer claim, bounded to five active rows for reading.
    let bounded = Arc::new(
        PostgresSessionService::activate(
            fixture.db.pool.clone(),
            authority_for("claim-1", 1, 1, [0x22; 32]),
            small,
            Arc::new(TestStateWriterLease::current()),
        )
        .await
        .expect("activate the bounded reader"),
    );
    let view = IncrementalRunnerView::new(Arc::clone(&bounded), fixture.identity.clone(), None);
    for index in 0..5 {
        fixture
            .append(event(&format!("r{index}"), "user"))
            .await
            .expect("append");
    }
    let at_limit = view
        .refresh_for_tests()
        .await
        .expect("five rows are served");
    assert_eq!(at_limit.len(), 5);
    fixture.append(event("r5", "user")).await.expect("append");
    let over = view
        .refresh_for_tests()
        .await
        .expect_err("six rows pass the bound");
    let full = bounded
        .get(GetRequest {
            app_name: APP.to_owned(),
            user_id: "user-1".to_owned(),
            session_id: "session-1".to_owned(),
            num_recent_events: None,
            after: None,
        })
        .await
        .err()
        .expect("the full read refuses too");
    assert_eq!(over.code, full.code);
    assert_eq!(over.code, "session.resource_exhausted");
    assert!(
        view.cached_events().await.is_none(),
        "nothing stays resident over the bound"
    );
    fixture.db.pool.close().await;
}

#[tokio::test]
async fn an_active_view_over_the_byte_bound_is_read_in_full_and_not_kept() {
    let bounded = SessionLimits {
        max_retained_event_bytes: 2048,
        ..SessionLimits::default()
    };
    let Some(fixture) = Fixture::new(SessionLimits::default()).await else {
        return;
    };
    let reader = Arc::new(
        PostgresSessionService::activate(
            fixture.db.pool.clone(),
            authority_for("claim-1", 1, 1, [0x22; 32]),
            bounded,
            Arc::new(TestStateWriterLease::current()),
        )
        .await
        .expect("activate the bounded reader"),
    );
    let view = IncrementalRunnerView::new(Arc::clone(&reader), fixture.identity.clone(), None);
    for index in 0..14 {
        fixture
            .append(event(&format!("r{index}"), "user"))
            .await
            .expect("append");
    }
    for round in 0..3 {
        let served = view.refresh_for_tests().await.expect("served in full");
        let full = view.full_read_for_tests().await.expect("full");
        assert_eq!(encode(&served), encode(&full), "round {round}");
        assert!(view.cached_events().await.is_none(), "round {round}");
    }
    assert!(view.stats().snapshot().fallbacks >= 3);
    fixture.db.pool.close().await;
}

#[tokio::test]
async fn a_replayed_settle_changes_nothing_and_a_changed_one_conflicts() {
    let Some(fixture) = Fixture::new(SessionLimits::default()).await else {
        return;
    };
    let view = fixture.view(None);
    let settle = event("settle", "agent");
    fixture.append(settle.clone()).await.expect("settle");
    assert_equals_full(&view, &fixture, "settled").await;
    let stats = view.stats();
    let read = stats.snapshot().rows_read;
    fixture.append(settle.clone()).await.expect("exact replay");
    assert_equals_full(&view, &fixture, "replayed").await;
    assert_eq!(
        stats.snapshot().rows_read,
        read,
        "a replay allocates no ordinal"
    );
    let mut changed = settle;
    changed.set_content(Content::new("model").with_text("a different answer"));
    let error = fixture.append(changed).await.expect_err("a changed replay");
    assert_eq!(error.code, "session.event_conflict");
    assert_equals_full(&view, &fixture, "after the conflict").await;
    assert_eq!(stats.snapshot().rows_read, read);
    fixture.db.pool.close().await;
}

#[tokio::test]
async fn a_refresh_after_the_lease_moved_is_a_typed_failure_with_no_stale_view() {
    let Some(fixture) = Fixture::new(SessionLimits::default()).await else {
        return;
    };
    let view = fixture.view(None);
    fixture.append(event("one", "user")).await.expect("append");
    view.refresh_for_tests().await.expect("refresh");
    // A second claim takes the writer over.
    let replacement = PostgresSessionService::activate(
        fixture.db.pool.clone(),
        authority_for("claim-2", 2, 2, [0x33; 32]),
        SessionLimits::default(),
        Arc::new(TestStateWriterLease::current()),
    )
    .await
    .expect("takeover");
    replacement
        .append_event_for_identity(AppendEventRequest {
            identity: fixture.identity.clone(),
            event: event("two", "agent"),
        })
        .await
        .expect("the new writer appends");
    let error = view
        .refresh_for_tests()
        .await
        .expect_err("the old claim is fenced");
    assert_eq!(error.code, "session.writer_not_current");
    fixture.db.pool.close().await;
}

#[tokio::test]
async fn a_revoked_local_lease_stops_the_refresh() {
    let Some(fixture) = Fixture::new(SessionLimits::default()).await else {
        return;
    };
    let lease = Arc::new(TestStateWriterLease::current());
    let service = Arc::new(
        PostgresSessionService::activate(
            fixture.db.pool.clone(),
            authority_for("claim-1", 1, 1, [0x22; 32]),
            SessionLimits::default(),
            Arc::clone(&lease) as Arc<dyn crate::state::StateWriterLease>,
        )
        .await
        .expect("activate"),
    );
    let view = IncrementalRunnerView::new(service, fixture.identity.clone(), None);
    view.refresh_for_tests().await.expect("refresh");
    lease.revoke();
    let error = view.refresh_for_tests().await.expect_err("revoked");
    assert_eq!(error.code, "session.writer_not_current");
    fixture.db.pool.close().await;
}

#[tokio::test]
async fn a_view_serves_only_its_own_session() {
    let Some(fixture) = Fixture::new(SessionLimits::default()).await else {
        return;
    };
    let view = fixture.view(None);
    let mut foreign = fixture.identity.clone();
    foreign.session_id = "session-2".try_into().expect("session id");
    let error = view
        .refreshed_events(&foreign)
        .await
        .expect_err("a foreign session is refused");
    assert_eq!(error.code, "session.not_found");
    let mut other_user = fixture.identity.clone();
    other_user.user_id = "user-2".try_into().expect("user id");
    assert!(view.refreshed_events(&other_user).await.is_err());
    assert_eq!(view.stats().snapshot().refreshes, 0, "nothing was read");
    fixture.db.pool.close().await;
}

/// Collects the `agent.session.persist` spans of `operation = "refresh"` with
/// the `rows_read` and `fallback` fields their owner recorded.
#[derive(Clone, Default)]
struct RefreshSpans(Arc<std::sync::Mutex<HashMap<u64, RecordedRefresh>>>);

/// `(rows_read, fallback, recorded)` of one refresh span.
type RecordedRefresh = (u64, bool, bool);

#[derive(Default)]
struct SpanFields {
    refresh: bool,
    rows_read: Option<u64>,
    fallback: Option<bool>,
}

impl tracing::field::Visit for SpanFields {
    fn record_str(&mut self, field: &tracing::field::Field, value: &str) {
        if field.name() == "operation" && value == "refresh" {
            self.refresh = true;
        }
    }
    fn record_u64(&mut self, field: &tracing::field::Field, value: u64) {
        if field.name() == "rows_read" {
            self.rows_read = Some(value);
        }
    }
    fn record_bool(&mut self, field: &tracing::field::Field, value: bool) {
        if field.name() == "fallback" {
            self.fallback = Some(value);
        }
    }
    fn record_debug(&mut self, _: &tracing::field::Field, _: &dyn std::fmt::Debug) {}
}

impl<S> tracing_subscriber::Layer<S> for RefreshSpans
where
    S: tracing::Subscriber,
{
    fn on_new_span(
        &self,
        attributes: &tracing::span::Attributes<'_>,
        id: &tracing::span::Id,
        _: tracing_subscriber::layer::Context<'_, S>,
    ) {
        let mut fields = SpanFields::default();
        attributes.record(&mut fields);
        if attributes.metadata().name() == "agent.session.persist" && fields.refresh {
            self.0
                .lock()
                .unwrap()
                .insert(id.into_u64(), (0, false, false));
        }
    }

    fn on_record(
        &self,
        id: &tracing::span::Id,
        values: &tracing::span::Record<'_>,
        _: tracing_subscriber::layer::Context<'_, S>,
    ) {
        let mut fields = SpanFields::default();
        values.record(&mut fields);
        if let Some(entry) = self.0.lock().unwrap().get_mut(&id.into_u64()) {
            if let Some(rows) = fields.rows_read {
                entry.0 = rows;
            }
            if let Some(fallback) = fields.fallback {
                entry.1 = fallback;
                entry.2 = true;
            }
        }
    }
}

/// The wiring every Runner assembly site uses: `RunnerSessionService::refreshing`
/// gives the Runner the incremental view, each refresh is one
/// `agent.session.persist` span with `operation = refresh`, and the span carries
/// the rows it read and whether it fell back to a full read.
///
/// The spans are captured in a child process that owns a global subscriber:
/// a thread-local one races parallel tests over callsite interest, which can
/// leave this test's refresh spans unrecorded (the #1172 pattern).
#[test]
fn the_runner_assembly_refreshes_incrementally_and_reports_rows_and_fallback_in_its_span() {
    if crate::test_pg::database_url_var("ELITEA_TEST_DATABASE_URL").is_err() {
        eprintln!("SKIP: set ELITEA_TEST_DATABASE_URL for the refresh span proof");
        return;
    }
    let output = std::process::Command::new(std::env::current_exe().expect("test binary"))
        .args([
            "--exact",
            "agents::runner_view_pg_tests::refresh_span_child",
            "--nocapture",
            "--test-threads=1",
        ])
        .env(REFRESH_SPAN_CHILD_ENV, "1")
        .output()
        .expect("refresh span child process");
    let logged = format!(
        "{}{}",
        String::from_utf8_lossy(&output.stdout),
        String::from_utf8_lossy(&output.stderr)
    );
    assert!(output.status.success(), "{logged}");
    assert!(logged.contains("REFRESH-SPAN-CHILD-RAN"), "{logged}");
}

const REFRESH_SPAN_CHILD_ENV: &str = "ELITEA_REFRESH_SPAN_CHILD";

#[tokio::test(flavor = "current_thread")]
async fn refresh_span_child() {
    use adk_rust::futures::StreamExt as _;
    use tracing_subscriber::layer::SubscriberExt as _;
    if std::env::var(REFRESH_SPAN_CHILD_ENV).is_err() {
        return;
    }
    let Some(fixture) = Fixture::new(SessionLimits::default()).await else {
        return;
    };
    // Whole-run cross-check stays on: the Runner's own refreshes are compared.
    let spans = RefreshSpans::default();
    tracing::subscriber::set_global_default(tracing_subscriber::registry().with(spans.clone()))
        .expect("the child owns the global subscriber");
    let sessions = Arc::new(
        super::session::RunnerSessionService::new(fixture.service.clone(), None)
            .with_history_scope(DEFINITION, "recording-probe")
            .with_event_source(Some(Arc::clone(&fixture.service))),
    );
    let runner = adk_rust::runner::Runner::builder()
        .app_name(APP)
        .agent(Arc::new(
            super::native_history_tests::RecordingStreamProbe::new(),
        ))
        .session_service(sessions.clone())
        .build()
        .expect("runner")
        .with_session_event_refresh(true);
    let runner = sessions.refreshing(runner, fixture.identity.clone());
    let mut stream = runner
        .run(
            "user-1".try_into().expect("user"),
            "session-1".try_into().expect("session"),
            Content::new("user").with_text("Run through the assembly"),
        )
        .await
        .expect("run");
    while let Some(event) = stream.next().await {
        event.expect("a refreshed run");
    }
    let spans = spans
        .0
        .lock()
        .unwrap()
        .values()
        .copied()
        .collect::<Vec<_>>();
    // Three persisted non-partial events, each followed by a refresh.
    assert_eq!(spans.len(), 3, "{spans:?}");
    assert!(spans.iter().all(|(_, _, recorded)| *recorded), "{spans:?}");
    // The first refresh seeds (not a fallback); the later ones read one row each.
    assert_eq!(spans.iter().filter(|(_, fallback, _)| *fallback).count(), 0);
    let rows = spans.iter().map(|(rows, _, _)| *rows).collect::<Vec<_>>();
    assert!(rows.iter().all(|rows| *rows <= 2), "{rows:?}");
    fixture.db.pool.close().await;
    println!("REFRESH-SPAN-CHILD-RAN");
}

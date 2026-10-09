//! Real-PostgreSQL proof that a forwarded node event never strands a child
//! model-scope append mid-transaction (2026-10-09 "answer card 1" hang).
//!
//! A child append locks the ROOT session writer `FOR SHARE` before its own
//! row. If the wrapper yields a node event while that append is suspended, the
//! Runner's root append of the yielded event waits for the root writer, and the
//! child append is never polled again to release it.

use std::str::FromStr as _;
use std::sync::Arc;
use std::time::Duration;

use adk_rust::futures::StreamExt as _;
use adk_rust::session::{AppendEventRequest, CreateRequest, GetRequest, SessionService};
use adk_rust::{AdkIdentity, Agent, Content, Event, EventStream, InvocationContext};
use async_trait::async_trait;
use sqlx::postgres::{PgConnectOptions, PgPoolOptions};
use sqlx::{ConnectOptions as _, PgPool};

use super::{PipelineNodeEventStreamingAgent, pipeline_node_event_channel};
use crate::state::postgres_session_tests::{IsolatedPostgres, authority_for, install_schema};
use crate::state::{PostgresSessionService, SessionLimits, TestStateWriterLease};

pub(crate) const APP: &str = "elitea-agent-v1";
pub(crate) const USER: &str = "user-1";
pub(crate) const ROOT_SESSION: &str = "session-1";
pub(crate) const NODE_EVENT_ID: &str = "forwarded-node-event";
pub(crate) const ROOT_EVENT_ID: &str = "root-terminal-event";
const CHILD_EVENT_ID: &str = "child-model-event";
pub(crate) const STEP_DEADLINE: Duration = Duration::from_secs(10);

pub(crate) fn child_session_id() -> String {
    format!("elitea-model-{}", "a".repeat(64))
}

/// One graph step: a child model-scope append, then the root's own event.
pub(crate) struct ChildAppendThenAnswer {
    pub(crate) child: Arc<PostgresSessionService>,
}

#[async_trait]
impl Agent for ChildAppendThenAnswer {
    fn name(&self) -> &'static str {
        "root-agent"
    }

    fn description(&self) -> &'static str {
        "test"
    }

    fn sub_agents(&self) -> &[Arc<dyn Agent>] {
        &[]
    }

    async fn run(&self, ctx: Arc<dyn InvocationContext>) -> adk_rust::Result<EventStream> {
        let child = self.child.clone();
        let invocation_id = ctx.invocation_id().to_owned();
        let branch = ctx.branch().to_owned();
        Ok(Box::pin(async_stream::try_stream! {
            let mut child_event = Event::with_id(CHILD_EVENT_ID, "child-invocation");
            child_event.author = "child-agent".to_owned();
            child_event.set_content(Content::new("model").with_text("child answer"));
            child
                .append_event_for_identity(AppendEventRequest {
                    identity: AdkIdentity::new(
                        APP.try_into()?,
                        USER.try_into()?,
                        child_session_id().as_str().try_into()?,
                    ),
                    event: child_event,
                })
                .await?;
            let mut answer = Event::with_id(ROOT_EVENT_ID, &invocation_id);
            answer.author = "root-agent".to_owned();
            answer.branch = branch;
            answer.set_content(Content::new("model").with_text("root answer"));
            yield answer;
        }))
    }
}

/// Root writer, child model scope, both sessions created, and a side pool
/// (gate + lock monitor) that never shares the services' connections.
pub(crate) struct LockFixture {
    pub(crate) database: IsolatedPostgres,
    pub(crate) side: PgPool,
    pub(crate) root: Arc<PostgresSessionService>,
    pub(crate) child: Arc<PostgresSessionService>,
}

impl LockFixture {
    pub(crate) async fn create(url: &str) -> Self {
        let database = IsolatedPostgres::create(url).await;
        install_schema(&database.pool).await;
        let side = PgPoolOptions::new()
            .max_connections(2)
            .acquire_timeout(STEP_DEADLINE)
            .connect_with(
                PgConnectOptions::from_str(url)
                    .expect("parse test URL")
                    .disable_statement_logging()
                    .database(&database.database_name),
            )
            .await
            .expect("connect side pool");
        let root = Arc::new(
            PostgresSessionService::activate(
                database.pool.clone(),
                authority_for("claim-1", 1, 1, [1; 32]),
                SessionLimits::default(),
                Arc::new(TestStateWriterLease::current()),
            )
            .await
            .expect("activate root writer"),
        );
        create_session(&root, ROOT_SESSION).await;
        let child_id = child_session_id();
        let child = Arc::new(
            root.model_scope(&AdkIdentity::new(
                APP.try_into().expect("app"),
                USER.try_into().expect("user"),
                child_id.as_str().try_into().expect("child session"),
            ))
            .await
            .expect("activate child model scope"),
        );
        create_session(&child, &child_id).await;
        Self {
            database,
            side,
            root,
            child,
        }
    }

    /// Hold the child's session row so its append parks inside
    /// `merge_session_state`, AFTER it locked the root writer `FOR SHARE`.
    pub(crate) async fn gate(&self) -> (sqlx::Transaction<'static, sqlx::Postgres>, i32) {
        let mut gate = self.side.begin().await.expect("begin gate");
        let pid = sqlx::query_scalar::<_, i32>("SELECT pg_backend_pid()")
            .fetch_one(&mut *gate)
            .await
            .expect("gate pid");
        sqlx::query("SELECT 1 FROM elitea_runtime.agent_sessions WHERE session_id = $1 FOR UPDATE")
            .bind(child_session_id())
            .fetch_one(&mut *gate)
            .await
            .expect("hold child session row");
        (gate, pid)
    }

    /// Wait until some backend of this database is blocked by `blocker`.
    pub(crate) async fn blocked_by(&self, blocker: i32) -> Option<i32> {
        tokio::time::timeout(STEP_DEADLINE, async {
            loop {
                let waiting = sqlx::query_scalar::<_, i32>(
                    "SELECT pid FROM pg_stat_activity \
                     WHERE datname = $1 AND $2 = ANY(pg_blocking_pids(pid)) LIMIT 1",
                )
                .bind(&self.database.database_name)
                .bind(blocker)
                .fetch_optional(&self.side)
                .await
                .expect("inspect lock waits");
                if let Some(pid) = waiting {
                    return pid;
                }
                tokio::time::sleep(Duration::from_millis(20)).await;
            }
        })
        .await
        .ok()
    }

    pub(crate) async fn stored_ids(
        &self,
        service: &PostgresSessionService,
        session: &str,
    ) -> Vec<String> {
        service
            .get(GetRequest {
                app_name: APP.to_owned(),
                user_id: USER.to_owned(),
                session_id: session.to_owned(),
                num_recent_events: None,
                after: None,
            })
            .await
            .expect("reload session")
            .events()
            .all()
            .into_iter()
            .map(|event| event.id)
            .collect()
    }

    pub(crate) fn runner(&self, agent: Arc<dyn Agent>) -> adk_rust::runner::Runner {
        adk_rust::runner::Runner::builder()
            .app_name(APP)
            .agent(agent)
            .session_service(self.root.clone() as Arc<dyn SessionService>)
            .build()
            .expect("runner")
    }
}

async fn create_session(service: &PostgresSessionService, session_id: &str) {
    service
        .create(CreateRequest {
            app_name: APP.to_owned(),
            user_id: USER.to_owned(),
            session_id: Some(session_id.to_owned()),
            state: std::collections::HashMap::new(),
        })
        .await
        .expect("create durable session");
}

/// Drive one Runner turn to completion and return the yielded event ids.
pub(crate) fn spawn_turn(
    runner: adk_rust::runner::Runner,
) -> tokio::task::JoinHandle<adk_rust::Result<Vec<String>>> {
    tokio::spawn(async move {
        let mut events = runner
            .run(
                USER.try_into()?,
                ROOT_SESSION.try_into()?,
                Content::new("user").with_text("answer card 1"),
            )
            .await?;
        let mut ids = Vec::new();
        while let Some(event) = events.next().await {
            ids.push(event?.id);
        }
        Ok(ids)
    })
}

pub(crate) fn node_event() -> Event {
    let mut event = Event::with_id(NODE_EVENT_ID, "pipeline-child:call-1");
    event.author = "child-agent".to_owned();
    event.set_content(Content::new("model").with_text("progress"));
    event
}

/// The ordered interleaving shared by every wrapper: child append parked
/// holding the root writer, a forwarded event arrives, the Runner's root
/// append waits on the child, then the gate opens.
pub(crate) async fn assert_no_self_deadlock<F, Fut>(
    fixture: &LockFixture,
    runner: adk_rust::runner::Runner,
    signal: F,
) where
    F: FnOnce() -> Fut,
    Fut: std::future::Future<Output = ()>,
{
    let (gate, gate_pid) = fixture.gate().await;
    let mut run = spawn_turn(runner);
    let child_pid = fixture
        .blocked_by(gate_pid)
        .await
        .expect("the child append must reach the gate holding the root writer");
    signal().await;
    assert!(
        fixture.blocked_by(child_pid).await.is_some(),
        "the Runner's root append must wait on the child's root-writer lock"
    );
    gate.commit().await.expect("release gate");
    let Ok(joined) = tokio::time::timeout(STEP_DEADLINE, &mut run).await else {
        run.abort();
        panic!(
            "self-deadlock: the child append was left suspended while the Runner \
             persisted the forwarded event"
        );
    };
    let ids = joined.expect("run task").expect("run succeeds");
    assert!(ids.iter().any(|id| id == NODE_EVENT_ID), "{ids:?}");
    assert!(ids.iter().any(|id| id == ROOT_EVENT_ID), "{ids:?}");
    let stored = fixture.stored_ids(&fixture.root, ROOT_SESSION).await;
    assert!(stored.iter().any(|id| id == NODE_EVENT_ID), "{stored:?}");
    assert!(stored.iter().any(|id| id == ROOT_EVENT_ID), "{stored:?}");
    let child = fixture
        .stored_ids(&fixture.child, &child_session_id())
        .await;
    assert_eq!(child, vec![CHILD_EVENT_ID.to_owned()]);
}

#[tokio::test]
async fn node_event_yield_never_strands_a_child_append_holding_the_root_writer() {
    let Ok(url) = std::env::var("ELITEA_TEST_DATABASE_URL") else {
        eprintln!("skipping node-event session lock test: set ELITEA_TEST_DATABASE_URL");
        return;
    };
    let fixture = LockFixture::create(&url).await;
    let (sender, receiver) = pipeline_node_event_channel();
    let agent = PipelineNodeEventStreamingAgent::new(
        Arc::new(ChildAppendThenAnswer {
            child: fixture.child.clone(),
        }),
        receiver,
    );
    let runner = fixture.runner(Arc::new(agent));
    assert_no_self_deadlock(&fixture, runner, || async move {
        sender
            .send_routed_application_event(node_event())
            .await
            .expect("queue node event");
    })
    .await;
    fixture.side.close().await;
}

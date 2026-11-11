//! FU-291 risk 1, before any durable pipeline-tool code: what a graph compiled
//! on a durable thread does when it runs again on that thread, through the
//! path the pipeline tool runs its child on (a `Runner` over
//! `EliteaGraphAgent`, which is ADK's `GraphAgent::run`), on the production
//! `PostgresCheckpointer`.
//!
//! * A row with pending nodes re-enters there: a node that completed before
//!   the loss never runs again, with no explicit resume read.
//! * A finished row runs no node (state unchanged; the only event is the
//!   completion marker), but the executor saves one more row: adk-graph 2.3.0
//!   `GraphAgent::run` runs a turn with `restarting_completed_threads`. So the
//!   durable tool reads a completed row and returns its stored result before
//!   it compiles anything, and never writes to a completed thread (D-4, C3).

use std::sync::Arc;

use adk_rust::graph::{Checkpoint, Checkpointer};
use async_trait::async_trait;
use serde_json::json;

use crate::agents::graph::compiler::PipelineDefinition;
use crate::agents::graph::pipeline_pg_test_support::{
    CHECKPOINT_MIGRATION, SESSION, activate, create_session, history, run,
};
use crate::state::postgres_session_tests::{IsolatedPostgres, install_schema};

fn pipeline() -> PipelineDefinition {
    PipelineDefinition::from_yaml(
        r#"
state:
  trail: {type: list, value: [], reducer: append}
entry_point: first
nodes:
  - id: first
    type: state_modifier
    template: '["first"]'
    output: [trail]
    transition: second
  - id: second
    type: state_modifier
    template: '["second"]'
    output: [trail]
    transition: END
"#,
    )
    .expect("two-node pipeline")
}

/// The real checkpointer; once the row that leaves `second` pending is
/// durable, the run never returns, as a Worker lost right after that save.
struct LostAfterFirst(Arc<dyn Checkpointer>);

#[async_trait]
impl Checkpointer for LostAfterFirst {
    async fn save(&self, checkpoint: &Checkpoint) -> adk_rust::graph::Result<String> {
        let id = self.0.save(checkpoint).await?;
        if checkpoint.pending_nodes == ["second"] {
            std::future::pending::<()>().await;
        }
        Ok(id)
    }
    async fn load(&self, thread_id: &str) -> adk_rust::graph::Result<Option<Checkpoint>> {
        self.0.load(thread_id).await
    }
    async fn load_by_id(&self, id: &str) -> adk_rust::graph::Result<Option<Checkpoint>> {
        self.0.load_by_id(id).await
    }
    async fn list(&self, thread_id: &str) -> adk_rust::graph::Result<Vec<Checkpoint>> {
        self.0.list(thread_id).await
    }
    async fn delete(&self, thread_id: &str) -> adk_rust::graph::Result<()> {
        self.0.delete(thread_id).await
    }
}

fn trail(checkpoint: &Checkpoint) -> serde_json::Value {
    checkpoint.state["trail"].clone()
}

#[tokio::test]
#[ignore = "requires disposable PostgreSQL"]
async fn real_pg_a_durable_thread_re_enters_its_pending_node_and_runs_nothing_when_finished() {
    let url = crate::test_pg::database_url_var("ELITEA_TEST_DATABASE_URL")
        .expect("ELITEA_TEST_DATABASE_URL");
    let db = IsolatedPostgres::create(&url).await;
    install_schema(&db.pool).await;
    sqlx::raw_sql(CHECKPOINT_MIGRATION)
        .execute(&db.pool)
        .await
        .expect("apply checkpoint migration");
    let definition = pipeline();

    // Claim 1: lost right after the row that leaves `second` pending.
    let lost = activate(&db.database_name, 1, &definition).await;
    create_session(lost.sessions.as_ref()).await;
    let crashed = tokio::time::timeout(
        std::time::Duration::from_secs(5),
        run(
            &definition,
            Arc::new(LostAfterFirst(lost.checkpointer.clone())),
            lost.sessions.clone(),
            None,
        ),
    )
    .await;
    assert!(crashed.is_err(), "the run is lost after its first save");
    let frontier = history(lost.checkpointer.as_ref()).await;
    let paused = frontier.last().expect("the durable row");
    assert_eq!(paused.pending_nodes, ["second"]);
    assert_eq!(trail(paused), json!(["first"]));
    lost.pool.close().await;

    // Claim 2 (a replacement writer): the same path re-enters at `second`.
    let resumed = activate(&db.database_name, 2, &definition).await;
    run(
        &definition,
        resumed.checkpointer.clone(),
        resumed.sessions.clone(),
        None,
    )
    .await;
    let after = history(resumed.checkpointer.as_ref()).await;
    let finished = after.last().expect("the finished row");
    assert_eq!(finished.pending_nodes, Vec::<String>::new());
    assert_eq!(
        trail(finished),
        json!(["first", "second"]),
        "`first` completed before the loss and never ran again"
    );

    // A finished row through the same path runs no node: the state is
    // unchanged and the only event is the completion marker. The executor
    // still saves one more row, so the durable tool reads a completed row and
    // returns its stored result before it compiles anything (D-4, C3).
    let third = run(
        &definition,
        resumed.checkpointer.clone(),
        resumed.sessions.clone(),
        None,
    )
    .await;
    assert_eq!(third.len(), 1, "only the completion marker");
    assert!(
        third[0]
            .provider_metadata
            .contains_key(crate::agents::graph::PIPELINE_COMPLETED_METADATA_KEY)
    );
    let again = history(resumed.checkpointer.as_ref()).await;
    assert_eq!(again.len(), after.len() + 1, "one more row, no node");
    let latest = again.last().expect("the latest row");
    assert_eq!(latest.pending_nodes, Vec::<String>::new());
    assert_eq!(
        trail(latest),
        json!(["first", "second"]),
        "no node ran again"
    );
    assert_eq!(SESSION, finished.thread_id);
    resumed.pool.close().await;
}

use std::sync::Mutex;

use adk_rust::graph::{MemoryCheckpointer, State};
use serde_json::json;

use super::*;

const ROOT: &str = "root-thread";
const STAMP: &str = "elitea.pipeline.execution.v1";

/// The store contract the authority relies on: append compares the latest
/// row by identity; an existing id replays only with identical bytes.
#[derive(Default)]
struct Store {
    rows: MemoryCheckpointer,
    appended: Mutex<Vec<Checkpoint>>,
}

#[async_trait]
impl Checkpointer for Store {
    async fn save(&self, checkpoint: &Checkpoint) -> Result<String, GraphError> {
        self.appended
            .lock()
            .expect("appended")
            .push(checkpoint.clone());
        self.rows.save(checkpoint).await
    }
    async fn load(&self, thread: &str) -> Result<Option<Checkpoint>, GraphError> {
        self.rows.load(thread).await
    }
    async fn load_by_id(&self, id: &str) -> Result<Option<Checkpoint>, GraphError> {
        self.rows.load_by_id(id).await
    }
    async fn list(&self, thread: &str) -> Result<Vec<Checkpoint>, GraphError> {
        self.rows.list(thread).await
    }
    async fn delete(&self, thread: &str) -> Result<(), GraphError> {
        self.rows.delete(thread).await
    }
}

#[async_trait]
impl ParallelCheckpointAppender for Store {
    async fn append_after(
        &self,
        expected: Option<&Checkpoint>,
        candidate: &Checkpoint,
    ) -> Result<String, GraphError> {
        if let Some(existing) = self.rows.load_by_id(&candidate.checkpoint_id).await? {
            return if serde_json::to_value(&existing).expect("existing")
                == serde_json::to_value(candidate).expect("candidate")
            {
                Ok(existing.checkpoint_id)
            } else {
                Err(GraphError::CheckpointError(
                    "checkpoint.conflict: bytes".into(),
                ))
            };
        }
        let latest = self.rows.load(&candidate.thread_id).await?;
        if latest.map(|row| row.checkpoint_id) != expected.map(|row| row.checkpoint_id.clone()) {
            return Err(GraphError::CheckpointError(
                "checkpoint.conflict: latest".into(),
            ));
        }
        self.save(candidate).await
    }
}

#[async_trait]
impl ParallelChildCheckpointerFactory for Store {
    fn child_origin(&self, _: &ParallelActivation) -> Result<ParallelChildOrigin, GraphError> {
        Ok(ParallelChildOrigin {
            execution_id: "origin".into(),
            generation: 1,
        })
    }
    fn branch_thread_id(
        &self,
        _: &ParallelActivation,
        branch: &ParallelBranchDefinition,
        _: usize,
        _: &[u8; 32],
        _: &ParallelChildOrigin,
    ) -> Result<String, GraphError> {
        Ok(format!("{ROOT}/{}", branch.id()))
    }
    async fn for_branch(
        &self,
        _: &ParallelActivation,
        _: &ParallelBranchDefinition,
        _: usize,
        _: &[u8; 32],
        _: &ParallelChildOrigin,
    ) -> Result<ParallelChildCheckpoint, GraphError> {
        Err(GraphError::CheckpointError("test does not mint".into()))
    }
}

impl ParallelCheckpointAuthority for Store {}

fn turn(store: &Arc<Store>, execution: &str, resume: bool) -> Arc<FanoutTurnAuthority> {
    let inner: Arc<dyn ParallelCheckpointAuthority> = store.clone();
    Arc::new(FanoutTurnAuthority::new(inner, ROOT, execution, 1, resume).expect("turn authority"))
}

fn row(thread: &str, step: usize, pending: &[&str]) -> Checkpoint {
    Checkpoint::new(
        thread,
        State::from([("topic".to_owned(), json!("release"))]),
        step,
        pending.iter().map(|node| (*node).to_owned()).collect(),
    )
}

fn stamp(checkpoint: &Checkpoint) -> Option<&Value> {
    checkpoint.metadata.get(STAMP)
}

#[test]
fn an_invalid_turn_identity_is_refused() {
    let store: Arc<dyn ParallelCheckpointAuthority> = Arc::new(Store::default());
    for (root, execution, generation) in [
        ("", "execution-1", 1),
        (ROOT, "", 1),
        (ROOT, "execution-1", 0),
        ("root\nthread", "execution-1", 1),
        (ROOT, "execution\u{0}1", 1),
    ] {
        assert!(
            FanoutTurnAuthority::new(Arc::clone(&store), root, execution, generation, false)
                .is_err()
        );
    }
}

#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn every_new_row_of_the_turn_is_stamped_and_replays_are_not() {
    let store = Arc::new(Store::default());
    let authority = turn(&store, "execution-1", false);
    let first = row(ROOT, 0, &["gather"]);
    authority
        .append_after(None, &first)
        .await
        .expect("first row");
    let head = authority
        .probe_parent(ROOT, "next")
        .await
        .expect("probe")
        .latest
        .expect("own row is visible");
    // The occurrence freeze row (`append_after`) and the join row (`append_after_head`).
    let freeze = row(ROOT, 0, &["gather"]);
    authority
        .append_after(Some(&store.load(ROOT).await.unwrap().unwrap()), &freeze)
        .await
        .expect("freeze row");
    let join = row(ROOT, 1, &[]);
    let head_after_freeze = authority
        .probe_parent(ROOT, &join.checkpoint_id)
        .await
        .expect("probe")
        .latest
        .expect("freeze row visible");
    assert_ne!(head.checkpoint_id, head_after_freeze.checkpoint_id);
    authority
        .append_after_head(Some(&head_after_freeze), &join)
        .await
        .expect("join row");
    let child = row(&format!("{ROOT}/left"), 0, &["workerA"]);
    authority.save(&child).await.expect("child row");
    // An exact replay of a stored row keeps its bytes.
    let stored = store
        .load_by_id(&first.checkpoint_id)
        .await
        .unwrap()
        .unwrap();
    authority
        .append_after(None, &stored)
        .await
        .expect("exact replay");

    let appended = store.appended.lock().expect("appended").clone();
    assert_eq!(appended.len(), 4, "the replay appends nothing");
    for root_row in appended.iter().filter(|row| row.thread_id == ROOT) {
        assert_eq!(stamp(root_row), Some(&json!(["execution-1", 1])));
    }
    // A saved child pipeline's thread beneath the root is part of the turn, as
    // `TurnCheckpointer` scopes it (FU-249): stamped, and hidden from a fresh
    // turn of another execution while a continuation still reads it.
    assert_eq!(
        stamp(&appended[3]),
        Some(&json!(["execution-1", 1])),
        "a child pipeline row carries the turn"
    );
    let child_thread = format!("{ROOT}/left");
    assert!(
        turn(&store, "execution-2", false)
            .load(&child_thread)
            .await
            .unwrap()
            .is_none()
    );
    assert!(
        turn(&store, "execution-2", true)
            .load(&child_thread)
            .await
            .unwrap()
            .is_some()
    );
    // A thread that only shares the root's prefix is not beneath it.
    assert_eq!(
        authority
            .save(&row(&format!("{ROOT}x"), 0, &[]))
            .await
            .map(|_| ())
            .ok(),
        Some(())
    );
    let sibling = store
        .appended
        .lock()
        .expect("appended")
        .last()
        .cloned()
        .expect("sibling row");
    assert_eq!(stamp(&sibling), None, "a foreign thread is never stamped");
}

#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn a_fresh_turn_hides_the_earlier_frontier_and_appends_after_it() {
    let store = Arc::new(Store::default());
    let first = turn(&store, "execution-1", false);
    let parked = row(ROOT, 2, &["gather"]);
    first.append_after(None, &parked).await.expect("parked row");

    let fresh = turn(&store, "execution-2", false);
    assert!(fresh.load(ROOT).await.unwrap().is_none());
    let probe = fresh.probe_parent(ROOT, "candidate").await.unwrap();
    assert!(
        probe.latest.is_none(),
        "the earlier execution is not this turn's frontier"
    );
    let start = row(ROOT, 0, &["pre"]);
    fresh
        .append_after_head(None, &start)
        .await
        .expect("the first row appends after the retained physical row");
    let latest = fresh.load(ROOT).await.unwrap().expect("own row");
    assert_eq!(latest.checkpoint_id, start.checkpoint_id);
    assert_eq!(stamp(&latest), Some(&json!(["execution-2", 1])));

    // The caller believed it had no row yet, but this execution already wrote one.
    let stale = row(ROOT, 1, &["post"]);
    assert!(fresh.append_after_head(None, &stale).await.is_err());
    assert!(fresh.append_after(None, &stale).await.is_err());
}

#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn a_continuation_reads_the_frontier_another_execution_parked() {
    let store = Arc::new(Store::default());
    let parked = row(ROOT, 2, &["gather"]);
    turn(&store, "execution-1", false)
        .append_after(None, &parked)
        .await
        .expect("parked row");
    let wake = turn(&store, "execution-2", true);
    let latest = wake.load(ROOT).await.unwrap().expect("parked frontier");
    assert_eq!(latest.checkpoint_id, parked.checkpoint_id);
    let head = wake
        .probe_parent(ROOT, "candidate")
        .await
        .unwrap()
        .latest
        .expect("parked head");
    let next = row(ROOT, 2, &["gather"]);
    wake.append_after_head(Some(&head), &next)
        .await
        .expect("re-entered row");
    assert_eq!(
        stamp(&wake.load(ROOT).await.unwrap().unwrap()),
        Some(&json!(["execution-2", 1]))
    );
}

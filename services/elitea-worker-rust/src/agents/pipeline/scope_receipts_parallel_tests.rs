use std::collections::BTreeMap;
use std::sync::atomic::{AtomicUsize, Ordering};

use adk_rust::graph::{ExecutionConfig, NodeContext, State};
use serde_json::json;

use super::super::{GRAPH_CALL_REVISION_METADATA_KEY, prepare_graph_call, receipt_revision};
use super::*;
use crate::agents::events::APPLICATION_BRANCH_ROOT;
use crate::agents::graph::{ParallelNodeDefinition, compiler::PipelineDefinition};
use crate::agents::pipeline::scoped_applications::{
    PipelineApplicationScope, PipelineApplicationScopeRegistry,
};
use crate::agents::session::ApplicationRuntimeProjection;

#[derive(Default)]
struct Stored {
    latest: Option<Checkpoint>,
    by_id: BTreeMap<String, Checkpoint>,
    appends: usize,
}

#[derive(Default)]
struct AtomicMemory {
    stored: Mutex<Stored>,
    child_calls: AtomicUsize,
}

fn same(left: &Checkpoint, right: &Checkpoint) -> bool {
    serde_json::to_value(left).unwrap() == serde_json::to_value(right).unwrap()
}

fn insert(stored: &mut Stored, candidate: &Checkpoint) -> Result<String, GraphError> {
    if let Some(original) = stored.by_id.get(&candidate.checkpoint_id) {
        return if same(original, candidate) {
            Ok(candidate.checkpoint_id.clone())
        } else {
            Err(receipt_error())
        };
    }
    stored
        .by_id
        .insert(candidate.checkpoint_id.clone(), candidate.clone());
    stored.latest = Some(candidate.clone());
    Ok(candidate.checkpoint_id.clone())
}

#[async_trait]
impl Checkpointer for AtomicMemory {
    async fn save(&self, candidate: &Checkpoint) -> Result<String, GraphError> {
        let mut stored = self.stored.lock().await;
        insert(&mut stored, candidate)
    }

    async fn load(&self, thread: &str) -> Result<Option<Checkpoint>, GraphError> {
        Ok(self
            .stored
            .lock()
            .await
            .latest
            .clone()
            .filter(|c| c.thread_id == thread))
    }

    async fn load_by_id(&self, id: &str) -> Result<Option<Checkpoint>, GraphError> {
        Ok(self.stored.lock().await.by_id.get(id).cloned())
    }

    async fn list(&self, thread: &str) -> Result<Vec<Checkpoint>, GraphError> {
        Ok(self
            .stored
            .lock()
            .await
            .by_id
            .values()
            .filter(|c| c.thread_id == thread)
            .cloned()
            .collect())
    }

    async fn delete(&self, thread: &str) -> Result<(), GraphError> {
        let mut stored = self.stored.lock().await;
        stored.by_id.retain(|_, c| c.thread_id != thread);
        if stored
            .latest
            .as_ref()
            .is_some_and(|c| c.thread_id == thread)
        {
            stored.latest = None;
        }
        Ok(())
    }

    async fn prune(&self, _: &str, _: &RetentionPolicy) -> Result<usize, GraphError> {
        Ok(0)
    }
}

#[async_trait]
impl ParallelCheckpointAppender for AtomicMemory {
    async fn append_after(
        &self,
        expected: Option<&Checkpoint>,
        candidate: &Checkpoint,
    ) -> Result<String, GraphError> {
        let mut stored = self.stored.lock().await;
        stored.appends += 1;
        if stored.by_id.contains_key(&candidate.checkpoint_id) {
            return insert(&mut stored, candidate);
        }
        match (stored.latest.as_ref(), expected) {
            (None, None) => {}
            (Some(actual), Some(expected)) if same(actual, expected) => {}
            _ => return Err(receipt_error()),
        }
        insert(&mut stored, candidate)
    }
}

#[async_trait]
impl ParallelChildCheckpointerFactory for AtomicMemory {
    fn child_origin(
        &self,
        _: &ParallelActivation,
    ) -> Result<crate::agents::graph::ParallelChildOrigin, GraphError> {
        Ok(test_origin())
    }

    fn branch_thread_id(
        &self,
        _: &ParallelActivation,
        _: &ParallelBranchDefinition,
        _: usize,
        _: &[u8; 32],
        _: &crate::agents::graph::ParallelChildOrigin,
    ) -> Result<String, GraphError> {
        Err(receipt_error())
    }

    async fn for_branch(
        &self,
        _: &ParallelActivation,
        _: &ParallelBranchDefinition,
        _: usize,
        _: &[u8; 32],
        _: &crate::agents::graph::ParallelChildOrigin,
    ) -> Result<ParallelChildCheckpoint, GraphError> {
        self.child_calls.fetch_add(1, Ordering::SeqCst);
        Err(receipt_error())
    }
}

impl ParallelCheckpointAuthority for AtomicMemory {}

fn test_origin() -> crate::agents::graph::ParallelChildOrigin {
    crate::agents::graph::ParallelChildOrigin {
        execution_id: "test-execution".to_owned(),
        generation: 1,
    }
}

fn definition() -> PipelineDefinition {
    PipelineDefinition::from_yaml(
        "state: {answer: str}\nentry_point: child\nnodes:\n  - id: child\n    type: agent\n    tool: assistant\n    input_mapping: {task: {type: fixed, value: work}}\n    output: [answer]\n    transition: END\n",
    ).unwrap()
}

fn overlay(inner: Arc<AtomicMemory>) -> Arc<dyn ParallelCheckpointAuthority> {
    let mut registry = PipelineApplicationScopeRegistry::default();
    registry
        .insert(
            String::new(),
            &definition(),
            None,
            ApplicationRuntimeProjection::default(),
        )
        .unwrap();
    let authority: Arc<dyn ParallelCheckpointAuthority> = inner;
    assert!(
        registry
            .wrap_parallel_authority("unknown", authority.clone())
            .is_err()
    );
    registry.wrap_parallel_authority("", authority).unwrap()
}

async fn original_receipt(
    inner: &Arc<AtomicMemory>,
    wrapped: &Arc<dyn ParallelCheckpointAuthority>,
) -> (Checkpoint, Checkpoint) {
    let root = Checkpoint::new("owned-root", State::new(), 0, vec!["child".to_owned()]);
    inner.save(&root).await.unwrap();
    let scope = PipelineApplicationScope::new(String::new(), &definition(), None).unwrap();
    prepare_graph_call(
        wrapped.as_ref(),
        &Mutex::new(()),
        &NodeContext::new(State::new(), ExecutionConfig::new("owned-root"), 0),
        scope.activate("owned-root", "child", 0).unwrap(),
        "assistant",
        &json!({"task":"work"}),
        "original-runtime",
        "graph",
        APPLICATION_BRANCH_ROOT,
    )
    .await
    .unwrap();
    (root, inner.load("owned-root").await.unwrap().unwrap())
}

fn parallel_revision(parent: &Checkpoint, phase: &str) -> Checkpoint {
    let mut candidate = Checkpoint::new(
        &parent.thread_id,
        parent.state.clone(),
        parent.step,
        parent.pending_nodes.clone(),
    );
    candidate.metadata.insert(
        "fixture.parallel.occurrence".to_owned(),
        json!({"phase":phase}),
    );
    candidate
}

#[tokio::test]
async fn freeze_and_decision_appends_retain_original_receipts_and_immutable_replays() {
    let inner = Arc::new(AtomicMemory::default());
    let wrapped = overlay(inner.clone());
    let (root, parent) = original_receipt(&inner, &wrapped).await;
    let original = parent.metadata[GRAPH_CALL_RECEIPTS_METADATA_KEY].clone();
    let freeze = parallel_revision(&parent, "frozen");
    wrapped.append_after(Some(&parent), &freeze).await.unwrap();
    let frozen = wrapped.load("owned-root").await.unwrap().unwrap();
    assert_eq!(frozen.metadata[GRAPH_CALL_RECEIPTS_METADATA_KEY], original);
    assert!(
        !frozen
            .metadata
            .contains_key(GRAPH_CALL_REVISION_METADATA_KEY)
    );
    // The original pre-decoration bytes cannot replace the stored immutable ID.
    assert!(wrapped.append_after(Some(&frozen), &freeze).await.is_err());
    let mut decision = parallel_revision(&frozen, "decided");
    decision
        .state
        .insert("fixture.decisions".to_owned(), json!(true));
    wrapped
        .append_after(Some(&frozen), &decision)
        .await
        .unwrap();
    let decided = wrapped.load("owned-root").await.unwrap().unwrap();
    assert_eq!(decided.metadata[GRAPH_CALL_RECEIPTS_METADATA_KEY], original);
    wrapped.append_after(Some(&decided), &frozen).await.unwrap();
    wrapped.append_after(None, &root).await.unwrap();
    assert!(same(
        &wrapped.load("owned-root").await.unwrap().unwrap(),
        &decided
    ));
    assert!(same(
        &wrapped
            .load_by_id(&root.checkpoint_id)
            .await
            .unwrap()
            .unwrap(),
        &root
    ));
}

#[tokio::test]
async fn atomic_append_refuses_receipt_invention_replacement_and_unproved_parent() {
    let inner = Arc::new(AtomicMemory::default());
    let wrapped = overlay(inner.clone());
    let (_, parent) = original_receipt(&inner, &wrapped).await;
    let mut receipts = read_receipts(&parent).unwrap();
    receipts.calls.get_mut("child").unwrap().original_start.id = "forged-original".to_owned();
    let forged = serde_json::to_value(&receipts).unwrap();
    let mut candidate = parallel_revision(&parent, "frozen");
    candidate
        .metadata
        .insert(GRAPH_CALL_RECEIPTS_METADATA_KEY.to_owned(), forged.clone());
    // This is a valid receipt codec value. It still cannot replace the original.
    assert!(read_receipts(&candidate).is_ok());
    assert!(
        wrapped
            .append_after(Some(&parent), &candidate)
            .await
            .is_err()
    );
    receipts.calls.clear();
    candidate.metadata.insert(
        GRAPH_CALL_RECEIPTS_METADATA_KEY.to_owned(),
        serde_json::to_value(&receipts).unwrap(),
    );
    assert!(
        wrapped
            .append_after(Some(&parent), &candidate)
            .await
            .is_err()
    );
    candidate.metadata.insert(
        GRAPH_CALL_RECEIPTS_METADATA_KEY.to_owned(),
        json!({"schema":"invalid"}),
    );
    assert!(
        wrapped
            .append_after(Some(&parent), &candidate)
            .await
            .is_err()
    );
    candidate.metadata.insert(
        GRAPH_CALL_RECEIPTS_METADATA_KEY.to_owned(),
        parent.metadata[GRAPH_CALL_RECEIPTS_METADATA_KEY].clone(),
    );
    assert!(wrapped.append_after(None, &candidate).await.is_err());
    assert_eq!(inner.stored.lock().await.appends, 0);
    let mut false_parent = parent.clone();
    false_parent
        .metadata
        .insert(GRAPH_CALL_RECEIPTS_METADATA_KEY.to_owned(), forged);
    let candidate = parallel_revision(&false_parent, "frozen");
    assert!(
        wrapped
            .append_after(Some(&false_parent), &candidate)
            .await
            .is_err()
    );
    assert_eq!(inner.stored.lock().await.appends, 1);
    assert!(same(
        &wrapped.load("owned-root").await.unwrap().unwrap(),
        &parent
    ));
    assert!(
        wrapped
            .load_by_id(&candidate.checkpoint_id)
            .await
            .unwrap()
            .is_none()
    );
}

#[tokio::test]
async fn real_graph_call_revision_requires_exact_frontier_on_atomic_path() {
    let source = Arc::new(AtomicMemory::default());
    let source_overlay = overlay(source.clone());
    let (root, recorded) = original_receipt(&source, &source_overlay).await;
    let raw = recorded.metadata[GRAPH_CALL_RECEIPTS_METADATA_KEY].clone();
    let candidate = receipt_revision(&root, raw).unwrap();
    let inner = Arc::new(AtomicMemory::default());
    let wrapped = overlay(inner.clone());
    inner.save(&root).await.unwrap();
    let mut changed = candidate.clone();
    changed.state.insert("forged".to_owned(), json!(1));
    assert!(wrapped.append_after(Some(&root), &changed).await.is_err());
    let mut stale = root.clone();
    stale.checkpoint_id = "stale".to_owned();
    assert!(
        wrapped
            .append_after(Some(&stale), &candidate)
            .await
            .is_err()
    );
    assert_eq!(inner.stored.lock().await.appends, 0);
    wrapped.append_after(Some(&root), &candidate).await.unwrap();
    assert!(same(
        &wrapped.load("owned-root").await.unwrap().unwrap(),
        &candidate
    ));
    let mut stale_marker = parallel_revision(&candidate, "decided");
    stale_marker.metadata.insert(
        GRAPH_CALL_REVISION_METADATA_KEY.to_owned(),
        candidate.metadata[GRAPH_CALL_REVISION_METADATA_KEY].clone(),
    );
    assert!(
        wrapped
            .append_after(Some(&candidate), &stale_marker)
            .await
            .is_err()
    );
}

#[tokio::test]
async fn ordinary_save_and_child_factory_keep_the_same_underlying_authority() {
    let inner = Arc::new(AtomicMemory::default());
    let wrapped = overlay(inner.clone());
    let (_, parent) = original_receipt(&inner, &wrapped).await;
    let frontier = Checkpoint::new("owned-root", State::new(), 1, vec!["next".to_owned()]);
    wrapped.save(&frontier).await.unwrap();
    let latest = wrapped.load("owned-root").await.unwrap().unwrap();
    assert_eq!(
        latest.metadata[GRAPH_CALL_RECEIPTS_METADATA_KEY],
        parent.metadata[GRAPH_CALL_RECEIPTS_METADATA_KEY]
    );
    let definition = ParallelNodeDefinition::from_yaml(
        "id: fanout\ntype: parallel\nbranches: [{id: one, node: child}, {id: two, node: sibling}]\nmax_concurrency: 1\nwait: all\noutput: [results]\ntransition: END\n",
    ).unwrap();
    let activation = ParallelActivation {
        root_thread_id: "owned-root".to_owned(),
        node_id: definition.id().to_owned(),
        step: 1,
        config_digest: definition.config_digest(),
    };
    assert!(
        wrapped
            .for_branch(
                &activation,
                &definition.branches()[0],
                0,
                &[9; 32],
                &test_origin(),
            )
            .await
            .is_err()
    );
    assert_eq!(inner.child_calls.load(Ordering::SeqCst), 1);
    assert_eq!(inner.stored.lock().await.appends, 0);
    assert_eq!(wrapped.list("owned-root").await.unwrap().len(), 3);
    wrapped.delete("owned-root").await.unwrap();
    assert!(wrapped.load("owned-root").await.unwrap().is_none());
}

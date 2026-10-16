use std::collections::{BTreeMap, BTreeSet, HashMap};
use std::fmt::Write as _;
use std::future::Future;
use std::pin::Pin;
use std::sync::Arc;
use std::sync::atomic::{AtomicUsize, Ordering};
use std::time::Duration;

use adk_rust::graph::checkpoint::{Checkpointer, MemoryCheckpointer};
use adk_rust::graph::{
    Checkpoint, CompiledGraph, ExecutionConfig, FunctionNode, GraphError, Node, NodeContext,
    NodeOutput, START, State, StateGraph,
};
use async_trait::async_trait;
use serde_json::{Value, json};
use tokio::sync::{Mutex, Notify, Semaphore, mpsc};

use elitea_agent_runtime::graph::fanout::runner::AckResult;
use elitea_agent_runtime::graph::fanout_budget::{
    CANCEL_CLEANUP, MAX_CHILD_RESULT_BYTES, MAX_JOINED_RESULT_BYTES, MAX_OPEN_INTERRUPTS,
};

use super::fanout_control::FanoutCancellation;
use super::fanout_test_ledger::{LedgerSeam, TestLedger};
use super::parallel::{
    AdkParallelBranchRuntime, DurableParallelNode, FANOUT_PARKED_SCHEMA, ParallelActivation,
    ParallelBranchExecution, ParallelBranchGraphFactory, ParallelBranchPause,
    ParallelBranchTerminal, ParallelCheckpointAppender, ParallelChildCheckpoint,
    ParallelChildCheckpointerFactory, ParallelChildOrigin, ParallelDecision,
    ParallelOccurrenceCheckpointer, ParallelPauseCard, projected_input_digest,
};
use super::{ParallelBranchDefinition, ParallelNodeDefinition};

const OCCURRENCE: &str = "elitea.graph.parallel.occurrence.v3";

type BehaviorFuture = Pin<Box<dyn Future<Output = Result<Value, GraphError>> + Send>>;
type Behavior = Arc<dyn Fn() -> BehaviorFuture + Send + Sync>;

#[derive(Default)]
struct AtomicParentRows {
    checkpoints: Vec<Checkpoint>,
    before_append: Option<Checkpoint>,
}

#[derive(Default)]
struct AtomicParentCheckpoints {
    rows: Mutex<AtomicParentRows>,
}

fn checkpoint_equal(left: &Checkpoint, right: &Checkpoint) -> Result<bool, GraphError> {
    Ok(serde_json::to_value(left)? == serde_json::to_value(right)?)
}

fn insert_immutable(
    rows: &mut AtomicParentRows,
    checkpoint: &Checkpoint,
) -> Result<String, GraphError> {
    if let Some(existing) = rows
        .checkpoints
        .iter()
        .find(|row| row.checkpoint_id == checkpoint.checkpoint_id)
    {
        return if checkpoint_equal(existing, checkpoint)? {
            Ok(existing.checkpoint_id.clone())
        } else {
            Err(GraphError::CheckpointError(
                "immutable checkpoint conflict".to_owned(),
            ))
        };
    }
    rows.checkpoints.push(checkpoint.clone());
    Ok(checkpoint.checkpoint_id.clone())
}

#[async_trait]
impl Checkpointer for AtomicParentCheckpoints {
    async fn save(&self, checkpoint: &Checkpoint) -> Result<String, GraphError> {
        let mut rows = self.rows.lock().await;
        insert_immutable(&mut rows, checkpoint)
    }

    async fn load(&self, thread: &str) -> Result<Option<Checkpoint>, GraphError> {
        Ok(self
            .rows
            .lock()
            .await
            .checkpoints
            .iter()
            .rev()
            .find(|row| row.thread_id == thread)
            .cloned())
    }

    async fn load_by_id(&self, id: &str) -> Result<Option<Checkpoint>, GraphError> {
        Ok(self
            .rows
            .lock()
            .await
            .checkpoints
            .iter()
            .find(|row| row.checkpoint_id == id)
            .cloned())
    }

    async fn list(&self, thread: &str) -> Result<Vec<Checkpoint>, GraphError> {
        Ok(self
            .rows
            .lock()
            .await
            .checkpoints
            .iter()
            .filter(|row| row.thread_id == thread)
            .cloned()
            .collect())
    }

    async fn delete(&self, thread: &str) -> Result<(), GraphError> {
        self.rows
            .lock()
            .await
            .checkpoints
            .retain(|row| row.thread_id != thread);
        Ok(())
    }
}

#[async_trait]
impl ParallelCheckpointAppender for AtomicParentCheckpoints {
    async fn append_after(
        &self,
        expected: Option<&Checkpoint>,
        candidate: &Checkpoint,
    ) -> Result<String, GraphError> {
        let mut rows = self.rows.lock().await;
        // This models a competing committed writer after the wrapper's load and
        // before its atomic comparison. Ordinary writer fencing is insufficient.
        if let Some(competing) = rows.before_append.take() {
            insert_immutable(&mut rows, &competing)?;
        }
        if rows
            .checkpoints
            .iter()
            .any(|row| row.checkpoint_id == candidate.checkpoint_id)
        {
            return insert_immutable(&mut rows, candidate);
        }
        let latest = rows
            .checkpoints
            .iter()
            .rev()
            .find(|row| row.thread_id == candidate.thread_id);
        let matches = match (expected, latest) {
            (Some(expected), Some(latest)) => checkpoint_equal(expected, latest)?,
            (None, None) => true,
            _ => false,
        };
        if !matches {
            return Err(GraphError::CheckpointError(
                "atomic expected-parent conflict".to_owned(),
            ));
        }
        insert_immutable(&mut rows, candidate)
    }
}

#[derive(Default)]
struct MemoryChildCheckpoints {
    stores: Mutex<HashMap<String, Arc<MemoryCheckpointer>>>,
    issued_threads: Mutex<Vec<String>>,
    parent: Arc<AtomicParentCheckpoints>,
    /// Branch ID whose child writer reports `checkpoint.writer_not_current` on save.
    lease_lost_branch: Option<&'static str>,
    /// Loads issued against every child store.
    child_loads: Arc<AtomicUsize>,
}

/// Counts reads of a child store; writes and other calls pass through.
struct CountingLoads {
    store: Arc<MemoryCheckpointer>,
    loads: Arc<AtomicUsize>,
}

#[async_trait]
impl Checkpointer for CountingLoads {
    async fn save(&self, checkpoint: &Checkpoint) -> Result<String, GraphError> {
        self.store.save(checkpoint).await
    }
    async fn load(&self, thread: &str) -> Result<Option<Checkpoint>, GraphError> {
        self.loads.fetch_add(1, Ordering::SeqCst);
        self.store.load(thread).await
    }
    async fn load_by_id(&self, id: &str) -> Result<Option<Checkpoint>, GraphError> {
        self.loads.fetch_add(1, Ordering::SeqCst);
        self.store.load_by_id(id).await
    }
    async fn list(&self, thread: &str) -> Result<Vec<Checkpoint>, GraphError> {
        self.store.list(thread).await
    }
    async fn delete(&self, thread: &str) -> Result<(), GraphError> {
        self.store.delete(thread).await
    }
}

const LEASE_LOST: &str =
    "checkpoint.writer_not_current: the PostgreSQL checkpoint writer is no longer current";

/// A child store whose writer fence has been lost: reads work, every save is refused.
struct LeaseLostStore(Arc<MemoryCheckpointer>);

#[async_trait]
impl Checkpointer for LeaseLostStore {
    async fn save(&self, _checkpoint: &Checkpoint) -> Result<String, GraphError> {
        Err(GraphError::CheckpointError(LEASE_LOST.to_owned()))
    }
    async fn load(&self, thread: &str) -> Result<Option<Checkpoint>, GraphError> {
        self.0.load(thread).await
    }
    async fn load_by_id(&self, id: &str) -> Result<Option<Checkpoint>, GraphError> {
        self.0.load_by_id(id).await
    }
    async fn list(&self, thread: &str) -> Result<Vec<Checkpoint>, GraphError> {
        self.0.list(thread).await
    }
    async fn delete(&self, thread: &str) -> Result<(), GraphError> {
        self.0.delete(thread).await
    }
}

#[async_trait]
impl ParallelChildCheckpointerFactory for MemoryChildCheckpoints {
    fn child_origin(&self, _: &ParallelActivation) -> Result<ParallelChildOrigin, GraphError> {
        Ok(ParallelChildOrigin {
            execution_id: "test-execution".to_owned(),
            generation: 1,
        })
    }

    fn branch_thread_id(
        &self,
        activation: &ParallelActivation,
        branch: &ParallelBranchDefinition,
        ordinal: usize,
        input_digest: &[u8; 32],
        origin: &ParallelChildOrigin,
    ) -> Result<String, GraphError> {
        Ok(format!(
            "test:{}:{}:{}:{}:{}:{}:{ordinal}:{input_digest:?}",
            activation.root_thread_id,
            activation.node_id,
            activation.step,
            origin.execution_id,
            origin.generation,
            branch.id(),
        ))
    }

    async fn for_branch(
        &self,
        activation: &ParallelActivation,
        branch: &ParallelBranchDefinition,
        ordinal: usize,
        input_digest: &[u8; 32],
        origin: &ParallelChildOrigin,
    ) -> Result<ParallelChildCheckpoint, GraphError> {
        let thread_id = self.branch_thread_id(activation, branch, ordinal, input_digest, origin)?;
        let mut stores = self.stores.lock().await;
        let store = Arc::clone(
            stores
                .entry(thread_id.clone())
                .or_insert_with(|| Arc::new(MemoryCheckpointer::new())),
        );
        self.issued_threads.lock().await.push(thread_id.clone());
        let checkpointer: Arc<dyn Checkpointer> = if self.lease_lost_branch == Some(branch.id()) {
            Arc::new(LeaseLostStore(store))
        } else {
            Arc::new(CountingLoads {
                store,
                loads: Arc::clone(&self.child_loads),
            })
        };
        Ok(ParallelChildCheckpoint {
            admitted_threads: std::collections::BTreeSet::from([thread_id.clone()]),
            thread_id,
            checkpointer,
        })
    }
}

struct TestBranchGraphs {
    behaviors: HashMap<String, Behavior>,
    rejected_target: Option<String>,
}

#[async_trait]
impl ParallelBranchGraphFactory for TestBranchGraphs {
    fn owned_definition_digest(
        &self,
        branch: &ParallelBranchDefinition,
    ) -> Result<[u8; 32], GraphError> {
        let mut result = [0; 32];
        result.copy_from_slice(
            ring::digest::digest(&ring::digest::SHA256, branch.node().as_bytes()).as_ref(),
        );
        Ok(result)
    }
    fn validate_branch(&self, branch: &ParallelBranchDefinition) -> Result<(), GraphError> {
        if self.rejected_target.as_deref() == Some(branch.node()) {
            return Err(GraphError::InvalidGraph(
                "the branch definition is not an admitted Agent".to_owned(),
            ));
        }
        if !self.behaviors.contains_key(branch.node()) {
            return Err(GraphError::NodeNotFound(branch.node().to_owned()));
        }
        Ok(())
    }

    fn compile_branch(
        &self,
        branch: &ParallelBranchDefinition,
        execution: ParallelBranchExecution,
    ) -> Result<CompiledGraph, GraphError> {
        let behavior = Arc::clone(
            self.behaviors
                .get(branch.node())
                .ok_or_else(|| GraphError::NodeNotFound(branch.node().to_owned()))?,
        );
        StateGraph::with_channels(&["branch_input", "branch_result"])
            .add_node(execution.wrap_node(FunctionNode::new("run", move |_| {
                let behavior = Arc::clone(&behavior);
                async move {
                    let value = behavior().await?;
                    Ok(NodeOutput::new().with_update("branch_result", value))
                }
            })))
            .add_edge(START, "run")
            .add_edge("run", adk_rust::graph::END)
            .compile()
            .map(|graph| {
                graph
                    .with_checkpointer_arc(execution.checkpointer())
                    .with_strict_channels()
            })
    }

    fn project_input(
        &self,
        _branch: &ParallelBranchDefinition,
        parent: &State,
    ) -> Result<State, GraphError> {
        Ok(HashMap::from([(
            "branch_input".to_owned(),
            parent.get("input").cloned().unwrap_or(Value::Null),
        )]))
    }

    fn project_result(
        &self,
        _branch: &ParallelBranchDefinition,
        child: &State,
    ) -> Result<ParallelBranchTerminal, GraphError> {
        child
            .get("branch_result")
            .and_then(Value::as_object)
            .cloned()
            .map(ParallelBranchTerminal::Completed)
            .ok_or_else(|| {
                GraphError::SerializationError("branch result projection is missing".to_owned())
            })
    }

    fn pause_cards(
        &self,
        _branch: &ParallelBranchDefinition,
        _pause: &ParallelBranchPause,
    ) -> Result<Vec<ParallelPauseCard>, GraphError> {
        Err(GraphError::InvalidGraph(
            "this fixture does not produce pauses".to_owned(),
        ))
    }

    async fn resume_input(
        &self,
        _branch: &ParallelBranchDefinition,
        _pause: &ParallelBranchPause,
        _decisions: &[ParallelDecision],
    ) -> Result<State, GraphError> {
        Err(GraphError::InvalidGraph(
            "this fixture does not resume pauses".to_owned(),
        ))
    }
}

/// The legacy `error_policy: fail_after_drain`, i.e. `on_error: stop`.
fn definition(branches: &[(&str, &str)], max_concurrency: usize) -> ParallelNodeDefinition {
    definition_with(
        branches,
        max_concurrency,
        "error_policy: fail_after_drain\n",
    )
}

/// `policy` holds the On Error and join lines (empty: the defaults).
fn definition_with(
    branches: &[(&str, &str)],
    max_concurrency: usize,
    policy: &str,
) -> ParallelNodeDefinition {
    let branches = branches.iter().fold(String::new(), |mut yaml, (id, node)| {
        write!(yaml, "  - id: {id}\n    node: {node}\n").expect("write YAML fixture");
        yaml
    });
    ParallelNodeDefinition::from_yaml(&format!(
        "id: gather\ntype: parallel\nbranches:\n{branches}max_concurrency: {max_concurrency}\nwait: all\n{policy}output: [gathered]\ntransition: END\n"
    ))
    .expect("valid parallel definition")
}

fn runtime(
    checkpoints: Arc<MemoryChildCheckpoints>,
    behaviors: HashMap<String, Behavior>,
) -> Arc<AdkParallelBranchRuntime> {
    let parent: Arc<dyn ParallelCheckpointAppender> = checkpoints.parent.clone();
    let checkpoint_factory: Arc<dyn ParallelChildCheckpointerFactory> = checkpoints;
    let graph_factory: Arc<dyn ParallelBranchGraphFactory> = Arc::new(TestBranchGraphs {
        behaviors,
        rejected_target: None,
    });
    Arc::new(AdkParallelBranchRuntime::new(
        checkpoint_factory,
        graph_factory,
        Arc::new(ParallelOccurrenceCheckpointer::new(parent)),
    ))
}

fn parent_graph(
    definition: ParallelNodeDefinition,
    runtime: Arc<AdkParallelBranchRuntime>,
) -> CompiledGraph {
    let parent = runtime.parent_checkpointer();
    StateGraph::with_channels(&["input", "gathered"])
        .add_node(DurableParallelNode::new(definition, runtime))
        .add_edge(START, "gather")
        .add_edge("gather", adk_rust::graph::END)
        .compile()
        .expect("compile parent graph")
        .with_checkpointer_arc(parent)
        .with_strict_channels()
}

fn constant(value: Value) -> Behavior {
    Arc::new(move || {
        let value = value.clone();
        Box::pin(async move { Ok(value) })
    })
}

#[test]
fn projected_input_digest_is_canonical_and_type_exact() {
    let mut nested_forward = serde_json::Map::new();
    nested_forward.insert("a".to_owned(), json!(1));
    nested_forward.insert("b".to_owned(), json!(2));
    let mut nested_reverse = serde_json::Map::new();
    nested_reverse.insert("b".to_owned(), json!(2));
    nested_reverse.insert("a".to_owned(), json!(1));

    let forward = HashMap::from([
        ("nested".to_owned(), Value::Object(nested_forward)),
        ("number".to_owned(), json!(1.0)),
    ]);
    let reverse = HashMap::from([
        ("number".to_owned(), json!(1.0)),
        ("nested".to_owned(), Value::Object(nested_reverse)),
    ]);
    let integer = HashMap::from([
        ("nested".to_owned(), json!({"a": 1, "b": 2})),
        ("number".to_owned(), json!(1)),
    ]);

    let digest = projected_input_digest(&forward).expect("digest canonical projected input");
    assert_eq!(
        digest,
        projected_input_digest(&reverse).expect("digest reordered projected input")
    );
    assert_ne!(
        digest,
        projected_input_digest(&integer).expect("digest integer projected input")
    );
    assert_eq!(
        digest,
        [
            56, 133, 28, 29, 234, 49, 17, 147, 54, 253, 80, 146, 101, 78, 38, 150, 197, 56, 159,
            224, 121, 75, 124, 245, 100, 59, 191, 160, 153, 46, 75, 143,
        ]
    );
}

#[test]
fn parallel_yaml_contract_is_strict_and_ui_shaped() {
    let definition = definition(&[("source_a", "fetch_a"), ("source_b", "fetch_b")], 2);

    assert_eq!(definition.id(), "gather");
    assert_eq!(definition.branches().len(), 2);
    assert_eq!(definition.max_concurrency(), 2);
    assert_eq!(definition.output_key(), "gathered");
    assert_eq!(definition.transition(), Some("END"));

    for unsupported in ["one", "many"] {
        let yaml = format!(
            "id: gather\ntype: parallel\nbranches:\n  - id: a\n    node: fetch_a\n  - id: b\n    node: fetch_b\nmax_concurrency: 2\nwait: {unsupported}\noutput: [gathered]\n"
        );
        assert!(ParallelNodeDefinition::from_yaml(&yaml).is_err());
    }
    for invalid in [
        "id: gather\ntype: parallel\nbranches: [{id: a, node: x}, {id: a, node: y}]\nmax_concurrency: 2\nwait: all\noutput: [gathered]\n",
        "id: gather\ntype: parallel\nbranches: [{id: a, node: x}, {id: b, node: y}]\nmax_concurrency: 3\nwait: all\noutput: [gathered]\n",
        "id: gather\ntype: parallel\nbranches: [{id: a, node: x}, {id: b, node: y}]\nmax_concurrency: 2\nwait: all\noutput: []\n",
        "id: gather\ntype: parallel\nbranches: [{id: a, node: x}, {id: b, node: y}]\nmax_concurrency: 2\nwait: all\noutput: [one, two]\n",
    ] {
        assert!(ParallelNodeDefinition::from_yaml(invalid).is_err());
    }

    let oversized_branches = (0..65).fold(String::new(), |mut yaml, index| {
        write!(yaml, "  - id: branch-{index}\n    node: node-{index}\n")
            .expect("write oversized YAML fixture");
        yaml
    });
    let oversized_yaml = format!(
        "id: gather\ntype: parallel\nbranches:\n{oversized_branches}max_concurrency: 2\nwait: all\noutput: [gathered]\n"
    );
    assert!(ParallelNodeDefinition::from_yaml(&oversized_yaml).is_err());
}

#[test]
fn on_error_and_join_default_to_continue_and_append_and_accept_the_legacy_stop() {
    use elitea_agent_runtime::graph::fanout::plan::{JoinMode, OnError};
    let two = [("a", "x"), ("b", "y")];
    let defaults = definition_with(&two, 2, "");
    assert_eq!(defaults.on_error(), OnError::Continue);
    assert_eq!(defaults.join(), JoinMode::Append);
    let legacy = definition(&two, 2);
    assert_eq!(legacy.on_error(), OnError::Stop);
    let stop = definition_with(&two, 2, "on_error: stop\n");
    assert_eq!(stop.on_error(), OnError::Stop);
    // `stop` with `append` keeps the digest every earlier node froze under.
    assert_eq!(stop.config_digest(), legacy.config_digest());
    assert_ne!(defaults.config_digest(), legacy.config_digest());
    assert_eq!(
        definition_with(&two, 2, "error_policy: fail_after_drain\non_error: stop\n").on_error(),
        OnError::Stop
    );
    for (join, mode) in [
        ("append", JoinMode::Append),
        ("keyed", JoinMode::Keyed),
        ("merge", JoinMode::Merge),
    ] {
        let parsed = definition_with(&two, 2, &format!("on_error: stop\njoin: {join}\n"));
        assert_eq!(parsed.join(), mode);
    }
    assert_ne!(
        definition_with(&two, 2, "join: keyed\n").config_digest(),
        defaults.config_digest()
    );
    for refused in [
        "on_error: ignore\n",
        "join: last_write_wins\n",
        // A merged object has no place for a failed branch's entry.
        "join: merge\n",
        "on_error: continue\njoin: merge\n",
        "error_policy: fail_after_drain\non_error: continue\n",
        "error_policy: collect\n",
    ] {
        let yaml = format!(
            "id: gather\ntype: parallel\nbranches: [{{id: a, node: x}}, {{id: b, node: y}}]\nmax_concurrency: 2\nwait: all\n{refused}output: [gathered]\n"
        );
        assert!(
            ParallelNodeDefinition::from_yaml(&yaml).is_err(),
            "{refused}"
        );
    }
}

#[test]
fn sixteen_branches_and_concurrency_eight_are_the_limits() {
    let branches = |count: usize| {
        (0..count).fold(String::new(), |mut yaml, index| {
            write!(yaml, "  - id: b{index}\n    node: n{index}\n").expect("write YAML fixture");
            yaml
        })
    };
    let yaml = |count: usize, concurrency: usize| {
        format!(
            "id: gather\ntype: parallel\nbranches:\n{}max_concurrency: {concurrency}\nwait: all\noutput: [gathered]\n",
            branches(count)
        )
    };
    assert!(ParallelNodeDefinition::from_yaml(&yaml(16, 8)).is_ok());
    assert!(ParallelNodeDefinition::from_yaml(&yaml(17, 8)).is_err());
    assert!(ParallelNodeDefinition::from_yaml(&yaml(16, 9)).is_err());
}

#[tokio::test]
async fn declared_order_is_stable_when_branches_finish_in_reverse_order() {
    let release_first = Arc::new(Notify::new());
    let first_release = Arc::clone(&release_first);
    let first: Behavior = Arc::new(move || {
        let first_release = Arc::clone(&first_release);
        Box::pin(async move {
            first_release.notified().await;
            Ok(json!({"value": "first"}))
        })
    });
    let second_release = Arc::clone(&release_first);
    let second: Behavior = Arc::new(move || {
        let second_release = Arc::clone(&second_release);
        Box::pin(async move {
            second_release.notify_one();
            Ok(json!({"value": "second"}))
        })
    });
    let graph = parent_graph(
        definition(&[("a", "first"), ("b", "second")], 2),
        runtime(
            Arc::new(MemoryChildCheckpoints::default()),
            HashMap::from([("first".to_owned(), first), ("second".to_owned(), second)]),
        ),
    );

    let result = graph
        .invoke(
            HashMap::from([("input".to_owned(), json!("same"))]),
            ExecutionConfig::new("root-order"),
        )
        .await
        .expect("parallel graph result");

    assert_eq!(
        result["gathered"],
        json!([
            {"branch_id": "a", "node": "first", "status": "succeeded", "outputs": {"value": "first"}},
            {"branch_id": "b", "node": "second", "status": "succeeded", "outputs": {"value": "second"}},
        ])
    );
}

#[tokio::test]
async fn multiple_failures_drain_and_select_the_first_declared_branch() {
    let runs = Arc::new(AtomicUsize::new(0));
    let failing = |node: &'static str| {
        let runs = Arc::clone(&runs);
        Arc::new(move || {
            let runs = Arc::clone(&runs);
            Box::pin(async move {
                runs.fetch_add(1, Ordering::SeqCst);
                Err(GraphError::NodeExecutionFailed {
                    node: node.to_owned(),
                    message: "fixture failure".to_owned(),
                })
            }) as BehaviorFuture
        }) as Behavior
    };
    let graph = parent_graph(
        definition(&[("a", "first"), ("b", "second")], 2),
        runtime(
            Arc::new(MemoryChildCheckpoints::default()),
            HashMap::from([
                ("first".to_owned(), failing("first")),
                ("second".to_owned(), failing("second")),
            ]),
        ),
    );

    let error = graph
        .invoke(State::new(), ExecutionConfig::new("root-errors"))
        .await
        .expect_err("parallel graph must report a branch failure");

    assert_eq!(runs.load(Ordering::SeqCst), 2);
    match error {
        GraphError::NodeExecutionFailed { message, .. } => {
            assert!(message.contains("branch 'a'"));
            assert!(!message.contains("branch 'b'"));
        }
        other => panic!("unexpected parallel failure: {other}"),
    }
}

#[tokio::test]
async fn failure_stops_later_admission_at_concurrency_one() {
    let later_runs = Arc::new(AtomicUsize::new(0));
    let later_counter = Arc::clone(&later_runs);
    let later: Behavior = Arc::new(move || {
        let later_counter = Arc::clone(&later_counter);
        Box::pin(async move {
            later_counter.fetch_add(1, Ordering::SeqCst);
            Ok(json!({"unexpected": true}))
        })
    });
    let first: Behavior = Arc::new(|| {
        Box::pin(async {
            Err(GraphError::NodeExecutionFailed {
                node: "first".to_owned(),
                message: "fixture failure".to_owned(),
            })
        })
    });
    let graph = parent_graph(
        definition(&[("first", "first"), ("later", "later")], 1),
        runtime(
            Arc::new(MemoryChildCheckpoints::default()),
            HashMap::from([("first".to_owned(), first), ("later".to_owned(), later)]),
        ),
    );

    assert!(
        graph
            .invoke(State::new(), ExecutionConfig::new("root-stop-admission"))
            .await
            .is_err()
    );
    assert_eq!(later_runs.load(Ordering::SeqCst), 0);
}

#[tokio::test]
async fn failure_drains_a_slow_inflight_sibling_without_admitting_pending_work() {
    let (entered_tx, mut entered_rx) = mpsc::channel(1);
    let release = Arc::new(Semaphore::new(0));
    let slow_release = Arc::clone(&release);
    let slow: Behavior = Arc::new(move || {
        let entered_tx = entered_tx.clone();
        let slow_release = Arc::clone(&slow_release);
        Box::pin(async move {
            entered_tx
                .send(())
                .await
                .map_err(|_| GraphError::Other("observer closed".to_owned()))?;
            slow_release
                .acquire()
                .await
                .map_err(|_| GraphError::Other("release closed".to_owned()))?
                .forget();
            Ok(json!({"drained": true}))
        })
    });
    let first: Behavior = Arc::new(|| {
        Box::pin(async {
            Err(GraphError::NodeExecutionFailed {
                node: "first".to_owned(),
                message: "fixture failure".to_owned(),
            })
        })
    });
    let pending_runs = Arc::new(AtomicUsize::new(0));
    let pending_counter = Arc::clone(&pending_runs);
    let pending: Behavior = Arc::new(move || {
        let pending_counter = Arc::clone(&pending_counter);
        Box::pin(async move {
            pending_counter.fetch_add(1, Ordering::SeqCst);
            Ok(json!({"unexpected": true}))
        })
    });
    let graph = parent_graph(
        definition(
            &[("first", "first"), ("slow", "slow"), ("pending", "pending")],
            2,
        ),
        runtime(
            Arc::new(MemoryChildCheckpoints::default()),
            HashMap::from([
                ("first".to_owned(), first),
                ("slow".to_owned(), slow),
                ("pending".to_owned(), pending),
            ]),
        ),
    );
    let run = tokio::spawn(async move {
        graph
            .invoke(State::new(), ExecutionConfig::new("root-inflight-drain"))
            .await
    });

    tokio::time::timeout(Duration::from_secs(1), entered_rx.recv())
        .await
        .expect("slow sibling entry wait")
        .expect("slow sibling entry");
    assert!(!run.is_finished());
    release.add_permits(1);
    assert!(run.await.expect("parallel graph task").is_err());
    assert_eq!(pending_runs.load(Ordering::SeqCst), 0);
}

#[tokio::test]
async fn failed_branch_is_stable_and_changed_input_is_refused() {
    let checkpoints = Arc::new(MemoryChildCheckpoints::default());
    let short_runs = Arc::new(AtomicUsize::new(0));
    let short_counter = Arc::clone(&short_runs);
    let short: Behavior = Arc::new(move || {
        let short_counter = Arc::clone(&short_counter);
        Box::pin(async move {
            short_counter.fetch_add(1, Ordering::SeqCst);
            Ok(json!({"value": "short"}))
        })
    });
    let long_runs = Arc::new(AtomicUsize::new(0));
    let long_counter = Arc::clone(&long_runs);
    let long: Behavior = Arc::new(move || {
        let long_counter = Arc::clone(&long_counter);
        Box::pin(async move {
            if long_counter.fetch_add(1, Ordering::SeqCst) == 0 {
                Err(GraphError::NodeExecutionFailed {
                    node: "long".to_owned(),
                    message: "fixture failure".to_owned(),
                })
            } else {
                Ok(json!({"value": "long"}))
            }
        })
    });
    let behaviors = HashMap::from([("short".to_owned(), short), ("long".to_owned(), long)]);
    let definition = definition(&[("short", "short"), ("long", "long")], 2);

    let first = parent_graph(
        definition.clone(),
        runtime(Arc::clone(&checkpoints), behaviors.clone()),
    );
    assert!(
        first
            .invoke(
                HashMap::from([("input".to_owned(), json!("original"))]),
                ExecutionConfig::new("root-restart"),
            )
            .await
            .is_err()
    );

    let recreated = parent_graph(
        definition.clone(),
        runtime(Arc::clone(&checkpoints), behaviors.clone()),
    );
    let same_input = recreated
        .invoke(
            HashMap::from([("input".to_owned(), json!("original"))]),
            ExecutionConfig::new("root-restart"),
        )
        .await;
    assert!(same_input.is_err());
    assert_eq!(short_runs.load(Ordering::SeqCst), 1);
    assert_eq!(long_runs.load(Ordering::SeqCst), 1);

    let changed = parent_graph(definition, runtime(checkpoints, behaviors))
        .invoke(
            HashMap::from([("input".to_owned(), json!("changed-after-restart"))]),
            ExecutionConfig::new("root-restart"),
        )
        .await;
    assert!(changed.is_err());
    assert_eq!(short_runs.load(Ordering::SeqCst), 1);
    assert_eq!(long_runs.load(Ordering::SeqCst), 1);
}

#[tokio::test]
async fn admission_bound_is_structural_and_all_started_branches_drain() {
    const WIDTH: usize = 4;
    const LIMIT: usize = 2;
    let (entered_tx, mut entered_rx) = mpsc::channel(WIDTH);
    let release = Arc::new(Semaphore::new(0));
    let mut behaviors = HashMap::new();
    let mut branches = Vec::new();
    for index in 0..WIDTH {
        let entered = entered_tx.clone();
        let permits = Arc::clone(&release);
        let behavior: Behavior = Arc::new(move || {
            let entered = entered.clone();
            let permits = Arc::clone(&permits);
            Box::pin(async move {
                entered
                    .send(index)
                    .await
                    .map_err(|_| GraphError::Other("observer closed".to_owned()))?;
                permits
                    .acquire()
                    .await
                    .map_err(|_| GraphError::Other("release closed".to_owned()))?
                    .forget();
                Ok(json!({"index": index}))
            })
        });
        let name = format!("node-{index}");
        behaviors.insert(name.clone(), behavior);
        branches.push((format!("branch-{index}"), name));
    }
    drop(entered_tx);
    let branch_refs = branches
        .iter()
        .map(|(branch, node)| (branch.as_str(), node.as_str()))
        .collect::<Vec<_>>();
    let graph = parent_graph(
        definition(&branch_refs, LIMIT),
        runtime(Arc::new(MemoryChildCheckpoints::default()), behaviors),
    );
    let run = tokio::spawn(async move {
        graph
            .invoke(State::new(), ExecutionConfig::new("root-bounded"))
            .await
    });

    for _ in 0..LIMIT {
        tokio::time::timeout(Duration::from_secs(1), entered_rx.recv())
            .await
            .expect("bounded entry wait")
            .expect("entry observer");
    }
    assert!(entered_rx.try_recv().is_err());
    release.add_permits(LIMIT);
    for _ in LIMIT..WIDTH {
        tokio::time::timeout(Duration::from_secs(1), entered_rx.recv())
            .await
            .expect("next bounded entry wait")
            .expect("entry observer");
    }
    release.add_permits(WIDTH - LIMIT);

    let result = run
        .await
        .expect("parallel graph task")
        .expect("parallel result");
    assert_eq!(result["gathered"].as_array().map(Vec::len), Some(WIDTH));
}

#[tokio::test]
async fn loop_visits_get_distinct_child_checkpoint_threads() {
    let checkpoints = Arc::new(MemoryChildCheckpoints::default());
    let runtime = runtime(
        Arc::clone(&checkpoints),
        HashMap::from([
            ("one".to_owned(), constant(json!({"value": 1}))),
            ("two".to_owned(), constant(json!({"value": 2}))),
        ]),
    );
    let node = DurableParallelNode::new(definition(&[("a", "one"), ("b", "two")], 2), runtime);

    node.execute(&NodeContext::new(
        State::new(),
        ExecutionConfig::new("root-loop"),
        3,
    ))
    .await
    .expect("first loop visit");
    // Simulate the owning parent's successful frontier advancement.
    let parent: Arc<dyn ParallelCheckpointAppender> = checkpoints.parent.clone();
    parent
        .save(&adk_rust::graph::Checkpoint::new(
            "root-loop",
            State::new(),
            4,
            vec!["gather".to_owned()],
        ))
        .await
        .expect("parent advancement");
    node.execute(&NodeContext::new(
        State::new(),
        ExecutionConfig::new("root-loop"),
        7,
    ))
    .await
    .expect("second loop visit");

    let threads = checkpoints.issued_threads.lock().await;
    assert_eq!(threads.len(), 4);
    assert_ne!(threads[0], threads[2]);
    assert_ne!(threads[1], threads[3]);
}

struct PausingBranchGraphs {
    runs: Arc<HashMap<String, AtomicUsize>>,
    complete: BTreeSet<String>,
    /// Branches that pause with this many cards at once (default one).
    cards: BTreeMap<String, usize>,
}

impl PausingBranchGraphs {
    fn card_count(&self, name: &str) -> usize {
        self.cards.get(name).copied().unwrap_or(1)
    }
}

#[async_trait]
impl ParallelBranchGraphFactory for PausingBranchGraphs {
    fn validate_branch(&self, branch: &ParallelBranchDefinition) -> Result<(), GraphError> {
        if self.runs.contains_key(branch.node()) {
            Ok(())
        } else {
            Err(GraphError::NodeNotFound(branch.node().to_owned()))
        }
    }

    fn owned_definition_digest(
        &self,
        branch: &ParallelBranchDefinition,
    ) -> Result<[u8; 32], GraphError> {
        let mut digest = [0; 32];
        digest.copy_from_slice(
            ring::digest::digest(&ring::digest::SHA256, branch.node().as_bytes()).as_ref(),
        );
        Ok(digest)
    }

    fn compile_branch(
        &self,
        branch: &ParallelBranchDefinition,
        execution: ParallelBranchExecution,
    ) -> Result<CompiledGraph, GraphError> {
        let runs = Arc::clone(&self.runs);
        let name = branch.node().to_owned();
        let node_name = name.clone();
        let complete = self.complete.contains(&name);
        let count = self.card_count(&name);
        let node = FunctionNode::new(&name, move |context| {
            let runs = Arc::clone(&runs);
            let name = node_name.clone();
            async move {
                runs[&name].fetch_add(1, Ordering::SeqCst);
                if !complete && !context.state.contains_key("__elitea_hitl_resume_v1") {
                    let cards = (0..count)
                        .map(|card| {
                            let suffix = if card == 0 {
                                String::new()
                            } else {
                                format!("-{card}")
                            };
                            json!({
                                "interrupt_id": format!("card-{name}{suffix}"),
                                "tool_call_id": format!("call-{name}{suffix}"),
                            })
                        })
                        .collect::<Vec<_>>();
                    return Ok(NodeOutput::interrupt_with_data(
                        "Child paused.",
                        json!({ "cards": cards }),
                    ));
                }
                let blocked = context
                    .state
                    .get("__elitea_hitl_resume_v1")
                    .and_then(|value| value.get("action"))
                    .and_then(Value::as_str)
                    == Some("reject");
                Ok(NodeOutput::new()
                    .with_update("branch_result", json!({"output": name, "blocked": blocked})))
            }
        });
        StateGraph::with_channels(&["branch_input", "branch_result", "__elitea_hitl_resume_v1"])
            .add_node(execution.wrap_node(node))
            .add_edge(START, &name)
            .add_edge(&name, adk_rust::graph::END)
            .compile()
            .map(|graph| {
                graph
                    .with_checkpointer_arc(execution.checkpointer())
                    .with_strict_channels()
            })
    }

    fn project_input(
        &self,
        _branch: &ParallelBranchDefinition,
        parent: &State,
    ) -> Result<State, GraphError> {
        Ok(HashMap::from([(
            "branch_input".to_owned(),
            parent.get("input").cloned().unwrap_or(Value::Null),
        )]))
    }

    fn project_result(
        &self,
        _branch: &ParallelBranchDefinition,
        child: &State,
    ) -> Result<ParallelBranchTerminal, GraphError> {
        let result = child
            .get("branch_result")
            .and_then(Value::as_object)
            .cloned()
            .ok_or_else(|| GraphError::Other("result missing".to_owned()))?;
        if result.get("blocked") == Some(&json!(true)) {
            Ok(ParallelBranchTerminal::Blocked)
        } else {
            Ok(ParallelBranchTerminal::Completed(result))
        }
    }

    fn pause_cards(
        &self,
        _branch: &ParallelBranchDefinition,
        pause: &ParallelBranchPause,
    ) -> Result<Vec<ParallelPauseCard>, GraphError> {
        let adk_rust::graph::interrupt::Interrupt::Dynamic {
            data: Some(data), ..
        } = &pause.interrupt
        else {
            return Err(GraphError::Other("pause missing".to_owned()));
        };
        serde_json::from_value(data["cards"].clone())
            .map_err(|_| GraphError::Other("pause malformed".to_owned()))
    }

    async fn resume_input(
        &self,
        branch: &ParallelBranchDefinition,
        pause: &ParallelBranchPause,
        decisions: &[ParallelDecision],
    ) -> Result<State, GraphError> {
        assert!(!pause.thread_id.is_empty());
        assert!(!pause.checkpoint_id.is_empty());
        // One resume carries exactly this child's whole decision set.
        assert_eq!(decisions.len(), self.card_count(branch.node()));
        assert!(decisions.iter().all(|decision| {
            decision
                .interrupt_id
                .starts_with(&format!("card-{}", branch.node()))
        }));
        Ok(HashMap::from([(
            "__elitea_hitl_resume_v1".to_owned(),
            json!({"action": decisions[0].action}),
        )]))
    }
}

fn pausing_runtime(
    checkpoints: &Arc<MemoryChildCheckpoints>,
    runs: &Arc<HashMap<String, AtomicUsize>>,
    complete: &[&str],
) -> Arc<AdkParallelBranchRuntime> {
    multi_card_runtime(checkpoints, runs, complete, &[])
}

fn multi_card_runtime(
    checkpoints: &Arc<MemoryChildCheckpoints>,
    runs: &Arc<HashMap<String, AtomicUsize>>,
    complete: &[&str],
    cards: &[(&str, usize)],
) -> Arc<AdkParallelBranchRuntime> {
    let factory: Arc<dyn ParallelChildCheckpointerFactory> = checkpoints.clone();
    let parent: Arc<dyn ParallelCheckpointAppender> = checkpoints.parent.clone();
    Arc::new(AdkParallelBranchRuntime::new(
        factory,
        Arc::new(PausingBranchGraphs {
            runs: Arc::clone(runs),
            complete: complete.iter().map(|name| (*name).to_owned()).collect(),
            cards: cards
                .iter()
                .map(|(name, count)| ((*name).to_owned(), *count))
                .collect(),
        }),
        Arc::new(ParallelOccurrenceCheckpointer::new(parent)),
    ))
}

fn run_counts(names: &[&str]) -> Arc<HashMap<String, AtomicUsize>> {
    Arc::new(
        names
            .iter()
            .map(|name| ((*name).to_owned(), AtomicUsize::new(0)))
            .collect(),
    )
}

fn runs_of(runs: &HashMap<String, AtomicUsize>, name: &str) -> usize {
    runs[name].load(Ordering::SeqCst)
}

fn stored_activation(checkpoint: &Checkpoint) -> ParallelActivation {
    serde_json::from_value(checkpoint.metadata[OCCURRENCE]["activation"].clone()).unwrap()
}

/// The parked payload (`fanout-parked.schema.json`): `(interrupt_key,
/// interrupt_id)` per open card. It never carries a checkpoint or thread id.
fn parked(output: &NodeOutput) -> Vec<(String, String)> {
    let Some(adk_rust::graph::interrupt::Interrupt::Dynamic {
        data: Some(data), ..
    }) = output.interrupt.as_ref()
    else {
        panic!("expected the parked fan-out interrupt");
    };
    parked_data(data)
}

fn parked_data(data: &Value) -> Vec<(String, String)> {
    let object = data.as_object().expect("parked payload object");
    assert_eq!(
        object.keys().map(String::as_str).collect::<BTreeSet<_>>(),
        BTreeSet::from(["decision_revision_seen", "pending", "schema"])
    );
    assert_eq!(data["schema"], FANOUT_PARKED_SCHEMA);
    assert!(data["decision_revision_seen"].as_u64().is_some());
    let text = serde_json::to_string(data).unwrap();
    assert!(!text.contains("checkpoint") && !text.contains("thread"));
    let pending = data["pending"].as_array().expect("pending cards");
    assert!((1..=MAX_OPEN_INTERRUPTS).contains(&pending.len()));
    let cards = pending
        .iter()
        .map(|card| {
            assert_eq!(card.as_object().map(serde_json::Map::len), Some(2));
            let key = card["interrupt_key"].as_str().expect("key").to_owned();
            assert_eq!(key.len(), 64);
            assert!(
                key.bytes()
                    .all(|byte| matches!(byte, b'0'..=b'9' | b'a'..=b'f'))
            );
            (key, card["interrupt_id"].as_str().expect("id").to_owned())
        })
        .collect::<Vec<_>>();
    assert_eq!(
        cards
            .iter()
            .map(|(key, _)| key)
            .collect::<BTreeSet<_>>()
            .len(),
        cards.len(),
        "a card is listed twice"
    );
    cards
}

fn key_of(cards: &[(String, String)], interrupt_id: &str) -> String {
    cards.iter().find(|(_, id)| id == interrupt_id).map_or_else(
        || panic!("no open card {interrupt_id}"),
        |(key, _)| key.clone(),
    )
}

fn ledger_node(
    definition: ParallelNodeDefinition,
    runtime: Arc<AdkParallelBranchRuntime>,
    ledger: &Arc<TestLedger>,
    claim: u64,
) -> DurableParallelNode {
    DurableParallelNode::new(definition, runtime).with_decisions(ledger.client(claim))
}

fn input_context(thread: &str, step: usize) -> NodeContext {
    NodeContext::new(
        HashMap::from([("input".to_owned(), json!("original"))]),
        ExecutionConfig::new(thread),
        step,
    )
}

fn applied(acks: &[(String, AckResult)], key: &str) -> usize {
    acks.iter()
        .filter(|(acked, result)| acked == key && matches!(result, AckResult::Applied { .. }))
        .count()
}

#[tokio::test]
async fn child_pause_and_completion_survive_loss_before_parent_pause_save() {
    use super::parallel::ParallelBranchRuntime;
    let checkpoints = Arc::new(MemoryChildCheckpoints::default());
    let runs = run_counts(&["done", "pause"]);
    let definition = definition(&[("done", "done"), ("pause", "pause")], 2);
    let runtime = pausing_runtime(&checkpoints, &runs, &["done"]);
    let context = input_context("root-cutpoint", 4);
    let mut activation = ParallelActivation {
        root_thread_id: "root-cutpoint".to_owned(),
        node_id: "gather".to_owned(),
        step: 4,
        config_digest: definition.config_digest(),
    };
    for branch in runtime
        .prepare(&mut activation, &definition, &context)
        .await
        .unwrap()
    {
        let _ = runtime.invoke(&activation, branch, &context).await;
    }
    // The parent has no pause row at this cutpoint; the children's receipts
    // carry everything a replacement process needs.
    let ledger = TestLedger::new();
    let recreated = ledger_node(
        definition.clone(),
        pausing_runtime(&checkpoints, &runs, &["done"]),
        &ledger,
        1,
    );
    let cards = parked(&recreated.execute(&context).await.unwrap());
    assert_eq!(cards.len(), 1);
    assert_eq!((runs_of(&runs, "done"), runs_of(&runs, "pause")), (1, 1));
    ledger.decide(&key_of(&cards, "card-pause"), "approve", None);
    let output = recreated.execute(&context).await.unwrap();
    assert_eq!(output.updates["gathered"][0]["outputs"]["output"], "done");
    assert_eq!(output.updates["gathered"][1]["outputs"]["output"], "pause");
    assert_eq!((runs_of(&runs, "done"), runs_of(&runs, "pause")), (1, 2));
    assert_eq!(ledger.open_decisions(), 0);
    assert_eq!(applied(&ledger.acks(), &key_of(&cards, "card-pause")), 1);
}

/// `before_ack`: the resumed child saved, the process died before the ACK.
/// The replacement finds the child past its pause, ACKs `applied` with the
/// current checkpoint and never resumes it again.
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn a_decision_applied_before_a_lost_ack_is_acknowledged_without_a_second_resume() {
    let checkpoints = Arc::new(MemoryChildCheckpoints::default());
    let runs = run_counts(&["a", "b"]);
    let definition = definition(&[("a", "a"), ("b", "b")], 1);
    let context = input_context("root-lost-ack", 2);
    let ledger = TestLedger::new();
    let first = ledger_node(
        definition.clone(),
        pausing_runtime(&checkpoints, &runs, &[]),
        &ledger,
        1,
    );
    let cards = parked(&first.execute(&context).await.unwrap());
    ledger.decide(&key_of(&cards, "card-a"), "approve", None);
    ledger.crash_at(LedgerSeam::BeforeAck);
    let crashed = tokio::spawn(async move {
        let _ = first.execute(&input_context("root-lost-ack", 2)).await;
    });
    ledger.reached.notified().await;
    crashed.abort();
    let _ = crashed.await;
    assert_eq!((runs_of(&runs, "a"), runs_of(&runs, "b")), (2, 1));
    assert_eq!(ledger.open_decisions(), 1, "the ACK never reached Main");

    ledger.decide(&key_of(&cards, "card-b"), "approve", None);
    let output = ledger_node(
        definition,
        pausing_runtime(&checkpoints, &runs, &[]),
        &ledger,
        2,
    )
    .execute(&context)
    .await
    .unwrap();
    assert!(output.interrupt.is_none());
    assert_eq!(output.updates["gathered"].as_array().unwrap().len(), 2);
    assert_eq!((runs_of(&runs, "a"), runs_of(&runs, "b")), (2, 2));
    let acks = ledger.acks();
    assert_eq!(applied(&acks, &key_of(&cards, "card-a")), 1);
    assert_eq!(applied(&acks, &key_of(&cards, "card-b")), 1);
    assert_eq!(ledger.open_decisions(), 0);
}

/// Per-interrupt rules (design §10): a decision for a card this activation
/// never opened, a forged earlier pause, a duplicate in one fetch, or changed
/// business input runs no child. Only an exact open card resumes its child.
#[tokio::test]
async fn stale_foreign_duplicate_and_changed_input_decisions_run_no_child() {
    let checkpoints = Arc::new(MemoryChildCheckpoints::default());
    let runs = run_counts(&["a", "b"]);
    let definition = definition(&[("a", "a"), ("b", "b")], 2);
    let ledger = TestLedger::new();
    let node = ledger_node(
        definition.clone(),
        pausing_runtime(&checkpoints, &runs, &[]),
        &ledger,
        1,
    );
    let context = input_context("root-invalid-decisions", 0);
    let cards = parked(&node.execute(&context).await.unwrap());
    assert_eq!((runs_of(&runs, "a"), runs_of(&runs, "b")), (1, 1));

    // Foreign: a key no child of this occurrence ever paused under.
    let foreign = "f".repeat(64);
    ledger.decide(&foreign, "approve", None);
    // Stale: the right card under a pause checkpoint the child never saved.
    let parent = checkpoints
        .parent
        .load("root-invalid-decisions")
        .await
        .unwrap()
        .unwrap();
    let activation = stored_activation(&parent);
    let child_thread = checkpoints.issued_threads.lock().await[0].clone();
    let stale = elitea_agent_runtime::graph::fanout::identity::interrupt_key(
        &elitea_agent_runtime::graph::fanout::identity::InterruptKeyInput {
            root_thread: "root-invalid-decisions",
            fanout_node_id: "gather",
            step: 0,
            config_digest: &elitea_agent_runtime::graph::fanout::identity::lowercase_hex(
                &activation.config_digest,
            ),
            ordinal: 1,
            child_thread: &child_thread,
            child_paused_checkpoint_id: "an-older-pause",
            interrupt_id: "card-a",
            tool_call_id: "call-a",
        },
    );
    ledger.decide(&stale, "approve", None);
    let again = parked(&node.execute(&context).await.unwrap());
    assert_eq!(
        again, cards,
        "a stale or foreign decision changed the open cards"
    );
    assert_eq!((runs_of(&runs, "a"), runs_of(&runs, "b")), (1, 1));
    let acks = ledger.acks();
    for key in [&foreign, &stale] {
        assert!(
            acks.iter()
                .any(|(acked, result)| acked == key && *result == AckResult::Stale)
        );
    }
    assert_eq!(ledger.open_decisions(), 0);

    // Changed business input: the occurrence refuses before any fetch or child.
    ledger.decide(&key_of(&cards, "card-a"), "approve", None);
    let fetches = ledger.fetches();
    let changed = NodeContext::new(
        HashMap::from([("input".to_owned(), json!("changed"))]),
        ExecutionConfig::new("root-invalid-decisions"),
        0,
    );
    assert!(node.execute(&changed).await.is_err());
    assert_eq!(ledger.fetches(), fetches);
    assert_eq!((runs_of(&runs, "a"), runs_of(&runs, "b")), (1, 1));

    // Duplicate: the same decision fetched twice resumes its child once.
    ledger.redeliver(&key_of(&cards, "card-a"));
    let duplicate_node = ledger_node(
        definition,
        pausing_runtime(&checkpoints, &runs, &[]),
        &ledger,
        2,
    );
    let first = parked(&duplicate_node.execute(&context).await.unwrap());
    assert_eq!(first, vec![cards[1].clone()], "only b stays open");
    let replay = parked(&duplicate_node.execute(&context).await.unwrap());
    assert_eq!(replay, first);
    assert_eq!((runs_of(&runs, "a"), runs_of(&runs, "b")), (2, 1));
    assert_eq!(applied(&ledger.acks(), &key_of(&cards, "card-a")), 1);
}

/// Card 2 answered first resumes only child 2; child 1 keeps its card.
#[tokio::test]
async fn the_second_card_answered_first_resumes_only_its_own_child() {
    let checkpoints = Arc::new(MemoryChildCheckpoints::default());
    let runs = run_counts(&["a", "b", "c"]);
    let definition = definition_with(&[("a", "a"), ("b", "b"), ("c", "c")], 3, "");
    let ledger = TestLedger::new();
    let node = ledger_node(
        definition,
        pausing_runtime(&checkpoints, &runs, &["c"]),
        &ledger,
        1,
    );
    let context = input_context("root-reverse", 1);
    let cards = parked(&node.execute(&context).await.unwrap());
    assert_eq!(cards.len(), 2);
    ledger.decide(&key_of(&cards, "card-b"), "approve", None);
    let after_b = parked(&node.execute(&context).await.unwrap());
    assert_eq!(after_b, vec![cards[0].clone()]);
    assert_eq!(
        (
            runs_of(&runs, "a"),
            runs_of(&runs, "b"),
            runs_of(&runs, "c")
        ),
        (1, 2, 1)
    );
    ledger.decide(&key_of(&cards, "card-a"), "approve", None);
    let output = node.execute(&context).await.unwrap();
    assert_eq!(output.updates["gathered"].as_array().unwrap().len(), 3);
    assert_eq!(
        (
            runs_of(&runs, "a"),
            runs_of(&runs, "b"),
            runs_of(&runs, "c")
        ),
        (2, 2, 1)
    );
}

/// A child paused with two cards resumes once, with both decisions.
#[tokio::test]
async fn a_child_with_two_cards_resumes_once_after_both_are_decided() {
    let checkpoints = Arc::new(MemoryChildCheckpoints::default());
    let runs = run_counts(&["a", "b"]);
    let ledger = TestLedger::new();
    let node = ledger_node(
        definition_with(&[("a", "a"), ("b", "b")], 2, ""),
        multi_card_runtime(&checkpoints, &runs, &["b"], &[("a", 2)]),
        &ledger,
        1,
    );
    let context = input_context("root-two-cards", 0);
    let cards = parked(&node.execute(&context).await.unwrap());
    assert_eq!(cards.len(), 2);
    ledger.decide(&key_of(&cards, "card-a"), "approve", None);
    assert_eq!(parked(&node.execute(&context).await.unwrap()), cards);
    assert_eq!(runs_of(&runs, "a"), 1, "resumed before its second card");
    ledger.decide(&key_of(&cards, "card-a-1"), "approve", None);
    let output = node.execute(&context).await.unwrap();
    assert!(output.interrupt.is_none());
    assert_eq!(runs_of(&runs, "a"), 2);
}

#[tokio::test]
async fn the_parked_keys_are_minted_from_the_frozen_child_and_its_pause_checkpoint() {
    use elitea_agent_runtime::graph::fanout::identity::{
        InterruptKeyInput, interrupt_key, lowercase_hex,
    };
    let checkpoints = Arc::new(MemoryChildCheckpoints::default());
    let runs = run_counts(&["a", "b"]);
    let node = DurableParallelNode::new(
        definition_with(&[("a", "a"), ("b", "b")], 2, ""),
        pausing_runtime(&checkpoints, &runs, &["a"]),
    );
    let cards = parked(&node.execute(&input_context("root-keys", 5)).await.unwrap());
    let parent = checkpoints.parent.load("root-keys").await.unwrap().unwrap();
    let activation = stored_activation(&parent);
    let thread = checkpoints.issued_threads.lock().await[1].clone();
    let store = Arc::clone(&checkpoints.stores.lock().await[&thread]);
    let pause = store.load(&thread).await.unwrap().unwrap();
    assert_eq!(
        cards,
        vec![(
            interrupt_key(&InterruptKeyInput {
                root_thread: "root-keys",
                fanout_node_id: "gather",
                step: 5,
                config_digest: &lowercase_hex(&activation.config_digest),
                ordinal: 2,
                child_thread: &thread,
                child_paused_checkpoint_id: &pause.checkpoint_id,
                interrupt_id: "card-b",
                tool_call_id: "call-b",
            }),
            "card-b".to_owned(),
        )]
    );
}

#[tokio::test]
async fn sixteen_open_cards_park_and_a_seventeenth_fails_typed() {
    let names = (0..16).map(|index| format!("n{index}")).collect::<Vec<_>>();
    let refs = names.iter().map(String::as_str).collect::<Vec<_>>();
    let branches = names
        .iter()
        .map(|name| (name.as_str(), name.as_str()))
        .collect::<Vec<_>>();
    let checkpoints = Arc::new(MemoryChildCheckpoints::default());
    let runs = run_counts(&refs);
    let at_limit = DurableParallelNode::new(
        definition_with(&branches, 8, ""),
        pausing_runtime(&checkpoints, &runs, &[]),
    );
    let cards = parked(
        &at_limit
            .execute(&input_context("root-16", 0))
            .await
            .unwrap(),
    );
    assert_eq!(cards.len(), MAX_OPEN_INTERRUPTS);

    let checkpoints = Arc::new(MemoryChildCheckpoints::default());
    let runs = run_counts(&refs);
    let over = DurableParallelNode::new(
        definition_with(&branches, 8, ""),
        multi_card_runtime(&checkpoints, &runs, &[], &[("n0", 2)]),
    );
    let error = over
        .execute(&input_context("root-17", 0))
        .await
        .err()
        .expect("seventeen open cards are refused");
    assert!(
        error
            .to_string()
            .contains("graph.parallel.pause_resource_exhausted"),
        "{error}"
    );
}

#[tokio::test]
async fn a_blocked_branch_is_a_typed_failure_and_the_adk_parent_cannot_report_success() {
    let checkpoints = Arc::new(MemoryChildCheckpoints::default());
    let runs = run_counts(&["a", "b"]);
    let definition = definition(&[("a", "a"), ("b", "b")], 2);
    let ledger = TestLedger::new();
    let runtime = pausing_runtime(&checkpoints, &runs, &[]);
    let parent = runtime.parent_checkpointer();
    let successor_runs = Arc::new(AtomicUsize::new(0));
    let successor_counter = Arc::clone(&successor_runs);
    let graph = StateGraph::with_channels(&["input", "gathered"])
        .add_node(ledger_node(definition.clone(), runtime, &ledger, 1))
        .add_node(FunctionNode::new("after", move |_| {
            let counter = Arc::clone(&successor_counter);
            async move {
                counter.fetch_add(1, Ordering::SeqCst);
                Ok(NodeOutput::new())
            }
        }))
        .add_edge(START, "gather")
        .add_edge("gather", "after")
        .add_edge("after", adk_rust::graph::END)
        .compile()
        .unwrap()
        .with_checkpointer_arc(parent);
    let thread = "root-blocked";
    let GraphError::Interrupted(interrupted) = graph
        .invoke(
            HashMap::from([("input".to_owned(), json!("original"))]),
            ExecutionConfig::new(thread),
        )
        .await
        .unwrap_err()
    else {
        panic!("expected parent pause")
    };
    let adk_rust::graph::interrupt::Interrupt::Dynamic {
        data: Some(pause), ..
    } = interrupted.interrupt
    else {
        panic!("expected the parked payload")
    };
    let cards = parked_data(&pause);
    ledger.decide(&key_of(&cards, "card-a"), "reject", None);
    ledger.decide(&key_of(&cards, "card-b"), "approve", None);
    let error = graph
        .invoke(
            HashMap::new(),
            ExecutionConfig::new(thread).with_resume_from(&interrupted.checkpoint_id),
        )
        .await
        .unwrap_err();
    assert!(matches!(error, GraphError::NodeExecutionFailed { .. }));
    assert!(error.to_string().contains("graph.parallel.branch_failed"));
    assert!(error.to_string().contains("graph.parallel.branch_blocked"));
    assert!(error.to_string().contains("branch 'a'"));
    assert_eq!(successor_runs.load(Ordering::SeqCst), 0);
    let saved = checkpoints.parent.load(thread).await.unwrap().unwrap();
    assert!(!saved.state.contains_key("gathered"));
    assert_eq!(saved.pending_nodes, ["gather"]);
    // No parent denial row: freeze and the pause row only.
    assert_eq!(checkpoints.parent.list(thread).await.unwrap().len(), 2);
    // The failure is replayed from the child receipts, typed, deterministic.
    let replay = DurableParallelNode::new(definition, pausing_runtime(&checkpoints, &runs, &[]))
        .execute(&NodeContext::new(
            saved.state.clone(),
            ExecutionConfig::new(thread),
            saved.step,
        ))
        .await
        .err()
        .expect("the denial stays a failure");
    assert!(replay.to_string().contains("graph.parallel.branch_blocked"));
    assert_eq!((runs_of(&runs, "a"), runs_of(&runs, "b")), (2, 2));
}

#[tokio::test]
async fn on_error_continue_lands_a_typed_failed_entry_and_all_failing_fails() {
    let checkpoints = Arc::new(MemoryChildCheckpoints::default());
    let runs = run_counts(&["a", "b"]);
    let ledger = TestLedger::new();
    let node = ledger_node(
        definition_with(&[("a", "a"), ("b", "b")], 2, ""),
        pausing_runtime(&checkpoints, &runs, &[]),
        &ledger,
        1,
    );
    let context = input_context("root-continue", 0);
    let cards = parked(&node.execute(&context).await.unwrap());
    ledger.decide(&key_of(&cards, "card-a"), "reject", None);
    ledger.decide(&key_of(&cards, "card-b"), "approve", None);
    let output = node.execute(&context).await.unwrap();
    assert_eq!(
        output.updates["gathered"],
        json!([
            {"branch_id": "a", "node": "a", "status": "failed", "error": {
                "code": "graph.parallel.branch_blocked",
                "message_safe": "The branch was blocked.",
                "support_ref": output.updates["gathered"][0]["error"]["support_ref"],
            }},
            {"branch_id": "b", "node": "b", "status": "succeeded",
             "outputs": {"output": "b", "blocked": false}},
        ])
    );
    let support = output.updates["gathered"][0]["error"]["support_ref"]
        .as_str()
        .expect("support ref");
    assert!(support.len() == 32 && support.bytes().all(|b| b.is_ascii_hexdigit()));

    let checkpoints = Arc::new(MemoryChildCheckpoints::default());
    let runs = run_counts(&["a", "b"]);
    let node = ledger_node(
        definition_with(&[("a", "a"), ("b", "b")], 2, ""),
        pausing_runtime(&checkpoints, &runs, &[]),
        &ledger,
        2,
    );
    let cards = parked(&node.execute(&context).await.unwrap());
    for (key, _) in &cards {
        ledger.decide(key, "reject", None);
    }
    let error = node
        .execute(&context)
        .await
        .err()
        .expect("every branch failing fails the node");
    assert!(error.to_string().contains("branch 'a'"), "{error}");
}

#[tokio::test]
async fn freeze_rejects_a_competing_parent_before_any_child_is_admitted() {
    let checkpoints = Arc::new(MemoryChildCheckpoints::default());
    let thread = "root-freeze-race";
    let original = Checkpoint::new(
        thread,
        HashMap::from([("input".to_owned(), json!("original"))]),
        0,
        vec!["gather".to_owned()],
    );
    checkpoints.parent.save(&original).await.unwrap();
    let competing = Checkpoint::new(
        thread,
        HashMap::from([("input".to_owned(), json!("newer"))]),
        1,
        vec!["after".to_owned()],
    );
    checkpoints.parent.rows.lock().await.before_append = Some(competing.clone());
    let definition = definition(&[("a", "a"), ("b", "b")], 2);
    let node = DurableParallelNode::new(
        definition,
        runtime(
            Arc::clone(&checkpoints),
            HashMap::from([
                ("a".to_owned(), constant(json!({"output": "a"}))),
                ("b".to_owned(), constant(json!({"output": "b"}))),
            ]),
        ),
    );
    assert!(
        node.execute(&NodeContext::new(
            original.state,
            ExecutionConfig::new(thread),
            0
        ))
        .await
        .is_err()
    );
    assert!(checkpoints.issued_threads.lock().await.is_empty());
    assert!(
        checkpoint_equal(
            &checkpoints.parent.load(thread).await.unwrap().unwrap(),
            &competing
        )
        .unwrap()
    );
    assert_eq!(checkpoints.parent.list(thread).await.unwrap().len(), 2);
}

#[tokio::test]
async fn a_pause_receipt_cannot_attach_to_an_advanced_parent_frontier() {
    use super::parallel::ParallelBranchRuntime;
    let checkpoints = Arc::new(MemoryChildCheckpoints::default());
    let runs = Arc::new(HashMap::from([
        ("a".to_owned(), AtomicUsize::new(0)),
        ("b".to_owned(), AtomicUsize::new(0)),
    ]));
    let definition = definition(&[("a", "a"), ("b", "b")], 2);
    let runtime = pausing_runtime(&checkpoints, &runs, &[]);
    let context = NodeContext::new(
        HashMap::from([("input".to_owned(), json!("original"))]),
        ExecutionConfig::new("root-pause-race"),
        0,
    );
    let mut activation = ParallelActivation {
        root_thread_id: context.config.thread_id.clone(),
        node_id: "gather".to_owned(),
        step: 0,
        config_digest: definition.config_digest(),
    };
    for branch in runtime
        .prepare(&mut activation, &definition, &context)
        .await
        .unwrap()
    {
        let _ = runtime.invoke(&activation, branch, &context).await;
    }
    let competing = Checkpoint::new(
        &context.config.thread_id,
        HashMap::from([("input".to_owned(), json!("newer"))]),
        1,
        vec!["after".to_owned()],
    );
    checkpoints.parent.rows.lock().await.before_append = Some(competing.clone());
    // The pause is computed from the child receipts: no parent row yet.
    let paused = DurableParallelNode::new(definition, runtime.clone())
        .execute(&context)
        .await
        .expect("pause computed");
    assert!(paused.interrupt.is_some());
    // The parent learns of the pause only at the ADK pause save, which a
    // competing committed writer refuses.
    let frozen = checkpoints
        .parent
        .load(&context.config.thread_id)
        .await
        .unwrap()
        .unwrap();
    let pause_row = Checkpoint::new(
        &context.config.thread_id,
        frozen.state.clone(),
        frozen.step,
        frozen.pending_nodes.clone(),
    );
    assert!(
        runtime
            .parent_checkpointer()
            .save(&pause_row)
            .await
            .is_err()
    );
    let latest = checkpoints
        .parent
        .load(&context.config.thread_id)
        .await
        .unwrap()
        .unwrap();
    assert!(checkpoint_equal(&latest, &competing).unwrap());
    assert!(!latest.metadata.contains_key(OCCURRENCE));
    assert_eq!(runs["a"].load(Ordering::SeqCst), 1);
    assert_eq!(runs["b"].load(Ordering::SeqCst), 1);
}

/// Applying a decision writes nothing to the parent: the proof is the child
/// checkpoint. A changed business state is still refused before any child.
#[tokio::test]
async fn applying_decisions_writes_no_parent_row_and_changed_business_state_is_refused() {
    let checkpoints = Arc::new(MemoryChildCheckpoints::default());
    let runs = run_counts(&["a", "b"]);
    let definition = definition(&[("a", "a"), ("b", "b")], 2);
    let ledger = TestLedger::new();
    let node = ledger_node(
        definition,
        pausing_runtime(&checkpoints, &runs, &[]),
        &ledger,
        1,
    );
    let thread = "root-decision-race";
    let state = HashMap::from([
        ("input".to_owned(), json!("original")),
        ("unmapped".to_owned(), json!("preserved")),
    ]);
    let context = NodeContext::new(state.clone(), ExecutionConfig::new(thread), 0);
    let cards = parked(&node.execute(&context).await.unwrap());
    let original = checkpoints.parent.load(thread).await.unwrap().unwrap();
    let rows = checkpoints.parent.list(thread).await.unwrap().len();
    ledger.decide(&key_of(&cards, "card-a"), "approve", None);

    let mut missing_business = state.clone();
    missing_business.remove("unmapped");
    assert!(
        node.execute(&NodeContext::new(
            missing_business,
            ExecutionConfig::new(thread),
            0
        ))
        .await
        .is_err()
    );
    assert_eq!(runs_of(&runs, "a"), 1);

    let mut competing = original.clone();
    competing
        .metadata
        .insert("unrelated_receipt".to_owned(), json!("newer"));
    checkpoints.parent.rows.lock().await.before_append = Some(competing);
    let parked_b = parked(&node.execute(&context).await.unwrap());
    assert_eq!(parked_b, vec![cards[1].clone()]);
    assert_eq!(runs_of(&runs, "a"), 2);
    assert_eq!(
        checkpoints.parent.list(thread).await.unwrap().len(),
        rows,
        "a decision wrote a parent row"
    );
    assert!(
        checkpoint_equal(
            &checkpoints.parent.load(thread).await.unwrap().unwrap(),
            &original
        )
        .unwrap()
    );
    assert!(checkpoints.parent.rows.lock().await.before_append.is_some());
}

#[tokio::test]
async fn immutable_pre_freeze_replay_stays_unchanged_and_does_not_replace_latest() {
    use super::parallel::ParallelBranchRuntime;
    let checkpoints = Arc::new(MemoryChildCheckpoints::default());
    let thread = "root-immutable-replay";
    let original = Checkpoint::new(
        thread,
        HashMap::from([("input".to_owned(), json!("original"))]),
        0,
        vec!["gather".to_owned()],
    );
    checkpoints.parent.save(&original).await.unwrap();
    let definition = definition(&[("a", "a"), ("b", "b")], 2);
    let runtime = runtime(
        Arc::clone(&checkpoints),
        HashMap::from([
            ("a".to_owned(), constant(json!({"output": "a"}))),
            ("b".to_owned(), constant(json!({"output": "b"}))),
        ]),
    );
    let context = NodeContext::new(original.state.clone(), ExecutionConfig::new(thread), 0);
    let mut activation = ParallelActivation {
        root_thread_id: thread.to_owned(),
        node_id: "gather".to_owned(),
        step: 0,
        config_digest: definition.config_digest(),
    };
    runtime
        .prepare(&mut activation, &definition, &context)
        .await
        .unwrap();
    let frozen = checkpoints.parent.load(thread).await.unwrap().unwrap();
    assert_ne!(frozen.checkpoint_id, original.checkpoint_id);
    assert_eq!(
        runtime.parent_checkpointer().save(&original).await.unwrap(),
        original.checkpoint_id
    );
    assert!(
        checkpoint_equal(
            &checkpoints
                .parent
                .load_by_id(&original.checkpoint_id)
                .await
                .unwrap()
                .unwrap(),
            &original
        )
        .unwrap()
    );
    assert!(
        checkpoint_equal(
            &checkpoints.parent.load(thread).await.unwrap().unwrap(),
            &frozen
        )
        .unwrap()
    );
    let mut conflicting = original;
    conflicting
        .state
        .insert("input".to_owned(), json!("changed"));
    assert!(
        runtime
            .parent_checkpointer()
            .save(&conflicting)
            .await
            .is_err()
    );
    assert_eq!(checkpoints.parent.list(thread).await.unwrap().len(), 2);
}

struct DropObserver(Arc<AtomicUsize>);

impl Drop for DropObserver {
    fn drop(&mut self) {
        self.0.fetch_add(1, Ordering::SeqCst);
    }
}

#[tokio::test(start_paused = true)]
async fn deadline_stops_admission_and_owned_future_cleanup_is_bounded() {
    let entered = Arc::new(AtomicUsize::new(0));
    let dropped = Arc::new(AtomicUsize::new(0));
    let behavior: Behavior = {
        let entered = Arc::clone(&entered);
        let dropped = Arc::clone(&dropped);
        Arc::new(move || {
            let entered = Arc::clone(&entered);
            let dropped = Arc::clone(&dropped);
            Box::pin(async move {
                entered.fetch_add(1, Ordering::SeqCst);
                let _observer = DropObserver(dropped);
                std::future::pending::<Result<Value, GraphError>>().await
            })
        })
    };
    let runtime = runtime(
        Arc::new(MemoryChildCheckpoints::default()),
        HashMap::from([
            ("a".to_owned(), Arc::clone(&behavior)),
            ("b".to_owned(), Arc::clone(&behavior)),
            ("later".to_owned(), behavior),
        ]),
    );
    let node = DurableParallelNode::new(
        definition(&[("a", "a"), ("b", "b"), ("later", "later")], 2),
        runtime,
    )
    .with_deadline(tokio::time::Instant::now() + Duration::from_secs(1));
    let error = node
        .execute(&NodeContext::new(
            State::new(),
            ExecutionConfig::new("root-deadline"),
            0,
        ))
        .await
        .err()
        .expect("parallel deadline error");
    // Running children at the deadline: stop them within the bound and fail typed.
    assert!(
        error
            .to_string()
            .contains("graph.parallel.deadline_exceeded"),
        "{error}"
    );
    assert_eq!(entered.load(Ordering::SeqCst), 2);
    assert_eq!(dropped.load(Ordering::SeqCst), 2);
}

#[test]
fn unadmitted_branch_is_rejected_before_the_parent_graph_runs() {
    let definition = definition(&[("a", "safe"), ("b", "pause")], 2);
    let checkpoint_factory: Arc<dyn ParallelChildCheckpointerFactory> =
        Arc::new(MemoryChildCheckpoints::default());
    let graph_factory: Arc<dyn ParallelBranchGraphFactory> = Arc::new(TestBranchGraphs {
        behaviors: HashMap::from([
            ("safe".to_owned(), constant(json!({"ok": true}))),
            ("pause".to_owned(), constant(json!({"unused": true}))),
        ]),
        rejected_target: Some("pause".to_owned()),
    });
    let runtime = Arc::new(AdkParallelBranchRuntime::new(
        checkpoint_factory,
        graph_factory,
        Arc::new(ParallelOccurrenceCheckpointer::new(Arc::new(
            AtomicParentCheckpoints::default(),
        ))),
    ));

    assert!(
        StateGraph::with_channels(&["gathered"])
            .add_node(DurableParallelNode::new(definition, runtime))
            .add_edge(START, "gather")
            .add_edge("gather", adk_rust::graph::END)
            .compile()
            .is_err()
    );
}

#[tokio::test]
async fn new_parallel_revision_consumes_previous_graph_call_marker_and_retains_receipt_bytes() {
    use super::parallel::ParallelBranchRuntime;
    use crate::agents::pipeline::scope_receipts::{
        GRAPH_CALL_RECEIPTS_METADATA_KEY, GRAPH_CALL_REVISION_METADATA_KEY,
    };
    let checkpoints = Arc::new(MemoryChildCheckpoints::default());
    let definition = definition(&[("a", "a"), ("b", "b")], 2);
    let runs = Arc::new(HashMap::from([
        ("a".to_owned(), AtomicUsize::new(0)),
        ("b".to_owned(), AtomicUsize::new(0)),
    ]));
    let runtime = pausing_runtime(&checkpoints, &runs, &[]);
    let state = State::from([("input".to_owned(), json!("original"))]);
    let mut previous = adk_rust::graph::Checkpoint::new(
        "receipt-marker",
        state.clone(),
        0,
        vec!["gather".to_owned()],
    );
    let original = json!({"retained": "opaque exact original bytes"});
    previous.metadata.insert(
        GRAPH_CALL_RECEIPTS_METADATA_KEY.to_owned(),
        original.clone(),
    );
    previous.metadata.insert(
        GRAPH_CALL_REVISION_METADATA_KEY.to_owned(),
        json!({"parent": "old-frontier"}),
    );
    checkpoints.parent.save(&previous).await.unwrap();
    let mut activation = ParallelActivation {
        root_thread_id: "receipt-marker".to_owned(),
        node_id: "gather".to_owned(),
        step: 0,
        config_digest: definition.config_digest(),
    };
    runtime
        .prepare(
            &mut activation,
            &definition,
            &NodeContext::new(state, ExecutionConfig::new("receipt-marker"), 0),
        )
        .await
        .unwrap();
    let latest = checkpoints
        .parent
        .load("receipt-marker")
        .await
        .unwrap()
        .unwrap();
    assert_eq!(latest.metadata[GRAPH_CALL_RECEIPTS_METADATA_KEY], original);
    assert!(
        !latest
            .metadata
            .contains_key(GRAPH_CALL_REVISION_METADATA_KEY)
    );
    assert_ne!(latest.checkpoint_id, previous.checkpoint_id);
    assert_eq!(
        checkpoints
            .parent
            .load_by_id(&previous.checkpoint_id)
            .await
            .unwrap()
            .unwrap()
            .metadata,
        previous.metadata
    );
}

#[test]
fn structural_input_bounds_precede_recursive_hash() {
    let mut deep = Value::Null;
    for _ in 0..256 {
        deep = Value::Array(vec![deep]);
    }
    assert!(projected_input_digest(&State::from([("input".into(), deep)])).is_err());
}

#[tokio::test]
async fn structural_parent_rejection_precedes_checkpoint_mint_and_child_effects() {
    let checkpoints = Arc::new(MemoryChildCheckpoints::default());
    let runs = Arc::new(AtomicUsize::new(0));
    let make_behavior = || {
        let runs = Arc::clone(&runs);
        Arc::new(move || {
            let runs = Arc::clone(&runs);
            Box::pin(async move {
                runs.fetch_add(1, Ordering::SeqCst);
                Ok(json!({"value": "should not run"}))
            }) as BehaviorFuture
        }) as Behavior
    };
    let node = DurableParallelNode::new(
        definition(&[("a", "first"), ("b", "second")], 2),
        runtime(
            Arc::clone(&checkpoints),
            HashMap::from([
                ("first".into(), make_behavior()),
                ("second".into(), make_behavior()),
            ]),
        ),
    );
    let mut deep = Value::Null;
    for _ in 0..256 {
        deep = Value::Array(vec![deep]);
    }
    let context = NodeContext::new(
        State::from([("input".into(), deep)]),
        ExecutionConfig::new("deep-parent"),
        0,
    );
    assert!(node.execute_outcome(&context).await.is_err());
    assert_eq!(runs.load(Ordering::SeqCst), 0);
    assert!(checkpoints.issued_threads.lock().await.is_empty());
    assert!(
        checkpoints
            .parent
            .list("deep-parent")
            .await
            .unwrap()
            .is_empty()
    );
}

#[tokio::test]
async fn a_legacy_occurrence_without_frozen_child_identity_is_refused_by_type() {
    for format in ["v1", "v2"] {
        legacy_occurrence_is_refused(format).await;
    }
}

/// v1 froze no child identity; v2 kept a complete-set decision record.
async fn legacy_occurrence_is_refused(format: &str) {
    let checkpoints = Arc::new(MemoryChildCheckpoints::default());
    let thread = "root-legacy";
    let state = HashMap::from([("input".to_owned(), json!("original"))]);
    let mut legacy = Checkpoint::new(thread, state.clone(), 3, vec!["gather".to_owned()]);
    legacy.metadata.insert(
        format!("elitea.graph.parallel.occurrence.{format}"),
        json!({"activation": {}, "branches": []}),
    );
    checkpoints.parent.save(&legacy).await.unwrap();
    let node = DurableParallelNode::new(
        definition(&[("a", "a"), ("b", "b")], 2),
        runtime(
            Arc::clone(&checkpoints),
            HashMap::from([
                ("a".to_owned(), constant(json!({"output": "a"}))),
                ("b".to_owned(), constant(json!({"output": "b"}))),
            ]),
        ),
    );
    let error = node
        .execute(&NodeContext::new(state, ExecutionConfig::new(thread), 3))
        .await
        .err()
        .expect("a legacy occurrence is refused");
    assert!(
        error
            .to_string()
            .contains("graph.parallel.unsupported_occurrence")
    );
    assert!(checkpoints.issued_threads.lock().await.is_empty());
    assert_eq!(checkpoints.parent.list(thread).await.unwrap().len(), 1);
}

fn pending_behavior(
    entered: &Arc<AtomicUsize>,
    dropped: &Arc<AtomicUsize>,
    announce: Option<mpsc::UnboundedSender<()>>,
) -> Behavior {
    let entered = Arc::clone(entered);
    let dropped = Arc::clone(dropped);
    Arc::new(move || {
        let entered = Arc::clone(&entered);
        let dropped = Arc::clone(&dropped);
        let announce = announce.clone();
        Box::pin(async move {
            entered.fetch_add(1, Ordering::SeqCst);
            let _observer = DropObserver(dropped);
            if let Some(announce) = announce {
                let _ = announce.send(());
            }
            std::future::pending::<Result<Value, GraphError>>().await
        })
    })
}

fn pending_runtime(behavior: &Behavior, names: &[&str]) -> Arc<AdkParallelBranchRuntime> {
    runtime(
        Arc::new(MemoryChildCheckpoints::default()),
        names
            .iter()
            .map(|name| ((*name).to_owned(), Arc::clone(behavior)))
            .collect(),
    )
}

fn root_context(thread: &str) -> NodeContext {
    NodeContext::new(State::new(), ExecutionConfig::new(thread), 0)
}

async fn failed_receipt_count(checkpoints: &MemoryChildCheckpoints) -> usize {
    let stores = checkpoints.stores.lock().await;
    let mut failed = 0;
    for (thread, store) in stores.iter() {
        for saved in store.list(thread).await.expect("list child checkpoints") {
            let metadata = serde_json::to_string(&saved.metadata).expect("encode metadata");
            if metadata.contains("\"status\":\"failed\"") {
                failed += 1;
            }
        }
    }
    failed
}

#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn latch_cancel_stops_inflight_branches_within_the_cleanup_bound_and_admits_nothing_new() {
    let entered = Arc::new(AtomicUsize::new(0));
    let dropped = Arc::new(AtomicUsize::new(0));
    let (announce, mut entries) = mpsc::unbounded_channel();
    let behavior = pending_behavior(&entered, &dropped, Some(announce));
    let latch = Arc::new(FanoutCancellation::new());
    let node = DurableParallelNode::new(
        definition(&[("a", "a"), ("b", "b"), ("later", "later")], 2),
        pending_runtime(&behavior, &["a", "b", "later"]),
    )
    .with_cancellation(Arc::clone(&latch));
    let run = tokio::spawn(async move { node.execute(&root_context("root-latch")).await });
    entries.recv().await.expect("first branch entered");
    entries.recv().await.expect("second branch entered");

    let fired = std::time::Instant::now();
    latch.cancel();
    let error = tokio::time::timeout(Duration::from_secs(5), run)
        .await
        .expect("the latch must stop the node")
        .expect("node task")
        .err()
        .expect("parallel cancellation error");
    let elapsed = fired.elapsed();
    eprintln!("latch_cancel_latency: {elapsed:?}");

    // Non-cooperative children are dropped at the shared cleanup bound.
    assert!(
        error.to_string().contains("graph.parallel.cancel"),
        "{error}"
    );
    assert!(
        elapsed <= CANCEL_CLEANUP + Duration::from_millis(150),
        "{elapsed:?}"
    );
    assert_eq!(dropped.load(Ordering::SeqCst), 2);
    assert_eq!(
        entered.load(Ordering::SeqCst),
        2,
        "a branch was admitted after cancel"
    );
}

#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn deadline_stops_non_cooperative_branches_at_the_deadline_plus_cleanup() {
    let entered = Arc::new(AtomicUsize::new(0));
    let dropped = Arc::new(AtomicUsize::new(0));
    let behavior = pending_behavior(&entered, &dropped, None);
    let started = std::time::Instant::now();
    let node = DurableParallelNode::new(
        definition(&[("a", "a"), ("b", "b"), ("later", "later")], 2),
        pending_runtime(&behavior, &["a", "b", "later"]),
    )
    .with_deadline(tokio::time::Instant::now() + Duration::from_millis(50));
    let result = tokio::time::timeout(
        Duration::from_secs(5),
        node.execute(&root_context("root-deadline-live")),
    )
    .await
    .expect("the deadline must stop the node");
    let elapsed = started.elapsed();
    eprintln!("deadline_honored: {elapsed:?}");

    assert!(result.err().is_some_and(|error| {
        error
            .to_string()
            .contains("graph.parallel.deadline_exceeded")
    }));
    assert!(elapsed >= Duration::from_millis(50), "{elapsed:?}");
    assert!(
        elapsed <= Duration::from_millis(50) + CANCEL_CLEANUP + Duration::from_millis(150),
        "{elapsed:?}"
    );
    assert_eq!(entered.load(Ordering::SeqCst), 2);
    assert_eq!(dropped.load(Ordering::SeqCst), 2);
}

#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn a_lease_lost_branch_is_a_control_stop_not_a_recorded_failure() {
    let checkpoints = Arc::new(MemoryChildCheckpoints {
        lease_lost_branch: Some("a"),
        ..MemoryChildCheckpoints::default()
    });
    let second_runs = Arc::new(AtomicUsize::new(0));
    let counter = Arc::clone(&second_runs);
    let second: Behavior = Arc::new(move || {
        let counter = Arc::clone(&counter);
        Box::pin(async move {
            counter.fetch_add(1, Ordering::SeqCst);
            Ok(json!({"value": "second"}))
        })
    });
    let node = DurableParallelNode::new(
        definition(&[("a", "first"), ("b", "second")], 1),
        runtime(
            Arc::clone(&checkpoints),
            HashMap::from([
                ("first".to_owned(), constant(json!({"value": "first"}))),
                ("second".to_owned(), second),
            ]),
        ),
    );
    let error = node
        .execute(&root_context("root-lease"))
        .await
        .err()
        .expect("lease loss must stop the node");

    assert!(
        matches!(&error, GraphError::CheckpointError(message) if message == LEASE_LOST),
        "{error}"
    );
    assert!(!error.to_string().contains("branch_failed"));
    assert_eq!(
        second_runs.load(Ordering::SeqCst),
        0,
        "admitted after lease loss"
    );
    assert_eq!(failed_receipt_count(&checkpoints).await, 0);
    let parent = checkpoints
        .parent
        .load("root-lease")
        .await
        .expect("load parent")
        .expect("frozen occurrence");
    let occurrence = parent.metadata[OCCURRENCE]
        .as_object()
        .expect("frozen occurrence");
    // The parent freezes children only: no cards, decisions or denials.
    assert_eq!(
        occurrence
            .keys()
            .map(String::as_str)
            .collect::<BTreeSet<_>>(),
        BTreeSet::from(["activation", "branches", "child_threads", "origin"])
    );
}

struct CancellableInvocation {
    cancelled: Arc<std::sync::atomic::AtomicBool>,
    content: adk_rust::Content,
    config: adk_rust::RunConfig,
}

#[async_trait]
#[allow(clippy::unnecessary_literal_bound)] // Fixed by the foreign trait signature.
impl adk_rust::ReadonlyContext for CancellableInvocation {
    fn invocation_id(&self) -> &str {
        "inv"
    }
    fn agent_name(&self) -> &str {
        "agent"
    }
    fn user_id(&self) -> &str {
        "user"
    }
    fn app_name(&self) -> &str {
        "app"
    }
    fn session_id(&self) -> &str {
        "session"
    }
    fn branch(&self) -> &str {
        ""
    }
    fn user_content(&self) -> &adk_rust::Content {
        &self.content
    }
}

#[async_trait]
impl adk_rust::CallbackContext for CancellableInvocation {
    fn artifacts(&self) -> Option<Arc<dyn adk_rust::Artifacts>> {
        None
    }
}

struct NoSession;

#[allow(clippy::unnecessary_literal_bound)] // Fixed by the foreign trait signature.
impl adk_rust::Session for NoSession {
    fn id(&self) -> &str {
        "session"
    }
    fn app_name(&self) -> &str {
        "app"
    }
    fn user_id(&self) -> &str {
        "user"
    }
    fn state(&self) -> &dyn adk_rust::State {
        &NoState
    }
    fn conversation_history(&self) -> Vec<adk_rust::Content> {
        Vec::new()
    }
}

struct NoState;

impl adk_rust::State for NoState {
    fn get(&self, _key: &str) -> Option<Value> {
        None
    }
    fn set(&mut self, _key: String, _value: Value) {}
    fn all(&self) -> HashMap<String, Value> {
        HashMap::new()
    }
}

#[async_trait]
impl adk_rust::InvocationContext for CancellableInvocation {
    fn agent(&self) -> Arc<dyn adk_rust::Agent> {
        unreachable!("the parallel runner never reads the parent agent")
    }
    fn memory(&self) -> Option<Arc<dyn adk_rust::Memory>> {
        None
    }
    fn session(&self) -> &dyn adk_rust::Session {
        &NoSession
    }
    fn run_config(&self) -> &adk_rust::RunConfig {
        &self.config
    }
    fn end_invocation(&self) {}
    fn ended(&self) -> bool {
        false
    }
    fn is_cancelled(&self) -> bool {
        self.cancelled.load(Ordering::SeqCst)
    }
}

#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn a_branch_failing_after_cancellation_writes_no_failed_receipt() {
    let cancelled = Arc::new(std::sync::atomic::AtomicBool::new(false));
    let flag = Arc::clone(&cancelled);
    let failing: Behavior = Arc::new(move || {
        let flag = Arc::clone(&flag);
        Box::pin(async move {
            flag.store(true, Ordering::SeqCst);
            Err(GraphError::NodeExecutionFailed {
                node: "run".to_owned(),
                message: "interrupted by cancellation".to_owned(),
            })
        })
    });
    let checkpoints = Arc::new(MemoryChildCheckpoints::default());
    let node = DurableParallelNode::new(
        definition(&[("a", "a"), ("b", "b")], 1),
        runtime(
            Arc::clone(&checkpoints),
            HashMap::from([
                ("a".to_owned(), failing),
                ("b".to_owned(), constant(json!({"value": "b"}))),
            ]),
        ),
    );
    let parent: Arc<dyn adk_rust::InvocationContext> = Arc::new(CancellableInvocation {
        cancelled,
        content: adk_rust::Content::new("user"),
        config: adk_rust::RunConfig::default(),
    });
    let config = ExecutionConfig::new("root-cancelled-failure").with_parent_context(parent);
    let error = node
        .execute(&NodeContext::new(State::new(), config, 0))
        .await
        .err()
        .expect("cancelled parallel node");

    assert!(
        error.to_string().contains("graph.parallel.cancelled"),
        "{error}"
    );
    assert_eq!(failed_receipt_count(&checkpoints).await, 0);
}

fn pausing_parent_graph(
    definition: ParallelNodeDefinition,
    runtime: Arc<AdkParallelBranchRuntime>,
    ledger: &Arc<TestLedger>,
) -> CompiledGraph {
    let parent = runtime.parent_checkpointer();
    StateGraph::with_channels(&["input", "gathered"])
        .add_node(DurableParallelNode::new(definition, runtime).with_decisions(ledger.client(1)))
        .add_edge(START, "gather")
        .add_edge("gather", adk_rust::graph::END)
        .compile()
        .expect("compile parent graph")
        .with_checkpointer_arc(parent)
}

#[tokio::test]
async fn each_activation_visit_appends_at_most_two_parent_rows() {
    use super::parallel::MAX_PARENT_ROWS_PER_VISIT;
    // Completion visit: freeze + join.
    let checkpoints = Arc::new(MemoryChildCheckpoints::default());
    let graph = parent_graph(
        definition(&[("a", "first"), ("b", "second")], 2),
        runtime(
            Arc::clone(&checkpoints),
            HashMap::from([
                ("first".to_owned(), constant(json!({"value": 1}))),
                ("second".to_owned(), constant(json!({"value": 2}))),
            ]),
        ),
    );
    graph
        .invoke(
            HashMap::from([("input".to_owned(), json!("x"))]),
            ExecutionConfig::new("root-budget-join"),
        )
        .await
        .unwrap();
    assert_eq!(
        checkpoints
            .parent
            .list("root-budget-join")
            .await
            .unwrap()
            .len(),
        MAX_PARENT_ROWS_PER_VISIT
    );

    // Pause visit: freeze + the ADK pause row. Cards live on the children.
    let checkpoints = Arc::new(MemoryChildCheckpoints::default());
    let runs = run_counts(&["a", "b"]);
    let definition = definition(&[("a", "a"), ("b", "b")], 2);
    let ledger = TestLedger::new();
    let graph = pausing_parent_graph(
        definition.clone(),
        pausing_runtime(&checkpoints, &runs, &[]),
        &ledger,
    );
    let thread = "root-budget-pause";
    let GraphError::Interrupted(interrupted) = graph
        .invoke(
            HashMap::from([("input".to_owned(), json!("original"))]),
            ExecutionConfig::new(thread),
        )
        .await
        .unwrap_err()
    else {
        panic!("expected the aggregate pause");
    };
    let rows = checkpoints.parent.list(thread).await.unwrap();
    assert_eq!(rows.len(), MAX_PARENT_ROWS_PER_VISIT);
    assert_eq!(rows[1].metadata[OCCURRENCE], rows[0].metadata[OCCURRENCE]);

    // Resume visit: per-card decisions are applied on the children, then the
    // join is the visit's only parent row.
    let adk_rust::graph::interrupt::Interrupt::Dynamic {
        data: Some(pause), ..
    } = interrupted.interrupt
    else {
        panic!("expected the parked payload");
    };
    for (key, _) in parked_data(&pause).iter().rev() {
        ledger.decide(key, "approve", None);
    }
    let result = graph
        .invoke(
            HashMap::new(),
            ExecutionConfig::new(thread).with_resume_from(&interrupted.checkpoint_id),
        )
        .await
        .unwrap();
    assert_eq!(result["gathered"].as_array().map(Vec::len), Some(2));
    assert_eq!(
        checkpoints.parent.list(thread).await.unwrap().len(),
        MAX_PARENT_ROWS_PER_VISIT + 1
    );
}

#[tokio::test]
async fn a_third_parent_row_in_one_visit_is_refused_with_a_typed_error() {
    use super::parallel::ParallelBranchRuntime;
    let checkpoints = Arc::new(MemoryChildCheckpoints::default());
    let runs = Arc::new(HashMap::from([
        ("a".to_owned(), AtomicUsize::new(0)),
        ("b".to_owned(), AtomicUsize::new(0)),
    ]));
    let definition = definition(&[("a", "a"), ("b", "b")], 2);
    let runtime = pausing_runtime(&checkpoints, &runs, &[]);
    let thread = "root-budget-refusal";
    let context = NodeContext::new(
        HashMap::from([("input".to_owned(), json!("original"))]),
        ExecutionConfig::new(thread),
        0,
    );
    let mut activation = ParallelActivation {
        root_thread_id: thread.to_owned(),
        node_id: "gather".to_owned(),
        step: 0,
        config_digest: definition.config_digest(),
    };
    // Row 1: the frozen occurrence. Row 2: the ADK pause row.
    runtime
        .prepare(&mut activation, &definition, &context)
        .await
        .unwrap();
    let latest = checkpoints.parent.load(thread).await.unwrap().unwrap();
    let pause_row = Checkpoint::new(
        thread,
        latest.state.clone(),
        latest.step,
        latest.pending_nodes.clone(),
    );
    runtime
        .parent_checkpointer()
        .save(&pause_row)
        .await
        .unwrap();
    let latest = checkpoints.parent.load(thread).await.unwrap().unwrap();
    let third = Checkpoint::new(
        thread,
        latest.state.clone(),
        latest.step,
        latest.pending_nodes.clone(),
    );
    let error = runtime
        .parent_checkpointer()
        .save(&third)
        .await
        .unwrap_err();
    assert!(
        error
            .to_string()
            .contains("graph.parallel.parent_write_budget_exhausted"),
        "{error}"
    );
    assert_eq!(checkpoints.parent.list(thread).await.unwrap().len(), 2);
}

#[tokio::test]
async fn a_completed_child_is_proven_by_its_fenced_save_without_a_reload() {
    let checkpoints = Arc::new(MemoryChildCheckpoints::default());
    let graph = parent_graph(
        definition(&[("a", "first"), ("b", "second")], 2),
        runtime(
            Arc::clone(&checkpoints),
            HashMap::from([
                ("first".to_owned(), constant(json!({"value": 1}))),
                ("second".to_owned(), constant(json!({"value": 2}))),
            ]),
        ),
    );
    graph
        .invoke(
            HashMap::from([("input".to_owned(), json!("x"))]),
            ExecutionConfig::new("root-child-reads"),
        )
        .await
        .unwrap();
    // Per fresh child: the preparation read (batched by a durable store) and
    // one fenced ADK start probe. ADK's second empty probe and the former
    // post-completion verify reload cost nothing.
    assert_eq!(checkpoints.child_loads.load(Ordering::SeqCst), 2 * 2);
}

/// `{"value":"<n x>"}` serializes to n + 12 bytes.
fn branch_value(bytes: usize) -> Value {
    json!({ "value": "x".repeat(bytes - 12) })
}

#[tokio::test]
async fn a_branch_result_at_its_bound_lands_and_one_byte_more_fails_typed() {
    for (bytes, fits) in [
        (MAX_CHILD_RESULT_BYTES, true),
        (MAX_CHILD_RESULT_BYTES + 1, false),
    ] {
        let node = DurableParallelNode::new(
            definition(&[("big", "big"), ("small", "small")], 2),
            runtime(
                Arc::new(MemoryChildCheckpoints::default()),
                HashMap::from([
                    ("big".to_owned(), constant(branch_value(bytes))),
                    ("small".to_owned(), constant(json!({"value": 1}))),
                ]),
            ),
        );
        let result = node.execute(&root_context("root-result-bound")).await;
        if fits {
            let output = result.expect("a result at its bound lands");
            assert_eq!(
                serde_json::to_vec(&output.updates["gathered"][0]["outputs"])
                    .unwrap()
                    .len(),
                MAX_CHILD_RESULT_BYTES
            );
        } else {
            let error = result.err().expect("one byte more fails the branch");
            assert!(error.to_string().contains("branch 'big'"), "{error}");
        }
    }
}

/// Sixteen branches whose joined collection is exactly `total` bytes.
fn exact_join(total: usize) -> (ParallelNodeDefinition, HashMap<String, Behavior>) {
    let branches = (0..16)
        .map(|index| (format!("b{index:02}"), format!("n{index:02}")))
        .collect::<Vec<_>>();
    let empty = (0..16)
        .map(|index| {
            json!({
                "branch_id": format!("b{index:02}"),
                "node": format!("n{index:02}"),
                "status": "succeeded",
                "outputs": {"value": ""},
            })
        })
        .collect::<Vec<_>>();
    let padding = total - serde_json::to_vec(&empty).unwrap().len();
    let behaviors = (0..16)
        .map(|index| {
            let share = padding / 16 + usize::from(index < padding % 16);
            (
                format!("n{index:02}"),
                constant(json!({ "value": "x".repeat(share) })),
            )
        })
        .collect();
    let refs = branches
        .iter()
        .map(|(branch, node)| (branch.as_str(), node.as_str()))
        .collect::<Vec<_>>();
    (definition(&refs, 8), behaviors)
}

#[tokio::test]
async fn a_joined_result_at_eight_mib_lands_and_one_byte_more_fails_typed() {
    let (definition, behaviors) = exact_join(MAX_JOINED_RESULT_BYTES);
    let output = DurableParallelNode::new(
        definition,
        runtime(Arc::new(MemoryChildCheckpoints::default()), behaviors),
    )
    .execute(&root_context("root-join-bound"))
    .await
    .expect("an 8 MiB join lands");
    assert_eq!(
        serde_json::to_vec(&output.updates["gathered"])
            .unwrap()
            .len(),
        MAX_JOINED_RESULT_BYTES
    );
    let (definition, behaviors) = exact_join(MAX_JOINED_RESULT_BYTES + 1);
    let error = DurableParallelNode::new(
        definition,
        runtime(Arc::new(MemoryChildCheckpoints::default()), behaviors),
    )
    .execute(&root_context("root-join-over"))
    .await
    .err()
    .expect("one byte more is refused");
    assert!(
        error
            .to_string()
            .contains("graph.fanout.joined_result_too_large"),
        "{error}"
    );
}

fn two_constant_branches(policy: &str, first: Value, second: Value) -> DurableParallelNode {
    DurableParallelNode::new(
        definition_with(&[("a", "one"), ("b", "two")], 2, policy),
        runtime(
            Arc::new(MemoryChildCheckpoints::default()),
            HashMap::from([
                ("one".to_owned(), constant(first)),
                ("two".to_owned(), constant(second)),
            ]),
        ),
    )
}

fn held_context(thread: &str, gathered: Value) -> NodeContext {
    NodeContext::new(
        HashMap::from([("gathered".to_owned(), gathered)]),
        ExecutionConfig::new(thread),
        0,
    )
}

#[tokio::test]
async fn join_append_keyed_and_merge_land_through_the_node() {
    // Append without a reducer: the ordered collection after what the key held.
    let output = two_constant_branches("", json!({"x": 1}), json!({"y": 2}))
        .execute(&held_context("root-append", json!([{"seed": true}])))
        .await
        .unwrap();
    assert_eq!(
        output.updates["gathered"],
        json!([
            {"seed": true},
            {"branch_id": "a", "node": "one", "status": "succeeded", "outputs": {"x": 1}},
            {"branch_id": "b", "node": "two", "status": "succeeded", "outputs": {"y": 2}},
        ])
    );
    // Keyed: one object by branch id.
    let output = two_constant_branches("join: keyed\n", json!({"x": 1}), json!({"y": 2}))
        .execute(&held_context("root-keyed", json!({})))
        .await
        .unwrap();
    assert_eq!(
        output.updates["gathered"],
        json!({
            "a": {"branch_id": "a", "node": "one", "status": "succeeded", "outputs": {"x": 1}},
            "b": {"branch_id": "b", "node": "two", "status": "succeeded", "outputs": {"y": 2}},
        })
    );
    // Merge (requires stop): one object of every branch's outputs.
    let output = two_constant_branches(
        "on_error: stop\njoin: merge\n",
        json!({"x": 1}),
        json!({"y": 2}),
    )
    .execute(&held_context("root-merge", json!({})))
    .await
    .unwrap();
    assert_eq!(output.updates["gathered"], json!({"x": 1, "y": 2}));
    // Two branches writing one key conflict: typed, never last-write-wins.
    let error = two_constant_branches(
        "on_error: stop\njoin: merge\n",
        json!({"x": 1}),
        json!({"x": 2}),
    )
    .execute(&held_context("root-merge-conflict", json!({})))
    .await
    .err()
    .expect("a merge conflict is refused");
    assert!(
        error
            .to_string()
            .contains("graph.fanout_join_merge_conflict"),
        "{error}"
    );
}

#[tokio::test]
async fn a_declared_append_reducer_on_the_output_key_receives_only_the_new_entries() {
    use super::state_reducers::StateReducer;
    let node = two_constant_branches("", json!({"x": 1}), json!({"y": 2}))
        .with_output_reducer(Some(StateReducer::Append));
    let output = node
        .execute(&held_context("root-reducer", json!([{"seed": true}])))
        .await
        .unwrap();
    // The channel's own reducer appends; the node never repeats what it held.
    assert_eq!(output.updates["gathered"].as_array().map(Vec::len), Some(2));
    let mut schema = adk_rust::graph::StateSchema::new();
    schema.channels.insert(
        "gathered".to_owned(),
        adk_rust::graph::Channel::new("gathered")
            .with_default(json!([]))
            .with_reducer(StateReducer::Append.channel_reducer("gathered")),
    );
    let checkpoints = Arc::new(MemoryChildCheckpoints::default());
    let runtime = runtime(
        Arc::clone(&checkpoints),
        HashMap::from([
            ("one".to_owned(), constant(json!({"x": 1}))),
            ("two".to_owned(), constant(json!({"y": 2}))),
        ]),
    );
    let parent = runtime.parent_checkpointer();
    let graph = StateGraph::new(schema)
        .add_node(
            DurableParallelNode::new(
                definition_with(&[("a", "one"), ("b", "two")], 2, ""),
                runtime,
            )
            .with_output_reducer(Some(StateReducer::Append)),
        )
        .add_edge(START, "gather")
        .add_edge("gather", adk_rust::graph::END)
        .compile()
        .unwrap()
        .with_checkpointer_arc(parent);
    let state = graph
        .invoke(
            HashMap::from([("gathered".to_owned(), json!([{"seed": true}]))]),
            ExecutionConfig::new("root-reducer-graph"),
        )
        .await
        .unwrap();
    assert_eq!(
        state["gathered"].as_array().map(|entries| entries
            .iter()
            .map(|entry| entry.get("branch_id").cloned())
            .collect::<Vec<_>>()),
        Some(vec![None, Some(json!("a")), Some(json!("b"))])
    );
}

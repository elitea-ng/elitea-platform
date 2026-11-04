//! T-2A3: a Map node on the shared fan-out adapter, in memory. Join order,
//! the v1 and v2 landings, the limits, On Error `stop`, and per-item lineage.
//! Restore across a new execution and lease loss are proven on real
//! PostgreSQL (`postgres_checkpointer_tests/fanout_lineage.rs`), the child-id
//! domain in `parallel_map_identity_tests.rs`, and the compile-time refusals
//! in `parallel_compiler_map_tests.rs` and `map_compiler_tests.rs`.
#![allow(clippy::unwrap_used, clippy::expect_used)]

use std::collections::HashMap;
use std::future::Future;
use std::pin::Pin;
use std::sync::Arc;
use std::sync::atomic::{AtomicUsize, Ordering};
use std::time::Duration;

use adk_rust::graph::checkpoint::{Checkpointer, MemoryCheckpointer};
use adk_rust::graph::{
    Checkpoint, CompiledGraph, ExecutionConfig, FunctionNode, GraphError, NodeOutput, START, State,
    StateGraph,
};
use async_trait::async_trait;
use elitea_agent_runtime::graph::fanout_budget::{MAX_CHILD_RESULT_BYTES, MAX_JOINED_RESULT_BYTES};
use serde_json::{Value, json};
use tokio::sync::{Mutex, Notify};

use super::ParallelBranchDefinition;
use super::compiler::{
    ParallelCompilerBinding, PipelineConfigurationError, PipelineDefinition, PipelineNodeRuntimes,
};
use super::fanout_source::{FanoutSource, MapSource};
use super::map_reduce::{
    MAX_ITEM_BYTES, MapDefinition, MapJoin, MapOnError, MapPolicy, MapReduction,
};
use super::parallel::ParallelCheckpointAuthority;
use super::parallel::{
    AdkParallelBranchRuntime, DurableParallelNode, ParallelActivation, ParallelBranchExecution,
    ParallelBranchGraphFactory, ParallelBranchPause, ParallelBranchTerminal,
    ParallelCheckpointAppender, ParallelChildCheckpoint, ParallelChildCheckpointerFactory,
    ParallelChildOrigin, ParallelDecision, ParallelOccurrenceCheckpointer, ParallelPauseCard,
};
use crate::agents::graph::application::{
    ApplicationExecutionError, PipelineApplicationResolver, PipelineApplicationSelection,
    ResolvedApplicationParticipant,
};

type ItemFuture = Pin<Box<dyn Future<Output = Result<Value, GraphError>> + Send>>;
/// The worker's result for one item, from its index and its frozen `entity`.
type ItemBehavior = Arc<dyn Fn(usize, Value) -> ItemFuture + Send + Sync>;

#[derive(Default)]
struct ParentRows(Mutex<Vec<Checkpoint>>);

fn same(left: &Checkpoint, right: &Checkpoint) -> bool {
    serde_json::to_value(left).ok() == serde_json::to_value(right).ok()
}

#[async_trait]
impl Checkpointer for ParentRows {
    async fn save(&self, checkpoint: &Checkpoint) -> Result<String, GraphError> {
        let mut rows = self.0.lock().await;
        if let Some(existing) = rows
            .iter()
            .find(|row| row.checkpoint_id == checkpoint.checkpoint_id)
        {
            return if same(existing, checkpoint) {
                Ok(existing.checkpoint_id.clone())
            } else {
                Err(GraphError::CheckpointError("immutable conflict".to_owned()))
            };
        }
        rows.push(checkpoint.clone());
        Ok(checkpoint.checkpoint_id.clone())
    }
    async fn load(&self, thread: &str) -> Result<Option<Checkpoint>, GraphError> {
        Ok(self
            .0
            .lock()
            .await
            .iter()
            .rev()
            .find(|row| row.thread_id == thread)
            .cloned())
    }
    async fn load_by_id(&self, id: &str) -> Result<Option<Checkpoint>, GraphError> {
        Ok(self
            .0
            .lock()
            .await
            .iter()
            .find(|row| row.checkpoint_id == id)
            .cloned())
    }
    async fn list(&self, thread: &str) -> Result<Vec<Checkpoint>, GraphError> {
        Ok(self
            .0
            .lock()
            .await
            .iter()
            .filter(|row| row.thread_id == thread)
            .cloned()
            .collect())
    }
    async fn delete(&self, thread: &str) -> Result<(), GraphError> {
        self.0.lock().await.retain(|row| row.thread_id != thread);
        Ok(())
    }
}

#[async_trait]
impl ParallelCheckpointAppender for ParentRows {
    async fn append_after(
        &self,
        expected: Option<&Checkpoint>,
        candidate: &Checkpoint,
    ) -> Result<String, GraphError> {
        let latest = self.load(&candidate.thread_id).await?;
        let matches = match (expected, latest.as_ref()) {
            (Some(expected), Some(latest)) => same(expected, latest),
            (None, None) => true,
            _ => false,
        };
        if !matches
            && latest.as_ref().map(|row| &row.checkpoint_id) != Some(&candidate.checkpoint_id)
        {
            return Err(GraphError::CheckpointError(
                "atomic expected-parent conflict".to_owned(),
            ));
        }
        self.save(candidate).await
    }
}

/// One memory store per derived child thread; records every thread issued.
#[derive(Default)]
struct ItemChildren {
    stores: Mutex<HashMap<String, Arc<MemoryCheckpointer>>>,
    issued: Mutex<Vec<String>>,
}

#[async_trait]
impl ParallelChildCheckpointerFactory for ItemChildren {
    fn child_origin(&self, _: &ParallelActivation) -> Result<ParallelChildOrigin, GraphError> {
        Ok(ParallelChildOrigin {
            execution_id: "map-execution".to_owned(),
            generation: 1,
        })
    }

    fn branch_thread_id(
        &self,
        activation: &ParallelActivation,
        branch: &ParallelBranchDefinition,
        ordinal: usize,
        input_digest: &[u8; 32],
        _origin: &ParallelChildOrigin,
    ) -> Result<String, GraphError> {
        let identity = activation
            .map
            .as_ref()
            .ok_or_else(|| GraphError::CheckpointError("a Map item needs its identity".into()))?;
        Ok(format!(
            "map:{}:{}:{}:{}:{ordinal}:{:?}:{input_digest:?}",
            activation.root_thread_id,
            activation.node_id,
            activation.step,
            branch.id(),
            identity.source_digest,
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
        let store = Arc::clone(
            self.stores
                .lock()
                .await
                .entry(thread_id.clone())
                .or_insert_with(|| Arc::new(MemoryCheckpointer::new())),
        );
        self.issued.lock().await.push(thread_id.clone());
        Ok(ParallelChildCheckpoint {
            admitted_threads: std::collections::BTreeSet::from([thread_id.clone()]),
            thread_id,
            checkpointer: store,
            node_recovery: None,
        })
    }
}

/// The Map worker: one node writing `item_result` (landed) and `scratch`
/// (a worker output the Map does not select).
struct ItemGraphs {
    behavior: ItemBehavior,
    runs: Arc<std::sync::Mutex<Vec<usize>>>,
    in_flight: Arc<AtomicUsize>,
    peak: Arc<AtomicUsize>,
}

#[async_trait]
impl ParallelBranchGraphFactory for ItemGraphs {
    fn validate_branch(&self, branch: &ParallelBranchDefinition) -> Result<(), GraphError> {
        if branch.node() == "worker" {
            Ok(())
        } else {
            Err(GraphError::NodeNotFound(branch.node().to_owned()))
        }
    }

    fn owned_definition_digest(
        &self,
        _branch: &ParallelBranchDefinition,
    ) -> Result<[u8; 32], GraphError> {
        Ok(WORKER_DIGEST)
    }

    fn compile_branch(
        &self,
        branch: &ParallelBranchDefinition,
        execution: ParallelBranchExecution,
    ) -> Result<CompiledGraph, GraphError> {
        let index = branch
            .id()
            .parse::<usize>()
            .map_err(|_| GraphError::InvalidGraph("a Map item key is its index".into()))?;
        let behavior = Arc::clone(&self.behavior);
        let runs = Arc::clone(&self.runs);
        let in_flight = Arc::clone(&self.in_flight);
        let peak = Arc::clone(&self.peak);
        StateGraph::with_channels(&["entity", "item_index", "prefix", "item_result", "scratch"])
            .add_node(
                execution.wrap_node(FunctionNode::new("worker", move |context| {
                    let behavior = Arc::clone(&behavior);
                    let runs = Arc::clone(&runs);
                    let in_flight = Arc::clone(&in_flight);
                    let peak = Arc::clone(&peak);
                    let entity = context.state.get("entity").cloned().unwrap_or(Value::Null);
                    async move {
                        runs.lock().unwrap().push(index);
                        let now = in_flight.fetch_add(1, Ordering::SeqCst) + 1;
                        peak.fetch_max(now, Ordering::SeqCst);
                        let result = behavior(index, entity).await;
                        in_flight.fetch_sub(1, Ordering::SeqCst);
                        Ok(NodeOutput::new()
                            .with_update("item_result", result?)
                            .with_update("scratch", json!("not landed")))
                    }
                })),
            )
            .add_edge(START, "worker")
            .add_edge("worker", adk_rust::graph::END)
            .compile()
            .map(|graph| graph.with_checkpointer_arc(execution.checkpointer()))
    }

    fn project_input(
        &self,
        _branch: &ParallelBranchDefinition,
        parent: &State,
    ) -> Result<State, GraphError> {
        Ok(parent.clone())
    }

    fn project_result(
        &self,
        _branch: &ParallelBranchDefinition,
        child: &State,
    ) -> Result<ParallelBranchTerminal, GraphError> {
        let mut outputs = serde_json::Map::new();
        for key in ["item_result", "scratch"] {
            outputs.insert(
                key.to_owned(),
                child.get(key).cloned().unwrap_or(Value::Null),
            );
        }
        Ok(ParallelBranchTerminal::Completed(outputs))
    }

    fn pause_cards(
        &self,
        _branch: &ParallelBranchDefinition,
        _pause: &ParallelBranchPause,
    ) -> Result<Vec<ParallelPauseCard>, GraphError> {
        Err(GraphError::InvalidGraph("this worker never pauses".into()))
    }

    async fn resume_input(
        &self,
        _branch: &ParallelBranchDefinition,
        _pause: &ParallelBranchPause,
        _decisions: &[ParallelDecision],
    ) -> Result<State, GraphError> {
        Err(GraphError::InvalidGraph("this worker never resumes".into()))
    }
}

const WORKER_DIGEST: [u8; 32] = [0x77; 32];

struct Harness {
    graph: CompiledGraph,
    children: Arc<ItemChildren>,
    runs: Arc<std::sync::Mutex<Vec<usize>>>,
    peak: Arc<AtomicUsize>,
}

fn map_definition(
    max_items: usize,
    max_concurrency: usize,
    policy: Option<MapPolicy>,
) -> MapDefinition {
    MapDefinition {
        id: "each".to_owned(),
        worker: "worker".to_owned(),
        source: "items".to_owned(),
        item: "entity".to_owned(),
        index: "item_index".to_owned(),
        broadcast: vec!["prefix".to_owned()],
        outputs: vec!["item_result".to_owned()],
        destination: "mapped".to_owned(),
        max_items,
        max_concurrency,
        reduction: policy.is_none().then_some(MapReduction::OrderedCollection),
        policy,
    }
}

const V2_CONTINUE: Option<MapPolicy> = Some(MapPolicy {
    on_error: MapOnError::Continue,
    join: MapJoin::Append,
});

fn harness(definition: MapDefinition, behavior: ItemBehavior) -> Harness {
    let children = Arc::new(ItemChildren::default());
    let parent = Arc::new(ParentRows::default());
    let runs = Arc::new(std::sync::Mutex::new(Vec::new()));
    let peak = Arc::new(AtomicUsize::new(0));
    let graphs = Arc::new(ItemGraphs {
        behavior,
        runs: Arc::clone(&runs),
        in_flight: Arc::new(AtomicUsize::new(0)),
        peak: Arc::clone(&peak),
    });
    let checkpoint_factory: Arc<dyn ParallelChildCheckpointerFactory> = children.clone();
    let appender: Arc<dyn ParallelCheckpointAppender> = parent;
    let runtime = Arc::new(AdkParallelBranchRuntime::new(
        checkpoint_factory,
        graphs,
        Arc::new(ParallelOccurrenceCheckpointer::new(appender)),
    ));
    let source = FanoutSource::Map(MapSource::new(definition, WORKER_DIGEST).expect("Map source"));
    let parent_checkpointer = runtime.parent_checkpointer();
    let graph = StateGraph::with_channels(&["items", "prefix", "mapped"])
        .add_node(DurableParallelNode::from_source(source, runtime))
        .add_edge(START, "each")
        .add_edge("each", adk_rust::graph::END)
        .compile()
        .expect("compile the Map parent")
        .with_checkpointer_arc(parent_checkpointer)
        .with_strict_channels();
    Harness {
        graph,
        children,
        runs,
        peak,
    }
}

fn echo() -> ItemBehavior {
    Arc::new(|_, entity| Box::pin(async move { Ok(entity) }))
}

async fn run(harness: &Harness, items: Value, thread: &str) -> Result<State, GraphError> {
    harness
        .graph
        .invoke(
            HashMap::from([
                ("items".to_owned(), items),
                ("prefix".to_owned(), json!("frozen")),
                ("mapped".to_owned(), json!([{"previous": true}])),
            ]),
            ExecutionConfig::new(thread),
        )
        .await
}

/// Items land in source order although they finish in reverse, and only the
/// Map's selected outputs land.
#[tokio::test]
async fn map_items_land_in_source_order_when_they_finish_in_reverse() {
    let second_done = Arc::new(Notify::new());
    let wait = Arc::clone(&second_done);
    let behavior: ItemBehavior = Arc::new(move |index, entity| {
        let wait = Arc::clone(&wait);
        Box::pin(async move {
            if index == 0 {
                wait.notified().await;
            } else {
                wait.notify_one();
            }
            Ok(entity)
        })
    });
    let harness = harness(map_definition(8, 2, V2_CONTINUE), behavior);
    let state = run(&harness, json!(["a", "b"]), "root-order")
        .await
        .unwrap();
    assert_eq!(*harness.runs.lock().unwrap(), vec![0, 1]);
    assert_eq!(
        state["mapped"],
        json!([
            {"previous": true},
            {"index": 0, "node": "worker", "status": "succeeded", "outputs": {"item_result": "a"}},
            {"index": 1, "node": "worker", "status": "succeeded", "outputs": {"item_result": "b"}},
        ])
    );
}

/// A v1 document lands main's `{index, outputs}` collection through its
/// destination key's own reducer (overwrite here), as main's Map did.
#[tokio::test]
async fn a_v1_map_lands_index_and_outputs_through_its_key_reducer() {
    let harness = harness(map_definition(8, 4, None), echo());
    let state = run(&harness, json!(["a", {"b": 2}, [3]]), "root-v1")
        .await
        .unwrap();
    assert_eq!(
        state["mapped"],
        json!([
            {"index": 0, "outputs": {"item_result": "a"}},
            {"index": 1, "outputs": {"item_result": {"b": 2}}},
            {"index": 2, "outputs": {"item_result": [3]}},
        ])
    );
}

/// Admission never runs more items at once than the Map's concurrency.
#[tokio::test]
async fn a_map_admits_at_most_its_concurrency() {
    let behavior: ItemBehavior = Arc::new(|_, entity| {
        Box::pin(async move {
            tokio::time::sleep(Duration::from_millis(15)).await;
            Ok(entity)
        })
    });
    let harness = harness(map_definition(8, 2, V2_CONTINUE), behavior);
    run(&harness, json!([1, 2, 3, 4, 5, 6]), "root-concurrency")
        .await
        .unwrap();
    assert_eq!(harness.runs.lock().unwrap().len(), 6);
    assert_eq!(harness.peak.load(Ordering::SeqCst), 2);
}

/// Over the item limit, a non-list source, or one item over its byte bound:
/// refused before any child thread is issued.
#[tokio::test]
async fn an_invalid_or_oversized_source_is_refused_before_any_child() {
    let oversized = "x".repeat(MAX_ITEM_BYTES + 1);
    for (items, code) in [
        (json!([1, 2, 3]), "graph.map.resource_exhausted"),
        (json!({"not": "a list"}), "graph.map.invalid_source"),
        (json!([oversized]), "graph.map.resource_exhausted"),
        (deep(60), "graph.map."),
    ] {
        let harness = harness(map_definition(2, 2, V2_CONTINUE), echo());
        let error = run(&harness, items, "root-limits").await.unwrap_err();
        assert!(error.to_string().contains(code), "{code}: {error}");
        assert!(harness.children.issued.lock().await.is_empty());
        assert!(harness.runs.lock().unwrap().is_empty());
    }
}

/// One item nested deeper than the structure guard admits.
fn deep(levels: usize) -> Value {
    let mut value = json!(0);
    for _ in 0..levels {
        value = json!([value]);
    }
    json!([value])
}

/// The node's own limits are refused when the source is built.
#[test]
fn a_map_over_its_configuration_limits_is_refused() {
    for definition in [
        map_definition(65, 2, None),
        map_definition(8, 9, None),
        map_definition(8, 0, None),
    ] {
        assert!(MapSource::new(definition, WORKER_DIGEST).is_err());
    }
    assert!(MapSource::new(map_definition(8, 2, None), [0; 32]).is_err());
}

/// Each item result stays within its bound, but together they exceed the
/// joined bound: the node fails typed and lands nothing.
#[tokio::test]
async fn a_joined_result_over_its_bound_fails_typed() {
    let size = MAX_CHILD_RESULT_BYTES - 1024;
    let items = MAX_JOINED_RESULT_BYTES / size + 1;
    let behavior: ItemBehavior =
        Arc::new(move |_, _| Box::pin(async move { Ok(json!("y".repeat(size))) }));
    let harness = harness(map_definition(64, 8, V2_CONTINUE), behavior);
    let error = run(&harness, json!(vec![0; items]), "root-joined")
        .await
        .unwrap_err();
    assert!(
        error
            .to_string()
            .contains("graph.fanout.joined_result_too_large"),
        "{error}"
    );
}

/// One item's result over the per-child bound is refused when the child
/// saves it: that item fails, and the node fails naming it.
#[tokio::test]
async fn an_item_result_over_its_bound_fails_that_item() {
    let behavior: ItemBehavior =
        Arc::new(|_, _| Box::pin(async { Ok(json!("z".repeat(MAX_CHILD_RESULT_BYTES + 1))) }));
    let harness = harness(map_definition(8, 2, V2_CONTINUE), behavior);
    let error = run(&harness, json!([0]), "root-child-bound")
        .await
        .unwrap_err();
    assert!(error.to_string().contains("branch '0' failed"), "{error}");
}

/// v1 stops on the first failed item: no later item is admitted, the node
/// fails naming that item's index, and nothing lands.
#[tokio::test]
async fn a_failed_item_under_v1_stop_fails_the_map_by_its_index() {
    let behavior: ItemBehavior = Arc::new(|index, entity| {
        Box::pin(async move {
            if index == 1 {
                Err(GraphError::Other("item 1 failed".to_owned()))
            } else {
                Ok(entity)
            }
        })
    });
    let harness = harness(map_definition(8, 1, None), behavior);
    let error = run(&harness, json!(["a", "b", "c"]), "root-stop")
        .await
        .unwrap_err();
    assert!(error.to_string().contains("branch '1' failed"), "{error}");
    assert_eq!(*harness.runs.lock().unwrap(), vec![0, 1]);
}

/// Duplicate items are separate children: each index gets its own thread,
/// derived from the Map identity the activation carries.
#[tokio::test]
async fn duplicate_items_get_distinct_child_threads() {
    let harness = harness(map_definition(8, 2, V2_CONTINUE), echo());
    run(&harness, json!(["same", "same", "same"]), "root-lineage")
        .await
        .unwrap();
    let mut issued = harness.children.issued.lock().await.clone();
    assert_eq!(issued.len(), 3);
    issued.sort();
    issued.dedup();
    assert_eq!(issued.len(), 3, "two items shared a child thread");
    for index in 0..3 {
        assert!(
            issued
                .iter()
                .any(|thread| thread.contains(&format!(":each:0:{index}:"))),
            "{issued:?}"
        );
    }
}

/// Parent rows and item children behind one authority, as the compiler binds it.
#[derive(Default)]
struct MemoryAuthority {
    parent: ParentRows,
    children: ItemChildren,
}

#[async_trait]
impl Checkpointer for MemoryAuthority {
    async fn save(&self, checkpoint: &Checkpoint) -> Result<String, GraphError> {
        self.parent.save(checkpoint).await
    }
    async fn load(&self, thread: &str) -> Result<Option<Checkpoint>, GraphError> {
        self.parent.load(thread).await
    }
    async fn load_by_id(&self, id: &str) -> Result<Option<Checkpoint>, GraphError> {
        self.parent.load_by_id(id).await
    }
    async fn list(&self, thread: &str) -> Result<Vec<Checkpoint>, GraphError> {
        self.parent.list(thread).await
    }
    async fn delete(&self, thread: &str) -> Result<(), GraphError> {
        self.parent.delete(thread).await
    }
}

#[async_trait]
impl ParallelCheckpointAppender for MemoryAuthority {
    async fn append_after(
        &self,
        expected: Option<&Checkpoint>,
        candidate: &Checkpoint,
    ) -> Result<String, GraphError> {
        self.parent.append_after(expected, candidate).await
    }
}

#[async_trait]
impl ParallelChildCheckpointerFactory for MemoryAuthority {
    fn child_origin(
        &self,
        activation: &ParallelActivation,
    ) -> Result<ParallelChildOrigin, GraphError> {
        self.children.child_origin(activation)
    }
    fn branch_thread_id(
        &self,
        activation: &ParallelActivation,
        branch: &ParallelBranchDefinition,
        ordinal: usize,
        input_digest: &[u8; 32],
        origin: &ParallelChildOrigin,
    ) -> Result<String, GraphError> {
        self.children
            .branch_thread_id(activation, branch, ordinal, input_digest, origin)
    }
    async fn for_branch(
        &self,
        activation: &ParallelActivation,
        branch: &ParallelBranchDefinition,
        ordinal: usize,
        input_digest: &[u8; 32],
        origin: &ParallelChildOrigin,
    ) -> Result<ParallelChildCheckpoint, GraphError> {
        self.children
            .for_branch(activation, branch, ordinal, input_digest, origin)
            .await
    }
}

impl ParallelCheckpointAuthority for MemoryAuthority {}

const PIPELINE: &str = "entry_point: map_entities\nstate:\n  entities: {type: list, value: []}\n  item_result: {type: str, value: ''}\n  mapped: {type: list, value: []}\nnodes:\n  - {id: map_entities, type: map, worker: render, source: entities, item: entity, index: item_index, outputs: [item_result], destination: mapped, max_items: 64, max_concurrency: 4, reduction: ordered_collection, transition: END}\n  - {id: render, type: state_modifier, input: [entity, item_index], output: [item_result], template: '{{ entity }}-{{ item_index }}'}\n";

fn bound(authority: &Arc<MemoryAuthority>) -> (Arc<dyn Checkpointer>, PipelineNodeRuntimes) {
    let authority: Arc<dyn ParallelCheckpointAuthority> = authority.clone();
    let checkpointer: Arc<dyn Checkpointer> = authority.clone();
    let runtimes =
        PipelineNodeRuntimes::default().with_parallel(ParallelCompilerBinding::for_tests(
            authority,
            tokio::time::Instant::now() + Duration::from_secs(30),
            None,
        ));
    (checkpointer, runtimes)
}

/// The compiler binds a YAML Map with a State Modifier worker on the shared
/// adapter: each item renders through the real worker, one child per item.
#[tokio::test]
async fn a_compiled_map_runs_its_state_modifier_worker_on_the_adapter() {
    let definition = PipelineDefinition::from_yaml(PIPELINE).unwrap();
    let authority = Arc::new(MemoryAuthority::default());
    let (checkpointer, runtimes) = bound(&authority);
    let graph = definition
        .compile_subgraph_with_runtime(checkpointer, &runtimes)
        .expect("the Map compiles on the adapter");
    let state = graph
        .invoke(
            HashMap::from([("entities".to_owned(), json!(["a", "b"]))]),
            ExecutionConfig::new("root-compiled"),
        )
        .await
        .expect("the Map runs");
    assert_eq!(
        state["mapped"],
        json!([
            {"index": 0, "outputs": {"item_result": "a-0"}},
            {"index": 1, "outputs": {"item_result": "b-1"}},
        ])
    );
    assert_eq!(authority.children.issued.lock().await.len(), 2);
}

/// Without the fan-out binding (a run that did not compose one) a Map is
/// refused readably at compile time, never run.
#[test]
fn a_map_without_its_fanout_binding_is_refused_readably() {
    let definition = PipelineDefinition::from_yaml(PIPELINE).unwrap();
    match definition.compile_subgraph_with_runtime(
        Arc::new(MemoryCheckpointer::new()),
        &PipelineNodeRuntimes::default(),
    ) {
        Err(PipelineConfigurationError::Unsupported(message)) => {
            assert_eq!(message, "the Map fan-out checkpoint authority is not bound");
        }
        Err(other) => panic!("expected a readable refusal, got {other:?}"),
        Ok(_) => panic!("a Map compiled without its fan-out binding"),
    }
}

/// C1: a saved worker pipeline that holds Code is refused by the worker's
/// name with the fan-out child code (Wave 3), as a Parallel branch is.
#[test]
fn a_map_worker_whose_saved_pipeline_contains_code_is_refused_typed() {
    let yaml = PIPELINE.replace(
        "type: state_modifier, input: [entity, item_index], output: [item_result], template: '{{ entity }}-{{ item_index }}'",
        "type: agent, tool: selected, input: [entity, item_index], input_mapping: {task: {type: fixed, value: render}}, output: [item_result]",
    );
    let definition = PipelineDefinition::from_yaml(&yaml).unwrap();
    let child = PipelineDefinition::from_yaml(
        "entry_point: execute\nstate:\n  messages: list\nnodes:\n  - id: execute\n    type: code\n    code: {type: fixed, value: 'result = 1'}\n    input: [messages]\n    output: [messages]\n    structured_output: false\n    transition: END\n",
    )
    .expect("saved worker pipeline with Code");
    let authority = Arc::new(MemoryAuthority::default());
    let (checkpointer, runtimes) = bound(&authority);
    let runtimes = runtimes.with_composition_definitions(std::collections::BTreeMap::from([(
        "render".to_owned(),
        child,
    )]));
    match definition.compile_subgraph_with_runtime(checkpointer, &runtimes) {
        Err(error @ PipelineConfigurationError::FanoutChildUnsupported { .. }) => {
            assert_eq!(error.code(), "graph.fanout.child_contains_code");
            assert!(error.to_string().contains("render"), "{error}");
        }
        Err(other) => panic!("expected a typed Code refusal, got {other:?}"),
        Ok(_) => panic!("a Map worker with Code compiled"),
    }
}

struct UnprovedResolver;

impl PipelineApplicationResolver for UnprovedResolver {
    fn resolve(
        &self,
        _: &PipelineApplicationSelection,
        _: Arc<dyn Checkpointer>,
    ) -> Result<ResolvedApplicationParticipant, ApplicationExecutionError> {
        Err(ApplicationExecutionError::Unavailable)
    }
}

/// A saved Agent worker is admitted by Parallel's participant proof; a
/// participant the resolver cannot prove is refused readably at compile time.
#[test]
fn a_map_agent_worker_without_a_proven_participant_is_refused_readably() {
    let yaml = PIPELINE.replace(
        "type: state_modifier, input: [entity, item_index], output: [item_result], template: '{{ entity }}-{{ item_index }}'",
        "type: agent, tool: selected, input: [entity, item_index], input_mapping: {task: {type: fixed, value: render}}, output: [item_result]",
    );
    let definition = PipelineDefinition::from_yaml(&yaml).unwrap();
    let authority: Arc<dyn ParallelCheckpointAuthority> = Arc::new(MemoryAuthority::default());
    let checkpointer: Arc<dyn Checkpointer> = authority.clone();
    let runtimes = PipelineNodeRuntimes::new(None, None, Some(Arc::new(UnprovedResolver)))
        .with_parallel(ParallelCompilerBinding::for_tests(
            authority,
            tokio::time::Instant::now() + Duration::from_secs(30),
            None,
        ));
    match definition.compile_subgraph_with_runtime(checkpointer, &runtimes) {
        Err(PipelineConfigurationError::Unsupported(message)) => {
            assert_eq!(message, "the Map worker identity is unproved");
        }
        Err(other) => panic!("expected a readable refusal, got {other:?}"),
        Ok(_) => panic!("an unproved Agent worker compiled"),
    }
}

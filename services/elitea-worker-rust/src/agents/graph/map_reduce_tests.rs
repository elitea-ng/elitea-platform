//! Real ADK execution over component checkpoint doubles. These are not PG proofs.
#![allow(clippy::unwrap_used, clippy::expect_used)]

use std::collections::{BTreeMap, HashMap};
use std::fmt::Write as _;
use std::sync::atomic::{AtomicUsize, Ordering};
use std::sync::{Arc, Mutex};
use std::time::Duration;

use adk_rust::graph::checkpoint::RetentionPolicy;
use adk_rust::graph::interrupt::Interrupt;
use adk_rust::graph::{
    Channel, Checkpoint, Checkpointer, CompiledGraph, END, ExecutionConfig, GraphError, Node,
    NodeContext, NodeOutput, START, State, StateGraph, StateSchema,
};
use async_trait::async_trait;
use serde_json::{Map, Value, json};

use super::map_reduce::*;
use super::parallel::ParallelCheckpointAppender;

#[derive(Default)]
struct Rows {
    by_id: HashMap<String, Checkpoint>,
    latest: HashMap<String, String>,
}
#[derive(Default)]
struct Store {
    rows: Mutex<Rows>,
}

fn exact(checkpoint: &Checkpoint) -> Value {
    serde_json::to_value(checkpoint).unwrap()
}

#[async_trait]
impl ParallelCheckpointAppender for Store {
    async fn append_after(
        &self,
        expected: Option<&Checkpoint>,
        candidate: &Checkpoint,
    ) -> Result<String, GraphError> {
        let mut rows = self.rows.lock().unwrap();
        if let Some(existing) = rows.by_id.get(&candidate.checkpoint_id) {
            return if exact(existing) == exact(candidate) {
                Ok(candidate.checkpoint_id.clone())
            } else {
                Err(GraphError::CheckpointError("conflict".to_owned()))
            };
        }
        let current = rows
            .latest
            .get(&candidate.thread_id)
            .and_then(|id| rows.by_id.get(id));
        if current.map(exact) != expected.map(exact) {
            return Err(GraphError::CheckpointError("stale parent".to_owned()));
        }
        rows.by_id
            .insert(candidate.checkpoint_id.clone(), candidate.clone());
        rows.latest
            .insert(candidate.thread_id.clone(), candidate.checkpoint_id.clone());
        Ok(candidate.checkpoint_id.clone())
    }
}
#[async_trait]
impl Checkpointer for Store {
    async fn save(&self, candidate: &Checkpoint) -> Result<String, GraphError> {
        let latest = self.load(&candidate.thread_id).await?;
        self.append_after(latest.as_ref(), candidate).await
    }
    async fn load(&self, thread: &str) -> Result<Option<Checkpoint>, GraphError> {
        let rows = self.rows.lock().unwrap();
        Ok(rows
            .latest
            .get(thread)
            .and_then(|id| rows.by_id.get(id))
            .cloned())
    }
    async fn load_by_id(&self, id: &str) -> Result<Option<Checkpoint>, GraphError> {
        Ok(self.rows.lock().unwrap().by_id.get(id).cloned())
    }
    async fn list(&self, thread: &str) -> Result<Vec<Checkpoint>, GraphError> {
        Ok(self
            .rows
            .lock()
            .unwrap()
            .by_id
            .values()
            .filter(|row| row.thread_id == thread)
            .cloned()
            .collect())
    }
    async fn delete(&self, _thread: &str) -> Result<(), GraphError> {
        Err(GraphError::CheckpointError("immutable".to_owned()))
    }
    async fn prune(&self, _thread: &str, _policy: &RetentionPolicy) -> Result<usize, GraphError> {
        Ok(0)
    }
}

#[derive(Default)]
struct Children {
    stores: Mutex<BTreeMap<String, Arc<Store>>>,
    foreign: bool,
    collide: bool,
}
#[async_trait]
impl MapChildCheckpointerFactory for Children {
    async fn for_item(
        &self,
        activation: &MapActivation,
        item: &FrozenMapItem,
        _worker: &str,
        _kind: MapWorkerKind,
    ) -> Result<MapChildCheckpoint, GraphError> {
        if activation.root_thread_id != "root" {
            return Err(GraphError::CheckpointError("scope".to_owned()));
        }
        let label = item_identity(activation, item)?;
        let thread = if self.foreign {
            "root".to_owned()
        } else if self.collide {
            "child".to_owned()
        } else {
            let mut encoded = String::from("m1:");
            for byte in label {
                write!(&mut encoded, "{byte:02x}").unwrap();
            }
            encoded
        };
        let store = self
            .stores
            .lock()
            .unwrap()
            .entry(thread.clone())
            .or_default()
            .clone();
        Ok(MapChildCheckpoint {
            admitted_threads: std::collections::BTreeSet::from([thread.clone()]),
            thread_id: thread,
            checkpointer: store,
        })
    }
}

#[derive(Clone, Copy)]
enum Mode {
    Normal,
    Fail(usize),
    Pause(usize),
    Block(usize),
    BadOutput,
    Route,
    LargeOutput,
    DeepProjection,
    DeepOutput,
    DeepInterrupt,
    DeepEvent,
    DeepRuntimeInput,
}
struct Workers {
    calls: Arc<Mutex<Vec<usize>>>,
    events: Arc<Mutex<Vec<(bool, usize)>>>,
    active: Arc<AtomicUsize>,
    peak: Arc<AtomicUsize>,
    mode: Mode,
    reject_pause: bool,
    invalid_candidate: bool,
    delayed_first: bool,
}
impl Workers {
    fn new(mode: Mode) -> Self {
        Self {
            calls: Arc::default(),
            events: Arc::default(),
            active: Arc::default(),
            peak: Arc::default(),
            mode,
            reject_pause: false,
            invalid_candidate: false,
            delayed_first: false,
        }
    }
}
struct Worker {
    calls: Arc<Mutex<Vec<usize>>>,
    events: Arc<Mutex<Vec<(bool, usize)>>>,
    active: Arc<AtomicUsize>,
    peak: Arc<AtomicUsize>,
    mode: Mode,
    delayed_first: bool,
}
struct ActiveItem<'a>(&'a AtomicUsize);
impl Drop for ActiveItem<'_> {
    fn drop(&mut self) {
        self.0.fetch_sub(1, Ordering::SeqCst);
    }
}
#[async_trait]
impl Node for Worker {
    fn name(&self) -> &'static str {
        "worker"
    }
    async fn execute(&self, context: &NodeContext) -> Result<NodeOutput, GraphError> {
        let index = usize::try_from(context.state["item_index"].as_u64().unwrap()).unwrap();
        self.calls.lock().unwrap().push(index);
        self.events.lock().unwrap().push((true, index));
        let active = self.active.fetch_add(1, Ordering::SeqCst) + 1;
        let active_item = ActiveItem(&self.active);
        self.peak.fetch_max(active, Ordering::SeqCst);
        let millis = if self.delayed_first {
            if index == 0 { 40 } else { 1 }
        } else {
            u64::try_from(8usize.saturating_sub(index)).unwrap()
        };
        tokio::time::sleep(Duration::from_millis(millis)).await;
        drop(active_item);
        self.events.lock().unwrap().push((false, index));
        if matches!(self.mode,Mode::Fail(n) if n==index) {
            return Err(GraphError::NodeExecutionFailed {
                node: "worker".to_owned(),
                message: "failed".to_owned(),
            });
        }
        if matches!(self.mode,Mode::Pause(n) if n==index) {
            return Ok(NodeOutput::new().with_interrupt(Interrupt::Before("worker".to_owned())));
        }
        if matches!(self.mode, Mode::DeepInterrupt) {
            return Ok(NodeOutput::new().with_interrupt(Interrupt::Dynamic {
                message: "fixture".to_owned(),
                data: Some(deep_value(96)),
            }));
        }
        if matches!(self.mode, Mode::DeepEvent) {
            let mut output = NodeOutput::new();
            output
                .events
                .push(adk_rust::graph::stream::StreamEvent::custom(
                    "worker",
                    "fixture",
                    deep_value(96),
                ));
            return Ok(output);
        }
        let result = if matches!(self.mode, Mode::LargeOutput) {
            json!("x".repeat(300_000))
        } else if matches!(self.mode, Mode::DeepOutput) {
            deep_value(96)
        } else {
            json!({"value":context.state["entity"],"prefix":context.state.get("prefix"),"fields":[1,2,3]})
        };
        let output = NodeOutput::new().with_update("item_result", result);
        if matches!(self.mode, Mode::Route) {
            return Ok(output.with_goto(["worker"]));
        }
        Ok(output)
    }
}

impl MapWorkerGraphFactory for Workers {
    fn validate_nonpausing(&self, _definition: &MapDefinition) -> Result<(), GraphError> {
        if self.reject_pause {
            Err(GraphError::InvalidGraph("pausing worker".to_owned()))
        } else {
            Ok(())
        }
    }
    fn owned_definition_digest(&self) -> [u8; 32] {
        [9; 32]
    }
    fn worker_kind(&self) -> MapWorkerKind {
        MapWorkerKind::StateModifier
    }
    fn compile_item(
        &self,
        _definition: &MapDefinition,
        execution: &MapItemExecution,
    ) -> Result<CompiledGraph, GraphError> {
        let worker = Worker {
            calls: self.calls.clone(),
            events: self.events.clone(),
            active: self.active.clone(),
            peak: self.peak.clone(),
            mode: self.mode,
            delayed_first: self.delayed_first,
        };
        Ok(StateGraph::new(StateSchema::simple(&[
            "entity",
            "item_index",
            "prefix",
            "item_result",
        ]))
        .add_node(execution.wrap_node(worker))
        .add_edge(START, "worker")
        .add_edge("worker", END)
        .compile()?
        .with_checkpointer_arc(execution.checkpointer())
        .with_max_concurrency(1))
    }
    fn runtime_input(
        &self,
        input: &State,
        _context: &NodeContext,
        _thread: &str,
    ) -> Result<State, GraphError> {
        let mut input = input.clone();
        if matches!(self.mode, Mode::DeepRuntimeInput) {
            input.insert("__elitea_tool_resume_v1".to_owned(), deep_value(96));
        }
        Ok(input)
    }
    fn project_result(
        &self,
        _definition: &MapDefinition,
        state: &State,
    ) -> Result<MapWorkerTerminal, GraphError> {
        let index = usize::try_from(state["item_index"].as_u64().unwrap()).unwrap();
        if matches!(self.mode,Mode::Block(n) if n==index) {
            return Ok(MapWorkerTerminal::Blocked);
        }
        let mut output = Map::new();
        output.insert(
            if matches!(self.mode, Mode::BadOutput) {
                "foreign"
            } else {
                "item_result"
            }
            .to_owned(),
            if matches!(self.mode, Mode::DeepProjection) {
                deep_value(96)
            } else {
                state.get("item_result").cloned().unwrap_or(Value::Null)
            },
        );
        Ok(MapWorkerTerminal::Completed(output))
    }
    fn validate_candidate(&self, candidate: &State) -> Result<(), GraphError> {
        if self.invalid_candidate || !candidate["mapped"].is_array() {
            Err(GraphError::InvalidGraph("candidate".to_owned()))
        } else {
            Ok(())
        }
    }
}

fn definition() -> MapDefinition {
    MapDefinition {
        id: "map_items".to_owned(),
        worker: "worker".to_owned(),
        source: "items".to_owned(),
        item: "entity".to_owned(),
        index: "item_index".to_owned(),
        broadcast: vec!["prefix".to_owned()],
        outputs: vec!["item_result".to_owned()],
        destination: "mapped".to_owned(),
        max_items: 64,
        max_concurrency: 4,
        reduction: MapReduction::OrderedCollection,
    }
}
fn context(items: Value, append: bool) -> NodeContext {
    let state = State::from([
        ("items".to_owned(), items),
        ("prefix".to_owned(), json!("frozen")),
        ("mapped".to_owned(), json!([{"previous":true}])),
        ("kept".to_owned(), json!({"nested":[false,null]})),
    ]);
    let mut schema = StateSchema::simple(&["items", "prefix", "mapped", "kept"]);
    if append {
        schema
            .channels
            .insert("mapped".to_owned(), Channel::list("mapped"));
    }
    let mut context = NodeContext::new(state, ExecutionConfig::new("root"), 0);
    context.set_parent_schema(Arc::new(schema));
    context
}
fn setup(
    workers: Workers,
    definition: MapDefinition,
) -> (DurableMapNode, Arc<Store>, Arc<Children>, Arc<Workers>) {
    let store = Arc::new(Store::default());
    let children = Arc::new(Children::default());
    let workers = Arc::new(workers);
    let parent = Arc::new(MapOccurrenceCheckpointer::new(store.clone()));
    let node = DurableMapNode::new(definition, parent, children.clone(), workers.clone()).unwrap();
    (node, store, children, workers)
}
fn completed(outcome: MapNodeOutcome) -> NodeOutput {
    match outcome {
        MapNodeOutcome::Completed(output) => output,
        MapNodeOutcome::Stopped(_) => panic!("expected success"),
    }
}

#[tokio::test]
async fn eight_items_refill_four_slots_without_batch_barrier() {
    let mut workers = Workers::new(Mode::Normal);
    workers.delayed_first = true;
    let (node, _, _, workers) = setup(workers, definition());
    let context = context(json!([0, 1, 2, 3, 4, 5, 6, 7]), false);
    completed(node.execute_outcome(&context).await.unwrap());
    assert_eq!(workers.peak.load(Ordering::SeqCst), 4);
    assert_eq!(workers.calls.lock().unwrap().len(), 8);
    let events = workers.events.lock().unwrap();
    assert!(
        events.iter().position(|event| *event == (true, 4)).unwrap()
            < events
                .iter()
                .position(|event| *event == (false, 0))
                .unwrap()
    );
}

#[tokio::test]
async fn reverse_completion_collects_original_order_and_preserves_structured_values() {
    let (node, _, _, workers) = setup(Workers::new(Mode::Normal), definition());
    let context = context(json!([{"x":1},["x",null],false,42]), false);
    let output = completed(node.execute_outcome(&context).await.unwrap());
    let values = output.updates["mapped"].as_array().unwrap();
    assert_eq!(
        values
            .iter()
            .map(|value| value["index"].as_u64().unwrap())
            .collect::<Vec<_>>(),
        vec![0, 1, 2, 3]
    );
    assert_eq!(
        values[1]["outputs"]["item_result"]["value"],
        json!(["x", null])
    );
    assert_eq!(
        values[0]["outputs"]["item_result"]["fields"],
        json!([1, 2, 3])
    );
    assert_eq!(
        workers
            .events
            .lock()
            .unwrap()
            .iter()
            .find(|event| !event.0)
            .unwrap()
            .1,
        3
    );
    assert_eq!(context.state["kept"], json!({"nested":[false,null]}));
}

#[tokio::test]
async fn completed_item_receipts_survive_parent_loss_without_worker_replay() {
    let (node, store, children, workers) = setup(Workers::new(Mode::Normal), definition());
    let context = context(json!(["duplicate", "duplicate"]), false);
    let first = completed(node.execute_outcome(&context).await.unwrap());
    assert_eq!(workers.calls.lock().unwrap().len(), 2);
    assert_eq!(children.stores.lock().unwrap().len(), 2);
    // Replace only runtime wrappers. Retain the original component store objects.
    let replacement = DurableMapNode::new(
        definition(),
        Arc::new(MapOccurrenceCheckpointer::new(store)),
        children,
        workers.clone(),
    )
    .unwrap();
    let second = completed(replacement.execute_outcome(&context).await.unwrap());
    assert_eq!(first.updates, second.updates);
    assert_eq!(workers.calls.lock().unwrap().len(), 2);
}

#[tokio::test]
async fn failed_item_drains_admitted_items_and_keeps_successful_receipts() {
    let mut workers = Workers::new(Mode::Fail(1));
    workers.delayed_first = true;
    let (node, store, children, workers) = setup(workers, definition());
    let context = context(json!([0, 1, 2, 3, 4, 5, 6, 7]), false);
    let original = context.state.clone();
    assert!(matches!(
        node.execute_outcome(&context).await.unwrap(),
        MapNodeOutcome::Stopped(MapStop::Failed { index: 1 })
    ));
    assert_eq!(workers.active.load(Ordering::SeqCst), 0);
    assert_eq!(workers.calls.lock().unwrap().len(), 4);
    assert_eq!(store.load("root").await.unwrap().unwrap().state, original);
    let terminal = children
        .stores
        .lock()
        .unwrap()
        .values()
        .filter(|store| {
            let rows = store.rows.lock().unwrap();
            rows.by_id.values().any(|row| row.pending_nodes.is_empty())
        })
        .count();
    assert_eq!(terminal, 3);
    node.execute_outcome(&context).await.unwrap();
    assert_eq!(workers.calls.lock().unwrap().len(), 4);
}

#[tokio::test]
async fn unexpected_pause_is_typed_non_success_and_does_not_dispatch_on_replay() {
    let (node, store, children, workers) = setup(Workers::new(Mode::Pause(0)), definition());
    let context = context(json!(["one"]), false);
    assert!(matches!(
        node.execute_outcome(&context).await.unwrap(),
        MapNodeOutcome::Stopped(MapStop::UnexpectedPause { index: 0 })
    ));
    assert!(node.execute(&context).await.is_err());
    assert_eq!(workers.calls.lock().unwrap().len(), 1);
    assert_eq!(
        store.load("root").await.unwrap().unwrap().state,
        context.state
    );
    let children = children.stores.lock().unwrap();
    let rows = children.values().next().unwrap().rows.lock().unwrap();
    assert!(rows.by_id.values().any(|row| {
        row.metadata
            .get("elitea.graph.map.item-receipt.v1")
            .is_some_and(|receipt| {
                receipt["interrupt"]
                    == serde_json::to_value(Interrupt::Before("worker".to_owned())).unwrap()
            })
    }));
}

#[tokio::test]
async fn blocked_terminal_stays_non_success() {
    let (node, _, _, _) = setup(Workers::new(Mode::Block(0)), definition());
    let context = context(json!(["one"]), false);
    assert!(matches!(
        node.execute_outcome(&context).await.unwrap(),
        MapNodeOutcome::Stopped(MapStop::Blocked { index: 0 })
    ));
    assert!(node.execute(&context).await.is_err());
}

#[tokio::test]
async fn source_change_at_original_activation_fails_without_new_children() {
    let (node, _, children, workers) = setup(Workers::new(Mode::Normal), definition());
    node.execute_outcome(&context(json!([1]), false))
        .await
        .unwrap();
    assert!(
        node.execute_outcome(&context(json!([2]), false))
            .await
            .is_err()
    );
    assert_eq!(children.stores.lock().unwrap().len(), 1);
    assert_eq!(workers.calls.lock().unwrap().len(), 1);
}

#[tokio::test]
async fn collector_returns_one_delta_and_parent_reducer_runs_once() {
    let (node, _, _, _) = setup(Workers::new(Mode::Normal), definition());
    let context = context(json!([1, 2]), true);
    let output = completed(node.execute_outcome(&context).await.unwrap());
    assert_eq!(output.updates.len(), 1);
    assert_eq!(output.updates["mapped"].as_array().unwrap().len(), 2);
    let mut applied = context.state.clone();
    context.parent_schema().unwrap().apply_update(
        &mut applied,
        "mapped",
        output.updates["mapped"].clone(),
    );
    assert_eq!(applied["mapped"].as_array().unwrap().len(), 3);
    assert_eq!(context.state["mapped"].as_array().unwrap().len(), 1);
}

#[tokio::test]
async fn empty_list_emits_empty_collection_without_children() {
    let (node, _, children, workers) = setup(Workers::new(Mode::Normal), definition());
    assert_eq!(
        completed(
            node.execute_outcome(&context(json!([]), false))
                .await
                .unwrap()
        )
        .updates["mapped"],
        json!([])
    );
    assert!(children.stores.lock().unwrap().is_empty());
    assert!(workers.calls.lock().unwrap().is_empty());
}

#[tokio::test]
async fn candidate_validation_failure_keeps_parent_state_and_committed_children() {
    let mut workers = Workers::new(Mode::Normal);
    workers.invalid_candidate = true;
    let (node, store, _, workers) = setup(workers, definition());
    let context = context(json!([1]), false);
    assert!(node.execute_outcome(&context).await.is_err());
    assert_eq!(
        store.load("root").await.unwrap().unwrap().state,
        context.state
    );
    assert_eq!(workers.calls.lock().unwrap().len(), 1);
    assert!(node.execute_outcome(&context).await.is_err());
    assert_eq!(workers.calls.lock().unwrap().len(), 1);
}

#[tokio::test]
async fn foreign_outputs_and_worker_routes_are_non_success() {
    for mode in [Mode::BadOutput, Mode::Route] {
        let (node, store, _, _) = setup(Workers::new(mode), definition());
        let context = context(json!([1]), false);
        assert!(matches!(
            node.execute_outcome(&context).await.unwrap(),
            MapNodeOutcome::Stopped(MapStop::Failed { index: 0 })
        ));
        assert_eq!(
            store.load("root").await.unwrap().unwrap().state,
            context.state
        );
    }
}

#[test]
fn invalid_sources_and_resource_limits_fail_before_dispatch() {
    let definition = definition();
    for items in [
        Value::Null,
        json!({"not":"list"}),
        json!((0..65).collect::<Vec<_>>()),
        json!(["x".repeat(MAX_ITEM_BYTES)]),
    ] {
        assert!(definition.freeze(&context(items, false), [9; 32]).is_err());
    }
    let items = Value::Array((0..5).map(|_| Value::String("x".repeat(450_000))).collect());
    assert!(definition.freeze(&context(items, false), [9; 32]).is_err());
}

#[test]
fn collisions_reserved_controls_and_duplicate_channels_are_rejected() {
    let base = definition();
    let mut cases = Vec::new();
    let mut case = base.clone();
    case.index = case.item.clone();
    cases.push(case);
    let mut case = base.clone();
    case.broadcast.push(case.item.clone());
    cases.push(case);
    let mut case = base.clone();
    case.item = "hitl_decisions".to_owned();
    cases.push(case);
    let mut case = base.clone();
    case.outputs.push(case.outputs[0].clone());
    cases.push(case);
    let mut case = base.clone();
    case.max_concurrency = 9;
    cases.push(case);
    let mut case = base;
    case.max_items = 65;
    cases.push(case);
    for case in cases {
        assert!(case.validate().is_err());
    }
}

#[test]
fn duplicate_items_step_worker_and_sequence_binding_are_distinct() {
    let config = definition();
    let context = context(json!(["same", "same"]), false);
    let plan = config.freeze(&context, [9; 32]).unwrap();
    assert_ne!(
        item_identity(&plan.activation, &plan.items[0]).unwrap(),
        item_identity(&plan.activation, &plan.items[1]).unwrap()
    );
    let mut later = context;
    later.step = 1;
    assert!(plan.activation != config.freeze(&later, [9; 32]).unwrap().activation);
    assert!(plan.activation != config.freeze(&later, [8; 32]).unwrap().activation);
    let mut config = definition();
    config.broadcast = vec!["prefix".to_owned(), "kept".to_owned()];
    let first = config.freeze(&later, [9; 32]).unwrap();
    config.broadcast.reverse();
    assert_ne!(
        first.activation.config_digest,
        config
            .freeze(&later, [9; 32])
            .unwrap()
            .activation
            .config_digest
    );
}

#[test]
fn compiler_pausing_admission_fails_closed() {
    let mut workers = Workers::new(Mode::Normal);
    workers.reject_pause = true;
    assert!(
        DurableMapNode::new(
            definition(),
            Arc::new(MapOccurrenceCheckpointer::new(Arc::new(Store::default()))),
            Arc::new(Children::default()),
            Arc::new(workers)
        )
        .is_err()
    );
}

#[tokio::test]
async fn child_scope_collision_fails_before_worker_invocation() {
    for (foreign, collide) in [(true, false), (false, true)] {
        let workers = Arc::new(Workers::new(Mode::Normal));
        let node = DurableMapNode::new(
            definition(),
            Arc::new(MapOccurrenceCheckpointer::new(Arc::new(Store::default()))),
            Arc::new(Children {
                foreign,
                collide,
                ..Children::default()
            }),
            workers.clone(),
        )
        .unwrap();
        assert!(
            node.execute_outcome(&context(json!([1, 2]), false))
                .await
                .is_err()
        );
        assert!(workers.calls.lock().unwrap().is_empty());
    }
}

#[tokio::test]
async fn atomic_appender_rejects_stale_parent_and_preserves_exact_replay() {
    let store = Store::default();
    let original = Checkpoint::new("root", State::new(), 0, vec!["map_items".to_owned()]);
    store.append_after(None, &original).await.unwrap();
    let first = Checkpoint::new("root", State::new(), 0, vec!["map_items".to_owned()]);
    let second = Checkpoint::new("root", State::new(), 0, vec!["map_items".to_owned()]);
    store.append_after(Some(&original), &first).await.unwrap();
    assert!(store.append_after(Some(&original), &second).await.is_err());
    store.append_after(None, &original).await.unwrap();
    assert_eq!(
        store.load("root").await.unwrap().unwrap().checkpoint_id,
        first.checkpoint_id
    );
}

#[tokio::test]
async fn real_parent_adk_transition_applies_once_and_terminal_replay_skips_map() {
    let (node, store, _, workers) = setup(Workers::new(Mode::Normal), definition());
    let context = context(json!([1, 2]), true);
    let checkpointer = node.parent_checkpointer();
    let graph = StateGraph::new((*context.parent_schema().unwrap()).clone())
        .add_node(node)
        .add_edge(START, "map_items")
        .add_edge("map_items", END)
        .compile()
        .unwrap()
        .with_checkpointer_arc(checkpointer)
        .with_max_concurrency(1);
    let first = graph
        .invoke(context.state.clone(), ExecutionConfig::new("root"))
        .await
        .unwrap();
    assert_eq!(first["mapped"].as_array().unwrap().len(), 3);
    let second = graph
        .invoke(State::new(), ExecutionConfig::new("root"))
        .await
        .unwrap();
    assert_eq!(first, second);
    assert_eq!(workers.calls.lock().unwrap().len(), 2);
    assert!(
        store
            .load("root")
            .await
            .unwrap()
            .unwrap()
            .pending_nodes
            .is_empty()
    );
}

#[tokio::test]
async fn collected_memory_limit_stops_admission_and_preserves_parent_state() {
    let (node, store, _, workers) = setup(Workers::new(Mode::LargeOutput), definition());
    let context = context(json!((0..64).collect::<Vec<_>>()), false);
    assert!(matches!(
        node.execute_outcome(&context).await.unwrap(),
        MapNodeOutcome::Stopped(MapStop::ResourceExhausted { .. })
    ));
    assert_eq!(workers.active.load(Ordering::SeqCst), 0);
    assert!(workers.calls.lock().unwrap().len() < 64);
    assert_eq!(
        store.load("root").await.unwrap().unwrap().state,
        context.state
    );
}

#[tokio::test]
async fn incompatible_reducer_fails_before_any_item_dispatch() {
    let (node, _, children, workers) = setup(Workers::new(Mode::Normal), definition());
    let mut context = context(json!([1]), false);
    let mut schema = (*context.parent_schema().unwrap()).clone();
    schema
        .channels
        .insert("mapped".to_owned(), Channel::counter("mapped"));
    context.set_parent_schema(Arc::new(schema));
    assert!(node.execute_outcome(&context).await.is_err());
    assert!(children.stores.lock().unwrap().is_empty());
    assert!(workers.calls.lock().unwrap().is_empty());
}

fn deep_value(depth: usize) -> Value {
    let mut value = Value::Null;
    for _ in 0..depth {
        value = Value::Array(vec![value]);
    }
    value
}

#[test]
fn adversarial_compact_source_and_broadcast_values_fail_structure_guard() {
    for depth in [48, 60, 96, 512] {
        let source = context(Value::Array(vec![deep_value(depth)]), false);
        assert!(definition().freeze(&source, [9; 32]).is_err());
        let mut broadcast = context(json!([0]), false);
        broadcast
            .state
            .insert("prefix".to_owned(), deep_value(depth));
        assert!(definition().freeze(&broadcast, [9; 32]).is_err());
    }
    let wide = Value::Array(vec![Value::Null; 32_769]);
    assert!(
        definition()
            .freeze(&context(Value::Array(vec![wide]), false), [9; 32])
            .is_err()
    );
}

#[tokio::test]
async fn adversarial_projected_result_fails_before_collection_and_keeps_parent_state() {
    let (node, store, _, workers) = setup(Workers::new(Mode::DeepProjection), definition());
    let context = context(json!([1]), false);
    assert!(matches!(
        node.execute_outcome(&context).await.unwrap(),
        MapNodeOutcome::Stopped(MapStop::Failed { index: 0 })
    ));
    assert_eq!(
        store.load("root").await.unwrap().unwrap().state,
        context.state
    );
    node.execute_outcome(&context).await.unwrap();
    assert_eq!(workers.calls.lock().unwrap().len(), 1);
}

#[tokio::test]
async fn adversarial_worker_updates_interrupts_and_events_fail_before_adk_capture() {
    for mode in [Mode::DeepOutput, Mode::DeepInterrupt, Mode::DeepEvent] {
        let (node, store, _, workers) = setup(Workers::new(mode), definition());
        let context = context(json!([1]), false);
        assert!(matches!(
            node.execute_outcome(&context).await.unwrap(),
            MapNodeOutcome::Stopped(MapStop::Failed { index: 0 })
        ));
        assert_eq!(
            store.load("root").await.unwrap().unwrap().state,
            context.state
        );
        node.execute_outcome(&context).await.unwrap();
        assert_eq!(workers.calls.lock().unwrap().len(), 1);
    }
}

#[tokio::test]
async fn adversarial_runtime_controls_fail_before_worker_dispatch() {
    let workers = Workers::new(Mode::DeepRuntimeInput);
    let (node, store, _, workers) = setup(workers, definition());
    let context = context(json!([1]), false);
    assert!(matches!(
        node.execute_outcome(&context).await.unwrap(),
        MapNodeOutcome::Stopped(MapStop::Failed { index: 0 })
    ));
    assert!(workers.calls.lock().unwrap().is_empty());
    assert_eq!(
        store.load("root").await.unwrap().unwrap().state,
        context.state
    );
}

#[tokio::test]
async fn adversarial_checkpoint_metadata_fails_before_store_clone_or_serialization() {
    let store = Arc::new(Store::default());
    let checkpoint = MapOccurrenceCheckpointer::new(store.clone());
    let mut candidate = Checkpoint::new("root", State::new(), 0, vec!["map_items".to_owned()]);
    candidate
        .metadata
        .insert("fixture".to_owned(), deep_value(96));
    assert!(checkpoint.save(&candidate).await.is_err());
    assert!(store.rows.lock().unwrap().by_id.is_empty());
}

#[tokio::test(start_paused = true)]
async fn original_deadline_stops_and_drops_owned_effectfree_futures_without_replay() {
    let (node, store, _, workers) = setup(Workers::new(Mode::Normal), definition());
    let node = node.with_deadline(tokio::time::Instant::now() + Duration::from_millis(1));
    let context = context(json!([0, 1, 2, 3, 4, 5]), false);
    assert!(matches!(
        node.execute_outcome(&context).await.unwrap(),
        MapNodeOutcome::Stopped(MapStop::DeadlineExceeded)
    ));
    assert_eq!(workers.active.load(Ordering::SeqCst), 0);
    assert_eq!(
        store.load("root").await.unwrap().unwrap().state,
        context.state
    );
    let calls = workers.calls.lock().unwrap().len();
    assert!(calls <= 4);
    node.execute_outcome(&context).await.unwrap();
    assert_eq!(workers.calls.lock().unwrap().len(), calls);
}

//! Actual ADK compilation with component authority holders. No `PostgreSQL` proof.
#![allow(clippy::unwrap_used, clippy::expect_used)]

use std::collections::{BTreeMap, BTreeSet};
use std::sync::{Arc, Mutex};
use std::time::Duration;

use adk_rust::graph::checkpoint::RetentionPolicy;
use adk_rust::graph::{Checkpoint, Checkpointer, ExecutionConfig, GraphError, State};
use async_trait::async_trait;
use serde_json::{Value, json};

use super::*;
use crate::agents::graph::{
    FrozenMapItem, MapActivation, MapChildCheckpoint, MapChildCheckpointerFactory,
    MapExecutionIdentity, MapWorkerKind, ParallelCheckpointAppender,
};

#[derive(Default)]
struct Rows {
    latest: BTreeMap<String, String>,
    by_id: BTreeMap<String, Checkpoint>,
}
#[derive(Default)]
struct TestAuthority {
    rows: Mutex<Rows>,
    minted: Mutex<Vec<String>>,
}

fn exact(checkpoint: &Checkpoint) -> Value {
    serde_json::to_value(checkpoint).unwrap()
}

#[async_trait]
impl Checkpointer for TestAuthority {
    async fn save(&self, checkpoint: &Checkpoint) -> Result<String, GraphError> {
        let latest = self.load(&checkpoint.thread_id).await?;
        self.append_after(latest.as_ref(), checkpoint).await
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
    async fn delete(&self, _: &str) -> Result<(), GraphError> {
        Err(map_error("immutable_receipt"))
    }
    async fn prune(&self, _: &str, _: &RetentionPolicy) -> Result<usize, GraphError> {
        Err(map_error("immutable_receipt"))
    }
}

#[async_trait]
impl ParallelCheckpointAppender for TestAuthority {
    async fn append_after(
        &self,
        expected: Option<&Checkpoint>,
        candidate: &Checkpoint,
    ) -> Result<String, GraphError> {
        let mut rows = self.rows.lock().unwrap();
        if let Some(row) = rows.by_id.get(&candidate.checkpoint_id) {
            return if exact(row) == exact(candidate) {
                Ok(candidate.checkpoint_id.clone())
            } else {
                Err(map_error("stale_activation"))
            };
        }
        let latest = rows
            .latest
            .get(&candidate.thread_id)
            .and_then(|id| rows.by_id.get(id));
        if latest.map(exact) != expected.map(exact) {
            return Err(map_error("stale_activation"));
        }
        rows.latest
            .insert(candidate.thread_id.clone(), candidate.checkpoint_id.clone());
        rows.by_id
            .insert(candidate.checkpoint_id.clone(), candidate.clone());
        Ok(candidate.checkpoint_id.clone())
    }
}

struct ChildAuthority {
    inner: Arc<TestAuthority>,
    threads: BTreeSet<String>,
}
#[async_trait]
impl Checkpointer for ChildAuthority {
    async fn save(&self, checkpoint: &Checkpoint) -> Result<String, GraphError> {
        if !self.threads.contains(&checkpoint.thread_id) {
            return Err(map_error("invalid_child_scope"));
        }
        self.inner.save(checkpoint).await
    }
    async fn load(&self, thread: &str) -> Result<Option<Checkpoint>, GraphError> {
        if !self.threads.contains(thread) {
            return Err(map_error("invalid_child_scope"));
        }
        self.inner.load(thread).await
    }
    async fn load_by_id(&self, id: &str) -> Result<Option<Checkpoint>, GraphError> {
        let saved = self.inner.load_by_id(id).await?;
        if saved
            .as_ref()
            .is_some_and(|saved| !self.threads.contains(&saved.thread_id))
        {
            return Err(map_error("invalid_child_scope"));
        }
        Ok(saved)
    }
    async fn list(&self, thread: &str) -> Result<Vec<Checkpoint>, GraphError> {
        if !self.threads.contains(thread) {
            return Err(map_error("invalid_child_scope"));
        }
        self.inner.list(thread).await
    }
    async fn delete(&self, _: &str) -> Result<(), GraphError> {
        Err(map_error("immutable_receipt"))
    }
    async fn prune(&self, _: &str, _: &RetentionPolicy) -> Result<usize, GraphError> {
        Err(map_error("immutable_receipt"))
    }
}

// Tests opt into this holder. Production factories derive it from the existing writer authority.
struct BoundAuthority(Arc<TestAuthority>, &'static str);
impl crate::agents::graph::map_authority::sealed::Sealed for BoundAuthority {}
#[async_trait]
impl MapChildCheckpointerFactory for BoundAuthority {
    fn execution_identity(&self, root_thread: &str) -> Result<MapExecutionIdentity, GraphError> {
        if root_thread != "root" {
            return Err(map_error("invalid_turn_authority"));
        }
        Ok(MapExecutionIdentity {
            execution_id: self.1.to_owned(),
            generation: 1,
        })
    }
    async fn for_item(
        &self,
        activation: &MapActivation,
        item: &FrozenMapItem,
        worker: &str,
        kind: MapWorkerKind,
    ) -> Result<MapChildCheckpoint, GraphError> {
        if activation.root_thread_id != "root" {
            return Err(map_error("invalid_child_scope"));
        }
        let thread_id = format!("map-test:{}:{}", activation.step, item.index);
        self.0.minted.lock().unwrap().push(thread_id.clone());
        let mut admitted_threads = BTreeSet::from([thread_id.clone()]);
        if kind == MapWorkerKind::Application {
            admitted_threads.insert(format!("{thread_id}/{worker}"));
        }
        Ok(MapChildCheckpoint {
            thread_id,
            checkpointer: Arc::new(ChildAuthority {
                inner: Arc::clone(&self.0),
                threads: admitted_threads.clone(),
            }),
            admitted_threads,
        })
    }
}
#[async_trait]
impl Checkpointer for BoundAuthority {
    async fn save(&self, checkpoint: &Checkpoint) -> Result<String, GraphError> {
        self.0.as_ref().save(checkpoint).await
    }
    async fn load(&self, thread: &str) -> Result<Option<Checkpoint>, GraphError> {
        self.0.as_ref().load(thread).await
    }
    async fn load_by_id(&self, id: &str) -> Result<Option<Checkpoint>, GraphError> {
        self.0.as_ref().load_by_id(id).await
    }
    async fn list(&self, thread: &str) -> Result<Vec<Checkpoint>, GraphError> {
        self.0.as_ref().list(thread).await
    }
    async fn delete(&self, thread: &str) -> Result<(), GraphError> {
        self.0.as_ref().delete(thread).await
    }
    async fn prune(&self, thread: &str, policy: &RetentionPolicy) -> Result<usize, GraphError> {
        self.0.as_ref().prune(thread, policy).await
    }
}
#[async_trait]
impl ParallelCheckpointAppender for BoundAuthority {
    async fn append_after(
        &self,
        parent: Option<&Checkpoint>,
        checkpoint: &Checkpoint,
    ) -> Result<String, GraphError> {
        self.0.as_ref().append_after(parent, checkpoint).await
    }
}
impl MapCheckpointAuthority for BoundAuthority {}

fn document() -> String {
    "entry_point: map_entities\nstate:\n  entities: {type: list, value: []}\n  item_result: {type: str, value: ''}\n  mapped: {type: list, value: []}\nnodes:\n  - {id: map_entities, type: map, worker: render, source: entities, item: entity, index: item_index, outputs: [item_result], destination: mapped, max_items: 64, max_concurrency: 4, reduction: ordered_collection, transition: END}\n  - {id: render, type: state_modifier, input: [entity, item_index], output: [item_result], template: '{{ item_index }}'}\n".to_owned()
}

fn binding(store: &Arc<TestAuthority>, deadline: tokio::time::Instant) -> Arc<MapCompilerBinding> {
    MapCompilerBinding::for_tests(
        Arc::new(BoundAuthority(Arc::clone(store), "original")),
        deadline,
    )
}

#[test]
fn map_yaml_child_channels_are_opaque_and_not_parent_descriptors() {
    let definition = PipelineDefinition::from_yaml(&document()).unwrap();
    assert!(definition.has_map_nodes());
    assert!(!definition.declared_variable_types().contains_key("entity"));
    assert!(!definition.runtime_channels().contains("entity"));
    assert!(!definition.runtime_channels().contains("item_index"));
    assert!(definition.map_owned_nodes.contains("render"));
}

#[test]
fn map_yaml_rejects_ownership_routes_pauses_and_local_global_collisions() {
    for yaml in [
        document().replace("entry_point: map_entities", "entry_point: render"),
        document().replace(
            "input: [entity, item_index]",
            "transition: END, input: [entity, item_index]",
        ),
        document().replace("state:\n", "interrupt_before: [render]\nstate:\n"),
        document().replace("state:\n", "state:\n  entity: {type: dict, value: {}}\n"),
        document().replace("outputs: [item_result]", "outputs: [mapped]"),
        document().replace("max_items: 64", "max_items: 65"),
        document().replace("max_concurrency: 4", "max_concurrency: 9"),
        document().replace("reduction: ordered_collection", "reduction: infer"),
    ] {
        assert!(PipelineDefinition::from_yaml(&yaml).is_err());
    }
}

#[tokio::test]
async fn map_production_binding_and_missing_authority_fail_before_dispatch() {
    let store = Arc::new(TestAuthority::default());
    let definition = PipelineDefinition::from_yaml(&document()).unwrap();
    assert!(
        MapCompilerBinding::new(
            Arc::new(BoundAuthority(Arc::clone(&store), "original")),
            tokio::time::Instant::now() + Duration::from_secs(1)
        )
        .is_err()
    );
    assert!(
        definition
            .compile_subgraph_with_runtime(store.clone(), &PipelineNodeRuntimes::default())
            .is_err()
    );
    assert!(store.rows.lock().unwrap().by_id.is_empty());
}

#[tokio::test]
async fn map_foreign_parent_pointer_refuses_before_child_activation() {
    let store = Arc::new(TestAuthority::default());
    let definition = PipelineDefinition::from_yaml(&document()).unwrap();
    let runtimes = PipelineNodeRuntimes::default().with_map(binding(
        &store,
        tokio::time::Instant::now() + Duration::from_secs(1),
    ));
    assert!(
        definition
            .compile_subgraph_with_runtime(store.clone(), &runtimes)
            .is_err()
    );
    assert!(store.minted.lock().unwrap().is_empty());
}

#[tokio::test]
async fn map_actual_adk_graph_collects_unknown_items_and_terminal_replay_once() {
    let store = Arc::new(TestAuthority::default());
    let binding = binding(
        &store,
        tokio::time::Instant::now() + Duration::from_secs(30),
    );
    let runtimes = PipelineNodeRuntimes::default().with_map(Arc::clone(&binding));
    let definition = PipelineDefinition::from_yaml(&document()).unwrap();
    let graph = definition
        .compile_subgraph_with_runtime(binding.parent_checkpointer(), &runtimes)
        .unwrap();
    let source = json!([{"x":[1,"two"]}, null, true, ["opaque",3]]);
    let result = graph
        .invoke(
            State::from([("entities".to_owned(), source.clone())]),
            ExecutionConfig::new("root"),
        )
        .await
        .unwrap();
    assert_eq!(result["entities"], source);
    assert_eq!(result["item_result"], "");
    assert_eq!(
        result["mapped"],
        json!([{"index":0,"outputs":{"item_result":"0"}},{"index":1,"outputs":{"item_result":"1"}},{"index":2,"outputs":{"item_result":"2"}},{"index":3,"outputs":{"item_result":"3"}}])
    );
    let minted = store.minted.lock().unwrap().len();
    let replay = graph
        .invoke(State::new(), ExecutionConfig::new("root"))
        .await
        .unwrap();
    assert_eq!(replay["mapped"], result["mapped"]);
    assert_eq!(store.minted.lock().unwrap().len(), minted);
}

#[tokio::test]
async fn map_expired_original_deadline_preserves_parent_business_state() {
    let store = Arc::new(TestAuthority::default());
    let binding = binding(&store, tokio::time::Instant::now() - Duration::from_secs(1));
    let definition = PipelineDefinition::from_yaml(&document()).unwrap();
    let graph = definition
        .compile_subgraph_with_runtime(
            binding.parent_checkpointer(),
            &PipelineNodeRuntimes::default().with_map(binding),
        )
        .unwrap();
    assert!(
        graph
            .invoke(
                State::from([("entities".to_owned(), json!([1]))]),
                ExecutionConfig::new("root")
            )
            .await
            .is_err()
    );
    let saved = store.load("root").await.unwrap().unwrap();
    assert_eq!(saved.state["mapped"], json!([]));
    assert_eq!(saved.state["entities"], json!([1]));
    assert!(
        store
            .rows
            .lock()
            .unwrap()
            .by_id
            .values()
            .all(|row| row.thread_id == "root")
    );
    assert!(store.minted.lock().unwrap().is_empty());
}

#[test]
fn map_checkpoint_json_roundtrip_reserves_codec_envelope_depth() {
    let mut value = Value::Null;
    for _ in 0..47 {
        value = Value::Array(vec![value]);
    }
    let checkpoint = Checkpoint::new(
        "root",
        State::from([("opaque".to_owned(), value)]),
        0,
        vec![],
    );
    crate::agents::graph::map_reduce::validate_checkpoint_boundary(&checkpoint).unwrap();
    let decoded: Checkpoint =
        serde_json::from_slice(&serde_json::to_vec(&checkpoint).unwrap()).unwrap();
    assert_eq!(exact(&decoded), exact(&checkpoint));
}

#[test]
fn map_keeps_explicit_root_declaration_order_and_selected_terminal_key_seam() {
    let yaml = document().replace(
        "state:\n",
        "state:\n  '10': {type: str, value: ten}\n  '2': {type: bool, value: false}\n",
    );
    let definition = PipelineDefinition::from_yaml(&yaml).unwrap();
    assert_eq!(&definition.state_declaration_order[..2], ["10", "2"]);
    assert_eq!(
        definition.map_terminal_json_keys(),
        BTreeSet::from(["mapped".to_owned()])
    );
    assert!(!definition.recovery_frontier_supported(&["render".to_owned()]));
    assert!(definition.recovery_frontier_supported(&["map_entities".to_owned()]));
}

#[test]
fn map_nonpausing_cohort_refuses_static_pause_and_effectful_saved_definitions() {
    let yaml = "entry_point: render\nstate: {topic: {type: str, value: test}, detail: {type: str, value: ''}}\nnodes:\n  - {id: render, type: state_modifier, input: [topic], output: [detail], template: '{{ topic }}', transition: END}\n";
    assert!(
        PipelineDefinition::from_yaml(yaml)
            .unwrap()
            .map_nonpausing_effectfree()
    );
    assert!(
        !PipelineDefinition::from_yaml(
            &yaml.replace("state:", "interrupt_before: [render]\nstate:")
        )
        .unwrap()
        .map_nonpausing_effectfree()
    );
    assert!(
        !PipelineDefinition::from_yaml(&document())
            .unwrap()
            .map_nonpausing_effectfree()
    );
}

#[tokio::test]
async fn map_typed_turn_reclaim_retains_capabilities_and_exact_original_execution() {
    use crate::agents::graph::map_turn::MapTurnAuthority;
    let store = Arc::new(TestAuthority::default());
    let authority: Arc<dyn MapCheckpointAuthority> =
        Arc::new(BoundAuthority(Arc::clone(&store), "original"));
    let original: Arc<dyn Checkpointer> = authority.clone();
    let turn = Arc::new(
        MapTurnAuthority::new(authority.clone(), &original, "root", "original", 1, false).unwrap(),
    );
    let parent: Arc<dyn Checkpointer> = turn.clone();
    let map =
        MapCompilerBinding::for_tests(turn, tokio::time::Instant::now() + Duration::from_secs(10));
    let definition = PipelineDefinition::from_yaml(&document()).unwrap();
    let graph = definition
        .compile_subgraph_with_runtime(parent, &PipelineNodeRuntimes::default().with_map(map))
        .unwrap();
    let mut input = State::new();
    input.insert("entities".to_owned(), json!([null, {"unknown":[1]}]));
    let result = graph
        .invoke_detailed(input, ExecutionConfig::new("root"))
        .await
        .unwrap();
    let completed = store.load("root").await.unwrap().unwrap();
    assert_eq!(
        completed.metadata["elitea.pipeline.execution.v1"],
        json!(["original", 1])
    );
    let reclaimed =
        MapTurnAuthority::new(authority.clone(), &original, "root", "original", 1, false).unwrap();
    assert_eq!(
        reclaimed.load("root").await.unwrap().unwrap().state,
        result.state
    );
    assert!(
        MapTurnAuthority::new(authority.clone(), &original, "root", "different", 1, false).is_err()
    );
    assert!(
        MapTurnAuthority::new(authority.clone(), &original, "root", "original", 2, false).is_err()
    );
    let new_authority: Arc<dyn MapCheckpointAuthority> =
        Arc::new(BoundAuthority(Arc::clone(&store), "different"));
    let new_original: Arc<dyn Checkpointer> = new_authority.clone();
    let fresh =
        MapTurnAuthority::new(new_authority, &new_original, "root", "different", 1, false).unwrap();
    assert!(fresh.load("root").await.unwrap().is_none());
    assert!(
        fresh
            .load_by_id(&completed.checkpoint_id)
            .await
            .unwrap()
            .is_some()
    );
    let foreign: Arc<dyn Checkpointer> = Arc::new(TestAuthority::default());
    assert!(
        MapTurnAuthority::new(authority.clone(), &foreign, "root", "original", 1, false).is_err()
    );
    assert!(MapTurnAuthority::new(authority, &original, "root", "new-command", 1, true).is_err());
}

struct UnprovedAgent {
    zero: bool,
}
impl crate::agents::graph::PipelineApplicationResolver for UnprovedAgent {
    fn map_worker_definition_digest(
        &self,
        _: &str,
    ) -> Result<[u8; 32], crate::agents::graph::ApplicationExecutionError> {
        if self.zero {
            Ok([0; 32])
        } else {
            Err(crate::agents::graph::ApplicationExecutionError::Unavailable)
        }
    }
    fn resolve(
        &self,
        _: &crate::agents::graph::PipelineApplicationSelection,
        _: Arc<dyn Checkpointer>,
    ) -> Result<
        crate::agents::graph::ResolvedApplicationParticipant,
        crate::agents::graph::ApplicationExecutionError,
    > {
        Err(crate::agents::graph::ApplicationExecutionError::Unavailable)
    }
}
#[tokio::test]
async fn map_saved_agent_refuses_alias_and_zero_fingerprint_before_dispatch() {
    let yaml = document().replace("type: state_modifier, input: [entity, item_index], output: [item_result], template: '{{ item_index }}'", "type: agent, tool: selected, input: [entity, item_index], input_mapping: {task: {type: fixed, value: render}}, output: [item_result]");
    let definition = PipelineDefinition::from_yaml(&yaml).unwrap();
    for zero in [true, false] {
        let store = Arc::new(TestAuthority::default());
        let binding = binding(
            &store,
            tokio::time::Instant::now() + Duration::from_secs(10),
        );
        let runtimes =
            PipelineNodeRuntimes::new(None, None, Some(Arc::new(UnprovedAgent { zero })))
                .with_map(binding.clone());
        assert!(
            definition
                .compile_subgraph_with_runtime(binding.parent_checkpointer(), &runtimes)
                .is_err()
        );
        assert!(store.minted.lock().unwrap().is_empty());
        assert!(store.rows.lock().unwrap().by_id.is_empty());
    }
}
#[test]
fn map_saved_agent_rejects_deep_fixed_value_before_definition_hashing() {
    let deep = format!("{}null{}", "[".repeat(96), "]".repeat(96));
    let yaml = document().replace("type: state_modifier, input: [entity, item_index], output: [item_result], template: '{{ item_index }}'", &format!("type: agent, tool: selected, input: [entity, item_index], input_mapping: {{task: {{type: fixed, value: render}}, opaque: {{type: fixed, value: {deep}}}}}, output: [item_result]"));
    assert!(PipelineDefinition::from_yaml(&yaml).is_err());
}

#[tokio::test]
async fn map_in_place_collection_and_overlapping_broadcast_use_original_frozen_values() {
    let yaml = document()
        .replace("destination: mapped", "destination: entities")
        .replace(
            "outputs: [item_result], destination:",
            "broadcast: [entities, item_result], outputs: [item_result], destination:",
        )
        .replace(
            "input: [entity, item_index]",
            "input: [entity, item_index, entities, item_result]",
        )
        .replace(
            "template: '{{ item_index }}'",
            "template: '{{ item_result }}-{{ item_index }}'",
        );
    let definition = PipelineDefinition::from_yaml(&yaml).unwrap();
    let store = Arc::new(TestAuthority::default());
    let binding = binding(
        &store,
        tokio::time::Instant::now() + Duration::from_secs(10),
    );
    let graph = definition
        .compile_subgraph_with_runtime(
            binding.parent_checkpointer(),
            &PipelineNodeRuntimes::default().with_map(binding),
        )
        .unwrap();
    let input = State::from([
        ("entities".to_owned(), json!([null, {"opaque":[1]}])),
        ("item_result".to_owned(), json!("original")),
    ]);
    let result = graph
        .invoke(input, ExecutionConfig::new("root"))
        .await
        .unwrap();
    assert_eq!(
        result["entities"],
        json!([{"index":0,"outputs":{"item_result":"original-0"}},{"index":1,"outputs":{"item_result":"original-1"}}])
    );
    assert_eq!(result["item_result"], "original");
    assert_eq!(result["mapped"], json!([]));
}

#[tokio::test]
async fn map_fresh_turn_keeps_exact_physical_parent_cas_and_old_receipts() {
    use crate::agents::graph::map_turn::MapTurnAuthority;
    let store = Arc::new(TestAuthority::default());
    let first: Arc<dyn MapCheckpointAuthority> =
        Arc::new(BoundAuthority(Arc::clone(&store), "original"));
    let original: Arc<dyn Checkpointer> = first.clone();
    let first = MapTurnAuthority::new(first, &original, "root", "original", 1, false).unwrap();
    let previous = Checkpoint::new(
        "root",
        State::from([("old".to_owned(), json!(true))]),
        2,
        vec![],
    );
    first.save(&previous).await.unwrap();
    let saved = first.load("root").await.unwrap().unwrap();
    let next: Arc<dyn MapCheckpointAuthority> =
        Arc::new(BoundAuthority(Arc::clone(&store), "different"));
    let next_original: Arc<dyn Checkpointer> = next.clone();
    let next = MapTurnAuthority::new(next, &next_original, "root", "different", 1, false).unwrap();
    assert!(next.load("root").await.unwrap().is_none());
    let candidate = Checkpoint::new(
        "root",
        State::from([("fresh".to_owned(), json!(true))]),
        0,
        vec!["map".to_owned()],
    );
    next.append_after(None, &candidate).await.unwrap();
    let latest = next.load("root").await.unwrap().unwrap();
    assert_eq!(latest.state, candidate.state);
    assert!(!latest.state.contains_key("old"));
    assert_eq!(
        exact(
            &next
                .load_by_id(&saved.checkpoint_id)
                .await
                .unwrap()
                .unwrap()
        ),
        exact(&saved)
    );
    next.append_after(None, &saved).await.unwrap();
    assert_eq!(
        next.load("root").await.unwrap().unwrap().checkpoint_id,
        latest.checkpoint_id
    );
    let stale_none = Checkpoint::new("root", State::new(), 1, vec!["map".to_owned()]);
    assert!(next.append_after(None, &stale_none).await.is_err());
}

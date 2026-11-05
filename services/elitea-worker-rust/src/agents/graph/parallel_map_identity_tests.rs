//! T-2A3: the per-family identity a fan-out activation carries. A Map source
//! always carries the Map identity and derives main's `m1:` child ids; a
//! Parallel source never carries it and keeps its `p1:` ids and bytes.

use std::collections::HashMap;

use adk_rust::graph::{ExecutionConfig, NodeContext};
use serde_json::json;

use super::super::fanout_source::{FanoutSource, MapSource};
use super::super::map_reduce::{MapDefinition, MapReduction, source_digest};
use super::super::yaml::ParallelNodeDefinition;
use super::{FanoutChildOrigin, ParallelActivation};
use crate::state::{CheckpointWriterAuthority, derive_child_thread_id};

const WORKER_DIGEST: [u8; 32] = [0x5a; 32];

fn writer() -> CheckpointWriterAuthority {
    CheckpointWriterAuthority::new(
        "tenant-1".to_owned(),
        7,
        8,
        "agent.execute.application.v1",
        [0x11; 32],
        "root-thread".to_owned(),
        "execution-1".to_owned(),
        1,
        "claim-1".to_owned(),
        1,
        1,
        1_700_000_000_000_000,
        "workload-1".to_owned(),
        "producer-1".to_owned(),
        [0x22; 32],
    )
    .expect("valid writer")
}

fn origin() -> FanoutChildOrigin {
    FanoutChildOrigin {
        execution_id: "execution-1".to_owned(),
        generation: 1,
    }
}

fn map_definition() -> MapDefinition {
    MapDefinition {
        id: "each".to_owned(),
        worker: "worker".to_owned(),
        source: "items".to_owned(),
        item: "entity".to_owned(),
        index: "item_index".to_owned(),
        broadcast: vec!["prefix".to_owned()],
        outputs: vec!["item_result".to_owned()],
        destination: "mapped".to_owned(),
        max_items: 64,
        max_concurrency: 4,
        reduction: Some(MapReduction::OrderedCollection),
        policy: None,
    }
}

fn context() -> NodeContext {
    NodeContext::new(
        HashMap::from([
            ("items".to_owned(), json!(["a", {"b": 2}, [3]])),
            ("prefix".to_owned(), json!("frozen")),
        ]),
        ExecutionConfig::new("root-thread"),
        4,
    )
}

fn parallel_definition() -> ParallelNodeDefinition {
    ParallelNodeDefinition::from_yaml(
        r"
id: gather
type: parallel
branches:
  - id: short
    node: fetch_short
  - id: long
    node: fetch_long
max_concurrency: 2
wait: all
error_policy: fail_after_drain
output: [gathered]
transition: END
        ",
    )
    .expect("parallel fixture")
}

/// Main's Map child threads for this fixture, captured from main's
/// `map_child_thread_id` (`state/postgres_checkpointer/map_children.rs`)
/// before that function was deleted with the old Map path (T-2A3 commit 6).
const MAIN_MAP_CHILD_IDS: [&str; 3] = [
    "m1:V9KNIZ94knM1hI84jjLtU81ED2Ygrj1ckL411hO_DC8",
    "m1:bMN-UGLwU6pfF7LKL0jO9Vk1fPxi_dxeIKeUJGRubaI",
    "m1:dQAaGRsi0Bm0VqbydTEylnCKyqlwO799P5NT2XhlLEw",
];

/// Every item derives main's child thread through the shared adapter's
/// activation, children and inputs, with main's config, worker, source and
/// item-input digests.
#[test]
fn a_map_child_on_the_shared_adapter_keeps_mains_child_id() {
    let context = context();
    let definition = map_definition();
    let items = definition.source_items(&context.state).expect("items");
    let main_inputs = definition
        .item_inputs(&context.state, items)
        .expect("main's item inputs");
    let main_source = source_digest(items).expect("main's source digest");
    let main_config = definition.config_digest().expect("main's config digest");
    let source =
        FanoutSource::Map(MapSource::new(definition, WORKER_DIGEST).expect("the Map source"));
    let activation = ParallelActivation::from_context(&source, &context).expect("activation");
    let identity = activation
        .map
        .as_ref()
        .expect("a Map activation carries its identity");
    assert_eq!(identity.worker_digest, WORKER_DIGEST);
    assert_eq!(identity.source_digest, main_source);
    assert_eq!(activation.config_digest, main_config);
    let children = source.children(&context).expect("children");
    let inputs = source
        .frozen_inputs(&children, &context.state)
        .expect("a Map freezes its own inputs")
        .expect("inputs");
    assert_eq!(children.len(), 3);
    assert_eq!(inputs.len(), 3);
    for (index, item) in main_inputs.iter().enumerate() {
        assert_eq!(children[index].id(), index.to_string());
        assert_eq!(children[index].node(), "worker");
        assert_eq!(inputs[index].input_digest, item.input_digest);
        assert_eq!(inputs[index].input, item.input);
        assert_eq!(inputs[index].owned_definition_digest, WORKER_DIGEST);
        let derived = derive_child_thread_id(
            &writer(),
            &activation,
            &children[index],
            u64::try_from(index).expect("index"),
            &inputs[index].input_digest,
            &origin(),
        )
        .expect("the shared adapter's id");
        assert_eq!(derived, MAIN_MAP_CHILD_IDS[index]);
    }
}

/// A Parallel activation never carries the Map identity: it derives `p1:`
/// ids, stores no `map` field, and a stored Parallel activation reads back
/// without one.
#[test]
fn a_parallel_source_never_carries_the_map_identity() {
    let context = context();
    let definition = parallel_definition();
    let source = FanoutSource::Parallel(definition.clone());
    let activation = ParallelActivation::from_context(&source, &context).expect("activation");
    assert_eq!(activation.map, None);
    assert!(
        source
            .frozen_inputs(definition.branches(), &context.state)
            .is_none()
    );
    let stored = serde_json::to_value(&activation).expect("encode");
    assert!(stored.get("map").is_none(), "{stored}");
    assert_eq!(
        serde_json::from_value::<ParallelActivation>(stored).expect("decode"),
        activation
    );
    let derived = derive_child_thread_id(
        &writer(),
        &activation,
        &definition.branches()[0],
        0,
        &[0x42; 32],
        &origin(),
    )
    .expect("the Parallel id");
    assert!(derived.starts_with("p1:"), "{derived}");
}

/// A Map item the shared factory is handed must match the Map identity's
/// own shape; it never falls back to a Parallel id.
#[test]
fn a_map_activation_refuses_a_child_that_is_not_its_item() {
    let context = context();
    let source =
        FanoutSource::Map(MapSource::new(map_definition(), WORKER_DIGEST).expect("the Map source"));
    let activation = ParallelActivation::from_context(&source, &context).expect("activation");
    let parallel = parallel_definition();
    for (branch, ordinal) in [(&parallel.branches()[0], 0), (&parallel.branches()[1], 64)] {
        let error = derive_child_thread_id(
            &writer(),
            &activation,
            branch,
            ordinal,
            &[0x42; 32],
            &origin(),
        )
        .expect_err("a foreign child is refused");
        assert!(
            error.to_string().contains("checkpoint.invalid_scope"),
            "{error}"
        );
    }
}

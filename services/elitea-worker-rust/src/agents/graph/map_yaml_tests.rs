use elitea_agent_runtime::graph::fanout::plan::{JoinMode, OnError};
use ring::digest;
use serde::Serialize;

use super::{MAP_REDUCTION_AND_JOIN, MapNodeDefinition, MapYamlError};
use crate::agents::graph::map_reduce::{MapJoin, MapOnError, MapPolicy, MapReduction};

const NODE: &str = "id: map_entities\ntype: map\nworker: render\nsource: entities\nitem: entity\nindex: item_index\nbroadcast: [tone]\noutputs: [item_result]\ndestination: mapped\nmax_items: 64\nmax_concurrency: 4\n";

fn document(extra: &str) -> String {
    format!("{NODE}{extra}transition: END\n")
}

/// The definition exactly as main `0c41d5b65` serialized it: the v1 field
/// set in declaration order, with `reduction` always present.
#[derive(Serialize)]
struct V1Definition<'a> {
    id: &'a str,
    worker: &'a str,
    source: &'a str,
    item: &'a str,
    index: &'a str,
    broadcast: Vec<&'a str>,
    outputs: Vec<&'a str>,
    destination: &'a str,
    max_items: usize,
    max_concurrency: usize,
    reduction: MapReduction,
}

fn v1_digest() -> [u8; 32] {
    let definition = V1Definition {
        id: "map_entities",
        worker: "render",
        source: "entities",
        item: "entity",
        index: "item_index",
        broadcast: vec!["tone"],
        outputs: vec!["item_result"],
        destination: "mapped",
        max_items: 64,
        max_concurrency: 4,
        reduction: MapReduction::OrderedCollection,
    };
    let mut context = digest::Context::new(&digest::SHA256);
    context.update(b"elitea.graph.map.yaml.v1\0");
    context.update(&serde_json::to_vec(&definition).expect("serializes"));
    context.update(&[0]);
    context.update(b"END");
    let mut out = [0; 32];
    out.copy_from_slice(context.finish().as_ref());
    out
}

// M2: a saved v1 Map keeps its meaning and its digest, so its child
// identities, receipts and checkpoints stay the ones it was saved with.
#[test]
fn a_v1_map_keeps_its_meaning_and_its_digest() {
    let node = MapNodeDefinition::from_yaml(&document("reduction: ordered_collection\n"))
        .expect("the v1 shape still compiles");
    assert_eq!(node.runtime().policy, None);
    assert_eq!(node.runtime().on_error(), OnError::Stop);
    assert_eq!(node.runtime().join(), JoinMode::Append);
    assert_eq!(node.config_digest(), v1_digest());
}

#[test]
fn a_v2_map_defaults_to_continue_and_append_like_parallel() {
    let node = MapNodeDefinition::from_yaml(&document("")).expect("compiles");
    assert_eq!(node.runtime().reduction, None);
    assert_eq!(
        node.runtime().policy,
        Some(MapPolicy {
            on_error: MapOnError::Continue,
            join: MapJoin::Append
        })
    );
    assert_eq!(node.runtime().on_error(), OnError::Continue);
    assert_eq!(node.runtime().join(), JoinMode::Append);
    // A different meaning is a different definition, so it digests apart.
    assert_ne!(node.config_digest(), v1_digest());
}

#[test]
fn a_v2_map_reads_every_on_error_and_join() {
    for (extra, on_error, join) in [
        (
            "on_error: stop\njoin: append\n",
            OnError::Stop,
            JoinMode::Append,
        ),
        (
            "on_error: continue\njoin: keyed\n",
            OnError::Continue,
            JoinMode::Keyed,
        ),
        (
            "on_error: stop\njoin: merge\n",
            OnError::Stop,
            JoinMode::Merge,
        ),
    ] {
        let node = MapNodeDefinition::from_yaml(&document(extra)).expect(extra);
        assert_eq!(
            (node.runtime().on_error(), node.runtime().join()),
            (on_error, join),
            "{extra}"
        );
    }
}

#[test]
fn reduction_with_an_explicit_on_error_is_a_v2_append() {
    let node = MapNodeDefinition::from_yaml(&document(
        "reduction: ordered_collection\non_error: continue\n",
    ))
    .expect("compiles");
    assert_eq!(node.runtime().on_error(), OnError::Continue);
    assert_eq!(node.runtime().join(), JoinMode::Append);
    assert_ne!(node.config_digest(), v1_digest());
}

// M2: a document that could mean two collections is refused readably,
// never resolved silently.
#[test]
fn reduction_and_join_together_are_refused_with_the_fix() {
    let error =
        MapNodeDefinition::from_yaml(&document("reduction: ordered_collection\njoin: keyed\n"))
            .err()
            .expect("refused");
    assert_eq!(error, MapYamlError::ReductionAndJoin);
    assert!(MAP_REDUCTION_AND_JOIN.contains("remove `reduction`"));
}

#[test]
fn an_unknown_on_error_join_or_reduction_is_invalid() {
    for extra in [
        "on_error: retry\n",
        "join: union\n",
        "reduction: infer\n",
        "on_error: [stop]\n",
    ] {
        assert_eq!(
            MapNodeDefinition::from_yaml(&document(extra)).err(),
            Some(MapYamlError::Invalid),
            "{extra}"
        );
    }
}

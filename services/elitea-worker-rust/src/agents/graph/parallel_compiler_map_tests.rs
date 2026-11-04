//! T-2A3 B2a: a Map's State Modifier worker in the shared branch factory.
//! Every item resolves to the one owned worker, is digested as main's Map
//! worker, reads only its declared inputs, and lands its outputs typed.

use std::collections::BTreeMap;
use std::sync::Arc;

use adk_rust::graph::State;
use serde_json::json;

use super::{CompilerParallelBranches, OwnedBranch};
use crate::agents::graph::ParallelBranchDefinition;
use crate::agents::graph::parallel::{ParallelBranchGraphFactory, ParallelBranchTerminal};
use crate::agents::graph::state_modifier::StateModifierNodeDefinition;

fn worker() -> StateModifierNodeDefinition {
    StateModifierNodeDefinition::from_yaml(
        r"
id: render
type: state_modifier
template: '{{ entity }}-{{ item_index }}'
input: [entity, item_index]
output: [item_result]
",
    )
    .expect("worker fixture")
}

fn factory(items: usize) -> CompilerParallelBranches {
    CompilerParallelBranches {
        owned: (0..items)
            .map(|index| (index.to_string(), OwnedBranch::StateModifier(worker())))
            .collect(),
        state_types: BTreeMap::from([("item_result".to_owned(), "str".to_owned())]),
        resolver: None,
        llm: None,
        events: None,
        root_session: None,
    }
}

#[test]
fn a_map_state_modifier_item_resolves_to_its_worker_with_mains_worker_digest() {
    let factory = factory(3);
    for index in 0..3 {
        let item = ParallelBranchDefinition::map_item(index, "render");
        factory.validate_branch(&item).expect("an admitted item");
        assert_eq!(
            factory.owned_definition_digest(&item).expect("digest"),
            worker().config_digest()
        );
    }
    for foreign in [
        ParallelBranchDefinition::map_item(3, "render"),
        ParallelBranchDefinition::map_item(0, "other"),
    ] {
        assert!(factory.validate_branch(&foreign).is_err());
    }
}

#[test]
fn a_map_state_modifier_item_reads_only_its_declared_inputs() {
    let factory = factory(1);
    let item = ParallelBranchDefinition::map_item(0, "render");
    let input = State::from([
        ("entity".to_owned(), json!("a")),
        ("item_index".to_owned(), json!(0)),
        (
            "prefix".to_owned(),
            json!("broadcast the worker does not read"),
        ),
    ]);
    assert_eq!(
        factory.project_input(&item, &input).expect("projection"),
        State::from([
            ("entity".to_owned(), json!("a")),
            ("item_index".to_owned(), json!(0)),
        ])
    );
    let missing = State::from([("item_index".to_owned(), json!(0))]);
    assert!(factory.project_input(&item, &missing).is_err());
}

#[test]
fn a_map_state_modifier_item_lands_its_typed_outputs() {
    let factory = factory(1);
    let item = ParallelBranchDefinition::map_item(0, "render");
    let child = State::from([
        ("entity".to_owned(), json!("a")),
        ("item_index".to_owned(), json!(0)),
        ("item_result".to_owned(), json!("a-0")),
    ]);
    let Ok(ParallelBranchTerminal::Completed(outputs)) = factory.project_result(&item, &child)
    else {
        panic!("a completed item");
    };
    assert_eq!(
        outputs,
        serde_json::Map::from_iter([("item_result".to_owned(), json!("a-0"))])
    );
    let wrong = State::from([("item_result".to_owned(), json!(7))]);
    assert!(factory.project_result(&item, &wrong).is_err());
    let _ = Arc::new(factory);
}

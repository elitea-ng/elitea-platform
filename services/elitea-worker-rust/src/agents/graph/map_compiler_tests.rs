//! Map save-time validation: ownership, item channels and worker kinds. Map execution is proven on the shared fan-out
//! adapter (`map_fanout_tests.rs`, `parallel_compiler_map_tests.rs`).
#![allow(clippy::unwrap_used, clippy::expect_used)]

use std::collections::BTreeSet;

use super::*;

fn document() -> String {
    "entry_point: map_entities\nstate:\n  entities: {type: list, value: []}\n  item_result: {type: str, value: ''}\n  mapped: {type: list, value: []}\nnodes:\n  - {id: map_entities, type: map, worker: render, source: entities, item: entity, index: item_index, outputs: [item_result], destination: mapped, max_items: 64, max_concurrency: 4, reduction: ordered_collection, transition: END}\n  - {id: render, type: state_modifier, input: [entity, item_index], output: [item_result], template: '{{ item_index }}'}\n".to_owned()
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
    assert!(
        !definition.recovery_frontier_supported(
            &["render".to_owned()],
            &std::collections::BTreeSet::new()
        )
    );
    assert!(definition.recovery_frontier_supported(
        &["map_entities".to_owned()],
        &std::collections::BTreeSet::new()
    ));
}

#[test]
fn map_saved_agent_rejects_deep_fixed_value_before_definition_hashing() {
    let deep = format!("{}null{}", "[".repeat(96), "]".repeat(96));
    let yaml = document().replace("type: state_modifier, input: [entity, item_index], output: [item_result], template: '{{ item_index }}'", &format!("type: agent, tool: selected, input: [entity, item_index], input_mapping: {{task: {{type: fixed, value: render}}, opaque: {{type: fixed, value: {deep}}}}}, output: [item_result]"));
    assert!(PipelineDefinition::from_yaml(&yaml).is_err());
}

#[test]
fn map_worker_kind_refusal_is_typed_and_names_the_node() {
    let yaml = document().replace(
        "type: state_modifier, input: [entity, item_index], output: [item_result], template: '{{ item_index }}'",
        "type: router, condition: 'END', routes: [END], default_output: END, input: [entity, item_index]",
    );
    match PipelineDefinition::from_yaml(&yaml) {
        Err(error @ PipelineConfigurationError::FanoutChildUnsupported { .. }) => {
            assert_eq!(error.code(), "graph.fanout.child_kind_unsupported");
            assert!(error.to_string().contains("render"), "{error}");
        }
        Err(other) => panic!("expected a typed child-kind refusal, got {other:?}"),
        Ok(_) => panic!("an unadmitted child kind compiled"),
    }
}

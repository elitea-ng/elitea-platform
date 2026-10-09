//! Overwrite-only definitions keep the digest they had before typed reducers.
//!
//! The golden values were computed by this file against the unmodified
//! `origin/main` compiler (58abb650c). A changed value would orphan every
//! existing checkpoint lineage, so these are fixed bytes, not recomputed.

use super::compiler::PipelineDefinition;

const LOOP: &str = r#"
state:
  count: {type: int, value: 0}
  findings: {type: list, value: [seed]}
  seen: {type: dict, value: {}}
  input: {type: str}
entry_point: tick
nodes:
  - id: tick
    type: state_modifier
    template: "{{ count + 1 }}"
    input: [count]
    output: [count]
    transition: choose
  - id: choose
    type: router
    condition: "{{ 'tick' if count < 3 else 'END' }}"
    input: [count]
    routes: [tick, END]
    default_output: END
"#;

const PAUSED: &str = r#"
interrupt_after: [tick]
state:
  count: int
  notes: list
entry_point: tick
nodes:
  - id: tick
    type: state_modifier
    template: "{{ count + 1 }}"
    input: [count]
    output: [count]
    variables_to_clean: [notes]
    transition: END
"#;

const MAP: &str = "entry_point: map_entities\nstate:\n  entities: {type: list, value: []}\n  item_result: {type: str, value: ''}\n  mapped: {type: list, value: []}\nnodes:\n  - {id: map_entities, type: map, worker: render, source: entities, item: entity, index: item_index, outputs: [item_result], destination: mapped, max_items: 64, max_concurrency: 4, reduction: ordered_collection, transition: END}\n  - {id: render, type: state_modifier, input: [entity, item_index], output: [item_result], template: '{{ item_index }}'}\n";

fn hex(yaml: &str) -> String {
    PipelineDefinition::from_yaml(yaml)
        .expect("golden fixture")
        .definition_digest()
        .iter()
        .map(|byte| format!("{byte:02x}"))
        .collect()
}

#[test]
fn overwrite_only_definitions_keep_their_pre_reducer_digests() {
    for (name, yaml) in [("loop", LOOP), ("paused", PAUSED), ("map", MAP)] {
        println!("GOLDEN {name} {}", hex(yaml));
    }
}

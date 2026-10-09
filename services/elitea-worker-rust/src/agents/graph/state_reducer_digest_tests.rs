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

const GOLDEN: [(&str, &str, &str); 3] = [
    (
        "loop",
        LOOP,
        "03879059de057d7fdc445657465f1bab128a52c95a4c9424da468e9132fea38c",
    ),
    (
        "paused",
        PAUSED,
        "396e3129030776807f742f1c88361c0e63410d3b55550430cf135d5089478663",
    ),
    (
        "map",
        MAP,
        "63527fd0a1e5d8c2473f82f42576e48d85123cb566a2d8b9925d39f28a1c2a59",
    ),
];

#[test]
fn overwrite_only_definitions_keep_their_pre_reducer_digests() {
    for (name, yaml, golden) in GOLDEN {
        assert_eq!(hex(yaml), golden, "{name}");
    }
}

/// An explicit `reducer: overwrite` is the default, not a new definition.
#[cfg(feature = "graph-extensions-rehearsal")]
#[test]
fn explicit_overwrite_keeps_the_golden_digest_and_typed_reducers_change_it() {
    let explicit = LOOP
        .replace(
            "count: {type: int, value: 0}",
            "count: {type: int, value: 0, reducer: overwrite}",
        )
        .replace("seen: {type: dict, value: {}}", "seen: {type: dict, value: {}, reducer: overwrite}");
    assert_eq!(hex(&explicit), GOLDEN[0].2);
    let append = LOOP.replace(
        "findings: {type: list, value: [seed]}",
        "findings: {type: list, value: [seed], reducer: append}",
    );
    let merge = LOOP.replace(
        "seen: {type: dict, value: {}}",
        "seen: {type: dict, value: {}, reducer: merge}",
    );
    let sum = LOOP.replace(
        "count: {type: int, value: 0}",
        "count: {type: int, value: 0, reducer: sum_int}",
    );
    let digests = [hex(&append), hex(&merge), hex(&sum)];
    for digest in &digests {
        assert_ne!(digest, GOLDEN[0].2);
    }
    assert_ne!(digests[0], digests[1]);
    assert_ne!(digests[1], digests[2]);
    assert_eq!(hex(&append), digests[0], "the fold is deterministic");
}

//! N2 (W3-2): the fan-out child families parity. The compiler admits exactly
//! the families of `libs/proto/contracts/pipeline-fanout-child-families-v1.json`
//! as direct Map workers and Parallel branches; Main's save check and the Web
//! editor assert the same file (next to the node-admission contract).

use std::collections::BTreeSet;

const MAP_COMPILER: &str = include_str!("map_compiler.rs");
const PARALLEL_COMPILER: &str = include_str!("parallel_compiler.rs");

fn contract() -> serde_json::Value {
    let path = std::path::Path::new(env!("CARGO_MANIFEST_DIR"))
        .join("../../libs/proto/contracts/pipeline-fanout-child-families-v1.json");
    serde_json::from_str(&std::fs::read_to_string(path).expect("the families contract"))
        .expect("the families contract is JSON")
}

fn family(contract: &serde_json::Value, key: &str) -> BTreeSet<String> {
    contract[key]
        .as_array()
        .expect("a family list")
        .iter()
        .map(|value| value.as_str().expect("a node type").to_owned())
        .collect()
}

/// The YAML `type:` of a compiler node variant.
fn yaml_type(variant: &str) -> String {
    match variant {
        "Application" => "agent".to_owned(),
        "Llm" => "llm".to_owned(),
        "StateModifier" => "state_modifier".to_owned(),
        "Code" => "code".to_owned(),
        other => panic!("no YAML type mapped for {other}"),
    }
}

/// The `PipelineNodeDefinition::X` variants named in `source` between the
/// two markers.
fn variants(source: &str, start: &str, end: &str) -> BTreeSet<String> {
    let from = source.find(start).expect("start marker") + start.len();
    let body = &source[from..];
    let body = &body[..body.find(end).expect("end marker")];
    body.split("PipelineNodeDefinition::")
        .skip(1)
        .filter_map(|rest| rest.split_once('(').map(|(name, _)| name.to_owned()))
        .filter(|name| name != "Router")
        .map(|name| yaml_type(&name))
        .collect()
}

#[test]
fn the_compiler_admits_exactly_the_contract_fanout_families() {
    let contract = contract();
    assert_eq!(
        variants(MAP_COMPILER, "match worker {", "// Readable and typed"),
        family(&contract, "map_worker"),
        "Map worker families differ from the contract"
    );
    assert_eq!(
        variants(
            PARALLEL_COMPILER,
            "let (id, transition) = match nodes.iter().find(|node| node.id() == branch.node()) {",
            "// Other kinds run as branches",
        ),
        family(&contract, "parallel_branch"),
        "Parallel branch families differ from the contract"
    );
    assert_eq!(
        family(&contract, "refused_with_own_text"),
        BTreeSet::from(["router".to_owned()])
    );
    for source in [MAP_COMPILER, PARALLEL_COMPILER] {
        assert!(source.contains("refusal: FanoutChildRefusal::Router"));
    }
}

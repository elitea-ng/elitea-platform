//! Node-type admission parity: the Worker compiler admits exactly the node types
//! of the shared contract `libs/proto/contracts/pipeline-node-admission-v1.json`.
//!
//! Main's save check and the Web editor assert the same file, so a node type
//! added to one layer only fails there. The pending Wave 2a drift (`parallel`,
//! `map`) is recorded in the file and compared exactly, so new drift fails too.

use std::collections::BTreeSet;

use serde_json::Value;

use super::compiler::node_type_has_parse_arm;

const COMPILER_SOURCE: &str = include_str!("compiler.rs");

fn contract() -> Value {
    let path = std::path::Path::new(env!("CARGO_MANIFEST_DIR"))
        .join("../../libs/proto/contracts/pipeline-node-admission-v1.json");
    let text = std::fs::read_to_string(&path).expect("the shared admission contract is readable");
    serde_json::from_str(&text).expect("the shared admission contract is JSON")
}

fn names(value: &Value) -> BTreeSet<String> {
    value
        .as_array()
        .expect("a list of node types")
        .iter()
        .map(|entry| entry.as_str().expect("a node type name").to_owned())
        .collect()
}

/// The `type:` literals of the match arms of `parse_pipeline_node`, read from the
/// compiler source so a new arm cannot hide from the contract.
fn parse_arms() -> BTreeSet<String> {
    let start = COMPILER_SOURCE
        .find("\nfn parse_pipeline_node(")
        .expect("parse_pipeline_node");
    let body = &COMPILER_SOURCE[start..];
    let end = body
        .find("\nconst fn node_limit(")
        .expect("end of parse_pipeline_node");
    let mut arms = BTreeSet::new();
    for line in body[..end].lines() {
        // A match arm of the node-type switch sits at exactly eight spaces.
        let Some(rest) = line.strip_prefix("        \"") else {
            continue;
        };
        let Some((patterns, _)) = rest.split_once(" =>") else {
            continue;
        };
        for pattern in format!("\"{patterns}").split(" | ") {
            arms.insert(pattern.trim_matches('"').to_owned());
        }
    }
    arms
}

#[test]
fn the_compiler_admits_exactly_the_contract_node_types() {
    let contract = contract();
    let admitted = names(&contract["admitted"]);
    let pending = names(&contract["pending_wave_2a"]["worker"]);
    let refused = names(&contract["refused"]);

    assert!(admitted.is_disjoint(&pending), "pending overlaps admitted");
    assert!(admitted.is_disjoint(&refused), "refused overlaps admitted");
    assert!(pending.is_disjoint(&refused), "refused overlaps pending");

    let expected: BTreeSet<String> = admitted.union(&pending).cloned().collect();
    assert_eq!(
        parse_arms(),
        expected,
        "compiler arms differ from the contract"
    );

    for node_type in &expected {
        assert!(
            node_type_has_parse_arm(node_type),
            "{node_type} has no parse arm"
        );
    }
    for node_type in &refused {
        assert!(
            !node_type_has_parse_arm(node_type),
            "{node_type} must stay refused"
        );
    }
}

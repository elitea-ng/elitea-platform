//! Named, data-free causes for the pipeline size and count bounds.
//!
//! Each bound is proven at the limit and at limit + 1, and every refusal is
//! checked to carry the limit name but never the document content.

use std::fmt::Write as _;

use super::compiler::{MAX_PIPELINE_YAML_BYTES, PipelineDefinition};

const YAML_BYTES: &str = "graph.pipeline.yaml_bytes_exceeded";
const NODE_COUNT: &str = "graph.pipeline.node_count_exceeded";
const NODE_LIMIT: &str = "graph.pipeline.node_limit_exceeded";
const NODE_YAML_CAP: usize = 64 * 1024;
const SENTINEL: &str = "SENTINELPRIVATEPAYLOAD";

const SMALL: &str = "entry_point: a\nnodes:\n  - id: a\n    type: state_modifier\n    template: x\n    transition: END\n";

/// Pad with a trailing YAML comment so the document is exactly `total` bytes.
fn padded_to(base: &str, total: usize) -> String {
    let comment_overhead = "# \n".len();
    let padding = total
        .checked_sub(base.len() + comment_overhead)
        .expect("base document fits");
    let document = format!("{base}# {}\n", "p".repeat(padding));
    assert_eq!(document.len(), total);
    document
}

fn assert_data_free(error: &super::compiler::PipelineConfigurationError) {
    for rendered in [error.to_string(), format!("{error:?}")] {
        assert!(!rendered.contains(SENTINEL), "content leaked: {rendered}");
        assert!(
            !rendered.contains("entry_point"),
            "content leaked: {rendered}"
        );
    }
}

#[test]
fn yaml_document_bound_is_named_at_limit_and_limit_plus_one() {
    let at_limit = padded_to(SMALL, MAX_PIPELINE_YAML_BYTES);
    PipelineDefinition::from_yaml(&at_limit).expect("a document exactly at the bound is admitted");

    let over = padded_to(SMALL, MAX_PIPELINE_YAML_BYTES + 1);
    let Err(error) = PipelineDefinition::from_yaml(&over) else {
        panic!("a document one byte over the bound was admitted");
    };
    assert_eq!(error.code(), YAML_BYTES);
    assert!(error.to_string().contains("yaml_bytes"), "{error}");
    assert_data_free(&error);
}

#[test]
fn empty_document_is_invalid_not_a_size_limit() {
    let Err(error) = PipelineDefinition::from_yaml("") else {
        panic!("an empty pipeline was admitted");
    };
    assert_eq!(error.code(), "graph.pipeline.invalid_configuration");
}

fn chain(nodes: usize) -> String {
    let mut body = String::new();
    for index in 0..nodes {
        let transition = if index + 1 == nodes {
            "END".to_owned()
        } else {
            format!("n{}", index + 1)
        };
        write!(
            body,
            "  - id: n{index}\n    type: state_modifier\n    template: x\n    transition: {transition}\n"
        )
        .expect("write to string");
    }
    format!("entry_point: n0\nnodes:\n{body}")
}

#[test]
fn node_count_bound_is_named_at_limit_and_limit_plus_one() {
    PipelineDefinition::from_yaml(&chain(128)).expect("128 nodes are admitted");

    let Err(error) = PipelineDefinition::from_yaml(&chain(129)) else {
        panic!("129 nodes were admitted");
    };
    assert_eq!(error.code(), NODE_COUNT);
    assert!(error.to_string().contains("node_count"), "{error}");
    assert_data_free(&error);

    // A zero-node document is not a size problem.
    let Err(error) = PipelineDefinition::from_yaml("entry_point: a\nnodes: []\n") else {
        panic!("an empty node list was admitted");
    };
    assert_eq!(error.code(), "graph.pipeline.invalid_configuration");
}

/// Byte length of the node as the compiler re-encodes it for its family parser.
fn encoded_node_len(yaml: &str) -> usize {
    let document: serde_yaml_ng::Value = serde_yaml_ng::from_str(yaml).expect("fixture yaml");
    let node = &document["nodes"][0];
    serde_yaml_ng::to_string(node).expect("node encoding").len()
}

#[test]
fn per_node_size_bound_is_named_at_limit_and_limit_plus_one() {
    let render = |filler: usize| {
        format!(
            "entry_point: a\nnodes:\n  - id: a\n    type: state_modifier\n    template: {}\n    transition: END\n",
            "x".repeat(filler)
        )
    };
    // An empty template is encoded quoted, so measure the plain-scalar slope from one byte.
    let base = encoded_node_len(&render(1)) - 1;
    let at_limit = render(NODE_YAML_CAP - base);
    assert_eq!(encoded_node_len(&at_limit), NODE_YAML_CAP);
    PipelineDefinition::from_yaml(&at_limit).expect("a node exactly at its bound is admitted");

    let over = render(NODE_YAML_CAP - base + 1);
    assert_eq!(encoded_node_len(&over), NODE_YAML_CAP + 1);
    let Err(error) = PipelineDefinition::from_yaml(&over) else {
        panic!("a node one byte over its bound was admitted");
    };
    assert_eq!(error.code(), NODE_LIMIT);
    assert!(
        error.to_string().contains("nodes[].state_modifier"),
        "{error}"
    );
}

#[test]
fn oversized_nodes_of_other_families_name_their_own_family() {
    let filler = SENTINEL.repeat(NODE_YAML_CAP / SENTINEL.len() + 8);
    let cases = [
        (
            format!(
                "entry_point: a\nnodes:\n  - id: a\n    type: llm\n    input_mapping:\n      task: {{type: fixed, value: {filler}}}\n    transition: END\n"
            ),
            "nodes[].llm",
        ),
        (
            format!(
                "entry_point: a\nnodes:\n  - id: a\n    type: decision\n    nodes: [END]\n    description: {filler}\n"
            ),
            "nodes[].decision",
        ),
        (
            format!(
                "entry_point: a\nnodes:\n  - id: a\n    type: hitl\n    user_message: {{type: fixed, value: {filler}}}\n    routes:\n      approve: END\n"
            ),
            "nodes[].hitl",
        ),
        (
            format!(
                "entry_point: a\nnodes:\n  - id: a\n    type: agent\n    tool: Helper\n    input_mapping:\n      task: {{type: fixed, value: {filler}}}\n    transition: END\n"
            ),
            "nodes[].agent",
        ),
        (
            format!(
                "entry_point: a\nnodes:\n  - id: a\n    type: router\n    condition: {filler}\n    routes: [END]\n"
            ),
            "nodes[].router",
        ),
    ];
    for (yaml, family) in cases {
        assert!(yaml.len() < MAX_PIPELINE_YAML_BYTES);
        let Err(error) = PipelineDefinition::from_yaml(&yaml) else {
            panic!("an oversized {family} node was admitted");
        };
        assert_eq!(error.code(), NODE_LIMIT, "{family}");
        assert!(error.to_string().contains(family), "{error}");
        assert_data_free(&error);
    }
}

#[test]
fn per_node_entry_count_bound_uses_the_same_named_cause() {
    // 65 inputs exceed the router's 64-entry bound without exceeding any byte bound.
    let inputs = (0..65)
        .map(|n| format!("k{n}"))
        .collect::<Vec<_>>()
        .join(", ");
    let yaml = format!(
        "entry_point: a\nnodes:\n  - id: a\n    type: router\n    condition: x\n    routes: [END]\n    input: [{inputs}]\n"
    );
    let Err(error) = PipelineDefinition::from_yaml(&yaml) else {
        panic!("a router with too many inputs was admitted");
    };
    assert_eq!(error.code(), NODE_LIMIT);
    assert!(error.to_string().contains("nodes[].router"), "{error}");
}

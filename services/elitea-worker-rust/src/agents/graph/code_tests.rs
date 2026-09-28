use std::collections::BTreeMap;

use adk_rust::graph::State;
use serde_json::json;

use super::code::{CodeLanguage, CodeNodeDefinition, CodeProvenance};
use super::compiler::{PipelineConfigurationError, PipelineDefinition};

const NODE: &str = "id: execute\ntype: code\ncode: {type: fixed, value: 'result = 1'}\ninput: [messages]\noutput: [messages]\nstructured_output: false\ndebug: true\ntransition: END\n";

#[test]
fn legacy_code_defaults_to_python_and_preserves_literal_source() {
    let node = CodeNodeDefinition::from_yaml(NODE).expect("legacy YAML");
    assert_eq!(node.language(), CodeLanguage::Python);
    let state = State::new();
    let resolved = node
        .resolve_source(&state, &BTreeMap::new())
        .expect("source");
    assert_eq!(resolved.source, "result = 1");
    assert_eq!(resolved.provenance, CodeProvenance::SavedLiteral);
}

#[test]
fn all_languages_have_distinct_configuration_identity() {
    let mut digests = std::collections::BTreeSet::new();
    for language in ["python", "javascript", "typescript", "rust"] {
        let node = CodeNodeDefinition::from_yaml(&format!("{NODE}language: {language}\n"))
            .expect("language");
        assert!(digests.insert(node.config_digest().expect("digest")));
    }
    let implicit = CodeNodeDefinition::from_yaml(NODE).expect("implicit");
    let explicit =
        CodeNodeDefinition::from_yaml(&format!("{NODE}language: python\n")).expect("explicit");
    assert_eq!(implicit.config_digest(), explicit.config_digest());
}

#[test]
fn legacy_bare_source_normalizes_to_the_same_fixed_mapping() {
    let bare = NODE.replace("{type: fixed, value: 'result = 1'}", "'result = 1'");
    let bare = CodeNodeDefinition::from_yaml(&bare).expect("legacy bare source");
    let mapped = CodeNodeDefinition::from_yaml(NODE).expect("mapped source");
    assert_eq!(bare.config_digest(), mapped.config_digest());
}

#[test]
fn variable_source_retains_dynamic_provenance_and_requires_declared_string() {
    let yaml = NODE.replace(
        "type: fixed, value: 'result = 1'",
        "type: variable, value: source",
    );
    let node = CodeNodeDefinition::from_yaml(&yaml).expect("mapping");
    let state = State::from([("source".into(), json!("result = 2"))]);
    assert!(node.resolve_source(&state, &BTreeMap::new()).is_err());
    let types = BTreeMap::from([("source".into(), "str".into())]);
    let resolved = node.resolve_source(&state, &types).expect("source");
    assert_eq!(resolved.source, "result = 2");
    assert_eq!(resolved.provenance, CodeProvenance::StateVariable);
    assert!(node.resolve_source(&State::new(), &types).is_err());
    assert!(
        node.resolve_source(&State::from([("source".into(), json!(3))]), &types)
            .is_err()
    );
}

#[test]
fn code_source_bounds_apply_to_fixed_and_dynamic_values() {
    let source = "x".repeat(160 * 1024);
    let node = CodeNodeDefinition::from_yaml(&NODE.replace("result = 1", &source))
        .expect("large valid source");
    let state = State::new();
    assert_eq!(
        node.resolve_source(&state, &BTreeMap::new())
            .expect("source")
            .source
            .len(),
        source.len()
    );
    let too_large = "x".repeat(256 * 1024 + 1);
    assert!(CodeNodeDefinition::from_yaml(&NODE.replace("result = 1", &too_large)).is_err());
    let dynamic = CodeNodeDefinition::from_yaml(&NODE.replace(
        "type: fixed, value: 'result = 1'",
        "type: variable, value: input",
    ))
    .expect("mapping");
    assert!(
        dynamic
            .resolve_source(
                &State::from([("input".into(), json!(too_large))]),
                &BTreeMap::new()
            )
            .is_err()
    );
}

#[test]
fn malformed_configuration_fails_without_echoing_source() {
    for yaml in [
        format!("{NODE}language: shell\n"),
        format!("{NODE}secret_field: PRIVATE_MARKER\n"),
        NODE.replace("input: [messages]", "input: [input, input]"),
        NODE.replace(
            "type: fixed, value: 'result = 1'",
            "type: variable, value: session_id",
        ),
        NODE.replace("'result = 1'", "''"),
        NODE.replace("type: fixed", "type: command"),
    ] {
        let error = CodeNodeDefinition::from_yaml(&yaml)
            .err()
            .expect("rejected");
        assert!(!error.contains("PRIVATE_MARKER"));
    }
}

#[test]
fn valid_code_still_cannot_execute_without_sandbox_admission() {
    let yaml = format!(
        "entry_point: execute\nnodes:\n{}",
        NODE.lines()
            .enumerate()
            .map(|(index, line)| {
                if index == 0 {
                    format!("  - {line}\n")
                } else {
                    format!("    {line}\n")
                }
            })
            .collect::<String>()
    );
    assert!(matches!(
        PipelineDefinition::from_yaml(&yaml),
        Err(PipelineConfigurationError::Unsupported(_))
    ));
}

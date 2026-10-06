use std::{collections::BTreeMap, fmt::Write};

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
    assert_eq!(resolved.source(), "result = 1");
    assert_eq!(resolved.provenance(), CodeProvenance::SavedLiteral);
    assert_eq!(node.dependencies_toml(), None);
}

#[test]
fn absent_dependencies_preserve_existing_serialized_bytes_and_digests() {
    let serialized = r#"{"id":"execute","type":"code","language":"python","code":{"type":"fixed","value":"result = 1"},"input":["messages"],"output":["messages"],"structured_output":false,"debug":true,"transition":"END"}"#;
    for (language, expected_digest) in [
        (
            "python",
            "2cd5a90916aa445e46445d70d38a6171ce8e8766393ca1039cd9898a3ac79e1e",
        ),
        (
            "rust",
            "93d71a5644ada7e22ff7dd729e47b577de77726d72f5040d5ff64c62508b0d69",
        ),
    ] {
        let node = CodeNodeDefinition::from_yaml(&format!("{NODE}language: {language}\n"))
            .expect("existing configuration");
        assert_eq!(
            serde_json::to_string(&node).unwrap(),
            serialized.replace("\"python\"", &format!("\"{language}\""))
        );
        let mut digest = String::new();
        for byte in node.config_digest().unwrap() {
            write!(&mut digest, "{byte:02x}").unwrap();
        }
        assert_eq!(digest, expected_digest);
    }
}

fn rust_dependencies_yaml(declaration: &str) -> String {
    format!(
        "{NODE}language: rust\ndependencies: {}\n",
        serde_json::to_string(declaration).unwrap()
    )
}

#[test]
fn rust_registry_dependencies_preserve_saved_text_and_configuration_identity() {
    let declaration = "# Saved Cargo declaration\n[dependencies]\nregex = \"1\"\nrenamed = { package = \"csv\", version = \"1\", default-features = false, features = [\"default\"] }\n[dependencies.itoa]\nversion = \"1\"\n";
    let node = CodeNodeDefinition::from_yaml(&rust_dependencies_yaml(declaration))
        .expect("native Cargo dependency table");
    assert_eq!(node.dependencies_toml(), Some(declaration));
    assert_eq!(
        serde_json::to_value(&node).unwrap()["dependencies"],
        declaration
    );
    let changed = CodeNodeDefinition::from_yaml(&rust_dependencies_yaml(
        &declaration.replace("regex = \"1\"", "regex = \"2\""),
    ))
    .expect("changed registry declaration");
    let absent = CodeNodeDefinition::from_yaml(&format!("{NODE}language: rust\n")).unwrap();
    assert_ne!(node.config_digest(), changed.config_digest());
    assert_ne!(node.config_digest(), absent.config_digest());
}

#[test]
fn dependency_declarations_require_explicit_rust_and_string_values() {
    let declaration = serde_json::to_string("[dependencies]\nregex = \"1\"\n").unwrap();
    for suffix in [
        format!("dependencies: {declaration}\n"),
        format!("language: python\ndependencies: {declaration}\n"),
        format!("language: javascript\ndependencies: {declaration}\n"),
        format!("language: typescript\ndependencies: {declaration}\n"),
        "language: rust\ndependencies: null\n".into(),
        "language: rust\ndependencies: [regex]\n".into(),
        "language: rust\ndependencies: {regex: '1'}\n".into(),
        "language: rust\ndependencies: true\n".into(),
    ] {
        assert!(CodeNodeDefinition::from_yaml(&format!("{NODE}{suffix}")).is_err());
    }
}

#[test]
fn invalid_or_unsupported_cargo_declarations_fail_without_echoing_values() {
    for declaration in [
        "",
        "[dependencies]\n",
        "[dependencies\nPRIVATE_MARKER",
        "[package]\nname = \"PRIVATE_MARKER\"\n[dependencies]\nregex = \"1\"",
        "[dependencies]\nregex = \"1\"\n[build-dependencies]\ncc = \"1\"",
        "[dependencies]\nregex = \"1\"\n[patch.crates-io]\nregex = { path = \"PRIVATE_MARKER\" }",
        "[dependencies]\nregex = { path = \"PRIVATE_MARKER\" }",
        "[dependencies]\nregex = { version = \"1\", git = \"PRIVATE_MARKER\" }",
        "[dependencies]\nregex = { version = \"1\", registry = \"PRIVATE_MARKER\" }",
        "[dependencies]\nregex = { version = \"1\", workspace = true }",
        "[dependencies]\nregex = { version = \"1\", optional = true }",
        "[dependencies]\nregex = { version = 1 }",
        "[dependencies]\nregex = { version = \"1\", features = \"default\" }",
        "[dependencies]\nregex = { version = \"1\", features = [1] }",
        "[dependencies]\nregex = { version = \"1\", default-features = \"false\" }",
        "[dependencies]\nregex = []",
        "[dependencies]\nregex = \"\"",
        "[dependencies]\nregex = \"1\"\nregex = \"2\"",
        "[dependencies]\nserde_json = \"1\"",
        "[dependencies]\nserde-json = \"1\"",
        "[dependencies]\nrenamed = { package = \"serde_json\", version = \"1\" }",
        "[dependencies]\n\"PRIVATE_MARKER.name\" = \"1\"",
        "[dependencies]\nregex = \"1\\u0000PRIVATE_MARKER\"",
    ] {
        let error = CodeNodeDefinition::from_yaml(&rust_dependencies_yaml(declaration))
            .err()
            .expect("declaration rejected");
        assert!(!error.contains("PRIVATE_MARKER"));
    }
}

#[test]
fn cargo_declaration_size_and_count_limits_apply_before_execution() {
    let mut declaration = "[dependencies]\nregex = \"1\"\n#".to_owned();
    declaration.push_str(&"x".repeat(64 * 1024 - declaration.len()));
    assert!(CodeNodeDefinition::from_yaml(&rust_dependencies_yaml(&declaration)).is_ok());
    declaration.push('x');
    assert!(CodeNodeDefinition::from_yaml(&rust_dependencies_yaml(&declaration)).is_err());

    let mut dependencies = "[dependencies]\n".to_owned();
    for index in 0..128 {
        writeln!(&mut dependencies, "crate_{index} = \"1\"").unwrap();
    }
    assert!(CodeNodeDefinition::from_yaml(&rust_dependencies_yaml(&dependencies)).is_ok());
    dependencies.push_str("crate_extra = \"1\"\n");
    assert!(CodeNodeDefinition::from_yaml(&rust_dependencies_yaml(&dependencies)).is_err());

    let features = (0..129)
        .map(|index| format!("\"feature{index}\""))
        .collect::<Vec<_>>()
        .join(",");
    let too_many_features =
        format!("[dependencies]\nregex = {{ version = \"1\", features = [{features}] }}\n");
    assert!(CodeNodeDefinition::from_yaml(&rust_dependencies_yaml(&too_many_features)).is_err());
    let oversized_version = format!("[dependencies]\nregex = \"{}\"\n", "x".repeat(257));
    assert!(CodeNodeDefinition::from_yaml(&rust_dependencies_yaml(&oversized_version)).is_err());
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
    assert_eq!(resolved.source(), "result = 2");
    assert_eq!(resolved.provenance(), CodeProvenance::StateVariable);
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
            .source()
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
        PipelineDefinition::from_yaml(&yaml)
            .expect("valid Code definition")
            .compile(
                "test",
                std::sync::Arc::new(adk_rust::graph::MemoryCheckpointer::new()),
                None
            ),
        Err(PipelineConfigurationError::Unsupported(_))
    ));
}

fn mapped_source_yaml(kind: &str, value: &str) -> String {
    NODE.replace(
        "{type: fixed, value: 'result = 1'}",
        &json!({"type": kind, "value": value}).to_string(),
    )
}

#[test]
fn template_named_fields_and_escaped_braces_preserve_exact_text() {
    let template = "result = {{'label': '{label}', 'count': {count}}}\n{input}";
    let node = CodeNodeDefinition::from_yaml(&mapped_source_yaml("fstring", template)).unwrap();
    let state = State::from([
        ("label".into(), json!("é\nnext")),
        ("count".into(), json!("7")),
        ("input".into(), json!("# final")),
    ]);
    let types = BTreeMap::from([
        ("label".into(), "str".into()),
        ("count".into(), "str".into()),
    ]);
    let before = state.clone();
    let resolved = node.resolve_source(&state, &types).unwrap();
    assert_eq!(
        resolved.source(),
        "result = {'label': 'é\nnext', 'count': 7}\n# final"
    );
    assert_eq!(resolved.provenance(), CodeProvenance::StateTemplate);
    assert_eq!(state, before);
}

#[test]
fn template_references_declared_text_even_when_not_selected_for_state_export() {
    let node =
        CodeNodeDefinition::from_yaml(&mapped_source_yaml("fstring", "result = {source}")).unwrap();
    let state = State::from([("source".into(), json!("7"))]);
    let types = BTreeMap::from([("source".into(), "str".into())]);
    assert_eq!(node.input_keys(), ["messages"]);
    assert!(node.validate_source_types(&types).is_ok());
    assert_eq!(
        node.resolve_source(&state, &types).unwrap().source(),
        "result = 7"
    );
}

#[test]
fn template_mapping_keeps_dynamic_provenance_without_any_fields() {
    let template =
        CodeNodeDefinition::from_yaml(&mapped_source_yaml("fstring", "result = 7")).unwrap();
    let fixed = CodeNodeDefinition::from_yaml(&mapped_source_yaml("fixed", "result = 7")).unwrap();
    let state = State::new();
    let resolved = template.resolve_source(&state, &BTreeMap::new()).unwrap();
    assert_eq!(resolved.source(), "result = 7");
    assert_eq!(resolved.provenance(), CodeProvenance::StateTemplate);
    assert_ne!(template.validated_digest(), fixed.validated_digest());
}

#[test]
fn template_resolution_does_not_change_frozen_mapping_or_definition_digest() {
    let node =
        CodeNodeDefinition::from_yaml(&mapped_source_yaml("fstring", "result = {source}")).unwrap();
    let encoded = serde_json::to_string(&node).unwrap();
    let digest = node.validated_digest();
    let types = BTreeMap::from([("source".into(), "str".into())]);
    for source in ["7", "8"] {
        let state = State::from([("source".into(), json!(source))]);
        assert_eq!(
            node.resolve_source(&state, &types).unwrap().source(),
            format!("result = {source}")
        );
        assert_eq!(serde_json::to_string(&node).unwrap(), encoded);
        assert_eq!(node.validated_digest(), digest);
    }
    let changed =
        CodeNodeDefinition::from_yaml(&mapped_source_yaml("fstring", "result = 0 + {source}"))
            .unwrap();
    assert_ne!(node.validated_digest(), changed.validated_digest());
}

#[test]
fn template_fields_require_declared_scalar_values_before_binding_and_resolution() {
    let node =
        CodeNodeDefinition::from_yaml(&mapped_source_yaml("fstring", "result = {source}")).unwrap();
    let state = State::from([("source".into(), json!("7"))]);
    assert!(node.validate_source_types(&BTreeMap::new()).is_err());
    assert!(node.resolve_source(&state, &BTreeMap::new()).is_err());
    for kind in ["float", "bool", "list", "dict"] {
        let types = BTreeMap::from([("source".into(), kind.into())]);
        assert!(node.validate_source_types(&types).is_err());
        assert!(node.resolve_source(&state, &types).is_err());
    }
    let types = BTreeMap::from([("source".into(), "str".into())]);
    for invalid in [json!(7), json!(true), json!([]), json!({}), json!(null)] {
        assert!(
            node.resolve_source(&State::from([("source".into(), invalid)]), &types)
                .is_err()
        );
    }
    assert!(node.resolve_source(&State::new(), &types).is_err());
}

#[test]
fn template_builtin_input_is_text_and_cannot_override_its_declared_type() {
    let node =
        CodeNodeDefinition::from_yaml(&mapped_source_yaml("fstring", "result = {input}")).unwrap();
    let state = State::from([("input".into(), json!("7"))]);
    assert_eq!(
        node.resolve_source(&state, &BTreeMap::new())
            .unwrap()
            .source(),
        "result = 7"
    );
    assert!(
        node.validate_source_types(&BTreeMap::from([("input".into(), "int".into())]))
            .is_err()
    );
}

#[test]
fn template_rejects_expressions_format_specifiers_reserved_and_malformed_fields() {
    for template in [
        "result = {source!r}",
        "result = {source:>10}",
        "result = {source[0]}",
        "result = {source.attribute}",
        "result = {source()}",
        "result = {0}",
        "result = {}",
        "result = {source",
        "result = source}",
        "result = {é}",
        "result = {messages}",
        "result = {session_id}",
        "result = {state_types}",
        "result = {__elitea_application_variable_x}",
        "result = {source{field}}",
        "result = {PRIVATE_MARKER!r}",
    ] {
        let error = CodeNodeDefinition::from_yaml(&mapped_source_yaml("fstring", template))
            .err()
            .unwrap();
        assert!(!error.contains("PRIVATE_MARKER"));
    }
}

#[test]
fn template_fields_and_variable_name_lengths_have_fixed_bounds() {
    let allowed = "{source}".repeat(256);
    assert!(CodeNodeDefinition::from_yaml(&mapped_source_yaml("fstring", &allowed)).is_ok());
    assert!(
        CodeNodeDefinition::from_yaml(&mapped_source_yaml("fstring", &"{source}".repeat(257)))
            .is_err()
    );
    let allowed_key = "a".repeat(256);
    assert!(
        CodeNodeDefinition::from_yaml(&mapped_source_yaml(
            "fstring",
            &format!("{{{allowed_key}}}")
        ))
        .is_ok()
    );
    assert!(
        CodeNodeDefinition::from_yaml(&mapped_source_yaml(
            "fstring",
            &format!("{{{allowed_key}a}}")
        ))
        .is_err()
    );
}

#[test]
fn template_expansion_checks_final_bytes_before_copying_an_oversized_value() {
    let node = CodeNodeDefinition::from_yaml(&mapped_source_yaml("fstring", "{source}")).unwrap();
    let types = BTreeMap::from([("source".into(), "str".into())]);
    let state = State::from([("source".into(), json!("x".repeat(256 * 1024)))]);
    assert_eq!(
        node.resolve_source(&state, &types).unwrap().source().len(),
        256 * 1024
    );
    for source in [
        "x".repeat(256 * 1024 + 1),
        String::new(),
        "PRIVATE_MARKER\0".into(),
    ] {
        let error = node
            .resolve_source(&State::from([("source".into(), json!(source))]), &types)
            .err()
            .unwrap();
        assert!(!error.contains("PRIVATE_MARKER"));
    }
    let repeated =
        CodeNodeDefinition::from_yaml(&mapped_source_yaml("fstring", "{source}{source}")).unwrap();
    assert!(repeated.resolve_source(&state, &types).is_err());
    let multi_byte = State::from([("source".into(), json!("é".repeat(128 * 1024 + 1)))]);
    assert!(node.resolve_source(&multi_byte, &types).is_err());
}

#[test]
fn template_source_bound_stays_equal_to_fixed_source_bound() {
    let source = "x".repeat(256 * 1024);
    let node = CodeNodeDefinition::from_yaml(&mapped_source_yaml("fstring", &source)).unwrap();
    let state = State::new();
    assert_eq!(
        node.resolve_source(&state, &BTreeMap::new())
            .unwrap()
            .source(),
        source
    );
    assert!(
        CodeNodeDefinition::from_yaml(&mapped_source_yaml("fstring", &(source + "x"))).is_err()
    );
}

#[test]
fn template_native_integer_text_matches_sdk_decimal_values_without_coercion() {
    let node =
        CodeNodeDefinition::from_yaml(&mapped_source_yaml("fstring", "result = {count}")).unwrap();
    let types = BTreeMap::from([("count".into(), "int".into())]);
    assert!(node.validate_source_types(&types).is_ok());
    for number in [json!(i64::MIN), json!(0), json!(u64::MAX)] {
        let expected = format!("result = {number}");
        let state = State::from([("count".into(), number)]);
        assert_eq!(
            node.resolve_source(&state, &types).unwrap().source(),
            expected
        );
    }
    for value in [json!(1.5), json!("7"), json!(true), json!(null)] {
        assert!(
            node.resolve_source(&State::from([("count".into(), value)]), &types)
                .is_err()
        );
    }
}

use super::super::code::CodeProvenance;
use super::*;
use serde_json::json;
fn invocation(source: &str) -> CodeInvocation<'_> {
    CodeInvocation {
        original: None,
        platform_client: false,
        workspace: None,
        activation: [7; 32],
        language: CodeLanguage::Python,
        source,
        dependencies_toml: None,
        provenance: CodeProvenance::StateVariable,
        input_json: br#"{"nested":[1,null,{"unknown":"value"}],"large":9007199254740993}"#.to_vec(),
        trace: None,
        debug: Some(CodeDebugSelection {
            node_id: "run".into(),
            graph_thread_id: "thread".into(),
            graph_step: "2".into(),
            configuration_json: r#"{"id":"run","type":"code","debug":true}"#.into(),
            original: Some(CodeDebugDefinitionPin {
                definition: [3; 32],
                yaml: [4; 32],
            }),
        }),
    }
}
#[test]
#[allow(
    clippy::items_after_statements,
    reason = "Keep local fixture types beside their exact validation checks."
)]
fn resolved_source_and_selected_input_are_lossless_and_data_only() {
    let source = "# λ\r\nprint('quoted\\ntext')\n\n";
    let invocation = invocation(source);
    let (intent, bytes) = snapshot(&invocation, &original_visit(), &[7; 32], 1, &[5; 32]).unwrap();
    let value: serde_json::Value = serde_json::from_slice(&bytes).unwrap();
    assert_eq!(value["source"], source);
    assert_eq!(
        value["selected_input"]["large"],
        json!(9_007_199_254_740_993_u64)
    );
    assert_eq!(value.as_object().unwrap().len(), 4);
    assert_eq!(intent.source_sha256, sha(source.as_bytes()));
    assert_eq!(intent.input_sha256, sha(&invocation.input_json));
    assert_eq!(intent.snapshot_sha256, sha(&bytes));
    assert_eq!(intent.request_sha256, hex(&[5; 32]));
    for forbidden in [
        "grant",
        "fence_token",
        "platform_client",
        "workspace",
        "environment",
    ] {
        assert!(!value.as_object().unwrap().contains_key(forbidden));
    }
    #[derive(Deserialize)]
    struct RawSnapshot {
        selected_input: Box<RawValue>,
    }
    let raw: RawSnapshot = serde_json::from_slice(&bytes).unwrap();
    assert_eq!(raw.selected_input.get().as_bytes(), invocation.input_json);
}
#[test]
fn all_languages_export_exact_source_without_adapter_credentials() {
    for language in [
        CodeLanguage::Python,
        CodeLanguage::JavaScript,
        CodeLanguage::TypeScript,
        CodeLanguage::Rust,
    ] {
        let mut call = invocation("line1\r\nline2\n");
        call.language = language;
        let (_, bytes) = snapshot(&call, &original_visit(), &[7; 32], 1, &[5; 32]).unwrap();
        let value: serde_json::Value = serde_json::from_slice(&bytes).unwrap();
        assert_eq!(value["language"], super::language(language));
        assert_eq!(value["source"], call.source);
    }
}
#[test]
fn maximum_source_and_input_are_not_truncated() {
    let source = "a".repeat(256 * 1024);
    let mut call = invocation(&source);
    call.input_json = format!("{{\"value\":\"{}\"}}", "b".repeat(512 * 1024 - 12)).into_bytes();
    assert_eq!(call.input_json.len(), 512 * 1024);
    let (_, bytes) = snapshot(&call, &original_visit(), &[7; 32], 1, &[5; 32]).unwrap();
    let value: serde_json::Value = serde_json::from_slice(&bytes).unwrap();
    assert_eq!(value["source"].as_str().unwrap().len(), source.len());
    assert_eq!(
        value["selected_input"]["value"].as_str().unwrap().len(),
        512 * 1024 - 12
    );
    call.input_json.push(b' ');
    assert!(snapshot(&call, &original_visit(), &[7; 32], 1, &[5; 32]).is_err());
}
#[test]
fn absent_selection_and_missing_original_pin_grant_no_export() {
    let mut call = invocation("7");
    call.debug = None;
    assert!(snapshot(&call, &original_visit(), &[7; 32], 1, &[5; 32]).is_err());
    call.debug = invocation("7").debug;
    call.debug.as_mut().unwrap().original = None;
    assert!(snapshot(&call, &original_visit(), &[7; 32], 1, &[5; 32]).is_err());
}
#[test]
fn reference_requires_exact_digest_size_project_and_opaque_key() {
    let (intent, _) = snapshot(&invocation("7"), &original_visit(), &[7; 32], 1, &[5; 32]).unwrap();
    let mut reference = CodeDebugArtifactReference {
        schema_version: ARTIFACT_SCHEMA.into(),
        project_id: 7,
        bucket: "code-debug".into(),
        name: format!("{}.json", "a".repeat(64)),
        media_type: "application/json".into(),
        byte_length: intent.byte_length,
        sha256: intent.snapshot_sha256.clone(),
    };
    assert!(reference.matches(&intent, 7));
    assert!(!reference.matches(&intent, 8));
    reference.byte_length += 1;
    assert!(!reference.matches(&intent, 7));
    reference.byte_length = intent.byte_length;
    reference.sha256 = "a".repeat(64);
    assert!(!reference.matches(&intent, 7));
}
#[test]
fn trace_refuses_payload_fields_and_links_after_failed_export() {
    let mut proof = CodeDebugProof {
        revision: 1,
        original_visit: original_visit(),
        attempt: 1,
        execution_id: "execution".into(),
        generation: "1".into(),
        node_id: "run".into(),
        activation_id: "a".repeat(64),
        request_sha256: "b".repeat(64),
        status: "unavailable".into(),
        artifact: None,
    };
    assert!(proof.validate().is_ok());
    let mut raw = serde_json::to_value(&proof).unwrap();
    raw["source"] = json!("private");
    assert!(serde_json::from_value::<CodeDebugProof>(raw).is_err());
    proof.status = "committed".into();
    assert!(proof.validate().is_err());
    proof.status = "denied".into();
    proof.artifact = Some(CodeDebugArtifactReference {
        schema_version: ARTIFACT_SCHEMA.into(),
        project_id: 7,
        bucket: "code-debug".into(),
        name: format!("{}.json", "c".repeat(64)),
        media_type: "application/json".into(),
        byte_length: 10,
        sha256: "d".repeat(64),
    });
    assert!(proof.validate().is_err());
}
#[test]
fn debug_metadata_pin_preserves_all_legacy_configuration_bytes_and_digest() {
    let mut definition =
        super::super::code::CodeNodeDefinition::from_yaml("id: run\ntype: code\ncode: '7'\n")
            .unwrap();
    let bytes = serde_json::to_vec(&definition).unwrap();
    let digest = definition.config_digest().unwrap();
    definition.bind_debug_definition([3; 32], [4; 32]);
    assert_eq!(serde_json::to_vec(&definition).unwrap(), bytes);
    assert_eq!(definition.config_digest().unwrap(), digest);
}

#[test]
fn anchored_saved_code_matches_the_shared_main_declaration_fixture() {
    let saved = include_str!(
        "../../../../../libs/proto/elitea/runtime/v1/fixtures/code_debug_anchored.yaml"
    );
    let pipeline = super::super::compiler::PipelineDefinition::from_yaml(saved).unwrap();
    assert_eq!(pipeline.node_count(), 1);
    // This is the same Value -> reserialized node -> owning Code parser path
    // used by parse_pipeline_node; no independent YAML interpretation is used.
    let document: serde_yaml_ng::Value =
        crate::bounded_yaml::from_str(saved, super::super::compiler::PIPELINE_YAML_BUDGET).unwrap();
    let encoded = serde_yaml_ng::to_string(&document["nodes"][0]).unwrap();
    let definition = super::super::code::CodeNodeDefinition::from_yaml(&encoded).unwrap();
    let actual = serde_json::to_string(&definition).unwrap();
    let expected = r#"{"id":"run","type":"code","language":"python","code":{"type":"fixed","value":"input['input']\n"},"input":["input"],"output":["answer"],"structured_output":false,"debug":true,"transition":"END"}"#;
    assert_eq!(actual, expected);
}

#[test]
fn owning_rust_parser_refuses_cyclic_duplicate_and_multiple_document_fixtures() {
    let base = "entry_point: run\nnodes:\n - id: run\n   type: code\n   code: '7'\n";
    let invalid = [
        "entry_point: run\nloop: &loop [*loop]\nnodes: [{id: run, type: code, code: '7'}]\n"
            .to_owned(),
        base.replace("   code: '7'", "   code: &source '7'\n   code: *source"),
        format!("{base}---\nentry_point: other\nnodes: []\n"),
        base.replace("code: '7'", "code: *absent"),
    ];
    for saved in invalid {
        assert!(super::super::compiler::PipelineDefinition::from_yaml(&saved).is_err());
    }
    let merge = "entry_point: run\ndefaults: &defaults {debug: true}\nnodes:\n - id: run\n   type: code\n   code: '7'\n   <<: *defaults\n";
    // serde_yaml_ng Value preserves << as a literal field; Code denies it.
    assert!(super::super::compiler::PipelineDefinition::from_yaml(merge).is_err());
}

#[test]
fn saved_input_approval_matches_the_actual_code_state_boundary() {
    use adk_rust::graph::State;
    use std::collections::BTreeMap;
    let types = BTreeMap::from([
        ("count".into(), "int".into()),
        ("ratio".into(), "float".into()),
        ("flag".into(), "bool".into()),
        ("items".into(), "list".into()),
        ("record".into(), "dict".into()),
        ("messages".into(), "list".into()),
        ("10".into(), "str".into()),
    ]);
    let boundary = super::super::code_state::CodeStateBoundary::new(&types).unwrap();
    let mut state = State::new();
    for (key, value) in [
        ("input", json!("hello")),
        ("count", json!(u64::MAX)),
        ("ratio", json!(2)),
        ("flag", json!(true)),
        ("items", json!([null, 7, {"unknown": [false, "opaque"]}])),
        ("record", json!({"nested": null})),
        ("10", json!("quoted")),
        ("messages", json!(["framework-message"])),
        ("context_info", json!({"private": "framework"})),
        ("undeclared", json!("must not enter input")),
    ] {
        state.insert(key.into(), value);
    }
    for selected in [vec![], vec!["messages".to_owned()]] {
        let bytes = boundary.input_json(&state, &selected).unwrap();
        let input: serde_json::Value = serde_json::from_slice(&bytes).unwrap();
        assert_eq!(input.as_object().unwrap().len(), 7);
        assert_eq!(input["count"], json!(u64::MAX));
        for excluded in ["messages", "context_info", "undeclared"] {
            assert!(input.get(excluded).is_none());
        }
    }
    for selected in [
        vec!["count".to_owned()],
        vec!["messages".to_owned(), "count".to_owned()],
    ] {
        let bytes = boundary.input_json(&state, &selected).unwrap();
        assert_eq!(
            serde_json::from_slice::<serde_json::Value>(&bytes).unwrap(),
            json!({"count": u64::MAX})
        );
    }
    state.remove("count");
    assert_eq!(
        boundary.input_json(&state, &["count".to_owned()]).unwrap(),
        b"{}"
    );
    state.insert("count".into(), json!(1.0));
    assert!(boundary.input_json(&state, &["count".to_owned()]).is_err());
}

#[test]
fn malformed_unicode_artifact_names_fail_without_panicking() {
    let (intent, _) = snapshot(&invocation("7"), &original_visit(), &[7; 32], 1, &[5; 32]).unwrap();
    for name in [
        format!("{}é.json", "a".repeat(62)),
        format!("{}éa.json", "a".repeat(61)),
    ] {
        let reference = CodeDebugArtifactReference {
            schema_version: ARTIFACT_SCHEMA.into(),
            project_id: 7,
            bucket: "code-debug".into(),
            name,
            media_type: "application/json".into(),
            byte_length: intent.byte_length,
            sha256: intent.snapshot_sha256.clone(),
        };
        assert!(!reference.matches(&intent, 7));
        let proof = CodeDebugProof {
            revision: 1,
            original_visit: original_visit(),
            attempt: 1,
            execution_id: "execution".into(),
            generation: "1".into(),
            node_id: "run".into(),
            activation_id: "a".repeat(64),
            request_sha256: "b".repeat(64),
            status: "committed".into(),
            artifact: Some(reference),
        };
        assert!(proof.validate().is_err());
    }
}
#[test]
fn same_generation_replacement_reuses_identity_and_new_generation_does_not() {
    let mut proof = CodeDebugProof {
        revision: 1,
        original_visit: original_visit(),
        attempt: 1,
        execution_id: "execution".into(),
        generation: "1".into(),
        node_id: "run".into(),
        activation_id: "a".repeat(64),
        request_sha256: "b".repeat(64),
        status: "denied".into(),
        artifact: None,
    };
    let id = proof.run_id();
    assert_eq!(proof.run_id(), id);
    proof.generation = "2".into();
    assert_ne!(proof.run_id(), id);
    proof.generation = "1".into();
    proof.activation_id = "c".repeat(64);
    assert_ne!(proof.run_id(), id);
    proof.activation_id = "a".repeat(64);
    proof.request_sha256 = "c".repeat(64);
    assert_ne!(proof.run_id(), id);
}

#[test]
fn distinct_retry_visit_has_its_own_trace_and_cannot_be_the_previous_attempt() {
    let first = CodeDebugProof {
        revision: 1,
        original_visit: original_visit(),
        attempt: 1,
        execution_id: "execution".into(),
        generation: "7".into(),
        node_id: "run".into(),
        activation_id: "a".repeat(64),
        request_sha256: "b".repeat(64),
        status: "unavailable".into(),
        artifact: None,
    };
    let mut replacement = first.clone();
    assert_eq!(first.run_id(), replacement.run_id());
    replacement.original_visit.visit_id = "3".repeat(64);
    replacement.original_visit.digest_sha256 = "4".repeat(64);
    replacement.attempt = 2;
    assert!(replacement.validate().is_ok());
    assert_ne!(first.run_id(), replacement.run_id());
    let (intent, _) = snapshot(
        &invocation("7"),
        &replacement.original_visit,
        &[7; 32],
        2,
        &[5; 32],
    )
    .unwrap();
    assert_eq!(
        intent.original_visit.visit_id,
        replacement.original_visit.visit_id
    );
    assert_eq!(intent.attempt, 2);
    for invalid in [0, 17] {
        assert!(
            snapshot(
                &invocation("7"),
                &original_visit(),
                &[7; 32],
                invalid,
                &[5; 32]
            )
            .is_err()
        );
    }
}

struct NonfatalDebugRuntime;
#[async_trait::async_trait]
impl super::super::code_runtime::CodeSandboxRuntime for NonfatalDebugRuntime {
    async fn execute(
        &self,
        invocation: CodeInvocation<'_>,
    ) -> Result<Vec<u8>, adk_rust::graph::GraphError> {
        assert_eq!(invocation.source, "source-private");
        assert_eq!(invocation.input_json, br#"{"count":2}"#);
        if invocation.debug.is_some() {
            let trace = invocation
                .trace
                .as_ref()
                .unwrap()
                .bind(("execution-1", 7))?;
            trace
                .debug(
                    &original_visit(),
                    1,
                    &[7; 32],
                    &[5; 32],
                    Err(CodeDebugFailure::Denied),
                )
                .await?;
        }
        Ok(serde_json::to_vec(&json!({"revision":1,"status":"completed","exit_code":0,"stdout":"{\"revision\":1,\"result\":7}","stderr":""})).unwrap())
    }
}
#[tokio::test]
async fn actual_code_node_keeps_result_and_selected_state_when_debug_is_denied() {
    use adk_rust::graph::{ExecutionConfig, Node, NodeContext, State};
    use std::{collections::BTreeMap, sync::Arc};
    for debug in [false, true] {
        let (sender, receiver) = super::super::node_events::pipeline_node_event_channel();
        let definition=super::super::code::CodeNodeDefinition::from_yaml(&format!("id: Code_1\ntype: code\ncode: source-private\ninput: [count]\noutput: [count]\ntransition: END\ndebug: {debug}\n")).unwrap();
        let node = super::super::code_runtime::CodeNode::new(
            definition,
            BTreeMap::from([("count".into(), "int".into())]),
            Arc::new(NonfatalDebugRuntime),
        )
        .unwrap()
        .with_events(Some(sender));
        let context = NodeContext::new(
            State::from([
                ("count".into(), json!(2)),
                ("private_state".into(), json!("never export")),
            ]),
            ExecutionConfig::new("root/child"),
            4,
        );
        let result = node.execute(&context).await.unwrap();
        assert_eq!(result.updates, State::from([("count".into(), json!(7))]));
        let mut drain = receiver
            .drain("original-invocation", "root-agent", "")
            .await
            .unwrap();
        if debug {
            let event = drain.try_recv().unwrap().unwrap();
            let proof = CodeDebugProof::from_event(&event).unwrap().unwrap();
            assert_eq!(proof.status, "denied");
            assert!(proof.artifact.is_none());
            let encoded = serde_json::to_string(&event).unwrap();
            for forbidden in ["source-private", "private_state", "never export"] {
                assert!(!encoded.contains(forbidden));
            }
        }
        assert!(drain.try_recv().is_none());
    }
}

#[test]
fn admitted_control_heavy_fixed_source_is_exported_without_configuration_truncation() {
    let source = "\u{0001}".repeat(100 * 1024);
    // YAML serializes this admitted control character with a shorter escape than JSON.
    let document =
        serde_yaml_ng::to_string(&json!({"id":"run","type":"code","debug":true,"code":source}))
            .unwrap();
    let definition = super::super::code::CodeNodeDefinition::from_yaml(&document).unwrap();
    let mut call = invocation(&source);
    call.debug.as_mut().unwrap().configuration_json = serde_json::to_string(&definition).unwrap();
    assert!(call.debug.as_ref().unwrap().configuration_json.len() > 512 * 1024);
    let (_, bytes) = snapshot(&call, &original_visit(), &[7; 32], 1, &[5; 32]).unwrap();
    let value: serde_json::Value = serde_json::from_slice(&bytes).unwrap();
    assert_eq!(
        value["source"].as_str().unwrap().as_bytes(),
        source.as_bytes()
    );
}

fn original_visit() -> OriginalCodeVisitRef {
    OriginalCodeVisitRef {
        visit_id: "1".repeat(64),
        revision: 1,
        digest_sha256: "2".repeat(64),
    }
}

#[test]
fn missing_original_visit_identity_never_exports_user_data() {
    let mut original = original_visit();
    original.revision = 0;
    assert!(snapshot(&invocation("7"), &original, &[7; 32], 1, &[5; 32]).is_err());
}

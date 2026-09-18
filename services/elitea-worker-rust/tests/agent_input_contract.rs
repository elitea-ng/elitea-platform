use elitea_worker_rust::agents::{
    AGENT_RESULT_CLASSIFICATION, AGENT_RESULT_MEDIA_TYPE, AgentExecutionKind, AgentInputBinding,
    AgentProtocolError, AgentResultArtifact, AgentTerminalState, UserInput, bind_result_artifact,
    parse_agent_execution_input, request_from,
};
use elitea_worker_rust::protocol::elitea::runtime::v1::{
    AgentExecutionInputV1, AgentExecutionTerminalStateV1, DigestAlgorithmV1,
};

const ARBITRARY_INTEGER: &str =
    "100000000000000000000000000000000000000000000000000000000000000000000000000000000";

fn decode_hex(value: &str) -> Vec<u8> {
    let value = value.trim();
    assert_eq!(value.len() % 2, 0, "fixture must contain full bytes");
    value
        .as_bytes()
        .chunks_exact(2)
        .map(|pair| {
            let digits = std::str::from_utf8(pair).expect("ASCII hex fixture");
            u8::from_str_radix(digits, 16).expect("valid hex fixture")
        })
        .collect()
}

fn application_bytes() -> Vec<u8> {
    decode_hex(include_str!("fixtures/agent_application_input.hex"))
}

fn adhoc_bytes() -> Vec<u8> {
    decode_hex(include_str!("fixtures/agent_adhoc_input.hex"))
}

fn binding() -> AgentInputBinding {
    AgentInputBinding {
        input_bundle_id: "bundle-1".to_owned(),
        input_bundle_digest: [b'b'; 32],
        request_entry_id: "agent-request".to_owned(),
        request_immutable_version: "v1".to_owned(),
        request_content_digest: [b'r'; 32],
    }
}

fn application_message() -> AgentExecutionInputV1 {
    parse_agent_execution_input(&application_bytes()).expect("Python application fixture")
}

#[test]
fn authorized_model_limits_survive_canonical_wire_and_old_inputs_remain_absent() {
    use elitea_worker_rust::protocol::elitea::runtime::v1::ModelContextLimitsV1;
    use prost::Message;
    let mut message = application_message();
    assert!(message.model_context_limits.is_none());
    message.model_context_limits = Some(ModelContextLimitsV1 {
        context_window_tokens: 1_000_000,
        max_output_tokens: 128_000,
        context_window_fallback: false,
        max_output_fallback: true,
        max_input_tokens: Some(872_000),
    });
    let decoded = parse_agent_execution_input(&message.encode_to_vec()).unwrap();
    let request = request_from(decoded, AgentExecutionKind::Application, binding()).unwrap();
    let limits = request.payload.model_context_limits.unwrap();
    assert_eq!(limits.context_window_tokens, 1_000_000);
    assert_eq!(limits.max_output_tokens, 128_000);
    assert_eq!(limits.max_input_tokens, Some(872_000));
    assert!(!limits.context_window_fallback);
    assert!(limits.max_output_fallback);
}

#[test]
fn summary_model_requires_its_own_limits_and_preserves_its_selection() {
    use elitea_worker_rust::protocol::elitea::runtime::v1::{
        ModelContextLimitsV1, SummaryModelSnapshotV1,
    };
    use prost::Message;
    let mut message = application_message();
    assert!(message.summary_model.is_none());
    message.summary_model = Some(SummaryModelSnapshotV1 {
        llm_settings: br#"{"model_name":"summary-model","model_project_id":7,"max_tokens":2048,"openai_compatible":true}"#.to_vec(),
        model_context_limits: Some(ModelContextLimitsV1 {
            context_window_tokens: 32_000, max_output_tokens: 4_000,
            context_window_fallback: false, max_output_fallback: false, max_input_tokens: Some(24_000),
        }),
    });
    let decoded = parse_agent_execution_input(&message.encode_to_vec()).unwrap();
    let request = request_from(decoded, AgentExecutionKind::Application, binding()).unwrap();
    let summary = request.payload.summary_model.unwrap();
    assert_eq!(summary.llm_settings["model_name"], "summary-model");
    assert_eq!(summary.model_context_limits.max_input_tokens, Some(24_000));
    assert!(request.payload.model_context_limits.is_none());
    message.summary_model.as_mut().unwrap().model_context_limits = None;
    assert!(request_from(message, AgentExecutionKind::Application, binding()).is_err());
}

#[test]
fn python_generated_application_fixture_maps_all_current_fields() {
    let raw = application_bytes();
    let message = parse_agent_execution_input(&raw).expect("canonical Python protobuf");
    let request = request_from(message, AgentExecutionKind::Application, binding())
        .expect("typed application request");
    let payload = &request.payload;

    assert_eq!(request.kind, AgentExecutionKind::Application);
    assert_eq!(payload.thread_id.as_deref(), Some("thread-1"));
    assert_eq!(payload.checkpoint_id.as_deref(), Some("checkpoint-1"));
    assert!(payload.debug);
    assert_eq!(payload.steps_limit, Some(17));
    assert!(payload.should_continue);
    assert!(payload.hitl_resume);
    assert_eq!(payload.hitl_action.as_deref(), Some("approve"));
    assert_eq!(payload.hitl_value.as_deref(), Some(""));
    assert_eq!(
        payload.execution_generation.as_deref(),
        Some("generation-2")
    );
    assert!(payload.is_regenerate);
    assert_eq!(payload.conversation_id.as_deref(), Some("conversation-1"));
    assert_eq!(payload.persona, "reviewer");
    assert!(payload.supports_vision);
    assert!(payload.return_chat_history);
    assert!(payload.auto_approve_sensitive_actions);
    assert_eq!(payload.exception_handling_enabled, Some(true));
    assert_eq!(payload.debug_mode, Some(false));
    assert!(payload.parallel_reconcile.is_none());
    assert!(payload.parallel_terminal_errors.is_empty());
    assert_eq!(payload.internal_tools, ["lazy_tools_mode"]);
    assert_eq!(payload.chat_history.len(), 1);
    assert_eq!(payload.tools.len(), 1);
    assert_eq!(payload.ignored_mcp_servers.len(), 1);
    assert_eq!(payload.user_declined_mcp_servers.len(), 1);
    assert_eq!(payload.invoked_skills.len(), 1);
    assert_eq!(payload.applied_skills.len(), 1);
    assert_eq!(payload.attached_skills.len(), 1);
    assert_eq!(payload.input_attachments.len(), 1);
    assert_eq!(payload.meta["source"], "python");
    assert_eq!(payload.context_settings["project"], "fixture");
    assert_eq!(payload.llm["kwargs"]["provider_limit"], 999);
    assert_eq!(payload.application["id"], 11);
    assert_eq!(payload.mcp_tokens["server"], "non-secret-fixture-reference");
    assert_eq!(
        payload.next_input_suggestion,
        elitea_worker_rust::agents::NextInputSuggestionPolicy {
            enabled: true,
            min_response_chars: 151,
            timeout_seconds: 16,
        }
    );
    assert!(payload.toolkit_guardrails.is_none());
    let UserInput::ContentBlocks(blocks) = &payload.user_input else {
        panic!("fixture user input must remain content blocks");
    };
    assert_eq!(blocks[1]["ordinal"], 2);
}

#[test]
fn python_generated_adhoc_fixture_selects_distinct_semantics() {
    let message = parse_agent_execution_input(&adhoc_bytes()).expect("canonical Python protobuf");
    let request =
        request_from(message, AgentExecutionKind::Adhoc, binding()).expect("typed ad-hoc request");

    assert_eq!(request.kind, AgentExecutionKind::Adhoc);
    assert_eq!(request.payload.application["instructions"], "Be concise");
    assert_eq!(request.payload.llm["kwargs"]["model"], "fixture-model");
}

#[test]
fn protobuf_boundary_rejects_noncanonical_unknown_and_oversize_inputs() {
    let mut unknown_field = application_bytes();
    unknown_field.extend_from_slice(&[0xa0, 0x06, 0x01]);
    assert!(matches!(
        parse_agent_execution_input(&unknown_field),
        Err(AgentProtocolError::InvalidInput(message)) if message.contains("canonical")
    ));
    assert!(matches!(
        parse_agent_execution_input(&[]),
        Err(AgentProtocolError::ResourceExhausted(_))
    ));
    assert!(matches!(
        parse_agent_execution_input(&vec![0; 1024 * 1024 + 1]),
        Err(AgentProtocolError::ResourceExhausted(_))
    ));
}

#[test]
fn every_truncated_required_json_document_fails_semantic_admission() {
    let original = application_message().llm;
    for end in 0..original.len() {
        let mut message = application_message();
        message.llm = original[..end].to_vec();
        let admitted = request_from(message, AgentExecutionKind::Application, binding());
        assert!(
            admitted.is_err(),
            "truncated llm JSON prefix {end} was admitted"
        );
    }
}

#[test]
fn semantic_shapes_and_resume_requirements_are_fail_closed() {
    let mut message = application_message();
    message.steps_limit = Some(0);
    assert_invalid(message, AgentExecutionKind::Application, "step limit");

    let mut message = application_message();
    message.hitl_action = None;
    message.hitl_resume = true;
    message.hitl_decisions = b"[]".to_vec();
    assert_invalid(message, AgentExecutionKind::Application, "HITL resume");

    let mut message = application_message();
    message.application = br#"{"id":1}"#.to_vec();
    assert_invalid(
        message,
        AgentExecutionKind::Application,
        "application identity",
    );

    let mut message = application_message();
    message.llm = br#"{"kwargs":{}}"#.to_vec();
    assert_invalid(message, AgentExecutionKind::Adhoc, "model");

    let mut message = application_message();
    message.user_input = b"17".to_vec();
    assert_invalid(message, AgentExecutionKind::Application, "user input");
}

#[test]
fn json_boundary_rejects_duplicate_escaped_duplicate_and_nonfinite_members() {
    for malformed in [
        br#"{"first":1,"first":2}"#.as_slice(),
        br#"{"first":1,"\u0066irst":2}"#.as_slice(),
        br#"{"value":1e400}"#.as_slice(),
    ] {
        let mut message = application_message();
        message.meta = malformed.to_vec();
        assert!(matches!(
            request_from(message, AgentExecutionKind::Application, binding()),
            Err(AgentProtocolError::InvalidInput(_))
        ));
    }
}

#[test]
fn admitted_history_reaches_compaction_without_control_field_limits() {
    use prost::Message;
    let mut message = application_message();
    message.chat_history = serde_json::to_vec(&serde_json::json!([
        {"role": "assistant", "content": "a".repeat(430_000)}
    ]))
    .unwrap();
    let decoded = parse_agent_execution_input(&message.encode_to_vec()).unwrap();
    assert!(request_from(decoded, AgentExecutionKind::Application, binding()).is_ok());

    // Other fields retain their smaller budget and strict JSON validation.
    message.meta = message.chat_history.clone();
    assert!(matches!(
        request_from(message, AgentExecutionKind::Application, binding()),
        Err(AgentProtocolError::ResourceExhausted(_))
    ));
    let mut message = application_message();
    message.chat_history = br#"[{"role":"user","role":"assistant"}]"#.to_vec();
    assert!(matches!(
        request_from(message, AgentExecutionKind::Application, binding()),
        Err(AgentProtocolError::InvalidInput(_))
    ));
}

#[test]
fn json_boundary_preserves_python_sized_integers_without_rounding() {
    let mut message = application_message();
    message.llm =
        format!("{{\"kwargs\":{{\"model\":\"fixture-model\",\"limit\":{ARBITRARY_INTEGER}}}}}")
            .into_bytes();
    message.application = format!("{{\"id\":{ARBITRARY_INTEGER},\"version_id\":22}}").into_bytes();

    let request = request_from(message, AgentExecutionKind::Application, binding())
        .expect("arbitrary precision integer contract");

    assert_eq!(
        request.payload.llm["kwargs"]["limit"].to_string(),
        ARBITRARY_INTEGER
    );
    assert_eq!(
        request.payload.application["id"].to_string(),
        ARBITRARY_INTEGER
    );
}

#[test]
fn json_depth_and_decoded_string_limits_match_the_python_worker() {
    let mut accepted_depth = b"0".to_vec();
    // The object itself is depth zero, so 63 arrays place the leaf at depth 64.
    for _ in 0..63 {
        accepted_depth = [b"[".as_slice(), &accepted_depth, b"]".as_slice()].concat();
    }
    let mut message = application_message();
    message.meta = [b"{\"value\":".as_slice(), &accepted_depth, b"}".as_slice()].concat();
    request_from(message, AgentExecutionKind::Application, binding())
        .expect("a value at depth 64 is admitted");

    let rejected_depth = [b"[".as_slice(), &accepted_depth, b"]".as_slice()].concat();
    let mut message = application_message();
    message.meta = [b"{\"value\":".as_slice(), &rejected_depth, b"}".as_slice()].concat();
    assert!(matches!(
        request_from(message, AgentExecutionKind::Application, binding()),
        Err(AgentProtocolError::ResourceExhausted(message)) if message.contains("nesting")
    ));

    let accepted_string = "x".repeat(64 * 1024);
    let mut message = application_message();
    message.meta = format!(
        "{{\"value\":{}}}",
        serde_json::to_string(&accepted_string).unwrap()
    )
    .into_bytes();
    request_from(message, AgentExecutionKind::Application, binding())
        .expect("64 KiB decoded string is admitted");

    let rejected_string = "x".repeat(64 * 1024 + 1);
    let mut message = application_message();
    message.meta = format!(
        "{{\"value\":{}}}",
        serde_json::to_string(&rejected_string).unwrap()
    )
    .into_bytes();
    assert!(matches!(
        request_from(message, AgentExecutionKind::Application, binding()),
        Err(AgentProtocolError::ResourceExhausted(message)) if message.contains("string")
    ));
}

#[test]
fn empty_next_input_policy_defaults_and_unknown_fields_are_rejected() {
    let mut message = application_message();
    message.next_input_suggestion.clear();
    let request = request_from(message, AgentExecutionKind::Application, binding())
        .expect("empty policy retains the current default");
    assert_eq!(
        request.payload.next_input_suggestion.min_response_chars,
        150
    );

    let mut message = application_message();
    message.next_input_suggestion = br#"{"enabled":true,"future":1}"#.to_vec();
    assert_invalid(
        message,
        AgentExecutionKind::Application,
        "next input suggestion",
    );
}

#[test]
fn per_run_toolkit_guardrails_distinguish_absent_empty_and_invalid() {
    let mut message = application_message();
    message.toolkit_guardrails.clear();
    let request = request_from(message, AgentExecutionKind::Application, binding())
        .expect("an absent policy preserves the deployment fallback");
    assert!(request.payload.toolkit_guardrails.is_none());

    let mut message = application_message();
    message.toolkit_guardrails = br"{}".to_vec();
    let request = request_from(message, AgentExecutionKind::Application, binding())
        .expect("an explicit empty policy is authoritative");
    assert_eq!(
        request.payload.toolkit_guardrails,
        Some(serde_json::Map::new())
    );

    let mut message = application_message();
    message.toolkit_guardrails = br#"{"future_policy":true}"#.to_vec();
    assert_invalid(
        message,
        AgentExecutionKind::Application,
        "toolkit guardrails",
    );
}

#[test]
fn terminal_artifact_binding_covers_only_three_current_supported_states() {
    let request = request_from(
        application_message(),
        AgentExecutionKind::Application,
        binding(),
    )
    .expect("application request");
    for (state, expected) in [
        (
            AgentTerminalState::Completed,
            AgentExecutionTerminalStateV1::Completed,
        ),
        (
            AgentTerminalState::PausedHitl,
            AgentExecutionTerminalStateV1::PausedHitl,
        ),
        (
            AgentTerminalState::PausedMcpAuth,
            AgentExecutionTerminalStateV1::PausedMcpAuth,
        ),
    ] {
        let result = bind_result_artifact(
            &request,
            state,
            AgentResultArtifact {
                artifact_id: "artifact-1".to_owned(),
                immutable_version: "v1".to_owned(),
                byte_length: 123,
                digest: [b'a'; 32],
            },
        )
        .expect("terminal artifact binding");
        assert_eq!(result.message().terminal_state, expected as i32);
        assert_eq!(result.message().input_bundle_id, "bundle-1");
        assert_eq!(result.message().request_entry_id, "agent-request");
        assert_eq!(result.message().request_immutable_version, "v1");
        assert_eq!(
            result
                .message()
                .request_content_digest
                .as_ref()
                .unwrap()
                .value,
            vec![b'r'; 32]
        );
        assert_eq!(
            result
                .message()
                .input_bundle_digest
                .as_ref()
                .unwrap()
                .algorithm,
            DigestAlgorithmV1::Sha256 as i32
        );
        let artifact = result.message().result_artifact.as_ref().unwrap();
        assert_eq!(artifact.media_type, AGENT_RESULT_MEDIA_TYPE);
        assert_eq!(artifact.classification, AGENT_RESULT_CLASSIFICATION);
        assert_eq!(artifact.digest.as_ref().unwrap().value, vec![b'a'; 32]);
    }
}

#[test]
fn malformed_content_bindings_and_artifacts_are_rejected_before_delivery() {
    let message = application_message();
    let mut malformed_binding = binding();
    malformed_binding.request_entry_id.clear();
    assert!(matches!(
        request_from(
            message,
            AgentExecutionKind::Application,
            malformed_binding
        ),
        Err(AgentProtocolError::InvalidInput(message)) if message.contains("binding")
    ));

    let mut zero_binding = binding();
    zero_binding.input_bundle_digest.fill(0);
    assert!(matches!(
        request_from(
            application_message(),
            AgentExecutionKind::Application,
            zero_binding
        ),
        Err(AgentProtocolError::InvalidInput(message)) if message.contains("binding")
    ));

    let request = request_from(
        application_message(),
        AgentExecutionKind::Application,
        binding(),
    )
    .expect("application request");

    for artifact in [
        AgentResultArtifact {
            artifact_id: String::new(),
            immutable_version: "v1".to_owned(),
            byte_length: 1,
            digest: [b'a'; 32],
        },
        AgentResultArtifact {
            artifact_id: "artifact-1".to_owned(),
            immutable_version: "v1".to_owned(),
            byte_length: 64 * 1024 + 1,
            digest: [b'a'; 32],
        },
        AgentResultArtifact {
            artifact_id: "artifact-1".to_owned(),
            immutable_version: "v1".to_owned(),
            byte_length: 1,
            digest: [0; 32],
        },
        AgentResultArtifact {
            artifact_id: "artifact-1".to_owned(),
            immutable_version: "v1".to_owned(),
            byte_length: 0,
            digest: [b'a'; 32],
        },
    ] {
        assert!(matches!(
            bind_result_artifact(&request, AgentTerminalState::Completed, artifact),
            Err(AgentProtocolError::InvalidInput(message)) if message.contains("artifact binding")
        ));
    }
}

#[test]
fn artifact_metadata_limits_are_enforced_at_the_typed_boundary() {
    let request = request_from(
        application_message(),
        AgentExecutionKind::Application,
        binding(),
    )
    .expect("application request");

    for (artifact_id, immutable_version) in [
        ("x".repeat(256), "v1".to_owned()),
        ("artifact-1".to_owned(), "x".repeat(256)),
    ] {
        bind_result_artifact(
            &request,
            AgentTerminalState::Completed,
            AgentResultArtifact {
                artifact_id,
                immutable_version,
                byte_length: 1,
                digest: [b'a'; 32],
            },
        )
        .expect("256-byte artifact metadata");
    }

    for (artifact_id, immutable_version) in [
        ("artifact-1".to_owned(), String::new()),
        ("x".repeat(257), "v1".to_owned()),
        ("artifact-1".to_owned(), "x".repeat(257)),
        ("artifact\n1".to_owned(), "v1".to_owned()),
        ("artifact-1".to_owned(), "v1\u{7f}".to_owned()),
    ] {
        assert!(matches!(
            bind_result_artifact(
                &request,
                AgentTerminalState::Completed,
                AgentResultArtifact {
                    artifact_id,
                    immutable_version,
                    byte_length: 1,
                    digest: [b'a'; 32],
                },
            ),
            Err(AgentProtocolError::InvalidInput(message)) if message.contains("artifact binding")
        ));
    }
}

fn assert_invalid(message: AgentExecutionInputV1, kind: AgentExecutionKind, expected: &str) {
    let Err(error) = request_from(message, kind, binding()) else {
        panic!("input must be rejected");
    };
    assert!(
        error.to_string().contains(expected),
        "expected {expected:?}, got {error}"
    );
}

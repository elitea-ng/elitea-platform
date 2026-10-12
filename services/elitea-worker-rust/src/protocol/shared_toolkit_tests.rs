use super::*;
use crate::protocol::command::{
    TestOnlyConformanceHmacAuthenticator, ToolkitCommandKind, VerifiedExecutionCommandKind,
    parse_and_verify_execution_command, parse_and_verify_toolkit_execute_read_command,
};
use crate::protocol::elitea::runtime::v1::{
    ConfigurationValidationCommandV1, ConfigurationValidationIssueV1,
    ConfigurationValidationResultV1, ExecutionOutputEventTypeV1, IndexIngestCommandV1,
    IndexIngestDocumentV1, IndexIngestInputBindingV1, IndexIngestResultV1, IndexIngestStatusV1,
    IndexIngestSummaryV1, IndexIngestTerminalStateV1, SignedWorkerCommandEnvelopeV1,
    ToolkitAuthorizationRequiredV1, ToolkitAvailableToolsArtifactReferenceV1,
    ToolkitAvailableToolsCommandV1, ToolkitAvailableToolsResultV1, ToolkitCallToolCommandV1,
    ToolkitCallToolResultV1, ToolkitCallToolStatusV1, ToolkitCallToolSummaryV1, VectorClaimTokenV1,
    WorkerCommandTypeV1, WorkerCommandV1, execution_output_frame_v1,
};
use crate::protocol::output::validate_restored_toolkit_execute_read_output_frame;
use ring::hmac;

const NOW: i64 = 1_700_000_000_000;

fn vector(name: &str) -> Vec<u8> {
    let (_, hex) = include_str!("../../tests/fixtures/agent_control_vectors.txt")
        .lines()
        .filter_map(|line| line.split_once('='))
        .find(|(key, _)| *key == name)
        .expect("fixture");
    hex.as_bytes()
        .chunks_exact(2)
        .map(|pair| u8::from_str_radix(std::str::from_utf8(pair).expect("hex"), 16).expect("byte"))
        .collect()
}

fn sha256(raw: &[u8]) -> DigestV1 {
    DigestV1 {
        algorithm: DigestAlgorithmV1::Sha256 as i32,
        value: digest::digest(&digest::SHA256, raw).as_ref().to_vec(),
    }
}

fn signed_bytes(command: &WorkerCommandV1) -> Vec<u8> {
    sign_raw(command.encode_to_vec())
}

fn sign_raw(raw: Vec<u8>) -> Vec<u8> {
    let mut signed =
        SignedWorkerCommandEnvelopeV1::decode(vector("signed_command").as_slice()).unwrap();
    signed.worker_command_bytes = raw;
    signed.worker_command_digest = Some(sha256(&signed.worker_command_bytes));
    signed.signature = hmac::sign(
        &hmac::Key::new(
            hmac::HMAC_SHA256,
            b"ELITEA_RUNTIME_V1_TEST_ONLY_NOT_A_SECRET",
        ),
        &signed.worker_command_bytes,
    )
    .as_ref()
    .to_vec();
    signed.encode_to_vec()
}

fn fixture(kind: ToolkitCommandKind) -> (WorkerCommandV1, ClaimCommandResponseV1) {
    let signed =
        SignedWorkerCommandEnvelopeV1::decode(vector("signed_command").as_slice()).unwrap();
    let mut command = WorkerCommandV1::decode(signed.worker_command_bytes.as_slice()).unwrap();
    let mut response = ClaimCommandResponseV1::decode(vector("accepted_claim").as_slice()).unwrap();
    let receipt = response.receipt.as_mut().unwrap();
    let manifest = receipt.input_bundle.as_mut().unwrap();
    let mut settings = manifest.entries[0].clone();
    settings.entry_id = "toolkit-settings".into();
    settings.content.as_mut().unwrap().media_type = "application/json".into();
    settings.content.as_mut().unwrap().byte_length = 2;
    let mut context = settings.clone();
    context.entry_id = "toolkit-runtime-context".into();
    context.content.as_mut().unwrap().content_id = "context-content".into();
    match kind {
        ToolkitCommandKind::CallTool => {
            settings.semantic_role = "toolkit.call_tool.settings".into();
            context.semantic_role = "toolkit.call_tool.runtime_context".into();
            let mut arguments = settings.clone();
            arguments.entry_id = "toolkit-arguments".into();
            arguments.semantic_role = "toolkit.call_tool.arguments".into();
            arguments.content.as_mut().unwrap().content_id = "argument-content".into();
            arguments.content.as_mut().unwrap().digest = Some(sha256(b"arguments"));
            manifest.entries = vec![arguments, context, settings];
            command.command_type = WorkerCommandTypeV1::ToolkitCallTool as i32;
            command.capability_id = "toolkit.call_tool.v1".into();
            command.capability_command = Some(
                worker_command_v1::CapabilityCommand::ToolkitCallTool(ToolkitCallToolCommandV1 {
                    toolkit_type: "openapi".into(),
                    settings_entry_id: "toolkit-settings".into(),
                    tool_name: "echo".into(),
                    arguments_entry_id: "toolkit-arguments".into(),
                    toolkit_id: "12".into(),
                    toolkit_version: "1".into(),
                }),
            );
        }
        ToolkitCommandKind::AvailableTools => {
            settings.semantic_role = "toolkit.available_tools.settings".into();
            context.semantic_role = "toolkit.available_tools.runtime_context".into();
            manifest.entries = vec![context, settings];
            command.command_type = WorkerCommandTypeV1::ToolkitAvailableTools as i32;
            command.capability_id = "toolkit.available_tools.v1".into();
            command.capability_command =
                Some(worker_command_v1::CapabilityCommand::ToolkitAvailableTools(
                    ToolkitAvailableToolsCommandV1 {
                        toolkit_id: "19".into(),
                        toolkit_type: "openapi".into(),
                        settings_entry_id: "toolkit-settings".into(),
                    },
                ));
        }
        ToolkitCommandKind::ConfigurationValidate => {
            settings.entry_id = "settings".into();
            settings.semantic_role = "configuration.settings".into();
            manifest.entries = vec![settings];
            command.command_type = WorkerCommandTypeV1::ConfigurationValidate as i32;
            command.capability_id = "configuration.validate.v1".into();
            command.capability_command = Some(
                worker_command_v1::CapabilityCommand::ConfigurationValidation(
                    crate::validation::ConfigurationCatalog::pinned()
                        .unwrap()
                        .command_for("jira")
                        .unwrap(),
                ),
            );
        }
        ToolkitCommandKind::ExecuteRead => unreachable!(),
        ToolkitCommandKind::IndexIngest => {
            index_ingest_fixture(&mut command, manifest, &settings);
        }
    }
    rebind_manifest(&mut command, &mut response);
    (command, response)
}

/// The index bundle: the toolkit configuration, the tool parameters, a model
/// name and the embedding binding (its digest pinned by the command).
fn index_ingest_fixture(
    command: &mut WorkerCommandV1,
    manifest: &mut crate::protocol::elitea::runtime::v1::ExecutionInputBundleV1,
    settings: &crate::protocol::elitea::runtime::v1::ExecutionInputEntryV1,
) {
    let entry = |id: &str, role: &str, content_id: &str, digest: &[u8]| {
        let mut entry = settings.clone();
        entry.entry_id = id.into();
        entry.semantic_role = role.into();
        let content = entry.content.as_mut().unwrap();
        content.content_id = content_id.into();
        content.digest = Some(sha256(digest));
        entry
    };
    let configuration = entry(
        "index-toolkit-configuration",
        "index.toolkit_configuration",
        "configuration-content",
        b"configuration",
    );
    let parameters = entry(
        "index-tool-parameters",
        "index.tool_parameters",
        "parameters-content",
        b"parameters",
    );
    let model = entry(
        "index-llm-model",
        "index.llm_model",
        "model-content",
        b"model",
    );
    let binding = entry(
        "index-embedding-binding",
        "index.embedding_binding",
        "binding-content",
        b"binding",
    );
    let pinned = crate::protocol::elitea::runtime::v1::IndexIngestInputBindingV1 {
        entry_id: binding.entry_id.clone(),
        immutable_version: binding.immutable_version.clone(),
        content_digest: binding.content.as_ref().unwrap().digest.clone(),
    };
    manifest.entries = vec![configuration, parameters, model, binding];
    command.command_type = WorkerCommandTypeV1::IndexIngest as i32;
    command.capability_id = "index.ingest.v1".into();
    command.capability_version = "2".into();
    command.capability_command = Some(worker_command_v1::CapabilityCommand::IndexIngest(
        crate::protocol::elitea::runtime::v1::IndexIngestCommandV1 {
            toolkit_configuration_entry_id: "index-toolkit-configuration".into(),
            tool_parameters_entry_id: "index-tool-parameters".into(),
            llm_model_entry_id: "index-llm-model".into(),
            embedding_binding: Some(pinned),
            initiator: "user".into(),
            sio_event: "test_toolkit_tool".into(),
            index_id: "0b0f8c1e-6c1a-4d8e-9b1a-2f6d1c3e4a5b".into(),
            ..Default::default()
        },
    ));
}

fn rebind_manifest(command: &mut WorkerCommandV1, response: &mut ClaimCommandResponseV1) {
    let receipt = response.receipt.as_mut().unwrap();
    let raw = receipt.input_bundle.as_ref().unwrap().encode_to_vec();
    let reference = command.input_bundle_ref.as_mut().unwrap();
    reference.digest = Some(sha256(&raw));
    reference.byte_length = raw.len() as u64;
    receipt.input_bundle_ref = Some(reference.clone());
}

fn verified(command: &WorkerCommandV1) -> VerifiedToolkitExecuteReadCommand {
    parse_and_verify_toolkit_execute_read_command(
        &signed_bytes(command),
        Some(&TestOnlyConformanceHmacAuthenticator),
    )
    .unwrap()
}

fn claim(command: &WorkerCommandV1, response: ClaimCommandResponseV1) -> AcceptedAgentClaim {
    parse_accepted_agent_claim(&verified(command), response, "workload-1", "worker-1", NOW).unwrap()
}

fn call_result(
    claim: &AcceptedAgentClaim,
    status: ToolkitCallToolStatusV1,
) -> ToolkitCallToolResultV1 {
    let arguments = claim.arguments_entry.as_ref().unwrap();
    ToolkitCallToolResultV1 {
        toolkit_type: "openapi".into(),
        tool_name: "echo".into(),
        input_bundle_id: claim.input_bundle_ref.input_bundle_id.clone(),
        input_bundle_digest: claim.input_bundle_ref.digest.clone(),
        settings_entry_id: claim.request_entry.entry_id.clone(),
        settings_entry_version: claim.request_entry.immutable_version.clone(),
        settings_content_digest: claim.request_entry.content.as_ref().unwrap().digest.clone(),
        arguments_entry_id: arguments.entry_id.clone(),
        arguments_content_digest: arguments.content.as_ref().unwrap().digest.clone(),
        result_artifact: None,
        result_summary: Some(ToolkitCallToolSummaryV1 {
            authorization_required: None,
            status: status as i32,
            result_json: if status == ToolkitCallToolStatusV1::Ok {
                "{\"answer\":42}".into()
            } else {
                String::new()
            },
            error_message: if status == ToolkitCallToolStatusV1::Ok {
                String::new()
            } else {
                "The tool is unavailable.".into()
            },
            truncated: false,
        }),
    }
}

#[test]
fn shared_toolkit_commands_preserve_kind_and_admit_exact_entries() {
    for kind in [
        ToolkitCommandKind::CallTool,
        ToolkitCommandKind::AvailableTools,
    ] {
        let (command, response) = fixture(kind);
        let VerifiedExecutionCommandKind::ToolkitExecuteRead(verified) =
            parse_and_verify_execution_command(
                &signed_bytes(&command),
                Some(&TestOnlyConformanceHmacAuthenticator),
            )
            .unwrap()
        else {
            panic!("toolkit wrapper")
        };
        assert_eq!(verified.kind(), kind);
        assert_eq!(verified.request_entry_id(), "toolkit-settings");
        let execution = LeaseMonitoredAgentExecution {
            claim: claim(&command, response),
        };
        assert_eq!(execution.request_entry().entry_id, "toolkit-settings");
        assert!(
            execution
                .input_content_authority_for_entry("toolkit-runtime-context")
                .is_some()
        );
        assert!(
            execution
                .input_content_authority_for_entry("unbound")
                .is_none()
        );
        assert_eq!(
            execution.arguments_entry().is_some(),
            kind == ToolkitCommandKind::CallTool
        );
        if kind == ToolkitCommandKind::CallTool {
            assert_eq!(
                execution
                    .input_content_authority_for_entry("toolkit-arguments")
                    .unwrap()
                    .content_id,
                "argument-content"
            );
        }
    }
}

#[test]
fn shared_toolkit_command_rejects_wrong_kind_and_ambiguous_wire() {
    let (mut command, _) = fixture(ToolkitCommandKind::CallTool);
    command.capability_id = "toolkit.available_tools.v1".into();
    assert!(
        parse_and_verify_toolkit_execute_read_command(
            &signed_bytes(&command),
            Some(&TestOnlyConformanceHmacAuthenticator)
        )
        .is_err()
    );
    let (command, _) = fixture(ToolkitCommandKind::CallTool);
    let mut raw = command.encode_to_vec();
    raw.extend_from_slice(&[0x8a, 0x02, 0x00]);
    assert!(
        parse_and_verify_toolkit_execute_read_command(
            &sign_raw(raw),
            Some(&TestOnlyConformanceHmacAuthenticator)
        )
        .is_err()
    );
    let (mut command, _) = fixture(ToolkitCommandKind::CallTool);
    let Some(worker_command_v1::CapabilityCommand::ToolkitCallTool(call)) =
        command.capability_command.as_mut()
    else {
        panic!()
    };
    call.arguments_entry_id.clone_from(&call.settings_entry_id);
    assert!(
        parse_and_verify_toolkit_execute_read_command(
            &signed_bytes(&command),
            Some(&TestOnlyConformanceHmacAuthenticator)
        )
        .is_err()
    );
}

#[test]
fn shared_toolkit_claim_rejects_missing_context_wrong_roles_and_unbound_entries() {
    for kind in [
        ToolkitCommandKind::CallTool,
        ToolkitCommandKind::AvailableTools,
    ] {
        for mutation in 0..4 {
            let (mut command, mut response) = fixture(kind);
            let entries = &mut response
                .receipt
                .as_mut()
                .unwrap()
                .input_bundle
                .as_mut()
                .unwrap()
                .entries;
            match mutation {
                0 => entries.retain(|entry| entry.entry_id != "toolkit-runtime-context"),
                1 => entries[0].semantic_role = "unbound".into(),
                2 => entries.push(entries[0].clone()),
                3 => {
                    entries[0].content.as_mut().unwrap().required_grant_audience =
                        "wrong-audience".into();
                }
                _ => unreachable!(),
            }
            rebind_manifest(&mut command, &mut response);
            assert!(
                parse_accepted_agent_claim(
                    &verified(&command),
                    response,
                    "workload-1",
                    "worker-1",
                    NOW
                )
                .is_err()
            );
        }
    }
}

#[test]
fn shared_toolkit_claim_rejects_manifest_substitution() {
    let (command, mut response) = fixture(ToolkitCommandKind::CallTool);
    response
        .receipt
        .as_mut()
        .unwrap()
        .input_bundle
        .as_mut()
        .unwrap()
        .entries[0]
        .content
        .as_mut()
        .unwrap()
        .digest = Some(sha256(b"substitute"));
    assert!(matches!(
        parse_accepted_agent_claim(&verified(&command), response, "workload-1", "worker-1", NOW),
        Err(ControlSemanticError::AuthorizationFailed(_))
    ));
}

#[test]
fn shared_toolkit_call_output_binds_inputs_and_derives_refusal_outcome() {
    let (command, response) = fixture(ToolkitCommandKind::CallTool);
    let claim = claim(&command, response);
    let verified = verified(&command);
    for status in [
        ToolkitCallToolStatusV1::Ok,
        ToolkitCallToolStatusV1::ToolError,
        ToolkitCallToolStatusV1::UnknownTool,
        ToolkitCallToolStatusV1::UnsupportedToolkit,
    ] {
        let result = call_result(&claim, status);
        assert!(claim.matches_toolkit_call_tool_result_binding(&result));
        let frame = build_toolkit_execute_read_terminal_output_frame(
            &verified,
            &claim.fence,
            ToolkitExecuteReadTerminalOutput::CallTool(Box::new(result)),
            1,
            NOW,
            0,
        )
        .unwrap();
        validate_restored_toolkit_execute_read_output_frame(&verified, &frame).unwrap();
        assert_eq!(frame.logical_output_id, "toolkit-call-tool:execution-1");
        assert_eq!(
            frame
                .settlement_proposal
                .as_ref()
                .unwrap()
                .requested_outcome,
            if matches!(
                status,
                ToolkitCallToolStatusV1::UnknownTool | ToolkitCallToolStatusV1::UnsupportedToolkit
            ) {
                ExecutionOutcomeV1::Failed as i32
            } else {
                ExecutionOutcomeV1::Succeeded as i32
            }
        );
    }
    let mut result = call_result(&claim, ToolkitCallToolStatusV1::Ok);
    result.arguments_content_digest = Some(sha256(b"substitute"));
    assert!(!claim.matches_toolkit_call_tool_result_binding(&result));
    result.arguments_entry_id = "wrong-entry".into();
    assert!(
        build_toolkit_execute_read_terminal_output_frame(
            &verified,
            &claim.fence,
            ToolkitExecuteReadTerminalOutput::CallTool(Box::new(result)),
            1,
            NOW,
            0
        )
        .is_err()
    );
}

#[test]
fn shared_toolkit_discovery_output_binds_artifact_and_rejects_cross_capability_replay() {
    let (command, response) = fixture(ToolkitCommandKind::AvailableTools);
    let claim = claim(&command, response);
    let verified = verified(&command);
    let result = ToolkitAvailableToolsResultV1 {
        toolkit_type: "openapi".into(),
        input_bundle_id: claim.input_bundle_ref.input_bundle_id.clone(),
        input_bundle_digest: claim.input_bundle_ref.digest.clone(),
        settings_entry_id: claim.request_entry.entry_id.clone(),
        settings_entry_version: claim.request_entry.immutable_version.clone(),
        settings_content_digest: claim.request_entry.content.as_ref().unwrap().digest.clone(),
        result_artifact: Some(ToolkitAvailableToolsArtifactReferenceV1 {
            artifact_id: "discovery-result".into(),
            immutable_version: "1".into(),
            media_type: "application/vnd.elitea.toolkit-available-tools.v1+json".into(),
            byte_length: 2,
            digest: Some(sha256(b"{}")),
            classification: "tenant-confidential".into(),
        }),
    };
    assert!(claim.matches_toolkit_available_tools_result_binding(&result));
    let frame = build_toolkit_execute_read_terminal_output_frame(
        &verified,
        &claim.fence,
        ToolkitExecuteReadTerminalOutput::AvailableTools(Box::new(result)),
        1,
        NOW,
        0,
    )
    .unwrap();
    validate_restored_toolkit_execute_read_output_frame(&verified, &frame).unwrap();
    let (call_command, _) = fixture(ToolkitCommandKind::CallTool);
    let call_verified = super::shared_toolkit_tests::verified(&call_command);
    assert!(validate_restored_toolkit_execute_read_output_frame(&call_verified, &frame).is_err());
    let mut malformed = frame;
    let Some(execution_output_frame_v1::Payload::ToolkitAvailableTools(result)) =
        malformed.payload.as_mut()
    else {
        panic!()
    };
    result.result_artifact.as_mut().unwrap().digest = None;
    assert!(validate_restored_toolkit_execute_read_output_frame(&verified, &malformed).is_err());
}

#[test]
fn shared_toolkit_terminal_authority_rejects_changed_input_digest() {
    let (command, response) = fixture(ToolkitCommandKind::CallTool);
    let claim = claim(&command, response);
    let mut result = call_result(&claim, ToolkitCallToolStatusV1::Ok);
    result.arguments_content_digest = Some(sha256(b"changed-arguments"));
    let authority = AgentExecutionOutputAuthority { claim };
    assert!(matches!(
        authority.bind_toolkit_execute_read_terminal(
            &verified(&command),
            ToolkitExecuteReadTerminalOutput::CallTool(Box::new(result)),
            NOW
        ),
        Err(ProtocolError::AuthorizationFailed(_))
    ));
}

#[test]
fn shared_toolkit_failure_output_preserves_capability_during_recovery() {
    for kind in [
        ToolkitCommandKind::CallTool,
        ToolkitCommandKind::AvailableTools,
    ] {
        let (command, response) = fixture(kind);
        let claim = claim(&command, response);
        let verified = verified(&command);
        let frame = AgentExecutionOutputAuthority { claim }
            .bind_toolkit_execute_read_terminal(
                &verified,
                ToolkitExecuteReadTerminalOutput::Failure(RuntimeFailureKind::Cancelled),
                NOW,
            )
            .unwrap();
        validate_restored_toolkit_execute_read_output_frame(&verified, &frame).unwrap();
        assert_eq!(
            frame.logical_output_id,
            terminal_logical_output_id(&command)
        );
        assert_eq!(
            frame.settlement_proposal.unwrap().requested_outcome,
            ExecutionOutcomeV1::Cancelled as i32
        );
    }
}

fn authorization_result(claim: &AcceptedAgentClaim) -> ToolkitCallToolResultV1 {
    let mut result = call_result(claim, ToolkitCallToolStatusV1::AuthorizationRequired);
    let summary = result.result_summary.as_mut().unwrap();
    summary.error_message = crate::protocol::output::TOOLKIT_AUTHORIZATION_REQUIRED_MESSAGE.into();
    summary.authorization_required = Some(ToolkitAuthorizationRequiredV1 {
        toolkit_name: "configured toolkit".into(),
        toolkit_type: "openapi".into(),
        toolkit_id: "12".into(),
        server_url: "https://provider.example.test/mcp".into(),
        resource_metadata_url: "https://provider.example.test/.well-known/oauth-protected-resource"
            .into(),
        resource_metadata_json: serde_json::to_vec(&serde_json::json!({
            "authorization_servers": ["https://login.example.test"],
            "oauth_authorization_server": {
                "authorization_endpoint": "https://login.example.test/authorize",
                "token_endpoint": "https://login.example.test/token"
            },
            "toolkit_id": "12"
        }))
        .unwrap(),
    });
    result
}

#[test]
fn shared_toolkit_authorization_result_preserves_safe_challenge_and_succeeds() {
    let (command, response) = fixture(ToolkitCommandKind::CallTool);
    let claim = claim(&command, response);
    let verified = verified(&command);
    for with_metadata in [false, true] {
        let mut result = authorization_result(&claim);
        if !with_metadata {
            result
                .result_summary
                .as_mut()
                .unwrap()
                .authorization_required
                .as_mut()
                .unwrap()
                .resource_metadata_json
                .clear();
        }
        assert!(claim.matches_toolkit_call_tool_result_binding(&result));
        let expected = result.clone();
        let frame = build_toolkit_execute_read_terminal_output_frame(
            &verified,
            &claim.fence,
            ToolkitExecuteReadTerminalOutput::CallTool(Box::new(result)),
            1,
            NOW,
            0,
        )
        .unwrap();
        assert!(frame.encoded_len() <= crate::protocol::output::MAX_OUTPUT_FRAME_BYTES);
        assert_eq!(
            frame.payload,
            Some(execution_output_frame_v1::Payload::ToolkitCallTool(
                expected
            ))
        );
        assert_eq!(
            frame
                .settlement_proposal
                .as_ref()
                .unwrap()
                .requested_outcome,
            ExecutionOutcomeV1::Succeeded as i32
        );
        validate_restored_toolkit_execute_read_output_frame(&verified, &frame).unwrap();
    }
}

#[test]
fn shared_toolkit_authorization_result_rejects_ambiguous_status_and_raw_error_text() {
    let (command, response) = fixture(ToolkitCommandKind::CallTool);
    let claim = claim(&command, response);
    let verified = verified(&command);
    for mutation in 0..8 {
        let mut result = authorization_result(&claim);
        let summary = result.result_summary.as_mut().unwrap();
        match mutation {
            0 => summary.authorization_required = None,
            1 => summary.result_json = "{}".into(),
            2 => summary.truncated = true,
            3 => summary.error_message = "Provider response contains private content.".into(),
            4 => summary.status = ToolkitCallToolStatusV1::Ok as i32,
            5 => summary.status = ToolkitCallToolStatusV1::ToolError as i32,
            6 => summary.status = ToolkitCallToolStatusV1::UnknownTool as i32,
            7 => summary.status = ToolkitCallToolStatusV1::UnsupportedToolkit as i32,
            _ => unreachable!(),
        }
        assert!(
            build_toolkit_execute_read_terminal_output_frame(
                &verified,
                &claim.fence,
                ToolkitExecuteReadTerminalOutput::CallTool(Box::new(result)),
                1,
                NOW,
                0,
            )
            .is_err(),
            "summary mutation {mutation}"
        );
    }
}

#[test]
fn shared_toolkit_authorization_result_rejects_wrong_identity_unsafe_urls_and_secret_metadata() {
    let (command, response) = fixture(ToolkitCommandKind::CallTool);
    let claim = claim(&command, response);
    let verified = verified(&command);
    for mutation in 0..14 {
        let mut result = authorization_result(&claim);
        let challenge = result
            .result_summary
            .as_mut()
            .unwrap()
            .authorization_required
            .as_mut()
            .unwrap();
        match mutation {
            0 => challenge.toolkit_type = "mcp".into(),
            1 => challenge.toolkit_id = "13".into(),
            2 => challenge.toolkit_name = "invalid\nname".into(),
            3 => challenge.server_url = "http://provider.example.test".into(),
            4 => challenge.server_url = "https://user:secret@provider.example.test".into(),
            5 => challenge.resource_metadata_url = "https://provider.example.test/#fragment".into(),
            6 => challenge.server_url = "https://provider.example.test/\nsecret".into(),
            7 => challenge.resource_metadata_json = br#"{"toolkit_id":"13"}"#.to_vec(),
            8 => challenge.resource_metadata_json = br#"{"www_authenticate":"Bearer private"}"#.to_vec(),
            9 => challenge.resource_metadata_json = br#"{"authorization_servers":["https://login.example.test"],"provided_settings":{"mcp_client_id":"public-client","mcp_client_secret":"private-secret"}}"#.to_vec(),
            10 => challenge.resource_metadata_json = b"null".to_vec(),
            11 => challenge.resource_metadata_json = b"{ invalid json }".to_vec(),
            12 => challenge.resource_metadata_json.push(b' '),
            13 => challenge.resource_metadata_json = br#"{"authorization_servers":["https://login.example.test"],"authorization_servers":["https://other.example.test"]}"#.to_vec(),
            _ => unreachable!(),
        }
        assert!(
            build_toolkit_execute_read_terminal_output_frame(
                &verified,
                &claim.fence,
                ToolkitExecuteReadTerminalOutput::CallTool(Box::new(result)),
                1,
                NOW,
                0,
            )
            .is_err(),
            "challenge mutation {mutation}"
        );
    }
}

#[test]
fn shared_toolkit_authorization_result_bounds_metadata_urls_and_identity() {
    let (command, response) = fixture(ToolkitCommandKind::CallTool);
    let claim = claim(&command, response);
    let verified = verified(&command);
    for mutation in 0..4 {
        let mut result = authorization_result(&claim);
        let challenge = result
            .result_summary
            .as_mut()
            .unwrap()
            .authorization_required
            .as_mut()
            .unwrap();
        match mutation {
            0 => challenge.resource_metadata_json = vec![b' '; 16 * 1024 + 1],
            1 => {
                challenge.server_url =
                    format!("https://provider.example.test/{}", "x".repeat(4096));
            }
            2 => {
                challenge.resource_metadata_url =
                    format!("https://provider.example.test/{}", "x".repeat(4096));
            }
            3 => challenge.toolkit_name = "x".repeat(1025),
            _ => unreachable!(),
        }
        assert!(
            build_toolkit_execute_read_terminal_output_frame(
                &verified,
                &claim.fence,
                ToolkitExecuteReadTerminalOutput::CallTool(Box::new(result)),
                1,
                NOW,
                0,
            )
            .is_err(),
            "bound mutation {mutation}"
        );
    }
}

// ---------------------------------------------------------------------------
// configuration.validate.v1
// ---------------------------------------------------------------------------

const CASES_JSON: &str = include_str!("../../tests/fixtures/configuration_validation_cases.json");

fn catalog() -> &'static crate::validation::ConfigurationCatalog {
    crate::validation::ConfigurationCatalog::pinned().expect("embedded rules")
}

fn validation_command(configuration_type: &str) -> (WorkerCommandV1, ClaimCommandResponseV1) {
    let (mut command, response) = fixture(ToolkitCommandKind::ConfigurationValidate);
    command.capability_command = Some(
        worker_command_v1::CapabilityCommand::ConfigurationValidation(
            catalog().command_for(configuration_type).unwrap(),
        ),
    );
    (command, response)
}

fn validation_result(
    claim: &AcceptedAgentClaim,
    command: &ConfigurationValidationCommandV1,
    issues: &[crate::validation::Issue],
) -> ConfigurationValidationResultV1 {
    ConfigurationValidationResultV1 {
        configuration_revision_id: command.configuration_revision_id.clone(),
        configuration_type: command.configuration_type.clone(),
        catalog_revision: command.catalog_revision.clone(),
        catalog_digest: command.catalog_digest.clone(),
        schema_id: command.schema_id.clone(),
        schema_revision: command.schema_revision.clone(),
        schema_digest: command.schema_digest.clone(),
        input_bundle_id: claim.input_bundle_ref.input_bundle_id.clone(),
        input_bundle_digest: claim.input_bundle_ref.digest.clone(),
        settings_entry_id: claim.request_entry.entry_id.clone(),
        settings_entry_version: claim.request_entry.immutable_version.clone(),
        settings_content_digest: claim.request_entry.content.as_ref().unwrap().digest.clone(),
        valid: issues.is_empty(),
        issues: issues
            .iter()
            .map(|issue| ConfigurationValidationIssueV1 {
                code: issue.code.code().to_owned(),
                json_pointer: issue.json_pointer.clone(),
                safe_message: issue.code.safe_message().to_owned(),
            })
            .collect(),
    }
}

fn validation_command_message(command: &WorkerCommandV1) -> &ConfigurationValidationCommandV1 {
    let Some(worker_command_v1::CapabilityCommand::ConfigurationValidation(validation)) =
        command.capability_command.as_ref()
    else {
        panic!("validation command")
    };
    validation
}

#[test]
fn configuration_validation_command_is_a_verified_kind_with_its_own_output_identity() {
    let (command, response) = fixture(ToolkitCommandKind::ConfigurationValidate);
    let VerifiedExecutionCommandKind::ToolkitExecuteRead(verified) =
        parse_and_verify_execution_command(
            &signed_bytes(&command),
            Some(&TestOnlyConformanceHmacAuthenticator),
        )
        .unwrap()
    else {
        panic!("validation rides the direct-execution wrapper")
    };
    assert_eq!(verified.kind(), ToolkitCommandKind::ConfigurationValidate);
    assert_eq!(verified.request_entry_id(), "settings");
    // Main expects the revision-keyed id, not an execution-keyed one.
    assert_eq!(
        verified.logical_output_id(),
        "configuration-validation:revision-1"
    );
    assert_eq!(
        terminal_logical_output_id(&command),
        "configuration-validation:revision-1"
    );
    let execution = LeaseMonitoredAgentExecution {
        claim: claim(&command, response),
    };
    assert_eq!(execution.request_entry().entry_id, "settings");
    assert!(execution.arguments_entry().is_none());
    assert!(
        execution
            .input_content_authority_for_entry("toolkit-runtime-context")
            .is_none()
    );
}

#[test]
fn configuration_validation_command_rejects_malformed_and_ambiguous_wire() {
    let accepts = |command: &WorkerCommandV1| {
        parse_and_verify_execution_command(
            &signed_bytes(command),
            Some(&TestOnlyConformanceHmacAuthenticator),
        )
        .is_ok()
    };
    let (good, _) = fixture(ToolkitCommandKind::ConfigurationValidate);
    assert!(accepts(&good));
    for mutation in 0..9 {
        let mut command = good.clone();
        let Some(worker_command_v1::CapabilityCommand::ConfigurationValidation(validation)) =
            command.capability_command.as_mut()
        else {
            unreachable!()
        };
        match mutation {
            0 => command.capability_id = "toolkit.available_tools.v1".into(),
            1 => command.command_type = WorkerCommandTypeV1::ToolkitAvailableTools as i32,
            2 => command.capability_version = "2".into(),
            3 => {
                validation.catalog_digest.as_mut().unwrap().value.pop();
            }
            4 => validation.schema_digest = None,
            5 => validation.configuration_type.clear(),
            6 => validation.settings_entry_id.clear(),
            7 => validation.configuration_revision_id = "bad\nrevision".into(),
            8 => validation.schema_id = "x".repeat(257),
            _ => unreachable!(),
        }
        assert!(!accepts(&command), "mutation {mutation}");
    }
    // Wire-level: a second capability payload, and an unknown field inside it.
    let mut raw = good.encode_to_vec();
    raw.extend_from_slice(&[0x8a, 0x02, 0x00]);
    assert!(
        parse_and_verify_execution_command(
            &sign_raw(raw),
            Some(&TestOnlyConformanceHmacAuthenticator)
        )
        .is_err()
    );
    let mut with_unknown = validation_command_message(&good).encode_to_vec();
    with_unknown.extend_from_slice(&[0x4a, 0x00]); // field 9: reserved in v1
    let mut raw = good.clone();
    raw.capability_command = None;
    let mut raw = raw.encode_to_vec();
    raw.extend_from_slice(&[0x82, 0x02, u8::try_from(with_unknown.len()).unwrap()]);
    raw.extend_from_slice(&with_unknown);
    assert!(
        parse_and_verify_execution_command(
            &sign_raw(raw),
            Some(&TestOnlyConformanceHmacAuthenticator)
        )
        .is_err()
    );
}

#[test]
fn configuration_validation_claim_admits_exactly_the_settings_entry() {
    for mutation in 0..5 {
        let (mut command, mut response) = fixture(ToolkitCommandKind::ConfigurationValidate);
        let entries = &mut response
            .receipt
            .as_mut()
            .unwrap()
            .input_bundle
            .as_mut()
            .unwrap()
            .entries;
        match mutation {
            0 => entries[0].semantic_role = "toolkit.available_tools.settings".into(),
            1 => entries[0].content.as_mut().unwrap().media_type = "text/plain".into(),
            2 => {
                let mut context = entries[0].clone();
                context.entry_id = "toolkit-runtime-context".into();
                entries.push(context);
            }
            3 => entries[0].content.as_mut().unwrap().byte_length = 256 * 1024 + 1,
            4 => entries[0].content.as_mut().unwrap().required_grant_audience = "other".into(),
            _ => unreachable!(),
        }
        rebind_manifest(&mut command, &mut response);
        assert!(
            parse_accepted_agent_claim(
                &verified(&command),
                response,
                "workload-1",
                "worker-1",
                NOW
            )
            .is_err(),
            "mutation {mutation}"
        );
    }
}

#[test]
fn configuration_validation_output_binds_the_admitted_input_and_succeeds_when_invalid() {
    let (command, response) = fixture(ToolkitCommandKind::ConfigurationValidate);
    let claim = claim(&command, response);
    let verified = verified(&command);
    let message = validation_command_message(&command).clone();
    let invalid = [
        crate::validation::Issue {
            code: crate::validation::IssueCode::InvalidValue,
            json_pointer: "/base_url".into(),
        },
        crate::validation::Issue {
            code: crate::validation::IssueCode::RequiredField,
            json_pointer: "/token".into(),
        },
    ];
    for issues in [&invalid[..], &[]] {
        let result = validation_result(&claim, &message, issues);
        assert!(claim.matches_configuration_validation_result_binding(&result));
        let frame = build_toolkit_execute_read_terminal_output_frame(
            &verified,
            &claim.fence,
            ToolkitExecuteReadTerminalOutput::ConfigurationValidation(Box::new(result)),
            1,
            NOW,
            0,
        )
        .unwrap();
        validate_restored_toolkit_execute_read_output_frame(&verified, &frame).unwrap();
        assert_eq!(
            frame.logical_output_id,
            "configuration-validation:revision-1"
        );
        assert_eq!(
            frame.event_type,
            ExecutionOutputEventTypeV1::ConfigurationValidationResult as i32
        );
        assert!(frame.terminal);
        let proposal = frame.settlement_proposal.as_ref().unwrap();
        // An invalid configuration is a completed validation, not a failure.
        assert_eq!(
            proposal.requested_outcome,
            ExecutionOutcomeV1::Succeeded as i32
        );
        assert_eq!(proposal.terminal_logical_output_id, frame.logical_output_id);
    }
}

#[test]
fn configuration_validation_restored_frame_accepts_every_code_main_accepts() {
    // Main's vocabulary is six codes. This worker emits four of them, but a
    // restored frame is validated against what Main accepts, not against what
    // this build happens to emit.
    let (command, response) = fixture(ToolkitCommandKind::ConfigurationValidate);
    let claim = claim(&command, response);
    let verified = verified(&command);
    let message = validation_command_message(&command).clone();
    let codes = crate::validation::IssueCode::ALL;
    assert_eq!(codes.len(), 6);
    for code in codes {
        let result = validation_result(
            &claim,
            &message,
            &[crate::validation::Issue {
                code,
                json_pointer: "/field".into(),
            }],
        );
        let frame = build_toolkit_execute_read_terminal_output_frame(
            &verified,
            &claim.fence,
            ToolkitExecuteReadTerminalOutput::ConfigurationValidation(Box::new(result)),
            1,
            NOW,
            0,
        )
        .unwrap_or_else(|error| panic!("{}: {error:?}", code.code()));
        validate_restored_toolkit_execute_read_output_frame(&verified, &frame)
            .unwrap_or_else(|error| panic!("{}: {error:?}", code.code()));
        let Some(execution_output_frame_v1::Payload::ConfigurationValidation(restored)) =
            frame.payload.as_ref()
        else {
            panic!("{}: not a validation payload", code.code());
        };
        assert_eq!(restored.issues[0].code, code.code());
        assert_eq!(restored.issues[0].safe_message, code.safe_message());
    }
    // All six together, in canonical order, is one well-formed result.
    let mut all: Vec<_> = codes
        .iter()
        .map(|code| crate::validation::Issue {
            code: *code,
            json_pointer: format!("/{}", code.code().to_ascii_lowercase()),
        })
        .collect();
    all.sort_by(|a, b| (&a.json_pointer, a.code).cmp(&(&b.json_pointer, b.code)));
    let result = validation_result(&claim, &message, &all);
    let frame = build_toolkit_execute_read_terminal_output_frame(
        &verified,
        &claim.fence,
        ToolkitExecuteReadTerminalOutput::ConfigurationValidation(Box::new(result)),
        1,
        NOW,
        0,
    )
    .unwrap();
    validate_restored_toolkit_execute_read_output_frame(&verified, &frame).unwrap();
}

#[test]
fn configuration_validation_output_rejects_every_unbound_or_noncanonical_result() {
    use crate::validation::IssueCode::{InvalidValue, RequiredField};
    let (command, response) = fixture(ToolkitCommandKind::ConfigurationValidate);
    let claim = claim(&command, response);
    let verified = verified(&command);
    let message = validation_command_message(&command).clone();
    let issue = |code, pointer: &str| crate::validation::Issue {
        code,
        json_pointer: pointer.to_owned(),
    };
    for mutation in 0..16 {
        let mut result = validation_result(&claim, &message, &[issue(InvalidValue, "/a")]);
        match mutation {
            0 => result.configuration_revision_id = "other".into(),
            1 => result.configuration_type = "confluence".into(),
            2 => result.catalog_revision = "other".into(),
            3 => result.schema_digest = Some(sha256(b"substitute")),
            4 => result.input_bundle_id = "other".into(),
            5 => result.input_bundle_digest = Some(sha256(b"substitute")),
            6 => result.settings_entry_id = "other".into(),
            7 => result.settings_entry_version.clear(),
            8 => result.valid = true,
            9 => result.issues.clear(),
            10 => result.issues[0].code = "FREE_TEXT".into(),
            11 => result.issues[0].safe_message = "token abc was rejected".into(),
            12 => result.issues.push(result.issues[0].clone()),
            13 => {
                result.issues = vec![
                    ConfigurationValidationIssueV1 {
                        code: "INVALID_VALUE".into(),
                        json_pointer: "/b".into(),
                        safe_message: InvalidValue.safe_message().into(),
                    },
                    ConfigurationValidationIssueV1 {
                        code: "INVALID_VALUE".into(),
                        json_pointer: "/a".into(),
                        safe_message: InvalidValue.safe_message().into(),
                    },
                ];
            }
            14 => result.issues[0].json_pointer = "/".to_owned() + &"x".repeat(256),
            15 => {
                result.issues = (0..65)
                    .map(|index| ConfigurationValidationIssueV1 {
                        code: RequiredField.code().into(),
                        json_pointer: format!("/f{index:03}"),
                        safe_message: RequiredField.safe_message().into(),
                    })
                    .collect();
            }
            _ => unreachable!(),
        }
        assert!(
            build_toolkit_execute_read_terminal_output_frame(
                &verified,
                &claim.fence,
                ToolkitExecuteReadTerminalOutput::ConfigurationValidation(Box::new(result)),
                1,
                NOW,
                0,
            )
            .is_err(),
            "mutation {mutation}"
        );
    }
    // The authority layer additionally pins the result to the admitted claim.
    let mut result = validation_result(&claim, &message, &[]);
    result.settings_content_digest = Some(sha256(b"changed-settings"));
    assert!(matches!(
        AgentExecutionOutputAuthority { claim }.bind_toolkit_execute_read_terminal(
            &verified,
            ToolkitExecuteReadTerminalOutput::ConfigurationValidation(Box::new(result)),
            NOW
        ),
        Err(ProtocolError::AuthorizationFailed(_))
    ));
}

#[test]
fn configuration_validation_output_does_not_replay_across_capabilities_or_revisions() {
    let (command, response) = fixture(ToolkitCommandKind::ConfigurationValidate);
    let claim = claim(&command, response);
    let verified = verified(&command);
    let message = validation_command_message(&command).clone();
    let frame = build_toolkit_execute_read_terminal_output_frame(
        &verified,
        &claim.fence,
        ToolkitExecuteReadTerminalOutput::ConfigurationValidation(Box::new(validation_result(
            &claim,
            &message,
            &[],
        ))),
        1,
        NOW,
        0,
    )
    .unwrap();
    let (other, _) = fixture(ToolkitCommandKind::AvailableTools);
    assert!(
        validate_restored_toolkit_execute_read_output_frame(
            &super::shared_toolkit_tests::verified(&other),
            &frame
        )
        .is_err()
    );
    let (mut other_revision, _) = fixture(ToolkitCommandKind::ConfigurationValidate);
    let Some(worker_command_v1::CapabilityCommand::ConfigurationValidation(validation)) =
        other_revision.capability_command.as_mut()
    else {
        unreachable!()
    };
    validation.configuration_revision_id = "revision-2".into();
    assert!(
        validate_restored_toolkit_execute_read_output_frame(&verified_for(&other_revision), &frame)
            .is_err()
    );
}

fn verified_for(command: &WorkerCommandV1) -> VerifiedToolkitExecuteReadCommand {
    verified(command)
}

#[test]
fn configuration_validation_failure_output_keeps_the_revision_keyed_identity() {
    let (command, response) = fixture(ToolkitCommandKind::ConfigurationValidate);
    let claim = claim(&command, response);
    let verified = verified(&command);
    for failure in [
        RuntimeFailureKind::UnsupportedCapability,
        RuntimeFailureKind::IncompatibleVersion,
        RuntimeFailureKind::InvalidInput,
        RuntimeFailureKind::ResourceExhausted,
        RuntimeFailureKind::DeadlineExceeded,
        RuntimeFailureKind::Cancelled,
    ] {
        let frame = build_toolkit_execute_read_terminal_output_frame(
            &verified,
            &claim.fence,
            ToolkitExecuteReadTerminalOutput::Failure(failure),
            1,
            NOW,
            0,
        )
        .unwrap();
        validate_restored_toolkit_execute_read_output_frame(&verified, &frame).unwrap();
        assert_eq!(
            frame.logical_output_id,
            "configuration-validation:revision-1"
        );
        assert_eq!(
            frame.event_type,
            ExecutionOutputEventTypeV1::RuntimeError as i32
        );
    }
}

#[derive(serde::Deserialize)]
struct RecordedCases {
    cases: Vec<RecordedCase>,
}

#[derive(serde::Deserialize)]
struct RecordedCase {
    #[serde(rename = "type")]
    configuration_type: String,
    settings: String,
    valid: bool,
}

/// The fixture transport for every covered type: a signed command bound to the
/// pinned catalog, an admitted claim, the real evaluator over a recorded valid
/// and a recorded invalid document, and a terminal frame that a restarted
/// worker restores and Main would accept.
#[test]
fn every_covered_configuration_type_round_trips_through_the_wire() {
    let recorded: RecordedCases = serde_json::from_str(CASES_JSON).unwrap();
    let mut covered = 0;
    for name in catalog().type_names() {
        let (command, response) = validation_command(name);
        let claim = claim(&command, response);
        let verified = verified(&command);
        let message = validation_command_message(&command).clone();
        let rules = catalog().bind(&message).unwrap();
        let mut outcomes = std::collections::BTreeSet::new();
        for want_valid in [true, false] {
            let case = recorded
                .cases
                .iter()
                .find(|case| case.configuration_type == name && case.valid == want_valid)
                .unwrap_or_else(|| panic!("{name} has no recorded valid={want_valid} case"));
            let issues = rules.evaluate(case.settings.as_bytes()).unwrap();
            assert_eq!(issues.is_empty(), want_valid, "{name}");
            let result = validation_result(&claim, &message, &issues);
            let frame = AgentExecutionOutputAuthority {
                claim: claim_again(&command),
            }
            .bind_toolkit_execute_read_terminal(
                &verified,
                ToolkitExecuteReadTerminalOutput::ConfigurationValidation(Box::new(result)),
                NOW,
            )
            .unwrap();
            validate_restored_toolkit_execute_read_output_frame(&verified, &frame).unwrap();
            let Some(execution_output_frame_v1::Payload::ConfigurationValidation(sent)) =
                frame.payload
            else {
                panic!("validation payload")
            };
            assert_eq!(sent.valid, want_valid);
            assert_eq!(sent.configuration_type, name);
            outcomes.insert(sent.valid);
        }
        assert_eq!(outcomes.len(), 2, "{name} proves both verdicts");
        covered += 1;
    }
    assert_eq!(covered, 32);
}

fn claim_again(command: &WorkerCommandV1) -> AcceptedAgentClaim {
    let (_, response) = fixture(ToolkitCommandKind::ConfigurationValidate);
    claim(command, response)
}

// ── the per-claim elitea-vector token (ADR-0031 decision 1) ────────────────

#[test]
fn the_vector_token_rides_with_the_claim_to_toolkit_and_agent_code() {
    use crate::protocol::vector_token::{VectorSource, test_vector_claim_token};

    let (command, mut response) = fixture(ToolkitCommandKind::CallTool);
    let wire = test_vector_claim_token();
    let bearer = wire.bearer.clone();
    response.receipt.as_mut().unwrap().vector_token = Some(wire);
    let claim = claim(&command, response);

    // The toolkit path: the claim, then the lease-monitored execution.
    let token = claim.vector_token().expect("carried");
    assert_eq!(token.bearer(), bearer);
    assert!(token.allows(VectorSource::ToolkitIndex));
    assert!(!token.allows(VectorSource::Deepwiki));

    // The agent path: the runtime-context authority derived at AUTHORIZED_NOW.
    let runtime = ClaimBoundRuntimeContextAuthority::from_claim(&claim);
    assert_eq!(
        runtime.vector_token().expect("carried").bearer(),
        bearer.as_str()
    );

    let execution = LeaseMonitoredAgentExecution { claim };
    assert_eq!(
        execution.vector_token().expect("carried").bearer(),
        bearer.as_str()
    );
    // Nothing that prints the claim's token shows the bearer.
    let printed = format!("{:?}", execution.vector_token());
    assert!(!printed.contains(&bearer), "{printed}");
}

#[test]
fn a_claim_without_a_vector_token_carries_none() {
    let (command, response) = fixture(ToolkitCommandKind::CallTool);
    let claim = claim(&command, response);
    assert!(claim.vector_token().is_none());
    assert!(
        ClaimBoundRuntimeContextAuthority::from_claim(&claim)
            .vector_token()
            .is_none()
    );
}

/// The token only gates vector calls: whatever the receipt's token looks
/// like, the claim itself is accepted.
#[test]
fn a_bad_vector_token_never_fails_the_claim() {
    use crate::protocol::vector_token::{VectorSource, test_vector_claim_token};

    for (name, wire) in [
        (
            "malformed bearer",
            VectorClaimTokenV1 {
                bearer: "garbage".to_owned(),
                ..test_vector_claim_token()
            },
        ),
        (
            "no usable source",
            VectorClaimTokenV1 {
                allowed_sources: vec!["everything".to_owned()],
                ..test_vector_claim_token()
            },
        ),
        (
            "no expiry",
            VectorClaimTokenV1 {
                expires_at_unix_millis: 0,
                ..test_vector_claim_token()
            },
        ),
    ] {
        let (command, mut response) = fixture(ToolkitCommandKind::CallTool);
        response.receipt.as_mut().unwrap().vector_token = Some(wire);
        let claim = parse_accepted_agent_claim(
            &verified(&command),
            response,
            "workload-1",
            "worker-1",
            NOW,
        )
        .unwrap_or_else(|error| panic!("{name}: the claim was refused: {error:?}"));
        assert!(claim.vector_token().is_none(), "{name}");
    }

    // An unknown source beside a known one: the claim keeps the known one.
    let (command, mut response) = fixture(ToolkitCommandKind::CallTool);
    response.receipt.as_mut().unwrap().vector_token = Some(VectorClaimTokenV1 {
        allowed_sources: vec!["toolkit_index".to_owned(), "a_future_source".to_owned()],
        ..test_vector_claim_token()
    });
    let claim =
        parse_accepted_agent_claim(&verified(&command), response, "workload-1", "worker-1", NOW)
            .expect("accepted");
    let token = claim.vector_token().expect("carried");
    assert!(token.allows(VectorSource::ToolkitIndex));
}

// ── index.ingest.v1 (ADR-0030 I1) ────────────────────────────────────────────

fn index_command_message(command: &WorkerCommandV1) -> &IndexIngestCommandV1 {
    let Some(worker_command_v1::CapabilityCommand::IndexIngest(index)) =
        command.capability_command.as_ref()
    else {
        panic!("index command")
    };
    index
}

type Mutation = Box<dyn Fn(&mut IndexIngestResultV1)>;

fn index_summary() -> IndexIngestSummaryV1 {
    IndexIngestSummaryV1 {
        status: IndexIngestStatusV1::Ok as i32,
        message: "Index 'docs': 2 documents indexed (5 chunks).".to_owned(),
        terminal_state: IndexIngestTerminalStateV1::Completed as i32,
        indexed: 2,
        updated: 0,
        reindex: Some(false),
        indexed_documents: 2,
        indexed_chunks: 5,
        embedding_model: "text-embedding-3-small".to_owned(),
        embedding_dimension: 1536,
        documents: vec![
            IndexIngestDocumentV1 {
                document_key: "README.md".to_owned(),
                version: "abc".to_owned(),
                chunk_count: 2,
            },
            IndexIngestDocumentV1 {
                document_key: "src/app.py".to_owned(),
                version: "def".to_owned(),
                chunk_count: 3,
            },
        ],
        total_documents: Some(2),
        total_chunks: Some(5),
        whole_index_record: true,
        ..Default::default()
    }
}

fn index_result(claim: &AcceptedAgentClaim, summary: IndexIngestSummaryV1) -> IndexIngestResultV1 {
    let binding = |entry: &crate::protocol::elitea::runtime::v1::ExecutionInputEntryV1| {
        IndexIngestInputBindingV1 {
            entry_id: entry.entry_id.clone(),
            immutable_version: entry.immutable_version.clone(),
            content_digest: entry.content.as_ref().unwrap().digest.clone(),
        }
    };
    let by_id = |id: &str| binding(claim.input_entry(id).expect("entry"));
    IndexIngestResultV1 {
        input_bundle_id: claim.input_bundle_ref.input_bundle_id.clone(),
        input_bundle_digest: claim.input_bundle_ref.digest.clone(),
        toolkit_configuration: Some(binding(&claim.request_entry)),
        tool_parameters: Some(binding(claim.arguments_entry.as_ref().unwrap())),
        llm_model: Some(by_id("index-llm-model")),
        embedding_binding: Some(by_id("index-embedding-binding")),
        result_summary: Some(summary),
        ..Default::default()
    }
}

#[test]
fn index_ingest_command_is_a_verified_kind_with_the_execution_keyed_output_identity() {
    let (command, response) = fixture(ToolkitCommandKind::IndexIngest);
    let VerifiedExecutionCommandKind::ToolkitExecuteRead(verified) =
        parse_and_verify_execution_command(
            &signed_bytes(&command),
            Some(&TestOnlyConformanceHmacAuthenticator),
        )
        .unwrap()
    else {
        panic!("index ingest rides the direct-execution wrapper")
    };
    assert_eq!(verified.kind(), ToolkitCommandKind::IndexIngest);
    assert_eq!(verified.request_entry_id(), "index-toolkit-configuration");
    let expected = format!("index-ingest:{}", command.execution_id);
    assert_eq!(verified.logical_output_id(), expected);
    assert_eq!(terminal_logical_output_id(&command), expected);
    let execution = LeaseMonitoredAgentExecution {
        claim: claim(&command, response),
    };
    assert_eq!(
        execution.request_entry().entry_id,
        "index-toolkit-configuration"
    );
    assert_eq!(
        execution
            .arguments_entry()
            .map(|entry| entry.entry_id.as_str()),
        Some("index-tool-parameters")
    );
    // Every bundle entry is fetchable by id, nothing else is.
    for id in [
        "index-toolkit-configuration",
        "index-tool-parameters",
        "index-llm-model",
        "index-embedding-binding",
    ] {
        assert!(
            execution.input_content_authority_for_entry(id).is_some(),
            "{id}"
        );
    }
    assert!(
        execution
            .input_content_authority_for_entry("toolkit-runtime-context")
            .is_none()
    );
}

#[test]
fn index_ingest_command_rejects_malformed_identities_and_wire() {
    let accepts = |command: &WorkerCommandV1| {
        parse_and_verify_execution_command(
            &signed_bytes(command),
            Some(&TestOnlyConformanceHmacAuthenticator),
        )
        .is_ok()
    };
    let (good, _) = fixture(ToolkitCommandKind::IndexIngest);
    assert!(accepts(&good));
    for mutation in 0..13 {
        let mut command = good.clone();
        let Some(worker_command_v1::CapabilityCommand::IndexIngest(index)) =
            command.capability_command.as_mut()
        else {
            unreachable!()
        };
        match mutation {
            0 => command.capability_id = "toolkit.call_tool.v1".into(),
            1 => command.command_type = WorkerCommandTypeV1::ConfigurationValidate as i32,
            2 => command.capability_version = "1".into(),
            3 => index.toolkit_configuration_entry_id.clear(),
            4 => index.tool_parameters_entry_id = index.toolkit_configuration_entry_id.clone(),
            5 => index.initiator = "cron".into(),
            6 => index.sio_event = "other".into(),
            7 => index.expected_embedding_model = "m".into(),
            8 => {
                index.expected_embedding_model = "m".into();
                index.expected_embedding_dimension = 70_000;
            }
            9 => index.index_id = "not-a-uuid".into(),
            10 => index.index_id = "0B0F8C1E-6C1A-4D8E-9B1A-2F6D1C3E4A5B".into(),
            11 => index.embedding_binding.as_mut().unwrap().content_digest = None,
            12 => index.client_stream_id = "x".repeat(257),
            _ => unreachable!(),
        }
        assert!(!accepts(&command), "mutation {mutation}");
    }
    // The pair may be absent (the first run stamps the index). The index id
    // may not: a command without one is the Python runtime's and is refused
    // as unsupported, before any claim, as every index command was before
    // this worker served the capability.
    let mut unstamped = good.clone();
    let Some(worker_command_v1::CapabilityCommand::IndexIngest(index)) =
        unstamped.capability_command.as_mut()
    else {
        unreachable!()
    };
    index.expected_embedding_model.clear();
    index.expected_embedding_dimension = 0;
    assert!(accepts(&unstamped));
    let mut python_runtime = good.clone();
    let Some(worker_command_v1::CapabilityCommand::IndexIngest(index)) =
        python_runtime.capability_command.as_mut()
    else {
        unreachable!()
    };
    index.index_id.clear();
    let refused = parse_and_verify_execution_command(
        &signed_bytes(&python_runtime),
        Some(&TestOnlyConformanceHmacAuthenticator),
    );
    assert!(
        matches!(refused, Err(ProtocolError::UnsupportedCapability(_))),
        "a command without an index id must be refused as unsupported: {:?}",
        refused.err()
    );
    // Wire-level: a reserved field inside the index command is refused.
    let mut with_unknown = index_command_message(&good).encode_to_vec();
    with_unknown.extend_from_slice(&[0x52, 0x00]); // field 10: reserved
    let mut raw = good.clone();
    raw.capability_command = None;
    let mut raw = raw.encode_to_vec();
    raw.extend_from_slice(&[0x92, 0x02, u8::try_from(with_unknown.len()).unwrap()]);
    raw.extend_from_slice(&with_unknown);
    assert!(
        parse_and_verify_execution_command(
            &sign_raw(raw),
            Some(&TestOnlyConformanceHmacAuthenticator)
        )
        .is_err()
    );
}

#[test]
fn index_ingest_claim_admits_exactly_the_bundles_entries() {
    for mutation in 0..6 {
        let (mut command, mut response) = fixture(ToolkitCommandKind::IndexIngest);
        let entries = &mut response
            .receipt
            .as_mut()
            .unwrap()
            .input_bundle
            .as_mut()
            .unwrap()
            .entries;
        match mutation {
            0 => entries[0].semantic_role = "index.tool_parameters".into(),
            1 => entries[1].content.as_mut().unwrap().media_type = "text/plain".into(),
            2 => {
                let mut extra = entries[0].clone();
                extra.entry_id = "index-mcp-tokens".into();
                extra.semantic_role = "index.mcp_tokens".into();
                entries.push(extra);
            }
            3 => {
                entries.remove(2);
            }
            4 => entries[3].content.as_mut().unwrap().digest = Some(sha256(b"other")),
            5 => entries[3].immutable_version = "other".into(),
            _ => unreachable!(),
        }
        rebind_manifest(&mut command, &mut response);
        assert!(
            parse_accepted_agent_claim(
                &verified(&command),
                response,
                "workload-1",
                "worker-1",
                NOW
            )
            .is_err(),
            "mutation {mutation}"
        );
    }
}

#[test]
fn index_ingest_output_binds_every_entry_and_derives_the_outcome_from_the_status() {
    let (command, response) = fixture(ToolkitCommandKind::IndexIngest);
    let claim = claim(&command, response);
    let verified = verified(&command);
    let frame = AgentExecutionOutputAuthority {
        claim: claim_again_for(&command),
    }
    .bind_toolkit_execute_read_terminal(
        &verified,
        ToolkitExecuteReadTerminalOutput::IndexIngest(Box::new(index_result(
            &claim,
            index_summary(),
        ))),
        NOW,
    )
    .unwrap();
    assert_eq!(
        frame.event_type,
        ExecutionOutputEventTypeV1::IndexIngestResult as i32
    );
    assert_eq!(frame.logical_output_id, verified.logical_output_id());
    let proposal = frame.settlement_proposal.as_ref().unwrap();
    assert_eq!(
        proposal.requested_outcome,
        crate::protocol::elitea::runtime::v1::ExecutionOutcomeV1::Succeeded as i32
    );
    assert!(validate_restored_toolkit_execute_read_output_frame(&verified, &frame).is_ok());

    let mut failed = index_summary();
    failed.status = IndexIngestStatusV1::Error as i32;
    failed.terminal_state = IndexIngestTerminalStateV1::Failed as i32;
    failed.embedding_model.clear();
    failed.embedding_dimension = 0;
    let frame = AgentExecutionOutputAuthority {
        claim: claim_again_for(&command),
    }
    .bind_toolkit_execute_read_terminal(
        &verified,
        ToolkitExecuteReadTerminalOutput::IndexIngest(Box::new(index_result(&claim, failed))),
        NOW,
    )
    .unwrap();
    assert_eq!(
        frame.settlement_proposal.unwrap().requested_outcome,
        crate::protocol::elitea::runtime::v1::ExecutionOutcomeV1::Failed as i32
    );
}

#[test]
fn index_ingest_output_rejects_unbound_entries_and_summaries_outside_mains_bounds() {
    let (command, response) = fixture(ToolkitCommandKind::IndexIngest);
    let accepted = claim(&command, response);
    let good = verified(&command);
    let bind = |result: IndexIngestResultV1| {
        AgentExecutionOutputAuthority {
            claim: claim_again_for(&command),
        }
        .bind_toolkit_execute_read_terminal(
            &good,
            ToolkitExecuteReadTerminalOutput::IndexIngest(Box::new(result)),
            NOW,
        )
        .is_ok()
    };
    assert!(bind(index_result(&accepted, index_summary())));
    let mutations: Vec<Mutation> = vec![
        Box::new(|r| r.toolkit_configuration.as_mut().unwrap().entry_id = "other".into()),
        Box::new(|r| r.tool_parameters = None),
        Box::new(|r| r.llm_model = None),
        Box::new(|r| r.embedding_binding.as_mut().unwrap().content_digest = Some(sha256(b"x"))),
        Box::new(|r| r.input_bundle_digest = Some(sha256(b"x"))),
        Box::new(|r| r.result_summary = None),
        Box::new(|r| r.result_summary.as_mut().unwrap().message.clear()),
        Box::new(|r| r.result_summary.as_mut().unwrap().status = 0),
        Box::new(|r| {
            // A completed state on an error status.
            r.result_summary.as_mut().unwrap().status = IndexIngestStatusV1::Error as i32;
        }),
        Box::new(|r| {
            let summary = r.result_summary.as_mut().unwrap();
            summary.embedding_dimension = 0;
        }),
        Box::new(|r| {
            let summary = r.result_summary.as_mut().unwrap();
            summary.skipped.insert("Needs Model".into(), 1);
        }),
        Box::new(|r| {
            let summary = r.result_summary.as_mut().unwrap();
            summary.documents[1].document_key = summary.documents[0].document_key.clone();
        }),
        Box::new(|r| {
            let summary = r.result_summary.as_mut().unwrap();
            summary.removed_document_keys = vec![summary.documents[0].document_key.clone()];
        }),
        Box::new(|r| {
            // Half a totals pair.
            r.result_summary.as_mut().unwrap().total_chunks = None;
        }),
        Box::new(|r| {
            // A whole-index statement over a truncated list.
            r.result_summary.as_mut().unwrap().documents_truncated = true;
        }),
        Box::new(|r| {
            // A whole-index statement whose totals do not count its list.
            r.result_summary.as_mut().unwrap().total_documents = Some(3);
        }),
        Box::new(|r| {
            // A whole-index statement without totals.
            let summary = r.result_summary.as_mut().unwrap();
            summary.total_documents = None;
            summary.total_chunks = None;
        }),
        Box::new(|r| {
            // A success that names another space than the command expects.
            r.result_summary.as_mut().unwrap().embedding_dimension = 3;
        }),
    ];
    // The last mutation needs an expected space on the command.
    for (position, mutate) in mutations.iter().enumerate() {
        let mut result = index_result(&accepted, index_summary());
        mutate(&mut result);
        if position == mutations.len() - 1 {
            let mut stamped = command.clone();
            let Some(worker_command_v1::CapabilityCommand::IndexIngest(index)) =
                stamped.capability_command.as_mut()
            else {
                unreachable!()
            };
            index.expected_embedding_model = "text-embedding-3-small".into();
            index.expected_embedding_dimension = 1536;
            let (_, response) = fixture(ToolkitCommandKind::IndexIngest);
            let stamped_claim = claim(&stamped, response);
            let rejected = AgentExecutionOutputAuthority {
                claim: stamped_claim,
            }
            .bind_toolkit_execute_read_terminal(
                &verified(&stamped),
                ToolkitExecuteReadTerminalOutput::IndexIngest(Box::new(result)),
                NOW,
            )
            .is_err();
            assert!(rejected, "mutation {position}");
            continue;
        }
        assert!(!bind(result), "mutation {position}");
    }
}

/// Main settles exactly what was proposed or rejects the frame; a receipt
/// with another outcome (FAILED for a SUCCEEDED index proposal included) is
/// malformed.
#[test]
fn a_settlement_receipt_must_carry_the_proposed_outcome() {
    use crate::protocol::elitea::runtime::v1::{ExecutionOutcomeV1, PrepareSettlementResponseV1};
    let (command, response) = fixture(ToolkitCommandKind::IndexIngest);
    let claim = claim(&command, response);
    let verified = verified(&command);
    let frame = AgentExecutionOutputAuthority {
        claim: claim_again_for(&command),
    }
    .bind_toolkit_execute_read_terminal(
        &verified,
        ToolkitExecuteReadTerminalOutput::IndexIngest(Box::new(index_result(
            &claim,
            index_summary(),
        ))),
        NOW,
    )
    .unwrap();
    let proposed =
        ExecutionOutcomeV1::try_from(frame.settlement_proposal.clone().unwrap().requested_outcome)
            .unwrap();
    assert_eq!(proposed, ExecutionOutcomeV1::Succeeded);
    let receipt = parse_prepare_settlement_response(
        PrepareSettlementResponseV1 {
            settlement_receipt_id: "receipt-1".into(),
            outcome: ExecutionOutcomeV1::Succeeded as i32,
            ..Default::default()
        },
        proposed,
        command_retirement_binding(&verified),
    )
    .expect("the proposed outcome settles");
    assert_eq!(receipt.outcome(), ExecutionOutcomeV1::Succeeded);
    for other in [
        ExecutionOutcomeV1::Failed,
        ExecutionOutcomeV1::Cancelled,
        ExecutionOutcomeV1::OutcomeUnknown,
    ] {
        assert!(
            parse_prepare_settlement_response(
                PrepareSettlementResponseV1 {
                    settlement_receipt_id: "receipt-1".into(),
                    outcome: other as i32,
                    ..Default::default()
                },
                proposed,
                command_retirement_binding(&verified),
            )
            .is_err(),
            "{other:?} must not replace the proposed {proposed:?}"
        );
    }
}

/// A run stopped after it touched the store reports an error summary with
/// the store's record in the CANCELLED terminal state, and the frame
/// proposes CANCELLED; without the totals the state is malformed.
#[test]
fn a_stopped_index_run_with_the_stores_record_proposes_cancelled() {
    use crate::protocol::elitea::runtime::v1::ExecutionOutcomeV1;
    let (command, response) = fixture(ToolkitCommandKind::IndexIngest);
    let claim = claim(&command, response);
    let verified = verified(&command);
    let mut stopped = index_summary();
    stopped.status = IndexIngestStatusV1::Error as i32;
    stopped.terminal_state = IndexIngestTerminalStateV1::Cancelled as i32;
    stopped.embedding_model.clear();
    stopped.embedding_dimension = 0;
    assert!(
        stopped.total_documents.is_some(),
        "the fixture carries totals"
    );
    let frame = AgentExecutionOutputAuthority {
        claim: claim_again_for(&command),
    }
    .bind_toolkit_execute_read_terminal(
        &verified,
        ToolkitExecuteReadTerminalOutput::IndexIngest(Box::new(index_result(
            &claim,
            stopped.clone(),
        ))),
        NOW,
    )
    .unwrap();
    assert_eq!(
        frame.settlement_proposal.unwrap().requested_outcome,
        ExecutionOutcomeV1::Cancelled as i32
    );
    for mutate in [
        (|s: &mut IndexIngestSummaryV1| {
            s.total_documents = None;
            s.total_chunks = None;
            s.whole_index_record = false;
        }) as fn(&mut IndexIngestSummaryV1),
        |s| s.status = IndexIngestStatusV1::Ok as i32,
    ] {
        let mut malformed = stopped.clone();
        mutate(&mut malformed);
        assert!(
            AgentExecutionOutputAuthority {
                claim: claim_again_for(&command),
            }
            .bind_toolkit_execute_read_terminal(
                &verified,
                ToolkitExecuteReadTerminalOutput::IndexIngest(Box::new(index_result(
                    &claim, malformed
                ))),
                NOW,
            )
            .is_err()
        );
    }
}

fn claim_again_for(command: &WorkerCommandV1) -> AcceptedAgentClaim {
    let (_, response) = fixture(ToolkitCommandKind::IndexIngest);
    claim(command, response)
}

use super::*;
use crate::protocol::command::{
    TestOnlyConformanceHmacAuthenticator, ToolkitCommandKind, VerifiedExecutionCommandKind,
    parse_and_verify_execution_command, parse_and_verify_toolkit_execute_read_command,
};
use crate::protocol::elitea::runtime::v1::{
    SignedWorkerCommandEnvelopeV1, ToolkitAuthorizationRequiredV1,
    ToolkitAvailableToolsArtifactReferenceV1, ToolkitAvailableToolsCommandV1,
    ToolkitAvailableToolsResultV1, ToolkitCallToolCommandV1, ToolkitCallToolResultV1,
    ToolkitCallToolStatusV1, ToolkitCallToolSummaryV1, WorkerCommandTypeV1, WorkerCommandV1,
    execution_output_frame_v1,
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
                        toolkit_type: "openapi".into(),
                        settings_entry_id: "toolkit-settings".into(),
                    },
                ));
        }
        ToolkitCommandKind::ExecuteRead => unreachable!(),
    }
    rebind_manifest(&mut command, &mut response);
    (command, response)
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

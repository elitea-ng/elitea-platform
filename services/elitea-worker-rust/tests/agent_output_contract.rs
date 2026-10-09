use std::collections::BTreeMap;

use elitea_worker_rust::agents::{
    AgentExecutionKind, AgentInputBinding, AgentResultArtifact, AgentTerminalState,
    BoundAgentExecutionResult, bind_result_artifact, parse_agent_execution_input, request_from,
};
use elitea_worker_rust::protocol::command::{
    SignedCommandAuthenticator, TestOnlyConformanceHmacAuthenticator,
    parse_and_verify_agent_command,
};
use elitea_worker_rust::protocol::elitea::runtime::v1::{
    DigestAlgorithmV1, ExecutionFenceV1, ExecutionOutcomeV1, ExecutionOutputEventTypeV1,
    RuntimeErrorCodeV1, execution_output_frame_v1,
};
use elitea_worker_rust::protocol::output::{
    AgentTerminalOutput, OUTPUT_SCHEMA_REVISION, RuntimeFailureKind,
    build_agent_terminal_output_frame,
};
use prost::Message;
use ring::digest;

fn decode_hex(value: &str) -> Vec<u8> {
    let value = value.trim();
    assert_eq!(value.len() % 2, 0);
    value
        .as_bytes()
        .chunks_exact(2)
        .map(|pair| {
            u8::from_str_radix(std::str::from_utf8(pair).unwrap(), 16).expect("fixture hex")
        })
        .collect()
}

fn named_vectors(path: &str) -> BTreeMap<String, Vec<u8>> {
    path.lines()
        .map(|line| {
            let (name, value) = line.split_once('=').expect("named fixture");
            (name.to_owned(), decode_hex(value))
        })
        .collect()
}

#[test]
fn completed_agent_result_and_settlement_match_python_exactly() {
    let outputs = named_vectors(include_str!("fixtures/agent_output_vectors.txt"));
    let result = bound_result(AgentTerminalState::Completed, output_binding());
    assert_eq!(
        result.message().encode_to_vec(),
        outputs["agent_result_proto"]
    );
    let verified = verified_command();
    let frame = build_agent_terminal_output_frame(
        &verified,
        &fixture_fence(),
        AgentTerminalOutput::Result(Box::new(result)),
        10,
        1_786_940_222_700,
        4,
    )
    .expect("completed terminal frame");

    assert_eq!(frame.encode_to_vec(), outputs["completed_output_frame"]);
    assert_eq!(frame.output_schema_revision, OUTPUT_SCHEMA_REVISION);
    assert_eq!(frame.stream_id, "execution-1:2");
    assert_eq!(frame.logical_output_id, "agent-execution:execution-1");
    assert_eq!(frame.event_id, "command-1:10");
    assert_eq!(
        frame.event_type,
        ExecutionOutputEventTypeV1::AgentExecutionResult as i32
    );
    assert!(frame.terminal);
    let proposal = frame.settlement_proposal.as_ref().expect("settlement");
    assert_eq!(proposal.proposal_id, "command-1:settlement");
    assert_eq!(
        proposal.requested_outcome,
        ExecutionOutcomeV1::Succeeded as i32
    );
    assert_eq!(proposal.terminal_sequence, 10);
    assert_eq!(
        proposal.prepare_idempotency_key,
        "command-1:prepare-settlement"
    );
    assert_eq!(proposal.terminal_payload_digest, frame.payload_digest);
}

#[test]
fn cancelled_failure_and_settlement_match_python_exactly() {
    let outputs = named_vectors(include_str!("fixtures/agent_output_vectors.txt"));
    let verified = verified_command();
    let frame = build_agent_terminal_output_frame(
        &verified,
        &fixture_fence(),
        AgentTerminalOutput::Failure(RuntimeFailureKind::Cancelled),
        10,
        1_786_940_222_700,
        4,
    )
    .expect("cancelled terminal frame");

    assert_eq!(frame.encode_to_vec(), outputs["cancelled_output_frame"]);
    assert_eq!(
        frame.event_type,
        ExecutionOutputEventTypeV1::RuntimeError as i32
    );
    let Some(execution_output_frame_v1::Payload::RuntimeError(error)) = &frame.payload else {
        panic!("runtime error payload");
    };
    assert_eq!(error.code, RuntimeErrorCodeV1::Cancelled as i32);
    assert_eq!(error.safe_message, "Execution was cancelled.");
    assert!(!error.retryable);
    assert_eq!(
        frame
            .settlement_proposal
            .as_ref()
            .unwrap()
            .requested_outcome,
        ExecutionOutcomeV1::Cancelled as i32
    );
}

#[test]
fn both_supported_agent_pause_results_settle_as_success() {
    for state in [
        AgentTerminalState::PausedHitl,
        AgentTerminalState::PausedMcpAuth,
    ] {
        let result = bound_result(state, output_binding());
        let frame = build_agent_terminal_output_frame(
            &verified_command(),
            &fixture_fence(),
            AgentTerminalOutput::Result(Box::new(result)),
            10,
            1,
            4,
        )
        .expect("supported agent pause frame");
        assert_eq!(
            frame.settlement_proposal.unwrap().requested_outcome,
            ExecutionOutcomeV1::Succeeded as i32
        );
    }
}

#[test]
fn all_registered_runtime_failures_use_only_canonical_safe_policy() {
    let verified = verified_command();
    for (kind, expected_code, expected_message, expected_retryable) in [
        (
            RuntimeFailureKind::UnsupportedCapability,
            RuntimeErrorCodeV1::UnsupportedCapability,
            "Configuration type is not supported.",
            false,
        ),
        (
            RuntimeFailureKind::PipelineNodeTypeNotAvailable,
            RuntimeErrorCodeV1::UnsupportedCapability,
            "This pipeline uses a node type that is not available on this deployment. Open the pipeline to see which node, then remove or replace it.",
            false,
        ),
        (
            RuntimeFailureKind::IncompatibleVersion,
            RuntimeErrorCodeV1::IncompatibleVersion,
            "The requested contract version is not compatible.",
            false,
        ),
        (
            RuntimeFailureKind::InvalidInput,
            RuntimeErrorCodeV1::InvalidInput,
            "The execution input is invalid.",
            false,
        ),
        (
            RuntimeFailureKind::ResourceExhausted,
            RuntimeErrorCodeV1::ResourceExhausted,
            "The execution exceeded an approved resource limit.",
            false,
        ),
        (
            RuntimeFailureKind::DependencyUnavailable,
            RuntimeErrorCodeV1::DependencyUnavailable,
            "A required runtime dependency is unavailable.",
            true,
        ),
        (
            RuntimeFailureKind::DeadlineExceeded,
            RuntimeErrorCodeV1::DeadlineExceeded,
            "The execution deadline was exceeded.",
            true,
        ),
        (
            RuntimeFailureKind::AuthorizationFailed,
            RuntimeErrorCodeV1::AuthorizationFailed,
            "Execution authorization failed.",
            false,
        ),
        (
            RuntimeFailureKind::Cancelled,
            RuntimeErrorCodeV1::Cancelled,
            "Execution was cancelled.",
            false,
        ),
        (
            RuntimeFailureKind::Internal,
            RuntimeErrorCodeV1::Internal,
            "The runtime operation failed.",
            false,
        ),
    ] {
        let frame = build_agent_terminal_output_frame(
            &verified,
            &fixture_fence(),
            AgentTerminalOutput::Failure(kind),
            1,
            1,
            0,
        )
        .unwrap();
        let Some(execution_output_frame_v1::Payload::RuntimeError(error)) = &frame.payload else {
            panic!("runtime error payload");
        };
        assert_eq!(error.code, expected_code as i32);
        assert_eq!(error.safe_message, expected_message);
        assert_eq!(error.retryable, expected_retryable);
        assert!(
            frame
                .encode_to_vec()
                .windows(5)
                .all(|window| window != b"cause")
        );
        assert_eq!(
            frame.settlement_proposal.unwrap().requested_outcome,
            if kind == RuntimeFailureKind::Cancelled {
                ExecutionOutcomeV1::Cancelled as i32
            } else {
                ExecutionOutcomeV1::Failed as i32
            }
        );
    }
}

#[test]
fn result_digest_and_settlement_bind_exact_payload_bytes() {
    let result = bound_result(AgentTerminalState::Completed, output_binding());
    let result_bytes = result.message().encode_to_vec();
    let frame = build_agent_terminal_output_frame(
        &verified_command(),
        &fixture_fence(),
        AgentTerminalOutput::Result(Box::new(result)),
        10,
        1,
        4,
    )
    .unwrap();
    let expected = digest::digest(&digest::SHA256, &result_bytes);
    let payload_digest = frame.payload_digest.as_ref().unwrap();
    assert_eq!(payload_digest.algorithm, DigestAlgorithmV1::Sha256 as i32);
    assert_eq!(payload_digest.value, expected.as_ref());
    assert_eq!(
        frame
            .settlement_proposal
            .as_ref()
            .unwrap()
            .terminal_payload_digest
            .as_ref(),
        Some(payload_digest)
    );
}

#[test]
fn invalid_result_fence_and_terminal_identity_fail_before_delivery() {
    let verified = verified_command();
    let mut mismatched_bindings = Vec::new();
    let mut binding = output_binding();
    binding.input_bundle_id = "different-bundle".to_owned();
    mismatched_bindings.push(binding);
    let mut binding = output_binding();
    binding.input_bundle_digest = [b'x'; 32];
    mismatched_bindings.push(binding);
    let mut binding = output_binding();
    binding.request_entry_id = "different-entry".to_owned();
    mismatched_bindings.push(binding);
    for binding in mismatched_bindings {
        let result = bound_result(AgentTerminalState::Completed, binding);
        assert!(
            build_agent_terminal_output_frame(
                &verified,
                &fixture_fence(),
                AgentTerminalOutput::Result(Box::new(result)),
                10,
                1,
                4,
            )
            .is_err()
        );
    }
    for (sequence, occurred, watermark) in [(0, 1, 0), (1, 0, 0), (4, 1, 4)] {
        assert!(
            build_agent_terminal_output_frame(
                &verified,
                &fixture_fence(),
                AgentTerminalOutput::Failure(RuntimeFailureKind::Internal),
                sequence,
                occurred,
                watermark,
            )
            .is_err()
        );
    }
    let mut fence = fixture_fence();
    fence.fence_token.fill(0);
    assert!(
        build_agent_terminal_output_frame(
            &verified,
            &fence,
            AgentTerminalOutput::Failure(RuntimeFailureKind::Internal),
            1,
            1,
            0,
        )
        .is_err()
    );
}

fn verified_command() -> elitea_worker_rust::protocol::command::VerifiedAgentCommand {
    let commands = named_vectors(include_str!("fixtures/agent_command_vectors.txt"));
    let authenticator = TestOnlyConformanceHmacAuthenticator;
    parse_and_verify_agent_command(
        &commands["application_hmac"],
        Some(&authenticator as &dyn SignedCommandAuthenticator),
    )
    .expect("Python signed command")
}

fn fixture_fence() -> ExecutionFenceV1 {
    ExecutionFenceV1 {
        workload_session_id: "workload-session-1".to_owned(),
        claim_attempt: 3,
        lease_epoch: 7,
        producer_id: "rust-worker-fixture".to_owned(),
        fence_token: vec![b'f'; 32],
    }
}

fn output_binding() -> AgentInputBinding {
    AgentInputBinding {
        input_bundle_id: "bundle-1".to_owned(),
        input_bundle_digest: [b'b'; 32],
        request_entry_id: "agent-request".to_owned(),
        request_immutable_version: "v1".to_owned(),
        request_content_digest: [b'c'; 32],
    }
}

fn bound_result(
    state: AgentTerminalState,
    binding: AgentInputBinding,
) -> BoundAgentExecutionResult {
    let input = parse_agent_execution_input(&decode_hex(include_str!(
        "fixtures/agent_application_input.hex"
    )))
    .expect("Python application input");
    let request = request_from(input, AgentExecutionKind::Application, binding)
        .expect("admitted application request");
    bind_result_artifact(
        &request,
        state,
        AgentResultArtifact {
            artifact_id: "artifact-1".to_owned(),
            immutable_version: "v1".to_owned(),
            byte_length: 123,
            digest: [b'a'; 32],
        },
    )
    .expect("bound terminal result")
}

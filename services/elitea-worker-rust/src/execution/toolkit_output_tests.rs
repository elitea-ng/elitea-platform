//! Exercise encrypted read-tool recovery without input or tool execution.

use std::fs;
use std::os::unix::fs::PermissionsExt;
use std::path::PathBuf;
use std::sync::Arc;
use std::time::Duration;

use prost::Message;
use ring::{digest, hmac};

use super::output_delivery::{AgentOutputPreflight, AgentOutputSpoolFactory};
use super::toolkit_delivery::{FreshToolkitDelivery, test_fresh_toolkit_delivery};
use super::toolkit_output::{ToolkitOutputPreflightOutcome, prepare_toolkit_output};
use crate::protocol::command::{
    TestOnlyConformanceHmacAuthenticator, VerifiedToolkitExecuteReadCommand,
    parse_and_verify_toolkit_execute_read_command,
};
use crate::protocol::control::test_accepted_agent_claim;
use crate::protocol::elitea::runtime::v1::{
    ClaimCommandResponseV1, DigestAlgorithmV1, DigestV1, ExecutionOutputFrameV1,
    SignedWorkerCommandEnvelopeV1, ToolkitExecuteReadCommandV1, ToolkitExecuteReadResultV1,
    WorkerCommandTypeV1, WorkerCommandV1, execution_output_frame_v1, worker_command_v1,
};
use crate::protocol::output::{
    RuntimeFailureKind, ToolkitExecuteReadTerminalOutput,
    build_toolkit_execute_read_terminal_output_frame, restored_terminal_failure_kind,
};
use crate::spool::{SpoolLimits, SpoolMasterKey};
use crate::transport::OutputGrpcConfig;
use crate::transport::redis_commands::{RedisCommandDelivery, RedisCommandLimits};

const DEADLINE: i64 = 1_700_000_100_000;
const NOW: i64 = DEADLINE + 1_000;

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

fn command_and_claim() -> (Vec<u8>, ClaimCommandResponseV1) {
    let mut signed = SignedWorkerCommandEnvelopeV1::decode(vector("signed_command").as_slice())
        .expect("signed command");
    let mut command =
        WorkerCommandV1::decode(signed.worker_command_bytes.as_slice()).expect("command");
    let mut response =
        ClaimCommandResponseV1::decode(vector("accepted_claim").as_slice()).expect("claim");
    let receipt = response.receipt.as_mut().expect("receipt");
    let manifest = receipt.input_bundle.as_mut().expect("manifest");
    let entry = &mut manifest.entries[0];
    entry.semantic_role = "toolkit.execute_read_request".to_owned();
    entry.content.as_mut().expect("content").media_type =
        "application/vnd.elitea.toolkit-execute-read-input.v1+protobuf".to_owned();
    command.command_type = WorkerCommandTypeV1::ToolkitExecuteRead as i32;
    command.capability_id = "toolkit.execute.read.v1".to_owned();
    command.capability_command = Some(worker_command_v1::CapabilityCommand::ToolkitExecuteRead(
        ToolkitExecuteReadCommandV1 {
            request_entry_id: entry.entry_id.clone(),
        },
    ));
    let raw = manifest.encode_to_vec();
    let reference = command.input_bundle_ref.as_mut().expect("reference");
    reference.digest = Some(sha256(&raw));
    reference.byte_length = raw.len() as u64;
    receipt.input_bundle_ref = Some(reference.clone());
    receipt.claim_started_at_unix_micros = NOW * 1_000;
    receipt.lease_expires_at_unix_millis = NOW + 30_000;
    signed.worker_command_bytes = command.encode_to_vec();
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
    (signed.encode_to_vec(), response)
}

fn verified(raw: &[u8]) -> VerifiedToolkitExecuteReadCommand {
    parse_and_verify_toolkit_execute_read_command(raw, Some(&TestOnlyConformanceHmacAuthenticator))
        .expect("authenticated toolkit command")
}

fn fresh(response: ClaimCommandResponseV1) -> FreshToolkitDelivery {
    let (raw, _) = command_and_claim();
    let verified = verified(&raw);
    let claim = test_accepted_agent_claim(&verified, response, "workload-1", "worker-1", NOW)
        .expect("authenticated claim");
    let delivery = RedisCommandDelivery::decode(
        b"runtime.commands.v1",
        b"1700000000000-0",
        vec![(b"signed_envelope".to_vec(), raw)],
        RedisCommandLimits {
            max_entry_bytes: 64 * 1024,
            max_field_bytes: 48 * 1024,
        },
    )
    .expect("delivery");
    test_fresh_toolkit_delivery(delivery, verified, claim)
}

fn terminal(response: &ClaimCommandResponseV1, success: bool) -> ExecutionOutputFrameV1 {
    let (raw, _) = command_and_claim();
    let verified = verified(&raw);
    let receipt = response.receipt.as_ref().expect("receipt");
    let mut fence = receipt.fence.clone().expect("fence");
    fence.claim_attempt -= 1;
    fence.lease_epoch -= 1;
    fence.fence_token = vec![b'o'; 32];
    let outcome = if success {
        let bundle = receipt.input_bundle_ref.as_ref().expect("bundle");
        let entry = &receipt.input_bundle.as_ref().expect("manifest").entries[0];
        ToolkitExecuteReadTerminalOutput::Result(Box::new(ToolkitExecuteReadResultV1 {
            input_bundle_id: bundle.input_bundle_id.clone(),
            input_bundle_digest: bundle.digest.clone(),
            request_entry_id: entry.entry_id.clone(),
            request_entry_version: entry.immutable_version.clone(),
            request_content_digest: entry.content.as_ref().expect("content").digest.clone(),
            result_json: br#"{"marker":"completed-read"}"#.to_vec(),
            toolkit_type: "openapi".to_owned(),
            toolkit_name: "echo".to_owned(),
            tool_name: "echo_marker".to_owned(),
        }))
    } else {
        ToolkitExecuteReadTerminalOutput::Failure(RuntimeFailureKind::Internal)
    };
    build_toolkit_execute_read_terminal_output_frame(
        &verified,
        &fence,
        outcome,
        receipt.claim_handoff_watermark + 1,
        DEADLINE - 1,
        receipt.claim_handoff_watermark,
    )
    .expect("valid old terminal")
}

fn preflight(root: PathBuf) -> AgentOutputPreflight {
    AgentOutputPreflight::new(
        root,
        SpoolMasterKey::new([b'p'; 32]),
        SpoolLimits {
            max_frames: 1,
            max_encrypted_bytes: 65 * 1024,
            max_frame_bytes: 64 * 1024,
        },
        OutputGrpcConfig {
            max_queued_frames: 1,
            max_queued_bytes: 64 * 1024,
            max_frame_bytes: 64 * 1024,
            max_server_credit_frames: 1,
            max_server_credit_bytes: 64 * 1024,
            stream_deadline: Duration::from_mins(1),
            ack_timeout: Duration::from_secs(1),
            workload_session_id: "workload-1".to_owned(),
            producer_id: "worker-1".to_owned(),
        },
    )
}

fn root() -> tempfile::TempDir {
    let temporary = tempfile::tempdir_in(std::env::temp_dir().canonicalize().expect("temp root"))
        .expect("private root");
    fs::set_permissions(temporary.path(), fs::Permissions::from_mode(0o700)).expect("permissions");
    temporary
}

async fn persist(
    output: &AgentOutputPreflight,
    frame: &ExecutionOutputFrameV1,
) -> AgentOutputSpoolFactory {
    let factory = AgentOutputSpoolFactory::new(
        output.shared_policy(),
        Arc::new(fresh(command_and_claim().1).spool_identity()),
    );
    factory
        .reopen()
        .await
        .expect("open spool")
        .persist(frame.clone())
        .expect("persist terminal");
    factory
}

#[tokio::test]
async fn expired_read_terminal_uses_replacement_authority_without_business_execution() {
    for success in [false, true] {
        let temporary = root();
        let output = preflight(temporary.path().to_path_buf());
        let (_, response) = command_and_claim();
        let expected = terminal(&response, success);
        let factory = persist(&output, &expected).await;
        let recovered = prepare_toolkit_output(&output, fresh(response.clone()), NOW)
            .await
            .expect("deadline recovery");
        let ToolkitOutputPreflightOutcome::Terminal(recovered) = recovered else {
            panic!("must not grant fresh execution")
        };
        assert_eq!(
            restored_terminal_failure_kind(&recovered.frame),
            Some(RuntimeFailureKind::DeadlineExceeded)
        );
        assert_eq!(
            recovered.frame.fence,
            response.receipt.as_ref().expect("receipt").fence
        );
        assert_eq!(recovered.frame.sequence, expected.sequence);
        assert_eq!(recovered.frame.identity, expected.identity);
        assert_eq!(recovered.frame.event_id, expected.event_id);
        assert_eq!(
            recovered.frame.logical_output_id,
            expected.logical_output_id
        );
        fresh(response.clone())
            .validate_output_frame(&recovered.frame)
            .expect("canonical failure");
        let replacement = recovered.frame.clone();
        drop(recovered);
        assert_eq!(
            factory
                .reopen()
                .await
                .expect("durable recovery")
                .pending_replay_frame(),
            Some(replacement.clone())
        );
        let reopened = prepare_toolkit_output(&output, fresh(response), NOW + 1)
            .await
            .expect("exact replay after reopen");
        let ToolkitOutputPreflightOutcome::Terminal(reopened) = reopened else {
            panic!("must retain terminal-only authority")
        };
        assert_eq!(reopened.frame, replacement);
    }
}

#[tokio::test]
async fn deadline_recovery_survives_another_claim_and_workload_session() {
    let temporary = root();
    let output = preflight(temporary.path().to_path_buf());
    let (_, mut response) = command_and_claim();
    let mut expected = terminal(&response, false);
    expected
        .fence
        .as_mut()
        .expect("old fence")
        .workload_session_id = "retired-session".to_owned();
    persist(&output, &expected).await;
    for attempt in 5..=7 {
        let fence = response
            .receipt
            .as_mut()
            .expect("receipt")
            .fence
            .as_mut()
            .expect("fence");
        fence.claim_attempt = attempt;
        fence.lease_epoch = attempt;
        fence.fence_token = vec![u8::try_from(attempt).expect("small attempt"); 32];
        let result = prepare_toolkit_output(&output, fresh(response.clone()), NOW)
            .await
            .expect("replacement recovery");
        let ToolkitOutputPreflightOutcome::Terminal(recovered) = result else {
            panic!("no business authority")
        };
        assert_eq!(
            restored_terminal_failure_kind(&recovered.frame),
            Some(RuntimeFailureKind::DeadlineExceeded)
        );
        assert_eq!(
            recovered.frame.fence.as_ref().expect("fence").claim_attempt,
            attempt
        );
    }
}

#[tokio::test]
async fn local_clock_does_not_turn_a_recovered_result_into_a_deadline_failure() {
    for started_at in [
        DEADLINE * 1_000 - 1,
        (DEADLINE - 1) * 1_000,
        (DEADLINE - 100) * 1_000,
    ] {
        let temporary = root();
        let output = preflight(temporary.path().to_path_buf());
        let (_, mut response) = command_and_claim();
        let expected = terminal(&response, true);
        response
            .receipt
            .as_mut()
            .expect("receipt")
            .claim_started_at_unix_micros = started_at;
        let factory = persist(&output, &expected).await;
        let result = prepare_toolkit_output(&output, fresh(response), NOW + 86_400_000)
            .await
            .expect("recover exact result; Main owns the deadline decision");
        let ToolkitOutputPreflightOutcome::Terminal(recovered) = result else {
            panic!("no business authority")
        };
        assert_eq!(recovered.frame.payload, expected.payload);
        assert_eq!(
            recovered.frame.settlement_proposal,
            expected.settlement_proposal
        );
        assert_eq!(
            recovered.frame.occurred_at_unix_millis,
            expected.occurred_at_unix_millis
        );
        let replacement = recovered.frame.clone();
        drop(recovered);
        assert_eq!(
            factory
                .reopen()
                .await
                .expect("untouched spool")
                .pending_replay_frame(),
            Some(replacement)
        );
    }
}

#[tokio::test]
async fn unexpired_takeover_replays_the_complete_terminal_without_repeating_the_tool() {
    for success in [false, true] {
        let temporary = root();
        let output = preflight(temporary.path().to_path_buf());
        let (_, mut response) = command_and_claim();
        response
            .receipt
            .as_mut()
            .expect("receipt")
            .claim_started_at_unix_micros = (DEADLINE - 1) * 1_000;
        let expected = terminal(&response, success);
        persist(&output, &expected).await;
        let result = prepare_toolkit_output(&output, fresh(response.clone()), DEADLINE - 1)
            .await
            .expect("unexpired takeover");
        let ToolkitOutputPreflightOutcome::Terminal(recovered) = result else {
            panic!("no tool invocation authority")
        };
        let mut rebound = expected;
        rebound.fence = response.receipt.as_ref().expect("receipt").fence.clone();
        assert_eq!(recovered.frame, rebound);
        fresh(response)
            .validate_output_frame(&recovered.frame)
            .expect("complete canonical terminal");
    }
}

#[tokio::test]
async fn deadline_recovery_rejects_changed_identity_payload_sequence_and_fence() {
    let mutations: &[fn(&mut ExecutionOutputFrameV1)] = &[
        |f| {
            let identity = f.identity.as_mut().unwrap();
            identity.generation += 1;
            f.stream_id = format!("{}:{}", identity.execution_id, identity.generation);
        },
        |f| f.identity.as_mut().unwrap().resource_project_id = "8".to_owned(),
        |f| f.identity.as_mut().unwrap().command_id = "another-command".to_owned(),
        |f| f.payload_digest.as_mut().unwrap().value[0] ^= 1,
        |f| f.sequence += 1,
        |f| f.fence.as_mut().unwrap().claim_attempt += 2,
        |f| f.fence.as_mut().unwrap().lease_epoch += 2,
        |f| {
            f.fence.as_mut().unwrap().fence_token = command_and_claim()
                .1
                .receipt
                .unwrap()
                .fence
                .unwrap()
                .fence_token;
        },
        |f| f.fence.as_mut().unwrap().producer_id = "another-worker".to_owned(),
    ];
    for mutate in mutations {
        let temporary = root();
        let output = preflight(temporary.path().to_path_buf());
        let (_, response) = command_and_claim();
        let mut expected = terminal(&response, false);
        mutate(&mut expected);
        let factory = persist(&output, &expected).await;
        assert!(
            prepare_toolkit_output(&output, fresh(response), NOW)
                .await
                .is_err()
        );
        assert_eq!(
            factory
                .reopen()
                .await
                .expect("untouched spool")
                .pending_replay_frame(),
            Some(expected)
        );
    }
}

#[tokio::test]
async fn current_fence_keeps_exact_terminal_for_server_side_deadline_resolution() {
    let temporary = root();
    let output = preflight(temporary.path().to_path_buf());
    let (_, response) = command_and_claim();
    let mut expected = terminal(&response, true);
    expected.fence = response.receipt.as_ref().expect("receipt").fence.clone();
    persist(&output, &expected).await;
    let result = prepare_toolkit_output(&output, fresh(response), NOW)
        .await
        .expect("exact recovery");
    let ToolkitOutputPreflightOutcome::Terminal(recovered) = result else {
        panic!("no business authority")
    };
    assert_eq!(recovered.frame, expected);
}

#[tokio::test]
async fn empty_spool_keeps_existing_delivery_path() {
    let temporary = root();
    let output = preflight(temporary.path().to_path_buf());
    assert!(matches!(
        prepare_toolkit_output(&output, fresh(command_and_claim().1), NOW)
            .await
            .expect("empty"),
        ToolkitOutputPreflightOutcome::Empty(_)
    ));
}

#[tokio::test]
async fn deadline_boundary_preserves_authenticated_handoff_without_integer_wrap() {
    for handoff in [0, 1, 4, 63, u64::from(u32::MAX), i64::MAX as u64 - 1] {
        let temporary = root();
        let output = preflight(temporary.path().to_path_buf());
        let (_, mut response) = command_and_claim();
        let receipt = response.receipt.as_mut().expect("receipt");
        receipt.claim_handoff_watermark = handoff;
        receipt.claim_started_at_unix_micros = DEADLINE * 1_000;
        let expected = terminal(&response, false);
        persist(&output, &expected).await;
        let result = prepare_toolkit_output(&output, fresh(response), DEADLINE)
            .await
            .expect("deadline boundary");
        let ToolkitOutputPreflightOutcome::Terminal(recovered) = result else {
            panic!("terminal-only recovery")
        };
        assert_eq!(recovered.frame.sequence, handoff + 1);
        assert_eq!(recovered.frame.claim_handoff_watermark, handoff);
    }
}

#[tokio::test]
async fn changed_handoff_or_request_binding_keeps_old_output_untouched() {
    for mutation in 0..4 {
        let temporary = root();
        let output = preflight(temporary.path().to_path_buf());
        let (raw, mut response) = command_and_claim();
        let mut expected = terminal(&response, true);
        if mutation < 2 {
            response
                .receipt
                .as_mut()
                .expect("receipt")
                .claim_handoff_watermark = if mutation == 0 { 3 } else { 5 };
        } else {
            let Some(execution_output_frame_v1::Payload::ToolkitExecuteRead(mut result)) =
                expected.payload.take()
            else {
                panic!("read result")
            };
            if mutation == 2 {
                result.request_entry_version = "another-version".to_owned();
            } else {
                result
                    .request_content_digest
                    .as_mut()
                    .expect("digest")
                    .value[0] ^= 1;
            }
            expected = build_toolkit_execute_read_terminal_output_frame(
                &verified(&raw),
                expected.fence.as_ref().expect("fence"),
                ToolkitExecuteReadTerminalOutput::Result(Box::new(result)),
                expected.sequence,
                expected.occurred_at_unix_millis,
                expected.claim_handoff_watermark,
            )
            .expect("well-formed but wrong request binding");
        }
        let factory = persist(&output, &expected).await;
        assert!(
            prepare_toolkit_output(&output, fresh(response), NOW)
                .await
                .is_err()
        );
        assert_eq!(
            factory
                .reopen()
                .await
                .expect("unchanged spool")
                .pending_replay_frame(),
            Some(expected)
        );
    }
}

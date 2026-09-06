use prost::Message;
use ring::digest;

use crate::agents::result::{BoundAgentExecutionResult, validate_agent_execution_result};

use super::command::{
    VerifiedAgentCommand, VerifiedExecutionCommand, VerifiedToolkitExecuteReadCommand,
};
use super::node_event::encode_current_node_event_json;
use super::{
    ProtocolError,
    elitea::runtime::v1::{
        AgentExecutionResultV1, DigestAlgorithmV1, DigestV1, ExecutionFenceV1, ExecutionIdentityV1,
        ExecutionOutcomeV1, ExecutionOutputEventTypeV1, ExecutionOutputFrameV1, NodeEventV1,
        RuntimeErrorCodeV1, RuntimeErrorV1, SettlementProposalV1, ToolkitExecuteReadResultV1,
        execution_output_frame_v1, worker_command_v1,
    },
};

pub const OUTPUT_SCHEMA_REVISION: &str = "elitea.runtime.execution-output.v1";
pub const MAX_OUTPUT_FRAME_BYTES: usize = 64 * 1024;

const MAX_SAFE_STRING_BYTES: usize = 256;
const MAX_TOOLKIT_IDENTITY_BYTES: usize = 1024;
const MAX_TOOLKIT_EXECUTE_READ_RESULT_BYTES: usize = 48 * 1024;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum RuntimeFailureKind {
    UnsupportedCapability,
    IncompatibleVersion,
    InvalidInput,
    ResourceExhausted,
    DependencyUnavailable,
    DeadlineExceeded,
    AuthorizationFailed,
    Cancelled,
    Internal,
}

pub enum AgentTerminalOutput {
    Result(Box<BoundAgentExecutionResult>),
    Failure(RuntimeFailureKind),
}

pub(crate) enum ToolkitExecuteReadTerminalOutput {
    Result(Box<ToolkitExecuteReadResultV1>),
    Failure(RuntimeFailureKind),
}

/// Fully validated semantic shape of one restored agent output frame.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum ValidatedAgentOutputFrameKind {
    Progress,
    Terminal,
}

/// Revalidate decrypted durable output against the exact signed command.
///
/// Spool authentication proves that bytes were written under one execution
/// key; it does not replace semantic validation. Recovery calls this before
/// trusting a payload digest, settlement proposal, or deterministic identity.
pub(crate) fn validate_restored_agent_output_frame(
    verified: &VerifiedAgentCommand,
    frame: &ExecutionOutputFrameV1,
) -> Result<ValidatedAgentOutputFrameKind, ProtocolError> {
    validate_restored_frame_identity(verified, frame)?;
    let (kind, payload_bytes, requested_outcome) =
        validate_restored_agent_payload(verified, frame)?;
    let payload_digest = sha256(&payload_bytes);
    if frame.payload_digest.as_ref() != Some(&payload_digest) {
        return Err(malformed_restored_output());
    }
    validate_restored_settlement(verified, frame, payload_digest, requested_outcome)?;
    Ok(kind)
}

/// Revalidate one terminal direct-tool frame against its authenticated
/// reference-only command. Direct executions never publish progress frames.
pub(crate) fn validate_restored_toolkit_execute_read_output_frame(
    verified: &VerifiedToolkitExecuteReadCommand,
    frame: &ExecutionOutputFrameV1,
) -> Result<(), ProtocolError> {
    validate_restored_frame_identity(verified, frame)?;
    let (payload_bytes, requested_outcome) = match frame.payload.as_ref() {
        Some(execution_output_frame_v1::Payload::ToolkitExecuteRead(result)) => {
            validate_toolkit_execute_read_result(verified, result)?;
            if !frame.terminal
                || frame.event_type != ExecutionOutputEventTypeV1::ToolkitExecuteReadResult as i32
                || frame.logical_output_id
                    != format!("toolkit-execute-read:{}", verified.command().execution_id)
            {
                return Err(malformed_restored_output());
            }
            (result.encode_to_vec(), ExecutionOutcomeV1::Succeeded)
        }
        Some(execution_output_frame_v1::Payload::RuntimeError(error)) => {
            let failure = canonical_runtime_failure(error).ok_or(malformed_restored_output())?;
            if !frame.terminal
                || frame.event_type != ExecutionOutputEventTypeV1::RuntimeError as i32
                || frame.logical_output_id
                    != format!("toolkit-execute-read:{}", verified.command().execution_id)
            {
                return Err(malformed_restored_output());
            }
            (
                error.encode_to_vec(),
                if failure == RuntimeFailureKind::Cancelled {
                    ExecutionOutcomeV1::Cancelled
                } else {
                    ExecutionOutcomeV1::Failed
                },
            )
        }
        _ => return Err(malformed_restored_output()),
    };
    let payload_digest = sha256(&payload_bytes);
    if frame.payload_digest.as_ref() != Some(&payload_digest) {
        return Err(malformed_restored_output());
    }
    validate_restored_settlement(verified, frame, payload_digest, Some(requested_outcome))
}

/// Return the registered failure carried by an already validated terminal.
///
/// Callers use this only after [`validate_restored_agent_output_frame`] has
/// established the complete canonical frame shape. It prevents a frame-bound
/// cancellation or deadline winner from repeatedly replacing the same durable
/// terminal.
#[allow(dead_code)] // Used by the disabled accepted-terminal recovery owner.
pub(crate) fn restored_terminal_failure_kind(
    frame: &ExecutionOutputFrameV1,
) -> Option<RuntimeFailureKind> {
    if !frame.terminal {
        return None;
    }
    let execution_output_frame_v1::Payload::RuntimeError(error) = frame.payload.as_ref()? else {
        return None;
    };
    canonical_runtime_failure(error)
}

fn validate_restored_frame_identity(
    verified: &impl VerifiedExecutionCommand,
    frame: &ExecutionOutputFrameV1,
) -> Result<(), ProtocolError> {
    let command = verified.command();
    let identity = frame.identity.as_ref().ok_or(malformed_restored_output())?;
    let fence = frame.fence.as_ref().ok_or(malformed_restored_output())?;
    if frame.output_schema_revision != OUTPUT_SCHEMA_REVISION
        || frame.stream_id != format!("{}:{}", command.execution_id, command.generation)
        || identity.tenant_id != command.tenant_id
        || identity.resource_project_id != command.resource_project_id
        || identity.projection_project_id != command.projection_project_id
        || identity.command_id != command.command_id
        || identity.execution_id != command.execution_id
        || identity.generation != command.generation
        || frame.sequence == 0
        || frame.sequence > i64::MAX as u64
        || frame.claim_handoff_watermark >= frame.sequence
        || frame.claim_handoff_watermark > i64::MAX as u64
        || frame.occurred_at_unix_millis <= 0
        || frame.event_id != format!("{}:{}", command.command_id, frame.sequence)
    {
        return Err(malformed_restored_output());
    }
    validate_fence(fence)?;
    if frame.encoded_len() > MAX_OUTPUT_FRAME_BYTES {
        return Err(ProtocolError::ResourceExhausted(
            "the restored agent output frame exceeds the approved limit",
        ));
    }
    Ok(())
}

fn validate_restored_agent_payload(
    verified: &VerifiedAgentCommand,
    frame: &ExecutionOutputFrameV1,
) -> Result<
    (
        ValidatedAgentOutputFrameKind,
        Vec<u8>,
        Option<ExecutionOutcomeV1>,
    ),
    ProtocolError,
> {
    let command = verified.command();
    match frame.payload.as_ref() {
        Some(execution_output_frame_v1::Payload::NodeEvent(event)) => {
            encode_current_node_event_json(event)?;
            if frame.terminal
                || frame.event_type != ExecutionOutputEventTypeV1::NodeEvent as i32
                || frame.logical_output_id
                    != format!("node-event:{}:{}", command.execution_id, frame.sequence)
                || frame.settlement_proposal.is_some()
            {
                return Err(malformed_restored_output());
            }
            Ok((
                ValidatedAgentOutputFrameKind::Progress,
                event.encode_to_vec(),
                None,
            ))
        }
        Some(execution_output_frame_v1::Payload::AgentExecution(result)) => {
            validate_agent_execution_result(result)?;
            validate_result_command_binding(verified, result)?;
            if !frame.terminal
                || frame.event_type != ExecutionOutputEventTypeV1::AgentExecutionResult as i32
                || frame.logical_output_id != format!("agent-execution:{}", command.execution_id)
            {
                return Err(malformed_restored_output());
            }
            Ok((
                ValidatedAgentOutputFrameKind::Terminal,
                result.encode_to_vec(),
                Some(ExecutionOutcomeV1::Succeeded),
            ))
        }
        Some(execution_output_frame_v1::Payload::RuntimeError(error)) => {
            let failure = canonical_runtime_failure(error).ok_or(malformed_restored_output())?;
            if !frame.terminal
                || frame.event_type != ExecutionOutputEventTypeV1::RuntimeError as i32
                || frame.logical_output_id != format!("agent-execution:{}", command.execution_id)
            {
                return Err(malformed_restored_output());
            }
            Ok((
                ValidatedAgentOutputFrameKind::Terminal,
                error.encode_to_vec(),
                Some(if failure == RuntimeFailureKind::Cancelled {
                    ExecutionOutcomeV1::Cancelled
                } else {
                    ExecutionOutcomeV1::Failed
                }),
            ))
        }
        Some(
            execution_output_frame_v1::Payload::ConfigurationValidation(_)
            | execution_output_frame_v1::Payload::ToolkitAvailableTools(_)
            | execution_output_frame_v1::Payload::IndexIngest(_)
            | execution_output_frame_v1::Payload::ToolkitExecuteRead(_),
        )
        | None => Err(malformed_restored_output()),
    }
}

fn validate_restored_settlement(
    verified: &impl VerifiedExecutionCommand,
    frame: &ExecutionOutputFrameV1,
    payload_digest: DigestV1,
    requested_outcome: Option<ExecutionOutcomeV1>,
) -> Result<(), ProtocolError> {
    if let Some(requested_outcome) = requested_outcome {
        let command = verified.command();
        let expected = SettlementProposalV1 {
            proposal_id: format!("{}:settlement", command.command_id),
            requested_outcome: requested_outcome as i32,
            terminal_logical_output_id: frame.logical_output_id.clone(),
            terminal_event_id: frame.event_id.clone(),
            terminal_sequence: frame.sequence,
            terminal_payload_digest: Some(payload_digest),
            prepare_idempotency_key: format!("{}:prepare-settlement", command.command_id),
        };
        if frame.settlement_proposal.as_ref() != Some(&expected) {
            return Err(malformed_restored_output());
        }
    }
    Ok(())
}

const fn malformed_restored_output() -> ProtocolError {
    ProtocolError::InvalidInput("the restored agent output frame is malformed")
}

/// Bind one validated current `NodeEvent` to the exact claimed agent stream.
///
/// The control service issues `fence` after claim; it is deliberately supplied
/// separately from the Redis signed command. The returned frame has no
/// settlement proposal and is never terminal.
///
/// # Errors
///
/// Returns a bounded [`ProtocolError`] for an invalid fence, sequence,
/// occurrence time, `NodeEvent`, or complete output-frame size.
pub fn build_node_event_output_frame(
    verified: &VerifiedAgentCommand,
    fence: &ExecutionFenceV1,
    event: NodeEventV1,
    sequence: u64,
    occurred_at_unix_millis: i64,
    claim_handoff_watermark: u64,
) -> Result<ExecutionOutputFrameV1, ProtocolError> {
    if sequence == 0 || occurred_at_unix_millis <= 0 || claim_handoff_watermark >= sequence {
        return Err(ProtocolError::InvalidInput(
            "the progress output identity is malformed",
        ));
    }
    validate_fence(fence)?;
    encode_current_node_event_json(&event)?;

    let payload = event.encode_to_vec();
    let payload_digest = digest::digest(&digest::SHA256, &payload);
    let command = verified.command();
    let frame = ExecutionOutputFrameV1 {
        output_schema_revision: OUTPUT_SCHEMA_REVISION.to_owned(),
        stream_id: format!("{}:{}", command.execution_id, command.generation),
        identity: Some(ExecutionIdentityV1 {
            tenant_id: command.tenant_id.clone(),
            resource_project_id: command.resource_project_id.clone(),
            projection_project_id: command.projection_project_id.clone(),
            command_id: command.command_id.clone(),
            execution_id: command.execution_id.clone(),
            generation: command.generation,
        }),
        fence: Some(fence.clone()),
        logical_output_id: format!("node-event:{}:{sequence}", command.execution_id),
        event_id: format!("{}:{sequence}", command.command_id),
        sequence,
        claim_handoff_watermark,
        event_type: ExecutionOutputEventTypeV1::NodeEvent as i32,
        occurred_at_unix_millis,
        payload_digest: Some(DigestV1 {
            algorithm: DigestAlgorithmV1::Sha256 as i32,
            value: payload_digest.as_ref().to_vec(),
        }),
        terminal: false,
        settlement_proposal: None,
        payload: Some(execution_output_frame_v1::Payload::NodeEvent(event)),
    };
    if frame.encoded_len() > MAX_OUTPUT_FRAME_BYTES {
        return Err(ProtocolError::ResourceExhausted(
            "the progress event exceeds the approved output limit",
        ));
    }
    Ok(frame)
}

/// Bind one terminal agent result or registered safe failure to the claimed
/// stream and its deterministic settlement proposal.
///
/// # Errors
///
/// Returns a bounded [`ProtocolError`] for an invalid result, fence, sequence,
/// occurrence time, or complete terminal frame.
pub fn build_agent_terminal_output_frame(
    verified: &VerifiedAgentCommand,
    fence: &ExecutionFenceV1,
    outcome: AgentTerminalOutput,
    sequence: u64,
    occurred_at_unix_millis: i64,
    claim_handoff_watermark: u64,
) -> Result<ExecutionOutputFrameV1, ProtocolError> {
    if sequence == 0 || occurred_at_unix_millis <= 0 || claim_handoff_watermark >= sequence {
        return Err(ProtocolError::InvalidInput(
            "the terminal output identity is malformed",
        ));
    }
    validate_fence(fence)?;

    let (event_type, payload, payload_bytes, requested_outcome) = match outcome {
        AgentTerminalOutput::Result(result) => {
            let result = result.into_message();
            validate_agent_execution_result(&result)?;
            validate_result_command_binding(verified, &result)?;
            let bytes = result.encode_to_vec();
            (
                ExecutionOutputEventTypeV1::AgentExecutionResult,
                execution_output_frame_v1::Payload::AgentExecution(result),
                bytes,
                ExecutionOutcomeV1::Succeeded,
            )
        }
        AgentTerminalOutput::Failure(kind) => {
            let error = runtime_error(kind);
            let bytes = error.encode_to_vec();
            (
                ExecutionOutputEventTypeV1::RuntimeError,
                execution_output_frame_v1::Payload::RuntimeError(error),
                bytes,
                if kind == RuntimeFailureKind::Cancelled {
                    ExecutionOutcomeV1::Cancelled
                } else {
                    ExecutionOutcomeV1::Failed
                },
            )
        }
    };
    let payload_digest = sha256(&payload_bytes);
    let command = verified.command();
    let logical_output_id = format!("agent-execution:{}", command.execution_id);
    let event_id = format!("{}:{sequence}", command.command_id);
    let frame = ExecutionOutputFrameV1 {
        output_schema_revision: OUTPUT_SCHEMA_REVISION.to_owned(),
        stream_id: format!("{}:{}", command.execution_id, command.generation),
        identity: Some(ExecutionIdentityV1 {
            tenant_id: command.tenant_id.clone(),
            resource_project_id: command.resource_project_id.clone(),
            projection_project_id: command.projection_project_id.clone(),
            command_id: command.command_id.clone(),
            execution_id: command.execution_id.clone(),
            generation: command.generation,
        }),
        fence: Some(fence.clone()),
        logical_output_id: logical_output_id.clone(),
        event_id: event_id.clone(),
        sequence,
        claim_handoff_watermark,
        event_type: event_type as i32,
        occurred_at_unix_millis,
        payload_digest: Some(payload_digest.clone()),
        terminal: true,
        settlement_proposal: Some(SettlementProposalV1 {
            proposal_id: format!("{}:settlement", command.command_id),
            requested_outcome: requested_outcome as i32,
            terminal_logical_output_id: logical_output_id,
            terminal_event_id: event_id,
            terminal_sequence: sequence,
            terminal_payload_digest: Some(payload_digest),
            prepare_idempotency_key: format!("{}:prepare-settlement", command.command_id),
        }),
        payload: Some(payload),
    };
    if frame.encoded_len() > MAX_OUTPUT_FRAME_BYTES {
        return Err(ProtocolError::ResourceExhausted(
            "the terminal event exceeds the approved output limit",
        ));
    }
    Ok(frame)
}

/// Bind one direct read result or registered safe failure to the exact claim.
/// The returned terminal is small enough for the v1 inline output frame.
pub(crate) fn build_toolkit_execute_read_terminal_output_frame(
    verified: &VerifiedToolkitExecuteReadCommand,
    fence: &ExecutionFenceV1,
    outcome: ToolkitExecuteReadTerminalOutput,
    sequence: u64,
    occurred_at_unix_millis: i64,
    claim_handoff_watermark: u64,
) -> Result<ExecutionOutputFrameV1, ProtocolError> {
    if sequence == 0 || occurred_at_unix_millis <= 0 || claim_handoff_watermark >= sequence {
        return Err(ProtocolError::InvalidInput(
            "the direct toolkit terminal identity is malformed",
        ));
    }
    validate_fence(fence)?;
    let (event_type, payload, payload_bytes, requested_outcome) = match outcome {
        ToolkitExecuteReadTerminalOutput::Result(result) => {
            validate_toolkit_execute_read_result(verified, &result)?;
            let bytes = result.encode_to_vec();
            (
                ExecutionOutputEventTypeV1::ToolkitExecuteReadResult,
                execution_output_frame_v1::Payload::ToolkitExecuteRead(*result),
                bytes,
                ExecutionOutcomeV1::Succeeded,
            )
        }
        ToolkitExecuteReadTerminalOutput::Failure(kind) => {
            let error = runtime_error(kind);
            let bytes = error.encode_to_vec();
            (
                ExecutionOutputEventTypeV1::RuntimeError,
                execution_output_frame_v1::Payload::RuntimeError(error),
                bytes,
                if kind == RuntimeFailureKind::Cancelled {
                    ExecutionOutcomeV1::Cancelled
                } else {
                    ExecutionOutcomeV1::Failed
                },
            )
        }
    };
    let payload_digest = sha256(&payload_bytes);
    let command = verified.command();
    let logical_output_id = format!("toolkit-execute-read:{}", command.execution_id);
    let event_id = format!("{}:{sequence}", command.command_id);
    let frame = ExecutionOutputFrameV1 {
        output_schema_revision: OUTPUT_SCHEMA_REVISION.to_owned(),
        stream_id: format!("{}:{}", command.execution_id, command.generation),
        identity: Some(ExecutionIdentityV1 {
            tenant_id: command.tenant_id.clone(),
            resource_project_id: command.resource_project_id.clone(),
            projection_project_id: command.projection_project_id.clone(),
            command_id: command.command_id.clone(),
            execution_id: command.execution_id.clone(),
            generation: command.generation,
        }),
        fence: Some(fence.clone()),
        logical_output_id: logical_output_id.clone(),
        event_id: event_id.clone(),
        sequence,
        claim_handoff_watermark,
        event_type: event_type as i32,
        occurred_at_unix_millis,
        payload_digest: Some(payload_digest.clone()),
        terminal: true,
        settlement_proposal: Some(SettlementProposalV1 {
            proposal_id: format!("{}:settlement", command.command_id),
            requested_outcome: requested_outcome as i32,
            terminal_logical_output_id: logical_output_id,
            terminal_event_id: event_id,
            terminal_sequence: sequence,
            terminal_payload_digest: Some(payload_digest),
            prepare_idempotency_key: format!("{}:prepare-settlement", command.command_id),
        }),
        payload: Some(payload),
    };
    if frame.encoded_len() > MAX_OUTPUT_FRAME_BYTES {
        return Err(ProtocolError::ResourceExhausted(
            "the direct toolkit terminal exceeds the approved output limit",
        ));
    }
    Ok(frame)
}

fn runtime_error(kind: RuntimeFailureKind) -> RuntimeErrorV1 {
    let (code, safe_message, retryable) = match kind {
        RuntimeFailureKind::UnsupportedCapability => (
            RuntimeErrorCodeV1::UnsupportedCapability,
            "Configuration type is not supported.",
            false,
        ),
        RuntimeFailureKind::IncompatibleVersion => (
            RuntimeErrorCodeV1::IncompatibleVersion,
            "The requested contract version is not compatible.",
            false,
        ),
        RuntimeFailureKind::InvalidInput => (
            RuntimeErrorCodeV1::InvalidInput,
            "The execution input is invalid.",
            false,
        ),
        RuntimeFailureKind::ResourceExhausted => (
            RuntimeErrorCodeV1::ResourceExhausted,
            "The execution exceeded an approved resource limit.",
            false,
        ),
        RuntimeFailureKind::DependencyUnavailable => (
            RuntimeErrorCodeV1::DependencyUnavailable,
            "A required runtime dependency is unavailable.",
            true,
        ),
        RuntimeFailureKind::DeadlineExceeded => (
            RuntimeErrorCodeV1::DeadlineExceeded,
            "The execution deadline was exceeded.",
            true,
        ),
        RuntimeFailureKind::AuthorizationFailed => (
            RuntimeErrorCodeV1::AuthorizationFailed,
            "Execution authorization failed.",
            false,
        ),
        RuntimeFailureKind::Cancelled => (
            RuntimeErrorCodeV1::Cancelled,
            "Execution was cancelled.",
            false,
        ),
        RuntimeFailureKind::Internal => (
            RuntimeErrorCodeV1::Internal,
            "The runtime operation failed.",
            false,
        ),
    };
    RuntimeErrorV1 {
        code: code as i32,
        safe_message: safe_message.to_owned(),
        retryable,
    }
}

fn canonical_runtime_failure(error: &RuntimeErrorV1) -> Option<RuntimeFailureKind> {
    [
        RuntimeFailureKind::UnsupportedCapability,
        RuntimeFailureKind::IncompatibleVersion,
        RuntimeFailureKind::InvalidInput,
        RuntimeFailureKind::ResourceExhausted,
        RuntimeFailureKind::DependencyUnavailable,
        RuntimeFailureKind::DeadlineExceeded,
        RuntimeFailureKind::AuthorizationFailed,
        RuntimeFailureKind::Cancelled,
        RuntimeFailureKind::Internal,
    ]
    .into_iter()
    .find(|kind| runtime_error(*kind) == *error)
}

fn validate_result_command_binding(
    verified: &VerifiedAgentCommand,
    result: &AgentExecutionResultV1,
) -> Result<(), ProtocolError> {
    let command = verified.command();
    let Some(input) = command.input_bundle_ref.as_ref() else {
        return Err(ProtocolError::InvalidInput(
            "the agent result does not match its command binding",
        ));
    };
    let Some(worker_command_v1::CapabilityCommand::AgentExecution(agent)) =
        command.capability_command.as_ref()
    else {
        return Err(ProtocolError::InvalidInput(
            "the agent result does not match its command binding",
        ));
    };
    if result.input_bundle_id != input.input_bundle_id
        || result.input_bundle_digest != input.digest
        || result.request_entry_id != agent.request_entry_id
    {
        return Err(ProtocolError::InvalidInput(
            "the agent result does not match its command binding",
        ));
    }
    Ok(())
}

fn validate_toolkit_execute_read_result(
    verified: &VerifiedToolkitExecuteReadCommand,
    result: &ToolkitExecuteReadResultV1,
) -> Result<(), ProtocolError> {
    let command = verified.command();
    let input = command
        .input_bundle_ref
        .as_ref()
        .ok_or_else(malformed_restored_output)?;
    let Some(worker_command_v1::CapabilityCommand::ToolkitExecuteRead(toolkit)) =
        command.capability_command.as_ref()
    else {
        return Err(malformed_restored_output());
    };
    let identities = [
        result.request_entry_id.as_str(),
        result.request_entry_version.as_str(),
        result.toolkit_type.as_str(),
        result.toolkit_name.as_str(),
        result.tool_name.as_str(),
    ];
    if result.input_bundle_id != input.input_bundle_id
        || result.input_bundle_digest != input.digest
        || result.request_entry_id != toolkit.request_entry_id
        || identities.iter().any(|value| {
            value.is_empty()
                || value.len() > MAX_TOOLKIT_IDENTITY_BYTES
                || value
                    .bytes()
                    .any(|byte| matches!(byte, b'\0' | b'\r' | b'\n'))
        })
        || !valid_sha256(result.request_content_digest.as_ref())
        || result.result_json.is_empty()
        || result.result_json.len() > MAX_TOOLKIT_EXECUTE_READ_RESULT_BYTES
    {
        return Err(malformed_restored_output());
    }
    let value: serde_json::Value =
        serde_json::from_slice(&result.result_json).map_err(|_| malformed_restored_output())?;
    if serde_json::to_vec(&value).ok().as_deref() != Some(result.result_json.as_slice()) {
        return Err(malformed_restored_output());
    }
    Ok(())
}

fn valid_sha256(value: Option<&DigestV1>) -> bool {
    value.is_some_and(|digest| {
        digest.algorithm == DigestAlgorithmV1::Sha256 as i32
            && digest.value.len() == 32
            && digest.value.iter().any(|byte| *byte != 0)
    })
}

fn sha256(payload: &[u8]) -> DigestV1 {
    DigestV1 {
        algorithm: DigestAlgorithmV1::Sha256 as i32,
        value: digest::digest(&digest::SHA256, payload).as_ref().to_vec(),
    }
}

fn validate_fence(fence: &ExecutionFenceV1) -> Result<(), ProtocolError> {
    if fence.workload_session_id.is_empty()
        || fence.producer_id.is_empty()
        || fence.claim_attempt == 0
        || fence.lease_epoch == 0
        || fence.fence_token.len() != 32
        || fence.fence_token.iter().all(|byte| *byte == 0)
    {
        return Err(ProtocolError::InvalidInput(
            "the worker command fence is malformed",
        ));
    }
    if fence.workload_session_id.len() > MAX_SAFE_STRING_BYTES
        || fence.producer_id.len() > MAX_SAFE_STRING_BYTES
    {
        return Err(ProtocolError::ResourceExhausted(
            "the worker command fence exceeds the string limit",
        ));
    }
    Ok(())
}

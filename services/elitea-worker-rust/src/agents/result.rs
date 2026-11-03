use super::request::AgentExecutionRequest;
#[cfg(test)]
use super::request::AgentInputBinding;
use crate::protocol::{
    ProtocolError as AgentProtocolError,
    elitea::runtime::v1::{
        AgentExecutionArtifactReferenceV1, AgentExecutionResultV1, AgentExecutionTerminalStateV1,
        DigestAlgorithmV1, DigestV1,
    },
};

pub const AGENT_RESULT_MEDIA_TYPE: &str = "application/vnd.elitea.agent-execution-result.v1+json";
pub const AGENT_RESULT_CLASSIFICATION: &str = "tenant-confidential";

const MAX_RESULT_METADATA_BYTES: usize = 256;
const MAX_RESULT_ARTIFACT_BYTES: u64 = 64 * 1024;

/// Terminal states currently accepted end to end by Main.
///
/// `PARKED_CHILDREN` is deliberately absent until the declared proto state is
/// accepted by Main and proven by durable parent/child component tests.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum AgentTerminalState {
    Completed,
    PausedHitl,
    PausedMcpAuth,
    /// The run reached its step limit under an unattended grant (ADR-0045):
    /// it completed with its text so far, and Main may continue it.
    StepLimitReached,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct AgentResultArtifact {
    pub artifact_id: String,
    pub immutable_version: String,
    pub byte_length: u64,
    pub digest: [u8; 32],
}

pub struct BoundAgentExecutionResult(AgentExecutionResultV1);

impl BoundAgentExecutionResult {
    pub(crate) fn into_message(self) -> AgentExecutionResultV1 {
        self.0
    }

    #[must_use]
    pub const fn message(&self) -> &AgentExecutionResultV1 {
        &self.0
    }
}

/// Exact admitted-input binding retained without the request payload.
///
/// This value lets the durable output owner build the terminal result without
/// accepting a second caller-selected request after native execution. It is
/// neither cloneable nor formattable because it is a provenance boundary, even
/// though it contains no invocation or output authority.
pub(crate) struct AgentResultBinding {
    input_bundle_id: String,
    input_bundle_digest: [u8; 32],
    request_entry_id: String,
    request_immutable_version: String,
    request_content_digest: [u8; 32],
}

impl AgentResultBinding {
    #[must_use]
    pub(crate) fn from_request(request: &AgentExecutionRequest) -> Self {
        let binding = &request.binding;
        Self {
            input_bundle_id: binding.input_bundle_id.clone(),
            input_bundle_digest: binding.input_bundle_digest,
            request_entry_id: binding.request_entry_id.clone(),
            request_immutable_version: binding.request_immutable_version.clone(),
            request_content_digest: binding.request_content_digest,
        }
    }

    #[cfg(test)]
    #[must_use]
    pub(crate) fn from_input_binding(binding: &AgentInputBinding) -> Self {
        Self {
            input_bundle_id: binding.input_bundle_id.clone(),
            input_bundle_digest: binding.input_bundle_digest,
            request_entry_id: binding.request_entry_id.clone(),
            request_immutable_version: binding.request_immutable_version.clone(),
            request_content_digest: binding.request_content_digest,
        }
    }

    pub(crate) fn bind_artifact(
        &self,
        terminal_state: AgentTerminalState,
        artifact: AgentResultArtifact,
    ) -> Result<BoundAgentExecutionResult, AgentProtocolError> {
        validate_artifact(&artifact)?;
        Ok(BoundAgentExecutionResult(AgentExecutionResultV1 {
            input_bundle_id: self.input_bundle_id.clone(),
            input_bundle_digest: Some(digest(self.input_bundle_digest)),
            request_entry_id: self.request_entry_id.clone(),
            request_immutable_version: self.request_immutable_version.clone(),
            request_content_digest: Some(digest(self.request_content_digest)),
            terminal_state: terminal_state_value(terminal_state),
            result_artifact: Some(artifact_reference(artifact)),
            // Main can now persist attachment text that a worker consumed, but
            // the Rust worker does not yet own the attachment read/write path.
            // Keep the protocol field explicit and empty until that platform
            // boundary is wired end to end.
            attachment_contents: Vec::new(),
        }))
    }
}

/// Bind one admitted terminal outcome to an immutable artifact reference.
///
/// # Errors
///
/// Returns [`AgentProtocolError::InvalidInput`] when the artifact identity,
/// version, or length is empty.
pub fn bind_result_artifact(
    request: &AgentExecutionRequest,
    terminal_state: AgentTerminalState,
    artifact: AgentResultArtifact,
) -> Result<BoundAgentExecutionResult, AgentProtocolError> {
    AgentResultBinding::from_request(request).bind_artifact(terminal_state, artifact)
}

fn validate_artifact(artifact: &AgentResultArtifact) -> Result<(), AgentProtocolError> {
    if !valid_metadata(&artifact.artifact_id)
        || !valid_metadata(&artifact.immutable_version)
        || artifact.byte_length == 0
        || artifact.byte_length > MAX_RESULT_ARTIFACT_BYTES
        || artifact.digest.iter().all(|byte| *byte == 0)
    {
        return Err(AgentProtocolError::InvalidInput(
            "the agent result artifact binding is malformed",
        ));
    }
    Ok(())
}

fn terminal_state_value(terminal_state: AgentTerminalState) -> i32 {
    match terminal_state {
        AgentTerminalState::Completed => AgentExecutionTerminalStateV1::Completed.into(),
        AgentTerminalState::PausedHitl => AgentExecutionTerminalStateV1::PausedHitl.into(),
        AgentTerminalState::PausedMcpAuth => AgentExecutionTerminalStateV1::PausedMcpAuth.into(),
        AgentTerminalState::StepLimitReached => {
            AgentExecutionTerminalStateV1::StepLimitReached.into()
        }
    }
}

fn artifact_reference(artifact: AgentResultArtifact) -> AgentExecutionArtifactReferenceV1 {
    AgentExecutionArtifactReferenceV1 {
        artifact_id: artifact.artifact_id,
        immutable_version: artifact.immutable_version,
        media_type: AGENT_RESULT_MEDIA_TYPE.to_owned(),
        byte_length: artifact.byte_length,
        digest: Some(digest(artifact.digest)),
        classification: AGENT_RESULT_CLASSIFICATION.to_owned(),
    }
}

pub(crate) fn validate_agent_execution_result(
    result: &AgentExecutionResultV1,
) -> Result<(), AgentProtocolError> {
    if !valid_metadata(&result.input_bundle_id)
        || !valid_metadata(&result.request_entry_id)
        || !valid_metadata(&result.request_immutable_version)
        || !valid_sha256(result.input_bundle_digest.as_ref())
        || !valid_sha256(result.request_content_digest.as_ref())
        || !matches!(
            AgentExecutionTerminalStateV1::try_from(result.terminal_state),
            Ok(AgentExecutionTerminalStateV1::Completed
                | AgentExecutionTerminalStateV1::PausedHitl
                | AgentExecutionTerminalStateV1::PausedMcpAuth
                | AgentExecutionTerminalStateV1::StepLimitReached)
        )
    {
        return Err(AgentProtocolError::InvalidInput(
            "the agent result binding is malformed",
        ));
    }
    let Some(artifact) = result.result_artifact.as_ref() else {
        return Err(AgentProtocolError::InvalidInput(
            "the agent result artifact binding is malformed",
        ));
    };
    if !valid_metadata(&artifact.artifact_id)
        || !valid_metadata(&artifact.immutable_version)
        || artifact.media_type != AGENT_RESULT_MEDIA_TYPE
        || artifact.classification != AGENT_RESULT_CLASSIFICATION
        || artifact.byte_length == 0
        || artifact.byte_length > MAX_RESULT_ARTIFACT_BYTES
        || !valid_sha256(artifact.digest.as_ref())
    {
        return Err(AgentProtocolError::InvalidInput(
            "the agent result artifact binding is malformed",
        ));
    }
    Ok(())
}

fn valid_metadata(value: &str) -> bool {
    !value.is_empty()
        && value.len() <= MAX_RESULT_METADATA_BYTES
        && !value
            .bytes()
            .any(|byte| byte.is_ascii_control() || byte == 0x7f)
}

fn valid_sha256(value: Option<&DigestV1>) -> bool {
    value.is_some_and(|value| {
        value.algorithm == DigestAlgorithmV1::Sha256 as i32
            && value.value.len() == 32
            && value.value.iter().any(|byte| *byte != 0)
    })
}

fn digest(value: [u8; 32]) -> DigestV1 {
    DigestV1 {
        algorithm: DigestAlgorithmV1::Sha256.into(),
        value: value.into(),
    }
}

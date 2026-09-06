use std::fmt;

use prost::Message;
use ring::digest;
use subtle::ConstantTimeEq;
use tonic::transport::Channel;
use zeroize::Zeroizing;

use super::ProtocolError;
use super::command::{
    VerifiedAgentCommand, VerifiedExecutionCommand, VerifiedToolkitExecuteReadCommand,
};
use super::elitea::runtime::v1::{
    AgentExecutionResultV1, AuthorizeInvocationDispositionV1, AuthorizeInvocationRequestV1,
    AuthorizeInvocationResponseV1, BeginExecutionDispositionV1, BeginExecutionRequestV1,
    BeginExecutionResponseV1, ClaimCommandRequestV1, ClaimCommandResponseV1, ClaimDispositionV1,
    ClaimReceiptV1, DesiredExecutionStateV1, DigestAlgorithmV1, DigestV1, ExecutionFenceV1,
    ExecutionIdentityV1, ExecutionInputBundleReferenceV1, ExecutionInputBundleV1,
    ExecutionInputEntryV1, ExecutionOutcomeV1, ExecutionOutputFrameV1, NodeEventV1,
    ObserveDesiredStateRequestV1, ObserveDesiredStateResponseV1, PrepareSettlementRequestV1,
    PrepareSettlementResponseV1, RenewLeaseRequestV1, RenewLeaseResponseV1, RuntimeErrorCodeV1,
    RuntimeErrorV1, ScopedContentReferenceV1, SettlementProposalV1, SettlementRecoveryV1,
    worker_command_v1,
};
use super::output::{
    AgentTerminalOutput, RuntimeFailureKind, ToolkitExecuteReadTerminalOutput,
    build_agent_terminal_output_frame, build_node_event_output_frame,
    build_toolkit_execute_read_terminal_output_frame,
};
use crate::agents::result::BoundAgentExecutionResult;
use crate::transport::output_grpc::{
    FrameBoundProgressRejection, ProgressRejectionWinner, progress_frame_sha256,
};
use crate::transport::{
    ControlGrpcClient, ControlGrpcConfig, ControlGrpcError, ControlRpc, DurablyAckedProgress,
    DurablyAckedTerminal, TonicControlRpc,
};

const MAX_CONTROL_IDENTITY_BYTES: usize = 256;
const MAX_MANIFEST_TEXT_BYTES: usize = 128;
const MAX_AGENT_INPUT_BYTES: u64 = 1024 * 1024;
const MAX_TOOLKIT_EXECUTE_READ_INPUT_BYTES: u64 = 1024 * 1024;
const AGENT_EXECUTION_REQUEST_ROLE: &str = "agent.execution_request";
const AGENT_INPUT_MEDIA_TYPE: &str = "application/vnd.elitea.agent-execution-input.v1+protobuf";
const TOOLKIT_EXECUTE_READ_REQUEST_ROLE: &str = "toolkit.execute_read_request";
const TOOLKIT_EXECUTE_READ_INPUT_MEDIA_TYPE: &str =
    "application/vnd.elitea.toolkit-execute-read-input.v1+protobuf";
const INPUT_GRANT_AUDIENCE: &str = "elitea.runtime.input.read.v1";
const DEADLINE_RETIREMENT_SAFE_MESSAGE: &str =
    "The execution deadline was exceeded before worker authority was granted.";

/// Stable semantic failures returned by the runtime control boundary.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum ControlSemanticError {
    InvalidInput(&'static str),
    ResourceExhausted(&'static str),
    IncompatibleVersion(&'static str),
    AuthorizationFailed(&'static str),
    UnsupportedCapability(&'static str),
    DependencyUnavailable(&'static str),
    Cancelled(&'static str),
    DeadlineExceeded(&'static str),
    Rejected(RuntimeControlRejection),
}

impl fmt::Display for ControlSemanticError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::InvalidInput(message)
            | Self::ResourceExhausted(message)
            | Self::IncompatibleVersion(message)
            | Self::AuthorizationFailed(message)
            | Self::UnsupportedCapability(message)
            | Self::DependencyUnavailable(message)
            | Self::Cancelled(message)
            | Self::DeadlineExceeded(message) => formatter.write_str(message),
            Self::Rejected(rejection) => rejection.fmt(formatter),
        }
    }
}

impl std::error::Error for ControlSemanticError {}

/// Stable semantic-or-transport failure for authenticated agent control.
#[derive(Clone, Debug)]
pub enum AgentControlError {
    Semantic(ControlSemanticError),
    Transport(ControlGrpcError),
}

impl AgentControlError {
    /// Whether another delivery attempt can succeed without changing the
    /// immutable command. Authenticated Main rejections retain Main's explicit
    /// decision; only transport unavailability is intrinsically retryable.
    #[must_use]
    pub const fn retryable(&self) -> bool {
        match self {
            Self::Transport(ControlGrpcError::Unavailable(_))
            | Self::Semantic(ControlSemanticError::DependencyUnavailable(_)) => true,
            Self::Semantic(ControlSemanticError::Rejected(rejection)) => rejection.retryable(),
            Self::Transport(
                ControlGrpcError::InvalidConfiguration(_) | ControlGrpcError::ResourceExhausted(_),
            )
            | Self::Semantic(
                ControlSemanticError::InvalidInput(_)
                | ControlSemanticError::ResourceExhausted(_)
                | ControlSemanticError::IncompatibleVersion(_)
                | ControlSemanticError::AuthorizationFailed(_)
                | ControlSemanticError::UnsupportedCapability(_)
                | ControlSemanticError::Cancelled(_)
                | ControlSemanticError::DeadlineExceeded(_),
            ) => false,
        }
    }
}

impl fmt::Display for AgentControlError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::Semantic(error) => error.fmt(formatter),
            Self::Transport(error) => error.fmt(formatter),
        }
    }
}

impl std::error::Error for AgentControlError {
    fn source(&self) -> Option<&(dyn std::error::Error + 'static)> {
        match self {
            Self::Semantic(error) => Some(error),
            Self::Transport(error) => Some(error),
        }
    }
}

impl From<ControlSemanticError> for AgentControlError {
    fn from(value: ControlSemanticError) -> Self {
        Self::Semantic(value)
    }
}

impl From<ControlGrpcError> for AgentControlError {
    fn from(value: ControlGrpcError) -> Self {
        Self::Transport(value)
    }
}

/// Closed runtime rejection categories without server-supplied text.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum RuntimeControlRejectionKind {
    UnsupportedCapability,
    IncompatibleVersion,
    InvalidInput,
    ResourceExhausted,
    DependencyUnavailable,
    AuthorizationFailed,
    Cancelled,
    DeadlineExceeded,
}

/// One authenticated control rejection, retaining Main's retry decision.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct RuntimeControlRejection {
    kind: RuntimeControlRejectionKind,
    retryable: bool,
}

impl RuntimeControlRejection {
    #[must_use]
    pub const fn kind(self) -> RuntimeControlRejectionKind {
        self.kind
    }

    #[must_use]
    pub const fn retryable(self) -> bool {
        self.retryable
    }
}

impl fmt::Display for RuntimeControlRejection {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        let message = match self.kind {
            RuntimeControlRejectionKind::UnsupportedCapability => {
                "the runtime capability is unsupported"
            }
            RuntimeControlRejectionKind::IncompatibleVersion => {
                "the runtime contract is incompatible"
            }
            RuntimeControlRejectionKind::InvalidInput => {
                "the runtime control response rejected its input"
            }
            RuntimeControlRejectionKind::ResourceExhausted => {
                "the runtime control operation exceeded an approved limit"
            }
            RuntimeControlRejectionKind::DependencyUnavailable => {
                "a required runtime control dependency is unavailable"
            }
            RuntimeControlRejectionKind::AuthorizationFailed => {
                "runtime control authorization failed"
            }
            RuntimeControlRejectionKind::Cancelled => "execution cancellation was observed",
            RuntimeControlRejectionKind::DeadlineExceeded => "the execution deadline was exceeded",
        };
        formatter.write_str(message)
    }
}

/// One fresh, active, command-bound agent claim.
///
/// Its fields are private so a decoded protobuf cannot be mistaken for worker
/// authority. The type is intentionally not `Clone`.
pub struct AcceptedAgentClaim {
    identity: ExecutionIdentityV1,
    command_binding: [u8; 32],
    fence: ExecutionFenceV1,
    lease_expires_at_unix_millis: i64,
    claim_started_at_unix_micros: i64,
    claim_id: String,
    claim_handoff_watermark: u64,
    input_bundle_ref: ExecutionInputBundleReferenceV1,
    input_bundle: ExecutionInputBundleV1,
    request_entry: ExecutionInputEntryV1,
}

impl AcceptedAgentClaim {
    #[must_use]
    pub const fn lease_expires_at_unix_millis(&self) -> i64 {
        self.lease_expires_at_unix_millis
    }

    #[must_use]
    pub const fn claim_handoff_watermark(&self) -> u64 {
        self.claim_handoff_watermark
    }

    /// Return the non-secret producer identity needed for execution-spool key
    /// separation. The fence token and remaining bearer authority stay sealed.
    #[must_use]
    pub(crate) fn producer_id(&self) -> &str {
        &self.fence.producer_id
    }

    /// Compare non-secret output transport metadata without exposing the raw
    /// fence or allowing callers to reconstruct claim authority.
    #[must_use]
    pub(crate) fn matches_output_transport(
        &self,
        workload_session_id: &str,
        producer_id: &str,
    ) -> bool {
        self.fence.workload_session_id == workload_session_id
            && self.fence.producer_id == producer_id
    }

    /// Compare restored output binding without exposing the raw fence token.
    #[must_use]
    pub(crate) fn matches_output_binding(
        &self,
        identity: Option<&ExecutionIdentityV1>,
        fence: Option<&ExecutionFenceV1>,
        handoff_watermark: u64,
    ) -> bool {
        identity == Some(&self.identity)
            && fence == Some(&self.fence)
            && handoff_watermark == self.claim_handoff_watermark
    }

    /// Compare the non-secret structural command identity independently from
    /// a possibly stale output fence and handoff watermark.
    #[must_use]
    pub(crate) fn matches_output_identity(&self, identity: Option<&ExecutionIdentityV1>) -> bool {
        identity == Some(&self.identity)
    }

    #[must_use]
    fn matches_verified_command(&self, verified: &impl VerifiedExecutionCommand) -> bool {
        let binding = verified_command_binding(verified);
        self.identity == identity_from_command(verified)
            && bool::from(self.command_binding.ct_eq(&binding))
    }

    #[must_use]
    pub(crate) const fn input_bundle_ref(&self) -> &ExecutionInputBundleReferenceV1 {
        &self.input_bundle_ref
    }

    #[must_use]
    pub(crate) const fn input_bundle(&self) -> &ExecutionInputBundleV1 {
        &self.input_bundle
    }

    #[must_use]
    pub(crate) const fn request_entry(&self) -> &ExecutionInputEntryV1 {
        &self.request_entry
    }

    #[must_use]
    pub(crate) fn matches_agent_result_binding(&self, result: &AgentExecutionResultV1) -> bool {
        let Some(content) = self.request_entry.content.as_ref() else {
            return false;
        };
        result.request_immutable_version == self.request_entry.immutable_version
            && result.request_content_digest.as_ref() == content.digest.as_ref()
    }

    #[must_use]
    pub(crate) fn matches_toolkit_execute_read_result_binding(
        &self,
        result: &super::elitea::runtime::v1::ToolkitExecuteReadResultV1,
    ) -> bool {
        let Some(content) = self.request_entry.content.as_ref() else {
            return false;
        };
        result.request_entry_version == self.request_entry.immutable_version
            && result.request_content_digest.as_ref() == content.digest.as_ref()
    }

    /// Consume fresh business authority after a terminal spool has been
    /// admitted, retaining only the exact lease/output binding needed for
    /// replay. The input manifest is deliberately destroyed here.
    pub(crate) fn into_terminal_recovery(self) -> AcceptedTerminalClaimRecovery {
        let Self {
            identity,
            fence,
            lease_expires_at_unix_millis,
            claim_started_at_unix_micros: _,
            claim_id,
            claim_handoff_watermark,
            command_binding: _,
            input_bundle_ref: _,
            input_bundle: _,
            request_entry: _,
        } = self;
        AcceptedTerminalClaimRecovery {
            binding: RecoveryClaimBinding {
                identity,
                fence,
                lease_expires_at_unix_millis,
                claim_id,
                claim_handoff_watermark,
                desired_state: DesiredExecutionState::Running,
            },
        }
    }

    #[must_use]
    fn begin_execution_request(&self) -> BeginExecutionRequestV1 {
        BeginExecutionRequestV1 {
            identity: Some(self.identity.clone()),
            fence: Some(self.fence.clone()),
        }
    }

    #[must_use]
    #[allow(dead_code)] // Called by the next owned invocation coordinator.
    fn authorize_invocation_request(&self) -> AuthorizeInvocationRequestV1 {
        AuthorizeInvocationRequestV1 {
            identity: Some(self.identity.clone()),
            fence: Some(self.fence.clone()),
        }
    }
}

/// Exhaustive, authenticated claim result for one agent delivery.
///
/// Only [`Self::Accepted`] contains immutable business inputs. Every recovery
/// variant is deliberately input-free and cannot be converted into fresh SDK
/// invocation authority.
pub enum AgentClaimDecision {
    Accepted(Box<AcceptedAgentClaim>),
    RecoverTerminalAck(Box<RecoverTerminalAck>),
    RecoverSettlement(RecoveredSettlement),
    SettledAck(TerminalCommandAck),
    ObsoleteAck(TerminalCommandAck),
    ActiveLeaseNoAck(AgentOutputRecovery),
    RetryLaterNoAck(RetryLaterNoAck),
    RetiredAck(TerminalCommandAck),
    RecoverRunningNoAck(AgentOutputRecovery),
    RecoverAmbiguousInvocationNoAck(AgentOutputRecovery),
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum TerminalRedeliveryKind {
    Settled,
    Obsolete,
    Retired,
}

/// Server proof that one redelivered command may be retired without business
/// input resolution or SDK execution. The value is opaque and non-cloneable.
pub struct TerminalCommandAck {
    kind: TerminalRedeliveryKind,
    retirement: CommandRetirementBinding,
}

impl TerminalCommandAck {
    #[must_use]
    pub const fn kind(&self) -> TerminalRedeliveryKind {
        self.kind
    }
}

/// A retry/quarantine decision carrying no worker authority.
pub struct RetryLaterNoAck {
    _sealed: (),
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum AgentOutputRecoveryKind {
    ActiveLease,
    Running,
    AmbiguousInvocation,
}

struct RecoveryClaimBinding {
    identity: ExecutionIdentityV1,
    fence: ExecutionFenceV1,
    lease_expires_at_unix_millis: i64,
    claim_id: String,
    claim_handoff_watermark: u64,
    desired_state: DesiredExecutionState,
}

/// Input-free authority retained for an exact terminal found on ACCEPTED.
///
/// Unlike [`AcceptedAgentClaim`], this type has no Begin or input path. The
/// next recovery coordinator may consume it only into supervised lease and
/// terminal-output operations.
#[allow(dead_code)] // Consumed by the disabled terminal-recovery coordinator.
pub(crate) struct AcceptedTerminalClaimRecovery {
    binding: RecoveryClaimBinding,
}

impl AcceptedTerminalClaimRecovery {
    /// Consume terminal-only recovery authority into the unique lease handle
    /// used while the exact durable frame is replayed and settled.
    #[allow(dead_code)] // Consumed by the disabled terminal-recovery coordinator.
    pub(crate) fn into_lease_handle(self) -> ClaimLeaseHandle {
        ClaimLeaseHandle {
            identity: self.binding.identity,
            fence: self.binding.fence,
            claim_id: self.binding.claim_id,
            lease_expires_at_unix_millis: self.binding.lease_expires_at_unix_millis,
            renewal_sequence: 0,
        }
    }
}

/// Input-free authority for exact durable output recovery under an active
/// replacement fence. It never grants `BeginExecution` or SDK invocation.
pub struct AgentOutputRecovery {
    kind: AgentOutputRecoveryKind,
    binding: RecoveryClaimBinding,
}

impl AgentOutputRecovery {
    #[must_use]
    pub const fn kind(&self) -> AgentOutputRecoveryKind {
        self.kind
    }

    #[must_use]
    pub const fn desired_state(&self) -> DesiredExecutionState {
        self.binding.desired_state
    }

    #[must_use]
    pub const fn claim_handoff_watermark(&self) -> u64 {
        self.binding.claim_handoff_watermark
    }

    #[must_use]
    pub const fn lease_expires_at_unix_millis(&self) -> i64 {
        self.binding.lease_expires_at_unix_millis
    }

    #[must_use]
    pub(crate) fn matches_output_transport(
        &self,
        workload_session_id: &str,
        producer_id: &str,
    ) -> bool {
        self.binding.fence.workload_session_id == workload_session_id
            && self.binding.fence.producer_id == producer_id
    }

    #[must_use]
    pub(crate) fn producer_id(&self) -> &str {
        &self.binding.fence.producer_id
    }

    /// Separate the exact-fence recovery state from its unique lease handle.
    /// The delivery integration must supervise that handle before publishing.
    #[must_use]
    pub fn split_lease_authority(self) -> (LeasedAgentOutputRecovery, ClaimLeaseHandle) {
        let lease = ClaimLeaseHandle {
            identity: self.binding.identity.clone(),
            fence: self.binding.fence.clone(),
            claim_id: self.binding.claim_id.clone(),
            lease_expires_at_unix_millis: self.binding.lease_expires_at_unix_millis,
            renewal_sequence: 0,
        };
        (
            LeasedAgentOutputRecovery {
                kind: self.kind,
                binding: self.binding,
            },
            lease,
        )
    }
}

/// Recovery state exposed only after the caller owns the unique lease handle.
pub struct LeasedAgentOutputRecovery {
    kind: AgentOutputRecoveryKind,
    binding: RecoveryClaimBinding,
}

impl LeasedAgentOutputRecovery {
    #[must_use]
    pub const fn kind(&self) -> AgentOutputRecoveryKind {
        self.kind
    }

    #[must_use]
    pub const fn desired_state(&self) -> DesiredExecutionState {
        self.binding.desired_state
    }

    #[must_use]
    pub const fn claim_handoff_watermark(&self) -> u64 {
        self.binding.claim_handoff_watermark
    }

    /// Consume recovery-only authority into one canonical failure terminal.
    ///
    /// This value has no input or `BeginExecution` path. The terminal sequence
    /// comes only from Main's authenticated handoff watermark, and the frame
    /// uses the replacement claim fence that the caller is supervising.
    pub(crate) fn bind_failure_terminal(
        self,
        verified: &VerifiedAgentCommand,
        failure: RuntimeFailureKind,
        occurred_at_unix_millis: i64,
    ) -> Result<ExecutionOutputFrameV1, ProtocolError> {
        if self.binding.identity != identity_from_command(verified) {
            return Err(ProtocolError::AuthorizationFailed(
                "the output recovery authority does not match its command",
            ));
        }
        build_agent_terminal_output_frame(
            verified,
            &self.binding.fence,
            AgentTerminalOutput::Failure(failure),
            next_output_sequence(self.binding.claim_handoff_watermark)?,
            occurred_at_unix_millis,
            self.binding.claim_handoff_watermark,
        )
    }

    pub(crate) fn bind_toolkit_execute_read_failure_terminal(
        self,
        verified: &VerifiedToolkitExecuteReadCommand,
        failure: RuntimeFailureKind,
        occurred_at_unix_millis: i64,
    ) -> Result<ExecutionOutputFrameV1, ProtocolError> {
        if self.binding.identity != identity_from_command(verified) {
            return Err(ProtocolError::AuthorizationFailed(
                "the output recovery authority does not match its command",
            ));
        }
        build_toolkit_execute_read_terminal_output_frame(
            verified,
            &self.binding.fence,
            ToolkitExecuteReadTerminalOutput::Failure(failure),
            next_output_sequence(self.binding.claim_handoff_watermark)?,
            occurred_at_unix_millis,
            self.binding.claim_handoff_watermark,
        )
    }
}

/// Exact persisted proposal returned after a terminal output ACK was lost.
/// It can only be consumed by the authenticated settlement RPC method.
pub struct RecoverTerminalAck {
    identity: ExecutionIdentityV1,
    fence: ExecutionFenceV1,
    proposal: SettlementProposalV1,
    proposal_digest: DigestV1,
    idempotency_key: String,
    retirement: CommandRetirementBinding,
}

/// Exact already-prepared settlement receipt. Possession authorizes command
/// retirement but never output replay or SDK execution.
pub struct RecoveredSettlement {
    receipt_id: String,
    outcome: ExecutionOutcomeV1,
    retirement: CommandRetirementBinding,
}

impl RecoveredSettlement {
    #[must_use]
    pub fn receipt_id(&self) -> &str {
        &self.receipt_id
    }

    #[must_use]
    pub const fn outcome(&self) -> ExecutionOutcomeV1 {
        self.outcome
    }
}

fn renewal_request(
    identity: &ExecutionIdentityV1,
    fence: &ExecutionFenceV1,
    claim_id: &str,
    sequence: u64,
) -> Result<RenewLeaseRequestV1, ControlSemanticError> {
    let mut seed = identity.encode_to_vec();
    seed.push(0);
    fence.encode(&mut seed).map_err(|_| {
        ControlSemanticError::InvalidInput("the lease renewal binding is malformed")
    })?;
    seed.push(0);
    seed.extend_from_slice(claim_id.as_bytes());
    let binding = digest::digest(&digest::SHA256, &seed);
    Ok(RenewLeaseRequestV1 {
        identity: Some(identity.clone()),
        fence: Some(fence.clone()),
        idempotency_key: format!("lease-renew:{}:{sequence}", hex_lower(binding.as_ref())),
    })
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum BeginExecutionDecision {
    StartedNow,
    AlreadyStarted,
}

/// Claim state after the restart-safe `BeginExecution` fence succeeds.
pub struct PreparingAgentExecution {
    claim: AcceptedAgentClaim,
}

impl PreparingAgentExecution {
    /// Split the preparation into one execution typestate and one unique lease
    /// handle before exposing business input references.
    #[must_use]
    pub fn start_lease_monitor(self) -> LeaseStartingAgentExecution {
        let lease = ClaimLeaseHandle {
            identity: self.claim.identity.clone(),
            fence: self.claim.fence.clone(),
            claim_id: self.claim.claim_id.clone(),
            lease_expires_at_unix_millis: self.claim.lease_expires_at_unix_millis,
            renewal_sequence: 0,
        };
        LeaseStartingAgentExecution {
            claim: self.claim,
            lease,
        }
    }
}

/// Post-Begin execution state which has not completed its immediate lease poll.
/// Business input and invocation authority remain inaccessible.
pub struct LeaseStartingAgentExecution {
    claim: AcceptedAgentClaim,
    lease: ClaimLeaseHandle,
}

impl LeaseStartingAgentExecution {
    pub(crate) fn split(self) -> (PendingLeaseActivation, ClaimLeaseHandle) {
        (PendingLeaseActivation { claim: self.claim }, self.lease)
    }
}

pub(crate) struct PendingLeaseActivation {
    claim: AcceptedAgentClaim,
}

impl PendingLeaseActivation {
    pub(crate) fn into_monitored(self) -> LeaseMonitoredAgentExecution {
        LeaseMonitoredAgentExecution { claim: self.claim }
    }

    pub(crate) fn into_inactive(self) -> InactiveAgentExecution {
        InactiveAgentExecution { claim: self.claim }
    }
}

/// Opaque execution ownership retained when the immediate lease poll does not
/// grant business-input access. A later coordinator may use it only to produce
/// a canonical pre-invocation terminal or a recovery disposition.
pub struct InactiveAgentExecution {
    claim: AcceptedAgentClaim,
}

impl InactiveAgentExecution {
    pub(crate) fn into_output_authority(self) -> AgentExecutionOutputAuthority {
        AgentExecutionOutputAuthority { claim: self.claim }
    }
}

/// Business-input access after the delivery path owns a unique lease handle.
pub struct LeaseMonitoredAgentExecution {
    claim: AcceptedAgentClaim,
}

/// Borrowed, post-Begin authority used only by the claim-bound input client.
///
/// The type and every field remain crate-private so the fence cannot cross the
/// public API or be reconstructed from caller-selected values.
pub(crate) struct ClaimBoundInputAuthority<'a> {
    pub(crate) execution_id: &'a str,
    pub(crate) generation: u64,
    pub(crate) content_id: &'a str,
    pub(crate) immutable_version: &'a str,
    pub(crate) claim_id: &'a str,
    pub(crate) fence_token: &'a [u8],
    pub(crate) expected_source_length: u64,
    pub(crate) expected_source_sha256: &'a [u8],
    pub(crate) media_type: &'a str,
}

/// Opaque authority for one post-authorization runtime-context redemption.
///
/// The Rust worker allows Main to resolve the actual execution actor only after
/// `AUTHORIZED_NOW`. This value is therefore minted at that exact durable boundary,
/// is neither cloneable nor formattable, and zeroizes its duplicated fence bytes on drop.
/// It cannot submit ADK work, publish output, or settle the Redis delivery.
pub(crate) struct ClaimBoundRuntimeContextAuthority {
    execution_id: String,
    generation: u64,
    claim_id: String,
    fence_token: Zeroizing<Vec<u8>>,
    resource_project_id: String,
}

/// Opaque authority for one claim-fenced ADK session writer.
///
/// This grant is minted beside the runtime-context grant only after
/// `AUTHORIZED_NOW`. It carries the accepted claim identity and fence needed
/// to activate durable session persistence, but no output, settlement, Redis,
/// provider, or invocation-submission capability. The value is intentionally
/// neither cloneable nor formattable and zeroizes its duplicated fence bytes
/// on drop.
pub(crate) struct ClaimBoundSessionAuthority {
    tenant_id: String,
    resource_project_id: String,
    projection_project_id: String,
    execution_id: String,
    generation: u64,
    claim_id: String,
    claim_attempt: u64,
    lease_epoch: u64,
    claim_started_at_unix_micros: i64,
    workload_session_id: String,
    producer_id: String,
    fence_token: Zeroizing<Vec<u8>>,
}

/// Consuming session-writer material exposed only inside the worker crate.
///
/// The raw fence remains owned by this non-cloneable value until the
/// `PostgreSQL` session boundary moves it into its narrower writer authority.
pub(crate) struct SessionWriterClaimBinding {
    pub(crate) tenant_id: String,
    pub(crate) resource_project_id: String,
    pub(crate) projection_project_id: String,
    pub(crate) execution_id: String,
    pub(crate) generation: u64,
    pub(crate) claim_id: String,
    pub(crate) claim_attempt: u64,
    pub(crate) lease_epoch: u64,
    pub(crate) claim_started_at_unix_micros: i64,
    pub(crate) workload_session_id: String,
    pub(crate) producer_id: String,
    pub(crate) fence_token: Zeroizing<Vec<u8>>,
}

/// Borrowed request material exposed only to the hardened runtime-context
/// transport. Keeping this view tied to the opaque owner prevents credentials
/// from being redeemed from caller-selected loose identity fields.
pub(crate) struct RuntimeContextRedemptionBinding<'a> {
    pub(crate) execution_id: &'a str,
    pub(crate) generation: u64,
    pub(crate) claim_id: &'a str,
    pub(crate) fence_token: &'a [u8],
    pub(crate) resource_project_id: &'a str,
}

impl ClaimBoundRuntimeContextAuthority {
    #[must_use]
    fn from_claim(claim: &AcceptedAgentClaim) -> Self {
        Self {
            execution_id: claim.identity.execution_id.clone(),
            generation: claim.identity.generation,
            claim_id: claim.claim_id.clone(),
            fence_token: Zeroizing::new(claim.fence.fence_token.clone()),
            resource_project_id: claim.identity.resource_project_id.clone(),
        }
    }

    #[must_use]
    pub(crate) fn redemption_binding(&self) -> RuntimeContextRedemptionBinding<'_> {
        RuntimeContextRedemptionBinding {
            execution_id: &self.execution_id,
            generation: self.generation,
            claim_id: &self.claim_id,
            fence_token: self.fence_token.as_slice(),
            resource_project_id: &self.resource_project_id,
        }
    }
}

impl ClaimBoundSessionAuthority {
    #[must_use]
    fn from_claim(claim: &AcceptedAgentClaim) -> Self {
        Self {
            tenant_id: claim.identity.tenant_id.clone(),
            resource_project_id: claim.identity.resource_project_id.clone(),
            projection_project_id: claim.identity.projection_project_id.clone(),
            execution_id: claim.identity.execution_id.clone(),
            generation: claim.identity.generation,
            claim_id: claim.claim_id.clone(),
            claim_attempt: claim.fence.claim_attempt,
            lease_epoch: claim.fence.lease_epoch,
            claim_started_at_unix_micros: claim.claim_started_at_unix_micros,
            workload_session_id: claim.fence.workload_session_id.clone(),
            producer_id: claim.fence.producer_id.clone(),
            fence_token: Zeroizing::new(claim.fence.fence_token.clone()),
        }
    }

    #[must_use]
    pub(crate) fn into_writer_binding(self) -> SessionWriterClaimBinding {
        SessionWriterClaimBinding {
            tenant_id: self.tenant_id,
            resource_project_id: self.resource_project_id,
            projection_project_id: self.projection_project_id,
            execution_id: self.execution_id,
            generation: self.generation,
            claim_id: self.claim_id,
            claim_attempt: self.claim_attempt,
            lease_epoch: self.lease_epoch,
            claim_started_at_unix_micros: self.claim_started_at_unix_micros,
            workload_session_id: self.workload_session_id,
            producer_id: self.producer_id,
            fence_token: self.fence_token,
        }
    }
}

impl LeaseMonitoredAgentExecution {
    #[must_use]
    pub const fn input_bundle_ref(&self) -> &ExecutionInputBundleReferenceV1 {
        self.claim.input_bundle_ref()
    }

    #[must_use]
    pub const fn input_bundle(&self) -> &ExecutionInputBundleV1 {
        self.claim.input_bundle()
    }

    #[must_use]
    pub const fn request_entry(&self) -> &ExecutionInputEntryV1 {
        self.claim.request_entry()
    }

    #[must_use]
    pub(crate) fn input_content_authority(&self) -> Option<ClaimBoundInputAuthority<'_>> {
        let content = self.claim.request_entry.content.as_ref()?;
        let source_digest = content.digest.as_ref()?;
        Some(ClaimBoundInputAuthority {
            execution_id: &self.claim.identity.execution_id,
            generation: self.claim.identity.generation,
            content_id: &content.content_id,
            immutable_version: &content.immutable_version,
            claim_id: &self.claim.claim_id,
            fence_token: &self.claim.fence.fence_token,
            expected_source_length: content.byte_length,
            expected_source_sha256: &source_digest.value,
            media_type: &content.media_type,
        })
    }

    pub(crate) fn into_output_authority(self) -> AgentExecutionOutputAuthority {
        AgentExecutionOutputAuthority { claim: self.claim }
    }
}

/// Opaque post-Begin authority retained for pre-invocation terminal output.
/// It exposes neither the fence token nor business input through public API.
pub struct AgentExecutionOutputAuthority {
    #[allow(dead_code)] // Consumed by the next output-coordination slice.
    claim: AcceptedAgentClaim,
}

impl AgentExecutionOutputAuthority {
    /// Consume the exact claim into an ACK-driven output cursor.
    ///
    /// The cursor owns the raw fence and authenticated handoff watermark. A
    /// caller can only obtain claim-bound progress or terminal values and can
    /// advance the sequence after the exact frame ACK is observed.
    pub(crate) fn into_output_cursor(
        self,
        verified: &VerifiedAgentCommand,
    ) -> Result<AgentExecutionOutputCursor, ProtocolError> {
        self.try_into_output_cursor(verified)
            .map_err(AgentOutputCursorBindFailure::into_error)
    }

    /// Preserve the unique claim authority when cursor admission fails.
    ///
    /// The authorized-run publisher transition uses this form so a local
    /// configuration or binding failure can return the complete unstarted run
    /// for explicit no-ACK cleanup instead of silently dropping authority.
    pub(crate) fn try_into_output_cursor(
        self,
        verified: &VerifiedAgentCommand,
    ) -> Result<AgentExecutionOutputCursor, Box<AgentOutputCursorBindFailure>> {
        if !self.claim.matches_verified_command(verified) {
            return Err(Box::new(AgentOutputCursorBindFailure {
                authority: self,
                error: ProtocolError::AuthorizationFailed(
                    "the output authority does not match its command",
                ),
            }));
        }
        let next_sequence = match next_output_sequence(self.claim.claim_handoff_watermark) {
            Ok(next_sequence) => next_sequence,
            Err(error) => {
                return Err(Box::new(AgentOutputCursorBindFailure {
                    authority: self,
                    error,
                }));
            }
        };
        Ok(AgentExecutionOutputCursor {
            claim: self.claim,
            state: AgentOutputCursorState::Ready(next_sequence),
        })
    }

    /// Consume the exact claim authority into one canonical failure terminal.
    ///
    /// The fence token never leaves this boundary. The sequence is derived
    /// from the authenticated handoff watermark and cannot be supplied by a
    /// caller or mixed with another verified command.
    pub(crate) fn bind_failure_terminal(
        self,
        verified: &VerifiedAgentCommand,
        failure: RuntimeFailureKind,
        occurred_at_unix_millis: i64,
    ) -> Result<ExecutionOutputFrameV1, ProtocolError> {
        self.into_output_cursor(verified)?
            .bind_failure_terminal(verified, failure, occurred_at_unix_millis)
            .map(ClaimBoundAgentTerminal::into_frame)
    }

    pub(crate) fn bind_toolkit_execute_read_terminal(
        self,
        verified: &VerifiedToolkitExecuteReadCommand,
        terminal: ToolkitExecuteReadTerminalOutput,
        occurred_at_unix_millis: i64,
    ) -> Result<ExecutionOutputFrameV1, ProtocolError> {
        if !self.claim.matches_verified_command(verified) {
            return Err(ProtocolError::AuthorizationFailed(
                "the output authority does not match its command",
            ));
        }
        build_toolkit_execute_read_terminal_output_frame(
            verified,
            &self.claim.fence,
            terminal,
            next_output_sequence(self.claim.claim_handoff_watermark)?,
            occurred_at_unix_millis,
            self.claim.claim_handoff_watermark,
        )
    }
}

/// Failed cursor admission which still owns the unique output authority.
#[allow(dead_code)] // Consumed by the capability-disabled native lifecycle.
pub(crate) struct AgentOutputCursorBindFailure {
    authority: AgentExecutionOutputAuthority,
    error: ProtocolError,
}

#[allow(dead_code)] // Consumed by the capability-disabled native lifecycle.
impl AgentOutputCursorBindFailure {
    pub(crate) fn into_parts(self: Box<Self>) -> (AgentExecutionOutputAuthority, ProtocolError) {
        (self.authority, self.error)
    }

    fn into_error(self: Box<Self>) -> ProtocolError {
        self.error
    }
}

/// One claim-bound nonterminal output which cannot be mixed with a raw fence.
///
/// The complete frame remains private until the execution-owned delivery
/// coordinator consumes it. This type is intentionally neither cloneable nor
/// formattable.
#[allow(dead_code)] // Consumed by the capability-disabled fresh progress publisher.
pub(crate) struct ClaimBoundAgentProgress {
    frame: ExecutionOutputFrameV1,
}

#[allow(dead_code)] // Consumed by the capability-disabled fresh progress publisher.
impl ClaimBoundAgentProgress {
    #[must_use]
    pub(crate) const fn sequence(&self) -> u64 {
        self.frame.sequence
    }

    pub(crate) fn into_frame(self) -> ExecutionOutputFrameV1 {
        self.frame
    }
}

/// One claim-bound terminal output which consumes all remaining cursor
/// authority. Only the delivery coordinator may unwrap it for durable send.
pub(crate) struct ClaimBoundAgentTerminal {
    frame: ExecutionOutputFrameV1,
}

impl ClaimBoundAgentTerminal {
    pub(crate) fn into_frame(self) -> ExecutionOutputFrameV1 {
        self.frame
    }
}

/// Claim-bound, ACK-driven output sequence owner.
///
/// The cursor retains the only fence copy used by fresh progress and terminal
/// construction. It never speculatively advances: a caller must provide the
/// exact sequence returned by the bound output ACK before another event can be
/// allocated.
pub(crate) struct AgentExecutionOutputCursor {
    claim: AcceptedAgentClaim,
    state: AgentOutputCursorState,
}

enum AgentOutputCursorState {
    Ready(u64),
    Pending {
        sequence: u64,
        frame_sha256: [u8; 32],
    },
    Exhausted,
}

/// Result of consuming an exact durable progress ACK.
pub(crate) enum AgentProgressCommit {
    Advanced,
    Exhausted(ProtocolError),
}

#[allow(dead_code)] // Progress/result methods land before native lifecycle registration.
impl AgentExecutionOutputCursor {
    #[must_use]
    pub(crate) const fn next_sequence(&self) -> Option<u64> {
        match self.state {
            AgentOutputCursorState::Ready(sequence) => Some(sequence),
            AgentOutputCursorState::Pending { .. } | AgentOutputCursorState::Exhausted => None,
        }
    }

    /// Bind one current-compatible `NodeEvent` to the exact live claim.
    pub(crate) fn bind_progress(
        &mut self,
        verified: &VerifiedAgentCommand,
        event: NodeEventV1,
        occurred_at_unix_millis: i64,
    ) -> Result<ClaimBoundAgentProgress, ProtocolError> {
        self.require_command(verified)?;
        let sequence = self.require_ready_sequence()?;
        let frame = build_node_event_output_frame(
            verified,
            &self.claim.fence,
            event,
            sequence,
            occurred_at_unix_millis,
            self.claim.claim_handoff_watermark,
        )?;
        self.state = AgentOutputCursorState::Pending {
            sequence,
            frame_sha256: progress_frame_sha256(&frame),
        };
        Ok(ClaimBoundAgentProgress { frame })
    }

    /// Advance only after Main's bound ACK commits the current progress frame.
    pub(crate) fn commit_progress(
        &mut self,
        acknowledged: DurablyAckedProgress,
    ) -> Result<(), ProtocolError> {
        match self.commit_progress_durable(acknowledged)? {
            AgentProgressCommit::Advanced => Ok(()),
            AgentProgressCommit::Exhausted(error) => Err(error),
        }
    }

    /// Consume one exact ACK while distinguishing durable sequence exhaustion.
    pub(crate) fn commit_progress_durable(
        &mut self,
        acknowledged: DurablyAckedProgress,
    ) -> Result<AgentProgressCommit, ProtocolError> {
        let (acknowledged_sequence, acknowledged_sha256) = acknowledged.into_binding();
        let AgentOutputCursorState::Pending {
            sequence,
            frame_sha256,
        } = &self.state
        else {
            return Err(ProtocolError::AuthorizationFailed(
                "the output cursor has no pending progress frame",
            ));
        };
        if acknowledged_sequence != *sequence
            || !bool::from(acknowledged_sha256.ct_eq(frame_sha256))
        {
            return Err(ProtocolError::AuthorizationFailed(
                "the progress ACK does not cover the exact pending frame",
            ));
        }
        match next_output_sequence(acknowledged_sequence) {
            Ok(next) => {
                self.state = AgentOutputCursorState::Ready(next);
                Ok(AgentProgressCommit::Advanced)
            }
            Err(error) => {
                self.state = AgentOutputCursorState::Exhausted;
                Ok(AgentProgressCommit::Exhausted(error))
            }
        }
    }

    /// Consume the cursor into one request-bound successful terminal.
    pub(crate) fn bind_result_terminal(
        self,
        verified: &VerifiedAgentCommand,
        result: BoundAgentExecutionResult,
        occurred_at_unix_millis: i64,
    ) -> Result<ClaimBoundAgentTerminal, ProtocolError> {
        self.bind_terminal(
            verified,
            AgentTerminalOutput::Result(Box::new(result)),
            occurred_at_unix_millis,
        )
    }

    /// Consume the cursor into one canonical safe failure terminal.
    pub(crate) fn bind_failure_terminal(
        self,
        verified: &VerifiedAgentCommand,
        failure: RuntimeFailureKind,
        occurred_at_unix_millis: i64,
    ) -> Result<ClaimBoundAgentTerminal, ProtocolError> {
        self.bind_terminal(
            verified,
            AgentTerminalOutput::Failure(failure),
            occurred_at_unix_millis,
        )
    }

    /// Consume an exact Main rejection into a same-sequence failure terminal.
    ///
    /// The opaque proof must cover the cursor's sole pending progress frame.
    /// The terminal occurrence time is sampled by the execution lifecycle after
    /// its final lease/deadline priority check; it is not inherited from the
    /// rejected progress event.
    pub(crate) fn bind_rejected_progress_terminal(
        self,
        verified: &VerifiedAgentCommand,
        rejected: FrameBoundProgressRejection,
        occurred_at_unix_millis: i64,
    ) -> Result<ClaimBoundAgentTerminal, ProtocolError> {
        self.require_command(verified)?;
        let (rejected_sequence, rejected_sha256, winner) = rejected.into_binding();
        let AgentOutputCursorState::Pending {
            sequence,
            frame_sha256,
        } = &self.state
        else {
            return Err(ProtocolError::AuthorizationFailed(
                "the output cursor has no rejected progress frame",
            ));
        };
        if rejected_sequence != *sequence || !bool::from(rejected_sha256.ct_eq(frame_sha256)) {
            return Err(ProtocolError::AuthorizationFailed(
                "the progress rejection does not cover the exact pending frame",
            ));
        }
        let failure = match winner {
            ProgressRejectionWinner::Cancelled => RuntimeFailureKind::Cancelled,
            ProgressRejectionWinner::DeadlineExceeded => RuntimeFailureKind::DeadlineExceeded,
        };
        let frame = build_agent_terminal_output_frame(
            verified,
            &self.claim.fence,
            AgentTerminalOutput::Failure(failure),
            rejected_sequence,
            occurred_at_unix_millis,
            self.claim.claim_handoff_watermark,
        )?;
        Ok(ClaimBoundAgentTerminal { frame })
    }

    fn bind_terminal(
        self,
        verified: &VerifiedAgentCommand,
        terminal: AgentTerminalOutput,
        occurred_at_unix_millis: i64,
    ) -> Result<ClaimBoundAgentTerminal, ProtocolError> {
        self.require_command(verified)?;
        let frame = build_agent_terminal_output_frame(
            verified,
            &self.claim.fence,
            terminal,
            self.require_ready_sequence()?,
            occurred_at_unix_millis,
            self.claim.claim_handoff_watermark,
        )?;
        Ok(ClaimBoundAgentTerminal { frame })
    }

    fn require_command(&self, verified: &VerifiedAgentCommand) -> Result<(), ProtocolError> {
        if self.claim.matches_verified_command(verified) {
            Ok(())
        } else {
            Err(ProtocolError::AuthorizationFailed(
                "the output cursor does not match its command",
            ))
        }
    }

    fn require_ready_sequence(&self) -> Result<u64, ProtocolError> {
        match self.state {
            AgentOutputCursorState::Ready(sequence) => Ok(sequence),
            AgentOutputCursorState::Pending { .. } => Err(ProtocolError::AuthorizationFailed(
                "a progress frame remains unacknowledged",
            )),
            AgentOutputCursorState::Exhausted => Err(ProtocolError::ResourceExhausted(
                "the output sequence exceeds the durable counter limit",
            )),
        }
    }
}

fn verified_command_binding(verified: &impl VerifiedExecutionCommand) -> [u8; 32] {
    let value = digest::digest(&digest::SHA256, verified.exact_signed_envelope());
    let mut binding = [0_u8; 32];
    binding.copy_from_slice(value.as_ref());
    binding
}

fn next_output_sequence(current: u64) -> Result<u64, ProtocolError> {
    current
        .checked_add(1)
        .filter(|value| i64::try_from(*value).is_ok())
        .ok_or(ProtocolError::ResourceExhausted(
            "the output sequence exceeds the durable counter limit",
        ))
}

#[cfg(test)]
pub(crate) fn test_agent_output_authority(
    claim: AcceptedAgentClaim,
) -> AgentExecutionOutputAuthority {
    AgentExecutionOutputAuthority { claim }
}

#[cfg(test)]
pub(crate) fn test_agent_output_authority_with_handoff(
    mut claim: AcceptedAgentClaim,
    claim_handoff_watermark: u64,
) -> AgentExecutionOutputAuthority {
    claim.claim_handoff_watermark = claim_handoff_watermark;
    AgentExecutionOutputAuthority { claim }
}

#[cfg(test)]
pub(crate) fn test_lease_monitored_input_execution(
    expected_source_length: u64,
    expected_source_sha256: [u8; 32],
) -> LeaseMonitoredAgentExecution {
    let content = ScopedContentReferenceV1 {
        content_id: "settings id".to_owned(),
        immutable_version: "v/1".to_owned(),
        media_type: AGENT_INPUT_MEDIA_TYPE.to_owned(),
        byte_length: expected_source_length,
        digest: Some(DigestV1 {
            algorithm: DigestAlgorithmV1::Sha256 as i32,
            value: expected_source_sha256.to_vec(),
        }),
        classification: "project".to_owned(),
        required_grant_audience: INPUT_GRANT_AUDIENCE.to_owned(),
    };
    LeaseMonitoredAgentExecution {
        claim: AcceptedAgentClaim {
            identity: ExecutionIdentityV1 {
                execution_id: "execution/one".to_owned(),
                generation: 2,
                ..ExecutionIdentityV1::default()
            },
            command_binding: [0_u8; 32],
            fence: ExecutionFenceV1 {
                fence_token: vec![b'f'; 32],
                ..ExecutionFenceV1::default()
            },
            lease_expires_at_unix_millis: 1_700_000_060_000,
            claim_started_at_unix_micros: 1_700_000_000_123_456,
            claim_id: "claim-1".to_owned(),
            claim_handoff_watermark: 0,
            input_bundle_ref: ExecutionInputBundleReferenceV1::default(),
            input_bundle: ExecutionInputBundleV1::default(),
            request_entry: ExecutionInputEntryV1 {
                entry_id: "agent-request".to_owned(),
                immutable_version: "v/1".to_owned(),
                semantic_role: AGENT_EXECUTION_REQUEST_ROLE.to_owned(),
                content: Some(content),
            },
        },
    }
}

#[cfg(test)]
pub(crate) fn test_runtime_context_authority() -> ClaimBoundRuntimeContextAuthority {
    ClaimBoundRuntimeContextAuthority {
        execution_id: "execution/one".to_owned(),
        generation: 2,
        claim_id: "claim-1".to_owned(),
        fence_token: Zeroizing::new(vec![b'f'; 32]),
        resource_project_id: "17".to_owned(),
    }
}

#[cfg(test)]
pub(crate) fn test_session_authority() -> ClaimBoundSessionAuthority {
    test_session_authority_for("execution/one", 3)
}

#[cfg(test)]
pub(crate) fn test_session_authority_for(
    execution_id: &str,
    generation: u64,
) -> ClaimBoundSessionAuthority {
    ClaimBoundSessionAuthority {
        tenant_id: "tenant-1".to_owned(),
        resource_project_id: "17".to_owned(),
        projection_project_id: "9".to_owned(),
        execution_id: execution_id.to_owned(),
        generation,
        claim_id: "claim-1".to_owned(),
        claim_attempt: 1,
        lease_epoch: 1,
        claim_started_at_unix_micros: 1_700_000_000_123_456,
        workload_session_id: "workload-session-1".to_owned(),
        producer_id: "worker-1".to_owned(),
        fence_token: Zeroizing::new(vec![b'f'; 32]),
    }
}

#[cfg(test)]
pub(crate) fn test_lease_starting_execution(
    lease_expires_at_unix_millis: i64,
) -> LeaseStartingAgentExecution {
    let mut execution = test_lease_monitored_input_execution(1, [0x61; 32]);
    execution.claim.lease_expires_at_unix_millis = lease_expires_at_unix_millis;
    let lease = ClaimLeaseHandle {
        identity: execution.claim.identity.clone(),
        fence: execution.claim.fence.clone(),
        claim_id: execution.claim.claim_id.clone(),
        lease_expires_at_unix_millis,
        renewal_sequence: 0,
    };
    LeaseStartingAgentExecution {
        claim: execution.claim,
        lease,
    }
}

#[cfg(test)]
pub(crate) fn test_terminal_claim_recovery(
    lease_expires_at_unix_millis: i64,
) -> AcceptedTerminalClaimRecovery {
    let mut execution = test_lease_monitored_input_execution(1, [0x61; 32]);
    execution.claim.lease_expires_at_unix_millis = lease_expires_at_unix_millis;
    execution.claim.into_terminal_recovery()
}

/// Recovery-only state proving `BeginExecution` reported prior possible work.
pub struct BeginExecutionRecovery {
    _claim: AcceptedAgentClaim,
}

/// Unique exact-fence lease state. It is neither cloneable nor formattable.
pub struct ClaimLeaseHandle {
    identity: ExecutionIdentityV1,
    fence: ExecutionFenceV1,
    claim_id: String,
    lease_expires_at_unix_millis: i64,
    renewal_sequence: u64,
}

impl ClaimLeaseHandle {
    #[must_use]
    pub const fn lease_expires_at_unix_millis(&self) -> i64 {
        self.lease_expires_at_unix_millis
    }

    fn next_renewal_request(&mut self) -> Result<RenewLeaseRequestV1, ControlSemanticError> {
        let sequence = self
            .renewal_sequence
            .checked_add(1)
            .filter(|value| i64::try_from(*value).is_ok())
            .ok_or(ControlSemanticError::ResourceExhausted(
                "the lease renewal sequence exceeds the durable counter limit",
            ))?;
        self.renewal_sequence = sequence;
        renewal_request(&self.identity, &self.fence, &self.claim_id, sequence)
    }

    fn observe_request(&self) -> ObserveDesiredStateRequestV1 {
        ObserveDesiredStateRequestV1 {
            identity: Some(self.identity.clone()),
            fence: Some(self.fence.clone()),
        }
    }

    pub(crate) fn commit_renewal(&mut self, observation: LeaseObservation) {
        self.lease_expires_at_unix_millis = observation.lease_expires_at_unix_millis;
    }
}

pub enum BeginAgentExecution {
    Preparing(PreparingAgentExecution),
    AlreadyStarted(BeginExecutionRecovery),
}

/// The only value that permits one native ADK submission.
///
/// It is deliberately opaque and non-cloneable. The invocation coordinator
/// must consume the enclosing execution-layer authorized run at the actual
/// runner submission boundary; the permit is never exposed independently.
#[allow(dead_code)] // Constructed by the next owned invocation coordinator.
pub(crate) struct InvocationSubmissionPermit {
    _sealed: (),
}

/// Complete prepared payload bound to the accepted claim before authorization.
///
/// The constructor is crate-private so only the preparation coordinator can
/// bind a validated request, verified command, delivery, reservation, and
/// supervised lease to the claim that produced them.
#[allow(dead_code)] // Consumed by the next owned invocation coordinator.
pub(crate) struct InvocationAuthorizationCandidate<T> {
    execution: LeaseMonitoredAgentExecution,
    payload: T,
}

impl LeaseMonitoredAgentExecution {
    #[allow(dead_code)] // Called only by the sealed preparation handoff for now.
    pub(crate) fn bind_invocation<T>(self, payload: T) -> InvocationAuthorizationCandidate<T> {
        InvocationAuthorizationCandidate {
            execution: self,
            payload,
        }
    }
}

/// Opaque claim ownership retained only long enough to close an authorization
/// attempt whose transport outcome is unknown.
///
/// It grants neither output nor submission access and deliberately exposes no
/// raw claim/fence fields to the execution coordinator.
pub(crate) struct InvocationAuthorizationNoAckAuthority {
    _claim: AcceptedAgentClaim,
}

/// Canonical terminal cause when Main answered the authorization attempt but
/// did not grant fresh invocation authority.
#[derive(Clone, Debug)]
pub enum InvocationAuthorizationTerminalCause {
    AlreadyAuthorized,
    Rejected(AgentControlError),
}

impl InvocationAuthorizationTerminalCause {
    /// Stable low-cardinality category for logs and terminal mapping.
    #[must_use]
    pub const fn code(&self) -> &'static str {
        match self {
            Self::AlreadyAuthorized => "invocation_authorization.already_authorized",
            Self::Rejected(_) => "invocation_authorization.rejected",
        }
    }

    /// Convert only registered, data-free authorization outcomes into their
    /// canonical runtime terminal category.
    #[must_use]
    pub const fn runtime_failure_kind(&self) -> RuntimeFailureKind {
        match self {
            Self::AlreadyAuthorized => RuntimeFailureKind::Internal,
            Self::Rejected(error) => authorization_failure_kind(error),
        }
    }
}

impl fmt::Display for InvocationAuthorizationTerminalCause {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::AlreadyAuthorized => formatter.write_str(
                "the invocation may already have crossed the durable authorization boundary",
            ),
            Self::Rejected(error) => write!(formatter, "invocation authorization failed: {error}"),
        }
    }
}

impl std::error::Error for InvocationAuthorizationTerminalCause {
    fn source(&self) -> Option<&(dyn std::error::Error + 'static)> {
        match self {
            Self::Rejected(error) => Some(error),
            Self::AlreadyAuthorized => None,
        }
    }
}

const fn authorization_failure_kind(error: &AgentControlError) -> RuntimeFailureKind {
    match error {
        AgentControlError::Transport(_) => RuntimeFailureKind::DependencyUnavailable,
        AgentControlError::Semantic(error) => match error {
            ControlSemanticError::InvalidInput(_) => RuntimeFailureKind::InvalidInput,
            ControlSemanticError::ResourceExhausted(_) => RuntimeFailureKind::ResourceExhausted,
            ControlSemanticError::IncompatibleVersion(_) => RuntimeFailureKind::IncompatibleVersion,
            ControlSemanticError::AuthorizationFailed(_) => RuntimeFailureKind::AuthorizationFailed,
            ControlSemanticError::UnsupportedCapability(_) => {
                RuntimeFailureKind::UnsupportedCapability
            }
            ControlSemanticError::DependencyUnavailable(_) => {
                RuntimeFailureKind::DependencyUnavailable
            }
            ControlSemanticError::Cancelled(_) => RuntimeFailureKind::Cancelled,
            ControlSemanticError::DeadlineExceeded(_) => RuntimeFailureKind::DeadlineExceeded,
            ControlSemanticError::Rejected(rejection) => match rejection.kind {
                RuntimeControlRejectionKind::UnsupportedCapability => {
                    RuntimeFailureKind::UnsupportedCapability
                }
                RuntimeControlRejectionKind::IncompatibleVersion => {
                    RuntimeFailureKind::IncompatibleVersion
                }
                RuntimeControlRejectionKind::InvalidInput => RuntimeFailureKind::InvalidInput,
                RuntimeControlRejectionKind::ResourceExhausted => {
                    RuntimeFailureKind::ResourceExhausted
                }
                RuntimeControlRejectionKind::DependencyUnavailable => {
                    RuntimeFailureKind::DependencyUnavailable
                }
                RuntimeControlRejectionKind::AuthorizationFailed => {
                    RuntimeFailureKind::AuthorizationFailed
                }
                RuntimeControlRejectionKind::Cancelled => RuntimeFailureKind::Cancelled,
                RuntimeControlRejectionKind::DeadlineExceeded => {
                    RuntimeFailureKind::DeadlineExceeded
                }
            },
        },
    }
}

/// Sealed payload conversion performed inside the authenticated authorization
/// operation.
///
/// An implementation must consume both the prepared payload and its exact
/// claim output authority into one terminal-only type. The control layer never
/// exposes a generic tuple/map operation that could separate or cross-swap
/// those values.
pub(crate) trait InvocationAuthorizationPayload: Sized {
    type Authorized;
    type Terminal;
    type Unknown;

    fn into_authorized(
        self,
        permit: InvocationSubmissionPermit,
        output: AgentExecutionOutputAuthority,
        runtime_context: ClaimBoundRuntimeContextAuthority,
        session: ClaimBoundSessionAuthority,
    ) -> Self::Authorized;

    fn into_authorization_terminal(
        self,
        output: AgentExecutionOutputAuthority,
        cause: InvocationAuthorizationTerminalCause,
    ) -> Self::Terminal;

    fn into_authorization_unknown(
        self,
        authority: InvocationAuthorizationNoAckAuthority,
        error: AgentControlError,
    ) -> Self::Unknown;
}

/// Exhaustive result of the final durable fence before native ADK execution.
///
/// The operation itself is the authority-minting boundary: raw protobuf
/// responses cannot construct any of these opaque values.
#[allow(dead_code)] // Exhausted by the next owned invocation coordinator.
pub(crate) enum InvocationAuthorizationDecision<T: InvocationAuthorizationPayload> {
    AuthorizedNow(Box<T::Authorized>),
    AlreadyAuthorized(Box<T::Terminal>),
    Rejected(Box<T::Terminal>),
    Unknown(Box<T::Unknown>),
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum DesiredExecutionState {
    Running,
    Cancelled,
    Draining,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct LeaseObservation {
    pub lease_expires_at_unix_millis: i64,
    pub desired_state: DesiredExecutionState,
}

/// Opaque, outcome-bound settlement receipt.
pub struct SettlementReceipt {
    receipt_id: String,
    outcome: ExecutionOutcomeV1,
    retirement: CommandRetirementBinding,
}

impl SettlementReceipt {
    #[must_use]
    pub fn receipt_id(&self) -> &str {
        &self.receipt_id
    }

    #[must_use]
    pub const fn outcome(&self) -> ExecutionOutcomeV1 {
        self.outcome
    }
}

struct CommandRetirementBinding {
    identity: ExecutionIdentityV1,
    stable_delivery_id: String,
    exact_signed_envelope: Box<[u8]>,
}

/// Consuming proof that one exact agent command reached a durable terminal
/// state and may cross the Redis retirement boundary.
pub enum AgentCommandRetirementAuthority {
    TerminalRedelivery(TerminalCommandAck),
    RecoveredSettlement(RecoveredSettlement),
    PreparedSettlement(SettlementReceipt),
}

impl From<TerminalCommandAck> for AgentCommandRetirementAuthority {
    fn from(value: TerminalCommandAck) -> Self {
        Self::TerminalRedelivery(value)
    }
}

impl From<RecoveredSettlement> for AgentCommandRetirementAuthority {
    fn from(value: RecoveredSettlement) -> Self {
        Self::RecoveredSettlement(value)
    }
}

impl From<SettlementReceipt> for AgentCommandRetirementAuthority {
    fn from(value: SettlementReceipt) -> Self {
        Self::PreparedSettlement(value)
    }
}

impl AgentCommandRetirementAuthority {
    pub(crate) fn into_binding(self) -> (ExecutionIdentityV1, String, Box<[u8]>) {
        let binding = match self {
            Self::TerminalRedelivery(value) => value.retirement,
            Self::RecoveredSettlement(value) => value.retirement,
            Self::PreparedSettlement(value) => value.retirement,
        };
        (
            binding.identity,
            binding.stable_delivery_id,
            binding.exact_signed_envelope,
        )
    }
}

/// Authenticated semantic control operations for one worker identity.
///
/// Raw protobuf responses never mint execution or retirement authority. The
/// client owns the one-attempt transport call and consumes typestate on the two
/// durable side-effect fences.
#[derive(Clone)]
pub struct AgentControlClient<R> {
    control: ControlGrpcClient<R>,
    workload_session_id: String,
    producer_id: String,
}

impl AgentControlClient<TonicControlRpc> {
    /// Compose semantic control over a caller-verified mTLS channel.
    ///
    /// # Errors
    ///
    /// Returns a stable configuration error for invalid identity or deadline.
    pub fn from_channel(
        channel: Channel,
        config: ControlGrpcConfig,
    ) -> Result<Self, AgentControlError> {
        Self::new(TonicControlRpc::new(channel), config)
    }
}

impl<R> AgentControlClient<R> {
    /// Bind one injected transport to an immutable worker identity.
    ///
    /// # Errors
    ///
    /// Returns a stable configuration error for invalid identity or deadline.
    pub fn new(rpc: R, config: ControlGrpcConfig) -> Result<Self, AgentControlError> {
        let workload_session_id = config.workload_session_id.clone();
        let producer_id = config.producer_id.clone();
        let control = ControlGrpcClient::new(rpc, config)?;
        Ok(Self {
            control,
            workload_session_id,
            producer_id,
        })
    }
}

impl<R: ControlRpc> AgentControlClient<R> {
    /// Claim and validate one exact fresh agent command.
    ///
    /// # Errors
    ///
    /// Returns a typed transport, runtime rejection, or claim-binding failure.
    pub async fn claim_agent(
        &self,
        verified: &VerifiedAgentCommand,
        now_unix_millis: i64,
    ) -> Result<AcceptedAgentClaim, AgentControlError> {
        let request =
            build_agent_claim_request(verified, &self.workload_session_id, &self.producer_id)?;
        let response = self.control.claim_command(request).await?;
        parse_accepted_agent_claim(
            verified,
            response,
            &self.workload_session_id,
            &self.producer_id,
            now_unix_millis,
        )
        .map_err(Into::into)
    }

    /// Claim one agent delivery and retain every restart disposition as a
    /// closed, authority-specific type.
    ///
    /// Unlike [`Self::claim_agent`], this is the delivery-orchestration entry
    /// point: recovery receipts are not collapsed into fresh-claim failures.
    ///
    /// # Errors
    ///
    /// Returns a typed transport, runtime rejection, identity, fence, or
    /// disposition-shape failure.
    pub async fn claim_agent_delivery(
        &self,
        verified: &VerifiedAgentCommand,
        now_unix_millis: i64,
    ) -> Result<AgentClaimDecision, AgentControlError> {
        let request =
            build_agent_claim_request(verified, &self.workload_session_id, &self.producer_id)?;
        let response = self.control.claim_command(request).await?;
        parse_agent_claim_decision(
            verified,
            response,
            &self.workload_session_id,
            &self.producer_id,
            now_unix_millis,
        )
        .map_err(Into::into)
    }

    /// Claim one direct read-only toolkit delivery through the same durable
    /// claim and recovery state machine used by native agent commands.
    ///
    /// # Errors
    ///
    /// Returns a typed transport, runtime rejection, identity, fence, or
    /// disposition-shape failure.
    pub async fn claim_toolkit_execute_read_delivery(
        &self,
        verified: &VerifiedToolkitExecuteReadCommand,
        now_unix_millis: i64,
    ) -> Result<AgentClaimDecision, AgentControlError> {
        let request = build_claim_request(verified, &self.workload_session_id, &self.producer_id)?;
        let response = self.control.claim_command(request).await?;
        parse_claim_decision(
            verified,
            response,
            &self.workload_session_id,
            &self.producer_id,
            now_unix_millis,
        )
        .map_err(Into::into)
    }

    /// Cross the restart-safe `BeginExecution` fence, consuming the fresh claim.
    ///
    /// # Errors
    ///
    /// Returns a typed transport, runtime rejection, or response-shape failure.
    pub async fn begin_agent_execution(
        &self,
        claim: AcceptedAgentClaim,
    ) -> Result<BeginAgentExecution, AgentControlError> {
        let response = self
            .control
            .begin_execution(claim.begin_execution_request())
            .await?;
        match parse_begin_execution_response(&response)? {
            BeginExecutionDecision::StartedNow => {
                Ok(BeginAgentExecution::Preparing(PreparingAgentExecution {
                    claim,
                }))
            }
            BeginExecutionDecision::AlreadyStarted => Ok(BeginAgentExecution::AlreadyStarted(
                BeginExecutionRecovery { _claim: claim },
            )),
        }
    }

    /// Cross the final invocation fence exactly once.
    ///
    /// The owned execution state prevents replaying one response to mint more
    /// permits. Only `AUTHORIZED_NOW` produces an opaque submission/output
    /// pair. Authenticated semantic failures retain terminal output authority;
    /// transport uncertainty retains neither terminal nor submission access.
    #[allow(dead_code)] // Called by the next owned invocation coordinator.
    pub(crate) async fn authorize_agent_invocation<T>(
        &self,
        candidate: InvocationAuthorizationCandidate<T>,
    ) -> InvocationAuthorizationDecision<T>
    where
        T: InvocationAuthorizationPayload,
    {
        let InvocationAuthorizationCandidate { execution, payload } = candidate;
        let request = execution.claim.authorize_invocation_request();
        let response = self.control.authorize_invocation(request).await;
        let claim = execution.claim;
        let response = match response {
            Ok(response) => response,
            Err(error) => {
                return InvocationAuthorizationDecision::Unknown(Box::new(
                    payload.into_authorization_unknown(
                        InvocationAuthorizationNoAckAuthority { _claim: claim },
                        error.into(),
                    ),
                ));
            }
        };
        match parse_authorize_invocation_response(&response) {
            Ok(AuthorizeInvocationDecision::AuthorizedNow) => {
                let runtime_context = ClaimBoundRuntimeContextAuthority::from_claim(&claim);
                let session = ClaimBoundSessionAuthority::from_claim(&claim);
                InvocationAuthorizationDecision::AuthorizedNow(Box::new(payload.into_authorized(
                    InvocationSubmissionPermit { _sealed: () },
                    AgentExecutionOutputAuthority { claim },
                    runtime_context,
                    session,
                )))
            }
            Ok(AuthorizeInvocationDecision::AlreadyAuthorized) => {
                InvocationAuthorizationDecision::AlreadyAuthorized(Box::new(
                    payload.into_authorization_terminal(
                        AgentExecutionOutputAuthority { claim },
                        InvocationAuthorizationTerminalCause::AlreadyAuthorized,
                    ),
                ))
            }
            Err(error) => InvocationAuthorizationDecision::Rejected(Box::new(
                payload.into_authorization_terminal(
                    AgentExecutionOutputAuthority { claim },
                    InvocationAuthorizationTerminalCause::Rejected(error.into()),
                ),
            )),
        }
    }

    /// Renew one unchanged accepted claim with an internally generated key.
    ///
    /// This operation deliberately does not commit the returned expiry to the
    /// lease handle. The lease supervisor must first observe desired state and
    /// validate the post-RPC safety margin, then call the crate-private commit
    /// boundary. This prevents a slow observation from making a nominally
    /// successful renewal unsafe.
    ///
    /// # Errors
    ///
    /// Returns a typed transport, runtime rejection, or lease validation error.
    pub(crate) async fn renew_lease(
        &self,
        lease: &mut ClaimLeaseHandle,
    ) -> Result<LeaseObservation, AgentControlError> {
        let request = lease.next_renewal_request()?;
        let response = self.control.renew_lease(request).await?;
        parse_renew_lease_response(&response, lease.lease_expires_at_unix_millis)
            .map_err(Into::into)
    }

    /// Observe the server-owned desired state for one unchanged accepted claim.
    ///
    /// # Errors
    ///
    /// Returns a typed transport, runtime rejection, or response-shape failure.
    pub(crate) async fn observe_lease(
        &self,
        lease: &ClaimLeaseHandle,
    ) -> Result<DesiredExecutionState, AgentControlError> {
        let response = self
            .control
            .observe_desired_state(lease.observe_request())
            .await?;
        parse_observe_desired_state_response(&response).map_err(Into::into)
    }

    /// Prepare settlement only from a consumed, durably acknowledged terminal proof.
    ///
    /// # Errors
    ///
    /// Returns a typed transport, runtime rejection, or receipt-binding failure.
    pub async fn prepare_agent_settlement(
        &self,
        terminal: DurablyAckedTerminal,
    ) -> Result<SettlementReceipt, AgentControlError> {
        let (identity, fence, proposal, stable_delivery_id, exact_signed_envelope) =
            terminal.into_settlement_parts();
        let retirement = CommandRetirementBinding {
            identity: identity.clone(),
            stable_delivery_id,
            exact_signed_envelope,
        };
        let expected_outcome =
            ExecutionOutcomeV1::try_from(proposal.requested_outcome).map_err(|_| {
                ControlSemanticError::InvalidInput("the settlement outcome is malformed")
            })?;
        if !terminal_outcome(expected_outcome) {
            return Err(
                ControlSemanticError::InvalidInput("the settlement outcome is malformed").into(),
            );
        }
        let proposal_bytes = proposal.encode_to_vec();
        let proposal_digest = digest::digest(&digest::SHA256, &proposal_bytes);
        let request = PrepareSettlementRequestV1 {
            identity: Some(identity),
            fence: Some(fence),
            idempotency_key: proposal.prepare_idempotency_key.clone(),
            proposal: Some(proposal),
            proposal_digest: Some(DigestV1 {
                algorithm: DigestAlgorithmV1::Sha256 as i32,
                value: proposal_digest.as_ref().to_vec(),
            }),
        };
        let response = self.control.prepare_settlement(request).await?;
        parse_prepare_settlement_response(response, expected_outcome, retirement)
            .map_err(Into::into)
    }

    /// Replay the exact state-owner proposal after a terminal ACK crash.
    ///
    /// The recovery value is consumed, so decoded protobufs or replayed parser
    /// calls cannot independently mint settlement authority.
    ///
    /// # Errors
    ///
    /// Returns a typed transport, rejection, or outcome-binding failure.
    pub async fn prepare_recovered_agent_settlement(
        &self,
        recovery: Box<RecoverTerminalAck>,
    ) -> Result<SettlementReceipt, AgentControlError> {
        let expected_outcome = ExecutionOutcomeV1::try_from(recovery.proposal.requested_outcome)
            .map_err(|_| {
                ControlSemanticError::InvalidInput("the settlement recovery outcome is malformed")
            })?;
        let request = PrepareSettlementRequestV1 {
            identity: Some(recovery.identity),
            fence: Some(recovery.fence),
            proposal: Some(recovery.proposal),
            proposal_digest: Some(recovery.proposal_digest),
            idempotency_key: recovery.idempotency_key,
        };
        let response = self.control.prepare_settlement(request).await?;
        parse_prepare_settlement_response(response, expected_outcome, recovery.retirement)
            .map_err(Into::into)
    }
}

#[cfg(test)]
pub(crate) fn test_accepted_agent_claim(
    verified: &VerifiedAgentCommand,
    response: ClaimCommandResponseV1,
    workload_session_id: &str,
    producer_id: &str,
    now_unix_millis: i64,
) -> Result<AcceptedAgentClaim, ControlSemanticError> {
    parse_accepted_agent_claim(
        verified,
        response,
        workload_session_id,
        producer_id,
        now_unix_millis,
    )
}

/// Bind the exact authenticated envelope to one claim request.
///
/// # Errors
///
/// Returns a bounded semantic error for an invalid transport identity.
pub fn build_agent_claim_request(
    verified: &VerifiedAgentCommand,
    workload_session_id: &str,
    producer_id: &str,
) -> Result<ClaimCommandRequestV1, ControlSemanticError> {
    build_claim_request(verified, workload_session_id, producer_id)
}

fn build_claim_request(
    verified: &impl VerifiedExecutionCommand,
    workload_session_id: &str,
    producer_id: &str,
) -> Result<ClaimCommandRequestV1, ControlSemanticError> {
    if !valid_control_identity(workload_session_id) || !valid_control_identity(producer_id) {
        return Err(ControlSemanticError::InvalidInput(
            "the control workload identity is malformed",
        ));
    }
    Ok(ClaimCommandRequestV1 {
        workload_session_id: workload_session_id.to_owned(),
        producer_id: producer_id.to_owned(),
        signed_command: Some(verified.signed().clone()),
    })
}

fn parse_agent_claim_decision(
    verified: &VerifiedAgentCommand,
    response: ClaimCommandResponseV1,
    workload_session_id: &str,
    producer_id: &str,
    now_unix_millis: i64,
) -> Result<AgentClaimDecision, ControlSemanticError> {
    parse_claim_decision(
        verified,
        response,
        workload_session_id,
        producer_id,
        now_unix_millis,
    )
}

fn parse_claim_decision(
    verified: &impl VerifiedExecutionCommand,
    response: ClaimCommandResponseV1,
    workload_session_id: &str,
    producer_id: &str,
    now_unix_millis: i64,
) -> Result<AgentClaimDecision, ControlSemanticError> {
    if now_unix_millis <= 0 {
        return Err(ControlSemanticError::InvalidInput(
            "the runtime clock is malformed",
        ));
    }
    let receipt = exclusive_claim_receipt(response)?;
    validate_claim_identity(verified, &receipt)?;
    let disposition = ClaimDispositionV1::try_from(receipt.disposition)
        .map_err(|_| ControlSemanticError::InvalidInput("the claim disposition is malformed"))?;
    validate_retirement_placement(disposition, &receipt)?;

    match disposition {
        ClaimDispositionV1::Accepted => parse_accepted_agent_claim(
            verified,
            ClaimCommandResponseV1 {
                receipt: Some(receipt),
                rejection: None,
            },
            workload_session_id,
            producer_id,
            now_unix_millis,
        )
        .map(|claim| AgentClaimDecision::Accepted(Box::new(claim))),
        ClaimDispositionV1::RecoverTerminalAck => parse_terminal_ack_recovery(
            verified,
            &receipt,
            workload_session_id,
            producer_id,
            now_unix_millis,
        )
        .map(|recovery| AgentClaimDecision::RecoverTerminalAck(Box::new(recovery))),
        ClaimDispositionV1::RecoverSettlement => parse_recovered_settlement(
            verified,
            &receipt,
            workload_session_id,
            producer_id,
            now_unix_millis,
        )
        .map(AgentClaimDecision::RecoverSettlement),
        ClaimDispositionV1::SettledAck => settled_ack_decision(
            verified,
            &receipt,
            workload_session_id,
            producer_id,
            now_unix_millis,
        ),
        ClaimDispositionV1::ObsoleteAck => obsolete_ack_decision(verified, &receipt),
        ClaimDispositionV1::ActiveLeaseNoack => parse_output_recovery(
            &receipt,
            workload_session_id,
            producer_id,
            now_unix_millis,
            AgentOutputRecoveryKind::ActiveLease,
        )
        .map(AgentClaimDecision::ActiveLeaseNoAck),
        ClaimDispositionV1::RetryLaterNoack => {
            validate_no_worker_authority(&receipt)?;
            desired_state(receipt.desired_state)?;
            Ok(AgentClaimDecision::RetryLaterNoAck(RetryLaterNoAck {
                _sealed: (),
            }))
        }
        ClaimDispositionV1::RetiredAck => retired_ack_decision(verified, &receipt),
        ClaimDispositionV1::RecoverRunningNoack => parse_output_recovery(
            &receipt,
            workload_session_id,
            producer_id,
            now_unix_millis,
            AgentOutputRecoveryKind::Running,
        )
        .map(AgentClaimDecision::RecoverRunningNoAck),
        ClaimDispositionV1::RecoverAmbiguousInvocationNoack => parse_output_recovery(
            &receipt,
            workload_session_id,
            producer_id,
            now_unix_millis,
            AgentOutputRecoveryKind::AmbiguousInvocation,
        )
        .map(AgentClaimDecision::RecoverAmbiguousInvocationNoAck),
        ClaimDispositionV1::Unspecified => Err(ControlSemanticError::InvalidInput(
            "the claim disposition is malformed",
        )),
    }
}

fn settled_ack_decision(
    verified: &impl VerifiedExecutionCommand,
    receipt: &ClaimReceiptV1,
    workload_session_id: &str,
    producer_id: &str,
    now_unix_millis: i64,
) -> Result<AgentClaimDecision, ControlSemanticError> {
    validate_recovery_claim(
        receipt,
        workload_session_id,
        producer_id,
        now_unix_millis,
        false,
    )?;
    Ok(AgentClaimDecision::SettledAck(terminal_command_ack(
        verified,
        TerminalRedeliveryKind::Settled,
    )))
}

fn obsolete_ack_decision(
    verified: &impl VerifiedExecutionCommand,
    receipt: &ClaimReceiptV1,
) -> Result<AgentClaimDecision, ControlSemanticError> {
    validate_no_worker_authority(receipt)?;
    if receipt.desired_state != DesiredExecutionStateV1::Cancelled as i32 {
        return Err(ControlSemanticError::InvalidInput(
            "the obsolete claim desired state is malformed",
        ));
    }
    Ok(AgentClaimDecision::ObsoleteAck(terminal_command_ack(
        verified,
        TerminalRedeliveryKind::Obsolete,
    )))
}

fn retired_ack_decision(
    verified: &impl VerifiedExecutionCommand,
    receipt: &ClaimReceiptV1,
) -> Result<AgentClaimDecision, ControlSemanticError> {
    validate_no_worker_authority_except_retirement(receipt)?;
    desired_state(receipt.desired_state)?;
    validate_deadline_retirement(receipt.retirement.as_ref())?;
    Ok(AgentClaimDecision::RetiredAck(terminal_command_ack(
        verified,
        TerminalRedeliveryKind::Retired,
    )))
}

fn validate_retirement_placement(
    disposition: ClaimDispositionV1,
    receipt: &ClaimReceiptV1,
) -> Result<(), ControlSemanticError> {
    if disposition != ClaimDispositionV1::RetiredAck && receipt.retirement.is_some() {
        return Err(ControlSemanticError::InvalidInput(
            "the claim disposition contains unexpected retirement material",
        ));
    }
    Ok(())
}

fn validate_claim_identity(
    verified: &impl VerifiedExecutionCommand,
    receipt: &ClaimReceiptV1,
) -> Result<(), ControlSemanticError> {
    if receipt.identity.as_ref() != Some(&identity_from_command(verified)) {
        return Err(ControlSemanticError::AuthorizationFailed(
            "the claim receipt identity does not match its command",
        ));
    }
    Ok(())
}

fn validate_no_worker_authority(receipt: &ClaimReceiptV1) -> Result<(), ControlSemanticError> {
    if receipt.retirement.is_some() {
        return Err(ControlSemanticError::InvalidInput(
            "the no-authority claim contains retirement material",
        ));
    }
    validate_no_worker_authority_except_retirement(receipt)
}

fn validate_no_worker_authority_except_retirement(
    receipt: &ClaimReceiptV1,
) -> Result<(), ControlSemanticError> {
    if receipt.fence.is_some()
        || receipt.lease_expires_at_unix_millis != 0
        || receipt.input_bundle_ref.is_some()
        || receipt.input_bundle.is_some()
        || receipt.claim_handoff_watermark != 0
        || !receipt.claim_id.is_empty()
        || receipt.settlement_recovery.is_some()
        || receipt.claim_started_at_unix_micros != 0
    {
        return Err(ControlSemanticError::InvalidInput(
            "the no-authority claim contains worker authority or business material",
        ));
    }
    Ok(())
}

fn validate_deadline_retirement(
    retirement: Option<&RuntimeErrorV1>,
) -> Result<(), ControlSemanticError> {
    if retirement.is_none_or(|value| {
        value.code != RuntimeErrorCodeV1::DeadlineExceeded as i32
            || value.safe_message != DEADLINE_RETIREMENT_SAFE_MESSAGE
            || !value.retryable
    }) {
        return Err(ControlSemanticError::InvalidInput(
            "the retired claim detail is malformed",
        ));
    }
    Ok(())
}

fn validate_recovery_claim(
    receipt: &ClaimReceiptV1,
    workload_session_id: &str,
    producer_id: &str,
    now_unix_millis: i64,
    allow_settlement_recovery: bool,
) -> Result<RecoveryClaimBinding, ControlSemanticError> {
    if receipt.input_bundle_ref.is_some()
        || receipt.input_bundle.is_some()
        || receipt.retirement.is_some()
        || (!allow_settlement_recovery && receipt.settlement_recovery.is_some())
        || receipt.claim_handoff_watermark > i64::MAX as u64
        || receipt.claim_started_at_unix_micros != 0
    {
        return Err(ControlSemanticError::InvalidInput(
            "the recovery claim contains unexpected business material",
        ));
    }
    let fence = receipt
        .fence
        .as_ref()
        .ok_or(ControlSemanticError::AuthorizationFailed(
            "the claim fence is malformed or expired",
        ))?;
    validate_active_fence(
        fence,
        &receipt.claim_id,
        receipt.lease_expires_at_unix_millis,
        workload_session_id,
        producer_id,
        now_unix_millis,
    )?;
    Ok(RecoveryClaimBinding {
        identity: receipt
            .identity
            .clone()
            .ok_or(ControlSemanticError::AuthorizationFailed(
                "the claim receipt identity does not match its command",
            ))?,
        fence: fence.clone(),
        lease_expires_at_unix_millis: receipt.lease_expires_at_unix_millis,
        claim_id: receipt.claim_id.clone(),
        claim_handoff_watermark: receipt.claim_handoff_watermark,
        desired_state: desired_state(receipt.desired_state)?,
    })
}

fn parse_output_recovery(
    receipt: &ClaimReceiptV1,
    workload_session_id: &str,
    producer_id: &str,
    now_unix_millis: i64,
    kind: AgentOutputRecoveryKind,
) -> Result<AgentOutputRecovery, ControlSemanticError> {
    let binding = validate_recovery_claim(
        receipt,
        workload_session_id,
        producer_id,
        now_unix_millis,
        false,
    )?;
    if kind != AgentOutputRecoveryKind::ActiveLease
        && !matches!(
            binding.desired_state,
            DesiredExecutionState::Running | DesiredExecutionState::Cancelled
        )
    {
        return Err(ControlSemanticError::InvalidInput(
            "the running recovery desired state is malformed",
        ));
    }
    Ok(AgentOutputRecovery { kind, binding })
}

fn parse_terminal_ack_recovery(
    verified: &impl VerifiedExecutionCommand,
    receipt: &ClaimReceiptV1,
    workload_session_id: &str,
    producer_id: &str,
    now_unix_millis: i64,
) -> Result<RecoverTerminalAck, ControlSemanticError> {
    let binding = validate_recovery_claim(
        receipt,
        workload_session_id,
        producer_id,
        now_unix_millis,
        true,
    )?;
    let recovery =
        receipt
            .settlement_recovery
            .as_ref()
            .ok_or(ControlSemanticError::InvalidInput(
                "the terminal ACK recovery receipt is missing",
            ))?;
    let (proposal, proposal_digest, idempotency_key) =
        validate_terminal_recovery(verified, recovery)?;
    Ok(RecoverTerminalAck {
        identity: binding.identity,
        fence: binding.fence,
        proposal,
        proposal_digest,
        idempotency_key,
        retirement: command_retirement_binding(verified),
    })
}

fn validate_terminal_recovery(
    verified: &impl VerifiedExecutionCommand,
    recovery: &SettlementRecoveryV1,
) -> Result<(SettlementProposalV1, DigestV1, String), ControlSemanticError> {
    if !recovery.settlement_receipt_id.is_empty()
        || recovery.outcome != ExecutionOutcomeV1::Unspecified as i32
        || !bounded_text(&recovery.idempotency_key, MAX_CONTROL_IDENTITY_BYTES)
    {
        return Err(ControlSemanticError::InvalidInput(
            "the terminal ACK recovery receipt is malformed",
        ));
    }
    let proposal = recovery
        .proposal
        .as_ref()
        .ok_or(ControlSemanticError::InvalidInput(
            "the terminal ACK recovery receipt is malformed",
        ))?;
    let proposal_digest =
        recovery
            .proposal_digest
            .as_ref()
            .ok_or(ControlSemanticError::InvalidInput(
                "the terminal ACK recovery receipt is malformed",
            ))?;
    let outcome = ExecutionOutcomeV1::try_from(proposal.requested_outcome).map_err(|_| {
        ControlSemanticError::InvalidInput("the terminal ACK recovery proposal is malformed")
    })?;
    let command = verified.command();
    if !terminal_outcome(outcome)
        || proposal.terminal_sequence == 0
        || proposal.terminal_sequence > i64::MAX as u64
        || proposal.proposal_id != format!("{}:settlement", command.command_id)
        || proposal.terminal_logical_output_id != terminal_logical_output_id(command)
        || proposal.terminal_event_id
            != format!("{}:{}", command.command_id, proposal.terminal_sequence)
        || proposal.prepare_idempotency_key != format!("{}:prepare-settlement", command.command_id)
        || recovery.idempotency_key != proposal.prepare_idempotency_key
        || !valid_nonzero_sha256(proposal.terminal_payload_digest.as_ref())
        || !valid_nonzero_sha256(Some(proposal_digest))
    {
        return Err(ControlSemanticError::InvalidInput(
            "the terminal ACK recovery proposal is malformed",
        ));
    }
    let calculated = digest::digest(&digest::SHA256, &proposal.encode_to_vec());
    if calculated
        .as_ref()
        .ct_eq(&proposal_digest.value)
        .unwrap_u8()
        != 1
    {
        return Err(ControlSemanticError::AuthorizationFailed(
            "the terminal ACK recovery proposal digest is invalid",
        ));
    }
    Ok((
        proposal.clone(),
        proposal_digest.clone(),
        recovery.idempotency_key.clone(),
    ))
}

fn parse_recovered_settlement(
    verified: &impl VerifiedExecutionCommand,
    receipt: &ClaimReceiptV1,
    workload_session_id: &str,
    producer_id: &str,
    now_unix_millis: i64,
) -> Result<RecoveredSettlement, ControlSemanticError> {
    validate_recovery_claim(
        receipt,
        workload_session_id,
        producer_id,
        now_unix_millis,
        true,
    )?;
    let recovery =
        receipt
            .settlement_recovery
            .as_ref()
            .ok_or(ControlSemanticError::InvalidInput(
                "the prepared settlement recovery receipt is missing",
            ))?;
    let outcome = ExecutionOutcomeV1::try_from(recovery.outcome).map_err(|_| {
        ControlSemanticError::InvalidInput("the prepared settlement recovery receipt is malformed")
    })?;
    if recovery.proposal.is_some()
        || recovery.proposal_digest.is_some()
        || !recovery.idempotency_key.is_empty()
        || !bounded_text(&recovery.settlement_receipt_id, MAX_CONTROL_IDENTITY_BYTES)
        || !terminal_outcome(outcome)
    {
        return Err(ControlSemanticError::InvalidInput(
            "the prepared settlement recovery receipt is malformed",
        ));
    }
    Ok(RecoveredSettlement {
        receipt_id: recovery.settlement_receipt_id.clone(),
        outcome,
        retirement: command_retirement_binding(verified),
    })
}

/// Validate and seal a fresh `ACCEPTED` agent claim.
///
/// # Errors
///
/// Returns a typed runtime rejection or a bounded local semantic error. Other
/// claim dispositions belong to the recovery parser and are never coerced into
/// fresh execution authority.
fn parse_accepted_agent_claim(
    verified: &impl VerifiedExecutionCommand,
    response: ClaimCommandResponseV1,
    workload_session_id: &str,
    producer_id: &str,
    now_unix_millis: i64,
) -> Result<AcceptedAgentClaim, ControlSemanticError> {
    if now_unix_millis <= 0 {
        return Err(ControlSemanticError::InvalidInput(
            "the runtime clock is malformed",
        ));
    }
    let receipt = exclusive_claim_receipt(response)?;
    if receipt.disposition != ClaimDispositionV1::Accepted as i32 {
        return Err(ControlSemanticError::InvalidInput(
            "the claim disposition does not grant fresh execution authority",
        ));
    }
    if receipt.settlement_recovery.is_some() || receipt.retirement.is_some() {
        return Err(ControlSemanticError::InvalidInput(
            "the accepted claim contains recovery material",
        ));
    }
    let expected_identity = identity_from_command(verified);
    let identity = receipt.identity.ok_or(ControlSemanticError::InvalidInput(
        "the accepted claim identity is missing",
    ))?;
    if identity != expected_identity || identity.generation > i64::MAX as u64 {
        return Err(ControlSemanticError::AuthorizationFailed(
            "the claim receipt identity does not match its command",
        ));
    }
    let fence = receipt
        .fence
        .ok_or(ControlSemanticError::AuthorizationFailed(
            "the claim fence is malformed or expired",
        ))?;
    validate_active_fence(
        &fence,
        &receipt.claim_id,
        receipt.lease_expires_at_unix_millis,
        workload_session_id,
        producer_id,
        now_unix_millis,
    )?;
    validate_fresh_claim_started_at(
        receipt.claim_started_at_unix_micros,
        receipt.lease_expires_at_unix_millis,
    )?;
    if receipt.claim_handoff_watermark > i64::MAX as u64 {
        return Err(ControlSemanticError::ResourceExhausted(
            "the claim handoff watermark exceeds the durable counter limit",
        ));
    }
    if receipt.desired_state != DesiredExecutionStateV1::Running as i32 {
        return Err(ControlSemanticError::AuthorizationFailed(
            "the accepted claim desired state is malformed",
        ));
    }

    let expected_reference =
        verified
            .command()
            .input_bundle_ref
            .as_ref()
            .ok_or(ControlSemanticError::InvalidInput(
                "the command input reference is missing",
            ))?;
    let input_bundle_ref =
        receipt
            .input_bundle_ref
            .ok_or(ControlSemanticError::AuthorizationFailed(
                "the accepted claim changed the immutable input reference",
            ))?;
    if &input_bundle_ref != expected_reference {
        return Err(ControlSemanticError::AuthorizationFailed(
            "the accepted claim changed the immutable input reference",
        ));
    }
    let input_bundle = receipt
        .input_bundle
        .ok_or(ControlSemanticError::InvalidInput(
            "the accepted claim is missing its input manifest",
        ))?;
    validate_manifest_binding(&input_bundle_ref, &input_bundle)?;
    let request_entry = validate_execution_request_entry(verified.command(), &input_bundle)?;

    Ok(AcceptedAgentClaim {
        identity,
        command_binding: verified_command_binding(verified),
        fence,
        lease_expires_at_unix_millis: receipt.lease_expires_at_unix_millis,
        claim_started_at_unix_micros: receipt.claim_started_at_unix_micros,
        claim_id: receipt.claim_id,
        claim_handoff_watermark: receipt.claim_handoff_watermark,
        input_bundle_ref,
        input_bundle,
        request_entry,
    })
}

/// Interpret the durable begin-execution fence.
///
/// # Errors
///
/// Returns a typed runtime rejection or rejects ambiguous/malformed responses.
fn parse_begin_execution_response(
    response: &BeginExecutionResponseV1,
) -> Result<BeginExecutionDecision, ControlSemanticError> {
    if let Some(rejection) = response.rejection.as_ref() {
        if response.disposition != BeginExecutionDispositionV1::Unspecified as i32 {
            return Err(ControlSemanticError::InvalidInput(
                "the begin execution response is ambiguous",
            ));
        }
        return Err(runtime_rejection(rejection));
    }
    match BeginExecutionDispositionV1::try_from(response.disposition).ok() {
        Some(BeginExecutionDispositionV1::StartedNow) => Ok(BeginExecutionDecision::StartedNow),
        Some(BeginExecutionDispositionV1::AlreadyStarted) => {
            Ok(BeginExecutionDecision::AlreadyStarted)
        }
        _ => Err(ControlSemanticError::InvalidInput(
            "the begin execution disposition is malformed",
        )),
    }
}

/// Descriptive response value before the authenticated operation mints worker
/// authority.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
#[allow(dead_code)] // Parsed by the next owned invocation coordinator.
enum AuthorizeInvocationDecision {
    AuthorizedNow,
    AlreadyAuthorized,
}

/// Interpret the last durable fence before native ADK submission.
///
/// # Errors
///
/// Returns a typed runtime rejection or rejects ambiguous/malformed responses.
#[allow(dead_code)] // Parsed by the next owned invocation coordinator.
fn parse_authorize_invocation_response(
    response: &AuthorizeInvocationResponseV1,
) -> Result<AuthorizeInvocationDecision, ControlSemanticError> {
    if let Some(rejection) = response.rejection.as_ref() {
        if response.disposition != AuthorizeInvocationDispositionV1::Unspecified as i32 {
            return Err(ControlSemanticError::InvalidInput(
                "the invocation authorization response is ambiguous",
            ));
        }
        return Err(runtime_rejection(rejection));
    }
    match AuthorizeInvocationDispositionV1::try_from(response.disposition).ok() {
        Some(AuthorizeInvocationDispositionV1::AuthorizedNow) => {
            Ok(AuthorizeInvocationDecision::AuthorizedNow)
        }
        Some(AuthorizeInvocationDispositionV1::AlreadyAuthorized) => {
            Ok(AuthorizeInvocationDecision::AlreadyAuthorized)
        }
        _ => Err(ControlSemanticError::InvalidInput(
            "the invocation authorization disposition is malformed",
        )),
    }
}

/// Validate a renewed lease against the previous monotonic expiry.
///
/// # Errors
///
/// Returns a typed rejection, authorization failure for a shortened lease, or
/// invalid input for a malformed desired state. The delivery-level supervisor
/// owns the later clock and polling-margin validation.
fn parse_renew_lease_response(
    response: &RenewLeaseResponseV1,
    previous_expiry_unix_millis: i64,
) -> Result<LeaseObservation, ControlSemanticError> {
    if let Some(rejection) = response.rejection.as_ref() {
        if response.lease_expires_at_unix_millis != 0
            || response.desired_state != DesiredExecutionStateV1::Unspecified as i32
        {
            return Err(ControlSemanticError::InvalidInput(
                "the lease renewal response is ambiguous",
            ));
        }
        return Err(runtime_rejection(rejection));
    }
    if previous_expiry_unix_millis <= 0 {
        return Err(ControlSemanticError::InvalidInput(
            "the lease validation boundary is malformed",
        ));
    }
    if response.lease_expires_at_unix_millis < previous_expiry_unix_millis {
        return Err(ControlSemanticError::AuthorizationFailed(
            "the renewed claim lease moved backwards",
        ));
    }
    Ok(LeaseObservation {
        lease_expires_at_unix_millis: response.lease_expires_at_unix_millis,
        desired_state: desired_state(response.desired_state)?,
    })
}

/// Validate one server-owned desired-state observation.
///
/// # Errors
///
/// Returns a typed rejection or invalid input for an unknown state.
fn parse_observe_desired_state_response(
    response: &ObserveDesiredStateResponseV1,
) -> Result<DesiredExecutionState, ControlSemanticError> {
    if let Some(rejection) = response.rejection.as_ref() {
        if response.desired_state != DesiredExecutionStateV1::Unspecified as i32 {
            return Err(ControlSemanticError::InvalidInput(
                "the desired-state response is ambiguous",
            ));
        }
        return Err(runtime_rejection(rejection));
    }
    desired_state(response.desired_state)
}

/// Validate one outcome-bound settlement receipt.
///
/// # Errors
///
/// Returns a typed rejection or invalid input for an ambiguous, unbounded, or
/// outcome-mismatched receipt.
fn parse_prepare_settlement_response(
    response: PrepareSettlementResponseV1,
    expected_outcome: ExecutionOutcomeV1,
    retirement: CommandRetirementBinding,
) -> Result<SettlementReceipt, ControlSemanticError> {
    if let Some(rejection) = response.rejection.as_ref() {
        if !response.settlement_receipt_id.is_empty()
            || response.outcome != ExecutionOutcomeV1::Unspecified as i32
        {
            return Err(ControlSemanticError::InvalidInput(
                "the settlement response is ambiguous",
            ));
        }
        return Err(runtime_rejection(rejection));
    }
    if !bounded_text(&response.settlement_receipt_id, MAX_CONTROL_IDENTITY_BYTES)
        || response.outcome != expected_outcome as i32
        || !terminal_outcome(expected_outcome)
    {
        return Err(ControlSemanticError::InvalidInput(
            "the settlement receipt is malformed",
        ));
    }
    Ok(SettlementReceipt {
        receipt_id: response.settlement_receipt_id,
        outcome: expected_outcome,
        retirement,
    })
}

fn exclusive_claim_receipt(
    response: ClaimCommandResponseV1,
) -> Result<super::elitea::runtime::v1::ClaimReceiptV1, ControlSemanticError> {
    match (response.receipt, response.rejection) {
        (Some(_), Some(_)) => Err(ControlSemanticError::InvalidInput(
            "the claim response is ambiguous",
        )),
        (None, Some(rejection)) => Err(runtime_rejection(&rejection)),
        (Some(receipt), None) => Ok(receipt),
        (None, None) => Err(ControlSemanticError::InvalidInput(
            "the claim response is missing its receipt",
        )),
    }
}

fn validate_active_fence(
    fence: &ExecutionFenceV1,
    claim_id: &str,
    lease_expires_at_unix_millis: i64,
    workload_session_id: &str,
    producer_id: &str,
    now_unix_millis: i64,
) -> Result<(), ControlSemanticError> {
    if fence.workload_session_id != workload_session_id
        || fence.producer_id != producer_id
        || fence.claim_attempt == 0
        || fence.claim_attempt > i64::MAX as u64
        || fence.lease_epoch == 0
        || fence.lease_epoch > i64::MAX as u64
        || fence.fence_token.len() != 32
        || fence.fence_token.iter().all(|byte| *byte == 0)
        || !bounded_text(claim_id, MAX_CONTROL_IDENTITY_BYTES)
        || lease_expires_at_unix_millis <= now_unix_millis
    {
        return Err(ControlSemanticError::AuthorizationFailed(
            "the claim fence is malformed or expired",
        ));
    }
    Ok(())
}

fn validate_fresh_claim_started_at(
    claim_started_at_unix_micros: i64,
    lease_expires_at_unix_millis: i64,
) -> Result<(), ControlSemanticError> {
    if claim_started_at_unix_micros <= 0
        || lease_expires_at_unix_millis.checked_mul(1_000).is_none_or(
            |lease_expires_at_unix_micros| {
                claim_started_at_unix_micros >= lease_expires_at_unix_micros
            },
        )
    {
        return Err(ControlSemanticError::AuthorizationFailed(
            "the accepted claim creation time is malformed",
        ));
    }
    Ok(())
}

fn validate_manifest_binding(
    reference: &ExecutionInputBundleReferenceV1,
    manifest: &ExecutionInputBundleV1,
) -> Result<(), ControlSemanticError> {
    let encoded = manifest.encode_to_vec();
    let expected = require_sha256(reference.digest.as_ref())?;
    let calculated = digest::digest(&digest::SHA256, &encoded);
    if u64::try_from(encoded.len()).ok() != Some(reference.byte_length)
        || calculated.as_ref().ct_eq(expected).unwrap_u8() != 1
    {
        return Err(ControlSemanticError::AuthorizationFailed(
            "the accepted claim input manifest does not match its command",
        ));
    }
    if manifest.input_bundle_id != reference.input_bundle_id
        || manifest.immutable_version != reference.immutable_version
        || !valid_identifier(&manifest.input_bundle_id)
        || !valid_version(&manifest.immutable_version)
        || manifest.entries.len() != 1
    {
        return Err(ControlSemanticError::InvalidInput(
            "the accepted claim input manifest is malformed",
        ));
    }
    Ok(())
}

fn validate_execution_request_entry(
    command: &super::elitea::runtime::v1::WorkerCommandV1,
    manifest: &ExecutionInputBundleV1,
) -> Result<ExecutionInputEntryV1, ControlSemanticError> {
    let (request_entry_id, semantic_role, media_type, max_bytes) =
        match command.capability_command.as_ref() {
            Some(worker_command_v1::CapabilityCommand::AgentExecution(agent)) => (
                agent.request_entry_id.as_str(),
                AGENT_EXECUTION_REQUEST_ROLE,
                AGENT_INPUT_MEDIA_TYPE,
                MAX_AGENT_INPUT_BYTES,
            ),
            Some(worker_command_v1::CapabilityCommand::ToolkitExecuteRead(toolkit)) => (
                toolkit.request_entry_id.as_str(),
                TOOLKIT_EXECUTE_READ_REQUEST_ROLE,
                TOOLKIT_EXECUTE_READ_INPUT_MEDIA_TYPE,
                MAX_TOOLKIT_EXECUTE_READ_INPUT_BYTES,
            ),
            _ => {
                return Err(ControlSemanticError::UnsupportedCapability(
                    "the worker command capability is not supported",
                ));
            }
        };
    let entry = manifest
        .entries
        .first()
        .ok_or(ControlSemanticError::InvalidInput(
            "the selected agent request is absent or ambiguous",
        ))?;
    if entry.entry_id != request_entry_id
        || !valid_identifier(&entry.entry_id)
        || !valid_version(&entry.immutable_version)
        || entry.semantic_role != semantic_role
    {
        return Err(ControlSemanticError::InvalidInput(
            "the selected agent request is malformed",
        ));
    }
    let content = entry
        .content
        .as_ref()
        .ok_or(ControlSemanticError::InvalidInput(
            "the selected agent request is malformed",
        ))?;
    validate_execution_content(entry, content, media_type, max_bytes)?;
    Ok(entry.clone())
}

fn validate_execution_content(
    entry: &ExecutionInputEntryV1,
    content: &ScopedContentReferenceV1,
    media_type: &str,
    max_bytes: u64,
) -> Result<(), ControlSemanticError> {
    if !valid_identifier(&content.content_id)
        || !valid_version(&content.immutable_version)
        || entry.immutable_version != content.immutable_version
        || content.media_type != media_type
        || content.byte_length == 0
        || content.byte_length > max_bytes
        || !valid_identifier(&content.classification)
        || content.required_grant_audience != INPUT_GRANT_AUDIENCE
        || require_sha256(content.digest.as_ref()).is_err()
        || content
            .digest
            .as_ref()
            .is_some_and(|value| value.value.iter().all(|byte| *byte == 0))
    {
        return Err(ControlSemanticError::InvalidInput(
            "the selected agent request is malformed",
        ));
    }
    Ok(())
}

fn identity_from_command(verified: &impl VerifiedExecutionCommand) -> ExecutionIdentityV1 {
    let command = verified.command();
    ExecutionIdentityV1 {
        tenant_id: command.tenant_id.clone(),
        resource_project_id: command.resource_project_id.clone(),
        projection_project_id: command.projection_project_id.clone(),
        command_id: command.command_id.clone(),
        execution_id: command.execution_id.clone(),
        generation: command.generation,
    }
}

fn command_retirement_binding(
    verified: &impl VerifiedExecutionCommand,
) -> CommandRetirementBinding {
    CommandRetirementBinding {
        identity: identity_from_command(verified),
        stable_delivery_id: verified.command().idempotency_key.clone(),
        exact_signed_envelope: verified.exact_signed_envelope().into(),
    }
}

fn terminal_command_ack(
    verified: &impl VerifiedExecutionCommand,
    kind: TerminalRedeliveryKind,
) -> TerminalCommandAck {
    TerminalCommandAck {
        kind,
        retirement: command_retirement_binding(verified),
    }
}

fn terminal_logical_output_id(command: &super::elitea::runtime::v1::WorkerCommandV1) -> String {
    match command.capability_command.as_ref() {
        Some(worker_command_v1::CapabilityCommand::ToolkitExecuteRead(_)) => {
            format!("toolkit-execute-read:{}", command.execution_id)
        }
        _ => format!("agent-execution:{}", command.execution_id),
    }
}

fn require_sha256(digest: Option<&DigestV1>) -> Result<&[u8], ControlSemanticError> {
    let value = digest.ok_or(ControlSemanticError::InvalidInput(
        "the immutable content digest is malformed",
    ))?;
    if value.algorithm != DigestAlgorithmV1::Sha256 as i32 || value.value.len() != 32 {
        return Err(ControlSemanticError::InvalidInput(
            "the immutable content digest is malformed",
        ));
    }
    Ok(&value.value)
}

fn valid_nonzero_sha256(value: Option<&DigestV1>) -> bool {
    value.is_some_and(|value| {
        value.algorithm == DigestAlgorithmV1::Sha256 as i32
            && value.value.len() == 32
            && value.value.iter().any(|byte| *byte != 0)
    })
}

fn desired_state(value: i32) -> Result<DesiredExecutionState, ControlSemanticError> {
    match DesiredExecutionStateV1::try_from(value).ok() {
        Some(DesiredExecutionStateV1::Running) => Ok(DesiredExecutionState::Running),
        Some(DesiredExecutionStateV1::Cancelled) => Ok(DesiredExecutionState::Cancelled),
        Some(DesiredExecutionStateV1::Draining) => Ok(DesiredExecutionState::Draining),
        _ => Err(ControlSemanticError::InvalidInput(
            "the execution desired state is malformed",
        )),
    }
}

fn runtime_rejection(error: &RuntimeErrorV1) -> ControlSemanticError {
    let kind = match RuntimeErrorCodeV1::try_from(error.code).ok() {
        Some(RuntimeErrorCodeV1::UnsupportedCapability) => {
            RuntimeControlRejectionKind::UnsupportedCapability
        }
        Some(RuntimeErrorCodeV1::IncompatibleVersion) => {
            RuntimeControlRejectionKind::IncompatibleVersion
        }
        Some(RuntimeErrorCodeV1::InvalidInput | RuntimeErrorCodeV1::ProtocolViolation) => {
            RuntimeControlRejectionKind::InvalidInput
        }
        Some(RuntimeErrorCodeV1::ResourceExhausted) => {
            RuntimeControlRejectionKind::ResourceExhausted
        }
        Some(
            RuntimeErrorCodeV1::AuthenticationFailed
            | RuntimeErrorCodeV1::AuthorizationFailed
            | RuntimeErrorCodeV1::StaleFence,
        ) => RuntimeControlRejectionKind::AuthorizationFailed,
        Some(RuntimeErrorCodeV1::Cancelled) => RuntimeControlRejectionKind::Cancelled,
        Some(RuntimeErrorCodeV1::DeadlineExceeded) => RuntimeControlRejectionKind::DeadlineExceeded,
        Some(RuntimeErrorCodeV1::DependencyUnavailable | RuntimeErrorCodeV1::Internal) => {
            RuntimeControlRejectionKind::DependencyUnavailable
        }
        Some(RuntimeErrorCodeV1::Unspecified) | None => {
            return ControlSemanticError::InvalidInput(
                "the runtime control response contains an unknown error",
            );
        }
    };
    ControlSemanticError::Rejected(RuntimeControlRejection {
        kind,
        retryable: error.retryable,
    })
}

fn terminal_outcome(outcome: ExecutionOutcomeV1) -> bool {
    matches!(
        outcome,
        ExecutionOutcomeV1::Succeeded
            | ExecutionOutcomeV1::Failed
            | ExecutionOutcomeV1::Cancelled
            | ExecutionOutcomeV1::OutcomeUnknown
    )
}

fn valid_control_identity(value: &str) -> bool {
    bounded_text(value, MAX_CONTROL_IDENTITY_BYTES)
        && value.is_ascii()
        && !value
            .bytes()
            .any(|byte| matches!(byte, b'\0' | b'\r' | b'\n'))
}

fn bounded_text(value: &str, limit: usize) -> bool {
    !value.is_empty() && value.len() <= limit
}

fn valid_identifier(value: &str) -> bool {
    bounded_text(value, MAX_MANIFEST_TEXT_BYTES)
        && value.bytes().enumerate().all(|(index, byte)| {
            byte.is_ascii_alphanumeric() || (index > 0 && matches!(byte, b'.' | b'_' | b':' | b'-'))
        })
}

fn valid_version(value: &str) -> bool {
    bounded_text(value, MAX_MANIFEST_TEXT_BYTES)
        && value.bytes().enumerate().all(|(index, byte)| {
            byte.is_ascii_alphanumeric()
                || (index > 0 && matches!(byte, b'.' | b'_' | b':' | b'+' | b'@' | b'/' | b'-'))
        })
}

fn hex_lower(value: &[u8]) -> String {
    const HEX: &[u8; 16] = b"0123456789abcdef";
    let mut output = String::with_capacity(value.len() * 2);
    for byte in value {
        output.push(char::from(HEX[usize::from(byte >> 4)]));
        output.push(char::from(HEX[usize::from(byte & 0x0f)]));
    }
    output
}

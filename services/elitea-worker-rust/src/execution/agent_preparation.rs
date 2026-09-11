//! Shared application/ad-hoc preparation before invocation authorization.
//!
//! This stage consumes a fresh delivery whose encrypted output spool was
//! already proven empty. It then owns the exact order: invocation capacity ->
//! `BeginExecution` -> supervised lease activation -> claim-bound input
//! materialization -> canonical input parsing. It stops before
//! `AuthorizeInvocation`, so a prepared value is still safe to abandon without
//! creating ambiguous ADK side effects.

use std::fmt;
use std::sync::Arc;
use std::time::Duration;

use async_trait::async_trait;
use tracing::Instrument as _;

use super::agent_invocation::AgentAuthorizedLifecycleCompletion;
use super::agent_lease::{
    ClaimLeaseActivation, ClaimLeaseError, ClaimLeaseMonitor, ClaimLeaseMonitorConfig,
    ClaimLeaseStateProbe, SystemUnixMillisClock, UnixMillisClock,
};
use super::invocation_admission::{
    InvocationAdmission, InvocationAdmissionError, InvocationReservation,
};
use super::output_delivery::{
    AgentProgressConnector, AgentProgressPublishError, AgentProgressPublishOutcome,
    AgentProgressPublisherConfig, AgentTerminalRecoveryConfig, AgentTerminalReplay,
    EmptyAgentOutput, FreshAgentProgressPublisher, FreshAgentTerminalSelection,
    PreparedAgentOutput,
};
use crate::agents::result::AgentResultBinding;
use crate::agents::runtime::{
    AssembledNativeAgentInvocation, AuthorizedNativeAssembly, NativeAgentAssembler,
    NativeAgentCompletionSelector, NativeAgentRun, NativeAgentRuntimeError,
};
use crate::agents::session::AuthorizedNativeCommandBinding;
use crate::agents::{
    AgentExecutionKind, AgentExecutionRequest, AgentInputBinding, parse_agent_execution_input,
};
use crate::protocol::ProtocolError;
use crate::protocol::command::VerifiedAgentCommand;
use crate::protocol::control::{
    AgentControlClient, AgentControlError, AgentExecutionOutputAuthority, BeginAgentExecution,
    ClaimBoundRuntimeContextAuthority, ClaimBoundSessionAuthority,
    InvocationAuthorizationCandidate, InvocationAuthorizationNoAckAuthority,
    InvocationAuthorizationPayload, InvocationAuthorizationTerminalCause,
    InvocationSubmissionPermit, LeaseMonitoredAgentExecution,
};
use crate::protocol::elitea::runtime::v1::{DigestAlgorithmV1, DigestV1};
use crate::transport::redis_commands::{
    RedisCommandDelivery, RedisCommandRetirer, RedisRetirementClient,
};
use crate::transport::{ControlRpc, InputContentClient, InputContentError, MaterializedInput};

/// Immutable preparation policy.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct AgentPreparationConfig {
    lease: ClaimLeaseMonitorConfig,
}

impl AgentPreparationConfig {
    /// Validate the preparation policy before attaching command authority.
    ///
    /// # Errors
    ///
    /// Returns the underlying lease cadence validation failure.
    pub fn new(lease_poll_interval: Duration) -> Result<Self, AgentPreparationError> {
        let lease = ClaimLeaseMonitorConfig::new(lease_poll_interval)?;
        Ok(Self { lease })
    }

    #[must_use]
    pub(crate) const fn lease_config(self) -> ClaimLeaseMonitorConfig {
        self.lease
    }
}

/// Claim-bound materialization boundary used by production and deterministic
/// coordinator component tests.
#[async_trait]
pub(crate) trait AgentInputMaterializer: Send + Sync {
    async fn materialize(
        &self,
        execution: &LeaseMonitoredAgentExecution,
    ) -> Result<MaterializedInput, InputContentError>;

    async fn materialize_entry(
        &self,
        execution: &LeaseMonitoredAgentExecution,
        entry_id: &str,
    ) -> Result<MaterializedInput, InputContentError> {
        if entry_id != execution.request_entry().entry_id {
            return Err(InputContentError::InvalidInput(
                "the input entry is not available",
            ));
        }
        self.materialize(execution).await
    }

    async fn publish_toolkit_discovery(
        &self,
        _execution: &LeaseMonitoredAgentExecution,
        _content: &[u8],
    ) -> Result<
        crate::protocol::elitea::runtime::v1::ToolkitAvailableToolsArtifactReferenceV1,
        InputContentError,
    > {
        Err(InputContentError::InvalidInput(
            "the toolkit result writer is not available",
        ))
    }
}

#[async_trait]
impl AgentInputMaterializer for InputContentClient {
    async fn materialize(
        &self,
        execution: &LeaseMonitoredAgentExecution,
    ) -> Result<MaterializedInput, InputContentError> {
        self.fetch_materialized(execution).await
    }

    async fn materialize_entry(
        &self,
        execution: &LeaseMonitoredAgentExecution,
        entry_id: &str,
    ) -> Result<MaterializedInput, InputContentError> {
        self.fetch_materialized_entry(execution, entry_id).await
    }

    async fn publish_toolkit_discovery(
        &self,
        execution: &LeaseMonitoredAgentExecution,
        content: &[u8],
    ) -> Result<
        crate::protocol::elitea::runtime::v1::ToolkitAvailableToolsArtifactReferenceV1,
        InputContentError,
    > {
        InputContentClient::publish_toolkit_discovery(self, execution, content).await
    }
}

/// Stable preparation categories for terminal/no-ACK policy and tracing.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum AgentPreparationErrorCode {
    InvalidConfiguration,
    Admission,
    BeginControl,
    Lease,
    Clock,
}

impl AgentPreparationErrorCode {
    #[must_use]
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::InvalidConfiguration => "agent_preparation.invalid_configuration",
            Self::Admission => "agent_preparation.admission",
            Self::BeginControl => "agent_preparation.begin_control",
            Self::Lease => "agent_preparation.lease",
            Self::Clock => "agent_preparation.clock",
        }
    }
}

/// Safe, source-preserving preparation failure.
#[derive(Debug)]
pub enum AgentPreparationError {
    InvalidConfiguration(&'static str),
    Admission(InvocationAdmissionError),
    BeginControl(AgentControlError),
    Lease(ClaimLeaseError),
    Clock(&'static str),
}

impl AgentPreparationError {
    #[must_use]
    pub const fn code(&self) -> AgentPreparationErrorCode {
        match self {
            Self::InvalidConfiguration(_) => AgentPreparationErrorCode::InvalidConfiguration,
            Self::Admission(_) => AgentPreparationErrorCode::Admission,
            Self::BeginControl(_) => AgentPreparationErrorCode::BeginControl,
            Self::Lease(_) => AgentPreparationErrorCode::Lease,
            Self::Clock(_) => AgentPreparationErrorCode::Clock,
        }
    }

    #[must_use]
    pub fn retryable(&self) -> bool {
        match self {
            Self::Admission(error) => error.retryable(),
            Self::Lease(error) => error.retryable(),
            Self::BeginControl(error) => error.retryable(),
            Self::InvalidConfiguration(_) | Self::Clock(_) => false,
        }
    }
}

impl fmt::Display for AgentPreparationError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::InvalidConfiguration(message) | Self::Clock(message) => {
                formatter.write_str(message)
            }
            Self::Admission(error) => {
                write!(formatter, "agent invocation admission failed: {error}")
            }
            Self::BeginControl(error) => write!(formatter, "agent begin control failed: {error}"),
            Self::Lease(error) => write!(formatter, "agent lease activation failed: {error}"),
        }
    }
}

impl std::error::Error for AgentPreparationError {
    fn source(&self) -> Option<&(dyn std::error::Error + 'static)> {
        match self {
            Self::Admission(error) => Some(error),
            Self::BeginControl(error) => Some(error),
            Self::Lease(error) => Some(error),
            Self::InvalidConfiguration(_) | Self::Clock(_) => None,
        }
    }
}

impl From<ClaimLeaseError> for AgentPreparationError {
    fn from(value: ClaimLeaseError) -> Self {
        match value {
            ClaimLeaseError::InvalidConfiguration(message) => Self::InvalidConfiguration(message),
            error => Self::Lease(error),
        }
    }
}

/// Fresh preparation outcomes before authorization or ADK submission.
pub enum AgentPreparationOutcome {
    Prepared(Box<PreparedAgentInvocation>),
    RetryLaterNoAck,
    RecoveryRequiredNoAck,
    PreInvocationTerminal(Box<PreInvocationTerminal>),
}

/// Live typed input plus every authority required by the later authorize-and-run
/// coordinator. No raw fence or delivery content is exposed.
pub struct PreparedAgentInvocation {
    #[allow(dead_code)] // Consumed by the next authorize-and-run slice.
    delivery: RedisCommandDelivery,
    #[allow(dead_code)] // Consumed by the next authorize-and-run slice.
    verified: VerifiedAgentCommand,
    request: AgentExecutionRequest,
    #[allow(dead_code)] // Consumed by the next output-coordination slice.
    output_spool: PreparedAgentOutput,
    #[allow(dead_code)] // Consumed by the next authorize-and-run slice.
    execution: LeaseMonitoredAgentExecution,
    #[allow(dead_code)] // Consumed by the next authorize-and-run slice.
    reservation: InvocationReservation,
    #[allow(dead_code)] // Consumed by the next authorize-and-run slice.
    lease: ClaimLeaseMonitor,
}

impl PreparedAgentInvocation {
    #[must_use]
    pub const fn execution_kind(&self) -> AgentExecutionKind {
        self.request.kind
    }

    #[must_use]
    pub const fn request(&self) -> &AgentExecutionRequest {
        &self.request
    }

    /// Seal every prepared value together before the authorization RPC.
    ///
    /// The request and claim cannot be extracted independently: the control
    /// operation carries this complete payload through to the exact native ADK
    /// submission boundary.
    #[allow(dead_code)] // Called by the next owned invocation coordinator.
    pub(crate) fn into_supervised_authorization(
        self,
    ) -> (InvocationReservation, PreparedAgentAuthorization) {
        let Self {
            delivery,
            verified,
            request,
            output_spool,
            execution,
            reservation,
            lease,
        } = self;
        (
            reservation,
            PreparedAgentAuthorization {
                execution,
                payload: PreparedAgentAuthorizationPayload {
                    delivery,
                    verified,
                    request,
                    output_spool,
                    lease,
                },
            },
        )
    }

    #[cfg(test)]
    pub(crate) fn into_test_cleanup(self) -> (InvocationReservation, ClaimLeaseMonitor) {
        (self.reservation, self.lease)
    }
}

/// Unpolled authorization state transferred synchronously to the supervisor.
///
/// The claim is converted into an RPC candidate only on the owned task's first
/// poll. If worker drain wins the synchronous transfer race, this complete
/// state can instead close its lease and release the empty spool without ever
/// attempting authorization.
#[allow(dead_code)] // Consumed by the next supervised authorization coordinator.
pub(crate) struct PreparedAgentAuthorization {
    execution: LeaseMonitoredAgentExecution,
    payload: PreparedAgentAuthorizationPayload,
}

impl PreparedAgentAuthorization {
    #[allow(dead_code)] // Called only after synchronous supervisor ownership succeeds.
    pub(crate) fn into_candidate(
        self,
    ) -> InvocationAuthorizationCandidate<PreparedAgentAuthorizationPayload> {
        self.execution.bind_invocation(self.payload)
    }

    /// Close an operation rejected by the process supervisor before first poll.
    #[allow(dead_code)] // Used by the next supervisor stop-race coordinator.
    pub(crate) async fn close_no_ack(self) -> Option<ClaimLeaseError> {
        let Self {
            execution,
            payload:
                PreparedAgentAuthorizationPayload {
                    delivery,
                    verified,
                    request,
                    output_spool,
                    lease,
                },
        } = self;
        let lease_error = lease.close().await.err();
        drop(output_spool);
        drop(execution);
        drop(request);
        drop(verified);
        drop(delivery);
        lease_error
    }
}

/// Complete non-cloneable invocation state carried through authorization.
#[allow(dead_code)] // Consumed by the next native ADK invocation slice.
pub(crate) struct PreparedAgentAuthorizationPayload {
    #[allow(dead_code)] // Consumed by the native ADK invocation slice.
    delivery: RedisCommandDelivery,
    #[allow(dead_code)] // Consumed by the native ADK invocation slice.
    verified: VerifiedAgentCommand,
    request: AgentExecutionRequest,
    #[allow(dead_code)] // Consumed by the output coordinator after authorization.
    output_spool: PreparedAgentOutput,
    lease: ClaimLeaseMonitor,
}

impl PreparedAgentAuthorizationPayload {
    #[must_use]
    #[allow(dead_code)] // Used by the next native ADK invocation slice.
    pub(crate) const fn execution_kind(&self) -> AgentExecutionKind {
        self.request.kind
    }
}

impl InvocationAuthorizationPayload for PreparedAgentAuthorizationPayload {
    type Authorized = AuthorizedAgentRun;
    type Terminal = AgentFailureTerminal;
    type Unknown = AgentAuthorizationUnknown;

    fn into_authorized(
        self,
        permit: InvocationSubmissionPermit,
        output_authority: AgentExecutionOutputAuthority,
        runtime_context: ClaimBoundRuntimeContextAuthority,
        session: ClaimBoundSessionAuthority,
    ) -> Self::Authorized {
        let Self {
            delivery,
            verified,
            request,
            output_spool,
            lease,
        } = self;
        AuthorizedAgentRun {
            delivery,
            verified,
            request,
            output_authority,
            output: output_spool,
            lease,
            permit,
            runtime_context,
            session,
        }
    }

    fn into_authorization_terminal(
        self,
        output_authority: AgentExecutionOutputAuthority,
        cause: InvocationAuthorizationTerminalCause,
    ) -> Self::Terminal {
        let Self {
            delivery,
            verified,
            request: _,
            output_spool,
            lease,
        } = self;
        AgentFailureTerminal {
            delivery,
            verified,
            output_authority,
            output: output_spool,
            lease,
            proposed_failure: cause.runtime_failure_kind(),
        }
    }

    fn into_authorization_unknown(
        self,
        authority: InvocationAuthorizationNoAckAuthority,
        error: AgentControlError,
    ) -> Self::Unknown {
        let Self {
            delivery: _,
            verified: _,
            request,
            output_spool,
            lease,
        } = self;
        AgentAuthorizationUnknown {
            execution_kind: request.kind,
            authority,
            output: output_spool,
            lease,
            error,
        }
    }
}

/// Complete authorized run retained as one submission/output ownership unit.
///
/// No production method exposes its permit, request, output authority, lease,
/// or reservation independently. The native ADK coordinator must consume this
/// value at the actual supervised submission boundary.
#[allow(dead_code)] // Consumed by the next native ADK invocation slice.
pub(crate) struct AuthorizedAgentRun {
    delivery: RedisCommandDelivery,
    verified: VerifiedAgentCommand,
    request: AgentExecutionRequest,
    output_authority: AgentExecutionOutputAuthority,
    output: PreparedAgentOutput,
    lease: ClaimLeaseMonitor,
    permit: InvocationSubmissionPermit,
    runtime_context: ClaimBoundRuntimeContextAuthority,
    session: ClaimBoundSessionAuthority,
}

impl AuthorizedAgentRun {
    #[must_use]
    pub(crate) const fn execution_kind(&self) -> AgentExecutionKind {
        self.request.kind
    }

    #[must_use]
    pub(super) fn trace_tenant_id(&self) -> &str {
        &self.verified.command().tenant_id
    }

    #[must_use]
    pub(super) fn trace_resource_project_id(&self) -> &str {
        &self.verified.command().resource_project_id
    }

    #[must_use]
    pub(super) fn trace_execution_id(&self) -> &str {
        &self.verified.command().execution_id
    }

    #[must_use]
    pub(super) const fn trace_generation(&self) -> u64 {
        self.verified.command().generation
    }

    #[must_use]
    pub(super) fn trace_command_id(&self) -> &str {
        &self.verified.command().command_id
    }

    pub(super) async fn close_no_ack(
        self,
        code: &'static str,
        retryable: bool,
    ) -> AgentAuthorizedLifecycleCompletion {
        let execution_kind = self.request.kind;
        if let Err(error) = self.lease.close().await {
            tracing::warn!(
                error_code = error.code().as_str(),
                "claim lease supervision ended while closing an unstarted authorized run"
            );
        }
        AgentAuthorizedLifecycleCompletion::recovery_required(execution_kind, code, retryable)
    }

    /// Bind the exact accepted claim to the sole progress publisher before ADK
    /// submission can begin.
    ///
    /// On failure, the returned value still owns the complete unstarted run and
    /// connector so the coordinator can close it without losing output,
    /// submission, lease, or Redis authority.
    #[allow(dead_code)] // Consumed by the capability-disabled native lifecycle.
    pub(crate) fn bind_progress_publisher<C: AgentProgressConnector>(
        self,
        connector: C,
        max_output_sessions: usize,
    ) -> Result<CursorBoundAuthorizedAgentRun<C>, Box<AuthorizedAgentProgressBindError<C>>> {
        let config = match AgentProgressPublisherConfig::new(max_output_sessions) {
            Ok(config) => config,
            Err(error) => {
                return Err(Box::new(AuthorizedAgentProgressBindError {
                    run: self,
                    connector,
                    error,
                }));
            }
        };
        let Self {
            delivery,
            verified,
            request,
            output_authority,
            output,
            lease,
            permit,
            runtime_context,
            session,
        } = self;
        match output_authority.try_into_output_cursor(&verified) {
            Ok(cursor) => {
                let result_binding = AgentResultBinding::from_request(&request);
                Ok(CursorBoundAuthorizedAgentRun {
                    delivery,
                    verified,
                    request,
                    publisher: FreshAgentProgressPublisher::new(
                        cursor,
                        result_binding,
                        output,
                        connector,
                        config,
                    ),
                    lease,
                    permit: Some(permit),
                    runtime_context: Some(runtime_context),
                    session: Some(session),
                })
            }
            Err(failure) => {
                let (output_authority, error) = failure.into_parts();
                Err(Box::new(AuthorizedAgentProgressBindError {
                    run: Self {
                        delivery,
                        verified,
                        request,
                        output_authority,
                        output,
                        lease,
                        permit,
                        runtime_context,
                        session,
                    },
                    connector,
                    error: AgentProgressPublishError::InvalidFrame(error),
                }))
            }
        }
    }
}

/// Failed publisher admission with the complete unstarted run preserved.
#[allow(dead_code)] // Consumed by the capability-disabled native lifecycle.
pub(crate) struct AuthorizedAgentProgressBindError<C> {
    run: AuthorizedAgentRun,
    connector: C,
    error: AgentProgressPublishError,
}

#[allow(dead_code)] // Consumed by the capability-disabled native lifecycle.
impl<C> AuthorizedAgentProgressBindError<C> {
    #[must_use]
    pub(crate) const fn error(&self) -> &AgentProgressPublishError {
        &self.error
    }

    pub(crate) fn into_parts(self) -> (AuthorizedAgentRun, C, AgentProgressPublishError) {
        (self.run, self.connector, self.error)
    }
}

/// Authorized application/ad-hoc run with inseparable progress ownership.
///
/// No method exposes the request, submission permit, raw cursor, output
/// session, lease, or Redis delivery independently.
#[allow(dead_code)] // Consumed by the capability-disabled native lifecycle.
pub(crate) struct CursorBoundAuthorizedAgentRun<C: AgentProgressConnector> {
    delivery: RedisCommandDelivery,
    verified: VerifiedAgentCommand,
    request: AgentExecutionRequest,
    publisher: FreshAgentProgressPublisher<C>,
    lease: ClaimLeaseMonitor,
    permit: Option<InvocationSubmissionPermit>,
    runtime_context: Option<ClaimBoundRuntimeContextAuthority>,
    session: Option<ClaimBoundSessionAuthority>,
}

/// Closed outcome of the sole post-authorization assembly attempt.
pub(crate) enum AgentNativeAssemblyOutcome<C, S>
where
    C: AgentProgressConnector,
{
    Assembled(Box<AssembledAuthorizedAgentRun<C, S>>),
    Failed {
        run: Box<CursorBoundAuthorizedAgentRun<C>>,
        error: crate::agents::runtime::NativeAgentAssemblyError,
    },
    Lease {
        run: Box<CursorBoundAuthorizedAgentRun<C>>,
        error: ClaimLeaseError,
    },
}

/// Exact authorized run paired with the native assembly created from it.
///
/// Neither half can be extracted before the one-shot ADK start transition, so
/// concurrent claims cannot cross-swap provider, session, projector or result
/// selection state after credential redemption.
pub(crate) struct AssembledAuthorizedAgentRun<C, S>
where
    C: AgentProgressConnector,
{
    run: CursorBoundAuthorizedAgentRun<C>,
    assembled: AssembledNativeAgentInvocation<S>,
}

impl<C, S> AssembledAuthorizedAgentRun<C, S>
where
    C: AgentProgressConnector,
{
    pub(super) fn ensure_lease_running(&self) -> Result<(), ClaimLeaseError> {
        self.run.ensure_lease_running()
    }

    #[must_use]
    pub(super) fn deadline_unix_millis(&self) -> i64 {
        self.run.deadline_unix_millis()
    }

    pub(super) fn project_start(
        &mut self,
        occurred_at: chrono::DateTime<chrono::Utc>,
    ) -> Result<
        crate::agents::events::ProjectedAgentEventBatch,
        crate::agents::events::AgentEventProjectionError,
    > {
        self.assembled.project_start(occurred_at)
    }

    pub(super) async fn publish_progress(
        &mut self,
        event: crate::protocol::elitea::runtime::v1::NodeEventV1,
        occurred_at_unix_millis: i64,
    ) -> Result<AgentProgressPublishOutcome, AgentProgressPublishError> {
        self.run
            .publish_progress(event, occurred_at_unix_millis)
            .await
    }

    pub(super) async fn publish_result_event(
        &mut self,
        event: crate::protocol::elitea::runtime::v1::NodeEventV1,
        occurred_at_unix_millis: i64,
    ) -> Result<AgentProgressPublishOutcome, AgentProgressPublishError> {
        self.run
            .publish_result_event(event, occurred_at_unix_millis)
            .await
    }

    pub(super) fn into_run(self) -> CursorBoundAuthorizedAgentRun<C> {
        self.run
    }

    pub(super) fn start(
        self,
    ) -> Result<StartedAuthorizedAgentRun<C, S>, Box<NativeStartFailure<C>>> {
        let Self { mut run, assembled } = self;
        if run.permit.is_none() || run.runtime_context.is_some() || run.session.is_some() {
            return Err(Box::new(NativeStartFailure {
                run,
                error: NativeAgentRuntimeError::invalid_state_for_lifecycle(),
            }));
        }
        let Some(_submission_permit) = run.permit.take() else {
            return Err(Box::new(NativeStartFailure {
                run,
                error: NativeAgentRuntimeError::invalid_state_for_lifecycle(),
            }));
        };
        match assembled.start() {
            Ok((native, projector, completion)) => Ok(StartedAuthorizedAgentRun {
                run,
                native,
                projector,
                completion,
            }),
            Err(error) => Err(Box::new(NativeStartFailure { run, error })),
        }
    }
}

/// Successfully started native work still paired with its exact output owner.
pub(crate) struct StartedAuthorizedAgentRun<C, S>
where
    C: AgentProgressConnector,
{
    run: CursorBoundAuthorizedAgentRun<C>,
    native: NativeAgentRun,
    projector: Box<crate::agents::events::AgentEventProjector>,
    completion: S,
}

impl<C, S> StartedAuthorizedAgentRun<C, S>
where
    C: AgentProgressConnector,
{
    pub(super) fn into_parts(
        self,
    ) -> (
        CursorBoundAuthorizedAgentRun<C>,
        NativeAgentRun,
        Box<crate::agents::events::AgentEventProjector>,
        S,
    ) {
        (self.run, self.native, self.projector, self.completion)
    }
}

/// Failed one-shot start with the exact output/lease owner retained.
pub(crate) struct NativeStartFailure<C>
where
    C: AgentProgressConnector,
{
    run: CursorBoundAuthorizedAgentRun<C>,
    error: NativeAgentRuntimeError,
}

impl<C> NativeStartFailure<C>
where
    C: AgentProgressConnector,
{
    pub(super) fn into_parts(self) -> (CursorBoundAuthorizedAgentRun<C>, NativeAgentRuntimeError) {
        (self.run, self.error)
    }
}

#[allow(dead_code)] // Consumed by the capability-disabled native lifecycle.
impl<C: AgentProgressConnector> CursorBoundAuthorizedAgentRun<C> {
    #[must_use]
    pub(crate) const fn execution_kind(&self) -> AgentExecutionKind {
        self.request.kind
    }

    #[must_use]
    pub(crate) fn deadline_unix_millis(&self) -> i64 {
        self.verified.command().deadline_unix_millis
    }

    pub(crate) fn ensure_lease_running(&self) -> Result<(), ClaimLeaseError> {
        self.lease.ensure_running()
    }

    pub(crate) async fn check_lease_now(self) -> (Self, Result<(), ClaimLeaseError>) {
        let Self {
            delivery,
            verified,
            request,
            publisher,
            lease,
            permit,
            runtime_context,
            session,
        } = self;
        let result = lease.check_now().await;
        (
            Self {
                delivery,
                verified,
                request,
                publisher,
                lease,
                permit,
                runtime_context,
                session,
            },
            result,
        )
    }

    #[must_use]
    pub(crate) fn lease_state_probe(&self) -> ClaimLeaseStateProbe {
        self.lease.state_probe()
    }

    pub(crate) async fn assemble_native<A: NativeAgentAssembler>(
        self,
        assembler: &A,
    ) -> AgentNativeAssemblyOutcome<C, A::Completion> {
        let Self {
            delivery,
            verified,
            request,
            publisher,
            mut lease,
            permit,
            mut runtime_context,
            mut session,
        } = self;
        let (Some(runtime_context_authority), Some(session_authority)) =
            (runtime_context.take(), session.take())
        else {
            return AgentNativeAssemblyOutcome::Failed {
                run: Box::new(Self {
                    delivery,
                    verified,
                    request,
                    publisher,
                    lease,
                    permit,
                    runtime_context,
                    session,
                }),
                error: crate::agents::runtime::NativeAgentAssemblyError::new(
                    crate::agents::runtime::NativeAgentAssemblyErrorCode::InvalidConfiguration,
                    "the authorized runtime/session grants are unavailable",
                ),
            };
        };
        let command_binding = match AuthorizedNativeCommandBinding::from_verified(&verified) {
            Ok(binding) => binding,
            Err(error) => {
                return AgentNativeAssemblyOutcome::Failed {
                    run: Box::new(Self {
                        delivery,
                        verified,
                        request,
                        publisher,
                        lease,
                        permit,
                        runtime_context,
                        session,
                    }),
                    error,
                };
            }
        };
        let state_writer_lease = Arc::new(lease.state_probe());
        let assembly = AuthorizedNativeAssembly::from_authorized(
            &request,
            runtime_context_authority,
            session_authority,
            state_writer_lease,
            command_binding,
        );
        let assembly_result = lease
            .run_cancellation_safe_phase(assembler.assemble(assembly))
            .await;
        let run = Self {
            delivery,
            verified,
            request,
            publisher,
            lease,
            permit,
            runtime_context,
            session,
        };
        match assembly_result {
            Ok(Ok(assembled_invocation)) => {
                AgentNativeAssemblyOutcome::Assembled(Box::new(AssembledAuthorizedAgentRun {
                    run,
                    assembled: assembled_invocation,
                }))
            }
            Ok(Err(error)) => AgentNativeAssemblyOutcome::Failed {
                run: Box::new(run),
                error,
            },
            Err(error) => AgentNativeAssemblyOutcome::Lease {
                run: Box::new(run),
                error,
            },
        }
    }

    pub(crate) async fn select_native_completion<S: NativeAgentCompletionSelector>(
        self,
        selector: S,
    ) -> (
        Self,
        Result<
            Result<
                crate::agents::events::CompletedAgentBrowserOutput,
                crate::agents::runtime::NativeAgentAssemblyError,
            >,
            ClaimLeaseError,
        >,
    ) {
        let Self {
            delivery,
            verified,
            request,
            publisher,
            mut lease,
            permit,
            runtime_context,
            session,
        } = self;
        let selection_result = lease.run_cancellation_safe_phase(selector.select()).await;
        (
            Self {
                delivery,
                verified,
                request,
                publisher,
                lease,
                permit,
                runtime_context,
                session,
            },
            selection_result,
        )
    }

    pub(crate) async fn publish_progress(
        &mut self,
        event: crate::protocol::elitea::runtime::v1::NodeEventV1,
        occurred_at_unix_millis: i64,
    ) -> Result<AgentProgressPublishOutcome, AgentProgressPublishError> {
        self.publisher
            .publish(&self.verified, event, occurred_at_unix_millis)
            .await
    }

    pub(crate) async fn publish_full_message(
        &mut self,
        event: crate::protocol::elitea::runtime::v1::NodeEventV1,
        occurred_at_unix_millis: i64,
    ) -> Result<AgentProgressPublishOutcome, AgentProgressPublishError> {
        self.publisher
            .publish_full_message(&self.verified, event, occurred_at_unix_millis)
            .await
    }

    pub(crate) async fn publish_result_event(
        &mut self,
        event: crate::protocol::elitea::runtime::v1::NodeEventV1,
        occurred_at_unix_millis: i64,
    ) -> Result<AgentProgressPublishOutcome, AgentProgressPublishError> {
        self.publisher
            .publish_result_event(&self.verified, event, occurred_at_unix_millis)
            .await
    }

    pub(crate) async fn publish_pause_progress(
        &mut self,
        event: crate::protocol::elitea::runtime::v1::NodeEventV1,
        occurred_at_unix_millis: i64,
    ) -> Result<AgentProgressPublishOutcome, AgentProgressPublishError> {
        self.publisher
            .publish_pause_progress(&self.verified, event, occurred_at_unix_millis)
            .await
    }

    pub(crate) async fn resume_pending_progress(
        &mut self,
    ) -> Result<AgentProgressPublishOutcome, AgentProgressPublishError> {
        self.publisher.resume_pending().await
    }

    pub(super) async fn finish_terminal<R, RC>(
        self,
        selection: FreshAgentTerminalSelection,
        occurred_at_unix_millis: i64,
        control: Arc<AgentControlClient<R>>,
        retirer: Arc<RedisCommandRetirer<RC>>,
        recovery_config: AgentTerminalRecoveryConfig,
    ) -> AgentAuthorizedLifecycleCompletion
    where
        C: AgentTerminalReplay,
        R: ControlRpc + 'static,
        RC: RedisRetirementClient + 'static,
    {
        let Self {
            delivery,
            verified,
            request,
            publisher,
            lease,
            permit: _,
            runtime_context: _,
            session: _,
        } = self;
        let execution_kind = request.kind;
        let result = async {
            tracing::info!(event = "agent_terminal_output_started");
            let terminal = publisher
                .finish_terminal(
                    &verified,
                    selection,
                    occurred_at_unix_millis,
                    recovery_config,
                )
                .await
                .map_err(|error| (error.code(), error.retryable()))?;
            let sequence = terminal.frame.sequence;
            tracing::info!(event = "agent_terminal_output_acknowledged", sequence);
            tracing::info!(event = "agent_settlement_started");
            let receipt = control
                .prepare_agent_settlement(terminal.acknowledged)
                .await
                .map_err(|error| ("agent_lifecycle.settlement_failed", error.retryable()))?;
            tracing::info!(event = "agent_settlement_prepared");
            let settlement_receipt_id = receipt.receipt_id().to_owned();
            tracing::info!(event = "agent_redis_retirement_started");
            retirer
                .retire_agent_command(delivery, &verified, receipt.into())
                .await
                .map_err(|error| (error.code(), error.retryable()))?;
            tracing::info!(event = "agent_redis_retirement_completed");
            Ok::<_, (&'static str, bool)>((sequence, settlement_receipt_id))
        }
        .await;

        if let Err(error) = lease.close().await {
            tracing::warn!(
                error_code = error.code().as_str(),
                "claim lease supervision ended after authorized agent finalization"
            );
        }
        match result {
            Ok((sequence, settlement_receipt_id)) => AgentAuthorizedLifecycleCompletion::settled(
                execution_kind,
                sequence,
                settlement_receipt_id,
            ),
            Err((code, retryable)) => AgentAuthorizedLifecycleCompletion::recovery_required(
                execution_kind,
                code,
                retryable,
            ),
        }
    }

    pub(super) async fn close_no_ack(
        self,
        code: &'static str,
        retryable: bool,
    ) -> AgentAuthorizedLifecycleCompletion {
        let execution_kind = self.request.kind;
        if let Err(error) = self.lease.close().await {
            tracing::warn!(
                error_code = error.code().as_str(),
                "claim lease supervision ended while retaining an agent command for recovery"
            );
        }
        AgentAuthorizedLifecycleCompletion::recovery_required(execution_kind, code, retryable)
    }
}

#[cfg(test)]
impl AuthorizedAgentRun {
    pub(crate) fn into_test_cleanup(self) -> (AgentExecutionRequest, ClaimLeaseMonitor) {
        (self.request, self.lease)
    }
}

/// Request-free cleanup ownership for an authorization RPC with unknown effect.
///
/// The accepted claim can no longer create output or submission authority. The
/// empty spool lock remains owned until lease shutdown completes, while the
/// supervisor retains capacity and Redis remains deliberately unacknowledged.
#[allow(dead_code)] // Consumed by the next supervised authorization coordinator.
pub(crate) struct AgentAuthorizationUnknown {
    execution_kind: AgentExecutionKind,
    authority: InvocationAuthorizationNoAckAuthority,
    output: PreparedAgentOutput,
    lease: ClaimLeaseMonitor,
    error: AgentControlError,
}

impl AgentAuthorizationUnknown {
    #[must_use]
    #[allow(dead_code)] // Used by the next supervised authorization coordinator.
    pub(crate) const fn error(&self) -> &AgentControlError {
        &self.error
    }

    /// Close only local authority after an unknown authorization effect.
    ///
    /// No terminal, settlement, or Redis retirement authority is returned.
    #[allow(dead_code)] // Used by the next supervised authorization coordinator.
    pub(crate) async fn close_no_ack(self) -> AgentAuthorizationUnknownCompletion {
        let Self {
            execution_kind,
            authority,
            output,
            lease,
            error,
        } = self;
        let lease_error = lease.close().await.err();
        drop(output);
        drop(authority);
        AgentAuthorizationUnknownCompletion {
            execution_kind,
            authorization_error: error,
            lease_error,
        }
    }
}

/// Data-free disposition retained after local unknown-effect cleanup.
#[allow(dead_code)] // Returned by the next supervised authorization coordinator.
pub(crate) struct AgentAuthorizationUnknownCompletion {
    execution_kind: AgentExecutionKind,
    authorization_error: AgentControlError,
    lease_error: Option<ClaimLeaseError>,
}

#[allow(dead_code)] // Read by the next supervised authorization coordinator.
impl AgentAuthorizationUnknownCompletion {
    #[must_use]
    pub(crate) const fn execution_kind(&self) -> AgentExecutionKind {
        self.execution_kind
    }

    #[must_use]
    pub(crate) const fn authorization_error(&self) -> &AgentControlError {
        &self.authorization_error
    }

    #[must_use]
    pub(crate) const fn lease_error(&self) -> Option<&ClaimLeaseError> {
        self.lease_error.as_ref()
    }
}

/// Claim authority retained when the immediate lease boundary itself supplies
/// a canonical pre-invocation terminal cause such as durable Stop.
pub struct PreInvocationTerminal {
    #[allow(dead_code)] // Consumed by the next output-coordination slice.
    delivery: RedisCommandDelivery,
    #[allow(dead_code)] // Consumed by the next output-coordination slice.
    verified: VerifiedAgentCommand,
    #[allow(dead_code)] // Consumed by the next output-coordination slice.
    output_authority: AgentExecutionOutputAuthority,
    #[allow(dead_code)] // Consumed by the next output-coordination slice.
    output_spool: PreparedAgentOutput,
    #[allow(dead_code)] // Consumed by the next output-coordination slice.
    reservation: InvocationReservation,
    #[allow(dead_code)] // Consumed by the next output-coordination slice.
    lease: ClaimLeaseMonitor,
    cause: PreInvocationTerminalCause,
}

impl PreInvocationTerminal {
    #[must_use]
    pub const fn cause(&self) -> &PreInvocationTerminalCause {
        &self.cause
    }

    pub(super) fn into_failure_terminal(self) -> (AgentFailureTerminal, InvocationReservation) {
        (
            AgentFailureTerminal {
                delivery: self.delivery,
                verified: self.verified,
                output_authority: self.output_authority,
                output: self.output_spool,
                lease: self.lease,
                proposed_failure: self.cause.runtime_failure_kind(),
            },
            self.reservation,
        )
    }

    #[cfg(test)]
    pub(crate) fn into_test_parts(
        self,
    ) -> (
        RedisCommandDelivery,
        VerifiedAgentCommand,
        AgentExecutionOutputAuthority,
        PreparedAgentOutput,
        InvocationReservation,
        ClaimLeaseMonitor,
        PreInvocationTerminalCause,
    ) {
        (
            self.delivery,
            self.verified,
            self.output_authority,
            self.output_spool,
            self.reservation,
            self.lease,
            self.cause,
        )
    }
}

/// Sealed pre-authorization failure state consumed only by output delivery.
///
/// The type exposes neither request input nor fence material. Keeping delivery,
/// command, output and lease ownership together prevents a terminal from being
/// published or retired under another admitted invocation. Capacity remains
/// with either the pre-invocation caller or the process supervisor.
pub(crate) struct AgentFailureTerminal {
    pub(super) delivery: RedisCommandDelivery,
    pub(super) verified: VerifiedAgentCommand,
    pub(super) output_authority: AgentExecutionOutputAuthority,
    pub(super) output: PreparedAgentOutput,
    pub(super) lease: ClaimLeaseMonitor,
    pub(super) proposed_failure: crate::protocol::output::RuntimeFailureKind,
}

#[cfg(test)]
impl AgentFailureTerminal {
    pub(crate) fn into_test_cleanup(
        self,
    ) -> (
        AgentExecutionKind,
        crate::protocol::output::RuntimeFailureKind,
        ClaimLeaseMonitor,
    ) {
        (self.verified.kind(), self.proposed_failure, self.lease)
    }
}

/// Canonical terminal cause observed before the durable invocation fence.
#[derive(Debug)]
pub enum PreInvocationTerminalCause {
    Cancelled(ClaimLeaseError),
    InputContent(InputContentError),
    InputProtocol(ProtocolError),
    DeadlineExceeded,
}

impl PreInvocationTerminalCause {
    /// Stable low-cardinality category for logs and metrics.
    #[must_use]
    pub const fn code(&self) -> &'static str {
        match self {
            Self::Cancelled(_) => "agent_preparation.cancelled",
            Self::InputContent(error) => error.code(),
            Self::InputProtocol(ProtocolError::InvalidInput(_)) => "agent_input.invalid_input",
            Self::InputProtocol(ProtocolError::ResourceExhausted(_)) => {
                "agent_input.resource_exhausted"
            }
            Self::InputProtocol(ProtocolError::IncompatibleVersion(_)) => {
                "agent_input.incompatible_version"
            }
            Self::InputProtocol(ProtocolError::UnsupportedCapability(_)) => {
                "agent_input.unsupported_capability"
            }
            Self::InputProtocol(ProtocolError::AuthorizationFailed(_)) => {
                "agent_input.authorization_failed"
            }
            Self::DeadlineExceeded => "agent_preparation.deadline_exceeded",
        }
    }

    #[must_use]
    pub const fn runtime_failure_kind(&self) -> crate::protocol::output::RuntimeFailureKind {
        use crate::protocol::output::RuntimeFailureKind;
        match self {
            Self::Cancelled(_) => RuntimeFailureKind::Cancelled,
            Self::DeadlineExceeded => RuntimeFailureKind::DeadlineExceeded,
            Self::InputContent(InputContentError::InvalidInput(_))
            | Self::InputProtocol(ProtocolError::InvalidInput(_)) => {
                RuntimeFailureKind::InvalidInput
            }
            Self::InputContent(InputContentError::ResourceExhausted(_))
            | Self::InputProtocol(ProtocolError::ResourceExhausted(_)) => {
                RuntimeFailureKind::ResourceExhausted
            }
            Self::InputContent(InputContentError::AuthorizationFailed(_))
            | Self::InputProtocol(ProtocolError::AuthorizationFailed(_)) => {
                RuntimeFailureKind::AuthorizationFailed
            }
            Self::InputProtocol(ProtocolError::IncompatibleVersion(_)) => {
                RuntimeFailureKind::IncompatibleVersion
            }
            Self::InputProtocol(ProtocolError::UnsupportedCapability(_)) => {
                RuntimeFailureKind::UnsupportedCapability
            }
            Self::InputContent(
                InputContentError::DependencyUnavailable(_)
                | InputContentError::Transport(_)
                | InputContentError::Timeout(_),
            ) => RuntimeFailureKind::DependencyUnavailable,
            Self::InputContent(InputContentError::InvalidConfiguration(_)) => {
                RuntimeFailureKind::Internal
            }
        }
    }
}

impl fmt::Display for PreInvocationTerminalCause {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::Cancelled(error) => error.fmt(formatter),
            Self::InputContent(error) => {
                write!(formatter, "agent input materialization failed: {error}")
            }
            Self::InputProtocol(error) => {
                write!(formatter, "agent input validation failed: {error}")
            }
            Self::DeadlineExceeded => {
                formatter.write_str("the agent command deadline was exceeded before invocation")
            }
        }
    }
}

impl std::error::Error for PreInvocationTerminalCause {
    fn source(&self) -> Option<&(dyn std::error::Error + 'static)> {
        match self {
            Self::Cancelled(error) => Some(error),
            Self::InputContent(error) => Some(error),
            Self::InputProtocol(error) => Some(error),
            Self::DeadlineExceeded => None,
        }
    }
}

/// Prepare one routed fresh agent command through typed input, without crossing
/// the durable invocation fence.
///
/// # Errors
///
/// Returns typed admission/control/lease/input/deadline failures. Any returned
/// error owns no output or Redis retirement authority; the command remains
/// unacknowledged for recovery. Durable cancellation is instead returned as a
/// `PreInvocationTerminal` carrying its exact output authority.
pub async fn prepare_fresh_agent_invocation<R>(
    fresh: EmptyAgentOutput,
    control: Arc<AgentControlClient<R>>,
    admission: &InvocationAdmission,
    input: &InputContentClient,
    config: AgentPreparationConfig,
) -> Result<AgentPreparationOutcome, AgentPreparationError>
where
    R: ControlRpc + 'static,
{
    Box::pin(prepare_fresh_agent_invocation_with(
        fresh,
        control,
        admission,
        input,
        Arc::new(SystemUnixMillisClock),
        config,
    ))
    .await
}

pub(crate) async fn prepare_fresh_agent_invocation_with<R, K, I>(
    fresh: EmptyAgentOutput,
    control: Arc<AgentControlClient<R>>,
    admission: &InvocationAdmission,
    input: &I,
    clock: Arc<K>,
    config: AgentPreparationConfig,
) -> Result<AgentPreparationOutcome, AgentPreparationError>
where
    R: ControlRpc + 'static,
    K: UnixMillisClock,
    I: AgentInputMaterializer,
{
    let kind = fresh.execution_kind();
    let (fresh, output_spool) = fresh.into_parts();
    let (delivery, verified, claim) = fresh.into_parts();
    let command = verified.command();
    let span = tracing::info_span!(
        "agent.prepare",
        execution_kind = ?kind,
        tenant_id = %command.tenant_id,
        resource_project_id = %command.resource_project_id,
        execution_id = %command.execution_id,
        generation = command.generation,
        command_id = %command.command_id,
        stage = tracing::field::Empty,
        outcome = tracing::field::Empty,
        error_code = tracing::field::Empty,
    );
    Box::pin(
        async move {
            let reservation = match admission.reserve().await {
                Ok(reservation) => reservation,
                Err(error) if error.retryable() => {
                    tracing::Span::current().record("outcome", "retry_later_noack");
                    tracing::Span::current().record("error_code", error.code().as_str());
                    return Ok(AgentPreparationOutcome::RetryLaterNoAck);
                }
                Err(error) => return preparation_error(AgentPreparationError::Admission(error)),
            };
            tracing::Span::current().record("stage", "begin_execution");
            tracing::info!(event = "agent_preparation_begin_started");
            let preparing = match control.begin_agent_execution(claim).await {
                Ok(BeginAgentExecution::Preparing(preparing)) => {
                    tracing::info!(event = "agent_preparation_begin_accepted");
                    preparing
                }
                Ok(BeginAgentExecution::AlreadyStarted(_)) => {
                    tracing::Span::current().record("outcome", "recovery_required_noack");
                    tracing::warn!(
                        event = "agent_preparation_recovery_required",
                        result_code = "agent_preparation.already_started",
                        retryable = true,
                    );
                    return Ok(AgentPreparationOutcome::RecoveryRequiredNoAck);
                }
                Err(error) => return preparation_error(AgentPreparationError::BeginControl(error)),
            };

            let starting = preparing.start_lease_monitor();
            let mut lease = ClaimLeaseMonitor::start(
                Arc::clone(&control),
                starting,
                Arc::clone(&clock),
                config.lease,
            );
            tracing::Span::current().record("stage", "lease_activation");
            tracing::info!(event = "agent_preparation_lease_activation_started");
            let execution = match lease.activate().await {
                ClaimLeaseActivation::Active(execution) => {
                    tracing::info!(event = "agent_preparation_lease_active");
                    execution
                }
                ClaimLeaseActivation::Inactive { execution, error }
                    if is_terminal_lease(&error) =>
                {
                    tracing::Span::current().record("outcome", "pre_invocation_terminal");
                    tracing::Span::current().record("error_code", error.code().as_str());
                    return Ok(AgentPreparationOutcome::PreInvocationTerminal(Box::new(
                        PreInvocationTerminal {
                            delivery,
                            verified,
                            output_authority: execution.into_output_authority(),
                            output_spool,
                            reservation,
                            lease,
                            cause: PreInvocationTerminalCause::Cancelled(error),
                        },
                    )));
                }
                ClaimLeaseActivation::Inactive { error, .. }
                | ClaimLeaseActivation::Unavailable(error) => {
                    return preparation_error(AgentPreparationError::Lease(error));
                }
            };
            Box::pin(
                ActiveAgentPreparation {
                    kind,
                    delivery,
                    verified,
                    output_spool,
                    execution,
                    reservation,
                    lease,
                }
                .materialize(input, clock.as_ref()),
            )
            .await
        }
        .instrument(span),
    )
    .await
}

struct ActiveAgentPreparation {
    kind: AgentExecutionKind,
    delivery: RedisCommandDelivery,
    verified: VerifiedAgentCommand,
    output_spool: PreparedAgentOutput,
    execution: LeaseMonitoredAgentExecution,
    reservation: InvocationReservation,
    lease: ClaimLeaseMonitor,
}

impl ActiveAgentPreparation {
    async fn materialize<I, K>(
        mut self,
        input: &I,
        clock: &K,
    ) -> Result<AgentPreparationOutcome, AgentPreparationError>
    where
        I: AgentInputMaterializer,
        K: UnixMillisClock,
    {
        match deadline_exceeded(&self.verified, clock) {
            Ok(true) => {
                return self
                    .terminal_after_final_lease(PreInvocationTerminalCause::DeadlineExceeded)
                    .await;
            }
            Ok(false) => {}
            Err(error) => return preparation_error(error),
        }
        tracing::Span::current().record("stage", "input_materialization");
        tracing::info!(event = "agent_preparation_input_started");
        let materialized = match self
            .lease
            .run_pre_invocation(input.materialize(&self.execution))
            .await
        {
            Ok(Ok(materialized)) => {
                tracing::info!(event = "agent_preparation_input_materialized");
                materialized
            }
            Ok(Err(error)) => {
                tracing::warn!(
                    event = "agent_preparation_input_failed",
                    error_code = error.code(),
                    retryable = error.retryable(),
                );
                return self
                    .terminal_after_final_lease(PreInvocationTerminalCause::InputContent(error))
                    .await;
            }
            Err(error) if is_terminal_lease(&error) => {
                return Ok(self.terminal(PreInvocationTerminalCause::Cancelled(error)));
            }
            Err(error) => return preparation_error(AgentPreparationError::Lease(error)),
        };
        let request = match self.request_from(materialized.as_bytes()) {
            Ok(request) => request,
            Err(error) => {
                return self
                    .terminal_after_final_lease(PreInvocationTerminalCause::InputProtocol(error))
                    .await;
            }
        };
        match self.lease.ensure_running() {
            Ok(()) => {}
            Err(error) if is_terminal_lease(&error) => {
                return Ok(self.terminal(PreInvocationTerminalCause::Cancelled(error)));
            }
            Err(error) => return preparation_error(AgentPreparationError::Lease(error)),
        }
        match deadline_exceeded(&self.verified, clock) {
            Ok(true) => {
                return self
                    .terminal_after_final_lease(PreInvocationTerminalCause::DeadlineExceeded)
                    .await;
            }
            Ok(false) => {}
            Err(error) => return preparation_error(error),
        }
        tracing::Span::current().record("outcome", "prepared");
        tracing::info!(event = "agent_preparation_completed");
        Ok(AgentPreparationOutcome::Prepared(Box::new(
            PreparedAgentInvocation {
                delivery: self.delivery,
                verified: self.verified,
                request,
                output_spool: self.output_spool,
                execution: self.execution,
                reservation: self.reservation,
                lease: self.lease,
            },
        )))
    }

    fn request_from(&self, raw: &[u8]) -> Result<AgentExecutionRequest, ProtocolError> {
        let message = parse_agent_execution_input(raw)?;
        let binding = agent_input_binding(&self.execution)?;
        crate::agents::request_from(message, self.kind, binding)
    }

    async fn terminal_after_final_lease(
        self,
        proposed: PreInvocationTerminalCause,
    ) -> Result<AgentPreparationOutcome, AgentPreparationError> {
        let cause = match self.lease.check_now().await {
            Ok(()) => proposed,
            Err(error) if is_terminal_lease(&error) => PreInvocationTerminalCause::Cancelled(error),
            Err(error) => return preparation_error(AgentPreparationError::Lease(error)),
        };
        Ok(self.terminal(cause))
    }

    fn terminal(self, cause: PreInvocationTerminalCause) -> AgentPreparationOutcome {
        pre_invocation_terminal(
            self.delivery,
            self.verified,
            self.output_spool,
            self.execution,
            self.reservation,
            self.lease,
            cause,
        )
    }
}

fn deadline_exceeded<K: UnixMillisClock>(
    verified: &VerifiedAgentCommand,
    clock: &K,
) -> Result<bool, AgentPreparationError> {
    let now = clock.now_unix_millis();
    if now <= 0 {
        return Err(AgentPreparationError::Clock(
            "the wall clock cannot validate the agent command deadline",
        ));
    }
    let deadline = verified.command().deadline_unix_millis;
    Ok(deadline <= now)
}

fn agent_input_binding(
    execution: &LeaseMonitoredAgentExecution,
) -> Result<AgentInputBinding, ProtocolError> {
    let bundle = execution.input_bundle();
    let bundle_reference = execution.input_bundle_ref();
    let request = execution.request_entry();
    let content = request.content.as_ref().ok_or(ProtocolError::InvalidInput(
        "the agent request content binding is missing",
    ))?;
    Ok(AgentInputBinding {
        input_bundle_id: bundle.input_bundle_id.clone(),
        input_bundle_digest: sha256_bytes(bundle_reference.digest.as_ref())?,
        request_entry_id: request.entry_id.clone(),
        request_immutable_version: request.immutable_version.clone(),
        request_content_digest: sha256_bytes(content.digest.as_ref())?,
    })
}

fn sha256_bytes(value: Option<&DigestV1>) -> Result<[u8; 32], ProtocolError> {
    let value = value.ok_or(ProtocolError::InvalidInput(
        "the agent input digest binding is missing",
    ))?;
    if value.algorithm != DigestAlgorithmV1::Sha256 as i32 {
        return Err(ProtocolError::InvalidInput(
            "the agent input digest binding is malformed",
        ));
    }
    value
        .value
        .as_slice()
        .try_into()
        .map_err(|_| ProtocolError::InvalidInput("the agent input digest binding is malformed"))
}

fn is_terminal_lease(error: &ClaimLeaseError) -> bool {
    matches!(error, ClaimLeaseError::Cancelled(_))
}

fn pre_invocation_terminal(
    delivery: RedisCommandDelivery,
    verified: VerifiedAgentCommand,
    output_spool: PreparedAgentOutput,
    execution: LeaseMonitoredAgentExecution,
    reservation: InvocationReservation,
    lease: ClaimLeaseMonitor,
    cause: PreInvocationTerminalCause,
) -> AgentPreparationOutcome {
    tracing::Span::current().record("outcome", "pre_invocation_terminal");
    tracing::Span::current().record("error_code", cause.code());
    AgentPreparationOutcome::PreInvocationTerminal(Box::new(PreInvocationTerminal {
        delivery,
        verified,
        output_authority: execution.into_output_authority(),
        output_spool,
        reservation,
        lease,
        cause,
    }))
}

fn preparation_error<T>(error: AgentPreparationError) -> Result<T, AgentPreparationError> {
    tracing::Span::current().record("outcome", "error_noack");
    tracing::Span::current().record("error_code", error.code().as_str());
    Err(error)
}

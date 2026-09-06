//! Whole-delivery lifecycle for one external-MCP direct toolkit read.

use std::fmt;
use std::sync::Arc;

use tracing::Instrument as _;

use super::agent_lease::{
    ClaimLeaseActivation, ClaimLeaseError, ClaimLeaseMonitor, UnixMillisClock,
};
use super::agent_preparation::{AgentInputMaterializer, AgentPreparationConfig};
use super::invocation_admission::{InvocationAdmission, InvocationAdmissionError};
use super::output_delivery::{
    AgentOutputPreflight, AgentOutputPreflightError, AgentTerminalRecoveryConfig,
    PreparedAgentOutput, ToolkitTerminalReplay,
};
use super::toolkit_delivery::{
    FreshToolkitDelivery, ToolkitDeliveryError, ToolkitDeliveryRoute, ToolkitDeliveryRouter,
    ToolkitOutputRecoveryDelivery,
};
use super::toolkit_output::{
    EmptyToolkitOutputRecovery, ToolkitOutputPreflightOutcome, ToolkitTerminalError,
    ToolkitTerminalRecovery, prepare_empty_toolkit_recovery, prepare_toolkit_output,
    replay_toolkit_terminal_with_replacement,
};
use crate::protocol::ProtocolError;
use crate::protocol::command::VerifiedToolkitExecuteReadCommand;
use crate::protocol::control::{
    AgentControlClient, AgentExecutionOutputAuthority, AgentOutputRecoveryKind,
    BeginAgentExecution, DesiredExecutionState, LeaseMonitoredAgentExecution,
};
use crate::protocol::elitea::runtime::v1::{DigestV1, ToolkitExecuteReadResultV1};
use crate::protocol::output::{RuntimeFailureKind, ToolkitExecuteReadTerminalOutput};
use crate::toolkits::{
    DirectToolkitRequest, DirectToolkitRequestErrorCode, DirectToolkitRuntime,
    DirectToolkitRuntimeErrorCode,
};
use crate::transport::redis_commands::{
    RedisCommandDelivery, RedisCommandError, RedisCommandRetirer, RedisRetirementClient,
};
use crate::transport::{ControlRpc, InputContentError};

pub(super) struct ToolkitDeliveryProcessor<R, RC, T, K, I> {
    router: ToolkitDeliveryRouter<R, RC>,
    output: AgentOutputPreflight,
    control: Arc<AgentControlClient<R>>,
    retirer: Arc<RedisCommandRetirer<RC>>,
    replay: Arc<T>,
    input: Arc<I>,
    clock: Arc<K>,
    admission: InvocationAdmission,
    preparation: AgentPreparationConfig,
    terminal_recovery: AgentTerminalRecoveryConfig,
    runtime: DirectToolkitRuntime,
}

impl<R, RC, T, K, I> ToolkitDeliveryProcessor<R, RC, T, K, I>
where
    R: ControlRpc + 'static,
    RC: RedisRetirementClient + 'static,
    T: ToolkitTerminalReplay + 'static,
    K: UnixMillisClock,
    I: AgentInputMaterializer + 'static,
{
    #[allow(clippy::too_many_arguments)]
    pub(super) fn new(
        output: AgentOutputPreflight,
        control: Arc<AgentControlClient<R>>,
        retirer: Arc<RedisCommandRetirer<RC>>,
        replay: Arc<T>,
        input: Arc<I>,
        clock: Arc<K>,
        admission: InvocationAdmission,
        preparation: AgentPreparationConfig,
        terminal_recovery: AgentTerminalRecoveryConfig,
        runtime: DirectToolkitRuntime,
    ) -> Self {
        Self {
            router: ToolkitDeliveryRouter::new(Arc::clone(&control), Arc::clone(&retirer)),
            output,
            control,
            retirer,
            replay,
            input,
            clock,
            admission,
            preparation,
            terminal_recovery,
            runtime,
        }
    }

    pub(super) async fn process_verified(
        &self,
        delivery: RedisCommandDelivery,
        verified: VerifiedToolkitExecuteReadCommand,
    ) -> Result<ToolkitProcessOutcome, ToolkitProcessError> {
        let route = self
            .router
            .route(delivery, verified, self.clock.now_unix_millis())
            .await
            .map_err(ToolkitProcessError::Delivery)?;
        match route {
            ToolkitDeliveryRoute::Fresh(fresh) => Box::pin(self.process_fresh(*fresh)).await,
            ToolkitDeliveryRoute::OutputRecovery(recovery) => {
                Box::pin(self.process_output_recovery(*recovery)).await
            }
            ToolkitDeliveryRoute::RetryLaterNoAck => Ok(ToolkitProcessOutcome::retained(
                "toolkit_delivery.retry_later",
                true,
            )),
            ToolkitDeliveryRoute::Completed => Ok(ToolkitProcessOutcome::completed(
                "toolkit_delivery.redelivery_retired",
            )),
        }
    }

    async fn process_fresh(
        &self,
        fresh: FreshToolkitDelivery,
    ) -> Result<ToolkitProcessOutcome, ToolkitProcessError> {
        match prepare_toolkit_output(&self.output, fresh)
            .await
            .map_err(ToolkitProcessError::OutputPreflight)?
        {
            ToolkitOutputPreflightOutcome::Empty(empty) => {
                let (fresh, output) = empty.into_parts();
                Box::pin(self.execute_fresh(fresh, output)).await
            }
            ToolkitOutputPreflightOutcome::Terminal(recovery) => {
                self.recover_terminal(*recovery).await?;
                Ok(ToolkitProcessOutcome::completed(
                    "toolkit_delivery.accepted_terminal_retired",
                ))
            }
        }
    }

    async fn process_output_recovery(
        &self,
        recovery: ToolkitOutputRecoveryDelivery,
    ) -> Result<ToolkitProcessOutcome, ToolkitProcessError> {
        let failure = match (
            recovery.recovery().kind(),
            recovery.recovery().desired_state(),
        ) {
            (AgentOutputRecoveryKind::AmbiguousInvocation, DesiredExecutionState::Running) => {
                RuntimeFailureKind::Internal
            }
            (AgentOutputRecoveryKind::Running, DesiredExecutionState::Cancelled) => {
                RuntimeFailureKind::Cancelled
            }
            _ => {
                return Ok(ToolkitProcessOutcome::retained(
                    "toolkit_delivery.output_recovery_pending",
                    true,
                ));
            }
        };
        let Some(recovery) = prepare_empty_toolkit_recovery(&self.output, recovery)
            .await
            .map_err(ToolkitProcessError::OutputPreflight)?
        else {
            return Ok(ToolkitProcessOutcome::retained(
                "toolkit_delivery.output_recovery_pending",
                true,
            ));
        };
        self.reconcile_empty_recovery(recovery, failure).await?;
        Ok(ToolkitProcessOutcome::completed(
            "toolkit_delivery.ambiguous_execution_reconciled",
        ))
    }

    async fn execute_fresh(
        &self,
        fresh: FreshToolkitDelivery,
        output: PreparedAgentOutput,
    ) -> Result<ToolkitProcessOutcome, ToolkitProcessError> {
        let reservation = match self.admission.reserve().await {
            Ok(reservation) => reservation,
            Err(error) if error.retryable() => {
                return Ok(ToolkitProcessOutcome::retained(
                    "toolkit_delivery.admission_retry",
                    true,
                ));
            }
            Err(error) => return Err(ToolkitProcessError::Admission(error)),
        };
        let (delivery, verified, claim) = fresh.into_parts();
        let preparing = match self.control.begin_agent_execution(claim).await {
            Ok(BeginAgentExecution::Preparing(preparing)) => preparing,
            Ok(BeginAgentExecution::AlreadyStarted(_)) => {
                return Ok(ToolkitProcessOutcome::retained(
                    "toolkit_delivery.already_started",
                    true,
                ));
            }
            Err(error) => return Err(ToolkitProcessError::Control(error)),
        };
        let mut lease = ClaimLeaseMonitor::start(
            Arc::clone(&self.control),
            preparing.start_lease_monitor(),
            Arc::clone(&self.clock),
            self.preparation.lease_config(),
        );
        let execution = match lease.activate().await {
            ClaimLeaseActivation::Active(execution) => execution,
            ClaimLeaseActivation::Inactive { execution, error } => {
                let output_authority = execution.into_output_authority();
                let failure = lease_failure(&error);
                let outcome = Box::pin(self.publish_fresh_terminal(
                    delivery,
                    verified,
                    output_authority,
                    output,
                    lease,
                    ToolkitExecuteReadTerminalOutput::Failure(failure),
                ))
                .await;
                drop(reservation);
                outcome?;
                return Ok(ToolkitProcessOutcome::completed(
                    "toolkit_delivery.activation_terminal_retired",
                ));
            }
            ClaimLeaseActivation::Unavailable(error) => {
                return Err(ToolkitProcessError::Lease(error));
            }
        };

        let outcome = self
            .materialize_and_execute(&verified, &execution, &mut lease)
            .await;
        let output_authority = execution.into_output_authority();
        let terminal = match outcome {
            Ok(result) => ToolkitExecuteReadTerminalOutput::Result(Box::new(result)),
            Err(failure) => ToolkitExecuteReadTerminalOutput::Failure(failure),
        };
        let published = Box::pin(self.publish_fresh_terminal(
            delivery,
            verified,
            output_authority,
            output,
            lease,
            terminal,
        ))
        .await;
        drop(reservation);
        published?;
        Ok(ToolkitProcessOutcome::completed(
            "toolkit_delivery.executed_retired",
        ))
    }

    async fn materialize_and_execute(
        &self,
        verified: &VerifiedToolkitExecuteReadCommand,
        execution: &LeaseMonitoredAgentExecution,
        lease: &mut ClaimLeaseMonitor,
    ) -> Result<ToolkitExecuteReadResultV1, RuntimeFailureKind> {
        if deadline_exceeded(verified, self.clock.as_ref()) {
            record_toolkit_deadline("pre_materialization_deadline");
            return Err(RuntimeFailureKind::DeadlineExceeded);
        }
        let materialized = match lease
            .run_pre_invocation(self.input.materialize(execution))
            .await
        {
            Ok(Ok(materialized)) => materialized,
            Ok(Err(error)) => {
                record_toolkit_input_failure("input_materialization", &error);
                return Err(input_failure(&error));
            }
            Err(error) => {
                record_toolkit_execution_failure(
                    "input_materialization_lease",
                    error.code().as_str(),
                    error.retryable(),
                );
                return Err(lease_failure(&error));
            }
        };
        let request = match DirectToolkitRequest::parse(materialized.as_bytes()) {
            Ok(request) => request,
            Err(error) => {
                let code = error.code();
                record_toolkit_execution_failure(
                    "request_validation",
                    request_error_code(code),
                    false,
                );
                return Err(request_failure(code));
            }
        };
        if deadline_exceeded(verified, self.clock.as_ref()) {
            record_toolkit_deadline("pre_invocation_deadline");
            return Err(RuntimeFailureKind::DeadlineExceeded);
        }
        let result = match lease
            .run_pre_invocation(self.runtime.execute(
                &request,
                &verified.command().execution_id,
                &verified.command().command_id,
            ))
            .await
        {
            Ok(Ok(result)) => result,
            Ok(Err(error)) => {
                let code = error.code();
                record_toolkit_execution_failure(
                    "toolkit_invocation",
                    runtime_error_code(code),
                    error.retryable(),
                );
                return Err(runtime_failure(code));
            }
            Err(error) => {
                record_toolkit_execution_failure(
                    "toolkit_invocation_lease",
                    error.code().as_str(),
                    error.retryable(),
                );
                return Err(lease_failure(&error));
            }
        };
        if let Err(error) = lease.check_now().await {
            record_toolkit_execution_failure(
                "post_invocation_lease",
                error.code().as_str(),
                error.retryable(),
            );
            return Err(lease_failure(&error));
        }
        if deadline_exceeded(verified, self.clock.as_ref()) {
            record_toolkit_deadline("post_invocation_deadline");
            return Err(RuntimeFailureKind::DeadlineExceeded);
        }
        match bind_result(execution, &request, &result) {
            Ok(result) => Ok(result),
            Err(failure) => {
                record_toolkit_execution_failure(
                    "result_binding",
                    runtime_failure_code(failure),
                    false,
                );
                Err(failure)
            }
        }
    }

    #[allow(clippy::too_many_arguments)]
    async fn publish_fresh_terminal(
        &self,
        delivery: RedisCommandDelivery,
        verified: VerifiedToolkitExecuteReadCommand,
        output_authority: AgentExecutionOutputAuthority,
        output: PreparedAgentOutput,
        lease: ClaimLeaseMonitor,
        proposed: ToolkitExecuteReadTerminalOutput,
    ) -> Result<(), ToolkitProcessError> {
        let result = async {
            let terminal = match lease.check_now().await {
                Ok(()) if deadline_exceeded(&verified, self.clock.as_ref()) => {
                    ToolkitExecuteReadTerminalOutput::Failure(RuntimeFailureKind::DeadlineExceeded)
                }
                Ok(()) => proposed,
                Err(ClaimLeaseError::Cancelled(_)) => {
                    ToolkitExecuteReadTerminalOutput::Failure(RuntimeFailureKind::Cancelled)
                }
                Err(error) => return Err(ToolkitProcessError::Lease(error)),
            };
            let occurred_at = valid_now(self.clock.as_ref())?;
            let frame = output_authority
                .bind_toolkit_execute_read_terminal(&verified, terminal, occurred_at)
                .map_err(ToolkitProcessError::Protocol)?;
            let (spool, reopener) = output
                .persist_terminal(&frame)
                .await
                .map_err(ToolkitProcessError::Output)?;
            let (acknowledged, _) = replay_toolkit_terminal_with_replacement(
                self.replay.as_ref(),
                &verified,
                frame,
                spool,
                reopener,
                self.terminal_recovery.max_output_sessions(),
            )
            .await
            .map_err(ToolkitProcessError::Terminal)?;
            let receipt = self
                .control
                .prepare_agent_settlement(acknowledged)
                .await
                .map_err(ToolkitProcessError::Control)?;
            self.retirer
                .retire_toolkit_execute_read_command(delivery, &verified, receipt.into())
                .await
                .map_err(ToolkitProcessError::Redis)
        }
        .await;
        if let Err(error) = lease.close().await {
            tracing::warn!(
                event = "toolkit_delivery_lease_close_failed",
                error_code = error.code().as_str(),
                retryable = error.retryable(),
            );
        }
        result
    }

    async fn recover_terminal(
        &self,
        recovery: ToolkitTerminalRecovery,
    ) -> Result<(), ToolkitProcessError> {
        let ToolkitTerminalRecovery {
            delivery,
            verified,
            claim,
            spool,
            frame,
            reopener,
        } = recovery;
        let lease = ClaimLeaseMonitor::start_recovery(
            Arc::clone(&self.control),
            claim,
            Arc::clone(&self.clock),
            self.preparation.lease_config(),
        );
        let result = async {
            let (acknowledged, _) = replay_toolkit_terminal_with_replacement(
                self.replay.as_ref(),
                &verified,
                frame,
                spool,
                reopener,
                self.terminal_recovery.max_output_sessions(),
            )
            .await
            .map_err(ToolkitProcessError::Terminal)?;
            let receipt = self
                .control
                .prepare_agent_settlement(acknowledged)
                .await
                .map_err(ToolkitProcessError::Control)?;
            self.retirer
                .retire_toolkit_execute_read_command(delivery, &verified, receipt.into())
                .await
                .map_err(ToolkitProcessError::Redis)
        }
        .await;
        if let Err(error) = lease.close().await {
            tracing::warn!(
                event = "toolkit_delivery_recovery_lease_close_failed",
                error_code = error.code().as_str(),
                retryable = error.retryable(),
            );
        }
        result
    }

    async fn reconcile_empty_recovery(
        &self,
        recovery: EmptyToolkitOutputRecovery,
        proposed: RuntimeFailureKind,
    ) -> Result<(), ToolkitProcessError> {
        let (delivery, verified, recovery, output) = recovery.into_parts();
        let (authority, lease_handle) = recovery.split_lease_authority();
        let lease = ClaimLeaseMonitor::start_output_recovery(
            Arc::clone(&self.control),
            lease_handle,
            Arc::clone(&self.clock),
            self.preparation.lease_config(),
        );
        let result = async {
            let failure = match lease.check_now().await {
                Ok(()) if deadline_exceeded(&verified, self.clock.as_ref()) => {
                    RuntimeFailureKind::DeadlineExceeded
                }
                Ok(()) => proposed,
                Err(ClaimLeaseError::Cancelled(_)) => RuntimeFailureKind::Cancelled,
                Err(error) => return Err(ToolkitProcessError::Lease(error)),
            };
            let frame = authority
                .bind_toolkit_execute_read_failure_terminal(
                    &verified,
                    failure,
                    valid_now(self.clock.as_ref())?,
                )
                .map_err(ToolkitProcessError::Protocol)?;
            let (spool, reopener) = output
                .persist_terminal(&frame)
                .await
                .map_err(ToolkitProcessError::Output)?;
            let (acknowledged, _) = replay_toolkit_terminal_with_replacement(
                self.replay.as_ref(),
                &verified,
                frame,
                spool,
                reopener,
                self.terminal_recovery.max_output_sessions(),
            )
            .await
            .map_err(ToolkitProcessError::Terminal)?;
            let receipt = self
                .control
                .prepare_agent_settlement(acknowledged)
                .await
                .map_err(ToolkitProcessError::Control)?;
            self.retirer
                .retire_toolkit_execute_read_command(delivery, &verified, receipt.into())
                .await
                .map_err(ToolkitProcessError::Redis)
        }
        .await;
        if let Err(error) = lease.close().await {
            tracing::warn!(
                event = "toolkit_delivery_reconcile_lease_close_failed",
                error_code = error.code().as_str(),
                retryable = error.retryable(),
            );
        }
        result
    }
}

fn bind_result(
    execution: &LeaseMonitoredAgentExecution,
    request: &DirectToolkitRequest,
    result: &serde_json::Value,
) -> Result<ToolkitExecuteReadResultV1, RuntimeFailureKind> {
    let bundle = execution.input_bundle_ref();
    let entry = execution.request_entry();
    let content = entry
        .content
        .as_ref()
        .ok_or(RuntimeFailureKind::InvalidInput)?;
    let result_json = serde_json::to_vec(result).map_err(|_| RuntimeFailureKind::InvalidInput)?;
    Ok(ToolkitExecuteReadResultV1 {
        input_bundle_id: bundle.input_bundle_id.clone(),
        input_bundle_digest: clone_digest(bundle.digest.as_ref())?,
        request_entry_id: entry.entry_id.clone(),
        request_entry_version: entry.immutable_version.clone(),
        request_content_digest: clone_digest(content.digest.as_ref())?,
        result_json,
        toolkit_type: request.toolkit_type().to_owned(),
        toolkit_name: request.toolkit_name().to_owned(),
        tool_name: request.tool_name().to_owned(),
    })
}

fn clone_digest(digest: Option<&DigestV1>) -> Result<Option<DigestV1>, RuntimeFailureKind> {
    let digest = digest.ok_or(RuntimeFailureKind::InvalidInput)?;
    Ok(Some(digest.clone()))
}

fn valid_now(clock: &impl UnixMillisClock) -> Result<i64, ToolkitProcessError> {
    let now = clock.now_unix_millis();
    if now <= 0 {
        return Err(ToolkitProcessError::Clock);
    }
    Ok(now)
}

fn deadline_exceeded(
    verified: &VerifiedToolkitExecuteReadCommand,
    clock: &impl UnixMillisClock,
) -> bool {
    let now = clock.now_unix_millis();
    now <= 0 || now >= verified.command().deadline_unix_millis
}

const fn input_failure(error: &InputContentError) -> RuntimeFailureKind {
    match error {
        InputContentError::InvalidInput(_) => RuntimeFailureKind::InvalidInput,
        InputContentError::ResourceExhausted(_) => RuntimeFailureKind::ResourceExhausted,
        InputContentError::AuthorizationFailed(_) => RuntimeFailureKind::AuthorizationFailed,
        InputContentError::InvalidConfiguration(_) => RuntimeFailureKind::Internal,
        InputContentError::DependencyUnavailable(_)
        | InputContentError::Transport(_)
        | InputContentError::Timeout(_) => RuntimeFailureKind::DependencyUnavailable,
    }
}

const fn request_failure(code: DirectToolkitRequestErrorCode) -> RuntimeFailureKind {
    match code {
        DirectToolkitRequestErrorCode::InvalidInput => RuntimeFailureKind::InvalidInput,
        DirectToolkitRequestErrorCode::ResourceExhausted => RuntimeFailureKind::ResourceExhausted,
        DirectToolkitRequestErrorCode::IncompatibleVersion => {
            RuntimeFailureKind::IncompatibleVersion
        }
    }
}

const fn request_error_code(code: DirectToolkitRequestErrorCode) -> &'static str {
    match code {
        DirectToolkitRequestErrorCode::InvalidInput => "toolkit_request.invalid_input",
        DirectToolkitRequestErrorCode::ResourceExhausted => "toolkit_request.resource_exhausted",
        DirectToolkitRequestErrorCode::IncompatibleVersion => {
            "toolkit_request.incompatible_version"
        }
    }
}

const fn runtime_failure(code: DirectToolkitRuntimeErrorCode) -> RuntimeFailureKind {
    match code {
        DirectToolkitRuntimeErrorCode::InvalidInput
        | DirectToolkitRuntimeErrorCode::InvalidConfiguration
        | DirectToolkitRuntimeErrorCode::ToolNotSelected => RuntimeFailureKind::InvalidInput,
        DirectToolkitRuntimeErrorCode::ResourceExhausted => RuntimeFailureKind::ResourceExhausted,
        DirectToolkitRuntimeErrorCode::UnsupportedToolkit
        | DirectToolkitRuntimeErrorCode::EffectfulToolUnavailable => {
            RuntimeFailureKind::UnsupportedCapability
        }
        DirectToolkitRuntimeErrorCode::ToolBlocked
        | DirectToolkitRuntimeErrorCode::SensitiveToolUnavailable
        | DirectToolkitRuntimeErrorCode::AuthorizationRequired => {
            RuntimeFailureKind::AuthorizationFailed
        }
        DirectToolkitRuntimeErrorCode::DependencyUnavailable => {
            RuntimeFailureKind::DependencyUnavailable
        }
        DirectToolkitRuntimeErrorCode::DeadlineExceeded => RuntimeFailureKind::DeadlineExceeded,
    }
}

const fn runtime_error_code(code: DirectToolkitRuntimeErrorCode) -> &'static str {
    match code {
        DirectToolkitRuntimeErrorCode::InvalidInput => "toolkit_runtime.invalid_input",
        DirectToolkitRuntimeErrorCode::InvalidConfiguration => {
            "toolkit_runtime.invalid_configuration"
        }
        DirectToolkitRuntimeErrorCode::ResourceExhausted => "toolkit_runtime.resource_exhausted",
        DirectToolkitRuntimeErrorCode::UnsupportedToolkit => "toolkit_runtime.unsupported_toolkit",
        DirectToolkitRuntimeErrorCode::ToolNotSelected => "toolkit_runtime.tool_not_selected",
        DirectToolkitRuntimeErrorCode::ToolBlocked => "toolkit_runtime.tool_blocked",
        DirectToolkitRuntimeErrorCode::SensitiveToolUnavailable => {
            "toolkit_runtime.sensitive_tool_unavailable"
        }
        DirectToolkitRuntimeErrorCode::EffectfulToolUnavailable => {
            "toolkit_runtime.effectful_tool_unavailable"
        }
        DirectToolkitRuntimeErrorCode::AuthorizationRequired => {
            "toolkit_runtime.authorization_required"
        }
        DirectToolkitRuntimeErrorCode::DependencyUnavailable => {
            "toolkit_runtime.dependency_unavailable"
        }
        DirectToolkitRuntimeErrorCode::DeadlineExceeded => "toolkit_runtime.deadline_exceeded",
    }
}

const fn runtime_failure_code(failure: RuntimeFailureKind) -> &'static str {
    match failure {
        RuntimeFailureKind::UnsupportedCapability => "runtime.unsupported_capability",
        RuntimeFailureKind::IncompatibleVersion => "runtime.incompatible_version",
        RuntimeFailureKind::InvalidInput => "runtime.invalid_input",
        RuntimeFailureKind::ResourceExhausted => "runtime.resource_exhausted",
        RuntimeFailureKind::DependencyUnavailable => "runtime.dependency_unavailable",
        RuntimeFailureKind::DeadlineExceeded => "runtime.deadline_exceeded",
        RuntimeFailureKind::AuthorizationFailed => "runtime.authorization_failed",
        RuntimeFailureKind::Cancelled => "runtime.cancelled",
        RuntimeFailureKind::Internal => "runtime.internal",
    }
}

fn record_toolkit_execution_failure(
    stage: &'static str,
    error_code: &'static str,
    retryable: bool,
) {
    tracing::warn!(
        event = "toolkit_execution_failed",
        stage,
        error_code,
        retryable,
        "direct toolkit execution failed at a redacted runtime boundary"
    );
}

fn record_toolkit_deadline(stage: &'static str) {
    record_toolkit_execution_failure(stage, "toolkit_execution.deadline_exceeded", false);
}

fn record_toolkit_input_failure(stage: &'static str, error: &InputContentError) {
    tracing::warn!(
        event = "toolkit_execution_failed",
        stage,
        error_code = error.code(),
        retryable = error.retryable(),
        safe_message = %error,
        "direct toolkit input materialization failed at a redacted runtime boundary"
    );
}

const fn lease_failure(error: &ClaimLeaseError) -> RuntimeFailureKind {
    match error {
        ClaimLeaseError::Cancelled(_) => RuntimeFailureKind::Cancelled,
        _ => RuntimeFailureKind::DependencyUnavailable,
    }
}

pub(super) enum ToolkitProcessOutcome {
    Completed { code: &'static str },
    RetainedNoAck { code: &'static str, retryable: bool },
}

impl ToolkitProcessOutcome {
    const fn completed(code: &'static str) -> Self {
        Self::Completed { code }
    }

    const fn retained(code: &'static str, retryable: bool) -> Self {
        Self::RetainedNoAck { code, retryable }
    }

    pub(super) fn record(self) {
        match self {
            Self::Completed { code } => tracing::info!(
                event = "toolkit_delivery_completed",
                outcome = "completed",
                result_code = code,
            ),
            Self::RetainedNoAck { code, retryable } => tracing::warn!(
                event = "toolkit_delivery_retained_no_ack",
                outcome = "retained_no_ack",
                result_code = code,
                retryable,
            ),
        }
    }
}

#[derive(Debug)]
pub(super) enum ToolkitProcessError {
    Delivery(ToolkitDeliveryError),
    OutputPreflight(AgentOutputPreflightError),
    Admission(InvocationAdmissionError),
    Control(crate::protocol::control::AgentControlError),
    Lease(ClaimLeaseError),
    Protocol(ProtocolError),
    Output(crate::transport::OutputGrpcError),
    Terminal(ToolkitTerminalError),
    Redis(RedisCommandError),
    Clock,
}

impl ToolkitProcessError {
    pub(super) fn code(&self) -> &'static str {
        match self {
            Self::Delivery(error) => error.code(),
            Self::OutputPreflight(error) => error.code(),
            Self::Admission(error) => error.code().as_str(),
            Self::Control(error) if error.retryable() => "toolkit_delivery.control_unavailable",
            Self::Control(_) => "toolkit_delivery.control_rejected",
            Self::Lease(error) => error.code().as_str(),
            Self::Protocol(error) => match error {
                ProtocolError::ResourceExhausted(_) => "toolkit_delivery.resource_exhausted",
                _ => "toolkit_delivery.invalid_terminal",
            },
            Self::Output(error) if output_retryable(error) => "toolkit_delivery.output_unavailable",
            Self::Output(_) => "toolkit_delivery.output_rejected",
            Self::Terminal(error) => error.code(),
            Self::Redis(error) => error.code(),
            Self::Clock => "toolkit_delivery.invalid_clock",
        }
    }

    pub(super) fn retryable(&self) -> bool {
        match self {
            Self::Delivery(error) => error.retryable(),
            Self::OutputPreflight(error) => error.retryable(),
            Self::Admission(error) => error.retryable(),
            Self::Control(error) => error.retryable(),
            Self::Lease(error) => error.retryable(),
            Self::Terminal(error) => error.retryable(),
            Self::Redis(error) => error.retryable(),
            Self::Output(error) => output_retryable(error),
            Self::Protocol(_) | Self::Clock => false,
        }
    }
}

fn output_retryable(error: &crate::transport::OutputGrpcError) -> bool {
    matches!(
        error,
        crate::transport::OutputGrpcError::Unavailable(_)
            | crate::transport::OutputGrpcError::Protocol(
                crate::transport::OutputProtocolError::DependencyUnavailable
            )
            | crate::transport::OutputGrpcError::Spool(
                crate::spool::SpoolError::OwnershipUnavailable(_)
                    | crate::spool::SpoolError::Unavailable { .. }
            )
    )
}

impl fmt::Display for ToolkitProcessError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(self.code())
    }
}

impl std::error::Error for ToolkitProcessError {}

pub(super) async fn process_toolkit_verified<R, RC, T, K, I>(
    processor: &ToolkitDeliveryProcessor<R, RC, T, K, I>,
    delivery: RedisCommandDelivery,
    verified: VerifiedToolkitExecuteReadCommand,
) where
    R: ControlRpc + 'static,
    RC: RedisRetirementClient + 'static,
    T: ToolkitTerminalReplay + 'static,
    K: UnixMillisClock,
    I: AgentInputMaterializer + 'static,
{
    let command = verified.command();
    let span = tracing::info_span!(
        parent: None,
        "toolkit.delivery",
        execution_id = %command.execution_id,
        generation = command.generation,
        command_id = %command.command_id,
        tenant_id = %command.tenant_id,
        resource_project_id = %command.resource_project_id,
        projection_project_id = %command.projection_project_id,
        capability_id = %command.capability_id,
        redis_stream = %delivery.stream(),
        redis_entry_id = %delivery.entry_id(),
    );
    Box::pin(
        async move {
            match Box::pin(processor.process_verified(delivery, verified)).await {
                Ok(outcome) => outcome.record(),
                Err(error) => tracing::warn!(
                    event = "toolkit_delivery_failed",
                    error_code = error.code(),
                    retryable = error.retryable(),
                ),
            }
        }
        .instrument(span),
    )
    .await;
}

//! Whole-delivery composition for application and ad-hoc agent commands.
//!
//! This is the sole bridge from Redis PEL ownership into claim routing,
//! encrypted-output preflight, fresh preparation and the process-owned native
//! invocation coordinator. Its processing future does not return until the
//! exact command is retired or deliberately retained for recovery, allowing
//! the Redis runtime to heartbeat the PEL entry for the full lifecycle.

#![allow(dead_code)] // Production bootstrap and capability registration remain gated.

use std::sync::Arc;

use async_trait::async_trait;
use tracing::Instrument as _;

use super::agent_coordinator::{AgentInvocationCoordinator, native_agent_coordinator};
use super::agent_delivery::{AgentDeliveryError, AgentDeliveryRoute, AgentDeliveryRouter};
use super::agent_invocation::{
    AgentAuthorizationJobCompletion, AgentAuthorizationJobError,
    AgentAuthorizedLifecycleDisposition, AuthorizedAgentLifecycle,
};
use super::agent_lease::UnixMillisClock;
use super::agent_preparation::{
    AgentInputMaterializer, AgentPreparationConfig, AgentPreparationError, AgentPreparationOutcome,
    prepare_fresh_agent_invocation_with,
};
use super::invocation_admission::InvocationAdmission;
use super::invocation_supervisor::InvocationSupervisionError;
use super::native_agent_lifecycle::NativeAuthorizedAgentLifecycle;
use super::output_delivery::{
    AgentFailureTerminalError, AgentOutputPreflight, AgentOutputPreflightError,
    AgentOutputPreflightOutcome, AgentOutputRecoveryRequiredKind, AgentProgressConnector,
    AgentTerminalRecoveryConfig, AgentTerminalRecoveryError, AgentTerminalReplay,
    publish_pre_invocation_terminal, reconcile_empty_agent_output_recovery,
    recover_accepted_terminal,
};
use super::redis_delivery::RedisDeliveryProcessor as RedisDeliveryProcessorContract;
use crate::agents::runtime::NativeAgentAssembler;
use crate::diagnostics::attach_command_trace_parent;
use crate::protocol::command::{
    SignedCommandAuthenticator, VerifiedAgentCommand, parse_and_verify_agent_command,
};
use crate::protocol::control::{
    AgentControlClient, AgentOutputRecoveryKind, DesiredExecutionState,
};
use crate::protocol::output::RuntimeFailureKind;
use crate::transport::ControlRpc;
use crate::transport::redis_commands::{
    RedisCommandDelivery, RedisCommandRetirer, RedisRetirementClient,
};

/// Complete application/ad-hoc command processor behind one Redis delivery
/// worker. Every field is an immutable process-owned dependency; no execution
/// path may resolve a second client or service locator after claim.
pub(super) struct AgentDeliveryProcessor<R, RC, T, K, D, I> {
    router: AgentDeliveryRouter<R, RC>,
    authenticator: Arc<dyn SignedCommandAuthenticator>,
    output: AgentOutputPreflight,
    control: Arc<AgentControlClient<R>>,
    retirer: Arc<RedisCommandRetirer<RC>>,
    replay: Arc<T>,
    input: Arc<I>,
    clock: Arc<K>,
    admission: InvocationAdmission,
    preparation: AgentPreparationConfig,
    terminal_recovery: AgentTerminalRecoveryConfig,
    coordinator: AgentInvocationCoordinator<R, RC, T, K, D>,
}

impl<R, RC, T, K, D, I> AgentDeliveryProcessor<R, RC, T, K, D, I>
where
    R: ControlRpc + 'static,
    RC: RedisRetirementClient + 'static,
    T: AgentTerminalReplay + 'static,
    K: UnixMillisClock,
    D: AuthorizedAgentLifecycle,
    I: AgentInputMaterializer + 'static,
{
    #[allow(clippy::too_many_arguments)] // Every explicit owner is security-significant.
    #[must_use]
    pub(super) fn new(
        authenticator: Arc<dyn SignedCommandAuthenticator>,
        output: AgentOutputPreflight,
        control: Arc<AgentControlClient<R>>,
        retirer: Arc<RedisCommandRetirer<RC>>,
        replay: Arc<T>,
        input: Arc<I>,
        clock: Arc<K>,
        admission: InvocationAdmission,
        preparation: AgentPreparationConfig,
        terminal_recovery: AgentTerminalRecoveryConfig,
        coordinator: AgentInvocationCoordinator<R, RC, T, K, D>,
    ) -> Self {
        let router = AgentDeliveryRouter::from_shared(Arc::clone(&control), Arc::clone(&retirer));
        Self {
            router,
            authenticator,
            output,
            control,
            retirer,
            replay,
            input,
            clock,
            admission,
            preparation,
            terminal_recovery,
            coordinator,
        }
    }

    /// Stop later native submissions. Normal process shutdown calls this only
    /// after Redis intake has stopped and all owned delivery futures drained.
    pub(super) fn stop(&self) -> Result<(), InvocationSupervisionError> {
        self.coordinator.stop()
    }

    /// Drain every accepted native invocation after delivery processing ends.
    pub(super) async fn close(&self) -> Result<(), InvocationSupervisionError> {
        self.coordinator.close().await
    }

    async fn process_owned(
        &self,
        delivery: RedisCommandDelivery,
        verified: VerifiedAgentCommand,
    ) -> Result<AgentDeliveryProcessOutcome, AgentDeliveryProcessError> {
        tracing::info!(event = "agent_delivery_routing_started");
        let route = self
            .router
            .route_verified(delivery, verified, self.clock.now_unix_millis())
            .await
            .map_err(AgentDeliveryProcessError::Delivery)?;
        tracing::info!(event = "agent_delivery_routed", route = ?route.kind());
        match route {
            AgentDeliveryRoute::Fresh(fresh) => Box::pin(self.process_fresh(*fresh)).await,
            AgentDeliveryRoute::OutputRecovery(recovery) => {
                Box::pin(self.process_output_recovery(*recovery)).await
            }
            AgentDeliveryRoute::RetryLaterNoAck => Ok(AgentDeliveryProcessOutcome::retained(
                "agent_delivery.retry_later",
                true,
            )),
            AgentDeliveryRoute::Completed(_) => Ok(AgentDeliveryProcessOutcome::completed(
                "agent_delivery.redelivery_retired",
            )),
        }
    }

    pub(super) async fn process_verified_delivery(
        &self,
        delivery: RedisCommandDelivery,
        verified: VerifiedAgentCommand,
    ) {
        let command = verified.command();
        let span = tracing::info_span!(
            parent: None,
            "agent.delivery",
            execution_kind = ?verified.kind(),
            execution_id = %command.execution_id,
            root_execution_id = %command.root_execution_id,
            generation = command.generation,
            command_id = %command.command_id,
            tenant_id = %command.tenant_id,
            resource_project_id = %command.resource_project_id,
            projection_project_id = %command.projection_project_id,
            capability_id = %command.capability_id,
            parent_execution_id = %command.parent_execution_id,
            parent_call_id = %command.parent_call_id,
            redis_stream = %delivery.stream(),
            redis_entry_id = %delivery.entry_id(),
            trace_context_present = !command.traceparent.is_empty(),
            remote_parent = tracing::field::Empty,
            outcome = tracing::field::Empty,
            result_code = tracing::field::Empty,
            error_code = tracing::field::Empty,
            retryable = tracing::field::Empty,
        );
        let remote_parent =
            attach_command_trace_parent(&span, &command.traceparent, &command.tracestate);
        span.record("remote_parent", remote_parent);
        Box::pin(
            async move {
                tracing::info!(event = "agent_delivery_started");
                match Box::pin(self.process_owned(delivery, verified)).await {
                    Ok(outcome) => outcome.record(),
                    Err(error) => {
                        tracing::Span::current().record("outcome", "failed_no_ack");
                        tracing::Span::current().record("error_code", error.code());
                        tracing::Span::current().record("retryable", error.retryable());
                        tracing::warn!(
                            event = "agent_delivery_failed",
                            error_code = error.code(),
                            retryable = error.retryable(),
                        );
                    }
                }
            }
            .instrument(span),
        )
        .await;
    }

    async fn process_output_recovery(
        &self,
        recovery: super::agent_delivery::OutputRecoveryAgentDelivery,
    ) -> Result<AgentDeliveryProcessOutcome, AgentDeliveryProcessError> {
        let kind = recovery.recovery_kind();
        let failure = match (kind, recovery.desired_state()) {
            (AgentOutputRecoveryKind::AmbiguousInvocation, DesiredExecutionState::Running) => {
                RuntimeFailureKind::Internal
            }
            (AgentOutputRecoveryKind::Running, DesiredExecutionState::Cancelled) => {
                RuntimeFailureKind::Cancelled
            }
            _ => {
                return Ok(AgentDeliveryProcessOutcome::retained(
                    output_recovery_code(kind),
                    true,
                ));
            }
        };
        let Some(recovery) = self
            .output
            .prepare_empty_recovery(recovery)
            .await
            .map_err(AgentDeliveryProcessError::OutputPreflight)?
        else {
            return Ok(AgentDeliveryProcessOutcome::retained(
                "agent_delivery.output_recovery_pending",
                true,
            ));
        };
        reconcile_empty_agent_output_recovery(
            Arc::clone(&self.control),
            self.retirer.as_ref(),
            self.replay.as_ref(),
            recovery,
            failure,
            Arc::clone(&self.clock),
            self.preparation.lease_config(),
            self.terminal_recovery,
        )
        .await
        .map_err(AgentDeliveryProcessError::FailureTerminal)?;
        Ok(AgentDeliveryProcessOutcome::completed(
            "agent_delivery.ambiguous_invocation_reconciled",
        ))
    }

    async fn process_fresh(
        &self,
        fresh: super::agent_delivery::FreshAgentDelivery,
    ) -> Result<AgentDeliveryProcessOutcome, AgentDeliveryProcessError> {
        tracing::info!(event = "agent_output_preflight_started");
        let outcome = self
            .output
            .prepare(fresh)
            .await
            .map_err(AgentDeliveryProcessError::OutputPreflight)?;
        tracing::info!(
            event = "agent_output_preflight_completed",
            preflight_disposition = ?outcome.kind(),
        );
        match outcome {
            AgentOutputPreflightOutcome::Empty(empty) => {
                Box::pin(self.prepare_and_submit(*empty)).await
            }
            AgentOutputPreflightOutcome::TerminalRecovery(recovery) => {
                recover_accepted_terminal(
                    Arc::clone(&self.control),
                    self.retirer.as_ref(),
                    self.replay.as_ref(),
                    *recovery,
                    Arc::clone(&self.clock),
                    self.preparation.lease_config(),
                    self.terminal_recovery,
                )
                .await
                .map_err(AgentDeliveryProcessError::TerminalRecovery)?;
                Ok(AgentDeliveryProcessOutcome::completed(
                    "agent_delivery.accepted_terminal_retired",
                ))
            }
            AgentOutputPreflightOutcome::RecoveryRequiredNoAck(recovery) => {
                Ok(AgentDeliveryProcessOutcome::retained(
                    preflight_recovery_code(recovery.kind()),
                    true,
                ))
            }
        }
    }

    async fn prepare_and_submit(
        &self,
        empty: super::output_delivery::EmptyAgentOutput,
    ) -> Result<AgentDeliveryProcessOutcome, AgentDeliveryProcessError> {
        let outcome = prepare_fresh_agent_invocation_with(
            empty,
            Arc::clone(&self.control),
            &self.admission,
            self.input.as_ref(),
            Arc::clone(&self.clock),
            self.preparation,
        )
        .await
        .map_err(AgentDeliveryProcessError::Preparation)?;
        match outcome {
            AgentPreparationOutcome::Prepared(prepared) => {
                tracing::info!(event = "agent_invocation_submission_started");
                let waiter = match self.coordinator.submit(*prepared) {
                    Ok(waiter) => {
                        tracing::info!(event = "agent_invocation_submitted");
                        waiter
                    }
                    Err(rejected) => {
                        let code = rejected.error().code();
                        let lease_error = rejected
                            .close()
                            .await
                            .map_err(AgentDeliveryProcessError::RejectedClose)?;
                        if let Some(error) = lease_error {
                            tracing::warn!(
                                event = "agent_delivery_rejected_lease_close",
                                error_code = error.code().as_str(),
                                retryable = error.retryable(),
                            );
                        }
                        return Ok(AgentDeliveryProcessOutcome::retained(code, true));
                    }
                };
                tracing::info!(event = "agent_invocation_completion_wait_started");
                let completion = waiter
                    .wait()
                    .await
                    .map_err(AgentDeliveryProcessError::Supervision)?;
                tracing::info!(event = "agent_invocation_completion_received");
                Self::finish_invocation(completion)
            }
            AgentPreparationOutcome::RetryLaterNoAck => Ok(AgentDeliveryProcessOutcome::retained(
                "agent_preparation.retry_later",
                true,
            )),
            AgentPreparationOutcome::RecoveryRequiredNoAck => Ok(
                AgentDeliveryProcessOutcome::retained("agent_preparation.recovery_required", true),
            ),
            AgentPreparationOutcome::PreInvocationTerminal(terminal) => {
                publish_pre_invocation_terminal(
                    Arc::clone(&self.control),
                    self.retirer.as_ref(),
                    self.replay.as_ref(),
                    *terminal,
                    Arc::clone(&self.clock),
                    self.terminal_recovery,
                )
                .await
                .map_err(AgentDeliveryProcessError::FailureTerminal)?;
                Ok(AgentDeliveryProcessOutcome::completed(
                    "agent_delivery.pre_invocation_terminal_retired",
                ))
            }
        }
    }

    fn finish_invocation(
        completion: AgentAuthorizationJobCompletion,
    ) -> Result<AgentDeliveryProcessOutcome, AgentDeliveryProcessError> {
        match completion {
            AgentAuthorizationJobCompletion::Authorized(completion) => {
                match completion.disposition() {
                    AgentAuthorizedLifecycleDisposition::ExecutedSettledAcked { .. } => Ok(
                        AgentDeliveryProcessOutcome::completed("agent_delivery.executed_retired"),
                    ),
                    AgentAuthorizedLifecycleDisposition::RecoveryRequiredNoAck {
                        code,
                        retryable,
                    } => Ok(AgentDeliveryProcessOutcome::retained(code, *retryable)),
                }
            }
            AgentAuthorizationJobCompletion::Terminal(Ok(_)) => {
                Ok(AgentDeliveryProcessOutcome::completed(
                    "agent_delivery.authorization_terminal_retired",
                ))
            }
            AgentAuthorizationJobCompletion::Terminal(Err(error)) => {
                Err(AgentDeliveryProcessError::FailureTerminal(error))
            }
            AgentAuthorizationJobCompletion::Unknown(completion) => {
                if let Some(error) = completion.lease_error() {
                    tracing::warn!(
                        event = "agent_delivery_unknown_lease_close",
                        error_code = error.code().as_str(),
                        retryable = error.retryable(),
                    );
                }
                Ok(AgentDeliveryProcessOutcome::retained(
                    "agent_authorization.unknown",
                    completion.authorization_error().retryable(),
                ))
            }
            AgentAuthorizationJobCompletion::InvalidState(error) => {
                Err(AgentDeliveryProcessError::InvalidAuthorization(error))
            }
        }
    }
}

#[async_trait]
impl<R, RC, T, K, D, I> RedisDeliveryProcessorContract for AgentDeliveryProcessor<R, RC, T, K, D, I>
where
    R: ControlRpc + 'static,
    RC: RedisRetirementClient + 'static,
    T: AgentTerminalReplay + 'static,
    K: UnixMillisClock,
    D: AuthorizedAgentLifecycle,
    I: AgentInputMaterializer + 'static,
{
    async fn process(&self, delivery: RedisCommandDelivery) {
        let verification = tracing::info_span!(
            "agent.delivery.verify",
            redis_stream = %delivery.stream(),
            redis_entry_id = %delivery.entry_id(),
            outcome = tracing::field::Empty,
            error_code = tracing::field::Empty,
            retryable = tracing::field::Empty,
        );
        let verified = match verification.in_scope(|| {
            tracing::info!(event = "agent_delivery_verification_started");
            parse_and_verify_agent_command(
                delivery.signed_envelope(),
                Some(self.authenticator.as_ref()),
            )
        }) {
            Ok(verified) => {
                verification.record("outcome", "verified");
                verification.in_scope(|| {
                    tracing::info!(event = "agent_delivery_verified");
                });
                verified
            }
            Err(error) => {
                let error = AgentDeliveryProcessError::Delivery(error.into());
                verification.record("outcome", "failed_no_ack");
                verification.record("error_code", error.code());
                verification.record("retryable", error.retryable());
                verification.in_scope(|| {
                    tracing::warn!(
                        event = "agent_delivery_verification_failed",
                        error_code = error.code(),
                        retryable = error.retryable(),
                    );
                });
                return;
            }
        };
        self.process_verified_delivery(delivery, verified).await;
    }
}

#[allow(clippy::too_many_arguments, clippy::type_complexity)]
pub(super) fn native_agent_delivery_processor<A, C, R, RC, K, I>(
    authenticator: Arc<dyn SignedCommandAuthenticator>,
    output: AgentOutputPreflight,
    control: Arc<AgentControlClient<R>>,
    retirer: Arc<RedisCommandRetirer<RC>>,
    input: Arc<I>,
    clock: Arc<K>,
    admission: InvocationAdmission,
    preparation: AgentPreparationConfig,
    assembler: Arc<A>,
    connector: C,
    max_output_sessions: usize,
    terminal_recovery: AgentTerminalRecoveryConfig,
) -> AgentDeliveryProcessor<R, RC, C, K, NativeAuthorizedAgentLifecycle<A, C, R, RC, K>, I>
where
    A: NativeAgentAssembler,
    C: AgentProgressConnector + AgentTerminalReplay + Clone + Send + Sync + 'static,
    R: ControlRpc + 'static,
    RC: RedisRetirementClient + 'static,
    K: UnixMillisClock,
    I: AgentInputMaterializer + 'static,
{
    let coordinator = native_agent_coordinator(
        admission.clone(),
        assembler,
        connector.clone(),
        Arc::clone(&control),
        Arc::clone(&retirer),
        Arc::clone(&clock),
        max_output_sessions,
        terminal_recovery,
    );
    AgentDeliveryProcessor::new(
        authenticator,
        output,
        control,
        retirer,
        Arc::new(connector),
        input,
        clock,
        admission,
        preparation,
        terminal_recovery,
        coordinator,
    )
}

#[derive(Clone, Copy)]
enum AgentDeliveryProcessOutcome {
    Completed { code: &'static str },
    RetainedNoAck { code: &'static str, retryable: bool },
}

impl AgentDeliveryProcessOutcome {
    const fn completed(code: &'static str) -> Self {
        Self::Completed { code }
    }

    const fn retained(code: &'static str, retryable: bool) -> Self {
        Self::RetainedNoAck { code, retryable }
    }

    fn record(self) {
        match self {
            Self::Completed { code } => {
                tracing::Span::current().record("outcome", "completed");
                tracing::Span::current().record("result_code", code);
                tracing::Span::current().record("retryable", false);
                tracing::info!(event = "agent_delivery_completed", result_code = code);
            }
            Self::RetainedNoAck { code, retryable } => {
                tracing::Span::current().record("outcome", "retained_no_ack");
                tracing::Span::current().record("result_code", code);
                tracing::Span::current().record("retryable", retryable);
                tracing::warn!(
                    event = "agent_delivery_retained_no_ack",
                    result_code = code,
                    retryable,
                );
            }
        }
    }
}

enum AgentDeliveryProcessError {
    Delivery(AgentDeliveryError),
    OutputPreflight(AgentOutputPreflightError),
    Preparation(AgentPreparationError),
    FailureTerminal(AgentFailureTerminalError),
    TerminalRecovery(AgentTerminalRecoveryError),
    Supervision(InvocationSupervisionError),
    RejectedClose(AgentAuthorizationJobError),
    InvalidAuthorization(AgentAuthorizationJobError),
}

impl AgentDeliveryProcessError {
    const fn code(&self) -> &'static str {
        match self {
            Self::Delivery(error) => error.code(),
            Self::OutputPreflight(error) => error.code(),
            Self::Preparation(error) => error.code().as_str(),
            Self::FailureTerminal(error) => error.code(),
            Self::TerminalRecovery(error) => error.code(),
            Self::Supervision(error) => error.code(),
            Self::RejectedClose(error) | Self::InvalidAuthorization(error) => error.code(),
        }
    }

    fn retryable(&self) -> bool {
        match self {
            Self::Delivery(error) => error.retryable(),
            Self::OutputPreflight(error) => error.retryable(),
            Self::Preparation(error) => error.retryable(),
            Self::FailureTerminal(error) => error.retryable(),
            Self::TerminalRecovery(error) => error.retryable(),
            Self::Supervision(
                InvocationSupervisionError::TaskLost(_)
                | InvocationSupervisionError::Unavailable(_),
            ) => true,
            Self::Supervision(
                InvocationSupervisionError::InvalidReservation(_)
                | InvocationSupervisionError::Closed(_),
            )
            | Self::RejectedClose(_)
            | Self::InvalidAuthorization(_) => false,
        }
    }
}

const fn output_recovery_code(kind: AgentOutputRecoveryKind) -> &'static str {
    match kind {
        AgentOutputRecoveryKind::ActiveLease => "agent_delivery.active_lease_recovery",
        AgentOutputRecoveryKind::Running => "agent_delivery.running_recovery",
        AgentOutputRecoveryKind::AmbiguousInvocation => {
            "agent_delivery.ambiguous_invocation_recovery"
        }
    }
}

const fn preflight_recovery_code(kind: AgentOutputRecoveryRequiredKind) -> &'static str {
    match kind {
        AgentOutputRecoveryRequiredKind::PendingProgress => {
            "agent_delivery.pending_progress_recovery"
        }
        AgentOutputRecoveryRequiredKind::ReconciledStaleProgress => {
            "agent_delivery.reconciled_progress_recovery"
        }
    }
}

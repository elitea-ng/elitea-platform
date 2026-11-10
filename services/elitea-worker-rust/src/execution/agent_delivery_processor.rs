//! Whole-delivery composition for application and ad-hoc agent commands.
//!
//! This is the sole bridge from `JetStream` delivery ownership into claim routing,
//! encrypted-output preflight, fresh preparation and the process-owned native
//! invocation coordinator. Its processing future does not return until the
//! exact command is retired or deliberately retained for recovery, allowing
//! the delivery runtime to heartbeat (`+WPI`) the message for the full lifecycle.

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
use super::command_delivery::{CommandDeliveryProcessor, verification_poison};
use super::invocation_admission::InvocationAdmission;
use super::invocation_supervisor::InvocationSupervisionError;
use super::native_agent_lifecycle::NativeAuthorizedAgentLifecycle;
use super::output_delivery::{
    AgentFailureTerminalError, AgentOutputPreflight, AgentOutputPreflightError,
    AgentOutputPreflightOutcome, AgentOutputRecoveryRequiredKind, AgentProgressConnector,
    AgentTerminalRecoveryConfig, AgentTerminalRecoveryError, AgentTerminalReplay,
    FRESH_CLAIM_TERMINAL_UNEXPECTED_CODE, NodeRecoveryOutput, OutputRecovery,
    UNRECOVERABLE_TERMINAL_CODE, publish_pre_invocation_terminal,
    reconcile_empty_agent_output_recovery, recover_accepted_terminal,
};
use crate::agents::runtime::NativeAgentAssembler;
use crate::diagnostics::attach_command_trace_parent;
use crate::protocol::command::{
    SignedCommandAuthenticator, VerifiedAgentCommand, parse_and_verify_agent_command,
};
use crate::protocol::control::{
    AgentControlClient, AgentOutputRecoveryKind, DesiredExecutionState,
};
use crate::protocol::output::RuntimeFailureKind;
use crate::transport::command_bus::{
    CommandDelivery, CommandRetirementClient, CommandRetirer, DeliveryVerdict, PoisonReason,
};
use crate::transport::{ControlRpc, OutputGrpcError, OutputProtocolError};

/// Complete application/ad-hoc command processor behind one command delivery
/// worker. Every field is an immutable process-owned dependency; no execution
/// path may resolve a second client or service locator after claim.
pub(super) struct AgentDeliveryProcessor<R, RC, T, K, D, I> {
    router: AgentDeliveryRouter<R, RC>,
    checkpoint_recovery: bool,
    node_recovery: bool,
    sandbox_stop: Option<Arc<dyn crate::sandbox::dispatch::SandboxStopDelivery>>,
    authenticator: Arc<dyn SignedCommandAuthenticator>,
    output: AgentOutputPreflight,
    control: Arc<AgentControlClient<R>>,
    retirer: Arc<CommandRetirer<RC>>,
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
    RC: CommandRetirementClient + 'static,
    T: AgentTerminalReplay + AgentProgressConnector + 'static,
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
        retirer: Arc<CommandRetirer<RC>>,
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
            checkpoint_recovery: false,
            node_recovery: false,
            sandbox_stop: None,
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

    pub(super) fn with_node_recovery(mut self, enabled: bool) -> Self {
        self.node_recovery = enabled;
        self
    }

    pub(super) fn with_checkpoint_recovery(mut self, enabled: bool) -> Self {
        self.checkpoint_recovery = enabled;
        self
    }

    /// Stop later native submissions. Normal process shutdown calls this only
    /// after command intake has stopped and all owned delivery futures drained.
    pub(super) fn stop(&self) -> Result<(), InvocationSupervisionError> {
        self.coordinator.stop()
    }

    /// Drain every accepted native invocation after delivery processing ends.
    pub(super) async fn close(&self) -> Result<(), InvocationSupervisionError> {
        self.coordinator.close().await
    }

    async fn process_owned(
        &self,
        delivery: CommandDelivery,
        verified: VerifiedAgentCommand,
    ) -> Result<AgentDeliveryProcessOutcome, AgentDeliveryProcessError> {
        if self.node_recovery {
            return self
                .process_node_recovery_verified(delivery, verified)
                .await;
        }
        if self.checkpoint_recovery {
            return self.process_checkpoint_verified(delivery, verified).await;
        }
        tracing::info!(event = "agent_delivery_routing_started");
        let route = self
            .router
            .route_verified(delivery, verified, self.clock.now_unix_millis())
            .await
            .map_err(AgentDeliveryProcessError::Delivery)?;
        tracing::info!(event = "agent_delivery_routed", route = ?route.kind());
        self.process_route(route).await
    }

    async fn process_route(
        &self,
        route: AgentDeliveryRoute,
    ) -> Result<AgentDeliveryProcessOutcome, AgentDeliveryProcessError> {
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

    /// Opt-in delivery composition. Bootstrap remains on the ordinary route
    /// until restored browser output and deployed recovery acceptance pass.
    pub(super) async fn process_checkpoint_verified(
        &self,
        delivery: CommandDelivery,
        verified: VerifiedAgentCommand,
    ) -> Result<AgentDeliveryProcessOutcome, AgentDeliveryProcessError>
    where
        T: AgentProgressConnector,
    {
        use super::agent_delivery::CheckpointDeliveryRoute;
        match self
            .router
            .route_checkpoint_verified(delivery, verified, self.clock.now_unix_millis())
            .await
            .map_err(AgentDeliveryProcessError::Delivery)?
        {
            CheckpointDeliveryRoute::Ordinary(route) => self.process_route(route).await,
            CheckpointDeliveryRoute::Inspect(recovery) => {
                self.process_checkpoint_delivery(*recovery).await
            }
        }
    }

    pub(super) async fn process_verified_delivery(
        &self,
        delivery: CommandDelivery,
        verified: VerifiedAgentCommand,
    ) -> DeliveryVerdict {
        let command = verified.command();
        let delivery_token = delivery.delivery_token().to_owned();
        let span = tracing::info_span!(
            parent: None,
            "agent.delivery",
            execution_kind = ?verified.kind(),
            execution_id = %command.execution_id,
            // FU-299: what a user quotes to support is the execution id.
            support_ref = %command.execution_id,
            root_execution_id = %command.root_execution_id,
            generation = command.generation,
            command_id = %command.command_id,
            tenant_id = %command.tenant_id,
            resource_project_id = %command.resource_project_id,
            projection_project_id = %command.projection_project_id,
            capability_id = %command.capability_id,
            parent_execution_id = %command.parent_execution_id,
            parent_call_id = %command.parent_call_id,
            nats_stream = %delivery.stream(),
            nats_stream_sequence = delivery.stream_sequence(),
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
                    Ok(outcome) => {
                        let verdict = outcome.verdict(&delivery_token);
                        outcome.record();
                        verdict
                    }
                    Err(error) => {
                        tracing::Span::current().record("outcome", "failed_no_ack");
                        tracing::Span::current().record("error_code", error.code());
                        tracing::Span::current().record("retryable", error.retryable());
                        tracing::warn!(
                            event = "agent_delivery_failed",
                            error_code = error.code(),
                            retryable = error.retryable(),
                        );
                        if error.terminal_frame_refused() {
                            // FU-290: one exact replay and one replacement are
                            // spent and Main still refuses the terminal. A NAK
                            // would redeliver it every retry delay forever, so
                            // the delivery is terminated and dead-lettered.
                            tracing::error!(
                                event = "agent_failure_terminal.canonical_refused",
                                error_code = error.code(),
                            );
                            DeliveryVerdict::Poison {
                                reason: PoisonReason::TerminalRefused,
                                delivery_token: Some(delivery_token),
                            }
                        } else {
                            DeliveryVerdict::Processed
                        }
                    }
                }
            }
            .instrument(span),
        )
        .await
    }

    async fn process_checkpoint_delivery(
        &self,
        recovery: super::agent_delivery::CheckpointAgentDelivery,
    ) -> Result<AgentDeliveryProcessOutcome, AgentDeliveryProcessError>
    where
        T: AgentProgressConnector,
    {
        let reservation = match self.admission.reserve().await {
            Ok(reservation) => reservation,
            Err(error) => {
                return Ok(AgentDeliveryProcessOutcome::retained(
                    error.code().as_str(),
                    error.retryable(),
                ));
            }
        };
        let output = self
            .output
            .prepare_checkpoint(&recovery)
            .await
            .map_err(AgentDeliveryProcessError::OutputPreflight)?;
        let Some(output) = output else {
            use super::output_delivery::CheckpointPendingOutput;
            match self
                .output
                .prepare_checkpoint_pending(recovery)
                .await
                .map_err(AgentDeliveryProcessError::OutputPreflight)?
            {
                CheckpointPendingOutput::Progress(recovery) => {
                    use super::output_delivery::CheckpointProgressReplay;
                    match self
                        .output
                        .replay_checkpoint_progress(*recovery, self.replay.as_ref())
                        .await
                        .map_err(AgentDeliveryProcessError::OutputPreflight)?
                    {
                        CheckpointProgressReplay::Settled => {
                            return Ok(AgentDeliveryProcessOutcome::retained(
                                "agent_delivery.checkpoint_output_reclaim",
                                true,
                            ));
                        }
                        CheckpointProgressReplay::Rejected { recovery, failure } => {
                            // Main refused this exact frame (a stop, the deadline,
                            // or the frame itself). Retrying cannot change that:
                            // settle the failure it selected, once, and retire.
                            self.settle_replacement(*recovery, failure).await?;
                            return Ok(AgentDeliveryProcessOutcome::completed(
                                "agent_delivery.rejected_progress_settled",
                            ));
                        }
                    }
                }
                CheckpointPendingOutput::Unrecognised { recovery, failure } => {
                    // The pending terminal's text is one this build does not
                    // recognise: set it aside, settle one canonical failure.
                    self.settle_replacement(*recovery, failure).await?;
                    return Ok(AgentDeliveryProcessOutcome::completed(
                        "agent_delivery.unrecognised_terminal_settled",
                    ));
                }
                CheckpointPendingOutput::Terminal(terminal) => {
                    recover_accepted_terminal(
                        self.control.clone(),
                        self.retirer.as_ref(),
                        self.replay.as_ref(),
                        *terminal,
                        self.clock.clone(),
                        self.preparation.lease_config(),
                        self.terminal_recovery,
                    )
                    .await
                    .map_err(AgentDeliveryProcessError::TerminalRecovery)?;
                    return Ok(AgentDeliveryProcessOutcome::completed(
                        "agent_delivery.checkpoint_terminal_retired",
                    ));
                }
            }
        };
        let waiter = self
            .coordinator
            .submit_checkpoint(
                recovery,
                output,
                reservation,
                self.input.clone(),
                self.preparation.lease_config(),
            )
            .map_err(AgentDeliveryProcessError::Supervision)?;
        let completion = waiter
            .wait()
            .await
            .map_err(AgentDeliveryProcessError::Supervision)?;
        Self::finish_invocation(completion)
    }

    /// Replace the one pending frame with `failure`, then replay, settle and
    /// retire that exact canonical terminal.
    async fn settle_replacement(
        &self,
        recovery: super::agent_delivery::CheckpointAgentDelivery,
        failure: RuntimeFailureKind,
    ) -> Result<(), AgentDeliveryProcessError> {
        let terminal = self
            .output
            .settle_rejected_checkpoint_progress(recovery, failure, self.clock.now_unix_millis())
            .await
            .map_err(AgentDeliveryProcessError::OutputPreflight)?;
        recover_accepted_terminal(
            self.control.clone(),
            self.retirer.as_ref(),
            self.replay.as_ref(),
            terminal,
            self.clock.clone(),
            self.preparation.lease_config(),
            self.terminal_recovery,
        )
        .await
        .map_err(AgentDeliveryProcessError::TerminalRecovery)?;
        Ok(())
    }

    async fn process_node_recovery_verified(
        &self,
        delivery: CommandDelivery,
        verified: VerifiedAgentCommand,
    ) -> Result<AgentDeliveryProcessOutcome, AgentDeliveryProcessError>
    where
        T: AgentProgressConnector,
    {
        use super::agent_delivery::NodeRecoveryDeliveryRoute;
        match self
            .router
            .route_node_recovery_verified(delivery, verified, self.clock.now_unix_millis())
            .await
            .map_err(AgentDeliveryProcessError::Delivery)?
        {
            NodeRecoveryDeliveryRoute::Ordinary(route) => self.process_route(route).await,
            NodeRecoveryDeliveryRoute::Model(recovery) => {
                self.process_checkpoint_delivery(*recovery).await
            }
            NodeRecoveryDeliveryRoute::Node(recovery) => {
                let reservation = match self.admission.reserve().await {
                    Ok(value) => value,
                    Err(error) => {
                        return Ok(AgentDeliveryProcessOutcome::retained(
                            error.code().as_str(),
                            error.retryable(),
                        ));
                    }
                };
                let output = match self
                    .output
                    .prepare_node_recovery(&recovery)
                    .await
                    .map_err(AgentDeliveryProcessError::OutputPreflight)?
                {
                    NodeRecoveryOutput::Ready(output) => *output,
                    other => {
                        let code = other
                            .retained_code()
                            .unwrap_or("node_recovery.output_replay_required");
                        return Ok(AgentDeliveryProcessOutcome::retained(code, true));
                    }
                };
                let waiter = self
                    .coordinator
                    .submit_node_recovery(
                        *recovery,
                        output,
                        reservation,
                        self.input.clone(),
                        self.preparation.lease_config(),
                    )
                    .map_err(AgentDeliveryProcessError::Supervision)?;
                let completion = waiter
                    .wait()
                    .await
                    .map_err(AgentDeliveryProcessError::Supervision)?;
                Self::finish_invocation(completion)
            }
        }
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
        if failure == RuntimeFailureKind::Cancelled
            && let Some(delivery) = &self.sandbox_stop
        {
            let Ok(authority) = recovery.sandbox_stop_authority() else {
                return Ok(AgentDeliveryProcessOutcome::retained(
                    "sandbox.stop_authority_invalid",
                    false,
                ));
            };
            if !crate::sandbox::dispatch::BoundSandboxStop::new(delivery.clone(), authority)
                .confirmed()
                .await
            {
                return Ok(AgentDeliveryProcessOutcome::retained(
                    "sandbox.stop_delivery_pending",
                    true,
                ));
            }
        }
        let recovery = match self
            .output
            .prepare_output_recovery(recovery, true)
            .await
            .map_err(AgentDeliveryProcessError::OutputPreflight)?
        {
            OutputRecovery::Retained(code) => {
                return Ok(AgentDeliveryProcessOutcome::retained(code, true));
            }
            OutputRecovery::CommittedTerminalUnvalidated => {
                // Main answered an output recovery for a frame its watermark
                // covers: it was just authenticated for this delivery, and the
                // verdict names this delivery's own token (like B).
                return Ok(AgentDeliveryProcessOutcome::DeadLettered {
                    code: UNRECOVERABLE_TERMINAL_CODE,
                    reason: PoisonReason::CommittedTerminalUnvalidated,
                });
            }
            OutputRecovery::Empty(recovery) => recovery,
            OutputRecovery::Terminal(terminal) => {
                // A terminal is already pending: replay that exact frame (a
                // refused one is replaced once, never joined by a second).
                recover_accepted_terminal(
                    Arc::clone(&self.control),
                    self.retirer.as_ref(),
                    self.replay.as_ref(),
                    *terminal,
                    Arc::clone(&self.clock),
                    self.preparation.lease_config(),
                    self.terminal_recovery,
                )
                .await
                .map_err(AgentDeliveryProcessError::TerminalRecovery)?;
                return Ok(AgentDeliveryProcessOutcome::completed(
                    "agent_delivery.output_recovery_terminal_retired",
                ));
            }
        };
        // A stop wins; otherwise an unrecognised pending failure keeps its kind.
        let failure = recovery
            .kept_failure()
            .filter(|_| failure != RuntimeFailureKind::Cancelled)
            .unwrap_or(failure);
        reconcile_empty_agent_output_recovery(
            Arc::clone(&self.control),
            self.retirer.as_ref(),
            self.replay.as_ref(),
            *recovery,
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
                        let lease_error = Box::pin(rejected.close())
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
impl<R, RC, T, K, D, I> CommandDeliveryProcessor for AgentDeliveryProcessor<R, RC, T, K, D, I>
where
    R: ControlRpc + 'static,
    RC: CommandRetirementClient + 'static,
    T: AgentTerminalReplay + AgentProgressConnector + 'static,
    K: UnixMillisClock,
    D: AuthorizedAgentLifecycle,
    I: AgentInputMaterializer + 'static,
{
    async fn process(&self, delivery: CommandDelivery) -> DeliveryVerdict {
        let verification = tracing::info_span!(
            "agent.delivery.verify",
            nats_stream = %delivery.stream(),
            nats_stream_sequence = delivery.stream_sequence(),
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
                let poison = verification_poison(&error);
                let error = AgentDeliveryProcessError::Delivery(error.into());
                verification.record("outcome", "poison");
                verification.record("error_code", error.code());
                verification.record("retryable", error.retryable());
                verification.in_scope(|| {
                    tracing::warn!(
                        event = "agent_delivery_verification_failed",
                        error_code = error.code(),
                        retryable = error.retryable(),
                    );
                });
                return DeliveryVerdict::poison(poison);
            }
        };
        if let Err(verdict) = delivery.bind_idempotency_key(&verified.command().idempotency_key) {
            verification.in_scope(|| {
                tracing::warn!(event = "agent_delivery_subject_mismatch");
            });
            return verdict;
        }
        self.process_verified_delivery(delivery, verified).await
    }
}

#[allow(clippy::too_many_arguments, clippy::type_complexity)]
pub(super) fn native_agent_delivery_processor<A, C, R, RC, K, I>(
    authenticator: Arc<dyn SignedCommandAuthenticator>,
    output: AgentOutputPreflight,
    control: Arc<AgentControlClient<R>>,
    retirer: Arc<CommandRetirer<RC>>,
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
    RC: CommandRetirementClient + 'static,
    K: UnixMillisClock,
    I: AgentInputMaterializer + 'static,
{
    let sandbox_stop = assembler.sandbox_stop_delivery();
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
    let mut processor = AgentDeliveryProcessor::new(
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
    );
    processor.sandbox_stop = sandbox_stop;
    processor
}

#[derive(Clone, Copy)]
pub(super) enum AgentDeliveryProcessOutcome {
    Completed {
        code: &'static str,
    },
    RetainedNoAck {
        code: &'static str,
        retryable: bool,
    },
    /// Dead-lettered: redelivery cannot change the outcome.
    DeadLettered {
        code: &'static str,
        reason: PoisonReason,
    },
}

impl AgentDeliveryProcessOutcome {
    const fn completed(code: &'static str) -> Self {
        Self::Completed { code }
    }

    const fn retained(code: &'static str, retryable: bool) -> Self {
        Self::RetainedNoAck { code, retryable }
    }

    fn verdict(&self, delivery_token: &str) -> DeliveryVerdict {
        match self {
            Self::DeadLettered { reason, .. } => DeliveryVerdict::Poison {
                reason: *reason,
                delivery_token: Some(delivery_token.to_owned()),
            },
            Self::Completed { .. } | Self::RetainedNoAck { .. } => DeliveryVerdict::Processed,
        }
    }

    fn record(self) {
        match self {
            Self::DeadLettered { code, reason } => {
                tracing::Span::current().record("outcome", "dead_lettered");
                tracing::Span::current().record("result_code", code);
                tracing::Span::current().record("retryable", false);
                tracing::error!(
                    event = "agent_delivery_dead_lettered",
                    result_code = code,
                    reason = reason.code(),
                );
            }
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
                elitea_faults::fault!(
                    tracing::Level::WARN,
                    code = elitea_faults::codes::AGENT_DELIVERY_RETAINED_NO_ACK,
                    event = "agent_delivery_retained_no_ack",
                    result_code = code,
                    retryable
                );
            }
        }
    }
}

pub(super) enum AgentDeliveryProcessError {
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
    /// Main refused the terminal output frame itself (bound, nonretryable).
    const fn terminal_frame_refused(&self) -> bool {
        matches!(
            self,
            Self::TerminalRecovery(AgentTerminalRecoveryError::Output(
                OutputGrpcError::Protocol(OutputProtocolError::FrameRefused)
            )) | Self::FailureTerminal(AgentFailureTerminalError::Output(
                OutputGrpcError::Protocol(OutputProtocolError::FrameRefused)
            ))
        )
    }

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

pub(super) const fn preflight_recovery_code(kind: AgentOutputRecoveryRequiredKind) -> &'static str {
    match kind {
        AgentOutputRecoveryRequiredKind::UnexpectedTerminal => FRESH_CLAIM_TERMINAL_UNEXPECTED_CODE,
        AgentOutputRecoveryRequiredKind::PendingProgress => {
            "agent_delivery.pending_progress_recovery"
        }
        AgentOutputRecoveryRequiredKind::ReconciledStaleProgress => {
            "agent_delivery.reconciled_progress_recovery"
        }
    }
}

/// FU-299: the retained-delivery outcome is a registered fault line.
#[cfg(test)]
mod fault_line_tests {
    use tracing_subscriber::layer::SubscriberExt as _;
    use tracing_subscriber::util::SubscriberInitExt as _;

    use super::AgentDeliveryProcessOutcome;

    const CHILD_ENV: &str = "ELITEA_RETAINED_NO_ACK_FAULT_LINE_CHILD";

    #[test]
    fn retained_delivery_is_one_registered_fault_line() {
        let output = elitea_faults::logtest::run_child(
            "execution::agent_delivery_processor::fault_line_tests::retained_delivery_child",
            CHILD_ENV,
        );
        let lines = output.operator_lines();
        assert_eq!(lines.len(), 1, "{}", output.stdout);
        let line = &lines[0];
        let code = elitea_faults::codes::AGENT_DELIVERY_RETAINED_NO_ACK;
        assert_eq!(line["level"], "WARN");
        assert_eq!(line["msg"], code.sentence());
        assert_eq!(line["code"], code.as_str());
        assert_eq!(line["action"], code.action());
        assert_eq!(line["event"], "agent_delivery_retained_no_ack");
        assert_eq!(line["result_code"], "agent_delivery.scripted_retry");
        assert_eq!(line["retryable"], true);
        assert_eq!(line["support_ref"], "exec-retained");
        assert_eq!(line["span"], "agent.delivery");
    }

    #[test]
    fn retained_delivery_child() {
        if !elitea_faults::logtest::is_child(CHILD_ENV) {
            return;
        }
        tracing_subscriber::registry()
            .with(
                crate::diagnostics::log_layer(Some("info"), std::io::stdout)
                    .expect("operator stream"),
            )
            .init();
        let span = tracing::info_span!(
            "agent.delivery",
            execution_id = "exec-retained",
            support_ref = "exec-retained",
            outcome = tracing::field::Empty,
            result_code = tracing::field::Empty,
            retryable = tracing::field::Empty,
        );
        span.in_scope(|| {
            AgentDeliveryProcessOutcome::retained("agent_delivery.scripted_retry", true).record();
        });
        elitea_faults::logtest::child_ran();
    }
}

//! Supervisor-owned native ADK execution and durable output composition.
//!
//! This module is deliberately capability-disabled. It proves the single-owner
//! lifecycle shared by direct application/ad-hoc agents and stored pipelines
//! before production process bootstrap and capability registration are admitted.

#![allow(dead_code)] // Production capability registration remains fail-closed.

use std::future::Future;
use std::pin::Pin;
use std::sync::Arc;

use chrono::{DateTime, Utc};
use serde_json::{Map, Value};
use tracing::Instrument as _;

use super::agent_invocation::{
    AgentAuthorizedLifecycleCompletion, AgentAuthorizedLifecycleDisposition,
    AuthorizedAgentLifecycle, OwnedFuture,
};
use super::agent_lease::{ClaimLeaseError, ClaimLeaseStateProbe, UnixMillisClock};
use super::agent_preparation::{
    AgentNativeAssemblyOutcome, AssembledAuthorizedAgentRun, AuthorizedAgentRun,
    CursorBoundAuthorizedAgentRun, StartedAuthorizedAgentRun,
};
use super::output_delivery::{
    AgentProgressConnector, AgentProgressPublishError, AgentProgressPublishOutcome,
    AgentTerminalRecoveryConfig, AgentTerminalReplay, FreshAgentTerminalSelection,
};
use crate::agents::events::{
    AgentEventProjectionError, AgentEventProjectionErrorCode, AgentEventProjector,
    ProjectedAgentEventBatch,
};
use crate::agents::runtime::{
    NativeAgentAssembler, NativeAgentAssemblyError, NativeAgentAssemblyErrorCode, NativeAgentRun,
};
use crate::protocol::control::AgentControlClient;
use crate::protocol::elitea::runtime::v1::NodeEventV1;
use crate::protocol::node_event::encode_current_node_event_json;
use crate::protocol::output::RuntimeFailureKind;
use crate::transport::ControlRpc;
use crate::transport::redis_commands::{RedisCommandRetirer, RedisRetirementClient};

/// Capability-disabled concrete lifecycle shared by both native runtime modes.
pub(super) struct NativeAuthorizedAgentLifecycle<A, C, R, RC, K> {
    native_factory: Arc<A>,
    connector: C,
    control: Arc<AgentControlClient<R>>,
    retirer: Arc<RedisCommandRetirer<RC>>,
    clock: Arc<K>,
    max_output_sessions: usize,
    terminal_recovery: AgentTerminalRecoveryConfig,
}

impl<A, C, R, RC, K> NativeAuthorizedAgentLifecycle<A, C, R, RC, K> {
    #[allow(clippy::too_many_arguments)] // Explicit owners are safer than a service locator.
    pub(super) const fn new(
        native_factory: Arc<A>,
        connector: C,
        control: Arc<AgentControlClient<R>>,
        retirer: Arc<RedisCommandRetirer<RC>>,
        clock: Arc<K>,
        max_output_sessions: usize,
        terminal_recovery: AgentTerminalRecoveryConfig,
    ) -> Self {
        Self {
            native_factory,
            connector,
            control,
            retirer,
            clock,
            max_output_sessions,
            terminal_recovery,
        }
    }
}

impl<A, C, R, RC, K> AuthorizedAgentLifecycle for NativeAuthorizedAgentLifecycle<A, C, R, RC, K>
where
    A: NativeAgentAssembler,
    C: AgentProgressConnector + AgentTerminalReplay + Clone + Send + Sync + 'static,
    R: ControlRpc + 'static,
    RC: RedisRetirementClient + 'static,
    K: UnixMillisClock,
{
    fn run(&self, run: AuthorizedAgentRun) -> OwnedFuture<AgentAuthorizedLifecycleCompletion> {
        let execution_kind = run.execution_kind();
        let native_factory = Arc::clone(&self.native_factory);
        let connector = self.connector.clone();
        let control = Arc::clone(&self.control);
        let retirer = Arc::clone(&self.retirer);
        let clock = Arc::clone(&self.clock);
        let max_output_sessions = self.max_output_sessions;
        let terminal_recovery = self.terminal_recovery;
        // Box the generated state machine before adding instrumentation. The
        // lifecycle deliberately crosses several ownership-heavy async phases;
        // keeping the poll frame behind this allocation prevents its stack size
        // from growing with every additional durable boundary.
        let span = tracing::info_span!(
            "agent.native_lifecycle",
            execution_kind = ?execution_kind,
            tenant_id = %run.trace_tenant_id(),
            resource_project_id = %run.trace_resource_project_id(),
            execution_id = %run.trace_execution_id(),
            generation = run.trace_generation(),
            command_id = %run.trace_command_id(),
            outcome = tracing::field::Empty,
            error_code = tracing::field::Empty,
            retryable = tracing::field::Empty,
            terminal_sequence = tracing::field::Empty,
        );
        let operation: OwnedFuture<AgentAuthorizedLifecycleCompletion> = Box::pin(
            execute_owned(
                run,
                native_factory,
                connector,
                control,
                retirer,
                clock,
                max_output_sessions,
                terminal_recovery,
            )
            .instrument(span.clone()),
        );
        Box::pin(async move {
            let completion = operation.await;
            match completion.disposition() {
                AgentAuthorizedLifecycleDisposition::ExecutedSettledAcked { sequence, .. } => {
                    span.record("outcome", "executed_settled_acked");
                    span.record("terminal_sequence", sequence);
                }
                AgentAuthorizedLifecycleDisposition::RecoveryRequiredNoAck { code, retryable } => {
                    span.record("outcome", "recovery_required_no_ack");
                    span.record("error_code", code);
                    span.record("retryable", retryable);
                }
            }
            completion
        })
    }
}

#[allow(clippy::too_many_arguments, clippy::too_many_lines)]
// Keeping the state transitions linear makes the authority handoff auditable.
async fn execute_owned<A, C, R, RC, K>(
    run: AuthorizedAgentRun,
    native_factory: Arc<A>,
    connector: C,
    control: Arc<AgentControlClient<R>>,
    retirer: Arc<RedisCommandRetirer<RC>>,
    clock: Arc<K>,
    max_output_sessions: usize,
    terminal_recovery: AgentTerminalRecoveryConfig,
) -> AgentAuthorizedLifecycleCompletion
where
    A: NativeAgentAssembler,
    C: AgentProgressConnector + AgentTerminalReplay + Send + Sync + 'static,
    R: ControlRpc + 'static,
    RC: RedisRetirementClient + 'static,
    K: UnixMillisClock,
{
    tracing::info!(event = "agent_native_lifecycle_started");
    let run = match run.bind_progress_publisher(connector, max_output_sessions) {
        Ok(run) => {
            tracing::info!(event = "agent_progress_publisher_bound");
            run
        }
        Err(failure) => {
            let (run, _connector, error) = failure.into_parts();
            return Box::pin(run.close_no_ack(error.code(), error.retryable())).await;
        }
    };

    match pre_start_boundary(&run, clock.as_ref()) {
        LifecycleBoundary::Continue => {}
        LifecycleBoundary::Terminal(failure) => {
            return Box::pin(finalize(
                run,
                failure,
                control,
                retirer,
                clock,
                terminal_recovery,
            ))
            .await;
        }
        LifecycleBoundary::RecoveryRequired { code, retryable } => {
            return Box::pin(run.close_no_ack(code, retryable)).await;
        }
    }

    tracing::info!(event = "agent_native_assembly_started");
    let mut native_assembly = match Box::pin(run.assemble_native(native_factory.as_ref())).await {
        AgentNativeAssemblyOutcome::Assembled(native_assembly) => {
            tracing::info!(event = "agent_native_assembly_completed");
            *native_assembly
        }
        AgentNativeAssemblyOutcome::Failed { run, error } => {
            let run = *run;
            let failure = assembly_failure(&error);
            tracing::warn!(
                error_code = error.code().as_str(),
                "native agent assembly failed after invocation authorization"
            );
            return Box::pin(finalize(
                run,
                failure,
                control,
                retirer,
                clock,
                terminal_recovery,
            ))
            .await;
        }
        AgentNativeAssemblyOutcome::Lease {
            run,
            error: ClaimLeaseError::Cancelled(_),
        } => {
            let run = *run;
            return Box::pin(finalize(
                run,
                RuntimeFailureKind::Cancelled,
                control,
                retirer,
                clock,
                terminal_recovery,
            ))
            .await;
        }
        AgentNativeAssemblyOutcome::Lease { run, error } => {
            let run = *run;
            return Box::pin(run.close_no_ack(error.code().as_str(), error.retryable())).await;
        }
    };

    tracing::info!(event = "agent_start_event_projection_started");
    let start_time = match sampled_time(clock.as_ref()) {
        Ok((_, time)) => time,
        Err(code) => return native_assembly.into_run().close_no_ack(code, false).await,
    };
    let start_batch = match native_assembly.project_start(start_time) {
        Ok(batch) => batch,
        Err(error) => {
            let run = native_assembly.into_run();
            let failure = projection_failure(&error);
            tracing::warn!(
                error_code = error.code().as_str(),
                "native agent start-event projection failed"
            );
            return Box::pin(finalize(
                run,
                failure,
                control,
                retirer,
                clock,
                terminal_recovery,
            ))
            .await;
        }
    };
    tracing::info!(event = "agent_start_event_publication_started");
    match Box::pin(publish_assembled_batch(
        &mut native_assembly,
        start_batch,
        clock.as_ref(),
    ))
    .await
    {
        BatchPublication::Acknowledged => {
            tracing::info!(event = "agent_start_event_published");
        }
        BatchPublication::Rejected => {
            let run = native_assembly.into_run();
            return Box::pin(finalize(
                run,
                RuntimeFailureKind::Cancelled,
                control,
                retirer,
                clock,
                terminal_recovery,
            ))
            .await;
        }
        BatchPublication::RecoveryRequired { code, retryable } => {
            return Box::pin(native_assembly.into_run().close_no_ack(code, retryable)).await;
        }
    }

    if let Err(error) = native_assembly.ensure_lease_running() {
        let run = native_assembly.into_run();
        return Box::pin(finish_lease_boundary(
            run,
            error,
            control,
            retirer,
            clock,
            terminal_recovery,
        ))
        .await;
    }
    match deadline_reached_assembled(&native_assembly, clock.as_ref()) {
        Ok(false) => {}
        Ok(true) => {
            let run = native_assembly.into_run();
            return Box::pin(finalize(
                run,
                RuntimeFailureKind::DeadlineExceeded,
                control,
                retirer,
                clock,
                terminal_recovery,
            ))
            .await;
        }
        Err(code) => {
            return native_assembly.into_run().close_no_ack(code, false).await;
        }
    }

    tracing::info!(event = "agent_runner_start_started");
    let started = match native_assembly.start() {
        Ok(started) => {
            tracing::info!(event = "agent_runner_started");
            started
        }
        Err(failure) => {
            let (run, error) = (*failure).into_parts();
            tracing::warn!(
                error_code = error.code().as_str(),
                upstream_error_code = error.upstream_code(),
                "native agent runtime failed to start"
            );
            return Box::pin(finalize(
                run,
                RuntimeFailureKind::Internal,
                control,
                retirer,
                clock,
                terminal_recovery,
            ))
            .await;
        }
    };
    Box::pin(execute_started(
        started,
        control,
        retirer,
        clock,
        terminal_recovery,
    ))
    .await
}

/// Own the post-start stream and terminal phases in a separate boxed future.
///
/// The authorized pre-start path already owns several large generic futures.
/// Keeping the live ADK stream phase out of that poll frame prevents each new
/// durable pause boundary from increasing the executor thread's stack demand.
#[allow(clippy::too_many_lines)] // Keep pause ACK, terminal selection and retirement authority linear.
async fn execute_started<C, S, R, RC, K>(
    started: StartedAuthorizedAgentRun<C, S>,
    control: Arc<AgentControlClient<R>>,
    retirer: Arc<RedisCommandRetirer<RC>>,
    clock: Arc<K>,
    terminal_recovery: AgentTerminalRecoveryConfig,
) -> AgentAuthorizedLifecycleCompletion
where
    C: AgentProgressConnector + AgentTerminalReplay,
    S: crate::agents::runtime::NativeAgentCompletionSelector,
    R: ControlRpc + 'static,
    RC: RedisRetirementClient + 'static,
    K: UnixMillisClock,
{
    let (mut run, mut native, mut projector, completion_selector) = started.into_parts();

    let mut probe = run.lease_state_probe();
    let mut pauses = AgentPauseAccumulator::default();
    tracing::info!(event = "agent_event_stream_started");
    let stream = Box::pin(drive_native_stream(
        &mut run,
        &mut native,
        &mut projector,
        &mut pauses,
        &mut probe,
        clock.as_ref(),
    ))
    .await;
    tracing::info!(
        event = "agent_event_stream_completed",
        stream_disposition = stream_disposition(&stream),
    );
    let mut successful_terminal = FreshAgentTerminalSelection::Completed;
    let mut failure = match stream {
        NativeStreamOutcome::Eos => None,
        NativeStreamOutcome::Paused => {
            let pause = match pauses.finish() {
                Ok(pause) => pause,
                Err(error) => {
                    return Box::pin(run.close_no_ack(error.code(), false)).await;
                }
            };
            let occurred_at_unix_millis = clock.now_unix_millis();
            if occurred_at_unix_millis <= 0 {
                return Box::pin(run.close_no_ack("agent_lifecycle.invalid_clock", false)).await;
            }
            successful_terminal = pause.selection;
            match Box::pin(run.publish_result_event(pause.event, occurred_at_unix_millis)).await {
                Ok(AgentProgressPublishOutcome::Acknowledged { .. }) => None,
                Ok(AgentProgressPublishOutcome::Rejected { .. }) => {
                    Some(RuntimeFailureKind::Cancelled)
                }
                Err(error) => {
                    return Box::pin(run.close_no_ack(error.code(), error.retryable())).await;
                }
            }
        }
        NativeStreamOutcome::Failure(failure) => Some(failure),
        NativeStreamOutcome::RecoveryRequired { code, retryable } => {
            return Box::pin(run.close_no_ack(code, retryable)).await;
        }
        NativeStreamOutcome::FatalLease(error) => {
            return Box::pin(run.close_no_ack(error.code().as_str(), error.retryable())).await;
        }
    };

    if failure.is_none() && matches!(successful_terminal, FreshAgentTerminalSelection::Completed) {
        tracing::info!(event = "agent_completion_selection_started");
        let (next_run, completion_result) =
            Box::pin(run.select_native_completion(completion_selector)).await;
        run = next_run;
        let completion = match completion_result {
            Ok(Ok(completion)) => completion,
            Ok(Err(error)) => {
                tracing::warn!(
                    error_code = error.code().as_str(),
                    "native agent completion selection failed"
                );
                failure = Some(assembly_failure(&error));
                return Box::pin(finish_after_stream(
                    run,
                    failure,
                    FreshAgentTerminalSelection::Completed,
                    control,
                    retirer,
                    clock,
                    terminal_recovery,
                ))
                .await;
            }
            Err(ClaimLeaseError::Cancelled(_)) => {
                return Box::pin(finalize(
                    run,
                    RuntimeFailureKind::Cancelled,
                    control,
                    retirer,
                    clock,
                    terminal_recovery,
                ))
                .await;
            }
            Err(error) => {
                return Box::pin(run.close_no_ack(error.code().as_str(), error.retryable())).await;
            }
        };
        tracing::info!(event = "agent_completion_selected");
        let finish_time = match sampled_time(clock.as_ref()) {
            Ok((_, time)) => time,
            Err(code) => return run.close_no_ack(code, false).await,
        };
        let batch = match projector.finish_after_eos(completion, finish_time) {
            Ok(batch) => batch,
            Err(error) => {
                tracing::warn!(
                    error_code = error.code().as_str(),
                    "native agent completion projection failed"
                );
                failure = Some(projection_failure(&error));
                return Box::pin(finish_after_stream(
                    run,
                    failure,
                    FreshAgentTerminalSelection::Completed,
                    control,
                    retirer,
                    clock,
                    terminal_recovery,
                ))
                .await;
            }
        };
        match Box::pin(publish_batch(&mut run, batch, &mut pauses, clock.as_ref())).await {
            BatchPublication::Acknowledged => {}
            BatchPublication::Rejected => {
                failure = Some(RuntimeFailureKind::Cancelled);
            }
            BatchPublication::RecoveryRequired { code, retryable } => {
                return Box::pin(run.close_no_ack(code, retryable)).await;
            }
        }
    }

    Box::pin(finish_after_stream(
        run,
        failure,
        successful_terminal,
        control,
        retirer,
        clock,
        terminal_recovery,
    ))
    .await
}

enum NativeStreamOutcome {
    Eos,
    Paused,
    Failure(RuntimeFailureKind),
    RecoveryRequired { code: &'static str, retryable: bool },
    FatalLease(ClaimLeaseError),
}

const fn stream_disposition(outcome: &NativeStreamOutcome) -> &'static str {
    match outcome {
        NativeStreamOutcome::Eos => "eos",
        NativeStreamOutcome::Paused => "paused",
        NativeStreamOutcome::Failure(_) => "failed",
        NativeStreamOutcome::RecoveryRequired { .. } => "recovery_required",
        NativeStreamOutcome::FatalLease(_) => "lease_failed",
    }
}

async fn drive_native_stream<C, K>(
    run: &mut CursorBoundAuthorizedAgentRun<C>,
    native: &mut NativeAgentRun,
    projector: &mut AgentEventProjector,
    pauses: &mut AgentPauseAccumulator,
    probe: &mut ClaimLeaseStateProbe,
    clock: &K,
) -> NativeStreamOutcome
where
    C: AgentProgressConnector,
    K: UnixMillisClock,
{
    let mut failure = None;
    loop {
        tokio::select! {
            biased;
            lease = probe.wait_for_change() => {
                match lease {
                    Err(ClaimLeaseError::Cancelled(_)) => {
                        failure = Some(RuntimeFailureKind::Cancelled);
                        let _requested = native.request_stop();
                    }
                    Err(error) => {
                        let _requested = native.request_stop();
                        return NativeStreamOutcome::FatalLease(error);
                    }
                    Ok(()) => {}
                }
            }
            event = native.next_event() => {
                let event = match event {
                    Ok(Some(event)) => event,
                    Ok(None) => {
                        if let Some(failure) = failure {
                            return NativeStreamOutcome::Failure(failure);
                        }
                        return if projector.is_paused() {
                            NativeStreamOutcome::Paused
                        } else {
                            NativeStreamOutcome::Eos
                        };
                    }
                    Err(error) => {
                        tracing::warn!(
                            error = %error,
                            error_code = error.code().as_str(),
                            upstream_error_code = error.upstream_code(),
                            "native agent event stream failed"
                        );
                        return NativeStreamOutcome::Failure(
                            failure.unwrap_or(RuntimeFailureKind::Internal),
                        );
                    }
                };
                if failure.is_some() {
                    continue;
                }
                let batch = match projector.project(&event) {
                    Ok(batch) => batch,
                    Err(error) => {
                        tracing::warn!(
                            error = %error,
                            error_code = error.code().as_str(),
                            "native agent event projection failed"
                        );
                        failure = Some(projection_failure(&error));
                        let _requested = native.request_stop();
                        continue;
                    }
                };
                match Box::pin(publish_batch(run, batch, pauses, clock)).await {
                    BatchPublication::Acknowledged => {
                        if let Err(error) = probe.ensure_running() {
                            match error {
                                ClaimLeaseError::Cancelled(_) => {
                                    failure = Some(RuntimeFailureKind::Cancelled);
                                    let _requested = native.request_stop();
                                }
                                other => {
                                    let _requested = native.request_stop();
                                    return NativeStreamOutcome::FatalLease(other);
                                }
                            }
                        }
                    }
                    BatchPublication::Rejected => {
                        failure = Some(RuntimeFailureKind::Cancelled);
                        let _requested = native.request_stop();
                    }
                    BatchPublication::RecoveryRequired { code, retryable } => {
                        let _requested = native.request_stop();
                        return NativeStreamOutcome::RecoveryRequired { code, retryable };
                    }
                }
            }
        }
    }
}

enum BatchPublication {
    Acknowledged,
    Rejected,
    RecoveryRequired { code: &'static str, retryable: bool },
}

async fn publish_batch<C, K>(
    run: &mut CursorBoundAuthorizedAgentRun<C>,
    batch: ProjectedAgentEventBatch,
    pauses: &mut AgentPauseAccumulator,
    clock: &K,
) -> BatchPublication
where
    C: AgentProgressConnector,
    K: UnixMillisClock,
{
    for event in batch {
        match run.ensure_lease_running() {
            Ok(()) => {}
            Err(ClaimLeaseError::Cancelled(_)) => return BatchPublication::Rejected,
            Err(error) => {
                return BatchPublication::RecoveryRequired {
                    code: error.code().as_str(),
                    retryable: error.retryable(),
                };
            }
        }
        let occurred_at_unix_millis = clock.now_unix_millis();
        if occurred_at_unix_millis <= 0 {
            return BatchPublication::RecoveryRequired {
                code: "agent_lifecycle.invalid_clock",
                retryable: false,
            };
        }
        let is_pause = match pauses.observe(&event) {
            Ok(is_pause) => is_pause,
            Err(error) => {
                return BatchPublication::RecoveryRequired {
                    code: error.code(),
                    retryable: false,
                };
            }
        };
        let publication: Pin<
            Box<
                dyn Future<Output = Result<AgentProgressPublishOutcome, AgentProgressPublishError>>
                    + Send
                    + '_,
            >,
        > = if is_pause {
            Box::pin(run.publish_pause_progress(event, occurred_at_unix_millis))
        } else if event.r#type == "full_message" {
            Box::pin(run.publish_result_event(event, occurred_at_unix_millis))
        } else {
            Box::pin(run.publish_progress(event, occurred_at_unix_millis))
        };
        let result = publication.await;
        match result {
            Ok(AgentProgressPublishOutcome::Acknowledged { .. }) => {}
            Ok(AgentProgressPublishOutcome::Rejected { .. }) => {
                return BatchPublication::Rejected;
            }
            Err(error) => {
                return BatchPublication::RecoveryRequired {
                    code: error.code(),
                    retryable: error.retryable(),
                };
            }
        }
    }
    BatchPublication::Acknowledged
}

const MAX_PENDING_PAUSE_CARDS: usize = 16;

#[derive(Clone, Copy, Debug)]
pub(super) enum AgentPauseAggregationError {
    InvalidState,
    ResourceExhausted,
    MixedGuardrails,
    InvalidOutput,
}

impl AgentPauseAggregationError {
    const fn code(self) -> &'static str {
        match self {
            Self::InvalidState => "agent_pause.invalid_state",
            Self::ResourceExhausted => "agent_pause.resource_exhausted",
            Self::MixedGuardrails => "agent_pause.mixed_guardrails_unsupported",
            Self::InvalidOutput => "agent_pause.invalid_output",
        }
    }
}

pub(super) struct AggregatedAgentPause {
    pub(super) event: NodeEventV1,
    pub(super) selection: FreshAgentTerminalSelection,
}

/// Collects already-sanitized pause cards after their complete hierarchy has
/// been projected. Each card is published as ordinary progress immediately;
/// this owner creates the sole aggregate result event only at ADK EOS.
#[derive(Default)]
pub(super) struct AgentPauseAccumulator {
    hitl_event: Option<NodeEventV1>,
    hitl_interrupts: Vec<Value>,
    authorization_event: Option<NodeEventV1>,
    authorization_requests: Vec<Value>,
}

impl AgentPauseAccumulator {
    pub(super) fn observe(
        &mut self,
        event: &NodeEventV1,
    ) -> Result<bool, AgentPauseAggregationError> {
        match event.r#type.as_str() {
            "agent_hitl_interrupt" => {
                self.observe_hitl(event)?;
                Ok(true)
            }
            "mcp_authorization_required" => {
                self.observe_authorization(event)?;
                Ok(true)
            }
            _ => Ok(false),
        }
    }

    fn observe_hitl(&mut self, event: &NodeEventV1) -> Result<(), AgentPauseAggregationError> {
        let metadata = pause_metadata(event)?;
        let interrupts = metadata
            .get("hitl_interrupts")
            .and_then(Value::as_array)
            .filter(|interrupts| !interrupts.is_empty())
            .ok_or(AgentPauseAggregationError::InvalidState)?;
        for interrupt in interrupts {
            let identity = interrupt
                .get("interrupt_id")
                .and_then(Value::as_str)
                .filter(|identity| !identity.is_empty())
                .ok_or(AgentPauseAggregationError::InvalidState)?;
            if self.hitl_interrupts.iter().any(|pending| {
                pending.get("interrupt_id").and_then(Value::as_str) == Some(identity)
            }) {
                return Err(AgentPauseAggregationError::InvalidState);
            }
            self.ensure_capacity()?;
            self.hitl_interrupts.push(interrupt.clone());
        }
        if self.hitl_event.is_none() {
            self.hitl_event = Some(event.clone());
        }
        Ok(())
    }

    fn observe_authorization(
        &mut self,
        event: &NodeEventV1,
    ) -> Result<(), AgentPauseAggregationError> {
        let mut request = pause_metadata(event)?;
        if request.remove("authorization_requests").is_some() {
            return Err(AgentPauseAggregationError::InvalidState);
        }
        let identity =
            authorization_identity(&request).ok_or(AgentPauseAggregationError::InvalidState)?;
        if self.authorization_requests.iter().any(|pending| {
            pending
                .as_object()
                .and_then(authorization_identity)
                .is_some_and(|pending| pending == identity)
        }) {
            return Err(AgentPauseAggregationError::InvalidState);
        }
        self.ensure_capacity()?;
        self.authorization_requests.push(Value::Object(request));
        self.authorization_event = Some(event.clone());
        Ok(())
    }

    fn ensure_capacity(&self) -> Result<(), AgentPauseAggregationError> {
        if self.hitl_interrupts.len() + self.authorization_requests.len() >= MAX_PENDING_PAUSE_CARDS
        {
            Err(AgentPauseAggregationError::ResourceExhausted)
        } else {
            Ok(())
        }
    }

    pub(super) fn finish(&mut self) -> Result<AggregatedAgentPause, AgentPauseAggregationError> {
        match (
            self.hitl_interrupts.is_empty(),
            self.authorization_requests.is_empty(),
        ) {
            (true, true) => Err(AgentPauseAggregationError::InvalidState),
            (false, false) => Err(AgentPauseAggregationError::MixedGuardrails),
            (false, true) => self.finish_hitl(),
            (true, false) => self.finish_authorization(),
        }
    }

    fn finish_hitl(&mut self) -> Result<AggregatedAgentPause, AgentPauseAggregationError> {
        let mut event = self
            .hitl_event
            .take()
            .ok_or(AgentPauseAggregationError::InvalidState)?;
        let primary = self
            .hitl_interrupts
            .first()
            .cloned()
            .ok_or(AgentPauseAggregationError::InvalidState)?;
        let mut metadata = pause_metadata(&event)?;
        metadata.insert("hitl_interrupt".to_owned(), primary);
        metadata.insert(
            "hitl_interrupts".to_owned(),
            Value::Array(std::mem::take(&mut self.hitl_interrupts)),
        );
        bind_pause_metadata(&mut event, metadata)?;
        Ok(AggregatedAgentPause {
            event,
            selection: FreshAgentTerminalSelection::PausedHitl,
        })
    }

    fn finish_authorization(&mut self) -> Result<AggregatedAgentPause, AgentPauseAggregationError> {
        let mut event = self
            .authorization_event
            .take()
            .ok_or(AgentPauseAggregationError::InvalidState)?;
        let mut metadata = pause_metadata(&event)?;
        metadata.insert(
            "authorization_requests".to_owned(),
            Value::Array(std::mem::take(&mut self.authorization_requests)),
        );
        bind_pause_metadata(&mut event, metadata)?;
        Ok(AggregatedAgentPause {
            event,
            selection: FreshAgentTerminalSelection::PausedMcpAuth,
        })
    }
}

fn pause_metadata(event: &NodeEventV1) -> Result<Map<String, Value>, AgentPauseAggregationError> {
    serde_json::from_slice::<Value>(&event.response_metadata)
        .ok()
        .and_then(|metadata| metadata.as_object().cloned())
        .ok_or(AgentPauseAggregationError::InvalidState)
}

fn authorization_identity(metadata: &Map<String, Value>) -> Option<&str> {
    ["interrupt_id", "tool_run_id", "tool_call_id"]
        .into_iter()
        .find_map(|key| metadata.get(key).and_then(Value::as_str))
        .filter(|identity| !identity.is_empty())
}

fn bind_pause_metadata(
    event: &mut NodeEventV1,
    metadata: Map<String, Value>,
) -> Result<(), AgentPauseAggregationError> {
    event.response_metadata = serde_json::to_vec(&Value::Object(metadata))
        .map_err(|_| AgentPauseAggregationError::InvalidOutput)?;
    encode_current_node_event_json(event)
        .map(|_| ())
        .map_err(|_| AgentPauseAggregationError::InvalidOutput)
}

async fn publish_assembled_batch<C, S, K>(
    run: &mut AssembledAuthorizedAgentRun<C, S>,
    batch: ProjectedAgentEventBatch,
    clock: &K,
) -> BatchPublication
where
    C: AgentProgressConnector,
    K: UnixMillisClock,
{
    for event in batch {
        match run.ensure_lease_running() {
            Ok(()) => {}
            Err(ClaimLeaseError::Cancelled(_)) => return BatchPublication::Rejected,
            Err(error) => {
                return BatchPublication::RecoveryRequired {
                    code: error.code().as_str(),
                    retryable: error.retryable(),
                };
            }
        }
        let occurred_at_unix_millis = clock.now_unix_millis();
        if occurred_at_unix_millis <= 0 {
            return BatchPublication::RecoveryRequired {
                code: "agent_lifecycle.invalid_clock",
                retryable: false,
            };
        }
        let is_result = matches!(
            event.r#type.as_str(),
            "full_message" | "agent_hitl_interrupt" | "mcp_authorization_required"
        );
        let result = if is_result {
            run.publish_result_event(event, occurred_at_unix_millis)
                .await
        } else {
            run.publish_progress(event, occurred_at_unix_millis).await
        };
        match result {
            Ok(AgentProgressPublishOutcome::Acknowledged { .. }) => {}
            Ok(AgentProgressPublishOutcome::Rejected { .. }) => {
                return BatchPublication::Rejected;
            }
            Err(error) => {
                return BatchPublication::RecoveryRequired {
                    code: error.code(),
                    retryable: error.retryable(),
                };
            }
        }
    }
    BatchPublication::Acknowledged
}

async fn finish_after_stream<C, R, RC, K>(
    run: CursorBoundAuthorizedAgentRun<C>,
    mut failure: Option<RuntimeFailureKind>,
    successful_terminal: FreshAgentTerminalSelection,
    control: Arc<AgentControlClient<R>>,
    retirer: Arc<RedisCommandRetirer<RC>>,
    clock: Arc<K>,
    terminal_recovery: AgentTerminalRecoveryConfig,
) -> AgentAuthorizedLifecycleCompletion
where
    C: AgentProgressConnector + AgentTerminalReplay,
    R: ControlRpc + 'static,
    RC: RedisRetirementClient + 'static,
    K: UnixMillisClock,
{
    tracing::info!(event = "agent_terminal_publication_started");
    let (run, lease_result) = run.check_lease_now().await;
    match lease_result {
        Ok(()) => {}
        Err(ClaimLeaseError::Cancelled(_)) => failure = Some(RuntimeFailureKind::Cancelled),
        Err(error) => {
            return run
                .close_no_ack(error.code().as_str(), error.retryable())
                .await;
        }
    }
    let occurred_at_unix_millis = match sampled_time(clock.as_ref()) {
        Ok((now, _)) => now,
        Err(code) => return run.close_no_ack(code, false).await,
    };
    if failure != Some(RuntimeFailureKind::Cancelled)
        && occurred_at_unix_millis >= run.deadline_unix_millis()
    {
        failure = Some(RuntimeFailureKind::DeadlineExceeded);
    }
    let selection = failure.map_or(successful_terminal, FreshAgentTerminalSelection::Failure);
    let completion = Box::pin(run.finish_terminal(
        selection,
        occurred_at_unix_millis,
        control,
        retirer,
        terminal_recovery,
    ))
    .await;
    tracing::info!(event = "agent_terminal_publication_completed");
    completion
}

async fn finish_lease_boundary<C, R, RC, K>(
    run: CursorBoundAuthorizedAgentRun<C>,
    error: ClaimLeaseError,
    control: Arc<AgentControlClient<R>>,
    retirer: Arc<RedisCommandRetirer<RC>>,
    clock: Arc<K>,
    terminal_recovery: AgentTerminalRecoveryConfig,
) -> AgentAuthorizedLifecycleCompletion
where
    C: AgentProgressConnector + AgentTerminalReplay,
    R: ControlRpc + 'static,
    RC: RedisRetirementClient + 'static,
    K: UnixMillisClock,
{
    match error {
        ClaimLeaseError::Cancelled(_) => {
            finalize(
                run,
                RuntimeFailureKind::Cancelled,
                control,
                retirer,
                clock,
                terminal_recovery,
            )
            .await
        }
        error => {
            run.close_no_ack(error.code().as_str(), error.retryable())
                .await
        }
    }
}

async fn finalize<C, R, RC, K>(
    run: CursorBoundAuthorizedAgentRun<C>,
    failure: RuntimeFailureKind,
    control: Arc<AgentControlClient<R>>,
    retirer: Arc<RedisCommandRetirer<RC>>,
    clock: Arc<K>,
    terminal_recovery: AgentTerminalRecoveryConfig,
) -> AgentAuthorizedLifecycleCompletion
where
    C: AgentProgressConnector + AgentTerminalReplay,
    R: ControlRpc + 'static,
    RC: RedisRetirementClient + 'static,
    K: UnixMillisClock,
{
    finish_after_stream(
        run,
        Some(failure),
        FreshAgentTerminalSelection::Completed,
        control,
        retirer,
        clock,
        terminal_recovery,
    )
    .await
}

enum LifecycleBoundary {
    Continue,
    Terminal(RuntimeFailureKind),
    RecoveryRequired { code: &'static str, retryable: bool },
}

fn pre_start_boundary<C, K>(run: &CursorBoundAuthorizedAgentRun<C>, clock: &K) -> LifecycleBoundary
where
    C: AgentProgressConnector,
    K: UnixMillisClock,
{
    match run.ensure_lease_running() {
        Ok(()) => {}
        Err(ClaimLeaseError::Cancelled(_)) => {
            return LifecycleBoundary::Terminal(RuntimeFailureKind::Cancelled);
        }
        Err(error) => {
            return LifecycleBoundary::RecoveryRequired {
                code: error.code().as_str(),
                retryable: error.retryable(),
            };
        }
    }
    match deadline_reached(run, clock) {
        Ok(true) => LifecycleBoundary::Terminal(RuntimeFailureKind::DeadlineExceeded),
        Ok(false) => LifecycleBoundary::Continue,
        Err(code) => LifecycleBoundary::RecoveryRequired {
            code,
            retryable: false,
        },
    }
}

fn deadline_reached<C, K>(
    run: &CursorBoundAuthorizedAgentRun<C>,
    clock: &K,
) -> Result<bool, &'static str>
where
    C: AgentProgressConnector,
    K: UnixMillisClock,
{
    let now = clock.now_unix_millis();
    if now <= 0 {
        return Err("agent_lifecycle.invalid_clock");
    }
    Ok(now >= run.deadline_unix_millis())
}

fn deadline_reached_assembled<C, S, K>(
    run: &AssembledAuthorizedAgentRun<C, S>,
    clock: &K,
) -> Result<bool, &'static str>
where
    C: AgentProgressConnector,
    K: UnixMillisClock,
{
    let now = clock.now_unix_millis();
    if now <= 0 {
        return Err("agent_lifecycle.invalid_clock");
    }
    Ok(now >= run.deadline_unix_millis())
}

fn sampled_time<K: UnixMillisClock>(clock: &K) -> Result<(i64, DateTime<Utc>), &'static str> {
    let now = clock.now_unix_millis();
    let time = (now > 0)
        .then(|| DateTime::<Utc>::from_timestamp_millis(now))
        .flatten()
        .ok_or("agent_lifecycle.invalid_clock")?;
    Ok((now, time))
}

fn assembly_failure(error: &NativeAgentAssemblyError) -> RuntimeFailureKind {
    match error.code() {
        NativeAgentAssemblyErrorCode::UnsupportedCapability => {
            RuntimeFailureKind::UnsupportedCapability
        }
        NativeAgentAssemblyErrorCode::DependencyUnavailable => {
            RuntimeFailureKind::DependencyUnavailable
        }
        NativeAgentAssemblyErrorCode::InvalidInput => RuntimeFailureKind::InvalidInput,
        NativeAgentAssemblyErrorCode::ResourceExhausted => RuntimeFailureKind::ResourceExhausted,
        NativeAgentAssemblyErrorCode::AuthorizationFailed => {
            RuntimeFailureKind::AuthorizationFailed
        }
        NativeAgentAssemblyErrorCode::InvalidConfiguration
        | NativeAgentAssemblyErrorCode::InvalidResult => RuntimeFailureKind::Internal,
    }
}

fn projection_failure(error: &AgentEventProjectionError) -> RuntimeFailureKind {
    match error.code() {
        AgentEventProjectionErrorCode::UnsupportedCapability => {
            RuntimeFailureKind::UnsupportedCapability
        }
        AgentEventProjectionErrorCode::ResourceExhausted => RuntimeFailureKind::ResourceExhausted,
        AgentEventProjectionErrorCode::ProviderFailure
        | AgentEventProjectionErrorCode::InvalidState
        | AgentEventProjectionErrorCode::InvalidOutput => RuntimeFailureKind::Internal,
    }
}

#[cfg(test)]
mod taxonomy_tests {
    use super::assembly_failure;
    use crate::agents::runtime::{NativeAgentAssemblyError, NativeAgentAssemblyErrorCode};
    use crate::protocol::output::RuntimeFailureKind;

    #[test]
    fn assembly_failures_keep_the_canonical_terminal_kind() {
        for (code, expected) in [
            (
                NativeAgentAssemblyErrorCode::InvalidInput,
                RuntimeFailureKind::InvalidInput,
            ),
            (
                NativeAgentAssemblyErrorCode::UnsupportedCapability,
                RuntimeFailureKind::UnsupportedCapability,
            ),
            (
                NativeAgentAssemblyErrorCode::ResourceExhausted,
                RuntimeFailureKind::ResourceExhausted,
            ),
            (
                NativeAgentAssemblyErrorCode::AuthorizationFailed,
                RuntimeFailureKind::AuthorizationFailed,
            ),
            (
                NativeAgentAssemblyErrorCode::DependencyUnavailable,
                RuntimeFailureKind::DependencyUnavailable,
            ),
            (
                NativeAgentAssemblyErrorCode::InvalidConfiguration,
                RuntimeFailureKind::Internal,
            ),
            (
                NativeAgentAssemblyErrorCode::InvalidResult,
                RuntimeFailureKind::Internal,
            ),
        ] {
            let error = NativeAgentAssemblyError::new(code, "fixture");
            assert_eq!(assembly_failure(&error), expected);
        }
    }
}

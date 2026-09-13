//! Single process-owned entry point from prepared agent input to authorization.
//!
//! The coordinator synchronously transfers the complete authorization future
//! and its capacity reservation to the invocation supervisor. Caller
//! cancellation can abandon only the returned result waiter; rejected
//! submissions retain an explicit closeable owner for every unstarted grant.

#![allow(dead_code)] // Production process bootstrap and capability registration remain disabled.

use std::sync::Arc;

use super::agent_invocation::{
    AgentAuthorizationJob, AgentAuthorizationJobCompletion, AgentAuthorizationJobError,
    AuthorizedAgentLifecycle,
};
use super::agent_lease::{ClaimLeaseError, UnixMillisClock};
use super::agent_preparation::PreparedAgentInvocation;
use super::invocation_admission::InvocationAdmission;
use super::invocation_supervisor::{
    InvocationSubmissionRejected, InvocationSupervisionError, InvocationSupervisor,
    SupervisedInvocation,
};
use super::native_agent_lifecycle::NativeAuthorizedAgentLifecycle;
use super::output_delivery::{
    AgentProgressConnector, AgentTerminalRecoveryConfig, AgentTerminalReplay,
};
use crate::agents::runtime::NativeAgentAssembler;
use crate::protocol::control::AgentControlClient;
use crate::transport::ControlRpc;
use crate::transport::redis_commands::{RedisCommandRetirer, RedisRetirementClient};

/// Process-owned authorization and native-runtime task coordinator.
pub(super) struct AgentInvocationCoordinator<R, RC, T, K, D> {
    supervisor: InvocationSupervisor,
    control: Arc<AgentControlClient<R>>,
    retirer: Arc<RedisCommandRetirer<RC>>,
    replay: Arc<T>,
    clock: Arc<K>,
    terminal_recovery: AgentTerminalRecoveryConfig,
    authorized: Arc<D>,
}

impl<R, RC, T, K, D> AgentInvocationCoordinator<R, RC, T, K, D>
where
    R: ControlRpc + 'static,
    RC: RedisRetirementClient + 'static,
    T: AgentTerminalReplay + 'static,
    K: UnixMillisClock,
    D: AuthorizedAgentLifecycle,
{
    #[allow(clippy::too_many_arguments)] // Every explicit owner is security-significant.
    #[must_use]
    pub(super) fn new(
        admission: InvocationAdmission,
        control: Arc<AgentControlClient<R>>,
        retirer: Arc<RedisCommandRetirer<RC>>,
        replay: Arc<T>,
        clock: Arc<K>,
        terminal_recovery: AgentTerminalRecoveryConfig,
        authorized: Arc<D>,
    ) -> Self {
        Self {
            supervisor: InvocationSupervisor::new(admission),
            control,
            retirer,
            replay,
            clock,
            terminal_recovery,
            authorized,
        }
    }

    /// Transfer one prepared invocation to the process supervisor without an
    /// await point between reservation creation and task ownership.
    pub(super) fn submit(
        &self,
        prepared: PreparedAgentInvocation,
    ) -> Result<AgentInvocationWaiter, RejectedAgentInvocation<R, RC, T, K, D>> {
        let (reservation, job) = AgentAuthorizationJob::new(
            prepared,
            Arc::clone(&self.control),
            Arc::clone(&self.retirer),
            Arc::clone(&self.replay),
            Arc::clone(&self.clock),
            self.terminal_recovery,
            Arc::clone(&self.authorized),
        );
        match self.supervisor.submit(reservation, job) {
            Ok(invocation) => Ok(AgentInvocationWaiter { invocation }),
            Err(rejected) => Err(RejectedAgentInvocation { rejected }),
        }
    }

    /// Transfer an unpolled recovery future and its reservation atomically.
    pub(super) fn submit_checkpoint<
        I: super::agent_preparation::AgentInputMaterializer + 'static,
    >(
        &self,
        recovery: super::agent_delivery::CheckpointAgentDelivery,
        output: super::output_delivery::PreparedAgentOutput,
        reservation: super::invocation_admission::InvocationReservation,
        input: Arc<I>,
        lease_config: super::agent_lease::ClaimLeaseMonitorConfig,
    ) -> Result<AgentInvocationWaiter, InvocationSupervisionError> {
        let services = super::checkpoint_recovery::CheckpointRecoveryServices {
            control: self.control.clone(),
            retirer: self.retirer.clone(),
            replay: self.replay.clone(),
            clock: self.clock.clone(),
            authorized: self.authorized.clone(),
            input,
            lease_config,
            terminal_config: self.terminal_recovery,
        };
        match self
            .supervisor
            .submit(reservation, Box::pin(services.run(recovery, output)))
        {
            Ok(invocation) => Ok(AgentInvocationWaiter { invocation }),
            Err(rejected) => {
                let (error, reservation, unpolled) = rejected.into_parts();
                // Rejection guarantees no poll: no actor or RPC was started.
                drop(unpolled);
                drop(reservation);
                Err(error)
            }
        }
    }

    #[must_use]
    pub(super) fn active_count(&self) -> usize {
        self.supervisor.active_count()
    }

    /// Reject later ownership transfers without cancelling accepted work.
    pub(super) fn stop(&self) -> Result<(), InvocationSupervisionError> {
        self.supervisor.stop()
    }

    /// Stop submission and cancellation-safely drain every accepted task.
    pub(super) async fn close(&self) -> Result<(), InvocationSupervisionError> {
        self.supervisor.close().await
    }
}

/// Result waiter with no cancellation authority over its supervised task.
pub(super) struct AgentInvocationWaiter {
    invocation: SupervisedInvocation<AgentAuthorizationJobCompletion>,
}

impl AgentInvocationWaiter {
    pub(super) async fn wait(
        self,
    ) -> Result<AgentAuthorizationJobCompletion, InvocationSupervisionError> {
        self.invocation.wait().await
    }
}

/// Rejected submission retaining the exact unstarted authorization job.
///
/// This type intentionally implements neither `Clone` nor `Debug`. The caller
/// must consume it with [`Self::close`] so the claim/session grants and
/// reservation are closed under their normal typed owners.
pub(super) struct RejectedAgentInvocation<R, RC, T, K, D> {
    rejected: InvocationSubmissionRejected<AgentAuthorizationJob<R, RC, T, K, D>>,
}

impl<R, RC, T, K, D> RejectedAgentInvocation<R, RC, T, K, D>
where
    R: ControlRpc + 'static,
    RC: RedisRetirementClient + 'static,
    T: AgentTerminalReplay + 'static,
    K: UnixMillisClock,
    D: AuthorizedAgentLifecycle,
{
    #[must_use]
    pub(super) const fn error(&self) -> InvocationSupervisionError {
        self.rejected.error()
    }

    /// Close the unstarted job and then release its exact admission slot.
    pub(super) async fn close(self) -> Result<Option<ClaimLeaseError>, AgentAuthorizationJobError> {
        let (_error, reservation, job) = self.rejected.into_parts();
        let result = job.close_unstarted().await;
        drop(reservation);
        result
    }
}

/// Compose the common native ADK lifecycle and authorization coordinator.
///
/// The same connector, control client, retirement owner and clock generation
/// are shared across authorization, progress, terminal replay and settlement;
/// no direct/pipeline mode can install an alternate delivery path.
#[allow(clippy::too_many_arguments, clippy::type_complexity)]
pub(super) fn native_agent_coordinator<A, C, R, RC, K>(
    admission: InvocationAdmission,
    assembler: Arc<A>,
    connector: C,
    control: Arc<AgentControlClient<R>>,
    retirer: Arc<RedisCommandRetirer<RC>>,
    clock: Arc<K>,
    max_output_sessions: usize,
    terminal_recovery: AgentTerminalRecoveryConfig,
) -> AgentInvocationCoordinator<R, RC, C, K, NativeAuthorizedAgentLifecycle<A, C, R, RC, K>>
where
    A: NativeAgentAssembler,
    C: AgentProgressConnector + AgentTerminalReplay + Clone + Send + Sync + 'static,
    R: ControlRpc + 'static,
    RC: RedisRetirementClient + 'static,
    K: UnixMillisClock,
{
    let lifecycle = Arc::new(NativeAuthorizedAgentLifecycle::new(
        assembler,
        connector.clone(),
        Arc::clone(&control),
        Arc::clone(&retirer),
        Arc::clone(&clock),
        max_output_sessions,
        terminal_recovery,
    ));
    AgentInvocationCoordinator::new(
        admission,
        control,
        retirer,
        Arc::new(connector),
        clock,
        terminal_recovery,
        lifecycle,
    )
}

//! Process-owned checkpoint preparation, inspection, and authorization.
#![allow(dead_code)] // Intake stays disabled until partial-output replacement is connected.

use std::sync::Arc;

use super::agent_delivery::CheckpointAgentDelivery;
use super::agent_invocation::{
    AgentAuthorizationJobCompletion, AgentAuthorizedLifecycleCompletion, AuthorizedAgentLifecycle,
};
use super::agent_lease::{
    ClaimLeaseError, ClaimLeaseMonitor, ClaimLeaseMonitorConfig, UnixMillisClock,
};
use super::agent_preparation::{
    AgentFailureTerminal, AgentInputMaterializer, AuthorizedAgentRun, CheckpointInputError,
    PreInvocationTerminalCause, prepare_checkpoint_input,
};
use super::native_agent_lifecycle::assembly_failure;
use super::output_delivery::{
    AgentTerminalRecoveryConfig, AgentTerminalReplay, PreparedAgentOutput,
    publish_agent_failure_terminal,
};
use crate::agents::session::AuthorizedNativeCommandBinding;
use crate::protocol::control::AgentControlClient;
use crate::protocol::output::RuntimeFailureKind;
use crate::transport::ControlRpc;
use crate::transport::redis_commands::{RedisCommandRetirer, RedisRetirementClient};

pub(super) struct CheckpointRecoveryServices<R, RC, T, K, D, I> {
    pub control: Arc<AgentControlClient<R>>,
    pub retirer: Arc<RedisCommandRetirer<RC>>,
    pub replay: Arc<T>,
    pub clock: Arc<K>,
    pub authorized: Arc<D>,
    pub input: Arc<I>,
    pub lease_config: ClaimLeaseMonitorConfig,
    pub terminal_config: AgentTerminalRecoveryConfig,
}

impl<R, RC, T, K, D, I> CheckpointRecoveryServices<R, RC, T, K, D, I>
where
    R: ControlRpc + 'static,
    RC: RedisRetirementClient + 'static,
    T: AgentTerminalReplay + 'static,
    K: UnixMillisClock,
    D: AuthorizedAgentLifecycle,
    I: AgentInputMaterializer + 'static,
{
    async fn terminal(&self, terminal: AgentFailureTerminal) -> AgentAuthorizationJobCompletion {
        AgentAuthorizationJobCompletion::Terminal(
            publish_agent_failure_terminal(
                self.control.clone(),
                self.retirer.as_ref(),
                self.replay.as_ref(),
                terminal,
                self.clock.clone(),
                self.terminal_config,
            )
            .await,
        )
    }

    /// This future must be transferred to the supervisor before its first poll.
    /// No lease actor, content request, or authorization exists before polling.
    #[allow(clippy::too_many_lines)] // Keep the ordered authority transitions in one owner.
    pub(super) async fn run(
        self,
        recovery: CheckpointAgentDelivery,
        output: PreparedAgentOutput,
    ) -> AgentAuthorizationJobCompletion {
        let (delivery, verified, inspection) = recovery.into_parts();
        let kind = verified.kind();
        let mut lease = ClaimLeaseMonitor::start_checkpoint_inspection(
            self.control.clone(),
            inspection,
            self.clock.clone(),
            self.lease_config,
        );
        let live = match lease.activate_checkpoint_inspection().await {
            Ok(live) => live,
            Err(error) => {
                return retain(lease, kind, error.code().as_str(), error.retryable()).await;
            }
        };
        let request = match prepare_checkpoint_input(
            self.input.as_ref(),
            &live,
            &verified,
            &mut lease,
            self.clock.as_ref(),
        )
        .await
        {
            Ok(request) => request,
            Err(CheckpointInputError::Lease(error))
                if !matches!(error, ClaimLeaseError::Cancelled(_)) =>
            {
                return retain(lease, kind, error.code().as_str(), error.retryable()).await;
            }
            Err(error) => {
                let failure = match error {
                    CheckpointInputError::Lease(_) => RuntimeFailureKind::Cancelled,
                    CheckpointInputError::Content(error) => {
                        PreInvocationTerminalCause::InputContent(error).runtime_failure_kind()
                    }
                    CheckpointInputError::Protocol(error) => {
                        PreInvocationTerminalCause::InputProtocol(error).runtime_failure_kind()
                    }
                    CheckpointInputError::DeadlineExceeded => RuntimeFailureKind::DeadlineExceeded,
                };
                return self
                    .terminal(AgentFailureTerminal {
                        delivery,
                        verified,
                        output_authority: live.into_output_authority(),
                        output,
                        lease,
                        proposed_failure: failure,
                    })
                    .await;
            }
        };
        let command = match AuthorizedNativeCommandBinding::from_verified(&verified) {
            Ok(command) => command,
            Err(error) => {
                return self
                    .terminal(AgentFailureTerminal {
                        delivery,
                        verified,
                        output_authority: live.into_output_authority(),
                        output,
                        lease,
                        proposed_failure: assembly_failure(&error),
                    })
                    .await;
            }
        };
        let (claim, session) = live.into_session_inspection();
        let writer_lease = Arc::new(lease.state_probe());
        let evidence = match lease
            .run_cancellation_safe_phase(self.authorized.inspect_checkpoint(
                &request,
                &command,
                session,
                writer_lease,
            ))
            .await
        {
            Ok(Ok(evidence)) => evidence,
            Ok(Err(error)) => {
                return self
                    .terminal(AgentFailureTerminal {
                        delivery,
                        verified,
                        output_authority: claim.into_output_authority(),
                        output,
                        lease,
                        proposed_failure: assembly_failure(&error),
                    })
                    .await;
            }
            Err(ClaimLeaseError::Cancelled(_)) => {
                return self
                    .terminal(AgentFailureTerminal {
                        delivery,
                        verified,
                        output_authority: claim.into_output_authority(),
                        output,
                        lease,
                        proposed_failure: RuntimeFailureKind::Cancelled,
                    })
                    .await;
            }
            Err(error) => {
                return retain(lease, kind, error.code().as_str(), error.retryable()).await;
            }
        };
        if let Err(error) = lease.check_now().await {
            if matches!(error, ClaimLeaseError::Cancelled(_)) {
                return self
                    .terminal(AgentFailureTerminal {
                        delivery,
                        verified,
                        output_authority: claim.into_output_authority(),
                        output,
                        lease,
                        proposed_failure: RuntimeFailureKind::Cancelled,
                    })
                    .await;
            }
            return retain(lease, kind, error.code().as_str(), error.retryable()).await;
        }
        // Never race this one-attempt RPC against cancellation: its outcome
        // must remain supervised even if the caller stops waiting.
        let authorization = match claim.authorize(self.control.as_ref(), evidence).await {
            Ok(authorization) => authorization,
            Err(failure) => {
                return retain(
                    lease,
                    kind,
                    "agent_checkpoint.authorization_uncertain",
                    failure.error().retryable(),
                )
                .await;
            }
        };
        let run = AuthorizedAgentRun::from_checkpoint(
            delivery,
            verified,
            request,
            output,
            lease,
            authorization,
        );
        let completion = self.authorized.run(run).await;
        if completion.execution_kind() != kind {
            return AgentAuthorizationJobCompletion::InvalidState(
                super::agent_invocation::AgentAuthorizationJobError::InvalidState(
                    "checkpoint lifecycle returned another execution kind",
                ),
            );
        }
        AgentAuthorizationJobCompletion::Authorized(completion)
    }
}

async fn retain(
    lease: ClaimLeaseMonitor,
    kind: crate::agents::AgentExecutionKind,
    code: &'static str,
    retryable: bool,
) -> AgentAuthorizationJobCompletion {
    let _ = lease.close().await;
    AgentAuthorizationJobCompletion::Authorized(
        AgentAuthorizedLifecycleCompletion::recovery_required(kind, code, retryable),
    )
}

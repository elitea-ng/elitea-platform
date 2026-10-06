//! Process-owned same-visit recovery. No `BeginExecution` or fresh invocation.

use super::agent_delivery::NodeRecoveryAgentDelivery;
use super::agent_invocation::{
    AgentAuthorizationJobCompletion, AgentAuthorizedLifecycleCompletion, AuthorizedAgentLifecycle,
};
use super::agent_lease::{ClaimLeaseMonitor, ClaimLeaseMonitorConfig, UnixMillisClock};
use super::agent_preparation::{
    AgentFailureTerminal, AgentInputMaterializer, AuthorizedAgentRun, input_binding_from_parts,
};
use super::output_delivery::{
    AgentTerminalRecoveryConfig, AgentTerminalReplay, PreparedAgentOutput,
    publish_agent_failure_terminal,
};
use crate::protocol::control::{
    AgentControlClient, ClaimLeaseHandle, NodeRecoveryAckAuthorization, NodeRecoveryJournalLease,
};
use crate::state::StateWriterLease as _;
use crate::transport::ControlRpc;
use crate::transport::redis_commands::{RedisCommandRetirer, RedisRetirementClient};
use std::sync::Arc;
use tokio::sync::watch;
use tokio::task::JoinHandle;

pub(super) struct NodeRecoveryServices<R, RC, T, K, D, I> {
    pub control: Arc<AgentControlClient<R>>,
    pub retirer: Arc<RedisCommandRetirer<RC>>,
    pub replay: Arc<T>,
    pub clock: Arc<K>,
    pub authorized: Arc<D>,
    pub input: Arc<I>,
    pub lease_config: ClaimLeaseMonitorConfig,
    pub terminal_config: AgentTerminalRecoveryConfig,
}
impl<R, RC, T, K, D, I> NodeRecoveryServices<R, RC, T, K, D, I>
where
    R: ControlRpc + 'static,
    RC: RedisRetirementClient + 'static,
    T: AgentTerminalReplay + 'static,
    K: UnixMillisClock,
    D: AuthorizedAgentLifecycle,
    I: AgentInputMaterializer + 'static,
{
    /// Submitted unpolled with its capacity reservation to `InvocationSupervisor`.
    #[allow(
        clippy::manual_let_else,
        reason = "Keep explicit typed owner outcomes beside state checks."
    )]
    #[allow(
        clippy::single_match_else,
        reason = "Keep explicit typed owner outcomes beside state checks."
    )]
    #[allow(
        clippy::too_many_lines,
        reason = "Keep ordered authority checks and durable phases in one owner operation."
    )]
    pub(super) async fn run(
        self,
        recovery: NodeRecoveryAgentDelivery,
        output: PreparedAgentOutput,
    ) -> AgentAuthorizationJobCompletion {
        let (delivery, verified, inspection) = recovery.into_parts();
        let kind = verified.kind();
        let (mut authority, handle, session) = inspection.into_recovery_supervision();
        if !authority.matches_command(&verified) {
            return retained(kind, "node_recovery.command_mismatch", false);
        }
        let mut guard =
            match RecoveryLeaseGuard::activate(self.control.clone(), handle, self.clock.clone())
                .await
            {
                Ok(guard) => guard,
                Err(()) => return retained(kind, "node_recovery.lease_unavailable", true),
            };
        let prepare_probe = guard.probe.clone();
        let prepare = async {
            if self.clock.now_unix_millis() <= 0
                || self.clock.now_unix_millis() >= verified.command().deadline_unix_millis
            {
                return Err("node_recovery.deadline_expired");
            }
            let bytes = self
                .input
                .materialize_node_recovery(&authority)
                .await
                .map_err(|_| "node_recovery.input_unavailable")?;
            prepare_probe
                .ensure_current()
                .map_err(|_| "node_recovery.lease_lost")?;
            let (bundle, reference, entry) = authority.input_binding_parts();
            let binding = input_binding_from_parts(bundle, reference, entry)
                .map_err(|_| "node_recovery.input_invalid")?;
            let message = crate::agents::parse_agent_execution_input(bytes.as_bytes())
                .map_err(|_| "node_recovery.input_invalid")?;
            let request = crate::agents::request_from(message, kind, binding)
                .map_err(|_| "node_recovery.input_invalid")?;
            let command =
                crate::agents::session::AuthorizedNativeCommandBinding::from_verified(&verified)
                    .map_err(|_| "node_recovery.command_invalid")?;
            let visit = self
                .authorized
                .inspect_node_recovery(
                    &request,
                    &command,
                    session,
                    prepare_probe.clone(),
                    authority.receipt(),
                )
                .await
                .map_err(|_| "node_recovery.frontier_invalid")?;
            prepare_probe
                .ensure_current()
                .map_err(|_| "node_recovery.lease_lost")?;
            Ok::<_, &'static str>((request, visit))
        };
        let (request, mut visit) = match guard.run_phase(prepare).await {
            Ok(value) => value,
            Err(code) => {
                guard.close().await;
                return retained(kind, code, false);
            }
        };
        let (applied, ack, operator_request) = loop {
            if self.clock.now_unix_millis() >= verified.command().deadline_unix_millis {
                guard.close().await;
                return retained(kind, "node_recovery.deadline_expired", false);
            }
            if guard.probe.ensure_current().is_err() {
                guard.close().await;
                return retained(kind, "node_recovery.lease_lost", false);
            }
            let now = match u64::try_from(self.clock.now_unix_millis()) {
                Ok(now) if now > 0 => now,
                _ => {
                    guard.close().await;
                    return retained(kind, "node_recovery.clock_invalid", false);
                }
            };
            let action = self.input.apply_node_recovery_action(
                &authority,
                &visit.journal,
                &visit.activation,
                visit.result_projection().0,
                visit.result_projection().1,
                now,
            );
            // ACK is authority-minting and stays owned until its bounded reply.
            let action_result = action.await.map_err(|_| "node_recovery.control_uncertain");
            if guard.probe.ensure_current().is_err() {
                guard.close().await;
                return retained(kind, "node_recovery.lease_lost", false);
            }
            match action_result {
                Ok(Some((applied, ack, operator_request))) => {
                    if applied.continuation_receipt().is_some() {
                        if authority
                            .advance_no_effect_continuation(
                                &ack,
                                &applied,
                                operator_request,
                                &mut visit,
                            )
                            .await
                            .is_err()
                        {
                            guard.close().await;
                            return retained(kind, "node_recovery.continuation_denied", false);
                        }
                        // Main remains SUSPENDED. Wait for a separate operator Retry.
                    } else {
                        break (applied, ack, operator_request);
                    }
                }
                Ok(None) => {}
                Err(code) => {
                    guard.close().await;
                    return retained(kind, code, true);
                }
            }
            tokio::select! {
                ()=tokio::time::sleep(std::time::Duration::from_millis(500))=>{},
                _=guard.health.changed()=>{
                    if !*guard.health.borrow() {guard.close().await;return retained(kind,"node_recovery.lease_lost",false);}
                }
            }
        };
        let authorization = match authority
            .seal_resumption(ack, applied, operator_request, visit)
            .await
        {
            Ok(value) => value,
            Err(_) => {
                guard.close().await;
                return retained(kind, "node_recovery.restore_denied", false);
            }
        };
        let Some(handle) = guard.close().await else {
            return retained(kind, "node_recovery.lease_lost", false);
        };
        // This normal lease probe is issued only after Main's exact ACK changed
        // the claim to RUNNING. The NODE_RECOVERY claim still cannot Begin/Invoke.
        let monitor = ClaimLeaseMonitor::start_output_recovery(
            self.control.clone(),
            handle,
            self.clock.clone(),
            self.lease_config,
        );
        if monitor.check_now().await.is_err() {
            let _ = monitor.close().await;
            return retained(kind, "node_recovery.resume_lease_lost", false);
        }
        match authorization {
            NodeRecoveryAckAuthorization::Restore(authorization) => {
                let run = AuthorizedAgentRun::from_node_checkpoint(
                    delivery,
                    verified,
                    request,
                    output,
                    monitor,
                    authorization,
                );
                let completion = self.authorized.run(run).await;
                if completion.execution_kind() != kind {
                    return retained(kind, "node_recovery.lifecycle_mismatch", false);
                }
                AgentAuthorizationJobCompletion::Authorized(completion)
            }
            NodeRecoveryAckAuthorization::Terminal(authorization) => {
                AgentAuthorizationJobCompletion::Terminal(
                    publish_agent_failure_terminal(
                        self.control,
                        self.retirer.as_ref(),
                        self.replay.as_ref(),
                        AgentFailureTerminal {
                            delivery,
                            verified,
                            output_authority: authorization.into_output_authority(),
                            output,
                            lease: monitor,
                            proposed_failure:
                                crate::protocol::output::RuntimeFailureKind::PipelineCodeFailed,
                        },
                        self.clock,
                        self.terminal_config,
                    )
                    .await,
                )
            }
        }
    }
}
fn retained(
    kind: crate::agents::AgentExecutionKind,
    code: &'static str,
    retryable: bool,
) -> AgentAuthorizationJobCompletion {
    AgentAuthorizationJobCompletion::Authorized(
        AgentAuthorizedLifecycleCompletion::recovery_required(kind, code, retryable),
    )
}

/// One owned actor renews a suspended claim. Drop revokes journal writes;
/// close also joins the actor and returns its exact last renewed handle.
struct RecoveryLeaseGuard {
    probe: Arc<NodeRecoveryJournalLease>,
    health: watch::Receiver<bool>,
    stop: watch::Sender<bool>,
    actor: Option<JoinHandle<Option<ClaimLeaseHandle>>>,
}
impl RecoveryLeaseGuard {
    async fn activate<R: ControlRpc + 'static, K: UnixMillisClock>(
        control: Arc<AgentControlClient<R>>,
        mut handle: ClaimLeaseHandle,
        clock: Arc<K>,
    ) -> Result<Self, ()> {
        let probe = Arc::new(NodeRecoveryJournalLease::new(&handle, clock));
        probe
            .renew(control.as_ref(), &mut handle)
            .await
            .map_err(|_| ())?;
        let (health_tx, health) = watch::channel(true);
        let (stop, mut shutdown) = watch::channel(false);
        let actor_probe = probe.clone();
        let actor = tokio::spawn(async move {
            loop {
                tokio::select! {
                    biased;
                    _=shutdown.changed()=> {return Some(handle);},
                    ()=tokio::time::sleep(std::time::Duration::from_secs(1))=>{},
                }
                let renewed = tokio::select! {
                    biased;
                    _=shutdown.changed()=>{return Some(handle);},
                    result=actor_probe.renew(control.as_ref(),&mut handle)=>result,
                };
                if renewed.is_err() {
                    actor_probe.revoke();
                    let _ = health_tx.send(false);
                    return None;
                }
            }
        });
        Ok(Self {
            probe,
            health,
            stop,
            actor: Some(actor),
        })
    }
    async fn run_phase<T>(
        &mut self,
        phase: impl std::future::Future<Output = Result<T, &'static str>>,
    ) -> Result<T, &'static str> {
        if self.probe.ensure_current().is_err() {
            return Err("node_recovery.lease_lost");
        }
        tokio::select! {
            biased;
            _=self.health.changed()=>Err("node_recovery.lease_lost"),
            result=phase=> {self.probe.ensure_current().map_err(|_|"node_recovery.lease_lost")?;result},
        }
    }
    async fn close(mut self) -> Option<ClaimLeaseHandle> {
        let _ = self.stop.send(true);
        let result = match self.actor.take() {
            Some(actor) => actor.await.ok().flatten(),
            None => None,
        };
        self.probe.revoke();
        result
    }
}
impl Drop for RecoveryLeaseGuard {
    fn drop(&mut self) {
        self.probe.revoke();
        let _ = self.stop.send(true);
    }
}

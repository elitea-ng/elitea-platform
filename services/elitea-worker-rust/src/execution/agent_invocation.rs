//! Supervisor-owned authorization and post-authorization lifecycle handoff.
//!
//! A prepared invocation is converted into an authorization candidate only
//! after the process supervisor has synchronously accepted its task. Dropping
//! the returned result waiter therefore cannot cancel `AuthorizeInvocation`,
//! lose its response, or abandon the unique lease/output authority.

#![allow(dead_code)] // Production process bootstrap and capability registration remain disabled.

use std::fmt;
use std::future::Future;
use std::pin::Pin;
use std::sync::Arc;
use std::task::{Context, Poll};

use super::agent_lease::{ClaimLeaseError, UnixMillisClock};
use super::agent_preparation::{
    AgentAuthorizationUnknownCompletion, AuthorizedAgentRun, PreparedAgentAuthorization,
    PreparedAgentInvocation,
};
use super::invocation_admission::InvocationReservation;
use super::output_delivery::{
    AgentFailureTerminalCompletion, AgentFailureTerminalError, AgentTerminalRecoveryConfig,
    AgentTerminalReplay, publish_agent_failure_terminal,
};
use crate::agents::AgentExecutionKind;
use crate::protocol::control::{AgentControlClient, InvocationAuthorizationDecision};
use crate::transport::ControlRpc;
use crate::transport::redis_commands::{RedisCommandRetirer, RedisRetirementClient};

pub(super) type OwnedFuture<T> = Pin<Box<dyn Future<Output = T> + Send + 'static>>;

/// The sole continuation allowed to consume an authorized native run.
///
/// Implementations must retain the complete run until ADK event production,
/// terminal ACK, settlement, Redis retirement, and lease shutdown have reached
/// one closed outcome. Returning earlier would violate the supervisor contract.
pub(super) trait AuthorizedAgentLifecycle: Send + Sync + 'static {
    fn inspect_checkpoint<'a>(
        &'a self,
        _request: &'a crate::agents::AgentExecutionRequest,
        _command: &'a crate::agents::session::AuthorizedNativeCommandBinding,
        _session: crate::protocol::control::ClaimBoundSessionAuthority,
        _lease: Arc<dyn crate::state::StateWriterLease>,
    ) -> Pin<
        Box<
            dyn Future<
                    Output = Result<
                        crate::agents::session::ValidatedModelCheckpoint,
                        crate::agents::runtime::NativeAgentAssemblyError,
                    >,
                > + Send
                + 'a,
        >,
    > {
        Box::pin(async {
            Err(crate::agents::runtime::NativeAgentAssemblyError::new(
                crate::agents::runtime::NativeAgentAssemblyErrorCode::UnsupportedCapability,
                "checkpoint inspection is unavailable",
            ))
        })
    }

    fn run(&self, run: AuthorizedAgentRun) -> OwnedFuture<AgentAuthorizedLifecycleCompletion>;
}

/// Authority-free outcome of one fully owned authorized lifecycle.
pub(super) enum AgentAuthorizedLifecycleDisposition {
    ExecutedSettledAcked {
        sequence: u64,
        settlement_receipt_id: String,
    },
    RecoveryRequiredNoAck {
        code: &'static str,
        retryable: bool,
    },
}

/// Authority-free observation returned only after the authorized lifecycle
/// implementation has consumed every run capability.
pub(super) struct AgentAuthorizedLifecycleCompletion {
    execution_kind: AgentExecutionKind,
    disposition: AgentAuthorizedLifecycleDisposition,
}

impl AgentAuthorizedLifecycleCompletion {
    #[must_use]
    pub(super) const fn execution_kind(&self) -> AgentExecutionKind {
        self.execution_kind
    }

    #[must_use]
    pub(super) const fn disposition(&self) -> &AgentAuthorizedLifecycleDisposition {
        &self.disposition
    }

    pub(super) fn settled(
        execution_kind: AgentExecutionKind,
        sequence: u64,
        settlement_receipt_id: String,
    ) -> Self {
        Self {
            execution_kind,
            disposition: AgentAuthorizedLifecycleDisposition::ExecutedSettledAcked {
                sequence,
                settlement_receipt_id,
            },
        }
    }

    pub(super) const fn recovery_required(
        execution_kind: AgentExecutionKind,
        code: &'static str,
        retryable: bool,
    ) -> Self {
        Self {
            execution_kind,
            disposition: AgentAuthorizedLifecycleDisposition::RecoveryRequiredNoAck {
                code,
                retryable,
            },
        }
    }

    #[cfg(test)]
    pub(super) const fn for_test(execution_kind: AgentExecutionKind) -> Self {
        Self::recovery_required(execution_kind, "agent_test.incomplete", false)
    }
}

/// Closed result of one supervisor-owned authorization task.
pub(super) enum AgentAuthorizationJobCompletion {
    Authorized(AgentAuthorizedLifecycleCompletion),
    Terminal(Result<AgentFailureTerminalCompletion, AgentFailureTerminalError>),
    Unknown(AgentAuthorizationUnknownCompletion),
    InvalidState(AgentAuthorizationJobError),
}

/// Stable local lifecycle errors. These never authorize output or Redis ACK.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(super) enum AgentAuthorizationJobError {
    InvalidState(&'static str),
}

impl AgentAuthorizationJobError {
    #[must_use]
    pub(super) const fn code(self) -> &'static str {
        match self {
            Self::InvalidState(_) => "agent_authorization.invalid_state",
        }
    }
}

impl fmt::Display for AgentAuthorizationJobError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::InvalidState(message) => formatter.write_str(message),
        }
    }
}

impl std::error::Error for AgentAuthorizationJobError {}

struct AgentAuthorizationInputs<R, C, T, K, D> {
    prepared: PreparedAgentAuthorization,
    control: Arc<AgentControlClient<R>>,
    retirer: Arc<RedisCommandRetirer<C>>,
    replay: Arc<T>,
    clock: Arc<K>,
    recovery_config: AgentTerminalRecoveryConfig,
    authorized: Arc<D>,
}

type BoxedAuthorizationInputs<R, C, T, K, D> = Box<AgentAuthorizationInputs<R, C, T, K, D>>;

enum AgentAuthorizationJobState<R, C, T, K, D> {
    Unstarted(Option<BoxedAuthorizationInputs<R, C, T, K, D>>),
    Running(OwnedFuture<AgentAuthorizationJobCompletion>),
    Complete,
}

/// Future transferred to the process supervisor before authorization starts.
///
/// This type is neither cloneable nor formattable. Before first poll it can be
/// recovered from a supervisor stop race and closed without performing the RPC.
pub(super) struct AgentAuthorizationJob<R, C, T, K, D> {
    state: AgentAuthorizationJobState<R, C, T, K, D>,
}

impl<R, C, T, K, D> AgentAuthorizationJob<R, C, T, K, D>
where
    R: ControlRpc + 'static,
    C: RedisRetirementClient + 'static,
    T: AgentTerminalReplay + 'static,
    K: UnixMillisClock,
    D: AuthorizedAgentLifecycle,
{
    #[allow(clippy::too_many_arguments)] // Explicit authority dependencies are intentional.
    pub(super) fn new(
        prepared: PreparedAgentInvocation,
        control: Arc<AgentControlClient<R>>,
        retirer: Arc<RedisCommandRetirer<C>>,
        replay: Arc<T>,
        clock: Arc<K>,
        recovery_config: AgentTerminalRecoveryConfig,
        authorized: Arc<D>,
    ) -> (InvocationReservation, Self) {
        let (reservation, prepared) = prepared.into_supervised_authorization();
        (
            reservation,
            Self {
                state: AgentAuthorizationJobState::Unstarted(Some(Box::new(
                    AgentAuthorizationInputs {
                        prepared,
                        control,
                        retirer,
                        replay,
                        clock,
                        recovery_config,
                        authorized,
                    },
                ))),
            },
        )
    }

    /// Close a task returned by a supervisor stop race before first poll.
    ///
    /// # Errors
    ///
    /// Returns a stable state error if the operation was already polled.
    pub(super) async fn close_unstarted(
        self,
    ) -> Result<Option<ClaimLeaseError>, AgentAuthorizationJobError> {
        match self.state {
            AgentAuthorizationJobState::Unstarted(Some(inputs)) => {
                let inputs = *inputs;
                Ok(inputs.prepared.close_no_ack().await)
            }
            AgentAuthorizationJobState::Unstarted(None)
            | AgentAuthorizationJobState::Running(_)
            | AgentAuthorizationJobState::Complete => {
                Err(AgentAuthorizationJobError::InvalidState(
                    "the authorization task has already started",
                ))
            }
        }
    }
}

impl<R, C, T, K, D> Future for AgentAuthorizationJob<R, C, T, K, D>
where
    R: ControlRpc + 'static,
    C: RedisRetirementClient + 'static,
    T: AgentTerminalReplay + 'static,
    K: UnixMillisClock,
    D: AuthorizedAgentLifecycle,
{
    type Output = AgentAuthorizationJobCompletion;

    fn poll(self: Pin<&mut Self>, context: &mut Context<'_>) -> Poll<Self::Output> {
        let this = self.get_mut();
        loop {
            match &mut this.state {
                AgentAuthorizationJobState::Unstarted(inputs) => {
                    let Some(inputs) = inputs.take() else {
                        this.state = AgentAuthorizationJobState::Complete;
                        return Poll::Ready(AgentAuthorizationJobCompletion::InvalidState(
                            AgentAuthorizationJobError::InvalidState(
                                "the authorization task lost its unstarted state",
                            ),
                        ));
                    };
                    this.state = AgentAuthorizationJobState::Running(run_authorization(inputs));
                }
                AgentAuthorizationJobState::Running(operation) => {
                    let Poll::Ready(completion) = operation.as_mut().poll(context) else {
                        return Poll::Pending;
                    };
                    this.state = AgentAuthorizationJobState::Complete;
                    return Poll::Ready(completion);
                }
                AgentAuthorizationJobState::Complete => {
                    return Poll::Ready(AgentAuthorizationJobCompletion::InvalidState(
                        AgentAuthorizationJobError::InvalidState(
                            "the authorization task was polled after completion",
                        ),
                    ));
                }
            }
        }
    }
}

fn run_authorization<R, C, T, K, D>(
    inputs: BoxedAuthorizationInputs<R, C, T, K, D>,
) -> OwnedFuture<AgentAuthorizationJobCompletion>
where
    R: ControlRpc + 'static,
    C: RedisRetirementClient + 'static,
    T: AgentTerminalReplay + 'static,
    K: UnixMillisClock,
    D: AuthorizedAgentLifecycle,
{
    Box::pin(async move {
        let AgentAuthorizationInputs {
            prepared,
            control,
            retirer,
            replay,
            clock,
            recovery_config,
            authorized,
        } = *inputs;
        match control
            .authorize_agent_invocation(prepared.into_candidate())
            .await
        {
            InvocationAuthorizationDecision::AuthorizedNow(run) => {
                let expected_kind = run.execution_kind();
                let completion = authorized.run(*run).await;
                if completion.execution_kind() == expected_kind {
                    AgentAuthorizationJobCompletion::Authorized(completion)
                } else {
                    AgentAuthorizationJobCompletion::InvalidState(
                        AgentAuthorizationJobError::InvalidState(
                            "the authorized lifecycle returned a mismatched execution kind",
                        ),
                    )
                }
            }
            InvocationAuthorizationDecision::AlreadyAuthorized(terminal)
            | InvocationAuthorizationDecision::Rejected(terminal) => {
                AgentAuthorizationJobCompletion::Terminal(
                    publish_agent_failure_terminal(
                        control,
                        retirer.as_ref(),
                        replay.as_ref(),
                        *terminal,
                        clock,
                        recovery_config,
                    )
                    .await,
                )
            }
            InvocationAuthorizationDecision::Unknown(unknown) => {
                AgentAuthorizationJobCompletion::Unknown(unknown.close_no_ack().await)
            }
        }
    })
}

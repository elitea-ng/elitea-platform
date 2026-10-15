//! Toolkit adaptation of the shared, owned final invocation fence.
use std::sync::Arc;

use super::{
    AgentControlError, AgentExecutionOutputAuthority, ClaimBoundRuntimeContextAuthority,
    ClaimBoundSessionAuthority, InvocationAuthorizationNoAckAuthority,
    InvocationAuthorizationPayload, InvocationAuthorizationTerminalCause,
    InvocationSubmissionPermit, LeaseMonitoredAgentExecution,
};

pub(crate) struct ToolkitInvocationPayload;

/// Only an authenticated `AUTHORIZED_NOW` response can construct this value.
/// The borrowed execution view permits existing input/result binding, but cannot
/// be consumed to request another invocation permit.
pub(crate) struct AuthorizedToolkitExecution {
    execution: LeaseMonitoredAgentExecution,
    runtime_context: Arc<ClaimBoundRuntimeContextAuthority>,
    _permit: InvocationSubmissionPermit,
}

impl AuthorizedToolkitExecution {
    pub(crate) fn execution(&self) -> &LeaseMonitoredAgentExecution {
        &self.execution
    }

    /// The claim-bound authority an index run redeems its platform PAT with
    /// (the gateway embeddings call and the artifact object API, ADR-0030
    /// I1). Minted at `AUTHORIZED_NOW` like the agent's.
    pub(crate) fn runtime_context(&self) -> &ClaimBoundRuntimeContextAuthority {
        &self.runtime_context
    }

    /// The same authority, shared: the `artifact` family's platform writer
    /// holds it for the read-only tools a direct call runs (D4).
    pub(crate) fn shared_runtime_context(&self) -> Arc<ClaimBoundRuntimeContextAuthority> {
        Arc::clone(&self.runtime_context)
    }

    /// The elitea-vector token of the authorized claim (ADR-0031): the handle
    /// a toolkit's vector-index client uses. `None` when Main minted none.
    #[allow(
        dead_code,
        reason = "The vector-index client (ADR-0030) is its consumer."
    )]
    pub(crate) fn vector_token(&self) -> Option<&super::super::vector_token::VectorClaimToken> {
        self.execution.vector_token()
    }

    pub(crate) fn into_output_authority(self) -> AgentExecutionOutputAuthority {
        self.execution.into_output_authority()
    }
}

pub(crate) struct ToolkitAuthorizationTerminal {
    pub(crate) output: AgentExecutionOutputAuthority,
    pub(crate) cause: InvocationAuthorizationTerminalCause,
}

pub(crate) struct ToolkitAuthorizationUnknown {
    _authority: InvocationAuthorizationNoAckAuthority,
    pub(crate) error: AgentControlError,
}

impl InvocationAuthorizationPayload for ToolkitInvocationPayload {
    type Authorized = AuthorizedToolkitExecution;
    type Terminal = ToolkitAuthorizationTerminal;
    type Unknown = ToolkitAuthorizationUnknown;

    fn into_authorized(
        self,
        permit: InvocationSubmissionPermit,
        output: AgentExecutionOutputAuthority,
        runtime_context: ClaimBoundRuntimeContextAuthority,
        _session: ClaimBoundSessionAuthority,
    ) -> Self::Authorized {
        AuthorizedToolkitExecution {
            execution: LeaseMonitoredAgentExecution {
                claim: output.claim,
            },
            runtime_context: Arc::new(runtime_context),
            _permit: permit,
        }
    }

    fn into_authorization_terminal(
        self,
        output: AgentExecutionOutputAuthority,
        cause: InvocationAuthorizationTerminalCause,
    ) -> Self::Terminal {
        ToolkitAuthorizationTerminal { output, cause }
    }

    fn into_authorization_unknown(
        self,
        authority: InvocationAuthorizationNoAckAuthority,
        error: AgentControlError,
    ) -> Self::Unknown {
        ToolkitAuthorizationUnknown {
            _authority: authority,
            error,
        }
    }
}

//! Toolkit adaptation of the shared, owned final invocation fence.
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
    _permit: InvocationSubmissionPermit,
}

impl AuthorizedToolkitExecution {
    pub(crate) fn execution(&self) -> &LeaseMonitoredAgentExecution {
        &self.execution
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
        _runtime_context: ClaimBoundRuntimeContextAuthority,
        _session: ClaimBoundSessionAuthority,
    ) -> Self::Authorized {
        AuthorizedToolkitExecution {
            execution: LeaseMonitoredAgentExecution {
                claim: output.claim,
            },
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

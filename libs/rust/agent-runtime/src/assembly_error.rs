//! The native assembly error every runtime module reports (ADR-0029
//! decision 2, stage 2), moved from the worker's `agents::runtime`.
//!
//! Its payload, the sanitized delegated-authorization requirement, is the
//! toolkits' own type. The worker keeps `impl From<RuntimeContextError>` (its
//! transport error), which the orphan rule allows there.
#![allow(
    clippy::implicit_hasher,
    clippy::missing_errors_doc,
    clippy::must_use_candidate,
    clippy::return_self_not_must_use,
    reason = "moved verbatim from the worker, where these items were crate-private"
)]

use std::fmt;

use crate::toolkits::DelegatedAuthorizationRequirement;

/// Stable native assembly and result-selection failures.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum NativeAgentAssemblyErrorCode {
    InvalidConfiguration,
    InvalidInput,
    UnsupportedCapability,
    ResourceExhausted,
    AuthorizationFailed,
    DependencyUnavailable,
    InvalidResult,
    /// The saved agent instructions or settings exceed a platform bound that
    /// has a registered readable terminal message (the worker's
    /// `InputLimitField::AgentSettings`). Not retryable.
    AgentSettingsLimit,
}

impl NativeAgentAssemblyErrorCode {
    #[must_use]
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::InvalidConfiguration => "native_agent.invalid_configuration",
            Self::InvalidInput => "native_agent.invalid_input",
            Self::UnsupportedCapability => "native_agent.unsupported_capability",
            Self::ResourceExhausted => "native_agent.resource_exhausted",
            Self::AuthorizationFailed => "native_agent.authorization_failed",
            Self::DependencyUnavailable => "native_agent.dependency_unavailable",
            Self::InvalidResult => "native_agent.invalid_result",
            Self::AgentSettingsLimit => "native_agent.input_limit",
        }
    }
}

/// Data-free reason behind an assembly failure that shares a coarse wire code.
///
/// An id-shape refusal keeps the `InvalidInput` wire code because no registered
/// terminal message fits it. This keeps the worker's own typed code and the
/// limit or field it names (both `'static`, never user content) for the
/// structured log at the lifecycle boundary.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct NativeAgentAssemblyCause {
    code: &'static str,
    detail: Option<&'static str>,
}

impl NativeAgentAssemblyCause {
    #[must_use]
    pub const fn code(&self) -> &'static str {
        self.code
    }

    #[must_use]
    pub const fn detail(&self) -> Option<&'static str> {
        self.detail
    }
}

/// Failure before or after one native ADK stream.
///
/// The message and code remain data-free. The ONE exception is a delegated
/// authorization requirement (#982): when a remote MCP server answers the
/// assembly dial with a `401` challenge, the sanitized
/// [`DelegatedAuthorizationRequirement`] the toolkit built — toolkit name,
/// endpoint and the resource-metadata URL, never a token or a response body —
/// travels with the error so the lifecycle can tell the person WHICH of their
/// connections is asking to be authorized. Discarding it left every challenge
/// indistinguishable from any other runtime failure.
pub struct NativeAgentAssemblyError {
    code: NativeAgentAssemblyErrorCode,
    message: &'static str,
    authorization: Option<Box<DelegatedAuthorizationRequirement>>,
    cause: Option<NativeAgentAssemblyCause>,
}

impl NativeAgentAssemblyError {
    pub const fn new(code: NativeAgentAssemblyErrorCode, message: &'static str) -> Self {
        Self {
            code,
            message,
            authorization: None,
            cause: None,
        }
    }

    /// Attach the worker's typed, data-free reason (see [`NativeAgentAssemblyCause`]).
    #[must_use]
    pub const fn with_cause(mut self, code: &'static str, detail: Option<&'static str>) -> Self {
        self.cause = Some(NativeAgentAssemblyCause { code, detail });
        self
    }

    #[must_use]
    pub const fn cause(&self) -> Option<&NativeAgentAssemblyCause> {
        self.cause.as_ref()
    }

    /// The saved agent instructions or settings exceed a platform bound.
    pub const fn agent_settings_limit(
        message: &'static str,
        cause_code: &'static str,
        detail: &'static str,
    ) -> Self {
        Self::new(NativeAgentAssemblyErrorCode::AgentSettingsLimit, message)
            .with_cause(cause_code, Some(detail))
    }

    /// Attach the sanitized delegated-authorization requirement, when the
    /// failure is one.
    #[must_use]
    pub fn with_authorization(
        mut self,
        authorization: Option<DelegatedAuthorizationRequirement>,
    ) -> Self {
        self.authorization = authorization.map(Box::new);
        self
    }

    #[must_use]
    pub fn authorization(&self) -> Option<&DelegatedAuthorizationRequirement> {
        self.authorization.as_deref()
    }

    #[must_use]
    pub const fn code(&self) -> NativeAgentAssemblyErrorCode {
        self.code
    }

    #[must_use]
    pub const fn retryable(&self) -> bool {
        matches!(
            self.code,
            NativeAgentAssemblyErrorCode::DependencyUnavailable
        )
    }
}

impl fmt::Debug for NativeAgentAssemblyError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("NativeAgentAssemblyError")
            .field("code", &self.code)
            .finish_non_exhaustive()
    }
}

impl fmt::Display for NativeAgentAssemblyError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(self.message)
    }
}

impl std::error::Error for NativeAgentAssemblyError {}

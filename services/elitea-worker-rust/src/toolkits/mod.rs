//! Frozen toolkit references admitted with one agent request.
//!
//! The current Main service resolves configured toolkit settings to immutable
//! references before dispatch. This module validates that frozen boundary but
//! deliberately performs no credential redemption, discovery, or invocation.
//!
//! The families, MCP, policy, snapshot, binding and materialisation live in
//! `elitea-agent-runtime` (ADR-0029 decision 2, stage 3) and are re-exported
//! here at their old paths, with the per-family behaviour suites and the SDK
//! conformance gate. The worker keeps what still parses the gRPC input
//! (`direct_request`, `direct_runtime`) and the two suites that compose a
//! family with worker modules (`artifact_tests`, `openapi_pipeline_tests`).

mod direct_request;
mod direct_runtime;

pub(crate) use direct_request::{DirectToolkitRequest, DirectToolkitRequestErrorCode};
pub(crate) use direct_runtime::{
    DirectToolkitRuntime, DirectToolkitRuntimeError, DirectToolkitRuntimeErrorCode,
};
#[allow(
    clippy::wildcard_imports,
    reason = "the moved toolkits keep their old paths: every module and item of \
              elitea_agent_runtime::toolkits is crate::toolkits::* here"
)]
pub(crate) use elitea_agent_runtime::toolkits::*;

/// Identity header names the platform edge projects for Main. Exact names, not
/// an `x-auth-` prefix: third-party APIs use `X-Auth-Token`, `X-Auth-Key` and
/// similar names for their own credentials.
const RESERVED_PLATFORM_HEADER_NAMES: [&str; 10] = [
    "x-auth-type",
    "x-auth-id",
    "x-auth-user-id",
    "x-auth-reference",
    "x-auth-signature",
    "x-auth-avatar",
    "x-auth-avatar-state",
    "x-auth-session-id",
    "x-auth-session-name",
    "x-auth-session-endpoint",
];

/// Header-name prefix owned by the platform for its signed identity and
/// execution context.
const RESERVED_PLATFORM_HEADER_PREFIX: &str = "x-elitea-";

/// Whether `name` (compared ASCII case-insensitively) is a platform-owned
/// header that user-configured toolkit headers must not carry.
#[must_use]
pub(crate) fn is_reserved_platform_header(name: &str) -> bool {
    RESERVED_PLATFORM_HEADER_NAMES
        .iter()
        .any(|reserved| name.eq_ignore_ascii_case(reserved))
        || name
            .as_bytes()
            .get(..RESERVED_PLATFORM_HEADER_PREFIX.len())
            .is_some_and(|head| {
                head.eq_ignore_ascii_case(RESERVED_PLATFORM_HEADER_PREFIX.as_bytes())
            })
}

#[cfg(test)]
mod reserved_header_tests {
    use super::is_reserved_platform_header;

    #[test]
    fn platform_identity_names_match_case_insensitively() {
        for name in [
            "x-auth-id",
            "X-Auth-Type",
            "X-AUTH-USER-ID",
            "X-Auth-Signature",
            "X-Auth-Avatar-State",
            "X-Elitea-Project-Id",
            "x-elitea-execution-id",
        ] {
            assert!(is_reserved_platform_header(name), "{name}");
        }
        for name in [
            "x-authz",
            "x-auth",
            "X-Authorization-Hint",
            "X-Custom",
            "x-elitea",
            "X-Auth-Token",
            "X-Auth-Key",
            "X-Auth-Email",
            "x-auth-id-extra",
            "",
        ] {
            assert!(!is_reserved_platform_header(name), "{name}");
        }
    }
}

#[cfg(test)]
mod artifact_tests;
#[cfg(test)]
mod openapi_pipeline_tests;

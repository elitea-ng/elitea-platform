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

/// Header-name prefixes owned by the platform edge and Main. Toolkit settings
/// and per-call arguments never set them on an outbound request.
const RESERVED_PLATFORM_HEADER_PREFIXES: [&str; 2] = ["x-auth-", "x-elitea-"];

/// Whether `name` (compared ASCII case-insensitively) is a platform-owned
/// header that user-configured toolkit headers must not carry.
#[must_use]
pub(crate) fn is_reserved_platform_header(name: &str) -> bool {
    RESERVED_PLATFORM_HEADER_PREFIXES.iter().any(|prefix| {
        name.as_bytes()
            .get(..prefix.len())
            .is_some_and(|head| head.eq_ignore_ascii_case(prefix.as_bytes()))
    })
}

#[cfg(test)]
mod reserved_header_tests {
    use super::is_reserved_platform_header;

    #[test]
    fn platform_prefixes_match_case_insensitively_on_the_dashed_prefix() {
        for name in [
            "x-auth-id",
            "X-Auth-Type",
            "X-AUTH-USER-ID",
            "X-Elitea-Project-Id",
        ] {
            assert!(is_reserved_platform_header(name), "{name}");
        }
        for name in [
            "x-authz",
            "x-auth",
            "X-Authorization-Hint",
            "X-Custom",
            "x-elitea",
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

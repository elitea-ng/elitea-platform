//! Native toolkit families, Streamable HTTP MCP, tool admission policy and
//! the flat tool namespace (ADR-0029 decision 2, stage 3 of `EXTRACTION.md`).
//!
//! Main freezes toolkit identity and redeems schema-declared secrets before a
//! host receives the turn, so nothing here redeems a credential or holds a
//! claim: a family is built from already-materialised settings
//! (`materialize`), and the one family that writes to the platform itself
//! (`artifact`) goes through the host's [`crate::host::PlatformWriter`]. The
//! cloud worker passes the settings its claim redeemed; the desktop host
//! never passes a credentialed family (it reaches those through
//! `RemoteToolkit`, ADR-0029 decision 3) but uses the MCP client and the
//! policy as they are.
//!
//! The `sql` family needs `sqlx` with its `postgres` and `mysql` drivers and is
//! compiled only with the `toolkit-sql` feature; without it a `sql` toolkit
//! is skipped as unsupported, like any family a runtime cannot serve.
//!
//! The per-family behaviour suites and the SDK conformance gate
//! (`sdk_conformance`, test-only, which reads elitea-main's toolkit schema snapshots)
//! live here, beside the code they test, so the families keep the
//! crate-private visibility they had in the worker. The worker re-exports
//! this module at its old path (`crate::toolkits`) and keeps the two modules
//! that still parse the gRPC input (`direct_request`, `direct_runtime`) and
//! the suites that compose a family with worker transport (`artifact`,
//! `openapi` in a pipeline); `families`, `families::artifact` and
//! `families::openapi` are public only under `test-support` for those.

// The test fixtures (`test-support`, never in a production build) panic, so
// this fires in some feature sets and targets and not others.
#![allow(clippy::missing_panics_doc)]
#![expect(
    clippy::missing_errors_doc,
    clippy::must_use_candidate,
    clippy::new_without_default,
    clippy::result_unit_err,
    reason = "moved verbatim from the worker, where these items were crate-private; \
              each error type documents its codes, and per-function sections \
              follow when the module's public API is reviewed"
)]

pub(crate) mod delegated_auth;
pub mod direct_execution;
// Public only to the worker's composition suites (`test-support`).
#[cfg(any(test, feature = "test-support"))]
pub mod families;
#[cfg(not(any(test, feature = "test-support")))]
pub(crate) mod families;
pub(crate) mod invocation;
pub(crate) mod materialize;
pub(crate) mod mcp;
pub(crate) mod mcp_error;
pub(crate) mod mcp_tool_cache;
pub(crate) mod policy;
pub mod snapshot;
pub mod tool_binding;

#[cfg(test)]
mod aha_tests;
#[cfg(test)]
mod azure_search_tests;
#[cfg(test)]
mod azure_tests;
#[cfg(test)]
mod direct_execution_tests;
#[cfg(test)]
mod elastic_tests;
#[cfg(test)]
mod gcp_tests;
#[cfg(test)]
mod github_tests;
#[cfg(test)]
mod gitlab_org_tests;
#[cfg(test)]
mod google_places_tests;
#[cfg(test)]
mod invocation_tests;
#[cfg(test)]
mod keycloak_tests;
#[cfg(test)]
mod kubernetes_tests;
#[cfg(test)]
mod mcp_tests;
#[cfg(test)]
mod openapi_bounds_tests;
#[cfg(test)]
mod openapi_tests;
#[cfg(test)]
mod policy_tests;
#[cfg(test)]
mod postman_tests;
#[cfg(test)]
mod rally_tests;
#[cfg(test)]
mod report_portal_tests;
#[cfg(test)]
mod salesforce_tests;
/// The SDK schema gate (ADR-0027 decision 8). Test-only: the worker's
/// artifact suite (`test-support`) asserts the same contract.
#[cfg(any(test, feature = "test-support"))]
pub mod sdk_conformance;
#[cfg(test)]
mod service_now_tests;
#[cfg(test)]
mod sharepoint_tests;
#[cfg(test)]
mod slack_tests;
#[cfg(test)]
mod snapshot_tests;
#[cfg(test)]
mod sonar_tests;
#[cfg(all(test, feature = "toolkit-sql"))]
mod sql_tests;
#[cfg(test)]
mod yagmail_tests;
#[cfg(test)]
mod zephyr_enterprise_tests;
#[cfg(test)]
mod zephyr_rest_tests;
#[cfg(test)]
mod zephyr_squad_tests;
#[cfg(test)]
mod zephyr_tests;

#[cfg(any(test, feature = "test-support"))]
pub use delegated_auth::delegated_authorization_error_fixture;
pub use delegated_auth::{
    DELEGATED_AUTHORIZATION_METADATA_KEY, DELEGATED_AUTHORIZATION_SCOPE_KEY,
    DelegatedAuthorizationCatalog, DelegatedAuthorizationRequirement,
    bind_authorization_model_tools, decode_declined_authorization_scope,
    decode_delegated_authorization_requirement, delegated_authorization_declined_result,
    delegated_authorization_granted_result, delegated_authorization_requirement,
    encode_delegated_authorization_requirement, hide_model_tools,
};
pub use families::artifact::ArtifactToolAuthority;
pub use materialize::{
    ToolsetMaterializationError, ToolsetMaterializationErrorCode,
    materialize_configured_toolsets_with_artifact_authority,
    materialize_configured_toolsets_with_tokens_and_authorization,
};
pub use mcp::{
    AdkHttpMcpConnector, McpConnector, McpMaterializationError, McpMaterializationErrorCode,
    RETIRED_SSE_MESSAGE, materialize_mcp_toolsets_with_tokens_and_authorization,
};
#[cfg(any(test, feature = "test-support"))]
pub use mcp::{RemoteMcpConfig, mcp_authorization_required_fixture};
pub use policy::{
    SensitiveToolPolicy, ToolAdmissionDecision, ToolAdmissionPolicy, ToolAdmissionPolicyError,
    ToolAdmissionPolicyErrorCode,
};
pub use snapshot::{
    AdmittedToolSnapshot, FrozenToolKind, FrozenToolSnapshot, FrozenToolSnapshotError,
    FrozenToolSnapshotErrorCode,
};
pub use tool_binding::{
    FrozenToolset, ToolBindingError, ToolBindingPlan, bind_frozen_toolsets, bind_toolsets,
    freeze_toolsets,
};

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

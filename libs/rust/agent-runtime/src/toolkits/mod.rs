//! Native toolkit families, Streamable HTTP MCP, tool admission policy and
//! the flat tool namespace (ADR-0029 decision 2, stage 3 of `EXTRACTION.md`).
//!
//! Main freezes toolkit identity and redeems schema-declared secrets before a
//! host receives the turn, so nothing here redeems a credential or holds a
//! claim: a family is built from already-materialised settings
//! ([`materialize`]), and the one family that writes to the platform itself
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
//! The worker re-exports this module at its old path (`crate::toolkits`) and
//! keeps the per-family behaviour suites, the SDK conformance gate and the
//! two modules that still parse the gRPC input (`direct_request`,
//! `direct_runtime`).
#![allow(dead_code)]
// Materialization remains capability-gated.
// Only the `test-support` fixtures (never in a production build) panic or
// expose a bare `len`, so these fire in some feature sets and not others.
#![allow(clippy::missing_panics_doc, clippy::len_without_is_empty)]
#![expect(
    clippy::missing_errors_doc,
    clippy::must_use_candidate,
    clippy::return_self_not_must_use,
    clippy::new_without_default,
    clippy::result_unit_err,
    reason = "moved verbatim from the worker, where these items were crate-private; \
              each error type documents its codes, and per-function sections \
              follow when the module's public API is reviewed"
)]

pub mod delegated_auth;
pub mod direct_execution;
pub mod families;
pub mod invocation;
pub mod materialize;
pub mod mcp;
pub mod mcp_error;
pub mod mcp_tool_cache;
pub mod policy;
pub mod snapshot;
pub mod tool_binding;

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

//! Frozen toolkit references admitted with one agent request.
//!
//! The current Main service resolves configured toolkit settings to immutable
//! references before dispatch. This module validates that frozen boundary but
//! deliberately performs no credential redemption, discovery, or invocation.

#![allow(dead_code)] // Materialization remains capability-gated.

mod delegated_auth;
mod direct_execution;
mod families;
mod invocation;
mod materialize;
mod mcp;
mod policy;
mod snapshot;
mod tool_binding;

#[cfg(test)]
pub(crate) use delegated_auth::delegated_authorization_error_fixture;
pub(crate) use delegated_auth::{
    DELEGATED_AUTHORIZATION_METADATA_KEY, DelegatedAuthorizationCatalog,
    DelegatedAuthorizationRequirement, decode_delegated_authorization_requirement,
    delegated_authorization_declined_result, delegated_authorization_requirement,
    encode_delegated_authorization_requirement,
};
pub(crate) use materialize::{
    ToolsetMaterializationError, ToolsetMaterializationErrorCode,
    materialize_configured_toolsets_with_tokens_and_authorization,
};
pub(crate) use mcp::{
    AdkHttpMcpConnector, McpConnector, McpMaterializationError, McpMaterializationErrorCode,
    materialize_mcp_toolsets_with_tokens_and_authorization,
};
#[cfg(test)]
pub(crate) use mcp::{RemoteMcpConfig, mcp_authorization_required_fixture};
pub(crate) use policy::{
    SensitiveToolPolicy, ToolAdmissionDecision, ToolAdmissionPolicy, ToolAdmissionPolicyError,
    ToolAdmissionPolicyErrorCode,
};
pub(crate) use snapshot::{
    AdmittedToolSnapshot, FrozenToolKind, FrozenToolSnapshot, FrozenToolSnapshotError,
    FrozenToolSnapshotErrorCode,
};
pub(crate) use tool_binding::{
    FrozenToolset, ToolBindingError, ToolBindingPlan, bind_frozen_toolsets, bind_toolsets,
    freeze_toolsets,
};

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
#[cfg(test)]
mod sql_tests;
#[cfg(test)]
mod yagmail_tests;
#[cfg(test)]
mod zephyr_squad_tests;
#[cfg(test)]
mod zephyr_tests;

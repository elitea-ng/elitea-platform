//! Frozen toolkit references admitted with one agent request.
//!
//! The current Main service resolves configured toolkit settings to immutable
//! references before dispatch. This module validates that frozen boundary but
//! deliberately performs no credential redemption, discovery, or invocation.
//!
//! The families, MCP, policy, snapshot, binding and materialisation live in
//! `elitea-agent-runtime` (ADR-0029 decision 2, stage 3) and are re-exported
//! here at their old paths. The worker keeps what still parses the gRPC
//! input (`direct_request`, `direct_runtime`), the per-family behaviour
//! suites and the SDK conformance gate, which reads elitea-main's toolkit
//! schema snapshot.

#![allow(dead_code)] // Materialization remains capability-gated.

mod direct_request;
mod direct_runtime;
mod sdk_conformance;

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

#[cfg(test)]
mod aha_tests;
#[cfg(test)]
mod artifact_tests;
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
mod openapi_pipeline_tests;
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

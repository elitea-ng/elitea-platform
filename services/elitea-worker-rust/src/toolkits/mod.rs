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

#[cfg(test)]
mod artifact_tests;
#[cfg(test)]
mod openapi_pipeline_tests;

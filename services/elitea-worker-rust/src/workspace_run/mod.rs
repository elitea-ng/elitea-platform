//! The cloud executor (ADR-0034 decisions 7–10): a cloud turn of a
//! handed-off Local work thread.
//!
//! The worker, outside the sandbox, moves the bytes: it fetches the thread's
//! segment over the gRPC runtime plane, verifies and checks it out, re-scans
//! it for secrets, and at the end of the turn commits, bundles, uploads and
//! publishes. The sandbox only runs the tools.
//!
//! Composition: `agents::ordinary` takes a [`turn::WorkspaceRunFactory`]
//! (`with_workspace_runs`). Production builds none until the sandbox
//! supervisor serves workspace runs (a [`sandbox::WorkspaceSandboxLauncher`]
//! over its helper protocol); until then a cloud turn fails with
//! `workspace_run.unavailable` instead of running without its files.
#![allow(
    dead_code,
    reason = "composed once the sandbox supervisor serves workspace runs (ADR-0034 executor deviations)"
)]

pub(crate) mod codec;
pub(crate) mod context;
pub(crate) mod gix_codec;
pub(crate) mod redact;
pub(crate) mod sandbox;
pub(crate) mod scan;
pub(crate) mod store;
pub(crate) mod tools;
pub(crate) mod turn;
#[cfg(test)]
mod turn_tests;

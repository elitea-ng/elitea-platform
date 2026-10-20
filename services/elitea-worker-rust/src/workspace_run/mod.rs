//! The cloud executor (ADR-0034 decisions 7–10): a cloud turn of a
//! handed-off Local work thread.
//!
//! The worker, outside the sandbox, moves the bytes: it fetches the thread's
//! segment over the gRPC runtime plane, verifies and checks it out, re-scans
//! it for secrets, and at the end of the turn commits, bundles, uploads and
//! publishes. The sandbox only runs the tools.
//!
//! Composition: `bootstrap.rs` builds a [`turn::WorkspaceRunFactory`] when
//! the deployment names the sandbox supervisor's `workspace` profile
//! (`workspace_runs` in the runtime config): the gRPC store, the gix codec,
//! the [`supervisor::SupervisorWorkspaceLauncher`] and claim-bound grants.
//! `agents::ordinary` takes it (`with_workspace_runs`). Without it a cloud
//! turn fails with `workspace_run.unavailable` instead of running without
//! its files.

pub(crate) mod audit;
pub(crate) mod codec;
pub(crate) mod context;
pub(crate) mod file_redact;
pub(crate) mod gix_codec;
pub(crate) mod pr_diff;
pub(crate) mod redact;
pub(crate) mod sandbox;
pub(crate) mod scan;
pub(crate) mod store;
pub(crate) mod supervisor;
pub(crate) mod tools;
pub(crate) mod turn;
#[cfg(test)]
mod turn_tests;
pub(crate) mod wrap_up;

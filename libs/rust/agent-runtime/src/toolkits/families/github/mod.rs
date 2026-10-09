//! GitHub configuration, connection probing, and native ADK tools.
//!
//! The family consumes Main's already-authorized, claim-materialized settings.
//! It does not resolve saved configuration IDs or read environment credentials.
//! A single origin-bound HTTP client owns pooling for the lifetime of one
//! invocation. The public check-connection API remains in Main and will call
//! this same probe boundary rather than reimplement GitHub authentication.

#![allow(dead_code)] // Production tool assembly remains capability-gated.

pub(in crate::toolkits) mod client;
pub(in crate::toolkits) mod code_search;
pub(in crate::toolkits) mod commits;
pub(in crate::toolkits) mod config;
pub(in crate::toolkits) mod projects;
pub(in crate::toolkits) mod pull_requests;
pub(in crate::toolkits) mod tools;
pub(in crate::toolkits) mod workflow_runs;

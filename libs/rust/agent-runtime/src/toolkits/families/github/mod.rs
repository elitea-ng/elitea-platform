//! GitHub configuration, connection probing, and native ADK tools.
//!
//! The family consumes Main's already-authorized, claim-materialized settings.
//! It does not resolve saved configuration IDs or read environment credentials.
//! A single origin-bound HTTP client owns pooling for the lifetime of one
//! invocation. The public check-connection API remains in Main and will call
//! this same probe boundary rather than reimplement GitHub authentication.

#![allow(dead_code)] // Production tool assembly remains capability-gated.

pub mod client;
pub mod code_search;
pub mod commits;
pub mod config;
pub mod projects;
pub mod pull_requests;
pub mod tools;
pub mod workflow_runs;

//! `ReportPortal` configuration, bounded read client, and native ADK tools.
//!
//! Main materializes one project's endpoint and credential for an accepted
//! claim. This family keeps that authority invocation-local and exposes only
//! the nine read operations in the current SDK catalog.

#![allow(dead_code)] // Production toolkit assembly remains capability-gated.

pub mod client;
pub mod config;
pub mod tools;

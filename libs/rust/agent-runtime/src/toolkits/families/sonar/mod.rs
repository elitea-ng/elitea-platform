//! Sonar configuration, bounded issue-search client, and native ADK tool.
//!
//! The family consumes Main's claim-materialized project and credential. It
//! exposes only the issue-search endpoint promised by the current SDK tool
//! description; it is not a generic Sonar Web API escape hatch.

#![allow(dead_code)] // Production toolkit assembly remains capability-gated.

pub(in crate::toolkits) mod client;
pub(in crate::toolkits) mod config;
pub(in crate::toolkits) mod tools;

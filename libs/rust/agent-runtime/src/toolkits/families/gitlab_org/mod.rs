//! Complete capability-disabled GitLab Org toolkit family.
//!
//! The family owns one claim-scoped GitLab authority, either a fixed repository
//! allowlist or capability-gated organization-wide project authority, and
//! invocation-local active-branch state. It is intentionally distinct from the
//! standard GitLab toolkit catalog.

#![allow(dead_code)] // Production toolkit assembly remains capability-gated.

pub(in crate::toolkits) mod client;
pub(in crate::toolkits) mod config;
pub(in crate::toolkits) mod diff;
pub(in crate::toolkits) mod edit;
pub(in crate::toolkits) mod tools;

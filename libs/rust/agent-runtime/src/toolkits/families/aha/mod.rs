//! Complete capability-disabled Aha! toolkit family.
//!
//! The family owns one claim-scoped Aha origin and bearer credential. Remote
//! effects, including artifact attachment, remain unavailable to production
//! activation until durable effect receipts and artifact-read grants are wired.

#![allow(dead_code)] // Production toolkit assembly remains capability-gated.

pub(in crate::toolkits) mod artifact;
pub(in crate::toolkits) mod client;
pub(in crate::toolkits) mod config;
pub(in crate::toolkits) mod format;
pub(in crate::toolkits) mod tools;

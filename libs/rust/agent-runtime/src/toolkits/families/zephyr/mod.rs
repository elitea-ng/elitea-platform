//! Complete, capability-disabled legacy Zephyr ZAPI toolkit family.
//!
//! This is the four-tool Jira test-step family registered as `zephyr`. It is
//! separate from Zephyr Scale, Zephyr Squad Cloud, Zephyr Essential, and
//! Zephyr Enterprise. One invocation owns one frozen HTTPS ZAPI base and one
//! Basic-auth credential pair; no provider authority is process-global.

#![allow(dead_code)] // Production toolkit assembly remains capability-gated.

pub(in crate::toolkits) mod client;
pub(in crate::toolkits) mod config;
pub(in crate::toolkits) mod tools;

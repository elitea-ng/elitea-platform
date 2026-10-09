//! Complete, capability-disabled Zephyr Squad toolkit family.
//!
//! The implementation preserves all fifteen public SDK operations while
//! replacing its process-global client with one invocation-scoped, bounded
//! JWT HTTP authority.

#![allow(dead_code)] // Production toolkit assembly remains capability-gated.

pub(in crate::toolkits) mod client;
pub(in crate::toolkits) mod config;
pub(in crate::toolkits) mod tools;

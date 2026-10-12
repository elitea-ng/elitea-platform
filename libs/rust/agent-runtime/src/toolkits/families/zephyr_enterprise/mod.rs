//! Zephyr Enterprise (`flex/services/rest/latest`) toolkit family.
//!
//! The five business operations of the SDK's `ZephyrEnterpriseToolkit` over
//! one invocation-scoped bearer client. The SDK's six indexing tools are not
//! served: indexing does not exist in this runtime, so the capability snapshot
//! lists this family per tool and the catalogue marks the rest unavailable.

#![allow(dead_code)] // Production toolkit assembly remains capability-gated.

pub(in crate::toolkits) mod client;
pub(in crate::toolkits) mod config;
pub(in crate::toolkits) mod tools;

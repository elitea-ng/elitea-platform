//! Capability-disabled Postman collection management and request execution.
//!
//! Management calls are confined to one claim-owned Postman origin. Executing
//! a stored request is a separate dynamic-egress capability and has no
//! production authority constructor until the platform can bind an approved
//! downstream origin to the invocation.

#![allow(dead_code)] // The complete family remains capability-gated.

pub(in crate::toolkits) mod analysis;
pub(in crate::toolkits) mod client;
pub mod collection;
pub(in crate::toolkits) mod config;
pub(in crate::toolkits) mod tools;

//! Capability-disabled Postman collection management and request execution.
//!
//! Management calls are confined to one claim-owned Postman origin. Executing
//! a stored request is a separate dynamic-egress capability and has no
//! production authority constructor until the platform can bind an approved
//! downstream origin to the invocation.

#![allow(dead_code)] // The complete family remains capability-gated.

pub mod analysis;
pub mod client;
pub mod collection;
pub mod config;
pub mod tools;

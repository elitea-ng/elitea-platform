//! Complete, capability-disabled Salesforce REST toolkit.
//!
//! The family preserves all six current SDK tools, including create, update,
//! generic execute and delete-capable requests. Production registration stays
//! closed until claim materialization, durable exact-ID approval and
//! cancellation-safe effect reconciliation are composed around these tools.

#![allow(dead_code)] // Production toolkit assembly remains capability-gated.

pub(in crate::toolkits) mod client;
pub(in crate::toolkits) mod config;
pub(in crate::toolkits) mod tools;

//! Capability-disabled SQL toolkit configuration, admission, and bounded drivers.
//!
//! The family preserves the SDK's two-tool catalog for `PostgreSQL` and `MySQL`,
//! while treating arbitrary SQL as an effect. Production registration remains
//! gated on durable effect receipts and verified database/TLS authority.

#![allow(dead_code)] // The complete family remains capability-gated.

pub(in crate::toolkits) mod client;
pub(in crate::toolkits) mod config;
pub(in crate::toolkits) mod lexer;
pub(in crate::toolkits) mod project;
pub(in crate::toolkits) mod tools;

//! Capability-disabled SQL toolkit configuration, admission, and bounded drivers.
//!
//! The family preserves the SDK's two-tool catalog for `PostgreSQL` and `MySQL`,
//! while treating arbitrary SQL as an effect. Production registration remains
//! gated on durable effect receipts and verified database/TLS authority.

#![allow(dead_code)] // The complete family remains capability-gated.

pub mod client;
pub mod config;
pub mod lexer;
pub mod project;
pub mod tools;

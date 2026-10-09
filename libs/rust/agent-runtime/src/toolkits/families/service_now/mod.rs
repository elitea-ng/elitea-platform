//! Capability-disabled `ServiceNow` incident tools.
//!
//! The current SDK is the behavioral source, while this implementation uses
//! `ServiceNow`'s Table API directly instead of recreating `pysnc`'s mutable,
//! process-global wrapper state. Two operations can create external effects;
//! production assembly therefore remains disabled until the shared durable
//! sensitive-tool/HITL wrapper is composed.

#![allow(dead_code)] // Production toolkit assembly remains capability-gated.

pub(in crate::toolkits) mod client;
pub(in crate::toolkits) mod config;
pub(in crate::toolkits) mod tools;

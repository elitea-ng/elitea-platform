//! Complete, capability-disabled Rally toolkit family.
//!
//! The implementation preserves the SDK's eight public operations while
//! replacing its eager, process-global `pyral` client with one lazy,
//! invocation-scoped, bounded HTTP authority.

#![allow(dead_code)] // Production toolkit assembly remains capability-gated.

pub(in crate::toolkits) mod client;
pub(in crate::toolkits) mod config;
pub(in crate::toolkits) mod tools;

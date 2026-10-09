//! Complete, capability-disabled Google Cloud REST toolkit family.
//!
//! One claim-owned service-account key signs a short-lived scoped OAuth grant.
//! The resulting token is usable only for one bounded Google API invocation.

#![allow(dead_code)] // Production toolkit assembly remains capability-gated.

pub mod client;
pub mod config;
pub mod tools;

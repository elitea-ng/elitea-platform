//! Capability-disabled Azure Resource Manager toolkit family.
//!
//! The family owns one claim-scoped public-cloud ARM subscription authority.
//! Generic execution remains unavailable to production assembly until durable
//! approval, effect reconciliation, and external egress enforcement exist.

#![allow(dead_code)] // Production toolkit assembly remains capability-gated.

pub(in crate::toolkits) mod client;
pub(in crate::toolkits) mod config;
pub(in crate::toolkits) mod tools;

//! Complete, capability-disabled Elasticsearch read toolkit family.
//!
//! The family owns one exact claim-materialized cluster origin and one optional
//! encoded API key. It never inherits ambient proxy, credential, or TLS policy.

#![allow(dead_code)] // Production toolkit assembly remains capability-gated.

pub(in crate::toolkits) mod client;
pub(in crate::toolkits) mod config;
pub(in crate::toolkits) mod tools;

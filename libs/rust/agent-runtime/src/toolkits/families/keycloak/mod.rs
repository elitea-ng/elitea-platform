//! Capability-disabled Keycloak Admin REST family.
//!
//! The family owns one frozen HTTPS authority and one service-account client.
//! Production registration remains closed until direct-tool approval and
//! cancellation-safe effect reconciliation are composed by the agent runtime.

#![allow(dead_code)] // Production toolkit assembly remains capability-gated.

pub(in crate::toolkits) mod client;
pub(in crate::toolkits) mod config;
pub(in crate::toolkits) mod tools;

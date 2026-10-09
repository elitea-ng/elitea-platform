//! Complete, capability-disabled Kubernetes REST toolkit family.
//!
//! The family keeps both SDK operations while replacing ambient kubeconfig and
//! certificate-verification bypasses with one claim-owned HTTPS cluster origin.

#![allow(dead_code)] // Production toolkit assembly remains capability-gated.

pub(in crate::toolkits) mod client;
pub(in crate::toolkits) mod config;
pub(in crate::toolkits) mod tools;

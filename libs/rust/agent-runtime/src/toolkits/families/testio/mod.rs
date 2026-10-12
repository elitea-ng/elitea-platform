//! Partial Test IO toolkit family: the thirteen customer-API reads.
//!
//! The SDK's two writes (`create_exploratory_test`, `confirm_bug_fix`) send
//! legacy payloads that map to no current Test IO customer-API operation, so
//! they are not served rather than guessed (see
//! `services/elitea-worker-rust/docs/source-mapping/configuration-toolsets.md`).
//! The SDK has no index tools for this type.

#![allow(dead_code)] // Production toolkit assembly remains capability-gated.

pub(in crate::toolkits) mod client;
pub(in crate::toolkits) mod config;
pub(in crate::toolkits) mod tools;

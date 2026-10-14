//! Test IO toolkit family: the thirteen customer-API reads and the two SDK
//! writes (`create_exploratory_test`, `confirm_bug_fix`).
//!
//! The writes send the SDK's payloads verbatim to `POST /customer/v2/...`
//! and are never retried: a failure after the request may have been sent is
//! an unknown outcome. The SDK has no index tools for this type.

#![allow(dead_code)] // Production toolkit assembly remains capability-gated.

pub(in crate::toolkits) mod client;
pub(in crate::toolkits) mod config;
pub(in crate::toolkits) mod tools;

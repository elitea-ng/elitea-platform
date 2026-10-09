//! Azure AI Search configuration, bounded document client, and native tools.
//!
//! The family implements the two reads registered by the current SDK. Dormant
//! vector and hybrid helpers, Azure `OpenAI` settings, and the SDK's mismatched
//! connection probe are deliberately outside this capability-disabled slice.

#![allow(dead_code)] // Production toolkit assembly remains capability-gated.

pub(in crate::toolkits) mod client;
pub(in crate::toolkits) mod config;
pub(in crate::toolkits) mod tools;

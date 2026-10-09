//! Complete, capability-disabled Yagmail toolkit family.
//!
//! The family preserves the SDK's single SMTP send operation while replacing
//! its process-global client, unverified TLS, implicit local-file attachment,
//! and post-DATA retries with one claim-scoped, bounded SMTP authority.

#![allow(dead_code)] // Production toolkit assembly remains capability-gated.

pub mod client;
pub mod config;
pub mod tools;

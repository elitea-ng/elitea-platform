//! Partial Xray Cloud toolkit family (`xray_cloud`): GraphQL over
//! client-credential token authentication.
//!
//! All six business tools are served. `add_attachment_to_test_step` serves
//! its `filedata` form; its `filepath` form reads raw bytes from artifact
//! storage, which this runtime's artifact authority does not expose, and is
//! answered with that explanation. The six inherited index tools wait for
//! indexing in Rust.

#![allow(dead_code)] // Production toolkit assembly remains capability-gated.

pub(in crate::toolkits) mod client;
pub(in crate::toolkits) mod config;
pub(in crate::toolkits) mod tools;

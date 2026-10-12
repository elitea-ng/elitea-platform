//! Shared bearer-token JSON transport for the Zephyr REST families.
//!
//! Zephyr Essential and Zephyr Scale both speak the Zephyr Scale Cloud v2 API
//! (`/testcases`, `/folders`, ...), and Zephyr Enterprise speaks its own
//! `flex/services/rest/latest` API; all three authenticate with one bearer
//! token and exchange JSON. This module owns that one shape: invocation-scoped
//! configuration helpers, one hardened HTTPS client per toolset, bounded
//! responses and stable, credential-free failures. Each family keeps its own
//! error-code namespace through [`client::ZephyrRestFamily`].

#![allow(dead_code)] // Production toolkit assembly remains capability-gated.

pub(in crate::toolkits) mod client;
pub(in crate::toolkits) mod config;
pub(in crate::toolkits) mod tools;

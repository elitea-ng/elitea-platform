//! Zephyr Essential (Zephyr Scale Cloud v2 API) toolkit family.
//!
//! The SDK's `ZephyrEssentialToolkit` business operations over one
//! invocation-scoped bearer client. Not served: the six indexing tools
//! (indexing does not exist in this runtime), the three automation-result
//! uploads, whose SDK `files` argument is a path string that `requests`
//! cannot encode, and the BDD archive download, which answers with a binary
//! ZIP no text result can carry. The capability snapshot lists this family per
//! tool, so the catalogue marks the rest unavailable.

#![allow(dead_code)] // Production toolkit assembly remains capability-gated.

pub(in crate::toolkits) mod client;
pub(in crate::toolkits) mod config;
pub(in crate::toolkits) mod tools;

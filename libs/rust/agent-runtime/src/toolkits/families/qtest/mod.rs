//! Partial qTest Manager toolkit family (v3 REST API, Bearer token).
//!
//! Seventeen of the SDK's twenty-five tools are served: the DQL searches,
//! entity/relationship lookups, modules, field definitions and versions, and
//! the test-case, requirement-link and test-run-status effects.
//! `add_file_to_test_case` and `upload_attachment_to_test_run` are not
//! served: they upload raw bytes read from artifact storage, and this
//! runtime's artifact authority returns decoded text, not bytes. The six
//! inherited index tools wait for indexing in Rust. The SDK's optional LLM
//! transcription of embedded images is not available in-family; embedded
//! images are removed from the text, as the SDK removes them by default.

#![allow(dead_code)] // Production toolkit assembly remains capability-gated.

pub(in crate::toolkits) mod client;
pub(in crate::toolkits) mod config;
mod effects;
mod fields;
mod html;
mod ops;
mod schema;
mod search;
pub(in crate::toolkits) mod tools;

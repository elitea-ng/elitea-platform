//! Partial `TestRail` toolkit family (`testrail_api==1.13.4` wire contract).
//!
//! Sixteen of the SDK's twenty-three tools are served: every case, suite,
//! section, run and result read plus the case and section effects.
//! `add_file_to_case` is not served because it uploads raw bytes read from
//! artifact storage, and this runtime's artifact authority returns decoded
//! text, not bytes. The six inherited index tools wait for indexing in Rust.

#![allow(dead_code)] // Production toolkit assembly remains capability-gated.

pub(in crate::toolkits) mod client;
pub(in crate::toolkits) mod config;
mod format;
mod schema;
pub(in crate::toolkits) mod tools;

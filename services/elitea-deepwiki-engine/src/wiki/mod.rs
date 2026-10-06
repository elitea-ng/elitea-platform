//! Wiki page generation and export (ADR-0026 phase 5b).
//!
//! The live Python path after the structure planner, ported module by
//! module:
//!
//! * [`spec`] — the structure and page models (`state/wiki_state.py`);
//! * [`pyregex`] — Python `re` semantics for the transcribed patterns;
//! * [`sanitizer`] — `diagram_sanitizer.py`.
#![cfg_attr(
    not(test),
    deny(clippy::expect_used, clippy::panic, clippy::unwrap_used)
)]
// `macro_id` / `micro_id` (and the sanitizer's `fix` / `fixes`) are the
// Python names, kept so the port reads line for line against it.
#![allow(clippy::similar_names)]

pub mod compose;
pub mod context;
pub mod doc;
pub mod expansion;
pub mod export;
pub mod index;
pub mod pages;
pub mod parity;
pub mod prompts;
pub mod pyregex;
pub mod ranker;
pub mod repo_files;
pub mod retrieve;
pub mod run;
pub mod sanitizer;
pub mod search;
pub mod spec;

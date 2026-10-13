//! The `pptx` toolkit family (ADR-0032 P3): create, read, edit, fill,
//! translate, preview and export `PowerPoint` files, in process, on
//! `elitea-office-core`.
//!
//! The family names no zavora type: the deck is [`elitea_office_core::Deck`],
//! bytes in and out, behind the size, depth and time limits that crate
//! enforces before a library sees a byte. Artifacts are read and written
//! through [`super::office_host::OfficeToolHost`] (object bytes, which the
//! text-only `artifact` family cannot carry), and the two tools that call a
//! model do so through the host's `complete`, which the worker binds on the
//! run's own model path: the same gateway, credential, budget and request
//! log as every other model call of the turn.
//!
//! `fill_template` and `translate_presentation` keep the SDK's names,
//! arguments, prompts, batching and result shapes
//! (`elitea_sdk/tools/pptx/pptx_wrapper.py`). The deterministic halves are
//! `list_placeholders` and `replace_placeholders`; the rest of the family is
//! authoring. `SmartArt` text is not read or edited (a stated gap until
//! `office-core` has a raw-part editor).

#![allow(dead_code)] // Production toolkit assembly remains capability-gated.

pub(crate) mod config;
pub(crate) mod model;
pub(crate) mod ops;
pub(crate) mod schema;
pub(crate) mod tools;

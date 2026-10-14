//! The `docx` toolkit family (ADR-0032 P4): create, read, edit, tables,
//! charts, template replace and export to PDF for Word files, in process, on
//! `elitea-office-core`.
//!
//! The family names no zavora type: the document is
//! [`elitea_office_core::Document`], bytes in and out, behind the size, depth
//! and time limits that crate enforces before a library sees a byte. Artifacts
//! are read and written through [`super::office_host::OfficeToolHost`] (object
//! bytes, which the text-only `artifact` family cannot carry).
//!
//! The SDK has no `docx` toolkit, so the tool names are this family's own,
//! following the `pptx` family's conventions. There is no model-calling tool:
//! `list_placeholders` and `replace_placeholders` are the deterministic
//! template halves.
//!
//! Stated gaps: content is appended at the end (there is no insert-at or
//! delete), a paragraph's runs are replaced as a whole by
//! `set_paragraph_text`, headers, footers and text boxes are replaced in but
//! not listed or read, and `SmartArt` and tracked changes are neither read nor
//! edited.

#![allow(dead_code)] // Production toolkit assembly remains capability-gated.

pub(crate) mod config;
pub(crate) mod ops;
pub(crate) mod schema;
pub(crate) mod tools;

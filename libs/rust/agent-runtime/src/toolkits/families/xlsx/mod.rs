//! The `xlsx` toolkit family (ADR-0032 P4): create, read, edit, sheets,
//! cells and charts for Excel files, in process, on `elitea-office-core`.
//!
//! The family names no zavora type: the workbook is
//! [`elitea_office_core::Workbook`], bytes in and out, behind the size, depth
//! and time limits that crate enforces before a library sees a byte. Artifacts
//! are read and written through [`super::office_host::OfficeToolHost`] (object
//! bytes, which the text-only `artifact` family cannot carry).
//!
//! The SDK has no `xlsx` or `excel` toolkit (its `pandas` toolkit writes
//! `.xlsx` through `df.to_excel` and is not superseded here), so the tool names
//! are this family's own.
//!
//! Stated gaps: there is no render and no PDF (a spreadsheet is previewed as a
//! Markdown table), formulas are stored and not calculated, error values and
//! dates cannot be written, a workbook that stores text as inline strings can
//! be read but not edited (the library reads those cells back as empty, so a
//! save would erase them), renaming or deleting a sheet that a formula or a
//! chart names is refused, and styles, merged cells, validation and
//! conditional formats are neither read nor edited by any tool.

#![allow(dead_code)] // Production toolkit assembly remains capability-gated.

pub(crate) mod config;
pub(crate) mod ops;
pub(crate) mod schema;
pub(crate) mod tools;

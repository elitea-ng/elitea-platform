//! The JavaScript / JSX parser: a port of the Python engine's
//! `parsers/javascript_visitor_parser.py` (`JavaScriptVisitorParser`).
//!
//! [`JavaScriptParser::parse_files`] is `parse_multiple_files`:
//!
//! 1. every file is parsed on its own (`visitor`) — in parallel here —
//!    keeping its import bindings and export entries;
//! 2. in the caller's (sorted) order: the global export index (export name
//!    → every entry, file order), the per-file class → method index, and
//!    the global symbol registry (top-level class, function and constant
//!    name → the FIRST file defining it);
//! 3. per file (`resolve`): import bindings are resolved to export entries
//!    (relative specifiers through the file system, others through a
//!    unique global export), `imports`, `calls`, `references` and
//!    `inheritance` get their `target_file` and resolution annotations,
//!    duplicates are dropped, and dotted calls are matched against the
//!    method index.
//!
//! The file is read as Python's `open(path, 'r', encoding='utf-8')` reads
//! it — strict UTF-8 (an invalid byte fails the whole file with the
//! `UnicodeDecodeError` text), universal newlines, a BOM kept as U+FEFF —
//! and its `_byte_to_line_col` mixes byte offsets with code-point indexes
//! exactly as the Java parser's does, so both share
//! `crate::parsers::java::source`.
//!
//! Python's JavaScript parser does not call `BaseParser.validate_result`,
//! so neither does this one.
//!
//! Two differences are deliberate:
//!
//! * `imports` (the module specifiers) is a Python `set` turned into a list,
//!   so its order follows the interpreter's string hash seed; it is sorted
//!   here.
//! * Python visits the tree recursively under its default recursion limit,
//!   so a file nested some 500 levels deep fails with `maximum recursion
//!   depth exceeded` and no symbols (while the import bindings and exports
//!   found before the failure still enter the multi-file pass). The depth
//!   at which that happens depends on the caller's stack, so it cannot be
//!   reproduced; this parser parses such a file normally, on a large
//!   worker stack. No file of the parity corpora reaches the limit.

mod resolve;
mod visitor;

#[cfg(test)]
mod tests;

use super::LanguageParser;
use super::java::os_error_text;
use super::java::source::Source;
use rayon::prelude::*;
use std::collections::BTreeMap;
use visitor::FileOutput;

/// The JavaScript [`LanguageParser`].
#[derive(Debug, Default, Clone, Copy)]
pub struct JavaScriptParser;

/// Worker stack for the recursive walk: a long call chain or string
/// concatenation nests the tree deeply.
const WORKER_STACK: usize = 64 * 1024 * 1024;

impl LanguageParser for JavaScriptParser {
    fn language(&self) -> &'static str {
        "javascript"
    }

    fn parse_files(&self, files: &[String]) -> BTreeMap<String, super::model::ParseResult> {
        let parse_all = || -> Vec<FileOutput> { files.par_iter().map(|f| parse_path(f)).collect() };
        let outputs = match rayon::ThreadPoolBuilder::new()
            .stack_size(WORKER_STACK)
            .build()
        {
            Ok(pool) => pool.install(parse_all),
            Err(_) => files.iter().map(|f| parse_path(f)).collect(),
        };
        let results = resolve::resolve_cross_file(files, outputs);
        files.iter().cloned().zip(results).collect()
    }
}

/// Read and parse one file as `parse_file` does: any failure (a missing
/// file, an invalid UTF-8 byte) is the exception text.
fn parse_path(path: &str) -> FileOutput {
    let bytes = match std::fs::read(path) {
        Ok(bytes) => bytes,
        Err(error) => return visitor::failed(path, os_error_text(&error, path)),
    };
    match Source::decode(&bytes) {
        Ok(source) => visitor::parse_source(path, &source),
        Err(error) => visitor::failed(path, error),
    }
}

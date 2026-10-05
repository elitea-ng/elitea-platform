//! The Java parser: the Python `JavaVisitorParser`
//! (`java_visitor_parser.py`).
//!
//! Two passes, as Python's `parse_multiple_files`:
//!
//! 1. every file alone (`visitor`), in parallel;
//! 2. over the files that produced symbols, in the caller's sorted order:
//!    fill `_global_class_locations` (class and interface name → file, the
//!    LAST file wins) and `_global_method_locations` (method name → every
//!    `(class, file)` whose class name is a SUBSTRING of the method's
//!    parent), then point `calls`, `creates`, `inheritance`,
//!    `implementation`, `imports` and `references` at another file when the
//!    registry places their target there.
//!
//! The registries are keyed by the bare name (no package), so two packages
//! that define `Owner` share one entry — the Python behaviour, kept because
//! the graph gate compares with it. A file that parsed without any symbol
//! (a `package-info.java`, a parse error) is left out of both steps, so
//! its `imports` keep their own file as target.
//!
//! Python's Java parser does not call `BaseParser.validate_result`, so
//! neither does this one: duplicate relationships are kept.
//!
//! One difference is deliberate: Python walks the tree recursively under
//! its default recursion limit (1000 frames), so a file nested some 500
//! levels deep (a long string concatenation or call chain) fails whole with
//! `maximum recursion depth exceeded` and no symbols. The exact depth
//! depends on the caller's own stack (a thread-pool worker in production,
//! inline in the reference dump), so it cannot be reproduced; this parser
//! parses such a file normally, on a large worker stack, up to
//! `limits::MAX_TREE_DEPTH` tree levels (deeper, the file fails alone with
//! `maximum recursion depth exceeded`).

/// Shared with the C# and JavaScript parsers, whose Python originals read
/// and position files the same way.
pub(super) mod source;
mod visitor;

#[cfg(test)]
mod tests;

use super::LanguageParser;
use super::limits;
use super::model::{ParseResult, Relationship, RelationshipType, SymbolType};
use rayon::prelude::*;
use source::Source;
use std::collections::{BTreeMap, HashMap};

/// The Java [`LanguageParser`].
#[derive(Debug, Default, Clone, Copy)]
pub struct JavaParser;

/// Worker stack for the recursive walk: a long call chain or string
/// concatenation nests the tree deeply (3000 levels were checked in the
/// parity run).
const WORKER_STACK: usize = 64 * 1024 * 1024;

impl LanguageParser for JavaParser {
    fn language(&self) -> &'static str {
        "java"
    }

    fn parse_files(&self, files: &[String]) -> BTreeMap<String, ParseResult> {
        let parse_all =
            || -> Vec<ParseResult> { files.par_iter().map(|f| parse_path(f)).collect() };
        let mut results = match limits::on_worker_pool("java", WORKER_STACK, parse_all) {
            Ok(results) => results,
            Err(error) => {
                return files
                    .iter()
                    .map(|f| (f.clone(), visitor::failed(f, error.clone())))
                    .collect();
            }
        };
        resolve_cross_file(files, &mut results);
        files.iter().cloned().zip(results).collect()
    }
}

/// Read and parse one file as `parse_file` does: a missing file is
/// `File not found: …`, any other failure (an invalid UTF-8 byte above all)
/// is the exception text.
fn parse_path(path: &str) -> ParseResult {
    let bytes = match std::fs::read(path) {
        Ok(bytes) => bytes,
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => {
            return visitor::failed(path, format!("File not found: {path}"));
        }
        Err(error) => return visitor::failed(path, os_error_text(&error, path)),
    };
    match Source::decode(&bytes) {
        Ok(source) => limits::with_output_budget(|| visitor::parse_source(path, &source))
            .unwrap_or_else(|error| visitor::failed(path, error)),
        Err(error) => visitor::failed(path, error),
    }
}

/// `str(OSError)` for a read failure: `[Errno 13] Permission denied:
/// 'path'` (Python's `repr` of the path is approximated by plain quotes).
pub(super) fn os_error_text(error: &std::io::Error, path: &str) -> String {
    let text = error.to_string();
    match error.raw_os_error() {
        Some(code) => {
            let suffix = format!(" (os error {code})");
            let message = text.strip_suffix(&suffix).unwrap_or(&text);
            format!("[Errno {code}] {message}: '{path}'")
        }
        None => text,
    }
}

/// The two cross-file registries. Values are indexes into the file list.
#[derive(Default)]
struct Registries<'r> {
    /// Class (records included) and interface name → file; last wins.
    classes: HashMap<&'r str, usize>,
    /// Method name → `(class name, file)`, in registration order, with
    /// repeats.
    methods: HashMap<&'r str, Vec<(&'r str, usize)>>,
}

/// `_extract_global_symbols` then `_enhance_cross_file_relationships` over
/// `results`, which are in `files`' order.
fn resolve_cross_file(files: &[String], results: &mut [ParseResult]) {
    let mut registries = Registries::default();
    for (index, result) in results.iter().enumerate() {
        for symbol in &result.symbols {
            if !matches!(
                symbol.symbol_type,
                SymbolType::Class | SymbolType::Interface
            ) {
                continue;
            }
            registries.classes.insert(symbol.name.as_str(), index);
            // Python quirk: `symbol.name in parent_symbol` is a substring
            // test, so `Owner`'s methods include `OwnerController`'s, and a
            // class whose name occurs in the package claims every method.
            for method in &result.symbols {
                if method.symbol_type == SymbolType::Method
                    && method
                        .parent_symbol
                        .as_deref()
                        .is_some_and(|p| !p.is_empty() && p.contains(symbol.name.as_str()))
                {
                    registries
                        .methods
                        .entry(method.name.as_str())
                        .or_default()
                        .push((symbol.name.as_str(), index));
                }
            }
        }
    }

    let updates: Vec<Vec<Option<Update>>> = results
        .iter()
        .enumerate()
        .map(|(index, result)| {
            if result.symbols.is_empty() {
                return Vec::new();
            }
            let imports = imported_names(result);
            result
                .relationships
                .iter()
                .map(|r| enhance(r, index, &imports, &registries))
                .collect()
        })
        .collect();
    for (result, updates) in results.iter_mut().zip(updates) {
        for (relationship, update) in result.relationships.iter_mut().zip(updates) {
            if let Some(update) = update {
                update.apply(relationship, files);
            }
        }
    }
}

/// The simple names this file imports: the last segment of every dotted
/// `imports` target (a wildcard import contributes its package's last
/// segment).
fn imported_names(result: &ParseResult) -> std::collections::HashSet<&str> {
    result
        .relationships
        .iter()
        .filter(|r| r.relationship_type == RelationshipType::Imports)
        .filter_map(|r| r.target_symbol.rsplit_once('.').map(|(_, last)| last))
        .collect()
}

/// What `_enhance_relationship` changes on one relationship.
struct Update {
    target_file: usize,
    /// A bare call resolved to `Class.method`.
    target_symbol: Option<String>,
    /// Python rebuilds the relationship without its annotations (for
    /// `references`, the parameter context is lost).
    clear_annotations: bool,
}

impl Update {
    fn apply(self, relationship: &mut Relationship, files: &[String]) {
        relationship.target_file = Some(files[self.target_file].clone());
        if let Some(target) = self.target_symbol {
            relationship.target_symbol = target;
        }
        if self.clear_annotations {
            relationship.annotations.clear();
        }
    }
}

/// `_enhance_relationship`: the change, if the target resolves to ANOTHER
/// file. Python rebuilds the relationship with default confidence and no
/// annotations except for `creates`; only `references` carries any
/// annotation (or confidence) that this loses.
///
/// The Python `if name in imports` branches look up the same registry as
/// their `else` branches, so whether a name is imported never matters —
/// except for which class a bare method call picks.
fn enhance(
    relationship: &Relationship,
    index: usize,
    imports: &std::collections::HashSet<&str>,
    registries: &Registries<'_>,
) -> Option<Update> {
    let other_file = |name: &str| {
        registries
            .classes
            .get(name)
            .copied()
            .filter(|&f| f != index)
    };
    let plain = |file: usize, clear_annotations: bool| Update {
        target_file: file,
        target_symbol: None,
        clear_annotations,
    };
    let target = relationship.target_symbol.as_str();
    match relationship.relationship_type {
        RelationshipType::Creates
        | RelationshipType::Inheritance
        | RelationshipType::Implementation => other_file(target).map(|f| plain(f, false)),
        RelationshipType::References => other_file(target).map(|f| plain(f, true)),
        RelationshipType::Imports => target
            .rsplit_once('.')
            .and_then(|(_, class)| other_file(class))
            .map(|f| plain(f, false)),
        RelationshipType::Calls => {
            if target.contains('.') {
                // Only `Class.method` (exactly two parts) is looked up.
                let mut parts = target.split('.');
                match (parts.next(), parts.next(), parts.next()) {
                    (Some(class), Some(_), None) => other_file(class).map(|f| plain(f, false)),
                    _ => None,
                }
            } else {
                // `_find_best_method_target`: the first location whose class
                // is imported, else the first registered.
                let locations = registries.methods.get(target)?;
                let &(class, file) = locations
                    .iter()
                    .find(|(class, _)| imports.contains(class))
                    .or_else(|| locations.first())?;
                (file != index).then(|| Update {
                    target_file: file,
                    target_symbol: Some(format!("{class}.{target}")),
                    clear_annotations: false,
                })
            }
        }
        _ => None,
    }
}

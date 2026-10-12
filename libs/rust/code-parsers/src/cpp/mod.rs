//! The C++ parser: the Python `CppEnhancedParser`
//! (`cpp_enhanced_parser.py`).
//!
//! Per file (`parse_file`), in parallel:
//!
//! 1. `symbols` — the `SymbolExtractor` pass, with the field, signature,
//!    alias and override relationships it keeps aside;
//! 2. `relations` — the `RelationshipExtractor` pass over those symbols;
//! 3. its relationships, then pass one's, have every `::` in a full name,
//!    parent, source and target replaced by `.`;
//! 4. `BaseParser.validate_result`: duplicates dropped (first wins), then
//!    every relationship whose source and target are both unknown to the
//!    file (a file-stem source counts as known).
//!
//! Then `parse_multiple_files`' two cross-file passes over the files that
//! produced symbols, in the caller's sorted order (Python's thread pool
//! collects `as_completed`; the reference dump makes that submission order):
//! registries of class, function and other symbol names → file, where the
//! FIRST file wins unless a later one is a header (`.h`, `.hpp`, `.hxx`,
//! `.h++` — not `.hh`), which always overwrites; then `inheritance`,
//! `calls`, `references`, `creates` and `defines_body` targets get the
//! registered file when it is another one.
//!
//! The registries hold names AFTER normalisation, so Python's extra
//! `Class::method` registration (guarded by `'::' in full_name`) never
//! fires and is not ported.
//!
//! One difference is deliberate: Python walks the tree recursively under
//! its default recursion limit (1000 frames), so a function body nested
//! some 450 levels deep fails the whole file with `Parse error: maximum
//! recursion depth exceeded`. The exact depth depends on the caller's
//! stack, so it cannot be reproduced; this parser parses such a file
//! normally, on a large worker stack, up to
//! `limits::MAX_TREE_DEPTH` tree levels (deeper, the file fails alone with
//! `maximum recursion depth exceeded`).

mod names;
mod relations;
mod source;
mod symbols;

#[cfg(test)]
mod tests;

use super::LanguageParser;
use super::limits;
use super::model::{ParseResult, RelationshipType, SymbolType};
use crate::input::Sources;
use elitea_engine_core::pystr::stem;
use rayon::prelude::*;
use source::Source;
use std::collections::{BTreeMap, HashMap};

/// The C++ [`LanguageParser`].
#[derive(Debug, Default, Clone, Copy)]
pub struct CppParser;

/// Worker stack for the recursive walks: a debug build's frames overflow a
/// 2 MiB stack on a deeply nested expression.
const WORKER_STACK: usize = 64 * 1024 * 1024;

impl LanguageParser for CppParser {
    fn language(&self) -> &'static str {
        "cpp"
    }

    fn parse_sources(
        &self,
        files: &[String],
        sources: Sources<'_>,
    ) -> BTreeMap<String, ParseResult> {
        let parse_all =
            || -> Vec<ParseResult> { files.par_iter().map(|f| parse_path(f, sources)).collect() };
        let mut results = match limits::on_worker_pool("C++", WORKER_STACK, parse_all) {
            Ok(results) => results,
            Err(error) => {
                return files
                    .iter()
                    .map(|f| (f.clone(), failed(f, error.clone())))
                    .collect();
            }
        };
        resolve_cross_file(files, &mut results);
        files.iter().cloned().zip(results).collect()
    }
}

/// `parse_file(path)`: a read failure is `Parse error: <OSError>`; bytes
/// never fail (`errors='ignore'`).
fn parse_path(path: &str, sources: Sources<'_>) -> ParseResult {
    match sources.read(path) {
        Ok(bytes) => {
            let source = Source::decode(&bytes);
            limits::with_output_budget(|| parse_source(path, &source))
                .unwrap_or_else(|error| failed(path, error.to_owned()))
        }
        Err(error) => failed(
            path,
            format!("Parse error: {}", os_error_text(&error, path)),
        ),
    }
}

fn failed(path: &str, error: String) -> ParseResult {
    let mut result = ParseResult::new(path, "cpp");
    result.errors.push(error);
    result
}

/// `str(OSError)`: `[Errno 2] No such file or directory: 'path'`.
fn os_error_text(error: &std::io::Error, path: &str) -> String {
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

/// One file's text through both passes, normalisation and validation.
fn parse_source(path: &str, source: &Source) -> ParseResult {
    let mut parser = tree_sitter::Parser::new();
    if parser
        .set_language(&tree_sitter_cpp::LANGUAGE.into())
        .is_err()
    {
        return failed(path, "Tree-sitter parser not available".to_owned());
    }
    let Some(tree) = parser.parse(source.bytes(), None) else {
        return failed(path, "Failed to parse C++ file".to_owned());
    };
    let root = tree.root_node();
    if limits::too_deep(root) {
        return failed(path, format!("Parse error: {}", limits::RECURSION_ERROR));
    }
    let file_stem = stem(path);
    let extracted = symbols::extract(root, source, path, file_stem);
    let mut symbols = extracted.symbols;
    let mut relationships = relations::extract(root, source, path, file_stem, &symbols);
    relationships.extend(extracted.relationships);
    for symbol in &mut symbols {
        normalise(&mut symbol.full_name);
        normalise(&mut symbol.parent_symbol);
    }
    for relationship in &mut relationships {
        relationship.source_symbol = relationship.source_symbol.replace("::", ".");
        relationship.target_symbol = relationship.target_symbol.replace("::", ".");
    }
    let mut result = ParseResult::new(path, "cpp");
    result.symbols = symbols;
    result.relationships = relationships;
    result.validate();
    result
}

/// `_normalize_full_name`: `::` → `.`.
fn normalise(name: &mut Option<String>) {
    if let Some(text) = name.as_mut().filter(|t| t.contains("::")) {
        *text = text.replace("::", ".");
    }
}

/// The three cross-file registries; values index the file list.
#[derive(Default)]
struct Registries {
    classes: HashMap<String, usize>,
    functions: HashMap<String, usize>,
    symbols: HashMap<String, usize>,
}

/// `name not in registry or is_header`: the first file wins, a header
/// always overwrites.
fn register(registry: &mut HashMap<String, usize>, name: &str, file: usize, is_header: bool) {
    if is_header || !registry.contains_key(name) {
        registry.insert(name.to_owned(), file);
    }
}

/// `_extract_global_symbols` then `_enhance_cross_file_relationships`.
fn resolve_cross_file(files: &[String], results: &mut [ParseResult]) {
    let mut registries = Registries::default();
    for (index, (path, result)) in files.iter().zip(results.iter()).enumerate() {
        if result.symbols.is_empty() {
            continue;
        }
        let is_header = [".h", ".hpp", ".hxx", ".h++"]
            .iter()
            .any(|suffix| path.ends_with(suffix));
        for symbol in &result.symbols {
            let full = symbol.full_name.as_deref().filter(|f| !f.is_empty());
            match symbol.symbol_type {
                SymbolType::Class | SymbolType::Struct => {
                    register(&mut registries.classes, &symbol.name, index, is_header);
                    if let Some(full) = full {
                        register(&mut registries.classes, full, index, is_header);
                        register(&mut registries.symbols, full, index, is_header);
                    }
                    register(&mut registries.symbols, &symbol.name, index, is_header);
                }
                SymbolType::Function | SymbolType::Method => {
                    register(&mut registries.functions, &symbol.name, index, is_header);
                    if let Some(full) = full {
                        register(&mut registries.functions, full, index, is_header);
                        register(&mut registries.symbols, full, index, is_header);
                    }
                    register(&mut registries.symbols, &symbol.name, index, is_header);
                }
                SymbolType::Constant | SymbolType::Macro => {
                    register(&mut registries.symbols, &symbol.name, index, is_header);
                    if let Some(full) = full {
                        register(&mut registries.symbols, full, index, is_header);
                    }
                }
                _ => {}
            }
        }
    }
    for result in results.iter_mut() {
        if result.symbols.is_empty() {
            continue;
        }
        for relationship in &mut result.relationships {
            let target = relationship.target_symbol.as_str();
            let other = |file: usize| (files[file] != relationship.source_file).then_some(file);
            let found = match relationship.relationship_type {
                RelationshipType::Inheritance
                | RelationshipType::Calls
                | RelationshipType::References
                | RelationshipType::Creates => registries
                    .classes
                    .get(target)
                    .or_else(|| registries.functions.get(target))
                    .or_else(|| registries.symbols.get(target))
                    .and_then(|&file| other(file)),
                RelationshipType::DefinesBody => registries
                    .functions
                    .get(target)
                    .and_then(|&file| other(file))
                    .or_else(|| registries.symbols.get(target).and_then(|&file| other(file))),
                _ => None,
            };
            if let Some(file) = found {
                relationship.target_file = Some(files[file].clone());
            }
        }
    }
}

//! The C# parser: the Python `CSharpVisitorParser`
//! (`csharp_visitor_parser.py`).
//!
//! A file is read as the Java parser reads it — strict UTF-8 (one invalid
//! byte fails the whole file), universal newlines, and positions that count
//! newlines by code point but columns by byte — so it shares
//! `java::source::Source`.
//!
//! Two passes, as Python's `parse_multiple_files`:
//!
//! 1. every file alone (`visitor`), in parallel;
//! 2. over the files that produced symbols, in the caller's sorted order:
//!    fill `_global_class_locations` (class, interface, struct and enum
//!    name → file, the LAST file wins) and `_global_method_locations`
//!    (method name → every `(type, file)` whose type name is a SUBSTRING of
//!    the method's parent), then point `calls`, `creates`, `inheritance`,
//!    `implementation`, `imports` and `references` at another file when the
//!    registry places their target there.
//!
//! The registries are keyed by the bare name (no namespace), so a partial
//! class split over files is registered to its last file, and two
//! namespaces that define `Result` share one entry — the Python behaviour,
//! kept because the graph gate compares with it.
//!
//! Python's C# parser does not call `BaseParser.validate_result`, so
//! neither does this one: duplicate relationships are kept.
//!
//! One difference is deliberate, as for Java: Python walks the tree
//! recursively under its default recursion limit (1000 frames), so a file
//! nested some 450 levels deep (a long string concatenation) fails whole
//! with `maximum recursion depth exceeded` and no symbols. The exact depth
//! depends on the caller's own stack, so it cannot be reproduced; this
//! parser parses such a file normally, on a large worker stack.
//!
//! The grammar is the language pack's, but the runtime is not: the pinned
//! `tree-sitter` 0.27 recovers from some syntax errors differently from the
//! runtime Python uses (the change is in 0.26.13; 0.26.12 gives Python's
//! trees). Where an `#if` splits a statement into an ERROR node, a call
//! whose "callee" is a stretch of recovered text can differ — one file of
//! Newtonsoft.Json's 951.

mod visitor;

#[cfg(test)]
mod tests;

use super::LanguageParser;
use super::java::os_error_text;
use super::java::source::Source;
use super::model::{ParseResult, Relationship, RelationshipType, SymbolType};
use rayon::prelude::*;
use std::collections::{BTreeMap, HashMap};

/// The C# [`LanguageParser`].
#[derive(Debug, Default, Clone, Copy)]
pub struct CSharpParser;

/// Worker stack for the recursive walk: a long string concatenation nests
/// the tree deeply.
const WORKER_STACK: usize = 64 * 1024 * 1024;

impl LanguageParser for CSharpParser {
    fn language(&self) -> &'static str {
        "csharp"
    }

    fn parse_files(&self, files: &[String]) -> BTreeMap<String, ParseResult> {
        let parse_all =
            || -> Vec<ParseResult> { files.par_iter().map(|f| parse_path(f)).collect() };
        let mut results = match rayon::ThreadPoolBuilder::new()
            .stack_size(WORKER_STACK)
            .build()
        {
            Ok(pool) => pool.install(parse_all),
            Err(_) => files.iter().map(|f| parse_path(f)).collect(),
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
        Ok(source) => visitor::parse_source(path, &source),
        Err(error) => visitor::failed(path, error),
    }
}

/// The two cross-file registries. Values are indexes into the file list.
#[derive(Default)]
struct Registries<'r> {
    /// Type name → file; last wins.
    classes: HashMap<&'r str, usize>,
    /// Method name → `(type name, file)`, in registration order, with
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
                SymbolType::Class | SymbolType::Interface | SymbolType::Struct | SymbolType::Enum
            ) {
                continue;
            }
            registries.classes.insert(symbol.name.as_str(), index);
            // Python quirk: `sym.name in s.parent_symbol` is a substring
            // test, so `Todo`'s methods include `TodoItem`'s, and a type
            // whose name occurs in the namespace claims every method.
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
            result
                .relationships
                .iter()
                .map(|r| enhance(r, index, &registries))
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

/// What `_enhance_relationship` changes on one relationship.
struct Update {
    target_file: usize,
    /// A bare call resolved to `Type.method`.
    target_symbol: Option<String>,
    /// Python rebuilds `calls`, `imports` and `references` without their
    /// annotations (a constructor chain, a `global` or alias import, a
    /// parameter context are lost); confidence and weight are 1 for all of
    /// them before and after.
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
/// file. Python looks up the target's FIRST dotted segment for creations,
/// supertypes and references, the LAST for imports, and everything before
/// the last dot for a dotted call (so `a.B.C` looks up `a.B` and finds
/// nothing). A bare call takes the first registered type in another file —
/// not the imported one, unlike Java.
fn enhance(
    relationship: &Relationship,
    index: usize,
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
    let first_segment = target.split('.').next().unwrap_or(target);
    match relationship.relationship_type {
        RelationshipType::Creates
        | RelationshipType::Inheritance
        | RelationshipType::Implementation => other_file(first_segment).map(|f| plain(f, false)),
        RelationshipType::References => other_file(first_segment).map(|f| plain(f, true)),
        RelationshipType::Imports => target
            .rsplit_once('.')
            .and_then(|(_, class)| other_file(class))
            .map(|f| plain(f, true)),
        RelationshipType::Calls => {
            if let Some((class, _)) = target.rsplit_once('.') {
                return other_file(class).map(|f| plain(f, true));
            }
            let &(class, file) = registries
                .methods
                .get(target)?
                .iter()
                .find(|&&(_, file)| file != index)?;
            Some(Update {
                target_file: file,
                target_symbol: Some(format!("{class}.{target}")),
                clear_annotations: true,
            })
        }
        _ => None,
    }
}

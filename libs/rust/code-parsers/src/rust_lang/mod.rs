//! The Rust parser: the Python `RustVisitorParser` (`rust_visitor_parser.py`).
//!
//! The module is `rust_lang`, not `rust`, so that a path such as
//! `parsers::rust` is never confused with the language or the toolchain.
//!
//! Two passes, as Python's `parse_multiple_files`:
//!
//! 1. every file alone (`visitor` and `body`), in parallel;
//! 2. cross-file linking over the results in the caller's sorted order: the
//!    type, function and method registries, then a `defines` edge for each
//!    method whose type another file defines, then `target_file` for calls
//!    into another file.
//!
//! What this port leaves out, and why the output does not change:
//!
//! * Python's pass 0 (Cargo workspace discovery: `rglob('Cargo.toml')`,
//!   member globs, `tomllib`) runs only when `parse_multiple_files` gets a
//!   `repo_path`. The graph builder never passes one, and even with one the
//!   pass fills only `_crate_map` / `_file_to_crate`, which nothing reads.
//! * `_global_trait_methods`, `_file_type_names`, `_file_trait_names` and
//!   `_file_impl_methods` are written and never read.
//!
//! Python's Rust parser does not call `BaseParser.validate_result`, so
//! neither does this one: duplicate relationships (a call inside a struct
//! literal's field is recorded twice) and orphaned ones are kept.
//!
//! Python fills the registries in `as_completed` order. The reference dump
//! (and this port) uses the caller's sorted order, so the last file in
//! sorted order wins a name that several files define.

mod body;
mod visitor;

#[cfg(test)]
mod tests;

use super::LanguageParser;
use super::limits;
use super::model::{ParseResult, Relationship, RelationshipType, SymbolType};
use indexmap::IndexMap;
use rayon::prelude::*;
use serde_json::{Map, Value};
use std::collections::{BTreeMap, HashMap};

/// The Rust [`LanguageParser`].
#[derive(Debug, Default, Clone, Copy)]
pub struct RustParser;

/// Worker stack for the recursive body walk. A deeply nested expression
/// needs more than rayon's default stack.
const WORKER_STACK: usize = 64 * 1024 * 1024;

impl LanguageParser for RustParser {
    fn language(&self) -> &'static str {
        "rust"
    }

    fn parse_files(&self, files: &[String]) -> BTreeMap<String, ParseResult> {
        let parse_all =
            || -> Vec<ParseResult> { files.par_iter().map(|f| parse_path(f)).collect() };
        let results = match limits::on_worker_pool("Rust", WORKER_STACK, parse_all) {
            Ok(results) => results,
            Err(error) => {
                return files
                    .iter()
                    .map(|f| (f.clone(), visitor::failed(f, error.clone())))
                    .collect();
            }
        };
        link(files, results)
    }
}

/// Read one file as Python's `open(..., errors='replace')` does and parse
/// it. The Rust parser decodes exactly as the Go parser does: invalid bytes
/// become U+FFFD and newlines are normalised; node text is then the true
/// UTF-8 slice (`node.text.decode('utf8')`), so non-ASCII text is not shifted.
fn parse_path(path: &str) -> ParseResult {
    match super::go::read_python_text(path) {
        Ok(source) => limits::with_output_budget(|| visitor::parse_source(path, &source))
            .unwrap_or_else(|error| visitor::failed(path, error)),
        Err(error) => visitor::failed(path, error),
    }
}

/// `RUST_BUILTIN_TYPES`: never a relationship target.
pub(crate) fn is_builtin_type(name: &str) -> bool {
    matches!(
        name,
        "bool"
            | "char"
            | "i8"
            | "i16"
            | "i32"
            | "i64"
            | "i128"
            | "isize"
            | "u8"
            | "u16"
            | "u32"
            | "u64"
            | "u128"
            | "usize"
            | "f32"
            | "f64"
            | "str"
            | "String"
            | "()"
            | "!"
            | "Box"
            | "Rc"
            | "Arc"
            | "Cell"
            | "RefCell"
            | "Mutex"
            | "RwLock"
            | "Vec"
            | "HashMap"
            | "HashSet"
            | "BTreeMap"
            | "BTreeSet"
            | "Option"
            | "Result"
            | "Fn"
            | "FnMut"
            | "FnOnce"
            | "Pin"
            | "Future"
            | "Iterator"
            | "Send"
            | "Sync"
            | "Sized"
            | "Copy"
            | "Clone"
            | "Debug"
            | "Display"
            | "Default"
            | "Drop"
            | "Deref"
            | "DerefMut"
            | "From"
            | "Into"
            | "TryFrom"
            | "TryInto"
            | "AsRef"
            | "AsMut"
            | "PartialEq"
            | "Eq"
            | "PartialOrd"
            | "Ord"
            | "Hash"
            | "ToString"
            | "Self"
    )
}

/// `RUST_BUILTIN_MACROS`: never a call target.
pub(crate) fn is_builtin_macro(name: &str) -> bool {
    matches!(
        name,
        "println"
            | "print"
            | "eprintln"
            | "eprint"
            | "format"
            | "write"
            | "writeln"
            | "vec"
            | "todo"
            | "unimplemented"
            | "unreachable"
            | "panic"
            | "assert"
            | "assert_eq"
            | "assert_ne"
            | "debug_assert"
            | "debug_assert_eq"
            | "debug_assert_ne"
            | "cfg"
            | "env"
            | "option_env"
            | "concat"
            | "stringify"
            | "include"
            | "include_str"
            | "include_bytes"
            | "file"
            | "line"
            | "column"
            | "module_path"
            | "compile_error"
            | "matches"
    )
}

/// `RUST_REFERENCE_WRAPPERS`. Python tests these with `startswith`, so a
/// prefix (`Boxed…`, `RefCell<…>`, `Arcade`) counts too.
pub(crate) const REFERENCE_WRAPPERS: &[&str] = &[
    "Box",
    "Rc",
    "Arc",
    "Weak",
    "Ref",
    "RefMut",
    "MutexGuard",
    "RwLockReadGuard",
    "RwLockWriteGuard",
];

/// Pass 2 over per-file results that are in `files`' order.
pub(crate) fn link(
    files: &[String],
    mut results: Vec<ParseResult>,
) -> BTreeMap<String, ParseResult> {
    let registries = Registries::build(&results);
    link_impl_methods_cross_file(files, &mut results, &registries);
    resolve_cross_file_calls(files, &mut results, &registries);
    files.iter().cloned().zip(results).collect()
}

/// The `_global_*` registries. Values are indexes into the file list.
#[derive(Default)]
struct Registries {
    /// Struct, enum, trait and type alias name → file. Associated types
    /// (`type Output = …` in an impl) are type aliases, so they register too.
    types: HashMap<String, usize>,
    functions: HashMap<String, usize>,
    /// Impl target (or trait) → method name → file, in first-seen order as
    /// Python's dict, a later file overwriting the inner value in place.
    methods: IndexMap<String, IndexMap<String, usize>>,
    /// Method name → the file of the FIRST type in `methods` order that has
    /// it: what Python's linear scan for an `obj.method` call finds.
    first_method: HashMap<String, usize>,
}

impl Registries {
    fn build(results: &[ParseResult]) -> Self {
        let mut registries = Self::default();
        for (index, result) in results.iter().enumerate() {
            for symbol in &result.symbols {
                match symbol.symbol_type {
                    SymbolType::Struct
                    | SymbolType::Enum
                    | SymbolType::Trait
                    | SymbolType::TypeAlias => {
                        registries.types.insert(symbol.name.clone(), index);
                    }
                    SymbolType::Function => {
                        registries.functions.insert(symbol.name.clone(), index);
                    }
                    SymbolType::Method => {
                        if let Some(parent) =
                            symbol.parent_symbol.as_ref().filter(|p| !p.is_empty())
                        {
                            registries
                                .methods
                                .entry(parent.clone())
                                .or_default()
                                .insert(symbol.name.clone(), index);
                        }
                    }
                    _ => {}
                }
            }
        }
        for methods in registries.methods.values() {
            for (name, &file) in methods {
                registries.first_method.entry(name.clone()).or_insert(file);
            }
        }
        registries
    }
}

/// `_link_impl_methods_cross_file`: a `defines` edge, stored with the
/// METHOD's file but with `source_file` the TYPE's file, for every method
/// whose parent the type registry places in another file. Trait methods
/// count too (their parent is the trait).
fn link_impl_methods_cross_file(
    files: &[String],
    results: &mut [ParseResult],
    registries: &Registries,
) {
    for (index, result) in results.iter_mut().enumerate() {
        let mut added = Vec::new();
        for symbol in &result.symbols {
            if symbol.symbol_type != SymbolType::Method {
                continue;
            }
            let Some(parent) = symbol.parent_symbol.as_ref().filter(|p| !p.is_empty()) else {
                continue;
            };
            let Some(&type_file) = registries.types.get(parent) else {
                continue;
            };
            if type_file == index {
                continue;
            }
            let mut relationship = Relationship::new(
                parent.clone(),
                format!("{parent}.{}", symbol.name),
                RelationshipType::Defines,
                files[type_file].clone(),
            );
            relationship.target_file = Some(files[index].clone());
            relationship.source_range = Some(symbol.range);
            let mut annotations = Map::new();
            annotations.insert("member_type".to_owned(), Value::String("method".to_owned()));
            annotations.insert("cross_file".to_owned(), Value::Bool(true));
            annotations.insert(
                "impl_kind".to_owned(),
                symbol
                    .metadata
                    .get("impl_kind")
                    .cloned()
                    .unwrap_or_else(|| Value::String("inherent".to_owned())),
            );
            relationship.annotations = annotations;
            added.push(relationship);
        }
        result.relationships.extend(added);
    }
}

/// `_resolve_cross_file_calls`: point a call's `target_file` at the file
/// that defines it.
///
/// * `Left::right` — the method `right` of type `Left` if `Left` has
///   methods (and nothing when it lacks `right`), else type `Left`'s file;
/// * `expr.method` — Python quirk: the method name is everything after the
///   FIRST dot (`self.a.b` looks up `a.b`), and the first registered type
///   with that method wins;
/// * a bare name — the function registry.
fn resolve_cross_file_calls(
    files: &[String],
    results: &mut [ParseResult],
    registries: &Registries,
) {
    for (index, result) in results.iter_mut().enumerate() {
        let path = &files[index];
        for relationship in &mut result.relationships {
            if relationship.relationship_type != RelationshipType::Calls {
                continue;
            }
            if relationship
                .target_file
                .as_ref()
                .is_some_and(|t| !t.is_empty() && t != path)
            {
                continue;
            }
            let target = relationship.target_symbol.as_str();
            let resolved = if let Some((left, right)) = target.rsplit_once("::") {
                match registries.methods.get(left) {
                    Some(methods) => methods.get(right).copied(),
                    None => registries.types.get(left).copied(),
                }
            } else if let Some((_, method)) = target.split_once('.') {
                registries.first_method.get(method).copied()
            } else {
                registries.functions.get(target).copied()
            };
            if let Some(file) = resolved {
                relationship.target_file = Some(files[file].clone());
            }
        }
    }
}

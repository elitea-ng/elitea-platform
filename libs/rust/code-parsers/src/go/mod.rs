//! The Go parser: the Python `GoVisitorParser` (`go_visitor_parser.py`).
//!
//! Two passes, as Python's `parse_multiple_files`:
//!
//! 1. every file alone (`visitor`), in parallel;
//! 2. cross-file linking over the results in the caller's sorted order:
//!    the type, function, method and interface registries, then `defines`
//!    edges for methods declared away from their type, implicit interface
//!    `implementation` edges, and `target_file` for calls into another file.
//!
//! The registries are keyed by the bare name (no package), so two packages
//! that define `Config` share one entry and the LAST file in sorted order
//! wins — the Python behaviour, kept because the graph gate compares with it.
//!
//! Python's Go parser does not call `BaseParser.validate_result`, so neither
//! does this one: duplicate and orphaned relationships are kept.

mod visitor;

#[cfg(test)]
mod tests;

use super::LanguageParser;
use super::input::Sources;
use super::limits;
use super::model::{ParseResult, Range, Relationship, RelationshipType, SymbolType};
use indexmap::IndexMap;
use rayon::prelude::*;
use serde_json::{Map, Value};
use std::collections::{BTreeMap, BTreeSet, HashMap};

/// `GO_BUILTIN_TYPES`: never relationship targets.
pub(crate) const BUILTIN_TYPES: &[&str] = &[
    "int",
    "int8",
    "int16",
    "int32",
    "int64",
    "uint",
    "uint8",
    "uint16",
    "uint32",
    "uint64",
    "uintptr",
    "float32",
    "float64",
    "complex64",
    "complex128",
    "string",
    "byte",
    "rune",
    "bool",
    "error",
    "any",
    "comparable",
];

/// `GO_BUILTIN_FUNCTIONS`: never call targets, also as the last segment of a
/// selector (`x.append(…)` is dropped too).
pub(crate) const BUILTIN_FUNCTIONS: &[&str] = &[
    "make", "new", "len", "cap", "append", "copy", "delete", "close", "panic", "recover", "print",
    "println", "complex", "real", "imag", "clear", "min", "max",
];

/// The Go [`LanguageParser`].
#[derive(Debug, Default, Clone, Copy)]
pub struct GoParser;

impl LanguageParser for GoParser {
    fn language(&self) -> &'static str {
        "go"
    }

    fn parse_sources(
        &self,
        files: &[String],
        sources: Sources<'_>,
    ) -> BTreeMap<String, ParseResult> {
        let results = files
            .par_iter()
            .map(|path| match read_python_text(path, sources) {
                Ok(source) => limits::with_output_budget(|| visitor::parse_source(path, &source))
                    .unwrap_or_else(|error| visitor::failed(path, error)),
                Err(error) => visitor::failed(path, error),
            })
            .collect();
        link(files, results)
    }
}

/// Read a file as Python's `open(path, encoding='utf-8', errors='replace')`
/// does: invalid bytes become U+FFFD and universal newlines turn `\r\n` and
/// a lone `\r` into `\n` — which moves byte columns, so it matters here.
pub(crate) fn read_python_text(path: &str, sources: Sources<'_>) -> Result<String, String> {
    let bytes = sources.read(path).map_err(|error| match error.kind() {
        std::io::ErrorKind::NotFound => format!("File not found: {path}"),
        _ => error.to_string(),
    })?;
    let text = String::from_utf8_lossy(&bytes);
    if text.contains('\r') {
        Ok(text.replace("\r\n", "\n").replace('\r', "\n"))
    } else {
        Ok(text.into_owned())
    }
}

/// Pass 2 over per-file results that are in `files`' order.
///
/// Every registry is filled by walking the results in that order, and every
/// set the Python code iterates in hash order is iterated sorted here, so
/// the output never depends on how pass 1 was scheduled.
pub(crate) fn link(
    files: &[String],
    mut results: Vec<ParseResult>,
) -> BTreeMap<String, ParseResult> {
    let registries = Registries::build(&results);
    link_methods_cross_file(files, &mut results, &registries);
    detect_implicit_interfaces(files, &mut results, &registries);
    resolve_cross_file_calls(files, &mut results, &registries);
    files.iter().cloned().zip(results).collect()
}

/// The `_global_*` registries. Values are indexes into the file list.
#[derive(Default)]
struct Registries {
    /// Struct, interface, enum group and type alias name → file.
    types: HashMap<String, usize>,
    functions: HashMap<String, usize>,
    /// Receiver (or interface) name → method name → file, in first-seen
    /// order, as Python's dict.
    methods: IndexMap<String, IndexMap<String, usize>>,
    /// Interface name → its abstract method names, in first-seen order.
    interface_methods: IndexMap<String, BTreeSet<String>>,
}

impl Registries {
    fn build(results: &[ParseResult]) -> Self {
        let mut registries = Self::default();
        for (index, result) in results.iter().enumerate() {
            // Abstract methods per interface in this file.
            let mut abstract_methods: HashMap<&str, BTreeSet<String>> = HashMap::new();
            for symbol in &result.symbols {
                if symbol.symbol_type == SymbolType::Method
                    && symbol.metadata.get("is_abstract").is_some_and(truthy)
                    && let Some(parent) = symbol.parent_symbol.as_deref()
                {
                    abstract_methods
                        .entry(parent)
                        .or_default()
                        .insert(symbol.name.clone());
                }
            }
            for symbol in &result.symbols {
                match symbol.symbol_type {
                    SymbolType::Struct
                    | SymbolType::Interface
                    | SymbolType::Enum
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
                if symbol.symbol_type == SymbolType::Interface
                    && let Some(methods) = abstract_methods.get(symbol.name.as_str())
                {
                    registries
                        .interface_methods
                        .insert(symbol.name.clone(), methods.clone());
                }
            }
        }
        registries
    }
}

/// Python truthiness for the JSON values metadata holds.
fn truthy(value: &Value) -> bool {
    match value {
        Value::Null => false,
        Value::Bool(b) => *b,
        Value::Number(n) => n.as_f64().is_some_and(|f| f != 0.0),
        Value::String(s) => !s.is_empty(),
        Value::Array(a) => !a.is_empty(),
        Value::Object(o) => !o.is_empty(),
    }
}

/// `_link_methods_to_structs_cross_file`: a `defines` edge, stored with the
/// METHOD's file but with `source_file` the TYPE's file, for every method
/// whose type the registry places in another file. Interface methods count
/// too (their parent is the interface), which only matters when another
/// file defines a type of the same name.
fn link_methods_cross_file(files: &[String], results: &mut [ParseResult], registries: &Registries) {
    for (index, result) in results.iter_mut().enumerate() {
        let mut added = Vec::new();
        for symbol in &result.symbols {
            if symbol.symbol_type != SymbolType::Method {
                continue;
            }
            let Some(receiver) = symbol.parent_symbol.as_ref().filter(|p| !p.is_empty()) else {
                continue;
            };
            let Some(&type_file) = registries.types.get(receiver) else {
                continue;
            };
            if type_file == index {
                continue;
            }
            let mut relationship = Relationship::new(
                receiver.clone(),
                format!("{receiver}.{}", symbol.name),
                RelationshipType::Defines,
                files[type_file].clone(),
            );
            relationship.target_file = Some(files[index].clone());
            relationship.source_range = Some(symbol.range);
            let mut annotations = Map::new();
            annotations.insert("member_type".to_owned(), Value::String("method".to_owned()));
            annotations.insert("cross_file".to_owned(), Value::Bool(true));
            annotations.insert(
                "is_pointer_receiver".to_owned(),
                symbol
                    .metadata
                    .get("is_pointer_receiver")
                    .cloned()
                    .unwrap_or(Value::Bool(false)),
            );
            relationship.annotations = annotations;
            added.push(relationship);
        }
        result.relationships.extend(added);
    }
}

/// `_detect_implicit_interfaces`: a type whose method set contains every
/// EXPORTED method of an interface implements it — one type-level edge and
/// one edge per shared method, all at `1:0` with confidence 0.9, stored in
/// the type's file. The "types" include interfaces (their abstract methods
/// are in the method registry), so an interface whose methods cover another
/// interface's "implements" it as well.
fn detect_implicit_interfaces(
    files: &[String],
    results: &mut [ParseResult],
    registries: &Registries,
) {
    for (iface_name, iface_methods) in &registries.interface_methods {
        let exported: Vec<&String> = iface_methods
            .iter()
            .filter(|m| visitor::visibility(m) == "public")
            .collect();
        if exported.is_empty() {
            continue;
        }
        let iface_file = registries.types.get(iface_name).map(|&i| files[i].clone());
        for (type_name, methods) in &registries.methods {
            if type_name == iface_name || !exported.iter().all(|m| methods.contains_key(*m)) {
                continue;
            }
            let Some(&type_file) = registries.types.get(type_name) else {
                continue;
            };
            let edge = |source: String, target: String, method_level: bool| {
                let mut relationship = Relationship::new(
                    source,
                    target,
                    RelationshipType::Implementation,
                    files[type_file].clone(),
                );
                relationship.target_file.clone_from(&iface_file);
                relationship.source_range = Some(Range::new(1, 0, 1, 0));
                relationship.confidence = 0.9;
                relationship
                    .annotations
                    .insert("implicit".to_owned(), Value::Bool(true));
                if method_level {
                    relationship
                        .annotations
                        .insert("method_level".to_owned(), Value::Bool(true));
                }
                relationship
            };
            let relationships = &mut results[type_file].relationships;
            relationships.push(edge(type_name.clone(), iface_name.clone(), false));
            for method in &exported {
                relationships.push(edge(
                    format!("{type_name}.{method}"),
                    format!("{iface_name}.{method}"),
                    true,
                ));
            }
        }
    }
}

/// `_resolve_cross_file_calls`: point a call's `target_file` at the file
/// that defines a bare function, or the method `Left.right` when `Left` is
/// a registered receiver name. Python quirk: `Left` is the receiver
/// EXPRESSION's text (`s.store.Get` splits as `s` / `store.Get`), so only
/// calls spelled with the type name itself resolve.
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
            let resolved = match relationship.target_symbol.split_once('.') {
                Some((left, right)) => registries
                    .methods
                    .get(left)
                    .and_then(|methods| methods.get(right)),
                None => registries.functions.get(&relationship.target_symbol),
            };
            if let Some(&file) = resolved {
                relationship.target_file = Some(files[file].clone());
            }
        }
    }
}

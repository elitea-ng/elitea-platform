//! The Kotlin parser: the Inventory engine's regex `KotlinParser`
//! (`elitea_inventory/engine/inventory/parsers/kotlin_parser.py`).
//!
//! Not a tree-sitter visitor: the Python parser runs `re` patterns over the
//! file text, and this module runs the same patterns (with the `regex`
//! crate, whose leftmost-first semantics give the same matches and captures
//! for these lookaround-free patterns), in the same order, building the same
//! symbols and relationships. Its quirks are kept on purpose, for parity:
//! every symbol is global (class members are not nested under their class),
//! `import a.b.*` yields both `a.b.` and `a.b` import edges, and a `KDoc`
//! search reaches back to the FIRST `/**` in the file, so a later
//! declaration's docstring spans every earlier `KDoc`. That last one makes the
//! docstrings quadratic in the file, so they count against the crate's
//! output budget (`limits`): a hostile file fails alone instead.
//!
//! Patterns the Python module declares but never uses (`method_call`,
//! `coroutine_launch`, `generic_usage`, `dsl_builder`, `destructuring`,
//! `kdoc`, `kdoc_reference`) are not ported.
//!
//! Each file is parsed alone, as `parse_file`. Python's
//! `parse_multiple_files` calls a `self.parse` that does not exist, so it
//! returns nothing and the Inventory engine falls back to `parse_file` per
//! file (`parsers/__init__.py::parse_files`); its cross-file registry is
//! never read. `validate_result` is not called, so duplicates are kept.
//!
//! Mapping onto the shared model, which is not the Inventory one:
//!
//! * `Symbol.is_exported` (never set, so always `false`) is
//!   `metadata["is_exported"]`;
//! * `Relationship.metadata` is `annotations`; `is_cross_file` is always
//!   `false` and `target_file` always `None`, which is how the model says
//!   "same file";
//! * `RelationshipType.USES` (constructor calls) has no wire value here and
//!   becomes [`RelationshipType::Aggregation`]: the Inventory ingestion maps
//!   both `USES` and `AGGREGATION` to its `uses` relation, so the graph sees
//!   the same edge;
//! * the Python parser emits no `IMPORT` symbols and leaves
//!   `ParseResult.imports` empty; imports are `imports` relationships from
//!   the file stem, as in Python.

#[cfg(test)]
mod tests;

use super::LanguageParser;
use crate::limits;
use crate::model::{ParseResult, Relationship, RelationshipType, Symbol, SymbolType};
use crate::regex_support::{
    Source, compile, file_stem, group, parse_path, pattern_failure, put, span, word_list,
};
use rayon::prelude::*;
use regex::Regex;
use serde_json::{Map, Value};
use std::collections::{BTreeMap, HashSet};
use std::sync::LazyLock;

/// Annotations that never become `decorates` edges.
const BUILTIN_ANNOTATIONS: &[&str] = &[
    "Override",
    "Deprecated",
    "Suppress",
    "JvmStatic",
    "JvmField",
    "JvmOverloads",
    "JvmName",
    "Throws",
    "Nullable",
    "NotNull",
    "Test",
    "Before",
    "After",
];

/// Keywords and stdlib calls that never become `calls` edges.
const SKIPPED_CALLS: &[&str] = &[
    "if",
    "when",
    "for",
    "while",
    "try",
    "catch",
    "finally",
    "return",
    "throw",
    "break",
    "continue",
    "print",
    "println",
    "listOf",
    "mapOf",
    "setOf",
    "arrayOf",
    "mutableListOf",
    "mutableMapOf",
    "mutableSetOf",
    "lazy",
    "require",
    "check",
    "assert",
    "error",
    "TODO",
    "also",
    "apply",
    "let",
    "run",
    "with",
];

/// `PATTERNS`, the ones the parser uses, plus its inline patterns.
pub(crate) struct Patterns {
    package: Regex,
    import: Regex,
    import_wildcard: Regex,
    class: Regex,
    interface: Regex,
    object: Regex,
    function: Regex,
    property: Regex,
    typealias: Regex,
    annotation_use: Regex,
    function_call: Regex,
    constructor_call: Regex,
    delegation: Regex,
    /// `_find_preceding_kdoc`'s search.
    preceding_kdoc: Regex,
    /// `_find_preceding_kdoc`'s per-line cleanup.
    kdoc_line: Regex,
    /// `<[^>]*>`, removed before splitting a type list.
    generics: Regex,
    /// `\([^)]*\)`, removed from a supertype.
    parens: Regex,
}

pub(crate) static PATTERNS: LazyLock<Option<Patterns>> = LazyLock::new(|| {
    Some(Patterns {
        package: compile(r"(?m)^\s*package\s+([\w.]+)")?,
        import: compile(r"(?m)^\s*import\s+([\w.]+)(?:\s+as\s+(\w+))?")?,
        import_wildcard: compile(r"(?m)^\s*import\s+([\w.]+)\.\*")?,
        class: compile(concat!(
            r"(?m)^\s*(?:(public|private|protected|internal)\s+)?",
            r"(?:(abstract|open|final|sealed|data|enum|annotation|inner|value)\s+)*",
            r"class\s+(\w+)",
            r"(?:<[^>]+>)?",
            r"(?:\s*\([^)]*\))?",
            r"(?:\s*:\s*([^{]+))?",
        ))?,
        interface: compile(concat!(
            r"(?m)^\s*(?:(public|private|protected|internal)\s+)?",
            r"(?:(fun)\s+)?",
            r"interface\s+(\w+)",
            r"(?:<[^>]+>)?",
            r"(?:\s*:\s*([^{]+))?",
        ))?,
        object: compile(concat!(
            r"(?m)^\s*(?:(public|private|protected|internal)\s+)?",
            r"(?:(companion)\s+)?",
            r"object\s+(\w+)?",
            r"(?:\s*:\s*([^{]+))?",
        ))?,
        function: compile(concat!(
            r"(?m)^\s*(?:(public|private|protected|internal|override|open|final|abstract|inline|suspend|tailrec|operator|infix|external)\s+)*",
            r"fun\s+",
            r"(?:<[^>]+>\s+)?",
            r"(?:(\w+)\.)?",
            r"(\w+)",
            r"\s*\([^)]*\)",
            r"(?:\s*:\s*([^\n{=]+))?",
        ))?,
        property: compile(concat!(
            r"(?m)^\s*(?:(public|private|protected|internal|override|open|final|abstract|lateinit|const)\s+)*",
            r"(val|var)\s+",
            r"(?:(\w+)\.)?",
            r"(\w+)",
            r"(?:\s*:\s*([^\n=]+))?",
            r"(?:\s*(?:=|by)\s*([^\n]+))?",
        ))?,
        typealias: compile(concat!(
            r"(?m)^\s*(?:(public|private|protected|internal)\s+)?",
            r"typealias\s+(\w+)",
            r"(?:<[^>]+>)?",
            r"\s*=\s*([^\n]+)",
        ))?,
        annotation_use: compile(r"(?m)@(\w+)(?:\([^)]*\))?")?,
        function_call: compile(r"(?m)(?:^|[^\w.])(\w+)\s*\(")?,
        constructor_call: compile(r"(?m)(?:^|[^\w])([A-Z]\w*)\s*\(")?,
        delegation: compile(r"(?m)(?:class|interface)\s+\w+[^{]*:\s*[^{]*\s+by\s+(\w+)")?,
        preceding_kdoc: compile(r"/\*\*\s*([\s\S]*?)\s*\*/\s*$")?,
        kdoc_line: compile(r"^\s*\*\s?")?,
        generics: compile(r"<[^>]*>")?,
        parens: compile(r"\([^)]*\)")?,
    })
});

/// The Kotlin [`LanguageParser`].
#[derive(Debug, Default, Clone, Copy)]
pub struct KotlinParser;

impl LanguageParser for KotlinParser {
    fn language(&self) -> &'static str {
        "kotlin"
    }

    fn parse_files(&self, files: &[String]) -> BTreeMap<String, ParseResult> {
        files
            .par_iter()
            .map(|path| (path.clone(), parse_path(path, "kotlin", parse_source)))
            .collect()
    }
}

/// `parse_file(file_path, content)`.
pub(crate) fn parse_source(file_path: &str, content: &str) -> ParseResult {
    let Some(patterns) = PATTERNS.as_ref() else {
        return pattern_failure(file_path, "kotlin");
    };
    let source = Source::new(content);
    let symbols = extract_symbols(patterns, &source, file_path);
    let relationships = extract_relationships(patterns, &source, file_path, &symbols);
    let mut result = ParseResult::new(file_path, "kotlin");
    result.symbols = symbols;
    result.relationships = relationships;
    result
}

/// `_extract_symbols`.
#[allow(clippy::too_many_lines)] // One block per Python pattern, in order.
fn extract_symbols(p: &Patterns, source: &Source<'_>, file_path: &str) -> Vec<Symbol> {
    let content = source.text;
    let mut symbols = Vec::new();
    let package = p.package.captures(content).and_then(|c| group(&c, 1));
    let qualify = |name: &str| match package {
        Some(package) => format!("{package}.{name}"),
        None => name.to_owned(),
    };

    for caps in p.class.captures_iter(content) {
        let (start, end) = span(&caps);
        let visibility = group(&caps, 1).unwrap_or("public");
        let modifiers = group(&caps, 2);
        let name = group(&caps, 3).unwrap_or_default();
        let end_line = source.block_end(end);
        let mut symbol_type = SymbolType::Class;
        let mut metadata = Map::new();
        put(&mut metadata, "visibility", visibility);
        if let Some(modifiers) = modifiers {
            put(&mut metadata, "modifiers", modifiers.trim());
            if modifiers.contains("enum") {
                symbol_type = SymbolType::Enum;
            } else if modifiers.contains("data") {
                put(&mut metadata, "is_data_class", true);
            } else if modifiers.contains("sealed") {
                put(&mut metadata, "is_sealed", true);
            }
        }
        let mut symbol = source.symbol(file_path, name, symbol_type, start, end_line, metadata);
        symbol.full_name = Some(qualify(name));
        symbol.docstring = preceding_kdoc(p, content, start);
        symbol.visibility = Some(visibility.to_owned());
        symbols.push(symbol);
    }

    for caps in p.interface.captures_iter(content) {
        let (start, end) = span(&caps);
        let visibility = group(&caps, 1).unwrap_or("public");
        let name = group(&caps, 3).unwrap_or_default();
        let end_line = source.block_end(end);
        let mut metadata = Map::new();
        put(&mut metadata, "visibility", visibility);
        if group(&caps, 2).is_some() {
            put(&mut metadata, "is_fun_interface", true);
        }
        let mut symbol = source.symbol(
            file_path,
            name,
            SymbolType::Interface,
            start,
            end_line,
            metadata,
        );
        symbol.full_name = Some(qualify(name));
        symbol.docstring = preceding_kdoc(p, content, start);
        symbol.visibility = Some(visibility.to_owned());
        symbols.push(symbol);
    }

    for caps in p.object.captures_iter(content) {
        let (start, end) = span(&caps);
        let visibility = group(&caps, 1).unwrap_or("public");
        let companion = group(&caps, 2).is_some();
        let name = group(&caps, 3);
        if name.is_none() && !companion {
            continue; // An anonymous object in an expression.
        }
        let name = name.unwrap_or("Companion");
        let end_line = source.block_end(end);
        let mut metadata = Map::new();
        put(&mut metadata, "visibility", visibility);
        if companion {
            put(&mut metadata, "is_companion", true);
        }
        let mut symbol = source.symbol(
            file_path,
            name,
            SymbolType::Class,
            start,
            end_line,
            metadata,
        );
        symbol.full_name = Some(qualify(name));
        symbol.docstring = preceding_kdoc(p, content, start);
        symbol.visibility = Some(visibility.to_owned());
        symbols.push(symbol);
    }

    for caps in p.function.captures_iter(content) {
        let (start, end) = span(&caps);
        let name = group(&caps, 3).unwrap_or_default();
        let end_line = function_end(source, end);
        let mut metadata = Map::new();
        let mut is_async = false;
        if let Some(modifiers) = group(&caps, 1) {
            let list: Vec<&str> = modifiers.split_whitespace().collect();
            put(&mut metadata, "modifiers", word_list(modifiers));
            if list.contains(&"suspend") {
                is_async = true;
                put(&mut metadata, "is_suspend", true);
            }
            if list.contains(&"inline") {
                put(&mut metadata, "is_inline", true);
            }
        }
        if let Some(receiver) = group(&caps, 2) {
            put(&mut metadata, "extension_receiver", receiver);
        }
        let mut symbol = source.symbol(
            file_path,
            name,
            SymbolType::Function,
            start,
            end_line,
            metadata,
        );
        symbol.full_name = Some(qualify(name));
        symbol.docstring = preceding_kdoc(p, content, start);
        symbol.is_async = is_async;
        symbol.return_type = group(&caps, 4).map(|t| t.trim().to_owned());
        symbols.push(symbol);
    }

    for caps in p.property.captures_iter(content) {
        let (start, _) = span(&caps);
        let name = group(&caps, 4).unwrap_or_default();
        let mut metadata = Map::new();
        put(&mut metadata, "mutable", group(&caps, 2) == Some("var"));
        if let Some(modifiers) = group(&caps, 1) {
            put(&mut metadata, "modifiers", word_list(modifiers));
        }
        if let Some(receiver) = group(&caps, 3) {
            put(&mut metadata, "extension_receiver", receiver);
        }
        if let Some(type_decl) = group(&caps, 5) {
            put(&mut metadata, "type", type_decl.trim());
        }
        let line = source.line(start);
        let mut symbol =
            source.symbol(file_path, name, SymbolType::Variable, start, line, metadata);
        symbol.full_name = Some(qualify(name));
        symbols.push(symbol);
    }

    for caps in p.typealias.captures_iter(content) {
        let (start, _) = span(&caps);
        let visibility = group(&caps, 1).unwrap_or("public");
        let name = group(&caps, 2).unwrap_or_default();
        let mut metadata = Map::new();
        put(
            &mut metadata,
            "aliased_type",
            group(&caps, 3).unwrap_or_default().trim(),
        );
        let line = source.line(start);
        let mut symbol = source.symbol(
            file_path,
            name,
            SymbolType::TypeAlias,
            start,
            line,
            metadata,
        );
        symbol.full_name = Some(qualify(name));
        symbol.visibility = Some(visibility.to_owned());
        symbols.push(symbol);
    }

    symbols
}

/// `_extract_relationships`.
#[allow(clippy::too_many_lines)] // One block per Python pattern, in order.
fn extract_relationships(
    p: &Patterns,
    source: &Source<'_>,
    file_path: &str,
    symbols: &[Symbol],
) -> Vec<Relationship> {
    let content = source.text;
    let scope = file_stem(file_path);
    let mut out = Vec::new();
    let edge = |source_symbol: &str,
                target: &str,
                kind: RelationshipType,
                offset: usize,
                annotations: Map<String, Value>| {
        source.relationship(file_path, source_symbol, target, kind, offset, annotations)
    };

    for caps in p.import.captures_iter(content) {
        let (start, _) = span(&caps);
        let mut annotations = Map::new();
        if let Some(alias) = group(&caps, 2) {
            put(&mut annotations, "alias", alias);
        }
        let target = group(&caps, 1).unwrap_or_default();
        out.push(edge(
            &scope,
            target,
            RelationshipType::Imports,
            start,
            annotations,
        ));
    }

    for caps in p.import_wildcard.captures_iter(content) {
        let (start, _) = span(&caps);
        let mut annotations = Map::new();
        put(&mut annotations, "wildcard", true);
        let target = group(&caps, 1).unwrap_or_default();
        out.push(edge(
            &scope,
            target,
            RelationshipType::Imports,
            start,
            annotations,
        ));
    }

    for caps in p.class.captures_iter(content) {
        let (start, _) = span(&caps);
        let class_name = group(&caps, 3).unwrap_or_default();
        if let Some(inheritance) = group(&caps, 4) {
            for (parent, kind) in parse_inheritance(p, inheritance) {
                out.push(edge(class_name, &parent, kind, start, Map::new()));
            }
        }
    }

    for caps in p.interface.captures_iter(content) {
        let (start, _) = span(&caps);
        let name = group(&caps, 3).unwrap_or_default();
        if let Some(extends) = group(&caps, 4) {
            for parent in parse_type_list(p, extends) {
                out.push(edge(
                    name,
                    &parent,
                    RelationshipType::Inheritance,
                    start,
                    Map::new(),
                ));
            }
        }
    }

    for caps in p.object.captures_iter(content) {
        let (start, _) = span(&caps);
        if let (Some(implements), Some(name)) = (group(&caps, 4), group(&caps, 3)) {
            for parent in parse_type_list(p, implements) {
                out.push(edge(
                    name,
                    &parent,
                    RelationshipType::Implementation,
                    start,
                    Map::new(),
                ));
            }
        }
    }

    for caps in p.delegation.captures_iter(content) {
        let (start, _) = span(&caps);
        let mut annotations = Map::new();
        put(&mut annotations, "delegation", true);
        let delegate = group(&caps, 1).unwrap_or_default();
        out.push(edge(
            &scope,
            delegate,
            RelationshipType::Composition,
            start,
            annotations,
        ));
    }

    for caps in p.annotation_use.captures_iter(content) {
        let (start, _) = span(&caps);
        let annotation = group(&caps, 1).unwrap_or_default();
        if !BUILTIN_ANNOTATIONS.contains(&annotation) {
            out.push(edge(
                &scope,
                annotation,
                RelationshipType::Decorates,
                start,
                Map::new(),
            ));
        }
    }

    let names: HashSet<&str> = symbols.iter().map(|s| s.name.as_str()).collect();
    for caps in p.function_call.captures_iter(content) {
        let (start, _) = span(&caps);
        let name = group(&caps, 1).unwrap_or_default();
        if !names.contains(name) && !SKIPPED_CALLS.contains(&name) {
            out.push(edge(
                &scope,
                name,
                RelationshipType::Calls,
                start,
                Map::new(),
            ));
        }
    }

    for caps in p.constructor_call.captures_iter(content) {
        let (start, _) = span(&caps);
        let name = group(&caps, 1).unwrap_or_default();
        if !names.contains(name) {
            // Python `RelationshipType.USES` (see the module docs).
            out.push(edge(
                &scope,
                name,
                RelationshipType::Aggregation,
                start,
                Map::new(),
            ));
        }
    }

    out
}

/// `_parse_inheritance`: a supertype written with a constructor call
/// (`Base()`) before the first `(` of the clause is inherited, every other
/// one implemented.
fn parse_inheritance(p: &Patterns, inheritance: &str) -> Vec<(String, RelationshipType)> {
    let clean = p.generics.replace_all(inheritance, "");
    let before_paren = inheritance.split('(').next().unwrap_or_default();
    let mut out = Vec::new();
    for part in clean.split(',') {
        let part = part.trim();
        if part.is_empty() {
            continue;
        }
        let part = p.parens.replace_all(part, "");
        let Some(type_name) = part.split_whitespace().next() else {
            continue;
        };
        let kind = if inheritance.contains('(') && before_paren.contains(type_name) {
            RelationshipType::Inheritance
        } else {
            RelationshipType::Implementation
        };
        out.push((type_name.to_owned(), kind));
    }
    out
}

/// `_parse_type_list`.
fn parse_type_list(p: &Patterns, types: &str) -> Vec<String> {
    let clean = p.generics.replace_all(types, "");
    let mut out = Vec::new();
    for part in clean.split(',') {
        let Some(first) = part.split_whitespace().next() else {
            continue;
        };
        let name = p.parens.replace_all(first, "");
        let name = name.trim();
        if !name.is_empty() {
            out.push(name.to_owned());
        }
    }
    out
}

/// `_find_preceding_kdoc`. The text before `position` must end with `*/`
/// and whitespace for the Python search to match at all, which is checked
/// first so the (linear) search runs only where it can succeed.
fn preceding_kdoc(p: &Patterns, content: &str, position: usize) -> Option<String> {
    let before = content.get(..position)?;
    if !before.trim_end().ends_with("*/") {
        return None;
    }
    let caps = p.preceding_kdoc.captures(before)?;
    let doc = caps.get(1).map_or("", |m| m.as_str());
    let cleaned: Vec<String> = doc
        .split('\n')
        .map(|line| p.kdoc_line.replace(line, "").trim().to_owned())
        .collect();
    limits::kept_str(cleaned.join("\n").trim())
}

/// `_find_function_end`: an expression body (`=` and no `{` on the rest of
/// the first line, within 100 characters) ends on its own line.
fn function_end(source: &Source<'_>, start: usize) -> u32 {
    let tail = source.text.get(start..).unwrap_or_default();
    let window_end = tail.char_indices().nth(100).map_or(tail.len(), |(i, _)| i);
    let window = tail.get(..window_end).unwrap_or_default();
    let first_line = window.split('\n').next().unwrap_or_default();
    if first_line.contains('=') && !first_line.contains('{') {
        // `content.find('\n', start)`: its line is the start's line.
        source.line(start)
    } else {
        source.block_end(start)
    }
}

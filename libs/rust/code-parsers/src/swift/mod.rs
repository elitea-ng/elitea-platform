//! The Swift parser: the Inventory engine's regex `SwiftParser`
//! (`elitea_inventory/engine/inventory/parsers/swift_parser.py`).
//!
//! Not a tree-sitter visitor: the Python parser runs `re` patterns over the
//! file text, and this module runs the same patterns (with the `regex`
//! crate, whose leftmost-first semantics give the same matches and captures
//! for these lookaround-free patterns), in the same order, building the same
//! symbols and relationships. Its quirks are kept on purpose, for parity:
//! every symbol is global and qualified by the file stem, methods are
//! `function`s, structs and actors are `class`es (flagged in metadata), an
//! extension is a class named `<Type>_extension`, and the import pattern's
//! optional kind group swallows a following line, so `import A\nimport B`
//! reads as one import of `import` with kind `A`.
//!
//! Patterns the Python module declares but never uses (`init`,
//! `computed_property`, `associatedtype`, `function_call`, `type_ref`,
//! `generic_usage`, `closure_type`, `doc_comment`, `doc_comment_block`,
//! `mark`) are not ported.
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
//! * `RelationshipType.USES` (initializer calls) has no wire value here and
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

/// Attributes that never become `decorates` edges.
const BUILTIN_ATTRIBUTES: &[&str] = &[
    "available",
    "objc",
    "objcMembers",
    "nonobjc",
    "escaping",
    "autoclosure",
    "discardableResult",
    "inlinable",
    "usableFromInline",
    "frozen",
    "unknown",
    "IBOutlet",
    "IBAction",
    "IBDesignable",
    "IBInspectable",
    "main",
    "testable",
    "Published",
    "State",
    "Binding",
    "ObservedObject",
    "EnvironmentObject",
    "Environment",
    "AppStorage",
    "SceneStorage",
    "FetchRequest",
    "NSManaged",
];

/// Standard types whose initializer calls are not `uses` edges.
const BUILTIN_TYPES: &[&str] = &[
    "String",
    "Int",
    "Double",
    "Float",
    "Bool",
    "Array",
    "Dictionary",
    "Set",
    "Optional",
    "Result",
    "UUID",
    "URL",
    "Date",
    "Data",
    "Error",
    "NSError",
    "Range",
    "ClosedRange",
    "Substring",
    "Character",
    "CGFloat",
    "CGPoint",
    "CGSize",
    "CGRect",
    "UIColor",
    "NSColor",
    "UIImage",
    "NSImage",
    "UIView",
    "NSView",
    "DispatchQueue",
    "Task",
    "URLSession",
    "JSONDecoder",
    "JSONEncoder",
];

/// Common methods whose calls are not `calls` edges.
const SKIPPED_METHODS: &[&str] = &[
    "map",
    "flatMap",
    "compactMap",
    "filter",
    "reduce",
    "forEach",
    "sorted",
    "first",
    "last",
    "append",
    "insert",
    "remove",
    "contains",
    "count",
    "isEmpty",
    "joined",
    "split",
    "prefix",
    "suffix",
    "dropFirst",
    "dropLast",
    "init",
    "deinit",
    "description",
    "debugDescription",
    "hash",
];

/// `PATTERNS`, the ones the parser uses, plus its inline patterns.
pub(crate) struct Patterns {
    import: Regex,
    class: Regex,
    structure: Regex,
    protocol: Regex,
    enumeration: Regex,
    extension: Regex,
    actor: Regex,
    function: Regex,
    property: Regex,
    typealias: Regex,
    property_wrapper: Regex,
    method_call: Regex,
    init_call: Regex,
    /// `<[^>]*>`, removed before splitting a type list.
    generics: Regex,
    /// `\s+where\s+.*`, removed before splitting a type list.
    where_clause: Regex,
}

pub(crate) static PATTERNS: LazyLock<Option<Patterns>> = LazyLock::new(|| {
    Some(Patterns {
        import: compile(r"(?m)^\s*import\s+(?:(\w+)\s+)?(\w+(?:\.\w+)*)")?,
        class: compile(concat!(
            r"(?m)^\s*(?:@\w+(?:\([^)]*\))?\s*)*",
            r"(?:(open|public|internal|fileprivate|private)\s+)?",
            r"(?:(final)\s+)?",
            r"class\s+(\w+)",
            r"(?:<[^>]+>)?",
            r"(?:\s*:\s*([^{]+))?",
        ))?,
        structure: compile(concat!(
            r"(?m)^\s*(?:@\w+(?:\([^)]*\))?\s*)*",
            r"(?:(public|internal|fileprivate|private)\s+)?",
            r"struct\s+(\w+)",
            r"(?:<[^>]+>)?",
            r"(?:\s*:\s*([^{]+))?",
        ))?,
        protocol: compile(concat!(
            r"(?m)^\s*(?:@\w+(?:\([^)]*\))?\s*)*",
            r"(?:(public|internal|fileprivate|private)\s+)?",
            r"protocol\s+(\w+)",
            r"(?:\s*:\s*([^{]+))?",
        ))?,
        enumeration: compile(concat!(
            r"(?m)^\s*(?:@\w+(?:\([^)]*\))?\s*)*",
            r"(?:(public|internal|fileprivate|private)\s+)?",
            r"(?:(indirect)\s+)?",
            r"enum\s+(\w+)",
            r"(?:<[^>]+>)?",
            r"(?:\s*:\s*([^{]+))?",
        ))?,
        extension: compile(concat!(
            r"(?m)^\s*(?:@\w+(?:\([^)]*\))?\s*)*",
            r"(?:(public|internal|fileprivate|private)\s+)?",
            r"extension\s+(\w+)",
            r"(?:\s*:\s*([^{]+))?",
        ))?,
        actor: compile(concat!(
            r"(?m)^\s*(?:@\w+(?:\([^)]*\))?\s*)*",
            r"(?:(public|internal|fileprivate|private)\s+)?",
            r"actor\s+(\w+)",
            r"(?:<[^>]+>)?",
            r"(?:\s*:\s*([^{]+))?",
        ))?,
        function: compile(concat!(
            r"(?m)^\s*(?:@\w+(?:\([^)]*\))?\s*)*",
            r"(?:(open|public|internal|fileprivate|private)\s+)?",
            r"(?:(override|static|class|final|mutating|nonmutating|async|nonisolated)\s+)*",
            r"func\s+(\w+)",
            r"(?:<[^>]+>)?",
            r"\s*\([^)]*\)",
            r"(?:\s*(?:async\s+)?(?:throws\s+)?->\s*([^\n{]+))?",
        ))?,
        property: compile(concat!(
            r"(?m)^\s*(?:@\w+(?:\([^)]*\))?\s*)*",
            r"(?:(open|public|internal|fileprivate|private)\s+)?",
            r"(?:(static|class|lazy|weak|unowned)\s+)*",
            r"(let|var)\s+(\w+)\s*:\s*([^\n=]+)",
        ))?,
        typealias: compile(concat!(
            r"(?m)^\s*(?:(public|internal|fileprivate|private)\s+)?",
            r"typealias\s+(\w+)",
            r"(?:<[^>]+>)?",
            r"\s*=\s*([^\n]+)",
        ))?,
        property_wrapper: compile(r"(?m)@(\w+)(?:\([^)]*\))?")?,
        method_call: compile(r"(?m)\.(\w+)\s*\(")?,
        init_call: compile(r"(?m)([A-Z]\w*)\s*\(")?,
        generics: compile(r"<[^>]*>")?,
        where_clause: compile(r"\s+where\s+.*")?,
    })
});

/// The Swift [`LanguageParser`].
#[derive(Debug, Default, Clone, Copy)]
pub struct SwiftParser;

impl LanguageParser for SwiftParser {
    fn language(&self) -> &'static str {
        "swift"
    }

    fn parse_files(&self, files: &[String]) -> BTreeMap<String, ParseResult> {
        files
            .par_iter()
            .map(|path| (path.clone(), parse_path(path, "swift", parse_source)))
            .collect()
    }
}

/// `parse_file(file_path, content)`.
pub(crate) fn parse_source(file_path: &str, content: &str) -> ParseResult {
    let Some(patterns) = PATTERNS.as_ref() else {
        return pattern_failure(file_path, "swift");
    };
    let source = Source::new(content);
    let symbols = extract_symbols(patterns, &source, file_path);
    let relationships = extract_relationships(patterns, &source, file_path, &symbols);
    let mut result = ParseResult::new(file_path, "swift");
    result.symbols = symbols;
    result.relationships = relationships;
    result
}

/// The symbol every type-like pattern makes: `symbol_type`, full name
/// `<stem>.<name>`, the preceding doc, the visibility (default
/// `internal`) both as the field and first in `metadata`.
struct TypeSymbol<'a> {
    name: &'a str,
    symbol_type: SymbolType,
    start: usize,
    end: usize,
    visibility: &'a str,
    docstring: Option<String>,
    metadata: Map<String, Value>,
}

/// `_extract_symbols`.
#[allow(clippy::too_many_lines)] // One block per Python pattern, in order.
fn extract_symbols(p: &Patterns, source: &Source<'_>, file_path: &str) -> Vec<Symbol> {
    let content = source.text;
    let module = file_stem(file_path);
    let mut symbols = Vec::new();
    let type_symbol = |t: TypeSymbol<'_>| {
        let end_line = source.block_end(t.end);
        let mut metadata = Map::new();
        put(&mut metadata, "visibility", t.visibility);
        metadata.extend(t.metadata);
        let mut symbol = source.symbol(
            file_path,
            t.name,
            t.symbol_type,
            t.start,
            end_line,
            metadata,
        );
        symbol.full_name = Some(format!("{module}.{}", t.name));
        symbol.docstring = t.docstring;
        symbol.visibility = Some(t.visibility.to_owned());
        symbol
    };
    let flag = |key: &str| {
        let mut map = Map::new();
        put(&mut map, key, true);
        map
    };

    for caps in p.class.captures_iter(content) {
        let (start, end) = span(&caps);
        let metadata = if group(&caps, 2).is_some() {
            flag("final")
        } else {
            Map::new()
        };
        symbols.push(type_symbol(TypeSymbol {
            name: group(&caps, 3).unwrap_or_default(),
            symbol_type: SymbolType::Class,
            start,
            end,
            visibility: group(&caps, 1).unwrap_or("internal"),
            docstring: preceding_doc(content, start),
            metadata,
        }));
    }

    for caps in p.structure.captures_iter(content) {
        let (start, end) = span(&caps);
        symbols.push(type_symbol(TypeSymbol {
            name: group(&caps, 2).unwrap_or_default(),
            symbol_type: SymbolType::Class,
            start,
            end,
            visibility: group(&caps, 1).unwrap_or("internal"),
            docstring: preceding_doc(content, start),
            metadata: flag("is_struct"),
        }));
    }

    for caps in p.protocol.captures_iter(content) {
        let (start, end) = span(&caps);
        symbols.push(type_symbol(TypeSymbol {
            name: group(&caps, 2).unwrap_or_default(),
            symbol_type: SymbolType::Interface,
            start,
            end,
            visibility: group(&caps, 1).unwrap_or("internal"),
            docstring: preceding_doc(content, start),
            metadata: Map::new(),
        }));
    }

    for caps in p.enumeration.captures_iter(content) {
        let (start, end) = span(&caps);
        let mut metadata = Map::new();
        if group(&caps, 2).is_some() {
            put(&mut metadata, "indirect", true);
        }
        if let Some(raw_type) = group(&caps, 4) {
            put(&mut metadata, "raw_type", raw_type.trim());
        }
        symbols.push(type_symbol(TypeSymbol {
            name: group(&caps, 3).unwrap_or_default(),
            symbol_type: SymbolType::Enum,
            start,
            end,
            visibility: group(&caps, 1).unwrap_or("internal"),
            docstring: preceding_doc(content, start),
            metadata,
        }));
    }

    for caps in p.actor.captures_iter(content) {
        let (start, end) = span(&caps);
        symbols.push(type_symbol(TypeSymbol {
            name: group(&caps, 2).unwrap_or_default(),
            symbol_type: SymbolType::Class,
            start,
            end,
            visibility: group(&caps, 1).unwrap_or("internal"),
            docstring: preceding_doc(content, start),
            metadata: flag("is_actor"),
        }));
    }

    for caps in p.extension.captures_iter(content) {
        let (start, end) = span(&caps);
        let extended = group(&caps, 2).unwrap_or_default();
        let name = format!("{extended}_extension");
        let mut metadata = flag("is_extension");
        if let Some(conformance) = group(&caps, 3) {
            let list = conformance
                .split(',')
                .map(|c| Value::String(c.trim().to_owned()))
                .collect();
            put(&mut metadata, "conformance", Value::Array(list));
        }
        symbols.push(type_symbol(TypeSymbol {
            name: &name,
            symbol_type: SymbolType::Class,
            start,
            end,
            visibility: group(&caps, 1).unwrap_or("internal"),
            docstring: None,
            metadata,
        }));
    }

    for caps in p.function.captures_iter(content) {
        let (start, end) = span(&caps);
        let mut metadata = Map::new();
        let mut is_async = false;
        let mut is_static = false;
        if let Some(modifiers) = group(&caps, 2) {
            let list: Vec<&str> = modifiers.split_whitespace().collect();
            put(&mut metadata, "modifiers", word_list(modifiers));
            if list.contains(&"async") {
                is_async = true;
                put(&mut metadata, "is_async", true);
            }
            if list.contains(&"static") || list.contains(&"class") {
                is_static = true;
                put(&mut metadata, "is_static", true);
            }
        }
        let return_type = group(&caps, 4).map(|t| t.trim().to_owned());
        if let Some(return_type) = &return_type {
            put(&mut metadata, "return_type", return_type.as_str());
        }
        let mut symbol = type_symbol(TypeSymbol {
            name: group(&caps, 3).unwrap_or_default(),
            symbol_type: SymbolType::Function,
            start,
            end,
            visibility: group(&caps, 1).unwrap_or("internal"),
            docstring: preceding_doc(content, start),
            metadata,
        });
        symbol.is_async = is_async;
        symbol.is_static = is_static;
        symbol.return_type = return_type;
        symbols.push(symbol);
    }

    for caps in p.property.captures_iter(content) {
        let (start, _) = span(&caps);
        let visibility = group(&caps, 1).unwrap_or("internal");
        let name = group(&caps, 4).unwrap_or_default();
        let mut metadata = Map::new();
        put(&mut metadata, "visibility", visibility);
        put(&mut metadata, "mutable", group(&caps, 3) == Some("var"));
        put(
            &mut metadata,
            "type",
            group(&caps, 5).unwrap_or_default().trim(),
        );
        let mut is_static = false;
        if let Some(modifiers) = group(&caps, 2) {
            let list: Vec<&str> = modifiers.split_whitespace().collect();
            put(&mut metadata, "modifiers", word_list(modifiers));
            is_static = list.contains(&"static") || list.contains(&"class");
        }
        let line = source.line(start);
        let mut symbol =
            source.symbol(file_path, name, SymbolType::Variable, start, line, metadata);
        symbol.full_name = Some(format!("{module}.{name}"));
        symbol.visibility = Some(visibility.to_owned());
        symbol.is_static = is_static;
        symbols.push(symbol);
    }

    for caps in p.typealias.captures_iter(content) {
        let (start, _) = span(&caps);
        let visibility = group(&caps, 1).unwrap_or("internal");
        let name = group(&caps, 2).unwrap_or_default();
        let mut metadata = Map::new();
        put(&mut metadata, "visibility", visibility);
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
        symbol.full_name = Some(format!("{module}.{name}"));
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
        if let Some(kind) = group(&caps, 1) {
            put(&mut annotations, "kind", kind);
        }
        let module = group(&caps, 2).unwrap_or_default();
        out.push(edge(
            &scope,
            module,
            RelationshipType::Imports,
            start,
            annotations,
        ));
    }

    for caps in p.class.captures_iter(content) {
        let (start, _) = span(&caps);
        let class_name = group(&caps, 3).unwrap_or_default();
        if let Some(inheritance) = group(&caps, 4) {
            for (index, parent) in parse_inheritance(p, inheritance).iter().enumerate() {
                // The first parent is the superclass, the rest protocols.
                let kind = if index == 0 && !parent.starts_with("Any") {
                    RelationshipType::Inheritance
                } else {
                    RelationshipType::Implementation
                };
                out.push(edge(class_name, parent, kind, start, Map::new()));
            }
        }
    }

    for caps in p.structure.captures_iter(content) {
        let (start, _) = span(&caps);
        let name = group(&caps, 2).unwrap_or_default();
        if let Some(conformance) = group(&caps, 3) {
            for protocol in parse_inheritance(p, conformance) {
                out.push(edge(
                    name,
                    &protocol,
                    RelationshipType::Implementation,
                    start,
                    Map::new(),
                ));
            }
        }
    }

    for caps in p.protocol.captures_iter(content) {
        let (start, _) = span(&caps);
        let name = group(&caps, 2).unwrap_or_default();
        if let Some(inheritance) = group(&caps, 3) {
            for parent in parse_inheritance(p, inheritance) {
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

    for caps in p.enumeration.captures_iter(content) {
        let (start, _) = span(&caps);
        let name = group(&caps, 3).unwrap_or_default();
        if let Some(conformance) = group(&caps, 4) {
            for item in parse_inheritance(p, conformance) {
                out.push(edge(
                    name,
                    &item,
                    RelationshipType::Implementation,
                    start,
                    Map::new(),
                ));
            }
        }
    }

    for caps in p.extension.captures_iter(content) {
        let (start, _) = span(&caps);
        let extended = group(&caps, 2).unwrap_or_default();
        let mut annotations = Map::new();
        put(&mut annotations, "extension", true);
        out.push(edge(
            &format!("{extended}_extension"),
            extended,
            RelationshipType::Inheritance,
            start,
            annotations,
        ));
        if let Some(conformance) = group(&caps, 3) {
            for protocol in parse_inheritance(p, conformance) {
                let mut annotations = Map::new();
                put(&mut annotations, "via_extension", true);
                out.push(edge(
                    extended,
                    &protocol,
                    RelationshipType::Implementation,
                    start,
                    annotations,
                ));
            }
        }
    }

    let names: HashSet<&str> = symbols.iter().map(|s| s.name.as_str()).collect();
    for caps in p.property_wrapper.captures_iter(content) {
        let (start, _) = span(&caps);
        let wrapper = group(&caps, 1).unwrap_or_default();
        let upper = wrapper.chars().next().is_some_and(char::is_uppercase);
        if !BUILTIN_ATTRIBUTES.contains(&wrapper) && upper {
            out.push(edge(
                &scope,
                wrapper,
                RelationshipType::Decorates,
                start,
                Map::new(),
            ));
        }
    }

    for caps in p.init_call.captures_iter(content) {
        let (start, _) = span(&caps);
        let name = group(&caps, 1).unwrap_or_default();
        if !names.contains(name) && !BUILTIN_TYPES.contains(&name) {
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

    for caps in p.method_call.captures_iter(content) {
        let (start, _) = span(&caps);
        let name = group(&caps, 1).unwrap_or_default();
        if !SKIPPED_METHODS.contains(&name) {
            out.push(edge(
                &scope,
                name,
                RelationshipType::Calls,
                start,
                Map::new(),
            ));
        }
    }

    out
}

/// `_parse_inheritance`: generic arguments and `where` clauses dropped,
/// then the first word of each comma-separated part.
fn parse_inheritance(p: &Patterns, inheritance: &str) -> Vec<String> {
    let clean = p.generics.replace_all(inheritance, "");
    let clean = p.where_clause.replace_all(&clean, "");
    clean
        .split(',')
        .filter_map(|part| part.split_whitespace().next())
        .map(str::to_owned)
        .collect()
}

/// `_find_preceding_doc`: the `///` lines directly above `position`
/// (attribute and blank lines skipped), walked backwards lazily instead of
/// splitting the whole prefix as Python does.
fn preceding_doc(content: &str, position: usize) -> Option<String> {
    let before = content.get(..position)?;
    let mut doc_lines = Vec::new();
    // `lines[:-1]` reversed: skip the (partial) line `position` is on.
    for line in before.rsplit('\n').skip(1) {
        let stripped = line.trim();
        if let Some(doc) = stripped.strip_prefix("///") {
            doc_lines.push(doc.trim());
        } else if !stripped.starts_with('@') && !stripped.is_empty() {
            break;
        }
    }
    if doc_lines.is_empty() {
        return None;
    }
    doc_lines.reverse();
    limits::kept(doc_lines.join("\n"))
}

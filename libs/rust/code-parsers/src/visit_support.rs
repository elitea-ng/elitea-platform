//! Shared plumbing of the Kotlin and Swift tree-sitter visitors
//! ([`crate::kotlin`], [`crate::swift`]).
//!
//! Both started as transcriptions of the Inventory engine's Python regex
//! parsers and keep that output model (every symbol `global`, qualified by
//! the package or file stem, `is_exported` in the metadata and always
//! `false`, file-stem sources for imports, calls and decorators). What they
//! share is kept here:
//!
//! * **Reading.** `open(path, encoding='utf-8', errors='ignore')`, as the
//!   regex ports read: invalid UTF-8 is DROPPED (not replaced), a BOM is
//!   kept, and universal newlines turn `\r\n` and a lone `\r` into `\n`.
//!   A file that cannot be read fails with Python's `str(OSError)`.
//! * **Positions.** Tree-sitter's: 1-based lines, 0-based BYTE columns, as
//!   every other tree-sitter visitor of this crate (the regex ports counted
//!   characters and always ended at column 0).
//! * **Limits.** Every file is parsed on the large-stack worker pool, under
//!   the output budget, and a file deeper than `limits::MAX_TREE_DEPTH`
//!   fails alone ([`crate::limits`]).

use crate::limits;
use crate::model::{ParseResult, Range, Relationship, RelationshipType, Scope, Symbol, SymbolType};
use rayon::prelude::*;
use serde_json::{Map, Value};
use std::collections::BTreeMap;
use tree_sitter::{Language, Node};

/// Worker stack for the recursive walks (as the Java parser's).
const WORKER_STACK: usize = 64 * 1024 * 1024;

/// One file's visitor: the path, its text and the root of its tree.
pub(crate) type Visit = fn(&str, &str, Node<'_>) -> ParseResult;

/// Parse every file of `language` with `grammar`, each alone (neither
/// language has cross-file resolution), in parallel on the worker pool.
pub(crate) fn parse_files(
    files: &[String],
    language: &'static str,
    grammar: &Language,
    visit: Visit,
) -> BTreeMap<String, ParseResult> {
    let parse_all = || -> Vec<ParseResult> {
        files
            .par_iter()
            .map(|path| parse_path(path, language, grammar, visit))
            .collect()
    };
    match limits::on_worker_pool(language, WORKER_STACK, parse_all) {
        Ok(results) => files.iter().cloned().zip(results).collect(),
        Err(error) => files
            .iter()
            .map(|path| (path.clone(), failed(path, language, error.clone())))
            .collect(),
    }
}

/// A result that carries only `error`.
pub(crate) fn failed(path: &str, language: &str, error: impl Into<String>) -> ParseResult {
    let mut result = ParseResult::new(path, language);
    result.errors.push(error.into());
    result
}

/// Read, parse and visit one file.
fn parse_path(path: &str, language: &str, grammar: &Language, visit: Visit) -> ParseResult {
    let text = match read_text(path) {
        Ok(text) => text,
        Err(error) => return failed(path, language, error),
    };
    parse_text(path, language, grammar, &text, visit)
}

/// Parse and visit one already-read source.
pub(crate) fn parse_text(
    path: &str,
    language: &str,
    grammar: &Language,
    text: &str,
    visit: Visit,
) -> ParseResult {
    let mut parser = tree_sitter::Parser::new();
    if parser.set_language(grammar).is_err() {
        return failed(
            path,
            language,
            format!("Tree-sitter {language} parser not available"),
        );
    }
    let Some(tree) = parser.parse(text, None) else {
        return failed(
            path,
            language,
            format!("Tree-sitter {language} parser returned no tree"),
        );
    };
    if limits::too_deep(tree.root_node()) {
        return failed(path, language, limits::RECURSION_ERROR);
    }
    limits::with_output_budget(|| visit(path, text, tree.root_node()))
        .unwrap_or_else(|error| failed(path, language, error))
}

/// Read a file as the Python parsers' `open(..., errors='ignore')` does.
pub(crate) fn read_text(path: &str) -> Result<String, String> {
    let bytes = std::fs::read(path).map_err(|error| python_os_error(&error, path))?;
    let mut text = String::with_capacity(bytes.len());
    for chunk in bytes.utf8_chunks() {
        text.push_str(chunk.valid());
    }
    if text.contains('\r') {
        text = text.replace("\r\n", "\n").replace('\r', "\n");
    }
    Ok(text)
}

/// `str(OSError)`: `[Errno 2] No such file or directory: 'path'`.
fn python_os_error(error: &std::io::Error, path: &str) -> String {
    let Some(code) = error.raw_os_error() else {
        return error.to_string();
    };
    let full = error.to_string();
    let suffix = format!(" (os error {code})");
    let message = full.strip_suffix(&suffix).unwrap_or(&full);
    // `repr(str)`: single quotes unless the text holds one and no double.
    let quoted = if path.contains('\'') && !path.contains('"') {
        format!("\"{path}\"")
    } else {
        format!("'{}'", path.replace('\\', "\\\\").replace('\'', "\\'"))
    };
    format!("[Errno {code}] {message}: {quoted}")
}

/// `Path(file_path).stem`.
pub(crate) fn file_stem(path: &str) -> String {
    std::path::Path::new(path)
        .file_stem()
        .map(|s| s.to_string_lossy().into_owned())
        .unwrap_or_default()
}

/// Insert `key: value` into a metadata map.
pub(crate) fn put(map: &mut Map<String, Value>, key: &str, value: impl Into<Value>) {
    map.insert(key.to_owned(), value.into());
}

/// A list of words as a JSON list.
pub(crate) fn word_list(words: &[String]) -> Value {
    Value::Array(words.iter().map(|w| Value::String(w.clone())).collect())
}

/// A node's range: 1-based lines, 0-based byte columns.
pub(crate) fn range_of(node: Node<'_>) -> Range {
    let (start, end) = (node.start_position(), node.end_position());
    Range::new(
        to_u32(start.row + 1),
        to_u32(start.column),
        to_u32(end.row + 1),
        to_u32(end.column),
    )
}

fn to_u32(value: usize) -> u32 {
    u32::try_from(value).unwrap_or(u32::MAX)
}

/// Every child of `node`, named or not.
pub(crate) fn children(node: Node<'_>) -> Vec<Node<'_>> {
    let mut cursor = node.walk();
    node.children(&mut cursor).collect()
}

/// The first child of `node` of `kind`.
pub(crate) fn child_of_kind<'t>(node: Node<'t>, kind: &str) -> Option<Node<'t>> {
    let mut cursor = node.walk();
    node.children(&mut cursor).find(|c| c.kind() == kind)
}

/// Whether `node` has an unnamed child token `token` (a keyword).
pub(crate) fn has_token(node: Node<'_>, token: &str) -> bool {
    let mut cursor = node.walk();
    node.children(&mut cursor)
        .any(|c| !c.is_named() && c.kind() == token)
}

/// A type's name as the regex ports wrote it: generic arguments dropped
/// (`Comparable<Product>` is `Comparable`), whitespace removed.
pub(crate) fn type_name(text: &str) -> String {
    let base = text.split('<').next().unwrap_or_default();
    base.split_whitespace().collect()
}

/// Whether `name` starts with an upper-case letter (a type or initializer).
pub(crate) fn starts_upper(name: &str) -> bool {
    name.chars().next().is_some_and(char::is_uppercase)
}

/// One file's text and the containers the visitors fill.
pub(crate) struct Output<'s> {
    pub(crate) file: &'s str,
    pub(crate) source: &'s str,
    /// `Path(file).stem`: the source of file-level edges.
    pub(crate) stem: String,
    pub(crate) symbols: Vec<Symbol>,
    pub(crate) relationships: Vec<Relationship>,
}

impl<'s> Output<'s> {
    pub(crate) fn new(file: &'s str, source: &'s str) -> Self {
        Self {
            file,
            source,
            stem: file_stem(file),
            symbols: Vec::new(),
            relationships: Vec::new(),
        }
    }

    pub(crate) fn text(&self, node: Node<'_>) -> &'s str {
        self.source.get(node.byte_range()).unwrap_or("")
    }

    /// A global-scope symbol over `node`, its metadata `metadata` plus
    /// `is_exported` (always `false`, as in the regex ports).
    pub(crate) fn symbol(
        &self,
        name: &str,
        symbol_type: SymbolType,
        node: Node<'_>,
        mut metadata: Map<String, Value>,
    ) -> Symbol {
        let mut symbol = Symbol::new(name, symbol_type, Scope::Global, range_of(node), self.file);
        put(&mut metadata, "is_exported", false);
        symbol.metadata = metadata;
        symbol
    }

    /// A same-file relationship (`target_file` `None`) at `node`.
    pub(crate) fn relate(
        &mut self,
        source: &str,
        target: &str,
        relationship_type: RelationshipType,
        node: Node<'_>,
        annotations: Map<String, Value>,
    ) {
        let mut relationship = Relationship::new(source, target, relationship_type, self.file);
        relationship.source_range = Some(range_of(node));
        relationship.annotations = annotations;
        self.relationships.push(relationship);
    }

    /// The finished result.
    pub(crate) fn finish(self, language: &str) -> ParseResult {
        let mut result = ParseResult::new(self.file, language);
        result.symbols = self.symbols;
        result.relationships = self.relationships;
        result
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn reading_drops_invalid_bytes_and_translates_newlines() {
        let dir = std::env::temp_dir().join(format!("visit-support-{}", std::process::id()));
        let _ = std::fs::create_dir_all(&dir);
        let path = dir.join("a.kt");
        let _ = std::fs::write(&path, b"a\xff\r\nb\rc");
        let path = path.to_string_lossy().into_owned();
        assert_eq!(read_text(&path).ok().as_deref(), Some("a\nb\nc"));
        let _ = std::fs::remove_dir_all(&dir);
        let missing = read_text("/nonexistent/it's.kt");
        assert_eq!(
            missing.err().as_deref(),
            Some("[Errno 2] No such file or directory: \"/nonexistent/it's.kt\"")
        );
    }

    #[test]
    fn type_names_drop_generics_and_spaces() {
        assert_eq!(type_name("Comparable<Product>"), "Comparable");
        assert_eq!(type_name("a. b .C"), "a.b.C");
        assert!(starts_upper("Ünï") && !starts_upper("x") && !starts_upper(""));
    }
}

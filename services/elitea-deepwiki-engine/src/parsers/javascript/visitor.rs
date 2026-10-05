//! One JavaScript file: the Python `JavaScriptVisitorParser.parse_file`.
//!
//! Every rule here is the Python visitor's, quirks included, because the
//! graph gate compares this output with the Python output field by field.
//! Each quirk the port keeps on purpose is marked "Python quirk". Node kinds
//! are the `tree-sitter-javascript` 0.25.0 grammar's, which is the grammar
//! `tree_sitter_language_pack` 1.16.1 ships (1870 parse states, 36 fields,
//! 265 node kinds, identical kind and field names; only the ABI number of
//! the generated table differs, 15 here against 14).
//!
//! The file is processed in four steps, as in Python: the recursive visit
//! (symbols, imports, exports, calls, `new`, JSX tags), the JSX flush, the
//! class → member `defines`, and the body scan for `references`.
//!
//! What the Python parser never fills, this one does not either: no
//! `docstring`, `signature`, `comments`, `return_type`, `parameter_types`,
//! `dependencies` or `module_docstring`.

use crate::parsers::java::source::Source;
use crate::parsers::model::{
    ParseResult, Range, Relationship, RelationshipType, Scope, Symbol, SymbolType,
};
use indexmap::IndexMap;
use regex::Regex;
use serde_json::{Map, Value};
use std::collections::HashSet;
use std::sync::LazyLock;
use tree_sitter::Node;

/// `DEFAULT_EXPORT_SENTINEL`: the export name of `export default …`.
pub(super) const DEFAULT_EXPORT: &str = "<default>";

/// `_JS_KEYWORDS`: names the body scan never turns into a reference.
const JS_KEYWORDS: &[&str] = &[
    "break",
    "case",
    "catch",
    "continue",
    "debugger",
    "default",
    "delete",
    "do",
    "else",
    "export",
    "extends",
    "finally",
    "for",
    "function",
    "if",
    "import",
    "in",
    "instanceof",
    "new",
    "return",
    "switch",
    "this",
    "throw",
    "try",
    "typeof",
    "var",
    "void",
    "while",
    "with",
    "class",
    "const",
    "enum",
    "let",
    "static",
    "yield",
    "await",
    "async",
    "super",
    "implements",
    "interface",
    "package",
    "private",
    "protected",
    "public",
    "true",
    "false",
    "null",
    "undefined",
    "NaN",
    "Infinity",
    "arguments",
    "eval",
    "console",
    "window",
    "document",
    "global",
    "globalThis",
    "process",
    "require",
    "module",
    "exports",
    "setTimeout",
    "setInterval",
    "clearTimeout",
    "clearInterval",
    "fetch",
    "Response",
    "Request",
    "Headers",
    "URL",
    "Array",
    "Object",
    "String",
    "Number",
    "Boolean",
    "Symbol",
    "BigInt",
    "Date",
    "Math",
    "JSON",
    "RegExp",
    "Map",
    "Set",
    "WeakMap",
    "WeakSet",
    "Promise",
    "Proxy",
    "Reflect",
    "Error",
    "TypeError",
    "RangeError",
    "SyntaxError",
    "ReferenceError",
    "parseInt",
    "parseFloat",
    "isNaN",
    "isFinite",
    "encodeURIComponent",
    "decodeURIComponent",
    "encodeURI",
    "decodeURI",
    "Buffer",
    "Uint8Array",
    "Int8Array",
    "ArrayBuffer",
    "DataView",
];

/// `_BUILTIN_TYPES` of `_emit_jsdoc_references`.
const JSDOC_BUILTIN_TYPES: &[&str] = &[
    "string",
    "number",
    "boolean",
    "object",
    "undefined",
    "null",
    "void",
    "any",
    "symbol",
    "bigint",
    "never",
    "unknown",
    "String",
    "Number",
    "Boolean",
    "Object",
    "Function",
    "Array",
    "Promise",
    "Map",
    "Set",
    "Date",
    "RegExp",
    "Error",
    "Symbol",
    "BigInt",
];

/// The `JSDoc` patterns. Python compiles them on `str`, so `\s` and `\w` are
/// Unicode-aware there too; the regex crate's Unicode classes differ from
/// Python's only on exotic characters (combining marks in `\w`, the
/// U+001C..U+001F separators in `\s`).
static JSDOC_PARAM: LazyLock<Option<Regex>> =
    LazyLock::new(|| Regex::new(r"@param\s+\{([^}]+)\}\s+(\w+)").ok());
static JSDOC_RETURNS: LazyLock<Option<Regex>> =
    LazyLock::new(|| Regex::new(r"@returns?\s+\{([^}]+)\}").ok());
static JSDOC_TYPE: LazyLock<Option<Regex>> =
    LazyLock::new(|| Regex::new(r"@type\s+\{([^}]+)\}").ok());
static JSDOC_IDENT: LazyLock<Option<Regex>> = LazyLock::new(|| Regex::new(r"[A-Z]\w*").ok());

/// `ImportBinding`: one local name an `import` (or a destructured
/// `require`) binds.
#[derive(Debug, Clone, PartialEq, Eq)]
pub(super) struct ImportBinding {
    pub(super) local: String,
    /// The raw module specifier (`./utils`, `react`).
    pub(super) source_path: String,
    /// The exported name; `None` for a default import, `*` for a namespace.
    pub(super) imported: Option<String>,
    pub(super) is_namespace: bool,
    pub(super) is_default: bool,
}

/// `ExportEntry` without its file, which is the file it was found in.
#[derive(Debug, Clone, PartialEq, Eq)]
pub(super) struct ExportEntry {
    /// The exported name, or [`DEFAULT_EXPORT`].
    pub(super) name: String,
    /// The declared name behind it.
    pub(super) internal_symbol: String,
    pub(super) is_default: bool,
}

/// What `_parse_single_js_file` returns: the result and the per-file
/// import and export bookkeeping the multi-file pass reads.
#[derive(Debug, Clone, PartialEq)]
pub(super) struct FileOutput {
    pub(super) result: ParseResult,
    /// `_import_bindings`, a dict: a re-bound name keeps its first position
    /// and takes the last binding.
    pub(super) imports: IndexMap<String, ImportBinding>,
    pub(super) exports: Vec<ExportEntry>,
}

/// A result with only an error, as Python's exception handler builds it.
pub(super) fn failed(file_path: &str, error: impl Into<String>) -> FileOutput {
    let mut result = ParseResult::new(file_path, "javascript");
    result.errors.push(error.into());
    FileOutput {
        result,
        imports: IndexMap::new(),
        exports: Vec::new(),
    }
}

/// `parse_file` for decoded text.
pub(super) fn parse_source(file_path: &str, source: &Source) -> FileOutput {
    let mut parser = tree_sitter::Parser::new();
    if parser
        .set_language(&tree_sitter_javascript::LANGUAGE.into())
        .is_err()
    {
        // Python: a parser that failed to initialise gives an empty result.
        return empty(file_path);
    }
    let Some(tree) = parser.parse(source.bytes(), None) else {
        return empty(file_path);
    };
    let mut visitor = Visitor::new(source, file_path);
    visitor.visit(tree.root_node());
    visitor.flush_jsx_usages();
    visitor.extract_defines();
    visitor.scan_body_references();

    let mut result = ParseResult::new(file_path, "javascript");
    result.exports = visitor.exports.iter().map(|e| e.name.clone()).collect();
    // Python: `list({b.source_path for b in …})`, a set whose order follows
    // the string hash seed. Sorted here so the output is deterministic.
    let mut imports: Vec<String> = visitor
        .imports
        .values()
        .map(|b| b.source_path.clone())
        .collect();
    imports.sort();
    imports.dedup();
    result.imports = imports;
    result.symbols = visitor.symbols;
    result.relationships = visitor.relationships;
    FileOutput {
        result,
        imports: visitor.imports,
        exports: visitor.exports,
    }
}

fn empty(file_path: &str) -> FileOutput {
    FileOutput {
        result: ParseResult::new(file_path, "javascript"),
        imports: IndexMap::new(),
        exports: Vec::new(),
    }
}

/// `Path(file).stem`.
pub(super) fn file_stem(file_path: &str) -> String {
    std::path::Path::new(file_path)
        .file_stem()
        .map(|s| s.to_string_lossy().into_owned())
        .unwrap_or_default()
}

/// Every child, named or anonymous, in order (Python's `node.children`).
fn children(node: Node<'_>) -> Vec<Node<'_>> {
    let mut cursor = node.walk();
    node.children(&mut cursor).collect()
}

/// The first child whose kind is one of `kinds`, in CHILD order.
fn find_child<'t>(node: Node<'t>, kinds: &[&str]) -> Option<Node<'t>> {
    let mut cursor = node.walk();
    node.children(&mut cursor)
        .find(|child| kinds.contains(&child.kind()))
}

fn has_child(node: Node<'_>, kind: &str) -> bool {
    find_child(node, &[kind]).is_some()
}

/// Python's `str.strip('"\'')`.
fn strip_quotes(text: &str) -> &str {
    text.trim_matches(|c| c == '"' || c == '\'')
}

/// Python's `str.strip()`: Unicode whitespace plus U+001C..U+001F.
fn py_strip(text: &str) -> &str {
    text.trim_matches(|c: char| c.is_whitespace() || ('\u{1c}'..='\u{1f}').contains(&c))
}

/// `_parse_jsdoc`'s result.
#[derive(Debug, Default)]
struct JsDoc {
    /// `@param {Type} name`, a dict: a repeated name keeps its first
    /// position and takes the last type.
    params: IndexMap<String, String>,
    /// `@returns {Type}` (or `@return`), the first one.
    returns: Option<String>,
    /// `@type {Type}`, the first one.
    type_: Option<String>,
}

impl JsDoc {
    fn parse(text: &str) -> Self {
        let mut doc = Self::default();
        if let Some(re) = JSDOC_PARAM.as_ref() {
            for caps in re.captures_iter(text) {
                if let (Some(ty), Some(name)) = (caps.get(1), caps.get(2)) {
                    doc.params
                        .insert(name.as_str().to_owned(), py_strip(ty.as_str()).to_owned());
                }
            }
        }
        let first = |re: &Option<Regex>| {
            re.as_ref()
                .and_then(|re| re.captures(text))
                .and_then(|c| c.get(1))
                .map(|m| py_strip(m.as_str()).to_owned())
        };
        doc.returns = first(&JSDOC_RETURNS);
        doc.type_ = first(&JSDOC_TYPE);
        doc
    }

    /// The `jsdoc_params` / `jsdoc_returns` metadata of a function or
    /// method: each key only when non-empty (Python's truthiness).
    fn callable_metadata(&self) -> Map<String, Value> {
        let mut meta = Map::new();
        if !self.params.is_empty() {
            let params: Map<String, Value> = self
                .params
                .iter()
                .map(|(k, v)| (k.clone(), Value::String(v.clone())))
                .collect();
            meta.insert("jsdoc_params".to_owned(), Value::Object(params));
        }
        if let Some(returns) = self.returns.as_ref().filter(|r| !r.is_empty()) {
            meta.insert("jsdoc_returns".to_owned(), Value::String(returns.clone()));
        }
        meta
    }

    /// The user-defined type names `_emit_jsdoc_references` emits, in order:
    /// every capitalised identifier of the param types, then the return
    /// type, then `@type` — for a function too, although its metadata
    /// carries no `@type` — skipping the built-ins and repeats.
    fn referenced_types(&self) -> Vec<String> {
        let Some(re) = JSDOC_IDENT.as_ref() else {
            return Vec::new();
        };
        let mut seen = HashSet::new();
        let mut names = Vec::new();
        let types = self
            .params
            .values()
            .chain(self.returns.iter())
            .chain(self.type_.iter());
        for ty in types {
            for m in re.find_iter(ty) {
                let name = m.as_str();
                if !JSDOC_BUILTIN_TYPES.contains(&name) && seen.insert(name.to_owned()) {
                    names.push(name.to_owned());
                }
            }
        }
        names
    }
}

/// The per-file visitor state (`_reset_file_state`).
struct Visitor<'s, 't> {
    source: &'s Source,
    file_path: &'s str,
    /// `_module_symbol_name`: the file stem.
    module: String,
    symbols: Vec<Symbol>,
    relationships: Vec<Relationship>,
    /// Starts with the module name; a class, function or method pushes
    /// its bare name.
    scope: Vec<String>,
    imports: IndexMap<String, ImportBinding>,
    exports: Vec<ExportEntry>,
    /// `(raw tag, range, enclosing scope)` per capitalised JSX tag.
    jsx_usages: Vec<(String, Range, String)>,
    /// `(full name, declaration)` of every function and method, for the
    /// body scan.
    bodies: Vec<(String, Node<'t>)>,
}

impl<'s, 't> Visitor<'s, 't> {
    fn new(source: &'s Source, file_path: &'s str) -> Self {
        let module = file_stem(file_path);
        Self {
            source,
            file_path,
            scope: vec![module.clone()],
            module,
            symbols: Vec::new(),
            relationships: Vec::new(),
            imports: IndexMap::new(),
            exports: Vec::new(),
            jsx_usages: Vec::new(),
            bodies: Vec::new(),
        }
    }

    fn text(&self, node: Node<'_>) -> &'s str {
        self.source.text(node)
    }

    fn range(&self, node: Node<'_>) -> Range {
        self.source.range(node)
    }

    /// `_current_scope_path`.
    fn scope_path(&self) -> String {
        self.scope.join(".")
    }

    /// `_current_scope_path() or self._module_symbol_name`.
    fn scope_or_module(&self) -> String {
        let path = self.scope_path();
        if path.is_empty() {
            self.module.clone()
        } else {
            path
        }
    }

    /// `_qualify`.
    fn qualify(&self, name: &str) -> String {
        let scope = self.scope_path();
        if scope.is_empty() {
            name.to_owned()
        } else {
            format!("{scope}.{name}")
        }
    }

    fn relationship(
        &self,
        source: impl Into<String>,
        target: impl Into<String>,
        kind: RelationshipType,
    ) -> Relationship {
        Relationship::new(source, target, kind, self.file_path)
    }

    /// `_visit_node`: a handled kind goes to its handler (which decides
    /// whether to descend); any other node descends into every child.
    fn visit(&mut self, node: Node<'t>) {
        match node.kind() {
            "import_statement" => self.import_statement(node),
            "export_statement" => self.export_statement(node),
            "function_declaration" => self.function_declaration(node),
            "class_declaration" => self.class_declaration(node),
            "method_definition" => self.method_definition(node),
            "field_definition" => self.field_definition(node),
            "lexical_declaration" | "variable_declaration" => self.lexical_declaration(node),
            "call_expression" => self.call_expression(node),
            "new_expression" => self.new_expression(node),
            "jsx_opening_element" | "jsx_self_closing_element" => self.jsx_element(node),
            _ => self.visit_children(node),
        }
    }

    fn visit_children(&mut self, node: Node<'t>) {
        for child in children(node) {
            self.visit(child);
        }
    }

    /// `_get_preceding_jsdoc`: a `/**` comment that is the previous named
    /// sibling, else the previous named sibling of an enclosing
    /// `export_statement`.
    fn preceding_jsdoc(&self, node: Node<'_>) -> Option<&'s str> {
        let jsdoc = |sibling: Option<Node<'_>>| {
            sibling
                .filter(|p| p.kind() == "comment")
                .map(|p| self.text(p))
                .filter(|t| t.starts_with("/**"))
        };
        if let Some(text) = jsdoc(node.prev_named_sibling()) {
            return Some(text);
        }
        node.parent()
            .filter(|p| p.kind() == "export_statement")
            .and_then(|p| jsdoc(p.prev_named_sibling()))
    }

    /// `_emit_jsdoc_references`.
    fn emit_jsdoc_references(&mut self, doc: &JsDoc, source_symbol: &str, range: Range) {
        for name in doc.referenced_types() {
            let mut rel = self.relationship(source_symbol, name, RelationshipType::References);
            rel.source_range = Some(range);
            rel.annotations
                .insert("reference_type".to_owned(), Value::from("jsdoc"));
            self.relationships.push(rel);
        }
    }

    /// `_handle_import_statement`.
    fn import_statement(&mut self, node: Node<'t>) {
        let mut source_path: Option<String> = None;
        let mut clause = None;
        for child in children(node) {
            match child.kind() {
                "string" => source_path = Some(strip_quotes(self.text(child)).to_owned()),
                "import_clause" => clause = Some(child),
                _ => {}
            }
        }
        let Some(source_path) = source_path.filter(|s| !s.is_empty()) else {
            return;
        };
        let Some(clause) = clause else {
            // A side-effect import: the specifier itself is the target.
            let rel =
                self.relationship(self.module.clone(), source_path, RelationshipType::Imports);
            self.relationships.push(rel);
            return;
        };
        for item in children(clause) {
            match item.kind() {
                "identifier" => {
                    let local = self.text(item).to_owned();
                    self.bind(ImportBinding {
                        local,
                        source_path: source_path.clone(),
                        imported: None,
                        is_namespace: false,
                        is_default: true,
                    });
                }
                "namespace_import" => {
                    if let Some(ident) = find_child(item, &["identifier"]) {
                        let local = self.text(ident).to_owned();
                        self.bind(ImportBinding {
                            local,
                            source_path: source_path.clone(),
                            imported: Some("*".to_owned()),
                            is_namespace: true,
                            is_default: false,
                        });
                    }
                }
                "named_imports" => {
                    for spec in children(item) {
                        if spec.kind() != "import_specifier" {
                            continue;
                        }
                        // Python quirk: the first identifier is the imported
                        // name and the second the alias, so `"a-b" as c`
                        // (a string name) imports `c` as `c`.
                        let mut imported: Option<String> = None;
                        let mut local: Option<String> = None;
                        for part in children(spec) {
                            if part.kind() == "identifier" {
                                let text = self.text(part).to_owned();
                                if imported.is_none() {
                                    imported = Some(text);
                                } else {
                                    local = Some(text);
                                }
                            }
                        }
                        let imported = imported.filter(|i| !i.is_empty());
                        let local = match local.filter(|l| !l.is_empty()) {
                            Some(local) => Some(local),
                            None => imported.clone(),
                        };
                        if let Some(local) = local {
                            self.bind(ImportBinding {
                                local,
                                source_path: source_path.clone(),
                                imported,
                                is_namespace: false,
                                is_default: false,
                            });
                        }
                    }
                }
                _ => {}
            }
        }
    }

    /// Record an ES import binding and its `imports` relationship.
    fn bind(&mut self, binding: ImportBinding) {
        let rel = self.relationship(
            self.module.clone(),
            binding.local.clone(),
            RelationshipType::Imports,
        );
        self.relationships.push(rel);
        self.imports.insert(binding.local.clone(), binding);
    }

    /// `_handle_export_statement`: record what is exported, then visit every
    /// child (the declaration goes to its own handler).
    fn export_statement(&mut self, node: Node<'t>) {
        let kids = children(node);
        let is_default = kids.iter().any(|c| c.kind() == "default");
        let declares =
            |c: &Node<'_>| matches!(c.kind(), "function_declaration" | "class_declaration");
        for child in kids.iter().filter(|c| declares(c)) {
            if let Some(name_node) = find_child(*child, &["identifier"]) {
                let name = self.text(name_node).to_owned();
                self.exports.push(ExportEntry {
                    name: if is_default {
                        DEFAULT_EXPORT.to_owned()
                    } else {
                        name.clone()
                    },
                    internal_symbol: name,
                    is_default,
                });
            }
        }
        for clause in kids.iter().filter(|c| c.kind() == "export_clause") {
            for spec in children(*clause) {
                if spec.kind() != "export_specifier" {
                    continue;
                }
                let names: Vec<Node<'_>> = children(spec)
                    .into_iter()
                    .filter(|c| c.kind() == "identifier")
                    .collect();
                if let (Some(first), Some(last)) = (names.first(), names.last()) {
                    let original = self.text(*first).to_owned();
                    let exported = if names.len() > 1 {
                        self.text(*last).to_owned()
                    } else {
                        original.clone()
                    };
                    self.exports.push(ExportEntry {
                        name: exported,
                        internal_symbol: original,
                        is_default: false,
                    });
                }
            }
        }
        if is_default
            && !kids.iter().any(declares)
            && let Some(ident) = kids.iter().find(|c| c.kind() == "identifier")
        {
            self.exports.push(ExportEntry {
                name: DEFAULT_EXPORT.to_owned(),
                internal_symbol: self.text(*ident).to_owned(),
                is_default: true,
            });
        }
        for child in kids {
            self.visit(child);
        }
    }

    /// `_handle_function_declaration`. Python quirk: only
    /// `function_declaration` — a generator declaration (`function*`), a
    /// function expression or an arrow function is no symbol; its body is
    /// visited in the enclosing scope.
    fn function_declaration(&mut self, node: Node<'t>) {
        let Some(name_node) = find_child(node, &["identifier"]) else {
            return;
        };
        let name = self.text(name_node).to_owned();
        let doc = self.preceding_jsdoc(node).map(JsDoc::parse);
        let mut symbol = Symbol::new(
            name.clone(),
            SymbolType::Function,
            Scope::Function,
            self.range(node),
            self.file_path,
        );
        symbol.parent_symbol = Some(self.scope_or_module());
        let full_name = self.qualify(&name);
        symbol.full_name = Some(full_name.clone());
        symbol.source_text = Some(self.text(node).to_owned());
        if let Some(doc) = &doc {
            symbol.metadata = doc.callable_metadata();
        }
        let range = symbol.range;
        self.symbols.push(symbol);
        if let Some(doc) = &doc {
            self.emit_jsdoc_references(doc, &full_name, range);
        }
        self.bodies.push((full_name, node));
        self.scope.push(name);
        self.visit_children(node);
        self.scope.pop();
    }

    /// `_handle_class_declaration`. Python quirk: only a bare identifier
    /// after `extends` is an `inheritance` (`extends React.Component` and
    /// `extends mixin(A)` are not), and its source is the BARE class name.
    fn class_declaration(&mut self, node: Node<'t>) {
        let Some(name_node) = find_child(node, &["identifier"]) else {
            return;
        };
        let name = self.text(name_node).to_owned();
        let mut symbol = Symbol::new(
            name.clone(),
            SymbolType::Class,
            Scope::Class,
            self.range(node),
            self.file_path,
        );
        symbol.parent_symbol = Some(self.scope_or_module());
        symbol.full_name = Some(self.qualify(&name));
        symbol.source_text = Some(self.text(node).to_owned());
        self.symbols.push(symbol);
        let kids = children(node);
        let base = if let Some(heritage) = find_child(node, &["class_heritage"]) {
            find_child(heritage, &["identifier"])
        } else {
            // Python's textual fallback for a tree without `class_heritage`.
            kids.iter()
                .position(|c| self.text(*c) == "extends")
                .and_then(|i| kids.get(i + 1))
                .filter(|n| n.kind() == "identifier")
                .copied()
        };
        if let Some(base) = base {
            let rel = self.relationship(
                name.clone(),
                self.text(base).to_owned(),
                RelationshipType::Inheritance,
            );
            self.relationships.push(rel);
        }
        self.scope.push(name);
        for child in kids {
            self.visit(child);
        }
        self.scope.pop();
    }

    /// `_handle_method_definition`, for class AND object-literal methods.
    /// Python quirk: a `#private`, computed or string-named method has no
    /// `property_identifier` name, so it is skipped with its whole body.
    fn method_definition(&mut self, node: Node<'t>) {
        let Some(name_node) = find_child(node, &["property_identifier", "identifier"]) else {
            return;
        };
        let name = self.text(name_node).to_owned();
        let full_name = self.qualify(&name);
        let doc = self.preceding_jsdoc(node).map(JsDoc::parse);
        let symbol_type = if name == "constructor" {
            SymbolType::Constructor
        } else {
            SymbolType::Method
        };
        let mut symbol = Symbol::new(
            name.clone(),
            symbol_type,
            Scope::Function,
            self.range(node),
            self.file_path,
        );
        symbol.parent_symbol = Some(self.scope_or_module());
        symbol.full_name = Some(full_name.clone());
        symbol.source_text = Some(self.text(node).to_owned());
        symbol.is_static = has_child(node, "static");
        if let Some(doc) = &doc {
            symbol.metadata = doc.callable_metadata();
        }
        let range = symbol.range;
        self.symbols.push(symbol);
        if let Some(doc) = &doc {
            self.emit_jsdoc_references(doc, &full_name, range);
        }
        self.bodies.push((full_name, node));
        self.scope.push(name);
        self.visit_children(node);
        self.scope.pop();
    }

    /// `_handle_field_definition`: a class field (`x = 1`, `static y`,
    /// `#secret`). A computed field name is skipped with its initializer.
    fn field_definition(&mut self, node: Node<'t>) {
        let Some(name_node) = find_child(
            node,
            &["property_identifier", "private_property_identifier"],
        ) else {
            return;
        };
        let name = self.text(name_node).to_owned();
        let is_private = name_node.kind() == "private_property_identifier";
        let full_name = self.qualify(&name);
        let doc = self.preceding_jsdoc(node).map(JsDoc::parse);
        let mut metadata = Map::new();
        metadata.insert("is_private".to_owned(), Value::Bool(is_private));
        metadata.insert(
            "has_initializer".to_owned(),
            Value::Bool(has_child(node, "=")),
        );
        if let Some(ty) = doc
            .as_ref()
            .and_then(|d| d.type_.as_ref())
            .filter(|t| !t.is_empty())
        {
            metadata.insert("jsdoc_type".to_owned(), Value::String(ty.clone()));
        }
        let mut symbol = Symbol::new(
            name,
            SymbolType::Field,
            Scope::Class,
            self.range(node),
            self.file_path,
        );
        // Python: the scope path itself, no module fallback here.
        symbol.parent_symbol = Some(self.scope_path());
        symbol.full_name = Some(full_name.clone());
        symbol.source_text = Some(self.text(node).to_owned());
        symbol.is_static = has_child(node, "static");
        symbol.visibility = Some(if is_private { "private" } else { "public" }.to_owned());
        symbol.metadata = metadata;
        let range = symbol.range;
        self.symbols.push(symbol);
        if let Some(doc) = &doc {
            self.emit_jsdoc_references(doc, &full_name, range);
        }
        for child in children(node) {
            if !matches!(
                child.kind(),
                "property_identifier" | "private_property_identifier" | "static" | "=" | ";"
            ) {
                self.visit(child);
            }
        }
    }

    /// `_handle_lexical_declaration` (and `var`): one `constant` per
    /// declarator, whatever the keyword and however deep the scope.
    ///
    /// Python quirks: the name is the declarator's FIRST `identifier`
    /// child, so `const {a} = obj` declares a constant `obj` (the value),
    /// and `const {a} = f()` none; the parent is always the module while
    /// the full name follows the scope (`mod.fn.x`).
    fn lexical_declaration(&mut self, node: Node<'t>) {
        let is_export = node
            .parent()
            .is_some_and(|p| p.kind() == "export_statement");
        let kids = children(node);
        for child in kids.iter().filter(|c| c.kind() == "variable_declarator") {
            if let Some(ident) = find_child(*child, &["identifier"]) {
                let name = self.text(ident).to_owned();
                let mut symbol = Symbol::new(
                    name.clone(),
                    SymbolType::Constant,
                    Scope::Global,
                    self.range(*child),
                    self.file_path,
                );
                symbol.parent_symbol = Some(self.module.clone());
                symbol.full_name = Some(self.qualify(&name));
                symbol.source_text = Some(self.text(*child).to_owned());
                self.symbols.push(symbol);
                if is_export {
                    self.exports.push(ExportEntry {
                        name: name.clone(),
                        internal_symbol: name,
                        is_default: false,
                    });
                }
            } else {
                self.register_require_bindings(*child);
            }
        }
        for child in kids {
            self.visit(child);
        }
    }

    /// `_try_register_require_bindings`: `const { X, Y: Z } = require("./m")`
    /// binds `X` and `Z` as named imports — without an `imports`
    /// relationship. A plain `const m = require(…)` binds nothing.
    fn register_require_bindings(&mut self, declarator: Node<'_>) {
        let (Some(pattern), Some(call)) = (
            find_child(declarator, &["object_pattern"]),
            find_child(declarator, &["call_expression"]),
        ) else {
            return;
        };
        let Some(callee) = find_child(call, &["identifier"]) else {
            return;
        };
        if self.text(callee) != "require" {
            return;
        }
        let Some(string) =
            find_child(call, &["arguments"]).and_then(|a| find_child(a, &["string"]))
        else {
            return;
        };
        let source_path = strip_quotes(self.text(string)).to_owned();
        for part in children(pattern) {
            match part.kind() {
                "shorthand_property_identifier" | "shorthand_property_identifier_pattern" => {
                    let local = self.text(part).to_owned();
                    self.imports.insert(
                        local.clone(),
                        ImportBinding {
                            imported: Some(local.clone()),
                            local,
                            source_path: source_path.clone(),
                            is_namespace: false,
                            is_default: false,
                        },
                    );
                }
                "pair_pattern" => {
                    let pair = children(part);
                    let key = pair
                        .iter()
                        .find(|c| matches!(c.kind(), "property_identifier" | "identifier"))
                        .copied();
                    let value = pair
                        .iter()
                        .find(|c| {
                            c.kind() == "shorthand_property_identifier_pattern"
                                || (c.kind() == "identifier" && Some(**c) != key)
                        })
                        .copied();
                    if let (Some(key), Some(value)) = (key, value) {
                        let local = self.text(value).to_owned();
                        self.imports.insert(
                            local.clone(),
                            ImportBinding {
                                local,
                                source_path: source_path.clone(),
                                imported: Some(self.text(key).to_owned()),
                                is_namespace: false,
                                is_default: false,
                            },
                        );
                    }
                }
                _ => {}
            }
        }
    }

    /// `_handle_call_expression`: a `calls` from the current scope to the
    /// callee's dotted name, then every child.
    fn call_expression(&mut self, node: Node<'t>) {
        if let Some(callee) = find_child(node, &["identifier", "member_expression"])
            && let Some(target) = self.callee_name(callee).filter(|t| !t.is_empty())
        {
            let rel = self.relationship(self.scope_or_module(), target, RelationshipType::Calls);
            self.relationships.push(rel);
        }
        self.visit_children(node);
    }

    /// `_extract_callee_name`: `a.b.c`, `this.m`, `super.m`. Python quirk: a
    /// link that is a call or a subscript ends the chain on that side, so
    /// `a().b` reads `b` and `x[0].y` reads `y`; a `#private` property is
    /// dropped (`this.#p` reads `this`).
    fn callee_name(&self, node: Node<'_>) -> Option<String> {
        match node.kind() {
            "identifier" | "this" | "super" | "property_identifier" => {
                Some(self.text(node).to_owned())
            }
            "member_expression" => {
                let kids = children(node);
                let first = kids.first()?;
                let left = self.callee_name(*first).filter(|l| !l.is_empty());
                let prop = kids[1..]
                    .iter()
                    .rev()
                    .find(|c| matches!(c.kind(), "property_identifier" | "identifier"))
                    .map(|c| self.text(*c).to_owned())
                    .filter(|p| !p.is_empty());
                match (left, prop) {
                    (Some(left), Some(prop)) => Some(format!("{left}.{prop}")),
                    (left, prop) => left.or(prop),
                }
            }
            _ => None,
        }
    }

    /// `_handle_new_expression`: `new Foo()` creates `Foo` (confidence
    /// 0.95); inside a `constructor` it is also a `composition`. Python
    /// quirk: `new ns.Foo()` is neither.
    fn new_expression(&mut self, node: Node<'t>) {
        if let Some(ident) = find_child(node, &["identifier"]) {
            let name = self.text(ident).to_owned();
            let source = self.scope_or_module();
            let mut rel =
                self.relationship(source.clone(), name.clone(), RelationshipType::Creates);
            rel.confidence = 0.95;
            rel.annotations
                .insert("creation_type".to_owned(), Value::from("new"));
            self.relationships.push(rel);
            if self
                .scope
                .last()
                .is_some_and(|s| s == "constructor" || s == "<init>")
            {
                let rel = self.relationship(source, name, RelationshipType::Composition);
                self.relationships.push(rel);
            }
        }
        self.visit_children(node);
    }

    /// `_handle_jsx_element`: remember a capitalised tag. Python quirk: the
    /// element's attributes are NOT visited, so calls inside them are lost.
    fn jsx_element(&mut self, node: Node<'t>) {
        let Some(name_node) = find_child(
            node,
            &[
                "identifier",
                "jsx_identifier",
                "jsx_member_expression",
                "member_expression",
            ],
        ) else {
            return;
        };
        let raw = self.text(name_node).to_owned();
        let first = raw.split('.').next().unwrap_or_default();
        if !first.chars().next().is_some_and(char::is_uppercase) || first.contains('-') {
            return;
        }
        let enclosing = self.scope_or_module();
        let range = self.range(name_node);
        self.jsx_usages.push((raw, range, enclosing));
    }

    /// `_flush_jsx_usages`: one `references` per (scope, tag), the first
    /// occurrence's range.
    fn flush_jsx_usages(&mut self) {
        let mut seen = HashSet::new();
        for (raw, range, enclosing) in std::mem::take(&mut self.jsx_usages) {
            if !seen.insert((enclosing.clone(), raw.clone())) {
                continue;
            }
            let mut rel = self.relationship(enclosing, raw, RelationshipType::References);
            rel.source_range = Some(range);
            self.relationships.push(rel);
        }
    }

    /// `_extract_defines_relationships`: class → method, constructor and
    /// field, from the BARE class name to the member's full name.
    ///
    /// Python quirk: members are looked up under the bare class name AND
    /// the full name, so in `User.js` a `class User` also defines every
    /// object-literal method at module level (their parent is `User`, the
    /// module), and two classes of one name each define both's members.
    fn extract_defines(&mut self) {
        let mut by_parent: IndexMap<&str, Vec<usize>> = IndexMap::new();
        for (index, symbol) in self.symbols.iter().enumerate() {
            if let Some(parent) = symbol.parent_symbol.as_deref().filter(|p| !p.is_empty()) {
                by_parent.entry(parent).or_default().push(index);
            }
        }
        let mut defines = Vec::new();
        for class in self
            .symbols
            .iter()
            .filter(|s| s.symbol_type == SymbolType::Class)
        {
            let full = class.full_name.as_deref().unwrap_or_default();
            let members = by_parent
                .get(class.name.as_str())
                .into_iter()
                .chain(by_parent.get(full))
                .flatten();
            for &member in members {
                let member = &self.symbols[member];
                let member_type = match member.symbol_type {
                    SymbolType::Field => "field",
                    SymbolType::Method | SymbolType::Constructor
                        if member.name == "constructor" =>
                    {
                        "constructor"
                    }
                    SymbolType::Method | SymbolType::Constructor => "method",
                    _ => continue,
                };
                let mut rel = self.relationship(
                    class.name.clone(),
                    member.full_name.clone().unwrap_or_default(),
                    RelationshipType::Defines,
                );
                rel.source_range = Some(member.range);
                rel.annotations
                    .insert("member_type".to_owned(), Value::from(member_type));
                rel.annotations
                    .insert("is_static".to_owned(), Value::Bool(member.is_static));
                defines.push(rel);
            }
        }
        self.relationships.extend(defines);
    }

    /// `_scan_all_body_references`: in every function and method body, the
    /// first use of each known name — a top-level constant, function or
    /// class of this file, or an import binding — that is not a parameter,
    /// a top-level local, a keyword or the function's own name.
    ///
    /// Python quirk: the walk is an explicit stack (last child first), so
    /// the order of the edges and which occurrence's range is kept follow
    /// that order, not the source order. Nested functions are scanned with
    /// their parent's body too.
    fn scan_body_references(&mut self) {
        let module_prefix = format!("{}.", self.module);
        let mut known: HashSet<&str> = self
            .symbols
            .iter()
            .filter(|s| {
                matches!(
                    s.symbol_type,
                    SymbolType::Constant | SymbolType::Function | SymbolType::Class
                ) && s
                    .full_name
                    .as_deref()
                    .and_then(|f| f.strip_prefix(&module_prefix))
                    == Some(s.name.as_str())
            })
            .map(|s| s.name.as_str())
            .collect();
        known.extend(self.imports.keys().map(String::as_str));
        if known.is_empty() {
            return;
        }
        let mut found = Vec::new();
        for (full_name, decl) in &self.bodies {
            let mut body = None;
            let mut params = None;
            for child in children(*decl) {
                match child.kind() {
                    "statement_block" => body = Some(child),
                    "formal_parameters" => params = Some(child),
                    _ => {}
                }
            }
            let Some(body) = body else { continue };
            let mut exclude: HashSet<&str> =
                params.map(|p| self.param_names(p)).unwrap_or_default();
            exclude.extend(self.local_var_names(body));
            exclude.insert(full_name.rsplit('.').next().unwrap_or_default());
            let mut seen: HashSet<&str> = HashSet::new();
            let mut stack = children(body);
            while let Some(node) = stack.pop() {
                if node.kind() == "identifier" {
                    let name = self.text(node);
                    if known.contains(name)
                        && !exclude.contains(name)
                        && !JS_KEYWORDS.contains(&name)
                        && !seen.contains(name)
                    {
                        let is_declared_name = node.parent().is_some_and(|p| {
                            p.kind() == "variable_declarator" && p.child(0) == Some(node)
                        });
                        if is_declared_name {
                            continue;
                        }
                        seen.insert(name);
                        let mut rel = self.relationship(
                            full_name.clone(),
                            name.to_owned(),
                            RelationshipType::References,
                        );
                        rel.source_range = Some(self.range(node));
                        found.push(rel);
                    }
                }
                stack.extend(children(node));
            }
        }
        self.relationships.extend(found);
    }

    /// `_extract_param_names`.
    fn param_names(&self, params: Node<'_>) -> HashSet<&'s str> {
        let mut names = HashSet::new();
        for child in children(params) {
            match child.kind() {
                "identifier" => {
                    names.insert(self.text(child));
                }
                "assignment_pattern" | "rest_pattern" => {
                    if let Some(ident) = find_child(child, &["identifier"]) {
                        names.insert(self.text(ident));
                    }
                }
                "object_pattern" | "array_pattern" => names.extend(self.destructured_names(child)),
                _ => {}
            }
        }
        names
    }

    /// `_extract_destructured_names`.
    fn destructured_names(&self, node: Node<'_>) -> HashSet<&'s str> {
        let mut names = HashSet::new();
        let mut stack = children(node);
        while let Some(child) = stack.pop() {
            match child.kind() {
                "identifier" | "shorthand_property_identifier_pattern" => {
                    names.insert(self.text(child));
                }
                "object_pattern" | "array_pattern" | "assignment_pattern" | "rest_pattern"
                | "pair_pattern" => stack.extend(children(child)),
                _ => {}
            }
        }
        names
    }

    /// `_extract_local_var_names`: the body's own top-level declarations.
    fn local_var_names(&self, body: Node<'_>) -> HashSet<&'s str> {
        let mut names = HashSet::new();
        for statement in children(body) {
            if matches!(
                statement.kind(),
                "lexical_declaration" | "variable_declaration"
            ) {
                for child in children(statement) {
                    if child.kind() == "variable_declarator"
                        && let Some(ident) = find_child(child, &["identifier"])
                    {
                        names.insert(self.text(ident));
                    }
                }
            }
        }
        names
    }
}

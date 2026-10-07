//! Pass 1b: the Python `RelationshipExtractor` (calls, references, creates,
//! alias-of) and `_extract_defines_relationships`.
//!
//! Python quirks kept, because the graph is built from them:
//!
//! * the scope stack starts EMPTY (no module stem), so a method's computed
//!   name is `Class.method` while its symbol's full name is
//!   `module.Class.method`; the lookup then falls back to the first symbol
//!   with the bare name — which may be another file-local symbol of that
//!   name;
//! * every handler is followed by the generic walk of the node's children,
//!   so a class, function, method or arrow body is walked twice: once inside
//!   the handler (scope pushed, current symbol set) and once more after it
//!   with the outer state. The second walk attributes the body's calls and
//!   references to the ENCLOSING symbol too. Validation later drops exact
//!   duplicates (first wins), so only the differently-attributed copies
//!   survive.
//!
//! The double walk is exponential in nesting depth. Its output depends only
//! on the node and the walk state (scope stack, current symbol), and a
//! repeated (node, state) visit can only re-emit relationships that are
//! already in the list — which validation drops — so a repeat is skipped.
//! The output is the Python output; only the wasted work is gone.

use super::ast::{children, find_child, range, starts_lower, starts_upper, text};
use super::source::Source;
use crate::model::{Relationship, RelationshipType, Symbol, SymbolType};
use serde_json::{Map, Value, json};
use std::collections::{HashMap, HashSet};
use tree_sitter::Node;

/// `self.ts_keywords`: never the target of a call or reference.
const TS_KEYWORDS: &[&str] = &[
    "abstract",
    "any",
    "as",
    "async",
    "await",
    "boolean",
    "break",
    "case",
    "catch",
    "class",
    "const",
    "constructor",
    "continue",
    "debugger",
    "declare",
    "default",
    "delete",
    "do",
    "else",
    "enum",
    "export",
    "extends",
    "false",
    "finally",
    "for",
    "from",
    "function",
    "get",
    "if",
    "implements",
    "import",
    "in",
    "instanceof",
    "interface",
    "is",
    "keyof",
    "let",
    "module",
    "namespace",
    "never",
    "new",
    "null",
    "number",
    "of",
    "package",
    "private",
    "protected",
    "public",
    "readonly",
    "require",
    "return",
    "set",
    "static",
    "string",
    "super",
    "switch",
    "symbol",
    "this",
    "throw",
    "true",
    "try",
    "type",
    "typeof",
    "undefined",
    "unique",
    "unknown",
    "var",
    "void",
    "while",
    "with",
    "yield",
];

/// Generic types whose base is not a reference (their arguments still are).
const BUILTIN_GENERICS: &[&str] = &[
    "Array", "Promise", "Map", "Set", "Record", "Partial", "Required", "Readonly", "Pick", "Omit",
];

fn is_keyword(name: &str) -> bool {
    TS_KEYWORDS.contains(&name)
}

pub(super) struct RelationshipExtractor<'a> {
    source: &'a Source,
    file_path: &'a str,
    symbols: &'a [Symbol],
    by_name: HashMap<&'a str, usize>,
    by_full_name: HashMap<&'a str, usize>,
    pub(super) relationships: Vec<Relationship>,
    scope_stack: Vec<String>,
    current: Option<usize>,
    /// The interned id of (scope stack, current symbol).
    state: u32,
    states: HashMap<(String, Option<usize>), u32>,
    visited: HashSet<(usize, u32)>,
}

impl<'a> RelationshipExtractor<'a> {
    /// `symbols` is the extractor's raw list (before validation), as Python
    /// passes it; lookups return the FIRST symbol with a name.
    pub(super) fn new(source: &'a Source, file_path: &'a str, symbols: &'a [Symbol]) -> Self {
        let mut by_name = HashMap::new();
        let mut by_full_name = HashMap::new();
        for (index, symbol) in symbols.iter().enumerate() {
            by_name.entry(symbol.name.as_str()).or_insert(index);
            if let Some(full) = symbol.full_name.as_deref().filter(|f| !f.is_empty()) {
                by_full_name.entry(full).or_insert(index);
            }
        }
        let mut extractor = Self {
            source,
            file_path,
            symbols,
            by_name,
            by_full_name,
            relationships: Vec::new(),
            scope_stack: Vec::new(),
            current: None,
            state: 0,
            states: HashMap::new(),
            visited: HashSet::new(),
        };
        extractor.refresh_state();
        extractor
    }

    fn refresh_state(&mut self) {
        let key = (self.scope_stack.join("\u{0}"), self.current);
        let next = u32::try_from(self.states.len()).unwrap_or(u32::MAX);
        self.state = *self.states.entry(key).or_insert(next);
    }

    fn set_current(&mut self, current: Option<usize>) {
        self.current = current;
        self.refresh_state();
    }

    fn text(&self, node: Node<'_>) -> String {
        text(self.source, node).to_owned()
    }

    /// `_find_symbol(full_name, name)`.
    fn find_symbol(&self, full_name: &str, name: &str) -> Option<usize> {
        self.by_full_name
            .get(full_name)
            .or_else(|| self.by_name.get(name))
            .copied()
    }

    /// `symbol.full_name or symbol.name`.
    fn source_name(&self, index: usize) -> String {
        let symbol = &self.symbols[index];
        symbol
            .full_name
            .clone()
            .filter(|f| !f.is_empty())
            .unwrap_or_else(|| symbol.name.clone())
    }

    fn qualified(&self, name: &str) -> String {
        if self.scope_stack.is_empty() {
            name.to_owned()
        } else {
            let parent = self.scope_stack.join(".");
            if parent.is_empty() {
                name.to_owned()
            } else {
                format!("{parent}.{name}")
            }
        }
    }

    fn push(
        &mut self,
        source: usize,
        target: String,
        rel_type: RelationshipType,
        node: Node<'_>,
        annotations: Map<String, Value>,
    ) {
        let mut rel = Relationship::new(self.source_name(source), target, rel_type, self.file_path);
        rel.source_range = Some(range(node));
        rel.annotations = annotations;
        self.relationships.push(rel);
    }

    pub(super) fn visit(&mut self, node: Node<'_>) {
        if !self.visited.insert((node.id(), self.state)) {
            return;
        }
        match node.kind() {
            "class_declaration" | "abstract_class_declaration" | "interface_declaration" => {
                self.visit_scope(node);
            }
            "function_declaration" => self.visit_function(node, &["identifier"]),
            "method_definition" => {
                self.visit_function(node, &["property_identifier", "identifier"]);
            }
            "lexical_declaration" => self.visit_lexical_declaration(node),
            "call_expression" => self.visit_call(node),
            "new_expression" => self.visit_new(node),
            "type_alias_declaration" => self.visit_type_alias(node),
            "type_identifier" => self.visit_type_identifier(node),
            "identifier" => self.visit_identifier(node),
            "jsx_element" => {
                if self.current.is_some() {
                    if let Some(opening) = find_child(node, &["jsx_opening_element"]) {
                        self.jsx_component_reference(opening);
                    }
                    for child in children(node) {
                        self.visit(child);
                    }
                }
            }
            "jsx_self_closing_element" if self.current.is_some() => {
                self.jsx_component_reference(node);
            }
            _ => {}
        }
        for child in children(node) {
            self.visit(child);
        }
    }

    /// A class or interface: walk the children with its name pushed.
    fn visit_scope(&mut self, node: Node<'_>) {
        let Some(name_node) = find_child(node, &["type_identifier"]) else {
            return;
        };
        self.scope_stack.push(self.text(name_node));
        self.refresh_state();
        for child in children(node) {
            self.visit(child);
        }
        self.scope_stack.pop();
        self.refresh_state();
    }

    /// A function declaration or a method: parameter and return-type
    /// references, then the body with the symbol current.
    fn visit_function(&mut self, node: Node<'_>, name_kinds: &[&str]) {
        let Some(name_node) = find_child(node, name_kinds) else {
            return;
        };
        let name = self.text(name_node);
        let symbol = self.find_symbol(&self.qualified(&name), &name);
        if let Some(symbol) = symbol {
            if let Some(params) = find_child(node, &["formal_parameters"]) {
                self.parameter_references(params, symbol);
            }
            self.return_type_references(node, symbol);
        }
        let old = self.current;
        self.set_current(symbol);
        if let Some(body) = find_child(node, &["statement_block"]) {
            for child in children(body) {
                self.visit(child);
            }
        }
        self.set_current(old);
    }

    /// `const f = (...) => ...` anywhere (not only at the top level).
    fn visit_lexical_declaration(&mut self, node: Node<'_>) {
        let Some(declarator) = find_child(node, &["variable_declarator"]) else {
            return;
        };
        let Some(arrow) = find_child(declarator, &["arrow_function"]) else {
            return;
        };
        let Some(identifier) = find_child(declarator, &["identifier"]) else {
            return;
        };
        let name = self.text(identifier);
        let symbol = self.find_symbol(&self.qualified(&name), &name);
        if let Some(symbol) = symbol {
            if let Some(params) = find_child(arrow, &["formal_parameters"]) {
                self.parameter_references(params, symbol);
            }
            self.return_type_references(declarator, symbol);
        }
        let old = self.current;
        self.set_current(symbol);
        if let Some(body) = find_child(arrow, &["statement_block"]) {
            for child in children(body) {
                self.visit(child);
            }
        } else {
            for child in children(arrow) {
                if !matches!(child.kind(), "formal_parameters" | "=>" | "async") {
                    self.visit(child);
                }
            }
        }
        self.set_current(old);
    }

    fn visit_call(&mut self, node: Node<'_>) {
        let Some(current) = self.current else {
            return;
        };
        let Some(function) = children(node).into_iter().next() else {
            return;
        };
        let called = match function.kind() {
            "identifier" => Some(self.text(function)),
            "member_expression" => {
                find_child(function, &["property_identifier"]).map(|p| self.text(p))
            }
            _ => None,
        };
        if let Some(called) = called.filter(|c| !c.is_empty() && !is_keyword(c)) {
            self.push(current, called, RelationshipType::Calls, node, Map::new());
        }
    }

    fn visit_new(&mut self, node: Node<'_>) {
        let Some(current) = self.current else {
            return;
        };
        let mut found: Option<(String, Node<'_>)> = None;
        for child in children(node) {
            match child.kind() {
                "identifier" => {
                    found = Some((self.text(child), child));
                    break;
                }
                "member_expression" => {
                    found = find_child(child, &["property_identifier"]).map(|p| (self.text(p), p));
                    break;
                }
                "generic_type" => {
                    found = find_child(child, &["type_identifier"]).map(|b| (self.text(b), b));
                    if let Some(args) = find_child(child, &["type_arguments"]) {
                        for arg in children(args) {
                            if !matches!(arg.kind(), "<" | ">" | ",") {
                                self.type_references(arg, current, "constructor_generic_arg");
                            }
                        }
                    }
                    break;
                }
                _ => {}
            }
        }
        if let Some((name, at)) = found.filter(|(n, _)| !n.is_empty() && !is_keyword(n)) {
            let mut annotations = Map::new();
            annotations.insert("creation_type".to_owned(), json!("new"));
            self.push(current, name, RelationshipType::Creates, at, annotations);
        }
    }

    /// References from a type alias to its constituent types, and `alias_of`
    /// for `type X = Y` / `type X = G<...>` (G not a builtin generic).
    fn visit_type_alias(&mut self, node: Node<'_>) {
        let Some(name_node) = find_child(node, &["type_identifier"]) else {
            return;
        };
        let name = self.text(name_node);
        let Some(symbol) = self.find_symbol(&self.qualified(&name), &name) else {
            return;
        };
        let mut past_equals = false;
        let mut value = None;
        for child in children(node) {
            if child.kind() == "=" {
                past_equals = true;
                continue;
            }
            if past_equals && !matches!(child.kind(), ";" | "type_parameters") {
                value = Some(child);
                break;
            }
        }
        let Some(value) = value else {
            return;
        };
        self.type_references(value, symbol, "type_alias");
        let target = match value.kind() {
            "type_identifier" => {
                let target = self.text(value);
                (!target.is_empty() && !is_keyword(&target)).then(|| (target.clone(), target))
            }
            "generic_type" => find_child(value, &["type_identifier"])
                .map(|base| self.text(base))
                .filter(|b| {
                    !b.is_empty() && !is_keyword(b) && !BUILTIN_GENERICS.contains(&b.as_str())
                })
                .map(|b| (b, self.text(value))),
            _ => None,
        };
        if let Some((target, aliased)) = target {
            let mut annotations = Map::new();
            annotations.insert("alias_name".to_owned(), json!(name));
            annotations.insert("aliased_type".to_owned(), json!(aliased));
            self.push(symbol, target, RelationshipType::AliasOf, node, annotations);
        }
    }

    fn visit_type_identifier(&mut self, node: Node<'_>) {
        let Some(current) = self.current else {
            return;
        };
        let name = self.text(node);
        if is_keyword(&name) {
            return;
        }
        let mut annotations = Map::new();
        annotations.insert("reference_type".to_owned(), json!("type_usage"));
        self.push(
            current,
            name,
            RelationshipType::References,
            node,
            annotations,
        );
    }

    fn visit_identifier(&mut self, node: Node<'_>) {
        let Some(current) = self.current else {
            return;
        };
        let name = self.text(node);
        if is_keyword(&name) || name == self.symbols[current].name {
            return;
        }
        if self.find_symbol(&name, &name).is_some() || starts_upper(&name) {
            self.push(
                current,
                name,
                RelationshipType::References,
                node,
                Map::new(),
            );
        }
    }

    fn jsx_component_reference(&mut self, jsx_node: Node<'_>) {
        let Some(current) = self.current else {
            return;
        };
        let Some(identifier) = find_child(jsx_node, &["identifier"]) else {
            return;
        };
        let name = self.text(identifier);
        if name.is_empty() || starts_lower(&name) {
            return;
        }
        let mut annotations = Map::new();
        annotations.insert("reference_type".to_owned(), json!("jsx_component"));
        self.push(
            current,
            name,
            RelationshipType::References,
            jsx_node,
            annotations,
        );
    }

    fn parameter_references(&mut self, params: Node<'_>, symbol: usize) {
        for child in children(params) {
            if matches!(child.kind(), "required_parameter" | "optional_parameter")
                && let Some(annotation) = find_child(child, &["type_annotation"])
            {
                self.annotation_references(annotation, symbol, "parameter_type");
            }
        }
    }

    fn return_type_references(&mut self, node: Node<'_>, symbol: usize) {
        if let Some(annotation) = find_child(node, &["type_annotation"]) {
            self.annotation_references(annotation, symbol, "return_type");
        }
    }

    /// `_extract_type_references`: the type after the annotation's `:`.
    fn annotation_references(&mut self, annotation: Node<'_>, symbol: usize, reference_type: &str) {
        if let Some(value) = children(annotation).into_iter().find(|c| c.kind() != ":") {
            self.type_references(value, symbol, reference_type);
        }
    }

    /// `_extract_type_references_recursive`.
    fn type_references(&mut self, node: Node<'_>, symbol: usize, reference_type: &str) {
        match node.kind() {
            "type_identifier" => {
                let name = self.text(node);
                if !name.is_empty() && !is_keyword(&name) {
                    let mut annotations = Map::new();
                    annotations.insert("reference_type".to_owned(), json!(reference_type));
                    self.push(
                        symbol,
                        name,
                        RelationshipType::References,
                        node,
                        annotations,
                    );
                }
            }
            "generic_type" => {
                if let Some(base) = find_child(node, &["type_identifier"]) {
                    let name = self.text(base);
                    if !BUILTIN_GENERICS.contains(&name.as_str()) {
                        let mut annotations = Map::new();
                        annotations.insert(
                            "reference_type".to_owned(),
                            json!(format!("{reference_type}_generic")),
                        );
                        self.push(
                            symbol,
                            name,
                            RelationshipType::References,
                            base,
                            annotations,
                        );
                    }
                }
                if let Some(args) = find_child(node, &["type_arguments"]) {
                    let nested = format!("{reference_type}_generic_arg");
                    for child in children(args) {
                        if !matches!(child.kind(), "<" | ">" | ",") {
                            self.type_references(child, symbol, &nested);
                        }
                    }
                }
            }
            "union_type" | "intersection_type" => {
                for child in children(node) {
                    if !matches!(child.kind(), "|" | "&") {
                        self.type_references(child, symbol, reference_type);
                    }
                }
            }
            "array_type" => {
                if let Some(element) = children(node).into_iter().next() {
                    self.type_references(element, symbol, &format!("{reference_type}_array"));
                }
            }
            "parenthesized_type" => {
                for child in children(node) {
                    if !matches!(child.kind(), "(" | ")") {
                        self.type_references(child, symbol, reference_type);
                    }
                }
            }
            _ => {}
        }
    }
}

/// `_extract_defines_relationships`: class/interface → its methods,
/// constructors, fields and properties (matched by `parent_symbol` equal to
/// the container's full name), over the raw symbol list.
pub(super) fn defines_relationships(symbols: &[Symbol], file_path: &str) -> Vec<Relationship> {
    let mut by_parent: HashMap<&str, Vec<&Symbol>> = HashMap::new();
    for symbol in symbols {
        if let Some(parent) = symbol.parent_symbol.as_deref().filter(|p| !p.is_empty()) {
            by_parent.entry(parent).or_default().push(symbol);
        }
    }
    let mut relationships = Vec::new();
    for container in symbols {
        if !matches!(
            container.symbol_type,
            SymbolType::Class | SymbolType::Interface
        ) {
            continue;
        }
        let container_name = container
            .full_name
            .clone()
            .filter(|f| !f.is_empty())
            .unwrap_or_else(|| container.name.clone());
        let Some(members) = by_parent.get(container_name.as_str()) else {
            continue;
        };
        for member in members {
            let member_type = match member.symbol_type {
                SymbolType::Method => "method",
                SymbolType::Constructor => "constructor",
                SymbolType::Field => "field",
                SymbolType::Property => "property",
                _ => continue,
            };
            let mut annotations = Map::new();
            annotations.insert("member_type".to_owned(), json!(member_type));
            annotations.insert("is_static".to_owned(), Value::Bool(member.is_static));
            if let Some(visibility) = member.visibility.as_ref().filter(|v| !v.is_empty()) {
                annotations.insert("visibility".to_owned(), json!(visibility));
            }
            if member.is_abstract {
                annotations.insert("is_abstract".to_owned(), Value::Bool(true));
            }
            if truthy(member.metadata.get("is_readonly")) {
                annotations.insert("is_readonly".to_owned(), Value::Bool(true));
            }
            if truthy(member.metadata.get("is_optional")) {
                annotations.insert("is_optional".to_owned(), Value::Bool(true));
            }
            let mut rel = Relationship::new(
                container_name.clone(),
                member.full_name.clone().unwrap_or_default(),
                RelationshipType::Defines,
                file_path,
            );
            rel.source_range = Some(member.range);
            rel.annotations = annotations;
            relationships.push(rel);
        }
    }
    relationships
}

/// Python truthiness of a metadata value (only booleans are stored here).
fn truthy(value: Option<&Value>) -> bool {
    value.is_some_and(|v| v.as_bool().unwrap_or(!v.is_null()))
}

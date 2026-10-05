//! Pass one over a file: the Python `extract_symbols` (its nested
//! `SymbolExtractor`).
//!
//! It also emits the relationships the Python extractor keeps in
//! `_field_relationships`: `composition` / `aggregation` for fields,
//! `references` for function signatures, `alias_of` and `overrides`.
//! Full names use `::` here; the caller normalises them to `.` after the
//! relationship pass, as Python does.
//!
//! Python quirks kept on purpose (each marked below): an anonymous
//! namespace, a nested `a::b` namespace, an anonymous class and a class
//! named by a template specialisation are skipped with everything inside
//! them; a function body is never entered (local types are invisible), but
//! every other node is recursed into, so a `struct stat` in a parameter
//! list is a (forward-declared) struct of the file.

use super::names::{TYPE_KINDS, children, find, find_any, user_defined_types};
use super::source::Source;
use crate::parsers::model::{Relationship, RelationshipType, Scope, Symbol, SymbolType};
use serde_json::{Map, Value, json};
use std::collections::HashMap;
use tree_sitter::Node;

/// Kinds whose visitor already handled the children.
const NO_RECURSE: &[&str] = &[
    "class_specifier",
    "struct_specifier",
    "function_definition",
    "namespace_definition",
    "enum_specifier",
    "template_declaration",
];

/// What pass one found.
pub(super) struct Extracted {
    pub(super) symbols: Vec<Symbol>,
    pub(super) relationships: Vec<Relationship>,
}

/// A parameter as `_extract_parameter_info` describes it.
struct Parameter {
    name: String,
    type_name: String,
    range: crate::parsers::model::Range,
}

pub(super) fn extract(root: Node<'_>, source: &Source, file_path: &str, stem: &str) -> Extracted {
    let mut extractor = SymbolExtractor {
        source,
        file_path,
        stem,
        symbols: Vec::new(),
        relationships: Vec::new(),
        scope_stack: Vec::new(),
        class_scope_stack: Vec::new(),
        template_params: Vec::new(),
        class_bases: HashMap::new(),
    };
    extractor.visit(root);
    Extracted {
        symbols: extractor.symbols,
        relationships: extractor.relationships,
    }
}

struct SymbolExtractor<'s> {
    source: &'s Source,
    file_path: &'s str,
    stem: &'s str,
    symbols: Vec<Symbol>,
    relationships: Vec<Relationship>,
    scope_stack: Vec<String>,
    class_scope_stack: Vec<String>,
    /// `current_template_params`; empty reads as Python's falsy `[]`/`None`.
    template_params: Vec<String>,
    /// Class full name → its base names (`class_bases`).
    class_bases: HashMap<String, Vec<String>>,
}

impl SymbolExtractor<'_> {
    fn text(&self, node: Node<'_>) -> String {
        self.source.text(node).to_owned()
    }

    /// `'::'.join(scope_stack) if scope_stack else None` and the full name.
    fn scoped(&self, name: &str) -> (Option<String>, String) {
        if self.scope_stack.is_empty() {
            (None, name.to_owned())
        } else {
            let parent = self.scope_stack.join("::");
            let full = format!("{parent}::{name}");
            (Some(parent), full)
        }
    }

    fn symbol(
        &self,
        name: &str,
        symbol_type: SymbolType,
        scope: Scope,
        node_range: crate::parsers::model::Range,
    ) -> Symbol {
        Symbol::new(name, symbol_type, scope, node_range, self.file_path)
    }

    fn template_metadata(&self, metadata: &mut Map<String, Value>) {
        if !self.template_params.is_empty() {
            metadata.insert("is_template".to_owned(), Value::Bool(true));
            metadata.insert("template_params".to_owned(), json!(self.template_params));
        }
    }

    fn relationship(
        &self,
        source: &str,
        target: &str,
        rel_type: RelationshipType,
        node: Node<'_>,
        annotations: Map<String, Value>,
    ) -> Relationship {
        let mut relationship = Relationship::new(source, target, rel_type, self.file_path);
        relationship.source_range = Some(Source::range(node));
        relationship.annotations = annotations;
        relationship
    }

    fn visit(&mut self, node: Node<'_>) {
        let kind = node.kind();
        let handled = match kind {
            "translation_unit" => {
                self.scope_stack.push(self.stem.to_owned());
                true
            }
            "namespace_definition" => {
                self.namespace(node);
                true
            }
            "class_specifier" => {
                self.class_or_struct(node, SymbolType::Class);
                true
            }
            "struct_specifier" => {
                self.class_or_struct(node, SymbolType::Struct);
                true
            }
            "enum_specifier" => {
                self.enumeration(node);
                true
            }
            "alias_declaration" => {
                self.alias(node);
                true
            }
            "type_definition" => {
                self.typedef(node);
                true
            }
            "template_declaration" => {
                self.template(node);
                true
            }
            "declaration" => {
                self.declaration(node);
                true
            }
            "function_definition" => {
                self.function_definition(node);
                true
            }
            "field_declaration" => {
                self.field_declaration(node);
                true
            }
            "preproc_def" => {
                self.macro_def(node);
                true
            }
            "preproc_function_def" => {
                self.function_macro(node);
                true
            }
            "preproc_include" => {
                self.include(node);
                true
            }
            _ => false,
        };
        if handled && NO_RECURSE.contains(&kind) {
            return;
        }
        for child in children(node) {
            self.visit(child);
        }
    }

    /// Python quirk: only `namespace name { … }` is entered. An anonymous
    /// namespace and `namespace a::b { … }` (a `nested_namespace_specifier`)
    /// are skipped WITH their contents.
    fn namespace(&mut self, node: Node<'_>) {
        let Some(name_node) = find(node, "namespace_identifier") else {
            return;
        };
        self.scope_stack.push(self.text(name_node));
        if let Some(body) = find(node, "declaration_list") {
            for child in children(body) {
                if !matches!(child.kind(), "{" | "}") {
                    self.visit(child);
                }
            }
        }
        self.scope_stack.pop();
    }

    fn class_or_struct(&mut self, node: Node<'_>, symbol_type: SymbolType) {
        // Python quirk: the name is the first `type_identifier` child, so
        // `struct hash<Foo> {…}` (a `template_type`) and an anonymous class
        // are skipped with their members.
        let Some(name) = find(node, "type_identifier")
            .map(|n| self.text(n))
            .filter(|n| !n.is_empty())
        else {
            return;
        };
        let (parent, full) = self.scoped(&name);
        let has_body = find(node, "field_declaration_list").is_some();
        let bases = if has_body {
            self.base_classes(node)
        } else {
            Vec::new()
        };
        let scope = if self.scope_stack.is_empty() {
            Scope::Global
        } else {
            Scope::Class
        };
        let mut symbol = self.symbol(&name, symbol_type, scope, Source::range(node));
        symbol.parent_symbol = parent;
        symbol.full_name = Some(full.clone());
        symbol.source_text = Some(self.text(node));
        if !has_body {
            symbol
                .metadata
                .insert("is_forward_declaration".to_owned(), Value::Bool(true));
            symbol
                .metadata
                .insert("declaration_kind".to_owned(), json!("forward"));
        }
        self.template_metadata(&mut symbol.metadata);
        self.symbols.push(symbol);
        if has_body {
            self.scope_stack.push(name.clone());
            self.class_scope_stack.push(name);
            if !bases.is_empty() {
                self.class_bases.insert(full, bases);
            }
            for child in children(node) {
                self.visit(child);
            }
            self.class_scope_stack.pop();
            self.scope_stack.pop();
        }
    }

    fn base_classes(&self, node: Node<'_>) -> Vec<String> {
        let mut bases = Vec::new();
        if let Some(clause) = find(node, "base_class_clause") {
            for child in children(clause) {
                match child.kind() {
                    "type_identifier" | "qualified_identifier" => bases.push(self.text(child)),
                    "template_type" => {
                        if let Some(name) = find(child, "type_identifier") {
                            bases.push(self.text(name));
                        }
                    }
                    _ => {}
                }
            }
        }
        bases
    }

    fn enumeration(&mut self, node: Node<'_>) {
        let name_node = find(node, "type_identifier");
        let is_scoped = children(node).iter().any(|c| c.kind() == "class");
        let named = name_node.map(|n| self.text(n)).filter(|n| !n.is_empty());
        let is_anonymous = named.is_none();
        let name = if let Some(name) = named {
            name
        } else {
            // `enum { a = 1 }` is named after its first enumerator.
            let first = find(node, "enumerator_list").and_then(|list| {
                children(list)
                    .into_iter()
                    .filter(|c| c.kind() == "enumerator")
                    .find_map(|e| find(e, "identifier"))
                    .map(|id| self.text(id))
            });
            match first {
                Some(first) if !first.is_empty() => format!("<anonymous_enum:{first}>"),
                _ => return,
            }
        };
        let (parent, full) = self.scoped(&name);
        let mut symbol = self.symbol(&name, SymbolType::Enum, Scope::Class, Source::range(node));
        symbol.parent_symbol = parent;
        symbol.full_name = Some(full.clone());
        symbol.source_text = Some(self.text(node));
        symbol
            .metadata
            .insert("is_enum".to_owned(), Value::Bool(true));
        symbol
            .metadata
            .insert("is_scoped_enum".to_owned(), Value::Bool(is_scoped));
        symbol
            .metadata
            .insert("is_anonymous".to_owned(), Value::Bool(is_anonymous));
        self.symbols.push(symbol);
        self.scope_stack.push(name);
        if let Some(list) = find(node, "enumerator_list") {
            for child in children(list) {
                if child.kind() == "enumerator" {
                    self.enumerator(child, &full);
                }
            }
        }
        self.scope_stack.pop();
    }

    fn enumerator(&mut self, node: Node<'_>, parent: &str) {
        let Some(name_node) = find(node, "identifier") else {
            return;
        };
        let name = self.text(name_node);
        let mut symbol = self.symbol(
            &name,
            SymbolType::Constant,
            Scope::Class,
            Source::range(node),
        );
        symbol.full_name = Some(format!("{parent}::{name}"));
        symbol.parent_symbol = Some(parent.to_owned());
        symbol.source_text = Some(self.text(node));
        self.symbols.push(symbol);
    }

    fn alias(&mut self, node: Node<'_>) {
        let Some(name_node) = find(node, "type_identifier") else {
            return;
        };
        let name = self.text(name_node);
        let (parent, full) = self.scoped(&name);
        let aliased = find(node, "type_descriptor")
            .map(|t| crate::graph::pystr::strip(self.source.text(t)).to_owned());
        let mut symbol = self.symbol(
            &name,
            SymbolType::TypeAlias,
            Scope::Global,
            Source::range(node),
        );
        symbol.parent_symbol = parent;
        symbol.full_name = Some(full.clone());
        symbol.return_type.clone_from(&aliased);
        symbol.source_text = Some(self.text(node));
        symbol
            .metadata
            .insert("is_type_alias".to_owned(), Value::Bool(true));
        symbol
            .metadata
            .insert("aliased_type".to_owned(), json!(aliased));
        self.symbols.push(symbol);
        if let Some(aliased) = aliased.filter(|a| !a.is_empty()) {
            for (target, _) in user_defined_types(&aliased, &self.template_params, 0) {
                let mut annotations = Map::new();
                annotations.insert("alias_name".to_owned(), json!(name));
                annotations.insert("aliased_type".to_owned(), json!(aliased));
                let relationship =
                    self.relationship(&full, &target, RelationshipType::AliasOf, node, annotations);
                self.relationships.push(relationship);
            }
        }
    }

    fn typedef(&mut self, node: Node<'_>) {
        // The name: the LAST `type_identifier`, or a function-pointer
        // typedef's `(*name)`; a function declarator without that name lets
        // the search continue to earlier children.
        let mut name_node = None;
        for child in children(node).into_iter().rev() {
            match child.kind() {
                "type_identifier" => {
                    name_node = Some(child);
                    break;
                }
                "function_declarator" => {
                    if let Some(found) = find(child, "parenthesized_declarator")
                        .and_then(|p| find(p, "pointer_declarator"))
                        .and_then(|p| find(p, "type_identifier"))
                    {
                        name_node = Some(found);
                        break;
                    }
                }
                _ => {}
            }
        }
        let Some(name_node) = name_node else {
            return;
        };
        let name = self.text(name_node);
        let (parent, full) = self.scoped(&name);
        let base_type = find_any(
            node,
            &[
                "qualified_identifier",
                "primitive_type",
                "struct_specifier",
                "enum_specifier",
                "type_identifier",
                "template_type",
                "sized_type_specifier",
                "placeholder_type_specifier",
            ],
        )
        .map(|c| crate::graph::pystr::strip(self.source.text(c)).to_owned());
        let mut symbol = self.symbol(
            &name,
            SymbolType::TypeAlias,
            Scope::Global,
            Source::range(node),
        );
        symbol.parent_symbol = parent;
        symbol.full_name = Some(full.clone());
        symbol.return_type.clone_from(&base_type);
        symbol.source_text = Some(self.text(node));
        symbol
            .metadata
            .insert("is_typedef".to_owned(), Value::Bool(true));
        symbol
            .metadata
            .insert("base_type".to_owned(), json!(base_type));
        self.symbols.push(symbol);
        if let Some(base_type) = base_type.filter(|b| !b.is_empty()) {
            for (target, _) in user_defined_types(&base_type, &self.template_params, 0) {
                let mut annotations = Map::new();
                annotations.insert("alias_name".to_owned(), json!(name));
                annotations.insert("base_type".to_owned(), json!(base_type));
                let relationship =
                    self.relationship(&full, &target, RelationshipType::AliasOf, node, annotations);
                self.relationships.push(relationship);
            }
        }
    }

    fn template(&mut self, node: Node<'_>) {
        let mut params = Vec::new();
        if let Some(list) = find(node, "template_parameter_list") {
            for child in children(list) {
                if matches!(
                    child.kind(),
                    "type_parameter_declaration"
                        | "parameter_declaration"
                        | "optional_type_parameter_declaration"
                        | "variadic_type_parameter_declaration"
                ) && let Some(name) = template_param_name(child, self.source)
                {
                    params.push(name);
                }
            }
        }
        let old = std::mem::replace(&mut self.template_params, params);
        for child in children(node) {
            match child.kind() {
                "class_specifier"
                | "struct_specifier"
                | "function_definition"
                | "alias_declaration"
                | "type_definition" => self.visit(child),
                "declaration" => self.template_method_declaration(child),
                _ => {}
            }
        }
        self.template_params = old;
    }

    /// `_handle_template_method_declaration`: no `source_text`, but
    /// parameters and signature relationships.
    fn template_method_declaration(&mut self, node: Node<'_>) {
        let Some(declarator) = find(node, "function_declarator") else {
            return;
        };
        let Some(name) = self.function_name(declarator).filter(|n| !n.is_empty()) else {
            return;
        };
        let (parent, full) = self.scoped(&name);
        let return_type = self.return_type(node);
        let parameters = self.parameters(declarator);
        let symbol_type = if self.class_scope_stack.is_empty() {
            SymbolType::Function
        } else {
            SymbolType::Method
        };
        let parameter_types: Vec<String> = parameters.iter().map(|p| p.type_name.clone()).collect();
        let mut symbol = self.symbol(&name, symbol_type, Scope::Function, Source::range(node));
        symbol.parent_symbol = parent;
        symbol.full_name = Some(full.clone());
        symbol.return_type.clone_from(&return_type);
        symbol.parameter_types.clone_from(&parameter_types);
        self.template_metadata(&mut symbol.metadata);
        self.symbols.push(symbol);
        self.signature_relationships(&full, return_type.as_deref(), &parameter_types, node);
        self.parameter_symbols(&parameters, &full, false);
    }

    /// `_extract_function_name`: the first name-like child.
    fn function_name(&self, declarator: Node<'_>) -> Option<String> {
        for child in children(declarator) {
            match child.kind() {
                "identifier" | "operator_name" | "field_identifier" => {
                    return Some(self.text(child));
                }
                "qualified_identifier" => {
                    let text = self.text(child);
                    return Some(text.rsplit("::").next().unwrap_or("").to_owned());
                }
                _ => {}
            }
        }
        None
    }

    /// `_extract_return_type`: the first type child, unstripped.
    fn return_type(&self, node: Node<'_>) -> Option<String> {
        find_any(node, TYPE_KINDS).map(|c| self.text(c))
    }

    /// `_extract_qualified_name_parts` (the symbol extractor's version: no
    /// `type_identifier`, no `destructor_name`).
    fn qualified_parts(&self, declarator: Node<'_>) -> Option<Vec<String>> {
        let qualified = find(declarator, "qualified_identifier")?;
        let mut parts = Vec::new();
        self.collect_qualified(qualified, &mut parts);
        (!parts.is_empty()).then_some(parts)
    }

    fn collect_qualified(&self, node: Node<'_>, parts: &mut Vec<String>) {
        for child in children(node) {
            match child.kind() {
                "namespace_identifier" | "template_type" | "identifier" | "operator_name" => {
                    parts.push(self.text(child));
                }
                "qualified_identifier" => self.collect_qualified(child, parts),
                _ => {}
            }
        }
    }

    fn is_const_method(&self, declarator: Node<'_>) -> bool {
        declarator.kind() == "function_declarator"
            && children(declarator)
                .iter()
                .any(|c| c.kind() == "type_qualifier" && self.source.text(*c).contains("const"))
    }

    fn is_override(&self, declarator: Node<'_>) -> bool {
        children(declarator)
            .iter()
            .any(|c| c.kind() == "virtual_specifier" && self.source.text(*c) == "override")
    }

    fn parameters(&self, declarator: Node<'_>) -> Vec<Parameter> {
        find(declarator, "parameter_list")
            .map(|list| {
                children(list)
                    .into_iter()
                    .filter(|c| c.kind() == "parameter_declaration")
                    .filter_map(|c| self.parameter(c))
                    .collect()
            })
            .unwrap_or_default()
    }

    /// `_extract_parameter_info`: the name and the type with its `const`,
    /// pointer and reference qualifiers; `None` without both.
    fn parameter(&self, node: Node<'_>) -> Option<Parameter> {
        let mut type_name: Option<String> = None;
        let mut name: Option<String> = None;
        let mut is_const = false;
        let mut pointer = String::new();
        let mut reference = "";
        for child in children(node) {
            match child.kind() {
                "type_qualifier" => {
                    if self.source.text(child).contains("const") {
                        is_const = true;
                    }
                }
                k if TYPE_KINDS.contains(&k) => type_name = Some(self.text(child)),
                "identifier" => name = Some(self.text(child)),
                "pointer_declarator" => {
                    let declarator_text = self.source.text(child);
                    for sub in children(child) {
                        if sub.kind() == "identifier" {
                            name = Some(self.text(sub));
                            break;
                        } else if sub.kind() == "pointer_declarator" {
                            name = self.nested_identifier(sub);
                            if name.as_deref().is_some_and(|n| !n.is_empty()) {
                                break;
                            }
                        }
                    }
                    if let Some(found) = name.as_deref().filter(|n| !n.is_empty()) {
                        let before = declarator_text
                            .find(found)
                            .map_or(declarator_text, |at| &declarator_text[..at]);
                        pointer = pointer_qualifier(before);
                    }
                }
                "reference_declarator" => {
                    let text = self.source.text(child);
                    if text.contains("&&") {
                        reference = "&&";
                    } else if text.contains('&') {
                        reference = "&";
                    }
                    for sub in children(child) {
                        if sub.kind() == "identifier" {
                            name = Some(self.text(sub));
                        }
                    }
                }
                _ => {}
            }
        }
        let name = name.filter(|n| !n.is_empty())?;
        let mut full_type = type_name.filter(|t| !t.is_empty())?;
        if is_const {
            full_type = format!("const {full_type}");
        }
        full_type.push_str(&pointer);
        full_type.push_str(reference);
        Some(Parameter {
            name,
            type_name: full_type,
            range: Source::range(node),
        })
    }

    /// `find_identifier`: the first identifier down nested pointers.
    fn nested_identifier(&self, node: Node<'_>) -> Option<String> {
        for child in children(node) {
            if child.kind() == "identifier" {
                return Some(self.text(child));
            }
            if child.kind() == "pointer_declarator"
                && let Some(found) = self.nested_identifier(child).filter(|n| !n.is_empty())
            {
                return Some(found);
            }
        }
        None
    }

    fn declaration(&mut self, node: Node<'_>) {
        if let Some(declarator) = find(node, "function_declarator") {
            self.function_declaration(node, declarator);
            return;
        }
        let mut has_const = false;
        let mut has_extern = false;
        for child in children(node) {
            match child.kind() {
                "type_qualifier" => {
                    has_const |= self.source.text(child).to_lowercase().contains("const");
                }
                "storage_class_specifier" => {
                    has_extern |= self.source.text(child).to_lowercase().contains("extern");
                }
                _ => {}
            }
        }
        if has_const || has_extern {
            self.const_declaration(node);
        }
    }

    fn function_declaration(&mut self, node: Node<'_>, declarator: Node<'_>) {
        let (name, parent, full, symbol_type) = match self.qualified_parts(declarator) {
            Some(parts) if parts.len() > 1 => {
                let name = parts[parts.len() - 1].clone();
                (
                    name,
                    Some(parts[..parts.len() - 1].join("::")),
                    parts.join("::"),
                    SymbolType::Method,
                )
            }
            _ => {
                let Some(name) = self.function_name(declarator).filter(|n| !n.is_empty()) else {
                    return;
                };
                let (parent, full) = self.scoped(&name);
                let symbol_type = if self.class_scope_stack.is_empty() {
                    SymbolType::Function
                } else {
                    SymbolType::Method
                };
                (name, parent, full, symbol_type)
            }
        };
        let return_type = self.return_type(node);
        let parameters = self.parameters(declarator);
        let mut symbol = self.symbol(&name, symbol_type, Scope::Function, Source::range(node));
        symbol.parent_symbol = parent;
        symbol.full_name = Some(full);
        symbol.return_type = return_type;
        symbol.parameter_types = parameters.into_iter().map(|p| p.type_name).collect();
        symbol.source_text = Some(self.text(node));
        self.template_metadata(&mut symbol.metadata);
        symbol
            .metadata
            .insert("has_body".to_owned(), Value::Bool(false));
        self.symbols.push(symbol);
    }

    fn const_declaration(&mut self, node: Node<'_>) {
        let const_type = self.return_type(node);
        let mut found_declarators = false;
        let node_children = children(node);
        for child in &node_children {
            if child.kind() != "init_declarator" {
                continue;
            }
            found_declarators = true;
            if let Some(identifier) = find(*child, "identifier") {
                self.constant(&self.text(identifier), *child, node, const_type.as_ref());
            }
        }
        if !found_declarators {
            for child in &node_children {
                if child.kind() == "identifier" {
                    self.constant(&self.text(*child), *child, node, const_type.as_ref());
                }
            }
        }
    }

    fn constant(
        &mut self,
        name: &str,
        at: Node<'_>,
        declaration: Node<'_>,
        type_name: Option<&String>,
    ) {
        let (parent, full) = self.scoped(name);
        let scope = if parent.is_some() {
            Scope::Class
        } else {
            Scope::Global
        };
        let mut symbol = self.symbol(name, SymbolType::Constant, scope, Source::range(at));
        symbol.parent_symbol = parent;
        symbol.full_name = Some(full);
        symbol.return_type = type_name.cloned();
        symbol.source_text = Some(self.text(declaration));
        self.symbols.push(symbol);
    }

    /// The function declarator of a definition or field, directly or under
    /// a pointer / reference declarator.
    fn find_function_declarator(node: Node<'_>) -> Option<Node<'_>> {
        find(node, "function_declarator").or_else(|| {
            children(node)
                .into_iter()
                .filter(|c| matches!(c.kind(), "reference_declarator" | "pointer_declarator"))
                .find_map(|c| find(c, "function_declarator"))
        })
    }

    fn function_definition(&mut self, node: Node<'_>) {
        // Python quirk: `class EXPORT Name {…}` parses as a function
        // definition holding the class; only the class is extracted.
        if let Some(class) = find(node, "class_specifier") {
            self.class_or_struct(class, SymbolType::Class);
            return;
        }
        if let Some(record) = find(node, "struct_specifier") {
            self.class_or_struct(record, SymbolType::Struct);
            return;
        }
        let Some(declarator) = Self::find_function_declarator(node) else {
            return;
        };
        let qualified = self.qualified_parts(declarator).filter(|p| p.len() > 1);
        let (name, parent, full, symbol_type) = if let Some(parts) = &qualified {
            let name = parts[parts.len() - 1].clone();
            let mut absolute = self.scope_stack.clone();
            absolute.extend(parts.iter().cloned());
            let parent = absolute[..absolute.len() - 1].join("::");
            let full = absolute.join("::");
            let parent_class = &parts[parts.len() - 2];
            let parent_base = parent_class.split('<').next().unwrap_or("");
            // Python heuristic: a capitalised qualifier is a class.
            let symbol_type = if name == parent_base {
                SymbolType::Constructor
            } else if super::names::starts_upper(parent_class) {
                SymbolType::Method
            } else {
                SymbolType::Function
            };
            (name, Some(parent), full, symbol_type)
        } else {
            let Some(name) = self.function_name(declarator).filter(|n| !n.is_empty()) else {
                return;
            };
            let (parent, full) = self.scoped(&name);
            let symbol_type = match self.class_scope_stack.last() {
                Some(class) if parent.is_some() && *class == name => SymbolType::Constructor,
                Some(_) => SymbolType::Method,
                None => SymbolType::Function,
            };
            (name, parent, full, symbol_type)
        };
        let return_type = self.return_type(node);
        let parameters = self.parameters(declarator);
        let is_const = self.is_const_method(declarator);
        let is_override = self.is_override(declarator);
        let parameter_types: Vec<String> = parameters.iter().map(|p| p.type_name.clone()).collect();
        let mut symbol = self.symbol(&name, symbol_type, Scope::Function, Source::range(node));
        symbol.parent_symbol.clone_from(&parent);
        symbol.full_name = Some(full.clone());
        symbol.return_type.clone_from(&return_type);
        symbol.parameter_types.clone_from(&parameter_types);
        symbol.source_text = Some(self.text(node));
        self.template_metadata(&mut symbol.metadata);
        if is_const {
            symbol
                .metadata
                .insert("is_const_method".to_owned(), Value::Bool(true));
        }
        if is_override {
            symbol
                .metadata
                .insert("is_override".to_owned(), Value::Bool(true));
        }
        symbol
            .metadata
            .insert("has_body".to_owned(), Value::Bool(true));
        symbol.metadata.insert(
            "has_qualified_name".to_owned(),
            Value::Bool(qualified.is_some()),
        );
        self.symbols.push(symbol);
        if is_override {
            self.overrides(parent.as_deref(), &full, &name, node);
        }
        self.signature_relationships(&full, return_type.as_deref(), &parameter_types, node);
        self.parameter_symbols(&parameters, &full, false);
    }

    /// Phase 5: `overrides` to the FIRST base's member of the same name.
    fn overrides(&mut self, parent: Option<&str>, full: &str, name: &str, node: Node<'_>) {
        let Some(parent) = parent.filter(|p| !p.is_empty()) else {
            return;
        };
        let Some(base) = self.class_bases.get(parent).and_then(|b| b.first()) else {
            return;
        };
        let mut annotations = Map::new();
        annotations.insert("override_keyword".to_owned(), json!("override"));
        let relationship = self.relationship(
            full,
            &format!("{base}::{name}"),
            RelationshipType::Overrides,
            node,
            annotations,
        );
        self.relationships.push(relationship);
    }

    /// `PARAMETER` symbols; a method declaration's carry a full name.
    fn parameter_symbols(&mut self, parameters: &[Parameter], parent: &str, with_full_name: bool) {
        for parameter in parameters {
            let mut symbol = self.symbol(
                &parameter.name,
                SymbolType::Parameter,
                Scope::Function,
                parameter.range,
            );
            symbol.parent_symbol = Some(parent.to_owned());
            if with_full_name {
                symbol.full_name = Some(format!("{parent}::{}", parameter.name));
            }
            symbol.return_type = Some(parameter.type_name.clone());
            self.symbols.push(symbol);
        }
    }

    /// `_extract_function_type_relationships`: `references` to the
    /// user-defined types of the return and parameter types.
    fn signature_relationships(
        &mut self,
        function: &str,
        return_type: Option<&str>,
        parameter_types: &[String],
        node: Node<'_>,
    ) {
        if let Some(return_type) = return_type.filter(|r| !r.is_empty()) {
            for (target, _) in user_defined_types(return_type, &self.template_params, 0) {
                let mut annotations = Map::new();
                annotations.insert("usage".to_owned(), json!("return_type"));
                annotations.insert("return_type".to_owned(), json!(return_type));
                let relationship = self.relationship(
                    function,
                    &target,
                    RelationshipType::References,
                    node,
                    annotations,
                );
                self.relationships.push(relationship);
            }
        }
        for parameter_type in parameter_types.iter().filter(|p| !p.is_empty()) {
            for (target, _) in user_defined_types(parameter_type, &self.template_params, 0) {
                let mut annotations = Map::new();
                annotations.insert("usage".to_owned(), json!("parameter_type"));
                annotations.insert("parameter_type".to_owned(), json!(parameter_type));
                let relationship = self.relationship(
                    function,
                    &target,
                    RelationshipType::References,
                    node,
                    annotations,
                );
                self.relationships.push(relationship);
            }
        }
    }

    fn field_declaration(&mut self, node: Node<'_>) {
        if self.scope_stack.is_empty() {
            return;
        }
        if let Some(declarator) = Self::find_function_declarator(node) {
            self.method_declaration(node, declarator);
            return;
        }
        let base_type = find_any(
            node,
            &[
                "primitive_type",
                "type_identifier",
                "qualified_identifier",
                "template_type",
                "sized_type_specifier",
                "enum_specifier",
                "struct_specifier",
                "placeholder_type_specifier",
            ],
        )
        .map(|c| self.text(c));
        // Python: `base + '*' if base else None` (an empty base is None).
        let with_suffix = |suffix: &str| {
            base_type
                .as_ref()
                .filter(|b| !b.is_empty())
                .map(|b| format!("{b}{suffix}"))
        };
        for child in children(node) {
            let mut field_type = base_type.clone();
            let field_name = match child.kind() {
                "field_declarator" => {
                    let name = self.field_name(child);
                    if let Some(pointer) = find(child, "pointer_declarator") {
                        let stars = self.source.text(pointer).matches('*').count();
                        field_type = with_suffix(&"*".repeat(stars));
                    } else if find(child, "reference_declarator").is_some() {
                        field_type = with_suffix("&");
                    }
                    name
                }
                "pointer_declarator" => {
                    let stars = self.source.text(child).matches('*').count();
                    field_type = with_suffix(&"*".repeat(stars));
                    self.declarator_field_name(child)
                }
                "reference_declarator" => {
                    field_type = with_suffix("&");
                    self.declarator_field_name(child)
                }
                "array_declarator" => {
                    let mut inner = child;
                    while let Some(nested) = find(inner, "array_declarator") {
                        inner = nested;
                    }
                    find(inner, "field_identifier").map(|f| self.text(f))
                }
                "field_identifier" => Some(self.text(child)),
                _ => None,
            };
            let Some(field_name) = field_name.filter(|n| !n.is_empty()) else {
                continue;
            };
            let parent = self.scope_stack.join("::");
            let full = format!("{parent}::{field_name}");
            let mut symbol = self.symbol(
                &field_name,
                SymbolType::Field,
                Scope::Class,
                Source::range(child),
            );
            symbol.parent_symbol = Some(parent);
            symbol.full_name = Some(full.clone());
            symbol.return_type.clone_from(&field_type);
            self.symbols.push(symbol);
            if let Some(field_type) = field_type.filter(|t| !t.is_empty()) {
                for (target, by_pointer) in
                    user_defined_types(&field_type, &self.template_params, 0)
                {
                    let rel_type = if by_pointer {
                        RelationshipType::Aggregation
                    } else {
                        RelationshipType::Composition
                    };
                    let mut annotations = Map::new();
                    annotations.insert("field_name".to_owned(), json!(field_name));
                    annotations.insert("field_type".to_owned(), json!(field_type));
                    let relationship =
                        self.relationship(&full, &target, rel_type, node, annotations);
                    self.relationships.push(relationship);
                }
            }
        }
    }

    /// `_extract_field_name`: directly, or one pointer / reference down.
    fn field_name(&self, node: Node<'_>) -> Option<String> {
        if let Some(id) = find(node, "field_identifier") {
            return Some(self.text(id));
        }
        children(node)
            .into_iter()
            .filter(|c| matches!(c.kind(), "pointer_declarator" | "reference_declarator"))
            .find_map(|c| find(c, "field_identifier"))
            .map(|id| self.text(id))
    }

    /// `_extract_field_name_from_declarator`: any depth of pointers.
    fn declarator_field_name(&self, node: Node<'_>) -> Option<String> {
        if let Some(id) = find(node, "field_identifier") {
            return Some(self.text(id));
        }
        children(node)
            .into_iter()
            .filter(|c| matches!(c.kind(), "pointer_declarator" | "reference_declarator"))
            .find_map(|c| self.declarator_field_name(c).filter(|n| !n.is_empty()))
    }

    fn method_declaration(&mut self, node: Node<'_>, declarator: Node<'_>) {
        let Some(name) = self.function_name(declarator).filter(|n| !n.is_empty()) else {
            return;
        };
        let parent = self.scope_stack.join("::");
        let full = format!("{parent}::{name}");
        let symbol_type = if self.scope_stack.last() == Some(&name) {
            SymbolType::Constructor
        } else {
            SymbolType::Method
        };
        let return_type = self.return_type(node);
        let parameters = self.parameters(declarator);
        let is_const = self.is_const_method(declarator);
        let is_override = self.is_override(declarator);
        let mut symbol = self.symbol(&name, symbol_type, Scope::Function, Source::range(node));
        symbol.parent_symbol = Some(parent.clone());
        symbol.full_name = Some(full.clone());
        symbol.return_type = return_type;
        symbol.parameter_types = parameters.iter().map(|p| p.type_name.clone()).collect();
        symbol.source_text = Some(self.text(node));
        if is_const {
            symbol
                .metadata
                .insert("is_const_method".to_owned(), Value::Bool(true));
        }
        if is_override {
            symbol
                .metadata
                .insert("is_override".to_owned(), Value::Bool(true));
        }
        if !self.template_params.is_empty() {
            self.template_metadata(&mut symbol.metadata);
            symbol
                .comments
                .push(format!("TEMPLATE<{}>", self.template_params.join(", ")));
        }
        self.symbols.push(symbol);
        if is_override {
            self.overrides(Some(&parent), &full, &name, node);
        }
        self.parameter_symbols(&parameters, &full, true);
    }

    fn macro_def(&mut self, node: Node<'_>) {
        let Some(name_node) = find(node, "identifier") else {
            return;
        };
        let name = self.text(name_node);
        let value = find(node, "preproc_arg").map(|v| self.text(v));
        let (parent, full) = self.scoped(&name);
        let mut symbol = self.symbol(&name, SymbolType::Macro, Scope::Global, Source::range(node));
        symbol.parent_symbol = parent;
        symbol.full_name = Some(full);
        symbol.source_text = Some(self.text(node));
        symbol
            .metadata
            .insert("macro".to_owned(), Value::Bool(true));
        symbol.metadata.insert("value".to_owned(), json!(value));
        self.symbols.push(symbol);
    }

    fn function_macro(&mut self, node: Node<'_>) {
        let Some(name_node) = find(node, "identifier") else {
            return;
        };
        let name = self.text(name_node);
        let param_list = find(node, "preproc_params");
        let params: Vec<String> = param_list
            .map(|list| {
                children(list)
                    .into_iter()
                    .filter(|c| c.kind() == "identifier")
                    .map(|c| self.text(c))
                    .collect()
            })
            .unwrap_or_default();
        let body = find(node, "preproc_arg").map(|b| self.text(b));
        let (parent, full) = self.scoped(&name);
        let mut symbol = self.symbol(&name, SymbolType::Macro, Scope::Global, Source::range(node));
        symbol.parent_symbol = parent;
        symbol.full_name = Some(full.clone());
        symbol.parameter_types.clone_from(&params);
        symbol.source_text = Some(self.text(node));
        symbol
            .metadata
            .insert("macro".to_owned(), Value::Bool(true));
        symbol
            .metadata
            .insert("function_macro".to_owned(), Value::Bool(true));
        symbol.metadata.insert("body".to_owned(), json!(body));
        self.symbols.push(symbol);
        if let Some(list) = param_list {
            for (position, param) in params.iter().enumerate() {
                let mut symbol = self.symbol(
                    param,
                    SymbolType::Parameter,
                    Scope::Function,
                    Source::range(list),
                );
                symbol.parent_symbol = Some(full.clone());
                symbol.full_name = Some(format!("{full}::{param}"));
                symbol
                    .metadata
                    .insert("macro_param".to_owned(), Value::Bool(true));
                symbol
                    .metadata
                    .insert("position".to_owned(), json!(position));
                self.symbols.push(symbol);
            }
        }
    }

    fn include(&mut self, node: Node<'_>) {
        let Some((path, is_system)) = include_path(node, self.source) else {
            return;
        };
        let include_name = format!("include::{path}");
        let (parent, full) = self.scoped(&include_name);
        let mut symbol = self.symbol(
            &include_name,
            SymbolType::Variable,
            Scope::Global,
            Source::range(node),
        );
        symbol.parent_symbol = parent;
        symbol.full_name = Some(full);
        symbol.source_text = Some(self.text(node));
        symbol
            .metadata
            .insert("is_include".to_owned(), Value::Bool(true));
        symbol
            .metadata
            .insert("is_system".to_owned(), Value::Bool(is_system));
        symbol
            .metadata
            .insert("include_path".to_owned(), json!(path));
        symbol.metadata.insert(
            "include_type".to_owned(),
            json!(if is_system { "system" } else { "local" }),
        );
        self.symbols.push(symbol);
    }
}

/// `_extract_template_param_name`: the first type identifier or identifier.
pub(super) fn template_param_name(node: Node<'_>, source: &Source) -> Option<String> {
    find_any(node, &["type_identifier", "identifier"])
        .map(|c| source.text(c).to_owned())
        .filter(|n| !n.is_empty())
}

/// An `#include`'s path (`<…>` stripped, or a string's content) and
/// whether it is a system include. `None` when empty.
pub(super) fn include_path(node: Node<'_>, source: &Source) -> Option<(String, bool)> {
    if let Some(system) = find(node, "system_lib_string") {
        let path = source.text(system).trim_matches(['<', '>']).to_owned();
        return (!path.is_empty()).then_some((path, true));
    }
    let content = find(node, "string_literal").and_then(|s| find(s, "string_content"))?;
    let path = source.text(content).to_owned();
    (!path.is_empty()).then_some((path, false))
}

/// The pointer qualifier of a parameter: the declarator text before the
/// name, stripped, whitespace runs collapsed, `* *` → `**` and
/// `* const *` → `* const*` (the Python regex chain; its `\*\s+const`
/// step is the identity once whitespace is collapsed).
fn pointer_qualifier(before: &str) -> String {
    let mut collapsed = String::with_capacity(before.len());
    let mut in_space = false;
    for c in crate::graph::pystr::strip(before).chars() {
        if crate::graph::pystr::is_space(c) {
            if !in_space {
                collapsed.push(' ');
            }
            in_space = true;
        } else {
            collapsed.push(c);
            in_space = false;
        }
    }
    collapsed
        .replace("* *", "**")
        .replace("* const *", "* const*")
}

#[cfg(test)]
mod tests {
    use super::pointer_qualifier;

    #[test]
    fn pointer_qualifiers_normalise_as_the_regex_chain() {
        assert_eq!(pointer_qualifier("*"), "*");
        assert_eq!(pointer_qualifier("* *"), "**");
        assert_eq!(pointer_qualifier("*  const\n *"), "* const*");
        assert_eq!(pointer_qualifier("* * *"), "** *");
    }
}

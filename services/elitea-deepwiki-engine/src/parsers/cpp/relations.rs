//! Pass two over a file: the Python `CppEnhancedParser.RelationshipExtractor`.
//!
//! It runs on pass one's symbols BEFORE their names are normalised, so
//! every lookup here uses `::` names, as Python's does.
//!
//! Python quirks kept on purpose (each marked below), among them: a
//! `field_declaration`'s children are visited twice (its visitor visits
//! them and the dispatcher does again; `validate_result` later drops the
//! repeats), a top-level `declaration` is not entered at all, an
//! out-of-class definition's symbol is looked up WITHOUT the enclosing
//! namespaces (so it may resolve by bare name to another symbol), and
//! `#include` edges start at the file PATH (so `validate_result` drops them
//! unless they sit inside a function).

use super::names::{
    TYPE_KINDS, children, find, is_keyword, is_std_any, is_std_qualified, is_std_short,
    is_std_unqualified, qualified_parts_from_node, starts_upper,
};
use super::source::Source;
use super::symbols::{include_path, template_param_name};
use crate::parsers::model::{Relationship, RelationshipType, Symbol, SymbolType};
use indexmap::IndexMap;
use serde_json::{Map, Value, json};
use std::collections::{HashMap, HashSet};
use tree_sitter::Node;

/// Kinds whose visitor handled (or deliberately skipped) the children.
const NO_RECURSE: &[&str] = &[
    "class_specifier",
    "struct_specifier",
    "namespace_definition",
    "declaration",
    "function_definition",
    "translation_unit",
    "template_instantiation",
    "template_declaration",
];

pub(super) fn extract(
    root: Node<'_>,
    source: &Source,
    file_path: &str,
    stem: &str,
    symbols: &[Symbol],
) -> Vec<Relationship> {
    let mut by_name: HashMap<&str, Vec<usize>> = HashMap::new();
    let mut by_full: IndexMap<&str, Vec<usize>> = IndexMap::new();
    for (index, symbol) in symbols.iter().enumerate() {
        by_name.entry(symbol.name.as_str()).or_default().push(index);
        if let Some(full) = symbol.full_name.as_deref().filter(|f| !f.is_empty()) {
            by_full.entry(full).or_default().push(index);
        }
    }
    let mut extractor = Extractor {
        source,
        file_path,
        stem,
        symbols,
        by_name,
        by_full,
        relationships: Vec::new(),
        scope_stack: Vec::new(),
        current: None,
        current_function: None,
        template_params: Vec::new(),
    };
    extractor.visit(root);
    extractor.relationships
}

struct Extractor<'a, 't> {
    source: &'a Source,
    file_path: &'a str,
    stem: &'a str,
    symbols: &'a [Symbol],
    by_name: HashMap<&'a str, Vec<usize>>,
    /// Insertion-ordered: `defines` edges follow the first appearance of
    /// each full name.
    by_full: IndexMap<&'a str, Vec<usize>>,
    relationships: Vec<Relationship>,
    scope_stack: Vec<String>,
    /// `current_symbol`: the symbol whose body is being walked.
    current: Option<usize>,
    /// `current_function_node`.
    current_function: Option<Node<'t>>,
    template_params: Vec<String>,
}

impl<'t> Extractor<'_, 't> {
    fn text(&self, node: Node<'_>) -> String {
        self.source.text(node).to_owned()
    }

    /// `symbol.full_name or symbol.name`.
    fn name_of(&self, index: usize) -> String {
        let symbol = &self.symbols[index];
        symbol
            .full_name
            .clone()
            .filter(|f| !f.is_empty())
            .unwrap_or_else(|| symbol.name.clone())
    }

    fn current_name(&self) -> Option<String> {
        self.current.map(|i| self.name_of(i))
    }

    fn push(
        &mut self,
        source: &str,
        target: &str,
        rel_type: RelationshipType,
        range_node: Node<'_>,
        annotations: Map<String, Value>,
    ) {
        self.push_range(
            source,
            target,
            rel_type,
            Source::range(range_node),
            annotations,
        );
    }

    fn push_range(
        &mut self,
        source: &str,
        target: &str,
        rel_type: RelationshipType,
        range: crate::parsers::model::Range,
        annotations: Map<String, Value>,
    ) {
        let mut relationship = Relationship::new(source, target, rel_type, self.file_path);
        relationship.source_range = Some(range);
        relationship.annotations = annotations;
        self.relationships.push(relationship);
    }

    /// `_get_single_symbol`: by full name, else by name, the first.
    fn single(&self, full_name: &str, name: &str) -> Option<usize> {
        self.by_full
            .get(full_name)
            .or_else(|| self.by_name.get(name))
            .and_then(|c| c.first().copied())
    }

    fn visit(&mut self, node: Node<'t>) {
        let kind = node.kind();
        let custom = match kind {
            "translation_unit" => {
                self.translation_unit(node);
                true
            }
            "namespace_definition" => {
                self.namespace(node);
                true
            }
            "class_specifier" | "struct_specifier" => {
                self.class(node);
                true
            }
            "field_declaration" => {
                self.field_declaration(node);
                true
            }
            "function_definition" => {
                self.function_definition(node);
                true
            }
            "call_expression" => {
                self.call(node);
                true
            }
            "declaration" => {
                self.declaration(node);
                true
            }
            "identifier" => {
                self.identifier(node);
                true
            }
            "template_declaration" => {
                self.template_declaration(node);
                true
            }
            "template_type" => {
                self.template_type(node);
                true
            }
            "template_instantiation" => {
                self.template_instantiation(node);
                true
            }
            "preproc_include" => {
                self.include(node);
                true
            }
            "new_expression" => {
                self.new_expression(node);
                true
            }
            "field_initializer_list" => {
                self.field_initializer_list(node);
                true
            }
            "preproc_def" | "preproc_function_def" => true,
            _ => false,
        };
        if kind == "field_expression"
            && node.parent().is_some_and(|p| p.kind() != "call_expression")
        {
            self.field_access(node);
        }
        if !custom || !NO_RECURSE.contains(&kind) {
            for child in children(node) {
                self.visit(child);
            }
        }
    }

    fn translation_unit(&mut self, node: Node<'t>) {
        self.scope_stack.push(self.stem.to_owned());
        for child in children(node) {
            self.visit(child);
        }
        self.scope_stack.pop();
    }

    fn namespace(&mut self, node: Node<'t>) {
        if let Some(name) = find(node, "namespace_identifier") {
            self.scope_stack.push(self.text(name));
            for child in children(node) {
                self.visit(child);
            }
            self.scope_stack.pop();
        }
    }

    fn class(&mut self, node: Node<'t>) {
        let Some(name) = find(node, "type_identifier")
            .map(|n| self.text(n))
            .filter(|n| !n.is_empty())
        else {
            return;
        };
        self.scope_stack.push(name);
        self.inheritance(node);
        for child in children(node) {
            self.visit(child);
        }
        self.defines();
        self.scope_stack.pop();
    }

    /// `_extract_inheritance`. Python quirk: a qualified base keeps only
    /// its direct `identifier` / `type_identifier` parts, so `a::Base` is
    /// `Base` and `a::b::Base` is dropped.
    fn inheritance(&mut self, node: Node<'_>) {
        let Some(clause) = find(node, "base_class_clause") else {
            return;
        };
        for child in children(clause) {
            let base = match child.kind() {
                "type_identifier" | "identifier" => Some(self.text(child)),
                "qualified_identifier" => Some(
                    children(child)
                        .into_iter()
                        .filter(|c| matches!(c.kind(), "identifier" | "type_identifier"))
                        .map(|c| self.text(c))
                        .collect::<Vec<_>>()
                        .join("::"),
                ),
                "template_type" => find(child, "type_identifier").map(|n| self.text(n)),
                _ => None,
            };
            if let Some(base) = base.filter(|b| !b.is_empty()) {
                let source = self.scope_stack.join("::");
                self.push(
                    &source,
                    &base,
                    RelationshipType::Inheritance,
                    child,
                    Map::new(),
                );
            }
        }
    }

    /// `_extract_defines_relationships`: every method, constructor and
    /// field of the file whose parent is this class.
    fn defines(&mut self) {
        let class = self.scope_stack.join("::");
        let mut found = Vec::new();
        for indexes in self.by_full.values() {
            for &index in indexes {
                let symbol = &self.symbols[index];
                if matches!(
                    symbol.symbol_type,
                    SymbolType::Method | SymbolType::Constructor | SymbolType::Field
                ) && symbol.parent_symbol.as_deref() == Some(class.as_str())
                {
                    found.push(index);
                }
            }
        }
        for index in found {
            let symbol = &self.symbols[index];
            let mut annotations = Map::new();
            annotations.insert("member_type".to_owned(), json!(symbol.symbol_type.as_str()));
            let target = self.name_of(index);
            self.push_range(
                &class,
                &target,
                RelationshipType::Defines,
                symbol.range,
                annotations,
            );
        }
    }

    fn field_declaration(&mut self, node: Node<'t>) {
        if self.scope_stack.is_empty() {
            return;
        }
        let parent = self.scope_stack.join("::");
        for child in children(node) {
            match child.kind() {
                "type_identifier" => {
                    let type_name = self.text(child);
                    if !type_name.is_empty()
                        && !is_keyword(&type_name)
                        && !self.template_params.contains(&type_name)
                    {
                        self.push(
                            &parent,
                            &type_name,
                            RelationshipType::References,
                            child,
                            reference_type("field_type"),
                        );
                    }
                    break;
                }
                "qualified_identifier" => {
                    if let Some(template) = find(child, "template_type") {
                        let type_name = self.qualified_template_name(child, None);
                        if let Some(type_name) = type_name.filter(|t| !is_std_short(t)) {
                            self.push(
                                &parent,
                                &type_name,
                                RelationshipType::References,
                                child,
                                reference_type("field_type"),
                            );
                        }
                        self.template_arguments(template, &parent);
                    } else {
                        let type_name = qualified_parts_from_node(child, self.source).join("::");
                        if !type_name.is_empty() && !is_keyword(&type_name) {
                            self.push(
                                &parent,
                                &type_name,
                                RelationshipType::References,
                                child,
                                reference_type("field_type"),
                            );
                        }
                    }
                    break;
                }
                "template_type" => {
                    self.template_arguments(child, &parent);
                    break;
                }
                _ => {}
            }
        }
        // Python quirk: visited here AND by the dispatcher.
        for child in children(node) {
            self.visit(child);
        }
    }

    /// The `ns::tmpl` name of a qualified template type: its direct
    /// `namespace_identifier`s and each `template_type`'s name. With a
    /// source, each template's arguments are referenced as they are met.
    fn qualified_template_name(
        &mut self,
        node: Node<'_>,
        arguments_for: Option<&str>,
    ) -> Option<String> {
        let mut parts = Vec::new();
        for child in children(node) {
            match child.kind() {
                "namespace_identifier" => parts.push(self.text(child)),
                "template_type" => {
                    if let Some(name) = find(child, "type_identifier") {
                        parts.push(self.text(name));
                    }
                    if let Some(source) = arguments_for {
                        self.template_arguments(child, source);
                    }
                }
                _ => {}
            }
        }
        (!parts.is_empty()).then(|| parts.join("::"))
    }

    fn function_definition(&mut self, node: Node<'t>) {
        let Some(declarator) = find_function_declarator(node) else {
            return;
        };
        // Python quirk: unlike pass one, a qualified name is NOT prefixed
        // with the enclosing scopes.
        let (name, full) = match self.qualified_parts(declarator) {
            Some(parts) if parts.len() > 1 => (parts[parts.len() - 1].clone(), parts.join("::")),
            _ => {
                let Some(name) = self.function_name(declarator).filter(|n| !n.is_empty()) else {
                    return;
                };
                let full = if self.scope_stack.is_empty() {
                    name.clone()
                } else {
                    format!("{}::{name}", self.scope_stack.join("::"))
                };
                (name, full)
            }
        };
        let parameter_types = self.parameter_types(declarator);
        let is_const = is_const_method(declarator, self.source);
        let symbol = self.by_signature(&full, &name, &parameter_types, is_const);
        let (old_symbol, old_function) = (self.current, self.current_function);
        self.current = symbol;
        self.current_function = Some(node);
        if let Some(index) = symbol {
            self.defines_body(node, index);
            if let Some(list) = find(declarator, "parameter_list") {
                self.parameter_references(list, index);
            }
            self.return_type_references(node, index);
        }
        if let Some(list) = find(node, "field_initializer_list") {
            self.visit(list);
        }
        if let Some(body) = find(node, "compound_statement") {
            for child in children(body) {
                self.visit(child);
            }
        }
        self.current = old_symbol;
        self.current_function = old_function;
    }

    /// `_extract_qualified_name_parts` (this extractor's version: no
    /// `template_type`, no `operator_name`).
    fn qualified_parts(&self, declarator: Node<'_>) -> Option<Vec<String>> {
        let qualified = find(declarator, "qualified_identifier")?;
        let mut parts = Vec::new();
        self.collect_qualified(qualified, &mut parts);
        (!parts.is_empty()).then_some(parts)
    }

    fn collect_qualified(&self, node: Node<'_>, parts: &mut Vec<String>) {
        for child in children(node) {
            match child.kind() {
                "namespace_identifier" | "type_identifier" | "identifier" => {
                    parts.push(self.text(child));
                }
                "qualified_identifier" => self.collect_qualified(child, parts),
                _ => {}
            }
        }
    }

    /// `_extract_function_name` (this extractor's version).
    fn function_name(&self, declarator: Node<'_>) -> Option<String> {
        for child in children(declarator) {
            match child.kind() {
                "identifier" | "field_identifier" => return Some(self.text(child)),
                "qualified_identifier" => {
                    return qualified_parts_from_node(child, self.source).pop();
                }
                _ => {}
            }
        }
        None
    }

    /// `_extract_parameter_types_from_declarator`.
    fn parameter_types(&self, declarator: Node<'_>) -> Vec<String> {
        let Some(list) = find(declarator, "parameter_list") else {
            return Vec::new();
        };
        children(list)
            .into_iter()
            .filter(|c| c.kind() == "parameter_declaration")
            .filter_map(|c| self.parameter_type(c))
            .collect()
    }

    /// `_extract_type_from_param`.
    fn parameter_type(&self, node: Node<'_>) -> Option<String> {
        let mut type_name: Option<String> = None;
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
                "pointer_declarator" => {
                    let mut parts = String::new();
                    self.pointer_parts(child, &mut parts);
                    pointer = parts;
                }
                "reference_declarator" => {
                    let text = self.source.text(child);
                    if text.contains("&&") {
                        reference = "&&";
                    } else if text.contains('&') {
                        reference = "&";
                    }
                    if type_name.as_deref().is_none_or(str::is_empty)
                        && let Some(base) = find(child, "type_identifier")
                    {
                        type_name = Some(self.text(base));
                    }
                }
                _ => {}
            }
        }
        let mut full = type_name.filter(|t| !t.is_empty())?;
        if is_const {
            full = format!("const {full}");
        }
        full.push_str(&pointer);
        full.push_str(reference);
        Some(full)
    }

    fn pointer_parts(&self, node: Node<'_>, parts: &mut String) {
        for child in children(node) {
            match child.kind() {
                "*" => parts.push('*'),
                "type_qualifier" => {
                    if self.source.text(child).contains("const") {
                        parts.push_str(" const");
                    }
                }
                "pointer_declarator" => self.pointer_parts(child, parts),
                _ => {}
            }
        }
    }

    /// `_find_symbol_by_signature`: one candidate wins outright; else the
    /// first with equal parameter types and const-ness, else the first
    /// non-const with equal parameter types, else the first.
    fn by_signature(
        &self,
        full_name: &str,
        name: &str,
        parameter_types: &[String],
        is_const: bool,
    ) -> Option<usize> {
        let candidates = self
            .by_full
            .get(full_name)
            .or_else(|| self.by_name.get(name))?;
        if candidates.len() == 1 {
            return candidates.first().copied();
        }
        let const_flag = |index: usize| {
            self.symbols[index]
                .metadata
                .get("is_const_method")
                .is_some_and(truthy)
        };
        for &index in candidates {
            let symbol = &self.symbols[index];
            // Python: `metadata and metadata.get(..., False) == is_const`
            // (an empty metadata never matches).
            if symbol.parameter_types == parameter_types
                && !symbol.metadata.is_empty()
                && const_flag(index) == is_const
            {
                return Some(index);
            }
        }
        for &index in candidates {
            if self.symbols[index].parameter_types == parameter_types
                && !is_const
                && !const_flag(index)
            {
                return Some(index);
            }
        }
        candidates.first().copied()
    }

    /// `_extract_defines_body_relationship`.
    fn defines_body(&mut self, node: Node<'_>, index: usize) {
        if find(node, "compound_statement").is_none() {
            return;
        }
        let symbol = &self.symbols[index];
        let qualified = symbol
            .metadata
            .get("has_qualified_name")
            .is_some_and(truthy);
        let mut annotations = Map::new();
        annotations.insert(
            "implementation_type".to_owned(),
            json!(if qualified { "out_of_line" } else { "inline" }),
        );
        annotations.insert("has_body".to_owned(), Value::Bool(true));
        if !symbol.parameter_types.is_empty() {
            annotations.insert("parameter_types".to_owned(), json!(symbol.parameter_types));
        }
        if symbol.metadata.get("is_const_method").is_some_and(truthy) {
            annotations.insert("is_const_method".to_owned(), Value::Bool(true));
        }
        let name = self.name_of(index);
        let range = symbol.range;
        self.push_range(
            &name,
            &name,
            RelationshipType::DefinesBody,
            range,
            annotations,
        );
    }

    /// `_extract_parameter_references`.
    fn parameter_references(&mut self, list: Node<'_>, index: usize) {
        let source = self.name_of(index);
        for parameter in children(list) {
            if parameter.kind() != "parameter_declaration" {
                continue;
            }
            let mut type_name: Option<String> = None;
            for child in children(parameter) {
                match child.kind() {
                    "type_identifier" => {
                        type_name = Some(self.text(child));
                        break;
                    }
                    "qualified_identifier" => {
                        type_name = if find(child, "template_type").is_some() {
                            self.qualified_template_name(child, Some(&source))
                        } else {
                            Some(qualified_parts_from_node(child, self.source).join("::"))
                        };
                        break;
                    }
                    "struct_specifier" | "class_specifier" | "enum_specifier" => {
                        if let Some(name) = find(child, "type_identifier") {
                            type_name = Some(self.text(name));
                            break;
                        }
                    }
                    "template_type" => {
                        if let Some(name) = find(child, "type_identifier") {
                            type_name = Some(self.text(name));
                            self.template_arguments(child, &source);
                            break;
                        }
                    }
                    _ => {}
                }
            }
            if let Some(type_name) = type_name.filter(|t| !t.is_empty() && !is_keyword(t)) {
                self.push(
                    &source,
                    &type_name,
                    RelationshipType::References,
                    parameter,
                    reference_type("parameter_type"),
                );
            }
        }
    }

    /// `_extract_template_argument_references`: each user-defined type
    /// argument (nested templates first, then their own name).
    fn template_arguments(&mut self, template: Node<'_>, source: &str) {
        let Some(arguments) = find(template, "template_argument_list") else {
            return;
        };
        for argument in children(arguments) {
            if argument.kind() != "type_descriptor" {
                continue;
            }
            let mut type_name: Option<String> = None;
            for child in children(argument) {
                match child.kind() {
                    "type_identifier" => {
                        type_name = Some(self.text(child));
                        break;
                    }
                    "qualified_identifier" => {
                        type_name = if find(child, "template_type").is_some() {
                            self.qualified_template_name(child, Some(source))
                        } else {
                            Some(qualified_parts_from_node(child, self.source).join("::"))
                        };
                        break;
                    }
                    "template_type" => {
                        type_name = find(child, "type_identifier").map(|n| self.text(n));
                        self.template_arguments(child, source);
                        break;
                    }
                    _ => {}
                }
            }
            if let Some(type_name) =
                type_name.filter(|t| !t.is_empty() && !is_keyword(t) && !is_std_any(t))
            {
                self.push(
                    source,
                    &type_name,
                    RelationshipType::References,
                    argument,
                    reference_type("template_argument"),
                );
            }
        }
    }

    /// `_extract_return_type_references`: the FIRST type-like child decides.
    fn return_type_references(&mut self, node: Node<'_>, index: usize) {
        let source = self.name_of(index);
        let node_children = children(node);
        for (position, child) in node_children.iter().enumerate() {
            let child = *child;
            match child.kind() {
                "type_identifier" => {
                    let type_name = self.text(child);
                    if !type_name.is_empty() && !is_keyword(&type_name) {
                        self.push(
                            &source,
                            &type_name,
                            RelationshipType::References,
                            child,
                            reference_type("return_type"),
                        );
                    }
                    return;
                }
                "qualified_identifier" => {
                    if find(child, "template_type").is_some() {
                        let type_name = self.qualified_template_name(child, Some(&source));
                        if let Some(type_name) = type_name.filter(|t| !is_std_short(t)) {
                            self.push(
                                &source,
                                &type_name,
                                RelationshipType::References,
                                child,
                                reference_type("return_type"),
                            );
                        }
                    } else {
                        let type_name = qualified_parts_from_node(child, self.source).join("::");
                        if !type_name.is_empty()
                            && !is_keyword(&type_name)
                            && !is_std_qualified(&type_name)
                        {
                            self.push(
                                &source,
                                &type_name,
                                RelationshipType::References,
                                child,
                                reference_type("return_type"),
                            );
                        }
                    }
                    return;
                }
                "template_type" => {
                    if let Some(name) = find(child, "type_identifier") {
                        let type_name = self.text(name);
                        if !is_std_unqualified(&type_name) {
                            self.push(
                                &source,
                                &type_name,
                                RelationshipType::References,
                                child,
                                reference_type("return_type"),
                            );
                        }
                    }
                    self.template_arguments(child, &source);
                    return;
                }
                // Python: the type BEFORE a pointer / reference declarator.
                // (Unreachable in practice: that type child returned above.)
                "pointer_declarator" | "reference_declarator" => {
                    let Some(previous) = position.checked_sub(1).map(|p| node_children[p]) else {
                        continue;
                    };
                    if matches!(
                        previous.kind(),
                        "type_identifier" | "qualified_identifier" | "template_type"
                    ) {
                        let type_name = self.text(previous);
                        if !type_name.is_empty() && !is_keyword(&type_name) {
                            self.push(
                                &source,
                                &type_name,
                                RelationshipType::References,
                                previous,
                                reference_type("return_type"),
                            );
                        }
                        if previous.kind() != "type_identifier" {
                            self.template_arguments(previous, &source);
                        }
                        return;
                    }
                }
                _ => {}
            }
        }
    }

    fn call(&mut self, node: Node<'t>) {
        let Some(current) = self.current_name() else {
            return;
        };
        let Some(function) = children(node).into_iter().next() else {
            return;
        };
        let mut called: Option<String> = None;
        let mut annotations = Map::new();
        match function.kind() {
            "identifier" => called = Some(self.text(function)),
            "field_expression" => {
                if let Some(field) = find(function, "field_identifier") {
                    let method = self.text(field);
                    annotations = self.method_call_annotations(function);
                    called = Some(method);
                }
            }
            "qualified_identifier" => {
                let template_function = find(function, "template_function");
                if let Some(template_function) = template_function {
                    let mut parts = Vec::new();
                    for child in children(function) {
                        match child.kind() {
                            "namespace_identifier" | "identifier" | "type_identifier" => {
                                parts.push(self.text(child));
                            }
                            "qualified_identifier" => {
                                parts.extend(qualified_parts_from_node(child, self.source));
                            }
                            "template_function" => {
                                if let Some(name) = find(child, "identifier") {
                                    parts.push(self.text(name));
                                }
                            }
                            _ => {}
                        }
                    }
                    let name = parts.join("::");
                    if is_smart_pointer_factory(&name) {
                        self.smart_pointer(template_function, &name, node, &current);
                        return;
                    }
                    called = Some(name);
                } else {
                    called = Some(qualified_parts_from_node(function, self.source).join("::"));
                }
            }
            "template_function" => {
                if let Some(name) = find(function, "identifier") {
                    called = Some(self.text(name));
                } else if let Some(qualified) = find(function, "qualified_identifier") {
                    called = Some(qualified_parts_from_node(qualified, self.source).join("::"));
                }
                if let Some(name) = called.as_deref().filter(|n| is_smart_pointer_factory(n)) {
                    let name = name.to_owned();
                    self.smart_pointer(function, &name, node, &current);
                }
            }
            _ => {}
        }
        let Some(called) = called.filter(|c| !c.is_empty() && !is_keyword(c)) else {
            return;
        };
        let target = self.single(&called, &called);
        let is_constructor = target.is_some_and(|i| {
            matches!(
                self.symbols[i].symbol_type,
                SymbolType::Class | SymbolType::Struct
            )
        }) || (!called.contains("::") && starts_upper(&called));
        if is_constructor {
            let mut annotations = Map::new();
            annotations.insert("creation_type".to_owned(), json!("stack"));
            annotations.insert("creation_context".to_owned(), json!("statement"));
            self.push(
                &current,
                &called,
                RelationshipType::Creates,
                node,
                annotations,
            );
        } else {
            self.push(
                &current,
                &called,
                RelationshipType::Calls,
                node,
                annotations,
            );
        }
    }

    /// `_resolve_method_call_target`'s annotations; the target stays the
    /// bare method name.
    fn method_call_annotations(&self, field_expression: Node<'_>) -> Map<String, Value> {
        let mut annotations = Map::new();
        annotations.insert("call_type".to_owned(), json!("method_call"));
        let fe_children = children(field_expression);
        let access = if fe_children.iter().any(|c| c.kind() == "->") {
            "arrow"
        } else {
            "dot"
        };
        annotations.insert("access_type".to_owned(), json!(access));
        let Some(object) = fe_children.first() else {
            return annotations;
        };
        let object_name = self.text(*object);
        annotations.insert("object_name".to_owned(), json!(object_name));
        let Some(current) = self.current else {
            return annotations;
        };
        let current_symbol = &self.symbols[current];
        let candidates = self
            .by_name
            .get(object_name.as_str())
            .map_or(&[][..], Vec::as_slice);
        let truthy_type = |index: &usize| {
            self.symbols[*index]
                .return_type
                .clone()
                .filter(|t| !t.is_empty())
        };
        let mut object_type = candidates
            .iter()
            .filter(|&&i| {
                let symbol = &self.symbols[i];
                symbol.symbol_type == SymbolType::Parameter
                    && (symbol.parent_symbol == current_symbol.full_name
                        || symbol.parent_symbol.as_deref() == Some(current_symbol.name.as_str()))
            })
            .find_map(truthy_type);
        if object_type.is_none() {
            object_type = candidates
                .iter()
                .filter(|&&i| self.symbols[i].symbol_type == SymbolType::Field)
                .find_map(truthy_type);
        }
        if object_type.is_none()
            && let Some(function) = self.current_function
        {
            object_type = find(function, "compound_statement")
                .and_then(|body| self.local_variable_type(body, &object_name));
        }
        if let Some(object_type) = object_type {
            let cleaned = object_type.replace(['*', '&'], "").replace("const", "");
            annotations.insert(
                "resolved_class".to_owned(),
                json!(crate::graph::pystr::strip(&cleaned)),
            );
        }
        annotations
    }

    /// `_find_local_variable_type`'s `search_declarations`: depth first,
    /// the first declaration that declares `name` with a type.
    fn local_variable_type(&self, node: Node<'_>, name: &str) -> Option<String> {
        if node.kind() == "declaration" {
            let mut var_type: Option<String> = None;
            let mut has_var = false;
            for child in children(node) {
                match child.kind() {
                    "type_identifier"
                    | "qualified_identifier"
                    | "primitive_type"
                    | "template_type"
                    | "sized_type_specifier"
                    | "placeholder_type_specifier" => {
                        var_type = Some(self.text(child));
                    }
                    "init_declarator" => {
                        let Some(declarator) = children(child).into_iter().next() else {
                            continue;
                        };
                        match declarator.kind() {
                            "pointer_declarator" => {
                                if find(declarator, "identifier")
                                    .is_some_and(|id| self.source.text(id) == name)
                                {
                                    has_var = true;
                                    let stars = self.source.text(declarator).matches('*').count();
                                    var_type = var_type
                                        .filter(|t| !t.is_empty())
                                        .map(|t| format!("{t}{}", "*".repeat(stars)));
                                }
                            }
                            "reference_declarator" => {
                                if find(declarator, "identifier")
                                    .is_some_and(|id| self.source.text(id) == name)
                                {
                                    has_var = true;
                                    var_type =
                                        var_type.filter(|t| !t.is_empty()).map(|t| format!("{t}&"));
                                }
                            }
                            "identifier" => has_var |= self.source.text(declarator) == name,
                            _ => {}
                        }
                    }
                    "identifier" => has_var |= self.source.text(child) == name,
                    _ => {}
                }
            }
            if has_var && let Some(found) = var_type.filter(|t| !t.is_empty()) {
                return Some(found);
            }
        }
        children(node)
            .into_iter()
            .find_map(|child| self.local_variable_type(child, name))
    }

    fn field_access(&mut self, node: Node<'_>) {
        let Some(current) = self.current_name() else {
            return;
        };
        let Some(field) = find(node, "field_identifier") else {
            return;
        };
        let field_name = self.text(field);
        let target = self
            .by_name
            .get(field_name.as_str())
            .and_then(|indexes| {
                indexes.iter().find_map(|&i| {
                    let symbol = &self.symbols[i];
                    (symbol.symbol_type == SymbolType::Field)
                        .then(|| symbol.full_name.clone())
                        .flatten()
                        .filter(|f| !f.is_empty())
                })
            })
            .unwrap_or_else(|| field_name.clone());
        let mut annotations = reference_type("field_access");
        annotations.insert("field_name".to_owned(), json!(field_name));
        self.push(
            &current,
            &target,
            RelationshipType::References,
            node,
            annotations,
        );
    }

    fn declaration(&mut self, node: Node<'t>) {
        // Python quirk: outside a function nothing here is visited, not
        // even a class defined in the declaration.
        let Some(current) = self.current_name() else {
            return;
        };
        let node_children = children(node);
        let is_pointer_declaration = node_children
            .iter()
            .any(|c| c.kind() == "pointer_declarator");
        let mut type_name: Option<String> = None;
        let mut is_template_instantiation = false;
        let mut template_args = Vec::new();
        for &child in &node_children {
            match child.kind() {
                "type_identifier" => {
                    let name = self.text(child);
                    if !name.is_empty() && !is_keyword(&name) {
                        self.push(
                            &current,
                            &name,
                            RelationshipType::References,
                            child,
                            Map::new(),
                        );
                    }
                    type_name = Some(name);
                }
                "qualified_identifier" => {
                    if let Some(template) = find(child, "template_type") {
                        is_template_instantiation = true;
                        type_name = self.qualified_template_name(child, None);
                        if let Some(name) = type_name.clone().filter(|n| !n.is_empty()) {
                            self.push(
                                &current,
                                &name,
                                RelationshipType::References,
                                child,
                                reference_type("template_instantiation"),
                            );
                            self.template_arguments(template, &current);
                        }
                    } else {
                        let name = qualified_parts_from_node(child, self.source).join("::");
                        if !name.is_empty() {
                            self.push(
                                &current,
                                &name,
                                RelationshipType::References,
                                child,
                                Map::new(),
                            );
                        }
                        type_name = Some(name);
                    }
                }
                "template_type" => {
                    is_template_instantiation = true;
                    if let Some(name) =
                        self.declared_template(child, &current, is_pointer_declaration)
                    {
                        type_name = Some(name);
                    }
                    if let Some(arguments) = find(child, "template_argument_list") {
                        for argument in children(arguments) {
                            if argument.kind() == "type_descriptor"
                                && let Some(id) = find(argument, "type_identifier")
                            {
                                template_args.push(self.text(id));
                            }
                        }
                    }
                }
                _ => {}
            }
        }
        if let Some(type_name) = type_name.filter(|t| !t.is_empty()) {
            let creation = Creation {
                type_name: &type_name,
                is_template: is_template_instantiation,
                template_args: &template_args,
                current: &current,
            };
            self.declared_creations(&node_children, &creation);
        }
        for child in node_children {
            self.visit(child);
        }
    }

    /// A declaration's `Tmpl<Args>` type: `references` the template,
    /// `instantiates` it unless only a pointer is declared, then
    /// `references` its arguments. `None` without a template name.
    fn declared_template(
        &mut self,
        template: Node<'_>,
        current: &str,
        is_pointer_declaration: bool,
    ) -> Option<String> {
        let name = self.text(find(template, "type_identifier")?);
        self.push(
            current,
            &name,
            RelationshipType::References,
            template,
            reference_type("template_instantiation"),
        );
        if !is_pointer_declaration {
            let args = self.instantiation_arguments(template);
            let mut annotations = Map::new();
            if !args.is_empty() {
                annotations.insert("type_args".to_owned(), json!(args));
            }
            self.push(
                current,
                &name,
                RelationshipType::Instantiates,
                template,
                annotations,
            );
        }
        self.template_arguments(template, current);
        Some(name)
    }

    /// The `creates` edges of a declaration of a value: `T x(args)` per
    /// init declarator, else `T x;` (default construction).
    fn declared_creations(&mut self, node_children: &[Node<'_>], creation: &Creation<'_>) {
        let mut has_declarator = false;
        for &child in node_children {
            match child.kind() {
                "init_declarator" => {
                    self.stack_creation(
                        child,
                        creation.type_name,
                        creation.is_template,
                        creation.current,
                    );
                    has_declarator = true;
                }
                "pointer_declarator" => has_declarator = true,
                _ => {}
            }
        }
        if has_declarator {
            return;
        }
        let Some(identifier) = node_children.iter().find(|c| c.kind() == "identifier") else {
            return;
        };
        let mut annotations = Map::new();
        annotations.insert("creation_type".to_owned(), json!("stack"));
        annotations.insert("construction".to_owned(), json!("default"));
        annotations.insert("creation_context".to_owned(), json!("statement"));
        if creation.is_template {
            annotations.insert("template_instantiation".to_owned(), Value::Bool(true));
            if !creation.template_args.is_empty() {
                annotations.insert("template_args".to_owned(), json!(creation.template_args));
            }
        }
        self.push(
            creation.current,
            creation.type_name,
            RelationshipType::Creates,
            *identifier,
            annotations,
        );
    }

    /// The `type_args` of an `instantiates` edge.
    fn instantiation_arguments(&self, template: Node<'_>) -> Vec<String> {
        let Some(arguments) = find(template, "template_argument_list") else {
            return Vec::new();
        };
        let mut args = Vec::new();
        for argument in children(arguments) {
            match argument.kind() {
                "type_identifier" | "qualified_identifier" => args.push(self.text(argument)),
                "type_descriptor" => {
                    let chosen = find(argument, "primitive_type")
                        .or_else(|| find(argument, "type_identifier"))
                        .unwrap_or(argument);
                    args.push(self.text(chosen));
                }
                _ => {}
            }
        }
        args
    }

    /// `_extract_stack_creation`: `T x(args)` / `T x{args}` only.
    fn stack_creation(
        &mut self,
        init: Node<'_>,
        type_name: &str,
        is_template: bool,
        current: &str,
    ) {
        let init_children = children(init);
        if init_children
            .iter()
            .any(|c| matches!(c.kind(), "pointer_declarator" | "reference_declarator"))
        {
            return;
        }
        let explicit = init_children
            .iter()
            .find(|c| {
                matches!(
                    c.kind(),
                    "argument_list" | "initializer_list" | "call_expression" | "new_expression"
                )
            })
            .is_some_and(|c| matches!(c.kind(), "argument_list" | "initializer_list"));
        if explicit {
            let mut annotations = Map::new();
            annotations.insert("creation_type".to_owned(), json!("stack"));
            annotations.insert("construction".to_owned(), json!("explicit"));
            annotations.insert("creation_context".to_owned(), json!("statement"));
            if is_template {
                annotations.insert("template_instantiation".to_owned(), Value::Bool(true));
            }
            self.push(
                current,
                type_name,
                RelationshipType::Creates,
                init,
                annotations,
            );
        }
    }

    fn identifier(&mut self, node: Node<'_>) {
        let Some(current) = self.current else {
            return;
        };
        let name = self.text(node);
        if is_keyword(&name) || name == self.symbols[current].name {
            return;
        }
        if self.single(&name, &name).is_some() || starts_upper(&name) {
            let source = self.name_of(current);
            self.push(
                &source,
                &name,
                RelationshipType::References,
                node,
                Map::new(),
            );
        }
    }

    fn template_declaration(&mut self, node: Node<'t>) {
        let mut params = Vec::new();
        if let Some(list) = find(node, "template_parameter_list") {
            for child in children(list) {
                if matches!(
                    child.kind(),
                    "type_parameter_declaration" | "parameter_declaration"
                ) && let Some(name) = template_param_name(child, self.source)
                {
                    params.push(name);
                }
            }
        }
        let old = std::mem::replace(&mut self.template_params, params);
        for child in children(node) {
            if !matches!(child.kind(), "template" | "template_parameter_list") {
                self.visit(child);
            }
        }
        self.template_params = old;
    }

    /// `visit_template_type`: `specializes` from the class for a base
    /// template, else `references` from the current symbol; then each
    /// DIRECT type argument.
    fn template_type(&mut self, node: Node<'_>) {
        let Some(name_node) = find(node, "type_identifier") else {
            return;
        };
        let template_name = self.text(name_node);
        let Some(arguments) = find(node, "template_argument_list") else {
            return;
        };
        let mut in_base_clause = false;
        let mut parent = node.parent();
        while let Some(p) = parent {
            match p.kind() {
                "base_class_clause" => {
                    in_base_clause = true;
                    break;
                }
                "class_specifier"
                | "struct_specifier"
                | "function_definition"
                | "translation_unit" => break,
                _ => {}
            }
            parent = p.parent();
        }
        if in_base_clause && let Some(class) = self.scope_stack.last().cloned() {
            self.push(
                &class,
                &template_name,
                RelationshipType::Specializes,
                node,
                Map::new(),
            );
        } else if let Some(current) = self.current_name() {
            self.push(
                &current,
                &template_name,
                RelationshipType::References,
                node,
                Map::new(),
            );
        }
        let Some(current) = self.current_name() else {
            return;
        };
        let template_params: HashSet<String> = self.template_params.iter().cloned().collect();
        for child in children(arguments) {
            if matches!(child.kind(), "type_identifier" | "qualified_identifier") {
                let argument = self.text(child);
                if !is_keyword(&argument) && !template_params.contains(&argument) {
                    self.push(
                        &current,
                        &argument,
                        RelationshipType::References,
                        child,
                        Map::new(),
                    );
                }
            }
        }
    }

    fn template_instantiation(&mut self, node: Node<'_>) {
        let mut template_name: Option<String> = None;
        let mut type_args = Vec::new();
        self.find_instantiated(node, &mut template_name, &mut type_args);
        let Some(template_name) = template_name.filter(|n| !n.is_empty()) else {
            return;
        };
        let source = self.current_name().unwrap_or_else(|| self.stem.to_owned());
        let mut annotations = Map::new();
        if !type_args.is_empty() {
            annotations.insert("type_args".to_owned(), json!(type_args));
        }
        self.push(
            &source,
            &template_name,
            RelationshipType::Instantiates,
            node,
            annotations,
        );
    }

    /// `_find_template_type`.
    fn find_instantiated(&self, node: Node<'_>, name: &mut Option<String>, args: &mut Vec<String>) {
        for child in children(node) {
            match child.kind() {
                "template_type" => {
                    if let Some(id) = find(child, "type_identifier") {
                        *name = Some(self.text(id));
                    }
                    if let Some(list) = find(child, "template_argument_list") {
                        for argument in children(list) {
                            match argument.kind() {
                                "type_identifier" | "qualified_identifier" => {
                                    args.push(self.text(argument));
                                }
                                "type_descriptor" => {
                                    let chosen =
                                        find(argument, "primitive_type").unwrap_or(argument);
                                    args.push(self.text(chosen));
                                }
                                _ => {}
                            }
                        }
                    }
                    return;
                }
                "class_specifier" | "struct_specifier" => {
                    self.find_instantiated(child, name, args);
                    if name.as_deref().is_some_and(|n| !n.is_empty()) {
                        return;
                    }
                }
                "type_identifier" | "qualified_identifier"
                    if name.as_deref().is_none_or(str::is_empty) =>
                {
                    *name = Some(self.text(child));
                }
                _ => {}
            }
        }
    }

    fn include(&mut self, node: Node<'_>) {
        let Some((path, is_system)) = include_path(node, self.source) else {
            return;
        };
        let source = self
            .current_name()
            .unwrap_or_else(|| self.file_path.to_owned());
        let mut annotations = Map::new();
        annotations.insert("is_system".to_owned(), Value::Bool(is_system));
        annotations.insert("include_path".to_owned(), json!(path));
        annotations.insert(
            "include_type".to_owned(),
            json!(if is_system { "system" } else { "local" }),
        );
        self.push(
            &source,
            &format!("include::{path}"),
            RelationshipType::Imports,
            node,
            annotations,
        );
    }

    fn new_expression(&mut self, node: Node<'_>) {
        let Some(current) = self.current_name() else {
            return;
        };
        let node_children = children(node);
        let is_placement = node_children
            .get(1)
            .is_some_and(|c| c.kind() == "argument_list");
        let mut type_name: Option<String> = None;
        let mut is_array = false;
        for &child in &node_children {
            match child.kind() {
                "type_identifier" => type_name = Some(self.text(child)),
                "qualified_identifier" => {
                    type_name = Some(qualified_parts_from_node(child, self.source).join("::"));
                }
                "template_type" => {
                    if let Some(name) = find(child, "type_identifier") {
                        type_name = Some(self.text(name));
                    }
                }
                "new_declarator" => is_array = true,
                _ => {}
            }
        }
        let Some(type_name) = type_name.filter(|t| !t.is_empty() && !is_keyword(t)) else {
            return;
        };
        let creation = if is_placement {
            "placement_new"
        } else if is_array {
            "new_array"
        } else {
            "new"
        };
        let mut annotations = Map::new();
        annotations.insert("creation_type".to_owned(), json!(creation));
        annotations.insert("creation_context".to_owned(), json!("statement"));
        self.push(
            &current,
            &type_name,
            RelationshipType::Creates,
            node,
            annotations,
        );
    }

    fn field_initializer_list(&mut self, node: Node<'_>) {
        let Some(current) = self.current_name() else {
            return;
        };
        for child in children(node) {
            if child.kind() == "field_initializer" {
                self.field_initializer(child, &current);
            }
        }
    }

    /// `_extract_field_initializer_creation`. Python quirk: the field is
    /// looked up as `<innermost scope>::<field>`, which for an out-of-class
    /// constructor is the namespace, so it usually resolves by bare name —
    /// possibly to a parameter, whose type then names the creation.
    fn field_initializer(&mut self, node: Node<'_>, current: &str) {
        let mut field_name: Option<String> = None;
        let mut has_constructor_call = false;
        for child in children(node) {
            match child.kind() {
                "field_identifier" => field_name = Some(self.text(child)),
                "argument_list" | "initializer_list" => has_constructor_call = true,
                _ => {}
            }
        }
        let Some(field_name) = field_name.filter(|f| !f.is_empty()) else {
            return;
        };
        if !has_constructor_call {
            return;
        }
        let Some(class) = self.scope_stack.last().filter(|c| !c.is_empty()) else {
            return;
        };
        let Some(index) = self.single(&format!("{class}::{field_name}"), &field_name) else {
            return;
        };
        let Some(return_type) = self.symbols[index]
            .return_type
            .as_deref()
            .filter(|t| !t.is_empty())
        else {
            return;
        };
        let cleaned = return_type.replace(['*', '&'], "");
        let type_name = crate::graph::pystr::strip(&cleaned).to_owned();
        if type_name.is_empty() || is_keyword(&type_name) {
            return;
        }
        let mut annotations = Map::new();
        annotations.insert("creation_type".to_owned(), json!("initializer_list"));
        annotations.insert("creation_context".to_owned(), json!("initializer_list"));
        annotations.insert("member_name".to_owned(), json!(field_name));
        self.push(
            current,
            &type_name,
            RelationshipType::Creates,
            node,
            annotations,
        );
    }

    /// `_extract_smart_pointer_creation`.
    fn smart_pointer(
        &mut self,
        template_function: Node<'_>,
        called: &str,
        call: Node<'_>,
        current: &str,
    ) {
        let Some(arguments) = find(template_function, "template_argument_list") else {
            return;
        };
        let mut type_name: Option<String> = None;
        for child in children(arguments) {
            match child.kind() {
                "type_identifier" => {
                    type_name = Some(self.text(child));
                    break;
                }
                "type_descriptor" => {
                    if let Some(id) = find(child, "type_identifier") {
                        type_name = Some(self.text(id));
                        break;
                    }
                    // Python quirk: this inner `break` leaves only the inner
                    // loop; a later argument may still set the type.
                    for inner in children(child) {
                        match inner.kind() {
                            "template_type" => {
                                if let Some(name) = find(inner, "type_identifier") {
                                    type_name = Some(self.text(name));
                                }
                                break;
                            }
                            "qualified_identifier" => {
                                type_name = Some(self.text(inner));
                                break;
                            }
                            _ => {}
                        }
                    }
                }
                "qualified_identifier" => {
                    type_name = Some(qualified_parts_from_node(child, self.source).join("::"));
                    break;
                }
                "template_type" => {
                    if let Some(name) = find(child, "type_identifier") {
                        type_name = Some(self.text(name));
                    }
                    break;
                }
                _ => {}
            }
        }
        let Some(type_name) = type_name.filter(|t| !t.is_empty() && !is_keyword(t)) else {
            return;
        };
        let creation = if called.contains("make_shared") {
            "make_shared"
        } else {
            "make_unique"
        };
        let mut annotations = Map::new();
        annotations.insert("creation_type".to_owned(), json!(creation));
        annotations.insert("creation_context".to_owned(), json!("statement"));
        self.push(
            current,
            &type_name,
            RelationshipType::Creates,
            call,
            annotations,
        );
    }
}

/// What a value declaration creates.
struct Creation<'c> {
    type_name: &'c str,
    is_template: bool,
    template_args: &'c [String],
    current: &'c str,
}

fn is_smart_pointer_factory(name: &str) -> bool {
    !name.is_empty() && (name.contains("make_unique") || name.contains("make_shared"))
}

/// The function declarator of a definition, directly or under a pointer /
/// reference declarator.
fn find_function_declarator(node: Node<'_>) -> Option<Node<'_>> {
    find(node, "function_declarator").or_else(|| {
        children(node)
            .into_iter()
            .filter(|c| matches!(c.kind(), "pointer_declarator" | "reference_declarator"))
            .find_map(|c| find(c, "function_declarator"))
    })
}

fn is_const_method(declarator: Node<'_>, source: &Source) -> bool {
    declarator.kind() == "function_declarator"
        && children(declarator)
            .iter()
            .any(|c| c.kind() == "type_qualifier" && source.text(*c).contains("const"))
}

fn reference_type(kind: &str) -> Map<String, Value> {
    let mut annotations = Map::new();
    annotations.insert("reference_type".to_owned(), json!(kind));
    annotations
}

/// Python truthiness of a metadata value.
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

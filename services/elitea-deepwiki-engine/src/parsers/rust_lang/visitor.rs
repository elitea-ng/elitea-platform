//! One Rust file: the Python `RustVisitorParser.parse_file`, item handlers
//! and `use` declarations. Function bodies and signature references are in
//! [`super::body`].
//!
//! Every rule here is the Python visitor's, quirks included, because the
//! graph gate compares this output with the Python output field by field.
//! Each quirk the port keeps on purpose is marked "Python quirk". Node kinds
//! are the `tree-sitter-rust` 0.24 grammar's, which is the grammar
//! `tree_sitter_language_pack` 1.16.1 ships (same state, symbol, alias and
//! field counts). In that grammar a generic parameter is `type_parameter`,
//! not `constrained_type_parameter`, so several Python branches never fire.

use super::{REFERENCE_WRAPPERS, is_builtin_type};
use crate::parsers::limits;
use crate::parsers::model::{
    ParseResult, Range, Relationship, RelationshipType, Scope, Symbol, SymbolType,
};
use indexmap::IndexMap;
use serde_json::{Map, Value};
use std::collections::HashMap;
use tree_sitter::Node;

/// Tuple-struct field types (`_extract_tuple_struct_fields`).
const TUPLE_FIELD_KINDS: &[&str] = &[
    "type_identifier",
    "generic_type",
    "reference_type",
    "scoped_type_identifier",
    "primitive_type",
    "tuple_type",
    "array_type",
    "dynamic_type",
    "abstract_type",
];

/// Named-field types (`_extract_field_type_string`).
const FIELD_TYPE_KINDS: &[&str] = &[
    "type_identifier",
    "generic_type",
    "reference_type",
    "scoped_type_identifier",
    "primitive_type",
    "tuple_type",
    "array_type",
    "dynamic_type",
    "abstract_type",
    "function_type",
    "macro_invocation",
];

/// Tuple enum-variant field types: a shorter list than a tuple struct's.
const VARIANT_TUPLE_KINDS: &[&str] = &[
    "type_identifier",
    "generic_type",
    "reference_type",
    "scoped_type_identifier",
    "primitive_type",
];

/// The aliased type of a `type` item.
const ALIAS_TARGET_KINDS: &[&str] = &[
    "type_identifier",
    "generic_type",
    "scoped_type_identifier",
    "reference_type",
    "tuple_type",
    "array_type",
    "function_type",
    "dynamic_type",
    "abstract_type",
];

/// `(name, bound)` pairs, as Python's type-parameter tuples.
type Pair = (String, String);

/// Parse one Rust source (already decoded as Python decodes it) into the
/// per-file result. Cross-file linking happens later, in `super`.
pub(super) fn parse_source(file_path: &str, source: &str) -> ParseResult {
    let mut parser = tree_sitter::Parser::new();
    let language: tree_sitter::Language = tree_sitter_rust::LANGUAGE.into();
    if parser.set_language(&language).is_err() {
        return failed(file_path, "Tree-sitter Rust parser not available");
    }
    let Some(tree) = parser.parse(source, None) else {
        return failed(file_path, "Tree-sitter Rust parser returned no tree");
    };
    if limits::too_deep(tree.root_node()) {
        return failed(file_path, limits::RECURSION_ERROR);
    }
    let mut visitor = Visitor::new(file_path, source);
    visitor.visit_top_level(tree.root_node());
    ParseResult {
        imports: visitor.imports.values().cloned().collect(),
        symbols: visitor.symbols,
        relationships: visitor.relationships,
        ..ParseResult::new(file_path, "rust")
    }
}

/// A result that carries only an error, as every Python failure path builds.
pub(super) fn failed(file_path: &str, error: impl Into<String>) -> ParseResult {
    let mut result = ParseResult::new(file_path, "rust");
    result.errors.push(error.into());
    result
}

/// Options of `_handle_function_item` for a method.
#[derive(Default, Clone, Copy)]
pub(super) struct MethodContext<'a> {
    pub parent: Option<&'a str>,
    pub impl_kind: Option<&'a str>,
    pub impl_trait: Option<&'a str>,
    pub is_trait_default: bool,
}

/// Per-file visitor state (the Python parser's `_current_*` / `_pending_*`).
pub(super) struct Visitor<'s> {
    pub(super) file: &'s str,
    source: &'s str,
    /// `Path(file).stem`: the source of every `imports` edge.
    stem: String,
    pub(super) symbols: Vec<Symbol>,
    pub(super) relationships: Vec<Relationship>,
    /// Short name → import path. A later import with the same key replaces
    /// the value in place, as a Python dict assignment does.
    pub(super) imports: IndexMap<String, String>,
    /// The impl block being visited, for `Self` resolution.
    pub(super) impl_target: Option<String>,
    /// Python quirk: pending derives and attributes live on the parser, so
    /// an attribute that no item consumes (one before a `use`, or the last
    /// member of an impl) is applied to the next item that consumes them.
    pending_derives: Vec<String>,
    pending_attributes: Vec<Value>,
    /// `extern crate x as y`: effective name → crate name.
    pub(super) extern_crate_aliases: HashMap<String, String>,
}

impl<'s> Visitor<'s> {
    fn new(file: &'s str, source: &'s str) -> Self {
        let stem = std::path::Path::new(file)
            .file_stem()
            .map(|s| s.to_string_lossy().into_owned())
            .unwrap_or_default();
        Self {
            file,
            source,
            stem,
            symbols: Vec::new(),
            relationships: Vec::new(),
            imports: IndexMap::new(),
            impl_target: None,
            pending_derives: Vec::new(),
            pending_attributes: Vec::new(),
            extern_crate_aliases: HashMap::new(),
        }
    }

    /// `_get_node_text`: the node's own UTF-8 bytes.
    pub(super) fn text(&self, node: Node<'_>) -> &'s str {
        self.source.get(node.byte_range()).unwrap_or("")
    }

    fn symbol(&self, name: &str, symbol_type: SymbolType, scope: Scope, node: Node<'_>) -> Symbol {
        Symbol::new(name, symbol_type, scope, range_of(node), self.file)
    }

    /// `_make_relationship`: the target file is always this file.
    pub(super) fn relate(
        &mut self,
        source: &str,
        target: &str,
        rel_type: RelationshipType,
        node: Node<'_>,
        annotations: Map<String, Value>,
    ) {
        let mut relationship = Relationship::new(source, target, rel_type, self.file);
        relationship.target_file = Some(self.file.to_owned());
        relationship.source_range = Some(range_of(node));
        relationship.annotations = annotations;
        self.relationships.push(relationship);
    }

    fn visit_top_level(&mut self, root: Node<'_>) {
        for child in children(root) {
            match child.kind() {
                "attribute_item" => self.attribute(child),
                "struct_item" => self.struct_item(child),
                "enum_item" => self.enum_item(child),
                "trait_item" => self.trait_item(child),
                "impl_item" => self.impl_item(child),
                "function_item" => self.function_item(child, MethodContext::default()),
                "const_item" => self.const_item(child, None),
                "static_item" => self.static_item(child),
                "type_item" => self.type_item(child, None),
                "mod_item" => self.mod_item(child),
                "use_declaration" => self.use_declaration(child),
                "macro_definition" => self.macro_definition(child),
                "union_item" => self.union_item(child),
                "extern_crate_declaration" => self.extern_crate(child),
                // Python quirk: anything else, a comment included, drops
                // the pending derives and attributes.
                _ => {
                    self.pending_derives.clear();
                    self.pending_attributes.clear();
                }
            }
        }
    }

    fn consume_pending(&mut self) -> (Vec<String>, Vec<Value>) {
        (
            std::mem::take(&mut self.pending_derives),
            std::mem::take(&mut self.pending_attributes),
        )
    }

    /// `_get_visibility`.
    pub(super) fn visibility(&self, node: Node<'_>) -> &'static str {
        let Some(modifier) = child_of_kind(node, "visibility_modifier") else {
            return "private";
        };
        let text = self.text(modifier);
        if text == "pub" {
            "public"
        } else if text.contains("crate") {
            "crate"
        } else if text.contains("super") {
            "protected"
        } else if text.contains("in") {
            "restricted"
        } else {
            "public"
        }
    }

    /// `_get_preceding_comment`: the run of `///`, `//!` and `/** */`
    /// comments right before the item. Python quirk: an attribute between
    /// the doc comment and the item hides the comment, and a plain `//`
    /// comment ends the run.
    pub(super) fn preceding_comment(&self, node: Node<'_>) -> Option<String> {
        let mut comments = Vec::new();
        let mut previous = node.prev_named_sibling();
        while let Some(prev) = previous {
            if !matches!(prev.kind(), "line_comment" | "block_comment") {
                break;
            }
            let text = self.text(prev);
            if let Some(rest) = text
                .strip_prefix("///")
                .or_else(|| text.strip_prefix("//!"))
            {
                comments.push(py_strip(rest));
            } else if text.starts_with("/**") && text.ends_with("*/") {
                // `text[3:-2]`, empty when the two ends overlap (`/**/`).
                comments.push(py_strip(text.get(3..text.len() - 2).unwrap_or("")));
            } else {
                break;
            }
            previous = prev.prev_named_sibling();
        }
        if comments.is_empty() {
            return None;
        }
        comments.reverse();
        Some(comments.join("\n"))
    }

    /// `_extract_function_modifiers`: substring tests on the modifiers text.
    pub(super) fn function_modifiers(&self, node: Node<'_>) -> Map<String, Value> {
        let mut modifiers = Map::new();
        if let Some(m) = child_of_kind(node, "function_modifiers") {
            let text = self.text(m);
            for (word, key) in [
                ("async", "is_async"),
                ("unsafe", "is_unsafe"),
                ("extern", "is_extern"),
            ] {
                if text.contains(word) {
                    modifiers.insert(key.to_owned(), Value::Bool(true));
                }
            }
        }
        modifiers
    }

    /// `_extract_type_parameters`. Python quirk: the grammar's generic
    /// parameter is `type_parameter`, which Python does not list, so only a
    /// bare `type_identifier` or `lifetime` child (neither occurs) would
    /// count — this is empty in practice, and kept as written.
    fn type_parameters(&self, node: Node<'_>) -> Vec<Pair> {
        let mut params = Vec::new();
        let Some(list) = child_of_kind(node, "type_parameters") else {
            return params;
        };
        for child in children(list) {
            match child.kind() {
                "type_identifier" => params.push((self.text(child).to_owned(), String::new())),
                "constrained_type_parameter" => {
                    let name = child_of_kind(child, "type_identifier").map_or("", |n| self.text(n));
                    let bounds = child_of_kind(child, "trait_bounds").map_or("", |n| self.text(n));
                    params.push((name.to_owned(), bounds.to_owned()));
                }
                "lifetime" => params.push((self.text(child).to_owned(), "lifetime".to_owned())),
                _ => {}
            }
        }
        params
    }

    /// `_extract_lifetimes`: lifetime names without the leading `'`.
    fn lifetimes(&self, node: Node<'_>) -> Vec<String> {
        let mut lifetimes = Vec::new();
        let Some(list) = child_of_kind(node, "type_parameters") else {
            return lifetimes;
        };
        for child in children(list) {
            let lifetime = match child.kind() {
                "lifetime_parameter" => child_of_kind(child, "lifetime"),
                "lifetime" => Some(child),
                _ => None,
            };
            if let Some(rest) = lifetime.and_then(|l| self.text(l).strip_prefix('\'')) {
                lifetimes.push(rest.to_owned());
            }
        }
        lifetimes
    }

    // ------------------------------------------------------------------
    // Items
    // ------------------------------------------------------------------

    /// `_handle_attribute`: `#[derive(…)]` (any attribute whose text holds
    /// `derive`, `cfg_attr(…, derive(…))` included) adds the comma-split
    /// text of its first token tree to the pending derives; any other
    /// attribute is kept as `{text, range}`.
    pub(super) fn attribute(&mut self, node: Node<'_>) {
        let text = self.text(node);
        if text.contains("derive") {
            if let Some(tree) = first_descendant_of_kind(node, "token_tree") {
                let inner = self.text(tree).trim_matches(['(', ')']);
                self.pending_derives.extend(
                    inner
                        .split(',')
                        .map(py_strip)
                        .filter(|d| !d.is_empty())
                        .map(str::to_owned),
                );
            }
        } else {
            let mut attribute = Map::new();
            attribute.insert("text".to_owned(), Value::String(text.to_owned()));
            attribute.insert("range".to_owned(), range_value(range_of(node)));
            self.pending_attributes.push(Value::Object(attribute));
        }
    }

    /// `ANNOTATES` edges from each non-builtin derive to the type.
    fn derive_edges(&mut self, name: &str, node: Node<'_>, derives: &[String]) {
        for derive in derives {
            if !is_builtin_type(derive) {
                let annotations = object([("derives", strings(derives))]);
                self.relate(derive, name, RelationshipType::Annotates, node, annotations);
            }
        }
    }

    fn struct_item(&mut self, node: Node<'_>) {
        let Some(name_node) = child_of_kind(node, "type_identifier") else {
            return;
        };
        let name = self.text(name_node);
        let (derives, attributes) = self.consume_pending();
        let type_params = self.type_parameters(node);
        let lifetimes = self.lifetimes(node);

        let mut metadata = Map::new();
        if !type_params.is_empty() {
            metadata.insert("type_parameters".to_owned(), pairs_value(&type_params));
        }
        if !lifetimes.is_empty() {
            metadata.insert("lifetimes".to_owned(), strings(&lifetimes));
        }
        if !derives.is_empty() {
            metadata.insert("derives".to_owned(), strings(&derives));
        }
        if !attributes.is_empty() {
            metadata.insert("attributes".to_owned(), Value::Array(attributes));
        }
        let mut symbol = self.symbol(name, SymbolType::Struct, Scope::Global, node);
        symbol.full_name = Some(name.to_owned());
        symbol.visibility = Some(self.visibility(node).to_owned());
        symbol.docstring = self.preceding_comment(node);
        symbol.source_text = limits::kept_str(self.text(node));
        symbol.signature = Some(format!("struct {name}{}", generic_suffix(&type_params)));
        symbol.metadata = metadata;
        self.symbols.push(symbol);

        self.derive_edges(name, node, &derives);
        if let Some(list) = child_of_kind(node, "field_declaration_list") {
            self.struct_fields(name, list);
        }
        if let Some(list) = child_of_kind(node, "ordered_field_declaration_list") {
            self.tuple_struct_fields(name, list);
        }
    }

    /// `_extract_struct_fields`.
    fn struct_fields(&mut self, struct_name: &str, list: Node<'_>) {
        for decl in children_of_kind(list, "field_declaration") {
            let Some(name_node) = child_of_kind(decl, "field_identifier") else {
                continue;
            };
            let field_name = self.text(name_node);
            let field_type = field_type_string(self, decl);
            let full_name = format!("{struct_name}.{field_name}");
            let mut symbol = self.symbol(field_name, SymbolType::Field, Scope::Class, decl);
            symbol.full_name = Some(full_name.clone());
            symbol.parent_symbol = Some(struct_name.to_owned());
            symbol.visibility = Some(self.visibility(decl).to_owned());
            symbol.source_text = limits::kept_str(self.text(decl));
            symbol.metadata = object([("field_type", Value::String(field_type.to_owned()))]);
            self.symbols.push(symbol);

            let annotations = object([("member_type", Value::String("field".to_owned()))]);
            self.relate(
                struct_name,
                &full_name,
                RelationshipType::Defines,
                decl,
                annotations,
            );
            self.field_type_edges(struct_name, field_type, decl);
        }
    }

    /// `_extract_tuple_struct_fields`: fields `0`, `1`, … Python quirk: the
    /// visibility is the LIST's first modifier, so one `pub` field makes
    /// every field public; a tuple field has no source text.
    fn tuple_struct_fields(&mut self, struct_name: &str, list: Node<'_>) {
        let visibility = self.visibility(list);
        let mut index = 0usize;
        for child in children(list) {
            if !TUPLE_FIELD_KINDS.contains(&child.kind()) {
                continue;
            }
            let field_name = index.to_string();
            let field_type = self.text(child);
            let full_name = format!("{struct_name}.{field_name}");
            let mut symbol = self.symbol(&field_name, SymbolType::Field, Scope::Class, child);
            symbol.full_name = Some(full_name.clone());
            symbol.parent_symbol = Some(struct_name.to_owned());
            symbol.visibility = Some(visibility.to_owned());
            symbol.metadata = object([
                ("field_type", Value::String(field_type.to_owned())),
                ("is_tuple_field", Value::Bool(true)),
            ]);
            self.symbols.push(symbol);

            let annotations = object([
                ("member_type", Value::String("field".to_owned())),
                ("is_tuple_field", Value::Bool(true)),
            ]);
            self.relate(
                struct_name,
                &full_name,
                RelationshipType::Defines,
                child,
                annotations,
            );
            self.field_type_edges(struct_name, field_type, child);
            index += 1;
        }
    }

    /// `_emit_field_type_relationships`: `aggregation` for a reference,
    /// pointer or wrapper-prefixed type, else `composition`, to the core
    /// type name.
    fn field_type_edges(&mut self, owner: &str, field_type: &str, node: Node<'_>) {
        if field_type.is_empty() {
            return;
        }
        let core = core_type_name(field_type);
        if core.is_empty() || is_builtin_type(&core) {
            return;
        }
        let is_reference = field_type.starts_with('&')
            || field_type.starts_with('*')
            || REFERENCE_WRAPPERS.iter().any(|w| field_type.starts_with(w));
        let rel_type = if is_reference {
            RelationshipType::Aggregation
        } else {
            RelationshipType::Composition
        };
        self.relate(owner, &core, rel_type, node, Map::new());
    }

    fn enum_item(&mut self, node: Node<'_>) {
        let Some(name_node) = child_of_kind(node, "type_identifier") else {
            return;
        };
        let name = self.text(name_node);
        let (derives, attributes) = self.consume_pending();
        let type_params = self.type_parameters(node);

        let mut metadata = Map::new();
        if !type_params.is_empty() {
            metadata.insert("type_parameters".to_owned(), pairs_value(&type_params));
        }
        if !derives.is_empty() {
            metadata.insert("derives".to_owned(), strings(&derives));
        }
        if !attributes.is_empty() {
            metadata.insert("attributes".to_owned(), Value::Array(attributes));
        }
        let mut symbol = self.symbol(name, SymbolType::Enum, Scope::Global, node);
        symbol.full_name = Some(name.to_owned());
        symbol.visibility = Some(self.visibility(node).to_owned());
        symbol.docstring = self.preceding_comment(node);
        symbol.source_text = limits::kept_str(self.text(node));
        symbol.signature = Some(format!("enum {name}{}", generic_suffix(&type_params)));
        symbol.metadata = metadata;
        self.symbols.push(symbol);

        self.derive_edges(name, node, &derives);
        if let Some(list) = child_of_kind(node, "enum_variant_list") {
            for variant in children_of_kind(list, "enum_variant") {
                self.enum_variant(name, variant);
            }
        }
    }

    /// `_handle_enum_variant`: a `field` symbol with no visibility.
    fn enum_variant(&mut self, enum_name: &str, node: Node<'_>) {
        let Some(name_node) = child_of_kind(node, "identifier") else {
            return;
        };
        let variant_name = self.text(name_node);
        let field_list = child_of_kind(node, "field_declaration_list");
        let ordered_list = child_of_kind(node, "ordered_field_declaration_list");
        let kind = if field_list.is_some() {
            "struct"
        } else if ordered_list.is_some() {
            "tuple"
        } else {
            "unit"
        };
        let full_name = format!("{enum_name}.{variant_name}");
        let mut symbol = self.symbol(variant_name, SymbolType::Field, Scope::Class, node);
        symbol.full_name = Some(full_name.clone());
        symbol.parent_symbol = Some(enum_name.to_owned());
        symbol.source_text = limits::kept_str(self.text(node));
        symbol.metadata = object([("variant_kind", Value::String(kind.to_owned()))]);
        self.symbols.push(symbol);

        let annotations = object([
            ("member_type", Value::String("variant".to_owned())),
            ("variant_kind", Value::String(kind.to_owned())),
        ]);
        self.relate(
            enum_name,
            &full_name,
            RelationshipType::Defines,
            node,
            annotations,
        );

        if let Some(list) = field_list {
            for decl in children_of_kind(list, "field_declaration") {
                let field_type = field_type_string(self, decl);
                self.field_type_edges(enum_name, field_type, decl);
            }
        } else if let Some(list) = ordered_list {
            for child in children(list) {
                if VARIANT_TUPLE_KINDS.contains(&child.kind()) {
                    let field_type = self.text(child);
                    self.field_type_edges(enum_name, field_type, child);
                }
            }
        }
    }

    /// `_handle_trait_item`. The derives and attributes are consumed and
    /// dropped. Only `type_identifier` and `scoped_type_identifier`
    /// supertraits give `inheritance` edges (a generic one does not).
    /// Python quirk: an attribute inside the trait body is not handled.
    fn trait_item(&mut self, node: Node<'_>) {
        let Some(name_node) = child_of_kind(node, "type_identifier") else {
            return;
        };
        let name = self.text(name_node);
        let _ = self.consume_pending();
        let type_params = self.type_parameters(node);

        let mut symbol = self.symbol(name, SymbolType::Trait, Scope::Global, node);
        symbol.full_name = Some(name.to_owned());
        symbol.visibility = Some(self.visibility(node).to_owned());
        symbol.docstring = self.preceding_comment(node);
        symbol.source_text = limits::kept_str(self.text(node));
        symbol.signature = Some(format!("trait {name}{}", generic_suffix(&type_params)));
        if !type_params.is_empty() {
            symbol
                .metadata
                .insert("type_parameters".to_owned(), pairs_value(&type_params));
        }
        self.symbols.push(symbol);

        if let Some(bounds) = child_of_kind(node, "trait_bounds") {
            for bound in children(bounds) {
                if matches!(bound.kind(), "type_identifier" | "scoped_type_identifier") {
                    let supertrait = self.text(bound);
                    if !supertrait.is_empty() && !is_builtin_type(supertrait) {
                        let annotations = object([("supertrait", Value::Bool(true))]);
                        self.relate(
                            name,
                            supertrait,
                            RelationshipType::Inheritance,
                            bound,
                            annotations,
                        );
                    }
                }
            }
        }

        if let Some(list) = child_of_kind(node, "declaration_list") {
            for child in children(list) {
                match child.kind() {
                    "function_signature_item" => self.trait_method_signature(name, child),
                    "function_item" => self.function_item(
                        child,
                        MethodContext {
                            parent: Some(name),
                            is_trait_default: true,
                            ..MethodContext::default()
                        },
                    ),
                    "type_item" => self.type_item(child, Some(name)),
                    "const_item" => self.const_item(child, Some(name)),
                    _ => {}
                }
            }
        }
    }

    /// `_handle_trait_method_signature`: an abstract method, always public,
    /// with no docstring. It consumes no pending attributes.
    fn trait_method_signature(&mut self, trait_name: &str, node: Node<'_>) {
        let Some(name_node) =
            child_of_kind(node, "identifier").or_else(|| child_of_kind(node, "name"))
        else {
            return;
        };
        let method_name = self.text(name_node);
        let modifiers = self.function_modifiers(node);
        let return_type = self.return_type(node);
        let full_name = format!("{trait_name}.{method_name}");

        let mut symbol = self.symbol(method_name, SymbolType::Method, Scope::Class, node);
        symbol.full_name = Some(full_name.clone());
        symbol.parent_symbol = Some(trait_name.to_owned());
        symbol.visibility = Some("public".to_owned());
        symbol.is_abstract = true;
        symbol.is_async = modifiers.contains_key("is_async");
        symbol.source_text = limits::kept_str(self.text(node));
        symbol.signature = Some(self.function_signature(method_name, node, return_type));
        symbol.return_type = return_type.map(str::to_owned);
        symbol.parameter_types = self.parameter_types(node);
        let mut metadata = object([("is_abstract", Value::Bool(true))]);
        metadata.extend(modifiers);
        symbol.metadata = metadata;
        self.symbols.push(symbol);

        let annotations = object([
            ("member_type", Value::String("method".to_owned())),
            ("is_abstract", Value::Bool(true)),
        ]);
        self.relate(
            trait_name,
            &full_name,
            RelationshipType::Defines,
            node,
            annotations,
        );
        self.signature_type_references(&full_name, node);
    }

    /// `_handle_impl_item`: not a symbol. Its target is the `type` field's
    /// text cut at the first `<` (`&'a Foo`, `dyn Bar`, `(A, B)` stay as
    /// written); its trait likewise. A trait impl gives one
    /// `implementation` edge; its functions are methods of the target.
    /// Pending attributes are left for the first function to consume.
    fn impl_item(&mut self, node: Node<'_>) {
        let mut target = node
            .child_by_field_name("type")
            .map(|t| cut_generics(self.text(t)).to_owned());
        let mut trait_name = node
            .child_by_field_name("trait")
            .map(|t| cut_generics(self.text(t)).to_owned());

        if target.as_deref().is_none_or(str::is_empty) {
            let type_ids = children_of_kind(node, "type_identifier");
            let has_for = children(node)
                .iter()
                .any(|c| c.kind() == "for" || self.text(*c) == "for");
            if has_for && type_ids.len() >= 2 {
                trait_name = Some(self.text(type_ids[0]).to_owned());
                target = Some(self.text(type_ids[type_ids.len() - 1]).to_owned());
            } else if let Some(last) = type_ids.last() {
                target = Some(self.text(*last).to_owned());
            }
            if target.as_deref().is_none_or(str::is_empty)
                && let Some(last) = children_of_kind(node, "generic_type").last()
            {
                let text = self.text(*last);
                target = Some(text.split('<').next().unwrap_or(text).to_owned());
            }
        }
        let Some(target) = target.filter(|t| !t.is_empty()) else {
            return;
        };
        let trait_name = trait_name.filter(|t| !t.is_empty());

        let saved = self.impl_target.replace(target.clone());
        if let Some(trait_name) = trait_name.as_deref() {
            let annotations = object([
                ("trait_name", Value::String(trait_name.to_owned())),
                ("target", Value::String(target.clone())),
                ("impl_kind", Value::String("trait".to_owned())),
            ]);
            self.relate(
                &target,
                trait_name,
                RelationshipType::Implementation,
                node,
                annotations,
            );
        }
        if let Some(list) = child_of_kind(node, "declaration_list") {
            for child in children(list) {
                match child.kind() {
                    "function_item" => self.function_item(
                        child,
                        MethodContext {
                            parent: Some(&target),
                            impl_kind: Some(if trait_name.is_some() {
                                "trait"
                            } else {
                                "inherent"
                            }),
                            impl_trait: trait_name.as_deref(),
                            is_trait_default: false,
                        },
                    ),
                    "type_item" => self.type_item(child, Some(&target)),
                    "const_item" => self.const_item(child, Some(&target)),
                    "attribute_item" => self.attribute(child),
                    _ => {}
                }
            }
        }
        self.impl_target = saved;
    }

    /// `_handle_function_item`: a free function, or a method when it has a
    /// parent (impl target or trait). A method is always `public`; one
    /// without a `self` parameter is static.
    pub(super) fn function_item(&mut self, node: Node<'_>, context: MethodContext<'_>) {
        let Some(name_node) = child_of_kind(node, "identifier") else {
            return;
        };
        let func_name = self.text(name_node);
        let _ = self.consume_pending();
        let modifiers = self.function_modifiers(node);
        let type_params = self.type_parameters(node);
        let is_method = context.parent.is_some();
        let is_static_method = is_method && !self.has_self_parameter(node);
        let return_type = self.return_type(node);
        let full_name = context
            .parent
            .map_or_else(|| func_name.to_owned(), |p| format!("{p}.{func_name}"));

        let mut metadata = modifiers.clone();
        if !type_params.is_empty() {
            metadata.insert("type_parameters".to_owned(), pairs_value(&type_params));
        }
        if let Some(kind) = context.impl_kind {
            metadata.insert("impl_kind".to_owned(), Value::String(kind.to_owned()));
        }
        if let Some(trait_name) = context.impl_trait {
            metadata.insert(
                "impl_trait".to_owned(),
                Value::String(trait_name.to_owned()),
            );
        }
        if context.is_trait_default {
            metadata.insert("is_default".to_owned(), Value::Bool(true));
        }
        if is_static_method {
            metadata.insert("is_static_method".to_owned(), Value::Bool(true));
        }

        let (symbol_type, scope) = if is_method {
            (SymbolType::Method, Scope::Class)
        } else {
            (SymbolType::Function, Scope::Global)
        };
        let mut symbol = self.symbol(func_name, symbol_type, scope, node);
        symbol.full_name = Some(full_name.clone());
        symbol.parent_symbol = context.parent.map(str::to_owned);
        symbol.visibility = Some(
            if is_method {
                "public"
            } else {
                self.visibility(node)
            }
            .to_owned(),
        );
        symbol.is_static = is_static_method;
        symbol.is_async = modifiers.contains_key("is_async");
        symbol.docstring = self.preceding_comment(node);
        symbol.source_text = limits::kept_str(self.text(node));
        symbol.signature = Some(self.function_signature(func_name, node, return_type));
        symbol.return_type = return_type.map(str::to_owned);
        symbol.parameter_types = self.parameter_types(node);
        symbol.metadata = metadata;
        self.symbols.push(symbol);

        if let Some(parent) = context.parent {
            let impl_kind = match (context.impl_kind, context.is_trait_default) {
                (Some(kind), _) => Value::String(kind.to_owned()),
                (None, true) => Value::String("default".to_owned()),
                (None, false) => Value::Null,
            };
            let annotations = object([
                ("member_type", Value::String("method".to_owned())),
                ("impl_kind", impl_kind),
                ("cross_file", Value::Bool(false)),
            ]);
            self.relate(
                parent,
                &full_name,
                RelationshipType::Defines,
                node,
                annotations,
            );
        }

        self.signature_type_references(&full_name, node);
        if let Some(block) = child_of_kind(node, "block") {
            self.walk_body(block, &full_name);
        }
    }

    /// `_handle_const_item`.
    fn const_item(&mut self, node: Node<'_>, parent: Option<&str>) {
        let Some(name_node) = child_of_kind(node, "identifier") else {
            return;
        };
        let name = self.text(name_node);
        let _ = self.consume_pending();
        let full_name = parent.map_or_else(|| name.to_owned(), |p| format!("{p}.{name}"));
        let scope = if parent.is_some() {
            Scope::Class
        } else {
            Scope::Global
        };
        let mut symbol = self.symbol(name, SymbolType::Constant, scope, node);
        symbol.full_name = Some(full_name.clone());
        symbol.parent_symbol = parent.map(str::to_owned);
        symbol.visibility = Some(self.visibility(node).to_owned());
        symbol.docstring = self.preceding_comment(node);
        symbol.source_text = limits::kept_str(self.text(node));
        self.symbols.push(symbol);

        if let Some(parent) = parent {
            let annotations = object([("member_type", Value::String("constant".to_owned()))]);
            self.relate(
                parent,
                &full_name,
                RelationshipType::Defines,
                node,
                annotations,
            );
        }
    }

    /// `_handle_static_item`: a global `constant` marked static.
    fn static_item(&mut self, node: Node<'_>) {
        let Some(name_node) = child_of_kind(node, "identifier") else {
            return;
        };
        let name = self.text(name_node);
        let _ = self.consume_pending();
        let mut symbol = self.symbol(name, SymbolType::Constant, Scope::Global, node);
        symbol.full_name = Some(name.to_owned());
        symbol.visibility = Some(self.visibility(node).to_owned());
        symbol.is_static = true;
        symbol.docstring = self.preceding_comment(node);
        symbol.source_text = limits::kept_str(self.text(node));
        symbol.metadata = object([("is_static", Value::Bool(true))]);
        self.symbols.push(symbol);
    }

    /// `_handle_type_item`: a `type_alias` symbol, and `alias_of` edges from
    /// the BARE name to the aliased type's core name and, for a generic
    /// target, to each plain type argument that is not one of the alias's
    /// own parameters.
    fn type_item(&mut self, node: Node<'_>, parent: Option<&str>) {
        let Some(name_node) = child_of_kind(node, "type_identifier") else {
            return;
        };
        let name = self.text(name_node);
        let _ = self.consume_pending();
        let full_name = parent.map_or_else(|| name.to_owned(), |p| format!("{p}.{name}"));
        let scope = if parent.is_some() {
            Scope::Class
        } else {
            Scope::Global
        };
        let mut symbol = self.symbol(name, SymbolType::TypeAlias, scope, node);
        symbol.full_name = Some(full_name.clone());
        symbol.parent_symbol = parent.map(str::to_owned);
        symbol.visibility = Some(self.visibility(node).to_owned());
        symbol.docstring = self.preceding_comment(node);
        symbol.source_text = limits::kept_str(self.text(node));
        self.symbols.push(symbol);

        if let Some(parent) = parent {
            let annotations = object([("member_type", Value::String("type".to_owned()))]);
            self.relate(
                parent,
                &full_name,
                RelationshipType::Defines,
                node,
                annotations,
            );
        }

        let all = children(node);
        let Some(&child) = all
            .iter()
            .rev()
            .find(|c| ALIAS_TARGET_KINDS.contains(&c.kind()) && **c != name_node)
        else {
            return;
        };
        let core = core_type_name(self.text(child));
        let core_last = core.rsplit_once("::").map_or(core.as_str(), |(_, r)| r);
        let builtin = is_builtin_type(&core) || is_builtin_type(core_last);
        if !core.is_empty() && !builtin && core != name {
            self.relate(name, &core, RelationshipType::AliasOf, child, Map::new());
        }
        if child.kind() == "generic_type" {
            let mut own_params = Vec::new();
            if let Some(list) = child_of_kind(node, "type_parameters") {
                for param in children_of_kind(list, "type_parameter") {
                    if let Some(ident) = child_of_kind(param, "type_identifier") {
                        own_params.push(self.text(ident));
                    }
                }
            }
            if let Some(args) = child_of_kind(child, "type_arguments") {
                for arg in children_of_kind(args, "type_identifier") {
                    let arg_name = self.text(arg);
                    if !is_builtin_type(arg_name)
                        && arg_name != name
                        && !own_params.contains(&arg_name)
                    {
                        self.relate(name, arg_name, RelationshipType::AliasOf, arg, Map::new());
                    }
                }
            }
        }
    }

    /// `_handle_mod_item`: a `module` symbol; an inline module's items are
    /// visited with the top-level handlers. Python quirk: a nested `mod`,
    /// a `union`, an `extern crate` or a macro call inside an inline module
    /// is skipped, and an unhandled member does not clear pending attributes.
    fn mod_item(&mut self, node: Node<'_>) {
        let Some(name_node) = child_of_kind(node, "identifier") else {
            return;
        };
        let name = self.text(name_node);
        let _ = self.consume_pending();
        let list = child_of_kind(node, "declaration_list");
        let mut symbol = self.symbol(name, SymbolType::Module, Scope::Global, node);
        symbol.full_name = Some(name.to_owned());
        symbol.visibility = Some(self.visibility(node).to_owned());
        symbol.docstring = self.preceding_comment(node);
        symbol.source_text = limits::kept_str(self.text(node));
        symbol.metadata = object([("is_inline", Value::Bool(list.is_some()))]);
        self.symbols.push(symbol);

        let Some(list) = list else {
            return;
        };
        for child in children(list) {
            match child.kind() {
                "struct_item" => self.struct_item(child),
                "enum_item" => self.enum_item(child),
                "trait_item" => self.trait_item(child),
                "impl_item" => self.impl_item(child),
                "function_item" => self.function_item(child, MethodContext::default()),
                "const_item" => self.const_item(child, None),
                "static_item" => self.static_item(child),
                "type_item" => self.type_item(child, None),
                "use_declaration" => self.use_declaration(child),
                "macro_definition" => self.macro_definition(child),
                "attribute_item" => self.attribute(child),
                _ => {}
            }
        }
    }

    /// `_handle_macro_definition`: `macro_rules!`, always public.
    fn macro_definition(&mut self, node: Node<'_>) {
        let Some(name_node) = child_of_kind(node, "identifier") else {
            return;
        };
        let name = self.text(name_node);
        let _ = self.consume_pending();
        let mut symbol = self.symbol(name, SymbolType::Macro, Scope::Global, node);
        symbol.full_name = Some(name.to_owned());
        symbol.visibility = Some("public".to_owned());
        symbol.docstring = self.preceding_comment(node);
        symbol.source_text = limits::kept_str(self.text(node));
        symbol.metadata = object([("is_declarative", Value::Bool(true))]);
        self.symbols.push(symbol);
    }

    /// `_handle_union_item`: a `struct` marked `is_union`, with no derive
    /// edges.
    fn union_item(&mut self, node: Node<'_>) {
        let Some(name_node) = child_of_kind(node, "type_identifier") else {
            return;
        };
        let name = self.text(name_node);
        let (derives, _) = self.consume_pending();
        let mut metadata = object([("is_union", Value::Bool(true))]);
        if !derives.is_empty() {
            metadata.insert("derives".to_owned(), strings(&derives));
        }
        let mut symbol = self.symbol(name, SymbolType::Struct, Scope::Global, node);
        symbol.full_name = Some(name.to_owned());
        symbol.visibility = Some(self.visibility(node).to_owned());
        symbol.docstring = self.preceding_comment(node);
        symbol.source_text = limits::kept_str(self.text(node));
        symbol.signature = Some(format!("union {name}"));
        symbol.metadata = metadata;
        self.symbols.push(symbol);
        if let Some(list) = child_of_kind(node, "field_declaration_list") {
            self.struct_fields(name, list);
        }
    }

    /// `_handle_extern_crate`: the first identifier is the crate, a second
    /// one the alias. Python quirk: `extern crate self as x` has one
    /// identifier, so the "crate" is `x`.
    fn extern_crate(&mut self, node: Node<'_>) {
        let identifiers = children_of_kind(node, "identifier");
        let Some(crate_name) = identifiers.first().map(|n| self.text(*n)) else {
            return;
        };
        let alias = identifiers
            .last()
            .filter(|_| identifiers.len() > 1)
            .map(|n| self.text(*n));
        if crate_name.is_empty() {
            return;
        }
        self.extern_crate_aliases.insert(
            alias
                .filter(|a| !a.is_empty())
                .unwrap_or(crate_name)
                .to_owned(),
            crate_name.to_owned(),
        );
        let annotations = object([
            ("path", Value::String(crate_name.to_owned())),
            (
                "alias",
                alias.map_or(Value::Null, |a| Value::String(a.to_owned())),
            ),
            ("cross_crate", Value::Bool(true)),
            ("extern_crate", Value::Bool(true)),
        ]);
        let stem = self.stem.clone();
        self.relate(
            &stem,
            crate_name,
            RelationshipType::Imports,
            node,
            annotations,
        );
    }

    // ------------------------------------------------------------------
    // `use` declarations
    // ------------------------------------------------------------------

    /// `_handle_use_declaration`: `re_export` only for a plain `pub`.
    fn use_declaration(&mut self, node: Node<'_>) {
        let is_pub = self.visibility(node) == "public";
        self.use_paths(node, "", is_pub);
    }

    fn import_edge(
        &mut self,
        full_path: &str,
        node: Node<'_>,
        extra: &[(&str, Value)],
        is_pub: bool,
    ) {
        let mut annotations = object([("path", Value::String(full_path.to_owned()))]);
        for (key, value) in extra {
            annotations.insert((*key).to_owned(), value.clone());
        }
        annotations.insert("re_export".to_owned(), Value::Bool(is_pub));
        let stem = self.stem.clone();
        self.relate(
            &stem,
            full_path,
            RelationshipType::Imports,
            node,
            annotations,
        );
    }

    /// `_extract_use_paths`, also called on a `use_as_clause` inside a
    /// bracketed list. Python quirks kept:
    ///
    /// * `use a::*;` puts the path INSIDE the `use_wildcard` node, where
    ///   Python does not look, so the target is `::*` (plus any prefix);
    /// * in `{a::b as c}` the recursion records `…a::b` (short name `b`)
    ///   and then `…c`, the alias as if it were a path;
    /// * a bare top-level `use {…}` list skips `a::b` and nested lists.
    fn use_paths(&mut self, node: Node<'_>, prefix: &str, is_pub: bool) {
        let all = children(node);
        for (position, &child) in all.iter().enumerate() {
            match child.kind() {
                "scoped_identifier" => {
                    let path = self.text(child);
                    let full_path = format!("{prefix}{path}");
                    let short = path.rsplit_once("::").map_or(path, |(_, r)| r);
                    self.imports.insert(short.to_owned(), full_path.clone());
                    self.import_edge(&full_path, child, &[], is_pub);
                }
                "use_as_clause" => {
                    let path_node = child_of_kind(child, "scoped_identifier")
                        .or_else(|| child_of_kind(child, "identifier"));
                    let identifiers = children_of_kind(child, "identifier");
                    if let (Some(path_node), Some(alias_node)) = (path_node, identifiers.last()) {
                        let full_path = format!("{prefix}{}", self.text(path_node));
                        let alias = self.text(*alias_node);
                        self.imports.insert(alias.to_owned(), full_path.clone());
                        self.import_edge(
                            &full_path,
                            child,
                            &[("alias", Value::String(alias.to_owned()))],
                            is_pub,
                        );
                    }
                }
                "use_list" => {
                    let mut scope_path = String::new();
                    for sibling in &all[..position] {
                        if matches!(sibling.kind(), "scoped_identifier" | "identifier") {
                            scope_path = format!("{}::", self.text(*sibling));
                        }
                    }
                    for item in children(child) {
                        match item.kind() {
                            "identifier" => {
                                let item_name = self.text(item);
                                let full_path = format!("{prefix}{scope_path}{item_name}");
                                self.imports.insert(item_name.to_owned(), full_path.clone());
                                self.import_edge(&full_path, item, &[], is_pub);
                            }
                            "self" => self.import_self(prefix, &scope_path),
                            "use_as_clause" => {
                                self.use_paths(item, &format!("{prefix}{scope_path}"), is_pub);
                            }
                            _ => {}
                        }
                    }
                }
                "scoped_use_list" => self.scoped_use_list(child, prefix, is_pub),
                "use_wildcard" => {
                    let mut path = prefix.trim_end_matches(':');
                    for sibling in &all[..position] {
                        if matches!(sibling.kind(), "scoped_identifier" | "identifier") {
                            path = self.text(*sibling);
                        }
                    }
                    let glob = format!("{path}::*");
                    self.imports.insert(glob.clone(), glob.clone());
                    self.import_edge(&glob, child, &[("glob", Value::Bool(true))], is_pub);
                }
                "identifier" if position + 1 == all.len() => {
                    let name = self.text(child);
                    if name != "use" && name != "pub" {
                        let full_path = format!("{prefix}{name}");
                        self.imports.insert(name.to_owned(), full_path.clone());
                        self.import_edge(&full_path, child, &[], is_pub);
                    }
                }
                _ => {}
            }
        }
    }

    /// `{self, …}`: the module itself, recorded as an import but with no
    /// edge.
    fn import_self(&mut self, prefix: &str, scope_path: &str) {
        let trimmed = scope_path.trim_end_matches(':');
        let module = trimmed.rsplit_once("::").map_or(trimmed, |(_, r)| r);
        if !scope_path.is_empty() && !module.is_empty() {
            self.imports
                .insert(module.to_owned(), format!("{prefix}{trimmed}"));
        }
    }

    /// `_handle_scoped_use_list`: `a::b::{…}`. Python quirk: inside the
    /// braces only plain names, nested lists, `self` and `*` count — an
    /// `x::Y` or `x as y` item is dropped.
    fn scoped_use_list(&mut self, node: Node<'_>, prefix: &str, is_pub: bool) {
        let mut parts = Vec::new();
        let mut use_list = None;
        for child in children(node) {
            match child.kind() {
                "use_list" => use_list = Some(child),
                "identifier" | "scoped_identifier" | "self" | "crate" | "super" => {
                    parts.push(self.text(child));
                }
                _ => {}
            }
        }
        let scope_path = if parts.is_empty() {
            String::new()
        } else {
            format!("{}::", parts.join("::"))
        };
        let full_prefix = format!("{prefix}{scope_path}");
        let Some(use_list) = use_list else {
            return;
        };
        for item in children(use_list) {
            match item.kind() {
                "identifier" => {
                    let item_name = self.text(item);
                    let full_path = format!("{full_prefix}{item_name}");
                    self.imports.insert(item_name.to_owned(), full_path.clone());
                    self.import_edge(&full_path, item, &[], is_pub);
                }
                "scoped_use_list" => self.scoped_use_list(item, &full_prefix, is_pub),
                "self" => self.import_self(prefix, &scope_path),
                "use_wildcard" => {
                    let glob = format!("{}::*", full_prefix.trim_end_matches(':'));
                    self.imports.insert(glob.clone(), glob.clone());
                    self.import_edge(&glob, item, &[("glob", Value::Bool(true))], is_pub);
                }
                _ => {}
            }
        }
    }
}

// ----------------------------------------------------------------------
// Helpers
// ----------------------------------------------------------------------

/// `_extract_field_type_string`: the LAST child of a type kind.
fn field_type_string<'s>(visitor: &Visitor<'s>, decl: Node<'_>) -> &'s str {
    children(decl)
        .iter()
        .rev()
        .find(|c| FIELD_TYPE_KINDS.contains(&c.kind()))
        .map_or("", |c| visitor.text(*c))
}

/// `<…>` after a type's name in its signature: `T: Bound` or `T`.
fn generic_suffix(params: &[Pair]) -> String {
    if params.is_empty() {
        return String::new();
    }
    let joined = params
        .iter()
        .map(|(n, c)| {
            if c.is_empty() || c == "lifetime" {
                n.clone()
            } else {
                format!("{n}: {c}")
            }
        })
        .collect::<Vec<_>>()
        .join(", ");
    format!("<{joined}>")
}

/// `text[:text.index('<')]`, or the whole text.
fn cut_generics(text: &str) -> &str {
    text.find('<').map_or(text, |i| &text[..i])
}

/// `_extract_core_type_name`: strip `&`, `*`, `mut `, `dyn `, `impl `;
/// unwrap one builtin single-argument generic (`Box<T>` → `T`, recursing);
/// otherwise keep the text before `<`; else the last `::` segment.
///
/// Python quirks kept: `&'a T` stays `'a T` (the lifetime is not stripped),
/// `*const T` becomes `const T`, and a scoped generic keeps its full path
/// (`std::sync::Arc<X>` → `std::sync::Arc`).
pub(super) fn core_type_name(type_str: &str) -> String {
    let mut s = py_strip(type_str);
    while s.starts_with('&') || s.starts_with('*') {
        s = py_strip(s.trim_start_matches(['&', '*']));
        if let Some(rest) = s.strip_prefix("mut ") {
            s = py_strip(rest);
        }
    }
    if let Some(rest) = s.strip_prefix("dyn ") {
        s = py_strip(rest);
    }
    if let Some(rest) = s.strip_prefix("impl ") {
        s = py_strip(rest);
    }
    if let Some(open) = s.find('<') {
        let outer = py_strip(&s[..open]);
        let inner = match s.rfind('>') {
            Some(close) if close > open => py_strip(&s[open + 1..close]),
            _ => "",
        };
        if is_builtin_type(outer) && !inner.is_empty() && !inner.contains(',') {
            return core_type_name(inner);
        }
        return outer.to_owned();
    }
    s.rsplit_once("::").map_or(s, |(_, r)| r).to_owned()
}

/// Python's `str.strip()` with no argument: Unicode whitespace plus the
/// ASCII separators `\x1c`–`\x1f`, which `str.isspace` also counts.
pub(super) fn py_strip(text: &str) -> &str {
    text.trim_matches(|c: char| c.is_whitespace() || ('\x1c'..='\x1f').contains(&c))
}

pub(super) fn children(node: Node<'_>) -> Vec<Node<'_>> {
    let mut cursor = node.walk();
    node.children(&mut cursor).collect()
}

pub(super) fn child_of_kind<'t>(node: Node<'t>, kind: &str) -> Option<Node<'t>> {
    let mut cursor = node.walk();
    node.children(&mut cursor).find(|c| c.kind() == kind)
}

pub(super) fn children_of_kind<'t>(node: Node<'t>, kind: &str) -> Vec<Node<'t>> {
    let mut cursor = node.walk();
    node.children(&mut cursor)
        .filter(|c| c.kind() == kind)
        .collect()
}

/// The first descendant of `kind` in pre-order (`_iter_descendants`).
fn first_descendant_of_kind<'t>(node: Node<'t>, kind: &str) -> Option<Node<'t>> {
    for child in children(node) {
        if child.kind() == kind {
            return Some(child);
        }
        if let Some(found) = first_descendant_of_kind(child, kind) {
            return Some(found);
        }
    }
    None
}

/// `_make_range`: 1-based lines, 0-based byte columns.
pub(super) fn range_of(node: Node<'_>) -> Range {
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

/// A `Range` as Python's dump writes the dataclass inside metadata.
fn range_value(range: Range) -> Value {
    let position = |line: u32, column: u32| {
        object([("line", Value::from(line)), ("column", Value::from(column))])
    };
    Value::Object(object([
        (
            "start",
            Value::Object(position(range.start.line, range.start.column)),
        ),
        (
            "end",
            Value::Object(position(range.end.line, range.end.column)),
        ),
    ]))
}

/// A JSON object from key/value pairs, in order.
pub(super) fn object<const N: usize>(pairs: [(&str, Value); N]) -> Map<String, Value> {
    pairs.into_iter().map(|(k, v)| (k.to_owned(), v)).collect()
}

fn strings(values: &[String]) -> Value {
    Value::Array(values.iter().cloned().map(Value::String).collect())
}

/// Pairs as Python's JSON writes a list of tuples.
fn pairs_value(pairs: &[Pair]) -> Value {
    Value::Array(
        pairs
            .iter()
            .map(|(n, t)| Value::Array(vec![Value::String(n.clone()), Value::String(t.clone())]))
            .collect(),
    )
}

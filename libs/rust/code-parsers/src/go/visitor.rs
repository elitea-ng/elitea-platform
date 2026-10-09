//! One Go file: the Python `GoVisitorParser.parse_file`.
//!
//! Every rule here is the Python visitor's, quirks included, because the
//! graph gate compares this output with the Python output field by field.
//! Each quirk the port keeps on purpose is marked "Python quirk". Node kinds
//! are the `tree-sitter-go` 0.25 grammar's, which is the grammar
//! `tree_sitter_language_pack` 1.16.1 ships (`dot`, `var_spec_list`,
//! `statement_list`, `method_elem` and `type_elem` all exist in both).

use super::{BUILTIN_FUNCTIONS, BUILTIN_TYPES};
use crate::limits;
use crate::model::{ParseResult, Range, Relationship, RelationshipType, Scope, Symbol, SymbolType};
use indexmap::{IndexMap, IndexSet};
use serde_json::{Map, Value};
use std::collections::HashSet;
use std::fmt::Write as _;
use tree_sitter::Node;

/// Type node kinds a parameter or result declaration may hold.
const PARAM_TYPE_KINDS: &[&str] = &[
    "type_identifier",
    "pointer_type",
    "qualified_type",
    "slice_type",
    "map_type",
    "channel_type",
    "array_type",
    "function_type",
    "interface_type",
    "struct_type",
];

/// What a method's simple (unparenthesised) result may be. Python quirk:
/// `function_type`, `interface_type`, `struct_type` and `generic_type` are
/// not in the list, so such results give no return type.
const SIMPLE_RESULT_KINDS: &[&str] = &[
    "type_identifier",
    "pointer_type",
    "qualified_type",
    "slice_type",
    "map_type",
    "channel_type",
    "array_type",
];

/// What an interface method's simple result may be.
const METHOD_ELEM_RESULT_KINDS: &[&str] = &[
    "type_identifier",
    "pointer_type",
    "qualified_type",
    "slice_type",
    "map_type",
    "channel_type",
    "array_type",
    "function_type",
];

/// The underlying type of a type alias (`type A = B`).
const ALIAS_TARGET_KINDS: &[&str] = &[
    "type_identifier",
    "qualified_type",
    "pointer_type",
    "slice_type",
    "map_type",
];

/// The underlying type of a named type (`type A B`) after its name.
const NAMED_UNDERLYING_KINDS: &[&str] = &[
    "qualified_type",
    "pointer_type",
    "slice_type",
    "map_type",
    "struct_type",
    "interface_type",
    "function_type",
    "channel_type",
    "array_type",
];

/// A `(name, type)` pair, as Python's parameter tuples.
type Pair = (String, String);

/// Parse one Go source (already decoded as Python decodes it) into the
/// per-file result. Cross-file linking happens later, in `super`.
pub(super) fn parse_source(file_path: &str, source: &str) -> ParseResult {
    let mut parser = tree_sitter::Parser::new();
    let language: tree_sitter::Language = tree_sitter_go::LANGUAGE.into();
    if parser.set_language(&language).is_err() {
        return failed(file_path, "Tree-sitter Go parser not available");
    }
    let Some(tree) = parser.parse(source, None) else {
        return failed(file_path, "Tree-sitter Go parser returned no tree");
    };
    if limits::too_deep(tree.root_node()) {
        return failed(file_path, limits::RECURSION_ERROR);
    }
    let mut visitor = Visitor::new(file_path, source);
    visitor.visit_top_level(tree.root_node());
    visitor.link_same_file_methods();
    ParseResult {
        imports: visitor.imports.values().cloned().collect(),
        symbols: visitor.symbols,
        relationships: visitor.relationships,
        ..ParseResult::new(file_path, "go")
    }
}

/// A result that carries only an error, as every Python failure path builds.
pub(super) fn failed(file_path: &str, error: impl Into<String>) -> ParseResult {
    let mut result = ParseResult::new(file_path, "go");
    result.errors.push(error.into());
    result
}

/// Per-file visitor state (the Python parser's `_current_*` / `_file_*`).
struct Visitor<'s> {
    file: &'s str,
    source: &'s str,
    /// `Path(file).stem`: the source of every `imports` edge.
    stem: String,
    symbols: Vec<Symbol>,
    relationships: Vec<Relationship>,
    /// Short name → import path. A later import with the same short name
    /// replaces the value in place, as a Python dict assignment does.
    imports: IndexMap<String, String>,
    /// Receiver type → method names, in first-seen order.
    method_receivers: IndexMap<String, IndexSet<String>>,
    /// Types (struct, interface, enum group, alias) this file defines.
    type_names: HashSet<String>,
    init_counter: u32,
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
            method_receivers: IndexMap::new(),
            type_names: HashSet::new(),
            init_counter: 0,
        }
    }

    fn text(&self, node: Node<'_>) -> &'s str {
        self.source.get(node.byte_range()).unwrap_or("")
    }

    fn text_of(&self, node: Option<Node<'_>>) -> &'s str {
        node.map_or("", |n| self.text(n))
    }

    fn symbol(&self, name: &str, symbol_type: SymbolType, scope: Scope, node: Node<'_>) -> Symbol {
        Symbol::new(name, symbol_type, scope, range_of(node), self.file)
    }

    /// `_make_relationship`: the target file is always this file, and a
    /// relationship without a node sits at `1:0-1:0`.
    fn relate(
        &mut self,
        source: &str,
        target: &str,
        rel_type: RelationshipType,
        node: Option<Node<'_>>,
        annotations: Map<String, Value>,
    ) {
        let mut relationship = Relationship::new(source, target, rel_type, self.file);
        relationship.target_file = Some(self.file.to_owned());
        relationship.source_range = Some(node.map_or(Range::new(1, 0, 1, 0), range_of));
        relationship.annotations = annotations;
        self.relationships.push(relationship);
    }

    fn visit_top_level(&mut self, root: Node<'_>) {
        for child in children(root) {
            match child.kind() {
                "import_declaration" => self.import_declaration(child),
                "type_declaration" => self.type_declaration(child),
                "function_declaration" => self.function_declaration(child),
                "method_declaration" => self.method_declaration(child),
                "const_declaration" => self.const_declaration(child),
                "var_declaration" => self.var_declaration(child),
                // `package_clause` only sets a package name nothing reads.
                _ => {}
            }
        }
    }

    /// `_get_preceding_comment`: the previous named sibling, if a comment.
    fn preceding_comment(&self, node: Node<'_>) -> Option<String> {
        let previous = node.prev_named_sibling()?;
        if previous.kind() != "comment" {
            return None;
        }
        let text = self.text(previous);
        if let Some(rest) = text.strip_prefix("//") {
            return Some(py_strip(rest).to_owned());
        }
        if text.starts_with("/*") && text.ends_with("*/") && text.len() >= 4 {
            return Some(py_strip(&text[2..text.len() - 2]).to_owned());
        }
        None
    }

    // ---- imports -----------------------------------------------------

    fn import_declaration(&mut self, node: Node<'_>) {
        if let Some(list) = child_of_kind(node, "import_spec_list") {
            for spec in children_of_kind(list, "import_spec") {
                self.import_spec(spec);
            }
        } else if let Some(spec) = child_of_kind(node, "import_spec") {
            self.import_spec(spec);
        }
    }

    fn import_spec(&mut self, node: Node<'_>) {
        // Python quirk: a raw-string import path (`import \`fmt\``) is skipped.
        let Some(path_node) = child_of_kind(node, "interpreted_string_literal") else {
            return;
        };
        let import_path = self.text(path_node).trim_matches('"').to_owned();
        let alias = child_of_kind(node, "package_identifier");
        // Python quirk: the grammar wraps `.` in a `dot` node, so this never
        // matches and a dot import is recorded under its last path segment.
        let dot = child_of_kind(node, ".");
        let blank = child_of_kind(node, "blank_identifier");
        let short_name = if let Some(alias) = alias {
            self.text(alias).to_owned()
        } else if dot.is_some() {
            ".".to_owned()
        } else if blank.is_some() {
            "_".to_owned()
        } else {
            import_path
                .rsplit_once('/')
                .map_or(import_path.as_str(), |(_, last)| last)
                .to_owned()
        };
        self.imports.insert(short_name.clone(), import_path.clone());
        let mut annotations = Map::new();
        annotations.insert(
            "alias".to_owned(),
            if alias.is_some() {
                Value::String(short_name)
            } else {
                Value::Null
            },
        );
        annotations.insert("is_dot".to_owned(), Value::Bool(dot.is_some()));
        annotations.insert("is_blank".to_owned(), Value::Bool(blank.is_some()));
        let stem = self.stem.clone();
        self.relate(
            &stem,
            &import_path,
            RelationshipType::Imports,
            Some(node),
            annotations,
        );
    }

    // ---- type declarations ------------------------------------------

    fn type_declaration(&mut self, node: Node<'_>) {
        for child in children(node) {
            match child.kind() {
                "type_spec" => self.type_spec(child, node),
                "type_alias" => self.type_alias(child, node),
                _ => {}
            }
        }
    }

    fn type_spec(&mut self, spec: Node<'_>, decl: Node<'_>) {
        let Some(name_node) = child_of_kind(spec, "type_identifier") else {
            return;
        };
        let name = self.text(name_node);
        if let Some(struct_node) = child_of_kind(spec, "struct_type") {
            self.struct_type(name, spec, struct_node, decl);
        } else if let Some(iface_node) = child_of_kind(spec, "interface_type") {
            self.interface_type(name, spec, iface_node, decl);
        } else {
            self.named_type(name, spec, decl);
        }
    }

    /// The STRUCT / INTERFACE symbol both share. Python quirk: the range,
    /// source text and docstring are the whole `type_declaration`'s, so every
    /// spec of a grouped `type ( … )` carries the group's.
    fn type_symbol(
        &mut self,
        name: &str,
        symbol_type: SymbolType,
        keyword: &str,
        spec: Node<'_>,
        decl: Node<'_>,
    ) {
        let type_params = self.type_parameters(spec);
        let mut signature = format!("type {name}");
        if !type_params.is_empty() {
            let _ = write!(signature, "[{}]", join_pairs(&type_params));
        }
        signature.push(' ');
        signature.push_str(keyword);
        let mut symbol = self.symbol(name, symbol_type, Scope::Global, decl);
        symbol.full_name = Some(name.to_owned());
        symbol.visibility = Some(visibility(name).to_owned());
        symbol.docstring = self.preceding_comment(decl);
        symbol.source_text = limits::kept_str(self.text(decl));
        symbol.signature = Some(signature);
        if !type_params.is_empty() {
            symbol
                .metadata
                .insert("type_parameters".to_owned(), pairs_value(&type_params));
        }
        self.symbols.push(symbol);
        self.type_names.insert(name.to_owned());
    }

    fn struct_type(&mut self, name: &str, spec: Node<'_>, struct_node: Node<'_>, decl: Node<'_>) {
        self.type_symbol(name, SymbolType::Struct, "struct", spec, decl);
        if let Some(list) = child_of_kind(struct_node, "field_declaration_list") {
            for field in children_of_kind(list, "field_declaration") {
                self.struct_field(name, field);
            }
        }
    }

    fn struct_field(&mut self, struct_name: &str, field: Node<'_>) {
        // Python quirk: `a, b int` records only `a`.
        if let Some(field_id) = child_of_kind(field, "field_identifier") {
            let field_name = self.text(field_id);
            let field_type = children(field)
                .into_iter()
                .find(|c| PARAM_TYPE_KINDS.contains(&c.kind()))
                .map_or("", |c| self.text(c));
            let is_pointer = child_of_kind(field, "pointer_type").is_some();
            let tag = child_of_kind(field, "raw_string_literal")
                .or_else(|| child_of_kind(field, "interpreted_string_literal"))
                .map(|t| self.text(t).trim_matches(['`', '"']).to_owned());
            let full_name = format!("{struct_name}.{field_name}");
            let mut symbol = self.symbol(field_name, SymbolType::Field, Scope::Class, field);
            symbol.parent_symbol = Some(struct_name.to_owned());
            symbol.full_name = Some(full_name.clone());
            symbol.visibility = Some(visibility(field_name).to_owned());
            symbol.source_text = limits::kept_str(self.text(field));
            symbol.metadata.insert(
                "field_type".to_owned(),
                Value::String(field_type.to_owned()),
            );
            symbol
                .metadata
                .insert("is_pointer".to_owned(), Value::Bool(is_pointer));
            symbol.metadata.insert(
                "struct_tag".to_owned(),
                tag.map_or(Value::Null, Value::String),
            );
            self.symbols.push(symbol);
            self.relate(
                struct_name,
                &full_name,
                RelationshipType::Defines,
                Some(field),
                member_type("field"),
            );
            let base = strip_pointer_slice(field_type);
            if !base.is_empty() && !BUILTIN_TYPES.contains(&base) {
                let rel_type = if is_pointer {
                    RelationshipType::Aggregation
                } else {
                    RelationshipType::Composition
                };
                self.relate(struct_name, base, rel_type, Some(field), Map::new());
            }
            return;
        }
        let type_node = child_of_kind(field, "type_identifier")
            .or_else(|| child_of_kind(field, "qualified_type"))
            .or_else(|| child_of_kind(field, "pointer_type"));
        let Some(embed) = type_node.and_then(|t| self.embedded_name(t)) else {
            return;
        };
        let full_name = format!("{struct_name}.{embed}");
        let mut symbol = self.symbol(embed, SymbolType::Field, Scope::Class, field);
        symbol.parent_symbol = Some(struct_name.to_owned());
        symbol.full_name = Some(full_name.clone());
        symbol.visibility = Some(visibility(embed).to_owned());
        symbol.source_text = limits::kept_str(self.text(field));
        symbol
            .metadata
            .insert("is_embedded".to_owned(), Value::Bool(true));
        self.symbols.push(symbol);
        let mut embedding = Map::new();
        embedding.insert("is_embedding".to_owned(), Value::Bool(true));
        self.relate(
            struct_name,
            embed,
            RelationshipType::Composition,
            Some(field),
            embedding,
        );
        self.relate(
            struct_name,
            &full_name,
            RelationshipType::Defines,
            Some(field),
            member_type("embedded_field"),
        );
    }

    /// `_extract_embedded_name`: `io.Reader` and `*Base` embed as `Reader`
    /// and `Base`.
    fn embedded_name(&self, type_node: Node<'_>) -> Option<&'s str> {
        match type_node.kind() {
            "type_identifier" => Some(self.text(type_node)),
            "qualified_type" | "pointer_type" => {
                child_of_kind(type_node, "type_identifier").map(|t| self.text(t))
            }
            _ => None,
        }
    }

    fn interface_type(&mut self, name: &str, spec: Node<'_>, iface: Node<'_>, decl: Node<'_>) {
        self.type_symbol(name, SymbolType::Interface, "interface", spec, decl);
        for child in children(iface) {
            match child.kind() {
                "method_elem" => self.interface_method(name, child),
                "type_elem" => {
                    let qualified = child_of_kind(child, "qualified_type");
                    let type_id = child_of_kind(child, "type_identifier");
                    let Some(embedded) = qualified.or(type_id).map(|n| self.text(n)) else {
                        continue;
                    };
                    let mut annotations = Map::new();
                    annotations.insert("is_interface_embedding".to_owned(), Value::Bool(true));
                    self.relate(
                        name,
                        embedded,
                        RelationshipType::Inheritance,
                        Some(child),
                        annotations,
                    );
                }
                _ => {}
            }
        }
    }

    fn interface_method(&mut self, iface_name: &str, elem: Node<'_>) {
        let Some(name_node) = child_of_kind(elem, "field_identifier") else {
            return;
        };
        let method_name = self.text(name_node);
        let params = self.parameters(child_of_kind(elem, "parameter_list"));
        // Python quirk: a parenthesised result list is skipped, so only a
        // simple result type is recorded.
        let mut past_params = false;
        let mut return_types = Vec::new();
        for child in children(elem) {
            if child.kind() == "parameter_list" {
                past_params = true;
            } else if past_params && METHOD_ELEM_RESULT_KINDS.contains(&child.kind()) {
                return_types.push(self.text(child).to_owned());
            }
        }
        let mut signature = format!("{method_name}({})", join_pairs(&params));
        if !return_types.is_empty() {
            signature.push(' ');
            signature.push_str(&return_types.join(" "));
        }
        let full_name = format!("{iface_name}.{method_name}");
        let mut symbol = self.symbol(method_name, SymbolType::Method, Scope::Function, elem);
        symbol.parent_symbol = Some(iface_name.to_owned());
        symbol.full_name = Some(full_name.clone());
        symbol.visibility = Some(visibility(method_name).to_owned());
        symbol.is_abstract = true;
        symbol.source_text = limits::kept_str(self.text(elem));
        symbol.signature = Some(signature);
        symbol.parameter_types = params.into_iter().map(|(_, t)| t).collect();
        symbol.return_type = joined_returns(&return_types);
        symbol
            .metadata
            .insert("is_abstract".to_owned(), Value::Bool(true));
        self.symbols.push(symbol);
        self.relate(
            iface_name,
            &full_name,
            RelationshipType::Defines,
            Some(elem),
            member_type("interface_method"),
        );
    }

    fn type_alias(&mut self, alias: Node<'_>, decl: Node<'_>) {
        let Some(name_node) = child_of_kind(alias, "type_identifier") else {
            return;
        };
        let name = self.text(name_node);
        let mut found_eq = false;
        let mut target = None;
        for child in children(alias) {
            if child.kind() == "=" {
                found_eq = true;
            } else if found_eq && ALIAS_TARGET_KINDS.contains(&child.kind()) {
                target = Some(self.text(child));
                break;
            }
        }
        let mut symbol = self.symbol(name, SymbolType::TypeAlias, Scope::Global, decl);
        symbol.full_name = Some(name.to_owned());
        symbol.visibility = Some(visibility(name).to_owned());
        symbol.docstring = self.preceding_comment(decl);
        symbol.source_text = limits::kept_str(self.text(decl));
        symbol.signature = Some(format!("type {name} = {}", target.unwrap_or("?")));
        symbol.metadata.insert(
            "target_type".to_owned(),
            target.map_or(Value::Null, |t| Value::String(t.to_owned())),
        );
        symbol
            .metadata
            .insert("is_true_alias".to_owned(), Value::Bool(true));
        self.symbols.push(symbol);
        self.type_names.insert(name.to_owned());
        if let Some(target) = target.filter(|t| !BUILTIN_TYPES.contains(t)) {
            self.relate(
                name,
                target,
                RelationshipType::AliasOf,
                Some(alias),
                Map::new(),
            );
        }
    }

    fn named_type(&mut self, name: &str, spec: Node<'_>, decl: Node<'_>) {
        let mut underlying = None;
        let mut name_found = false;
        for child in children(spec) {
            if child.kind() == "type_identifier" {
                if name_found {
                    underlying = Some(self.text(child));
                    break;
                }
                name_found = true;
            } else if name_found && NAMED_UNDERLYING_KINDS.contains(&child.kind()) {
                underlying = Some(self.text(child));
                break;
            }
        }
        let mut symbol = self.symbol(name, SymbolType::TypeAlias, Scope::Global, decl);
        symbol.full_name = Some(name.to_owned());
        symbol.visibility = Some(visibility(name).to_owned());
        symbol.docstring = self.preceding_comment(decl);
        symbol.source_text = limits::kept_str(self.text(decl));
        symbol.signature = Some(format!("type {name} {}", underlying.unwrap_or("?")));
        symbol.metadata.insert(
            "target_type".to_owned(),
            underlying.map_or(Value::Null, |t| Value::String(t.to_owned())),
        );
        symbol
            .metadata
            .insert("is_true_alias".to_owned(), Value::Bool(false));
        self.symbols.push(symbol);
        self.type_names.insert(name.to_owned());
        if let Some(underlying) = underlying.filter(|t| !BUILTIN_TYPES.contains(t)) {
            self.relate(
                name,
                underlying,
                RelationshipType::AliasOf,
                Some(spec),
                Map::new(),
            );
        }
    }

    // ---- functions and methods --------------------------------------

    fn function_declaration(&mut self, node: Node<'_>) {
        let Some(name_node) = child_of_kind(node, "identifier") else {
            return;
        };
        let mut name = self.text(name_node).to_owned();
        if name == "init" {
            self.init_counter += 1;
            if self.init_counter > 1 {
                name = format!("init_L{}", node.start_position().row + 1);
            }
        }
        let type_params = self.type_parameters(node);
        let param_lists = children_of_kind(node, "parameter_list");
        let params = self.parameters(param_lists.first().copied());
        // Python quirk: only a parenthesised result list is read; `func f()
        // error` has no return type.
        let return_types = self.result_types(&param_lists, 1);
        let mut signature = format!("func {name}");
        if !type_params.is_empty() {
            let _ = write!(signature, "[{}]", join_pairs(&type_params));
        }
        let _ = write!(signature, "({})", join_pairs(&params));
        push_results(&mut signature, &return_types);
        let mut symbol = self.symbol(&name, SymbolType::Function, Scope::Global, node);
        symbol.full_name = Some(name.clone());
        symbol.visibility = Some(visibility(&name).to_owned());
        symbol.docstring = self.preceding_comment(node);
        symbol.source_text = limits::kept_str(self.text(node));
        symbol.signature = Some(signature);
        symbol.parameter_types = params.into_iter().map(|(_, t)| t).collect();
        symbol.return_type = joined_returns(&return_types);
        if !type_params.is_empty() {
            symbol
                .metadata
                .insert("type_parameters".to_owned(), pairs_value(&type_params));
        }
        self.symbols.push(symbol);
        if let Some(body) = child_of_kind(node, "block") {
            self.walk_body(body, &name);
        }
    }

    fn method_declaration(&mut self, node: Node<'_>) {
        let Some(name_node) = child_of_kind(node, "field_identifier") else {
            return;
        };
        let method_name = self.text(name_node);
        // Python quirk: a generic receiver (`*Stack[T]`) has no bare
        // `type_identifier`, so the whole method is dropped.
        let Some((receiver, is_pointer)) = self.receiver(node) else {
            return;
        };
        let param_lists = children_of_kind(node, "parameter_list");
        let params = self.parameters(param_lists.get(1).copied());
        let mut return_types = self.result_types(&param_lists, 2);
        if return_types.is_empty()
            && let Some(simple) = children(node)
                .into_iter()
                .find(|c| SIMPLE_RESULT_KINDS.contains(&c.kind()))
        {
            return_types.push(self.text(simple).to_owned());
        }
        let full_name = format!("{receiver}.{method_name}");
        let mut signature = format!("func ({receiver}) {method_name}({})", join_pairs(&params));
        push_results(&mut signature, &return_types);
        let mut symbol = self.symbol(method_name, SymbolType::Method, Scope::Function, node);
        symbol.parent_symbol = Some(receiver.to_owned());
        symbol.full_name = Some(full_name.clone());
        symbol.visibility = Some(visibility(method_name).to_owned());
        symbol.docstring = self.preceding_comment(node);
        symbol.source_text = limits::kept_str(self.text(node));
        symbol.signature = Some(signature);
        symbol.parameter_types = params.into_iter().map(|(_, t)| t).collect();
        symbol.return_type = joined_returns(&return_types);
        symbol
            .metadata
            .insert("is_pointer_receiver".to_owned(), Value::Bool(is_pointer));
        symbol.metadata.insert(
            "receiver_type".to_owned(),
            Value::String(receiver.to_owned()),
        );
        self.symbols.push(symbol);
        self.method_receivers
            .entry(receiver.to_owned())
            .or_default()
            .insert(method_name.to_owned());
        if let Some(body) = child_of_kind(node, "block") {
            self.walk_body(body, &full_name);
        }
    }

    /// `_extract_receiver_info`: the receiver's type name and whether it is
    /// a pointer.
    fn receiver(&self, method: Node<'_>) -> Option<(&'s str, bool)> {
        let list = child_of_kind(method, "parameter_list")?;
        let decl = child_of_kind(list, "parameter_declaration")?;
        if let Some(pointer) = child_of_kind(decl, "pointer_type") {
            return child_of_kind(pointer, "type_identifier").map(|t| (self.text(t), true));
        }
        child_of_kind(decl, "type_identifier").map(|t| (self.text(t), false))
    }

    /// `_extract_parameters_from_list`. Python quirks kept: an unnamed
    /// parameter has the empty name (so the signature shows `" int"`), a
    /// parameter whose type kind is not listed (e.g. `generic_type`) has the
    /// empty type, and variadic parameters come after all the others.
    fn parameters(&self, list: Option<Node<'_>>) -> Vec<Pair> {
        let Some(list) = list else {
            return Vec::new();
        };
        let mut params = Vec::new();
        for decl in children_of_kind(list, "parameter_declaration") {
            let mut names = Vec::new();
            let mut type_str: Option<&str> = None;
            for child in children(decl) {
                let kind = child.kind();
                if kind == "identifier" {
                    names.push(self.text(child));
                } else if PARAM_TYPE_KINDS.contains(&kind)
                    || kind == "variadic_parameter_declaration"
                    || kind == "variadic_argument"
                {
                    type_str = Some(self.text(child));
                }
            }
            match (names.is_empty(), type_str.filter(|t| !t.is_empty())) {
                (false, Some(t)) => {
                    params.extend(names.iter().map(|n| ((*n).to_owned(), t.to_owned())));
                }
                (true, Some(t)) => params.push((String::new(), t.to_owned())),
                (false, None) => {
                    params.extend(names.iter().map(|n| ((*n).to_owned(), String::new())));
                }
                (true, None) => {}
            }
        }
        for variadic in children_of_kind(list, "variadic_parameter_declaration") {
            let name = self.text_of(child_of_kind(variadic, "identifier"));
            params.push((name.to_owned(), self.text(variadic).to_owned()));
        }
        params
    }

    /// `_extract_return_types_from_param_lists`: the type of every
    /// declaration in the parameter list at `skip`.
    fn result_types(&self, lists: &[Node<'_>], skip: usize) -> Vec<String> {
        let Some(list) = lists.get(skip) else {
            return Vec::new();
        };
        let mut types = Vec::new();
        for decl in children_of_kind(*list, "parameter_declaration") {
            for child in children(decl) {
                if PARAM_TYPE_KINDS.contains(&child.kind()) {
                    types.push(self.text(child).to_owned());
                }
            }
        }
        types
    }

    /// `_extract_type_parameters`. Python quirk: `[K, V any]` records only
    /// `K`.
    fn type_parameters(&self, node: Node<'_>) -> Vec<Pair> {
        let Some(list) = child_of_kind(node, "type_parameter_list") else {
            return Vec::new();
        };
        children_of_kind(list, "type_parameter_declaration")
            .into_iter()
            .map(|decl| {
                let name = self.text_of(child_of_kind(decl, "identifier"));
                let constraint = child_of_kind(decl, "type_constraint").map_or("", |c| {
                    child_of_kind(c, "type_identifier")
                        .map_or_else(|| self.text(c), |t| self.text(t))
                });
                (name.to_owned(), constraint.to_owned())
            })
            .collect()
    }

    // ---- const and var ----------------------------------------------

    fn const_declaration(&mut self, node: Node<'_>) {
        let specs = children_of_kind(node, "const_spec");
        let Some(first) = specs.first().copied() else {
            return;
        };
        // Python quirk: only a bare `iota` in the FIRST spec counts;
        // `iota + 1` or `1 << iota` does not.
        let has_iota = child_of_kind(first, "expression_list")
            .is_some_and(|list| children(list).iter().any(|c| c.kind() == "iota"));
        let iota_type = if has_iota {
            child_of_kind(first, "type_identifier").map(|t| self.text(t))
        } else {
            None
        };
        let is_group = has_iota && specs.len() > 1;
        let mut enum_name = String::new();
        if is_group {
            enum_name = iota_type.map_or_else(
                || format!("{}_group", self.text_of(child_of_kind(first, "identifier"))),
                str::to_owned,
            );
            let mut symbol = self.symbol(&enum_name, SymbolType::Enum, Scope::Global, node);
            symbol.full_name = Some(enum_name.clone());
            symbol.visibility = Some(visibility(&enum_name).to_owned());
            symbol.docstring = self.preceding_comment(node);
            symbol.source_text = limits::kept_str(self.text(node));
            symbol.signature = Some(format!("const ({enum_name} = iota ...)"));
            symbol
                .metadata
                .insert("is_iota_group".to_owned(), Value::Bool(true));
            self.symbols.push(symbol);
            self.type_names.insert(enum_name.clone());
        }
        for spec in specs {
            let Some(id) = child_of_kind(spec, "identifier") else {
                continue;
            };
            let name = self.text(id);
            let mut symbol = self.symbol(name, SymbolType::Constant, Scope::Global, spec);
            symbol.full_name = Some(name.to_owned());
            symbol.visibility = Some(visibility(name).to_owned());
            symbol.source_text = limits::kept_str(self.text(spec));
            symbol.metadata.insert(
                "iota_type".to_owned(),
                iota_type.map_or(Value::Null, |t| Value::String(t.to_owned())),
            );
            symbol
                .metadata
                .insert("is_iota".to_owned(), Value::Bool(has_iota));
            self.symbols.push(symbol);
            if is_group {
                self.relate(
                    &enum_name,
                    name,
                    RelationshipType::Defines,
                    Some(spec),
                    member_type("enum_value"),
                );
            }
        }
    }

    /// Python quirk: only `var_spec` children directly under the declaration
    /// are read, so a grouped `var ( … )` (a `var_spec_list`) yields nothing,
    /// and `var a, b int` yields only `a`.
    fn var_declaration(&mut self, node: Node<'_>) {
        for spec in children_of_kind(node, "var_spec") {
            let Some(id) = child_of_kind(spec, "identifier") else {
                continue;
            };
            let name = self.text(id);
            let mut symbol = self.symbol(name, SymbolType::Variable, Scope::Global, spec);
            symbol.full_name = Some(name.to_owned());
            symbol.visibility = Some(visibility(name).to_owned());
            symbol.source_text = limits::kept_str(self.text(spec));
            self.symbols.push(symbol);
        }
    }

    // ---- bodies -------------------------------------------------------

    /// `_walk_body`, iterative so a deeply nested body cannot overflow the
    /// stack; the visiting order is Python's pre-order.
    ///
    /// Python quirk: the call directly under `go` / `defer` is recorded but
    /// not descended into, so `defer f(g())` records `f` and not `g`.
    fn walk_body(&mut self, body: Node<'_>, source: &str) {
        enum Step<'t> {
            Walk(Node<'t>),
            Call(Node<'t>, &'static str),
        }
        let mut stack = vec![Step::Walk(body)];
        while let Some(step) = stack.pop() {
            let node = match step {
                Step::Call(node, flag) => {
                    let mut annotations = Map::new();
                    annotations.insert(flag.to_owned(), Value::Bool(true));
                    self.call_expression(node, source, annotations);
                    continue;
                }
                Step::Walk(node) => node,
            };
            let flag = match node.kind() {
                "call_expression" => {
                    self.call_expression(node, source, Map::new());
                    None
                }
                "composite_literal" => {
                    self.composite_literal(node, source);
                    None
                }
                "type_assertion_expression" => {
                    self.type_assertion(node, source);
                    None
                }
                "go_statement" => Some("is_goroutine"),
                "defer_statement" => Some("is_deferred"),
                _ => None,
            };
            for child in children(node).into_iter().rev() {
                stack.push(match flag {
                    Some(flag) if child.kind() == "call_expression" => Step::Call(child, flag),
                    _ => Step::Walk(child),
                });
            }
        }
    }

    fn call_expression(
        &mut self,
        node: Node<'_>,
        source: &str,
        mut annotations: Map<String, Value>,
    ) {
        let Some(func) = children(node).into_iter().next() else {
            return;
        };
        let target = match func.kind() {
            "identifier" => self.text(func).to_owned(),
            "selector_expression" => {
                let left = children(func).into_iter().next();
                let right = child_of_kind(func, "field_identifier");
                let (Some(left), Some(right)) = (left, right) else {
                    return;
                };
                let left = self.text(left);
                let flag = if self.imports.contains_key(left) {
                    "is_package_call"
                } else {
                    "is_method_call"
                };
                annotations.insert(flag.to_owned(), Value::Bool(true));
                format!("{left}.{}", self.text(right))
            }
            _ => return,
        };
        let base = target.rsplit('.').next().unwrap_or(&target);
        if target.is_empty()
            || BUILTIN_FUNCTIONS.contains(&target.as_str())
            || BUILTIN_FUNCTIONS.contains(&base)
        {
            return;
        }
        self.relate(
            source,
            &target,
            RelationshipType::Calls,
            Some(node),
            annotations,
        );
    }

    fn composite_literal(&mut self, node: Node<'_>, source: &str) {
        let Some(type_node) = children(node).into_iter().next() else {
            return;
        };
        if !matches!(type_node.kind(), "type_identifier" | "qualified_type") {
            return;
        }
        let target = self.text(type_node);
        if !target.is_empty() && !BUILTIN_TYPES.contains(&target) {
            self.relate(
                source,
                target,
                RelationshipType::Creates,
                Some(node),
                Map::new(),
            );
        }
    }

    fn type_assertion(&mut self, node: Node<'_>, source: &str) {
        let Some(type_id) = child_of_kind(node, "type_identifier") else {
            return;
        };
        let target = self.text(type_id);
        if !target.is_empty() && !BUILTIN_TYPES.contains(&target) {
            self.relate(
                source,
                target,
                RelationshipType::References,
                Some(node),
                Map::new(),
            );
        }
    }

    /// `_link_same_file_methods`: a `defines` edge from a type to each of
    /// its methods declared in this file. These have no node, so they sit at
    /// `1:0`.
    fn link_same_file_methods(&mut self) {
        let receivers = std::mem::take(&mut self.method_receivers);
        for (receiver, methods) in &receivers {
            if !self.type_names.contains(receiver) {
                continue;
            }
            for method in methods {
                let mut annotations = member_type("method");
                annotations.insert("cross_file".to_owned(), Value::Bool(false));
                self.relate(
                    receiver,
                    &format!("{receiver}.{method}"),
                    RelationshipType::Defines,
                    None,
                    annotations,
                );
            }
        }
        self.method_receivers = receivers;
    }
}

// ---- free helpers -------------------------------------------------------

fn children(node: Node<'_>) -> Vec<Node<'_>> {
    let mut cursor = node.walk();
    node.children(&mut cursor).collect()
}

fn child_of_kind<'t>(node: Node<'t>, kind: &str) -> Option<Node<'t>> {
    let mut cursor = node.walk();
    node.children(&mut cursor).find(|c| c.kind() == kind)
}

fn children_of_kind<'t>(node: Node<'t>, kind: &str) -> Vec<Node<'t>> {
    let mut cursor = node.walk();
    node.children(&mut cursor)
        .filter(|c| c.kind() == kind)
        .collect()
}

/// `_make_range`: 1-based lines, 0-based byte columns.
fn range_of(node: Node<'_>) -> Range {
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

/// `_get_visibility`: exported iff the first character is upper case
/// (Python's `str.isupper` on one character is Unicode `Uppercase`).
pub(super) fn visibility(name: &str) -> &'static str {
    if name.chars().next().is_some_and(char::is_uppercase) {
        "public"
    } else {
        "private"
    }
}

fn member_type(kind: &str) -> Map<String, Value> {
    let mut annotations = Map::new();
    annotations.insert("member_type".to_owned(), Value::String(kind.to_owned()));
    annotations
}

/// `', '.join(f'{n} {t}' for n, t in pairs)`.
fn join_pairs(pairs: &[Pair]) -> String {
    pairs
        .iter()
        .map(|(n, t)| format!("{n} {t}"))
        .collect::<Vec<_>>()
        .join(", ")
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

fn push_results(signature: &mut String, results: &[String]) {
    match results {
        [] => {}
        [one] => {
            signature.push(' ');
            signature.push_str(one);
        }
        many => {
            let _ = write!(signature, " ({})", many.join(", "));
        }
    }
}

fn joined_returns(results: &[String]) -> Option<String> {
    (!results.is_empty()).then(|| results.join(", "))
}

/// Python's `str.strip()` with no argument: Unicode whitespace plus the
/// ASCII separators `\x1c`–`\x1f`, which `str.isspace` also counts.
pub(super) fn py_strip(text: &str) -> &str {
    text.trim_matches(|c: char| c.is_whitespace() || ('\x1c'..='\x1f').contains(&c))
}

/// `_strip_pointer_slice`: drop leading `*`, `[]` and `[N]`, then a `map[K]`
/// prefix. Python quirk: `chan T`, `func(…)` and `interface{}` are left as
/// they are and so become relationship targets.
pub(super) fn strip_pointer_slice(type_str: &str) -> &str {
    let mut s = py_strip(type_str);
    while s.starts_with('*') || s.starts_with('[') {
        if let Some(rest) = s.strip_prefix('*') {
            s = rest;
        } else if let Some(rest) = s.strip_prefix("[]") {
            s = rest;
        } else if let Some(end) = s.find(']') {
            s = &s[end + 1..];
        } else {
            break;
        }
    }
    if s.starts_with("map[") {
        let mut depth = 0i32;
        for (index, ch) in s.char_indices() {
            if ch == '[' {
                depth += 1;
            } else if ch == ']' {
                depth -= 1;
                if depth == 0 {
                    s = &s[index + 1..];
                    break;
                }
            }
        }
    }
    py_strip(s)
}

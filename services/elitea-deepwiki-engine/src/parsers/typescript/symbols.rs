//! Pass 1a: the Python `SymbolExtractor` — symbols, plus the relationships
//! the symbol walk itself emits (inheritance, implementation, imports and
//! field composition/aggregation).
//!
//! The walk is the Python visitor's, rule for rule. Its quirks are kept on
//! purpose, because the graph is built from what they produce:
//!
//! * the walk does not enter a class, interface, enum, function, method or
//!   type alias except through its own handler, so nothing inside a
//!   function body or a method body is a symbol — but an arrow function is
//!   walked normally, so declarations inside one ARE symbols, with the
//!   enclosing class (or module) as parent;
//! * a handler that is not one of those still walks its children after it
//!   runs, so an object type nested in an interface property, a variable
//!   annotation or a parameter yields `property` symbols of the enclosing
//!   scope;
//! * decorators are collected as the walk meets them and attached to the
//!   NEXT class, interface, function, method or field — a field's own
//!   decorators are its children, met after the field is made, so they go
//!   to whatever follows it;
//! * only the first declarator of `const a = 1, b = 2` is a symbol; enum
//!   members with an initialiser are not symbols.

use super::ast::{
    annotation_text, children, contains_jsx, find_child, first_type_in_annotation, has_child,
    is_tsx, range, stem, text,
};
use super::source::{Source, py_strip};
use crate::parsers::limits;
use crate::parsers::model::{Range, Relationship, RelationshipType, Scope, Symbol, SymbolType};
use serde_json::{Map, Value, json};
use tree_sitter::Node;

/// Handled kinds the Python visitor does not recurse into afterwards.
/// (`arrow_function` and the namespace kinds are listed in Python too, but
/// have no handler, so they are walked normally.)
const NO_RECURSE: &[&str] = &[
    "class_declaration",
    "abstract_class_declaration",
    "interface_declaration",
    "function_declaration",
    "method_definition",
    "enum_declaration",
    "type_alias_declaration",
];

/// Field types that are not a composition/aggregation target.
const PRIMITIVES: &[&str] = &[
    "string", "number", "boolean", "any", "void", "never", "unknown",
];

/// A function parameter as `_extract_parameter_info` returns it.
struct Param {
    name: String,
    ty: Option<String>,
    range: Option<Range>,
}

pub(super) struct SymbolExtractor<'a> {
    source: &'a Source,
    file_path: &'a str,
    stem: String,
    tsx_mode: bool,
    pub(super) symbols: Vec<Symbol>,
    pub(super) relationships: Vec<Relationship>,
    scope_stack: Vec<String>,
    current_decorators: Vec<Value>,
}

impl<'a> SymbolExtractor<'a> {
    pub(super) fn new(source: &'a Source, file_path: &'a str) -> Self {
        Self {
            source,
            file_path,
            stem: stem(file_path),
            tsx_mode: is_tsx(file_path),
            symbols: Vec::new(),
            relationships: Vec::new(),
            scope_stack: Vec::new(),
            current_decorators: Vec::new(),
        }
    }

    fn text(&self, node: Node<'_>) -> String {
        text(self.source, node).to_owned()
    }

    /// `'.'.join(scope_stack) if scope_stack else None`.
    fn parent_symbol(&self) -> Option<String> {
        (!self.scope_stack.is_empty()).then(|| self.scope_stack.join("."))
    }

    /// `f"{parent}.{name}" if parent else name` (an empty parent is falsy).
    fn qualify(parent: Option<&String>, name: &str) -> String {
        match parent {
            Some(parent) if !parent.is_empty() => format!("{parent}.{name}"),
            _ => name.to_owned(),
        }
    }

    pub(super) fn visit(&mut self, node: Node<'_>) {
        let kind = node.kind();
        let handled = match kind {
            "program" => {
                self.scope_stack.push(self.stem.clone());
                true
            }
            "class_declaration" | "abstract_class_declaration" => {
                self.visit_class(node);
                true
            }
            "interface_declaration" => {
                self.visit_interface(node);
                true
            }
            "type_alias_declaration" => {
                self.visit_type_alias(node);
                true
            }
            "enum_declaration" => {
                self.visit_enum(node);
                true
            }
            "function_declaration" => {
                self.visit_function(node);
                true
            }
            "method_definition" => {
                self.visit_method(node);
                true
            }
            "public_field_definition" => {
                self.extract_class_field(node);
                true
            }
            "method_signature" | "abstract_method_signature" => {
                self.visit_method_signature(node);
                true
            }
            "property_signature" => {
                self.visit_property_signature(node);
                true
            }
            "lexical_declaration" => {
                // Always true: `program` pushed the stem first.
                if self.scope_stack.is_empty() || self.scope_stack[0] == self.stem {
                    self.extract_variable_declaration(node);
                }
                true
            }
            "variable_declaration" => {
                // Python passes the `variable_declaration` itself, which has
                // no `identifier` child: `var` declarations are never symbols.
                self.extract_variable_from_declarator(node, Some("const"));
                true
            }
            "import_statement" => {
                self.visit_import(node);
                true
            }
            "decorator" => {
                self.visit_decorator(node);
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

    fn new_symbol(
        &self,
        name: &str,
        symbol_type: SymbolType,
        scope: Scope,
        node: Node<'_>,
    ) -> Symbol {
        Symbol::new(name, symbol_type, scope, range(node), self.file_path)
    }

    /// Take the pending decorators into `metadata` (and clear them).
    fn take_decorators(&mut self, metadata: &mut Map<String, Value>) {
        if !self.current_decorators.is_empty() {
            metadata.insert(
                "decorators".to_owned(),
                Value::Array(std::mem::take(&mut self.current_decorators)),
            );
        }
    }

    fn generic_metadata(&self, node: Node<'_>, metadata: &mut Map<String, Value>) {
        let generic_params = self.generic_parameters(node);
        if !generic_params.is_empty() {
            metadata.insert("is_generic".to_owned(), Value::Bool(true));
            metadata.insert("generic_params".to_owned(), json!(generic_params));
        }
    }

    fn visit_class(&mut self, node: Node<'_>) {
        let Some(name_node) = find_child(node, &["type_identifier"]) else {
            return;
        };
        let name = self.text(name_node);
        let parent_symbol = self.parent_symbol();
        let full_name = Self::qualify(parent_symbol.as_ref(), &name);
        let is_abstract = has_modifier(node, "abstract");
        let is_exported = has_modifier(node, "export");
        let (extends_types, implements_types) = find_child(node, &["class_heritage"])
            .map(|heritage| self.class_heritage(heritage))
            .unwrap_or_default();

        let scope = if self.scope_stack.is_empty() {
            Scope::Global
        } else {
            Scope::Class
        };
        let mut symbol = self.new_symbol(&name, SymbolType::Class, scope, node);
        symbol.parent_symbol.clone_from(&parent_symbol);
        symbol.full_name = Some(full_name.clone());
        symbol.source_text = limits::kept_text(node.byte_range().len(), || self.text(node));
        symbol.is_abstract = is_abstract;
        let mut metadata = Map::new();
        if is_exported {
            metadata.insert("is_exported".to_owned(), Value::Bool(true));
        }
        self.generic_metadata(node, &mut metadata);
        self.take_decorators(&mut metadata);
        if self.tsx_mode
            && extends_types
                .iter()
                .any(|base| base.contains("Component") || base.contains("PureComponent"))
        {
            metadata.insert("is_react_component".to_owned(), Value::Bool(true));
            metadata.insert("component_type".to_owned(), json!("class"));
        }
        symbol.metadata = metadata;
        self.symbols.push(symbol);

        self.scope_stack.push(name);
        if let Some(body) = find_child(node, &["class_body"]) {
            for child in children(body) {
                self.visit(child);
            }
        }
        self.scope_stack.pop();

        for (targets, rel_type) in [
            (extends_types, RelationshipType::Inheritance),
            (implements_types, RelationshipType::Implementation),
        ] {
            for target in targets {
                let mut rel =
                    Relationship::new(full_name.clone(), target, rel_type, self.file_path);
                rel.source_range = Some(range(node));
                self.relationships.push(rel);
            }
        }
    }

    fn visit_interface(&mut self, node: Node<'_>) {
        let Some(name_node) = find_child(node, &["type_identifier"]) else {
            return;
        };
        let name = self.text(name_node);
        let parent_symbol = self.parent_symbol();
        let full_name = Self::qualify(parent_symbol.as_ref(), &name);
        let is_exported = has_modifier(node, "export");
        let extends_types = self.interface_extends(node);

        let scope = if truthy(parent_symbol.as_ref()) {
            Scope::Class
        } else {
            Scope::Global
        };
        let mut symbol = self.new_symbol(&name, SymbolType::Interface, scope, node);
        symbol.parent_symbol.clone_from(&parent_symbol);
        symbol.full_name = Some(full_name.clone());
        symbol.source_text = limits::kept_text(node.byte_range().len(), || self.text(node));
        let mut metadata = Map::new();
        if is_exported {
            metadata.insert("is_exported".to_owned(), Value::Bool(true));
        }
        self.generic_metadata(node, &mut metadata);
        self.take_decorators(&mut metadata);
        symbol.metadata = metadata;
        self.symbols.push(symbol);

        self.scope_stack.push(name);
        if let Some(body) = find_child(node, &["interface_body"]) {
            for child in children(body) {
                self.visit(child);
            }
        }
        self.scope_stack.pop();

        for target in extends_types {
            let mut rel = Relationship::new(
                full_name.clone(),
                target,
                RelationshipType::Inheritance,
                self.file_path,
            );
            rel.source_range = Some(range(node));
            self.relationships.push(rel);
        }
    }

    fn visit_type_alias(&mut self, node: Node<'_>) {
        let Some(name_node) = find_child(node, &["type_identifier"]) else {
            return;
        };
        let name = self.text(name_node);
        let parent_symbol = self.parent_symbol();
        let full_name = Self::qualify(parent_symbol.as_ref(), &name);
        let is_exported = has_modifier(node, "export");
        // Python searches the children for the first of these kinds — and
        // the alias's own name is the first `type_identifier`, so this is
        // almost always the alias's name, not the aliased type.
        let aliased_type = find_child(
            node,
            &[
                "union_type",
                "intersection_type",
                "object_type",
                "type_identifier",
            ],
        )
        .map(|t| self.text(t));
        let generic_params = self.generic_parameters(node);

        let scope = if truthy(parent_symbol.as_ref()) {
            Scope::Class
        } else {
            Scope::Global
        };
        let mut symbol = self.new_symbol(&name, SymbolType::TypeAlias, scope, node);
        symbol.parent_symbol = parent_symbol;
        symbol.full_name = Some(full_name);
        symbol.return_type.clone_from(&aliased_type);
        symbol.source_text = limits::kept_text(node.byte_range().len(), || self.text(node));
        let mut metadata = Map::new();
        metadata.insert("is_type_alias".to_owned(), Value::Bool(true));
        metadata.insert("aliased_type".to_owned(), json!(aliased_type));
        metadata.insert("is_exported".to_owned(), Value::Bool(is_exported));
        metadata.insert(
            "is_generic".to_owned(),
            Value::Bool(!generic_params.is_empty()),
        );
        metadata.insert(
            "generic_params".to_owned(),
            if generic_params.is_empty() {
                Value::Null
            } else {
                json!(generic_params)
            },
        );
        symbol.metadata = metadata;
        self.symbols.push(symbol);
    }

    fn visit_enum(&mut self, node: Node<'_>) {
        let Some(name_node) = find_child(node, &["identifier"]) else {
            return;
        };
        let name = self.text(name_node);
        let parent_symbol = self.parent_symbol();
        let full_name = Self::qualify(parent_symbol.as_ref(), &name);
        let is_exported = has_modifier(node, "export");
        let is_const = has_modifier(node, "const");

        let scope = if self.scope_stack.is_empty() {
            Scope::Global
        } else {
            Scope::Class
        };
        let mut symbol = self.new_symbol(&name, SymbolType::Enum, scope, node);
        symbol.parent_symbol = parent_symbol;
        symbol.full_name = Some(full_name.clone());
        symbol.source_text = limits::kept_text(node.byte_range().len(), || self.text(node));
        let mut metadata = Map::new();
        metadata.insert("is_enum".to_owned(), Value::Bool(true));
        metadata.insert("is_const_enum".to_owned(), Value::Bool(is_const));
        if is_exported {
            metadata.insert("is_exported".to_owned(), Value::Bool(true));
        }
        symbol.metadata = metadata;
        self.symbols.push(symbol);

        self.scope_stack.push(name);
        if let Some(body) = find_child(node, &["enum_body"]) {
            // Only bare members: `A = 1` is an `enum_assignment`, skipped.
            for child in children(body) {
                if child.kind() == "property_identifier" {
                    let member = self.text(child);
                    let mut symbol =
                        self.new_symbol(&member, SymbolType::Constant, Scope::Class, child);
                    symbol.full_name = Some(format!("{full_name}.{member}"));
                    symbol.parent_symbol = Some(full_name.clone());
                    symbol
                        .metadata
                        .insert("is_enum_member".to_owned(), Value::Bool(true));
                    self.symbols.push(symbol);
                }
            }
        }
        self.scope_stack.pop();
    }

    fn visit_function(&mut self, node: Node<'_>) {
        let Some(name_node) = find_child(node, &["identifier"]) else {
            return;
        };
        let name = self.text(name_node);
        let parent_symbol = self.parent_symbol();
        let full_name = Self::qualify(parent_symbol.as_ref(), &name);
        let is_exported = has_modifier(node, "export");
        let is_async = has_modifier(node, "async");
        let parameters = self.function_parameters(node);

        let mut symbol = self.new_symbol(&name, SymbolType::Function, Scope::Function, node);
        symbol.parent_symbol = parent_symbol;
        symbol.full_name = Some(full_name.clone());
        symbol.return_type = annotation_text(self.source, node);
        symbol.parameter_types = parameter_types(&parameters);
        symbol.source_text = limits::kept_text(node.byte_range().len(), || self.text(node));
        symbol.is_async = is_async;
        let mut metadata = Map::new();
        if is_exported {
            metadata.insert("is_exported".to_owned(), Value::Bool(true));
        }
        self.generic_metadata(node, &mut metadata);
        self.take_decorators(&mut metadata);
        if self.tsx_mode && children(node).into_iter().any(contains_jsx) {
            metadata.insert("is_react_component".to_owned(), Value::Bool(true));
            metadata.insert("component_type".to_owned(), json!("functional"));
        }
        symbol.metadata = metadata;
        let fallback = symbol.range;
        self.symbols.push(symbol);
        self.push_parameters(&parameters, &full_name, fallback);
    }

    fn visit_method(&mut self, node: Node<'_>) {
        let Some(name_node) = find_child(node, &["property_identifier", "identifier"]) else {
            return;
        };
        let name = self.text(name_node);
        if name == "constructor" {
            self.extract_constructor(node);
            return;
        }
        let parent_symbol = self.parent_symbol();
        let full_name = Self::qualify(parent_symbol.as_ref(), &name);
        let parameters = self.function_parameters(node);

        let mut symbol = self.new_symbol(&name, SymbolType::Method, Scope::Function, node);
        symbol.parent_symbol = parent_symbol;
        symbol.full_name = Some(full_name.clone());
        symbol.return_type = annotation_text(self.source, node);
        symbol.parameter_types = parameter_types(&parameters);
        symbol.source_text = limits::kept_text(node.byte_range().len(), || self.text(node));
        symbol.is_static = has_modifier(node, "static");
        symbol.is_async = has_modifier(node, "async");
        symbol.is_abstract = has_modifier(node, "abstract");
        symbol.visibility = self.access_modifier(node);
        let mut metadata = Map::new();
        self.generic_metadata(node, &mut metadata);
        self.take_decorators(&mut metadata);
        symbol.metadata = metadata;
        let fallback = symbol.range;
        self.symbols.push(symbol);
        self.push_parameters(&parameters, &full_name, fallback);
    }

    fn extract_constructor(&mut self, node: Node<'_>) {
        let parent_symbol = self.parent_symbol();
        let full_name = match &parent_symbol {
            Some(parent) if !parent.is_empty() => format!("{parent}.constructor"),
            _ => "constructor".to_owned(),
        };
        let parameters = self.function_parameters(node);
        let mut symbol = self.new_symbol(
            "constructor",
            SymbolType::Constructor,
            Scope::Function,
            node,
        );
        symbol.parent_symbol = parent_symbol;
        symbol.full_name = Some(full_name.clone());
        symbol.parameter_types = parameter_types(&parameters);
        symbol.source_text = limits::kept_text(node.byte_range().len(), || self.text(node));
        let fallback = symbol.range;
        self.symbols.push(symbol);
        self.push_parameters(&parameters, &full_name, fallback);
    }

    /// The `parameter` symbols of a function, method or constructor.
    fn push_parameters(&mut self, parameters: &[Param], owner: &str, fallback: Range) {
        for param in parameters {
            let mut symbol = Symbol::new(
                param.name.clone(),
                SymbolType::Parameter,
                Scope::Function,
                param.range.unwrap_or(fallback),
                self.file_path,
            );
            symbol.parent_symbol = Some(owner.to_owned());
            symbol.full_name = Some(format!("{owner}::{}", param.name));
            symbol.return_type.clone_from(&param.ty);
            self.symbols.push(symbol);
        }
    }

    fn visit_method_signature(&mut self, node: Node<'_>) {
        let Some(name_node) = find_child(node, &["property_identifier"]) else {
            return;
        };
        let name = self.text(name_node);
        let parent_symbol = self.parent_symbol();
        let full_name = Self::qualify(parent_symbol.as_ref(), &name);
        let parameters = self.function_parameters(node);
        let mut symbol = self.new_symbol(&name, SymbolType::Method, Scope::Function, node);
        symbol.parent_symbol = parent_symbol;
        symbol.full_name = Some(full_name);
        symbol.return_type = annotation_text(self.source, node);
        symbol.parameter_types = parameter_types(&parameters);
        symbol.source_text = limits::kept_text(node.byte_range().len(), || self.text(node));
        symbol.is_abstract = true;
        self.symbols.push(symbol);
    }

    fn visit_property_signature(&mut self, node: Node<'_>) {
        let Some(name_node) = find_child(node, &["property_identifier"]) else {
            return;
        };
        let name = self.text(name_node);
        let parent_symbol = self.parent_symbol();
        let full_name = Self::qualify(parent_symbol.as_ref(), &name);
        let is_optional = has_child(node, "?");
        let is_readonly = has_modifier(node, "readonly");
        let type_annotation = annotation_text(self.source, node);

        let mut symbol = self.new_symbol(&name, SymbolType::Property, Scope::Class, node);
        symbol.parent_symbol = parent_symbol;
        symbol.full_name = Some(full_name.clone());
        symbol.return_type.clone_from(&type_annotation);
        symbol.source_text = limits::kept_text(node.byte_range().len(), || self.text(node));
        if is_optional {
            symbol
                .metadata
                .insert("is_optional".to_owned(), Value::Bool(true));
        }
        if is_readonly {
            symbol
                .metadata
                .insert("is_readonly".to_owned(), Value::Bool(true));
        }
        self.symbols.push(symbol);
        if let Some(field_type) = type_annotation.filter(|t| !t.is_empty()) {
            self.field_type_relationship(&field_type, &full_name, &name, node);
        }
    }

    fn extract_class_field(&mut self, node: Node<'_>) {
        if self.scope_stack.is_empty() {
            return;
        }
        // Python takes the first `property_identifier` OR `identifier` child:
        // for `#x = y` that is the initialiser `y`.
        let Some(name_node) = find_child(node, &["property_identifier", "identifier"]) else {
            return;
        };
        let field_name = self.text(name_node);
        let parent_symbol = self.scope_stack.join(".");
        let full_name = format!("{parent_symbol}.{field_name}");
        let is_readonly = has_modifier(node, "readonly");
        let is_optional = has_child(node, "?");
        let field_type = annotation_text(self.source, node);

        let mut symbol = self.new_symbol(&field_name, SymbolType::Field, Scope::Class, node);
        symbol.parent_symbol = Some(parent_symbol);
        symbol.full_name = Some(full_name.clone());
        symbol.return_type.clone_from(&field_type);
        symbol.is_static = has_modifier(node, "static");
        symbol.visibility = self.access_modifier(node);
        let mut metadata = Map::new();
        if is_readonly {
            metadata.insert("is_readonly".to_owned(), Value::Bool(true));
        }
        if is_optional {
            metadata.insert("is_optional".to_owned(), Value::Bool(true));
        }
        self.take_decorators(&mut metadata);
        symbol.metadata = metadata;
        self.symbols.push(symbol);
        if let Some(field_type) = field_type.filter(|t| !t.is_empty()) {
            self.field_type_relationship(&field_type, &full_name, &field_name, node);
        }
    }

    /// `_extract_field_type_relationship`: composition for a required type,
    /// aggregation when the type text mentions `?`, `undefined` or `null`.
    /// The "base type" is the text with those markers and `[]` removed — so
    /// `null` alone yields an empty target, as in Python.
    fn field_type_relationship(
        &mut self,
        field_type: &str,
        field_symbol: &str,
        field_name: &str,
        node: Node<'_>,
    ) {
        let is_optional = field_type.contains('?')
            || field_type.contains("undefined")
            || field_type.contains("null");
        let stripped = field_type
            .replace('?', "")
            .replace("[]", "")
            .replace("undefined", "")
            .replace("null", "");
        let base_type = py_strip(&stripped);
        if PRIMITIVES.contains(&base_type.to_lowercase().as_str()) {
            return;
        }
        let rel_type = if is_optional {
            RelationshipType::Aggregation
        } else {
            RelationshipType::Composition
        };
        let mut rel = Relationship::new(field_symbol, base_type, rel_type, self.file_path);
        rel.source_range = Some(range(node));
        rel.annotations
            .insert("field_name".to_owned(), json!(field_name));
        rel.annotations
            .insert("field_type".to_owned(), json!(field_type));
        self.relationships.push(rel);
    }

    fn extract_variable_declaration(&mut self, node: Node<'_>) {
        let kind = children(node)
            .into_iter()
            .map(|c| c.kind())
            .find(|k| matches!(*k, "const" | "let" | "var"));
        if let Some(declarator) = find_child(node, &["variable_declarator"]) {
            self.extract_variable_from_declarator(declarator, kind);
        }
    }

    fn extract_variable_from_declarator(&mut self, node: Node<'_>, kind: Option<&str>) {
        let Some(identifier) = find_child(node, &["identifier"]) else {
            return;
        };
        let var_name = self.text(identifier);
        let parent_symbol = self.parent_symbol();
        let full_name = Self::qualify(parent_symbol.as_ref(), &var_name);
        if let Some(arrow) = find_child(node, &["arrow_function"]) {
            self.extract_arrow_function(node, arrow, &var_name, full_name, parent_symbol);
            return;
        }
        let var_type = annotation_text(self.source, node);
        let is_exported = grandparent_exported(node);
        let symbol_type = if kind == Some("const") {
            SymbolType::Constant
        } else {
            SymbolType::Variable
        };
        let scope = if truthy(parent_symbol.as_ref()) {
            Scope::Class
        } else {
            Scope::Global
        };
        let mut symbol = self.new_symbol(&var_name, symbol_type, scope, node);
        symbol.parent_symbol = parent_symbol;
        symbol.full_name = Some(full_name);
        symbol.return_type = var_type;
        symbol.source_text = limits::kept_text(node.byte_range().len(), || self.text(node));
        symbol.metadata.insert("kind".to_owned(), json!(kind));
        symbol
            .metadata
            .insert("is_exported".to_owned(), Value::Bool(is_exported));
        self.symbols.push(symbol);
    }

    /// `const f = (...) => ...`: a `function` symbol spanning the declarator.
    /// No `parameter` symbols; a `React.FC<Props>` annotation supplies the
    /// parameter types when the arrow has no parenthesised parameters.
    fn extract_arrow_function(
        &mut self,
        declarator: Node<'_>,
        arrow: Node<'_>,
        name: &str,
        full_name: String,
        parent_symbol: Option<String>,
    ) {
        let mut return_type = None;
        let mut react_prop_types = Vec::new();
        if let Some(t) =
            find_child(declarator, &["type_annotation"]).and_then(first_type_in_annotation)
        {
            return_type = Some(self.text(t));
            react_prop_types = self.react_fc_prop_types(t);
        }
        let mut parameters = self.function_parameters(arrow);
        if parameters.is_empty() && !react_prop_types.is_empty() {
            parameters = react_prop_types;
        }
        let is_exported = grandparent_exported(declarator);
        let mut symbol = self.new_symbol(name, SymbolType::Function, Scope::Function, declarator);
        symbol.parent_symbol = parent_symbol;
        symbol.full_name = Some(full_name);
        symbol.return_type = return_type;
        symbol.parameter_types = parameter_types(&parameters);
        symbol.source_text =
            limits::kept_text(declarator.byte_range().len(), || self.text(declarator));
        symbol.is_async = has_modifier(arrow, "async");
        if is_exported {
            symbol
                .metadata
                .insert("is_exported".to_owned(), Value::Bool(true));
        }
        if self.tsx_mode && children(arrow).into_iter().any(contains_jsx) {
            symbol
                .metadata
                .insert("is_react_component".to_owned(), Value::Bool(true));
            symbol
                .metadata
                .insert("component_type".to_owned(), json!("functional"));
        }
        self.symbols.push(symbol);
    }

    /// `_extract_react_fc_prop_types`: the props of `React.FC<...>`.
    fn react_fc_prop_types(&self, type_node: Node<'_>) -> Vec<Param> {
        if type_node.kind() != "generic_type" {
            return Vec::new();
        }
        let is_component = children(type_node).into_iter().find_map(|child| {
            matches!(
                child.kind(),
                "type_identifier" | "member_expression" | "nested_type_identifier"
            )
            .then(|| self.text(child))
            .filter(|t| t.contains("FC") || t.contains("FunctionComponent"))
        });
        if is_component.is_none() {
            return Vec::new();
        }
        let Some(arguments) = find_child(type_node, &["type_arguments"]) else {
            return Vec::new();
        };
        let Some(first) = find_child(
            arguments,
            &["object_type", "type_identifier", "type_reference"],
        ) else {
            return Vec::new();
        };
        if first.kind() != "object_type" {
            return vec![Param {
                name: "props".to_owned(),
                ty: Some(self.text(first)),
                range: None,
            }];
        }
        children(first)
            .into_iter()
            .filter(|c| c.kind() == "property_signature")
            .filter_map(|prop| {
                let name = self.text(find_child(prop, &["property_identifier"])?);
                let ty = annotation_text(self.source, prop).filter(|t| !t.is_empty())?;
                Some(Param {
                    name,
                    ty: Some(ty),
                    range: None,
                })
            })
            .collect()
    }

    fn visit_import(&mut self, node: Node<'_>) {
        let Some(source_node) = find_child(node, &["string"]) else {
            return;
        };
        let raw = self.text(source_node);
        let source_module = raw.trim_matches('"').trim_matches('\'').to_owned();
        let Some(clause) = find_child(node, &["import_clause"]) else {
            return;
        };
        let mut imported = Vec::new();
        if let Some(named) = find_child(clause, &["named_imports"]) {
            for spec in children(named) {
                if spec.kind() == "import_specifier"
                    && let Some(id) = find_child(spec, &["identifier"])
                {
                    imported.push(self.text(id));
                }
            }
        }
        if let Some(id) =
            find_child(clause, &["namespace_import"]).and_then(|ns| find_child(ns, &["identifier"]))
        {
            imported.push(self.text(id));
        }
        if let Some(id) = find_child(clause, &["identifier"]) {
            imported.push(self.text(id));
        }
        for imported_name in imported {
            let mut rel = Relationship::new(
                self.stem.clone(),
                format!("{source_module}::{imported_name}"),
                RelationshipType::Imports,
                self.file_path,
            );
            rel.source_range = Some(range(node));
            rel.annotations
                .insert("import_type".to_owned(), json!("named"));
            rel.annotations
                .insert("source_module".to_owned(), json!(source_module));
            rel.annotations
                .insert("imported_name".to_owned(), json!(imported_name));
            self.relationships.push(rel);
        }
    }

    fn visit_decorator(&mut self, node: Node<'_>) {
        let identifier = match find_child(node, &["call_expression"]) {
            Some(call) => find_child(call, &["identifier"]),
            None => find_child(node, &["identifier"]),
        };
        if let Some(identifier) = identifier {
            let name = self.text(identifier);
            if !name.is_empty() {
                let r = range(node);
                self.current_decorators.push(json!({
                    "name": name,
                    "range": {
                        "start": {"line": r.start.line, "column": r.start.column},
                        "end": {"line": r.end.line, "column": r.end.column},
                    },
                }));
            }
        }
    }

    /// `_get_access_modifier`.
    fn access_modifier(&self, node: Node<'_>) -> Option<String> {
        for child in children(node) {
            match child.kind() {
                kind @ ("public" | "private" | "protected") => return Some(kind.to_owned()),
                "accessibility_modifier" => return Some(self.text(child)),
                _ => {}
            }
        }
        None
    }

    /// `_extract_generic_parameters`.
    fn generic_parameters(&self, node: Node<'_>) -> Vec<String> {
        let Some(params) = find_child(node, &["type_parameters"]) else {
            return Vec::new();
        };
        children(params)
            .into_iter()
            .filter(|c| c.kind() == "type_parameter")
            .filter_map(|c| find_child(c, &["type_identifier"]))
            .map(|n| self.text(n))
            .collect()
    }

    /// `_parse_class_heritage`: one `extends` target, then every distinct
    /// `implements` target.
    fn class_heritage(&self, heritage: Node<'_>) -> (Vec<String>, Vec<String>) {
        let mut extends_types = Vec::new();
        let mut implements_types: Vec<String> = Vec::new();
        for child in children(heritage) {
            match child.kind() {
                "extends_clause" => {
                    if let Some(t) = find_child(
                        child,
                        &[
                            "identifier",
                            "type_identifier",
                            "generic_type",
                            "member_expression",
                        ],
                    ) {
                        extends_types.push(self.text(t));
                    }
                }
                "implements_clause" => {
                    for sub in children(child) {
                        if matches!(
                            sub.kind(),
                            "type_identifier" | "identifier" | "generic_type"
                        ) {
                            let t = self.text(sub);
                            if !implements_types.contains(&t) && t != "implements" {
                                implements_types.push(t);
                            }
                        }
                    }
                }
                _ => {}
            }
        }
        (extends_types, implements_types)
    }

    /// `_extract_interface_extends`.
    fn interface_extends(&self, node: Node<'_>) -> Vec<String> {
        let clause = find_child(node, &["extends_type_clause"])
            .or_else(|| find_child(node, &["extends_clause"]));
        clause
            .map(|clause| {
                children(clause)
                    .into_iter()
                    .filter(|c| matches!(c.kind(), "type_identifier" | "generic_type"))
                    .map(|c| self.text(c))
                    .collect()
            })
            .unwrap_or_default()
    }

    /// `_extract_function_parameters`: named parameters only — a
    /// destructured, rest or `this` parameter has no `identifier` child.
    fn function_parameters(&self, node: Node<'_>) -> Vec<Param> {
        let Some(formal) = find_child(node, &["formal_parameters"]) else {
            return Vec::new();
        };
        children(formal)
            .into_iter()
            .filter(|c| matches!(c.kind(), "required_parameter" | "optional_parameter"))
            .filter_map(|param| {
                let identifier = find_child(param, &["identifier"])?;
                Some(Param {
                    name: self.text(identifier),
                    ty: annotation_text(self.source, param),
                    range: Some(range(param)),
                })
            })
            .collect()
    }
}

/// `[p['type'] for p in parameters if p.get('type')]`.
fn parameter_types(parameters: &[Param]) -> Vec<String> {
    parameters
        .iter()
        .filter_map(|p| p.ty.clone().filter(|t| !t.is_empty()))
        .collect()
}

/// `bool(value)` for an optional string.
fn truthy(value: Option<&String>) -> bool {
    value.is_some_and(|v| !v.is_empty())
}

/// `_has_modifier`: a direct child of that kind; for `export`, also a
/// sibling `export` (the `export_statement` wrapping the declaration).
pub(super) fn has_modifier(node: Node<'_>, modifier: &str) -> bool {
    if has_child(node, modifier) {
        return true;
    }
    modifier == "export"
        && node
            .parent()
            .is_some_and(|parent| has_child(parent, "export"))
}

/// `node.parent and _has_modifier(node.parent.parent, 'export')` for a
/// declarator: true when the declaration sits in an `export_statement`.
fn grandparent_exported(declarator: Node<'_>) -> bool {
    declarator
        .parent()
        .and_then(|p| p.parent())
        .is_some_and(|gp| has_modifier(gp, "export"))
}

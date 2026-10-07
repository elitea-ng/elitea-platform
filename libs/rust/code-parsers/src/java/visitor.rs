//! One Java file: the Python `JavaVisitorParser.parse_file`.
//!
//! Every rule here is the Python visitor's, quirks included, because the
//! graph gate compares this output with the Python output field by field.
//! Each quirk the port keeps on purpose is marked "Python quirk". Node kinds
//! are the `tree-sitter-java` 0.23.5 grammar's, which is the grammar
//! `tree_sitter_language_pack` 1.16.1 ships (same ABI 14, 1385 parse
//! states, 40 fields, 321 node kinds).
//!
//! What the Python parser never fills, this one does not either: no
//! `visibility`, `docstring`, `signature`, `comments`, `imports` or
//! `exports`, and no relationship has a `source_range`.

use super::source::Source;
use crate::limits;
use crate::model::{ParseResult, Relationship, RelationshipType, Scope, Symbol, SymbolType};
use serde_json::{Map, Value};
use std::collections::HashMap;
use tree_sitter::Node;

/// The type kinds a field, local variable or parameter type is read from.
/// Python quirk: `scoped_type_identifier` (`Map.Entry`) and the primitive
/// kinds are not in it, so such declarations have no type.
const TYPE_KINDS: &[&str] = &["type_identifier", "generic_type", "array_type"];

/// What a method's return type may be.
const RETURN_TYPE_KINDS: &[&str] = &["type_identifier", "generic_type", "array_type", "void_type"];

/// What a parameter type for `parameter_types` may be (primitives count).
const PARAMETER_TYPE_KINDS: &[&str] = &[
    "type_identifier",
    "generic_type",
    "array_type",
    "boolean_type",
    "integral_type",
    "floating_point_type",
    "void_type",
];

/// Fully qualified prefixes `_is_builtin_type` treats as the standard
/// library.
const STD_PACKAGES: &[&str] = &[
    "java.lang",
    "java.util",
    "java.io",
    "java.nio",
    "java.time",
    "java.math",
    "java.net",
    "java.text",
    "java.util.regex",
    "java.util.concurrent",
    "java.util.function",
    "java.util.stream",
    "java.security",
    "java.awt",
    "javax.swing",
    "javax.annotation",
];

/// Simple names `_is_builtin_type` treats as built in.
const BUILTIN_TYPES: &[&str] = &[
    "byte",
    "short",
    "int",
    "long",
    "float",
    "double",
    "char",
    "boolean",
    "void",
    "Byte",
    "Short",
    "Integer",
    "Long",
    "Float",
    "Double",
    "Character",
    "Boolean",
    "Void",
    "String",
    "Object",
    "Class",
    "Number",
    "Enum",
    "List",
    "ArrayList",
    "LinkedList",
    "Set",
    "HashSet",
    "LinkedHashSet",
    "TreeSet",
    "Map",
    "HashMap",
    "LinkedHashMap",
    "TreeMap",
    "Queue",
    "Deque",
    "ArrayDeque",
    "Collection",
    "Iterator",
    "Iterable",
    "Optional",
    "Objects",
    "Exception",
    "RuntimeException",
    "Error",
    "Throwable",
    "IllegalArgumentException",
    "IllegalStateException",
    "NullPointerException",
    "File",
    "Path",
    "URI",
    "URL",
    "Date",
    "Calendar",
    "LocalDate",
    "LocalDateTime",
    "BigInteger",
    "BigDecimal",
    "UUID",
    "Random",
    "Thread",
    "Runnable",
    "Pattern",
    "Matcher",
    "Serializable",
    "Override",
    "Deprecated",
    "SuppressWarnings",
    "FunctionalInterface",
];

/// `_is_builtin_type`. An empty name counts as built in.
pub(super) fn is_builtin_type(name: &str) -> bool {
    if name.is_empty() {
        return true;
    }
    if name.contains('.')
        && STD_PACKAGES.iter().any(|package| {
            name.strip_prefix(package)
                .is_some_and(|rest| rest.starts_with('.'))
        })
    {
        return true;
    }
    BUILTIN_TYPES.contains(&name)
}

/// Parse one decoded Java source into the per-file result. Cross-file
/// resolution happens later, in `super`.
pub(super) fn parse_source(file_path: &str, source: &Source) -> ParseResult {
    let mut parser = tree_sitter::Parser::new();
    let language: tree_sitter::Language = tree_sitter_java::LANGUAGE.into();
    if parser.set_language(&language).is_err() {
        return failed(file_path, "Tree-sitter Java parser not available");
    }
    let Some(tree) = parser.parse(source.bytes(), None) else {
        return failed(file_path, "Tree-sitter Java parser returned no tree");
    };
    let root = tree.root_node();
    if limits::too_deep(root) {
        return failed(file_path, limits::RECURSION_ERROR);
    }
    let mut visitor = Visitor::new(file_path, source);
    // A file without a package declaration (among the ROOT's children)
    // scopes its symbols under the file stem instead.
    let mut cursor = root.walk();
    if !root
        .children(&mut cursor)
        .any(|c| c.kind() == "package_declaration")
    {
        visitor.scope.push(file_stem(file_path));
    }
    visitor.visit(root);
    let defines = defines_relationships(&visitor.symbols, file_path);
    visitor.relationships.extend(defines);
    ParseResult {
        symbols: visitor.symbols,
        relationships: visitor.relationships,
        ..ParseResult::new(file_path, "java")
    }
}

/// A result that carries only an error, as every Python failure path builds.
pub(super) fn failed(file_path: &str, error: impl Into<String>) -> ParseResult {
    let mut result = ParseResult::new(file_path, "java");
    result.errors.push(error.into());
    result
}

/// `Path(file).stem`.
pub(super) fn file_stem(file_path: &str) -> String {
    std::path::Path::new(file_path)
        .file_stem()
        .map(|s| s.to_string_lossy().into_owned())
        .unwrap_or_default()
}

/// The first direct child of `node` whose kind is `kind`.
fn child_of_kind<'t>(node: Node<'t>, kind: &str) -> Option<Node<'t>> {
    let mut cursor = node.walk();
    node.children(&mut cursor).find(|c| c.kind() == kind)
}

/// The direct children of `node`, collected (tree-sitter's iterator borrows
/// a cursor, which the recursive walks cannot hold across calls).
fn children(node: Node<'_>) -> Vec<Node<'_>> {
    let mut cursor = node.walk();
    node.children(&mut cursor).collect()
}

/// Per-file visitor state (the Python parser's `_current_*` fields).
struct Visitor<'s> {
    file: &'s str,
    source: &'s Source,
    symbols: Vec<Symbol>,
    relationships: Vec<Relationship>,
    /// Scope names: the package (or file stem), then each class, method
    /// and `<init>`. Never popped for the package.
    scope: Vec<String>,
    /// Field name → type; file-wide, so a field of one class resolves a
    /// call in another class of the same file (Python quirk).
    field_types: HashMap<String, String>,
    /// Local variable name → type; file-wide too, never cleared between
    /// methods (Python quirk).
    variable_types: HashMap<String, String>,
}

impl<'s> Visitor<'s> {
    fn new(file: &'s str, source: &'s Source) -> Self {
        Self {
            file,
            source,
            symbols: Vec::new(),
            relationships: Vec::new(),
            scope: Vec::new(),
            field_types: HashMap::new(),
            variable_types: HashMap::new(),
        }
    }

    fn text(&self, node: Node<'_>) -> String {
        self.source.text(node).to_owned()
    }

    /// `_get_current_scope_path`.
    fn scope_path(&self) -> String {
        self.scope.join(".")
    }

    /// `parent_symbol = scope path if the stack is non-empty else None`.
    fn parent(&self) -> Option<String> {
        (!self.scope.is_empty()).then(|| self.scope_path())
    }

    /// `_build_qualified_name`.
    fn qualified(&self, name: &str) -> String {
        if self.scope.is_empty() {
            name.to_owned()
        } else {
            format!("{}.{name}", self.scope_path())
        }
    }

    /// The source of a call or creation: the scope path, or the file path
    /// when the stack is empty (only for an empty package name).
    fn scope_or_file(&self) -> String {
        if self.scope.is_empty() {
            self.file.to_owned()
        } else {
            self.scope_path()
        }
    }

    /// A relationship whose `target_file` is this file, as every Python
    /// constructor call here passes ("will be resolved later").
    fn relationship(
        &self,
        source: String,
        target: String,
        rel_type: RelationshipType,
    ) -> Relationship {
        let mut relationship = Relationship::new(source, target, rel_type, self.file);
        relationship.target_file = Some(self.file.to_owned());
        relationship
    }

    fn symbol(&self, name: &str, symbol_type: SymbolType, scope: Scope, node: Node<'_>) -> Symbol {
        Symbol::new(name, symbol_type, scope, self.source.range(node), self.file)
    }

    /// `visit_node`: dispatch by kind; `superclass` and `super_interfaces`
    /// are not descended into.
    fn visit(&mut self, node: Node<'_>) {
        match node.kind() {
            "package_declaration" => self.package(node),
            "import_declaration" => self.import(node),
            "class_declaration" => self.type_declaration(node, SymbolType::Class, true),
            "interface_declaration" => self.type_declaration(node, SymbolType::Interface, true),
            "enum_declaration" => self.type_declaration(node, SymbolType::Enum, false),
            "annotation_type_declaration" => self.annotation_declaration(node),
            "record_declaration" => self.record_declaration(node),
            "method_declaration" => self.method(node),
            "constructor_declaration" => self.constructor(node),
            "compact_constructor_declaration" => self.compact_constructor(node),
            "field_declaration" => self.field(node),
            "local_variable_declaration" => self.local_variable(node),
            "method_invocation" => self.method_invocation(node),
            "object_creation_expression" => self.object_creation(node),
            "super_interfaces" | "superclass" => {}
            _ => self.visit_children(node),
        }
    }

    fn visit_children(&mut self, node: Node<'_>) {
        for child in children(node) {
            self.visit(child);
        }
    }

    /// Visit `node`'s children inside the scope `name`.
    fn visit_in_scope(&mut self, node: Node<'_>, name: String) {
        self.scope.push(name);
        self.visit_children(node);
        self.scope.pop();
    }

    /// `_handle_package_declaration`: push the package name as a scope (no
    /// symbol). Python quirk: never popped, so a second declaration (in an
    /// ERROR recovery) nests under the first.
    fn package(&mut self, node: Node<'_>) {
        if let Some(name) = children(node)
            .into_iter()
            .find(|c| matches!(c.kind(), "scoped_identifier" | "identifier"))
        {
            let name = self.text(name);
            if !name.is_empty() {
                self.scope.push(name);
            }
        }
    }

    /// `_handle_import_declaration`: one `imports` edge from the scope path
    /// (the package) or the file stem to the dotted path. A wildcard import
    /// names its package (`java.util.*` → `java.util`); a static import
    /// names the member.
    fn import(&mut self, node: Node<'_>) {
        let Some(path) = children(node)
            .into_iter()
            .find(|c| matches!(c.kind(), "scoped_identifier" | "identifier"))
        else {
            return;
        };
        let path = self.text(path);
        if path.is_empty() {
            return;
        }
        let source = if self.scope.is_empty() {
            file_stem(self.file)
        } else {
            self.scope_path()
        };
        let relationship = self.relationship(source, path, RelationshipType::Imports);
        self.relationships.push(relationship);
    }

    /// The name of a type declaration: its first direct `identifier`.
    fn declared_name(&self, node: Node<'_>) -> Option<String> {
        child_of_kind(node, "identifier")
            .map(|n| self.text(n))
            .filter(|n| !n.is_empty())
    }

    /// `_handle_class_declaration`, `_handle_interface_declaration` and
    /// `_handle_enum_declaration`: one symbol, then (classes and interfaces
    /// only) the `extends` / `implements` edges, then the body in scope.
    fn type_declaration(&mut self, node: Node<'_>, symbol_type: SymbolType, inheritance: bool) {
        let Some(name) = self.declared_name(node) else {
            return;
        };
        let mut symbol = self.symbol(&name, symbol_type, Scope::Class, node);
        symbol.parent_symbol = self.parent();
        symbol.full_name = Some(self.qualified(&name));
        symbol.source_text = limits::kept_text(node.byte_range().len(), || self.text(node));
        let source = symbol.qualified_name();
        self.symbols.push(symbol);
        if inheritance {
            self.inheritance(node, &source);
        }
        self.visit_in_scope(node, name);
    }

    /// `_extract_inheritance_from_class`. Python quirk: only a bare
    /// `type_identifier` counts — `extends Base<T>`, `implements
    /// Comparable<T>` and `extends a.B` give no edge — and an interface's
    /// `extends` (an `extends_interfaces` node) is not read at all.
    fn inheritance(&mut self, node: Node<'_>, source: &str) {
        for child in children(node) {
            match child.kind() {
                "superclass" => {
                    if let Some(parent) = children(child)
                        .into_iter()
                        .find(|g| matches!(g.kind(), "type_identifier" | "identifier"))
                    {
                        let parent = self.text(parent);
                        if !parent.is_empty() {
                            let relationship = self.relationship(
                                source.to_owned(),
                                parent,
                                RelationshipType::Inheritance,
                            );
                            self.relationships.push(relationship);
                        }
                    }
                }
                "super_interfaces" => {
                    for list in children(child) {
                        if list.kind() != "type_list" {
                            continue;
                        }
                        for interface in children(list) {
                            if !matches!(interface.kind(), "type_identifier" | "identifier") {
                                continue;
                            }
                            let interface = self.text(interface);
                            if !interface.is_empty() {
                                let relationship = self.relationship(
                                    source.to_owned(),
                                    interface,
                                    RelationshipType::Implementation,
                                );
                                self.relationships.push(relationship);
                            }
                        }
                    }
                }
                _ => {}
            }
        }
    }

    /// `_handle_annotation_declaration`. Python quirk: the symbol has no
    /// parent, full name or source text.
    fn annotation_declaration(&mut self, node: Node<'_>) {
        let Some(name) = self.declared_name(node) else {
            return;
        };
        let symbol = self.symbol(&name, SymbolType::Annotation, Scope::Class, node);
        self.symbols.push(symbol);
        self.visit_in_scope(node, name);
    }

    /// `_handle_record_declaration`: a class marked `is_record`, without
    /// inheritance edges (its `implements` is skipped).
    fn record_declaration(&mut self, node: Node<'_>) {
        let Some(name) = self.declared_name(node) else {
            return;
        };
        let mut symbol = self.symbol(&name, SymbolType::Class, Scope::Class, node);
        symbol.parent_symbol = self.parent();
        symbol.full_name = Some(self.qualified(&name));
        symbol.source_text = limits::kept_text(node.byte_range().len(), || self.text(node));
        symbol
            .metadata
            .insert("is_record".to_owned(), Value::Bool(true));
        self.symbols.push(symbol);
        self.visit_in_scope(node, name);
    }

    /// `_extract_type_name`: the base name of a type node, or `None`.
    fn type_name(&self, node: Node<'_>) -> Option<String> {
        match node.kind() {
            "type_identifier"
            | "identifier"
            | "boolean_type"
            | "integral_type"
            | "floating_point_type"
            | "void_type" => Some(self.text(node)),
            "generic_type" => child_of_kind(node, "type_identifier").map(|c| self.text(c)),
            "array_type" | "wildcard" => children(node)
                .into_iter()
                .find(|c| matches!(c.kind(), "type_identifier" | "generic_type"))
                .and_then(|c| self.type_name(c)),
            _ => None,
        }
    }

    /// `_extract_generic_type_parameters`: the user types inside the first
    /// `type_arguments` of a `generic_type`, depth first.
    fn generic_parameters(&self, node: Node<'_>) -> Vec<String> {
        let mut out = Vec::new();
        if node.kind() != "generic_type" {
            return out;
        }
        if let Some(arguments) = child_of_kind(node, "type_arguments") {
            for argument in children(arguments) {
                self.collect_user_types(argument, &mut out);
            }
        }
        out
    }

    /// `_collect_user_types_from_type_node`.
    fn collect_user_types(&self, node: Node<'_>, out: &mut Vec<String>) {
        match node.kind() {
            "type_identifier" => {
                let name = self.text(node);
                if !is_builtin_type(&name) {
                    out.push(name);
                }
            }
            "generic_type" => {
                if let Some(base) = self.type_name(node).filter(|b| !is_builtin_type(b)) {
                    out.push(base);
                }
                if let Some(arguments) = child_of_kind(node, "type_arguments") {
                    for argument in children(arguments) {
                        self.collect_user_types(argument, out);
                    }
                }
            }
            "array_type" | "wildcard" => {
                for child in children(node) {
                    if matches!(
                        child.kind(),
                        "type_identifier" | "generic_type" | "array_type"
                    ) {
                        self.collect_user_types(child, out);
                    }
                }
            }
            _ => {}
        }
    }

    /// `_extract_parameter_types`: per `formal_parameter`, the first child
    /// of a type kind, when it yields a name. Python quirk: a varargs
    /// parameter (`spread_parameter`) is not a `formal_parameter`, and a
    /// `scoped_type_identifier` type is skipped past.
    fn parameter_types(&self, parameters: Node<'_>) -> Vec<String> {
        let mut types = Vec::new();
        for parameter in children(parameters) {
            if parameter.kind() != "formal_parameter" {
                continue;
            }
            if let Some(type_node) = children(parameter)
                .into_iter()
                .find(|c| PARAMETER_TYPE_KINDS.contains(&c.kind()))
                && let Some(name) = self.type_name(type_node).filter(|n| !n.is_empty())
            {
                types.push(name);
            }
        }
        types
    }

    /// `_handle_method_declaration`.
    fn method(&mut self, node: Node<'_>) {
        let mut name = None;
        let mut return_type = None;
        let mut is_override = false;
        for child in children(node) {
            match child.kind() {
                "identifier" => name = Some(self.text(child)),
                kind if RETURN_TYPE_KINDS.contains(&kind) => return_type = self.type_name(child),
                "modifiers" => {
                    for modifier in children(child) {
                        if modifier.kind() == "marker_annotation"
                            && child_of_kind(modifier, "identifier")
                                .is_some_and(|i| self.source.text(i) == "Override")
                        {
                            is_override = true;
                        }
                    }
                }
                _ => {}
            }
        }
        let Some(name) = name.filter(|n| !n.is_empty()) else {
            return;
        };
        let formal_parameters = child_of_kind(node, "formal_parameters");
        let parent = self.parent();
        let qualified = self.qualified(&name);

        let mut symbol = self.symbol(&name, SymbolType::Method, Scope::Function, node);
        symbol.parent_symbol.clone_from(&parent);
        symbol.full_name = Some(qualified.clone());
        symbol.return_type.clone_from(&return_type);
        symbol.parameter_types = formal_parameters
            .map(|p| self.parameter_types(p))
            .unwrap_or_default();
        symbol.source_text = limits::kept_text(node.byte_range().len(), || self.text(node));
        if is_override {
            symbol
                .metadata
                .insert("is_override".to_owned(), Value::Bool(true));
        }
        self.symbols.push(symbol);

        // `@Override` against the first `inheritance` edge (so far in this
        // file) out of the enclosing scope path. Python quirk: an override
        // of an interface method finds none.
        if is_override
            && let Some(parent) = parent.as_deref().filter(|p| !p.is_empty())
            && let Some(superclass) = self
                .relationships
                .iter()
                .find(|r| {
                    r.relationship_type == RelationshipType::Inheritance
                        && r.source_symbol == parent
                })
                .map(|r| r.target_symbol.clone())
        {
            let mut relationship = self.relationship(
                qualified.clone(),
                format!("{superclass}.{name}"),
                RelationshipType::Overrides,
            );
            relationship.annotations.insert(
                "override_annotation".to_owned(),
                Value::String("@Override".to_owned()),
            );
            self.relationships.push(relationship);
        }

        if let Some(return_type) = return_type
            .as_deref()
            .filter(|t| !t.is_empty() && *t != "void" && !is_builtin_type(t))
        {
            let relationship = self.relationship(
                qualified.clone(),
                return_type.to_owned(),
                RelationshipType::References,
            );
            self.relationships.push(relationship);
        }
        // Python quirk: emitted even when the return type itself is built in
        // (`List<Owner>` references `Owner`).
        if let Some(generic) = child_of_kind(node, "generic_type") {
            for parameter in self.generic_parameters(generic) {
                let relationship =
                    self.relationship(qualified.clone(), parameter, RelationshipType::References);
                self.relationships.push(relationship);
            }
        }
        if let Some(parameters) = formal_parameters {
            self.method_parameters(parameters, &qualified);
        }
        self.visit_in_scope(node, name);
    }

    /// `_handle_method_parameters`: a `references` edge per typed formal
    /// parameter — built-in types included, unlike every other edge — and
    /// one per user type in its first generic type.
    fn method_parameters(&mut self, parameters: Node<'_>, method: &str) {
        for parameter in children(parameters) {
            if parameter.kind() != "formal_parameter" {
                continue;
            }
            let mut param_type = None;
            let mut param_name = None;
            for child in children(parameter) {
                if TYPE_KINDS.contains(&child.kind()) {
                    param_type = self.type_name(child);
                } else if child.kind() == "identifier" {
                    param_name = Some(self.text(child));
                }
            }
            if let Some(param_type) = param_type.filter(|t| !t.is_empty()) {
                let mut relationship =
                    self.relationship(method.to_owned(), param_type, RelationshipType::References);
                relationship
                    .annotations
                    .insert("context".to_owned(), Value::String("parameter".to_owned()));
                relationship.annotations.insert(
                    "param_name".to_owned(),
                    Value::String(
                        param_name
                            .filter(|n| !n.is_empty())
                            .unwrap_or_else(|| "unknown".to_owned()),
                    ),
                );
                self.relationships.push(relationship);
            }
            if let Some(generic) = child_of_kind(parameter, "generic_type") {
                for generic_type in self.generic_parameters(generic) {
                    let mut relationship = self.relationship(
                        method.to_owned(),
                        generic_type,
                        RelationshipType::References,
                    );
                    relationship.annotations.insert(
                        "context".to_owned(),
                        Value::String("generic_parameter".to_owned()),
                    );
                    self.relationships.push(relationship);
                }
            }
        }
    }

    /// `_handle_constructor_declaration`: a constructor named after the
    /// innermost scope (the enclosing type), qualified as `Type.Type`; its
    /// body is visited in the scope `<init>`.
    fn constructor(&mut self, node: Node<'_>) {
        let Some(name) = self.scope.last().cloned() else {
            return;
        };
        let mut symbol = self.symbol(&name, SymbolType::Constructor, Scope::Function, node);
        symbol.parent_symbol = self.parent();
        symbol.full_name = Some(self.qualified(&name));
        symbol.parameter_types = child_of_kind(node, "formal_parameters")
            .map(|p| self.parameter_types(p))
            .unwrap_or_default();
        symbol.source_text = limits::kept_text(node.byte_range().len(), || self.text(node));
        self.symbols.push(symbol);
        self.visit_in_scope(node, "<init>".to_owned());
    }

    /// `_handle_compact_constructor_declaration`. Python quirk: no parent,
    /// full name, parameter types or source text.
    fn compact_constructor(&mut self, node: Node<'_>) {
        let Some(name) = self.scope.last().cloned() else {
            return;
        };
        let symbol = self.symbol(&name, SymbolType::Constructor, Scope::Function, node);
        self.symbols.push(symbol);
        self.visit_in_scope(node, "<init>".to_owned());
    }

    /// `_handle_field_declaration`. Python quirk: `int a, b;` is ONE field
    /// named after the LAST declarator.
    fn field(&mut self, node: Node<'_>) {
        let mut field_type = None;
        let mut field_name = None;
        for child in children(node) {
            if TYPE_KINDS.contains(&child.kind()) {
                field_type = self.type_name(child);
            } else if child.kind() == "variable_declarator"
                && let Some(identifier) = child_of_kind(child, "identifier")
            {
                field_name = Some(self.text(identifier));
            }
        }
        if let Some(field_name) = field_name.filter(|n| !n.is_empty()) {
            let qualified = self.qualified(&field_name);
            let mut symbol = self.symbol(&field_name, SymbolType::Field, Scope::Class, node);
            symbol.parent_symbol = self.parent();
            symbol.full_name = Some(qualified.clone());
            symbol.source_text = limits::kept_text(node.byte_range().len(), || self.text(node));
            self.symbols.push(symbol);
            if let Some(field_type) = field_type.filter(|t| !t.is_empty()) {
                self.field_types
                    .insert(field_name.clone(), field_type.clone());
                if !is_builtin_type(&field_type) {
                    let rel_type = self.field_relationship_type(&field_type, node);
                    let relationship =
                        self.relationship(qualified.clone(), field_type.clone(), rel_type);
                    self.relationships.push(relationship);
                }
                let optional = field_type == "Optional";
                if let Some(type_node) = children(node)
                    .into_iter()
                    .find(|c| TYPE_KINDS.contains(&c.kind()))
                    && type_node.kind() == "generic_type"
                {
                    for parameter in self.generic_parameters(type_node) {
                        let rel_type = if is_builtin_type(&parameter) {
                            RelationshipType::References
                        } else if optional {
                            RelationshipType::Aggregation
                        } else {
                            RelationshipType::Composition
                        };
                        let relationship =
                            self.relationship(qualified.clone(), parameter, rel_type);
                        self.relationships.push(relationship);
                    }
                }
            }
        }
        self.visit_children(node);
    }

    /// `_determine_field_relationship_type` for a (non-built-in) field type.
    /// The type is already a bare base name, so the generic unwrapping the
    /// Python method starts with never applies.
    fn field_relationship_type(&self, field_type: &str, node: Node<'_>) -> RelationshipType {
        const INTERFACE_HINTS: &[&str] = &[
            "Interface",
            "Service",
            "Repository",
            "Controller",
            "Validator",
        ];
        const BASIC: &[&str] = &[
            "String",
            "Integer",
            "Long",
            "Double",
            "Float",
            "Boolean",
            "Date",
            "LocalDateTime",
            "LocalDate",
            "UUID",
        ];
        if INTERFACE_HINTS.iter().any(|h| field_type.contains(h)) || BASIC.contains(&field_type) {
            return RelationshipType::References;
        }
        // Only the FIRST `modifiers` child is read.
        let nullable = child_of_kind(node, "modifiers").is_some_and(|modifiers| {
            children(modifiers).into_iter().any(|m| {
                m.kind() == "marker_annotation"
                    && child_of_kind(m, "identifier")
                        .is_some_and(|i| self.source.text(i) == "Nullable")
            })
        });
        if field_type.starts_with("Optional") || nullable {
            RelationshipType::Aggregation
        } else {
            RelationshipType::Composition
        }
    }

    /// `_handle_local_variable_declaration`: track each declarator's type,
    /// and a `references` edge per user type.
    fn local_variable(&mut self, node: Node<'_>) {
        if let Some(type_node) = children(node)
            .into_iter()
            .find(|c| TYPE_KINDS.contains(&c.kind()))
            && let Some(variable_type) = self.type_name(type_node).filter(|t| !t.is_empty())
        {
            for declarator in children(node) {
                if declarator.kind() == "variable_declarator" {
                    self.declarator(declarator, &variable_type, type_node);
                }
            }
        }
        self.visit_children(node);
    }

    /// `_handle_variable_declarator_with_type`. Python quirk: the edge's
    /// source is `_get_current_method()`, which qualifies the innermost
    /// scope name AGAIN (`pkg.A.run.run`).
    fn declarator(&mut self, node: Node<'_>, variable_type: &str, type_node: Node<'_>) {
        let Some(identifier) = child_of_kind(node, "identifier") else {
            return;
        };
        let name = self.text(identifier);
        if name.is_empty() {
            return;
        }
        self.variable_types
            .insert(name.clone(), variable_type.to_owned());
        let source = match self.scope.last() {
            Some(innermost) => self.qualified(innermost),
            None => self.qualified(&name),
        };
        if !is_builtin_type(variable_type) {
            let relationship = self.relationship(
                source.clone(),
                variable_type.to_owned(),
                RelationshipType::References,
            );
            self.relationships.push(relationship);
        }
        if type_node.kind() == "generic_type" {
            for parameter in self.generic_parameters(type_node) {
                let relationship =
                    self.relationship(source.clone(), parameter, RelationshipType::References);
                self.relationships.push(relationship);
            }
        }
    }

    /// `_handle_method_invocation`: a `calls` edge from the scope path.
    /// With a receiver identifier whose type is a known field or local, the
    /// target is `Type.method`; otherwise the bare method name. Python
    /// quirk: a receiver that is not a plain identifier (`this.x.m()`,
    /// `a.b().m()`) leaves one identifier, the method, so the call is bare.
    fn method_invocation(&mut self, node: Node<'_>) {
        let identifiers: Vec<Node<'_>> = children(node)
            .into_iter()
            .filter(|c| c.kind() == "identifier")
            .collect();
        let (object, method) = match identifiers.as_slice() {
            [] => (None, None),
            [method] => (None, Some(self.text(*method))),
            [object, method, ..] => (Some(self.text(*object)), Some(self.text(*method))),
        };
        if let Some(method) = method.filter(|m| !m.is_empty()) {
            let target = match object.and_then(|o| self.object_type(&o)) {
                Some(object_type) => format!("{object_type}.{method}"),
                None => method,
            };
            let relationship =
                self.relationship(self.scope_or_file(), target, RelationshipType::Calls);
            self.relationships.push(relationship);
        }
        self.visit_children(node);
    }

    /// `_resolve_object_type`: a field's type, else a local's (the
    /// parameter table Python consults third is never filled).
    fn object_type(&self, object: &str) -> Option<String> {
        self.field_types
            .get(object)
            .or_else(|| self.variable_types.get(object))
            .map(|t| t.rsplit('.').next().unwrap_or(t).to_owned())
    }

    /// `_handle_object_creation`: a `creates` edge (confidence 0.95) to the
    /// instantiated type. Python quirk: only a bare `type_identifier` —
    /// `new ArrayList<>()` and `new a.B()` create nothing.
    fn object_creation(&mut self, node: Node<'_>) {
        if let Some(type_node) = children(node)
            .into_iter()
            .find(|c| matches!(c.kind(), "type_identifier" | "identifier"))
        {
            let type_name = self.text(type_node);
            if !type_name.is_empty() {
                let mut relationship =
                    self.relationship(self.scope_or_file(), type_name, RelationshipType::Creates);
                relationship.confidence = 0.95;
                relationship
                    .annotations
                    .insert("creation_type".to_owned(), Value::String("new".to_owned()));
                self.relationships.push(relationship);
            }
        }
        self.visit_children(node);
    }
}

/// `_extract_defines_relationships`: a `defines` edge from each class or
/// interface to every METHOD and FIELD whose parent is its full name (or,
/// when that finds none, its bare name). Constructors are `constructor`
/// symbols and get none; enums, records-as-enums and annotations define
/// nothing.
fn defines_relationships(symbols: &[Symbol], file_path: &str) -> Vec<Relationship> {
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
        let full_name = container.full_name.as_deref().unwrap_or_default();
        let members = by_parent
            .get(full_name)
            .filter(|m| !m.is_empty())
            .or_else(|| by_parent.get(container.name.as_str()))
            .map(Vec::as_slice)
            .unwrap_or_default();
        for member in members {
            if !matches!(member.symbol_type, SymbolType::Method | SymbolType::Field) {
                continue;
            }
            let is_method = member.symbol_type == SymbolType::Method;
            let member_type = member_type(member);
            let params = if is_method {
                member.parameter_types.join(", ")
            } else {
                String::new()
            };
            let member_full = member.full_name.clone().unwrap_or_default();
            let target = if member_type == "constructor" {
                format!("{full_name}.<init>({params})")
            } else if members
                .iter()
                .filter(|m| m.symbol_type == SymbolType::Method && m.name == member.name)
                .count()
                > 1
            {
                // Python quirk: a FIELD sharing its name with two methods is
                // "overloaded" too, and gets `name()`.
                format!("{member_full}({params})")
            } else {
                member_full
            };
            let modifiers = modifiers(member);
            let has = |m: &str| modifiers.contains(&m);
            let visibility = if has("public") {
                "public"
            } else if has("private") {
                "private"
            } else if has("protected") {
                "protected"
            } else {
                "package"
            };
            let mut annotations = Map::new();
            annotations.insert(
                "member_type".to_owned(),
                Value::String(member_type.to_owned()),
            );
            annotations.insert("is_static".to_owned(), Value::Bool(has("static")));
            annotations.insert("is_abstract".to_owned(), Value::Bool(has("abstract")));
            annotations.insert(
                "visibility".to_owned(),
                Value::String(visibility.to_owned()),
            );
            if is_method {
                annotations.insert(
                    "parameter_types".to_owned(),
                    Value::Array(
                        member
                            .parameter_types
                            .iter()
                            .cloned()
                            .map(Value::String)
                            .collect(),
                    ),
                );
                annotations.insert(
                    "signature".to_owned(),
                    Value::String(format!("{}({params})", member.name)),
                );
            }
            let mut relationship =
                Relationship::new(full_name, target, RelationshipType::Defines, file_path);
            relationship.target_file = Some(file_path.to_owned());
            relationship.annotations = annotations;
            relationships.push(relationship);
        }
    }
    relationships
}

/// `_get_member_type`. Python quirk: a METHOD is a "constructor" when its
/// name equals its parent or its full name contains `<init>` — a method of
/// a local class declared inside a constructor.
fn member_type(symbol: &Symbol) -> &'static str {
    match symbol.symbol_type {
        SymbolType::Field => "field",
        SymbolType::Method => {
            if symbol.parent_symbol.as_deref() == Some(symbol.name.as_str())
                || symbol
                    .full_name
                    .as_deref()
                    .is_some_and(|f| f.contains("<init>"))
            {
                "constructor"
            } else {
                "method"
            }
        }
        _ => "unknown",
    }
}

/// `_extract_modifiers`: the modifier keywords found as SUBSTRINGS of the
/// lower-cased first line of the source text. Python quirk: a member whose
/// first line is an annotation has none; `publicKey` reads as `public`.
fn modifiers(symbol: &Symbol) -> Vec<&'static str> {
    const KEYWORDS: &[&str] = &[
        "public",
        "private",
        "protected",
        "static",
        "final",
        "abstract",
        "synchronized",
        "native",
        "transient",
        "volatile",
    ];
    let Some(text) = symbol.source_text.as_deref().filter(|t| !t.is_empty()) else {
        return Vec::new();
    };
    let first = text.split('\n').next().unwrap_or_default().to_lowercase();
    KEYWORDS
        .iter()
        .copied()
        .filter(|k| first.contains(k))
        .collect()
}

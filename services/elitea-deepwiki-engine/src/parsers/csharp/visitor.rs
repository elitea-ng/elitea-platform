//! One C# file: the Python `CSharpVisitorParser.parse_file`.
//!
//! Every rule here is the Python visitor's, quirks included, because the
//! graph gate compares this output with the Python output field by field.
//! Each quirk the port keeps on purpose is marked "Python quirk". Node kinds
//! are those of the grammar `tree_sitter_language_pack` 1.16.1 builds
//! (`tree-sitter-c-sharp` at rev `9150f7d`, pinned in `Cargo.toml`).
//!
//! What the Python parser never fills, this one does not either: no
//! `docstring` (XML doc comments are not read), `signature`, `comments`,
//! `imports` or `exports`, and no relationship has a `source_range`. A
//! Python `metadata=None` or `annotations=None` is an empty map here; the
//! graph builder reads both as `{}`.

use crate::parsers::java::source::Source;
use crate::parsers::limits;
use crate::parsers::model::{
    ParseResult, Relationship, RelationshipType, Scope, Symbol, SymbolType,
};
use serde_json::{Map, Value};
use std::collections::HashMap;
use tree_sitter::Node;

/// The type kinds a parameter, field or indexer type is read from
/// (`identifier` included: a user type).
const TYPE_KINDS: &[&str] = &[
    "predefined_type",
    "identifier",
    "qualified_name",
    "generic_name",
    "nullable_type",
    "array_type",
    "tuple_type",
];

/// The type kinds a method, property or operator return type is read from
/// directly; an `identifier` there needs a look at its next sibling first.
const TYPE_CANDIDATES: &[&str] = &[
    "predefined_type",
    "qualified_name",
    "generic_name",
    "nullable_type",
    "array_type",
    "tuple_type",
];

/// `_CSHARP_BUILTIN_TYPES`.
const BUILTIN_TYPES: &[&str] = &[
    "byte",
    "sbyte",
    "short",
    "ushort",
    "int",
    "uint",
    "long",
    "ulong",
    "float",
    "double",
    "decimal",
    "char",
    "bool",
    "string",
    "object",
    "void",
    "nint",
    "nuint",
    "dynamic",
    "var",
    "Byte",
    "SByte",
    "Int16",
    "UInt16",
    "Int32",
    "UInt32",
    "Int64",
    "UInt64",
    "Single",
    "Double",
    "Decimal",
    "Char",
    "Boolean",
    "String",
    "Object",
    "Void",
    "Task",
    "ValueTask",
    "Action",
    "Func",
    "Predicate",
    "EventHandler",
    "IDisposable",
    "IAsyncDisposable",
    "IEnumerable",
    "IEnumerator",
    "ICollection",
    "IList",
    "IDictionary",
    "IReadOnlyList",
    "IReadOnlyCollection",
    "IReadOnlyDictionary",
    "ISet",
    "IComparer",
    "IEqualityComparer",
    "List",
    "Dictionary",
    "HashSet",
    "SortedSet",
    "Queue",
    "Stack",
    "LinkedList",
    "SortedDictionary",
    "SortedList",
    "ConcurrentDictionary",
    "ConcurrentBag",
    "ConcurrentQueue",
    "ConcurrentStack",
    "Array",
    "Span",
    "ReadOnlySpan",
    "Memory",
    "ReadOnlyMemory",
    "Nullable",
    "Tuple",
    "ValueTuple",
    "KeyValuePair",
    "Type",
    "Enum",
    "Attribute",
    "Exception",
    "EventArgs",
    "Delegate",
    "IObservable",
    "IObserver",
    "DateTime",
    "DateTimeOffset",
    "TimeSpan",
    "DateOnly",
    "TimeOnly",
    "Guid",
    "Uri",
    "Version",
    "BigInteger",
    "IOException",
    "ArgumentException",
    "ArgumentNullException",
    "InvalidOperationException",
    "NotImplementedException",
    "NotSupportedException",
    "NullReferenceException",
    "IndexOutOfRangeException",
    "OverflowException",
    "CancellationToken",
    "CancellationTokenSource",
    "StringBuilder",
    "Regex",
    "Match",
    "Stream",
    "MemoryStream",
    "FileStream",
    "StreamReader",
    "StreamWriter",
    "HttpClient",
    "HttpRequestMessage",
    "HttpResponseMessage",
    "JsonSerializer",
    "JsonElement",
    "JsonDocument",
    "ILogger",
    "ILoggerFactory",
    "IServiceProvider",
    "IServiceCollection",
    "IConfiguration",
    "IOptions",
    "IOptionsMonitor",
    "IOptionsSnapshot",
];

/// Generic constraints that are keywords, not types.
const CONSTRAINT_KEYWORDS: &[&str] = &[
    "class",
    "struct",
    "new()",
    "notnull",
    "unmanaged",
    "default",
];

/// Python's `str.isspace` for one character (`char::is_whitespace` lacks
/// the four information separators).
fn py_space(c: char) -> bool {
    c.is_whitespace() || ('\u{1c}'..='\u{1f}').contains(&c)
}

/// `str.strip()`.
fn py_strip(text: &str) -> &str {
    text.trim_matches(py_space)
}

/// `_is_builtin_type`. An empty name counts as built in.
pub(super) fn is_builtin_type(name: &str) -> bool {
    if name.is_empty() {
        return true;
    }
    let base = first_part(first_part(first_part(name, '<'), '['), '?');
    let mut base = py_strip(base);
    if base.contains('.') {
        if ["System.", "Microsoft.", "Newtonsoft.", "NLog."]
            .iter()
            .any(|p| base.starts_with(p))
        {
            return true;
        }
        base = base.rsplit('.').next().unwrap_or(base);
    }
    BUILTIN_TYPES.contains(&base)
}

/// `text.split(separator)[0]`.
fn first_part(text: &str, separator: char) -> &str {
    text.split(separator).next().unwrap_or(text)
}

/// `_is_interface_name`: `I` then an upper-case letter (the C# naming
/// convention; the only way the parser tells an interface from a class in
/// a base list).
pub(super) fn is_interface_name(name: &str) -> bool {
    let mut chars = first_part(name, '<').chars();
    chars.next() == Some('I') && chars.next().is_some_and(char::is_uppercase)
}

/// `_determine_field_relationship_type`: a nullable type (or `Nullable<…>`)
/// is aggregation, anything else composition.
fn field_relationship_type(type_name: &str, nullable: bool) -> RelationshipType {
    let base = py_strip(first_part(first_part(type_name, '<'), '['));
    if nullable || base == "Nullable" {
        RelationshipType::Aggregation
    } else {
        RelationshipType::Composition
    }
}

/// Parse one decoded C# source into the per-file result. Cross-file
/// resolution happens later, in `super`.
pub(super) fn parse_source(file_path: &str, source: &Source) -> ParseResult {
    let mut parser = tree_sitter::Parser::new();
    let language: tree_sitter::Language = tree_sitter_c_sharp::LANGUAGE.into();
    if parser.set_language(&language).is_err() {
        return failed(file_path, "Tree-sitter C# not available");
    }
    let Some(tree) = parser.parse(source.bytes(), None) else {
        return failed(file_path, "Tree-sitter C# parser returned no tree");
    };
    if limits::too_deep(tree.root_node()) {
        return failed(file_path, limits::RECURSION_ERROR);
    }
    let mut visitor = Visitor::new(file_path, source);
    visitor.visit(tree.root_node());
    let defines = defines_relationships(&visitor.symbols, file_path);
    visitor.relationships.extend(defines);
    ParseResult {
        symbols: visitor.symbols,
        relationships: visitor.relationships,
        ..ParseResult::new(file_path, "csharp")
    }
}

/// A result that carries only an error, as every Python failure path builds.
pub(super) fn failed(file_path: &str, error: impl Into<String>) -> ParseResult {
    let mut result = ParseResult::new(file_path, "csharp");
    result.errors.push(error.into());
    result
}

/// `Path(file).stem`.
fn file_stem(file_path: &str) -> String {
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

/// The modifier flags `_extract_modifiers_from_node` reads, in its dict
/// order (which is the order of the `metadata` keys).
const MODIFIER_FLAGS: &[(&str, &str)] = &[
    ("static", "is_static"),
    ("abstract", "is_abstract"),
    ("sealed", "is_sealed"),
    ("virtual", "is_virtual"),
    ("override", "is_override"),
    ("readonly", "is_readonly"),
    ("const", "is_const"),
    ("async", "is_async"),
    ("partial", "is_partial"),
    ("extern", "is_extern"),
    ("new", "is_new"),
    ("volatile", "is_volatile"),
    ("unsafe", "is_unsafe"),
];

/// `_extract_modifiers_from_node`: the `modifier` children's keywords.
struct Modifiers {
    /// One flag per [`MODIFIER_FLAGS`] entry.
    flags: [bool; MODIFIER_FLAGS.len()],
    /// The last access keyword; C#'s default is `internal`. Python quirk:
    /// `protected internal` reads as `internal`, `private protected` as
    /// `protected`.
    visibility: &'static str,
}

impl Modifiers {
    fn has(&self, flag: &str) -> bool {
        MODIFIER_FLAGS
            .iter()
            .position(|(_, key)| *key == flag)
            .is_some_and(|i| self.flags[i])
    }

    /// `{k: v for k, v in mods.items() if v and k != 'visibility'}`.
    fn metadata(&self) -> Map<String, Value> {
        MODIFIER_FLAGS
            .iter()
            .zip(self.flags)
            .filter(|(_, set)| *set)
            .map(|((_, key), _)| ((*key).to_owned(), Value::Bool(true)))
            .collect()
    }
}

/// Per-file visitor state (the Python parser's `_current_*` fields).
struct Visitor<'s> {
    file: &'s str,
    source: &'s Source,
    symbols: Vec<Symbol>,
    relationships: Vec<Relationship>,
    /// Scope names: namespaces, types, members, `<init>`. A file-scoped
    /// namespace is pushed and never popped.
    scope: Vec<String>,
    /// Field name → type; file-wide, so a field of one class resolves a
    /// call in another class of the same file (Python quirk).
    field_types: HashMap<String, String>,
    /// Parameter name → type for the method being visited. Python's
    /// `_variable_types`, consulted between the two, is never filled.
    parameter_types: HashMap<String, String>,
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
            parameter_types: HashMap::new(),
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
        let scope = self.scope_path();
        if scope.is_empty() {
            name.to_owned()
        } else {
            format!("{scope}.{name}")
        }
    }

    /// `scope path or Path(file).stem`: the source of an import, call or
    /// creation outside any scope.
    fn scope_or_stem(&self) -> String {
        let scope = self.scope_path();
        if scope.is_empty() {
            file_stem(self.file)
        } else {
            scope
        }
    }

    /// A relationship whose `target_file` is this file, as every Python
    /// constructor call here passes.
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

    fn push_relationship(&mut self, source: String, target: String, rel_type: RelationshipType) {
        let relationship = self.relationship(source, target, rel_type);
        self.relationships.push(relationship);
    }

    /// A symbol with the fields every Python constructor call here sets:
    /// range and source text of `node`, the scope path as parent, the
    /// qualified name and the visibility.
    fn symbol(
        &self,
        name: &str,
        symbol_type: SymbolType,
        scope: Scope,
        node: Node<'_>,
        visibility: &str,
    ) -> Symbol {
        let mut symbol = Symbol::new(name, symbol_type, scope, self.source.range(node), self.file);
        symbol.parent_symbol = self.parent();
        symbol.full_name = Some(self.qualified(name));
        symbol.visibility = Some(visibility.to_owned());
        symbol.source_text = self.node_source(node);
        symbol
    }

    /// `_extract_node_source`: `None` only for an empty file.
    fn node_source(&self, node: Node<'_>) -> Option<String> {
        if self.source.bytes().is_empty() {
            return None;
        }
        limits::kept_text(node.byte_range().len(), || self.text(node))
    }

    /// `visit_node`: dispatch by kind, else descend.
    fn visit(&mut self, node: Node<'_>) {
        match node.kind() {
            "namespace_declaration" => self.namespace(node),
            "file_scoped_namespace_declaration" => self.file_scoped_namespace(node),
            "using_directive" => self.using(node),
            "class_declaration" => self.class(node),
            "struct_declaration" => self.structure(node),
            "interface_declaration" => self.interface(node),
            "enum_declaration" => self.enumeration(node),
            "record_declaration" => self.record(node),
            "delegate_declaration" => self.delegate(node),
            "method_declaration" => self.method(node),
            "constructor_declaration" => self.constructor(node),
            "destructor_declaration" => self.destructor(node),
            "property_declaration" => self.property(node),
            "field_declaration" => self.field(node),
            "event_field_declaration" => self.event_field(node),
            "indexer_declaration" => self.indexer(node),
            "operator_declaration" => self.operator(node),
            "invocation_expression" => self.invocation(node),
            "object_creation_expression" => self.object_creation(node),
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

    // ------------------------------------------------------------------
    // Node helpers
    // ------------------------------------------------------------------

    /// The text of the first `identifier` child (Python takes the first
    /// even when it is an empty MISSING node, then gives up on the
    /// declaration).
    fn first_identifier(&self, node: Node<'_>) -> Option<String> {
        child_of_kind(node, "identifier").map(|n| self.text(n))
    }

    /// The text of the first `identifier` or `qualified_name` child: a
    /// namespace name.
    fn namespace_name(&self, node: Node<'_>) -> Option<String> {
        children(node)
            .into_iter()
            .find(|c| matches!(c.kind(), "identifier" | "qualified_name"))
            .map(|n| self.text(n))
            .filter(|n| !n.is_empty())
    }

    fn modifiers(&self, node: Node<'_>) -> Modifiers {
        let mut modifiers = Modifiers {
            flags: [false; MODIFIER_FLAGS.len()],
            visibility: "internal",
        };
        for child in children(node) {
            if child.kind() != "modifier" {
                continue;
            }
            let text = self.text(child);
            match py_strip(&text) {
                "public" => modifiers.visibility = "public",
                "private" => modifiers.visibility = "private",
                "protected" => modifiers.visibility = "protected",
                "internal" => modifiers.visibility = "internal",
                keyword => {
                    if let Some(i) = MODIFIER_FLAGS.iter().position(|(k, _)| *k == keyword) {
                        modifiers.flags[i] = true;
                    }
                }
            }
        }
        modifiers
    }

    /// `_extract_type_name`. Python quirk: a generic type is its bare name
    /// (`List<Owner>` → `List`), a tuple is `ValueTuple`, a nullable or
    /// array type its element's name; any other kind is its text.
    fn type_name(&self, node: Node<'_>) -> String {
        match node.kind() {
            "nullable_type" => {
                if let Some(inner) = children(node).into_iter().find(|c| c.kind() != "?") {
                    return self.type_name(inner);
                }
            }
            "generic_name" => {
                if let Some(id) = child_of_kind(node, "identifier") {
                    return self.text(id);
                }
            }
            "array_type" => {
                if let Some(inner) = children(node).into_iter().find(|c| {
                    matches!(
                        c.kind(),
                        "predefined_type"
                            | "identifier"
                            | "qualified_name"
                            | "generic_name"
                            | "nullable_type"
                    )
                }) {
                    return self.type_name(inner);
                }
            }
            "tuple_type" => return "ValueTuple".to_owned(),
            _ => {}
        }
        self.text(node)
    }

    /// The type of the first child among [`TYPE_KINDS`]: `_get_parameter_type`
    /// and `_extract_return_type`.
    fn first_type(&self, node: Node<'_>) -> Option<String> {
        children(node)
            .into_iter()
            .find(|c| TYPE_KINDS.contains(&c.kind()))
            .map(|c| self.type_name(c))
    }

    /// `_get_parameter_name`: the LAST identifier child (the first is the
    /// type when the type is a bare identifier).
    fn parameter_name(&self, parameter: Node<'_>) -> Option<String> {
        children(parameter)
            .into_iter()
            .rfind(|c| c.kind() == "identifier")
            .map(|n| self.text(n))
    }

    /// The `parameter` children of a parameter list.
    fn parameters(list: Node<'_>) -> Vec<Node<'_>> {
        children(list)
            .into_iter()
            .filter(|c| c.kind() == "parameter")
            .collect()
    }

    /// `_extract_parameter_types`: non-empty parameter types, in order.
    fn parameter_types_of(&self, list: Option<Node<'_>>) -> Vec<String> {
        list.map(Self::parameters)
            .unwrap_or_default()
            .into_iter()
            .filter_map(|p| self.first_type(p))
            .filter(|t| !t.is_empty())
            .collect()
    }

    /// `_extract_generic_type_parameters`.
    fn generic_parameters(&self, node: Node<'_>) -> Vec<String> {
        let Some(list) = child_of_kind(node, "type_parameter_list") else {
            return Vec::new();
        };
        children(list)
            .into_iter()
            .filter(|c| c.kind() == "type_parameter")
            .filter_map(|c| child_of_kind(c, "identifier"))
            .map(|n| self.text(n))
            .collect()
    }

    /// `_extract_generic_constraints`: per `where` clause, the parameter
    /// (the last identifier) and each constraint's text.
    fn generic_constraints(&self, node: Node<'_>) -> Vec<(String, Vec<String>)> {
        children(node)
            .into_iter()
            .filter(|c| c.kind() == "type_parameter_constraints_clause")
            .map(|clause| {
                let mut parameter = String::new();
                let mut constraints = Vec::new();
                for child in children(clause) {
                    match child.kind() {
                        "identifier" => parameter = self.text(child),
                        "type_parameter_constraint" => constraints.push(self.text(child)),
                        _ => {}
                    }
                }
                (parameter, constraints)
            })
            .collect()
    }

    /// `_extract_attributes`: the name of every attribute whose name is a
    /// bare identifier (`[System.Obsolete]` and `[Foo<T>]` give none).
    fn attributes(&self, node: Node<'_>) -> Vec<String> {
        children(node)
            .into_iter()
            .filter(|c| c.kind() == "attribute_list")
            .flat_map(children)
            .filter(|c| c.kind() == "attribute")
            .filter_map(|c| child_of_kind(c, "identifier"))
            .map(|n| self.text(n))
            .collect()
    }

    /// `_emit_annotates`: an `annotates` edge from each attribute name.
    fn annotates(&mut self, attributes: Vec<String>, target: &str) {
        for attribute in attributes {
            self.push_relationship(attribute, target.to_owned(), RelationshipType::Annotates);
        }
    }

    /// `_find_superclass`: the target of the file's first `inheritance`
    /// edge from `class` so far.
    fn superclass(&self, class: &str) -> Option<String> {
        self.relationships
            .iter()
            .find(|r| {
                r.relationship_type == RelationshipType::Inheritance && r.source_symbol == class
            })
            .map(|r| r.target_symbol.clone())
    }

    // ------------------------------------------------------------------
    // Namespaces and usings
    // ------------------------------------------------------------------

    /// `_handle_namespace_declaration`: the body in the namespace's scope.
    fn namespace(&mut self, node: Node<'_>) {
        match self.namespace_name(node) {
            Some(name) => self.visit_in_scope(node, name),
            None => self.visit_children(node),
        }
    }

    /// `_handle_file_scoped_namespace`: push the name for the rest of the
    /// file. The declarations after it are its SIBLINGS, so it visits
    /// nothing itself.
    fn file_scoped_namespace(&mut self, node: Node<'_>) {
        if let Some(name) = self.namespace_name(node) {
            self.scope.push(name);
        }
    }

    /// `_handle_using_directive`: one `imports` edge to the imported name.
    /// Python quirk: an alias of a generic type (`using X = List<int>;`)
    /// imports nothing, and an alias of a simple name imports that name.
    fn using(&mut self, node: Node<'_>) {
        let mut global = false;
        let mut alias = None;
        let mut path: Option<String> = None;
        for child in children(node) {
            match child.kind() {
                "global" => global = true,
                "identifier" if path.is_none() => {
                    if child
                        .next_sibling()
                        .is_some_and(|n| self.source.text(n) == "=")
                    {
                        alias = Some(self.text(child));
                    } else {
                        path = Some(self.text(child));
                    }
                }
                "qualified_name" => path = Some(self.text(child)),
                _ => {}
            }
        }
        let Some(path) = path.filter(|p| !p.is_empty()) else {
            return;
        };
        let mut relationship =
            self.relationship(self.scope_or_stem(), path, RelationshipType::Imports);
        if global {
            relationship
                .annotations
                .insert("global".to_owned(), Value::Bool(true));
        }
        if let Some(alias) = alias.filter(|a| !a.is_empty()) {
            relationship
                .annotations
                .insert("alias".to_owned(), Value::String(alias));
        }
        self.relationships.push(relationship);
    }

    // ------------------------------------------------------------------
    // Type declarations
    // ------------------------------------------------------------------

    /// `_handle_class_declaration`.
    fn class(&mut self, node: Node<'_>) {
        let Some(name) = self.first_identifier(node).filter(|n| !n.is_empty()) else {
            return;
        };
        let modifiers = self.modifiers(node);
        let attributes = self.attributes(node);
        let generics = self.generic_parameters(node);
        let constraints = self.generic_constraints(node);
        let mut metadata = modifiers.metadata();
        if !generics.is_empty() {
            metadata.insert("generic_parameters".to_owned(), strings(&generics));
        }
        if !constraints.is_empty() {
            let clauses = constraints
                .iter()
                .map(|(parameter, constraints)| {
                    let mut clause = Map::new();
                    clause.insert("parameter".to_owned(), Value::String(parameter.clone()));
                    clause.insert("constraints".to_owned(), strings(constraints));
                    Value::Object(clause)
                })
                .collect();
            metadata.insert("generic_constraints".to_owned(), Value::Array(clauses));
        }
        let mut symbol = self.symbol(
            &name,
            SymbolType::Class,
            Scope::Class,
            node,
            modifiers.visibility,
        );
        symbol.is_static = modifiers.has("is_static");
        symbol.is_abstract = modifiers.has("is_abstract");
        symbol.metadata = metadata;
        let qualified = symbol.full_name.clone().unwrap_or_default();
        self.symbols.push(symbol);
        self.annotates(attributes, &qualified);
        self.base_list(node, &qualified, BaseKind::Class);
        for constraint in constraints.iter().flat_map(|(_, c)| c) {
            if CONSTRAINT_KEYWORDS.contains(&constraint.as_str()) || is_builtin_type(constraint) {
                continue;
            }
            let mut relationship = self.relationship(
                qualified.clone(),
                constraint.clone(),
                RelationshipType::References,
            );
            relationship.annotations.insert(
                "context".to_owned(),
                Value::String("generic_constraint".to_owned()),
            );
            self.relationships.push(relationship);
        }
        self.visit_in_scope(node, name);
    }

    /// `_handle_struct_declaration`: a class without constraints, whose
    /// base types are all implemented.
    fn structure(&mut self, node: Node<'_>) {
        let Some(name) = self.first_identifier(node).filter(|n| !n.is_empty()) else {
            return;
        };
        let modifiers = self.modifiers(node);
        let attributes = self.attributes(node);
        let generics = self.generic_parameters(node);
        let mut metadata = modifiers.metadata();
        if !generics.is_empty() {
            metadata.insert("generic_parameters".to_owned(), strings(&generics));
        }
        let mut symbol = self.symbol(
            &name,
            SymbolType::Struct,
            Scope::Class,
            node,
            modifiers.visibility,
        );
        symbol.is_static = modifiers.has("is_static");
        symbol.metadata = metadata;
        let qualified = symbol.full_name.clone().unwrap_or_default();
        self.symbols.push(symbol);
        self.annotates(attributes, &qualified);
        self.base_list(node, &qualified, BaseKind::Struct);
        self.visit_in_scope(node, name);
    }

    /// `_handle_interface_declaration`: base interfaces are inherited.
    fn interface(&mut self, node: Node<'_>) {
        let Some(name) = self.first_identifier(node).filter(|n| !n.is_empty()) else {
            return;
        };
        let modifiers = self.modifiers(node);
        let attributes = self.attributes(node);
        let generics = self.generic_parameters(node);
        let mut metadata = modifiers.metadata();
        if !generics.is_empty() {
            metadata.insert("generic_parameters".to_owned(), strings(&generics));
        }
        let mut symbol = self.symbol(
            &name,
            SymbolType::Interface,
            Scope::Class,
            node,
            modifiers.visibility,
        );
        symbol.metadata = metadata;
        let qualified = symbol.full_name.clone().unwrap_or_default();
        self.symbols.push(symbol);
        self.annotates(attributes, &qualified);
        self.base_list(node, &qualified, BaseKind::Interface);
        self.visit_in_scope(node, name);
    }

    /// `_handle_enum_declaration`: the enum, then one static public FIELD
    /// per member. Python quirk: the body is not visited, so a call in a
    /// member's value is lost, and the enum has no metadata.
    fn enumeration(&mut self, node: Node<'_>) {
        let Some(name) = self.first_identifier(node).filter(|n| !n.is_empty()) else {
            return;
        };
        let modifiers = self.modifiers(node);
        let attributes = self.attributes(node);
        let symbol = self.symbol(
            &name,
            SymbolType::Enum,
            Scope::Class,
            node,
            modifiers.visibility,
        );
        let qualified = symbol.full_name.clone().unwrap_or_default();
        self.symbols.push(symbol);
        self.annotates(attributes, &qualified);
        self.scope.push(name);
        if let Some(list) = child_of_kind(node, "enum_member_declaration_list") {
            for member in children(list) {
                if member.kind() != "enum_member_declaration" {
                    continue;
                }
                // Python quirk: an empty (MISSING) name still gives a member.
                let Some(member_name) = self.first_identifier(member) else {
                    continue;
                };
                let mut symbol = self.symbol(
                    &member_name,
                    SymbolType::Field,
                    Scope::Class,
                    member,
                    "public",
                );
                symbol.parent_symbol = Some(qualified.clone());
                symbol.is_static = true;
                symbol
                    .metadata
                    .insert("is_enum_member".to_owned(), Value::Bool(true));
                self.symbols.push(symbol);
            }
        }
        self.scope.pop();
    }

    /// `_handle_record_declaration`: a class (a struct for `record struct`)
    /// whose positional parameters are public properties, each composing
    /// its non-built-in type.
    fn record(&mut self, node: Node<'_>) {
        let mut is_struct = false;
        let mut name = None;
        for child in children(node) {
            match child.kind() {
                "struct" => is_struct = true,
                "identifier" => {
                    name = Some(self.text(child));
                    break;
                }
                _ => {}
            }
        }
        let Some(name) = name.filter(|n| !n.is_empty()) else {
            return;
        };
        let modifiers = self.modifiers(node);
        let attributes = self.attributes(node);
        let generics = self.generic_parameters(node);
        let mut metadata = modifiers.metadata();
        metadata.insert("is_record".to_owned(), Value::Bool(true));
        if is_struct {
            metadata.insert("is_record_struct".to_owned(), Value::Bool(true));
        }
        if !generics.is_empty() {
            metadata.insert("generic_parameters".to_owned(), strings(&generics));
        }
        let symbol_type = if is_struct {
            SymbolType::Struct
        } else {
            SymbolType::Class
        };
        // Python quirk: a record is never static or abstract.
        let mut symbol = self.symbol(&name, symbol_type, Scope::Class, node, modifiers.visibility);
        symbol.metadata = metadata;
        let qualified = symbol.full_name.clone().unwrap_or_default();
        self.symbols.push(symbol);
        self.annotates(attributes, &qualified);
        let kind = if is_struct {
            BaseKind::Struct
        } else {
            BaseKind::Class
        };
        self.base_list(node, &qualified, kind);
        self.scope.push(name);
        if let Some(list) = child_of_kind(node, "parameter_list") {
            for parameter in Self::parameters(list) {
                let parameter_type = self.first_type(parameter);
                let Some(parameter_name) = self.parameter_name(parameter).filter(|n| !n.is_empty())
                else {
                    continue;
                };
                let mut symbol = self.symbol(
                    &parameter_name,
                    SymbolType::Property,
                    Scope::Class,
                    parameter,
                    "public",
                );
                symbol.parent_symbol = Some(qualified.clone());
                symbol.return_type.clone_from(&parameter_type);
                symbol
                    .metadata
                    .insert("is_record_parameter".to_owned(), Value::Bool(true));
                let property = symbol.full_name.clone().unwrap_or_default();
                self.symbols.push(symbol);
                if let Some(parameter_type) = parameter_type.filter(|t| !is_builtin_type(t)) {
                    self.push_relationship(property, parameter_type, RelationshipType::Composition);
                }
            }
        }
        self.visit_children(node);
        self.scope.pop();
    }

    /// `_handle_delegate_declaration`: a `TYPE_ALIAS`. After the `delegate`
    /// keyword the first type is the return type and the next identifier
    /// the name; an identifier return type is told from the name by its
    /// next named sibling.
    fn delegate(&mut self, node: Node<'_>) {
        let mut return_type: Option<String> = None;
        let mut name: Option<String> = None;
        let mut after_keyword = false;
        for child in children(node) {
            if child.kind() == "delegate" {
                after_keyword = true;
                continue;
            }
            if !after_keyword {
                continue;
            }
            let kind = child.kind();
            if return_type.is_none() && TYPE_CANDIDATES.contains(&kind) {
                return_type = Some(self.type_name(child));
            } else if return_type.is_none() && kind == "identifier" {
                if child
                    .next_named_sibling()
                    .is_some_and(|n| n.kind() == "parameter_list")
                {
                    name = Some(self.text(child));
                } else {
                    return_type = Some(self.text(child));
                }
            } else if kind == "identifier" && name.is_none() {
                name = Some(self.text(child));
            }
        }
        let Some(name) = name.filter(|n| !n.is_empty()) else {
            return;
        };
        let modifiers = self.modifiers(node);
        let mut symbol = self.symbol(
            &name,
            SymbolType::TypeAlias,
            Scope::Class,
            node,
            modifiers.visibility,
        );
        symbol.return_type = return_type;
        symbol
            .metadata
            .insert("is_delegate".to_owned(), Value::Bool(true));
        self.symbols.push(symbol);
    }

    /// `_extract_base_list`: one edge per base type. For a class the
    /// I-prefix convention decides between implementation and inheritance;
    /// an interface inherits everything, a struct implements everything.
    fn base_list(&mut self, node: Node<'_>, source: &str, kind: BaseKind) {
        let Some(list) = child_of_kind(node, "base_list") else {
            return;
        };
        for child in children(list) {
            if !matches!(
                child.kind(),
                "identifier" | "qualified_name" | "generic_name"
            ) {
                continue;
            }
            let base = self.type_name(child);
            if base.is_empty() {
                continue;
            }
            let implemented = match kind {
                BaseKind::Interface => false,
                BaseKind::Struct => true,
                BaseKind::Class => is_interface_name(&base),
            };
            let rel_type = if implemented {
                RelationshipType::Implementation
            } else {
                RelationshipType::Inheritance
            };
            self.push_relationship(source.to_owned(), base, rel_type);
        }
    }

    // ------------------------------------------------------------------
    // Members
    // ------------------------------------------------------------------

    /// The return type and name of a method: modifiers and attributes
    /// skipped, the first type candidate is the return type, and an
    /// identifier is the return type unless a parameter list follows it.
    fn method_signature(&self, node: Node<'_>) -> (Option<String>, Option<String>) {
        let mut return_type = None;
        let mut found_return = false;
        for child in children(node) {
            let kind = child.kind();
            if matches!(kind, "modifier" | "attribute_list") {
                continue;
            }
            if !found_return && TYPE_CANDIDATES.contains(&kind) {
                return_type = Some(self.type_name(child));
                found_return = true;
            } else if kind == "identifier" {
                if found_return
                    || child
                        .next_named_sibling()
                        .is_some_and(|n| n.kind() == "parameter_list")
                {
                    return (return_type, Some(self.text(child)));
                }
                return_type = Some(self.text(child));
                found_return = true;
            }
        }
        (return_type, None)
    }

    /// `_handle_method_declaration`.
    fn method(&mut self, node: Node<'_>) {
        let modifiers = self.modifiers(node);
        let attributes = self.attributes(node);
        let (return_type, name) = self.method_signature(node);
        let Some(name) = name.filter(|n| !n.is_empty()) else {
            return;
        };
        let list = child_of_kind(node, "parameter_list");
        let parameter_types = self.parameter_types_of(list);
        let parameters = list.map(Self::parameters).unwrap_or_default();
        for &parameter in &parameters {
            if let (Some(t), Some(n)) = (self.first_type(parameter), self.parameter_name(parameter))
                && !t.is_empty()
                && !n.is_empty()
            {
                self.parameter_types.insert(n, t);
            }
        }
        let mut metadata = modifiers.metadata();
        // `_is_extension_method`: the FIRST parameter's text starts with
        // `this ` (Python quirk: `[A] this T x` or `this\tT x` is not one).
        if parameters.first().is_some_and(|p| {
            self.source
                .text(*p)
                .trim_start_matches(py_space)
                .starts_with("this ")
        }) {
            metadata.insert("is_extension".to_owned(), Value::Bool(true));
        }
        let parent = self.parent();
        let mut symbol = self.symbol(
            &name,
            SymbolType::Method,
            Scope::Function,
            node,
            modifiers.visibility,
        );
        symbol.is_static = modifiers.has("is_static");
        symbol.is_abstract = modifiers.has("is_abstract");
        symbol.is_async = modifiers.has("is_async");
        symbol.return_type.clone_from(&return_type);
        symbol.parameter_types = parameter_types;
        symbol.metadata = metadata;
        let qualified = symbol.full_name.clone().unwrap_or_default();
        self.symbols.push(symbol);
        self.annotates(attributes, &qualified);
        if modifiers.has("is_override")
            && let Some(parent) = parent.filter(|p| !p.is_empty())
            && let Some(superclass) = self.superclass(&parent)
        {
            let mut relationship = self.relationship(
                qualified.clone(),
                format!("{superclass}.{name}"),
                RelationshipType::Overrides,
            );
            relationship
                .annotations
                .insert("override_keyword".to_owned(), Value::Bool(true));
            self.relationships.push(relationship);
        }
        if let Some(return_type) = return_type.filter(|t| t != "void" && !is_builtin_type(t)) {
            self.push_relationship(qualified.clone(), return_type, RelationshipType::References);
        }
        for &parameter in &parameters {
            let Some(parameter_type) = self.first_type(parameter) else {
                continue;
            };
            if is_builtin_type(&parameter_type) {
                continue;
            }
            let parameter_name = self
                .parameter_name(parameter)
                .filter(|n| !n.is_empty())
                .unwrap_or_else(|| "unknown".to_owned());
            let mut relationship = self.relationship(
                qualified.clone(),
                parameter_type,
                RelationshipType::References,
            );
            relationship
                .annotations
                .insert("context".to_owned(), Value::String("parameter".to_owned()));
            relationship
                .annotations
                .insert("param_name".to_owned(), Value::String(parameter_name));
            self.relationships.push(relationship);
        }
        self.visit_in_scope(node, name);
        for &parameter in &parameters {
            if let Some(n) = self.parameter_name(parameter).filter(|n| !n.is_empty()) {
                self.parameter_types.remove(&n);
            }
        }
    }

    /// `_handle_constructor_declaration`: outside any scope it is skipped
    /// whole. `: this(…)` calls itself; `: base(…)` calls the superclass's
    /// constructor, `Base.Base` (`base.<init>` when none is known).
    fn constructor(&mut self, node: Node<'_>) {
        let Some(innermost) = self.scope.last().cloned() else {
            return;
        };
        let name = self
            .first_identifier(node)
            .filter(|n| !n.is_empty())
            .unwrap_or(innermost);
        let modifiers = self.modifiers(node);
        let parent = self.parent().unwrap_or_default();
        let list = child_of_kind(node, "parameter_list");
        let mut symbol = self.symbol(
            &name,
            SymbolType::Constructor,
            Scope::Function,
            node,
            modifiers.visibility,
        );
        symbol.is_static = modifiers.has("is_static");
        symbol.parameter_types = self.parameter_types_of(list);
        let qualified = symbol.full_name.clone().unwrap_or_default();
        self.symbols.push(symbol);
        if let Some(initializer) = child_of_kind(node, "constructor_initializer") {
            for child in children(initializer) {
                let (target, chain) = match child.kind() {
                    "this" => (qualified.clone(), "this"),
                    "base" => {
                        let target = self.superclass(&parent).map_or_else(
                            || "base.<init>".to_owned(),
                            |s| format!("{s}.{}", s.rsplit('.').next().unwrap_or(&s)),
                        );
                        (target, "base")
                    }
                    _ => continue,
                };
                let mut relationship =
                    self.relationship(qualified.clone(), target, RelationshipType::Calls);
                relationship.annotations.insert(
                    "constructor_chain".to_owned(),
                    Value::String(chain.to_owned()),
                );
                self.relationships.push(relationship);
            }
        }
        self.visit_in_scope(node, "<init>".to_owned());
    }

    /// `_handle_destructor_declaration`: a protected METHOD `~Finalize`.
    fn destructor(&mut self, node: Node<'_>) {
        if self.scope.is_empty() {
            return;
        }
        let name = "~Finalize";
        let mut symbol = self.symbol(name, SymbolType::Method, Scope::Function, node, "protected");
        symbol
            .metadata
            .insert("is_destructor".to_owned(), Value::Bool(true));
        self.symbols.push(symbol);
        self.visit_in_scope(node, name.to_owned());
    }

    /// `_handle_property_declaration`. Python quirk: the body is visited in
    /// the CLASS's scope, so a getter's calls come from the class.
    fn property(&mut self, node: Node<'_>) {
        let modifiers = self.modifiers(node);
        let attributes = self.attributes(node);
        let mut property_type = None;
        let mut nullable = false;
        let mut found_type = false;
        let mut name = None;
        for child in children(node) {
            let kind = child.kind();
            if matches!(kind, "modifier" | "attribute_list") {
                continue;
            }
            if !found_type && TYPE_CANDIDATES.contains(&kind) {
                property_type = Some(self.type_name(child));
                nullable = kind == "nullable_type";
                found_type = true;
            } else if kind == "identifier" {
                if found_type
                    || child.next_named_sibling().is_some_and(|n| {
                        matches!(n.kind(), "accessor_list" | "arrow_expression_clause")
                    })
                {
                    name = Some(self.text(child));
                    break;
                }
                property_type = Some(self.text(child));
                found_type = true;
            }
        }
        let Some(name) = name.filter(|n| !n.is_empty()) else {
            return;
        };
        let mut metadata = modifiers.metadata();
        if nullable {
            metadata.insert("is_nullable".to_owned(), Value::Bool(true));
        }
        if let Some(accessors) = child_of_kind(node, "accessor_list")
            && children(accessors).into_iter().any(|a| {
                a.kind() == "accessor_declaration"
                    && py_strip(self.source.text(a)).starts_with("init")
            })
        {
            metadata.insert("has_init_accessor".to_owned(), Value::Bool(true));
        }
        let mut symbol = self.symbol(
            &name,
            SymbolType::Property,
            Scope::Class,
            node,
            modifiers.visibility,
        );
        symbol.is_static = modifiers.has("is_static");
        symbol.is_abstract = modifiers.has("is_abstract");
        symbol.return_type.clone_from(&property_type);
        symbol.metadata = metadata;
        let qualified = symbol.full_name.clone().unwrap_or_default();
        self.symbols.push(symbol);
        self.annotates(attributes, &qualified);
        if let Some(property_type) = property_type.filter(|t| !is_builtin_type(t)) {
            let rel_type = field_relationship_type(&property_type, nullable);
            self.push_relationship(qualified, property_type, rel_type);
        }
        self.visit_children(node);
    }

    /// The type and nullability of a `variable_declaration`'s first type
    /// child among `kinds`.
    fn declared_type(&self, declaration: Node<'_>, kinds: &[&str]) -> (Option<String>, bool) {
        children(declaration)
            .into_iter()
            .find(|c| kinds.contains(&c.kind()))
            .map_or((None, false), |c| {
                (Some(self.type_name(c)), c.kind() == "nullable_type")
            })
    }

    /// `_handle_field_declaration`: one FIELD (CONSTANT when `const`) per
    /// declarator, each with the whole declaration's range and text.
    fn field(&mut self, node: Node<'_>) {
        let modifiers = self.modifiers(node);
        let attributes = self.attributes(node);
        let Some(declaration) = child_of_kind(node, "variable_declaration") else {
            return;
        };
        let (field_type, nullable) = self.declared_type(declaration, TYPE_KINDS);
        let symbol_type = if modifiers.has("is_const") {
            SymbolType::Constant
        } else {
            SymbolType::Field
        };
        for declarator in children(declaration) {
            if declarator.kind() != "variable_declarator" {
                continue;
            }
            let name = match child_of_kind(declarator, "identifier") {
                Some(id) => self.text(id),
                None => py_strip(first_part(self.source.text(declarator), '=')).to_owned(),
            };
            if name.is_empty() {
                continue;
            }
            let mut metadata = modifiers.metadata();
            if nullable {
                metadata.insert("is_nullable".to_owned(), Value::Bool(true));
            }
            let mut symbol =
                self.symbol(&name, symbol_type, Scope::Class, node, modifiers.visibility);
            symbol.is_static = modifiers.has("is_static");
            symbol.return_type.clone_from(&field_type);
            symbol.metadata = metadata;
            let qualified = symbol.full_name.clone().unwrap_or_default();
            self.symbols.push(symbol);
            self.annotates(attributes.clone(), &qualified);
            if let Some(field_type) = field_type.as_ref().filter(|t| !t.is_empty()) {
                self.field_types.insert(name, field_type.clone());
                if !is_builtin_type(field_type) {
                    let rel_type = field_relationship_type(field_type, nullable);
                    self.push_relationship(qualified, field_type.clone(), rel_type);
                }
            }
        }
        self.visit_children(node);
    }

    /// `_handle_event_field_declaration`: one FIELD per declarator, no
    /// edges. Python quirk: a nullable or predefined event type is none,
    /// and initialisers are not visited.
    fn event_field(&mut self, node: Node<'_>) {
        let modifiers = self.modifiers(node);
        let Some(declaration) = child_of_kind(node, "variable_declaration") else {
            return;
        };
        let (event_type, _) = self.declared_type(
            declaration,
            &["identifier", "qualified_name", "generic_name"],
        );
        for declarator in children(declaration) {
            if declarator.kind() != "variable_declarator" {
                continue;
            }
            let Some(name) = self.first_identifier(declarator) else {
                continue;
            };
            let mut symbol = self.symbol(
                &name,
                SymbolType::Field,
                Scope::Class,
                node,
                modifiers.visibility,
            );
            symbol.is_static = modifiers.has("is_static");
            symbol.return_type.clone_from(&event_type);
            symbol
                .metadata
                .insert("is_event".to_owned(), Value::Bool(true));
            self.symbols.push(symbol);
        }
    }

    /// `_handle_indexer_declaration`: a PROPERTY `this[]`; its body is
    /// visited in the class's scope.
    fn indexer(&mut self, node: Node<'_>) {
        let modifiers = self.modifiers(node);
        let mut symbol = self.symbol(
            "this[]",
            SymbolType::Property,
            Scope::Class,
            node,
            modifiers.visibility,
        );
        symbol.return_type = self.first_type(node);
        symbol.parameter_types =
            self.parameter_types_of(child_of_kind(node, "bracketed_parameter_list"));
        symbol
            .metadata
            .insert("is_indexer".to_owned(), Value::Bool(true));
        self.symbols.push(symbol);
        self.visit_children(node);
    }

    /// `_handle_operator_declaration`: a static METHOD `operator +`.
    /// Python quirk: the body is not visited. A conversion operator
    /// (`implicit operator int`) is another node kind and gives no symbol.
    fn operator(&mut self, node: Node<'_>) {
        let modifiers = self.modifiers(node);
        let mut return_type = None;
        let mut found_return = false;
        let mut after_keyword = false;
        let mut operator: Option<String> = None;
        for child in children(node) {
            let kind = child.kind();
            if matches!(kind, "modifier" | "attribute_list") {
                continue;
            }
            if !found_return && (TYPE_CANDIDATES.contains(&kind) || kind == "identifier") {
                return_type = Some(self.type_name(child));
                found_return = true;
            } else if kind == "operator" {
                after_keyword = true;
            } else if after_keyword && operator.is_none() {
                operator = Some(self.text(child));
            }
        }
        let Some(operator) = operator.filter(|o| !o.is_empty()) else {
            return;
        };
        let name = format!("operator {operator}");
        let mut symbol = self.symbol(
            &name,
            SymbolType::Method,
            Scope::Function,
            node,
            modifiers.visibility,
        );
        symbol.is_static = true;
        symbol.return_type = return_type;
        symbol.parameter_types = self.parameter_types_of(child_of_kind(node, "parameter_list"));
        symbol
            .metadata
            .insert("is_operator".to_owned(), Value::Bool(true));
        self.symbols.push(symbol);
    }

    // ------------------------------------------------------------------
    // Expressions
    // ------------------------------------------------------------------

    /// `_resolve_object_type`: a field's type, else a parameter's, as its
    /// last dotted segment.
    fn object_type(&self, object: &str) -> Option<&str> {
        self.field_types
            .get(object)
            .or_else(|| self.parameter_types.get(object))
            .map(|t| t.rsplit('.').next().unwrap_or(t))
    }

    /// `_handle_invocation_expression`: a `calls` edge to the callee's
    /// text, `Type.method` when the receiver is a known field or parameter.
    /// Python quirk: a generic or conditional callee (`Foo<T>()`, `a?.B()`)
    /// gives no edge, and the receiver of a chain is the segment before the
    /// LAST dot of the whole text (`a.B(c.D).E` → `D)`).
    fn invocation(&mut self, node: Node<'_>) {
        let Some(callee) = children(node)
            .into_iter()
            .find(|c| matches!(c.kind(), "member_access_expression" | "identifier"))
            .map(|c| self.text(c))
            .filter(|c| !c.is_empty())
        else {
            self.visit_children(node);
            return;
        };
        let target = match callee.rsplit_once('.') {
            Some((object, method)) => {
                let object = object.rsplit('.').next().unwrap_or(object);
                match self.object_type(object).filter(|t| !t.is_empty()) {
                    Some(object_type) => format!("{object_type}.{method}"),
                    None => callee,
                }
            }
            None => callee,
        };
        self.push_relationship(self.scope_or_stem(), target, RelationshipType::Calls);
        self.visit_children(node);
    }

    /// `_handle_object_creation`: a `creates` edge (confidence 0.95) to a
    /// non-built-in type. Python quirk: `new()` (target-typed) is another
    /// node kind and creates nothing.
    fn object_creation(&mut self, node: Node<'_>) {
        if let Some(type_node) = children(node)
            .into_iter()
            .find(|c| matches!(c.kind(), "identifier" | "qualified_name" | "generic_name"))
        {
            let type_name = self.type_name(type_node);
            if !is_builtin_type(&type_name) {
                let mut relationship =
                    self.relationship(self.scope_or_stem(), type_name, RelationshipType::Creates);
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

/// Which `_extract_base_list` rule applies.
#[derive(Clone, Copy)]
enum BaseKind {
    Class,
    Struct,
    Interface,
}

fn strings(values: &[String]) -> Value {
    Value::Array(values.iter().cloned().map(Value::String).collect())
}

/// `_extract_defines_relationships`: a `defines` edge from each class,
/// struct, interface and enum to every method, constructor, field,
/// property, constant and delegate whose parent is its full name (or, when
/// that finds none, its bare name). A partial class declared twice in one
/// file defines its members twice (Python quirk).
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
            SymbolType::Class | SymbolType::Struct | SymbolType::Interface | SymbolType::Enum
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
            let member_type = match member.symbol_type {
                SymbolType::Constructor => "constructor",
                SymbolType::Field => "field",
                SymbolType::Property => "property",
                SymbolType::Constant => "constant",
                SymbolType::Method => "method",
                SymbolType::TypeAlias => "delegate",
                _ => continue,
            };
            let params = member.parameter_types.join(", ");
            let member_full = member.full_name.clone().unwrap_or_default();
            let is_method = member.symbol_type == SymbolType::Method;
            let target = if member.symbol_type == SymbolType::Constructor {
                format!("{full_name}.<init>({params})")
            } else if is_method
                && members
                    .iter()
                    .filter(|m| m.symbol_type == SymbolType::Method && m.name == member.name)
                    .count()
                    > 1
            {
                format!("{member_full}({params})")
            } else {
                member_full
            };
            let mut annotations = Map::new();
            annotations.insert(
                "member_type".to_owned(),
                Value::String(member_type.to_owned()),
            );
            annotations.insert("is_static".to_owned(), Value::Bool(member.is_static));
            annotations.insert(
                "visibility".to_owned(),
                member.visibility.clone().map_or(Value::Null, Value::String),
            );
            if is_method {
                annotations.insert(
                    "parameter_types".to_owned(),
                    strings(&member.parameter_types),
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

//! One Kotlin file: declarations first (symbols, imports, supertypes,
//! delegation), then one walk of the whole tree for annotations and calls.
//! Node kinds are `tree-sitter-kotlin-ng` 1.1.0's.

use crate::limits;
use crate::model::{ParseResult, RelationshipType, SymbolType};
use crate::visit_support::{
    Output, child_of_kind, children, has_token, put, starts_upper, type_name, word_list,
};
use serde_json::Map;
use std::collections::HashSet;
use tree_sitter::Node;

/// Annotations that never become `decorates` edges.
const BUILTIN_ANNOTATIONS: &[&str] = &[
    "Override",
    "Deprecated",
    "Suppress",
    "JvmStatic",
    "JvmField",
    "JvmOverloads",
    "JvmName",
    "Throws",
    "Nullable",
    "NotNull",
    "Test",
    "Before",
    "After",
];

/// Keywords and stdlib calls that never become `calls` edges.
const SKIPPED_CALLS: &[&str] = &[
    "print",
    "println",
    "listOf",
    "mapOf",
    "setOf",
    "arrayOf",
    "mutableListOf",
    "mutableMapOf",
    "mutableSetOf",
    "lazy",
    "require",
    "check",
    "assert",
    "error",
    "TODO",
    "also",
    "apply",
    "let",
    "run",
    "with",
];

/// The concrete kinds of the grammar's `type` supertype.
const TYPE_KINDS: &[&str] = &[
    "user_type",
    "nullable_type",
    "function_type",
    "parenthesized_type",
    "non_nullable_type",
    "dynamic",
];

fn is_type(node: Node<'_>) -> bool {
    TYPE_KINDS.contains(&node.kind())
}

/// Visit one parsed file.
pub(super) fn visit(file_path: &str, source: &str, root: Node<'_>) -> ParseResult {
    let mut visitor = Visitor {
        out: Output::new(file_path, source),
        package: None,
    };
    if let Some(header) = child_of_kind(root, "package_header")
        && let Some(name) = child_of_kind(header, "qualified_identifier")
    {
        visitor.package = Some(visitor.out.text(name).to_owned());
    }
    visitor.declarations(root, None);
    let names: HashSet<String> = visitor.out.symbols.iter().map(|s| s.name.clone()).collect();
    visitor.uses(root, &names);
    visitor.out.finish("kotlin")
}

/// A declaration's modifiers.
#[derive(Default)]
struct Modifiers {
    /// The first visibility modifier.
    visibility: Option<String>,
    /// Every keyword modifier, in source order (annotations left out).
    words: Vec<String>,
    /// The class and inheritance modifiers (`data`, `sealed`, `abstract`…).
    class_words: Vec<String>,
}

struct Visitor<'s> {
    out: Output<'s>,
    package: Option<String>,
}

impl Visitor<'_> {
    fn qualify(&self, name: &str) -> String {
        match &self.package {
            Some(package) => format!("{package}.{name}"),
            None => name.to_owned(),
        }
    }

    fn modifiers(&self, node: Node<'_>) -> Modifiers {
        let mut modifiers = Modifiers::default();
        let Some(list) = child_of_kind(node, "modifiers") else {
            return modifiers;
        };
        for child in children(list) {
            let kind = child.kind();
            if !child.is_named() || kind == "annotation" {
                continue;
            }
            let word = self.out.text(child).trim().to_owned();
            if kind == "visibility_modifier" && modifiers.visibility.is_none() {
                modifiers.visibility = Some(word.clone());
            }
            if matches!(kind, "class_modifier" | "inheritance_modifier") {
                modifiers.class_words.push(word.clone());
            }
            modifiers.words.push(word);
        }
        modifiers
    }

    /// The `KDoc` directly before `node`: the previous sibling, when it is
    /// a `/** … */` comment, cleaned line by line.
    fn kdoc(&self, node: Node<'_>) -> Option<String> {
        let previous = node.prev_named_sibling()?;
        if previous.kind() != "block_comment" {
            return None;
        }
        let text = self.out.text(previous);
        let body = text.strip_prefix("/**")?;
        let body = body.strip_suffix("*/").unwrap_or(body);
        let lines: Vec<&str> = body
            .split('\n')
            .map(|line| {
                let line = line.trim();
                line.strip_prefix('*').unwrap_or(line).trim()
            })
            .collect();
        let doc = lines.join("\n");
        let doc = doc.trim();
        if doc.is_empty() {
            None
        } else {
            limits::kept_str(doc)
        }
    }

    /// The declarations directly in `container` (a file, a class body, or
    /// an `ERROR` node the parser recovered around).
    fn declarations(&mut self, container: Node<'_>, parent: Option<&str>) {
        for child in children(container) {
            match child.kind() {
                "import" => self.import(child),
                "class_declaration" => self.class(child, parent),
                "object_declaration" => self.object(child, parent, false),
                "companion_object" => self.object(child, parent, true),
                "function_declaration" => self.function(child, parent),
                "property_declaration" => self.property(child, parent),
                "type_alias" => self.type_alias(child, parent),
                "ERROR" => self.declarations(child, parent),
                _ => {}
            }
        }
    }

    fn import(&mut self, node: Node<'_>) {
        let Some(path) = child_of_kind(node, "qualified_identifier") else {
            return;
        };
        let mut annotations = Map::new();
        if let Some(alias) = child_of_kind(node, "identifier") {
            put(&mut annotations, "alias", self.out.text(alias));
        }
        if has_token(node, "*") {
            put(&mut annotations, "wildcard", true);
        }
        let stem = self.out.stem.clone();
        let target = self.out.text(path).to_owned();
        self.out
            .relate(&stem, &target, RelationshipType::Imports, node, annotations);
    }

    fn class(&mut self, node: Node<'_>, parent: Option<&str>) {
        let Some(name_node) = node.child_by_field_name("name") else {
            return;
        };
        let name = self.out.text(name_node).to_owned();
        let modifiers = self.modifiers(node);
        let visibility = modifiers.visibility.as_deref().unwrap_or("public");
        let is_interface = has_token(node, "interface");
        let mut metadata = Map::new();
        put(&mut metadata, "visibility", visibility);
        let mut symbol_type = SymbolType::Class;
        if is_interface {
            symbol_type = SymbolType::Interface;
            if has_token(node, "fun") {
                put(&mut metadata, "is_fun_interface", true);
            }
        } else if !modifiers.class_words.is_empty() {
            let words = &modifiers.class_words;
            put(&mut metadata, "modifiers", words.join(" "));
            if words.iter().any(|w| w == "enum") {
                symbol_type = SymbolType::Enum;
            } else if words.iter().any(|w| w == "data") {
                put(&mut metadata, "is_data_class", true);
            } else if words.iter().any(|w| w == "sealed") {
                put(&mut metadata, "is_sealed", true);
            }
        }
        let full_name = self.qualify(&name);
        let mut symbol = self.out.symbol(&name, symbol_type, node, metadata);
        symbol.full_name = Some(full_name.clone());
        symbol.parent_symbol = parent.map(str::to_owned);
        symbol.docstring = self.kdoc(node);
        symbol.visibility = Some(visibility.to_owned());
        self.out.symbols.push(symbol);
        self.supertypes(node, &name, is_interface);
        self.body(node, &full_name);
    }

    fn object(&mut self, node: Node<'_>, parent: Option<&str>, companion: bool) {
        let name = match node.child_by_field_name("name") {
            Some(name) => self.out.text(name).to_owned(),
            None if companion => "Companion".to_owned(),
            None => return,
        };
        let modifiers = self.modifiers(node);
        let visibility = modifiers.visibility.as_deref().unwrap_or("public");
        let mut metadata = Map::new();
        put(&mut metadata, "visibility", visibility);
        if companion {
            put(&mut metadata, "is_companion", true);
        }
        let full_name = self.qualify(&name);
        let mut symbol = self.out.symbol(&name, SymbolType::Class, node, metadata);
        symbol.full_name = Some(full_name.clone());
        symbol.parent_symbol = parent.map(str::to_owned);
        symbol.docstring = self.kdoc(node);
        symbol.visibility = Some(visibility.to_owned());
        self.out.symbols.push(symbol);
        self.supertypes(node, &name, false);
        self.body(node, &full_name);
    }

    fn body(&mut self, node: Node<'_>, full_name: &str) {
        for kind in ["class_body", "enum_class_body"] {
            if let Some(body) = child_of_kind(node, kind) {
                self.declarations(body, Some(full_name));
            }
        }
    }

    /// The `: A(), B, C by d` clause of a class, interface or object.
    fn supertypes(&mut self, node: Node<'_>, name: &str, is_interface: bool) {
        let Some(list) = child_of_kind(node, "delegation_specifiers") else {
            return;
        };
        for specifier in children(list) {
            if specifier.kind() != "delegation_specifier" {
                continue;
            }
            let Some(inner) = specifier.named_child(0) else {
                continue;
            };
            let (type_node, constructed) = match inner.kind() {
                "constructor_invocation" => (child_of_kind(inner, "user_type"), true),
                "explicit_delegation" => {
                    let type_node = children(inner).into_iter().find(|c| is_type(*c));
                    if let Some(delegate) = inner.named_child(last_index(inner))
                        && !is_type(delegate)
                    {
                        let mut annotations = Map::new();
                        put(&mut annotations, "delegation", true);
                        let stem = self.out.stem.clone();
                        let target = self.out.text(delegate).to_owned();
                        self.out.relate(
                            &stem,
                            &target,
                            RelationshipType::Composition,
                            inner,
                            annotations,
                        );
                    }
                    (type_node, false)
                }
                _ if is_type(inner) => (Some(inner), false),
                _ => (None, false),
            };
            let Some(type_node) = type_node else {
                continue;
            };
            let target = type_name(self.out.text(type_node));
            let kind = if is_interface || constructed {
                RelationshipType::Inheritance
            } else {
                RelationshipType::Implementation
            };
            self.out.relate(name, &target, kind, specifier, Map::new());
        }
    }

    fn function(&mut self, node: Node<'_>, parent: Option<&str>) {
        let Some(name_node) = node.child_by_field_name("name") else {
            return;
        };
        let name = self.out.text(name_node).to_owned();
        let modifiers = self.modifiers(node);
        let mut receiver = None;
        let mut return_type = None;
        let mut after_parameters = false;
        for child in children(node) {
            if child.kind() == "function_value_parameters" {
                after_parameters = true;
            } else if is_type(child) {
                if after_parameters {
                    return_type.get_or_insert(child);
                } else if child.start_byte() < name_node.start_byte() {
                    receiver = Some(child);
                }
            }
        }
        let mut metadata = Map::new();
        let is_suspend = modifiers.words.iter().any(|w| w == "suspend");
        if !modifiers.words.is_empty() {
            put(&mut metadata, "modifiers", word_list(&modifiers.words));
            if is_suspend {
                put(&mut metadata, "is_suspend", true);
            }
            if modifiers.words.iter().any(|w| w == "inline") {
                put(&mut metadata, "is_inline", true);
            }
        }
        if let Some(receiver) = receiver {
            put(&mut metadata, "extension_receiver", self.out.text(receiver));
        }
        let mut symbol = self.out.symbol(&name, SymbolType::Function, node, metadata);
        symbol.full_name = Some(self.qualify(&name));
        symbol.parent_symbol = parent.map(str::to_owned);
        symbol.docstring = self.kdoc(node);
        symbol.is_async = is_suspend;
        symbol.return_type = return_type.map(|t| self.out.text(t).trim().to_owned());
        self.out.symbols.push(symbol);
    }

    fn property(&mut self, node: Node<'_>, parent: Option<&str>) {
        let Some(variable) = child_of_kind(node, "variable_declaration") else {
            return; // A destructuring declaration.
        };
        let Some(name_node) = child_of_kind(variable, "identifier") else {
            return;
        };
        let name = self.out.text(name_node).to_owned();
        let modifiers = self.modifiers(node);
        let receiver = children(node)
            .into_iter()
            .take_while(|c| c.kind() != "variable_declaration")
            .find(|c| is_type(*c));
        let declared = children(variable).into_iter().find(|c| is_type(*c));
        let mut metadata = Map::new();
        put(&mut metadata, "mutable", has_token(node, "var"));
        if !modifiers.words.is_empty() {
            put(&mut metadata, "modifiers", word_list(&modifiers.words));
        }
        if let Some(receiver) = receiver {
            put(&mut metadata, "extension_receiver", self.out.text(receiver));
        }
        if let Some(declared) = declared {
            put(&mut metadata, "type", self.out.text(declared).trim());
        }
        let mut symbol = self.out.symbol(&name, SymbolType::Variable, node, metadata);
        symbol.full_name = Some(self.qualify(&name));
        symbol.parent_symbol = parent.map(str::to_owned);
        self.out.symbols.push(symbol);
    }

    fn type_alias(&mut self, node: Node<'_>, parent: Option<&str>) {
        // The grammar names the alias's identifier `type`.
        let Some(name_node) = node.child_by_field_name("type") else {
            return;
        };
        let name = self.out.text(name_node).to_owned();
        let modifiers = self.modifiers(node);
        let visibility = modifiers.visibility.as_deref().unwrap_or("public");
        let aliased = children(node).into_iter().rev().find(|c| is_type(*c));
        let mut metadata = Map::new();
        put(
            &mut metadata,
            "aliased_type",
            aliased.map_or("", |a| self.out.text(a)).trim(),
        );
        let mut symbol = self
            .out
            .symbol(&name, SymbolType::TypeAlias, node, metadata);
        symbol.full_name = Some(self.qualify(&name));
        symbol.parent_symbol = parent.map(str::to_owned);
        symbol.visibility = Some(visibility.to_owned());
        self.out.symbols.push(symbol);
    }

    /// Annotations and calls anywhere in the tree under `node`.
    fn uses(&mut self, node: Node<'_>, names: &HashSet<String>) {
        match node.kind() {
            "annotation" | "file_annotation" => self.annotation(node),
            "call_expression" => self.call(node, names),
            _ => {}
        }
        for child in children(node) {
            self.uses(child, names);
        }
    }

    fn annotation(&mut self, node: Node<'_>) {
        let user_type = child_of_kind(node, "user_type").or_else(|| {
            child_of_kind(node, "constructor_invocation")
                .and_then(|c| child_of_kind(c, "user_type"))
        });
        let Some(user_type) = user_type else {
            return;
        };
        let full = type_name(self.out.text(user_type));
        let name = full.rsplit('.').next().unwrap_or_default().to_owned();
        if name.is_empty() || BUILTIN_ANNOTATIONS.contains(&name.as_str()) {
            return;
        }
        let stem = self.out.stem.clone();
        self.out
            .relate(&stem, &name, RelationshipType::Decorates, node, Map::new());
    }

    /// `f(…)` is a `calls` edge, `Type(…)` and `a.Type(…)` a `uses` edge;
    /// `a.f(…)` is neither, as in the regex parser.
    fn call(&mut self, node: Node<'_>, names: &HashSet<String>) {
        let Some(callee) = node.named_child(0) else {
            return;
        };
        let (name, bare) = match callee.kind() {
            "identifier" => (self.out.text(callee), true),
            "navigation_expression" => {
                let last = callee.named_child(last_index(callee));
                match last.filter(|l| l.kind() == "identifier") {
                    Some(last) => (self.out.text(last), false),
                    None => return,
                }
            }
            _ => return,
        };
        if names.contains(name) {
            return;
        }
        let kind = if starts_upper(name) {
            RelationshipType::Aggregation
        } else if bare && !SKIPPED_CALLS.contains(&name) {
            RelationshipType::Calls
        } else {
            return;
        };
        let stem = self.out.stem.clone();
        self.out.relate(&stem, name, kind, node, Map::new());
    }
}

/// The index of `node`'s last named child.
fn last_index(node: Node<'_>) -> u32 {
    u32::try_from(node.named_child_count().saturating_sub(1)).unwrap_or(u32::MAX)
}

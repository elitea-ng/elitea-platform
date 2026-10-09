//! One Swift file: declarations first (symbols, imports, conformances),
//! then one walk of the whole tree for attributes and calls. Node kinds are
//! `tree-sitter-swift` 0.7.4's.

use crate::limits;
use crate::model::{ParseResult, RelationshipType, Symbol, SymbolType};
use crate::visit_support::{
    Output, child_of_kind, children, has_token, put, starts_upper, type_name, word_list,
};
use serde_json::{Map, Value};
use std::collections::HashSet;
use tree_sitter::Node;

/// Attributes that never become `decorates` edges.
const BUILTIN_ATTRIBUTES: &[&str] = &[
    "available",
    "objc",
    "objcMembers",
    "nonobjc",
    "escaping",
    "autoclosure",
    "discardableResult",
    "inlinable",
    "usableFromInline",
    "frozen",
    "unknown",
    "IBOutlet",
    "IBAction",
    "IBDesignable",
    "IBInspectable",
    "main",
    "testable",
    "Published",
    "State",
    "Binding",
    "ObservedObject",
    "EnvironmentObject",
    "Environment",
    "AppStorage",
    "SceneStorage",
    "FetchRequest",
    "NSManaged",
];

/// Standard types whose initializer calls are not `uses` edges.
const BUILTIN_TYPES: &[&str] = &[
    "String",
    "Int",
    "Double",
    "Float",
    "Bool",
    "Array",
    "Dictionary",
    "Set",
    "Optional",
    "Result",
    "UUID",
    "URL",
    "Date",
    "Data",
    "Error",
    "NSError",
    "Range",
    "ClosedRange",
    "Substring",
    "Character",
    "CGFloat",
    "CGPoint",
    "CGSize",
    "CGRect",
    "UIColor",
    "NSColor",
    "UIImage",
    "NSImage",
    "UIView",
    "NSView",
    "DispatchQueue",
    "Task",
    "URLSession",
    "JSONDecoder",
    "JSONEncoder",
];

/// Common methods whose calls are not `calls` edges.
const SKIPPED_METHODS: &[&str] = &[
    "map",
    "flatMap",
    "compactMap",
    "filter",
    "reduce",
    "forEach",
    "sorted",
    "first",
    "last",
    "append",
    "insert",
    "remove",
    "contains",
    "count",
    "isEmpty",
    "joined",
    "split",
    "prefix",
    "suffix",
    "dropFirst",
    "dropLast",
    "init",
    "deinit",
    "description",
    "debugDescription",
    "hash",
];

/// Keywords a declaration may carry outside its `modifiers` node
/// (`class func`, `func f() async`).
const LOOSE_KEYWORDS: &[&str] = &["class", "static", "final", "mutating", "nonmutating"];

/// Visit one parsed file.
pub(super) fn visit(file_path: &str, source: &str, root: Node<'_>) -> ParseResult {
    let mut visitor = Visitor {
        out: Output::new(file_path, source),
    };
    visitor.declarations(root, None);
    let names: HashSet<String> = visitor.out.symbols.iter().map(|s| s.name.clone()).collect();
    visitor.uses(root, &names);
    visitor.out.finish("swift")
}

/// A declaration's modifiers.
#[derive(Default)]
struct Modifiers {
    /// The first visibility modifier that is not a setter's (`private(set)`).
    visibility: Option<String>,
    /// Every other keyword modifier, in source order (attributes and
    /// visibility left out).
    words: Vec<String>,
}

impl Modifiers {
    fn has(&self, word: &str) -> bool {
        self.words.iter().any(|w| w == word)
    }
}

struct Visitor<'s> {
    out: Output<'s>,
}

impl Visitor<'_> {
    fn qualify(&self, name: &str) -> String {
        format!("{}.{name}", self.out.stem)
    }

    fn modifiers(&self, node: Node<'_>) -> Modifiers {
        let mut modifiers = Modifiers::default();
        if let Some(list) = child_of_kind(node, "modifiers") {
            for child in children(list) {
                if !child.is_named() || child.kind() == "attribute" {
                    continue;
                }
                let word = self.out.text(child).trim().to_owned();
                if child.kind() == "visibility_modifier" {
                    if modifiers.visibility.is_none() && !word.contains('(') {
                        modifiers.visibility = Some(word);
                    }
                } else {
                    modifiers.words.push(word);
                }
            }
        }
        for child in children(node) {
            if !child.is_named() && LOOSE_KEYWORDS.contains(&child.kind()) {
                modifiers.words.push(child.kind().to_owned());
            }
        }
        modifiers
    }

    /// The doc comment directly before `node`: its `///` lines, or a
    /// `/** … */` block.
    fn doc(&self, node: Node<'_>) -> Option<String> {
        let mut lines = Vec::new();
        let mut previous = node.prev_named_sibling();
        while let Some(comment) = previous {
            let text = self.out.text(comment).trim();
            if comment.kind() == "comment"
                && let Some(line) = text.strip_prefix("///")
            {
                lines.push(line.trim().to_owned());
                previous = comment.prev_named_sibling();
                continue;
            }
            if lines.is_empty()
                && comment.kind() == "multiline_comment"
                && let Some(body) = text.strip_prefix("/**")
            {
                let body = body.strip_suffix("*/").unwrap_or(body);
                lines = body
                    .split('\n')
                    .rev()
                    .map(|line| {
                        let line = line.trim();
                        line.strip_prefix('*').unwrap_or(line).trim().to_owned()
                    })
                    .collect();
            }
            break;
        }
        lines.reverse();
        let doc = lines.join("\n");
        let doc = doc.trim();
        if doc.is_empty() {
            None
        } else {
            limits::kept_str(doc)
        }
    }

    /// The declarations directly in `container`.
    fn declarations(&mut self, container: Node<'_>, parent: Option<&str>) {
        for child in children(container) {
            match child.kind() {
                "import_declaration" => self.import(child),
                "class_declaration" => self.type_declaration(child, parent),
                "protocol_declaration" => self.protocol(child, parent),
                "function_declaration" | "protocol_function_declaration" => {
                    self.function(child, parent);
                }
                "property_declaration" | "protocol_property_declaration" => {
                    self.property(child, parent);
                }
                "typealias_declaration" => self.type_alias(child, parent),
                "ERROR" => self.declarations(child, parent),
                _ => {}
            }
        }
    }

    fn import(&mut self, node: Node<'_>) {
        let Some(module) = child_of_kind(node, "identifier") else {
            return;
        };
        let mut annotations = Map::new();
        let kind = children(node).into_iter().find(|c| {
            !c.is_named()
                && matches!(
                    c.kind(),
                    "typealias" | "struct" | "class" | "enum" | "protocol" | "let" | "var" | "func"
                )
        });
        if let Some(kind) = kind {
            put(&mut annotations, "kind", kind.kind());
        }
        let stem = self.out.stem.clone();
        let target = self.out.text(module).to_owned();
        self.out
            .relate(&stem, &target, RelationshipType::Imports, node, annotations);
    }

    /// The supertypes after `:`, generic arguments dropped.
    fn inherited<'t>(&self, node: Node<'t>) -> Vec<(String, Node<'t>)> {
        children(node)
            .into_iter()
            .filter(|c| c.kind() == "inheritance_specifier")
            .map(|c| {
                let target = c.child_by_field_name("inherits_from").unwrap_or(c);
                (type_name(self.out.text(target)), c)
            })
            .collect()
    }

    /// A type-like symbol: visibility first in the metadata, then `extra`.
    fn type_symbol(
        &self,
        node: Node<'_>,
        name: &str,
        symbol_type: SymbolType,
        parent: Option<&str>,
        extra: Map<String, Value>,
        docstring: Option<String>,
    ) -> Symbol {
        let modifiers = self.modifiers(node);
        let visibility = modifiers.visibility.as_deref().unwrap_or("internal");
        let mut metadata = Map::new();
        put(&mut metadata, "visibility", visibility);
        metadata.extend(extra);
        let mut symbol = self.out.symbol(name, symbol_type, node, metadata);
        symbol.full_name = Some(self.qualify(name));
        symbol.parent_symbol = parent.map(str::to_owned);
        symbol.docstring = docstring;
        symbol.visibility = Some(visibility.to_owned());
        symbol
    }

    fn body(&mut self, node: Node<'_>, full_name: &str) {
        if let Some(body) = node.child_by_field_name("body") {
            self.declarations(body, Some(full_name));
        }
    }

    /// `class`, `struct`, `enum`, `actor` and `extension` declarations.
    fn type_declaration(&mut self, node: Node<'_>, parent: Option<&str>) {
        let Some(kind) = node.child_by_field_name("declaration_kind") else {
            return;
        };
        let Some(name_node) = node.child_by_field_name("name") else {
            return;
        };
        let name = type_name(self.out.text(name_node));
        let inherited = self.inherited(node);
        let modifiers = self.modifiers(node);
        let mut extra = Map::new();
        let flag = |key: &str| {
            let mut map = Map::new();
            put(&mut map, key, true);
            map
        };
        let declared_name = if kind.kind() == "extension" {
            format!("{name}_extension")
        } else {
            name.clone()
        };
        let symbol_type = match kind.kind() {
            "struct" => {
                extra = flag("is_struct");
                SymbolType::Class
            }
            "actor" => {
                extra = flag("is_actor");
                SymbolType::Class
            }
            "enum" => {
                if has_token(node, "indirect") || modifiers.has("indirect") {
                    put(&mut extra, "indirect", true);
                }
                if !inherited.is_empty() {
                    let raw: Vec<&str> = inherited.iter().map(|(t, _)| t.as_str()).collect();
                    put(&mut extra, "raw_type", raw.join(", "));
                }
                SymbolType::Enum
            }
            "extension" => {
                extra = flag("is_extension");
                if !inherited.is_empty() {
                    let list = inherited
                        .iter()
                        .map(|(t, _)| Value::String(t.clone()))
                        .collect();
                    put(&mut extra, "conformance", Value::Array(list));
                }
                SymbolType::Class
            }
            _ => {
                if modifiers.has("final") {
                    extra = flag("final");
                }
                SymbolType::Class
            }
        };
        let docstring = if kind.kind() == "extension" {
            None
        } else {
            self.doc(node)
        };
        let symbol = self.type_symbol(node, &declared_name, symbol_type, parent, extra, docstring);
        let full_name = symbol.full_name.clone().unwrap_or_default();
        self.out.symbols.push(symbol);

        if kind.kind() == "extension" {
            let mut annotations = Map::new();
            put(&mut annotations, "extension", true);
            self.out.relate(
                &declared_name,
                &name,
                RelationshipType::Inheritance,
                node,
                annotations,
            );
        }
        for (index, (target, at)) in inherited.iter().enumerate() {
            let (source, kind, annotations) = match kind.kind() {
                "extension" => {
                    let mut annotations = Map::new();
                    put(&mut annotations, "via_extension", true);
                    (&name, RelationshipType::Implementation, annotations)
                }
                // The first parent is the superclass, the rest protocols.
                "class" if index == 0 && !target.starts_with("Any") => {
                    (&name, RelationshipType::Inheritance, Map::new())
                }
                _ => (&name, RelationshipType::Implementation, Map::new()),
            };
            self.out.relate(source, target, kind, *at, annotations);
        }
        self.body(node, &full_name);
    }

    fn protocol(&mut self, node: Node<'_>, parent: Option<&str>) {
        let Some(name_node) = node.child_by_field_name("name") else {
            return;
        };
        let name = type_name(self.out.text(name_node));
        let docstring = self.doc(node);
        let symbol = self.type_symbol(
            node,
            &name,
            SymbolType::Interface,
            parent,
            Map::new(),
            docstring,
        );
        let full_name = symbol.full_name.clone().unwrap_or_default();
        self.out.symbols.push(symbol);
        for (target, at) in self.inherited(node) {
            self.out.relate(
                &name,
                &target,
                RelationshipType::Inheritance,
                at,
                Map::new(),
            );
        }
        self.body(node, &full_name);
    }

    fn function(&mut self, node: Node<'_>, parent: Option<&str>) {
        let Some(name_node) = node.child_by_field_name("name") else {
            return;
        };
        let name = self.out.text(name_node).to_owned();
        let modifiers = self.modifiers(node);
        let is_async = has_token(node, "async") || modifiers.has("async");
        let is_static = modifiers.has("static") || modifiers.has("class");
        let return_type = node
            .child_by_field_name("return_type")
            .map(|t| self.out.text(t).trim().to_owned());
        let mut extra = Map::new();
        if !modifiers.words.is_empty() {
            put(&mut extra, "modifiers", word_list(&modifiers.words));
        }
        if is_async {
            put(&mut extra, "is_async", true);
        }
        if is_static {
            put(&mut extra, "is_static", true);
        }
        if let Some(return_type) = &return_type {
            put(&mut extra, "return_type", return_type.as_str());
        }
        let docstring = self.doc(node);
        let mut symbol =
            self.type_symbol(node, &name, SymbolType::Function, parent, extra, docstring);
        symbol.is_async = is_async;
        symbol.is_static = is_static;
        symbol.return_type = return_type;
        self.out.symbols.push(symbol);
    }

    fn property(&mut self, node: Node<'_>, parent: Option<&str>) {
        let modifiers = self.modifiers(node);
        let visibility = modifiers.visibility.as_deref().unwrap_or("internal");
        let binding = child_of_kind(node, "value_binding_pattern").or_else(|| {
            node.child_by_field_name("name")
                .and_then(|p| child_of_kind(p, "value_binding_pattern"))
        });
        let mutable = binding.is_some_and(|b| has_token(b, "var"));
        let declared = child_of_kind(node, "type_annotation").map(|t| {
            let text = self.out.text(t).trim();
            text.strip_prefix(':').unwrap_or(text).trim().to_owned()
        });
        let is_static = modifiers.has("static") || modifiers.has("class");
        let mut cursor = node.walk();
        let patterns: Vec<Node<'_>> = node.children_by_field_name("name", &mut cursor).collect();
        for pattern in patterns {
            let Some(bound) = pattern.child_by_field_name("bound_identifier") else {
                continue; // A tuple pattern.
            };
            let name = self.out.text(bound).to_owned();
            let mut metadata = Map::new();
            put(&mut metadata, "visibility", visibility);
            put(&mut metadata, "mutable", mutable);
            if let Some(declared) = &declared {
                put(&mut metadata, "type", declared.as_str());
            }
            if !modifiers.words.is_empty() {
                put(&mut metadata, "modifiers", word_list(&modifiers.words));
            }
            let mut symbol = self.out.symbol(&name, SymbolType::Variable, node, metadata);
            symbol.full_name = Some(self.qualify(&name));
            symbol.parent_symbol = parent.map(str::to_owned);
            symbol.visibility = Some(visibility.to_owned());
            symbol.is_static = is_static;
            self.out.symbols.push(symbol);
        }
    }

    fn type_alias(&mut self, node: Node<'_>, parent: Option<&str>) {
        let Some(name_node) = node.child_by_field_name("name") else {
            return;
        };
        let name = self.out.text(name_node).to_owned();
        let modifiers = self.modifiers(node);
        let visibility = modifiers.visibility.as_deref().unwrap_or("internal");
        let aliased = node.child_by_field_name("value");
        let mut metadata = Map::new();
        put(&mut metadata, "visibility", visibility);
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

    /// Attributes and calls anywhere in the tree under `node`.
    fn uses(&mut self, node: Node<'_>, names: &HashSet<String>) {
        match node.kind() {
            "attribute" => self.attribute(node),
            "call_expression" => self.call(node, names),
            _ => {}
        }
        for child in children(node) {
            self.uses(child, names);
        }
    }

    fn attribute(&mut self, node: Node<'_>) {
        let Some(user_type) = child_of_kind(node, "user_type") else {
            return;
        };
        let full = type_name(self.out.text(user_type));
        let name = full.rsplit('.').next().unwrap_or_default().to_owned();
        if !starts_upper(&name) || BUILTIN_ATTRIBUTES.contains(&name.as_str()) {
            return;
        }
        let stem = self.out.stem.clone();
        self.out
            .relate(&stem, &name, RelationshipType::Decorates, node, Map::new());
    }

    /// `Type(…)` is a `uses` edge, `a.f(…)` a `calls` edge; a subscript
    /// (`a[i]`, which the grammar also calls a call) is neither.
    fn call(&mut self, node: Node<'_>, names: &HashSet<String>) {
        let Some(suffix) = child_of_kind(node, "call_suffix") else {
            return;
        };
        let is_call = suffix.named_child(0).is_some_and(|first| {
            first.kind() == "lambda_literal" || self.out.text(first).starts_with('(')
        });
        let Some(callee) = node.named_child(0).filter(|_| is_call) else {
            return;
        };
        let stem = self.out.stem.clone();
        match callee.kind() {
            "simple_identifier" => {
                let name = self.out.text(callee);
                if starts_upper(name) && !names.contains(name) && !BUILTIN_TYPES.contains(&name) {
                    self.out
                        .relate(&stem, name, RelationshipType::Aggregation, node, Map::new());
                }
            }
            "navigation_expression" => {
                let name = callee
                    .child_by_field_name("suffix")
                    .and_then(|s| s.child_by_field_name("suffix"))
                    .filter(|s| s.kind() == "simple_identifier")
                    .map(|s| self.out.text(s));
                if let Some(name) = name
                    && !SKIPPED_METHODS.contains(&name)
                {
                    self.out
                        .relate(&stem, name, RelationshipType::Calls, node, Map::new());
                }
            }
            _ => {}
        }
    }
}

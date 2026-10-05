//! Function signatures and bodies: `calls`, `creates` and `references`.
//!
//! The Python body walk (`_walk_body`) handles a node and then STILL walks
//! all its children, except for `await`, closure and `return` expressions.
//! Python quirk: a struct literal walks its field values itself and then
//! again through that generic recursion, so a call or literal inside a
//! field value is recorded twice. The port keeps the duplicates; the
//! Python parser does not de-duplicate (`validate_result` is not called).

use super::visitor::{Visitor, child_of_kind, children, core_type_name, object};
use super::{is_builtin_macro, is_builtin_type};
use crate::parsers::model::RelationshipType;
use serde_json::{Map, Value};
use tree_sitter::Node;

/// Parameter type kinds (`_extract_parameter_types`).
const PARAMETER_TYPE_KINDS: &[&str] = &[
    "type_identifier",
    "generic_type",
    "reference_type",
    "scoped_type_identifier",
    "primitive_type",
    "dynamic_type",
    "abstract_type",
    "tuple_type",
    "array_type",
    "function_type",
];

/// What ends the search for a return type after `->`.
fn ends_return_search(kind: &str) -> bool {
    matches!(kind, "block" | "where_clause" | ";")
}

impl Visitor<'_> {
    /// `_has_self_parameter`: a `self_parameter`, or a parameter whose text
    /// starts with `self` (Python quirk: `selfish: u8` counts).
    pub(super) fn has_self_parameter(&self, node: Node<'_>) -> bool {
        let Some(params) = child_of_kind(node, "parameters") else {
            return false;
        };
        children(params).into_iter().any(|child| {
            matches!(child.kind(), "self_parameter" | "self")
                || (child.kind() == "parameter"
                    && super::visitor::py_strip(self.text(child)).starts_with("self"))
        })
    }

    /// The first child after `->` that is not the body, a where clause or
    /// `;`.
    fn return_type_node<'t>(&self, node: Node<'t>) -> Option<Node<'t>> {
        let mut found_arrow = false;
        for child in children(node) {
            if self.text(child) == "->" {
                found_arrow = true;
            } else if found_arrow && !ends_return_search(child.kind()) {
                return Some(child);
            }
        }
        None
    }

    /// `_extract_return_type`: the return type's text.
    pub(super) fn return_type(&self, node: Node<'_>) -> Option<&str> {
        self.return_type_node(node).map(|n| self.text(n))
    }

    /// `_extract_parameter_types`: each parameter's last type-kind child
    /// (a parameter of another type kind — `!`, `()`, `*const T` — is
    /// skipped), `self` for a `self` parameter.
    pub(super) fn parameter_types(&self, node: Node<'_>) -> Vec<String> {
        let mut types = Vec::new();
        let Some(params) = child_of_kind(node, "parameters") else {
            return types;
        };
        for child in children(params) {
            match child.kind() {
                "parameter" => {
                    if let Some(found) = children(child)
                        .into_iter()
                        .rev()
                        .find(|s| PARAMETER_TYPE_KINDS.contains(&s.kind()))
                    {
                        types.push(self.text(found).to_owned());
                    }
                }
                "self_parameter" => types.push("self".to_owned()),
                _ => {}
            }
        }
        types
    }

    /// `_build_function_signature`: `[async] [unsafe] fn name<T…> (params)
    /// [-> ret]`, parts joined by one space (so `fn f (x: u8)`).
    pub(super) fn function_signature(
        &self,
        name: &str,
        node: Node<'_>,
        return_type: Option<&str>,
    ) -> String {
        let modifiers = self.function_modifiers(node);
        let mut parts: Vec<String> = Vec::new();
        if modifiers.contains_key("is_async") {
            parts.push("async".to_owned());
        }
        if modifiers.contains_key("is_unsafe") {
            parts.push("unsafe".to_owned());
        }
        parts.push("fn".to_owned());
        let mut head = name.to_owned();
        if let Some(type_params) = child_of_kind(node, "type_parameters") {
            head.push_str(self.text(type_params));
        }
        parts.push(head);
        parts.push(
            child_of_kind(node, "parameters")
                .map_or_else(|| "()".to_owned(), |p| self.text(p).to_owned()),
        );
        if let Some(ret) = return_type.filter(|r| !r.is_empty()) {
            parts.push(format!("-> {ret}"));
        }
        parts.join(" ")
    }

    // ------------------------------------------------------------------
    // Signature references
    // ------------------------------------------------------------------

    /// `_extract_signature_type_references`: `dyn` / `impl` traits in the
    /// parameters and return type, then generic bounds, then where bounds.
    pub(super) fn signature_type_references(&mut self, source: &str, node: Node<'_>) {
        if let Some(params) = child_of_kind(node, "parameters") {
            self.trait_references(params, source);
        }
        if let Some(ret) = self.return_type_node(node) {
            self.trait_references(ret, source);
        }
        if let Some(list) = child_of_kind(node, "type_parameters") {
            for child in children(list) {
                if matches!(
                    child.kind(),
                    "constrained_type_parameter" | "type_parameter"
                ) {
                    self.bound_references(child, source, "generic_bound");
                }
            }
        }
        if let Some(clause) = child_of_kind(node, "where_clause") {
            for child in children(clause) {
                if child.kind() == "where_predicate" {
                    self.bound_references(child, source, "where_bound");
                }
            }
        }
    }

    /// `_extract_trait_references_recursive`: `dyn T` (dispatch `dynamic`)
    /// and `impl T` (dispatch `static`; `impl Fn(…)` is cut at `(`), then
    /// every child.
    fn trait_references(&mut self, node: Node<'_>, source: &str) {
        match node.kind() {
            "dynamic_type" => {
                let trait_node = child_of_kind(node, "type_identifier")
                    .or_else(|| child_of_kind(node, "scoped_type_identifier"));
                if let Some(trait_node) = trait_node {
                    let text = self.text(trait_node);
                    if !is_builtin_type(text) {
                        let annotations = object([("dispatch", Value::String("dynamic".into()))]);
                        self.relate(
                            source,
                            text,
                            RelationshipType::References,
                            trait_node,
                            annotations,
                        );
                    }
                }
            }
            "abstract_type" => {
                let trait_node = child_of_kind(node, "type_identifier")
                    .or_else(|| child_of_kind(node, "scoped_type_identifier"))
                    .or_else(|| child_of_kind(node, "function_type"));
                if let Some(trait_node) = trait_node {
                    let text = self.text(trait_node);
                    let core = text.split('(').next().unwrap_or(text);
                    if !is_builtin_type(core) {
                        let annotations = object([("dispatch", Value::String("static".into()))]);
                        self.relate(
                            source,
                            core,
                            RelationshipType::References,
                            trait_node,
                            annotations,
                        );
                    }
                }
            }
            _ => {}
        }
        for child in children(node) {
            self.trait_references(child, source);
        }
    }

    /// `_extract_generic_bound_references` / `_extract_where_clause_references`
    /// for one parameter or predicate: the bounded name is its first
    /// `type_identifier` (empty when the left side is not one).
    fn bound_references(&mut self, node: Node<'_>, source: &str, dispatch: &str) {
        let type_param = child_of_kind(node, "type_identifier").map_or("", |n| self.text(n));
        let Some(bounds) = child_of_kind(node, "trait_bounds") else {
            return;
        };
        for child in children(bounds) {
            let trait_name = match child.kind() {
                "type_identifier" | "scoped_type_identifier" => Some(self.text(child)),
                // `Iterator<Item = T>`: the generic's own name, at the
                // generic's range; a scoped generic gives nothing.
                "generic_type" => child_of_kind(child, "type_identifier").map(|n| self.text(n)),
                _ => None,
            };
            if let Some(trait_name) = trait_name.filter(|t| !is_builtin_type(t)) {
                let annotations = object([
                    ("dispatch", Value::String(dispatch.to_owned())),
                    ("type_param", Value::String(type_param.to_owned())),
                ]);
                self.relate(
                    source,
                    trait_name,
                    RelationshipType::References,
                    child,
                    annotations,
                );
            }
        }
    }

    // ------------------------------------------------------------------
    // Bodies
    // ------------------------------------------------------------------

    /// `_walk_body`.
    pub(super) fn walk_body(&mut self, node: Node<'_>, source: &str) {
        match node.kind() {
            "call_expression" => self.call_expression(node, source),
            "struct_expression" => self.struct_expression(node, source),
            "macro_invocation" => self.macro_invocation(node, source),
            "type_cast_expression" => self.type_cast(node, source),
            "closure_expression" => {
                // A block body is walked alone; an expression closure walks
                // every child (its parameters too).
                if let Some(block) = child_of_kind(node, "block") {
                    self.walk_body(block, source);
                    return;
                }
            }
            _ => {}
        }
        for child in children(node) {
            self.walk_body(child, source);
        }
    }

    /// `_handle_call_expression`.
    fn call_expression(&mut self, node: Node<'_>, source: &str) {
        let Some(function) = node.child(0) else {
            return;
        };
        let mut annotations = Map::new();
        let mut target: Option<String> = None;
        match function.kind() {
            "identifier" => target = Some(self.text(function).to_owned()),
            "field_expression" => {
                if let Some(field) = child_of_kind(function, "field_identifier") {
                    let left = function.child(0).map_or("", |n| self.text(n));
                    target = Some(format!("{left}.{}", self.text(field)));
                    annotations.insert("is_method_call".to_owned(), Value::Bool(true));
                }
            }
            "scoped_identifier" => {
                let text = self.text(function);
                if let Some((head, _)) = text.split_once("::")
                    && let Some(resolved) = self.extern_crate_aliases.get(head)
                {
                    annotations
                        .insert("resolved_crate".to_owned(), Value::String(resolved.clone()));
                }
                target = Some(text.to_owned());
            }
            "generic_function" => {
                let inner = child_of_kind(function, "identifier")
                    .or_else(|| child_of_kind(function, "scoped_identifier"))
                    .or_else(|| child_of_kind(function, "field_expression"));
                if let Some(inner) = inner {
                    target = Some(self.text(inner).to_owned());
                    annotations.insert("is_turbofish".to_owned(), Value::Bool(true));
                }
            }
            _ => {}
        }
        let Some(mut target) = target.filter(|t| !t.is_empty()) else {
            return;
        };
        if let Some(rest) = target.strip_prefix("Self::")
            && let Some(impl_target) = self.impl_target.as_deref()
        {
            target = format!("{impl_target}::{rest}");
            annotations.insert("via_self".to_owned(), Value::Bool(true));
        }
        // Python quirk (operator precedence): with a `.` in the last `::`
        // segment the call is kept iff anything follows the first dot;
        // otherwise iff the segment is not a builtin type.
        let base = target.rsplit_once("::").map_or(target.as_str(), |(_, r)| r);
        let keep = match base.split_once('.') {
            Some((_, after)) => !after.is_empty(),
            None => !is_builtin_type(base),
        };
        if keep {
            self.relate(source, &target, RelationshipType::Calls, node, annotations);
        }
    }

    /// `_handle_struct_expression`: a `creates` edge to the literal's type
    /// (`Self` resolved to the impl target), then its field values walked.
    fn struct_expression(&mut self, node: Node<'_>, source: &str) {
        let Some(type_node) = node.child(0) else {
            return;
        };
        if matches!(
            type_node.kind(),
            "type_identifier" | "scoped_type_identifier"
        ) {
            let mut target = self.text(type_node).to_owned();
            let mut annotations = Map::new();
            if target == "Self"
                && let Some(impl_target) = self.impl_target.as_deref()
            {
                target = impl_target.to_owned();
                annotations.insert("via_self".to_owned(), Value::Bool(true));
            }
            if !target.is_empty() && !is_builtin_type(&target) {
                self.relate(
                    source,
                    &target,
                    RelationshipType::Creates,
                    node,
                    annotations,
                );
            }
        }
        let Some(list) = child_of_kind(node, "field_initializer_list") else {
            return;
        };
        for child in children(list) {
            match child.kind() {
                "field_initializer" => {
                    for sub in children(child) {
                        if !matches!(sub.kind(), "field_identifier" | ":") {
                            self.walk_body(sub, source);
                        }
                    }
                }
                "base_field_initializer" => {
                    for sub in children(child) {
                        self.walk_body(sub, source);
                    }
                }
                _ => {}
            }
        }
    }

    /// `_handle_macro_invocation`: a `calls` edge to a non-builtin macro
    /// (`tracing::info` is not builtin; token trees hold no expressions).
    fn macro_invocation(&mut self, node: Node<'_>, source: &str) {
        let Some(name_node) =
            child_of_kind(node, "identifier").or_else(|| child_of_kind(node, "scoped_identifier"))
        else {
            return;
        };
        let name = self.text(name_node);
        if !name.is_empty() && !is_builtin_macro(name) {
            let annotations = object([("is_macro", Value::Bool(true))]);
            self.relate(source, name, RelationshipType::Calls, node, annotations);
        }
    }

    /// `_handle_type_cast`: `x as T` references `T`'s core name.
    fn type_cast(&mut self, node: Node<'_>, source: &str) {
        for child in children(node) {
            if matches!(
                child.kind(),
                "type_identifier" | "generic_type" | "scoped_type_identifier"
            ) {
                let core = core_type_name(self.text(child));
                if !core.is_empty() && !is_builtin_type(&core) {
                    let annotations = object([("via_cast", Value::Bool(true))]);
                    self.relate(
                        source,
                        &core,
                        RelationshipType::References,
                        child,
                        annotations,
                    );
                }
            }
        }
    }
}

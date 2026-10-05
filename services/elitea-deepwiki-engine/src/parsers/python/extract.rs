//! One Python file: `PythonParser.parse_file` and `extract_relationships`.
//!
//! Each visitor is the Python visitor over the same `ast` tree, in the same
//! traversal order (`generic_visit` follows `_fields`, `ast.walk` is breadth
//! first), so even the relationship order matches. Every quirk kept on
//! purpose is marked "Python quirk".

use super::ast::{
    self, ClassDef, Const, Ctx, Expr, ExprKind, FunctionDef, Node, Stmt, StmtKind, walk,
    walk_module,
};
use super::text::{cleandoc, is_upper, repr_bytes, repr_float, repr_imaginary};
use super::unparse::unparse;
use crate::parsers::model::{
    ParseResult, Position, Range, Relationship, RelationshipType, Scope, Symbol, SymbolType,
};
use indexmap::IndexMap;
use serde_json::{Map, Value, json};
use std::collections::{BTreeSet, HashMap, HashSet};

/// Names `visit_Name` never records as references.
const EXCLUDED_NAMES: &[&str] = &[
    "self",
    "cls",
    "True",
    "False",
    "None",
    "print",
    "len",
    "str",
    "int",
    "float",
    "bool",
    "list",
    "dict",
    "tuple",
    "set",
    "range",
    "enumerate",
    "zip",
    "map",
    "filter",
    "sum",
    "min",
    "max",
    "abs",
    "all",
    "any",
];

/// `_TYPING_BUILTINS`: generic wrappers whose own name is not a reference.
const TYPING_BUILTINS: &[&str] = &[
    "List",
    "Dict",
    "Set",
    "Tuple",
    "FrozenSet",
    "Deque",
    "Optional",
    "Union",
    "Callable",
    "Iterator",
    "Generator",
    "AsyncIterator",
    "AsyncGenerator",
    "Coroutine",
    "ClassVar",
    "Final",
    "Literal",
    "Annotated",
    "Type",
    "Sequence",
    "Mapping",
    "MutableMapping",
    "MutableSequence",
    "Iterable",
    "Collection",
    "MutableSet",
    "OrderedDict",
    "DefaultDict",
    "Counter",
    "ChainMap",
    "list",
    "dict",
    "set",
    "tuple",
    "frozenset",
    "type",
];

/// `_PRIMITIVE_TYPES`.
const PRIMITIVE_TYPES: &[&str] = &[
    "str",
    "int",
    "float",
    "bool",
    "bytes",
    "bytearray",
    "complex",
    "memoryview",
    "object",
    "None",
    "NoneType",
    "Any",
    "NoReturn",
    "Never",
];

/// The built-ins `_extract_fields_and_composition` never links a field to.
const FIELD_BUILTINS: &[&str] = &[
    "str",
    "int",
    "float",
    "bool",
    "list",
    "dict",
    "tuple",
    "set",
    "frozenset",
    "bytes",
    "bytearray",
    "range",
    "complex",
    "type",
    "object",
    "None",
    "NoneType",
];

fn range_of(loc: ast::Loc) -> Range {
    Range::new(loc.line, loc.col, loc.end_line, loc.end_col)
}

/// `Path(file_path).stem`.
pub fn stem(file: &str) -> String {
    std::path::Path::new(file)
        .file_stem()
        .map(|s| s.to_string_lossy().into_owned())
        .unwrap_or_default()
}

/// `parse_file` after `ast.parse` succeeded: symbols, relationships with
/// only this file's classes known, fields, module info, validation.
pub fn extract_file(file: &str, source: &str, module: &[Stmt]) -> ParseResult {
    let mut symbols = extract_symbols(file, source, module);
    let no_globals = HashSet::new();
    let (relationships, fields) = extract_relationships(file, module, &symbols, &no_globals);
    symbols.extend(fields);
    let (imports, exports) = module_info(module);
    let dependencies = dependencies(&imports, &relationships);
    let mut result = ParseResult::new(file, "python");
    result.symbols = symbols;
    result.relationships = relationships;
    result.imports = imports;
    result.exports = exports;
    result.dependencies = dependencies;
    result.module_docstring = docstring(module);
    result.validate();
    result
}

/// `ast.get_docstring`: the first statement, if a plain string constant,
/// cleaned with `inspect.cleandoc`.
pub fn docstring(body: &[Stmt]) -> Option<String> {
    match body.first().map(|s| &s.kind) {
        Some(StmtKind::Expr(Expr {
            kind:
                ExprKind::Constant {
                    value: Const::Str(text),
                    ..
                },
            ..
        })) => Some(cleandoc(text)),
        _ => None,
    }
}

/// `_extract_node_source`: the WHOLE lines the node spans (`content`
/// split on `\n`), or `""` when the span runs past the end.
fn node_source(lines: &[&str], loc: ast::Loc) -> String {
    let start = loc.line.saturating_sub(1) as usize;
    let end = loc.end_line as usize;
    if start < lines.len() && end <= lines.len() && start <= end {
        lines[start..end].join("\n")
    } else {
        String::new()
    }
}

/// `_get_full_attribute_name`: the dotted chain; a non-name base (`f().x`)
/// is dropped, so `f().x.y` gives `x.y`.
fn full_attribute_name(expr: &Expr) -> String {
    let mut parts = Vec::new();
    let mut current = expr;
    while let ExprKind::Attribute { value, attr, .. } = &current.kind {
        parts.push(attr.as_str());
        current = value;
    }
    if let ExprKind::Name { id, .. } = &current.kind {
        parts.push(id);
    }
    parts.reverse();
    parts.join(".")
}

/// `_get_decorator_name`.
fn decorator_name(expr: &Expr) -> String {
    match &expr.kind {
        ExprKind::Name { id, .. } => id.clone(),
        ExprKind::Attribute { .. } => full_attribute_name(expr),
        ExprKind::Call { func, .. } => match &func.kind {
            ExprKind::Name { id, .. } => id.clone(),
            ExprKind::Attribute { .. } => full_attribute_name(func),
            _ => String::new(),
        },
        _ => String::new(),
    }
}

/// `str(constant.value)`.
fn constant_str(value: &Const) -> String {
    match value {
        Const::None => "None".to_owned(),
        Const::Bool(true) => "True".to_owned(),
        Const::Bool(false) => "False".to_owned(),
        Const::Ellipsis => "Ellipsis".to_owned(),
        Const::Str(s) => s.clone(),
        Const::Bytes(b) => repr_bytes(b),
        Const::Int(digits) => digits.clone(),
        Const::Float(f) => repr_float(*f),
        Const::Complex(f) => repr_imaginary(*f),
    }
}

/// `_get_type_annotation`: names, dotted names, constants (as `str()`) and
/// subscripts by hand; everything else through `ast.unparse` — Python
/// quirk: a tuple slice is unparsed alone, so `Dict[str, int]` gives
/// `Dict[(str, int)]`.
pub fn type_annotation(expr: &Expr) -> String {
    match &expr.kind {
        ExprKind::Name { id, .. } => id.clone(),
        ExprKind::Attribute { .. } => full_attribute_name(expr),
        ExprKind::Constant { value, .. } => constant_str(value),
        ExprKind::Subscript { value, slice, .. } => {
            format!("{}[{}]", type_annotation(value), type_annotation(slice))
        }
        _ => unparse(expr),
    }
}

/// One `_extract_function_parameters` entry.
struct Param {
    name: String,
    type_: Option<String>,
    range: Range,
}

/// `_extract_function_parameters`. Python quirk: positional-only and
/// keyword-only parameters are left out.
fn function_parameters(def: &FunctionDef) -> Vec<Param> {
    let mut params = Vec::new();
    for arg in def
        .args
        .args
        .iter()
        .chain(&def.args.vararg)
        .chain(&def.args.kwarg)
    {
        params.push(Param {
            name: arg.name.clone(),
            type_: arg.annotation.as_ref().map(type_annotation),
            range: range_of(arg.loc),
        });
    }
    params
}

/// `_build_function_signature`: `name(a: T, *args, **kw) -> R`, from the
/// same parameters (no defaults, no positional/keyword-only ones).
fn function_signature(def: &FunctionDef, return_type: Option<&str>) -> String {
    let mut params = Vec::new();
    for arg in &def.args.args {
        let mut text = arg.name.clone();
        if let Some(annotation) = &arg.annotation {
            text.push_str(": ");
            text.push_str(&type_annotation(annotation));
        }
        params.push(text);
    }
    for (prefix, arg) in [("*", &def.args.vararg), ("**", &def.args.kwarg)] {
        if let Some(arg) = arg {
            let mut text = format!("{prefix}{}", arg.name);
            if let Some(annotation) = &arg.annotation {
                text.push_str(": ");
                text.push_str(&type_annotation(annotation));
            }
            params.push(text);
        }
    }
    let mut signature = format!("{}({})", def.name, params.join(", "));
    if let Some(return_type) = return_type.filter(|r| !r.is_empty()) {
        signature.push_str(" -> ");
        signature.push_str(return_type);
    }
    signature
}

// ----- symbols ------------------------------------------------------------

/// `extract_symbols` (`SymbolExtractor`).
pub fn extract_symbols(file: &str, source: &str, module: &[Stmt]) -> Vec<Symbol> {
    let lines: Vec<&str> = source.split('\n').collect();
    let mut extractor = SymbolExtractor {
        file,
        lines: &lines,
        symbols: Vec::new(),
        scope_stack: vec![stem(file)],
        class_names: HashSet::new(),
    };
    for stmt in module {
        extractor.visit(stmt);
    }
    extractor.symbols
}

struct SymbolExtractor<'a> {
    file: &'a str,
    lines: &'a [&'a str],
    symbols: Vec<Symbol>,
    scope_stack: Vec<String>,
    /// Every class seen so far in the file, at any depth.
    class_names: HashSet<String>,
}

impl SymbolExtractor<'_> {
    /// `'.'.join(scope_stack) if scope_stack else None` and the full name
    /// built on it.
    fn names(&self, name: &str) -> (Option<String>, String) {
        let parent = self.scope_stack.join(".");
        let full = if parent.is_empty() {
            name.to_owned()
        } else {
            format!("{parent}.{name}")
        };
        ((!self.scope_stack.is_empty()).then_some(parent), full)
    }

    fn is_module_level(&self) -> bool {
        self.scope_stack.len() <= 1
    }

    /// Only statements hold definitions and assignments, so the walk stays
    /// on statement lists — in `_fields` order, as `generic_visit` goes.
    fn visit(&mut self, stmt: &Stmt) {
        match &stmt.kind {
            StmtKind::ClassDef(def) => self.class(stmt, def),
            StmtKind::FunctionDef(def) => self.function(stmt, def),
            StmtKind::Assign { targets, .. } => {
                for target in targets {
                    if let ExprKind::Name { id, .. } = &target.kind {
                        let symbol = self.variable(id, target, stmt);
                        self.symbols.push(symbol);
                    }
                }
            }
            StmtKind::AnnAssign {
                target, annotation, ..
            } => {
                if let ExprKind::Name { id, .. } = &target.kind {
                    let mut symbol = self.variable(id, target, stmt);
                    symbol.return_type = Some(type_annotation(annotation));
                    self.symbols.push(symbol);
                }
            }
            _ => {
                for_each_nested_body(stmt, &mut |body| {
                    for inner in body {
                        self.visit(inner);
                    }
                });
            }
        }
    }

    /// `visit_Assign` / `visit_AnnAssign`: Python quirk — every
    /// module-level name is a constant; elsewhere only an upper-case one.
    fn variable(&self, id: &str, target: &Expr, stmt: &Stmt) -> Symbol {
        let (parent, _) = self.names(id);
        let is_module = self.is_module_level();
        let symbol_type = if is_module || is_upper(id) {
            SymbolType::Constant
        } else {
            SymbolType::Variable
        };
        let scope = if is_module {
            Scope::Global
        } else {
            Scope::Function
        };
        let mut symbol = Symbol::new(id, symbol_type, scope, range_of(target.loc), self.file);
        symbol.parent_symbol = parent;
        symbol.source_text = Some(node_source(self.lines, stmt.loc));
        symbol
    }

    fn class(&mut self, stmt: &Stmt, def: &ClassDef) {
        let (parent, full) = self.names(&def.name);
        let scope = if self.is_module_level() {
            Scope::Global
        } else {
            Scope::Class
        };
        let mut symbol = Symbol::new(
            &def.name,
            SymbolType::Class,
            scope,
            range_of(stmt.loc),
            self.file,
        );
        symbol.parent_symbol = parent;
        symbol.full_name = Some(full);
        symbol.docstring = docstring(&def.body);
        symbol.source_text = Some(node_source(self.lines, stmt.loc));
        self.symbols.push(symbol);
        self.class_names.insert(def.name.clone());
        self.scope_stack.push(def.name.clone());
        for inner in &def.body {
            self.visit(inner);
        }
        self.scope_stack.pop();
    }

    fn function(&mut self, stmt: &Stmt, def: &FunctionDef) {
        let (parent, full) = self.names(&def.name);
        // Python quirk: "in a class" means any enclosing scope NAME equals
        // any class name seen so far in the file (the module stem too).
        let in_class = self
            .scope_stack
            .iter()
            .any(|s| self.class_names.contains(s));
        let symbol_type = match (in_class, def.name == "__init__") {
            (true, true) => SymbolType::Constructor,
            (true, false) => SymbolType::Method,
            _ => SymbolType::Function,
        };
        let decorators: Vec<Value> = def
            .decorators
            .iter()
            .map(|d| Value::String(decorator_name(d)))
            .collect();
        let return_type = def.returns.as_ref().map(type_annotation);
        let params = function_parameters(def);
        let range = range_of(stmt.loc);
        let mut symbol = Symbol::new(&def.name, symbol_type, Scope::Function, range, self.file);
        symbol.parent_symbol = parent;
        symbol.full_name = Some(full.clone());
        symbol.docstring = docstring(&def.body);
        symbol.parameter_types = params
            .iter()
            .filter_map(|p| p.type_.clone().filter(|t| !t.is_empty()))
            .collect();
        symbol.is_async = def.is_async;
        symbol.source_text = Some(node_source(self.lines, stmt.loc));
        symbol.signature = Some(function_signature(def, return_type.as_deref()));
        symbol.return_type = return_type;
        symbol
            .metadata
            .insert("decorators".to_owned(), Value::Array(decorators));
        self.symbols.push(symbol);
        for param in params {
            let mut symbol = Symbol::new(
                param.name,
                SymbolType::Parameter,
                Scope::Function,
                param.range,
                self.file,
            );
            symbol.parent_symbol = Some(full.clone());
            symbol.return_type = param.type_;
            self.symbols.push(symbol);
        }
        self.scope_stack.push(def.name.clone());
        for inner in &def.body {
            self.visit(inner);
        }
        self.scope_stack.pop();
    }
}

/// The statement lists nested in a compound statement other than a
/// definition, in `_fields` order.
fn for_each_nested_body<'a>(stmt: &'a Stmt, f: &mut impl FnMut(&'a [Stmt])) {
    match &stmt.kind {
        StmtKind::For { body, orelse, .. }
        | StmtKind::While { body, orelse, .. }
        | StmtKind::If { body, orelse, .. } => {
            f(body);
            f(orelse);
        }
        StmtKind::With { body, .. } => f(body),
        StmtKind::Try {
            body,
            handlers,
            orelse,
            finalbody,
        } => {
            f(body);
            for handler in handlers {
                f(&handler.body);
            }
            f(orelse);
            f(finalbody);
        }
        StmtKind::Match { cases, .. } => {
            for case in cases {
                f(&case.body);
            }
        }
        StmtKind::FunctionDef(def) => f(&def.body),
        StmtKind::ClassDef(def) => f(&def.body),
        _ => {}
    }
}

// ----- relationships ------------------------------------------------------

fn relationship(
    source: &str,
    target: &str,
    rel_type: RelationshipType,
    file: &str,
    loc: ast::Loc,
    confidence: f64,
    weight: f64,
) -> Relationship {
    let mut relationship = Relationship::new(source, target, rel_type, file);
    relationship.source_range = Some(range_of(loc));
    relationship.confidence = confidence;
    relationship.weight = weight;
    relationship
}

/// `extract_relationships`: the visitor, then DEFINES (pass 2), then
/// fields and composition (pass 3). Returns the relationships and the
/// field symbols.
///
/// `global_classes` is `_global_class_locations`: empty in pass 1, every
/// class of every parsed file in the re-extraction.
pub fn extract_relationships(
    file: &str,
    module: &[Stmt],
    symbols: &[Symbol],
    global_classes: &HashSet<String>,
) -> (Vec<Relationship>, Vec<Symbol>) {
    let local_classes: HashSet<&str> = symbols
        .iter()
        .filter(|s| s.symbol_type == SymbolType::Class)
        .map(|s| s.name.as_str())
        .collect();
    let mut extractor = RelationshipExtractor {
        file,
        stem: stem(file),
        local_classes: &local_classes,
        global_classes,
        relationships: Vec::new(),
        current: None,
    };
    for stmt in module {
        extractor.visit(Node::Stmt(stmt));
    }
    let mut relationships = extractor.relationships;
    relationships.extend(defines_relationships(file, symbols));
    let (fields, composition) = fields_and_composition(file, module, symbols);
    relationships.extend(composition);
    (relationships, fields)
}

struct RelationshipExtractor<'a> {
    file: &'a str,
    stem: String,
    local_classes: &'a HashSet<&'a str>,
    global_classes: &'a HashSet<String>,
    relationships: Vec<Relationship>,
    /// `current_symbol`: the enclosing class or function NAME.
    current: Option<String>,
}

impl RelationshipExtractor<'_> {
    fn visit(&mut self, node: Node<'_>) {
        match node {
            Node::Stmt(stmt) => match &stmt.kind {
                StmtKind::ClassDef(def) => self.class(node, def),
                StmtKind::FunctionDef(def) => self.function(node, def),
                StmtKind::Import(names) => {
                    for alias in names {
                        let imported = alias.asname.as_ref().unwrap_or(&alias.name);
                        self.import(imported, stmt.loc);
                    }
                }
                StmtKind::ImportFrom { module, names } => {
                    let module = module.as_deref().unwrap_or("");
                    for alias in names {
                        let target = match &alias.asname {
                            Some(asname) => asname.clone(),
                            None if !module.is_empty() && alias.name != "*" => {
                                format!("{module}.{}", alias.name)
                            }
                            None => alias.name.clone(),
                        };
                        self.import(&target, stmt.loc);
                    }
                }
                _ => self.generic(node),
            },
            Node::Expr(expr) => match &expr.kind {
                ExprKind::Call { func, .. } => {
                    self.call(expr, func);
                    self.generic(node);
                }
                ExprKind::Name { id, ctx } => self.name(expr, id, *ctx),
                _ => self.generic(node),
            },
            _ => self.generic(node),
        }
    }

    fn generic(&mut self, node: Node<'_>) {
        ast::for_each_child(node, &mut |child| self.visit(child));
    }

    fn import(&mut self, target: &str, loc: ast::Loc) {
        let relationship = relationship(
            &self.stem,
            target,
            RelationshipType::Imports,
            self.file,
            loc,
            1.0,
            0.5,
        );
        self.relationships.push(relationship);
    }

    /// `visit_ClassDef`. Python quirk: `current_symbol` is reset to `None`
    /// after the class, not restored, so whatever follows a nested class in
    /// its enclosing body records no calls or references.
    fn class(&mut self, node: Node<'_>, def: &ClassDef) {
        self.current = Some(def.name.clone());
        for base in &def.bases {
            let base_name = match &base.kind {
                ExprKind::Name { id, .. } => id.clone(),
                ExprKind::Attribute { .. } => full_attribute_name(base),
                ExprKind::Subscript { value, slice, .. } => {
                    let mut params = Vec::new();
                    match &slice.kind {
                        ExprKind::Name { id, .. } => params.push(id.clone()),
                        ExprKind::Tuple { elts, .. } => {
                            for elt in elts {
                                match &elt.kind {
                                    ExprKind::Name { id, .. } => params.push(id.clone()),
                                    ExprKind::Attribute { .. } => {
                                        params.push(full_attribute_name(elt));
                                    }
                                    _ => {}
                                }
                            }
                        }
                        ExprKind::Attribute { .. } => params.push(full_attribute_name(slice)),
                        _ => {}
                    }
                    for param in params.iter().filter(|p| !p.is_empty()) {
                        let relationship = relationship(
                            &def.name,
                            param,
                            RelationshipType::References,
                            self.file,
                            slice.loc,
                            0.8,
                            0.6,
                        );
                        self.relationships.push(relationship);
                    }
                    match &value.kind {
                        ExprKind::Name { id, .. } => id.clone(),
                        ExprKind::Attribute { .. } => full_attribute_name(value),
                        _ => String::new(),
                    }
                }
                _ => String::new(),
            };
            if !base_name.is_empty() {
                let relationship = relationship(
                    &def.name,
                    &base_name,
                    RelationshipType::Inheritance,
                    self.file,
                    base.loc,
                    0.9,
                    0.8,
                );
                self.relationships.push(relationship);
            }
        }
        for decorator in &def.decorators {
            let name = decorator_name(decorator);
            if !name.is_empty() {
                let relationship = relationship(
                    &name,
                    &def.name,
                    RelationshipType::Decorates,
                    self.file,
                    decorator.loc,
                    0.95,
                    0.6,
                );
                self.relationships.push(relationship);
            }
        }
        self.generic(node);
        self.current = None;
    }

    /// `_visit_function`. Python quirk: a function's decorator edge runs
    /// function → decorator, a class's runs decorator → class.
    fn function(&mut self, node: Node<'_>, def: &FunctionDef) {
        let old = self.current.replace(def.name.clone());
        for decorator in &def.decorators {
            let name = decorator_name(decorator);
            if !name.is_empty() {
                let relationship = relationship(
                    &def.name,
                    &name,
                    RelationshipType::Decorates,
                    self.file,
                    decorator.loc,
                    0.95,
                    0.6,
                );
                self.relationships.push(relationship);
            }
        }
        for arg in &def.args.args {
            if let Some(annotation) = &arg.annotation {
                type_references(
                    annotation,
                    &def.name,
                    self.file,
                    "parameter_type",
                    &mut self.relationships,
                );
            }
        }
        if let Some(returns) = &def.returns {
            type_references(
                returns,
                &def.name,
                self.file,
                "return_type",
                &mut self.relationships,
            );
        }
        self.generic(node);
        self.current = old;
    }

    /// `visit_Call`: a call of a known class name is `creates`.
    fn call(&mut self, call: &Expr, func: &Expr) {
        let Some(current) = self.current.clone() else {
            return;
        };
        let (called, module_prefix) = match &func.kind {
            ExprKind::Name { id, .. } => (id.as_str(), None),
            ExprKind::Attribute { value, attr, .. } => (
                attr.as_str(),
                match &value.kind {
                    ExprKind::Name { id, .. } => Some(id.clone()),
                    _ => None,
                },
            ),
            _ => return,
        };
        if called.is_empty() {
            return;
        }
        let is_constructor =
            self.local_classes.contains(called) || self.global_classes.contains(called);
        let mut relationship = if is_constructor {
            relationship(
                &current,
                called,
                RelationshipType::Creates,
                self.file,
                call.loc,
                0.95,
                0.8,
            )
        } else {
            relationship(
                &current,
                called,
                RelationshipType::Calls,
                self.file,
                call.loc,
                0.8,
                0.7,
            )
        };
        if is_constructor {
            let annotations = &mut relationship.annotations;
            annotations.insert("creation_context".to_owned(), json!("call"));
            annotations.insert("detection_method".to_owned(), json!("ast_registry"));
            if let Some(prefix) = module_prefix {
                annotations.insert("module_prefix".to_owned(), Value::String(prefix));
            }
        }
        self.relationships.push(relationship);
    }

    fn name(&mut self, expr: &Expr, id: &str, ctx: Ctx) {
        let Some(current) = self.current.as_deref() else {
            return;
        };
        if ctx != Ctx::Load || EXCLUDED_NAMES.contains(&id) || id == current {
            return;
        }
        let relationship = relationship(
            current,
            id,
            RelationshipType::References,
            self.file,
            expr.loc,
            0.7,
            0.3,
        );
        self.relationships.push(relationship);
    }
}

/// `_extract_type_references_recursive`.
fn type_references(
    annotation: &Expr,
    source: &str,
    file: &str,
    reference_type: &str,
    out: &mut Vec<Relationship>,
) {
    let user_defined =
        |name: &str| !PRIMITIVE_TYPES.contains(&name) && !TYPING_BUILTINS.contains(&name);
    let push =
        |target: &str, loc: ast::Loc, reference_type: String, out: &mut Vec<Relationship>| {
            let mut relationship = relationship(
                source,
                target,
                RelationshipType::References,
                file,
                loc,
                0.9,
                0.7,
            );
            relationship
                .annotations
                .insert("reference_type".to_owned(), Value::String(reference_type));
            out.push(relationship);
        };
    match &annotation.kind {
        ExprKind::Name { id, .. } => {
            if user_defined(id) {
                push(id, annotation.loc, reference_type.to_owned(), out);
            }
        }
        ExprKind::Attribute { attr, .. } => {
            if user_defined(attr) {
                push(attr, annotation.loc, reference_type.to_owned(), out);
            }
        }
        ExprKind::Subscript { value, slice, .. } => {
            let outer = match &value.kind {
                ExprKind::Name { id, .. } => Some(id.as_str()),
                ExprKind::Attribute { attr, .. } => Some(attr.as_str()),
                _ => None,
            };
            if let Some(outer) = outer.filter(|o| !o.is_empty() && user_defined(o)) {
                push(outer, value.loc, format!("{reference_type}_generic"), out);
            }
            let arg_ref = format!("{reference_type}_generic_arg");
            match &slice.kind {
                ExprKind::Tuple { elts, .. } => {
                    for elt in elts {
                        type_references(elt, source, file, &arg_ref, out);
                    }
                }
                _ => type_references(slice, source, file, &arg_ref, out),
            }
        }
        ExprKind::BinOp {
            left,
            op: ast::BinOp::BitOr,
            right,
        } => {
            type_references(left, source, file, reference_type, out);
            type_references(right, source, file, reference_type, out);
        }
        ExprKind::Tuple { elts, .. } | ExprKind::List { elts, .. } => {
            for elt in elts {
                type_references(elt, source, file, reference_type, out);
            }
        }
        _ => {}
    }
}

/// `_extract_defines_relationships`: class → member edges at `0:0-0:0`
/// (Python quirk: `Symbol` has no `source_code_location`, so the fallback
/// range is always used). Members are looked up by the class's full name,
/// then by its bare name.
fn defines_relationships(file: &str, symbols: &[Symbol]) -> Vec<Relationship> {
    let mut by_parent: HashMap<&str, Vec<&Symbol>> = HashMap::new();
    for symbol in symbols {
        if let Some(parent) = symbol.parent_symbol.as_deref().filter(|p| !p.is_empty()) {
            by_parent.entry(parent).or_default().push(symbol);
        }
    }
    let mut out = Vec::new();
    for class in symbols
        .iter()
        .filter(|s| s.symbol_type == SymbolType::Class)
    {
        let class_full = class.full_name.as_deref().unwrap_or("");
        let members = by_parent
            .get(class_full)
            .filter(|m| !m.is_empty())
            .or_else(|| by_parent.get(class.name.as_str()));
        let Some(members) = members else {
            continue;
        };
        for member in members {
            let member_type = match member.symbol_type {
                SymbolType::Method => "method",
                SymbolType::Constructor => "constructor",
                SymbolType::Variable | SymbolType::Constant => "variable",
                SymbolType::Field => "field",
                _ => continue,
            };
            let member_full = match member.full_name.as_deref().filter(|f| !f.is_empty()) {
                Some(full) => full.to_owned(),
                None if !class_full.is_empty() => format!("{class_full}.{}", member.name),
                None => member.name.clone(),
            };
            let decorators: Vec<Value> = member
                .metadata
                .get("decorators")
                .and_then(Value::as_array)
                .cloned()
                .unwrap_or_default();
            let has = |name: &str| decorators.iter().any(|d| d.as_str() == Some(name));
            let mut relationship =
                Relationship::new(class_full, member_full, RelationshipType::Defines, file);
            relationship.source_range = Some(Range {
                start: Position { line: 0, column: 0 },
                end: Position { line: 0, column: 0 },
            });
            let mut annotations = Map::new();
            annotations.insert("member_type".to_owned(), json!(member_type));
            annotations.insert("is_static".to_owned(), json!(has("staticmethod")));
            annotations.insert("is_classmethod".to_owned(), json!(has("classmethod")));
            annotations.insert("is_property".to_owned(), json!(has("property")));
            annotations.insert("is_abstract".to_owned(), json!(has("abstractmethod")));
            annotations.insert("decorators".to_owned(), Value::Array(decorators));
            relationship.annotations = annotations;
            out.push(relationship);
        }
    }
    out
}

/// The `ClassDef` statements of the module in `ast.walk` (breadth-first)
/// order.
fn classes_breadth_first(module: &[Stmt]) -> Vec<&Stmt> {
    let mut classes = Vec::new();
    walk_module(module, &mut |node| {
        if let Node::Stmt(stmt) = node
            && matches!(stmt.kind, StmtKind::ClassDef(_))
        {
            classes.push(stmt);
        }
    });
    classes
}

fn class_def(stmt: &Stmt) -> Option<&ClassDef> {
    match &stmt.kind {
        StmtKind::ClassDef(def) => Some(def),
        _ => None,
    }
}

/// A field found in an assignment.
#[derive(Default)]
struct FieldShape {
    field_type: Option<String>,
    module_prefix: Option<String>,
    is_optional: bool,
}

/// The type an annotation gives a field: a name, the last part of a
/// dotted name, or the (single) argument of a subscript — whose outer name
/// says whether it is `Optional`.
fn annotation_shape(annotation: &Expr) -> FieldShape {
    let mut shape = FieldShape::default();
    match &annotation.kind {
        ExprKind::Name { id, .. } => shape.field_type = Some(id.clone()),
        ExprKind::Attribute { value, attr, .. } => {
            shape.field_type = Some(attr.clone());
            if let ExprKind::Name { id, .. } = &value.kind {
                shape.module_prefix = Some(id.clone());
            }
        }
        ExprKind::Subscript { value, slice, .. } => {
            let outer = match &value.kind {
                ExprKind::Name { id, .. } | ExprKind::Attribute { attr: id, .. } => {
                    Some(id.as_str())
                }
                _ => None,
            };
            shape.is_optional = outer == Some("Optional");
            match &slice.kind {
                ExprKind::Name { id, .. } => shape.field_type = Some(id.clone()),
                ExprKind::Attribute { value, attr, .. } => {
                    shape.field_type = Some(attr.clone());
                    if let ExprKind::Name { id, .. } = &value.kind {
                        shape.module_prefix = Some(id.clone());
                    }
                }
                _ => {}
            }
        }
        _ => {}
    }
    shape
}

fn is_none(expr: &Expr) -> bool {
    matches!(
        expr.kind,
        ExprKind::Constant {
            value: Const::None,
            ..
        }
    )
}

/// The attribute name of a `self.<name>` target.
fn self_attribute(target: &Expr) -> Option<&str> {
    match &target.kind {
        ExprKind::Attribute { value, attr, .. } => match &value.kind {
            ExprKind::Name { id, .. } if id == "self" => Some(attr),
            _ => None,
        },
        _ => None,
    }
}

/// `_extract_fields_and_composition`.
#[allow(clippy::too_many_lines)]
fn fields_and_composition(
    file: &str,
    module: &[Stmt],
    symbols: &[Symbol],
) -> (Vec<Symbol>, Vec<Relationship>) {
    let mut fields: Vec<Symbol> = Vec::new();
    let mut relationships = Vec::new();
    // A dict: a repeated full name keeps its first position, last value.
    let mut class_index: IndexMap<&str, &Symbol> = IndexMap::new();
    for symbol in symbols
        .iter()
        .filter(|s| s.symbol_type == SymbolType::Class)
    {
        class_index.insert(symbol.full_name.as_deref().unwrap_or(""), symbol);
    }
    let classes = classes_breadth_first(module);
    for init in symbols.iter().filter(|s| {
        matches!(s.symbol_type, SymbolType::Method | SymbolType::Constructor)
            && s.name == "__init__"
    }) {
        let Some(parent_name) = init.parent_symbol.as_deref().filter(|p| !p.is_empty()) else {
            continue;
        };
        let Some(parent) = class_index.get(parent_name) else {
            continue;
        };
        let parent_full = parent.full_name.clone().unwrap_or_default();
        // `_find_function_node`: Python quirk — the FIRST class (breadth
        // first) whose name is the parent's last part, then a method of
        // that name directly in its body.
        let class_node = classes
            .iter()
            .filter_map(|s| class_def(s))
            .find(|c| c.name == parent_name || parent_name.ends_with(&format!(".{}", c.name)));
        let Some(init_stmt) = class_node.and_then(|c| {
            c.body
                .iter()
                .find(|s| matches!(&s.kind, StmtKind::FunctionDef(f) if f.name == init.name))
        }) else {
            continue;
        };
        let mut seen: HashSet<String> = HashSet::new();
        let mut assignments: Vec<&Stmt> = Vec::new();
        walk(Node::Stmt(init_stmt), &mut |node| {
            if let Node::Stmt(stmt) = node
                && matches!(
                    stmt.kind,
                    StmtKind::Assign { .. } | StmtKind::AnnAssign { .. }
                )
            {
                assignments.push(stmt);
            }
        });
        for stmt in assignments {
            let mut shape = FieldShape::default();
            let mut field_name = None;
            let mut is_instantiation = false;
            let mut initialized_to_none = false;
            match &stmt.kind {
                StmtKind::Assign { targets, value } => {
                    if let [target] = targets.as_slice()
                        && let Some(name) = self_attribute(target)
                    {
                        field_name = Some(name.to_owned());
                        match &value.kind {
                            ExprKind::Call { func, .. } => {
                                is_instantiation = true;
                                match &func.kind {
                                    ExprKind::Name { id, .. } => {
                                        shape.field_type = Some(id.clone());
                                    }
                                    ExprKind::Attribute { value, attr, .. } => {
                                        shape.field_type = Some(attr.clone());
                                        if let ExprKind::Name { id, .. } = &value.kind {
                                            shape.module_prefix = Some(id.clone());
                                        }
                                    }
                                    _ => {}
                                }
                            }
                            _ if is_none(value) => initialized_to_none = true,
                            // `self.x = param` is aggregation: skipped.
                            ExprKind::Name { .. } => continue,
                            _ => {}
                        }
                    }
                }
                StmtKind::AnnAssign {
                    target,
                    annotation,
                    value,
                } => {
                    if let Some(name) = self_attribute(target) {
                        field_name = Some(name.to_owned());
                        shape = annotation_shape(annotation);
                        if let Some(value) = value {
                            if is_none(value) {
                                initialized_to_none = true;
                            } else if matches!(value.kind, ExprKind::Call { .. }) {
                                is_instantiation = true;
                            }
                        }
                    }
                }
                _ => {}
            }
            let Some(field_name) = field_name else {
                continue;
            };
            if !seen.insert(field_name.clone()) {
                continue;
            }
            let field_full = format!("{parent_full}.{field_name}");
            let mut symbol = Symbol::new(
                &field_name,
                SymbolType::Field,
                Scope::Class,
                range_of(stmt.loc),
                file,
            );
            symbol.full_name = Some(field_full.clone());
            symbol.parent_symbol = Some(parent_full.clone());
            symbol.metadata.insert(
                "field_type".to_owned(),
                json!(shape.field_type.as_deref().unwrap_or("unknown")),
            );
            symbol
                .metadata
                .insert("is_instance_field".to_owned(), json!(true));
            symbol
                .metadata
                .insert("module_prefix".to_owned(), json!(shape.module_prefix));
            fields.push(symbol);
            let mut defines =
                Relationship::new(&parent_full, &field_full, RelationshipType::Defines, file);
            defines.target_file = Some(file.to_owned());
            defines.source_range = Some(range_of(stmt.loc));
            defines.weight = 0.9;
            defines
                .annotations
                .insert("member_type".to_owned(), json!("field"));
            defines
                .annotations
                .insert("container_type".to_owned(), json!("class"));
            relationships.push(defines);
            if let Some(field_type) = shape
                .field_type
                .as_deref()
                .filter(|t| !t.is_empty() && !FIELD_BUILTINS.contains(t))
            {
                let rel_type = if shape.is_optional || initialized_to_none {
                    RelationshipType::Aggregation
                } else {
                    RelationshipType::Composition
                };
                let mut composition = Relationship::new(&field_full, field_type, rel_type, file);
                composition.target_file = Some(file.to_owned());
                composition.source_range = Some(range_of(stmt.loc));
                composition.confidence = if is_instantiation { 0.9 } else { 0.7 };
                composition.weight = 0.8;
                composition
                    .annotations
                    .insert("field_name".to_owned(), json!(field_name));
                if initialized_to_none {
                    composition
                        .annotations
                        .insert("initialized_to_none".to_owned(), json!(true));
                }
                if let Some(prefix) = shape.module_prefix.as_deref().filter(|p| !p.is_empty()) {
                    composition
                        .annotations
                        .insert("module_prefix".to_owned(), json!(prefix));
                }
                relationships.push(composition);
            }
        }
    }
    // Class-level annotated variables (`database: Database`).
    for class_symbol in class_index.values() {
        // `_find_class_node`: the first class of that bare name.
        let Some(class_node) = classes
            .iter()
            .filter_map(|s| class_def(s))
            .find(|c| c.name == class_symbol.name)
        else {
            continue;
        };
        let class_full = class_symbol.full_name.clone().unwrap_or_default();
        for stmt in &class_node.body {
            let StmtKind::AnnAssign {
                target, annotation, ..
            } = &stmt.kind
            else {
                continue;
            };
            let ExprKind::Name { id: field_name, .. } = &target.kind else {
                continue;
            };
            let shape = annotation_shape(annotation);
            let field_full = format!("{class_full}.{field_name}");
            if fields
                .iter()
                .any(|f| f.full_name.as_deref() == Some(field_full.as_str()))
            {
                continue;
            }
            let mut symbol = Symbol::new(
                field_name,
                SymbolType::Field,
                Scope::Class,
                range_of(stmt.loc),
                file,
            );
            symbol.full_name = Some(field_full.clone());
            symbol.parent_symbol = Some(class_full.clone());
            symbol.metadata.insert(
                "field_type".to_owned(),
                json!(shape.field_type.as_deref().unwrap_or("unknown")),
            );
            symbol
                .metadata
                .insert("is_class_variable".to_owned(), json!(true));
            symbol
                .metadata
                .insert("module_prefix".to_owned(), json!(shape.module_prefix));
            fields.push(symbol);
            let mut defines =
                Relationship::new(&class_full, &field_full, RelationshipType::Defines, file);
            defines.target_file = Some(file.to_owned());
            defines.source_range = Some(range_of(stmt.loc));
            defines.weight = 0.9;
            defines
                .annotations
                .insert("member_type".to_owned(), json!("field"));
            defines
                .annotations
                .insert("container_type".to_owned(), json!("class"));
            defines
                .annotations
                .insert("class_variable".to_owned(), json!(true));
            relationships.push(defines);
            if let Some(field_type) = shape
                .field_type
                .as_deref()
                .filter(|t| !t.is_empty() && !FIELD_BUILTINS.contains(t))
            {
                let rel_type = if shape.is_optional {
                    RelationshipType::Aggregation
                } else {
                    RelationshipType::Composition
                };
                let mut composition = Relationship::new(&field_full, field_type, rel_type, file);
                composition.target_file = Some(file.to_owned());
                composition.source_range = Some(range_of(stmt.loc));
                composition.confidence = 0.7;
                composition.weight = 0.6;
                composition
                    .annotations
                    .insert("field_name".to_owned(), json!(field_name));
                composition
                    .annotations
                    .insert("class_variable".to_owned(), json!(true));
                if let Some(prefix) = shape.module_prefix.as_deref().filter(|p| !p.is_empty()) {
                    composition
                        .annotations
                        .insert("module_prefix".to_owned(), json!(prefix));
                }
                relationships.push(composition);
            }
        }
    }
    (fields, relationships)
}

// ----- module info --------------------------------------------------------

/// `_extract_module_info`: imports (breadth first over the whole tree, so
/// a nested import comes after every shallower one) and `__all__`.
pub fn module_info(module: &[Stmt]) -> (Vec<String>, Vec<String>) {
    let mut imports = Vec::new();
    let mut exports = Vec::new();
    walk_module(module, &mut |node| {
        let Node::Stmt(stmt) = node else {
            return;
        };
        match &stmt.kind {
            StmtKind::Import(names) => imports.extend(names.iter().map(|a| a.name.clone())),
            StmtKind::ImportFrom { module, names } => {
                let module = module.as_deref().unwrap_or("");
                for alias in names {
                    imports.push(format!("{module}.{}", alias.name));
                }
            }
            StmtKind::Assign { targets, value } => {
                if let [target] = targets.as_slice()
                    && matches!(&target.kind, ExprKind::Name { id, .. } if id == "__all__")
                    && let ExprKind::List { elts, .. } = &value.kind
                {
                    for elt in elts {
                        if let ExprKind::Constant {
                            value: Const::Str(s),
                            ..
                        } = &elt.kind
                        {
                            exports.push(s.clone());
                        }
                    }
                }
            }
            _ => {}
        }
    });
    (imports, exports)
}

/// `_extract_dependencies`: the first dotted part of every import and of
/// every dotted relationship target. A set in Python; sorted here.
pub fn dependencies(imports: &[String], relationships: &[Relationship]) -> Vec<String> {
    let mut set = BTreeSet::new();
    for import in imports {
        set.insert(import.split('.').next().unwrap_or("").to_owned());
    }
    for relationship in relationships {
        if let Some((first, _)) = relationship.target_symbol.split_once('.') {
            set.insert(first.to_owned());
        }
    }
    set.into_iter().collect()
}

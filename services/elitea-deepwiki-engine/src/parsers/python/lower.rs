//! tree-sitter-python 0.25 → the `ast` tree Python 3.12 builds.
//!
//! WHY positions are taken the way they are: every range the Python parser
//! records is an `ast` node's, so each node gets the span `ast` gives it.
//! Most are the matching tree-sitter node's span; the exceptions are named
//! where they are made:
//!
//! * a parenthesised expression has the INNER expression's span, but a node
//!   built around it (`(a).b`, `(f)()`) starts at the parenthesis;
//! * a compound statement (`def`, `class`, `if` …) ends where its last
//!   statement ends — tree-sitter's block also covers trailing comments;
//! * `def`/`class` start at the keyword (`async` included), not at a
//!   decorator;
//! * an `arg` spans name to annotation (`*args: int` starts at `args`);
//! * an unparenthesised tuple (`x[A, B]`, `return a, b`) spans its elements
//!   and a trailing comma.
//!
//! What Python rejects but tree-sitter accepts (Python 2 `print x`,
//! `except A, B:`, a non-default parameter after a default one, octal
//! `0777`) is reported as a [`SyntaxError`], as `ast.parse` would.

use super::ast::{
    Alias, Arg, Arguments, BinOp, BoolOp, ClassDef, CmpOp, Comprehension, Const, Ctx,
    ExceptHandler, Expr, ExprKind, FunctionDef, Keyword, Loc, MatchCase, Pattern, Stmt, StmtKind,
    TypeParam, UnaryOp, WithItem,
};
use super::text::{Prefix, decode_bytes, decode_str, int_literal};
use tree_sitter::Node;

/// What `ast.parse` raised: the message and, when it has one, the line.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct SyntaxError {
    pub message: String,
    pub line: Option<u32>,
    /// Not a syntax error: the tree is nested too deeply for Python's
    /// visitors (`RecursionError`), found before lowering recurses into it.
    pub too_deep: bool,
}

impl SyntaxError {
    fn at(message: impl Into<String>, node: Node<'_>) -> Self {
        Self {
            message: message.into(),
            line: Some(to_u32(node.start_position().row + 1)),
            too_deep: false,
        }
    }

    fn invalid(node: Node<'_>) -> Self {
        Self::at("invalid syntax", node)
    }
}

type Result<T> = std::result::Result<T, SyntaxError>;

/// Parse `source` (already decoded and newline-normalised) into the module
/// body.
pub fn parse_module(source: &str) -> Result<Vec<Stmt>> {
    if source.contains('\0') {
        return Err(SyntaxError {
            message: "source code string cannot contain null bytes".to_owned(),
            line: None,
            too_deep: false,
        });
    }
    let mut parser = tree_sitter::Parser::new();
    let language: tree_sitter::Language = tree_sitter_python::LANGUAGE.into();
    let Ok(()) = parser.set_language(&language) else {
        return Err(SyntaxError {
            message: "Tree-sitter Python parser not available".to_owned(),
            line: None,
            too_deep: false,
        });
    };
    let Some(tree) = parser.parse(source, None) else {
        return Err(SyntaxError {
            message: "Tree-sitter Python parser returned no tree".to_owned(),
            line: None,
            too_deep: false,
        });
    };
    let root = tree.root_node();
    // A tree deeper than `MAX_TREE_DEPTH` is far past what Python's
    // recursive visitors survive (about 500 `ast` levels, each one to three
    // tree-sitter levels), so it is rejected before the recursive lowering —
    // which keeps the stack bounded.
    if crate::parsers::limits::too_deep(root) {
        return Err(SyntaxError {
            message: crate::parsers::limits::RECURSION_ERROR.to_owned(),
            line: None,
            too_deep: true,
        });
    }
    let (nesting, never_closed) = scan_brackets(source);
    if let Some(error) = nesting {
        return Err(error);
    }
    if root.has_error() {
        if let Some(error) = never_closed {
            return Err(error);
        }
        let node = first_error(root).unwrap_or(root);
        return Err(SyntaxError::invalid(node));
    }
    if let Some(error) = non_printable(source) {
        return Err(error);
    }
    let lowerer = Lowerer {
        src: source,
        indent: std::cell::Cell::new(0),
    };
    lowerer.block(root)
}

/// The first `ERROR` or missing node in document order.
fn first_error(root: Node<'_>) -> Option<Node<'_>> {
    let mut cursor = root.walk();
    let mut stack = vec![root];
    while let Some(node) = stack.pop() {
        if node.is_error() || node.is_missing() {
            return Some(node);
        }
        if node.has_error() {
            let children: Vec<Node<'_>> = node.children(&mut cursor).collect();
            stack.extend(children.into_iter().rev());
        }
    }
    None
}

/// What Python's tokenizer checks about brackets: more than
/// [`MAX_BRACKET_LEVEL`] open at once is `too many nested parentheses`; one
/// still open at the end of the file is `'(' was never closed` at its line.
/// A light scan: brackets outside strings and comments.
fn scan_brackets(source: &str) -> (Option<SyntaxError>, Option<SyntaxError>) {
    let mut open: Vec<(char, u32)> = Vec::new();
    let mut nesting = None;
    let mut line = 1u32;
    let mut chars = source.chars().peekable();
    while let Some(c) = chars.next() {
        match c {
            '\n' => line += 1,
            '#' => {
                while chars.peek().is_some_and(|&n| n != '\n') {
                    chars.next();
                }
            }
            '\'' | '"' => {
                let triple = chars.peek() == Some(&c) && {
                    let mut look = chars.clone();
                    look.next();
                    look.peek() == Some(&c)
                };
                if triple {
                    chars.next();
                    chars.next();
                }
                let mut run = 0;
                while let Some(n) = chars.next() {
                    match n {
                        '\\' => {
                            if chars.next() == Some('\n') {
                                line += 1;
                            }
                            run = 0;
                        }
                        '\n' => {
                            line += 1;
                            if !triple {
                                break;
                            }
                            run = 0;
                        }
                        n if n == c => {
                            run += 1;
                            if !triple || run == 3 {
                                break;
                            }
                        }
                        _ => run = 0,
                    }
                }
            }
            '(' | '[' | '{' => {
                open.push((c, line));
                if open.len() > MAX_BRACKET_LEVEL && nesting.is_none() {
                    nesting = Some(SyntaxError {
                        message: "too many nested parentheses".to_owned(),
                        line: Some(line),
                        too_deep: false,
                    });
                }
            }
            ')' | ']' | '}' => {
                open.pop();
            }
            _ => {}
        }
    }
    let never_closed = open.last().map(|&(bracket, at)| SyntaxError {
        message: format!("'{bracket}' was never closed"),
        line: Some(at),
        too_deep: false,
    });
    (nesting, never_closed)
}

/// `MAXLEVEL` of Python's tokenizer.
const MAX_BRACKET_LEVEL: usize = 200;

/// `MAXINDENT` of Python's tokenizer: the 100th nested indented block is
/// `too many levels of indentation`.
const MAX_INDENT: usize = 100;

/// Python's tokenizer rejects a BOM or another invisible format character
/// outside strings and comments; a BOM at the start is the case that
/// occurs (`open(…, encoding='utf-8')` keeps it).
fn non_printable(source: &str) -> Option<SyntaxError> {
    source.starts_with('\u{feff}').then(|| SyntaxError {
        message: "invalid non-printable character U+FEFF".to_owned(),
        line: Some(1),
        too_deep: false,
    })
}

fn to_u32(value: usize) -> u32 {
    u32::try_from(value).unwrap_or(u32::MAX)
}

fn loc(node: Node<'_>) -> Loc {
    let (start, end) = (node.start_position(), node.end_position());
    Loc {
        line: to_u32(start.row + 1),
        col: to_u32(start.column),
        end_line: to_u32(end.row + 1),
        end_col: to_u32(end.column),
    }
}

/// Named children that are code (comments and line continuations are
/// tree-sitter "extras" and can appear anywhere).
fn named(node: Node<'_>) -> Vec<Node<'_>> {
    let mut cursor = node.walk();
    node.named_children(&mut cursor)
        .filter(|c| !is_extra(*c))
        .collect()
}

/// Comments and line continuations are tree-sitter "extras": named nodes
/// that can sit between any two tokens.
fn is_extra(node: Node<'_>) -> bool {
    matches!(node.kind(), "comment" | "line_continuation")
}

fn all_children(node: Node<'_>) -> Vec<Node<'_>> {
    let mut cursor = node.walk();
    node.children(&mut cursor).collect()
}

fn has_token(node: Node<'_>, token: &str) -> bool {
    all_children(node)
        .iter()
        .any(|c| !c.is_named() && c.kind() == token)
}

fn field<'t>(node: Node<'t>, name: &str) -> Option<Node<'t>> {
    node.child_by_field_name(name)
}

fn fields<'t>(node: Node<'t>, name: &str) -> Vec<Node<'t>> {
    let mut cursor = node.walk();
    node.children_by_field_name(name, &mut cursor)
        .filter(|c| !is_extra(*c))
        .collect()
}

fn expr(loc: Loc, kind: ExprKind) -> Expr {
    Expr { loc, kind }
}

fn boxed(e: Expr) -> Box<Expr> {
    Box::new(e)
}

/// The end of a statement list (`ast` ends a compound statement there).
fn end_of(body: &[Stmt], fallback: Loc) -> Loc {
    body.last().map_or(fallback, |s| s.loc)
}

struct Lowerer<'s> {
    src: &'s str,
    /// How many indented blocks enclose the current one.
    indent: std::cell::Cell<usize>,
}

impl<'s> Lowerer<'s> {
    fn text(&self, node: Node<'_>) -> &'s str {
        self.src.get(node.byte_range()).unwrap_or("")
    }

    // ----- statements -------------------------------------------------

    fn block(&self, node: Node<'_>) -> Result<Vec<Stmt>> {
        // A block on its own lines is one indentation level deeper; a
        // one-line suite (`if x: pass`) is not.
        let indented = node.kind() == "block"
            && node
                .parent()
                .is_some_and(|p| node.start_position().row > p.start_position().row);
        if indented {
            self.indent.set(self.indent.get() + 1);
            if self.indent.get() >= MAX_INDENT {
                return Err(SyntaxError::at("too many levels of indentation", node));
            }
        }
        let mut out = Vec::new();
        let mut outcome = Ok(());
        for child in named(node) {
            outcome = self.statement(child, &mut out);
            if outcome.is_err() {
                break;
            }
        }
        if indented {
            self.indent.set(self.indent.get() - 1);
        }
        outcome.map(|()| out)
    }

    /// The statements of a `_suite` field: a `block`, or the simple
    /// statements of a one-line body.
    fn suite(&self, node: Option<Node<'_>>, parent: Node<'_>) -> Result<Vec<Stmt>> {
        let node = node.ok_or_else(|| SyntaxError::invalid(parent))?;
        if node.kind() == "block" {
            self.block(node)
        } else {
            let mut out = Vec::new();
            self.statement(node, &mut out)?;
            Ok(out)
        }
    }

    #[allow(clippy::too_many_lines)]
    fn statement(&self, node: Node<'_>, out: &mut Vec<Stmt>) -> Result<()> {
        let at = loc(node);
        let kind = match node.kind() {
            "expression_statement" => self.expression_statement(node)?,
            "assignment" | "augmented_assignment" => self.assignment_like(node)?,
            "return_statement" => StmtKind::Return(
                named(node)
                    .first()
                    .map(|c| self.expression(*c))
                    .transpose()?,
            ),
            "pass_statement" => StmtKind::Pass,
            "break_statement" => StmtKind::Break,
            "continue_statement" => StmtKind::Continue,
            "delete_statement" => {
                let mut targets = Vec::new();
                for child in named(node) {
                    if child.kind() == "expression_list" {
                        for item in named(child) {
                            targets.push(self.target(item, Ctx::Del)?);
                        }
                    } else {
                        targets.push(self.target(child, Ctx::Del)?);
                    }
                }
                StmtKind::Delete(targets)
            }
            "raise_statement" => {
                let cause = field(node, "cause");
                let exc = named(node)
                    .into_iter()
                    .find(|c| Some(*c) != cause)
                    .map(|c| {
                        if c.kind() == "expression_list" {
                            Err(SyntaxError::invalid(c))
                        } else {
                            self.expression(c)
                        }
                    })
                    .transpose()?;
                StmtKind::Raise {
                    exc,
                    cause: cause.map(|c| self.expression(c)).transpose()?,
                }
            }
            "global_statement" => StmtKind::Global,
            "nonlocal_statement" => StmtKind::Nonlocal,
            "assert_statement" => {
                let parts = named(node);
                let test = parts.first().ok_or_else(|| SyntaxError::invalid(node))?;
                StmtKind::Assert {
                    test: self.expression(*test)?,
                    msg: parts.get(1).map(|m| self.expression(*m)).transpose()?,
                }
            }
            "import_statement" => StmtKind::Import(self.aliases(&fields(node, "name"))),
            "import_from_statement" => self.import_from(node),
            "future_import_statement" => StmtKind::ImportFrom {
                module: Some("__future__".to_owned()),
                names: self.aliases(&fields(node, "name")),
            },
            "print_statement" => {
                return Err(SyntaxError::at(
                    "Missing parentheses in call to 'print'. Did you mean print(...)?",
                    node,
                ));
            }
            "exec_statement" => {
                return Err(SyntaxError::at(
                    "Missing parentheses in call to 'exec'. Did you mean exec(...)?",
                    node,
                ));
            }
            "type_alias_statement" => self.type_alias(node)?,
            "if_statement" => {
                let stmt = self.if_statement(node)?;
                out.push(stmt);
                return Ok(());
            }
            "for_statement" => {
                let body = self.suite(field(node, "body"), node)?;
                let orelse = match field(node, "alternative") {
                    Some(alt) => self.suite(field(alt, "body"), alt)?,
                    None => Vec::new(),
                };
                let end = end_of(&orelse, end_of(&body, at));
                out.push(Stmt {
                    loc: at.to(end),
                    kind: StmtKind::For {
                        target: self.target(self.required(node, "left")?, Ctx::Store)?,
                        iter: self.expression(self.required(node, "right")?)?,
                        body,
                        orelse,
                    },
                });
                return Ok(());
            }
            "while_statement" => {
                let body = self.suite(field(node, "body"), node)?;
                let orelse = match field(node, "alternative") {
                    Some(alt) => self.suite(field(alt, "body"), alt)?,
                    None => Vec::new(),
                };
                let end = end_of(&orelse, end_of(&body, at));
                out.push(Stmt {
                    loc: at.to(end),
                    kind: StmtKind::While {
                        test: self.expression(self.required(node, "condition")?)?,
                        body,
                        orelse,
                    },
                });
                return Ok(());
            }
            "try_statement" => {
                out.push(self.try_statement(node)?);
                return Ok(());
            }
            "with_statement" => {
                out.push(self.with_statement(node)?);
                return Ok(());
            }
            "match_statement" => {
                out.push(self.match_statement(node)?);
                return Ok(());
            }
            "function_definition" => {
                out.push(self.function(node, Vec::new())?);
                return Ok(());
            }
            "class_definition" => {
                out.push(self.class(node, Vec::new())?);
                return Ok(());
            }
            "decorated_definition" => {
                let mut decorators = Vec::new();
                for child in named(node) {
                    if child.kind() == "decorator" {
                        let inner = named(child)
                            .into_iter()
                            .next()
                            .ok_or_else(|| SyntaxError::invalid(child))?;
                        decorators.push(self.expression(inner)?);
                    }
                }
                let definition = self.required(node, "definition")?;
                let stmt = if definition.kind() == "class_definition" {
                    self.class(definition, decorators)?
                } else {
                    self.function(definition, decorators)?
                };
                out.push(stmt);
                return Ok(());
            }
            _ => {
                // A bare expression where a statement is expected is how
                // some grammar versions spell an expression statement.
                let value = self.expression(node)?;
                StmtKind::Expr(value)
            }
        };
        out.push(Stmt { loc: at, kind });
        Ok(())
    }

    fn required<'t>(&self, node: Node<'t>, name: &str) -> Result<Node<'t>> {
        let _ = self;
        field(node, name).ok_or_else(|| SyntaxError::invalid(node))
    }

    fn expression_statement(&self, node: Node<'_>) -> Result<StmtKind> {
        let parts = named(node);
        match parts.as_slice() {
            [single] => match single.kind() {
                "assignment" | "augmented_assignment" => self.assignment_like(*single),
                _ => Ok(StmtKind::Expr(self.expression(*single)?)),
            },
            [] => Err(SyntaxError::invalid(node)),
            many => {
                let elts = many
                    .iter()
                    .map(|p| self.expression(*p))
                    .collect::<Result<Vec<_>>>()?;
                Ok(StmtKind::Expr(expr(
                    loc(node),
                    ExprKind::Tuple {
                        elts,
                        ctx: Ctx::Load,
                    },
                )))
            }
        }
    }

    /// `a = b = value`, `a: T = value`, `a += value`.
    fn assignment_like(&self, node: Node<'_>) -> Result<StmtKind> {
        if node.kind() == "augmented_assignment" {
            let operator = self.required(node, "operator")?;
            let op = BinOp::parse(self.text(operator)).ok_or_else(|| SyntaxError::invalid(node))?;
            return Ok(StmtKind::AugAssign {
                target: self.target(self.required(node, "left")?, Ctx::Store)?,
                op,
                value: self.expression(self.required(node, "right")?)?,
            });
        }
        let left = self.required(node, "left")?;
        if let Some(annotation) = field(node, "type") {
            if matches!(
                left.kind(),
                "pattern_list" | "tuple_pattern" | "list_pattern"
            ) {
                return Err(SyntaxError::at(
                    "only single target (not tuple) can be annotated",
                    left,
                ));
            }
            return Ok(StmtKind::AnnAssign {
                target: self.target(left, Ctx::Store)?,
                annotation: self.type_expr(annotation)?,
                value: field(node, "right")
                    .map(|r| self.expression(r))
                    .transpose()?,
            });
        }
        let mut targets = vec![self.target(left, Ctx::Store)?];
        let mut current = self.required(node, "right")?;
        while current.kind() == "assignment" && field(current, "type").is_none() {
            targets.push(self.target(self.required(current, "left")?, Ctx::Store)?);
            current = self.required(current, "right")?;
        }
        if matches!(current.kind(), "assignment" | "augmented_assignment") {
            return Err(SyntaxError::invalid(current));
        }
        Ok(StmtKind::Assign {
            targets,
            value: self.expression(current)?,
        })
    }

    fn dotted(&self, node: Node<'_>) -> String {
        named(node)
            .iter()
            .map(|c| self.text(*c))
            .collect::<Vec<_>>()
            .join(".")
    }

    fn aliases(&self, names: &[Node<'_>]) -> Vec<Alias> {
        names
            .iter()
            .map(|n| match n.kind() {
                "aliased_import" => Alias {
                    name: field(*n, "name")
                        .map(|d| self.dotted(d))
                        .unwrap_or_default(),
                    asname: field(*n, "alias").map(|a| self.text(a).to_owned()),
                },
                _ => Alias {
                    name: self.dotted(*n),
                    asname: None,
                },
            })
            .collect()
    }

    fn import_from(&self, node: Node<'_>) -> StmtKind {
        let module = field(node, "module_name").and_then(|m| {
            if m.kind() == "relative_import" {
                named(m)
                    .into_iter()
                    .find(|c| c.kind() == "dotted_name")
                    .map(|d| self.dotted(d))
            } else {
                Some(self.dotted(m))
            }
        });
        let names = if named(node).iter().any(|c| c.kind() == "wildcard_import") {
            vec![Alias {
                name: "*".to_owned(),
                asname: None,
            }]
        } else {
            self.aliases(&fields(node, "name"))
        };
        StmtKind::ImportFrom { module, names }
    }

    fn type_alias(&self, node: Node<'_>) -> Result<StmtKind> {
        let left = self.required(node, "left")?;
        let inner = named(left).into_iter().next().unwrap_or(left);
        let (name_node, type_params) = if inner.kind() == "generic_type" {
            let parts = named(inner);
            let name = parts.first().copied().unwrap_or(inner);
            let params = match parts.get(1) {
                Some(p) => self.type_params(*p)?,
                None => Vec::new(),
            };
            (name, params)
        } else {
            (inner, Vec::new())
        };
        Ok(StmtKind::TypeAlias {
            name: expr(
                loc(name_node),
                ExprKind::Name {
                    id: self.text(name_node).to_owned(),
                    ctx: Ctx::Store,
                },
            ),
            type_params,
            value: self.type_expr(self.required(node, "right")?)?,
        })
    }

    fn if_statement(&self, node: Node<'_>) -> Result<Stmt> {
        let at = loc(node);
        let test = self.expression(self.required(node, "condition")?)?;
        let body = self.suite(field(node, "consequence"), node)?;
        let alternatives = fields(node, "alternative");
        let mut orelse: Vec<Stmt> = Vec::new();
        for alt in alternatives.iter().rev() {
            if alt.kind() == "else_clause" {
                orelse = self.suite(field(*alt, "body"), *alt)?;
            } else {
                let elif_body = self.suite(field(*alt, "consequence"), *alt)?;
                let end = end_of(&orelse, end_of(&elif_body, loc(*alt)));
                let stmt = Stmt {
                    loc: loc(*alt).to(end),
                    kind: StmtKind::If {
                        test: self.expression(self.required(*alt, "condition")?)?,
                        body: elif_body,
                        orelse,
                    },
                };
                orelse = vec![stmt];
            }
        }
        let end = end_of(&orelse, end_of(&body, at));
        Ok(Stmt {
            loc: at.to(end),
            kind: StmtKind::If { test, body, orelse },
        })
    }

    fn try_statement(&self, node: Node<'_>) -> Result<Stmt> {
        let at = loc(node);
        let body = self.suite(field(node, "body"), node)?;
        let mut handlers = Vec::new();
        let mut orelse = Vec::new();
        let mut finalbody = Vec::new();
        let mut last_end = end_of(&body, at);
        for child in named(node) {
            match child.kind() {
                "except_clause" | "except_group_clause" => {
                    let values = fields(child, "value");
                    if values.len() > 1 {
                        return Err(SyntaxError::at(
                            "multiple exception types must be parenthesized",
                            child,
                        ));
                    }
                    let type_ = match values.first() {
                        Some(v) if v.kind() == "as_pattern" => {
                            let inner = named(*v)
                                .into_iter()
                                .next()
                                .ok_or_else(|| SyntaxError::invalid(*v))?;
                            Some(self.expression(inner)?)
                        }
                        Some(v) => Some(self.expression(*v)?),
                        None => None,
                    };
                    let block = named(child)
                        .into_iter()
                        .find(|c| c.kind() == "block")
                        .ok_or_else(|| SyntaxError::invalid(child))?;
                    let handler_body = self.block(block)?;
                    last_end = end_of(&handler_body, last_end);
                    handlers.push(ExceptHandler {
                        type_,
                        body: handler_body,
                    });
                }
                "else_clause" => {
                    orelse = self.suite(field(child, "body"), child)?;
                    last_end = end_of(&orelse, last_end);
                }
                "finally_clause" => {
                    let block = named(child)
                        .into_iter()
                        .find(|c| c.kind() == "block")
                        .ok_or_else(|| SyntaxError::invalid(child))?;
                    finalbody = self.block(block)?;
                    last_end = end_of(&finalbody, last_end);
                }
                _ => {}
            }
        }
        Ok(Stmt {
            loc: at.to(last_end),
            kind: StmtKind::Try {
                body,
                handlers,
                orelse,
                finalbody,
            },
        })
    }

    fn with_statement(&self, node: Node<'_>) -> Result<Stmt> {
        let at = loc(node);
        let mut items = Vec::new();
        for clause in named(node)
            .into_iter()
            .filter(|c| c.kind() == "with_clause")
        {
            for item in named(clause) {
                let mut value = self.required(item, "value")?;
                // `with (open(a) as f):` parses as a parenthesised item.
                if value.kind() == "parenthesized_expression"
                    && let [inner] = named(value).as_slice()
                    && inner.kind() == "as_pattern"
                {
                    value = *inner;
                }
                if value.kind() == "as_pattern" {
                    let parts = named(value);
                    let context = parts.first().ok_or_else(|| SyntaxError::invalid(value))?;
                    let alias = field(value, "alias").ok_or_else(|| SyntaxError::invalid(value))?;
                    items.push(WithItem {
                        context_expr: self.expression(*context)?,
                        optional_vars: Some(self.as_target(alias)?),
                    });
                } else {
                    items.push(WithItem {
                        context_expr: self.expression(value)?,
                        optional_vars: None,
                    });
                }
            }
        }
        let body = self.suite(field(node, "body"), node)?;
        Ok(Stmt {
            loc: at.to(end_of(&body, at)),
            kind: StmtKind::With { items, body },
        })
    }

    /// The `as_pattern_target` of a `with` item: the wrapped target.
    fn as_target(&self, alias: Node<'_>) -> Result<Expr> {
        match named(alias).as_slice() {
            [single] => self.target(*single, Ctx::Store),
            _ => self.target(alias, Ctx::Store),
        }
    }

    fn match_statement(&self, node: Node<'_>) -> Result<Stmt> {
        let at = loc(node);
        let subjects = fields(node, "subject");
        let subject = match subjects.as_slice() {
            [single] => self.expression(*single)?,
            [first, .., last] => {
                let elts = subjects
                    .iter()
                    .map(|s| self.expression(*s))
                    .collect::<Result<Vec<_>>>()?;
                expr(
                    loc(*first).to(loc(*last)),
                    ExprKind::Tuple {
                        elts,
                        ctx: Ctx::Load,
                    },
                )
            }
            [] => return Err(SyntaxError::invalid(node)),
        };
        let body = self.required(node, "body")?;
        let mut cases = Vec::new();
        let mut last_end = at;
        for case in named(body)
            .into_iter()
            .filter(|c| c.kind() == "case_clause")
        {
            let patterns: Vec<Node<'_>> = named(case)
                .into_iter()
                .filter(|c| c.kind() == "case_pattern")
                .collect();
            let pattern = match patterns.as_slice() {
                [single] if !has_token(case, ",") => self.pattern(*single)?,
                many => Pattern::Sequence(
                    many.iter()
                        .map(|p| self.pattern(*p))
                        .collect::<Result<Vec<_>>>()?,
                ),
            };
            let guard = match field(case, "guard") {
                Some(g) => {
                    let inner = named(g)
                        .into_iter()
                        .next()
                        .ok_or_else(|| SyntaxError::invalid(g))?;
                    Some(self.expression(inner)?)
                }
                None => None,
            };
            let case_body = self.suite(field(case, "consequence"), case)?;
            last_end = end_of(&case_body, last_end);
            cases.push(MatchCase {
                pattern,
                guard,
                body: case_body,
            });
        }
        Ok(Stmt {
            loc: at.to(last_end),
            kind: StmtKind::Match { subject, cases },
        })
    }

    fn pattern(&self, node: Node<'_>) -> Result<Pattern> {
        match node.kind() {
            "case_pattern" => {
                let parts = named(node);
                match parts.as_slice() {
                    [single] => self.pattern(*single),
                    // `_` is an anonymous token: the wildcard.
                    [] => Ok(Pattern::As(None)),
                    [first, ..] if has_token(node, "-") => {
                        Ok(Pattern::Value(self.signed_number(node, *first)?))
                    }
                    _ => Err(SyntaxError::invalid(node)),
                }
            }
            "as_pattern" => {
                let inner = named(node).into_iter().find(|c| c.kind() == "case_pattern");
                Ok(Pattern::As(
                    inner.map(|i| self.pattern(i)).transpose()?.map(Box::new),
                ))
            }
            "keyword_pattern" => {
                let parts = named(node);
                match parts.get(1) {
                    Some(value) => self.pattern(*value),
                    None => Ok(Pattern::As(None)),
                }
            }
            "class_pattern" => {
                let parts = named(node);
                let cls = parts.first().ok_or_else(|| SyntaxError::invalid(node))?;
                let mut patterns = Vec::new();
                let mut kwd_patterns = Vec::new();
                for part in &parts[1..] {
                    let inner = named(*part).into_iter().next();
                    if inner.is_some_and(|i| i.kind() == "keyword_pattern") {
                        kwd_patterns.push(self.pattern(*part)?);
                    } else {
                        patterns.push(self.pattern(*part)?);
                    }
                }
                Ok(Pattern::Class {
                    cls: self.dotted_expr(*cls),
                    patterns,
                    kwd_patterns,
                })
            }
            "splat_pattern" => Ok(Pattern::Star),
            "union_pattern" => Ok(Pattern::Or(
                named(node)
                    .iter()
                    .map(|p| self.pattern(*p))
                    .collect::<Result<Vec<_>>>()?,
            )),
            "list_pattern" | "tuple_pattern" => Ok(Pattern::Sequence(
                named(node)
                    .iter()
                    .map(|p| self.pattern(*p))
                    .collect::<Result<Vec<_>>>()?,
            )),
            "dict_pattern" => {
                let keys = fields(node, "key")
                    .into_iter()
                    .map(|k| self.pattern_value(k))
                    .collect::<Result<Vec<_>>>()?;
                let patterns = fields(node, "value")
                    .into_iter()
                    .map(|v| self.pattern(v))
                    .collect::<Result<Vec<_>>>()?;
                Ok(Pattern::Mapping { keys, patterns })
            }
            "dotted_name" => {
                if named(node).len() == 1 {
                    Ok(Pattern::As(None))
                } else {
                    Ok(Pattern::Value(self.dotted_expr(node)))
                }
            }
            "true" | "false" | "none" => Ok(Pattern::Singleton),
            _ => Ok(Pattern::Value(self.pattern_value(node)?)),
        }
    }

    /// A literal or dotted value inside a pattern.
    fn pattern_value(&self, node: Node<'_>) -> Result<Expr> {
        match node.kind() {
            "dotted_name" => Ok(self.dotted_expr(node)),
            "complex_pattern" => {
                let parts = named(node);
                let operands = parts
                    .iter()
                    .map(|p| self.expression(*p))
                    .collect::<Result<Vec<_>>>()?;
                let mut iter = operands.into_iter();
                let (Some(left), Some(right)) = (iter.next(), iter.next()) else {
                    return Err(SyntaxError::invalid(node));
                };
                let op = if self.text(node).trim_start_matches('-').contains('+') {
                    BinOp::Add
                } else {
                    BinOp::Sub
                };
                Ok(expr(
                    loc(node),
                    ExprKind::BinOp {
                        left: boxed(left),
                        op,
                        right: boxed(right),
                    },
                ))
            }
            "case_pattern" => match named(node).as_slice() {
                [single] => self.pattern_value(*single),
                [first, ..] => self.signed_number(node, *first),
                [] => Err(SyntaxError::invalid(node)),
            },
            _ => self.expression(node),
        }
    }

    fn signed_number(&self, whole: Node<'_>, number: Node<'_>) -> Result<Expr> {
        let operand = self.expression(number)?;
        Ok(expr(
            loc(whole),
            ExprKind::UnaryOp {
                op: UnaryOp::USub,
                operand: boxed(operand),
            },
        ))
    }

    /// `a.b.c` in a pattern: a Name/Attribute chain of loads.
    fn dotted_expr(&self, node: Node<'_>) -> Expr {
        let parts = named(node);
        let mut iter = parts.iter();
        let Some(first) = iter.next() else {
            return expr(
                loc(node),
                ExprKind::Name {
                    id: String::new(),
                    ctx: Ctx::Load,
                },
            );
        };
        let start = loc(*first);
        let mut current = expr(
            start,
            ExprKind::Name {
                id: self.text(*first).to_owned(),
                ctx: Ctx::Load,
            },
        );
        for part in iter {
            current = expr(
                start.to(loc(*part)),
                ExprKind::Attribute {
                    value: boxed(current),
                    attr: self.text(*part).to_owned(),
                    ctx: Ctx::Load,
                },
            );
        }
        current
    }

    fn function(&self, node: Node<'_>, decorators: Vec<Expr>) -> Result<Stmt> {
        let at = loc(node);
        let name = self.text(self.required(node, "name")?).to_owned();
        let is_async = all_children(node)
            .first()
            .is_some_and(|c| c.kind() == "async");
        let args = self.parameters(self.required(node, "parameters")?)?;
        let returns = field(node, "return_type")
            .map(|r| self.type_expr(r))
            .transpose()?;
        let type_params = match field(node, "type_parameters") {
            Some(p) => self.type_params(p)?,
            None => Vec::new(),
        };
        let body = self.suite(field(node, "body"), node)?;
        Ok(Stmt {
            loc: at.to(end_of(&body, at)),
            kind: StmtKind::FunctionDef(Box::new(FunctionDef {
                name,
                is_async,
                args,
                body,
                decorators,
                returns,
                type_params,
            })),
        })
    }

    fn class(&self, node: Node<'_>, decorators: Vec<Expr>) -> Result<Stmt> {
        let at = loc(node);
        let name = self.text(self.required(node, "name")?).to_owned();
        let (bases, keywords) = match field(node, "superclasses") {
            Some(list) => self.call_arguments(list)?,
            None => (Vec::new(), Vec::new()),
        };
        let type_params = match field(node, "type_parameters") {
            Some(p) => self.type_params(p)?,
            None => Vec::new(),
        };
        let body = self.suite(field(node, "body"), node)?;
        Ok(Stmt {
            loc: at.to(end_of(&body, at)),
            kind: StmtKind::ClassDef(Box::new(ClassDef {
                name,
                bases,
                keywords,
                body,
                decorators,
                type_params,
            })),
        })
    }

    /// PEP 695 `[T: bound, *Ts, **P]`.
    fn type_params(&self, node: Node<'_>) -> Result<Vec<TypeParam>> {
        let mut params = Vec::new();
        for item in named(node) {
            let inner = if item.kind() == "type" {
                named(item).into_iter().next().unwrap_or(item)
            } else {
                item
            };
            let bound = if inner.kind() == "constrained_type" {
                let parts = named(inner);
                match parts.get(1) {
                    Some(b) => Some(self.type_expr(*b)?),
                    None => None,
                }
            } else {
                None
            };
            params.push(TypeParam { bound });
        }
        Ok(params)
    }

    // ----- parameters -------------------------------------------------

    /// An `arg`: from the name to the end of the annotation's SYNTAX — a
    /// parenthesised annotation's `arg` ends at the `)` although the
    /// annotation expression itself does not.
    fn arg(&self, name: Node<'_>, annotation: Option<Node<'_>>) -> Result<Arg> {
        let start = loc(name);
        let span = annotation.map_or(start, |a| start.to(loc(a)));
        let annotation = annotation.map(|a| self.type_expr(a)).transpose()?;
        Ok(Arg {
            loc: span,
            name: self.text(name).to_owned(),
            annotation,
        })
    }

    /// The identifier inside a `*args` / `**kwargs` pattern.
    fn splat_name(node: Node<'_>) -> Result<Node<'_>> {
        named(node)
            .into_iter()
            .find(|c| c.kind() == "identifier")
            .ok_or_else(|| SyntaxError::invalid(node))
    }

    fn parameters(&self, node: Node<'_>) -> Result<Arguments> {
        let mut args = Arguments::default();
        let mut keyword_only = false;
        for child in named(node) {
            let (arg, default) = match child.kind() {
                "identifier" => (self.arg(child, None)?, None),
                "default_parameter" => (
                    self.arg(self.required(child, "name")?, None)?,
                    Some(self.required(child, "value")?),
                ),
                "typed_default_parameter" => (
                    self.arg(self.required(child, "name")?, field(child, "type"))?,
                    Some(self.required(child, "value")?),
                ),
                "typed_parameter" => {
                    let annotation = field(child, "type");
                    let inner = named(child)
                        .into_iter()
                        .find(|c| Some(*c) != annotation)
                        .ok_or_else(|| SyntaxError::invalid(child))?;
                    match inner.kind() {
                        "list_splat_pattern" => {
                            args.vararg = Some(self.arg(Self::splat_name(inner)?, annotation)?);
                            keyword_only = true;
                            continue;
                        }
                        "dictionary_splat_pattern" => {
                            args.kwarg = Some(self.arg(Self::splat_name(inner)?, annotation)?);
                            continue;
                        }
                        _ => (self.arg(inner, annotation)?, None),
                    }
                }
                "list_splat_pattern" => {
                    args.vararg = Some(self.arg(Self::splat_name(child)?, None)?);
                    keyword_only = true;
                    continue;
                }
                "dictionary_splat_pattern" => {
                    args.kwarg = Some(self.arg(Self::splat_name(child)?, None)?);
                    continue;
                }
                "keyword_separator" => {
                    keyword_only = true;
                    continue;
                }
                "positional_separator" => {
                    args.posonlyargs.append(&mut args.args);
                    continue;
                }
                _ => return Err(SyntaxError::invalid(child)),
            };
            let default = default.map(|d| self.expression(d)).transpose()?;
            if keyword_only {
                args.kwonlyargs.push(arg);
                args.kw_defaults.push(default);
            } else {
                match default {
                    Some(d) => args.defaults.push(d),
                    None if !args.defaults.is_empty() => {
                        return Err(SyntaxError::at(
                            "parameter without a default follows parameter with a default",
                            child,
                        ));
                    }
                    None => {}
                }
                args.args.push(arg);
            }
        }
        Ok(args)
    }

    // ----- expressions ------------------------------------------------

    /// An assignment / `for` / `with` / comprehension target.
    fn target(&self, node: Node<'_>, ctx: Ctx) -> Result<Expr> {
        let at = loc(node);
        Ok(match node.kind() {
            "identifier" => expr(
                at,
                ExprKind::Name {
                    id: self.text(node).to_owned(),
                    ctx,
                },
            ),
            "attribute" => {
                let (value, attr) = self.attribute_parts(node)?;
                expr(
                    at,
                    ExprKind::Attribute {
                        value: boxed(value),
                        attr,
                        ctx,
                    },
                )
            }
            "subscript" => {
                let (value, slice) = self.subscript_parts(node)?;
                expr(
                    at,
                    ExprKind::Subscript {
                        value: boxed(value),
                        slice: boxed(slice),
                        ctx,
                    },
                )
            }
            "pattern_list" | "expression_list" | "tuple_pattern" | "tuple" => expr(
                at,
                ExprKind::Tuple {
                    elts: self.targets(node, ctx)?,
                    ctx,
                },
            ),
            "list_pattern" | "list" => expr(
                at,
                ExprKind::List {
                    elts: self.targets(node, ctx)?,
                    ctx,
                },
            ),
            "list_splat_pattern" | "list_splat" => {
                let inner = named(node)
                    .into_iter()
                    .next()
                    .ok_or_else(|| SyntaxError::invalid(node))?;
                expr(
                    at,
                    ExprKind::Starred {
                        value: boxed(self.target(inner, ctx)?),
                        ctx,
                    },
                )
            }
            "parenthesized_expression" => {
                let inner = named(node)
                    .into_iter()
                    .next()
                    .ok_or_else(|| SyntaxError::invalid(node))?;
                self.target(inner, ctx)?
            }
            "as_pattern_target" => self.as_target(node)?,
            _ => self.expression(node)?,
        })
    }

    fn targets(&self, node: Node<'_>, ctx: Ctx) -> Result<Vec<Expr>> {
        named(node)
            .into_iter()
            .map(|c| self.target(c, ctx))
            .collect()
    }

    fn attribute_parts(&self, node: Node<'_>) -> Result<(Expr, String)> {
        let object = self.required(node, "object")?;
        let attr = self.required(node, "attribute")?;
        Ok((self.expression(object)?, self.text(attr).to_owned()))
    }

    /// `value[...]`: one subscript, or a tuple of them spanning the
    /// elements and a trailing comma (`ast` excludes the brackets).
    fn subscript_parts(&self, node: Node<'_>) -> Result<(Expr, Expr)> {
        let value = self.expression(self.required(node, "value")?)?;
        let items = fields(node, "subscript");
        let slice = self.slice_items(node, &items)?;
        Ok((value, slice))
    }

    fn slice_items(&self, node: Node<'_>, items: &[Node<'_>]) -> Result<Expr> {
        let (Some(first), Some(last)) = (items.first(), items.last()) else {
            return Err(SyntaxError::invalid(node));
        };
        let trailing = all_children(node)
            .iter()
            .find(|c| !c.is_named() && c.kind() == "," && c.start_byte() >= last.end_byte())
            .copied();
        if items.len() == 1 && trailing.is_none() {
            return self.slice_item(*first);
        }
        let elts = items
            .iter()
            .map(|i| self.slice_item(*i))
            .collect::<Result<Vec<_>>>()?;
        let end = trailing.map_or(loc(*last), loc);
        Ok(expr(
            loc(*first).to(end),
            ExprKind::Tuple {
                elts,
                ctx: Ctx::Load,
            },
        ))
    }

    fn slice_item(&self, node: Node<'_>) -> Result<Expr> {
        match node.kind() {
            "slice" => {
                let mut parts: [Option<Box<Expr>>; 3] = [None, None, None];
                let mut index = 0;
                for child in all_children(node) {
                    if !child.is_named() {
                        if child.kind() == ":" {
                            index += 1;
                        }
                        continue;
                    }
                    if is_extra(child) {
                        continue;
                    }
                    if let Some(slot) = parts.get_mut(index) {
                        *slot = Some(boxed(self.expression(child)?));
                    }
                }
                let [lower, upper, step] = parts;
                Ok(expr(loc(node), ExprKind::Slice { lower, upper, step }))
            }
            "type" => self.type_expr(node),
            _ => self.expression(node),
        }
    }

    /// A `type` node (annotation): an expression or one of the grammar's
    /// type-only shapes, rebuilt as the expression `ast` holds.
    fn type_expr(&self, node: Node<'_>) -> Result<Expr> {
        let inner = if node.kind() == "type" {
            named(node)
                .into_iter()
                .next()
                .ok_or_else(|| SyntaxError::invalid(node))?
        } else {
            node
        };
        let at = loc(inner);
        match inner.kind() {
            "splat_type" => {
                let name = Self::splat_name(inner)?;
                let value = expr(
                    loc(name),
                    ExprKind::Name {
                        id: self.text(name).to_owned(),
                        ctx: Ctx::Load,
                    },
                );
                Ok(expr(
                    at,
                    ExprKind::Starred {
                        value: boxed(value),
                        ctx: Ctx::Load,
                    },
                ))
            }
            "generic_type" => {
                let parts = named(inner);
                let (Some(name), Some(params)) = (parts.first(), parts.get(1)) else {
                    return Err(SyntaxError::invalid(inner));
                };
                let value = expr(
                    loc(*name),
                    ExprKind::Name {
                        id: self.text(*name).to_owned(),
                        ctx: Ctx::Load,
                    },
                );
                let items = named(*params);
                let slice = self.slice_items(*params, &items)?;
                Ok(expr(
                    at,
                    ExprKind::Subscript {
                        value: boxed(value),
                        slice: boxed(slice),
                        ctx: Ctx::Load,
                    },
                ))
            }
            "union_type" => {
                // `A | B | C` is `(A | B) | C` in `ast`, whichever way the
                // grammar nested it.
                let mut operands = Vec::new();
                self.union_operands(inner, &mut operands);
                let mut operands = operands.into_iter();
                let first = operands.next().ok_or_else(|| SyntaxError::invalid(inner))?;
                let mut current = self.type_expr(first)?;
                for operand in operands {
                    let right = self.type_expr(operand)?;
                    current = expr(
                        current.loc.to(right.loc),
                        ExprKind::BinOp {
                            left: boxed(current),
                            op: BinOp::BitOr,
                            right: boxed(right),
                        },
                    );
                }
                Ok(current)
            }
            "member_type" => {
                let parts = named(inner);
                let (Some(value), Some(attr)) = (parts.first(), parts.get(1)) else {
                    return Err(SyntaxError::invalid(inner));
                };
                Ok(expr(
                    at,
                    ExprKind::Attribute {
                        value: boxed(self.type_expr(*value)?),
                        attr: self.text(*attr).to_owned(),
                        ctx: Ctx::Load,
                    },
                ))
            }
            "constrained_type" => Err(SyntaxError::invalid(inner)),
            _ => self.expression(inner),
        }
    }

    /// The operands of a `|` chain in a type, left to right. The grammar
    /// may nest it either way and mix `union_type` with an expression
    /// `binary_operator` (`list[str] | str | None`); `ast` folds the whole
    /// chain to the left. A parenthesised operand stays whole.
    fn union_operands<'t>(&self, node: Node<'t>, out: &mut Vec<Node<'t>>) {
        let node = if node.kind() == "type" {
            match named(node).as_slice() {
                [inner] => *inner,
                _ => node,
            }
        } else {
            node
        };
        match node.kind() {
            "union_type" => {
                for part in named(node) {
                    self.union_operands(part, out);
                }
            }
            "binary_operator" if field(node, "operator").is_some_and(|o| self.text(o) == "|") => {
                for side in ["left", "right"] {
                    if let Some(part) = field(node, side) {
                        self.union_operands(part, out);
                    }
                }
            }
            _ => out.push(node),
        }
    }

    fn call_arguments(&self, list: Node<'_>) -> Result<(Vec<Expr>, Vec<Keyword>)> {
        let mut args = Vec::new();
        let mut keywords = Vec::new();
        for child in named(list) {
            match child.kind() {
                "keyword_argument" => keywords.push(Keyword {
                    arg: Some(self.text(self.required(child, "name")?).to_owned()),
                    value: self.expression(self.required(child, "value")?)?,
                }),
                "dictionary_splat" => {
                    let inner = named(child)
                        .into_iter()
                        .next()
                        .ok_or_else(|| SyntaxError::invalid(child))?;
                    keywords.push(Keyword {
                        arg: None,
                        value: self.expression(inner)?,
                    });
                }
                _ => args.push(self.expression(child)?),
            }
        }
        Ok((args, keywords))
    }

    fn elements(&self, node: Node<'_>) -> Result<Vec<Expr>> {
        named(node)
            .into_iter()
            .map(|c| self.expression(c))
            .collect()
    }

    #[allow(clippy::too_many_lines)]
    fn expression(&self, node: Node<'_>) -> Result<Expr> {
        let at = loc(node);
        let kind = match node.kind() {
            "identifier" | "keyword_identifier" => ExprKind::Name {
                id: self.text(node).to_owned(),
                ctx: Ctx::Load,
            },
            "attribute" => {
                let (value, attr) = self.attribute_parts(node)?;
                ExprKind::Attribute {
                    value: boxed(value),
                    attr,
                    ctx: Ctx::Load,
                }
            }
            "subscript" => {
                let (value, slice) = self.subscript_parts(node)?;
                ExprKind::Subscript {
                    value: boxed(value),
                    slice: boxed(slice),
                    ctx: Ctx::Load,
                }
            }
            "call" => {
                let func = self.expression(self.required(node, "function")?)?;
                let arguments = self.required(node, "arguments")?;
                let (args, keywords) = if arguments.kind() == "generator_expression" {
                    (vec![self.expression(arguments)?], Vec::new())
                } else {
                    self.call_arguments(arguments)?
                };
                ExprKind::Call {
                    func: boxed(func),
                    args,
                    keywords,
                }
            }
            "parenthesized_expression" => {
                let inner = named(node)
                    .into_iter()
                    .next()
                    .ok_or_else(|| SyntaxError::invalid(node))?;
                return self.expression(inner);
            }
            "binary_operator" => {
                let operator = self.required(node, "operator")?;
                ExprKind::BinOp {
                    left: boxed(self.expression(self.required(node, "left")?)?),
                    op: BinOp::parse(self.text(operator))
                        .ok_or_else(|| SyntaxError::invalid(node))?,
                    right: boxed(self.expression(self.required(node, "right")?)?),
                }
            }
            "unary_operator" => {
                let operator = self.required(node, "operator")?;
                let op = match self.text(operator) {
                    "-" => UnaryOp::USub,
                    "+" => UnaryOp::UAdd,
                    _ => UnaryOp::Invert,
                };
                ExprKind::UnaryOp {
                    op,
                    operand: boxed(self.expression(self.required(node, "argument")?)?),
                }
            }
            "not_operator" => ExprKind::UnaryOp {
                op: UnaryOp::Not,
                operand: boxed(self.expression(self.required(node, "argument")?)?),
            },
            "boolean_operator" => {
                let op = if self.text(self.required(node, "operator")?) == "and" {
                    BoolOp::And
                } else {
                    BoolOp::Or
                };
                let left = self.required(node, "left")?;
                let mut values = Vec::new();
                // `a and b and c` is one BoolOp; tree-sitter nests it to the
                // left. A parenthesised operand stays its own node.
                let left_expr = self.expression(left)?;
                match left_expr.kind {
                    ExprKind::BoolOp {
                        op: inner,
                        values: inner_values,
                    } if inner == op && left.kind() == "boolean_operator" => {
                        values.extend(inner_values);
                    }
                    _ => values.push(left_expr),
                }
                values.push(self.expression(self.required(node, "right")?)?);
                ExprKind::BoolOp { op, values }
            }
            "comparison_operator" => self.comparison(node)?,
            "conditional_expression" => {
                let parts = named(node);
                let [body, test, orelse] = parts.as_slice() else {
                    return Err(SyntaxError::invalid(node));
                };
                ExprKind::IfExp {
                    test: boxed(self.expression(*test)?),
                    body: boxed(self.expression(*body)?),
                    orelse: boxed(self.expression(*orelse)?),
                }
            }
            "lambda" => ExprKind::Lambda {
                args: Box::new(match field(node, "parameters") {
                    Some(p) => self.parameters(p)?,
                    None => Arguments::default(),
                }),
                body: boxed(self.expression(self.required(node, "body")?)?),
            },
            "named_expression" => {
                let name = self.required(node, "name")?;
                ExprKind::NamedExpr {
                    target: boxed(expr(
                        loc(name),
                        ExprKind::Name {
                            id: self.text(name).to_owned(),
                            ctx: Ctx::Store,
                        },
                    )),
                    value: boxed(self.expression(self.required(node, "value")?)?),
                }
            }
            "await" => {
                let inner = named(node)
                    .into_iter()
                    .next()
                    .ok_or_else(|| SyntaxError::invalid(node))?;
                ExprKind::Await(boxed(self.expression(inner)?))
            }
            "yield" => {
                let inner = named(node).into_iter().next();
                if has_token(node, "from") {
                    let inner = inner.ok_or_else(|| SyntaxError::invalid(node))?;
                    ExprKind::YieldFrom(boxed(self.expression(inner)?))
                } else {
                    ExprKind::Yield(inner.map(|i| self.expression(i)).transpose()?.map(boxed))
                }
            }
            "list" => ExprKind::List {
                elts: self.elements(node)?,
                ctx: Ctx::Load,
            },
            "tuple" | "expression_list" => ExprKind::Tuple {
                elts: self.elements(node)?,
                ctx: Ctx::Load,
            },
            "set" => ExprKind::Set(self.elements(node)?),
            "dictionary" => {
                let mut keys = Vec::new();
                let mut values = Vec::new();
                for child in named(node) {
                    if child.kind() == "pair" {
                        keys.push(Some(self.expression(self.required(child, "key")?)?));
                        values.push(self.expression(self.required(child, "value")?)?);
                    } else {
                        let inner = named(child)
                            .into_iter()
                            .next()
                            .ok_or_else(|| SyntaxError::invalid(child))?;
                        keys.push(None);
                        values.push(self.expression(inner)?);
                    }
                }
                ExprKind::Dict { keys, values }
            }
            "list_comprehension" | "set_comprehension" | "generator_expression" => {
                let elt = boxed(self.expression(self.required(node, "body")?)?);
                let generators = self.comprehensions(node)?;
                match node.kind() {
                    "list_comprehension" => ExprKind::ListComp { elt, generators },
                    "set_comprehension" => ExprKind::SetComp { elt, generators },
                    _ => ExprKind::GeneratorExp { elt, generators },
                }
            }
            "dictionary_comprehension" => {
                let pair = self.required(node, "body")?;
                ExprKind::DictComp {
                    key: boxed(self.expression(self.required(pair, "key")?)?),
                    value: boxed(self.expression(self.required(pair, "value")?)?),
                    generators: self.comprehensions(node)?,
                }
            }
            "string" | "concatenated_string" => return self.string(node),
            "integer" => {
                let text = self.text(node);
                if let Some(imag) = text.strip_suffix(['j', 'J']) {
                    ExprKind::Constant {
                        value: Const::Complex(parse_float(imag)),
                        u_prefix: false,
                    }
                } else {
                    let digits: String = text.chars().filter(|&c| c != '_').collect();
                    if digits.len() > 1
                        && digits.starts_with('0')
                        && digits.chars().all(|c| c.is_ascii_digit())
                        && digits.chars().any(|c| c != '0')
                    {
                        return Err(SyntaxError::at(
                            "leading zeros in decimal integer literals are not permitted; use an 0o prefix for octal integers",
                            node,
                        ));
                    }
                    ExprKind::Constant {
                        value: Const::Int(int_literal(text)),
                        u_prefix: false,
                    }
                }
            }
            "float" => {
                let text = self.text(node);
                let value = match text.strip_suffix(['j', 'J']) {
                    Some(imag) => Const::Complex(parse_float(imag)),
                    None => Const::Float(parse_float(text)),
                };
                ExprKind::Constant {
                    value,
                    u_prefix: false,
                }
            }
            "true" => constant(Const::Bool(true)),
            "false" => constant(Const::Bool(false)),
            "none" => constant(Const::None),
            "ellipsis" => constant(Const::Ellipsis),
            "list_splat" | "parenthesized_list_splat" => {
                let inner = named(node)
                    .into_iter()
                    .next()
                    .ok_or_else(|| SyntaxError::invalid(node))?;
                if node.kind() == "parenthesized_list_splat" {
                    return self.expression(inner);
                }
                ExprKind::Starred {
                    value: boxed(self.expression(inner)?),
                    ctx: Ctx::Load,
                }
            }
            "type" => return self.type_expr(node),
            _ => return Err(SyntaxError::invalid(node)),
        };
        Ok(expr(at, kind))
    }

    /// `a < b not in c`: operands are the named children; the operator
    /// between two operands is the words of the anonymous tokens there
    /// (`not in` / `is not` may be one token or two).
    fn comparison(&self, node: Node<'_>) -> Result<ExprKind> {
        let mut operands = Vec::new();
        let mut ops = Vec::new();
        let mut words: Vec<&str> = Vec::new();
        for child in all_children(node) {
            if child.is_named() {
                if is_extra(child) {
                    continue;
                }
                if !words.is_empty() {
                    ops.push(cmp_op(&words.join(" ")).ok_or_else(|| SyntaxError::invalid(child))?);
                    words.clear();
                }
                operands.push(self.expression(child)?);
            } else {
                words.extend(self.text(child).split_whitespace());
            }
        }
        let mut operands = operands.into_iter();
        let left = operands.next().ok_or_else(|| SyntaxError::invalid(node))?;
        Ok(ExprKind::Compare {
            left: boxed(left),
            ops,
            comparators: operands.collect(),
        })
    }

    fn comprehensions(&self, node: Node<'_>) -> Result<Vec<Comprehension>> {
        let mut generators: Vec<Comprehension> = Vec::new();
        for child in named(node) {
            match child.kind() {
                "for_in_clause" => {
                    let rights = fields(child, "right");
                    let rights: Vec<Node<'_>> = rights.into_iter().filter(Node::is_named).collect();
                    if rights.len() != 1 {
                        return Err(SyntaxError::invalid(child));
                    }
                    generators.push(Comprehension {
                        target: self.target(self.required(child, "left")?, Ctx::Store)?,
                        iter: self.expression(rights[0])?,
                        ifs: Vec::new(),
                        is_async: all_children(child)
                            .first()
                            .is_some_and(|c| c.kind() == "async"),
                    });
                }
                "if_clause" => {
                    let inner = named(child)
                        .into_iter()
                        .next()
                        .ok_or_else(|| SyntaxError::invalid(child))?;
                    let condition = self.expression(inner)?;
                    generators
                        .last_mut()
                        .ok_or_else(|| SyntaxError::invalid(child))?
                        .ifs
                        .push(condition);
                }
                _ => {}
            }
        }
        Ok(generators)
    }

    // ----- strings ----------------------------------------------------

    /// A string literal or an implicit concatenation: one `Constant`, or a
    /// `JoinedStr` when any part is an f-string.
    fn string(&self, node: Node<'_>) -> Result<Expr> {
        let parts = if node.kind() == "concatenated_string" {
            named(node)
        } else {
            vec![node]
        };
        let mut prefixes = Vec::with_capacity(parts.len());
        for part in &parts {
            let start = all_children(*part)
                .into_iter()
                .find(|c| c.kind() == "string_start")
                .ok_or_else(|| SyntaxError::invalid(*part))?;
            prefixes.push((Prefix::parse(self.text(start)), start));
        }
        let any_bytes = prefixes.iter().any(|(p, _)| p.bytes);
        if any_bytes && prefixes.iter().any(|(p, _)| !p.bytes) {
            return Err(SyntaxError::at(
                "cannot mix bytes and nonbytes literals",
                node,
            ));
        }
        let u_prefix = prefixes.first().is_some_and(|(p, _)| p.unicode);
        if any_bytes {
            let mut value = Vec::new();
            for (part, (prefix, start)) in parts.iter().zip(&prefixes) {
                value.extend(decode_bytes(self.body(*part, *start), prefix.raw));
            }
            return Ok(expr(
                loc(node),
                ExprKind::Constant {
                    value: Const::Bytes(value),
                    u_prefix: false,
                },
            ));
        }
        if !prefixes.iter().any(|(p, _)| p.format) {
            let mut value = String::new();
            for (part, (prefix, start)) in parts.iter().zip(&prefixes) {
                value.push_str(&decode_str(self.body(*part, *start), prefix.raw));
            }
            return Ok(expr(
                loc(node),
                ExprKind::Constant {
                    value: Const::Str(value),
                    u_prefix,
                },
            ));
        }
        let mut values = Vec::new();
        let mut pending = String::new();
        for (part, (prefix, start)) in parts.iter().zip(&prefixes) {
            if prefix.format {
                self.fstring_parts(*part, prefix.raw, &mut pending, &mut values)?;
            } else {
                pending.push_str(&decode_str(self.body(*part, *start), prefix.raw));
            }
        }
        flush(&mut pending, &mut values, loc(node));
        Ok(expr(loc(node), ExprKind::JoinedStr(values)))
    }

    /// The text between a string's quotes.
    fn body(&self, part: Node<'_>, start: Node<'_>) -> &'s str {
        let end = all_children(part)
            .into_iter()
            .rev()
            .find(|c| c.kind() == "string_end");
        let from = start.end_byte();
        let to = end.map_or(part.end_byte(), |e| e.start_byte());
        self.src.get(from..to).unwrap_or("")
    }

    fn fstring_parts(
        &self,
        part: Node<'_>,
        raw: bool,
        pending: &mut String,
        values: &mut Vec<Expr>,
    ) -> Result<()> {
        for child in all_children(part) {
            match child.kind() {
                "string_content" => {
                    let text = decode_str(self.text(child), raw);
                    pending.push_str(&text.replace("{{", "{").replace("}}", "}"));
                }
                "interpolation" => {
                    let value = self.interpolation(child, pending)?;
                    flush(pending, values, loc(part));
                    values.push(value);
                }
                _ => {}
            }
        }
        Ok(())
    }

    /// One `{expr!c:spec}`. A self-documenting `{x=}` adds its source text
    /// to the preceding constant and defaults the conversion to `!r`.
    fn interpolation(&self, node: Node<'_>, pending: &mut String) -> Result<Expr> {
        let expression = self.required(node, "expression")?;
        let value = self.expression(expression)?;
        let conversion = field(node, "type_conversion")
            .and_then(|c| self.text(c).chars().nth(1))
            .map_or(-1, |c| i32::try_from(u32::from(c)).unwrap_or(-1));
        let format_spec = field(node, "format_specifier");
        let mut conversion = conversion;
        if let Some(equals) = all_children(node)
            .into_iter()
            .find(|c| !c.is_named() && c.kind() == "=")
        {
            let from = node.start_byte() + 1;
            pending.push_str(self.src.get(from..equals.end_byte()).unwrap_or(""));
            if conversion == -1 && format_spec.is_none() {
                conversion = i32::from(b'r');
            }
        }
        let format_spec = match format_spec {
            Some(spec) => {
                // The literal text is a regex token, not a child: it is
                // whatever lies between the `:` and the nested fields.
                let mut spec_values = Vec::new();
                let mut spec_pending = String::new();
                let mut cursor = spec.start_byte();
                for child in all_children(spec) {
                    if child.kind() == ":" && cursor == spec.start_byte() {
                        cursor = child.end_byte();
                    } else if child.kind() == "format_expression" {
                        spec_pending
                            .push_str(self.src.get(cursor..child.start_byte()).unwrap_or(""));
                        let inner = self.interpolation(child, &mut spec_pending)?;
                        flush(&mut spec_pending, &mut spec_values, loc(spec));
                        spec_values.push(inner);
                        cursor = child.end_byte();
                    }
                }
                spec_pending.push_str(self.src.get(cursor..spec.end_byte()).unwrap_or(""));
                flush(&mut spec_pending, &mut spec_values, loc(spec));
                Some(boxed(expr(loc(spec), ExprKind::JoinedStr(spec_values))))
            }
            None => None,
        };
        Ok(expr(
            loc(node),
            ExprKind::FormattedValue {
                value: boxed(value),
                conversion,
                format_spec,
            },
        ))
    }
}

fn flush(pending: &mut String, values: &mut Vec<Expr>, at: Loc) {
    if !pending.is_empty() {
        values.push(expr(
            at,
            ExprKind::Constant {
                value: Const::Str(std::mem::take(pending)),
                u_prefix: false,
            },
        ));
    }
}

fn cmp_op(text: &str) -> Option<CmpOp> {
    Some(match text {
        "<" => CmpOp::Lt,
        ">" => CmpOp::Gt,
        "==" => CmpOp::Eq,
        ">=" => CmpOp::GtE,
        "<=" => CmpOp::LtE,
        "!=" => CmpOp::NotEq,
        "in" => CmpOp::In,
        "not in" => CmpOp::NotIn,
        "is" => CmpOp::Is,
        "is not" => CmpOp::IsNot,
        _ => return None,
    })
}

fn constant(value: Const) -> ExprKind {
    ExprKind::Constant {
        value,
        u_prefix: false,
    }
}

fn parse_float(text: &str) -> f64 {
    let clean: String = text.chars().filter(|&c| c != '_').collect();
    clean.parse().unwrap_or(0.0)
}

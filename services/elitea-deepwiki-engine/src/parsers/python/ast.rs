//! A Python `ast` module tree, as Python 3.12 builds it.
//!
//! WHY a tree of our own: the Python parser walks the stdlib `ast`, and its
//! output depends on what `ast` hands it — node classes, positions, field
//! ORDER (`NodeVisitor.generic_visit` and `ast.walk` follow `_fields`), and
//! expression contexts. tree-sitter's concrete tree has none of these
//! directly, so [`super::lower`] turns it into this tree first and the
//! extractors port the Python visitors over it one to one.
//!
//! Only what the extractors and `ast.unparse` read is kept: operators,
//! contexts and constants are kept, type comments and `kind` strings other
//! than the `u` prefix are not.

/// `lineno`, `col_offset`, `end_lineno`, `end_col_offset`: 1-based lines and
/// UTF-8 byte columns, as `ast` reports them.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Default)]
pub struct Loc {
    pub line: u32,
    pub col: u32,
    pub end_line: u32,
    pub end_col: u32,
}

impl Loc {
    /// The span from `self`'s start to `end`'s end.
    #[must_use]
    pub fn to(self, end: Loc) -> Loc {
        Loc {
            line: self.line,
            col: self.col,
            end_line: end.end_line,
            end_col: end.end_col,
        }
    }
}

/// `expr_context`.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Ctx {
    Load,
    Store,
    Del,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum BoolOp {
    And,
    Or,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum BinOp {
    Add,
    Sub,
    Mult,
    MatMult,
    Div,
    Mod,
    Pow,
    LShift,
    RShift,
    BitOr,
    BitXor,
    BitAnd,
    FloorDiv,
}

impl BinOp {
    /// The operator's source text.
    #[must_use]
    pub fn parse(text: &str) -> Option<Self> {
        Some(match text.trim_end_matches('=') {
            "+" => Self::Add,
            "-" => Self::Sub,
            "*" => Self::Mult,
            "@" => Self::MatMult,
            "/" => Self::Div,
            "%" => Self::Mod,
            "**" => Self::Pow,
            "<<" => Self::LShift,
            ">>" => Self::RShift,
            "|" => Self::BitOr,
            "^" => Self::BitXor,
            "&" => Self::BitAnd,
            "//" => Self::FloorDiv,
            _ => return None,
        })
    }

    #[must_use]
    pub fn text(self) -> &'static str {
        match self {
            Self::Add => "+",
            Self::Sub => "-",
            Self::Mult => "*",
            Self::MatMult => "@",
            Self::Div => "/",
            Self::Mod => "%",
            Self::Pow => "**",
            Self::LShift => "<<",
            Self::RShift => ">>",
            Self::BitOr => "|",
            Self::BitXor => "^",
            Self::BitAnd => "&",
            Self::FloorDiv => "//",
        }
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum UnaryOp {
    Invert,
    Not,
    UAdd,
    USub,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum CmpOp {
    Eq,
    NotEq,
    Lt,
    LtE,
    Gt,
    GtE,
    Is,
    IsNot,
    In,
    NotIn,
}

impl CmpOp {
    #[must_use]
    pub fn text(self) -> &'static str {
        match self {
            Self::Eq => "==",
            Self::NotEq => "!=",
            Self::Lt => "<",
            Self::LtE => "<=",
            Self::Gt => ">",
            Self::GtE => ">=",
            Self::Is => "is",
            Self::IsNot => "is not",
            Self::In => "in",
            Self::NotIn => "not in",
        }
    }
}

/// A `Constant` value.
#[derive(Debug, Clone, PartialEq)]
pub enum Const {
    None,
    Bool(bool),
    Ellipsis,
    Str(String),
    Bytes(Vec<u8>),
    /// The decimal digits (Python ints are unbounded; this is `repr`).
    Int(String),
    Float(f64),
    /// An imaginary literal (`2j`): the imaginary part.
    Complex(f64),
}

#[derive(Debug, Clone, PartialEq)]
pub struct Expr {
    pub loc: Loc,
    pub kind: ExprKind,
}

#[derive(Debug, Clone, PartialEq)]
pub enum ExprKind {
    BoolOp {
        op: BoolOp,
        values: Vec<Expr>,
    },
    NamedExpr {
        target: Box<Expr>,
        value: Box<Expr>,
    },
    BinOp {
        left: Box<Expr>,
        op: BinOp,
        right: Box<Expr>,
    },
    UnaryOp {
        op: UnaryOp,
        operand: Box<Expr>,
    },
    Lambda {
        args: Box<Arguments>,
        body: Box<Expr>,
    },
    IfExp {
        test: Box<Expr>,
        body: Box<Expr>,
        orelse: Box<Expr>,
    },
    /// `keys[i]` is `None` for a `**mapping` entry.
    Dict {
        keys: Vec<Option<Expr>>,
        values: Vec<Expr>,
    },
    Set(Vec<Expr>),
    ListComp {
        elt: Box<Expr>,
        generators: Vec<Comprehension>,
    },
    SetComp {
        elt: Box<Expr>,
        generators: Vec<Comprehension>,
    },
    GeneratorExp {
        elt: Box<Expr>,
        generators: Vec<Comprehension>,
    },
    DictComp {
        key: Box<Expr>,
        value: Box<Expr>,
        generators: Vec<Comprehension>,
    },
    Await(Box<Expr>),
    Yield(Option<Box<Expr>>),
    YieldFrom(Box<Expr>),
    Compare {
        left: Box<Expr>,
        ops: Vec<CmpOp>,
        comparators: Vec<Expr>,
    },
    Call {
        func: Box<Expr>,
        args: Vec<Expr>,
        keywords: Vec<Keyword>,
    },
    /// `conversion` is `-1` or the character code (`'r'` …).
    FormattedValue {
        value: Box<Expr>,
        conversion: i32,
        format_spec: Option<Box<Expr>>,
    },
    JoinedStr(Vec<Expr>),
    /// `u_prefix` is `kind == "u"`, which `ast.unparse` writes back.
    Constant {
        value: Const,
        u_prefix: bool,
    },
    Attribute {
        value: Box<Expr>,
        attr: String,
        ctx: Ctx,
    },
    Subscript {
        value: Box<Expr>,
        slice: Box<Expr>,
        ctx: Ctx,
    },
    Starred {
        value: Box<Expr>,
        ctx: Ctx,
    },
    Name {
        id: String,
        ctx: Ctx,
    },
    List {
        elts: Vec<Expr>,
        ctx: Ctx,
    },
    Tuple {
        elts: Vec<Expr>,
        ctx: Ctx,
    },
    Slice {
        lower: Option<Box<Expr>>,
        upper: Option<Box<Expr>>,
        step: Option<Box<Expr>>,
    },
}

#[derive(Debug, Clone, PartialEq)]
pub struct Keyword {
    /// `None` for `**kwargs`.
    pub arg: Option<String>,
    pub value: Expr,
}

#[derive(Debug, Clone, PartialEq)]
pub struct Comprehension {
    pub target: Expr,
    pub iter: Expr,
    pub ifs: Vec<Expr>,
    pub is_async: bool,
}

/// `arg`: the span runs from the name to the end of the annotation.
#[derive(Debug, Clone, PartialEq)]
pub struct Arg {
    pub loc: Loc,
    pub name: String,
    pub annotation: Option<Expr>,
}

#[derive(Debug, Clone, PartialEq, Default)]
pub struct Arguments {
    pub posonlyargs: Vec<Arg>,
    pub args: Vec<Arg>,
    pub vararg: Option<Arg>,
    pub kwonlyargs: Vec<Arg>,
    /// One per keyword-only argument; `None` where it has no default.
    pub kw_defaults: Vec<Option<Expr>>,
    pub kwarg: Option<Arg>,
    /// The defaults of the LAST positional arguments.
    pub defaults: Vec<Expr>,
}

/// A PEP 695 type parameter; only a `TypeVar` bound holds expressions.
#[derive(Debug, Clone, PartialEq)]
pub struct TypeParam {
    pub bound: Option<Expr>,
}

#[derive(Debug, Clone, PartialEq)]
pub struct FunctionDef {
    pub name: String,
    pub is_async: bool,
    pub args: Arguments,
    pub body: Vec<Stmt>,
    pub decorators: Vec<Expr>,
    pub returns: Option<Expr>,
    pub type_params: Vec<TypeParam>,
}

#[derive(Debug, Clone, PartialEq)]
pub struct ClassDef {
    pub name: String,
    pub bases: Vec<Expr>,
    pub keywords: Vec<Keyword>,
    pub body: Vec<Stmt>,
    pub decorators: Vec<Expr>,
    pub type_params: Vec<TypeParam>,
}

#[derive(Debug, Clone, PartialEq)]
pub struct Alias {
    pub name: String,
    pub asname: Option<String>,
}

#[derive(Debug, Clone, PartialEq)]
pub struct WithItem {
    pub context_expr: Expr,
    pub optional_vars: Option<Expr>,
}

#[derive(Debug, Clone, PartialEq)]
pub struct ExceptHandler {
    pub type_: Option<Expr>,
    pub body: Vec<Stmt>,
}

#[derive(Debug, Clone, PartialEq)]
pub struct MatchCase {
    pub pattern: Pattern,
    pub guard: Option<Expr>,
    pub body: Vec<Stmt>,
}

/// `pattern`: capture names are plain strings in `ast`, so only the
/// expressions a pattern holds (value patterns, class names, mapping keys)
/// are kept.
#[derive(Debug, Clone, PartialEq)]
pub enum Pattern {
    Value(Expr),
    Singleton,
    Sequence(Vec<Pattern>),
    Mapping {
        keys: Vec<Expr>,
        patterns: Vec<Pattern>,
    },
    Class {
        cls: Expr,
        patterns: Vec<Pattern>,
        kwd_patterns: Vec<Pattern>,
    },
    Star,
    As(Option<Box<Pattern>>),
    Or(Vec<Pattern>),
}

#[derive(Debug, Clone, PartialEq)]
pub struct Stmt {
    pub loc: Loc,
    pub kind: StmtKind,
}

#[derive(Debug, Clone, PartialEq)]
pub enum StmtKind {
    FunctionDef(Box<FunctionDef>),
    ClassDef(Box<ClassDef>),
    Return(Option<Expr>),
    Delete(Vec<Expr>),
    Assign {
        targets: Vec<Expr>,
        value: Expr,
    },
    TypeAlias {
        name: Expr,
        type_params: Vec<TypeParam>,
        value: Expr,
    },
    AugAssign {
        target: Expr,
        op: BinOp,
        value: Expr,
    },
    AnnAssign {
        target: Expr,
        annotation: Expr,
        value: Option<Expr>,
    },
    For {
        target: Expr,
        iter: Expr,
        body: Vec<Stmt>,
        orelse: Vec<Stmt>,
    },
    While {
        test: Expr,
        body: Vec<Stmt>,
        orelse: Vec<Stmt>,
    },
    If {
        test: Expr,
        body: Vec<Stmt>,
        orelse: Vec<Stmt>,
    },
    With {
        items: Vec<WithItem>,
        body: Vec<Stmt>,
    },
    Match {
        subject: Expr,
        cases: Vec<MatchCase>,
    },
    Raise {
        exc: Option<Expr>,
        cause: Option<Expr>,
    },
    Try {
        body: Vec<Stmt>,
        handlers: Vec<ExceptHandler>,
        orelse: Vec<Stmt>,
        finalbody: Vec<Stmt>,
    },
    Assert {
        test: Expr,
        msg: Option<Expr>,
    },
    Import(Vec<Alias>),
    ImportFrom {
        module: Option<String>,
        names: Vec<Alias>,
    },
    Global,
    Nonlocal,
    Expr(Expr),
    Pass,
    Break,
    Continue,
}

/// A borrowed node of any class, for the generic walks.
#[derive(Debug, Clone, Copy)]
pub enum Node<'a> {
    Stmt(&'a Stmt),
    Expr(&'a Expr),
    Arguments(&'a Arguments),
    Arg(&'a Arg),
    Keyword(&'a Keyword),
    Comprehension(&'a Comprehension),
    ExceptHandler(&'a ExceptHandler),
    WithItem(&'a WithItem),
    MatchCase(&'a MatchCase),
    Pattern(&'a Pattern),
    TypeParam(&'a TypeParam),
}

/// `ast.iter_child_nodes`: every child node, in `_fields` order.
///
/// Leaf objects `ast` also yields (operators, contexts, `alias`) are left
/// out: no extractor looks for them and they have no children.
pub fn for_each_child<'a>(node: Node<'a>, f: &mut impl FnMut(Node<'a>)) {
    match node {
        Node::Stmt(stmt) => stmt_children(stmt, f),
        Node::Expr(expr) => expr_children(expr, f),
        Node::Arguments(a) => {
            for arg in a
                .posonlyargs
                .iter()
                .chain(&a.args)
                .chain(&a.vararg)
                .chain(&a.kwonlyargs)
            {
                f(Node::Arg(arg));
            }
            for default in a.kw_defaults.iter().flatten() {
                f(Node::Expr(default));
            }
            if let Some(kwarg) = &a.kwarg {
                f(Node::Arg(kwarg));
            }
            exprs(&a.defaults, f);
        }
        Node::Arg(arg) => {
            if let Some(annotation) = &arg.annotation {
                f(Node::Expr(annotation));
            }
        }
        Node::Keyword(k) => f(Node::Expr(&k.value)),
        Node::Comprehension(c) => {
            f(Node::Expr(&c.target));
            f(Node::Expr(&c.iter));
            exprs(&c.ifs, f);
        }
        Node::ExceptHandler(h) => {
            if let Some(type_) = &h.type_ {
                f(Node::Expr(type_));
            }
            stmts(&h.body, f);
        }
        Node::WithItem(w) => {
            f(Node::Expr(&w.context_expr));
            if let Some(vars) = &w.optional_vars {
                f(Node::Expr(vars));
            }
        }
        Node::MatchCase(c) => {
            f(Node::Pattern(&c.pattern));
            if let Some(guard) = &c.guard {
                f(Node::Expr(guard));
            }
            stmts(&c.body, f);
        }
        Node::Pattern(p) => pattern_children(p, f),
        Node::TypeParam(t) => {
            if let Some(bound) = &t.bound {
                f(Node::Expr(bound));
            }
        }
    }
}

fn exprs<'a>(items: &'a [Expr], f: &mut impl FnMut(Node<'a>)) {
    for item in items {
        f(Node::Expr(item));
    }
}

fn stmts<'a>(items: &'a [Stmt], f: &mut impl FnMut(Node<'a>)) {
    for item in items {
        f(Node::Stmt(item));
    }
}

fn type_params<'a>(items: &'a [TypeParam], f: &mut impl FnMut(Node<'a>)) {
    for item in items {
        f(Node::TypeParam(item));
    }
}

#[allow(clippy::too_many_lines)] // one arm per `stmt` class
fn stmt_children<'a>(stmt: &'a Stmt, f: &mut impl FnMut(Node<'a>)) {
    match &stmt.kind {
        StmtKind::FunctionDef(def) => {
            f(Node::Arguments(&def.args));
            stmts(&def.body, f);
            exprs(&def.decorators, f);
            if let Some(returns) = &def.returns {
                f(Node::Expr(returns));
            }
            type_params(&def.type_params, f);
        }
        StmtKind::ClassDef(def) => {
            exprs(&def.bases, f);
            for keyword in &def.keywords {
                f(Node::Keyword(keyword));
            }
            stmts(&def.body, f);
            exprs(&def.decorators, f);
            type_params(&def.type_params, f);
        }
        StmtKind::Return(value) => {
            if let Some(value) = value {
                f(Node::Expr(value));
            }
        }
        StmtKind::Delete(targets) => exprs(targets, f),
        StmtKind::Assign { targets, value } => {
            exprs(targets, f);
            f(Node::Expr(value));
        }
        StmtKind::TypeAlias {
            name,
            type_params: params,
            value,
        } => {
            f(Node::Expr(name));
            type_params(params, f);
            f(Node::Expr(value));
        }
        StmtKind::AugAssign { target, value, .. } => {
            f(Node::Expr(target));
            f(Node::Expr(value));
        }
        StmtKind::AnnAssign {
            target,
            annotation,
            value,
        } => {
            f(Node::Expr(target));
            f(Node::Expr(annotation));
            if let Some(value) = value {
                f(Node::Expr(value));
            }
        }
        StmtKind::For {
            target,
            iter,
            body,
            orelse,
        } => {
            f(Node::Expr(target));
            f(Node::Expr(iter));
            stmts(body, f);
            stmts(orelse, f);
        }
        StmtKind::While { test, body, orelse } | StmtKind::If { test, body, orelse } => {
            f(Node::Expr(test));
            stmts(body, f);
            stmts(orelse, f);
        }
        StmtKind::With { items, body } => {
            for item in items {
                f(Node::WithItem(item));
            }
            stmts(body, f);
        }
        StmtKind::Match { subject, cases } => {
            f(Node::Expr(subject));
            for case in cases {
                f(Node::MatchCase(case));
            }
        }
        StmtKind::Raise { exc, cause } => {
            for e in [exc, cause].into_iter().flatten() {
                f(Node::Expr(e));
            }
        }
        StmtKind::Try {
            body,
            handlers,
            orelse,
            finalbody,
        } => {
            stmts(body, f);
            for handler in handlers {
                f(Node::ExceptHandler(handler));
            }
            stmts(orelse, f);
            stmts(finalbody, f);
        }
        StmtKind::Assert { test, msg } => {
            f(Node::Expr(test));
            if let Some(msg) = msg {
                f(Node::Expr(msg));
            }
        }
        StmtKind::Expr(value) => f(Node::Expr(value)),
        StmtKind::Import(_)
        | StmtKind::ImportFrom { .. }
        | StmtKind::Global
        | StmtKind::Nonlocal
        | StmtKind::Pass
        | StmtKind::Break
        | StmtKind::Continue => {}
    }
}

fn comprehensions<'a>(items: &'a [Comprehension], f: &mut impl FnMut(Node<'a>)) {
    for item in items {
        f(Node::Comprehension(item));
    }
}

#[allow(clippy::too_many_lines)] // one arm per `expr` class
fn expr_children<'a>(expr: &'a Expr, f: &mut impl FnMut(Node<'a>)) {
    match &expr.kind {
        ExprKind::BoolOp { values, .. } => exprs(values, f),
        ExprKind::NamedExpr { target, value } => {
            f(Node::Expr(target));
            f(Node::Expr(value));
        }
        ExprKind::BinOp { left, right, .. } => {
            f(Node::Expr(left));
            f(Node::Expr(right));
        }
        ExprKind::UnaryOp { operand, .. } => f(Node::Expr(operand)),
        ExprKind::Lambda { args, body } => {
            f(Node::Arguments(args));
            f(Node::Expr(body));
        }
        ExprKind::IfExp { test, body, orelse } => {
            f(Node::Expr(test));
            f(Node::Expr(body));
            f(Node::Expr(orelse));
        }
        ExprKind::Dict { keys, values } => {
            for key in keys.iter().flatten() {
                f(Node::Expr(key));
            }
            exprs(values, f);
        }
        ExprKind::Set(elts)
        | ExprKind::JoinedStr(elts)
        | ExprKind::List { elts, .. }
        | ExprKind::Tuple { elts, .. } => exprs(elts, f),
        ExprKind::ListComp { elt, generators }
        | ExprKind::SetComp { elt, generators }
        | ExprKind::GeneratorExp { elt, generators } => {
            f(Node::Expr(elt));
            comprehensions(generators, f);
        }
        ExprKind::DictComp {
            key,
            value,
            generators,
        } => {
            f(Node::Expr(key));
            f(Node::Expr(value));
            comprehensions(generators, f);
        }
        ExprKind::Await(value) | ExprKind::YieldFrom(value) => f(Node::Expr(value)),
        ExprKind::Yield(value) => {
            if let Some(value) = value {
                f(Node::Expr(value));
            }
        }
        ExprKind::Compare {
            left, comparators, ..
        } => {
            f(Node::Expr(left));
            exprs(comparators, f);
        }
        ExprKind::Call {
            func,
            args,
            keywords,
        } => {
            f(Node::Expr(func));
            exprs(args, f);
            for keyword in keywords {
                f(Node::Keyword(keyword));
            }
        }
        ExprKind::FormattedValue {
            value, format_spec, ..
        } => {
            f(Node::Expr(value));
            if let Some(spec) = format_spec {
                f(Node::Expr(spec));
            }
        }
        ExprKind::Attribute { value, .. } | ExprKind::Starred { value, .. } => {
            f(Node::Expr(value));
        }
        ExprKind::Subscript { value, slice, .. } => {
            f(Node::Expr(value));
            f(Node::Expr(slice));
        }
        ExprKind::Slice { lower, upper, step } => {
            for part in [lower, upper, step].into_iter().flatten() {
                f(Node::Expr(part));
            }
        }
        ExprKind::Constant { .. } | ExprKind::Name { .. } => {}
    }
}

fn pattern_children<'a>(pattern: &'a Pattern, f: &mut impl FnMut(Node<'a>)) {
    let patterns = |items: &'a [Pattern], f: &mut dyn FnMut(Node<'a>)| {
        for item in items {
            f(Node::Pattern(item));
        }
    };
    match pattern {
        Pattern::Value(value) => f(Node::Expr(value)),
        Pattern::Sequence(items) | Pattern::Or(items) => patterns(items, f),
        Pattern::Mapping { keys, patterns: p } => {
            exprs(keys, f);
            patterns(p, f);
        }
        Pattern::Class {
            cls,
            patterns: p,
            kwd_patterns,
        } => {
            f(Node::Expr(cls));
            patterns(p, f);
            patterns(kwd_patterns, f);
        }
        Pattern::As(inner) => {
            if let Some(inner) = inner {
                f(Node::Pattern(inner));
            }
        }
        Pattern::Singleton | Pattern::Star => {}
    }
}

/// `ast.walk`: breadth first, the node itself first.
pub fn walk<'a>(root: Node<'a>, f: &mut impl FnMut(Node<'a>)) {
    let mut queue = std::collections::VecDeque::from([root]);
    while let Some(node) = queue.pop_front() {
        for_each_child(node, &mut |child| queue.push_back(child));
        f(node);
    }
}

/// `ast.walk` over a module body (the `Module` node yields nothing itself).
pub fn walk_module<'a>(body: &'a [Stmt], f: &mut impl FnMut(Node<'a>)) {
    let mut queue: std::collections::VecDeque<Node<'a>> = body.iter().map(Node::Stmt).collect();
    while let Some(node) = queue.pop_front() {
        for_each_child(node, &mut |child| queue.push_back(child));
        f(node);
    }
}

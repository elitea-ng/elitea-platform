//! `ast.unparse` for expressions, as Python 3.12's `_Unparser` writes them.
//!
//! WHY: `_get_type_annotation` falls back to `ast.unparse` for every
//! annotation that is not a name, attribute, constant or subscript — a tuple
//! slice (`Dict[str, int]` gives `Dict[(str, int)]`), `X | None`,
//! `Annotated[str, Field(...)]` — and those strings are return types,
//! parameter types and signatures. The precedence rules, the
//! parenthesised top-level tuple and `repr`-quoted constants are ported
//! from `_Unparser`; statements are never unparsed by the parser.
//!
//! Known gap: f-string quoting follows `_Unparser` for the common cases only
//! (no `repr` fallback when no quote type fits every part).

use super::ast::{Arguments, BinOp, BoolOp, Comprehension, Const, Expr, ExprKind, UnaryOp};
use super::text::{repr_bytes, repr_float, repr_imaginary, repr_str};

/// `_Precedence`.
#[derive(Debug, Clone, Copy, PartialEq, Eq, PartialOrd, Ord)]
enum Prec {
    NamedExpr = 1,
    Tuple,
    Yield,
    Test,
    Or,
    And,
    Not,
    Cmp,
    /// `EXPR` and `BOR` share a value.
    Expr,
    BXor,
    BAnd,
    Shift,
    Arith,
    Term,
    Factor,
    Power,
    Await,
    Atom,
}

impl Prec {
    fn next(self) -> Self {
        match self {
            Self::NamedExpr => Self::Tuple,
            Self::Tuple => Self::Yield,
            Self::Yield => Self::Test,
            Self::Test => Self::Or,
            Self::Or => Self::And,
            Self::And => Self::Not,
            Self::Not => Self::Cmp,
            Self::Cmp => Self::Expr,
            Self::Expr => Self::BXor,
            Self::BXor => Self::BAnd,
            Self::BAnd => Self::Shift,
            Self::Shift => Self::Arith,
            Self::Arith => Self::Term,
            Self::Term => Self::Factor,
            Self::Factor => Self::Power,
            Self::Power => Self::Await,
            Self::Await | Self::Atom => Self::Atom,
        }
    }
}

fn binop_prec(op: BinOp) -> Prec {
    match op {
        BinOp::Add | BinOp::Sub => Prec::Arith,
        BinOp::Mult | BinOp::MatMult | BinOp::Div | BinOp::Mod | BinOp::FloorDiv => Prec::Term,
        BinOp::LShift | BinOp::RShift => Prec::Shift,
        BinOp::BitOr => Prec::Expr,
        BinOp::BitXor => Prec::BXor,
        BinOp::BitAnd => Prec::BAnd,
        BinOp::Pow => Prec::Power,
    }
}

/// `ast.unparse(expr)`.
#[must_use]
pub fn unparse(expr: &Expr) -> String {
    let mut out = String::new();
    write_expr(&mut out, expr, Prec::Test);
    out
}

/// Write `expr` given the precedence its parent set (`get_precedence`;
/// `TEST` when the parent set none).
#[allow(clippy::too_many_lines)]
fn write_expr(out: &mut String, expr: &Expr, prec: Prec) {
    let parens = |out: &mut String, own: Prec, f: &mut dyn FnMut(&mut String)| {
        let wrap = prec > own;
        if wrap {
            out.push('(');
        }
        f(out);
        if wrap {
            out.push(')');
        }
    };
    match &expr.kind {
        ExprKind::Name { id, .. } => out.push_str(id),
        ExprKind::Constant { value, u_prefix } => {
            if matches!(value, Const::Ellipsis) {
                out.push_str("...");
            } else {
                if *u_prefix {
                    out.push('u');
                }
                write_constant(out, value);
            }
        }
        ExprKind::Attribute { value, attr, .. } => {
            write_expr(out, value, Prec::Atom);
            if matches!(
                value.kind,
                ExprKind::Constant {
                    value: Const::Int(_),
                    ..
                }
            ) {
                out.push(' ');
            }
            out.push('.');
            out.push_str(attr);
        }
        ExprKind::Subscript { value, slice, .. } => {
            write_expr(out, value, Prec::Atom);
            out.push('[');
            match &slice.kind {
                ExprKind::Tuple { elts, .. } if !elts.is_empty() => items_view(out, elts),
                _ => write_expr(out, slice, Prec::Test),
            }
            out.push(']');
        }
        ExprKind::Starred { value, .. } => {
            out.push('*');
            write_expr(out, value, Prec::Expr);
        }
        ExprKind::Tuple { elts, .. } => {
            let wrap = elts.is_empty() || prec > Prec::Tuple;
            if wrap {
                out.push('(');
            }
            items_view(out, elts);
            if wrap {
                out.push(')');
            }
        }
        ExprKind::List { elts, .. } => {
            out.push('[');
            interleave(out, elts);
            out.push(']');
        }
        ExprKind::Set(elts) => {
            if elts.is_empty() {
                out.push_str("{*()}");
            } else {
                out.push('{');
                interleave(out, elts);
                out.push('}');
            }
        }
        ExprKind::Dict { keys, values } => {
            out.push('{');
            for (index, (key, value)) in keys.iter().zip(values).enumerate() {
                if index > 0 {
                    out.push_str(", ");
                }
                if let Some(key) = key {
                    write_expr(out, key, Prec::Test);
                    out.push_str(": ");
                    write_expr(out, value, Prec::Test);
                } else {
                    out.push_str("**");
                    write_expr(out, value, Prec::Expr);
                }
            }
            out.push('}');
        }
        ExprKind::Call {
            func,
            args,
            keywords,
        } => {
            write_expr(out, func, Prec::Atom);
            out.push('(');
            let mut comma = false;
            for arg in args {
                if comma {
                    out.push_str(", ");
                }
                comma = true;
                write_expr(out, arg, Prec::Test);
            }
            for keyword in keywords {
                if comma {
                    out.push_str(", ");
                }
                comma = true;
                match &keyword.arg {
                    Some(name) => {
                        out.push_str(name);
                        out.push('=');
                    }
                    None => out.push_str("**"),
                }
                write_expr(out, &keyword.value, Prec::Test);
            }
            out.push(')');
        }
        ExprKind::BinOp { left, op, right } => {
            let own = binop_prec(*op);
            parens(out, own, &mut |out| {
                let (left_prec, right_prec) = if *op == BinOp::Pow {
                    (own.next(), own)
                } else {
                    (own, own.next())
                };
                write_expr(out, left, left_prec);
                out.push(' ');
                out.push_str(op.text());
                out.push(' ');
                write_expr(out, right, right_prec);
            });
        }
        ExprKind::UnaryOp { op, operand } => {
            let (text, own) = match op {
                UnaryOp::Not => ("not", Prec::Not),
                UnaryOp::Invert => ("~", Prec::Factor),
                UnaryOp::UAdd => ("+", Prec::Factor),
                UnaryOp::USub => ("-", Prec::Factor),
            };
            parens(out, own, &mut |out| {
                out.push_str(text);
                if own != Prec::Factor {
                    out.push(' ');
                }
                write_expr(out, operand, own);
            });
        }
        ExprKind::BoolOp { op, values } => {
            let (text, own) = match op {
                BoolOp::And => (" and ", Prec::And),
                BoolOp::Or => (" or ", Prec::Or),
            };
            parens(out, own, &mut |out| {
                let mut level = own;
                for (index, value) in values.iter().enumerate() {
                    if index > 0 {
                        out.push_str(text);
                    }
                    level = level.next();
                    write_expr(out, value, level);
                }
            });
        }
        ExprKind::Compare {
            left,
            ops,
            comparators,
        } => {
            parens(out, Prec::Cmp, &mut |out| {
                write_expr(out, left, Prec::Cmp.next());
                for (op, comparator) in ops.iter().zip(comparators) {
                    out.push(' ');
                    out.push_str(op.text());
                    out.push(' ');
                    write_expr(out, comparator, Prec::Cmp.next());
                }
            });
        }
        ExprKind::IfExp { test, body, orelse } => {
            parens(out, Prec::Test, &mut |out| {
                write_expr(out, body, Prec::Test.next());
                out.push_str(" if ");
                write_expr(out, test, Prec::Test.next());
                out.push_str(" else ");
                write_expr(out, orelse, Prec::Test);
            });
        }
        ExprKind::Lambda { args, body } => {
            parens(out, Prec::Test, &mut |out| {
                out.push_str("lambda");
                let mut buffer = String::new();
                write_arguments(&mut buffer, args);
                if !buffer.is_empty() {
                    out.push(' ');
                    out.push_str(&buffer);
                }
                out.push_str(": ");
                write_expr(out, body, Prec::Test);
            });
        }
        ExprKind::NamedExpr { target, value } => {
            parens(out, Prec::NamedExpr, &mut |out| {
                write_expr(out, target, Prec::Atom);
                out.push_str(" := ");
                write_expr(out, value, Prec::Atom);
            });
        }
        ExprKind::Await(value) => parens(out, Prec::Await, &mut |out| {
            out.push_str("await ");
            write_expr(out, value, Prec::Atom);
        }),
        ExprKind::Yield(value) => parens(out, Prec::Yield, &mut |out| {
            out.push_str("yield");
            if let Some(value) = value {
                out.push(' ');
                write_expr(out, value, Prec::Atom);
            }
        }),
        ExprKind::YieldFrom(value) => parens(out, Prec::Yield, &mut |out| {
            out.push_str("yield from ");
            write_expr(out, value, Prec::Atom);
        }),
        ExprKind::ListComp { elt, generators } => {
            comprehension(out, "[", "]", elt, None, generators);
        }
        ExprKind::SetComp { elt, generators } => {
            comprehension(out, "{", "}", elt, None, generators);
        }
        ExprKind::GeneratorExp { elt, generators } => {
            comprehension(out, "(", ")", elt, None, generators);
        }
        ExprKind::DictComp {
            key,
            value,
            generators,
        } => comprehension(out, "{", "}", key, Some(value), generators),
        ExprKind::Slice { lower, upper, step } => {
            if let Some(lower) = lower {
                write_expr(out, lower, Prec::Test);
            }
            out.push(':');
            if let Some(upper) = upper {
                write_expr(out, upper, Prec::Test);
            }
            if let Some(step) = step {
                out.push(':');
                write_expr(out, step, Prec::Test);
            }
        }
        ExprKind::JoinedStr(values) => write_joined_str(out, values),
        ExprKind::FormattedValue { .. } => {
            write_joined_str(out, std::slice::from_ref(expr));
        }
    }
}

fn interleave(out: &mut String, items: &[Expr]) {
    for (index, item) in items.iter().enumerate() {
        if index > 0 {
            out.push_str(", ");
        }
        write_expr(out, item, Prec::Test);
    }
}

/// `items_view`: a one-element sequence gets a trailing comma.
fn items_view(out: &mut String, items: &[Expr]) {
    if let [single] = items {
        write_expr(out, single, Prec::Test);
        out.push(',');
    } else {
        interleave(out, items);
    }
}

fn comprehension(
    out: &mut String,
    open: &str,
    close: &str,
    elt: &Expr,
    value: Option<&Expr>,
    generators: &[Comprehension],
) {
    out.push_str(open);
    write_expr(out, elt, Prec::Test);
    if let Some(value) = value {
        out.push_str(": ");
        write_expr(out, value, Prec::Test);
    }
    for generator in generators {
        out.push_str(if generator.is_async {
            " async for "
        } else {
            " for "
        });
        write_expr(out, &generator.target, Prec::Tuple);
        out.push_str(" in ");
        write_expr(out, &generator.iter, Prec::Test.next());
        for condition in &generator.ifs {
            out.push_str(" if ");
            write_expr(out, condition, Prec::Test.next());
        }
    }
    out.push_str(close);
}

/// `_INFSTR`: `1e` + `repr(sys.float_info.max_10_exp + 1)`.
const INFSTR: &str = "1e309";

fn write_constant(out: &mut String, value: &Const) {
    match value {
        Const::None => out.push_str("None"),
        Const::Bool(true) => out.push_str("True"),
        Const::Bool(false) => out.push_str("False"),
        Const::Ellipsis => out.push_str("Ellipsis"),
        Const::Str(s) => out.push_str(&repr_str(s)),
        Const::Bytes(b) => out.push_str(&repr_bytes(b)),
        Const::Int(digits) => out.push_str(digits),
        Const::Float(f) => out.push_str(
            &repr_float(*f)
                .replace("inf", INFSTR)
                .replace("nan", &format!("({INFSTR}-{INFSTR})")),
        ),
        Const::Complex(f) => out.push_str(&repr_imaginary(*f).replace("inf", INFSTR)),
    }
}

/// `visit_arguments`.
fn write_arguments(out: &mut String, args: &Arguments) {
    let mut first = true;
    let sep = |out: &mut String, first: &mut bool| {
        if *first {
            *first = false;
        } else {
            out.push_str(", ");
        }
    };
    let positional: Vec<_> = args.posonlyargs.iter().chain(&args.args).collect();
    let missing = positional.len().saturating_sub(args.defaults.len());
    for (index, arg) in positional.iter().enumerate() {
        sep(out, &mut first);
        out.push_str(&arg.name);
        if let Some(annotation) = &arg.annotation {
            out.push_str(": ");
            write_expr(out, annotation, Prec::Test);
        }
        if index >= missing
            && let Some(default) = args.defaults.get(index - missing)
        {
            out.push('=');
            write_expr(out, default, Prec::Test);
        }
        if index + 1 == args.posonlyargs.len() {
            out.push_str(", /");
        }
    }
    if args.vararg.is_some() || !args.kwonlyargs.is_empty() {
        sep(out, &mut first);
        out.push('*');
        if let Some(vararg) = &args.vararg {
            out.push_str(&vararg.name);
            if let Some(annotation) = &vararg.annotation {
                out.push_str(": ");
                write_expr(out, annotation, Prec::Test);
            }
        }
    }
    for (arg, default) in args.kwonlyargs.iter().zip(&args.kw_defaults) {
        out.push_str(", ");
        out.push_str(&arg.name);
        if let Some(annotation) = &arg.annotation {
            out.push_str(": ");
            write_expr(out, annotation, Prec::Test);
        }
        if let Some(default) = default {
            out.push('=');
            write_expr(out, default, Prec::Test);
        }
    }
    if let Some(kwarg) = &args.kwarg {
        sep(out, &mut first);
        out.push_str("**");
        out.push_str(&kwarg.name);
        if let Some(annotation) = &kwarg.annotation {
            out.push_str(": ");
            write_expr(out, annotation, Prec::Test);
        }
    }
}

const ALL_QUOTES: [&str; 4] = ["'", "\"", "\"\"\"", "'''"];

/// `visit_JoinedStr`, without the all-quotes-taken `repr` fallback.
fn write_joined_str(out: &mut String, values: &[Expr]) {
    let mut parts = Vec::with_capacity(values.len());
    for value in values {
        let mut buffer = String::new();
        write_fstring_inner(&mut buffer, value, false);
        let is_constant = matches!(value.kind, ExprKind::Constant { .. });
        parts.push((buffer, is_constant));
    }
    let mut quotes: Vec<&str> = ALL_QUOTES.to_vec();
    let mut written = String::new();
    for (value, is_constant) in parts {
        if is_constant {
            let escaped: String = value
                .chars()
                .map(|c| match c {
                    '\n' => "\\n".to_owned(),
                    '\t' => "\\t".to_owned(),
                    '\\' => "\\\\".to_owned(),
                    c => c.to_string(),
                })
                .collect();
            if escaped.contains('\n') {
                quotes.retain(|q| q.len() == 3);
            }
            let fitting: Vec<&str> = quotes
                .iter()
                .copied()
                .filter(|q| !escaped.contains(q))
                .collect();
            if !fitting.is_empty() {
                quotes = fitting;
            }
            written.push_str(&escaped);
        } else {
            if value.contains('\n') {
                quotes.retain(|q| q.len() == 3);
            }
            let fitting: Vec<&str> = quotes
                .iter()
                .copied()
                .filter(|q| !value.contains(q))
                .collect();
            if !fitting.is_empty() {
                quotes = fitting;
            }
            written.push_str(&value);
        }
    }
    let quote = quotes.first().copied().unwrap_or("'");
    out.push('f');
    out.push_str(quote);
    out.push_str(&written);
    out.push_str(quote);
}

fn write_fstring_inner(out: &mut String, node: &Expr, is_format_spec: bool) {
    match &node.kind {
        ExprKind::JoinedStr(values) => {
            for value in values {
                write_fstring_inner(out, value, is_format_spec);
            }
        }
        ExprKind::Constant {
            value: Const::Str(s),
            ..
        } => {
            let mut value = s.replace('{', "{{").replace('}', "}}");
            if is_format_spec {
                value = value
                    .replace('\\', "\\\\")
                    .replace('\'', "\\'")
                    .replace('"', "\\\"")
                    .replace('\n', "\\n");
            }
            out.push_str(&value);
        }
        ExprKind::FormattedValue {
            value,
            conversion,
            format_spec,
        } => {
            out.push('{');
            let mut inner = String::new();
            write_expr(&mut inner, value, Prec::Test.next());
            if inner.starts_with('{') {
                out.push(' ');
            }
            out.push_str(&inner);
            if *conversion != -1
                && let Some(c) = u32::try_from(*conversion).ok().and_then(char::from_u32)
            {
                out.push('!');
                out.push(c);
            }
            if let Some(spec) = format_spec {
                out.push(':');
                write_fstring_inner(out, spec, true);
            }
            out.push('}');
        }
        _ => {}
    }
}

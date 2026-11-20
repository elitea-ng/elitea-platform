//! The SDK's `query` argument, ported as a bounded, hand-written parser.
//!
//! Upstream passes the string to `pandas.DataFrame.query`. This reads the
//! literal-only subset of that syntax and evaluates it on the same bounded
//! scan as `filters`:
//!
//! * comparisons `==`, `!=`, `<>`, `<`, `<=`, `>`, `>=`, chained as in
//!   Python (`1 < a <= 5`), between columns and literals or between columns;
//! * `and`, `or`, `not` and the pandas spellings `&`, `|`, `~`, with
//!   parentheses;
//! * `in` and `not in` against a literal list (`[1, 2]` or `(1, 2)`);
//! * `is null`, `is not null` (also `is None`), the SQL spelling of a null
//!   test (pandas spells it `col.isnull()`, a method call);
//! * the arithmetic `+ - * / %` over numbers (`+` also joins text);
//! * integer, float, string (`'x'` or `"x"`), `True`, `False` and `None`
//!   literals, and backtick-quoted column names (`` `my col` ``).
//!
//! Everything else (function and method calls, attribute access, `@variable`,
//! `**`, `//`, subqueries, `like`) is refused with the offending token named.
//! Nothing is executed: the query is parsed to a tree of at most
//! [`MAX_NODES`] nodes and [`MAX_DEPTH`] levels and walked once per row.

use std::cmp::Ordering;

use delta_kernel::arrow::record_batch::RecordBatch;

use super::filter::{Cell, ColumnReader};

const MAX_QUERY_CHARS: usize = 2_048;
const MAX_TOKENS: usize = 1_024;
const MAX_NODES: usize = 512;
const MAX_DEPTH: usize = 32;
const MAX_LIST_ITEMS: usize = 1_000;
const MAX_TEXT_BYTES: usize = 4_096;

#[derive(Clone, Debug, PartialEq)]
enum Value {
    Null,
    Bool(bool),
    Int(i128),
    Float(f64),
    Text(String),
    List(Vec<Value>),
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
enum Cmp {
    Eq,
    Ne,
    Lt,
    Le,
    Gt,
    Ge,
    In,
    NotIn,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
enum Arith {
    Add,
    Sub,
    Mul,
    Div,
    Rem,
}

#[derive(Debug)]
enum Node {
    Literal(Value),
    Column(usize),
    Negate(Box<Node>),
    Arith(Box<Node>, Arith, Box<Node>),
    Compare(Vec<Node>, Vec<Cmp>),
    IsNull(Box<Node>, bool),
    Not(Box<Node>),
    And(Vec<Node>),
    Or(Vec<Node>),
}

/// A parsed `query`.
#[derive(Debug)]
pub(in crate::toolkits) struct Query {
    root: Node,
    columns: Vec<String>,
}

#[derive(Clone, Debug, PartialEq)]
enum Token {
    Number(String),
    Text(String),
    Word(String),
    Quoted(String),
    Symbol(&'static str),
}

struct Lexed {
    token: Token,
    position: usize,
    shown: String,
}

impl Query {
    /// Parse `text`, or say which token the subset does not accept.
    pub(in crate::toolkits) fn parse(text: &str) -> Result<Self, String> {
        if text.chars().count() > MAX_QUERY_CHARS {
            return Err(format!(
                "query is longer than {MAX_QUERY_CHARS} characters."
            ));
        }
        let tokens = lex(text)?;
        let mut parser = Parser {
            tokens,
            at: 0,
            nodes: 0,
            columns: Vec::new(),
        };
        let root = parser.expression(0)?;
        if let Some(extra) = parser.tokens.get(parser.at) {
            return Err(unexpected(extra, "expected the end of the query"));
        }
        if !is_condition(&root) {
            return Err(
                "query must be a condition (a comparison, in, is null, or and/or/not of those), not a value."
                    .to_owned(),
            );
        }
        Ok(Self {
            root,
            columns: parser.columns,
        })
    }

    /// The columns the query reads.
    pub(in crate::toolkits) fn columns(&self) -> impl Iterator<Item = &str> {
        self.columns.iter().map(String::as_str)
    }

    /// The rows of `batch` the query holds for.
    pub(in crate::toolkits) fn matches(&self, batch: &RecordBatch) -> Result<Vec<bool>, String> {
        let schema = batch.schema();
        let mut readers = Vec::with_capacity(self.columns.len());
        for column in &self.columns {
            let index = schema
                .index_of(column)
                .map_err(|_| format!("Column '{column}' not found in table."))?;
            readers.push(ColumnReader::new(batch.column(index)));
        }
        Ok((0..batch.num_rows())
            .map(|row| truthy(&evaluate(&self.root, &readers, row)))
            .collect())
    }
}

fn is_condition(node: &Node) -> bool {
    match node {
        Node::Literal(Value::Bool(_))
        | Node::Column(_)
        | Node::Compare(..)
        | Node::IsNull(..)
        | Node::Not(_) => true,
        Node::And(items) | Node::Or(items) => items.iter().all(is_condition),
        _ => false,
    }
}

// ---- lexing ---------------------------------------------------------------

#[allow(
    clippy::too_many_lines,
    reason = "one scanner over the token classes; each arm is a short, separate case"
)]
fn lex(text: &str) -> Result<Vec<Lexed>, String> {
    let chars: Vec<(usize, char)> = text.char_indices().collect();
    let mut tokens: Vec<Lexed> = Vec::new();
    let mut at = 0;
    while at < chars.len() {
        let (position, c) = chars[at];
        if tokens.len() >= MAX_TOKENS {
            return Err(format!("query has more than {MAX_TOKENS} tokens."));
        }
        if c.is_whitespace() {
            at += 1;
            continue;
        }
        let rest = &text[position..];
        let two = |symbol: &'static str| rest.starts_with(symbol).then_some(symbol);
        if c.is_ascii_digit() {
            let mut end = at;
            let mut seen_exponent = false;
            while end < chars.len() {
                let d = chars[end].1;
                let exponent_sign = seen_exponent
                    && matches!(d, '+' | '-')
                    && matches!(chars[end - 1].1, 'e' | 'E');
                if d.is_ascii_digit() || d == '.' || exponent_sign {
                    end += 1;
                } else if matches!(d, 'e' | 'E') && !seen_exponent && end > at {
                    seen_exponent = true;
                    end += 1;
                } else {
                    break;
                }
            }
            let shown = chars_to_string(&chars, at, end, text);
            tokens.push(Lexed {
                token: Token::Number(shown.clone()),
                position,
                shown,
            });
            at = end;
        } else if c.is_alphabetic() || c == '_' {
            let mut end = at;
            while end < chars.len() && (chars[end].1.is_alphanumeric() || chars[end].1 == '_') {
                end += 1;
            }
            let shown = chars_to_string(&chars, at, end, text);
            tokens.push(Lexed {
                token: Token::Word(shown.clone()),
                position,
                shown,
            });
            at = end;
        } else if c == '\'' || c == '"' {
            let (value, end) = lex_string(&chars, at, position)?;
            tokens.push(Lexed {
                token: Token::Text(value),
                position,
                shown: chars_to_string(&chars, at, end, text),
            });
            at = end;
        } else if c == '`' {
            let mut end = at + 1;
            while end < chars.len() && chars[end].1 != '`' {
                end += 1;
            }
            if end >= chars.len() {
                return Err(format!(
                    "Unterminated backtick column name at position {position} in query."
                ));
            }
            let name = chars_to_string(&chars, at + 1, end, text);
            if name.is_empty() || name.len() > 512 {
                return Err(format!(
                    "Invalid backtick column name at position {position} in query."
                ));
            }
            tokens.push(Lexed {
                token: Token::Quoted(name),
                position,
                shown: chars_to_string(&chars, at, end + 1, text),
            });
            at = end + 1;
        } else {
            let (symbol, length): (&'static str, usize) = if let Some(s) = two("==") {
                (s, 2)
            } else if let Some(s) = two("!=") {
                (s, 2)
            } else if two("<>").is_some() {
                ("!=", 2)
            } else if let Some(s) = two("<=") {
                (s, 2)
            } else if let Some(s) = two(">=") {
                (s, 2)
            } else if rest.starts_with("**") || rest.starts_with("//") {
                return Err(format!(
                    "Unsupported token '{}' at position {position} in query.",
                    &rest[..2]
                ));
            } else {
                let symbol = match c {
                    '<' => "<",
                    '>' => ">",
                    '+' => "+",
                    '-' => "-",
                    '*' => "*",
                    '/' => "/",
                    '%' => "%",
                    '(' => "(",
                    ')' => ")",
                    '[' => "[",
                    ']' => "]",
                    ',' => ",",
                    '&' => "&",
                    '|' => "|",
                    '~' => "~",
                    '.' => {
                        return Err(format!(
                            "Unsupported token '.' at position {position} in query: attribute access and method calls (such as .str.contains()) are not supported."
                        ));
                    }
                    '@' => {
                        return Err(format!(
                            "Unsupported token '@' at position {position} in query: variables are not supported."
                        ));
                    }
                    '=' => {
                        return Err(format!(
                            "Unsupported token '=' at position {position} in query: use '==' to compare."
                        ));
                    }
                    other => {
                        return Err(format!(
                            "Unsupported token '{other}' at position {position} in query."
                        ));
                    }
                };
                (symbol, 1)
            };
            tokens.push(Lexed {
                token: Token::Symbol(symbol),
                position,
                shown: rest.chars().take(length).collect(),
            });
            at += length;
        }
    }
    Ok(tokens)
}

fn chars_to_string(chars: &[(usize, char)], from: usize, to: usize, text: &str) -> String {
    let start = chars.get(from).map_or(text.len(), |entry| entry.0);
    let end = chars.get(to).map_or(text.len(), |entry| entry.0);
    text[start..end].to_owned()
}

fn lex_string(
    chars: &[(usize, char)],
    start: usize,
    position: usize,
) -> Result<(String, usize), String> {
    let quote = chars[start].1;
    let mut value = String::new();
    let mut at = start + 1;
    while at < chars.len() {
        let c = chars[at].1;
        if c == quote {
            return Ok((value, at + 1));
        }
        if c == '\\' {
            at += 1;
            let escaped = chars
                .get(at)
                .map(|entry| entry.1)
                .ok_or_else(|| format!("Unterminated string at position {position} in query."))?;
            value.push(match escaped {
                'n' => '\n',
                't' => '\t',
                other => other,
            });
        } else {
            value.push(c);
        }
        if value.len() > MAX_TEXT_BYTES {
            return Err(format!(
                "A string at position {position} in query is longer than {MAX_TEXT_BYTES} bytes."
            ));
        }
        at += 1;
    }
    Err(format!(
        "Unterminated string at position {position} in query."
    ))
}

// ---- parsing --------------------------------------------------------------

struct Parser {
    tokens: Vec<Lexed>,
    at: usize,
    nodes: usize,
    columns: Vec<String>,
}

fn unexpected(token: &Lexed, why: &str) -> String {
    format!(
        "Unsupported token '{}' at position {} in query: {why}.",
        token.shown, token.position
    )
}

fn keyword(token: &Lexed, word: &str) -> bool {
    matches!(&token.token, Token::Word(text) if text.eq_ignore_ascii_case(word))
}

impl Parser {
    fn peek(&self) -> Option<&Lexed> {
        self.tokens.get(self.at)
    }

    fn symbol(&self, symbol: &str) -> bool {
        matches!(self.peek(), Some(Lexed { token: Token::Symbol(s), .. }) if *s == symbol)
    }

    fn word(&self, word: &str) -> bool {
        self.peek().is_some_and(|token| keyword(token, word))
    }

    fn node(&mut self, node: Node) -> Result<Node, String> {
        self.nodes += 1;
        if self.nodes > MAX_NODES {
            return Err(format!("query has more than {MAX_NODES} terms."));
        }
        Ok(node)
    }

    fn expect_symbol(&mut self, symbol: &str) -> Result<(), String> {
        if self.symbol(symbol) {
            self.at += 1;
            return Ok(());
        }
        Err(self.peek().map_or_else(
            || format!("Unexpected end of query: expected '{symbol}'."),
            |token| unexpected(token, &format!("expected '{symbol}'")),
        ))
    }

    /// `or` level.
    fn expression(&mut self, depth: usize) -> Result<Node, String> {
        if depth > MAX_DEPTH {
            return Err(format!("query is nested deeper than {MAX_DEPTH} levels."));
        }
        let mut items = vec![self.conjunction(depth)?];
        while self.word("or") || self.symbol("|") {
            self.at += 1;
            items.push(self.conjunction(depth)?);
        }
        if items.len() == 1 {
            Ok(items.remove(0))
        } else {
            self.node(Node::Or(items))
        }
    }

    fn conjunction(&mut self, depth: usize) -> Result<Node, String> {
        let mut items = vec![self.negation(depth)?];
        while self.word("and") || self.symbol("&") {
            self.at += 1;
            items.push(self.negation(depth)?);
        }
        if items.len() == 1 {
            Ok(items.remove(0))
        } else {
            self.node(Node::And(items))
        }
    }

    fn negation(&mut self, depth: usize) -> Result<Node, String> {
        if depth > MAX_DEPTH {
            return Err(format!("query is nested deeper than {MAX_DEPTH} levels."));
        }
        if self.word("not") || self.symbol("~") {
            self.at += 1;
            let inner = self.negation(depth + 1)?;
            return self.node(Node::Not(Box::new(inner)));
        }
        self.comparison(depth)
    }

    fn comparison(&mut self, depth: usize) -> Result<Node, String> {
        let first = self.sum(depth)?;
        let mut operands = vec![first];
        let mut operators = Vec::new();
        loop {
            // `is [not] null`
            if self.word("is") {
                self.at += 1;
                let negated = if self.word("not") {
                    self.at += 1;
                    true
                } else {
                    false
                };
                let is_null = self
                    .peek()
                    .is_some_and(|token| keyword(token, "null") || keyword(token, "none"));
                if !is_null {
                    return Err(self.peek().map_or_else(
                        || "Unexpected end of query: expected null after 'is'.".to_owned(),
                        |token| unexpected(token, "expected null or None after 'is'"),
                    ));
                }
                self.at += 1;
                if operands.len() != 1 {
                    return Err("'is null' cannot follow a chained comparison.".to_owned());
                }
                let operand = operands.remove(0);
                return self.node(Node::IsNull(Box::new(operand), negated));
            }
            let operator = match self.peek().map(|token| &token.token) {
                Some(Token::Symbol("==")) => Cmp::Eq,
                Some(Token::Symbol("!=")) => Cmp::Ne,
                Some(Token::Symbol("<")) => Cmp::Lt,
                Some(Token::Symbol("<=")) => Cmp::Le,
                Some(Token::Symbol(">")) => Cmp::Gt,
                Some(Token::Symbol(">=")) => Cmp::Ge,
                Some(Token::Word(word)) if word.eq_ignore_ascii_case("in") => Cmp::In,
                Some(Token::Word(word)) if word.eq_ignore_ascii_case("not") => {
                    // `not in`; a bare `not` here is a syntax error.
                    let next = self.tokens.get(self.at + 1);
                    if next.is_some_and(|token| keyword(token, "in")) {
                        self.at += 1;
                        Cmp::NotIn
                    } else {
                        break;
                    }
                }
                _ => break,
            };
            self.at += 1;
            if matches!(operator, Cmp::In | Cmp::NotIn) {
                let list = self.list_literal()?;
                operands.push(list);
            } else {
                operands.push(self.sum(depth)?);
            }
            operators.push(operator);
        }
        if operators.is_empty() {
            Ok(operands.remove(0))
        } else {
            self.node(Node::Compare(operands, operators))
        }
    }

    fn list_literal(&mut self) -> Result<Node, String> {
        let close = if self.symbol("[") {
            "]"
        } else if self.symbol("(") {
            ")"
        } else {
            return Err(self.peek().map_or_else(
                || "Unexpected end of query: expected a list after 'in'.".to_owned(),
                |token| unexpected(token, "'in' needs a list of literals such as [1, 2]"),
            ));
        };
        self.at += 1;
        let mut items = Vec::new();
        while !self.symbol(close) {
            if items.len() >= MAX_LIST_ITEMS {
                return Err(format!("a list has more than {MAX_LIST_ITEMS} items."));
            }
            items.push(self.literal_value()?);
            if self.symbol(",") {
                self.at += 1;
            } else if !self.symbol(close) {
                return Err(self.peek().map_or_else(
                    || format!("Unexpected end of query: expected '{close}'."),
                    |token| unexpected(token, &format!("expected ',' or '{close}'")),
                ));
            }
        }
        self.at += 1;
        self.node(Node::Literal(Value::List(items)))
    }

    fn literal_value(&mut self) -> Result<Value, String> {
        let negative = self.symbol("-");
        if negative || self.symbol("+") {
            self.at += 1;
        }
        let token = self
            .peek()
            .ok_or_else(|| "Unexpected end of query: expected a literal.".to_owned())?;
        let value = match &token.token {
            Token::Number(text) => number(text, token)?,
            Token::Text(text) if !negative => Value::Text(text.clone()),
            Token::Word(word) if !negative => match word.as_str() {
                "True" | "true" => Value::Bool(true),
                "False" | "false" => Value::Bool(false),
                "None" | "null" | "NULL" => Value::Null,
                _ => return Err(unexpected(token, "lists hold literals only")),
            },
            _ => return Err(unexpected(token, "lists hold literals only")),
        };
        self.at += 1;
        Ok(if negative { negate(value) } else { value })
    }

    fn sum(&mut self, depth: usize) -> Result<Node, String> {
        let mut left = self.product(depth)?;
        loop {
            let operator = if self.symbol("+") {
                Arith::Add
            } else if self.symbol("-") {
                Arith::Sub
            } else {
                return Ok(left);
            };
            self.at += 1;
            let right = self.product(depth)?;
            left = self.node(Node::Arith(Box::new(left), operator, Box::new(right)))?;
        }
    }

    fn product(&mut self, depth: usize) -> Result<Node, String> {
        let mut left = self.unary(depth)?;
        loop {
            let operator = if self.symbol("*") {
                Arith::Mul
            } else if self.symbol("/") {
                Arith::Div
            } else if self.symbol("%") {
                Arith::Rem
            } else {
                return Ok(left);
            };
            self.at += 1;
            let right = self.unary(depth)?;
            left = self.node(Node::Arith(Box::new(left), operator, Box::new(right)))?;
        }
    }

    fn unary(&mut self, depth: usize) -> Result<Node, String> {
        if depth > MAX_DEPTH {
            return Err(format!("query is nested deeper than {MAX_DEPTH} levels."));
        }
        if self.symbol("-") {
            self.at += 1;
            let inner = self.unary(depth + 1)?;
            return self.node(Node::Negate(Box::new(inner)));
        }
        if self.symbol("+") {
            self.at += 1;
            return self.unary(depth + 1);
        }
        self.atom(depth)
    }

    fn atom(&mut self, depth: usize) -> Result<Node, String> {
        let Some(token) = self.peek() else {
            return Err("Unexpected end of query.".to_owned());
        };
        let (kind, shown, position) = (token.token.clone(), token.shown.clone(), token.position);
        match kind {
            Token::Number(text) => {
                let value = number(&text, &self.tokens[self.at])?;
                self.at += 1;
                self.node(Node::Literal(value))
            }
            Token::Text(text) => {
                self.at += 1;
                self.node(Node::Literal(Value::Text(text)))
            }
            Token::Quoted(name) => {
                self.at += 1;
                let index = self.column(&name);
                self.node(Node::Column(index))
            }
            Token::Word(word) => {
                self.at += 1;
                if self.symbol("(") {
                    return Err(format!(
                        "Unsupported token '{shown}(' at position {position} in query: function calls are not supported."
                    ));
                }
                match word.as_str() {
                    "True" | "true" => self.node(Node::Literal(Value::Bool(true))),
                    "False" | "false" => self.node(Node::Literal(Value::Bool(false))),
                    "None" => self.node(Node::Literal(Value::Null)),
                    "and" | "or" | "not" | "in" | "is" | "AND" | "OR" | "NOT" | "IN" | "IS" => {
                        Err(format!(
                            "Unsupported token '{shown}' at position {position} in query: unexpected operator."
                        ))
                    }
                    "like" | "LIKE" | "ilike" | "ILIKE" | "between" | "BETWEEN" | "select"
                    | "SELECT" => Err(format!(
                        "Unsupported token '{shown}' at position {position} in query: only comparisons, in, is null and and/or/not are supported."
                    )),
                    _ => {
                        let index = self.column(&word);
                        self.node(Node::Column(index))
                    }
                }
            }
            Token::Symbol("(") => {
                self.at += 1;
                let inner = self.expression(depth + 1)?;
                self.expect_symbol(")")?;
                Ok(inner)
            }
            Token::Symbol(_) => Err(unexpected(
                &self.tokens[self.at],
                "expected a column, a literal or '('",
            )),
        }
    }

    fn column(&mut self, name: &str) -> usize {
        if let Some(index) = self.columns.iter().position(|column| column == name) {
            return index;
        }
        self.columns.push(name.to_owned());
        self.columns.len() - 1
    }
}

fn number(text: &str, token: &Lexed) -> Result<Value, String> {
    if !text.contains(['.', 'e', 'E']) {
        return text
            .parse::<i128>()
            .map(Value::Int)
            .map_err(|_| unexpected(token, "the number is too large"));
    }
    text.parse::<f64>()
        .ok()
        .filter(|value| value.is_finite())
        .map(Value::Float)
        .ok_or_else(|| unexpected(token, "not a number"))
}

fn negate(value: Value) -> Value {
    match value {
        Value::Int(number) => Value::Int(-number),
        Value::Float(number) => Value::Float(-number),
        other => other,
    }
}

// ---- evaluation -----------------------------------------------------------

fn truthy(value: &Value) -> bool {
    matches!(value, Value::Bool(true))
}

fn evaluate(node: &Node, readers: &[ColumnReader<'_>], row: usize) -> Value {
    match node {
        Node::Literal(value) => value.clone(),
        Node::Column(index) => match readers[*index].cell(row) {
            Cell::Null => Value::Null,
            Cell::Bool(value) => Value::Bool(value),
            Cell::Int(value) => Value::Int(value),
            Cell::Float(value) => Value::Float(value),
            Cell::Text(value) => Value::Text(value),
        },
        Node::Negate(inner) => negate(evaluate(inner, readers, row)),
        Node::Arith(left, operator, right) => arithmetic(
            &evaluate(left, readers, row),
            *operator,
            &evaluate(right, readers, row),
        ),
        Node::IsNull(inner, negated) => {
            Value::Bool(matches!(evaluate(inner, readers, row), Value::Null) != *negated)
        }
        Node::Not(inner) => Value::Bool(!truthy(&evaluate(inner, readers, row))),
        Node::And(items) => Value::Bool(
            items
                .iter()
                .all(|item| truthy(&evaluate(item, readers, row))),
        ),
        Node::Or(items) => Value::Bool(
            items
                .iter()
                .any(|item| truthy(&evaluate(item, readers, row))),
        ),
        Node::Compare(operands, operators) => {
            let mut left = evaluate(&operands[0], readers, row);
            for (operator, operand) in operators.iter().zip(&operands[1..]) {
                let right = evaluate(operand, readers, row);
                if !compare(&left, *operator, &right) {
                    return Value::Bool(false);
                }
                left = right;
            }
            Value::Bool(true)
        }
    }
}

fn order(left: &Value, right: &Value) -> Option<Ordering> {
    match (left, right) {
        (Value::Int(left), Value::Int(right)) => Some(left.cmp(right)),
        (Value::Int(left), Value::Float(right)) => int_float(*left, *right),
        (Value::Float(left), Value::Int(right)) => int_float(*right, *left).map(Ordering::reverse),
        (Value::Float(left), Value::Float(right)) => left.partial_cmp(right),
        (Value::Text(left), Value::Text(right)) => Some(left.cmp(right)),
        (Value::Bool(left), Value::Bool(right)) => Some(left.cmp(right)),
        _ => None,
    }
}

#[allow(
    clippy::cast_precision_loss,
    reason = "an integer compared with a float is compared as a float, as pandas does"
)]
fn int_float(left: i128, right: f64) -> Option<Ordering> {
    (left as f64).partial_cmp(&right)
}

/// `a OP b` for one link of a (possibly chained) comparison. Pandas answers
/// `False` for an ordering of values of different kinds or with a missing
/// value, `False` for `==` and `True` for `!=`.
fn compare(left: &Value, operator: Cmp, right: &Value) -> bool {
    match operator {
        Cmp::Eq => order(left, right) == Some(Ordering::Equal),
        Cmp::Ne => order(left, right) != Some(Ordering::Equal),
        Cmp::Lt => order(left, right) == Some(Ordering::Less),
        Cmp::Le => matches!(order(left, right), Some(Ordering::Less | Ordering::Equal)),
        Cmp::Gt => order(left, right) == Some(Ordering::Greater),
        Cmp::Ge => matches!(
            order(left, right),
            Some(Ordering::Greater | Ordering::Equal)
        ),
        Cmp::In | Cmp::NotIn => {
            let Value::List(items) = right else {
                return false;
            };
            let found = items
                .iter()
                .any(|item| order(left, item) == Some(Ordering::Equal));
            found == (operator == Cmp::In)
        }
    }
}

#[allow(
    clippy::cast_precision_loss,
    reason = "mixed integer and float arithmetic is done in floats, as pandas does"
)]
fn arithmetic(left: &Value, operator: Arith, right: &Value) -> Value {
    match (left, right) {
        (Value::Int(a), Value::Int(b)) => match operator {
            Arith::Add => a.checked_add(*b).map_or(Value::Null, Value::Int),
            Arith::Sub => a.checked_sub(*b).map_or(Value::Null, Value::Int),
            Arith::Mul => a.checked_mul(*b).map_or(Value::Null, Value::Int),
            Arith::Div => float_result(*a as f64, operator, *b as f64),
            Arith::Rem => a.checked_rem_euclid(*b).map_or(Value::Null, Value::Int),
        },
        (Value::Int(a), Value::Float(b)) => float_result(*a as f64, operator, *b),
        (Value::Float(a), Value::Int(b)) => float_result(*a, operator, *b as f64),
        (Value::Float(a), Value::Float(b)) => float_result(*a, operator, *b),
        (Value::Text(a), Value::Text(b)) if operator == Arith::Add => {
            if a.len() + b.len() > MAX_TEXT_BYTES {
                Value::Null
            } else {
                Value::Text(format!("{a}{b}"))
            }
        }
        _ => Value::Null,
    }
}

fn float_result(left: f64, operator: Arith, right: f64) -> Value {
    let value = match operator {
        Arith::Add => left + right,
        Arith::Sub => left - right,
        Arith::Mul => left * right,
        Arith::Div => left / right,
        Arith::Rem => left.rem_euclid(right),
    };
    if value.is_finite() {
        Value::Float(value)
    } else {
        Value::Null
    }
}

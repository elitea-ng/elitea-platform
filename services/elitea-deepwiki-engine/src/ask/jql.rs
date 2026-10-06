//! The JQL parser (`code_graph/jql_parser.py`, SPEC-2): `field:value`
//! clauses for `query_graph`.
//!
//! A clause is `field:value`, `field:"quoted value"` or `field:>N`
//! (`>`, `>=`, `<`, `<=`; `!` and `!=` parse and compare as equality, as
//! in Python). `AND` and `OR` are read and ignored for evaluation (Python
//! records `is_or` and no caller reads it). Unknown fields and tokens are
//! skipped. `limit` is clamped to 1..=500.

use regex::Regex;
use std::sync::LazyLock;

/// Clause comparison (`ClauseOp`).
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Op {
    Eq,
    Gt,
    Gte,
    Lt,
    Lte,
    Match,
}

/// One clause.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Clause {
    pub field: String,
    pub op: Op,
    pub value: String,
}

impl Clause {
    /// `matches_numeric`: an unparseable threshold matches nothing.
    #[must_use]
    pub fn matches_numeric(&self, candidate: i64) -> bool {
        let Some(threshold) = parse_int(&self.value) else {
            return false;
        };
        let candidate = i128::from(candidate);
        match self.op {
            Op::Gt => candidate > threshold,
            Op::Gte => candidate >= threshold,
            Op::Lt => candidate < threshold,
            Op::Lte => candidate <= threshold,
            Op::Eq => candidate == threshold,
            Op::Match => false,
        }
    }
}

/// A parsed query (`JQLQuery`).
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Query {
    pub index_clauses: Vec<Clause>,
    pub post_filters: Vec<Clause>,
    pub limit: usize,
    pub is_or: bool,
}

impl Default for Query {
    fn default() -> Self {
        Self {
            index_clauses: Vec::new(),
            post_filters: Vec::new(),
            limit: 20,
            is_or: false,
        }
    }
}

const INDEX_FIELDS: &[&str] = &["type", "layer", "file", "name", "text"];
const POST_FILTER_FIELDS: &[&str] = &["related", "dir", "has_rel", "connections"];

impl Query {
    fn first<'a>(clauses: &'a [Clause], field: &str) -> Option<&'a Clause> {
        clauses.iter().find(|clause| clause.field == field)
    }

    #[must_use]
    pub fn text_value(&self) -> Option<&str> {
        Self::first(&self.index_clauses, "text").map(|c| c.value.as_str())
    }

    /// Every `type:` value, lower-cased.
    #[must_use]
    pub fn type_values(&self) -> Vec<String> {
        self.index_clauses
            .iter()
            .filter(|c| c.field == "type")
            .map(|c| c.value.to_lowercase())
            .collect()
    }

    #[must_use]
    pub fn layer_value(&self) -> Option<String> {
        Self::first(&self.index_clauses, "layer").map(|c| c.value.to_lowercase())
    }

    #[must_use]
    pub fn file_value(&self) -> Option<&str> {
        Self::first(&self.index_clauses, "file").map(|c| c.value.as_str())
    }

    #[must_use]
    pub fn name_value(&self) -> Option<&str> {
        Self::first(&self.index_clauses, "name").map(|c| c.value.as_str())
    }

    #[must_use]
    pub fn related_value(&self) -> Option<&str> {
        Self::first(&self.post_filters, "related").map(|c| c.value.as_str())
    }

    /// `dir:`, lower-cased, `both` by default.
    #[must_use]
    pub fn direction_value(&self) -> String {
        Self::first(&self.post_filters, "dir")
            .map_or_else(|| "both".to_owned(), |c| c.value.to_lowercase())
    }

    #[must_use]
    pub fn has_rel_values(&self) -> Vec<String> {
        self.post_filters
            .iter()
            .filter(|c| c.field == "has_rel")
            .map(|c| c.value.to_lowercase())
            .collect()
    }

    #[must_use]
    pub fn connections_clause(&self) -> Option<&Clause> {
        Self::first(&self.post_filters, "connections")
    }

    #[must_use]
    pub fn is_empty(&self) -> bool {
        self.index_clauses.is_empty() && self.post_filters.is_empty()
    }
}

static CLAUSE: LazyLock<Option<Regex>> = LazyLock::new(|| {
    Regex::new(r#"^(?P<field>[a-z_]+):(?:"(?P<quoted>[^"]*)"|(?P<comp>[><!]=?)?(?P<bare>[^\s"]+))"#)
        .ok()
});

/// Python `int(text)` for the values a clause carries: surrounding
/// whitespace, an optional sign, decimal digits with single `_` between
/// digits.
fn parse_int(text: &str) -> Option<i128> {
    let text = crate::graph::pystr::strip(text);
    let (negative, digits) = match text.as_bytes().first() {
        Some(b'-') => (true, &text[1..]),
        Some(b'+') => (false, &text[1..]),
        _ => (false, text),
    };
    if digits.is_empty()
        || digits.starts_with('_')
        || digits.ends_with('_')
        || digits.contains("__")
        || !digits.chars().all(|c| c.is_ascii_digit() || c == '_')
    {
        return None;
    }
    let clean: String = digits.chars().filter(|c| *c != '_').collect();
    // Beyond i128 the comparison is still decidable; saturate.
    let value = clean.parse::<i128>().unwrap_or(i128::MAX);
    Some(if negative { -value } else { value })
}

fn is_keyword(text: &str, word: &str) -> Option<usize> {
    let bytes = text.as_bytes();
    let n = word.len();
    if bytes.len() > n
        && text.is_char_boundary(n)
        && text[..n].eq_ignore_ascii_case(word)
        && matches!(bytes[n], b' ' | b'\t')
    {
        Some(n)
    } else {
        None
    }
}

fn tokenize(expression: &str) -> Vec<Token> {
    let mut tokens = Vec::new();
    let Some(clause) = CLAUSE.as_ref() else {
        return tokens;
    };
    let mut remaining = crate::graph::pystr::strip(expression);
    while !remaining.is_empty() {
        remaining = remaining.trim_start_matches(crate::graph::pystr::is_space);
        if remaining.is_empty() {
            break;
        }
        if let Some(n) = is_keyword(remaining, "AND") {
            tokens.push(Token::And);
            remaining = remaining[n..].trim_start_matches(crate::graph::pystr::is_space);
            continue;
        }
        if let Some(n) = is_keyword(remaining, "OR") {
            tokens.push(Token::Or);
            remaining = remaining[n..].trim_start_matches(crate::graph::pystr::is_space);
            continue;
        }
        if let Some(found) = clause.find(remaining) {
            tokens.push(Token::Clause(found.as_str().to_owned()));
            remaining = &remaining[found.end()..];
            continue;
        }
        match remaining.find(' ') {
            None => break,
            Some(at) => {
                remaining = remaining[at..].trim_start_matches(crate::graph::pystr::is_space);
            }
        }
    }
    tokens
}

enum Token {
    And,
    Or,
    Clause(String),
}

fn parse_clause(text: &str) -> Option<Clause> {
    let captures = CLAUSE.as_ref()?.captures(text)?;
    let field = captures.name("field")?.as_str().to_lowercase();
    let known = INDEX_FIELDS.contains(&field.as_str())
        || POST_FILTER_FIELDS.contains(&field.as_str())
        || field == "limit";
    if !known {
        return None;
    }
    let comp = captures.name("comp").map_or("", |m| m.as_str());
    let value = match captures.name("quoted") {
        Some(quoted) => quoted.as_str().to_owned(),
        None => captures
            .name("bare")
            .map_or_else(String::new, |m| m.as_str().to_owned()),
    };
    let op = match comp {
        ">" => Op::Gt,
        ">=" => Op::Gte,
        "<" => Op::Lt,
        "<=" => Op::Lte,
        _ if field == "text" => Op::Match,
        _ => Op::Eq,
    };
    Some(Clause { field, op, value })
}

/// `parse_jql`.
#[must_use]
pub fn parse(expression: &str) -> Query {
    let mut query = Query::default();
    if crate::graph::pystr::strip(expression).is_empty() {
        return query;
    }
    for token in tokenize(expression) {
        let text = match token {
            Token::Or => {
                query.is_or = true;
                continue;
            }
            Token::And => continue,
            Token::Clause(text) => text,
        };
        let Some(clause) = parse_clause(&text) else {
            continue;
        };
        if clause.field == "limit" {
            if let Some(limit) = parse_int(&clause.value) {
                query.limit = usize::try_from(limit.clamp(1, 500)).unwrap_or(20);
            }
            continue;
        }
        if INDEX_FIELDS.contains(&clause.field.as_str()) {
            query.index_clauses.push(clause);
        } else {
            query.post_filters.push(clause);
        }
    }
    query
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_python_doctests_hold() {
        let q = parse("type:class file:src/auth/*");
        assert_eq!(q.type_values(), vec!["class"]);
        assert_eq!(q.file_value(), Some("src/auth/*"));
        let q = parse(r#"related:"BaseHandler" dir:incoming has_rel:inherits"#);
        assert_eq!(q.related_value(), Some("BaseHandler"));
        assert_eq!(q.direction_value(), "incoming");
        assert_eq!(q.has_rel_values(), vec!["inherits"]);
        let q = parse("type:class connections:>10 limit:5");
        assert_eq!(q.connections_clause().map(|c| c.op), Some(Op::Gt));
        assert_eq!(q.limit, 5);
    }

    #[test]
    fn limits_unknown_fields_and_operators() {
        assert_eq!(parse("limit:100000").limit, 500);
        assert_eq!(parse("limit:0").limit, 1);
        assert_eq!(parse("limit:many").limit, 20);
        assert!(parse("bogus:x").is_empty());
        assert!(parse("").is_empty());
        let q = parse("type:class OR type:interface and text:auth");
        assert!(q.is_or);
        assert_eq!(q.type_values(), vec!["class", "interface"]);
        assert_eq!(q.text_value(), Some("auth"));
        assert_eq!(q.index_clauses[2].op, Op::Match);
        let c = parse("connections:>=3").connections_clause().cloned();
        assert!(
            c.as_ref()
                .is_some_and(|c| c.matches_numeric(3) && !c.matches_numeric(2))
        );
        let c = parse("connections:!5").connections_clause().cloned();
        assert!(c.as_ref().is_some_and(|c| c.matches_numeric(5)));
        let c = parse("connections:>x").connections_clause().cloned();
        assert!(c.as_ref().is_some_and(|c| !c.matches_numeric(5)));
        // Garbage is skipped up to the next space.
        assert_eq!(parse("??? type:enum").type_values(), vec!["enum"]);
        // An uppercase field is not a field.
        assert!(parse("TYPE:class").is_empty());
    }
}

//! The SQL / DDL subgraph (`sql_extractor.build_sql_graph`).
//!
//! A regex parser for the common DDL shapes: `CREATE SCHEMA`, `TABLE`
//! (columns and foreign keys), `VIEW` (`FROM` / `JOIN` tables), `INDEX`,
//! `TRIGGER`, `FUNCTION` / `PROCEDURE`. References resolve across all SQL
//! files after every file is read. On by default
//! (`FeatureFlags.sql_extraction`).
//!
//! Python works on `str` indices (code points); so does this module, so
//! that statement offsets, line numbers and the `[:2000]`-style truncations
//! agree on non-ASCII text. Python's `\s` also matches U+001C..U+001F,
//! which Rust's does not, so the patterns spell that class out.

use super::pystr;
use super::{Attributes, CodeGraph, EdgeData, EdgeKey, NodeData};
use regex::Regex;
use serde_json::{Map, Value};
use std::collections::HashMap;
use std::sync::LazyLock;

const LANGUAGE: &str = "sql";
const CREATED_BY: &str = "sql_extractor";
const ANALYSIS_LEVEL: &str = "code";

/// Python's `\s` (`str.isspace`).
const WS: &str = r"[\s\x1C-\x1F]";
/// `_IDENT`.
const IDENT: &str = r#"[\w".`\[\]]+"#;

fn compile(pattern: &str) -> Regex {
    Regex::new(pattern).unwrap_or_else(|error| unreachable!("constant pattern: {error}"))
}

static RE_SCHEMA: LazyLock<Regex> = LazyLock::new(|| {
    compile(&format!(
        r"(?i)^{WS}*CREATE{WS}+(?:OR{WS}+REPLACE{WS}+)?SCHEMA{WS}+(?:IF{WS}+NOT{WS}+EXISTS{WS}+)?(?:AUTHORIZATION{WS}+\w+{WS}+)?(?P<name>{IDENT})"
    ))
});
static RE_TABLE: LazyLock<Regex> = LazyLock::new(|| {
    compile(&format!(
        r"(?is)^{WS}*CREATE{WS}+(?:(?:GLOBAL|LOCAL){WS}+)?(?:(?:TEMP|TEMPORARY|UNLOGGED){WS}+)?TABLE{WS}+(?:IF{WS}+NOT{WS}+EXISTS{WS}+)?(?P<name>{IDENT}){WS}*\((?P<body>.*)\){WS}*[^)]*$"
    ))
});
static RE_VIEW: LazyLock<Regex> = LazyLock::new(|| {
    compile(&format!(
        r"(?is)^{WS}*CREATE{WS}+(?:OR{WS}+REPLACE{WS}+)?(?:MATERIALIZED{WS}+)?VIEW{WS}+(?:IF{WS}+NOT{WS}+EXISTS{WS}+)?(?P<name>{IDENT})\b.*?\bAS\b(?P<query>.*)$"
    ))
});
static RE_INDEX: LazyLock<Regex> = LazyLock::new(|| {
    compile(&format!(
        r"(?is)^{WS}*CREATE{WS}+(?:UNIQUE{WS}+)?INDEX{WS}+(?:CONCURRENTLY{WS}+)?(?:IF{WS}+NOT{WS}+EXISTS{WS}+)?(?P<name>{IDENT}){WS}+ON{WS}+(?:ONLY{WS}+)?(?P<table>{IDENT})"
    ))
});
static RE_TRIGGER: LazyLock<Regex> = LazyLock::new(|| {
    compile(&format!(
        r"(?is)^{WS}*CREATE{WS}+(?:OR{WS}+REPLACE{WS}+)?(?:CONSTRAINT{WS}+)?TRIGGER{WS}+(?P<name>{IDENT})\b.*?\bON{WS}+(?P<table>{IDENT})"
    ))
});
static RE_FUNCTION: LazyLock<Regex> = LazyLock::new(|| {
    compile(&format!(
        r"(?is)^{WS}*CREATE{WS}+(?:OR{WS}+REPLACE{WS}+)?(?:FUNCTION|PROCEDURE){WS}+(?P<name>{IDENT}){WS}*\("
    ))
});
static RE_REFERENCES: LazyLock<Regex> = LazyLock::new(|| {
    compile(&format!(
        r"(?i)\bREFERENCES{WS}+(?P<table>{IDENT}){WS}*(?:\({WS}*(?P<col>{IDENT}){WS}*\))?"
    ))
});
static RE_FK_CONSTRAINT: LazyLock<Regex> = LazyLock::new(|| {
    compile(&format!(
        r"(?i)\bFOREIGN{WS}+KEY{WS}*\({WS}*(?P<local>{IDENT}){WS}*\){WS}*REFERENCES{WS}+(?P<table>{IDENT}){WS}*(?:\({WS}*(?P<col>{IDENT}){WS}*\))?"
    ))
});
static RE_FROM_JOIN: LazyLock<Regex> =
    LazyLock::new(|| compile(&format!(r"(?i)\b(?:FROM|JOIN){WS}+(?P<table>{IDENT})")));
static RE_CALL: LazyLock<Regex> =
    LazyLock::new(|| compile(&format!(r"\b(?P<name>[A-Za-z_]\w*){WS}*\(")));

/// First words that open a table-level constraint, not a column.
const CONSTRAINT_LEADERS: &[&str] = &[
    "constraint",
    "primary",
    "foreign",
    "unique",
    "check",
    "key",
    "index",
    "exclude",
    "like",
    "period",
];

/// Counts of what was extracted (`builder.stats`).
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct SqlStats {
    pub sql_files: usize,
    pub tables: usize,
    pub views: usize,
    pub columns: usize,
    pub indexes: usize,
    pub functions: usize,
    pub triggers: usize,
    pub schemas: usize,
    pub defines_edges: usize,
    pub references_edges: usize,
    pub triggered_by_edges: usize,
    pub calls_edges: usize,
}

/// `_clean_ident`: strip one level of `"…"`, `` `…` `` or `[…]` quoting.
fn clean_ident(raw: &str) -> String {
    let raw = pystr::strip(raw);
    for (open, close) in [('"', '"'), ('`', '`'), ('[', ']')] {
        if raw.len() >= 2 && raw.starts_with(open) && raw.ends_with(close) {
            return raw[1..raw.len() - 1].to_owned();
        }
        if raw.len() == 1 && raw.starts_with(open) && open == close {
            // `'"'[1:-1]` is empty.
            return String::new();
        }
    }
    raw.to_owned()
}

/// `_split_qualified`: `(schema, bare)` of a possibly qualified name. An
/// empty schema counts as none, as Python's truthiness test does.
fn split_qualified(name: &str) -> (Option<String>, String) {
    let stripped = pystr::strip(name);
    let parts: Vec<String> = stripped
        .split('.')
        .filter(|p| !pystr::strip(p).is_empty())
        .map(clean_ident)
        .collect();
    match parts.as_slice() {
        [] => (None, stripped.to_owned()),
        [bare] => (None, bare.clone()),
        [.., schema, bare] => (Some(schema.clone()).filter(|s| !s.is_empty()), bare.clone()),
    }
}

/// `_blank_comments`: comments become spaces (newlines kept), so offsets
/// and line numbers do not move. Single-quoted strings are stepped over.
fn blank_comments(sql: &[char]) -> Vec<char> {
    let mut out = sql.to_vec();
    let n = sql.len();
    let mut i = 0;
    while i < n {
        let ch = sql[i];
        if ch == '-' && i + 1 < n && sql[i + 1] == '-' {
            let mut j = i;
            while j < n && sql[j] != '\n' {
                out[j] = ' ';
                j += 1;
            }
            i = j;
            continue;
        }
        if ch == '/' && i + 1 < n && sql[i + 1] == '*' {
            let mut j = i;
            while j < n && !(sql[j] == '*' && j + 1 < n && sql[j + 1] == '/') {
                if sql[j] != '\n' {
                    out[j] = ' ';
                }
                j += 1;
            }
            if j < n {
                out[j] = ' ';
                if j + 1 < n {
                    out[j + 1] = ' ';
                }
                j += 2;
            }
            i = j;
            continue;
        }
        if ch == '\'' {
            i += 1;
            while i < n {
                if sql[i] == '\'' && i + 1 < n && sql[i + 1] == '\'' {
                    i += 2;
                    continue;
                }
                if sql[i] == '\'' {
                    i += 1;
                    break;
                }
                i += 1;
            }
            continue;
        }
        i += 1;
    }
    out
}

/// Python's `\w` for one character.
fn is_word(c: char) -> bool {
    c.is_alphanumeric() || c == '_'
}

/// The dollar-quote tag at `sql[i]` (`\$(\w*)\$`), if any.
fn dollar_tag(sql: &[char], i: usize) -> Option<usize> {
    let mut j = i + 1;
    while j < sql.len() && is_word(sql[j]) {
        j += 1;
    }
    (j < sql.len() && sql[j] == '$').then_some(j + 1 - i)
}

fn find_chars(haystack: &[char], needle: &[char], from: usize) -> Option<usize> {
    if needle.is_empty() {
        return Some(from);
    }
    (from..=haystack.len().checked_sub(needle.len())?)
        .find(|&start| haystack[start..start + needle.len()] == *needle)
}

fn is_blank(chars: &[char]) -> bool {
    chars.iter().all(|&c| pystr::is_space(c))
}

/// `_split_statements`: `(start, end)` character ranges split on top-level
/// `;`, skipping quoted strings and `$tag$ … $tag$` bodies.
fn split_statements(sql: &[char]) -> Vec<(usize, usize)> {
    let mut statements = Vec::new();
    let n = sql.len();
    let (mut i, mut depth, mut start) = (0, 0_usize, 0);
    while i < n {
        let ch = sql[i];
        if ch == '\'' || ch == '"' {
            let quote = ch;
            i += 1;
            while i < n {
                if sql[i] == quote && quote == '\'' && i + 1 < n && sql[i + 1] == '\'' {
                    i += 2;
                    continue;
                }
                if sql[i] == quote {
                    i += 1;
                    break;
                }
                i += 1;
            }
            continue;
        }
        if ch == '$'
            && let Some(length) = dollar_tag(sql, i)
        {
            let tag = &sql[i..i + length];
            i = find_chars(sql, tag, i + length).map_or(n, |end| end + length);
            continue;
        }
        match ch {
            '(' => depth += 1,
            ')' => depth = depth.saturating_sub(1),
            ';' if depth == 0 => {
                if !is_blank(&sql[start..i]) {
                    statements.push((start, i));
                }
                start = i + 1;
            }
            _ => {}
        }
        i += 1;
    }
    if start < n && !is_blank(&sql[start..]) {
        statements.push((start, n));
    }
    statements
}

/// `_split_top_level_commas`.
fn split_top_level_commas(body: &str) -> Vec<String> {
    let mut parts = Vec::new();
    let mut depth = 0_usize;
    let mut current = String::new();
    for ch in body.chars() {
        match ch {
            '(' => {
                depth += 1;
                current.push(ch);
            }
            ')' => {
                depth = depth.saturating_sub(1);
                current.push(ch);
            }
            ',' if depth == 0 => parts.push(std::mem::take(&mut current)),
            _ => current.push(ch),
        }
    }
    if !current.is_empty() {
        parts.push(current);
    }
    parts
}

/// `re.split(r"\s+", part, 1)[0]` of a stripped part.
fn first_token(part: &str) -> &str {
    part.split(pystr::is_space).next().unwrap_or("")
}

/// What a deferred `_pending_fk` entry resolves to.
enum Pending {
    /// schema → table `defines` (the source is the table).
    Schema,
    /// table → index `defines` (the source is the index).
    Index,
    /// column → column (or table) `references`.
    ForeignKey(Option<String>),
}

/// `_SqlGraph`.
#[derive(Default)]
struct SqlGraph {
    graph: CodeGraph,
    tables: HashMap<String, String>,
    columns: HashMap<String, String>,
    functions: HashMap<String, String>,
    schemas: HashMap<String, String>,
    qualified_tables: HashMap<String, String>,
    pending_fk: Vec<(String, String, Pending)>,
    pending_view: Vec<(String, Vec<String>)>,
    pending_trigger: Vec<(String, String)>,
    pending_calls: Vec<(String, String)>,
    stats: SqlStats,
}

/// One statement's location.
struct Statement<'a> {
    rel_path: &'a str,
    file_path: &'a str,
    start_line: i64,
    end_line: i64,
}

impl SqlGraph {
    fn node_id(kind: &str, rel_path: &str, qualified: &str) -> String {
        format!("sql::{rel_path}::{kind}:{qualified}")
    }

    /// `_add_node`: the first definition of an id wins.
    fn add_node(
        &mut self,
        id: &str,
        name: &str,
        kind: &str,
        at: &Statement<'_>,
        source_text: &str,
    ) {
        if self.graph.has_node(id) {
            return;
        }
        self.graph.add_node(
            id,
            NodeData {
                symbol_name: name.to_owned(),
                symbol_type: kind.to_owned().into(),
                file_path: at.file_path.into(),
                rel_path: at.rel_path.into(),
                file_name: pystr::stem(at.rel_path).into(),
                language: LANGUAGE.into(),
                start_line: at.start_line,
                end_line: at.end_line,
                analysis_level: ANALYSIS_LEVEL.into(),
                source_text: source_text.to_owned(),
                parameters: "[]".to_owned(),
                ..NodeData::default()
            },
        );
    }

    /// `_add_edge`: keyed `sql::<rel_type>`, so a repeat replaces the edge.
    fn add_edge(&mut self, source: &str, target: &str, rel_type: &str, raw_score: f64) {
        let (Some(from), Some(to)) = (self.graph.node(source), self.graph.node(target)) else {
            return;
        };
        let mut annotations = Map::new();
        annotations.insert(
            "confidence".to_owned(),
            Value::String("EXTRACTED".to_owned()),
        );
        let data = EdgeData {
            rel_type: rel_type.to_owned().into(),
            edge_class: "structural".into(),
            analysis_level: ANALYSIS_LEVEL.into(),
            language: LANGUAGE.to_owned(),
            weight: 1.0,
            raw_similarity: Some(raw_score),
            source_file: from.rel_path.as_str().into(),
            target_file: to.rel_path.as_str().into(),
            created_by: CREATED_BY.into(),
            source_context: String::new(),
            target_context: String::new(),
            annotations: annotations.into(),
            extra: Attributes::new(),
        };
        self.graph.add_edge_keyed(
            source,
            target,
            EdgeKey::Named(format!("sql::{rel_type}")),
            data,
        );
    }

    /// Pass 1 for one file.
    fn add_file(&mut self, file_path: &str, rel_path: &str, text: &str) {
        self.stats.sql_files += 1;
        let original: Vec<char> = text.chars().collect();
        let cleaned = blank_comments(&original);
        let mut line = 1_i64;
        let mut counted = 0;
        for (start, end) in split_statements(&cleaned) {
            line += newlines(&original[counted..start]);
            counted = start;
            let statement: String = cleaned[start..end].iter().collect();
            let original_statement: String = original[start..end].iter().collect();
            let at = Statement {
                rel_path,
                file_path,
                start_line: line,
                end_line: line + newlines(&cleaned[start..end]),
            };
            self.classify(&statement, &original_statement, &at);
        }
    }

    fn classify(&mut self, statement: &str, original: &str, at: &Statement<'_>) {
        let trimmed = pystr::strip(original);
        if let Some(m) = RE_SCHEMA.captures(statement) {
            let (_, bare) = split_qualified(&m["name"]);
            let id = Self::node_id("schema", at.rel_path, &bare);
            self.add_node(
                &id,
                &bare,
                "sql_schema",
                at,
                pystr::prefix_chars(trimmed, 2000),
            );
            self.schemas.insert(bare.to_lowercase(), id);
            self.stats.schemas += 1;
        } else if let Some(m) = RE_TABLE.captures(statement) {
            self.handle_table(&m["name"], &m["body"], at, trimmed);
        } else if let Some(m) = RE_VIEW.captures(statement) {
            let (schema, bare) = split_qualified(&m["name"]);
            let qualified = schema.map_or_else(|| bare.clone(), |s| format!("{s}.{bare}"));
            let id = Self::node_id("view", at.rel_path, &qualified);
            self.add_node(
                &id,
                &bare,
                "sql_view",
                at,
                pystr::prefix_chars(trimmed, 2000),
            );
            self.qualified_tables
                .insert(qualified.to_lowercase(), id.clone());
            self.tables
                .entry(bare.to_lowercase())
                .or_insert_with(|| id.clone());
            self.stats.views += 1;
            let references = RE_FROM_JOIN
                .captures_iter(&m["query"])
                .map(|t| split_qualified(&t["table"]).1)
                .collect();
            self.pending_view.push((id, references));
        } else if let Some(m) = RE_INDEX.captures(statement) {
            let (_, bare) = split_qualified(&m["name"]);
            let (_, table) = split_qualified(&m["table"]);
            let id = Self::node_id("index", at.rel_path, &format!("{table}.{bare}"));
            self.add_node(
                &id,
                &bare,
                "sql_index",
                at,
                pystr::prefix_chars(trimmed, 1000),
            );
            self.stats.indexes += 1;
            self.pending_fk.push((id, table, Pending::Index));
        } else if let Some(m) = RE_TRIGGER.captures(statement) {
            let (_, bare) = split_qualified(&m["name"]);
            let (_, table) = split_qualified(&m["table"]);
            let id = Self::node_id("trigger", at.rel_path, &bare);
            self.add_node(
                &id,
                &bare,
                "sql_trigger",
                at,
                pystr::prefix_chars(trimmed, 1500),
            );
            self.stats.triggers += 1;
            self.pending_trigger.push((id, table));
        } else if let Some(m) = RE_FUNCTION.captures(statement) {
            let (_, bare) = split_qualified(&m["name"]);
            let id = Self::node_id("function", at.rel_path, &bare);
            self.add_node(
                &id,
                &bare,
                "sql_function",
                at,
                pystr::prefix_chars(trimmed, 4000),
            );
            self.functions.insert(bare.to_lowercase(), id.clone());
            self.stats.functions += 1;
            self.pending_calls.push((id, statement.to_owned()));
        }
    }

    /// `_handle_table`.
    fn handle_table(&mut self, name: &str, body: &str, at: &Statement<'_>, trimmed: &str) {
        let (schema, bare) = split_qualified(name);
        let qualified = schema
            .as_ref()
            .map_or_else(|| bare.clone(), |s| format!("{s}.{bare}"));
        let table_id = Self::node_id("table", at.rel_path, &qualified);
        self.add_node(
            &table_id,
            &bare,
            "sql_table",
            at,
            pystr::prefix_chars(trimmed, 4000),
        );
        self.qualified_tables
            .insert(qualified.to_lowercase(), table_id.clone());
        self.tables
            .entry(bare.to_lowercase())
            .or_insert_with(|| table_id.clone());
        self.stats.tables += 1;
        if let Some(schema) = schema {
            self.pending_fk
                .push((table_id.clone(), schema, Pending::Schema));
        }

        for raw_part in split_top_level_commas(body) {
            let part = pystr::strip(&raw_part);
            if part.is_empty() {
                continue;
            }
            let first = first_token(part).to_lowercase();
            let first = first.trim_matches(['"', '`', '[', ']']);
            if CONSTRAINT_LEADERS.contains(&first) {
                if let Some(fk) = RE_FK_CONSTRAINT.captures(part) {
                    let (_, local) = split_qualified(&fk["local"]);
                    let (_, ref_table) = split_qualified(&fk["table"]);
                    let ref_col = fk.name("col").map(|c| split_qualified(c.as_str()).1);
                    let column_id =
                        Self::node_id("column", at.rel_path, &format!("{bare}.{local}"));
                    self.pending_fk
                        .push((column_id, ref_table, Pending::ForeignKey(ref_col)));
                }
                continue;
            }
            let column = clean_ident(first_token(part));
            if column.is_empty() {
                continue;
            }
            let column_id = Self::node_id("column", at.rel_path, &format!("{bare}.{column}"));
            self.add_node(
                &column_id,
                &column,
                "sql_column",
                at,
                pystr::prefix_chars(part, 500),
            );
            self.columns
                .insert(format!("{bare}.{column}").to_lowercase(), column_id.clone());
            self.stats.columns += 1;
            self.add_edge(&table_id, &column_id, "defines", 1.0);
            self.stats.defines_edges += 1;
            if let Some(reference) = RE_REFERENCES.captures(part) {
                // The raw (possibly qualified) table, for the qualified lookup.
                let ref_table = reference["table"].to_owned();
                let ref_col = reference.name("col").map(|c| split_qualified(c.as_str()).1);
                self.pending_fk
                    .push((column_id, ref_table, Pending::ForeignKey(ref_col)));
            }
        }
    }

    /// Pass 2: `resolve`.
    fn resolve(&mut self) {
        for (source, ref_table, pending) in std::mem::take(&mut self.pending_fk) {
            let (_, bare_table) = split_qualified(&ref_table);
            let table_id = self
                .qualified_tables
                .get(&ref_table.to_lowercase())
                .or_else(|| self.tables.get(&bare_table.to_lowercase()))
                .cloned();
            match pending {
                Pending::Schema => {
                    if let Some(schema_id) = self.schemas.get(&bare_table.to_lowercase()).cloned() {
                        self.add_edge(&schema_id, &source, "defines", 1.0);
                        self.stats.defines_edges += 1;
                    }
                }
                Pending::Index => {
                    if let Some(table_id) = table_id {
                        self.add_edge(&table_id, &source, "defines", 1.0);
                        self.stats.defines_edges += 1;
                    }
                }
                Pending::ForeignKey(ref_col) => {
                    let column = ref_col.and_then(|col| {
                        self.columns
                            .get(&format!("{bare_table}.{col}").to_lowercase())
                            .cloned()
                    });
                    if let Some(target) = column.or(table_id) {
                        self.add_edge(&source, &target, "references", 0.95);
                        self.stats.references_edges += 1;
                    }
                }
            }
        }
        for (view, tables) in std::mem::take(&mut self.pending_view) {
            for table in tables {
                if let Some(table_id) = self.tables.get(&table.to_lowercase()).cloned()
                    && table_id != view
                {
                    self.add_edge(&view, &table_id, "references", 0.9);
                    self.stats.references_edges += 1;
                }
            }
        }
        for (trigger, table) in std::mem::take(&mut self.pending_trigger) {
            if let Some(table_id) = self.tables.get(&table.to_lowercase()).cloned() {
                self.add_edge(&trigger, &table_id, "triggered_by", 1.0);
                self.stats.triggered_by_edges += 1;
            }
        }
        for (function, body) in std::mem::take(&mut self.pending_calls) {
            let mut seen: Vec<String> = Vec::new();
            for call in RE_CALL.captures_iter(&body) {
                let callee = call["name"].to_lowercase();
                if let Some(target) = self.functions.get(&callee).cloned()
                    && target != function
                    && !seen.contains(&target)
                {
                    self.add_edge(&function, &target, "calls", 1.0);
                    self.stats.calls_edges += 1;
                    seen.push(target);
                }
            }
        }
    }
}

fn newlines(chars: &[char]) -> i64 {
    i64::try_from(chars.iter().filter(|&&c| c == '\n').count()).unwrap_or(i64::MAX)
}

/// `build_sql_graph`: the SQL subgraph of `file_paths` (absolute, sorted).
#[must_use]
pub fn build_sql_graph(file_paths: &[String], repo_root: &str) -> (CodeGraph, SqlStats) {
    let mut builder = SqlGraph::default();
    for file_path in file_paths {
        let Ok(bytes) = std::fs::read(file_path) else {
            continue;
        };
        let text = pystr::decode_text(&bytes, pystr::Errors::Replace);
        let rel_path = super::documents::relative_path(file_path, repo_root);
        let rel_path = if rel_path == file_path.as_str() {
            pystr::file_name(file_path)
        } else {
            rel_path
        };
        builder.add_file(file_path, rel_path, &text);
    }
    builder.resolve();
    (builder.graph, builder.stats)
}

#[cfg(test)]
mod tests {
    use super::*;

    fn graph_of(files: &[(&str, &str)]) -> (CodeGraph, SqlStats) {
        let mut builder = SqlGraph::default();
        for (rel, text) in files {
            builder.add_file(&format!("/r/{rel}"), rel, text);
        }
        builder.resolve();
        (builder.graph, builder.stats)
    }

    fn edges(graph: &CodeGraph) -> Vec<(String, String, String)> {
        graph
            .edges()
            .map(|e| {
                (
                    e.source.to_owned(),
                    e.target.to_owned(),
                    e.data.rel_type.to_string(),
                )
            })
            .collect()
    }

    #[test]
    fn tables_columns_and_foreign_keys() {
        let ddl = "-- users; not a statement\nCREATE SCHEMA app;\n\
            CREATE TABLE app.users (\n  id BIGINT PRIMARY KEY,\n  \"name\" TEXT\n);\n\
            CREATE TABLE posts (\n  id INT,\n  author_id INT REFERENCES users(id),\n  \
            editor INT,\n  CONSTRAINT fk_ed FOREIGN KEY (editor) REFERENCES app.users\n);\n\
            CREATE UNIQUE INDEX posts_author ON posts (author_id);";
        let (graph, stats) = graph_of(&[("db/schema.sql", ddl)]);
        assert_eq!(
            (stats.tables, stats.columns, stats.indexes, stats.schemas),
            (2, 5, 1, 1)
        );
        let users = graph.node("sql::db/schema.sql::table:app.users").unwrap();
        assert_eq!((users.start_line, users.end_line), (2, 6));
        assert_eq!(users.symbol_name, "users");
        assert_eq!(users.file_name, "schema");
        assert!(graph.has_node("sql::db/schema.sql::column:users.name"));
        assert_eq!(
            edges(&graph),
            [
                (
                    "sql::db/schema.sql::schema:app".into(),
                    "sql::db/schema.sql::table:app.users".into(),
                    "defines".into()
                ),
                (
                    "sql::db/schema.sql::table:app.users".into(),
                    "sql::db/schema.sql::column:users.id".into(),
                    "defines".into()
                ),
                (
                    "sql::db/schema.sql::table:app.users".into(),
                    "sql::db/schema.sql::column:users.name".into(),
                    "defines".into()
                ),
                (
                    "sql::db/schema.sql::table:posts".into(),
                    "sql::db/schema.sql::column:posts.id".into(),
                    "defines".into()
                ),
                (
                    "sql::db/schema.sql::table:posts".into(),
                    "sql::db/schema.sql::column:posts.author_id".into(),
                    "defines".into()
                ),
                (
                    "sql::db/schema.sql::table:posts".into(),
                    "sql::db/schema.sql::column:posts.editor".into(),
                    "defines".into()
                ),
                (
                    "sql::db/schema.sql::table:posts".into(),
                    "sql::db/schema.sql::index:posts.posts_author".into(),
                    "defines".into()
                ),
                (
                    "sql::db/schema.sql::column:posts.author_id".into(),
                    "sql::db/schema.sql::column:users.id".into(),
                    "references".into()
                ),
                (
                    "sql::db/schema.sql::column:posts.editor".into(),
                    "sql::db/schema.sql::table:app.users".into(),
                    "references".into()
                ),
            ]
        );
        let fk = graph
            .edges()
            .find(|e| e.data.rel_type == "references")
            .unwrap()
            .data;
        assert_eq!(fk.raw_similarity, Some(0.95));
        assert_eq!(fk.created_by, "sql_extractor");
        assert_eq!(fk.analysis_level, "code");
    }

    #[test]
    fn views_triggers_and_function_calls_across_files() {
        let a = "CREATE TABLE t (id int);\nCREATE FUNCTION f() RETURNS int AS $$ SELECT g(1); $$ LANGUAGE sql;";
        let b = "CREATE OR REPLACE VIEW v AS SELECT * FROM t JOIN t2 ON true;\n\
                 CREATE TRIGGER trg AFTER INSERT ON t FOR EACH ROW EXECUTE FUNCTION f();\n\
                 CREATE FUNCTION g(x int) RETURNS int AS 'select x';";
        let (graph, stats) = graph_of(&[("a.sql", a), ("b.sql", b)]);
        assert_eq!((stats.views, stats.triggers, stats.functions), (1, 1, 2));
        let found = edges(&graph);
        assert!(found.contains(&(
            "sql::b.sql::view:v".into(),
            "sql::a.sql::table:t".into(),
            "references".into()
        )));
        assert!(found.contains(&(
            "sql::b.sql::trigger:trg".into(),
            "sql::a.sql::table:t".into(),
            "triggered_by".into()
        )));
        assert!(found.contains(&(
            "sql::a.sql::function:f".into(),
            "sql::b.sql::function:g".into(),
            "calls".into()
        )));
        let f = graph.node("sql::a.sql::function:f").unwrap();
        assert_eq!((f.start_line, f.end_line), (1, 2));
    }

    #[test]
    fn statements_split_on_top_level_semicolons_only() {
        let text: Vec<char> = "a 'x;y' (b;c); $t$ d; $t$ e; /* ; */ f".chars().collect();
        let cleaned = blank_comments(&text);
        let parts: Vec<String> = split_statements(&cleaned)
            .into_iter()
            .map(|(s, e)| cleaned[s..e].iter().collect::<String>().trim().to_owned())
            .collect();
        assert_eq!(parts, ["a 'x;y' (b;c)", "$t$ d; $t$ e", "f"]);
    }

    #[test]
    fn identifiers_lose_quotes_and_schemas() {
        assert_eq!(
            split_qualified("\"app\".\"Users\""),
            (Some("app".into()), "Users".into())
        );
        assert_eq!(split_qualified("[dbo].t"), (Some("dbo".into()), "t".into()));
        assert_eq!(split_qualified("t"), (None, "t".into()));
        assert_eq!(split_qualified("."), (None, ".".into()));
        assert_eq!(clean_ident(" `x` "), "x");
    }
}

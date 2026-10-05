//! ORM model → SQL table links (`orm_linker.link_orm_models`).
//!
//! A class whose source looks like an ORM model (`SQLAlchemy` `Column(` /
//! `__tablename__` / `mapped_column(`, Django `models.*Field(`, JPA
//! `@Entity`) gets a `models_table` edge to the `sql_table` of the same
//! name, else a `models_view` edge to a `sql_view`. On by default
//! (`FeatureFlags.orm_linking`).
//!
//! Python quirk kept: the pass reads the node's own `source_text`
//! ATTRIBUTE. A rich-tier node keeps its source on the parser symbol, not
//! as an attribute, so today only basic-tier classes (Kotlin) can match;
//! rich Python and Java models never link. Fixing that would change the
//! graph, so it waits for its own decision.

use super::{CodeGraph, EdgeData, EdgeKey};
use regex::Regex;
use serde_json::{Map, Value, json};
use std::collections::HashMap;
use std::sync::LazyLock;

fn compile(pattern: &str) -> Regex {
    Regex::new(pattern).unwrap_or_else(|error| unreachable!("constant pattern: {error}"))
}

/// Python's `\s`.
const WS: &str = r"[\s\x1C-\x1F]";

static SQLALCHEMY_SIGNALS: LazyLock<[Regex; 3]> = LazyLock::new(|| {
    [
        compile(&format!(r"\bColumn{WS}*\(")),
        compile(r"\b__tablename__\b"),
        compile(&format!(r"\bmapped_column{WS}*\(")),
    ]
});
static DJANGO_SIGNAL: LazyLock<Regex> =
    LazyLock::new(|| compile(&format!(r"\bmodels\.\w*Field{WS}*\(")));
static HIBERNATE_SIGNAL: LazyLock<Regex> = LazyLock::new(|| compile(r"@Entity\b"));
static SQLALCHEMY_TABLENAME: LazyLock<Regex> = LazyLock::new(|| {
    compile(&format!(
        r#"__tablename__{WS}*={WS}*['"]([A-Za-z_][\w]*)['"]"#
    ))
});
static DJANGO_DB_TABLE: LazyLock<Regex> =
    LazyLock::new(|| compile(&format!(r#"db_table{WS}*={WS}*['"]([A-Za-z_][\w]*)['"]"#)));
static HIBERNATE_TABLE: LazyLock<Regex> = LazyLock::new(|| {
    compile(&format!(
        r#"@Table{WS}*\({WS}*(?:[^)]*\b)?name{WS}*={WS}*"([A-Za-z_][\w]*)""#
    ))
});

/// `_to_snake_case`: `_` before an upper-case letter that follows a
/// lower-case one, and before the last capital of a run that a lower-case
/// letter follows (`APIKey` → `api_key`). Python uses look-arounds, which
/// the `regex` crate has not, hence the loop.
#[must_use]
pub fn to_snake_case(name: &str) -> String {
    let chars: Vec<char> = name.chars().collect();
    let mut out = String::with_capacity(name.len() + 4);
    for (i, &c) in chars.iter().enumerate() {
        if i > 0 {
            let previous = chars[i - 1];
            let next = chars.get(i + 1).copied();
            let lower_upper = previous.is_ascii_lowercase() && c.is_ascii_uppercase();
            let run_end = previous.is_ascii_uppercase()
                && c.is_ascii_uppercase()
                && next.is_some_and(|n| n.is_ascii_lowercase());
            if lower_upper || run_end {
                out.push('_');
            }
        }
        out.push(c);
    }
    out.to_lowercase()
}

/// `_detect_orm`.
fn detect_orm(source_text: &str, language: &str) -> bool {
    if source_text.is_empty() {
        return false;
    }
    match language.to_lowercase().as_str() {
        "python" | "py" => {
            DJANGO_SIGNAL.is_match(source_text)
                || SQLALCHEMY_SIGNALS.iter().any(|s| s.is_match(source_text))
        }
        "java" | "kotlin" => HIBERNATE_SIGNAL.is_match(source_text),
        _ => false,
    }
}

fn first_group(pattern: &Regex, text: &str) -> Option<String> {
    pattern
        .captures(text)
        .and_then(|c| c.get(1))
        .map(|m| m.as_str().to_owned())
}

/// `_candidate_table_names`, best first, de-duplicated case-insensitively.
fn candidate_table_names(class_name: &str, source_text: &str, language: &str) -> Vec<String> {
    let mut names = Vec::new();
    match language.to_lowercase().as_str() {
        "python" | "py" => {
            names.extend(first_group(&SQLALCHEMY_TABLENAME, source_text));
            names.extend(first_group(&DJANGO_DB_TABLE, source_text));
            names.push(class_name.to_lowercase());
            names.push(to_snake_case(class_name));
        }
        "java" | "kotlin" => {
            names.extend(first_group(&HIBERNATE_TABLE, source_text));
            names.push(class_name.to_owned());
            names.push(class_name.to_lowercase());
            names.push(to_snake_case(class_name));
        }
        _ => {}
    }
    let mut seen = Vec::new();
    names.retain(|name| {
        let key = name.to_lowercase();
        if seen.contains(&key) {
            false
        } else {
            seen.push(key);
            true
        }
    });
    names
}

/// `lower(name) → node id` of the nodes of one symbol type; the first wins.
fn name_index(graph: &CodeGraph, symbol_type: &str) -> HashMap<String, String> {
    let mut index = HashMap::new();
    for (id, data) in graph.nodes() {
        if data.symbol_type.to_lowercase() == symbol_type && !data.symbol_name.is_empty() {
            index
                .entry(data.symbol_name.to_lowercase())
                .or_insert_with(|| id.to_owned());
        }
    }
    index
}

/// `link_orm_models`: the number of edges added.
pub fn link_orm_models(graph: &mut CodeGraph) -> usize {
    let tables = name_index(graph, "sql_table");
    let views = name_index(graph, "sql_view");
    if tables.is_empty() && views.is_empty() {
        return 0;
    }
    let mut links = Vec::new();
    for (id, data) in graph.nodes() {
        if data.symbol_type.to_lowercase() != "class"
            || !detect_orm(&data.source_text, &data.language)
            || data.symbol_name.is_empty()
        {
            continue;
        }
        let candidates =
            candidate_table_names(&data.symbol_name, &data.source_text, &data.language);
        let found = candidates
            .iter()
            .find_map(|c| {
                tables
                    .get(&c.to_lowercase())
                    .map(|t| (t, c, "models_table"))
            })
            .or_else(|| {
                candidates
                    .iter()
                    .find_map(|c| views.get(&c.to_lowercase()).map(|v| (v, c, "models_view")))
            });
        if let Some((target, matched, kind)) = found {
            links.push((
                id.to_owned(),
                target.clone(),
                matched.clone(),
                kind,
                data.language.clone(),
            ));
        }
    }
    let mut added = 0;
    for (source, target, matched, kind, language) in links {
        let key = EdgeKey::Named(kind.to_owned());
        if graph.has_edge(&source, &target, &key) {
            continue;
        }
        let mut annotations = Map::new();
        annotations.insert("confidence".to_owned(), json!("INFERRED"));
        annotations.insert("table_name".to_owned(), Value::String(matched));
        let mut extra = Map::new();
        extra.insert("type".to_owned(), json!(kind));
        extra.insert(
            "provenance".to_owned(),
            json!({"source": "orm_linker", "matcher": "orm_table_name"}),
        );
        graph.add_edge_keyed(
            &source,
            &target,
            key,
            EdgeData {
                rel_type: kind.to_owned().into(),
                edge_class: "cross_language".into(),
                weight: 0.85,
                language: language.to_string(),
                created_by: "orm_linker".into(),
                annotations: annotations.into(),
                extra: extra.into(),
                ..EdgeData::default()
            },
        );
        added += 1;
    }
    added
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::graph::NodeData;

    fn add(graph: &mut CodeGraph, id: &str, kind: &str, name: &str, language: &str, text: &str) {
        graph.add_node(
            id,
            NodeData {
                symbol_type: kind.to_owned().into(),
                symbol_name: name.to_owned(),
                language: language.into(),
                source_text: text.to_owned(),
                ..NodeData::default()
            },
        );
    }

    #[test]
    fn snake_case_treats_capital_runs_as_words() {
        assert_eq!(to_snake_case("BlogPost"), "blog_post");
        assert_eq!(to_snake_case("APIKey"), "api_key");
        assert_eq!(to_snake_case("HTTPSConnection"), "https_connection");
        assert_eq!(to_snake_case("user"), "user");
    }

    #[test]
    fn models_link_to_tables_then_views() {
        let mut graph = CodeGraph::new();
        add(
            &mut graph,
            "sql::s.sql::table:blog_post",
            "sql_table",
            "blog_post",
            "sql",
            "",
        );
        add(
            &mut graph,
            "sql::s.sql::view:stats",
            "sql_view",
            "stats",
            "sql",
            "",
        );
        add(
            &mut graph,
            "py::m::BlogPost",
            "class",
            "BlogPost",
            "python",
            "id = Column(Integer)",
        );
        add(
            &mut graph,
            "kt::m::Stat",
            "class",
            "Stat",
            "kotlin",
            "@Entity @Table(name = \"stats\") class Stat",
        );
        add(
            &mut graph,
            "py::m::Plain",
            "class",
            "Plain",
            "python",
            "x = 1",
        );
        assert_eq!(link_orm_models(&mut graph), 2);
        let edges: Vec<(&str, &str, &str)> = graph
            .edges()
            .map(|e| (e.source, e.target, &*e.data.rel_type))
            .collect();
        assert_eq!(
            edges,
            [
                (
                    "py::m::BlogPost",
                    "sql::s.sql::table:blog_post",
                    "models_table"
                ),
                ("kt::m::Stat", "sql::s.sql::view:stats", "models_view"),
            ]
        );
        let edge = graph.edges().next().unwrap().data;
        assert_eq!(edge.annotations["table_name"], json!("blog_post"));
        assert_eq!(edge.edge_class, "cross_language");
        // Running again adds nothing: the keyed edge exists.
        assert_eq!(link_orm_models(&mut graph), 0);
    }

    #[test]
    fn a_rich_node_without_a_source_text_attribute_never_links() {
        let mut graph = CodeGraph::new();
        add(
            &mut graph,
            "sql::s.sql::table:users",
            "sql_table",
            "users",
            "sql",
            "",
        );
        add(&mut graph, "py::m::Users", "class", "Users", "python", "");
        assert_eq!(link_orm_models(&mut graph), 0);
    }
}

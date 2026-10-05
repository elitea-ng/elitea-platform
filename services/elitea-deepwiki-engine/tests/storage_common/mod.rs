//! Shared by the PostgreSQL storage tests.
//!
//! The tests need PostgreSQL with pgvector. They SKIP, loudly, when
//! `DEEPWIKI_TEST_DSN` is unset, and FAIL when `DEEPWIKI_REQUIRE_POSTGRES`
//! is set as well (CI sets it): the Python suite's rule, because a parity
//! gate that silently passes without a database is the failure this whole
//! phase exists to avoid.
//!
//! ```text
//! podman run -d --name dwpg -e POSTGRES_USER=deepwiki -e POSTGRES_PASSWORD=deepwiki \
//!     -e POSTGRES_DB=deepwiki -p 15436:5432 docker.io/pgvector/pgvector:0.8.5-pg16
//! export DEEPWIKI_TEST_DSN=postgresql://deepwiki:deepwiki@127.0.0.1:15436/deepwiki
//! ```
//!
//! Each test gets its own database (`dwt_<name>`), dropped and created
//! afresh, so tests run in parallel and never depend on a previous run.

#![allow(dead_code)]

use elitea_deepwiki_engine::storage::rows::IndexNode;
use elitea_deepwiki_engine::storage::{connect_options, migrate};
use serde_json::Value;
use sqlx::postgres::{PgPool, PgPoolOptions};
use std::path::PathBuf;

pub const DSN_ENV: &str = "DEEPWIKI_TEST_DSN";
pub const REQUIRE_ENV: &str = "DEEPWIKI_REQUIRE_POSTGRES";

/// The DSN, or `None` to skip. Panics when the database is required.
pub fn dsn() -> Option<String> {
    if let Some(value) = std::env::var(DSN_ENV).ok().filter(|v| !v.trim().is_empty()) {
        return Some(value);
    }
    let message = format!(
        "{DSN_ENV} is not set — the PostgreSQL storage tests did NOT run. Start pgvector and export the DSN (see tests/storage_common/mod.rs)."
    );
    let required = std::env::var(REQUIRE_ENV)
        .is_ok_and(|v| matches!(v.trim().to_lowercase().as_str(), "1" | "true" | "yes"));
    assert!(
        !required,
        "{message} {REQUIRE_ENV} is set, so this is an error."
    );
    eprintln!("SKIPPED: {message}");
    None
}

/// A freshly created and migrated database for one test.
pub async fn fresh_database(name: &str) -> Option<PgPool> {
    let dsn = dsn()?;
    let database = format!("dwt_{name}");
    assert!(
        database
            .bytes()
            .all(|b| b.is_ascii_lowercase() || b.is_ascii_digit() || b == b'_'),
        "test database names are plain identifiers"
    );
    let options = connect_options(&dsn).expect("the test DSN parses");
    let admin = PgPoolOptions::new()
        .max_connections(1)
        .connect_with(options.clone())
        .await
        .expect("connect to the test server");
    // Identifiers cannot be bound; the name is checked above.
    sqlx::raw_sql(&format!(
        "DROP DATABASE IF EXISTS \"{database}\" WITH (FORCE)"
    ))
    .execute(&admin)
    .await
    .expect("drop the test database");
    sqlx::raw_sql(&format!("CREATE DATABASE \"{database}\""))
        .execute(&admin)
        .await
        .expect("create the test database");
    admin.close().await;
    let pool = PgPoolOptions::new()
        .max_connections(8)
        .connect_with(options.database(&database))
        .await
        .expect("connect to the test database");
    migrate::apply_all(&pool).await.expect("migrate");
    Some(pool)
}

/// The repository root.
pub fn repo_root() -> PathBuf {
    PathBuf::from(env!("CARGO_MANIFEST_DIR")).join("../..")
}

/// The P0 retrieval fixtures.
pub fn fixtures() -> PathBuf {
    repo_root().join("conformance/provider/fixtures/deepwiki/retrieval/sample-repo")
}

pub fn load(relative: &str) -> Value {
    let path = fixtures().join(relative);
    let text = std::fs::read_to_string(&path).unwrap_or_else(|e| panic!("{}: {e}", path.display()));
    serde_json::from_str(&text).unwrap_or_else(|e| panic!("{}: {e}", path.display()))
}

/// `(slug, fixture)` of every recorded query, sorted by file name.
pub fn queries() -> Vec<(String, Value)> {
    let dir = fixtures().join("queries");
    let mut paths: Vec<PathBuf> = std::fs::read_dir(&dir)
        .unwrap_or_else(|e| panic!("{}: {e}", dir.display()))
        .map(|entry| entry.expect("dir entry").path())
        .filter(|path| path.extension().is_some_and(|e| e == "json"))
        .collect();
    paths.sort();
    paths
        .into_iter()
        .map(|path| {
            let slug = path
                .file_stem()
                .expect("stem")
                .to_string_lossy()
                .into_owned();
            let text = std::fs::read_to_string(&path).expect("read query");
            (slug, serde_json::from_str(&text).expect("parse query"))
        })
        .collect()
}

fn text(record: &Value, key: &str) -> String {
    record[key].as_str().unwrap_or_default().to_owned()
}

/// The frozen chunk set the fixtures were recorded over, as the Python
/// conftest builds it (flags default to false).
pub fn corpus() -> Vec<IndexNode> {
    load("nodes.json")["nodes"]
        .as_array()
        .expect("nodes")
        .iter()
        .map(|record| IndexNode {
            node_id: text(record, "node_id"),
            rel_path: text(record, "rel_path"),
            file_name: text(record, "file_name"),
            language: text(record, "language"),
            start_line: i32::try_from(record["start_line"].as_i64().expect("start")).expect("i32"),
            end_line: i32::try_from(record["end_line"].as_i64().expect("end")).expect("i32"),
            symbol_name: text(record, "symbol_name"),
            symbol_type: text(record, "symbol_type"),
            parent_symbol: record["parent_symbol"].as_str().map(str::to_owned),
            source_text: text(record, "source_text"),
            docstring: text(record, "docstring"),
            signature: text(record, "signature"),
            ..IndexNode::default()
        })
        .collect()
}

/// The recorded stub-embedder vectors, in file order.
pub fn embeddings() -> Vec<(String, Vec<f64>)> {
    load("embedding_model.json")["vectors"]
        .as_object()
        .expect("vectors")
        .iter()
        .map(|(id, vector)| (id.clone(), floats(vector)))
        .collect()
}

pub fn floats(value: &Value) -> Vec<f64> {
    value
        .as_array()
        .expect("array")
        .iter()
        .map(|v| v.as_f64().expect("number"))
        .collect()
}

/// `[start, end)` runs of equal scores (`ranking.tie_groups`).
pub fn tie_groups(scores: &[f64], tolerance: f64) -> Vec<(usize, usize)> {
    let mut groups = Vec::new();
    let mut start = 0;
    for index in 1..=scores.len() {
        if index == scores.len() || (scores[index] - scores[start]).abs() > tolerance {
            groups.push((start, index));
            start = index;
        }
    }
    groups
}

/// `ranking.assert_rankings_agree`: same documents, same scores within
/// tolerance, same order up to recorded ties.
pub fn assert_rankings_agree(
    actual_ids: &[String],
    actual_scores: &[f64],
    expected_ids: &[String],
    expected_scores: &[f64],
    tolerance: f64,
    label: &str,
) {
    assert_eq!(
        actual_ids.len(),
        expected_ids.len(),
        "{label}: length differs\n  actual:   {actual_ids:?}\n  expected: {expected_ids:?}"
    );
    let mut a = actual_ids.to_vec();
    let mut e = expected_ids.to_vec();
    a.sort();
    e.sort();
    assert_eq!(a, e, "{label}: different documents were retrieved");
    for (position, (got, want)) in actual_scores.iter().zip(expected_scores).enumerate() {
        assert!(
            (got - want).abs() <= tolerance,
            "{label}: score differs at position {position}: {got} vs {want} (tolerance {tolerance})"
        );
    }
    for (start, end) in tie_groups(expected_scores, tolerance) {
        let mut got = actual_ids[start..end].to_vec();
        let mut want = expected_ids[start..end].to_vec();
        got.sort();
        want.sort();
        assert_eq!(
            got, want,
            "{label}: a document moved across the ranking boundary at [{start}:{end}]"
        );
    }
}

/// `ranking.assert_ordering_agrees`: same documents, and every recorded
/// tie group holds the same documents (scores not compared).
pub fn assert_ordering_agrees(
    actual_ids: &[String],
    expected_ids: &[String],
    expected_scores: &[f64],
    tolerance: f64,
    label: &str,
) {
    let mut a = actual_ids.to_vec();
    let mut e = expected_ids.to_vec();
    a.sort();
    e.sort();
    assert_eq!(a, e, "{label}: different documents were retrieved");
    for (start, end) in tie_groups(expected_scores, tolerance) {
        let mut got = actual_ids[start..end].to_vec();
        let mut want = expected_ids[start..end].to_vec();
        got.sort();
        want.sort();
        assert_eq!(
            got, want,
            "{label}: documents crossed a ranking boundary at [{start}:{end}]"
        );
    }
}

/// `ranking.discordant_pairs`.
pub fn discordant_pairs(actual_ids: &[String], expected_ids: &[String]) -> usize {
    let position: std::collections::HashMap<&str, usize> = actual_ids
        .iter()
        .enumerate()
        .map(|(index, id)| (id.as_str(), index))
        .collect();
    let mut count = 0;
    for i in 0..expected_ids.len() {
        for j in (i + 1)..expected_ids.len() {
            if let (Some(left), Some(right)) = (
                position.get(expected_ids[i].as_str()),
                position.get(expected_ids[j].as_str()),
            ) && left > right
            {
                count += 1;
            }
        }
    }
    count
}

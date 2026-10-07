//! The embedded migrations: one directory, one ledger, pinned checksums.

mod storage_common;

use elitea_deepwiki_engine::storage::StorageError;
use elitea_deepwiki_engine::storage::migrate::{self, MIGRATIONS_DIR};
use std::collections::BTreeMap;
use std::path::PathBuf;
use storage_common as common;

fn migrations_dir() -> PathBuf {
    PathBuf::from(env!("CARGO_MANIFEST_DIR")).join(MIGRATIONS_DIR)
}

/// Every `.sql` file in the directory is embedded, with its current text.
/// A new migration file that is not added to the embedded list fails here.
#[test]
fn the_embedded_set_is_the_directory() {
    let dir = migrations_dir();
    let mut on_disk: Vec<String> = std::fs::read_dir(&dir)
        .unwrap_or_else(|e| panic!("{}: {e}", dir.display()))
        .map(|entry| {
            entry
                .expect("entry")
                .file_name()
                .to_string_lossy()
                .into_owned()
        })
        .filter(|name| {
            std::path::Path::new(name)
                .extension()
                .is_some_and(|e| e.eq_ignore_ascii_case("sql"))
        })
        .collect();
    on_disk.sort();
    assert_eq!(migrate::embedded_file_names(), on_disk);
    let embedded = migrate::embedded().expect("valid");
    for (migration, name) in embedded.iter().zip(&on_disk) {
        let raw = std::fs::read_to_string(dir.join(name)).expect("read");
        let text = elitea_deepwiki_engine::storage::text::universal_newlines(&raw);
        assert_eq!(migration.sql, text, "{name} changed since the build");
    }
}

/// The ledger checksums, pinned. Databases migrated by this binary or by
/// the retired Python runner hold these values in `schema_migrations`; a
/// change to a file's bytes makes every such database refuse to start.
#[test]
fn the_checksums_are_the_ledger_values() {
    let rust: BTreeMap<String, String> = migrate::embedded()
        .expect("valid")
        .into_iter()
        .map(|m| (format!("{}_{}", m.version, m.name), m.checksum()))
        .collect();
    let pinned: BTreeMap<String, String> = [
        (
            "0001_wiki_index_storage",
            "ea5af872e89869cdf824e92f71586ed58143efa30572c74c73a3e7bfa1bb40c3",
        ),
        (
            "0002_invocations",
            "639bb4e76683606c9c5a02b56e868af901be7f4a2bbab53269b2ff508ab909a2",
        ),
        (
            "0003_build_space",
            "e36c440fb1c6d43c2900931f94f4f1c1ea023359c9e9ffbddd93e191efaf2b7e",
        ),
        (
            "0004_build_boot_id",
            "2572543733819202a9dd358c9aaad28ae598bd71691e705a2d4a553b881864ea",
        ),
    ]
    .into_iter()
    .map(|(k, v)| (k.to_owned(), v.to_owned()))
    .collect();
    assert_eq!(rust, pinned);
}

#[tokio::test(flavor = "multi_thread")]
async fn the_ledger_is_written_once_and_guarded() {
    let Some(pool) = common::fresh_database("migrate_ledger").await else {
        return;
    };
    // fresh_database applied everything; a second run applies nothing.
    assert!(migrate::apply_all(&pool).await.expect("rerun").is_empty());
    let rows: Vec<(String, String, String)> =
        sqlx::query_as("SELECT version, name, checksum FROM schema_migrations ORDER BY version")
            .fetch_all(&pool)
            .await
            .expect("ledger");
    let expected: Vec<(String, String, String)> = migrate::embedded()
        .expect("valid")
        .into_iter()
        .map(|m| {
            let checksum = m.checksum();
            (m.version, m.name, checksum)
        })
        .collect();
    assert_eq!(rows, expected);

    // The build space exists, unlogged where ADR-0026 says so.
    // Sorted here: the server's order depends on its collation.
    let mut unlogged: Vec<String> = sqlx::query_scalar(
        "SELECT c.relname::text FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace \
         WHERE n.nspname = 'deepwiki_build' AND c.relkind = 'r' AND c.relpersistence = 'u'",
    )
    .fetch_all(&pool)
    .await
    .expect("catalog");
    unlogged.sort();
    assert_eq!(
        unlogged,
        [
            "bm25_docs",
            "bm25_postings",
            "wiki_edges",
            "wiki_node_embeddings",
            "wiki_nodes"
        ]
    );

    // 0004 added the nullable boot id to the builds.
    let boot_id: Option<(String, String)> = sqlx::query_as(
        "SELECT data_type::text, is_nullable::text FROM information_schema.columns \
         WHERE table_schema = 'deepwiki_build' AND table_name = 'builds' AND column_name = 'boot_id'",
    )
    .fetch_optional(&pool)
    .await
    .expect("catalog");
    assert_eq!(boot_id, Some(("text".to_owned(), "YES".to_owned())));

    // An applied migration whose text changed is refused, before anything
    // new is applied.
    sqlx::query("UPDATE schema_migrations SET checksum = 'tampered' WHERE version = '0002'")
        .execute(&pool)
        .await
        .expect("tamper");
    let refused = migrate::apply_all(&pool).await;
    assert!(
        matches!(&refused, Err(StorageError::Migration(m)) if m.contains("0002_invocations") && m.contains("immutable")),
        "{refused:?}"
    );

    // A failing migration is rolled back with its ledger row.
    let broken = migrate::discover_from([(
        "0099_broken.sql",
        "CREATE TABLE ok_part (x int); SELECT no_such_function();",
    )])
    .expect("named well");
    assert!(migrate::apply(&pool, &broken).await.is_err());
    let recorded: i64 =
        sqlx::query_scalar("SELECT count(*) FROM schema_migrations WHERE version = '0099'")
            .fetch_one(&pool)
            .await
            .expect("ledger");
    let created: Option<String> = sqlx::query_scalar("SELECT to_regclass('ok_part')::text")
        .fetch_one(&pool)
        .await
        .expect("catalog");
    assert_eq!((recorded, created), (0, None));
}

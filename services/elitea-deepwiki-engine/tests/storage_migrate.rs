//! The embedded migrations against `migrate.py`: one directory, one ledger,
//! one checksum.

mod storage_common;

use elitea_deepwiki_engine::storage::StorageError;
use elitea_deepwiki_engine::storage::migrate::{self, MIGRATIONS_DIR};
use std::collections::BTreeMap;
use std::path::PathBuf;
use std::process::Command;
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

/// The checksums `migrate.py` itself computes (its `discover()`, run by
/// python3 from this checkout) equal the Rust ones, file for file.
#[test]
fn rust_and_python_compute_the_same_checksums() {
    let src = PathBuf::from(env!("CARGO_MANIFEST_DIR")).join("../elitea-deepwiki/src");
    let script = "import json, sys\n\
                  sys.path.insert(0, sys.argv[1])\n\
                  from elitea_deepwiki.storage.migrate import discover\n\
                  print(json.dumps({m.version + '_' + m.name: m.checksum for m in discover()}))\n";
    let output = Command::new("python3")
        .arg("-c")
        .arg(script)
        .arg(&src)
        .output()
        .expect("python3 runs (it is a test dependency, as git is for the ingest tests)");
    assert!(
        output.status.success(),
        "migrate.discover failed: {}",
        String::from_utf8_lossy(&output.stderr)
    );
    let python: BTreeMap<String, String> =
        serde_json::from_slice(&output.stdout).expect("python printed JSON");
    let rust: BTreeMap<String, String> = migrate::embedded()
        .expect("valid")
        .into_iter()
        .map(|m| (format!("{}_{}", m.version, m.name), m.checksum()))
        .collect();
    assert_eq!(rust.len(), 3);
    assert_eq!(rust, python);
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
    let unlogged: Vec<String> = sqlx::query_scalar(
        "SELECT c.relname::text FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace \
         WHERE n.nspname = 'deepwiki_build' AND c.relkind = 'r' AND c.relpersistence = 'u' \
         ORDER BY 1",
    )
    .fetch_all(&pool)
    .await
    .expect("catalog");
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

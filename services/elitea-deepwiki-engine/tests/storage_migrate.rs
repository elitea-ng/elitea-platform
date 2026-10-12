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
        (
            "0005_project_scope",
            "fe71faa1fc925a050ad73b5d3b7313e1cc8ca926d1218fe476ae950bd3a7cac7",
        ),
        (
            "0006_wiki_embedding_model",
            "85f96a1d833f4b0b8a0df67be6c3b1b4113d68a9e1c8518f875814c9c804cac6",
        ),
        (
            "0007_drop_bm25_branch_rows",
            "67dee023fc4d7a0c9f3dec6fbb82e3a908ad3d30b6425735709434e59b02e1dd",
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

    // 0005: the project leads the key of every index table, and a build
    // names its project.
    for table in [
        "wikis",
        "wiki_nodes",
        "wiki_edges",
        "wiki_node_embeddings",
        "wiki_bm25_meta",
        "wiki_bm25_docs",
        "wiki_bm25_terms",
        "wiki_bm25_postings",
    ] {
        let first: Option<String> = sqlx::query_scalar(
            "SELECT a.attname::text FROM pg_index i \
             JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = i.indkey[0] \
             WHERE i.indrelid = ('public.' || $1)::regclass AND i.indisprimary",
        )
        .bind(table)
        .fetch_optional(&pool)
        .await
        .expect("catalog");
        assert_eq!(first.as_deref(), Some("project_id"), "{table}");
    }
    let project: Option<(String, String)> = sqlx::query_as(
        "SELECT data_type::text, is_nullable::text FROM information_schema.columns \
         WHERE table_schema = 'deepwiki_build' AND table_name = 'builds' AND column_name = 'project_id'",
    )
    .fetch_optional(&pool)
    .await
    .expect("catalog");
    assert_eq!(project, Some(("integer".to_owned(), "NO".to_owned())));

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

/// 0006: the model a wiki was embedded with, nullable, unrecorded for a wiki
/// that was published before it. 0007 is a no-op: the dead `'bm25'` branch's
/// rows are removed by the engine's background cleanup in bounded batches
/// (`storage::cleanup`), never by one unbatched `DELETE` in a migration.
#[tokio::test(flavor = "multi_thread")]
async fn the_embedding_model_columns_exist_and_the_migration_leaves_the_dead_branch_to_the_cleanup()
{
    let Some(pool) = common::empty_database("migrate_model").await else {
        return;
    };
    let all = migrate::embedded().expect("valid");
    let (before, after) = all.split_at(6);
    assert_eq!(after[0].name, "drop_bm25_branch_rows");
    migrate::apply(&pool, before).await.expect("0001-0006");
    for (column, kind) in [("embedding_model", "text"), ("embedding_dim", "integer")] {
        let found: Option<(String, String)> = sqlx::query_as(
            "SELECT data_type::text, is_nullable::text FROM information_schema.columns \
             WHERE table_schema = 'public' AND table_name = 'wikis' AND column_name = $1",
        )
        .bind(column)
        .fetch_optional(&pool)
        .await
        .expect("catalog");
        assert_eq!(found, Some((kind.to_owned(), "YES".to_owned())), "{column}");
    }
    sqlx::raw_sql(
        "INSERT INTO wikis (project_id, wiki_id, repo, branch) VALUES (1, 'w', 'r', 'main');
         INSERT INTO wiki_bm25_meta (project_id, wiki_id, branch, doc_count, avgdl, k1, b)
             VALUES (1, 'w', 'bm25', 1, 1.0, 1.5, 0.75), (1, 'w', 'fts', 1, 1.0, 1.2, 0.75);
         INSERT INTO wiki_bm25_docs (project_id, wiki_id, branch, doc_idx, node_id, length)
             VALUES (1, 'w', 'bm25', 0, 'n', 1), (1, 'w', 'fts', 0, 'n', 1);
         INSERT INTO wiki_bm25_terms (project_id, wiki_id, branch, term, df)
             VALUES (1, 'w', 'bm25', 't', 1), (1, 'w', 'fts', 't', 1);
         INSERT INTO wiki_bm25_postings (project_id, wiki_id, branch, term, doc_idx, tf)
             VALUES (1, 'w', 'bm25', 't', 0, 1), (1, 'w', 'fts', 't', 0, 1);",
    )
    .execute(&pool)
    .await
    .expect("a wiki published before 0007");
    assert_eq!(
        migrate::apply_all(&pool).await.expect("0007"),
        ["0007".to_owned()]
    );
    // Nothing was deleted by the migration: both branches are still there.
    for table in [
        "wiki_bm25_meta",
        "wiki_bm25_docs",
        "wiki_bm25_terms",
        "wiki_bm25_postings",
    ] {
        let branches: Vec<String> =
            sqlx::query_scalar(&format!("SELECT branch FROM {table} ORDER BY branch"))
                .fetch_all(&pool)
                .await
                .expect("branches");
        assert_eq!(branches, ["bm25", "fts"], "{table}");
    }
    // The engine's cleanup removes the dead branch and only that.
    let pacing = elitea_deepwiki_engine::storage::cleanup::Pacing {
        batch_pause: std::time::Duration::ZERO,
        ..Default::default()
    };
    let removed = elitea_deepwiki_engine::storage::cleanup::run_pass(&pool, &pacing)
        .await
        .expect("cleanup");
    assert_eq!(removed, 4);
    for table in [
        "wiki_bm25_meta",
        "wiki_bm25_docs",
        "wiki_bm25_terms",
        "wiki_bm25_postings",
    ] {
        let branches: Vec<String> =
            sqlx::query_scalar(&format!("SELECT branch FROM {table} ORDER BY branch"))
                .fetch_all(&pool)
                .await
                .expect("branches");
        assert_eq!(branches, ["fts"], "{table}");
    }
}

/// 0005 on a database that holds an index from before it: the rows cannot
/// be attributed to a project, so they are deleted (with every build in
/// progress), and the next generation rebuilds the index.
#[tokio::test(flavor = "multi_thread")]
async fn the_project_scope_migration_deletes_the_unattributable_index() {
    let Some(pool) = common::empty_database("migrate_scope").await else {
        return;
    };
    let all = migrate::embedded().expect("valid");
    let (before, scope) = all.split_at(4);
    assert_eq!(scope[0].name, "project_scope");
    migrate::apply(&pool, before).await.expect("0001-0004");
    sqlx::raw_sql(
        "INSERT INTO wikis (wiki_id, repo, branch) VALUES ('acme--repo--main', 'acme/repo', 'main');
         INSERT INTO wiki_nodes (wiki_id, node_id, source_text) VALUES ('acme--repo--main', 'n0', 'x');
         INSERT INTO wiki_bm25_meta (wiki_id, branch, doc_count, avgdl, k1, b)
             VALUES ('acme--repo--main', 'bm25', 1, 1.0, 1.5, 0.75);
         INSERT INTO deepwiki_build.builds (build_id, wiki_id, owner) VALUES ('b1', 'acme--repo--main', 'r');
         INSERT INTO deepwiki_build.wiki_nodes (build_id, node_id) VALUES ('b1', 'n0');",
    )
    .execute(&pool)
    .await
    .expect("a pre-0005 index");

    assert_eq!(
        migrate::apply_all(&pool).await.expect("0005-0007"),
        ["0005".to_owned(), "0006".to_owned(), "0007".to_owned()]
    );
    for table in [
        "wikis",
        "wiki_nodes",
        "wiki_bm25_meta",
        "deepwiki_build.builds",
        "deepwiki_build.wiki_nodes",
    ] {
        let count: i64 = sqlx::query_scalar(&format!("SELECT count(*) FROM {table}"))
            .fetch_one(&pool)
            .await
            .expect("count");
        assert_eq!(count, 0, "{table}");
    }
    // A row without a project is refused from now on.
    let unscoped = sqlx::query(
        "INSERT INTO wikis (wiki_id, repo, branch) VALUES ('acme--repo--main', 'acme/repo', 'main')",
    )
    .execute(&pool)
    .await;
    assert!(unscoped.is_err());
}

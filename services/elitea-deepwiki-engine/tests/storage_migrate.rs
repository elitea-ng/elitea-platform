//! The embedded migrations: one directory, one ledger, pinned checksums,
//! and the #1359 rewrite of 0001, 0003 and 0005 (pgvector removal): a fresh
//! install on plain PostgreSQL, an upgrade of a database that applied the
//! released texts (`tests/fixtures/migrations_released/`), and a ledger
//! checksum that is neither the released nor the current one, refused.

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
///
/// 0001, 0003 and 0005 were rewritten by #1359; a database that applied
/// their released texts holds [`RELEASED`]'s checksums, which
/// `migrate::REWRITTEN` accepts.
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
            "7e6dfac0960cf5ffd48e2e07ca4e551e41762d421604ddde97ae16437d3ecc17",
        ),
        (
            "0002_invocations",
            "639bb4e76683606c9c5a02b56e868af901be7f4a2bbab53269b2ff508ab909a2",
        ),
        (
            "0003_build_space",
            "6934176a49f12a0df1031f20c3a9a260181df70aab0da1cfd8df18cb0b255d17",
        ),
        (
            "0004_build_boot_id",
            "2572543733819202a9dd358c9aaad28ae598bd71691e705a2d4a553b881864ea",
        ),
        (
            "0005_project_scope",
            "874e92ba7cae420735068e740ad237757d1bb566ec93da1d1c0e21df653e8b94",
        ),
        (
            "0006_wiki_embedding_model",
            "85f96a1d833f4b0b8a0df67be6c3b1b4113d68a9e1c8518f875814c9c804cac6",
        ),
        (
            "0007_drop_bm25_branch_rows",
            "67dee023fc4d7a0c9f3dec6fbb82e3a908ad3d30b6425735709434e59b02e1dd",
        ),
        (
            "0008_vectors_in_qdrant",
            "985358967531833f8f10d8543fa8bedc5351e554566021e559e00c5fa67c397a",
        ),
        (
            "0009_host_fleet",
            "b5b45631bf05f8881f92a1faf11b3faa6b185fc59d4478889368873700c4262a",
        ),
    ]
    .into_iter()
    .map(|(k, v)| (k.to_owned(), v.to_owned()))
    .collect();
    assert_eq!(rust, pinned);
}

/// The released texts of the rewritten migrations and their checksums.
const RELEASED: [(&str, &str, &str); 3] = [
    (
        "0001_wiki_index_storage.sql",
        include_str!("fixtures/migrations_released/0001_wiki_index_storage.sql"),
        "ea5af872e89869cdf824e92f71586ed58143efa30572c74c73a3e7bfa1bb40c3",
    ),
    (
        "0003_build_space.sql",
        include_str!("fixtures/migrations_released/0003_build_space.sql"),
        "e36c440fb1c6d43c2900931f94f4f1c1ea023359c9e9ffbddd93e191efaf2b7e",
    ),
    (
        "0005_project_scope.sql",
        include_str!("fixtures/migrations_released/0005_project_scope.sql"),
        "fe71faa1fc925a050ad73b5d3b7313e1cc8ca926d1218fe476ae950bd3a7cac7",
    ),
];

/// The released 0001-0007: the fixtures for the rewritten files, the
/// embedded text (unchanged since its release) for the others.
fn released_chain() -> Vec<migrate::Migration> {
    let embedded = migrate::embedded().expect("valid");
    let files: Vec<(String, String)> = embedded
        .iter()
        .filter(|m| m.version.as_str() <= "0007")
        .map(|m| {
            let name = format!("{}_{}.sql", m.version, m.name);
            let text = RELEASED
                .iter()
                .find(|(file, _, _)| *file == name)
                .map_or_else(|| m.sql.clone(), |(_, text, _)| (*text).to_owned());
            (name, text)
        })
        .collect();
    migrate::discover_from(files.iter().map(|(n, t)| (n.as_str(), t.as_str()))).expect("valid")
}

#[test]
fn the_released_fixtures_are_the_released_checksums() {
    for (name, text, checksum) in RELEASED {
        let text = elitea_deepwiki_engine::storage::text::universal_newlines(text);
        assert_eq!(migrate::checksum(&text), checksum, "{name}");
    }
    // `REWRITTEN` names exactly these, one checksum each.
    let listed: Vec<(&str, Vec<&str>)> = migrate::REWRITTEN
        .iter()
        .map(|r| (r.version, r.released_checksums.to_vec()))
        .collect();
    let expected: Vec<(&str, Vec<&str>)> = RELEASED
        .iter()
        .map(|(name, _, checksum)| (&name[..4], vec![*checksum]))
        .collect();
    assert_eq!(listed, expected);
}

async fn vector_extension_installed(pool: &sqlx::PgPool) -> bool {
    sqlx::query_scalar("SELECT EXISTS (SELECT 1 FROM pg_extension WHERE extname = 'vector')")
        .fetch_one(pool)
        .await
        .expect("catalog")
}

async fn relation_exists(pool: &sqlx::PgPool, name: &str) -> bool {
    sqlx::query_scalar::<_, Option<String>>("SELECT to_regclass($1)::text")
        .bind(name)
        .fetch_one(pool)
        .await
        .expect("catalog")
        .is_some()
}

/// A fresh install never needs the `vector` extension (plain postgres:18
/// has none) and creates no vector table; 0008 succeeds without it.
#[tokio::test(flavor = "multi_thread")]
async fn a_fresh_install_needs_no_vector_extension() {
    let Some(pool) = common::fresh_database("migrate_fresh").await else {
        return;
    };
    assert!(!vector_extension_installed(&pool).await);
    assert!(!relation_exists(&pool, "public.wiki_node_embeddings").await);
    assert!(!relation_exists(&pool, "deepwiki_build.wiki_node_embeddings").await);
    let column: Option<(String, String)> = sqlx::query_as(
        "SELECT data_type::text, is_nullable::text FROM information_schema.columns \
         WHERE table_schema = 'public' AND table_name = 'wikis' AND column_name = 'active_generation'",
    )
    .fetch_optional(&pool)
    .await
    .expect("catalog");
    assert_eq!(column, Some(("text".to_owned(), "YES".to_owned())));
    // The generation's shape is elitea-vector's.
    let bad = sqlx::query(
        "INSERT INTO wikis (project_id, wiki_id, repo, branch, active_generation) \
         VALUES (1, 'w', 'r', 'main', 'not a generation')",
    )
    .execute(&pool)
    .await;
    assert!(bad.is_err());
}

/// A database that applied the RELEASED texts of 0001-0007 (with the
/// `vector` extension and both embedding tables) upgrades: the rewritten
/// files are accepted by their released checksums, 0008 drops the tables
/// and the extension, and the wikis keep their rows (answering lexically
/// until regenerated). On a server without pgvector the released texts run
/// with the extension line removed and `VECTOR` as `real[]`, the ledger
/// recording the released checksums, which is what the runner checks.
#[tokio::test(flavor = "multi_thread")]
async fn a_database_on_the_released_texts_upgrades() {
    let Some(pool) = common::empty_database("migrate_upgrade").await else {
        return;
    };
    let available: bool = sqlx::query_scalar(
        "SELECT EXISTS (SELECT 1 FROM pg_available_extensions WHERE name = 'vector')",
    )
    .fetch_one(&pool)
    .await
    .expect("catalog");
    let released = released_chain();
    if available {
        migrate::apply(&pool, &released)
            .await
            .expect("the released chain");
        assert!(vector_extension_installed(&pool).await);
    } else {
        eprintln!("no pgvector on this server: the released texts run shimmed");
        sqlx::raw_sql(
            "CREATE TABLE schema_migrations (version TEXT PRIMARY KEY, name TEXT NOT NULL, \
             checksum TEXT NOT NULL, applied_at TIMESTAMPTZ NOT NULL DEFAULT now())",
        )
        .execute(&pool)
        .await
        .expect("ledger");
        for migration in &released {
            let sql = migration
                .sql
                .replace("CREATE EXTENSION IF NOT EXISTS vector;", "")
                .replace(" VECTOR ", " real[] ")
                .replace(" VECTOR  ", " real[]  ");
            sqlx::raw_sql(&sql)
                .execute(&pool)
                .await
                .expect("released text");
            sqlx::query(
                "INSERT INTO schema_migrations (version, name, checksum) VALUES ($1, $2, $3)",
            )
            .bind(&migration.version)
            .bind(&migration.name)
            .bind(migration.checksum())
            .execute(&pool)
            .await
            .expect("ledger row");
        }
    }
    assert!(relation_exists(&pool, "public.wiki_node_embeddings").await);
    assert!(relation_exists(&pool, "deepwiki_build.wiki_node_embeddings").await);
    let embedding = if available { "'[1,2,3]'" } else { "'{1,2,3}'" };
    sqlx::raw_sql(&format!(
        "INSERT INTO wikis (project_id, wiki_id, repo, branch, embedding_model, embedding_dim) \
             VALUES (1, 'w', 'r', 'main', 'm', 3);
         INSERT INTO wiki_nodes (project_id, wiki_id, node_id, source_text) VALUES (1, 'w', 'n0', 'x');
         INSERT INTO wiki_node_embeddings (project_id, wiki_id, node_id, embedding) \
             VALUES (1, 'w', 'n0', {embedding});"
    ))
    .execute(&pool)
    .await
    .expect("an index published by the previous release");

    assert_eq!(
        migrate::apply_all(&pool).await.expect("the new chain"),
        ["0008".to_owned(), "0009".to_owned()]
    );
    assert!(!relation_exists(&pool, "public.wiki_node_embeddings").await);
    assert!(!relation_exists(&pool, "deepwiki_build.wiki_node_embeddings").await);
    assert!(!vector_extension_installed(&pool).await);
    let kept: (String, Option<String>) =
        sqlx::query_as("SELECT wiki_id, active_generation FROM wikis")
            .fetch_one(&pool)
            .await
            .expect("the wiki row survives");
    assert_eq!(kept, ("w".to_owned(), None));
    // The ledger keeps the released checksums of the rewritten files.
    for (name, _, checksum) in RELEASED {
        let recorded: String =
            sqlx::query_scalar("SELECT checksum FROM schema_migrations WHERE version = $1")
                .bind(&name[..4])
                .fetch_one(&pool)
                .await
                .expect("ledger");
        assert_eq!(recorded, checksum, "{name}");
    }
    // Current, so a rerun applies nothing.
    assert_eq!(
        migrate::apply_all(&pool).await.expect("rerun"),
        [] as [std::string::String; 0]
    );
}

/// 0008 keeps the `vector` extension when another object still uses it,
/// and still succeeds. Needs pgvector on the server (skipped otherwise).
#[tokio::test(flavor = "multi_thread")]
async fn the_drop_migration_keeps_an_extension_something_else_uses() {
    let Some(pool) = common::empty_database("migrate_dependent").await else {
        return;
    };
    let available: bool = sqlx::query_scalar(
        "SELECT EXISTS (SELECT 1 FROM pg_available_extensions WHERE name = 'vector')",
    )
    .fetch_one(&pool)
    .await
    .expect("catalog");
    if !available {
        eprintln!("SKIPPED: no pgvector on this server");
        return;
    }
    migrate::apply(&pool, &released_chain())
        .await
        .expect("the released chain");
    sqlx::raw_sql("CREATE TABLE another_service_vectors (v vector(3))")
        .execute(&pool)
        .await
        .expect("a dependent object");
    assert_eq!(
        migrate::apply_all(&pool).await.expect("0008 succeeds"),
        ["0008".to_owned(), "0009".to_owned()]
    );
    assert!(vector_extension_installed(&pool).await);
    assert!(!relation_exists(&pool, "public.wiki_node_embeddings").await);
    assert!(relation_exists(&pool, "public.another_service_vectors").await);
}

/// The rewrite exception is exact: a ledger checksum of a rewritten
/// migration that is neither its released nor its current text is refused,
/// before anything new is applied.
#[tokio::test(flavor = "multi_thread")]
async fn a_tampered_checksum_of_a_rewritten_migration_is_refused() {
    let Some(pool) = common::fresh_database("migrate_tampered").await else {
        return;
    };
    for (version, name) in [
        ("0001", "0001_wiki_index_storage"),
        ("0003", "0003_build_space"),
        ("0005", "0005_project_scope"),
    ] {
        let original: String =
            sqlx::query_scalar("SELECT checksum FROM schema_migrations WHERE version = $1")
                .bind(version)
                .fetch_one(&pool)
                .await
                .expect("ledger");
        // A released checksum one character off.
        let released = RELEASED
            .iter()
            .find(|(file, _, _)| file.starts_with(version))
            .map(|(_, _, c)| *c)
            .expect("released");
        let tampered = format!("{}0", &released[..63]);
        assert_ne!(tampered, released);
        sqlx::query("UPDATE schema_migrations SET checksum = $1 WHERE version = $2")
            .bind(&tampered)
            .bind(version)
            .execute(&pool)
            .await
            .expect("tamper");
        let refused = migrate::apply_all(&pool).await;
        assert!(
            matches!(&refused, Err(StorageError::Migration(m)) if m.contains(name) && m.contains("immutable")),
            "{version}: {refused:?}"
        );
        // The released checksum itself is accepted.
        sqlx::query("UPDATE schema_migrations SET checksum = $1 WHERE version = $2")
            .bind(released)
            .bind(version)
            .execute(&pool)
            .await
            .expect("released");
        assert_eq!(
            migrate::apply_all(&pool).await.expect("released"),
            [] as [std::string::String; 0]
        );
        sqlx::query("UPDATE schema_migrations SET checksum = $1 WHERE version = $2")
            .bind(&original)
            .bind(version)
            .execute(&pool)
            .await
            .expect("restore");
    }
}

#[tokio::test(flavor = "multi_thread")]
async fn the_ledger_is_written_once_and_guarded() {
    let Some(pool) = common::fresh_database("migrate_ledger").await else {
        return;
    };
    // fresh_database applied everything; a second run applies nothing.
    let rerun = migrate::apply_all(&pool).await.expect("rerun");
    assert_eq!(rerun, [] as [String; 0]);
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
        ["bm25_docs", "bm25_postings", "wiki_edges", "wiki_nodes"]
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
        ["0007".to_owned(), "0008".to_owned(), "0009".to_owned()]
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
        migrate::apply_all(&pool).await.expect("0005-0008"),
        [
            "0005".to_owned(),
            "0006".to_owned(),
            "0007".to_owned(),
            "0008".to_owned(),
            "0009".to_owned()
        ]
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

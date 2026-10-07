//! The service migrations, embedded, and the `schema_migrations` ledger.
//!
//! ADR-0026 decision 5: the Rust binary writes the ledger the retired Python
//! runner (`migrate.py`) wrote, so a database that runner migrated stays
//! valid. The SQL files moved from the Python package into this crate's
//! `migrations/` unchanged; their bytes are frozen because the ledger holds
//! each file's SHA-256 (`tests/storage_migrate.rs` pins them).
//!
//! The rules are `migrate.py`'s:
//!
//! * files are `NNNN_name.sql` (`^(\d{4})_([a-z0-9_]+)\.sql$`), applied in
//!   version order, a duplicate version is an error;
//! * the checksum is the lowercase SHA-256 hex of the UTF-8 text Python
//!   reads (`Path.read_text`, which translates `\r\n` and `\r` to `\n`), and
//!   that same text is what is executed;
//! * an applied migration whose file now hashes differently is an error
//!   (applied migrations are immutable), checked before anything is applied;
//! * each migration runs in its own transaction together with its ledger
//!   row.
//!
//! One addition: the run holds a session advisory lock, so two engine
//! replicas started together cannot both apply the same file. The Python
//! runner took no lock; the lock changes nothing in the ledger.

use crate::storage::text::universal_newlines;
use crate::storage::{Result, StorageError};
use sha2::{Digest, Sha256};
use sqlx::postgres::PgPool;
use sqlx::{Connection, Row};
use std::fmt::Write as _;

/// The migration directory, relative to the crate root. Tests read it to
/// prove the embedded list is the directory's list.
pub const MIGRATIONS_DIR: &str = "migrations";

/// `(file name, file text)` of every migration, in version order.
///
/// A new migration file is added here too; a test fails while the
/// directory and this list disagree.
const EMBEDDED: &[(&str, &str)] = &[
    (
        "0001_wiki_index_storage.sql",
        include_str!("../../migrations/0001_wiki_index_storage.sql"),
    ),
    (
        "0002_invocations.sql",
        include_str!("../../migrations/0002_invocations.sql"),
    ),
    (
        "0003_build_space.sql",
        include_str!("../../migrations/0003_build_space.sql"),
    ),
    (
        "0004_build_boot_id.sql",
        include_str!("../../migrations/0004_build_boot_id.sql"),
    ),
];

/// `migrate._BOOTSTRAP`, verbatim.
const BOOTSTRAP: &str = "
CREATE TABLE IF NOT EXISTS schema_migrations (
    version    TEXT        PRIMARY KEY,
    name       TEXT        NOT NULL,
    checksum   TEXT        NOT NULL,
    applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
";

/// The advisory lock key: `hashtext('elitea_deepwiki.schema_migrations')`
/// computed by the server, so no constant can drift from it.
const LOCK_KEY_SQL: &str = "SELECT hashtext('elitea_deepwiki.schema_migrations')::bigint";

/// One migration.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Migration {
    pub version: String,
    pub name: String,
    /// The text Python reads (newlines translated). The executed SQL.
    pub sql: String,
}

impl Migration {
    /// `hashlib.sha256(self.sql.encode("utf-8")).hexdigest()`.
    #[must_use]
    pub fn checksum(&self) -> String {
        checksum(&self.sql)
    }
}

/// The lowercase SHA-256 hex of `text`'s UTF-8 bytes.
#[must_use]
pub fn checksum(text: &str) -> String {
    let digest = Sha256::digest(text.as_bytes());
    let mut hex = String::with_capacity(64);
    for byte in digest {
        let _ = write!(hex, "{byte:02x}");
    }
    hex
}

/// `^(\d{4})_([a-z0-9_]+)\.sql$` → `(version, name)`.
fn split_file_name(file_name: &str) -> Option<(&str, &str)> {
    let stem = file_name.strip_suffix(".sql")?;
    let (version, name) = (stem.get(..4)?, stem.get(4..)?.strip_prefix('_')?);
    let version_ok = version.bytes().all(|b| b.is_ascii_digit());
    let name_ok = !name.is_empty()
        && name
            .bytes()
            .all(|b| b.is_ascii_lowercase() || b.is_ascii_digit() || b == b'_');
    (version_ok && name_ok).then_some((version, name))
}

/// Build the ordered migration list from `(file name, raw text)` pairs:
/// `migrate.discover` over the given files.
///
/// # Errors
///
/// [`StorageError::Migration`] for a misnamed file or a duplicate version.
pub fn discover_from<'a>(
    files: impl IntoIterator<Item = (&'a str, &'a str)>,
) -> Result<Vec<Migration>> {
    let mut files: Vec<(&str, &str)> = files.into_iter().collect();
    // `sorted(directory.glob("*.sql"))`: path order.
    files.sort_by(|a, b| a.0.cmp(b.0));
    let mut migrations: Vec<Migration> = Vec::with_capacity(files.len());
    for (file_name, raw) in files {
        let Some((version, name)) = split_file_name(file_name) else {
            return Err(StorageError::Migration(format!(
                "{file_name} does not match NNNN_name.sql; migrations must be numbered so their order is unambiguous"
            )));
        };
        if let Some(previous) = migrations.iter().find(|m| m.version == version) {
            return Err(StorageError::Migration(format!(
                "duplicate migration version {version}: {}_{}.sql and {file_name}",
                previous.version, previous.name
            )));
        }
        migrations.push(Migration {
            version: version.to_owned(),
            name: name.to_owned(),
            sql: universal_newlines(raw).into_owned(),
        });
    }
    Ok(migrations)
}

/// The embedded migrations, in version order.
///
/// # Errors
///
/// See [`discover_from`]; a test proves the embedded set is valid.
pub fn embedded() -> Result<Vec<Migration>> {
    discover_from(EMBEDDED.iter().copied())
}

/// The embedded file names, for the test that compares them with the
/// directory.
#[must_use]
pub fn embedded_file_names() -> Vec<&'static str> {
    EMBEDDED.iter().map(|(name, _)| *name).collect()
}

/// `migrate.apply_all` with the embedded migrations. Returns the versions
/// applied by this call (empty when the database was already current).
///
/// # Errors
///
/// [`StorageError::Migration`] when an applied migration's checksum differs
/// from its file; [`StorageError::Database`] when a statement fails (the
/// failed migration's transaction is rolled back, so it stays unapplied).
pub async fn apply_all(pool: &PgPool) -> Result<Vec<String>> {
    apply(pool, &embedded()?).await
}

/// [`apply_all`] over an explicit migration list (tests use their own).
///
/// # Errors
///
/// See [`apply_all`].
pub async fn apply(pool: &PgPool, migrations: &[Migration]) -> Result<Vec<String>> {
    let mut connection = pool.acquire().await?;
    let key: i64 = sqlx::query_scalar(LOCK_KEY_SQL)
        .fetch_one(&mut *connection)
        .await?;
    sqlx::query("SELECT pg_advisory_lock($1)")
        .bind(key)
        .execute(&mut *connection)
        .await?;
    let outcome = apply_locked(&mut connection, migrations).await;
    // Released even when a migration failed. A failure to release ends the
    // session's lock with the connection, so it is not reported over the
    // migration's own outcome.
    let unlocked = sqlx::query("SELECT pg_advisory_unlock($1)")
        .bind(key)
        .execute(&mut *connection)
        .await;
    if unlocked.is_err() {
        connection.detach();
    }
    outcome
}

async fn apply_locked(
    connection: &mut sqlx::postgres::PgConnection,
    migrations: &[Migration],
) -> Result<Vec<String>> {
    sqlx::raw_sql(BOOTSTRAP).execute(&mut *connection).await?;

    let rows = sqlx::query("SELECT version, name, checksum FROM schema_migrations")
        .fetch_all(&mut *connection)
        .await?;
    let mut applied = std::collections::HashMap::with_capacity(rows.len());
    for row in rows {
        let version: String = row.try_get("version")?;
        let checksum: String = row.try_get("checksum")?;
        applied.insert(version, checksum);
    }

    for migration in migrations {
        if let Some(recorded) = applied.get(&migration.version) {
            let current = migration.checksum();
            if *recorded != current {
                return Err(StorageError::Migration(format!(
                    "migration {}_{} was applied with checksum {recorded} but the file now hashes to {current}. Applied migrations are immutable — add a new migration instead of editing this one.",
                    migration.version, migration.name
                )));
            }
        }
    }

    let mut newly_applied = Vec::new();
    for migration in migrations {
        if applied.contains_key(&migration.version) {
            continue;
        }
        tracing::info!(
            version = %migration.version,
            name = %migration.name,
            "applying migration"
        );
        let mut transaction = connection.begin().await?;
        // The simple-query protocol: a migration is several statements,
        // as psycopg sends a parameterless execute.
        sqlx::raw_sql(&migration.sql)
            .execute(&mut *transaction)
            .await?;
        sqlx::query("INSERT INTO schema_migrations (version, name, checksum) VALUES ($1, $2, $3)")
            .bind(&migration.version)
            .bind(&migration.name)
            .bind(migration.checksum())
            .execute(&mut *transaction)
            .await?;
        transaction.commit().await?;
        newly_applied.push(migration.version.clone());
    }
    Ok(newly_applied)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_embedded_set_is_valid_and_ordered() {
        let migrations = embedded().unwrap_or_default();
        let versions: Vec<&str> = migrations.iter().map(|m| m.version.as_str()).collect();
        assert_eq!(versions, ["0001", "0002", "0003", "0004"]);
    }

    #[test]
    fn a_misnamed_file_is_an_error() {
        let result = discover_from([("0001_fine.sql", "SELECT 1;"), ("oops.sql", "SELECT 1;")]);
        assert!(
            matches!(&result, Err(StorageError::Migration(m)) if m.contains("does not match")),
            "{result:?}"
        );
        for bad in [
            "001_x.sql",
            "0001-x.sql",
            "0001_X.sql",
            "0001_.sql",
            "0001_x.SQL",
        ] {
            assert!(split_file_name(bad).is_none(), "{bad}");
        }
    }

    #[test]
    fn duplicate_versions_are_rejected() {
        let result = discover_from([("0001_one.sql", "SELECT 1;"), ("0001_two.sql", "SELECT 2;")]);
        assert!(
            matches!(&result, Err(StorageError::Migration(m)) if m.contains("duplicate migration version")),
            "{result:?}"
        );
    }

    #[test]
    fn the_checksum_is_over_the_text_python_reads() {
        let crlf = discover_from([("0001_a.sql", "SELECT 1;\r\nSELECT 2;\r")]).unwrap_or_default();
        let lf = discover_from([("0001_a.sql", "SELECT 1;\nSELECT 2;\n")]).unwrap_or_default();
        assert_eq!(crlf[0].checksum(), lf[0].checksum());
        assert_eq!(
            checksum(""),
            "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
        );
    }

    #[test]
    fn the_migration_creates_the_text_search_configuration() {
        let migrations = embedded().unwrap_or_default();
        let config = format!(
            "CREATE TEXT SEARCH CONFIGURATION {}",
            crate::storage::text::TS_CONFIG
        );
        assert!(migrations[0].sql.contains(&config));
    }
}

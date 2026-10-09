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
//! The runner itself is the shared `elitea-pg-migrate` crate (ADR-0027);
//! this module holds what is the engine's: the files and the [`LEDGER`],
//! whose table and lock name are the ones this engine always used.
//!
//! One addition: the run holds a session advisory lock, so two engine
//! replicas started together cannot both apply the same file. The Python
//! runner took no lock; the lock changes nothing in the ledger.

use crate::storage::{Result, StorageError};
use elitea_pg_migrate::{Ledger, MigrateError};
use sqlx::postgres::PgPool;

pub use elitea_pg_migrate::{Migration, checksum};

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
    (
        "0005_project_scope.sql",
        include_str!("../../migrations/0005_project_scope.sql"),
    ),
];

/// This engine's ledger: `schema_migrations`, locked on
/// `hashtext('elitea_deepwiki.schema_migrations')` — unchanged, so a database
/// migrated before the runner moved stays valid.
pub const LEDGER: Ledger = Ledger {
    table: "schema_migrations",
    lock_name: "elitea_deepwiki.schema_migrations",
};

impl From<MigrateError> for StorageError {
    fn from(error: MigrateError) -> Self {
        match error {
            MigrateError::Migration(message) => Self::Migration(message),
            MigrateError::Database(error) => Self::Database(error),
        }
    }
}

/// Build the ordered migration list from `(file name, raw text)` pairs.
///
/// # Errors
///
/// [`StorageError::Migration`] for a misnamed file or a duplicate version.
pub fn discover_from<'a>(
    files: impl IntoIterator<Item = (&'a str, &'a str)>,
) -> Result<Vec<Migration>> {
    Ok(elitea_pg_migrate::discover_from(files)?)
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
/// from its file; [`StorageError::Database`] when a statement fails.
pub async fn apply_all(pool: &PgPool) -> Result<Vec<String>> {
    apply(pool, &embedded()?).await
}

/// [`apply_all`] over an explicit migration list (tests use their own).
///
/// # Errors
///
/// See [`apply_all`].
pub async fn apply(pool: &PgPool, migrations: &[Migration]) -> Result<Vec<String>> {
    Ok(elitea_pg_migrate::apply(pool, &LEDGER, migrations).await?)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_embedded_set_is_valid_and_ordered() {
        let migrations = embedded().unwrap_or_default();
        let versions: Vec<&str> = migrations.iter().map(|m| m.version.as_str()).collect();
        assert_eq!(versions, ["0001", "0002", "0003", "0004", "0005"]);
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

//! Embedded SQL migrations and their checksum ledger, for the engines'
//! own PostgreSQL schemas (ADR-0027).
//!
//! Extracted from the `DeepWiki` engine, whose rules are the retired Python
//! runner's (`migrate.py`), so a database that runner migrated stays valid:
//!
//! * files are `NNNN_name.sql` (`^(\d{4})_([a-z0-9_]+)\.sql$`), applied in
//!   version order, a duplicate version is an error;
//! * the checksum is the lowercase SHA-256 hex of the UTF-8 text with `\r\n`
//!   and `\r` translated to `\n` (Python's `read_text`), and that same text
//!   is what is executed;
//! * an applied migration whose file now hashes differently is an error
//!   (applied migrations are immutable), checked before anything is applied;
//! * each migration runs in its own transaction together with its ledger row;
//! * the run holds a session advisory lock, so two replicas started together
//!   cannot both apply the same file.
//!
//! What an engine supplies is its [`Ledger`]: the table the ledger lives in
//! and the name the lock key is hashed from. Two engines in one database
//! must not share either.

use sha2::{Digest, Sha256};
use sqlx::postgres::PgPool;
use sqlx::{Connection, Row};
use std::fmt::Write as _;

/// A migration failure: a misnamed or edited file, or the database.
#[derive(Debug, thiserror::Error)]
pub enum MigrateError {
    #[error("{0}")]
    Migration(String),
    #[error(transparent)]
    Database(#[from] sqlx::Error),
}

type Result<T> = std::result::Result<T, MigrateError>;

/// Where an engine's ledger lives and how its runs are serialized.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct Ledger {
    /// The ledger table, optionally schema-qualified (`schema_migrations`,
    /// `inventory_graph.schema_migrations`). Letters, digits, `_` and one `.`.
    pub table: &'static str,
    /// Hashed by the server (`hashtext`) into the advisory lock key, so no
    /// constant can drift from it.
    pub lock_name: &'static str,
}

impl Ledger {
    fn checked_table(&self) -> Result<&'static str> {
        let valid = !self.table.is_empty()
            && self.table.matches('.').count() <= 1
            && self.table.split('.').all(|part| {
                part.bytes()
                    .next()
                    .is_some_and(|b| b.is_ascii_lowercase() || b == b'_')
                    && part
                        .bytes()
                        .all(|b| b.is_ascii_lowercase() || b.is_ascii_digit() || b == b'_')
            });
        if valid {
            Ok(self.table)
        } else {
            Err(MigrateError::Migration(format!(
                "ledger table {:?} is not a lowercase SQL identifier",
                self.table
            )))
        }
    }
}

/// Python's `read_text` newline translation.
fn universal_newlines(text: &str) -> std::borrow::Cow<'_, str> {
    if !text.contains('\r') {
        return std::borrow::Cow::Borrowed(text);
    }
    std::borrow::Cow::Owned(text.replace("\r\n", "\n").replace('\r', "\n"))
}

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
/// [`MigrateError::Migration`] for a misnamed file or a duplicate version.
pub fn discover_from<'a>(
    files: impl IntoIterator<Item = (&'a str, &'a str)>,
) -> Result<Vec<Migration>> {
    let mut files: Vec<(&str, &str)> = files.into_iter().collect();
    // `sorted(directory.glob("*.sql"))`: path order.
    files.sort_by(|a, b| a.0.cmp(b.0));
    let mut migrations: Vec<Migration> = Vec::with_capacity(files.len());
    for (file_name, raw) in files {
        let Some((version, name)) = split_file_name(file_name) else {
            return Err(MigrateError::Migration(format!(
                "{file_name} does not match NNNN_name.sql; migrations must be numbered so their order is unambiguous"
            )));
        };
        if let Some(previous) = migrations.iter().find(|m| m.version == version) {
            return Err(MigrateError::Migration(format!(
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

/// Apply every migration of `migrations` the ledger does not hold, in
/// order. Returns the versions applied by this call (empty when the database
/// was already current).
///
/// # Errors
///
/// [`MigrateError::Migration`] when an applied migration's checksum differs
/// from its file; [`MigrateError::Database`] when a statement fails (the
/// failed migration's transaction is rolled back, so it stays unapplied).
pub async fn apply(
    pool: &PgPool,
    ledger: &Ledger,
    migrations: &[Migration],
) -> Result<Vec<String>> {
    let table = ledger.checked_table()?;
    let mut connection = pool.acquire().await?;
    let key: i64 = sqlx::query_scalar("SELECT hashtext($1)::bigint")
        .bind(ledger.lock_name)
        .fetch_one(&mut *connection)
        .await?;
    sqlx::query("SELECT pg_advisory_lock($1)")
        .bind(key)
        .execute(&mut *connection)
        .await?;
    let outcome = apply_locked(&mut connection, table, migrations).await;
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
    table: &str,
    migrations: &[Migration],
) -> Result<Vec<String>> {
    // `migrate._BOOTSTRAP`, with the engine's ledger table.
    sqlx::raw_sql(&format!(
        "CREATE TABLE IF NOT EXISTS {table} (
    version    TEXT        PRIMARY KEY,
    name       TEXT        NOT NULL,
    checksum   TEXT        NOT NULL,
    applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
);"
    ))
    .execute(&mut *connection)
    .await?;

    let rows = sqlx::query(&format!("SELECT version, name, checksum FROM {table}"))
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
                return Err(MigrateError::Migration(format!(
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
        sqlx::query(&format!(
            "INSERT INTO {table} (version, name, checksum) VALUES ($1, $2, $3)"
        ))
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
    fn a_ledger_table_must_be_an_identifier() {
        for table in [
            "schema_migrations",
            "inventory_graph.schema_migrations",
            "_x9",
        ] {
            assert!(
                Ledger {
                    table,
                    lock_name: "l"
                }
                .checked_table()
                .is_ok(),
                "{table}"
            );
        }
        for table in ["", "Schema", "a.b.c", "x; DROP TABLE y", "9x", "a-b"] {
            assert!(
                Ledger {
                    table,
                    lock_name: "l"
                }
                .checked_table()
                .is_err(),
                "{table}"
            );
        }
    }

    #[test]
    fn a_misnamed_file_is_an_error() {
        let result = discover_from([("0001_fine.sql", "SELECT 1;"), ("oops.sql", "SELECT 1;")]);
        assert!(
            matches!(&result, Err(MigrateError::Migration(m)) if m.contains("does not match")),
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
            matches!(&result, Err(MigrateError::Migration(m)) if m.contains("duplicate migration version")),
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
}

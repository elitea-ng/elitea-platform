//! The index storage: PostgreSQL only (ADR-0026 decision 5).
//!
//! The owner decision is "no SQLite anywhere; the build space is in
//! PostgreSQL". The Python engine built a `.wiki.db` (SQLite with FTS5 and
//! sqlite-vec) on scratch and `storage/publish.py` copied it into the
//! ADR-0022 tables afterwards. This engine has no local index file. It
//! writes the code graph into the build space (migration 0003, schema
//! `deepwiki_build`) and publishes it into the ADR-0022 tables in one
//! transaction.
//!
//! * [`migrate`] applies the service migrations (`migrations/` in this
//!   crate), which are embedded here. Their bytes are frozen: databases
//!   hold each file's SHA-256 in the `schema_migrations` ledger.
//! * [`rows`] maps graph rows onto the ADR-0022 columns as `publish.py`
//!   maps the `.wiki.db` rows.
//! * [`build`] stages a graph (`COPY`), publishes it, and reconciles
//!   abandoned builds.
//! * [`search`] is the read path: a port of `storage/postgres.py`'s search
//!   methods and `storage/base.py`'s frozen weighted RRF.
//! * [`adapter`] is the read surface `ask`, `deep_research` and
//!   `resolve_wiki` use: a port of `storage/unified_db_adapter.py`.
//! * [`cleanup`] removes the dead `'bm25'` statistics branch in the
//!   background, in bounded statements (migration 0007 is a no-op).
//! * [`text`] holds the tokenizer and the BM25 arithmetic both sides share.
//! * [`topology`] is Phase 2's index over a build's staged rows.
//! * [`scope`] is the tenancy key: every index row belongs to one
//!   platform project, and every reader and build is made with one
//!   ([`ProjectScope`], [`WikiKey`]; migration 0005).
//!
//! A DSN carries a password. Nothing in this module logs or formats one,
//! and a DSN that does not parse is reported without its text.

#![cfg_attr(
    not(test),
    deny(clippy::expect_used, clippy::panic, clippy::unwrap_used)
)]

pub mod adapter;
pub mod build;
pub mod cleanup;
mod copy;
pub mod delete;
pub mod migrate;
pub mod rows;
pub mod scope;
pub mod search;
pub mod text;
pub mod topology;

pub use scope::{PROJECT_ARG, ProjectScope, WikiKey};
use sqlx::postgres::{PgConnectOptions, PgPool, PgPoolOptions};
use std::str::FromStr;
use std::time::Duration;

/// The variable the Python service and its migration Job read. The engine
/// reads the same one, so a deployment cannot point the two runners at
/// different databases.
pub const DSN_ENV: &str = "ELITEA_DEEPWIKI_DATABASE_URL";

/// A storage failure.
#[derive(Debug, thiserror::Error)]
pub enum StorageError {
    /// The database refused a statement or could not be reached.
    #[error("database error: {0}")]
    Database(#[from] sqlx::Error),
    /// The migration set and the database disagree (`MigrationError`).
    #[error("{0}")]
    Migration(String),
    /// A build cannot be staged or published (`PublishError`).
    #[error("{0}")]
    Publish(String),
    /// A deletion that cannot start or cannot finish (issue #1243): a stale
    /// project listing, or a project that still holds wikis after the
    /// rounds a deletion makes. The message says which and what remains.
    #[error("{0}")]
    Delete(String),
    /// The wiki was republished with another embedding model between the
    /// caller choosing its question's model and the search: the stored
    /// vectors are no longer comparable to the question's.
    #[error(
        "wiki '{wiki_id}' was re-indexed with the embedding model '{stored}' while this question was embedded with '{expected}'; its vectors are only comparable with '{stored}'. Ask again"
    )]
    EmbeddingModelChanged {
        wiki_id: String,
        stored: String,
        expected: String,
    },
    /// A deletion gave up waiting for a publish of the same wiki (the
    /// bounded advisory-lock wait). Nothing was deleted; the caller retries.
    #[error("{0}")]
    Busy(String),
    /// A DSN that does not parse. The text is never part of the message:
    /// it can carry a password.
    #[error(
        "the database URL is not a valid postgres:// or postgresql:// URL (its text is not shown because it can carry a password)"
    )]
    InvalidDsn,
}

/// The result type of this module.
pub type Result<T> = std::result::Result<T, StorageError>;

/// Parse a DSN.
///
/// Only the URL form (`postgresql://user:password@host:port/db`) is
/// accepted. psycopg also accepts the `key=value` conninfo form; sqlx does
/// not, so a deployment that used it must switch to the URL form.
///
/// # Errors
///
/// [`StorageError::InvalidDsn`], without the DSN text.
pub fn connect_options(dsn: &str) -> Result<PgConnectOptions> {
    // The parse error can quote the URL; it is dropped, not wrapped.
    PgConnectOptions::from_str(dsn.trim())
        .map_err(|_| StorageError::InvalidDsn)
        .map(|options| {
            options
                .application_name("elitea-deepwiki-engine")
                // `IF NOT EXISTS` notices ("relation ... already exists,
                // skipping") would log at info on every migrate run.
                .options([("client_min_messages", "warning")])
        })
}

/// A pool that connects on first use.
///
/// Lazy, so a process that starts while the database is still coming up
/// (a compose stack, a rolling restart) does not fail its start; the first
/// statement reports the failure instead.
///
/// # Errors
///
/// [`StorageError::InvalidDsn`].
pub fn lazy_pool(dsn: &str, max_connections: u32) -> Result<PgPool> {
    let options = connect_options(dsn)?;
    Ok(PgPoolOptions::new()
        .max_connections(max_connections.max(1))
        .acquire_timeout(Duration::from_secs(30))
        .connect_lazy_with(options))
}

/// How often a generation worker's connections have the server check that
/// the client is still there while a statement runs (5 s): a killed worker
/// leaves no backend running its statement, holding its locks, until the
/// statement ends.
pub const CLIENT_CHECK_INTERVAL_MS: u32 = 5_000;

/// [`lazy_pool`] for a generation worker: every connection sets
/// `client_connection_check_interval` ([`CLIENT_CHECK_INTERVAL_MS`]), so a
/// worker that was killed mid-statement (a stop during the publish) has
/// its statement and transaction aborted within seconds and its locks
/// released. A server that does not know the setting (before PostgreSQL
/// 14) or cannot use it (not Linux) keeps the connection without it.
///
/// # Errors
///
/// [`StorageError::InvalidDsn`].
pub fn worker_pool(dsn: &str, max_connections: u32) -> Result<PgPool> {
    let options = connect_options(dsn)?;
    Ok(PgPoolOptions::new()
        .max_connections(max_connections.max(1))
        .acquire_timeout(Duration::from_secs(30))
        .after_connect(|connection, _meta| {
            Box::pin(async move {
                let set = sqlx::query("SELECT set_config('client_connection_check_interval', $1, false)")
                    .bind(CLIENT_CHECK_INTERVAL_MS.to_string())
                    .execute(&mut *connection)
                    .await;
                if let Err(error) = set {
                    tracing::debug!(%error, "client_connection_check_interval is not available on this server");
                }
                Ok(())
            })
        })
        .connect_lazy_with(options))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn an_invalid_dsn_is_reported_without_its_text() {
        let error = connect_options("host=db password=hunter2").err();
        let message = error.map(|e| e.to_string()).unwrap_or_default();
        assert!(message.contains("not a valid"), "{message}");
        assert!(!message.contains("hunter2"), "{message}");
    }

    #[test]
    fn a_url_dsn_parses() {
        assert!(connect_options("postgresql://u:p@127.0.0.1:5432/deepwiki").is_ok());
    }
}

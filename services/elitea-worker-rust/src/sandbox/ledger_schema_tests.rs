//! SQLSTATE classification checks. No database connection occurs.
use super::{LedgerError, SchemaMigrationRequired};
use sqlx::error::{DatabaseError, ErrorKind};
use std::{borrow::Cow, error::Error};

const PRIVATE_CAUSE: &str = "PRIVATE_SQL_STATEMENT_COLUMN_TABLE_PAYLOAD_DSN";

struct DatabaseFailure(Option<&'static str>);
impl std::fmt::Debug for DatabaseFailure {
    fn fmt(&self, out: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        out.debug_struct("DatabaseFailure")
            .field("code", &self.0)
            .field("private_context", &PRIVATE_CAUSE)
            .finish()
    }
}
impl std::fmt::Display for DatabaseFailure {
    fn fmt(&self, out: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        out.write_str(PRIVATE_CAUSE)
    }
}
impl Error for DatabaseFailure {}
impl DatabaseError for DatabaseFailure {
    fn message(&self) -> &str {
        PRIVATE_CAUSE
    }
    fn code(&self) -> Option<Cow<'_, str>> {
        self.0.map(Cow::Borrowed)
    }
    fn as_error(&self) -> &(dyn Error + Send + Sync + 'static) {
        self
    }
    fn as_error_mut(&mut self) -> &mut (dyn Error + Send + Sync + 'static) {
        self
    }
    fn into_error(self: Box<Self>) -> Box<dyn Error + Send + Sync + 'static> {
        self
    }
    fn kind(&self) -> ErrorKind {
        ErrorKind::Other
    }
}
fn database_error(code: Option<&'static str>) -> sqlx::Error {
    sqlx::Error::Database(Box::new(DatabaseFailure(code)))
}

#[test]
fn missing_schema_classifies_exact_sqlstates_without_private_context() {
    for (code, expected) in [
        ("42703", SchemaMigrationRequired::MissingColumn),
        ("42P01", SchemaMigrationRequired::MissingTable),
    ] {
        let error = LedgerError::from(database_error(Some(code)));
        assert!(
            matches!(&error, LedgerError::SchemaMigrationRequired(schema) if *schema == expected)
        );
        let display = error.to_string();
        assert!(display.contains(code));
        assert!(display.contains("Main AgentState release migrations"));
        assert!(!display.contains(PRIVATE_CAUSE));
        assert!(!format!("{error:?}").contains(PRIVATE_CAUSE));
        assert_eq!(expected.sqlstate(), code);
    }
}

#[test]
fn other_database_codes_keep_the_existing_unavailable_class() {
    for code in [
        None,
        Some("08006"),
        Some("40001"),
        Some("42501"),
        Some("42703PRIVATE"),
    ] {
        let error = LedgerError::from(database_error(code));
        assert!(matches!(&error, LedgerError::Database(_)));
        assert_eq!(error.to_string(), "sandbox job persistence failed");
        assert!(!error.to_string().contains(PRIVATE_CAUSE));
    }
}

#[test]
fn pool_protocol_and_row_errors_never_invent_a_schema_sqlstate() {
    for error in [
        sqlx::Error::PoolTimedOut,
        sqlx::Error::Protocol(PRIVATE_CAUSE.into()),
        sqlx::Error::ColumnNotFound(PRIVATE_CAUSE.into()),
    ] {
        let error = LedgerError::from(error);
        assert!(matches!(&error, LedgerError::Database(_)));
        assert_eq!(error.to_string(), "sandbox job persistence failed");
    }
}

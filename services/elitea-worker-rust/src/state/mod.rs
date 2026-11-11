//! Durable worker-owned execution state.
//!
//! Product records and migrations remain owned by Elitea Main. This module is
//! limited to execution-local state whose schema is versioned with the Rust
//! runtime lineage.

mod postgres_checkpointer;
#[cfg(test)]
mod postgres_checkpointer_tests;
mod postgres_session;
#[cfg(test)]
pub(crate) mod postgres_session_tests;
mod writer_lease;

pub(crate) use postgres_checkpointer::CheckpointWriterAuthority;
pub(crate) use postgres_checkpointer::checkpoint_error_code;
#[cfg(test)]
pub(crate) use postgres_checkpointer::derive_child_thread_id;
pub use postgres_checkpointer::{CheckpointLimits, PostgresCheckpointError, PostgresCheckpointer};
pub(crate) use postgres_session::{
    ClassifiedEvent, SessionWriterAuthority, refresh_span, session_read_error,
};
pub use postgres_session::{PostgresSessionError, PostgresSessionService, SessionLimits};
#[cfg(test)]
pub(crate) use postgres_session::{
    active_events_sql, classified_events_sql, disable_capacity_fast_path,
};
#[cfg(test)]
pub(crate) use writer_lease::TestStateWriterLease;
pub(crate) use writer_lease::{StateWriterLease, StateWriterLeaseLost, TurnSteering};

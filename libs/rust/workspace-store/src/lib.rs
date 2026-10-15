//! The Elitea workspace store client (ADR-0034 decision 2).
//!
//! A thread's folder state moves between the desktop and the cloud executor
//! as **segments**: plain git bundle v2 files, content-addressed by the
//! SHA-256 of their bytes. Phase 1 writes `COMPACT` segments only: every
//! ref the writer names, no prerequisites. PostgreSQL in Main is the commit
//! point (refs, the segment log, the epoch fence); this crate is the client
//! side both writers share:
//!
//! * [`bundle`]: write a `COMPACT` bundle from an object database, and read
//!   one back: parse the header, index the pack into a quarantine, and run
//!   the connectivity check ([`connectivity`]) before any ref or file is
//!   touched. sha1 repositories only; sha256 ones are refused
//!   ([`StoreError::UnsupportedObjectFormat`]).
//! * [`objects`]: a read-only object database (`objects/` and nothing
//!   else: no config, hooks, filters or helpers are read or run), and the
//!   tree walk and tree diff take-back uses.
//! * [`refs`]: the ref allowlist Main enforces, mirrored client-side.
//! * [`transport`]: the store operations (upload grant, presigned PUT or
//!   multipart, publish, fetch plan, GET), [`sync`] the flows over them with
//!   retries and backoff (an idempotent publish retried after a lost ack, a
//!   dropped upload re-sent, a stale plan fetched again), [`memory`] an
//!   in-process store with Main's rules, and [`fault`] a fault-injecting
//!   wrapper for tests. Feature `rest`: the desktop's REST transport.
//!
//! No Tauri, no git CLI: the cloud worker links this crate too.
#![cfg_attr(
    not(test),
    deny(
        clippy::expect_used,
        clippy::panic,
        clippy::todo,
        clippy::unimplemented,
        clippy::unwrap_used
    )
)]

pub mod bundle;
pub mod connectivity;
pub mod digest;
pub mod error;
pub mod fault;
pub mod memory;
pub mod objects;
mod pack;
pub mod refs;
#[cfg(feature = "rest")]
pub mod rest;
pub mod sync;
pub mod transport;

pub use error::StoreError;

/// The segment kind Phase 1 writes (ADR-0034 decision 2).
pub const KIND_COMPACT: &str = "COMPACT";
/// The only object format Phase 1 moves.
pub const OBJECT_FORMAT_SHA1: &str = "sha1";

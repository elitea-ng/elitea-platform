//! The agent runtime the cloud worker and the desktop host share (ADR-0029
//! decision 2).
//!
//! * [`host`] — the host interface: definitions, models, tools, events,
//!   state, memory, approvals and the execution guard, each documented with
//!   the worker code it replaces;
//! * [`canonical`] — order-explicit JSON for digests and rendered values, so
//!   no digest depends on `serde_json`'s `preserve_order`.
//!
//! The move from `services/elitea-worker-rust` is staged; `EXTRACTION.md`
//! holds the dependency map and the remaining stages. The worker re-exports
//! everything here at its old module paths. Nothing here may depend on tonic,
//! async-nats, the agent-state database or the worker's spool; sqlx comes in
//! only behind the SQL toolkit's feature (`EXTRACTION.md`, stage 3).
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

pub mod canonical;
pub mod host;

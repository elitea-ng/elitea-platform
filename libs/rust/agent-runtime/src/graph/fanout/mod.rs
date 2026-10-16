//! Fan-out V2: one runner, two planners (design
//! `services/elitea-worker-rust/docs/fanout-v2-runtime-design.md` §2).
//!
//! - [`plan`]: On Error policy, ordered join entries and join landing.
//! - [`admission`]: Worker-wide child permits and per-model stream lanes.
//! - [`apply`]: the per-interrupt apply rule against the child checkpoint.
//! - [`identity`]: the Worker-minted `interrupt_key` and member `call_id`.
//! - [`runner`]: the single-owner scheduling loop.
//!
//! Hosts (the Worker) supply the planners, the child adapters, the decision
//! client and the lineage store through the traits in [`runner`].

pub mod admission;
pub mod apply;
pub mod identity;
pub mod plan;
pub mod runner;

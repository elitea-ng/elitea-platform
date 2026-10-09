//! The agent runtime the cloud worker and the desktop host share (ADR-0029
//! decision 2).
//!
//! * [`host`] — the host interface: definitions, models, tools, events,
//!   state, memory, approvals and the execution guard, each documented with
//!   the worker code it replaces;
//! * [`graph`] — pipeline graph nodes and value types with no cloud
//!   coupling (YAML contract, Printer, Router, State Modifier, HITL, node
//!   recovery, activation and turn checkpointers, parallel branch control);
//! * [`canonical`] — order-explicit JSON for digests and rendered values, so
//!   no digest depends on `serde_json`'s `preserve_order`;
//! * [`tool_namespacing`] — the provider-alias notice for renamed tools;
//! * [`toolkits`] — native toolkit families, the Streamable HTTP MCP client,
//!   tool admission policy and the flat tool namespace;
//! * [`platform`] — what the runtime writes to the platform
//!   ([`host::PlatformWriter`]);
//! * [`request`], [`context_summary`] — the execution request vocabulary and
//!   the typed continuation notes context compaction writes;
//! * [`assembly_error`], [`instruction_authority`], [`context_budget`],
//!   [`context_status`], [`context_management`] — the shared assembly
//!   vocabulary: the native assembly error, skill and project-context
//!   instruction authority, the request context budget and its status, and
//!   the compaction plan.
//!
//! The move from `services/elitea-worker-rust` is staged; `EXTRACTION.md`
//! holds the dependency map and the remaining stages. The worker re-exports
//! everything here at its old module paths. Nothing here may depend on tonic,
//! async-nats, the agent-state database or the worker's spool; sqlx comes in
//! only behind the SQL toolkit's `toolkit-sql` feature.
// Test fixtures (`test-support`, never in a production build) may panic.
#![cfg_attr(
    not(any(test, feature = "test-support")),
    deny(
        clippy::expect_used,
        clippy::panic,
        clippy::todo,
        clippy::unimplemented,
        clippy::unwrap_used
    )
)]

pub mod assembly_error;
pub mod canonical;
pub mod context_budget;
pub mod context_management;
pub mod context_status;
pub mod context_summary;
pub mod graph;
pub mod host;
pub mod instruction_authority;
pub mod platform;
pub mod request;
pub mod tool_namespacing;
pub mod toolkits;

/// The patched-adk guard. The two builders below exist only in the patched
/// adk 2.2.0 crates in `libs/rust/vendor` (the registry releases have
/// neither), and these constants are compiled into every build of this crate,
/// not only its tests. So a workspace or host that links the runtime without
/// the `[patch.crates-io]` lines fails to COMPILE here instead of running the
/// agent loop unpatched (`libs/rust/vendor/README.md`).
const _: fn(adk_agent::LlmAgentBuilder, bool) -> adk_agent::LlmAgentBuilder =
    adk_agent::LlmAgentBuilder::retain_prepared_history;
const _: fn(adk_runner::Runner, bool) -> adk_runner::Runner =
    adk_runner::Runner::with_session_event_refresh;

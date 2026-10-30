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
//! * [`exact_number`] — exact integer reading of JSON number text, never
//!   through `f64`, shared by typed state reducers and data shaping;
//! * [`planner`], [`lazy_tools`], [`subagents`] — the `planner`,
//!   `lazy_tools_mode` (Smart Tools Selection) and `subagents` internal
//!   tools;
//! * [`memory_tools`] — `save_memory`, `search_memory` and `delete_memory`
//!   over the host's [`host::MemoryStore`] (decision 8, gate 7b);
//! * [`tool_namespacing`] — the provider-alias notice for renamed tools;
//! * [`tool_category`], [`approval_policy`] — the category of every tool and
//!   the tier by category approval policy (ADR-0041 decision 5);
//! * [`context`] — shrinking older tool results in a long turn's model
//!   requests (host opt-in);
//! * [`step_limit`] — the end of a run that used its model steps, and the
//!   person's "Continue?" that runs it again on the same session;
//! * [`egress`] — a run's public egress policy, the one HTTP client a
//!   toolkit family builds, and the reached/refused host ledger (#1490);
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
//!   the compaction plan with the step compaction a host composes around its
//!   model (`StepCompaction`, #1217: pi's reserve and recent tail, the
//!   iterative v1 summary, compact-and-retry once).
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

pub mod approval_policy;
pub mod assembly_error;
pub mod bounded_yaml;
pub mod canonical;
pub mod context;
pub mod context_budget;
pub mod context_management;
pub mod context_status;
pub mod context_summary;
pub mod delegation;
pub mod edit_profile;
pub mod egress;
pub mod exact_number;
pub mod graph;
pub mod host;
pub mod image_result;
pub mod instruction_authority;
pub mod lazy_tools;
pub mod memory_tools;
pub mod planner;
pub mod platform;
pub mod remote_assertion;
pub mod request;
pub mod run_caps;
pub mod step_limit;
pub mod subagents;
pub mod text;
pub mod tool_category;
pub mod tool_namespacing;
pub mod toolkits;
pub mod workspace_archive;
pub mod workspace_tools;

/// The patched-adk guard. The three builders below exist only in the patched
/// adk 2.3.0 crates in `libs/rust/vendor` (the registry releases have
/// none of them), and these constants are compiled into every build of this crate,
/// not only its tests. So a workspace or host that links the runtime without
/// the `[patch.crates-io]` lines fails to COMPILE here instead of running the
/// agent loop unpatched (`libs/rust/vendor/README.md`).
const _: fn(adk_agent::LlmAgentBuilder, bool) -> adk_agent::LlmAgentBuilder =
    adk_agent::LlmAgentBuilder::retain_prepared_history;
const _: fn(adk_runner::Runner, bool) -> adk_runner::Runner =
    adk_runner::Runner::with_session_event_refresh;
const _: fn(
    adk_agent::LlmAgentBuilder,
    std::sync::Arc<dyn adk_agent::SteeringSource>,
) -> adk_agent::LlmAgentBuilder = adk_agent::LlmAgentBuilder::steering_source;

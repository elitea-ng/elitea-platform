//! Pipeline graph nodes and value types that carry no cloud coupling.
//!
//! ADK-Rust owns graph execution. These modules own the stricter YAML
//! contract and the durable node extensions that are not part of the
//! upstream `Checkpoint` model. The compiler and the nodes that still reach
//! the worker's transport, sandbox or state modules stay in the worker until
//! their stage of `EXTRACTION.md`; the worker re-exports these modules at
//! their old paths (`crate::agents::graph::<module>`).
#![expect(
    clippy::missing_errors_doc,
    reason = "moved verbatim from the worker, where these items were crate-private; \
              each error type documents its codes, and per-function `# Errors` \
              sections follow when the module's public API is reviewed"
)]

pub mod application_activation;
pub mod code_platform_drive;
pub mod hitl;
pub mod http_action;
pub mod node_recovery;
pub mod parallel_control;
pub mod printer;
pub mod router;
pub mod state_modifier;
#[cfg(test)]
mod state_modifier_tests;
pub mod turn_checkpointer;
pub mod yaml;

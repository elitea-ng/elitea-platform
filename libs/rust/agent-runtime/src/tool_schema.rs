//! Provider-aware tool parameter schemas (#2145, #2155).
//!
//! The normaliser lives in `elitea-llm-wire` (#2181) so the engines' model
//! client and the adk gateway apply the same rewrite as the worker. This
//! module re-exports it under its original path.

pub use elitea_llm_wire::tool_schema::*;

#[cfg(test)]
#[path = "tool_schema_tests.rs"]
mod tests;

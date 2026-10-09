//! Graph library adapters.
//!
//! Each supported graph library has its own submodule, gated behind a feature flag.
//! Each adapter provides a single entry point function that produces a [`crate::graph::GraphData`].

#[cfg(feature = "gryf")]
pub mod gryf;

#[cfg(feature = "petgraph")]
pub mod petgraph;

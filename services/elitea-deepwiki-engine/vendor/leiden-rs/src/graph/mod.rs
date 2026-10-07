//! Graph data structures for the Leiden algorithm.
//!
//! This module contains the core graph representation ([`GraphData`]) used by
//! all algorithm implementations, the builder ([`GraphDataBuilder`]) for
//! constructing graphs from any source, and the [`MoveComponents`] struct
//! used by quality functions.

pub mod builder;
pub mod data;
/// Precomputed move components used by quality functions during local moving.
pub mod move_components;

pub use builder::GraphDataBuilder;
pub use data::GraphData;
pub use move_components::MoveComponents;

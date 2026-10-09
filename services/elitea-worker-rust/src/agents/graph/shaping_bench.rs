//! Rehearsal-only entry points that let `benches/graph_shaping.rs` time the
//! pure `split_out` and `aggregate` evaluators through the public API.
//!
//! The YAML is parsed once, outside the timed region. Errors are the
//! `graph.shaping.*` messages, which never carry data values.

use serde_json::Value;

use super::aggregate::{AggregateNodeDefinition, run as run_aggregate};
use super::split_out::{SplitOutNodeDefinition, run as run_split_out};

/// A pre-parsed `split_out` node.
pub struct SplitOutBench(SplitOutNodeDefinition);

impl SplitOutBench {
    /// Parses one node YAML.
    ///
    /// # Errors
    /// Returns the configuration error text when the YAML is refused.
    pub fn new(yaml: &str) -> Result<Self, String> {
        SplitOutNodeDefinition::from_yaml(yaml)
            .map(Self)
            .map_err(|error| error.to_string())
    }

    /// Evaluates the node over `source`.
    ///
    /// # Errors
    /// Returns the `graph.shaping.*` message of a runtime failure.
    pub fn run(&self, source: &Value) -> Result<Value, String> {
        run_split_out(&self.0, Some(source)).map_err(|error| error.message())
    }
}

/// A pre-parsed `aggregate` node.
pub struct AggregateBench(AggregateNodeDefinition);

impl AggregateBench {
    /// Parses one node YAML.
    ///
    /// # Errors
    /// Returns the configuration error text when the YAML is refused.
    pub fn new(yaml: &str) -> Result<Self, String> {
        AggregateNodeDefinition::from_yaml(yaml)
            .map(Self)
            .map_err(|error| error.to_string())
    }

    /// Evaluates the node over `source`.
    ///
    /// # Errors
    /// Returns the `graph.shaping.*` message of a runtime failure.
    pub fn run(&self, source: &Value) -> Result<Value, String> {
        run_aggregate(&self.0, Some(source)).map_err(|error| error.message())
    }
}

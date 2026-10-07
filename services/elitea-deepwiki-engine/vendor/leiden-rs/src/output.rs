//! JSON serialization for community detection results.
//!
//! Provides a [`ToJson`] trait that converts community detection output structs
//! to JSON strings without external dependencies. JSON is built manually using
//! [`format!`] and string manipulation.
//!
//! # Supported Types
//!
//! - [`LeidenOutput`] — algorithm: `"leiden"`
//! - [`LabelPropagationOutput`] — algorithm: `"label_propagation"`
//! - [`InfomapOutput`] — algorithm: `"infomap"`
//! - [`FluidCommunitiesOutput`] — algorithm: `"fluid_communities"`

use crate::fluid_communities::FluidCommunitiesOutput;
use crate::infomap::InfomapOutput;
use crate::label_propagation::LabelPropagationOutput;
use crate::leiden::LeidenOutput;

/// Generic community detection output for JSON serialization.
///
/// Collects common fields from all algorithm-specific output types into a
/// unified structure that can be serialized to JSON without external dependencies.
pub struct CommunityDetectionOutput {
    /// Name of the algorithm that produced this output
    /// (e.g., `"leiden"`, `"label_propagation"`, `"infomap"`, `"fluid_communities"`).
    pub algorithm: String,
    /// Community membership for each node: `membership[i]` = community ID of node `i`.
    pub membership: Vec<usize>,
    /// Number of distinct communities detected.
    pub num_communities: usize,
    /// Quality score of the partition.
    ///
    /// Meaning depends on the algorithm:
    /// - **leiden**: modularity / CPM / RBConfiguration / RBER value.
    /// - **label_propagation**: iteration count (proxy for convergence cost).
    /// - **infomap**: Map Equation codelength (lower is better).
    /// - **fluid_communities**: always 0.0 (not applicable).
    pub quality: f64,
    /// Number of iterations the algorithm performed.
    pub iterations: usize,
    /// Whether the algorithm converged before reaching the iteration limit.
    pub converged: bool,
}

impl CommunityDetectionOutput {
    /// Build a compact (single-line) JSON string from the fields.
    ///
    /// The output is a single line with no extra whitespace:
    /// ```json
    /// {"algorithm":"leiden","num_communities":2,"quality":0.42,"iterations":0,"converged":true,"membership":[0,0,1,1]}
    /// ```
    #[must_use]
    fn to_json_string(&self) -> String {
        format!(
            r#"{{"algorithm":"{}","num_communities":{},"quality":{},"iterations":{},"converged":{},"membership":{:?}}}"#,
            self.algorithm,
            self.num_communities,
            self.quality,
            self.iterations,
            self.converged,
            self.membership,
        )
    }

    /// Build a pretty-printed (multi-line) JSON string from the fields.
    ///
    /// The output is indented for human readability:
    /// ```json
    /// {
    ///   "algorithm": "leiden",
    ///   "num_communities": 2,
    ///   "quality": 0.42,
    ///   "iterations": 0,
    ///   "converged": true,
    ///   "membership": [0, 0, 1, 1]
    /// }
    /// ```
    #[must_use]
    fn to_json_pretty_string(&self) -> String {
        format!(
            r#"{{"algorithm": "{}",
  "num_communities": {},
  "quality": {},
  "iterations": {},
  "converged": {},
  "membership": {:?}
}}"#,
            self.algorithm,
            self.num_communities,
            self.quality,
            self.iterations,
            self.converged,
            self.membership,
        )
    }
}

/// Trait for converting community detection output structs to JSON strings.
///
/// All JSON is built manually via [`format!`] — no external serialization
/// dependencies are required.
///
/// # Example
///
/// ```ignore
/// use leiden_rs::output::ToJson;
/// use leiden_rs::{Leiden, LeidenConfig};
///
/// let leiden = Leiden::new(LeidenConfig::default());
/// let result = leiden.run(&graph).unwrap();
/// println!("{}", result.to_json_pretty());
/// ```
pub trait ToJson {
    /// Compact single-line JSON representation.
    #[must_use]
    fn to_json(&self) -> String;

    /// Pretty-printed multi-line JSON representation.
    #[must_use]
    fn to_json_pretty(&self) -> String;
}

impl ToJson for LeidenOutput {
    fn to_json(&self) -> String {
        CommunityDetectionOutput {
            algorithm: "leiden".to_string(),
            membership: self.partition.as_slice().to_vec(),
            num_communities: self.partition.num_communities(),
            quality: self.quality,
            iterations: 0,
            converged: true,
        }
        .to_json_string()
    }

    fn to_json_pretty(&self) -> String {
        CommunityDetectionOutput {
            algorithm: "leiden".to_string(),
            membership: self.partition.as_slice().to_vec(),
            num_communities: self.partition.num_communities(),
            quality: self.quality,
            iterations: 0,
            converged: true,
        }
        .to_json_pretty_string()
    }
}

impl ToJson for LabelPropagationOutput {
    fn to_json(&self) -> String {
        CommunityDetectionOutput {
            algorithm: "label_propagation".to_string(),
            membership: self.partition.as_slice().to_vec(),
            num_communities: self.partition.num_communities(),
            // Use iteration count as a quality proxy (lower = faster convergence).
            quality: self.iterations as f64,
            iterations: self.iterations,
            converged: self.converged,
        }
        .to_json_string()
    }

    fn to_json_pretty(&self) -> String {
        CommunityDetectionOutput {
            algorithm: "label_propagation".to_string(),
            membership: self.partition.as_slice().to_vec(),
            num_communities: self.partition.num_communities(),
            quality: self.iterations as f64,
            iterations: self.iterations,
            converged: self.converged,
        }
        .to_json_pretty_string()
    }
}

impl ToJson for InfomapOutput {
    fn to_json(&self) -> String {
        CommunityDetectionOutput {
            algorithm: "infomap".to_string(),
            membership: self.partition.as_slice().to_vec(),
            num_communities: self.partition.num_communities(),
            // Map Equation codelength: lower is better.
            quality: self.codelength,
            iterations: self.iterations,
            converged: true,
        }
        .to_json_string()
    }

    fn to_json_pretty(&self) -> String {
        CommunityDetectionOutput {
            algorithm: "infomap".to_string(),
            membership: self.partition.as_slice().to_vec(),
            num_communities: self.partition.num_communities(),
            quality: self.codelength,
            iterations: self.iterations,
            converged: true,
        }
        .to_json_pretty_string()
    }
}

impl ToJson for FluidCommunitiesOutput {
    fn to_json(&self) -> String {
        CommunityDetectionOutput {
            algorithm: "fluid_communities".to_string(),
            membership: self.partition.as_slice().to_vec(),
            num_communities: self.partition.num_communities(),
            // Fluid Communities has no natural quality score.
            quality: 0.0,
            iterations: self.iterations,
            converged: self.converged,
        }
        .to_json_string()
    }

    fn to_json_pretty(&self) -> String {
        CommunityDetectionOutput {
            algorithm: "fluid_communities".to_string(),
            membership: self.partition.as_slice().to_vec(),
            num_communities: self.partition.num_communities(),
            quality: 0.0,
            iterations: self.iterations,
            converged: self.converged,
        }
        .to_json_pretty_string()
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::partition::Partition;

    /// Helper: build a `CommunityDetectionOutput` for testing.
    fn test_output() -> CommunityDetectionOutput {
        CommunityDetectionOutput {
            algorithm: "test".to_string(),
            membership: vec![0, 0, 1, 1, 2],
            num_communities: 3,
            quality: 0.5,
            iterations: 10,
            converged: true,
        }
    }

    #[test]
    fn test_to_json_compact_format() {
        let out = test_output();
        let json = out.to_json_string();

        // Must start with `{` and end with `}`
        assert!(json.starts_with('{'), "JSON must start with '{{'");
        assert!(json.ends_with('}'), "JSON must end with '}}'");

        // Must contain all expected keys
        assert!(json.contains("\"algorithm\""), "missing algorithm key");
        assert!(json.contains("\"num_communities\""), "missing num_communities key");
        assert!(json.contains("\"quality\""), "missing quality key");
        assert!(json.contains("\"iterations\""), "missing iterations key");
        assert!(json.contains("\"converged\""), "missing converged key");
        assert!(json.contains("\"membership\""), "missing membership key");

        // Values
        assert!(json.contains("\"test\""), "missing algorithm value");
        assert!(json.contains("3"), "missing num_communities value");
        assert!(json.contains("0.5"), "missing quality value");
        assert!(json.contains("10"), "missing iterations value");
        assert!(json.contains("true"), "missing converged value");
        assert!(json.contains("[0, 0, 1, 1, 2]"), "missing membership array");

        // No newlines in compact mode
        assert!(!json.contains('\n'), "compact JSON must not contain newlines");
    }

    #[test]
    fn test_to_json_pretty_format() {
        let out = test_output();
        let json = out.to_json_pretty_string();

        // Must start with `{` and end with `}`
        assert!(json.starts_with('{'), "JSON must start with '{{'");
        assert!(json.ends_with('}'), "JSON must end with '}}'");

        // Must contain all expected keys
        assert!(json.contains("\"algorithm\""), "missing algorithm key");
        assert!(json.contains("\"num_communities\""), "missing num_communities key");
        assert!(json.contains("\"quality\""), "missing quality key");
        assert!(json.contains("\"iterations\""), "missing iterations key");
        assert!(json.contains("\"converged\""), "missing converged key");
        assert!(json.contains("\"membership\""), "missing membership key");

        // Values
        assert!(json.contains("\"test\""), "missing algorithm value");
        assert!(json.contains("3"), "missing num_communities value");
        assert!(json.contains("0.5"), "missing quality value");
        assert!(json.contains("10"), "missing iterations value");
        assert!(json.contains("true"), "missing converged value");
        assert!(json.contains("[0, 0, 1, 1, 2]"), "missing membership array");

        // Must have newlines in pretty mode
        assert!(json.contains('\n'), "pretty JSON must contain newlines");

        // First line contains the opening brace with algorithm field
        let first_line = json.lines().next().unwrap();
        assert!(first_line.starts_with('{'), "first line must start with '{{'");
        assert!(first_line.contains("\"algorithm\""), "first line must contain algorithm");
    }

    #[test]
    fn test_compact_vs_pretty_differ() {
        let out = test_output();
        let compact = out.to_json_string();
        let pretty = out.to_json_pretty_string();

        assert_ne!(compact, pretty, "compact and pretty JSON must differ");
        assert!(
            compact.len() < pretty.len(),
            "compact JSON must be shorter than pretty JSON"
        );
    }

    #[test]
    fn test_json_algorithm_field() {
        let out = CommunityDetectionOutput {
            algorithm: "custom_algo".to_string(),
            membership: vec![0],
            num_communities: 1,
            quality: 1.0,
            iterations: 5,
            converged: false,
        };
        let json = out.to_json_string();
        assert!(json.contains("\"custom_algo\""), "custom algorithm should appear in JSON");
    }

    #[test]
    fn test_json_converged_false() {
        let out = CommunityDetectionOutput {
            algorithm: "test".to_string(),
            membership: vec![0, 0],
            num_communities: 1,
            quality: 0.0,
            iterations: 100,
            converged: false,
        };
        let json = out.to_json_string();
        assert!(json.contains("false"), "converged: false should appear in JSON");
        assert!(!json.contains("true"), "no true value expected when converged is false");
    }

    #[test]
    fn test_json_empty_membership() {
        let out = CommunityDetectionOutput {
            algorithm: "empty".to_string(),
            membership: vec![],
            num_communities: 0,
            quality: 0.0,
            iterations: 0,
            converged: true,
        };
        let json = out.to_json_string();
        assert!(json.contains("[]"), "empty membership should be []");
    }

    #[test]
    fn test_leiden_output_to_json() {
        let partition = Partition::from_membership(vec![0, 0, 1, 1, 2]);
        let output = LeidenOutput {
            partition,
            quality: 0.42,
            quality_history: vec![],
        };
        let json = output.to_json();

        assert!(json.contains("\"algorithm\":\"leiden\""), "leiden algorithm name");
        assert!(json.contains("\"num_communities\":3"), "3 communities");
        assert!(json.contains("\"quality\":0.42"), "quality 0.42");
        assert!(json.contains("\"iterations\":0"), "iterations 0");
        assert!(json.contains("\"converged\":true"), "converged true");
        assert!(json.contains("[0, 0, 1, 1, 2]"), "membership array");
    }

    #[test]
    fn test_label_propagation_output_to_json() {
        let partition = Partition::from_membership(vec![0, 0, 0, 1, 1]);
        let output = LabelPropagationOutput {
            partition,
            iterations: 15,
            converged: true,
        };
        let json = output.to_json();

        assert!(json.contains("\"algorithm\":\"label_propagation\""), "algorithm name");
        assert!(json.contains("\"num_communities\":2"), "2 communities");
        assert!(json.contains("\"quality\":15"), "quality equals iterations");
        assert!(json.contains("\"iterations\":15"), "iterations 15");
        assert!(json.contains("\"converged\":true"), "converged true");
    }

    #[test]
    fn test_infomap_output_to_json() {
        let partition = Partition::from_membership(vec![0, 0, 1, 1]);
        let output = InfomapOutput {
            partition,
            codelength: 3.25,
            num_levels: 2,
            iterations: 8,
        };
        let json = output.to_json();

        assert!(json.contains("\"algorithm\":\"infomap\""), "algorithm name");
        assert!(json.contains("\"num_communities\":2"), "2 communities");
        assert!(json.contains("\"quality\":3.25"), "quality equals codelength");
        assert!(json.contains("\"iterations\":8"), "iterations 8");
        assert!(json.contains("\"converged\":true"), "converged true");
    }

    #[test]
    fn test_fluid_communities_output_to_json() {
        let partition = Partition::from_membership(vec![1, 1, 0, 0]);
        let output = FluidCommunitiesOutput {
            partition,
            iterations: 6,
            converged: true,
        };
        let json = output.to_json();

        assert!(json.contains("\"algorithm\":\"fluid_communities\""), "algorithm name");
        assert!(json.contains("\"num_communities\":2"), "2 communities");
        assert!(json.contains("\"quality\":0,"), "quality defaults to 0.0");
        assert!(json.contains("\"iterations\":6"), "iterations 6");
        assert!(json.contains("\"converged\":true"), "converged true");
    }

    #[test]
    fn test_fluid_not_converged() {
        let partition = Partition::from_membership(vec![0, 0, 1, 1]);
        let output = FluidCommunitiesOutput {
            partition,
            iterations: 100,
            converged: false,
        };
        let json = output.to_json();

        assert!(json.contains("\"algorithm\":\"fluid_communities\""), "algorithm name");
        assert!(json.contains("\"iterations\":100"), "iterations 100");
        assert!(json.contains("\"converged\":false"), "converged false");
    }

    #[test]
    fn test_to_json_pretty_on_leiden_output() {
        let partition = Partition::from_membership(vec![0, 0, 1]);
        let output = LeidenOutput {
            partition,
            quality: 0.33,
            quality_history: vec![],
        };
        let pretty = output.to_json_pretty();

        // Check multi-line structure
        assert!(pretty.starts_with('{'), "starts with brace");
        assert!(pretty.contains('\n'), "contains newlines");
        assert!(pretty.contains("\"algorithm\": \"leiden\""), "algorithm with space after colon");
    }

    #[test]
    fn test_to_json_pretty_on_label_propagation() {
        let partition = Partition::from_membership(vec![0, 1]);
        let output = LabelPropagationOutput {
            partition,
            iterations: 3,
            converged: true,
        };
        let pretty = output.to_json_pretty();

        assert!(pretty.contains("\"iterations\": 3"), "iterations with space after colon");
        assert!(pretty.contains('\n'), "contains newlines");
    }

    #[test]
    fn test_all_output_types_implement_trait() {
        // Compile-time check that all four output types implement ToJson.
        fn assert_to_json<T: ToJson>(_val: &T) {}

        let leiden_out = LeidenOutput {
            partition: Partition::new(1),
            quality: 0.0,
            quality_history: vec![],
        };
        let lp_out = LabelPropagationOutput {
            partition: Partition::new(1),
            iterations: 0,
            converged: true,
        };
        let infomap_out = InfomapOutput {
            partition: Partition::new(1),
            codelength: 0.0,
            num_levels: 1,
            iterations: 0,
        };
        let fluid_out = FluidCommunitiesOutput {
            partition: Partition::new(1),
            iterations: 0,
            converged: true,
        };

        assert_to_json(&leiden_out);
        assert_to_json(&lp_out);
        assert_to_json(&infomap_out);
        assert_to_json(&fluid_out);
    }
}

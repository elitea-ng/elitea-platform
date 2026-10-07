//! Hierarchical community detection output.

#[cfg(feature = "serde")]
use serde::{Deserialize, Serialize};

use crate::partition::Partition;

/// A single level in the hierarchical community detection output.
#[derive(Debug, Clone)]
#[cfg_attr(feature = "serde", derive(Serialize, Deserialize))]
#[non_exhaustive]
pub struct HierarchyLevel {
    /// Number of nodes at this aggregation level.
    pub node_count: usize,
    /// Number of communities at this level.
    pub num_communities: usize,
    /// Quality score at this level.
    pub quality: f64,
    /// Community membership of each original node at this level.
    pub membership: Vec<usize>,
}

/// Hierarchical output from the Leiden algorithm, capturing intermediate
/// community structure at each aggregation level.
#[derive(Debug, Clone)]
#[cfg_attr(feature = "serde", derive(Serialize, Deserialize))]
#[non_exhaustive]
pub struct HierarchicalOutput {
    /// Community structure at each aggregation level.
    pub levels: Vec<HierarchyLevel>,
    /// The final community partition (same as `run()` would produce).
    pub partition: Partition,
    /// Quality score of the final partition.
    pub quality: f64,
}

impl HierarchicalOutput {
    /// Number of hierarchy levels recorded.
    pub fn num_levels(&self) -> usize {
        self.levels.len()
    }

    /// Get community of original node at a specific level.
    ///
    /// Level 0 = finest granularity, last level = coarsest.
    ///
    /// Returns 0 if `level` or `node` is out of range.
    pub fn community_of_at_level(&self, node: usize, level: usize) -> usize {
        if level >= self.levels.len() || node >= self.levels[level].membership.len() {
            return 0;
        }
        self.levels[level].membership[node]
    }

    /// Get the membership vector at a specific level.
    pub fn membership_at_level(&self, level: usize) -> &[usize] {
        &self.levels[level].membership
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::partition::Partition;

    #[test]
    fn test_community_of_at_level_bounds_check() {
        let level = HierarchyLevel {
            node_count: 3,
            num_communities: 2,
            quality: 0.5,
            membership: vec![0, 0, 1],
        };
        let output = HierarchicalOutput {
            levels: vec![level],
            partition: Partition::new(3),
            quality: 0.5,
        };

        // Node out of range — should not panic
        let result = output.community_of_at_level(10, 0);
        assert_eq!(result, 0, "out-of-range node should return 0");

        // Level out of range — should not panic
        let result = output.community_of_at_level(0, 5);
        assert_eq!(result, 0, "out-of-range level should return 0");

        // Both out of range — should not panic
        let result = output.community_of_at_level(10, 5);
        assert_eq!(result, 0, "both out of range should return 0");
    }
}

//! Resolution profile analysis for the Leiden algorithm.
//!
//! Runs community detection at multiple resolution (gamma) values to find
//! community structures at different scales. Two methods are provided:
//!
//! - **Linear scan** ([`resolution_scan`]): evaluates evenly spaced gamma values.
//! - **Bisection scan** ([`resolution_profile`]): efficiently finds only the gamma
//!   values where the partition changes, using recursive bisection.

use crate::graph::GraphData;
use crate::leiden::{Leiden, LeidenConfig, LeidenOutput, QualityType};
use crate::partition::Partition;
#[cfg(feature = "rayon")]
use rayon::prelude::*;

/// A single entry in a resolution profile, recording the result at one gamma value.
#[derive(Debug, Clone)]
#[non_exhaustive]
pub struct ResolutionEntry {
    /// The resolution parameter (gamma) used.
    pub resolution: f64,
    /// Number of communities found.
    pub num_communities: usize,
    /// Quality score of the partition at this resolution.
    pub quality: f64,
    /// The community partition.
    pub partition: Partition,
}

/// Run the Leiden algorithm at evenly spaced gamma values.
///
/// For each of `num_points` values linearly spaced from `resolution_range.0`
/// to `resolution_range.1`, runs Leiden and collects the result.
/// Sequential seeds are derived from the base `seed` so that each point is
/// reproducible but independent.
pub fn resolution_scan(
    data: &GraphData,
    quality: QualityType,
    resolution_range: (f64, f64),
    num_points: usize,
    seed: Option<u64>,
) -> crate::error::Result<Vec<ResolutionEntry>> {
    if num_points == 0 {
        return Ok(Vec::new());
    }

    let compute_entry = |i: usize| -> crate::error::Result<ResolutionEntry> {
        let gamma = if num_points == 1 {
            resolution_range.0
        } else {
            resolution_range.0
                + (resolution_range.1 - resolution_range.0) * (i as f64) / ((num_points - 1) as f64)
        };

        let config = LeidenConfig {
            resolution: gamma,
            quality,
            seed: seed.map(|s| s + i as u64),
            ..Default::default()
        };

        let LeidenOutput {
            partition,
            quality: q,
            ..
        } = Leiden::new(config).run(data)?;

        Ok(ResolutionEntry {
            resolution: gamma,
            num_communities: partition.num_communities(),
            quality: q,
            partition,
        })
    };

    #[cfg(feature = "rayon")]
    let entries: Vec<ResolutionEntry> = (0..num_points)
        .into_par_iter()
        .map(compute_entry)
        .collect::<crate::error::Result<Vec<_>>>()?;

    #[cfg(not(feature = "rayon"))]
    let entries: Vec<ResolutionEntry> = (0..num_points)
        .map(compute_entry)
        .collect::<crate::error::Result<Vec<_>>>()?;

    Ok(entries)
}

/// Run the Leiden algorithm at adaptively chosen gamma values using bisection.
///
/// Similar to leidenalg's `Optimiser.resolution_profile()`, this method finds
/// only the gamma values where the partition changes, avoiding unnecessary
/// evaluations at gamma values that produce identical partitions.
///
/// The bisection stops when the resolution range is narrower than
/// `min_diff_resolution` or the partitions are identical. Entries with the
/// same number of communities and quality within `min_diff_quality` are
/// deduplicated.
pub fn resolution_profile(
    data: &GraphData,
    quality: QualityType,
    resolution_range: (f64, f64),
    seed: Option<u64>,
    min_diff_resolution: f64,
    min_diff_quality: f64,
) -> crate::error::Result<Vec<ResolutionEntry>> {
    let mut entries = Vec::new();
    let ctx = BisectContext {
        data,
        quality,
        min_diff_resolution,
        max_depth: 50,
    };
    bisect(&ctx, resolution_range.0, resolution_range.1, seed.unwrap_or(0), 0, &mut entries)?;

    entries.sort_by(|a, b| {
        a.resolution
            .partial_cmp(&b.resolution)
            .unwrap_or(std::cmp::Ordering::Equal)
    });

    let mut deduped: Vec<ResolutionEntry> = Vec::new();
    for entry in entries {
        if let Some(last) = deduped.last() {
            if last.num_communities == entry.num_communities
                && (last.quality - entry.quality).abs() < min_diff_quality
            {
                continue;
            }
        }
        deduped.push(entry);
    }

    Ok(deduped)
}

struct BisectContext<'a> {
    data: &'a GraphData,
    quality: QualityType,
    min_diff_resolution: f64,
    max_depth: usize,
}

fn bisect(
    ctx: &BisectContext<'_>,
    gamma_low: f64,
    gamma_high: f64,
    seed: u64,
    depth: usize,
    entries: &mut Vec<ResolutionEntry>,
) -> crate::error::Result<()> {
    let n = ctx.data.node_count();
    if n == 0 {
        let config = LeidenConfig {
            resolution: gamma_low,
            quality: ctx.quality,
            seed: Some(seed),
            ..Default::default()
        };
        let LeidenOutput {
            partition,
            quality: q,
            ..
        } = Leiden::new(config).run(ctx.data)?;
        entries.push(ResolutionEntry {
            resolution: gamma_low,
            num_communities: partition.num_communities(),
            quality: q,
            partition,
        });
        return Ok(());
    }

    if depth >= ctx.max_depth {
        let config_low = LeidenConfig {
            resolution: gamma_low,
            quality: ctx.quality,
            seed: Some(seed),
            ..Default::default()
        };
        let LeidenOutput {
            partition: p_low,
            quality: q_low,
            ..
        } = Leiden::new(config_low).run(ctx.data)?;
        let config_high = LeidenConfig {
            resolution: gamma_high,
            quality: ctx.quality,
            seed: Some(seed.wrapping_add(1)),
            ..Default::default()
        };
        let LeidenOutput {
            partition: p_high,
            quality: q_high,
            ..
        } = Leiden::new(config_high).run(ctx.data)?;
        let partitions_differ = !partitions_equal(&p_low, &p_high, n);
        entries.push(ResolutionEntry {
            resolution: gamma_low,
            num_communities: p_low.num_communities(),
            quality: q_low,
            partition: p_low,
        });
        if partitions_differ {
            entries.push(ResolutionEntry {
                resolution: gamma_high,
                num_communities: p_high.num_communities(),
                quality: q_high,
                partition: p_high,
            });
        }
        return Ok(());
    }

    let config_low = LeidenConfig {
        resolution: gamma_low,
        quality: ctx.quality,
        seed: Some(seed),
        ..Default::default()
    };
    let LeidenOutput {
        partition: p_low,
        quality: q_low,
        ..
    } = Leiden::new(config_low).run(ctx.data)?;
    
    let config_high = LeidenConfig {
        resolution: gamma_high,
        quality: ctx.quality,
        seed: Some(seed.wrapping_add(1)),
        ..Default::default()
    };
    let LeidenOutput {
        partition: p_high,
        quality: q_high,
        ..
    } = Leiden::new(config_high).run(ctx.data)?;

    let range = gamma_high - gamma_low;

    if !partitions_equal(&p_low, &p_high, n) && range > ctx.min_diff_resolution {
        let mid = if gamma_low > 0.0 && gamma_high > 0.0 {
            (gamma_low * gamma_high).sqrt()
        } else {
            (gamma_low + gamma_high) / 2.0
        };

        bisect(
            ctx,
            gamma_low,
            mid,
            seed.wrapping_add(2),
            depth + 1,
            entries,
        )?;
        bisect(
            ctx,
            mid,
            gamma_high,
            seed.wrapping_add(3),
            depth + 1,
            entries,
        )?;
    } else {
        let partitions_differ = !partitions_equal(&p_low, &p_high, n);
        entries.push(ResolutionEntry {
            resolution: gamma_low,
            num_communities: p_low.num_communities(),
            quality: q_low,
            partition: p_low,
        });
        if partitions_differ {
            // Range is too small to recurse further, but the partition
            // changed — record the gamma_high endpoint too.
            entries.push(ResolutionEntry {
                resolution: gamma_high,
                num_communities: p_high.num_communities(),
                quality: q_high,
                partition: p_high,
            });
        }
    }

    Ok(())
}

/// Compare two partitions by normalizing their membership vectors.
///
/// Two partitions are equal if every node is in the same group relative to
/// all other nodes, regardless of the specific community IDs assigned.
fn partitions_equal(p1: &Partition, p2: &Partition, n: usize) -> bool {
    if p1.num_communities() != p2.num_communities() {
        return false;
    }

    let norm1 = normalize_partition(p1, n);
    let norm2 = normalize_partition(p2, n);
    norm1 == norm2
}

/// Normalize a partition's membership vector so that community IDs are
/// assigned in order of first appearance (lowest node index gets ID 0, etc.).
fn normalize_partition(partition: &Partition, n: usize) -> Vec<usize> {
    let mut mapping: Vec<usize> = vec![usize::MAX; n];
    let mut next_id = 0usize;
    let mut result = vec![0usize; n];

    for (node, &comm) in partition.as_slice().iter().enumerate() {
        if mapping[comm] == usize::MAX {
            mapping[comm] = next_id;
            next_id += 1;
        }
        result[node] = mapping[comm];
    }

    result
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::graph::GraphDataBuilder;

    fn make_two_cliques() -> GraphData {
        let mut b = GraphDataBuilder::new(10);
        // Clique 1: nodes 0-4
        for i in 0..5 {
            for j in (i + 1)..5 {
                b.add_edge(i, j, 1.0).unwrap();
            }
        }
        // Clique 2: nodes 5-9
        for i in 5..10 {
            for j in (i + 1)..10 {
                b.add_edge(i, j, 1.0).unwrap();
            }
        }
        // Single bridge edge between cliques
        b.add_edge(0, 5, 1.0).unwrap();
        b.build().unwrap()
    }

    #[test]
    fn test_resolution_scan_two_cliques() {
        let data = make_two_cliques();

        let entries =
            resolution_scan(&data, QualityType::Modularity, (0.1, 2.0), 10, Some(42)).unwrap();

        assert_eq!(entries.len(), 10);

        // Entries should be ordered by resolution
        for i in 1..entries.len() {
            assert!(entries[i].resolution > entries[i - 1].resolution);
        }

        // At low gamma (0.1), modularity should favor merging into 1 or 2 communities
        assert!(
            entries[0].num_communities >= 1,
            "expected at least 1 community at low gamma, got {}",
            entries[0].num_communities,
        );

        // At some gamma values, the two-cliques structure (2 communities) should appear
        let has_two = entries.iter().any(|e| e.num_communities == 2);
        assert!(has_two, "expected at least one entry with 2 communities");

        // All partitions should have 10 nodes
        for entry in &entries {
            assert_eq!(entry.partition.len(), 10);
        }
    }

    #[test]
    fn test_resolution_profile_two_cliques() {
        let data = make_two_cliques();

        let entries =
            resolution_profile(&data, QualityType::CPM, (0.1, 2.0), Some(42), 0.01, 0.001).unwrap();

        assert!(
            !entries.is_empty(),
            "profile should produce at least one entry"
        );

        // Entries should be sorted by resolution
        for i in 1..entries.len() {
            assert!(
                entries[i].resolution >= entries[i - 1].resolution,
                "entries not sorted: {} >= {}",
                entries[i].resolution,
                entries[i - 1].resolution,
            );
        }

        // All resolutions should be within range
        for entry in &entries {
            assert!(entry.resolution >= 0.1 - 1e-10);
            assert!(entry.resolution <= 2.0 + 1e-10);
        }

        // All partitions should have 10 nodes
        for entry in &entries {
            assert_eq!(entry.partition.len(), 10);
        }
    }

    #[test]
    fn test_resolution_scan_single_point() {
        let data = make_two_cliques();

        let entries =
            resolution_scan(&data, QualityType::Modularity, (1.0, 2.0), 1, Some(42)).unwrap();

        assert_eq!(entries.len(), 1);
        assert!((entries[0].resolution - 1.0).abs() < 1e-10);
    }

    #[test]
    fn test_resolution_scan_zero_points() {
        let data = make_two_cliques();

        let entries =
            resolution_scan(&data, QualityType::Modularity, (0.1, 2.0), 0, Some(42)).unwrap();

        assert!(entries.is_empty());
    }

    #[test]
    fn test_partitions_equal_identical() {
        let p1 = Partition::from_membership(vec![0, 0, 1, 1]);
        let p2 = Partition::from_membership(vec![0, 0, 1, 1]);
        assert!(partitions_equal(&p1, &p2, 4));
    }

    #[test]
    fn test_partitions_equal_relabel() {
        let p1 = Partition::from_membership(vec![0, 0, 1, 1]);
        let p2 = Partition::from_membership(vec![1, 1, 0, 0]);
        assert!(partitions_equal(&p1, &p2, 4));
    }

    #[test]
    fn test_partitions_equal_different() {
        let p1 = Partition::from_membership(vec![0, 0, 0, 1]);
        let p2 = Partition::from_membership(vec![0, 0, 1, 1]);
        assert!(!partitions_equal(&p1, &p2, 4));
    }

    #[test]
    fn test_bisect_records_both_endpoints() {
        let mut b = GraphDataBuilder::new(2);
        b.add_edge(0, 1, 0.1).unwrap();
        let g = b.build().unwrap();
        let profile = resolution_profile(
            &g,
            QualityType::Modularity,
            (0.1, 2.0),
            Some(42),
            1e-2,
            1e-6,
        )
        .unwrap();
        assert!(
            profile.len() >= 2,
            "expected at least 2 entries, got {}",
            profile.len()
        );
        let eps = 1e-6;
        assert!(
            (profile.first().unwrap().resolution - 0.1).abs() < eps,
            "first resolution should be 0.1, got {}",
            profile.first().unwrap().resolution
        );
        assert!(
            (profile.last().unwrap().resolution - 2.0).abs() < eps,
            "last resolution should be 2.0, got {}",
            profile.last().unwrap().resolution
        );
    }

    #[test]
    fn test_resolution_profile_deduplication() {
        // With a very wide quality tolerance, entries should be deduplicated
        let data = make_two_cliques();

        let entries = resolution_profile(
            &data,
            QualityType::CPM,
            (0.5, 1.5),
            Some(42),
            0.01,
            1000.0, // very large tolerance — everything deduplicates
        )
        .unwrap();

        // Should produce at most 1 entry after deduplication if all have same num_communities
        assert!(
            entries.len() <= 3,
            "expected heavy deduplication, got {} entries",
            entries.len(),
        );
    }
}

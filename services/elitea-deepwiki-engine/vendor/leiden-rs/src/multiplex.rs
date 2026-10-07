//! Multiplex/multilayer network community detection.

use rand::rngs::StdRng;
use rand::SeedableRng;
#[cfg(feature = "rayon")]
use rayon::prelude::*;
use rustc_hash::FxHashMap;

use crate::algorithm;
use crate::leiden::QualityType;
use crate::partition::Partition;
use crate::quality::{GraphData, Modularity, QualityFunction};

/// Configuration for multiplex Leiden optimization.
#[derive(Debug, Clone)]
pub struct MultiplexConfig {
    /// Maximum number of Leiden iterations per level.
    pub max_iterations: usize,
    /// Resolution parameter γ for all layers.
    pub resolution: f64,
    /// Optional RNG seed for reproducible results.
    pub seed: Option<u64>,
    /// Quality function to optimize (applied to all layers).
    pub quality: QualityType,
    /// Convergence threshold.
    pub epsilon: f64,
    /// Maximum community size (0 = unlimited).
    pub max_comm_size: usize,
    /// Weight for each layer. Length must match the number of layers.
    /// Negative weights invert the quality (push nodes apart).
    pub layer_weights: Vec<f64>,
    /// Minimum edge slots (CSR entries) for parallel aggregation (default: 10000).
    pub parallel_aggregation_threshold: Option<usize>,
}

impl Default for MultiplexConfig {
    fn default() -> Self {
        Self {
            max_iterations: 100,
            resolution: 1.0,
            seed: None,
            quality: QualityType::default(),
            epsilon: 1e-10,
            max_comm_size: 0,
            layer_weights: Vec::new(),
            parallel_aggregation_threshold: None,
        }
    }
}

impl MultiplexConfig {
    /// Validate configuration parameters.
    ///
    /// Returns `Ok(())` if all parameters are valid, or an `InvalidParameter`
    /// error describing the first invalid parameter found.
    pub fn validate(&self) -> crate::error::Result<()> {
        if self.max_iterations == 0 {
            return Err(crate::error::LeidenError::InvalidParameter {
                message: "max_iterations must be > 0".to_string(),
            });
        }
        if !self.resolution.is_finite() || self.resolution < 0.0 {
            return Err(crate::error::LeidenError::InvalidParameter {
                message: format!(
                    "resolution must be finite and non-negative, got {}",
                    self.resolution
                ),
            });
        }
        if !self.epsilon.is_finite() || self.epsilon <= 0.0 {
            return Err(crate::error::LeidenError::InvalidParameter {
                message: format!("epsilon must be finite and positive, got {}", self.epsilon),
            });
        }
        if !self.layer_weights.iter().all(|w| w.is_finite()) {
            return Err(crate::error::LeidenError::InvalidParameter {
                message: format!(
                    "all layer_weights must be finite, got {:?}",
                    self.layer_weights
                ),
            });
        }
        Ok(())
    }
}

/// Result of multiplex Leiden optimization.
#[derive(Debug, Clone)]
#[non_exhaustive]
pub struct MultiplexOutput {
    /// The community partition (same for all layers).
    pub partition: Partition,
    /// Total weighted quality score: Σ w_l * Q_l.
    pub quality: f64,
    /// Per-layer quality scores.
    pub layer_qualities: Vec<f64>,
}

/// Run Leiden community detection on multiple graph layers simultaneously.
///
/// All layers must have the same number of nodes. The algorithm optimizes a
/// weighted sum of quality functions: Q = Σ_l w_l * Q_l.
///
/// # Arguments
/// * `layers` - Graph data for each layer (one GraphData per layer)
/// * `config` - Multiplex configuration including layer weights
///
/// # Returns
/// A `MultiplexOutput` with the shared partition and quality scores.
pub fn run_multiplex(
    layers: &[GraphData],
    config: &MultiplexConfig,
) -> crate::error::Result<MultiplexOutput> {
    config.validate()?;
    if layers.is_empty() {
        return Err(crate::error::LeidenError::InvalidParameter {
            message: "at least one layer is required".to_string(),
        });
    }

    let n = layers[0].node_count();
    for (i, layer) in layers.iter().enumerate() {
        if layer.node_count() != n {
            return Err(crate::error::LeidenError::InvalidParameter {
                message: format!(
                    "layer {} has {} nodes, expected {} (all layers must share the same vertex set)",
                    i,
                    layer.node_count(),
                    n
                ),
            });
        }
    }

    let layer_weights: Vec<f64> = if config.layer_weights.is_empty() {
        vec![1.0; layers.len()]
    } else if config.layer_weights.len() != layers.len() {
        return Err(crate::error::LeidenError::InvalidParameter {
            message: format!(
                "layer_weights has {} entries but there are {} layers",
                config.layer_weights.len(),
                layers.len()
            ),
        });
    } else {
        config.layer_weights.clone()
    };

    if n == 0 {
        return Ok(MultiplexOutput {
            partition: Partition::new(0),
            quality: 0.0,
            layer_qualities: vec![0.0; layers.len()],
        });
    }

    let modularity = Modularity::with_resolution(config.resolution);
    let cpm = crate::quality::CPM::new(config.resolution);
    let rbconfig = crate::quality::RBConfiguration::with_resolution(config.resolution);
    let rber = crate::quality::RBER::new(config.resolution);
    let quality: &(dyn QualityFunction + Sync) = match config.quality {
        QualityType::Modularity => &modularity,
        QualityType::CPM => &cpm,
        QualityType::RBConfiguration => &rbconfig,
        QualityType::RBER => &rber,
    };

    let original_n = n;
    let mut partition = Partition::new(n);
    let mut flat_mapping: Vec<usize> = (0..n).collect();

    let mut rng = match config.seed {
        Some(seed) => StdRng::seed_from_u64(seed),
        None => StdRng::from_rng(&mut rand::rng()),
    };

    let mut local_moving_buffers =
        algorithm::LocalMovingBuffers::new(n, layers.len());
    let mut refinement_buffers =
        algorithm::RefinementBuffers::new(n, layers.len());

    let mut agg_layer: Option<GraphData> = None;

    for _iter in 0..config.max_iterations {
        let (current_layers, current_weights): (&[GraphData], &[f64]) = match &agg_layer {
            Some(data) => (std::slice::from_ref(data), &[1.0][..]),
            None => (layers, &layer_weights),
        };

        let q_before = weighted_quality(current_layers, current_weights, &partition, quality);

        let changed = algorithm::local_moving_generic(
            current_layers,
            current_weights,
            &mut partition,
            quality,
            &mut rng,
            &algorithm::MovingConfig {
                max_comm_size: config.max_comm_size,
                epsilon: config.epsilon,
            },
            &mut local_moving_buffers,
        );
        if !changed {
            break;
        }
        partition.renumber();

        let q_after = weighted_quality(current_layers, current_weights, &partition, quality);
        if (q_after - q_before).abs() < config.epsilon {
            break;
        }

        let refined = multiplex_refinement(
            current_layers,
            current_weights,
            &partition,
            quality,
            &mut rng,
            config.epsilon,
            &mut refinement_buffers,
        );

        let (agg_data, orig_to_agg, agg_initial) =
            multiplex_aggregate(current_layers, &refined, &partition, config.parallel_aggregation_threshold)?;

        for orig_node in 0..original_n {
            flat_mapping[orig_node] = orig_to_agg[flat_mapping[orig_node]];
        }

        if agg_data.node_count() <= 1 {
            break;
        }

        agg_layer = Some(agg_data);
        partition = agg_initial;
    }

    let mut result = Partition::from_membership(vec![0; original_n]);
    for (orig_node, &agg_node) in flat_mapping.iter().enumerate() {
        let comm = partition.community_of(agg_node);
        result.move_node(orig_node, comm);
    }
    result.renumber();

    let layer_qualities: Vec<f64> = layers
        .iter()
        .map(|layer| quality.total_quality(layer, &result))
        .collect();
    let total_quality: f64 = layer_weights
        .iter()
        .zip(layer_qualities.iter())
        .map(|(w, q)| w * q)
        .sum();

    Ok(MultiplexOutput {
        partition: result,
        quality: total_quality,
        layer_qualities,
    })
}

fn weighted_quality(
    layers: &[GraphData],
    layer_weights: &[f64],
    partition: &Partition,
    quality: &dyn QualityFunction,
) -> f64 {
    layer_weights
        .iter()
        .zip(layers.iter())
        .map(|(w, layer)| w * quality.total_quality(layer, partition))
        .sum()
}

fn multiplex_refinement(
    layers: &[GraphData],
    layer_weights: &[f64],
    partition: &Partition,
    quality: &(dyn QualityFunction + Sync),
    rng: &mut StdRng,
    epsilon: f64,
    buffers: &mut algorithm::RefinementBuffers,
) -> Partition {
    let m: f64 = layers.iter().map(|l| l.total_weight()).sum();
    if m <= 0.0 {
        return Partition::new(layers[0].node_count());
    }
    algorithm::refinement_generic(
        layers[0].node_count(),
        layers.len(),
        partition,
        rng,
        buffers,
        |community, nodes, buf| {
            algorithm::refine_community_generic(
                layers,
                layer_weights,
                partition,
                quality,
                &algorithm::CommunitySubset { community, nodes },
                &algorithm::MovingConfig {
                    max_comm_size: 0,
                    epsilon,
                },
                buf,
            )
        },
    )
}

fn multiplex_aggregate_edges_sequential(
    layers: &[GraphData],
    orig_to_agg: &[usize],
    n: usize,
) -> FxHashMap<(usize, usize), f64> {
    let mut agg_edges: FxHashMap<(usize, usize), f64> = FxHashMap::default();
    for layer in layers {
        let directed = layer.is_directed();
        for u in 0..n {
            algorithm::aggregate_node_edges_into(layer, u, orig_to_agg, directed, &mut agg_edges);
        }
    }
    agg_edges
}

#[cfg(feature = "rayon")]
fn multiplex_aggregate_edges_parallel(
    layers: &[GraphData],
    orig_to_agg: &[usize],
    n: usize,
) -> FxHashMap<(usize, usize), f64> {
    (0..n)
        .into_par_iter()
        .fold(FxHashMap::<(usize, usize), f64>::default, |mut local, u| {
            for layer in layers {
                let directed = layer.is_directed();
                algorithm::aggregate_node_edges_into(layer, u, orig_to_agg, directed, &mut local);
            }
            local
        })
        .reduce(
            FxHashMap::<(usize, usize), f64>::default,
            |mut acc, local| {
                for (k, v) in local {
                    *acc.entry(k).or_default() += v;
                }
                acc
            },
        )
}

fn multiplex_aggregate(
    layers: &[GraphData],
    refined_partition: &Partition,
    coarse_partition: &Partition,
    parallel_aggregation_threshold: Option<usize>,
) -> crate::error::Result<(GraphData, Vec<usize>, Partition)> {
    let n = layers[0].node_count();
    let (orig_to_agg, agg_n) = algorithm::build_orig_to_agg_mapping(n, refined_partition);

    let any_directed = layers.iter().any(|l| l.is_directed());
    let agg_edges_map: FxHashMap<(usize, usize), f64> = {
        #[cfg(feature = "rayon")]
        {
            let edge_slots: usize = layers.iter().map(|l| l.out_offsets[n]).sum();
            let threshold = parallel_aggregation_threshold.unwrap_or(crate::parallel::AGG_PARALLEL_THRESHOLD);
            if edge_slots >= threshold {
                multiplex_aggregate_edges_parallel(layers, &orig_to_agg, n)
            } else {
                multiplex_aggregate_edges_sequential(layers, &orig_to_agg, n)
            }
        }
        #[cfg(not(feature = "rayon"))]
        {
            let _ = parallel_aggregation_threshold;
            multiplex_aggregate_edges_sequential(layers, &orig_to_agg, n)
        }
    };

    algorithm::build_aggregated_graph(
        orig_to_agg,
        agg_n,
        any_directed,
        agg_edges_map,
        coarse_partition,
        |orig| layers[0].node_weight(orig),
    )
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::graph::GraphDataBuilder;

    fn build_graph(n: usize, edges: &[(usize, usize, f64)]) -> crate::graph::GraphData {
        let mut b = GraphDataBuilder::new(n);
        for &(u, v, w) in edges {
            b.add_edge(u, v, w).unwrap();
        }
        b.build().unwrap()
    }

    #[test]
    fn test_multiplex_two_layers() {
        let edges1 = [
            (0, 1, 1.0),
            (1, 2, 1.0),
            (0, 2, 1.0),
            (3, 4, 1.0),
            (4, 5, 1.0),
            (3, 5, 1.0),
            (2, 3, 1.0),
        ];
        let layer1 = build_graph(6, &edges1);

        let edges2 = [
            (0, 1, 1.0),
            (1, 2, 1.0),
            (0, 2, 1.0),
            (3, 4, 1.0),
            (4, 5, 1.0),
            (3, 5, 1.0),
            (1, 4, 1.0),
        ];
        let layer2 = build_graph(6, &edges2);

        let config = MultiplexConfig {
            seed: Some(42),
            layer_weights: vec![1.0, 1.0],
            ..Default::default()
        };

        let result = run_multiplex(&[layer1, layer2], &config).unwrap();
        assert!(result.partition.num_communities() >= 1);
        assert_eq!(result.layer_qualities.len(), 2);
    }

    #[test]
    fn test_multiplex_single_layer_matches_standard() {
        let edges = [
            (0, 1, 1.0),
            (1, 2, 1.0),
            (0, 2, 1.0),
            (3, 4, 1.0),
            (4, 5, 1.0),
            (3, 5, 1.0),
            (2, 3, 1.0),
        ];
        let layer = build_graph(6, &edges);

        let config = MultiplexConfig {
            seed: Some(42),
            layer_weights: vec![1.0],
            ..Default::default()
        };

        let result = run_multiplex(&[layer], &config).unwrap();
        assert!(result.partition.num_communities() >= 1);
    }

    #[test]
    fn test_multiplex_mismatched_nodes() {
        let layer1 = build_graph(2, &[(0, 1, 1.0)]);
        let layer2 = build_graph(3, &[(0, 1, 1.0), (1, 2, 1.0)]);

        let config = MultiplexConfig {
            layer_weights: vec![1.0, 1.0],
            ..Default::default()
        };

        assert!(run_multiplex(&[layer1, layer2], &config).is_err());
    }

    #[test]
    fn test_multiplex_empty_layers() {
        let config = MultiplexConfig::default();
        assert!(run_multiplex(&[], &config).is_err());
    }

    #[test]
    fn test_multiplex_per_layer_stats_correctness() {
        // Two layers with different degree distributions to expose the
        // accumulated-sigma bug in modularity delta computation.
        let mut b1 = GraphDataBuilder::new(4);
        b1.add_edge(0, 1, 1.0).unwrap();
        b1.add_edge(1, 2, 1.0).unwrap();
        b1.add_edge(0, 2, 1.0).unwrap();

        let mut b2 = GraphDataBuilder::new(4);
        b2.add_edge(0, 1, 2.0).unwrap();
        b2.add_edge(1, 2, 2.0).unwrap();
        b2.add_edge(2, 3, 2.0).unwrap();

        let config = MultiplexConfig {
            seed: Some(42),
            layer_weights: vec![1.0, 0.5],
            quality: QualityType::Modularity,
            ..Default::default()
        };

        let result =
            run_multiplex(&[b1.build().unwrap(), b2.build().unwrap()], &config).unwrap();

        assert!(
            result.quality >= 0.0,
            "total quality should be non-negative, got {}",
            result.quality,
        );
        assert_eq!(result.layer_qualities.len(), 2);
        assert!(
            result.layer_qualities.iter().all(|&q| q.is_finite()),
            "per-layer qualities should be finite: {:?}",
            result.layer_qualities,
        );
    }

    #[test]
    fn test_multiplex_weighted_layers() {
        let edges1 = [
            (0, 1, 1.0),
            (1, 2, 1.0),
            (0, 2, 1.0),
            (3, 4, 1.0),
            (4, 5, 1.0),
            (3, 5, 1.0),
            (2, 3, 0.01),
        ];
        let edges2 = [(0, 3, 1.0), (1, 4, 1.0), (2, 5, 1.0)];
        let layer1 = build_graph(6, &edges1);
        let layer2 = build_graph(6, &edges2);

        let config = MultiplexConfig {
            seed: Some(42),
            layer_weights: vec![10.0, 0.1],
            ..Default::default()
        };

        let result = run_multiplex(&[layer1, layer2], &config).unwrap();
        assert!(
            result.partition.num_communities() >= 2,
            "expected >= 2 communities, got {}",
            result.partition.num_communities()
        );
    }

    #[test]
    fn test_validate_resolution_negative() {
        let config = MultiplexConfig {
            resolution: -0.1,
            ..Default::default()
        };
        let err = config.validate().unwrap_err();
        assert!(
            err.to_string().contains("resolution"),
            "expected resolution error, got: {}",
            err
        );
    }

    #[test]
    fn test_validate_resolution_nan() {
        let config = MultiplexConfig {
            resolution: f64::NAN,
            ..Default::default()
        };
        let err = config.validate().unwrap_err();
        assert!(
            err.to_string().contains("resolution"),
            "expected resolution error, got: {}",
            err
        );
    }

    #[test]
    fn test_validate_epsilon_zero() {
        let config = MultiplexConfig {
            epsilon: 0.0,
            ..Default::default()
        };
        let err = config.validate().unwrap_err();
        assert!(
            err.to_string().contains("epsilon"),
            "expected epsilon error, got: {}",
            err
        );
    }

    #[test]
    fn test_validate_epsilon_negative() {
        let config = MultiplexConfig {
            epsilon: -1e-10,
            ..Default::default()
        };
        let err = config.validate().unwrap_err();
        assert!(
            err.to_string().contains("epsilon"),
            "expected epsilon error, got: {}",
            err
        );
    }

    #[test]
    fn test_validate_epsilon_nan() {
        let config = MultiplexConfig {
            epsilon: f64::NAN,
            ..Default::default()
        };
        let err = config.validate().unwrap_err();
        assert!(
            err.to_string().contains("epsilon"),
            "expected epsilon error, got: {}",
            err
        );
    }

    #[test]
    fn test_validate_max_iterations_zero() {
        let config = MultiplexConfig {
            max_iterations: 0,
            ..Default::default()
        };
        let err = config.validate().unwrap_err();
        assert!(
            err.to_string().contains("max_iterations"),
            "expected max_iterations error, got: {}",
            err
        );
    }

    #[test]
    fn test_validate_layer_weights_nan() {
        let config = MultiplexConfig {
            layer_weights: vec![1.0, f64::NAN],
            ..Default::default()
        };
        let err = config.validate().unwrap_err();
        assert!(
            err.to_string().contains("layer_weights"),
            "expected layer_weights error, got: {}",
            err
        );
    }

    #[test]
    fn test_validate_layer_weights_inf() {
        let config = MultiplexConfig {
            layer_weights: vec![1.0, f64::INFINITY],
            ..Default::default()
        };
        let err = config.validate().unwrap_err();
        assert!(
            err.to_string().contains("layer_weights"),
            "expected layer_weights error, got: {}",
            err
        );
    }

    #[test]
    fn test_validate_valid() {
        let config = MultiplexConfig {
            layer_weights: vec![1.0, 2.0],
            ..Default::default()
        };
        config.validate().expect("valid config should pass validation");
    }
}

use criterion::{black_box, criterion_group, criterion_main, BenchmarkId, Criterion};
use leiden_rs::fluid_communities::{FluidCommunities, FluidCommunitiesConfig};
use leiden_rs::graph::GraphDataBuilder;
use leiden_rs::generators::{generate_er_graph, generate_planted_partition, generate_sbm};
use leiden_rs::infomap::{Infomap, InfomapConfig};
use leiden_rs::label_propagation::{LabelPropagation, LabelPropagationConfig};
use leiden_rs::{load_edgelist, GraphData, Leiden, LeidenConfig};

// Synthetic clustered graphs
// ---------------------------------------------------------------------------

fn make_clustered_graph(
    n_clusters: usize,
    nodes_per_cluster: usize,
    intra_weight: f64,
    inter_weight: f64,
) -> GraphData {
    let total = n_clusters * nodes_per_cluster;
    let mut b = GraphDataBuilder::new(total);
    for c in 0..n_clusters {
        let base = c * nodes_per_cluster;
        for i in 0..nodes_per_cluster {
            for j in (i + 1)..nodes_per_cluster {
                b.add_edge(base + i, base + j, intra_weight).unwrap();
            }
        }
    }
    for i in 0..n_clusters {
        for j in (i + 1)..n_clusters {
            b.add_edge(i * nodes_per_cluster, j * nodes_per_cluster, inter_weight)
                .unwrap();
        }
    }
    b.build().unwrap()
}

fn make_directed_clustered_graph(
    n_clusters: usize,
    nodes_per_cluster: usize,
    intra_weight: f64,
    inter_weight: f64,
) -> GraphData {
    let total = n_clusters * nodes_per_cluster;
    let mut b = GraphDataBuilder::new(total).directed();
    for c in 0..n_clusters {
        let base = c * nodes_per_cluster;
        for i in 0..nodes_per_cluster {
            for j in 0..nodes_per_cluster {
                if i != j {
                    b.add_edge(base + i, base + j, intra_weight).unwrap();
                }
            }
        }
    }
    for i in 0..n_clusters {
        for j in 0..n_clusters {
            if i != j {
                b.add_edge(i * nodes_per_cluster, j * nodes_per_cluster, inter_weight)
                    .unwrap();
            }
        }
    }
    b.build().unwrap()
}

fn make_clustered_graph_fa(
    n_clusters: usize,
    nodes_per_cluster: usize,
    intra_weight: f32,
    inter_weight: f32,
) -> fa_leiden_cd::Graph<String, ()> {
    let mut graph = fa_leiden_cd::Graph::<String, ()>::new();
    let mut nodes: Vec<Vec<usize>> = Vec::with_capacity(n_clusters);

    for c in 0..n_clusters {
        let mut cluster = Vec::with_capacity(nodes_per_cluster);
        for i in 0..nodes_per_cluster {
            cluster.push(graph.add_node(format!("n{}", c * nodes_per_cluster + i)));
        }
        nodes.push(cluster);
    }

    for cluster in &nodes {
        for i in 0..cluster.len() {
            for j in (i + 1)..cluster.len() {
                graph.add_edge(cluster[i], cluster[j], (), intra_weight);
            }
        }
    }

    for i in 0..n_clusters {
        for j in (i + 1)..n_clusters {
            graph.add_edge(nodes[i][0], nodes[j][0], (), inter_weight);
        }
    }

    graph
}

fn bench_small_rs(c: &mut Criterion) {
    let graph = make_clustered_graph(2, 5, 1.0, 0.1);
    let leiden = Leiden::new(LeidenConfig::default());
    c.bench_function("leiden_rs_small", |b| {
        b.iter(|| {
            let partition = leiden.run(&graph).unwrap().partition;
            black_box(partition.num_communities());
        })
    });
}

fn bench_small_fa(c: &mut Criterion) {
    let graph = make_clustered_graph_fa(2, 5, 1.0, 0.1);
    c.bench_function("fa_leiden_cd_small", |b| {
        b.iter(|| {
            let mut optimizer = fa_leiden_cd::TrivialModularityOptimizer {
                parallel_scale: 128,
                tol: 1e-11,
            };
            let hierarchy = graph.leiden(Some(100), &mut optimizer);
            black_box(hierarchy.node_data_slice().len());
        })
    });
}

fn bench_medium_rs(c: &mut Criterion) {
    let graph = make_clustered_graph(4, 25, 1.0, 0.1);
    let leiden = Leiden::new(LeidenConfig::default());
    c.bench_function("leiden_rs_medium", |b| {
        b.iter(|| {
            let partition = leiden.run(&graph).unwrap().partition;
            black_box(partition.num_communities());
        })
    });
}

fn bench_medium_fa(c: &mut Criterion) {
    let graph = make_clustered_graph_fa(4, 25, 1.0, 0.1);
    c.bench_function("fa_leiden_cd_medium", |b| {
        b.iter(|| {
            let mut optimizer = fa_leiden_cd::TrivialModularityOptimizer {
                parallel_scale: 128,
                tol: 1e-11,
            };
            let hierarchy = graph.leiden(Some(100), &mut optimizer);
            black_box(hierarchy.node_data_slice().len());
        })
    });
}

fn bench_large_rs(c: &mut Criterion) {
    let graph = make_clustered_graph(4, 50, 1.0, 0.1);
    let leiden = Leiden::new(LeidenConfig::default());
    c.bench_function("leiden_rs_large", |b| {
        b.iter(|| {
            let partition = leiden.run(&graph).unwrap().partition;
            black_box(partition.num_communities());
        })
    });
}

fn bench_large_fa(c: &mut Criterion) {
    let graph = make_clustered_graph_fa(4, 50, 1.0, 0.1);
    c.bench_function("fa_leiden_cd_large", |b| {
        b.iter(|| {
            let mut optimizer = fa_leiden_cd::TrivialModularityOptimizer {
                parallel_scale: 128,
                tol: 1e-11,
            };
            let hierarchy = graph.leiden(Some(100), &mut optimizer);
            black_box(hierarchy.node_data_slice().len());
        })
    });
}

// Real-world networks (from shared data/ directory)
// ---------------------------------------------------------------------------

struct NetworkData {
    name: &'static str,
    graph: GraphData,
    nodes: usize,
}

fn load_real_networks() -> Vec<NetworkData> {
    vec![
        NetworkData {
            name: "karate",
            graph: load_edgelist(include_str!("../data/karate.edgelist"), 34).unwrap(),
            nodes: 34,
        },
        NetworkData {
            name: "dolphins",
            graph: load_edgelist(include_str!("../data/dolphins.edgelist"), 62).unwrap(),
            nodes: 62,
        },
        NetworkData {
            name: "jazz",
            graph: load_edgelist(include_str!("../data/jazz.edgelist"), 198).unwrap(),
            nodes: 198,
        },
        NetworkData {
            name: "cora",
            graph: load_edgelist(include_str!("../data/cora.edgelist"), 2708).unwrap(),
            nodes: 2708,
        },
    ]
}

fn bench_real_networks_rs(c: &mut Criterion) {
    let networks = load_real_networks();
    let mut group = c.benchmark_group("real_networks");
    for net in &networks {
        let leiden = Leiden::new(LeidenConfig {
            seed: Some(42),
            ..Default::default()
        });
        group.bench_with_input(
            BenchmarkId::new("leiden_rs", net.name),
            &net.graph,
            |b, graph| {
                b.iter(|| {
                    let result = leiden.run(graph).unwrap();
                    black_box(result.partition.num_communities());
                });
            },
        );
    }
    group.finish();
}

fn bench_real_networks_fa(c: &mut Criterion) {
    let networks = load_real_networks();
    let mut group = c.benchmark_group("real_networks");
    for net in &networks {
        let data = match net.name {
            "karate" => include_str!("../data/karate.edgelist"),
            "dolphins" => include_str!("../data/dolphins.edgelist"),
            "jazz" => include_str!("../data/jazz.edgelist"),
            "cora" => include_str!("../data/cora.edgelist"),
            _ => "",
        };
        let mut graph = fa_leiden_cd::Graph::<String, ()>::new();
        let mut nodes = Vec::with_capacity(net.nodes);
        for i in 0..net.nodes {
            nodes.push(graph.add_node(format!("n{i}")));
        }
        for line in data.lines() {
            let line = line.trim();
            if line.is_empty() || line.starts_with('#') {
                continue;
            }
            let parts: Vec<&str> = line.split_whitespace().collect();
            if parts.len() < 2 {
                continue;
            }
            let src: usize = parts[0].parse().unwrap();
            let dst: usize = parts[1].parse().unwrap();
            if src < net.nodes && dst < net.nodes && src != dst {
                graph.add_edge(nodes[src], nodes[dst], (), 1.0);
            }
        }

        group.bench_with_input(
            BenchmarkId::new("fa_leiden_cd", net.name),
            &graph,
            |b, graph| {
                b.iter(|| {
                    let mut optimizer = fa_leiden_cd::TrivialModularityOptimizer {
                        parallel_scale: 128,
                        tol: 1e-11,
                    };
                    let hierarchy = graph.leiden(Some(100), &mut optimizer);
                    black_box(hierarchy.node_data_slice().len());
                });
            },
        );
    }
    group.finish();
}

fn bench_directed_medium_rs(c: &mut Criterion) {
    let graph = make_directed_clustered_graph(4, 25, 1.0, 0.1);
    let leiden = Leiden::new(LeidenConfig::default());
    c.bench_function("leiden_rs_directed_medium", |b| {
        b.iter(|| {
            let partition = leiden.run(&graph).unwrap().partition;
            black_box(partition.num_communities());
        })
    });
}

fn bench_directed_large_rs(c: &mut Criterion) {
    let graph = make_directed_clustered_graph(4, 50, 1.0, 0.1);
    let leiden = Leiden::new(LeidenConfig::default());
    c.bench_function("leiden_rs_directed_large", |b| {
        b.iter(|| {
            let partition = leiden.run(&graph).unwrap().partition;
            black_box(partition.num_communities());
        })
    });
}

// Label Propagation benchmarks
// ---------------------------------------------------------------------------

fn bench_label_propagation(c: &mut Criterion) {
    let mut group = c.benchmark_group("label_propagation");

    // Small graph (10 nodes, ~25 edges)
    let small = generate_er_graph(10, 0.5, Some(42)).unwrap();
    group.bench_function("small", |b| {
        b.iter(|| {
            let lp = LabelPropagation::new(LabelPropagationConfig::default());
            black_box(lp.run(black_box(&small)));
        })
    });

    // Medium graph (100 nodes, ~2475 edges)
    let medium = generate_er_graph(100, 0.5, Some(42)).unwrap();
    group.bench_function("medium", |b| {
        b.iter(|| {
            let lp = LabelPropagation::new(LabelPropagationConfig::default());
            black_box(lp.run(black_box(&medium)));
        })
    });

    // Large graph (200 nodes, ~9950 edges)
    let large = generate_er_graph(200, 0.5, Some(42)).unwrap();
    group.bench_function("large", |b| {
        b.iter(|| {
            let lp = LabelPropagation::new(LabelPropagationConfig::default());
            black_box(lp.run(black_box(&large)));
        })
    });

    group.finish();
}

// Louvain comparison (Leiden with and without refinement)
// ---------------------------------------------------------------------------

fn bench_louvain_comparison(c: &mut Criterion) {
    let mut group = c.benchmark_group("louvain_comparison");

    // Use planted partition graph for meaningful comparison
    let graph = generate_planted_partition(100, 4, 0.8, 0.05, Some(42))
        .unwrap()
        .0;

    group.bench_function("leiden", |b| {
        b.iter(|| {
            let leiden = Leiden::new(LeidenConfig {
                seed: Some(42),
                ..Default::default()
            });
            black_box(leiden.run(black_box(&graph)).unwrap());
        })
    });

    group.bench_function("louvain", |b| {
        b.iter(|| {
            let config = LeidenConfig {
                seed: Some(42),
                skip_refinement: true,
                ..Default::default()
            };
            let leiden = Leiden::new(config);
            black_box(leiden.run(black_box(&graph)).unwrap());
        })
    });

    group.finish();
}

// Infomap benchmarks
// ---------------------------------------------------------------------------

fn bench_infomap(c: &mut Criterion) {
    let mut group = c.benchmark_group("infomap");

    // Small graph (2 clusters of 5)
    let small = make_clustered_graph(2, 5, 1.0, 0.1);
    group.bench_function("small", |b| {
        b.iter(|| {
            let infomap = Infomap::new(InfomapConfig {
                seed: Some(42),
                num_trials: 3,
                ..Default::default()
            });
            black_box(infomap.run(black_box(&small)));
        })
    });

    // Medium graph (4 clusters of 25)
    let medium = make_clustered_graph(4, 25, 1.0, 0.1);
    group.bench_function("medium", |b| {
        b.iter(|| {
            let infomap = Infomap::new(InfomapConfig {
                seed: Some(42),
                num_trials: 3,
                ..Default::default()
            });
            black_box(infomap.run(black_box(&medium)));
        })
    });

    // Large graph (4 clusters of 50)
    let large = make_clustered_graph(4, 50, 1.0, 0.1);
    group.bench_function("large", |b| {
        b.iter(|| {
            let infomap = Infomap::new(InfomapConfig {
                seed: Some(42),
                num_trials: 3,
                ..Default::default()
            });
            black_box(infomap.run(black_box(&large)));
        })
    });

    group.finish();
}

// Fluid Communities benchmarks
// ---------------------------------------------------------------------------

fn bench_fluid_communities(c: &mut Criterion) {
    let mut group = c.benchmark_group("fluid_communities");

    // Small graph (2 clusters of 5)
    let small = make_clustered_graph(2, 5, 1.0, 0.1);
    group.bench_function("small", |b| {
        b.iter(|| {
            let fc = FluidCommunities::new(FluidCommunitiesConfig {
                k: 2,
                seed: Some(42),
                max_iterations: 100,
            });
            black_box(fc.run(black_box(&small)).unwrap());
        })
    });

    // Medium graph (4 clusters of 25)
    let medium = make_clustered_graph(4, 25, 1.0, 0.1);
    group.bench_function("medium", |b| {
        b.iter(|| {
            let fc = FluidCommunities::new(FluidCommunitiesConfig {
                k: 4,
                seed: Some(42),
                max_iterations: 100,
            });
            black_box(fc.run(black_box(&medium)).unwrap());
        })
    });

    // Large graph (4 clusters of 50)
    let large = make_clustered_graph(4, 50, 1.0, 0.1);
    group.bench_function("large", |b| {
        b.iter(|| {
            let fc = FluidCommunities::new(FluidCommunitiesConfig {
                k: 4,
                seed: Some(42),
                max_iterations: 100,
            });
            black_box(fc.run(black_box(&large)).unwrap());
        })
    });

    group.finish();
}

// Algorithm comparison (all 4 algorithms on the same SBM graph)
// ---------------------------------------------------------------------------

fn bench_algorithm_comparison(c: &mut Criterion) {
    let mut group = c.benchmark_group("algorithm_comparison");

    // SBM graph with 3 communities of varying sizes
    let community_sizes = vec![20, 30, 50];
    let affinity = vec![
        vec![0.5, 0.01, 0.01],
        vec![0.01, 0.3, 0.01],
        vec![0.01, 0.01, 0.2],
    ];
    let (graph, _truth) = generate_sbm(&community_sizes, &affinity, Some(42)).unwrap();

    group.bench_function("leiden", |b| {
        b.iter(|| {
            let leiden = Leiden::new(LeidenConfig {
                seed: Some(42),
                ..Default::default()
            });
            black_box(leiden.run(black_box(&graph)).unwrap());
        })
    });

    group.bench_function("infomap", |b| {
        b.iter(|| {
            let infomap = Infomap::new(InfomapConfig {
                seed: Some(42),
                num_trials: 3,
                ..Default::default()
            });
            black_box(infomap.run(black_box(&graph)));
        })
    });

    group.bench_function("label_propagation", |b| {
        b.iter(|| {
            let lp = LabelPropagation::new(LabelPropagationConfig {
                seed: Some(42),
                ..Default::default()
            });
            black_box(lp.run(black_box(&graph)));
        })
    });

    group.bench_function("fluid_communities", |b| {
        b.iter(|| {
            let fc = FluidCommunities::new(FluidCommunitiesConfig {
                k: 3,
                seed: Some(42),
                max_iterations: 100,
            });
            black_box(fc.run(black_box(&graph)).unwrap());
        })
    });

    group.finish();
}

criterion_group!(small, bench_small_rs, bench_small_fa);
criterion_group!(medium, bench_medium_rs, bench_medium_fa);
criterion_group!(large, bench_large_rs, bench_large_fa);
criterion_group!(real, bench_real_networks_rs, bench_real_networks_fa);
criterion_group!(directed, bench_directed_medium_rs, bench_directed_large_rs);
criterion_group!(label_propagation, bench_label_propagation);
criterion_group!(louvain_comparison, bench_louvain_comparison);
criterion_group!(infomap, bench_infomap);
criterion_group!(fluid_communities, bench_fluid_communities);
criterion_group!(algorithm_comparison, bench_algorithm_comparison);
criterion_main!(
    small, medium, large, real, directed, label_propagation, louvain_comparison, infomap,
    fluid_communities, algorithm_comparison
);

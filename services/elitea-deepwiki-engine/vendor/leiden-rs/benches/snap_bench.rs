use criterion::{black_box, criterion_group, criterion_main, BenchmarkId, Criterion};
use leiden_rs::{GraphData, GraphDataBuilder, Leiden, LeidenConfig};
use std::collections::HashMap;
use std::path::PathBuf;

fn cache_dir() -> PathBuf {
    let manifest_dir = std::env::var("CARGO_MANIFEST_DIR").unwrap();
    PathBuf::from(manifest_dir).join("data").join(".cache")
}

struct SnapNetwork {
    name: &'static str,
    filename: &'static str,
    directed: bool,
}

const SNAP_NETWORKS: &[SnapNetwork] = &[
    SnapNetwork {
        name: "ca-GrQc",
        filename: "ca-GrQc.txt",
        directed: false,
    },
    SnapNetwork {
        name: "ca-HepTh",
        filename: "ca-HepTh.txt",
        directed: false,
    },
    SnapNetwork {
        name: "ca-HepPh",
        filename: "ca-HepPh.txt",
        directed: false,
    },
    SnapNetwork {
        name: "email-Enron",
        filename: "email-Enron.txt",
        directed: false,
    },
];

fn load_snap(name: &str, filename: &str, directed: bool) -> GraphData {
    let path = cache_dir().join(filename);
    let text = std::fs::read_to_string(&path)
        .unwrap_or_else(|e| panic!("Dataset {name} not found at {}: {e}\nRun `cargo test --features snap-tests` first to download.", path.display()));

    let mut node_map: HashMap<usize, usize> = HashMap::new();
    let mut next_id = 0;
    let mut edges: Vec<(usize, usize)> = Vec::new();

    for line in text.lines() {
        let line = line.trim();
        if line.is_empty() || line.starts_with('#') {
            continue;
        }
        let parts: Vec<&str> = line.split_whitespace().collect();
        if parts.len() < 2 {
            continue;
        }
        let Ok(src) = parts[0].parse::<usize>() else {
            continue;
        };
        let Ok(dst) = parts[1].parse::<usize>() else {
            continue;
        };

        let src_idx = *node_map.entry(src).or_insert_with(|| {
            let id = next_id;
            next_id += 1;
            id
        });
        let dst_idx = *node_map.entry(dst).or_insert_with(|| {
            let id = next_id;
            next_id += 1;
            id
        });
        edges.push((src_idx, dst_idx));
    }

    let node_count = node_map.len();
    let mut builder = GraphDataBuilder::new(node_count);
    if directed {
        builder = builder.directed();
        for (src, dst) in &edges {
            builder.add_edge(*src, *dst, 1.0).unwrap();
        }
    } else {
        let mut seen = std::collections::HashSet::new();
        for (src, dst) in &edges {
            let key = (*src).min(*dst) * node_count + (*src).max(*dst);
            if seen.insert(key) && *src != *dst {
                builder.add_edge(*src, *dst, 1.0).unwrap();
            }
        }
    }
    builder.build().unwrap()
}

fn bench_snap_networks(c: &mut Criterion) {
    let mut group = c.benchmark_group("snap_networks");
    for net in SNAP_NETWORKS {
        let graph = load_snap(net.name, net.filename, net.directed);
        let leiden = Leiden::new(LeidenConfig {
            seed: Some(42),
            ..Default::default()
        });
        group.bench_with_input(
            BenchmarkId::new("leiden_rs", net.name),
            &graph,
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

criterion_group!(snap, bench_snap_networks);
criterion_main!(snap);

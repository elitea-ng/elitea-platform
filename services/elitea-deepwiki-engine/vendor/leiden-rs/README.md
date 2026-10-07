English | [中文](./README_zh.md)

# leiden-rs

A Rust implementation of the Leiden algorithm for community detection in graphs, with optional adapters for [gryf](https://github.com/pnevyk/gryf) and [petgraph](https://github.com/petgraph/petgraph).

## Overview

The Leiden algorithm is a widely-used method for detecting communities in networks. It improves upon the Louvain algorithm by guaranteeing well-connected communities: every node in a community is guaranteed to be reachable from every other node through paths that stay entirely within the community. This is achieved through a three-phase process of local moving, refinement, and aggregation, iterated until convergence.

## Features

- Full three-phase Leiden algorithm: local moving, refinement, and aggregation
- Modularity (Newman-Girvan) and CPM (Constant Potts Model) quality functions
- RBConfiguration (Reichardt-Bornholdt with configuration null model) quality function
- RBER (Reichardt-Bornholdt with Erdős-Rényi null model) quality function
- `QualityFunction` trait for pluggable custom quality functions
- Seeded RNG for deterministic, reproducible results
- Rayon-based parallelism with graph coloring for parallel local moving on large graphs
- Supports undirected weighted graphs, self-loops, disconnected components, and isolated nodes
- Supports undirected and directed weighted graphs, self-loops, disconnected components, and isolated nodes
- Optional graph library adapters for `gryf` and `petgraph`
- Optional CLI tool for edge list input/output
- Criterion benchmarks and comparison benchmarks against `fa-leiden-cd`
- Resolution profile: linear scan and bisection-based sweep across gamma values
- LFR benchmark graph generator with known ground-truth community structure
- Hierarchical partition output: inspect communities at each aggregation level
- Evaluation metrics: NMI, ARI, conductance, coverage, internal density
- Multiplex/multilayer network optimization: detect communities across multiple graph layers simultaneously
- WebAssembly support: compile and run in browsers via the `wasm` feature flag

## Quick Start

Add `leiden-rs` to your `Cargo.toml`:

```toml
[dependencies]
leiden-rs = "0.7"
```

Detect communities in a graph:

```rust
use leiden_rs::{GraphDataBuilder, Leiden, LeidenConfig};

let mut b = GraphDataBuilder::new(3);
b.add_edge(0, 1, 1.0).unwrap();
b.add_edge(1, 2, 1.0).unwrap();
let graph = b.build().unwrap();

let leiden = Leiden::new(LeidenConfig::default());
let result = leiden.run(&graph).expect("leiden failed");
println!("Found {} communities (quality: {:.4})", result.partition.num_communities(), result.quality);
```

## Configuration

`LeidenConfig` controls algorithm behavior:

| Field | Type | Default | Description |
|---|---|---|---|
| `max_iterations` | `usize` | `100` | Maximum number of Leiden iterations (local move + refine + aggregate cycles) |
| `resolution` | `f64` | `1.0` | Resolution parameter gamma; higher values favor smaller communities |
| `seed` | `Option<u64>` | `None` | RNG seed for reproducible results; `None` uses a random seed |
| `quality` | `QualityType` | `Modularity` | Quality function to optimize (`Modularity`, `CPM`, `RBConfiguration`, or `RBER`) |
| `epsilon` | `f64` | `1e-10` | Convergence threshold; stop when quality improvement is below this value |
| `max_comm_size` | `usize` | `0` | Maximum number of nodes per community (0 = unlimited) |
| `parallel_local_moving_threshold` | `Option<usize>` | `None` | Minimum edge slots (CSR entries) for parallel local moving (default: 2000). Also requires at least 100 nodes. |
| `parallel_aggregation_threshold` | `Option<usize>` | `None` | Minimum edge slots (CSR entries) for parallel aggregation (default: 10000) |

```rust
use leiden_rs::{Leiden, LeidenConfig, QualityType};

let config = LeidenConfig {
    max_iterations: 200,
    resolution: 0.8,
    seed: Some(42),
    quality: QualityType::CPM,
    epsilon: 1e-8,
    max_comm_size: 0,
    ..Default::default()
};

let leiden = Leiden::new(config);
let result = leiden.run(&graph).expect("leiden failed");
```

You can also use the builder pattern:

```rust
use leiden_rs::LeidenConfig;

let config = LeidenConfig::builder()
    .resolution(0.8)
    .seed(42)
    .build();
```

## Quality Functions

The algorithm optimizes a quality function that measures the goodness of a partition. Four built-in functions are provided:

**Modularity** (Newman-Girvan):

```
Q = sum_c [ e_c / m  -  gamma * (k_c / (2m))^2 ]
```

Where `e_c` is the total edge weight inside community `c`, `m` is the total edge weight in the graph, `k_c` is the total degree of nodes in community `c`, and `gamma` is the resolution parameter.

**CPM** (Constant Potts Model):

```
H = sum_c [ e_c  -  gamma * n_c * (n_c - 1) / 2 ]
```

Where `n_c` is the number of nodes in community `c`. CPM avoids the resolution limit inherent in modularity and allows the resolution parameter to directly control expected community sizes.

**RBConfiguration** (Reichardt-Bornholdt with configuration model null):

```
Q = sum_c [ e_c - gamma * K_c^2 / (4m) ]
```

Where `K_c` is the total degree of nodes in community `c`. This is mathematically equivalent to `Modularity` with a configurable resolution parameter. It uses the configuration model (expected edges proportional to `k_i * k_j / 2m`) as the null model.

**RBER** (Reichardt-Bornholdt with Erdős-Rényi null model):

```
Q = sum_c [ e_c - gamma * p * n_c * (n_c - 1) / 2 ]
```

Where `p = 2m / (N*(N-1))` is the graph density. Similar to CPM but uses the Erdős-Rényi random graph as the null model, making the effective resolution scale with graph density.

## Error Handling

Functions that can fail return `Result<T, LeidenError>`:

- `GraphDataBuilder::add_edge()` returns `Result<&mut Self, LeidenError>` — validates edge weights are finite and non-negative, and node IDs are in range.
- `GraphDataBuilder::build()` returns `Result<GraphData, LeidenError>` — validates CSR structural invariants after construction.
- `Leiden::run()` returns `Result<LeidenOutput, LeidenError>` — propagates input validation errors.
- `run_multiplex()` returns `Result<MultiplexOutput, LeidenError>` — validates layer consistency (same node count, matching weights).

Error variants:

- `LeidenError::InvalidEdgeWeight { weight }` — an edge weight is NaN, infinite, or negative.
- `LeidenError::InconsistentStructure { message }` — CSR components have inconsistent lengths or bounds.
- `LeidenError::InvalidParameter { message }` — an algorithm parameter is invalid (e.g., mismatched layer node counts).

## Advanced Features

### Resolution Profile

The library provides two methods for scanning community structure across resolution parameter values:

**Linear scan** — runs Leiden at evenly spaced gamma values:

```rust
use leiden_rs::{resolution_scan, QualityType};

let entries = resolution_scan(&graph, QualityType::CPM, (0.0, 1.0), 20, Some(42))?;
for entry in &entries {
    println!("gamma={:.3}: {} communities (quality={:.4})",
             entry.resolution, entry.num_communities, entry.quality);
}
```

**Bisection profile** — efficiently finds only the gamma values where the partition changes (like leidenalg):

```rust
use leiden_rs::{resolution_profile, QualityType};

let profile = resolution_profile(&graph, QualityType::CPM, (0.0, 1.0), Some(42), 1e-3, 1.0)?;
for entry in &profile {
    println!("gamma={:.3}: {} communities", entry.resolution, entry.num_communities);
}
```

### Hierarchical Output

Inspect community structure at each aggregation level:

```rust
use leiden_rs::{Leiden, LeidenConfig};

let leiden = Leiden::new(LeidenConfig { seed: Some(42), ..Default::default() });
let h = leiden.run_hierarchical(&graph)?;

println!("{} aggregation levels", h.num_levels());
for (i, level) in h.levels.iter().enumerate() {
    println!("Level {}: {} nodes → {} communities (quality={:.4})",
             i, level.node_count, level.num_communities, level.quality);
}

// Query community of a specific node at any level
let comm = h.community_of_at_level(0, 0); // node 0, finest level
```

### Multiplex Networks

Detect communities across multiple graph layers simultaneously. All layers must share the same node set. The algorithm optimizes a weighted sum of quality functions: `Q = Σ_l w_l * Q_l`.

```rust
use leiden_rs::{run_multiplex, MultiplexConfig, GraphDataBuilder};

// Two layers representing different relationship types on the same node set
let mut b1 = GraphDataBuilder::new(6);
b1.add_edge(0, 1, 1.0).unwrap();
b1.add_edge(1, 2, 1.0).unwrap();
b1.add_edge(0, 2, 1.0).unwrap();
b1.add_edge(3, 4, 1.0).unwrap();
b1.add_edge(4, 5, 1.0).unwrap();
b1.add_edge(3, 5, 1.0).unwrap();
b1.add_edge(2, 3, 1.0).unwrap();
let layer1 = b1.build().unwrap();

let mut b2 = GraphDataBuilder::new(6);
b2.add_edge(0, 1, 1.0).unwrap();
b2.add_edge(1, 2, 1.0).unwrap();
b2.add_edge(0, 2, 1.0).unwrap();
b2.add_edge(3, 4, 1.0).unwrap();
b2.add_edge(4, 5, 1.0).unwrap();
b2.add_edge(3, 5, 1.0).unwrap();
b2.add_edge(1, 4, 1.0).unwrap();
let layer2 = b2.build().unwrap();

let config = MultiplexConfig {
    seed: Some(42),
    layer_weights: vec![1.0, 1.0],  // equal weight for both layers
    ..Default::default()
};

let result = run_multiplex(&[layer1, layer2], &config)?;
println!("Found {} communities (quality: {:.4})",
         result.partition.num_communities(), result.quality);
println!("Per-layer qualities: {:?}", result.layer_qualities);
```

**Layer weights**: Use different weights to prioritize certain layers. Higher weights give more influence. Negative weights invert the quality contribution (push nodes apart), useful for repulsive inter-layer coupling.

**Use cases**: social networks with multiple relationship types, temporal network snapshots, multimodal transportation graphs, brain connectivity across frequency bands.

### Evaluation Metrics

Compare detected communities against ground truth or measure community quality:

```rust
use leiden_rs::{nmi, ari, conductance, coverage};

// NMI and ARI compare two partitions (e.g., detected vs ground truth)
// Accepts &Partition, &[usize], or &Vec<usize>
let similarity = nmi(&ground_truth, &detected_partition); // 0..1
let agreement = ari(&ground_truth, &detected_partition);  // -0.5..1

// Community-level metrics
let cond = conductance(&graph_data, &partition); // per-community conductance
let cov = coverage(&graph_data, &partition);     // fraction of intra-community edges
```

### LFR Benchmark

Generate synthetic graphs with known community structure for evaluating detection algorithms:

```rust
use leiden_rs::{generate_lfr_graph, LfrConfig};

let lfr = generate_lfr_graph(LfrConfig {
    n: 250,
    tau1: 3.0,         // degree distribution exponent
    tau2: 1.5,         // community size exponent
    mu: 0.1,           // mixing parameter (0 = perfect communities)
    average_degree: Some(5.0),
    min_community: Some(20),
    seed: Some(42),
    ..Default::default()
})?;

println!("Generated {} nodes, {} edges, {} communities",
         lfr.node_count, lfr.edges.len(),
         lfr.community_sizes.len());
// lfr.ground_truth contains the true community assignment per node
```

## Platform Support

### CLI Usage

The `leiden-cli` binary reads an edge list from stdin and writes community assignments to stdout. It is feature-gated behind the `cli` feature (enabled by default).

```
cargo run --bin leiden-cli -- [options]
```

Options:

```
[INPUT]                        Input edge list file (stdin if not specified)
--quality <modularity|cpm|rbconfiguration|rber>   Quality function (default: modularity)
--resolution <gamma>          Resolution parameter (default: 1.0)
--seed <u64>                  RNG seed for reproducibility
--iterations <n>              Maximum iterations (default: 100)
--epsilon <float>             Convergence threshold (default: 1e-10)
--max-comm-size <n>           Maximum nodes per community (default: 0, unlimited)
--directed                     Treat the graph as directed (default: undirected)
--parallel-local-moving-threshold <n>   Minimum edge slots for parallel local moving (default: 2000)
--parallel-aggregation-threshold <n>    Minimum edge slots for parallel aggregation (default: 10000)
-o, --output <FILE>           Output file (stdout if not specified)
```

Example:

```bash
echo -e "1 2 1.0\n2 3 1.0\n3 4 0.5\n4 1 0.3" | cargo run --bin leiden-cli -- --seed 42
```

Input format: one edge per line as `source target weight`. Node IDs are remapped to dense indices internally.

### Feature Flags

| Feature | Default | Description |
|---|---|---|
| `cli` | Yes | Enables the `leiden-cli` binary and pulls in `clap` |
| `gryf` | Yes | Enables the `gryf` graph library adapter (`from_gryf` / `from_gryf_directed`) |
| `rayon` | Yes | Enables Rayon-based parallel local moving on large graphs |
| `petgraph` | No | Enables the `petgraph` graph library adapter (`from_petgraph`) |
| `serde` | No | Enables `Serialize`/`Deserialize` for `Partition`, `LeidenConfig`, and `QualityType` |
| `wasm` | No | Enables WebAssembly bindings via `wasm-bindgen` (disables parallelism) |

To use as a library without the CLI dependency:

```toml
[dependencies]
leiden-rs = { version = "0.7", default-features = false, features = ["rayon"] }
```

To use with a specific graph library:

```toml
[dependencies]
leiden-rs = { version = "0.7", default-features = false, features = ["rayon", "gryf"] }
gryf = "0.2"
```

### WebAssembly

Compile to WebAssembly for browser use:

```bash
rustup target add wasm32-unknown-unknown
cargo build --no-default-features --features wasm --target wasm32-unknown-unknown
```

The `wasm` feature provides a JavaScript-friendly API via `wasm-bindgen`:

```rust
use leiden_rs::wasm::leiden_from_edgelist;

// edges: flat array [src0, dst0, weight0, src1, dst1, weight1, ...]
let communities = leiden_from_edgelist(&edges, num_nodes, Some(42));
```

## Implementation Notes

- **Graph representation**: Uses a custom CSR (Compressed Sparse Row) representation with separate out-edge and in-edge storage. Supports both undirected and directed graphs.
- **Parallelism**: Graph coloring-based parallel local moving with rayon. Nodes are colored so that same-color nodes form independent sets and can be moved simultaneously. Automatic fallback to sequential for small graphs (< 500 nodes).
- **Determinism**: When a seed is provided, all random decisions are reproducible across runs. Without a seed, results vary between runs.
- **Convergence**: Iteration stops when the total quality of the partition fails to improve beyond an epsilon threshold of 1e-10, preventing infinite loops from floating-point noise.
- **Move components**: The refinement phase uses the move-components subgraph to guarantee community connectivity.
- **Graph library adapters**: Optional adapters for `gryf` and `petgraph` are available behind feature flags, converting external graph types into the internal `GraphData` CSR format.

## Benchmarks

Benchmarks use [Criterion](https://github.com/bheisler/criterion.rs) and compare against the `fa-leiden-cd` crate at three graph sizes:

| Benchmark | Graph size |
|---|---|
| Small | 2 clusters, 5 nodes each (10 nodes, 21 edges) |
| Medium | 4 clusters, 25 nodes each (100 nodes, 1206 edges) |
| Large | 4 clusters, 50 nodes each (200 nodes, 4906 edges) |

Run benchmarks:

```bash
cargo bench
```

## Testing

The project includes 128 tests covering quality functions, resolution profiles, LFR generation, hierarchical output, evaluation metrics (NMI, ARI), multiplex networks, real-world network integration tests (Karate Club, Dolphins, Jazz Musicians, Cora), LFR benchmark validation, edge cases (empty graphs, single-node, disconnected, isolated nodes, self-loops), and convergence behavior.

```bash
cargo test
```

## Acknowledgments

Special thanks to the [gryf](https://github.com/pnevyk/gryf) project for providing a clean, idiomatic Rust graph library that is available as an optional adapter.

## References

Traag, V.A., Waltman, L. & van Eck, N.J. (2019). From Louvain to Leiden: guaranteeing well-connected communities. *Scientific Reports*, 9, 5233. https://doi.org/10.1038/s41598-019-41695-z

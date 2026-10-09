[English](./README.md) | 中文

# leiden-rs

基于 [gryf](https://github.com/pnevyk/gryf) 和 [petgraph](https://github.com/petgraph/petgraph) 可选适配的 Rust Leiden 社区发现算法实现。

## 概述

Leiden 算法是一种广泛使用的网络社区发现方法。它在 Louvain 算法的基础上进行了改进，保证了社区的良好连通性：社区中的每个节点都能通过完全位于社区内部的路径到达该社区中的任何其他节点。算法通过局部移动、细化和聚合三个阶段迭代执行，直到收敛。

## 功能特性

- 完整的三阶段 Leiden 算法：局部移动、细化、聚合
- 模块度（Newman-Girvan）和 CPM（Constant Potts Model）质量函数
- RBConfiguration（Reichardt-Bornholdt 配置零模型）质量函数
- RBER（Reichardt-Bornholdt Erdős-Rényi 零模型）质量函数
- `QualityFunction` trait 支持自定义质量函数扩展
- 种子 RNG，保证结果可复现
- 基于 Rayon 的并行化，小图自动回退串行
- 支持无向和有向加权图、自环、不连通分量和孤立节点
- 可选的 `gryf` 和 `petgraph` 图库适配器
- 可选 CLI 工具，支持边列表输入/输出
- Criterion 基准测试及与 `fa-leiden-cd` 的对比基准
- 分辨率剖面：线性扫描和基于二分的 gamma 值扫描
- LFR 基准图生成器，带有已知真实社区结构
- 层次化分区输出：查看每个聚合层级的社区结构
- 评估指标：NMI、ARI、conductance、coverage、内部密度
- 多路复用/多层网络优化：同时检测多个图层的社区结构
- WebAssembly 支持：通过 `wasm` feature flag 编译并在浏览器中运行

## 快速开始

在 `Cargo.toml` 中添加依赖：

```toml
[dependencies]
leiden-rs = "0.7"
```

检测图中的社区：

```rust
use leiden_rs::{GraphDataBuilder, Leiden, LeidenConfig};

let mut b = GraphDataBuilder::new(3);
b.add_edge(0, 1, 1.0).unwrap();
b.add_edge(1, 2, 1.0).unwrap();
let graph = b.build().unwrap();

let leiden = Leiden::new(LeidenConfig::default());
let result = leiden.run(&graph).expect("leiden failed");
println!("发现 {} 个社区（质量：{:.4}）", result.partition.num_communities(), result.quality);
```

## 配置

`LeidenConfig` 控制算法行为：

| 字段 | 类型 | 默认值 | 说明 |
|---|---|---|---|
| `max_iterations` | `usize` | `100` | 最大迭代次数（局部移动 + 细化 + 聚合循环） |
| `resolution` | `f64` | `1.0` | 分辨率参数 gamma；值越大越倾向产生更小的社区 |
| `seed` | `Option<u64>` | `None` | RNG 种子，用于复现结果；`None` 使用随机种子 |
| `quality` | `QualityType` | `Modularity` | 优化的质量函数（`Modularity`、`CPM`、`RBConfiguration` 或 `RBER`） |
| `epsilon` | `f64` | `1e-10` | 收敛阈值；质量改进低于此值时停止迭代 |
| `max_comm_size` | `usize` | `0` | 每个社区的最大节点数（0 = 不限制） |
| `parallel_local_moving_threshold` | `Option<usize>` | `None` | 并行局部移动的最小边槽数（CSR 条目数，默认：2000）。还需至少 100 个节点 |
| `parallel_aggregation_threshold` | `Option<usize>` | `None` | 并行聚合的最小边槽数（CSR 条目数，默认：10000） |

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

也可以使用 builder 模式：

```rust
use leiden_rs::LeidenConfig;

let config = LeidenConfig::builder()
    .resolution(0.8)
    .seed(42)
    .build();
```

## 质量函数

算法通过优化一个质量函数来衡量社区划分的好坏。内置四种质量函数：

**模块度**（Newman-Girvan）：

```
Q = sum_c [ e_c / m  -  gamma * (k_c / (2m))^2 ]
```

其中 `e_c` 是社区 `c` 内部的总边权重，`m` 是图中总边权重，`k_c` 是社区 `c` 中所有节点的总度数，`gamma` 是分辨率参数。

**CPM**（Constant Potts Model）：

```
H = sum_c [ e_c  -  gamma * n_c * (n_c - 1) / 2 ]
```

其中 `n_c` 是社区 `c` 中的节点数。CPM 避免了模块度固有的分辨率限制问题，使分辨率参数能够直接控制期望的社区大小。

**RBConfiguration**（Reichardt-Bornholdt 配置模型零模型）：

```
Q = sum_c [ e_c - gamma * K_c^2 / (4m) ]
```

其中 `K_c` 是社区 `c` 中所有节点的总度数。数学上等同于支持自定义分辨率参数的模块度。使用配置模型（期望边权重与 `k_i * k_j / 2m` 成正比）作为零模型。

**RBER**（Reichardt-Bornholdt Erdős-Rényi 零模型）：

```
Q = sum_c [ e_c - gamma * p * n_c * (n_c - 1) / 2 ]
```

其中 `p = 2m / (N*(N-1))` 是图密度。类似于 CPM，但使用 Erdős-Rényi 随机图作为零模型，使有效分辨率随图密度缩放。

## 错误处理

可能失败的函数返回 `Result<T, LeidenError>`：

- `GraphDataBuilder::add_edge()` 返回 `Result<&mut Self, LeidenError>` — 验证边权重是否为有限值且非负，以及节点 ID 是否在有效范围内。
- `GraphDataBuilder::build()` 返回 `Result<GraphData, LeidenError>` — 验证构建后 CSR 结构的一致性。
- `Leiden::run()` 返回 `Result<LeidenOutput, LeidenError>` — 传播输入验证错误。
- `run_multiplex()` 返回 `Result<MultiplexOutput, LeidenError>` — 验证图层一致性（相同节点数、匹配的权重）。

错误类型：

- `LeidenError::InvalidEdgeWeight { weight }` — 边权重为 NaN、无穷大或负数。
- `LeidenError::InconsistentStructure { message }` — CSR 组件长度或边界不一致。
- `LeidenError::InvalidParameter { message }` — 算法参数无效（如图层节点数不匹配）。

## 高级功能

### 分辨率剖面

库提供两种扫描分辨率参数的社区结构方法：

**线性扫描** — 在均匀分布的 gamma 值上运行 Leiden：

```rust
use leiden_rs::{resolution_scan, QualityType};

let entries = resolution_scan(&graph, QualityType::CPM, (0.0, 1.0), 20, Some(42))?;
for entry in &entries {
    println!("gamma={:.3}: {} communities (quality={:.4})",
             entry.resolution, entry.num_communities, entry.quality);
}
```

**二分剖面** — 高效地找到划分发生变化的 gamma 值（类似 leidenalg）：

```rust
use leiden_rs::{resolution_profile, QualityType};

let profile = resolution_profile(&graph, QualityType::CPM, (0.0, 1.0), Some(42), 1e-3, 1.0)?;
for entry in &profile {
    println!("gamma={:.3}: {} communities", entry.resolution, entry.num_communities);
}
```

### 层次化输出

查看每个聚合层级的社区结构：

```rust
use leiden_rs::{Leiden, LeidenConfig};

let leiden = Leiden::new(LeidenConfig { seed: Some(42), ..Default::default() });
let h = leiden.run_hierarchical(&graph)?;

println!("{} 个聚合层级", h.num_levels());
for (i, level) in h.levels.iter().enumerate() {
    println!("层级 {}: {} 个节点 → {} 个社区 (质量={:.4})",
             i, level.node_count, level.num_communities, level.quality);
}

// 查询特定节点在任意层级的社区归属
let comm = h.community_of_at_level(0, 0); // 节点 0，最细粒度层级
```

### 多路复用网络

同时检测多个图层的社区结构。所有图层必须共享相同的节点集。算法优化质量函数的加权和：`Q = Σ_l w_l * Q_l`。

```rust
use leiden_rs::{run_multiplex, MultiplexConfig, GraphDataBuilder};

// 两个图层表示同一节点集上的不同关系类型
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
    layer_weights: vec![1.0, 1.0],  // 两个图层等权重
    ..Default::default()
};

let result = run_multiplex(&[layer1, layer2], &config)?;
println!("发现 {} 个社区（质量：{:.4}）",
         result.partition.num_communities(), result.quality);
println!("每层质量：{:?}", result.layer_qualities);
```

**图层权重**：使用不同的权重来优先考虑某些图层。权重越高影响越大。负权重会反转质量贡献（将节点推开），适用于图层间的排斥耦合。

**应用场景**：具有多种关系类型的社交网络、时间网络快照、多模态交通图、跨频段的脑连接组。

### 评估指标

将检测到的社区与真实标签对比，或衡量社区质量：

```rust
use leiden_rs::{nmi, ari, conductance, coverage};

// NMI 和 ARI 比较两个分区（如检测结果 vs 真实标签）
// 支持 &Partition、&[usize] 或 &Vec<usize>
let similarity = nmi(&ground_truth, &detected_partition); // 0..1
let agreement = ari(&ground_truth, &detected_partition);  // -0.5..1

// 社区级指标
let cond = conductance(&graph_data, &partition); // 每个社区的 conductance
let cov = coverage(&graph_data, &partition);     // 社区内边占比
```

### LFR 基准图

生成带有已知社区结构的合成图，用于评估社区检测算法：

```rust
use leiden_rs::{generate_lfr_graph, LfrConfig};

let lfr = generate_lfr_graph(LfrConfig {
    n: 250,
    tau1: 3.0,         // 度分布指数
    tau2: 1.5,         // 社区大小分布指数
    mu: 0.1,           // 混合参数（0 = 完美社区结构）
    average_degree: Some(5.0),
    min_community: Some(20),
    seed: Some(42),
    ..Default::default()
})?;

println!("生成了 {} 个节点、{} 条边、{} 个社区",
         lfr.node_count, lfr.edges.len(),
         lfr.community_sizes.len());
// lfr.ground_truth 包含每个节点的真实社区归属
```

## 平台支持

### CLI 使用

`leiden-cli` 二进制从标准输入读取边列表，将社区分配结果写入标准输出。通过 `cli` feature 控制（默认启用）。

```
cargo run --bin leiden-cli -- [选项]
```

选项：

```
[INPUT]                        输入边列表文件（未指定时从标准输入读取）
--quality <modularity|cpm|rbconfiguration|rber>   质量函数（默认：modularity）
--resolution <gamma>          分辨率参数（默认：1.0）
--seed <u64>                  RNG 种子，用于复现结果
--iterations <n>              最大迭代次数（默认：100）
--epsilon <float>             收敛阈值（默认：1e-10）
--max-comm-size <n>           每个社区的最大节点数（默认：0，不限制）
--directed                     将图视为有向图（默认：无向图）
--parallel-local-moving-threshold <n>   并行局部移动的最小边槽数（默认：2000）
--parallel-aggregation-threshold <n>    并行聚合的最小边槽数（默认：10000）
-o, --output <FILE>           输出文件（未指定时输出到标准输出）
```

示例：

```bash
echo -e "1 2 1.0\n2 3 1.0\n3 4 0.5\n4 1 0.3" | cargo run --bin leiden-cli -- --seed 42
```

输入格式：每行一条边，格式为 `源节点 目标节点 权重`。节点 ID 在内部会被重映射为稠密索引。

### Feature Flags

| Feature | 默认 | 说明 |
|---|---|---|
| `cli` | 是 | 启用 `leiden-cli` 二进制并引入 `clap` 依赖 |
| `gryf` | 是 | 启用 `gryf` 图库适配器（`from_gryf` / `from_gryf_directed`） |
| `rayon` | 是 | 启用基于 Rayon 的大图并行局部移动 |
| `petgraph` | 否 | 启用 `petgraph` 图库适配器（`from_petgraph`） |
| `serde` | 否 | 为 `Partition`、`LeidenConfig` 和 `QualityType` 启用 `Serialize`/`Deserialize` |
| `wasm` | 否 | 启用通过 `wasm-bindgen` 的 WebAssembly 绑定（禁用并行化） |

作为库使用时可以禁用 CLI 依赖：

```toml
[dependencies]
leiden-rs = { version = "0.7", default-features = false, features = ["rayon"] }
```

使用特定图库时：

```toml
[dependencies]
leiden-rs = { version = "0.7", default-features = false, features = ["rayon", "gryf"] }
gryf = "0.2"
```

### WebAssembly

编译为 WebAssembly 以在浏览器中使用：

```bash
rustup target add wasm32-unknown-unknown
cargo build --no-default-features --features wasm --target wasm32-unknown-unknown
```

`wasm` feature 通过 `wasm-bindgen` 提供了 JavaScript 友好的 API：

```rust
use leiden_rs::wasm::leiden_from_edgelist;

// edges: 扁平数组 [源0, 目标0, 权重0, 源1, 目标1, 权重1, ...]
let communities = leiden_from_edgelist(&edges, num_nodes, Some(42));
```

## 实现说明

- **图表示**：使用自定义的 CSR（压缩稀疏行）表示，具有独立的出边和入边存储。支持无向图和有向图。
- **并行化**：基于图着色的并行局部移动，使用 Rayon 实现。节点被着色使得同色节点构成独立集，可同时移动。小图（< 500 节点）自动回退到串行模式。
- **确定性**：提供种子时，所有随机决策在多次运行间可复现。不提供种子时，每次运行结果不同。
- **收敛检测**：当社区划分的总质量改善不超过 epsilon 阈值（1e-10）时停止迭代，防止浮点噪声导致的无限循环。
- **Move components**：细化阶段使用 move-components 子图来保证社区连通性。
- **图库适配器**：通过 feature flag 提供 `gryf` 和 `petgraph` 的可选适配器，将外部图类型转换为内部 `GraphData` CSR 格式。

## 基准测试

基准测试使用 [Criterion](https://github.com/bheisler/criterion.rs)，并在三种图规模上与 `fa-leiden-cd` crate 进行对比：

| 基准 | 图规模 |
|---|---|
| 小型 | 2 个簇，每簇 5 个节点（10 节点，21 条边） |
| 中型 | 4 个簇，每簇 25 个节点（100 节点，1206 条边） |
| 大型 | 4 个簇，每簇 50 个节点（200 节点，4906 条边） |

运行基准测试：

```bash
cargo bench
```

## 测试

项目包含 128 个测试，覆盖质量函数、分辨率剖面、LFR 生成、层次化输出、评估指标（NMI、ARI）、多路复用网络、真实网络集成测试（Karate Club、Dolphins、Jazz Musicians、Cora）、LFR 基准验证、边界情况（空图、单节点、不连通分量、孤立节点、自环）和收敛行为等。

```bash
cargo test
```

## 致谢

特别感谢 [gryf](https://github.com/pnevyk/gryf) 项目提供了一个简洁、地道的 Rust 图库，可作为可选适配器使用。

## 参考文献

Traag, V.A., Waltman, L. & van Eck, N.J. (2019). From Louvain to Leiden: guaranteeing well-connected communities. *Scientific Reports*, 9, 5233. https://doi.org/10.1038/s41598-019-41695-z

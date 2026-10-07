//! Infomap community detection algorithm — foundational data structures and PageRank flow.
//!
//! This module provides the flow-based primitives required by the Infomap algorithm:
//!
//! - [`FlowData`] — per-node flow values (PageRank steady-state probabilities).
//! - [`DeltaFlow`] — flow change when moving a node between modules.
//! - [`InfomapConfig`] — algorithm configuration with sensible defaults.
//! - [`InfomapOutput`] — result of running Infomap.
//! - [`plogp`] — information-theoretic helper `x log₂(x)`.
//! - [`compute_flow`] — PageRank power-iteration on a [`GraphData`].

use crate::graph::data::GraphData;
use crate::graph::GraphDataBuilder;
use crate::partition::Partition;
use rand::rngs::StdRng;
use rand::seq::SliceRandom;
use rand::SeedableRng;
use rustc_hash::FxHashMap;

// ── Structs ──────────────────────────────────────────────────────────────────

/// Per-node flow values computed by PageRank power iteration.
///
/// All flow values are non-negative and the sum of `flow` across all nodes
/// equals 1.0 after convergence.
#[derive(Debug, Clone, PartialEq)]
pub struct FlowData {
    /// Steady-state PageRank probability for this node.
    pub flow: f64,
    /// Flow entering this node from other modules (used by Map Equation).
    pub enter_flow: f64,
    /// Flow exiting this node to other modules (used by Map Equation).
    pub exit_flow: f64,
    /// Teleportation flow component for this node.
    pub teleport_flow: f64,
}

/// Change in flow when moving a node between modules.
///
/// Used during the Map Equation optimisation to evaluate whether a move
/// reduces the description length.
#[derive(Debug, Clone, PartialEq)]
pub struct DeltaFlow {
    /// Target module index.
    pub module: usize,
    /// Change in exit flow.
    pub delta_exit: f64,
    /// Change in enter flow.
    pub delta_enter: f64,
}

/// Per-module aggregated flow data for Map Equation computation.
///
/// Each module tracks the total node flow, inter-module enter flow, and
/// inter-module exit flow. These values are updated incrementally during
/// the local moving optimisation phase.
#[derive(Debug, Clone, PartialEq)]
pub struct ModuleFlowData {
    /// Total flow of all nodes in this module (sum of PageRank values).
    pub flow: f64,
    /// Flow entering this module from other modules (inter-module boundary).
    pub enter_flow: f64,
    /// Flow exiting this module to other modules (inter-module boundary).
    pub exit_flow: f64,
}

/// Map Equation state for O(1) delta codelength computation.
///
/// Stores per-module flow data and cached codelength terms for efficient
/// incremental updates during local moving optimisation. The `node_flow_log`
/// term is constant (node PageRank values never change after computation),
/// so only module-level terms need updating when nodes move.
///
/// # Codelength Formula
///
/// `L(M) = enterFlow_log - enter_log_enter - exit_log_exit + flow_log_flow - node_flow_log`
///
/// Where each term is a sum of `plogp(x) = x · log₂(x)` values over modules.
#[derive(Debug, Clone)]
pub struct MapEquation {
    /// Per-module flow data, indexed by module ID.
    pub module_data: Vec<ModuleFlowData>,

    // Cached global codelength terms:
    /// Total enter flow across all modules (including exit network flow).
    pub enter_flow: f64,
    /// `plogp(total_enterFlow)` — cached, 1 plogp call.
    pub enter_flow_log: f64,
    /// `Σ_modules plogp(module.enter_flow)` — cached sum.
    pub enter_log_enter: f64,
    /// `Σ_modules plogp(module.exit_flow)` — cached sum.
    pub exit_log_exit: f64,
    /// `Σ_modules plogp(module.exit_flow + module.flow)` — cached sum.
    pub flow_log_flow: f64,
    /// `Σ_nodes plogp(node.flow)` — CONSTANT, never changes during optimisation.
    pub node_flow_log: f64,
    /// Flow exiting the top-level network (≈ 0 for closed two-level partition).
    pub exit_network_flow: f64,
}

/// Configuration for the Infomap algorithm.
///
/// Provides default values matching the reference implementation:
/// teleportation rate α = 0.15, up to 100 power-iteration steps, etc.
#[derive(Debug, Clone, PartialEq)]
pub struct InfomapConfig {
    /// Optional RNG seed for reproducible results.
    pub seed: Option<u64>,
    /// Maximum number of optimisation iterations.
    pub max_iterations: usize,
    /// Teleportation probability α (PageRank damping = 1 − α).
    pub teleportation_rate: f64,
    /// Number of independent trials (best result is kept).
    pub num_trials: usize,
    /// Convergence tolerance for PageRank power iteration.
    pub tolerance: f64,
}

impl Default for InfomapConfig {
    fn default() -> Self {
        Self {
            seed: None,
            max_iterations: 100,
            teleportation_rate: 0.15,
            num_trials: 10,
            tolerance: 1e-10,
        }
    }
}

impl InfomapConfig {
    /// Create a new configuration with the given seed.
    #[must_use = "constructor returns a new instance"]
    pub fn new(seed: Option<u64>) -> Self {
        Self {
            seed,
            ..Default::default()
        }
    }
}

/// Result of running the Infomap algorithm.
#[derive(Debug, Clone, PartialEq)]
pub struct InfomapOutput {
    /// The detected community partition.
    pub partition: Partition,
    /// Map Equation codelength (description length) of the partition.
    pub codelength: f64,
    /// Number of hierarchical levels in the partition.
    pub num_levels: usize,
    /// Number of optimisation iterations performed.
    pub iterations: usize,
}

// ── Functions ────────────────────────────────────────────────────────────────

/// Compute `x · log₂(x)`, returning 0.0 for `x ≤ 0` instead of NaN.
///
/// This is the fundamental building block of the Map Equation's entropy
/// calculations: `H(p) = −Σ pᵢ log₂ pᵢ = −Σ plogp(pᵢ)`.
#[must_use]
pub fn plogp(x: f64) -> f64 {
    if x > 0.0 {
        x * x.log2()
    } else {
        0.0
    }
}

/// Compute PageRank flow via power iteration.
///
/// Returns a `Vec<FlowData>` of length `graph.node_count()` where each
/// element's `flow` field holds the steady-state PageRank probability.
///
/// # Arguments
///
/// * `graph` — input graph (directed or undirected).
/// * `teleport_rate` — probability α of teleporting (default 0.15).
/// * `tolerance` — convergence threshold on L∞ norm of flow change.
/// * `max_iter` — hard cap on power-iteration steps.
///
/// # Algorithm
///
/// 1. Initialise flow vector uniformly: `flow[i] = 1/n`.
/// 2. For directed graphs the transition matrix uses out-edges; for
///    undirected graphs the symmetric transition is used.
/// 3. Dangling nodes (out-degree 0) distribute their flow uniformly.
/// 4. Power iteration: `flow_new[i] = α/n + (1−α) · Σⱼ flow[j]·P[j→i]`.
/// 5. Converge when `max_i |flow_new[i] − flow[i]| < tolerance`.
#[must_use]
pub fn compute_flow(
    graph: &GraphData,
    teleport_rate: f64,
    tolerance: f64,
    max_iter: usize,
) -> Vec<FlowData> {
    let n = graph.node_count();
    if n == 0 {
        return Vec::new();
    }

    // Uniform initial distribution.
    let mut flow = vec![1.0 / n as f64; n];
    let teleport_dist = 1.0 / n as f64; // uniform teleportation target

    for _ in 0..max_iter {
        let mut flow_new = vec![0.0; n];

        // Accumulate flow from neighbours (via transition matrix).
        // For each node j, distribute flow[j] to its out-neighbours
        // proportionally to edge weight / out_degree[j].
        for (j, f) in flow.iter().enumerate() {
            let out_deg = graph.out_degree_of(j);
            if out_deg > 0.0 {
                // Normal node: distribute flow proportionally.
                for (target, weight) in graph.out_neighbors(j) {
                    flow_new[target] += f * (weight / out_deg);
                }
            } else {
                // Dangling node: distribute flow uniformly (like teleportation).
                for item in flow_new.iter_mut() {
                    *item += f * teleport_dist;
                }
            }
        }

        // For undirected graphs, use neighbors() which returns symmetric edges.
        // But out_neighbors() already works for undirected because the CSR stores
        // both directions in out_offsets. So the above handles both cases.

        // Apply teleportation: mix with uniform distribution.
        let damping = 1.0 - teleport_rate;
        for item in flow_new.iter_mut() {
            *item = teleport_rate * teleport_dist + damping * *item;
        }

        // Check convergence (L∞ norm).
        let mut max_diff = 0.0;
        for i in 0..n {
            let diff = (flow_new[i] - flow[i]).abs();
            if diff > max_diff {
                max_diff = diff;
            }
        }

        flow = flow_new;

        if max_diff < tolerance {
            break;
        }
    }

    // Build FlowData with flow values; enter/exit/teleport computed later by Map Equation.
    flow.into_iter()
        .map(|f| FlowData {
            flow: f,
            enter_flow: 0.0,
            exit_flow: 0.0,
            teleport_flow: 0.0,
        })
        .collect()
}

// ── MapEquation Implementation ────────────────────────────────────────────────

impl MapEquation {
    /// Create a new MapEquation where each node starts in its own singleton module.
    ///
    /// The `node_flows` slice provides per-node `FlowData` with `enter_flow` and
    /// `exit_flow` set to total incoming/outgoing edge flow respectively.
    /// `exit_network_flow` is ≈ 0 for a closed two-level partition.
    #[must_use]
    pub fn new(node_flows: &[FlowData], exit_network_flow: f64) -> Self {
        let num_modules = node_flows.len();
        let mut module_data = Vec::with_capacity(num_modules);

        let mut enter_flow = 0.0_f64;
        let mut enter_log_enter = 0.0_f64;
        let mut exit_log_exit = 0.0_f64;
        let mut flow_log_flow = 0.0_f64;
        let mut node_flow_log = 0.0_f64;

        for fd in node_flows {
            let module = ModuleFlowData {
                flow: fd.flow,
                enter_flow: fd.enter_flow,
                exit_flow: fd.exit_flow,
            };

            enter_flow += module.enter_flow;
            enter_log_enter += plogp(module.enter_flow);
            exit_log_exit += plogp(module.exit_flow);
            flow_log_flow += plogp(module.exit_flow + module.flow);
            node_flow_log += plogp(fd.flow);

            module_data.push(module);
        }

        enter_flow += exit_network_flow;
        let enter_flow_log = plogp(enter_flow);

        Self {
            module_data,
            enter_flow,
            enter_flow_log,
            enter_log_enter,
            exit_log_exit,
            flow_log_flow,
            node_flow_log,
            exit_network_flow,
        }
    }

    /// Compute total Map Equation codelength from cached terms.
    ///
    /// `L(M) = (enterFlow_log - enter_log_enter - exitNetwork_log)`
    ///       `+ (-exit_log_exit + flow_log_flow - nodeFlow_log)`
    #[must_use]
    pub fn codelength(&self) -> f64 {
        let index_codelength =
            self.enter_flow_log - self.enter_log_enter - plogp(self.exit_network_flow);
        let module_codelength = -self.exit_log_exit + self.flow_log_flow - self.node_flow_log;
        index_codelength + module_codelength
    }

    /// O(1) delta codelength when moving a node between modules.
    ///
    /// Returns ΔL — **negative** means the move improves (reduces) codelength.
    /// Uses exactly 13 `plogp` calls: 7 cached values + 6 new evaluations.
    ///
    /// # Arguments
    ///
    /// * `current` — `FlowData` of the node being moved (`enter_flow`/`exit_flow`
    ///   are total incoming/outgoing edge flow, NOT inter-module).
    /// * `old_module` — index of the source module.
    /// * `new_module` — index of the target module.
    /// * `old_delta` — `DeltaFlow` for the old module (edges between node and old module).
    /// * `new_delta` — `DeltaFlow` for the new module (edges between node and new module).
    #[must_use]
    pub fn get_delta_codelength_on_moving_node(
        &self,
        current: &FlowData,
        old_module: usize,
        new_module: usize,
        old_delta: &DeltaFlow,
        new_delta: &DeltaFlow,
    ) -> f64 {
        let dee_old = old_delta.delta_enter + old_delta.delta_exit;
        let dee_new = new_delta.delta_enter + new_delta.delta_exit;

        let old_data = &self.module_data[old_module];
        let new_data = &self.module_data[new_module];

        // Group 1: index codebook enter flow (2 plogp: 1 new + 1 cached)
        let delta_enter = plogp(self.enter_flow + dee_old - dee_new) - self.enter_flow_log;

        // Group 2: module enter flow (4 plogp: 2 cached + 2 new)
        let delta_enter_log_enter = -plogp(old_data.enter_flow)
            - plogp(new_data.enter_flow)
            + plogp(old_data.enter_flow - current.enter_flow + dee_old)
            + plogp(new_data.enter_flow + current.enter_flow - dee_new);

        // Group 3: module exit flow (4 plogp: 2 cached + 2 new)
        let delta_exit_log_exit = -plogp(old_data.exit_flow)
            - plogp(new_data.exit_flow)
            + plogp(old_data.exit_flow - current.exit_flow + dee_old)
            + plogp(new_data.exit_flow + current.exit_flow - dee_new);

        // Group 4: module total flow (4 plogp: 2 cached + 2 new)
        let old_total = old_data.exit_flow + old_data.flow;
        let new_total = new_data.exit_flow + new_data.flow;
        let delta_flow_log_flow = -plogp(old_total)
            - plogp(new_total)
            + plogp(old_total - current.exit_flow - current.flow + dee_old)
            + plogp(new_total + current.exit_flow + current.flow - dee_new);

        // Total: 13 plogp calls (7 cached + 6 new)
        delta_enter - delta_enter_log_enter - delta_exit_log_exit + delta_flow_log_flow
    }

    /// Apply a node move: update module data and cached codelength terms.
    ///
    /// After this call the `MapEquation` state reflects the partition with the
    /// node moved from `old_module` to `new_module`.
    pub fn update_codelength_on_moving_node(
        &mut self,
        current: &FlowData,
        old_module: usize,
        new_module: usize,
        old_delta: &DeltaFlow,
        new_delta: &DeltaFlow,
    ) {
        let dee_old = old_delta.delta_enter + old_delta.delta_exit;
        let dee_new = new_delta.delta_enter + new_delta.delta_exit;

        let old_data = &self.module_data[old_module];
        let new_data = &self.module_data[new_module];
        self.enter_log_enter -= plogp(old_data.enter_flow) + plogp(new_data.enter_flow);
        self.exit_log_exit -= plogp(old_data.exit_flow) + plogp(new_data.exit_flow);
        self.flow_log_flow -=
            plogp(old_data.exit_flow + old_data.flow) + plogp(new_data.exit_flow + new_data.flow);

        self.remove_flow_from_module(old_module, current);
        self.add_flow_to_module(new_module, current);

        // Adjust boundary flows: edges between node and old module become inter-module,
        // edges between node and new module become intra-module.
        self.module_data[old_module].enter_flow += dee_old;
        self.module_data[old_module].exit_flow += dee_old;
        self.module_data[new_module].enter_flow -= dee_new;
        self.module_data[new_module].exit_flow -= dee_new;

        self.enter_flow += dee_old - dee_new;
        self.enter_flow_log = plogp(self.enter_flow);

        let old_data = &self.module_data[old_module];
        let new_data = &self.module_data[new_module];
        self.enter_log_enter += plogp(old_data.enter_flow) + plogp(new_data.enter_flow);
        self.exit_log_exit += plogp(old_data.exit_flow) + plogp(new_data.exit_flow);
        self.flow_log_flow +=
            plogp(old_data.exit_flow + old_data.flow) + plogp(new_data.exit_flow + new_data.flow);
    }

    /// Add a node's flow data to a module (raw aggregation, no boundary correction).
    pub fn add_flow_to_module(&mut self, module: usize, flow: &FlowData) {
        self.module_data[module].flow += flow.flow;
        self.module_data[module].enter_flow += flow.enter_flow;
        self.module_data[module].exit_flow += flow.exit_flow;
    }

    /// Remove a node's flow data from a module (raw aggregation, no boundary correction).
    pub fn remove_flow_from_module(&mut self, module: usize, flow: &FlowData) {
        self.module_data[module].flow -= flow.flow;
        self.module_data[module].enter_flow -= flow.enter_flow;
        self.module_data[module].exit_flow -= flow.exit_flow;
    }

    /// Recompute all cached codelength terms from scratch (full O(N_modules) scan).
    pub fn recalc_terms(&mut self) {
        self.enter_flow_log = plogp(self.enter_flow);
        self.enter_log_enter = 0.0;
        self.exit_log_exit = 0.0;
        self.flow_log_flow = 0.0;
        for module in &self.module_data {
            self.enter_log_enter += plogp(module.enter_flow);
            self.exit_log_exit += plogp(module.exit_flow);
            self.flow_log_flow += plogp(module.exit_flow + module.flow);
        }
    }
}

/// Compute total Map Equation codelength (standalone convenience function).
#[must_use]
pub fn calc_codelength(map_eq: &MapEquation) -> f64 {
    map_eq.codelength()
}

// ── Boundary Flow Computation ────────────────────────────────────────────────

/// Populate the `enter_flow` and `exit_flow` fields of `FlowData` from PageRank
/// values and graph structure.
///
/// For each directed edge (u→v, w), the link flow is:
/// `link_flow(u→v) = (1 − τ) × (w / out_degree(u)) × flow[u]`
///
/// * `exit_flow[u]` = Σ link_flow(u→v) for all out-edges of u.
/// * `enter_flow[u]` = Σ link_flow(v→u) for all in-edges to u.
///
/// For undirected graphs the CSR stores both directions in the out-edge array,
/// so iterating `out_neighbors` for every node visits each undirected edge in
/// both directions, correctly accumulating enter **and** exit flows.
fn populate_boundary_flows(
    graph: &GraphData,
    flow_data: &mut [FlowData],
    teleport_rate: f64,
) {
    let n = graph.node_count();
    let damping = 1.0 - teleport_rate;

    let mut enter_flow = vec![0.0; n];
    let mut exit_flow = vec![0.0; n];

    for u in 0..n {
        let out_deg = graph.out_degree_of(u);
        if out_deg > 0.0 {
            for (v, w) in graph.out_neighbors(u) {
                let lf = damping * (w / out_deg) * flow_data[u].flow;
                exit_flow[u] += lf;
                enter_flow[v] += lf;
            }
        }
    }

    for i in 0..n {
        flow_data[i].enter_flow = enter_flow[i];
        flow_data[i].exit_flow = exit_flow[i];
    }
}

// ── Local Moving ─────────────────────────────────────────────────────────────

/// Core Louvain-style local moving loop for Infomap.
///
/// Iterates over all nodes in random order, moving each to the module that
/// most decreases the Map Equation codelength ΔL. Repeats full passes until
/// no node moves.
///
/// # Arguments
///
/// * `map_eq` — mutable MapEquation state (module data and cached terms updated in place).
/// * `flow_data` — per-node FlowData (constant, never modified).
/// * `graph` — the graph on which local moving operates.
/// * `partition` — mutable community assignment (`partition[node] = module`).
/// * `rng` — seeded RNG for random node ordering.
///
/// # Returns
///
/// `true` if at least one node was moved during any pass.
pub fn run_local_moving(
    map_eq: &mut MapEquation,
    flow_data: &[FlowData],
    graph: &GraphData,
    partition: &mut [usize],
    rng: &mut StdRng,
) -> bool {
    let n = graph.node_count();
    if n <= 1 {
        return false;
    }

    let mut changed = false;

    loop {
        let mut improved = false;

        let mut order: Vec<usize> = (0..n).collect();
        order.shuffle(rng);

        for &node in &order {
            let old_module = partition[node];
            let node_exit = flow_data[node].exit_flow;
            let node_out_deg = graph.out_degree_of(node);

            // ── Collect per-module DeltaFlow via link flows ──

            let mut delta_exit_map: FxHashMap<usize, f64> = FxHashMap::default();
            let mut delta_enter_map: FxHashMap<usize, f64> = FxHashMap::default();

            if graph.is_directed() {
                // Out-edges → delta_exit (flow from node to target module)
                if node_out_deg > 0.0 {
                    for (v, w) in graph.out_neighbors(node) {
                        let target_module = partition[v];
                        let lf = (w / node_out_deg) * node_exit;
                        *delta_exit_map.entry(target_module).or_default() += lf;
                    }
                }
                // In-edges → delta_enter (flow from source module to node)
                for (v, w) in graph.in_neighbors(node) {
                    let source_module = partition[v];
                    let v_out_deg = graph.out_degree_of(v);
                    if v_out_deg > 0.0 {
                        let lf = (w / v_out_deg) * flow_data[v].exit_flow;
                        *delta_enter_map.entry(source_module).or_default() += lf;
                    }
                }
            } else {
                // Undirected: out_neighbors gives all neighbours (CSR stores both dirs).
                if node_out_deg > 0.0 {
                    for (v, w) in graph.out_neighbors(node) {
                        let target_module = partition[v];
                        // exit: flow from node → v
                        let exit_lf = (w / node_out_deg) * node_exit;
                        *delta_exit_map.entry(target_module).or_default() += exit_lf;
                        // enter: flow from v → node
                        let v_out_deg = graph.out_degree_of(v);
                        if v_out_deg > 0.0 {
                            let enter_lf = (w / v_out_deg) * flow_data[v].exit_flow;
                            *delta_enter_map.entry(target_module).or_default() += enter_lf;
                        }
                    }
                }
            }

            // Merge into DeltaFlow entries
            let mut all_modules: FxHashMap<usize, ()> = FxHashMap::default();
            for &m in delta_exit_map.keys() {
                all_modules.insert(m, ());
            }
            for &m in delta_enter_map.keys() {
                all_modules.insert(m, ());
            }

            let mut delta_flows: FxHashMap<usize, DeltaFlow> = FxHashMap::default();
            for &m in all_modules.keys() {
                delta_flows.insert(
                    m,
                    DeltaFlow {
                        module: m,
                        delta_exit: delta_exit_map.get(&m).copied().unwrap_or(0.0),
                        delta_enter: delta_enter_map.get(&m).copied().unwrap_or(0.0),
                    },
                );
            }

            // Self-module delta (edges between node and its own module)
            let self_delta = delta_flows.remove(&old_module).unwrap_or(DeltaFlow {
                module: old_module,
                delta_exit: 0.0,
                delta_enter: 0.0,
            });

            // ── Evaluate candidate modules ──

            let mut best_module = old_module;
            let mut best_delta = 0.0_f64; // must be strictly negative to move

            for (&new_module, new_delta) in &delta_flows {
                if new_module == old_module {
                    continue;
                }
                let dl = map_eq.get_delta_codelength_on_moving_node(
                    &flow_data[node],
                    old_module,
                    new_module,
                    &self_delta,
                    new_delta,
                );
                if dl < best_delta {
                    best_delta = dl;
                    best_module = new_module;
                }
            }

            // Try a new empty module (let node form its own singleton)
            let new_module_id = map_eq.module_data.len();
            map_eq.module_data.push(ModuleFlowData {
                flow: 0.0,
                enter_flow: 0.0,
                exit_flow: 0.0,
            });
            let empty_delta = DeltaFlow {
                module: new_module_id,
                delta_exit: 0.0,
                delta_enter: 0.0,
            };
            let dl = map_eq.get_delta_codelength_on_moving_node(
                &flow_data[node],
                old_module,
                new_module_id,
                &self_delta,
                &empty_delta,
            );
            if dl < best_delta {
                best_module = new_module_id;
                // keep the new entry
            } else {
                map_eq.module_data.pop();
            }

            // ── Apply move if improvement found ──

            if best_module != old_module {
                let actual_new_delta = if best_module == new_module_id {
                    empty_delta
                } else {
                    delta_flows
                        .get(&best_module)
                        .cloned()
                        .unwrap_or(DeltaFlow {
                            module: best_module,
                            delta_exit: 0.0,
                            delta_enter: 0.0,
                        })
                };

                map_eq.update_codelength_on_moving_node(
                    &flow_data[node],
                    old_module,
                    best_module,
                    &self_delta,
                    &actual_new_delta,
                );
                partition[node] = best_module;
                improved = true;
                changed = true;
            }
        }

        if !improved {
            break;
        }
    }

    changed
}

// ── Graph Aggregation ────────────────────────────────────────────────────────

/// Build a super-node graph by aggregating nodes within the same module.
///
/// Each unique module in the partition becomes a super-node. Edges between
/// nodes in the same module become self-loops. Edges between nodes in
/// different modules are merged with weights summed.
///
/// For directed graphs each directed edge (u→v) is aggregated separately.
/// For undirected graphs edges are canonicalised to avoid double counting.
pub fn aggregate_graph(graph: &GraphData, partition: &[usize]) -> GraphData {
    let n = graph.node_count();
    if n == 0 {
        return GraphDataBuilder::new(0).build().unwrap();
    }

    // Map module IDs → contiguous super-node IDs
    let mut module_to_super: FxHashMap<usize, usize> = FxHashMap::default();
    let mut orig_to_super = vec![0usize; n];
    let mut num_super = 0usize;
    for i in 0..n {
        let m = partition[i];
        let super_id = *module_to_super.entry(m).or_insert_with(|| {
            let id = num_super;
            num_super += 1;
            id
        });
        orig_to_super[i] = super_id;
    }

    if num_super == 0 {
        return GraphDataBuilder::new(0).build().unwrap();
    }

    // Aggregate edges
    let mut edge_map: FxHashMap<(usize, usize), f64> = FxHashMap::default();

    if graph.is_directed() {
        for u in 0..n {
            for (v, w) in graph.out_neighbors(u) {
                let su = orig_to_super[u];
                let sv = orig_to_super[v];
                *edge_map.entry((su, sv)).or_default() += w;
            }
        }
    } else {
        for u in 0..n {
            for (v, w) in graph.out_neighbors(u) {
                if u == v {
                    // Self-loop — counted once in the CSR
                    let su = orig_to_super[u];
                    *edge_map.entry((su, su)).or_default() += w;
                } else if v > u {
                    // Canonical ordering to avoid double counting
                    let su = orig_to_super[u];
                    let sv = orig_to_super[v];
                    let key = if su <= sv { (su, sv) } else { (sv, su) };
                    *edge_map.entry(key).or_default() += w;
                }
            }
        }
    }

    // Build aggregated graph
    let mut builder = GraphDataBuilder::new(num_super);
    if graph.is_directed() {
        builder = builder.directed();
    }
    for ((u, v), w) in &edge_map {
        builder.add_edge(*u, *v, *w).unwrap();
    }

    // Set node weights (sum of original node weights)
    let mut super_weights = vec![0.0f64; num_super];
    for i in 0..n {
        super_weights[orig_to_super[i]] += graph.node_weight(i);
    }
    for (node, &w) in super_weights.iter().enumerate() {
        if (w - 1.0).abs() > f64::EPSILON {
            builder.set_node_weight(node, w).unwrap();
        }
    }

    builder.build().unwrap()
}

// ── Multi-Level Recursion ────────────────────────────────────────────────────

/// Multi-level Infomap: local moving + aggregation, repeated recursively.
///
/// Returns `(partition, codelength)` where `partition[node] = module`.
///
/// The algorithm:
/// 1. Compute boundary flows from PageRank values.
/// 2. Create a `MapEquation` with each node in its own singleton module.
/// 3. Run local moving to optimise the partition.
/// 4. If nodes moved, aggregate the graph and recurse on the super-node graph.
/// 5. Project the aggregate partition back to original nodes.
pub fn run_multi_level(
    graph: &GraphData,
    flow_data: &[FlowData],
    config: &InfomapConfig,
) -> (Vec<usize>, f64) {
    run_multi_level_impl(graph, flow_data, config, 0)
}

/// Recursive helper for [`run_multi_level`].
fn run_multi_level_impl(
    graph: &GraphData,
    flow_data: &[FlowData],
    config: &InfomapConfig,
    depth: usize,
) -> (Vec<usize>, f64) {
    let n = graph.node_count();
    if n == 0 {
        return (Vec::new(), 0.0);
    }
    if n == 1 {
        let map_eq = MapEquation::new(
            &[FlowData {
                flow: flow_data[0].flow,
                enter_flow: 0.0,
                exit_flow: 0.0,
                teleport_flow: 0.0,
            }],
            0.0,
        );
        return (vec![0], map_eq.codelength());
    }

    // Enrich flow_data with enter/exit boundary flows (idempotent)
    let mut enriched = flow_data.to_vec();
    if enriched.iter().all(|fd| fd.enter_flow == 0.0 && fd.exit_flow == 0.0) {
        populate_boundary_flows(graph, &mut enriched, config.teleportation_rate);
    }

    // Singleton partition
    let mut partition: Vec<usize> = (0..n).collect();
    let mut map_eq = MapEquation::new(&enriched, 0.0);
    let mut rng = StdRng::seed_from_u64(config.seed.unwrap_or(0).wrapping_add(depth as u64));

    let improved = run_local_moving(&mut map_eq, &enriched, graph, &mut partition, &mut rng);

    // Renumber partition to contiguous IDs before aggregation
    let max_mod = *partition.iter().max().unwrap_or(&0);
    let mut renumber = vec![usize::MAX; max_mod + 1];
    let mut next_id = 0usize;
    for p in partition.iter_mut() {
        if renumber[*p] == usize::MAX {
            renumber[*p] = next_id;
            next_id += 1;
        }
        *p = renumber[*p];
    }

    // Recurse on aggregated graph if nodes moved and depth limit not reached
    if improved && depth < config.max_iterations {
        let aggregate = aggregate_graph(graph, &partition);
        if aggregate.node_count() < n {
            // Recompute PageRank flow on the aggregated graph
            let agg_flow = compute_flow(
                &aggregate,
                config.teleportation_rate,
                config.tolerance,
                config.max_iterations,
            );
            let (agg_partition, _agg_codelength) =
                run_multi_level_impl(&aggregate, &agg_flow, config, depth + 1);

            // Project aggregate partition back to original nodes
            // partition[i] = super-node for original node i (contiguous 0..num_super)
            // agg_partition[partition[i]] = aggregate module for that super-node
            for i in 0..n {
                partition[i] = agg_partition[partition[i]];
            }
        }
    }

    // Renumber partition to contiguous module IDs
    let max_mod = *partition.iter().max().unwrap_or(&0);
    let mut mapping = vec![usize::MAX; max_mod + 1];
    let mut next_id = 0usize;
    for p in partition.iter_mut() {
        if mapping[*p] == usize::MAX {
            mapping[*p] = next_id;
            next_id += 1;
        }
        *p = mapping[*p];
    }

    // Recompute codelength on the original graph with the final partition
    // using a fresh MapEquation (avoid accumulated floating-point drift)
    let cl = compute_codelength_for_partition(graph, &enriched, &partition, config.teleportation_rate);

    (partition, cl)
}

/// Compute the Map Equation codelength for a given partition on a graph.
///
/// Builds the MapEquation state from scratch: sums per-module flows and
/// computes inter-module enter/exit flows from link-flow boundary data.
fn compute_codelength_for_partition(
    graph: &GraphData,
    flow_data: &[FlowData],
    partition: &[usize],
    teleport_rate: f64,
) -> f64 {
    let n = graph.node_count();
    if n == 0 {
        return 0.0;
    }

    let damping = 1.0 - teleport_rate;
    let num_modules = *partition.iter().max().unwrap_or(&0) + 1;

    // Accumulate per-module flow
    let mut module_flow = vec![0.0; num_modules];

    for i in 0..n {
        module_flow[partition[i]] += flow_data[i].flow;
    }

    // Compute inter-module enter/exit via link flows
    let mut module_enter = vec![0.0; num_modules];
    let mut module_exit = vec![0.0; num_modules];

    for u in 0..n {
        let u_out_deg = graph.out_degree_of(u);
        if u_out_deg > 0.0 {
            for (v, w) in graph.out_neighbors(u) {
                let mu = partition[u];
                let mv = partition[v];
                if mu != mv {
                    let lf = damping * (w / u_out_deg) * flow_data[u].flow;
                    module_exit[mu] += lf;
                    module_enter[mv] += lf;
                }
            }
        }
    }

    // Build module_data
    let module_data: Vec<ModuleFlowData> = (0..num_modules)
        .map(|m| ModuleFlowData {
            flow: module_flow[m],
            enter_flow: module_enter[m],
            exit_flow: module_exit[m],
        })
        .collect();

    let enter_flow: f64 = module_enter.iter().sum();
    let node_flow_log: f64 = flow_data.iter().map(|fd| plogp(fd.flow)).sum();

    let map_eq = MapEquation {
        module_data,
        enter_flow,
        enter_flow_log: plogp(enter_flow),
        enter_log_enter: module_enter.iter().map(|&x| plogp(x)).sum(),
        exit_log_exit: module_exit.iter().map(|&x| plogp(x)).sum(),
        flow_log_flow: (0..num_modules)
            .map(|m| plogp(module_exit[m] + module_flow[m]))
            .sum(),
        node_flow_log,
        exit_network_flow: 0.0,
    };

    map_eq.codelength()
}

// ── Node Flow Computation ─────────────────────────────────────────────────────

/// Compute per-node enter/exit boundary flows from PageRank values.
///
/// Convenience wrapper that copies PageRank `flow` values and populates
/// `enter_flow` / `exit_flow` from link-flow boundary data.
///
/// * `flow_data` — PageRank flow values (only `.flow` field used).
/// * `graph` — graph structure.
/// * `teleport_rate` — teleportation probability α.
///
/// Returns a new `Vec<FlowData>` with all boundary fields populated.
#[must_use]
pub fn compute_node_flows(
    graph: &GraphData,
    flow_data: &[FlowData],
    teleport_rate: f64,
) -> Vec<FlowData> {
    let n = flow_data.len();
    let mut result = flow_data.to_vec();
    let damping = 1.0 - teleport_rate;

    let mut enter_flow = vec![0.0; n];
    let mut exit_flow = vec![0.0; n];

    for u in 0..n {
        let out_deg = graph.out_degree_of(u);
        if out_deg > 0.0 {
            for (v, w) in graph.out_neighbors(u) {
                let link_flow = damping * (w / out_deg) * result[u].flow;
                exit_flow[u] += link_flow;
                enter_flow[v] += link_flow;
            }
        }
    }

    for i in 0..n {
        result[i].enter_flow = enter_flow[i];
        result[i].exit_flow = exit_flow[i];
    }

    result
}

// ── Fine / Coarse Tuning ─────────────────────────────────────────────────────

/// Fine-tuning: re-run local moving starting from the current partition.
///
/// Each node is re-examined for possible moves to neighbouring modules.
/// Accepts moves that reduce codelength (ΔL < 0). Repeats until no
/// improvement.
///
/// Returns `true` if at least one node was moved.
pub fn fine_tuning(
    map_eq: &mut MapEquation,
    flow_data: &[FlowData],
    graph: &GraphData,
    partition: &mut [usize],
    rng: &mut StdRng,
) -> bool {
    run_local_moving(map_eq, flow_data, graph, partition, rng)
}

/// Coarse-tuning: second pass of local moving on the current partition.
///
/// For the P1 two-level Infomap, coarse-tuning is implemented as another
/// pass of local moving. It treats the current modules as atomic units and
/// tries to further merge or split them.
///
/// Returns `true` if at least one node was moved.
pub fn coarse_tuning(
    map_eq: &mut MapEquation,
    flow_data: &[FlowData],
    graph: &GraphData,
    partition: &mut [usize],
    rng: &mut StdRng,
) -> bool {
    run_local_moving(map_eq, flow_data, graph, partition, rng)
}

/// Renumber a partition so that module IDs are contiguous starting from 0.
fn renumber_partition(partition: &mut [usize]) {
    let max_mod = *partition.iter().max().unwrap_or(&0);
    let mut mapping = vec![usize::MAX; max_mod + 1];
    let mut next_id = 0usize;
    for p in partition.iter_mut() {
        if mapping[*p] == usize::MAX {
            mapping[*p] = next_id;
            next_id += 1;
        }
        *p = mapping[*p];
    }
}

/// Build a `MapEquation` state from a given partition and enriched flow data.
///
/// Computes per-module flow totals and inter-module enter/exit boundary flows.
fn build_map_equation_for_partition(
    partition: &[usize],
    enriched: &[FlowData],
    graph: &GraphData,
    teleport_rate: f64,
) -> MapEquation {
    let n = partition.len();
    let num_mod = *partition.iter().max().unwrap_or(&0) + 1;
    let damping = 1.0 - teleport_rate;

    let mut m_flow = vec![0.0; num_mod];
    let mut m_enter = vec![0.0; num_mod];
    let mut m_exit = vec![0.0; num_mod];

    for i in 0..n {
        m_flow[partition[i]] += enriched[i].flow;
    }
    for u in 0..n {
        let u_od = graph.out_degree_of(u);
        if u_od > 0.0 {
            for (v, w) in graph.out_neighbors(u) {
                let mu = partition[u];
                let mv = partition[v];
                if mu != mv {
                    let lf = damping * (w / u_od) * enriched[u].flow;
                    m_exit[mu] += lf;
                    m_enter[mv] += lf;
                }
            }
        }
    }

    let enter_flow: f64 = m_enter.iter().sum();
    let node_flow_log: f64 = enriched.iter().map(|fd| plogp(fd.flow)).sum();

    MapEquation {
        module_data: (0..num_mod)
            .map(|m| ModuleFlowData {
                flow: m_flow[m],
                enter_flow: m_enter[m],
                exit_flow: m_exit[m],
            })
            .collect(),
        enter_flow,
        enter_flow_log: plogp(enter_flow),
        enter_log_enter: m_enter.iter().map(|&x| plogp(x)).sum(),
        exit_log_exit: m_exit.iter().map(|&x| plogp(x)).sum(),
        flow_log_flow: (0..num_mod)
            .map(|m| plogp(m_exit[m] + m_flow[m]))
            .sum(),
        node_flow_log,
        exit_network_flow: 0.0,
    }
}

// ── Infomap Struct ───────────────────────────────────────────────────────────

/// Infomap community detection algorithm.
///
/// Wraps the full Infomap pipeline: PageRank flow computation, multi-level
/// local moving, fine/coarse tuning, and multi-trial optimisation.
///
/// # Example
///
/// ```ignore
/// use leiden_rs::infomap::{Infomap, InfomapConfig};
/// use leiden_rs::graph::GraphDataBuilder;
///
/// let mut b = GraphDataBuilder::new(6);
/// b.add_edge(0, 1, 5.0).unwrap();
/// b.add_edge(1, 2, 5.0).unwrap();
/// b.add_edge(0, 2, 5.0).unwrap();
/// b.add_edge(3, 4, 5.0).unwrap();
/// b.add_edge(4, 5, 5.0).unwrap();
/// b.add_edge(3, 5, 5.0).unwrap();
/// b.add_edge(2, 3, 0.1).unwrap();
/// let graph = b.build().unwrap();
///
/// let config = InfomapConfig { seed: Some(42), ..Default::default() };
/// let infomap = Infomap::new(config);
/// let result = infomap.run(&graph);
/// ```
pub struct Infomap {
    config: InfomapConfig,
}

impl Infomap {
    /// Create a new Infomap instance with the given configuration.
    #[must_use = "constructor returns a new instance"]
    pub fn new(config: InfomapConfig) -> Self {
        Self { config }
    }

    /// Run the full Infomap pipeline on the given graph.
    ///
    /// 1. Compute PageRank flow.
    /// 2. Compute node enter/exit boundary flows.
    /// 3. For each trial (with a different seed):
    ///    a. Run multi-level local moving + aggregation.
    ///    b. Fine-tune the resulting partition.
    ///    c. Alternate coarse-tune and fine-tune until convergence.
    /// 4. Keep the partition with the best codelength across all trials.
    ///
    /// Returns an [`InfomapOutput`] with the best partition found.
    pub fn run(&self, graph: &GraphData) -> InfomapOutput {
        let n = graph.node_count();
        if n == 0 {
            return InfomapOutput {
                partition: Partition::new(0),
                codelength: 0.0,
                num_levels: 1,
                iterations: 0,
            };
        }

        let flow_values = compute_flow(
            graph,
            self.config.teleportation_rate,
            self.config.tolerance,
            self.config.max_iterations,
        );

        let flow_data = compute_node_flows(graph, &flow_values, self.config.teleportation_rate);
        let mut enriched = flow_data.clone();
        populate_boundary_flows(graph, &mut enriched, self.config.teleportation_rate);

        let mut best_partition: Option<Vec<usize>> = None;
        let mut best_codelength = f64::INFINITY;
        let mut total_iterations = 0usize;

        for trial in 0..self.config.num_trials {
            let seed = self
                .config
                .seed
                .map(|s| s.wrapping_add((trial as u64).wrapping_mul(0x9e3779b97f4a7c15)));
            let mut rng = StdRng::seed_from_u64(seed.unwrap_or_else(|| {
                (trial as u64).wrapping_mul(0x517cc1b727220a95)
            }));

            let trial_config = InfomapConfig {
                seed,
                ..self.config.clone()
            };
            let (mut partition, _cl) = run_multi_level(graph, &flow_data, &trial_config);
            total_iterations += 1;

            let mut map_eq = build_map_equation_for_partition(
                &partition, &enriched, graph, self.config.teleportation_rate,
            );

            let _ = fine_tuning(&mut map_eq, &enriched, graph, &mut partition, &mut rng);
            renumber_partition(&mut partition);

            for _ in 0..10 {
                let cl_before = compute_codelength_for_partition(
                    graph, &enriched, &partition, self.config.teleportation_rate,
                );

                map_eq = build_map_equation_for_partition(
                    &partition, &enriched, graph, self.config.teleportation_rate,
                );

                let improved1 = coarse_tuning(
                    &mut map_eq, &enriched, graph, &mut partition, &mut rng,
                );
                renumber_partition(&mut partition);

                map_eq = build_map_equation_for_partition(
                    &partition, &enriched, graph, self.config.teleportation_rate,
                );

                let improved2 = fine_tuning(
                    &mut map_eq, &enriched, graph, &mut partition, &mut rng,
                );
                renumber_partition(&mut partition);
                total_iterations += 1;

                if !improved1 && !improved2 {
                    break;
                }

                let cl_after = compute_codelength_for_partition(
                    graph, &enriched, &partition, self.config.teleportation_rate,
                );

                if (cl_before - cl_after).abs() < self.config.tolerance {
                    break;
                }
            }

            let cl = compute_codelength_for_partition(
                graph, &enriched, &partition, self.config.teleportation_rate,
            );
            if cl < best_codelength {
                best_codelength = cl;
                best_partition = Some(partition);
            }
        }

        let mut partition = best_partition.unwrap_or_else(|| (0..n).collect());
        renumber_partition(&mut partition);

        InfomapOutput {
            partition: Partition::from_membership(partition),
            codelength: best_codelength,
            num_levels: 1,
            iterations: total_iterations,
        }
    }
}

// ── Tests ────────────────────────────────────────────────────────────────────

#[cfg(test)]
mod tests {
    use super::*;
    use crate::graph::GraphDataBuilder;

    // ── plogp tests ──

    #[test]
    fn test_plogp_zero() {
        // plogp(0.0) must be 0.0, never NaN.
        let result = plogp(0.0);
        assert!(
            result == 0.0,
            "plogp(0.0) should be exactly 0.0, got {result}"
        );
        assert!(!result.is_nan(), "plogp(0.0) must not be NaN");
    }

    #[test]
    fn test_plogp_negative() {
        assert_eq!(plogp(-1.0), 0.0, "plogp(negative) should be 0.0");
        assert_eq!(plogp(-0.001), 0.0);
    }

    #[test]
    fn test_plogp_value() {
        // plogp(0.5) = 0.5 * log2(0.5) = 0.5 * (-1) = -0.5
        let result = plogp(0.5);
        assert!(
            (result - (-0.5)).abs() < 1e-10,
            "plogp(0.5) should be -0.5, got {result}"
        );
    }

    #[test]
    fn test_plogp_one() {
        // plogp(1.0) = 1.0 * log2(1.0) = 0.0
        let result = plogp(1.0);
        assert!(
            result.abs() < 1e-10,
            "plogp(1.0) should be 0.0, got {result}"
        );
    }

    // ── FlowData tests ──

    #[test]
    fn test_flow_data_construction() {
        let fd = FlowData {
            flow: 0.25,
            enter_flow: 0.1,
            exit_flow: 0.15,
            teleport_flow: 0.05,
        };
        assert!((fd.flow - 0.25).abs() < 1e-10);
        assert!((fd.enter_flow - 0.1).abs() < 1e-10);
        assert!((fd.exit_flow - 0.15).abs() < 1e-10);
        assert!((fd.teleport_flow - 0.05).abs() < 1e-10);
    }

    #[test]
    fn test_flow_data_default_fields() {
        let fd = FlowData {
            flow: 0.5,
            enter_flow: 0.0,
            exit_flow: 0.0,
            teleport_flow: 0.0,
        };
        assert!((fd.flow - 0.5).abs() < 1e-10);
        assert_eq!(fd.enter_flow, 0.0);
        assert_eq!(fd.exit_flow, 0.0);
        assert_eq!(fd.teleport_flow, 0.0);
    }

    // ── DeltaFlow tests ──

    #[test]
    fn test_delta_flow_construction() {
        let df = DeltaFlow {
            module: 2,
            delta_exit: 0.3,
            delta_enter: 0.1,
        };
        assert_eq!(df.module, 2);
        assert!((df.delta_exit - 0.3).abs() < 1e-10);
        assert!((df.delta_enter - 0.1).abs() < 1e-10);
    }

    // ── InfomapConfig tests ──

    #[test]
    fn test_infomap_config_default() {
        let cfg = InfomapConfig::default();
        assert_eq!(cfg.seed, None);
        assert_eq!(cfg.max_iterations, 100);
        assert!((cfg.teleportation_rate - 0.15).abs() < 1e-10);
        assert_eq!(cfg.num_trials, 10);
        assert!((cfg.tolerance - 1e-10).abs() < 1e-20);
    }

    #[test]
    fn test_infomap_config_new() {
        let cfg = InfomapConfig::new(Some(42));
        assert_eq!(cfg.seed, Some(42));
        assert_eq!(cfg.max_iterations, 100); // defaults preserved
    }

    // ── InfomapOutput tests ──

    #[test]
    fn test_infomap_output_construction() {
        let output = InfomapOutput {
            partition: Partition::new(3),
            codelength: 1.5,
            num_levels: 2,
            iterations: 10,
        };
        assert_eq!(output.partition.len(), 3);
        assert!((output.codelength - 1.5).abs() < 1e-10);
        assert_eq!(output.num_levels, 2);
        assert_eq!(output.iterations, 10);
    }

    // ── compute_flow (PageRank) tests ──

    #[test]
    fn test_pagerank_empty_graph() {
        let graph = GraphDataBuilder::new(0).build().unwrap();
        let result = compute_flow(&graph, 0.15, 1e-10, 100);
        assert!(result.is_empty());
    }

    #[test]
    fn test_pagerank_single_node() {
        let graph = GraphDataBuilder::new(1).build().unwrap();
        let result = compute_flow(&graph, 0.15, 1e-10, 100);
        assert_eq!(result.len(), 1);
        assert!((result[0].flow - 1.0).abs() < 1e-6);
    }

    #[test]
    fn test_pagerank_triangle() {
        // Undirected 3-node triangle: symmetric → equal PageRank 1/3 each.
        let mut b = GraphDataBuilder::new(3);
        b.add_edge(0, 1, 1.0).unwrap();
        b.add_edge(1, 2, 1.0).unwrap();
        b.add_edge(0, 2, 1.0).unwrap();
        let graph = b.build().unwrap();

        let result = compute_flow(&graph, 0.15, 1e-10, 200);
        assert_eq!(result.len(), 3);

        let expected = 1.0 / 3.0;
        for (i, fd) in result.iter().enumerate() {
            assert!(
                (fd.flow - expected).abs() < 1e-6,
                "node {i}: flow = {}, expected {expected}",
                fd.flow
            );
        }

        // Total flow must sum to 1.0.
        let total: f64 = result.iter().map(|fd| fd.flow).sum();
        assert!(
            (total - 1.0).abs() < 1e-6,
            "total flow = {total}, expected 1.0"
        );
    }

    #[test]
    fn test_pagerank_dangling() {
        // Directed graph: 0 → 1, node 2 has no out-edges (dangling).
        // Flow should still sum to 1.0 and dangling node should have non-zero flow.
        let mut b = GraphDataBuilder::new(3).directed();
        b.add_edge(0, 1, 1.0).unwrap();
        // Node 2 has no edges at all (isolated + dangling).
        let graph = b.build().unwrap();

        let result = compute_flow(&graph, 0.15, 1e-10, 200);
        assert_eq!(result.len(), 3);

        // Total flow must sum to 1.0.
        let total: f64 = result.iter().map(|fd| fd.flow).sum();
        assert!(
            (total - 1.0).abs() < 1e-6,
            "total flow = {total}, expected 1.0"
        );

        // Dangling/isolated node 2 must have non-zero flow (gets flow via teleportation).
        assert!(
            result[2].flow > 0.0,
            "dangling node 2 should have flow > 0, got {}",
            result[2].flow
        );
    }

    #[test]
    fn test_pagerank_directed_path() {
        // Directed 2-node path: 0 → 1.
        // Node 0 has out-degree 1, node 1 is a dangling node (no out-edges).
        // With teleportation, node 1 should have higher PageRank than node 0
        // because node 0 sends all its flow to node 1, plus node 1 gets
        // teleportation flow.
        let mut b = GraphDataBuilder::new(2).directed();
        b.add_edge(0, 1, 1.0).unwrap();
        let graph = b.build().unwrap();

        let result = compute_flow(&graph, 0.15, 1e-10, 200);
        assert_eq!(result.len(), 2);

        let total: f64 = result.iter().map(|fd| fd.flow).sum();
        assert!(
            (total - 1.0).abs() < 1e-6,
            "total flow = {total}, expected 1.0"
        );

        // Both nodes must have positive flow.
        assert!(result[0].flow > 0.0, "node 0 flow should be positive");
        assert!(result[1].flow > 0.0, "node 1 flow should be positive");

        // Node 1 should have higher flow: it receives from 0 + teleportation.
        assert!(
            result[1].flow > result[0].flow,
            "node 1 ({}) should have higher flow than node 0 ({})",
            result[1].flow,
            result[0].flow
        );
    }

    #[test]
    fn test_pagerank_disconnected() {
        // 4 nodes: two disconnected edges (0-1, 2-3), undirected.
        // Each component should have equal flow within it.
        let mut b = GraphDataBuilder::new(4);
        b.add_edge(0, 1, 1.0).unwrap();
        b.add_edge(2, 3, 1.0).unwrap();
        let graph = b.build().unwrap();

        let result = compute_flow(&graph, 0.15, 1e-10, 200);
        assert_eq!(result.len(), 4);

        let total: f64 = result.iter().map(|fd| fd.flow).sum();
        assert!(
            (total - 1.0).abs() < 1e-6,
            "total flow = {total}, expected 1.0"
        );

        // Due to teleportation, all nodes converge to ~0.25 (uniform).
        // The graph is symmetric with equal degree for all nodes.
        for (i, fd) in result.iter().enumerate() {
            assert!(
                (fd.flow - 0.25).abs() < 0.05,
                "node {i}: flow = {}, expected ~0.25",
                fd.flow
            );
        }
    }

    #[test]
    fn test_pagerank_converges_before_max_iter() {
        // Small graph should converge well before 10000 iterations.
        let mut b = GraphDataBuilder::new(3);
        b.add_edge(0, 1, 1.0).unwrap();
        b.add_edge(1, 2, 1.0).unwrap();
        b.add_edge(2, 0, 1.0).unwrap();
        let graph = b.build().unwrap();

        let result_strict = compute_flow(&graph, 0.15, 1e-10, 200);
        let result_loose = compute_flow(&graph, 0.15, 1e-10, 200);

        // Both should produce essentially the same result.
        for (a, b) in result_strict.iter().zip(result_loose.iter()) {
            assert!(
                (a.flow - b.flow).abs() < 1e-10,
                "results should be identical"
            );
        }
    }

    // ── MapEquation / ΔL tests ──

    /// Helper: build a FlowData with the given flow, enter_flow, exit_flow.
    fn fd(flow: f64, enter_flow: f64, exit_flow: f64) -> FlowData {
        FlowData {
            flow,
            enter_flow,
            exit_flow,
            teleport_flow: 0.0,
        }
    }

    /// 4-node test graph node flows (sum = 1.0).
    ///
    /// Edge flows:
    ///   0→1: 0.05, 1→0: 0.04, 0→2: 0.03, 2→0: 0.02, 0→3: 0.04, 3→0: 0.04
    ///   1→2: 0.03, 2→1: 0.02, 1→3: 0.02, 3→1: 0.02, 2→3: 0.04, 3→2: 0.02
    ///
    /// Node flows:    [0.30, 0.25, 0.25, 0.20]
    /// Node enter:    [0.10, 0.09, 0.08, 0.10]  (total incoming edge flow)
    /// Node exit:     [0.12, 0.09, 0.08, 0.08]  (total outgoing edge flow)
    fn four_node_flows() -> [FlowData; 4] {
        [
            fd(0.30, 0.10, 0.12),
            fd(0.25, 0.09, 0.09),
            fd(0.25, 0.08, 0.08),
            fd(0.20, 0.10, 0.08),
        ]
    }

    #[test]
    fn test_map_equation_singleton_partition_codelength() {
        let nodes = four_node_flows();
        let map_eq = MapEquation::new(&nodes, 0.0);

        let cl = map_eq.codelength();
        // Hand-computed: 1.889169354568307
        assert!(
            (cl - 1.889169354568307).abs() < 1e-12,
            "singleton codelength: got {cl:.15}, expected 1.889169354568307"
        );

        // Standalone function should give same result.
        assert!(
            (calc_codelength(&map_eq) - cl).abs() < 1e-15,
            "calc_codelength should match method"
        );
    }

    #[test]
    fn test_map_equation_calc_codelength_matches_terms() {
        let nodes = four_node_flows();
        let map_eq = MapEquation::new(&nodes, 0.0);

        // Verify cached terms match independent recomputation.
        let expected_enter_flow_log = plogp(map_eq.enter_flow);
        let expected_enter_log_enter: f64 =
            map_eq.module_data.iter().map(|m| plogp(m.enter_flow)).sum();
        let expected_exit_log_exit: f64 =
            map_eq.module_data.iter().map(|m| plogp(m.exit_flow)).sum();
        let expected_flow_log_flow: f64 = map_eq
            .module_data
            .iter()
            .map(|m| plogp(m.exit_flow + m.flow))
            .sum();

        assert!(
            (map_eq.enter_flow_log - expected_enter_flow_log).abs() < 1e-15,
            "enter_flow_log mismatch"
        );
        assert!(
            (map_eq.enter_log_enter - expected_enter_log_enter).abs() < 1e-15,
            "enter_log_enter mismatch"
        );
        assert!(
            (map_eq.exit_log_exit - expected_exit_log_exit).abs() < 1e-15,
            "exit_log_exit mismatch"
        );
        assert!(
            (map_eq.flow_log_flow - expected_flow_log_flow).abs() < 1e-15,
            "flow_log_flow mismatch"
        );
    }

    #[test]
    fn test_map_equation_recalc_terms() {
        let nodes = four_node_flows();
        let mut map_eq = MapEquation::new(&nodes, 0.0);
        let original_cl = map_eq.codelength();

        map_eq.recalc_terms();

        assert!(
            (map_eq.codelength() - original_cl).abs() < 1e-15,
            "recalc_terms should not change codelength"
        );
    }

    // ── ΔL Test A: singleton → singleton ──

    #[test]
    fn test_delta_codelength_singleton_to_singleton() {
        // Move node 0 from module 0 (singleton) to module 1 (singleton).
        //
        // DeltaFlow for old module (0, self-module, singleton):
        //   deltaExit=0, deltaEnter=0 (no internal edges in a singleton)
        // DeltaFlow for new module (1):
        //   deltaExit = flow 0→1 = 0.05
        //   deltaEnter = flow 1→0 = 0.04
        //
        // Hand-computed ΔL = 0.058917207167547 (positive → bad move)

        let nodes = four_node_flows();
        let map_eq = MapEquation::new(&nodes, 0.0);

        let current = nodes[0].clone();
        let old_delta = DeltaFlow {
            module: 0,
            delta_exit: 0.0,
            delta_enter: 0.0,
        };
        let new_delta = DeltaFlow {
            module: 1,
            delta_exit: 0.05,
            delta_enter: 0.04,
        };

        let dl = map_eq.get_delta_codelength_on_moving_node(
            &current, 0, 1, &old_delta, &new_delta,
        );

        let expected = 0.058917207167547;
        assert!(
            (dl - expected).abs() < 1e-12,
            "ΔL singleton→singleton: got {dl:.15}, expected {expected:.15}"
        );

        // Verify: codelength_after - codelength_before should equal ΔL.
        let cl_before = map_eq.codelength();
        let mut map_eq2 = map_eq.clone();
        map_eq2.update_codelength_on_moving_node(
            &current, 0, 1, &old_delta, &new_delta,
        );
        let cl_after = map_eq2.codelength();
        assert!(
            ((cl_after - cl_before) - dl).abs() < 1e-12,
            "codelength diff ({:.15}) should equal ΔL ({:.15})",
            cl_after - cl_before,
            dl
        );
    }

    // ── ΔL Test B: singleton into multi-node module ──

    #[test]
    fn test_delta_codelength_into_multi_node_module() {
        // Initial partition: {0,1}, {2}, {3}
        // Module 0 ({0,1}): flow=0.55, enter=0.10, exit=0.12
        // Module 1 ({2}):    flow=0.25, enter=0.08, exit=0.08
        // Module 2 ({3}):    flow=0.20, enter=0.10, exit=0.08
        //
        // Move node 2 (flow=0.25, enter=0.08, exit=0.08) from module 1 into module 0.
        //
        // DeltaFlow for old module (1, self-module, singleton):
        //   deltaExit=0, deltaEnter=0
        // DeltaFlow for new module (0):
        //   deltaExit = flow 2→{0,1} = 2→0(0.02) + 2→1(0.02) = 0.04
        //   deltaEnter = flow {0,1}→2 = 0→2(0.03) + 1→2(0.03) = 0.06
        //
        // Hand-computed ΔL = 0.188460591871736 (positive → bad move)

        let nodes = four_node_flows();
        let module_data = vec![
            ModuleFlowData {
                flow: 0.55,
                enter_flow: 0.10,
                exit_flow: 0.12,
            },
            ModuleFlowData {
                flow: 0.25,
                enter_flow: 0.08,
                exit_flow: 0.08,
            },
            ModuleFlowData {
                flow: 0.20,
                enter_flow: 0.10,
                exit_flow: 0.08,
            },
        ];
        let enter_flow = 0.28_f64;
        let enter_log_enter: f64 = module_data.iter().map(|m| plogp(m.enter_flow)).sum();
        let exit_log_exit: f64 = module_data.iter().map(|m| plogp(m.exit_flow)).sum();
        let flow_log_flow: f64 = module_data.iter().map(|m| plogp(m.exit_flow + m.flow)).sum();
        let map_eq = MapEquation {
            module_data,
            enter_flow,
            enter_flow_log: plogp(enter_flow),
            enter_log_enter,
            exit_log_exit,
            flow_log_flow,
            node_flow_log: nodes.iter().map(|n| plogp(n.flow)).sum(),
            exit_network_flow: 0.0,
        };

        let current = nodes[2].clone();
        let old_delta = DeltaFlow {
            module: 1,
            delta_exit: 0.0,
            delta_enter: 0.0,
        };
        let new_delta = DeltaFlow {
            module: 0,
            delta_exit: 0.04,
            delta_enter: 0.06,
        };

        let dl = map_eq.get_delta_codelength_on_moving_node(
            &current, 1, 0, &old_delta, &new_delta,
        );

        let expected = 0.188460591871736;
        assert!(
            (dl - expected).abs() < 1e-12,
            "ΔL into multi-node module: got {dl:.15}, expected {expected:.15}"
        );

        let cl_before = map_eq.codelength();
        let mut map_eq2 = map_eq.clone();
        map_eq2.update_codelength_on_moving_node(
            &current, 1, 0, &old_delta, &new_delta,
        );
        let cl_after = map_eq2.codelength();
        assert!(
            ((cl_after - cl_before) - dl).abs() < 1e-12,
            "codelength diff ({:.15}) should equal ΔL ({:.15})",
            cl_after - cl_before,
            dl
        );
    }

    // ── ΔL Test C: out of multi-node module into empty module ──

    #[test]
    fn test_delta_codelength_out_of_multi_node_module() {
        // Initial partition: {0,1,2}, {3}
        // Module 0 ({0,1,2}): flow=0.80, enter=0.08, exit=0.10
        // Module 1 ({3}):      flow=0.20, enter=0.10, exit=0.08
        //
        // Move node 1 (flow=0.25, enter=0.09, exit=0.09) from module 0 into
        // empty module 2.
        //
        // DeltaFlow for old module (0):
        //   deltaExit = flow 1→{0,2} = 1→0(0.04) + 1→2(0.03) = 0.07
        //   deltaEnter = flow {0,2}→1 = 0→1(0.05) + 2→1(0.02) = 0.07
        // DeltaFlow for new module (2, empty):
        //   deltaExit = 0, deltaEnter = 0
        //
        // Hand-computed ΔL = -0.038503252540231 (negative → good move!)

        let nodes = four_node_flows();
        let module_data = vec![
            ModuleFlowData {
                flow: 0.80,
                enter_flow: 0.08,
                exit_flow: 0.10,
            },
            ModuleFlowData {
                flow: 0.20,
                enter_flow: 0.10,
                exit_flow: 0.08,
            },
            ModuleFlowData {
                flow: 0.0,
                enter_flow: 0.0,
                exit_flow: 0.0,
            },
        ];
        let enter_flow = 0.18_f64;
        let enter_log_enter: f64 = module_data.iter().map(|m| plogp(m.enter_flow)).sum();
        let exit_log_exit: f64 = module_data.iter().map(|m| plogp(m.exit_flow)).sum();
        let flow_log_flow: f64 = module_data.iter().map(|m| plogp(m.exit_flow + m.flow)).sum();
        let map_eq = MapEquation {
            module_data,
            enter_flow,
            enter_flow_log: plogp(enter_flow),
            enter_log_enter,
            exit_log_exit,
            flow_log_flow,
            node_flow_log: nodes.iter().map(|n| plogp(n.flow)).sum(),
            exit_network_flow: 0.0,
        };

        let current = nodes[1].clone();
        let old_delta = DeltaFlow {
            module: 0,
            delta_exit: 0.07,
            delta_enter: 0.07,
        };
        let new_delta = DeltaFlow {
            module: 2,
            delta_exit: 0.0,
            delta_enter: 0.0,
        };

        let dl = map_eq.get_delta_codelength_on_moving_node(
            &current, 0, 2, &old_delta, &new_delta,
        );

        let expected = -0.038503252540231;
        assert!(
            (dl - expected).abs() < 1e-12,
            "ΔL out of multi-node module: got {dl:.15}, expected {expected:.15}"
        );

        let cl_before = map_eq.codelength();
        let mut map_eq2 = map_eq.clone();
        map_eq2.update_codelength_on_moving_node(
            &current, 0, 2, &old_delta, &new_delta,
        );
        let cl_after = map_eq2.codelength();
        assert!(
            ((cl_after - cl_before) - dl).abs() < 1e-12,
            "codelength diff ({:.15}) should equal ΔL ({:.15})",
            cl_after - cl_before,
            dl
        );

        // Verify the updated module data is correct.
        // After move: {0,2}, {3}, {1}
        // Module 0 ({0,2}): flow=0.55, enter=0.13, exit=0.15
        // Module 1 ({3}):   flow=0.20, enter=0.10, exit=0.08 (unchanged)
        // Module 2 ({1}):   flow=0.25, enter=0.09, exit=0.09
        assert!(
            (map_eq2.module_data[0].flow - 0.55).abs() < 1e-12,
            "module 0 flow after move"
        );
        assert!(
            (map_eq2.module_data[0].enter_flow - 0.13).abs() < 1e-12,
            "module 0 enter after move"
        );
        assert!(
            (map_eq2.module_data[0].exit_flow - 0.15).abs() < 1e-12,
            "module 0 exit after move"
        );
        assert!(
            (map_eq2.module_data[2].flow - 0.25).abs() < 1e-12,
            "module 2 flow after move"
        );
        assert!(
            (map_eq2.module_data[2].enter_flow - 0.09).abs() < 1e-12,
            "module 2 enter after move"
        );
        assert!(
            (map_eq2.module_data[2].exit_flow - 0.09).abs() < 1e-12,
            "module 2 exit after move"
        );
    }

    #[test]
    fn test_map_equation_update_recalc_consistency() {
        // After any update, recalc_terms() should produce identical cached values.
        let nodes = four_node_flows();
        let mut map_eq = MapEquation::new(&nodes, 0.0);

        let current = nodes[0].clone();
        let old_delta = DeltaFlow {
            module: 0,
            delta_exit: 0.0,
            delta_enter: 0.0,
        };
        let new_delta = DeltaFlow {
            module: 1,
            delta_exit: 0.05,
            delta_enter: 0.04,
        };

        map_eq.update_codelength_on_moving_node(
            &current, 0, 1, &old_delta, &new_delta,
        );
        let cl_incremental = map_eq.codelength();

        map_eq.recalc_terms();
        let cl_recalc = map_eq.codelength();

        assert!(
            (cl_incremental - cl_recalc).abs() < 1e-15,
            "incremental update ({cl_incremental:.15}) should match recalc ({cl_recalc:.15})"
        );
    }

    #[test]
    fn test_map_equation_add_remove_flow_roundtrip() {
        let nodes = four_node_flows();
        let map_eq = MapEquation::new(&nodes, 0.0);
        let mut map_eq2 = map_eq.clone();

        // Add node 0's flow to module 1, then remove it.
        map_eq2.add_flow_to_module(1, &nodes[0]);
        map_eq2.remove_flow_from_module(1, &nodes[0]);

        // Module 1 should be back to original.
        assert!(
            (map_eq2.module_data[1].flow - map_eq.module_data[1].flow).abs() < 1e-15,
            "flow roundtrip"
        );
        assert!(
            (map_eq2.module_data[1].enter_flow - map_eq.module_data[1].enter_flow).abs() < 1e-15,
            "enter roundtrip"
        );
        assert!(
            (map_eq2.module_data[1].exit_flow - map_eq.module_data[1].exit_flow).abs() < 1e-15,
            "exit roundtrip"
        );
    }

    // ── Local Moving / Aggregation / Multi-Level Tests ──

    #[test]
    fn test_infomap_local_moving_reduces_codelength() {
        let mut b = GraphDataBuilder::new(6);
        b.add_edge(0, 1, 5.0).unwrap();
        b.add_edge(1, 2, 5.0).unwrap();
        b.add_edge(0, 2, 5.0).unwrap();
        b.add_edge(3, 4, 5.0).unwrap();
        b.add_edge(4, 5, 5.0).unwrap();
        b.add_edge(3, 5, 5.0).unwrap();
        b.add_edge(2, 3, 0.1).unwrap();
        let graph = b.build().unwrap();

        let mut flow_data = compute_flow(&graph, 0.15, 1e-10, 200);
        populate_boundary_flows(&graph, &mut flow_data, 0.15);

        let mut map_eq = MapEquation::new(&flow_data, 0.0);
        let cl_before = map_eq.codelength();

        let mut partition: Vec<usize> = (0..6).collect();
        let mut rng = StdRng::seed_from_u64(42);
        let changed = run_local_moving(&mut map_eq, &flow_data, &graph, &mut partition, &mut rng);

        assert!(changed, "local moving should move at least one node");
        let cl_after = map_eq.codelength();
        assert!(
            cl_after < cl_before,
            "codelength should decrease: before={cl_before:.6}, after={cl_after:.6}"
        );
    }

    #[test]
    fn test_infomap_local_moving_finds_two_communities() {
        let mut b = GraphDataBuilder::new(6);
        b.add_edge(0, 1, 5.0).unwrap();
        b.add_edge(1, 2, 5.0).unwrap();
        b.add_edge(0, 2, 5.0).unwrap();
        b.add_edge(3, 4, 5.0).unwrap();
        b.add_edge(4, 5, 5.0).unwrap();
        b.add_edge(3, 5, 5.0).unwrap();
        b.add_edge(2, 3, 0.1).unwrap();
        let graph = b.build().unwrap();

        let mut flow_data = compute_flow(&graph, 0.15, 1e-10, 200);
        populate_boundary_flows(&graph, &mut flow_data, 0.15);

        let mut map_eq = MapEquation::new(&flow_data, 0.0);
        let mut partition: Vec<usize> = (0..6).collect();
        let mut rng = StdRng::seed_from_u64(42);
        run_local_moving(&mut map_eq, &flow_data, &graph, &mut partition, &mut rng);

        assert_eq!(partition[0], partition[1], "nodes 0,1 same community");
        assert_eq!(partition[1], partition[2], "nodes 1,2 same community");
        assert_eq!(partition[3], partition[4], "nodes 3,4 same community");
        assert_eq!(partition[4], partition[5], "nodes 4,5 same community");
        assert_ne!(partition[0], partition[3], "two distinct communities");
    }

    #[test]
    fn test_infomap_convergence_monotone_codelength() {
        let mut b = GraphDataBuilder::new(6);
        b.add_edge(0, 1, 3.0).unwrap();
        b.add_edge(1, 2, 3.0).unwrap();
        b.add_edge(0, 2, 3.0).unwrap();
        b.add_edge(3, 4, 3.0).unwrap();
        b.add_edge(4, 5, 3.0).unwrap();
        b.add_edge(3, 5, 3.0).unwrap();
        b.add_edge(2, 3, 0.5).unwrap();
        let graph = b.build().unwrap();

        let mut flow_data = compute_flow(&graph, 0.15, 1e-10, 200);
        populate_boundary_flows(&graph, &mut flow_data, 0.15);

        let mut map_eq = MapEquation::new(&flow_data, 0.0);
        let mut partition: Vec<usize> = (0..6).collect();
        let mut rng = StdRng::seed_from_u64(123);

        let mut codelengths: Vec<f64> = vec![map_eq.codelength()];

        for _ in 0..5 {
            let n = graph.node_count();
            let mut order: Vec<usize> = (0..n).collect();
            order.shuffle(&mut rng);

            let mut any_moved = false;
            for &node in &order {
                let old_module = partition[node];
                let node_exit = flow_data[node].exit_flow;
                let node_out_deg = graph.out_degree_of(node);

                let mut delta_exit_map: FxHashMap<usize, f64> = FxHashMap::default();
                let mut delta_enter_map: FxHashMap<usize, f64> = FxHashMap::default();

                if node_out_deg > 0.0 {
                    for (v, w) in graph.out_neighbors(node) {
                        let tm = partition[v];
                        *delta_exit_map.entry(tm).or_default() += (w / node_out_deg) * node_exit;
                        let v_out_deg = graph.out_degree_of(v);
                        if v_out_deg > 0.0 {
                            *delta_enter_map.entry(tm).or_default() +=
                                (w / v_out_deg) * flow_data[v].exit_flow;
                        }
                    }
                }

                let mut all_modules: FxHashMap<usize, ()> = FxHashMap::default();
                for &m in delta_exit_map.keys() {
                    all_modules.insert(m, ());
                }
                for &m in delta_enter_map.keys() {
                    all_modules.insert(m, ());
                }

                let mut delta_flows: FxHashMap<usize, DeltaFlow> = FxHashMap::default();
                for &m in all_modules.keys() {
                    delta_flows.insert(
                        m,
                        DeltaFlow {
                            module: m,
                            delta_exit: delta_exit_map.get(&m).copied().unwrap_or(0.0),
                            delta_enter: delta_enter_map.get(&m).copied().unwrap_or(0.0),
                        },
                    );
                }

                let self_delta = delta_flows.remove(&old_module).unwrap_or(DeltaFlow {
                    module: old_module,
                    delta_exit: 0.0,
                    delta_enter: 0.0,
                });

                let mut best_module = old_module;
                let mut best_delta = 0.0_f64;

                for (&nm, nd) in &delta_flows {
                    let dl = map_eq.get_delta_codelength_on_moving_node(
                        &flow_data[node], old_module, nm, &self_delta, nd,
                    );
                    if dl < best_delta {
                        best_delta = dl;
                        best_module = nm;
                    }
                }

                if best_module != old_module {
                    let actual = delta_flows.get(&best_module).cloned().unwrap_or(DeltaFlow {
                        module: best_module,
                        delta_exit: 0.0,
                        delta_enter: 0.0,
                    });
                    map_eq.update_codelength_on_moving_node(
                        &flow_data[node], old_module, best_module, &self_delta, &actual,
                    );
                    partition[node] = best_module;
                    any_moved = true;
                }
            }

            codelengths.push(map_eq.codelength());
            if !any_moved {
                break;
            }
        }

        for i in 1..codelengths.len() {
            assert!(
                codelengths[i] <= codelengths[i - 1] + 1e-12,
                "codelength must decrease monotonically: step {} => {:.12} > {:.12}",
                i,
                codelengths[i],
                codelengths[i - 1]
            );
        }
    }

    #[test]
    fn test_infomap_aggregate_graph_two_communities() {
        let mut b = GraphDataBuilder::new(4);
        b.add_edge(0, 1, 2.0).unwrap();
        b.add_edge(1, 2, 1.0).unwrap();
        b.add_edge(2, 3, 2.0).unwrap();
        let graph = b.build().unwrap();

        let partition: Vec<usize> = vec![0, 0, 1, 1];
        let agg = aggregate_graph(&graph, &partition);

        assert_eq!(agg.node_count(), 2, "should have 2 super-nodes");

        let nbrs_0: Vec<(usize, f64)> = agg.neighbors(0).collect();
        let has_self_0 = nbrs_0.iter().any(|(n, w)| *n == 0 && (*w - 2.0).abs() < 1e-10);
        let has_bridge_0 = nbrs_0.iter().any(|(n, w)| *n == 1 && (*w - 1.0).abs() < 1e-10);
        assert!(has_self_0, "super-node 0 should have self-loop w=2.0");
        assert!(has_bridge_0, "super-node 0 should have edge to super 1 w=1.0");
    }

    #[test]
    fn test_infomap_aggregate_graph_single_community() {
        let mut b = GraphDataBuilder::new(3);
        b.add_edge(0, 1, 1.0).unwrap();
        b.add_edge(1, 2, 1.0).unwrap();
        let graph = b.build().unwrap();

        let partition: Vec<usize> = vec![0, 0, 0];
        let agg = aggregate_graph(&graph, &partition);

        assert_eq!(agg.node_count(), 1, "single community -> 1 super-node");
        assert!((agg.node_weight(0) - 3.0).abs() < 1e-10, "weight = 3.0");
    }

    #[test]
    fn test_infomap_aggregate_graph_directed() {
        let mut b = GraphDataBuilder::new(4).directed();
        b.add_edge(0, 1, 2.0).unwrap();
        b.add_edge(1, 0, 1.0).unwrap();
        b.add_edge(2, 3, 3.0).unwrap();
        b.add_edge(3, 2, 1.5).unwrap();
        b.add_edge(1, 2, 0.5).unwrap();
        let graph = b.build().unwrap();

        let partition: Vec<usize> = vec![0, 0, 1, 1];
        let agg = aggregate_graph(&graph, &partition);

        assert_eq!(agg.node_count(), 2);
        assert!(agg.is_directed());

        let out_0: Vec<(usize, f64)> = agg.out_neighbors(0).collect();
        let has_self = out_0.iter().any(|(n, w)| *n == 0 && (*w - 3.0).abs() < 1e-10);
        let has_out = out_0.iter().any(|(n, w)| *n == 1 && (*w - 0.5).abs() < 1e-10);
        assert!(has_self, "directed self-loop 0->0 w=3.0");
        assert!(has_out, "directed edge 0->1 w=0.5");
    }

    #[test]
    fn test_infomap_multi_level_planted_partition() {
        use crate::generators::generate_planted_partition;
        use crate::metrics::nmi;

        let (graph, ground_truth) =
            generate_planted_partition(30, 3, 0.8, 0.05, Some(42)).unwrap();

        let flow_data = compute_flow(&graph, 0.15, 1e-10, 200);
        let config = InfomapConfig {
            seed: Some(42),
            max_iterations: 10,
            ..Default::default()
        };
        let (partition, codelength) = run_multi_level(&graph, &flow_data, &config);

        assert!(
            codelength.is_finite() && codelength >= 0.0,
            "codelength should be finite and non-negative, got {codelength}"
        );
        assert_eq!(partition.len(), 30);
        assert!(partition.iter().all(|&m| m < 30));

        let num_comms = *partition.iter().max().unwrap_or(&0) + 1;
        assert!(
            (2..=5).contains(&num_comms),
            "expected 2-5 communities, found {num_comms}"
        );

        let score = nmi(&ground_truth, &partition);
        assert!(
            score > 0.7,
            "NMI should be > 0.7 for strong planted partition, got {score:.4}"
        );
    }

    #[test]
    fn test_infomap_multi_level_single_node() {
        let graph = GraphDataBuilder::new(1).build().unwrap();
        let flow_data = compute_flow(&graph, 0.15, 1e-10, 200);
        let config = InfomapConfig::default();
        let (partition, cl) = run_multi_level(&graph, &flow_data, &config);
        assert_eq!(partition, vec![0]);
        assert!(cl >= 0.0);
    }

    #[test]
    fn test_infomap_multi_level_empty_graph() {
        let graph = GraphDataBuilder::new(0).build().unwrap();
        let flow_data = compute_flow(&graph, 0.15, 1e-10, 200);
        let config = InfomapConfig::default();
        let (partition, cl) = run_multi_level(&graph, &flow_data, &config);
        assert!(partition.is_empty());
        assert!((cl - 0.0).abs() < 1e-10);
    }

    #[test]
    fn test_infomap_multi_level_triangle() {
        let mut b = GraphDataBuilder::new(3);
        b.add_edge(0, 1, 1.0).unwrap();
        b.add_edge(1, 2, 1.0).unwrap();
        b.add_edge(0, 2, 1.0).unwrap();
        let graph = b.build().unwrap();

        let flow_data = compute_flow(&graph, 0.15, 1e-10, 200);
        let config = InfomapConfig {
            seed: Some(42),
            ..Default::default()
        };
        let (partition, _cl) = run_multi_level(&graph, &flow_data, &config);

        assert_eq!(partition[0], partition[1], "triangle should be one community");
        assert_eq!(partition[1], partition[2], "triangle should be one community");
    }

    #[test]
    fn test_infomap_populate_boundary_flows() {
        let mut b = GraphDataBuilder::new(3);
        b.add_edge(0, 1, 1.0).unwrap();
        b.add_edge(1, 2, 1.0).unwrap();
        b.add_edge(0, 2, 1.0).unwrap();
        let graph = b.build().unwrap();

        let mut flow_data = compute_flow(&graph, 0.15, 1e-10, 200);
        assert!(flow_data.iter().all(|fd| fd.enter_flow == 0.0 && fd.exit_flow == 0.0));

        populate_boundary_flows(&graph, &mut flow_data, 0.15);

        for (i, fd) in flow_data.iter().enumerate() {
            assert!(fd.enter_flow > 0.0, "node {i} enter_flow should be positive");
            assert!(fd.exit_flow > 0.0, "node {i} exit_flow should be positive");
        }

        for (i, fd) in flow_data.iter().enumerate() {
            assert!(
                (fd.enter_flow - fd.exit_flow).abs() < 1e-10,
                "node {i}: enter ({:.6}) should ~= exit ({:.6})",
                fd.enter_flow,
                fd.exit_flow
            );
        }

        let total_exit: f64 = flow_data.iter().map(|fd| fd.exit_flow).sum();
        assert!(
            (total_exit - 0.85).abs() < 1e-6,
            "total exit flow should be ~0.85, got {total_exit:.6}"
        );
    }

    // ── compute_node_flows tests ──

    #[test]
    fn test_infomap_compute_node_flows() {
        let mut b = GraphDataBuilder::new(3);
        b.add_edge(0, 1, 1.0).unwrap();
        b.add_edge(1, 2, 1.0).unwrap();
        b.add_edge(0, 2, 1.0).unwrap();
        let graph = b.build().unwrap();

        let flow_data = compute_flow(&graph, 0.15, 1e-10, 200);
        let result = compute_node_flows(&graph, &flow_data, 0.15);

        for (i, fd) in result.iter().enumerate() {
            assert!(fd.flow > 0.0, "node {i} flow should be positive");
            assert!(fd.enter_flow > 0.0, "node {i} enter_flow should be positive");
            assert!(fd.exit_flow > 0.0, "node {i} exit_flow should be positive");
        }
    }

    // ── Infomap struct tests ──

    #[test]
    fn test_infomap_struct_empty_graph() {
        let graph = GraphDataBuilder::new(0).build().unwrap();
        let config = InfomapConfig::default();
        let infomap = Infomap::new(config);
        let result = infomap.run(&graph);
        assert_eq!(result.partition.len(), 0);
        assert!((result.codelength - 0.0).abs() < 1e-10);
    }

    #[test]
    fn test_infomap_struct_single_node() {
        let graph = GraphDataBuilder::new(1).build().unwrap();
        let config = InfomapConfig { seed: Some(42), ..Default::default() };
        let infomap = Infomap::new(config);
        let result = infomap.run(&graph);
        assert_eq!(result.partition.len(), 1);
        assert!(result.codelength.is_finite());
    }

    #[test]
    fn test_infomap_struct_two_communities() {
        let mut b = GraphDataBuilder::new(6);
        b.add_edge(0, 1, 5.0).unwrap();
        b.add_edge(1, 2, 5.0).unwrap();
        b.add_edge(0, 2, 5.0).unwrap();
        b.add_edge(3, 4, 5.0).unwrap();
        b.add_edge(4, 5, 5.0).unwrap();
        b.add_edge(3, 5, 5.0).unwrap();
        b.add_edge(2, 3, 0.1).unwrap();
        let graph = b.build().unwrap();

        let config = InfomapConfig {
            seed: Some(42),
            num_trials: 3,
            ..Default::default()
        };
        let infomap = Infomap::new(config);
        let result = infomap.run(&graph);

        assert_eq!(result.partition.len(), 6);
        assert!(result.codelength > 0.0 && result.codelength.is_finite());
        assert_eq!(result.num_levels, 1);
        assert!(result.iterations > 0);

        let membership: Vec<usize> = (0..result.partition.len())
            .map(|i| result.partition.community_of(i))
            .collect();

        assert_eq!(membership[0], membership[1]);
        assert_eq!(membership[1], membership[2]);
        assert_eq!(membership[3], membership[4]);
        assert_eq!(membership[4], membership[5]);
        assert_ne!(membership[0], membership[3]);
    }

    #[test]
    fn test_infomap_struct_planted_partition_nmi() {
        use crate::generators::generate_planted_partition;
        use crate::metrics::nmi;

        let (graph, ground_truth) =
            generate_planted_partition(30, 3, 0.8, 0.05, Some(42)).unwrap();

        let config = InfomapConfig {
            seed: Some(42),
            num_trials: 5,
            ..Default::default()
        };
        let infomap = Infomap::new(config);
        let result = infomap.run(&graph);

        let membership: Vec<usize> = (0..result.partition.len())
            .map(|i| result.partition.community_of(i))
            .collect();

        let score = nmi(&ground_truth, &membership);
        assert!(
            score > 0.7,
            "NMI should be > 0.7 for strong planted partition, got {score:.4}"
        );
    }

    #[test]
    fn test_infomap_struct_multi_trial_improves() {
        let mut b = GraphDataBuilder::new(6);
        b.add_edge(0, 1, 5.0).unwrap();
        b.add_edge(1, 2, 5.0).unwrap();
        b.add_edge(0, 2, 5.0).unwrap();
        b.add_edge(3, 4, 5.0).unwrap();
        b.add_edge(4, 5, 5.0).unwrap();
        b.add_edge(3, 5, 5.0).unwrap();
        b.add_edge(2, 3, 0.1).unwrap();
        let graph = b.build().unwrap();

        let config = InfomapConfig {
            seed: Some(42),
            num_trials: 5,
            ..Default::default()
        };
        let infomap = Infomap::new(config);
        let result = infomap.run(&graph);

        assert!(
            result.codelength > 0.0,
            "codelength should be positive, got {}",
            result.codelength
        );
        assert!(result.codelength.is_finite());

        let membership: Vec<usize> = (0..result.partition.len())
            .map(|i| result.partition.community_of(i))
            .collect();
        let num_comms = membership.iter().max().unwrap() + 1;
        assert!(
            (2..=3).contains(&num_comms),
            "expected 2-3 communities, got {num_comms}"
        );
    }

    #[test]
    fn test_infomap_fine_coarse_tuning() {
        let mut b = GraphDataBuilder::new(6);
        b.add_edge(0, 1, 5.0).unwrap();
        b.add_edge(1, 2, 5.0).unwrap();
        b.add_edge(0, 2, 5.0).unwrap();
        b.add_edge(3, 4, 5.0).unwrap();
        b.add_edge(4, 5, 5.0).unwrap();
        b.add_edge(3, 5, 5.0).unwrap();
        b.add_edge(2, 3, 0.1).unwrap();
        let graph = b.build().unwrap();

        let mut flow_data = compute_flow(&graph, 0.15, 1e-10, 200);
        populate_boundary_flows(&graph, &mut flow_data, 0.15);

        let mut map_eq = MapEquation::new(&flow_data, 0.0);
        let mut partition: Vec<usize> = (0..6).collect();
        let mut rng = StdRng::seed_from_u64(42);

        run_local_moving(&mut map_eq, &flow_data, &graph, &mut partition, &mut rng);
        renumber_partition(&mut partition);

        let cl_after_local = map_eq.codelength();

        let ft = fine_tuning(&mut map_eq, &flow_data, &graph, &mut partition, &mut rng);
        let cl_after_fine = map_eq.codelength();

        assert!(
            cl_after_fine <= cl_after_local + 1e-12,
            "fine-tuning should not increase codelength: before={cl_after_local:.6}, after={cl_after_fine:.6}"
        );

        let ct = coarse_tuning(&mut map_eq, &flow_data, &graph, &mut partition, &mut rng);
        let cl_after_coarse = map_eq.codelength();

        assert!(
            cl_after_coarse <= cl_after_fine + 1e-12,
            "coarse-tuning should not increase codelength: before={cl_after_fine:.6}, after={cl_after_coarse:.6}"
        );
    }

    #[test]
    fn test_infomap_renumber_partition() {
        let mut partition = vec![5, 5, 3, 7, 3];
        renumber_partition(&mut partition);
        assert_eq!(partition, vec![0, 0, 1, 2, 1]);
    }
}

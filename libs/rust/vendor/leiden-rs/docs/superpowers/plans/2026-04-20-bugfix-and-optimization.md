# Bugfix & Optimization Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Fix correctness/robustness issues and performance optimizations identified in the codebase audit, ordered by priority.

**Architecture:** Targeted fixes to existing modules — no structural changes. Each task is an independent patch that can be verified in isolation.

**Tech Stack:** Rust (edition 2024), cargo test, cargo clippy.

**Baseline:** 154 tests passing, 0 failures.

---

## Task 1: Remove `#[allow(dead_code)]` from `GraphData`

**Files:**
- Modify: `src/graph/data.rs:16`

**Problem:** `#[allow(dead_code)]` suppresses compiler warnings on `GraphData` even though all `pub(crate)` fields are used internally. This is a leftover from early development.

**Root Cause:** Initially some fields may not have been used; now they all are.

**Fix:** Remove the attribute and confirm no warnings.

- [ ] **Step 1: Remove the attribute**

In `src/graph/data.rs`, change:
```rust
#[derive(Debug, Clone)]
#[allow(dead_code)]
pub struct GraphData {
```
to:
```rust
#[derive(Debug, Clone)]
pub struct GraphData {
```

- [ ] **Step 2: Verify no warnings**

Run: `cargo check 2>&1`
Expected: No warnings about dead code on `GraphData`.

---

## Task 2: Add `#[inline]` to quality function `delta_move_from_components` implementations

**Files:**
- Modify: `src/quality.rs:39,134,207,240`

**Problem:** The concrete quality function implementations of `delta_move_from_components` lack `#[inline]`. When monomorphized through `QualityFn` enum dispatch (which has `#[inline]`), the compiler may or may not inline through the trait dispatch. Adding `#[inline]` on the concrete impls gives the compiler the hint.

**Root Cause:** Omission during initial implementation — the enum wrapper has `#[inline]` but the underlying structs don't.

**Fix:** Add `#[inline]` to all four `delta_move_from_components` trait impl methods.

- [ ] **Step 1: Add `#[inline]` to `Modularity::delta_move_from_components`**

In `src/quality.rs`, line 39:
```rust
impl QualityFunction for Modularity {
    #[inline]
    fn delta_move_from_components(&self, c: &MoveComponents) -> f64 {
```

- [ ] **Step 2: Add `#[inline]` to `CPM::delta_move_from_components`**

In `src/quality.rs`, line 134:
```rust
impl QualityFunction for CPM {
    #[inline]
    fn delta_move_from_components(&self, c: &MoveComponents) -> f64 {
```

- [ ] **Step 3: Add `#[inline]` to `RBConfiguration::delta_move_from_components`**

In `src/quality.rs`, line 207:
```rust
impl QualityFunction for RBConfiguration {
    #[inline]
    fn delta_move_from_components(&self, c: &MoveComponents) -> f64 {
```

- [ ] **Step 4: Add `#[inline]` to `RBER::delta_move_from_components`**

In `src/quality.rs`, line 240:
```rust
impl QualityFunction for RBER {
    #[inline]
    fn delta_move_from_components(&self, c: &MoveComponents) -> f64 {
```

- [ ] **Step 5: Verify tests still pass**

Run: `cargo test`
Expected: All tests pass (same count as baseline).

---

## Task 3: Add `max_depth` guard to `resolution_profile` bisection

**Files:**
- Modify: `src/resolution.rs:138-227`

**Problem:** `resolution_profile` uses unbounded recursive bisection. If `min_diff_resolution` is very small and partitions change at every point, the recursion depth can grow large (up to `log2(range/min_diff)`). While not a practical problem for typical parameters, there's no safety net.

**Root Cause:** The original implementation follows leidenalg's approach but omits a depth guard.

**Fix:** Add a `depth` parameter to `bisect()`, increment it on each recursive call, and stop when it exceeds `max_depth` (default 50).

- [ ] **Step 1: Add `max_depth` parameter to `bisect` signature**

In `src/resolution.rs`, change the `bisect` function signature from:
```rust
fn bisect(
    data: &GraphData,
    quality: QualityType,
    gamma_low: f64,
    gamma_high: f64,
    seed: u64,
    min_diff_resolution: f64,
    entries: &mut Vec<ResolutionEntry>,
) -> crate::error::Result<()> {
```
to:
```rust
fn bisect(
    data: &GraphData,
    quality: QualityType,
    gamma_low: f64,
    gamma_high: f64,
    seed: u64,
    min_diff_resolution: f64,
    max_depth: usize,
    depth: usize,
    entries: &mut Vec<ResolutionEntry>,
) -> crate::error::Result<()> {
```

- [ ] **Step 2: Add depth check at the start of `bisect`**

After the `n == 0` early return block, before creating `config_low`, add:
```rust
    if depth >= max_depth {
        let config = LeidenConfig {
            resolution: gamma_low,
            quality,
            seed: Some(seed),
            ..Default::default()
        };
        let LeidenOutput {
            partition: p,
            quality: q,
        } = Leiden::new(config).run(data)?;
        entries.push(ResolutionEntry {
            resolution: gamma_low,
            num_communities: p.num_communities(),
            quality: q,
            partition: p,
        });
        return Ok(());
    }
```

- [ ] **Step 3: Update recursive calls to pass `depth + 1`**

In the recursive branch of `bisect`, change:
```rust
        bisect(
            data,
            quality,
            gamma_low,
            mid,
            seed.wrapping_add(2),
            min_diff_resolution,
            entries,
        )?;
        bisect(
            data,
            quality,
            mid,
            gamma_high,
            seed.wrapping_add(3),
            min_diff_resolution,
            entries,
        )?;
```
to:
```rust
        bisect(
            data,
            quality,
            gamma_low,
            mid,
            seed.wrapping_add(2),
            min_diff_resolution,
            max_depth,
            depth + 1,
            entries,
        )?;
        bisect(
            data,
            quality,
            mid,
            gamma_high,
            seed.wrapping_add(3),
            min_diff_resolution,
            max_depth,
            depth + 1,
            entries,
        )?;
```

- [ ] **Step 4: Update the call site in `resolution_profile` to pass initial depth**

In `resolution_profile`, change:
```rust
    bisect(
        data,
        quality,
        resolution_range.0,
        resolution_range.1,
        seed.unwrap_or(0),
        min_diff_resolution,
        &mut entries,
    )?;
```
to:
```rust
    bisect(
        data,
        quality,
        resolution_range.0,
        resolution_range.1,
        seed.unwrap_or(0),
        min_diff_resolution,
        50,
        0,
        &mut entries,
    )?;
```

- [ ] **Step 5: Verify tests still pass**

Run: `cargo test`
Expected: All tests pass.

---

## Task 4: Convert `metrics::nmi` and `metrics::ari` from panic to `Result` for length mismatch

**Files:**
- Modify: `src/metrics.rs:17-101` (nmi)
- Modify: `src/metrics.rs:115-183` (ari)
- Modify: `src/lib.rs:56` (re-export)

**Problem:** `nmi()` and `ari()` panic on length mismatch. Public library APIs should not panic on invalid input — they should return `Result`.

**Root Cause:** The `assert!` was used for brevity during initial implementation.

**Fix:** Add `try_nmi` / `try_ari` that return `Result`, keep `nmi` / `ari` as convenience wrappers (still panicking, but documented). Also update `lib.rs` to re-export the new functions.

**Design decision:** Keep the original `nmi`/`ari` functions as-is (with documented panic behavior) to avoid breaking changes. Add new `try_nmi`/`try_ari` that return `Result<f64, LeidenError>`.

- [ ] **Step 1: Add `try_nmi` function to `src/metrics.rs`**

Add this function right after the existing `nmi` function (after line 101):
```rust
/// Fallible version of [`nmi`] that returns an error instead of panicking.
///
/// Returns `Err(LeidenError::InvalidParameter)` if the two slices have
/// different lengths.
pub fn try_nmi<T1: AsRef<[usize]> + ?Sized, T2: AsRef<[usize]> + ?Sized>(
    partition_true: &T1,
    partition_pred: &T2,
) -> crate::error::Result<f64> {
    let partition_true = partition_true.as_ref();
    let partition_pred = partition_pred.as_ref();

    if partition_true.len() != partition_pred.len() {
        return Err(crate::error::LeidenError::InvalidParameter {
            message: format!(
                "nmi: partition lengths differ ({} vs {})",
                partition_true.len(),
                partition_pred.len()
            ),
        });
    }

    Ok(nmi(partition_true, partition_pred))
}
```

- [ ] **Step 2: Add `try_ari` function to `src/metrics.rs`**

Add this function right after the existing `ari` function (after line 183):
```rust
/// Fallible version of [`ari`] that returns an error instead of panicking.
///
/// Returns `Err(LeidenError::InvalidParameter)` if the two slices have
/// different lengths.
pub fn try_ari<T1: AsRef<[usize]> + ?Sized, T2: AsRef<[usize]> + ?Sized>(
    partition_true: &T1,
    partition_pred: &T2,
) -> crate::error::Result<f64> {
    let partition_true = partition_true.as_ref();
    let partition_pred = partition_pred.as_ref();

    if partition_true.len() != partition_pred.len() {
        return Err(crate::error::LeidenError::InvalidParameter {
            message: format!(
                "ari: partition lengths differ ({} vs {})",
                partition_true.len(),
                partition_pred.len()
            ),
        });
    }

    Ok(ari(partition_true, partition_pred))
}
```

- [ ] **Step 3: Update `lib.rs` to re-export `try_nmi` and `try_ari`**

In `src/lib.rs`, change line 56:
```rust
pub use metrics::{ari, conductance, coverage, internal_density, nmi};
```
to:
```rust
pub use metrics::{ari, conductance, coverage, internal_density, nmi, try_ari, try_nmi};
```

- [ ] **Step 4: Add tests for `try_nmi` and `try_ari`**

In the `tests` module of `src/metrics.rs`, add:
```rust
    #[test]
    fn test_try_nmi_length_mismatch() {
        let p1 = vec![0, 0, 1];
        let p2 = vec![0, 1];
        let result = try_nmi(&p1, &p2);
        assert!(result.is_err());
    }

    #[test]
    fn test_try_ari_length_mismatch() {
        let p1 = vec![0, 0, 1];
        let p2 = vec![0, 1];
        let result = try_ari(&p1, &p2);
        assert!(result.is_err());
    }

    #[test]
    fn test_try_nmi_matches_nmi() {
        let p1 = vec![0, 0, 1, 1, 2, 2];
        let p2 = vec![0, 1, 0, 1, 0, 1];
        assert!((try_nmi(&p1, &p2).unwrap() - nmi(&p1, &p2)).abs() < 1e-10);
    }

    #[test]
    fn test_try_ari_matches_ari() {
        let p1 = vec![0, 0, 0, 1, 1, 1];
        let p2 = vec![0, 0, 1, 1, 2, 2];
        assert!((try_ari(&p1, &p2).unwrap() - ari(&p1, &p2)).abs() < 1e-10);
    }
```

- [ ] **Step 5: Verify tests still pass**

Run: `cargo test`
Expected: All tests pass (4 new tests added).

---

## Task 5: Sort edges by `(src, dst)` in CSR builder for cache-friendly neighbor iteration

**Files:**
- Modify: `src/graph/builder.rs:122-195` (build_undirected_csr)
- Modify: `src/graph/builder.rs:204-298` (build_directed_csr)

**Problem:** Edges are stored in CSR in insertion order. Sorting by `(src, dst)` before building CSR ensures neighbors are in ascending ID order, improving spatial locality when accessing `partition.community_of(neighbor)`.

**Root Cause:** No explicit sorting step in the build pipeline.

**Fix:** Sort the edge list by `(src, dst)` before counting degrees and building CSR offsets.

- [ ] **Step 1: Sort edges in `build_undirected_csr`**

In `src/graph/builder.rs`, add a sort at the beginning of `build_undirected_csr`, right after the function signature. Change:
```rust
fn build_undirected_csr(
    mut edges: Vec<(usize, usize, f64)>,
    node_weights: Vec<f64>,
) -> Result<GraphData> {
```
Note: we need to take ownership of `edges` by removing the `&` if present. Currently the signature takes `edges: Vec<...>` by value, so we can sort in place. Add after the opening brace:
```rust
    edges.sort_by(|a, b| (a.0, a.1).cmp(&(b.0, b.1)));
```

Wait — the current signature is `edges: Vec<(usize, usize, f64)>` (by value). But in `build_undirected_csr`, the parameter is `edges: Vec<...>` already. Let me re-check.

Actually looking at the code, `build_undirected_csr` takes `edges: Vec<(usize, usize, f64)>` by value. So we just add the sort.

Add right after the opening brace of `build_undirected_csr`:
```rust
    edges.sort_by(|a, b| (a.0, a.1).cmp(&(b.0, b.1)));
```

- [ ] **Step 2: Sort edges in `build_directed_csr`**

Similarly, add at the beginning of `build_directed_csr`:
```rust
    edges.sort_by(|a, b| (a.0, a.1).cmp(&(b.0, b.1)));
```

- [ ] **Step 3: Verify tests still pass**

Run: `cargo test`
Expected: All tests pass. Sorting is deterministic so seeded tests should produce identical results.

---

## Task 6: Fix WASM binding — use `Leiden::run` instead of `run_multiplex` single-layer workaround

**Files:**
- Modify: `src/wasm.rs`

**Problem:** `leiden_from_edgelist` uses `run_multiplex` with a single layer as a workaround. This creates unnecessary overhead (layer validation, layer weight handling) compared to using `Leiden::run` directly. Also, errors are silently swallowed.

**Root Cause:** The function was written to reuse multiplex infrastructure rather than the simpler single-layer API.

**Fix:** Replace `run_multiplex` with `Leiden::run`, keep the same function signature for backward compatibility.

- [ ] **Step 1: Rewrite `leiden_from_edgelist` to use `Leiden::run`**

Replace the entire body of `leiden_from_edgelist` in `src/wasm.rs`:
```rust
use crate::graph::GraphDataBuilder;
use crate::leiden::{Leiden, LeidenConfig};

/// Run Leiden community detection from a flat edge list.
///
/// # Arguments
/// * `edges` - Flat array of `[src0, dst0, weight0, src1, dst1, weight1, ...]`
/// * `num_nodes` - Total number of nodes (IDs must be in `0..num_nodes`)
/// * `seed` - Optional RNG seed for reproducibility
///
/// # Returns
/// Community assignment vector where `result[i]` is the community ID of node `i`.
/// Returns singleton partition (each node in its own community) on error.
#[cfg_attr(feature = "wasm", wasm_bindgen::prelude::wasm_bindgen)]
pub fn leiden_from_edgelist(edges: &[f64], num_nodes: usize, seed: Option<u64>) -> Vec<usize> {
    let mut builder = GraphDataBuilder::new(num_nodes);
    for chunk in edges.chunks_exact(3) {
        let u = chunk[0] as usize;
        let v = chunk[1] as usize;
        let w = chunk[2];
        if builder.add_edge(u, v, w).is_err() {
            return (0..num_nodes).collect();
        }
    }
    let graph_data = match builder.build() {
        Ok(g) => g,
        Err(_) => return (0..num_nodes).collect(),
    };

    let config = LeidenConfig {
        seed,
        ..Default::default()
    };

    match Leiden::new(config).run(&graph_data) {
        Ok(result) => {
            let membership: Vec<usize> = (0..num_nodes)
                .map(|i| result.partition.community_of(i))
                .collect();
            membership
        }
        Err(_) => (0..num_nodes).collect(),
    }
}
```

- [ ] **Step 2: Verify tests still pass**

Run: `cargo test`
Expected: All tests pass.

---

## Task 7: Document parallel local moving approximation behavior

**Files:**
- Modify: `src/leiden.rs` (doc comments on `LeidenConfig::parallel_local_moving_threshold`)

**Problem:** The parallel local moving path uses a "relaxed" consistency model — nodes within the same color group see a snapshot of community statistics from the start of that group's processing, not real-time updates from other same-group nodes. While this is a standard technique in graph coloring-based parallelism, it's not documented.

**Root Cause:** Implementation was correct but documentation was not updated when parallel support was added.

**Fix:** Add documentation to `LeidenConfig::parallel_local_moving_threshold` and the `local_moving_parallel` function.

- [ ] **Step 1: Update doc comment on `parallel_local_moving_threshold`**

In `src/leiden.rs`, update the doc comment for the `parallel_local_moving_threshold` field:
```rust
    /// Minimum edge slots (CSR entries) for parallel local moving (default: 2000).
    /// Also requires at least 100 nodes. Only depends on graph structure for determinism.
    ///
    /// **Note:** The parallel path uses graph coloring to partition nodes into
    /// independent sets. Nodes in the same color group are processed concurrently
    /// using a shared snapshot of community statistics. This "relaxed" consistency
    /// model may produce slightly different results compared to the sequential path,
    /// but typically converges to the same partition quality. The sequential path is
    /// always used as a final refinement pass after parallel processing.
    pub parallel_local_moving_threshold: Option<usize>,
```

- [ ] **Step 2: Add doc comment to `local_moving_parallel`**

In `src/leiden.rs`, add a doc comment to `fn local_moving_parallel`:
```rust
/// Parallel local moving using graph coloring.
///
/// Nodes are colored so that same-color nodes form independent sets (no edges
/// between them). Each color group is processed in parallel using Rayon. Within
/// a group, all nodes see the same snapshot of community statistics. Moves are
/// collected and applied sequentially at the end of each color group.
///
/// This relaxed consistency model may produce slightly different results than
/// [`algorithm::local_moving_generic`]. When the parallel pass does not converge
/// naturally (detected by `!converged_naturally`), the caller falls back to a
/// sequential pass for final refinement.
```

- [ ] **Step 3: Verify no warnings**

Run: `cargo check`
Expected: No warnings.

---

## Self-Review Checklist

**1. Spec coverage:** Each issue from the audit has a corresponding task:
- 🔴 #1 (dead_code) → Task 1
- 🔴 #3 (recursive depth) → Task 3
- 🟡 #4 (edge sort) → Task 5
- 🟡 #5 (MoveComponents) → deferred (requires architectural changes, risk of regression)
- 🟡 #6 (FxHashMap alternative) → deferred (needs benchmarking, unclear win)
- 🟡 #7 (neighbor_slices in total_quality) → deferred (minor, total_quality not hot path)
- 🟢 #8 (Builder consuming API) → deferred (API expansion, not a fix)
- 🟢 #9 (WASM) → Task 6
- 🟢 #10 (dead_code) → Task 1
- 🟢 #11 (LFR rng) → deferred (minor, called once)
- 🟢 #12 (metrics panic) → Task 4
- 🟢 #13 (#[inline]) → Task 2
- 🟢 #14 (no_std) → deferred (major effort, separate plan)

**2. Placeholder scan:** No TBD/TODO found. All steps have complete code.

**3. Type consistency:** All function signatures, parameter names, and types are consistent across tasks.

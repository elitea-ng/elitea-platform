// Portions ported from Aider (https://github.com/Aider-AI/aider), aider/repomap.py @ 5dc9490bb35f9729ef2c95d00a19ccd30c26339c. Apache License 2.0. Copyright Paul Gauthier and Aider contributors.
//
// Aider ranks a repository's files with `networkx.pagerank(G,
// weight="weight", personalization=p, dangling=p)`; this is that call
// without networkx (its `_pagerank_scipy` power iteration), so the repo map
// in elitea-inventory-core ranks files the way Aider does. Changes: a dense
// node range and an edge list instead of a networkx graph, an all-zero
// personalization means uniform (networkx raises ZeroDivisionError, which
// Aider answers by calling it again without one), and no error when the
// iteration does not converge (the last vector is returned).

//! Personalized `PageRank` by power iteration, with networkx's semantics.
//!
//! On a graph of `n` nodes and weighted directed edges (parallel edges add
//! up, as networkx's multigraph-to-matrix conversion sums them):
//!
//! ```text
//! x ← d · (x · M + Σ x[dangling] · p) + (1 − d) · p
//! ```
//!
//! from the uniform vector, where `M` is the row-normalised weight matrix,
//! `p` the normalised personalization and a dangling node (no outgoing
//! weight) hands its rank to `p`. It stops when the L1 change is below
//! `n · tol`, or after `max_iter` steps.
//!
//! The result is deterministic: the sums run in node order and then edge
//! order, never in hash order, so the same graph gives the same bits.

/// The iteration's parameters (networkx's defaults by [`Default`]).
#[derive(Debug, Clone, Copy, PartialEq)]
pub struct PageRankParams {
    /// The damping factor (networkx's `alpha`).
    pub damping: f64,
    /// The per-node convergence tolerance.
    pub tol: f64,
    pub max_iter: u32,
}

impl Default for PageRankParams {
    fn default() -> Self {
        Self {
            damping: 0.85,
            tol: 1e-6,
            max_iter: 100,
        }
    }
}

/// The `PageRank` of nodes `0..n` over `edges` (`(source, target,
/// weight)`), personalized toward `personalization` (one non-negative mass
/// per node; it need not sum to 1, and all zeros, or a slice of another
/// length, mean uniform). Dangling nodes redistribute to the
/// personalization, as Aider asks networkx to.
///
/// An edge with an endpoint outside `0..n`, or a weight that is negative,
/// zero or not finite, is ignored. The ranks sum to 1 (to rounding).
#[must_use]
pub fn personalized_pagerank(
    n: usize,
    edges: &[(u32, u32, f64)],
    personalization: &[f64],
    params: &PageRankParams,
) -> Vec<f64> {
    if n == 0 {
        return Vec::new();
    }
    let uniform = 1.0 / count_f64(n);
    let p = normalised(n, personalization).unwrap_or_else(|| vec![uniform; n]);

    // CSR by source: the edges of node u are targets[offsets[u]..offsets[u + 1]],
    // in the order given.
    let index = |node: u32| usize::try_from(node).ok().filter(|&i| i < n);
    let kept: Vec<(usize, usize, f64)> = edges
        .iter()
        .filter_map(|&(source, target, weight)| {
            let usable = weight.is_finite() && weight > 0.0;
            usable.then_some((index(source)?, index(target)?, weight))
        })
        .collect();
    let mut offsets = vec![0usize; n + 1];
    for &(source, _, _) in &kept {
        offsets[source + 1] += 1;
    }
    for i in 0..n {
        offsets[i + 1] += offsets[i];
    }
    let mut cursor = offsets.clone();
    let mut targets = vec![(0usize, 0.0f64); kept.len()];
    for &(source, target, weight) in &kept {
        targets[cursor[source]] = (target, weight);
        cursor[source] += 1;
    }
    // 1 / out-weight, 0 for a dangling node.
    let inverse: Vec<f64> = (0..n)
        .map(|u| {
            let total: f64 = targets[offsets[u]..offsets[u + 1]]
                .iter()
                .map(|&(_, w)| w)
                .sum();
            if total > 0.0 { 1.0 / total } else { 0.0 }
        })
        .collect();
    let dangling: Vec<usize> = (0..n).filter(|&u| inverse[u] == 0.0).collect();

    let d = params.damping;
    let threshold = count_f64(n) * params.tol;
    let mut x = vec![uniform; n];
    let mut next = vec![0.0f64; n];
    for _ in 0..params.max_iter {
        next.fill(0.0);
        for u in 0..n {
            let share = x[u] * inverse[u];
            if share == 0.0 {
                continue;
            }
            for &(v, w) in &targets[offsets[u]..offsets[u + 1]] {
                next[v] += share * w;
            }
        }
        let lost: f64 = dangling.iter().map(|&u| x[u]).sum();
        let mut err = 0.0;
        for v in 0..n {
            let value = d * (next[v] + lost * p[v]) + (1.0 - d) * p[v];
            err += (value - x[v]).abs();
            next[v] = value;
        }
        std::mem::swap(&mut x, &mut next);
        if err < threshold {
            break;
        }
    }
    x
}

/// `personalization` scaled to sum to 1, when it is one usable mass per
/// node.
fn normalised(n: usize, personalization: &[f64]) -> Option<Vec<f64>> {
    if personalization.len() != n {
        return None;
    }
    let clean: Vec<f64> = personalization
        .iter()
        .map(|&m| if m.is_finite() && m > 0.0 { m } else { 0.0 })
        .collect();
    let total: f64 = clean.iter().sum();
    (total > 0.0).then(|| clean.into_iter().map(|m| m / total).collect())
}

/// A node count as a float (counts stay far below 2^52).
#[allow(clippy::cast_precision_loss)]
fn count_f64(n: usize) -> f64 {
    n as f64
}

#[cfg(test)]
mod tests {
    use super::*;

    fn close(a: &[f64], b: &[f64], eps: f64) -> bool {
        a.len() == b.len() && a.iter().zip(b).all(|(x, y)| (x - y).abs() < eps)
    }

    #[test]
    fn dangling_mass_goes_to_personalization() {
        // 2 → 0 → 1, and 1 has no out-edge: its rank goes to node 2 (all
        // the personalization), not to every node. Then x2 = 0.15 + 0.85 x1,
        // x0 = 0.85 x2, x1 = 0.85 x0.
        let params = PageRankParams {
            tol: 1e-14,
            max_iter: 10_000,
            ..PageRankParams::default()
        };
        let ranks =
            personalized_pagerank(3, &[(2, 0, 1.0), (0, 1, 1.0)], &[0.0, 0.0, 1.0], &params);
        let x2 = 0.15 / (1.0 - 0.85f64.powi(3));
        let expected = [0.85 * x2, 0.85 * 0.85 * x2, x2];
        assert!(close(&ranks, &expected, 1e-9), "{ranks:?} vs {expected:?}");
        assert!((ranks.iter().sum::<f64>() - 1.0).abs() < 1e-9);
    }

    #[test]
    fn an_all_zero_personalization_is_uniform() {
        let edges = [(0, 1, 1.0), (1, 2, 2.0), (2, 0, 0.5)];
        let params = PageRankParams::default();
        let zero = personalized_pagerank(3, &edges, &[0.0; 3], &params);
        let uniform = personalized_pagerank(3, &edges, &[1.0; 3], &params);
        let missing = personalized_pagerank(3, &edges, &[], &params);
        assert_eq!(zero, uniform);
        assert_eq!(zero, missing);
    }

    #[test]
    fn unusable_edges_are_ignored() {
        let params = PageRankParams::default();
        let clean = personalized_pagerank(2, &[(0, 1, 1.0)], &[], &params);
        let noisy = personalized_pagerank(
            2,
            &[(0, 1, 1.0), (0, 7, 1.0), (1, 0, -1.0), (1, 0, f64::NAN)],
            &[],
            &params,
        );
        assert_eq!(clean, noisy);
        assert_eq!(
            personalized_pagerank(0, &[], &[], &params),
            Vec::<f64>::new()
        );
    }

    #[test]
    fn deterministic_across_runs() {
        let edges: Vec<(u32, u32, f64)> = (0..50u32)
            .flat_map(|u| {
                [
                    (u, (u * 7 + 3) % 50, 1.0 + f64::from(u % 5)),
                    (u, (u * 13 + 1) % 50, 0.1),
                ]
            })
            .collect();
        let personalization: Vec<f64> = (0..50).map(|i| f64::from(u8::from(i % 9 == 0))).collect();
        let params = PageRankParams::default();
        let first = personalized_pagerank(50, &edges, &personalization, &params);
        for _ in 0..5 {
            let again = personalized_pagerank(50, &edges, &personalization, &params);
            assert_eq!(
                first.iter().map(|r| r.to_bits()).collect::<Vec<_>>(),
                again.iter().map(|r| r.to_bits()).collect::<Vec<_>>()
            );
        }
        assert!(close(&[first.iter().sum::<f64>()], &[1.0], 1e-9));
    }
}

//! Evaluation metrics for community detection.

use crate::partition::Partition;
use crate::quality::GraphData;

/// Normalized Mutual Information between two membership vectors.
///
/// Returns 1.0 for identical partitions and 0.0 for independent ones.
/// Uses natural logarithm (ln) for all entropy/MI computations.
///
/// Accepts any type implementing `AsRef<[usize]>` (e.g., `&[usize]`,
/// `&Vec<usize>`, or `&Partition`).
///
/// # Panics
///
/// Panics if the two slices have different lengths.
#[must_use = "the computed value should be used"]
pub fn nmi<T1: AsRef<[usize]> + ?Sized, T2: AsRef<[usize]> + ?Sized>(
    partition_true: &T1,
    partition_pred: &T2,
) -> f64 {
    try_nmi(partition_true, partition_pred)
        .expect("nmi: partitions differ in length")
}

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

    let n = partition_true.len();
    if n == 0 {
        return Ok(1.0);
    }

    let max_true = *partition_true
        .iter()
        .max()
        .expect("nmi: non-empty partition must have a maximum label");
    let max_pred = *partition_pred
        .iter()
        .max()
        .expect("nmi: non-empty partition must have a maximum label");

    let rows = max_true + 1;
    let cols = max_pred + 1;

    // Build contingency table
    let mut contingency = vec![0usize; rows * cols];
    for i in 0..n {
        contingency[partition_true[i] * cols + partition_pred[i]] += 1;
    }

    // Row sums (a) and column sums (b)
    let mut a = vec![0usize; rows];
    let mut b = vec![0usize; cols];
    for i in 0..rows {
        for j in 0..cols {
            let val = contingency[i * cols + j];
            a[i] += val;
            b[j] += val;
        }
    }

    let nf = n as f64;
    let epsilon = 1e-10;

    // Compute mutual information
    let mut mi = 0.0f64;
    for i in 0..rows {
        if a[i] == 0 {
            continue;
        }
        for j in 0..cols {
            let n_ij = contingency[i * cols + j];
            if n_ij == 0 {
                continue;
            }
            mi += (n_ij as f64 / nf) * (nf * n_ij as f64 / (a[i] as f64 * b[j] as f64)).ln();
        }
    }

    // Compute entropies
    let mut h_true = 0.0f64;
    for &count in &a {
        if count > 0 {
            let p = count as f64 / nf;
            h_true -= p * p.ln();
        }
    }

    let mut h_pred = 0.0f64;
    for &count in &b {
        if count > 0 {
            let p = count as f64 / nf;
            h_pred -= p * p.ln();
        }
    }

    if h_true + h_pred < epsilon {
        return Ok(1.0);
    }
    if mi < epsilon {
        return Ok(0.0);
    }

    Ok(2.0 * mi / (h_true + h_pred))
}

/// Adjusted Rand Index between two membership vectors.
///
/// Returns 1.0 for perfect agreement. Values near 0 indicate random-level
/// agreement; negative values indicate worse than random.
///
/// Accepts any type implementing `AsRef<[usize]>` (e.g., `&[usize]`,
/// `&Vec<usize>`, or `&Partition`).
///
/// # Panics
///
/// Panics if the two slices have different lengths.
#[must_use = "the computed value should be used"]
pub fn ari<T1: AsRef<[usize]> + ?Sized, T2: AsRef<[usize]> + ?Sized>(
    partition_true: &T1,
    partition_pred: &T2,
) -> f64 {
    try_ari(partition_true, partition_pred)
        .expect("ari: partitions differ in length")
}

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

    let n = partition_true.len();
    if n == 0 {
        return Ok(1.0);
    }

    let max_true = *partition_true
        .iter()
        .max()
        .expect("ari: non-empty partition must have a maximum label");
    let max_pred = *partition_pred
        .iter()
        .max()
        .expect("ari: non-empty partition must have a maximum label");

    let rows = max_true + 1;
    let cols = max_pred + 1;

    // Build contingency table
    let mut contingency = vec![0usize; rows * cols];
    for i in 0..n {
        contingency[partition_true[i] * cols + partition_pred[i]] += 1;
    }

    // Row sums and column sums
    let mut a = vec![0usize; rows];
    let mut b = vec![0usize; cols];
    for i in 0..rows {
        for j in 0..cols {
            let val = contingency[i * cols + j];
            a[i] += val;
            b[j] += val;
        }
    }

    // Sum of C(n_ij, 2) over all cells
    let sum_comb_n: f64 = contingency
        .iter()
        .map(|&v| (v as f64) * (v as f64 - 1.0) / 2.0)
        .sum();

    // Sum of C(a_i, 2) over rows
    let sum_comb_a: f64 = a.iter().map(|&v| (v as f64) * (v as f64 - 1.0) / 2.0).sum();

    // Sum of C(b_j, 2) over columns
    let sum_comb_b: f64 = b.iter().map(|&v| (v as f64) * (v as f64 - 1.0) / 2.0).sum();

    let total_pairs = (n as f64) * (n as f64 - 1.0) / 2.0;

    if total_pairs == 0.0 {
        return Ok(1.0);
    }

    let expected = sum_comb_a * sum_comb_b / total_pairs;
    let max_val = (sum_comb_a + sum_comb_b) / 2.0;

    if (max_val - expected).abs() < 1e-10 {
        return Ok(1.0);
    }

    Ok((sum_comb_n - expected) / (max_val - expected))
}

// ---------------------------------------------------------------------------
// Shared helper — builds contingency table + row/column sums
// ---------------------------------------------------------------------------

/// Build a contingency table and row/column sums from two membership vectors.
///
/// Returns `(n, rows, cols, contingency, row_sums, col_sums)` where
/// `contingency` is a flat row-major `rows × cols` matrix, `row_sums`
/// is the sum of each row, and `col_sums` is the sum of each column.
/// Contingency table with metadata: `(n, rows, cols, flat_matrix, row_sums, col_sums)`.
type ContingencyTable = (usize, usize, usize, Vec<usize>, Vec<usize>, Vec<usize>);

fn build_contingency_table(
    partition_true: &[usize],
    partition_pred: &[usize],
) -> crate::error::Result<ContingencyTable> {
    if partition_true.len() != partition_pred.len() {
        return Err(crate::error::LeidenError::InvalidParameter {
            message: format!(
                "partitions differ in length ({} vs {})",
                partition_true.len(),
                partition_pred.len()
            ),
        });
    }

    let n = partition_true.len();
    let max_true = partition_true.iter().max().copied().unwrap_or(0);
    let max_pred = partition_pred.iter().max().copied().unwrap_or(0);
    let rows = max_true + 1;
    let cols = max_pred + 1;

    let mut contingency = vec![0usize; rows * cols];
    for i in 0..n {
        contingency[partition_true[i] * cols + partition_pred[i]] += 1;
    }

    let mut a = vec![0usize; rows];
    let mut b = vec![0usize; cols];
    for i in 0..rows {
        for j in 0..cols {
            let val = contingency[i * cols + j];
            a[i] += val;
            b[j] += val;
        }
    }

    Ok((n, rows, cols, contingency, a, b))
}

// ---------------------------------------------------------------------------
// VI (Variation of Information)
// ---------------------------------------------------------------------------

/// Variation of Information between two membership vectors.
///
/// VI = H(X) + H(Y) - 2·I(X;Y)
///
/// where H is entropy and I is mutual information. Returns 0.0 for identical
/// partitions and positive values otherwise. The metric is bounded below by 0
/// and above by log(n) where n is the number of elements.
///
/// Accepts any type implementing `AsRef<[usize]>` (e.g., `&[usize]`,
/// `&Vec<usize>`, or `&Partition`).
///
/// # Panics
///
/// Panics if the two slices have different lengths.
#[must_use = "the computed value should be used"]
pub fn vi<T1: AsRef<[usize]> + ?Sized, T2: AsRef<[usize]> + ?Sized>(
    partition_true: &T1,
    partition_pred: &T2,
) -> f64 {
    try_vi(partition_true, partition_pred)
        .expect("vi: partitions differ in length")
}

/// Fallible version of [`vi`] that returns an error instead of panicking.
///
/// Returns `Err(LeidenError::InvalidParameter)` if the two slices have
/// different lengths.
pub fn try_vi<T1: AsRef<[usize]> + ?Sized, T2: AsRef<[usize]> + ?Sized>(
    partition_true: &T1,
    partition_pred: &T2,
) -> crate::error::Result<f64> {
    let (n, rows, cols, contingency, a, b) =
        build_contingency_table(partition_true.as_ref(), partition_pred.as_ref())?;

    if n == 0 {
        return Ok(0.0);
    }

    let nf = n as f64;

    // Mutual information I(X;Y)
    let mut mi = 0.0f64;
    for i in 0..rows {
        if a[i] == 0 {
            continue;
        }
        for j in 0..cols {
            let n_ij = contingency[i * cols + j];
            if n_ij == 0 {
                continue;
            }
            mi += (n_ij as f64 / nf) * (nf * n_ij as f64 / (a[i] as f64 * b[j] as f64)).ln();
        }
    }

    // Entropy H(X) — true partition
    let mut h_true = 0.0f64;
    for &count in &a {
        if count > 0 {
            let p = count as f64 / nf;
            h_true -= p * p.ln();
        }
    }

    // Entropy H(Y) — predicted partition
    let mut h_pred = 0.0f64;
    for &count in &b {
        if count > 0 {
            let p = count as f64 / nf;
            h_pred -= p * p.ln();
        }
    }

    Ok(h_true + h_pred - 2.0 * mi)
}

// ---------------------------------------------------------------------------
// FMI (Fowlkes-Mallows Index)
// ---------------------------------------------------------------------------

/// Fowlkes-Mallows Index between two membership vectors.
///
/// FMI = TP / √((TP+FP) · (TP+FN))
///
/// where TP is the number of true-positive pairs (same community in both
/// partitions), FP is the number of false-positive pairs, and FN is the
/// number of false-negative pairs. All pair counts are derived from the
/// contingency table without explicit O(n²) enumeration.
///
/// Returns 1.0 for identical partitions and 0.0 when no pair agrees.
/// The metric ranges in [0, 1].
///
/// Accepts any type implementing `AsRef<[usize]>` (e.g., `&[usize]`,
/// `&Vec<usize>`, or `&Partition`).
///
/// # Panics
///
/// Panics if the two slices have different lengths.
#[must_use = "the computed value should be used"]
pub fn fmi<T1: AsRef<[usize]> + ?Sized, T2: AsRef<[usize]> + ?Sized>(
    partition_true: &T1,
    partition_pred: &T2,
) -> f64 {
    try_fmi(partition_true, partition_pred)
        .expect("fmi: partitions differ in length")
}

/// Fallible version of [`fmi`] that returns an error instead of panicking.
///
/// Returns `Err(LeidenError::InvalidParameter)` if the two slices have
/// different lengths.
pub fn try_fmi<T1: AsRef<[usize]> + ?Sized, T2: AsRef<[usize]> + ?Sized>(
    partition_true: &T1,
    partition_pred: &T2,
) -> crate::error::Result<f64> {
    let (n, _rows, _cols, contingency, a, b) =
        build_contingency_table(partition_true.as_ref(), partition_pred.as_ref())?;

    if n == 0 {
        return Ok(1.0);
    }

    let sum_comb_n: f64 = contingency
        .iter()
        .map(|&v| (v as f64) * (v as f64 - 1.0) / 2.0)
        .sum();
    let sum_comb_a: f64 = a
        .iter()
        .map(|&v| (v as f64) * (v as f64 - 1.0) / 2.0)
        .sum();
    let sum_comb_b: f64 = b
        .iter()
        .map(|&v| (v as f64) * (v as f64 - 1.0) / 2.0)
        .sum();

    // Edge cases where one or both have no co-occurring pairs
    if sum_comb_a == 0.0 && sum_comb_b == 0.0 {
        return Ok(1.0);
    }
    if sum_comb_a == 0.0 || sum_comb_b == 0.0 {
        return Ok(0.0);
    }

    Ok(sum_comb_n / (sum_comb_a * sum_comb_b).sqrt())
}

// ---------------------------------------------------------------------------
// Purity
// ---------------------------------------------------------------------------

/// Purity between two membership vectors.
///
/// Purity = (1/n) · Σ_k maxⱼ n_{kj}
///
/// where n_{kj} is the contingency table entry for true community k and
/// predicted community j. For each true community, the largest overlap
/// with any predicted cluster is summed. Returns 1.0 for perfectly aligned
/// partitions and lower values otherwise. The metric ranges in (0, 1].
///
/// Accepts any type implementing `AsRef<[usize]>` (e.g., `&[usize]`,
/// `&Vec<usize>`, or `&Partition`).
///
/// # Panics
///
/// Panics if the two slices have different lengths.
#[must_use = "the computed value should be used"]
pub fn purity<T1: AsRef<[usize]> + ?Sized, T2: AsRef<[usize]> + ?Sized>(
    partition_true: &T1,
    partition_pred: &T2,
) -> f64 {
    try_purity(partition_true, partition_pred)
        .expect("purity: partitions differ in length")
}

/// Fallible version of [`purity`] that returns an error instead of panicking.
///
/// Returns `Err(LeidenError::InvalidParameter)` if the two slices have
/// different lengths.
pub fn try_purity<T1: AsRef<[usize]> + ?Sized, T2: AsRef<[usize]> + ?Sized>(
    partition_true: &T1,
    partition_pred: &T2,
) -> crate::error::Result<f64> {
    let (n, rows, cols, contingency, _a, _b) =
        build_contingency_table(partition_true.as_ref(), partition_pred.as_ref())?;

    if n == 0 {
        return Ok(1.0);
    }

    let mut sum_max = 0usize;
    for i in 0..rows {
        let mut max_val = 0usize;
        for j in 0..cols {
            let val = contingency[i * cols + j];
            if val > max_val {
                max_val = val;
            }
        }
        sum_max += max_val;
    }

    Ok(sum_max as f64 / n as f64)
}

/// Conductance of each community: cut_c / min(vol_c, 2m - vol_c).
///
/// Returns a `Vec<f64>` with one entry per community. Communities with
/// zero volume or volume equal to `2m` have conductance 0.0.
#[must_use = "the computed value should be used"]
pub fn conductance(data: &GraphData, partition: &Partition) -> Vec<f64> {
    let n = data.node_count();
    let two_m = 2.0 * data.total_weight();
    let num_comms = partition.num_communities();

    let mut vol = vec![0.0f64; num_comms];
    let mut cut = vec![0.0f64; num_comms];

    for node in 0..n {
        let comm = partition.community_of(node);
        if comm >= num_comms {
            continue;
        }
        vol[comm] += data.degree_of(node);
        for (neighbor, weight) in data.neighbors(node) {
            if partition.community_of(neighbor) != comm {
                cut[comm] += weight;
            }
        }
        if data.is_directed() {
            for (neighbor, weight) in data.in_neighbors(node) {
                if partition.community_of(neighbor) != comm {
                    cut[comm] += weight;
                }
            }
        }
    }

    let mut result = Vec::with_capacity(num_comms);
    for c in 0..num_comms {
        if vol[c] == 0.0 || vol[c] == two_m {
            result.push(0.0);
        } else {
            let denom = vol[c].min(two_m - vol[c]);
            result.push(cut[c] / denom);
        }
    }
    result
}

/// Internal edge density of each community: 2 * e_c / (n_c * (n_c - 1)).
///
/// Communities with 0 or 1 nodes have density 1.0.
#[must_use = "the computed value should be used"]
pub fn internal_density(data: &GraphData, partition: &Partition) -> Vec<f64> {
    let n = data.node_count();
    let num_comms = partition.num_communities();

    let mut n_c = vec![0usize; num_comms];
    let mut e_c = vec![0.0f64; num_comms];

    for node in 0..n {
        let comm = partition.community_of(node);
        if comm >= num_comms {
            continue;
        }
        n_c[comm] += 1;
        for (neighbor, weight) in data.neighbors(node) {
            if data.is_directed() {
                if partition.community_of(neighbor) == comm {
                    e_c[comm] += weight;
                }
            } else if neighbor > node && partition.community_of(neighbor) == comm {
                e_c[comm] += weight;
            }
        }
    }

    let directed = data.is_directed();
    let mut result = Vec::with_capacity(num_comms);
    for c in 0..num_comms {
        if n_c[c] <= 1 {
            result.push(1.0);
        } else {
            let pairs = (n_c[c] * (n_c[c] - 1)) as f64;
            if directed {
                result.push(e_c[c] / pairs);
            } else {
                result.push(2.0 * e_c[c] / pairs);
            }
        }
    }
    result
}

/// Coverage: fraction of intra-community edges (sum of e_c / m).
#[must_use = "the computed value should be used"]
pub fn coverage(data: &GraphData, partition: &Partition) -> f64 {
    let m = data.total_weight();
    if m == 0.0 {
        return 0.0;
    }

    let n = data.node_count();
    let num_comms = partition.num_communities();
    let mut total_internal = 0.0f64;

    for node in 0..n {
        let comm = partition.community_of(node);
        if comm >= num_comms {
            continue;
        }
        for (neighbor, weight) in data.neighbors(node) {
            if data.is_directed() {
                if partition.community_of(neighbor) == comm {
                    total_internal += weight;
                }
            } else if neighbor > node && partition.community_of(neighbor) == comm {
                total_internal += weight;
            }
        }
    }

    total_internal / m
}

/// Size of each community (number of nodes).
///
/// Delegates to `Partition::community_sizes()`.
#[must_use = "the computed value should be used"]
pub fn community_sizes(partition: &Partition) -> Vec<usize> {
    partition.community_sizes()
}

// ---------------------------------------------------------------------------
// Homogeneity, Completeness, V-measure
// ---------------------------------------------------------------------------

/// Compute entropy H(X) from a list of category counts.
fn entropy(counts: &[usize], n: usize) -> f64 {
    let nf = n as f64;
    let mut h = 0.0;
    for &c in counts {
        if c > 0 {
            let p = c as f64 / nf;
            h -= p * p.ln();
        }
    }
    h
}

/// Compute joint entropy H(X,Y) from a flat row-major contingency matrix.
fn joint_entropy(contingency: &[usize], rows: usize, cols: usize, n: usize) -> f64 {
    let nf = n as f64;
    let mut h = 0.0;
    for i in 0..rows {
        for j in 0..cols {
            let val = contingency[i * cols + j];
            if val > 0 {
                let p = val as f64 / nf;
                h -= p * p.ln();
            }
        }
    }
    h
}

/// Compute C(n, k) in log-space using lgamma to avoid overflow.
fn log_comb(n: usize, k: usize) -> f64 {
    if k > n {
        return f64::NEG_INFINITY;
    }
    if k == 0 || k == n {
        return 0.0;
    }
    let k = k.min(n - k);
    let mut result = 0.0;
    for i in 0..k {
        result += ((n - i) as f64).ln();
        result -= ((k - i) as f64).ln();
    }
    result
}

/// Expected Mutual Information under the hypergeometric model of independent
/// partitions, using the Vinh et al. 2009 analytic formula.
fn expected_mutual_info(
    _rows: usize,
    _cols: usize,
    a: &[usize],
    b: &[usize],
    n: usize,
) -> f64 {
    if n <= 1 {
        return 0.0;
    }
    let nf = n as f64;
    let mut emi = 0.0;
    for &a_i in a.iter() {
        if a_i == 0 {
            continue;
        }
        let a_f = a_i as f64;
        for &b_j in b.iter() {
            if b_j == 0 {
                continue;
            }
            let b_f = b_j as f64;
            let min_k = (a_i + b_j).saturating_sub(n);
            let max_k = a_i.min(b_j);
            for k in min_k..=max_k {
                if k == 0 {
                    continue;
                }
                let kf = k as f64;
                let log_p = log_comb(a_i, k)
                    + log_comb(n - a_i, b_j - k)
                    - log_comb(n, b_j);
                let p = log_p.exp();
                if p < 1e-15 {
                    continue;
                }
                emi += p * (kf / nf) * (nf * kf / (a_f * b_f)).ln();
            }
        }
    }
    emi
}

/// Homogeneity between two membership vectors.
///
/// Measures whether each predicted cluster contains only members of a single
/// true class: h = 1 − H(C_true|C_pred) / H(C_true).
///
/// Returns 1.0 for perfectly homogeneous clusters. The metric is undefined
/// when H(C_true) = 0 (single true class), in which case 1.0 is returned.
///
/// Accepts any type implementing `AsRef<[usize]>` (e.g., `&[usize]`,
/// `&Vec<usize>`, or `&Partition`).
///
/// # Panics
///
/// Panics if the two slices have different lengths.
#[must_use = "the computed value should be used"]
pub fn homogeneity<T1: AsRef<[usize]> + ?Sized, T2: AsRef<[usize]> + ?Sized>(
    partition_true: &T1,
    partition_pred: &T2,
) -> f64 {
    try_homogeneity(partition_true, partition_pred)
        .expect("homogeneity: partitions differ in length")
}

/// Fallible version of [`homogeneity`] that returns an error instead of panicking.
///
/// Returns `Err(LeidenError::InvalidParameter)` if the two slices have
/// different lengths.
pub fn try_homogeneity<T1: AsRef<[usize]> + ?Sized, T2: AsRef<[usize]> + ?Sized>(
    partition_true: &T1,
    partition_pred: &T2,
) -> crate::error::Result<f64> {
    let (n, rows, cols, contingency, a, b) =
        build_contingency_table(partition_true.as_ref(), partition_pred.as_ref())?;

    if n == 0 {
        return Ok(1.0);
    }

    let h_true = entropy(&a, n);
    if h_true.abs() < 1e-12 {
        return Ok(1.0);
    }

    let h_pred = entropy(&b, n);
    let h_joint = joint_entropy(&contingency, rows, cols, n);
    let h_true_given_pred = h_joint - h_pred;

    let val = if h_true_given_pred <= 0.0 {
        1.0
    } else {
        1.0 - h_true_given_pred / h_true
    };

    Ok(val.clamp(0.0, 1.0))
}

/// Completeness between two membership vectors.
///
/// Measures whether all members of a given true class are assigned to the
/// same predicted cluster: c = 1 − H(C_pred|C_true) / H(C_pred).
///
/// Returns 1.0 for perfectly complete clustering. The metric is undefined
/// when H(C_pred) = 0 (single predicted cluster), in which case 1.0 is
/// returned.
///
/// Accepts any type implementing `AsRef<[usize]>` (e.g., `&[usize]`,
/// `&Vec<usize>`, or `&Partition`).
///
/// # Panics
///
/// Panics if the two slices have different lengths.
#[must_use = "the computed value should be used"]
pub fn completeness<T1: AsRef<[usize]> + ?Sized, T2: AsRef<[usize]> + ?Sized>(
    partition_true: &T1,
    partition_pred: &T2,
) -> f64 {
    try_completeness(partition_true, partition_pred)
        .expect("completeness: partitions differ in length")
}

/// Fallible version of [`completeness`] that returns an error instead of panicking.
///
/// Returns `Err(LeidenError::InvalidParameter)` if the two slices have
/// different lengths.
pub fn try_completeness<T1: AsRef<[usize]> + ?Sized, T2: AsRef<[usize]> + ?Sized>(
    partition_true: &T1,
    partition_pred: &T2,
) -> crate::error::Result<f64> {
    let (n, rows, cols, contingency, a, b) =
        build_contingency_table(partition_true.as_ref(), partition_pred.as_ref())?;

    if n == 0 {
        return Ok(1.0);
    }

    let h_pred = entropy(&b, n);
    if h_pred.abs() < 1e-12 {
        return Ok(1.0);
    }

    let h_true = entropy(&a, n);
    let h_joint = joint_entropy(&contingency, rows, cols, n);
    let h_pred_given_true = h_joint - h_true;

    let val = if h_pred_given_true <= 0.0 {
        1.0
    } else {
        1.0 - h_pred_given_true / h_pred
    };

    Ok(val.clamp(0.0, 1.0))
}

/// V-measure between two membership vectors.
///
/// The harmonic mean of homogeneity and completeness:
/// V = 2 · h · c / (h + c)
///
/// Returns 0.0 when both h and c are zero, and the function is symmetric.
///
/// Accepts any type implementing `AsRef<[usize]>` (e.g., `&[usize]`,
/// `&Vec<usize>`, or `&Partition`).
///
/// # Panics
///
/// Panics if the two slices have different lengths.
#[must_use = "the computed value should be used"]
pub fn v_measure<T1: AsRef<[usize]> + ?Sized, T2: AsRef<[usize]> + ?Sized>(
    partition_true: &T1,
    partition_pred: &T2,
) -> f64 {
    try_v_measure(partition_true, partition_pred)
        .expect("v_measure: partitions differ in length")
}

/// Fallible version of [`v_measure`] that returns an error instead of panicking.
///
/// Returns `Err(LeidenError::InvalidParameter)` if the two slices have
/// different lengths.
pub fn try_v_measure<T1: AsRef<[usize]> + ?Sized, T2: AsRef<[usize]> + ?Sized>(
    partition_true: &T1,
    partition_pred: &T2,
) -> crate::error::Result<f64> {
    let h = try_homogeneity(partition_true, partition_pred)?;
    let c = try_completeness(partition_true, partition_pred)?;

    let val = if h + c < 1e-12 {
        0.0
    } else {
        2.0 * h * c / (h + c)
    };

    Ok(val)
}

// ---------------------------------------------------------------------------
// AMI (Adjusted Mutual Information)
// ---------------------------------------------------------------------------

/// Adjusted Mutual Information between two membership vectors.
///
/// AMI = (MI − E\[MI]) / (max(H(C_true), H(C_pred)) − E\[MI])
///
/// Uses the Vinh et al. 2009 analytic formula for the expected mutual
/// information under the hypergeometric model of independence. Returns
/// 1.0 for identical partitions, 0.0 for independent ones, and negative
/// values for worse-than-random partitions.
///
/// Accepts any type implementing `AsRef<[usize]>` (e.g., `&[usize]`,
/// `&Vec<usize>`, or `&Partition`).
///
/// # Panics
///
/// Panics if the two slices have different lengths.
#[must_use = "the computed value should be used"]
pub fn ami<T1: AsRef<[usize]> + ?Sized, T2: AsRef<[usize]> + ?Sized>(
    partition_true: &T1,
    partition_pred: &T2,
) -> f64 {
    try_ami(partition_true, partition_pred)
        .expect("ami: partitions differ in length")
}

/// Fallible version of [`ami`] that returns an error instead of panicking.
///
/// Returns `Err(LeidenError::InvalidParameter)` if the two slices have
/// different lengths.
pub fn try_ami<T1: AsRef<[usize]> + ?Sized, T2: AsRef<[usize]> + ?Sized>(
    partition_true: &T1,
    partition_pred: &T2,
) -> crate::error::Result<f64> {
    let (n, rows, cols, contingency, a, b) =
        build_contingency_table(partition_true.as_ref(), partition_pred.as_ref())?;

    if n == 0 {
        return Ok(1.0);
    }

    let nf = n as f64;

    // Mutual information I(X;Y)
    let mut mi = 0.0;
    for i in 0..rows {
        if a[i] == 0 {
            continue;
        }
        for j in 0..cols {
            let n_ij = contingency[i * cols + j];
            if n_ij == 0 {
                continue;
            }
            mi += (n_ij as f64 / nf)
                * (nf * n_ij as f64 / (a[i] as f64 * b[j] as f64)).ln();
        }
    }

    let h_true = entropy(&a, n);
    let h_pred = entropy(&b, n);

    // When both partitions have zero entropy (single class/cluster),
    // AMI is conventionally 1.0 (identical trivial partitions).
    if h_true.abs() < 1e-12 && h_pred.abs() < 1e-12 {
        return Ok(1.0);
    }

    // Expected MI under the hypergeometric model (Vinh et al. 2009)
    let emi = expected_mutual_info(rows, cols, &a, &b, n);

    let denom = h_true.max(h_pred) - emi;
    if denom <= 1e-12 {
        return Ok(0.0);
    }

    Ok((mi - emi) / denom)
}

// ---------------------------------------------------------------------------
// Performance
// ---------------------------------------------------------------------------

/// Graph-aware performance of a partition.
///
/// Performance measures the fraction of correctly classified node pairs:
///
/// Performance = (TP + TN) / (total_pairs)
///
/// where a pair (i, j) is correctly classified if (i and j are in the same
/// community AND an edge exists) OR (i and j are in different communities
/// AND no edge exists).
///
/// Returns 1.0 for a perfect partition and 0.0 for the worst case. Graphs
/// with zero or one node always return 1.0.
#[must_use = "the computed value should be used"]
pub fn performance(data: &GraphData, partition: &Partition) -> f64 {
    let n = data.node_count();
    if n <= 1 {
        return 1.0;
    }

    let total_pairs = (n * (n - 1) / 2) as f64;
    let directed = data.is_directed();
    let num_comms = partition.num_communities();

    // Count community sizes for same-community pairs
    let mut comm_sizes = vec![0usize; num_comms];
    for i in 0..n {
        let c = partition.community_of(i);
        if c < num_comms {
            comm_sizes[c] += 1;
        }
    }

    // Count same-community pairs: Σ_c C(size_c, 2)
    let same_comm_pairs: usize = comm_sizes
        .iter()
        .map(|&s| {
            if s >= 2 {
                s * (s - 1) / 2
            } else {
                0
            }
        })
        .sum();

    // Count edges (each counted once for undirected) and same-community edges
    let mut same_comm_edges = 0usize;
    let mut total_edges = 0usize;
    for i in 0..n {
        let comm_i = partition.community_of(i);
        for (j, _) in data.neighbors(i) {
            if !directed && j < i {
                continue;
            }
            if comm_i == partition.community_of(j) {
                same_comm_edges += 1;
            }
            total_edges += 1;
        }
    }

    // TP  = same-community-and-connected
    // TN  = diff-community-and-not-connected
    //      = (total_pairs - same_comm_pairs) - (total_edges - same_comm_edges)
    let tp = same_comm_edges as f64;
    let tn = (total_pairs - same_comm_pairs as f64) - (total_edges as f64 - tp);

    (tp + tn.max(0.0)) / total_pairs
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn test_nmi_identical() {
        let p = vec![0, 0, 1, 1, 2, 2];
        assert!((nmi(&p, &p) - 1.0).abs() < 1e-10);
    }

    #[test]
    fn test_nmi_independent() {
        let p1 = vec![0, 0, 0, 1, 1, 1];
        let p2 = vec![0, 1, 0, 1, 0, 1];
        assert!(nmi(&p1, &p2) < 0.5);
    }

    #[test]
    fn test_nmi_single_community() {
        let p1 = vec![0, 0, 0, 0];
        let p2 = vec![0, 0, 0, 0];
        assert!((nmi(&p1, &p2) - 1.0).abs() < 1e-10);
    }

    #[test]
    fn test_nmi_symmetric() {
        let p1 = vec![0, 0, 1, 1, 2, 2, 3, 3];
        let p2 = vec![0, 0, 0, 1, 1, 1, 2, 2];
        assert!((nmi(&p1, &p2) - nmi(&p2, &p1)).abs() < 1e-10);
    }

    #[test]
    fn test_nmi_empty() {
        let p: Vec<usize> = vec![];
        assert!((nmi(&p, &p) - 1.0).abs() < 1e-10);
    }

    #[test]
    fn test_nmi_range() {
        let p1 = vec![0, 0, 1, 1, 2, 2];
        let p2 = vec![0, 1, 2, 0, 1, 2];
        let val = nmi(&p1, &p2);
        assert!((0.0..=1.0).contains(&val), "NMI out of range: {val}");
    }

    #[test]
    fn test_ari_identical() {
        let p = vec![0, 0, 1, 1, 2, 2];
        assert!((ari(&p, &p) - 1.0).abs() < 1e-10);
    }

    #[test]
    fn test_ari_random() {
        let p1 = vec![0, 0, 0, 1, 1, 1];
        let p2 = vec![0, 1, 0, 1, 0, 1];
        assert!(ari(&p1, &p2) < 0.2);
    }

    #[test]
    fn test_ari_perfect_split() {
        let p1 = vec![0, 0, 0, 1, 1, 1];
        let p2 = vec![0, 0, 0, 1, 1, 1];
        assert!((ari(&p1, &p2) - 1.0).abs() < 1e-10);
    }

    #[test]
    fn test_ari_empty() {
        let p: Vec<usize> = vec![];
        assert!((ari(&p, &p) - 1.0).abs() < 1e-10);
    }

    #[test]
    fn test_ari_range() {
        let p1 = vec![0, 0, 1, 1, 2, 2];
        let p2 = vec![0, 1, 2, 0, 1, 2];
        let val = ari(&p1, &p2);
        assert!(
            (-0.5..=1.0).contains(&val),
            "ARI out of expected range: {val}"
        );
    }

    #[test]
    fn test_community_metrics() {
        let edges = [
            (0, 1, 1.0),
            (1, 2, 1.0),
            (0, 2, 1.0), // triangle 1
            (3, 4, 1.0),
            (4, 5, 1.0),
            (3, 5, 1.0), // triangle 2
            (2, 3, 1.0), // bridge
        ];
        let mut builder = crate::graph::GraphDataBuilder::new(6);
        for &(u, v, w) in &edges {
            builder.add_edge(u, v, w).unwrap();
        }
        let data = builder.build().unwrap();
        let partition = Partition::from_membership(vec![0, 0, 0, 1, 1, 1]);

        let cond = conductance(&data, &partition);
        assert_eq!(cond.len(), 2);
        // Community 0: cut = 1 (edge 2-3), vol = 7, conductance = 1/min(7, 5) = 0.2
        assert!(cond[0] > 0.0 && cond[0] < 0.5);
        // Community 1: symmetric
        assert!(cond[1] > 0.0 && cond[1] < 0.5);

        let cov = coverage(&data, &partition);
        // 6 internal edges out of 7 total = 6/7
        assert!(cov > 0.8 && cov < 1.0);

        let density = internal_density(&data, &partition);
        // Each community has 3 nodes, 3 internal edges: 2*3/(3*2) = 1.0
        assert!((density[0] - 1.0).abs() < 1e-10);
        assert!((density[1] - 1.0).abs() < 1e-10);
    }

    #[test]
    fn test_community_sizes_delegates() {
        let partition = Partition::from_membership(vec![0, 0, 1, 1, 1, 2]);
        let sizes = community_sizes(&partition);
        assert_eq!(sizes, vec![2, 3, 1]);
    }

    #[test]
    fn test_conductance_values() {
        // Two triangles connected by one bridge edge
        let edges = [
            (0, 1, 1.0),
            (1, 2, 1.0),
            (0, 2, 1.0),
            (3, 4, 1.0),
            (4, 5, 1.0),
            (3, 5, 1.0),
            (2, 3, 1.0),
        ];
        let mut builder = crate::graph::GraphDataBuilder::new(6);
        for &(u, v, w) in &edges {
            builder.add_edge(u, v, w).unwrap();
        }
        let data = builder.build().unwrap();
        let partition = Partition::from_membership(vec![0, 0, 0, 1, 1, 1]);

        let cond = conductance(&data, &partition);
        // total_weight m = 7, 2m = 14
        // Community 0: nodes 0,1,2; degrees 2+2+3=7; cut = 1 (edge 2-3)
        //   vol = 7, 2m-vol = 7, min(7,7) = 7, conductance = 1/7 ≈ 0.1429
        assert!((cond[0] - 1.0 / 7.0).abs() < 1e-10);
        assert!((cond[1] - 1.0 / 7.0).abs() < 1e-10);
    }

    #[test]
    fn test_coverage_exact() {
        let edges = [
            (0, 1, 1.0),
            (1, 2, 1.0),
            (0, 2, 1.0),
            (3, 4, 1.0),
            (4, 5, 1.0),
            (3, 5, 1.0),
            (2, 3, 1.0),
        ];
        let mut builder = crate::graph::GraphDataBuilder::new(6);
        for &(u, v, w) in &edges {
            builder.add_edge(u, v, w).unwrap();
        }
        let data = builder.build().unwrap();
        let partition = Partition::from_membership(vec![0, 0, 0, 1, 1, 1]);

        let cov = coverage(&data, &partition);
        // 6 internal edges (3 per community) / 7 total = 6/7
        assert!((cov - 6.0 / 7.0).abs() < 1e-10);
    }

    #[test]
    fn test_nmi_different_lengths_panics() {
        let p1 = vec![0, 0, 1];
        let p2 = vec![0, 1];
        let result = std::panic::catch_unwind(|| nmi(&p1, &p2));
        assert!(result.is_err());
    }

    #[test]
    fn test_ari_different_lengths_panics() {
        let p1 = vec![0, 0, 1];
        let p2 = vec![0, 1];
        let result = std::panic::catch_unwind(|| ari(&p1, &p2));
        assert!(result.is_err());
    }

    #[test]
    fn test_nmi_with_partition() {
        use crate::partition::Partition;
        let p1 = Partition::from_membership(vec![0, 0, 1, 1, 2, 2]);
        let p2 = Partition::from_membership(vec![0, 0, 1, 1, 2, 2]);
        let via_partition = nmi(&p1, &p2);
        let via_slice = nmi(p1.as_slice(), p2.as_slice());
        assert!((via_partition - 1.0).abs() < 1e-10);
        assert!((via_partition - via_slice).abs() < 1e-10);
    }

    #[test]
    fn test_ari_with_partition() {
        use crate::partition::Partition;
        let p1 = Partition::from_membership(vec![0, 0, 0, 1, 1, 1]);
        let p2 = Partition::from_membership(vec![0, 0, 1, 1, 2, 2]);
        let via_partition = ari(&p1, &p2);
        let via_slice = ari(p1.as_slice(), p2.as_slice());
        assert!((via_partition - via_slice).abs() < 1e-10);
    }

    #[test]
    fn test_nmi_mixed_types() {
        use crate::partition::Partition;
        let vec_truth = vec![0usize, 0, 1, 1];
        let partition_pred = Partition::from_membership(vec![0, 1, 0, 1]);
        let score = nmi(&vec_truth, &partition_pred);
        assert!((0.0..=1.0).contains(&score));
    }

    #[test]
    fn test_try_nmi_length_mismatch() {
        let p1 = vec![0, 0, 1];
        let p2 = vec![0, 1];
        assert!(try_nmi(&p1, &p2).is_err());
    }

    #[test]
    fn test_try_ari_length_mismatch() {
        let p1 = vec![0, 0, 1];
        let p2 = vec![0, 1];
        assert!(try_ari(&p1, &p2).is_err());
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

    // -- vi tests --

    #[test]
    fn test_vi_identical() {
        let p = vec![0, 0, 1, 1, 2, 2];
        assert!((vi(&p, &p) - 0.0).abs() < 1e-10);
    }

    #[test]
    fn test_vi_different() {
        let p1 = vec![0, 0, 0, 1, 1, 1];
        let p2 = vec![0, 1, 0, 1, 0, 1];
        assert!(vi(&p1, &p2) > 0.0);
    }

    #[test]
    fn test_vi_single_community() {
        let p1 = vec![0, 0, 0, 0];
        let p2 = vec![0, 0, 0, 0];
        assert!((vi(&p1, &p2) - 0.0).abs() < 1e-10);
    }

    #[test]
    fn test_vi_symmetric() {
        let p1 = vec![0, 0, 1, 1, 2, 2, 3, 3];
        let p2 = vec![0, 0, 0, 1, 1, 1, 2, 2];
        assert!((vi(&p1, &p2) - vi(&p2, &p1)).abs() < 1e-10);
    }

    #[test]
    fn test_vi_empty() {
        let p: Vec<usize> = vec![];
        assert!((vi(&p, &p) - 0.0).abs() < 1e-10);
    }

    #[test]
    fn test_vi_range() {
        let p1 = vec![0, 0, 1, 1, 2, 2];
        let p2 = vec![0, 1, 2, 0, 1, 2];
        let val = vi(&p1, &p2);
        assert!(val >= 0.0, "VI out of range: {val}");
    }

    #[test]
    fn test_vi_different_lengths_panics() {
        let p1 = vec![0, 0, 1];
        let p2 = vec![0, 1];
        let result = std::panic::catch_unwind(|| vi(&p1, &p2));
        assert!(result.is_err());
    }

    #[test]
    fn test_try_vi_length_mismatch() {
        let p1 = vec![0, 0, 1];
        let p2 = vec![0, 1];
        assert!(try_vi(&p1, &p2).is_err());
    }

    #[test]
    fn test_try_vi_matches_vi() {
        let p1 = vec![0, 0, 1, 1, 2, 2];
        let p2 = vec![0, 1, 0, 1, 0, 1];
        assert!((try_vi(&p1, &p2).unwrap() - vi(&p1, &p2)).abs() < 1e-10);
    }

    // -- fmi tests --

    #[test]
    fn test_fmi_identical() {
        let p = vec![0, 0, 1, 1, 2, 2];
        assert!((fmi(&p, &p) - 1.0).abs() < 1e-10);
    }

    #[test]
    fn test_fmi_independent() {
        let p1 = vec![0, 0, 0, 1, 1, 1];
        let p2 = vec![0, 1, 0, 1, 0, 1];
        let val = fmi(&p1, &p2);
        // For this case: sum_comb_a=C(3,2)+C(3,2)=6, sum_comb_b=6, sum_comb_n=C(2,2)+C(2,2)=2
        // FMI = 2/sqrt(36) = 1/3
        assert!((val - 1.0 / 3.0).abs() < 1e-10);
    }

    #[test]
    fn test_fmi_perfect_split() {
        let p1 = vec![0, 0, 0, 1, 1, 1];
        let p2 = vec![0, 0, 0, 1, 1, 1];
        assert!((fmi(&p1, &p2) - 1.0).abs() < 1e-10);
    }

    #[test]
    fn test_fmi_empty() {
        let p: Vec<usize> = vec![];
        assert!((fmi(&p, &p) - 1.0).abs() < 1e-10);
    }

    #[test]
    fn test_fmi_range() {
        let p1 = vec![0, 0, 1, 1, 2, 2];
        let p2 = vec![0, 1, 2, 0, 1, 2];
        let val = fmi(&p1, &p2);
        assert!((0.0..=1.0).contains(&val), "FMI out of range: {val}");
    }

    #[test]
    fn test_fmi_symmetric() {
        let p1 = vec![0, 0, 0, 1, 1, 1];
        let p2 = vec![0, 0, 1, 1, 2, 2];
        assert!((fmi(&p1, &p2) - fmi(&p2, &p1)).abs() < 1e-10);
    }

    #[test]
    fn test_fmi_different_lengths_panics() {
        let p1 = vec![0, 0, 1];
        let p2 = vec![0, 1];
        let result = std::panic::catch_unwind(|| fmi(&p1, &p2));
        assert!(result.is_err());
    }

    #[test]
    fn test_try_fmi_length_mismatch() {
        let p1 = vec![0, 0, 1];
        let p2 = vec![0, 1];
        assert!(try_fmi(&p1, &p2).is_err());
    }

    #[test]
    fn test_try_fmi_matches_fmi() {
        let p1 = vec![0, 0, 0, 1, 1, 1];
        let p2 = vec![0, 0, 1, 1, 2, 2];
        assert!((try_fmi(&p1, &p2).unwrap() - fmi(&p1, &p2)).abs() < 1e-10);
    }

    // -- purity tests --

    #[test]
    fn test_purity_identical() {
        let p = vec![0, 0, 1, 1, 2, 2];
        assert!((purity(&p, &p) - 1.0).abs() < 1e-10);
    }

    #[test]
    fn test_purity_known() {
        let p1 = vec![0, 0, 0, 1, 1, 1];
        let p2 = vec![0, 0, 1, 1, 2, 2];
        // contingency row 0: [2,1,0] → max=2
        // contingency row 1: [0,1,2] → max=2
        // purity = (2+2)/6 = 2/3
        let val = purity(&p1, &p2);
        assert!((val - 2.0 / 3.0).abs() < 1e-10);
    }

    #[test]
    fn test_purity_empty() {
        let p: Vec<usize> = vec![];
        assert!((purity(&p, &p) - 1.0).abs() < 1e-10);
    }

    #[test]
    fn test_purity_different_lengths_panics() {
        let p1 = vec![0, 0, 1];
        let p2 = vec![0, 1];
        let result = std::panic::catch_unwind(|| purity(&p1, &p2));
        assert!(result.is_err());
    }

    #[test]
    fn test_try_purity_length_mismatch() {
        let p1 = vec![0, 0, 1];
        let p2 = vec![0, 1];
        assert!(try_purity(&p1, &p2).is_err());
    }

    #[test]
    fn test_try_purity_matches_purity() {
        let p1 = vec![0, 0, 0, 1, 1, 1];
        let p2 = vec![0, 0, 1, 1, 2, 2];
        assert!((try_purity(&p1, &p2).unwrap() - purity(&p1, &p2)).abs() < 1e-10);
    }

    // -- homogeneity tests --

    #[test]
    fn test_homogeneity_identical() {
        let p = vec![0, 0, 1, 1, 2, 2];
        assert!((homogeneity(&p, &p) - 1.0).abs() < 1e-10);
    }

    #[test]
    fn test_homogeneity_known() {
        // true = [0,0,0,1,1,1], pred = [0,1,0,1,0,1]
        // contingency: row0=[2,1], row1=[1,2], a=[3,3], b=[3,3]
        // H(true) = -0.5·ln(0.5)·2 = ln(2) ≈ 0.6931
        // H(pred) = ln(2) ≈ 0.6931
        // H(joint) = -2·(2/6)·ln(2/6) - 2·(1/6)·ln(1/6) = 1.3297
        // H(true|pred) = H(joint) − H(pred) = 1.3297 − 0.6931 = 0.6366
        // homogeneity = 1 − 0.6366/0.6931 ≈ 0.0817
        let p1 = vec![0, 0, 0, 1, 1, 1];
        let p2 = vec![0, 1, 0, 1, 0, 1];
        let val = homogeneity(&p1, &p2);
        assert!((val - 0.0817).abs() < 1e-3, "homogeneity too far: {val}");
    }

    #[test]
    fn test_homogeneity_single_class() {
        let p1 = vec![0, 0, 0, 0];
        let p2 = vec![0, 0, 1, 1];
        assert!((homogeneity(&p1, &p2) - 1.0).abs() < 1e-10);
    }

    #[test]
    fn test_homogeneity_different_lengths_panics() {
        let p1 = vec![0, 0, 1];
        let p2 = vec![0, 1];
        let result = std::panic::catch_unwind(|| homogeneity(&p1, &p2));
        assert!(result.is_err());
    }

    #[test]
    fn test_try_homogeneity_length_mismatch() {
        let p1 = vec![0, 0, 1];
        let p2 = vec![0, 1];
        assert!(try_homogeneity(&p1, &p2).is_err());
    }

    #[test]
    fn test_try_homogeneity_matches() {
        let p1 = vec![0, 0, 0, 1, 1, 1];
        let p2 = vec![0, 1, 0, 1, 0, 1];
        assert!(
            (try_homogeneity(&p1, &p2).unwrap() - homogeneity(&p1, &p2)).abs() < 1e-10
        );
    }

    // -- completeness tests --

    #[test]
    fn test_completeness_identical() {
        let p = vec![0, 0, 1, 1, 2, 2];
        assert!((completeness(&p, &p) - 1.0).abs() < 1e-10);
    }

    #[test]
    fn test_completeness_symmetric_with_homogeneity() {
        let p1 = vec![0, 0, 0, 1, 1, 1];
        let p2 = vec![0, 1, 0, 1, 0, 1];
        // Contingency symmetric → completeness = homogeneity ≈ 0.0817
        let val = completeness(&p1, &p2);
        assert!((val - 0.0817).abs() < 1e-3, "completeness too far: {val}");
    }

    #[test]
    fn test_completeness_single_cluster() {
        let p1 = vec![0, 0, 1, 1];
        let p2 = vec![0, 0, 0, 0];
        assert!((completeness(&p1, &p2) - 1.0).abs() < 1e-10);
    }

    #[test]
    fn test_completeness_different_lengths_panics() {
        let p1 = vec![0, 0, 1];
        let p2 = vec![0, 1];
        let result = std::panic::catch_unwind(|| completeness(&p1, &p2));
        assert!(result.is_err());
    }

    #[test]
    fn test_try_completeness_length_mismatch() {
        let p1 = vec![0, 0, 1];
        let p2 = vec![0, 1];
        assert!(try_completeness(&p1, &p2).is_err());
    }

    #[test]
    fn test_try_completeness_matches() {
        let p1 = vec![0, 0, 0, 1, 1, 1];
        let p2 = vec![0, 1, 0, 1, 0, 1];
        assert!(
            (try_completeness(&p1, &p2).unwrap() - completeness(&p1, &p2)).abs() < 1e-10
        );
    }

    // -- v_measure tests --

    #[test]
    fn test_v_measure_identical() {
        let p = vec![0, 0, 1, 1, 2, 2];
        assert!((v_measure(&p, &p) - 1.0).abs() < 1e-10);
    }

    #[test]
    fn test_v_measure_known() {
        let p1 = vec![0, 0, 0, 1, 1, 1];
        let p2 = vec![0, 1, 0, 1, 0, 1];
        let val = v_measure(&p1, &p2);
        // Both homogeneity and completeness ≈ 0.0817
        // v = 2·0.0817·0.0817/(0.0817+0.0817) ≈ 0.0817
        assert!((val - 0.0817).abs() < 1e-3, "v_measure too far: {val}");
    }

    #[test]
    fn test_v_measure_symmetric() {
        let p1 = vec![0, 0, 0, 1, 1, 1];
        let p2 = vec![0, 1, 0, 1, 0, 1];
        assert!(
            (v_measure(&p1, &p2) - v_measure(&p2, &p1)).abs() < 1e-10
        );
    }

    #[test]
    fn test_v_measure_different_lengths_panics() {
        let p1 = vec![0, 0, 1];
        let p2 = vec![0, 1];
        let result = std::panic::catch_unwind(|| v_measure(&p1, &p2));
        assert!(result.is_err());
    }

    #[test]
    fn test_try_v_measure_length_mismatch() {
        let p1 = vec![0, 0, 1];
        let p2 = vec![0, 1];
        assert!(try_v_measure(&p1, &p2).is_err());
    }

    #[test]
    fn test_v_measure_edge_case_all_same_community() {
        let p1 = vec![0, 0, 0, 0];
        let p2 = vec![0, 0, 0, 0];
        assert!((v_measure(&p1, &p2) - 1.0).abs() < 1e-10);
    }

    #[test]
    fn test_try_v_measure_matches() {
        let p1 = vec![0, 0, 0, 1, 1, 1];
        let p2 = vec![0, 1, 0, 1, 0, 1];
        assert!(
            (try_v_measure(&p1, &p2).unwrap() - v_measure(&p1, &p2)).abs() < 1e-10
        );
    }

    // -- AMI tests --

    #[test]
    fn test_ami_identical() {
        let p = vec![0, 0, 1, 1, 2, 2];
        assert!((ami(&p, &p) - 1.0).abs() < 1e-10, "AMI not 1.0 for identical");
    }

    #[test]
    fn test_ami_independent_approx_zero() {
        let p1 = vec![0, 0, 0, 1, 1, 1];
        let p2 = vec![0, 1, 0, 1, 0, 1];
        let val = ami(&p1, &p2);
        // Expected to be near 0 (could be slightly negative)
        assert!(val.abs() < 0.3, "AMI far from zero: {val}");
    }

    #[test]
    fn test_ami_symmetric() {
        let p1 = vec![0, 0, 1, 1, 2, 2, 3, 3];
        let p2 = vec![0, 0, 0, 1, 1, 1, 2, 2];
        assert!((ami(&p1, &p2) - ami(&p2, &p1)).abs() < 1e-10);
    }

    #[test]
    fn test_ami_single_community() {
        let p1 = vec![0, 0, 0, 0];
        let p2 = vec![0, 0, 0, 0];
        assert!((ami(&p1, &p2) - 1.0).abs() < 1e-10);
    }

    #[test]
    fn test_ami_different_lengths_panics() {
        let p1 = vec![0, 0, 1];
        let p2 = vec![0, 1];
        let result = std::panic::catch_unwind(|| ami(&p1, &p2));
        assert!(result.is_err());
    }

    #[test]
    fn test_try_ami_length_mismatch() {
        let p1 = vec![0, 0, 1];
        let p2 = vec![0, 1];
        assert!(try_ami(&p1, &p2).is_err());
    }

    #[test]
    fn test_try_ami_matches() {
        let p1 = vec![0, 0, 0, 1, 1, 1];
        let p2 = vec![0, 1, 0, 1, 0, 1];
        assert!((try_ami(&p1, &p2).unwrap() - ami(&p1, &p2)).abs() < 1e-10);
    }

    // -- performance tests --

    #[test]
    fn test_performance_perfect() {
        // Two disconnected triangles
        let edges = [(0, 1, 1.0), (1, 2, 1.0), (0, 2, 1.0), (3, 4, 1.0), (4, 5, 1.0), (3, 5, 1.0)];
        let mut builder = crate::graph::GraphDataBuilder::new(6);
        for &(u, v, w) in &edges {
            builder.add_edge(u, v, w).unwrap();
        }
        let data = builder.build().unwrap();
        let partition = Partition::from_membership(vec![0, 0, 0, 1, 1, 1]);

        let perf = performance(&data, &partition);
        // TP=6 (all edges internal), TN=9-6=3 (diff comm pairs), total_pairs=15
        // TN = 9 - 0 = 9 (diff pairs have no edges)
        // Wait: same_comm pairs = C(3,2)+C(3,2) = 6 diff comm pairs = 15-6=9 all diff edges = 0
        // performance = (6 + 9) / 15 = 1.0
        assert!((perf - 1.0).abs() < 1e-10, "perfect performance: {perf}");
    }

    #[test]
    fn test_performance_worst() {
        // Chain: 0-1-2-3-4-5, partition splits each edge
        let edges = [(0, 1, 1.0), (1, 2, 1.0), (2, 3, 1.0), (3, 4, 1.0), (4, 5, 1.0)];
        let mut builder = crate::graph::GraphDataBuilder::new(6);
        for &(u, v, w) in &edges {
            builder.add_edge(u, v, w).unwrap();
        }
        let data = builder.build().unwrap();
        // Each node in its own community: worst case for performance
        let partition = Partition::new(6);

        let perf = performance(&data, &partition);
        // total_pairs = 15, same_comm_pairs = 0, total_edges = 5, same_comm_edges = 0
        // tp = 0, tn = (15 - 0) - (5 - 0) = 10
        // perf = 10/15 = 2/3
        assert!((perf - 10.0 / 15.0).abs() < 1e-10);
    }

    #[test]
    fn test_performance_empty() {
        let mut builder = crate::graph::GraphDataBuilder::new(0);
        let data = builder.build().unwrap();
        let partition = Partition::from_membership(vec![]);
        assert!((performance(&data, &partition) - 1.0).abs() < 1e-10);
    }

    #[test]
    fn test_performance_one_node() {
        let mut builder = crate::graph::GraphDataBuilder::new(1);
        let data = builder.build().unwrap();
        let partition = Partition::from_membership(vec![0]);
        assert!((performance(&data, &partition) - 1.0).abs() < 1e-10);
    }

    #[test]
    fn test_performance_mixed() {
        // 4 nodes, edges: 0-1, 2-3
        // Partition: [0,0,1,1] (perfect match)
        let edges = [(0, 1, 1.0), (2, 3, 1.0)];
        let mut builder = crate::graph::GraphDataBuilder::new(4);
        for &(u, v, w) in &edges {
            builder.add_edge(u, v, w).unwrap();
        }
        let data = builder.build().unwrap();
        let partition = Partition::from_membership(vec![0, 0, 1, 1]);
        let perf = performance(&data, &partition);
        // total_pairs=6 same_comm_pairs=2 total_edges=2 same_comm_edges=2
        // tp=2 tn=(6-2)-(2-2)=4 perf=(2+4)/6=1.0
        assert!((perf - 1.0).abs() < 1e-10);
    }

    #[test]
    fn test_performance_with_partition() {
        let edges = [(0, 1, 1.0), (1, 2, 1.0), (0, 2, 1.0), (3, 4, 1.0), (4, 5, 1.0), (3, 5, 1.0)];
        let mut builder = crate::graph::GraphDataBuilder::new(6);
        for &(u, v, w) in &edges {
            builder.add_edge(u, v, w).unwrap();
        }
        let data = builder.build().unwrap();
        let partition = Partition::from_membership(vec![0, 0, 0, 1, 1, 1]);
        let perf = performance(&data, &partition);
        assert!((perf - 1.0).abs() < 1e-10);
    }
}

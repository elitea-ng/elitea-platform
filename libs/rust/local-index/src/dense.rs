//! The dense side of `search_workspace`: the chunk vectors of the
//! workspace's embedder, held in memory as one int8 matrix and scanned
//! exhaustively (a quarter-million 384-wide vectors is ~100 MB and tens of
//! milliseconds; no approximate index to keep in step with refreshes).
//!
//! The query is embedded by the same embedder the chunks were (the
//! background embedder's model, loaded on demand); its vector stays in
//! floats, the stored side is int8 with a scale per vector.

use crate::search::Dense;
use crate::sqlite_store::{SqliteGraphStore, StoreError, VectorSet};
use elitea_local_embed::{EmbedError, Embedder, vector};
use std::cmp::Ordering;
use std::collections::BinaryHeap;
use std::sync::Arc;

/// What embeds a search's query: the background embedder (it shares its
/// one model with it).
pub trait QueryEmbedder: Send + Sync {
    /// The vector set `choice` makes now (`None`: unknown, or no longer
    /// allowed: no dense side).
    fn vector_set(&self, choice: &str) -> Option<VectorSet>;

    /// The embedder of `choice` for `workspace_id`, loaded if needed.
    ///
    /// # Errors
    ///
    /// It does not load.
    fn embedder(&self, choice: &str, workspace_id: &str) -> Result<Arc<dyn Embedder>, EmbedError>;
}

/// Every vector of one set, as one matrix.
#[derive(Debug)]
pub struct VectorMatrix {
    stamp: String,
    dim: usize,
    ids: Vec<i64>,
    scales: Vec<f32>,
    values: Vec<i8>,
}

/// A scored chunk, ordered for a min-heap of the best.
#[derive(PartialEq)]
struct Scored(f32, i64);

impl Eq for Scored {}

impl PartialOrd for Scored {
    fn partial_cmp(&self, other: &Self) -> Option<Ordering> {
        Some(self.cmp(other))
    }
}

impl Ord for Scored {
    fn cmp(&self, other: &Self) -> Ordering {
        // Reversed: the heap's top is the worst kept.
        other
            .0
            .total_cmp(&self.0)
            .then_with(|| self.1.cmp(&other.1))
    }
}

impl VectorMatrix {
    /// Read the vectors of `set` (blobs of another width are skipped).
    ///
    /// # Errors
    ///
    /// The store failed.
    pub fn load(store: &SqliteGraphStore, set: &VectorSet) -> Result<Self, StoreError> {
        let stamp = set.stamp();
        let rows = store.chunk_vectors(&stamp)?;
        let dim = set.dim;
        let mut matrix = Self {
            stamp,
            dim,
            ids: Vec::with_capacity(rows.len()),
            scales: Vec::with_capacity(rows.len()),
            values: Vec::with_capacity(rows.len() * dim),
        };
        for (id, blob) in rows {
            let Some((scale, values)) = vector::dequantize_parts(&blob, dim) else {
                continue;
            };
            matrix.ids.push(id);
            matrix.scales.push(scale);
            matrix
                .values
                .extend(values.iter().map(|byte| i8::from_le_bytes([*byte])));
        }
        Ok(matrix)
    }

    #[must_use]
    pub fn stamp(&self) -> &str {
        &self.stamp
    }

    #[must_use]
    pub fn len(&self) -> usize {
        self.ids.len()
    }

    #[must_use]
    pub fn is_empty(&self) -> bool {
        self.ids.is_empty()
    }

    /// Bytes held.
    #[must_use]
    pub fn bytes(&self) -> usize {
        self.values.len() + self.ids.len() * 12
    }

    /// The `limit` chunks nearest `query` (normalised here): `(id,
    /// cosine)`, best first. Scans on up to four threads.
    #[must_use]
    pub fn nearest(&self, query: &[f32], limit: usize) -> Vec<(i64, f32)> {
        if query.len() != self.dim || self.is_empty() || limit == 0 {
            return Vec::new();
        }
        let mut q = query.to_vec();
        vector::normalize(&mut q);
        let parts = std::thread::available_parallelism()
            .map_or(1, std::num::NonZero::get)
            .clamp(1, 4)
            .min(self.len().div_ceil(16_384).max(1));
        let per = self.len().div_ceil(parts);
        let best: Vec<BinaryHeap<Scored>> = std::thread::scope(|scope| {
            let handles: Vec<_> = (0..parts)
                .map(|part| {
                    let q = &q;
                    scope.spawn(move || {
                        self.scan(q, part * per, ((part + 1) * per).min(self.len()), limit)
                    })
                })
                .collect();
            handles
                .into_iter()
                .map(|handle| handle.join().unwrap_or_default())
                .collect()
        });
        let mut all: Vec<(i64, f32)> = best
            .into_iter()
            .flatten()
            .map(|Scored(score, id)| (id, score))
            .collect();
        all.sort_by(|a, b| b.1.total_cmp(&a.1).then_with(|| a.0.cmp(&b.0)));
        all.truncate(limit);
        all
    }

    fn scan(&self, q: &[f32], from: usize, to: usize, limit: usize) -> BinaryHeap<Scored> {
        let mut heap = BinaryHeap::with_capacity(limit + 1);
        for row in from..to {
            let values = &self.values[row * self.dim..(row + 1) * self.dim];
            let dot: f32 = values.iter().zip(q).map(|(v, x)| f32::from(*v) * x).sum();
            let score = dot * self.scales[row];
            if heap.len() < limit {
                heap.push(Scored(score, self.ids[row]));
            } else if heap.peek().is_some_and(|worst| score > worst.0) {
                heap.pop();
                heap.push(Scored(score, self.ids[row]));
            }
        }
        heap
    }
}

/// [`Dense`] over a loaded matrix and the embedder of its set.
pub struct DenseSearch {
    pub(crate) matrix: Arc<VectorMatrix>,
    pub(crate) embedder: Arc<dyn QueryEmbedder>,
    pub(crate) choice: String,
    pub(crate) workspace_id: String,
}

impl Dense for DenseSearch {
    fn nearest(
        &self,
        query: &str,
        limit: usize,
        execution_id: Option<&str>,
    ) -> Result<Vec<(i64, f32)>, String> {
        let embedder = self
            .embedder
            .embedder(&self.choice, &self.workspace_id)
            .map_err(|error| error.to_string())?;
        let vector = embedder
            .embed_query_for(query, execution_id)
            .map_err(|error| error.to_string())?;
        Ok(self.matrix.nearest(&vector, limit))
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_scan_keeps_the_best_in_order_across_parts() {
        let dim = 4;
        let n = 40_000;
        let mut matrix = VectorMatrix {
            stamp: "t:m:4".into(),
            dim,
            ids: Vec::new(),
            scales: Vec::new(),
            values: Vec::new(),
        };
        for i in 0..n {
            #[allow(clippy::cast_precision_loss)]
            let angle = i as f32 / n as f32 * std::f32::consts::PI;
            let v = [angle.cos(), angle.sin(), 0.0, 0.0];
            let blob = vector::quantize(&v);
            let (scale, values) = vector::dequantize_parts(&blob, dim).unwrap();
            matrix.ids.push(i64::from(i));
            matrix.scales.push(scale);
            matrix
                .values
                .extend(values.iter().map(|b| i8::from_le_bytes([*b])));
        }
        let near = matrix.nearest(&[1.0, 0.0, 0.0, 0.0], 5);
        assert_eq!(near.len(), 5);
        assert!(near.iter().all(|(id, _)| *id < 200), "{near:?}");
        assert!(near.windows(2).all(|w| w[0].1 >= w[1].1));
        let opposite = matrix.nearest(&[-1.0, 0.0, 0.0, 0.0], 1);
        assert!(opposite[0].0 > i64::from(n) - 200, "{opposite:?}");
        assert!(matrix.nearest(&[1.0, 0.0], 3).is_empty(), "width differs");
    }
}

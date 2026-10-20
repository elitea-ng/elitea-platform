//! Vectors as the index keeps them.
//!
//! Every vector is L2-normalised, truncated to the stored width
//! (Matryoshka models are renormalised after the cut), and stored as int8
//! with one scale per vector: `[scale: f32 LE][dims × i8]`. A 512-wide
//! vector is 516 bytes. The cosine of two normalised vectors is their dot
//! product, computed on the integers and rescaled.

/// Scale `vector` to unit length (a zero vector stays zero).
pub fn normalize(vector: &mut [f32]) {
    let norm = vector.iter().map(|v| v * v).sum::<f32>().sqrt();
    if norm > 0.0 {
        for value in vector.iter_mut() {
            *value /= norm;
        }
    }
}

/// Cut `vector` to `dimension` and normalise it again.
#[must_use]
pub fn truncate(mut vector: Vec<f32>, dimension: usize) -> Vec<f32> {
    vector.truncate(dimension);
    normalize(&mut vector);
    vector
}

/// The int8 blob of a (normalised) vector.
#[must_use]
pub fn quantize(vector: &[f32]) -> Vec<u8> {
    let peak = vector.iter().fold(0.0_f32, |peak, v| peak.max(v.abs()));
    let scale = if peak > 0.0 { peak / 127.0 } else { 1.0 };
    let mut blob = Vec::with_capacity(4 + vector.len());
    blob.extend_from_slice(&scale.to_le_bytes());
    for value in vector {
        #[allow(clippy::cast_possible_truncation)]
        let q = (value / scale).round().clamp(-127.0, 127.0) as i8;
        blob.push(q.to_le_bytes()[0]);
    }
    blob
}

/// A stored blob read back: its scale and its integers. `None` when it is
/// not a blob of `dimension` values.
#[must_use]
pub fn dequantize_parts(blob: &[u8], dimension: usize) -> Option<(f32, &[u8])> {
    if blob.len() != 4 + dimension {
        return None;
    }
    let (head, values) = blob.split_at(4);
    let scale = f32::from_le_bytes(head.try_into().ok()?);
    Some((scale, values))
}

/// A query prepared for scoring against int8 blobs: the vector itself
/// (the query stays in floats; only the stored side is quantised).
#[derive(Debug, Clone)]
pub struct Query {
    values: Vec<f32>,
}

impl Query {
    #[must_use]
    pub fn new(vector: &[f32]) -> Self {
        let mut values = vector.to_vec();
        normalize(&mut values);
        Self { values }
    }

    #[must_use]
    pub fn dimension(&self) -> usize {
        self.values.len()
    }

    /// The cosine with a stored blob; `None` when the widths differ.
    #[must_use]
    pub fn cosine(&self, blob: &[u8]) -> Option<f32> {
        let (scale, values) = dequantize_parts(blob, self.values.len())?;
        let dot: f32 = values
            .iter()
            .zip(&self.values)
            .map(|(q, v)| f32::from(i8::from_le_bytes([*q])) * v)
            .sum();
        Some(dot * scale)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn quantised_cosines_stay_close_to_the_float_ones() {
        let mut a: Vec<f32> = (0..512)
            .map(|i| (f32::from(u16::try_from(i * 37 % 101).unwrap()) - 50.0) / 50.0)
            .collect();
        let mut b: Vec<f32> = (0..512)
            .map(|i| (f32::from(u16::try_from(i * 53 % 97).unwrap()) - 48.0) / 48.0)
            .collect();
        normalize(&mut a);
        normalize(&mut b);
        let exact: f32 = a.iter().zip(&b).map(|(x, y)| x * y).sum();
        let blob = quantize(&b);
        assert_eq!(blob.len(), 516);
        let approx = Query::new(&a).cosine(&blob).unwrap();
        assert!((exact - approx).abs() < 0.01, "{exact} vs {approx}");
        let self_cos = Query::new(&b).cosine(&blob).unwrap();
        assert!((self_cos - 1.0).abs() < 0.01, "{self_cos}");
        assert!(
            Query::new(&a[..10]).cosine(&blob).is_none(),
            "widths differ"
        );
    }

    #[test]
    fn truncation_renormalises() {
        let v = truncate(vec![3.0, 4.0, 12.0], 2);
        assert!((v[0] - 0.6).abs() < 1e-6 && (v[1] - 0.8).abs() < 1e-6);
        let zero = quantize(&[0.0, 0.0]);
        assert_eq!(Query::new(&[1.0, 0.0]).cosine(&zero), Some(0.0));
    }
}

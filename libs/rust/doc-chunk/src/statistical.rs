//! The statistical chunker (`statistical_chunker.py`), without the
//! embeddings call.
//!
//! The SDK cuts a document into 300-token splits, embeds them, scores each
//! split against the mean of the (up to 5) splits before it, and starts a new
//! chunk where the score falls below a threshold. With `dynamic_threshold`
//! (the default) the threshold is found by a binary search for the value that
//! gives chunks whose median size is 100 to 300 tokens. This module owns the
//! arithmetic: [`statistical_chunks`] turns the splits and their vectors into
//! chunks; the caller owns the embeddings request.
//!
//! Differences from the SDK, deliberate:
//!
//! * the SDK embedded 64 splits at a time and planned each batch alone,
//!   carrying the last chunk of a batch into the next batch's start: that
//!   chunk was emitted twice (once ending its batch, once opening the next)
//!   and the next batch's first chunk repeated its text. Here the batch size
//!   only bounds an embeddings request: the whole document is planned once
//!   (the same chunks as the SDK for a document of 64 splits or fewer), and
//!   no text is emitted twice;
//! * `min_split_tokens` and `max_split_tokens` also bound the chunk grouping
//!   (the SDK used them for the threshold search and its defaults, 100 and
//!   300, for the grouping);
//! * a `threshold_adjustment` that is not positive takes the default (the
//!   SDK's search would not end).

use crate::config::ModelStepParams;
use crate::route::positive;
use crate::tokens::{self, Encoding};
use crate::{Chunk, ChunkError};
use std::collections::BTreeSet;

/// The most iterations of the threshold search (the search's interval
/// shrinks by `threshold_adjustment` each step; this is the backstop).
const MAX_SEARCH_STEPS: usize = 10_000;

/// The statistical chunker's settings, SDK defaults applied.
#[derive(Debug, Clone, Copy, PartialEq)]
pub struct StatisticalSettings {
    /// Tokens per split (`max_doc_size`).
    pub split_tokens: usize,
    /// Splits per embeddings request (`batch_size`).
    pub batch_size: usize,
    /// Splits the running context averages (`window_size`).
    pub window_size: usize,
    pub dynamic_threshold: bool,
    pub min_split_tokens: usize,
    pub max_split_tokens: usize,
    pub split_tokens_tolerance: usize,
    pub threshold_adjustment: f64,
    /// The fixed threshold of `dynamic_threshold: false`.
    pub score_threshold: f64,
}

impl Default for StatisticalSettings {
    fn default() -> Self {
        Self {
            split_tokens: 300,
            batch_size: 64,
            window_size: 5,
            dynamic_threshold: true,
            min_split_tokens: 100,
            max_split_tokens: 300,
            split_tokens_tolerance: 10,
            threshold_adjustment: 0.01,
            score_threshold: 0.5,
        }
    }
}

impl StatisticalSettings {
    /// The settings a chunking config names, defaults for the rest.
    #[must_use]
    pub fn from_params(params: &ModelStepParams) -> Self {
        let default = Self::default();
        Self {
            split_tokens: positive(params.max_doc_size).unwrap_or(default.split_tokens),
            batch_size: positive(params.batch_size).unwrap_or(default.batch_size),
            window_size: positive(params.window_size).unwrap_or(default.window_size),
            dynamic_threshold: params
                .dynamic_threshold
                .unwrap_or(default.dynamic_threshold),
            min_split_tokens: positive(params.min_split_tokens).unwrap_or(default.min_split_tokens),
            max_split_tokens: positive(params.max_split_tokens).unwrap_or(default.max_split_tokens),
            split_tokens_tolerance: params
                .split_tokens_tolerance
                .and_then(|n| usize::try_from(n).ok())
                .unwrap_or(default.split_tokens_tolerance),
            threshold_adjustment: params
                .threshold_adjustment
                .filter(|a| *a > 0.0)
                .unwrap_or(default.threshold_adjustment),
            score_threshold: params.score_threshold.unwrap_or(default.score_threshold),
        }
    }
}

/// `text` as the statistical chunker's splits: cl100k windows of
/// `split_tokens`, no overlap, each an exact slice of `text` (a split never
/// cuts a character; the splits concatenate back to the text), with each
/// split's token count from the same encoding pass. Empty text gives none.
///
/// # Errors
/// [`ChunkError::Tokenizer`] if the encoding is unavailable.
pub(crate) fn split_text(
    text: &str,
    settings: &StatisticalSettings,
) -> Result<(Vec<String>, Vec<usize>), ChunkError> {
    tokens::split_exact_counted(text, Encoding::Cl100k, settings.split_tokens)
}

/// The chunks of `splits` given their token `counts` (from the split step)
/// and embedding `vectors` (one per split, in order, one dimension). No text
/// is tokenized here.
///
/// # Errors
/// [`ChunkError::InvalidModelStep`] when the counts or vectors do not match
/// the splits.
pub fn statistical_chunks(
    splits: &[String],
    counts: &[usize],
    vectors: &[Vec<f32>],
    settings: &StatisticalSettings,
) -> Result<Vec<Chunk>, ChunkError> {
    if counts.len() != splits.len() {
        return Err(ChunkError::InvalidModelStep(format!(
            "{} token counts for {} splits",
            counts.len(),
            splits.len()
        )));
    }
    if vectors.len() != splits.len() {
        return Err(ChunkError::InvalidModelStep(format!(
            "{} vectors for {} splits",
            vectors.len(),
            splits.len()
        )));
    }
    if let Some(first) = vectors.first()
        && (first.is_empty() || vectors.iter().any(|v| v.len() != first.len()))
    {
        return Err(ChunkError::InvalidModelStep(
            "the vectors differ in dimension".to_owned(),
        ));
    }
    let similarities = similarity_scores(vectors, settings.window_size);
    let threshold = if settings.dynamic_threshold && !similarities.is_empty() {
        optimal_threshold(counts, &similarities, settings)
    } else {
        settings.score_threshold
    };
    let breaks = split_indices(&similarities, threshold);
    let groups = group(splits, counts, &breaks, settings);
    Ok(groups
        .into_iter()
        .enumerate()
        .map(|(i, (text, tokens))| {
            let mut chunk = Chunk::new(text, i + 1, "document", "statistical");
            chunk.metadata.insert(
                "chunk_token_count".to_owned(),
                serde_json::Value::from(tokens),
            );
            chunk
        })
        .collect())
}

/// Each split after the first against the mean of the `window` splits before
/// it: the cosine, with the SDK's `1e-10` in the denominator.
fn similarity_scores(vectors: &[Vec<f32>], window: usize) -> Vec<f64> {
    let norm = |v: &[f64]| v.iter().map(|x| x * x).sum::<f64>().sqrt();
    let widened: Vec<Vec<f64>> = vectors
        .iter()
        .map(|v| v.iter().map(|x| f64::from(*x)).collect())
        .collect();
    let mut out = Vec::with_capacity(widened.len().saturating_sub(1));
    for idx in 1..widened.len() {
        let start = idx.saturating_sub(window);
        let context_splits = &widened[start..idx];
        let dimension = widened[idx].len();
        let mut context = vec![0.0_f64; dimension];
        for v in context_splits {
            for (slot, x) in context.iter_mut().zip(v) {
                *slot += x;
            }
        }
        #[allow(clippy::cast_precision_loss)]
        let divisor = context_splits.len() as f64;
        for slot in &mut context {
            *slot /= divisor;
        }
        let dot: f64 = context.iter().zip(&widened[idx]).map(|(a, b)| a * b).sum();
        out.push(dot / (norm(&context) * norm(&widened[idx]) + 1e-10));
    }
    out
}

fn median(values: &[f64]) -> f64 {
    if values.is_empty() {
        return f64::NAN;
    }
    let mut sorted = values.to_vec();
    sorted.sort_by(f64::total_cmp);
    let mid = sorted.len() / 2;
    if sorted.len() % 2 == 1 {
        sorted[mid]
    } else {
        f64::midpoint(sorted[mid - 1], sorted[mid])
    }
}

/// The break positions: before split `i + 1` when its score is under the
/// threshold.
fn split_indices(similarities: &[f64], threshold: f64) -> BTreeSet<usize> {
    similarities
        .iter()
        .enumerate()
        .filter(|(_, score)| **score < threshold)
        .map(|(i, _)| i + 1)
        .collect()
}

/// The binary search of `_find_optimal_threshold`: the last threshold tried
/// when the median chunk size lands in `[min - tolerance, max + tolerance]`,
/// or when the interval is spent.
fn optimal_threshold(counts: &[usize], similarities: &[f64], s: &StatisticalSettings) -> f64 {
    #[allow(clippy::cast_precision_loss)]
    let count = similarities.len() as f64;
    let median_score = median(similarities);
    let mean = similarities.iter().sum::<f64>() / count;
    let std_dev = (similarities.iter().map(|x| (x - mean).powi(2)).sum::<f64>() / count).sqrt();
    let mut low = (median_score - std_dev).max(0.0);
    let mut high = (median_score + std_dev).min(1.0);
    #[allow(clippy::cast_precision_loss)]
    let (min_ok, max_ok) = (
        s.min_split_tokens as f64 - s.split_tokens_tolerance as f64,
        s.max_split_tokens as f64 + s.split_tokens_tolerance as f64,
    );
    let mut threshold = 0.0;
    for _ in 0..MAX_SEARCH_STEPS {
        if low > high {
            break;
        }
        threshold = f64::midpoint(low, high);
        let breaks = split_indices(similarities, threshold);
        let mut sizes: Vec<f64> = Vec::new();
        let mut start = 0;
        for end in breaks.iter().copied().chain(std::iter::once(counts.len())) {
            #[allow(clippy::cast_precision_loss)]
            sizes.push(counts[start..end].iter().sum::<usize>() as f64);
            start = end;
        }
        let median_tokens = median(&sizes);
        if (min_ok..=max_ok).contains(&median_tokens) {
            break;
        }
        #[allow(clippy::cast_precision_loss)]
        if median_tokens < s.min_split_tokens as f64 {
            high = threshold - s.threshold_adjustment;
        } else {
            low = threshold + s.threshold_adjustment;
        }
    }
    threshold
}

/// `_split_documents`: accumulate splits until a break (when the chunk so far
/// holds at least `min` and under `max` tokens) or until the next split would
/// pass `max` (when it holds at least `min`). The chunk's text is its splits
/// concatenated: the splits are slices of the original text, so no separator
/// is inserted (the SDK joined decoded splits by a space).
fn group(
    splits: &[String],
    counts: &[usize],
    breaks: &BTreeSet<usize>,
    s: &StatisticalSettings,
) -> Vec<(String, usize)> {
    let mut chunks: Vec<(String, usize)> = Vec::new();
    let mut current: Vec<&str> = Vec::new();
    let mut tokens = 0_usize;
    for (idx, split) in splits.iter().enumerate() {
        let size = counts[idx];
        if breaks.contains(&(idx + 1))
            && (s.min_split_tokens..s.max_split_tokens).contains(&(tokens + size))
        {
            current.push(split);
            tokens += size;
            chunks.push((current.concat(), tokens));
            current.clear();
            tokens = 0;
            continue;
        }
        if tokens + size > s.max_split_tokens && tokens >= s.min_split_tokens {
            chunks.push((current.concat(), tokens));
            current.clear();
            tokens = 0;
        }
        current.push(split);
        tokens += size;
    }
    if !current.is_empty() {
        chunks.push((current.concat(), tokens));
    }
    chunks
}

#[cfg(test)]
mod tests {
    use super::*;

    /// `statistical_chunks` with the counts a split step would give.
    fn chunks_of(
        splits: &[String],
        vectors: &[Vec<f32>],
        settings: &StatisticalSettings,
    ) -> Result<Vec<Chunk>, ChunkError> {
        let counts: Vec<usize> = splits
            .iter()
            .map(|s| tokens::encode(s, Encoding::Cl100k).unwrap().len())
            .collect();
        statistical_chunks(splits, &counts, vectors, settings)
    }

    /// Splits of `n` tokens each ("w w w …" is one token per word).
    fn splits(sizes: &[usize]) -> Vec<String> {
        sizes.iter().map(|n| vec!["w"; *n].join(" ")).collect()
    }

    fn unit(axis: usize, dimension: usize) -> Vec<f32> {
        (0..dimension)
            .map(|i| if i == axis { 1.0 } else { 0.0 })
            .collect()
    }

    #[test]
    fn a_topic_change_starts_a_chunk_where_the_context_stops_matching() {
        // Six 60-token splits: three on one axis, three on another.
        let text = splits(&[60; 6]);
        let vectors: Vec<Vec<f32>> = (0..6).map(|i| unit(usize::from(i >= 3), 2)).collect();
        let settings = StatisticalSettings {
            dynamic_threshold: false,
            min_split_tokens: 100,
            ..StatisticalSettings::default()
        };
        let chunks = chunks_of(&text, &vectors, &settings).unwrap();
        // Break before split 3 (similarity 0): 180 tokens is in [100, 300).
        let counts: Vec<_> = chunks
            .iter()
            .map(|c| c.metadata["chunk_token_count"].clone())
            .collect();
        assert_eq!(counts, [180, 180]);
        assert_eq!(chunks[0].chunk_id, 1);
        assert_eq!(chunks[1].chunk_id, 2);
        assert!(chunks.iter().all(|c| c.chunk_type == "document"));
        assert_eq!(chunks[0].text, text[..3].concat());
        assert_eq!(chunks[1].text, text[3..].concat());
    }

    #[test]
    fn splits_are_original_slices_with_no_replacement_character_and_chunks_add_no_space() {
        let text = "日本語のテキストです😀😀😀 unbelievably long words 😀 end. ".repeat(30);
        let settings = StatisticalSettings {
            split_tokens: 7,
            ..StatisticalSettings::default()
        };
        let (splits, _) = split_text(&text, &settings).unwrap();
        assert!(splits.len() > 3);
        assert_eq!(splits.concat(), text);
        assert!(splits.iter().all(|s| !s.contains('\u{FFFD}')));
        let vectors: Vec<Vec<f32>> = (0..splits.len()).map(|i| unit(i % 2, 2)).collect();
        let chunks = chunks_of(&splits, &vectors, &settings).unwrap();
        let rejoined: String = chunks.iter().map(|c| c.text.as_str()).collect();
        assert_eq!(rejoined, text, "no space inserted between splits");
    }

    #[test]
    fn the_split_step_hands_over_each_splits_token_count() {
        let text = "alpha beta gamma delta epsilon zeta eta theta. ".repeat(40);
        let settings = StatisticalSettings {
            split_tokens: 11,
            ..StatisticalSettings::default()
        };
        let (splits, counts) = split_text(&text, &settings).unwrap();
        assert_eq!(splits.len(), counts.len());
        let whole = tokens::encode(&text, Encoding::Cl100k).unwrap().len();
        assert_eq!(counts.iter().sum::<usize>(), whole, "one pass, no loss");
        assert!(counts.iter().all(|n| (1..=11).contains(n)));
        // The chunker reads these counts: a mismatched list is refused, and
        // the token metadata is the counts' sum.
        let vectors: Vec<Vec<f32>> = (0..splits.len()).map(|_| unit(0, 2)).collect();
        assert!(matches!(
            statistical_chunks(&splits, &counts[1..], &vectors, &settings),
            Err(ChunkError::InvalidModelStep(_))
        ));
        let chunks = statistical_chunks(&splits, &counts, &vectors, &settings).unwrap();
        let total: u64 = chunks
            .iter()
            .map(|c| c.metadata["chunk_token_count"].as_u64().unwrap())
            .sum();
        assert_eq!(total, u64::try_from(whole).unwrap());
    }

    #[test]
    fn every_split_is_emitted_exactly_once() {
        // 70 splits, past the SDK's 64-split batch: no carried-over copy.
        let text = splits(&[40; 70]);
        let vectors: Vec<Vec<f32>> = (0..70).map(|i| unit(i % 3, 3)).collect();
        let chunks = chunks_of(&text, &vectors, &StatisticalSettings::default()).unwrap();
        let joined = chunks.iter().map(|c| c.text.as_str()).collect::<String>();
        assert_eq!(joined, text.concat());
        let total: usize = chunks
            .iter()
            .map(|c| usize::try_from(c.metadata["chunk_token_count"].as_u64().unwrap()).unwrap())
            .sum();
        assert_eq!(total, 70 * 40);
    }

    #[test]
    fn a_chunk_never_passes_the_maximum_once_it_holds_the_minimum() {
        let text = splits(&[120; 8]);
        let vectors: Vec<Vec<f32>> = (0..8).map(|_| unit(0, 2)).collect();
        let settings = StatisticalSettings {
            dynamic_threshold: false,
            ..StatisticalSettings::default()
        };
        let chunks = chunks_of(&text, &vectors, &settings).unwrap();
        for chunk in &chunks[..chunks.len() - 1] {
            assert!(chunk.metadata["chunk_token_count"].as_u64().unwrap() <= 300);
        }
    }

    #[test]
    fn the_dynamic_threshold_search_ends_and_covers_the_document() {
        let text = splits(&[50; 20]);
        let vectors: Vec<Vec<f32>> = (0..20_u8)
            .map(|i| vec![1.0, f32::from(i % 4) * 0.7, f32::from(i % 7) * 0.3])
            .collect();
        let chunks = chunks_of(&text, &vectors, &StatisticalSettings::default()).unwrap();
        assert_ne!(chunks, [] as [Chunk; 0]);
        let joined = chunks.iter().map(|c| c.text.as_str()).collect::<String>();
        assert_eq!(joined, text.concat());
    }

    #[test]
    fn one_split_is_one_chunk_and_no_split_is_none() {
        let text = splits(&[10]);
        let one = chunks_of(&text, &[unit(0, 2)], &StatisticalSettings::default()).unwrap();
        assert_eq!(one.len(), 1);
        assert_eq!(
            chunks_of(&[], &[], &StatisticalSettings::default()).unwrap(),
            [] as [Chunk; 0]
        );
    }

    #[test]
    fn vectors_that_do_not_match_the_splits_are_refused() {
        let text = splits(&[10, 10]);
        let s = StatisticalSettings::default();
        assert!(matches!(
            chunks_of(&text, &[unit(0, 2)], &s),
            Err(ChunkError::InvalidModelStep(_))
        ));
        assert!(matches!(
            chunks_of(&text, &[unit(0, 2), unit(0, 3)], &s),
            Err(ChunkError::InvalidModelStep(_))
        ));
    }

    #[test]
    fn medians_and_settings_follow_the_sdk() {
        assert!((median(&[3.0, 1.0, 2.0]) - 2.0).abs() < 1e-12);
        assert!((median(&[4.0, 1.0, 2.0, 3.0]) - 2.5).abs() < 1e-12);
        let defaults = StatisticalSettings::from_params(&ModelStepParams::default());
        assert_eq!(defaults, StatisticalSettings::default());
        let set = StatisticalSettings::from_params(&ModelStepParams {
            max_doc_size: Some(-1),
            batch_size: Some(8),
            threshold_adjustment: Some(0.0),
            ..ModelStepParams::default()
        });
        assert_eq!(set.split_tokens, 300, "zero and negative mean default");
        assert_eq!(set.batch_size, 8);
        assert!((set.threshold_adjustment - 0.01).abs() < 1e-12);
    }
}

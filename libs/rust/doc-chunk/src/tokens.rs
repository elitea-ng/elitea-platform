//! Token windows, as `LangChain`'s `TokenTextSplitter` cuts them
//! (`split_text_on_tokens`): windows of `size` tokens that each start
//! `size - overlap` tokens after the last, decoded back to text.

use crate::ChunkError;
use std::ops::Range;
use std::sync::LazyLock;
use tiktoken_rs::CoreBPE;

static CL100K: LazyLock<Result<CoreBPE, String>> =
    LazyLock::new(|| tiktoken_rs::cl100k_base().map_err(|e| e.to_string()));
/// tiktoken's `gpt2` encoding is the `r50k_base` ranks.
static GPT2: LazyLock<Result<CoreBPE, String>> =
    LazyLock::new(|| tiktoken_rs::r50k_base().map_err(|e| e.to_string()));

/// Which encoding a split counts in.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub(crate) enum Encoding {
    Cl100k,
    /// Only where the SDK uses it: the unknown-language code split.
    Gpt2,
}

fn bpe(encoding: Encoding) -> Result<&'static CoreBPE, ChunkError> {
    match encoding {
        Encoding::Cl100k => &CL100K,
        Encoding::Gpt2 => &GPT2,
    }
    .as_ref()
    .map_err(|error| ChunkError::Tokenizer(error.clone()))
}

/// Tokens in `text`, special-token spellings counted as text
/// (`disallowed_special=()` in the SDK's `tiktoken_length`).
#[cfg(test)]
pub(crate) fn count(text: &str) -> Result<usize, ChunkError> {
    Ok(bpe(Encoding::Cl100k)?.encode_ordinary(text).len())
}

/// A window overlap that leaves the window a step: the SDK's code chunker
/// falls back to an eighth of the size, and `LangChain` raises where the
/// others would loop forever.
pub(crate) fn sane_overlap(size: usize, overlap: usize) -> usize {
    if overlap >= size { size / 8 } else { overlap }
}

/// The token ranges of the windows over `length` tokens.
pub(crate) fn windows(length: usize, size: usize, overlap: usize) -> Vec<Range<usize>> {
    let size = size.max(1);
    let step = size - sane_overlap(size, overlap);
    let mut out = Vec::new();
    let mut start = 0;
    while start < length || out.is_empty() {
        let end = (start + size).min(length);
        out.push(start..end);
        if end == length {
            break;
        }
        start += step;
    }
    out
}

/// `text`'s token ids, special-token spellings counted as text.
pub(crate) fn encode(text: &str, encoding: Encoding) -> Result<Vec<u32>, ChunkError> {
    Ok(bpe(encoding)?.encode_ordinary(text))
}

/// `text` cut into token windows. Empty text gives no window. A window that
/// cuts through a multi-byte character decodes with a replacement character,
/// as Python's `decode(errors="replace")` does.
pub(crate) fn split(
    text: &str,
    encoding: Encoding,
    size: usize,
    overlap: usize,
) -> Result<Vec<String>, ChunkError> {
    if text.is_empty() {
        return Ok(Vec::new());
    }
    split_ids(&encode(text, encoding)?, encoding, size, overlap)
}

/// The windows over ids already encoded (so a caller that counted them does
/// not encode the text again).
pub(crate) fn split_ids(
    ids: &[u32],
    encoding: Encoding,
    size: usize,
    overlap: usize,
) -> Result<Vec<String>, ChunkError> {
    if ids.is_empty() {
        return Ok(Vec::new());
    }
    let bpe = bpe(encoding)?;
    let mut out = Vec::new();
    for window in windows(ids.len(), size, overlap) {
        let bytes = bpe
            .decode_bytes(&ids[window])
            .map_err(|error| ChunkError::Tokenizer(error.to_string()))?;
        out.push(String::from_utf8_lossy(&bytes).into_owned());
    }
    Ok(out)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn windows_step_by_size_minus_overlap_and_end_at_the_length() {
        assert_eq!(windows(10, 4, 1), [0..4, 3..7, 6..10]);
        assert_eq!(windows(4, 4, 1), [Range { start: 0, end: 4 }]);
        assert_eq!(windows(5, 4, 0), [0..4, 4..5]);
        assert_eq!(windows(0, 4, 1), [Range { start: 0, end: 0 }]);
        // An overlap that is not smaller than the window falls back to an
        // eighth of it.
        assert_eq!(windows(20, 8, 8), [0..8, 7..15, 14..20]);
    }

    #[test]
    fn the_gpt2_ranks_differ_from_cl100k() {
        let text = "    return    event";
        let a = bpe(Encoding::Cl100k).map(|b| b.encode_ordinary(text).len());
        let b = bpe(Encoding::Gpt2).map(|b| b.encode_ordinary(text).len());
        assert!(a.is_ok() && b.is_ok());
        assert_ne!(a, b, "whitespace runs tokenize differently");
    }
}

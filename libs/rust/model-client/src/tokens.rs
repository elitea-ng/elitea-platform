//! Token counting (ADR-0026 decision 8).
//!
//! [`count_tokens`] is `engine/token_counter.py`'s `count_tokens`: the
//! `o200k_base` encoding (`tiktoken.encoding_for_model("gpt-4o")`), 0 for
//! an empty text. Two differences, both deliberate:
//!
//! * there is no `chars/4` fallback. Python fell back when `tiktoken` was
//!   missing or when `encode` raised, so a budget silently changed its
//!   unit. Here the BPE file is compiled into the binary, and the encoder
//!   cannot be missing;
//! * special-token text (`<|endoftext|>` in a source file) is counted as
//!   ordinary text. Python's `encode` raised on it, and the `except`
//!   switched that one count to `chars/4`.
//!
//! [`embedding_split`] is the length split `LangChain`'s `OpenAIEmbeddings`
//! did before it sent a text: `cl100k_base` (what `encoding_for_model`
//! gives for every embedding model name, known or not), windows of 8191
//! tokens.

use elitea_engine_core::errors::{EngineError, ErrorType};
use std::sync::LazyLock;
use tiktoken_rs::{CoreBPE, Rank};

/// `LangChain`'s `embedding_ctx_length`.
pub const EMBEDDING_CTX_LENGTH: usize = 8191;

/// `TokenCounter.count_document`'s formatting overhead.
pub const DOCUMENT_OVERHEAD_TOKENS: usize = 20;

static O200K: LazyLock<Result<CoreBPE, String>> =
    LazyLock::new(|| tiktoken_rs::o200k_base().map_err(|e| e.to_string()));

static CL100K: LazyLock<Result<CoreBPE, String>> =
    LazyLock::new(|| tiktoken_rs::cl100k_base().map_err(|e| e.to_string()));

fn encoder(
    cell: &'static LazyLock<Result<CoreBPE, String>>,
    name: &str,
) -> Result<&'static CoreBPE, EngineError> {
    // The BPE text is compiled in, so this fails only for a broken build.
    cell.as_ref().map_err(|error| {
        EngineError::new(
            ErrorType::Runtime,
            format!("the embedded {name} tokenizer cannot load: {error}"),
        )
    })
}

/// The number of `o200k_base` tokens in `text`.
///
/// # Errors
///
/// Only if the embedded tokenizer cannot load (a broken build).
pub fn count_tokens(text: &str) -> Result<usize, EngineError> {
    if text.is_empty() {
        return Ok(0);
    }
    Ok(encoder(&O200K, "o200k_base")?.encode_ordinary(text).len())
}

/// `TokenCounter.count_document`: the content, plus — when any metadata
/// value is non-empty — those values joined by spaces and a fixed
/// [`DOCUMENT_OVERHEAD_TOKENS`] for the heading they are formatted into.
///
/// `metadata` are the values of `symbol_name`, `file_path`, `symbol_type`,
/// `expansion_reason`, `expansion_source` and `expansion_via`, in that
/// order; the caller picks them, because the document type is its own.
///
/// # Errors
///
/// See [`count_tokens`].
pub fn count_document_tokens<'a>(
    content: &str,
    metadata: impl IntoIterator<Item = &'a str>,
) -> Result<usize, EngineError> {
    let mut tokens = count_tokens(content)?;
    let present: Vec<&str> = metadata.into_iter().filter(|v| !v.is_empty()).collect();
    if !present.is_empty() {
        tokens += count_tokens(&present.join(" "))? + DOCUMENT_OVERHEAD_TOKENS;
    }
    Ok(tokens)
}

/// One window of a text that is embedded on its own.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Window {
    pub text: String,
    /// Its `cl100k_base` token count: the weight of its vector in the
    /// text's average.
    pub tokens: usize,
}

/// Split `text` into windows of at most `ctx` `cl100k_base` tokens.
///
/// A text within the limit is one window holding the text unchanged. An
/// empty text gives no window (`LangChain` then embedded `""` on its own).
/// `LangChain` sent the token ids; this sends the windows' TEXT, because a
/// non-OpenAI embedding model behind the gateway cannot read cl100k ids.
/// A window boundary that falls inside a UTF-8 sequence carries the
/// sequence's leading bytes into the next window, so the windows joined
/// are the text, byte for byte.
///
/// # Errors
///
/// See [`count_tokens`].
pub fn embedding_split(text: &str, ctx: usize) -> Result<Vec<Window>, EngineError> {
    let bpe = encoder(&CL100K, "cl100k_base")?;
    let ids: Vec<Rank> = bpe.encode_ordinary(text);
    if ids.is_empty() {
        return Ok(Vec::new());
    }
    let ctx = ctx.max(1);
    if ids.len() <= ctx {
        return Ok(vec![Window {
            text: text.to_owned(),
            tokens: ids.len(),
        }]);
    }
    let mut windows = Vec::with_capacity(ids.len().div_ceil(ctx));
    let mut carry: Vec<u8> = Vec::new();
    for chunk in ids.chunks(ctx) {
        let mut bytes = std::mem::take(&mut carry);
        bytes.extend(bpe.decode_bytes(chunk).map_err(|error| {
            EngineError::new(
                ErrorType::Runtime,
                format!("the cl100k_base tokenizer cannot decode its own tokens: {error}"),
            )
        })?);
        let valid = match std::str::from_utf8(&bytes) {
            Ok(_) => bytes.len(),
            Err(error) => error.valid_up_to(),
        };
        carry = bytes.split_off(valid);
        // `valid` is a UTF-8 boundary, so this cannot fail; the lossy form
        // only avoids a second error path.
        windows.push(Window {
            text: String::from_utf8_lossy(&bytes).into_owned(),
            tokens: chunk.len(),
        });
    }
    if !carry.is_empty()
        && let Some(last) = windows.last_mut()
    {
        last.text.push_str(&String::from_utf8_lossy(&carry));
    }
    Ok(windows)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn counts_match_tiktoken_o200k() {
        // python -c "import tiktoken; e=tiktoken.get_encoding('o200k_base'); print(len(e.encode('Hello world')))"
        assert_eq!(count_tokens("").ok(), Some(0));
        assert_eq!(count_tokens("Hello world").ok(), Some(2));
        assert_eq!(
            count_tokens("fn main() { println!(\"hi\"); }").ok(),
            Some(9)
        );
    }

    #[test]
    fn special_token_text_is_ordinary_text() {
        let counted = count_tokens("<|endoftext|>");
        assert!(counted.is_ok_and(|n| n > 1));
    }

    #[test]
    fn document_overhead_applies_only_with_metadata() {
        let bare = count_document_tokens("Hello world", ["", ""]).ok();
        assert_eq!(bare, Some(2));
        let with = count_document_tokens("Hello world", ["main", "src/main.rs"]).ok();
        let meta = count_tokens("main src/main.rs").ok();
        assert_eq!(with, meta.map(|m| 2 + m + DOCUMENT_OVERHEAD_TOKENS));
    }

    #[test]
    fn a_short_text_is_one_window_unchanged() {
        let windows = embedding_split("def f():\n    return 1\n", EMBEDDING_CTX_LENGTH);
        let Ok(windows) = windows else {
            panic!("split failed");
        };
        assert_eq!(windows.len(), 1);
        assert_eq!(windows[0].text, "def f():\n    return 1\n");
        assert!(embedding_split("", 10).is_ok_and(|w| w.is_empty()));
    }

    #[test]
    fn a_long_text_splits_and_rejoins_byte_for_byte() {
        let text = "Grüße, 世界! 🦀 ".repeat(200);
        let Ok(windows) = embedding_split(&text, 7) else {
            panic!("split failed");
        };
        assert!(windows.len() > 1);
        assert!(windows.iter().all(|w| w.tokens <= 7));
        let joined: String = windows.iter().map(|w| w.text.as_str()).collect();
        assert_eq!(joined, text);
        assert!(!joined.contains('\u{FFFD}'));
    }
}

//! Document text to index chunks (ADR-0030 decision 4).
//!
//! The chunkers of the Python SDK (`elitea_sdk/tools/chunkers`, pinned at
//! b5113a1) with the SDK's defaults, so the `index_data` `chunking_config`
//! form keeps its meaning:
//!
//! | chunker    | behaviour                                                         |
//! | ---------- | ----------------------------------------------------------------- |
//! | `markdown` | header split on H1-H4, merge under 100 characters, 512 tokens / 10 |
//! | `text`     | cl100k token windows, 512 tokens, overlap 10                      |
//! | `json`     | `LangChain`'s `RecursiveJsonSplitter`, max chunk size 512           |
//! | `code`     | tree-sitter per method, then a 1024 / 128 character split        |
//! | `universal`| the extension router                                              |
//!
//! Chunk boundaries are tested against goldens of this crate's own, not
//! byte-compared with the SDK.
//!
//! The statistical and proposal chunkers call models; they are not here.
//! A config that asks for one (or for `use_llm`) gets
//! [`ChunkError::RequiresModel`], so the caller can report the document as
//! skipped with a reason (they arrive with ADR-0030's I4).
//!
//! The entry point is [`chunk`]; an indexer that converted an HTML page to
//! markdown uses [`chunk_with`] and says so ([`ChunkOptions`]).

mod code;
mod config;
mod json;
mod markdown;
mod route;
mod tokens;

pub use config::{ChunkParams, ChunkingConfig, ConfigError};
pub use route::{
    ChunkOptions, Chunker, chunk, chunk_with, chunker_for, chunker_for_with, extension_of,
};

use serde_json::{Map, Value};

/// One chunk of a document.
#[derive(Debug, Clone, PartialEq)]
pub struct Chunk {
    pub text: String,
    /// 1-based, in document order.
    pub chunk_id: usize,
    /// `document` for the text chunkers, `code` for the code chunker.
    pub chunk_type: &'static str,
    /// What the SDK put in the chunk document's metadata: `chunk_id`,
    /// `chunk_type`, `method_name`, and `headers` (markdown, text) or
    /// `language` (code). The caller adds the document's own fields.
    pub metadata: Map<String, Value>,
}

impl Chunk {
    fn new(text: String, chunk_id: usize, chunk_type: &'static str, method: &str) -> Self {
        let mut metadata = Map::new();
        metadata.insert("chunk_id".to_owned(), Value::from(chunk_id));
        metadata.insert("chunk_type".to_owned(), Value::from(chunk_type));
        metadata.insert("method_name".to_owned(), Value::from(method));
        Self {
            text,
            chunk_id,
            chunk_type,
            metadata,
        }
    }

    fn with(mut self, key: &str, value: &str) -> Self {
        self.metadata.insert(key.to_owned(), Value::from(value));
        self
    }
}

/// Why a document was not chunked.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum ChunkError {
    /// The config asks for a chunker (or a step) that calls a model, which
    /// this crate does not: report the document as skipped with `reason`.
    RequiresModel {
        /// `statistical`, `proposal` or `use_llm`.
        chunker: &'static str,
        reason: String,
    },
    /// `chunking_tool` names no chunker.
    UnknownChunker(String),
    /// The token encoding could not be built.
    Tokenizer(String),
}

impl std::fmt::Display for ChunkError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            Self::RequiresModel { chunker, reason } => {
                write!(f, "the {chunker} chunker needs a model: {reason}")
            }
            Self::UnknownChunker(name) => write!(f, "unknown chunker `{name}`"),
            Self::Tokenizer(error) => write!(f, "the token encoding is unavailable: {error}"),
        }
    }
}

impl std::error::Error for ChunkError {}

//! A retrieved document of a page's context (the `LangChain` `Document`s
//! of the Python page path, reduced to the metadata something reads).

use crate::errors::EngineError;
use crate::llm::tokens::{count_document_tokens, count_tokens};

/// One document. An empty string stands for a metadata key the Python
/// document did not carry: every reader used `.get(key, "")`, a
/// truthiness test, or `or 0`.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct ContextDoc {
    /// `page_content`.
    pub content: String,
    /// `metadata["source"]`: the file the context groups by.
    pub source: String,
    /// `metadata["rel_path"]` (absent on the doc-scan documents).
    pub rel_path: String,
    /// `metadata["file_path"]` (only the explicit target documents).
    pub file_path: String,
    /// `metadata["symbol_name"]` (absent on the document fetches, which
    /// set `symbol` instead).
    pub symbol_name: String,
    pub symbol_type: String,
    pub start_line: i64,
    pub end_line: i64,
    /// `metadata["is_documentation"]`.
    pub is_documentation: bool,
    /// `metadata["expansion_via"]` (only the legacy expansion set it).
    pub expansion_via: String,
    /// `metadata["is_initially_retrieved"]` (cluster expansion).
    pub is_initial: bool,
    pub node_id: String,
    pub language: String,
}

impl ContextDoc {
    /// `TokenCounter.count_document(doc)`: the content plus the metadata
    /// heading (`symbol_name`, `file_path`, `symbol_type`,
    /// `expansion_reason`, `expansion_source`, `expansion_via`).
    ///
    /// # Errors
    ///
    /// Only for a broken build (the tokenizer is embedded).
    pub fn tokens(&self) -> Result<usize, EngineError> {
        count_document_tokens(
            &self.content,
            [
                self.symbol_name.as_str(),
                self.file_path.as_str(),
                self.symbol_type.as_str(),
                "",
                "",
                self.expansion_via.as_str(),
            ],
        )
    }

    /// The content's tokens alone (`TokenCounter.count`).
    ///
    /// # Errors
    ///
    /// See [`ContextDoc::tokens`].
    pub fn content_tokens(&self) -> Result<usize, EngineError> {
        count_tokens(&self.content)
    }
}

/// `TokenCounter.count_documents`.
///
/// # Errors
///
/// See [`ContextDoc::tokens`].
pub fn count_documents(docs: &[ContextDoc]) -> Result<usize, EngineError> {
    docs.iter().map(ContextDoc::tokens).sum()
}

//! The extension router (`universal_chunker`) and the entry point.
//!
//! * markdown extensions (and HTML, which `doc-extract` hands over as
//!   markdown) -> `markdown`
//! * `.json`, `.jsonl`, `.jsonc` -> `json`
//! * the SDK's code extensions -> `code`
//! * everything else -> `text`
//!
//! The router uses each chunker's own SDK defaults (the ADR-0030 table), not
//! the heavier ones `universal_chunker` hard-codes (1024 tokens for markdown,
//! a 1000-character recursive split for text).

use crate::code::{self, Code};
use crate::config::ChunkParams;
use crate::markdown::{self, Markdown};
use crate::{Chunk, ChunkError, ChunkingConfig, json};

/// A chunker.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Chunker {
    Markdown,
    Text,
    Json,
    Code,
}

const MARKDOWN: &[&str] = &[
    ".md",
    ".markdown",
    ".mdown",
    ".mkd",
    ".mdx",
    ".html",
    ".htm",
    ".xhtml",
];
const JSON: &[&str] = &[".json", ".jsonl", ".jsonc"];
/// `universal_chunker.CODE_EXTENSIONS`, plus Swift.
const CODE: &[&str] = &[
    ".py", ".js", ".jsx", ".mjs", ".cjs", ".ts", ".tsx", ".java", ".kt", ".rs", ".go", ".cpp",
    ".c", ".cs", ".hs", ".rb", ".scala", ".lua", ".sh", ".bash", ".zsh", ".sql", ".r", ".swift",
    ".php", ".pl", ".pm", ".h", ".hpp", ".m", ".bat", ".pas", ".asm", ".dart", ".groovy",
];

/// The chunker `universal` picks for a lower-case, dotted extension.
#[must_use]
pub fn chunker_for(extension: &str) -> Chunker {
    if MARKDOWN.contains(&extension) {
        Chunker::Markdown
    } else if JSON.contains(&extension) {
        Chunker::Json
    } else if CODE.contains(&extension) {
        Chunker::Code
    } else {
        Chunker::Text
    }
}

/// The dotted lower-case extension of `source`: a file name or path
/// (`docs/Guide.MD`), a bare extension (`md`, `.md`) or a media type
/// (`text/markdown`). A source with no extension gives `""`.
#[must_use]
pub fn extension_of(source: &str) -> String {
    let source = source.trim().to_ascii_lowercase();
    let source = source.split(';').next().unwrap_or("").trim();
    if source.contains('/') && !source.rsplit('/').next().unwrap_or("").contains('.') {
        return mime_extension(source).to_owned();
    }
    let name = source.rsplit(['/', '\\']).next().unwrap_or("");
    match name.rsplit_once('.') {
        Some((_, ext)) if !ext.is_empty() => format!(".{ext}"),
        // No dot: a bare extension such as `md`.
        None if (1..=5).contains(&name.len())
            && name.chars().all(|c| c.is_ascii_alphanumeric()) =>
        {
            format!(".{name}")
        }
        _ => String::new(),
    }
}

fn mime_extension(mime: &str) -> &'static str {
    match mime {
        "text/markdown" | "text/x-markdown" => ".md",
        "text/html" | "application/xhtml+xml" => ".html",
        "application/json" | "text/json" => ".json",
        "text/x-python" | "application/x-python-code" => ".py",
        "text/javascript" | "application/javascript" => ".js",
        "text/typescript" | "application/typescript" => ".ts",
        "text/x-java" | "text/x-java-source" => ".java",
        "text/x-rust" => ".rs",
        "text/x-go" => ".go",
        "text/x-kotlin" => ".kt",
        "text/x-csharp" => ".cs",
        "text/x-c" => ".c",
        "text/x-c++" | "text/x-cpp" => ".cpp",
        _ => ".txt",
    }
}

fn positive(value: Option<i64>) -> Option<usize> {
    value
        .and_then(|n| usize::try_from(n).ok())
        .filter(|n| *n > 0)
}

fn chunker_named(name: &str, extension: &str) -> Result<Chunker, ChunkError> {
    match name.trim().to_ascii_lowercase().as_str() {
        "markdown" => Ok(Chunker::Markdown),
        "text" => Ok(Chunker::Text),
        "json" => Ok(Chunker::Json),
        "code" | "code_parser" => Ok(Chunker::Code),
        "universal" => Ok(chunker_for(extension)),
        "statistical" => Err(ChunkError::RequiresModel {
            chunker: "statistical",
            reason: "it embeds sentences to find topic boundaries".to_owned(),
        }),
        "proposal" => Err(ChunkError::RequiresModel {
            chunker: "proposal",
            reason: "it asks a model to restate the text as propositions".to_owned(),
        }),
        other => Err(ChunkError::UnknownChunker(other.to_owned())),
    }
}

fn markdown_chunks(text: &str, params: &ChunkParams) -> Result<Vec<Chunk>, ChunkError> {
    let default_headers = markdown::default_headers();
    let headers = params
        .headers_to_split_on
        .as_deref()
        .filter(|h| !h.is_empty())
        .unwrap_or(&default_headers);
    markdown::markdown(
        text,
        &Markdown {
            headers,
            strip_headers: params.strip_header.unwrap_or(false),
            return_each_line: params.return_each_line.unwrap_or(false),
            min_chunk_chars: params.min_chunk_chars.unwrap_or(markdown::MIN_CHUNK_CHARS),
            max_tokens: positive(params.max_tokens).unwrap_or(markdown::MAX_TOKENS),
            token_overlap: params
                .token_overlap
                .and_then(|n| usize::try_from(n).ok())
                .unwrap_or(markdown::TOKEN_OVERLAP),
        },
    )
}

fn text_chunks(text: &str, params: &ChunkParams) -> Result<Vec<Chunk>, ChunkError> {
    markdown::text(
        text,
        positive(params.max_tokens).unwrap_or(markdown::MAX_TOKENS),
        params
            .token_overlap
            .and_then(|n| usize::try_from(n).ok())
            .unwrap_or(markdown::TOKEN_OVERLAP),
    )
}

fn json_chunks(text: &str, params: &ChunkParams) -> Result<Vec<Chunk>, ChunkError> {
    match json::split(
        text,
        positive(params.max_tokens).unwrap_or(json::MAX_CHUNK_SIZE),
    ) {
        json::Split::Whole => Ok(vec![
            Chunk::new(text.to_owned(), 1, "document", "json").with("headers", ""),
        ]),
        json::Split::Chunks(parts) => Ok(parts
            .into_iter()
            .enumerate()
            .map(|(i, part)| Chunk::new(part, i + 1, "document", "json").with("headers", ""))
            .collect()),
        // The SDK yields such a document unchunked; a long one would
        // overflow the embedding model, so it is split as text.
        json::Split::NotAnObject => text_chunks(text, params),
    }
}

fn code_chunks(
    text: &str,
    extension: &str,
    params: &ChunkParams,
) -> Result<Vec<Chunk>, ChunkError> {
    let overlap =
        |own: Option<i64>, other: Option<i64>| own.or(other).and_then(|n| usize::try_from(n).ok());
    code::code(
        text,
        extension,
        &Code {
            chunk_size: positive(params.max_tokens.or(params.chunk_size))
                .unwrap_or(code::CHUNK_SIZE),
            chunk_overlap: overlap(params.token_overlap, params.chunk_overlap)
                .unwrap_or(code::CHUNK_OVERLAP),
            unknown_chunk_size: positive(params.unknown_chunk_size)
                .unwrap_or(code::UNKNOWN_CHUNK_SIZE),
            unknown_chunk_overlap: params
                .unknown_chunk_overlap
                .and_then(|n| usize::try_from(n).ok())
                .unwrap_or(code::UNKNOWN_CHUNK_OVERLAP),
        },
    )
}

/// Chunk `document_text`. `source` says what the document is: a file name
/// or path, an extension (`md`, `.md`) or a media type (`text/markdown`).
///
/// The chunker is `config`'s `chunking_tool` when it names one, else the
/// router's pick for the extension; settings are the extension's own keys
/// over the top-level ones over the SDK defaults.
///
/// # Errors
/// [`ChunkError::RequiresModel`] for the statistical and proposal chunkers
/// and for `use_llm`; [`ChunkError::UnknownChunker`] for a `chunking_tool`
/// that names none; [`ChunkError::Tokenizer`] if the encoding is unavailable.
pub fn chunk(
    document_text: &str,
    source: &str,
    config: &ChunkingConfig,
) -> Result<Vec<Chunk>, ChunkError> {
    let extension = extension_of(source);
    let params = config.params_for(&extension);
    if params.use_llm == Some(true) {
        return Err(ChunkError::RequiresModel {
            chunker: "use_llm",
            reason: format!("`use_llm` is set for {extension:?} and model steps are not ported"),
        });
    }
    let chunker = match params.chunker.as_deref() {
        Some(name) => chunker_named(name, &extension)?,
        None => chunker_for(&extension),
    };
    match chunker {
        Chunker::Markdown => markdown_chunks(document_text, &params),
        Chunker::Text => text_chunks(document_text, &params),
        Chunker::Json => json_chunks(document_text, &params),
        Chunker::Code => code_chunks(document_text, &extension, &params),
    }
}

#[cfg(test)]
mod tests {
    #![allow(clippy::format_collect)]
    use super::*;
    use serde_json::json;

    #[test]
    fn extensions_come_from_names_bare_extensions_and_media_types() {
        for (source, expected) in [
            ("docs/Guide.MD", ".md"),
            ("md", ".md"),
            (".md", ".md"),
            ("text/markdown; charset=utf-8", ".md"),
            ("application/json", ".json"),
            ("src\\main.RS", ".rs"),
            ("text/plain", ".txt"),
            ("Makefile", ""),
            ("", ""),
        ] {
            assert_eq!(extension_of(source), expected, "{source}");
        }
    }

    #[test]
    fn the_router_picks_by_extension() {
        for (ext, chunker) in [
            (".md", Chunker::Markdown),
            (".mdx", Chunker::Markdown),
            (".json", Chunker::Json),
            (".py", Chunker::Code),
            (".sql", Chunker::Code),
            (".txt", Chunker::Text),
            (".yaml", Chunker::Text),
            (".csv", Chunker::Text),
            ("", Chunker::Text),
        ] {
            assert_eq!(chunker_for(ext), chunker, "{ext}");
        }
    }

    #[test]
    fn model_chunkers_and_use_llm_are_typed_outcomes() {
        let statistical = ChunkingConfig::from_value(&json!({"chunking_tool": "statistical"}))
            .unwrap_or_default();
        assert!(matches!(
            chunk("text", "a.txt", &statistical),
            Err(ChunkError::RequiresModel {
                chunker: "statistical",
                ..
            })
        ));
        let proposal = ChunkingConfig::from_value(&json!({".md": {"chunking_tool": "proposal"}}))
            .unwrap_or_default();
        assert!(matches!(
            chunk("# t", "a.md", &proposal),
            Err(ChunkError::RequiresModel {
                chunker: "proposal",
                ..
            })
        ));
        // Another extension is unaffected.
        assert!(chunk("plain", "a.txt", &proposal).is_ok());
        let llm =
            ChunkingConfig::from_value(&json!({".pdf": {"use_llm": true}})).unwrap_or_default();
        assert!(matches!(
            chunk("text", "a.pdf", &llm),
            Err(ChunkError::RequiresModel {
                chunker: "use_llm",
                ..
            })
        ));
        let nonsense =
            ChunkingConfig::from_value(&json!({"chunking_tool": "nope"})).unwrap_or_default();
        assert!(matches!(
            chunk("text", "a.txt", &nonsense),
            Err(ChunkError::UnknownChunker(_))
        ));
    }

    #[test]
    fn config_overrides_reach_the_chunkers() {
        let config = ChunkingConfig::from_value(&json!({
            ".txt": { "max_tokens": 20, "token_overlap": 4 }
        }))
        .unwrap_or_default();
        let text: String = (0..200).map(|n| format!("w{n} ")).collect();
        let small = chunk(&text, "a.txt", &config).unwrap_or_default();
        let default = chunk(&text, "a.txt", &ChunkingConfig::default()).unwrap_or_default();
        assert!(
            small.len() > 5 && default.len() == 1,
            "{} {}",
            small.len(),
            default.len()
        );
    }
}

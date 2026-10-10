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
use elitea_content_source::{Kind, extension_of_mime, kind_of_extension};

/// A chunker.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Chunker {
    Markdown,
    Text,
    Json,
    Code,
}

/// The chunker `universal` picks for a lower-case, dotted extension: the
/// kind the shared extension table (`elitea-content-source`) gives it.
#[must_use]
pub fn chunker_for(extension: &str) -> Chunker {
    match kind_of_extension(extension) {
        // HTML reaches the chunker as markdown (`doc-extract`).
        Kind::Markdown | Kind::Html => Chunker::Markdown,
        Kind::Json => Chunker::Json,
        Kind::Code => Chunker::Code,
        Kind::Text | Kind::Binary => Chunker::Text,
    }
}

/// Top-level types of a media type (RFC 6838 and font/model).
const MIME_TYPES: &[&str] = &[
    "text",
    "application",
    "image",
    "audio",
    "video",
    "font",
    "model",
    "multipart",
    "message",
];

/// `type/subtype` (parameters stripped, lower-case), if `source` is a media
/// type: exactly one `/`, a known top-level type, and a subtype of token
/// characters. Anything else (`docs/README`, `a/b.md?raw=1`) is a path.
fn parse_mime(source: &str) -> Option<String> {
    let essence = source.split(';').next()?.trim().to_ascii_lowercase();
    let (top, subtype) = essence.split_once('/')?;
    let token = |c: char| c.is_ascii_alphanumeric() || "!#$&^_.+-".contains(c);
    (MIME_TYPES.contains(&top) && !subtype.is_empty() && subtype.chars().all(token))
        .then_some(essence)
}

/// The extension a media type stands for: the table's, else its structured
/// suffix (`+json`, `+xml`). `None` when it names neither.
fn mime_extension(mime: &str) -> Option<String> {
    extension_of_mime(mime).or_else(|| {
        if mime.ends_with("+json") {
            Some(".json".to_owned())
        } else if mime.ends_with("+xml") {
            Some(".xml".to_owned())
        } else {
            None
        }
    })
}

/// The dotted lower-case extension of `source`: a file name, path or URL
/// (`docs/Guide.MD`, `a/b.md?raw=1`), a bare extension (`md`, `.md`) or a
/// media type (`text/markdown`, `application/vnd.api+json`). A source with
/// no extension gives `""`.
#[must_use]
pub fn extension_of(source: &str) -> String {
    let source = source.trim();
    if let Some(mime) = parse_mime(source) {
        if let Some(extension) = mime_extension(&mime) {
            return extension;
        }
        // A media type this table does not know. A subtype with a dot may be
        // a path (`text/readme.md`); the others are types.
        if !mime.rsplit('/').next().unwrap_or("").contains('.') {
            return if mime.starts_with("text/") {
                ".txt".to_owned()
            } else {
                String::new()
            };
        }
    }
    // A path or URL: no query, no fragment.
    let path = source.split(['?', '#']).next().unwrap_or("");
    let path = path.trim().to_ascii_lowercase();
    let name = path.rsplit(['/', '\\']).next().unwrap_or("");
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

/// A set size: a positive number (zero and negative mean "default": the CSV
/// and Excel loaders use -1 for "no limit").
fn positive(value: Option<i64>) -> Option<usize> {
    value
        .and_then(|n| usize::try_from(n).ok())
        .filter(|n| *n > 0)
}

/// An overlap: zero or more.
fn non_negative(value: Option<i64>) -> Option<usize> {
    value.and_then(|n| usize::try_from(n).ok())
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
            token_overlap: non_negative(params.token_overlap).unwrap_or(markdown::TOKEN_OVERLAP),
        },
    )
}

fn text_chunks(text: &str, params: &ChunkParams) -> Result<Vec<Chunk>, ChunkError> {
    markdown::text(
        text,
        positive(params.max_tokens).unwrap_or(markdown::MAX_TOKENS),
        non_negative(params.token_overlap).unwrap_or(markdown::TOKEN_OVERLAP),
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
            .map(|(i, part)| {
                let chunk = Chunk::new(part.text, i + 1, "document", "json").with("headers", "");
                match part.path {
                    Some(path) => chunk.with("json_path", &path),
                    None => chunk,
                }
            })
            .collect()),
        // The SDK yields such a document unchunked; a long one would
        // overflow the embedding model, so it is split as text.
        json::Split::NotAnObject => text_chunks(text, params),
    }
}

/// The code chunker's settings: `max_tokens` over `chunk_size`, and
/// `token_overlap` over `chunk_overlap`, each falling to the other when the
/// first is unset or unusable (zero, negative).
fn code_settings(params: &ChunkParams) -> Code {
    Code {
        chunk_size: positive(params.max_tokens)
            .or(positive(params.chunk_size))
            .unwrap_or(code::CHUNK_SIZE),
        chunk_overlap: non_negative(params.token_overlap)
            .or(non_negative(params.chunk_overlap))
            .unwrap_or(code::CHUNK_OVERLAP),
        unknown_chunk_size: positive(params.unknown_chunk_size).unwrap_or(code::UNKNOWN_CHUNK_SIZE),
        unknown_chunk_overlap: non_negative(params.unknown_chunk_overlap)
            .unwrap_or(code::UNKNOWN_CHUNK_OVERLAP),
    }
}

fn code_chunks(
    text: &str,
    extension: &str,
    params: &ChunkParams,
) -> Result<Vec<Chunk>, ChunkError> {
    code::code(text, extension, &code_settings(params))
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

    #[test]
    fn sources_are_classified_as_media_types_or_paths() {
        for (source, expected) in [
            ("application/vnd.api+json", ".json"),
            ("application/ld+json; charset=utf-8", ".json"),
            ("application/atom+xml", ".xml"),
            ("application/vnd.ms-excel", ".xls"),
            ("application/pdf", ".pdf"),
            ("text/x-unknown", ".txt"),
            ("image/svg", ""),
            // Not media types: the first part is no top-level type.
            ("docs/README", ""),
            ("docs/guide.md", ".md"),
            ("a/b.md?raw=1", ".md"),
            ("https://example.com/x/page.html#top", ".html"),
            ("https://example.com/x/data.json?v=2#frag", ".json"),
            // A dotted last part is a path unless the table knows the type.
            ("text/readme.md", ".md"),
            ("model/part.json", ".json"),
        ] {
            assert_eq!(extension_of(source), expected, "{source}");
        }
    }

    #[test]
    fn kotlin_script_is_code_like_kotlin() {
        assert_eq!(
            chunker_for(&extension_of("build.gradle.kts")),
            Chunker::Code
        );
        assert_eq!(chunker_for(&extension_of("a.xhtml")), Chunker::Markdown);
        assert_eq!(
            chunker_for(&extension_of("application/xhtml+xml")),
            Chunker::Markdown
        );
    }

    #[test]
    fn max_tokens_beats_chunk_size_and_an_unusable_one_falls_to_the_other() {
        let params = |max, size, token_overlap, chunk_overlap| ChunkParams {
            max_tokens: max,
            chunk_size: size,
            token_overlap,
            chunk_overlap,
            ..ChunkParams::default()
        };
        let settings = code_settings(&params(Some(100), Some(300), Some(5), Some(50)));
        assert_eq!((settings.chunk_size, settings.chunk_overlap), (100, 5));
        // -1 is "no limit" in the loaders that write it: not a size.
        let settings = code_settings(&params(Some(-1), Some(300), Some(-1), Some(50)));
        assert_eq!((settings.chunk_size, settings.chunk_overlap), (300, 50));
        let settings = code_settings(&params(Some(0), None, None, None));
        assert_eq!(
            (settings.chunk_size, settings.chunk_overlap),
            (code::CHUNK_SIZE, code::CHUNK_OVERLAP)
        );
        // An overlap of zero is a set overlap.
        let settings = code_settings(&params(None, Some(300), Some(0), Some(50)));
        assert_eq!(settings.chunk_overlap, 0);
    }
}

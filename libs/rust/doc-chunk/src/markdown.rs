//! The markdown and text chunkers.
//!
//! `markdown_chunker`: `LangChain`'s `MarkdownHeaderTextSplitter` (a line walk
//! that opens a chunk at each header and tracks fenced code), then the SDK's
//! merge of chunks under `min_chunk_chars` into the next one, then a token
//! split of any chunk over `max_tokens`. `text_chunker`: the token split
//! alone.

use crate::tokens::{self, Encoding};
use crate::{Chunk, ChunkError};

/// SDK defaults (`markdown_chunker`, `text_chunker`).
pub(crate) const MAX_TOKENS: usize = 512;
pub(crate) const TOKEN_OVERLAP: usize = 10;
pub(crate) const MIN_CHUNK_CHARS: usize = 100;

/// H1-H4, named as the SDK's universal chunker names them.
pub(crate) fn default_headers() -> Vec<(String, String)> {
    ["#", "##", "###", "####"]
        .iter()
        .enumerate()
        .map(|(i, marker)| ((*marker).to_owned(), format!("Header {}", i + 1)))
        .collect()
}

/// The settings of one markdown run.
pub(crate) struct Markdown<'a> {
    pub headers: &'a [(String, String)],
    pub strip_headers: bool,
    pub return_each_line: bool,
    pub min_chunk_chars: usize,
    pub max_tokens: usize,
    pub token_overlap: usize,
}

/// A block of text and the header path above it (name, text) in level order.
#[derive(Debug, Clone, PartialEq, Eq)]
struct Piece {
    content: String,
    path: Vec<(String, String)>,
}

/// `MarkdownHeaderTextSplitter.split_text`.
fn split_headers(text: &str, settings: &Markdown<'_>) -> Vec<Piece> {
    // Longest marker first, so `###` is tried before `#`.
    let mut markers: Vec<&(String, String)> = settings.headers.iter().collect();
    markers.sort_by_key(|(marker, _)| std::cmp::Reverse(marker.len()));

    let mut pieces: Vec<Piece> = Vec::new();
    let mut content: Vec<String> = Vec::new();
    // (level, name, text) of the headers in force.
    let mut stack: Vec<(usize, String, String)> = Vec::new();
    let path_of = |stack: &[(usize, String, String)]| -> Vec<(String, String)> {
        stack
            .iter()
            .map(|(_, n, t)| (n.clone(), t.clone()))
            .collect()
    };
    let mut current = Vec::new();
    let mut fence: Option<&str> = None;

    for line in text.split('\n') {
        // `strip()`, then only printable characters (as LangChain does).
        let stripped: String = line.trim().chars().filter(|c| !c.is_control()).collect();
        match fence {
            None => {
                if stripped.starts_with("```") && stripped.matches("```").count() == 1 {
                    fence = Some("```");
                } else if stripped.starts_with("~~~") {
                    fence = Some("~~~");
                }
            }
            Some(open) => {
                if stripped.starts_with(open) {
                    fence = None;
                }
            }
        }
        if fence.is_some() {
            // Inside a fence (the opening line included): kept as written.
            content.push(stripped);
            continue;
        }

        let header = markers.iter().find(|(marker, _)| {
            stripped.starts_with(marker.as_str())
                && (stripped.len() == marker.len() || stripped[marker.len()..].starts_with(' '))
        });
        if let Some((marker, name)) = header {
            let level = marker.matches('#').count();
            while stack.last().is_some_and(|(l, _, _)| *l >= level) {
                stack.pop();
            }
            stack.push((
                level,
                name.clone(),
                stripped[marker.len()..].trim().to_owned(),
            ));
            if !content.is_empty() {
                pieces.push(Piece {
                    content: content.join("\n"),
                    path: current.clone(),
                });
                content.clear();
            }
            if !settings.strip_headers {
                content.push(stripped);
            }
        } else if !stripped.is_empty() {
            content.push(stripped);
        } else if !content.is_empty() {
            pieces.push(Piece {
                content: content.join("\n"),
                path: current.clone(),
            });
            content.clear();
        }
        current = path_of(&stack);
    }
    if !content.is_empty() {
        pieces.push(Piece {
            content: content.join("\n"),
            path: current,
        });
    }

    if settings.return_each_line {
        return pieces;
    }
    aggregate(pieces, settings.strip_headers)
}

/// `aggregate_lines_to_chunks`: blocks with the same header path join with
/// `"  \n"`; a block that is only a header line joins the block below it.
fn aggregate(pieces: Vec<Piece>, strip_headers: bool) -> Vec<Piece> {
    let mut out: Vec<Piece> = Vec::new();
    for piece in pieces {
        match out.last_mut() {
            Some(last) if last.path == piece.path => {
                last.content.push_str("  \n");
                last.content.push_str(&piece.content);
            }
            Some(last)
                if last.path.len() < piece.path.len()
                    && !strip_headers
                    && last
                        .content
                        .rsplit('\n')
                        .next()
                        .is_some_and(|l| l.starts_with('#')) =>
            {
                last.path = piece.path;
                last.content.push_str("  \n");
                last.content.push_str(&piece.content);
            }
            _ => out.push(piece),
        }
    }
    out
}

/// `_merge_small_chunks`: a block under `min_chars` joins the next block
/// (with `"\n\n"`) until the result reaches `min_chars`.
fn merge_small(pieces: Vec<Piece>, min_chars: usize) -> Vec<Piece> {
    let mut merged = Vec::new();
    let mut pending: Option<Piece> = None;
    for piece in pieces {
        let content = piece.content.trim().to_owned();
        match pending.take() {
            Some(mut waiting) => {
                waiting.content = format!("{}\n\n{content}", waiting.content);
                for (name, text) in piece.path {
                    match waiting.path.iter_mut().find(|(n, _)| *n == name) {
                        Some((_, existing)) if existing.is_empty() => *existing = text,
                        Some(_) => {}
                        None => waiting.path.push((name, text)),
                    }
                }
                if waiting.content.chars().count() >= min_chars {
                    merged.push(waiting);
                } else {
                    pending = Some(waiting);
                }
            }
            None if content.chars().count() < min_chars => {
                pending = Some(Piece {
                    content,
                    path: piece.path,
                });
            }
            None => merged.push(piece),
        }
    }
    merged.extend(pending);
    merged
}

/// Chunk markdown.
pub(crate) fn markdown(text: &str, settings: &Markdown<'_>) -> Result<Vec<Chunk>, ChunkError> {
    let mut pieces = split_headers(text, settings);
    if !settings.return_each_line {
        pieces = merge_small(pieces, settings.min_chunk_chars);
    }
    let mut chunks = Vec::new();
    for piece in pieces {
        if piece.content.trim().is_empty() {
            continue;
        }
        let headers = piece
            .path
            .iter()
            .map(|(_, text)| text.as_str())
            .collect::<Vec<_>>()
            .join("; ");
        // Encoded once: the ids count the piece and, if it is over, split it.
        let ids = tokens::encode(&piece.content, Encoding::Cl100k)?;
        let (parts, method) = if ids.len() > settings.max_tokens {
            let parts = tokens::split_ids(
                &ids,
                Encoding::Cl100k,
                settings.max_tokens,
                settings.token_overlap,
            )?;
            (parts, "text")
        } else {
            (vec![piece.content], "markdown")
        };
        for part in parts {
            let id = chunks.len() + 1;
            chunks.push(Chunk::new(part, id, "document", method).with("headers", &headers));
        }
    }
    Ok(chunks)
}

/// Chunk plain text: token windows, `headers` empty, `method_name` `text`.
pub(crate) fn text(
    text: &str,
    max_tokens: usize,
    token_overlap: usize,
) -> Result<Vec<Chunk>, ChunkError> {
    Ok(
        tokens::split(text, Encoding::Cl100k, max_tokens, token_overlap)?
            .into_iter()
            .enumerate()
            .map(|(i, part)| Chunk::new(part, i + 1, "document", "text").with("headers", ""))
            .collect(),
    )
}

#[cfg(test)]
mod tests {
    #![allow(clippy::format_collect)]
    use super::*;

    fn settings(headers: &[(String, String)]) -> Markdown<'_> {
        Markdown {
            headers,
            strip_headers: false,
            return_each_line: false,
            min_chunk_chars: 0,
            max_tokens: MAX_TOKENS,
            token_overlap: TOKEN_OVERLAP,
        }
    }

    #[test]
    fn nested_headers_become_the_chunks_header_path() {
        let headers = default_headers();
        let text = "# Guide\nintro line\n\n## Setup\nInstall it.\n\n### Linux\napt install x\n\n## Usage\nRun it.\n";
        let pieces = split_headers(text, &settings(&headers));
        let shape: Vec<(String, String)> = pieces
            .iter()
            .map(|p| {
                (
                    p.path
                        .iter()
                        .map(|(_, t)| t.as_str())
                        .collect::<Vec<_>>()
                        .join("; "),
                    p.content.clone(),
                )
            })
            .collect();
        assert_eq!(
            shape,
            [
                ("Guide".to_owned(), "# Guide\nintro line".to_owned()),
                (
                    "Guide; Setup".to_owned(),
                    "## Setup\nInstall it.".to_owned()
                ),
                (
                    "Guide; Setup; Linux".to_owned(),
                    "### Linux\napt install x".to_owned()
                ),
                ("Guide; Usage".to_owned(), "## Usage\nRun it.".to_owned()),
            ]
        );
    }

    #[test]
    fn a_header_inside_a_code_fence_is_not_a_header() {
        let headers = default_headers();
        let text = "# Top\n```sh\n# a comment\necho hi\n```\nafter\n";
        let pieces = split_headers(text, &settings(&headers));
        assert_eq!(pieces.len(), 1, "{pieces:?}");
        assert!(pieces[0].content.contains("# a comment"));
        // `#hashtag` (no space) is not a header either.
        let pieces = split_headers("#hashtag here\nbody", &settings(&headers));
        assert!(pieces[0].path.is_empty());
    }

    #[test]
    fn small_chunks_merge_into_the_next_and_big_ones_stay() {
        let headers = default_headers();
        let big = "word ".repeat(40);
        let text = format!("# A\n\n## B\n{big}\n\n## C\nshort\n");
        let mut s = settings(&headers);
        s.min_chunk_chars = MIN_CHUNK_CHARS;
        let chunks = markdown(&text, &s).unwrap_or_else(|e| panic!("{e}"));
        // "# A" is only a header: it joins "## B ...", which is big enough.
        assert_eq!(chunks.len(), 2, "{chunks:?}");
        assert!(chunks[0].text.starts_with("# A"));
        assert_eq!(chunks[0].metadata["headers"], "A; B");
        assert_eq!(chunks[1].metadata["headers"], "A; C");
        assert_eq!(chunks[1].chunk_id, 2);
        assert_eq!(chunks[0].metadata["method_name"], "markdown");
        assert_eq!(chunks[0].metadata["chunk_type"], "document");
    }

    #[test]
    fn a_chunk_over_the_token_limit_is_token_split_and_named_text() {
        let headers = default_headers();
        let body: String = (0..400).map(|n| format!("alpha{n} ")).collect();
        let text = format!("# Long\n{body}");
        let mut s = settings(&headers);
        s.max_tokens = 64;
        s.token_overlap = 8;
        let chunks = markdown(&text, &s).unwrap_or_else(|e| panic!("{e}"));
        assert!(chunks.len() > 3);
        for chunk in &chunks {
            assert!(tokens::count(&chunk.text).unwrap_or(usize::MAX) <= 64);
            assert_eq!(chunk.metadata["method_name"], "text");
            assert_eq!(chunk.metadata["headers"], "Long");
        }
    }

    #[test]
    fn text_chunks_carry_empty_headers() {
        let chunks = text("hello world", 512, 10).unwrap_or_else(|e| panic!("{e}"));
        assert_eq!(chunks.len(), 1);
        assert_eq!(chunks[0].metadata["headers"], "");
        assert_eq!(chunks[0].metadata["method_name"], "text");
        assert!(text("", 512, 10).unwrap_or_default().is_empty());
    }
}

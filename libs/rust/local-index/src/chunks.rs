//! A workspace file as search chunks: what `chunks` and `chunks_fts` hold.
//!
//! The text is cut by `elitea-doc-chunk`'s extension router (tree-sitter
//! methods for code, header sections for markdown, token windows for the
//! rest), as the cloud index cuts it. That router leaves out what is not in
//! a method (imports, constants, type definitions, module docs); here those
//! lines become chunks of their own ("gap" chunks), so every line of a file
//! is searchable. Each chunk is located back in the file to give its line
//! range.
//!
//! Beside its text a chunk carries an identifier column: every identifier
//! split at `camelCase` and `snake_case` boundaries, and the parts of its path,
//! so `index registry` finds `IndexRegistry` and `sqlite store` finds
//! `sqlite_store.rs`.

use elitea_content_source::{Kind, kind_of_extension};
use elitea_doc_chunk::{ChunkingConfig, chunk, extension_of};
use sha2::{Digest as _, Sha256};

/// Bumped when chunking changes: an index whose chunks were made by another
/// generation re-reads every file once (unchanged ones are not re-parsed).
pub const GENERATION: &str = "1";

/// The most lines a gap chunk spans.
const GAP_LINES: usize = 60;
/// The most characters a gap chunk holds.
const GAP_CHARS: usize = 2000;

/// What a chunk is, for `search_workspace`'s `kind` filter.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum ChunkKind {
    Code,
    Docs,
}

impl ChunkKind {
    #[must_use]
    pub fn as_str(self) -> &'static str {
        match self {
            Self::Code => "code",
            Self::Docs => "docs",
        }
    }

    #[must_use]
    pub fn parse(text: &str) -> Option<Self> {
        match text {
            "code" => Some(Self::Code),
            "docs" => Some(Self::Docs),
            _ => None,
        }
    }
}

/// One chunk of a file.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ChunkRow {
    /// 1-based, inclusive.
    pub line_start: u32,
    pub line_end: u32,
    pub kind: ChunkKind,
    pub text: String,
    /// The identifier column (lower-case words).
    pub idents: String,
    /// SHA-256 hex of `text`: a vector made from the same text is reused.
    pub text_hash: String,
}

/// The kind of a file, by its extension: markdown, HTML and prose
/// extensions are docs; every other text file is code.
#[must_use]
pub fn kind_of(path: &str) -> ChunkKind {
    let extension = extension_of(path);
    match kind_of_extension(&extension) {
        Kind::Markdown | Kind::Html => ChunkKind::Docs,
        _ if matches!(
            extension.as_str(),
            ".txt" | ".rst" | ".adoc" | ".asciidoc" | ".org" | ".tex"
        ) =>
        {
            ChunkKind::Docs
        }
        _ => ChunkKind::Code,
    }
}

/// The largest data file (JSON, YAML, CSV, …) that is chunked: a bigger
/// one is a dump or a fixture, which buries every search in noise.
const MAX_DATA_BYTES: usize = 256 * 1024;

/// Whether a file of this path and size is chunked at all. Binary
/// documents and images are not (they have no lines to cite), nor are
/// lock files, minified bundles, source maps and large data files.
#[must_use]
#[allow(clippy::case_sensitive_file_extension_comparisons)] // `name` is lower-cased
pub fn is_chunked(path: &str, size: usize) -> bool {
    let name = path.rsplit('/').next().unwrap_or(path).to_lowercase();
    let extension = extension_of(path);
    if matches!(kind_of_extension(&extension), Kind::Binary)
        || name.ends_with(".lock")
        || name.ends_with(".lock.json")
        || name.ends_with(".min.js")
        || name.ends_with(".min.css")
        || name.ends_with(".map")
        || matches!(
            name.as_str(),
            "package-lock.json" | "pnpm-lock.yaml" | "yarn.lock" | "go.sum" | "npm-shrinkwrap.json"
        )
    {
        return false;
    }
    let data = matches!(kind_of_extension(&extension), Kind::Json)
        || matches!(
            extension.as_str(),
            ".yaml" | ".yml" | ".csv" | ".tsv" | ".xml" | ".jsonl" | ".ndjson"
        );
    !(data && size > MAX_DATA_BYTES)
}

fn hash(text: &str) -> String {
    let digest = Sha256::digest(text.as_bytes());
    let mut out = String::with_capacity(64);
    for byte in digest {
        use std::fmt::Write as _;
        let _ = write!(out, "{byte:02x}");
    }
    out
}

/// The words of an identifier: `IndexRegistry` → `index registry`,
/// `for_turn` → `for turn`, `HTTPServer2` → `http server 2`.
pub fn split_identifier(word: &str, out: &mut Vec<String>) {
    let chars: Vec<char> = word.chars().collect();
    let mut current = String::new();
    for (i, &c) in chars.iter().enumerate() {
        if !c.is_alphanumeric() {
            if !current.is_empty() {
                out.push(std::mem::take(&mut current).to_lowercase());
            }
            continue;
        }
        let previous = i.checked_sub(1).map(|p| chars[p]);
        let next = chars.get(i + 1).copied();
        let boundary = match previous {
            Some(p) if p.is_alphanumeric() => {
                (p.is_lowercase() && c.is_uppercase())
                    || (p.is_alphabetic() && c.is_numeric())
                    || (p.is_numeric() && c.is_alphabetic())
                    || (p.is_uppercase()
                        && c.is_uppercase()
                        && next.is_some_and(char::is_lowercase))
            }
            _ => false,
        };
        if boundary && !current.is_empty() {
            out.push(std::mem::take(&mut current).to_lowercase());
        }
        current.push(c);
    }
    if !current.is_empty() {
        out.push(current.to_lowercase());
    }
}

/// The identifier column of a text: the parts of every identifier that has
/// more than one (single words are already in the text column), then the
/// path's parts.
#[must_use]
pub fn identifier_terms(text: &str, path: &str) -> String {
    let mut terms = Vec::new();
    for word in text.split(|c: char| !(c.is_alphanumeric() || c == '_')) {
        if word.len() < 3 {
            continue;
        }
        let mut parts = Vec::new();
        split_identifier(word, &mut parts);
        if parts.len() > 1 {
            terms.extend(parts);
        }
    }
    split_identifier(path, &mut terms);
    terms.join(" ")
}

/// The query terms an identifier-aware search looks for: each word, and
/// the parts of each compound one.
#[must_use]
pub fn query_terms(query: &str) -> Vec<String> {
    let mut terms = Vec::new();
    for word in query.split(|c: char| !(c.is_alphanumeric() || c == '_')) {
        if word.is_empty() {
            continue;
        }
        let lower = word.to_lowercase();
        let mut parts = Vec::new();
        split_identifier(word, &mut parts);
        if parts.len() > 1 {
            terms.extend(parts);
        }
        terms.push(lower);
    }
    let mut seen = std::collections::HashSet::new();
    terms.retain(|term| !STOPWORDS.contains(&term.as_str()) && seen.insert(term.clone()));
    terms
}

/// Words a question is made of that find nothing.
const STOPWORDS: &[&str] = &[
    "a", "an", "and", "are", "as", "at", "be", "by", "can", "do", "does", "for", "from", "how",
    "i", "if", "in", "is", "it", "of", "on", "or", "the", "this", "to", "what", "when", "where",
    "which", "who", "why", "with",
];

/// Line starts (byte offsets) of `text`.
fn line_starts(text: &str) -> Vec<usize> {
    std::iter::once(0)
        .chain(text.match_indices('\n').map(|(at, _)| at + 1))
        .filter(|&at| at <= text.len())
        .collect()
}

/// The 1-based line of byte offset `at`.
fn line_of(starts: &[usize], at: usize) -> usize {
    match starts.binary_search(&at) {
        Ok(i) => i + 1,
        Err(i) => i,
    }
}

/// Where `piece` is in `text`, searching from `from` first: its byte
/// range. A piece that is not a substring (a chunker trimmed or joined it)
/// is placed by its first non-empty line.
fn locate(text: &str, piece: &str, from: usize) -> Option<(usize, usize)> {
    let piece = piece.trim_matches('\n');
    if piece.is_empty() {
        return None;
    }
    let found = text
        .get(from..)
        .and_then(|rest| rest.find(piece))
        .map(|at| at + from)
        .or_else(|| text.find(piece));
    if let Some(at) = found {
        return Some((at, at + piece.len()));
    }
    // Placed by its first and last non-empty lines (a chunker that joins
    // sections rewrites the blank lines and the heading marks between).
    let mut lines = piece.lines().map(str::trim).filter(|line| !line.is_empty());
    let first = lines.next()?;
    let last = lines.next_back().unwrap_or(first);
    let at = text
        .get(from..)
        .and_then(|rest| rest.find(first))
        .map(|at| at + from)
        .or_else(|| text.find(first))?;
    let end = text[at..]
        .find(last)
        .map_or(at + first.len(), |offset| at + offset + last.len());
    Some((at, end))
}

/// The chunks of a file's text.
#[must_use]
pub fn chunk_document(path: &str, text: &str) -> Vec<ChunkRow> {
    let kind = kind_of(path);
    let starts = line_starts(text);
    let total_lines = starts.len();
    let mut covered = vec![false; total_lines + 1];
    let mut ranges: Vec<(usize, usize)> = Vec::new();
    let pieces = chunk(text, path, &ChunkingConfig::default()).unwrap_or_default();
    let mut cursor = 0;
    for piece in &pieces {
        let Some((start, end)) = locate(text, &piece.text, cursor) else {
            continue;
        };
        cursor = start;
        let first = line_of(&starts, start);
        let last = line_of(&starts, end.saturating_sub(1).max(start)).max(first);
        for line in covered.iter_mut().take(last + 1).skip(first) {
            *line = true;
        }
        ranges.push((first, last));
    }
    let lines: Vec<&str> = text.split('\n').collect();
    let mut rows: Vec<ChunkRow> = pieces
        .iter()
        .zip(ranges.iter().copied())
        .map(|(piece, (first, last))| (first, last, piece.text.clone()))
        .chain(gaps(&lines, &covered))
        .filter(|(_, _, chunk_text)| !chunk_text.trim().is_empty())
        .map(|(first, last, chunk_text)| ChunkRow {
            line_start: u32::try_from(first).unwrap_or(u32::MAX),
            line_end: u32::try_from(last).unwrap_or(u32::MAX),
            kind,
            idents: identifier_terms(&chunk_text, path),
            text_hash: hash(&chunk_text),
            text: chunk_text,
        })
        .collect();
    rows.sort_by_key(|row| (row.line_start, row.line_end));
    rows.dedup_by(|a, b| a.line_start == b.line_start && a.text == b.text);
    rows
}

/// The runs of lines no chunk covers, cut at [`GAP_LINES`] lines or
/// [`GAP_CHARS`] characters: `(first, last, text)`, 1-based.
fn gaps(lines: &[&str], covered: &[bool]) -> Vec<(usize, usize, String)> {
    let mut out = Vec::new();
    let mut current: Option<(usize, usize, String)> = None;
    for (index, line) in lines.iter().enumerate() {
        let number = index + 1;
        if covered.get(number).copied().unwrap_or(false) {
            if let Some(gap) = current.take() {
                out.push(gap);
            }
            continue;
        }
        let full = current
            .as_ref()
            .is_some_and(|(first, _, text)| number - first >= GAP_LINES || text.len() >= GAP_CHARS);
        if full && let Some(gap) = current.take() {
            out.push(gap);
        }
        if let Some((_, last, text)) = &mut current {
            *last = number;
            text.push('\n');
            text.push_str(line);
        } else if !line.trim().is_empty() {
            current = Some((number, number, (*line).to_owned()));
        }
    }
    if let Some(gap) = current {
        out.push(gap);
    }
    // A gap's trailing blank lines are not part of it.
    for (first, last, text) in &mut out {
        let trimmed = text.trim_end().len();
        let dropped = text[trimmed..].matches('\n').count();
        text.truncate(trimmed);
        *last = (*last - dropped).max(*first);
    }
    out
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn identifiers_split_at_case_and_underscores() {
        let mut parts = Vec::new();
        split_identifier("IndexRegistry", &mut parts);
        split_identifier("for_turn", &mut parts);
        split_identifier("HTTPServer2", &mut parts);
        assert_eq!(
            parts,
            ["index", "registry", "for", "turn", "http", "server", "2"]
        );
        let terms = identifier_terms("let r = IndexRegistry::new(); x", "src/sqlite_store.rs");
        assert_eq!(terms, "index registry src sqlite store rs");
        assert_eq!(
            query_terms("where is the IndexRegistry?"),
            ["index", "registry", "indexregistry"]
        );
    }

    #[test]
    fn every_line_of_a_rust_file_is_in_a_chunk_with_its_lines() {
        let text = "//! Module docs.\nuse std::fmt;\n\npub const LIMIT: usize = 3;\n\n/// Adds.\npub fn add(a: u32, b: u32) -> u32 {\n    a + b\n}\n\npub struct Point {\n    x: i32,\n}\n";
        let rows = chunk_document("src/lib.rs", text);
        let all: String = rows
            .iter()
            .map(|r| r.text.as_str())
            .collect::<Vec<_>>()
            .join("\n");
        for needle in ["Module docs", "LIMIT", "fn add", "struct Point"] {
            assert!(all.contains(needle), "{needle} in {rows:#?}");
        }
        let add = rows.iter().find(|r| r.text.contains("fn add")).unwrap();
        assert_eq!((add.line_start, add.line_end), (6, 9), "{add:?}");
        assert_eq!(add.kind, ChunkKind::Code);
        let gap = rows.iter().find(|r| r.text.contains("LIMIT")).unwrap();
        assert_eq!(gap.line_start, 1);
        let point = rows
            .iter()
            .find(|r| r.text.contains("struct Point"))
            .unwrap();
        assert_eq!((point.line_start, point.line_end), (11, 13), "{point:?}");
    }

    #[test]
    fn markdown_is_docs_and_sections_keep_their_lines() {
        let text = "# Title\n\nIntro text that is long enough to stand on its own as a chunk of prose here.\n\n## Install\n\nRun the installer and follow the steps it shows, then restart the app.\n";
        let rows = chunk_document("docs/guide.md", text);
        assert!(rows.iter().all(|r| r.kind == ChunkKind::Docs));
        let install = rows.iter().find(|r| r.text.contains("installer")).unwrap();
        // Small sections are merged (the SDK's markdown defaults): one chunk
        // over every line, and no gap chunk repeating a line of it.
        assert_eq!((install.line_start, install.line_end), (1, 7), "{rows:#?}");
        assert_eq!(rows.len(), 1, "{rows:#?}");
        assert!(is_chunked("a.md", 10) && !is_chunked("a.pdf", 10) && !is_chunked("a.png", 10));
        assert!(!is_chunked("web/package-lock.json", 10) && !is_chunked("Cargo.lock", 10));
        assert!(!is_chunked("dist/app.min.js", 10));
        assert!(is_chunked("data.json", 1000) && !is_chunked("data.json", 1 << 20));
        assert!(
            is_chunked("src/huge.rs", 1 << 20),
            "code is chunked at any size"
        );
        assert_eq!(kind_of("README.txt"), ChunkKind::Docs);
        assert_eq!(kind_of("config.yaml"), ChunkKind::Code);
    }
}

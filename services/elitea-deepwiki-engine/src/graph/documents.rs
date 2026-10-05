//! Documentation files as graph nodes (`_parse_documentation_files`).
//!
//! A documentation file (markdown, YAML, JSON, config, scripts, …) becomes
//! one or more text chunks, and each chunk one node. Markdown is split on
//! H1–H4 headers by a transcription of `LangChain`'s
//! `MarkdownHeaderTextSplitter` (langchain-text-splitters 1.1.2, with
//! `strip_headers=False, return_each_line=False`); every other type is one
//! chunk up to 17,000 characters and 30-line chunks above that.
//!
//! The splitter's quirks are reproduced because they shape node ids and
//! line numbers:
//!
//! * every line is `strip()`ped and loses its non-printable characters
//!   BEFORE header detection, so a section's text is not a substring of the
//!   file and `_estimate_line_number` then falls back to line 1;
//! * aggregated lines are joined with `"  \n"`;
//! * the aggregation reads the first character of the previous chunk's last
//!   line, which raises `IndexError` when that line is empty. The engine
//!   catches it and falls back to 50-line generic chunks for the whole
//!   file. `split_text` does not appear to produce such a chunk, but the
//!   fallback is kept so the two cannot drift apart.

use super::pystr;

/// The metadata of a markdown section: `Header 1` … `Header 4`.
type Headers = [Option<String>; 4];

/// One chunk of a documentation file (the Python chunk dict).
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Chunk {
    pub content: String,
    pub summary: String,
    pub start_line: i64,
    pub end_line: i64,
    pub section_type: String,
}

/// One chunk as a symbol (`BasicSymbol` of a documentation result).
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct DocSymbol {
    pub name: String,
    pub symbol_type: String,
    pub start_line: i64,
    pub end_line: i64,
    pub rel_path: String,
    /// `"[File: <rel_path>]\n"` + the chunk text.
    pub source_text: String,
    pub docstring: String,
    /// The document type (`return_type=doc_type`).
    pub return_type: String,
}

/// The chunks of one documentation file.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct DocResult {
    pub file_path: String,
    /// The document type (`markdown`, `yaml`, …), used as the node language.
    pub language: String,
    pub symbols: Vec<DocSymbol>,
}

/// Single-chunk limit for non-markdown documents, in characters.
const MAX_SINGLE_CHUNK_CHARS: usize = 17_000;

/// `os.path.relpath(path, root)` for a path under `root`.
#[must_use]
pub fn relative_path<'p>(path: &'p str, root: &str) -> &'p str {
    let root = root.trim_end_matches('/');
    path.strip_prefix(root)
        .and_then(|rest| rest.strip_prefix('/'))
        .unwrap_or(path)
}

/// Parse documentation files, in the given (sorted) order. A file that
/// cannot be read is skipped, as Python logs and skips it.
#[must_use]
pub fn parse_documentation_files(file_paths: &[String], repo_root: &str) -> Vec<DocResult> {
    let mut results = Vec::with_capacity(file_paths.len());
    for file_path in file_paths {
        let Ok(bytes) = std::fs::read(file_path) else {
            continue;
        };
        let content = pystr::decode_text(&bytes, pystr::Errors::Ignore);
        results.push(document_result(file_path, repo_root, &content));
    }
    results
}

/// The result for one documentation file whose text is `content`.
#[must_use]
pub fn document_result(file_path: &str, repo_root: &str, content: &str) -> DocResult {
    let doc_type = super::discover::doc_type_for(file_path).unwrap_or("text");
    let rel_path = relative_path(file_path, repo_root);
    let symbols = chunk_text_content(content, file_path, doc_type)
        .into_iter()
        .map(|chunk| DocSymbol {
            source_text: format!("[File: {rel_path}]\n{}", chunk.content),
            docstring: chunk.summary.clone(),
            name: chunk.summary,
            symbol_type: chunk.section_type,
            start_line: chunk.start_line,
            end_line: chunk.end_line,
            rel_path: rel_path.to_owned(),
            return_type: doc_type.to_owned(),
        })
        .collect();
    DocResult {
        file_path: file_path.to_owned(),
        language: doc_type.to_owned(),
        symbols,
    }
}

fn count_lines(text: &str) -> i64 {
    i64::try_from(text.split('\n').count()).unwrap_or(i64::MAX)
}

fn count_newlines(text: &str) -> i64 {
    i64::try_from(text.matches('\n').count()).unwrap_or(i64::MAX)
}

/// The whole file as one chunk of type `section_type`.
fn whole_file(content: &str, file_path: &str, section_type: String) -> Chunk {
    Chunk {
        content: content.to_owned(),
        summary: pystr::stem(file_path).to_owned(),
        start_line: 1,
        end_line: count_lines(content),
        section_type,
    }
}

/// `_chunk_text_content`.
#[must_use]
pub fn chunk_text_content(content: &str, file_path: &str, doc_type: &str) -> Vec<Chunk> {
    if doc_type == "markdown" {
        return chunk_markdown_content(content, file_path);
    }
    if content.chars().count() <= MAX_SINGLE_CHUNK_CHARS {
        return vec![whole_file(
            content,
            file_path,
            format!("{doc_type}_document"),
        )];
    }
    // Above 17,000 characters is always above the 13,000 split threshold.
    chunk_generic_text(content, file_path, 30)
}

/// `_chunk_markdown_content`.
#[must_use]
pub fn chunk_markdown_content(content: &str, file_path: &str) -> Vec<Chunk> {
    let Ok(sections) = split_markdown(content) else {
        return chunk_generic_text(content, file_path, 50);
    };
    let mut chunks: Vec<Chunk> = sections
        .iter()
        .enumerate()
        .map(|(index, (section, headers))| {
            let start_line = estimate_line_number(content, section);
            Chunk {
                content: section.clone(),
                summary: extract_section_name(headers, section, index),
                start_line,
                end_line: start_line + count_newlines(section),
                section_type: "markdown_section".to_owned(),
            }
        })
        .collect();
    if chunks.is_empty() {
        chunks.push(whole_file(
            content,
            file_path,
            "markdown_document".to_owned(),
        ));
    }
    chunks
}

/// `_extract_section_name`: the deepest header, else a `#` first line,
/// else `Section <n>`.
fn extract_section_name(headers: &Headers, content: &str, index: usize) -> String {
    if let Some(deepest) = headers.iter().rev().flatten().next() {
        return pystr::strip(deepest).to_owned();
    }
    let first_line = pystr::strip(content.split('\n').next().unwrap_or(""));
    if first_line.starts_with('#') {
        return pystr::strip(first_line.trim_start_matches('#')).to_owned();
    }
    format!("Section {}", index + 1)
}

/// `_estimate_line_number`: 1 + the newlines before the chunk's first
/// occurrence, or 1 when the chunk text is not in the file.
fn estimate_line_number(full: &str, chunk: &str) -> i64 {
    full.find(chunk)
        .map_or(1, |start| count_newlines(&full[..start]) + 1)
}

/// `_chunk_generic_text`: `max_lines`-line chunks, skipping blank ones.
#[must_use]
pub fn chunk_generic_text(content: &str, file_path: &str, max_lines: usize) -> Vec<Chunk> {
    let lines: Vec<&str> = content.split('\n').collect();
    let mut chunks = Vec::new();
    let mut start = 0;
    while start < lines.len() {
        let end = (start + max_lines).min(lines.len());
        let chunk_lines = &lines[start..end];
        let text = chunk_lines.join("\n");
        if !pystr::strip(&text).is_empty() {
            let summary = chunk_lines
                .iter()
                .map(|line| pystr::strip(line))
                .find(|line| !line.is_empty())
                .map_or_else(
                    || format!("{} chunk {}", pystr::stem(file_path), chunks.len() + 1),
                    |line| {
                        let mut summary = pystr::prefix_chars(line, 50).to_owned();
                        if line.chars().count() > 50 {
                            summary.push_str("...");
                        }
                        summary
                    },
                );
            chunks.push(Chunk {
                content: text,
                summary,
                start_line: i64::try_from(start + 1).unwrap_or(i64::MAX),
                end_line: i64::try_from(end).unwrap_or(i64::MAX),
                section_type: "text_chunk".to_owned(),
            });
        }
        start += max_lines;
    }
    if chunks.is_empty() {
        chunks.push(whole_file(content, file_path, "text_document".to_owned()));
    }
    chunks
}

/// The splitter raised (`IndexError` in `aggregate_lines_to_chunks`).
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct SplitError;

/// `MarkdownHeaderTextSplitter.split_text` with H1–H4, headers kept,
/// sections aggregated: `(page_content, metadata)` per section.
///
/// # Errors
///
/// [`SplitError`] where `LangChain` raises `IndexError`.
pub fn split_markdown(text: &str) -> Result<Vec<(String, Headers)>, SplitError> {
    // Sorted by separator length, longest first, as the splitter does.
    const SEPARATORS: [(&str, usize); 4] = [("####", 4), ("###", 3), ("##", 2), ("#", 1)];
    let mut lines_with_metadata: Vec<(String, Headers)> = Vec::new();
    let mut current_content: Vec<String> = Vec::new();
    let mut current_metadata: Headers = Headers::default();
    let mut header_stack: Vec<usize> = Vec::new();
    let mut initial_metadata: Headers = Headers::default();
    let mut in_code_block = false;
    let mut opening_fence = "";

    for line in text.split('\n') {
        let stripped: String = pystr::strip(line)
            .chars()
            .filter(|&c| pystr::is_printable(c))
            .collect();
        if !in_code_block {
            if stripped.starts_with("```") && stripped.matches("```").count() == 1 {
                in_code_block = true;
                opening_fence = "```";
            } else if stripped.starts_with("~~~") {
                in_code_block = true;
                opening_fence = "~~~";
            }
        } else if stripped.starts_with(opening_fence) {
            in_code_block = false;
            opening_fence = "";
        }

        if in_code_block {
            current_content.push(stripped);
            continue;
        }

        let header = SEPARATORS.iter().find(|(sep, _)| {
            stripped.starts_with(sep)
                && (stripped.len() == sep.len() || stripped[sep.len()..].starts_with(' '))
        });
        if let Some(&(sep, level)) = header {
            while header_stack.last().is_some_and(|&top| top >= level) {
                if let Some(popped) = header_stack.pop() {
                    initial_metadata[popped - 1] = None;
                }
            }
            header_stack.push(level);
            initial_metadata[level - 1] = Some(pystr::strip(&stripped[sep.len()..]).to_owned());
            if !current_content.is_empty() {
                lines_with_metadata.push((current_content.join("\n"), current_metadata.clone()));
                current_content.clear();
            }
            current_content.push(stripped);
        } else if !stripped.is_empty() {
            current_content.push(stripped);
        } else if !current_content.is_empty() {
            lines_with_metadata.push((current_content.join("\n"), current_metadata.clone()));
            current_content.clear();
        }
        current_metadata.clone_from(&initial_metadata);
    }
    if !current_content.is_empty() {
        lines_with_metadata.push((current_content.join("\n"), current_metadata));
    }
    aggregate_lines_to_chunks(lines_with_metadata)
}

fn header_count(headers: &Headers) -> usize {
    headers.iter().flatten().count()
}

/// `aggregate_lines_to_chunks`.
fn aggregate_lines_to_chunks(
    lines: Vec<(String, Headers)>,
) -> Result<Vec<(String, Headers)>, SplitError> {
    let mut aggregated: Vec<(String, Headers)> = Vec::new();
    for (content, metadata) in lines {
        if let Some(last) = aggregated.last_mut() {
            if last.1 == metadata {
                last.0.push_str("  \n");
                last.0.push_str(&content);
                continue;
            }
            if header_count(&last.1) < header_count(&metadata) {
                // `content.split("\n")[-1][0]`: IndexError on an empty line.
                let last_line = last.0.rsplit('\n').next().unwrap_or("");
                let first = last_line.chars().next().ok_or(SplitError)?;
                if first == '#' {
                    last.0.push_str("  \n");
                    last.0.push_str(&content);
                    last.1 = metadata;
                    continue;
                }
            }
        }
        aggregated.push((content, metadata));
    }
    Ok(aggregated)
}

#[cfg(test)]
mod tests {
    use super::*;

    fn h(values: [Option<&str>; 4]) -> Headers {
        values.map(|v| v.map(str::to_owned))
    }

    #[test]
    fn splits_on_headers_and_keeps_them() {
        let text = "Intro line\n\n# Title\nbody one\n\n## Sub\n  body two  \n### Deep\nx\n## Other\n### Under\ny\n\nz";
        let sections = split_markdown(text).unwrap();
        let contents: Vec<&str> = sections.iter().map(|(c, _)| c.as_str()).collect();
        assert_eq!(
            contents,
            [
                "Intro line",
                "# Title\nbody one",
                "## Sub\nbody two",
                "### Deep\nx",
                // A deeper section right after a bare header line joins it,
                // and so does the next run with the same headers.
                "## Other  \n### Under\ny  \nz",
            ]
        );
        assert_eq!(sections[1].1, h([Some("Title"), None, None, None]));
        assert_eq!(
            sections[3].1,
            h([Some("Title"), Some("Sub"), Some("Deep"), None])
        );
        assert_eq!(
            sections[4].1,
            h([Some("Title"), Some("Other"), Some("Under"), None])
        );
    }

    #[test]
    fn code_blocks_hide_headers_and_lines_are_stripped() {
        let text = "# A\n```\n# not a header\n\n```\n\u{200b}## B\nz";
        let sections = split_markdown(text).unwrap();
        assert_eq!(sections.len(), 2);
        assert_eq!(sections[0].0, "# A\n```\n# not a header\n\n```");
        // The zero-width space is not printable, so `## B` is a header.
        assert_eq!(sections[1].0, "## B\nz");
    }

    #[test]
    fn an_empty_last_line_is_langchains_index_error() {
        // `split_text` itself never ends a flushed chunk with an empty line;
        // the aggregation step still fails on one, as LangChain's does.
        let lines = vec![
            ("```\n".to_owned(), h([None, None, None, None])),
            ("# A".to_owned(), h([Some("A"), None, None, None])),
        ];
        assert_eq!(aggregate_lines_to_chunks(lines), Err(SplitError));
    }

    #[test]
    fn markdown_chunks_get_names_and_line_numbers() {
        let text = "# Title\nbody\n\n## Next\nmore\n";
        let chunks = chunk_markdown_content(text, "/r/README.md");
        let names: Vec<&str> = chunks.iter().map(|c| c.summary.as_str()).collect();
        assert_eq!(names, ["Title", "Next"]);
        assert_eq!((chunks[0].start_line, chunks[0].end_line), (1, 2));
        assert_eq!((chunks[1].start_line, chunks[1].end_line), (4, 5));
        // An aggregated section ("  \n" joins) is not a substring: line 1.
        let chunks = chunk_markdown_content("x\n\n## A\np1\n\np2\n", "/r/a.md");
        assert_eq!(chunks[0].summary, "Section 1");
        assert_eq!(chunks[1].content, "## A\np1  \np2");
        assert_eq!((chunks[1].start_line, chunks[1].end_line), (1, 3));
        // No content at all: one markdown_document chunk.
        let chunks = chunk_markdown_content("", "/r/empty.md");
        assert_eq!(chunks[0].section_type, "markdown_document");
        assert_eq!(chunks[0].summary, "empty");
    }

    #[test]
    fn other_documents_are_one_chunk_up_to_17000_characters() {
        let small = "a: 1\nb: 2";
        let chunks = chunk_text_content(small, "/r/x.yaml", "yaml");
        assert_eq!(chunks.len(), 1);
        assert_eq!(chunks[0].section_type, "yaml_document");
        assert_eq!((chunks[0].start_line, chunks[0].end_line), (1, 2));
        let line = "é".repeat(99);
        let big: Vec<String> = (0..200).map(|_| line.clone()).collect();
        let big = big.join("\n");
        assert!(big.chars().count() > 17_000);
        let chunks = chunk_text_content(&big, "/r/x.json", "json");
        assert_eq!(chunks.len(), 7);
        assert_eq!((chunks[6].start_line, chunks[6].end_line), (181, 200));
        assert_eq!(chunks[0].summary.chars().count(), 53);
        assert!(chunks[0].summary.ends_with("..."));
    }

    #[test]
    fn a_document_result_carries_the_file_header() {
        let result = document_result("/r/deploy/Makefile", "/r", "all:\n\techo");
        assert_eq!(result.language, "build_config");
        let symbol = &result.symbols[0];
        assert_eq!(symbol.name, "Makefile");
        assert_eq!(symbol.symbol_type, "build_config_document");
        assert_eq!(symbol.source_text, "[File: deploy/Makefile]\nall:\n\techo");
        assert_eq!(symbol.rel_path, "deploy/Makefile");
    }
}

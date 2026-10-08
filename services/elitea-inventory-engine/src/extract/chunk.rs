//! Chunking a file for the model, with `text-splitter`: chunks of at most
//! 1000 characters overlapping by 100, as the Python pipeline's
//! `RecursiveCharacterTextSplitter(chunk_size=1000, chunk_overlap=100)`.
//!
//! A source file whose language has a tree-sitter grammar is split along
//! its syntax tree (`CodeSplitter`), as the Python pipeline split code with
//! the SDK's tree-sitter chunker; any other text is split at its largest
//! semantic boundary that fits (paragraphs, then lines, sentences, words).
//!
//! Each chunk also records its first line, which the Python pipeline
//! computed and never applied, so every model citation past a file's first
//! chunk pointed at the wrong lines; here they are made absolute.

use crate::ingest::parse::language_of;
use text_splitter::{ChunkConfig, CodeSplitter, TextSplitter};

/// One chunk of a file.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Chunk {
    pub text: String,
    /// The 1-based line of the file the chunk starts on.
    pub start_line: usize,
}

const CHUNK_SIZE: usize = 1000;
const CHUNK_OVERLAP: usize = 100;

fn config() -> ChunkConfig<text_splitter::Characters> {
    ChunkConfig::new(CHUNK_SIZE)
        .with_overlap(CHUNK_OVERLAP)
        .unwrap_or_else(|_| ChunkConfig::new(CHUNK_SIZE))
}

/// The chunks' byte offsets and texts: by syntax for a file with a
/// grammar (and a grammar the splitter accepts), else by text.
fn pieces<'t>(path: &str, text: &'t str) -> Vec<(usize, &'t str)> {
    let code = language_of(path)
        .and_then(|language| elitea_code_parsers::grammar_for(language, path))
        .and_then(|grammar| CodeSplitter::new(grammar, config()).ok());
    match code {
        Some(splitter) => splitter.chunk_indices(text).collect(),
        None => TextSplitter::new(config()).chunk_indices(text).collect(),
    }
}

/// Split the file at `path`, each chunk with its first line. A text the
/// splitter leaves nothing of (all whitespace) is one chunk, as the Python
/// pipeline fell back to the whole document.
#[must_use]
pub fn chunks(path: &str, text: &str) -> Vec<Chunk> {
    let pieces = pieces(path, text);
    if pieces.is_empty() {
        return vec![Chunk {
            text: text.to_owned(),
            start_line: 1,
        }];
    }
    pieces
        .into_iter()
        .map(|(offset, piece)| Chunk {
            start_line: text[..offset].matches('\n').count() + 1,
            text: piece.to_owned(),
        })
        .collect()
}

#[cfg(test)]
mod tests {
    use super::*;

    fn length(text: &str) -> usize {
        text.chars().count()
    }

    #[test]
    fn a_short_text_is_one_chunk() {
        assert_eq!(
            chunks("a.md", "  hello\nworld \n"),
            [Chunk {
                text: "hello\nworld".to_owned(),
                start_line: 1
            }]
        );
        assert_eq!(chunks("a.md", " \n ")[0].start_line, 1);
    }

    #[test]
    fn paragraphs_are_merged_up_to_the_size_and_overlap() {
        let paragraph = |n: usize| format!("{}{n}", "word ".repeat(60));
        let text = (0..10).map(paragraph).collect::<Vec<_>>().join("\n\n");
        let pieces = chunks("a.md", &text);
        assert!(pieces.len() > 1);
        for piece in &pieces {
            assert!(length(&piece.text) <= CHUNK_SIZE, "{}", length(&piece.text));
        }
        // Every paragraph ends up in some chunk, and lines are absolute.
        for n in 0..10 {
            assert!(pieces.iter().any(|p| p.text.contains(&format!("word {n}"))));
        }
        let last = pieces.last().unwrap_or_else(|| panic!("chunks"));
        assert_eq!(
            last.start_line,
            text[..text.rfind(&last.text).unwrap_or(0)]
                .matches('\n')
                .count()
                + 1
        );
        assert!(last.start_line > 1);
    }

    #[test]
    fn a_long_line_without_separators_is_cut_by_characters() {
        let text = "x".repeat(2500);
        let pieces = chunks("a.txt", &text);
        assert_eq!(pieces.len(), 3);
        assert!(pieces.iter().all(|p| length(&p.text) <= CHUNK_SIZE));
    }

    #[test]
    fn code_is_split_along_its_syntax() {
        let function = |n: usize| {
            format!(
                "def handler_{n}(event):\n{}    return event\n",
                "    event = event + 1\n".repeat(12)
            )
        };
        let text = (0..6).map(function).collect::<Vec<_>>().join("\n");
        let pieces = chunks("src/app.py", &text);
        assert!(pieces.len() > 1);
        for piece in &pieces {
            assert!(length(&piece.text) <= CHUNK_SIZE);
            assert!(
                piece.text.starts_with("def handler_"),
                "a chunk starts at a function: {:?}",
                &piece.text[..20]
            );
            assert_eq!(
                text.lines().nth(piece.start_line - 1),
                piece.text.lines().next()
            );
        }
    }
}

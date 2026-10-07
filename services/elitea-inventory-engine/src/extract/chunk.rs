//! Chunking a file for the model: `LangChain`'s
//! `RecursiveCharacterTextSplitter(chunk_size=1000, chunk_overlap=100)`
//! with its defaults (separators `"\n\n"`, `"\n"`, `" "`, `""`; separators
//! kept at the start of the next piece; chunks whitespace-stripped; lengths
//! in characters).
//!
//! The Python pipeline split code files with the SDK's tree-sitter chunker
//! instead; here every file is split the same way (a chunk is at most 200
//! numbered lines to the model either way). Each chunk also records its
//! first line, which the Python pipeline computed and never applied, so
//! every model citation past a file's first chunk pointed at the wrong
//! lines; here they are made absolute.

/// One chunk of a file.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Chunk {
    pub text: String,
    /// The 1-based line of the file the chunk starts on.
    pub start_line: usize,
}

const CHUNK_SIZE: usize = 1000;
const CHUNK_OVERLAP: usize = 100;
const SEPARATORS: [&str; 4] = ["\n\n", "\n", " ", ""];

fn length(text: &str) -> usize {
    text.chars().count()
}

/// `_split_text_with_regex(text, separator, keep_separator=True)`.
fn split_keeping_separator(text: &str, separator: &str) -> Vec<String> {
    if separator.is_empty() {
        return text.chars().map(String::from).collect();
    }
    let mut pieces: Vec<String> = Vec::new();
    let mut parts = text.split(separator);
    if let Some(first) = parts.next() {
        pieces.push(first.to_owned());
    }
    for part in parts {
        pieces.push(format!("{separator}{part}"));
    }
    pieces
        .into_iter()
        .filter(|piece| !piece.is_empty())
        .collect()
}

fn join(docs: &[String], separator: &str) -> Option<String> {
    let joined = docs.join(separator);
    let stripped = elitea_engine_core::pystr::strip(&joined);
    (!stripped.is_empty()).then(|| stripped.to_owned())
}

/// `_merge_splits`.
fn merge(splits: &[String], separator: &str) -> Vec<String> {
    let separator_len = length(separator);
    let mut docs = Vec::new();
    let mut current: std::collections::VecDeque<String> = std::collections::VecDeque::new();
    let mut total = 0usize;
    for split in splits {
        let len = length(split);
        let joiner = |current: &std::collections::VecDeque<String>| {
            if current.is_empty() { 0 } else { separator_len }
        };
        if total + len + joiner(&current) > CHUNK_SIZE && !current.is_empty() {
            if let Some(doc) = join(current.make_contiguous(), separator) {
                docs.push(doc);
            }
            while total > CHUNK_OVERLAP
                || (total + len + joiner(&current) > CHUNK_SIZE && total > 0)
            {
                let Some(first) = current.pop_front() else {
                    break;
                };
                total -= length(&first) + if current.is_empty() { 0 } else { separator_len };
            }
        }
        current.push_back(split.clone());
        total += len + if current.len() > 1 { separator_len } else { 0 };
    }
    if let Some(doc) = join(current.make_contiguous(), separator) {
        docs.push(doc);
    }
    docs
}

/// `_split_text`.
fn split(text: &str, separators: &[&str]) -> Vec<String> {
    let mut separator = separators[separators.len() - 1];
    let mut rest: &[&str] = &[];
    for (index, candidate) in separators.iter().enumerate() {
        if candidate.is_empty() {
            separator = candidate;
            break;
        }
        if text.contains(candidate) {
            separator = candidate;
            rest = &separators[index + 1..];
            break;
        }
    }
    let splits = split_keeping_separator(text, separator);
    let mut chunks = Vec::new();
    let mut good: Vec<String> = Vec::new();
    for piece in splits {
        if length(&piece) < CHUNK_SIZE {
            good.push(piece);
        } else {
            if !good.is_empty() {
                chunks.extend(merge(&good, ""));
                good.clear();
            }
            if rest.is_empty() {
                chunks.push(piece);
            } else {
                chunks.extend(split(&piece, rest));
            }
        }
    }
    if !good.is_empty() {
        chunks.extend(merge(&good, ""));
    }
    chunks
}

/// Split `text`, each chunk with its first line. A text the splitter
/// leaves nothing of (all whitespace) is one chunk, as the Python pipeline
/// fell back to the whole document.
#[must_use]
pub fn chunks(text: &str) -> Vec<Chunk> {
    let pieces = split(text, &SEPARATORS);
    if pieces.is_empty() {
        return vec![Chunk {
            text: text.to_owned(),
            start_line: 1,
        }];
    }
    // `add_start_index`: each chunk is found from just past the previous
    // one's start (chunks overlap).
    let mut search_from = 0usize;
    pieces
        .into_iter()
        .map(|piece| {
            let start = text[search_from..]
                .find(&piece)
                .map_or(search_from, |offset| search_from + offset);
            search_from = text[start..]
                .char_indices()
                .nth(1)
                .map_or(text.len(), |(offset, _)| start + offset);
            Chunk {
                start_line: text[..start].matches('\n').count() + 1,
                text: piece,
            }
        })
        .collect()
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn a_short_text_is_one_chunk() {
        assert_eq!(
            chunks("  hello\nworld \n"),
            [Chunk {
                text: "hello\nworld".to_owned(),
                start_line: 1
            }]
        );
    }

    #[test]
    fn paragraphs_are_merged_up_to_the_size_and_overlap() {
        let paragraph = |n: usize| format!("{}{n}", "word ".repeat(60));
        let text = (0..10).map(paragraph).collect::<Vec<_>>().join("\n\n");
        let pieces = chunks(&text);
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
            text[..text.find(&last.text).unwrap_or(0)]
                .matches('\n')
                .count()
                + 1
        );
        assert!(last.start_line > 1);
    }

    #[test]
    fn a_long_line_without_separators_is_cut_by_characters() {
        let text = "x".repeat(2500);
        let pieces = chunks(&text);
        assert_eq!(pieces.len(), 3);
        assert!(pieces.iter().all(|p| length(&p.text) <= CHUNK_SIZE));
    }
}

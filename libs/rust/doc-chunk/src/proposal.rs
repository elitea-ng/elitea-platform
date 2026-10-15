//! The proposal chunker (`proposal_chunker.py`), without the model call.
//!
//! The SDK cuts a document into 1024-token windows, asks a chat model for a
//! [`ChunkDetails`] list per window (a short title, a one-sentence summary
//! and the window's propositions) and emits four chunks per entry: `title`,
//! `summary`, `propositions` and `document` (the whole window). This module
//! owns what is pure: the windows, the prompt, reading the model's answer and
//! the four chunks. The caller owns the model call, so it can bound, retry
//! and count it.
//!
//! Differences from the SDK, deliberate:
//!
//! * the SDK swallowed a failed model call and the document silently lost
//!   that window; here an unreadable answer is an error the caller reports
//!   ([`AnalysisError`]);
//! * the SDK asked for `with_structured_output`; this asks for a JSON object
//!   in the prompt and reads it tolerantly (a fenced or prefixed answer), so
//!   it works on every provider behind the gateway;
//! * a title, summary or proposal list that is blank is not emitted as a
//!   chunk, because a blank text cannot be embedded (the entry still takes
//!   its `chunk_id`, and its `document` chunk is always emitted).

use crate::tokens::{self, Encoding};
use crate::{Chunk, ChunkError};
use serde::Deserialize;

/// Tokens per window (`max_doc_tokens` in the SDK).
pub const PROPOSAL_WINDOW_TOKENS: usize = 1024;

/// The SDK's system prompt (`CHUNK_SUMMARY_PROMPT`), verbatim.
const CHUNK_SUMMARY_PROMPT: &str = "
You are the steward of a group of chunks, where each chunk represents a set of sentences discussing a similar topic.
Your task is to generate a structured List of JSON output containing both a brief chunk title and a set of one-sentence
summary that inform viewers about the chunk content, it also have a list of propositions from user message associated with chunk

Guidelines:
- chunk_title: A short, generalized phrase that broadly describes the topic.
- Example: If the chunk is about apples, generalize it to “Food.”
- If the chunk is about a specific month, generalize it to “Dates & Times.”
- chunk_summary: A concise, one-sentence explanation that clarifies the chunk content and may include guidance on what additional information should be added.
- Example: If given “Greg likes to eat pizza,” the summary should be “This chunk contains information about the types of food Greg likes to eat.
- propositions: is a list of decomposed clear and simple propositions, ensuring they are interpretable out of context.
- How to build propositions
-- Split compound sentence into simple sentences. Maintain the original phrasing from the input
whenever possible.
-- For any named entity that is accompanied by additional descriptive information, separate this
information into its own distinct proposition.
-- Decontextualize the proposition by adding necessary modifier to nouns or entire sentences
and replacing pronouns (e.g., \"it\", \"he\", \"she\", \"they\", \"this\", \"that\") with the full name of the
entities they refer to.
";

/// What the SDK's structured output enforced, said in words.
const JSON_INSTRUCTION: &str = "
Reply with one JSON object and nothing else, shaped exactly like this:
{\"chunks\": [{\"chunk_title\": \"...\", \"chunk_summary\": \"...\", \"propositions\": [\"...\", \"...\"]}]}
";

/// The system prompt of one window's model call.
#[must_use]
pub fn proposal_system_prompt() -> String {
    format!("{CHUNK_SUMMARY_PROMPT}{JSON_INSTRUCTION}")
}

/// The user message of one window's model call (the SDK's `Content: {split}`).
#[must_use]
pub fn proposal_user_message(window: &str) -> String {
    format!("Content: {window}")
}

/// `text` as the SDK cuts it for the proposal chunker: whole when it holds at
/// most `size` cl100k tokens, else windows of `size` tokens with no overlap.
/// Empty text gives no window.
///
/// # Errors
/// [`ChunkError::Tokenizer`] if the encoding is unavailable.
pub fn split_windows(text: &str, size: usize) -> Result<Vec<String>, ChunkError> {
    if text.is_empty() {
        return Ok(Vec::new());
    }
    let ids = tokens::encode(text, Encoding::Cl100k)?;
    if ids.len() <= size.max(1) {
        // Whole, byte for byte: a window decoded from tokens is not.
        return Ok(vec![text.to_owned()]);
    }
    tokens::split_ids_exact(text, &ids, Encoding::Cl100k, size)
}

/// One entry of the model's answer (the SDK's `ChunkDetails`).
#[derive(Debug, Clone, PartialEq, Eq, Deserialize)]
pub struct ChunkDetails {
    pub chunk_title: String,
    pub chunk_summary: String,
    pub propositions: Vec<String>,
}

#[derive(Deserialize)]
struct ChunkAnalysis {
    chunks: Vec<ChunkDetails>,
}

/// Why a model's answer could not be used.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum AnalysisError {
    /// No JSON object with a `chunks` list of the right shape.
    Unreadable,
    /// The object was right and its list empty: the window would vanish.
    NoChunks,
}

impl std::fmt::Display for AnalysisError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            Self::Unreadable => f.write_str("the model's answer is not the requested JSON object"),
            Self::NoChunks => f.write_str("the model's answer holds no chunks"),
        }
    }
}

impl std::error::Error for AnalysisError {}

/// The [`ChunkDetails`] of a model's reply. A reasoning model's
/// `<think>…</think>` blocks are dropped first; the reply may then be wrapped
/// in a code fence or a sentence. Every balanced JSON object in what is left
/// is tried (braces inside strings do not count) and the last one that reads
/// as the requested shape (a `chunks` list) is taken.
///
/// # Errors
/// [`AnalysisError`] when the reply is not the object or lists no chunk.
pub fn parse_chunk_analysis(reply: &str) -> Result<Vec<ChunkDetails>, AnalysisError> {
    let visible = strip_think_blocks(reply);
    let mut found: Option<Vec<ChunkDetails>> = None;
    for object in balanced_objects(&visible) {
        if let Ok(analysis) = serde_json::from_str::<ChunkAnalysis>(object) {
            found = Some(analysis.chunks);
        }
    }
    match found {
        None => Err(AnalysisError::Unreadable),
        Some(chunks) if chunks.is_empty() => Err(AnalysisError::NoChunks),
        Some(chunks) => Ok(chunks),
    }
}

/// `text` without the reasoning a model wrote into it, by the ONE rule every
/// caller uses ([`elitea_llm_wire::reasoning::strip_reasoning`]).
fn strip_think_blocks(text: &str) -> String {
    elitea_llm_wire::reasoning::strip_reasoning(text).text
}

/// The top-level balanced `{…}` slices of `text`, in order. Braces inside a
/// JSON string (and escaped quotes in it) do not count.
fn balanced_objects(text: &str) -> Vec<&str> {
    let mut out = Vec::new();
    let mut depth = 0_usize;
    let mut start = 0_usize;
    let (mut in_string, mut escaped) = (false, false);
    for (at, c) in text.char_indices() {
        if in_string {
            match c {
                _ if escaped => escaped = false,
                '\\' => escaped = true,
                '"' => in_string = false,
                _ => {}
            }
            continue;
        }
        match c {
            '"' if depth > 0 => in_string = true,
            '{' => {
                if depth == 0 {
                    start = at;
                }
                depth += 1;
            }
            '}' if depth > 0 => {
                depth -= 1;
                if depth == 0 {
                    out.push(&text[start..=at]);
                }
            }
            _ => {}
        }
    }
    out
}

/// The chunks of one window: per entry a `title`, a `summary`, a
/// `propositions` and a `document` chunk, in that order, all with the entry's
/// `chunk_id` (the SDK numbers entries, not chunks; `elitea-vector` keeps the
/// four apart by chunk type, and `extended_search` joins on the shared id).
/// `first_chunk_id` is the id of the first entry (1 for a document's first
/// window; the caller continues the count across windows with
/// `first_chunk_id + details.len()`). The `document` chunk holds the whole
/// window and carries `chunk_title` and `chunk_summary` in its metadata.
#[must_use]
pub fn proposal_chunks(
    details: &[ChunkDetails],
    window: &str,
    first_chunk_id: usize,
) -> Vec<Chunk> {
    let mut out = Vec::with_capacity(details.len() * 4);
    for (offset, entry) in details.iter().enumerate() {
        let id = first_chunk_id + offset;
        let propositions = entry.propositions.join("\n");
        for (kind, text) in [
            ("title", entry.chunk_title.as_str()),
            ("summary", entry.chunk_summary.as_str()),
            ("propositions", propositions.as_str()),
        ] {
            if !text.trim().is_empty() {
                out.push(Chunk::new(text.to_owned(), id, kind, "proposal"));
            }
        }
        out.push(
            Chunk::new(window.to_owned(), id, "document", "proposal")
                .with("chunk_title", &entry.chunk_title)
                .with("chunk_summary", &entry.chunk_summary),
        );
    }
    out
}

#[cfg(test)]
mod tests {
    use super::*;

    fn entry(title: &str, summary: &str, propositions: &[&str]) -> ChunkDetails {
        ChunkDetails {
            chunk_title: title.to_owned(),
            chunk_summary: summary.to_owned(),
            propositions: propositions.iter().map(|p| (*p).to_owned()).collect(),
        }
    }

    #[test]
    fn a_short_text_is_one_window_unchanged_and_a_long_one_is_cut_by_tokens() {
        assert!(split_windows("", 8).unwrap().is_empty());
        let short = "héllo wörld";
        assert_eq!(split_windows(short, 64).unwrap(), [short]);
        let long = "alpha beta gamma delta ".repeat(50);
        let windows = split_windows(&long, 64).unwrap();
        assert!(windows.len() > 1);
        assert_eq!(windows.concat(), long, "no overlap and no loss");
        for window in &windows {
            assert!(tokens::count(window).unwrap() <= 64);
        }
    }

    #[test]
    fn the_reply_may_be_fenced_or_introduced() {
        let json =
            r#"{"chunks":[{"chunk_title":"T","chunk_summary":"S","propositions":["a","b"]}]}"#;
        for reply in [
            json.to_owned(),
            format!("```json\n{json}\n```"),
            format!("Here you go: {json} Hope it helps."),
        ] {
            let parsed = parse_chunk_analysis(&reply).unwrap();
            assert_eq!(parsed, [entry("T", "S", &["a", "b"])]);
        }
    }

    #[test]
    fn a_reasoning_models_think_text_with_braces_is_skipped() {
        let json = r#"{"chunks":[{"chunk_title":"T {x}","chunk_summary":"S \"}\"","propositions":["a"]}]}"#;
        let reply = format!(
            "<think>The shape is {{\"chunks\": [...]}} and maybe {{ broken</think>\n```json\n{json}\n```\nDone {{ok}}."
        );
        let parsed = parse_chunk_analysis(&reply).unwrap();
        assert_eq!(parsed, [entry("T {x}", "S \"}\"", &["a"])]);
        // The last parseable object wins; an earlier draft is ignored.
        let two = format!(
            "{{\"chunks\":[{{\"chunk_title\":\"old\",\"chunk_summary\":\"s\",\"propositions\":[]}}]}} then {json}"
        );
        assert_eq!(parse_chunk_analysis(&two).unwrap()[0].chunk_title, "T {x}");
        assert_eq!(
            parse_chunk_analysis("<think>{\"chunks\":[]} never closed"),
            Err(AnalysisError::Unreadable)
        );
    }

    /// The shared reasoning rule: Qwen's bare closing tag ends the reasoning,
    /// so a draft object in it is never read as the answer.
    #[test]
    fn a_qwen_bare_closing_tag_hides_the_reasoning_draft() {
        let reply = r#"Draft: {"chunks":[{"chunk_title":"draft","chunk_summary":"s","propositions":[]}]}</think>

{"chunks":[{"chunk_title":"T","chunk_summary":"S","propositions":["a"]}]}"#;
        // Before the shared rule the draft (the last-but-one object) was
        // visible; now only the answer is.
        assert_eq!(strip_think_blocks(reply).matches("chunk_title").count(), 1);
        assert_eq!(parse_chunk_analysis(reply).unwrap()[0].chunk_title, "T");
        // A literal tag that does not lead the reply is kept.
        assert_eq!(
            strip_think_blocks("x <think>a</think> y"),
            "x <think>a</think> y"
        );
    }

    #[test]
    fn a_proposal_window_never_cuts_a_character() {
        let text = "日本語の文章😀😀😀 ".repeat(200);
        let windows = split_windows(&text, 16).unwrap();
        assert!(windows.len() > 2);
        assert_eq!(windows.concat(), text);
        assert!(windows.iter().all(|w| !w.contains('\u{FFFD}')));
    }

    #[test]
    fn an_unusable_reply_is_an_error_not_an_empty_window() {
        for reply in [
            "",
            "no json",
            "{\"chunks\": 3}",
            "{\"chunks\":[{\"chunk_title\":\"T\"}]}",
        ] {
            assert_eq!(
                parse_chunk_analysis(reply),
                Err(AnalysisError::Unreadable),
                "{reply}"
            );
        }
        assert_eq!(
            parse_chunk_analysis("{\"chunks\":[]}"),
            Err(AnalysisError::NoChunks)
        );
    }

    #[test]
    fn four_chunks_per_entry_in_the_sdk_order_with_the_entrys_id() {
        let details = [
            entry(
                "Food",
                "About food.",
                &["Greg likes pizza.", "Pizza is food."],
            ),
            entry("Dates", "About dates.", &["It is May."]),
        ];
        let chunks = proposal_chunks(&details, "WINDOW", 3);
        let shape: Vec<(usize, &str)> = chunks.iter().map(|c| (c.chunk_id, c.chunk_type)).collect();
        assert_eq!(
            shape,
            [
                (3, "title"),
                (3, "summary"),
                (3, "propositions"),
                (3, "document"),
                (4, "title"),
                (4, "summary"),
                (4, "propositions"),
                (4, "document"),
            ]
        );
        assert_eq!(chunks[2].text, "Greg likes pizza.\nPizza is food.");
        assert_eq!(chunks[3].text, "WINDOW");
        assert_eq!(chunks[3].metadata["chunk_title"], "Food");
        assert_eq!(chunks[3].metadata["chunk_summary"], "About food.");
        assert_eq!(chunks[0].metadata["chunk_type"], "title");
        assert!(!chunks[0].metadata.contains_key("chunk_title"));
    }

    #[test]
    fn a_blank_title_summary_or_proposal_list_is_not_a_chunk() {
        let chunks = proposal_chunks(&[entry(" ", "S", &[])], "W", 1);
        let types: Vec<&str> = chunks.iter().map(|c| c.chunk_type).collect();
        assert_eq!(types, ["summary", "document"]);
    }
}

//! Grounding indicators (ADR-0043 decision 5, #1500).
//!
//! The platform never attaches a numeric confidence to an answer. For an
//! agent that is configured to cite (`meta.citations` is `required`), it
//! reports two facts about the final answer instead:
//!
//! * how many of the run's sources the answer cites;
//! * which claim spans of the answer have no citation.
//!
//! **Sources.** A source is one tool result that the model saw in the request
//! that produced the answer (toolkit, MCP, retrieval and nested agent results
//! alike). The worker numbers them 1, 2, 3 in request order and writes the
//! number in front of the source name of the untrusted-content envelope
//! ([`numbered_source`], for example `<untrusted source="3:search_index">`).
//! The citation rule ([`CITATION_RULE`]) asks the model to cite a source with
//! its number in square brackets, as in `[3]`.
//!
//! **Claim spans.** [`indicator`] splits the answer into lines, and each line
//! into sentences. A sentence ends at `.`, `!`, `?`, `。`, `！` or `？`
//! followed by white space or the end of the line. Citation markers directly
//! after the end of a sentence belong to that sentence (`It is red. [1]`). A
//! span is a claim span when it has a letter or a digit outside its markers,
//! and it is not:
//!
//! * in a fenced code block;
//! * a heading (`#`) or a table row (`|`);
//! * a question (it ends with `?` or `？`);
//! * a lead-in (it ends with `:`).
//!
//! A list marker (`-`, `*`, `+`, `1.`, `1)`) at the start of a line is not
//! part of the span.
//!
//! **Markers.** A marker is `[n]` or `[n, m, ...]` with decimal numbers, not
//! followed by `(` (that is a Markdown link). A number from 1 to the number of
//! sources cites that source. Any other number is an invalid marker: it is
//! counted, and it does not make the span cited.
//!
//! The computation reads the answer and the source count only. It never
//! calls a model. A cited source does not prove that the source supports the
//! claim.

use serde_json::{Value, json};

/// The agent version meta key.
pub const META_KEY: &str = "citations";
/// The response metadata and message meta key of the indicator.
pub const METADATA_KEY: &str = "grounding";
/// The indicator's shape version.
pub const INDICATOR_VERSION: u32 = 1;
/// The most uncited span positions an indicator carries. The count is
/// always the full count.
pub const MAX_UNCITED_SPANS: usize = 64;
/// The longest source name after the number, in bytes. The envelope keeps
/// 128 bytes of a source name, so the number is never cut.
const MAX_NUMBERED_NAME_BYTES: usize = 112;
/// The most digits a marker number can have.
const MAX_MARKER_DIGITS: usize = 6;

/// The instruction text that asks the model to cite. The worker adds it
/// after the agent's instructions when citations are required.
pub const CITATION_RULE: &str = "Citation rule: Each <untrusted> block has a source that starts with a number, as in source=\"3:search\". When a sentence uses facts from a block, end the sentence with that number in square brackets, as in [3]. Use [2, 5] for more than one block. Do not cite a number that no block has.";

/// The citation setting of an agent version (`meta.citations`).
#[derive(Clone, Copy, Debug, Default, Eq, PartialEq)]
pub enum CitationMode {
    /// No citation rule and no indicator. Absent or `null` is off.
    #[default]
    Off,
    /// The citation rule is in the instructions, and the final answer gets
    /// an indicator when the model saw at least one source.
    Required,
}

/// A `meta.citations` value that is not `off`, `required` or `null`.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct InvalidCitationMode;

impl std::fmt::Display for InvalidCitationMode {
    fn fmt(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        formatter.write_str("meta.citations must be \"off\" or \"required\"")
    }
}

impl std::error::Error for InvalidCitationMode {}

impl CitationMode {
    /// Read `meta.citations`. Absent or `null` is [`CitationMode::Off`].
    ///
    /// # Errors
    ///
    /// [`InvalidCitationMode`] for any other value than `"off"` or
    /// `"required"`.
    pub fn from_value(value: Option<&Value>) -> Result<Self, InvalidCitationMode> {
        match value {
            None | Some(Value::Null) => Ok(Self::Off),
            Some(Value::String(mode)) if mode == "off" => Ok(Self::Off),
            Some(Value::String(mode)) if mode == "required" => Ok(Self::Required),
            Some(_) => Err(InvalidCitationMode),
        }
    }

    /// Whether the agent must cite.
    #[must_use]
    pub const fn is_required(self) -> bool {
        matches!(self, Self::Required)
    }
}

/// The agent's instructions with the citation rule after them.
#[must_use]
pub fn with_citation_rule(instructions: &str) -> String {
    if instructions.trim().is_empty() {
        CITATION_RULE.to_owned()
    } else {
        format!("{instructions}\n\n{CITATION_RULE}")
    }
}

/// The envelope source name of source `ordinal` (1-based): the number, a
/// colon and the tool name, cut to a short length at a character boundary.
#[must_use]
pub fn numbered_source(ordinal: usize, name: &str) -> String {
    let mut end = name.len().min(MAX_NUMBERED_NAME_BYTES);
    while !name.is_char_boundary(end) {
        end -= 1;
    }
    format!("{ordinal}:{}", &name[..end])
}

/// One uncited claim span: UTF-16 code unit offsets into the answer, `start`
/// inclusive and `end` exclusive. UTF-16 is the unit of the web and mobile
/// clients, which slice the stored answer with them.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct Span {
    pub start: usize,
    pub end: usize,
}

/// The grounding indicator of one final answer.
#[derive(Clone, Debug, Default, Eq, PartialEq)]
pub struct Indicator {
    /// The sources the model saw.
    pub sources_available: usize,
    /// The distinct sources that the answer cites with a valid marker.
    pub sources_cited: usize,
    /// The claim spans of the answer.
    pub claim_spans: usize,
    /// The claim spans without a valid marker.
    pub uncited_spans: usize,
    /// The first [`MAX_UNCITED_SPANS`] uncited spans, in answer order.
    pub uncited: Vec<Span>,
    /// Marker numbers that name no source (0, or above the source count).
    pub invalid_markers: usize,
}

impl Indicator {
    /// The JSON form that the worker sends as `response_metadata.grounding`
    /// and Main stores as `meta.grounding`.
    #[must_use]
    pub fn to_value(&self) -> Value {
        json!({
            "version": INDICATOR_VERSION,
            "sources_available": self.sources_available,
            "sources_cited": self.sources_cited,
            "claim_spans": self.claim_spans,
            "uncited_spans": self.uncited_spans,
            "uncited": self.uncited.iter()
                .map(|span| json!({"start": span.start, "end": span.end}))
                .collect::<Vec<_>>(),
            "invalid_markers": self.invalid_markers,
        })
    }
}

/// The indicator of `answer` for a request with `sources_available` numbered
/// sources. `None` when there is no source: the indicator is absent, not
/// zero.
#[must_use]
pub fn indicator(answer: &str, sources_available: usize) -> Option<Indicator> {
    if sources_available == 0 {
        return None;
    }
    let mut cited = std::collections::BTreeSet::new();
    let mut result = Indicator {
        sources_available,
        ..Indicator::default()
    };
    let mut utf16 = Utf16Cursor::new(answer);
    for span in claim_candidates(answer) {
        let text = &answer[span.0..span.1];
        let markers = markers_in(text);
        let mut span_cited = false;
        for number in markers.numbers {
            if (1..=sources_available).contains(&number) {
                cited.insert(number);
                span_cited = true;
            } else {
                result.invalid_markers += 1;
            }
        }
        if !is_claim(text, &markers.ranges) {
            continue;
        }
        result.claim_spans += 1;
        if span_cited {
            continue;
        }
        result.uncited_spans += 1;
        if result.uncited.len() < MAX_UNCITED_SPANS {
            result.uncited.push(Span {
                start: utf16.offset(span.0),
                end: utf16.offset(span.1),
            });
        }
    }
    result.sources_cited = cited.len();
    Some(result)
}

/// Converts increasing byte offsets to UTF-16 offsets in one pass.
struct Utf16Cursor<'a> {
    text: &'a str,
    byte: usize,
    units: usize,
}

impl<'a> Utf16Cursor<'a> {
    const fn new(text: &'a str) -> Self {
        Self {
            text,
            byte: 0,
            units: 0,
        }
    }

    fn offset(&mut self, byte: usize) -> usize {
        if byte < self.byte {
            self.byte = 0;
            self.units = 0;
        }
        self.units += self.text[self.byte..byte].encode_utf16().count();
        self.byte = byte;
        self.units
    }
}

/// The trimmed byte ranges of every sentence outside code fences, headings
/// and table rows, list markers removed.
fn claim_candidates(answer: &str) -> Vec<(usize, usize)> {
    let mut spans = Vec::new();
    let mut in_fence = false;
    let mut line_start = 0;
    for line in answer.split_inclusive('\n') {
        let start = line_start;
        line_start += line.len();
        let body = line.trim_end_matches(['\n', '\r']);
        let trimmed = body.trim_start();
        if trimmed.starts_with("```") || trimmed.starts_with("~~~") {
            in_fence = !in_fence;
            continue;
        }
        if in_fence || trimmed.is_empty() || trimmed.starts_with('#') || trimmed.starts_with('|') {
            continue;
        }
        let indent = body.len() - trimmed.len();
        let content_start = start + indent + list_marker_len(trimmed);
        let content_end = start + body.len();
        sentences(answer, content_start, content_end, &mut spans);
    }
    spans
}

/// The byte length of a list marker and its space at the start of `line`.
fn list_marker_len(line: &str) -> usize {
    let bytes = line.as_bytes();
    if matches!(bytes.first(), Some(b'-' | b'*' | b'+')) && bytes.get(1) == Some(&b' ') {
        return 2;
    }
    let digits = bytes
        .iter()
        .take_while(|byte| byte.is_ascii_digit())
        .count();
    if (1..=9).contains(&digits)
        && matches!(bytes.get(digits), Some(b'.' | b')'))
        && bytes.get(digits + 1) == Some(&b' ')
    {
        return digits + 2;
    }
    0
}

/// Split `answer[start..end]` (one line) into trimmed sentence ranges.
fn sentences(answer: &str, start: usize, end: usize, spans: &mut Vec<(usize, usize)>) {
    let line = &answer[start..end];
    let mut sentence_start = 0;
    let mut chars = line.char_indices().peekable();
    while let Some((index, character)) = chars.next() {
        if !matches!(character, '.' | '!' | '?' | '。' | '！' | '？') {
            continue;
        }
        let after = index + character.len_utf8();
        let ends = match chars.peek() {
            None => true,
            Some((_, next)) => next.is_whitespace() || matches!(character, '。' | '！' | '？'),
        };
        if !ends {
            continue;
        }
        // Markers after the end belong to this sentence.
        let sentence_end = after + trailing_markers_len(&line[after..]);
        push_trimmed(answer, start + sentence_start, start + sentence_end, spans);
        sentence_start = sentence_end;
        while chars.peek().is_some_and(|(next, _)| *next < sentence_end) {
            chars.next();
        }
    }
    push_trimmed(answer, start + sentence_start, end, spans);
}

/// The byte length of white space and markers at the start of `rest`, when
/// at least one marker is there; else 0.
fn trailing_markers_len(rest: &str) -> usize {
    let mut consumed = 0;
    let mut found = 0;
    loop {
        let spaces = rest[consumed..].len() - rest[consumed..].trim_start().len();
        match marker_at(&rest[consumed + spaces..]) {
            Some((length, _)) => {
                consumed += spaces + length;
                found = consumed;
            }
            None => return found,
        }
    }
}

fn push_trimmed(answer: &str, start: usize, end: usize, spans: &mut Vec<(usize, usize)>) {
    if start >= end {
        return;
    }
    let text = &answer[start..end];
    let leading = text.len() - text.trim_start().len();
    let trimmed = text.trim();
    if !trimmed.is_empty() {
        spans.push((start + leading, start + leading + trimmed.len()));
    }
}

/// The markers of one span: their numbers and their byte ranges.
struct Markers {
    numbers: Vec<usize>,
    ranges: Vec<(usize, usize)>,
}

fn markers_in(text: &str) -> Markers {
    let mut markers = Markers {
        numbers: Vec::new(),
        ranges: Vec::new(),
    };
    for (index, _) in text.match_indices('[') {
        if let Some((length, numbers)) = marker_at(&text[index..]) {
            markers.numbers.extend(numbers);
            markers.ranges.push((index, index + length));
        }
    }
    markers
}

/// A marker at the start of `text`: its byte length and numbers.
fn marker_at(text: &str) -> Option<(usize, Vec<usize>)> {
    let inner = text.strip_prefix('[')?;
    let close = inner.find(']')?;
    let list = &inner[..close];
    let mut numbers = Vec::new();
    for item in list.split(',') {
        let item = item.trim();
        if item.is_empty()
            || item.len() > MAX_MARKER_DIGITS
            || !item.bytes().all(|byte| byte.is_ascii_digit())
        {
            return None;
        }
        numbers.push(item.parse().ok()?);
    }
    let length = close + 2;
    // `[1](https://...)` is a Markdown link, not a citation.
    if text[length..].starts_with('(') {
        return None;
    }
    Some((length, numbers))
}

/// Whether a candidate span is a claim span.
fn is_claim(text: &str, markers: &[(usize, usize)]) -> bool {
    let mut without_markers = String::with_capacity(text.len());
    let mut copied = 0;
    for &(start, end) in markers {
        without_markers.push_str(&text[copied..start]);
        copied = end;
    }
    without_markers.push_str(&text[copied..]);
    let core = without_markers.trim();
    if !core.chars().any(char::is_alphanumeric) {
        return false;
    }
    let last = core
        .trim_end_matches(|character: char| {
            matches!(character, '*' | '_' | '`' | '"' | '\'' | ')' | '”' | '’')
        })
        .chars()
        .last();
    !matches!(last, Some('?' | '？' | ':' | '：'))
}

#[cfg(test)]
#[path = "grounding_tests.rs"]
mod tests;

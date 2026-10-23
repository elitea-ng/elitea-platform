//! Documents by page: the range a caller asks for, the pages it gets back,
//! and the [`DocumentExtractor`] seam a host puts its own extractor behind
//! (the desktop's sandboxed sidecar, ADR-0029 decision 4).
//!
//! A page is whatever unit the format has: a PDF page, a slide, a sheet.
//! A format with none (a Word document, an e-mail, plain text) is one page.

use crate::{Extracted, extract, is_document_type};

/// An inclusive, 1-based page range: `5`, `1-20`, `30-` (page 30 on).
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct PageRange {
    pub from: u32,
    /// `None`: to the last page.
    pub to: Option<u32>,
}

impl PageRange {
    /// Every page.
    pub const ALL: Self = Self { from: 1, to: None };

    /// Parses `5`, `1-20` or `30-`.
    ///
    /// # Errors
    /// A sentence naming what is wrong with `text`.
    pub fn parse(text: &str) -> Result<Self, String> {
        let text = text.trim();
        let number = |part: &str| -> Result<u32, String> {
            match part.trim().parse::<u32>() {
                Ok(0) => Err("pages are numbered from 1".to_owned()),
                Ok(n) => Ok(n),
                Err(_) => Err(format!(
                    "`{text}` is not a page range (use `5`, `1-20` or `30-`)"
                )),
            }
        };
        let range = match text.split_once('-') {
            None => {
                let page = number(text)?;
                Self {
                    from: page,
                    to: Some(page),
                }
            }
            Some((from, to)) if to.trim().is_empty() => Self {
                from: number(from)?,
                to: None,
            },
            Some((from, to)) => Self {
                from: number(from)?,
                to: Some(number(to)?),
            },
        };
        if range.to.is_some_and(|to| to < range.from) {
            return Err(format!("`{text}` ends before it starts"));
        }
        Ok(range)
    }

    #[must_use]
    pub fn contains(&self, page: u32) -> bool {
        page >= self.from && self.to.is_none_or(|to| page <= to)
    }
}

impl std::fmt::Display for PageRange {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self.to {
            Some(to) if to == self.from => write!(f, "{to}"),
            Some(to) => write!(f, "{}-{to}", self.from),
            None => write!(f, "{}-", self.from),
        }
    }
}

/// One page's text.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Page {
    /// 1-based.
    pub number: u32,
    pub text: String,
}

/// The pages of a document that fall in a range, and how many it has.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct PagedText {
    /// Which extractor produced it (`utf8`, `xberg`).
    pub extractor: String,
    /// Pages the whole document has, whether or not they were returned.
    pub page_count: u32,
    /// The requested pages that exist, in order.
    pub pages: Vec<Page>,
}

impl PagedText {
    /// Keeps the pages in `range`, and stops after the page that takes the
    /// text past `max_bytes` (always at least one page when any is in
    /// range), so a caller's output stays bounded; the caller sees the cut
    /// from the last page number.
    #[must_use]
    pub fn select(mut self, range: &PageRange, max_bytes: usize) -> Self {
        let mut total = 0_usize;
        let mut kept = Vec::new();
        for page in self.pages.drain(..) {
            if !range.contains(page.number) {
                continue;
            }
            total = total.saturating_add(page.text.len());
            kept.push(page);
            if total >= max_bytes {
                break;
            }
        }
        self.pages = kept;
        self
    }

    /// Bounds the pages' text as a JSON answer carries it (escaped) to
    /// `max_escaped` bytes. The C0 control characters other than tab,
    /// newline and carriage return go first: they carry no text, and JSON
    /// escapes each as six bytes. Then pages are kept while their escaped
    /// text fits; a first page that alone does not is cut (and says so).
    #[must_use]
    pub fn fit_escaped(mut self, max_escaped: usize) -> Self {
        const CUT: &str = "\n[the rest of this page is too long to return]";
        let mut left = max_escaped;
        let mut kept = Vec::new();
        for mut page in self.pages.drain(..) {
            page.text
                .retain(|c| u32::from(c) >= 0x20 || matches!(c, '\t' | '\n' | '\r'));
            let size = escaped_len(&page.text);
            if size <= left {
                left -= size;
                kept.push(page);
                continue;
            }
            if kept.is_empty() {
                let room = left.saturating_sub(escaped_len(CUT));
                let mut used = 0;
                let end = page
                    .text
                    .char_indices()
                    .find(|&(_, c)| {
                        used += escaped_char_len(c);
                        used > room
                    })
                    .map_or(page.text.len(), |(at, _)| at);
                page.text.truncate(end);
                page.text.push_str(CUT);
                kept.push(page);
            }
            break;
        }
        self.pages = kept;
        self
    }
}

/// The bytes `c` takes inside a JSON string (`serde_json`'s escaping).
fn escaped_char_len(c: char) -> usize {
    match c {
        '"' | '\\' | '\u{8}' | '\u{c}' | '\n' | '\r' | '\t' => 2,
        c if u32::from(c) < 0x20 => 6,
        c => c.len_utf8(),
    }
}

/// The bytes `text` takes inside a JSON string.
fn escaped_len(text: &str) -> usize {
    text.chars().map(escaped_char_len).sum()
}

/// Why a document gave no pages.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum ExtractFailure {
    /// No extractor handles this media type.
    Unsupported,
    /// The bytes are not what the media type says, or hold no text.
    Unreadable(String),
    /// The extractor ran past its time limit.
    Timeout(String),
    /// The extractor itself failed (it could not start, it crashed).
    Failed(String),
    /// The host could not confine the extractor as it requires, so the
    /// document was not given to it.
    Unconfined(String),
}

impl std::fmt::Display for ExtractFailure {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            Self::Unsupported => f.write_str("unsupported document type"),
            Self::Unreadable(reason)
            | Self::Timeout(reason)
            | Self::Failed(reason)
            | Self::Unconfined(reason) => f.write_str(reason),
        }
    }
}

impl std::error::Error for ExtractFailure {}

/// Turns a document into text by page. The default is [`InProcessExtractor`];
/// a host that must not link the extractor (or must confine it) supplies its
/// own.
pub trait DocumentExtractor: Send + Sync {
    /// The pages of `bytes` (a document of media type `mime`) that fall in
    /// `range`. Blocking: call it off an async runtime.
    ///
    /// # Errors
    /// [`ExtractFailure`], one variant per reason.
    fn extract(
        &self,
        mime: &str,
        bytes: Vec<u8>,
        range: &PageRange,
    ) -> Result<PagedText, ExtractFailure>;
}

/// [`extract`] in this process: text and (with the `documents` feature) PDF
/// and Office, page by page; without it, only text, as one page.
#[derive(Debug, Clone, Copy, Default)]
pub struct InProcessExtractor;

impl DocumentExtractor for InProcessExtractor {
    fn extract(
        &self,
        mime: &str,
        bytes: Vec<u8>,
        range: &PageRange,
    ) -> Result<PagedText, ExtractFailure> {
        #[cfg(feature = "documents")]
        if is_document_type(mime) {
            return paged_document(mime, bytes, range);
        }
        let _ = is_document_type;
        match extract(mime, &bytes) {
            Extracted::Text { text, extractor } => {
                let pages = if range.contains(1) {
                    vec![Page { number: 1, text }]
                } else {
                    Vec::new()
                };
                Ok(PagedText {
                    extractor: extractor.to_owned(),
                    page_count: 1,
                    pages,
                })
            }
            Extracted::Unsupported => Err(ExtractFailure::Unsupported),
            Extracted::Unreadable(reason) => Err(ExtractFailure::Unreadable(reason)),
        }
    }
}

#[cfg(feature = "documents")]
fn paged_document(
    mime: &str,
    bytes: Vec<u8>,
    range: &PageRange,
) -> Result<PagedText, ExtractFailure> {
    use crate::{DocumentError, DocumentLimits, extract_document};
    let limits = DocumentLimits {
        pages: true,
        ..DocumentLimits::default()
    };
    let document = extract_document(mime, bytes, &limits).map_err(|error| match error {
        DocumentError::Unsupported(_) => ExtractFailure::Unsupported,
        DocumentError::Timeout(_) => ExtractFailure::Timeout(error.to_string()),
        DocumentError::Extractor(_) => ExtractFailure::Failed(error.to_string()),
        other => ExtractFailure::Unreadable(other.to_string()),
    })?;
    let pages: Vec<Page> = if document.pages.is_empty() {
        // A format with no pages: the whole is page 1.
        vec![Page {
            number: 1,
            text: document.text,
        }]
    } else {
        document
            .pages
            .into_iter()
            .map(|page| Page {
                number: page.number,
                text: page.text,
            })
            .collect()
    };
    let page_count = pages.iter().map(|page| page.number).max().unwrap_or(1);
    Ok(PagedText {
        extractor: document.extractor.to_owned(),
        page_count,
        pages: pages
            .into_iter()
            .filter(|page| range.contains(page.number))
            .collect(),
    })
}

#[cfg(test)]
mod tests {
    use super::*;

    fn paged(texts: &[&str]) -> PagedText {
        PagedText {
            extractor: "utf8".to_owned(),
            page_count: u32::try_from(texts.len()).unwrap_or(u32::MAX),
            pages: (1..)
                .zip(texts)
                .map(|(number, text)| Page {
                    number,
                    text: (*text).to_owned(),
                })
                .collect(),
        }
    }

    #[test]
    fn escaped_text_fits_its_budget_and_control_characters_go() {
        // Control characters (six bytes each, escaped) carry no text.
        let control = "a\u{1}b\u{1f}\tc\n".repeat(10);
        let fitted = paged(&[&control]).fit_escaped(1000);
        assert_eq!(fitted.pages[0].text, "ab\tc\n".repeat(10));
        // A page past what is left is not returned; the caller goes on from it.
        let fitted = paged(&["x".repeat(60).as_str(), "y".repeat(60).as_str()]).fit_escaped(100);
        assert_eq!(fitted.pages.len(), 1);
        // A first page that alone does not fit is cut, within the budget.
        let quotes = "\"".repeat(1000);
        let fitted = paged(&[&quotes]).fit_escaped(200);
        let text = &fitted.pages[0].text;
        assert!(escaped_len(text) <= 200, "{}", escaped_len(text));
        assert!(text.starts_with(&"\"".repeat(70)));
        assert!(fitted.pages[0].text.ends_with("too long to return]"));
    }

    #[test]
    fn page_ranges_parse_and_print() {
        assert_eq!(
            PageRange::parse("5").expect("one"),
            PageRange {
                from: 5,
                to: Some(5)
            }
        );
        assert_eq!(
            PageRange::parse(" 1-20 ").expect("range").to_string(),
            "1-20"
        );
        assert_eq!(PageRange::parse("30-").expect("open").to_string(), "30-");
        for bad in ["", "0", "0-3", "3-1", "a", "1-b", "-4", "1-2-3"] {
            assert!(PageRange::parse(bad).is_err(), "{bad}");
        }
    }

    #[test]
    fn a_selection_keeps_the_range_and_stops_past_the_budget() {
        let page = |number, text: &str| Page {
            number,
            text: text.to_owned(),
        };
        let all = PagedText {
            extractor: "t".to_owned(),
            page_count: 4,
            pages: vec![
                page(1, "aaaa"),
                page(2, "bbbb"),
                page(3, "cccc"),
                page(4, "d"),
            ],
        };
        let picked = all
            .clone()
            .select(&PageRange::parse("2-").expect("range"), 5);
        assert_eq!(
            picked.pages.iter().map(|p| p.number).collect::<Vec<_>>(),
            [2, 3]
        );
        assert_eq!(picked.page_count, 4);
        let one = all.select(&PageRange::ALL, 1);
        assert_eq!(one.pages.len(), 1, "at least one page, whatever the budget");
    }

    #[test]
    fn text_is_one_page() {
        let text = InProcessExtractor
            .extract("text/plain", b"hello".to_vec(), &PageRange::ALL)
            .expect("text");
        assert_eq!((text.page_count, text.pages.len()), (1, 1));
        let none = InProcessExtractor
            .extract(
                "text/plain",
                b"hello".to_vec(),
                &PageRange::parse("2").expect("r"),
            )
            .expect("text");
        assert_eq!(none.pages.len(), 0);
    }
}

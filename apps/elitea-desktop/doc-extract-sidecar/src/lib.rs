//! The desktop app's document extractor, as a library so the tests can drive
//! it without a process. The protocol is `elitea_doc_extract::wire`: a
//! request on stdin (a media type, a page range and the file's bytes), one
//! JSON object on stdout.
//!
//! The sidecar never opens a path: the host reads the file through its
//! confined workspace and sends the bytes, then runs this process read-only
//! with the network off. It is one request per process, so a crashed or
//! timed-out parse costs the host one process and nothing else.

use std::io::Read;

use elitea_doc_extract::wire::{self, HEADER_LEN};
use elitea_doc_extract::{DocumentExtractor, ExtractFailure, InProcessExtractor, PageRange};
use serde::Serialize;

/// The largest request: the host's own file cap (64 MiB) and the header.
pub const MAX_REQUEST_BYTES: u64 = 64 * 1024 * 1024 + 1024;
/// The most page text one answer carries. A page range that holds more is
/// cut after the page that crosses it, and the host asks for the rest from
/// the next page.
pub const MAX_ANSWER_TEXT_BYTES: usize = 3 * 1024 * 1024;
/// The most bytes the pages' text takes in the answer once JSON-escaped:
/// the host reads at most 8 MiB of stdout, and the envelope (page numbers,
/// keys) takes far less than the rest. Measured, not assumed: a control
/// character escapes to six bytes.
pub const MAX_ANSWER_ESCAPED_BYTES: usize = 7 * 1024 * 1024;

/// One page of the answer.
#[derive(Serialize)]
pub struct PageOut {
    pub n: u32,
    pub text: String,
}

/// The JSON object on stdout.
#[derive(Serialize)]
#[serde(untagged)]
pub enum Answer {
    Ok {
        ok: bool,
        extractor: String,
        page_count: u32,
        pages: Vec<PageOut>,
    },
    Failed {
        ok: bool,
        error: &'static str,
        message: String,
    },
}

impl Answer {
    fn failed(error: &'static str, message: impl Into<String>) -> Self {
        Self::Failed {
            ok: false,
            error,
            message: message.into(),
        }
    }

    /// The JSON line.
    ///
    /// # Panics
    /// Never: the answer holds only strings and numbers.
    #[must_use]
    pub fn to_json(&self) -> String {
        serde_json::to_string(self).unwrap_or_else(|_| {
            r#"{"ok":false,"error":"unreadable","message":"no answer"}"#.to_owned()
        })
    }
}

/// Reads one request from `input` and answers it.
pub fn handle(input: impl Read) -> Answer {
    let mut input = input.take(MAX_REQUEST_BYTES + 1);
    let mut head = [0_u8; HEADER_LEN];
    if input.read_exact(&mut head).is_err() {
        return Answer::failed("unreadable", "the request is shorter than its header");
    }
    let header = match wire::decode_header(&head) {
        Ok(header) => header,
        Err(reason) => return Answer::failed("unreadable", reason),
    };
    let mut mime = vec![0_u8; header.mime_len];
    if input.read_exact(&mut mime).is_err() {
        return Answer::failed("unreadable", "the request ends inside the media type");
    }
    let Ok(mime) = String::from_utf8(mime) else {
        return Answer::failed("unreadable", "the media type is not text");
    };
    let mut bytes = Vec::new();
    if input.read_to_end(&mut bytes).is_err() {
        return Answer::failed("unreadable", "the request could not be read");
    }
    if bytes.len() as u64 > MAX_REQUEST_BYTES {
        return Answer::failed("unreadable", "the document is larger than the limit");
    }
    answer(&mime, bytes, &header.range)
}

/// The pages of `bytes` in `range`, or why there are none.
#[must_use]
pub fn answer(mime: &str, bytes: Vec<u8>, range: &PageRange) -> Answer {
    match InProcessExtractor.extract(mime, bytes, range) {
        Ok(text) => {
            let text = text
                .select(range, MAX_ANSWER_TEXT_BYTES)
                .fit_escaped(MAX_ANSWER_ESCAPED_BYTES);
            Answer::Ok {
                ok: true,
                extractor: text.extractor,
                page_count: text.page_count,
                pages: text
                    .pages
                    .into_iter()
                    .map(|page| PageOut {
                        n: page.number,
                        text: page.text,
                    })
                    .collect(),
            }
        }
        Err(ExtractFailure::Unsupported) => {
            Answer::failed("unsupported", format!("no extractor for {mime}"))
        }
        Err(ExtractFailure::Timeout(reason)) => Answer::failed("timeout", reason),
        Err(
            ExtractFailure::Unreadable(reason)
            | ExtractFailure::Failed(reason)
            | ExtractFailure::Unconfined(reason),
        ) => Answer::failed("unreadable", reason),
    }
}

/// Caps the process's address space where the OS enforces it (Linux): a
/// runaway parse dies instead of taking the machine's memory. macOS does not
/// enforce `RLIMIT_AS`/`RLIMIT_DATA` for this, so there the host's timeout is
/// the bound.
pub fn limit_memory() {
    #[cfg(target_os = "linux")]
    {
        use rustix::process::{Resource, Rlimit, setrlimit};
        // 512 MiB of resident memory is the target; address space also
        // counts the parser's 16 MiB stack, mapped libraries and allocator
        // arenas, so the cap sits well above it.
        let cap = 4 * 1024 * 1024 * 1024;
        let _ = setrlimit(
            Resource::As,
            Rlimit {
                current: Some(cap),
                maximum: Some(cap),
            },
        );
    }
}

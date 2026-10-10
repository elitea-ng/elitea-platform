//! A document's bytes as text (ADR-0028 D4).
//!
//! * A text media type is decoded as UTF-8 first. Bytes that are not UTF-8
//!   (and are not binary: no NUL byte) are decoded with the encoding
//!   `chardetng` guesses, through `encoding_rs`, as the SDK's text loaders'
//!   `autodetect_encoding` does; a decode that needed replacement characters
//!   is refused rather than indexed damaged (ADR-0030 decision 4).
//! * HTML (`text/html`) is text like any other and is returned as written
//!   (decoded), so a code-search index cites its lines. Indexing for
//!   retrieval asks for more with [`ExtractOptions::html_to_markdown`]:
//!   `text/html` and `application/xhtml+xml` are then converted to markdown
//!   with `html-to-markdown-rs`, as the SDK's HTML loader does, so headings
//!   and lists survive for the markdown chunker. The conversion runs on its
//!   own thread (large stack, timeout, input cap): it recurses with the
//!   page's nesting. This needs the `html` feature (off by default); without
//!   it, asking for the conversion gives [`Extracted::Unsupported`].
//! * Any other format is extracted by a document extractor when this crate
//!   is built with the `documents` feature, and reported unsupported
//!   otherwise — never guessed at.
//!
//! Code is text here; its structure is the code parsers' job, not this
//! crate's.

use elitea_content_source::is_text;
#[cfg(feature = "html")]
use std::sync::{Arc, Condvar, Mutex, mpsc};
#[cfg(feature = "html")]
use std::time::{Duration, Instant};

/// Seconds one document may take (the extractors' and the HTML
/// conversion's).
#[cfg(any(feature = "html", feature = "documents"))]
const TIMEOUT_SECONDS: u64 = 120;
/// The stack of a thread that runs an extractor: they recurse deeper than a
/// runtime worker's default stack allows.
#[cfg(any(feature = "html", feature = "documents"))]
const STACK_BYTES: usize = 16 * 1024 * 1024;
/// The largest HTML page converted; a larger one is unreadable, not
/// converted.
#[cfg(feature = "html")]
const MAX_HTML_BYTES: usize = 10 * 1024 * 1024;
/// Conversion threads that may exist at once, process-wide. A thread that
/// timed out still counts until it finishes.
#[cfg(feature = "html")]
const MAX_CONVERSION_THREADS: usize = 4;
/// A page this small converts on the caller's thread: it cannot nest deeply
/// enough to need the large stack, and it finishes at once.
#[cfg(feature = "html")]
const INLINE_HTML_BYTES: usize = 64 * 1024;

/// How extraction treats the formats that can be read more than one way.
#[derive(Debug, Clone, Copy, Default, PartialEq, Eq)]
pub struct ExtractOptions {
    /// Convert HTML to markdown. Off (the default), `text/html` is returned
    /// as the text it is, and `application/xhtml+xml` is unsupported.
    pub html_to_markdown: bool,
}

/// What extraction gave.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Extracted {
    /// The document's text.
    Text {
        text: String,
        /// Which extractor produced it (`utf8`, `xberg`).
        extractor: &'static str,
    },
    /// No extractor handles this media type in this build.
    Unsupported,
    /// The bytes are not what the media type says (not UTF-8, a corrupt
    /// PDF, …).
    Unreadable(String),
}

/// Bytes that are not readable as text, and why.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Unreadable(pub String);

impl std::fmt::Display for Unreadable {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str(&self.0)
    }
}

impl std::error::Error for Unreadable {}

/// `bytes` as text by the rules extraction reads a text file with: a
/// byte-order mark, else UTF-8, else (refusing UTF-8 with damage) the
/// legacy encoding `chardetng` guesses, and a result that is mostly control
/// or replacement characters is binary and refused. Lets a reader show a
/// file exactly as ingestion indexed it.
///
/// # Errors
/// [`Unreadable`] when the bytes are not text.
pub fn decode_text(bytes: &[u8]) -> Result<String, Unreadable> {
    decode(bytes).map(|(text, _)| text).map_err(Unreadable)
}

/// Media types the `documents` feature extracts.
const DOCUMENT_TYPES: &[&str] = &[
    "application/pdf",
    "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
    "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
    "application/vnd.openxmlformats-officedocument.presentationml.presentation",
    "application/msword",
    "application/vnd.ms-excel",
    "application/vnd.ms-powerpoint",
    "application/vnd.oasis.opendocument.text",
    "application/rtf",
    "application/epub+zip",
    "message/rfc822",
    "application/vnd.ms-outlook",
];

/// Media types converted from HTML to markdown, when asked.
fn is_html(mime: &str) -> bool {
    matches!(mime, "text/html" | "application/xhtml+xml")
}

/// Whether this build turns `mime` into text with the default options.
#[must_use]
pub fn can_extract(mime: &str) -> bool {
    can_extract_with(mime, &ExtractOptions::default())
}

/// Whether this build turns `mime` into text with `options`.
#[must_use]
pub fn can_extract_with(mime: &str, options: &ExtractOptions) -> bool {
    if options.html_to_markdown && is_html(mime) {
        return cfg!(feature = "html");
    }
    is_text(mime) || (cfg!(feature = "documents") && DOCUMENT_TYPES.contains(&mime))
}

/// `bytes` of media type `mime` as text, with the default options: HTML is
/// text as written.
#[must_use]
pub fn extract(mime: &str, bytes: &[u8]) -> Extracted {
    extract_with(mime, bytes, &ExtractOptions::default())
}

/// `bytes` of media type `mime` as text.
#[must_use]
pub fn extract_with(mime: &str, bytes: &[u8], options: &ExtractOptions) -> Extracted {
    if options.html_to_markdown && is_html(mime) {
        #[cfg(feature = "html")]
        return html(bytes);
        #[cfg(not(feature = "html"))]
        return Extracted::Unsupported;
    }
    if is_text(mime) {
        return match decode(bytes) {
            Ok((text, extractor)) => Extracted::Text { text, extractor },
            Err(reason) => Extracted::Unreadable(reason),
        };
    }
    if !can_extract_with(mime, options) {
        return Extracted::Unsupported;
    }
    documents::extract(mime, bytes)
}

/// Whether `bytes`, which are not UTF-8 as a whole, are UTF-8 with damage (a
/// stray byte, a cut sequence) rather than a legacy encoding.
///
/// Damaged UTF-8 is made of well-formed multi-byte sequences with a few
/// bad bytes among them; a legacy text has none, or only the odd accidental
/// one (`Shift_JIS` pairs can look like a UTF-8 sequence: in a sample, 22 of
/// 50 high bytes did). So the text is damaged UTF-8 when it holds a
/// well-formed sequence and at least four of every five of its non-ASCII
/// bytes lie inside well-formed ones. This is deliberately not "any
/// well-formed sequence at all", which would refuse real `Shift_JIS` files.
fn is_damaged_utf8(bytes: &[u8]) -> bool {
    let (mut well_formed, mut stray) = (0_usize, 0_usize);
    let mut rest = bytes;
    loop {
        match std::str::from_utf8(rest) {
            Ok(text) => {
                well_formed += text.bytes().filter(|b| !b.is_ascii()).count();
                break;
            }
            Err(error) => {
                let (valid, after) = rest.split_at(error.valid_up_to());
                well_formed += valid.iter().filter(|b| !b.is_ascii()).count();
                // Skip the bad byte(s); a truncated sequence ends the input.
                let skip = error.error_len().unwrap_or(after.len());
                stray += skip;
                rest = &after[skip..];
            }
        }
    }
    well_formed > 0 && stray * 4 <= well_formed
}

/// `bytes` as text: a byte-order mark names its encoding; else UTF-8; else
/// the encoding `chardetng` guesses. Returns the text and which decoder made
/// it.
fn decode(bytes: &[u8]) -> Result<(String, &'static str), String> {
    if let Some((encoding, _)) = encoding_rs::Encoding::for_bom(bytes) {
        // `decode` strips the mark.
        let (text, _, malformed) = encoding.decode(bytes);
        return if malformed {
            Err(format!("the content is not valid {}", encoding.name()))
        } else {
            Ok((text.into_owned(), "bom"))
        };
    }
    if let Ok(text) = std::str::from_utf8(bytes) {
        return Ok((text.to_owned(), "utf8"));
    }
    // A NUL byte is binary data, not a legacy text encoding.
    if bytes.contains(&0) {
        return Err("the content is not text".to_owned());
    }
    // UTF-8 with damage (a stray byte, a cut sequence) is not a legacy
    // encoding: guessing would turn its characters into other characters.
    if is_damaged_utf8(bytes) {
        return Err("the content is UTF-8 with invalid bytes".to_owned());
    }
    let mut detector = chardetng::EncodingDetector::new(chardetng::Iso2022JpDetection::Allow);
    detector.feed(bytes, true);
    let encoding = detector.guess(None, chardetng::Utf8Detection::Allow);
    let (text, _, malformed) = encoding.decode(bytes);
    if malformed {
        return Err(format!(
            "the content is not UTF-8 and does not decode as {}",
            encoding.name()
        ));
    }
    if looks_binary(&text) {
        return Err(format!(
            "the content is binary (it decodes as {} only into control or replacement characters)",
            encoding.name()
        ));
    }
    Ok((text.into_owned(), "chardetng"))
}

/// Whether a legacy-encoding decode is binary data that happened to decode:
/// over 1% of its characters are control characters (C0 other than tab, line
/// feed, carriage return and form feed; DEL; C1), or over 0.5% are U+FFFD.
/// Real text in any legacy encoding has almost none of either.
fn looks_binary(text: &str) -> bool {
    let (mut total, mut controls, mut replacements) = (0_usize, 0_usize, 0_usize);
    for c in text.chars() {
        total += 1;
        if c == '\u{FFFD}' {
            replacements += 1;
        } else if c.is_control() && !matches!(c, '\t' | '\n' | '\r' | '\u{000C}') {
            controls += 1;
        }
    }
    controls * 100 > total || replacements * 200 > total
}

/// The conversion options the HTML extraction uses: the page's own metadata
/// and the cleanup pass off, as in the Confluence toolkit. Shared so another
/// caller converts HTML the same way.
#[cfg(feature = "html")]
#[must_use]
pub fn markdown_options() -> html_to_markdown_rs::ConversionOptions {
    html_to_markdown_rs::ConversionOptions {
        extract_metadata: false,
        preprocessing: html_to_markdown_rs::PreprocessingOptions {
            enabled: false,
            ..html_to_markdown_rs::PreprocessingOptions::default()
        },
        ..html_to_markdown_rs::ConversionOptions::default()
    }
}

/// A counting semaphore: std has none.
#[cfg(feature = "html")]
struct Semaphore {
    free: Mutex<usize>,
    released: Condvar,
}

/// A held permit; released when dropped, on whichever thread drops it.
#[cfg(feature = "html")]
struct Permit(Arc<Semaphore>);

#[cfg(feature = "html")]
impl Semaphore {
    fn new(permits: usize) -> Arc<Self> {
        Arc::new(Self {
            free: Mutex::new(permits),
            released: Condvar::new(),
        })
    }

    /// A permit, waiting until `deadline` for one; `None` if none came free.
    fn acquire(this: &Arc<Self>, deadline: Instant) -> Option<Permit> {
        let mut free = this
            .free
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner);
        loop {
            if *free > 0 {
                *free -= 1;
                return Some(Permit(Arc::clone(this)));
            }
            let remaining = deadline.checked_duration_since(Instant::now())?;
            let (guard, result) = this
                .released
                .wait_timeout(free, remaining)
                .unwrap_or_else(std::sync::PoisonError::into_inner);
            free = guard;
            if result.timed_out() && *free == 0 {
                return None;
            }
        }
    }
}

#[cfg(feature = "html")]
impl Drop for Permit {
    fn drop(&mut self) {
        *self
            .0
            .free
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner) += 1;
        self.0.released.notify_one();
    }
}

/// The process-wide conversion capacity.
#[cfg(feature = "html")]
fn capacity() -> &'static Arc<Semaphore> {
    static CAPACITY: std::sync::OnceLock<Arc<Semaphore>> = std::sync::OnceLock::new();
    CAPACITY.get_or_init(|| Semaphore::new(MAX_CONVERSION_THREADS))
}

/// HTML as markdown; a page with no text is unreadable.
#[cfg(feature = "html")]
fn html(bytes: &[u8]) -> Extracted {
    html_guarded(
        bytes,
        &Guard {
            timeout: Duration::from_secs(TIMEOUT_SECONDS),
            max_bytes: MAX_HTML_BYTES,
            inline_bytes: INLINE_HTML_BYTES,
            capacity: Arc::clone(capacity()),
        },
    )
}

/// The limits one conversion runs under.
#[cfg(feature = "html")]
struct Guard {
    timeout: Duration,
    max_bytes: usize,
    /// Pages up to this size convert on the caller's thread.
    inline_bytes: usize,
    capacity: Arc<Semaphore>,
}

/// [`html`] with its limits as arguments.
///
/// A larger page converts on its own thread (a large stack, since the
/// converter recurses with the page's nesting) under a deadline. A thread
/// that overruns CANNOT be killed: it is left to finish and its result is
/// dropped, so it keeps its capacity permit until it really ends. At most
/// [`MAX_CONVERSION_THREADS`] exist at once, however many pages time out; a
/// conversion that finds none free waits for the rest of its deadline and
/// then gives up.
#[cfg(feature = "html")]
fn html_guarded(bytes: &[u8], guard: &Guard) -> Extracted {
    let deadline = Instant::now() + guard.timeout;
    if bytes.len() > guard.max_bytes {
        return Extracted::Unreadable(format!(
            "the page is {} bytes, over the {}-byte limit",
            bytes.len(),
            guard.max_bytes
        ));
    }
    let source = match decode(bytes) {
        Ok((source, _)) => source,
        Err(reason) => return Extracted::Unreadable(reason),
    };
    if bytes.len() <= guard.inline_bytes {
        return convert_html(&source);
    }
    let Some(permit) = Semaphore::acquire(&guard.capacity, deadline) else {
        return Extracted::Unreadable("conversion capacity exhausted".to_owned());
    };
    let (sender, receiver) = mpsc::channel();
    let spawned = std::thread::Builder::new()
        .name("html-to-markdown".to_owned())
        .stack_size(STACK_BYTES)
        .spawn(move || {
            let extracted = convert_html(&source);
            let _ = sender.send(extracted);
            drop(permit);
        });
    if let Err(error) = spawned {
        return Extracted::Unreadable(format!("the conversion thread did not start: {error}"));
    }
    let remaining = deadline.saturating_duration_since(Instant::now());
    match receiver.recv_timeout(remaining) {
        Ok(extracted) => extracted,
        Err(mpsc::RecvTimeoutError::Timeout) => Extracted::Unreadable(format!(
            "the page took longer than {} seconds to convert",
            guard.timeout.as_secs()
        )),
        Err(mpsc::RecvTimeoutError::Disconnected) => {
            Extracted::Unreadable("the page did not convert (the converter failed)".to_owned())
        }
    }
}

#[cfg(feature = "html")]
fn convert_html(source: &str) -> Extracted {
    match html_to_markdown_rs::convert(source, markdown_options()) {
        Ok(result) => match result.content {
            Some(text) if !text.trim().is_empty() => Extracted::Text {
                text,
                extractor: "html-to-markdown",
            },
            _ => Extracted::Unreadable("the page has no text".to_owned()),
        },
        Err(error) => Extracted::Unreadable(format!("the page did not convert: {error}")),
    }
}

#[cfg(not(feature = "documents"))]
mod documents {
    use super::Extracted;

    pub(super) fn extract(_mime: &str, _bytes: &[u8]) -> Extracted {
        Extracted::Unsupported
    }
}

#[cfg(feature = "documents")]
mod documents {
    //! xberg, as measured for the engines (ADR-0028 D4): its cache off (a
    //! distroless root filesystem is read-only), a timeout, a page and a
    //! size cap, and its own thread with a large stack — its extraction can
    //! recurse deeper than a runtime worker's default stack allows.

    use super::{Extracted, STACK_BYTES, TIMEOUT_SECONDS};

    /// Pages read from one document (where the format has pages).
    const MAX_PAGES: usize = 1000;
    /// Bytes of content one document may hold.
    const MAX_CONTENT_BYTES: usize = 50 * 1024 * 1024;

    pub(super) fn extract(mime: &str, bytes: &[u8]) -> Extracted {
        let (mime, bytes) = (mime.to_owned(), bytes.to_vec());
        let worker = std::thread::Builder::new()
            .name("doc-extract".to_owned())
            .stack_size(STACK_BYTES)
            .spawn(move || run(&mime, bytes));
        match worker.map(std::thread::JoinHandle::join) {
            Ok(Ok(extracted)) => extracted,
            Ok(Err(_)) => Extracted::Unreadable("the document extractor failed".to_owned()),
            Err(error) => {
                Extracted::Unreadable(format!("the extraction thread did not start: {error}"))
            }
        }
    }

    fn run(mime: &str, bytes: Vec<u8>) -> Extracted {
        let runtime = match tokio::runtime::Builder::new_current_thread()
            .enable_time()
            .build()
        {
            Ok(runtime) => runtime,
            Err(error) => {
                return Extracted::Unreadable(format!("no runtime for the extractor: {error}"));
            }
        };
        let config = xberg::ExtractionConfig {
            use_cache: false,
            extraction_timeout_secs: Some(TIMEOUT_SECONDS),
            security_limits: Some(xberg::SecurityLimits {
                max_pages: Some(MAX_PAGES),
                max_content_size: MAX_CONTENT_BYTES,
                ..xberg::SecurityLimits::default()
            }),
            ..xberg::ExtractionConfig::default()
        };
        let input = xberg::ExtractInput::from_bytes(bytes, mime.to_owned(), None);
        match runtime.block_on(xberg::extract(input, &config)) {
            Ok(result) => match result.results.into_iter().next() {
                Some(document) if !document.content.trim().is_empty() => Extracted::Text {
                    text: document.content,
                    extractor: "xberg",
                },
                _ => Extracted::Unreadable("the document has no text".to_owned()),
            },
            Err(error) => Extracted::Unreadable(error.to_string()),
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    /// A minimal RTF and an EML: formats the `documents` build reads.
    #[cfg(feature = "documents")]
    #[test]
    fn documents_are_extracted_by_xberg() {
        let rtf =
            br"{\rtf1\ansi{\fonttbl\f0\fswiss Helvetica;}\f0\pard Refunds within thirty days.\par}";
        match extract("application/rtf", rtf) {
            Extracted::Text { text, extractor } => {
                assert_eq!(extractor, "xberg");
                assert!(text.contains("Refunds within thirty days"), "{text}");
            }
            other => panic!("{other:?}"),
        }
        let eml = b"From: a@example.com\r\nTo: b@example.com\r\nSubject: Refund policy\r\nContent-Type: text/plain\r\n\r\nRefunds go through Billing.\r\n";
        match extract("message/rfc822", eml) {
            Extracted::Text { text, .. } => {
                assert!(text.contains("Refunds go through Billing"), "{text}");
            }
            other => panic!("{other:?}"),
        }
        assert!(matches!(
            extract("application/pdf", b"not a pdf"),
            Extracted::Unreadable(_)
        ));
    }

    #[test]
    fn utf8_text_is_read_as_it_is_and_other_formats_are_not_guessed() {
        assert_eq!(
            extract("text/markdown", b"# hi\n"),
            Extracted::Text {
                text: "# hi\n".to_owned(),
                extractor: "utf8"
            }
        );
        assert_eq!(extract("image/png", b"\x89PNG"), Extracted::Unsupported);
        assert!(can_extract("application/json") && can_extract("text/html"));
        assert_eq!(can_extract("application/pdf"), cfg!(feature = "documents"));
    }

    #[test]
    fn legacy_encodings_are_detected_and_binary_is_refused() {
        // Windows-1252 French prose: 0xE9 is e-acute, which is not UTF-8.
        let latin =
            b"Le caf\xe9 est ferm\xe9 le dimanche; les clients r\xe9guliers le savent d\xe9j\xe0.";
        match extract("text/plain", latin) {
            Extracted::Text { text, extractor } => {
                assert_eq!(extractor, "chardetng");
                assert!(text.contains("café") && text.contains("fermé"), "{text}");
            }
            other => panic!("{other:?}"),
        }
        // Shift_JIS.
        let (sjis, _, _) = encoding_rs::SHIFT_JIS
            .encode("これは日本語の文章です。返金は三十日以内に受け付けます。");
        match extract("text/plain", &sjis) {
            Extracted::Text { text, .. } => assert!(text.contains("返金"), "{text}"),
            other => panic!("{other:?}"),
        }
        // UTF-16LE with a byte-order mark.
        let mut utf16 = vec![0xFF, 0xFE];
        for unit in "hello world".encode_utf16() {
            utf16.extend(unit.to_le_bytes());
        }
        assert_eq!(
            extract("text/plain", &utf16),
            Extracted::Text {
                text: "hello world".to_owned(),
                extractor: "bom"
            }
        );
        // A NUL byte is binary, not a legacy encoding.
        assert!(matches!(
            extract("text/plain", b"abc\xe9\x00def"),
            Extracted::Unreadable(_)
        ));
        // Valid UTF-8 is untouched.
        assert!(matches!(
            extract("text/plain", "naïve".as_bytes()),
            Extracted::Text {
                extractor: "utf8",
                ..
            }
        ));
    }

    #[test]
    fn damaged_utf8_is_refused_not_guessed() {
        // UTF-8 prose with one stray byte (a Latin-1 e-acute pasted in):
        // the rest is UTF-8, so the file is damaged, not Windows-1252.
        let mut damaged = "Le café est fermé le dimanche, naïve résumé"
            .as_bytes()
            .to_vec();
        damaged.extend_from_slice(b" et r\xe9ouvre lundi.");
        assert!(matches!(
            extract("text/plain", &damaged),
            Extracted::Unreadable(_)
        ));
        // A sequence cut off at the end.
        let mut cut = "Émile et Zoé sont déjà là, à côté".as_bytes().to_vec();
        cut.push(0xC3);
        assert!(matches!(
            extract("text/plain", &cut),
            Extracted::Unreadable(_)
        ));
        // ASCII with one high byte has no sequence: legacy text. So is a
        // text where UTF-8 lookalikes are the minority (the Shift_JIS test).
        assert!(matches!(
            extract("text/plain", b"plain ascii with one caf\xe9 only"),
            Extracted::Text {
                extractor: "chardetng",
                ..
            }
        ));
    }

    #[test]
    fn random_binary_without_a_nul_is_refused_but_real_legacy_text_decodes() {
        // Pseudo-random bytes, none of them NUL.
        let mut state = 0x2545_F491_4F6C_DD1D_u64;
        let binary: Vec<u8> = (0..4096)
            .map(|_| {
                state ^= state << 13;
                state ^= state >> 7;
                state ^= state << 17;
                u8::try_from(state % 255).unwrap_or(1) + 1
            })
            .collect();
        assert!(!binary.contains(&0));
        assert!(matches!(
            extract("text/plain", &binary),
            Extracted::Unreadable(_)
        ));
        assert!(decode_text(&binary).is_err());
        // Control-heavy bytes that are not UTF-8 either (0x81 is a C1
        // control in the single-byte encodings).
        let controls: Vec<u8> = (0..2000).map(|i| [1_u8, 2, 3, 0x81, 4][i % 5]).collect();
        assert!(decode_text(&controls).is_err());
        // Windows-1252 and Shift_JIS prose still decode.
        let latin = b"Le caf\xe9 est ferm\xe9 le dimanche; les clients r\xe9guliers le savent d\xe9j\xe0.\r\n\tIndent.";
        assert!(decode_text(latin).is_ok_and(|t| t.contains("café")));
        let (sjis, _, _) =
            encoding_rs::SHIFT_JIS.encode("これは日本語の文章です。返金は三十日以内です。");
        assert!(decode_text(&sjis).is_ok_and(|t| t.contains("返金")));
        assert_eq!(decode_text("plain".as_bytes()), Ok("plain".to_owned()));
    }

    #[cfg(not(feature = "html"))]
    #[test]
    fn html_conversion_needs_the_html_feature() {
        let options = ExtractOptions {
            html_to_markdown: true,
        };
        assert_eq!(
            extract_with("text/html", b"<h1>x</h1>", &options),
            Extracted::Unsupported
        );
        assert!(!can_extract_with("text/html", &options));
        // Without the opt-in, HTML is still text.
        assert!(can_extract("text/html"));
    }

    #[cfg(feature = "html")]
    #[test]
    fn html_is_raw_text_by_default_and_markdown_when_asked() {
        let page = b"<html><head><title>T</title></head><body><h1>Refunds</h1>\
            <p>Within <strong>thirty</strong> days.</p><ul><li>Billing</li><li>Support</li></ul>\
            <script>alert(1)</script></body></html>";
        // Default: the text as written, for line citations.
        assert_eq!(
            extract("text/html", page),
            Extracted::Text {
                text: String::from_utf8_lossy(page).into_owned(),
                extractor: "utf8"
            }
        );
        assert_eq!(
            extract("application/xhtml+xml", page),
            Extracted::Unsupported
        );
        assert!(!can_extract("application/xhtml+xml"));
        // Asked for: markdown.
        let options = ExtractOptions {
            html_to_markdown: true,
        };
        assert!(can_extract_with("application/xhtml+xml", &options));
        for mime in ["text/html", "application/xhtml+xml"] {
            match extract_with(mime, page, &options) {
                Extracted::Text { text, extractor } => {
                    assert_eq!(extractor, "html-to-markdown");
                    assert!(text.contains("# Refunds"), "{text}");
                    assert!(text.contains("**thirty**"), "{text}");
                    assert!(text.contains("Billing") && !text.contains("<h1>"), "{text}");
                    assert!(!text.contains("alert(1)"), "{text}");
                }
                other => panic!("{other:?}"),
            }
        }
        // A page in a legacy encoding is decoded before it is converted.
        let latin = b"<html><body><p>Le caf\xe9 est ferm\xe9 le dimanche et les clients r\xe9guliers le savent.</p></body></html>";
        match extract_with("text/html", latin, &options) {
            Extracted::Text { text, .. } => assert!(text.contains("café"), "{text}"),
            other => panic!("{other:?}"),
        }
        assert!(matches!(
            extract_with("text/html", b"<html><body></body></html>", &options),
            Extracted::Unreadable(_)
        ));
    }

    #[cfg(feature = "html")]
    fn guard(timeout: Duration, inline_bytes: usize, permits: usize) -> Guard {
        Guard {
            timeout,
            max_bytes: MAX_HTML_BYTES,
            inline_bytes,
            capacity: Semaphore::new(permits),
        }
    }

    #[cfg(feature = "html")]
    #[test]
    fn html_conversion_survives_deep_nesting_and_huge_input() {
        let options = ExtractOptions {
            html_to_markdown: true,
        };
        // 50 000 nested divs: converted or refused, never a crash.
        let depth = 50_000;
        let deep = format!(
            "{}deep text{}",
            "<div>".repeat(depth),
            "</div>".repeat(depth)
        );
        let outcome = extract_with("text/html", deep.as_bytes(), &options);
        assert!(
            matches!(outcome, Extracted::Text { .. } | Extracted::Unreadable(_)),
            "{outcome:?}"
        );
        // Over the size cap: unreadable, without converting.
        let huge = vec![b'a'; MAX_HTML_BYTES + 1];
        assert!(matches!(
            extract_with("text/html", &huge, &options),
            Extracted::Unreadable(reason) if reason.contains("limit")
        ));
    }

    #[cfg(feature = "html")]
    #[test]
    fn the_deadline_and_the_capacity_bound_the_conversion_threads() {
        // A small page (2 000 nested divs) on a thread (`inline_bytes` 0)
        // with no time left: unreadable, and the thread's permit comes back
        // when it finishes.
        let page = format!("{}x{}", "<div>".repeat(2000), "</div>".repeat(2000));
        let capacity = Semaphore::new(1);
        let timed = Guard {
            timeout: Duration::ZERO,
            max_bytes: MAX_HTML_BYTES,
            inline_bytes: 0,
            capacity: Arc::clone(&capacity),
        };
        match html_guarded(page.as_bytes(), &timed) {
            Extracted::Unreadable(reason) => assert!(reason.contains("longer than"), "{reason}"),
            other => panic!("{other:?}"),
        }
        let back = Instant::now() + Duration::from_mins(1);
        assert!(Semaphore::acquire(&capacity, back).is_some());
        // No permit free: the conversion waits out its deadline and refuses.
        let none = guard(Duration::from_millis(50), 0, 0);
        assert_eq!(
            html_guarded(page.as_bytes(), &none),
            Extracted::Unreadable("conversion capacity exhausted".to_owned())
        );
        // A page under the inline size needs no thread and no permit.
        let small = guard(Duration::from_millis(50), INLINE_HTML_BYTES, 0);
        assert!(matches!(
            html_guarded(b"<h1>Hi</h1>", &small),
            Extracted::Text { .. }
        ));
        // `markdown_options` is the shared set.
        assert!(!markdown_options().extract_metadata);
    }
}

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

/// What a standalone image file is to the vision step (ADR-0030 I4 part B).
/// Extraction never turns an image into text; a caller that can ask a model
/// to describe it asks [`image_for_vision`].
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum VisionImage {
    /// Not an image media type: extraction decides.
    NotAnImage,
    /// An image a model is sent as it is (when small enough), with this
    /// media type (the bytes' own, which wins over the declared one).
    Supported(&'static str),
    /// A BMP: no model accepts it, so the caller rasterises it to PNG first.
    Bmp,
    /// An image format the step does not convert (SVG, TIFF, ...): the SDK
    /// rasterises SVG, this build does not.
    Unsupported,
    /// Declared PNG, JPEG, GIF, WebP or BMP, but the bytes are none of them.
    Corrupt,
}

/// Whether `mime` (already lower-cased, parameters cut) names a BMP.
fn is_bmp_type(mime: &str) -> bool {
    matches!(mime, "image/bmp" | "image/x-bmp" | "image/x-ms-bmp")
}

/// The BMP signature plus a known DIB header size (so a text file that
/// merely starts with `BM` is not one).
fn is_bmp(bytes: &[u8]) -> bool {
    bytes.len() >= 18
        && bytes.starts_with(b"BM")
        && matches!(
            u32::from_le_bytes([bytes[14], bytes[15], bytes[16], bytes[17]]),
            12 | 40 | 52 | 56 | 64 | 108 | 124
        )
}

/// Classifies `bytes` of declared media type `mime` for the vision step.
/// Pure and cheap: it reads the magic bytes and nothing else.
#[must_use]
pub fn image_for_vision(mime: &str, bytes: &[u8]) -> VisionImage {
    let mime = mime
        .split(';')
        .next()
        .unwrap_or("")
        .trim()
        .to_ascii_lowercase();
    if !mime.starts_with("image/") {
        return VisionImage::NotAnImage;
    }
    if !matches!(
        mime.as_str(),
        "image/png"
            | "image/x-png"
            | "image/jpeg"
            | "image/jpg"
            | "image/pjpeg"
            | "image/gif"
            | "image/webp"
    ) && !is_bmp_type(&mime)
    {
        return VisionImage::Unsupported;
    }
    let sniffed = if bytes.starts_with(b"\x89PNG\r\n\x1a\n") {
        Some("image/png")
    } else if bytes.starts_with(&[0xff, 0xd8, 0xff]) {
        Some("image/jpeg")
    } else if bytes.starts_with(b"GIF87a") || bytes.starts_with(b"GIF89a") {
        Some("image/gif")
    } else if bytes.len() >= 12 && &bytes[..4] == b"RIFF" && &bytes[8..12] == b"WEBP" {
        Some("image/webp")
    } else {
        None
    };
    match sniffed {
        Some(media_type) => VisionImage::Supported(media_type),
        None if is_bmp(bytes) => VisionImage::Bmp,
        None => VisionImage::Corrupt,
    }
}

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
pub use documents::{
    DocumentError, DocumentLimits, DocumentText, EXTRACTION_STACK_BYTES, EmbeddedImage,
    ImageLimits, ImagesSkipped, cooperative_timeout, extract_document,
    extract_document_on_this_thread,
};

/// Whether `mime` is one of the formats the document extractor reads (PDF,
/// Office, spreadsheets, e-mail, e-books); text and HTML are not. True
/// whether or not this build has the `documents` feature: a consumer
/// without it hands such a document to the extractor sidecar
/// (`services/elitea-doc-extractor`) or reports it unsupported.
#[must_use]
pub fn is_document_type(mime: &str) -> bool {
    DOCUMENT_TYPES.contains(&mime)
}

/// The media types [`is_document_type`] accepts.
#[must_use]
pub fn document_types() -> &'static [&'static str] {
    DOCUMENT_TYPES
}

#[cfg(feature = "documents")]
mod documents {
    //! xberg, as measured for the engines (ADR-0028 D4): its cache off (a
    //! distroless root filesystem is read-only), a timeout, a page and a
    //! size cap, and its own thread with a large stack — its extraction can
    //! recurse deeper than a runtime worker's default stack allows.

    use super::{Extracted, STACK_BYTES, TIMEOUT_SECONDS, is_document_type};
    use std::time::Duration;

    /// Pages read from one document (where the format has pages).
    const MAX_PAGES: usize = 1000;
    /// Bytes of content one document may hold.
    const MAX_CONTENT_BYTES: usize = 50 * 1024 * 1024;

    /// The stack a thread that runs [`extract_document_on_this_thread`]
    /// needs: the extractor recurses deeper than a runtime worker's default
    /// stack allows. [`extract_document`] spawns its own thread with it.
    pub const EXTRACTION_STACK_BYTES: usize = STACK_BYTES;

    /// The timeout the extractor really runs under for a caller's
    /// `timeout`: xberg takes whole seconds, so the figure is rounded UP
    /// (never down: a 1500 ms budget is two seconds, not one) and never
    /// under one second. A caller that bounds the extraction from outside
    /// (the sidecar) waits for at least this long, so the extractor's own
    /// cooperative timeout fires first and an ordinary overrun ends by
    /// itself.
    #[must_use]
    pub fn cooperative_timeout(timeout: Duration) -> Duration {
        let seconds = u64::try_from(timeout.as_millis().div_ceil(1000)).unwrap_or(u64::MAX);
        Duration::from_secs(seconds.max(1))
    }

    /// The bounds one document extraction runs under.
    #[derive(Debug, Clone, Copy, PartialEq, Eq)]
    pub struct DocumentLimits {
        /// Pages read from a paged format; a longer document is refused
        /// ([`DocumentError::TooManyPages`]), not truncated.
        pub max_pages: usize,
        /// Bytes of content the extractor may produce or decode in one
        /// operation; more is refused ([`DocumentError::TooLarge`]).
        pub max_content_bytes: usize,
        /// How long the extractor may run.
        pub timeout: Duration,
        /// Ask for the pictures embedded in the document (PDF, DOCX, PPTX),
        /// under these bounds. `None` (the default) asks for none: the
        /// extraction is then exactly what it was before pictures were
        /// asked for, and no picture is read.
        pub images: Option<ImageLimits>,
    }

    impl Default for DocumentLimits {
        fn default() -> Self {
            Self {
                max_pages: MAX_PAGES,
                max_content_bytes: MAX_CONTENT_BYTES,
                timeout: Duration::from_secs(TIMEOUT_SECONDS),
                images: None,
            }
        }
    }

    /// The bounds on the pictures returned with a document's text.
    #[derive(Debug, Clone, Copy, PartialEq, Eq)]
    pub struct ImageLimits {
        /// Pictures returned; later ones are counted in
        /// [`ImagesSkipped::too_many`].
        pub max_images: usize,
        /// Bytes of one picture; a larger one is counted in
        /// [`ImagesSkipped::too_large`].
        pub max_image_bytes: usize,
        /// Bytes of all the returned pictures together; a picture that would
        /// cross it is counted in [`ImagesSkipped::too_large`].
        pub max_total_bytes: usize,
    }

    /// One picture embedded in a document, as the parser stored it: the
    /// bytes are passed through, never decoded or re-encoded here.
    #[derive(Debug, Clone, PartialEq, Eq)]
    pub struct EmbeddedImage {
        pub data: Vec<u8>,
        /// The media type the BYTES say (`image/png`, `image/jpeg`,
        /// `image/gif`, `image/webp`, `image/bmp`), never the package's
        /// claim.
        pub media_type: &'static str,
        /// Position among the document's pictures, in document order.
        pub ordinal: u32,
        /// Where in the text its description belongs: a byte offset on a
        /// character boundary of [`DocumentText::text`]. Inline, right after
        /// the picture's own `[Image: …]` line, when the text has one (DOCX);
        /// otherwise at the end of its page or slide's text; otherwise at the
        /// end of the text.
        pub text_offset: usize,
        /// The page or slide (1-based) the parser found it on, when it is
        /// paged.
        pub page: Option<u32>,
        /// The file name inside the package (`image1.png`), when the format
        /// has one.
        pub name: Option<String>,
    }

    /// Pictures found and not returned, by reason.
    #[derive(Debug, Clone, Copy, Default, PartialEq, Eq)]
    pub struct ImagesSkipped {
        /// Over [`ImageLimits::max_images`].
        pub too_many: u32,
        /// Over [`ImageLimits::max_image_bytes`] or the total.
        pub too_large: u32,
        /// Not a format a model takes (PNG, JPEG, GIF, WebP, BMP), or a
        /// mask.
        pub unsupported: u32,
    }

    /// A document's text, and (when asked for) its embedded pictures.
    #[derive(Debug, Clone, PartialEq, Eq)]
    pub struct DocumentText {
        pub text: String,
        /// Which extractor produced it (`xberg`).
        pub extractor: &'static str,
        /// The pictures, in document order. Always empty unless
        /// [`DocumentLimits::images`] asked for them.
        pub images: Vec<EmbeddedImage>,
        pub images_skipped: ImagesSkipped,
    }

    /// Why a document gave no text. Each variant is a reason a caller can
    /// report as such (the extractor sidecar's typed skip reasons); the
    /// detail is the extractor's own sentence, for a log line.
    #[derive(Debug, Clone, PartialEq, Eq)]
    pub enum DocumentError {
        /// No extractor handles this media type.
        Unsupported(String),
        /// The document is password-protected, or uses an encryption the
        /// extractor does not implement.
        Encrypted(String),
        /// More pages than [`DocumentLimits::max_pages`].
        TooManyPages(String),
        /// More content than [`DocumentLimits::max_content_bytes`], or
        /// another of the extractor's security limits (archive size,
        /// nesting, entity length) was hit.
        TooLarge(String),
        /// The extractor ran past [`DocumentLimits::timeout`].
        Timeout(String),
        /// The bytes are not what the media type says, or are damaged.
        Corrupt(String),
        /// The document parsed but holds no text (a scanned PDF, an empty
        /// sheet).
        NoText,
        /// The extractor itself failed: a panic on its thread, no thread, no
        /// runtime. Not a statement about the document.
        Extractor(String),
    }

    impl std::fmt::Display for DocumentError {
        fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
            match self {
                Self::Unsupported(detail) => write!(f, "unsupported document type: {detail}"),
                Self::Encrypted(detail) => write!(f, "the document is encrypted: {detail}"),
                Self::TooManyPages(detail) => {
                    write!(f, "the document has too many pages: {detail}")
                }
                Self::TooLarge(detail) => write!(f, "the document is too large: {detail}"),
                Self::Timeout(detail) => write!(f, "the extraction timed out: {detail}"),
                Self::Corrupt(detail) => write!(f, "the document is corrupt: {detail}"),
                Self::NoText => f.write_str("the document has no text"),
                Self::Extractor(detail) => write!(f, "the document extractor failed: {detail}"),
            }
        }
    }

    impl std::error::Error for DocumentError {}

    impl From<DocumentError> for Extracted {
        fn from(error: DocumentError) -> Self {
            match error {
                DocumentError::Unsupported(_) => Self::Unsupported,
                other => Self::Unreadable(other.to_string()),
            }
        }
    }

    /// [`super::extract`] for the formats behind the `documents` feature.
    /// The one copy of the document on this path is made here, for the
    /// borrowed API; [`extract_document`] itself takes the bytes by value.
    pub(super) fn extract(mime: &str, bytes: &[u8]) -> Extracted {
        match extract_document(mime, bytes.to_vec(), &DocumentLimits::default()) {
            Ok(DocumentText {
                text, extractor, ..
            }) => Extracted::Text { text, extractor },
            Err(error) => error.into(),
        }
    }

    /// `bytes` of media type `mime` as text, under `limits`, with the reason
    /// typed when there is none.
    ///
    /// The bytes are taken by value and MOVED onto the extraction thread and
    /// into the parser: this function makes no copy of the document, so a
    /// caller's memory budget counts the one buffer it hands over plus the
    /// parser's own expansion. (The borrowed [`super::extract`] copies once,
    /// before calling this.)
    ///
    /// The extraction runs on its own thread with a large stack. A thread
    /// that overruns the timeout CANNOT be killed: the extractor's own
    /// cooperative timeout ends most overruns, and a caller that must bound
    /// the worst case runs this in a process it can end (the extractor
    /// sidecar does).
    ///
    /// # Errors
    ///
    /// [`DocumentError`], one variant per reason.
    pub fn extract_document(
        mime: &str,
        bytes: Vec<u8>,
        limits: &DocumentLimits,
    ) -> Result<DocumentText, DocumentError> {
        if !is_document_type(mime) {
            return Err(DocumentError::Unsupported(mime.to_owned()));
        }
        let (mime, limits) = (mime.to_owned(), *limits);
        let worker = std::thread::Builder::new()
            .name("doc-extract".to_owned())
            .stack_size(STACK_BYTES)
            .spawn(move || extract_document_on_this_thread(&mime, bytes, &limits));
        match worker.map(std::thread::JoinHandle::join) {
            Ok(Ok(result)) => result,
            Ok(Err(_)) => Err(DocumentError::Extractor(
                "the extraction thread panicked".to_owned(),
            )),
            Err(error) => Err(DocumentError::Extractor(format!(
                "the extraction thread did not start: {error}"
            ))),
        }
    }

    /// [`extract_document`] on the CALLING thread: no thread is spawned
    /// here. For a caller that already runs on a thread of its own with
    /// [`EXTRACTION_STACK_BYTES`] of stack and bounds it from outside (the
    /// extractor sidecar, which holds its slot and its overrun ledger on
    /// that one thread). Never call this on a runtime worker thread: the
    /// extractor blocks, recurses deeply, and builds a runtime of its own.
    ///
    /// # Errors
    ///
    /// [`DocumentError`], one variant per reason.
    pub fn extract_document_on_this_thread(
        mime: &str,
        bytes: Vec<u8>,
        limits: &DocumentLimits,
    ) -> Result<DocumentText, DocumentError> {
        if !is_document_type(mime) {
            return Err(DocumentError::Unsupported(mime.to_owned()));
        }
        let runtime = tokio::runtime::Builder::new_current_thread()
            .enable_time()
            .build()
            .map_err(|error| {
                DocumentError::Extractor(format!("no runtime for the extractor: {error}"))
            })?;
        let mut config = xberg::ExtractionConfig {
            use_cache: false,
            extraction_timeout_secs: Some(cooperative_timeout(limits.timeout).as_secs()),
            security_limits: Some(xberg::SecurityLimits {
                max_pages: Some(limits.max_pages),
                max_content_size: limits.max_content_bytes,
                ..xberg::SecurityLimits::default()
            }),
            ..xberg::ExtractionConfig::default()
        };
        // What the pictures cost to read is decided BEFORE the parser runs:
        // the parser reads every picture it is allowed to into memory and
        // only then can they be dropped.
        let mut refused = ImagesSkipped::default();
        let mut per_page = None;
        if let Some(images) = &limits.images {
            match picture_work(mime, &bytes, images) {
                PictureWork::Extract { per_page_cap } => {
                    // The plain text stays exactly what it was: the parser's
                    // own placeholders (DOCX prints `[Image: ...]` for a
                    // picture) stay on, because turning them off DROPS those
                    // lines from the text (measured), and the page layout is
                    // asked for only to anchor a picture to its page.
                    per_page = Some(per_page_cap);
                    config.images = Some(xberg::ImageExtractionConfig {
                        extract_images: true,
                        inject_placeholders: true,
                        // The parser keeps one more than the cap, so a page
                        // that overflows is SEEN to (see `embedded_images`).
                        max_images_per_page: Some(
                            u32::try_from(per_page_cap.saturating_add(1)).unwrap_or(u32::MAX),
                        ),
                        ..xberg::ImageExtractionConfig::default()
                    });
                    config.pages = Some(xberg::PageConfig {
                        extract_pages: true,
                        insert_page_markers: false,
                        ..xberg::PageConfig::default()
                    });
                }
                PictureWork::Refuse(counted) => refused = counted,
            }
        }
        let input = xberg::ExtractInput::from_bytes(bytes, mime.to_owned(), None);
        match runtime.block_on(xberg::extract(input, &config)) {
            Ok(result) => match result.results.into_iter().next() {
                Some(document) if !document.content.trim().is_empty() => {
                    let (images, mut images_skipped) = match (&limits.images, per_page) {
                        (Some(bounds), Some(per_page)) => {
                            embedded_images(&document, bounds, per_page)
                        }
                        _ => (Vec::new(), ImagesSkipped::default()),
                    };
                    images_skipped.too_many += refused.too_many;
                    images_skipped.too_large += refused.too_large;
                    images_skipped.unsupported += refused.unsupported;
                    Ok(DocumentText {
                        text: document.content,
                        extractor: "xberg",
                        images,
                        images_skipped,
                    })
                }
                _ => Err(DocumentError::NoText),
            },
            Err(error) => Err(classify(&error)),
        }
    }

    /// The typed reason behind an xberg error. The PDF backend's own errors
    /// arrive wrapped as `Parsing` with the PDF error as the source; the
    /// security limits as `Security` with the typed `SecurityError` as the
    /// source (`From<SecurityError> for XbergError` keeps it), which is what
    /// the page limit is read from — never the sentence, which an xberg
    /// release may reword. An xberg release that drops the typed source
    /// would turn every over-limit document into `TooLarge`, and the
    /// sidecar's over-limit PDF test (`too_many_pages_is_refused_not_truncated`,
    /// a real document through xberg) would fail with it.
    ///
    /// `Io`, `Serialization` and `Other` are the extractor's own failures (a
    /// temp file it could not write, a serde round-trip, a catch-all), not
    /// statements about the document: [`DocumentError::Extractor`], which
    /// the sidecar answers as a gRPC `INTERNAL` status and the worker records
    /// `extract_failed`, so the document is retried by the next run instead
    /// of being filed as corrupt.
    fn classify(error: &xberg::XbergError) -> DocumentError {
        use xberg::XbergError;
        use xberg::extractors::security::SecurityError;
        let detail = error.to_string();
        match error {
            XbergError::Timeout { .. } | XbergError::Cancelled => DocumentError::Timeout(detail),
            XbergError::UnsupportedFormat(_) | XbergError::MissingDependency(_) => {
                DocumentError::Unsupported(detail)
            }
            XbergError::Security { source, .. } => match source
                .as_ref()
                .and_then(|source| source.downcast_ref::<SecurityError>())
            {
                Some(SecurityError::TooManyPages { .. }) => DocumentError::TooManyPages(detail),
                _ => DocumentError::TooLarge(detail),
            },
            XbergError::Parsing { message, source } => {
                use xberg::pdf::error::PdfError;
                // The native PDF backend reports a document it cannot open
                // without a password as a `Parsing` error with this sentence
                // and no source (`pdf/native/mod.rs`); its typed errors
                // arrive as the source.
                let password_sentence = message.starts_with("PDF is encrypted");
                match source
                    .as_ref()
                    .and_then(|source| source.downcast_ref::<PdfError>())
                {
                    Some(
                        PdfError::PasswordRequired
                        | PdfError::InvalidPassword
                        | PdfError::EncryptionNotSupported(_),
                    ) => DocumentError::Encrypted(detail),
                    _ if password_sentence => DocumentError::Encrypted(detail),
                    _ => DocumentError::Corrupt(detail),
                }
            }
            XbergError::Validation { .. } | XbergError::ImageProcessing { .. } => {
                DocumentError::Corrupt(detail)
            }
            XbergError::Io(_) | XbergError::Serialization { .. } | XbergError::Other(_) => {
                DocumentError::Extractor(detail)
            }
            _ => DocumentError::Extractor(detail),
        }
    }

    /// The most pictures read from one PDF page: the parser can only be
    /// capped per page, and walks every page (up to `max_pages`) in
    /// parallel.
    const PDF_PICTURES_PER_PAGE: usize = 4;

    /// How many times the pictures' own bounds the media of a DOCX or PPTX
    /// may weigh, in count and in uncompressed bytes, before no picture is
    /// read from it at all.
    const MEDIA_WORK_FACTOR: usize = 4;

    /// What the pictures of a document cost, decided before the parser runs.
    enum PictureWork {
        /// Ask the parser; it keeps at most this many per page.
        Extract { per_page_cap: usize },
        /// Read none; this many were found and are counted as skipped.
        Refuse(ImagesSkipped),
    }

    fn picture_work(mime: &str, bytes: &[u8], bounds: &ImageLimits) -> PictureWork {
        if mime == "application/pdf" {
            // PDF: the parser decodes pictures page by page with no document
            // budget, so the work is bounded by the page count (`max_pages`)
            // times a small per-page cap, whatever the document holds.
            return PictureWork::Extract {
                per_page_cap: bounds.max_images.min(PDF_PICTURES_PER_PAGE),
            };
        }
        if !(mime.contains("wordprocessingml") || mime.contains("presentationml")) {
            // Other types have no pictures this crate returns.
            return PictureWork::Refuse(ImagesSkipped::default());
        }
        // DOCX and PPTX: the package's media are counted from its central
        // directory (nothing is decompressed) and the document gives its
        // pictures only when they fit the work bound.
        let Some(media) = scan_media(bytes) else {
            // Not scannable (no directory, zip64): no picture is read; the
            // text still is.
            return PictureWork::Refuse(ImagesSkipped::default());
        };
        let count_bound = bounds.max_images.saturating_mul(MEDIA_WORK_FACTOR);
        let bytes_bound = (bounds.max_total_bytes as u64).saturating_mul(MEDIA_WORK_FACTOR as u64);
        let found = u32::try_from(media.count).unwrap_or(u32::MAX);
        if media.count > count_bound {
            return PictureWork::Refuse(ImagesSkipped {
                too_many: found,
                ..ImagesSkipped::default()
            });
        }
        if media.bytes > bytes_bound {
            return PictureWork::Refuse(ImagesSkipped {
                too_large: found,
                ..ImagesSkipped::default()
            });
        }
        PictureWork::Extract {
            per_page_cap: bounds.max_images,
        }
    }

    /// The media entries of a package.
    #[derive(Debug, PartialEq, Eq)]
    struct Media {
        count: usize,
        /// Their uncompressed sizes, as the directory declares them.
        bytes: u64,
    }

    /// The files under `word/media/` and `ppt/media/` of a zip, from its
    /// central directory alone. `None` when the directory cannot be read as
    /// plain zip (no end record, truncated, zip64).
    fn scan_media(zip: &[u8]) -> Option<Media> {
        const END: [u8; 4] = [0x50, 0x4b, 0x05, 0x06];
        const ENTRY: [u8; 4] = [0x50, 0x4b, 0x01, 0x02];
        let le16 = |at: usize| -> Option<usize> {
            zip.get(at..at + 2)
                .map(|b| usize::from(u16::from_le_bytes([b[0], b[1]])))
        };
        let le32 = |at: usize| -> Option<u64> {
            zip.get(at..at + 4)
                .map(|b| u64::from(u32::from_le_bytes([b[0], b[1], b[2], b[3]])))
        };
        // The end record is within the last 22 + 65535 bytes.
        let from = zip.len().saturating_sub(22 + 65_535);
        let end = (from..=zip.len().checked_sub(22)?)
            .rev()
            .find(|at| zip[*at..*at + 4] == END)?;
        let entries = le16(end + 10)?;
        let size = le32(end + 12)?;
        let offset = le32(end + 16)?;
        if entries == 0xFFFF || size == 0xFFFF_FFFF || offset == 0xFFFF_FFFF {
            return None;
        }
        let mut at = usize::try_from(offset).ok()?;
        let stop = at.checked_add(usize::try_from(size).ok()?)?;
        if stop > zip.len() {
            return None;
        }
        let mut media = Media { count: 0, bytes: 0 };
        for _ in 0..entries {
            if zip.get(at..at + 4)? != ENTRY {
                return None;
            }
            let uncompressed = le32(at + 24)?;
            let (name, extra, comment) = (le16(at + 28)?, le16(at + 30)?, le16(at + 32)?);
            let name_bytes = zip.get(at + 46..at + 46 + name)?;
            let in_media =
                name_bytes.starts_with(b"word/media/") || name_bytes.starts_with(b"ppt/media/");
            if in_media && !name_bytes.ends_with(b"/") {
                media.count += 1;
                // A zip64 size is a size this scan does not know: it counts
                // as over any bound.
                media.bytes = media.bytes.saturating_add(if uncompressed == 0xFFFF_FFFF {
                    u64::MAX
                } else {
                    uncompressed
                });
            }
            at += 46 + name + extra + comment;
            if at > stop {
                return None;
            }
        }
        Some(media)
    }

    /// The media type the bytes of a picture say, if a model takes it.
    fn sniff_image(bytes: &[u8]) -> Option<&'static str> {
        match super::image_for_vision("image/png", bytes) {
            super::VisionImage::Supported(media_type) => Some(media_type),
            super::VisionImage::Bmp => Some("image/bmp"),
            _ => None,
        }
    }

    /// One page or slide of a document, as the parser split its text.
    struct PageText<'a> {
        number: u32,
        content: &'a str,
    }

    /// Where a picture was found.
    struct Site<'a> {
        page: Option<u32>,
        /// The text the parser prints for it in plain text, when it prints
        /// any (`[Image: <description>]`).
        description: Option<&'a str>,
    }

    /// The byte offset in `text` where each picture's description belongs.
    ///
    /// The parser's plain text carries no marker for a picture, so the
    /// position is recovered from what it does give: a picture that the
    /// text names (`[Image: <alt text or file name>]`, as DOCX does) is
    /// anchored right after that line; any other picture at the end of its
    /// page's or slide's text (the page is located by finding the parser's
    /// per-page text in the whole, in order); and one that is neither at the
    /// end of the text. An anchor is always on a character boundary. Pure:
    /// the same input gives the same offsets.
    fn anchors(text: &str, pages: &[PageText<'_>], sites: &[Site<'_>]) -> Vec<usize> {
        // (page number, start, end) of every page whose text was found.
        let mut spans: Vec<(u32, usize, usize)> = Vec::with_capacity(pages.len());
        let mut cursor = 0;
        for page in pages {
            if page.content.is_empty() {
                spans.push((page.number, cursor, cursor));
            } else if let Some(found) = text[cursor..].find(page.content) {
                let start = cursor + found;
                cursor = start + page.content.len();
                spans.push((page.number, start, cursor));
            }
        }
        let mut inline_cursor = 0;
        sites
            .iter()
            .map(|site| {
                let span = site
                    .page
                    .and_then(|page| spans.iter().find(|span| span.0 == page));
                let (low, high) = span.map_or((0, text.len()), |span| (span.1, span.2));
                if let Some(description) = site.description.filter(|d| !d.trim().is_empty()) {
                    let from = inline_cursor.max(low);
                    let needle = format!("[Image: {description}]");
                    if from <= high
                        && let Some(found) = text[from..high].find(&needle)
                    {
                        inline_cursor = from + found + needle.len();
                        return inline_cursor;
                    }
                }
                span.map_or(text.len(), |span| span.2)
            })
            .collect()
    }

    /// The pictures of `document` that fit `bounds`, in document order, each
    /// with its anchor in the text; and how many did not fit, by reason.
    /// The bytes are the parser's own, copied once into the result.
    fn embedded_images(
        document: &xberg::ExtractedDocument,
        bounds: &ImageLimits,
        per_page_cap: usize,
    ) -> (Vec<EmbeddedImage>, ImagesSkipped) {
        let mut found: Vec<&xberg::ExtractedImage> = document.images.iter().flatten().collect();
        found.sort_by_key(|image| image.image_index);
        let pages: Vec<PageText<'_>> = document
            .pages
            .iter()
            .flatten()
            .map(|page| PageText {
                number: page.page_number,
                content: &page.content,
            })
            .collect();
        let sites: Vec<Site<'_>> = found
            .iter()
            .map(|image| Site {
                page: image.page_number,
                description: image.description.as_deref(),
            })
            .collect();
        let offsets = anchors(&document.content, &pages, &sites);
        let mut kept = Vec::new();
        let mut skipped = ImagesSkipped::default();
        let mut total = 0_usize;
        let mut on_page: std::collections::BTreeMap<u32, usize> = std::collections::BTreeMap::new();
        for (image, text_offset) in found.into_iter().zip(offsets) {
            let Some(media_type) = sniff_image(&image.data).filter(|_| !image.is_mask) else {
                skipped.unsupported += 1;
                continue;
            };
            // The parser kept one picture more than the per-page cap on a
            // page that has more: that one is a picture dropped (the others
            // it did not return are not known, so this is a lower bound).
            let seen = on_page.entry(image.page_number.unwrap_or(0)).or_default();
            *seen += 1;
            if *seen > per_page_cap {
                skipped.too_many += 1;
                continue;
            }
            if kept.len() >= bounds.max_images {
                skipped.too_many += 1;
                continue;
            }
            if image.data.len() > bounds.max_image_bytes
                || total.saturating_add(image.data.len()) > bounds.max_total_bytes
            {
                skipped.too_large += 1;
                continue;
            }
            total += image.data.len();
            kept.push(EmbeddedImage {
                data: image.data.to_vec(),
                media_type,
                ordinal: image.image_index,
                text_offset,
                page: image.page_number,
                name: image
                    .source_path
                    .as_deref()
                    .and_then(|path| path.rsplit('/').next())
                    .filter(|name| !name.is_empty())
                    .map(str::to_owned),
            });
        }
        (kept, skipped)
    }

    #[cfg(test)]
    mod tests {
        use super::*;
        use xberg::XbergError;
        use xberg::extractors::security::SecurityError;

        #[test]
        fn the_page_limit_is_read_from_the_typed_source_not_the_sentence() {
            let over = XbergError::from(SecurityError::TooManyPages { count: 3, max: 2 });
            assert!(matches!(classify(&over), DocumentError::TooManyPages(_)));
            // Another security limit is "too large", whatever its sentence.
            let cells = XbergError::from(SecurityError::TooManyCells { cells: 9, max: 8 });
            assert!(matches!(classify(&cells), DocumentError::TooLarge(_)));
            // The sentence alone, with no typed source, is NOT trusted as the
            // page limit: it would be a reworded release's guess.
            let untyped = XbergError::Security {
                message: "Document has too many pages: 3 (max: 2)".to_owned(),
                source: None,
            };
            assert!(matches!(classify(&untyped), DocumentError::TooLarge(_)));
        }

        #[test]
        fn io_serialization_and_other_are_the_extractors_failure_not_the_documents() {
            let io = XbergError::Io(std::io::Error::other("no temp dir"));
            assert!(matches!(classify(&io), DocumentError::Extractor(_)), "{io}");
            let other = XbergError::Other("something uncommon".to_owned());
            assert!(matches!(classify(&other), DocumentError::Extractor(_)));
            let serialization = XbergError::Serialization {
                message: "msgpack".to_owned(),
                source: None,
            };
            assert!(matches!(
                classify(&serialization),
                DocumentError::Extractor(_)
            ));
            // The document's own faults stay the document's.
            let parsing = XbergError::Parsing {
                message: "bad xref".to_owned(),
                source: None,
            };
            assert!(matches!(classify(&parsing), DocumentError::Corrupt(_)));
        }

        #[test]
        fn a_picture_is_anchored_inline_when_the_text_names_it_else_at_the_end_of_its_page() {
            let text = "Before.\n\n[Image: Picture 1]\n\nBetween.\n\n[Image: a chart]\n\nAfter.";
            let named = |description| Site {
                page: Some(1),
                description: Some(description),
            };
            let offsets = anchors(text, &[], &[named("Picture 1"), named("a chart")]);
            assert_eq!(&text[..offsets[0]], "Before.\n\n[Image: Picture 1]");
            assert_eq!(
                &text[..offsets[1]],
                "Before.\n\n[Image: Picture 1]\n\nBetween.\n\n[Image: a chart]"
            );
            // The same description twice is two lines, in order.
            let twice = "[Image: x]\n\nmid\n\n[Image: x]\n\nend";
            let offsets = anchors(twice, &[], &[named("x"), named("x")]);
            assert_eq!(&twice[..offsets[0]], "[Image: x]");
            assert_eq!(&twice[..offsets[1]], "[Image: x]\n\nmid\n\n[Image: x]");

            // Pages: the end of the page the picture is on, not of the text.
            let whole = "One a\n\nOne b\n\nTwo a\n\nTwo b";
            let pages = [
                PageText {
                    number: 1,
                    content: "One a\n\nOne b",
                },
                PageText {
                    number: 2,
                    content: "Two a\n\nTwo b",
                },
            ];
            let on = |page| Site {
                page: Some(page),
                description: None,
            };
            let offsets = anchors(whole, &pages, &[on(1), on(2), on(1)]);
            assert_eq!(&whole[..offsets[0]], "One a\n\nOne b");
            assert_eq!(offsets[1], whole.len());
            assert_eq!(offsets[2], offsets[0]);

            // A blank page, a page whose text cannot be found, and no page.
            let blank = [
                PageText {
                    number: 1,
                    content: "",
                },
                PageText {
                    number: 2,
                    content: "never in the text",
                },
            ];
            let offsets = anchors(whole, &blank, &[on(1), on(2), on(3)]);
            assert_eq!(offsets, vec![0, whole.len(), whole.len()]);
            // A name the text does not carry falls back to the page's end.
            let offsets = anchors(whole, &pages, &[named("nowhere")]);
            assert_eq!(&whole[..offsets[0]], "One a\n\nOne b");
            // Multi-byte text: every anchor is a character boundary.
            let wide = "é\n\n[Image: ü]\n\nz";
            let offsets = anchors(wide, &[], &[named("ü")]);
            assert!(wide.is_char_boundary(offsets[0]));
        }

        #[test]
        #[allow(clippy::cast_possible_truncation, reason = "tiny test sizes")]
        fn the_media_of_a_package_are_counted_from_its_directory_alone() {
            // One stored entry `word/media/a.png` (3 bytes) and the end record.
            let name = b"word/media/a.png";
            let mut zip = Vec::new();
            zip.extend_from_slice(&[0x50, 0x4b, 0x03, 0x04]);
            zip.extend_from_slice(&[0; 26]);
            zip.extend_from_slice(name);
            zip.extend_from_slice(b"abc");
            let directory = zip.len();
            zip.extend_from_slice(&[0x50, 0x4b, 0x01, 0x02]);
            let mut entry = [0_u8; 42];
            entry[20..24].copy_from_slice(&3_u32.to_le_bytes());
            entry[24..26].copy_from_slice(&(name.len() as u16).to_le_bytes());
            zip.extend_from_slice(&entry);
            zip.extend_from_slice(name);
            let size = zip.len() - directory;
            zip.extend_from_slice(&[0x50, 0x4b, 0x05, 0x06, 0, 0, 0, 0, 1, 0, 1, 0]);
            zip.extend_from_slice(&(size as u32).to_le_bytes());
            zip.extend_from_slice(&(directory as u32).to_le_bytes());
            zip.extend_from_slice(&[0, 0]);
            assert_eq!(scan_media(&zip), Some(Media { count: 1, bytes: 3 }));
            // Anything that is not a readable plain zip gives no scan.
            assert_eq!(scan_media(b""), None);
            assert_eq!(scan_media(b"not a zip at all, just text"), None);
            assert_eq!(scan_media(&zip[..zip.len() - 30]), None);
        }

        #[test]
        fn pictures_are_recognised_by_their_bytes_not_by_a_claim() {
            assert_eq!(sniff_image(b"\x89PNG\r\n\x1a\n...."), Some("image/png"));
            assert_eq!(sniff_image(&[0xff, 0xd8, 0xff, 0xe0]), Some("image/jpeg"));
            assert_eq!(sniff_image(b"GIF89a.."), Some("image/gif"));
            assert_eq!(sniff_image(b"RIFF\0\0\0\0WEBPVP8 "), Some("image/webp"));
            assert_eq!(sniff_image(b"<svg xmlns=\"x\"/>"), None);
            assert_eq!(sniff_image(b"II*\0 a tiff"), None);
            assert_eq!(sniff_image(b""), None);
        }

        #[test]
        fn the_cooperative_timeout_rounds_up_to_whole_seconds_and_never_under_one() {
            assert_eq!(
                cooperative_timeout(Duration::from_millis(1)),
                Duration::from_secs(1)
            );
            assert_eq!(cooperative_timeout(Duration::ZERO), Duration::from_secs(1));
            assert_eq!(
                cooperative_timeout(Duration::from_millis(1500)),
                Duration::from_secs(2)
            );
            assert_eq!(
                cooperative_timeout(Duration::from_secs(2)),
                Duration::from_secs(2)
            );
            assert_eq!(
                cooperative_timeout(Duration::from_millis(2001)),
                Duration::from_secs(3)
            );
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
        // An image is never text; the vision step classifies it separately.
        assert_eq!(
            image_for_vision("image/png", b"\x89PNG\r\n\x1a\n...."),
            VisionImage::Supported("image/png")
        );
        assert_eq!(
            image_for_vision("image/png", b"GIF89a.."),
            VisionImage::Supported("image/gif")
        );
        assert_eq!(
            image_for_vision("IMAGE/JPEG; q=1", &[0xff, 0xd8, 0xff, 0xe0]),
            VisionImage::Supported("image/jpeg")
        );
        assert_eq!(
            image_for_vision("image/webp", b"RIFF\0\0\0\0WEBPVP8 "),
            VisionImage::Supported("image/webp")
        );
        assert_eq!(
            image_for_vision("image/webp", b"not an image"),
            VisionImage::Corrupt
        );
        let mut bmp = b"BM".to_vec();
        bmp.extend_from_slice(&[0; 12]);
        bmp.extend_from_slice(&40_u32.to_le_bytes());
        bmp.extend_from_slice(&[0; 8]);
        assert_eq!(image_for_vision("image/bmp", &bmp), VisionImage::Bmp);
        assert_eq!(image_for_vision("image/x-ms-bmp", &bmp), VisionImage::Bmp);
        assert_eq!(image_for_vision("image/png", &bmp), VisionImage::Bmp);
        assert_eq!(
            image_for_vision("image/bmp", b"BM not a bitmap at all"),
            VisionImage::Corrupt
        );
        assert_eq!(
            image_for_vision("image/x-png", b"\x89PNG\r\n\x1a\n.."),
            VisionImage::Supported("image/png")
        );
        assert_eq!(
            image_for_vision("image/jpg", &[0xff, 0xd8, 0xff, 0xdb]),
            VisionImage::Supported("image/jpeg")
        );
        assert_eq!(
            image_for_vision("image/tiff", b"II*\0"),
            VisionImage::Unsupported
        );
        assert_eq!(
            image_for_vision("image/svg+xml", b"<svg/>"),
            VisionImage::Unsupported
        );
        assert_eq!(
            image_for_vision("text/plain", b"x"),
            VisionImage::NotAnImage
        );
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

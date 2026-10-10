//! A document's bytes as text (ADR-0028 D4).
//!
//! * A text media type is decoded as UTF-8 first. Bytes that are not UTF-8
//!   (and are not binary: no NUL byte) are decoded with the encoding
//!   `chardetng` guesses, through `encoding_rs`, as the SDK's text loaders'
//!   `autodetect_encoding` does; a decode that needed replacement characters
//!   is refused rather than indexed damaged (ADR-0030 decision 4).
//! * HTML (`text/html`, `application/xhtml+xml`) is converted to markdown
//!   with `html-to-markdown-rs`, as the SDK's HTML loader does, so headings
//!   and lists survive for the markdown chunker.
//! * Any other format is extracted by a document extractor when this crate
//!   is built with the `documents` feature, and reported unsupported
//!   otherwise — never guessed at.
//!
//! Code is text here; its structure is the code parsers' job, not this
//! crate's.

use elitea_content_source::is_text;

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

/// Media types converted from HTML to markdown.
fn is_html(mime: &str) -> bool {
    matches!(mime, "text/html" | "application/xhtml+xml")
}

/// Whether this build turns `mime` into text.
#[must_use]
pub fn can_extract(mime: &str) -> bool {
    is_text(mime)
        || is_html(mime)
        || (cfg!(feature = "documents") && DOCUMENT_TYPES.contains(&mime))
}

/// `bytes` of media type `mime` as text.
#[must_use]
pub fn extract(mime: &str, bytes: &[u8]) -> Extracted {
    if is_html(mime) {
        return html(bytes);
    }
    if is_text(mime) {
        return match decode(bytes) {
            Ok((text, extractor)) => Extracted::Text { text, extractor },
            Err(reason) => Extracted::Unreadable(reason),
        };
    }
    if !can_extract(mime) {
        return Extracted::Unsupported;
    }
    documents::extract(mime, bytes)
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
    let mut detector = chardetng::EncodingDetector::new(chardetng::Iso2022JpDetection::Allow);
    detector.feed(bytes, true);
    let encoding = detector.guess(None, chardetng::Utf8Detection::Allow);
    let (text, _, malformed) = encoding.decode(bytes);
    if malformed {
        Err(format!(
            "the content is not UTF-8 and does not decode as {}",
            encoding.name()
        ))
    } else {
        Ok((text.into_owned(), "chardetng"))
    }
}

/// HTML as markdown. The page's own metadata and the cleanup pass are off,
/// as in the Confluence toolkit; a page with no text is unreadable.
fn html(bytes: &[u8]) -> Extracted {
    let source = match decode(bytes) {
        Ok((source, _)) => source,
        Err(reason) => return Extracted::Unreadable(reason),
    };
    let options = html_to_markdown_rs::ConversionOptions {
        extract_metadata: false,
        preprocessing: html_to_markdown_rs::PreprocessingOptions {
            enabled: false,
            ..html_to_markdown_rs::PreprocessingOptions::default()
        },
        ..html_to_markdown_rs::ConversionOptions::default()
    };
    match html_to_markdown_rs::convert(&source, options) {
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

    use super::Extracted;

    /// Seconds one document may take.
    const TIMEOUT_SECONDS: u64 = 120;
    /// Pages read from one document (where the format has pages).
    const MAX_PAGES: usize = 1000;
    /// Bytes of content one document may hold.
    const MAX_CONTENT_BYTES: usize = 50 * 1024 * 1024;
    /// The extraction thread's stack.
    const STACK_BYTES: usize = 16 * 1024 * 1024;

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
    fn html_becomes_markdown() {
        let page = b"<html><head><title>T</title></head><body><h1>Refunds</h1>\
            <p>Within <strong>thirty</strong> days.</p><ul><li>Billing</li><li>Support</li></ul>\
            <script>alert(1)</script></body></html>";
        for mime in ["text/html", "application/xhtml+xml"] {
            match extract(mime, page) {
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
        match extract("text/html", latin) {
            Extracted::Text { text, .. } => assert!(text.contains("café"), "{text}"),
            other => panic!("{other:?}"),
        }
        assert!(matches!(
            extract("text/html", b"<html><body></body></html>"),
            Extracted::Unreadable(_)
        ));
    }
}

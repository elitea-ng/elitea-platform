//! A document's bytes as text (ADR-0028 D4).
//!
//! * A text media type is decoded as UTF-8, strictly: a file that is not
//!   UTF-8 is not text the engines can cite by line, and the Python engine
//!   (whose provider read decoded strictly) skipped it too.
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

/// Whether this build turns `mime` into text.
#[must_use]
pub fn can_extract(mime: &str) -> bool {
    is_text(mime) || (cfg!(feature = "documents") && DOCUMENT_TYPES.contains(&mime))
}

/// `bytes` of media type `mime` as text.
#[must_use]
pub fn extract(mime: &str, bytes: &[u8]) -> Extracted {
    if is_text(mime) {
        return match String::from_utf8(bytes.to_vec()) {
            Ok(text) => Extracted::Text {
                text,
                extractor: "utf8",
            },
            Err(_) => Extracted::Unreadable("the content is not UTF-8".to_owned()),
        };
    }
    if !can_extract(mime) {
        return Extracted::Unsupported;
    }
    documents::extract(mime, bytes)
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
    fn text_is_decoded_strictly_and_other_formats_are_not_guessed() {
        assert_eq!(
            extract("text/markdown", b"# hi\n"),
            Extracted::Text {
                text: "# hi\n".to_owned(),
                extractor: "utf8"
            }
        );
        assert!(matches!(
            extract("text/plain", b"caf\xe9"),
            Extracted::Unreadable(_)
        ));
        assert_eq!(extract("image/png", b"\x89PNG"), Extracted::Unsupported);
        assert!(can_extract("application/json"));
        assert_eq!(can_extract("application/pdf"), cfg!(feature = "documents"));
    }
}

//! The pictures a connector's own documents point at (ADR-0030 I4, PR-C2).
//!
//! The SDK's page and issue loaders describe the pictures a document links
//! (`process_images`, `bins_with_llm`) with the chat model and write the
//! description into the document's text. A connector here cannot call the
//! model, and the worker cannot fetch from a provider: the connector reads
//! the pictures through its own authenticated, egress-guarded transport and
//! hands the worker a [`PictureBatch`]; the worker describes them and calls
//! [`PictureBatch::render`], which writes the descriptions in.
//!
//! The batch holds a TEMPLATE of the document in which every picture that
//! was read stands as a token, so a description lands exactly where the
//! picture was (a picture that could not be read keeps its original markup).
//! A document the worker never asks for pictures (no chat model, the step
//! off) is the connector's plain `fetch`, unchanged.

use std::collections::BTreeMap;

/// Pictures read for one document, at most.
pub const MAX_PICTURES: usize = 20;
/// Occurrences of pictures in one document that are given a picture, at
/// most (the SDK's bound for work items; the same for every connector).
pub const MAX_OCCURRENCES: usize = 200;
/// Bytes of one picture, at most.
pub const MAX_PICTURE_BYTES: u64 = 5 << 20;
/// Bytes of all of a document's pictures together, at most.
pub const MAX_TOTAL_PICTURE_BYTES: u64 = 12 << 20;

/// The metadata key under which a connector hands the worker what its
/// picture pass needs from the fetch; the run removes it before anything is
/// stored.
pub const CONTEXT_KEY: &str = "elitea.picture_context";

/// Skip reasons, the worker's closed labels.
pub const FETCH_FAILED: &str = "fetch_failed";
/// A reference that is not a picture this connector can read (an external
/// link, a file that is not an image).
pub const UNSUPPORTED: &str = "unsupported_type";
pub const TOO_LARGE: &str = "too_large";
/// A reference to a file that is not there.
pub const UNREADABLE: &str = "unreadable";
pub const TOO_MANY: &str = "too_many_pictures";
/// The provider refused the credential (401, 403): another run does not cure it.
pub const FORBIDDEN: &str = "forbidden";

/// What a connector asks of the worker for a document's pictures.
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum PictureStep {
    /// Describe the pictures the document's text links ([`PictureBatch`]).
    /// `prompt` replaces the default prompt of the picture when given (a
    /// per-picture prompt of [`SourcePicture`] still wins).
    Inline { prompt: Option<String> },
    /// The document IS the binary (an attachment): describe it with the
    /// vision step of these extensions, forced on, with this prompt.
    Binary {
        extensions: &'static [&'static str],
        prompt: String,
    },
}

/// One picture read for a document.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct SourcePicture {
    /// Where the template holds this picture.
    pub token: String,
    /// The picture's file name, for the log and for the prompt.
    pub name: String,
    /// Shared with every other occurrence of the same read.
    pub bytes: bytes::Bytes,
    /// This picture's whole prompt, when it differs from the document's.
    pub prompt: Option<String>,
    /// The text before and after the description.
    pub before: String,
    pub after: String,
    /// What stands in the text when the picture is not described.
    pub fallback: String,
    /// How the description is made fit for the text it goes into, in order.
    pub escapes: &'static [Escape],
}

/// A step in making a description fit the text around the picture.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Escape {
    /// Each line trimmed, blank lines dropped (HTML's `get_text` form).
    Lines,
    /// As the value of a double-quoted HTML attribute.
    HtmlAttribute,
    /// As the contents of a JSON string (the template is JSON text).
    Json,
    /// As the target of a markdown link: one line, `\`, `]` and `)` escaped.
    MarkdownLink,
}

/// A document with its pictures read.
#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct PictureBatch {
    /// The document's text with a token where each of `pictures` goes and
    /// each of `fallbacks` stood.
    pub template: String,
    pub pictures: Vec<SourcePicture>,
    /// Pictures that could not be read: their token and the text that
    /// stands for them.
    pub fallbacks: Vec<(String, String)>,
    /// Pictures not read, by reason.
    pub skipped: BTreeMap<&'static str, u32>,
}

impl PictureBatch {
    /// The document's text with each description written in. `described[i]`
    /// is the description of `pictures[i]`; `None` leaves the picture's
    /// `fallback`.
    #[must_use]
    pub fn render(&self, described: &[Option<&str>]) -> String {
        let mut text = self.template.clone();
        for (token, fallback) in &self.fallbacks {
            text = text.replace(token.as_str(), fallback);
        }
        for (index, picture) in self.pictures.iter().enumerate() {
            let replacement = match described.get(index).copied().flatten() {
                Some(description) => {
                    let body = picture
                        .escapes
                        .iter()
                        .fold(description.to_owned(), |text, escape| escape.apply(&text));
                    format!("{}{body}{}", picture.before, picture.after)
                }
                None => picture.fallback.clone(),
            };
            text = text.replace(picture.token.as_str(), &replacement);
        }
        text
    }

    /// A batch with no picture: the document as it is and one picture
    /// that could not be read, for `reason` (a retryable one marks the
    /// document for another run).
    #[must_use]
    pub fn unread(text: &str, reason: &'static str) -> Self {
        let mut batch = Self {
            template: text.to_owned(),
            ..Self::default()
        };
        batch.skip(reason);
        batch
    }

    /// [`Self::unread`] with several skips.
    #[must_use]
    pub fn with_skips(text: String, skipped: BTreeMap<&'static str, u32>) -> Self {
        Self {
            template: text,
            skipped,
            ..Self::default()
        }
    }

    pub fn skip(&mut self, reason: &'static str) {
        *self.skipped.entry(reason).or_default() += 1;
    }
}

impl Escape {
    fn apply(self, text: &str) -> String {
        match self {
            Self::Lines => text
                .lines()
                .map(str::trim)
                .filter(|line| !line.is_empty())
                .collect::<Vec<_>>()
                .join("\n"),
            Self::HtmlAttribute => text
                .replace('&', "&amp;")
                .replace('<', "&lt;")
                .replace('>', "&gt;")
                .replace('"', "&quot;"),
            Self::Json => json_contents(text),
            Self::MarkdownLink => text
                .split_whitespace()
                .collect::<Vec<_>>()
                .join(" ")
                .replace('\\', "\\\\")
                .replace(']', "\\]")
                .replace(')', "\\)"),
        }
    }
}

/// `text` as the inside of a JSON string.
#[must_use]
pub fn json_contents(text: &str) -> String {
    let quoted = serde_json::Value::String(text.to_owned()).to_string();
    quoted
        .strip_prefix('"')
        .and_then(|inner| inner.strip_suffix('"'))
        .unwrap_or_default()
        .to_owned()
}

/// The skip reason of a failed picture read. Only what another run can
/// cure (a 5xx, 408, 429, a timeout or a broken connection) is
/// [`FETCH_FAILED`], which marks the document for a retry; a missing file,
/// a refused credential, a request this runtime will not send (egress,
/// another origin) and a body that is no picture are final.
#[must_use]
pub fn skip_reason(failure: &crate::source::Failure) -> &'static str {
    use crate::source::Failure;
    use crate::transport::StatusCode;
    match failure {
        Failure::TooLarge(_) => TOO_LARGE,
        Failure::Status(StatusCode::UNAUTHORIZED | StatusCode::FORBIDDEN) => FORBIDDEN,
        Failure::Status(status)
            if status.is_server_error()
                || *status == StatusCode::TOO_MANY_REQUESTS
                || *status == StatusCode::REQUEST_TIMEOUT =>
        {
            FETCH_FAILED
        }
        Failure::Transport(_) | Failure::RateLimited => FETCH_FAILED,
        Failure::Refused | Failure::Client(_) => UNSUPPORTED,
        Failure::Status(_) | Failure::Changed | Failure::InvalidResponse(_) => UNREADABLE,
    }
}

/// The token for the `index`-th picture of a template: private-use
/// characters no provider text carries.
#[must_use]
pub fn token(index: usize) -> String {
    format!("\u{F8FF}elitea-picture-{index}\u{F8FE}")
}

/// Whether `text` already holds a token (then no template can be built from
/// it: a token could not be told from the text).
#[must_use]
pub fn holds_token(text: &str) -> bool {
    text.contains("\u{F8FF}elitea-picture-")
}

/// The distinct reads of one document's pictures, keyed by the connector's
/// own source identity (a Jira attachment id, a wiki path after
/// normalisation, a boards `src`). Each is downloaded once, held once
/// (occurrences clone the cheap [`bytes::Bytes`] handle) and charged against
/// the document's budget once; a failure is counted once. Every connector
/// reads through this, so they all behave alike.
#[derive(Debug, Default)]
pub struct Reads {
    budget: Budget,
    known: BTreeMap<String, Option<bytes::Bytes>>,
    skipped: BTreeMap<&'static str, u32>,
}

impl Reads {
    /// The bytes of the picture `identity`, downloading it with `read` the
    /// first time only. `None`: it could not be read (counted once, under the
    /// reason `read` gave, or the budget's).
    pub async fn get<F, Fut>(&mut self, identity: &str, read: F) -> Option<bytes::Bytes>
    where
        F: FnOnce() -> Fut,
        Fut: std::future::Future<Output = Result<Vec<u8>, &'static str>>,
    {
        if let Some(known) = self.known.get(identity) {
            return known.clone();
        }
        let outcome = match self.budget.may_read() {
            Err(reason) => Err(reason),
            Ok(()) => match read().await {
                Ok(data) => self.budget.take(data.len() as u64).map(|()| data),
                Err(reason) => Err(reason),
            },
        };
        let bytes = match outcome {
            Ok(data) => Some(bytes::Bytes::from(data)),
            Err(reason) => {
                self.skip(reason);
                None
            }
        };
        self.known.insert(identity.to_owned(), bytes.clone());
        bytes
    }

    /// Counts a reference that was not a picture to read.
    pub fn skip(&mut self, reason: &'static str) {
        *self.skipped.entry(reason).or_default() += 1;
    }

    /// How many distinct pictures were read.
    #[must_use]
    pub fn read_count(&self) -> usize {
        self.known.values().flatten().count()
    }

    /// Whether anything was counted.
    #[must_use]
    pub fn has_skips(&self) -> bool {
        !self.skipped.is_empty()
    }

    /// The counts, for the batch.
    #[must_use]
    pub fn into_skipped(self) -> BTreeMap<&'static str, u32> {
        self.skipped
    }
}

/// The bounds one document's pictures are read under.
#[derive(Debug, Default)]
pub struct Budget {
    count: usize,
    total: u64,
}

impl Budget {
    /// Whether another picture may be read: a count bound, checked before
    /// the request.
    ///
    /// # Errors
    ///
    /// The count bound is reached.
    pub fn may_read(&self) -> Result<(), &'static str> {
        if self.count >= MAX_PICTURES {
            return Err(TOO_MANY);
        }
        Ok(())
    }

    /// Takes the `len` bytes a picture came to.
    ///
    /// # Errors
    ///
    /// The picture or the document's total is over its bound.
    pub fn take(&mut self, len: u64) -> Result<(), &'static str> {
        if len > MAX_PICTURE_BYTES || self.total.saturating_add(len) > MAX_TOTAL_PICTURE_BYTES {
            return Err(TOO_LARGE);
        }
        self.count += 1;
        self.total += len;
        Ok(())
    }
}

/// `%XX` escapes decoded (invalid ones kept, bytes that are not UTF-8
/// replaced), as Python's `unquote`.
#[must_use]
pub fn percent_decode(text: &str) -> String {
    percent_encoding::percent_decode_str(text)
        .decode_utf8_lossy()
        .into_owned()
}

/// Whether `name` ends in an extension an image model takes by name
/// (the SDK's wiki allow-list).
#[must_use]
pub fn has_image_extension(name: &str) -> bool {
    let path = name.split(['?', '#']).next().unwrap_or_default();
    let lower = path.to_ascii_lowercase();
    [
        ".png", ".jpg", ".jpeg", ".gif", ".bmp", ".webp", ".svg", ".tiff", ".tif", ".ico",
    ]
    .iter()
    .any(|extension| lower.ends_with(extension))
}

#[cfg(test)]
mod tests {
    use super::*;

    fn picture(index: usize, json: bool) -> SourcePicture {
        SourcePicture {
            token: token(index),
            name: format!("p{index}.png"),
            bytes: bytes::Bytes::from_static(&[1]),
            prompt: None,
            before: "[image: ".to_owned(),
            after: "]".to_owned(),
            fallback: "ORIGINAL".to_owned(),
            escapes: if json { &[Escape::Json] } else { &[] },
        }
    }

    #[test]
    fn render_writes_descriptions_falls_back_and_escapes_json() {
        let batch = PictureBatch {
            template: format!(
                "a {} b {} c {} d {}",
                token(0),
                token(1),
                token(0),
                token(2)
            ),
            pictures: vec![picture(0, false), picture(1, true)],
            fallbacks: vec![(token(2), "GONE".to_owned())],
            skipped: BTreeMap::new(),
        };
        assert_eq!(
            batch.render(&[Some("x \"q\"\nz"), None]),
            "a [image: x \"q\"\nz] b ORIGINAL c [image: x \"q\"\nz] d GONE"
        );
        assert_eq!(
            batch.render(&[None, Some("x \"q\"\nz")]),
            "a ORIGINAL b [image: x \\\"q\\\"\\nz] c ORIGINAL d GONE"
        );
    }

    #[test]
    fn escapes_make_the_description_fit_its_place() {
        assert_eq!(Escape::Lines.apply(" a \n\n  b\n"), "a\nb");
        assert_eq!(
            Escape::HtmlAttribute.apply("a<\"b\">&"),
            "a&lt;&quot;b&quot;&gt;&amp;"
        );
    }

    #[test]
    fn a_description_is_one_line_with_the_link_closers_escaped() {
        assert_eq!(
            Escape::MarkdownLink.apply("A (chart)\n\nof [x] \\ ok"),
            "A (chart\\) of [x\\] \\\\ ok"
        );
    }

    #[tokio::test]
    async fn a_picture_is_downloaded_held_and_charged_once() {
        let calls = std::cell::Cell::new(0);
        let mut reads = Reads::default();
        let big = || async {
            calls.set(calls.get() + 1);
            Ok(vec![1_u8; usize::try_from(MAX_PICTURE_BYTES).unwrap()])
        };
        let first = reads.get("a", big).await.expect("read");
        let second = reads.get("a", big).await.expect("again");
        let third = reads.get("a", big).await.expect("and again");
        assert_eq!(calls.get(), 1, "one download");
        assert_eq!(first.as_ptr(), third.as_ptr(), "one buffer");
        assert_eq!(second.len(), first.len());
        // Charged once: two more distinct 5 MiB pictures fit the 12 MiB total
        // only once more, so the third distinct picture is refused, not the
        // repeats of the first.
        reads
            .get("b", || async { Ok(vec![2_u8; 5 << 20]) })
            .await
            .expect("b");
        assert!(
            reads
                .get("c", || async { Ok(vec![3_u8; 5 << 20]) })
                .await
                .is_none()
        );
        assert!(
            reads.get("a", big).await.is_some(),
            "a repeat is never refused"
        );
        assert_eq!(reads.read_count(), 2);
        assert_eq!(reads.into_skipped().get(TOO_LARGE), Some(&1));
    }

    #[tokio::test]
    async fn a_failed_picture_is_counted_once() {
        let mut reads = Reads::default();
        for _ in 0..3 {
            assert!(
                reads
                    .get("x", || async { Err(FETCH_FAILED) })
                    .await
                    .is_none()
            );
        }
        assert_eq!(reads.into_skipped().get(FETCH_FAILED), Some(&1));
    }

    #[test]
    fn the_budget_bounds_count_each_picture_and_the_total() {
        let mut budget = Budget::default();
        assert_eq!(budget.take(MAX_PICTURE_BYTES + 1), Err(TOO_LARGE));
        for _ in 0..2 {
            budget.take(MAX_PICTURE_BYTES).expect("within");
        }
        assert_eq!(
            budget.take(MAX_PICTURE_BYTES),
            Err(TOO_LARGE),
            "total 15 MiB"
        );
        let mut budget = Budget::default();
        for _ in 0..MAX_PICTURES {
            budget.may_read().expect("room");
            budget.take(1).expect("within");
        }
        assert_eq!(budget.may_read(), Err(TOO_MANY));
    }

    #[test]
    fn only_what_another_run_cures_is_a_retryable_read_failure() {
        use crate::source::Failure;
        use crate::transport::StatusCode;
        let reason = |f: Failure| skip_reason(&f);
        assert_eq!(
            reason(Failure::Status(StatusCode::BAD_GATEWAY)),
            FETCH_FAILED
        );
        assert_eq!(
            reason(Failure::Status(StatusCode::TOO_MANY_REQUESTS)),
            FETCH_FAILED
        );
        assert_eq!(reason(Failure::Transport("timed out".into())), FETCH_FAILED);
        assert_eq!(reason(Failure::RateLimited), FETCH_FAILED);
        assert_eq!(reason(Failure::Status(StatusCode::UNAUTHORIZED)), FORBIDDEN);
        assert_eq!(reason(Failure::Status(StatusCode::FORBIDDEN)), FORBIDDEN);
        assert_eq!(reason(Failure::Status(StatusCode::NOT_FOUND)), UNREADABLE);
        assert_eq!(reason(Failure::Status(StatusCode::BAD_REQUEST)), UNREADABLE);
        assert_eq!(reason(Failure::Refused), UNSUPPORTED);
        assert_eq!(reason(Failure::Client("other origin".into())), UNSUPPORTED);
        assert_eq!(reason(Failure::InvalidResponse("html")), UNREADABLE);
        assert_eq!(reason(Failure::TooLarge(1)), TOO_LARGE);
    }

    #[test]
    fn percent_decoding_and_extensions() {
        assert_eq!(percent_decode("a%20b%2Fc%zz%4"), "a b/c%zz%4");
        assert_eq!(percent_decode("caf%C3%A9"), "café");
        assert_eq!(percent_decode("a%FFb"), "a\u{FFFD}b");
        assert!(has_image_extension(".attachments/A%20b.PNG?x=1"));
        assert!(!has_image_extension(".attachments/doc.pdf"));
    }
}

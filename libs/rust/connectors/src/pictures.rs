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
/// Bytes of one picture, at most.
pub const MAX_PICTURE_BYTES: u64 = 5 << 20;
/// Bytes of all of a document's pictures together, at most.
pub const MAX_TOTAL_PICTURE_BYTES: u64 = 12 << 20;

/// Skip reasons, the worker's closed labels.
pub const FETCH_FAILED: &str = "fetch_failed";
/// A reference that is not a picture this connector can read (an external
/// link, a file that is not an image).
pub const UNSUPPORTED: &str = "unsupported_type";
pub const TOO_LARGE: &str = "too_large";
/// A reference to a file that is not there.
pub const UNREADABLE: &str = "unreadable";
pub const TOO_MANY: &str = "too_many_pictures";

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
    pub bytes: Vec<u8>,
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

/// `%XX` escapes decoded (invalid ones kept), as Python's `unquote`.
#[must_use]
pub fn percent_decode(text: &str) -> String {
    let bytes = text.as_bytes();
    let mut out = Vec::with_capacity(bytes.len());
    let mut index = 0;
    while index < bytes.len() {
        if bytes[index] == b'%'
            && index + 2 < bytes.len()
            && let (Some(high), Some(low)) = (hex(bytes[index + 1]), hex(bytes[index + 2]))
        {
            out.push(high * 16 + low);
            index += 3;
            continue;
        }
        out.push(bytes[index]);
        index += 1;
    }
    String::from_utf8_lossy(&out).into_owned()
}

fn hex(byte: u8) -> Option<u8> {
    match byte {
        b'0'..=b'9' => Some(byte - b'0'),
        b'a'..=b'f' => Some(byte - b'a' + 10),
        b'A'..=b'F' => Some(byte - b'A' + 10),
        _ => None,
    }
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
            bytes: vec![1],
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
    fn percent_decoding_and_extensions() {
        assert_eq!(percent_decode("a%20b%2Fc%zz%4"), "a b/c%zz%4");
        assert_eq!(percent_decode("caf%C3%A9"), "café");
        assert!(has_image_extension(".attachments/A%20b.PNG?x=1"));
        assert!(!has_image_extension(".attachments/doc.pdf"));
    }
}

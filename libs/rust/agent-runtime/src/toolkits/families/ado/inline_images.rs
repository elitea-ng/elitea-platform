//! Inline image description for the ADO read tools (#985): `get_comments`,
//! `get_work_item` and the wiki page reads (the SDK's
//! `_embed_comment_image_descriptions`, `_patch_image_descriptions` and the
//! wiki's `_process_images`).
//!
//! This module finds the image references in HTML (`<img src>`) and markdown
//! (`![alt](url)`), describes each distinct reference once within the
//! per-call bounds, and writes the result back. Each family supplies how it
//! reads one picture. One picture that fails gives a short note beside that
//! reference; it never fails the tool call.

use std::collections::BTreeMap;
use std::future::Future;
use std::sync::LazyLock;

use regex::{Captures, Regex};

use serde_json::Value;

use crate::toolkits::families::ado::client::{AdoClientError, MAX_OUTPUT_BYTES, bounded_output};
use crate::toolkits::families::ado::pictures::describe_picture;
use crate::toolkits::families::attachment_host::{
    AttachmentHost, MAX_PICTURES, PictureTally, stops_every_later_picture,
};

/// The SDK's `_MARKDOWN_IMAGE_PATTERN` (work item comments).
#[allow(
    clippy::expect_used,
    reason = "a constant pattern; the tests exercise it"
)]
static MARKDOWN_IMAGE: LazyLock<Regex> = LazyLock::new(|| {
    Regex::new(r#"!\[(.*?)\]\(\s*([^)\s]+)(?:\s+"[^"]*")?\s*\)"#).expect("markdown image pattern")
});
/// The wiki's `!\[(.*?)\]\((.*?)\)`.
#[allow(
    clippy::expect_used,
    reason = "a constant pattern; the tests exercise it"
)]
static WIKI_IMAGE: LazyLock<Regex> =
    LazyLock::new(|| Regex::new(r"!\[(.*?)\]\((.*?)\)").expect("wiki image pattern"));
/// One `<img ...>` tag.
#[allow(
    clippy::expect_used,
    reason = "a constant pattern; the tests exercise it"
)]
static IMG_TAG: LazyLock<Regex> =
    LazyLock::new(|| Regex::new(r"(?is)<img\b[^>]*>").expect("img tag pattern"));
/// The `src` attribute of one tag: double-quoted, single-quoted or bare.
#[allow(
    clippy::expect_used,
    reason = "a constant pattern; the tests exercise it"
)]
static SRC_ATTRIBUTE: LazyLock<Regex> = LazyLock::new(|| {
    Regex::new(r#"(?is)\ssrc\s*=\s*(?:"([^"]*)"|'([^']*)'|([^\s"'>]+))"#).expect("src pattern")
});
#[allow(
    clippy::expect_used,
    reason = "a constant pattern; the tests exercise it"
)]
static DESCRIPTION_ATTRIBUTE: LazyLock<Regex> =
    LazyLock::new(|| Regex::new(r"(?is)\simage-description\s*=").expect("description pattern"));

/// What one reference becomes.
#[derive(Clone, Debug, Eq, PartialEq)]
pub(crate) enum Annotation {
    /// The model's description.
    Description(String),
    /// A short note on why the picture was not described.
    Note(String),
}

impl Annotation {
    fn text(&self) -> &str {
        match self {
            Self::Description(text) | Self::Note(text) => text,
        }
    }
}

/// `[image unavailable: <reason>]`, with the reason's first letter in lower
/// case (the SDK's `_IMAGE_UNAVAILABLE`).
pub(crate) fn unavailable(reason: &str) -> Annotation {
    let mut chars = reason.chars();
    let reason = chars.next().map_or_else(String::new, |first| {
        first.to_lowercase().chain(chars).collect::<String>()
    });
    Annotation::Note(format!(
        "[image unavailable: {}]",
        reason.trim_end_matches('.')
    ))
}

/// The `src` of every `<img>` in `text`, in order.
pub(crate) fn html_image_sources(text: &str) -> Vec<String> {
    if !text.contains("<img") && !text.contains("<IMG") {
        return Vec::new();
    }
    IMG_TAG
        .find_iter(text)
        .filter_map(|tag| tag_source(tag.as_str()))
        .collect()
}

fn tag_source(tag: &str) -> Option<String> {
    let captures = SRC_ATTRIBUTE.captures(tag)?;
    let raw = captures
        .get(1)
        .or_else(|| captures.get(2))
        .or_else(|| captures.get(3))?
        .as_str();
    let source = html_escape::decode_html_entities(raw).trim().to_owned();
    (!source.is_empty()).then_some(source)
}

/// The URL of every markdown image in `text`, in order.
pub(crate) fn markdown_image_urls(text: &str) -> Vec<String> {
    if !text.contains("![") {
        return Vec::new();
    }
    MARKDOWN_IMAGE
        .captures_iter(text)
        .filter_map(|captures| captures.get(2).map(|url| url.as_str().to_owned()))
        .collect()
}

/// `(alt, url)` of every wiki image link in `text`, in order.
pub(crate) fn wiki_image_links(text: &str) -> Vec<(String, String)> {
    WIKI_IMAGE
        .captures_iter(text)
        .map(|captures| (captures[1].to_owned(), captures[2].to_owned()))
        .collect()
}

/// `text` with each annotated `<img>` given an `image-description`
/// attribute (the SDK sets it with `BeautifulSoup`), and each annotated
/// markdown image followed by an `[image-description: ...]` line. The image
/// URL stays, so the model can still call `get_image_by_url`.
pub(crate) fn annotate(text: &str, annotations: &BTreeMap<String, Annotation>) -> String {
    let mut text = text.to_owned();
    if text.contains("<img") || text.contains("<IMG") {
        text = IMG_TAG
            .replace_all(&text, |captures: &Captures<'_>| {
                let tag = &captures[0];
                let annotation = tag_source(tag).and_then(|source| annotations.get(&source));
                match annotation {
                    Some(annotation) if !DESCRIPTION_ATTRIBUTE.is_match(tag) => {
                        with_attribute(tag, annotation.text())
                    }
                    _ => tag.to_owned(),
                }
            })
            .into_owned();
    }
    if text.contains("![") {
        text = MARKDOWN_IMAGE
            .replace_all(&text, |captures: &Captures<'_>| {
                let reference = &captures[0];
                match annotations.get(&captures[2]) {
                    Some(annotation) => {
                        format!("{reference}\n[image-description: {}]", annotation.text())
                    }
                    None => reference.to_owned(),
                }
            })
            .into_owned();
    }
    text
}

/// The wiki's shape: a described link becomes `![alt](description)`; a
/// link with a note keeps its URL and is followed by the note.
pub(crate) fn annotate_wiki(text: &str, annotations: &BTreeMap<String, Annotation>) -> String {
    WIKI_IMAGE
        .replace_all(text, |captures: &Captures<'_>| {
            let reference = &captures[0];
            match annotations.get(&captures[2]) {
                Some(Annotation::Description(description)) => {
                    format!("![{}]({description})", &captures[1])
                }
                Some(Annotation::Note(note)) => format!("{reference}\n{note}"),
                None => reference.to_owned(),
            }
        })
        .into_owned()
}

fn with_attribute(tag: &str, value: &str) -> String {
    let escaped = html_escape::encode_double_quoted_attribute(value);
    let body = tag.trim_end_matches('>');
    let (body, close) = match body.strip_suffix('/') {
        Some(body) => (body.trim_end(), " />"),
        None => (body.trim_end(), ">"),
    };
    format!("{body} image-description=\"{escaped}\"{close}")
}

/// What one reference is, decided before any request: a picture to read
/// (`Fetch`), a reference that is not a picture this family reads (`Skip`:
/// it stays as it is), or a picture that is not read, with the reason.
pub(crate) enum Target<T> {
    Fetch(T),
    Skip,
    Unavailable(String),
}

/// Describe each distinct reference of `references` once, in order, within
/// the per-call bounds of [`PictureTally`]: at most [`MAX_PICTURES`]
/// pictures are read, failed reads included, and at most
/// [`MAX_TOTAL_PICTURE_BYTES`](crate::toolkits::families::attachment_host::MAX_TOTAL_PICTURE_BYTES)
/// bytes are described. A `data:` URI and a
/// skipped reference get no annotation. `target` decides what a reference
/// is without a request; `read` reads one picture through the family's own
/// authenticated client (`Err` is the reason, as a sentence).
pub(crate) async fn describe_references<T, P, F, Fut>(
    references: impl IntoIterator<Item = String>,
    host: &AttachmentHost,
    prompt: Option<&str>,
    target: P,
    read: F,
) -> BTreeMap<String, Annotation>
where
    P: Fn(&str) -> Target<T>,
    F: Fn(T) -> Fut,
    Fut: Future<Output = Result<(Vec<u8>, &'static str), String>>,
{
    let mut annotations = BTreeMap::new();
    let mut seen = std::collections::BTreeSet::new();
    let mut tally = PictureTally::default();
    for reference in references {
        if !seen.insert(reference.clone()) || reference.to_ascii_lowercase().starts_with("data:") {
            continue;
        }
        let picture = match target(&reference) {
            Target::Fetch(picture) => picture,
            Target::Skip => continue,
            Target::Unavailable(reason) => {
                annotations.insert(reference, unavailable(&reason));
                continue;
            }
        };
        // A failure every later picture would meet (no budget, no model):
        // ask no more, and say why beside each later picture.
        if let Some(error) = tally.stopped_by() {
            annotations.insert(reference, unavailable(&error.sentence()));
            continue;
        }
        if tally.refusal(None).is_some() {
            annotations.insert(
                reference,
                Annotation::Note(format!(
                    "[image skipped: per-call image limit of {MAX_PICTURES} reached]"
                )),
            );
            continue;
        }
        // Count the read before it is sent: a read that fails also uses one
        // of the call's pictures.
        tally.attempt();
        let (bytes, media_type) = match read(picture).await {
            Ok(picture) => picture,
            Err(reason) => {
                annotations.insert(reference, unavailable(&reason));
                continue;
            }
        };
        if !tally.take(bytes.len() as u64) {
            annotations.insert(reference, budget_note());
            continue;
        }
        let annotation = match describe_picture(host, bytes, media_type, prompt).await {
            Ok(description) => Annotation::Description(description),
            Err(error) => {
                let note = unavailable(&error.sentence());
                if stops_every_later_picture(&error) {
                    tally.stop(error);
                }
                note
            }
        };
        annotations.insert(reference, annotation);
    }
    annotations
}

fn budget_note() -> Annotation {
    Annotation::Note("[image skipped: per-call download budget reached]".to_owned())
}

/// The note a result carries when its descriptions are dropped for size.
pub(crate) const DESCRIPTIONS_DROPPED: &str =
    "[image descriptions dropped: the result with them exceeds the tool's output limit]";

/// The described result when it fits one tool result. When it does not,
/// the result without descriptions (`plain`), with a short note that says
/// so: the descriptions must not make a result fail that fits without them.
pub(crate) fn bounded_described(plain: Value, described: Value) -> Result<Value, AdoClientError> {
    if fits(&described) {
        return Ok(described);
    }
    tracing::debug!(
        event = "ado_image_descriptions_dropped",
        limit = MAX_OUTPUT_BYTES,
        "the described result exceeds the output limit; it is returned without descriptions"
    );
    let noted = with_dropped_note(plain.clone());
    if fits(&noted) {
        return Ok(noted);
    }
    bounded_output(plain)
}

fn fits(value: &Value) -> bool {
    serde_json::to_vec(value).is_ok_and(|encoded| encoded.len() <= MAX_OUTPUT_BYTES)
}

/// `value` with [`DESCRIPTIONS_DROPPED`]: a line under a text, a key of an
/// object, a last item of a list.
fn with_dropped_note(value: Value) -> Value {
    match value {
        Value::String(mut text) => {
            text.push_str("\n\n");
            text.push_str(DESCRIPTIONS_DROPPED);
            Value::String(text)
        }
        Value::Object(mut object) => {
            object.insert(
                "image_descriptions".to_owned(),
                Value::String(DESCRIPTIONS_DROPPED.to_owned()),
            );
            Value::Object(object)
        }
        Value::Array(mut items) => {
            items.push(Value::String(DESCRIPTIONS_DROPPED.to_owned()));
            Value::Array(items)
        }
        other => other,
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn described(pairs: &[(&str, &str)]) -> BTreeMap<String, Annotation> {
        pairs
            .iter()
            .map(|(url, text)| {
                (
                    (*url).to_owned(),
                    Annotation::Description((*text).to_owned()),
                )
            })
            .collect()
    }

    #[test]
    fn img_sources_are_found_in_every_quoting_and_unescaped() {
        let html = r#"<p>a<img src="https://x/a.png?fileName=a.png&amp;v=1">b<IMG alt=x src='b.png'/><img src=c.png></p>"#;
        assert_eq!(
            html_image_sources(html),
            ["https://x/a.png?fileName=a.png&v=1", "b.png", "c.png"]
        );
        assert_eq!(
            html_image_sources("<p>no picture</p>"),
            Vec::<String>::new()
        );
    }

    #[test]
    fn an_annotated_img_gets_an_escaped_attribute_and_others_stay() {
        let html =
            r#"<img src="a.png"><img src="b.png" /><img src="c.png" image-description="kept">"#;
        let out = annotate(
            html,
            &described(&[
                ("a.png", "a \"quoted\" <tag> & more"),
                ("b.png", "bee"),
                ("c.png", "x"),
            ]),
        );
        assert_eq!(
            out,
            r#"<img src="a.png" image-description="a &quot;quoted&quot; &lt;tag&gt; &amp; more"><img src="b.png" image-description="bee" /><img src="c.png" image-description="kept">"#
        );
    }

    #[test]
    fn a_markdown_image_keeps_its_url_and_gains_a_description_line() {
        let text = "see ![shot](https://x/a.png \"title\") and ![other](b.png)";
        assert_eq!(markdown_image_urls(text), ["https://x/a.png", "b.png"]);
        assert_eq!(
            annotate(text, &described(&[("https://x/a.png", "a cat")])),
            "see ![shot](https://x/a.png \"title\")\n[image-description: a cat] and ![other](b.png)"
        );
    }

    #[test]
    fn the_wiki_shape_puts_the_description_in_place_of_the_url() {
        let text = "![s.png](/.attachments/s.png)\n![b.png](/.attachments/b.png)\n![d.pdf](/.attachments/d.pdf)";
        let mut annotations = described(&[("/.attachments/s.png", "a chart")]);
        annotations.insert(
            "/.attachments/b.png".to_owned(),
            unavailable("Could not read image dimensions"),
        );
        assert_eq!(
            annotate_wiki(text, &annotations),
            "![s.png](a chart)\n![b.png](/.attachments/b.png)\n[image unavailable: could not read image dimensions]\n![d.pdf](/.attachments/d.pdf)"
        );
        assert_eq!(wiki_image_links(text).len(), 3);
    }
}

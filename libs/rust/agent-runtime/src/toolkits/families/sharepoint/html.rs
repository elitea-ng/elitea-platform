//! The markup of a `OneNote` page, read the way the SDK's `html.parser`
//! readers read it: a forgiving tokenizer (no tree, no validation) that
//! finds the text, the `<img>` and the `<object data-attachment>` elements
//! of a page in document order.
//!
//! The page text is not a faithful rendering: block-level tags become line
//! breaks, `head`, `script` and `style` are dropped, and character
//! references are decoded. That is exactly the SDK's `onenote_read_page`
//! text, which agents were written against.

use html_escape::decode_html_entities;

/// Tags whose content is not page text.
const SKIPPED: [&str; 3] = ["script", "style", "head"];
/// Tags that end a line.
const BLOCKS: [&str; 11] = [
    "p", "div", "h1", "h2", "h3", "h4", "h5", "h6", "li", "br", "tr",
];

/// One `<img>` of a page.
#[derive(Clone, Debug, Eq, PartialEq)]
pub(super) struct ImageRef {
    pub(super) src: String,
    pub(super) alt: String,
}

/// One `<object data-attachment=…>` of a page.
#[derive(Clone, Debug, Eq, PartialEq)]
pub(super) struct AttachmentRef {
    pub(super) name: String,
    pub(super) src: String,
}

/// One piece of a page, in document order.
#[derive(Clone, Debug, Eq, PartialEq)]
pub(super) enum Chunk {
    Text(String),
    Image(usize),
    Attachment(usize),
}

/// A page read as text, images and attachments.
#[derive(Debug, Default)]
pub(super) struct Page {
    pub(super) chunks: Vec<Chunk>,
    pub(super) images: Vec<ImageRef>,
    pub(super) attachments: Vec<AttachmentRef>,
}

enum Event {
    Start {
        name: String,
        attributes: Vec<(String, String)>,
    },
    End(String),
    Text(String),
}

/// The page's text, images and attachments in document order. A `td` and a
/// `th` are not line breaks (the SDK lists neither).
pub(super) fn parse_page(html: &str) -> Page {
    let mut page = Page::default();
    let mut skip_depth = 0_usize;
    for event in events(html) {
        match event {
            Event::Start { name, attributes } => {
                if SKIPPED.contains(&name.as_str()) {
                    skip_depth += 1;
                } else if skip_depth == 0 {
                    match name.as_str() {
                        "img" => {
                            page.images.push(ImageRef {
                                src: attribute(&attributes, "src"),
                                alt: attribute(&attributes, "alt"),
                            });
                            page.chunks.push(Chunk::Image(page.images.len() - 1));
                        }
                        "object" => {
                            let attachment = attribute(&attributes, "data-attachment");
                            if !attachment.is_empty() {
                                page.attachments.push(AttachmentRef {
                                    name: attachment,
                                    src: attribute(&attributes, "data"),
                                });
                                page.chunks
                                    .push(Chunk::Attachment(page.attachments.len() - 1));
                            }
                        }
                        other if is_block(other) => page.chunks.push(Chunk::Text("\n".to_owned())),
                        _ => {}
                    }
                }
            }
            Event::End(name) => {
                if SKIPPED.contains(&name.as_str()) {
                    skip_depth = skip_depth.saturating_sub(1);
                } else if skip_depth == 0 && is_block(&name) {
                    page.chunks.push(Chunk::Text("\n".to_owned()));
                }
            }
            Event::Text(text) => {
                if skip_depth == 0 {
                    page.chunks.push(Chunk::Text(text));
                }
            }
        }
    }
    page
}

/// Every `<object data-attachment=…>` of the markup, wherever it is (the
/// SDK's attachment listing does not skip `head`).
pub(super) fn attachments_of(html: &str) -> Vec<AttachmentRef> {
    events(html)
        .into_iter()
        .filter_map(|event| match event {
            Event::Start { name, attributes } if name == "object" => {
                let attachment = attribute(&attributes, "data-attachment");
                (!attachment.is_empty()).then(|| AttachmentRef {
                    name: attachment,
                    src: attribute(&attributes, "data"),
                })
            }
            _ => None,
        })
        .collect()
}

/// The page's text blocks, as the SDK joins them: the chunks between two
/// non-text items, with runs of three or more line breaks cut to two.
pub(super) fn flush_text(buffer: &mut String) -> Option<String> {
    if buffer.is_empty() {
        return None;
    }
    let collapsed = collapse_line_breaks(buffer);
    buffer.clear();
    let text = collapsed.trim();
    (!text.is_empty()).then(|| text.to_owned())
}

fn collapse_line_breaks(text: &str) -> String {
    let mut out = String::with_capacity(text.len());
    let mut run = 0_usize;
    for character in text.chars() {
        if character == '\n' {
            run += 1;
            if run <= 2 {
                out.push('\n');
            }
        } else {
            run = 0;
            out.push(character);
        }
    }
    out
}

fn is_block(name: &str) -> bool {
    BLOCKS.contains(&name) || matches!(name, "td" | "th")
}

fn attribute(attributes: &[(String, String)], name: &str) -> String {
    attributes
        .iter()
        .find(|(candidate, _)| candidate == name)
        .map(|(_, value)| value.clone())
        .unwrap_or_default()
}

/// The tokens of `html`. A start tag that ends `/>` is a start and an end.
fn events(html: &str) -> Vec<Event> {
    let bytes = html.as_bytes();
    let mut events = Vec::new();
    let mut at = 0_usize;
    let mut text_start = 0_usize;
    while at < bytes.len() {
        if bytes[at] != b'<' {
            at += 1;
            continue;
        }
        let next = bytes.get(at + 1).copied().unwrap_or(0);
        let opens_tag = next.is_ascii_alphabetic();
        let closes_tag = next == b'/' && bytes.get(at + 2).is_some_and(u8::is_ascii_alphabetic);
        if opens_tag || closes_tag {
            push_text(&mut events, &html[text_start..at]);
            let end = tag_end(bytes, at);
            let tag = &html[at + 1..end.min(html.len())];
            if closes_tag {
                events.push(Event::End(tag_name(&tag[1..])));
            } else {
                let name = tag_name(tag);
                let (attributes, self_closing) = attributes_of(tag, name.len());
                let raw_text = matches!(name.as_str(), "script" | "style") && !self_closing;
                events.push(Event::Start {
                    name: name.clone(),
                    attributes,
                });
                if self_closing {
                    events.push(Event::End(name.clone()));
                }
                if raw_text {
                    let after = end + 1;
                    let close =
                        find_ignore_case(&html[after.min(html.len())..], &format!("</{name}"))
                            .map_or(html.len(), |offset| after + offset);
                    at = close;
                    text_start = close;
                    continue;
                }
            }
            at = end + 1;
            text_start = at;
        } else if html[at..].starts_with("<!--") {
            push_text(&mut events, &html[text_start..at]);
            let end = html[at + 4..]
                .find("-->")
                .map_or(html.len(), |i| at + 4 + i + 3);
            at = end;
            text_start = at;
        } else if next == b'!' || next == b'?' {
            push_text(&mut events, &html[text_start..at]);
            let end = html[at..].find('>').map_or(html.len(), |i| at + i + 1);
            at = end;
            text_start = at;
        } else {
            // A `<` that opens nothing is text.
            at += 1;
        }
    }
    if text_start < html.len() {
        push_text(&mut events, &html[text_start..]);
    }
    events
}

fn push_text(events: &mut Vec<Event>, raw: &str) {
    if !raw.is_empty() {
        events.push(Event::Text(decode_html_entities(raw).into_owned()));
    }
}

/// The index of the `>` that closes the tag opened at `start`, quotes
/// respected; the end of the input when it never closes.
fn tag_end(bytes: &[u8], start: usize) -> usize {
    let mut quote = 0_u8;
    let mut at = start + 1;
    while at < bytes.len() {
        let byte = bytes[at];
        if quote != 0 {
            if byte == quote {
                quote = 0;
            }
        } else if byte == b'"' || byte == b'\'' {
            quote = byte;
        } else if byte == b'>' {
            return at;
        }
        at += 1;
    }
    bytes.len()
}

fn tag_name(tag: &str) -> String {
    tag.chars()
        .take_while(|c| !c.is_whitespace() && *c != '/' && *c != '>')
        .collect::<String>()
        .to_ascii_lowercase()
}

/// The attributes of a start tag whose name is `name_len` bytes long, and
/// whether it ends `/>`.
fn attributes_of(tag: &str, name_len: usize) -> (Vec<(String, String)>, bool) {
    let self_closing = tag.trim_end().ends_with('/');
    let mut rest = tag[name_len.min(tag.len())..].trim_end_matches('/');
    let mut attributes = Vec::new();
    loop {
        rest = rest.trim_start_matches(|c: char| c.is_whitespace() || c == '/');
        if rest.is_empty() {
            break;
        }
        let name_end = rest
            .find(|c: char| c.is_whitespace() || c == '=' || c == '/')
            .unwrap_or(rest.len());
        let name = rest[..name_end].to_ascii_lowercase();
        rest = rest[name_end..].trim_start();
        let mut value = String::new();
        if let Some(after_equals) = rest.strip_prefix('=') {
            let after_equals = after_equals.trim_start();
            let (raw, remainder) = if let Some(quote @ ('"' | '\'')) = after_equals.chars().next() {
                let inner = &after_equals[1..];
                match inner.find(quote) {
                    Some(close) => (&inner[..close], &inner[close + 1..]),
                    None => (inner, ""),
                }
            } else {
                let end = after_equals
                    .find(char::is_whitespace)
                    .unwrap_or(after_equals.len());
                (&after_equals[..end], &after_equals[end..])
            };
            value = decode_html_entities(raw).into_owned();
            rest = remainder;
        }
        if !name.is_empty() && !attributes.iter().any(|(existing, _)| *existing == name) {
            attributes.push((name, value));
        }
    }
    (attributes, self_closing)
}

/// The first index of `needle` in `haystack`, ASCII case ignored, scanning
/// only as far as it must (no copy of the rest of the page).
fn find_ignore_case(haystack: &str, needle: &str) -> Option<usize> {
    let (haystack, needle) = (haystack.as_bytes(), needle.as_bytes());
    if needle.is_empty() || haystack.len() < needle.len() {
        return None;
    }
    (0..=haystack.len() - needle.len())
        .find(|&start| haystack[start..start + needle.len()].eq_ignore_ascii_case(needle))
}

/// The resource id of an address a page carries, only when it is a Graph
/// `OneNote` resource of the site `site_id`:
/// `https://graph.microsoft.com/v1.0/sites/<site>/onenote/resources/<id>[/$value|/content]`.
/// A picture on a CDN, another site's resource, a look-alike host or any
/// other address is not one, and is never fetched with the person's token.
pub(super) fn resource_id(src: &str, site_id: &str) -> Option<String> {
    let url = reqwest::Url::parse(src).ok()?;
    if url.scheme() != "https"
        || url.host_str() != Some("graph.microsoft.com")
        || url.port().is_some()
        || !url.username().is_empty()
        || url.password().is_some()
    {
        return None;
    }
    let segments = url.path_segments()?.collect::<Vec<_>>();
    if segments.len() < 6
        || !matches!(segments[0], "v1.0" | "beta")
        || segments[1] != "sites"
        || segments[3] != "onenote"
        || segments[4] != "resources"
    {
        return None;
    }
    let decode = |segment: &str| {
        percent_encoding::percent_decode_str(segment)
            .decode_utf8()
            .ok()
            .map(std::borrow::Cow::into_owned)
    };
    let site = decode(segments[2])?;
    // `host,collection,web` or the `collection,web` pair a page may carry.
    let same_site = site == site_id
        || site_id
            .split_once(',')
            .is_some_and(|(_, rest)| rest == site);
    let id = decode(segments[5])?;
    (same_site && !id.is_empty()).then_some(id)
}

#[cfg(test)]
mod tests {
    use super::*;

    const SITE: &str = "tenant.sharepoint.com,site-id,web-id";

    #[test]
    fn page_text_follows_the_sdk_chunker() {
        let html = r#"<!DOCTYPE html><html lang="en"><head><title>Hidden</title><meta name="created" content="x"/></head>
<body><div><p id="p1">Hello &amp; <b>world</b></p><p>Second<br/>line</p>
<img src="https://graph.microsoft.com/v1.0/sites/tenant.sharepoint.com,site-id,web-id/onenote/resources/0-abc!1-X/$value" alt="a chart" />
<p><object data-attachment="plan.pdf" data="https://graph.microsoft.com/v1.0/sites/tenant.sharepoint.com,site-id,web-id/onenote/resources/res-1/$value" type="application/pdf"></object></p>
<script>var a = "<p>no</p>";</script><p>Last &lt;one&gt;</p></div></body></html>"#;
        let page = parse_page(html);
        assert_eq!(page.images.len(), 1);
        assert_eq!(page.images[0].alt, "a chart");
        assert_eq!(
            resource_id(&page.images[0].src, SITE).as_deref(),
            Some("0-abc!1-X")
        );
        assert_eq!(page.attachments[0].name, "plan.pdf");
        assert_eq!(
            resource_id(&page.attachments[0].src, SITE).as_deref(),
            Some("res-1")
        );

        let mut text = String::new();
        let mut blocks = Vec::new();
        for chunk in &page.chunks {
            match chunk {
                Chunk::Text(piece) => text.push_str(piece),
                Chunk::Image(_) | Chunk::Attachment(_) => {
                    blocks.extend(flush_text(&mut text));
                    blocks.push("<item>".to_owned());
                }
            }
        }
        blocks.extend(flush_text(&mut text));
        assert_eq!(
            blocks,
            [
                "Hello & world\n\nSecond\n\nline",
                "<item>",
                "<item>",
                "Last <one>"
            ]
        );
    }

    #[test]
    fn the_attachment_list_does_not_skip_the_head_and_needs_a_name() {
        let html = r#"<head><object data-attachment="in-head.txt" data="x"></object></head>
<object data="no-name"></object><object data-attachment="" data="y"/>
<object data-attachment='quoted name.docx' data=unquoted></object>"#;
        let found = attachments_of(html);
        assert_eq!(
            found
                .iter()
                .map(|a| (a.name.as_str(), a.src.as_str()))
                .collect::<Vec<_>>(),
            [("in-head.txt", "x"), ("quoted name.docx", "unquoted")]
        );
    }

    #[test]
    fn markup_that_is_not_markup_stays_text() {
        let page = parse_page("a < b and 1<2 <p>x</p> &#65;&nbsp;&bogus;");
        let joined = page
            .chunks
            .iter()
            .filter_map(|c| match c {
                Chunk::Text(t) => Some(t.as_str()),
                _ => None,
            })
            .collect::<String>();
        assert_eq!(joined, "a < b and 1<2 \nx\n A\u{a0}&bogus;");
    }

    #[test]
    fn an_unclosed_tag_or_comment_ends_the_input_without_panicking() {
        for html in [
            "<p",
            "<p class=\"x",
            "<!-- open",
            "<script>never closed",
            "<",
            "</",
            "<img src=",
        ] {
            drop(parse_page(html));
            drop(attachments_of(html));
        }
    }

    #[test]
    fn thousands_of_script_tags_are_read_in_linear_time() {
        let html = "<script>x</script>".repeat(30_000);
        let started = std::time::Instant::now();
        let page = parse_page(&html);
        assert!(page.chunks.is_empty());
        assert!(
            started.elapsed() < std::time::Duration::from_secs(2),
            "quadratic scan: {:?}",
            started.elapsed()
        );
    }

    #[test]
    fn script_content_is_raw_text_up_to_its_own_close_in_any_case() {
        let page = parse_page("<SCRIPT>if (a<b) {}</ScRiPt><p>after</p>");
        let text = page
            .chunks
            .iter()
            .filter_map(|c| match c {
                Chunk::Text(t) => Some(t.as_str()),
                _ => None,
            })
            .collect::<String>();
        assert_eq!(text, "\nafter\n");
    }

    #[test]
    fn only_a_graph_onenote_resource_of_this_site_is_a_resource() {
        let base = "https://graph.microsoft.com/v1.0/sites";
        for (src, expected) in [
            (
                format!("{base}/{SITE}/onenote/resources/a-1/$value"),
                Some("a-1"),
            ),
            (
                format!("{base}/{SITE}/onenote/resources/a-1/content"),
                Some("a-1"),
            ),
            (format!("{base}/{SITE}/onenote/resources/a-1"), Some("a-1")),
            // The site as a page may spell it: encoded commas, or the
            // collection/web pair without the host.
            (
                format!(
                    "{base}/tenant.sharepoint.com%2Csite-id%2Cweb-id/onenote/resources/a-1/$value"
                ),
                Some("a-1"),
            ),
            (
                format!("{base}/site-id,web-id/onenote/resources/a-1/$value"),
                Some("a-1"),
            ),
            (
                format!("{base}/{SITE}/onenote/resources/0-abc%211-X/$value"),
                Some("0-abc!1-X"),
            ),
            // Another site, a picture on a CDN, a look-alike host, plain
            // http, a port, credentials, a different API surface.
            (
                format!("{base}/other-site,web/onenote/resources/a-1/$value"),
                None,
            ),
            (
                "https://cdn.example.com/files/resources/a-1/$value".to_owned(),
                None,
            ),
            (
                format!(
                    "https://graph.microsoft.com.evil.example/v1.0/sites/{SITE}/onenote/resources/a-1"
                ),
                None,
            ),
            (
                format!("http://graph.microsoft.com/v1.0/sites/{SITE}/onenote/resources/a-1"),
                None,
            ),
            (
                format!("https://graph.microsoft.com:8443/v1.0/sites/{SITE}/onenote/resources/a-1"),
                None,
            ),
            (
                format!("https://u:p@graph.microsoft.com/v1.0/sites/{SITE}/onenote/resources/a-1"),
                None,
            ),
            (
                "https://graph.microsoft.com/v1.0/users/me/onenote/resources/a-1".to_string(),
                None,
            ),
            (format!("{base}/{SITE}/drive/resources/a-1"), None),
            (format!("{base}/{SITE}/onenote/resources/"), None),
            ("x/resources/a-1/$value".to_owned(), None),
            ("https://example.org/pic.png".to_owned(), None),
        ] {
            assert_eq!(resource_id(&src, SITE).as_deref(), expected, "{src}");
        }
    }
}

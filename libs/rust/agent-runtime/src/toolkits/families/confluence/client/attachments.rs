//! The Confluence attachment tools: `get_page_with_image_descriptions`,
//! `get_page_attachments` and `add_file_to_page`, ported from the SDK's
//! `ConfluenceAPIWrapper` (`elitea_sdk/tools/confluence/api_wrapper.py`).
//!
//! Files go in and out through the run's [`AttachmentHost`] (the artifact
//! store and the chat model); Confluence is reached only through this
//! family's own authenticated transport. A picture or attachment is fetched
//! from the configured Confluence origin only; Cloud's redirect to its media
//! host is followed without the credential (see [`ConfluenceClient::fetch`]).
//!
//! Known differences from the SDK, stated where they matter:
//! * `get_page_attachments` reads text, JSON, XML, Markdown, SVG, CSV and
//!   HTML attachments, and describes pictures with the run's model. Word and
//!   Excel attachments, PDF page analysis and draw.io analysis need libraries
//!   this runtime does not carry: those entries say so in `content` or
//!   `llm_analysis` instead of the extracted text. HTML is reduced to its text
//!   by a plain tag stripper, not `BeautifulSoup`.
//! * The SDK asks for each attachment's comments through a client method that
//!   does not exist, so its `comments` list is always empty; so is this one.
//! * A caption and a file name placed in the page's storage markup are
//!   XML-escaped here; the SDK writes them raw.
//! * A picture is described when the model gateway takes its type (PNG, JPEG,
//!   GIF, WebP).

use std::collections::{HashMap, HashSet};

use base64::Engine as _;
use fancy_regex::Regex as PythonRegex;
use regex::Regex;
use serde_json::{Value, json};

use super::*;
use crate::toolkits::families::attachment_host::{
    AttachmentError, MAX_ATTACH_BYTES, MAX_PICTURE_BYTES, PictureTally, Step, describe_for_text,
    multipart_upload, picture_prompt, upload_name_is_valid,
};
use crate::toolkits::families::office_host::OfficeFailure;

/// `ConfluenceAPIWrapper._get_default_image_analysis_prompt`, verbatim (indentation included).
const DEFAULT_IMAGE_PROMPT: &str = "\n        ## Image Analysis Task:\n        Analyze this image in detail, paying special attention to contextual information provided about it.\n        Focus on:\n        1. Visual elements and their arrangement\n        2. Any text visible in the image\n        3. UI components if it's a screenshot\n        4. Diagrams, charts, or technical illustrations\n        5. The overall context and purpose of the image\n\n        ## Instructions:\n        - Begin your description by referencing the image name provided\n        - Provide a comprehensive description that could substitute for the image\n        - Incorporate the surrounding content in your analysis to provide contextual relevance\n        - Explain how this image relates to the content being discussed\n        - Format your response clearly with appropriate paragraph breaks\n        - Make it short\n        ";

/// Redirects one download follows (Cloud: one, to its media host).
const MAX_REDIRECTS: usize = 5;
const ATLASSIAN_MEDIA_HOST: &str = "api.media.atlassian.com";
/// Listing pages `get_page_attachments` walks at most.
const MAX_LISTING_PAGES: usize = 20;
/// The largest text attachment `get_page_attachments` reads.
const MAX_TEXT_ATTACHMENT_BYTES: usize = 10 * 1024 * 1024;
/// `get_page_attachments` returns at most this many characters per attachment.
pub(in crate::toolkits) const MAX_CONTENT_LENGTH: i64 = 100_000;
/// ... and processes at most this many attachments.
pub(in crate::toolkits) const MAX_ATTACHMENTS: i64 = 100;
/// Macros that render attachments wholesale (`ATTACHMENT_LISTING_MACROS`).
const ATTACHMENT_LISTING_MACROS: [&str; 4] = [
    "attachments",
    "gallery",
    "space-attachments",
    "recently-updated",
];
/// Macro parameters that name an attached file (`FILENAME_MACRO_PARAMS`).
const FILENAME_MACRO_PARAMS: [&str; 3] = ["name", "filename", "file"];
const NO_MODEL_TEXT: &str = "[LLM not available for image processing]";
/// Media types the SDK reads as text (`is_text_type`).
const TEXT_MEDIA_TYPES: [&str; 25] = [
    "application/xml",
    "text/xml",
    "application/json",
    "text/json",
    "text/markdown",
    "text/x-markdown",
    "text/plain",
    "text/csv",
    "text/html",
    "image/svg+xml",
    "application/vnd.ms-excel",
    "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
    "application/msword",
    "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
    "application/vnd.ms-excel.sheet.macroEnabled.12",
    "application/csv",
    "application/x-csv",
    "text/x-csv",
    "application/doc",
    "application/docx",
    "application/xls",
    "application/xlsx",
    "application/svg",
    "application/html",
    "application/octet-stream",
];
const TEXT_EXTENSIONS: [&str; 13] = [
    "xml", "json", "md", "markdown", "txt", "csv", "xls", "xlsx", "svg", "html", "htm", "doc",
    "docx",
];

fn refuse(message: String) -> ConfluenceClientError {
    ConfluenceClientError::described(
        ConfluenceClientErrorCode::InvalidInput,
        ErrorCategory::InvalidInput,
        "confluence.attachment.refused",
        message,
    )
}

fn attachment_failure(error: AttachmentError) -> ConfluenceClientError {
    let code = match &error {
        AttachmentError::Host(_, OfficeFailure::Timeout) => ConfluenceClientErrorCode::Timeout,
        AttachmentError::Host(_, OfficeFailure::RateLimited | OfficeFailure::BudgetRefused) => {
            ConfluenceClientErrorCode::RateLimited
        }
        AttachmentError::Host(
            _,
            OfficeFailure::Unreachable | OfficeFailure::NoModel | OfficeFailure::ReplyUnusable,
        ) => ConfluenceClientErrorCode::DependencyUnavailable,
        _ => ConfluenceClientErrorCode::InvalidInput,
    };
    ConfluenceClientError {
        code,
        retryable: false,
        detail: Some(Box::new(error.into_adk())),
    }
}

/// `str.lower().split('.')[-1] if '.' in title else ''`.
/// `path` without the leading `prefix` (a path segment such as `/wiki`),
/// only when the prefix is a whole segment: `/wiki/x` and `/wiki` lose it,
/// `/wikifoo/x` does not.
fn strip_path_prefix<'a>(path: &'a str, prefix: &str) -> &'a str {
    if prefix.is_empty() {
        return path;
    }
    match path.strip_prefix(prefix) {
        Some(rest) if rest.is_empty() || rest.starts_with('/') => rest,
        _ => path,
    }
}

fn extension_of(title: &str) -> String {
    if title.contains('.') {
        title
            .to_lowercase()
            .rsplit('.')
            .next()
            .unwrap_or_default()
            .to_owned()
    } else {
        String::new()
    }
}

/// The media type of an upload: `filetype.guess` on the bytes, else the
/// SDK's name fallback (`_upload_file_from_artifact`).
fn upload_media_type(bytes: &[u8], name: &str) -> &'static str {
    let named = |extensions: &[&str]| {
        std::path::Path::new(name)
            .extension()
            .and_then(std::ffi::OsStr::to_str)
            .is_some_and(|found| extensions.iter().any(|ext| found.eq_ignore_ascii_case(ext)))
    };
    if bytes.starts_with(&[0x89, b'P', b'N', b'G', 0x0D, 0x0A, 0x1A, 0x0A]) {
        "image/png"
    } else if bytes.starts_with(&[0xFF, 0xD8, 0xFF]) {
        "image/jpeg"
    } else if bytes.starts_with(b"GIF87a") || bytes.starts_with(b"GIF89a") {
        "image/gif"
    } else if bytes.len() >= 12 && &bytes[..4] == b"RIFF" && &bytes[8..12] == b"WEBP" {
        "image/webp"
    } else if bytes.starts_with(b"BM") && bytes.len() > 14 && named(&["bmp"]) {
        "image/bmp"
    } else if bytes.starts_with(b"%PDF-") {
        "application/pdf"
    } else if bytes.starts_with(b"PK\x03\x04") {
        "application/zip"
    } else if bytes.len() >= 12 && &bytes[4..8] == b"ftyp" {
        "video/mp4"
    } else if bytes.starts_with(&[0x1A, 0x45, 0xDF, 0xA3]) {
        "video/webm"
    } else if named(&["jpg", "jpeg"]) {
        "image/jpeg"
    } else if named(&["png"]) {
        "image/png"
    } else if named(&["gif"]) {
        "image/gif"
    } else if named(&["pdf"]) {
        "application/pdf"
    } else {
        "application/octet-stream"
    }
}

fn file_type(media_type: &str) -> &'static str {
    if media_type.starts_with("image/") {
        "image"
    } else if media_type.starts_with("video/") {
        "video"
    } else {
        "file"
    }
}

fn xml_escape(value: &str) -> String {
    let mut out = String::with_capacity(value.len());
    for character in value.chars() {
        match character {
            '&' => out.push_str("&amp;"),
            '<' => out.push_str("&lt;"),
            '>' => out.push_str("&gt;"),
            '"' => out.push_str("&quot;"),
            other => out.push(other),
        }
    }
    out
}

/// `str[:n]` in characters.
fn clip(text: &str, limit: usize) -> &str {
    match text.char_indices().nth(limit) {
        Some((index, _)) => &text[..index],
        None => text,
    }
}

fn truncated(text: &str, limit: usize) -> String {
    if text.chars().count() > limit {
        format!(
            "{}\n...[truncated, showing first {limit} characters]",
            clip(text, limit)
        )
    } else {
        text.to_owned()
    }
}

/// `_collect_context_for_image` of the Confluence wrapper, quirks included
/// (a marker that is not found takes the text from the start).
fn collect_context(content: &str, marker: &str, radius: i64) -> String {
    let to_i64 = |count: usize| i64::try_from(count).unwrap_or(i64::MAX);
    let radius = radius.max(0);
    let chars: Vec<char> = content.chars().collect();
    let found = content
        .find(marker)
        .map_or(-1, |byte| to_i64(content[..byte].chars().count()));
    let length = to_i64(chars.len());
    let start = (found - radius).max(0);
    let end = (found + to_i64(marker.chars().count()) + radius).min(length);
    let slice: String = if start < end {
        chars[usize::try_from(start).unwrap_or(0)..usize::try_from(end).unwrap_or(0)]
            .iter()
            .collect()
    } else {
        String::new()
    };
    slice.split_whitespace().collect::<Vec<_>>().join(" ")
}

/// A Python `csv.reader` row set, rendered as `', '.join(row)` per line.
fn csv_text(content: &str) -> String {
    let mut rows: Vec<Vec<String>> = Vec::new();
    let mut row: Vec<String> = Vec::new();
    let mut field = String::new();
    let mut quoted = false;
    let mut was_quoted = false;
    let mut chars = content.chars().peekable();
    let mut started = false;
    while let Some(character) = chars.next() {
        started = true;
        if quoted {
            if character == '"' {
                if chars.peek() == Some(&'"') {
                    chars.next();
                    field.push('"');
                } else {
                    quoted = false;
                }
            } else {
                field.push(character);
            }
            continue;
        }
        match character {
            '"' if field.is_empty() && !was_quoted => {
                quoted = true;
                was_quoted = true;
            }
            ',' => {
                row.push(std::mem::take(&mut field));
                was_quoted = false;
            }
            '\r' | '\n' => {
                if character == '\r' && chars.peek() == Some(&'\n') {
                    chars.next();
                }
                row.push(std::mem::take(&mut field));
                was_quoted = false;
                rows.push(std::mem::take(&mut row));
                started = false;
            }
            other => field.push(other),
        }
    }
    if started {
        row.push(field);
        rows.push(row);
    }
    // `csv.reader` yields `[]` for an empty line, which joins to "".
    rows.into_iter()
        .map(|row| {
            if row.len() == 1 && row[0].is_empty() {
                String::new()
            } else {
                row.join(", ")
            }
        })
        .collect::<Vec<_>>()
        .join("\n")
}

/// `BeautifulSoup(html).get_text(separator=' ', strip=True)`, approximated:
/// the text nodes outside `script`, `style` and comments, each stripped,
/// joined by a space.
fn html_text(html: &str) -> String {
    let mut parts: Vec<String> = Vec::new();
    let mut rest = html;
    let mut text = String::new();
    let flush = |text: &mut String, parts: &mut Vec<String>| {
        let decoded = decode_entities(text);
        let stripped = decoded.trim();
        if !stripped.is_empty() {
            parts.push(stripped.to_owned());
        }
        text.clear();
    };
    while !rest.is_empty() {
        if let Some(after) = rest.strip_prefix("<!--") {
            flush(&mut text, &mut parts);
            rest = after.find("-->").map_or("", |end| &after[end + 3..]);
        } else if rest.starts_with('<')
            && rest[1..]
                .chars()
                .next()
                .is_some_and(|c| c.is_ascii_alphabetic() || c == '/' || c == '!')
        {
            flush(&mut text, &mut parts);
            let Some(end) = rest.find('>') else {
                break;
            };
            let tag = rest[1..end].trim_start_matches('/').to_ascii_lowercase();
            let name = tag
                .split(|c: char| c.is_whitespace() || c == '/')
                .next()
                .unwrap_or_default()
                .to_owned();
            let is_open = !rest[1..].starts_with('/');
            rest = &rest[end + 1..];
            if is_open && (name == "script" || name == "style") {
                let close = format!("</{name}");
                let lower = rest.to_ascii_lowercase();
                rest = lower
                    .find(&close)
                    .and_then(|at| rest[at..].find('>').map(|end| &rest[at + end + 1..]))
                    .unwrap_or("");
            }
        } else {
            let next = rest[1..].find('<').map_or(rest.len(), |at| at + 1);
            text.push_str(&rest[..next]);
            rest = &rest[next..];
        }
    }
    flush(&mut text, &mut parts);
    parts.join(" ")
}

fn decode_entities(text: &str) -> String {
    let mut out = String::with_capacity(text.len());
    let mut rest = text;
    while let Some(at) = rest.find('&') {
        out.push_str(&rest[..at]);
        let tail = &rest[at..];
        let decoded = tail.find(';').filter(|end| *end <= 10).and_then(|end| {
            let entity = &tail[1..end];
            let character = match entity {
                "amp" => Some('&'),
                "lt" => Some('<'),
                "gt" => Some('>'),
                "quot" => Some('"'),
                "apos" => Some('\''),
                "nbsp" => Some('\u{a0}'),
                _ => entity
                    .strip_prefix('#')
                    .and_then(|number| {
                        number.strip_prefix(['x', 'X']).map_or_else(
                            || number.parse().ok(),
                            |hex| u32::from_str_radix(hex, 16).ok(),
                        )
                    })
                    .and_then(char::from_u32),
            }?;
            Some((character, end + 1))
        });
        if let Some((character, length)) = decoded {
            out.push(character);
            rest = &tail[length..];
        } else {
            out.push('&');
            rest = &tail[1..];
        }
    }
    out.push_str(rest);
    out
}

/// The file ids and names a page body references (`_get_body_file_references`).
type BodyReferences = (HashSet<String>, HashSet<String>);

fn body_references_of(document: &Value) -> Option<BodyReferences> {
    if !document.get("content").is_some_and(truthy) {
        return None;
    }
    let mut ids = HashSet::new();
    let mut names = HashSet::new();
    let mut nodes = vec![document];
    while let Some(node) = nodes.pop() {
        let Some(object) = node.as_object() else {
            continue;
        };
        let attrs = object.get("attrs").and_then(Value::as_object);
        if attrs
            .and_then(|attrs| attrs.get("extensionKey"))
            .and_then(Value::as_str)
            .is_some_and(|key| ATTACHMENT_LISTING_MACROS.contains(&key))
        {
            return None;
        }
        if object
            .get("type")
            .map(display_value)
            .is_some_and(|kind| kind.starts_with("media"))
            && let Some(id) = attrs
                .and_then(|attrs| attrs.get("id"))
                .filter(|id| truthy(id))
        {
            ids.insert(display_value(id));
        }
        if let Some(params) = attrs
            .and_then(|attrs| attrs.get("parameters"))
            .and_then(|parameters| parameters.get("macroParams"))
            .and_then(Value::as_object)
        {
            for (name, parameter) in params {
                if FILENAME_MACRO_PARAMS.contains(&name.as_str())
                    && let Some(value) = parameter.get("value").and_then(Value::as_str)
                {
                    names.insert(value.to_owned());
                }
            }
        }
        if let Some(children) = object.get("content").and_then(Value::as_array) {
            nodes.extend(children);
        }
    }
    Some((ids, names))
}

fn visible_on_page(attachment: &Value, references: Option<&BodyReferences>) -> bool {
    let Some((ids, names)) = references else {
        return true;
    };
    let Some(file_id) = attachment
        .pointer("/extensions/fileId")
        .filter(|id| truthy(id))
    else {
        return true;
    };
    ids.contains(&display_value(file_id))
        || attachment
            .get("title")
            .and_then(Value::as_str)
            .is_some_and(|title| names.contains(title))
}

/// What the attachment walk found.
struct Selection {
    selected: Vec<Value>,
    total: usize,
    visible: usize,
    has_more: bool,
    truncated: bool,
}

/// One downloaded answer.
struct Fetched {
    status: StatusCode,
    body: Vec<u8>,
    media_type: Option<String>,
}

enum FetchFailure {
    /// A refused redirect or a transport failure.
    Refused,
    /// The body is over the caller's bound.
    TooLarge,
}

impl ConfluenceClient {
    fn host(&self) -> Result<&AttachmentHost, ConfluenceClientError> {
        self.attachments.as_ref().ok_or_else(|| {
            ConfluenceClientError::described(
                ConfluenceClientErrorCode::DependencyUnavailable,
                ErrorCategory::Unavailable,
                "confluence.attachments.unavailable",
                "This run lends the Confluence attachment tools no artifact store or model."
                    .to_owned(),
            )
        })
    }

    /// GET `start` (on this Confluence's origin) with the toolkit's
    /// credential. Cloud answers a download with a redirect to its media host,
    /// which is followed WITHOUT the credential or any custom header, only
    /// over https and never back to the Confluence origin; any other target
    /// is refused. The final answer is returned whatever its status.
    async fn fetch(&self, start: Url, limit: usize) -> Result<Fetched, FetchFailure> {
        let origin = self.config.api_root().origin();
        let mut url = start;
        let mut credentialed = true;
        let mut left_origin = false;
        for _ in 0..=MAX_REDIRECTS {
            let mut request = if credentialed {
                self.request(Method::GET, url.clone(), None)
                    .map_err(|_| FetchFailure::Refused)?
            } else {
                Request::new(Method::GET, url.clone())
            };
            request
                .headers_mut()
                .insert(ACCEPT, HeaderValue::from_static("*/*"));
            let response = self
                .transport
                .execute_bounded(request, false, limit)
                .await
                .map_err(|error| {
                    if error.code == ConfluenceClientErrorCode::ResourceExhausted {
                        FetchFailure::TooLarge
                    } else {
                        FetchFailure::Refused
                    }
                })?;
            if matches!(response.status.as_u16(), 301 | 302 | 303 | 307 | 308) {
                let target = response
                    .location
                    .as_deref()
                    .and_then(|location| url.join(location).ok())
                    .ok_or(FetchFailure::Refused)?;
                if target.scheme() != "https" {
                    return Err(FetchFailure::Refused);
                }
                if target.origin() == origin {
                    if left_origin {
                        return Err(FetchFailure::Refused);
                    }
                } else if target
                    .host_str()
                    .is_some_and(|host| host.eq_ignore_ascii_case(ATLASSIAN_MEDIA_HOST))
                {
                    credentialed = false;
                    left_origin = true;
                } else {
                    return Err(FetchFailure::Refused);
                }
                url = target;
                continue;
            }
            return Ok(Fetched {
                status: response.status,
                body: response.body,
                media_type: response.media_type,
            });
        }
        Err(FetchFailure::Refused)
    }

    /// The URL of a `_links.download` path (see [`Self::download_link`]),
    /// `None` when it leaves this origin.
    fn link_url(&self, link: &str) -> Option<Url> {
        let url = Url::parse(&self.download_link(link)).ok()?;
        let root = self.config.api_root();
        (url.origin() == root.origin() && url.username().is_empty() && url.fragment().is_none())
            .then_some(url)
    }

    /// The absolute address of a `_links.download` path: under the API root
    /// (Cloud: `.../wiki`), with that prefix exactly once.
    fn download_link(&self, link: &str) -> String {
        let api_root = self.config.api_root().as_str().trim_end_matches('/');
        let base = self.config.base_url().as_str().trim_end_matches('/');
        let prefix = api_root.strip_prefix(base).unwrap_or_default();
        if !prefix.is_empty() && link.starts_with(&format!("{prefix}/")) {
            format!("{base}{link}")
        } else {
            format!("{api_root}{link}")
        }
    }

    /// A path under the API root, from a link Confluence gave or a page
    /// referenced; `None` when it leaves this origin.
    fn file_url(&self, relative: &str) -> Option<Url> {
        let root = self.config.api_root();
        let url = Url::parse(&format!(
            "{}/{}",
            root.as_str().trim_end_matches('/'),
            relative.trim_start_matches('/')
        ))
        .ok()?;
        (url.origin() == root.origin() && url.username().is_empty() && url.fragment().is_none())
            .then_some(url)
    }

    /// `_download_image`'s route to a file: a blob link names an attachment of
    /// a content; otherwise the path is taken from this instance's URL or a
    /// `/download/...` path, and a link to any other host is refused.
    async fn image_url(&self, image_url: &str) -> Option<Url> {
        if image_url.contains("blob:") || image_url.contains("media-blob-url=true") {
            let content = Regex::new(r"contentId-(\d+)").ok()?.captures(image_url);
            let name = Regex::new(r"name=([^&]+)").ok()?.captures(image_url);
            if let (Some(content), Some(name)) = (content, name) {
                let name = percent_encoding::percent_decode_str(&name[1])
                    .decode_utf8_lossy()
                    .into_owned();
                if let Ok(Reply::Accepted(listing)) = self
                    .get(
                        self.v1(&["content", &content[1], "child", "attachment"])
                            .ok()?,
                    )
                    .await
                    && let Some(link) = listing
                        .get("results")
                        .and_then(Value::as_array)
                        .into_iter()
                        .flatten()
                        .find(|attachment| {
                            attachment.get("title").and_then(Value::as_str) == Some(name.as_str())
                        })
                        .and_then(|attachment| attachment.pointer("/_links/download"))
                        .and_then(Value::as_str)
                {
                    return self.link_url(link);
                }
            }
        }
        let mut parsed = image_url;
        if image_url.contains('?')
            && (image_url.contains("version=") || image_url.contains("modificationDate="))
        {
            parsed = image_url.split('?').next().unwrap_or(image_url);
        }
        // A link under the API root (Cloud: `.../wiki/...`) or under the bare
        // base is made relative to the API root exactly once, so the `/wiki`
        // prefix of Cloud is neither doubled nor lost.
        let api_root = self.config.api_root().as_str().trim_end_matches('/');
        let base = self.config.base_url().as_str().trim_end_matches('/');
        let cloud_prefix = api_root.strip_prefix(base).unwrap_or_default();
        let relative = if let Some(rest) = parsed
            .strip_prefix(api_root)
            .filter(|rest| rest.is_empty() || rest.starts_with('/'))
        {
            rest
        } else if let Some(rest) = parsed
            .strip_prefix(base)
            .filter(|rest| rest.is_empty() || rest.starts_with('/'))
        {
            // Under the bare base: a path that already carries the API
            // root's own prefix (`/wiki/...`) is relative to the root.
            strip_path_prefix(rest, cloud_prefix)
        } else if let Some(at) = ["/download/attachments/", "/download/thumbnails/"]
            .iter()
            .find_map(|pattern| parsed.find(pattern))
        {
            &parsed[at..]
        } else if parsed.starts_with("http") {
            return None;
        } else {
            strip_path_prefix(parsed, cloud_prefix)
        };
        self.file_url(relative)
    }

    /// `_download_image`: a picture's bytes, `None` for the SDK's "download
    /// failed" (a refusal, an HTML page, fewer than 100 bytes, over `limit`).
    async fn download_image(&self, image_url: &str, limit: usize) -> Option<Vec<u8>> {
        let url = self.image_url(image_url).await?;
        let fetched = self.fetch(url, limit).await.ok()?;
        if fetched.status != StatusCode::OK
            || fetched.media_type.as_deref() == Some("text/html")
            || fetched.body.len() < 100
        {
            return None;
        }
        Some(fetched.body)
    }

    /// One listing page of a content's attachments.
    async fn attachment_listing(&self, page_id: &str) -> Result<Reply, ConfluenceClientError> {
        let mut url = self.v1(&["content", page_id, "child", "attachment"])?;
        url.query_pairs_mut().append_pair("limit", "50");
        self.get(url).await
    }

    pub(super) async fn get_page_with_image_descriptions(
        &self,
        page_id: &str,
        prompt: Option<&str>,
        context_radius: i64,
    ) -> Result<String, ConfluenceClientError> {
        const PREFIX: &str = "Error processing page with images";
        validate_identifier(page_id)?;
        let mut url = self.v1(&["content", page_id])?;
        url.query_pairs_mut().append_pair("expand", "body.storage");
        let page = match self.get(url).await? {
            Reply::Accepted(page) if page.is_object() => page,
            Reply::Accepted(_) => return Ok(format!("Page with ID {page_id} not found.")),
            Reply::Refused(StatusCode::NOT_FOUND, _) => {
                return Ok(format!(
                    "{PREFIX}: There is no content with the given id, or the calling user does not have permission to view the content"
                ));
            }
            Reply::Refused(_, message) => return Ok(format!("{PREFIX}: {message}")),
        };
        let title = page.get("title").map(display_value).unwrap_or_default();
        let original = page
            .pointer("/body/storage/value")
            .and_then(Value::as_str)
            .unwrap_or_default()
            .to_owned();
        let mut work = PageWork::new(self, page_id, prompt, context_radius, &original);
        let mut processed = original.clone();
        for kind in [Pass::Attachment, Pass::Url, Pass::Embedded] {
            processed = work.replace(&processed, kind).await?;
        }
        Ok(format!(
            "Page '{title}' (ID: {page_id}) with image descriptions:\n\n{}",
            markdown(&processed)
        ))
    }

    /// `_get_body_file_references`.
    async fn body_file_references(&self, page_id: &str) -> Option<BodyReferences> {
        if !self.config.cloud() {
            return None;
        }
        let route = |kind: &str| {
            let mut url = self.url(&["api", "v2", kind, page_id]).ok()?;
            url.query_pairs_mut()
                .append_pair("body-format", "atlas_doc_format");
            Some(url)
        };
        let mut reply = self.get(route("pages")?).await.ok()?;
        if matches!(reply, Reply::Refused(StatusCode::NOT_FOUND, _)) {
            reply = self.get(route("blogposts")?).await.ok()?;
        }
        let Reply::Accepted(page) = reply else {
            return None;
        };
        let value = page.pointer("/body/atlas_doc_format/value")?;
        let document = match value {
            Value::String(text) => serde_json::from_str::<Value>(text).ok()?,
            other => other.clone(),
        };
        body_references_of(&document)
    }

    /// The listing link `_links.next` names, on this instance only.
    fn listing_url(&self, link: &str) -> Option<Url> {
        if !link.starts_with('/') || link.starts_with("//") || link.contains("..") {
            return None;
        }
        let root = self.config.api_root();
        let link = if root.path().ends_with("/wiki") {
            link.strip_prefix("/wiki").unwrap_or(link)
        } else {
            link
        };
        self.file_url(link)
    }

    /// `_collect_visible_attachments`.
    async fn collect_visible(
        &self,
        first: &Value,
        references: Option<&BodyReferences>,
        matches_request: &(dyn Fn(&Value) -> bool + Send + Sync),
        limit: usize,
    ) -> Selection {
        let mut selection = Selection {
            selected: Vec::new(),
            total: 0,
            visible: 0,
            has_more: false,
            truncated: false,
        };
        let mut pending: Vec<Value> = first
            .get("results")
            .and_then(Value::as_array)
            .cloned()
            .unwrap_or_default();
        let mut next = references
            .and(first.pointer("/_links/next"))
            .and_then(Value::as_str)
            .map(ToOwned::to_owned);
        let mut visited: HashSet<String> = HashSet::new();
        for _ in 0..MAX_LISTING_PAGES {
            for entry in &pending {
                selection.total += 1;
                if !visible_on_page(entry, references) {
                    continue;
                }
                selection.visible += 1;
                if !matches_request(entry) {
                    continue;
                }
                if selection.selected.len() >= limit {
                    selection.has_more = true;
                    break;
                }
                selection.selected.push(entry.clone());
            }
            let Some(link) = next.take() else {
                break;
            };
            if selection.has_more || !visited.insert(link.clone()) {
                break;
            }
            let Some(url) = self.listing_url(&link) else {
                selection.truncated = true;
                break;
            };
            let Ok(Reply::Accepted(page)) = self.get(url).await else {
                selection.truncated = true;
                break;
            };
            pending = page
                .get("results")
                .and_then(Value::as_array)
                .cloned()
                .unwrap_or_default();
            next = page
                .pointer("/_links/next")
                .and_then(Value::as_str)
                .map(ToOwned::to_owned);
        }
        selection
    }

    #[allow(clippy::too_many_lines)] // One SDK tool, kept in the SDK's order.
    pub(super) async fn get_page_attachments(
        &self,
        page_id: &str,
        max_content_length: i64,
        custom_prompt: Option<&str>,
        allowed_extensions: Option<&[String]>,
        name_pattern: Option<&str>,
        max_attachments: i64,
    ) -> Result<Value, ConfluenceClientError> {
        const PREFIX: &str = "Error retrieving attachments";
        validate_identifier(page_id)?;
        let max_length =
            usize::try_from(max_content_length.clamp(0, MAX_CONTENT_LENGTH)).unwrap_or(0);
        let limit = usize::try_from(max_attachments.clamp(1, MAX_ATTACHMENTS)).unwrap_or(1);
        let pattern = match name_pattern.filter(|pattern| !pattern.is_empty()) {
            Some(pattern) => {
                Some(PythonRegex::new(&format!(r"\A(?:{pattern})")).map_err(|_| {
                    refuse("name_pattern is not a valid regular expression.".to_owned())
                })?)
            }
            None => None,
        };
        let first = match self.attachment_listing(page_id).await? {
            Reply::Accepted(first) => first,
            Reply::Refused(StatusCode::NOT_FOUND, _) => {
                return text(
                    format!(
                        "{PREFIX}: There is no content with the given id, or the calling user does not have permission to view the content"
                    ),
                    false,
                );
            }
            Reply::Refused(_, message) => return text(format!("{PREFIX}: {message}"), false),
        };
        if first
            .get("results")
            .and_then(Value::as_array)
            .is_none_or(Vec::is_empty)
        {
            return text(
                format!("No attachments found for page ID {page_id}."),
                false,
            );
        }
        let references = self.body_file_references(page_id).await;
        let allowed: Option<Vec<String>> = allowed_extensions
            .filter(|list| !list.is_empty())
            .map(<[String]>::to_vec);
        let matches_request = move |entry: &Value| {
            let title = entry
                .get("title")
                .and_then(Value::as_str)
                .unwrap_or_default();
            if let Some(allowed) = &allowed
                && !allowed.contains(&extension_of(title))
            {
                return false;
            }
            pattern
                .as_ref()
                .is_none_or(|pattern| pattern.is_match(title).unwrap_or(false))
        };
        let selection = self
            .collect_visible(&first, references.as_ref(), &matches_request, limit)
            .await;
        if selection.visible == 0 {
            let mut message = format!(
                "No attachments are visible on page {page_id}: {} attachment(s) exist but are not referenced in the page body.",
                selection.total
            );
            if selection.truncated {
                message.push_str(
                    " The attachment listing was truncated; further attachments were not checked.",
                );
            }
            return text(message, false);
        }
        let mut notices: Vec<String> = Vec::new();
        if selection.has_more {
            notices.push(format!(
                "Showing the first {limit} matching visible attachments; more exist — narrow with allowed_extensions/name_pattern or raise max_attachments."
            ));
        }
        if selection.truncated {
            notices.push(
                "The attachment listing could not be fully fetched; results may be incomplete."
                    .to_owned(),
            );
        }
        let mut histories: HashMap<String, Value> = HashMap::new();
        for attachment in &selection.selected {
            let Some(id) = attachment.get("id").map(display_value) else {
                continue;
            };
            if validate_identifier(&id).is_err() {
                continue;
            }
            if let Ok(Reply::Accepted(history)) =
                self.get(self.v1(&["content", &id, "history"])?).await
                && history.is_object()
            {
                histories.insert(id, history);
            }
        }
        let mut tally = PictureTally::default();
        let mut no_model = self.attachments.is_none();
        let mut results: Vec<Value> = Vec::new();
        for attachment in &selection.selected {
            let title = attachment
                .get("title")
                .and_then(Value::as_str)
                .unwrap_or_default();
            let file_ext = extension_of(title);
            let media_type = attachment
                .pointer("/metadata/mediaType")
                .and_then(Value::as_str)
                .unwrap_or_default();
            let history = attachment
                .get("id")
                .map(display_value)
                .and_then(|id| histories.get(&id))
                .filter(|history| truthy(history));
            let (created_by, created_date, last_updated) = match history {
                Some(history) => (
                    history
                        .pointer("/createdBy/displayName")
                        .map_or_else(String::new, display_value),
                    history
                        .get("createdDate")
                        .map_or_else(String::new, display_value),
                    history
                        .pointer("/lastUpdated/when")
                        .map_or_else(String::new, display_value),
                ),
                None => (
                    attachment
                        .pointer("/creator/displayName")
                        .map_or_else(String::new, display_value),
                    attachment
                        .get("created")
                        .map_or_else(String::new, display_value),
                    String::new(),
                ),
            };
            let link = attachment
                .pointer("/_links/download")
                .and_then(Value::as_str)
                .filter(|link| !link.is_empty());
            let labels: Vec<Value> = attachment
                .pointer("/metadata/labels/results")
                .and_then(Value::as_array)
                .into_iter()
                .flatten()
                .filter_map(|label| label.get("name").cloned())
                .collect();
            let metadata = json!({
                "name": title,
                "size": attachment.pointer("/extensions/fileSize").cloned().unwrap_or(Value::Null),
                "creator": created_by,
                "created": created_date,
                "updated": last_updated,
                "media_type": media_type,
                "labels": labels,
                "download_url": link.map_or(Value::Null, |link| Value::String(self.download_link(link))),
            });
            let mut body_text: Option<String> = None;
            let mut analysis: Option<String> = None;
            let is_text = TEXT_MEDIA_TYPES.contains(&media_type)
                || TEXT_EXTENSIONS.contains(&file_ext.as_str());
            let url = link.and_then(|link| self.link_url(link));
            if is_text {
                body_text = Some(match url {
                    None => {
                        "[Error downloading content: the attachment has no usable download link]"
                            .to_owned()
                    }
                    Some(url) => match self.fetch(url, MAX_TEXT_ATTACHMENT_BYTES).await {
                        Ok(fetched) if fetched.status == StatusCode::OK => {
                            let raw = match String::from_utf8(fetched.body) {
                                Ok(text) => text,
                                Err(error) => error
                                    .into_bytes()
                                    .iter()
                                    .map(|&byte| char::from(byte))
                                    .collect(),
                            };
                            match file_ext.as_str() {
                                "doc" | "docx" | "xls" | "xlsx" => format!(
                                    "[Text extraction from .{file_ext} files is not available in this runtime]"
                                ),
                                "csv" => csv_text(&raw),
                                "svg" => raw,
                                "html" | "htm" => html_text(&raw),
                                _ if media_type == "image/svg+xml" => raw,
                                _ if matches!(media_type, "text/html" | "application/html") => {
                                    html_text(&raw)
                                }
                                _ => raw,
                            }
                        }
                        Ok(fetched) => {
                            format!("[Failed to download: HTTP {}]", fetched.status.as_u16())
                        }
                        Err(FetchFailure::TooLarge) => format!(
                            "[Failed to download: larger than {} MiB]",
                            MAX_TEXT_ATTACHMENT_BYTES / (1024 * 1024)
                        ),
                        Err(FetchFailure::Refused) => {
                            "[Error downloading content: the download was refused or failed]"
                                .to_owned()
                        }
                    },
                });
                body_text = body_text.map(|body| truncated(&body, max_length));
            } else if media_type.starts_with("image/") {
                let declared = attachment
                    .pointer("/extensions/fileSize")
                    .and_then(Value::as_u64);
                // The call's bounds always apply; with no model the picture is
                // then not fetched at all.
                analysis = if let Some(reason) = tally.refusal(declared) {
                    Some(format!("[Image {title} - {reason}]"))
                } else if no_model {
                    Some(NO_MODEL_TEXT.to_owned())
                } else {
                    tally.attempt();
                    let image = match link {
                        Some(link) => {
                            self.download_image(
                                &self.download_link(link),
                                usize::try_from(MAX_PICTURE_BYTES).unwrap_or(usize::MAX),
                            )
                            .await
                        }
                        None => None,
                    };
                    match image {
                        None => None,
                        Some(bytes) if !tally.take(bytes.len() as u64) => Some(format!(
                            "[Image {title} - not described: the pictures of one call are limited]"
                        )),
                        Some(bytes) => {
                            let attachment_context =
                                format!("Attachment: {title} (type: {media_type})");
                            let prompt = picture_prompt(
                                DEFAULT_IMAGE_PROMPT,
                                custom_prompt,
                                title,
                                &attachment_context,
                            );
                            Some(match self.attachments.as_ref() {
                                None => {
                                    no_model = true;
                                    NO_MODEL_TEXT.to_owned()
                                }
                                Some(host) => {
                                    match describe_for_text(host, bytes, title, prompt).await {
                                        Ok(description) => description,
                                        Err(error) => {
                                            let note = if matches!(
                                                error,
                                                AttachmentError::Host(
                                                    Step::Model,
                                                    OfficeFailure::NoModel
                                                )
                                            ) {
                                                no_model = true;
                                                NO_MODEL_TEXT.to_owned()
                                            } else {
                                                format!(
                                                    "[Image {title} - not described: {}]",
                                                    error.sentence()
                                                )
                                            };
                                            tally.stop(error);
                                            note
                                        }
                                    }
                                }
                            })
                        }
                    }
                };
            } else if media_type == "application/pdf" {
                analysis = Some("[PDF page analysis is not available in this runtime]".to_owned());
            } else if media_type.starts_with("application/vnd.jgraph.mxfile") {
                analysis =
                    Some("[Draw.io diagram analysis is not available in this runtime]".to_owned());
            }
            let analysis = analysis.map(|analysis| truncated(&analysis, max_length));
            results.push(json!({
                "metadata": metadata,
                "comments": [],
                "content": body_text,
                "llm_analysis": analysis,
            }));
        }
        results.extend(
            notices
                .into_iter()
                .map(|notice| json!({ "notice": notice })),
        );
        let value = Value::Array(results);
        if serde_json::to_vec(&value).map_or(true, |bytes| bytes.len() > MAX_OUTPUT_BYTES) {
            return Err(resource_exhausted());
        }
        Ok(value)
    }

    /// Read the artifact and send it to the page as an attachment
    /// (`_upload_file_from_artifact`; a same-named attachment gets a new
    /// version, as `attach_content` does).
    async fn upload_artifact(
        &self,
        page_id: &str,
        filepath: &str,
        filename: Option<&str>,
    ) -> Result<Uploaded, ConfluenceClientError> {
        validate_identifier(page_id)?;
        let host = self.host()?;
        let file = host
            .read_artifact(filepath, MAX_ATTACH_BYTES)
            .await
            .map_err(attachment_failure)?;
        let name = match filename {
            Some(name) if !name.is_empty() => name.to_owned(),
            _ => file.name.clone(),
        };
        if !upload_name_is_valid(&name) {
            return Err(refuse(
                "The file name is not valid: it must not be empty, longer than 255 bytes, or contain a control character or a path separator. Nothing was uploaded."
                    .to_owned(),
            ));
        }
        let media_type = upload_media_type(&file.bytes, &name);
        let failed = |message: &str| {
            refuse(format!(
                "Failed to upload attachment '{name}' to page {page_id}: {message}. Nothing was uploaded."
            ))
        };
        // `attach_content` looks for an attachment of that name first.
        let mut url = self.v1(&["content", page_id, "child", "attachment"])?;
        url.query_pairs_mut().append_pair("filename", &name);
        let existing = match self.get(url).await? {
            Reply::Accepted(listing) => listing
                .get("results")
                .and_then(Value::as_array)
                .and_then(|results| results.first())
                .and_then(|first| first.get("id"))
                .map(display_value)
                .filter(|_| listing.get("size").is_some_and(truthy)),
            Reply::Refused(_, message) => return Err(failed(&message)),
        };
        if let Some(id) = &existing {
            validate_identifier(id)?;
        }
        let comment = format!("Uploaded {name}.");
        let body = multipart_upload(
            &[
                ("type", "attachment"),
                ("fileName", &name),
                ("contentType", media_type),
                ("comment", &comment),
                ("minorEdit", "true"),
            ],
            "file",
            &name,
            media_type,
            &file.bytes,
        );
        let url = match &existing {
            Some(id) => self.v1(&["content", page_id, "child", "attachment", id, "data"])?,
            None => self.v1(&["content", page_id, "child", "attachment"])?,
        };
        let mut request = self.request(Method::POST, url, None)?;
        {
            let headers = request.headers_mut();
            headers.insert(
                reqwest::header::HeaderName::from_static("x-atlassian-token"),
                HeaderValue::from_static("no-check"),
            );
            headers.insert(
                CONTENT_TYPE,
                HeaderValue::from_str(&body.content_type).map_err(|_| invalid_input())?,
            );
        }
        *request.body_mut() = Some(body.bytes.into());
        let response = self.transport.execute(request, true).await?;
        let stored = match classify(&response, true)? {
            Reply::Accepted(stored) => stored,
            Reply::Refused(status, message) => {
                let reason = match status {
                    StatusCode::FORBIDDEN => "Attachments are disabled or the calling user does not have permission to add attachments to this content".to_owned(),
                    StatusCode::NOT_FOUND => "The requested content is not found, the user does not have permission to view it, or the attachment exceeds the maximum configured attachment size".to_owned(),
                    _ => message,
                };
                return Err(failed(&reason));
            }
        };
        let title = stored
            .get("title")
            .or_else(|| stored.pointer("/results/0/title"))
            .and_then(Value::as_str)
            .map_or(name, ToOwned::to_owned);
        Ok(Uploaded {
            filename: title,
            media_type,
            new_version: existing.is_some(),
        })
    }

    pub(super) async fn add_file_to_page(
        &self,
        page_id: &str,
        filepath: &str,
        filename: Option<&str>,
        caption: Option<&str>,
        prepend: bool,
    ) -> Result<Value, ConfluenceClientError> {
        let upload = self.upload_artifact(page_id, filepath, filename).await?;
        let target = "the page content";
        let name = xml_escape(&upload.filename);
        let markup = if upload.media_type.starts_with("image/")
            || upload.media_type.starts_with("video/")
        {
            let caption = caption
                .filter(|caption| !caption.is_empty())
                .map(|caption| format!("<ac:caption><p>{}</p></ac:caption>", xml_escape(caption)))
                .unwrap_or_default();
            format!("<ac:image><ri:attachment ri:filename=\"{name}\"/>{caption}</ac:image>")
        } else {
            format!("<ac:link><ri:attachment ri:filename=\"{name}\"/></ac:link>")
        };
        let mut view_link = String::new();
        for attempt in 0..3 {
            match self
                .replace_page_body(page_id, &markup, prepend, &upload.filename)
                .await
            {
                Ok(Ok(link)) => {
                    view_link = link;
                    break;
                }
                Ok(Err((StatusCode::CONFLICT, _))) if attempt < 2 => {}
                Ok(Err((_, message))) => {
                    return Err(Self::after_upload_failure(
                        &upload, page_id, target, None, &message,
                    ));
                }
                Err(error) => {
                    let reason = error.to_string();
                    return Err(Self::after_upload_failure(
                        &upload,
                        page_id,
                        target,
                        Some(&error),
                        &reason,
                    ));
                }
            }
        }
        let note = if upload.new_version {
            format!(
                " Note: the page already had an attachment named '{}'; a new version of it was uploaded and the earlier version stays in its history.",
                upload.filename
            )
        } else {
            String::new()
        };
        text(
            format!(
                "File '{}' uploaded and added to page {page_id} as {}. View at: {view_link}{note}",
                upload.filename,
                file_type(upload.media_type)
            ),
            true,
        )
    }

    /// One attempt of the page edit (`update_page`): read the page, put the
    /// markup first or last, write the next version (taken from that same
    /// read, so a concurrent edit is a 409). A 4xx answer comes back
    /// as `Err((status, message))` for the caller to retry or report.
    async fn replace_page_body(
        &self,
        page_id: &str,
        markup: &str,
        prepend: bool,
        uploaded: &str,
    ) -> Result<Result<String, (StatusCode, String)>, ConfluenceClientError> {
        let mut url = self.v1(&["content", page_id])?;
        url.query_pairs_mut()
            .append_pair("expand", "body.storage,version");
        let page = match self.get(url).await? {
            Reply::Accepted(page) if page.is_object() => page,
            Reply::Accepted(_) => return Err(invalid_response()),
            Reply::Refused(status, message) => return Ok(Err((status, message))),
        };
        let title = page
            .get("title")
            .and_then(Value::as_str)
            .unwrap_or_default();
        let current = page
            .pointer("/body/storage/value")
            .and_then(Value::as_str)
            .unwrap_or_default();
        let body = if prepend {
            format!("{markup}<p></p>{current}")
        } else {
            format!("{current}<p></p>{markup}")
        };
        // The version comes from the SAME read as the body: a page edited
        // between this read and the write is then a 409 (the caller reads
        // again), never a silent overwrite of the other edit.
        let version = page
            .pointer("/version/number")
            .and_then(Value::as_u64)
            .ok_or_else(invalid_response)?
            .saturating_add(1);
        // A blog post is edited as a blog post: a `page` payload for it is
        // refused or retypes it.
        let content_type = match page.get("type").and_then(Value::as_str) {
            Some("blogpost") => "blogpost",
            _ => "page",
        };
        let payload = json!({
            "id": page_id,
            "type": content_type,
            "title": title,
            "version": {"number": version, "minorEdit": false, "message": format!("Added file: {uploaded}")},
            "metadata": {"properties": fixed_width()},
            "body": {"storage": {"value": body, "representation": "storage"}},
        });
        let mut url = self.v1(&["content", page_id])?;
        url.query_pairs_mut().append_pair("status", "current");
        match self.send(Method::PUT, url, Some(&payload), true).await? {
            Reply::Accepted(_) => {}
            Reply::Refused(status, message) => return Ok(Err((status, message))),
        }
        Ok(Ok(self.page_url(
            page.pointer("/_links/webui")
                .and_then(Value::as_str)
                .unwrap_or_default(),
        )))
    }

    /// The sentence for a page edit that failed after a successful upload.
    fn after_upload_failure(
        upload: &Uploaded,
        page_id: &str,
        target: &str,
        error: Option<&ConfluenceClientError>,
        reason: &str,
    ) -> ConfluenceClientError {
        let unknown =
            error.is_some_and(|error| error.code == ConfluenceClientErrorCode::UnknownOutcome);
        let message = if unknown {
            format!(
                "The file '{}' was uploaded to page {page_id}, but whether {target} was updated is unknown: Confluence may have applied the change. Check the page before retrying; do not upload the file again.",
                upload.filename
            )
        } else {
            format!(
                "The file '{}' was uploaded to page {page_id}, but {target} was NOT updated: {reason}. Do not upload the file again.",
                upload.filename
            )
        };
        ConfluenceClientError::described(
            if unknown {
                ConfluenceClientErrorCode::UnknownOutcome
            } else {
                ConfluenceClientErrorCode::InvalidInput
            },
            ErrorCategory::Internal,
            "confluence.attachment.partial",
            message,
        )
    }
}

/// One upload that Confluence stored.
struct Uploaded {
    filename: String,
    media_type: &'static str,
    /// A same-named attachment existed; this is its new version.
    new_version: bool,
}

#[derive(Clone, Copy)]
enum Pass {
    Attachment,
    Url,
    Embedded,
}

/// The pictures of one page: found through its attachments or its links,
/// fetched through the authenticated client, described by the run's model
/// within the call's bounds.
struct PageWork<'a> {
    client: &'a ConfluenceClient,
    page_id: &'a str,
    prompt: Option<&'a str>,
    radius: i64,
    original: &'a str,
    listing: Option<Vec<Value>>,
    tally: PictureTally,
    described: HashMap<String, String>,
    no_model: bool,
    /// Pictures left unfetched because the run has no model.
    skipped_no_model: u32,
}

impl<'a> PageWork<'a> {
    fn new(
        client: &'a ConfluenceClient,
        page_id: &'a str,
        prompt: Option<&'a str>,
        radius: i64,
        original: &'a str,
    ) -> Self {
        Self {
            client,
            page_id,
            prompt,
            radius,
            original,
            listing: None,
            tally: PictureTally::default(),
            described: HashMap::new(),
            // Without a host there is no model: no picture is fetched.
            no_model: client.attachments.is_none(),
            skipped_no_model: 0,
        }
    }

    /// `re.sub(pattern, handler, content)` for one kind of image reference.
    async fn replace(
        &mut self,
        content: &str,
        pass: Pass,
    ) -> Result<String, ConfluenceClientError> {
        let pattern = match pass {
            Pass::Attachment => {
                r#"<ac:image[^>]*>(?:.*?)<ri:attachment ri:filename="([^"]+)"[^>]*/>(?:.*?)</ac:image>"#
            }
            Pass::Url => {
                r#"<ac:image[^>]*>(?:.*?)<ri:url ri:value="([^"]+)"[^>]*/>(?:.*?)</ac:image>"#
            }
            Pass::Embedded => {
                r"<ac:image[^>]*>(?:.*?)<ac:resource>(?:.*?)<ac:media-type>([^<]+)</ac:media-type>(?:.*?)<ac:data>([^<]+)</ac:data>(?:.*?)</ac:resource>(?:.*?)</ac:image>"
            }
        };
        let pattern = Regex::new(pattern).map_err(|_| invalid_input())?;
        let matches: Vec<(usize, usize, String, String, String)> = pattern
            .captures_iter(content)
            .filter_map(|captures| {
                let whole = captures.get(0)?;
                Some((
                    whole.start(),
                    whole.end(),
                    captures.get(1)?.as_str().to_owned(),
                    captures
                        .get(2)
                        .map_or_else(String::new, |data| data.as_str().to_owned()),
                    whole.as_str().to_owned(),
                ))
            })
            .collect();
        let mut output = String::with_capacity(content.len());
        let mut last = 0;
        for (start, end, first, second, whole) in matches {
            output.push_str(&content[last..start]);
            let replacement = match pass {
                Pass::Attachment => self.attachment_image(&first, &whole).await?,
                Pass::Url => self.url_image(&first, &whole).await,
                Pass::Embedded => self.embedded_image(&first, &second, &whole).await,
            };
            output.push_str(&replacement);
            last = end;
        }
        output.push_str(&content[last..]);
        Ok(output)
    }

    async fn attachment_image(
        &mut self,
        filename: &str,
        whole: &str,
    ) -> Result<String, ConfluenceClientError> {
        if self.listing.is_none() {
            self.listing = Some(match self.client.attachment_listing(self.page_id).await? {
                Reply::Accepted(listing) => listing
                    .get("results")
                    .and_then(Value::as_array)
                    .cloned()
                    .unwrap_or_default(),
                Reply::Refused(_, message) => {
                    return Ok(format!("[Image: {filename} - Error: {message}]"));
                }
            });
        }
        let Some(link) = self
            .listing
            .iter()
            .flatten()
            .find(|attachment| attachment.get("title").and_then(Value::as_str) == Some(filename))
            .map(|attachment| {
                attachment
                    .pointer("/_links/download")
                    .and_then(Value::as_str)
                    .map(ToOwned::to_owned)
            })
        else {
            return Ok(format!("[Image: {filename} - attachment not found]"));
        };
        let Some(link) = link else {
            return Ok(format!("[Image: {filename} - Error: 'download']"));
        };
        let url = self.client.download_link(&link);
        Ok(self.describe(&url, filename, whole, filename).await)
    }

    async fn url_image(&mut self, image_url: &str, whole: &str) -> String {
        let name = image_url.rsplit('/').next().unwrap_or(image_url).to_owned();
        self.describe(image_url, &name, whole, &name).await
    }

    async fn embedded_image(&mut self, media_type: &str, data: &str, whole: &str) -> String {
        let format = media_type.rsplit('/').next().unwrap_or(media_type);
        let name = format!("embedded-image.{format}");
        let alphabet: String = data
            .chars()
            .filter(|c| c.is_ascii_alphanumeric() || matches!(c, '+' | '/' | '='))
            .collect();
        let Ok(bytes) = base64::engine::general_purpose::STANDARD
            .decode(alphabet)
            .map_err(|_| ())
            .and_then(|bytes| if bytes.is_empty() { Err(()) } else { Ok(bytes) })
        else {
            return format!("[Image: embedded {media_type} - decode failed]");
        };
        if bytes.len() as u64 > MAX_PICTURE_BYTES {
            return format!("[Image: embedded {media_type} - not described: larger than 5 MiB]");
        }
        self.describe_bytes(bytes, &name, whole, &format!("embedded {media_type}"))
            .await
            .0
    }

    /// Download a picture (`_download_image`) and describe it.
    async fn describe(&mut self, source: &str, name: &str, whole: &str, label: &str) -> String {
        if let Some(description) = self.described.get(source) {
            return format!("[Image {name} Description: {description}]");
        }
        if let Some(reason) = self.refusal(None) {
            return format!("[Image: {label} - {reason}]");
        }
        // No model in this run: nothing would be done with the bytes, so the
        // picture is not fetched (counted as skipped).
        if self.no_model {
            self.skipped_no_model += 1;
            tracing::debug!(
                skipped = self.skipped_no_model,
                "a picture was not fetched: the run has no chat model"
            );
            return format!("[Image {name} Description: {NO_MODEL_TEXT}]");
        }
        self.tally.attempt();
        let Some(bytes) = self
            .client
            .download_image(
                source,
                usize::try_from(MAX_PICTURE_BYTES).unwrap_or(usize::MAX),
            )
            .await
        else {
            return format!("[Image: {label} - download failed]");
        };
        let (text, description) = self.describe_bytes(bytes, name, whole, label).await;
        if let Some(description) = description {
            // Remembered by source: the same picture is described once.
            self.described.insert(source.to_owned(), description);
        }
        text
    }

    fn refusal(&self, declared: Option<u64>) -> Option<String> {
        self.tally.refusal(declared)
    }

    /// The replacement text and, when the model described the picture, the
    /// description (for the call's cache).
    async fn describe_bytes(
        &mut self,
        bytes: Vec<u8>,
        name: &str,
        whole: &str,
        label: &str,
    ) -> (String, Option<String>) {
        if !self.tally.take(bytes.len() as u64) {
            return (
                format!("[Image: {label} - not described: the pictures of one call are limited]"),
                None,
            );
        }
        let no_model = || (format!("[Image {name} Description: {NO_MODEL_TEXT}]"), None);
        if self.no_model {
            return no_model();
        }
        let context = collect_context(self.original, whole, self.radius);
        let prompt = picture_prompt(DEFAULT_IMAGE_PROMPT, self.prompt, name, &context);
        let Some(host) = self.client.attachments.as_ref() else {
            self.no_model = true;
            return no_model();
        };
        match describe_for_text(host, bytes, name, prompt).await {
            Ok(description) => (
                format!("[Image {name} Description: {description}]"),
                Some(description),
            ),
            Err(error) => {
                if matches!(
                    error,
                    AttachmentError::Host(Step::Model, OfficeFailure::NoModel)
                ) {
                    self.no_model = true;
                    return no_model();
                }
                let sentence = error.sentence();
                self.tally.stop(error);
                (
                    format!("[Image: {label} - not described: {sentence}]"),
                    None,
                )
            }
        }
    }
}

#[cfg(test)]
mod prefix_tests {
    use super::strip_path_prefix;

    #[test]
    fn a_path_prefix_is_removed_only_as_a_whole_segment() {
        assert_eq!(
            strip_path_prefix("/wiki/download/x", "/wiki"),
            "/download/x"
        );
        assert_eq!(strip_path_prefix("/wiki", "/wiki"), "");
        assert_eq!(
            strip_path_prefix("/wikifoo/download/x", "/wiki"),
            "/wikifoo/download/x"
        );
        assert_eq!(strip_path_prefix("/download/x", "/wiki"), "/download/x");
        assert_eq!(strip_path_prefix("/download/x", ""), "/download/x");
    }
}

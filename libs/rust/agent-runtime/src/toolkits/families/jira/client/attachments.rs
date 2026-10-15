//! The Jira attachment tools: `add_file_to_issue_description`,
//! `update_comment_with_file`, `get_attachments_content`,
//! `get_field_with_image_descriptions` and
//! `get_comments_with_image_descriptions`, ported from the SDK's
//! `JiraApiWrapper` (`elitea_sdk/tools/jira/api_wrapper.py`).
//!
//! Every file goes in and out through the run's [`AttachmentHost`] (the
//! artifact store and the chat model); Jira is reached only through this
//! family's own authenticated transport. A picture or an attachment is
//! fetched from the configured Jira origin only; Cloud's redirect to its
//! media host is followed without the credential (see
//! [`JiraClient::download`]).
//!
//! Known differences from the SDK, stated where they matter:
//! * Text attachments are read as one element, where the SDK's text loader
//!   splits long text into 512-token chunks; `get_attachments_content` parses
//!   plain text (`.txt`, `.yml`, `.yaml`, `.groovy`, files whose extension no
//!   SDK loader claims), `.csv` and the pictures `.png`, `.jpg`, `.jpeg`,
//!   `.gif`, `.webp`. Every other type the SDK parses (PDF, Office, HTML,
//!   JSON, XML, Markdown, source code, e-mail, SVG, BMP) is skipped.
//! * A picture is described when the model gateway takes its type (PNG, JPEG,
//!   GIF, WebP); the SDK also converts BMP and the like.

use std::collections::HashMap;
use std::fmt::Write as _;

use fancy_regex::Regex as PythonRegex;
use regex::Regex;
use serde_json::{Value, json};

use super::*;
use crate::toolkits::families::attachment_host::{
    AttachmentError, MAX_ATTACH_BYTES, MAX_PICTURE_BYTES, PictureTally, Step, describe_for_text,
    detect_media_type, is_inline_media, multipart_upload, picture_prompt, upload_name_is_valid,
};
use crate::toolkits::families::office_host::OfficeFailure;
use crate::toolkits::families::python_repr::repr_str_list;

/// `JiraApiWrapper._get_default_image_analysis_prompt`, verbatim (indentation included).
const DEFAULT_IMAGE_PROMPT: &str = "\n        ## Image Analysis Task:\n        Analyze this image and describe it in detail, focusing on structural elements and UI components rather than specific content details.\n        Consider image type and context:\n        ## Image Type: Diagrams (e.g., Sequence Diagram, Context Diagram, Component Diagram)\n        **Prompt**:\n        \"Analyze the given diagram to identify and describe the connections and relationships between components. Provide a detailed flow of interactions, highlighting key elements and their roles within the system architecture. Provide result in functional specification format ready to be used by BA's, Developers and QA's.\"\n        ## Image Type: Application Screenshots\n        **Prompt**:\n        \"Examine the application screenshot to construct a functional specification. Detail the user experience by identifying and describing all UX components, their functions, and the overall flow of the screen.\"\n        ## Image Type: Free Form Screenshots (e.g., Text Documents, Excel Sheets)\n        **Prompt**:\n        \"Extract and interpret the text from the screenshot. Establish and describe the relationships between the text and any visible components, providing a comprehensive understanding of the content and context.\"\n        ## Image Type: Mockup Screenshots\n        **Prompt**:\n        \"Delve into the UX specifics of the mockup screenshot. Offer a detailed description of each component, focusing on design elements, user interactions, and the overall user experience.\"\n\n        Focus on:\n        1. UI components and their arrangement (buttons, forms, navigation elements, etc.)\n        2. Visual structure and layout patterns\n        3. Functional elements if it's a screenshot or diagram\n        4. General purpose and type of the interface or diagram shown\n        5. Overall context and functionality represented\n\n        ### Instructions and Important guidelines:\n        - Prioritize describing the PURPOSE and FUNCTION of interface elements over their specific content\n        - For media content (like movie/video thumbnails), mention the presence of media elements but DON'T focus on specific titles\n        - For text fields, describe their purpose (e.g., \"username field\", \"search box\") rather than the exact text entered\n        - For diagrams, focus on the type of relationship shown rather than specific entity names\n        - Provide a comprehensive description that focuses on HOW the interface works rather than the specific content displayed\n        - Ensure clarity and precision in the analysis for each image type.\n        - Avoid introducing information does not present in the image.\n        - Maintain a structured and logical flow in the output to enhance understanding and usability.\n        - Avoid presenting the entire prompt for user.\n        ";
/// `image_processing_prompt` of the SDK's content parser, verbatim: what the image loader sends for an attachment.
const ATTACHMENT_IMAGE_PROMPT: &str = "\nYou are an AI model designed for analyzing images. Your task is to accurately describe the content of the given image. Depending on the type of image, follow these specific instructions:\n\nIf the image is a diagram (e.g., chart, table, pie chart, bar graph, etc.):\n\nIdentify the type of diagram.\nExtract all numerical values, labels, axis titles, headings, legends, and any other textual elements.\nDescribe the relationships or trends between the data, if visible.\nIf the image is a screenshot:\n\nDescribe what is shown in the screenshot.\nIf it is a software interface, identify the program or website name (if visible).\nList the key interface elements (e.g., buttons, menus, text fields, images, headers).\nIf there is text, extract it.\nIf the screenshot shows a conversation, describe the participants, the content of the messages, and timestamps (if visible).\nIf the image is a photograph:\n\nDescribe the main objects, people, animals, or elements visible in the photo.\nSpecify the setting (e.g., indoors, outdoors, nature, urban area).\nIf possible, identify the actions being performed by people or objects in the photo.\nIf the image is an illustration or drawing:\n\nDescribe the style of the illustration (e.g., realistic, cartoonish, abstract).\nIdentify the main elements, their colors, and the composition of the image.\nIf there is text, extract it.\nIf the image contains text:\n\nExtract all text from the image.\nSpecify the format of the text (e.g., heading, paragraph, list).\nIf the image is a mixed type (e.g., a diagram within a screenshot):\n\nIdentify all types of content present in the image.\nPerform an analysis for each type of content separately, following the relevant instructions above.\nIf the image does not fit into any of the above categories:\n\nProvide a detailed description of what is shown in the image.\nHighlight any visible details that could help in understanding the image.\nBe as precise and thorough as possible in your responses. If something is unclear or illegible, state that explicitly.\n";

/// The most attachments of one issue `get_attachments_content` reads.
const MAX_LISTED_ATTACHMENTS: usize = 100;
/// The largest attachment `get_attachments_content` reads (pictures: 5 MiB).
const MAX_TEXT_ATTACHMENT_BYTES: usize = 10 * 1024 * 1024;
/// Redirects one download follows (Cloud: one, to its media host).
const MAX_REDIRECTS: usize = 5;
const ATLASSIAN_MEDIA_HOST: &str = "api.media.atlassian.com";
/// `re.sub(r'!([^!|]+)(?:\|[^!]*)?!', ...)` of the SDK.
const IMAGE_REFERENCE: &str = r"!([^!|]+)(?:\|[^!]*)?!";
/// The extensions an SDK loader parses that this runtime does not (code,
/// markup, documents, SVG/BMP pictures): such an attachment is skipped.
const UNPORTED_EXTENSIONS: &[&str] = &[
    "bmp", "svg", "py", "js", "ts", "java", "cpp", "c", "h", "hpp", "cs", "rb", "go", "php",
    "swift", "kt", "rs", "m", "scala", "pl", "sh", "bat", "lua", "r", "pas", "asm", "dart", "sql",
    "jsx", "tsx", "mjs", "cjs", "hs", "bash", "zsh", "pm", "toml", "ini", "cfg", "conf", "env",
    "xml", "md", "mdx", "xlsx", "xls", "pdf", "docx", "json", "jsonl", "htm", "html", "pptx",
    "eml", "msg", "doc", "ppt", "rtf", "odt", "ods", "odp", "epub",
];

/// What the SDK's loaders would do with an attachment, as far as this
/// runtime follows them.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
enum AttachmentKind {
    /// `.txt`, `.yml`, `.yaml`, `.groovy`, and a type no loader claims (the
    /// SDK falls back to its text loader).
    Text,
    Csv,
    Picture,
    /// A type the SDK parses and this runtime does not.
    Unported,
}

fn attachment_kind(filename: &str) -> AttachmentKind {
    // `re.search(r'\.([^.]+)$', filename)`
    let extension = filename
        .rsplit_once('.')
        .map(|(_, extension)| extension.to_lowercase())
        .filter(|extension| !extension.is_empty());
    match extension.as_deref() {
        Some("png" | "jpg" | "jpeg" | "gif" | "webp") => AttachmentKind::Picture,
        Some("csv") => AttachmentKind::Csv,
        Some("txt" | "yml" | "yaml" | "groovy") | None => AttachmentKind::Text,
        Some(other) if UNPORTED_EXTENSIONS.contains(&other) => AttachmentKind::Unported,
        Some(_) => AttachmentKind::Text,
    }
}

fn refuse(message: String) -> JiraClientError {
    JiraClientError::described(
        JiraClientErrorCode::InvalidInput,
        ErrorCategory::InvalidInput,
        "jira.attachment.refused",
        message,
    )
}

fn attachment_failure(error: AttachmentError) -> JiraClientError {
    let code = match &error {
        AttachmentError::Host(_, OfficeFailure::Timeout) => JiraClientErrorCode::Timeout,
        AttachmentError::Host(_, OfficeFailure::RateLimited | OfficeFailure::BudgetRefused)
        | AttachmentError::ToolkitBudget => JiraClientErrorCode::RateLimited,
        AttachmentError::Host(
            _,
            OfficeFailure::Unreachable | OfficeFailure::NoModel | OfficeFailure::ReplyUnusable,
        ) => JiraClientErrorCode::DependencyUnavailable,
        _ => JiraClientErrorCode::InvalidInput,
    };
    JiraClientError {
        code,
        retryable: false,
        detail: Some(Box::new(error.into_adk())),
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

/// `_get_file_markup`.
fn file_markup(filename: &str, media_type: &str) -> String {
    if is_inline_media(media_type) {
        format!("!{filename}!")
    } else {
        format!("[^{filename}]")
    }
}

/// `text_to_adf`.
fn text_to_adf(text: &str) -> Value {
    if text.is_empty() {
        return json!({"type": "doc", "version": 1, "content": []});
    }
    let mut content = Vec::new();
    for paragraph in text.split("\n\n") {
        if paragraph.trim().is_empty() {
            continue;
        }
        let mut nodes: Vec<Value> = Vec::new();
        for line in paragraph.split('\n') {
            if !nodes.is_empty() {
                nodes.push(json!({"type": "hardBreak"}));
            }
            nodes.push(json!({"type": "text", "text": line}));
        }
        content.push(json!({"type": "paragraph", "content": nodes}));
    }
    json!({"type": "doc", "version": 1, "content": content})
}

/// `adf_to_text`.
fn adf_to_text(adf: &Value) -> String {
    fn inline(nodes: Option<&Value>) -> String {
        let mut out = String::new();
        for node in nodes.and_then(Value::as_array).into_iter().flatten() {
            let Some(object) = node.as_object() else {
                continue;
            };
            match object.get("type").and_then(Value::as_str) {
                Some("text") => {
                    if let Some(text) = object.get("text").and_then(Value::as_str) {
                        out.push_str(text);
                    }
                }
                Some("hardBreak") => out.push('\n'),
                _ => {
                    if object.get("content").is_some_and(truthy) {
                        out.push_str(&inline(object.get("content")));
                    }
                }
            }
        }
        out
    }
    match adf {
        Value::String(text) => return text.clone(),
        Value::Object(_) => {}
        _ => return String::new(),
    }
    let mut blocks: Vec<String> = Vec::new();
    for node in adf
        .get("content")
        .and_then(Value::as_array)
        .into_iter()
        .flatten()
    {
        let Some(object) = node.as_object() else {
            continue;
        };
        let kind = object.get("type").and_then(Value::as_str);
        if matches!(kind, Some("bulletList" | "orderedList")) {
            for (index, item) in object
                .get("content")
                .and_then(Value::as_array)
                .into_iter()
                .flatten()
                .enumerate()
            {
                let prefix = if kind == Some("bulletList") {
                    "* ".to_owned()
                } else {
                    format!("{}. ", index + 1)
                };
                blocks.push(format!("{prefix}{}", inline(item.get("content"))));
            }
        } else {
            blocks.push(inline(object.get("content")));
        }
    }
    blocks
        .into_iter()
        .filter(|block| !block.is_empty())
        .collect::<Vec<_>>()
        .join("\n\n")
}

/// `os.path.splitext(name)[0]`.
fn without_extension(name: &str) -> &str {
    let Some(dot) = name.rfind('.') else {
        return name;
    };
    if name[..dot].chars().all(|character| character == '.') {
        name
    } else {
        &name[..dot]
    }
}

/// `str.splitlines()`.
fn py_splitlines(text: &str) -> Vec<&str> {
    let mut lines = Vec::new();
    let mut start = 0;
    let mut chars = text.char_indices().peekable();
    while let Some((index, character)) = chars.next() {
        if matches!(
            character,
            '\n' | '\r'
                | '\u{b}'
                | '\u{c}'
                | '\u{1c}'
                | '\u{1d}'
                | '\u{1e}'
                | '\u{85}'
                | '\u{2028}'
                | '\u{2029}'
        ) {
            lines.push(&text[start..index]);
            let mut next = index + character.len_utf8();
            if character == '\r' && chars.peek().is_some_and(|(_, next)| *next == '\n') {
                chars.next();
                next += 1;
            }
            start = next;
        }
    }
    if start < text.len() {
        lines.push(&text[start..]);
    }
    lines
}

/// Python's universal-newline read: `\r\n` and `\r` become `\n`.
fn universal_newlines(text: &str) -> String {
    text.replace("\r\n", "\n").replace('\r', "\n")
}

/// `_collect_context_for_image` of the Jira wrapper.
fn collect_context(content: &str, marker: &str, radius: i64) -> String {
    let Some(byte_position) = content.find(marker) else {
        return String::new();
    };
    let radius = usize::try_from(radius.max(0)).unwrap_or(0);
    let chars: Vec<char> = content.chars().collect();
    let position = content[..byte_position].chars().count();
    let start = position.saturating_sub(radius);
    let end = position
        .saturating_add(marker.chars().count())
        .saturating_add(radius)
        .min(chars.len());
    let mut snippet: String = chars[start..end].iter().collect();
    if start > 0 {
        snippet.insert_str(0, "...");
    }
    if end < chars.len() {
        snippet.push_str("...");
    }
    snippet
}

/// The attachment id an Atlassian link names (`_attachment_id_from_href`).
fn attachment_id_from_href(href: &str) -> Option<String> {
    let pattern = Regex::new(
        r"/rest/api/[23]/attachment/content/(\d+)|/secure/attachment/(\d+)/|/secure/thumbnail/(\d+)/",
    )
    .ok()?;
    let captures = pattern.captures(href)?;
    (1..=3).find_map(|group| captures.get(group).map(|found| found.as_str().to_owned()))
}

fn scalar_text(value: Option<&Value>) -> Option<String> {
    match value? {
        Value::String(text) => Some(text.clone()),
        Value::Number(number) => Some(number.to_string()),
        _ => None,
    }
}

/// `_extract_image_data`: ADF (or a node list, or text) to readable text with
/// `!ref!` image references.
#[allow(clippy::too_many_lines)] // Mirrors the SDK walker node by node.
#[allow(clippy::cast_possible_truncation)] // The heading level is clamped to 1..=6.
fn extract_image_data(value: &Value) -> String {
    let object = match value {
        Value::String(text) => return text.clone(),
        Value::Array(nodes) => return nodes.iter().map(extract_image_data).collect(),
        Value::Object(object) => object,
        _ => return String::new(),
    };
    if let (Some(filename), Some(content)) = (object.get("filename"), object.get("content"))
        && !content.is_array()
    {
        let filename = scalar_text(Some(filename)).unwrap_or_default();
        return format!("!{filename}|alt={filename}!");
    }
    let children = || {
        object
            .get("content")
            .filter(|content| truthy(content))
            .map_or_else(String::new, extract_image_data)
    };
    let attrs = object
        .get("attrs")
        .filter(|attrs| truthy(attrs))
        .and_then(Value::as_object);
    let attribute = |name: &str| {
        attrs
            .and_then(|attrs| attrs.get(name))
            .filter(|value| truthy(value))
    };
    match object.get("type").and_then(Value::as_str) {
        Some("doc") => children().trim_end_matches('\n').to_owned(),
        Some("text") => {
            let text = object
                .get("text")
                .filter(|text| truthy(text))
                .and_then(Value::as_str)
                .unwrap_or_default();
            for mark in object
                .get("marks")
                .and_then(Value::as_array)
                .into_iter()
                .flatten()
            {
                if mark.get("type").and_then(Value::as_str) == Some("link") {
                    let href = mark
                        .get("attrs")
                        .and_then(|attrs| attrs.get("href"))
                        .and_then(Value::as_str)
                        .unwrap_or_default();
                    if let Some(id) = attachment_id_from_href(href) {
                        return format!("{text} !{id}!");
                    }
                }
            }
            text.to_owned()
        }
        Some("paragraph") => format!("{}\n\n", children()),
        Some("heading") => {
            let level = attribute("level")
                .and_then(|level| match level {
                    Value::Number(number) => number
                        .as_i64()
                        .or_else(|| number.as_f64().map(|float| float as i64)),
                    Value::String(text) => text.trim().parse::<i64>().ok(),
                    _ => None,
                })
                .unwrap_or(1)
                .clamp(1, 6);
            format!(
                "{} {}\n\n",
                "#".repeat(usize::try_from(level).unwrap_or(1)),
                children()
            )
        }
        Some(kind @ ("bulletList" | "orderedList")) => {
            let mut items = Vec::new();
            for (index, item) in object
                .get("content")
                .and_then(Value::as_array)
                .into_iter()
                .flatten()
                .enumerate()
            {
                let inner = item
                    .get("content")
                    .filter(|content| truthy(content))
                    .map_or_else(String::new, extract_image_data);
                let inner = inner.trim();
                if inner.is_empty() {
                    continue;
                }
                if kind == "bulletList" {
                    items.push(format!("- {}", inner.replace('\n', "\n  ")));
                } else {
                    items.push(format!("{}. {}", index + 1, inner.replace('\n', "\n   ")));
                }
            }
            if items.is_empty() {
                String::new()
            } else {
                format!("{}\n\n", items.join("\n"))
            }
        }
        Some("blockquote") => {
            let inner = children();
            let inner = inner.trim_end_matches('\n');
            if inner.is_empty() {
                return String::new();
            }
            let quoted = inner
                .split('\n')
                .map(|line| {
                    if line.is_empty() {
                        ">".to_owned()
                    } else {
                        format!("> {line}")
                    }
                })
                .collect::<Vec<_>>()
                .join("\n");
            format!("{quoted}\n\n")
        }
        Some("codeBlock") => format!("```\n{}\n```\n\n", children().trim_end_matches('\n')),
        Some("rule") => "\n---\n\n".to_owned(),
        Some("hardBreak") => "\n".to_owned(),
        Some("mention") => {
            let name = ["text", "displayName", "id"]
                .iter()
                .find_map(|name| scalar_text(attribute(name)))
                .unwrap_or_default();
            format!("@{name}")
        }
        Some("emoji") => ["shortName", "text"]
            .iter()
            .find_map(|name| scalar_text(attribute(name)))
            .unwrap_or_default(),
        Some("inlineCard") => scalar_text(attribute("url")).unwrap_or_default(),
        Some("media" | "mediaInline") => {
            let media_type = attrs.and_then(|attrs| attrs.get("type"));
            if media_type
                .is_none_or(|kind| matches!(kind.as_str(), Some("file" | "link")) || kind.is_null())
            {
                let alt = scalar_text(attribute("alt")).unwrap_or_default();
                let reference = if alt.is_empty() {
                    scalar_text(attribute("id")).unwrap_or_default()
                } else {
                    alt.clone()
                };
                if !reference.is_empty() {
                    return format!("!{reference}|alt=\"{alt}\"!");
                }
            }
            String::new()
        }
        Some("table") => format!("{}\n", children()),
        Some("tableRow") => {
            let cells = object
                .get("content")
                .and_then(Value::as_array)
                .into_iter()
                .flatten()
                .map(|cell| {
                    cell.get("content")
                        .filter(|content| truthy(content))
                        .map_or_else(String::new, extract_image_data)
                        .trim()
                        .replace('\n', " ")
                })
                .collect::<Vec<_>>();
            format!("| {} |\n", cells.join(" | "))
        }
        Some("listItem" | "mediaSingle" | "mediaGroup" | "tableCell" | "tableHeader") => children(),
        _ => {
            if object.get("content").is_some_and(Value::is_array) {
                children()
            } else {
                String::new()
            }
        }
    }
}

/// `AttachmentResolver`: an issue's attachments by id and by file name.
struct Resolver {
    by_id: HashMap<String, Value>,
    by_filename: Vec<(String, Value)>,
}

impl Resolver {
    fn new(attachments: &[Value]) -> Self {
        let mut resolver = Self {
            by_id: HashMap::new(),
            by_filename: Vec::new(),
        };
        for attachment in attachments {
            resolver.index(attachment);
        }
        resolver
    }

    fn put(&mut self, name: String, attachment: &Value) {
        if let Some(slot) = self
            .by_filename
            .iter_mut()
            .find(|(known, _)| *known == name)
        {
            slot.1 = attachment.clone();
        } else {
            self.by_filename.push((name, attachment.clone()));
        }
    }

    fn index(&mut self, attachment: &Value) {
        if let Some(id) = scalar_text(attachment.get("id")) {
            self.by_id.insert(id, attachment.clone());
        }
        let Some(filename) = attachment
            .get("filename")
            .and_then(Value::as_str)
            .filter(|name| !name.is_empty())
        else {
            return;
        };
        let root = without_extension(filename).to_owned();
        self.put(filename.to_owned(), attachment);
        self.put(root.clone(), attachment);
        self.put(format!("{filename}|thumbnail"), attachment);
        self.put(format!("{root}|thumbnail"), attachment);
    }

    fn find(&self, reference: &str) -> Option<&Value> {
        if !reference.is_empty()
            && reference
                .chars()
                .all(|character| character.is_ascii_digit())
            && let Some(found) = self.by_id.get(reference)
        {
            return Some(found);
        }
        if reference.starts_with("attachment")
            && let Some(id) = reference.split(':').nth(1)
            && let Some(found) = self.by_id.get(id.trim())
        {
            return Some(found);
        }
        let mut clean = reference;
        if clean.contains('/') {
            clean = clean.rsplit('/').next().unwrap_or(clean);
        }
        if clean.contains('|') {
            clean = clean.split('|').next().unwrap_or(clean);
        }
        if let Some((_, found)) = self.by_filename.iter().find(|(name, _)| name == clean) {
            return Some(found);
        }
        let lowered = clean.to_lowercase();
        if let Some((_, found)) = self
            .by_filename
            .iter()
            .find(|(name, _)| name.to_lowercase() == lowered)
        {
            return Some(found);
        }
        self.by_filename
            .iter()
            .find(|(name, _)| name.ends_with(clean))
            .map(|(_, found)| found)
    }
}

impl JiraClient {
    fn host(&self) -> Result<&AttachmentHost, JiraClientError> {
        self.attachments.as_ref().ok_or_else(|| {
            JiraClientError::described(
                JiraClientErrorCode::DependencyUnavailable,
                ErrorCategory::Unavailable,
                "jira.attachments.unavailable",
                "This run lends the Jira attachment tools no artifact store or model.".to_owned(),
            )
        })
    }

    /// A link the SDK downloads (`_download_attachment`): only on this
    /// toolkit's own origin; a relative link is taken against the base URL.
    fn attachment_url(&self, raw: &str) -> Option<Url> {
        let base = self.config.base_url();
        let parsed = if raw.starts_with("http://") || raw.starts_with("https://") {
            Url::parse(raw).ok()?
        } else {
            Url::parse(&format!("{}/{}", self.base(), raw.trim_start_matches('/'))).ok()?
        };
        (parsed.origin() == base.origin()
            && parsed.username().is_empty()
            && parsed.fragment().is_none())
        .then_some(parsed)
    }

    /// GET `start` (on this Jira's origin) with the toolkit's credential and
    /// return the body, at most `limit` bytes. Cloud answers with a redirect
    /// to its media host, which is followed WITHOUT the credential or any
    /// custom header, only over https and never back to the Jira origin; any
    /// other redirect target is refused. `None` is the SDK's "download
    /// failed" (a refusal, an HTML page where a file was expected when
    /// `picture_checks`, a body under 100 bytes when `picture_checks`, a body
    /// over `limit`).
    async fn download(&self, start: Url, limit: usize, picture_checks: bool) -> Option<Vec<u8>> {
        let mut url = start;
        let mut credentialed = true;
        let mut left_origin = false;
        for _ in 0..=MAX_REDIRECTS {
            let mut request = if credentialed {
                self.request(Method::GET, url.clone(), None).ok()?
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
                .ok()?;
            if matches!(response.status.as_u16(), 301 | 302 | 303 | 307 | 308) {
                let target = url.join(response.location.as_deref()?).ok()?;
                if target.scheme() != "https" {
                    return None;
                }
                if target.origin() == self.config.base_url().origin() {
                    if left_origin {
                        return None;
                    }
                } else if target
                    .host_str()
                    .is_some_and(|host| host.eq_ignore_ascii_case(ATLASSIAN_MEDIA_HOST))
                {
                    credentialed = false;
                    left_origin = true;
                } else {
                    return None;
                }
                url = target;
                continue;
            }
            if response.status != StatusCode::OK {
                return None;
            }
            if picture_checks
                && (response.media_type.as_deref() == Some("text/html")
                    || response.body.len() < 100)
            {
                return None;
            }
            return Some(response.body);
        }
        None
    }

    /// `_get_attachment_bytes`: Cloud reads the metadata's content URL, then
    /// `attachment/content/{id}`; Server reads `secure/attachment/{id}/{name}`.
    async fn attachment_bytes(&self, id: &str, filename: &str, limit: usize) -> Option<Vec<u8>> {
        if id.is_empty()
            || !id
                .chars()
                .all(|character| character.is_ascii_alphanumeric())
        {
            return None;
        }
        let cloud = self.config.cloud() || self.config.api_version() == JiraApiVersion::V3;
        if cloud {
            let metadata_url = self.api_url(self.version(), &["attachment", id]).ok()?;
            if let Ok(Reply::Accepted(metadata)) =
                self.send(Method::GET, metadata_url, None, false).await
                && let Some(raw) = metadata
                    .get("content")
                    .or_else(|| metadata.pointer("/_links/content"))
                    .and_then(Value::as_str)
                    .filter(|raw| !raw.is_empty())
                && let Some(url) = self.attachment_url(raw)
                && let Some(bytes) = self.download(url, limit, true).await
            {
                return Some(bytes);
            }
            let url = self
                .api_url(self.version(), &["attachment", "content", id])
                .ok()?;
            return self.download(url, limit, false).await;
        }
        let mut url = self.config.base_url().clone();
        {
            let mut path = url.path_segments_mut().ok()?;
            path.pop_if_empty();
            path.extend(["secure", "attachment", id, filename]);
        }
        self.download(url, limit, false).await
    }

    /// The `attachment` field of an issue (empty when Jira refuses).
    async fn issue_attachments(&self, issue_key: &str) -> Result<Vec<Value>, JiraClientError> {
        Ok(self
            .read_issue_attachments(issue_key)
            .await?
            .unwrap_or_default())
    }

    /// The same read, with Jira's refusal kept for a caller that must stop on
    /// it (`Err(message)` inside).
    async fn read_issue_attachments(
        &self,
        issue_key: &str,
    ) -> Result<Result<Vec<Value>, String>, JiraClientError> {
        let mut url = self.api_url(self.version(), &["issue", issue_key])?;
        url.query_pairs_mut().append_pair("fields", "attachment");
        Ok(match self.send(Method::GET, url, None, false).await? {
            Reply::Accepted(issue) => Ok(issue
                .pointer("/fields/attachment")
                .and_then(Value::as_array)
                .map(|list| {
                    list.iter()
                        .filter(|a| a.is_object())
                        .take(MAX_LISTED_ATTACHMENTS)
                        .cloned()
                        .collect()
                })
                .unwrap_or_default()),
            Reply::Refused(message) => Err(message),
        })
    }

    /// Read the artifact and send it to the issue as an attachment
    /// (`_upload_file_from_artifact`). Nothing is sent when the artifact or
    /// the issue cannot be read first; a lost answer on the upload itself is
    /// the family's "unknown outcome".
    async fn upload_artifact(
        &self,
        issue_key: &str,
        filepath: &str,
        filename: Option<&str>,
    ) -> Result<Uploaded, JiraClientError> {
        validate_identifier(issue_key)?;
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
        let media_type = detect_media_type(&file.bytes, &name);
        // Jira keeps a same-named upload beside the old file; say so.
        let duplicate = match self.read_issue_attachments(issue_key).await? {
            Ok(list) => list
                .iter()
                .any(|a| a.get("filename").and_then(Value::as_str) == Some(name.as_str())),
            Err(message) => {
                return Err(refuse(format!(
                    "Failed to upload attachment to {issue_key}: {message}. Nothing was uploaded."
                )));
            }
        };
        let body = multipart_upload(&[], "file", &name, media_type, &file.bytes);
        let url = self.api_url(self.version(), &["issue", issue_key, "attachments"])?;
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
            Reply::Refused(message) => {
                return Err(refuse(format!(
                    "Failed to upload attachment to {issue_key}: {message}. Nothing was uploaded."
                )));
            }
        };
        let info = match &stored {
            Value::Array(list) => list.first(),
            Value::Object(_) => Some(&stored),
            _ => None,
        }
        .filter(|info| info.is_object());
        let Some(info) = info else {
            return Err(JiraClientError::described(
                JiraClientErrorCode::UnknownOutcome,
                ErrorCategory::Internal,
                "jira.effect.unknown_outcome",
                format!(
                    "Jira accepted the upload request for {issue_key} but did not report the stored attachment, so the file may or may not have been attached. Check the issue's attachments before retrying."
                ),
            ));
        };
        Ok(Uploaded {
            filename: info
                .get("filename")
                .and_then(Value::as_str)
                .map_or(name, ToOwned::to_owned),
            attachment_id: scalar_text(info.get("id")).unwrap_or_default(),
            content_url: info
                .get("content")
                .and_then(Value::as_str)
                .unwrap_or_default()
                .to_owned(),
            media_type,
            duplicate,
        })
    }

    /// The new description or comment body with the file referenced
    /// (`add_file_to_issue_description`, `update_comment_with_file`).
    fn with_file_reference(&self, raw: Option<&Value>, upload: &Uploaded, prepend: bool) -> Value {
        let raw = raw.filter(|raw| truthy(raw));
        if self.config.api_version() == JiraApiVersion::V3 {
            let mut document = match raw {
                Some(document @ Value::Object(_)) => document.clone(),
                Some(Value::String(text)) => text_to_adf(text),
                _ => text_to_adf(""),
            };
            let node = json!({
                "type": "paragraph",
                "content": [
                    {"type": "text", "text": "See attached file: "},
                    {
                        "type": "text",
                        "text": upload.filename,
                        "marks": [{"type": "link", "attrs": {"href": upload.content_url}}]
                    }
                ]
            });
            let mut content = document
                .get("content")
                .and_then(Value::as_array)
                .cloned()
                .unwrap_or_default();
            if prepend {
                content.insert(0, node);
            } else {
                content.push(node);
            }
            if let Some(object) = document.as_object_mut() {
                object.insert("content".to_owned(), Value::Array(content));
            }
            return document;
        }
        let current = match raw {
            Some(document @ Value::Object(_)) => adf_to_text(document),
            Some(Value::String(text)) => text.clone(),
            _ => String::new(),
        };
        let markup = file_markup(&upload.filename, upload.media_type);
        Value::String(if prepend {
            format!("{markup}\n\n{current}")
        } else {
            let separator = if current.is_empty() { "" } else { "\n\n" };
            format!("{current}{separator}{markup}")
        })
    }

    /// The sentence for a step after a successful upload that did not
    /// complete: exactly what did and did not happen.
    fn after_upload_failure(
        upload: &Uploaded,
        target: &str,
        error: Option<&JiraClientError>,
        reason: &str,
    ) -> JiraClientError {
        let unknown = error.is_some_and(|error| error.code == JiraClientErrorCode::UnknownOutcome);
        let id = if upload.attachment_id.is_empty() {
            String::new()
        } else {
            format!(" (attachment {})", upload.attachment_id)
        };
        let message = if unknown {
            format!(
                "The file '{}' was uploaded{id}, but whether {target} was updated is unknown: Jira may have applied the change. Check {target} before retrying; do not upload the file again.",
                upload.filename
            )
        } else {
            format!(
                "The file '{}' was uploaded{id}, but {target} was NOT updated: {reason}. Do not upload the file again.",
                upload.filename
            )
        };
        JiraClientError::described(
            if unknown {
                JiraClientErrorCode::UnknownOutcome
            } else {
                JiraClientErrorCode::InvalidInput
            },
            ErrorCategory::Internal,
            "jira.attachment.partial",
            message,
        )
    }

    fn duplicate_note(upload: &Uploaded) -> String {
        if upload.duplicate {
            format!(
                " Note: the issue already had an attachment named '{}'; the new file was added beside it as attachment {}.",
                upload.filename, upload.attachment_id
            )
        } else {
            String::new()
        }
    }

    pub(super) async fn add_file_to_issue_description(
        &self,
        issue_key: &str,
        filepath: &str,
        filename: Option<&str>,
        prepend: bool,
    ) -> Result<Value, JiraClientError> {
        let upload = self.upload_artifact(issue_key, filepath, filename).await?;
        let target = "the description";
        let mut url = self.api_url(self.version(), &["issue", issue_key])?;
        url.query_pairs_mut().append_pair("fields", "description");
        let issue = match self.send(Method::GET, url, None, false).await {
            Ok(Reply::Accepted(issue)) => issue,
            Ok(Reply::Refused(message)) => {
                return Err(Self::after_upload_failure(&upload, target, None, &message));
            }
            Err(error) => {
                let reason = error.to_string();
                return Err(Self::after_upload_failure(
                    &upload,
                    target,
                    Some(&error),
                    &reason,
                ));
            }
        };
        let description =
            self.with_file_reference(issue.pointer("/fields/description"), &upload, prepend);
        let mut url = self.api_url(self.version(), &["issue", issue_key])?;
        url.query_pairs_mut().append_pair("notifyUsers", "true");
        let body = json!({"fields": {"description": description}});
        match self.send(Method::PUT, url, Some(&body), true).await {
            Ok(Reply::Accepted(_)) => {}
            Ok(Reply::Refused(message)) => {
                return Err(Self::after_upload_failure(&upload, target, None, &message));
            }
            Err(error) => {
                let reason = error.to_string();
                return Err(Self::after_upload_failure(
                    &upload,
                    target,
                    Some(&error),
                    &reason,
                ));
            }
        }
        text(
            format!(
                "File '{}' uploaded and added to {issue_key} description as {}. View at: {}.{}",
                upload.filename,
                file_type(upload.media_type),
                self.browse_url(issue_key),
                Self::duplicate_note(&upload)
            ),
            true,
        )
    }

    pub(super) async fn update_comment_with_file(
        &self,
        issue_key: &str,
        comment_id: &str,
        filepath: &str,
        filename: Option<&str>,
        prepend: bool,
    ) -> Result<Value, JiraClientError> {
        validate_identifier(issue_key)?;
        validate_identifier(comment_id)?;
        // The SDK reads the comment after the upload; reading it first keeps a
        // wrong comment id from leaving an orphan attachment behind.
        let comment_url =
            self.api_url(self.version(), &["issue", issue_key, "comment", comment_id])?;
        let comment = match self
            .send(Method::GET, comment_url.clone(), None, false)
            .await?
        {
            Reply::Accepted(comment) => comment,
            Reply::Refused(message) => {
                return Err(refuse(format!(
                    "Failed to update comment with file: {message}. Nothing was uploaded."
                )));
            }
        };
        let upload = self.upload_artifact(issue_key, filepath, filename).await?;
        let target = format!("comment {comment_id}");
        let body = self.with_file_reference(comment.get("body"), &upload, prepend);
        let mut url = comment_url;
        url.query_pairs_mut().append_pair("notifyUsers", "true");
        match self
            .send(Method::PUT, url, Some(&json!({ "body": body })), true)
            .await
        {
            Ok(Reply::Accepted(_)) => {}
            Ok(Reply::Refused(message)) => {
                return Err(Self::after_upload_failure(&upload, &target, None, &message));
            }
            Err(error) => {
                let reason = error.to_string();
                return Err(Self::after_upload_failure(
                    &upload,
                    &target,
                    Some(&error),
                    &reason,
                ));
            }
        }
        text(
            format!(
                "File '{}' uploaded and added to {issue_key} comment {comment_id} as {}. View at: {}.{}",
                upload.filename,
                file_type(upload.media_type),
                self.browse_url(issue_key),
                Self::duplicate_note(&upload)
            ),
            true,
        )
    }

    #[allow(clippy::too_many_lines)] // One SDK tool, kept in the SDK's order.
    pub(super) async fn get_attachments_content(
        &self,
        issue_key: &str,
        pattern: Option<&str>,
    ) -> Result<Value, JiraClientError> {
        validate_identifier(issue_key)?;
        let pattern = match pattern.filter(|pattern| !pattern.is_empty()) {
            Some(pattern) => Some(PythonRegex::new(pattern).map_err(|_| {
                refuse("attachment_pattern is not a valid regular expression.".to_owned())
            })?),
            None => None,
        };
        let mut url = self.api_url(self.version(), &["issue", issue_key])?;
        url.query_pairs_mut().append_pair("fields", "attachment");
        let issue = match self.send(Method::GET, url, None, false).await? {
            Reply::Accepted(issue) => issue,
            Reply::Refused(message) => return text(format!("Jira API error: {message}"), false),
        };
        let listed = issue
            .pointer("/fields/attachment")
            .and_then(Value::as_array)
            .cloned()
            .unwrap_or_default();
        let mut entries: Vec<String> = Vec::new();
        let mut tally = PictureTally::default();
        let mut skipped = 0_usize;
        let mut unread = 0_usize;
        for (index, attachment) in listed.iter().enumerate() {
            if index >= MAX_LISTED_ATTACHMENTS {
                unread = listed.len() - index;
                break;
            }
            let (Some(filename), Some(id)) = (
                attachment.get("filename").and_then(Value::as_str),
                scalar_text(attachment.get("id")),
            ) else {
                continue;
            };
            if let Some(pattern) = &pattern
                && !pattern.is_match(filename).unwrap_or(false)
            {
                continue;
            }
            let kind = attachment_kind(filename);
            if kind == AttachmentKind::Unported {
                skipped += 1;
                continue;
            }
            let declared = attachment.get("size").and_then(Value::as_u64);
            let limit = if kind == AttachmentKind::Picture {
                if let Some(reason) = tally.refusal(declared) {
                    entries.push(format!(
                        "filename: {filename}\ncontent: {}",
                        repr_str_list(&[format!("[Image: {filename} - {reason}]")])
                    ));
                    continue;
                }
                tally.attempt();
                usize::try_from(MAX_PICTURE_BYTES).unwrap_or(usize::MAX)
            } else {
                MAX_TEXT_ATTACHMENT_BYTES
            };
            if declared.is_some_and(|size| size > limit as u64) {
                skipped += 1;
                continue;
            }
            let Some(bytes) = self.attachment_bytes(&id, filename, limit).await else {
                continue;
            };
            let documents: Vec<String> = match kind {
                AttachmentKind::Picture => {
                    if tally.take(bytes.len() as u64) {
                        vec![
                            self.attachment_picture_text(&mut tally, bytes, filename)
                                .await,
                        ]
                    } else {
                        vec![format!(
                            "[Image: {filename} - not described: the pictures of one call are limited]"
                        )]
                    }
                }
                AttachmentKind::Csv => {
                    let Ok(content) = String::from_utf8(bytes) else {
                        skipped += 1;
                        continue;
                    };
                    let content = universal_newlines(&content);
                    let mut rows = py_splitlines(&content);
                    while rows.last().is_some_and(|row| row.is_empty()) {
                        rows.pop();
                    }
                    if rows.is_empty() {
                        Vec::new()
                    } else {
                        vec![rows.join("\n")]
                    }
                }
                _ => {
                    let Ok(content) = String::from_utf8(bytes) else {
                        skipped += 1;
                        continue;
                    };
                    if content.is_empty() {
                        Vec::new()
                    } else {
                        vec![universal_newlines(&content)]
                    }
                }
            };
            entries.push(format!(
                "filename: {filename}\ncontent: {}",
                repr_str_list(&documents)
            ));
        }
        if skipped > 0 {
            tracing::info!(
                event = "jira_attachments_skipped",
                skipped_count = skipped,
                "attachments of a type this runtime does not parse were skipped"
            );
        }
        let mut output = entries.join("\n\n");
        if unread > 0 {
            let _ = write!(
                output,
                "\n\n[{unread} more attachments were not read: at most {MAX_LISTED_ATTACHMENTS} are read per call]"
            );
        }
        text(output, true)
    }

    /// The text of one attachment picture, as the SDK's image loader returns
    /// it; a failure every later picture would meet stops the model calls.
    async fn attachment_picture_text(
        &self,
        tally: &mut PictureTally,
        bytes: Vec<u8>,
        filename: &str,
    ) -> String {
        let Some(host) = self.attachments.as_ref() else {
            return "[No readable text detected by LLM]".to_owned();
        };
        match describe_for_text(host, bytes, filename, ATTACHMENT_IMAGE_PROMPT.to_owned()).await {
            Ok(description) if description.trim().is_empty() => {
                "[No readable text detected by LLM]".to_owned()
            }
            Ok(description) => description,
            Err(error) => {
                let note = if matches!(
                    error,
                    AttachmentError::Host(Step::Model, OfficeFailure::NoModel)
                ) {
                    "[No readable text detected by LLM]".to_owned()
                } else {
                    format!("[Image: {filename} - not described: {}]", error.sentence())
                };
                tally.stop(error);
                note
            }
        }
    }

    pub(super) async fn get_field_with_image_descriptions(
        &self,
        issue_key: &str,
        field_name: &str,
        prompt: Option<&str>,
        context_radius: i64,
        process_images: bool,
    ) -> Result<Value, JiraClientError> {
        const PREFIX: &str = "Error processing field with images";
        validate_identifier(issue_key)?;
        let mut url = self.api_url(self.version(), &["issue", issue_key])?;
        url.query_pairs_mut().append_pair("fields", field_name);
        let issue = match self.send(Method::GET, url, None, false).await? {
            Reply::Accepted(issue) => issue,
            Reply::Refused(message) => return text(format!("{PREFIX}: {message}"), false),
        };
        let Some(field) = issue
            .get("fields")
            .and_then(|fields| fields.get(field_name))
            .filter(|field| truthy(field))
        else {
            let mut url = self.api_url(self.version(), &["issue", issue_key])?;
            url.query_pairs_mut().append_pair("fields", "*all");
            let issue = match self.send(Method::GET, url, None, false).await? {
                Reply::Accepted(issue) => issue,
                Reply::Refused(message) => return text(format!("{PREFIX}: {message}"), false),
            };
            let existing = issue
                .get("fields")
                .and_then(Value::as_object)
                .map(|fields| {
                    fields
                        .iter()
                        .filter(|(_, value)| !value.is_null())
                        .map(|(name, _)| name.as_str())
                        .collect::<Vec<_>>()
                        .join(", ")
                })
                .unwrap_or_default();
            return text(
                format!(
                    "Unable to find field '{field_name}' or it's empty. Available fields are: {existing}"
                ),
                false,
            );
        };
        let content = extract_image_data(field);
        if !process_images {
            return text(
                format!("Field '{field_name}' from issue '{issue_key}':\n\n{content}"),
                false,
            );
        }
        let mut work = PictureWork::new(self, issue_key, prompt, context_radius);
        let augmented = work.replace(&content, &content, false).await?;
        if let Some(reason) = work.no_model {
            return text(format!("{PREFIX}: {reason}"), false);
        }
        text(
            format!(
                "Field '{field_name}' from issue '{issue_key}' with image descriptions:\n\n{augmented}"
            ),
            false,
        )
    }

    pub(super) async fn get_comments_with_image_descriptions(
        &self,
        issue_key: &str,
        prompt: Option<&str>,
        context_radius: i64,
        process_images: bool,
    ) -> Result<Value, JiraClientError> {
        const PREFIX: &str = "Error processing comments with images";
        validate_identifier(issue_key)?;
        let url = self.api_url(self.version(), &["issue", issue_key, "comment"])?;
        let comments = match self.send(Method::GET, url, None, false).await? {
            Reply::Accepted(comments) => comments,
            Reply::Refused(message) => return text(format!("{PREFIX}: {message}"), false),
        };
        let mut work = PictureWork::new(self, issue_key, prompt, context_radius);
        let mut output = format!("Comments from issue '{issue_key}' with image descriptions:\n\n");
        let mut count = 0_usize;
        for comment in comments
            .get("comments")
            .and_then(Value::as_array)
            .into_iter()
            .flatten()
        {
            let Some(body) = comment.get("body").filter(|body| truthy(body)) else {
                continue;
            };
            let author = comment
                .pointer("/author/displayName")
                .map_or_else(|| "Unknown".to_owned(), display_value);
            let created = comment
                .get("created")
                .map_or_else(|| "Unknown date".to_owned(), display_value);
            let body = extract_image_data(body);
            let processed = if process_images {
                work.replace(&body, &body, true).await?
            } else {
                body
            };
            count += 1;
            let _ = write!(
                output,
                "Comment #{count} by {author} on {created}:\n{processed}\n\n"
            );
        }
        if let Some(reason) = work.no_model {
            return text(format!("{PREFIX}: {reason}"), false);
        }
        if count == 0 {
            return text(format!("No comments found for issue '{issue_key}'"), false);
        }
        text(output, false)
    }
}

/// One upload that Jira stored.
struct Uploaded {
    filename: String,
    attachment_id: String,
    content_url: String,
    media_type: &'static str,
    /// The issue already had an attachment of this name.
    duplicate: bool,
}

/// The pictures of one field or comment list: found through the issue's
/// attachments, fetched through the authenticated client, described by the
/// run's model within the call's bounds.
struct PictureWork<'a> {
    client: &'a JiraClient,
    issue_key: &'a str,
    prompt: Option<&'a str>,
    radius: i64,
    resolver: Option<Resolver>,
    tally: PictureTally,
    /// Descriptions by attachment link: the same picture is described once.
    described: HashMap<String, String>,
    /// Set when the run has no chat model: the whole result then says so, as
    /// the SDK's "LLM is required" does.
    no_model: Option<String>,
}

impl<'a> PictureWork<'a> {
    fn new(
        client: &'a JiraClient,
        issue_key: &'a str,
        prompt: Option<&'a str>,
        radius: i64,
    ) -> Self {
        Self {
            client,
            issue_key,
            prompt,
            radius,
            resolver: None,
            tally: PictureTally::default(),
            described: HashMap::new(),
            no_model: None,
        }
    }

    /// `re.sub(IMAGE_REFERENCE, describe, content)`; `body` is the text the
    /// context comes from. `absolute_ok`: an absolute link of this Jira's own
    /// origin may be fetched when no attachment is named by it (comments).
    async fn replace(
        &mut self,
        content: &str,
        body: &str,
        absolute_ok: bool,
    ) -> Result<String, JiraClientError> {
        let pattern = Regex::new(IMAGE_REFERENCE).map_err(|_| invalid_input())?;
        let matches: Vec<(usize, usize, String, String)> = pattern
            .captures_iter(content)
            .filter_map(|captures| {
                let whole = captures.get(0)?;
                Some((
                    whole.start(),
                    whole.end(),
                    captures.get(1)?.as_str().to_owned(),
                    whole.as_str().to_owned(),
                ))
            })
            .collect();
        let mut output = String::with_capacity(content.len());
        let mut last = 0;
        for (start, end, reference, whole) in matches {
            output.push_str(&content[last..start]);
            output.push_str(&self.describe(&reference, &whole, body, absolute_ok).await?);
            last = end;
        }
        output.push_str(&content[last..]);
        Ok(output)
    }

    async fn describe(
        &mut self,
        reference: &str,
        whole: &str,
        body: &str,
        absolute_ok: bool,
    ) -> Result<String, JiraClientError> {
        if self.resolver.is_none() {
            let attachments = self.client.issue_attachments(self.issue_key).await?;
            self.resolver = Some(Resolver::new(&attachments));
        }
        let found = self
            .resolver
            .as_ref()
            .and_then(|resolver| resolver.find(reference))
            .cloned();
        let (content_url, name, declared) = if let Some(attachment) = found {
            {
                let Some(url) = attachment
                    .get("content")
                    .and_then(Value::as_str)
                    .filter(|url| !url.is_empty())
                else {
                    return Ok(format!("[Image: {reference} - no content URL]"));
                };
                (
                    url.to_owned(),
                    attachment
                        .get("filename")
                        .and_then(Value::as_str)
                        .unwrap_or(reference)
                        .to_owned(),
                    attachment.get("size").and_then(Value::as_u64),
                )
            }
        } else {
            {
                let own = (reference.starts_with("http://") || reference.starts_with("https://"))
                    && absolute_ok
                    && self.client.attachment_url(reference).is_some();
                if !own {
                    return Ok(format!("[Image: {reference} - attachment not found]"));
                }
                (
                    reference.to_owned(),
                    reference.rsplit('/').next().unwrap_or(reference).to_owned(),
                    None,
                )
            }
        };
        if let Some(description) = self.described.get(&content_url) {
            return Ok(format!("[Image {name} Description: {description}]"));
        }
        if let Some(reason) = self.tally.refusal(declared) {
            return Ok(format!("[Image: {reference} - {reason}]"));
        }
        self.tally.attempt();
        let Some(url) = self.client.attachment_url(&content_url) else {
            return Ok(format!("[Image: {reference} - download failed]"));
        };
        let Some(bytes) = self
            .client
            .download(
                url,
                usize::try_from(MAX_PICTURE_BYTES).unwrap_or(usize::MAX),
                true,
            )
            .await
        else {
            return Ok(format!("[Image: {reference} - download failed]"));
        };
        if !self.tally.take(bytes.len() as u64) {
            return Ok(format!(
                "[Image: {reference} - not described: the pictures of one call are limited]"
            ));
        }
        let context = collect_context(body, whole, self.radius);
        let prompt = picture_prompt(DEFAULT_IMAGE_PROMPT, self.prompt, &name, &context);
        let Some(host) = self.client.attachments.as_ref() else {
            let error = AttachmentError::Host(Step::Model, OfficeFailure::NoModel);
            self.no_model = Some(error.sentence());
            self.tally.stop(error);
            return Ok(format!("[Image: {reference} - not described]"));
        };
        match describe_for_text(host, bytes, &name, prompt).await {
            Ok(description) => {
                self.described.insert(content_url, description.clone());
                Ok(format!("[Image {name} Description: {description}]"))
            }
            Err(error) => {
                if matches!(
                    error,
                    AttachmentError::Host(Step::Model, OfficeFailure::NoModel)
                ) {
                    self.no_model = Some(error.sentence());
                }
                let sentence = error.sentence();
                self.tally.stop(error);
                Ok(format!("[Image: {reference} - not described: {sentence}]"))
            }
        }
    }
}

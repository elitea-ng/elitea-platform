//! The tools that read a file or a `OneNote` page as text: `read_document`,
//! `read_file_from_sharing_link`, and the page and attachment readers.
//!
//! Graph gives the bytes (bounded before they are buffered); the host's
//! document reader turns a PDF or an Office file into text. What this runtime
//! does not do, it says: it never describes a picture (the SDK's
//! `capture_images` / `is_capture_image` need a vision model the toolkits are
//! not lent, and without one the SDK renders the same `[image: <alt>]`
//! placeholder), and it never selects a page or a sheet of a document.

use std::sync::Arc;

use adk_core::{AdkError, ErrorCategory, ErrorComponent};
use serde_json::{Value, json};

use super::client::{MAX_READ_BYTES, SharePointApi, SharePointClientError};
use super::html::{self, AttachmentRef, Chunk};
use crate::toolkits::families::office_host::{
    OfficeToolHost, SharedOfficeToolHost, TextExtractionFailure,
};
use crate::toolkits::families::python_repr::repr_str_list;
use crate::toolkits::families::vcs_text::{
    MAX_OUTPUT_CHARS, guard_text_read, python_line_ranges, requested_label, slice_lines,
};

/// The most attachments one page read opens.
const MAX_ATTACHMENTS_READ: usize = 20;
/// The most bytes of attachments one page read downloads, together.
const MAX_ATTACHMENT_TOTAL_BYTES: usize = 2 * MAX_READ_BYTES;
/// The most characters of one attachment's text a page read inlines.
const MAX_ATTACHMENT_CHARS: usize = 50_000;

pub(super) const UNSUPPORTED_BLOCKED: &str = "Not supported type of files entered. Supported types are TXT, DOCX, PDF, PPTX, XLSX and XLS only.";

/// The extensions `read_document` refuses before it downloads anything (the
/// SDK's deny list of executables and compiled binaries).
const BLOCKED_EXTENSIONS: &[&str] = &[
    "exe", "dll", "com", "scr", "cpl", "sys", "drv", "msi", "msix", "bin", "so", "dylib", "o", "a",
    "lib", "obj", "elf", "out", "app", "dmg", "pkg", "deb", "rpm", "apk", "run", "class", "pyc",
    "pyo",
];

pub(super) fn is_blocked_binary(path: &str) -> bool {
    let name = path.rsplit('/').next().unwrap_or(path);
    name.rsplit_once('.')
        .filter(|(stem, _)| !stem.is_empty())
        .is_some_and(|(_, extension)| {
            let extension = extension.to_ascii_lowercase();
            BLOCKED_EXTENSIONS.contains(&extension.as_str())
        })
}

/// Why a read failed: Graph (which may mean a token to renew), or a refusal
/// the tool words itself.
pub(super) enum Failure {
    Client(SharePointClientError),
    Tool(AdkError),
}

impl From<SharePointClientError> for Failure {
    fn from(error: SharePointClientError) -> Self {
        Self::Client(error)
    }
}

impl From<AdkError> for Failure {
    fn from(error: AdkError) -> Self {
        Self::Tool(error)
    }
}

fn refusal(code: &'static str, message: &'static str) -> AdkError {
    AdkError::new(
        ErrorComponent::Tool,
        ErrorCategory::InvalidInput,
        code,
        message,
    )
}

/// Why a document could not be turned into text, one sentence per cause.
pub(super) fn text_failure(failure: TextExtractionFailure) -> AdkError {
    match failure {
        TextExtractionFailure::NoExtractor => AdkError::new(
            ErrorComponent::Tool,
            ErrorCategory::Unsupported,
            "sharepoint.file.no_reader",
            "this runtime lends no document reader, so the file's text cannot be read",
        ),
        TextExtractionFailure::Unsupported => refusal(
            "sharepoint.file.unsupported",
            "the file type is not supported: documents (PDF, DOCX, XLSX, PPTX, DOC, XLS, PPT), text files (TXT, MD, CSV, JSON, HTML, XML) and common code files are read; images, archives (ZIP, RAR), media (MP4, MP3) and executables are not",
        ),
        TextExtractionFailure::Unreadable => refusal(
            "sharepoint.file.unreadable",
            "the file could not be read: it is encrypted, damaged, or holds no text",
        ),
        TextExtractionFailure::TooLarge => refusal(
            "sharepoint.file.reader_limit",
            "the file is over a limit of the document reader (its size, page count or text)",
        ),
        TextExtractionFailure::TimedOut => AdkError::new(
            ErrorComponent::Tool,
            ErrorCategory::Timeout,
            "sharepoint.file.reader_timeout",
            "reading the file took longer than the document reader allows",
        ),
        TextExtractionFailure::Unreachable => AdkError::new(
            ErrorComponent::Tool,
            ErrorCategory::Unavailable,
            "sharepoint.file.reader_unreachable",
            "the document reader could not be reached",
        ),
    }
}

/// The host a text read needs, before any byte is downloaded.
pub(super) fn reader_host(
    host: Option<&SharedOfficeToolHost>,
) -> Result<&SharedOfficeToolHost, AdkError> {
    host.ok_or_else(|| text_failure(TextExtractionFailure::NoExtractor))
}

pub(super) async fn extract(
    host: &SharedOfficeToolHost,
    name: &str,
    bytes: Vec<u8>,
) -> Result<String, AdkError> {
    OfficeToolHost::extract_text(host.as_ref(), name, bytes)
        .await
        .map_err(text_failure)
}

/// A line slice and the read limit, as the SDK's `bound_read_result`: the
/// text, or the guidance object when it is over the limit. An empty range is
/// an error that names the file's line count, never an empty answer.
pub(super) fn bound_text(
    full: &str,
    path: &str,
    start_line: Option<usize>,
    end_line: Option<usize>,
) -> Result<Value, AdkError> {
    if let (Some(start), Some(end)) = (start_line, end_line)
        && end < start
    {
        return Err(refusal(
            "sharepoint.read.range_inverted",
            "the line range is inverted: end_line is before start_line",
        ));
    }
    let content = slice_lines(full, start_line, end_line);
    if content.is_empty() && !full.is_empty() && (start_line.is_some() || end_line.is_some()) {
        return Err(refusal(
            "sharepoint.read.range_empty",
            "the requested line range is empty: start_line is past the end of the file",
        ));
    }
    let requested = requested_label(start_line, end_line);
    Ok(guard_text_read(content, path, &requested, full))
}

/// The options of the SDK read tools that this runtime reads past, said
/// before the content so a whole document is not taken for the slice that
/// was asked for.
pub(super) fn unapplied_notice(options: &[&'static str]) -> Option<String> {
    (!options.is_empty()).then(|| {
        format!(
            "[{} not applied: this runtime returns the document's text as a whole]\n",
            options.join(", ")
        )
    })
}

/// One item of a `OneNote` page.
pub(super) enum PageItem {
    Text(String),
    Image {
        description: String,
    },
    Attachment {
        name: String,
        download_url: String,
        /// The attachment's text, when it was asked for and read.
        content: Option<String>,
        /// Why asked-for text is missing. The SDK leaves such an attachment
        /// bare, which reads as an attachment with no text.
        missing: Option<&'static str>,
    },
}

/// How a page is read.
#[derive(Clone, Copy)]
pub(super) struct PageOptions {
    pub(super) include_attachments: bool,
    pub(super) read_attachment_content: bool,
}

/// The page as typed items in document order.
pub(super) async fn page_items(
    api: &dyn SharePointApi,
    host: Option<&SharedOfficeToolHost>,
    page_id: &str,
    options: PageOptions,
) -> Result<Vec<PageItem>, SharePointClientError> {
    let markup = api.onenote_get_page_content(page_id).await?;
    let markup = markup.as_str().unwrap_or_default();
    let page = html::parse_page(markup);

    // Attachment text, read for at most MAX_ATTACHMENTS_READ attachments and
    // MAX_ATTACHMENT_TOTAL_BYTES together. An attachment that is not read says
    // why.
    let attachments = page.attachments.len();
    let mut contents: Vec<Option<String>> = vec![None; attachments];
    let mut missing: Vec<Option<&'static str>> = vec![None; attachments];
    if options.read_attachment_content
        && options.include_attachments
        && let Some(host) = host
    {
        let mut downloaded = 0_usize;
        for (index, attachment) in page.attachments.iter().enumerate() {
            if index >= MAX_ATTACHMENTS_READ {
                missing[index] = Some("a page read opens at most 20 attachments");
                continue;
            }
            let budget = MAX_ATTACHMENT_TOTAL_BYTES.saturating_sub(downloaded);
            if budget == 0 {
                missing[index] =
                    Some("the attachments of this page together are over the read budget");
                continue;
            }
            let bytes = match attachment_bytes(api, attachment, budget).await? {
                Ok(bytes) => bytes,
                Err(reason) => {
                    missing[index] = Some(reason);
                    continue;
                }
            };
            downloaded = downloaded.saturating_add(bytes.len());
            match extract(host, &attachment.name, bytes).await {
                Ok(text) => match bounded_attachment_text(text) {
                    Some(text) => contents[index] = Some(text),
                    None => missing[index] = Some("the attachment holds no text"),
                },
                Err(_) => missing[index] = Some("the attachment could not be read as text"),
            }
        }
    }

    let mut items = Vec::new();
    let mut buffer = String::new();
    for chunk in &page.chunks {
        match chunk {
            Chunk::Text(text) => buffer.push_str(text),
            Chunk::Image(index) => {
                items.extend(html::flush_text(&mut buffer).map(PageItem::Text));
                let image = &page.images[*index];
                let alt = if image.alt.is_empty() {
                    "no description"
                } else {
                    image.alt.as_str()
                };
                items.push(PageItem::Image {
                    description: format!("[image: {alt}]"),
                });
            }
            Chunk::Attachment(index) => {
                items.extend(html::flush_text(&mut buffer).map(PageItem::Text));
                let attachment = &page.attachments[*index];
                items.push(PageItem::Attachment {
                    name: attachment.name.clone(),
                    download_url: resolved(api, &attachment.src).await?,
                    content: contents[*index].take(),
                    missing: missing[*index],
                });
            }
        }
    }
    items.extend(html::flush_text(&mut buffer).map(PageItem::Text));
    Ok(items)
}

/// An attachment's text, unless it starts like the SDK's own placeholder, cut
/// to the budget one attachment may take of a page read.
fn bounded_attachment_text(text: String) -> Option<String> {
    if text.is_empty() || text.starts_with("[attachment") {
        return None;
    }
    if text.chars().count() <= MAX_ATTACHMENT_CHARS {
        return Some(text);
    }
    let cut = text
        .char_indices()
        .nth(MAX_ATTACHMENT_CHARS)
        .map_or(text.len(), |(index, _)| index);
    Some(format!(
        "{}\n[attachment text cut at {MAX_ATTACHMENT_CHARS} characters]",
        &text[..cut]
    ))
}

async fn resolved(api: &dyn SharePointApi, src: &str) -> Result<String, SharePointClientError> {
    if src.is_empty() {
        Ok(String::new())
    } else {
        api.onenote_resource_url(src).await
    }
}

/// The bytes of one attachment, or why they are not read: it names no
/// resource of this site (an address that is not a `OneNote` resource is
/// never fetched) or Graph would not give them. A refused credential is an
/// error of the whole read.
async fn attachment_bytes(
    api: &dyn SharePointApi,
    attachment: &AttachmentRef,
    budget: usize,
) -> Result<Result<Vec<u8>, &'static str>, SharePointClientError> {
    let Some(resource) = html::resource_id(&attachment.src) else {
        return Ok(Err("the attachment is not a OneNote resource of this site"));
    };
    match api
        .onenote_download_resource(resource, budget.min(MAX_READ_BYTES))
        .await
    {
        Ok(bytes) => Ok(Ok(bytes)),
        Err(error) if error.code() == super::client::SharePointClientErrorCode::Authentication => {
            Err(error)
        }
        Err(error)
            if error.code() == super::client::SharePointClientErrorCode::ResourceExhausted =>
        {
            Ok(Err("the attachment is over the size this tool reads"))
        }
        Err(_) => Ok(Err("the attachment could not be downloaded")),
    }
}

/// `onenote_read_page`: the page as plain text, items split by `-----`.
pub(super) fn render_page(items: &[PageItem]) -> String {
    let parts = items.iter().filter_map(|item| {
        let part = match item {
            PageItem::Text(text) => text.clone(),
            PageItem::Image { description } => description.clone(),
            PageItem::Attachment {
                name,
                download_url,
                content,
                missing,
            } => {
                let mut block = format!("[attachment: {name}]\n  {download_url}");
                if let Some(content) = content {
                    block.push_str("\n  Content:\n");
                    block.push_str(&indent(content));
                } else if let Some(reason) = missing {
                    block.push_str("\n  Content: not available: ");
                    block.push_str(reason);
                }
                block
            }
        };
        (!part.is_empty()).then_some(part)
    });
    parts.collect::<Vec<_>>().join("\n-----\n")
}

/// `onenote_read_page_items`: the same items, each as the SDK's typed item
/// prints, items split by `-----`.
pub(super) fn render_items(items: &[PageItem]) -> String {
    let parts = items.iter().filter_map(|item| {
        let part = match item {
            PageItem::Text(text) => text.clone(),
            PageItem::Image { description } => description.clone(),
            PageItem::Attachment {
                name,
                download_url,
                content,
                missing,
            } => {
                let mut text = format!("[attachment: {name}]");
                if !download_url.is_empty() {
                    text.push_str("\n  download_url: ");
                    text.push_str(download_url);
                }
                if let Some(content) = content {
                    text.push_str("\n  content:\n");
                    text.push_str(&indent(content));
                } else if let Some(reason) = missing {
                    text.push_str("\n  content: not available: ");
                    text.push_str(reason);
                }
                text
            }
        };
        (!part.trim().is_empty()).then_some(part)
    });
    parts.collect::<Vec<_>>().join("\n-----\n")
}

/// Each line of `text` behind two spaces, as `str.splitlines` splits it.
fn indent(text: &str) -> String {
    python_line_ranges(text)
        .into_iter()
        .map(|(start, end)| {
            let line = text[start..end].trim_end_matches(['\n', '\r']);
            format!("  {line}")
        })
        .collect::<Vec<_>>()
        .join("\n")
}

/// The most characters a page read returns: the read limit of every other
/// text read.
pub(super) fn limit_page_text(text: String) -> Result<Value, AdkError> {
    if text.chars().count() <= MAX_OUTPUT_CHARS {
        return Ok(Value::String(text));
    }
    Err(refusal(
        "sharepoint.onenote.page_too_large",
        "the page's text is over the 200000-character read limit: read its sections with onenote_get_page_content, or without attachment content",
    ))
}

/// `onenote_list_attachments`: name, resource id and canonical address of
/// every attachment of the page.
pub(super) async fn list_attachments(
    api: &dyn SharePointApi,
    page_id: &str,
) -> Result<Value, SharePointClientError> {
    let markup = api.onenote_get_page_content(page_id).await?;
    let mut listed = Vec::new();
    for attachment in html::attachments_of(markup.as_str().unwrap_or_default()) {
        listed.push(json!({
            "name": attachment.name,
            "resource_id": html::resource_id(&attachment.src),
            "download_url": resolved(api, &attachment.src).await?,
        }));
    }
    Ok(Value::Array(listed))
}

/// What `onenote_read_attachment` tells when the page has no such attachment.
pub(super) fn missing_attachment(page_id: &str, name: &str, available: &[String]) -> String {
    format!(
        "Attachment '{name}' not found on page '{page_id}'. Available attachments: {}",
        repr_str_list(available)
    )
}

/// `onenote_read_attachment`: the parsed text of one attachment.
pub(super) async fn read_attachment(
    api: &Arc<dyn SharePointApi>,
    host: Option<&SharedOfficeToolHost>,
    page_id: &str,
    attachment_name: &str,
) -> Result<String, Failure> {
    let markup = api.onenote_get_page_content(page_id).await?;
    let attachments = html::attachments_of(markup.as_str().unwrap_or_default());
    let Some(found) = attachments.iter().find(|a| a.name == attachment_name) else {
        let available = attachments
            .iter()
            .map(|a| a.name.clone())
            .collect::<Vec<_>>();
        return Err(refusal_owned(
            "sharepoint.onenote.attachment_missing",
            missing_attachment(page_id, attachment_name, &available),
        )
        .into());
    };
    let Some(resource) = html::resource_id(&found.src) else {
        return Err(refusal(
            "sharepoint.onenote.attachment_unresolvable",
            "the attachment has no resolvable download address on this site",
        )
        .into());
    };
    let host = reader_host(host)?;
    let bytes = api
        .onenote_download_resource(resource, MAX_READ_BYTES)
        .await?;
    let text = extract(host, &found.name, bytes).await?;
    if text.chars().count() <= MAX_OUTPUT_CHARS {
        return Ok(text);
    }
    let cut = text
        .char_indices()
        .nth(MAX_OUTPUT_CHARS)
        .map_or(text.len(), |(index, _)| index);
    Ok(format!(
        "{}\n[attachment text cut at {MAX_OUTPUT_CHARS} characters]",
        &text[..cut]
    ))
}

fn refusal_owned(code: &'static str, message: String) -> AdkError {
    AdkError::new(ErrorComponent::Tool, ErrorCategory::NotFound, code, message)
}

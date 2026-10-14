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
const fn failure_parts(
    failure: TextExtractionFailure,
) -> (ErrorCategory, &'static str, &'static str) {
    match failure {
        TextExtractionFailure::NoExtractor => (
            ErrorCategory::Unsupported,
            "sharepoint.file.no_reader",
            "this runtime lends no document reader for this kind of file, so its text cannot be read",
        ),
        TextExtractionFailure::Unsupported => (
            ErrorCategory::InvalidInput,
            "sharepoint.file.unsupported",
            "the file type is not supported: documents (PDF, DOCX, XLSX, PPTX, DOC, XLS, PPT), text files (TXT, MD, CSV, JSON, HTML, XML) and common code files are read; images, archives (ZIP, RAR), media (MP4, MP3) and executables are not",
        ),
        TextExtractionFailure::Unreadable => (
            ErrorCategory::InvalidInput,
            "sharepoint.file.unreadable",
            "the file could not be read: it is encrypted, damaged, or holds no text",
        ),
        TextExtractionFailure::TooLarge => (
            ErrorCategory::InvalidInput,
            "sharepoint.file.reader_limit",
            "the file is over a limit of the document reader (its size, page count or text)",
        ),
        TextExtractionFailure::TimedOut => (
            ErrorCategory::Timeout,
            "sharepoint.file.reader_timeout",
            "reading the file took longer than the document reader allows",
        ),
        TextExtractionFailure::Unreachable => (
            ErrorCategory::Unavailable,
            "sharepoint.file.reader_unreachable",
            "the document reader could not be reached",
        ),
    }
}

pub(super) fn text_failure(failure: TextExtractionFailure) -> AdkError {
    let (category, code, message) = failure_parts(failure);
    AdkError::new(ErrorComponent::Tool, category, code, message)
}

/// The host a text read needs, and a file it can read, BEFORE any byte is
/// downloaded: the reader exists, and the file's kind (by its name) is one it
/// reads.
pub(super) fn check_readable(host: &SharedOfficeToolHost, name: &str) -> Result<(), AdkError> {
    OfficeToolHost::can_read_text(host.as_ref(), name).map_err(text_failure)
}

/// [`check_readable`] as the sentence of a refusal, for a caller that checks
/// from inside the client; `None` when the file can be read.
pub(super) fn unreadable_reason(
    host: Option<&SharedOfficeToolHost>,
    name: &str,
) -> Option<&'static str> {
    let failure = match host {
        None => TextExtractionFailure::NoExtractor,
        Some(host) => OfficeToolHost::can_read_text(host.as_ref(), name).err()?,
    };
    Some(failure_parts(failure).2)
}

/// The file name at the end of a path (an address of a file, whatever its
/// form).
pub(super) fn file_name_of(path: &str) -> &str {
    path.rsplit('/').next().unwrap_or(path)
}

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

/// A cost, in characters, kept back for each attachment's notes and markers.
const ATTACHMENT_RESERVE_CHARS: usize = 300;
/// Under this much room an attachment's text is not worth starting.
const MIN_ATTACHMENT_CHARS: usize = 1_000;

/// The page as typed items in document order. The page's own text is never
/// lost to attachment text: attachments are read into what the page's read
/// limit leaves, in order, each as much as fits, and one that does not (or
/// cannot) be read says why.
pub(super) async fn page_items(
    api: &dyn SharePointApi,
    host: Option<&SharedOfficeToolHost>,
    page_id: &str,
    options: PageOptions,
) -> Result<Vec<PageItem>, SharePointClientError> {
    let markup = api.onenote_get_page_content(page_id).await?;
    let markup = markup.as_str().unwrap_or_default();
    let page = html::parse_page(markup);

    let mut items = Vec::new();
    // (position in `items`, position in `page.attachments`)
    let mut slots = Vec::new();
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
                slots.push((items.len(), *index));
                items.push(PageItem::Attachment {
                    name: attachment.name.clone(),
                    download_url: resolved(api, &attachment.src).await?,
                    content: None,
                    missing: None,
                });
            }
        }
    }
    items.extend(html::flush_text(&mut buffer).map(PageItem::Text));
    // Positions shift by none: text pushed after the last slot only appends.

    if options.read_attachment_content && options.include_attachments && !slots.is_empty() {
        fill_attachment_text(api, host, &page.attachments, &mut items, &slots).await?;
    }
    Ok(items)
}

/// Reads attachment text into `items` within what the page's read limit
/// leaves after the page's own text.
async fn fill_attachment_text(
    api: &dyn SharePointApi,
    host: Option<&SharedOfficeToolHost>,
    attachments: &[AttachmentRef],
    items: &mut [PageItem],
    slots: &[(usize, usize)],
) -> Result<(), SharePointClientError> {
    let Some(host) = host else {
        // The caller says "no document reader" once for the page.
        return Ok(());
    };
    let base = render_page(items).chars().count();
    let mut allowance =
        MAX_OUTPUT_CHARS.saturating_sub(base + ATTACHMENT_RESERVE_CHARS * slots.len());
    let mut downloaded = 0_usize;
    for (opened, (item_at, attachment_at)) in slots.iter().enumerate() {
        let attachment = &attachments[*attachment_at];
        let outcome: Result<String, &'static str> = async {
            if opened >= MAX_ATTACHMENTS_READ {
                return Ok(Err("a page read opens at most 20 attachments"));
            }
            if allowance < MIN_ATTACHMENT_CHARS {
                return Ok(Err(
                    "cut: the page read limit of 200000 characters was reached by the text before it",
                ));
            }
            let budget = MAX_ATTACHMENT_TOTAL_BYTES.saturating_sub(downloaded);
            if budget == 0 {
                return Ok(Err(
                    "the attachments of this page together are over the read budget",
                ));
            }
            if let Some(reason) = unreadable_reason(Some(host), &attachment.name) {
                return Ok(Err(reason));
            }
            let bytes = match attachment_bytes(api, attachment, budget).await? {
                Ok(bytes) => bytes,
                Err(reason) => return Ok(Err(reason)),
            };
            downloaded = downloaded.saturating_add(bytes.len());
            Ok(match extract(host, &attachment.name, bytes).await {
                Ok(text) if text.is_empty() || text.starts_with("[attachment") => {
                    Err("the attachment holds no text")
                }
                Ok(text) => Ok(text),
                Err(_) => Err("the attachment could not be read as text"),
            })
        }
        .await?;
        let PageItem::Attachment {
            content, missing, ..
        } = &mut items[*item_at]
        else {
            continue;
        };
        match outcome {
            Ok(text) => {
                let (fitted, cut) = fit_text(&text, allowance.min(MAX_ATTACHMENT_CHARS));
                allowance = allowance.saturating_sub(indent(&fitted).chars().count());
                *content = Some(if cut {
                    format!(
                        "{fitted}\n[attachment text cut: the page read limit leaves no more room]"
                    )
                } else {
                    fitted
                });
            }
            Err(reason) => *missing = Some(reason),
        }
    }
    Ok(())
}

/// The lines of `text` that fit `room` characters once indented (two spaces
/// and a line break each), and whether any were left out. A first line longer
/// than the room is cut inside the line.
fn fit_text(text: &str, room: usize) -> (String, bool) {
    let mut used = 0_usize;
    let mut end = 0_usize;
    for (start, stop) in python_line_ranges(text) {
        let line = text[start..stop].trim_end_matches(['\n', '\r']);
        let cost = line.chars().count() + 3;
        if used + cost > room {
            if end == 0 {
                let keep = room.saturating_sub(3);
                let cut = line
                    .char_indices()
                    .nth(keep)
                    .map_or(line.len(), |(index, _)| index);
                return (line[..cut].to_owned(), true);
            }
            return (text[..end].trim_end_matches(['\n', '\r']).to_owned(), true);
        }
        used += cost;
        end = stop;
    }
    (text.to_owned(), false)
}

async fn resolved(api: &dyn SharePointApi, src: &str) -> Result<String, SharePointClientError> {
    if src.is_empty() {
        Ok(String::new())
    } else {
        api.onenote_resource_url(src).await
    }
}

/// The bytes of one attachment, or why they are not read: it names no
/// `OneNote` resource of this site (any other address is never fetched) or
/// Graph would not give them. A refused credential is an error of the whole
/// read.
async fn attachment_bytes(
    api: &dyn SharePointApi,
    attachment: &AttachmentRef,
    budget: usize,
) -> Result<Result<Vec<u8>, &'static str>, SharePointClientError> {
    let Some(resource) = api.onenote_resource_id(&attachment.src).await? else {
        return Ok(Err("the attachment is not a OneNote resource of this site"));
    };
    match api
        .onenote_download_resource(&resource, budget.min(MAX_READ_BYTES))
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
/// text read. Text over it is cut at the limit and says so; it is never a
/// refusal, so a page is never lost to its size.
pub(super) fn limit_page_text(text: String) -> Value {
    if text.chars().count() <= MAX_OUTPUT_CHARS {
        return Value::String(text);
    }
    let cut = text
        .char_indices()
        .nth(MAX_OUTPUT_CHARS)
        .map_or(text.len(), |(index, _)| index);
    Value::String(format!(
        "{}\n[page text cut at {MAX_OUTPUT_CHARS} characters]",
        &text[..cut]
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
            "resource_id": api.onenote_resource_id(&attachment.src).await?,
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
    let Some(resource) = api.onenote_resource_id(&found.src).await? else {
        return Err(refusal(
            "sharepoint.onenote.attachment_unresolvable",
            "the attachment has no resolvable download address on this site",
        )
        .into());
    };
    let host = reader_host(host)?;
    check_readable(host, &found.name)?;
    let bytes = api
        .onenote_download_resource(&resource, MAX_READ_BYTES)
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

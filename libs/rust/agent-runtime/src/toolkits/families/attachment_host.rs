//! What the tracker and test-management families need of the platform to
//! attach files and describe pictures: the artifact store in and out, and the
//! run's chat model, both through the Office host
//! ([`super::office_host::OfficeToolHost`]) the cloud worker lends on the
//! live claim. No family opens a connection of its own to the platform and no
//! internal route exists for this (AGENTS.md: internal calls are gRPC over
//! mTLS, never new REST).
//!
//! Like the Office families, a host that lends none (the desktop until P6, a
//! pipeline node with no artifact authority) leaves the tools that need it
//! out of the toolset, with one warning, instead of binding tools that would
//! refuse every call.
//!
//! One sentence per failure cause is stated here ([`AttachmentError`]) so a
//! refused credential is not reported as an unreachable platform, a taken name
//! as a bad one, nor a lost answer on a write as a clean failure.

use std::fmt::Write as _;
use std::sync::Arc;

use adk_core::{AdkError, ErrorCategory, ErrorComponent};

use super::office_host::{
    MAX_OFFICE_OBJECT_BYTES, OfficeFailure, OfficeHostError, OfficeModelRequest,
    SharedOfficeToolHost, TOOLKIT_BUDGET_SENTENCE,
};

/// The most bytes of one artifact a tool reads to attach it: the host's own
/// cap, restated so a tool can refuse before it buffers.
pub(crate) const MAX_ATTACH_BYTES: u64 = MAX_OFFICE_OBJECT_BYTES;
/// The most bytes of one picture sent to the model: the indexing connectors'
/// bound for the same step (`elitea_connectors::pictures::MAX_PICTURE_BYTES`,
/// which this crate's feature set does not link).
pub(crate) const MAX_PICTURE_BYTES: u64 = 5 << 20;
/// The most bytes of all the pictures one tool call sends together
/// (`MAX_TOTAL_PICTURE_BYTES` there).
pub(crate) const MAX_TOTAL_PICTURE_BYTES: u64 = 12 << 20;
/// The most pictures one tool call describes (`MAX_PICTURES` there).
pub(crate) const MAX_PICTURES: usize = 20;
/// The longest description of one picture that is kept, in characters.
pub(crate) const MAX_DESCRIPTION_CHARS: usize = 8_000;
/// How many names `write_unique_artifact` tries before giving up.
const MAX_NAME_ATTEMPTS: u32 = 20;
/// The document engine admits a few documents at a time per process and
/// answers `Busy` at once; every caller waits its turn the same way: up to 8
/// retries at 250 ms, 500 ms, ... (9 s in all), under the engine's 20 s call
/// deadline. The Office families and the Carrier workbook tools share these.
pub(crate) const ENGINE_BUSY_RETRIES: u32 = 8;
pub(crate) const ENGINE_BUSY_BACKOFF: std::time::Duration = std::time::Duration::from_millis(250);
/// The sentence for an engine that stayed busy.
pub(crate) const ENGINE_BUSY: &str =
    "The document engine is busy with other work. Try again in a moment.";
/// The longest artifact path a tool accepts.
const MAX_PATH_BYTES: usize = 2_048;

/// The platform capability the attachment tools of a family use, shared for
/// one run. Cheap to clone.
#[derive(Clone)]
pub(crate) struct AttachmentHost {
    host: SharedOfficeToolHost,
}

/// One artifact read for an upload.
#[derive(Debug)]
pub(crate) struct ArtifactFile {
    pub(crate) bytes: Vec<u8>,
    /// The last segment of the artifact's key: the default upload name.
    pub(crate) name: String,
    /// Detected from the bytes, then the name (see [`detect_media_type`]).
    pub(crate) media_type: &'static str,
}

/// An artifact written.
#[derive(Debug, Clone, Eq, PartialEq)]
pub(crate) struct ArtifactStored {
    pub(crate) bucket: String,
    pub(crate) key: String,
    pub(crate) byte_length: u64,
}

/// What an artifact call was doing, so the sentence says what did and did not
/// happen.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum Step {
    /// Reading a file to attach: nothing has been sent anywhere yet.
    Read,
    /// Storing a file the tool produced.
    Write,
    /// A model call.
    Model,
}

/// Why an attachment step failed.
#[derive(Clone, Debug, Eq, PartialEq)]
pub(crate) enum AttachmentError {
    /// The artifact path is not `/{bucket}/{file}`.
    Path,
    /// The artifact holds no bytes.
    Empty,
    /// The file is over the tool's size cap (bytes).
    TooLarge(u64),
    /// The host failed, in one of the steps.
    Host(Step, OfficeFailure),
    /// The claim's toolkit completion cap refused the model call
    /// ([`OfficeHostError::toolkit_budget`]); nothing was asked.
    ToolkitBudget,
}

impl AttachmentError {
    /// The sentence the model reads. Carries no path, name or credential.
    pub(crate) fn sentence(&self) -> String {
        match self {
            Self::Path => {
                "The artifact path is not valid: use /{bucket}/{filename}, with no `..` segment."
                    .to_owned()
            }
            Self::Empty => "The artifact is empty, so there is nothing to attach.".to_owned(),
            Self::TooLarge(limit) => format!(
                "The file is larger than the {} MiB this tool accepts; nothing was sent.",
                limit / (1024 * 1024)
            ),
            Self::Host(Step::Read, failure) => read_sentence(*failure),
            Self::Host(Step::Write, failure) => write_sentence(*failure),
            Self::Host(Step::Model, failure) => model_sentence(*failure),
            Self::ToolkitBudget => {
                format!("{TOOLKIT_BUDGET_SENTENCE}. Do not retry; tell the user.")
            }
        }
    }

    fn category(&self) -> (ErrorCategory, &'static str) {
        match self {
            Self::Path | Self::Empty => (ErrorCategory::InvalidInput, "attachment.input.invalid"),
            Self::TooLarge(_) => (ErrorCategory::InvalidInput, "attachment.too_large"),
            Self::ToolkitBudget => (
                ErrorCategory::RateLimited,
                "attachment.toolkit_budget_exhausted",
            ),
            Self::Host(_, failure) => match failure {
                OfficeFailure::NotFound => (ErrorCategory::NotFound, "attachment.not_found"),
                OfficeFailure::AccessRefused => {
                    (ErrorCategory::Forbidden, "attachment.access_refused")
                }
                OfficeFailure::AlreadyExists | OfficeFailure::Conflict => {
                    (ErrorCategory::InvalidInput, "attachment.conflict")
                }
                OfficeFailure::UnsupportedType
                | OfficeFailure::InvalidRequest
                | OfficeFailure::TooLarge
                | OfficeFailure::ReplyTooLarge => {
                    (ErrorCategory::InvalidInput, "attachment.request_refused")
                }
                OfficeFailure::BudgetRefused => {
                    (ErrorCategory::RateLimited, "attachment.budget_refused")
                }
                OfficeFailure::NoModel => (ErrorCategory::Unavailable, "attachment.no_model"),
                OfficeFailure::RateLimited => {
                    (ErrorCategory::RateLimited, "attachment.rate_limited")
                }
                OfficeFailure::Timeout => (ErrorCategory::Timeout, "attachment.timeout"),
                OfficeFailure::ReplyUnusable => {
                    (ErrorCategory::Internal, "attachment.reply_unusable")
                }
                OfficeFailure::Unreachable => {
                    (ErrorCategory::Unavailable, "attachment.unreachable")
                }
                OfficeFailure::Unsupported => {
                    (ErrorCategory::Unavailable, "attachment.unsupported")
                }
            },
        }
    }

    /// The tool error the run sees: the sentence above, with a stable code.
    pub(crate) fn into_adk(self) -> AdkError {
        let (category, code) = self.category();
        AdkError::new(ErrorComponent::Tool, category, code, self.sentence())
    }
}

impl From<AttachmentError> for AdkError {
    fn from(error: AttachmentError) -> Self {
        error.into_adk()
    }
}

fn read_sentence(failure: OfficeFailure) -> String {
    match failure {
        OfficeFailure::NotFound => "The artifact was not found. Check the path (/{bucket}/{filename}); nothing was sent.".to_owned(),
        OfficeFailure::AccessRefused => "The platform refused access to the artifact store. Do not retry; tell the user. Nothing was sent.".to_owned(),
        OfficeFailure::TooLarge => "The artifact is larger than the platform accepts; nothing was sent.".to_owned(),
        OfficeFailure::InvalidRequest | OfficeFailure::UnsupportedType => {
            "The artifact path or bucket was refused as invalid; nothing was sent.".to_owned()
        }
        _ => "The artifact store could not be reached; nothing was sent. Trying again may work.".to_owned(),
    }
}

fn write_sentence(failure: OfficeFailure) -> String {
    match failure {
        OfficeFailure::AlreadyExists => "A file with that name already exists in the bucket and was not replaced. Choose another name.".to_owned(),
        OfficeFailure::AccessRefused => "The platform refused access to the artifact store. Do not retry; tell the user.".to_owned(),
        OfficeFailure::NotFound => "The bucket was not found; nothing was stored.".to_owned(),
        OfficeFailure::UnsupportedType => "The artifact store does not accept that type of file; nothing was stored.".to_owned(),
        OfficeFailure::InvalidRequest => "The file name or bucket was refused as invalid; nothing was stored.".to_owned(),
        OfficeFailure::TooLarge => "The file is larger than the platform accepts; nothing was stored.".to_owned(),
        _ => "The answer from the artifact store was lost, so the file may or may not have been stored. List the bucket before retrying.".to_owned(),
    }
}

fn model_sentence(failure: OfficeFailure) -> String {
    match failure {
        OfficeFailure::BudgetRefused => "The model budget is exhausted. Do not retry; tell the user.".to_owned(),
        OfficeFailure::NoModel => "This tool needs a chat model and none is bound in this run (a pipeline node binds none). Run it from an agent, or turn picture description off.".to_owned(),
        OfficeFailure::RateLimited => "The model gateway is rate-limiting this call. Try again shortly.".to_owned(),
        OfficeFailure::Timeout => "The model call timed out. It may work if tried again.".to_owned(),
        OfficeFailure::Conflict => "The model gateway reported a conflict. It may work if tried again.".to_owned(),
        OfficeFailure::ReplyUnusable | OfficeFailure::ReplyTooLarge => "The model's reply could not be used. Do not retry without telling the user.".to_owned(),
        OfficeFailure::AccessRefused => "The model gateway refused the call. Do not retry; tell the user.".to_owned(),
        _ => "The model gateway could not be reached. Trying again may work.".to_owned(),
    }
}

impl AttachmentHost {
    pub(crate) fn new(host: SharedOfficeToolHost) -> Self {
        Self { host }
    }

    /// Read one artifact by its path (`/{bucket}/{filename}`; a key with
    /// slashes is allowed), at most `max_bytes`. A larger object is refused
    /// from its metadata when the host can tell, and is never buffered past
    /// the cap either way.
    pub(crate) async fn read_artifact(
        &self,
        filepath: &str,
        max_bytes: u64,
    ) -> Result<ArtifactFile, AttachmentError> {
        let (bucket, key) = parse_artifact_path(filepath)?;
        let max_bytes = max_bytes.min(MAX_ATTACH_BYTES);
        match self.host.object_size(&bucket, &key).await {
            Ok(Some(size)) if size > max_bytes => return Err(AttachmentError::TooLarge(max_bytes)),
            Ok(_) => {}
            Err(error) => return Err(Self::failed(Step::Read, &error)),
        }
        let bytes = self
            .host
            .read_object(&bucket, &key, max_bytes)
            .await
            .map_err(|error| match error.kind() {
                OfficeFailure::TooLarge => AttachmentError::TooLarge(max_bytes),
                _ => Self::failed(Step::Read, &error),
            })?;
        if bytes.is_empty() {
            return Err(AttachmentError::Empty);
        }
        if bytes.len() as u64 > max_bytes {
            return Err(AttachmentError::TooLarge(max_bytes));
        }
        let name = key.rsplit('/').next().unwrap_or(&key).to_owned();
        let media_type = detect_media_type(&bytes, &name);
        Ok(ArtifactFile {
            bytes,
            name,
            media_type,
        })
    }

    /// Store a file the tool produced, never replacing an existing one.
    pub(crate) async fn write_artifact(
        &self,
        bucket: &str,
        key: &str,
        media_type: &str,
        bytes: Vec<u8>,
    ) -> Result<ArtifactStored, AttachmentError> {
        let stored = self
            .host
            .write_object(bucket, key, media_type, bytes, false)
            .await
            .map_err(|error| Self::failed(Step::Write, &error))?;
        Ok(ArtifactStored {
            bucket: stored.bucket,
            key: stored.name,
            byte_length: stored.byte_length,
        })
    }

    /// Store a file under `{stem}.{ext}`, or `{stem}-2.{ext}`, `{stem}-3.{ext}`
    /// ... when the name is taken: a tool that picks the name itself never
    /// replaces a file and reports the name it ended up with. A lost answer
    /// is not retried under another name (the first attempt may have stored).
    pub(crate) async fn write_unique_artifact(
        &self,
        bucket: &str,
        stem: &str,
        ext: &str,
        media_type: &str,
        bytes: Vec<u8>,
    ) -> Result<ArtifactStored, AttachmentError> {
        self.write_unique(bucket, stem, ext, media_type, bytes, false)
            .await
    }

    /// [`Self::write_unique_artifact`] for a bucket the TOOL derived (not one
    /// the user or model named): a missing bucket is created once, an
    /// existing one is success. Never used for a supplied bucket, which is
    /// the caller's to create on purpose.
    pub(crate) async fn write_unique_artifact_creating_bucket(
        &self,
        bucket: &str,
        stem: &str,
        ext: &str,
        media_type: &str,
        bytes: Vec<u8>,
    ) -> Result<ArtifactStored, AttachmentError> {
        self.write_unique(bucket, stem, ext, media_type, bytes, true)
            .await
    }

    async fn write_unique(
        &self,
        bucket: &str,
        stem: &str,
        ext: &str,
        media_type: &str,
        bytes: Vec<u8>,
        create_bucket: bool,
    ) -> Result<ArtifactStored, AttachmentError> {
        let mut last = AttachmentError::Host(Step::Write, OfficeFailure::AlreadyExists);
        let mut bucket_ensured = false;
        let mut attempt = 1;
        while attempt <= MAX_NAME_ATTEMPTS {
            let key = if attempt == 1 {
                format!("{stem}.{ext}")
            } else {
                format!("{stem}-{attempt}.{ext}")
            };
            match self
                .write_artifact(bucket, &key, media_type, bytes.clone())
                .await
            {
                Err(error @ AttachmentError::Host(_, OfficeFailure::AlreadyExists)) => {
                    last = error;
                    attempt += 1;
                }
                // A bucket this tool derives may not exist in a fresh
                // project: create it once, then write again (the same name).
                Err(AttachmentError::Host(Step::Write, OfficeFailure::NotFound))
                    if create_bucket && !bucket_ensured =>
                {
                    bucket_ensured = true;
                    self.ensure_bucket(bucket).await?;
                }
                other => return other,
            }
        }
        Err(last)
    }

    /// Create `bucket` unless it exists (a bucket that is already there is
    /// success). A host that cannot create buckets, or a refusal, is reported
    /// as the write's failure: nothing was stored.
    pub(crate) async fn ensure_bucket(&self, bucket: &str) -> Result<(), AttachmentError> {
        match self.host.create_bucket(bucket, None).await {
            Ok(_) => Ok(()),
            Err(error) if error.kind() == OfficeFailure::AlreadyExists => Ok(()),
            Err(error) => Err(Self::failed(Step::Write, &error)),
        }
    }

    /// Ask the run's chat model one question with pictures beside it, accounted
    /// and budgeted by the model gateway like every other call the run makes.
    /// `pictures` are `(bytes, media type)`, at most [`MAX_PICTURES`], each at
    /// most [`MAX_PICTURE_BYTES`]; the caller has checked the media types with
    /// [`is_describable_image`].
    pub(crate) async fn ask_model(
        &self,
        prompt: String,
        pictures: Vec<(Vec<u8>, &'static str)>,
    ) -> Result<String, AttachmentError> {
        let (images, media_types): (Vec<Vec<u8>>, Vec<String>) = pictures
            .into_iter()
            .map(|(bytes, media_type)| (bytes, media_type.to_owned()))
            .unzip();
        let reply = self
            .host
            .complete(OfficeModelRequest {
                prompt,
                images_png: Arc::from(images),
                image_media_types: Arc::from(media_types),
            })
            .await
            .map_err(|error| Self::failed(Step::Model, &error))?;
        Ok(clip(reply.trim(), MAX_DESCRIPTION_CHARS))
    }

    /// Describe one picture with the run's chat model.
    pub(crate) async fn describe_picture(
        &self,
        bytes: Vec<u8>,
        media_type: &'static str,
        prompt: String,
    ) -> Result<String, AttachmentError> {
        self.ask_model(prompt, vec![(bytes, media_type)]).await
    }

    fn failed(step: Step, error: &OfficeHostError) -> AttachmentError {
        tracing::warn!(
            event = "attachment_host_failed",
            reason_code = error.reason_code(),
            step = ?step,
            "a tracker tool's platform call failed"
        );
        if error.is_toolkit_budget() {
            return AttachmentError::ToolkitBudget;
        }
        AttachmentError::Host(step, error.kind())
    }
}

fn clip(text: &str, limit: usize) -> String {
    if text.chars().count() <= limit {
        return text.to_owned();
    }
    text.chars().take(limit).collect()
}

/// A bucket name the platform accepts for a bucket a tool derives from
/// other text: `{prefix}-{text}` with the text lowercased and every other
/// character a hyphen, at most 63 bytes. Deterministic, and two different
/// texts do not share a bucket: when the text had to be changed (or cut), a
/// short digest of the original is appended, so `a_b` and `a b` stay apart.
pub(crate) fn derived_bucket_name(prefix: &str, raw: &str) -> String {
    let mut text = String::new();
    for c in raw.chars() {
        let c = c.to_ascii_lowercase();
        if c.is_ascii_lowercase() || c.is_ascii_digit() {
            text.push(c);
        } else if !text.ends_with('-') {
            text.push('-');
        }
    }
    let text = text.trim_matches('-');
    let mut name = format!("{prefix}-{text}");
    if text != raw || name.len() > 63 {
        let digest = ring::digest::digest(&ring::digest::SHA256, raw.as_bytes());
        let mut suffix = String::from("-");
        for byte in &digest.as_ref()[..4] {
            let _ = write!(suffix, "{byte:02x}");
        }
        let room = 63 - suffix.len();
        name.truncate(room);
        while name.ends_with('-') {
            name.pop();
        }
        name.push_str(&suffix);
    }
    name
}

/// `/{bucket}/{key}` (the form the platform shows for a file) or
/// `{bucket}/{key}` into the bucket and the key. The key may hold slashes.
pub(crate) fn parse_artifact_path(path: &str) -> Result<(String, String), AttachmentError> {
    let trimmed = path.trim();
    if trimmed.is_empty()
        || trimmed.len() > MAX_PATH_BYTES
        || trimmed.chars().any(char::is_control)
        || trimmed.contains(['\\', '?', '#'])
    {
        return Err(AttachmentError::Path);
    }
    let rooted = trimmed.trim_start_matches('/');
    let (bucket, key) = rooted.split_once('/').ok_or(AttachmentError::Path)?;
    if bucket.is_empty()
        || key.is_empty()
        || key.ends_with('/')
        || key
            .split('/')
            .any(|segment| segment.is_empty() || segment == ".." || segment == ".")
    {
        return Err(AttachmentError::Path);
    }
    Ok((bucket.to_owned(), key.to_owned()))
}

/// The media type of a file: the bytes first (a picture is a picture whatever
/// its name), then the SDK's extension table (`detect_mime_type`).
pub(crate) fn detect_media_type(bytes: &[u8], name: &str) -> &'static str {
    if let Some(media_type) = sniff_image(bytes) {
        return media_type;
    }
    if bytes.starts_with(b"%PDF-") {
        return "application/pdf";
    }
    let lower = name.to_ascii_lowercase();
    let extension = lower
        .rsplit_once('.')
        .map_or("", |(_, extension)| extension);
    match extension {
        "png" => "image/png",
        "jpg" | "jpeg" => "image/jpeg",
        "gif" => "image/gif",
        "bmp" => "image/bmp",
        "webp" => "image/webp",
        "svg" => "image/svg+xml",
        "pdf" => "application/pdf",
        "doc" | "docx" => "application/msword",
        "xls" | "xlsx" => "application/vnd.ms-excel",
        "ppt" | "pptx" => "application/vnd.ms-powerpoint",
        "txt" | "log" | "md" => "text/plain",
        "csv" => "text/csv",
        "html" | "htm" => "text/html",
        "json" => "application/json",
        "xml" => "application/xml",
        "zip" => "application/zip",
        "mp4" => "video/mp4",
        "mov" => "video/quicktime",
        "webm" => "video/webm",
        _ => "application/octet-stream",
    }
}

/// The image types the model facade sends as pictures.
fn sniff_image(bytes: &[u8]) -> Option<&'static str> {
    if bytes.starts_with(&[0x89, b'P', b'N', b'G', 0x0D, 0x0A, 0x1A, 0x0A]) {
        Some("image/png")
    } else if bytes.starts_with(&[0xFF, 0xD8, 0xFF]) {
        Some("image/jpeg")
    } else if bytes.starts_with(b"GIF87a") || bytes.starts_with(b"GIF89a") {
        Some("image/gif")
    } else if bytes.len() >= 12 && &bytes[..4] == b"RIFF" && &bytes[8..12] == b"WEBP" {
        Some("image/webp")
    } else {
        None
    }
}

/// Whether the model takes this type as a picture.
pub(crate) fn is_describable_image(media_type: &str) -> bool {
    matches!(
        media_type,
        "image/png" | "image/jpeg" | "image/gif" | "image/webp"
    )
}

/// The wiki-markup reference Jira and Confluence both use for an uploaded
/// file (`_get_file_markup`): pictures and videos inline, others as links.
pub(crate) fn is_inline_media(media_type: &str) -> bool {
    media_type.starts_with("image/") || media_type.starts_with("video/")
}

/// The SDK's `_process_image_with_llm` prompt assembly (Jira and Confluence
/// alike): the custom prompt when one is given and not empty, else the
/// family's default, then the image name and the surrounding text.
pub(crate) fn picture_prompt(
    base: &str,
    custom: Option<&str>,
    name: &str,
    context: &str,
) -> String {
    let mut prompt = match custom {
        Some(custom) if !custom.is_empty() => custom.to_owned(),
        _ => base.to_owned(),
    };
    if !name.is_empty() || !context.is_empty() {
        prompt.push_str("\n\n## Additional Context Information:\n");
        if !name.is_empty() {
            prompt.push_str("- Image Name/Reference: ");
            prompt.push_str(name);
            prompt.push('\n');
        }
        if !context.is_empty() {
            prompt.push_str("- Surrounding Content: ");
            prompt.push_str(context);
            prompt.push('\n');
        }
        prompt.push_str(
            "\nPlease incorporate this contextual information in your description when relevant.",
        );
    }
    prompt
}

/// Describe one picture the way the SDK's `_process_image_with_llm` returns
/// it: the model's text, or a bracketed sentence when this one picture cannot
/// be described. `Err` is a failure that every later picture of the call
/// would meet too (no model, an exhausted budget, a refused or unreachable
/// gateway, a rate limit): the caller stops calling the model and reports it.
pub(crate) async fn describe_for_text(
    host: &AttachmentHost,
    bytes: Vec<u8>,
    name: &str,
    prompt: String,
) -> Result<String, AttachmentError> {
    let media_type = detect_media_type(&bytes, name);
    if !is_describable_image(media_type) {
        return Ok(format!("[Could not identify image format for {name}]"));
    }
    match host.describe_picture(bytes, media_type, prompt).await {
        Ok(description) => Ok(description),
        Err(
            error @ (AttachmentError::Host(
                Step::Model,
                OfficeFailure::BudgetRefused
                | OfficeFailure::NoModel
                | OfficeFailure::RateLimited
                | OfficeFailure::AccessRefused
                | OfficeFailure::Unreachable,
            )
            | AttachmentError::ToolkitBudget),
        ) => Err(error),
        Err(error) => Ok(format!("[Image processing error: {}]", error.sentence())),
    }
}

/// The bounds one tool call keeps on the pictures it sends to the model:
/// [`MAX_PICTURES`] of them, [`MAX_PICTURE_BYTES`] each,
/// [`MAX_TOTAL_PICTURE_BYTES`] together, and none after a failure that every
/// later picture would meet.
#[derive(Debug, Default)]
pub(crate) struct PictureTally {
    attempted: usize,
    bytes: u64,
    stopped: Option<AttachmentError>,
}

impl PictureTally {
    /// Why the next picture may not be fetched (it is left out with this
    /// reason), or `None` when it may. `declared` is the provider's stated
    /// size, when it states one.
    pub(crate) fn refusal(&self, declared: Option<u64>) -> Option<String> {
        if let Some(error) = &self.stopped {
            return Some(format!("not described: {}", error.sentence()));
        }
        if self.attempted >= MAX_PICTURES {
            return Some(format!(
                "not described: at most {MAX_PICTURES} pictures are described per call"
            ));
        }
        if let Some(size) = declared {
            if size > MAX_PICTURE_BYTES {
                return Some(format!(
                    "not described: larger than {} MiB",
                    MAX_PICTURE_BYTES / (1024 * 1024)
                ));
            }
            if self.bytes.saturating_add(size) > MAX_TOTAL_PICTURE_BYTES {
                return Some(format!(
                    "not described: the pictures of one call are limited to {} MiB together",
                    MAX_TOTAL_PICTURE_BYTES / (1024 * 1024)
                ));
            }
        }
        None
    }

    /// A picture is about to be fetched.
    pub(crate) fn attempt(&mut self) {
        self.attempted += 1;
    }

    /// `true` when a fetched picture of `length` bytes fits the call's total;
    /// it is then counted.
    pub(crate) fn take(&mut self, length: u64) -> bool {
        if length > MAX_PICTURE_BYTES || self.bytes.saturating_add(length) > MAX_TOTAL_PICTURE_BYTES
        {
            return false;
        }
        self.bytes += length;
        true
    }

    pub(crate) fn stop(&mut self, error: AttachmentError) {
        self.stopped.get_or_insert(error);
    }
}

/// One `multipart/form-data` request body: text fields, then one file part.
pub(crate) struct MultipartBody {
    pub(crate) content_type: String,
    pub(crate) bytes: Vec<u8>,
}

/// Build the body of an upload. The boundary is random per body and is
/// checked against every part (a boundary that occurs in a payload is
/// replaced); `file_name` is sent as it is, UTF-8 included, exactly as Python
/// `requests` sends it (a quote, a backslash and control characters cannot
/// end the header line: see [`header_quote`]).
pub(crate) fn multipart_upload(
    fields: &[(&str, &str)],
    file_field: &str,
    file_name: &str,
    file_media_type: &str,
    file: &[u8],
) -> MultipartBody {
    multipart_with(
        &mut random_boundary,
        fields,
        file_field,
        file_name,
        file_media_type,
        file,
    )
}

/// A boundary nobody can predict: 128 random bits. Where the system source
/// fails, a process-wide counter mixed with the clock keeps boundaries
/// distinct; the caller's occurrence check, not secrecy, is what makes the
/// body well-formed either way.
fn random_boundary() -> String {
    use ring::rand::SecureRandom as _;
    static COUNTER: std::sync::atomic::AtomicU64 = std::sync::atomic::AtomicU64::new(0);
    let mut random = [0_u8; 16];
    if ring::rand::SystemRandom::new().fill(&mut random).is_err() {
        let nanos = std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .map_or(0, |elapsed| elapsed.as_nanos());
        let count = COUNTER.fetch_add(1, std::sync::atomic::Ordering::Relaxed);
        random[..8].copy_from_slice(&count.to_be_bytes());
        random[8..].copy_from_slice(&nanos.to_be_bytes()[8..]);
    }
    let mut boundary = String::from("----elitea");
    for byte in random {
        let _ = write!(boundary, "{byte:02x}");
    }
    boundary
}

fn contains_bytes(haystack: &[u8], needle: &[u8]) -> bool {
    !needle.is_empty()
        && haystack
            .windows(needle.len())
            .any(|window| window == needle)
}

fn multipart_with(
    next_boundary: &mut dyn FnMut() -> String,
    fields: &[(&str, &str)],
    file_field: &str,
    file_name: &str,
    file_media_type: &str,
    file: &[u8],
) -> MultipartBody {
    // Regenerate until the delimiter occurs in no part payload.
    let mut boundary = next_boundary();
    for _ in 0..64 {
        let delimiter = format!("--{boundary}");
        if !contains_bytes(file, delimiter.as_bytes())
            && !fields
                .iter()
                .any(|(_, value)| value.contains(delimiter.as_str()))
        {
            break;
        }
        boundary = next_boundary();
    }
    let delimiter = format!("--{boundary}");
    let mut bytes = Vec::with_capacity(file.len() + 1024);
    for (name, value) in fields {
        bytes.extend_from_slice(
            format!(
                "{delimiter}\r\nContent-Disposition: form-data; name=\"{}\"\r\n\r\n{value}\r\n",
                header_quote(name)
            )
            .as_bytes(),
        );
    }
    let shown = match header_quote(file_name) {
        shown if shown.is_empty() => "attachment".to_owned(),
        shown => shown,
    };
    bytes.extend_from_slice(
        format!(
            "{delimiter}\r\nContent-Disposition: form-data; name=\"{}\"; filename=\"{shown}\"\r\nContent-Type: {file_media_type}\r\n\r\n",
            header_quote(file_field)
        )
        .as_bytes(),
    );
    bytes.extend_from_slice(file);
    bytes.extend_from_slice(format!("\r\n{delimiter}--\r\n").as_bytes());
    MultipartBody {
        content_type: format!("multipart/form-data; boundary={boundary}"),
        bytes,
    }
}

/// A header parameter value that cannot end its line or its quotes: control
/// characters (CR and LF included) are dropped, `"` becomes `%22` and `\`
/// becomes `%5C` (what `urllib3` does); everything else, UTF-8 included, is
/// sent as it is.
fn header_quote(value: &str) -> String {
    let mut quoted = String::with_capacity(value.len());
    for character in value.chars().filter(|character| !character.is_control()) {
        match character {
            '"' => quoted.push_str("%22"),
            '\\' => quoted.push_str("%5C"),
            other => quoted.push(other),
        }
    }
    quoted
}

/// A name a tool uploads a file under: not empty, at most 255 bytes, with no
/// control character and no path separator.
pub(crate) fn upload_name_is_valid(name: &str) -> bool {
    !name.is_empty()
        && name.len() <= 255
        && !name
            .chars()
            .any(|character| character.is_control() || matches!(character, '/' | '\\'))
        && name != "."
        && name != ".."
}

/// A shared handle for tests of the families (see
/// `attachment_test_support`).
#[cfg(test)]
impl AttachmentHost {
    pub(crate) fn for_tests(host: SharedOfficeToolHost) -> Self {
        Self::new(host)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn artifact_paths_round_trip_in_both_forms() {
        assert_eq!(
            parse_artifact_path("/bucket/a/b.png").unwrap(),
            ("bucket".to_owned(), "a/b.png".to_owned())
        );
        assert_eq!(
            parse_artifact_path("bucket/b.png").unwrap(),
            ("bucket".to_owned(), "b.png".to_owned())
        );
    }

    #[test]
    fn bad_artifact_paths_are_refused() {
        for path in [
            "", "/", "/bucket", "/bucket/", "/b/../x", "/b/./x", "/b//x", "/b/x?y", "/b/x\ny",
            "/b\\x",
        ] {
            assert_eq!(
                parse_artifact_path(path),
                Err(AttachmentError::Path),
                "{path:?}"
            );
        }
    }

    #[test]
    fn media_types_come_from_bytes_before_names() {
        let png = [0x89, b'P', b'N', b'G', 0x0D, 0x0A, 0x1A, 0x0A, 0];
        assert_eq!(detect_media_type(&png, "x.txt"), "image/png");
        assert_eq!(detect_media_type(b"%PDF-1.7", "x"), "application/pdf");
        assert_eq!(detect_media_type(b"hello", "notes.CSV"), "text/csv");
        assert_eq!(
            detect_media_type(b"hello", "blob"),
            "application/octet-stream"
        );
        assert!(is_describable_image("image/webp"));
        assert!(!is_describable_image("image/svg+xml"));
    }

    #[test]
    fn derived_bucket_names_are_valid_deterministic_and_collision_resistant() {
        assert_eq!(derived_bucket_name("carrier", "mytest1"), "carrier-mytest1");
        let names = [
            derived_bucket_name("carrier", "a_b"),
            derived_bucket_name("carrier", "a b"),
            derived_bucket_name("carrier", "A-B"),
            derived_bucket_name("carrier", "a-b"),
            derived_bucket_name("carrier", "123"),
            derived_bucket_name("carrier", "--"),
            derived_bucket_name("carrier", "日本"),
            derived_bucket_name("carrier", "中文"),
        ];
        for (index, name) in names.iter().enumerate() {
            assert!(name.starts_with("carrier-"), "{name}");
            assert!(name.len() <= 63, "{name}");
            assert!(
                name.bytes()
                    .all(|b| b.is_ascii_lowercase() || b.is_ascii_digit() || b == b'-'),
                "{name}"
            );
            for other in &names[index + 1..] {
                assert_ne!(name, other, "two different texts must not share a bucket");
            }
        }
        // Deterministic, and a long text is cut but still distinct.
        assert_eq!(
            derived_bucket_name("carrier", "a_b"),
            derived_bucket_name("carrier", "a_b")
        );
        let long_a = derived_bucket_name("carrier", &format!("{}x", "a".repeat(200)));
        let long_b = derived_bucket_name("carrier", &format!("{}y", "a".repeat(200)));
        assert!(long_a.len() <= 63 && long_b.len() <= 63);
        assert_ne!(long_a, long_b);
    }

    #[tokio::test]
    async fn a_derived_bucket_is_created_once_in_a_fresh_project_and_an_existing_one_is_success() {
        use crate::toolkits::attachment_test_support::FakePlatform;
        let platform = FakePlatform::new();
        platform.no_buckets();
        let host = platform.host();
        let stored = host
            .write_unique_artifact_creating_bucket("fresh", "report", "xlsx", "x/y", b"1".to_vec())
            .await
            .expect("stored");
        assert_eq!(
            (stored.bucket.as_str(), stored.key.as_str()),
            ("fresh", "report.xlsx")
        );
        assert_eq!(
            *platform.created.lock().expect("created"),
            ["fresh".to_owned()]
        );
        // A second write finds the bucket; a racing creator is not an error.
        host.ensure_bucket("fresh")
            .await
            .expect("already exists is success");
        let second = host
            .write_unique_artifact_creating_bucket("fresh", "report", "xlsx", "x/y", b"2".to_vec())
            .await
            .expect("stored");
        assert_eq!(second.key, "report-2.xlsx");
        assert_eq!(platform.created.lock().expect("created").len(), 1);
    }

    #[tokio::test]
    async fn a_supplied_bucket_that_does_not_exist_is_never_created() {
        use crate::toolkits::families::office_host::OfficeFailure;
        let platform = crate::toolkits::attachment_test_support::FakePlatform::new();
        platform.no_buckets();
        let error = platform
            .host()
            .write_unique_artifact("typo-bucket", "r", "zip", "x/y", b"1".to_vec())
            .await
            .expect_err("not found");
        assert_eq!(
            error,
            AttachmentError::Host(Step::Write, OfficeFailure::NotFound)
        );
        assert!(error.sentence().contains("nothing was stored"));
        assert!(platform.created.lock().expect("created").is_empty());
        assert!(platform.objects.lock().expect("objects").is_empty());
    }

    #[tokio::test]
    async fn a_host_that_cannot_create_buckets_reports_the_write_as_not_done() {
        use crate::toolkits::families::office_host::OfficeFailure;
        let platform = crate::toolkits::attachment_test_support::FakePlatform::new();
        platform.no_buckets();
        platform.fail_create(OfficeFailure::Unsupported);
        let error = platform
            .host()
            .write_unique_artifact_creating_bucket("fresh", "r", "zip", "x/y", b"1".to_vec())
            .await
            .expect_err("no bucket");
        assert_eq!(
            error,
            AttachmentError::Host(Step::Write, OfficeFailure::Unsupported)
        );
        assert!(platform.objects.lock().expect("objects").is_empty());
    }

    #[test]
    fn upload_names_are_sent_as_they_are_with_quotes_and_line_breaks_neutralised() {
        let body = multipart_upload(&[], "file", "отчёт.pdf", "application/pdf", b"x");
        let text = String::from_utf8(body.bytes).expect("utf8");
        assert!(text.contains("filename=\"отчёт.pdf\"\r\n"), "{text}");
        assert!(!text.contains("filename*"), "{text}");
        let body = multipart_upload(&[], "file", "a\"b\\c\r\nX: y.txt", "text/plain", b"x");
        let text = String::from_utf8(body.bytes).expect("utf8");
        assert!(
            text.contains("filename=\"a%22b%5CcX: y.txt\"\r\n"),
            "{text}"
        );
        assert_eq!(text.matches("\r\nX: y.txt").count(), 0);
        // Nothing usable left: a plain stand-in, never an empty name.
        let body = multipart_upload(&[], "file", "\r\n", "text/plain", b"x");
        let text = String::from_utf8(body.bytes).expect("utf8");
        assert!(text.contains("filename=\"attachment\"\r\n"), "{text}");
    }

    #[test]
    fn a_boundary_found_in_a_payload_is_replaced_before_it_is_used() {
        let mut offered = vec!["----clean".to_owned(), "----inside".to_owned()];
        let mut next = || offered.pop().expect("a boundary");
        let body = multipart_with(
            &mut next,
            &[("note", "has ------inside in it")],
            "file",
            "f.bin",
            "application/octet-stream",
            b"bytes ------inside bytes",
        );
        // "----inside" is offered first (popped from the end) and occurs in
        // the payloads, so "----clean" is used.
        assert_eq!(body.content_type, "multipart/form-data; boundary=----clean");
        let text = String::from_utf8_lossy(&body.bytes);
        assert_eq!(text.matches("------clean").count(), 3);
        // Two random boundaries differ.
        assert_ne!(random_boundary(), random_boundary());
    }

    #[test]
    fn a_lost_write_answer_says_it_may_have_applied() {
        let sentence = AttachmentError::Host(Step::Write, OfficeFailure::Unreachable).sentence();
        assert!(sentence.contains("may or may not"), "{sentence}");
        let read = AttachmentError::Host(Step::Read, OfficeFailure::Unreachable).sentence();
        assert!(read.contains("nothing was sent"), "{read}");
    }

    #[test]
    fn the_toolkit_cap_is_its_own_budget_refusal() {
        let error = AttachmentHost::failed(Step::Model, &OfficeHostError::toolkit_budget());
        assert_eq!(error, AttachmentError::ToolkitBudget);
        assert!(
            error.sentence().contains("raise toolkit_model_call_cap"),
            "{}",
            error.sentence()
        );
        let adk = error.into_adk();
        assert_eq!(adk.code, "attachment.toolkit_budget_exhausted");
        assert_eq!(adk.category, ErrorCategory::RateLimited);
        // The gateway's budget keeps its own sentence.
        let gateway = AttachmentHost::failed(
            Step::Model,
            &OfficeHostError::new(OfficeFailure::BudgetRefused, "office_host.budget_refused"),
        );
        assert!(gateway.sentence().contains("model budget is exhausted"));
    }
}

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
    SharedOfficeToolHost,
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
        }
    }

    fn category(&self) -> (ErrorCategory, &'static str) {
        match self {
            Self::Path | Self::Empty => (ErrorCategory::InvalidInput, "attachment.input.invalid"),
            Self::TooLarge(_) => (ErrorCategory::InvalidInput, "attachment.too_large"),
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
        let mut last = AttachmentError::Host(Step::Write, OfficeFailure::AlreadyExists);
        for attempt in 1..=MAX_NAME_ATTEMPTS {
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
                }
                other => return other,
            }
        }
        Err(last)
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
        AttachmentError::Host(step, error.kind())
    }
}

fn clip(text: &str, limit: usize) -> String {
    if text.chars().count() <= limit {
        return text.to_owned();
    }
    text.chars().take(limit).collect()
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
            error @ AttachmentError::Host(
                Step::Model,
                OfficeFailure::BudgetRefused
                | OfficeFailure::NoModel
                | OfficeFailure::RateLimited
                | OfficeFailure::AccessRefused
                | OfficeFailure::Unreachable,
            ),
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

/// Build the body of an upload. The boundary is random per body; `file_name`
/// must already be a plain name (see [`upload_name_is_valid`]).
pub(crate) fn multipart_upload(
    fields: &[(&str, &str)],
    file_field: &str,
    file_name: &str,
    file_media_type: &str,
    file: &[u8],
) -> MultipartBody {
    use ring::rand::SecureRandom as _;
    let mut random = [0_u8; 16];
    if ring::rand::SystemRandom::new().fill(&mut random).is_err() {
        // No random source: the clock still makes the boundary unlikely to
        // occur in the file.
        let nanos = std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .map_or(0, |elapsed| elapsed.as_nanos());
        random = nanos.to_be_bytes();
    }
    let mut boundary = String::from("----elitea");
    for byte in random {
        let _ = write!(boundary, "{byte:02x}");
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
    let ascii = file_name.is_ascii();
    let quoted = header_quote(file_name);
    let extended = if ascii {
        String::new()
    } else {
        format!(
            "; filename*=UTF-8''{}",
            percent_encoding::utf8_percent_encode(file_name, percent_encoding::NON_ALPHANUMERIC)
        )
    };
    let shown = if ascii {
        quoted
    } else {
        header_quote(&file_name.chars().filter(char::is_ascii).collect::<String>())
    };
    bytes.extend_from_slice(
        format!(
            "{delimiter}\r\nContent-Disposition: form-data; name=\"{}\"; filename=\"{shown}\"{extended}\r\nContent-Type: {file_media_type}\r\n\r\n",
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

fn header_quote(value: &str) -> String {
    value
        .chars()
        .filter(|character| !character.is_control())
        .flat_map(|character| match character {
            '"' => vec!['%', '2', '2'],
            '\\' => vec!['/'],
            other => vec![other],
        })
        .collect()
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
    fn a_lost_write_answer_says_it_may_have_applied() {
        let sentence = AttachmentError::Host(Step::Write, OfficeFailure::Unreachable).sentence();
        assert!(sentence.contains("may or may not"), "{sentence}");
        let read = AttachmentError::Host(Step::Read, OfficeFailure::Unreachable).sentence();
        assert!(read.contains("nothing was sent"), "{read}");
    }
}

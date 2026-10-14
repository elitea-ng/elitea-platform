//! `attach_file_to_work_item` and `get_image_by_url`
//! (`tools/ado/work_item/ado_wrapper.py`), over the shared ADO client and the
//! run's [`AttachmentHost`].
//!
//! Every failure that a model must act on is returned as the tool's text, not
//! as an error: the admission layer reduces an error to a generic line, which
//! would lose what did and did not happen. `attach_file_to_work_item` is
//! three requests (upload, link, optional comment) and says exactly which of
//! them took effect.

use elitea_connectors::transport::Method;
use elitea_connectors::transport::Url;
use serde_json::{Map, Value, json};

use crate::toolkits::families::ado::client::{
    AdoClientError, AdoClientErrorCode, AdoRequest, AdoScope,
};
use crate::toolkits::families::ado::work_items::{get_work_item, update_work_item};
use crate::toolkits::families::attachment_host::{
    AttachmentError, AttachmentHost, MAX_ATTACH_BYTES, MAX_PICTURE_BYTES, detect_media_type,
    is_describable_image,
};

use super::client::AdoBoardsClient;

pub(super) const MAX_PATH_BYTES: usize = 2_048;
pub(super) const MAX_NAME_BYTES: usize = 255;
pub(super) const MAX_URL_BYTES: usize = 8_192;
/// `_MAX_IMAGE_PIXELS_FOR_LLM`.
const MAX_IMAGE_PIXELS: u64 = 36_000_000;
/// How many times the inline field is read and written when the work item
/// changes between the read and the write.
const MAX_FIELD_ATTEMPTS: u32 = 3;
const COMMENTS_API: &str = "7.1-preview.4";
const ATTACHMENTS_API: &str = "7.1-preview.3";
/// `image_processing_prompt`, the SDK's default image prompt, verbatim.
const DEFAULT_IMAGE_PROMPT: &str = include_str!("image_processing_prompt.txt");

pub(crate) struct AttachFile<'a> {
    pub(crate) work_item_id: u64,
    pub(crate) filepath: &'a str,
    pub(crate) filename: Option<&'a str>,
    pub(crate) inline_field: Option<&'a str>,
    pub(crate) add_as_comment: bool,
    pub(crate) comment: Option<&'a str>,
}

/// Why a step failed, in words the model can act on. Carries no URL, token
/// or provider body (a refused request may carry the provider's own rule
/// text, which Azure DevOps wrote for the user).
fn cause(error: &AdoClientError) -> String {
    match error.code() {
        AdoClientErrorCode::Authentication => {
            "Azure DevOps rejected the credentials (the PAT); do not retry, tell the user"
                .to_owned()
        }
        AdoClientErrorCode::Authorization => {
            "Azure DevOps did not authorize the request".to_owned()
        }
        AdoClientErrorCode::NotFound => {
            "Azure DevOps did not find the work item (or the attachment)".to_owned()
        }
        AdoClientErrorCode::InvalidInput | AdoClientErrorCode::Conflict => {
            match error.model_visible_message() {
                Some(message) => format!("Azure DevOps refused the request: {message}"),
                None => "Azure DevOps refused the request as invalid".to_owned(),
            }
        }
        AdoClientErrorCode::RateLimited => {
            "Azure DevOps is rate limiting requests; try again shortly".to_owned()
        }
        AdoClientErrorCode::Timeout => "the request to Azure DevOps timed out".to_owned(),
        AdoClientErrorCode::DependencyUnavailable => "Azure DevOps is unavailable".to_owned(),
        AdoClientErrorCode::EgressRefused => {
            "the Azure DevOps host is not on the egress allowlist".to_owned()
        }
        AdoClientErrorCode::UnknownOutcome => {
            "Azure DevOps may have applied the requested effect; reconcile it before retrying"
                .to_owned()
        }
        AdoClientErrorCode::InvalidResponse => {
            "Azure DevOps returned an unusable response".to_owned()
        }
        AdoClientErrorCode::ResourceExhausted => {
            "the file or the answer exceeds the size this tool accepts".to_owned()
        }
        AdoClientErrorCode::InvalidConfiguration => {
            "the Azure DevOps toolkit configuration is invalid".to_owned()
        }
    }
}

/// Whether a failed write may still have been applied.
fn unknown(error: &AdoClientError) -> bool {
    error.code() == AdoClientErrorCode::UnknownOutcome
}

/// A field reference name (`System.Description`): it becomes part of a JSON
/// Patch path, so nothing that could change the path is allowed.
fn field_reference(name: &str) -> bool {
    !name.is_empty()
        && name.len() <= 256
        && name
            .bytes()
            .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'_' | b'.'))
}

/// A name the upload query and the relation can carry.
fn plain_name(name: &str) -> bool {
    !name.is_empty() && name.len() <= MAX_NAME_BYTES && !name.chars().any(char::is_control)
}

/// `_build_inline_markup`, with the characters of a name that could close the
/// markup escaped.
fn inline_markup(url: &str, name: &str, is_image: bool) -> String {
    let safe = name
        .replace('&', "&amp;")
        .replace('<', "&lt;")
        .replace('>', "&gt;")
        .replace('"', "&quot;");
    if is_image {
        format!("<div><img src=\"{url}\" alt=\"{safe}\" /></div>")
    } else {
        format!("<div><a href=\"{url}\">{safe}</a></div>")
    }
}

impl AdoBoardsClient {
    #[allow(clippy::too_many_lines)] // Four requests, each with its own precise report.
    pub(crate) async fn attach_file_to_work_item(
        &self,
        request: AttachFile<'_>,
    ) -> Result<Value, AdoClientError> {
        let Some(host) = &self.attachments else {
            return Err(crate::toolkits::families::ado::client::invalid_input());
        };
        if request
            .inline_field
            .is_some_and(|field| !field_reference(field))
            || request
                .filename
                .is_some_and(|name| !name.is_empty() && !plain_name(name))
        {
            return Err(crate::toolkits::families::ado::client::invalid_input());
        }
        let id = request.work_item_id;
        let file = match host.read_artifact(request.filepath, MAX_ATTACH_BYTES).await {
            Ok(file) => file,
            Err(error) => return Ok(Value::String(error.sentence())),
        };
        // `filename or artifact_filename`.
        let name = request
            .filename
            .filter(|name| !name.is_empty())
            .map_or(file.name, ToOwned::to_owned);
        if !plain_name(&name) {
            return Ok(Value::String(
                "The artifact's file name cannot be used as an Azure DevOps attachment name; nothing was attached. Pass `filename` with a plain name.".to_owned(),
            ));
        }
        let mime_type = file.media_type;
        let is_image = mime_type.starts_with("image/");

        // Step 1: upload. Nothing is linked yet.
        let segments = ["wit", "attachments"];
        let uploaded = match self
            .ado
            .json(
                AdoRequest::new(Method::POST, AdoScope::Project, &segments, ATTACHMENTS_API)
                    .query("fileName", name.clone())
                    .query("uploadType", "Simple")
                    .binary(file.bytes),
                true,
            )
            .await
        {
            Ok(uploaded) => uploaded,
            Err(error) => {
                let tail = if unknown(&error) {
                    format!(
                        "the file may or may not have been uploaded, and it was not linked to work item {id}"
                    )
                } else {
                    format!("nothing was attached to work item {id}")
                };
                return Ok(Value::String(format!(
                    "Could not upload '{name}' to Azure DevOps: {}; {tail}.",
                    cause(&error)
                )));
            }
        };
        let attachment_id = uploaded.get("id").cloned().unwrap_or(Value::Null);
        let Some(attachment_url) = uploaded
            .get("url")
            .and_then(Value::as_str)
            .filter(|url| !url.is_empty())
            .map(ToOwned::to_owned)
        else {
            return Ok(Value::String(format!(
                "Azure DevOps stored the upload but returned no attachment URL, so '{name}' could not be linked to work item {id}; nothing was attached."
            )));
        };
        if !Url::parse(&attachment_url).is_ok_and(|url| self.ado.owns(&url)) {
            return Ok(Value::String(format!(
                "Azure DevOps returned an attachment URL outside the configured organization, so '{name}' was uploaded but NOT linked to work item {id}."
            )));
        }
        let uploaded_text = match &attachment_id {
            Value::String(text) => text.clone(),
            Value::Null => "(no id returned)".to_owned(),
            other => other.to_string(),
        };

        let mut warnings: Vec<String> = Vec::new();
        let mut relation = json!({
            "rel": "AttachedFile",
            "url": attachment_url,
            "attributes": {"name": name}
        });
        if let Some(comment) = request.comment.filter(|comment| !comment.is_empty()) {
            relation["attributes"]["comment"] = Value::String(comment.to_owned());
        }

        // Steps 2 and 3. With an inline field the new value is the field's
        // current value plus the markup, so the update is guarded by the
        // revision of the SAME read (`test /rev`): a work item edited in
        // between answers 409/412, and the field is read again (a bounded
        // number of times). A field that cannot be read is left alone:
        // writing "the new markup only" would replace what it holds.
        let mut inline_applied: Option<&str>;
        let mut attempt = 0;
        loop {
            attempt += 1;
            let mut document =
                vec![json!({"op": "add", "path": "/relations/-", "value": relation})];
            inline_applied = None;
            let mut field_warning = None;
            if let Some(field) = request.inline_field {
                match get_work_item(&self.ado, id, Some(&[field.to_owned()]), None, None).await {
                    Ok(item) => match item.get("rev").and_then(Value::as_u64) {
                        Some(rev) => {
                            let current = item
                                .get("fields")
                                .and_then(|fields| fields.get(field))
                                .and_then(Value::as_str)
                                .unwrap_or_default();
                            let value = format!(
                                "{current}{}",
                                inline_markup(&attachment_url, &name, is_image)
                            );
                            document.insert(0, json!({"op": "test", "path": "/rev", "value": rev}));
                            document.push(json!({"op": "add", "path": format!("/fields/{field}"), "value": value}));
                            inline_applied = Some(field);
                        }
                        None => {
                            field_warning = Some(format!(
                                "the field '{field}' was not updated because Azure DevOps returned no revision to guard the update with"
                            ));
                        }
                    },
                    Err(error) => {
                        field_warning = Some(format!(
                            "the field '{field}' was not updated because its current value could not be read ({})",
                            cause(&error)
                        ));
                    }
                }
            }
            match update_work_item(&self.ado, &id.to_string(), document).await {
                Ok(_) => {
                    warnings.extend(field_warning);
                    break;
                }
                Err(error)
                    if inline_applied.is_some()
                        && error.code() == AdoClientErrorCode::Conflict
                        && attempt < MAX_FIELD_ATTEMPTS => {}
                Err(error) => {
                    let conflict = if inline_applied.is_some()
                        && error.code() == AdoClientErrorCode::Conflict
                    {
                        format!(
                            "the work item changed on every one of {attempt} attempts (its revision no longer matched)"
                        )
                    } else {
                        cause(&error)
                    };
                    let state = if unknown(&error) {
                        format!(
                            "'{name}' was uploaded as attachment {uploaded_text} and may or may not be linked to work item {id}; check the work item's attachments before retrying"
                        )
                    } else {
                        format!(
                            "'{name}' was uploaded as attachment {uploaded_text} but NOT linked to work item {id}, which is unchanged; retrying uploads the file again"
                        )
                    };
                    return Ok(Value::String(format!("{state}: {conflict}.")));
                }
            }
        }

        // Step 4 (optional): the comment. The file is attached either way.
        let mut comment_added = None;
        if request.add_as_comment {
            let work_item = id.to_string();
            let comment_segments = ["wit", "workItems", work_item.as_str(), "comments"];
            let markup = inline_markup(&attachment_url, &name, is_image);
            match self
                .ado
                .json(
                    AdoRequest::new(
                        Method::POST,
                        AdoScope::Project,
                        &comment_segments,
                        COMMENTS_API,
                    )
                    .json(json!({"text": markup})),
                    true,
                )
                .await
            {
                Ok(_) => comment_added = Some(true),
                Err(error) => {
                    comment_added = Some(false);
                    warnings.push(if unknown(&error) {
                        "the comment may or may not have been added (Azure DevOps may have applied the requested effect); check the work item's comments before retrying".to_owned()
                    } else {
                        format!("the comment was not added ({})", cause(&error))
                    });
                }
            }
        }

        let mut message = format!("File '{name}' attached to work item {id}.");
        for warning in &warnings {
            message.push_str(" Warning: ");
            message.push_str(warning);
            message.push('.');
        }
        let mut result = Map::new();
        result.insert("work_item_id".to_owned(), json!(id));
        result.insert("attachment_id".to_owned(), attachment_id);
        result.insert("attachment_url".to_owned(), Value::String(attachment_url));
        result.insert("filename".to_owned(), Value::String(name));
        result.insert("mime_type".to_owned(), Value::String(mime_type.to_owned()));
        result.insert(
            "inline_field".to_owned(),
            inline_applied.map_or(Value::Null, |field| Value::String(field.to_owned())),
        );
        if let Some(added) = comment_added {
            result.insert("comment_added".to_owned(), Value::Bool(added));
        }
        if !warnings.is_empty() {
            result.insert("warnings".to_owned(), json!(warnings));
        }
        result.insert("message".to_owned(), Value::String(message));
        Ok(Value::Object(result))
    }

    /// `get_image_by_url`: the attachment id and file name come from the
    /// URL; the bytes are read through the configured connection, never from
    /// the URL's own host.
    pub(crate) async fn get_image_by_url(
        &self,
        attachment_url: &str,
        file_name: Option<&str>,
        prompt: Option<&str>,
    ) -> Result<Value, AdoClientError> {
        let Some(host) = &self.attachments else {
            return Err(crate::toolkits::families::ado::client::invalid_input());
        };
        Ok(Value::String(
            match self
                .describe_image(host, attachment_url, file_name, prompt)
                .await
            {
                Ok(description) => description,
                Err(message) => format!("Error analyzing attachment image: {message}"),
            },
        ))
    }

    #[allow(clippy::too_many_lines)] // The SDK's validation ladder, one message per rung.
    async fn describe_image(
        &self,
        host: &AttachmentHost,
        attachment_url: &str,
        file_name: Option<&str>,
        prompt: Option<&str>,
    ) -> Result<String, String> {
        if attachment_url.to_ascii_lowercase().starts_with("data:") {
            return Err(
                "data: URIs are not supported; provide an Azure DevOps attachment URL.".to_owned(),
            );
        }
        let (own, path, segments, derived_name) = self.parse_attachment_url(attachment_url);
        if !path
            .to_ascii_lowercase()
            .contains("/_apis/wit/attachments/")
        {
            return Err(
                "URL must be an Azure DevOps work item attachment URL (containing /_apis/wit/attachments/)."
                    .to_owned(),
            );
        }
        if !own {
            return Err(
                "The attachment URL is not on the configured Azure DevOps organization; nothing was fetched."
                    .to_owned(),
            );
        }
        let Some(attachment_id) = segments
            .iter()
            .position(|segment| segment == "attachments")
            .and_then(|index| segments.get(index + 1))
            .filter(|id| {
                !id.is_empty()
                    && id
                        .bytes()
                        .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'_' | b'-'))
            })
        else {
            return Err("Could not determine the attachment id from the URL.".to_owned());
        };
        let name = file_name
            .filter(|name| !name.is_empty())
            .map(ToOwned::to_owned)
            .or(derived_name)
            .ok_or_else(|| "Provide file_name; the URL has no fileName parameter.".to_owned())?;

        let suffix = name
            .rsplit_once('.')
            .map_or(String::new(), |(_, suffix)| format!(".{suffix}"));
        match suffix.to_ascii_lowercase().as_str() {
            ".png" | ".jpg" | ".jpeg" | ".gif" | ".webp" => {}
            ".bmp" | ".svg" => {
                return Err(format!(
                    "{} images cannot be described in this runtime (it takes png, jpg, jpeg, gif and webp).",
                    suffix.to_ascii_lowercase()
                ));
            }
            _ => {
                return Err(format!(
                    "Unsupported image format '{suffix}'; supported: png, jpg, jpeg, gif, webp, bmp, svg"
                ));
            }
        }

        let segments = ["wit", "attachments", attachment_id.as_str()];
        let bytes = self
            .ado
            .bytes(
                AdoRequest::new(
                    Method::GET,
                    AdoScope::Organization,
                    &segments,
                    ATTACHMENTS_API,
                )
                .query("download", "true")
                .octet_stream(),
                false,
            )
            .await
            .map_err(|error| {
                if error.code() == AdoClientErrorCode::ResourceExhausted {
                    "Image exceeds the 5 MB processing limit".to_owned()
                } else {
                    format!("Could not download the attachment: {}", cause(&error))
                }
            })?;
        if bytes.len() as u64 > MAX_PICTURE_BYTES {
            return Err("Image exceeds the 5 MB processing limit".to_owned());
        }
        let media_type = detect_media_type(&bytes, &name);
        let Some((width, height)) =
            image_dimensions(&bytes).filter(|_| is_describable_image(media_type))
        else {
            return Err("Could not read image dimensions".to_owned());
        };
        if width == 0 || height == 0 {
            return Err("Could not read image dimensions".to_owned());
        }
        if width.saturating_mul(height) > MAX_IMAGE_PIXELS {
            return Err(format!(
                "Image dimensions {width}x{height} exceed the processing limit"
            ));
        }
        let prompt = prompt
            .filter(|prompt| !prompt.is_empty())
            .map_or_else(|| DEFAULT_IMAGE_PROMPT.to_owned(), ToOwned::to_owned);
        let description = host
            .describe_picture(bytes, media_type, prompt)
            .await
            .map_err(|error: AttachmentError| error.sentence())?;
        // `EliteAImageLoader.get_content`.
        if description.trim().is_empty() {
            Ok("[No readable text detected by LLM]".to_owned())
        } else {
            Ok(description)
        }
    }

    /// `urlparse(attachment_url)`: whether the URL is inside the configured
    /// organization (a relative URL is), its path segments and the
    /// `fileName` parameter.
    fn parse_attachment_url(
        &self,
        attachment_url: &str,
    ) -> (bool, String, Vec<String>, Option<String>) {
        let (url, own) = match Url::parse(attachment_url) {
            Ok(url) => {
                let own = self.ado.owns(&url)
                    || same_organization_other_form(self.ado.organization_text(), &url);
                (url, own)
            }
            Err(_) => match Url::parse("https://relative.invalid/")
                .and_then(|base| base.join(attachment_url))
            {
                Ok(url) => (url, true),
                Err(_) => return (false, String::new(), Vec::new(), None),
            },
        };
        let segments = url
            .path_segments()
            .map(|segments| {
                segments
                    .filter(|segment| !segment.is_empty())
                    .map(ToOwned::to_owned)
                    .collect()
            })
            .unwrap_or_default();
        let name = url
            .query_pairs()
            .find(|(key, _)| key == "fileName")
            .map(|(_, value)| value.into_owned())
            .filter(|name| !name.is_empty());
        (own, url.path().to_owned(), segments, name)
    }
}

/// `https://{org}.visualstudio.com/...` for a configured
/// `https://dev.azure.com/{org}` and the reverse: one organization, two
/// address forms.
fn same_organization_other_form(configured: &str, url: &Url) -> bool {
    let Ok(configured) = Url::parse(configured) else {
        return false;
    };
    if url.scheme() != "https" || configured.scheme() != "https" {
        return false;
    }
    let host = |url: &Url| url.host_str().map(str::to_ascii_lowercase);
    let (Some(configured_host), Some(url_host)) = (host(&configured), host(url)) else {
        return false;
    };
    let first_segment = |url: &Url| {
        url.path_segments()
            .and_then(|mut segments| segments.next().map(str::to_ascii_lowercase))
            .filter(|segment| !segment.is_empty())
    };
    let visualstudio_org = |host: &str| {
        host.strip_suffix(".visualstudio.com")
            .map(ToOwned::to_owned)
    };
    match (
        configured_host.as_str(),
        url_host.as_str(),
        visualstudio_org(&configured_host),
        visualstudio_org(&url_host),
    ) {
        ("dev.azure.com", _, _, Some(org)) => first_segment(&configured) == Some(org),
        (_, "dev.azure.com", Some(org), _) => first_segment(url) == Some(org),
        _ => false,
    }
}

/// The pixel size of a PNG, GIF, JPEG or WebP, read from its header.
fn image_dimensions(bytes: &[u8]) -> Option<(u64, u64)> {
    let be32 = |at: usize| {
        bytes
            .get(at..at + 4)
            .map(|b| u64::from(u32::from_be_bytes([b[0], b[1], b[2], b[3]])))
    };
    let be16 = |at: usize| {
        bytes
            .get(at..at + 2)
            .map(|b| u64::from(u16::from_be_bytes([b[0], b[1]])))
    };
    let le16 = |at: usize| {
        bytes
            .get(at..at + 2)
            .map(|b| u64::from(u16::from_le_bytes([b[0], b[1]])))
    };
    let le24 = |at: usize| {
        bytes
            .get(at..at + 3)
            .map(|b| u64::from(b[0]) | u64::from(b[1]) << 8 | u64::from(b[2]) << 16)
    };
    if bytes.starts_with(&[0x89, b'P', b'N', b'G', 0x0D, 0x0A, 0x1A, 0x0A]) {
        return Some((be32(16)?, be32(20)?));
    }
    if bytes.starts_with(b"GIF87a") || bytes.starts_with(b"GIF89a") {
        return Some((le16(6)?, le16(8)?));
    }
    if bytes.len() >= 30 && &bytes[..4] == b"RIFF" && &bytes[8..12] == b"WEBP" {
        return match &bytes[12..16] {
            b"VP8X" => Some((le24(24)? + 1, le24(27)? + 1)),
            b"VP8L" => {
                let bits = u32::from_le_bytes([
                    *bytes.get(21)?,
                    *bytes.get(22)?,
                    *bytes.get(23)?,
                    *bytes.get(24)?,
                ]);
                Some((
                    u64::from(bits & 0x3FFF) + 1,
                    u64::from((bits >> 14) & 0x3FFF) + 1,
                ))
            }
            b"VP8 " => Some((le16(26)? & 0x3FFF, le16(28)? & 0x3FFF)),
            _ => None,
        };
    }
    if bytes.starts_with(&[0xFF, 0xD8]) {
        let mut at = 2;
        while at + 4 <= bytes.len() {
            if bytes[at] != 0xFF {
                return None;
            }
            let marker = bytes[at + 1];
            if marker == 0xFF {
                at += 1;
                continue;
            }
            if (0xD0..=0xD9).contains(&marker) || marker == 0x01 {
                at += 2;
                continue;
            }
            let length = usize::try_from(be16(at + 2)?).ok()?;
            if matches!(marker, 0xC0..=0xC3 | 0xC5..=0xC7 | 0xC9..=0xCB | 0xCD..=0xCF) {
                return Some((be16(at + 7)?, be16(at + 5)?));
            }
            at += 2 + length;
        }
    }
    None
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn dimensions_come_from_the_headers_of_the_four_picture_types() {
        let png = crate::toolkits::attachment_test_support::PNG;
        assert_eq!(image_dimensions(png), Some((1, 1)));
        let mut gif = b"GIF89a".to_vec();
        gif.extend([3, 0, 2, 0]);
        assert_eq!(image_dimensions(&gif), Some((3, 2)));
        // SOI, APP0 (length 4), SOF0 with 7x5.
        let jpeg = [
            0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x04, 0x00, 0x00, 0xFF, 0xC0, 0x00, 0x0B, 0x08, 0x00,
            0x05, 0x00, 0x07, 0x01,
        ];
        assert_eq!(image_dimensions(&jpeg), Some((7, 5)));
        let mut webp = b"RIFF\0\0\0\0WEBPVP8X\x0a\0\0\0\0\0\0\0".to_vec();
        webp.extend([9, 0, 0, 3, 0, 0]);
        assert_eq!(image_dimensions(&webp), Some((10, 4)));
        assert_eq!(image_dimensions(b"not a picture"), None);
    }

    #[test]
    fn both_address_forms_of_an_organization_match() {
        let url = |text: &str| Url::parse(text).expect("url");
        assert!(same_organization_other_form(
            "https://dev.azure.com/contoso",
            &url("https://contoso.visualstudio.com/_apis/wit/attachments/x")
        ));
        assert!(same_organization_other_form(
            "https://contoso.visualstudio.com",
            &url("https://dev.azure.com/Contoso/_apis/wit/attachments/x")
        ));
        assert!(!same_organization_other_form(
            "https://dev.azure.com/contoso",
            &url("https://evil.visualstudio.com/_apis/wit/attachments/x")
        ));
        assert!(!same_organization_other_form(
            "https://dev.azure.com/contoso",
            &url("https://dev.azure.com/other/_apis/wit/attachments/x")
        ));
    }
}

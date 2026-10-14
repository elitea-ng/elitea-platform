//! Files and list items over Graph: a document's bytes, an upload, a new
//! list item.
//!
//! Reads fetch bytes only under a cap that is checked before the bytes are
//! buffered. Changes are sent once on a transport that never retries; a
//! change whose answer was lost, or whose answer cannot be read, is reported
//! as an unknown outcome and never as a failure that is safe to repeat.

use base64::Engine as _;
use base64::engine::general_purpose::URL_SAFE_NO_PAD;

use super::{
    Duration, GRAPH_BASE, GraphCall, HeaderName, HeaderValue, Map, Method, SharePointClient,
    SharePointClientError, SharePointClientErrorCode, SharePointHttpResponse, Url, Value,
    dependency_unavailable, drive_library_path, file_refused, graph_url, invalid_configuration,
    invalid_input, invalid_response, json, normalize_folder_path, not_found, outcome_unknown,
    require_content_type, require_effect_success, require_success, required_json_text,
    resource_exhausted, set_query, strip_path_prefix, strip_site_prefix, validate_output,
    validate_segment, validate_transfer_url,
};

/// The most bytes of a file the readers fetch (the SDK's sharing-link cap).
pub(in crate::toolkits) const MAX_READ_BYTES: usize = 20 * 1_024 * 1_024;
/// The most bytes of an image the sharing-link reader fetches.
const MAX_IMAGE_READ_BYTES: usize = 3 * 1_024 * 1_024;
/// The most bytes one upload sends.
pub(in crate::toolkits) const MAX_UPLOAD_BYTES: usize = 50 * 1_024 * 1_024;
/// Files up to this size are one `PUT` (the SDK's threshold, inclusive).
const SMALL_UPLOAD_BYTES: usize = 4 * 1_024 * 1_024;
/// One resumable-upload chunk: a multiple of 320 KiB, as Graph requires.
const UPLOAD_CHUNK_BYTES: usize = 5 * 1_024 * 1_024;
const TRANSFER_TIMEOUT: Duration = Duration::from_mins(2);
const MAX_PATH_BYTES: usize = 8 * 1_024;
const MAX_SHARING_URL_BYTES: usize = 8 * 1_024;
const MAX_FILE_NAME_BYTES: usize = 255;

const NOT_A_FILE: &str = "the path does not point to a file: it names a document library or a folder; include the file name";
const TOO_LARGE: &str = "the file is larger than the 20 MB this tool reads (3 MB for images)";
const NO_EXTENSION: &str = "the file has no extension, so its type cannot be determined";
const DOUBLE_EXTENSION: &str = "the file name has a suspicious double extension (an archive, media or executable type before the last one)";
const UNSUPPORTED_TYPE: &str = "the file type is not supported: documents (PDF, DOCX, XLSX, PPTX), text files (TXT, MD, CSV, JSON, HTML, XML), images and common code files are; archives (ZIP, RAR), media (MP4, MP3) and executables are not";
const DANGEROUS_INTERMEDIATE: [&str; 18] = [
    "zip", "rar", "7z", "tar", "gz", "exe", "msi", "bat", "cmd", "sh", "mp4", "avi", "mov", "mp3",
    "wav", "dll", "so", "bin",
];

/// A file read from `SharePoint`.
pub(in crate::toolkits) struct SharePointFile {
    pub(in crate::toolkits) name: String,
    pub(in crate::toolkits) bytes: Vec<u8>,
}

/// One upload.
pub(in crate::toolkits) struct SharePointUpload {
    /// A server-relative folder path including the library, as the SDK's
    /// `folder_path`.
    pub(in crate::toolkits) folder_path: String,
    pub(in crate::toolkits) file_name: String,
    pub(in crate::toolkits) bytes: Vec<u8>,
    /// True replaces a file of that name; false refuses (a conflict).
    pub(in crate::toolkits) replace: bool,
}

/// The media type an upload is sent as, by extension (the SDK sniffs the
/// bytes first; Graph keeps its own record of the type either way).
pub(in crate::toolkits) fn detect_media_type(bytes: &[u8], file_name: &str) -> &'static str {
    if bytes.starts_with(b"\x89PNG\r\n\x1a\n") {
        return "image/png";
    }
    if bytes.starts_with(&[0xff, 0xd8, 0xff]) {
        return "image/jpeg";
    }
    if bytes.starts_with(b"GIF87a") || bytes.starts_with(b"GIF89a") {
        return "image/gif";
    }
    if bytes.starts_with(b"%PDF-") {
        return "application/pdf";
    }
    let extension = file_name
        .rsplit_once('.')
        .map(|(_, extension)| extension.to_ascii_lowercase())
        .unwrap_or_default();
    match extension.as_str() {
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
        "txt" | "log" => "text/plain",
        "csv" => "text/csv",
        "html" | "htm" => "text/html",
        "json" => "application/json",
        "xml" => "application/xml",
        "zip" => "application/zip",
        _ => "application/octet-stream",
    }
}

impl SharePointClient {
    pub(super) async fn create_list_item_impl(
        &self,
        list_title: &str,
        fields: &Map<String, Value>,
    ) -> Result<Value, SharePointClientError> {
        if fields.is_empty() {
            return Err(invalid_input());
        }
        let site_id = self.resolve_site_id().await?;
        let list_id = self.resolve_list_id(list_title).await?;
        let url = graph_url(&["sites", site_id, "lists", &list_id, "items"])?;
        let body = serde_json::to_vec(&json!({ "fields": fields })).map_err(|_| invalid_input())?;
        let response = self
            .send(
                GraphCall::new(Method::POST, url, "application/json")
                    .body("application/json", body),
            )
            .await
            .map_err(SharePointClientError::for_effect)?;
        require_effect_success(response.status)?;
        // The item exists once Graph answered 2xx. An answer that cannot be
        // read is not a failure to repeat: repeating creates a second item.
        let created = json_object(&response).map_err(|_| outcome_unknown())?;
        let result = json!({
            "id": created.get("id").cloned().unwrap_or_else(|| Value::String(String::new())),
            "fields": created.get("fields").cloned().unwrap_or_else(|| json!({})),
            "webUrl": created.get("webUrl").cloned().unwrap_or_else(|| Value::String(String::new())),
        });
        validate_output(&result)?;
        Ok(result)
    }

    pub(super) async fn upload_file_impl(
        &self,
        upload: SharePointUpload,
    ) -> Result<Value, SharePointClientError> {
        let SharePointUpload {
            folder_path,
            file_name,
            bytes,
            replace,
        } = upload;
        validate_file_name(&file_name)?;
        if bytes.len() > MAX_UPLOAD_BYTES {
            return Err(resource_exhausted());
        }
        let (drive_id, folder) = self
            .resolve_drive_and_folder(&folder_path)
            .await?
            .into_iter()
            .next()
            .ok_or_else(not_found)?;
        let item_path = if folder.is_empty() {
            file_name.clone()
        } else {
            format!("{folder}/{file_name}")
        };
        let conflict = if replace { "replace" } else { "fail" };
        let media_type = detect_media_type(&bytes, &file_name);
        let size = bytes.len();
        let item = if size <= SMALL_UPLOAD_BYTES {
            self.upload_small(&drive_id, &item_path, conflict, media_type, bytes)
                .await?
        } else {
            self.upload_in_chunks(&drive_id, &item_path, conflict, bytes)
                .await?
        };
        let parent = item
            .get("parentReference")
            .and_then(Value::as_object)
            .and_then(|parent| parent.get("path"))
            .and_then(Value::as_str)
            .unwrap_or_default();
        let name = item
            .get("name")
            .and_then(Value::as_str)
            .unwrap_or(&file_name);
        let result = json!({
            "id": item.get("id").and_then(Value::as_str).unwrap_or_default(),
            "webUrl": item.get("webUrl").and_then(Value::as_str).unwrap_or_default(),
            "path": format!("{parent}/{name}"),
            "size": item.get("size").and_then(Value::as_u64).unwrap_or(size as u64),
            "mime_type": item
                .get("file")
                .and_then(Value::as_object)
                .and_then(|file| file.get("mimeType"))
                .and_then(Value::as_str)
                .unwrap_or(media_type),
        });
        validate_output(&result)?;
        Ok(result)
    }

    async fn upload_small(
        &self,
        drive_id: &str,
        item_path: &str,
        conflict: &str,
        media_type: &'static str,
        bytes: Vec<u8>,
    ) -> Result<Map<String, Value>, SharePointClientError> {
        let mut url = drive_item_url(drive_id, item_path, Some("content"))?;
        url.set_query(Some(&format!(
            "@microsoft.graph.conflictBehavior={conflict}"
        )));
        let response = self
            .send(
                GraphCall::new(Method::PUT, url, "application/json")
                    .body(media_type, bytes)
                    .timeout(TRANSFER_TIMEOUT),
            )
            .await
            .map_err(SharePointClientError::for_effect)?;
        require_effect_success(response.status)?;
        json_object(&response).map_err(|_| outcome_unknown())
    }

    async fn upload_in_chunks(
        &self,
        drive_id: &str,
        item_path: &str,
        conflict: &str,
        bytes: Vec<u8>,
    ) -> Result<Map<String, Value>, SharePointClientError> {
        // Opening a session changes nothing a person can see: a failure here
        // is a plain failure.
        let url = drive_item_url(drive_id, item_path, Some("createUploadSession"))?;
        let body = serde_json::to_vec(&json!({
            "item": {"@microsoft.graph.conflictBehavior": conflict}
        }))
        .map_err(|_| invalid_input())?;
        let response = self
            .send(
                GraphCall::new(Method::POST, url, "application/json")
                    .body("application/json", body),
            )
            .await?;
        require_effect_success(response.status).map_err(demote_unknown)?;
        let session = json_object(&response)?;
        let upload_url = session
            .get("uploadUrl")
            .and_then(Value::as_str)
            .filter(|value| value.len() <= 16 * 1_024 && !value.contains('\\'))
            .and_then(|value| Url::parse(value).ok())
            .ok_or_else(invalid_response)?;
        validate_transfer_url(&upload_url)?;

        let total = bytes.len();
        let mut offset = 0_usize;
        while offset < total {
            let end = offset.saturating_add(UPLOAD_CHUNK_BYTES).min(total);
            let last = end == total;
            let range = HeaderValue::from_str(&format!("bytes {offset}-{}/{total}", end - 1))
                .map_err(|_| invalid_input())?;
            let mut call = GraphCall::new(Method::PUT, upload_url.clone(), "application/json")
                .body("application/octet-stream", bytes[offset..end].to_vec())
                .timeout(TRANSFER_TIMEOUT);
            call.headers
                .push((HeaderName::from_static("content-range"), range));
            let sent = self.send_transfer(call).await;
            let outcome = sent.and_then(|response| {
                require_effect_success(response.status)?;
                if last {
                    if matches!(response.status.as_u16(), 200 | 201) {
                        json_object(&response).map_err(|_| outcome_unknown())
                    } else {
                        // The last bytes went out and the item was not
                        // confirmed.
                        Err(outcome_unknown())
                    }
                } else if response.status.as_u16() == 202 {
                    Ok(Map::new())
                } else {
                    Err(invalid_response())
                }
            });
            match outcome {
                Ok(item) if last => return Ok(item),
                Ok(_) => offset = end,
                Err(error) => {
                    // Nothing is committed before the last chunk lands, so an
                    // earlier failure is a plain one (and the session is
                    // cancelled); only the last chunk's lost answer leaves the
                    // item possibly created.
                    if last {
                        return Err(error.for_effect());
                    }
                    self.cancel_upload_session(&upload_url).await;
                    return Err(demote_unknown(error));
                }
            }
        }
        Err(invalid_input())
    }

    /// Ends an abandoned upload session so it holds no space. Best effort.
    async fn cancel_upload_session(&self, upload_url: &Url) {
        let call = GraphCall::new(Method::DELETE, upload_url.clone(), "application/json")
            .timeout(Duration::from_secs(10));
        drop(self.send_transfer(call).await);
    }

    pub(super) async fn download_document_impl(
        &self,
        path: &str,
        max_bytes: usize,
    ) -> Result<SharePointFile, SharePointClientError> {
        if path.is_empty() || path.len() > MAX_PATH_BYTES || path.chars().any(char::is_control) {
            return Err(invalid_input());
        }
        let (drive_id, relative) = match graph_drive_path(path)? {
            Some((drive_id, relative)) => {
                self.require_site_drive(&drive_id).await?;
                (drive_id, relative)
            }
            None => self.locate_by_server_path(path).await?,
        };
        if relative.is_empty() {
            return Err(file_refused(NOT_A_FILE));
        }
        let name = relative.rsplit('/').next().unwrap_or_default().to_owned();
        let url = drive_item_url(&drive_id, &relative, Some("content"))?;
        let bytes = self.fetch_content(url, max_bytes).await?;
        Ok(SharePointFile { name, bytes })
    }

    /// A Graph drive path names its drive, and the toolkit reads the libraries
    /// of its own site only: a drive the site does not list is not found,
    /// whatever id the caller was handed.
    async fn require_site_drive(&self, drive_id: &str) -> Result<(), SharePointClientError> {
        let listed = self.list_drives().await?.iter().any(|drive| {
            drive
                .as_object()
                .and_then(|drive| drive.get("id"))
                .and_then(Value::as_str)
                == Some(drive_id)
        });
        listed.then_some(()).ok_or_else(not_found)
    }

    /// The drive and drive-relative path of a server-relative file path. A bare
    /// path found in several libraries is read from the default one.
    async fn locate_by_server_path(
        &self,
        path: &str,
    ) -> Result<(String, String), SharePointClientError> {
        let mut matches = self.resolve_drive_and_folder(path).await?;
        if matches.len() > 1 {
            let default = self.default_drive_id().await?;
            let chosen = matches
                .iter()
                .position(|(drive, _)| *drive == default)
                .unwrap_or(0);
            return Ok(matches.swap_remove(chosen));
        }
        matches.pop().ok_or_else(not_found)
    }

    /// The bytes behind a Graph content address: Graph answers with one
    /// redirect to a pre-authenticated `SharePoint` URL (fetched without the
    /// bearer), or with the bytes themselves. The cap applies to whichever
    /// answers, before the bytes are buffered.
    pub(super) async fn fetch_content(
        &self,
        url: Url,
        max_bytes: usize,
    ) -> Result<Vec<u8>, SharePointClientError> {
        let first = self
            .send(
                GraphCall::get(url, "application/octet-stream")
                    .max_body(max_bytes)
                    .timeout(TRANSFER_TIMEOUT),
            )
            .await?;
        let response = if first.status.is_redirection() {
            let target = redirect_target(&first)?;
            drop(first);
            self.send_transfer(
                GraphCall::get(target, "application/octet-stream")
                    .max_body(max_bytes)
                    .timeout(TRANSFER_TIMEOUT),
            )
            .await?
        } else {
            first
        };
        require_success(response.status)?;
        Ok(response.body)
    }

    pub(super) async fn download_shared_file_impl(
        &self,
        sharing_url: &str,
    ) -> Result<SharePointFile, SharePointClientError> {
        let share_url = sharing_url.trim();
        if share_url.len() > MAX_SHARING_URL_BYTES
            || share_url.chars().any(char::is_control)
            || !share_url.starts_with("https://")
            || Url::parse(share_url).is_err()
        {
            return Err(invalid_input());
        }
        // The link is only ever encoded into a Graph address; it is never
        // fetched, so it cannot name a host this process reaches.
        let share_id = format!("u!{}", URL_SAFE_NO_PAD.encode(share_url.as_bytes()));
        let item_url = graph_url(&["shares", &share_id, "driveItem"])?;
        let mut call = GraphCall::get(item_url, "application/json");
        call.prefer = Some("redeemSharingLinkIfNecessary");
        let response = self.send(call).await?;
        require_success(response.status)?;
        require_content_type(response.content_type.as_deref(), "application/json")?;
        let item = json_object(&response)?;

        if item.contains_key("folder") || !item.contains_key("file") {
            return Err(file_refused(NOT_A_FILE));
        }
        let name = item
            .get("name")
            .and_then(Value::as_str)
            .filter(|name| !name.is_empty() && name.len() <= 4 * 1_024)
            .ok_or_else(invalid_response)?
            .to_owned();
        let size = item.get("size").and_then(Value::as_u64);
        let limit = validate_shared_file(&name, size)?;

        let bytes = if let Some(download) = item
            .get("@microsoft.graph.downloadUrl")
            .and_then(Value::as_str)
            .filter(|value| value.len() <= 16 * 1_024 && !value.contains('\\'))
        {
            let download = Url::parse(download).map_err(|_| invalid_response())?;
            let response = self
                .send_transfer(
                    GraphCall::get(download, "application/octet-stream")
                        .max_body(limit)
                        .timeout(TRANSFER_TIMEOUT),
                )
                .await?;
            require_success(response.status)?;
            response.body
        } else {
            let content = graph_url(&["shares", &share_id, "driveItem", "content"])?;
            self.fetch_content(content, limit).await?
        };
        if bytes.is_empty() {
            return Err(file_refused("the file is empty"));
        }
        Ok(SharePointFile { name, bytes })
    }

    /// The drive the site's default library lives on.
    async fn default_drive_id(&self) -> Result<String, SharePointClientError> {
        let site_id = self.resolve_site_id().await?;
        let object = self
            .get_json(graph_url(&["sites", site_id, "drive"])?)
            .await?;
        required_json_text(&object, "id", 4 * 1_024).map(str::to_owned)
    }

    /// A server-relative folder (or file) path as `(drive id, path relative
    /// to the drive)` pairs, as the SDK's `_resolve_drive_and_folder`: the
    /// library named by the path's first segment; else every library that
    /// holds that relative path; else the default library with the path as
    /// written.
    ///
    /// Unlike the SDK, a probe that fails for any reason but "not there" is
    /// an error: skipping a library that could not be asked would send a
    /// write to the wrong one.
    pub(super) async fn resolve_drive_and_folder(
        &self,
        folder_path: &str,
    ) -> Result<Vec<(String, String)>, SharePointClientError> {
        let cleaned = normalize_folder_path(folder_path)?;
        let cleaned = strip_site_prefix(&cleaned, &self.config.site_path).to_owned();
        let drives = self.list_drives().await?;
        let mut matched = Vec::new();
        for drive in drives {
            let drive = drive.as_object().ok_or_else(invalid_response)?;
            let id = required_json_text(drive, "id", 4 * 1_024)?;
            let Some(library) =
                drive_library_path(drive, &self.config.site_hostname, &self.config.site_path)?
            else {
                continue;
            };
            if let Some(remainder) = strip_path_prefix(&cleaned, &library) {
                matched.push((id.to_owned(), remainder.to_owned()));
            }
        }
        if !matched.is_empty() {
            return Ok(matched);
        }
        if !cleaned.is_empty() {
            let mut probed = Vec::new();
            for drive in drives {
                let id = drive
                    .as_object()
                    .ok_or_else(invalid_response)
                    .and_then(|drive| required_json_text(drive, "id", 4 * 1_024))?;
                let mut url = drive_item_url(id, &cleaned, None)?;
                set_query(&mut url, &[("$select", "id,folder")]);
                match self.get_json(url).await {
                    Ok(_) => probed.push((id.to_owned(), cleaned.clone())),
                    Err(error) if error.code() == SharePointClientErrorCode::NotFound => {}
                    Err(error) => return Err(error),
                }
            }
            if !probed.is_empty() {
                return Ok(probed);
            }
        }
        Ok(vec![(self.default_drive_id().await?, cleaned)])
    }
}

/// `drives/{id}/root:/{path}` as `get_files_list` returns a file's `Path`; the
/// path is literal (it was never encoded), so a name with `%20` in it stays
/// as written. `None` for any other form.
fn graph_drive_path(path: &str) -> Result<Option<(String, String)>, SharePointClientError> {
    let trimmed = path.strip_prefix('/').unwrap_or(path);
    let Some(rest) = trimmed.strip_prefix("drives/") else {
        return Ok(None);
    };
    let Some((drive, after)) = rest.split_once('/') else {
        return Ok(None);
    };
    let Some(relative) = after.strip_prefix("root:") else {
        return Ok(None);
    };
    validate_segment(drive)?;
    let relative = relative.trim_matches('/');
    if relative.split('/').any(|part| matches!(part, "." | "..")) {
        return Err(invalid_input());
    }
    Ok(Some((drive.to_owned(), relative.to_owned())))
}

/// `drives/{id}/root:/{path}[:/{suffix}]`, each path part one encoded
/// segment, so a name with a space, `#`, `?` or `%` in it reaches Graph as
/// the name it is.
fn drive_item_url(
    drive_id: &str,
    path: &str,
    suffix: Option<&str>,
) -> Result<Url, SharePointClientError> {
    let parts = path
        .split('/')
        .filter(|part| !part.is_empty())
        .collect::<Vec<_>>();
    if parts.is_empty() || parts.iter().any(|part| matches!(*part, "." | "..")) {
        return Err(invalid_input());
    }
    let mut url = Url::parse(GRAPH_BASE).map_err(|_| invalid_configuration())?;
    {
        let mut segments = url
            .path_segments_mut()
            .map_err(|()| invalid_configuration())?;
        segments.pop_if_empty();
        validate_segment(drive_id)?;
        segments.push("drives").push(drive_id).push("root:");
        for (index, part) in parts.iter().enumerate() {
            validate_segment(part)?;
            if part.contains(':') {
                return Err(invalid_input());
            }
            if index + 1 == parts.len() && suffix.is_some() {
                segments.push(&format!("{part}:"));
            } else {
                segments.push(part);
            }
        }
        if let Some(suffix) = suffix {
            segments.push(suffix);
        }
    }
    Ok(url)
}

/// A name `SharePoint` can store and a path can carry unchanged.
fn validate_file_name(name: &str) -> Result<(), SharePointClientError> {
    let trimmed = name.trim();
    if trimmed.is_empty()
        || trimmed != name
        || name.len() > MAX_FILE_NAME_BYTES
        || matches!(name, "." | "..")
        || name.ends_with('.')
        || name.chars().any(|c| {
            c.is_control() || matches!(c, '"' | '*' | ':' | '<' | '>' | '?' | '/' | '\\' | '|')
        })
    {
        return Err(invalid_input());
    }
    Ok(())
}

/// A sharing-link file before any of it is fetched: its size against the cap
/// of its type, an extension, no archive or executable before the last one.
/// Returns the byte cap to download under.
fn validate_shared_file(name: &str, size: Option<u64>) -> Result<usize, SharePointClientError> {
    let extension = name
        .rsplit_once('.')
        .map(|(_, extension)| extension.to_ascii_lowercase())
        .filter(|extension| !extension.is_empty() && !name.starts_with('.'));
    let is_image = extension.as_deref().is_some_and(|extension| {
        matches!(
            extension,
            "png" | "jpg" | "jpeg" | "gif" | "webp" | "bmp" | "tif" | "tiff"
        )
    });
    let limit = if is_image {
        MAX_IMAGE_READ_BYTES
    } else {
        MAX_READ_BYTES
    };
    if size.is_some_and(|size| size > limit as u64) {
        return Err(file_refused(TOO_LARGE));
    }
    let Some(extension) = extension else {
        return Err(file_refused(NO_EXTENSION));
    };
    let lowered = name.to_ascii_lowercase();
    let parts = lowered.split('.').collect::<Vec<_>>();
    if parts.len() > 2
        && parts[1..parts.len() - 1]
            .iter()
            .any(|part| DANGEROUS_INTERMEDIATE.contains(part))
    {
        return Err(file_refused(DOUBLE_EXTENSION));
    }
    if matches!(
        extension.as_str(),
        "zip"
            | "rar"
            | "7z"
            | "tar"
            | "gz"
            | "exe"
            | "msi"
            | "bat"
            | "cmd"
            | "dll"
            | "so"
            | "bin"
            | "mp4"
            | "avi"
            | "mov"
            | "mp3"
            | "wav"
            | "iso"
            | "dmg"
            | "apk"
    ) {
        return Err(file_refused(UNSUPPORTED_TYPE));
    }
    Ok(limit)
}

/// The target of a redirect, or an invalid answer when it names none.
fn redirect_target(response: &SharePointHttpResponse) -> Result<Url, SharePointClientError> {
    response
        .location
        .as_deref()
        .filter(|value| !value.contains('\\'))
        .and_then(|value| Url::parse(value).ok())
        .ok_or_else(invalid_response)
}

/// The answer's JSON object.
pub(super) fn json_object(
    response: &SharePointHttpResponse,
) -> Result<Map<String, Value>, SharePointClientError> {
    require_content_type(response.content_type.as_deref(), "application/json")?;
    serde_json::from_slice::<Value>(&response.body)
        .map_err(|_| invalid_response())?
        .as_object()
        .cloned()
        .ok_or_else(invalid_response)
}

/// An unknown outcome before anything was committed is a plain, retryable
/// failure.
fn demote_unknown(error: SharePointClientError) -> SharePointClientError {
    if error.code() == SharePointClientErrorCode::OutcomeUnknown {
        dependency_unavailable()
    } else {
        error
    }
}

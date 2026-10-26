//! Images the person attaches to a turn (#1229): pasted, dropped or picked.
//!
//! The webview never sends a file's bytes over IPC: a dropped or picked
//! file is registered by its absolute path (`attachment_add`), a pasted
//! image once as base64 (`attachment_add_bytes`). Either way the host
//! checks, bounds and keeps the image **in memory** and answers a token;
//! `agent_turn_start` names images by token. Nothing is written to disk
//! but the small thumbnail the thread history keeps.
//!
//! * **Confined reads.** A path is read through a [`Workspace`] rooted at
//!   its parent folder (or at the workspace itself when it is inside it,
//!   so `path_deny` applies), with the session's deny list: credentials
//!   and the app's own data are refused (`path_denied`) wherever they are.
//!   The last component is opened `O_NOFOLLOW` and must be a regular file.
//! * **Bounds** are the host's, not the UI's ([`elitea_local_tools::picture`]):
//!   at most [`MAX_IMAGES`] per turn, PNG/JPEG/WebP/GIF sniffed from the
//!   bytes, shrunk to 2048 px on the longest side and 5 MiB.
//! * The registry holds at most [`MAX_HELD`] images and [`MAX_HELD_BYTES`]
//!   in all; the oldest go first. A token is used up by the turn that
//!   starts with it, and names one workspace only.

use std::collections::VecDeque;
use std::path::Path;
use std::sync::{Arc, Mutex, PoisonError};

use base64::Engine as _;
use base64::engine::general_purpose::STANDARD as BASE64;
use elitea_local_tools::deny::DenyList;
use elitea_local_tools::error::ErrorCode;
use elitea_local_tools::picture::{self, Picture, PictureError};
use elitea_local_tools::workspace::{Intent, Workspace};

use super::turn::TurnError;

/// The most images one turn takes.
pub const MAX_IMAGES: usize = 5;
/// The most images the registry holds at once.
pub const MAX_HELD: usize = 32;
/// The most bytes the registry holds at once.
pub const MAX_HELD_BYTES: usize = 64 * 1024 * 1024;

/// `agent_turn_start.images[]`: where the image came from, and its token.
#[derive(Clone, Debug, serde::Deserialize, PartialEq, Eq)]
pub struct ImagePick {
    pub source: ImageSource,
    pub token: String,
}

/// How the person attached an image.
#[derive(Clone, Copy, Debug, serde::Deserialize, PartialEq, Eq)]
#[serde(rename_all = "snake_case")]
pub enum ImageSource {
    Clipboard,
    File,
}

/// One attached image, as the turn sends it.
#[derive(Debug)]
pub struct Attachment {
    pub token: String,
    pub workspace_id: String,
    /// The file's name, or `pasted-image` for a paste: shown, and noted in
    /// the commit; never a path.
    pub name: String,
    pub picture: Picture,
    /// A JPEG of at most 64 KiB for the thread history and the composer.
    pub thumbnail: Option<Vec<u8>>,
}

/// `attachment_add`'s and `attachment_add_bytes`'s answer.
#[derive(Clone, Debug, serde::Serialize, PartialEq, Eq)]
pub struct Added {
    pub token: String,
    pub name: String,
    pub mime: String,
    pub bytes: usize,
    pub width: u32,
    pub height: u32,
    /// `data:image/jpeg;base64,…`, for the composer's thumbnail.
    pub thumbnail: Option<String>,
}

/// The images attached and not yet sent.
#[derive(Default)]
pub struct Attachments {
    held: Mutex<VecDeque<Arc<Attachment>>>,
}

impl Attachments {
    fn locked(&self) -> std::sync::MutexGuard<'_, VecDeque<Arc<Attachment>>> {
        self.held.lock().unwrap_or_else(PoisonError::into_inner)
    }

    fn keep(&self, attachment: Attachment) -> Added {
        let added = Added {
            token: attachment.token.clone(),
            name: attachment.name.clone(),
            mime: attachment.picture.mime.to_owned(),
            bytes: attachment.picture.bytes.len(),
            width: attachment.picture.width,
            height: attachment.picture.height,
            thumbnail: attachment
                .thumbnail
                .as_deref()
                .map(|jpeg| format!("data:image/jpeg;base64,{}", BASE64.encode(jpeg))),
        };
        let mut held = self.locked();
        held.push_back(Arc::new(attachment));
        let mut total: usize = held.iter().map(|a| a.picture.bytes.len()).sum();
        while held.len() > MAX_HELD || total > MAX_HELD_BYTES {
            let Some(oldest) = held.pop_front() else {
                break;
            };
            total -= oldest.picture.bytes.len();
        }
        added
    }

    /// Register a pasted image.
    ///
    /// # Errors
    ///
    /// `invalid_request` for bytes that are not an image the host takes,
    /// `image_too_large` for one that cannot be brought within the limits.
    pub fn add_bytes(
        &self,
        workspace_id: &str,
        name: Option<&str>,
        data: &str,
    ) -> Result<Added, TurnError> {
        // A base64 string of the cap's size is about 4/3 of it.
        if data.len() > picture::MAX_SOURCE_BYTES / 3 * 4 + 8 {
            return Err(too_large(&PictureError::TooLarge(format!(
                "the image is over {} MiB",
                picture::MAX_SOURCE_BYTES / (1024 * 1024)
            ))));
        }
        let bytes = BASE64
            .decode(data.trim())
            .map_err(|_| TurnError::new("invalid_request", "The image is not valid base64."))?;
        self.add(workspace_id, name.unwrap_or("pasted-image"), &bytes)
    }

    /// Register the file at `path`: absolute, a regular file (never a
    /// symlink), outside the deny list, and outside `path_deny` when it is
    /// inside the workspace `root`.
    ///
    /// # Errors
    ///
    /// `path_denied`, `not_found`, `image_too_large`, `invalid_request`.
    pub fn add_path(
        &self,
        workspace_id: &str,
        root: &Path,
        path_deny: &[String],
        deny: Arc<DenyList>,
        path: &str,
    ) -> Result<Added, TurnError> {
        let (name, bytes) = read_path(root, path_deny, deny, path)?;
        self.add(workspace_id, &name, &bytes)
    }

    fn add(&self, workspace_id: &str, name: &str, source: &[u8]) -> Result<Added, TurnError> {
        let picture = picture::prepare(source).map_err(|error| match error {
            PictureError::TooLarge(_) => too_large(&error),
            other => TurnError::new("invalid_request", other.message()),
        })?;
        let thumbnail = picture::thumbnail(&picture.bytes);
        Ok(self.keep(Attachment {
            token: uuid::Uuid::new_v4().to_string(),
            workspace_id: workspace_id.to_owned(),
            name: shown_name(name),
            picture,
            thumbnail,
        }))
    }

    /// Drop an image the person removed before sending.
    pub fn remove(&self, workspace_id: &str, token: &str) {
        self.locked()
            .retain(|a| !(a.token == token && a.workspace_id == workspace_id));
    }

    /// The images `picks` name, in order, for a turn in `workspace_id`.
    ///
    /// # Errors
    ///
    /// `too_many_images` past [`MAX_IMAGES`]; `attachment_unknown` for a
    /// token that is not held for this workspace (sent, removed, evicted,
    /// or from another workspace).
    pub fn resolve(
        &self,
        workspace_id: &str,
        picks: &[ImagePick],
    ) -> Result<Vec<Arc<Attachment>>, TurnError> {
        if picks.len() > MAX_IMAGES {
            return Err(TurnError::new(
                "too_many_images",
                format!("A message can carry at most {MAX_IMAGES} images."),
            ));
        }
        let held = self.locked();
        let mut out: Vec<Arc<Attachment>> = Vec::new();
        for pick in picks {
            if out.iter().any(|a| a.token == pick.token) {
                continue;
            }
            let found = held
                .iter()
                .find(|a| a.token == pick.token && a.workspace_id == workspace_id)
                .ok_or_else(|| {
                    TurnError::new(
                        "attachment_unknown",
                        "An attached image is no longer available; attach it again.",
                    )
                })?;
            out.push(found.clone());
        }
        Ok(out)
    }

    /// Forget the images a started turn carries.
    pub fn consume(&self, images: &[Arc<Attachment>]) {
        self.locked()
            .retain(|held| !images.iter().any(|a| a.token == held.token));
    }

    /// Forget every image (the session ended).
    pub fn clear(&self) {
        self.locked().clear();
    }

    /// How many images are held (tests).
    #[cfg(test)]
    pub fn len(&self) -> usize {
        self.locked().len()
    }
}

fn too_large(error: &PictureError) -> TurnError {
    TurnError::new("image_too_large", error.message())
}

/// A name for the UI and the commit: the final component, without control
/// characters, at most 120 characters.
fn shown_name(name: &str) -> String {
    let base = name.rsplit(['/', '\\']).next().unwrap_or(name);
    let clean: String = base.chars().filter(|c| !c.is_control()).take(120).collect();
    if clean.is_empty() {
        "image".to_owned()
    } else {
        clean
    }
}

/// The name and bytes of the file at `path`, confined (see the module).
fn read_path(
    root: &Path,
    path_deny: &[String],
    deny: Arc<DenyList>,
    path: &str,
) -> Result<(String, Vec<u8>), TurnError> {
    let invalid = |why: &str| TurnError::new("invalid_request", why);
    let given = Path::new(path);
    if !given.is_absolute() || path.contains('\0') {
        return Err(invalid("The dropped file needs an absolute path."));
    }
    let name = given
        .file_name()
        .and_then(|name| name.to_str())
        .ok_or_else(|| invalid("The file has no usable name."))?
        .to_owned();
    let parent = given
        .parent()
        .and_then(|parent| std::fs::canonicalize(parent).ok())
        .ok_or_else(|| TurnError::new("not_found", "That file does not exist."))?;
    // Inside the workspace the workspace's own view applies (path_deny
    // too); anywhere else the parent folder is the view.
    let inside = std::fs::canonicalize(root)
        .ok()
        .filter(|root| parent.starts_with(root));
    let (view_root, view_deny): (&Path, &[String]) = match &inside {
        Some(root) => (root, path_deny),
        None => (&parent, &[]),
    };
    let workspace = Workspace::open(view_root, view_deny)
        .map_err(|_| invalid("That folder cannot be used."))?
        .with_deny_list(deny);
    let target = parent.join(&name);
    let relative = target
        .strip_prefix(view_root)
        .ok()
        .and_then(|relative| relative.to_str())
        .filter(|relative| !relative.is_empty())
        .ok_or_else(|| invalid("That file cannot be used."))?;
    let resolved = workspace.resolve(relative, Intent::Read).map_err(denied)?;
    let file = workspace
        .read(&resolved, picture::MAX_SOURCE_BYTES as u64)
        .map_err(denied)?;
    Ok((name, file.bytes))
}

fn denied(error: elitea_local_tools::error::ToolError) -> TurnError {
    match error.code() {
        ErrorCode::Denied | ErrorCode::OutsideWorkspace => TurnError::new(
            "path_denied",
            "That file is protected (credentials, the app's own data or a denied path) and cannot be attached.",
        ),
        ErrorCode::NotFound => TurnError::new("not_found", "That file does not exist."),
        ErrorCode::TooLarge => TurnError::new(
            "image_too_large",
            format!(
                "The image is over {} MiB.",
                picture::MAX_SOURCE_BYTES / (1024 * 1024)
            ),
        ),
        _ => TurnError::new(
            "invalid_request",
            "That is not a regular file that can be read (links are not followed).",
        ),
    }
}

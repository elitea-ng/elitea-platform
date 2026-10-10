//! A workspace folder as a [`ContentSource`] (ADR-0028): every file the
//! agent's own tools would list is a document.
//!
//! The listing is the confined walk of `elitea-local-tools`
//! ([`walker`]): `.gitignore` honoured, `.git` and `path_deny` matches
//! pruned, symlinks never followed (nor listed: only regular files are
//! documents). Files over [`MAX_FILE_BYTES`] are left out, as the file
//! tools leave them.
//!
//! A document's version is the SHA-256 of its bytes, read through
//! [`Workspace::read`] (`openat` with `O_NOFOLLOW` from the root's
//! descriptor). Hashing is what costs, so it is done only when a file's
//! `(size, mtime)` differs from what the last completed run recorded
//! ([`LocalFolderSource::with_previous`]): a refresh of an unchanged
//! folder is one `stat` per file. A file whose content changed without
//! its size or mtime changing is not noticed until either does (or a full
//! rebuild), the trade-off every mtime-based build tool makes.
//!
//! Every document is readable by anyone who can open the index
//! ([`Acl::Project`]): it is the person's own folder.

use crate::sqlite_store::{DocumentStat, Stats};
use elitea_content_source::{
    Acl, ContentSource, Document, DocumentRef, SourceError, content_version, mime_of,
};
use elitea_inventory_core::ingest::files::Selection;
use elitea_local_tools::files::{MAX_FILE_BYTES, relative, walker};
use elitea_local_tools::workspace::{Workspace, WsPath};
use serde_json::Map;
use std::collections::HashMap;
use std::path::Path;
use std::sync::atomic::{AtomicUsize, Ordering};
use std::sync::{Arc, Mutex, PoisonError};

/// The name every citation of a local index carries (`source_toolkit`),
/// and its one source's status key.
pub const SOURCE_NAME: &str = "workspace";

/// The files of one workspace.
#[derive(Debug)]
pub struct LocalFolderSource {
    workspace: Arc<Workspace>,
    previous: HashMap<String, DocumentStat>,
    selection: Option<Selection>,
    seen: Mutex<Stats>,
    hashed: AtomicUsize,
}

fn mtime_ns(meta: &std::fs::Metadata) -> i64 {
    use std::os::unix::fs::MetadataExt as _;
    meta.mtime()
        .saturating_mul(1_000_000_000)
        .saturating_add(meta.mtime_nsec())
}

fn unavailable(message: impl Into<String>) -> SourceError {
    SourceError::Unavailable(message.into())
}

impl LocalFolderSource {
    /// Every regular file of `workspace`, each hashed when listed.
    #[must_use]
    pub fn new(workspace: Arc<Workspace>) -> Self {
        Self {
            workspace,
            previous: HashMap::new(),
            selection: None,
            seen: Mutex::default(),
            hashed: AtomicUsize::new(0),
        }
    }

    /// Reuse the version of a file whose `(size, mtime)` is the one
    /// `previous` recorded, instead of reading and hashing it again.
    #[must_use]
    pub fn with_previous(mut self, previous: HashMap<String, DocumentStat>) -> Self {
        self.previous = previous;
        self
    }

    /// Hash only the files `selection` admits; the others are still listed
    /// (ingestion counts them as skipped) with an empty version, never read.
    #[must_use]
    pub fn selecting(mut self, selection: Selection) -> Self {
        self.selection = Some(selection);
        self
    }

    /// The workspace listed.
    #[must_use]
    pub fn workspace(&self) -> &Arc<Workspace> {
        &self.workspace
    }

    /// How many files the listings so far read to hash.
    #[must_use]
    pub fn hashed(&self) -> usize {
        self.hashed.load(Ordering::SeqCst)
    }

    /// The `(size, mtime)` the last listing paired with each version it
    /// gave: what the store records beside the versions it commits.
    #[must_use]
    pub fn stats(&self) -> Stats {
        self.seen
            .lock()
            .unwrap_or_else(PoisonError::into_inner)
            .clone()
    }

    fn admitted(&self, key: &str, mime: &str) -> bool {
        self.selection
            .as_ref()
            .is_none_or(|selection| selection.admit_document(key, mime).is_ok())
    }

    /// The listing, synchronously (it walks and reads the disk).
    ///
    /// # Errors
    ///
    /// The walk cannot start.
    pub fn list_now(&self) -> Result<Vec<DocumentRef>, SourceError> {
        let walk = walker(&self.workspace, &WsPath::root(), None, None)
            .map_err(|error| unavailable(error.message()))?;
        let mut listed = Vec::new();
        let mut seen = Stats::new();
        for entry in walk {
            let Ok(entry) = entry else { continue };
            // Regular files only: a symlink is neither listed nor followed.
            if !entry.file_type().is_some_and(|kind| kind.is_file()) {
                continue;
            }
            let Some(path) = relative(&self.workspace, entry.path()) else {
                continue;
            };
            if path.is_root() {
                continue;
            }
            let Ok(meta) = entry.metadata() else { continue };
            if meta.len() > MAX_FILE_BYTES {
                continue;
            }
            let key = path.display_string();
            let mime = mime_of(&key).to_owned();
            if !self.admitted(&key, &mime) {
                listed.push(DocumentRef {
                    key,
                    version: String::new(),
                    mime,
                    size: meta.len(),
                    acl: Acl::Project,
                });
                continue;
            }
            let (size, modified) = (meta.len(), mtime_ns(&meta));
            let cached = self
                .previous
                .get(&key)
                .filter(|known| known.size == size && known.mtime_ns == modified);
            let (version, size, modified) = if let Some(known) = cached {
                (known.version.clone(), size, modified)
            } else {
                // Read through the confined path: what is hashed is what a
                // read of this key returns, and the stat is the read's own.
                let Ok(read) = self.workspace.read(&path, MAX_FILE_BYTES) else {
                    continue;
                };
                self.hashed.fetch_add(1, Ordering::SeqCst);
                let modified = i64::try_from(read.stamp.mtime_ns).unwrap_or(i64::MAX);
                (content_version(&read.bytes), read.stamp.len, modified)
            };
            seen.insert(key.clone(), (size, modified));
            listed.push(DocumentRef {
                key,
                version,
                mime,
                size,
                acl: Acl::Project,
            });
        }
        *self.seen.lock().unwrap_or_else(PoisonError::into_inner) = seen;
        Ok(listed)
    }

    /// One document, read through the confined path.
    ///
    /// # Errors
    ///
    /// The key is not a workspace path, the file is gone, denied, too
    /// large or not a regular file.
    pub fn fetch_now(&self, key: &str) -> Result<Document, SourceError> {
        let path = WsPath::from_relative(Path::new(key))
            .map_err(|_| SourceError::NotFound(key.to_owned()))?;
        let read = self
            .workspace
            .read(&path, MAX_FILE_BYTES)
            .map_err(|error| unavailable(error.message()))?;
        Ok(Document {
            reference: DocumentRef {
                key: key.to_owned(),
                version: content_version(&read.bytes),
                mime: mime_of(key).to_owned(),
                size: read.stamp.len,
                acl: Acl::Project,
            },
            title: path.file_name().unwrap_or(key).to_owned(),
            uri: None,
            bytes: read.bytes,
            metadata: Map::new(),
        })
    }
}

impl ContentSource for LocalFolderSource {
    async fn list(&self) -> Result<Vec<DocumentRef>, SourceError> {
        self.list_now()
    }

    async fn fetch(&self, key: &str) -> Result<Document, SourceError> {
        self.fetch_now(key)
    }
}

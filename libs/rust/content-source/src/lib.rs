//! The content layer (ADR-0028): what an engine ingests, whatever it came
//! from.
//!
//! A [`ContentSource`] lists [`DocumentRef`]s — a stable key, a version, a
//! mime type, an [`Acl`] — and fetches a [`Document`]'s bytes. The engines
//! never see a connector's protocol or credentials: a connector is built
//! per invocation from what the platform facade expanded (or calls the
//! platform with the invocation's bearer).
//!
//! * [`git`] — a checked-out repository (the clone is `elitea-repo-ingest`'s);
//!   every file is a document, readable by anyone with the project
//!   ([`Acl::Project`]).
//!
//! Connectors to come (artifact buckets, Confluence, `SharePoint`, Jira, …)
//! implement the same trait, with per-document ACLs where the source has
//! them.

pub mod acl;
pub mod git;

pub use acl::{Acl, Caller, Principal, PrincipalKind};

use serde::{Deserialize, Serialize};
use serde_json::{Map, Value};
use sha2::{Digest, Sha256};
use std::fmt::Write as _;
use std::future::Future;

/// A connector failed.
#[derive(Debug, thiserror::Error)]
pub enum SourceError {
    /// The source cannot be listed or a document cannot be read.
    #[error("{0}")]
    Unavailable(String),
    /// No document has this key.
    #[error("no document '{0}' in this source")]
    NotFound(String),
}

/// A document as listed: enough to decide whether to fetch it.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct DocumentRef {
    /// Stable within its source: a repository path, a page id, a URL.
    pub key: String,
    /// Changes whenever the content does: a content hash, an etag, a
    /// revision number.
    pub version: String,
    /// The content's media type (`text/markdown`, `application/pdf`, …).
    pub mime: String,
    pub size: u64,
    pub acl: Acl,
}

/// A fetched document.
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct Document {
    #[serde(flatten)]
    pub reference: DocumentRef,
    pub title: String,
    /// Where a reader opens it, when the source has such a place.
    pub uri: Option<String>,
    pub bytes: Vec<u8>,
    /// Author, update time, labels — what the source knows.
    pub metadata: Map<String, Value>,
}

/// A source of documents.
pub trait ContentSource: Send + Sync {
    /// Every document the source holds, in a stable order.
    fn list(&self) -> impl Future<Output = Result<Vec<DocumentRef>, SourceError>> + Send;

    /// One document's content.
    fn fetch(&self, key: &str) -> impl Future<Output = Result<Document, SourceError>> + Send;
}

/// The lowercase SHA-256 hex of `bytes`: the version a source without its
/// own revision ids uses.
#[must_use]
pub fn content_version(bytes: &[u8]) -> String {
    let mut hex = String::with_capacity(64);
    for byte in Sha256::digest(bytes) {
        let _ = write!(hex, "{byte:02x}");
    }
    hex
}

/// The media type of a path, by extension (lowercase); `text/plain` for a
/// known text-like file without a specific type, and
/// `application/octet-stream` otherwise.
#[must_use]
pub fn mime_of(key: &str) -> &'static str {
    let name = key.rsplit('/').next().unwrap_or(key);
    let extension = name
        .rfind('.')
        .filter(|dot| *dot > 0)
        .map(|dot| name[dot + 1..].to_lowercase())
        .unwrap_or_default();
    match extension.as_str() {
        "md" | "markdown" | "mdown" | "mkd" | "mdx" => "text/markdown",
        "html" | "htm" => "text/html",
        "xml" => "application/xml",
        "json" | "jsonl" | "jsonc" => "application/json",
        "yml" | "yaml" => "application/yaml",
        "csv" => "text/csv",
        "pdf" => "application/pdf",
        "docx" => "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
        "xlsx" => "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
        "pptx" => "application/vnd.openxmlformats-officedocument.presentationml.presentation",
        "doc" => "application/msword",
        "xls" => "application/vnd.ms-excel",
        "ppt" => "application/vnd.ms-powerpoint",
        "odt" => "application/vnd.oasis.opendocument.text",
        "rtf" => "application/rtf",
        "epub" => "application/epub+zip",
        "eml" => "message/rfc822",
        "msg" => "application/vnd.ms-outlook",
        "png" => "image/png",
        "jpg" | "jpeg" => "image/jpeg",
        "txt" | "rst" | "toml" | "ini" | "cfg" | "conf" | "env" | "py" | "js" | "jsx" | "mjs"
        | "cjs" | "ts" | "tsx" | "java" | "kt" | "kts" | "rs" | "go" | "cpp" | "c" | "cs" | "h"
        | "hpp" | "hs" | "rb" | "scala" | "lua" | "sh" | "bash" | "zsh" | "sql" | "r" | "swift"
        | "php" | "pl" | "pm" | "m" | "bat" | "pas" | "asm" | "dart" | "groovy" => "text/plain",
        _ => "application/octet-stream",
    }
}

/// Whether a media type's bytes are text to decode (not a format to
/// extract).
#[must_use]
pub fn is_text(mime: &str) -> bool {
    mime.starts_with("text/")
        || matches!(
            mime,
            "application/json" | "application/xml" | "application/yaml"
        )
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn media_types_follow_the_extension() {
        assert_eq!(mime_of("docs/Guide.MD"), "text/markdown");
        assert_eq!(mime_of("a/b.pdf"), "application/pdf");
        assert_eq!(mime_of("src/app.py"), "text/plain");
        assert_eq!(mime_of("a/.env"), "application/octet-stream");
        assert_eq!(mime_of("Makefile"), "application/octet-stream");
        assert!(is_text("text/markdown") && is_text("application/json"));
        assert!(!is_text("application/pdf"));
    }

    #[test]
    fn a_version_is_the_content_hash() {
        assert_eq!(
            content_version(b"abc"),
            "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
        );
    }
}

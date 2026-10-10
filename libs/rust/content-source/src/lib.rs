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

/// What an extension says a file is, for choosing a chunker. The one
/// table every consumer reads (`EXTENSIONS`).
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Kind {
    /// Markdown.
    Markdown,
    /// HTML: markdown once converted (`doc-extract`).
    Html,
    /// JSON and JSON lines.
    Json,
    /// Source code with a method structure (`doc-chunk`'s code chunker).
    Code,
    /// Anything else textual: plain text, configuration, CSV, XML, ….
    Text,
    /// Binary documents and images: extracted or skipped, never chunked as
    /// they are.
    Binary,
}

/// One row of the extension table: lower-case extension without the dot, the
/// media type `mime_of` gives it (`None`: a known kind with no media type of
/// its own here), and the kind.
struct Row(&'static str, Option<&'static str>, Kind);

const DOCX: &str = "application/vnd.openxmlformats-officedocument.wordprocessingml.document";
const XLSX: &str = "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet";
const PPTX: &str = "application/vnd.openxmlformats-officedocument.presentationml.presentation";

/// The canonical extension table. When two extensions give the same media
/// type, the first row is the one [`extension_of_mime`] answers with.
const EXTENSIONS: &[Row] = &[
    Row("md", Some("text/markdown"), Kind::Markdown),
    Row("markdown", Some("text/markdown"), Kind::Markdown),
    Row("mdown", Some("text/markdown"), Kind::Markdown),
    Row("mkd", Some("text/markdown"), Kind::Markdown),
    Row("mdx", Some("text/markdown"), Kind::Markdown),
    Row("html", Some("text/html"), Kind::Html),
    Row("htm", Some("text/html"), Kind::Html),
    // Chunked as HTML, but its media type stays unlisted (as before).
    Row("xhtml", None, Kind::Html),
    Row("xml", Some("application/xml"), Kind::Text),
    Row("json", Some("application/json"), Kind::Json),
    Row("jsonl", Some("application/json"), Kind::Json),
    Row("jsonc", Some("application/json"), Kind::Json),
    Row("yaml", Some("application/yaml"), Kind::Text),
    Row("yml", Some("application/yaml"), Kind::Text),
    Row("csv", Some("text/csv"), Kind::Text),
    Row("pdf", Some("application/pdf"), Kind::Binary),
    Row("docx", Some(DOCX), Kind::Binary),
    Row("xlsx", Some(XLSX), Kind::Binary),
    Row("pptx", Some(PPTX), Kind::Binary),
    Row("doc", Some("application/msword"), Kind::Binary),
    Row("xls", Some("application/vnd.ms-excel"), Kind::Binary),
    Row("ppt", Some("application/vnd.ms-powerpoint"), Kind::Binary),
    Row(
        "odt",
        Some("application/vnd.oasis.opendocument.text"),
        Kind::Binary,
    ),
    Row("rtf", Some("application/rtf"), Kind::Binary),
    Row("epub", Some("application/epub+zip"), Kind::Binary),
    Row("eml", Some("message/rfc822"), Kind::Binary),
    Row("msg", Some("application/vnd.ms-outlook"), Kind::Binary),
    Row("png", Some("image/png"), Kind::Binary),
    Row("jpg", Some("image/jpeg"), Kind::Binary),
    Row("jpeg", Some("image/jpeg"), Kind::Binary),
    Row("txt", Some("text/plain"), Kind::Text),
    Row("rst", Some("text/plain"), Kind::Text),
    Row("toml", Some("text/plain"), Kind::Text),
    Row("ini", Some("text/plain"), Kind::Text),
    Row("cfg", Some("text/plain"), Kind::Text),
    Row("conf", Some("text/plain"), Kind::Text),
    Row("env", Some("text/plain"), Kind::Text),
    Row("py", Some("text/plain"), Kind::Code),
    Row("js", Some("text/plain"), Kind::Code),
    Row("jsx", Some("text/plain"), Kind::Code),
    Row("mjs", Some("text/plain"), Kind::Code),
    Row("cjs", Some("text/plain"), Kind::Code),
    Row("ts", Some("text/plain"), Kind::Code),
    Row("tsx", Some("text/plain"), Kind::Code),
    Row("java", Some("text/plain"), Kind::Code),
    Row("kt", Some("text/plain"), Kind::Code),
    // Kotlin script: Kotlin code like `.kt` (it was text in `doc-chunk`
    // and code here before the tables were one).
    Row("kts", Some("text/plain"), Kind::Code),
    Row("rs", Some("text/plain"), Kind::Code),
    Row("go", Some("text/plain"), Kind::Code),
    Row("cpp", Some("text/plain"), Kind::Code),
    Row("c", Some("text/plain"), Kind::Code),
    Row("cs", Some("text/plain"), Kind::Code),
    Row("h", Some("text/plain"), Kind::Code),
    Row("hpp", Some("text/plain"), Kind::Code),
    Row("hs", Some("text/plain"), Kind::Code),
    Row("rb", Some("text/plain"), Kind::Code),
    Row("scala", Some("text/plain"), Kind::Code),
    Row("lua", Some("text/plain"), Kind::Code),
    Row("sh", Some("text/plain"), Kind::Code),
    Row("bash", Some("text/plain"), Kind::Code),
    Row("zsh", Some("text/plain"), Kind::Code),
    Row("sql", Some("text/plain"), Kind::Code),
    Row("r", Some("text/plain"), Kind::Code),
    Row("swift", Some("text/plain"), Kind::Code),
    Row("php", Some("text/plain"), Kind::Code),
    Row("pl", Some("text/plain"), Kind::Code),
    Row("pm", Some("text/plain"), Kind::Code),
    Row("m", Some("text/plain"), Kind::Code),
    Row("bat", Some("text/plain"), Kind::Code),
    Row("pas", Some("text/plain"), Kind::Code),
    Row("asm", Some("text/plain"), Kind::Code),
    Row("dart", Some("text/plain"), Kind::Code),
    Row("groovy", Some("text/plain"), Kind::Code),
];

/// Media types spelled in more than one way, and the extension each stands
/// for (the rows above answer for the canonical spelling).
const MIME_ALIASES: &[(&str, &str)] = &[
    ("text/x-markdown", "md"),
    ("application/xhtml+xml", "xhtml"),
    ("text/json", "json"),
    ("text/x-python", "py"),
    ("application/x-python-code", "py"),
    ("text/javascript", "js"),
    ("application/javascript", "js"),
    ("text/typescript", "ts"),
    ("application/typescript", "ts"),
    ("text/x-java", "java"),
    ("text/x-java-source", "java"),
    ("text/x-rust", "rs"),
    ("text/x-go", "go"),
    ("text/x-kotlin", "kt"),
    ("text/x-csharp", "cs"),
    ("text/x-c", "c"),
    ("text/x-c++", "cpp"),
    ("text/x-cpp", "cpp"),
    ("text/yaml", "yaml"),
    ("text/xml", "xml"),
];

fn row_of(extension: &str) -> Option<&'static Row> {
    EXTENSIONS.iter().find(|row| row.0 == extension)
}

/// The kind of a lower-case extension, with or without its dot. An unknown
/// one is [`Kind::Text`]: anything unlisted is read as plain text.
#[must_use]
pub fn kind_of_extension(extension: &str) -> Kind {
    row_of(extension.strip_prefix('.').unwrap_or(extension)).map_or(Kind::Text, |row| row.2)
}

/// The dotted extension a media type stands for (`text/markdown` -> `.md`),
/// when the table knows it. Parameters (`; charset=…`) are not stripped here.
#[must_use]
pub fn extension_of_mime(mime: &str) -> Option<String> {
    let mime = mime.to_ascii_lowercase();
    // `text/plain` is the table's answer for every code file; the canonical
    // extension of plain text is `.txt`.
    if mime == "text/plain" {
        return Some(".txt".to_owned());
    }
    EXTENSIONS
        .iter()
        .find(|row| row.1 == Some(mime.as_str()))
        .map(|row| row.0)
        .or_else(|| {
            MIME_ALIASES
                .iter()
                .find(|(alias, _)| *alias == mime)
                .map(|(_, extension)| *extension)
        })
        .map(|extension| format!(".{extension}"))
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
    row_of(&extension)
        .and_then(|row| row.1)
        .unwrap_or("application/octet-stream")
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
    fn the_extension_table_answers_kind_and_media_type_both_ways() {
        assert_eq!(mime_of("build.gradle.kts"), "text/plain");
        assert_eq!(kind_of_extension(".kts"), Kind::Code);
        assert_eq!(kind_of_extension("kt"), Kind::Code);
        assert_eq!(kind_of_extension(".md"), Kind::Markdown);
        assert_eq!(kind_of_extension(".xhtml"), Kind::Html);
        assert_eq!(mime_of("a.xhtml"), "application/octet-stream");
        assert_eq!(kind_of_extension(".jsonl"), Kind::Json);
        assert_eq!(kind_of_extension(".yaml"), Kind::Text);
        assert_eq!(kind_of_extension(".nope"), Kind::Text);
        assert_eq!(extension_of_mime("text/markdown").as_deref(), Some(".md"));
        assert_eq!(extension_of_mime("text/plain").as_deref(), Some(".txt"));
        assert_eq!(
            extension_of_mime("application/vnd.ms-excel").as_deref(),
            Some(".xls")
        );
        assert_eq!(extension_of_mime("text/x-kotlin").as_deref(), Some(".kt"));
        assert_eq!(extension_of_mime("application/octet-stream"), None);
        // Every row round-trips to a row with the same media type.
        for row in EXTENSIONS {
            if let Some(mime) = row.1 {
                assert_eq!(mime_of(&format!("f.{}", row.0)), mime);
            }
        }
    }

    #[test]
    fn a_version_is_the_content_hash() {
        assert_eq!(
            content_version(b"abc"),
            "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
        );
    }
}

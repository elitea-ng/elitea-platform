//! Which files of a source are ingested, and the file node each becomes.
//!
//! Selection is the SDK loader's (`CodeIndexerToolkit.loader`, elitea-sdk
//! 0.9.40), in its order: the whitelist (when there is one), the
//! blacklist, the supported extensions, then the content (an empty file
//! is skipped). A pattern matches a path when `fnmatch` does — `*` crosses
//! `/` — or when the path ends with `.` + the pattern, so `py` selects
//! every `.py` file.
//!
//! The file node is `_create_file_node` in `ingestion.py`: a
//! `source_file` / `document_file` / `config_file` / `web_file` / `file`
//! entity named after the file, cited over all its lines, whose id is the
//! file-scoped [`super::ids::entity_id`] of its path.

use super::ids::entity_id;
use crate::graph::Citation;
use elitea_engine_core::pystr;
use serde_json::{Map, Value, json};
use sha2::{Digest, Sha256};
use std::fmt::Write as _;

/// `MARKDOWN | JSON | CODE | CONFIG | TEXT _EXTENSIONS` of the SDK's
/// `universal_chunker`, lowercase with the dot.
const SUPPORTED_EXTENSIONS: &[&str] = &[
    // markdown
    ".md",
    ".markdown",
    ".mdown",
    ".mkd",
    ".mdx", //
    // json
    ".json",
    ".jsonl",
    ".jsonc", //
    // config
    ".yml",
    ".yaml",
    ".toml",
    ".ini",
    ".cfg",
    ".conf",
    ".env", //
    // code
    ".py",
    ".js",
    ".jsx",
    ".mjs",
    ".cjs",
    ".ts",
    ".tsx",
    ".java",
    ".kt",
    ".rs",
    ".go",
    ".cpp",
    ".c",
    ".cs",
    ".hs",
    ".rb",
    ".scala",
    ".lua",
    ".sh",
    ".bash",
    ".zsh",
    ".sql",
    ".r",
    ".swift",
    ".php",
    ".pl",
    ".pm",
    ".h",
    ".hpp",
    ".m",
    ".bat",
    ".pas",
    ".asm",
    ".dart",
    ".groovy", //
    // text
    ".txt",
    ".xml",
    ".html",
    ".htm",
    ".csv",
];

/// `os.path.splitext(path)[1]`: the last dot of the last component and
/// what follows, unless only dots precede it (`.env` has no extension,
/// `a.env` has `.env`).
#[must_use]
pub fn splitext_extension(path: &str) -> &str {
    let name = path.rsplit('/').next().unwrap_or(path);
    match name.rfind('.') {
        Some(dot) if name[..dot].chars().any(|c| c != '.') => &name[dot..],
        _ => "",
    }
}

/// Whether the SDK loader indexes a file of this path at all.
#[must_use]
pub fn has_supported_extension(path: &str) -> bool {
    SUPPORTED_EXTENSIONS.contains(&splitext_extension(path).to_lowercase().as_str())
}

/// A source's whitelist and blacklist.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct Selection {
    /// `None` (or empty) selects everything.
    pub whitelist: Option<Vec<String>>,
    pub blacklist: Option<Vec<String>>,
}

fn matches_any(path: &str, patterns: &[String]) -> bool {
    patterns.iter().any(|pattern| pystr::fnmatch(path, pattern))
        || patterns
            .iter()
            .any(|pattern| path.ends_with(&format!(".{pattern}")))
}

/// Why a path was not read.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Skipped {
    Whitelist,
    Blacklist,
    UnsupportedExtension,
}

impl Selection {
    /// The loader's verdict on a path, before reading it.
    ///
    /// # Errors
    ///
    /// Which rule skipped it.
    pub fn admit(&self, path: &str) -> Result<(), Skipped> {
        if let Some(whitelist) = self.whitelist.as_deref().filter(|list| !list.is_empty())
            && !matches_any(path, whitelist)
        {
            return Err(Skipped::Whitelist);
        }
        if let Some(blacklist) = self.blacklist.as_deref().filter(|list| !list.is_empty())
            && matches_any(path, blacklist)
        {
            return Err(Skipped::Blacklist);
        }
        if !has_supported_extension(path) {
            return Err(Skipped::UnsupportedExtension);
        }
        Ok(())
    }
}

/// The lowercase SHA-256 hex of a file's text (the loader's `commit_hash`).
#[must_use]
pub fn content_hash(text: &str) -> String {
    let mut hex = String::with_capacity(64);
    for byte in Sha256::digest(text.as_bytes()) {
        let _ = write!(hex, "{byte:02x}");
    }
    hex
}

/// The file node's type, by `PurePath.suffix` (lowercased).
#[must_use]
pub fn file_type(path: &str) -> &'static str {
    match pystr::suffix(path).to_lowercase().as_str() {
        ".py" | ".js" | ".ts" | ".jsx" | ".tsx" | ".java" | ".go" | ".rs" | ".kt" | ".swift"
        | ".cs" | ".c" | ".cpp" | ".h" => "source_file",
        ".md" | ".rst" | ".txt" => "document_file",
        ".yml" | ".yaml" | ".json" | ".toml" | ".ini" | ".cfg" => "config_file",
        ".html" | ".css" | ".scss" | ".less" => "web_file",
        _ => "file",
    }
}

/// What a file's entities count, for the file node's properties.
#[derive(Debug, Clone, Copy, Default, PartialEq, Eq)]
pub struct EntityCounts {
    /// Every entity extracted from the file (not the file node).
    pub entities: usize,
    /// Of those: class, function, method, module, interface.
    pub code: usize,
    /// Of those: fact.
    pub facts: usize,
}

/// One file node, ready for `Graph::add_entity`.
#[derive(Debug, Clone, PartialEq)]
pub struct FileNode {
    pub id: String,
    pub name: String,
    pub entity_type: &'static str,
    pub citation: Citation,
    pub properties: Map<String, Value>,
}

/// The file node of `path` with text `content`, read from source
/// `source_toolkit`.
#[must_use]
pub fn file_node(
    path: &str,
    content: &str,
    source_toolkit: &str,
    counts: EntityCounts,
) -> FileNode {
    let hash = content_hash(content);
    let line_count = content.matches('\n').count() + 1;
    let properties = json!({
        "full_path": path,
        "extension": pystr::suffix(path).to_lowercase(),
        "line_count": line_count,
        "size_bytes": content.len(),
        "content_hash": hash,
        "entity_count": counts.entities,
        "code_entity_count": counts.code,
        "fact_count": counts.facts,
        "other_entity_count": counts.entities - counts.code - counts.facts,
    });
    FileNode {
        id: entity_id("file", path, Some(path)),
        name: pystr::file_name(path).to_owned(),
        entity_type: file_type(path),
        citation: Citation {
            file_path: path.to_owned(),
            line_start: Some(1),
            line_end: i64::try_from(line_count).ok(),
            source_toolkit: Some(source_toolkit.to_owned()),
            doc_id: Some(format!("{source_toolkit}://{path}")),
            content_hash: Some(hash),
        },
        properties: match properties {
            Value::Object(fields) => fields,
            _ => Map::new(),
        },
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn splitext_follows_os_path() {
        for (path, extension) in [
            ("a/b.py", ".py"),
            ("a/b.tar.gz", ".gz"),
            ("a/.env", ""),
            ("a/x.env", ".env"),
            ("a/..b", ""),
            ("a/b.", "."),
            ("a.d/b", ""),
            ("Makefile", ""),
        ] {
            assert_eq!(splitext_extension(path), extension, "{path}");
        }
    }

    #[test]
    fn selection_is_the_loader_order() {
        let selection = Selection {
            whitelist: Some(vec!["src/*".to_owned(), "md".to_owned()]),
            blacklist: Some(vec!["*/vendor/*".to_owned()]),
        };
        assert_eq!(selection.admit("src/a/b.py"), Ok(()), "`*` crosses `/`");
        assert_eq!(selection.admit("docs/x.md"), Ok(()), "a bare extension");
        assert_eq!(selection.admit("docs/x.py"), Err(Skipped::Whitelist));
        assert_eq!(selection.admit("src/vendor/x.py"), Err(Skipped::Blacklist));
        assert_eq!(
            selection.admit("src/logo.png"),
            Err(Skipped::UnsupportedExtension)
        );
        assert_eq!(
            Selection::default().admit("README.MD"),
            Ok(()),
            "extension case folds"
        );
        let empty = Selection {
            whitelist: Some(Vec::new()),
            blacklist: None,
        };
        assert_eq!(
            empty.admit("a.py"),
            Ok(()),
            "an empty whitelist selects all"
        );
    }

    #[test]
    fn the_file_node_is_cited_over_all_its_lines() {
        let node = file_node("src/Users.PY", "a\nb\n", "repo", EntityCounts::default());
        assert_eq!(node.name, "Users.PY");
        assert_eq!(node.entity_type, "source_file");
        assert_eq!(node.citation.line_end, Some(3));
        assert_eq!(node.citation.doc_id.as_deref(), Some("repo://src/Users.PY"));
        assert_eq!(node.properties["extension"], json!(".py"));
        assert_eq!(node.properties["size_bytes"], json!(4));
    }
}

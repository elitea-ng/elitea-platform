//! The SDK loader's file-type allowlist, shared by every indexer that
//! reproduces it (the Inventory ingest and the toolkit index worker), so the
//! list lives once.
//!
//! `SUPPORTED_EXTENSIONS` is `SUPPORTED_EXTENSIONS` of the SDK's
//! `code_indexer_toolkit.loader` (from `chunkers/universal_chunker.py`),
//! kept verbatim; a test pins it.

/// `MARKDOWN | JSON | CODE | CONFIG | TEXT _EXTENSIONS` of the SDK's
/// `universal_chunker`, lowercase with the dot: markdown, JSON, config, code
/// and text.
pub const SUPPORTED_EXTENSIONS: &[&str] = &[
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

#[cfg(test)]
mod tests {
    use super::*;
    use sha2::{Digest, Sha256};

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

    /// The list is the SDK's, verbatim: any edit must be a deliberate
    /// re-sync with `universal_chunker.py`, and changes this digest.
    #[test]
    fn the_list_is_the_sdks_verbatim() {
        assert_eq!(SUPPORTED_EXTENSIONS.len(), 55);
        let joined = SUPPORTED_EXTENSIONS.join("\n");
        let digest = format!("{:x}", Sha256::digest(joined.as_bytes()));
        assert_eq!(
            digest,
            "efebf83caec4b6a40026ee498b59cae39050e4b7700d3b683d748d10c2ab1b4a"
        );
        for kept in ["src/a.PY", "docs/README.md", "x/.env.yaml", "a.csv"] {
            assert!(has_supported_extension(kept), "{kept}");
        }
        for skipped in ["logo.png", "paper.pdf", "a/.env", "Makefile", "a.tar.gz"] {
            assert!(!has_supported_extension(skipped), "{skipped}");
        }
    }
}

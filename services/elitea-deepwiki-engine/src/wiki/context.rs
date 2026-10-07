//! The page's context text: `_format_simple_context` and what it calls
//! (`_is_documentation_file`, `_extract_imports_for_file`,
//! `_pylon_endpoints_for_file`).
//!
//! QUIRK (kept): the parts are joined with the two characters `\` `n`,
//! not a newline (`"\\n".join(context_parts)` in Python), and so are the
//! documents of one file. The model has always read it that way; the
//! prompt bytes are the gate.

use super::doc::ContextDoc;
use super::index::GraphFacts;
use super::pyregex::{Flags, PyRe};
use super::spec::PageSpec;
use crate::graph::api_surface::{derive_pylon_endpoints, plugin_name_from_metadata_text};
use crate::graph::discover::{DOCUMENTATION_EXTENSIONS, KNOWN_FILENAMES};
use crate::graph::pystr;
use std::path::{Path, PathBuf};
use std::sync::{LazyLock, Mutex};

/// The literal two-character separator (see the module comment).
const SEP: &str = "\\n";

/// What `_format_simple_context` returns that generation reads.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct RelevantContent {
    /// `relevant_content["content"]`.
    pub content: String,
    /// `relevant_content["files"]`: prioritised files first.
    pub files: Vec<String>,
}

/// `_is_documentation_file`.
#[must_use]
pub fn is_documentation_file(file_path: &str) -> bool {
    let ext = pystr::suffix(file_path).to_lowercase();
    if DOCUMENTATION_EXTENSIONS.iter().any(|(e, _)| *e == ext) {
        return true;
    }
    let name = pystr::file_name(file_path);
    if KNOWN_FILENAMES.iter().any(|(n, _)| *n == name) {
        return true;
    }
    let stem = pystr::stem(file_path).to_lowercase();
    if ["readme", "changelog", "license", "contributing", "docs"].contains(&stem.as_str()) {
        return true;
    }
    let lower = file_path.to_lowercase();
    lower.contains("/docs/") || lower.contains("/documentation/")
}

/// The regex fallback of `_extract_imports_for_file`.
fn imports_regex(file_path: &str, content: &str) -> String {
    static JS: [&str; 3] = [
        r#"^import\s+.*\s+from\s+['"]([^'"]+)['"]"#,
        r#"^const\s+.*\s+=\s+require\(['"]([^'"]+)['"]\)"#,
        r#"^import\s+['"]([^'"]+)['"]"#,
    ];
    static PATTERNS: LazyLock<Vec<(&'static str, Vec<PyRe>)>> = LazyLock::new(|| {
        let compile = |patterns: &[&str]| -> Vec<PyRe> {
            patterns
                .iter()
                .filter_map(|p| PyRe::new(p, Flags::NONE).ok())
                .collect()
        };
        vec![
            (
                ".py",
                compile(&[
                    r"^from\s+([^\s]+)\s+import\s+(.+)$",
                    r"^import\s+([^\s,]+)(?:\s+as\s+[^\s,]+)?(?:\s*,\s*[^\s,]+(?:\s+as\s+[^\s,]+)?)*$",
                ]),
            ),
            (".js", compile(&JS)),
            (".jsx", compile(&JS)),
            (".ts", compile(&JS)),
            (".tsx", compile(&JS)),
            (
                ".java",
                compile(&[
                    r"^import\s+([^\s;]+);?$",
                    r"^import\s+static\s+([^\s;]+);?$",
                ]),
            ),
        ]
    });
    let ext = pystr::suffix(file_path).to_lowercase();
    let Some((_, patterns)) = PATTERNS.iter().find(|(e, _)| *e == ext) else {
        return "Import patterns not supported for this file type.".to_owned();
    };
    let mut imports = Vec::new();
    for line in content.split('\n') {
        let line = pystr::strip(line);
        if line.is_empty() {
            continue;
        }
        if patterns
            .iter()
            .any(|p| p.match_start(line).ok().flatten().is_some())
        {
            imports.push(line);
        }
    }
    if imports.is_empty() {
        return "No imports found in this file.".to_owned();
    }
    imports.join("\n")
}

/// Resolves the Pylon plugin name of a file (`_resolve_pylon_plugin_name`):
/// the `plugins/<name>/` path segment, else the repository's
/// `metadata.json` `name`, read once.
#[derive(Debug, Default)]
pub struct PylonPlugin {
    repo_root: Option<PathBuf>,
    cached: Mutex<Option<String>>,
}

impl PylonPlugin {
    #[must_use]
    pub fn new(repo_root: Option<PathBuf>) -> Self {
        Self {
            repo_root,
            cached: Mutex::new(None),
        }
    }

    fn name_for(&self, rel_path: &str) -> String {
        static SEGMENT: LazyLock<Option<PyRe>> =
            LazyLock::new(|| PyRe::new(r"(?:^|/)plugins/([^/]+)/", Flags::NONE).ok());
        if !rel_path.is_empty()
            && let Some(m) = SEGMENT
                .as_ref()
                .and_then(|re| re.search(rel_path).ok().flatten())
        {
            return m.group(1).to_owned();
        }
        let Ok(mut cached) = self.cached.lock() else {
            return String::new();
        };
        if let Some(name) = cached.as_ref() {
            return name.clone();
        }
        let name = self
            .repo_root
            .as_deref()
            .and_then(|root| super::repo_files::read_contained(root, Path::new("metadata.json")))
            .map(|bytes| {
                plugin_name_from_metadata_text(&pystr::decode_text(&bytes, pystr::Errors::Ignore))
            })
            .unwrap_or_default();
        *cached = Some(name.clone());
        name
    }

    /// `_pylon_endpoints_for_file`.
    fn endpoints(&self, file_path: &str, file_docs: &[&ContextDoc], combined: &str) -> Vec<String> {
        let rel_path = file_docs
            .iter()
            .map(|d| d.rel_path.as_str())
            .find(|r| !r.is_empty())
            .unwrap_or(file_path);
        let plugin = self.name_for(rel_path);
        derive_pylon_endpoints(rel_path, combined, &plugin)
    }
}

/// `_format_simple_context`.
#[must_use]
#[allow(clippy::too_many_lines)] // one Python function, kept whole
pub fn format_simple_context(
    docs: &[ContextDoc],
    page: &PageSpec,
    facts: &GraphFacts,
    pylon: &PylonPlugin,
) -> RelevantContent {
    let mut by_file: Vec<(&str, Vec<&ContextDoc>)> = Vec::new();
    let mut documentation: Vec<&ContextDoc> = Vec::new();
    for doc in docs {
        if is_documentation_file(&doc.source) {
            documentation.push(doc);
        } else {
            match by_file.iter_mut().find(|(f, _)| *f == doc.source) {
                Some(entry) => entry.1.push(doc),
                None => by_file.push((doc.source.as_str(), vec![doc])),
            }
        }
    }
    let mut prioritized: Vec<&str> = Vec::new();
    let mut others: Vec<&str> = Vec::new();
    for (file, _) in &by_file {
        let hit = |patterns: &[String]| {
            patterns
                .iter()
                .any(|p| !p.is_empty() && file.contains(p.as_str()))
        };
        if hit(&page.target_folders) || hit(&page.key_files) {
            prioritized.push(file);
        } else {
            others.push(file);
        }
    }
    let mut parts: Vec<String> = Vec::new();
    if !documentation.is_empty() {
        parts.push("Documentation Context:".to_owned());
        for doc in &documentation {
            if doc.start_line > 0 && doc.end_line > 0 {
                parts.push(format!(
                    "<document_source: {}:L{}-L{}>",
                    doc.source, doc.start_line, doc.end_line
                ));
            } else {
                parts.push(format!("<document_source: {}>", doc.source));
            }
            parts.push(doc.content.clone());
            parts.push("</document_source>".to_owned());
        }
        parts.push(String::new());
    }
    if !by_file.is_empty() {
        parts.push("Code Context:".to_owned());
        for file in prioritized.iter().chain(&others) {
            let Some((_, file_docs)) = by_file.iter().find(|(f, _)| f == file) else {
                continue;
            };
            let combined = file_docs
                .iter()
                .map(|d| d.content.as_str())
                .collect::<Vec<_>>()
                .join(SEP);
            let imports = match facts.imports.get(*file) {
                Some(found) if !found.is_empty() => found.clone(),
                _ => imports_regex(file, &combined),
            };
            parts.push(format!("<code_source: {file}>"));
            let endpoints = pylon.endpoints(file, file_docs, &combined);
            if !endpoints.is_empty() {
                parts.push("<api_endpoints>".to_owned());
                for endpoint in endpoints {
                    parts.push(format!("  [ENDPOINT] {endpoint}"));
                }
                parts.push("</api_endpoints>".to_owned());
            }
            let annotations: Vec<String> = file_docs
                .iter()
                .filter(|d| d.start_line != 0 && d.end_line != 0 && !d.symbol_name.is_empty())
                .map(|d| {
                    format!(
                        "  [SYMBOL] {}: L{}-L{}",
                        d.symbol_name, d.start_line, d.end_line
                    )
                })
                .collect();
            if !annotations.is_empty() {
                parts.push("<line_map>".to_owned());
                parts.extend(annotations);
                parts.push("</line_map>".to_owned());
            }
            if !imports.is_empty() {
                parts.push("<imports>".to_owned());
                parts.push(imports);
                parts.push("</imports>".to_owned());
            }
            parts.push("<implementation>".to_owned());
            parts.push(combined);
            parts.push("</implementation>".to_owned());
            parts.push("</code_source>".to_owned());
            parts.push(String::new());
        }
    }
    RelevantContent {
        content: parts.join(SEP),
        files: prioritized
            .into_iter()
            .chain(others)
            .map(str::to_owned)
            .collect(),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn documentation_files_follow_the_python_rules() {
        assert!(is_documentation_file("docs/guide.MD"));
        assert!(is_documentation_file("a/Makefile"));
        assert!(is_documentation_file("README"));
        assert!(is_documentation_file("x/docs/y.py"));
        assert!(!is_documentation_file("src/main.py"));
        assert!(!is_documentation_file(""));
    }

    #[test]
    fn regex_imports_by_extension() {
        assert_eq!(
            imports_regex("a.py", "import os\nfrom x import (y)\nz = 1"),
            "import os\nfrom x import (y)"
        );
        assert_eq!(
            imports_regex("a.java", "class A {}"),
            "No imports found in this file."
        );
        assert_eq!(
            imports_regex("a.cs", "using X;"),
            "Import patterns not supported for this file type."
        );
    }
}

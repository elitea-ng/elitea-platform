//! File discovery (`_discover_files_by_language`).
//!
//! Every regular file under the repository is classified by extension, then
//! by well-known file name, into a source language, `documentation`, or
//! `unknown`.
//!
//! Two deliberate differences from the Python engine (ADR-0026):
//!
//! * The exclude patterns are matched against the REPOSITORY-RELATIVE path
//!   with a leading `/`, not the absolute path. Python matches
//!   `**/build/**` against the absolute path, so a clone that happens to sit
//!   under a directory named `build`, `target` or `dist` loses every file.
//! * Symbolic links are never followed (a link to a file or a directory is
//!   skipped and counted). Python's `rglob` + `is_file` follows file links,
//!   which can read outside the clone.
//!
//! The lists are sorted, which is what the parity reference does to the
//! Python engine's (otherwise filesystem-ordered) result.

use elitea_engine_core::pystr;
use std::collections::BTreeMap;
use std::path::Path;

/// `EnhancedUnifiedGraphBuilder.SUPPORTED_LANGUAGES`: extension → language.
pub const SUPPORTED_LANGUAGES: &[(&str, &str)] = &[
    (".py", "python"),
    (".js", "javascript"),
    (".ts", "typescript"),
    (".jsx", "javascript"),
    (".tsx", "typescript"),
    (".java", "java"),
    (".go", "go"),
    (".cs", "csharp"),
    (".cpp", "cpp"),
    (".cc", "cpp"),
    (".cxx", "cpp"),
    (".c++", "cpp"),
    (".h", "cpp"),
    (".hpp", "cpp"),
    (".hh", "cpp"),
    (".hxx", "cpp"),
    (".c", "c"),
    (".rb", "ruby"),
    (".php", "php"),
    (".rs", "rust"),
    (".kt", "kotlin"),
    (".scala", "scala"),
    (".sql", "sql"),
    (".ddl", "sql"),
];

/// `constants.DOCUMENTATION_EXTENSIONS`: extension → document type.
pub const DOCUMENTATION_EXTENSIONS: &[(&str, &str)] = &[
    (".md", "markdown"),
    (".rst", "restructuredtext"),
    (".txt", "plaintext"),
    (".adoc", "asciidoc"),
    (".doc", "document"),
    (".docx", "document"),
    (".pdf", "pdf"),
    (".html", "html"),
    (".htm", "html"),
    (".xml", "xml"),
    (".json", "json"),
    (".yaml", "yaml"),
    (".yml", "yaml"),
    (".toml", "toml"),
    (".ini", "config"),
    (".cfg", "config"),
    (".conf", "config"),
    (".properties", "config"),
    (".env", "config"),
    (".gradle", "build_config"),
    (".kts", "build_config"),
    (".wsdl", "schema"),
    (".xsd", "schema"),
    (".proto", "schema"),
    (".tf", "infrastructure"),
    (".tfvars", "infrastructure"),
    (".hcl", "infrastructure"),
    (".mod", "config"),
    (".sh", "script"),
    (".bash", "script"),
    (".bat", "script"),
    (".cmd", "script"),
    (".ps1", "script"),
    (".psm1", "script"),
];

/// `constants.KNOWN_FILENAMES`: exact file name → document type, for files
/// the extension does not classify.
pub const KNOWN_FILENAMES: &[(&str, &str)] = &[
    ("Makefile", "build_config"),
    ("makefile", "build_config"),
    ("GNUmakefile", "build_config"),
    ("CMakeLists.txt", "build_config"),
    ("Justfile", "build_config"),
    ("justfile", "build_config"),
    ("Taskfile", "build_config"),
    ("Earthfile", "build_config"),
    ("Snakefile", "build_config"),
    ("SConstruct", "build_config"),
    ("SConscript", "build_config"),
    ("Rakefile", "build_config"),
    ("BUILD", "build_config"),
    ("BUILD.bazel", "build_config"),
    ("WORKSPACE", "build_config"),
    ("WORKSPACE.bazel", "build_config"),
    ("Tiltfile", "build_config"),
    ("Dockerfile", "infrastructure"),
    ("Containerfile", "infrastructure"),
    ("Vagrantfile", "infrastructure"),
    ("Procfile", "infrastructure"),
    ("Gemfile", "config"),
    ("Brewfile", "config"),
    ("Pipfile", "config"),
    ("Jenkinsfile", "script"),
    ("Doxyfile", "config"),
    ("LICENSE", "plaintext"),
    ("CHANGELOG", "plaintext"),
    ("CONTRIBUTING", "plaintext"),
    ("AUTHORS", "plaintext"),
    ("CODEOWNERS", "config"),
    (".editorconfig", "config"),
    (".gitattributes", "config"),
    (".gitignore", "config"),
    (".dockerignore", "config"),
    (".npmignore", "config"),
    (".eslintignore", "config"),
    (".prettierignore", "config"),
    (".env", "config"),
    (".env.example", "config"),
    (".env.local", "config"),
    (".env.development", "config"),
    (".env.production", "config"),
];

/// `constants.DEFAULT_EXCLUDE_PATTERNS` (fnmatch: `*` crosses `/`).
pub const DEFAULT_EXCLUDE_PATTERNS: &[&str] = &[
    "**/node_modules/**",
    "**/build/**",
    "**/dist/**",
    "**/target/**",
    "**/.git/**",
    "**/.svn/**",
    "**/.hg/**",
    "**/__pycache__/**",
    "**/.pytest_cache/**",
    "**/.mypy_cache/**",
    "**/.tox/**",
    "**/.eggs/**",
    "**/.vscode/**",
    "**/.idea/**",
    "**/.vs/**",
    "**/.venv/**",
    "**/.coverage/**",
    "**/.cache/**",
    "**/*.class",
    "**/*.jar",
    "**/*.war",
];

fn lookup(table: &[(&str, &'static str)], key: &str) -> Option<&'static str> {
    table.iter().find(|(k, _)| *k == key).map(|(_, v)| *v)
}

/// The source language of an extension (lower-cased by the caller).
#[must_use]
pub fn language_for_extension(extension: &str) -> Option<&'static str> {
    lookup(SUPPORTED_LANGUAGES, extension)
}

/// The document type of a file: by extension, else by known file name.
#[must_use]
pub fn doc_type_for(path: &str) -> Option<&'static str> {
    lookup(
        DOCUMENTATION_EXTENSIONS,
        &pystr::suffix(path).to_lowercase(),
    )
    .or_else(|| lookup(KNOWN_FILENAMES, pystr::file_name(path)))
}

/// What discovery found.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct Discovery {
    /// Language (or `documentation` / `unknown`) → sorted absolute paths.
    pub files_by_language: BTreeMap<String, Vec<String>>,
    /// Symbolic links skipped (never followed).
    pub symlinks_skipped: usize,
    /// Entries skipped because their name is not UTF-8 or they could not be
    /// listed.
    pub unreadable_skipped: usize,
}

impl Discovery {
    /// The files of one language (empty when there are none).
    #[must_use]
    pub fn files(&self, language: &str) -> &[String] {
        self.files_by_language
            .get(language)
            .map_or(&[], Vec::as_slice)
    }
}

/// Whether `/rel` matches an exclude pattern.
#[must_use]
pub fn is_excluded(rooted_rel_path: &str, patterns: &[&str]) -> bool {
    patterns.iter().any(|p| pystr::fnmatch(rooted_rel_path, p))
}

/// Whether every file under directory `/rel/` is excluded, so the walk can
/// skip it. Only a pattern ending in `*` qualifies: if it matches `/rel/`,
/// its trailing `*` also absorbs any longer path below.
fn prunes_directory(rooted_rel_dir: &str, patterns: &[&str]) -> bool {
    let probe = format!("{rooted_rel_dir}/");
    patterns
        .iter()
        .any(|p| p.ends_with('*') && pystr::fnmatch(&probe, p))
}

/// Classify one file the way `_discover_files_by_language` does.
#[must_use]
pub fn classify(path: &str) -> &'static str {
    let extension = pystr::suffix(path).to_lowercase();
    if let Some(language) = language_for_extension(&extension) {
        return language;
    }
    if doc_type_for(path).is_some() {
        "documentation"
    } else {
        "unknown"
    }
}

/// Discover the repository's files with the default exclude patterns.
///
/// `repo_root` is the absolute repository path as the caller wants it in
/// the returned paths (`<repo_root>/<rel>`); it is not canonicalised here.
#[must_use]
pub fn discover_files(repo_root: &str) -> Discovery {
    discover_files_with(repo_root, DEFAULT_EXCLUDE_PATTERNS)
}

/// [`discover_files`] with explicit exclude patterns.
#[must_use]
pub fn discover_files_with(repo_root: &str, patterns: &[&str]) -> Discovery {
    let mut discovery = Discovery::default();
    let root = repo_root.trim_end_matches('/');
    walk(Path::new(root), root, "", patterns, &mut discovery);
    for files in discovery.files_by_language.values_mut() {
        files.sort();
    }
    discovery
}

fn walk(dir: &Path, root: &str, rel_dir: &str, patterns: &[&str], out: &mut Discovery) {
    let Ok(entries) = std::fs::read_dir(dir) else {
        out.unreadable_skipped += 1;
        return;
    };
    for entry in entries {
        let Ok(entry) = entry else {
            out.unreadable_skipped += 1;
            continue;
        };
        let Ok(file_type) = entry.file_type() else {
            out.unreadable_skipped += 1;
            continue;
        };
        let Some(name) = entry.file_name().to_str().map(str::to_owned) else {
            out.unreadable_skipped += 1;
            continue;
        };
        let rel = if rel_dir.is_empty() {
            name
        } else {
            format!("{rel_dir}/{name}")
        };
        if file_type.is_symlink() {
            out.symlinks_skipped += 1;
        } else if file_type.is_dir() {
            if !prunes_directory(&format!("/{rel}"), patterns) {
                walk(&entry.path(), root, &rel, patterns, out);
            }
        } else if file_type.is_file() && !is_excluded(&format!("/{rel}"), patterns) {
            let absolute = format!("{root}/{rel}");
            out.files_by_language
                .entry(classify(&absolute).to_owned())
                .or_default()
                .push(absolute);
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn scratch(name: &str) -> std::path::PathBuf {
        let dir = std::env::temp_dir().join(format!("dw-discover-{name}-{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&dir);
        std::fs::create_dir_all(&dir).unwrap();
        dir
    }

    fn touch(root: &Path, rel: &str) {
        let path = root.join(rel);
        std::fs::create_dir_all(path.parent().unwrap()).unwrap();
        std::fs::write(path, b"x").unwrap();
    }

    #[test]
    fn classifies_by_extension_then_known_name() {
        assert_eq!(classify("/r/a/B.PY"), "python");
        assert_eq!(classify("/r/Makefile"), "documentation");
        // `.txt` wins over the known name `CMakeLists.txt`; both are docs.
        assert_eq!(doc_type_for("/r/CMakeLists.txt"), Some("plaintext"));
        assert_eq!(doc_type_for("/r/.env"), Some("config"));
        assert_eq!(classify("/r/x.sql"), "sql");
        assert_eq!(classify("/r/image.png"), "unknown");
    }

    #[test]
    fn excludes_match_the_rooted_relative_path() {
        // The clone itself lives under a `build/` directory: Python would
        // exclude every file; matching the relative path keeps them.
        let root = scratch("build").join("build").join("repo");
        touch(&root, "src/main.go");
        touch(&root, "web/node_modules/x/index.js");
        touch(&root, "out/build/gen.go");
        touch(&root, "lib/Foo.class");
        touch(&root, ".git/config");
        touch(&root, "README.md");
        let root_str = root.to_str().unwrap();
        let found = discover_files(root_str);
        assert_eq!(found.files("go"), [format!("{root_str}/src/main.go")]);
        assert!(found.files("javascript").is_empty());
        assert_eq!(
            found.files("documentation"),
            [format!("{root_str}/README.md")]
        );
        assert_eq!(found.files("unknown"), Vec::<String>::new());
        // A top-level excluded directory matches too (`/target/...`).
        assert!(is_excluded("/target/debug/x.rs", DEFAULT_EXCLUDE_PATTERNS));
        assert!(!is_excluded("/targets/x.rs", DEFAULT_EXCLUDE_PATTERNS));
    }

    #[cfg(unix)]
    #[test]
    fn symlinks_are_skipped_and_counted() {
        let root = scratch("links");
        touch(&root, "a/real.py");
        std::os::unix::fs::symlink(root.join("a/real.py"), root.join("link.py")).unwrap();
        std::os::unix::fs::symlink(root.join("a"), root.join("dirlink")).unwrap();
        let found = discover_files(root.to_str().unwrap());
        assert_eq!(found.files("python").len(), 1);
        assert_eq!(found.symlinks_skipped, 2);
    }

    #[test]
    fn output_is_sorted() {
        let root = scratch("sorted");
        for rel in ["b.py", "a/z.py", "a.py", "Zed.py"] {
            touch(&root, rel);
        }
        let root_str = root.to_str().unwrap();
        let found = discover_files(root_str);
        let rels: Vec<&str> = found
            .files("python")
            .iter()
            .map(|p| &p[root_str.len() + 1..])
            .collect();
        assert_eq!(rels, ["Zed.py", "a.py", "a/z.py", "b.py"]);
    }
}

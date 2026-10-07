//! The repository analysis node: `analyze_repository` →
//! `_llm_analyze_repository` in `agents/wiki_graph_optimized.py`.
//!
//! One model call. Its inputs are built from the clone and the documents
//! the graph builder chunked:
//!
//! * the TREE (`_create_repository_tree`) from the agent's file list
//!   ([`super::files`]): unique sorted paths, root files first, then every
//!   directory with up to 25 file names;
//! * the README (`_extract_full_readme_content_from_docs`): the live
//!   indexer has no traverser, so the file-based reader always answers "No
//!   README file found" and the DOCUMENTS are read instead — the chunks of
//!   the first documentation file whose repository-relative path contains
//!   `readme` (any case, anywhere in the path), joined by the four
//!   characters `\n\n` (a backslash-n quirk of the Python source), each
//!   chunk with its `[File: <path>]` header;
//! * the CODE SAMPLES (`_extract_representative_code_samples_from_files`):
//!   the first 800 characters of the first three `.py` files, 400 of the
//!   first two config files, 600 of the first two `.md` files that are not
//!   READMEs (the slice is taken before the README test, so a README among
//!   the first two leaves one), at most 4,000 characters in all;
//! * the FILE STATISTICS (`_prepare_basic_file_stats`): the count per
//!   extension, highest first (ties in walk order), the first 50, joined
//!   by the two characters `\n`.
//!
//! `analyze_repository`'s fallbacks over the graph builder's DOCUMENTS
//! ([`IndexerDocuments`]: every architectural symbol, a code symbol only at
//! top level, as `_generate_symbol_chunks` streamed them): no document at
//! all fails with [`NO_DOCUMENTS`]; an empty walk takes the documents'
//! paths as the file list; no sample from the files takes the documents'
//! samples ([`code_samples_from_documents`]).
//!
//! The answer is the `repository_context` every later step reads (page
//! generation, ask, deep research) and the `auto` planner measures.
//!
//! Deliberate differences: the README order follows the graph builder's
//! documents, which come language by language; a CODE file whose path
//! contains `readme` (in a language sorted before `documentation`) would
//! precede the documentation files in Python and is not considered here.
//! Python sorted a README's chunks by `section_id` and raised `TypeError`
//! when a large README was cut into generic chunks (they carry none), which
//! failed the analysis and with it the structure; the chunks keep their
//! order here.

use super::files;
use super::model::{ChatModel, user_request};
use super::prompts;
use crate::errors::{EngineError, ErrorType};
use crate::graph::constants;
use crate::graph::discover::Discovery;
use crate::graph::documents::{self, DocResult};
use crate::graph::helpers::is_package_or_namespace_parent;
use crate::llm::ChatMessage;
use crate::parsers::model::ParseResult;
use indexmap::IndexMap;
use std::fmt::Write as _;
use std::path::Path;
use std::sync::OnceLock;

/// `_create_repository_tree`'s per-directory listing cap.
pub const MAX_FILES_PER_DIR: usize = 25;

/// The marker Python returns when no README document exists.
pub const NO_README: &str = "No README file found";

/// The marker for no code samples.
pub const NO_SAMPLES: &str = "No representative code samples available";

/// What the analysis node puts in the graph state.
#[derive(Debug, Clone, PartialEq)]
pub struct RepositoryAnalysis {
    /// The model's analysis (`repository_context`).
    pub repository_context: String,
    pub repository_tree: String,
    pub readme_content: String,
    /// The structured summary (`RepositoryAnalysis`), used by the classic
    /// planner only when the model's analysis is empty.
    pub summary: AnalysisSummary,
    /// The agent's file list, in walk order.
    pub files: Vec<String>,
}

/// The analysis inputs, before the model call.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct AnalysisInputs {
    pub files: Vec<String>,
    pub repository_tree: String,
    pub readme_content: String,
    pub code_samples: String,
    pub file_stats: String,
}

/// `analyze_repository`'s error when the indexer holds no document.
pub const NO_DOCUMENTS: &str = "No documents found in indexer - ensure repository is indexed first";

/// Build the inputs of the analysis prompt, with `analyze_repository`'s
/// fallbacks:
///
/// * no indexer document at all: `ValueError` [`NO_DOCUMENTS`];
/// * an empty file walk: the file list is the documents' `source` paths
///   (Python: `list(set(…))`, hash-seed order; sorted here, which only
///   the ties of the file statistics see);
/// * no code sample from the files: the samples of the documents
///   (`_extract_representative_code_samples`).
///
/// The documents are built only as far as a step needs them (see
/// [`IndexerDocuments`]).
///
/// # Errors
///
/// [`NO_DOCUMENTS`] as `ValueError`.
pub fn analysis_inputs(
    repo_root: &Path,
    discovery: &Discovery,
) -> Result<AnalysisInputs, EngineError> {
    let root = repo_root.to_string_lossy();
    let documents = IndexerDocuments::new(discovery, &root);
    if documents.is_empty() {
        return Err(EngineError::new(ErrorType::Value, NO_DOCUMENTS));
    }
    let mut files = files::repository_files(repo_root);
    if files.is_empty() {
        let mut sources: Vec<String> = documents
            .all()
            .iter()
            .filter(|d| !d.source.is_empty())
            .map(|d| d.source.clone())
            .collect();
        sources.sort_unstable();
        sources.dedup();
        files = sources;
    }
    let repository_tree = repository_tree(&files, MAX_FILES_PER_DIR);
    let readme_content = readme_from_documents(discovery, repo_root);
    let mut code_samples = code_samples(repo_root, &files);
    if code_samples.is_empty() || code_samples == NO_SAMPLES {
        code_samples = code_samples_from_documents(documents.all());
    }
    let file_stats = file_stats(&files);
    Ok(AnalysisInputs {
        files,
        repository_tree,
        readme_content,
        code_samples,
        file_stats,
    })
}

/// One document of `indexer.get_all_documents()`: its `source` metadata
/// (the repository-relative path) and its `page_content`.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct IndexerDocument {
    pub source: String,
    pub content: String,
}

/// `_is_architectural_symbol`: `ARCHITECTURAL_SYMBOLS | DOC_SYMBOL_TYPES`.
fn architectural(symbol_type: &str) -> bool {
    let symbol_type = symbol_type.to_lowercase();
    constants::ARCHITECTURAL_SYMBOLS.contains(&symbol_type.as_str())
        || constants::DOC_SYMBOL_TYPES.contains(&symbol_type.as_str())
}

/// One language's parse, in discovery order.
enum Parsed {
    Documentation(Vec<DocResult>),
    Code(Vec<(String, ParseResult)>),
}

/// The graph builder's documents (`_generate_symbol_chunks` /
/// `_iter_symbol_chunks`, `DEEPWIKI_DOC_SEPARATE_INDEX` off): language by
/// language, file by file, every symbol whose type is architectural; a
/// code symbol only at top level (`_is_package_or_namespace_parent`).
/// Parsed with this engine's parsers on first need, at most once.
pub struct IndexerDocuments<'d> {
    discovery: &'d Discovery,
    root: &'d str,
    parsed: OnceLock<Vec<Parsed>>,
    documents: OnceLock<Vec<IndexerDocument>>,
}

impl<'d> IndexerDocuments<'d> {
    #[must_use]
    pub fn new(discovery: &'d Discovery, root: &'d str) -> Self {
        Self {
            discovery,
            root,
            parsed: OnceLock::new(),
            documents: OnceLock::new(),
        }
    }

    fn parse_language(&self, language: &str, files: &[String]) -> Option<Parsed> {
        match language {
            "documentation" => Some(Parsed::Documentation(documents::parse_documentation_files(
                files, self.root,
            ))),
            "sql" | "unknown" => None,
            _ => crate::parsers::parser_for(language)
                .map(|parser| Parsed::Code(parser.parse_files(files).into_iter().collect())),
        }
    }

    fn parsed(&self) -> &[Parsed] {
        self.parsed.get_or_init(|| {
            self.discovery
                .files_by_language
                .iter()
                .filter_map(|(language, files)| self.parse_language(language, files))
                .collect()
        })
    }

    /// Whether there is no document, parsing only as far as the answer
    /// needs: a documentation chunk (the documentation files are tried
    /// first, file by file) or a top-level code symbol settles it; a
    /// parent that only the other files can classify needs the whole
    /// parse.
    #[must_use]
    pub fn is_empty(&self) -> bool {
        if let Some(documents) = self.documents.get() {
            return documents.is_empty();
        }
        if self.parsed.get().is_none() {
            let (docs, code): (Vec<_>, Vec<_>) = self
                .discovery
                .files_by_language
                .iter()
                .partition(|(language, _)| *language == "documentation");
            for (language, files) in docs.into_iter().chain(code) {
                let settled = match language.as_str() {
                    "documentation" => files.iter().any(|file| {
                        documents::parse_documentation_files(std::slice::from_ref(file), self.root)
                            .iter()
                            .any(|r| r.symbols.iter().any(|s| architectural(&s.symbol_type)))
                    }),
                    "sql" | "unknown" => false,
                    _ => crate::parsers::parser_for(language).is_some_and(|parser| {
                        parser.parse_files(files).values().any(|result| {
                            result.symbols.iter().any(|s| {
                                architectural(s.symbol_type.as_str())
                                    && is_package_or_namespace_parent(
                                        s.parent_symbol.as_deref(),
                                        [],
                                    )
                            })
                        })
                    }),
                };
                if settled {
                    return false;
                }
            }
        }
        self.all().is_empty()
    }

    /// Every document, in the graph builder's order.
    #[must_use]
    pub fn all(&self) -> &[IndexerDocument] {
        self.documents.get_or_init(|| {
            let parsed = self.parsed();
            let code: Vec<&ParseResult> = parsed
                .iter()
                .filter_map(|p| match p {
                    Parsed::Code(results) => Some(results.iter().map(|(_, r)| r)),
                    Parsed::Documentation(_) => None,
                })
                .flatten()
                .collect();
            let mut out = Vec::new();
            for language in parsed {
                match language {
                    Parsed::Documentation(results) => {
                        for result in results {
                            for symbol in &result.symbols {
                                if !architectural(&symbol.symbol_type) {
                                    continue;
                                }
                                let source = if symbol.rel_path.is_empty() {
                                    documents::relative_path(&result.file_path, self.root)
                                } else {
                                    symbol.rel_path.as_str()
                                };
                                out.push(IndexerDocument {
                                    source: source.to_owned(),
                                    content: symbol.source_text.clone(),
                                });
                            }
                        }
                    }
                    Parsed::Code(results) => {
                        for (path, result) in results {
                            let source = documents::relative_path(path, self.root);
                            for symbol in &result.symbols {
                                if !architectural(symbol.symbol_type.as_str())
                                    || !is_package_or_namespace_parent(
                                        symbol.parent_symbol.as_deref(),
                                        code.iter().copied(),
                                    )
                                {
                                    continue;
                                }
                                out.push(IndexerDocument {
                                    source: source.to_owned(),
                                    content: symbol.source_text.clone().unwrap_or_default(),
                                });
                            }
                        }
                    }
                }
            }
            out
        })
    }
}

/// `_extract_representative_code_samples(all_documents)`: the first three
/// documents whose `source` ends with `.py` (800 characters each), the
/// first two whose lower-cased `source` CONTAINS one of the config words
/// (a substring test: `cmd/init.go` holds `ini`; 400 each), the first two
/// ending with `.md` that are not READMEs (the slice is taken first; 600
/// each), joined by the two characters `\n`, at most 4,000 characters.
// Python's `endswith` is case-sensitive here (`.PY` is not a sample).
#[allow(clippy::case_sensitive_file_extension_comparisons)]
#[must_use]
pub fn code_samples_from_documents(documents: &[IndexerDocument]) -> String {
    const CONFIG_WORDS: [&str; 9] = [
        "yaml", "json", "toml", "ini", "proto", "tf", "gradle", "wsdl", "xsd",
    ];
    let python = documents.iter().filter(|d| d.source.ends_with(".py"));
    let config = documents.iter().filter(|d| {
        let low = d.source.to_lowercase();
        CONFIG_WORDS.iter().any(|word| low.contains(word))
    });
    let docs = documents.iter().filter(|d| d.source.ends_with(".md"));
    let sample = |d: &IndexerDocument, max_chars: usize| {
        format!(
            "=== {} ===\\n{}\\n",
            d.source,
            crate::graph::pystr::prefix_chars(&d.content, max_chars)
        )
    };
    let mut samples: Vec<String> = Vec::new();
    samples.extend(python.take(3).map(|d| sample(d, 800)));
    samples.extend(config.take(2).map(|d| sample(d, 400)));
    samples.extend(
        docs.take(2)
            .filter(|d| !d.source.to_lowercase().contains("readme"))
            .map(|d| sample(d, 600)),
    );
    let mut combined = samples.join("\\n");
    if combined.chars().count() > 4000 {
        combined = format!(
            "{}\\n... [truncated for context length]",
            crate::graph::pystr::prefix_chars(&combined, 4000)
        );
    }
    if combined.is_empty() {
        NO_SAMPLES.to_owned()
    } else {
        combined
    }
}

/// `_create_repository_tree`.
#[must_use]
pub fn repository_tree(file_paths: &[String], max_files_per_dir: usize) -> String {
    let mut unique: Vec<&str> = file_paths
        .iter()
        .map(String::as_str)
        .filter(|p| !p.is_empty())
        .collect();
    if unique.is_empty() {
        return "No files found".to_owned();
    }
    unique.sort_unstable();
    unique.dedup();
    let mut dirs: IndexMap<&str, Vec<&str>> = IndexMap::new();
    let mut root_files = Vec::new();
    for path in &unique {
        match path.rsplit_once('/') {
            Some((dir, name)) => dirs.entry(dir).or_default().push(name),
            None => root_files.push(*path),
        }
    }
    let mut lines = vec![
        format!("Total files: {}", unique.len()),
        format!("Showing up to {max_files_per_dir} files per directory"),
    ];
    if !root_files.is_empty() {
        root_files.sort_unstable();
        lines.push("\u{1f4c1} /".to_owned());
        for name in root_files.iter().take(max_files_per_dir) {
            lines.push(format!("  \u{1f4c4} {name}"));
        }
        if root_files.len() > max_files_per_dir {
            lines.push(format!(
                "  ... (+{} more)",
                root_files.len() - max_files_per_dir
            ));
        }
    }
    dirs.sort_unstable_keys();
    for (dir, mut names) in dirs {
        names.sort_unstable();
        lines.push(format!("\u{1f4c1} {dir}/ ({} files)", names.len()));
        for name in names.iter().take(max_files_per_dir) {
            lines.push(format!("  \u{1f4c4} {name}"));
        }
        if names.len() > max_files_per_dir {
            lines.push(format!("  ... (+{} more)", names.len() - max_files_per_dir));
        }
    }
    lines.join("\n")
}

/// `_extract_full_readme_content_from_docs` over the graph builder's
/// documentation files (see the module docs).
#[must_use]
pub fn readme_from_documents(discovery: &Discovery, repo_root: &Path) -> String {
    let Some(root) = repo_root.to_str() else {
        return NO_README.to_owned();
    };
    for path in discovery.files("documentation") {
        let rel = documents::relative_path(path, root);
        if !rel.to_lowercase().contains("readme") {
            continue;
        }
        let parsed = documents::parse_documentation_files(std::slice::from_ref(path), root);
        let Some(result) = parsed.first() else {
            continue;
        };
        if result.symbols.is_empty() {
            continue;
        }
        return result
            .symbols
            .iter()
            .map(|symbol| symbol.source_text.as_str())
            .collect::<Vec<_>>()
            .join("\\n\\n");
    }
    NO_README.to_owned()
}

/// The config-file extensions of the code samples.
const CONFIG_EXTENSIONS: &[&str] = &[
    ".yaml", ".yml", ".json", ".toml", ".ini", ".proto", ".tf", ".tfvars", ".gradle", ".kts",
    ".wsdl", ".xsd", ".hcl",
];

/// `_extract_representative_code_samples_from_files`.
// The paths are lower-cased before the suffix tests, as in Python.
#[allow(clippy::case_sensitive_file_extension_comparisons)]
#[must_use]
pub fn code_samples(repo_root: &Path, file_paths: &[String]) -> String {
    if file_paths.is_empty() {
        return NO_SAMPLES.to_owned();
    }
    let lower: Vec<(String, &str)> = file_paths
        .iter()
        .filter(|p| !p.is_empty())
        .map(|p| (p.to_lowercase(), p.as_str()))
        .collect();
    let pick = |test: &dyn Fn(&str) -> bool| -> Vec<&str> {
        let mut picked: Vec<&str> = lower
            .iter()
            .filter(|(low, _)| test(low))
            .map(|(_, path)| *path)
            .collect();
        picked.sort_unstable();
        picked
    };
    let python = pick(&|low| low.ends_with(".py"));
    let config = pick(&|low| CONFIG_EXTENSIONS.iter().any(|ext| low.ends_with(ext)));
    let docs = pick(&|low| low.ends_with(".md"));

    let mut samples = Vec::new();
    let mut push = |rel: &str, max_chars: usize| {
        let content = files::read_prefix(repo_root, rel, max_chars);
        if !content.is_empty() {
            samples.push(format!("=== {rel} ===\n{content}\n"));
        }
    };
    for rel in python.iter().take(3) {
        push(rel, 800);
    }
    for rel in config.iter().take(2) {
        push(rel, 400);
    }
    for rel in docs.iter().take(2) {
        if rel.to_lowercase().contains("readme") {
            continue;
        }
        push(rel, 600);
    }
    let mut combined = samples.join("\n");
    if combined.chars().count() > 4000 {
        combined = format!(
            "{}\n... [truncated for context length]",
            crate::graph::pystr::prefix_chars(&combined, 4000)
        );
    }
    if combined.is_empty() {
        NO_SAMPLES.to_owned()
    } else {
        combined
    }
}

/// `_prepare_basic_file_stats`.
#[must_use]
pub fn file_stats(file_paths: &[String]) -> String {
    let mut counts: IndexMap<String, usize> = IndexMap::new();
    for path in file_paths {
        if let Some((_, ext)) = path.rsplit_once('.') {
            *counts.entry(ext.to_lowercase()).or_default() += 1;
        }
    }
    let mut sorted: Vec<(String, usize)> = counts.into_iter().collect();
    // Stable: ties keep the walk order.
    sorted.sort_by_key(|entry| std::cmp::Reverse(entry.1));
    let mut lines = vec![
        format!("Total Files: {}", file_paths.len()),
        "File Type Distribution:".to_owned(),
    ];
    for (ext, count) in sorted.iter().take(50) {
        lines.push(format!("  - .{ext}: {count} files"));
    }
    lines.join("\\n")
}

/// The analysis request: the markdown prompt, or the JSON one under
/// `DEEPWIKI_USE_STRUCTURED_REPO_ANALYSIS=1`.
///
/// # Errors
///
/// Only a broken prompt template.
pub fn analysis_messages(
    inputs: &AnalysisInputs,
    repository_name: &str,
    branch: &str,
    structured: bool,
) -> Result<Vec<ChatMessage>, EngineError> {
    let (system, template) = if structured {
        (
            prompts::REPO_ANALYSIS_STRUCTURED_SYSTEM,
            prompts::REPO_ANALYSIS_STRUCTURED_USER,
        )
    } else {
        (prompts::REPO_ANALYSIS_SYSTEM, prompts::REPO_ANALYSIS_USER)
    };
    // `readme_content if readme_content else "No README found"`: the
    // README is never empty (the marker says "file"), so the second
    // spelling never reaches the prompt. Kept for the empty case.
    let readme = if inputs.readme_content.is_empty() {
        "No README found"
    } else {
        inputs.readme_content.as_str()
    };
    let user = prompts::format(
        template,
        &[
            ("repository_name", repository_name),
            ("branch_name", branch),
            ("repository_tree", &inputs.repository_tree),
            ("readme_content", readme),
            ("code_samples", &inputs.code_samples),
            ("file_stats", &inputs.file_stats),
        ],
    )
    .map_err(|e| super::template_error(&e))?;
    Ok(vec![ChatMessage::System(system), ChatMessage::User(user)])
}

/// Run the analysis node.
///
/// # Errors
///
/// [`NO_DOCUMENTS`], or the model call's failure. Python logged either and
/// went on without an analysis, after which structure planning refused
/// ("Repository analysis missing from state"); the error is returned
/// instead.
pub async fn analyze_repository(
    model: &impl ChatModel,
    repo_root: &Path,
    discovery: &Discovery,
    repository_name: &str,
    branch: &str,
    structured: bool,
) -> Result<RepositoryAnalysis, EngineError> {
    let inputs = analysis_inputs(repo_root, discovery)?;
    let messages = analysis_messages(&inputs, repository_name, branch, structured)?;
    let repository_context = model.complete(&user_request(messages)).await?;
    if structured && serde_json::from_str::<serde_json::Value>(&repository_context).is_err() {
        tracing::warn!("the structured repository analysis is not valid JSON");
    }
    let summary = AnalysisSummary::new(&inputs.files, &inputs.readme_content);
    Ok(RepositoryAnalysis {
        repository_context,
        repository_tree: inputs.repository_tree,
        readme_content: inputs.readme_content,
        summary,
        files: inputs.files,
    })
}

/// `RepositoryAnalysis` (`_create_repository_analysis`): a structured
/// summary of the file list. Only its `str()` is ever read — by the classic
/// planner, when the model returned an empty analysis.
///
/// Python built it from a `set` of paths, so its lists followed the hash
/// seed; here the unique paths are sorted. `total_symbols` is 0: the
/// agent's stand-in for the indexer stats is not ported.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct AnalysisSummary {
    pub total_files: usize,
    pub programming_languages: Vec<&'static str>,
    pub primary_language: String,
    pub has_readme: bool,
    pub readme_content: String,
    pub total_symbols: usize,
    pub file_types: IndexMap<String, usize>,
    pub project_complexity: &'static str,
    pub key_directories: Vec<String>,
    pub main_modules: Vec<String>,
    pub documentation_files: Vec<String>,
    pub configuration_files: Vec<String>,
    pub test_files: Vec<String>,
}

fn language_of(ext: &str) -> Option<&'static str> {
    Some(match ext {
        "py" => "Python",
        "js" => "JavaScript",
        "ts" => "TypeScript",
        "java" => "Java",
        "cpp" | "cc" | "cxx" | "c++" | "hpp" | "hh" | "hxx" => "C++",
        "c" => "C",
        "h" => "C/C++",
        "go" => "Go",
        "rs" => "Rust",
        "rb" => "Ruby",
        "php" => "PHP",
        "cs" => "C#",
        _ => return None,
    })
}

impl AnalysisSummary {
    #[must_use]
    pub fn new(file_paths: &[String], readme_content: &str) -> Self {
        let mut unique: Vec<&str> = file_paths
            .iter()
            .map(String::as_str)
            .filter(|p| !p.is_empty())
            .collect();
        unique.sort_unstable();
        unique.dedup();
        let mut file_types: IndexMap<String, usize> = IndexMap::new();
        for path in &unique {
            if let Some((_, ext)) = path.rsplit_once('.') {
                let ext = ext.to_lowercase();
                if !ext.is_empty() {
                    *file_types.entry(ext).or_default() += 1;
                }
            }
        }
        let languages: Vec<&'static str> = file_types
            .keys()
            .filter_map(|ext| language_of(ext))
            .collect();
        let readme_sample = if readme_content.is_empty() || readme_content == NO_README {
            String::new()
        } else {
            crate::graph::pystr::prefix_chars(readme_content, 1000).to_owned()
        };
        let mut complexity = "simple";
        if unique.len() > 50 || languages.len() > 2 {
            complexity = "moderate";
        }
        if unique.len() > 200 || languages.len() > 3 {
            complexity = "complex";
        }
        let ends_any = |path: &str, exts: &[&str]| {
            let low = path.to_lowercase();
            exts.iter().any(|ext| low.ends_with(ext))
        };
        Self {
            total_files: unique.len(),
            programming_languages: languages.iter().take(5).copied().collect(),
            primary_language: languages.first().map_or("Unknown", |l| l).to_owned(),
            has_readme: unique.iter().any(|p| p.to_lowercase().contains("readme")),
            readme_content: readme_sample,
            total_symbols: 0,
            file_types,
            project_complexity: complexity,
            key_directories: key_directories(&unique),
            main_modules: main_modules(&unique),
            documentation_files: unique
                .iter()
                .filter(|p| ends_any(p, &[".md", ".rst", ".txt"]))
                .map(|p| (*p).to_owned())
                .collect(),
            configuration_files: unique
                .iter()
                .filter(|p| ends_any(p, &[".json", ".yaml", ".yml", ".toml", ".ini", ".cfg"]))
                .map(|p| (*p).to_owned())
                .collect(),
            test_files: unique
                .iter()
                .filter(|p| {
                    let low = p.to_lowercase();
                    low.contains("test") || low.contains("spec")
                })
                .map(|p| (*p).to_owned())
                .collect(),
        }
    }

    /// pydantic's `str(model)`: `name=repr(value)` pairs joined by spaces.
    #[must_use]
    pub fn to_python_str(&self) -> String {
        let list = |items: &[String]| {
            format!(
                "[{}]",
                items
                    .iter()
                    .map(|s| py_repr(s))
                    .collect::<Vec<_>>()
                    .join(", ")
            )
        };
        let mut out = String::new();
        let _ = write!(
            out,
            "total_files={} programming_languages=[{}] primary_language={} has_readme={} \
             readme_content={} total_symbols={} file_types={{{}}} project_complexity={} \
             key_directories={} main_modules={} documentation_files={} configuration_files={} \
             test_files={}",
            self.total_files,
            self.programming_languages
                .iter()
                .map(|s| py_repr(s))
                .collect::<Vec<_>>()
                .join(", "),
            py_repr(&self.primary_language),
            if self.has_readme { "True" } else { "False" },
            py_repr(&self.readme_content),
            self.total_symbols,
            self.file_types
                .iter()
                .map(|(k, v)| format!("{}: {v}", py_repr(k)))
                .collect::<Vec<_>>()
                .join(", "),
            py_repr(self.project_complexity),
            list(&self.key_directories),
            list(&self.main_modules),
            list(&self.documentation_files),
            list(&self.configuration_files),
            list(&self.test_files),
        );
        out
    }
}

fn key_directories(paths: &[&str]) -> Vec<String> {
    let mut dirs: Vec<String> = Vec::new();
    for path in paths {
        let parts: Vec<&str> = path.split('/').collect();
        if parts.len() > 1 {
            dirs.push(parts[0].to_owned());
            if parts.len() > 2 {
                dirs.push(format!("{}/{}", parts[0], parts[1]));
            }
        }
    }
    dirs.sort_unstable();
    dirs.dedup();
    dirs.truncate(20);
    dirs
}

// Python's `endswith` is case-sensitive here (`.PY` is not a module).
#[allow(clippy::case_sensitive_file_extension_comparisons)]
fn main_modules(paths: &[&str]) -> Vec<String> {
    let mut modules: Vec<String> = Vec::new();
    for path in paths {
        if path.ends_with(".py") {
            let module = path.replace('/', ".").replace(".py", "");
            if !module.starts_with('.') && !module.contains("test") {
                modules.push(module);
            }
        } else if (path.ends_with(".js") || path.ends_with(".ts")) && path.contains('/') {
            let name = path.rsplit('/').next().unwrap_or(path);
            modules.push(name.split('.').next().unwrap_or(name).to_owned());
        }
    }
    modules.sort_unstable();
    modules.dedup();
    modules.truncate(50);
    modules
}

/// Python's `repr()` of a `str`: single quotes unless the text holds a
/// single quote and no double quote; `\\`, the quote, `\n`, `\r`, `\t`
/// and other non-printable characters escaped.
#[must_use]
pub fn py_repr(text: &str) -> String {
    let quote = if text.contains('\'') && !text.contains('"') {
        '"'
    } else {
        '\''
    };
    let mut out = String::with_capacity(text.len() + 2);
    out.push(quote);
    for c in text.chars() {
        match c {
            '\\' => out.push_str("\\\\"),
            '\n' => out.push_str("\\n"),
            '\r' => out.push_str("\\r"),
            '\t' => out.push_str("\\t"),
            c if c == quote => {
                out.push('\\');
                out.push(c);
            }
            c if crate::graph::pystr::is_printable(c) => out.push(c),
            c if (c as u32) < 0x100 => {
                let _ = write!(out, "\\x{:02x}", c as u32);
            }
            c if (c as u32) < 0x10000 => {
                let _ = write!(out, "\\u{:04x}", c as u32);
            }
            c => {
                let _ = write!(out, "\\U{:08x}", c as u32);
            }
        }
    }
    out.push(quote);
    out
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn tree_lists_root_files_then_directories() {
        let files: Vec<String> = ["b.py", "a/x.md", "a/y.md", "c/d/e.txt", "b.py"]
            .iter()
            .map(|s| (*s).to_owned())
            .collect();
        let tree = repository_tree(&files, 1);
        assert_eq!(
            tree,
            "Total files: 4\nShowing up to 1 files per directory\n\u{1f4c1} /\n  \u{1f4c4} b.py\n\
             \u{1f4c1} a/ (2 files)\n  \u{1f4c4} x.md\n  ... (+1 more)\n\u{1f4c1} c/d/ (1 files)\n  \u{1f4c4} e.txt"
        );
        assert_eq!(repository_tree(&[], 25), "No files found");
    }

    #[test]
    fn stats_keep_walk_order_for_ties_and_join_with_backslash_n() {
        let files: Vec<String> = ["a.txt", "b.scss", "c.txt", "d.scss", "e.JAVA"]
            .iter()
            .map(|s| (*s).to_owned())
            .collect();
        assert_eq!(
            file_stats(&files),
            "Total Files: 5\\nFile Type Distribution:\\n  - .txt: 2 files\\n  - .scss: 2 files\\n  - .java: 1 files"
        );
    }

    #[test]
    fn repr_matches_python() {
        assert_eq!(py_repr("a'b"), "\"a'b\"");
        assert_eq!(py_repr("a'b\""), "'a\\'b\"'");
        assert_eq!(py_repr("x\ny\u{7}"), "'x\\ny\\x07'");
        assert_eq!(py_repr("\u{2014}"), "'\u{2014}'");
    }
}

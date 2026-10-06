//! The prompt texts of repository analysis and structure planning, and
//! Python's `str.format` for them (ADR-0026 decision 8).
//!
//! Every text is the Python value, byte for byte, in a data file next to
//! this module (`prompts/*.txt`), compiled in with `include_str!`.
//! `prompts/PROMPTS_MANIFEST.json` names, per file, the Python source file,
//! the symbol (and, for a literal inside a function, which `("system", …)`
//! tuple), and the SHA-256 of the Python value. `tests/structure_prompts.rs`
//! re-derives every hash from the Python source with `python3`, so a prompt
//! edited on one side only fails the build.
//!
//! The `*_USER` templates are formatted the way Python formatted them:
//! `ChatPromptTemplate` (f-string templates) and `str.format` are the same
//! grammar — `{name}` is replaced, `{{` and `}}` are literal braces. The
//! system prompts hold no braces, so formatting leaves them as they are
//! and they go out as the `'static` texts below (a system message never
//! holds repository text: see `llm::chat`).

use std::fmt;

/// `_llm_analyze_repository`'s system prompt (the markdown analysis).
pub const REPO_ANALYSIS_SYSTEM: &str = include_str!("prompts/repo_analysis_system.txt");
/// `ENHANCED_REPO_ANALYSIS_PROMPT`.
pub const REPO_ANALYSIS_USER: &str = include_str!("prompts/repo_analysis_user.txt");
/// `_llm_analyze_repository`'s system prompt under
/// `DEEPWIKI_USE_STRUCTURED_REPO_ANALYSIS=1`.
pub const REPO_ANALYSIS_STRUCTURED_SYSTEM: &str =
    include_str!("prompts/repo_analysis_structured_system.txt");
/// `STRUCTURED_REPO_ANALYSIS_PROMPT`.
pub const REPO_ANALYSIS_STRUCTURED_USER: &str =
    include_str!("prompts/repo_analysis_structured_user.txt");
/// `generate_wiki_structure`'s system prompt (the classic planner).
pub const STRUCTURE_SYSTEM: &str = include_str!("prompts/structure_system.txt");
/// `ENHANCED_WIKI_STRUCTURE_PROMPT`.
pub const STRUCTURE_USER: &str = include_str!("prompts/structure_user.txt");
/// `TARGET_AUDIENCES["mixed"]`: the agent is built with
/// `TargetAudience.MIXED`, so this is the only audience the prompt gets.
pub const TARGET_AUDIENCE_MIXED: &str = include_str!("prompts/target_audience_mixed.txt");
/// `cluster_planner.BATCHED_NAMING_SYSTEM`.
pub const BATCHED_NAMING_SYSTEM: &str = include_str!("prompts/batched_naming_system.txt");
/// `cluster_planner.BATCHED_NAMING_USER`.
pub const BATCHED_NAMING_USER: &str = include_str!("prompts/batched_naming_user.txt");
/// `cluster_planner.PAGE_NAMING_SYSTEM`.
pub const PAGE_NAMING_SYSTEM: &str = include_str!("prompts/page_naming_system.txt");
/// `cluster_planner.PAGE_NAMING_USER`.
pub const PAGE_NAMING_USER: &str = include_str!("prompts/page_naming_user.txt");
/// `cluster_planner.SECTION_FROM_PAGES_SYSTEM`.
pub const SECTION_FROM_PAGES_SYSTEM: &str = include_str!("prompts/section_from_pages_system.txt");
/// `cluster_planner.SECTION_FROM_PAGES_USER`.
pub const SECTION_FROM_PAGES_USER: &str = include_str!("prompts/section_from_pages_user.txt");
/// `cluster_planner.SECTION_NAMING_SYSTEM`.
pub const SECTION_NAMING_SYSTEM: &str = include_str!("prompts/section_naming_system.txt");
/// `cluster_planner.SECTION_NAMING_USER`.
pub const SECTION_NAMING_USER: &str = include_str!("prompts/section_naming_user.txt");

/// `PROMPTS_MANIFEST.json`, for the hash test.
pub const MANIFEST: &str = include_str!("prompts/PROMPTS_MANIFEST.json");

/// Every embedded prompt by its data file name, for the manifest test.
pub const ALL: &[(&str, &str)] = &[
    ("repo_analysis_system.txt", REPO_ANALYSIS_SYSTEM),
    ("repo_analysis_user.txt", REPO_ANALYSIS_USER),
    (
        "repo_analysis_structured_system.txt",
        REPO_ANALYSIS_STRUCTURED_SYSTEM,
    ),
    (
        "repo_analysis_structured_user.txt",
        REPO_ANALYSIS_STRUCTURED_USER,
    ),
    ("structure_system.txt", STRUCTURE_SYSTEM),
    ("structure_user.txt", STRUCTURE_USER),
    ("target_audience_mixed.txt", TARGET_AUDIENCE_MIXED),
    ("batched_naming_system.txt", BATCHED_NAMING_SYSTEM),
    ("batched_naming_user.txt", BATCHED_NAMING_USER),
    ("page_naming_system.txt", PAGE_NAMING_SYSTEM),
    ("page_naming_user.txt", PAGE_NAMING_USER),
    ("section_from_pages_system.txt", SECTION_FROM_PAGES_SYSTEM),
    ("section_from_pages_user.txt", SECTION_FROM_PAGES_USER),
    ("section_naming_system.txt", SECTION_NAMING_SYSTEM),
    ("section_naming_user.txt", SECTION_NAMING_USER),
];

/// A template `str.format` would refuse. The templates are compiled in and
/// tested, so this is a programming error, reported rather than panicked.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum FormatError {
    /// A `{name}` with no value (`KeyError`).
    MissingField(String),
    /// A lone `}` or an unclosed `{` (`ValueError`).
    Malformed(&'static str),
}

impl fmt::Display for FormatError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::MissingField(name) => write!(f, "the prompt template has no value for '{name}'"),
            Self::Malformed(what) => write!(f, "the prompt template is malformed: {what}"),
        }
    }
}

impl std::error::Error for FormatError {}

/// `template.format(**fields)` for templates whose fields are plain names
/// (every template here). A conversion (`!r`) or a format spec (`:>4`) is
/// refused: no template uses one, and supporting half of the mini-language
/// would hide a template change.
///
/// # Errors
///
/// A field without a value, a lone `}` or an unclosed `{`.
pub fn format(template: &str, fields: &[(&str, &str)]) -> Result<String, FormatError> {
    let mut out = String::with_capacity(
        template.len() + fields.iter().map(|(_, value)| value.len()).sum::<usize>(),
    );
    let mut rest = template;
    while let Some(at) = rest.find(['{', '}']) {
        out.push_str(&rest[..at]);
        let brace = rest.as_bytes()[at];
        let after = &rest[at + 1..];
        if brace == b'}' {
            if let Some(stripped) = after.strip_prefix('}') {
                out.push('}');
                rest = stripped;
                continue;
            }
            return Err(FormatError::Malformed("single '}' encountered"));
        }
        if let Some(stripped) = after.strip_prefix('{') {
            out.push('{');
            rest = stripped;
            continue;
        }
        let Some(end) = after.find('}') else {
            return Err(FormatError::Malformed("single '{' encountered"));
        };
        let name = &after[..end];
        if name.contains(['!', ':', '{', '[', '.']) {
            return Err(FormatError::Malformed(
                "only plain field names are supported",
            ));
        }
        let value = fields
            .iter()
            .find(|(field, _)| *field == name)
            .map(|(_, value)| *value)
            .ok_or_else(|| FormatError::MissingField(name.to_owned()))?;
        out.push_str(value);
        rest = &after[end + 1..];
    }
    out.push_str(rest);
    Ok(out)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn format_replaces_fields_and_unescapes_braces() {
        let text = format("a {x} {{b}} {y}}}", &[("x", "1"), ("y", "{z}")]);
        assert_eq!(text, Ok("a 1 {b} {z}}".to_owned()));
    }

    #[test]
    fn format_refuses_what_str_format_refuses() {
        assert_eq!(
            format("{missing}", &[]),
            Err(FormatError::MissingField("missing".to_owned()))
        );
        assert!(matches!(
            format("a } b", &[]),
            Err(FormatError::Malformed(_))
        ));
        assert!(matches!(
            format("a { b", &[]),
            Err(FormatError::Malformed(_))
        ));
        assert!(matches!(
            format("{x!r}", &[("x", "1")]),
            Err(FormatError::Malformed(_))
        ));
    }

    #[test]
    fn system_prompts_have_no_fields() {
        // They go out unformatted; formatting must not change them.
        for text in [
            REPO_ANALYSIS_SYSTEM,
            REPO_ANALYSIS_STRUCTURED_SYSTEM,
            STRUCTURE_SYSTEM,
            BATCHED_NAMING_SYSTEM,
            PAGE_NAMING_SYSTEM,
            SECTION_FROM_PAGES_SYSTEM,
            SECTION_NAMING_SYSTEM,
        ] {
            assert!(!text.contains(['{', '}']), "{text}");
        }
    }

    #[test]
    fn every_user_template_formats_with_its_fields() {
        let cases: &[(&str, &[&str])] = &[
            (
                REPO_ANALYSIS_USER,
                &[
                    "repository_name",
                    "branch_name",
                    "repository_tree",
                    "readme_content",
                    "code_samples",
                    "file_stats",
                ],
            ),
            (
                REPO_ANALYSIS_STRUCTURED_USER,
                &[
                    "repository_name",
                    "branch_name",
                    "repository_tree",
                    "readme_content",
                    "code_samples",
                    "file_stats",
                ],
            ),
            (
                STRUCTURE_USER,
                &[
                    "repository_tree",
                    "readme_content",
                    "repo_analysis",
                    "target_audience",
                    "wiki_type",
                ],
            ),
            (
                BATCHED_NAMING_USER,
                &["node_count", "file_count", "pages_json"],
            ),
            (
                PAGE_NAMING_USER,
                &[
                    "symbol_count",
                    "file_count",
                    "section_name",
                    "page_symbols_json",
                    "directories_json",
                ],
            ),
            (
                SECTION_FROM_PAGES_USER,
                &["page_count", "node_count", "pages_json"],
            ),
            (
                SECTION_NAMING_USER,
                &[
                    "node_count",
                    "file_count",
                    "dominant_symbols_json",
                    "micro_summaries_json",
                ],
            ),
        ];
        for (template, names) in cases {
            let fields: Vec<(&str, &str)> = names.iter().map(|n| (*n, "VALUE")).collect();
            let text = format(template, &fields).expect("formats");
            assert!(!text.contains("{{"));
            for missing in 0..names.len() {
                let mut fewer = fields.clone();
                fewer.remove(missing);
                assert!(
                    format(template, &fewer).is_err(),
                    "{} is used",
                    names[missing]
                );
            }
        }
    }
}

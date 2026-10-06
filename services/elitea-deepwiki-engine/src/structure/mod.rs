//! Repository analysis and wiki structure planning: the first two nodes of
//! `generate_wiki`'s agent graph (`analyze_repository` →
//! `generate_wiki_structure`, `agents/wiki_graph_optimized.py`; ADR-0026
//! phase 5a).
//!
//! * [`analysis`] — the repository-analysis model call and its inputs.
//! * [`cluster`] — the cluster planner (`planner_type=cluster`, what the
//!   web app sends), over the index rows of Phase 3 ([`index`]).
//! * [`plan_wiki_structure`] — the planner choice; the classic single-call
//!   planner (`ENHANCED_WIKI_STRUCTURE_PROMPT`) that `auto` reaches for a
//!   small repository; a clear refusal for the deepagents planner, which
//!   is a later unit (5d).
//!
//! What page generation reads from the result is the
//! [`spec::WikiStructureSpec`] (sections → pages with `target_symbols`,
//! `target_docs`, `target_folders`, `key_files`, `retrieval_query` and, for
//! the cluster planner, `metadata.cluster_node_ids`) and
//! [`analysis::RepositoryAnalysis::repository_context`].

pub mod analysis;
pub mod centrality;
pub mod cluster;
pub mod files;
pub mod index;
pub mod model;
pub mod parse;
pub mod prompts;
pub mod spec;
pub mod validation;

use crate::errors::{EngineError, ErrorType};
use crate::llm::{ChatMessage, count_tokens};
use analysis::RepositoryAnalysis;
use cluster::{ClusterPlanner, ClusterSettings};
use index::PlannerIndex;
use model::{ChatModel, user_request};
use spec::WikiStructureSpec;

/// The structure planner the request names (`_resolve_planner_choice`).
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum PlannerChoice {
    Cluster,
    DeepAgents,
    /// By size: deepagents for a large repository, else the classic one.
    Auto,
}

impl PlannerChoice {
    /// `planner_mode` or `planner_type` (the first that is set), else
    /// `DEEPWIKI_STRUCTURE_PLANNER`, else `auto`. `agent`, `agentic` and
    /// `deepagents` are deepagents; anything unknown is `auto`.
    #[must_use]
    pub fn resolve(raw: Option<&str>) -> Self {
        match raw.map(|r| r.trim().to_lowercase()).as_deref() {
            Some("agent" | "agentic" | "deepagents") => Self::DeepAgents,
            Some("cluster") => Self::Cluster,
            _ => Self::Auto,
        }
    }
}

/// The environment the two nodes read.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct StructureSettings {
    /// `DEEPWIKI_USE_STRUCTURED_REPO_ANALYSIS=1`: the JSON analysis prompt.
    pub structured_analysis: bool,
    /// `DEEPWIKI_DEEPAGENTS_FILE_THRESHOLD` (2000).
    pub deepagents_file_threshold: usize,
    /// `DEEPWIKI_DEEPAGENTS_REPOCTX_TOKENS` (8000).
    pub deepagents_token_threshold: usize,
    pub cluster: ClusterSettings,
}

impl Default for StructureSettings {
    fn default() -> Self {
        Self {
            structured_analysis: false,
            deepagents_file_threshold: 2000,
            deepagents_token_threshold: 8000,
            cluster: ClusterSettings::default(),
        }
    }
}

impl StructureSettings {
    /// From an environment lookup. A threshold that is not an integer is
    /// the default (`_get_env_int`).
    #[must_use]
    pub fn from_lookup(exclude_tests: bool, lookup: impl Fn(&str) -> Option<String>) -> Self {
        let int = |name: &str, default: usize| match lookup(name) {
            Some(value) if !value.is_empty() => value.trim().parse().unwrap_or(default),
            _ => default,
        };
        Self {
            structured_analysis: lookup("DEEPWIKI_USE_STRUCTURED_REPO_ANALYSIS").as_deref()
                == Some("1"),
            deepagents_file_threshold: int("DEEPWIKI_DEEPAGENTS_FILE_THRESHOLD", 2000),
            deepagents_token_threshold: int("DEEPWIKI_DEEPAGENTS_REPOCTX_TOKENS", 8000),
            cluster: ClusterSettings::from_lookup(exclude_tests, &lookup),
        }
    }

    /// From the process environment.
    #[must_use]
    pub fn from_env(exclude_tests: bool) -> Self {
        Self::from_lookup(exclude_tests, |name| std::env::var(name).ok())
    }
}

/// The deepagents planner's refusal.
pub const DEEPAGENTS_UNSUPPORTED: &str = "The deepagents structure planner is not supported by the native engine yet (ADR-0026 phase 5d)";

/// A broken embedded template: a bug, reported as a `RuntimeError`.
pub(crate) fn template_error(error: &prompts::FormatError) -> EngineError {
    EngineError::new(ErrorType::Runtime, error.to_string())
}

/// `_should_use_deepagents_structure_planner` for `auto` (and for a
/// cluster planner that could not run): at least `file_threshold` files, or
/// a repository context of at least `token_threshold` tokens.
///
/// # Errors
///
/// Only a broken tokenizer build.
pub fn auto_uses_deepagents(
    files: usize,
    repository_context: &str,
    settings: &StructureSettings,
) -> Result<bool, EngineError> {
    if files > 0 && files >= settings.deepagents_file_threshold {
        return Ok(true);
    }
    Ok(!repository_context.is_empty()
        && count_tokens(repository_context)? >= settings.deepagents_token_threshold)
}

/// `generate_wiki_structure`.
///
/// `cluster_index` is the index after Phase 3; without it the cluster
/// planner cannot run, and the choice falls back to `auto` as Python's did
/// when it found no `.wiki.db`.
///
/// # Errors
///
/// The deepagents refusal, a failed classic model call, a classic answer
/// that holds broken JSON, or a stop.
pub async fn plan_wiki_structure(
    model: &impl ChatModel,
    choice: PlannerChoice,
    analysis: &RepositoryAnalysis,
    cluster_index: Option<&PlannerIndex>,
    settings: &StructureSettings,
) -> Result<WikiStructureSpec, EngineError> {
    if choice == PlannerChoice::Cluster {
        if let Some(index) = cluster_index {
            return ClusterPlanner::new(index, settings.cluster)
                .plan_structure(model)
                .await;
        }
        tracing::warn!(
            "Cluster-based structure planner failed, falling back to deepagents: no cluster index"
        );
    }
    let deepagents = match choice {
        PlannerChoice::DeepAgents => true,
        PlannerChoice::Cluster | PlannerChoice::Auto => {
            auto_uses_deepagents(analysis.files.len(), &analysis.repository_context, settings)?
        }
    };
    if deepagents {
        return Err(EngineError::new(ErrorType::Runtime, DEEPAGENTS_UNSUPPORTED));
    }
    classic_structure(model, analysis).await
}

/// The classic planner's messages.
///
/// # Errors
///
/// Only a broken prompt template.
pub fn classic_messages(analysis: &RepositoryAnalysis) -> Result<Vec<ChatMessage>, EngineError> {
    // `repo_context or str(repo_analysis)`.
    let summary;
    let repo_analysis = if analysis.repository_context.is_empty() {
        summary = analysis.summary.to_python_str();
        summary.as_str()
    } else {
        analysis.repository_context.as_str()
    };
    let user = prompts::format(
        prompts::STRUCTURE_USER,
        &[
            ("repository_tree", &analysis.repository_tree),
            ("readme_content", &analysis.readme_content),
            ("repo_analysis", repo_analysis),
            ("target_audience", prompts::TARGET_AUDIENCE_MIXED),
            ("wiki_type", "comprehensive"),
        ],
    )
    .map_err(|e| template_error(&e))?;
    Ok(vec![
        ChatMessage::System(prompts::STRUCTURE_SYSTEM),
        ChatMessage::User(user),
    ])
}

/// The classic single-call planner: the structure prompt, the answer's
/// JSON validated as a `WikiStructureSpec`; an answer that does not
/// validate becomes the fallback structure (one "Overview" page).
///
/// # Errors
///
/// The model call's failure, or an answer whose JSON candidate does not
/// parse (Python failed the node with "Structure generation failed").
pub async fn classic_structure(
    model: &impl ChatModel,
    analysis: &RepositoryAnalysis,
) -> Result<WikiStructureSpec, EngineError> {
    let messages = classic_messages(analysis)?;
    let answer = model.complete(&user_request(messages)).await?;
    let data = parse::structure_json(&answer).map_err(|error| {
        EngineError::new(
            ErrorType::Runtime,
            format!("Structure generation failed: {}", error.0),
        )
    })?;
    match WikiStructureSpec::validate(&data) {
        Ok(spec) => Ok(spec),
        Err(error) => {
            tracing::warn!(
                "WikiStructureSpec validation failed; using fallback structure. Error: {error}"
            );
            WikiStructureSpec::validate(&parse::fallback_structure())
                .map_err(|e| EngineError::new(ErrorType::Runtime, e.to_string()))
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn planner_choice_follows_the_aliases() {
        assert_eq!(
            PlannerChoice::resolve(Some(" Cluster ")),
            PlannerChoice::Cluster
        );
        assert_eq!(
            PlannerChoice::resolve(Some("agentic")),
            PlannerChoice::DeepAgents
        );
        assert_eq!(PlannerChoice::resolve(Some("classic")), PlannerChoice::Auto);
        assert_eq!(PlannerChoice::resolve(None), PlannerChoice::Auto);
    }

    #[test]
    fn auto_measures_files_then_tokens() {
        let settings = StructureSettings::default();
        assert_eq!(auto_uses_deepagents(2000, "", &settings), Ok(true));
        assert_eq!(auto_uses_deepagents(1999, "short", &settings), Ok(false));
        let long = "word ".repeat(9000);
        assert_eq!(auto_uses_deepagents(10, &long, &settings), Ok(true));
        let custom = StructureSettings::from_lookup(false, |name| {
            (name == "DEEPWIKI_DEEPAGENTS_FILE_THRESHOLD").then(|| "5".to_owned())
        });
        assert_eq!(auto_uses_deepagents(5, "", &custom), Ok(true));
        let invalid = StructureSettings::from_lookup(false, |name| {
            (name == "DEEPWIKI_DEEPAGENTS_FILE_THRESHOLD").then(|| "five".to_owned())
        });
        assert_eq!(invalid.deepagents_file_threshold, 2000);
    }
}

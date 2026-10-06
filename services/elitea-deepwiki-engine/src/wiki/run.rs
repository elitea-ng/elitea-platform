//! Phase 5b end to end: the planner's structure in, the composed engine
//! result out (`dispatch_page_generation` → `generate_page_content`
//! × N → `finalize_wiki` → `export_wiki` → the hybrid wrapper's result →
//! `wiki_subprocess_worker`'s composition).

use super::compose::{ComposeInput, Composed, FailedPage, VersionClock, compose, wrapper_message};
use super::export::{ArtifactExporter, exporter_wiki_id};
use super::pages::{
    GeneratedPages, PageGenerator, generate_pages, max_symbols_per_page, split_overloaded_pages,
};
use super::search::PageSearch;
use super::spec::{PageStatus, WikiStructureSpec};
use crate::errors::EngineError;
use std::sync::Arc;

/// The worker inputs the composition reads.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct WikiIdentity {
    /// The toolkit repository after provider clean-up (`github_repository`).
    pub repository: String,
    /// `canonical_repository_path(repository, clone_config)`.
    pub canonical_repository: String,
    /// The branch the clone checked out.
    pub branch: String,
    pub commit_hash: Option<String>,
    pub provider_type: String,
    pub query: String,
}

/// What the page phase produced.
#[derive(Debug, Clone, PartialEq)]
pub struct PagesOutcome {
    /// The structure after `_split_overloaded_pages`.
    pub structure: WikiStructureSpec,
    pub pages: GeneratedPages,
    pub composed: Composed,
}

/// `DEEPWIKI_MAX_SYMBOLS_PER_PAGE` over the size-scaled default
/// (`_get_env_int`: an unset, empty or non-integer value is the default).
#[must_use]
pub fn symbol_cap(node_count: usize, file_count: usize) -> usize {
    let default = max_symbols_per_page(node_count, file_count);
    match std::env::var("DEEPWIKI_MAX_SYMBOLS_PER_PAGE") {
        Ok(value) if !value.is_empty() => value
            .trim()
            .parse::<i64>()
            .ok()
            .map_or(default, |v| usize::try_from(v).unwrap_or(0)),
        _ => default,
    }
}

/// Generate, export and compose.
///
/// `started` is when `generate_wiki` began (the result's
/// `execution_time` and the wrapper message cover the whole run).
///
/// # Errors
///
/// A stop request, or an export failure.
pub async fn generate_wiki_pages<S: PageSearch + 'static>(
    generator: Arc<PageGenerator<S>>,
    mut structure: WikiStructureSpec,
    repo_context: &str,
    identity: &WikiIdentity,
    clock: &VersionClock,
    started: std::time::Instant,
) -> Result<PagesOutcome, EngineError> {
    let facts = generator.retrieval.index.facts();
    // `_repository_file_count` is never set on the agent: always 0.
    let cap = symbol_cap(facts.node_count, 0);
    split_overloaded_pages(&mut structure, cap, &facts.name_paths);
    let pages = generate_pages(Arc::clone(&generator), &structure, repo_context).await?;
    let failed_pages: Vec<FailedPage> = pages
        .pages
        .iter()
        .filter(|p| p.status == PageStatus::Failed)
        .map(|p| FailedPage {
            page_id: p.page_id.clone(),
            title: p.title.clone(),
            status: p.status.as_str().to_owned(),
        })
        .collect();
    let exporter = ArtifactExporter {
        wiki_id: exporter_wiki_id(&generator.settings.repository_url, &identity.branch),
    };
    let artifacts = exporter.export(&structure, &pages.pages, &clock.now.file_stamp())?;
    let execution_time = started.elapsed().as_secs_f64();
    let message = wrapper_message(
        &identity.repository,
        &identity.query,
        pages.pages.len(),
        structure.sections.len(),
        execution_time,
        !pages.errors.is_empty() || !failed_pages.is_empty(),
    );
    let composed = compose(
        ComposeInput {
            repository: identity.repository.clone(),
            canonical_repository: identity.canonical_repository.clone(),
            branch: identity.branch.clone(),
            commit_hash: identity.commit_hash.clone(),
            provider_type: identity.provider_type.clone(),
            indexing_method: "filesystem".to_owned(),
            repository_context: repo_context.to_owned(),
            artifacts,
            errors: pages.errors.clone(),
            failed_pages,
            message,
            execution_time,
        },
        clock,
    );
    Ok(PagesOutcome {
        structure,
        pages,
        composed,
    })
}

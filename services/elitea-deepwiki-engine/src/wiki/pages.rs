//! The page fan-out: `dispatch_page_generation` (with
//! `_split_overloaded_pages`), `generate_page_content` per page,
//! `_generate_simple`, the Mermaid sanitizer, and `finalize_wiki`.
//!
//! `LangGraph` ran the pages as `Send` tasks with `max_concurrency` 4 and
//! accumulated them with `operator.add` in task order; here at most four
//! pages are in flight and the results keep the structure order.
//! `recursion_limit` 5000 bounded `LangGraph`'s super-steps, of which this
//! graph has a handful; there is no equivalent to port.
//!
//! A page whose retrieval or model call fails becomes a FAILED page with
//! empty content and an error line (Python's `except` in
//! `generate_page_content`); the wiki is still exported. A stop request
//! is not a page failure: it ends the run.

use super::context::RelevantContent;
use super::prompts::{
    PAGE_CONTENT_SYSTEM, PAGE_CONTENT_V3_TONE_ADJUSTED, TARGET_AUDIENCE_MIXED, format_template,
};
use super::retrieve::RetrievalContext;
use super::sanitizer::{SanitizerConfig, sanitize_content};
use super::search::PageSearch;
use super::spec::{PageSpec, PageStatus, WikiPage, WikiStructureSpec};
use crate::errors::EngineError;
use crate::llm::{ChatClient, ChatMessage, ChatRequest};
use crate::runner::StopSignal;
use std::collections::HashMap;
use std::sync::Arc;
use tokio::task::JoinSet;

/// `max_concurrency` of the generation graph.
pub const MAX_CONCURRENCY: usize = 4;

/// `OptimizedWikiGenerationAgent.MAX_SYMBOLS_PER_PAGE` scaled by
/// `_compute_max_symbols_per_page`: 25 / 30 / 40 / 50 at 500 / 2000 /
/// 5000 graph nodes or files.
#[must_use]
pub fn max_symbols_per_page(graph_nodes: usize, file_count: usize) -> usize {
    match graph_nodes.max(file_count) {
        n if n >= 5000 => 50,
        n if n >= 2000 => 40,
        n if n >= 500 => 30,
        _ => 25,
    }
}

/// `_split_overloaded_pages`: a page with more target symbols than the cap
/// becomes "(Part n)" pages of at most the cap, grouped by file.
///
/// `name_paths` is the file of `_name_index[symbol][0]`. The sub-pages
/// keep the rationale (so they still route to cluster expansion) but not
/// the metadata, exactly as Python built them.
///
/// DELIBERATE DIFFERENCE: a cap below 1 (`DEEPWIKI_MAX_SYMBOLS_PER_PAGE=0`)
/// made Python's chunk loop spin forever; it is ignored here.
pub fn split_overloaded_pages<H: std::hash::BuildHasher>(
    structure: &mut WikiStructureSpec,
    max_symbols: usize,
    name_paths: &HashMap<String, String, H>,
) -> usize {
    let max_symbols = max_symbols.max(1);
    let mut total_splits = 0;
    for section in &mut structure.sections {
        let mut new_pages = Vec::with_capacity(section.pages.len());
        for page in std::mem::take(&mut section.pages) {
            if page.target_symbols.len() <= max_symbols {
                new_pages.push(page);
                continue;
            }
            // file → symbols, insertion-ordered (a Python dict).
            let mut groups: Vec<(String, Vec<String>)> = Vec::new();
            let mut ungrouped: Vec<String> = Vec::new();
            for symbol in &page.target_symbols {
                match name_paths.get(symbol).filter(|p| !p.is_empty()) {
                    Some(path) => match groups.iter_mut().find(|(p, _)| p == path) {
                        Some(group) => group.1.push(symbol.clone()),
                        None => groups.push((path.clone(), vec![symbol.clone()])),
                    },
                    None => ungrouped.push(symbol.clone()),
                }
            }
            if !ungrouped.is_empty() {
                if groups.is_empty() {
                    groups.push(("__ungrouped__".to_owned(), ungrouped));
                } else {
                    let count = groups.len();
                    for (i, symbol) in ungrouped.into_iter().enumerate() {
                        groups[i % count].1.push(symbol);
                    }
                }
            }
            groups.sort_by(|a, b| a.0.cmp(&b.0));
            let mut chunks: Vec<Vec<String>> = Vec::new();
            let mut current: Vec<String> = Vec::new();
            for (_, symbols) in groups {
                if current.len() + symbols.len() > max_symbols && !current.is_empty() {
                    chunks.push(std::mem::take(&mut current));
                }
                current.extend(symbols);
                while current.len() > max_symbols {
                    let rest = current.split_off(max_symbols);
                    chunks.push(std::mem::replace(&mut current, rest));
                }
            }
            if !current.is_empty() {
                chunks.push(current);
            }
            let parts = chunks.len();
            for (index, chunk) in chunks.into_iter().enumerate() {
                let part = index + 1;
                let suffix = if parts > 1 {
                    format!(" (Part {part})")
                } else {
                    String::new()
                };
                new_pages.push(PageSpec {
                    page_name: format!("{}{suffix}", page.page_name),
                    page_order: page.page_order * 100 + i64::try_from(part).unwrap_or(i64::MAX),
                    description: page.description.clone(),
                    content_focus: page.content_focus.clone(),
                    rationale: format!("{} [auto-split part {part}/{parts}]", page.rationale),
                    target_symbols: chunk,
                    target_docs: page.target_docs.clone(),
                    target_folders: page.target_folders.clone(),
                    key_files: page.key_files.clone(),
                    retrieval_query: page.retrieval_query.clone(),
                    metadata: serde_json::Map::new(),
                });
            }
            total_splits += parts.saturating_sub(1);
        }
        section.pages = new_pages;
    }
    if total_splits > 0 {
        structure.total_pages = structure
            .sections
            .iter()
            .map(|s| i64::try_from(s.pages.len()).unwrap_or(i64::MAX))
            .sum();
    }
    total_splits
}

/// The agent configuration the page path reads.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct PageSettings {
    /// `repository_url` (the toolkit's `owner/repo`).
    pub repository_url: String,
    /// `wiki_style.value`: always `comprehensive`.
    pub wiki_style: String,
    /// `DEEPWIKI_SKIP_REPO_CONTEXT_FOR_PAGES=1`.
    pub skip_repo_context: bool,
}

impl PageSettings {
    #[must_use]
    pub fn new(repository_url: impl Into<String>) -> Self {
        Self {
            repository_url: repository_url.into(),
            wiki_style: "comprehensive".to_owned(),
            skip_repo_context: std::env::var("DEEPWIKI_SKIP_REPO_CONTEXT_FOR_PAGES").as_deref()
                == Ok("1"),
        }
    }
}

/// The user message of `_generate_simple`.
///
/// # Errors
///
/// Never for the embedded template (the tests format it).
pub fn page_prompt(
    page: &PageSpec,
    content: &RelevantContent,
    repo_context: &str,
    settings: &PageSettings,
) -> Result<String, EngineError> {
    let section_name = if page.page_name.contains('/') {
        page.page_name.split('/').next().unwrap_or("")
    } else {
        "Main"
    };
    let repository_context = if settings.skip_repo_context {
        ""
    } else {
        repo_context
    };
    // The files are joined with the two characters `\` `n` (Python quirk).
    let related_files = content.files.join("\\n");
    format_template(
        PAGE_CONTENT_V3_TONE_ADJUSTED,
        &[
            ("section_name", section_name),
            ("page_name", &page.page_name),
            ("page_description", &page.description),
            ("content_focus", &page.content_focus),
            ("repository_url", &settings.repository_url),
            ("wiki_style", &settings.wiki_style),
            ("repository_context", repository_context),
            ("relevant_content", &content.content),
            ("related_files", &related_files),
            ("target_audience", TARGET_AUDIENCE_MIXED),
        ],
    )
}

/// One page's outcome: the page and its error line.
type PageResult = Result<(WikiPage, Option<String>), EngineError>;

/// Progress lines (`invocation_thinking`).
pub type Thinking = Arc<dyn Fn(String) + Send + Sync>;

/// Everything the fan-out shares between pages.
pub struct PageGenerator<S: PageSearch + 'static> {
    pub retrieval: RetrievalContext<S>,
    pub chat: ChatClient,
    pub settings: PageSettings,
    pub stop: StopSignal,
    pub thinking: Option<Thinking>,
}

/// The pages, in structure order, and the error lines.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct GeneratedPages {
    pub pages: Vec<WikiPage>,
    pub errors: Vec<String>,
}

impl<S: PageSearch + 'static> PageGenerator<S> {
    fn think(&self, text: String) {
        if let Some(sink) = &self.thinking {
            sink(text);
        }
    }

    /// `generate_page_content` for one page.
    async fn generate_page(
        &self,
        page_id: String,
        page: PageSpec,
        repo_context: Arc<String>,
    ) -> Result<(WikiPage, Option<String>), EngineError> {
        self.think(format!(
            "I am on phase page_generation\nDrafting page: {}\nReasoning: Converting structural intent into detailed narrative leveraging repository signals.\nNext: Retrieve relevant symbols/files then invoke LLM.",
            page.page_name
        ));
        match self.draft(&page, &repo_context).await {
            Ok(content) => Ok((
                WikiPage {
                    page_id,
                    title: page.page_name,
                    content,
                    status: PageStatus::Completed,
                },
                None,
            )),
            Err(error) if self.stop.is_requested() => Err(error),
            Err(error) => {
                self.think(format!(
                    "I am on phase page_generation\nPage failed: {}\nError: {}",
                    page.page_name, error.message
                ));
                let line = format!(
                    "Page generation failed for {}: {}",
                    page.page_name, error.message
                );
                Ok((
                    WikiPage {
                        page_id,
                        title: page.page_name,
                        content: String::new(),
                        status: PageStatus::Failed,
                    },
                    Some(line),
                ))
            }
        }
    }

    /// Retrieval, `_generate_simple`, then the sanitizer.
    async fn draft(&self, page: &PageSpec, repo_context: &str) -> Result<String, EngineError> {
        let (content, _route) = self.retrieval.relevant_content(page, repo_context)?;
        let prompt = page_prompt(page, &content, repo_context, &self.settings)?;
        let request = ChatRequest::new(vec![
            ChatMessage::System(PAGE_CONTENT_SYSTEM),
            ChatMessage::User(prompt),
        ]);
        let response = self.chat.chat(&request, &self.stop, &mut |_| {}).await?;
        let generated = response.content;
        match sanitize_content(&generated, &SanitizerConfig::default()) {
            Ok((sanitized, summary)) if summary.total > 0 => Ok(sanitized),
            Ok(_) => Ok(generated),
            Err(error) => {
                tracing::warn!(page = %page.page_name, %error, "Mermaid sanitization skipped");
                Ok(generated)
            }
        }
    }
}

/// Generate every page of `structure` (already split), at most
/// [`MAX_CONCURRENCY`] at a time, in structure order.
///
/// # Errors
///
/// Only a stop request; a failing page is a failed page.
pub async fn generate_pages<S: PageSearch + 'static>(
    generator: Arc<PageGenerator<S>>,
    structure: &WikiStructureSpec,
    repo_context: &str,
) -> Result<GeneratedPages, EngineError> {
    let mut work: Vec<(String, PageSpec)> = Vec::new();
    for (section_index, section) in structure.sections.iter().enumerate() {
        for (page_index, page) in section.pages.iter().enumerate() {
            work.push((format!("{section_index}#{page_index}"), page.clone()));
        }
    }
    generator.think(format!(
        "I am on phase page_generation\nPlanning {} pages for parallel drafting\nReasoning: Concurrency reduces end-to-end latency while leveraging shared repo analysis.\nNext: Draft each page with targeted context retrieval.",
        work.len()
    ));
    let repo_context = Arc::new(repo_context.to_owned());
    let mut slots: Vec<Option<(WikiPage, Option<String>)>> = vec![None; work.len()];
    let mut tasks: JoinSet<(usize, PageResult)> = JoinSet::new();
    let mut pending = work.into_iter().enumerate();
    let spawn = |tasks: &mut JoinSet<_>,
                 pending: &mut dyn Iterator<Item = (usize, (String, PageSpec))>| {
        if let Some((slot, (page_id, page))) = pending.next() {
            let generator = Arc::clone(&generator);
            let repo_context = Arc::clone(&repo_context);
            tasks.spawn(async move {
                (
                    slot,
                    generator.generate_page(page_id, page, repo_context).await,
                )
            });
        }
    };
    for _ in 0..MAX_CONCURRENCY {
        spawn(&mut tasks, &mut pending);
    }
    while let Some(joined) = tasks.join_next().await {
        let (slot, result) = joined.map_err(|e| {
            EngineError::new(
                crate::errors::ErrorType::Runtime,
                format!("page task failed: {e}"),
            )
        })?;
        match result {
            Ok(done) => slots[slot] = Some(done),
            Err(error) => {
                tasks.abort_all();
                return Err(error);
            }
        }
        spawn(&mut tasks, &mut pending);
    }
    let mut generated = GeneratedPages::default();
    for (page, error) in slots.into_iter().flatten() {
        generated.pages.push(page);
        generated.errors.extend(error);
    }
    Ok(generated)
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::wiki::spec::SectionSpec;

    fn page(symbols: usize) -> PageSpec {
        PageSpec {
            page_name: "P".into(),
            page_order: 2,
            description: "d".into(),
            content_focus: "c".into(),
            rationale: "Grouped by graph clustering (macro=1, micro=2, 9 symbols)".into(),
            target_symbols: (0..symbols).map(|i| format!("S{i:02}")).collect(),
            target_docs: Vec::new(),
            target_folders: Vec::new(),
            key_files: Vec::new(),
            retrieval_query: String::new(),
            metadata: serde_json::Map::new(),
        }
    }

    #[test]
    fn caps_scale_with_size() {
        assert_eq!(max_symbols_per_page(10, 0), 25);
        assert_eq!(max_symbols_per_page(499, 500), 30);
        assert_eq!(max_symbols_per_page(2000, 0), 40);
        assert_eq!(max_symbols_per_page(0, 5000), 50);
    }

    #[test]
    fn an_overloaded_page_splits_by_file_then_linearly() {
        let mut structure = WikiStructureSpec {
            wiki_title: "W".into(),
            overview: "o".into(),
            sections: vec![SectionSpec {
                section_name: "S".into(),
                section_order: 1,
                description: "d".into(),
                rationale: "r".into(),
                pages: vec![page(3), page(7)],
            }],
            total_pages: 2,
        };
        let paths: HashMap<String, String> = [("S00", "b.py"), ("S01", "a.py"), ("S02", "b.py")]
            .into_iter()
            .map(|(s, p)| (s.to_owned(), p.to_owned()))
            .collect();
        assert_eq!(split_overloaded_pages(&mut structure, 3, &paths), 2);
        let pages = &structure.sections[0].pages;
        let names: Vec<&str> = pages.iter().map(|p| p.page_name.as_str()).collect();
        assert_eq!(names, ["P", "P (Part 1)", "P (Part 2)", "P (Part 3)"]);
        // a.py: S01 + the ungrouped S04 S06; b.py: S00 S02 + S03 S05.
        assert_eq!(pages[1].target_symbols, ["S01", "S04", "S06"]);
        assert_eq!(pages[2].target_symbols, ["S00", "S02", "S03"]);
        assert_eq!(pages[3].target_symbols, ["S05"]);
        assert_eq!(pages[1].page_order, 201);
        assert!(pages[1].rationale.ends_with("[auto-split part 1/3]"));
        assert_eq!(structure.total_pages, 4);
    }
}

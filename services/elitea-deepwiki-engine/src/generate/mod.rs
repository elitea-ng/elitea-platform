//! `generate_wiki`, natively (ADR-0026 phase 5c): the whole pipeline the
//! worker child runs, from the host's keyword set to the composed result.
//!
//! The Python path it replaces: `tool_operations.generate_wiki` →
//! `wiki_subprocess_worker.main` → `HybridWikiToolkitWrapper.generate_wiki`
//! (`FilesystemRepositoryIndexer.index_repository`, `_write_unified_db`,
//! then the agent graph) → the worker's composition → the sidecar's
//! `publish_generation`. In order:
//!
//! 1. the arguments ([`arguments`]) and the environment flags, refused up
//!    front when invalid;
//! 2. ingest: the clone target from `repo_config`, the git allowlist again,
//!    the shallow clone into this job's scratch directory; or, for an
//!    artifact-folder source, the folder listed and downloaded there from
//!    the platform's object API ([`ingest::artifact`]);
//! 3. Phase 1 + 1c: discovery, the eight parsers, the code graph;
//! 4. a build in the `deepwiki_build` space: the graph staged (`COPY`), then
//!    every node with text embedded and its vector staged ([`embed`]);
//! 5. Phase 2 against the staged rows ([`crate::storage::topology`]), the
//!    model as the fallback embedder; the staged edges replaced;
//! 6. Phase 3 with the hub list Python passed (the first 20 in id order);
//!    the cluster columns written to the staged nodes;
//! 7. repository analysis and the structure planner, over the rows as the
//!    index stores them (types before Phase 2's re-typing, the cluster
//!    columns);
//! 8. page generation, export and the worker's composition;
//! 9. the publish (one transaction), before the result line: a failure is
//!    reported in band in `errors`, as `publishing.py` reported it, and the
//!    build is abandoned.
//!
//! The publish runs LAST, as in Python (the sidecar published only a
//! generation that succeeded): a run that fails at the model never replaces
//! a wiki's live index. The open build beats its heartbeat meanwhile.
//!
//! Progress goes out as `thinking` lines worded like the Python worker's
//! log lines (the parent relayed those). A stop is checked between phases,
//! inside the clone, the embedding rounds, every Phase 2 index access and
//! every model call; the CPU-bound parse has no checkpoint, and the parent
//! kills the child 3 s after its SIGTERM.

pub mod arguments;
pub mod embed;

use crate::config::Settings;
use crate::errors::{EngineError, ErrorType};
use crate::graph::clustering::{
    ClusterAssignment, ClusterGraph, LeidenPartitioner, Phase3Flags, run_phase3,
};
use crate::graph::flags::Phase1cFlags;
use crate::graph::topology::{self, CalibrationProfile, Phase2Config};
use crate::graph::{CodeGraph, builder, discover, node_row};
use crate::ingest::{self, Admitted, ClonedRepository};
use crate::llm::{ChatClient, EmbeddingClient, EmbeddingOptions, Transport, TransportSettings};
use crate::runner::Context;
use crate::storage::build::{Build, BuildSpace, WikiRecord};
use crate::storage::topology::PgTopologyStore;
use crate::storage::{self, StorageError};
use crate::structure::index::PlannerIndex;
use crate::structure::model::LiveModel;
use crate::structure::{self, PlannerChoice, StructureSettings, analysis};
use crate::wiki::compose::{VersionClock, build_repo_identifier, normalize_wiki_id};
use crate::wiki::context::PylonPlugin;
use crate::wiki::expansion::ExpansionFlags;
use crate::wiki::index::{GraphFacts, PageIndex};
use crate::wiki::pages::{PageGenerator, PageSettings};
use crate::wiki::retrieve::{CONTEXT_TOKEN_BUDGET, RetrievalContext};
use crate::wiki::run::{WikiIdentity, generate_wiki_pages};
use crate::wiki::search::SubstringSearch;
use arguments::GenerateRequest;
use embed::BlockingEmbedder;
use serde_json::{Map, Value, json};
use std::collections::HashMap;
use std::path::{Path, PathBuf};
use std::sync::Arc;
use std::sync::atomic::{AtomicBool, Ordering};
use tokio::runtime::Handle;

/// Connections the generation keeps to the index database: the heartbeat,
/// a staging round or a Phase 2 query, and the publish.
const POOL_CONNECTIONS: u32 = 4;

/// What the worker child hands the pipeline.
pub struct Job<'a> {
    /// This generation's own scratch directory; the clone goes in it.
    pub scratch: &'a Path,
    /// The boot id the build is recorded under: the PARENT's run, so the
    /// parent's own reconciliation never takes it for an earlier run's.
    pub boot_id: Option<&'a str>,
    /// Told the build id as soon as the build is open, so the parent can
    /// delete it if the child is killed before it abandons the build.
    pub on_build: &'a (dyn Fn(&str) + Send + Sync),
    /// Told `true` just before the publish starts and `false` when it has
    /// ended (committed or not). The parent defers a stop's kill while the
    /// publish runs, so a stop never cuts a commit in two halves it cannot
    /// tell apart.
    pub on_publishing: &'a (dyn Fn(bool) + Send + Sync),
}

fn runtime(message: impl Into<String>) -> EngineError {
    EngineError::new(ErrorType::Runtime, message)
}

fn value_error(message: impl Into<String>) -> EngineError {
    EngineError::new(ErrorType::Value, message)
}

/// A storage failure while indexing.
pub(crate) fn storage_failure(what: &str, error: &StorageError) -> EngineError {
    runtime(format!("Repository indexing failed: {what}: {error}"))
}

fn join_failure(error: &tokio::task::JoinError) -> EngineError {
    runtime(format!(
        "Repository indexing failed: an indexing task ended abnormally ({error})"
    ))
}

/// The flags the pipeline reads from the environment, checked before any
/// work starts (a typo fails the request, not the run an hour in).
#[derive(Debug, Clone, Copy)]
struct Environment {
    phase1c: Phase1cFlags,
    profile: CalibrationProfile,
    exclude_tests: bool,
    planner: PlannerChoice,
    structure: StructureSettings,
}

impl Environment {
    fn read(request: &GenerateRequest) -> Result<Self, EngineError> {
        let phase1c = Phase1cFlags::from_env().map_err(|e| value_error(e.to_string()))?;
        let profile = CalibrationProfile::from_env().map_err(value_error)?;
        // The request's choice, else `DEEPWIKI_EXCLUDE_TESTS` (Python's
        // worker wrote the request's choice into that variable).
        let exclude_tests = match request.exclude_tests {
            Some(choice) => choice,
            None => ExpansionFlags::from_env()?.exclude_tests,
        };
        Ok(Self {
            phase1c,
            profile,
            exclude_tests,
            planner: request.planner(|name| std::env::var(name).ok()),
            structure: StructureSettings::from_env(exclude_tests),
        })
    }
}

/// Run `generate_wiki`.
///
/// # Errors
///
/// A refused argument or setting (`ValueError`), an ingest refusal or
/// failure, a model failure, an index failure, or the stop line. The build,
/// when one was opened and not published, is abandoned before returning.
pub async fn generate_wiki(
    arguments: &Map<String, Value>,
    settings: &Settings,
    context: &Context,
    job: &Job<'_>,
) -> Result<Value, EngineError> {
    let started = std::time::Instant::now();
    let request = GenerateRequest::parse(arguments)?;
    let environment = Environment::read(&request)?;
    let Some(url) = settings.database_url.as_ref() else {
        return Err(value_error(format!(
            "The native engine needs {}: every index is staged and published in PostgreSQL",
            storage::DSN_ENV
        )));
    };
    let pool = storage::worker_pool(url.expose(), POOL_CONNECTIONS)
        .map_err(|error| value_error(error.to_string()))?;
    let mut slot: Option<Build> = None;
    let outcome = Pipeline {
        request: &request,
        environment,
        settings,
        context,
        job,
        started,
    }
    .run(&pool, &mut slot)
    .await;
    if let Some(build) = slot.take() {
        let build_id = build.build_id().to_owned();
        if let Err(error) = build.abandon().await {
            tracing::warn!(build = %build_id, %error, "could not abandon the build; the parent or the sweep removes it");
        }
    }
    pool.close().await;
    outcome
}

/// One run's inputs.
struct Pipeline<'a> {
    request: &'a GenerateRequest,
    environment: Environment,
    settings: &'a Settings,
    context: &'a Context,
    job: &'a Job<'a>,
    started: std::time::Instant,
}

fn open_build(slot: &mut Option<Build>) -> Result<&mut Build, EngineError> {
    slot.as_mut()
        .ok_or_else(|| runtime("Repository indexing failed: the build is no longer held"))
}

/// The cluster columns of the index rows, by node id.
type ClusterColumns = HashMap<String, (Option<i64>, Option<i64>)>;

fn cluster_columns(assignments: &[ClusterAssignment]) -> ClusterColumns {
    assignments
        .iter()
        .map(|a| {
            (
                a.node_id.clone(),
                (
                    a.macro_cluster.and_then(|m| i64::try_from(m).ok()),
                    a.micro_cluster.and_then(|m| i64::try_from(m).ok()),
                ),
            )
        })
        .collect()
}

/// The run's error for a failed Phase 2: the stop line after a stop; the
/// model service's own type (and so its category) when the gateway failed,
/// worded as the node embedding words it; else a `RuntimeError`.
fn phase2_failure(error: &topology::StoreError, stopped: bool, model: &str) -> EngineError {
    if stopped {
        return EngineError::cancelled();
    }
    match error.engine_error() {
        Some(cause) => EngineError::new(
            cause.error_type,
            format!(
                "Embedding an orphan node with {model} failed: {}",
                cause.message
            ),
        ),
        None => runtime(format!(
            "Repository indexing failed: Phase 2 graph topology failed: {error}"
        )),
    }
}

/// `run_phase2`'s summary line, as `_write_unified_db` logged it.
fn phase2_summary(stats: &Value) -> String {
    let number = |path: &[&str]| {
        path.iter()
            .try_fold(stats, |value, key| value.get(*key))
            .and_then(Value::as_u64)
            .unwrap_or(0)
    };
    format!(
        "Phase 2 complete: {} edges weighted, {} hubs, {}/{} orphans resolved, {}→{} components bridged",
        number(&["weighting", "edges_weighted"]),
        number(&["hubs", "count"]),
        number(&["orphan_resolution", "orphans_resolved"]),
        number(&["orphan_resolution", "orphan_count"]),
        number(&["component_bridging", "components_before"]),
        number(&["component_bridging", "components_after"]),
    )
}

impl Pipeline<'_> {
    #[allow(clippy::too_many_lines)] // one phase after the other, as Python's
    async fn run(
        &self,
        pool: &sqlx::PgPool,
        slot: &mut Option<Build>,
    ) -> Result<Value, EngineError> {
        let (request, context, settings) = (self.request, self.context, self.settings);
        context.checkpoint()?;
        context.thinking(format!(
            "[worker] Building LLM+embeddings (repo={}, branch={}, provider={})",
            request.repository_label(),
            request.requested_branch,
            request.provider_label()
        ));
        let transport = Transport::new(&TransportSettings::from(&settings.model))?;
        // An artifact folder is read over the same TLS stack.
        let objects = transport.clone();
        let chat = ChatClient::new(transport.clone(), request.model.clone());
        let embeddings = EmbeddingClient::new(
            transport,
            request.model.clone(),
            request.embedding_model.clone(),
            EmbeddingOptions::from(&settings.model),
        );
        context.thinking(format!("[worker] Embeddings: {}", request.embedding_model));

        // Ingest: the derived host is checked against the allowlist BEFORE
        // the credential is used. An artifact folder has no git host.
        let admitted = ingest::admit_source(&request.repo_config, &settings.ingest.git_allowlist)?;
        let provider = admitted.provider().to_owned();
        let repository = admitted.repository().to_owned();
        let folder = match &admitted {
            Admitted::Artifact(target) => Some(target.source().url()),
            Admitted::Git(_) => None,
        };
        context.thinking(format!(
            "[worker] Clone config built: {provider} - {repository} @ {}",
            admitted.branch()
        ));
        context.thinking(format!(
            "[worker] Starting wiki generation: {}",
            request.query
        ));
        context.thinking(format!(
            "Getting local repository path for {repository} (branch: {})",
            admitted.branch()
        ));
        let cloned = self.fetch_source(admitted, &objects).await?;
        drop(objects);
        if let Some(folder) = folder {
            context.thinking(format!(
                "Materialised {folder}: {} objects, {} bytes",
                cloned.tree.files, cloned.tree.bytes
            ));
        }
        let commit = cloned.identity.commit().to_owned();
        let branch = cloned.identity.branch().to_owned();
        let repo_identifier = build_repo_identifier(&repository, &branch, Some(&commit));
        let wiki_id = normalize_wiki_id(&repo_identifier);
        context.thinking(format!(
            "Repository cloned (branch: {branch}, commit: {})",
            cloned.identity.short_commit()
        ));
        let root = std::fs::canonicalize(&cloned.path).map_err(|e| {
            runtime(format!(
                "Git clone failed: the checkout cannot be read: {e}"
            ))
        })?;
        let root_text = root
            .to_str()
            .ok_or_else(|| runtime("Git clone failed: the checkout path is not UTF-8"))?
            .to_owned();

        // Phase 1 + 1c.
        context.checkpoint()?;
        let (graph, discovery) = self.build_graph(&root_text).await?;
        context.checkpoint()?;

        // The build: stage, embed.
        let mut space = BuildSpace::new(pool.clone(), settings.build_owner.clone())
            .with_stale_after(settings.build_stale_after)
            .with_publish_settings(settings.publish);
        if let Some(boot_id) = self.job.boot_id {
            space = space.with_boot_id(boot_id);
        }
        let build = space
            .begin(&wiki_id)
            .await
            .map_err(|e| storage_failure("opening a build", &e))?;
        (self.job.on_build)(build.build_id());
        *slot = Some(build);
        context.thinking(format!(
            "Writing unified DB: {} nodes, {} edges",
            graph.node_count(),
            graph.edge_count()
        ));
        let staged = open_build(slot)?
            .stage_graph(&graph)
            .await
            .map_err(|e| storage_failure("staging the graph", &e))?;
        context.thinking(format!(
            "Staged {} nodes, {} edges and {} BM25 documents",
            staged.nodes, staged.edges, staged.bm25_documents
        ));
        context.checkpoint()?;
        context.thinking(format!(
            "Embedding the nodes with {}",
            request.embedding_model
        ));
        let round = settings.model.embed_batch_size * settings.model.embed_concurrency;
        let embedded =
            embed::populate(&graph, &embeddings, open_build(slot)?, round, context).await?;
        context.thinking(format!(
            "Unified DB: {} node embeddings stored{}",
            embedded.stored,
            embedded
                .dimension
                .map(|d| format!(" (dimension {d})"))
                .unwrap_or_default()
        ));

        // Phase 2 on the staged rows, with statistics that describe them.
        context.checkpoint()?;
        open_build(slot)?.refresh_statistics().await;
        let (mut graph, outcome) = self.phase2(graph, &embeddings, slot).await?;
        context.thinking(phase2_summary(&outcome.stats));

        // Phase 3 with the hub list Python passed (the first 20 in id order).
        context.checkpoint()?;
        let flags = Phase3Flags {
            exclude_tests: self.environment.exclude_tests,
            calibrated_weights: self.environment.profile == CalibrationProfile::Calibrated,
        };
        let hubs = outcome.hubs_for_phase3().to_vec();
        let (returned, phase3) = tokio::task::spawn_blocking(move || {
            let cluster_graph = ClusterGraph::from_code_graph(&graph);
            let result = run_phase3(&cluster_graph, &hubs, flags, &mut LeidenPartitioner)
                .map(|output| (output.assignments(&cluster_graph), output.stats));
            (graph, result)
        })
        .await
        .map_err(|e| join_failure(&e))?;
        graph = returned;
        let (assignments, stats) = phase3.map_err(|e| {
            runtime(format!(
                "Repository indexing failed: Phase 3 clustering failed: {e}"
            ))
        })?;
        context.thinking(format!(
            "Phase 3 complete: {} sections, {} pages, {} hubs reintegrated",
            stats.macro_.cluster_count, stats.micro.total_pages, stats.hubs.total
        ));
        let mut staged_clusters = Vec::new();
        for a in assignments
            .iter()
            .filter(|a| a.macro_cluster.is_some() || a.micro_cluster.is_some())
        {
            let column = |value: Option<usize>| {
                value
                    .map(i32::try_from)
                    .transpose()
                    .map_err(|_| runtime(format!("node {}: a cluster id overflows", a.node_id)))
            };
            staged_clusters.push((
                a.node_id.clone(),
                column(a.macro_cluster)?,
                column(a.micro_cluster)?,
            ));
        }
        open_build(slot)?
            .set_clusters(&staged_clusters)
            .await
            .map_err(|e| storage_failure("writing the cluster columns", &e))?;
        drop(staged_clusters);

        // The rows as the index stores them: the types before Phase 2's
        // re-typing (it changed the GRAPH only), the cluster columns.
        for (id, previous) in &outcome.retyped {
            if let Some(node) = graph.node_mut(id) {
                node.symbol_type.clone_from(previous);
            }
        }
        let clusters = cluster_columns(&assignments);
        drop(assignments);
        let planner_index = planner_index(&graph, &clusters, &repo_identifier);
        let facts = GraphFacts::from_graph(&graph);
        let page_index = Arc::new(PageIndex::from_graph(&graph, &clusters, facts));
        drop(clusters);
        drop(graph);
        context.thinking(
            "I am on phase initialization\nIndexing complete – preparing generation agent\nReasoning: Repository artifacts available for higher-level semantic synthesis.\nNext: Dispatch structure + page generation workflow.",
        );

        // analyze_repository → generate_wiki_structure.
        context.checkpoint()?;
        let model = LiveModel {
            client: chat.clone(),
            stop: context.stop_signal(),
        };
        context.thinking(format!("Analyzing repository {repository}"));
        let analysis = analysis::analyze_repository(
            &model,
            &root,
            &discovery,
            &repository,
            &branch,
            self.environment.structure.structured_analysis,
        )
        .await?;
        drop(discovery);
        context.checkpoint()?;
        context.thinking("Planning the wiki structure");
        let spec = structure::plan_wiki_structure(
            &model,
            self.environment.planner,
            &analysis,
            Some(&planner_index),
            &self.environment.structure,
        )
        .await?;
        drop(planner_index);
        context.thinking(format!(
            "Wiki structure planned: {} sections, {} pages",
            spec.sections.len(),
            spec.page_count()
        ));
        let structure = serde_json::from_value(spec.to_value()).map_err(|e| {
            runtime(format!(
                "Wiki generation failed: the structure does not convert: {e}"
            ))
        })?;

        // Pages, export, composition.
        context.checkpoint()?;
        context.thinking(
            "I am on phase page_generation\nDispatching page generation tasks\nReasoning: Parallel drafting accelerates throughput while leveraging shared repository context.\nNext: For each page, retrieve focused context then invoke LLM.",
        );
        let thinking = context.clone();
        let generator = Arc::new(PageGenerator {
            retrieval: RetrievalContext {
                index: Arc::clone(&page_index),
                search: Arc::new(SubstringSearch::new(Arc::clone(&page_index))),
                repo_root: Some(root.clone()),
                flags: ExpansionFlags {
                    exclude_tests: self.environment.exclude_tests,
                },
                budget: CONTEXT_TOKEN_BUDGET,
                pylon: PylonPlugin::new(Some(root.clone())),
            },
            chat,
            settings: PageSettings::new(repository.clone()),
            stop: context.stop_signal(),
            thinking: Some(Arc::new(move |text: String| thinking.thinking(text))),
        });
        let identity = WikiIdentity {
            repository: repository.clone(),
            canonical_repository: repository.clone(),
            branch: branch.clone(),
            commit_hash: Some(commit.clone()),
            provider_type: provider.clone(),
            query: request.query.clone(),
        };
        let pages = generate_wiki_pages(
            generator,
            structure,
            &analysis.repository_context,
            &identity,
            &VersionClock::system(),
            self.started,
        )
        .await?;
        context.thinking(
            "I am on phase completion\nWiki generation workflow finished\nReasoning: All requested phases executed successfully.\nNext: Return artifacts & summaries to caller.",
        );
        if pages.composed.wiki_id != wiki_id {
            return Err(runtime(format!(
                "Wiki generation failed: the composed wiki id {} is not the build's {wiki_id}",
                pages.composed.wiki_id
            )));
        }
        context.thinking(format!("[worker] Wiki ID for folder structure: {wiki_id}"));
        let mut result = pages.composed.result;

        // The publish, before the result line; never for a result the
        // host could not read.
        context.checkpoint()?;
        check_result_size(&mut result, MAX_RESULT_BYTES)?;
        // The critical section: a stop that arrives from here on waits for
        // the publish. A publish that committed reports its result; one
        // that did not reports the stop.
        (self.job.on_publishing)(true);
        let committed = self.publish(&mut result, slot).await;
        (self.job.on_publishing)(false);
        if !committed && context.stop_signal().is_requested() {
            return Err(EngineError::cancelled());
        }
        context.thinking("[worker] Done");
        Ok(Value::Object(result))
    }

    /// The clone, or the folder's download, stopped by the invocation's
    /// stop.
    async fn fetch_source(
        &self,
        admitted: Admitted,
        objects: &Transport,
    ) -> Result<ClonedRepository, EngineError> {
        let cancel = Arc::new(AtomicBool::new(false));
        let stop = self.context.stop_signal();
        let flag = Arc::clone(&cancel);
        let watcher = tokio::spawn(async move {
            stop.stopped().await;
            flag.store(true, Ordering::Release);
        });
        let outcome = match admitted {
            Admitted::Artifact(target) => match self.request.platform_objects.as_ref() {
                Some(platform) => {
                    ingest::ingest_artifact(
                        &target,
                        platform,
                        objects.http_client(),
                        &self.settings.ingest,
                        self.job.scratch,
                        cancel,
                    )
                    .await
                }
                None => Err(value_error(
                    "an artifact source needs the platform artifact credentials, and this invocation has none",
                )),
            },
            Admitted::Git(admitted) => {
                ingest::ingest_admitted(
                    admitted,
                    self.settings.ingest.limits,
                    self.job.scratch,
                    cancel,
                )
                .await
            }
        };
        watcher.abort();
        // A stop during the clone is the stop line, whatever the clone said.
        self.context.checkpoint()?;
        outcome
    }

    /// Discovery, the parsers and the graph, on a blocking thread.
    async fn build_graph(
        &self,
        root: &str,
    ) -> Result<(CodeGraph, discover::Discovery), EngineError> {
        let walk_root = root.to_owned();
        let discovery = tokio::task::spawn_blocking(move || discover::discover_files(&walk_root))
            .await
            .map_err(|e| join_failure(&e))?;
        let files: usize = discovery.files_by_language.values().map(Vec::len).sum();
        self.context
            .thinking(format!("Discovered {files} files after filtering"));
        self.context.thinking(format!(
            "Processing {files} files with Enhanced Unified Graph Builder"
        ));
        let flags = self.environment.phase1c;
        let build_root = root.to_owned();
        let (graph, discovery) = tokio::task::spawn_blocking(move || {
            let built = builder::try_build_index_graph_parsed(&build_root, &discovery, &flags);
            built.map(|(graph, _report, _phase1c)| (graph, discovery))
        })
        .await
        .map_err(|e| join_failure(&e))?
        .map_err(|failure| runtime(format!("Repository indexing failed: {failure}")))?;
        if graph.node_count() == 0 {
            return Err(runtime(
                "No documents found in repository. The repository may be empty, all files may be filtered out, or the branch may not exist.",
            ));
        }
        Ok((graph, discovery))
    }

    /// Phase 2 on a blocking thread, against the staged rows.
    async fn phase2(
        &self,
        graph: CodeGraph,
        embeddings: &EmbeddingClient,
        slot: &mut Option<Build>,
    ) -> Result<(CodeGraph, topology::Phase2Outcome), EngineError> {
        let build = slot
            .take()
            .ok_or_else(|| runtime("Repository indexing failed: the build is no longer held"))?;
        let stop = self.context.stop_signal();
        let handle = Handle::current();
        let store = PgTopologyStore::new(build, handle.clone(), stop.clone());
        let fallback = BlockingEmbedder::new(embeddings.clone(), handle, stop.clone());
        let config = Phase2Config {
            profile: self.environment.profile,
            ..Phase2Config::default()
        };
        let (graph, parts, outcome) = tokio::task::spawn_blocking(move || {
            let mut graph = graph;
            let mut store = store;
            let mut fallback = fallback;
            let outcome =
                topology::run_phase2(&mut graph, &mut store, Some(&mut fallback), &config);
            (graph, store.into_parts(), outcome)
        })
        .await
        .map_err(|e| join_failure(&e))?;
        *slot = Some(parts.build);
        let outcome = outcome
            .map_err(|error| phase2_failure(&error, stop.is_requested(), embeddings.model()))?;
        Ok((graph, outcome))
    }

    /// Publish the build as the wiki's live index. A failure is reported in
    /// band (`errors`), as `LegacyToolRunner._publish` did: the pages and the
    /// manifest are genuine and land; what is lost is answering questions
    /// about the wiki. The build is left for the caller to abandon. Returns
    /// whether the publish committed.
    async fn publish(&self, result: &mut Map<String, Value>, slot: &mut Option<Build>) -> bool {
        let context = self.context;
        context.thinking("Publishing the index for query replicas");
        let registry: Map<String, Value> = [
            "wiki_id",
            "canonical_repo_identifier",
            "provider_type",
            "wiki_title",
            "wiki_description",
            "branch",
            "commit_hash",
            "analysis_key",
            "wiki_version_id",
        ]
        .iter()
        .filter_map(|key| result.get(*key).map(|v| ((*key).to_owned(), v.clone())))
        .collect();
        let record = WikiRecord::from_result(&Value::Object(registry));
        let published = match slot.as_mut() {
            Some(build) => build.publish(&record).await,
            None => Err(StorageError::Publish(
                "the build is no longer held".to_owned(),
            )),
        };
        match published {
            Ok(counts) => {
                slot.take();
                context.thinking(format!(
                    "Published {} nodes and {} vectors",
                    counts.nodes, counts.embeddings
                ));
                true
            }
            Err(error) => {
                tracing::error!(%error, "publishing the index failed");
                let message = format!(
                    "The wiki was generated but could not be published to the index database, so questions about it cannot be answered: {error}"
                );
                match result.get_mut("errors") {
                    Some(Value::Array(errors)) => errors.push(json!(message)),
                    _ => {
                        result.insert("errors".to_owned(), json!([message]));
                    }
                }
                context.thinking("Publishing the index FAILED");
                false
            }
        }
    }
}

/// What the publish can add to the result line (an error in `errors`).
const PUBLISH_MARGIN: usize = 64 * 1024;

/// The largest result line, newline included, that is published: the
/// host's line limit less [`PUBLISH_MARGIN`].
const MAX_RESULT_BYTES: usize = crate::runner::native::MAX_RESULT_LINE - PUBLISH_MARGIN;

/// Refuse a result whose line (`{"result": …}` and its newline) is longer
/// than `limit` bytes: the host could not read it, so the run fails before
/// anything is published.
///
/// # Errors
///
/// A `RuntimeError` naming the size and the limit.
fn check_result_size(result: &mut Map<String, Value>, limit: usize) -> Result<(), EngineError> {
    let line = json!({ "result": Value::Object(std::mem::take(result)) });
    let size = crate::pyjson::dumps(&line).len() + 1;
    if let Value::Object(mut line) = line
        && let Some(Value::Object(back)) = line.remove("result")
    {
        *result = back;
    }
    if size > limit {
        return Err(runtime(format!(
            "The wiki result is too large: its line is {size} bytes and the host reads at most {limit}; nothing was published"
        )));
    }
    Ok(())
}

/// The cluster planner's index: the rows in graph order with the cluster
/// columns, and the edges Phase 2 persisted (the graph's, in its order).
fn planner_index(
    graph: &CodeGraph,
    clusters: &ClusterColumns,
    repo_identifier: &str,
) -> PlannerIndex {
    let nodes = graph
        .nodes()
        .map(|(id, data)| {
            let mut row = node_row(id, data);
            if let Some((section, page)) = clusters.get(id) {
                row.macro_cluster = *section;
                row.micro_cluster = *page;
            }
            structure::index::IndexNode::from_row(&row)
        })
        .collect();
    let edges = graph.edge_rows();
    PlannerIndex::new(nodes, &edges, Some(repo_identifier.to_owned()))
}

/// The job directory a worker gets: `{scratch_root}/jobs/{name}`.
#[must_use]
pub fn job_directory(scratch_root: &Path, name: &str) -> PathBuf {
    scratch_root.join("jobs").join(name)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn a_gateway_failure_in_phase_2_keeps_its_type_and_category() {
        let refusals = [
            EngineError::new(
                ErrorType::Runtime,
                "Embeddings inference refused: the gateway rejected the credential for model 'e' (HTTP 401)",
            ),
            EngineError::new(
                ErrorType::Value,
                "The model budget is exhausted: HTTP 402 for model 'e'",
            ),
            EngineError::new(
                ErrorType::Runtime,
                "Embeddings request for model 'e' hit a timeout after 3 attempt(s)",
            ),
            EngineError::new(
                ErrorType::Runtime,
                "The model service is busy: HTTP 429 for model 'e'",
            ),
        ];
        for cause in refusals {
            let error = topology::StoreError::from_engine(cause.clone());
            let failure = phase2_failure(&error, false, "text-embedding-3-small");
            assert_eq!(failure.error_type, cause.error_type, "{failure}");
            assert_eq!(failure.category(), cause.category(), "{failure}");
            assert!(failure.message.contains(&cause.message), "{failure}");
        }
        // An index failure stays a runtime error; a stop is the stop line.
        let index = topology::StoreError::new("the build space could not answer: x");
        let failure = phase2_failure(&index, false, "m");
        assert_eq!(failure.error_type, ErrorType::Runtime);
        assert!(
            failure
                .message
                .starts_with("Repository indexing failed: Phase 2")
        );
        assert_eq!(
            phase2_failure(
                &topology::StoreError::from_engine(EngineError::new(ErrorType::Value, "x")),
                true,
                "m"
            ),
            EngineError::cancelled()
        );
    }

    #[test]
    fn an_oversized_result_is_refused_before_the_publish() {
        let mut result = Map::new();
        result.insert("result".to_owned(), json!("x".repeat(100)));
        let exact =
            crate::pyjson::dumps(&json!({"result": Value::Object(result.clone())})).len() + 1;
        let kept = result.clone();
        assert_eq!(check_result_size(&mut result, exact), Ok(()));
        assert_eq!(result, kept, "the result is handed back whole");
        let refused = check_result_size(&mut result, exact - 1);
        let Err(error) = refused else {
            panic!("an oversized result passed");
        };
        assert_eq!(error.error_type, ErrorType::Runtime);
        assert!(error.message.contains("nothing was published"), "{error}");
        assert_eq!(result, kept);
        const { assert!(MAX_RESULT_BYTES < crate::runner::native::MAX_RESULT_LINE) };
    }

    #[test]
    fn the_phase2_line_reads_the_stats() {
        let stats = json!({
            "orphan_resolution": {"orphan_count": 9, "orphans_resolved": 7},
            "weighting": {"edges_weighted": 40},
            "component_bridging": {"components_before": 5, "components_after": 1},
            "hubs": {"count": 2, "node_ids": []},
        });
        assert_eq!(
            phase2_summary(&stats),
            "Phase 2 complete: 40 edges weighted, 2 hubs, 7/9 orphans resolved, 5→1 components bridged"
        );
        assert_eq!(
            phase2_summary(&json!({})),
            "Phase 2 complete: 0 edges weighted, 0 hubs, 0/0 orphans resolved, 0→0 components bridged"
        );
    }
}

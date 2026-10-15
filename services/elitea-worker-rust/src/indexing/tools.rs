//! The index tools' host (ADR-0030 I1): `elitea-agent-runtime`'s
//! [`IndexToolHost`] over `elitea-vector-index`, for one claim.
//!
//! The families own the tool names, schemas and the `selected_tools` rule;
//! `elitea-vector-index` owns the SDK search semantics and output shapes.
//! This module is what joins them to a claim: the claim's `elitea-vector`
//! token (the only credential the vector store ever sees), the gateway
//! embeddings route the ingest run also embeds through (the query is
//! embedded with the model the index was stamped with, by its registry
//! name), and — for the step-back tools — a chat model bound on the worker's
//! model path. A bound model is single-use, so one is bound for every model
//! call, inside the call and only once the arguments and the index have been
//! checked ([`LazyStepbackModel::bind`]): a turn that only lists or searches,
//! or whose step-back call is refused before any prompt, never binds one. A host without a model (a direct
//! `toolkit.call_tool.v1` run, which binds no agent model) refuses the two
//! step-back tools with a sentence; the other three work there as in a turn.
//!
//! The catalog a call reads is the toolkit's [`SharedIndexCatalog`] for the
//! run: once Main reports an index removed, `remove_index` takes it out, so
//! the turn's later `list_indexes` and searches no longer see it.
//!
//! `remove_index` never touches the vector store from here: elitea-main is
//! the only writer of the index registry (ADR-0031), so the tool asks Main
//! over the claim's control plane (`RuntimeControlService.RemoveIndex`), and
//! Main checks the run's principal, tombstones the row and deletes the
//! vectors through its own path or its sweeper. The worker's part is to name
//! the index from the frozen catalog and to turn Main's answer into the
//! SDK's sentence.
//!
//! Every failure is a sentence the model reads, never a turn-aborting error.

use std::collections::BTreeMap;
use std::sync::Arc;

use adk_rust::{
    AdkError, Content, ErrorCategory, ErrorComponent, GenerateContentConfig, Llm, Part,
};
use async_trait::async_trait;
use elitea_agent_runtime::toolkits::{
    IndexCatalog as FrozenCatalog, IndexToolCall, IndexToolFailure, IndexToolHost, IndexToolName,
    MemoryScope, MemoryToolCall, MemoryToolName, SharedIndexCatalog,
};
use elitea_vector_index::format::{Doctype, index_not_found_message};
use elitea_vector_index::memory::{
    self, GetMemoryArgs, ManageArgs, MemoryEmbedder, MemoryTools, SearchMemoryArgs, SpaceCache,
};
use elitea_vector_index::{
    Error as VectorError, IndexCatalog, IndexEntry, IndexTools, QueryEmbedder, SearchArgs,
    StepbackModel, VectorSession, pb,
};
use serde_json::Value;

use super::IndexIngestRuntime;
use super::slug::vector_space_slug;
use crate::protocol::control::{IndexRemovalBinding, IndexRemovalOutcome};
use crate::protocol::vector_token::{VectorClaimToken, VectorSource};
use crate::transport::embeddings_facade::{EmbeddingsClient, EmbeddingsError};
use crate::transport::model_facade::ModelFacadeError;
use crate::transport::model_text::{
    AdmittedChat, ModelCallBudget, TextFailure, stream_text, toolkit_budget_exhausted,
};
use crate::transport::runtime_context::ClaimScopedEliteaContext;

/// The most bytes a step-back model reply may hold.
const MAX_MODEL_TEXT_BYTES: usize = 256 * 1024;

/// The chat model the step-back tools call, bound for ONE call: the facade's
/// bound model is single-use (it captures one completion and counts a turn),
/// so every call binds its own ([`LazyStepbackModel::bind`]).
#[derive(Clone)]
pub(crate) struct StepbackBinding {
    /// The bound ADK model.
    pub(crate) model: Arc<dyn Llm>,
    /// The model name the request names (the binding's own).
    pub(crate) model_name: String,
    /// The generation config the model's facade admits
    /// (`BoundModelFacade::admitted_chat`), never one built by hand.
    pub(crate) config: GenerateContentConfig,
    /// The claim's completion budget, shared by every binding of the claim.
    pub(crate) budget: Option<ModelCallBudget>,
}

/// Binds the step-back model for one call.
pub(crate) type StepbackBinder =
    Box<dyn Fn() -> Result<StepbackBinding, ModelFacadeError> + Send + Sync>;

/// How often the same binding failure is logged again.
const BIND_FAILURE_LOG_INTERVAL: std::time::Duration = std::time::Duration::from_mins(1);

/// The step-back tools' chat model for a claim: a way to bind one single-use
/// model per call, never a bound model. Nothing is bound until a model call
/// is actually made, and a run that makes none binds none. A binding that
/// fails refuses that call with a sentence while the other three index tools
/// work; the failure is logged when it is new, and again once a minute while
/// it persists.
pub(crate) struct LazyStepbackModel {
    source: StepbackSource,
    last_failure: std::sync::Mutex<Option<(String, std::time::Instant)>>,
}

enum StepbackSource {
    None,
    /// A binder called afresh for every call.
    Binder(StepbackBinder),
    /// A fixed binding (tests with a model that allows any number of calls).
    #[cfg(test)]
    Fixed(Box<StepbackBinding>),
}

impl LazyStepbackModel {
    fn with_source(source: StepbackSource) -> Self {
        Self {
            source,
            last_failure: std::sync::Mutex::new(None),
        }
    }

    /// A model `binder` binds, once per call, when a call is made.
    #[must_use]
    pub(crate) fn new(binder: StepbackBinder) -> Self {
        Self::with_source(StepbackSource::Binder(binder))
    }

    /// No model at all, and no binding attempt: a run that binds none (a
    /// pipeline node) asks for it and is told so, without the "could not be
    /// bound" warning that only a failed binding deserves.
    #[must_use]
    pub(crate) fn unbound() -> Self {
        Self::with_source(StepbackSource::None)
    }

    /// An already bound model.
    #[cfg(test)]
    #[must_use]
    pub(crate) fn bound(binding: StepbackBinding) -> Self {
        Self::with_source(StepbackSource::Fixed(Box::new(binding)))
    }

    /// Whether the run has a model to bind at all (nothing is bound to find
    /// out).
    pub(crate) fn is_configured(&self) -> bool {
        !matches!(self.source, StepbackSource::None)
    }

    /// A fresh binding for one call; `None` when there is no binder or
    /// binding failed.
    pub(crate) fn bind(&self) -> Option<StepbackBinding> {
        match &self.source {
            StepbackSource::None => None,
            #[cfg(test)]
            StepbackSource::Fixed(binding) => Some(binding.as_ref().clone()),
            StepbackSource::Binder(binder) => match binder() {
                Ok(binding) => Some(binding),
                Err(error) => {
                    self.log_bind_failure(&error.to_string());
                    None
                }
            },
        }
    }

    fn log_bind_failure(&self, error: &str) {
        let mut last = self
            .last_failure
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner);
        let now = std::time::Instant::now();
        let repeat = last.as_ref().is_some_and(|(previous, at)| {
            previous == error && now.duration_since(*at) < BIND_FAILURE_LOG_INTERVAL
        });
        if !repeat {
            *last = Some((error.to_owned(), now));
            tracing::warn!(
                event = "index_tools_stepback_model_unbound",
                error,
                "the step-back model could not be bound; the step-back call is refused"
            );
        }
    }
}

/// An [`IndexToolHost`] for one claim.
pub(crate) struct ClaimIndexToolHost {
    runtime: Arc<IndexIngestRuntime>,
    token: VectorClaimToken,
    context: Arc<ClaimScopedEliteaContext>,
    model: Option<LazyStepbackModel>,
    /// The claim material `remove_index` addresses Main with.
    removal: IndexRemovalBinding,
    /// The embedding dimension this run has seen, so a memory read that
    /// embeds nothing names its one collection.
    memory_space: SpaceCache,
}

impl ClaimIndexToolHost {
    /// A host over `runtime`'s vector client and embeddings route, acting
    /// with the claim's `token` and redeemed `context`; `model` is the
    /// step-back tools' chat model, or `None` where the run binds no model;
    /// `removal` is the claim binding `remove_index` asks Main under.
    #[must_use]
    pub(crate) fn new(
        runtime: Arc<IndexIngestRuntime>,
        token: &VectorClaimToken,
        context: Arc<ClaimScopedEliteaContext>,
        model: Option<LazyStepbackModel>,
        removal: IndexRemovalBinding,
    ) -> Self {
        Self {
            runtime,
            token: token.duplicate(),
            context,
            model,
            removal,
            memory_space: SpaceCache::default(),
        }
    }

    /// `remove_index` through Main (ADR-0030 I1, ADR-0031): the name is
    /// checked against the frozen catalog here, so the model's typo is
    /// answered with the available indexes without a round trip, then Main
    /// runs its delete flow as the run's principal. The vector store is not
    /// touched from the worker. An index Main reports removed (or already
    /// gone) is taken out of the run's `shared` catalog.
    async fn remove_index(
        &self,
        toolkit_id: u64,
        shared: &SharedIndexCatalog,
        catalog: &IndexCatalog,
        index_name: &str,
    ) -> Result<Value, IndexToolFailure> {
        if index_name.trim().is_empty() {
            return Err(IndexToolFailure::new(
                "remove_index needs an index name: removing every index at once is not supported",
            ));
        }
        let Some(entry) = catalog.entry(index_name) else {
            return Err(IndexToolFailure::new(index_not_found_message(
                index_name,
                &catalog.list_indexes(),
            )));
        };
        let Some(control) = self.runtime.removal.as_ref() else {
            return Err(IndexToolFailure::new(
                "remove_index is not available on this worker: it has no control plane to ask elitea-main through",
            ));
        };
        let request = self
            .removal
            .request(toolkit_id, index_name, &entry.namespace_id);
        match control.remove_index(request).await {
            Ok(IndexRemovalOutcome::Removed) => {
                shared.remove(index_name);
                Ok(Value::String(format!(
                    "Index '{index_name}' has been removed from the vector store.\nAvailable indexes: {}",
                    catalog.without(index_name).list_indexes()
                )))
            }
            Ok(IndexRemovalOutcome::NotFound) => {
                // Gone or recreated under this name: either way the frozen
                // entry no longer names a live index.
                shared.remove(index_name);
                Err(IndexToolFailure::new(format!(
                    "Index '{index_name}' no longer exists: it was removed or recreated since this conversation started.\nAvailable indexes: {}",
                    catalog.without(index_name).list_indexes()
                )))
            }
            Ok(IndexRemovalOutcome::Forbidden) => Err(IndexToolFailure::new(format!(
                "you are not allowed to remove index '{index_name}': the run's user lacks the index delete permission in this project"
            ))),
            Ok(IndexRemovalOutcome::ActiveRun) => Err(IndexToolFailure::new(format!(
                "Index '{index_name}' is still being indexed and cannot be removed until that run ends"
            ))),
            Ok(IndexRemovalOutcome::SharedWriteRefused) => Err(IndexToolFailure::new(format!(
                "Index '{index_name}' was not removed: this conversation read personal memory, so it cannot write shared project memory or a shared index"
            ))),
            Err(error) => {
                tracing::warn!(
                    event = "index_tool_remove_index_failed",
                    error = %error,
                    "elitea-main did not complete the index removal"
                );
                Err(IndexToolFailure::new(
                    "elitea-main could not remove the index right now; nothing was deleted",
                ))
            }
        }
    }

    fn catalog(frozen: &FrozenCatalog) -> (IndexCatalog, BTreeMap<String, String>) {
        let mut models = BTreeMap::new();
        let entries = frozen
            .entries()
            .iter()
            .map(|entry| {
                let model_slug = vector_space_slug(&entry.embedding_model);
                models
                    .entry(model_slug.clone())
                    .or_insert_with(|| entry.embedding_model.clone());
                IndexEntry {
                    name: entry.name.clone(),
                    namespace_id: entry.index_id.clone(),
                    space: pb::EmbeddingSpace {
                        model_slug,
                        dimension: entry.embedding_dimension,
                    },
                    // Toolkit indexes are written incrementally, without a
                    // generation (ADR-0030 decision 3): every point of the
                    // namespace is the index.
                    active_generation: None,
                }
            })
            .collect();
        (IndexCatalog::new(entries), models)
    }
}

#[async_trait]
impl IndexToolHost for ClaimIndexToolHost {
    fn serves_memory(&self) -> bool {
        self.runtime.memory_model.is_some() && self.token.allows(VectorSource::Memory)
    }

    /// The `memory` toolkit's tools (worker unit M9) over the claim's vector
    /// token and the gateway embeddings route, with the deployment's memory
    /// embedding model. The project and the owner are the token's, decided by
    /// `elitea-vector`; the store is the toolkit's (its id and its
    /// `namespace` label, hashed by `elitea-vector-index`).
    async fn memory(&self, call: MemoryToolCall<'_>) -> Result<Value, IndexToolFailure> {
        let (prefix, memory_model) = (
            match call.tool {
                MemoryToolName::ManageMemory => "Failed to store memory",
                MemoryToolName::SearchMemory => "Failed to search memory",
                MemoryToolName::GetMemory => "Failed to retrieve memory",
                MemoryToolName::DeleteMemory => "Failed to delete memory",
            },
            self.runtime.memory_model.as_ref(),
        );
        let Some(memory_model) = memory_model else {
            return Err(IndexToolFailure::new(format!(
                "{prefix}: this deployment has no memory embedding model configured"
            )));
        };
        if !self.token.allows(VectorSource::Memory) {
            return Err(IndexToolFailure::new(format!(
                "{prefix}: memory is not available to this run: it carries no vector token, which Main mints only for a run a person started or a schedule owned by one, so a run without one has no memory at all"
            )));
        }
        // Whose memory is the SERVICE's decision: it stamps the owner from
        // the token (the user this run acts as; a scheduled run acts as its
        // owner) and narrows every call by it. All the worker says is which
        // scope the call works in.
        let scope = match call.scope {
            MemoryScope::Project => memory::MemoryScope::Project,
            MemoryScope::Personal => memory::MemoryScope::Personal,
        };
        let invalid = |error: serde_json::Error| {
            IndexToolFailure::new(format!("{prefix}: the tool arguments are invalid: {error}"))
        };
        // The store is the TOOLKIT's: the id is part of the namespace hash and
        // the service stamps and filters by it.
        let Ok(toolkit_id) = i64::try_from(call.toolkit_id) else {
            return Err(IndexToolFailure::new(format!(
                "{prefix}: the memory toolkit's id is out of range"
            )));
        };
        let session = VectorSession::new(&self.runtime.vector, &self.token);
        let embedder = GatewayMemoryEmbedder {
            embeddings: &self.runtime.embeddings,
            context: self.context.as_ref(),
            model: &memory_model.name,
        };
        let tools = MemoryTools::new(
            &session,
            &embedder,
            &memory_model.slug,
            scope,
            toolkit_id,
            call.namespace,
        )
        .with_space_cache(Arc::clone(&self.memory_space));
        let outcome = match call.tool {
            MemoryToolName::ManageMemory => {
                let args = serde_json::from_value::<ManageArgs>(call.arguments.clone())
                    .map_err(invalid)?;
                tools.manage_memory(&args).await
            }
            MemoryToolName::SearchMemory => {
                let args = serde_json::from_value::<SearchMemoryArgs>(call.arguments.clone())
                    .map_err(invalid)?;
                tools.search_memory(&args).await
            }
            MemoryToolName::GetMemory => {
                let args = serde_json::from_value::<GetMemoryArgs>(call.arguments.clone())
                    .map_err(invalid)?;
                tools.get_memory(&args).await
            }
            MemoryToolName::DeleteMemory => {
                #[derive(serde::Deserialize)]
                struct KeyArgs {
                    key: String,
                }
                let KeyArgs { key } =
                    serde_json::from_value(call.arguments.clone()).map_err(invalid)?;
                tools.delete_memory(&key).await
            }
        };
        outcome.map(Value::String).map_err(|error| {
            tracing::warn!(
                event = "memory_tool_failed",
                tool = call.tool.as_str(),
                retryable = error.is_retryable(),
                "a memory tool failed"
            );
            IndexToolFailure::new(format!("{prefix}: {}", memory::describe(&error)))
        })
    }

    async fn call(&self, call: IndexToolCall<'_>) -> Result<Value, IndexToolFailure> {
        if !self.token.allows(VectorSource::ToolkitIndex) {
            return Err(IndexToolFailure::new(
                "the vector store is not available to this run: the claim carries no project token for toolkit indexes",
            ));
        }
        let (catalog, models) = Self::catalog(&call.catalog.snapshot());
        let session = VectorSession::new(&self.runtime.vector, &self.token);
        let embedder = GatewayQueryEmbedder {
            embeddings: &self.runtime.embeddings,
            context: self.context.as_ref(),
            models,
        };
        let tools = IndexTools::new(&session, &embedder, &catalog, Doctype::parse(call.doctype));
        match call.tool {
            IndexToolName::ListIndexes => Ok(Value::String(tools.list_indexes())),
            IndexToolName::SearchIndex => {
                let args = search_args(call.arguments)?;
                tools
                    .search_index(&args)
                    .await
                    .map(elitea_vector_index::SearchOutput::into_value)
                    .map_err(|error| failure(&error))
            }
            IndexToolName::StepbackSearchIndex | IndexToolName::StepbackSummaryIndex => {
                let Some(lazy) = self.model.as_ref().filter(|model| model.is_configured()) else {
                    return Err(IndexToolFailure::new(format!(
                        "{} needs the agent's chat model, which this run does not bind; call it from an agent conversation, or use search_index",
                        call.tool.as_str()
                    )));
                };
                let args = search_args(call.arguments)?;
                // The model is bound lazily, at the first model call, which
                // `elitea-vector-index` makes after the arguments and the
                // index are checked and BEFORE any embedding or vector call:
                // a binding that fails refuses the call with the sentence
                // below, and nothing billed has happened.
                let model = AdkStepbackModel::new(
                    lazy,
                    if call.tool == IndexToolName::StepbackSearchIndex {
                        1
                    } else {
                        2
                    },
                );
                let text = if call.tool == IndexToolName::StepbackSearchIndex {
                    tools.stepback_search_index(&args, &model).await
                } else {
                    tools.stepback_summary_index(&args, &model).await
                };
                if model.bind_failed.load(std::sync::atomic::Ordering::SeqCst) {
                    return Err(IndexToolFailure::new(format!(
                        "{} needs the agent's chat model; use search_index instead. The chat model could not be bound for this call.",
                        call.tool.as_str()
                    )));
                }
                text.map(Value::String).map_err(|error| failure(&error))
            }
            IndexToolName::RemoveIndex => {
                let index_name = match call.arguments.get("index_name") {
                    None | Some(Value::Null) => "",
                    Some(Value::String(name)) => name.as_str(),
                    Some(_) => {
                        return Err(IndexToolFailure::new("index_name must be a string"));
                    }
                };
                self.remove_index(call.toolkit_id, call.catalog, &catalog, index_name)
                    .await
            }
        }
    }
}

fn search_args(arguments: &Value) -> Result<SearchArgs, IndexToolFailure> {
    serde_json::from_value::<SearchArgs>(arguments.clone())
        .map_err(|error| IndexToolFailure::new(format!("the tool arguments are invalid: {error}")))
}

fn failure(error: &VectorError) -> IndexToolFailure {
    IndexToolFailure::new(error.to_string())
}

/// Embeds a query through the gateway with the model an index was stamped
/// with (the registry name behind the space's slug).
struct GatewayQueryEmbedder<'a> {
    embeddings: &'a EmbeddingsClient,
    context: &'a ClaimScopedEliteaContext,
    /// Model slug to the gateway model name, from the catalog.
    models: BTreeMap<String, String>,
}

impl QueryEmbedder for GatewayQueryEmbedder<'_> {
    async fn embed_query(
        &self,
        text: &str,
        space: &pb::EmbeddingSpace,
    ) -> elitea_vector_index::Result<Vec<f32>> {
        let model = self.models.get(&space.model_slug).ok_or_else(|| {
            VectorError::Embedding(format!(
                "no embedding model is known for the index's space {}",
                space.model_slug
            ))
        })?;
        let embedded = self
            .embeddings
            .embed(self.context, model, &[text.to_owned()])
            .await
            .map_err(|error| VectorError::Embedding(embedding_reason(&error)))?;
        embedded
            .vectors
            .into_iter()
            .next()
            .ok_or_else(|| VectorError::Embedding("the gateway returned no vector".to_owned()))
    }
}

/// Embeds memories and memory queries through the gateway with the
/// deployment's memory model.
struct GatewayMemoryEmbedder<'a> {
    embeddings: &'a EmbeddingsClient,
    context: &'a ClaimScopedEliteaContext,
    model: &'a str,
}

impl MemoryEmbedder for GatewayMemoryEmbedder<'_> {
    async fn embed(&self, text: &str) -> elitea_vector_index::Result<Vec<f32>> {
        let embedded = self
            .embeddings
            .embed(self.context, self.model, &[text.to_owned()])
            .await
            .map_err(|error| VectorError::Embedding(embedding_reason(&error)))?;
        embedded
            .vectors
            .into_iter()
            .next()
            .ok_or_else(|| VectorError::Embedding("the gateway returned no vector".to_owned()))
    }
}

fn embedding_reason(error: &EmbeddingsError) -> String {
    match error {
        EmbeddingsError::Refused { code, .. } => format!("the model gateway refused ({code})"),
        other => other.to_string(),
    }
}

/// The step-back model over the bound ADK model: one user message holding
/// the prompt, answered through the shared admitted-request helper
/// ([`stream_text`]).
///
/// Every binding the tool call needs (one per model call: the bound model is
/// single-use) is made at its FIRST model call, which `elitea-vector-index`
/// makes after the arguments and the index are checked and before the query
/// is embedded or the store searched. A failed binding therefore ends the
/// call before any billed work, and is tried once per tool call.
struct AdkStepbackModel<'a> {
    model: &'a LazyStepbackModel,
    /// Model calls this tool makes (search: 1, summary: 2).
    needed: usize,
    /// Bindings made and not yet used; `None` until the first call.
    bound: std::sync::Mutex<Option<Vec<StepbackBinding>>>,
    /// A binding failed during this tool call: the caller refuses it with
    /// its own sentence.
    bind_failed: std::sync::atomic::AtomicBool,
}

impl<'a> AdkStepbackModel<'a> {
    fn new(model: &'a LazyStepbackModel, needed: usize) -> Self {
        Self {
            model,
            needed,
            bound: std::sync::Mutex::new(None),
            bind_failed: std::sync::atomic::AtomicBool::new(false),
        }
    }

    /// The binding for the next model call.
    fn next_binding(&self) -> Option<StepbackBinding> {
        let mut bound = self
            .bound
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner);
        if bound.is_none() {
            let mut all = Vec::with_capacity(self.needed);
            for _ in 0..self.needed {
                all.push(self.model.bind()?);
            }
            // Used front to back.
            all.reverse();
            *bound = Some(all);
        }
        match bound.as_mut().and_then(Vec::pop) {
            Some(binding) => Some(binding),
            // More calls than planned: bind afresh.
            None => self.model.bind(),
        }
    }
}

impl StepbackModel for AdkStepbackModel<'_> {
    async fn invoke(&self, prompt: &str) -> elitea_vector_index::Result<Value> {
        let binding = self.next_binding().ok_or_else(|| {
            self.bind_failed
                .store(true, std::sync::atomic::Ordering::SeqCst);
            VectorError::Model("the step-back model could not be bound".to_owned())
        })?;
        generate_text(
            &binding,
            vec![Part::Text {
                text: prompt.to_owned(),
            }],
        )
        .await
        .map(Value::String)
        .map_err(|error| VectorError::Model(error.message))
    }
}

/// The codes of a reply [`generate_text`] cannot use. Callers classify on
/// them (`office_host::model_failure`), never on the message.
pub(crate) mod reply_codes {
    /// Nothing but reasoning came back; more `max_tokens` cures it.
    pub(crate) const REASONING_ONLY: &str = "model.reply_reasoning_only";
    /// `max_tokens` cut the answer off.
    pub(crate) const LENGTH_CUT: &str = "model.reply_length_cut";
    /// The reply is over the size bound.
    pub(crate) const TOO_LARGE: &str = "model.reply_too_large";
    /// The stream ended without a usable reply (an error frame, another
    /// stop reason, no terminal frame).
    pub(crate) const FAILED: &str = "model.reply_failed";
}

fn reply_error(category: ErrorCategory, code: &'static str, message: String) -> AdkError {
    AdkError::new(ErrorComponent::Model, category, code, message)
}

/// One user message of `parts` on the bound model, and the text of its reply
/// (through [`stream_text`]: streaming, with the config the facade admits;
/// reasoning removed). Shared with the Office tools' model calls (ADR-0032),
/// which send a prompt and images.
///
/// An EMPTY reply is a valid answer (`Ok("")`): an Office or data-analysis
/// prompt may legitimately have nothing to say. A reply that held only
/// reasoning is not an answer and is an error.
///
/// # Errors
///
/// A typed [`AdkError`] with a data-free message: the facade's own, the
/// claim's spent toolkit budget
/// ([`crate::transport::model_text::TOOLKIT_BUDGET_EXHAUSTED_CODE`]), or one
/// of [`reply_codes`].
pub(crate) async fn generate_text(
    binding: &StepbackBinding,
    parts: Vec<Part>,
) -> Result<String, AdkError> {
    let chat = AdmittedChat {
        model: Arc::clone(&binding.model),
        config: binding.config.clone(),
        budget: binding.budget.clone(),
    };
    let user = Content {
        role: "user".to_owned(),
        parts,
    };
    match stream_text(&chat, &binding.model_name, user, MAX_MODEL_TEXT_BYTES).await {
        Ok(reply) if reply.reasoning_only => Err(reply_error(
            ErrorCategory::InvalidInput,
            reply_codes::REASONING_ONLY,
            "the model produced only reasoning and answered nothing (raise `max_tokens`?)"
                .to_owned(),
        )),
        Ok(reply) if reply.length_cut => Err(reply_error(
            ErrorCategory::InvalidInput,
            reply_codes::LENGTH_CUT,
            "the model's answer was cut off by `max_tokens`".to_owned(),
        )),
        Ok(reply) => Ok(reply.text),
        Err(TextFailure::Model(error)) => Err(error),
        Err(TextFailure::BudgetExhausted) => Err(toolkit_budget_exhausted()),
        Err(TextFailure::TooLarge) => Err(reply_error(
            ErrorCategory::InvalidInput,
            reply_codes::TOO_LARGE,
            "the reply exceeds the size bound".to_owned(),
        )),
        Err(TextFailure::ErrorCode) => Err(reply_error(
            ErrorCategory::Unavailable,
            reply_codes::FAILED,
            "the model ended the reply with an error".to_owned(),
        )),
        Err(TextFailure::Stopped(reason)) => Err(reply_error(
            ErrorCategory::Unavailable,
            reply_codes::FAILED,
            format!("the model stopped for {reason:?}"),
        )),
        Err(TextFailure::NoReply) => Err(reply_error(
            ErrorCategory::Unavailable,
            reply_codes::FAILED,
            "the model returned no reply".to_owned(),
        )),
    }
}

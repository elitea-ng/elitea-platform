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
//! model path. That model is bound lazily, on the first step-back call
//! ([`LazyStepbackModel`], review round 2): a turn that only lists or
//! searches never binds a second model. A host without a model (a direct
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
use std::sync::{Arc, OnceLock};

use adk_rust::futures::StreamExt as _;
use adk_rust::{Content, FinishReason, Llm, LlmRequest, Part};
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
use crate::transport::runtime_context::ClaimScopedEliteaContext;

/// The most bytes a step-back model reply may hold.
const MAX_MODEL_TEXT_BYTES: usize = 256 * 1024;

/// The chat model the step-back tools call, bound for this claim.
pub(crate) struct StepbackBinding {
    /// The bound ADK model.
    pub(crate) model: Arc<dyn Llm>,
    /// The model name the request names (the binding's own).
    pub(crate) model_name: String,
}

/// Binds the step-back model when first asked to.
pub(crate) type StepbackBinder =
    Box<dyn Fn() -> Result<StepbackBinding, ModelFacadeError> + Send + Sync>;

/// The step-back tools' chat model, bound on the first step-back call of the
/// run and never for a run that makes none (review round 2). A binding that
/// fails is logged once; the two step-back tools then refuse with a sentence
/// for the rest of the run while the other three tools work.
pub(crate) struct LazyStepbackModel {
    binder: Option<StepbackBinder>,
    bound: OnceLock<Option<StepbackBinding>>,
}

impl LazyStepbackModel {
    /// A model `binder` binds on first use.
    #[must_use]
    pub(crate) fn new(binder: StepbackBinder) -> Self {
        Self {
            binder: Some(binder),
            bound: OnceLock::new(),
        }
    }

    /// No model at all, and no binding attempt: a run that binds none (a
    /// pipeline node) asks for it and is told so, without the "could not be
    /// bound" warning that only a failed binding deserves.
    #[must_use]
    pub(crate) fn unbound() -> Self {
        Self {
            binder: None,
            bound: OnceLock::new(),
        }
    }

    /// An already bound model.
    #[cfg(test)]
    #[must_use]
    pub(crate) fn bound(binding: StepbackBinding) -> Self {
        Self {
            binder: None,
            bound: OnceLock::from(Some(binding)),
        }
    }

    /// The bound model, binding it on the first call; `None` when there is
    /// no binder or binding failed (logged once).
    pub(crate) fn get(&self) -> Option<&StepbackBinding> {
        self.bound
            .get_or_init(|| {
                let binder = self.binder.as_ref()?;
                match binder() {
                    Ok(binding) => Some(binding),
                    Err(error) => {
                        tracing::warn!(
                            event = "index_tools_stepback_model_unbound",
                            error = %error,
                            "the step-back model could not be bound; stepback_search_index and stepback_summary_index will refuse"
                        );
                        None
                    }
                }
            })
            .as_ref()
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
                let Some(binding) = self.model.as_ref().and_then(LazyStepbackModel::get) else {
                    return Err(IndexToolFailure::new(format!(
                        "{} needs the agent's chat model, which this run does not bind; call it from an agent conversation, or use search_index",
                        call.tool.as_str()
                    )));
                };
                let args = search_args(call.arguments)?;
                let model = AdkStepbackModel { binding };
                let text = if call.tool == IndexToolName::StepbackSearchIndex {
                    tools.stepback_search_index(&args, &model).await
                } else {
                    tools.stepback_summary_index(&args, &model).await
                };
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
/// the prompt, the reply's text taken from the one non-streaming response
/// (`generate_content(request, false)`, as the repo's other non-streaming
/// callers do). A streaming call would hand back partial deltas AND a final
/// response carrying the whole text, which a collector would then double.
struct AdkStepbackModel<'a> {
    binding: &'a StepbackBinding,
}

impl StepbackModel for AdkStepbackModel<'_> {
    async fn invoke(&self, prompt: &str) -> elitea_vector_index::Result<Value> {
        generate_text(
            self.binding,
            vec![Part::Text {
                text: prompt.to_owned(),
            }],
        )
        .await
        .map(Value::String)
        .map_err(VectorError::Model)
    }
}

/// One non-streaming user message of `parts` on the bound model, and the text
/// of the one complete reply. The error is a data-free sentence. Shared with
/// the Office tools' model calls (ADR-0032), which send a prompt and images.
pub(crate) async fn generate_text(
    binding: &StepbackBinding,
    parts: Vec<Part>,
) -> Result<String, String> {
    let request = LlmRequest {
        model: binding.model_name.clone(),
        contents: vec![Content {
            role: "user".to_owned(),
            parts,
        }],
        config: None,
        tools: std::collections::HashMap::new(),
        previous_response_id: None,
    };
    let mut responses = binding
        .model
        .generate_content(request, false)
        .await
        .map_err(|error| error.to_string())?;
    let mut text: Option<String> = None;
    while let Some(response) = responses.next().await {
        let response = response.map_err(|error| error.to_string())?;
        if response.partial {
            // Not expected of a non-streaming call; a partial frame is never
            // the reply.
            continue;
        }
        if response.error_code.is_some() {
            return Err("the model ended the reply with an error".to_owned());
        }
        if let Some(reason) = response.finish_reason
            && reason != FinishReason::Stop
        {
            return Err(format!("the model stopped for {reason:?}"));
        }
        let mut reply = String::new();
        if let Some(content) = &response.content {
            for part in &content.parts {
                if let Part::Text { text: piece } = part {
                    if reply.len().saturating_add(piece.len()) > MAX_MODEL_TEXT_BYTES {
                        return Err("the reply exceeds the step-back size bound".to_owned());
                    }
                    reply.push_str(piece);
                }
            }
        }
        // The last complete response is the reply; a non-streaming call
        // yields exactly one.
        text = Some(reply);
    }
    text.ok_or_else(|| "the model returned no reply".to_owned())
}

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
use std::sync::Arc;

use adk_rust::{Content, FinishReason, GenerateContentConfig, Llm, Part};
use async_trait::async_trait;
use elitea_agent_runtime::toolkits::{
    IndexCatalog as FrozenCatalog, IndexToolCall, IndexToolFailure, IndexToolHost, IndexToolName,
    SharedIndexCatalog,
};
use elitea_vector_index::format::{Doctype, index_not_found_message};
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
use crate::transport::model_text::{AdmittedChat, TextFailure, stream_text};
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
}

/// Binds the step-back model for one call.
pub(crate) type StepbackBinder =
    Box<dyn Fn() -> Result<StepbackBinding, ModelFacadeError> + Send + Sync>;

/// The step-back tools' chat model, bound on the first step-back call of the
/// run and never for a run that makes none (review round 2). A binding that
/// fails is logged once; the two step-back tools then refuse with a sentence
/// for the rest of the run while the other three tools work.
pub(crate) struct LazyStepbackModel {
    source: StepbackSource,
    /// The binding failure is logged once per run, not once per call.
    failure_logged: std::sync::atomic::AtomicBool,
}

enum StepbackSource {
    None,
    /// A binder called afresh for every call.
    Binder(StepbackBinder),
    /// A fixed binding (tests with a model that allows any number of calls).
    #[cfg(test)]
    Fixed(StepbackBinding),
}

impl LazyStepbackModel {
    /// A model `binder` binds, once per call, when a call is made.
    #[must_use]
    pub(crate) fn new(binder: StepbackBinder) -> Self {
        Self {
            source: StepbackSource::Binder(binder),
            failure_logged: std::sync::atomic::AtomicBool::new(false),
        }
    }

    /// No model at all, and no binding attempt: a run that binds none (a
    /// pipeline node) asks for it and is told so, without the "could not be
    /// bound" warning that only a failed binding deserves.
    #[must_use]
    pub(crate) fn unbound() -> Self {
        Self {
            source: StepbackSource::None,
            failure_logged: std::sync::atomic::AtomicBool::new(false),
        }
    }

    /// An already bound model.
    #[cfg(test)]
    #[must_use]
    pub(crate) fn bound(binding: StepbackBinding) -> Self {
        Self {
            source: StepbackSource::Fixed(binding),
            failure_logged: std::sync::atomic::AtomicBool::new(false),
        }
    }

    /// A fresh binding for one call; `None` when there is no binder or
    /// binding failed (logged once).
    pub(crate) fn bind(&self) -> Option<StepbackBinding> {
        match &self.source {
            StepbackSource::None => None,
            #[cfg(test)]
            StepbackSource::Fixed(binding) => Some(binding.clone()),
            StepbackSource::Binder(binder) => match binder() {
                Ok(binding) => Some(binding),
                Err(error) => {
                    if !self
                        .failure_logged
                        .swap(true, std::sync::atomic::Ordering::SeqCst)
                    {
                        tracing::warn!(
                            event = "index_tools_stepback_model_unbound",
                            error = %error,
                            "the step-back model could not be bound; stepback_search_index and stepback_summary_index will refuse"
                        );
                    }
                    None
                }
            },
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
                let Some((lazy, first)) = self
                    .model
                    .as_ref()
                    .and_then(|lazy| lazy.bind().map(|first| (lazy, first)))
                else {
                    return Err(IndexToolFailure::new(format!(
                        "{} needs the agent's chat model, which this run does not bind; call it from an agent conversation, or use search_index",
                        call.tool.as_str()
                    )));
                };
                let args = search_args(call.arguments)?;
                let model = AdkStepbackModel {
                    model: lazy,
                    first: std::sync::Mutex::new(Some(first)),
                };
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

fn embedding_reason(error: &EmbeddingsError) -> String {
    match error {
        EmbeddingsError::Refused { code, .. } => format!("the model gateway refused ({code})"),
        other => other.to_string(),
    }
}

/// The step-back model over the bound ADK model: one user message holding
/// the prompt, answered through the shared admitted-request helper
/// ([`stream_text`]).
struct AdkStepbackModel<'a> {
    model: &'a LazyStepbackModel,
    /// The binding made to find out whether the tool can run at all; the
    /// first call uses it, every later call binds its own.
    first: std::sync::Mutex<Option<StepbackBinding>>,
}

impl StepbackModel for AdkStepbackModel<'_> {
    async fn invoke(&self, prompt: &str) -> elitea_vector_index::Result<Value> {
        // A fresh binding for this call: the bound model is single-use.
        let first = self
            .first
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
            .take();
        let binding = first.or_else(|| self.model.bind()).ok_or_else(|| {
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
        .map_err(VectorError::Model)
    }
}

/// One user message of `parts` on the bound model, and the text of its reply
/// (through [`stream_text`]: streaming, with the config the facade admits;
/// reasoning removed). The error is a data-free sentence. Shared with the
/// Office tools' model calls (ADR-0032), which send a prompt and images.
pub(crate) async fn generate_text(
    binding: &StepbackBinding,
    parts: Vec<Part>,
) -> Result<String, String> {
    let chat = AdmittedChat {
        model: Arc::clone(&binding.model),
        config: binding.config.clone(),
    };
    let user = Content {
        role: "user".to_owned(),
        parts,
    };
    match stream_text(&chat, &binding.model_name, user, MAX_MODEL_TEXT_BYTES).await {
        Ok(reply) if reply.length_cut => Err(if reply.reasoning_only {
            "the model spent its max_tokens reasoning and answered nothing".to_owned()
        } else {
            format!("the model stopped for {:?}", FinishReason::MaxTokens)
        }),
        Ok(reply) if reply.reasoning_only => {
            Err("the model's reply held nothing but reasoning".to_owned())
        }
        Ok(reply) => Ok(reply.text),
        Err(TextFailure::Model(error)) => Err(error.to_string()),
        Err(TextFailure::ErrorCode) => Err("the model ended the reply with an error".to_owned()),
        Err(TextFailure::Stopped(reason)) => Err(format!("the model stopped for {reason:?}")),
        Err(TextFailure::TooLarge) => Err("the reply exceeds the step-back size bound".to_owned()),
        Err(TextFailure::NoReply) => Err("the model returned no reply".to_owned()),
    }
}

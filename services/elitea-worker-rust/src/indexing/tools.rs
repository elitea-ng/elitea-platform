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
//! model path. A host without a model (a direct `toolkit.call_tool.v1` run,
//! which binds no agent model) refuses the two step-back tools with a
//! sentence; the other three work there as in a turn.
//!
//! Every failure is a sentence the model reads, never a turn-aborting error.

use std::collections::BTreeMap;
use std::sync::Arc;

use adk_rust::futures::StreamExt as _;
use adk_rust::{Content, FinishReason, Llm, LlmRequest, Part};
use async_trait::async_trait;
use elitea_agent_runtime::toolkits::{
    IndexCatalog as FrozenCatalog, IndexToolCall, IndexToolFailure, IndexToolHost, IndexToolName,
};
use elitea_vector_index::format::Doctype;
use elitea_vector_index::{
    Error as VectorError, IndexCatalog, IndexEntry, IndexTools, QueryEmbedder, SearchArgs,
    StepbackModel, VectorSession, pb,
};
use serde_json::Value;

use super::IndexIngestRuntime;
use super::slug::vector_space_slug;
use crate::protocol::vector_token::{VectorClaimToken, VectorSource};
use crate::transport::embeddings_facade::{EmbeddingsClient, EmbeddingsError};
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

/// An [`IndexToolHost`] for one claim.
pub(crate) struct ClaimIndexToolHost {
    runtime: Arc<IndexIngestRuntime>,
    token: VectorClaimToken,
    context: Arc<ClaimScopedEliteaContext>,
    model: Option<StepbackBinding>,
}

impl ClaimIndexToolHost {
    /// A host over `runtime`'s vector client and embeddings route, acting
    /// with the claim's `token` and redeemed `context`; `model` is the
    /// step-back tools' chat model, or `None` where the run binds no model.
    #[must_use]
    pub(crate) fn new(
        runtime: Arc<IndexIngestRuntime>,
        token: &VectorClaimToken,
        context: Arc<ClaimScopedEliteaContext>,
        model: Option<StepbackBinding>,
    ) -> Self {
        Self {
            runtime,
            token: token.duplicate(),
            context,
            model,
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
        let (catalog, models) = Self::catalog(call.catalog);
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
                let Some(binding) = self.model.as_ref() else {
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
                tools
                    .remove_index(index_name)
                    .await
                    .map(Value::String)
                    .map_err(|error| failure(&error))
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
/// the prompt, the reply's text collected.
struct AdkStepbackModel<'a> {
    binding: &'a StepbackBinding,
}

impl StepbackModel for AdkStepbackModel<'_> {
    async fn invoke(&self, prompt: &str) -> elitea_vector_index::Result<Value> {
        let request = LlmRequest {
            model: self.binding.model_name.clone(),
            contents: vec![Content::new("user").with_text(prompt)],
            config: None,
            tools: std::collections::HashMap::new(),
            previous_response_id: None,
        };
        let mut responses = self
            .binding
            .model
            .generate_content(request, true)
            .await
            .map_err(|error| VectorError::Model(error.to_string()))?;
        let mut text = String::new();
        let mut finished = false;
        while let Some(response) = responses.next().await {
            let response = response.map_err(|error| VectorError::Model(error.to_string()))?;
            if let Some(content) = &response.content {
                for part in &content.parts {
                    if let Part::Text { text: delta } = part {
                        if text.len().saturating_add(delta.len()) > MAX_MODEL_TEXT_BYTES {
                            return Err(VectorError::Model(
                                "the reply exceeds the step-back size bound".to_owned(),
                            ));
                        }
                        text.push_str(delta);
                    }
                }
            }
            if !response.partial {
                if response.error_code.is_some() {
                    return Err(VectorError::Model(
                        "the model ended the reply with an error".to_owned(),
                    ));
                }
                if let Some(reason) = response.finish_reason
                    && reason != FinishReason::Stop
                {
                    return Err(VectorError::Model(format!(
                        "the model stopped for {reason:?}"
                    )));
                }
                finished = true;
            }
        }
        if !finished && text.is_empty() {
            return Err(VectorError::Model("the model returned no reply".to_owned()));
        }
        Ok(Value::String(text))
    }
}

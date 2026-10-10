//! The native runner (`ELITEA_INVENTORY_RUNNER=native`): the engine itself,
//! over the PostgreSQL graph store.
//!
//! Every tool of both families: `run_ingestion` (clone, parsers, model,
//! communities, embeddings — `crate::ingest`), the status tools,
//! `remove_source_entities`, `import_graph` / `export_graph`
//! (`crate::transfer`), `smart_normalize_types` (a model maps the
//! graph's stray types onto the canonical set, written back to the store),
//! `investigate` (`crate::investigate`), and the read tools
//! (`crate::retrieval`). A tool the dispatch does not know is
//! refused by name, never answered empty (a test holds the tables to it).

// The reports are built line by line, as the Python handlers built them.
#![allow(clippy::format_push_string)]

use crate::config::Settings;
use crate::extract::{self, Model};
use crate::graph::Graph;
use crate::ingest::source::Source;
use crate::ingest::{self, ModelOptions, Outcome, RunOptions};
use crate::store::{self, GraphKey, sources};
use crate::tools;
use elitea_engine_core::errors::{EngineError, ErrorType};
use elitea_engine_core::stream::{Context, StopSignal};
use elitea_model_client::chat::ChatClient;
use elitea_model_client::embeddings::{EmbeddingClient, EmbeddingOptions};
use elitea_model_client::settings::ModelSettings;
use elitea_model_client::transport::{Transport, TransportSettings};
use serde_json::{Map, Value, json};
use sqlx::postgres::PgPool;
use std::sync::Arc;
use std::time::Instant;

/// The model-transport settings name for this engine's CA file.
const CA_SETTING: &str = "ELITEA_INVENTORY_CALLBACK_CA_FILE";

/// The window setting named in a context-length refusal.
const EMBEDDING_CTX_SETTING: &str = "ELITEA_INVENTORY_EMBEDDING_CTX_LENGTH";

/// The engine over one store.
#[derive(Debug, Clone)]
pub struct NativeRunner {
    pool: PgPool,
    settings: Arc<Settings>,
    transport: Transport,
    views: Arc<crate::retrieval::ViewCache>,
    /// The ingestion model calls in flight across this process
    /// (`ELITEA_INVENTORY_MODEL_CONCURRENCY`).
    model_calls: Arc<tokio::sync::Semaphore>,
}

fn invalid(message: impl Into<String>) -> EngineError {
    EngineError::new(ErrorType::Value, message.into())
}

/// The stored graph, for a write that must not create one: a graph
/// deleted since the caller planned its work is a refusal (nothing is
/// saved), not an empty graph to write back.
///
/// # Errors
///
/// `FileNotFound` when no graph is stored; `Runtime` on a store failure.
pub async fn load_existing(pool: &PgPool, key: GraphKey) -> Result<Graph, EngineError> {
    match store::load(pool, key).await {
        Ok(Some((graph, _))) => Ok(graph),
        Ok(None) => Err(EngineError::new(
            ErrorType::FileNotFound,
            "no graph is stored for this Inventory toolkit (it was removed meanwhile); nothing was changed",
        )),
        Err(error) => Err(EngineError::new(ErrorType::Runtime, error.to_string())),
    }
}

/// How an `import_graph` failure reaches the caller. A document the
/// import cannot read and a refusal (an ingestion holds the lease, or the
/// graph has native ingestion state and `replace_ingestion_state` is
/// false) are the caller's to correct, so they are `ValueError`s; only a
/// store failure is a `RuntimeError`.
fn import_error(error: crate::transfer::TransferError) -> EngineError {
    use crate::transfer::TransferError;
    match error {
        TransferError::Document(_) | TransferError::Refused(_) => invalid(error.to_string()),
        other => EngineError::new(ErrorType::Runtime, other.to_string()),
    }
}

/// `run_ingestion`'s `full_rebuild`, read leniently: a full rebuild builds
/// the new graph and swaps it in, so turning it on by a `1` or `"yes"` is
/// safe, and reading those as off would silently run incrementally.
fn full_rebuild(params: &Map<String, Value>) -> bool {
    crate::retrieval::lenient_flag(params.get("full_rebuild"))
}

fn text_param<'a>(params: &'a Map<String, Value>, keys: &[&str]) -> Option<&'a str> {
    keys.iter()
        .find_map(|key| params.get(*key).and_then(Value::as_str))
        .map(str::trim)
        .filter(|text| !text.is_empty())
}

/// The caller the host verified (`caller_user_id`, ADR-0028 D3). Absent
/// on an unsigned hop: such a caller sees project-wide documents only.
fn caller_of(arguments: &Map<String, Value>) -> elitea_content_source::Caller {
    elitea_content_source::Caller {
        user_id: arguments.get("caller_user_id").and_then(|id| match id {
            Value::String(text) if !text.is_empty() => Some(text.clone()),
            Value::Number(number) => Some(number.to_string()),
            _ => None,
        }),
        ..elitea_content_source::Caller::default()
    }
}

fn json_format(params: &Map<String, Value>) -> bool {
    text_param(params, &["output_format"]).is_some_and(|f| f.eq_ignore_ascii_case("json"))
}

impl NativeRunner {
    /// A runner over `settings` (which name a database: `Settings` refuses
    /// `native` without one).
    ///
    /// # Errors
    ///
    /// The DSN does not parse, or the CA file cannot be read.
    pub fn new(settings: Settings) -> Result<Self, EngineError> {
        let dsn = settings
            .database_url
            .as_ref()
            .ok_or_else(|| invalid("the native runner needs ELITEA_INVENTORY_DATABASE_URL"))?;
        let pool = store::lazy_pool(dsn.expose(), 8).map_err(|e| invalid(e.to_string()))?;
        let transport = Transport::new(&TransportSettings {
            ca_file: settings.callback_ca_file.clone(),
            ca_file_setting: CA_SETTING,
            user_agent: concat!("elitea-inventory-engine/", env!("CARGO_PKG_VERSION")),
            ..TransportSettings::default()
        })?;
        let model_calls = Arc::new(tokio::sync::Semaphore::new(
            settings
                .model_concurrency
                .clamp(1, tokio::sync::Semaphore::MAX_PERMITS),
        ));
        Ok(Self {
            pool,
            settings: Arc::new(settings),
            transport,
            views: Arc::new(crate::retrieval::ViewCache::default()),
            model_calls,
        })
    }

    /// Run `tool` of the `inventory` / `inventory_search` family.
    ///
    /// # Errors
    ///
    /// An unknown family or tool, a malformed call, a tool not served
    /// natively yet, or the tool's own failure.
    pub async fn run(
        &self,
        tool: &str,
        arguments: &Map<String, Value>,
        context: &Context,
    ) -> Result<Value, EngineError> {
        let family = arguments
            .get("family")
            .and_then(Value::as_str)
            .filter(|name| !name.is_empty())
            .unwrap_or("inventory");
        let Some(table) = tools::family(family) else {
            return Err(invalid(format!(
                "Unknown toolkit: {family}. Expected: inventory or inventory_search"
            )));
        };
        if !table.contains(&tool) {
            let mut available: Vec<&str> = table.to_vec();
            available.sort_unstable();
            return Err(invalid(format!(
                "Unknown tool: {tool}. Available: {}",
                available.join(", ")
            )));
        }
        let empty = Map::new();
        let params = arguments
            .get("params")
            .and_then(Value::as_object)
            .unwrap_or(&empty);
        let key = GraphKey::from_arguments(arguments).map_err(|e| invalid(e.to_string()))?;
        context.thinking(format!("Running {tool}"));
        match tool {
            "run_ingestion" => self.run_ingestion(key, params, context).await,
            "get_sources_status" => self.sources_status(key, params).await,
            "get_ingestion_status" => self.ingestion_status(key, params).await,
            "investigate" => {
                self.investigate(key, params, &caller_of(arguments), context)
                    .await
            }
            "remove_source_entities" => self.remove_source(key, params).await,
            "smart_normalize_types" => self.smart_normalize(key, params, context).await,
            "import_graph" => self.import_graph(key, params, context).await,
            "export_graph" => self.export_graph(key, params, context).await,
            other => {
                self.read(other, family, key, params, &caller_of(arguments))
                    .await
            }
        }
    }

    /// `remove_source_entities`: the source's citations, its contribution
    /// to every edge (an edge only it found goes) and the entities only it
    /// cited go; its status and file hashes too. Under the
    /// ingestion lease, so it cannot interleave with a run.
    ///
    /// The Python handler matched the toolkit id against citations that
    /// carry the source NAME (so it almost never matched), deleted shared
    /// entities outright when it did, and left the source's status behind.
    async fn remove_source(
        &self,
        key: GraphKey,
        params: &Map<String, Value>,
    ) -> Result<Value, EngineError> {
        let store_error =
            |e: crate::store::StoreError| EngineError::new(ErrorType::Runtime, e.to_string());
        let (toolkit_id, name) = Self::source_identity(params)?;
        let Some(_lease) = sources::lease(&self.pool, key).await.map_err(store_error)? else {
            return Err(EngineError::new(
                ErrorType::Runtime,
                "an ingestion of this Inventory toolkit is running; remove the source when it finishes",
            ));
        };
        let mut graph = store::load(&self.pool, key)
            .await
            .map_err(store_error)?
            .map(|(graph, _)| graph)
            .unwrap_or_default();
        let removed = graph.remove_source(&name);
        sources::remove(&self.pool, key, &graph, &toolkit_id, &name)
            .await
            .map_err(store_error)?;
        Ok(crate::retrieval::answer(format!(
            "Removed {removed} entities from toolkit {toolkit_id}"
        )))
    }

    /// `import_graph`: the operator's `import-graph` as a tool. The Go
    /// host read the document from the toolkit's bucket into
    /// `graph_document` (it overwrites any caller value); the import is
    /// [`crate::transfer::import_graph`], under the ingestion lease.
    async fn import_graph(
        &self,
        key: GraphKey,
        params: &Map<String, Value>,
        context: &Context,
    ) -> Result<Value, EngineError> {
        use crate::transfer;
        let Some(text) = params.get("graph_document").and_then(Value::as_str) else {
            return Err(invalid(
                "import_graph needs the graph document the host reads from the toolkit's bucket; run the tool through the platform",
            ));
        };
        let name = text_param(params, &["artifact_name"]).unwrap_or("graph.json");
        context.thinking(format!("Importing {name}"));
        context.checkpoint()?;
        let replace = crate::retrieval::flag(params.get("replace_ingestion_state"));
        let report = transfer::import_graph(&self.pool, key, text, replace)
            .await
            .map_err(import_error)?;
        if json_format(params) {
            return Ok(crate::retrieval::answer(elitea_engine_core::pyjson::dumps(
                &json!({
                    "stored": true,
                    "entities": report.entities,
                    "relations": report.relations,
                    "revision": report.revision,
                    "embeddings_model": report.embeddings_model,
                }),
            )));
        }
        Ok(crate::retrieval::answer(format!(
            "Imported {} entities and {} relations from {name} (revision {}).{}",
            report.entities,
            report.relations,
            report.revision,
            report.embeddings_model.map_or_else(String::new, |model| format!(
                " The graph was embedded with {model}: semantic search needs that embedding model configured."
            ))
        )))
    }

    /// `export_graph`: the stored graph as `graph.json`, returned as an
    /// artifact the host uploads to the toolkit's bucket; the answer is a
    /// summary (counts, revision, size, bucket), never the document.
    async fn export_graph(
        &self,
        key: GraphKey,
        params: &Map<String, Value>,
        context: &Context,
    ) -> Result<Value, EngineError> {
        use crate::transfer::{self, TransferError};
        context.thinking("Exporting the stored graph");
        let report = transfer::export_report(&self.pool, key, &crate::clock::now_iso())
            .await
            .map_err(|error| match error {
                TransferError::NotFound { .. } => {
                    EngineError::new(ErrorType::FileNotFound, error.to_string())
                }
                other => EngineError::new(ErrorType::Runtime, other.to_string()),
            })?;
        if let Some(refusal) =
            transfer::export_size_refusal(&report.document, crate::MAX_EXPORT_DOCUMENT_BYTES)
        {
            return Err(invalid(refusal));
        }
        let (summary, text) = transfer::export_summary(
            &transfer::export_bucket(params),
            report.entities,
            report.relations,
            Some(report.revision),
            report.document.len(),
        );
        let result = if json_format(params) {
            elitea_engine_core::pyjson::dumps(&summary)
        } else {
            text
        };
        // The document goes to the bucket only: the host uploads the
        // artifact and leaves it out of the terminal body (run.ExportTool).
        Ok(json!({
            "success": true,
            "result": result,
            "artifacts": [{"name": transfer::EXPORT_ARTIFACT, "type": "application/json", "data": report.document}],
        }))
    }

    /// `smart_normalize_types` (`crate::retrieval::admin_tools`): the
    /// toolkit's model maps the types that qualify onto the canonical set,
    /// one batch at a time, through a forced call of Python's structured-
    /// output tool; unless `dry_run`, the mapping is applied to the stored
    /// graph under the ingestion lease and saved in one transaction.
    ///
    /// A batch the model cannot answer refuses the run, nothing written.
    async fn smart_normalize(
        &self,
        key: GraphKey,
        params: &Map<String, Value>,
        context: &Context,
    ) -> Result<Value, EngineError> {
        use crate::retrieval::admin_tools::{self as admin, Applied, SmartStep};
        let store_error =
            |e: crate::store::StoreError| EngineError::new(ErrorType::Runtime, e.to_string());
        context.thinking("Loading graph and analyzing types...");
        let stored = self
            .views
            .view(&self.pool, key)
            .await
            .map_err(store_error)?
            .unwrap_or_default();
        let plan = match admin::smart_plan(&stored.graph, params)? {
            SmartStep::Answer(text) => return Ok(crate::retrieval::answer(text)),
            SmartStep::Map(plan) => plan,
        };
        context.thinking(format!(
            "Found {} types to normalize...",
            plan.candidates.len()
        ));
        let model_name = Self::model_name(params).ok_or_else(|| {
            invalid("no LLM model is configured for this Inventory toolkit; set llm_model in the toolkit configuration")
        })?;
        let settings = Self::model_settings(params, &model_name)?;
        let client = ChatClient::new(self.transport.clone(), settings);
        let stop = context.stop_signal();
        let batches: Vec<Vec<String>> = plan.batches().collect();
        let mut mappings = indexmap::IndexMap::new();
        for (index, batch) in batches.iter().enumerate() {
            context.checkpoint()?;
            context.thinking(format!(
                "Processing batch {}/{} ({} types)...",
                index + 1,
                batches.len(),
                batch.len()
            ));
            let pairs = ask_batch(
                &client,
                &stop,
                &model_name,
                batch,
                (index + 1, batches.len()),
            )
            .await?;
            admin::merge_batch(&mut mappings, batch, pairs);
        }
        context.thinking(format!("Generated {} type mappings...", mappings.len()));
        if plan.dry_run {
            return Ok(crate::retrieval::answer(admin::smart_report(
                &plan,
                &mappings,
                None,
                &model_name,
            )));
        }
        context.checkpoint()?;
        context.thinking("Applying type mappings to graph...");
        let Some(_lease) = sources::lease(&self.pool, key).await.map_err(store_error)? else {
            return Err(EngineError::new(
                ErrorType::Runtime,
                "an ingestion of this Inventory toolkit is running; normalise the types when it finishes",
            ));
        };
        // The graph as stored now, not as planned: a run may have saved since.
        let mut graph = load_existing(&self.pool, key).await?;
        let entities_normalized = admin::apply_mappings(&mut graph, &mappings);
        store::save(&self.pool, key, &graph)
            .await
            .map_err(store_error)?;
        let applied = Applied {
            types_after: admin::graph_entity_types(&graph).len(),
            entities_normalized,
        };
        Ok(crate::retrieval::answer(admin::smart_report(
            &plan,
            &mappings,
            Some(applied),
            &model_name,
        )))
    }

    /// The toolkit's chat model: `llm_model` (either spelling), else
    /// `llm_settings.model_name`.
    fn model_name(params: &Map<String, Value>) -> Option<String> {
        text_param(params, &["llm_model", "toolkit_configuration_llm_model"])
            .map(str::to_owned)
            .or_else(|| {
                params
                    .get("llm_settings")
                    .and_then(|s| s.get("model_name"))
                    .and_then(Value::as_str)
                    .filter(|m| !m.is_empty())
                    .map(str::to_owned)
            })
    }

    /// The source a `remove_source_entities` call names: the expanded
    /// `source` object (the facade's) as `(status key, citation name)`, else
    /// a bare `toolkit_id` / `source_toolkit`, which names both.
    fn source_identity(params: &Map<String, Value>) -> Result<(String, String), EngineError> {
        if let Some(source) = params.get("source").and_then(Value::as_object) {
            let id = match source.get("toolkit_id") {
                Some(Value::String(text)) => text.clone(),
                Some(other) if !other.is_null() => other.to_string(),
                _ => String::new(),
            };
            let name = source
                .get("name")
                .and_then(Value::as_str)
                .map(str::trim)
                .filter(|name| !name.is_empty())
                .map_or_else(|| format!("toolkit_{id}"), str::to_owned);
            return Ok((id, name));
        }
        let id = text_param(params, &["toolkit_id", "source_toolkit"])
            .ok_or_else(|| invalid("remove_source_entities needs the source toolkit"))?
            .to_owned();
        Ok((id.clone(), id))
    }

    /// `semantic_search` for an investigation, when the graph has vectors:
    /// the query is embedded with the model the graph was built with, so
    /// the two compare (in that model's query format,
    /// [`crate::embed::query_text`]), and PostgreSQL ranks the entities
    /// (pgvector's cosine distance, which is scale-free: unnormalised
    /// vectors rank as normalised ones).
    fn ranker(
        &self,
        view: &crate::retrieval::view::GraphView,
        settings: &ModelSettings,
        key: GraphKey,
        stop: &StopSignal,
    ) -> Option<crate::investigate::Embed> {
        crate::retrieval::semantic::stamped_model(view)
            .filter(|_| crate::retrieval::semantic::has_embeddings(view))
            .map(|model| {
                let client = EmbeddingClient::new(
                    self.transport.clone(),
                    settings.clone(),
                    model,
                    EmbeddingOptions {
                        ctx_setting: EMBEDDING_CTX_SETTING,
                        ..EmbeddingOptions::default()
                    },
                );
                let (stop, pool) = (stop.clone(), self.pool.clone());
                let embed: crate::investigate::Embed = Arc::new(move |query: String| {
                    let (client, stop, pool) = (client.clone(), stop.clone(), pool.clone());
                    Box::pin(async move {
                        let text = crate::embed::query_text(client.model(), &query);
                        let vector: Vec<f64> = client
                            .embed_query(&text, &stop)
                            .await?
                            .into_iter()
                            .map(f64::from)
                            .collect();
                        store::vectors::rank(
                            &pool,
                            key,
                            &vector,
                            crate::retrieval::semantic::DEFAULT_MIN_SCORE,
                        )
                        .await
                        .map_err(|e| {
                            EngineError::new(
                                ErrorType::Runtime,
                                format!("the graph store failed: {e}"),
                            )
                        })
                    })
                });
                embed
            })
    }

    /// The investigation's model: the toolkit's, through the gateway, and
    /// its summarisation policy (the client reports usage, so the count
    /// scales).
    fn chat_model(
        &self,
        settings: &ModelSettings,
        stop: &StopSignal,
    ) -> crate::investigate::ChatModel {
        let client = Arc::new(ChatClient::new(self.transport.clone(), settings.clone()));
        let stop = stop.clone();
        crate::investigate::ChatModel {
            name: settings.model_name.clone(),
            chat: Arc::new(move |request| {
                let (client, stop) = (Arc::clone(&client), stop.clone());
                Box::pin(async move { client.complete(&request, &stop).await })
            }),
            policy: elitea_conversation::Policy::for_model(
                &settings.model_name,
                settings.provider == elitea_model_client::settings::Provider::Anthropic,
                true,
            ),
        }
    }

    /// `investigate`: the model agent (`crate::investigate`) over the
    /// graph's view and its source toolkits.
    async fn investigate(
        &self,
        key: GraphKey,
        params: &Map<String, Value>,
        caller: &elitea_content_source::Caller,
        context: &Context,
    ) -> Result<Value, EngineError> {
        use crate::investigate::{self as agent, Question, SourceToolkit};
        let Some(question) = Question::from_params(params) else {
            return Ok(crate::retrieval::answer(agent::missing_question()));
        };
        let model_name = Self::model_name(params).ok_or_else(|| {
                invalid("no LLM model is configured for this Inventory toolkit; set llm_model in the toolkit configuration")
            })?;
        let settings = Self::model_settings(params, &model_name)?;
        let stored = self
            .views
            .view(&self.pool, key)
            .await
            .map_err(|e| EngineError::new(ErrorType::Runtime, e.to_string()))?
            .unwrap_or_default();
        // The agent reads only what the caller may (ADR-0028 D3).
        let view = stored.for_caller(caller).map_or(stored, Arc::new);
        let document = sources::status_document(&self.pool, key)
            .await
            .map_err(|e| EngineError::new(ErrorType::Runtime, e.to_string()))?;
        let toolkits: Vec<SourceToolkit> = document["sources"]
            .as_object()
            .into_iter()
            .flat_map(|rows| rows.values())
            .map(|row| SourceToolkit {
                toolkit_id: row["toolkit_id"].as_str().unwrap_or_default().to_owned(),
                name: row["toolkit_name"].as_str().unwrap_or_default().to_owned(),
                kind: row["toolkit_type"].as_str().unwrap_or_default().to_owned(),
            })
            .collect();
        let stop = context.stop_signal();
        let call_source = source_caller(
            self.transport.http_client().clone(),
            &settings,
            key.project_id,
        );
        let embed = self.ranker(&view, &settings, key, &stop);
        let model = self.chat_model(&settings, &stop);
        let result = agent::investigate(
            &question,
            view,
            &model,
            &toolkits,
            &call_source,
            embed.as_ref(),
            &stop,
        )
        .await?;
        Ok(crate::retrieval::answer(agent::report(
            &result,
            json_format(params),
        )))
    }

    /// A read tool over the graph's current view (an empty graph when the
    /// toolkit has none yet, as the Python wrapper started one).
    async fn read(
        &self,
        tool: &str,
        family: &str,
        key: GraphKey,
        params: &Map<String, Value>,
        caller: &elitea_content_source::Caller,
    ) -> Result<Value, EngineError> {
        let stored = self
            .views
            .view(&self.pool, key)
            .await
            .map_err(|e| EngineError::new(ErrorType::Runtime, e.to_string()))?
            .unwrap_or_default();
        let filtered = stored.for_caller(caller);
        let call = crate::retrieval::Call {
            tool,
            family,
            params,
            view: filtered.as_ref().unwrap_or(&stored),
        };
        crate::retrieval::dispatch(&call).unwrap_or_else(|| {
            Err(EngineError::new(
                ErrorType::FileNotFound,
                format!(
                    "'{tool}' has no handler in the native Inventory engine; this is a defect of the engine's tool table, not of the request"
                ),
            ))
        })
    }

    fn model_settings(
        params: &Map<String, Value>,
        model_name: &str,
    ) -> Result<ModelSettings, EngineError> {
        let mut llm_settings = params
            .get("llm_settings")
            .and_then(Value::as_object)
            .cloned()
            .unwrap_or_default();
        llm_settings.insert("model_name".to_owned(), json!(model_name));
        // The extractors make one blocking call each (ChatOpenAI's default).
        llm_settings.insert("streaming".to_owned(), json!(false));
        if !llm_settings.contains_key("organization") {
            for alias in ["openai_organization", "project_id"] {
                if let Some(project) = llm_settings.get(alias).cloned().filter(|v| !v.is_null()) {
                    llm_settings.insert("organization".to_owned(), project);
                    break;
                }
            }
        }
        ModelSettings::from_llm_settings(&Value::Object(llm_settings))
    }

    async fn run_ingestion(
        &self,
        key: GraphKey,
        params: &Map<String, Value>,
        context: &Context,
    ) -> Result<Value, EngineError> {
        let started = Instant::now();
        let source = Source::parse(params.get("source"), &self.settings.source_types)?;
        let llm_model = text_param(params, &["llm_model", "toolkit_configuration_llm_model"])
            .ok_or_else(|| {
                invalid("no LLM model is configured for this Inventory toolkit; set llm_model in the toolkit configuration")
            })?;
        let chat_settings = Self::model_settings(params, llm_model)?;
        let model: Model = extract::gateway_model(
            ChatClient::new(self.transport.clone(), chat_settings.clone()),
            context.stop_signal(),
        )
        .limited(Arc::clone(&self.model_calls), context.stop_signal());
        let embeddings = text_param(
            params,
            &["embedding_model", "toolkit_configuration_embedding_model"],
        )
        .map(|name| {
            context.thinking(format!("Embedding entities with {name}"));
            EmbeddingClient::new(
                self.transport.clone(),
                chat_settings.clone(),
                name,
                EmbeddingOptions {
                    ctx_setting: EMBEDDING_CTX_SETTING,
                    ..EmbeddingOptions::default()
                },
            )
        });
        let options = RunOptions {
            model: Some(ModelOptions::new(model)),
            embeddings,
            full_rebuild: full_rebuild(params),
        };
        let outcome = ingest::run(
            &self.pool,
            key,
            &source,
            &self.settings.ingest,
            &options,
            context,
        )
        .await;
        if outcome
            .as_ref()
            .is_err_and(|e| *e == EngineError::cancelled())
        {
            return outcome.map(|_| Value::Null);
        }
        let seconds = started.elapsed().as_secs_f64();
        Ok(json!({
            "success": true,
            "result": report(&source.name, source.kind.name(), outcome.as_ref(), seconds, json_format(params)),
            "artifacts": [],
        }))
    }

    async fn sources_status(
        &self,
        key: GraphKey,
        params: &Map<String, Value>,
    ) -> Result<Value, EngineError> {
        let document = sources::status_document(&self.pool, key)
            .await
            .map_err(|e| EngineError::new(ErrorType::Runtime, e.to_string()))?;
        let summary = status_summary(&document);
        let result = if json_format(params) {
            elitea_engine_core::pyjson::dumps(&summary)
        } else {
            status_text(&summary)
        };
        Ok(json!({"success": true, "result": result}))
    }

    async fn ingestion_status(
        &self,
        key: GraphKey,
        params: &Map<String, Value>,
    ) -> Result<Value, EngineError> {
        let document = sources::status_document(&self.pool, key)
            .await
            .map_err(|e| EngineError::new(ErrorType::Runtime, e.to_string()))?;
        let running: Vec<&Value> = document["sources"]
            .as_object()
            .map(|sources| {
                sources
                    .values()
                    .filter(|s| s["status"] == json!("in_progress"))
                    .collect()
            })
            .unwrap_or_default();
        let current = running.first().map(|source| {
            json!({
                "task_id": format!("{}/{}/{}", key.project_id, key.application_id, source["toolkit_id"].as_str().unwrap_or_default()),
                "project_id": key.project_id,
                "application_id": key.application_id,
                "toolkit_id": source["toolkit_id"],
                "toolkit_name": source["toolkit_name"],
                "started_at": source["started_at"],
                "progress_message": source["progress_message"],
            })
        });
        // One ingestion per graph (the lease): that is the slot count.
        let active = usize::from(current.is_some());
        let result = json!({
            "has_active_ingestion": current.is_some(),
            "current_ingestion": current,
            "max_parallel": 1,
            "active_count": active,
            "available_slots": 1 - active,
            "all_active_ingestions": current.iter().collect::<Vec<_>>(),
        });
        let text = if json_format(params) {
            elitea_engine_core::pyjson::dumps(&result)
        } else if let Some(current) = &result["current_ingestion"].as_object() {
            let field = |k: &str| {
                current
                    .get(k)
                    .and_then(Value::as_str)
                    .unwrap_or("unknown")
                    .to_owned()
            };
            let mut out = format!(
                "# Active Ingestion\n\n**Task ID:** {}\n**Source Toolkit:** {}\n**Started:** {}\n",
                field("task_id"),
                field("toolkit_id"),
                field("started_at")
            );
            if let Some(progress) = current.get("progress_message").and_then(Value::as_str) {
                out.push_str(&format!("**Progress:** {progress}\n"));
            }
            out.push_str(&format!("\nSlots: {active}/1 in use\n"));
            out
        } else {
            format!("No active ingestion for this toolkit.\n\nSlots: {active}/1 in use\n")
        };
        Ok(json!({"success": true, "result": text}))
    }
}

/// The `test_tool` route as a source-tool caller: `POST
/// {platform}/api/v2/elitea_core/test_tool/prompt_lib/{project}/{toolkit}`
/// with the invocation's bearer. The platform reloads the toolkit's own
/// settings and credentials and applies the user's permissions; the engine
/// sends only the tool and its arguments. Exactly `request_id`,
/// `tool_name`, `tool_params` and `toolkit_config.toolkit_id`: the
/// platform's investigate grant (`material.SourceToolGate`) admits that
/// shape and nothing more, so no model choice (`llm_model`) rides on it.
fn source_caller(
    client: reqwest::Client,
    settings: &ModelSettings,
    project_id: i64,
) -> crate::investigate::SourceCall {
    let platform = settings
        .api_base
        .trim_end_matches('/')
        .trim_end_matches("/v1")
        .trim_end_matches("/llm")
        .to_owned();
    let bearer = settings.api_key.expose().to_owned();
    Arc::new(
        move |toolkit_id: String, tool: String, arguments: Map<String, Value>| {
            let url = format!(
                "{platform}/api/v2/elitea_core/test_tool/prompt_lib/{project_id}/{toolkit_id}"
            );
            let (client, bearer) = (client.clone(), bearer.clone());
            Box::pin(async move {
                let body = json!({
                    "request_id": format!("investigate-{toolkit_id}-{tool}"),
                    "tool_name": tool,
                    "tool_params": arguments,
                    "toolkit_config": {"toolkit_id": toolkit_id},
                });
                let response = client
                    .post(&url)
                    .bearer_auth(bearer)
                    .json(&body)
                    .timeout(std::time::Duration::from_secs(90))
                    .send()
                    .await
                    .map_err(|e| {
                        EngineError::new(
                            ErrorType::Runtime,
                            format!("the source tool call failed: {e}"),
                        )
                    })?;
                let status = response.status().as_u16();
                let answer: Value = response.json().await.unwrap_or(Value::Null);
                if answer["ok"] == json!(true) {
                    return Ok(match &answer["result"] {
                        Value::String(text) => text.clone(),
                        Value::Null if answer["truncated"] == json!(true) => {
                            "The tool's answer was too large to return.".to_owned()
                        }
                        other => elitea_engine_core::pyjson::dumps(other),
                    });
                }
                let reason = answer["error"].as_str().unwrap_or("no reason given");
                Err(EngineError::new(
                    ErrorType::Runtime,
                    format!("the source tool answered {status}: {reason}"),
                ))
            })
        },
    )
}

/// One smart-normalisation batch through the model: Python's prompt, its
/// structured-output tool forced, the reply as `(original, canonical)`
/// pairs. `position` is `(batch, of)`, for the refusal.
async fn ask_batch(
    client: &ChatClient,
    stop: &StopSignal,
    model_name: &str,
    batch: &[String],
    position: (usize, usize),
) -> Result<Vec<(String, String)>, EngineError> {
    use crate::retrieval::admin_tools as admin;
    use elitea_model_client::chat::{
        ChatMessage, ChatRequest, Sampling, ToolChoice, ToolDefinition,
    };
    let (tool_name, description, schema) = admin::mapping_tool();
    let mut request = ChatRequest::new(vec![ChatMessage::User(admin::smart_prompt(batch))]);
    request.tools = vec![ToolDefinition {
        name: tool_name.to_owned(),
        description: description.to_owned(),
        parameters: schema,
    }];
    request.tool_choice = Some(ToolChoice::Function(tool_name.to_owned()));
    request.sampling = Sampling::Deterministic;
    request.max_tokens = Some(4096);
    let refuse = |reason: String| {
        EngineError::new(
            ErrorType::Runtime,
            format!(
                "smart_normalize_types: batch {} of {} got no usable type mapping from {model_name} ({reason}); nothing was changed",
                position.0, position.1
            ),
        )
    };
    let response = client.complete(&request, stop).await.map_err(|e| {
        if e == EngineError::cancelled() {
            e
        } else {
            refuse(e.message)
        }
    })?;
    let reply = mapping_reply(&response).map_err(&refuse)?;
    admin::mappings_from_reply(&reply).map_err(&refuse)
}

/// A smart-normalisation batch's reply: the forced tool call's arguments,
/// or, from a model that answered in text instead, that text as JSON (a
/// fenced block is unwrapped).
fn mapping_reply(response: &elitea_model_client::chat::ChatResponse) -> Result<Value, String> {
    use crate::retrieval::admin_tools::MAPPING_TOOL;
    if let Some(call) = response
        .tool_calls
        .iter()
        .find(|call| call.name == MAPPING_TOOL)
    {
        return call
            .parsed_arguments()
            .map(Value::Object)
            .map_err(|e| e.message);
    }
    let text = response.content.trim();
    let text = text
        .strip_prefix("```json")
        .or_else(|| text.strip_prefix("```"))
        .and_then(|rest| rest.trim_end().strip_suffix("```"))
        .unwrap_or(text);
    serde_json::from_str::<Value>(text.trim())
        .ok()
        .filter(Value::is_object)
        .ok_or_else(|| {
            "the answer is neither a TypeMappingResponse call nor a JSON object".to_owned()
        })
}

/// `_ingestion_report`, from the run's outcome (or its failure).
fn report(
    name: &str,
    kind: &str,
    outcome: Result<&Outcome, &EngineError>,
    seconds: f64,
    as_json: bool,
) -> String {
    let errors: Vec<String> = match outcome {
        Ok(outcome) => outcome.parse_errors.clone(),
        Err(error) => vec![error.message.clone()],
    };
    // `IngestionResult.source`: the source name the pipeline ran.
    let _ = kind;
    let source = name.to_owned();
    if as_json {
        let empty = Outcome::default();
        let counts = outcome.unwrap_or(&empty);
        return elitea_engine_core::pyjson::dumps(&json!({
            "success": outcome.is_ok(),
            "source": source,
            "documents_processed": counts.documents_processed,
            // Python's keys: this run's extraction, merges included.
            "entities_added": counts.entities_added,
            "relations_added": counts.relations_added,
            // What the graph holds of the source (get_sources_status).
            "entities_in_graph": counts.entities_stored,
            "relations_in_graph": counts.relations_stored,
            "errors": errors.iter().take(10).collect::<Vec<_>>(),
            "duration_seconds": seconds,
        }));
    }
    let Ok(outcome) = outcome else {
        let mut lines = vec![format!("Ingestion failed for {name}"), String::new()];
        lines.push("Errors:".to_owned());
        lines.extend(errors.iter().take(10).map(|e| format!("- {e}")));
        return lines.join("\n");
    };
    // Entities and Relations are the stored graph's (what get_stats and
    // get_sources_status count); the extraction line is this run's add
    // calls, before duplicates merged and the quality pass pruned.
    let mut output = format!(
        "# Ingestion Complete: {name}\n\n**Source:** {source}\n**Documents:** {}\n**Entities:** {}\n**Relations:** {}\n**Extracted this run:** {} entities, {} relations (before duplicates merged)\n**Duration:** {seconds:.1}s\n",
        outcome.documents_processed,
        outcome.entities_stored,
        outcome.relations_stored,
        outcome.entities_added,
        outcome.relations_added
    );
    if !errors.is_empty() {
        output.push_str(&format!("\n**Warnings/Errors ({}):**\n", errors.len()));
        for error in errors.iter().take(5) {
            output.push_str(&format!("- {error}\n"));
        }
        if errors.len() > 5 {
            output.push_str(&format!("... and {} more\n", errors.len() - 5));
        }
    }
    output
}

/// `SourceStatusManager.get_status_summary` over the stored document.
fn status_summary(document: &Value) -> Value {
    let sources: Vec<Value> = document["sources"]
        .as_object()
        .map(|sources| sources.values().cloned().collect())
        .unwrap_or_default();
    let mut counts = Map::new();
    for state in ["pending", "in_progress", "completed", "error"] {
        counts.insert(state.to_owned(), json!(0));
    }
    let (mut entities, mut relations) = (0i64, 0i64);
    for source in &sources {
        let state = source["status"].as_str().unwrap_or("pending");
        if let Some(Value::Number(n)) = counts.get(state) {
            let next = n.as_i64().unwrap_or(0) + 1;
            counts.insert(state.to_owned(), json!(next));
        }
        entities += source["entities_count"].as_i64().unwrap_or(0);
        relations += source["relations_count"].as_i64().unwrap_or(0);
    }
    json!({
        "total_sources": sources.len(),
        "status_counts": counts,
        "total_entities": entities,
        "total_relations": relations,
        "sources": sources,
        "last_modified": document["last_modified"],
    })
}

/// The text `_tool_get_sources_status` writes.
fn status_text(summary: &Value) -> String {
    if summary["total_sources"] == json!(0) {
        return "No sources have been ingested yet. Use run_ingestion to add data from a toolkit."
            .to_owned();
    }
    let count = |state: &str| summary["status_counts"][state].as_i64().unwrap_or(0);
    let mut out = format!("# Source Status ({} sources)\n\n", summary["total_sources"]);
    out.push_str(&format!(
        "**Completed:** {} | **In Progress:** {} | **Error:** {} | **Pending:** {}\n\n",
        count("completed"),
        count("in_progress"),
        count("error"),
        count("pending")
    ));
    out.push_str(&format!(
        "**Total Entities:** {} | **Total Relations:** {}\n\n## Sources\n\n",
        summary["total_entities"], summary["total_relations"]
    ));
    for source in summary["sources"].as_array().into_iter().flatten() {
        let text = |key: &str, default: &str| match &source[key] {
            Value::String(s) => s.clone(),
            Value::Null => default.to_owned(),
            other => other.to_string(),
        };
        let icon = match source["status"].as_str().unwrap_or("pending") {
            "completed" => "OK",
            "in_progress" => "...",
            "error" => "ERR",
            "pending" => "---",
            _ => "?",
        };
        out.push_str(&format!(
            "- [{icon}] **{}** (ID: {}, Type: {})\n  - Status: {}\n  - Last Updated: {}\n  - Entities: {}, Relations: {}\n",
            text("toolkit_name", "Unknown"),
            text("toolkit_id", "?"),
            text("toolkit_type", "?"),
            text("status", "unknown"),
            text("last_updated", "never"),
            text("entities_count", "0"),
            text("relations_count", "0"),
        ));
        if let Some(error) = source["error_message"].as_str().filter(|e| !e.is_empty()) {
            out.push_str(&format!("  - Error: {error}\n"));
        }
        if let Some(branch) = source["branch"].as_str().filter(|b| !b.is_empty()) {
            out.push_str(&format!("  - Branch: {branch}\n"));
        }
        out.push('\n');
    }
    out
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn full_rebuild_is_lenient_and_replace_ingestion_state_is_strict() {
        for on in [
            json!(true),
            json!(1),
            json!(2.5),
            json!("true"),
            json!(" TRUE "),
            json!("1"),
            json!("yes"),
            json!("On"),
        ] {
            let params = json!({ "full_rebuild": on });
            let params = params.as_object().cloned().unwrap_or_default();
            assert!(full_rebuild(&params), "{params:?}");
        }
        for off in [
            json!(false),
            json!(0),
            json!(0.0),
            json!("false"),
            json!("0"),
            json!("no"),
            json!("off"),
            json!(""),
            json!(null),
            json!([]),
            json!({}),
        ] {
            let params = json!({"full_rebuild": off});
            let params = params.as_object().cloned().unwrap_or_default();
            assert!(!full_rebuild(&params), "{params:?}");
        }
        assert!(!full_rebuild(&Map::new()));
        // The destructive flag stays strict: 1 and "yes" do not delete state.
        for value in [json!(1), json!("1"), json!("yes"), json!("on")] {
            assert!(!crate::retrieval::flag(Some(&value)), "{value}");
        }
    }

    #[test]
    fn import_refusals_are_the_callers_to_correct() {
        use crate::transfer::TransferError;
        for (error, wanted) in [
            (TransferError::Document("bad".into()), ErrorType::Value),
            (
                TransferError::Refused("an ingestion of this Inventory toolkit is running".into()),
                ErrorType::Value,
            ),
            (
                TransferError::Refused("already has native ingestion state".into()),
                ErrorType::Value,
            ),
            (
                TransferError::NotFound {
                    project_id: 1,
                    application_id: 2,
                },
                ErrorType::Runtime,
            ),
        ] {
            let mapped = import_error(error);
            assert_eq!(mapped.error_type, wanted, "{}", mapped.message);
        }
    }

    /// The tools `run` answers itself, before the read dispatch.
    const RUN_TOOLS: [&str; 8] = [
        "import_graph",
        "export_graph",
        "run_ingestion",
        "get_sources_status",
        "get_ingestion_status",
        "investigate",
        "remove_source_entities",
        "smart_normalize_types",
    ];

    #[test]
    fn every_admitted_tool_is_served_natively() {
        let view = crate::retrieval::view::GraphView::default();
        let params = Map::new();
        for (family, table) in [
            ("inventory", &tools::INVENTORY_TOOLS[..]),
            ("inventory_search", &tools::SEARCH_TOOLS[..]),
        ] {
            for tool in table {
                let call = crate::retrieval::Call {
                    tool,
                    family,
                    params: &params,
                    view: &view,
                };
                assert!(
                    RUN_TOOLS.contains(tool) || crate::retrieval::dispatch(&call).is_some(),
                    "{family}/{tool} would be refused as not native yet"
                );
            }
        }
    }

    #[test]
    fn a_mapping_reply_is_the_tool_call_or_json_text() {
        use elitea_model_client::chat::{ChatResponse, ToolCall};
        let call = ChatResponse {
            content: "ignored".to_owned(),
            tool_calls: vec![ToolCall {
                id: "c1".to_owned(),
                name: "TypeMappingResponse".to_owned(),
                arguments: r#"{"mappings": []}"#.to_owned(),
            }],
            ..ChatResponse::default()
        };
        assert_eq!(mapping_reply(&call), Ok(json!({"mappings": []})));
        let fenced = ChatResponse {
            content: "```json\n{\"mappings\": [1]}\n```".to_owned(),
            ..ChatResponse::default()
        };
        assert_eq!(mapping_reply(&fenced), Ok(json!({"mappings": [1]})));
        let prose = ChatResponse {
            content: "I think widget is a fact".to_owned(),
            ..ChatResponse::default()
        };
        assert!(mapping_reply(&prose).is_err());
    }

    #[test]
    fn reports_follow_the_python_text() {
        let outcome = Outcome {
            documents_processed: 3,
            entities_added: 9,
            relations_added: 4,
            entities_stored: 7,
            relations_stored: 3,
            ..Outcome::default()
        };
        let text = report("repo", "github", Ok(&outcome), 1.25, false);
        // The counts are the stored graph's; the extraction is labelled.
        assert!(
            text.starts_with("# Ingestion Complete: repo\n\n**Source:** repo\n**Documents:** 3\n**Entities:** 7\n**Relations:** 3\n**Extracted this run:** 9 entities, 4 relations (before duplicates merged)\n"),
            "{text}"
        );
        assert!(text.contains("**Duration:** 1.2s"));
        let failed = report("repo", "github", Err(&invalid("clone refused")), 0.0, false);
        assert_eq!(
            failed,
            "Ingestion failed for repo\n\nErrors:\n- clone refused"
        );
        let as_json: Value =
            serde_json::from_str(&report("repo", "github", Ok(&outcome), 2.0, true))
                .unwrap_or_default();
        assert_eq!(as_json["entities_added"], json!(9));
        assert_eq!(as_json["entities_in_graph"], json!(7));
        assert_eq!(as_json["relations_in_graph"], json!(3));
        assert_eq!(as_json["success"], json!(true));
    }

    #[test]
    fn status_text_and_summary_follow_the_python_manager() {
        let document = json!({"sources": {"5": {
            "toolkit_id": "5", "toolkit_name": "repo", "toolkit_type": "github", "status": "error",
            "last_updated": "2026-10-08T00:00:00.000000", "entities_count": 3, "relations_count": 1,
            "error_message": "boom", "branch": "main"
        }}, "last_modified": "2026-10-08T00:00:00.000000"});
        let summary = status_summary(&document);
        assert_eq!(summary["status_counts"]["error"], json!(1));
        let text = status_text(&summary);
        assert!(text.contains("# Source Status (1 sources)"));
        assert!(text.contains("- [ERR] **repo** (ID: 5, Type: github)"));
        assert!(text.contains("  - Error: boom\n  - Branch: main\n"));
        assert_eq!(
            status_text(&status_summary(&json!({"sources": {}}))),
            "No sources have been ingested yet. Use run_ingestion to add data from a toolkit."
        );
    }
}

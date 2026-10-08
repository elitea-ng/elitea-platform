//! The native runner (`ELITEA_INVENTORY_RUNNER=native`): the engine itself,
//! over the PostgreSQL graph store.
//!
//! Every tool of both families: `run_ingestion` (clone, parsers, model,
//! communities, embeddings — `crate::ingest`), the status tools,
//! `remove_source_entities`, `investigate` (`crate::investigate`), and the
//! read tools (`crate::retrieval`). A tool the dispatch does not know is
//! refused by name, never answered empty (a test holds the tables to it).

// The reports are built line by line, as the Python handlers built them.
#![allow(clippy::format_push_string)]

use crate::config::Settings;
use crate::extract::{self, Model};
use crate::ingest::source::Source;
use crate::ingest::{self, ModelOptions, Outcome, RunOptions};
use crate::store::{self, GraphKey, sources};
use crate::tools;
use elitea_engine_core::errors::{EngineError, ErrorType};
use elitea_engine_core::stream::Context;
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
}

fn invalid(message: impl Into<String>) -> EngineError {
    EngineError::new(ErrorType::Value, message.into())
}

/// Python truthiness of a parameter.
fn truthy(value: Option<&Value>) -> bool {
    match value {
        None | Some(Value::Null) => false,
        Some(Value::Bool(flag)) => *flag,
        Some(Value::Number(n)) => n.as_f64().is_some_and(|n| n != 0.0),
        Some(Value::String(text)) => !text.is_empty(),
        Some(Value::Array(items)) => !items.is_empty(),
        Some(Value::Object(fields)) => !fields.is_empty(),
    }
}

fn text_param<'a>(params: &'a Map<String, Value>, keys: &[&str]) -> Option<&'a str> {
    keys.iter()
        .find_map(|key| params.get(*key).and_then(Value::as_str))
        .map(str::trim)
        .filter(|text| !text.is_empty())
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
        Ok(Self {
            pool,
            settings: Arc::new(settings),
            transport,
            views: Arc::new(crate::retrieval::ViewCache::default()),
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
            "investigate" => self.investigate(key, params, context).await,
            "remove_source_entities" => self.remove_source(key, params).await,
            other => self.read(other, family, key, params).await,
        }
    }

    /// `remove_source_entities`: the source's citations, its edges and the
    /// entities only it cited go; its status and file hashes too. Under the
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

    /// `investigate`: the model agent (`crate::investigate`) over the
    /// graph's view and its source toolkits.
    async fn investigate(
        &self,
        key: GraphKey,
        params: &Map<String, Value>,
        context: &Context,
    ) -> Result<Value, EngineError> {
        use crate::investigate::{self as agent, Question, SourceToolkit};
        let Some(question) = Question::from_params(params) else {
            return Ok(crate::retrieval::answer(agent::missing_question()));
        };
        let model_name = text_param(params, &["llm_model", "toolkit_configuration_llm_model"])
            .map(str::to_owned)
            .or_else(|| {
                params
                    .get("llm_settings")
                    .and_then(|s| s.get("model_name"))
                    .and_then(Value::as_str)
                    .filter(|m| !m.is_empty())
                    .map(str::to_owned)
            })
            .ok_or_else(|| {
                invalid("no LLM model is configured for this Inventory toolkit; set llm_model in the toolkit configuration")
            })?;
        let settings = Self::model_settings(params, &model_name)?;
        let view = self
            .views
            .view(&self.pool, key)
            .await
            .map_err(|e| EngineError::new(ErrorType::Runtime, e.to_string()))?
            .unwrap_or_default();
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
        let client = Arc::new(ChatClient::new(self.transport.clone(), settings.clone()));
        let stop = context.stop_signal();
        let chat: agent::Chat = {
            let stop = stop.clone();
            Arc::new(move |request| {
                let (client, stop) = (Arc::clone(&client), stop.clone());
                Box::pin(async move { client.complete(&request, &stop).await })
            })
        };
        let call_source = source_caller(
            self.transport.http_client().clone(),
            &settings,
            key.project_id,
            model_name,
        );
        // Semantic search when the graph has vectors: the query is embedded
        // with the model the graph was built with, so the two compare.
        let embed: Option<agent::Embed> = crate::retrieval::semantic::stamped_model(&view)
            .filter(|_| crate::retrieval::semantic::has_embeddings(&view))
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
                let stop = stop.clone();
                let embed: agent::Embed = Arc::new(move |query: String| {
                    let (client, stop) = (client.clone(), stop.clone());
                    Box::pin(async move {
                        let vectors = client.embed_documents(&[query], &stop).await?;
                        Ok(vectors
                            .into_iter()
                            .next()
                            .unwrap_or_default()
                            .into_iter()
                            .map(f64::from)
                            .collect())
                    })
                });
                embed
            });
        let result = agent::investigate(
            &question,
            &view,
            &chat,
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
    ) -> Result<Value, EngineError> {
        let view = self
            .views
            .view(&self.pool, key)
            .await
            .map_err(|e| EngineError::new(ErrorType::Runtime, e.to_string()))?
            .unwrap_or_default();
        let call = crate::retrieval::Call {
            tool,
            family,
            params,
            view: &view,
        };
        crate::retrieval::dispatch(&call).unwrap_or_else(|| {
            Err(EngineError::new(
                ErrorType::FileNotFound,
                format!(
                    "'{tool}' is not served by the native Inventory engine yet (ADR-0027 P4); run the deployment with ELITEA_INVENTORY_RUNNER=legacy for it"
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
        );
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
            full_rebuild: truthy(params.get("full_rebuild")),
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
/// sends only the tool and its arguments.
fn source_caller(
    client: reqwest::Client,
    settings: &ModelSettings,
    project_id: i64,
    model: String,
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
            let (client, bearer, model) = (client.clone(), bearer.clone(), model.clone());
            Box::pin(async move {
                let body = json!({
                    "request_id": format!("investigate-{toolkit_id}-{tool}"),
                    "tool_name": tool,
                    "tool_params": arguments,
                    "toolkit_config": {"toolkit_id": toolkit_id},
                    "llm_model": model,
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
        let (documents, entities, relations) = outcome.map_or((0, 0, 0), |o| {
            (o.documents_processed, o.entities_added, o.relations_added)
        });
        return elitea_engine_core::pyjson::dumps(&json!({
            "success": outcome.is_ok(),
            "source": source,
            "documents_processed": documents,
            "entities_added": entities,
            "relations_added": relations,
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
    let mut output = format!(
        "# Ingestion Complete: {name}\n\n**Source:** {source}\n**Documents:** {}\n**Entities:** {}\n**Relations:** {}\n**Duration:** {seconds:.1}s\n",
        outcome.documents_processed, outcome.entities_added, outcome.relations_added
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

    /// The tools `run` answers itself, before the read dispatch.
    const RUN_TOOLS: [&str; 5] = [
        "run_ingestion",
        "get_sources_status",
        "get_ingestion_status",
        "investigate",
        "remove_source_entities",
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
    fn reports_follow_the_python_text() {
        let outcome = Outcome {
            documents_processed: 3,
            entities_added: 9,
            relations_added: 4,
            ..Outcome::default()
        };
        let text = report("repo", "github", Ok(&outcome), 1.25, false);
        assert!(
            text.starts_with("# Ingestion Complete: repo\n\n**Source:** repo\n**Documents:** 3\n")
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

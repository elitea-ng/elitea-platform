//! The model stage of ingestion (ADR-0027 P3d): entities, facts and
//! relations a model reads out of a file.
//!
//! The Python pipeline's `_process_file_with_chunks` and
//! `_extract_relations_from_file`, with its prompts and type tables
//! ([`assets`]), its JSON repair ([`json`](mod@json)), its chunking ([`chunk`]), its
//! skip rules ([`skip`]) and its two type normalisers ([`types`]). Where it
//! is deliberately different:
//!
//! * **citations are absolute.** The model numbers a chunk's lines from 1;
//!   the Python pipeline computed each chunk's first line and never applied
//!   it, so every citation past the first chunk was wrong;
//! * **text facts are kept.** The text fact prompt asks for a `title`, the
//!   pipeline kept only facts with a `subject`, so every fact of a
//!   document was dropped; here a fact without a subject takes its title;
//! * **relations are extracted.** The Python relation step took the text
//!   from the file's first entity — always the file node, which carries
//!   none — then tried to re-open the file by its repository-relative path
//!   in the service's working directory, found nothing and returned no
//!   relation for any file. Here it reads the file's text, as intended;
//! * **a model edge records where it was found** (`discovered_in_file`,
//!   `confidence`), so an incremental run removes it with its file.

pub mod chunk;
pub mod json;
pub mod skip;
// The prompts and type tables, the type normalisers and the stage's
// per-file result live in the shared core (ADR-0029 decision 7); re-exported
// so every path in this crate stays the same.
pub use elitea_inventory_core::extract::{ModelExtraction, assets, types};

use crate::graph::Citation;
use crate::ingest::ids::entity_id;
use crate::ingest::parse::{ParsedEntity, PendingRelation};
use elitea_engine_core::errors::{EngineError, ErrorType};
use elitea_engine_core::stream::StopSignal;
use serde_json::{Map, Value, json};
use std::collections::{HashMap, HashSet};
use std::future::Future;
use std::pin::Pin;
use std::sync::Arc;
use std::time::Duration;

/// Whether `error` is the stop line.
fn is_cancel(error: &EngineError) -> bool {
    *error == EngineError::cancelled()
}

/// A model answer in flight.
pub type Answer = Pin<Box<dyn Future<Output = Result<String, EngineError>> + Send>>;

/// One prompt in, the model's text out. The engine's is the gateway
/// client ([`gateway_model`]); tests pass a closure.
#[derive(Clone)]
pub struct Model(Arc<dyn Fn(String) -> Answer + Send + Sync>);

impl std::fmt::Debug for Model {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str("Model")
    }
}

impl Model {
    pub fn new(call: impl Fn(String) -> Answer + Send + Sync + 'static) -> Self {
        Self(Arc::new(call))
    }

    pub(crate) async fn ask(&self, prompt: String) -> Result<String, EngineError> {
        (self.0)(prompt).await
    }

    /// This model with at most `limit`'s permits of calls in flight: the
    /// ingestion fans out files × chunks (10 × 5 by default), which a
    /// model server with a few slots only queues, and a queued request
    /// keeps the server busy after a stop. A call waits for a permit (a
    /// stop ends the wait) and holds it until its answer, or until its
    /// future is dropped.
    #[must_use]
    pub fn limited(self, limit: Arc<tokio::sync::Semaphore>, stop: StopSignal) -> Self {
        let inner = self.0;
        Self::new(move |prompt| {
            let (inner, limit, stop) = (Arc::clone(&inner), Arc::clone(&limit), stop.clone());
            Box::pin(async move {
                let _permit = tokio::select! {
                    permit = limit.acquire_owned() => permit.map_err(|_| {
                        EngineError::new(ErrorType::Runtime, "the model call limiter was closed")
                    })?,
                    () = stop.stopped() => return Err(EngineError::cancelled()),
                };
                if stop.is_requested() {
                    return Err(EngineError::cancelled());
                }
                (inner)(prompt).await
            })
        })
    }
}

/// The gateway client as a [`Model`]: the prompt as one user message
/// (`LangChain`'s `ChatPromptTemplate.from_template`), temperature 0 and at
/// most 4096 tokens (the Python `ChatOpenAI`), one blocking completion.
#[must_use]
pub fn gateway_model(client: elitea_model_client::chat::ChatClient, stop: StopSignal) -> Model {
    use elitea_model_client::chat::{ChatMessage, ChatRequest, Sampling};
    let client = Arc::new(client);
    Model::new(move |prompt| {
        let client = Arc::clone(&client);
        let stop = stop.clone();
        Box::pin(async move {
            let mut request = ChatRequest::new(vec![ChatMessage::User(prompt)]);
            request.sampling = Sampling::Deterministic;
            request.max_tokens = Some(4096);
            client
                .complete(&request, &stop)
                .await
                .map(|response| response.content)
        })
    })
}

/// How the extractors retry a failed call (a refusal, a transport error,
/// an answer that is not JSON) and how much runs at once.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct Tuning {
    /// Attempts per call (Python's `max_retries`, 3).
    pub attempts: u32,
    /// The entity and fact extractors wait `n × this` after attempt `n`
    /// (10 s); the relation step `2^(n-1) ×` it / 10 (1 s, 2 s).
    pub retry_unit: Duration,
    /// Files extracted at once (`max_parallel_extractions`, 10).
    pub parallel_files: usize,
    /// A file's chunks extracted at once (`max_parallel_chunks`, 5).
    pub parallel_chunks: usize,
}

impl Default for Tuning {
    fn default() -> Self {
        Self {
            attempts: 3,
            retry_unit: Duration::from_secs(10),
            parallel_files: 10,
            parallel_chunks: 5,
        }
    }
}

/// `f"{i+1:4d} | {line}"` over the first `limit` lines (`split('\n')`).
fn numbered(content: &str, limit: usize) -> String {
    content
        .split('\n')
        .take(limit)
        .enumerate()
        .map(|(index, line)| format!("{:4} | {line}", index + 1))
        .collect::<Vec<_>>()
        .join("\n")
}

/// The model's answer as a list (`[result] if result else []`).
fn as_list(value: Value) -> Vec<Value> {
    match value {
        Value::Array(items) => items,
        Value::Null => Vec::new(),
        Value::Object(fields) if fields.is_empty() => Vec::new(),
        other => vec![other],
    }
}

/// Ask until a parsable answer or the attempts run out.
async fn ask_json(
    model: &Model,
    prompt: &str,
    tuning: Tuning,
    delay: impl Fn(u32) -> Duration,
    stop: &StopSignal,
) -> Result<Vec<Value>, EngineError> {
    let mut last = String::new();
    for attempt in 1..=tuning.attempts.max(1) {
        if stop.is_requested() {
            return Err(EngineError::cancelled());
        }
        match model.ask(prompt.to_owned()).await {
            Ok(text) => match json::parse_model_json(&text) {
                Some(value) => return Ok(as_list(value)),
                None => "the answer is not JSON".clone_into(&mut last),
            },
            Err(error) if is_cancel(&error) => return Err(error),
            Err(error) => last = error.message,
        }
        if attempt < tuning.attempts {
            tokio::select! {
                () = tokio::time::sleep(delay(attempt)) => {}
                () = stop.stopped() => return Err(EngineError::cancelled()),
            }
        }
    }
    Err(EngineError::new(ErrorType::Runtime, last))
}

fn text_of(value: Option<&Value>) -> Option<String> {
    match value? {
        Value::String(text) => Some(text.clone()),
        Value::Null => None,
        other => Some(other.to_string()),
    }
}

// A model may write `12.0`; a line is whole.
#[allow(clippy::cast_possible_truncation)]
fn line_of(value: Option<&Value>) -> Option<i64> {
    match value? {
        Value::Number(number) => number
            .as_i64()
            .or_else(|| number.as_f64().map(|f| f as i64)),
        Value::String(text) => text.trim().parse().ok(),
        _ => None,
    }
}

/// `EntityExtractor.extract` for one chunk: the raw entities, line ranges
/// widened to at least five lines (chunk-relative still).
async fn chunk_entities(
    model: &Model,
    chunk: &chunk::Chunk,
    file_path: &str,
    source_toolkit: &str,
    tuning: Tuning,
    stop: &StopSignal,
) -> Result<Vec<Map<String, Value>>, EngineError> {
    let prompt = assets::render(
        &assets::assets().prompts.entity,
        &[
            ("content", &numbered(&chunk.text, 200)),
            ("file_path", file_path),
            ("source_toolkit", source_toolkit),
            ("schema_section", ""),
        ],
    );
    let total_lines = i64::try_from(chunk.text.split('\n').count()).unwrap_or(i64::MAX);
    let answer = ask_json(model, &prompt, tuning, |n| tuning.retry_unit * n, stop).await?;
    Ok(answer
        .into_iter()
        .filter_map(|value| match value {
            Value::Object(fields) => Some(fields),
            _ => None,
        })
        .map(|mut entity| {
            entity.insert("source_toolkit".to_owned(), json!(source_toolkit));
            entity.insert("file_path".to_owned(), json!(file_path));
            if !entity.contains_key("name") {
                let name = entity
                    .get("properties")
                    .and_then(|p| p.get("name"))
                    .cloned()
                    .or_else(|| entity.get("id").cloned())
                    .unwrap_or_else(|| json!("unnamed"));
                entity.insert("name".to_owned(), name);
            }
            let start = line_of(entity.get("line_start")).unwrap_or(1);
            let end = line_of(entity.get("line_end")).unwrap_or(start);
            if end - start < 2 {
                let center = (start + end).div_euclid(2);
                entity.insert("line_start".to_owned(), json!((center - 2).max(1)));
                entity.insert("line_end".to_owned(), json!((center + 2).min(total_lines)));
            }
            entity
        })
        .collect())
}

/// `EntityExtractor._deduplicate_entities`.
fn dedupe_entities(entities: Vec<Map<String, Value>>) -> Vec<Map<String, Value>> {
    let mut kept: Vec<Map<String, Value>> = Vec::new();
    let mut index: HashMap<(String, String), usize> = HashMap::new();
    for entity in entities {
        let kind = text_of(entity.get("type")).unwrap_or_else(|| "unknown".to_owned());
        let name = entity
            .get("properties")
            .and_then(|p| p.get("name"))
            .or_else(|| entity.get("id"))
            .and_then(Value::as_str)
            .unwrap_or_default();
        let normalized =
            elitea_engine_core::pystr::strip(&name.to_lowercase()).replace(['_', '-'], " ");
        let key = (kind, normalized);
        if let Some(&at) = index.get(&key) {
            if let Some(Value::Object(new)) = entity.get("properties") {
                let existing = kept[at]
                    .entry("properties")
                    .or_insert_with(|| Value::Object(Map::new()));
                if let Value::Object(existing) = existing {
                    for (k, v) in new {
                        existing.entry(k.clone()).or_insert_with(|| v.clone());
                    }
                }
            }
        } else {
            index.insert(key, kept.len());
            kept.push(entity);
        }
    }
    kept
}

/// `FactExtractor.extract` / `extract_code` for one chunk.
async fn chunk_facts(
    model: &Model,
    chunk: &chunk::Chunk,
    file_path: &str,
    source_toolkit: &str,
    code: bool,
    tuning: Tuning,
    stop: &StopSignal,
) -> Vec<Map<String, Value>> {
    let tables = assets::assets();
    let prompt = if code {
        assets::render(
            &tables.prompts.code_fact,
            &[
                ("content", &numbered(&chunk.text, 200)),
                ("file_path", file_path),
            ],
        )
    } else {
        assets::render(
            &tables.prompts.fact,
            &[
                ("content", &numbered(&chunk.text, 300)),
                ("file_path", file_path),
                ("source_toolkit", source_toolkit),
            ],
        )
    };
    // A fact chunk that fails is skipped, as in Python.
    let Ok(answer) = ask_json(model, &prompt, tuning, |n| tuning.retry_unit * n, stop).await else {
        return Vec::new();
    };
    let allowed = if code {
        &tables.code_fact_types
    } else {
        &tables.fact_types
    };
    answer
        .into_iter()
        .filter_map(|value| match value {
            Value::Object(fields) => Some(fields),
            _ => None,
        })
        .filter_map(|mut fact| {
            let fact_type = text_of(fact.get("fact_type"))
                .unwrap_or_default()
                .to_lowercase();
            if !allowed.contains(&fact_type) {
                return None;
            }
            if !fact.contains_key("id") {
                let key = if code { "subject" } else { "title" };
                let seed = format!(
                    "{file_path}:{}:{fact_type}",
                    text_of(fact.get(key)).unwrap_or_default()
                );
                fact.insert("id".to_owned(), json!(&md5_hex(&seed)[..12]));
            }
            fact.insert("source_toolkit".to_owned(), json!(source_toolkit));
            fact.insert("file_path".to_owned(), json!(file_path));
            fact.entry("confidence").or_insert(json!(0.7));
            Some(fact)
        })
        .collect()
}

fn md5_hex(text: &str) -> String {
    use md5::{Digest, Md5};
    use std::fmt::Write as _;
    let mut hex = String::with_capacity(32);
    for byte in Md5::digest(text.as_bytes()) {
        let _ = write!(hex, "{byte:02x}");
    }
    hex
}

fn confidence(fact: &Map<String, Value>) -> f64 {
    fact.get("confidence")
        .and_then(Value::as_f64)
        .unwrap_or(0.0)
}

/// `FactExtractor._deduplicate_facts` (one chunk list at a time, as
/// `extract_batch` called it over a file's chunks).
fn dedupe_facts(facts: Vec<Map<String, Value>>) -> Vec<Map<String, Value>> {
    let mut kept: Vec<Map<String, Value>> = Vec::new();
    let mut index: HashMap<(String, String), usize> = HashMap::new();
    for mut fact in facts {
        let fact_type = text_of(fact.get("fact_type")).unwrap_or_else(|| "unknown".to_owned());
        let title = text_of(fact.get("title").or_else(|| fact.get("id"))).unwrap_or_default();
        let mut normalized = elitea_engine_core::pystr::strip(&title.to_lowercase()).to_owned();
        for word in ["the", "a", "an", "is", "are", "was", "were"] {
            normalized = normalized.replace(&format!(" {word} "), " ");
        }
        let normalized = elitea_engine_core::pystr::split_whitespace(&normalized)
            .collect::<Vec<_>>()
            .join(" ");
        let key = (fact_type, normalized);
        if let Some(&at) = index.get(&key) {
            let existing = &mut kept[at];
            let (winner, loser) = if confidence(&fact) > confidence(existing) {
                (&mut fact, existing.clone())
            } else {
                (existing, fact.clone())
            };
            if let Some(Value::Object(from)) = loser.get("properties") {
                let into = winner
                    .entry("properties")
                    .or_insert_with(|| Value::Object(Map::new()));
                if let Value::Object(into) = into {
                    for (k, v) in from {
                        into.entry(k.clone()).or_insert_with(|| v.clone());
                    }
                }
            }
            if confidence(&fact) > confidence(&kept[at]) {
                kept[at] = fact;
            }
        } else {
            index.insert(key, kept.len());
            kept.push(fact);
        }
    }
    kept
}

/// The code-like extensions that get the code fact prompt
/// (`_is_code_file or _is_code_like_file`). A code-like file without a
/// parser (`crate::ingest::parse::language_of` is `None`: `.sh`, `.rb`,
/// `.lua`, C, …) is what Python's `run()` gave its file node and the model
/// stage, and nothing else (`tests/code_like.rs`).
#[must_use]
pub fn is_code_like(path: &str) -> bool {
    let extension = elitea_engine_core::pystr::suffix(path).to_lowercase();
    let name = elitea_engine_core::pystr::file_name(path).to_lowercase();
    matches!(name.as_str(), "makefile" | "gnumakefile")
        || matches!(
            extension.as_str(),
            ".py"
                | ".pyx"
                | ".pyi"
                | ".js"
                | ".jsx"
                | ".ts"
                | ".tsx"
                | ".mjs"
                | ".cjs"
                | ".java"
                | ".kt"
                | ".kts"
                | ".cs"
                | ".rs"
                | ".swift"
                | ".go"
                | ".lua"
                | ".pl"
                | ".pm"
                | ".perl"
                | ".rb"
                | ".php"
                | ".sh"
                | ".bash"
                | ".zsh"
                | ".fish"
                | ".ps1"
                | ".bat"
                | ".cmd"
                | ".scala"
                | ".clj"
                | ".cljs"
                | ".ex"
                | ".exs"
                | ".erl"
                | ".hrl"
                | ".hs"
                | ".ml"
                | ".fs"
                | ".fsx"
                | ".r"
                | ".jl"
                | ".dart"
                | ".nim"
                | ".v"
                | ".zig"
                | ".cr"
                | ".d"
                | ".c"
                | ".cpp"
                | ".cc"
                | ".cxx"
                | ".h"
                | ".hpp"
                | ".hxx"
                | ".m"
                | ".mm"
                | ".groovy"
                | ".gradle"
                | ".cmake"
                | ".makefile"
                | ".mk"
        )
}

/// One file for the model stage.
#[derive(Debug, Clone, Copy)]
pub struct FileInput<'a> {
    /// The cited path.
    pub path: &'a str,
    pub text: &'a str,
    pub content_hash: &'a str,
    pub source_toolkit: &'a str,
    /// The lowercased names the parser found: a model entity of a code
    /// type with one of them is the parser's, and dropped.
    pub parser_names: &'a HashSet<String>,
}

/// One file through the model (`_process_file_with_chunks`, the model
/// half): its chunks' entities and facts, converted, file-deduplicated.
///
/// # Errors
///
/// Only a stop: a failed chunk is counted, a failed fact chunk skipped.
pub async fn extract_file(
    model: &Model,
    input: &FileInput<'_>,
    tuning: Tuning,
    stop: &StopSignal,
) -> Result<ModelExtraction, EngineError> {
    let FileInput {
        path,
        text,
        source_toolkit,
        ..
    } = *input;
    let chunks = chunk::chunks(path, text);
    let code = is_code_like(path);
    let semaphore = Arc::new(tokio::sync::Semaphore::new(tuning.parallel_chunks.max(1)));
    let entity_calls = chunks.iter().map(|chunk| {
        let semaphore = Arc::clone(&semaphore);
        async move {
            let _permit = semaphore.acquire().await;
            (
                chunk.start_line,
                chunk_entities(model, chunk, path, source_toolkit, tuning, stop).await,
            )
        }
    });
    let fact_calls = async {
        let mut facts = Vec::new();
        for chunk in &chunks {
            for mut fact in
                chunk_facts(model, chunk, path, source_toolkit, code, tuning, stop).await
            {
                shift_lines(&mut fact, chunk.start_line);
                facts.push(fact);
            }
        }
        dedupe_facts(facts)
    };
    let (entity_results, facts) =
        tokio::join!(futures_util::future::join_all(entity_calls), fact_calls);
    if stop.is_requested() {
        return Err(EngineError::cancelled());
    }
    let mut failed_chunks = 0;
    let mut raw_entities = Vec::new();
    for (start_line, result) in entity_results {
        match result {
            Ok(entities) => raw_entities.extend(entities.into_iter().map(|mut entity| {
                shift_lines(&mut entity, start_line);
                entity
            })),
            Err(error) if is_cancel(&error) => return Err(error),
            Err(_) => failed_chunks += 1,
        }
    }
    let mut entities = convert_entities(raw_entities, input);
    entities.extend(convert_facts(facts, input));
    Ok(ModelExtraction {
        entities,
        failed_chunks,
    })
}

/// The citation of a model item in `input`'s file.
fn citation_in(input: &FileInput<'_>, item: &Map<String, Value>) -> Citation {
    Citation {
        file_path: input.path.to_owned(),
        line_start: line_of(item.get("line_start")),
        line_end: line_of(item.get("line_end")),
        source_toolkit: Some(input.source_toolkit.to_owned()),
        doc_id: Some(format!("{}://{}", input.source_toolkit, input.path)),
        content_hash: Some(input.content_hash.to_owned()),
    }
}

/// The model's entities as graph entities: types through the ingestion
/// normaliser, the parser's own names dropped, the file-level
/// deduplication by (type, name) merging properties into the first.
fn convert_entities(
    raw_entities: Vec<Map<String, Value>>,
    input: &FileInput<'_>,
) -> Vec<ParsedEntity> {
    let path = input.path;
    let parser_names = input.parser_names;
    let code_layer = [
        "class",
        "function",
        "method",
        "module",
        "interface",
        "constant",
        "variable",
        "import",
        "property",
        "field",
    ];
    let mut converted = Vec::new();
    for entity in dedupe_entities(raw_entities) {
        let name = text_of(entity.get("name")).unwrap_or_else(|| "unnamed".to_owned());
        let kind = types::normalize_ingestion(
            &text_of(entity.get("type")).unwrap_or_else(|| "unknown".to_owned()),
        );
        if parser_names.contains(&name.to_lowercase()) && code_layer.contains(&kind.as_str()) {
            continue;
        }
        let properties: Map<String, Value> = entity
            .iter()
            .filter(|(key, _)| {
                !matches!(
                    key.as_str(),
                    "id" | "name" | "type" | "content" | "text" | "line_start" | "line_end"
                )
            })
            .map(|(k, v)| (k.clone(), v.clone()))
            .collect();
        converted.push(ParsedEntity {
            id: entity_id(&kind, &name, Some(path)),
            name,
            entity_type: kind,
            citation: citation_in(input, &entity),
            properties,
        });
    }
    // File-level deduplication by (type, name): properties merge into the
    // first.
    let mut entities: Vec<ParsedEntity> = Vec::new();
    for entity in converted {
        let key = (entity.entity_type.clone(), entity.name.to_lowercase());
        if let Some(existing) = entities
            .iter_mut()
            .find(|e| (e.entity_type.clone(), e.name.to_lowercase()) == key)
        {
            if let Some(Value::Object(new)) = entity.properties.get("properties") {
                let into = existing
                    .properties
                    .entry("properties")
                    .or_insert_with(|| Value::Object(Map::new()));
                if let Value::Object(into) = into {
                    for (k, v) in new {
                        into.entry(k.clone()).or_insert_with(|| v.clone());
                    }
                }
            }
        } else {
            entities.push(entity);
        }
    }
    entities
}

/// The model's facts as graph entities: a subject (or, for a document
/// fact, its title), generic ones dropped, deduplicated by (`fact_type`,
/// subject) with the more confident winning.
fn convert_facts(facts: Vec<Map<String, Value>>, input: &FileInput<'_>) -> Vec<ParsedEntity> {
    let path = input.path;
    // Facts: a subject (or, for a document fact, its title); generic ones
    // dropped; deduplicated by (fact_type, subject), the more confident
    // winning.
    let mut fact_entities: Vec<ParsedEntity> = Vec::new();
    for fact in facts {
        let subject = text_of(fact.get("subject"))
            .filter(|s| !elitea_engine_core::pystr::strip(s).is_empty())
            .or_else(|| text_of(fact.get("title")))
            .map(|s| elitea_engine_core::pystr::strip(&s).to_owned())
            .unwrap_or_default();
        if subject.is_empty()
            || matches!(
                subject.to_lowercase().as_str(),
                "unknown" | "unknown fact" | "n/a" | "none"
            )
        {
            continue;
        }
        let fact_type = text_of(fact.get("fact_type")).unwrap_or_else(|| "unknown".to_owned());
        let prefix: String = subject.chars().take(30).collect();
        let fact_confidence = fact.get("confidence").cloned().unwrap_or(json!(0.8));
        let mut properties = Map::new();
        properties.insert("fact_type".to_owned(), json!(fact_type));
        properties.insert("subject".to_owned(), json!(subject));
        properties.insert(
            "predicate".to_owned(),
            fact.get("predicate").cloned().unwrap_or(Value::Null),
        );
        properties.insert(
            "object".to_owned(),
            fact.get("object").cloned().unwrap_or(Value::Null),
        );
        properties.insert("confidence".to_owned(), fact_confidence.clone());
        if let Some(Value::Object(extra)) = fact.get("properties") {
            for (k, v) in extra {
                properties.entry(k.clone()).or_insert_with(|| v.clone());
            }
        }
        let candidate = ParsedEntity {
            id: entity_id("fact", &format!("{fact_type}_{prefix}"), Some(path)),
            name: subject.clone(),
            entity_type: "fact".to_owned(),
            citation: citation_in(input, &fact),
            properties,
        };
        let key_of = |e: &ParsedEntity| {
            (
                e.properties.get("fact_type").cloned(),
                e.name.to_lowercase(),
            )
        };
        let key = key_of(&candidate);
        if let Some(existing) = fact_entities.iter_mut().find(|e| key_of(e) == key) {
            let better = fact_confidence.as_f64().unwrap_or(0.0)
                > existing
                    .properties
                    .get("confidence")
                    .and_then(Value::as_f64)
                    .unwrap_or(0.0);
            if better {
                *existing = candidate;
            }
        } else {
            fact_entities.push(candidate);
        }
    }
    fact_entities
}

/// Make a chunk-relative citation absolute.
fn shift_lines(item: &mut Map<String, Value>, start_line: usize) {
    let offset = i64::try_from(start_line).unwrap_or(1) - 1;
    for key in ["line_start", "line_end"] {
        if let Some(line) = line_of(item.get(key)) {
            item.insert(key.to_owned(), json!(line + offset));
        }
    }
}

/// `RelationExtractor.build_entity_id_lookup` + `resolve_entity_id`.
pub struct IdResolver {
    lookup: HashMap<String, String>,
    by_name: Vec<(String, String, HashSet<String>)>,
}

fn snake(text: &str) -> String {
    text.to_lowercase().replace([' ', '-', ':'], "_")
}

impl IdResolver {
    /// From `(id, name, type)` of every graph entity, in graph order.
    #[must_use]
    pub fn new<'a>(entities: impl IntoIterator<Item = (&'a str, &'a str, &'a str)>) -> Self {
        let mut lookup = HashMap::new();
        let mut by_name: Vec<(String, String, HashSet<String>)> = Vec::new();
        let mut name_position: HashMap<String, usize> = HashMap::new();
        for (id, name, kind) in entities {
            if id.is_empty() {
                continue;
            }
            lookup.insert(id.to_owned(), id.to_owned());
            lookup.insert(id.to_lowercase(), id.to_owned());
            if !name.is_empty() {
                lookup.insert(name.to_owned(), id.to_owned());
                lookup.insert(name.to_lowercase(), id.to_owned());
                let snake_name = snake(name);
                lookup.insert(snake_name.clone(), id.to_owned());
                let short = snake_name
                    .replace("_a_", "_")
                    .replace("_an_", "_")
                    .replace("_the_", "_")
                    .replace("_your_", "_")
                    .replace("_my_", "_");
                lookup.insert(short, id.to_owned());
                let typed = format!("{kind}:{snake_name}");
                lookup.insert(typed.to_lowercase(), id.to_owned());
                lookup.insert(typed, id.to_owned());
                let words: HashSet<String> = snake_name.split('_').map(str::to_owned).collect();
                // A dict: a later entity of the same snake name replaces
                // the earlier one in place.
                if let Some(&at) = name_position.get(&snake_name) {
                    by_name[at] = (snake_name, id.to_owned(), words);
                } else {
                    name_position.insert(snake_name.clone(), by_name.len());
                    by_name.push((snake_name, id.to_owned(), words));
                }
            }
        }
        Self { lookup, by_name }
    }

    /// The id a model's reference names, if it names one closely enough.
    #[must_use]
    pub fn resolve(&self, reference: &str) -> Option<String> {
        if reference.is_empty() {
            return None;
        }
        if let Some(id) = self.lookup.get(reference) {
            return Some(id.clone());
        }
        let lower = reference.to_lowercase();
        if let Some(id) = self.lookup.get(&lower) {
            return Some(id.clone());
        }
        let reference_snake = lower.replace([' ', '-', ':'], "_");
        if let Some(id) = self.lookup.get(&reference_snake) {
            return Some(id.clone());
        }
        let words: HashSet<&str> = reference_snake
            .split('_')
            .filter(|w| w.chars().count() >= 3)
            .collect();
        if words.is_empty() {
            return None;
        }
        let mut best = None;
        let mut best_score = 0;
        for (name, id, name_words) in &self.by_name {
            if reference_snake.chars().count() >= 10
                && (name.contains(&reference_snake) || reference_snake.contains(name.as_str()))
            {
                return Some(id.clone());
            }
            let significant: HashSet<&str> = name_words
                .iter()
                .map(String::as_str)
                .filter(|w| w.chars().count() >= 3)
                .collect();
            if significant.is_empty() {
                continue;
            }
            let overlap = words.intersection(&significant).count();
            let min_words = words.len().min(significant.len());
            #[allow(clippy::cast_precision_loss)]
            let enough = overlap as f64 >= min_words as f64 * 0.5;
            if overlap >= 2 && overlap > best_score && enough {
                best_score = overlap;
                best = Some(id.clone());
            }
        }
        best
    }
}

/// `_extract_relations_from_file` + `RelationExtractor.extract` for one
/// file: the model reads the file's first 4000 characters and the first 20
/// of its entities (single-word code names left out), and its relations
/// are resolved against every graph entity and kept at confidence 0.5 or
/// more.
///
/// # Errors
///
/// Only a stop; a failed call yields no relation, as in Python.
pub async fn extract_relations(
    model: &Model,
    path: &str,
    text: &str,
    file_entities: &[(String, String, String)],
    resolver: &IdResolver,
    tuning: Tuning,
    stop: &StopSignal,
) -> Result<Vec<PendingRelation>, EngineError> {
    let listed: Vec<&(String, String, String)> = file_entities
        .iter()
        .filter(|(_, name, kind)| {
            let code = matches!(
                kind.to_lowercase().as_str(),
                "function" | "variable" | "constant" | "import" | "class" | "method"
            );
            let single = elitea_engine_core::pystr::split_whitespace(name).count() <= 1
                && name.chars().count() <= 15;
            !(code && single)
        })
        .take(20)
        .collect();
    if listed.len() < 2 {
        return Ok(Vec::new());
    }
    let entities_list = listed
        .iter()
        .map(|(id, name, kind)| format!("- {id} -> {name} ({kind})"))
        .collect::<Vec<_>>()
        .join("\n");
    let content: String = text.chars().take(4000).collect();
    let prompt = assets::render(
        &assets::assets().prompts.relation,
        &[
            ("content", &content),
            ("entities_list", &entities_list),
            ("schema_section", ""),
        ],
    );
    let delay = |n: u32| tuning.retry_unit / 10 * 2u32.saturating_pow(n - 1);
    let answer = match ask_json(model, &prompt, tuning, delay, stop).await {
        Ok(answer) => answer,
        Err(error) if is_cancel(&error) => return Err(error),
        Err(_) => return Ok(Vec::new()),
    };
    let mut relations: Vec<PendingRelation> = Vec::new();
    for relation in answer {
        let Value::Object(relation) = relation else {
            continue;
        };
        let source = resolver.resolve(&text_of(relation.get("source_id")).unwrap_or_default());
        let target = resolver.resolve(&text_of(relation.get("target_id")).unwrap_or_default());
        let (Some(source), Some(target)) = (source, target) else {
            continue;
        };
        let confidence = relation
            .get("confidence")
            .and_then(Value::as_f64)
            .unwrap_or(0.0);
        if confidence < 0.5 {
            continue;
        }
        relations.push(PendingRelation {
            source_id: Some(source),
            target_id: Some(target),
            source_symbol: String::new(),
            target_symbol: String::new(),
            relation_type: text_of(relation.get("relation_type"))
                .unwrap_or_else(|| "RELATED_TO".to_owned()),
            discovered_in_file: path.to_owned(),
            confidence,
            is_cross_file: false,
        });
    }
    Ok(relations)
}

#[cfg(test)]
mod tests;

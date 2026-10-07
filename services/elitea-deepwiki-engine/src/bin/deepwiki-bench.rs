//! Developer tool for the engine benchmark (`docs/benchmark-2026-10.md`).
//! Not shipped in the image.
//!
//! ```text
//! deepwiki-bench search <project-id> <wiki-id> <questions.jsonl> <out.jsonl>
//!                       --api-base <url> --embedding-model <name> [--limit 10]
//! ```
//!
//! `search` answers each question of `<questions.jsonl>` (one JSON object
//! per line with `id` and `question`) with the PostgreSQL read path the
//! native `ask` uses: the question embedded through the engine's own
//! embedding client (`--api-base`, an OpenAI-compatible base URL ending in
//! `/v1`), then `IndexReader::search_hybrid` — FTS and exact dense KNN fused
//! by weighted RRF, candidate pools of 30 and 30, as `ask`'s
//! `repository_docs` asks for them — over the published index of
//! `<wiki-id>` of project `<project-id>` (the index is scoped by project,
//! migration 0005) in `ELITEA_DEEPWIKI_DATABASE_URL`. One line per question:
//! `id`, the ranked `hits` (`node_id`, `rel_path`, `symbol_name`,
//! `symbol_type`, `combined_score`), `embed_ms` and `search_ms`.

use elitea_deepwiki_engine::llm::{
    EmbeddingClient, EmbeddingOptions, ModelSettings, Transport, TransportSettings,
};
use elitea_deepwiki_engine::runner::StopSignal;
use elitea_deepwiki_engine::storage;
use elitea_deepwiki_engine::storage::search::{Hybrid, IndexReader};
use serde_json::{Value, json};
use std::collections::HashMap;
use std::io::{BufRead, BufWriter, Write};
use std::process::ExitCode;
use std::time::Instant;

const USAGE: &str = "usage: deepwiki-bench search <project-id> <wiki-id> <questions.jsonl> <out.jsonl> --api-base <url> --embedding-model <name> [--limit 10]";

fn main() -> ExitCode {
    let args: Vec<String> = std::env::args().skip(1).collect();
    if let Some("search") = args.first().map(String::as_str) {
        match search(&args[1..]) {
            Ok(()) => ExitCode::SUCCESS,
            Err(error) => {
                eprintln!("search: {error}");
                ExitCode::FAILURE
            }
        }
    } else {
        eprintln!("{USAGE}");
        ExitCode::from(2)
    }
}

/// The questions file: one JSON object per non-blank line.
fn read_questions(path: &str) -> Result<Vec<Value>, String> {
    let file = std::fs::File::open(path).map_err(|e| format!("{path}: {e}"))?;
    let mut items = Vec::new();
    for line in std::io::BufReader::new(file).lines() {
        let line = line.map_err(|e| e.to_string())?;
        if line.trim().is_empty() {
            continue;
        }
        items.push(serde_json::from_str(&line).map_err(|e| e.to_string())?);
    }
    Ok(items)
}

/// `<project-id>`: the project the index belongs to (migration 0005).
fn project_scope(text: &str) -> Result<storage::ProjectScope, String> {
    text.parse()
        .ok()
        .and_then(storage::ProjectScope::new)
        .ok_or_else(|| "<project-id> must be a positive project id".to_owned())
}

fn search(args: &[String]) -> Result<(), String> {
    let mut positional = Vec::new();
    let mut options: HashMap<&str, String> = HashMap::new();
    let mut iter = args.iter();
    while let Some(arg) = iter.next() {
        match arg.as_str() {
            "--api-base" | "--embedding-model" | "--limit" => {
                let value = iter.next().ok_or(USAGE)?;
                options.insert(
                    match arg.as_str() {
                        "--api-base" => "api_base",
                        "--embedding-model" => "model",
                        _ => "limit",
                    },
                    value.clone(),
                );
            }
            _ => positional.push(arg.clone()),
        }
    }
    let [project, wiki_id, questions, out] = positional.as_slice() else {
        return Err(USAGE.to_owned());
    };
    let project = project_scope(project)?;
    let api_base = options.remove("api_base").ok_or(USAGE)?;
    let model = options.remove("model").ok_or(USAGE)?;
    let limit: usize = options
        .remove("limit")
        .map_or(Ok(10), |text| text.parse())
        .map_err(|_| "--limit must be a number".to_owned())?;
    let dsn =
        std::env::var(storage::DSN_ENV).map_err(|_| format!("{} is not set", storage::DSN_ENV))?;

    let items = read_questions(questions)?;

    let settings = ModelSettings::from_llm_settings(&json!({
        "api_base": api_base,
        "api_key": "bench",
        "model_name": "bench",
    }))
    .map_err(|e| e.to_string())?;
    let transport = Transport::new(&TransportSettings::default()).map_err(|e| e.to_string())?;
    let client = EmbeddingClient::new(transport, settings, model, EmbeddingOptions::default());
    let stop = StopSignal::default();

    let runtime = tokio::runtime::Builder::new_multi_thread()
        .enable_all()
        .build()
        .map_err(|e| e.to_string())?;
    let mut writer = BufWriter::new(std::fs::File::create(out).map_err(|e| format!("{out}: {e}"))?);
    runtime.block_on(async {
        let pool = storage::lazy_pool(&dsn, 2).map_err(|e| e.to_string())?;
        let reader = IndexReader::new(
            pool.clone(),
            storage::WikiKey::new(project, wiki_id.as_str()),
        );
        let params = Hybrid {
            limit,
            fts_pool: 30,
            vec_pool: 30,
            ..Hybrid::default()
        };
        for item in &items {
            let question = item["question"].as_str().unwrap_or_default();
            let started = Instant::now();
            let embedding: Vec<f64> = client
                .embed_query(question, &stop)
                .await
                .map_err(|e| e.to_string())?
                .into_iter()
                .map(f64::from)
                .collect();
            let embed_ms = started.elapsed().as_secs_f64() * 1000.0;
            let started = Instant::now();
            let hits = reader
                .search_hybrid(question, Some(&embedding), &params)
                .await
                .map_err(|e| e.to_string())?;
            let search_ms = started.elapsed().as_secs_f64() * 1000.0;
            let hits: Vec<Value> = hits
                .iter()
                .map(|hit| {
                    json!({
                        "node_id": hit.node_id,
                        "rel_path": hit.rel_path,
                        "symbol_name": hit.symbol_name,
                        "symbol_type": hit.symbol_type,
                        "combined_score": hit.scores.combined_score,
                    })
                })
                .collect();
            let line = json!({
                "id": item["id"],
                "hits": hits,
                "embed_ms": embed_ms,
                "search_ms": search_ms,
            });
            writeln!(writer, "{line}").map_err(|e| e.to_string())?;
        }
        pool.close().await;
        writer.flush().map_err(|e| e.to_string())
    })
}

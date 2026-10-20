//! Recall of `search_workspace`, lexical against hybrid, over labelled
//! queries (S4's harness).
//!
//! ```text
//! cargo run --release -p elitea-local-index --features eval --example search_eval -- \
//!     <folder> <index dir> <queries.tsv> [<model id> <model dir> [cpu]]
//! ```
//!
//! `examples/eval/queries.tsv` holds 50 questions about `libs/rust`, each
//! with the files that answer it. Builds (or refreshes) the index, embeds
//! it with the model through the real background embedder when one is
//! given (timed), then runs every query in `lexical`, `auto` (hybrid) and
//! `semantic` mode and prints recall@10 (a relevant file among the ten
//! hits), MRR@10 and latency p50/p95. Set `EVAL_SHOW=1` for the misses.
#![allow(
    clippy::unwrap_used,
    clippy::expect_used,
    clippy::print_stdout,
    clippy::cast_precision_loss
)]

use elitea_local_embed::candle_backend::{CandleEmbedder, DevicePreference};
use elitea_local_embed::{EmbedError, Embedder, catalog};
use elitea_local_index::embedding::{
    AlwaysRun, EmbedConfig, EmbedState, EmbedderSource, Embedding, SETTING,
};
use elitea_local_index::search::Request;
use elitea_local_index::service::{IndexService, NoEvents, limit_parser_threads};
use elitea_local_index::sqlite_store::VectorSet;
use std::path::{Path, PathBuf};
use std::sync::Arc;
use std::time::{Duration, Instant};

struct Dir {
    spec: &'static catalog::ModelSpec,
    path: PathBuf,
    preference: DevicePreference,
}

impl EmbedderSource for Dir {
    fn vector_set(&self, choice: &str) -> Option<VectorSet> {
        (choice == self.spec.stamp()).then(|| VectorSet {
            origin: "local".into(),
            model: self.spec.id.into(),
            dim: self.spec.dimension,
        })
    }

    fn load(&self, _choice: &str, _workspace_id: &str) -> Result<Arc<dyn Embedder>, EmbedError> {
        Ok(Arc::new(CandleEmbedder::load(
            self.spec,
            &self.path,
            self.preference,
        )?))
    }
}

struct Case {
    query: String,
    relevant: Vec<String>,
}

fn cases(path: &Path) -> Vec<Case> {
    std::fs::read_to_string(path)
        .unwrap()
        .lines()
        .filter(|line| !line.starts_with('#') && !line.trim().is_empty())
        .map(|line| {
            let (query, files) = line.split_once('\t').unwrap();
            Case {
                query: query.to_owned(),
                relevant: files.split(',').map(|f| f.trim().to_owned()).collect(),
            }
        })
        .collect()
}

fn percentile(sorted: &[f64], p: usize) -> f64 {
    sorted[(sorted.len() * p / 100).min(sorted.len() - 1)]
}

fn evaluate(service: &IndexService, cases: &[Case], mode: &str) {
    let dense = service.dense();
    let show = std::env::var_os("EVAL_SHOW").is_some();
    let (mut found, mut rr) = (0_u32, 0.0_f64);
    let mut latencies = Vec::new();
    for case in cases {
        let args = serde_json::json!({"query": case.query, "mode": mode, "top_k": 10});
        let request = Request::from_args(args.as_object().unwrap()).unwrap();
        let started = Instant::now();
        let (answer, _) = service.search(&request, dense.as_deref()).unwrap();
        latencies.push(started.elapsed().as_secs_f64() * 1000.0);
        let rank = answer
            .hits
            .iter()
            .position(|hit| case.relevant.contains(&hit.path));
        if let Some(rank) = rank {
            found += 1;
            rr += 1.0 / f64::from(u32::try_from(rank + 1).unwrap());
        } else if show {
            let got: Vec<&str> = answer
                .hits
                .iter()
                .take(3)
                .map(|h| h.path.as_str())
                .collect();
            println!("  miss [{mode}] {:?} -> {got:?}", case.query);
        }
    }
    latencies.sort_by(f64::total_cmp);
    let n = f64::from(u32::try_from(cases.len()).unwrap());
    println!(
        "mode={mode:<8} dense={} recall@10={:.2} ({found}/{}) mrr@10={:.3} p50={:.1}ms p95={:.1}ms",
        dense.is_some(),
        f64::from(found) / n,
        cases.len(),
        rr / n,
        percentile(&latencies, 50),
        percentile(&latencies, 95),
    );
}

#[tokio::main(flavor = "multi_thread")]
async fn main() {
    let args: Vec<String> = std::env::args().collect();
    let (folder, index, queries) = (
        Path::new(&args[1]),
        Path::new(&args[2]),
        Path::new(&args[3]),
    );
    limit_parser_threads();
    let cases = cases(queries);
    let service = IndexService::open("eval", folder, &[], index, Arc::new(NoEvents)).unwrap();
    let started = Instant::now();
    service.refresh(false).await.unwrap();
    println!(
        "index: {} chunks, refreshed in {:.1}s",
        service.store().chunk_count().unwrap(),
        started.elapsed().as_secs_f64()
    );
    evaluate(&service, &cases, "lexical");
    let Some(model) = args.get(4) else {
        return;
    };
    let spec = catalog::model(model).expect("a catalogue model id");
    let preference = if args.get(6).map(String::as_str) == Some("cpu") {
        DevicePreference::Cpu
    } else {
        DevicePreference::Auto
    };
    let source = Arc::new(Dir {
        spec,
        path: PathBuf::from(&args[5]),
        preference,
    });
    let embedding = Embedding::start(
        source,
        Arc::new(AlwaysRun),
        EmbedConfig::default(),
        Arc::new(|_: &str, _: &_| {}),
    )
    .unwrap();
    service.store().set_setting(SETTING, &spec.stamp()).unwrap();
    embedding.add(&service);
    let started = Instant::now();
    let mut first = None;
    loop {
        tokio::time::sleep(Duration::from_secs(5)).await;
        let Some(status) = embedding.status("eval") else {
            continue;
        };
        let start_done = *first.get_or_insert(status.done);
        let rate = (status.done - start_done) as f64 / started.elapsed().as_secs_f64();
        println!(
            "embed: {:?} {}/{} ({rate:.1} texts/s) {:?}",
            status.state, status.done, status.total, status.error
        );
        if status.state == EmbedState::Done {
            break;
        }
    }
    let (with, all) = service
        .store()
        .vector_coverage(&format!("local:{}:{}", spec.id, spec.dimension))
        .unwrap();
    println!(
        "embedded {with}/{all} chunks in {:.0}s",
        started.elapsed().as_secs_f64()
    );
    // The first query loads the matrix (and the model, if unloaded).
    let warm = Instant::now();
    let dense = service.dense().expect("vectors");
    let _ = dense.nearest("warm up", 1, None);
    println!("dense side ready in {:.2}s", warm.elapsed().as_secs_f64());
    evaluate(&service, &cases, "lexical");
    evaluate(&service, &cases, "auto");
    evaluate(&service, &cases, "semantic");
}

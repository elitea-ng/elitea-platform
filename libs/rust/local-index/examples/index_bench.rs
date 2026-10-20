//! Build the local index of a folder and time it, then time searches.
//!
//! ```text
//! cargo run --release -p elitea-local-index --example index_bench -- <folder> <index dir>
//! ```
//!
//! Prints the build (files, entities, chunks, seconds, database bytes), an
//! incremental refresh of the unchanged folder, and `search_workspace`
//! latency over a fixed set of queries (p50 / p95, lexical mode).
#![allow(clippy::unwrap_used, clippy::expect_used, clippy::print_stdout)]

use elitea_local_index::search::{Mode, Request};
use elitea_local_index::service::{
    IndexEvent, IndexEvents, IndexService, NoEvents, limit_parser_threads,
};
use std::path::Path;
use std::sync::Arc;
use std::time::Instant;

const QUERIES: &[&str] = &[
    "where is the workspace index refreshed after a turn",
    "IndexRegistry for_turn",
    "policy fingerprint path_deny",
    "how are refresh tokens rotated",
    "sqlite schema migration user_version",
    "gateway budget enforcement",
    "SSE stream flush",
    "NATS JetStream consumer",
    "vault secret read",
    "reciprocal rank fusion",
];

/// The resident set now, in MB (`ps`; a bench only).
fn rss_mb() -> u64 {
    std::process::Command::new("ps")
        .args(["-o", "rss=", "-p", &std::process::id().to_string()])
        .output()
        .ok()
        .and_then(|out| {
            String::from_utf8_lossy(&out.stdout)
                .trim()
                .parse::<u64>()
                .ok()
        })
        .map_or(0, |kb| kb / 1024)
}

/// `BENCH_TRACE=1`: each phase change with the time and the RSS.
struct Trace(Instant, std::sync::Mutex<&'static str>);

impl IndexEvents for Trace {
    fn emit(&self, event: IndexEvent) {
        let mut last = self.1.lock().unwrap();
        if *last != event.phase {
            *last = event.phase;
            println!(
                "  [{:6.1}s] {:<9} rss={} MB",
                self.0.elapsed().as_secs_f64(),
                event.phase,
                rss_mb()
            );
        }
    }
}

fn dir_bytes(dir: &Path) -> u64 {
    std::fs::read_dir(dir).map_or(0, |entries| {
        entries
            .flatten()
            .filter_map(|e| e.metadata().ok())
            .map(|m| m.len())
            .sum()
    })
}

#[tokio::main(flavor = "multi_thread")]
async fn main() {
    let args: Vec<String> = std::env::args().collect();
    let folder = Path::new(&args[1]);
    let index = Path::new(&args[2]);
    limit_parser_threads();
    let events: Arc<dyn IndexEvents> = if std::env::var_os("BENCH_TRACE").is_some() {
        Arc::new(Trace(Instant::now(), std::sync::Mutex::new("")))
    } else {
        Arc::new(NoEvents)
    };
    let service = IndexService::open("bench", folder, &[], index, events).unwrap();
    let started = Instant::now();
    let status = if std::env::var_os("BENCH_SKIP_BUILD").is_some() {
        service.refresh(false).await.unwrap()
    } else {
        service.refresh(true).await.unwrap()
    };
    let build = started.elapsed();
    println!("rss after build: {} MB", rss_mb());
    let chunks = service.store().chunk_count().unwrap();
    println!(
        "build: files={} entities={} relations={} chunks={} seconds={:.1} db_bytes={}",
        status.files,
        status.entities,
        status.relations,
        chunks,
        build.as_secs_f64(),
        dir_bytes(index)
    );
    let started = Instant::now();
    service.refresh(false).await.unwrap();
    println!(
        "incremental refresh (nothing changed): seconds={:.2}",
        started.elapsed().as_secs_f64()
    );
    breakdown(&service);
    if std::env::var_os("BENCH_SHOW").is_some() {
        for query in QUERIES {
            let request = Request {
                query: (*query).to_owned(),
                top_k: 5,
                path_glob: None,
                kind: None,
                mode: Mode::Lexical,
                execution_id: None,
            };
            let (answer, _) = service.search(&request, None).unwrap();
            println!("{query}");
            for hit in answer.hits {
                println!(
                    "  {:.4} {}:{}-{} {:?} {:?}",
                    hit.score,
                    hit.path,
                    hit.line_start,
                    hit.line_end,
                    hit.matched_by,
                    hit.entity_ref
                );
            }
        }
    }
    let mut latencies = Vec::new();
    for _ in 0..5 {
        for query in QUERIES {
            let request = Request {
                query: (*query).to_owned(),
                top_k: 10,
                path_glob: None,
                kind: None,
                mode: Mode::Lexical,
                execution_id: None,
            };
            let started = Instant::now();
            let (answer, _) = service.search(&request, None).unwrap();
            latencies.push(started.elapsed().as_secs_f64() * 1000.0);
            assert!(answer.hits.len() <= 10);
        }
    }
    latencies.sort_by(f64::total_cmp);
    println!(
        "search_workspace lexical: queries={} p50={:.1}ms p95={:.1}ms",
        latencies.len(),
        latencies[latencies.len() / 2],
        latencies[latencies.len() * 95 / 100]
    );
}

fn breakdown(service: &IndexService) {
    use elitea_inventory_core::retrieval::query::{SearchFilters, search};
    let (view, _) = service.view().unwrap();
    let started = Instant::now();
    let names = elitea_local_index::search::NameIndex::build(&view);
    println!(
        "name index: build={:.0}ms",
        started.elapsed().as_secs_f64() * 1000.0
    );
    let (mut fts, mut graph, mut name_ms) = (0.0, 0.0, 0.0);
    for query in QUERIES {
        let started = Instant::now();
        let expression = elitea_local_index::search::fts_expression(query).unwrap();
        let _ = service.store().search_fts(&expression, 100).unwrap();
        fts += started.elapsed().as_secs_f64() * 1000.0;
        let started = Instant::now();
        let _ = search(&view, query, 50, SearchFilters::default());
        graph += started.elapsed().as_secs_f64() * 1000.0;
        let started = Instant::now();
        let _ = names.matches(query, 50);
        name_ms += started.elapsed().as_secs_f64() * 1000.0;
    }
    let n = f64::from(u32::try_from(QUERIES.len()).unwrap());
    println!(
        "breakdown per query: fts={:.1}ms names={:.2}ms (inventory search, not used: {:.1}ms)",
        fts / n,
        name_ms / n,
        graph / n
    );
}

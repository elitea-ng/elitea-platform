//! Throughput of the on-device backend (the S0 spike, kept as a bench).
//!
//! ```text
//! cargo run --release --features candle --example embed_bench -- \
//!     <model id> <model dir> <folder to read> [chunks=512] [cpu]
//! ```
//!
//! Reads text files of the folder, cuts them into ~1,500-character pieces
//! (a code chunk's size), embeds them in batches of 32, and prints load
//! time, chunks per second and query latency. Peak memory: run it under
//! `/usr/bin/time -l`.
#![allow(clippy::unwrap_used, clippy::expect_used, clippy::print_stdout)]

use elitea_local_embed::Embedder as _;
use elitea_local_embed::candle_backend::{CandleEmbedder, DevicePreference};
use elitea_local_embed::catalog;
use std::path::Path;
use std::time::Instant;

fn texts(folder: &Path, want: usize, out: &mut Vec<String>) {
    let Ok(entries) = std::fs::read_dir(folder) else {
        return;
    };
    let mut entries: Vec<_> = entries.flatten().collect();
    entries.sort_by_key(std::fs::DirEntry::path);
    for entry in entries {
        if out.len() >= want {
            return;
        }
        let path = entry.path();
        let name = path.file_name().and_then(|n| n.to_str()).unwrap_or("");
        if name.starts_with('.') || name == "target" || name == "node_modules" {
            continue;
        }
        if path.is_dir() {
            texts(&path, want, out);
        } else if matches!(
            path.extension().and_then(|e| e.to_str()),
            Some("rs" | "go" | "ts" | "tsx" | "md" | "py")
        ) && let Ok(text) = std::fs::read_to_string(&path)
        {
            let chars: Vec<char> = text.chars().collect();
            for piece in chars.chunks(1500) {
                if out.len() >= want {
                    return;
                }
                out.push(piece.iter().collect());
            }
        }
    }
}

fn main() {
    let args: Vec<String> = std::env::args().collect();
    let spec = catalog::model(&args[1]).expect("a catalogue model id");
    let dir = Path::new(&args[2]);
    let want: usize = args.get(4).and_then(|n| n.parse().ok()).unwrap_or(512);
    let preference = if args.get(5).map(String::as_str) == Some("cpu") {
        DevicePreference::Cpu
    } else {
        DevicePreference::Auto
    };
    let mut corpus = Vec::new();
    texts(Path::new(&args[3]), want, &mut corpus);
    let started = Instant::now();
    let embedder = CandleEmbedder::load(spec, dir, preference).unwrap();
    let load = started.elapsed();
    // Warm up (kernels compiled on first use).
    let warm: Vec<&str> = corpus.iter().take(32).map(String::as_str).collect();
    embedder.embed_documents(&warm).unwrap();
    let started = Instant::now();
    let mut vectors = 0;
    for batch in corpus.chunks(32) {
        let batch: Vec<&str> = batch.iter().map(String::as_str).collect();
        vectors += embedder.embed_documents(&batch).unwrap().len();
    }
    let took = started.elapsed();
    let mut latencies = Vec::new();
    for query in [
        "where is the workspace index refreshed",
        "how are tokens refreshed after sign in",
        "fusion of lexical and dense search",
        "which policy keys gate local work",
        "SQLite schema migration",
    ]
    .iter()
    .cycle()
    .take(20)
    {
        let started = Instant::now();
        let vector = embedder.embed_query(query).unwrap();
        assert_eq!(vector.len(), spec.dimension);
        latencies.push(started.elapsed().as_secs_f64() * 1000.0);
    }
    latencies.sort_by(f64::total_cmp);
    let chars: usize = corpus.iter().map(String::len).sum();
    println!(
        "model={} device={} load={:.2}s chunks={} avg_chars={} took={:.2}s rate={:.1} chunks/s query_p50={:.1}ms query_p95={:.1}ms",
        spec.id,
        embedder.device_name(),
        load.as_secs_f64(),
        vectors,
        chars / corpus.len().max(1),
        took.as_secs_f64(),
        f64::from(u32::try_from(vectors).unwrap()) / took.as_secs_f64(),
        latencies[latencies.len() / 2],
        latencies[latencies.len() * 95 / 100],
    );
}

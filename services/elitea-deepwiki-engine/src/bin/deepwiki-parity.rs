//! Developer tool for the ADR-0026 parity gates. Not shipped in the image.
//!
//! The `parity/*.py` scripts named below wrote the Python dumps and scored
//! the results. They were deleted with the Python engine and exist up to
//! origin/main 1233e1582; the README's "Parity with the Python engine"
//! section says how to use them. The committed golden fixtures are dumps in
//! the same formats.
//!
//! ```text
//! deepwiki-parity parse-dump <language> <repo> <files.txt> <out.jsonl>
//! deepwiki-parity graph-dump <repo> <out-dir> [--parses-from <dir>] [--no-phase1c]
//!                             [--through phase2 --replay <recording.jsonl>]
//! deepwiki-parity index-dump <repo> <project-id> <wiki-id> [--embeddings <dim>]
//! deepwiki-parity pages <python-dump-dir> <out-dir>
//! deepwiki-parity structure-dump <repo> <py-dump> <out-dir> --llm-base <url>
//!                             [--planner cluster] [--repo-name owner/name] [--branch main]
//!                             [--repo-identifier owner/name:main:0123abcd]
//! ```
//!
//! `pages` runs the page phase (phase 5b) over a dump
//! `parity/python_pages_dump.py` wrote, against the in-process stub model,
//! and writes the same files (see `wiki::parity`); `parity/compare_pages.py`
//! diffs the two.
//!
//! `structure-dump` is the Rust side of the structure gate
//! (`parity/python_structure_dump.py` writes `<py-dump>`): Phase 1 + 1c with
//! this engine's parsers, Phase 2 against the dump's `recording.jsonl`,
//! Phase 3 with the dump's leidenalg memberships replayed
//! (`p3_leiden_calls.jsonl`; the cluster columns are checked against
//! `p3_assignments.jsonl`), then the repository analysis and the structure
//! planner against the model at `--llm-base` (the LLM stub, which records
//! the request bodies). It writes `structure.json`, `analysis.json` and
//! `summary.json`; `parity/compare_structure.py` compares them and the two
//! request records.
//!
//! `index-dump` builds the graph of `<repo>` (Phase 1 + 1c, this engine's
//! parsers), stages it into the build space of the database
//! `ELITEA_DEEPWIKI_DATABASE_URL` names (migrated first), publishes it as
//! `<wiki-id>`, and prints the row counts and the time of each step. With
//! `--embeddings <dim>`, every node also gets a deterministic pseudo-vector
//! of that dimension (no model is called), to measure the dense rows.
//!
//! `parse-dump` parses the listed files (one repository-relative path per
//! line, as `parity/compare_parses.py` writes them from the Python
//! reference) and writes one `ParseResult` per line, sorted by path, with
//! paths made repository-relative — the shape `python_parse_dump.py` writes.
//!
//! `graph-dump` builds the index graph of `<repo>` — Phase 1, then Phase 1c
//! with the flags the environment sets (`DEEPWIKI_TEST_LINKER`), as
//! `parity/python_reference.py` does; `--no-phase1c` stops after Phase 1 —
//! and writes `nodes.jsonl` and
//! `edges.jsonl` in the `repo_nodes` / `repo_edges` row shape, sorted like
//! `parity/python_reference.py` (nodes by id; edges by source, target,
//! `rel_type`, `edge_class`, then graph order) and serialised like Python's
//! `json.dumps(row, sort_keys=True, ensure_ascii=False)`, so the files diff
//! line by line against the reference.
//!
//! With `--parses-from`, parsing is replaced by the reference `ParseResult`s
//! in `<dir>/<language>.jsonl` (written by `parity/python_parse_dump.py`,
//! paths repository-relative); they are re-absolutised to `<repo>/<rel>`,
//! the form the Python builder saw. Any difference is then a BUILDER
//! difference. Without it, this engine's own parsers run.
//!
//! `--through phase2 --replay <recording.jsonl>` goes on to Phase 2
//! (`graph::topology::run_phase2`) with the index answering from the
//! recording `parity/python_reference.py --through phase2` wrote, and the
//! stand-in embedding as the model. The rows are then the index's after
//! Phase 2 — the persisted edges (weights, the new edges) and `is_hub` —
//! and `stats.json` is the stats dict, as the reference writes them.

use elitea_deepwiki_engine::graph::builder::{self, ParseResultsByLanguage};
use elitea_deepwiki_engine::graph::discover;
use elitea_deepwiki_engine::graph::flags::Phase1cFlags;
use elitea_deepwiki_engine::graph::topology::replay::{ReplayStore, StandinEmbedder};
use elitea_deepwiki_engine::graph::topology::{self, CalibrationProfile, Phase2Config};
use elitea_deepwiki_engine::graph::{CodeGraph, EdgeRef, EdgeRow, NodeData, edge_row, node_row};
use elitea_deepwiki_engine::parsers::model::ParseResult;
use elitea_deepwiki_engine::parsers::parser_for;
use elitea_deepwiki_engine::pyjson;
use elitea_deepwiki_engine::storage;
use elitea_deepwiki_engine::storage::build::{BuildSpace, WikiRecord};
use elitea_deepwiki_engine::storage::search::IndexReader;
use serde::Serialize;
use serde_json::Value;
use std::collections::HashSet;
use std::io::{BufRead, BufWriter, Write};
use std::path::{Path, PathBuf};
use std::process::ExitCode;
use std::time::Instant;

const USAGE: &str = "usage: deepwiki-parity parse-dump <language> <repo> <files.txt> <out.jsonl>\n       deepwiki-parity graph-dump <repo> <out-dir> [--parses-from <dir>] [--no-phase1c] [--through phase2 --replay <recording.jsonl>]\n       deepwiki-parity index-dump <repo> <project-id> <wiki-id> [--embeddings <dim>]\n       deepwiki-parity structure-dump <repo> <py-dump> <out-dir> --llm-base <url> [--planner P] [--repo-name N] [--branch B] [--repo-identifier I]\n       deepwiki-parity pages <python-dump-dir> <out-dir>";

fn main() -> ExitCode {
    let args: Vec<String> = std::env::args().skip(1).collect();
    match args.first().map(String::as_str) {
        Some("parse-dump") if args.len() == 5 => parse_dump(&args[1], &args[2], &args[3], &args[4]),
        Some("index-dump") => match index_dump(&args[1..]) {
            Ok(()) => ExitCode::SUCCESS,
            Err(error) => {
                eprintln!("index-dump: {error}");
                ExitCode::FAILURE
            }
        },
        Some("pages") if args.len() == 3 => match pages(&args[1], &args[2]) {
            Ok(()) => ExitCode::SUCCESS,
            Err(error) => {
                eprintln!("pages: {error}");
                ExitCode::FAILURE
            }
        },
        Some("structure-dump") => match structure_dump(&args[1..]) {
            Ok(()) => ExitCode::SUCCESS,
            Err(error) => {
                eprintln!("structure-dump: {error}");
                ExitCode::FAILURE
            }
        },
        Some("graph-dump") => match graph_dump(&args[1..]) {
            Ok(()) => ExitCode::SUCCESS,
            Err(error) => {
                eprintln!("graph-dump: {error}");
                ExitCode::FAILURE
            }
        },
        _ => {
            eprintln!("{USAGE}");
            ExitCode::from(2)
        }
    }
}

/// The options of `graph-dump`.
struct GraphDumpArgs {
    repo: String,
    out_dir: String,
    parses_from: Option<PathBuf>,
    phase1c: bool,
    /// `--through phase2 --replay <recording>`.
    replay: Option<PathBuf>,
}

fn parse_graph_dump_args(args: &[String]) -> Result<GraphDumpArgs, String> {
    let mut positional = Vec::new();
    let mut parses_from: Option<PathBuf> = None;
    let mut phase1c = true;
    let mut phase2 = false;
    let mut replay: Option<PathBuf> = None;
    let mut iter = args.iter();
    while let Some(arg) = iter.next() {
        if arg == "--no-phase1c" {
            phase1c = false;
        } else if arg == "--through" {
            match iter.next().map(String::as_str) {
                Some("phase2") => phase2 = true,
                Some("phase1c") => phase2 = false,
                _ => return Err(USAGE.to_owned()),
            }
        } else if arg == "--replay" {
            replay = Some(PathBuf::from(iter.next().ok_or(USAGE)?));
        } else if arg == "--parses-from" {
            parses_from = Some(PathBuf::from(iter.next().ok_or(USAGE)?));
        } else {
            positional.push(arg.clone());
        }
    }
    let [repo, out_dir] = <[String; 2]>::try_from(positional).map_err(|_| USAGE.to_owned())?;
    match (phase2, &replay) {
        (true, None) => Err("--through phase2 needs --replay <recording.jsonl>".to_owned()),
        (false, Some(_)) => Err("--replay needs --through phase2".to_owned()),
        _ => Ok(GraphDumpArgs {
            repo,
            out_dir,
            parses_from,
            phase1c,
            replay,
        }),
    }
}

fn graph_dump(args: &[String]) -> Result<(), String> {
    let GraphDumpArgs {
        repo,
        out_dir,
        parses_from,
        phase1c,
        replay,
    } = parse_graph_dump_args(args)?;
    // `Path.resolve()`, as the reference harness does.
    let repo = std::fs::canonicalize(&repo).map_err(|e| format!("{repo}: {e}"))?;
    let repo = repo
        .to_str()
        .ok_or("the repository path is not UTF-8")?
        .to_owned();
    let out_dir = PathBuf::from(out_dir);
    std::fs::create_dir_all(&out_dir).map_err(|e| format!("{}: {e}", out_dir.display()))?;

    let started = Instant::now();
    rss("start");
    let discovery = discover::discover_files(&repo);
    let discovered = started.elapsed();
    let parse_started = Instant::now();
    let parses = match &parses_from {
        Some(dir) => Some(load_parses(dir, &repo, &discovery)?),
        None => None,
    };
    let parse_time = parse_started.elapsed();
    rss("parsed");
    let flags = if phase1c {
        Phase1cFlags::from_env().map_err(|e| e.to_string())?
    } else {
        Phase1cFlags::none()
    };
    let build_started = Instant::now();
    // Our own parsers run inside the build, one language at a time; the
    // parse time is then part of the build time.
    let (graph, report, phase1c_report) = match parses {
        Some(parses) => builder::build_index_graph(&repo, &discovery, parses, &flags),
        None => builder::build_index_graph_parsed(&repo, &discovery, &flags),
    };
    let build_time = build_started.elapsed();
    rss("built");
    let mut graph = graph;
    let persisted = match &replay {
        Some(recording) => Some(run_phase2_replayed(&mut graph, recording, &out_dir)?),
        None => None,
    };
    let write_started = Instant::now();
    let (nodes, edges) = write_rows(&graph, &out_dir, persisted.as_ref())?;
    let write_time = write_started.elapsed();
    rss("written");

    eprintln!(
        "discovery {:.2}s (symlinks skipped {}), parses {:.2}s, build {:.2}s, write {:.2}s, total {:.2}s",
        discovered.as_secs_f64(),
        discovery.symlinks_skipped,
        parse_time.as_secs_f64(),
        build_time.as_secs_f64(),
        write_time.as_secs_f64(),
        started.elapsed().as_secs_f64(),
    );
    for (step, took) in report.timings.iter().chain(&phase1c_report.timings) {
        eprintln!("  {step}: {:.3}s", took.as_secs_f64());
    }
    eprintln!(
        "rich files {}, doc files {}, sql files {}; relationships {} attempted, {} added; \
         contraction: {} removed, {} rewritten, {} self-loops, {} unresolved; orm edges {}",
        report.rich_files,
        report.doc_files,
        report.sql_files,
        report.relationships_attempted,
        report.relationships_added,
        report.contraction.nodes_removed,
        report.contraction.edges_rewritten,
        report.contraction.self_loops_dropped,
        report.contraction.unresolved,
        report.orm_edges,
    );
    if phase1c {
        eprintln!(
            "phase 1c {flags:?}: {} surface nodes, {} contract nodes, {} cross-language edges, \
             {} test links; markdown {} contains, {} references, {} documents synthesized",
            phase1c_report.surface_nodes,
            phase1c_report.contract_nodes,
            phase1c_report.cross_language_edges,
            phase1c_report.test_link_edges,
            phase1c_report.markdown.contains_edges,
            phase1c_report.markdown.references_edges,
            phase1c_report.markdown.documents_synthesized,
        );
    }
    println!("{{\"nodes\": {nodes}, \"edges\": {edges}}}");
    Ok(())
}

fn index_dump(args: &[String]) -> Result<(), String> {
    let mut positional = Vec::new();
    let mut dimension: Option<usize> = None;
    let mut iter = args.iter();
    while let Some(arg) = iter.next() {
        if arg == "--embeddings" {
            let text = iter.next().ok_or(USAGE)?;
            dimension = Some(
                text.parse()
                    .map_err(|_| format!("not a dimension: {text}"))?,
            );
        } else {
            positional.push(arg);
        }
    }
    let [repo, project, wiki_id] = positional.as_slice() else {
        return Err(USAGE.to_owned());
    };
    let key = storage::WikiKey::new(
        project
            .parse()
            .ok()
            .and_then(storage::ProjectScope::new)
            .ok_or("<project-id> must be a positive project id")?,
        wiki_id.as_str(),
    );
    let dsn =
        std::env::var(storage::DSN_ENV).map_err(|_| format!("{} is not set", storage::DSN_ENV))?;
    let repo = std::fs::canonicalize(repo).map_err(|e| format!("{repo}: {e}"))?;
    let repo = repo
        .to_str()
        .ok_or("the repository path is not UTF-8")?
        .to_owned();

    let started = Instant::now();
    rss("start");
    let discovery = discover::discover_files(&repo);
    let flags = Phase1cFlags::from_env().map_err(|e| e.to_string())?;
    let (graph, _report, _phase1c) = builder::build_index_graph_parsed(&repo, &discovery, &flags);
    rss("built");
    eprintln!(
        "graph: {} nodes, {} edges in {:.2}s",
        graph.node_count(),
        graph.edge_count(),
        started.elapsed().as_secs_f64()
    );

    let runtime = tokio::runtime::Builder::new_multi_thread()
        .enable_all()
        .build()
        .map_err(|e| e.to_string())?;
    runtime.block_on(async {
        let pool = storage::lazy_pool(&dsn, 4).map_err(|e| e.to_string())?;
        storage::migrate::apply_all(&pool)
            .await
            .map_err(|e| e.to_string())?;
        let summary = stage_and_publish(&pool, &graph, &key, dimension).await?;
        let reader = IndexReader::new(pool.clone(), key.clone());
        probe_queries(&reader, dimension).await?;
        let stats = reader.stats().await.map_err(|e| e.to_string())?;
        let branches: Vec<String> = stats
            .branches
            .iter()
            .map(|(name, b)| format!("{name}: {} docs, avgdl {:.3}", b.doc_count, b.avgdl))
            .collect();
        println!(
            "{{\"nodes\": {}, \"vectors\": {}, {summary}, \"branches\": {branches:?}}}",
            stats.node_count, stats.vector_count,
        );
        pool.close().await;
        Ok::<(), String>(())
    })
}

/// Stage the graph (and pseudo-vectors), publish, print the timings.
/// Returns the JSON fragment of the counts and times.
async fn stage_and_publish(
    pool: &sqlx::PgPool,
    graph: &CodeGraph,
    key: &storage::WikiKey,
    dimension: Option<usize>,
) -> Result<String, String> {
    let wiki_id = key.wiki_id();
    let space = BuildSpace::new(pool.clone(), "deepwiki-parity");
    let mut build = space.begin(key).await.map_err(|e| e.to_string())?;

    let stage_started = Instant::now();
    let staged = build.stage_graph(graph).await.map_err(|e| e.to_string())?;
    let stage_time = stage_started.elapsed();
    rss("staged");
    eprintln!(
        "staged {} nodes, {} edges (collapsed) in {:.2}s",
        staged.nodes,
        staged.edges,
        stage_time.as_secs_f64()
    );

    let mut vector_time = 0.0;
    if let Some(dimension) = dimension {
        let vectors_started = Instant::now();
        let ids: Vec<&str> = graph.nodes().map(|(id, _)| id).collect();
        let mut written = 0;
        for chunk in ids.chunks(5_000) {
            let vectors: Vec<(&str, Vec<f64>)> = chunk
                .iter()
                .map(|id| (*id, pseudo_vector(id, dimension)))
                .collect();
            written += build
                .stage_embeddings(vectors.iter().map(|(id, v)| (*id, v.as_slice())))
                .await
                .map_err(|e| e.to_string())?;
        }
        vector_time = vectors_started.elapsed().as_secs_f64();
        rss("vectors");
        eprintln!("staged {written} vectors of dimension {dimension} in {vector_time:.2}s");
    }

    let publish_started = Instant::now();
    let counts = build
        .publish(&WikiRecord {
            repo: Some(wiki_id.to_owned()),
            ..WikiRecord::default()
        })
        .await
        .map_err(|e| e.to_string())?;
    let publish_time = publish_started.elapsed();
    rss("published");
    eprintln!(
        "published {} nodes, {} edges, {} vectors, {} fts documents in {:.2}s",
        counts.nodes,
        counts.edges,
        counts.embeddings,
        counts.fts_documents,
        publish_time.as_secs_f64()
    );
    Ok(format!(
        "\"edges\": {}, \"stage_seconds\": {:.2}, \"vector_seconds\": {vector_time:.2}, \"publish_seconds\": {:.2}",
        counts.edges,
        stage_time.as_secs_f64(),
        publish_time.as_secs_f64()
    ))
}

/// A few searches over the published index, timed.
async fn probe_queries(reader: &IndexReader, dimension: Option<usize>) -> Result<(), String> {
    for query in ["connection", "publish transaction", "func main"] {
        let fts_started = Instant::now();
        let fts = reader
            .search_fts(query, 30)
            .await
            .map_err(|e| e.to_string())?;
        let fts_ms = fts_started.elapsed().as_secs_f64() * 1e3;
        eprintln!("query {query:?}: fts {} hits in {fts_ms:.0} ms", fts.len());
    }
    if let Some(dimension) = dimension {
        let query = pseudo_vector("query", dimension);
        let dense_started = Instant::now();
        let dense = reader
            .search_dense(&query, 30)
            .await
            .map_err(|e| e.to_string())?;
        eprintln!(
            "dense exact scan: {} hits in {:.0} ms",
            dense.len(),
            dense_started.elapsed().as_secs_f64() * 1e3
        );
    }
    Ok(())
}

/// A deterministic unit-ish vector from a node id: a measurement aid for
/// the dense rows, not an embedding.
fn pseudo_vector(id: &str, dimension: usize) -> Vec<f64> {
    use std::hash::{Hash, Hasher};
    let mut state = std::collections::hash_map::DefaultHasher::new();
    id.hash(&mut state);
    let mut seed = state.finish() | 1;
    (0..dimension)
        .map(|_| {
            // xorshift64
            seed ^= seed << 13;
            seed ^= seed >> 7;
            seed ^= seed << 17;
            f64::from(u32::try_from(seed >> 40).unwrap_or(0)) / f64::from(1_u32 << 24) - 0.5
        })
        .collect()
}

/// With `DEEPWIKI_PARITY_RSS` set, print the resident set size at a phase
/// boundary (through `ps`: the crate forbids the unsafe `getrusage` call).
/// A measurement aid for the memory work; off by default.
fn rss(phase: &str) {
    if std::env::var_os("DEEPWIKI_PARITY_RSS").is_none() {
        return;
    }
    let output = std::process::Command::new("ps")
        .args(["-o", "rss=", "-p", &std::process::id().to_string()])
        .output();
    if let Ok(output) = output {
        let kib = String::from_utf8_lossy(&output.stdout).trim().to_owned();
        let mib = kib.parse::<u64>().map_or(0, |k| k / 1024);
        eprintln!("rss {phase}: {mib} MiB");
    }
}

/// Load `<dir>/<language>.jsonl` for every language discovery found.
fn load_parses(
    dir: &Path,
    repo: &str,
    discovery: &discover::Discovery,
) -> Result<ParseResultsByLanguage, String> {
    let mut parses = ParseResultsByLanguage::new();
    for language in discovery.files_by_language.keys() {
        let path = dir.join(format!("{language}.jsonl"));
        if !path.exists() {
            continue;
        }
        let file = std::fs::File::open(&path).map_err(|e| format!("{}: {e}", path.display()))?;
        let mut results = std::collections::BTreeMap::new();
        for (number, line) in std::io::BufReader::new(file).lines().enumerate() {
            let line = line.map_err(|e| format!("{}: {e}", path.display()))?;
            let mut result: ParseResult = serde_json::from_str(&line)
                .map_err(|e| format!("{}:{}: {e}", path.display(), number + 1))?;
            absolutise(&mut result, repo);
            results.insert(result.file_path.clone(), result);
        }
        let expected = discovery.files(language);
        if results.len() != expected.len() || !expected.iter().all(|p| results.contains_key(p)) {
            eprintln!(
                "warning: {language}: {} parse results for {} discovered files",
                results.len(),
                expected.len()
            );
        }
        parses.insert(language.clone(), results);
    }
    Ok(parses)
}

/// `<repo>/<rel>` for every repository-relative path in a reference dump.
fn absolutise(result: &mut ParseResult, repo: &str) {
    let fix = |path: &mut String| {
        if !path.is_empty() && !path.starts_with('/') {
            *path = format!("{repo}/{path}");
        }
    };
    fix(&mut result.file_path);
    for symbol in &mut result.symbols {
        fix(&mut symbol.file_path);
    }
    for relationship in &mut result.relationships {
        fix(&mut relationship.source_file);
        if let Some(target) = relationship.target_file.as_mut() {
            fix(target);
        }
    }
}

/// What Phase 2 wrote to the (replayed) index: the hubs and the edges.
struct Persisted {
    hubs: HashSet<String>,
    edges: Vec<EdgeRow>,
}

/// Run Phase 2 against the recording; write `stats.json`.
fn run_phase2_replayed(
    graph: &mut CodeGraph,
    recording: &Path,
    out_dir: &Path,
) -> Result<Persisted, String> {
    let text =
        std::fs::read_to_string(recording).map_err(|e| format!("{}: {e}", recording.display()))?;
    let mut store = ReplayStore::from_jsonl(&text).map_err(|e| e.to_string())?;
    let config = Phase2Config {
        profile: CalibrationProfile::from_env()?,
        ..Phase2Config::default()
    };
    let mut embedder = StandinEmbedder;
    let started = Instant::now();
    let outcome = topology::run_phase2(graph, &mut store, Some(&mut embedder), &config)
        .map_err(|e| format!("phase 2: {e}"))?;
    let took = started.elapsed();
    // The index row keeps the type the tiered lexical pass re-typed in the
    // graph only: put it back for the row dump.
    for (id, previous) in outcome.retyped {
        if let Some(node) = graph.node_mut(&id) {
            node.symbol_type = previous;
        }
    }
    let stats = format!("{}\n", pyjson::dumps(&outcome.stats));
    std::fs::write(out_dir.join("stats.json"), stats).map_err(|e| e.to_string())?;
    eprintln!(
        "phase 2 {:.3}s: {} hubs, {} edges persisted; {} recorded calls not made",
        took.as_secs_f64(),
        store.hubs.len(),
        store.edges.len(),
        store.unused_calls(),
    );
    Ok(Persisted {
        hubs: std::mem::take(&mut store.hubs).into_iter().collect(),
        edges: std::mem::take(&mut store.edges),
    })
}

/// Write the rows one at a time, in the reference's order: only the sort
/// order (references into the graph) is held, not every row. After Phase 2
/// the edges are the rows Phase 2 persisted, and the hubs are flagged.
fn write_rows(
    graph: &CodeGraph,
    out_dir: &Path,
    persisted: Option<&Persisted>,
) -> Result<(usize, usize), String> {
    let mut nodes: Vec<(&str, &NodeData)> = graph.nodes().collect();
    nodes.sort_by(|a, b| a.0.cmp(b.0));
    write_jsonl(
        &out_dir.join("nodes.jsonl"),
        nodes.iter().map(|(id, data)| {
            let mut row = node_row(id, data);
            if persisted.is_some_and(|p| p.hubs.contains(*id)) {
                row.is_hub = 1;
            }
            row
        }),
    )?;
    if let Some(persisted) = persisted {
        let mut edges: Vec<&EdgeRow> = persisted.edges.iter().collect();
        // Stable: ties keep persist order, the reference's row id order.
        edges.sort_by(|a, b| {
            (&a.source_id, &a.target_id, &a.rel_type, &a.edge_class).cmp(&(
                &b.source_id,
                &b.target_id,
                &b.rel_type,
                &b.edge_class,
            ))
        });
        write_jsonl(&out_dir.join("edges.jsonl"), edges.into_iter())?;
        return Ok((nodes.len(), persisted.edges.len()));
    }
    let mut edges: Vec<EdgeRef<'_>> = graph.edges().collect();
    // Stable: ties keep graph order, which is the reference's row id order.
    edges.sort_by(|a, b| {
        (a.source, a.target, &a.data.rel_type, &a.data.edge_class).cmp(&(
            b.source,
            b.target,
            &b.data.rel_type,
            &b.data.edge_class,
        ))
    });
    write_jsonl(
        &out_dir.join("edges.jsonl"),
        edges.iter().map(|edge| edge_row(*edge)),
    )?;
    Ok((nodes.len(), edges.len()))
}

fn write_jsonl<T: Serialize>(path: &Path, rows: impl Iterator<Item = T>) -> Result<(), String> {
    let file = std::fs::File::create(path).map_err(|e| format!("{}: {e}", path.display()))?;
    let mut out = BufWriter::new(file);
    for row in rows {
        let value = serde_json::to_value(&row).map_err(|e| e.to_string())?;
        write_python_dumps_sorted(&mut out, &value)
            .map_err(|e| format!("{}: {e}", path.display()))?;
        out.write_all(b"\n")
            .map_err(|e| format!("{}: {e}", path.display()))?;
    }
    out.flush().map_err(|e| format!("{}: {e}", path.display()))
}

/// Write `json.dumps(row, sort_keys=True, ensure_ascii=False)` of a flat
/// row: `", "` and `": "` separators, each key and value as `serde_json`
/// writes it.
fn write_python_dumps_sorted(out: &mut impl Write, value: &Value) -> std::io::Result<()> {
    let Value::Object(map) = value else {
        return serde_json::to_writer(out, value).map_err(std::io::Error::from);
    };
    let mut entries: Vec<(&String, &Value)> = map.iter().collect();
    entries.sort_by(|a, b| a.0.cmp(b.0));
    out.write_all(b"{")?;
    for (index, (key, value)) in entries.into_iter().enumerate() {
        if index > 0 {
            out.write_all(b", ")?;
        }
        serde_json::to_writer(&mut *out, key)?;
        out.write_all(b": ")?;
        serde_json::to_writer(&mut *out, value)?;
    }
    out.write_all(b"}")
}

fn parse_dump(language: &str, repo: &str, list: &str, out: &str) -> ExitCode {
    let Some(parser) = parser_for(language) else {
        eprintln!("no parser for {language}");
        return ExitCode::FAILURE;
    };
    let root = std::path::Path::new(repo);
    let Ok(listing) = std::fs::read_to_string(list) else {
        eprintln!("cannot read {list}");
        return ExitCode::FAILURE;
    };
    let mut files: Vec<String> = listing
        .lines()
        .filter(|l| !l.trim().is_empty())
        .map(|l| root.join(l.trim()).to_string_lossy().into_owned())
        .collect();
    files.sort();
    let started = std::time::Instant::now();
    let results = parser.parse_files(&files);
    let elapsed = started.elapsed();
    let prefix = format!("{}/", root.to_string_lossy().trim_end_matches('/'));
    let Ok(mut handle) = std::fs::File::create(out).map(std::io::BufWriter::new) else {
        eprintln!("cannot write {out}");
        return ExitCode::FAILURE;
    };
    for (path, result) in &results {
        let mut value = serde_json::to_value(result).unwrap_or_default();
        relativise(&mut value, &prefix);
        if let Some(object) = value.as_object_mut() {
            object.remove("warnings");
            object.insert(
                "file_path".to_owned(),
                serde_json::Value::String(path.strip_prefix(&prefix).unwrap_or(path).to_owned()),
            );
        }
        if writeln!(handle, "{value}").is_err() {
            return ExitCode::FAILURE;
        }
    }
    eprintln!(
        "{{\"language\": \"{language}\", \"files\": {}, \"seconds\": {:.2}}}",
        results.len(),
        elapsed.as_secs_f64()
    );
    ExitCode::SUCCESS
}

/// Strip the repository prefix from every string, as the Python dump does.
fn relativise(value: &mut serde_json::Value, prefix: &str) {
    match value {
        serde_json::Value::String(text) => {
            if let Some(rest) = text.strip_prefix(prefix) {
                *text = rest.to_owned();
            }
        }
        serde_json::Value::Array(items) => items.iter_mut().for_each(|v| relativise(v, prefix)),
        serde_json::Value::Object(map) => map.values_mut().for_each(|v| relativise(v, prefix)),
        _ => {}
    }
}

/// `pages <python-dump-dir> <out-dir>` (see the module comment).
fn pages(dump_dir: &str, out_dir: &str) -> Result<(), String> {
    use elitea_deepwiki_engine::wiki::parity;
    let runtime = tokio::runtime::Builder::new_multi_thread()
        .enable_all()
        .build()
        .map_err(|e| e.to_string())?;
    runtime.block_on(async {
        let dump = parity::load_dump(Path::new(dump_dir)).map_err(|e| e.to_string())?;
        let model = parity::StubModel::start(dump.page_answer)
            .await
            .map_err(|e| e.to_string())?;
        let (outcome, timing) = parity::run_dump(dump, &model)
            .await
            .map_err(|e| e.to_string())?;
        parity::write_outcome(Path::new(out_dir), &outcome, &model.requests(), timing)
            .map_err(|e| e.to_string())?;
        println!(
            "{{\"pages\": {}, \"requests\": {}, \"page_seconds\": {:.4}, \"context_seconds\": {:.4}}}",
            outcome.pages.pages.len(),
            model.requests().len(),
            timing.page_seconds,
            timing.context_seconds
        );
        Ok(())
    })
}

/// The options of `structure-dump`.
struct StructureArgs {
    repo: String,
    py_dump: PathBuf,
    out_dir: PathBuf,
    llm_base: String,
    planner: String,
    repo_name: Option<String>,
    branch: String,
    repo_identifier: Option<String>,
}

fn parse_structure_args(args: &[String]) -> Result<StructureArgs, String> {
    let mut positional = Vec::new();
    let mut options: std::collections::HashMap<&str, String> = std::collections::HashMap::new();
    let mut iter = args.iter();
    while let Some(arg) = iter.next() {
        match arg.as_str() {
            "--llm-base" | "--planner" | "--repo-name" | "--branch" | "--repo-identifier" => {
                options.insert(arg.as_str(), iter.next().ok_or(USAGE)?.clone());
            }
            _ => positional.push(arg.clone()),
        }
    }
    let [repo, py_dump, out_dir] =
        <[String; 3]>::try_from(positional).map_err(|_| USAGE.to_owned())?;
    Ok(StructureArgs {
        repo,
        py_dump: PathBuf::from(py_dump),
        out_dir: PathBuf::from(out_dir),
        llm_base: options
            .remove("--llm-base")
            .ok_or("--llm-base is required")?,
        planner: options
            .remove("--planner")
            .unwrap_or_else(|| "cluster".to_owned()),
        repo_name: options.remove("--repo-name"),
        branch: options
            .remove("--branch")
            .unwrap_or_else(|| "main".to_owned()),
        repo_identifier: options.remove("--repo-identifier"),
    })
}

fn read_jsonl<T: for<'de> serde::Deserialize<'de>>(path: &Path) -> Result<Vec<T>, String> {
    let text = std::fs::read_to_string(path).map_err(|e| format!("{}: {e}", path.display()))?;
    text.lines()
        .filter(|l| !l.trim().is_empty())
        .enumerate()
        .map(|(i, l)| {
            serde_json::from_str(l).map_err(|e| format!("{}:{}: {e}", path.display(), i + 1))
        })
        .collect()
}

#[allow(clippy::too_many_lines)]
fn structure_dump(args: &[String]) -> Result<(), String> {
    use elitea_deepwiki_engine::graph::clustering::{
        ClusterAssignment, ClusterGraph, Phase3Flags, RecordedCall, ReplayPartitioner, run_phase3,
    };
    use elitea_deepwiki_engine::llm::{ChatClient, ModelSettings, Transport, TransportSettings};
    use elitea_deepwiki_engine::runner::StopSignal;
    use elitea_deepwiki_engine::structure::index::{IndexNode, PlannerIndex};
    use elitea_deepwiki_engine::structure::model::LiveModel;
    use elitea_deepwiki_engine::structure::{self, PlannerChoice, StructureSettings, analysis};

    let options = parse_structure_args(args)?;
    let repo =
        std::fs::canonicalize(&options.repo).map_err(|e| format!("{}: {e}", options.repo))?;
    let repo_str = repo
        .to_str()
        .ok_or("the repository path is not UTF-8")?
        .to_owned();
    let repo_name = options.repo_name.clone().unwrap_or_else(|| {
        format!(
            "acme/{}",
            repo.file_name().and_then(|n| n.to_str()).unwrap_or("repo")
        )
    });
    let repo_identifier = options
        .repo_identifier
        .clone()
        .unwrap_or_else(|| format!("{repo_name}:{}:0123abcd", options.branch));
    std::fs::create_dir_all(&options.out_dir).map_err(|e| e.to_string())?;
    let mut timings: Vec<(&str, f64)> = Vec::new();

    // Phase 1 + 1c.
    let started = Instant::now();
    let discovery = discover::discover_files(&repo_str);
    let flags = Phase1cFlags::from_env().map_err(|e| e.to_string())?;
    let (mut graph, _report, _phase1c) =
        builder::build_index_graph_parsed(&repo_str, &discovery, &flags);
    timings.push(("phase1", started.elapsed().as_secs_f64()));

    // Phase 2 against the recording.
    let started = Instant::now();
    let recording = options.py_dump.join("recording.jsonl");
    let text =
        std::fs::read_to_string(&recording).map_err(|e| format!("{}: {e}", recording.display()))?;
    let mut store = ReplayStore::from_jsonl(&text).map_err(|e| e.to_string())?;
    let config = Phase2Config {
        profile: CalibrationProfile::from_env()?,
        ..Phase2Config::default()
    };
    let mut embedder = StandinEmbedder;
    let outcome = topology::run_phase2(&mut graph, &mut store, Some(&mut embedder), &config)
        .map_err(|e| format!("phase 2: {e}"))?;
    timings.push(("phase2", started.elapsed().as_secs_f64()));

    // Phase 3 on the graph (it reads the re-typed types), memberships replayed.
    let started = Instant::now();
    let cluster_graph = ClusterGraph::from_code_graph(&graph);
    let calls: Vec<RecordedCall> = read_jsonl(&options.py_dump.join("p3_leiden_calls.jsonl"))?;
    let mut partitioner = ReplayPartitioner::new(calls);
    let exclude_tests = std::env::var("DEEPWIKI_EXCLUDE_TESTS").is_ok_and(|v| {
        matches!(
            v.trim().to_lowercase().as_str(),
            "1" | "true" | "yes" | "on"
        )
    });
    let phase3 = run_phase3(
        &cluster_graph,
        outcome.hubs_for_phase3(),
        Phase3Flags {
            exclude_tests,
            calibrated_weights: config.profile == CalibrationProfile::Calibrated,
        },
        &mut partitioner,
    )
    .map_err(|e| format!("phase 3: {e}"))?;
    let assignments = phase3.assignments(&cluster_graph);
    timings.push(("phase3", started.elapsed().as_secs_f64()));
    let mut expected: Vec<ClusterAssignment> =
        read_jsonl(&options.py_dump.join("p3_assignments.jsonl"))?;
    expected.sort_by(|a, b| a.node_id.cmp(&b.node_id));
    let mut sorted = assignments.clone();
    sorted.sort_by(|a, b| a.node_id.cmp(&b.node_id));
    let assignments_equal = sorted == expected;
    if !assignments_equal {
        eprintln!("warning: the Phase 3 cluster columns differ from the dump's");
    }

    // The index rows: types as stored (before the lexical re-typing), the
    // cluster columns, and the edges Phase 2 persisted.
    for (id, previous) in &outcome.retyped {
        if let Some(node) = graph.node_mut(id) {
            node.symbol_type.clone_from(previous);
        }
    }
    let by_id: std::collections::HashMap<&str, &ClusterAssignment> = assignments
        .iter()
        .map(|a| (a.node_id.as_str(), a))
        .collect();
    let nodes: Vec<IndexNode> = graph
        .nodes()
        .map(|(id, data)| {
            let mut row = node_row(id, data);
            if let Some(a) = by_id.get(id) {
                row.macro_cluster = a.macro_cluster.and_then(|m| i64::try_from(m).ok());
                row.micro_cluster = a.micro_cluster.and_then(|m| i64::try_from(m).ok());
            }
            IndexNode::from_row(&row)
        })
        .collect();
    let graph_edges = graph.edge_rows();
    let edges_equal_graph = graph_edges == store.edges;
    let index = PlannerIndex::new(nodes, &store.edges, Some(repo_identifier.clone()));

    // generate_wiki: analyze_repository → generate_wiki_structure.
    let settings = StructureSettings::from_env(exclude_tests);
    let model_settings = ModelSettings::from_llm_settings(&serde_json::json!({
        "api_base": options.llm_base,
        "api_key": "stub-key",
        "model_name": "gpt-4o",
    }))
    .map_err(|e| e.to_string())?;
    let transport = Transport::new(&TransportSettings::default()).map_err(|e| e.to_string())?;
    let model = LiveModel {
        client: ChatClient::new(transport, model_settings),
        stop: StopSignal::default(),
    };
    let runtime = tokio::runtime::Builder::new_current_thread()
        .enable_all()
        .build()
        .map_err(|e| e.to_string())?;
    let (repository, spec, analysis_seconds, structure_seconds) = runtime.block_on(async {
        let started = Instant::now();
        let repository = analysis::analyze_repository(
            &model,
            &repo,
            &discovery,
            &repo_name,
            &options.branch,
            settings.structured_analysis,
        )
        .await
        .map_err(|e| e.to_string())?;
        let analysis_seconds = started.elapsed().as_secs_f64();
        let started = Instant::now();
        let spec = structure::plan_wiki_structure(
            &model,
            PlannerChoice::resolve(Some(&options.planner)),
            &repository,
            Some(&index),
            &settings,
        )
        .await
        .map_err(|e| e.to_string())?;
        Ok::<_, String>((
            repository,
            spec,
            analysis_seconds,
            started.elapsed().as_secs_f64(),
        ))
    })?;
    timings.push(("analysis", analysis_seconds));
    timings.push(("structure", structure_seconds));

    let write = |name: &str, text: String| {
        std::fs::write(options.out_dir.join(name), text).map_err(|e| format!("{name}: {e}"))
    };
    write("structure.json", format!("{}\n", spec.to_python_json()))?;
    let analysis_json = serde_json::json!({
        "repository_context": repository.repository_context,
        "repository_tree": repository.repository_tree,
        "readme_content": repository.readme_content,
    });
    write(
        "analysis.json",
        format!("{}\n", pyjson::dumps_indent2(&analysis_json)),
    )?;
    let seconds: serde_json::Map<String, Value> = timings
        .iter()
        .map(|(k, v)| {
            (
                (*k).to_owned(),
                serde_json::json!((v * 1000.0).round() / 1000.0),
            )
        })
        .collect();
    let summary = serde_json::json!({
        "repo": repo_str,
        "repo_name": repo_name,
        "repo_identifier": repo_identifier,
        "planner": options.planner,
        "nodes": graph.node_count(),
        "phase3_assignments_equal": assignments_equal,
        "phase3_replayed": partitioner.replayed,
        "phase2_edges_equal_graph_edges": edges_equal_graph,
        "sections": spec.sections.len(),
        "pages": spec.page_count(),
        "seconds": seconds,
    });
    write(
        "summary.json",
        format!("{}\n", pyjson::dumps_indent2(&summary)),
    )?;
    println!("{}", pyjson::dumps(&summary));
    Ok(())
}

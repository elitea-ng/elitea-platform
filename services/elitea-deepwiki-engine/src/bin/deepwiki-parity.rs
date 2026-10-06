//! Developer tool for the ADR-0026 parity gates. Not shipped in the image.
//!
//! ```text
//! deepwiki-parity parse-dump <language> <repo> <files.txt> <out.jsonl>
//! deepwiki-parity graph-dump <repo> <out-dir> [--parses-from <dir>] [--no-phase1c]
//! ```
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

use elitea_deepwiki_engine::graph::builder::{self, ParseResultsByLanguage};
use elitea_deepwiki_engine::graph::discover;
use elitea_deepwiki_engine::graph::flags::Phase1cFlags;
use elitea_deepwiki_engine::graph::{CodeGraph, EdgeRef, NodeData, edge_row, node_row};
use elitea_deepwiki_engine::parsers::model::ParseResult;
use elitea_deepwiki_engine::parsers::parser_for;
use serde::Serialize;
use serde_json::Value;
use std::io::{BufRead, BufWriter, Write};
use std::path::{Path, PathBuf};
use std::process::ExitCode;
use std::time::Instant;

const USAGE: &str = "usage: deepwiki-parity parse-dump <language> <repo> <files.txt> <out.jsonl>\n       deepwiki-parity graph-dump <repo> <out-dir> [--parses-from <dir>] [--no-phase1c]";

fn main() -> ExitCode {
    let args: Vec<String> = std::env::args().skip(1).collect();
    match args.first().map(String::as_str) {
        Some("parse-dump") if args.len() == 5 => parse_dump(&args[1], &args[2], &args[3], &args[4]),
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

fn graph_dump(args: &[String]) -> Result<(), String> {
    let mut positional = Vec::new();
    let mut parses_from: Option<PathBuf> = None;
    let mut phase1c = true;
    let mut iter = args.iter();
    while let Some(arg) = iter.next() {
        if arg == "--no-phase1c" {
            phase1c = false;
        } else if arg == "--parses-from" {
            parses_from = Some(PathBuf::from(iter.next().ok_or(USAGE)?));
        } else {
            positional.push(arg);
        }
    }
    let [repo, out_dir] = positional.as_slice() else {
        return Err(USAGE.to_owned());
    };
    // `Path.resolve()`, as the reference harness does.
    let repo = std::fs::canonicalize(repo).map_err(|e| format!("{repo}: {e}"))?;
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
    let write_started = Instant::now();
    let (nodes, edges) = write_rows(&graph, &out_dir)?;
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

/// Write the rows one at a time, in the reference's order: only the sort
/// order (references into the graph) is held, not every row.
fn write_rows(graph: &CodeGraph, out_dir: &Path) -> Result<(usize, usize), String> {
    let mut nodes: Vec<(&str, &NodeData)> = graph.nodes().collect();
    nodes.sort_by(|a, b| a.0.cmp(b.0));
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
        &out_dir.join("nodes.jsonl"),
        nodes.iter().map(|(id, data)| node_row(id, data)),
    )?;
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

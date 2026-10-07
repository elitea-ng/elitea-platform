//! Developer tool for the ADR-0026 Phase 3 clustering gate. Not shipped in
//! the image.
//!
//! The `parity/*.py` scripts named below wrote the Python dumps and scored
//! the results. They were deleted with the Python engine and exist up to
//! origin/main 1233e1582; the README's "Parity with the Python engine"
//! section says how to use them. The committed golden fixtures are dumps in
//! the same formats.
//!
//! ```text
//! deepwiki-cluster-parity replay <dump-dir>
//! deepwiki-cluster-parity leiden <dump-dir> <out-dir> [--runs N]
//! ```
//!
//! Both read a dump of `parity/python_phase3_dump.py` (the Python Phase 2
//! output graph, the hub list, every leidenalg call and Python's cluster
//! columns).
//!
//! `replay` runs the Rust Phase 3 with leidenalg's recorded memberships and
//! compares the cluster columns and the stats with Python's. Exit status 1
//! on any difference.
//!
//! `leiden` runs the vendored Leiden (a) on each recorded call's input,
//! writing `rust_calls.jsonl` (one membership per call, in call order), and
//! (b) through the whole Rust Phase 3, writing `rust_assignments.jsonl`
//! (sorted by node id) and `rust_summary.json` (stats and the median wall
//! time of `--runs` runs, default 5). `parity/compare_phase3.py` scores
//! both against leidenalg.

use elitea_deepwiki_engine::graph::clustering::dump::{self, Phase3Dump};
use elitea_deepwiki_engine::graph::clustering::{
    ClusterAssignment, LeidenPartitioner, PartitionRequest, Partitioner, Phase3Flags,
    ReplayPartitioner, run_phase3,
};
use serde_json::json;
use std::io::{BufWriter, Write};
use std::path::Path;
use std::process::ExitCode;
use std::time::Instant;

const USAGE: &str = "usage: deepwiki-cluster-parity replay <dump-dir>\n       deepwiki-cluster-parity leiden <dump-dir> <out-dir> [--runs N]";

fn main() -> ExitCode {
    let args: Vec<String> = std::env::args().skip(1).collect();
    let result = match args.first().map(String::as_str) {
        Some("replay") if args.len() == 2 => replay(Path::new(&args[1])),
        Some("leiden") if args.len() == 3 || args.len() == 5 => {
            let runs = if args.len() == 5 && args[3] == "--runs" {
                args[4].parse().unwrap_or(5)
            } else {
                5
            };
            leiden(Path::new(&args[1]), Path::new(&args[2]), runs)
        }
        _ => {
            eprintln!("{USAGE}");
            return ExitCode::from(2);
        }
    };
    match result {
        Ok(true) => ExitCode::SUCCESS,
        Ok(false) => ExitCode::FAILURE,
        Err(error) => {
            eprintln!("deepwiki-cluster-parity: {error}");
            ExitCode::from(2)
        }
    }
}

type Error = Box<dyn std::error::Error>;

fn flags_of(dump: &Phase3Dump) -> Phase3Flags {
    let flags = &dump.summary["flags"];
    Phase3Flags {
        exclude_tests: flags["exclude_tests"].as_bool().unwrap_or(false),
        calibrated_weights: flags["weight_calibration_profile"] == "calibrated",
    }
}

fn sorted(mut rows: Vec<ClusterAssignment>) -> Vec<ClusterAssignment> {
    rows.sort_by(|a, b| a.node_id.cmp(&b.node_id));
    rows
}

fn replay(dir: &Path) -> Result<bool, Error> {
    let dump = dump::load(dir)?;
    let mut partitioner = ReplayPartitioner::new(dump.calls.clone());
    let started = Instant::now();
    let out = run_phase3(&dump.graph, &dump.hubs, flags_of(&dump), &mut partitioner)?;
    let seconds = started.elapsed().as_secs_f64();
    let rows = sorted(out.assignments(&dump.graph));
    let mut differing = 0;
    for (got, want) in rows.iter().zip(&dump.expected) {
        if got != want {
            if differing < 5 {
                eprintln!("differs: rust {got:?}\n       python {want:?}");
            }
            differing += 1;
        }
    }
    let stats = serde_json::to_value(&out.stats)?;
    let stats_equal = stats == dump.summary["phase3_stats"];
    if !stats_equal {
        eprintln!(
            "stats differ:\n  rust   {stats}\n  python {}",
            dump.summary["phase3_stats"]
        );
    }
    let equal = rows.len() == dump.expected.len() && differing == 0 && stats_equal;
    println!(
        "{}",
        json!({
            "dump": dir.display().to_string(),
            "nodes": rows.len(),
            "leiden_calls": dump.calls.len(),
            "replayed": partitioner.replayed,
            "rows_differing": differing,
            "stats_equal": stats_equal,
            "max_weight_difference": partitioner.max_weight_difference,
            "identical": equal,
            "rust_seconds": seconds,
        })
    );
    Ok(equal)
}

fn leiden(dir: &Path, out_dir: &Path, runs: usize) -> Result<bool, Error> {
    let dump = dump::load(dir)?;
    std::fs::create_dir_all(out_dir)?;

    // (a) The vendored Leiden on each recorded input.
    let mut calls = BufWriter::new(std::fs::File::create(out_dir.join("rust_calls.jsonl"))?);
    let mut leiden_seconds = 0.0;
    for call in &dump.calls {
        let names: Vec<&str> = call.nodes.iter().map(String::as_str).collect();
        let request = PartitionRequest {
            level: call.level,
            names: &names,
            edges: &call.edges,
            resolution: call.resolution,
            seed: call.seed,
        };
        let started = Instant::now();
        let membership = LeidenPartitioner.partition(&request)?;
        leiden_seconds += started.elapsed().as_secs_f64();
        writeln!(
            calls,
            "{}",
            json!({"level": call.level, "membership": membership})
        )?;
    }
    calls.flush()?;

    // (b) The whole Rust Phase 3 with the vendored Leiden.
    let flags = flags_of(&dump);
    let mut times = Vec::with_capacity(runs.max(1));
    let mut result = None;
    for _ in 0..runs.max(1) {
        let started = Instant::now();
        let out = run_phase3(&dump.graph, &dump.hubs, flags, &mut LeidenPartitioner)?;
        times.push(started.elapsed().as_secs_f64());
        result = Some(out);
    }
    let Some(out) = result else {
        return Ok(false);
    };
    times.sort_by(f64::total_cmp);
    let mut rows = BufWriter::new(std::fs::File::create(
        out_dir.join("rust_assignments.jsonl"),
    )?);
    for row in sorted(out.assignments(&dump.graph)) {
        writeln!(rows, "{}", serde_json::to_string(&row)?)?;
    }
    rows.flush()?;
    let summary = json!({
        "dump": dir.display().to_string(),
        "phase3_stats": out.stats,
        "phase3_seconds_median": times[times.len() / 2],
        "phase3_seconds_all": times,
        "per_call_leiden_seconds": leiden_seconds,
    });
    std::fs::write(
        out_dir.join("rust_summary.json"),
        serde_json::to_string_pretty(&summary)?,
    )?;
    println!("{summary}");
    Ok(true)
}

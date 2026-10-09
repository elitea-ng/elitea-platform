//! Timing budgets for the `split_out` and `aggregate` pipeline nodes.
//!
//! `cargo bench` passes `--bench`: every case runs 200 timed iterations after a
//! warm-up and the process fails when a p99 exceeds its budget. Without
//! `--bench` (for example `cargo test --all-targets`) each case runs once as a
//! smoke check and no timing is asserted.

use std::process::exit;
use std::time::Instant;

use elitea_worker_rust::agents::graph::shaping_bench::{AggregateBench, SplitOutBench};
use serde_json::{Value, json};

const WARMUP: usize = 20;
const ITERATIONS: usize = 200;

type Runner = Box<dyn Fn() -> Result<Value, String>>;

struct Case {
    name: &'static str,
    budget_ms: f64,
    expect_error: Option<&'static str>,
    run: Runner,
}

fn die(message: &str) -> ! {
    eprintln!("graph_shaping: {message}");
    exit(1)
}

fn split(yaml: &str, source: Value) -> Runner {
    let node = SplitOutBench::new(yaml).unwrap_or_else(|error| die(&error));
    Box::new(move || node.run(&source))
}

fn aggregate(yaml: &str, source: Value) -> Runner {
    let node = AggregateBench::new(yaml).unwrap_or_else(|error| die(&error));
    Box::new(move || node.run(&source))
}

fn cases() -> Vec<Case> {
    let list =
        "id: s\ntype: split_out\nsource: src\noutput: [out]\nsplit: {mode: list}\ndestination: d\n";
    let rows = "id: s\ntype: split_out\nsource: src\noutput: [out]\nsplit: {mode: rows_field, path: /f}\ndestination: d\nretain: {mode: all}\n";
    let head = "id: a\ntype: aggregate\nsource: src\noutput: [out]\n";
    let ints = |n: usize| Value::Array((0..n).map(|i| json!(i)).collect());
    let parents = (0..1_000)
        .map(|i| json!({"id": i, "f": [1, 2, 3, 4, 5]}))
        .collect();
    let plain = (0..10_000)
        .map(|i| json!({"k": i % 1_000, "v": i}))
        .collect();
    let envelopes = (0..6_500)
        .map(|i| json!({"parent_index": i % 1_000, "position": i / 1_000, "data": {"id": i % 1_000, "f": i}}))
        .collect();
    let small = (0..2_000).map(|i| json!({"a": i, "b": "x"})).collect();
    let group = "group_by: [{path: /k, output: k}]\noperations: [{operation: count_rows, output: n}, {operation: sum_int, field: {path: /v}, output: s}]\n";
    let regroup = "layout: split_out\nregroup: parent\noperations: [{operation: collect, field: {path: /f}, output: f}]\n";
    let collect = "operations: [{operation: collect_rows, output: r, retain: {mode: all}}]\n";
    vec![
        Case {
            name: "split_out list 6500 rows",
            budget_ms: 20.0,
            expect_error: None,
            run: split(list, ints(6_500)),
        },
        Case {
            name: "split_out list 10000 fail-fast",
            budget_ms: 20.0,
            expect_error: Some("limits.values"),
            run: split(list, ints(10_000)),
        },
        Case {
            name: "split_out rows_field 1000x5 all",
            budget_ms: 20.0,
            expect_error: None,
            run: split(rows, Value::Array(parents)),
        },
        Case {
            name: "aggregate group_by 10000->1000",
            budget_ms: 3.0,
            expect_error: None,
            run: aggregate(&format!("{head}{group}"), Value::Array(plain)),
        },
        Case {
            name: "aggregate regroup 6500 collect",
            budget_ms: 2.0,
            expect_error: None,
            run: aggregate(&format!("{head}{regroup}"), Value::Array(envelopes)),
        },
        Case {
            name: "aggregate collect_rows 2000",
            budget_ms: 5.0,
            expect_error: None,
            run: aggregate(&format!("{head}{collect}"), Value::Array(small)),
        },
    ]
}

/// Total compact bytes of the emitted list and its largest row (the analytic
/// peak of the output: the byte budget plus one row).
fn output_bytes(output: &Value) -> (usize, usize) {
    let size = |value: &Value| serde_json::to_vec(value).map_or(0, |bytes| bytes.len());
    let largest = output
        .as_array()
        .map_or(0, |rows| rows.iter().map(size).max().unwrap_or(0));
    (size(output), largest)
}

fn check(case: &Case, result: &Result<Value, String>) {
    match (&case.expect_error, result) {
        (None, Ok(_)) => {}
        (Some(field), Err(message)) if message.contains(field) => {}
        (_, other) => die(&format!(
            "{} produced an unexpected result: {:?}",
            case.name,
            other.as_ref().map(|_| "ok")
        )),
    }
}

fn percentile(sorted: &[f64], percent: usize) -> f64 {
    let rank = (sorted.len() * percent).div_ceil(100).max(1);
    sorted.get(rank - 1).copied().unwrap_or(f64::NAN)
}

fn main() {
    let timed = std::env::args().any(|argument| argument == "--bench");
    let mut failed = false;
    if timed {
        println!(
            "{:<34} {:>8} {:>8} {:>8} {:>8} {:>8} {:>10} {:>9}",
            "case", "min ms", "p50 ms", "p99 ms", "max ms", "budget", "out bytes", "max row"
        );
    }
    for case in cases() {
        let first = (case.run)();
        check(&case, &first);
        if !timed {
            continue;
        }
        let (bytes, largest) = first.as_ref().map_or((0, 0), output_bytes);
        for _ in 0..WARMUP {
            drop((case.run)());
        }
        let mut millis = Vec::with_capacity(ITERATIONS);
        for _ in 0..ITERATIONS {
            let started = Instant::now();
            let result = (case.run)();
            millis.push(started.elapsed().as_secs_f64() * 1_000.0);
            drop(result);
        }
        millis.sort_by(f64::total_cmp);
        let p99 = percentile(&millis, 99);
        println!(
            "{:<34} {:>8.3} {:>8.3} {:>8.3} {:>8.3} {:>8.1} {:>10} {:>9}",
            case.name,
            millis.first().copied().unwrap_or(f64::NAN),
            percentile(&millis, 50),
            p99,
            millis.last().copied().unwrap_or(f64::NAN),
            case.budget_ms,
            bytes,
            largest
        );
        if p99 > case.budget_ms {
            eprintln!(
                "graph_shaping: {} p99 {p99:.3} ms exceeds {} ms",
                case.name, case.budget_ms
            );
            failed = true;
        }
    }
    if failed {
        exit(1);
    }
}

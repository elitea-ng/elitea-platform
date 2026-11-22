//! Timing budgets for FU-347 (5b-1 PR B):
//! - 128-node no-settings compile guard
//! - Settings wrapper overhead p99 <= 50 us on no-settings and deterministic paths.
//!
//! `cargo bench --offline --locked --features node-settings-bench --bench node_settings`
//! passes `--bench`: every case runs 200 timed iterations after warmup and fails if
//! any p99 exceeds its budget. Without `--bench` (e.g. `cargo test --all-targets`),
//! each case runs once as a smoke check with no timing assertion.

use std::process::exit;
use std::time::Instant;

use elitea_worker_rust::agents::graph::node_settings_bench::{
    DeterministicSettingsBench, build_128_node_no_settings_yaml, compile_pipeline,
};

const COMPILE_WARMUP: usize = 5;
const COMPILE_ITERATIONS: usize = 30;
const NODE_WARMUP: usize = 50;
const NODE_ITERATIONS: usize = 500;

fn die(message: &str) -> ! {
    eprintln!("node_settings bench: {message}");
    exit(1)
}

fn percentile_us(samples: &mut [f64], num: usize, den: usize) -> f64 {
    samples.sort_by(f64::total_cmp);
    let idx = (samples.len() * num / den).min(samples.len().saturating_sub(1));
    samples[idx]
}

fn run_case<F>(
    name: &str,
    bench_mode: bool,
    warmup: usize,
    iterations: usize,
    budget_us: f64,
    mut body: F,
) -> (f64, f64)
where
    F: FnMut() -> Result<usize, String>,
{
    if !bench_mode {
        if let Err(err) = body() {
            die(&format!("{name}: {err}"));
        }
        println!("{name}: smoke ok");
        return (0.0, 0.0);
    }
    for _ in 0..warmup {
        if let Err(err) = body() {
            die(&format!("{name} (warmup): {err}"));
        }
    }
    let mut samples = Vec::with_capacity(iterations);
    for _ in 0..iterations {
        let start = Instant::now();
        if let Err(err) = body() {
            die(&format!("{name}: {err}"));
        }
        samples.push(start.elapsed().as_secs_f64() * 1_000_000.0);
    }
    let p50 = percentile_us(&mut samples, 50, 100);
    let p99 = percentile_us(&mut samples, 99, 100);
    println!("{name}: p50={p50:.2} us, p99={p99:.2} us (budget <= {budget_us:.2} us)");
    if p99 > budget_us {
        die(&format!(
            "{name} p99 {p99:.2} us exceeded budget {budget_us:.2} us"
        ));
    }
    (p50, p99)
}

fn main() {
    let bench_mode = std::env::args().any(|arg| arg == "--bench");
    let yaml_128 = build_128_node_no_settings_yaml();
    let rt = tokio::runtime::Builder::new_current_thread()
        .enable_all()
        .build()
        .unwrap_or_else(|e| die(&e.to_string()));
    let node_bench = DeterministicSettingsBench::new().unwrap_or_else(|e| die(&e));

    // 1. 128-node no-settings compile guard (50 ms p99 budget; typical ~5-15 ms).
    run_case(
        "compile 128-node no-settings",
        bench_mode,
        COMPILE_WARMUP,
        COMPILE_ITERATIONS,
        50_000.0,
        || compile_pipeline(&yaml_128),
    );

    // 2. Unwrapped StateModifier (no-settings baseline): p99 <= 50 us.
    let (_u50, u99) = run_case(
        "state_modifier unwrapped (no-settings)",
        bench_mode,
        NODE_WARMUP,
        NODE_ITERATIONS,
        50.0,
        || rt.block_on(node_bench.run_unwrapped()),
    );

    // 3. Wrapped StateModifier success path: p99 <= 50 us.
    let (_w50, w99) = run_case(
        "state_modifier wrapped success",
        bench_mode,
        NODE_WARMUP,
        NODE_ITERATIONS,
        50.0,
        || rt.block_on(node_bench.run_wrapped_ok()),
    );

    // 4. Wrapped StateModifier continue failure path: p99 <= 50 us.
    run_case(
        "state_modifier wrapped continue (settled failure)",
        bench_mode,
        NODE_WARMUP,
        NODE_ITERATIONS,
        50.0,
        || rt.block_on(node_bench.run_wrapped_continue()),
    );

    // 5. Wrapped StateModifier execute_once skip path: p99 <= 50 us.
    run_case(
        "state_modifier wrapped execute_once skip",
        bench_mode,
        NODE_WARMUP,
        NODE_ITERATIONS,
        50.0,
        || rt.block_on(node_bench.run_wrapped_once_skip()),
    );

    if bench_mode {
        println!(
            "settings wrapper delta (p99): {:.2} us",
            (w99 - u99).max(0.0)
        );
    }
}

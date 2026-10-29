#![cfg_attr(
    not(test),
    deny(
        clippy::expect_used,
        clippy::panic,
        clippy::todo,
        clippy::unimplemented,
        clippy::unwrap_used
    )
)]

use std::io;
use std::process::ExitCode;

/// Stack for each runtime worker thread. A saved agent that calls a saved pipeline that calls
/// another agent nests one agent loop per level on the polling thread; tokio's 2 MiB default
/// leaves too little headroom for that depth.
const WORKER_THREAD_STACK_BYTES: usize = 8 * 1024 * 1024;

fn main() -> ExitCode {
    let runtime = match tokio::runtime::Builder::new_multi_thread()
        .enable_all()
        .thread_stack_size(WORKER_THREAD_STACK_BYTES)
        .build()
    {
        Ok(runtime) => runtime,
        Err(error) => {
            eprintln!("the async runtime could not start: {error}");
            return ExitCode::FAILURE;
        }
    };
    runtime.block_on(run_worker())
}

async fn run_worker() -> ExitCode {
    elitea_worker_rust::diagnostics::install_redacted_panic_hook();
    if let Err(error) = elitea_worker_rust::diagnostics::install_tls_crypto_provider() {
        eprintln!("{error}");
        return ExitCode::FAILURE;
    }
    let mut diagnostics = match elitea_worker_rust::diagnostics::install_tracing_subscriber() {
        Ok(diagnostics) => diagnostics,
        Err(error) => {
            eprintln!("{error}");
            return ExitCode::FAILURE;
        }
    };
    let exit = match elitea_worker_rust::run(std::env::args().skip(1), io::stdout()).await {
        Ok(()) => ExitCode::SUCCESS,
        Err(error) => {
            tracing::error!(
                event = "worker_exit",
                error_code = error.code(),
                retryable = error.retryable(),
            );
            eprintln!("{error}");
            ExitCode::FAILURE
        }
    };
    if diagnostics.shutdown().is_err() {
        tracing::warn!(
            event = "telemetry_shutdown_failed",
            error_code = "telemetry.shutdown_failed",
            retryable = true,
        );
    }
    exit
}

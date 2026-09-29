//! Dedicated supervisor entry point. Build only with sandbox-supervisor.
use elitea_worker_rust::{diagnostics, sandbox::process};
use std::{path::PathBuf, process::ExitCode};

#[tokio::main(flavor = "multi_thread", worker_threads = 2)]
async fn main() -> ExitCode {
    diagnostics::install_redacted_panic_hook();
    if let Err(error) = diagnostics::install_tls_crypto_provider() {
        eprintln!("{error}");
        return ExitCode::FAILURE;
    }
    let mut tracing = match diagnostics::install_tracing_subscriber() {
        Ok(value) => value,
        Err(error) => {
            eprintln!("{error}");
            return ExitCode::FAILURE;
        }
    };
    let args: Vec<_> = std::env::args_os().skip(1).collect();
    let exit = if args.len() == 2 && args[0] == "--config" {
        match process::run(&PathBuf::from(&args[1])).await {
            Ok(()) => ExitCode::SUCCESS,
            Err(error) => {
                tracing::error!(event="sandbox_supervisor_exit", reason=%error);
                ExitCode::FAILURE
            }
        }
    } else {
        eprintln!("usage: elitea-sandbox-supervisor --config <absolute-path>");
        ExitCode::FAILURE
    };
    if tracing.shutdown().is_err() {
        eprintln!("sandbox telemetry shutdown failed");
    }
    exit
}

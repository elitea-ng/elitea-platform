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
    let exit = if !args.is_empty()
        && args.len() <= 8
        && args
            .chunks(2)
            .all(|pair| pair.len() == 2 && pair[0] == "--config")
    {
        let paths: Vec<_> = args
            .chunks_exact(2)
            .map(|pair| PathBuf::from(&pair[1]))
            .collect();
        match process::run_profiles(&paths).await {
            Ok(()) => ExitCode::SUCCESS,
            Err(error) => {
                tracing::error!(event="sandbox_supervisor_exit", reason=%error);
                ExitCode::FAILURE
            }
        }
    } else {
        eprintln!(
            "usage: elitea-sandbox-supervisor --config <absolute-path> [--config <absolute-path> ...]"
        );
        ExitCode::FAILURE
    };
    if tracing.shutdown().is_err() {
        eprintln!("sandbox telemetry shutdown failed");
    }
    exit
}

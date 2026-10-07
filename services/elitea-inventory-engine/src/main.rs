//! `elitea-inventory-engine [serve | healthcheck | --version]`.

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

use elitea_engine_sidecar::{healthcheck, server, telemetry};
use elitea_inventory_engine::build_runner;
use elitea_inventory_engine::config::Settings;
use std::process::ExitCode;
use std::time::Duration;

const USAGE: &str = "usage: elitea-inventory-engine [serve | healthcheck | --version]";

/// The OTLP `service.name` of this engine's spans.
const SERVICE_NAME: &str = "elitea-inventory-engine";

fn main() -> ExitCode {
    match std::env::args().nth(1).as_deref() {
        None | Some("serve") => on_runtime(serve()),
        Some("healthcheck") => on_runtime(probe()),
        Some("--version") => {
            println!("elitea-inventory-engine {}", env!("CARGO_PKG_VERSION"));
            ExitCode::SUCCESS
        }
        Some(_) => {
            eprintln!("{USAGE}");
            ExitCode::from(2)
        }
    }
}

fn on_runtime(future: impl Future<Output = ExitCode>) -> ExitCode {
    match tokio::runtime::Builder::new_multi_thread()
        .enable_all()
        .build()
    {
        Ok(runtime) => {
            let code = runtime.block_on(future);
            runtime.shutdown_background();
            code
        }
        Err(error) => {
            eprintln!("cannot start the async runtime: {error}");
            ExitCode::FAILURE
        }
    }
}

fn settings() -> Option<Settings> {
    match Settings::from_env() {
        Ok(settings) => Some(settings),
        Err(error) => {
            eprintln!("{error}");
            None
        }
    }
}

async fn probe() -> ExitCode {
    let Some(settings) = settings() else {
        return ExitCode::FAILURE;
    };
    match healthcheck::probe(&settings.engine_socket, Duration::from_secs(2)).await {
        Ok(()) => ExitCode::SUCCESS,
        Err(error) => {
            eprintln!("{error}");
            ExitCode::FAILURE
        }
    }
}

async fn serve() -> ExitCode {
    let telemetry = telemetry::init(SERVICE_NAME);
    let code = serve_engine().await;
    telemetry.shutdown().await;
    code
}

async fn serve_engine() -> ExitCode {
    let Some(settings) = settings() else {
        return ExitCode::FAILURE;
    };
    let runner = match build_runner(&settings) {
        Ok(runner) => runner,
        Err(error) => {
            tracing::error!(%error, "cannot start the runner");
            return ExitCode::FAILURE;
        }
    };
    let listener = match server::bind(&settings.engine_socket) {
        Ok(listener) => listener,
        Err(error) => {
            tracing::error!(socket = %settings.engine_socket.display(), %error, "cannot bind the engine socket");
            return ExitCode::FAILURE;
        }
    };
    tracing::info!(socket = %settings.engine_socket.display(), runner = runner.name(), "inventory engine sidecar listening");
    match server::serve(listener, runner, shutdown_signal()).await {
        Ok(()) => ExitCode::SUCCESS,
        Err(error) => {
            tracing::error!(%error, "inventory engine sidecar stopped");
            ExitCode::FAILURE
        }
    }
}

/// SIGTERM (the kubelet) or Ctrl-C.
async fn shutdown_signal() {
    let terminate = async {
        match tokio::signal::unix::signal(tokio::signal::unix::SignalKind::terminate()) {
            Ok(mut signal) => {
                signal.recv().await;
            }
            Err(_) => std::future::pending::<()>().await,
        }
    };
    tokio::select! {
        _ = tokio::signal::ctrl_c() => {}
        () = terminate => {}
    }
}

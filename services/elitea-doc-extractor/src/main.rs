//! `elitea-doc-extractor [serve | healthcheck | --version]`.
//!
//! `serve` (the default) binds the socket and serves `elitea.extract.v1`
//! until SIGTERM or SIGINT. `healthcheck` connects to the same socket and
//! asks `Capabilities`; it exits 0 when the service answers, so a distroless
//! image, which has no shell and no curl, can probe itself.

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

use std::process::ExitCode;
use std::sync::Arc;
use std::time::Duration;

use elitea_doc_extractor::client;
use elitea_doc_extractor::config::Config;
use elitea_doc_extractor::pb::document_extract_service_server::DocumentExtractServiceServer;
use elitea_doc_extractor::service::{ExitProcess, Extractor};
use elitea_engine_sidecar::cgroup::Cgroup;
use tokio_stream::wrappers::UnixListenerStream;
use tonic::transport::Server;

const USAGE: &str = "usage: elitea-doc-extractor [serve | healthcheck | --version]";

/// The largest gRPC message accepted: one streamed chunk plus its framing.
/// The document itself is bounded by the input cap, not by this.
const MAX_MESSAGE_BYTES: usize = 4 << 20;

fn main() -> ExitCode {
    match std::env::args().nth(1).as_deref() {
        None | Some("serve") => on_runtime(serve()),
        Some("healthcheck") => on_runtime(healthcheck()),
        Some("--version") => {
            println!("elitea-doc-extractor {}", env!("CARGO_PKG_VERSION"));
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

fn config() -> Option<Config> {
    match Config::from_lookup(|name| std::env::var(name).ok()) {
        Ok(config) => Some(config),
        Err(error) => {
            eprintln!("elitea-doc-extractor: {error}");
            None
        }
    }
}

async fn healthcheck() -> ExitCode {
    let Some(config) = config() else {
        return ExitCode::FAILURE;
    };
    let probe = async {
        let channel = client::channel(&config.socket).map_err(|error| error.to_string())?;
        client::capabilities(channel)
            .await
            .map_err(|status| status.to_string())
    };
    match tokio::time::timeout(Duration::from_secs(2), probe).await {
        Ok(Ok(_)) => ExitCode::SUCCESS,
        Ok(Err(error)) => {
            eprintln!("elitea-doc-extractor: not serving: {error}");
            ExitCode::FAILURE
        }
        Err(_) => {
            eprintln!("elitea-doc-extractor: no answer within 2 s");
            ExitCode::FAILURE
        }
    }
}

async fn serve() -> ExitCode {
    let filter = tracing_subscriber::EnvFilter::try_from_default_env()
        .unwrap_or_else(|_| tracing_subscriber::EnvFilter::new("info"));
    let _ = tracing_subscriber::fmt()
        .with_env_filter(filter)
        .with_writer(std::io::stderr)
        .try_init();
    let Some(mut config) = config() else {
        return ExitCode::FAILURE;
    };
    if let Some(limit) = Cgroup::discover().and_then(|cgroup| cgroup.memory_max())
        && let Some(lowered) = config.fit_memory(limit)
    {
        tracing::warn!(
            event = "extractor_input_cap_lowered",
            memory_limit = limit,
            max_input_bytes = lowered,
            "the input cap was lowered to fit the container's memory limit"
        );
    }
    let listener = match elitea_engine_sidecar::bind(&config.socket) {
        Ok(listener) => listener,
        Err(error) => {
            tracing::error!(%error, socket = %config.socket.display(), "cannot bind the socket");
            return ExitCode::FAILURE;
        }
    };
    tracing::info!(
        socket = %config.socket.display(),
        max_input_bytes = config.caps.max_input_bytes,
        max_pages = config.caps.max_pages,
        max_output_bytes = config.caps.max_output_bytes,
        timeout_ms = u64::try_from(config.caps.timeout.as_millis()).unwrap_or(u64::MAX),
        concurrency = config.concurrency,
        "elitea-doc-extractor serving"
    );
    let service = Extractor::new(
        config.caps,
        config.concurrency,
        config.stream_idle,
        Arc::new(ExitProcess),
    );
    let served = Server::builder()
        .add_service(
            DocumentExtractServiceServer::new(service).max_decoding_message_size(MAX_MESSAGE_BYTES),
        )
        .serve_with_incoming_shutdown(UnixListenerStream::new(listener), shutdown())
        .await;
    let _ = std::fs::remove_file(&config.socket);
    match served {
        Ok(()) => ExitCode::SUCCESS,
        Err(error) => {
            tracing::error!(%error, "elitea-doc-extractor stopped");
            ExitCode::FAILURE
        }
    }
}

async fn shutdown() {
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
    tracing::info!("elitea-doc-extractor draining");
}

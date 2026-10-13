//! `elitea-doc-extractor [serve | healthcheck | --version]`.
//!
//! `serve` (the default) binds the socket and serves `elitea.extract.v1`
//! until SIGTERM or SIGINT. `healthcheck` connects to the same socket and
//! asks `Capabilities`; it exits 0 when the service answers AND has a slot
//! that is not held by an overrun (`service::is_healthy`), so a distroless
//! image, which has no shell and no curl, can probe itself, and a wedged
//! process (every slot held by an extraction that cannot be killed) fails
//! its liveness probe and is restarted even if its own exit did not happen.
//! The probe runs as the process's own uid, which the service serves
//! `Capabilities` to whatever peer uids are configured.

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

use std::path::Path;
use std::process::ExitCode;
use std::sync::Arc;
use std::time::Duration;

use elitea_doc_extractor::config::Config;
use elitea_doc_extractor::pb::CapabilitiesRequest;
use elitea_doc_extractor::pb::document_extract_service_client::DocumentExtractServiceClient;
use elitea_doc_extractor::pb::document_extract_service_server::DocumentExtractServiceServer;
use elitea_doc_extractor::service::{ExitProcess, Extractor, MAX_MESSAGE_BYTES, is_healthy};
use elitea_engine_sidecar::cgroup::Cgroup;
use hyper_util::rt::TokioIo;
use tokio_stream::wrappers::UnixListenerStream;
use tonic::transport::{Endpoint, Server, Uri};
use tower::service_fn;

const USAGE: &str = "usage: elitea-doc-extractor [serve | healthcheck | --version]";

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

/// The probe: one `Capabilities` call over the socket, read for health.
async fn healthcheck() -> ExitCode {
    let Some(config) = config() else {
        return ExitCode::FAILURE;
    };
    match tokio::time::timeout(Duration::from_secs(2), probe(&config.socket)).await {
        Ok(Ok(answer)) if is_healthy(&answer) => ExitCode::SUCCESS,
        Ok(Ok(answer)) => {
            eprintln!(
                "elitea-doc-extractor: wedged: {} of {} slots are held by extractions that \
                 outran their timeout",
                answer.overruns, answer.concurrency
            );
            ExitCode::FAILURE
        }
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

/// `Capabilities` over the socket at `path`. The authority is a placeholder:
/// HTTP/2 needs one, and nothing routes on it.
async fn probe(
    path: &Path,
) -> Result<elitea_doc_extractor::pb::CapabilitiesResponse, Box<dyn std::error::Error>> {
    let path = path.to_path_buf();
    let channel = Endpoint::try_from("http://extractor.local")?
        .connect_timeout(Duration::from_secs(1))
        .connect_with_connector_lazy(service_fn(move |_: Uri| {
            let path = path.clone();
            async move {
                tokio::net::UnixStream::connect(path)
                    .await
                    .map(TokioIo::new)
            }
        }));
    Ok(DocumentExtractServiceClient::new(channel)
        .capabilities(CapabilitiesRequest {})
        .await?
        .into_inner())
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
            memory_limit_for_caps = config.memory_limit_for_caps(),
            max_input_bytes = lowered,
            "the configured input cap does not fit the container's memory limit \
             several times over; lowered (never raised) to what does"
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
        max_images = config.images.max_images,
        max_image_bytes = config.images.max_image_bytes,
        max_image_total_bytes = config.images.max_total_bytes,
        concurrency = config.capacity.concurrency,
        slot_wait_ms = u64::try_from(config.capacity.slot_wait.as_millis()).unwrap_or(u64::MAX),
        peer_uids = ?config.peer_uids,
        "elitea-doc-extractor serving"
    );
    let service = Extractor::new(
        config.caps,
        config.capacity,
        config.peer_uids,
        Arc::new(ExitProcess),
    )
    .with_image_caps(config.images);
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

//! `elitea-vector` serves `elitea.vector.v1.VectorService` (ADR-0031).
//!
//! `elitea-vector healthcheck` probes a running instance's `/healthz` on
//! loopback and exits 0 or 1; a distroless image has no curl for its
//! container health check.

use std::process::ExitCode;
use std::sync::Arc;

use elitea_vector::auth::{
    Authenticator, CachePolicy, CachingIntrospector, GrpcIntrospector, unix_now,
};
use elitea_vector::config::Config;
use elitea_vector::pb::vector_service_server::VectorServiceServer;
use elitea_vector::service::VectorService;
use elitea_vector::store::{CollectionSettings, Store};
use qdrant_client::Qdrant;
use tokio::io::{AsyncReadExt as _, AsyncWriteExt as _};
use tonic::transport::{Certificate, ClientTlsConfig, Endpoint, Identity, Server, ServerTlsConfig};

/// A full Upsert batch at the largest dimension fits with room to spare.
const MAX_MESSAGE_BYTES: usize = 64 << 20;

fn main() -> ExitCode {
    elitea_vector::install_crypto_provider();
    let runtime = match tokio::runtime::Builder::new_multi_thread()
        .enable_all()
        .build()
    {
        Ok(runtime) => runtime,
        Err(error) => {
            eprintln!("elitea-vector: no runtime: {error}");
            return ExitCode::FAILURE;
        }
    };
    if std::env::args().nth(1).as_deref() == Some("healthcheck") {
        return runtime.block_on(healthcheck());
    }
    runtime.block_on(async {
        let telemetry = elitea_engine_sidecar::telemetry::init("elitea-vector");
        let result = run().await;
        telemetry.shutdown().await;
        match result {
            Ok(()) => ExitCode::SUCCESS,
            Err(error) => {
                tracing::error!(%error, "elitea-vector stopped");
                ExitCode::FAILURE
            }
        }
    })
}

async fn healthcheck() -> ExitCode {
    let port = std::env::var("ELITEA_VECTOR_HEALTH_ADDR")
        .ok()
        .and_then(|value| value.parse::<std::net::SocketAddr>().ok())
        .map_or(9471, |address| address.port());
    let probe = async {
        let mut stream = tokio::net::TcpStream::connect(("127.0.0.1", port)).await?;
        stream
            .write_all(b"GET /healthz HTTP/1.1\r\nhost: localhost\r\n\r\n")
            .await?;
        let mut response = Vec::new();
        stream.read_to_end(&mut response).await?;
        Ok::<bool, std::io::Error>(response.starts_with(b"HTTP/1.1 200"))
    };
    match tokio::time::timeout(std::time::Duration::from_secs(3), probe).await {
        Ok(Ok(true)) => ExitCode::SUCCESS,
        _ => ExitCode::FAILURE,
    }
}

async fn run() -> Result<(), Box<dyn std::error::Error>> {
    let config = Config::from_lookup(|name| std::env::var(name).ok())?;
    let read = |path: &std::path::Path| {
        std::fs::read(path).map_err(|error| format!("read {}: {error}", path.display()))
    };

    let mut qdrant = Qdrant::from_url(&config.qdrant_url).skip_compatibility_check();
    if let Some(path) = &config.qdrant_api_key_file {
        let key =
            String::from_utf8(read(path)?).map_err(|_| "the Qdrant API key file is not UTF-8")?;
        qdrant = qdrant.api_key(key.trim().to_owned());
    }
    let store = Arc::new(Store::new(
        qdrant.build()?,
        CollectionSettings {
            replication_factor: config.replication_factor,
            shard_number: config.shard_number,
        },
    ));

    let mut main_tls = ClientTlsConfig::new()
        .ca_certificate(Certificate::from_pem(read(&config.introspection_ca_file)?))
        .identity(Identity::from_pem(
            read(&config.client_cert_file)?,
            read(&config.client_key_file)?,
        ));
    if let Some(name) = &config.introspection_server_name {
        main_tls = main_tls.domain_name(name.clone());
    }
    let channel = Endpoint::from_shared(config.introspection_url.clone())?
        .tls_config(main_tls)?
        .connect_timeout(config.introspection_timeout)
        .connect_lazy();
    let introspector = CachingIntrospector::new(
        Arc::new(GrpcIntrospector::new(channel, config.introspection_timeout)),
        CachePolicy {
            max_ttl_seconds: config.introspection_cache_max_seconds,
            ..CachePolicy::default()
        },
        Arc::new(unix_now),
    );
    let auth = Authenticator::new(Arc::new(introspector), config.admin_identities.clone());
    let service = VectorService::new(auth, Arc::clone(&store));

    let health = tokio::net::TcpListener::bind(config.health_addr).await?;
    tokio::spawn(elitea_vector::health::serve(health, Arc::clone(&store)));

    let server_tls = ServerTlsConfig::new()
        .identity(Identity::from_pem(
            read(&config.tls_cert_file)?,
            read(&config.tls_key_file)?,
        ))
        .client_ca_root(Certificate::from_pem(read(&config.tls_client_ca_file)?));
    tracing::info!(
        grpc = %config.grpc_addr,
        health = %config.health_addr,
        admins = config.admin_identities.len(),
        "elitea-vector serving"
    );
    Server::builder()
        .tls_config(server_tls)?
        .add_service(
            VectorServiceServer::new(service)
                .max_decoding_message_size(MAX_MESSAGE_BYTES)
                .max_encoding_message_size(MAX_MESSAGE_BYTES),
        )
        .serve_with_shutdown(config.grpc_addr, shutdown())
        .await?;
    Ok(())
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
    tracing::info!("elitea-vector draining");
}

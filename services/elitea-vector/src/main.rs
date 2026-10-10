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
use elitea_vector::health::ServiceReadiness;
use elitea_vector::pb::vector_service_server::VectorServiceServer;
use elitea_vector::service::SpaceLimits;
use elitea_vector::service::VectorService;
use elitea_vector::store::{CollectionSettings, Store};
use elitea_vector::tls::{self, IntrospectionChannel, ReloadingCertificate};
use qdrant_client::Qdrant;
use tokio::io::{AsyncReadExt as _, AsyncWriteExt as _};
use tonic::transport::Server;

/// A full Upsert batch at the largest dimension fits with room to spare.
const MAX_MESSAGE_BYTES: usize = 64 << 20;

/// How often the introspection self-check runs: well inside the window
/// `/readyz` remembers a refusal for.
const INTROSPECTION_SELF_CHECK_INTERVAL: std::time::Duration = std::time::Duration::from_secs(30);

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
            max_collections: config.max_collections,
        },
    ));

    // The certificates and the client CA follow their files (see `elitea_vector::tls`).
    let introspection = IntrospectionChannel {
        url: config.introspection_url.clone(),
        ca_file: config.introspection_ca_file.clone(),
        cert_file: config.client_cert_file.clone(),
        key_file: config.client_key_file.clone(),
        server_name: config.introspection_server_name.clone(),
        timeout: config.introspection_timeout,
    };
    let (channel, built_from) = introspection.build()?;
    let grpc_introspector = Arc::new(GrpcIntrospector::new(channel, config.introspection_timeout));
    // Main answers "inactive" to a dummy token when it authorizes this
    // service and PERMISSION_DENIED when it does not; `/readyz` follows.
    let _self_check = grpc_introspector.spawn_self_check(INTROSPECTION_SELF_CHECK_INTERVAL);
    let introspection_health = grpc_introspector.health();
    let _channel_reloader = tls::spawn_channel_reloader(
        introspection,
        Arc::clone(&grpc_introspector),
        built_from,
        tls::RELOAD_INTERVAL,
    );
    let introspector = CachingIntrospector::new(
        grpc_introspector,
        CachePolicy {
            max_ttl_seconds: config.introspection_cache_max_seconds,
            ..CachePolicy::default()
        },
        Arc::new(unix_now),
    );
    let auth = Authenticator::new(Arc::new(introspector), config.admin_identities.clone());
    let service = VectorService::new(auth, Arc::clone(&store)).with_limits(SpaceLimits {
        max_dimension: config.max_dimension,
        allowed: config.allowed_spaces.clone(),
    });

    let certificate = ReloadingCertificate::load(&config.tls_cert_file, &config.tls_key_file)?;
    let _certificate_reloader = certificate.spawn_reloader(tls::RELOAD_INTERVAL);
    let client_ca = tls::ReloadingClientCa::load(&config.tls_client_ca_file)?;
    let _client_ca_reloader = client_ca.spawn_reloader(tls::RELOAD_INTERVAL);
    let tls_config = tls::server_config_with_ca(Arc::clone(&certificate), client_ca);

    let health = tokio::net::TcpListener::bind(config.health_addr).await?;
    tokio::spawn(elitea_vector::health::serve(
        health,
        Arc::new(
            ServiceReadiness::new(Arc::clone(&store), Some(Arc::clone(&certificate)))
                .with_introspection(introspection_health),
        ),
    ));

    let listener = tokio::net::TcpListener::bind(config.grpc_addr).await?;
    tracing::info!(
        grpc = %config.grpc_addr,
        health = %config.health_addr,
        admins = config.admin_identities.len(),
        "elitea-vector serving"
    );
    Server::builder()
        .add_service(
            VectorServiceServer::new(service)
                .max_decoding_message_size(MAX_MESSAGE_BYTES)
                .max_encoding_message_size(MAX_MESSAGE_BYTES),
        )
        .serve_with_incoming_shutdown(tls::tls_incoming(listener, tls_config), shutdown())
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

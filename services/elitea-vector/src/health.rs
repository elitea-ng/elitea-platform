//! The probe listener: plain HTTP on its own port, so a kubelet or a compose
//! health check needs no client certificate.
//!
//! * `GET /healthz` answers 200 while the process serves.
//! * `GET /readyz` answers 200 when [`Readiness::ready`] says so (Qdrant
//!   answers its health check, the serving certificate is not about to
//!   expire, and elitea-main has not refused this service's token
//!   introspection within the last minute), else 503.
//!
//! It carries no data and reads at most one small request per connection.
//! The listener is a small attack surface on an unauthenticated port, so it
//! is bounded: at most [`ProbeLimits::max_connections`] connections at once
//! (an excess one is closed unanswered), a deadline on reading the request,
//! and a back-off when `accept` fails.

use std::sync::Arc;
use std::time::Duration;

use async_trait::async_trait;
use tokio::io::{AsyncReadExt as _, AsyncWriteExt as _};
use tokio::net::{TcpListener, TcpStream};
use tokio::sync::Semaphore;

use crate::auth::{IntrospectionHealth, unix_now};
use crate::store::Store;
use crate::tls::{MIN_VALIDITY_SECONDS, ReloadingCertificate};

const MAX_REQUEST_BYTES: usize = 1024;
/// The first pause after a failed `accept`; it doubles up to [`MAX_BACKOFF`].
const INITIAL_BACKOFF: Duration = Duration::from_millis(50);
const MAX_BACKOFF: Duration = Duration::from_secs(1);

/// Whether the service should receive traffic.
#[async_trait]
pub trait Readiness: Send + Sync {
    /// `true` when ready.
    async fn ready(&self) -> bool;
}

/// Ready while Qdrant answers and the serving certificate has more than an
/// hour left.
pub struct ServiceReadiness {
    store: Arc<Store>,
    certificate: Option<Arc<ReloadingCertificate>>,
    introspection: Option<Arc<IntrospectionHealth>>,
}

impl ServiceReadiness {
    /// Readiness over `store` and, when given, the serving `certificate`.
    #[must_use]
    pub fn new(store: Arc<Store>, certificate: Option<Arc<ReloadingCertificate>>) -> Self {
        Self {
            store,
            certificate,
            introspection: None,
        }
    }

    /// Also not ready while elitea-main refuses this service's token
    /// introspection ([`IntrospectionHealth::not_authorized`]).
    #[must_use]
    pub fn with_introspection(mut self, introspection: Arc<IntrospectionHealth>) -> Self {
        self.introspection = Some(introspection);
        self
    }

    /// Whether elitea-main has not refused this service's introspection
    /// within the last minute.
    fn introspection_ok(&self, now: i64) -> bool {
        let refused = self
            .introspection
            .as_ref()
            .is_some_and(|health| health.not_authorized(now));
        if refused {
            tracing::warn!(
                "elitea-main refuses this service's token introspection; not ready \
                 (check ELITEA_VECTOR_INTROSPECTION_CLIENTS and the client certificate)"
            );
        }
        !refused
    }
}

impl ServiceReadiness {
    /// Whether the serving certificate has more than [`MIN_VALIDITY_SECONDS`]
    /// left at `now` (Unix seconds).
    fn certificate_ok(&self, now: i64) -> bool {
        let Some(certificate) = &self.certificate else {
            return true;
        };
        let left = certificate.seconds_until_expiry(now);
        if left <= MIN_VALIDITY_SECONDS {
            tracing::warn!(
                seconds_left = left,
                "serving certificate expires within the hour or has expired; not ready"
            );
            return false;
        }
        true
    }
}

#[async_trait]
impl Readiness for ServiceReadiness {
    async fn ready(&self) -> bool {
        let now = unix_now();
        self.certificate_ok(now) && self.introspection_ok(now) && self.store.ready().await
    }
}

/// Bounds on the probe listener.
#[derive(Clone, Copy, Debug)]
pub struct ProbeLimits {
    /// Connections served at once; a further one is closed unanswered.
    pub max_connections: usize,
    /// How long a client may take to send its request line.
    pub read_timeout: Duration,
    /// How long one connection may live in all, readiness check included.
    pub connection_timeout: Duration,
}

impl Default for ProbeLimits {
    fn default() -> Self {
        Self {
            max_connections: 64,
            read_timeout: Duration::from_secs(5),
            connection_timeout: Duration::from_secs(10),
        }
    }
}

/// Serves probes on `listener` until the task is dropped.
pub async fn serve(listener: TcpListener, readiness: Arc<dyn Readiness>) {
    serve_with(listener, readiness, ProbeLimits::default()).await;
}

/// [`serve`] with explicit limits.
pub async fn serve_with(listener: TcpListener, readiness: Arc<dyn Readiness>, limits: ProbeLimits) {
    let slots = Arc::new(Semaphore::new(limits.max_connections));
    let mut backoff = INITIAL_BACKOFF;
    loop {
        let stream = match listener.accept().await {
            Ok((stream, _)) => {
                backoff = INITIAL_BACKOFF;
                stream
            }
            Err(error) => {
                tracing::warn!(%error, "probe accept failed");
                tokio::time::sleep(backoff).await;
                backoff = (backoff * 2).min(MAX_BACKOFF);
                continue;
            }
        };
        let Ok(slot) = Arc::clone(&slots).try_acquire_owned() else {
            // Dropped, not queued: a probe flood must not build a backlog.
            drop(stream);
            continue;
        };
        let readiness = Arc::clone(&readiness);
        tokio::spawn(async move {
            let _slot = slot;
            let _ = tokio::time::timeout(
                limits.connection_timeout,
                answer(stream, readiness.as_ref(), limits.read_timeout),
            )
            .await;
        });
    }
}

async fn read_request_line(
    stream: &mut TcpStream,
    buffer: &mut [u8],
    timeout: Duration,
) -> std::io::Result<usize> {
    let reading = async {
        let mut read = 0;
        while read < buffer.len() {
            let count = stream.read(&mut buffer[read..]).await?;
            if count == 0 {
                break;
            }
            read += count;
            if buffer[..read]
                .windows(4)
                .any(|window| window == b"\r\n\r\n")
            {
                break;
            }
        }
        Ok(read)
    };
    tokio::time::timeout(timeout, reading)
        .await
        .map_err(|_| std::io::Error::from(std::io::ErrorKind::TimedOut))?
}

async fn answer(
    mut stream: TcpStream,
    readiness: &dyn Readiness,
    read_timeout: Duration,
) -> std::io::Result<()> {
    let mut buffer = vec![0_u8; MAX_REQUEST_BYTES];
    let read = read_request_line(&mut stream, &mut buffer, read_timeout).await?;
    let line = buffer[..read]
        .split(|byte| *byte == b'\r' || *byte == b'\n')
        .next()
        .unwrap_or_default();
    let (status, body) = match line {
        b"GET /healthz HTTP/1.1" | b"GET /healthz HTTP/1.0" => ("200 OK", "ok\n"),
        b"GET /readyz HTTP/1.1" | b"GET /readyz HTTP/1.0" => {
            if readiness.ready().await {
                ("200 OK", "ready\n")
            } else {
                ("503 Service Unavailable", "not ready\n")
            }
        }
        _ => ("404 Not Found", "not found\n"),
    };
    let response = format!(
        "HTTP/1.1 {status}\r\ncontent-type: text/plain\r\ncontent-length: {}\r\nconnection: close\r\n\r\n{body}",
        body.len()
    );
    stream.write_all(response.as_bytes()).await?;
    stream.shutdown().await
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::atomic::{AtomicBool, Ordering};

    #[test]
    fn readiness_fails_within_an_hour_of_the_certificate_expiring() {
        use crate::store::CollectionSettings;
        let key = rcgen::KeyPair::generate().expect("key");
        let params = rcgen::CertificateParams::new(vec!["localhost".to_owned()]).expect("params");
        let dir = std::env::temp_dir().join(format!("elitea-vector-ready-{}", std::process::id()));
        std::fs::create_dir_all(&dir).expect("dir");
        std::fs::write(
            dir.join("c.crt"),
            params.self_signed(&key).expect("cert").pem(),
        )
        .expect("write");
        std::fs::write(dir.join("c.key"), key.serialize_pem()).expect("write");
        crate::install_crypto_provider();
        let certificate =
            ReloadingCertificate::load(&dir.join("c.crt"), &dir.join("c.key")).expect("load");
        let store = Arc::new(Store::new(
            qdrant_client::Qdrant::from_url("http://127.0.0.1:1")
                .build()
                .expect("client"),
            CollectionSettings::default(),
        ));
        let readiness = ServiceReadiness::new(store, Some(Arc::clone(&certificate)));
        let not_after = certificate.not_after_unix();
        assert!(readiness.certificate_ok(not_after - MIN_VALIDITY_SECONDS - 1));
        assert!(!readiness.certificate_ok(not_after - MIN_VALIDITY_SECONDS));
        assert!(!readiness.certificate_ok(not_after - 60));
        assert!(!readiness.certificate_ok(not_after + 1), "expired");
    }

    #[test]
    fn readiness_fails_while_main_refuses_introspection() {
        use crate::store::CollectionSettings;
        let store = Arc::new(Store::new(
            qdrant_client::Qdrant::from_url("http://127.0.0.1:1")
                .build()
                .expect("client"),
            CollectionSettings::default(),
        ));
        let health = Arc::new(IntrospectionHealth::default());
        let readiness = ServiceReadiness::new(store, None).with_introspection(Arc::clone(&health));
        assert!(readiness.introspection_ok(1_000));
        health.record(1_000, true);
        assert!(!readiness.introspection_ok(1_030));
        assert!(!readiness.introspection_ok(1_059));
        assert!(readiness.introspection_ok(1_060), "the refusal aged out");
        health.record(1_100, true);
        health.record(1_110, false);
        assert!(
            readiness.introspection_ok(1_111),
            "an answered attempt clears it"
        );
    }

    struct Switch(AtomicBool);

    #[async_trait]
    impl Readiness for Switch {
        async fn ready(&self) -> bool {
            self.0.load(Ordering::SeqCst)
        }
    }

    async fn start(limits: ProbeLimits, ready: bool) -> (std::net::SocketAddr, Arc<Switch>) {
        let listener = TcpListener::bind("127.0.0.1:0").await.expect("bind");
        let address = listener.local_addr().expect("address");
        let switch = Arc::new(Switch(AtomicBool::new(ready)));
        tokio::spawn(serve_with(
            listener,
            Arc::clone(&switch) as Arc<dyn Readiness>,
            limits,
        ));
        (address, switch)
    }

    async fn get(address: std::net::SocketAddr, path: &str) -> String {
        let mut stream = TcpStream::connect(address).await.expect("connect");
        stream
            .write_all(format!("GET {path} HTTP/1.1\r\nhost: x\r\n\r\n").as_bytes())
            .await
            .expect("write");
        let mut response = String::new();
        stream.read_to_string(&mut response).await.expect("read");
        response
    }

    #[tokio::test]
    async fn probes_answer() {
        let (address, switch) = start(ProbeLimits::default(), true).await;
        assert!(get(address, "/healthz").await.starts_with("HTTP/1.1 200"));
        assert!(get(address, "/readyz").await.starts_with("HTTP/1.1 200"));
        assert!(get(address, "/other").await.starts_with("HTTP/1.1 404"));
        switch.0.store(false, Ordering::SeqCst);
        assert!(get(address, "/readyz").await.starts_with("HTTP/1.1 503"));
        assert!(get(address, "/healthz").await.starts_with("HTTP/1.1 200"));
    }

    #[tokio::test]
    async fn connections_beyond_the_cap_are_dropped_and_slots_come_back() {
        let limits = ProbeLimits {
            max_connections: 2,
            read_timeout: Duration::from_secs(30),
            connection_timeout: Duration::from_mins(1),
        };
        let (address, _) = start(limits, true).await;
        // Two silent clients hold both slots.
        let first = TcpStream::connect(address).await.expect("connect");
        let second = TcpStream::connect(address).await.expect("connect");
        tokio::time::sleep(Duration::from_millis(100)).await;
        // The third is accepted by the kernel and then closed unanswered.
        let mut third = TcpStream::connect(address).await.expect("connect");
        let _ = third.write_all(b"GET /healthz HTTP/1.1\r\n\r\n").await;
        let mut response = Vec::new();
        let _ = tokio::time::timeout(Duration::from_secs(2), third.read_to_end(&mut response))
            .await
            .expect("an excess connection is closed promptly");
        assert!(response.is_empty(), "no answer for an excess connection");
        // Free a slot: probes work again.
        drop(first);
        let mut answered = false;
        for _ in 0..50 {
            tokio::time::sleep(Duration::from_millis(50)).await;
            let mut stream = TcpStream::connect(address).await.expect("connect");
            let _ = stream.write_all(b"GET /healthz HTTP/1.1\r\n\r\n").await;
            let mut text = String::new();
            let _ = stream.read_to_string(&mut text).await;
            if text.starts_with("HTTP/1.1 200") {
                answered = true;
                break;
            }
        }
        assert!(answered, "a freed slot serves the next probe");
        drop(second);
    }

    #[tokio::test]
    async fn a_silent_client_is_cut_off_at_the_read_timeout() {
        let limits = ProbeLimits {
            max_connections: 1,
            read_timeout: Duration::from_millis(200),
            connection_timeout: Duration::from_secs(5),
        };
        let (address, _) = start(limits, true).await;
        let mut silent = TcpStream::connect(address).await.expect("connect");
        let mut buffer = Vec::new();
        tokio::time::timeout(Duration::from_secs(3), silent.read_to_end(&mut buffer))
            .await
            .expect("closed after the read timeout")
            .ok();
        // The slot is free again.
        assert!(get(address, "/healthz").await.starts_with("HTTP/1.1 200"));
    }
}

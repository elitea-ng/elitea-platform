//! The drain after the shutdown signal (`server::serve_with_options`), over
//! a REAL Unix socket: the sidecar continues to serve while an invocation
//! runs, and starts the graceful stop only when the invocation ends or the
//! drain deadline comes.
#![cfg(feature = "server")]

use elitea_engine_core::errors::EngineError;
use elitea_engine_core::stream::Context;
use elitea_engine_sidecar::client::EngineClient;
use elitea_engine_sidecar::{Engine, server};
use serde_json::{Map, Value, json};
use std::io;
use std::path::PathBuf;
use std::sync::Arc;
use std::sync::atomic::{AtomicUsize, Ordering};
use std::time::Duration;
use tokio::sync::{Notify, oneshot};
use tokio::task::JoinHandle;

static NEXT: AtomicUsize = AtomicUsize::new(0);

/// `hold` runs until the test releases it (or until a stop).
#[derive(Clone, Default)]
struct Holder {
    release: Arc<Notify>,
}

impl Engine for Holder {
    fn runner_name(&self) -> &'static str {
        "holder"
    }

    fn serves(&self, tool: &str) -> bool {
        tool == "hold"
    }

    async fn run(
        &self,
        _tool: &str,
        _arguments: Map<String, Value>,
        context: &Context,
    ) -> Result<Value, EngineError> {
        tokio::select! {
            () = self.release.notified() => {}
            () = context.pause(Duration::from_secs(30)) => {}
        }
        context.checkpoint()?;
        Ok(json!({"success": true}))
    }
}

/// A sidecar with a drain deadline, on its own short socket path (macOS
/// caps a socket path at 104 bytes).
struct Sidecar {
    socket: PathBuf,
    shutdown: Option<oneshot::Sender<()>>,
    served: JoinHandle<io::Result<()>>,
}

impl Sidecar {
    fn start(engine: Holder, drain_timeout: Duration) -> Self {
        let socket = PathBuf::from(format!(
            "/tmp/escd-{}-{}/e.sock",
            std::process::id(),
            NEXT.fetch_add(1, Ordering::SeqCst)
        ));
        let listener = server::bind(&socket).expect("bind");
        let (shutdown, signal) = oneshot::channel::<()>();
        let options = server::ServeOptions {
            drain_timeout,
            ..server::ServeOptions::default()
        };
        let served = tokio::spawn(server::serve_with_options(
            listener,
            engine,
            options,
            async {
                let _ = signal.await;
            },
        ));
        Self {
            socket,
            shutdown: Some(shutdown),
            served,
        }
    }

    async fn client(&self) -> EngineClient {
        for _ in 0..200 {
            if let Ok(client) = EngineClient::connect(&self.socket).await {
                return client;
            }
            tokio::time::sleep(Duration::from_millis(10)).await;
        }
        panic!("cannot connect to {}", self.socket.display());
    }

    /// Start one `hold` invocation and wait until Health counts it.
    async fn hold(&self) -> JoinHandle<Result<Vec<Value>, tonic::Status>> {
        let mut runner = self.client().await;
        let invocation =
            tokio::spawn(async move { runner.invoke("held", "hold", &json!({})).await });
        let mut client = self.client().await;
        for _ in 0..200 {
            if client.health().await.expect("health")["active"] == 1 {
                return invocation;
            }
            tokio::time::sleep(Duration::from_millis(5)).await;
        }
        panic!("the invocation did not start");
    }

    fn signal(&mut self) {
        let _ = self.shutdown.take().expect("signal once").send(());
    }
}

impl Drop for Sidecar {
    fn drop(&mut self) {
        if let Some(dir) = self.socket.parent() {
            let _ = std::fs::remove_dir_all(dir);
        }
    }
}

#[tokio::test]
async fn the_sidecar_serves_new_connections_until_the_running_invocation_ends() {
    let engine = Holder::default();
    let mut sidecar = Sidecar::start(engine.clone(), Duration::from_secs(30));
    let invocation = sidecar.hold().await;

    sidecar.signal();
    tokio::time::sleep(Duration::from_millis(100)).await;
    assert!(
        !sidecar.served.is_finished(),
        "the server stopped while an invocation ran"
    );

    // A NEW connection after the signal: what the host's poll and the
    // container probe do during the drain.
    let mut late = tokio::time::timeout(Duration::from_secs(2), sidecar.client())
        .await
        .expect("a new connection during the drain");
    let health = late.health().await.expect("health during the drain");
    assert_eq!(health["status"], "UP");
    assert_eq!(health["active"], 1);
    assert!(!sidecar.served.is_finished());

    engine.release.notify_one();
    let events = invocation
        .await
        .expect("join")
        .expect("the invocation completes during the drain");
    assert_eq!(
        events.last().expect("a result")["result"]["success"],
        json!(true),
        "{events:?}"
    );
    drop(late);
    tokio::time::timeout(Duration::from_secs(5), &mut sidecar.served)
        .await
        .expect("the server stops when no invocation runs")
        .expect("join")
        .expect("serve");
}

#[tokio::test]
async fn the_sidecar_starts_its_graceful_stop_at_the_drain_deadline() {
    let engine = Holder::default();
    let mut sidecar = Sidecar::start(engine.clone(), Duration::from_millis(200));
    let invocation = sidecar.hold().await;

    sidecar.signal();
    tokio::time::sleep(Duration::from_millis(400)).await;

    // After the deadline the server takes no new connection, although the
    // invocation still runs: the graceful stop of tonic has started.
    let late = tokio::time::timeout(Duration::from_millis(300), async {
        let mut client = EngineClient::connect(&sidecar.socket).await?;
        client.health().await.map_err(|status| status.to_string())
    })
    .await;
    assert!(
        !matches!(late, Ok(Ok(_))),
        "a new call was served after the deadline: {late:?}"
    );
    assert!(!invocation.is_finished(), "the job ended on its own");

    // The graceful stop lets the call in progress complete; the host ends
    // it (here: the reader goes away), and then the server returns.
    invocation.abort();
    tokio::time::timeout(Duration::from_secs(5), &mut sidecar.served)
        .await
        .expect("the server stops after the deadline")
        .expect("join")
        .expect("serve");
}

#[tokio::test]
async fn a_zero_deadline_stops_at_once() {
    let engine = Holder::default();
    let mut sidecar = Sidecar::start(engine.clone(), Duration::ZERO);
    let invocation = sidecar.hold().await;
    sidecar.signal();
    tokio::time::sleep(Duration::from_millis(100)).await;
    let late = tokio::time::timeout(Duration::from_millis(300), async {
        let mut client = EngineClient::connect(&sidecar.socket).await?;
        client.health().await.map_err(|status| status.to_string())
    })
    .await;
    assert!(!matches!(late, Ok(Ok(_))), "served after a zero deadline");
    invocation.abort();
    tokio::time::timeout(Duration::from_secs(5), &mut sidecar.served)
        .await
        .expect("the server stops")
        .expect("join")
        .expect("serve");
}

#[test]
fn the_drain_setting_parses_whole_seconds() {
    assert_eq!(
        server::parse_drain_timeout(None),
        Ok(server::DEFAULT_DRAIN_TIMEOUT)
    );
    assert_eq!(
        server::parse_drain_timeout(Some(" ")),
        Ok(server::DEFAULT_DRAIN_TIMEOUT)
    );
    assert_eq!(
        server::parse_drain_timeout(Some("3600")),
        Ok(Duration::from_secs(3600))
    );
    assert_eq!(server::parse_drain_timeout(Some("0")), Ok(Duration::ZERO));
    assert!(server::parse_drain_timeout(Some("-1")).is_err());
    assert!(server::parse_drain_timeout(Some("1.5")).is_err());
}

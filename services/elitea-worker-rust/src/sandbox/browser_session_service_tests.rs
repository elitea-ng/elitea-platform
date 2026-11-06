//! `BrowserSessionService` end to end in one process: real Main-style signed
//! grants, the profile's ceilings, the lifetime table on an injected clock,
//! and a fake backend whose containers run the fake session agent.

use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::{Arc, Mutex};
use std::time::Duration;

use prost::Message as _;
use ring::signature::{self, KeyPair as _};
use tonic::{Code, Request};

use super::browser_session::{Isolation, spec_digest, view_digest};
use super::browser_session_lifetime::{Clock, IDLE, LINGER};
use super::browser_session_service::fake::FakeBackend;
use super::browser_session_service::{
    BrowserCeilings, BrowserSessionSupervisor, ERROR_METADATA, EngineCeiling,
    SharedBrowserSupervisor,
};
use crate::protocol::command::Ed25519PublicKeyResolver;
use crate::protocol::elitea::runtime::v1::{
    BrowserCallRequestV1, BrowserEngineV1, BrowserSessionEgressV1, BrowserSessionEndV1,
    BrowserSessionSpecV1, BrowserViewInputV1, BrowserViewOpenV1, DestroyBrowserSessionRequestV1,
    SandboxJobGrantClaimsV1, SignedSandboxJobGrantV1, StartBrowserSessionRequestV1,
    browser_session_service_client::BrowserSessionServiceClient,
    browser_session_service_server::BrowserSessionService, browser_view_input_v1,
    browser_view_output_v1,
};
use crate::protocol::sandbox_grant::GrantVerifier;

const AUDIENCE: &str = "dns:elitea-sandbox-browser";
const PEER: &str = "dns:worker-1";
const GRANT_DOMAIN: &[u8] = b"elitea.sandbox.job-grant.ed25519.v1\0";
const MIN: Duration = Duration::from_mins(1);

struct Keys([u8; 32]);

impl Ed25519PublicKeyResolver for Keys {
    fn resolve_ed25519_public_key(&self, id: &str) -> Option<[u8; 32]> {
        (id == "key-1").then_some(self.0)
    }
}

fn key() -> signature::Ed25519KeyPair {
    signature::Ed25519KeyPair::from_seed_unchecked(&[9; 32]).expect("key")
}

/// The injected clock.
#[derive(Default)]
struct ManualClock(Mutex<Duration>);

impl ManualClock {
    fn advance(&self, by: Duration) {
        *self.0.lock().unwrap() += by;
    }
}

impl Clock for Arc<ManualClock> {
    fn now(&self) -> Duration {
        *self.0.lock().unwrap()
    }
}

fn now_millis() -> i64 {
    i64::try_from(
        std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .expect("clock")
            .as_millis(),
    )
    .expect("millis")
}

/// Main's grant for `execution` (the run), activation `name`, `digest`.
fn grant(
    execution: &str,
    name: &str,
    digest: [u8; 32],
    generation: u64,
) -> SignedSandboxJobGrantV1 {
    let now = now_millis();
    let claims = SandboxJobGrantClaimsV1 {
        revision: 1,
        tenant_id: "tenant".into(),
        project_id: 2,
        execution_id: execution.to_owned(),
        activation_id: name.to_owned(),
        request_digest: digest.to_vec(),
        submitter_workload_identity: PEER.into(),
        audience: AUDIENCE.into(),
        issued_at_unix_millis: now - 1_000,
        expires_at_unix_millis: now + 20_000,
        generation,
        ..Default::default()
    };
    let bytes = claims.encode_to_vec();
    let mut input = GRANT_DOMAIN.to_vec();
    input.extend_from_slice(&(bytes.len() as u64).to_be_bytes());
    input.extend_from_slice(&bytes);
    SignedSandboxJobGrantV1 {
        key_id: "key-1".into(),
        claims_bytes: bytes,
        signature: key().sign(&input).as_ref().to_vec(),
    }
}

static NAMES: AtomicU64 = AtomicU64::new(1);

fn spec(engine: BrowserEngineV1, memory_bytes: u64, cpu_millis: u64) -> BrowserSessionSpecV1 {
    BrowserSessionSpecV1 {
        revision: 1,
        name: format!("elitea-br-{:024x}", NAMES.fetch_add(1, Ordering::Relaxed)),
        engine: engine.into(),
        cpu_millis,
        memory_bytes,
        wall_seconds: 3_600,
        max_pages: 3,
    }
}

fn full() -> BrowserSessionSpecV1 {
    spec(BrowserEngineV1::Full, 2 << 30, 2_000)
}

fn ceilings(concurrency: usize, engine_sandbox: bool) -> BrowserCeilings {
    BrowserCeilings {
        image: format!("registry.example/browser@sha256:{}", "e".repeat(64)),
        full: EngineCeiling {
            max_cpu_millis: 2_000,
            max_memory_bytes: 2 << 30,
        },
        lite: Some(EngineCeiling {
            max_cpu_millis: 1_000,
            max_memory_bytes: 512 << 20,
        }),
        max_wall_seconds: 3_600,
        max_pages: 3,
        sessions_per_run: 3,
        concurrency,
        engine_sandbox,
    }
}

struct Harness {
    backend: Arc<FakeBackend>,
    clock: Arc<ManualClock>,
    supervisor: Arc<BrowserSessionSupervisor<Keys>>,
    service: SharedBrowserSupervisor<Keys>,
}

fn harness_with(concurrency: usize, engine_sandbox: bool, capacity_wait: Duration) -> Harness {
    let mut public = [0; 32];
    public.copy_from_slice(key().public_key().as_ref());
    let verifier = GrantVerifier::new(Keys(public), AUDIENCE.into()).expect("verifier");
    let backend = Arc::new(FakeBackend::default());
    let clock = Arc::new(ManualClock::default());
    let supervisor = Arc::new(
        BrowserSessionSupervisor::new(
            verifier,
            Arc::clone(&backend) as Arc<dyn super::browser_session_service::BrowserSessionBackend>,
            ceilings(concurrency, engine_sandbox),
            Arc::new(Arc::clone(&clock)),
        )
        .with_fixed_peer(PEER)
        .with_limits(|limits| limits.capacity_wait = capacity_wait),
    );
    Harness {
        backend,
        clock,
        service: SharedBrowserSupervisor(Arc::clone(&supervisor)),
        supervisor,
    }
}

fn harness(concurrency: usize) -> Harness {
    harness_with(concurrency, false, Duration::from_millis(200))
}

fn start_request(run: &str, spec: &BrowserSessionSpecV1) -> Request<StartBrowserSessionRequestV1> {
    Request::new(StartBrowserSessionRequestV1 {
        grant: Some(grant(run, &spec.name, spec_digest(spec), 1)),
        spec: Some(spec.clone()),
        egress: Some(BrowserSessionEgressV1 {
            proxy_url: "http://10.0.0.7:3128".into(),
            proxy_token: "run-token".into(),
        }),
        unattended: false,
    })
}

fn call_request(
    run: &str,
    spec: &BrowserSessionSpecV1,
    generation: u64,
) -> Request<BrowserCallRequestV1> {
    Request::new(BrowserCallRequestV1 {
        grant: Some(grant(run, &spec.name, spec_digest(spec), generation)),
        spec: Some(spec.clone()),
        call_id: "toolu_1".into(),
        name: "browser_open".into(),
        arguments_json: r#"{"url":"https://example.org/"}"#.into(),
    })
}

fn destroy_request(
    run: &str,
    spec: &BrowserSessionSpecV1,
    end: BrowserSessionEndV1,
) -> Request<DestroyBrowserSessionRequestV1> {
    Request::new(DestroyBrowserSessionRequestV1 {
        grant: Some(grant(run, &spec.name, spec_digest(spec), 1)),
        spec: Some(spec.clone()),
        end: end.into(),
    })
}

fn code(status: &tonic::Status) -> &str {
    status
        .metadata()
        .get(ERROR_METADATA)
        .and_then(|value| value.to_str().ok())
        .unwrap_or_default()
}

#[tokio::test]
async fn a_session_starts_answers_calls_and_is_destroyed_never_to_be_reused() {
    let h = harness(4);
    let s = full();
    let started = h
        .service
        .start_browser_session(start_request("run-1", &s))
        .await
        .expect("started")
        .into_inner();
    assert!(started.created);
    assert_eq!(started.engine_version, "chrome-headless-shell/test");
    // The start line carries the engine, the --no-sandbox decision (gVisor)
    // and the proxy; the container spec never carries the token.
    let seen = Arc::clone(&h.backend.seen.lock().unwrap()[&s.name]);
    let start_line = seen.lines.lock().unwrap()[0].clone();
    assert_eq!(start_line["engine"], "full");
    assert_eq!(start_line["engine_sandbox"], false);
    assert_eq!(start_line["proxy"]["token"], "run-token");
    assert_eq!(start_line["attended"], true);
    let launch = h.backend.containers.lock().unwrap()[&s.name].clone();
    assert!(!format!("{launch:?}").contains("run-token"));
    // A retried start observes it.
    let again = h
        .service
        .start_browser_session(start_request("run-1", &s))
        .await
        .expect("observed")
        .into_inner();
    assert!(!again.created);
    let result = h
        .service
        .browser_call(call_request("run-1", &s, 1))
        .await
        .expect("call")
        .into_inner();
    let result: serde_json::Value = serde_json::from_str(&result.result_json).expect("json");
    assert_eq!(result["name"], "browser_open");
    let destroyed = h
        .service
        .destroy_browser_session(destroy_request("run-1", &s, BrowserSessionEndV1::Now))
        .await
        .expect("destroyed")
        .into_inner();
    assert!(destroyed.destroyed);
    assert!(h.backend.containers.lock().unwrap().is_empty());
    // Never reused: neither a start nor a call revives the name.
    let refused = h
        .service
        .start_browser_session(start_request("run-1", &s))
        .await
        .expect_err("ended");
    assert_eq!(code(&refused), "browser_session_ended");
    let refused = h
        .service
        .browser_call(call_request("run-1", &s, 1))
        .await
        .expect_err("ended");
    assert_eq!(code(&refused), "browser_session_ended");
    // A second destroy is idempotent.
    assert!(
        !h.service
            .destroy_browser_session(destroy_request("run-1", &s, BrowserSessionEndV1::Now))
            .await
            .expect("idempotent")
            .into_inner()
            .destroyed
    );
}

#[tokio::test]
async fn an_unattended_start_tells_the_agent_nobody_can_be_asked() {
    let h = harness(4);
    let s = full();
    let mut request = start_request("run-1", &s);
    request.get_mut().unattended = true;
    // The flag is on the request, not the spec: the grant's digest is the
    // same one an attended start signs.
    h.service
        .start_browser_session(request)
        .await
        .expect("started");
    let seen = Arc::clone(&h.backend.seen.lock().unwrap()[&s.name]);
    let start_line = seen.lines.lock().unwrap()[0].clone();
    assert_eq!(start_line["op"], "start");
    assert_eq!(start_line["attended"], false);
}

#[tokio::test]
async fn admission_refuses_another_audience_peer_digest_or_name() {
    let h = harness(4);
    let s = full();
    // A grant for another spec.
    let mut other = s.clone();
    other.max_pages = 2;
    let mut request = start_request("run-1", &s);
    request.get_mut().grant = Some(grant("run-1", &s.name, spec_digest(&other), 1));
    let status = h
        .service
        .start_browser_session(request)
        .await
        .expect_err("digest");
    assert_eq!(status.code(), Code::PermissionDenied);
    // A grant whose activation is another session's.
    let mut request = start_request("run-1", &s);
    request.get_mut().grant = Some(grant(
        "run-1",
        "elitea-br-ffffffffffffffffffffffff",
        spec_digest(&s),
        1,
    ));
    assert_eq!(
        h.service
            .start_browser_session(request)
            .await
            .expect_err("name")
            .code(),
        Code::PermissionDenied
    );
    // A workspace-run name is not a session.
    let mut ws = s.clone();
    ws.name = "elitea-ws-0123456789abcdef01234567".into();
    assert_eq!(
        h.service
            .start_browser_session(start_request("run-1", &ws))
            .await
            .expect_err("name")
            .code(),
        Code::InvalidArgument
    );
    // A proxy that is not an IP literal (the session has no resolver).
    let mut request = start_request("run-1", &s);
    request.get_mut().egress.as_mut().unwrap().proxy_url = "http://proxy.local:3128".into();
    assert_eq!(
        h.service
            .start_browser_session(request)
            .await
            .expect_err("proxy")
            .code(),
        Code::InvalidArgument
    );
    // A call that is not a browser tool.
    let mut call = call_request("run-1", &s, 1);
    call.get_mut().name = "run_command".into();
    assert_eq!(
        h.service.browser_call(call).await.expect_err("tool").code(),
        Code::InvalidArgument
    );
    // A call on a session that was never started.
    let status = h
        .service
        .browser_call(call_request("run-1", &s, 1))
        .await
        .expect_err("not started");
    assert_eq!(code(&status), "browser_session_not_started");
    // Another claim's session.
    h.service
        .start_browser_session(start_request("run-1", &s))
        .await
        .expect("started");
    let status = h
        .service
        .browser_call(call_request("run-2", &s, 1))
        .await
        .expect_err("another claim");
    assert_eq!(status.code(), Code::PermissionDenied);
    assert_eq!(h.backend.created.lock().unwrap().len(), 1);
}

#[tokio::test]
async fn ceilings_hold_per_engine_wall_and_pages() {
    let h = harness(8);
    // Full: 2 GiB and 2 CPUs; lite: 512 MiB and 1 CPU.
    for (spec, admitted) in [
        (spec(BrowserEngineV1::Full, 2 << 30, 2_000), true),
        (spec(BrowserEngineV1::Full, (2 << 30) + 1, 2_000), false),
        (spec(BrowserEngineV1::Full, 2 << 30, 2_001), false),
        (spec(BrowserEngineV1::Lite, 512 << 20, 1_000), true),
        (spec(BrowserEngineV1::Lite, 1 << 30, 1_000), false),
        (spec(BrowserEngineV1::Lite, 512 << 20, 2_000), false),
    ] {
        let outcome = h
            .service
            .start_browser_session(start_request(&format!("run-{}", spec.name), &spec))
            .await;
        assert_eq!(outcome.is_ok(), admitted, "{spec:?}");
        if let Err(status) = outcome {
            assert_eq!(code(&status), "browser_ceiling_exceeded");
        }
    }
    let mut long = full();
    long.wall_seconds = 3_601;
    let mut pages = full();
    pages.max_pages = 4;
    for spec in [long, pages] {
        let status = h
            .service
            .start_browser_session(start_request("run-x", &spec))
            .await
            .expect_err("ceiling");
        assert_eq!(code(&status), "browser_ceiling_exceeded");
    }
}

#[tokio::test]
async fn a_run_holds_at_most_three_sessions() {
    let h = harness(16);
    for _ in 0..3 {
        h.service
            .start_browser_session(start_request("run-1", &full()))
            .await
            .expect("started");
    }
    let status = h
        .service
        .start_browser_session(start_request("run-1", &full()))
        .await
        .expect_err("fourth");
    assert_eq!(status.code(), Code::ResourceExhausted);
    assert_eq!(code(&status), "browser_session_limit");
    // Another run is unaffected.
    h.service
        .start_browser_session(start_request("run-2", &full()))
        .await
        .expect("another run");
}

#[tokio::test]
async fn no_sandbox_is_refused_unless_the_runtime_is_gvisor() {
    let h = harness(4);
    *h.backend.isolation.lock().unwrap() = Isolation::Other;
    let status = h
        .service
        .start_browser_session(start_request("run-1", &full()))
        .await
        .expect_err("refused");
    assert_eq!(code(&status), "browser_isolation_refused");
    assert!(
        h.backend.created.lock().unwrap().is_empty(),
        "before any container"
    );
    assert!(h.supervisor.sweep().await.is_ok());
    // The engine keeping its own sandbox runs anywhere.
    let h = harness_with(4, true, Duration::from_millis(200));
    *h.backend.isolation.lock().unwrap() = Isolation::Other;
    let s = full();
    h.service
        .start_browser_session(start_request("run-1", &s))
        .await
        .expect("sandboxed engine");
    let seen = Arc::clone(&h.backend.seen.lock().unwrap()[&s.name]);
    assert_eq!(seen.lines.lock().unwrap()[0]["engine_sandbox"], true);
}

#[tokio::test]
async fn a_full_profile_waits_for_a_slot_then_fails_browser_capacity() {
    let h = harness_with(1, false, Duration::from_millis(300));
    let first = full();
    h.service
        .start_browser_session(start_request("run-1", &first))
        .await
        .expect("first");
    let began = tokio::time::Instant::now();
    let status = h
        .service
        .start_browser_session(start_request("run-2", &full()))
        .await
        .expect_err("full");
    assert!(began.elapsed() >= Duration::from_millis(300), "it waited");
    assert_eq!(status.code(), Code::ResourceExhausted);
    assert_eq!(code(&status), "browser_capacity");
    // A slot that frees during the wait admits the waiter.
    let h = Arc::new(harness_with(1, false, Duration::from_secs(10)));
    let first = full();
    h.service
        .start_browser_session(start_request("run-1", &first))
        .await
        .expect("first");
    let waiter = {
        let h = Arc::clone(&h);
        tokio::spawn(async move {
            h.service
                .start_browser_session(start_request("run-2", &full()))
                .await
        })
    };
    tokio::time::sleep(Duration::from_millis(100)).await;
    assert!(!waiter.is_finished(), "waiting for a slot");
    h.service
        .destroy_browser_session(destroy_request("run-1", &first, BrowserSessionEndV1::Now))
        .await
        .expect("freed");
    let admitted = tokio::time::timeout(Duration::from_secs(5), waiter)
        .await
        .expect("woken")
        .expect("joined");
    assert!(admitted.expect("admitted").into_inner().created);
}

#[tokio::test]
async fn idle_wall_and_linger_destroy_the_session_on_the_injected_clock() {
    // Idle: 15 minutes without a call.
    let h = harness(4);
    let s = full();
    h.service
        .start_browser_session(start_request("run-1", &s))
        .await
        .expect("started");
    h.clock.advance(IDLE.saturating_sub(MIN));
    h.service
        .browser_call(call_request("run-1", &s, 1))
        .await
        .expect("a call keeps it");
    h.clock.advance(IDLE.saturating_sub(MIN));
    assert_eq!(h.supervisor.sweep().await.expect("sweep"), 0);
    h.clock.advance(MIN);
    assert_eq!(h.supervisor.sweep().await.expect("sweep"), 1);
    assert!(h.backend.containers.lock().unwrap().is_empty());
    assert_eq!(
        code(
            &h.service
                .browser_call(call_request("run-1", &s, 1))
                .await
                .expect_err("gone")
        ),
        "browser_session_ended"
    );

    // Run end: calls refused at once, a viewer may still attach, destroyed
    // after the 10-minute linger.
    let s = full();
    h.service
        .start_browser_session(start_request("run-2", &s))
        .await
        .expect("started");
    let lingering = h
        .service
        .destroy_browser_session(destroy_request("run-2", &s, BrowserSessionEndV1::RunEnded))
        .await
        .expect("lingers")
        .into_inner();
    assert!(!lingering.destroyed);
    assert!(lingering.linger_until_unix_seconds > 0);
    assert_eq!(
        code(
            &h.service
                .browser_call(call_request("run-2", &s, 1))
                .await
                .expect_err("lingering")
        ),
        "browser_session_ended"
    );
    let seen = Arc::clone(&h.backend.seen.lock().unwrap()[&s.name]);
    h.clock.advance(LINGER.saturating_sub(MIN));
    assert_eq!(h.supervisor.sweep().await.expect("sweep"), 0);
    h.clock.advance(MIN);
    assert_eq!(h.supervisor.sweep().await.expect("sweep"), 1);
    assert!(h.backend.containers.lock().unwrap().is_empty());
    let ops = || -> Vec<String> {
        seen.lines
            .lock()
            .unwrap()
            .iter()
            .map(|line| line["op"].as_str().unwrap_or_default().to_owned())
            .collect()
    };
    tokio::time::timeout(Duration::from_secs(5), async {
        while ops().last().map(String::as_str) != Some("destroy") {
            tokio::time::sleep(Duration::from_millis(10)).await;
        }
    })
    .await
    .expect("the agent was told to stop");
    let ops = ops();
    assert!(ops.contains(&"run_ended".to_owned()), "{ops:?}");
    assert_eq!(ops.last().map(String::as_str), Some("destroy"), "{ops:?}");

    // Wall: a busy session still ends at 60 minutes.
    let s = full();
    h.service
        .start_browser_session(start_request("run-3", &s))
        .await
        .expect("started");
    for _ in 0..11 {
        h.clock.advance(5 * MIN);
        h.service
            .browser_call(call_request("run-3", &s, 1))
            .await
            .expect("busy");
        assert_eq!(h.supervisor.sweep().await.expect("sweep"), 0);
    }
    h.clock.advance(5 * MIN);
    assert_eq!(h.supervisor.sweep().await.expect("sweep"), 1);
}

#[tokio::test]
async fn an_orphaned_container_is_removed_by_the_sweep() {
    let h = harness(4);
    let s = full();
    h.service
        .start_browser_session(start_request("run-1", &s))
        .await
        .expect("started");
    // A container this process never admitted (a previous supervisor's).
    let mut orphan = h.backend.containers.lock().unwrap()[&s.name].clone();
    orphan.container.name = "elitea-br-aaaaaaaaaaaaaaaaaaaaaaaa".into();
    h.backend
        .containers
        .lock()
        .unwrap()
        .insert(orphan.container.name.clone(), orphan);
    assert_eq!(h.supervisor.sweep().await.expect("sweep"), 1);
    let containers = h.backend.containers.lock().unwrap();
    assert!(containers.contains_key(&s.name));
    assert_eq!(containers.len(), 1);
}

#[tokio::test]
async fn a_call_under_an_older_claim_generation_is_fenced() {
    let h = harness(4);
    let s = full();
    h.service
        .start_browser_session(start_request("run-1", &s))
        .await
        .expect("started");
    h.service
        .browser_call(call_request("run-1", &s, 2))
        .await
        .expect("a newer lease");
    let status = h
        .service
        .browser_call(call_request("run-1", &s, 1))
        .await
        .expect_err("fenced");
    assert_eq!(code(&status), "browser_session_fenced");
}

#[allow(
    clippy::too_many_lines,
    reason = "one view journey: watch-only, a forged control grant, a driving viewer, the end"
)]
#[tokio::test]
async fn the_view_streams_frames_ack_paced_and_refuses_watch_only_input() {
    let h = harness(4);
    let s = full();
    h.service
        .start_browser_session(start_request("run-1", &s))
        .await
        .expect("started");
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0")
        .await
        .expect("bind");
    let address = listener.local_addr().expect("address");
    let service = Arc::clone(&h.supervisor).into_service();
    tokio::spawn(async move {
        let _ = tonic::transport::Server::builder()
            .add_service(service)
            .serve_with_incoming(tokio_stream::wrappers::TcpListenerStream::new(listener))
            .await;
    });
    let mut client = BrowserSessionServiceClient::connect(format!("http://{address}"))
        .await
        .expect("connect");
    let open = |control: bool| BrowserViewInputV1 {
        frame: Some(browser_view_input_v1::Frame::Open(BrowserViewOpenV1 {
            grant: Some(grant("run-1", &s.name, view_digest(&s, control), 1)),
            spec: Some(s.clone()),
            control,
            max_fps: 10,
        })),
    };
    let input = BrowserViewInputV1 {
        frame: Some(browser_view_input_v1::Frame::Input(
            crate::protocol::elitea::runtime::v1::BrowserInputEventV1 {
                event: Some(
                    crate::protocol::elitea::runtime::v1::browser_input_event_v1::Event::TakeOver(
                        true,
                    ),
                ),
            },
        )),
    };
    let ack = |seq| BrowserViewInputV1 {
        frame: Some(browser_view_input_v1::Frame::Ack(seq)),
    };

    // A watch-only viewer: frames arrive, input is refused with a notice.
    let (up, rx) = tokio::sync::mpsc::channel(8);
    up.send(open(false)).await.unwrap();
    let mut down = client
        .browser_view(tokio_stream::wrappers::ReceiverStream::new(rx))
        .await
        .expect("view")
        .into_inner();
    let first = down.message().await.expect("ok").expect("a frame");
    let Some(browser_view_output_v1::Frame::Screen(frame)) = first.frame else {
        panic!("a frame first: {first:?}");
    };
    assert_eq!(frame.jpeg, b"jpeg");
    assert_eq!((frame.width, frame.height), (1280, 800));
    up.send(input.clone()).await.unwrap();
    let refused = down.message().await.expect("ok").expect("notice");
    assert!(matches!(
        refused.frame,
        Some(browser_view_output_v1::Frame::Notice(ref notice)) if notice.code == "input_refused"
    ));
    up.send(ack(frame.seq)).await.unwrap();
    drop(up);

    // A watch-only grant cannot open a driving stream.
    let (up, rx) = tokio::sync::mpsc::channel(8);
    let mut forged = open(true);
    if let Some(browser_view_input_v1::Frame::Open(open)) = forged.frame.as_mut() {
        open.grant = Some(grant("run-1", &s.name, view_digest(&s, false), 1));
    }
    up.send(forged).await.unwrap();
    let status = client
        .browser_view(tokio_stream::wrappers::ReceiverStream::new(rx))
        .await
        .expect_err("refused");
    assert_eq!(status.code(), Code::PermissionDenied);

    // A driving viewer's input reaches the agent; destroying the session
    // ends the stream with its reason.
    let (up, rx) = tokio::sync::mpsc::channel(8);
    up.send(open(true)).await.unwrap();
    let mut down = client
        .browser_view(tokio_stream::wrappers::ReceiverStream::new(rx))
        .await
        .expect("view")
        .into_inner();
    let _ = down.message().await.expect("ok").expect("a frame");
    up.send(input).await.unwrap();
    let seen = Arc::clone(&h.backend.seen.lock().unwrap()[&s.name]);
    tokio::time::timeout(Duration::from_secs(5), async {
        while !seen
            .lines
            .lock()
            .unwrap()
            .iter()
            .any(|line| line["op"] == "input" && line["event"]["type"] == "take_over")
        {
            tokio::time::sleep(Duration::from_millis(10)).await;
        }
    })
    .await
    .expect("input relayed");
    h.service
        .destroy_browser_session(destroy_request("run-1", &s, BrowserSessionEndV1::Now))
        .await
        .expect("destroyed");
    let end = tokio::time::timeout(Duration::from_secs(5), async {
        loop {
            match down.message().await {
                Ok(Some(message)) => {
                    if let Some(browser_view_output_v1::Frame::End(end)) = message.frame {
                        return Some(end.code);
                    }
                }
                _ => return None,
            }
        }
    })
    .await
    .expect("ended");
    assert_eq!(end.as_deref(), Some("session_destroyed"));
}

#[tokio::test]
async fn a_watched_session_does_not_go_idle() {
    let h = harness(4);
    let s = full();
    h.service
        .start_browser_session(start_request("run-1", &s))
        .await
        .expect("started");
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0")
        .await
        .expect("bind");
    let address = listener.local_addr().expect("address");
    let service = Arc::clone(&h.supervisor).into_service();
    tokio::spawn(async move {
        let _ = tonic::transport::Server::builder()
            .add_service(service)
            .serve_with_incoming(tokio_stream::wrappers::TcpListenerStream::new(listener))
            .await;
    });
    let mut client = BrowserSessionServiceClient::connect(format!("http://{address}"))
        .await
        .expect("connect");
    let (up, rx) = tokio::sync::mpsc::channel(8);
    up.send(BrowserViewInputV1 {
        frame: Some(browser_view_input_v1::Frame::Open(BrowserViewOpenV1 {
            grant: Some(grant("run-1", &s.name, view_digest(&s, false), 1)),
            spec: Some(s.clone()),
            control: false,
            max_fps: 10,
        })),
    })
    .await
    .unwrap();
    let mut down = client
        .browser_view(tokio_stream::wrappers::ReceiverStream::new(rx))
        .await
        .expect("view")
        .into_inner();
    let first = down.message().await.expect("ok").expect("a frame");
    let Some(browser_view_output_v1::Frame::Screen(frame)) = first.frame else {
        panic!("a frame first: {first:?}");
    };
    let seen = Arc::clone(&h.backend.seen.lock().unwrap()[&s.name]);
    let acks = || {
        seen.lines
            .lock()
            .unwrap()
            .iter()
            .filter(|line| line["op"] == "ack")
            .count()
    };
    // No call, only a watch-only viewer acknowledging frames, for well past
    // the idle limit: each ack keeps the session.
    for round in 1..=3 {
        h.clock.advance(IDLE.saturating_sub(MIN));
        up.send(BrowserViewInputV1 {
            frame: Some(browser_view_input_v1::Frame::Ack(frame.seq)),
        })
        .await
        .unwrap();
        // The touch lands before the ack reaches the agent.
        tokio::time::timeout(Duration::from_secs(5), async {
            while acks() < round {
                tokio::time::sleep(Duration::from_millis(10)).await;
            }
        })
        .await
        .expect("ack relayed");
        assert_eq!(h.supervisor.sweep().await.expect("sweep"), 0);
    }
    // Once the acks stop, the idle limit applies again.
    h.clock.advance(IDLE);
    assert_eq!(h.supervisor.sweep().await.expect("sweep"), 1);
    assert!(h.backend.containers.lock().unwrap().is_empty());
}

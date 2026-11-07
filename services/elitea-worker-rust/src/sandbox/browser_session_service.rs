//! The supervisor's `browser` profile (ADR-0049 decisions 1, 4, 6 and 10):
//! `BrowserSessionService` over the session table of
//! [`super::browser_session_lifetime`] and a [`BrowserSessionBackend`]
//! (Docker, `browser_session_docker.rs`; Kubernetes,
//! `kubernetes/browser_session.rs`; tests fake it).
//!
//! The service holds the policy: grant verification (the run's claim and
//! fence, as workspace runs), the profile's image and ceilings, the
//! `--no-sandbox` rule, the lifetime and the capacity wait. The backend only
//! creates, attaches to and removes containers.

use std::collections::BTreeMap;
use std::future::Future;
use std::sync::{Arc, Mutex, PoisonError};
use std::time::Duration;

use async_trait::async_trait;
use serde_json::{Value, json};
use tokio::sync::{Notify, mpsc, watch};
use tokio_stream::wrappers::ReceiverStream;
use tonic::metadata::MetadataValue;
use tonic::{Code, Request, Response, Status, Streaming};

use super::browser_session::{
    BrowserSessionContainer, Engine, Isolation, IsolationRefused, engine_sandbox, spec_digest,
    view_digest,
};
use super::browser_session_agent::{
    AgentError, Control, Frame, ProxyLine, SessionAgent, StartLine, Viewport,
};
use super::browser_session_lifetime::{
    Admission, Clock, EndReason, Refusal, SessionClaim, SessionTable,
};
use super::workspace_run::hex;
use super::workspace_run_service::{BackendError, RunBinding};
use crate::protocol::command::Ed25519PublicKeyResolver;
use crate::protocol::elitea::runtime::v1::{
    BrowserCallRequestV1, BrowserCallResponseV1, BrowserControlV1, BrowserFrameV1, BrowserImageV1,
    BrowserInputEventV1, BrowserSessionEndV1, BrowserSessionSpecV1, BrowserViewEndV1,
    BrowserViewInputV1, BrowserViewNoticeV1, BrowserViewOutputV1, DestroyBrowserSessionRequestV1,
    DestroyBrowserSessionResponseV1, SignedSandboxJobGrantV1, StartBrowserSessionRequestV1,
    StartBrowserSessionResponseV1, browser_input_event_v1,
    browser_session_service_server::{BrowserSessionService, BrowserSessionServiceServer},
    browser_view_input_v1, browser_view_output_v1,
};
use crate::protocol::sandbox_grant::GrantVerifier;

/// The metadata key that carries a refusal's stable code.
pub const ERROR_METADATA: &str = "elitea-browser-error";
const MAX_ARGUMENTS_BYTES: usize = 64 * 1024;
const MAX_CALL_ID_BYTES: usize = 128;
const MAX_KEY_BYTES: usize = 32;
const MAX_NOTE_BYTES: usize = 2 * 1024;
const MAX_FPS: u32 = 10;
const SWEEP_INTERVAL: Duration = Duration::from_secs(15);
/// The backstop past a session's wall and linger after which the backend
/// itself ends it (Kubernetes `activeDeadlineSeconds`, the Docker label).
pub const BACKSTOP_GRACE_SECONDS: u64 = 120;

/// One engine's ceiling.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct EngineCeiling {
    pub max_cpu_millis: u64,
    pub max_memory_bytes: u64,
}

/// The profile's image and ceilings (ADR-0049 decision 1).
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct BrowserCeilings {
    /// The session image, pinned by digest (chrome-headless-shell).
    pub image: String,
    /// The `lite` session image (Moli and chrome-headless-shell), pinned by
    /// digest; `None` when the profile does not run Moli.
    pub lite_image: Option<String>,
    pub full: EngineCeiling,
    /// `None`: the profile does not run `lite`.
    pub lite: Option<EngineCeiling>,
    pub max_wall_seconds: u64,
    pub max_pages: u32,
    pub sessions_per_run: usize,
    /// Sessions alive at once.
    pub concurrency: usize,
    /// The engine keeps its own sandbox. `false` (`--no-sandbox`) is
    /// admitted only on a gVisor runtime.
    pub engine_sandbox: bool,
}

impl BrowserCeilings {
    /// The ceiling a session of `engine` runs under: `auto` may end on
    /// Chrome, so it takes the `full` ceiling, and like `lite` it needs the
    /// profile to run Moli at all.
    fn engine(&self, engine: Engine) -> Option<EngineCeiling> {
        match engine {
            Engine::Full => Some(self.full),
            Engine::Lite => self.lite,
            Engine::Auto => self.lite.map(|_| self.full),
        }
    }

    /// The image a session of `engine` runs.
    fn image(&self, engine: Engine) -> Option<&str> {
        if engine.needs_moli() {
            self.lite_image.as_deref()
        } else {
            Some(self.image.as_str())
        }
    }
}

/// One session to create: the container and its binding.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct BrowserSessionLaunch {
    pub container: BrowserSessionContainer,
    pub binding: RunBinding,
    /// The backend's own end (Kubernetes `activeDeadlineSeconds`).
    pub backstop_seconds: u64,
}

/// The container side of browser sessions.
#[async_trait]
pub trait BrowserSessionBackend: Send + Sync + 'static {
    /// What isolates a session's container (verified, where it can be).
    async fn isolation(&self) -> Result<Isolation, BackendError>;
    /// Create and start the container. A name that exists is `Conflict`:
    /// a session is never reused.
    async fn create(&self, launch: &BrowserSessionLaunch) -> Result<(), BackendError>;
    /// Exec the session agent in the container.
    async fn attach(&self, name: &str) -> Result<Arc<dyn SessionAgent>, BackendError>;
    /// Remove the container; `false` when it was gone.
    async fn destroy(&self, name: &str) -> Result<bool, BackendError>;
    /// Every browser-session container this backend holds.
    async fn sessions(&self) -> Result<Vec<String>, BackendError>;
}

fn coded(code: Code, error: &'static str, message: &'static str) -> Status {
    let mut status = Status::new(code, message);
    status
        .metadata_mut()
        .insert(ERROR_METADATA, MetadataValue::from_static(error));
    status
}

fn refusal_status(refusal: Refusal) -> Status {
    match refusal {
        Refusal::Full => coded(
            Code::ResourceExhausted,
            "browser_capacity",
            "The browser profile is at its capacity.",
        ),
        Refusal::SessionLimit => coded(
            Code::ResourceExhausted,
            "browser_session_limit",
            "The run holds its most browser sessions.",
        ),
        Refusal::Ended => coded(
            Code::FailedPrecondition,
            "browser_session_ended",
            "The browser session has ended.",
        ),
        Refusal::NotStarted => coded(
            Code::NotFound,
            "browser_session_not_started",
            "The browser session does not exist.",
        ),
        Refusal::Conflict => coded(
            Code::PermissionDenied,
            "browser_session_conflict",
            "The browser session belongs to another claim.",
        ),
        Refusal::Fenced => coded(
            Code::FailedPrecondition,
            "browser_session_fenced",
            "The claim was superseded.",
        ),
    }
}

fn backend_status(error: BackendError) -> Status {
    match error {
        BackendError::Refused => coded(
            Code::FailedPrecondition,
            "browser_isolation_refused",
            "The browser session's runtime is refused.",
        ),
        BackendError::Conflict => coded(
            Code::FailedPrecondition,
            "browser_session_ended",
            "The browser session has ended.",
        ),
        BackendError::NotFound => refusal_status(Refusal::NotStarted),
        BackendError::Timeout | BackendError::Unavailable => coded(
            Code::Unavailable,
            "browser_agent_failed",
            "The browser session could not be reached.",
        ),
    }
}

fn agent_status(error: &AgentError) -> Status {
    match error {
        AgentError::Timeout => coded(
            Code::DeadlineExceeded,
            "browser_agent_failed",
            "The browser session did not answer.",
        ),
        AgentError::Gone | AgentError::Failed(_) => coded(
            Code::Unavailable,
            "browser_agent_failed",
            "The browser session failed.",
        ),
    }
}

fn now_unix_seconds() -> u64 {
    std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map_or(0, |elapsed| elapsed.as_secs())
}

fn now_millis() -> i64 {
    i64::try_from(
        std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .map_or(0, |elapsed| elapsed.as_millis()),
    )
    .unwrap_or(i64::MAX)
}

/// One live session's handle.
struct Live {
    agent: Arc<dyn SessionAgent>,
    engine_version: String,
    /// Set once, when the session goes away.
    ended: watch::Sender<Option<EndReason>>,
}

/// The browser-session service of one profile.
pub struct BrowserSessionSupervisor<R> {
    verifier: GrantVerifier<R>,
    backend: Arc<dyn BrowserSessionBackend>,
    ceilings: BrowserCeilings,
    clock: Arc<dyn Clock>,
    table: Mutex<SessionTable>,
    live: Mutex<BTreeMap<String, Live>>,
    /// Signalled whenever a slot frees.
    freed: Notify,
    /// Tests only: the peer a plaintext test listener cannot authenticate.
    fixed_peer: Option<String>,
}

impl<R: Ed25519PublicKeyResolver + Send + Sync + 'static> BrowserSessionSupervisor<R> {
    #[must_use]
    pub fn new(
        verifier: GrantVerifier<R>,
        backend: Arc<dyn BrowserSessionBackend>,
        ceilings: BrowserCeilings,
        clock: Arc<dyn Clock>,
    ) -> Self {
        let limits = super::browser_session_lifetime::LifetimeLimits::new(
            ceilings.concurrency,
            ceilings.sessions_per_run,
        );
        Self {
            verifier,
            backend,
            ceilings,
            clock,
            table: Mutex::new(SessionTable::new(limits)),
            live: Mutex::new(BTreeMap::new()),
            freed: Notify::new(),
            fixed_peer: None,
        }
    }

    #[cfg(test)]
    #[must_use]
    pub(crate) fn with_fixed_peer(mut self, peer: &str) -> Self {
        self.fixed_peer = Some(peer.to_owned());
        self
    }

    #[cfg(test)]
    pub(crate) fn with_limits(
        self,
        update: impl FnOnce(&mut super::browser_session_lifetime::LifetimeLimits),
    ) -> Self {
        let mut limits = self.table().limits();
        update(&mut limits);
        *self.table() = SessionTable::new(limits);
        self
    }

    fn table(&self) -> std::sync::MutexGuard<'_, SessionTable> {
        self.table.lock().unwrap_or_else(PoisonError::into_inner)
    }

    fn live(&self) -> std::sync::MutexGuard<'_, BTreeMap<String, Live>> {
        self.live.lock().unwrap_or_else(PoisonError::into_inner)
    }

    fn peer<T>(&self, request: &Request<T>) -> Result<String, Status> {
        match &self.fixed_peer {
            Some(peer) => Ok(peer.clone()),
            None => super::peer_identity::authenticated_peer(request),
        }
    }

    /// The spec within this profile's ceilings, and the grant's claim for
    /// `digest` (the session's, or a view stream's).
    fn authorize(
        &self,
        peer: &str,
        grant: Option<&SignedSandboxJobGrantV1>,
        spec: Option<&BrowserSessionSpecV1>,
        egress: bool,
        digest: impl FnOnce(&BrowserSessionSpecV1) -> [u8; 32],
    ) -> Result<(SessionClaim, BrowserSessionContainer), Status> {
        let (Some(grant), Some(spec)) = (grant, spec) else {
            return Err(Status::invalid_argument("A grant and a spec are required."));
        };
        let unavailable = || {
            coded(
                Code::FailedPrecondition,
                "browser_engine_unavailable",
                "This profile does not run that engine.",
            )
        };
        // The image follows the engine: Moli is only in the `lite` image.
        let engine = super::browser_session::Engine::from_wire(spec.engine)
            .ok_or_else(|| Status::invalid_argument("The browser session spec is invalid."))?;
        let image = self.ceilings.image(engine).ok_or_else(unavailable)?;
        let container = BrowserSessionContainer::from_wire(spec, image, egress)
            .map_err(|_| Status::invalid_argument("The browser session spec is invalid."))?;
        let Some(ceiling) = self.ceilings.engine(container.engine) else {
            return Err(unavailable());
        };
        if container.cpu_millis > ceiling.max_cpu_millis
            || container.memory_bytes > ceiling.max_memory_bytes
            || container.wall_seconds > self.ceilings.max_wall_seconds
            || container.max_pages > self.ceilings.max_pages
        {
            return Err(coded(
                Code::InvalidArgument,
                "browser_ceiling_exceeded",
                "The browser session exceeds this profile's ceilings.",
            ));
        }
        let authorized = self
            .verifier
            .verify_browser_session(grant, peer, &spec.name, digest(spec), now_millis())
            .map_err(|_| {
                Status::permission_denied("The grant does not authorize this browser session.")
            })?;
        Ok((
            SessionClaim {
                request: hex(&spec_digest(spec)),
                owner: authorized.owner,
                run: authorized.run,
                generation: authorized.generation,
            },
            container,
        ))
    }

    /// Admit `name`, waiting up to the capacity wait for a free slot.
    async fn admit(
        &self,
        name: &str,
        claim: &SessionClaim,
        wall: Duration,
    ) -> Result<Admission, Refusal> {
        let deadline = tokio::time::Instant::now() + self.table().limits().capacity_wait;
        loop {
            // Registered before the check, so a slot freed in between is
            // not missed.
            let freed = self.freed.notified();
            tokio::pin!(freed);
            freed.as_mut().enable();
            match self.table().admit(name, claim, wall, self.clock.now()) {
                Err(Refusal::Full) => {}
                outcome => return outcome,
            }
            if tokio::time::timeout_at(deadline, freed).await.is_err() {
                tracing::warn!(
                    event = "browser_session_capacity",
                    session = name,
                    "no browser slot freed within the capacity wait"
                );
                return Err(Refusal::Full);
            }
        }
    }

    /// Remove a session for `reason`: its slot, its agent and its container.
    /// The name is never admitted again.
    pub async fn retire(&self, name: &str, reason: EndReason) {
        let held = self.table().retire(name, self.clock.now());
        let live = self.live().remove(name);
        if let Some(live) = &live {
            live.ended.send_replace(Some(reason));
            live.agent.destroy().await;
        }
        if held || live.is_some() {
            if let Err(error) = self.backend.destroy(name).await {
                tracing::warn!(event = "browser_session_destroy_failed", session = name, reason = %error);
            }
            tracing::info!(
                event = "browser_session_destroyed",
                session = name,
                reason = reason.code()
            );
        }
        self.freed.notify_waiters();
    }

    /// Retire every session past its idle limit, wall or linger, and remove
    /// every container the table does not hold (a previous process's, or a
    /// leftover of a failed destroy). Returns the sessions removed.
    ///
    /// # Errors
    ///
    /// The backend could not be listed.
    pub async fn sweep(&self) -> Result<usize, BackendError> {
        let expired = self.table().expired(self.clock.now());
        let mut removed = expired.len();
        for (name, reason) in expired {
            self.retire(&name, reason).await;
        }
        for name in self.backend.sessions().await? {
            if !self.table().contains(&name) {
                // Never adopted: its agent died with the previous exec.
                self.table().retire(&name, self.clock.now());
                if self.backend.destroy(&name).await.unwrap_or(false) {
                    removed += 1;
                    tracing::warn!(
                        event = "browser_session_orphan_removed",
                        session = name.as_str()
                    );
                }
            }
        }
        Ok(removed)
    }

    /// Sweep every 15 seconds.
    pub fn sweeper(self: &Arc<Self>) -> impl Future<Output = ()> + Send + 'static {
        let this = Arc::clone(self);
        async move {
            let mut interval = tokio::time::interval(SWEEP_INTERVAL);
            loop {
                interval.tick().await;
                if let Err(error) = this.sweep().await {
                    tracing::warn!(event = "browser_session_sweep_failed", reason = %error);
                }
            }
        }
    }

    /// The gRPC service over this supervisor.
    #[must_use]
    pub fn into_service(
        self: Arc<Self>,
    ) -> BrowserSessionServiceServer<SharedBrowserSupervisor<R>> {
        BrowserSessionServiceServer::new(SharedBrowserSupervisor(self))
            .max_decoding_message_size(MAX_ARGUMENTS_BYTES + 8 * 1024)
            .max_encoding_message_size(super::browser_session_agent::MAX_LINE_BYTES)
    }

    async fn start(
        self: &Arc<Self>,
        claim: SessionClaim,
        container: BrowserSessionContainer,
        proxy: Option<ProxyLine>,
        attended: bool,
    ) -> Result<StartBrowserSessionResponseV1, Status> {
        let name = container.name.clone();
        let wall = Duration::from_secs(container.wall_seconds);
        match self
            .admit(&name, &claim, wall)
            .await
            .map_err(refusal_status)?
        {
            Admission::Existing => {
                let engine_version = self
                    .live()
                    .get(&name)
                    .map(|live| live.engine_version.clone())
                    .unwrap_or_default();
                return Ok(StartBrowserSessionResponseV1 {
                    created: false,
                    engine_version,
                });
            }
            Admission::New => {}
        }
        match self.launch(&claim, &container, proxy, attended).await {
            Ok(response) => Ok(response),
            Err(status) => {
                self.table().abandon(&name);
                self.freed.notify_waiters();
                Err(status)
            }
        }
    }

    async fn launch(
        self: &Arc<Self>,
        claim: &SessionClaim,
        container: &BrowserSessionContainer,
        proxy: Option<ProxyLine>,
        attended: bool,
    ) -> Result<StartBrowserSessionResponseV1, Status> {
        let name = container.name.clone();
        let isolation = self.backend.isolation().await.map_err(backend_status)?;
        let sandboxed = engine_sandbox(self.ceilings.engine_sandbox, isolation).map_err(
            |IsolationRefused| {
                tracing::error!(
                    event = "browser_session_isolation_refused",
                    session = name.as_str(),
                    "--no-sandbox is configured and the runtime is not gVisor"
                );
                coded(
                    Code::FailedPrecondition,
                    "browser_isolation_refused",
                    "The engine would run without a sandbox on this runtime.",
                )
            },
        )?;
        let linger = self.table().limits().linger.as_secs();
        let lifetime = container.wall_seconds + linger + BACKSTOP_GRACE_SECONDS;
        let launch = BrowserSessionLaunch {
            container: container.clone(),
            binding: RunBinding {
                request: claim.request.clone(),
                owner: claim.owner.clone(),
                expires_at: now_unix_seconds() + lifetime,
            },
            backstop_seconds: lifetime,
        };
        self.backend.create(&launch).await.map_err(backend_status)?;
        let started = async {
            let agent = self.backend.attach(&name).await.map_err(backend_status)?;
            let start = StartLine {
                engine: container.engine.as_str(),
                engine_sandbox: sandboxed,
                max_pages: container.max_pages,
                viewport: Viewport::default(),
                proxy,
                attended,
            };
            let engine_version = agent
                .start(&start)
                .await
                .map_err(|error| agent_status(&error))?;
            Ok::<_, Status>((agent, engine_version))
        }
        .await;
        let (agent, engine_version) = match started {
            Ok(started) => started,
            Err(status) => {
                let _ = self.backend.destroy(&name).await;
                return Err(status);
            }
        };
        let (ended, _) = watch::channel(None);
        let mut gone = agent.gone();
        self.live().insert(
            name.clone(),
            Live {
                agent,
                engine_version: engine_version.clone(),
                ended,
            },
        );
        self.table().confirm(&name, self.clock.now());
        // An agent that exits ends its session.
        let this = Arc::clone(self);
        let watched = name.clone();
        tokio::spawn(async move {
            if gone.wait_for(|gone| *gone).await.is_ok() && this.table().contains(&watched) {
                this.retire(&watched, EndReason::AgentFailed).await;
            }
        });
        tracing::info!(
            event = "browser_session_started",
            session = name.as_str(),
            engine = container.engine.as_str(),
            engine_sandbox = sandboxed
        );
        Ok(StartBrowserSessionResponseV1 {
            created: true,
            engine_version,
        })
    }

    fn agent(&self, name: &str) -> Result<Arc<dyn SessionAgent>, Status> {
        self.live()
            .get(name)
            .map(|live| Arc::clone(&live.agent))
            .ok_or_else(|| refusal_status(Refusal::NotStarted))
    }
}

/// The service handle tonic owns.
pub struct SharedBrowserSupervisor<R>(pub Arc<BrowserSessionSupervisor<R>>);

fn valid_call_id(id: &str) -> bool {
    !id.is_empty()
        && id.len() <= MAX_CALL_ID_BYTES
        && id
            .bytes()
            .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'_' | b'.' | b':' | b'-'))
}

/// A viewer's input event as the agent's line carries it. Bounded; the
/// agent applies it to the page through CDP.
fn input_line(event: &BrowserInputEventV1) -> Option<Value> {
    use browser_input_event_v1::Event;
    let bounded = |text: &str| text.len() <= MAX_KEY_BYTES;
    Some(match event.event.as_ref()? {
        Event::Pointer(pointer) => {
            let kind = match pointer.kind() {
                crate::protocol::elitea::runtime::v1::BrowserPointerKindV1::Move => "move",
                crate::protocol::elitea::runtime::v1::BrowserPointerKindV1::Down => "down",
                crate::protocol::elitea::runtime::v1::BrowserPointerKindV1::Up => "up",
                crate::protocol::elitea::runtime::v1::BrowserPointerKindV1::Unspecified => {
                    return None;
                }
            };
            if !pointer.x.is_finite() || !pointer.y.is_finite() || pointer.button > 3 {
                return None;
            }
            json!({"type": "pointer", "kind": kind, "x": pointer.x, "y": pointer.y,
                "button": pointer.button, "click_count": pointer.click_count.min(3),
                "modifiers": pointer.modifiers & 0xf})
        }
        Event::Wheel(wheel) => {
            if ![wheel.x, wheel.y, wheel.delta_x, wheel.delta_y]
                .iter()
                .all(|value| value.is_finite())
            {
                return None;
            }
            json!({"type": "wheel", "x": wheel.x, "y": wheel.y, "delta_x": wheel.delta_x,
                "delta_y": wheel.delta_y, "modifiers": wheel.modifiers & 0xf})
        }
        Event::Key(key) => {
            let kind = match key.kind() {
                crate::protocol::elitea::runtime::v1::BrowserKeyKindV1::Down => "down",
                crate::protocol::elitea::runtime::v1::BrowserKeyKindV1::Up => "up",
                crate::protocol::elitea::runtime::v1::BrowserKeyKindV1::Char => "char",
                crate::protocol::elitea::runtime::v1::BrowserKeyKindV1::Unspecified => {
                    return None;
                }
            };
            if !bounded(&key.key) || !bounded(&key.code) || !bounded(&key.text) {
                return None;
            }
            json!({"type": "key", "kind": kind, "key": key.key, "code": key.code,
                "text": key.text, "modifiers": key.modifiers & 0xf})
        }
        Event::TakeOver(true) => json!({"type": "take_over"}),
        Event::TakeOver(false) => return None,
        Event::HandBack(hand_back) => {
            if hand_back.note.len() > MAX_NOTE_BYTES {
                return None;
            }
            json!({"type": "hand_back", "note": hand_back.note})
        }
    })
}

#[cfg(test)]
mod input_line_tests {
    use super::input_line;
    use crate::protocol::elitea::runtime::v1::{
        BrowserInputEventV1, BrowserPointerEventV1, BrowserPointerKindV1, browser_input_event_v1,
    };

    fn pointer(kind: BrowserPointerKindV1, button: u32) -> BrowserInputEventV1 {
        BrowserInputEventV1 {
            event: Some(browser_input_event_v1::Event::Pointer(
                BrowserPointerEventV1 {
                    kind: kind.into(),
                    x: 1.0,
                    y: 2.0,
                    button,
                    click_count: 1,
                    modifiers: 0,
                },
            )),
        }
    }

    // The proto's numbering (0 none, 1 left, 2 middle, 3 right) reaches the
    // agent unchanged: the agent, not the supervisor, maps it to CDP's names.
    #[test]
    fn pointer_buttons_pass_through_in_the_protos_numbering() {
        for button in 0..=3 {
            let line = input_line(&pointer(BrowserPointerKindV1::Down, button)).unwrap();
            assert_eq!(line["button"], button);
        }
        assert!(input_line(&pointer(BrowserPointerKindV1::Down, 4)).is_none());
    }
}

fn frame_out(frame: &Frame) -> BrowserViewOutputV1 {
    BrowserViewOutputV1 {
        frame: Some(browser_view_output_v1::Frame::Screen(BrowserFrameV1 {
            seq: frame.seq,
            jpeg: frame.jpeg.clone(),
            width: frame.width,
            height: frame.height,
            url: frame.url.clone(),
            title: frame.title.clone(),
            control: match frame.control {
                Control::Agent => BrowserControlV1::Agent,
                Control::User => BrowserControlV1::User,
            }
            .into(),
        })),
    }
}

fn notice(code: &str) -> BrowserViewOutputV1 {
    BrowserViewOutputV1 {
        frame: Some(browser_view_output_v1::Frame::Notice(BrowserViewNoticeV1 {
            code: code.to_owned(),
        })),
    }
}

type ViewStream = ReceiverStream<Result<BrowserViewOutputV1, Status>>;

#[tonic::async_trait]
impl<R: Ed25519PublicKeyResolver + Send + Sync + 'static> BrowserSessionService
    for SharedBrowserSupervisor<R>
{
    async fn start_browser_session(
        &self,
        request: Request<StartBrowserSessionRequestV1>,
    ) -> Result<Response<StartBrowserSessionResponseV1>, Status> {
        let this = &self.0;
        let peer = this.peer(&request)?;
        let request = request.into_inner();
        let attended = !request.unattended;
        let proxy = match request.egress {
            None => None,
            Some(egress) => {
                let ip_literal = egress
                    .proxy_url
                    .strip_prefix("http://")
                    .and_then(|rest| rest.parse::<std::net::SocketAddr>().ok())
                    .is_some();
                if !ip_literal || egress.proxy_token.is_empty() || egress.proxy_token.len() > 8192 {
                    return Err(Status::invalid_argument(
                        "The egress proxy must be an http:// IP literal with a token.",
                    ));
                }
                Some(ProxyLine {
                    url: egress.proxy_url,
                    token: egress.proxy_token,
                })
            }
        };
        let (claim, container) = this.authorize(
            &peer,
            request.grant.as_ref(),
            request.spec.as_ref(),
            proxy.is_some(),
            spec_digest,
        )?;
        Ok(Response::new(
            this.start(claim, container, proxy, attended).await?,
        ))
    }

    async fn browser_call(
        &self,
        request: Request<BrowserCallRequestV1>,
    ) -> Result<Response<BrowserCallResponseV1>, Status> {
        let this = &self.0;
        let peer = this.peer(&request)?;
        let request = request.into_inner();
        if !valid_call_id(&request.call_id)
            || elitea_browser_tools::spec::find(&request.name).is_none()
            || request.arguments_json.len() > MAX_ARGUMENTS_BYTES
        {
            return Err(Status::invalid_argument("The browser call is invalid."));
        }
        let Ok(Value::Object(arguments)) = serde_json::from_str::<Value>(&request.arguments_json)
        else {
            return Err(Status::invalid_argument(
                "The browser call's arguments must be a JSON object.",
            ));
        };
        let (claim, container) = this.authorize(
            &peer,
            request.grant.as_ref(),
            request.spec.as_ref(),
            false,
            spec_digest,
        )?;
        this.table()
            .touch(&container.name, &claim, this.clock.now())
            .map_err(refusal_status)?;
        let agent = this.agent(&container.name)?;
        let answer = agent
            .call(&request.call_id, &request.name, &Value::Object(arguments))
            .await
            .map_err(|error| agent_status(&error))?;
        // The call's own duration is activity too.
        let _ = this
            .table()
            .touch(&container.name, &claim, this.clock.now());
        Ok(Response::new(BrowserCallResponseV1 {
            result_json: answer.result.to_string(),
            image: answer
                .image
                .map(|(media_type, data)| BrowserImageV1 { media_type, data }),
        }))
    }

    type BrowserViewStream = ViewStream;

    #[allow(
        clippy::too_many_lines,
        reason = "one view: open, authorize, pump frames with ack pacing, relay input"
    )]
    async fn browser_view(
        &self,
        request: Request<Streaming<BrowserViewInputV1>>,
    ) -> Result<Response<Self::BrowserViewStream>, Status> {
        use browser_view_input_v1::Frame as In;
        let this = Arc::clone(&self.0);
        let peer = this.peer(&request)?;
        let mut inbound = request.into_inner();
        let Some(BrowserViewInputV1 {
            frame: Some(In::Open(open)),
        }) = inbound.message().await?
        else {
            return Err(Status::invalid_argument(
                "A view starts with its open frame.",
            ));
        };
        let control = open.control;
        let (claim, container) = this.authorize(
            &peer,
            open.grant.as_ref(),
            open.spec.as_ref(),
            false,
            |spec| view_digest(spec, control),
        )?;
        let name = container.name;
        let lingering = this
            .table()
            .viewable(&name, &claim)
            .map_err(refusal_status)?;
        let (agent, mut ended) = {
            let live = this.live();
            let live = live
                .get(&name)
                .ok_or_else(|| refusal_status(Refusal::NotStarted))?;
            (Arc::clone(&live.agent), live.ended.subscribe())
        };
        let max_fps = match open.max_fps {
            0 => MAX_FPS,
            fps => fps.min(MAX_FPS),
        };
        agent.viewer_attached(max_fps).await;
        let (tx, rx) = mpsc::channel::<Result<BrowserViewOutputV1, Status>>(4);
        let (acked_tx, mut acked) = watch::channel(0_u64);
        // The first message says the view is admitted, and in which state:
        // Main waits for it before it upgrades the viewer's WebSocket, so a
        // refusal above is an HTTP status there and never a closed socket.
        let admitted = if lingering {
            "session_lingering"
        } else {
            "session_live"
        };
        let _ = tx.send(Ok(notice(admitted))).await;
        // Down: the latest frame, one at a time, each after the viewer
        // acknowledged the one before it.
        let mut frames = agent.frames();
        let down = tx.clone();
        tokio::spawn(async move {
            let mut sent: Option<u64> = None;
            let interval = Duration::from_millis(1_000 / u64::from(max_fps));
            loop {
                let ready = sent.is_none_or(|seq| *acked.borrow() >= seq);
                tokio::select! {
                    reason = async { ended.wait_for(Option::is_some).await.ok().and_then(|reason| *reason) } => {
                        let code = reason.unwrap_or(EndReason::Destroyed).code();
                        let _ = down.send(Ok(BrowserViewOutputV1 {
                            frame: Some(browser_view_output_v1::Frame::End(BrowserViewEndV1 { code: code.to_owned() })),
                        })).await;
                        return;
                    }
                    changed = frames.changed(), if ready => {
                        if changed.is_err() {
                            return;
                        }
                        let frame = frames.borrow_and_update().clone();
                        if let Some(frame) = frame {
                            if down.send(Ok(frame_out(&frame))).await.is_err() {
                                return;
                            }
                            sent = Some(frame.seq);
                            tokio::time::sleep(interval).await;
                        }
                    }
                    changed = acked.changed(), if !ready => {
                        if changed.is_err() {
                            return;
                        }
                    }
                    () = down.closed() => return,
                }
            }
        });
        // Up: acks and, on a driving stream of a live session, input.
        let up = tx;
        tokio::spawn(async move {
            while let Ok(Some(message)) = inbound.message().await {
                match message.frame {
                    Some(In::Ack(seq)) => {
                        // A person watching keeps the session from going
                        // idle. A lingering session refuses the touch,
                        // which is fine: it is already ending.
                        let _ = this.table().touch(&name, &claim, this.clock.now());
                        acked_tx.send_replace(seq);
                        agent.ack(seq).await;
                    }
                    Some(In::Input(event)) => {
                        let line = input_line(&event);
                        let admitted = control
                            && line.is_some()
                            && this.table().touch(&name, &claim, this.clock.now()).is_ok();
                        match line {
                            Some(line) if admitted => {
                                if agent.input(&line).await.is_err() {
                                    break;
                                }
                            }
                            _ => {
                                if up.send(Ok(notice("input_refused"))).await.is_err() {
                                    break;
                                }
                            }
                        }
                    }
                    _ => break,
                }
            }
            agent.viewer_detached().await;
        });
        Ok(Response::new(ReceiverStream::new(rx)))
    }

    async fn destroy_browser_session(
        &self,
        request: Request<DestroyBrowserSessionRequestV1>,
    ) -> Result<Response<DestroyBrowserSessionResponseV1>, Status> {
        let this = &self.0;
        let peer = this.peer(&request)?;
        let request = request.into_inner();
        let end = BrowserSessionEndV1::try_from(request.end)
            .ok()
            .filter(|end| *end != BrowserSessionEndV1::Unspecified)
            .ok_or_else(|| Status::invalid_argument("The end is unknown."))?;
        let (claim, container) = this.authorize(
            &peer,
            request.grant.as_ref(),
            request.spec.as_ref(),
            false,
            spec_digest,
        )?;
        let name = container.name;
        let now = this.clock.now();
        let gone = |refusal: Refusal| {
            matches!(refusal, Refusal::Ended | Refusal::NotStarted).then_some(
                DestroyBrowserSessionResponseV1 {
                    destroyed: false,
                    linger_until_unix_seconds: 0,
                },
            )
        };
        if end == BrowserSessionEndV1::RunEnded {
            let until = match this.table().run_ended(&name, &claim, now) {
                Ok(until) => until,
                Err(refusal) => {
                    return gone(refusal)
                        .map(Response::new)
                        .ok_or_else(|| refusal_status(refusal));
                }
            };
            if let Ok(agent) = this.agent(&name) {
                agent.run_ended().await;
            }
            tracing::info!(event = "browser_session_lingering", session = name.as_str());
            return Ok(Response::new(DestroyBrowserSessionResponseV1 {
                destroyed: false,
                linger_until_unix_seconds: now_unix_seconds() + until.saturating_sub(now).as_secs(),
            }));
        }
        if let Err(refusal) = this.table().check_owner(&name, &claim) {
            return gone(refusal)
                .map(Response::new)
                .ok_or_else(|| refusal_status(refusal));
        }
        this.retire(&name, EndReason::Destroyed).await;
        Ok(Response::new(DestroyBrowserSessionResponseV1 {
            destroyed: true,
            linger_until_unix_seconds: 0,
        }))
    }
}

/// An in-memory backend for tests: a container is a fake agent on a pipe
/// pair (`browser_session_agent::fake`).
#[cfg(test)]
pub(crate) mod fake {
    use super::*;
    use crate::sandbox::browser_session_agent::{LineAgent, fake as agent_fake};

    pub(crate) struct FakeBackend {
        pub(crate) isolation: Mutex<Isolation>,
        pub(crate) containers: Mutex<BTreeMap<String, BrowserSessionLaunch>>,
        pub(crate) created: Mutex<Vec<String>>,
        pub(crate) destroyed: Mutex<Vec<String>>,
        pub(crate) seen: Mutex<BTreeMap<String, Arc<agent_fake::Seen>>>,
    }

    impl Default for FakeBackend {
        fn default() -> Self {
            Self {
                isolation: Mutex::new(Isolation::Gvisor),
                containers: Mutex::default(),
                created: Mutex::default(),
                destroyed: Mutex::default(),
                seen: Mutex::default(),
            }
        }
    }

    #[async_trait]
    impl BrowserSessionBackend for FakeBackend {
        async fn isolation(&self) -> Result<Isolation, BackendError> {
            Ok(*self.isolation.lock().unwrap())
        }

        async fn create(&self, launch: &BrowserSessionLaunch) -> Result<(), BackendError> {
            let mut containers = self.containers.lock().unwrap();
            if containers.contains_key(&launch.container.name) {
                return Err(BackendError::Conflict);
            }
            containers.insert(launch.container.name.clone(), launch.clone());
            self.created
                .lock()
                .unwrap()
                .push(launch.container.name.clone());
            Ok(())
        }

        async fn attach(&self, name: &str) -> Result<Arc<dyn SessionAgent>, BackendError> {
            if !self.containers.lock().unwrap().contains_key(name) {
                return Err(BackendError::NotFound);
            }
            let (agent_in, fake_in) = tokio::io::duplex(1 << 20);
            let (fake_out, agent_out) = tokio::io::duplex(1 << 20);
            let seen = Arc::new(agent_fake::Seen::default());
            self.seen
                .lock()
                .unwrap()
                .insert(name.to_owned(), Arc::clone(&seen));
            tokio::spawn(agent_fake::serve(fake_in, fake_out, seen));
            Ok(LineAgent::new(agent_in, agent_out))
        }

        async fn destroy(&self, name: &str) -> Result<bool, BackendError> {
            self.destroyed.lock().unwrap().push(name.to_owned());
            Ok(self.containers.lock().unwrap().remove(name).is_some())
        }

        async fn sessions(&self) -> Result<Vec<String>, BackendError> {
            Ok(self.containers.lock().unwrap().keys().cloned().collect())
        }
    }
}

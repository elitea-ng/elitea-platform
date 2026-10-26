//! The ACP agent: sessions, prompt turns and permission requests over one
//! stdio connection, on the desktop's own agent host.
//!
//! A session is a Local work conversation on the server, keyed by its UUID
//! (`session/new` creates it, `session/load` reopens it). A prompt is one
//! `AgentHost::start` turn, exactly as `agent_turn_start` runs it (resolve,
//! start, run, commit): the same tools, sandbox, deny list, approval rules
//! and checkpoints. Its `agent://event` stream is turned into
//! `session/update` notifications; an `approval_request` (the rules engine
//! said *ask*) becomes `session/request_permission`, and the answer goes
//! back through the same broker the desktop UI answers. A policy deny, a
//! plan-mode deny or a rule's deny never reaches the client: the engine
//! decides those before anything is asked.
//!
//! A `session/prompt` that arrives while a turn of its session runs steers
//! that turn (IPC.md, "Steering"): its text goes into the turn's steering
//! queue, the agent takes it at the next safe point, and the prompt is
//! answered when the turn ends, with the turn's stop reason. A prompt the
//! turn did not take runs as the next turn of the session.

use std::collections::{HashMap, HashSet, VecDeque};
use std::path::{Path, PathBuf};
use std::sync::atomic::{AtomicBool, AtomicI64, Ordering};
use std::sync::{Arc, Mutex};
use std::time::Duration;

use reqwest::Method;
use serde::Serialize;
use serde::de::DeserializeOwned;
use serde_json::{Value, json};
use tokio::io::{AsyncRead, AsyncWrite, BufReader};
use tokio::sync::{mpsc, oneshot};

use super::rpc::{self, Id, Incoming, Line, RpcError};
use super::types::{
    AgentCapabilities, AuthenticateRequest, AvailableCommand, AvailableCommandInput,
    AvailableCommandsUpdate, CancelNotification, ContentBlock, ContentChunk, CurrentModeUpdate,
    Implementation, InitializeRequest, InitializeResponse, LoadSessionRequest, LoadSessionResponse,
    McpCapabilities, McpServer, NewSessionRequest, NewSessionResponse, PROTOCOL_VERSION,
    PermissionOption, PermissionOptionKind, Plan, PlanEntry, PlanEntryPriority, PlanEntryStatus,
    PromptCapabilities, PromptRequest, PromptResponse, RequestPermissionOutcome,
    RequestPermissionRequest, RequestPermissionResponse, SessionMode, SessionModeState,
    SessionNotification, SessionUpdate, SetSessionModeRequest, StopReason, ToolCall,
    ToolCallContent, ToolCallStatus, ToolCallUpdate, ToolKind,
};
use crate::d0::api::PlatformApi;
use crate::d0::approvals::UiDecision;
use crate::d0::attachments::{ImagePick, ImageSource};
use crate::d0::events::{AgentEvent, EventEmitter};
use crate::d0::slash_commands::CommandPick;
use crate::d0::steering;
use crate::d0::turn::{AgentHost, TurnError, TurnRequest};
use crate::workspaces::WorkspaceStore;

/// The modes a session offers: the desktop's build and plan profiles.
pub const MODE_DEFAULT: &str = "default";
pub const MODE_PLAN: &str = "plan";

/// The most content blocks one prompt may carry.
const MAX_PROMPT_BLOCKS: usize = 256;
/// The most text one prompt may carry, all its text blocks together.
const MAX_PROMPT_TEXT_BYTES: usize = 1024 * 1024;
/// The most images one prompt may carry (the turn's own limit).
const MAX_PROMPT_IMAGES: usize = 5;
/// The longest `cwd` accepted.
const MAX_PATH_BYTES: usize = 4096;
/// Events of turns nobody routed yet (a start's own events arrive before
/// `start` returns its turn id), kept for at most this many turns.
const MAX_PARKED_TURNS: usize = 16;
pub(super) const MAX_PARKED_EVENTS: usize = 4096;
/// Beyond `MAX_PARKED_EVENTS`, a turn's ending events (`status`, `error`,
/// `done`) are still parked, up to this many more: a full buffer drops
/// deltas, never the event that settles the prompt.
const MAX_PARKED_ENDINGS: usize = 64;
/// How long the end of the connection waits for finished turns' commits.
const EXIT_COMMIT_WAIT: Duration = Duration::from_secs(10);
/// The conversation's name on the server: the folder's, cut to this.
const MAX_CONVERSATION_NAME: usize = 80;

/// Which project and agent a new session runs: from the environment the
/// editor starts the agent with (`ELITEA_PROJECT_ID`, `ELITEA_AGENT_ID`,
/// `ELITEA_AGENT_VERSION_ID`). A folder already bound to a project in the
/// app keeps its binding.
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
pub struct Defaults {
    pub project_id: Option<i64>,
    pub application_id: Option<i64>,
    /// `None`: the agent's default version, asked of the platform.
    pub version_id: Option<i64>,
}

impl Defaults {
    /// From the environment (`get` reads one variable).
    ///
    /// # Errors
    ///
    /// A variable that is set but is not an id (1 to 2147483647).
    pub fn from_env(get: impl Fn(&str) -> Option<String>) -> Result<Self, String> {
        let id = |name: &str| -> Result<Option<i64>, String> {
            let Some(raw) = get(name).filter(|raw| !raw.trim().is_empty()) else {
                return Ok(None);
            };
            raw.trim()
                .parse::<i64>()
                .ok()
                .filter(|id| (1..=i64::from(i32::MAX)).contains(id))
                .map(Some)
                .ok_or_else(|| format!("{name} must be an id (1 to 2147483647)"))
        };
        Ok(Self {
            project_id: id("ELITEA_PROJECT_ID")?,
            application_id: id("ELITEA_AGENT_ID")?,
            version_id: id("ELITEA_AGENT_VERSION_ID")?,
        })
    }
}

/// What the agent runs on.
pub struct AcpConfig {
    pub agents: Arc<AgentHost>,
    pub workspaces: Arc<WorkspaceStore>,
    /// The platform calls a session's setup makes (the conversation and its
    /// participants), on the stored sign-in.
    pub api: Arc<PlatformApi>,
    pub router: Arc<EventRouter>,
    pub defaults: Defaults,
    /// The app's own directories: no session opens a folder inside one.
    pub app_dirs: Vec<PathBuf>,
    pub client_version: String,
}

// ---- the event router ----------------------------------------------------

/// The agent host's event sink in ACP mode: each turn's events go to the
/// prompt that started it. Events of a turn nobody routed yet are parked
/// (bounded) until its prompt asks for them.
#[derive(Default)]
pub struct EventRouter {
    state: Mutex<RouterState>,
}

#[derive(Default)]
struct RouterState {
    routes: HashMap<String, mpsc::UnboundedSender<AgentEvent>>,
    parked: HashMap<String, Parked>,
    order: VecDeque<String>,
}

/// One unrouted turn's events, and whether any were dropped.
#[derive(Default)]
struct Parked {
    events: Vec<AgentEvent>,
    dropped: bool,
}

/// The events that settle a prompt (`Mapper` reads its end from them).
fn is_ending(kind: &str) -> bool {
    matches!(kind, "status" | "error" | "done")
}

impl EventRouter {
    fn state(&self) -> std::sync::MutexGuard<'_, RouterState> {
        self.state
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
    }

    /// The events of `turn_id`, those parked so far first.
    #[cfg(test)]
    pub(super) fn route(&self, turn_id: &str) -> mpsc::UnboundedReceiver<AgentEvent> {
        self.route_checked(turn_id).0
    }

    /// As [`Self::route`], and whether the parked events overflowed (some
    /// of the turn's output was dropped: the prompt cannot report it whole).
    pub(super) fn route_checked(
        &self,
        turn_id: &str,
    ) -> (mpsc::UnboundedReceiver<AgentEvent>, bool) {
        let (sender, receiver) = mpsc::unbounded_channel();
        let mut state = self.state();
        let mut dropped = false;
        if let Some(parked) = state.parked.remove(turn_id) {
            dropped = parked.dropped;
            for event in parked.events {
                let _ = sender.send(event);
            }
        }
        state.order.retain(|id| id != turn_id);
        state.routes.insert(turn_id.to_owned(), sender);
        (receiver, dropped)
    }

    fn unroute(&self, turn_id: &str) {
        self.state().routes.remove(turn_id);
    }
}

impl EventEmitter for EventRouter {
    fn emit(&self, event: AgentEvent) {
        let mut state = self.state();
        if let Some(route) = state.routes.get(&event.turn_id) {
            let _ = route.send(event);
            return;
        }
        let turn_id = event.turn_id.clone();
        if !state.parked.contains_key(&turn_id) {
            if state.order.len() >= MAX_PARKED_TURNS
                && let Some(oldest) = state.order.pop_front()
            {
                state.parked.remove(&oldest);
            }
            state.order.push_back(turn_id.clone());
        }
        let parked = state.parked.entry(turn_id).or_default();
        let room = if is_ending(event.kind) {
            MAX_PARKED_EVENTS + MAX_PARKED_ENDINGS
        } else {
            MAX_PARKED_EVENTS
        };
        if parked.events.len() < room {
            parked.events.push(event);
        } else {
            parked.dropped = true;
        }
    }
}

// ---- sessions ------------------------------------------------------------

/// One ACP session: a workspace and a server conversation.
struct Session {
    /// The conversation UUID (the ACP session id).
    id: String,
    workspace_id: String,
    /// The workspace folder (canonical) and the `cwd` as the client gave it:
    /// a file link is matched against either.
    root: PathBuf,
    cwd: PathBuf,
    project_id: i64,
    application_id: i64,
    version_id: i64,
    plan: AtomicBool,
    /// A prompt runs (one at a time per session).
    running: AtomicBool,
    turn: Mutex<TurnSlot>,
    /// The custom commands offered in the last `available_commands_update`.
    commands: Mutex<HashSet<String>>,
}

#[derive(Default)]
struct TurnSlot {
    turn_id: Option<String>,
    /// A cancel that arrived before the turn had an id.
    cancel: bool,
    /// The prompts that arrived while the turn ran, oldest first.
    waiting: Vec<Waiter>,
}

/// A `session/prompt` that arrived while a turn of its session ran: a
/// steering message of that turn. Its request waits for `reply`.
struct Waiter {
    text: String,
    /// Its id in the running turn's steering queue. `None` while no turn
    /// has it in its queue (no turn id yet, or the turn ended first).
    steer_id: Option<String>,
    /// The turn took it (`steer_injected`): it is answered with the turn.
    injected: bool,
    /// A `session/cancel` arrived while it waited.
    cancelled: bool,
    reply: oneshot::Sender<Reply>,
}

/// What a waiting prompt gets when the turn ends.
enum Reply {
    /// Its answer: the turn's, or `cancelled`.
    Answer(Result<Value, RpcError>),
    /// The turn did not take it: it runs as the next turn. Its request now
    /// holds the session's `running` flag.
    RunNext,
}

impl Session {
    fn slot(&self) -> std::sync::MutexGuard<'_, TurnSlot> {
        self.turn
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
    }

    fn mode(&self) -> &'static str {
        if self.plan.load(Ordering::SeqCst) {
            MODE_PLAN
        } else {
            MODE_DEFAULT
        }
    }

    fn modes(&self) -> SessionModeState {
        SessionModeState {
            current_mode_id: self.mode().to_owned(),
            available_modes: vec![
                SessionMode {
                    id: MODE_DEFAULT.to_owned(),
                    name: "Default".to_owned(),
                    description: Some("Every tool; asks before what is destructive".to_owned()),
                },
                SessionMode {
                    id: MODE_PLAN.to_owned(),
                    name: "Plan".to_owned(),
                    description: Some(
                        "Read-only tools; proposes a plan before changing anything".to_owned(),
                    ),
                },
            ],
        }
    }
}

/// Holds a session's `running` flag for one prompt turn. [`Self::settle`]
/// answers the prompts that waited and gives the flag to the oldest one the
/// turn did not take. If the prompt stops without `settle` (a panic), the
/// drop releases the flag; each waiting prompt then gets an error answer.
struct Running<'a> {
    session: &'a Session,
    settled: bool,
}

impl<'a> Running<'a> {
    fn new(session: &'a Session) -> Self {
        Self {
            session,
            settled: false,
        }
    }

    /// The turn ended with `result`. A prompt the turn took gets the same
    /// result. A prompt that waited when `session/cancel` arrived gets
    /// `cancelled`. The oldest other prompt runs as the next turn, and
    /// the prompts after it wait for that turn.
    fn settle(&mut self, result: &Result<Value, RpcError>) {
        self.settled = true;
        let cancelled = to_value(&PromptResponse {
            stop_reason: StopReason::Cancelled,
        });
        let mut slot = self.session.slot();
        let mut next = Vec::new();
        for waiter in std::mem::take(&mut slot.waiting) {
            if waiter.injected {
                let _ = waiter.reply.send(Reply::Answer(result.clone()));
            } else if waiter.cancelled {
                let _ = waiter.reply.send(Reply::Answer(cancelled.clone()));
            } else {
                next.push(waiter);
            }
        }
        slot.turn_id = None;
        slot.cancel = false;
        let mut next = next.into_iter();
        while let Some(first) = next.next() {
            if first.reply.send(Reply::RunNext).is_ok() {
                // The flag stays set: the next turn's request has it now.
                slot.waiting = next
                    .map(|mut waiter| {
                        waiter.steer_id = None;
                        waiter
                    })
                    .collect();
                return;
            }
        }
        self.session.running.store(false, Ordering::SeqCst);
    }
}

impl Drop for Running<'_> {
    fn drop(&mut self) {
        if self.settled {
            return;
        }
        let mut slot = self.session.slot();
        *slot = TurnSlot::default();
        self.session.running.store(false, Ordering::SeqCst);
    }
}

// ---- the connection ------------------------------------------------------

struct Connection {
    config: AcpConfig,
    out: mpsc::UnboundedSender<Value>,
    next_id: AtomicI64,
    pending: Mutex<HashMap<Id, oneshot::Sender<Result<Value, RpcError>>>>,
    sessions: Mutex<HashMap<String, Arc<Session>>>,
    initialized: AtomicBool,
}

/// Serve one ACP client on `reader` / `writer` until its input ends; then
/// stop the turns still running and wait (bounded) for finished turns'
/// commits.
pub async fn serve<R, W>(config: AcpConfig, reader: R, writer: W)
where
    R: AsyncRead + Unpin + Send + 'static,
    W: AsyncWrite + Unpin + Send + 'static,
{
    let (out, mut outgoing) = mpsc::unbounded_channel::<Value>();
    let writer_task = tokio::spawn(async move {
        let mut writer = writer;
        while let Some(message) = outgoing.recv().await {
            if let Err(error) = rpc::write_message(&mut writer, &message).await {
                log::warn!("acp: the client's input closed: {error}");
                break;
            }
        }
    });
    let connection = Arc::new(Connection {
        config,
        out,
        next_id: AtomicI64::new(1),
        pending: Mutex::new(HashMap::new()),
        sessions: Mutex::new(HashMap::new()),
        initialized: AtomicBool::new(false),
    });
    let mut reader = BufReader::new(reader);
    loop {
        let line = match rpc::read_line(&mut reader, rpc::MAX_LINE_BYTES).await {
            Ok(Line::Message(line)) => line,
            Ok(Line::TooLong) => {
                log::warn!("acp: refused a message over {} bytes", rpc::MAX_LINE_BYTES);
                connection.send(rpc::response(
                    None,
                    Err(RpcError::new(rpc::PARSE_ERROR, "the message is too large")),
                ));
                continue;
            }
            Ok(Line::Eof) => break,
            Err(error) => {
                log::warn!("acp: reading the client failed: {error}");
                break;
            }
        };
        if line.iter().all(u8::is_ascii_whitespace) {
            continue;
        }
        match rpc::parse(&line) {
            Ok(Incoming::Request { id, method, params }) => {
                let connection = connection.clone();
                tokio::spawn(async move { connection.handle_request(id, method, params).await });
            }
            Ok(Incoming::Notification { method, params }) => {
                connection.handle_notification(&method, params);
            }
            Ok(Incoming::Response { id, result }) => connection.answered(&id, result),
            Err(refused) => {
                connection.send(rpc::response(refused.id.as_ref(), Err(refused.error)));
            }
        }
    }
    connection.shutdown().await;
    drop(connection);
    let _ = writer_task.await;
}

impl Connection {
    fn send(&self, message: Value) {
        let _ = self.out.send(message);
    }

    fn notify(&self, session_id: &str, update: SessionUpdate) {
        self.send(rpc::notification(
            "session/update",
            &SessionNotification {
                session_id: session_id.to_owned(),
                update,
            },
        ));
    }

    /// Send a request to the client and wait for its answer (an error when
    /// the connection ends first).
    async fn request(&self, method: &str, params: &impl Serialize) -> Result<Value, RpcError> {
        let id = Id::Number(self.next_id.fetch_add(1, Ordering::SeqCst));
        let (sender, receiver) = oneshot::channel();
        self.pending
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
            .insert(id.clone(), sender);
        self.send(rpc::request(&id, method, params));
        receiver
            .await
            .unwrap_or_else(|_| Err(RpcError::new(rpc::INTERNAL_ERROR, "the connection ended")))
    }

    fn answered(&self, id: &Id, result: Result<Value, RpcError>) {
        let waiting = self
            .pending
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
            .remove(id);
        match waiting {
            Some(sender) => {
                let _ = sender.send(result);
            }
            None => log::debug!("acp: an answer to no open request"),
        }
    }

    fn session(&self, id: &str) -> Result<Arc<Session>, RpcError> {
        self.sessions
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
            .get(id)
            .cloned()
            .ok_or_else(|| RpcError::invalid_params("unknown session"))
    }

    async fn handle_request(self: Arc<Self>, id: Id, method: String, params: Value) {
        if method != "initialize" && !self.initialized.load(Ordering::SeqCst) {
            self.send(rpc::response(
                Some(&id),
                Err(RpcError::new(rpc::INVALID_REQUEST, "initialize first")),
            ));
            return;
        }
        let mut opened = None;
        let result = match method.as_str() {
            "initialize" => self.initialize(params),
            // No method is advertised: the sign-in is the app's.
            "authenticate" => parse::<AuthenticateRequest>(params).and_then(|_| {
                Err(RpcError::invalid_params(
                    "this agent uses the Elitea app's sign-in and offers no other method",
                ))
            }),
            "session/new" => self.new_session(params).await.map(|(answer, session)| {
                opened = Some(session);
                answer
            }),
            "session/load" => self.load_session(params).await.map(|(answer, session)| {
                opened = Some(session);
                answer
            }),
            "session/prompt" => self.prompt(params).await,
            "session/set_mode" => self.set_mode(params),
            _ => Err(RpcError::new(rpc::METHOD_NOT_FOUND, "method not found")),
        };
        if let Err(error) = &result {
            log::info!("acp: {method} refused ({})", error.code);
        }
        self.send(rpc::response(Some(&id), result));
        // After the answer, so the client knows the session it is about.
        if let Some(session) = opened {
            self.offer_commands(&session).await;
        }
    }

    fn handle_notification(&self, method: &str, params: Value) {
        match method {
            "session/cancel" => match parse::<CancelNotification>(params) {
                Ok(cancel) => {
                    if let Ok(session) = self.session(&cancel.session_id) {
                        self.cancel(&session);
                    }
                }
                Err(_) => log::info!("acp: a malformed session/cancel was ignored"),
            },
            // `$/cancel_request` and anything newer: nothing to do.
            _ => log::debug!("acp: notification {method} ignored"),
        }
    }

    fn cancel(&self, session: &Session) {
        let mut slot = session.slot();
        // The prompts that wait now are answered `cancelled`, unless the
        // turn took one: that one gets the turn's answer.
        for waiter in &mut slot.waiting {
            waiter.cancelled = true;
        }
        match slot.turn_id.clone() {
            Some(turn_id) => {
                if let Err(error) = self.config.agents.cancel(&turn_id) {
                    log::info!("acp: cancel not applied ({})", error.code);
                }
            }
            None if session.running.load(Ordering::SeqCst) => slot.cancel = true,
            None => {}
        }
    }

    /// The input ended: stop what runs, then let finished turns' commits land.
    async fn shutdown(&self) {
        let sessions: Vec<Arc<Session>> = self
            .sessions
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
            .values()
            .cloned()
            .collect();
        for session in &sessions {
            self.cancel(session);
        }
        // Questions still waiting for the client are answered as denied.
        self.pending
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
            .clear();
        let agents = self.config.agents.clone();
        if tokio::time::timeout(EXIT_COMMIT_WAIT, agents.commits_idle())
            .await
            .is_err()
        {
            log::warn!(
                "acp: stopped waiting for local turn commits after {}s",
                EXIT_COMMIT_WAIT.as_secs()
            );
        }
    }

    // ---- initialize -------------------------------------------------------

    fn initialize(&self, params: Value) -> Result<Value, RpcError> {
        let request: InitializeRequest = parse(params)?;
        if let Some(client) = &request.client_info {
            log::info!(
                "acp: client {} {} (protocol {})",
                clip(&client.name, 64),
                clip(&client.version, 32),
                request.protocol_version
            );
        }
        self.initialized.store(true, Ordering::SeqCst);
        to_value(&InitializeResponse {
            // The only version this agent speaks; a client that cannot speak
            // it closes the connection.
            protocol_version: PROTOCOL_VERSION,
            agent_capabilities: AgentCapabilities {
                load_session: true,
                prompt_capabilities: PromptCapabilities {
                    image: true,
                    audio: false,
                    // A file the person references is passed as a link and
                    // read by the agent's own confined tools, never inlined.
                    embedded_context: false,
                },
                // Client-supplied MCP servers are never started.
                mcp_capabilities: McpCapabilities::default(),
            },
            auth_methods: Vec::new(),
            agent_info: Some(Implementation {
                name: "elitea".to_owned(),
                title: Some("Elitea".to_owned()),
                version: self.config.client_version.clone(),
            }),
        })
    }

    // ---- session/new, session/load -----------------------------------------

    async fn new_session(&self, params: Value) -> Result<(Value, Arc<Session>), RpcError> {
        let request: NewSessionRequest = parse(params)?;
        ignore_client_servers(
            &request.mcp_servers,
            request.additional_directories.as_ref(),
        );
        let opened = self.open_folder(&request.cwd)?;
        let target = self.target(&opened).await?;
        let api = &self.config.api;
        let user_id = api.current_user_id().await.map_err(api_refusal)?;
        let folder_name = clip(&opened.name, MAX_CONVERSATION_NAME);
        let created = api
            .send(
                Method::POST,
                &format!(
                    "/api/v2/elitea_core/conversations/prompt_lib/{}",
                    target.project_id
                ),
                Some(&json!({
                    "name": folder_name,
                    "is_private": true,
                    "source": "local_work",
                    // The folder's name, never its path.
                    "meta": { "local_work": { "folder_name": folder_name } },
                })),
                &[],
            )
            .await
            .map_err(api_refusal)?;
        if !(200..300).contains(&created.status) {
            return Err(answer_refusal(created.status, &created.body));
        }
        let uuid = created
            .body
            .get("uuid")
            .and_then(Value::as_str)
            .and_then(canonical_uuid)
            .ok_or_else(|| {
                RpcError::refused(
                    "conversation_without_uuid",
                    "the platform did not return the conversation's UUID",
                )
            })?;
        let added = api
            .send(
                Method::POST,
                &format!(
                    "/api/v2/elitea_core/participants/prompt_lib/{}/{uuid}",
                    target.project_id
                ),
                Some(&json!([
                    { "entity_name": "user", "entity_meta": { "id": user_id } },
                    {
                        "entity_name": "application",
                        "entity_meta": {
                            "id": target.application_id,
                            "name": target.application_name,
                            "project_id": target.project_id,
                        },
                        "entity_settings": {
                            "version_id": target.version_id,
                            "agent_type": target.agent_type,
                            "variables": [],
                            "icon_meta": {},
                        },
                    },
                ])),
                &[],
            )
            .await
            .map_err(api_refusal)?;
        if !(200..300).contains(&added.status) {
            return Err(answer_refusal(added.status, &added.body));
        }
        let session = self.register(uuid, opened, &target);
        log::info!("acp: session opened in a new conversation");
        let answer = to_value(&NewSessionResponse {
            session_id: session.id.clone(),
            modes: Some(session.modes()),
        })?;
        Ok((answer, session))
    }

    async fn load_session(&self, params: Value) -> Result<(Value, Arc<Session>), RpcError> {
        let request: LoadSessionRequest = parse(params)?;
        ignore_client_servers(
            &request.mcp_servers,
            request.additional_directories.as_ref(),
        );
        let uuid = canonical_uuid(&request.session_id)
            .ok_or_else(|| RpcError::invalid_params("the session id is a conversation UUID"))?;
        if let Ok(existing) = self.session(&uuid)
            && existing.running.load(Ordering::SeqCst)
        {
            return Err(RpcError::refused("busy", "That session is running a turn."));
        }
        let opened = self.open_folder(&request.cwd)?;
        let target = self.target(&opened).await?;
        // The conversation must be readable and hold the agent.
        self.config
            .api
            .answering_participant(
                target.project_id,
                &uuid,
                target.application_id,
                target.version_id,
            )
            .await
            .map_err(api_refusal)?;
        let session = self.register(uuid, opened, &target);
        self.replay(&session).await;
        log::info!("acp: session reopened");
        let answer = to_value(&LoadSessionResponse {
            modes: Some(session.modes()),
        })?;
        Ok((answer, session))
    }

    /// The thread this computer ran in that conversation, from its local
    /// history, as `session/update`s (the server keeps the transcript; a
    /// conversation whose turns ran elsewhere replays nothing).
    async fn replay(&self, session: &Session) {
        let turns = match self
            .config
            .agents
            .thread_history(&session.workspace_id, &session.id)
            .await
        {
            Ok(turns) => turns,
            Err(error) => {
                log::info!("acp: no history replayed ({})", error.code);
                return;
            }
        };
        for turn in turns {
            self.notify(
                &session.id,
                SessionUpdate::UserMessageChunk(ContentChunk::text(turn.prompt)),
            );
            let mut mapper = Mapper::default();
            for event in turn.events {
                for update in mapper.map(&event.kind, &event.payload) {
                    self.notify(&session.id, update);
                }
            }
        }
    }

    fn register(&self, id: String, opened: OpenedFolder, target: &Target) -> Arc<Session> {
        let session = Arc::new(Session {
            id: id.clone(),
            workspace_id: opened.workspace_id,
            root: opened.root,
            cwd: opened.cwd,
            project_id: target.project_id,
            application_id: target.application_id,
            version_id: target.version_id,
            plan: AtomicBool::new(false),
            running: AtomicBool::new(false),
            turn: Mutex::new(TurnSlot::default()),
            commands: Mutex::new(HashSet::new()),
        });
        self.sessions
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
            .insert(id, session.clone());
        session
    }

    /// Open `cwd` as a workspace, as the app's folder picker does.
    fn open_folder(&self, cwd: &str) -> Result<OpenedFolder, RpcError> {
        if cwd.is_empty() || cwd.len() > MAX_PATH_BYTES || cwd.contains('\0') {
            return Err(RpcError::invalid_params("cwd is not a usable path"));
        }
        let given = PathBuf::from(cwd);
        if !given.is_absolute() {
            return Err(RpcError::invalid_params("cwd must be an absolute path"));
        }
        let canonical = given
            .canonicalize()
            .map_err(|_| RpcError::invalid_params("cwd is not an existing folder"))?;
        if !canonical.is_dir() {
            return Err(RpcError::invalid_params("cwd is not a folder"));
        }
        if canonical.parent().is_none() {
            return Err(RpcError::refused(
                "workspace_refused",
                "The filesystem root cannot be a workspace.",
            ));
        }
        let inside_app_data = self.config.app_dirs.iter().any(|dir| {
            canonical.starts_with(dir)
                || dir
                    .canonicalize()
                    .is_ok_and(|resolved| canonical.starts_with(resolved))
        });
        if inside_app_data {
            return Err(RpcError::refused(
                "workspace_refused",
                "The app's own data cannot be a workspace.",
            ));
        }
        let workspace = self
            .config
            .workspaces
            .add(&canonical)
            .map_err(|error| RpcError::refused("storage", error.to_string()))?;
        Ok(OpenedFolder {
            workspace_id: workspace.id,
            name: workspace.name,
            project_id: workspace.project_id,
            root: PathBuf::from(workspace.path),
            cwd: given,
        })
    }

    /// The project and agent version a session of `opened` runs, binding
    /// the folder to the configured project when it has none yet.
    async fn target(&self, opened: &OpenedFolder) -> Result<Target, RpcError> {
        let defaults = self.config.defaults;
        let project_id = match opened.project_id {
            Some(project_id) => project_id,
            None => {
                let project_id = defaults.project_id.ok_or_else(|| {
                    RpcError::refused(
                        "workspace_unbound",
                        "Bind this folder to a project in the Elitea app, or set ELITEA_PROJECT_ID.",
                    )
                })?;
                self.config
                    .agents
                    .bind_project(&opened.workspace_id, project_id)
                    .map_err(turn_refusal)?;
                project_id
            }
        };
        let application_id = defaults.application_id.ok_or_else(|| {
            RpcError::refused(
                "agent_unset",
                "Set ELITEA_AGENT_ID (and optionally ELITEA_AGENT_VERSION_ID) to the agent to run.",
            )
        })?;
        let api = &self.config.api;
        let version_id = match defaults.version_id {
            Some(version_id) => version_id,
            None => {
                let answer = api
                    .send(
                        Method::GET,
                        &format!(
                            "/api/v2/elitea_core/default_version/prompt_lib/{project_id}/{application_id}"
                        ),
                        None,
                        &[],
                    )
                    .await
                    .map_err(api_refusal)?;
                if !(200..300).contains(&answer.status) {
                    return Err(answer_refusal(answer.status, &answer.body));
                }
                answer
                    .body
                    .get("id")
                    .and_then(Value::as_i64)
                    .filter(|id| *id > 0)
                    .ok_or_else(|| {
                        RpcError::refused("agent_version_unknown", "The agent has no version.")
                    })?
            }
        };
        let application = api
            .send(
                Method::GET,
                &format!(
                    "/api/v2/elitea_core/application/prompt_lib/{project_id}/{application_id}"
                ),
                None,
                &[],
            )
            .await
            .map_err(api_refusal)?;
        if !(200..300).contains(&application.status) {
            return Err(answer_refusal(application.status, &application.body));
        }
        let application_name = application
            .body
            .get("name")
            .and_then(Value::as_str)
            .map_or_else(|| "Agent".to_owned(), |name| clip(name, 256));
        let agent_type = application
            .body
            .get("versions")
            .and_then(Value::as_array)
            .and_then(|versions| {
                versions
                    .iter()
                    .find(|v| v.get("id").and_then(Value::as_i64) == Some(version_id))
            })
            .and_then(|version| version.get("agent_type"))
            .and_then(Value::as_str)
            .map_or_else(|| "openai".to_owned(), |kind| clip(kind, 64));
        Ok(Target {
            project_id,
            application_id,
            version_id,
            application_name,
            agent_type,
        })
    }

    /// `available_commands_update`: `/compact` and the workspace's custom
    /// commands, as the composer's "/" menu offers them.
    async fn offer_commands(&self, session: &Session) {
        let agents = self.config.agents.clone();
        let workspace_id = session.workspace_id.clone();
        let listed = tokio::task::spawn_blocking(move || agents.commands_list(&workspace_id))
            .await
            .ok()
            .and_then(Result::ok)
            .unwrap_or_default();
        let mut commands = vec![AvailableCommand {
            name: "compact".to_owned(),
            description: "Summarize the conversation's context now".to_owned(),
            input: None,
        }];
        let mut names = HashSet::new();
        for command in listed {
            if command.name == "compact" {
                continue;
            }
            names.insert(command.name.clone());
            commands.push(AvailableCommand {
                description: command
                    .description
                    .map_or_else(|| "Custom command".to_owned(), |d| clip(&d, 512)),
                input: command.argument_hint.map(|hint| AvailableCommandInput {
                    hint: clip(&hint, 256),
                }),
                name: command.name,
            });
        }
        *session
            .commands
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner) = names;
        self.notify(
            &session.id,
            SessionUpdate::AvailableCommandsUpdate(AvailableCommandsUpdate {
                available_commands: commands,
            }),
        );
    }

    // ---- session/set_mode ---------------------------------------------------

    fn set_mode(&self, params: Value) -> Result<Value, RpcError> {
        let request: SetSessionModeRequest = parse(params)?;
        let session = self.session(&request.session_id)?;
        let plan = match request.mode_id.as_str() {
            MODE_DEFAULT => false,
            MODE_PLAN => true,
            _ => return Err(RpcError::invalid_params("unknown mode")),
        };
        // A running turn keeps the profile it started in; the next prompt
        // takes this one.
        session.plan.store(plan, Ordering::SeqCst);
        Ok(json!({}))
    }

    // ---- session/prompt -----------------------------------------------------

    async fn prompt(self: &Arc<Self>, params: Value) -> Result<Value, RpcError> {
        let request: PromptRequest = parse(params)?;
        let session = self.session(&request.session_id)?;
        let blocks = request.prompt;
        loop {
            if session
                .running
                .compare_exchange(false, true, Ordering::SeqCst, Ordering::SeqCst)
                .is_ok()
            {
                break;
            }
            // A turn of the session runs: this prompt steers it.
            let text = steering_text(&session, &blocks)?;
            let reply = {
                let mut slot = session.slot();
                // The turn ended and released the session before the lock:
                // try to run this prompt as a turn.
                if !session.running.load(Ordering::SeqCst) {
                    continue;
                }
                self.queue_waiter(&mut slot, text)?
            };
            match reply.await {
                Ok(Reply::Answer(result)) => return result,
                Ok(Reply::RunNext) => break,
                Err(_) => {
                    return Err(RpcError::refused(
                        "turn_ended",
                        "The turn stopped before it answered this prompt.",
                    ));
                }
            }
        }
        let mut running = Running::new(&session);
        let result = self.run_prompt(&session, blocks).await;
        running.settle(&result);
        result
    }

    /// Queue a prompt that arrived while a turn runs. With a turn id, its
    /// text goes into the turn's steering queue now; else the turn takes
    /// it when it gets its id ([`Self::steer_waiting`]).
    fn queue_waiter(
        &self,
        slot: &mut TurnSlot,
        text: String,
    ) -> Result<oneshot::Receiver<Reply>, RpcError> {
        let waiting = slot.waiting.iter().filter(|w| !w.injected).count();
        if waiting >= steering::MAX_PENDING {
            return Err(busy(format!(
                "This session is running a prompt, and {} prompts wait for it already.",
                steering::MAX_PENDING
            )));
        }
        let (sender, receiver) = oneshot::channel();
        let mut waiter = Waiter {
            text,
            steer_id: None,
            injected: false,
            cancelled: false,
            reply: sender,
        };
        if let Some(turn_id) = slot.turn_id.clone() {
            match self.config.agents.steer(&turn_id, &waiter.text) {
                Ok(steer_id) => waiter.steer_id = Some(steer_id),
                Err(error) if error.code == "steer_queue_full" => {
                    return Err(busy(error.message));
                }
                Err(error) if error.code == "steer_invalid" => {
                    return Err(RpcError::invalid_params(error.message));
                }
                // The turn is ending, or it has the most steering messages
                // that one turn takes (`steer_turn_full`): the prompt runs
                // as the next turn.
                Err(error) => log::debug!("acp: a prompt waits for the next turn ({})", error.code),
            }
        }
        slot.waiting.push(waiter);
        Ok(receiver)
    }

    /// The turn `turn_id` now runs: queue each waiting prompt that no turn
    /// has, oldest first.
    fn steer_waiting(&self, slot: &mut TurnSlot, turn_id: &str) {
        for waiter in &mut slot.waiting {
            if waiter.steer_id.is_some() || waiter.injected || waiter.cancelled {
                continue;
            }
            match self.config.agents.steer(turn_id, &waiter.text) {
                Ok(steer_id) => waiter.steer_id = Some(steer_id),
                Err(error) => log::info!("acp: a prompt was not steered ({})", error.code),
            }
        }
    }

    /// `steer_injected`: the turn took a waiting prompt; it is answered
    /// with the turn. `steer_dropped`: the turn ended first; the prompt
    /// runs as the next turn.
    fn steered(session: &Session, kind: &str, payload: &Value) {
        let Some(steer_id) = payload.get("steer_id").and_then(Value::as_str) else {
            return;
        };
        let mut slot = session.slot();
        if let Some(waiter) = slot
            .waiting
            .iter_mut()
            .find(|waiter| waiter.steer_id.as_deref() == Some(steer_id))
        {
            if kind == "steer_injected" {
                waiter.injected = true;
            } else {
                waiter.steer_id = None;
            }
        }
    }

    /// One prompt turn of `session`, whose `running` flag the caller holds.
    async fn run_prompt(
        self: &Arc<Self>,
        session: &Arc<Session>,
        blocks: Vec<ContentBlock>,
    ) -> Result<Value, RpcError> {
        let input = self.prompt_input(session, blocks).await?;
        let tokens: Vec<String> = input.images.iter().map(|pick| pick.token.clone()).collect();
        let started = self
            .config
            .agents
            .start(TurnRequest {
                workspace_id: session.workspace_id.clone(),
                project_id: session.project_id,
                conversation_id: Value::String(session.id.clone()),
                application_id: session.application_id,
                version_id: session.version_id,
                prompt: input.text,
                plan_mode: session.plan.load(Ordering::SeqCst),
                bypass: false,
                mentions: input.mentions,
                skills: Vec::new(),
                compact: input.compact,
                model: None,
                project_skills: Vec::new(),
                command: input.command,
                images: input.images,
            })
            .await;
        let started = match started {
            Ok(started) => started,
            Err(error) => {
                for token in &tokens {
                    self.config
                        .agents
                        .attachment_remove(&session.workspace_id, token);
                }
                return Err(turn_refusal(error));
            }
        };
        let turn_id = started.turn_id;
        let (mut events, dropped) = self.config.router.route_checked(&turn_id);
        {
            let mut slot = session.slot();
            slot.turn_id = Some(turn_id.clone());
            // Output already lost cannot be reported: stop the turn, and
            // answer the prompt with an error once it settles.
            if slot.cancel || dropped {
                let _ = self.config.agents.cancel(&turn_id);
            } else {
                self.steer_waiting(&mut slot, &turn_id);
            }
        }
        let mut mapper = Mapper::default();
        while let Some(event) = events.recv().await {
            match event.kind {
                "approval_request" => self.ask(session, &mapper, &event.payload),
                "plan_proposed" => self.propose(session, &event.payload),
                "steer_injected" | "steer_dropped" => {
                    Self::steered(session, event.kind, &event.payload);
                }
                "plan_accepted" => {
                    session.plan.store(false, Ordering::SeqCst);
                    self.notify(
                        &session.id,
                        SessionUpdate::CurrentModeUpdate(CurrentModeUpdate {
                            current_mode_id: MODE_DEFAULT.to_owned(),
                        }),
                    );
                }
                kind => {
                    for update in mapper.map(kind, &event.payload) {
                        self.notify(&session.id, update);
                    }
                }
            }
            if event.kind == "done" {
                break;
            }
        }
        self.config.router.unroute(&turn_id);
        if dropped {
            return Err(RpcError::refused(
                "turn_output_dropped",
                "The turn produced more output than could be relayed before the prompt began; it was stopped."
                    .to_owned(),
            ));
        }
        match mapper.end {
            End::Cancelled => to_value(&PromptResponse {
                stop_reason: StopReason::Cancelled,
            }),
            End::Failed { code, message } => Err(RpcError::refused(&code, message)),
            End::StepLimit => to_value(&PromptResponse {
                stop_reason: StopReason::MaxTurnRequests,
            }),
            End::Done => to_value(&PromptResponse {
                stop_reason: StopReason::EndTurn,
            }),
        }
    }

    /// The prompt's blocks as a turn's text, mentions, images and command.
    async fn prompt_input(
        &self,
        session: &Session,
        blocks: Vec<ContentBlock>,
    ) -> Result<PromptInput, RpcError> {
        if blocks.is_empty() || blocks.len() > MAX_PROMPT_BLOCKS {
            return Err(RpcError::invalid_params(format!(
                "a prompt has 1 to {MAX_PROMPT_BLOCKS} content blocks"
            )));
        }
        let mut text = String::new();
        let mut mentions: Vec<String> = Vec::new();
        let mut images = Vec::new();
        for block in blocks {
            match block {
                ContentBlock::Text(content) => text.push_str(&content.text),
                ContentBlock::ResourceLink(link) => {
                    reference(&mut text, &mut mentions, session, &link.uri, &link.name);
                }
                // Not offered (`embeddedContext: false`); a client that sends
                // one anyway gets it treated as a link: the agent reads the
                // file itself, confined, and the inlined copy is dropped.
                ContentBlock::Resource(resource) => {
                    let uri = resource.resource.uri().to_owned();
                    let name = uri.rsplit('/').next().unwrap_or_default().to_owned();
                    reference(&mut text, &mut mentions, session, &uri, &name);
                }
                ContentBlock::Image(image) => {
                    if images.len() == MAX_PROMPT_IMAGES {
                        return Err(RpcError::refused(
                            "too_many_images",
                            format!("At most {MAX_PROMPT_IMAGES} images per message."),
                        ));
                    }
                    let agents = self.config.agents.clone();
                    let workspace_id = session.workspace_id.clone();
                    let added = tokio::task::spawn_blocking(move || {
                        agents.attachment_add_bytes(
                            &workspace_id,
                            Some("editor-image"),
                            &image.data,
                        )
                    })
                    .await
                    .map_err(|_| RpcError::new(rpc::INTERNAL_ERROR, "the image was not read"))?
                    .map_err(turn_refusal)?;
                    images.push(ImagePick {
                        source: ImageSource::Clipboard,
                        token: added.token,
                    });
                }
                ContentBlock::Audio(_) => {
                    return Err(RpcError::invalid_params("audio prompts are not supported"));
                }
            }
            if text.len() > MAX_PROMPT_TEXT_BYTES {
                return Err(RpcError::invalid_params("the prompt is too long"));
            }
        }
        if text.trim().is_empty() && images.is_empty() {
            return Err(RpcError::invalid_params("the prompt is empty"));
        }
        let (compact, command) = slash_command(&text, session);
        Ok(PromptInput {
            text,
            mentions,
            images,
            compact,
            command,
        })
    }

    /// An `approval_request` as `session/request_permission`, answered in
    /// the background so the turn's other events keep flowing.
    fn ask(self: &Arc<Self>, session: &Arc<Session>, mapper: &Mapper, payload: &Value) {
        let Some(request_id) = payload.get("request_id").and_then(Value::as_str) else {
            return;
        };
        let text = |key: &str| payload.get(key).and_then(Value::as_str).unwrap_or_default();
        let kind = text("kind");
        let can_remember = payload.get("can_remember") == Some(&Value::Bool(true));
        let (allow, reject) = match kind {
            "continue" => ("Continue", "Stop here"),
            "mcp_authorization" => ("I have signed in", "Cancel"),
            _ => ("Allow once", "Reject"),
        };
        let mut options = vec![PermissionOption {
            option_id: "allow_once".to_owned(),
            name: allow.to_owned(),
            kind: PermissionOptionKind::AllowOnce,
        }];
        // Only where the engine would keep the answer.
        if can_remember {
            options.push(PermissionOption {
                option_id: "allow_always".to_owned(),
                name: "Always allow".to_owned(),
                kind: PermissionOptionKind::AllowAlways,
            });
        }
        options.push(PermissionOption {
            option_id: "reject_once".to_owned(),
            name: reject.to_owned(),
            kind: PermissionOptionKind::RejectOnce,
        });
        let mut detail = text("detail").to_owned();
        for key in ["justification", "reason", "server_url"] {
            if !text(key).is_empty() {
                detail = format!("{detail}\n\n{}", text(key));
            }
        }
        let call_id = payload
            .get("call_id")
            .and_then(Value::as_str)
            .map(str::to_owned)
            .or_else(|| (kind.is_empty()).then(|| mapper.open_call()).flatten())
            .unwrap_or_else(|| format!("question-{request_id}"));
        let tool_call = ToolCallUpdate {
            tool_call_id: call_id,
            title: Some(clip(text("title"), 1024)),
            kind: Some(if kind == "continue" {
                ToolKind::Other
            } else {
                tool_kind(text("tool"), false)
            }),
            status: Some(ToolCallStatus::Pending),
            content: (!detail.trim().is_empty())
                .then(|| vec![ToolCallContent::text(clip(detail.trim(), 8 * 1024))]),
            ..ToolCallUpdate::default()
        };
        self.spawn_question(
            session,
            request_id,
            tool_call,
            options,
            |option| match option {
                Some("allow_once") => UiDecision::AllowOnce,
                Some("allow_always") => UiDecision::AllowAlways,
                _ => UiDecision::Deny,
            },
        );
    }

    /// A `plan_proposed` card: accept or decline the plan.
    fn propose(self: &Arc<Self>, session: &Arc<Session>, payload: &Value) {
        let Some(request_id) = payload.get("request_id").and_then(Value::as_str) else {
            return;
        };
        let plan = payload
            .get("plan")
            .and_then(Value::as_str)
            .unwrap_or_default();
        let call_id = payload
            .get("call_id")
            .and_then(Value::as_str)
            .map_or_else(|| format!("plan-{request_id}"), str::to_owned);
        let tool_call = ToolCallUpdate {
            tool_call_id: call_id,
            title: Some("Ready to leave plan mode".to_owned()),
            kind: Some(ToolKind::SwitchMode),
            status: Some(ToolCallStatus::Pending),
            content: Some(vec![ToolCallContent::text(clip(plan, 16 * 1024))]),
            ..ToolCallUpdate::default()
        };
        let options = vec![
            PermissionOption {
                option_id: "accept".to_owned(),
                name: "Accept the plan".to_owned(),
                kind: PermissionOptionKind::AllowOnce,
            },
            PermissionOption {
                option_id: "decline".to_owned(),
                name: "Keep planning".to_owned(),
                kind: PermissionOptionKind::RejectOnce,
            },
        ];
        self.spawn_question(session, request_id, tool_call, options, |option| {
            if option == Some("accept") {
                UiDecision::Accept
            } else {
                UiDecision::Decline(None)
            }
        });
    }

    /// Ask the client, then answer the broker. An option the client did not
    /// get, a malformed answer, a cancelled one or a closed connection all
    /// answer as `decide(None)`: never an approval.
    fn spawn_question(
        self: &Arc<Self>,
        session: &Arc<Session>,
        request_id: &str,
        tool_call: ToolCallUpdate,
        options: Vec<PermissionOption>,
        decide: fn(Option<&str>) -> UiDecision,
    ) {
        let connection = self.clone();
        let session_id = session.id.clone();
        let request_id = request_id.to_owned();
        tokio::spawn(async move {
            let offered: Vec<String> = options.iter().map(|o| o.option_id.clone()).collect();
            let answer = connection
                .request(
                    "session/request_permission",
                    &RequestPermissionRequest {
                        session_id,
                        tool_call,
                        options,
                    },
                )
                .await;
            let chosen = answer
                .ok()
                .and_then(|value| serde_json::from_value::<RequestPermissionResponse>(value).ok())
                .and_then(|response| match response.outcome {
                    RequestPermissionOutcome::Selected { option_id } => Some(option_id),
                    RequestPermissionOutcome::Cancelled => None,
                })
                .filter(|option| offered.contains(option));
            let decision = decide(chosen.as_deref());
            if !connection.config.agents.respond(&request_id, decision) {
                log::debug!("acp: the question was closed before the answer");
            }
        });
    }
}

// ---- the event mapping ---------------------------------------------------

/// How a turn ended, from its `status` and `done` events.
#[derive(Debug, Default, PartialEq, Eq)]
pub(super) enum End {
    #[default]
    Done,
    Cancelled,
    StepLimit,
    Failed {
        code: String,
        message: String,
    },
}

/// `agent://event`s to `session/update`s, for a live turn and for a
/// replayed one alike.
#[derive(Debug, Default)]
pub(super) struct Mapper {
    /// Tool calls started and not finished, in start order.
    open: Vec<String>,
    pub(super) end: End,
    error: Option<(String, String)>,
}

impl Mapper {
    /// The tool call an approval belongs to: the newest one still open
    /// (calls that ask run one at a time).
    fn open_call(&self) -> Option<String> {
        self.open.last().cloned()
    }

    pub(super) fn map(&mut self, kind: &str, payload: &Value) -> Vec<SessionUpdate> {
        let text = |key: &str| payload.get(key).and_then(Value::as_str).unwrap_or_default();
        match kind {
            "text_delta" if !text("text").is_empty() => vec![SessionUpdate::AgentMessageChunk(
                ContentChunk::text(text("text")),
            )],
            "thinking_delta" if !text("text").is_empty() => {
                vec![SessionUpdate::AgentThoughtChunk(ContentChunk::text(text(
                    "text",
                )))]
            }
            "tool_call" if !text("call_id").is_empty() => {
                let remote = payload.get("remote") == Some(&Value::Bool(true));
                let tool = text("tool");
                let summary = text("args_summary");
                self.open.push(text("call_id").to_owned());
                vec![SessionUpdate::ToolCall(ToolCall {
                    tool_call_id: text("call_id").to_owned(),
                    title: if summary.is_empty() {
                        tool.to_owned()
                    } else {
                        clip(&format!("{tool} {summary}"), 1024)
                    },
                    name: Some(tool.to_owned()),
                    kind: tool_kind(tool, remote),
                    status: ToolCallStatus::InProgress,
                    content: Vec::new(),
                    locations: Vec::new(),
                    raw_input: None,
                    raw_output: None,
                })]
            }
            "tool_result" if !text("call_id").is_empty() => {
                let call_id = text("call_id");
                self.open.retain(|open| open != call_id);
                let ok = payload.get("ok") == Some(&Value::Bool(true));
                let summary = text("summary");
                vec![SessionUpdate::ToolCallUpdate(ToolCallUpdate {
                    tool_call_id: call_id.to_owned(),
                    status: Some(if ok {
                        ToolCallStatus::Completed
                    } else {
                        ToolCallStatus::Failed
                    }),
                    content: (!summary.is_empty())
                        .then(|| vec![ToolCallContent::text(clip(summary, 8 * 1024))]),
                    ..ToolCallUpdate::default()
                })]
            }
            "plan_updated" => vec![SessionUpdate::Plan(plan_of(payload.get("plan")))],
            "error" => {
                self.error = Some((text("code").to_owned(), text("message").to_owned()));
                Vec::new()
            }
            "status" => {
                match text("phase") {
                    "cancelled" => self.end = End::Cancelled,
                    "error" => {
                        let (code, message) = self.error.clone().unwrap_or_else(|| {
                            ("turn_failed".to_owned(), "The turn failed.".to_owned())
                        });
                        self.end = End::Failed { code, message };
                    }
                    "done" if text("note") == "step_limit_reached" => self.end = End::StepLimit,
                    _ => {}
                }
                Vec::new()
            }
            _ => Vec::new(),
        }
    }
}

/// The ACP kind of a tool, by its name (`remote`: a toolkit on the platform).
pub(super) fn tool_kind(tool: &str, remote: bool) -> ToolKind {
    if remote {
        return ToolKind::Fetch;
    }
    match tool {
        "read_file" | "read_document" | "read_output" | "list_tree" | "git_status" | "git_diff"
        | "git_log" | "git_branches" | "process_output" | "process_list" | "get_entity_content"
        | "get_entity_details" => ToolKind::Read,
        "write_file" | "edit_file" | "multi_edit" | "apply_patch" | "git_commit" => ToolKind::Edit,
        "run_command" | "run_tests" | "process_write" | "process_kill" | "process_wait"
        | "git_push" => ToolKind::Execute,
        "search_files"
        | "find_files"
        | "search_workspace"
        | "repo_map"
        | "search_knowledge_graph"
        | "query_graph"
        | "get_related_entities"
        | "list_entity_types"
        | "impact_analysis" => ToolKind::Search,
        "update_plan" | "start_step" | "complete_step" | "get_plan_status" | "delete_plan"
        | "spawn_agent" | "send_input" | "wait_agent" | "close_agent" => ToolKind::Think,
        "exit_plan_mode" => ToolKind::SwitchMode,
        _ => ToolKind::Other,
    }
}

/// `plan_updated`'s plan as ACP plan entries (a cancelled step is left out:
/// ACP has no such status; `null`, a deleted plan, is no entries).
pub(super) fn plan_of(plan: Option<&Value>) -> Plan {
    let steps = plan
        .and_then(|plan| plan.get("steps"))
        .and_then(Value::as_array)
        .cloned()
        .unwrap_or_default();
    Plan {
        entries: steps
            .iter()
            .filter_map(|step| {
                let status = match step.get("status").and_then(Value::as_str)? {
                    "pending" => PlanEntryStatus::Pending,
                    "in_progress" => PlanEntryStatus::InProgress,
                    "completed" => PlanEntryStatus::Completed,
                    _ => return None,
                };
                Some(PlanEntry {
                    content: clip(step.get("step").and_then(Value::as_str)?, 4096),
                    priority: PlanEntryPriority::Medium,
                    status,
                })
            })
            .collect(),
    }
}

// ---- helpers -------------------------------------------------------------

struct OpenedFolder {
    workspace_id: String,
    name: String,
    project_id: Option<i64>,
    root: PathBuf,
    cwd: PathBuf,
}

struct Target {
    project_id: i64,
    application_id: i64,
    version_id: i64,
    application_name: String,
    agent_type: String,
}

struct PromptInput {
    text: String,
    mentions: Vec<String>,
    images: Vec<ImagePick>,
    compact: bool,
    command: Option<CommandPick>,
}

/// A referenced file: inside the workspace it is written `@path` and added
/// to the turn's mentions (checked again by the host); anywhere else only
/// its name is written. Nothing is read here.
fn reference(
    text: &mut String,
    mentions: &mut Vec<String>,
    session: &Session,
    uri: &str,
    name: &str,
) {
    match workspace_relative(uri, &session.root, &session.cwd) {
        Some(path) => {
            text.push('@');
            text.push_str(&path);
            if !mentions.contains(&path) {
                mentions.push(path);
            }
        }
        None => {
            text.push('`');
            text.push_str(&clip(&name.replace(['`', '\n', '\r'], ""), 256));
            text.push('`');
        }
    }
}

/// A `file://` URI as a workspace-relative path (`/`-separated), when it
/// names something inside `root` (or the `cwd` spelling of it).
pub(super) fn workspace_relative(uri: &str, root: &Path, cwd: &Path) -> Option<String> {
    let url = url::Url::parse(uri).ok()?;
    if url.scheme() != "file" {
        return None;
    }
    let path = url.to_file_path().ok()?;
    let relative = path
        .strip_prefix(root)
        .or_else(|_| path.strip_prefix(cwd))
        .ok()?;
    let mut parts = Vec::new();
    for component in relative.components() {
        match component {
            std::path::Component::Normal(part) => parts.push(part.to_str()?.to_owned()),
            std::path::Component::CurDir => {}
            _ => return None,
        }
    }
    (!parts.is_empty()).then(|| parts.join("/"))
}

/// `/compact` and a listed custom command at the prompt's start.
fn slash_command(text: &str, session: &Session) -> (bool, Option<CommandPick>) {
    let Some(rest) = text.trim_start().strip_prefix('/') else {
        return (false, None);
    };
    let name: String = rest.chars().take_while(|c| !c.is_whitespace()).collect();
    let arguments = rest[name.len()..].trim().to_owned();
    if name == "compact" {
        return (true, None);
    }
    let known = session
        .commands
        .lock()
        .unwrap_or_else(std::sync::PoisonError::into_inner)
        .contains(&name);
    if known {
        (false, Some(CommandPick { name, arguments }))
    } else {
        (false, None)
    }
}

/// The text of a prompt that steers a running turn: text blocks only, at
/// most the steering queue's bound, and not a slash command (a command
/// runs as its own turn).
fn steering_text(session: &Session, blocks: &[ContentBlock]) -> Result<String, RpcError> {
    if blocks.is_empty() || blocks.len() > MAX_PROMPT_BLOCKS {
        return Err(RpcError::invalid_params(format!(
            "a prompt has 1 to {MAX_PROMPT_BLOCKS} content blocks"
        )));
    }
    let mut text = String::new();
    for block in blocks {
        let ContentBlock::Text(content) = block else {
            return Err(RpcError::invalid_params(
                "a prompt sent while a turn runs steers it and can only be text; send images, files and audio when the turn ends",
            ));
        };
        text.push_str(&content.text);
        if text.len() > steering::MAX_TEXT_BYTES {
            return Err(busy(format!(
                "This session is running a prompt; a prompt sent now is at most {} KiB.",
                steering::MAX_TEXT_BYTES / 1024
            )));
        }
    }
    if text.trim().is_empty() {
        return Err(RpcError::invalid_params("the prompt is empty"));
    }
    let (compact, command) = slash_command(&text, session);
    if compact || command.is_some() {
        return Err(RpcError::invalid_params(
            "a slash command cannot steer a running turn; send it when the turn ends",
        ));
    }
    Ok(text)
}

/// The refusal of a prompt that the running turn cannot take.
fn busy(message: impl Into<String>) -> RpcError {
    RpcError::refused("busy", message)
}

/// Client-supplied MCP servers and extra folders are never used: noted in
/// the log by count and server name (their commands, URLs, headers and
/// environment never are).
fn ignore_client_servers(servers: &[McpServer], extra: Option<&Vec<String>>) {
    if !servers.is_empty() {
        let names: Vec<&str> = servers.iter().map(McpServer::name).take(8).collect();
        log::info!(
            "acp: the client's {} MCP server(s) are not started ({})",
            servers.len(),
            clip(&names.join(", "), 256)
        );
    }
    if extra.is_some_and(|dirs| !dirs.is_empty()) {
        log::info!("acp: additional directories are not opened");
    }
}

fn parse<T: DeserializeOwned>(params: Value) -> Result<T, RpcError> {
    serde_json::from_value(params)
        .map_err(|error| RpcError::invalid_params(clip(&error.to_string(), 200)))
}

fn to_value(value: &impl Serialize) -> Result<Value, RpcError> {
    serde_json::to_value(value)
        .map_err(|_| RpcError::new(rpc::INTERNAL_ERROR, "the answer could not be encoded"))
}

fn turn_refusal(error: TurnError) -> RpcError {
    refusal(&error.code, error.message)
}

fn api_refusal(error: crate::d0::api::ApiError) -> RpcError {
    refusal(&error.code, error.message)
}

/// A host refusal; a session that ended (the person signed out in the app)
/// is ACP's `auth_required`.
fn refusal(code: &str, message: String) -> RpcError {
    let mut error = RpcError::refused(code, message);
    if code == "not_signed_in" || code == "identity_changed" {
        error.code = rpc::AUTH_REQUIRED;
    }
    error
}

fn answer_refusal(status: u16, body: &Value) -> RpcError {
    let text = |key: &str| body.get(key).and_then(Value::as_str).map(|t| clip(t, 512));
    RpcError::refused(
        &text("error").unwrap_or_else(|| format!("http_{status}")),
        text("message").unwrap_or_else(|| format!("the platform answered HTTP {status}")),
    )
}

fn canonical_uuid(raw: &str) -> Option<String> {
    uuid::Uuid::parse_str(raw)
        .ok()
        .map(|parsed| parsed.hyphenated().to_string())
}

fn clip(text: &str, max: usize) -> String {
    elitea_agent_runtime::text::clip_bytes(text, max).to_owned()
}

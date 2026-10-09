//! The D0 assembler: one local agent turn, end to end.
//!
//! 1. Resolve the agent version (decision 5a) and refuse locally what D0
//!    does not run (withheld secrets, pipelines, nested agents, project MCP).
//! 2. Re-check `local_work.allowed`, find the conversation participant, and
//!    `start` the local turn (decision 5c): execution id, response message
//!    id, deadline and memory recall.
//! 3. Build an adk `LlmAgent` on the patched adk 2.2: instructions plus the
//!    recall (the cloud's splice), frozen skills and project context through
//!    the runtime's instruction authority, the model over `/llm`, the local
//!    tools of the workspace and the agent's toolkits as remote tools.
//! 4. Run it (the first change takes the turn's checkpoint), stream events
//!    to the webview, then `commit` the messages, tool steps, HITL exchanges
//!    and the work report. A cancelled turn commits nothing and its
//!    execution expires.
//!
//! Everything here goes through the runtime's host traits
//! (`ModelTransport`, `ToolProvider`, `ApprovalChannel`, `EventSink`) so
//! extraction stage 7 can replace this module with the runtime's own
//! assembly without touching the hosts' adapters.

use std::collections::HashMap;
use std::path::PathBuf;
use std::sync::{Arc, Mutex};
use std::time::Duration;

use adk_agent::LlmAgentBuilder;
use adk_core::{Agent, Content, Event, Part, RunConfig, StreamingMode, Toolset};
use adk_runner::Runner;
use adk_session::{CreateRequest, InMemorySessionService, SessionService};
use async_trait::async_trait;
use elitea_agent_runtime::host::{
    EventSink, ExecutionGuard, HostError, LocalExecutionGuard, LocalStop, ModelRequest,
    ModelTransport, RunOutcome, ToolProvider, ToolsetRequest,
};
use elitea_agent_runtime::instruction_authority::InstructionPlan;
use elitea_agent_runtime::request::{
    AgentExecutionKind, AgentExecutionPayload, AgentExecutionRequest, AgentInputBinding,
    NextInputSuggestionPolicy, ProjectContextSnapshot, UserInput,
};
use elitea_local_tools::approvals::{JsonFileChoices, WorkspaceSettings};
use elitea_local_tools::policy::{LocalWorkPolicy, SandboxMode};
use elitea_local_tools::provider::{LocalToolProvider, TOOLSET_NAME as LOCAL_TOOLSET};
use elitea_local_tools::session::{LocalSession, SessionConfig, TOOLS};
use futures::StreamExt as _;
use serde_json::{Map, Value, json};

use super::api::{ApiError, Credentials, LocalTurnStarted, PlatformApi};
use super::approvals::{ApprovalBroker, TurnBinding, UiDecision, UiPrompt};
use super::definition::{self, Admitted};
use super::events::{EventEmitter, Phase, TurnEvents};
use super::model::GatewayTransport;
use super::recorder::{FileChange, Recorder};
use super::remote_tools::{RemoteContext, RemoteToolProvider, RetryPolicy};
use super::tools::{ObservedToolset, ToolObserver};
use crate::workspaces::WorkspaceStore;

const APP_NAME: &str = "elitea-desktop";
const AGENT_NAME: &str = "elitea_agent";
const USER_ID: &str = "local";
/// How long one tool call may take (an approval can keep it waiting).
const TOOL_TIMEOUT: Duration = Duration::from_secs(60 * 60);

/// Where the stored `native_client_policy` comes from.
pub trait PolicySource: Send + Sync {
    /// The `local_work` section of the last policy the server sent.
    fn local_work(&self) -> Option<Value>;
}

/// A refused or failed IPC call, with the machine code the UI branches on.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct TurnError {
    pub code: String,
    pub message: String,
}

impl TurnError {
    fn new(code: &str, message: impl Into<String>) -> Self {
        Self {
            code: code.to_owned(),
            message: message.into(),
        }
    }
}

impl From<ApiError> for TurnError {
    fn from(error: ApiError) -> Self {
        Self {
            code: error.code,
            message: error.message,
        }
    }
}

impl std::fmt::Display for TurnError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str(&self.message)
    }
}

/// `agent_turn_start`'s arguments.
#[derive(Clone, Debug)]
pub struct TurnRequest {
    pub workspace_id: String,
    pub project_id: i64,
    /// The conversation's numeric id or UUID, as the UI holds it.
    pub conversation_id: Value,
    pub application_id: i64,
    pub version_id: i64,
    pub prompt: String,
    pub plan_mode: bool,
}

/// `agent_turn_start`'s answer.
#[derive(Clone, Debug, serde::Serialize, PartialEq, Eq)]
pub struct TurnStarted {
    pub turn_id: String,
    pub execution_id: String,
}

/// What the host is built from.
pub struct HostDeps {
    pub credentials: Arc<dyn Credentials>,
    pub client_version: String,
    pub policy: Arc<dyn PolicySource>,
    pub workspaces: Arc<WorkspaceStore>,
    pub emitter: Arc<dyn EventEmitter>,
    pub retry: RetryPolicy,
}

/// One workspace's local session and its prompt.
struct WorkspaceSession {
    session: Arc<LocalSession>,
    prompt: Arc<UiPrompt>,
    policy: LocalWorkPolicy,
    busy: Mutex<bool>,
}

/// A turn, kept after it ends for `turn_changes` and `checkpoint_restore`.
struct TurnEntry {
    workspace: Arc<WorkspaceSession>,
    recorder: Arc<Recorder>,
    stop: LocalStop,
    checkpoint: Mutex<Option<u64>>,
}

/// The D0 agent host: workspaces' sessions, running and finished turns.
pub struct AgentHost {
    deps: HostDeps,
    api: Arc<PlatformApi>,
    broker: Arc<ApprovalBroker>,
    sessions: Mutex<HashMap<String, Arc<WorkspaceSession>>>,
    turns: Mutex<HashMap<String, Arc<TurnEntry>>>,
}

/// The cloud's memory splice (`appendCurrentInstructionsMemories`,
/// services/elitea-main/internal/application/agentexecution/memories.go):
/// authored text, one blank line, the recall.
#[must_use]
pub fn splice_memory(instructions: &str, recall: &str) -> String {
    match (instructions.is_empty(), recall.is_empty()) {
        (_, true) => instructions.to_owned(),
        (true, false) => recall.to_owned(),
        (false, false) => format!("{instructions}\n\n{recall}"),
    }
}

fn conversation_key(value: &Value) -> Result<String, TurnError> {
    match value {
        // `.` and `..` would be resolved away as path segments.
        Value::String(text)
            if !text.is_empty() && text.len() <= 64 && text != "." && text != ".." =>
        {
            Ok(text.clone())
        }
        Value::Number(number) if number.as_u64().is_some() => Ok(number.to_string()),
        _ => Err(TurnError::new(
            "invalid_request",
            "conversation_id must be the conversation's id or UUID",
        )),
    }
}

impl AgentHost {
    /// # Errors
    ///
    /// The HTTP client cannot be built.
    pub fn new(deps: HostDeps) -> Result<Self, TurnError> {
        let api = Arc::new(PlatformApi::new(
            deps.credentials.clone(),
            &deps.client_version,
        )?);
        Ok(Self {
            deps,
            api,
            broker: Arc::new(ApprovalBroker::default()),
            sessions: Mutex::new(HashMap::new()),
            turns: Mutex::new(HashMap::new()),
        })
    }

    fn policy(&self) -> Result<LocalWorkPolicy, TurnError> {
        let policy: LocalWorkPolicy = self
            .deps
            .policy
            .local_work()
            .and_then(|section| serde_json::from_value(section).ok())
            .unwrap_or_default();
        if !policy.allowed {
            return Err(TurnError::new(
                "local_work_disabled",
                "Local work is turned off by your organisation's policy.",
            ));
        }
        Ok(policy)
    }

    /// The workspace's session, (re)opened when the policy changed.
    fn session(
        &self,
        workspace_id: &str,
        root: PathBuf,
        policy: &LocalWorkPolicy,
    ) -> Result<Arc<WorkspaceSession>, TurnError> {
        let mut sessions = self
            .sessions
            .lock()
            .map_err(|_| TurnError::new("internal", "session table poisoned"))?;
        if let Some(existing) = sessions.get(workspace_id)
            && &existing.policy == policy
        {
            return Ok(existing.clone());
        }
        if sessions
            .get(workspace_id)
            .is_some_and(|existing| existing.busy.lock().is_ok_and(|busy| *busy))
        {
            return Err(TurnError::new(
                "workspace_busy",
                "A turn is already running in this workspace.",
            ));
        }
        let data_dir = self.deps.workspaces.data_dir(workspace_id);
        std::fs::create_dir_all(&data_dir).map_err(|e| {
            TurnError::new(
                "storage",
                format!("could not prepare the workspace data: {e}"),
            )
        })?;
        let choices = JsonFileChoices::open(data_dir.join("choices.json"))
            .map_err(|e| TurnError::new("storage", e.message().to_owned()))?;
        let prompt = Arc::new(UiPrompt::new(self.broker.clone()));
        let session = LocalSession::open(SessionConfig {
            root,
            session_id: workspace_id.to_owned(),
            policy: policy.clone(),
            settings: WorkspaceSettings::default(),
            choices: Arc::new(choices),
            prompt: prompt.clone(),
            data_dir,
            shell: None,
        })
        .map_err(|e| TurnError::new("workspace_unavailable", e.message().to_owned()))?;
        let entry = Arc::new(WorkspaceSession {
            session,
            prompt,
            policy: policy.clone(),
            busy: Mutex::new(false),
        });
        sessions.insert(workspace_id.to_owned(), entry.clone());
        Ok(entry)
    }

    /// `agent_turn_start`: resolve, check, start; the run continues in the
    /// background. Every refusal is also an `error` event of the turn.
    ///
    /// # Errors
    ///
    /// A refusal (`secrets_withheld`, `local_work_disabled`, …) or a failed
    /// platform call.
    pub async fn start(self: &Arc<Self>, request: TurnRequest) -> Result<TurnStarted, TurnError> {
        let turn_id = uuid::Uuid::new_v4().to_string();
        let events = Arc::new(TurnEvents::new(turn_id.clone(), self.deps.emitter.clone()));
        events.status(Phase::Resolving, None);
        match self.prepare(&request, &events).await {
            Ok(prepared) => {
                let started = TurnStarted {
                    turn_id: turn_id.clone(),
                    execution_id: prepared.started.execution_id.clone(),
                };
                let (guard, stop) = LocalExecutionGuard::new();
                let entry = Arc::new(TurnEntry {
                    workspace: prepared.workspace.clone(),
                    recorder: Arc::new(Recorder::default()),
                    stop,
                    checkpoint: Mutex::new(None),
                });
                if let Ok(mut turns) = self.turns.lock() {
                    turns.insert(turn_id.clone(), entry.clone());
                }
                let host = self.clone();
                tokio::spawn(async move { host.run(prepared, entry, guard).await });
                Ok(started)
            }
            Err(error) => {
                events.error(&error.code, &error.message);
                events.status(Phase::Error, Some(&error.message));
                Err(error)
            }
        }
    }

    async fn prepare(
        &self,
        request: &TurnRequest,
        events: &Arc<TurnEvents>,
    ) -> Result<Prepared, TurnError> {
        let conversation = conversation_key(&request.conversation_id)?;
        if request.prompt.trim().is_empty() {
            return Err(TurnError::new("invalid_request", "The message is empty."));
        }
        let workspace = self
            .deps
            .workspaces
            .get(&request.workspace_id)
            .map_err(|e| TurnError::new("storage", e.to_string()))?
            .ok_or_else(|| TurnError::new("workspace_unknown", "That workspace is not open."))?;
        if workspace
            .project_id
            .is_some_and(|bound| bound != request.project_id)
        {
            return Err(TurnError::new(
                "workspace_project_mismatch",
                "This workspace is bound to another project.",
            ));
        }
        let policy = self.policy()?;
        let resolved = self
            .api
            .resolved_version(
                request.project_id,
                request.application_id,
                request.version_id,
            )
            .await?;
        if resolved.project_context_withheld {
            events.status(
                Phase::Resolving,
                Some("You cannot view this project's context, so this turn runs without it."),
            );
        }
        let local_names: Vec<&str> = TOOLS.iter().map(|tool| tool.name).collect();
        let admitted = definition::admit(&resolved, &local_names)
            .map_err(|refusal| TurnError::new(refusal.code, refusal.message))?;
        let participant_id = self
            .api
            .answering_participant(
                request.project_id,
                &conversation,
                request.application_id,
                request.version_id,
            )
            .await?;
        let workspace_session = self.session(
            &request.workspace_id,
            PathBuf::from(&workspace.path),
            &policy,
        )?;
        {
            let mut busy = workspace_session
                .busy
                .lock()
                .map_err(|_| TurnError::new("internal", "workspace state poisoned"))?;
            if *busy {
                return Err(TurnError::new(
                    "workspace_busy",
                    "A turn is already running in this workspace.",
                ));
            }
            *busy = true;
        }
        events.status(Phase::Starting, None);
        let question_id = uuid::Uuid::new_v4().to_string();
        let started = self
            .api
            .start_turn(
                request.project_id,
                &conversation,
                &json!({
                    "question_id": question_id,
                    "user_input": request.prompt,
                    "participant_id": participant_id,
                }),
            )
            .await;
        let started = match started {
            Ok(started) => started,
            Err(error) => {
                release(&workspace_session);
                return Err(error.into());
            }
        };
        Ok(Prepared {
            request: request.clone(),
            events: events.clone(),
            workspace: workspace_session,
            admitted,
            started,
            policy,
        })
    }

    async fn run(
        self: Arc<Self>,
        prepared: Prepared,
        entry: Arc<TurnEntry>,
        mut guard: LocalExecutionGuard,
    ) {
        let Prepared {
            request,
            events,
            workspace,
            admitted,
            started,
            policy,
        } = prepared;
        let recorder = entry.recorder.clone();
        workspace.prompt.bind(Some(TurnBinding {
            events: events.clone(),
            recorder: recorder.clone(),
        }));
        let session = workspace.session.clone();
        session.set_plan_mode(request.plan_mode);
        session.begin_turn(&checkpoint_label(&request.prompt));
        events.status(Phase::Running, None);

        let sink = Arc::new(TurnSink::default());
        let run = self.run_agent(
            &request, &events, &workspace, &admitted, &started, &recorder, &sink,
        );
        let outcome = tokio::select! {
            result = run => Some(result),
            _ = guard.ended() => None,
        };
        *entry
            .checkpoint
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner) = session.turn_checkpoint();
        workspace.prompt.bind(None);
        self.broker.forget_turn(events.turn_id());
        let conversation_id = request.conversation_id.clone();
        let Some(result) = outcome else {
            let _ = sink.finish(RunOutcome::Stopped).await;
            release(&workspace);
            events.status(Phase::Cancelled, None);
            events.send(
                "done",
                json!({
                    "committed": false,
                    "conversation_id": conversation_id,
                    "message_ids": [],
                    "changed_files": recorder.changes(session.workspace()).len(),
                }),
            );
            return;
        };
        let (answer, failure) = match result {
            Ok(answer) => (answer, None),
            Err(error) => (sink.last_text(), Some(error)),
        };
        let _ = sink
            .finish(match &failure {
                None => RunOutcome::Completed {
                    answer: answer.clone(),
                },
                Some(_) => RunOutcome::Failed {
                    code: "local_turn.failed",
                },
            })
            .await;
        if let Some(error) = &failure {
            events.error(&error.code, &error.message);
        }
        events.status(Phase::Committing, None);
        let sandbox_mode = SandboxMode::WorkspaceWrite.min(policy.max_sandbox_mode);
        let mut assistant = json!({ "content": answer });
        if let Some(error) = &failure {
            assistant["is_error"] = json!(true);
            assistant["error"] = json!(error.message.chars().take(4000).collect::<String>());
        }
        let body = json!({
            "user_message": { "content": request.prompt },
            "assistant_message": assistant,
            "tool_calls": recorder.tool_calls(),
            "hitl_exchanges": recorder.hitl_exchanges(),
            "local_work": recorder.work_report(sandbox_mode.as_str()),
        });
        let committed = self
            .api
            .commit_turn(request.project_id, &started.execution_id, &body)
            .await;
        release(&workspace);
        let changed_files = recorder.changes(session.workspace()).len();
        match committed {
            Ok(_) => {
                if failure.is_some() {
                    events.status(Phase::Error, failure.as_ref().map(|e| e.message.as_str()));
                } else {
                    events.status(Phase::Done, None);
                }
                events.send(
                    "done",
                    json!({
                        "committed": true,
                        "conversation_id": conversation_id,
                        "message_ids": [started.question_id, started.response_message_id],
                        "changed_files": changed_files,
                    }),
                );
            }
            Err(error) => {
                events.error(
                    &error.code,
                    &format!("The turn could not be saved: {}", error.message),
                );
                events.status(Phase::Error, Some(&error.message));
                events.send(
                    "done",
                    json!({
                        "committed": false,
                        "conversation_id": conversation_id,
                        "message_ids": [],
                        "changed_files": changed_files,
                    }),
                );
            }
        }
    }

    #[allow(clippy::too_many_arguments)]
    async fn run_agent(
        &self,
        request: &TurnRequest,
        events: &Arc<TurnEvents>,
        workspace: &Arc<WorkspaceSession>,
        admitted: &Admitted,
        started: &LocalTurnStarted,
        recorder: &Arc<Recorder>,
        sink: &Arc<TurnSink>,
    ) -> Result<String, TurnError> {
        let failed = |code: &str, message: &str| TurnError::new(code, message);
        // ModelTransport: /llm with the native token and the execution id.
        let transport = GatewayTransport {
            http: self.api.http().clone(),
            credentials: self.deps.credentials.clone(),
            project_id: request.project_id,
            execution_id: started.execution_id.clone(),
            events: events.clone(),
        };
        let bound = transport
            .bind(ModelRequest {
                model_project_id: u32::try_from(request.project_id).unwrap_or_default(),
                model_name: admitted.model.model_name.clone(),
                system_instruction: splice_memory(
                    &admitted.instructions,
                    &started.memory_recall.text,
                ),
                max_tokens: admitted.model.max_tokens,
                temperature: admitted.model.temperature,
                reasoning_effort: admitted.model.reasoning_effort,
                response_schema: None,
                allow_text_continuation: false,
                max_model_turns: admitted.step_limit,
            })
            .map_err(|e| failed("model_unavailable", &e.to_string()))?;

        // Skills and project context: the runtime's instruction authority.
        let plan = InstructionPlan::admit(&execution_request(request, admitted, started)).map_err(
            |_| {
                failed(
                    "skills_invalid",
                    "The agent's frozen skills could not be loaded.",
                )
            },
        )?;

        // ToolProvider: local tools, then the remote toolkits (none in plan
        // mode: a remote tool may write, and plan mode is read-only).
        let remote = Arc::new(RemoteContext {
            api: self.api.clone(),
            project_id: request.project_id,
            execution_id: started.execution_id.clone(),
            application_id: request.application_id,
            version_id: request.version_id,
            prompt: workspace.prompt.clone(),
            retry: self.deps.retry,
        });
        let remote_specs = if request.plan_mode {
            Vec::new()
        } else {
            admitted.remote_tools.clone()
        };
        let provider = LocalToolProvider::new(workspace.session.clone())
            .with(Arc::new(RemoteToolProvider::new(&remote_specs, &remote)));
        let toolsets = provider
            .toolsets(&ToolsetRequest {
                toolkits: Vec::new(),
                context_label: "agent".to_owned(),
            })
            .await
            .map_err(|e| failed("tools_unavailable", &e.to_string()))?;
        let observer = ToolObserver {
            events: events.clone(),
            recorder: recorder.clone(),
            session: Some(workspace.session.clone()),
        };

        let mut builder = LlmAgentBuilder::new(AGENT_NAME)
            .model(bound.adk_model())
            .max_iterations(admitted.step_limit)
            .tool_timeout(TOOL_TIMEOUT)
            .disallow_transfer_to_parent(true)
            .disallow_transfer_to_peers(true);
        builder = plan.bind_builder(builder);
        for toolset in toolsets {
            let remote = toolset.name() != LOCAL_TOOLSET;
            builder = builder.toolset(Arc::new(ObservedToolset::new(
                toolset,
                observer.clone(),
                remote,
            )) as Arc<dyn Toolset>);
        }
        for toolset in plan.toolsets() {
            builder = builder.toolset(Arc::new(ObservedToolset::new(
                toolset,
                ToolObserver {
                    session: None,
                    ..observer.clone()
                },
                false,
            )) as Arc<dyn Toolset>);
        }
        let agent: Arc<dyn Agent> = Arc::new(
            builder
                .build()
                .map_err(|_| failed("agent_invalid", "The agent could not be assembled."))?,
        );
        let agent = plan.wrap(agent);

        // StateStore: an in-memory session for this one turn.
        let sessions: Arc<dyn SessionService> = Arc::new(InMemorySessionService::new());
        let session_id = started.execution_id.clone();
        sessions
            .create(CreateRequest {
                app_name: APP_NAME.to_owned(),
                user_id: USER_ID.to_owned(),
                session_id: Some(session_id.clone()),
                state: HashMap::new(),
            })
            .await
            .map_err(|_| failed("internal", "The agent session could not be created."))?;
        let run_config = RunConfig {
            streaming_mode: StreamingMode::None,
            ..RunConfig::default()
        };
        let runner = Runner::builder()
            .app_name(APP_NAME)
            .agent(agent)
            .session_service(sessions)
            .run_config(run_config)
            .build()
            .map_err(|_| failed("internal", "The agent runner could not be built."))?;
        let mut stream = runner
            .run_str(
                USER_ID,
                &session_id,
                Content::new("user").with_text(request.prompt.clone()),
            )
            .await
            .map_err(|e| failed("agent_failed", &e.to_string()))?;
        while let Some(event) = stream.next().await {
            let event = event.map_err(|e| model_failure(&e.to_string()))?;
            sink.emit(&event)
                .await
                .map_err(|e| failed("agent_failed", &e.to_string()))?;
        }
        Ok(bound
            .take_completed_text()
            .unwrap_or_else(|_| sink.last_text()))
    }

    /// `agent_turn_cancel`. False when the turn is unknown.
    pub fn cancel(&self, turn_id: &str) -> bool {
        let entry = self
            .turns
            .lock()
            .ok()
            .and_then(|turns| turns.get(turn_id).cloned());
        entry.is_some_and(|entry| {
            entry.stop.stop();
            self.broker.forget_turn(turn_id);
            true
        })
    }

    /// `approval_respond`. False when no such question is open.
    pub fn respond(&self, request_id: &str, decision: UiDecision) -> bool {
        self.broker.respond(request_id, decision)
    }

    fn entry(&self, turn_id: &str) -> Result<Arc<TurnEntry>, TurnError> {
        self.turns
            .lock()
            .ok()
            .and_then(|turns| turns.get(turn_id).cloned())
            .ok_or_else(|| TurnError::new("turn_unknown", "That turn is not known to this app."))
    }

    /// `turn_changes`.
    ///
    /// # Errors
    ///
    /// An unknown turn.
    pub fn changes(&self, turn_id: &str) -> Result<Vec<FileChange>, TurnError> {
        let entry = self.entry(turn_id)?;
        Ok(entry.recorder.changes(entry.workspace.session.workspace()))
    }

    /// `checkpoint_restore`: the whole turn, or one file of it.
    ///
    /// # Errors
    ///
    /// An unknown or running turn, or a failed restore.
    pub fn restore(&self, turn_id: &str, path: Option<&str>) -> Result<Vec<String>, TurnError> {
        let entry = self.entry(turn_id)?;
        if entry.workspace.busy.lock().is_ok_and(|busy| *busy) {
            return Err(TurnError::new(
                "workspace_busy",
                "Wait for the running turn to end before undoing.",
            ));
        }
        let Some(seq) = *entry
            .checkpoint
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
        else {
            return Ok(Vec::new());
        };
        let session = &entry.workspace.session;
        let report = match path {
            Some(path) => session.restore_file(seq, path),
            None => session.restore_checkpoint(seq),
        }
        .map_err(|e| TurnError::new("restore_failed", e.message().to_owned()))?;
        let mut restored = report.restored;
        restored.extend(report.deleted);
        Ok(restored)
    }
}

struct Prepared {
    request: TurnRequest,
    events: Arc<TurnEvents>,
    workspace: Arc<WorkspaceSession>,
    admitted: Admitted,
    started: LocalTurnStarted,
    policy: LocalWorkPolicy,
}

fn release(workspace: &WorkspaceSession) {
    if let Ok(mut busy) = workspace.busy.lock() {
        *busy = false;
    }
}

fn model_failure(text: &str) -> TurnError {
    // `model_gateway.<code>: message` from the model adapter keeps its code,
    // whatever prefix the adk error's display adds.
    text.find("model_gateway.")
        .and_then(|start| text[start..].split_once(": "))
        .map_or_else(
            || TurnError::new("agent_failed", text.to_owned()),
            |(code, message)| TurnError::new(code, message.to_owned()),
        )
}

fn checkpoint_label(prompt: &str) -> String {
    let line = prompt.lines().next().unwrap_or_default();
    let mut label: String = line.chars().take(60).collect();
    if label.is_empty() {
        label.push_str("agent turn");
    }
    label
}

/// The request the runtime's instruction authority admits from: the
/// version's frozen skills and project context, run id = the execution.
fn execution_request(
    request: &TurnRequest,
    admitted: &Admitted,
    started: &LocalTurnStarted,
) -> AgentExecutionRequest {
    let mut application = Map::new();
    application.insert(
        "version_details".to_owned(),
        Value::Object(admitted.version_details.clone()),
    );
    let project_context = admitted
        .version_details
        .get("project_context")
        .filter(|value| !value.is_null())
        .and_then(|value| serde_json::from_value::<ProjectContextSnapshot>(value.clone()).ok());
    AgentExecutionRequest {
        kind: AgentExecutionKind::Application,
        binding: AgentInputBinding {
            input_bundle_id: started.execution_id.clone(),
            input_bundle_digest: [0; 32],
            request_entry_id: started.question_id.clone(),
            request_immutable_version: started.execution_id.clone(),
            request_content_digest: [0; 32],
        },
        payload: AgentExecutionPayload {
            llm: Map::new(),
            chat_history: Vec::new(),
            user_input: UserInput::Text(request.prompt.clone()),
            thread_id: None,
            checkpoint_id: None,
            debug: false,
            tools: Vec::new(),
            application,
            internal_tools: Vec::new(),
            steps_limit: Some(admitted.step_limit),
            mcp_tokens: Map::new(),
            ignored_mcp_servers: Vec::new(),
            user_declined_mcp_servers: Vec::new(),
            should_continue: false,
            hitl_resume: false,
            hitl_action: None,
            hitl_value: None,
            hitl_decisions: Vec::new(),
            execution_generation: None,
            is_regenerate: false,
            meta: Map::new(),
            conversation_id: None,
            persona: String::new(),
            context_settings: Map::new(),
            supports_vision: false,
            return_chat_history: false,
            invoked_skills: Vec::new(),
            applied_skills: Vec::new(),
            auto_approve_sensitive_actions: false,
            attached_skills: Vec::new(),
            input_attachments: Vec::new(),
            parallel_reconcile: None,
            parallel_terminal_errors: Vec::new(),
            exception_handling_enabled: None,
            debug_mode: None,
            next_input_suggestion: NextInputSuggestionPolicy::default(),
            toolkit_guardrails: None,
            truncated_content: None,
            project_context,
            model_context_limits: None,
            summary_model: None,
        },
    }
}

/// The runtime's [`EventSink`] on the desktop: the adk events in order
/// (the UI gets its events from the model adapter and the tool wrapper),
/// keeping the last model text as the answer of a run that failed late.
#[derive(Default)]
pub struct TurnSink {
    last_text: Mutex<String>,
    outcome: Mutex<Option<RunOutcome>>,
}

impl TurnSink {
    fn last_text(&self) -> String {
        self.last_text
            .lock()
            .map(|text| text.clone())
            .unwrap_or_default()
    }
}

#[async_trait]
impl EventSink for TurnSink {
    async fn emit(&self, event: &Event) -> Result<(), HostError> {
        if event.llm_response.partial {
            return Ok(());
        }
        if let Some(content) = &event.llm_response.content {
            let has_calls = content
                .parts
                .iter()
                .any(|part| matches!(part, Part::FunctionCall { .. }));
            let text: String = content
                .parts
                .iter()
                .filter_map(|part| match part {
                    Part::Text { text } => Some(text.as_str()),
                    _ => None,
                })
                .collect();
            if !has_calls
                && !text.is_empty()
                && content.role == "model"
                && let Ok(mut last) = self.last_text.lock()
            {
                *last = text;
            }
        }
        Ok(())
    }

    async fn finish(&self, outcome: RunOutcome) -> Result<(), HostError> {
        if let Ok(mut slot) = self.outcome.lock() {
            slot.get_or_insert(outcome);
        }
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_memory_splice_is_the_clouds() {
        assert_eq!(splice_memory("Be brief.", ""), "Be brief.");
        assert_eq!(splice_memory("", "Memory"), "Memory");
        assert_eq!(splice_memory("Be brief.", "Memory"), "Be brief.\n\nMemory");
    }

    #[test]
    fn conversation_ids_are_numbers_or_short_strings() {
        assert_eq!(conversation_key(&json!(12)).unwrap(), "12");
        assert_eq!(conversation_key(&json!("6f1c")).unwrap(), "6f1c");
        assert!(conversation_key(&json!(-1)).is_err());
        assert!(conversation_key(&json!("")).is_err());
        assert!(conversation_key(&json!(".")).is_err());
        assert!(conversation_key(&json!("..")).is_err());
        assert!(conversation_key(&json!({})).is_err());
    }

    #[test]
    fn gateway_codes_survive_the_adk_error() {
        let error =
            model_failure("Model error: model_gateway.member_budget_exhausted: your budget");
        assert_eq!(error.code, "model_gateway.member_budget_exhausted");
        assert_eq!(error.message, "your budget");
        assert_eq!(model_failure("boom").code, "agent_failed");
    }
}

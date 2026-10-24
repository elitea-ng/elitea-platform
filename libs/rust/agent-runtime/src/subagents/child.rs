//! One child: its read-only tools, its budgeted model and the task that runs
//! it on its own session.

use std::collections::BTreeSet;
use std::collections::HashMap;
use std::fmt;
use std::sync::atomic::{AtomicU32, AtomicU64, Ordering};
use std::sync::{Arc, Mutex};

use adk_agent::LlmAgentBuilder;
use adk_core::{
    AdkError, Content, Event, Llm, LlmRequest, LlmResponseStream, Part, ReadonlyContext, RunConfig,
    StreamingMode, Tool, ToolExecutionStrategy, Toolset,
};
use adk_runner::Runner;
use adk_session::{CreateRequest, InMemorySessionService, SessionService};
use async_trait::async_trait;
use futures::StreamExt as _;
use tokio::sync::{mpsc, watch};
use tokio::task::AbortHandle;

use super::{ChildIdentity, ChildReport, ChildStatus, MAX_RESULT_BYTES, SubAgentHost, TOOL_NAMES};
use crate::text::clip_bytes;

const APP_NAME: &str = "elitea_subagents";
const USER_ID: &str = "parent";
/// The local tool a child never gets even if a host marks it read-only: the
/// plan goes to the person from the parent only.
const EXIT_PLAN_MODE: &str = "exit_plan_mode";
/// The error a budgeted model ends a child's run with.
const BUDGET_EXHAUSTED: &str = "subagent_budget_exhausted";

/// Whether a child may be offered `tool`: read-only (unless the child has a
/// checkout of its own, `writes`), not a sub-agent tool, not
/// `exit_plan_mode`, not excluded by the host.
fn admissible_for(tool: &dyn Tool, excluded: &BTreeSet<String>, writes: bool) -> bool {
    let name = tool.name();
    (writes || tool.is_read_only())
        && !TOOL_NAMES.contains(&name)
        && name != EXIT_PLAN_MODE
        && !excluded.contains(name)
}

fn admissible(tool: &dyn Tool, excluded: &BTreeSet<String>) -> bool {
    admissible_for(tool, excluded, false)
}

/// A tool a child cannot be given: it may change something.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct WritableTool {
    pub name: String,
}

impl fmt::Display for WritableTool {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(
            formatter,
            "the tool {} is not read-only and cannot be given to a sub-agent",
            self.name
        )
    }
}

impl std::error::Error for WritableTool {}

/// A fixed list of child tools, every one read-only: the only way to build
/// one from tools a caller names, so a list with a writing tool (or a
/// sub-agent tool) is a construction error, not a prompt.
pub struct ChildTools {
    tools: Vec<Arc<dyn Tool>>,
}

impl ChildTools {
    /// # Errors
    ///
    /// The first tool that may change something, or that a child is never
    /// given (the sub-agent tools, `exit_plan_mode`).
    pub fn new(tools: Vec<Arc<dyn Tool>>) -> Result<Self, WritableTool> {
        let none = BTreeSet::new();
        if let Some(tool) = tools.iter().find(|tool| !admissible(tool.as_ref(), &none)) {
            return Err(WritableTool {
                name: tool.name().to_owned(),
            });
        }
        Ok(Self { tools })
    }

    /// The tools, as one toolset a child is offered.
    #[must_use]
    pub fn toolset(self) -> Arc<dyn Toolset> {
        Arc::new(adk_tool::BasicToolset::new("subagent_tools", self.tools))
    }
}

/// A host's toolset as a child sees it: only its admissible tools
/// (read-only, not a sub-agent tool, not `exit_plan_mode`, not excluded).
/// adk resolves a toolset again before every model step, so a toolset whose
/// tools change (plan mode) is filtered every time.
pub struct ChildToolset {
    inner: Arc<dyn Toolset>,
    excluded: Arc<BTreeSet<String>>,
    /// The child has a checkout of its own: writing tools are kept.
    writes: bool,
}

impl ChildToolset {
    #[must_use]
    pub fn new(inner: Arc<dyn Toolset>, excluded: Arc<BTreeSet<String>>) -> Self {
        Self {
            inner,
            excluded,
            writes: false,
        }
    }

    /// The filter for a child with a checkout of its own: writing tools
    /// stay, everything else a child never gets is still dropped.
    #[must_use]
    pub fn isolated(inner: Arc<dyn Toolset>, excluded: Arc<BTreeSet<String>>) -> Self {
        Self {
            inner,
            excluded,
            writes: true,
        }
    }
}

#[async_trait]
impl Toolset for ChildToolset {
    fn name(&self) -> &str {
        self.inner.name()
    }

    async fn tools(&self, ctx: Arc<dyn ReadonlyContext>) -> adk_core::Result<Vec<Arc<dyn Tool>>> {
        Ok(self
            .inner
            .tools(ctx)
            .await?
            .into_iter()
            .filter(|tool| admissible_for(tool.as_ref(), &self.excluded, self.writes))
            .collect())
    }
}

/// A child's token counts.
#[derive(Default)]
pub(super) struct Counters {
    input: AtomicU64,
    output: AtomicU64,
    steps: AtomicU32,
}

/// The child's model: stops the child once its input tokens reach the
/// budget, before the next call (so the budget is a ceiling one call may
/// cross, never a second one).
struct BudgetedLlm {
    inner: Arc<dyn Llm>,
    counters: Arc<Counters>,
    budget: u64,
}

#[async_trait]
impl Llm for BudgetedLlm {
    fn name(&self) -> &str {
        self.inner.name()
    }

    fn schema_adapter(&self) -> &dyn adk_core::SchemaAdapter {
        self.inner.schema_adapter()
    }

    fn uses_interactions_api(&self) -> bool {
        self.inner.uses_interactions_api()
    }

    async fn generate_content(
        &self,
        request: LlmRequest,
        stream: bool,
    ) -> adk_core::Result<LlmResponseStream> {
        if self.counters.input.load(Ordering::SeqCst) >= self.budget {
            return Err(AdkError::model(format!(
                "{BUDGET_EXHAUSTED}: the sub-agent used its input-token budget"
            )));
        }
        let responses = self.inner.generate_content(request, stream).await?;
        let counters = Arc::clone(&self.counters);
        Ok(Box::pin(responses.map(move |response| {
            if let Ok(response) = &response
                && !response.partial
            {
                if let Some(usage) = &response.usage_metadata {
                    let count = |n: i32| u64::try_from(n).unwrap_or_default();
                    counters
                        .input
                        .fetch_add(count(usage.prompt_token_count), Ordering::SeqCst);
                    counters
                        .output
                        .fetch_add(count(usage.candidates_token_count), Ordering::SeqCst);
                }
                counters.steps.fetch_add(1, Ordering::SeqCst);
            }
            response
        })))
    }
}

struct State {
    status: ChildStatus,
    result: Option<String>,
    error: Option<&'static str>,
    /// Messages sent while it ran, not yet taken up: it is not settled
    /// until they are.
    pending: usize,
}

struct Shared {
    identity: ChildIdentity,
    state: Mutex<State>,
    status: watch::Sender<ChildStatus>,
    counters: Arc<Counters>,
    inbox: Mutex<Option<mpsc::UnboundedSender<String>>>,
    abort: Mutex<Option<AbortHandle>>,
}

/// A handle on one child; clones share it.
#[derive(Clone)]
pub(super) struct Child {
    shared: Arc<Shared>,
}

/// Why a message could not be given to a child.
pub(super) enum InputRefused {
    /// It ended for good (failed, out of budget, closed).
    Ended(ChildStatus),
    /// Continuing it would pass the running cap.
    Busy,
}

impl Child {
    pub(super) fn identity(&self) -> &ChildIdentity {
        &self.shared.identity
    }

    pub(super) fn status(&self) -> ChildStatus {
        *self.shared.status.borrow()
    }

    pub(super) fn report(&self) -> ChildReport {
        let (status, result, error) = self.shared.state.lock().map_or(
            (ChildStatus::Failed, None, Some("subagent_state_lost")),
            |state| (state.status, state.result.clone(), state.error),
        );
        ChildReport {
            agent_id: self.shared.identity.agent_id.clone(),
            status,
            result,
            steps: self.shared.counters.steps.load(Ordering::SeqCst),
            input_tokens: self.shared.counters.input.load(Ordering::SeqCst),
            output_tokens: self.shared.counters.output.load(Ordering::SeqCst),
            error,
        }
    }

    /// Until the child is settled.
    pub(super) async fn settled(&self) -> ChildReport {
        let mut status = self.shared.status.subscribe();
        // The sender lives as long as this handle: `wait_for` ends only
        // when the status settles.
        let _ = status.wait_for(|status| status.is_settled()).await;
        self.report()
    }

    /// Give the child another message: queued while it runs, a new run on
    /// its session once it answered. `running` is how many children run now
    /// and `cap` the most that may.
    pub(super) fn send(
        &self,
        message: String,
        running: usize,
        cap: usize,
    ) -> Result<(), InputRefused> {
        let Ok(mut state) = self.shared.state.lock() else {
            return Err(InputRefused::Ended(ChildStatus::Failed));
        };
        match state.status {
            ChildStatus::Running => {}
            ChildStatus::Completed if running < cap => {}
            ChildStatus::Completed => return Err(InputRefused::Busy),
            ended => return Err(InputRefused::Ended(ended)),
        }
        let sent = self
            .shared
            .inbox
            .lock()
            .ok()
            .and_then(|inbox| inbox.as_ref().map(|inbox| inbox.send(message).is_ok()))
            .unwrap_or(false);
        if !sent {
            return Err(InputRefused::Ended(ChildStatus::Cancelled));
        }
        state.pending += 1;
        if state.status != ChildStatus::Running {
            state.status = ChildStatus::Running;
            state.error = None;
            self.shared.status.send_replace(ChildStatus::Running);
        }
        Ok(())
    }

    /// Stop the child. The report when this stopped it while it ran.
    pub(super) fn close(&self) -> Option<ChildReport> {
        if let Ok(mut abort) = self.shared.abort.lock()
            && let Some(handle) = abort.take()
        {
            handle.abort();
        }
        if let Ok(mut inbox) = self.shared.inbox.lock() {
            inbox.take();
        }
        let stopped = {
            let mut state = self.shared.state.lock().ok()?;
            let running = state.status == ChildStatus::Running;
            if running {
                state.status = ChildStatus::Cancelled;
                state.error = Some("subagent_cancelled");
                state.pending = 0;
            }
            running
        };
        if !stopped {
            return None;
        }
        self.shared.status.send_replace(ChildStatus::Cancelled);
        Some(self.report())
    }

    /// One run ended. False when the child had been closed meanwhile (the
    /// close settled it), or more input waits (it is not settled yet).
    fn settle(&self, outcome: Outcome) -> bool {
        let Ok(mut state) = self.shared.state.lock() else {
            return false;
        };
        if state.status != ChildStatus::Running {
            return false;
        }
        let (status, text, error) = match outcome {
            Outcome::Answered(text) => (ChildStatus::Completed, text, None),
            Outcome::Exhausted(text) => {
                (ChildStatus::BudgetExhausted, text, Some(BUDGET_EXHAUSTED))
            }
            Outcome::StepLimit(text) => (
                ChildStatus::BudgetExhausted,
                text,
                Some("subagent_step_limit"),
            ),
            Outcome::Failed(text) => (ChildStatus::Failed, text, Some("subagent_failed")),
        };
        if let Some(text) = text {
            state.result = Some(clip_bytes(&text, MAX_RESULT_BYTES).to_owned());
        }
        if state.pending > 0 && status == ChildStatus::Completed {
            return false;
        }
        state.status = status;
        state.error = error;
        state.pending = 0;
        drop(state);
        self.shared.status.send_replace(status);
        true
    }

    /// A queued message is being taken up.
    fn take_pending(&self) {
        if let Ok(mut state) = self.shared.state.lock() {
            state.pending = state.pending.saturating_sub(1);
        }
    }
}

/// How one run of a child ended, with its last text.
enum Outcome {
    Answered(Option<String>),
    Exhausted(Option<String>),
    StepLimit(Option<String>),
    Failed(Option<String>),
}

/// What [`start`] needs.
pub(super) struct Start {
    pub identity: ChildIdentity,
    pub task: String,
    pub steps: u32,
    pub budget: u64,
    pub instruction: String,
    pub excluded: Arc<BTreeSet<String>>,
    pub host: Arc<dyn SubAgentHost>,
    /// `isolation: "worktree"`: the host's isolated toolsets, writes kept.
    pub isolated: bool,
}

/// Build the child and start its first run on a task of its own.
///
/// # Errors
///
/// The stable code of what could not be prepared (its model, its tools,
/// its agent, its session).
pub(super) async fn start(start: Start) -> Result<Child, &'static str> {
    let Start {
        identity,
        task,
        steps,
        budget,
        instruction,
        excluded,
        host,
        isolated,
    } = start;
    let counters = Arc::new(Counters::default());
    let model = host
        .bind_model(&identity, &instruction)
        .map_err(|_| "subagent_model_unavailable")?;
    let toolsets = if isolated {
        host.isolated_toolsets(&identity)
            .await
            .map_err(|_| "subagent_checkout_unavailable")?
    } else {
        host.toolsets(&identity)
            .await
            .map_err(|_| "subagent_tools_unavailable")?
    };
    let name = identity.agent_name();
    let mut builder = LlmAgentBuilder::new(&name)
        .description(if isolated {
            "A sub-agent in a checkout of its own"
        } else {
            "A read-only sub-agent"
        })
        .model(Arc::new(BudgetedLlm {
            inner: model,
            counters: Arc::clone(&counters),
            budget,
        }))
        .max_iterations(steps)
        .disallow_transfer_to_parent(true)
        .disallow_transfer_to_peers(true)
        // Its reads run together, as the parent's do.
        .tool_execution_strategy(ToolExecutionStrategy::Auto);
    if let Some(config) = host.generation_config() {
        builder = builder.generate_content_config(config);
    }
    for toolset in toolsets {
        let filtered = if isolated {
            ChildToolset::isolated(toolset, Arc::clone(&excluded))
        } else {
            ChildToolset::new(toolset, Arc::clone(&excluded))
        };
        builder = builder.toolset(Arc::new(filtered));
    }
    let agent = builder.build().map_err(|_| "subagent_invalid")?;
    let sessions = Arc::new(InMemorySessionService::new());
    sessions
        .create(CreateRequest {
            app_name: APP_NAME.to_owned(),
            user_id: USER_ID.to_owned(),
            session_id: Some(identity.agent_id.clone()),
            state: HashMap::new(),
        })
        .await
        .map_err(|_| "subagent_session_unavailable")?;
    let runner = Runner::builder()
        .app_name(APP_NAME)
        .agent(Arc::new(agent))
        .session_service(sessions)
        .run_config(RunConfig {
            streaming_mode: StreamingMode::None,
            ..RunConfig::default()
        })
        .build()
        .map_err(|_| "subagent_invalid")?;
    let (status, _) = watch::channel(ChildStatus::Running);
    let (inbox, messages) = mpsc::unbounded_channel();
    let child = Child {
        shared: Arc::new(Shared {
            identity,
            state: Mutex::new(State {
                status: ChildStatus::Running,
                result: None,
                error: None,
                pending: 0,
            }),
            status,
            counters,
            inbox: Mutex::new(Some(inbox)),
            abort: Mutex::new(None),
        }),
    };
    host.started(child.identity());
    // Held across the spawn: a close that comes at once waits for the
    // handle it aborts.
    {
        let mut abort = child
            .shared
            .abort
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner);
        let run = tokio::spawn(drive(child.clone(), runner, messages, task, host));
        *abort = Some(run.abort_handle());
    }
    Ok(child)
}

/// The child's life: its task, then each message sent to it, one run each
/// on its own session, until it is closed or the parent's run ends.
async fn drive(
    child: Child,
    runner: Runner,
    mut messages: mpsc::UnboundedReceiver<String>,
    task: String,
    host: Arc<dyn SubAgentHost>,
) {
    let mut message = task;
    loop {
        let outcome = run(&child, &runner, message, host.as_ref()).await;
        if child.settle(outcome) {
            host.finished(child.identity(), &child.report());
        }
        // Only an answered child is continued; any other end is final.
        if !matches!(
            child.status(),
            ChildStatus::Running | ChildStatus::Completed
        ) {
            return;
        }
        match messages.recv().await {
            Some(next) => {
                child.take_pending();
                message = next;
            }
            None => return,
        }
    }
}

async fn run(child: &Child, runner: &Runner, message: String, host: &dyn SubAgentHost) -> Outcome {
    let identity = child.identity();
    let Ok(mut stream) = runner
        .run_str(
            USER_ID,
            &identity.agent_id,
            Content::new("user").with_text(message),
        )
        .await
    else {
        return Outcome::Failed(None);
    };
    let mut last: Option<String> = None;
    while let Some(event) = stream.next().await {
        match event {
            Ok(event) => {
                if let Some(text) = model_text(&event) {
                    last = Some(text);
                }
                host.event(identity, &event);
            }
            Err(error) => {
                let text = error.to_string();
                return if text.contains(BUDGET_EXHAUSTED) {
                    Outcome::Exhausted(last)
                } else if crate::step_limit::reached(&error) {
                    Outcome::StepLimit(last)
                } else {
                    tracing::warn!(
                        event = "subagent_failed",
                        agent_id = %identity.agent_id,
                        error = %error,
                        "a sub-agent's run ended in an error"
                    );
                    Outcome::Failed(last)
                };
            }
        }
    }
    Outcome::Answered(last)
}

/// The text of one finished model answer.
fn model_text(event: &Event) -> Option<String> {
    if event.llm_response.partial {
        return None;
    }
    let content = event.llm_response.content.as_ref()?;
    if content.role != "model" {
        return None;
    }
    let text: String = content
        .parts
        .iter()
        .filter_map(|part| match part {
            Part::Text { text } => Some(text.as_str()),
            _ => None,
        })
        .collect();
    (!text.trim().is_empty()).then_some(text)
}

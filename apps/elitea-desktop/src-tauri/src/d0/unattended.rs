//! Unattended turns (ADR-0045 decisions 1, 3 and 4; #1435, #1436).
//!
//! The person gives one grant at the start of a turn ([`UnattendedGrant`]).
//! The grant answers at once only the questions it names, with `source`
//! `grant`: "Continue?" at the step limit (`continue_step_limit`), a
//! `PreToolUse` hook's ask (`hook_asks`) and a remote toolkit confirmation
//! (`remote_confirmations`), and a browser form submit or upload on a host
//! that `browser_submits` names (ADR-0048 decision 8). Every other question goes to the person as in
//! a turn without a grant, and takes its default past its wait. The grant
//! does not imply bypass mode: the local rules' asks follow the turn's own
//! `bypass` (the person's Bypass mode), as in every other turn.
//!
//! The turn is the root of a chain. When the turn ends at a cap that the
//! grant covers ([`next_step`]), the host starts the next turn of the chain
//! after the commit landed, with the same grant and the runtime's
//! `CONTINUE_PROMPT`. The platform counts the chain and refuses a
//! continuation past one of its limits (409 `chain_limit_reached`). The
//! commit of the chain's last turn carries its [`ChainOutcome`]. With the
//! grant's `goal_check` the chain has a run brief, its continuations
//! restate it, and a turn that would end the chain `completed` is checked
//! against it first ([`GoalStep`], [`super::goal`]; decision 11).
//!
//! Every question that a person answers has a wait (decision 3):
//! [`RULE_ASK_WAIT`] for a rule's or a hook's ask, [`REMOTE_CONFIRMATION_WAIT`]
//! for a remote toolkit confirmation, [`PLAN_WAIT`] for a plan and the
//! runtime's `CONTINUE_TIMEOUT` for "Continue?". Past the wait the default
//! answers (`source` `timeout`). In an unattended turn a question of a kind
//! without its own wait gets [`RULE_ASK_WAIT`], so the waits bound every
//! turn of a chain; the platform's chain deadline refuses the next turn.

use std::sync::Arc;
use std::time::Duration;

use elitea_agent_runtime::step_limit::ContinuePolicy;
use elitea_local_tools::approvals::Unattended;
use serde::{Deserialize, Serialize};
use serde_json::{Value, json};

use super::api::{LocalTurnStarted, PlatformApi};
use super::events::TurnEvents;
use super::goal::{self, BriefAnswer, ChainGoal, GoalCheck, GoalOutcome};
use super::turn::{TurnError, TurnRequest};

/// How long the host waits, after a turn's commit lands, for the chain's
/// next turn to start. Past it the chain ends as `cancelled`: the UI, which
/// stays busy while the host continues, is never held for good. A slow
/// commit does not count against it.
pub const CONTINUE_WAIT: Duration = Duration::from_secs(60);

/// The platform's refusal of a continuation past a chain limit (409).
pub const CHAIN_LIMIT_REACHED: &str = "chain_limit_reached";

/// How long a local rule's or a hook's ask waits for the person. Past it
/// the call is declined.
pub const RULE_ASK_WAIT: Duration = Duration::from_mins(30);

/// How long a remote toolkit confirmation waits for the person (the
/// platform's approval lifetime). Past it the call is declined.
pub const REMOTE_CONFIRMATION_WAIT: Duration = Duration::from_mins(15);

/// How long a proposed plan waits for Accept or Decline. Past it the plan
/// is declined.
pub const PLAN_WAIT: Duration = Duration::from_mins(30);

/// The `source` of a HITL exchange the person answered.
pub const SOURCE_PERSON: &str = "person";
/// The `source` of a HITL exchange the unattended grant answered.
pub const SOURCE_GRANT: &str = "grant";
/// The `source` of a HITL exchange that bypass mode answered.
pub const SOURCE_BYPASS: &str = "bypass";
/// The `source` of a HITL exchange that its default answered: nobody
/// answered within the wait.
pub const SOURCE_TIMEOUT: &str = "timeout";

/// The platform's refusal of a grant (422). The platform owns the chain
/// maximums: the host checks only that a value makes sense, and shows the
/// platform's refusal ([`refusal_message`]).
pub const GRANT_REFUSED: &str = "unattended_grant_refused";

/// The host's code for a grant that the platform refused because an
/// administrator turned grants off (reason `disabled`). The UI hides the
/// "Run to completion" control on it.
pub const GRANTS_OFF: &str = "unattended_grants_off";

/// A readable message for a refused grant: the refusal's `field` and
/// `reason` (`over_maximum`, `invalid`, `disabled`, …).
#[must_use]
pub fn refusal_message(field: Option<&str>, reason: Option<&str>) -> String {
    let limit = match field {
        Some("chain.max_continuations") => Some("number of continuations"),
        Some("chain.max_spend") => Some("spend limit"),
        Some("chain.deadline_seconds") => Some("deadline"),
        _ => None,
    };
    match (reason, limit) {
        (Some("over_maximum"), Some(limit)) => format!(
            "The unattended run's {limit} is higher than the platform allows. Choose a lower value, or ask your administrator for the maximum."
        ),
        (Some("invalid"), Some(limit)) => {
            format!("The unattended run's {limit} is not valid.")
        }
        (Some("disabled"), _) => "Unattended runs are turned off on this platform.".to_owned(),
        (Some(reason), _) => format!("The platform refused the unattended grant ({reason})."),
        (None, _) => "The platform refused the unattended grant.".to_owned(),
    }
}

/// The chain's limits, as the person sets them. An absent value is the
/// platform's default.
#[derive(Clone, Debug, Default, Deserialize, Serialize, PartialEq)]
#[serde(deny_unknown_fields)]
pub struct ChainLimits {
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub max_continuations: Option<u32>,
    /// US dollars, at most four decimals.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub max_spend: Option<f64>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub deadline_seconds: Option<u64>,
}

impl ChainLimits {
    fn is_empty(&self) -> bool {
        self.max_continuations.is_none()
            && self.max_spend.is_none()
            && self.deadline_seconds.is_none()
    }
}

/// `agent_turn_start.unattended`: what the grant answers. Serialized as
/// the resume record keeps it (ADR-0045 decision 7).
#[derive(Clone, Debug, Default, Deserialize, Serialize, PartialEq)]
#[serde(deny_unknown_fields)]
pub struct UnattendedGrant {
    /// A UUID; the host makes one when the UI sends none.
    #[serde(default)]
    pub grant_id: Option<String>,
    /// "Continue?" at the step limit continues, and the chain continues
    /// after the turn's last continue.
    #[serde(default)]
    pub continue_step_limit: bool,
    /// The chain continues after the 512 tool-call cap.
    #[serde(default)]
    pub continue_tool_call_cap: bool,
    /// A `PreToolUse` hook's ask is allowed. Without it, the person
    /// answers the hook's ask.
    #[serde(default)]
    pub hook_asks: bool,
    /// A remote toolkit confirmation is approved. Without it, the person
    /// answers the confirmation.
    #[serde(default)]
    pub remote_confirmations: bool,
    #[serde(default)]
    pub chain: Option<ChainLimits>,
    /// The chain's run brief and goal check (ADR-0045 decision 11);
    /// absent is on.
    #[serde(default)]
    pub goal_check: Option<bool>,
    /// The hosts on which a browser pane form submit or upload is allowed
    /// (ADR-0048 decision 8, client contract 1.38). A buy, pay, delete,
    /// send or sign control and a download are never the grant's.
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub browser_submits: Vec<String>,
}

/// The most hosts a grant's `browser_submits` names (the platform's
/// `unattended.MaxBrowserSubmits`).
pub const MAX_BROWSER_SUBMITS: usize = 32;

/// Whether `host` may be named by `browser_submits`: a lowercase DNS host
/// name of at least two labels, no scheme, port, wildcard or address, and a
/// last label that is not all digits (`example.123` is refused). The
/// platform's `unattended.ValidBrowserSubmitHost` is the same rule.
fn valid_submit_host(host: &str) -> bool {
    !host.starts_with("*.")
        && elitea_local_tools::policy::valid_browser_domain(host)
        && host
            .rsplit('.')
            .next()
            .is_some_and(|last| !last.bytes().all(|byte| byte.is_ascii_digit()))
}

fn refused(message: &str) -> TurnError {
    TurnError::new("invalid_request", message)
}

impl UnattendedGrant {
    /// The grant checked, with its `grant_id` (a new one when absent).
    ///
    /// # Errors
    ///
    /// `invalid_request` for a `grant_id` that is not a UUID, a deadline of
    /// 0 seconds or a spend limit that is not more than 0. The platform
    /// checks its maximums itself (422 [`GRANT_REFUSED`]).
    pub fn admit(mut self) -> Result<Self, TurnError> {
        let id = match self.grant_id.take() {
            Some(id) => uuid::Uuid::parse_str(&id)
                .map_err(|_| refused("The unattended grant's id is not a UUID."))?,
            None => uuid::Uuid::new_v4(),
        };
        self.grant_id = Some(id.hyphenated().to_string());
        if let Some(chain) = &self.chain {
            if chain.deadline_seconds == Some(0) {
                return Err(refused(
                    "An unattended run's deadline is at least 1 second.",
                ));
            }
            if chain
                .max_spend
                .is_some_and(|spend| !spend.is_finite() || spend <= 0.0)
            {
                return Err(refused(
                    "An unattended run's spend limit is more than 0 US dollars.",
                ));
            }
        }
        if self.chain.as_ref().is_some_and(ChainLimits::is_empty) {
            self.chain = None;
        }
        let mut hosts: Vec<String> = Vec::with_capacity(self.browser_submits.len());
        for host in std::mem::take(&mut self.browser_submits) {
            let host = host.trim().trim_end_matches('.').to_ascii_lowercase();
            if !valid_submit_host(&host) {
                return Err(refused(
                    "An unattended grant's browser_submits names host names only (example.com).",
                ));
            }
            if !hosts.contains(&host) {
                hosts.push(host);
            }
        }
        if hosts.len() > MAX_BROWSER_SUBMITS {
            return Err(refused(
                "An unattended grant's browser_submits names at most 32 hosts.",
            ));
        }
        self.browser_submits = hosts;
        Ok(self)
    }

    /// The grant's id (set by [`Self::admit`]).
    #[must_use]
    pub fn id(&self) -> &str {
        self.grant_id.as_deref().unwrap_or_default()
    }

    /// What the rules engine applies ([`Unattended`]).
    #[must_use]
    pub fn rule_answers(&self) -> Unattended {
        Unattended {
            hook_asks: self.hook_asks,
            browser_submits: self.browser_submits.clone(),
        }
    }

    /// Who answers "Continue?" at the step limit.
    #[must_use]
    pub const fn step_policy(&self) -> ContinuePolicy {
        if self.continue_step_limit {
            ContinuePolicy::Granted
        } else {
            ContinuePolicy::Ask
        }
    }

    /// Whether the chain gets a run brief and a goal check (absent: on).
    #[must_use]
    pub fn goal_check(&self) -> bool {
        self.goal_check.unwrap_or(true)
    }

    /// The grant's switches, as the turn's first `status` `running` shows
    /// them.
    #[must_use]
    pub fn shown(&self) -> Value {
        json!({
            "grant_id": self.id(),
            "continue_step_limit": self.continue_step_limit,
            "continue_tool_call_cap": self.continue_tool_call_cap,
            "hook_asks": self.hook_asks,
            "remote_confirmations": self.remote_confirmations,
            "goal_check": self.goal_check(),
            "browser_submits": self.browser_submits,
        })
    }
}

/// Why the host started a continuation: the link kind the local turn
/// start carries (`unattended.kind`).
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum ContinuationKind {
    StepLimit,
    ToolCallCap,
    /// The goal check found a condition not met (ADR-0045 decision 11).
    GoalNotMet,
    /// The app started again while the chain ran (ADR-0045 decision 7).
    Resume,
}

impl ContinuationKind {
    #[must_use]
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::StepLimit => "step_limit",
            Self::ToolCallCap => "tool_call_cap",
            Self::GoalNotMet => "goal_not_met",
            Self::Resume => "resume",
        }
    }
}

/// A chain's brief and goal check ([`ChainGoal`]), shared by its turns:
/// equal only to itself.
#[derive(Clone, Debug, Default)]
pub struct GoalHandle(pub std::sync::Arc<ChainGoal>);

impl PartialEq for GoalHandle {
    fn eq(&self, other: &Self) -> bool {
        std::sync::Arc::ptr_eq(&self.0, &other.0)
    }
}

/// One unattended turn: the grant and the turn's place in its chain.
#[derive(Clone, Debug, PartialEq)]
pub struct UnattendedTurn {
    pub grant: UnattendedGrant,
    /// The chain's first turn's execution id; `None` on that first turn.
    pub chain_root_execution_id: Option<String>,
    /// 0 for the chain's first turn, then 1, 2, …: as the host counts.
    pub continuation_index: u32,
    /// Why the host started this continuation; `None` on the first turn.
    pub kind: Option<ContinuationKind>,
    /// The chain's brief and goal check; `None` with `goal_check` off.
    pub goal: Option<GoalHandle>,
}

impl UnattendedTurn {
    /// The first turn of a chain.
    #[must_use]
    pub fn root(grant: UnattendedGrant) -> Self {
        let goal = grant.goal_check().then(GoalHandle::default);
        Self {
            grant,
            chain_root_execution_id: None,
            continuation_index: 0,
            kind: None,
            goal,
        }
    }

    /// The next turn of the chain, after a turn of execution `execution_id`.
    #[must_use]
    pub fn next(&self, execution_id: &str) -> Self {
        Self {
            grant: self.grant.clone(),
            chain_root_execution_id: Some(
                self.chain_root_execution_id
                    .clone()
                    .unwrap_or_else(|| execution_id.to_owned()),
            ),
            continuation_index: self.continuation_index.saturating_add(1),
            kind: self.kind,
            goal: self.goal.clone(),
        }
    }

    /// The chain's brief and goal check, when on.
    #[must_use]
    pub fn goal(&self) -> Option<&std::sync::Arc<ChainGoal>> {
        self.goal.as_ref().map(|handle| &handle.0)
    }

    /// The local turn start's `unattended` object.
    #[must_use]
    pub fn wire(&self) -> Value {
        let grant = &self.grant;
        let mut wire = json!({
            "grant_id": grant.id(),
            "continue_step_limit": grant.continue_step_limit,
            "continue_tool_call_cap": grant.continue_tool_call_cap,
            "hook_asks": grant.hook_asks,
            "remote_confirmations": grant.remote_confirmations,
            "goal_check": grant.goal_check(),
        });
        if !grant.browser_submits.is_empty() {
            wire["browser_submits"] = json!(grant.browser_submits);
        }
        if let Some(root) = &self.chain_root_execution_id {
            wire["chain_root_execution_id"] = json!(root);
            if let Some(kind) = self.kind {
                wire["kind"] = json!(kind.as_str());
            }
        }
        if let Some(chain) = &grant.chain {
            wire["chain"] = json!(chain);
        }
        wire
    }
}

/// `LocalTurnStarted.unattended`: the chain as the platform admitted it.
#[derive(Clone, Debug, Default, Deserialize, PartialEq)]
pub struct StartedChain {
    #[serde(default)]
    pub grant_id: Option<String>,
    #[serde(default)]
    pub chain_root_execution_id: Option<String>,
    #[serde(default)]
    pub continuation_index: Option<u32>,
    /// The effective chain limits.
    #[serde(default)]
    pub chain: Option<Value>,
}

/// How a chain ended: the commit's `chain_outcome` on the chain's last turn.
#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum ChainOutcome {
    /// The agent answered.
    Completed,
    /// The turn failed.
    Failed,
    /// The turn stopped at a cap the grant does not cover: it waits for the
    /// person's **Continue**.
    Waiting,
    /// A chain limit refused the next turn.
    LimitReached,
    /// The person stopped the turn.
    Cancelled,
}

impl ChainOutcome {
    #[must_use]
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::Completed => "completed",
            Self::Failed => "failed",
            Self::Waiting => "waiting",
            Self::LimitReached => "limit_reached",
            Self::Cancelled => "cancelled",
        }
    }
}

/// How a turn's run ended, as the chain sees it.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum TurnEnd {
    Answered,
    Failed,
    /// At the step limit. `exhausted`: the turn used its continues.
    StepLimit {
        exhausted: bool,
    },
    /// At the 512 tool-call cap.
    ToolCallCap,
    /// Stopped at a step boundary for a send to the cloud (ADR-0045
    /// decision 8).
    Handoff,
}

/// What the host does after a turn of a chain.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum ChainStep {
    /// Start the next turn once the commit landed.
    Continue,
    /// The chain ends here, with this outcome.
    End(ChainOutcome),
    /// The chain goes on in the cloud (ADR-0045 decision 8): no local
    /// continuation, and the commit carries no outcome.
    Handoff,
}

/// The chain's next step after a turn that ended with `end`.
#[must_use]
pub const fn next_step(grant: &UnattendedGrant, end: TurnEnd) -> ChainStep {
    match end {
        TurnEnd::Answered => ChainStep::End(ChainOutcome::Completed),
        TurnEnd::Failed => ChainStep::End(ChainOutcome::Failed),
        TurnEnd::Handoff => ChainStep::Handoff,
        TurnEnd::ToolCallCap if grant.continue_tool_call_cap => ChainStep::Continue,
        TurnEnd::StepLimit { exhausted: true } if grant.continue_step_limit => ChainStep::Continue,
        TurnEnd::ToolCallCap | TurnEnd::StepLimit { .. } => ChainStep::End(ChainOutcome::Waiting),
    }
}

/// The next turn of `turn`'s chain, after `request` started as `started`,
/// for `kind`: the same thread, agent, mode and grant, from the runtime's
/// `CONTINUE_PROMPT`, restating the chain's brief when it is ready
/// ([`super::goal::continue_text`]); nothing the person picked for the
/// first turn (skills, a command, images, "@" files) is applied again.
#[must_use]
pub fn continuation(
    request: &TurnRequest,
    turn: &UnattendedTurn,
    started: &LocalTurnStarted,
    kind: ContinuationKind,
) -> TurnRequest {
    let prompt = turn.goal().map_or_else(
        || elitea_agent_runtime::step_limit::CONTINUE_PROMPT.to_owned(),
        |goal| goal.continue_text(),
    );
    let root = started
        .unattended
        .as_ref()
        .and_then(|chain| chain.chain_root_execution_id.as_deref())
        .unwrap_or(&started.execution_id);
    TurnRequest {
        workspace_id: request.workspace_id.clone(),
        project_id: request.project_id,
        conversation_id: request.conversation_id.clone(),
        application_id: request.application_id,
        version_id: request.version_id,
        prompt,
        plan_mode: false,
        bypass: request.bypass,
        mentions: Vec::new(),
        skills: Vec::new(),
        compact: false,
        model: None,
        project_skills: Vec::new(),
        command: None,
        images: Vec::new(),
        unattended: Some(UnattendedTurn {
            continuation_index: started
                .unattended
                .as_ref()
                .and_then(|chain| chain.continuation_index)
                .unwrap_or(turn.continuation_index)
                .saturating_add(1),
            kind: Some(kind),
            ..turn.next(root)
        }),
    }
}

/// The `status` `running` payload of an unattended turn: the grant's
/// switches and the chain's effective limits (`unattended`), and for a turn
/// the host started by itself `continued_automatically`,
/// `continuation_index` and `host_prompt` (the turn's `prompt`).
#[must_use]
pub fn running_status(
    turn: &UnattendedTurn,
    started: Option<&StartedChain>,
    prompt: &str,
) -> Value {
    let mut shown = turn.grant.shown();
    if let Some(chain) = started.and_then(|chain| chain.chain.clone()) {
        shown["chain"] = chain;
    }
    let mut payload = json!({ "unattended": shown });
    let index = started
        .and_then(|chain| chain.continuation_index)
        .unwrap_or(turn.continuation_index);
    if turn.chain_root_execution_id.is_some() {
        payload["continued_automatically"] = json!(true);
        payload["continuation_index"] = json!(index);
        payload["host_prompt"] = json!(prompt);
        if let Some(kind) = turn.kind {
            payload["continuation_kind"] = json!(kind.as_str());
        }
    }
    payload
}

/// Send `chain_ended` on the chain's last turn (after its `done`), with
/// the chain `limit` that refused the next turn, if any, and the last goal
/// check's summary (`goal`, ADR-0045 decision 11).
pub fn chain_ended(
    events: &TurnEvents,
    outcome: ChainOutcome,
    limit: Option<&str>,
    message: Option<&str>,
    goal: Option<Value>,
) {
    let mut payload = json!({ "outcome": outcome.as_str() });
    if let Some(limit) = limit {
        payload["limit"] = json!(limit);
    }
    if let Some(message) = message {
        payload["message"] = json!(message);
    }
    if let Some(goal) = goal {
        payload["goal"] = goal;
    }
    events.send("chain_ended", payload);
}

/// Ask Main for the brief of the chain rooted at `root`, in the background
/// (the chain's first turn never waits for it): the chain keeps it, and
/// the first turn's `events` say `run_brief`. Any error is a `failed`
/// brief.
pub fn request_brief(
    goal: Arc<ChainGoal>,
    api: Arc<PlatformApi>,
    project_id: i64,
    root: String,
    request: &str,
    events: Arc<TurnEvents>,
) {
    goal.set_request(request);
    let request = goal::head(request, goal::EXCERPT_BYTES).to_owned();
    tokio::spawn(async move {
        let answer = match api.run_brief(project_id, &root, &request).await {
            Ok(answer) => answer.checked(),
            // An older Main has no route: no brief, no check, no notice.
            Err(error) if error.status == Some(404) => BriefAnswer::off(),
            Err(error) => {
                log::warn!("a run brief was not made: {}", error.code);
                BriefAnswer::failed()
            }
        };
        let payload = answer.event(events.turn_id(), &root);
        goal.settle(answer);
        events.send("run_brief", payload);
    });
}

/// The goal step of a turn that would end its chain `completed`
/// (ADR-0045 decision 11): `next` is the continuation it starts when a
/// condition is not met.
pub struct GoalStep {
    pub goal: Arc<ChainGoal>,
    pub chain_root_execution_id: String,
    pub next: TurnRequest,
}

/// What the goal step decided.
pub enum GoalDecision {
    /// The chain ends, with the check's summary (`chain_ended.goal`) when
    /// one was made.
    End(ChainOutcome, Option<Value>),
    /// The chain continues with this turn (link kind `goal_not_met`).
    Continue(Box<TurnRequest>),
}

impl GoalStep {
    /// Wait for the brief (at most [`goal::BRIEF_WAIT`]), have Main check
    /// the chain's transcript, and say `goal_checked` on `events`. A brief
    /// with nothing to check ends the chain as before; a brief that failed
    /// or came too late, or a check that failed, is `not_checked`.
    pub async fn decide(
        self,
        api: &PlatformApi,
        project_id: i64,
        execution_id: &str,
        events: &TurnEvents,
    ) -> GoalDecision {
        let brief = self.goal.settled_within(goal::BRIEF_WAIT).await;
        let check = match &brief {
            None => GoalCheck::not_checked("The run brief was not ready in time."),
            Some(answer) if answer.state == "failed" => {
                GoalCheck::not_checked("The run brief could not be written.")
            }
            Some(answer)
                if answer
                    .ready()
                    .is_some_and(|brief| !brief.done_when.is_empty()) =>
            {
                match api
                    .run_check(
                        project_id,
                        &self.chain_root_execution_id,
                        execution_id,
                        &self.goal.excerpt(),
                    )
                    .await
                {
                    Ok(check) => check,
                    Err(error) => {
                        log::warn!("a goal check was not made: {}", error.code);
                        GoalCheck::not_checked("The goal check did not answer.")
                    }
                }
            }
            Some(_) => return GoalDecision::End(ChainOutcome::Completed, None),
        };
        events.send("goal_checked", check.event(events.turn_id()));
        let summary = Some(check.summary());
        let outcome = check.outcome();
        let continue_text = check.continue_text.clone().filter(|t| !t.trim().is_empty());
        self.goal.set_last_check(check);
        match outcome {
            GoalOutcome::Met | GoalOutcome::NotChecked => {
                GoalDecision::End(ChainOutcome::Completed, summary)
            }
            GoalOutcome::Blocked => GoalDecision::End(ChainOutcome::Failed, summary),
            GoalOutcome::NotMet => {
                let mut next = self.next;
                next.prompt = continue_text.unwrap_or_else(|| self.goal.continue_text());
                GoalDecision::Continue(Box::new(next))
            }
        }
    }
}

/// The HITL exchange of a "Continue?" that the grant answered at the step
/// limit of `steps` steps.
#[must_use]
pub fn granted_continue(steps: u32) -> Value {
    json!({
        "interrupt_id": uuid::Uuid::new_v4().to_string(),
        "kind": elitea_agent_runtime::step_limit::CONTINUE_KIND,
        "prompt": format!("The agent used its {steps} steps"),
        "decision": "approve",
        "source": SOURCE_GRANT,
    })
}

/// The wait of a question of `kind` that carries none: past it the
/// question's default answers. `None`: no wait (a kind not listed).
#[must_use]
pub fn default_wait(kind: &str) -> Option<Duration> {
    match kind {
        "command_approval" => Some(RULE_ASK_WAIT),
        "tool_confirmation" => Some(REMOTE_CONFIRMATION_WAIT),
        elitea_agent_runtime::step_limit::CONTINUE_KIND => {
            Some(elitea_agent_runtime::step_limit::CONTINUE_TIMEOUT)
        }
        _ => None,
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn grant(value: Value) -> Result<UnattendedGrant, TurnError> {
        serde_json::from_value::<UnattendedGrant>(value)
            .map_err(|e| TurnError::new("invalid_request", e.to_string()))?
            .admit()
    }

    #[test]
    fn browser_submits_are_host_names_deduplicated_and_bounded() {
        assert!(
            grant(json!({ "browser_submits": ["123.example.com", "a1.example.c0m"] })).is_ok(),
            "digits are fine in any label but the last"
        );
        let made = grant(json!({
            "browser_submits": ["Shop.Example.com.", "shop.example.com", "docs.example.org"]
        }))
        .expect("grant");
        assert_eq!(
            made.browser_submits,
            ["shop.example.com", "docs.example.org"]
        );
        assert_eq!(made.rule_answers().browser_submits, made.browser_submits);
        assert_eq!(made.shown()["browser_submits"], json!(made.browser_submits));
        let wire = UnattendedTurn::root(made).wire();
        assert_eq!(
            wire["browser_submits"],
            json!(["shop.example.com", "docs.example.org"])
        );
        assert!(
            UnattendedTurn::root(grant(json!({})).expect("grant"))
                .wire()
                .get("browser_submits")
                .is_none(),
            "absent stays absent on the wire"
        );
        for bad in [
            json!({ "browser_submits": ["*.example.com"] }),
            json!({ "browser_submits": ["https://example.com"] }),
            json!({ "browser_submits": ["example.com:443"] }),
            json!({ "browser_submits": ["10.0.0.1"] }),
            json!({ "browser_submits": ["localhost"] }),
            // The platform refuses a last label of digits: so does the
            // desktop (finding 7).
            json!({ "browser_submits": ["example.123"] }),
            json!({ "browser_submits": ["shop.example.0"] }),
            json!({ "browser_submits": "example.com" }),
        ] {
            assert!(grant(bad.clone()).is_err(), "{bad}");
        }
        let many: Vec<String> = (0..=MAX_BROWSER_SUBMITS)
            .map(|i| format!("h{i}.example.com"))
            .collect();
        assert!(grant(json!({ "browser_submits": many })).is_err());
    }

    #[test]
    fn a_grant_gets_an_id_and_its_limits_are_checked() {
        let made = grant(json!({ "hook_asks": true })).expect("grant");
        assert!(uuid::Uuid::parse_str(made.id()).is_ok());
        assert!(made.hook_asks && !made.continue_step_limit);
        let kept =
            grant(json!({ "grant_id": "6F9619FF-8B86-D011-B42D-00C04FC964FF" })).expect("grant");
        assert_eq!(kept.id(), "6f9619ff-8b86-d011-b42d-00c04fc964ff");
        for bad in [
            json!({ "grant_id": "x" }),
            json!({ "chain": { "max_continuations": -1 } }),
            json!({ "chain": { "max_continuations": 1.5 } }),
            json!({ "chain": { "deadline_seconds": 0 } }),
            json!({ "chain": { "max_spend": 0 } }),
            json!({ "chain": { "max_spend": -1 } }),
            json!({ "other": true }),
        ] {
            assert_eq!(grant(bad).expect_err("refused").code, "invalid_request");
        }
        // The platform owns the maximums: the host sends large values on.
        let large = grant(json!({ "chain": {
            "max_continuations": 100_000, "deadline_seconds": 30 * 24 * 3600, "max_spend": 5_000_000.0
        } }))
        .expect("grant");
        assert_eq!(large.chain.and_then(|c| c.max_continuations), Some(100_000));
    }

    #[test]
    fn a_refused_grant_reads_well() {
        assert!(
            refusal_message(Some("chain.max_continuations"), Some("over_maximum"))
                .contains("number of continuations is higher than the platform allows")
        );
        assert!(refusal_message(Some("chain.max_spend"), Some("invalid")).contains("spend limit"));
        assert_eq!(
            refusal_message(Some("unattended"), Some("disabled")),
            "Unattended runs are turned off on this platform."
        );
        assert!(refusal_message(None, Some("not_owner")).contains("not_owner"));
        let empty = grant(json!({ "chain": {} })).expect("grant");
        assert_eq!(empty.chain, None);
    }

    #[test]
    fn the_wire_names_the_chain_root_on_every_continuation() {
        let root = UnattendedTurn::root(
            grant(json!({
                "continue_step_limit": true,
                "chain": { "max_continuations": 2, "max_spend": 1.5 }
            }))
            .expect("grant"),
        );
        let wire = root.wire();
        assert!(wire.get("chain_root_execution_id").is_none());
        assert_eq!(
            wire["chain"],
            json!({ "max_continuations": 2, "max_spend": 1.5 })
        );
        assert_eq!(wire["continue_step_limit"], true);
        let first = root.next("exec-1");
        let second = first.next("exec-2");
        assert_eq!(first.continuation_index, 1);
        assert_eq!(second.continuation_index, 2);
        assert_eq!(second.wire()["chain_root_execution_id"], "exec-1");
        assert_eq!(second.wire()["grant_id"], root.wire()["grant_id"]);
    }

    #[test]
    fn only_a_covered_cap_continues_the_chain() {
        let none = UnattendedGrant::default();
        let all = UnattendedGrant {
            continue_step_limit: true,
            continue_tool_call_cap: true,
            ..UnattendedGrant::default()
        };
        assert_eq!(
            next_step(&all, TurnEnd::Answered),
            ChainStep::End(ChainOutcome::Completed)
        );
        assert_eq!(
            next_step(&all, TurnEnd::Failed),
            ChainStep::End(ChainOutcome::Failed)
        );
        assert_eq!(next_step(&all, TurnEnd::ToolCallCap), ChainStep::Continue);
        assert_eq!(
            next_step(&all, TurnEnd::StepLimit { exhausted: true }),
            ChainStep::Continue
        );
        assert_eq!(
            next_step(&all, TurnEnd::StepLimit { exhausted: false }),
            ChainStep::End(ChainOutcome::Waiting)
        );
        assert_eq!(
            next_step(&none, TurnEnd::ToolCallCap),
            ChainStep::End(ChainOutcome::Waiting)
        );
        assert_eq!(
            next_step(&none, TurnEnd::StepLimit { exhausted: true }),
            ChainStep::End(ChainOutcome::Waiting)
        );
        // A turn stopped for a send goes on in the cloud, under any grant.
        assert_eq!(next_step(&none, TurnEnd::Handoff), ChainStep::Handoff);
        assert_eq!(next_step(&all, TurnEnd::Handoff), ChainStep::Handoff);
        assert_eq!(all.step_policy(), ContinuePolicy::Granted);
        assert_eq!(none.step_policy(), ContinuePolicy::Ask);
        assert_eq!(
            serde_json::to_value(ChainOutcome::LimitReached).expect("json"),
            "limit_reached"
        );
    }

    #[test]
    fn every_local_question_has_a_wait() {
        assert_eq!(default_wait("command_approval"), Some(RULE_ASK_WAIT));
        assert_eq!(
            default_wait("tool_confirmation"),
            Some(REMOTE_CONFIRMATION_WAIT)
        );
        assert_eq!(
            default_wait("continue"),
            Some(elitea_agent_runtime::step_limit::CONTINUE_TIMEOUT)
        );
        assert_eq!(default_wait("mcp_authorization"), None);
    }
}

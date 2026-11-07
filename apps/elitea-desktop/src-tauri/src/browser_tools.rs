//! The browser tools for Local work threads (ADR-0048 decisions 3, 5 and 6):
//! the contract's tools (`elitea-browser-tools`, [`TOOLS`]) as a toolset of
//! their own, next to the local, index and MCP toolsets of a turn, never in
//! the workspace tools (`WORKSPACE_TOOLS`, the workspace runner's `TOOLS`).
//!
//! Each tool call goes to the thread's pane driver ([`BrowserDrivers`]): on
//! macOS the WKWebView driver of `browser-driver-macos`, attached when the
//! pane's child webview is made (`browser_pane.rs`). A thread whose pane is
//! not open answers `unsupported` with a hint to open it (the first
//! `browser_open` opening the pane is #1591's, with the control pill).
//!
//! Every call is first decided by the session's rules engine
//! ([`BrowserTool::approve`], ADR-0048 decision 8): the call is described
//! as an `elitea_local_tools::approvals::ToolCall` of kind `BrowserRead`,
//! `BrowserNavigate` or `BrowserAct` with its [`BrowserTarget`], and goes
//! through the session's approval channel. Before deciding, the host reads
//! the live pane ([`Live`]): the address it shows now (the host the ceiling
//! and the approval use; a ref act on another site than the latest
//! snapshot's is `stale_ref`), the focused element for `browser_press`, and
//! for a ref click the element's input type and `download` attribute. A
//! download is checked first ([`effect_of`]); an upload is a file input or
//! an upload/attach control, never a link. A submit or an upload is asked
//! with "allow for this site in this thread" (for that effect), a download
//! every time, a buy, pay, delete, send or sign control every time; plan
//! mode denies navigating and acting, and an unattended grant's
//! `browser_submits` answers a submit or an upload on its hosts. The
//! classifier is the cloud's (`classify_browser_call_with_focus`, ADR-0049
//! decision 3), so both hosts ask the same questions; it fails closed (a
//! ref it cannot resolve, or Enter/Space on a focus nobody could read,
//! asks every time).
//!
//! Control (ADR-0048 decision 4, #1591): each pane has a
//! [`crate::browser_control::PaneControl`]. A turn's first call makes the
//! pill say Agent; while the person drives, a call answers
//! `user_in_control` (snapshots and screenshots run when they ticked **Let
//! the agent watch**), and a call during which they used the page answers
//! `user_active` as soon as the input arrives. `browser_request_user` hands
//! the page to them through the driver's hand-over; a note they hand back
//! with goes to the turn's steering queue, the person's message. Each call
//! is a step record (`browser_step`, ADR-0048 decision 9): the host, the URL
//! path, the ref's role and name, the outcome; typed text only as its length
//! and the turn's keyed digest.
//!
//! Downloads: from a turn's first browser call until the turn ends, the
//! pane's downloads are the agent's ([`BrowserDrivers::download_requested`]):
//! one starts only when the call that caused it was approved as a download.
//!
//! The toolset is offered only while the client policy's
//! `local_work_browser` and `local_work_browser_agent` are on (`d0::turn`);
//! the rules engine denies every call when they are off as well.

use std::collections::HashMap;
use std::sync::atomic::Ordering;
use std::sync::{Arc, Mutex};

use adk_core::{ReadonlyContext, Tool, ToolContext, Toolset};
use async_trait::async_trait;
use base64::Engine as _;
use elitea_agent_runtime::host::{
    ApprovalOutcome, ApprovalRequest, HostError, ToolProvider, ToolsetRequest,
};
use elitea_agent_runtime::image_result::{IMAGE_FIELD, IMAGE_KIND};
use elitea_agent_runtime::toolkits::{
    BrowserEscalation, BrowserFocus, classify_browser_call_with_focus,
};
use elitea_browser_tools::driver::ToolOutput;
use elitea_browser_tools::{
    BrowserCall, BrowserDriver, BrowserError, ControlInfo, ErrorCode, Snapshot, TOOLS, ToolKind,
    ToolSpec, dispatch,
};
use elitea_local_tools::approvals::{
    self, BrowserEffect, BrowserTarget, ComputerControl, PAYLOAD_KEY, ToolCall,
};
use elitea_local_tools::session::LocalSession;
use serde_json::{Value, json};

use crate::browser_control::{ControlView, NoteSink, PaneControl};
use crate::d0::recorder::Recorder;

/// The toolset's name.
pub const TOOLSET_NAME: &str = "browser";

/// The live pane drivers, by thread (conversation id), and what an agent
/// turn driving a pane allowed of its downloads.
#[derive(Default)]
pub struct BrowserDrivers {
    drivers: Mutex<HashMap<String, Arc<dyn BrowserDriver>>>,
    drives: Mutex<HashMap<String, Drive>>,
    /// Who drives each thread's pane (made on first use, kept for the app
    /// session: the pill outlives a closed page).
    controls: Mutex<HashMap<String, Arc<PaneControl>>>,
    /// Who hears a pane's control change (the pane's UI).
    notify: Mutex<Option<ControlNotify>>,
}

/// Hears a thread's control change: the thread and its new view.
pub type ControlNotify = Arc<dyn Fn(&str, &ControlView) + Send + Sync>;

/// An agent turn driving a thread's pane (from its first browser call to
/// the end of the turn): the pane's downloads are the agent's then
/// (ADR-0048 decision 8).
#[derive(Default)]
struct Drive {
    /// The turn's page (one per turn's toolset): the drive ends when the
    /// turn's tools are gone.
    owner: std::sync::Weak<Mutex<Page>>,
    /// The latest call was approved as a download: one download may start.
    download_approved: bool,
    /// The latest call's control, as `(host, role, name)`.
    control: Option<(String, String, String)>,
    /// Controls that started a download nobody approved: the next act on
    /// one is asked as a download.
    learned: Vec<(String, String, String)>,
    /// The latest call started a download that was refused.
    blocked: bool,
}

/// What the pane does with a download it was asked to start
/// ([`BrowserDrivers::download_requested`]).
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum DownloadVerdict {
    /// No agent turn drives the pane: the person's own download.
    Person,
    /// The agent's latest call was approved as this download.
    Approved,
    /// An agent turn drives the pane and nobody approved a download.
    Refused,
}

impl BrowserDrivers {
    #[must_use]
    pub fn new() -> Self {
        Self::default()
    }

    fn lock(&self) -> std::sync::MutexGuard<'_, HashMap<String, Arc<dyn BrowserDriver>>> {
        self.drivers
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
    }

    /// The pane of `thread` was made: its driver.
    pub fn attach(&self, thread: &str, driver: Arc<dyn BrowserDriver>) {
        self.lock().insert(thread.to_owned(), driver);
    }

    /// The pane of `thread` closed.
    pub fn detach(&self, thread: &str) {
        self.lock().remove(thread);
    }

    /// Every pane closed (sign-out).
    pub fn detach_all(&self) {
        self.lock().clear();
        self.drives().clear();
        let controls: Vec<_> = self
            .controls
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
            .drain()
            .map(|(_, control)| control)
            .collect();
        for control in controls {
            control.agent_ended();
        }
    }

    /// Who hears every pane's control change from now on.
    pub fn set_notify(&self, notify: ControlNotify) {
        *self
            .notify
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner) = Some(notify);
    }

    /// The control of `thread`'s pane (made on first use).
    #[must_use]
    pub fn control(&self, thread: &str) -> Arc<PaneControl> {
        let mut controls = self
            .controls
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner);
        if let Some(control) = controls.get(thread) {
            return control.clone();
        }
        let control = Arc::new(PaneControl::default());
        let notify = self
            .notify
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
            .clone();
        if let Some(notify) = notify {
            let thread = thread.to_owned();
            control.set_notify(Arc::new(move |view: &ControlView| notify(&thread, view)));
        }
        controls.insert(thread.to_owned(), control.clone());
        control
    }

    #[must_use]
    pub fn get(&self, thread: &str) -> Option<Arc<dyn BrowserDriver>> {
        self.lock().get(thread).cloned()
    }

    fn drives(&self) -> std::sync::MutexGuard<'_, HashMap<String, Drive>> {
        self.drives
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
    }

    /// A call of the turn's toolset `owner` is about to run on the pane:
    /// the pane's downloads are the agent's until the turn ends.
    fn begin_call(
        &self,
        thread: &str,
        owner: &SharedPage,
        control: Option<(String, String, String)>,
        download_approved: bool,
    ) {
        let mut drives = self.drives();
        let drive = drives.entry(thread.to_owned()).or_default();
        if !std::ptr::eq(drive.owner.as_ptr(), Arc::as_ptr(owner)) {
            *drive = Drive {
                owner: Arc::downgrade(owner),
                ..Drive::default()
            };
        }
        drive.control = control;
        drive.download_approved = download_approved;
        drive.blocked = false;
    }

    /// Whether the latest call started a download that was refused.
    fn take_blocked(&self, thread: &str) -> bool {
        self.drives()
            .get_mut(thread)
            .is_some_and(|drive| std::mem::take(&mut drive.blocked))
    }

    /// Whether `control` started a refused download in this turn.
    fn learned_download(&self, thread: &str, control: &(String, String, String)) -> bool {
        self.drives()
            .get(thread)
            .is_some_and(|drive| drive.learned.contains(control))
    }

    /// The turn whose page is `owner` ended.
    fn end_turn(&self, thread: &str, owner: &SharedPage) {
        let mut drives = self.drives();
        if drives
            .get(thread)
            .is_some_and(|drive| std::ptr::eq(drive.owner.as_ptr(), Arc::as_ptr(owner)))
        {
            drives.remove(thread);
        }
    }

    /// The pane of `thread` was asked to start a download: the person's
    /// when no agent turn drives it; the agent's only when its latest call
    /// was approved as a download (once). A refused one is remembered for
    /// its control, so the agent's next try is asked.
    pub fn download_requested(&self, thread: &str) -> DownloadVerdict {
        let mut drives = self.drives();
        if drives
            .get(thread)
            .is_some_and(|drive| drive.owner.strong_count() == 0)
        {
            // The turn's tools are gone: it ended.
            drives.remove(thread);
        }
        let Some(drive) = drives.get_mut(thread) else {
            return DownloadVerdict::Person;
        };
        if std::mem::take(&mut drive.download_approved) {
            return DownloadVerdict::Approved;
        }
        drive.blocked = true;
        if let Some(control) = drive.control.clone()
            && !drive.learned.contains(&control)
        {
            drive.learned.push(control);
        }
        DownloadVerdict::Refused
    }
}

/// What the thread's pane last showed the agent: the latest snapshot (the
/// classifier's controls) and the page's URL (the host an act is on).
#[derive(Default)]
struct Page {
    snapshot: Option<Snapshot>,
    url: Option<String>,
    /// The person's input count when the agent last looked
    /// ([`PaneControl::input_epoch`]).
    input_epoch: u64,
    /// The person used the page since the agent's latest snapshot: a ref
    /// act is `stale_ref` until it snapshots again.
    refs_dropped: bool,
}

/// The pages of a toolset's tools, shared by its tools.
type SharedPage = Arc<Mutex<Page>>;

fn host_of(url: &str) -> Option<String> {
    url::Url::parse(url)
        .ok()?
        .host_str()
        .map(|host| host.trim_end_matches('.').to_ascii_lowercase())
}

/// A URL as an ask and the audit show it: no query or fragment.
fn shown(url: &str) -> Option<String> {
    let mut url = url::Url::parse(url).ok()?;
    url.set_query(None);
    url.set_fragment(None);
    Some(url.to_string())
}

/// What the host read of the live pane right before deciding a call
/// (ADR-0048 decision 8), next to the agent's last view of it ([`Page`]).
#[derive(Clone, Debug, Default)]
struct Live {
    /// The address the pane shows now (`None`: the driver could not say).
    url: Option<String>,
    /// For `browser_press`: the focused element (`None`: unknown, so Enter
    /// and Space fail closed; `Some(None)`: nothing has the focus).
    focused: Option<Option<ControlInfo>>,
    /// For a ref act: what the driver read of the element itself (an
    /// input's type, a link's `download` attribute).
    inspected: Option<ControlInfo>,
}

/// Whole words of `name`, split at non-alphanumerics.
fn has_word(name: &str, words: &[&str]) -> bool {
    name.split(|c: char| !c.is_alphanumeric())
        .any(|word| words.iter().any(|w| word.eq_ignore_ascii_case(w)))
}

/// Words that name a download on a link or button.
const DOWNLOAD_WORDS: &[&str] = &["download", "downloads", "export"];
/// Words that name an upload control.
const UPLOAD_WORDS: &[&str] = &["upload", "attach", "attachment", "attachments"];

/// What an act on `role`/`name` starts beyond the page, before the
/// classifier's escalation is read: a download first (a link that
/// downloads is never an upload or a submit), then an upload (only a file
/// input or a control named for uploading, never a link).
fn effect_of(role: &str, name: &str, info: Option<&ControlInfo>) -> Option<BrowserEffect> {
    let link = role.eq_ignore_ascii_case("link");
    if info.is_some_and(|info| info.download)
        || ((link || role.eq_ignore_ascii_case("button")) && has_word(name, DOWNLOAD_WORDS))
    {
        return Some(BrowserEffect::Download);
    }
    if link {
        return None;
    }
    let file_input = info.is_some_and(|info| info.input_type.as_deref() == Some("file"))
        || role.eq_ignore_ascii_case("file")
        || role.eq_ignore_ascii_case("fileupload");
    if file_input || has_word(name, UPLOAD_WORDS) {
        return Some(BrowserEffect::Upload);
    }
    None
}

/// The rules engine's view of one call ([`ToolCall::browser`]): its kind,
/// the live page's host (or the URL it opens), the control and what the
/// act starts. `None` for `browser_request_user` (a question, never ruled)
/// and a call that does not parse (it never reaches the page).
fn rule_call(name: &str, args: &Value, page: &Page, live: &Live) -> Option<ToolCall> {
    let call = BrowserCall::parse(name, args).ok()?;
    let tool = match call.kind() {
        ToolKind::BrowserRead => approvals::ToolKind::BrowserRead,
        ToolKind::BrowserNavigate => approvals::ToolKind::BrowserNavigate,
        ToolKind::BrowserAct => approvals::ToolKind::BrowserAct,
        ToolKind::Question => return None,
    };
    // The page the call acts on is the one the pane shows now, not the
    // agent's last view of it (a redirect, a script, the person).
    let url = match &call {
        BrowserCall::Open { url } => Some(url.clone()),
        _ => live.url.clone().or_else(|| page.url.clone()),
    };
    let mut target = BrowserTarget {
        tool: name.to_owned(),
        host: url.as_deref().and_then(host_of),
        url: url.as_deref().and_then(shown),
        ..BrowserTarget::default()
    };
    if tool == approvals::ToolKind::BrowserAct {
        let element = match &call {
            BrowserCall::Click { target, .. }
            | BrowserCall::Type { target, .. }
            | BrowserCall::Select { target, .. } => Some(target),
            _ => None,
        };
        let node = element.and_then(|element| page.snapshot.as_ref()?.resolve(element).ok());
        let focused = match (&call, &live.focused) {
            (BrowserCall::Press(_), Some(Some(info))) => Some(info),
            _ => None,
        };
        let (role, control_name) = match (node, focused) {
            (Some(node), _) => (Some(node.role.as_str()), Some(node.name.as_str())),
            (None, Some(info)) => (Some(info.role.as_str()), Some(info.name.as_str())),
            (None, None) => (None, None),
        };
        if let (Some(role), Some(control_name)) = (role, control_name) {
            target.control = Some(ComputerControl {
                role: Some(role.to_owned()),
                name: Some(control_name.to_owned()),
                ..ComputerControl::default()
            });
        }
        let focus = match &live.focused {
            None => BrowserFocus::Unknown,
            Some(None) => BrowserFocus::Nothing,
            Some(Some(info)) => BrowserFocus::Control {
                role: &info.role,
                name: &info.name,
            },
        };
        let classified =
            classify_browser_call_with_focus(name, args, page.snapshot.as_ref(), focus);
        // A click (or Enter/Space on the focus) that downloads or uploads is
        // that, whatever submit word its name has; a consequential word or
        // an unknown control still asks every time.
        let acts_on_control = matches!(call, BrowserCall::Click { .. } | BrowserCall::Press(_));
        let effect = match (role, control_name) {
            (Some(role), Some(control_name)) if acts_on_control => {
                let info = if node.is_some() {
                    live.inspected.as_ref()
                } else {
                    focused
                };
                effect_of(role, control_name, info)
            }
            _ => None,
        };
        match classified.escalation {
            Some(BrowserEscalation::Submit) => {
                target.effect = Some(effect.unwrap_or(BrowserEffect::Submit));
            }
            Some(
                BrowserEscalation::Payment
                | BrowserEscalation::Consequential
                | BrowserEscalation::Unresolved,
            ) => {
                // Asked every time, never remembered, no grant: the
                // classifier saw a buy, pay, send, delete or sign word, or
                // could not see the control at all.
                target.consequential = true;
                target.effect = Some(effect.unwrap_or(BrowserEffect::Submit));
            }
            None => target.effect = effect,
        }
    }
    Some(ToolCall {
        browser: Some(target),
        ..ToolCall::new(tool)
    })
}

/// One browser tool of one thread.
struct BrowserTool {
    spec: &'static ToolSpec,
    thread: String,
    drivers: Arc<BrowserDrivers>,
    /// The model can see images: a screenshot carries its bytes.
    vision: bool,
    /// The turn's session: its rules and approval channel
    /// ([`Self::approve`]). `None` only in tests of the dispatch.
    session: Option<Arc<LocalSession>>,
    /// What the pane last showed the agent.
    page: SharedPage,
    /// The turn's: its recorder, its steering queue, and whether it drove
    /// the pane yet.
    turn: Arc<TurnLink>,
}

/// What a turn's browser tools share with the turn.
#[derive(Default)]
struct TurnLink {
    /// Each call's step record (`browser_step`) and the typed-text digest.
    recorder: Option<Arc<Recorder>>,
    /// Where a hand-back note goes: the turn's steering queue.
    notes: Option<Arc<dyn NoteSink>>,
    /// A call of this turn ran on the pane: the pill says Agent until the
    /// turn ends.
    drove: std::sync::atomic::AtomicBool,
}

/// `browser_snapshot` and `browser_screenshot`: what the person may let
/// the agent do while they drive (**Let the agent watch**).
fn watchable(name: &str) -> bool {
    name == elitea_browser_tools::spec::BROWSER_SNAPSHOT
        || name == elitea_browser_tools::spec::BROWSER_SCREENSHOT
}

fn user_in_control() -> BrowserError {
    BrowserError::new(
        ErrorCode::UserInControl,
        "the person is driving the page: wait for them to hand it back, or ask with browser_request_user",
    )
}

fn user_active() -> BrowserError {
    BrowserError::new(
        ErrorCode::UserActive,
        "the person used the page during this call: the agent is paused until they hand it back",
    )
}

#[async_trait]
impl Tool for BrowserTool {
    fn name(&self) -> &str {
        self.spec.name
    }

    fn description(&self) -> &str {
        self.spec.description
    }

    fn parameters_schema(&self) -> Option<Value> {
        Some((self.spec.parameters)())
    }

    fn is_read_only(&self) -> bool {
        self.spec.kind == elitea_browser_tools::ToolKind::BrowserRead
    }

    fn is_concurrency_safe(&self) -> bool {
        // One page: calls run one at a time, in the order the model gave.
        false
    }

    async fn execute(&self, ctx: Arc<dyn ToolContext>, args: Value) -> adk_core::Result<Value> {
        Ok(self.run_call(ctx.function_call_id(), &args).await)
    }
}

impl BrowserTool {
    #[cfg(test)]
    async fn run(&self, args: &Value) -> Value {
        self.run_call("call-1", args).await
    }

    /// What the host reads of the live pane before deciding a call: the
    /// address it shows, and for a press the focused element, for a ref
    /// click the element itself. A read that fails leaves the part unknown
    /// (the rules fail closed on it).
    async fn read_live(&self, driver: Option<&dyn BrowserDriver>, args: &Value) -> Live {
        let Some(driver) = driver else {
            return Live::default();
        };
        let mut live = Live {
            url: driver.current_url().await.ok(),
            ..Live::default()
        };
        match BrowserCall::parse(self.spec.name, args) {
            Ok(BrowserCall::Press(_)) => live.focused = driver.focused().await.ok(),
            Ok(BrowserCall::Click { target, .. }) => {
                live.inspected = driver.inspect(&target).await.ok();
            }
            _ => {}
        }
        live
    }

    /// A ref act on a page that is not the one the agent's latest snapshot
    /// showed: its refs belong to another page (`stale_ref`).
    fn stale_page(&self, args: &Value, live: &Live) -> Result<(), BrowserError> {
        let ref_act = matches!(
            BrowserCall::parse(self.spec.name, args),
            Ok(BrowserCall::Click { .. }
                | BrowserCall::Type { .. }
                | BrowserCall::Select { .. }
                | BrowserCall::Scroll(elitea_browser_tools::call::ScrollTarget::Element(_)))
        );
        if !ref_act {
            return Ok(());
        }
        if self
            .page
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
            .refs_dropped
        {
            return Err(BrowserError::stale_ref(
                "the person used the page since your latest snapshot: snapshot again",
            ));
        }
        let Some(live_host) = live.url.as_deref().and_then(host_of) else {
            return Ok(());
        };
        let mut page = self
            .page
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner);
        let seen = page
            .snapshot
            .as_ref()
            .and_then(|snapshot| host_of(&snapshot.url));
        if seen.is_some_and(|seen| seen != live_host) {
            page.snapshot = None;
            return Err(BrowserError::stale_ref(
                "the pane shows another site than your latest snapshot: snapshot again",
            ));
        }
        Ok(())
    }

    /// The rules engine's answer to this call (ADR-0048 decision 8), through
    /// the session's approval channel: `Ok` with the call as it was decided
    /// (`None`: not ruled), or the tool result the model sees instead.
    async fn approve(
        &self,
        call_id: &str,
        args: &Value,
        live: &Live,
    ) -> Result<Option<ToolCall>, BrowserError> {
        let call = {
            let page = self
                .page
                .lock()
                .unwrap_or_else(std::sync::PoisonError::into_inner);
            rule_call(self.spec.name, args, &page, live)
        };
        let Some(mut call) = call else {
            return Ok(None);
        };
        // A control that started a download nobody approved in this turn is
        // asked as a download from now on.
        if let Some(target) = call.browser.as_mut()
            && let Some(control) = control_key(target)
            && target.effect != Some(BrowserEffect::Download)
            && self.drivers.learned_download(&self.thread, &control)
        {
            target.effect = Some(BrowserEffect::Download);
        }
        let Some(session) = &self.session else {
            return Ok(Some(call));
        };
        let request = ApprovalRequest {
            subject: call_id.to_owned(),
            message: format!("use the browser pane (`{}`)", self.spec.name),
            available_actions: vec!["approve".to_owned(), "reject".to_owned()],
            payload: json!({ PAYLOAD_KEY: call }),
        };
        let outcome = session
            .approvals()
            .request(request)
            .await
            .map_err(|error| {
                BrowserError::new(ErrorCode::Declined, format!("not approved: {error}"))
            })?;
        match outcome {
            ApprovalOutcome::Decided { action, .. } if action == "approve" => Ok(Some(call)),
            ApprovalOutcome::Decided { value, .. } => {
                let reason = value
                    .get("reason")
                    .and_then(Value::as_str)
                    .unwrap_or("the person declined it");
                let code = match value.get("source").and_then(Value::as_str) {
                    Some("plan_mode") => ErrorCode::Unsupported,
                    Some("policy") => ErrorCode::EgressRefused,
                    _ => ErrorCode::Declined,
                };
                Err(BrowserError::new(code, format!("not run: {reason}")))
            }
            ApprovalOutcome::Deferred => Err(BrowserError::new(
                ErrorCode::Declined,
                "not run: nobody could be asked",
            )),
        }
    }

    /// Keep what the call showed of the page for the next call's rules: a
    /// snapshot's controls, the page's URL; a navigation or an act may
    /// change the page, so the classifier forgets the controls.
    fn observe(&self, result: &Value) {
        if result.get("status").and_then(Value::as_str) != Some("ok") {
            return;
        }
        let Some(content) = result
            .get("content")
            .and_then(Value::as_str)
            .and_then(elitea_browser_tools::frame::unframe)
            .map(|(inner, _)| inner)
        else {
            return;
        };
        let url = content
            .lines()
            .find_map(|line| line.strip_prefix("# url "))
            .and_then(|quoted| serde_json::from_str::<String>(quoted).ok());
        let mut page = self
            .page
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner);
        if self.spec.name == elitea_browser_tools::spec::BROWSER_SNAPSHOT
            && result.get("snapshot").is_some()
        {
            page.snapshot = Snapshot::parse(content).ok();
            page.refs_dropped = false;
        } else if self.spec.kind != ToolKind::BrowserRead {
            page.snapshot = None;
        }
        if url.is_some() {
            page.url = url;
        }
    }

    async fn run_call(&self, call_id: &str, args: &Value) -> Value {
        let mut seen = Seen::default();
        let result = self.run_controlled(args, call_id, &mut seen).await;
        if let Some(recorder) = &self.turn.recorder {
            let record = step_record(self.spec.name, args, &seen, &result, |text| {
                recorder.text_digest(text)
            });
            recorder.step_record_under(BROWSER_STEP_KEY, call_id, record);
        }
        result
    }

    /// The pane's control (ADR-0048 decision 4): `user_in_control` while
    /// the person drives, unless the call is a watch they allowed.
    fn gate(&self, control: &PaneControl) -> Result<(), BrowserError> {
        if control.person_drives() && !(watchable(self.spec.name) && control.watching()) {
            return Err(user_in_control());
        }
        Ok(())
    }

    /// Forget the agent's last view of the page when the person used it
    /// since: its refs are not the page's any more (as on a cloud thread).
    fn sync_with_person(&self, control: &PaneControl) {
        let epoch = control.input_epoch();
        let mut page = self
            .page
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner);
        if page.input_epoch != epoch {
            page.input_epoch = epoch;
            if page.snapshot.take().is_some() {
                page.refs_dropped = true;
            }
        }
    }

    fn seen_url(&self) -> Option<String> {
        self.page
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
            .url
            .clone()
    }

    async fn run_controlled(&self, args: &Value, call_id: &str, seen: &mut Seen) -> Value {
        let driver = self.drivers.get(&self.thread);
        let control = self.drivers.control(&self.thread);
        let question = self.spec.kind == ToolKind::Question;
        if driver.is_some() {
            // From the turn's first call on a live pane the pill says Agent.
            if !self.turn.drove.swap(true, Ordering::SeqCst) {
                control.agent_started(self.turn.notes.clone());
            }
            if let Err(error) = self.gate(&control) {
                seen.url = self.seen_url();
                return error.to_result();
            }
            self.sync_with_person(&control);
        }
        let live = self.read_live(driver.as_deref(), args).await;
        seen.url = match BrowserCall::parse(self.spec.name, args) {
            Ok(BrowserCall::Open { url }) => Some(url),
            _ => live.url.clone().or_else(|| self.seen_url()),
        };
        if let Err(error) = self.stale_page(args, &live) {
            return error.to_result();
        }
        let call = match self.approve(call_id, args, &live).await {
            Ok(call) => call,
            Err(error) => return error.to_result(),
        };
        let target = call.as_ref().and_then(|call| call.browser.as_ref());
        seen.control = target
            .and_then(|target| target.control.as_ref())
            .and_then(|control| Some((control.role.clone()?, control.name.clone()?)));
        let Some(driver) = driver else {
            return BrowserError::new(
                ErrorCode::Unsupported,
                "the browser pane of this thread is not open, or the agent cannot drive it: ask the person to open it (Thread > Browser) or to reopen it",
            )
            .to_result();
        };
        // The person may have taken the page while the call was asked.
        if let Err(error) = self.gate(&control) {
            return error.to_result();
        }
        let epoch = control.input_epoch();
        // A watch the person allowed runs beside their input, and the
        // question is theirs to answer. Every other call stops at their
        // first input.
        let pausable = !question && !control.person_drives();
        // From here the pane's downloads are this turn's: one starts only
        // when this call was approved as a download (`browser_pane`).
        self.drivers.begin_call(
            &self.thread,
            &self.page,
            target.and_then(control_key),
            target.is_some_and(|target| target.effect == Some(BrowserEffect::Download)),
        );
        let output = if pausable {
            tokio::select! {
                biased;
                () = control.input_after(epoch) => None,
                output = dispatch(driver.as_ref(), self.spec.name, args) => Some(output),
            }
        } else {
            Some(dispatch(driver.as_ref(), self.spec.name, args).await)
        };
        let refused_download = self.drivers.take_blocked(&self.thread);
        let output = match output {
            Some(output) if !pausable || control.input_epoch() == epoch => output,
            // The person used the page meanwhile: the agent is paused now,
            // whatever the call would have answered.
            _ => {
                self.sync_with_person(&control);
                return user_active().to_result();
            }
        };
        if question {
            // The URL the agent finds on hand-back is the step's (decision 9).
            seen.url = driver.current_url().await.ok().or(seen.url.take());
            // The person had the page: whatever the agent knew of it is stale.
            self.sync_with_person(&control);
        }
        let mut output = output;
        if let ToolOutput::Json(result) = &mut output {
            self.observe(result);
            if refused_download && result.is_object() {
                result["download"] = json!(
                    "refused: nobody approved a download; act on the same control again to be asked"
                );
            }
        }
        tool_result(output, self.vision)
    }
}

/// The key of a browser call's step record on its tool step.
pub const BROWSER_STEP_KEY: &str = "browser_step";

/// What a call showed of the page, for its step record.
#[derive(Default)]
struct Seen {
    /// The page the call acted on (the URL it opens, for `browser_open`).
    url: Option<String>,
    /// The control's role and name, for an act on one.
    control: Option<(String, String)>,
}

/// The step record of one browser call (ADR-0048 decision 9, ADR-0042):
/// the tool, the page's host and URL path (no query, no fragment), the ref
/// and its role and name, and the outcome. Typed text is its length and the
/// turn's keyed digest, never the text; a screenshot is its size only (its
/// bytes went to the model and stay on the device).
fn step_record(
    tool: &str,
    args: &Value,
    seen: &Seen,
    result: &Value,
    text_digest: impl Fn(&str) -> String,
) -> Value {
    let outcome = if result.get("status").and_then(Value::as_str) == Some("ok") {
        "ok"
    } else {
        result
            .get("code")
            .and_then(Value::as_str)
            .unwrap_or("error")
    };
    let mut record = json!({ "v": 1, "tool": tool, "outcome": outcome });
    if let Some(url) = seen
        .url
        .as_deref()
        .and_then(|url| url::Url::parse(url).ok())
    {
        if let Some(host) = url.host_str() {
            record["host"] = json!(host.trim_end_matches('.').to_ascii_lowercase());
        }
        let path: String = url.path().chars().take(512).collect();
        record["path"] = json!(path);
    }
    if let Some(target) = args.get("ref").and_then(Value::as_str) {
        record["ref"] = json!(target.chars().take(64).collect::<String>());
    }
    if let Some((role, name)) = &seen.control {
        record["role"] = json!(role.chars().take(64).collect::<String>());
        record["name"] = json!(elitea_agent_runtime::text::clip_bytes(name, 512));
    }
    if tool == elitea_browser_tools::spec::BROWSER_TYPE
        && let Some(text) = args.get("text").and_then(Value::as_str)
    {
        record["text"] = json!({ "chars": text.chars().count(), "digest": text_digest(text) });
    }
    if result.get("kind").and_then(Value::as_str) == Some(IMAGE_KIND) {
        record["screenshot"] = json!({
            "taken": true,
            "mime": result.get("mime").cloned().unwrap_or(Value::Null),
            "bytes": result.get("bytes").cloned().unwrap_or(Value::Null),
            "width": result.get("width").cloned().unwrap_or(Value::Null),
            "height": result.get("height").cloned().unwrap_or(Value::Null),
        });
    }
    record
}

/// A browser act's control as the turn's download memory keys it: the
/// page's host, the control's role and name.
fn control_key(target: &BrowserTarget) -> Option<(String, String, String)> {
    let control = target.control.as_ref()?;
    Some((
        target.host.clone()?,
        control.role.clone()?.to_ascii_lowercase(),
        control.name.clone()?,
    ))
}

/// A browser result as the runtime carries a tool result: JSON, and a
/// screenshot as the runtime's image result when the model can see it.
fn tool_result(output: ToolOutput, vision: bool) -> Value {
    match output {
        ToolOutput::Json(result) => result,
        ToolOutput::Image {
            media_type,
            data,
            mut result,
        } => {
            result["kind"] = json!(IMAGE_KIND);
            result["mime"] = json!(media_type);
            result["bytes"] = json!(data.len());
            if vision {
                result[IMAGE_FIELD] =
                    json!(base64::engine::general_purpose::STANDARD.encode(&data));
            } else {
                result["hint"] = json!("This model cannot see images: use browser_snapshot.");
            }
            result
        }
    }
}

/// The browser toolset of one thread.
pub struct BrowserToolset {
    thread: String,
    drivers: Arc<BrowserDrivers>,
    vision: bool,
    session: Option<Arc<LocalSession>>,
    page: SharedPage,
    turn: Arc<TurnLink>,
}

impl Drop for BrowserToolset {
    /// The turn ended: the pane's downloads are the person's again, and
    /// the pill goes back to Idle.
    fn drop(&mut self) {
        self.drivers.end_turn(&self.thread, &self.page);
        if self.turn.drove.load(Ordering::SeqCst) {
            self.drivers.control(&self.thread).agent_ended();
        }
    }
}

#[async_trait]
impl Toolset for BrowserToolset {
    fn name(&self) -> &str {
        TOOLSET_NAME
    }

    async fn tools(&self, _ctx: Arc<dyn ReadonlyContext>) -> adk_core::Result<Vec<Arc<dyn Tool>>> {
        Ok(self.current())
    }
}

impl BrowserToolset {
    fn current(&self) -> Vec<Arc<dyn Tool>> {
        TOOLS
            .iter()
            .map(|spec| {
                Arc::new(BrowserTool {
                    spec,
                    thread: self.thread.clone(),
                    drivers: self.drivers.clone(),
                    vision: self.vision,
                    session: self.session.clone(),
                    page: self.page.clone(),
                    turn: self.turn.clone(),
                }) as Arc<dyn Tool>
            })
            .collect()
    }
}

/// The browser toolset as a turn's [`ToolProvider`].
pub struct BrowserToolProvider {
    toolset: Arc<BrowserToolset>,
}

impl BrowserToolProvider {
    #[must_use]
    pub fn new(
        thread: &str,
        drivers: Arc<BrowserDrivers>,
        vision: bool,
        session: Arc<LocalSession>,
    ) -> Self {
        Self {
            toolset: Arc::new(BrowserToolset {
                thread: thread.to_owned(),
                drivers,
                vision,
                session: Some(session),
                page: SharedPage::default(),
                turn: Arc::default(),
            }),
        }
    }

    /// The turn's recorder (each call's `browser_step`) and steering queue
    /// (a hand-back note, the person's message).
    #[must_use]
    pub fn with_turn(mut self, recorder: Arc<Recorder>, notes: Arc<dyn NoteSink>) -> Self {
        if let Some(toolset) = Arc::get_mut(&mut self.toolset) {
            toolset.turn = Arc::new(TurnLink {
                recorder: Some(recorder),
                notes: Some(notes),
                drove: std::sync::atomic::AtomicBool::new(false),
            });
        }
        self
    }
}

#[async_trait]
impl ToolProvider for BrowserToolProvider {
    async fn toolsets(
        &self,
        _request: &ToolsetRequest,
    ) -> Result<Vec<Arc<dyn Toolset>>, HostError> {
        Ok(vec![self.toolset.clone() as Arc<dyn Toolset>])
    }
}

#[cfg(test)]
mod tests {
    use std::time::Duration;

    use elitea_browser_tools::call::{ElementRef, Key, ScrollTarget, WaitCondition};
    use elitea_browser_tools::conformance;
    use elitea_browser_tools::driver::{Handback, HistoryStep, PageInfo, PageText, Screenshot};
    use elitea_browser_tools::snapshot::Snapshot;

    use super::*;
    use crate::browser_control::Pill;

    #[test]
    fn the_host_exposes_exactly_the_contract() {
        let toolset = BrowserToolset {
            thread: "t1".to_owned(),
            drivers: Arc::new(BrowserDrivers::new()),
            vision: true,
            session: None,
            page: SharedPage::default(),
            turn: Arc::default(),
        };
        let exposed: Vec<(String, String, Value)> = toolset
            .current()
            .iter()
            .map(|tool| {
                (
                    tool.name().to_owned(),
                    tool.description().to_owned(),
                    tool.parameters_schema().unwrap_or(Value::Null),
                )
            })
            .collect();
        conformance::check_exposed(&exposed).unwrap();
        // Never a workspace tool.
        for (name, ..) in &exposed {
            assert!(
                !elitea_local_tools::session::TOOLS
                    .iter()
                    .any(|spec| spec.name == name),
                "{name}"
            );
        }
    }

    #[tokio::test]
    async fn a_thread_without_a_pane_is_told_to_open_it() {
        let tool = BrowserTool {
            spec: &TOOLS[0],
            thread: "t1".to_owned(),
            drivers: Arc::new(BrowserDrivers::new()),
            vision: false,
            session: None,
            page: SharedPage::default(),
            turn: Arc::default(),
        };
        let result = tool.run(&json!({ "url": "https://example.com/" })).await;
        assert_eq!(result["code"], "unsupported");
        assert!(result["message"].as_str().unwrap().contains("Browser"));
    }

    struct Shot;

    #[async_trait]
    impl BrowserDriver for Shot {
        async fn open(&self, _url: &str) -> Result<PageInfo, BrowserError> {
            Ok(PageInfo::default())
        }
        async fn snapshot(&self, _max: usize) -> Result<Snapshot, BrowserError> {
            Err(BrowserError::new(ErrorCode::Internal, "no"))
        }
        async fn text(&self, _max: usize) -> Result<PageText, BrowserError> {
            Ok(PageText::default())
        }
        async fn screenshot(&self, _t: Option<&ElementRef>) -> Result<Screenshot, BrowserError> {
            Ok(Screenshot {
                media_type: "image/png",
                data: vec![1, 2, 3],
                width: 2,
                height: 1,
            })
        }
        async fn click(&self, _t: &ElementRef, _d: bool) -> Result<PageInfo, BrowserError> {
            Ok(PageInfo::default())
        }
        async fn type_text(
            &self,
            _t: &ElementRef,
            _x: &str,
            _c: bool,
            _s: bool,
        ) -> Result<PageInfo, BrowserError> {
            Ok(PageInfo::default())
        }
        async fn select(&self, _t: &ElementRef, _v: &[String]) -> Result<PageInfo, BrowserError> {
            Ok(PageInfo::default())
        }
        async fn scroll(&self, _t: &ScrollTarget) -> Result<PageInfo, BrowserError> {
            Ok(PageInfo::default())
        }
        async fn press(&self, _k: &Key) -> Result<PageInfo, BrowserError> {
            Ok(PageInfo::default())
        }
        async fn history(&self, _s: HistoryStep) -> Result<PageInfo, BrowserError> {
            Ok(PageInfo::default())
        }
        async fn wait(&self, _c: &WaitCondition, _t: Duration) -> Result<PageInfo, BrowserError> {
            Ok(PageInfo::default())
        }
        async fn request_user(&self, _r: &str) -> Result<Handback, BrowserError> {
            Err(BrowserError::new(ErrorCode::NoPerson, "nobody"))
        }
    }

    #[tokio::test]
    async fn a_screenshot_is_the_runtimes_image_result() {
        let drivers = Arc::new(BrowserDrivers::new());
        drivers.attach("t1", Arc::new(Shot));
        let spec = elitea_browser_tools::spec::find("browser_screenshot").unwrap();
        let seeing = BrowserTool {
            spec,
            thread: "t1".to_owned(),
            drivers: drivers.clone(),
            vision: true,
            session: None,
            page: SharedPage::default(),
            turn: Arc::default(),
        };
        let mut result = seeing.run(&json!({})).await;
        let image = elitea_agent_runtime::image_result::take_image(&mut result).unwrap();
        assert_eq!(image.mime, "image/png");
        assert_eq!(image.base64, "AQID");
        assert_eq!(result["width"], 2);
        let blind = BrowserTool {
            spec,
            thread: "t1".to_owned(),
            drivers: drivers.clone(),
            vision: false,
            session: None,
            page: SharedPage::default(),
            turn: Arc::default(),
        };
        let result = blind.run(&json!({})).await;
        assert!(result.get(IMAGE_FIELD).is_none());
        assert!(
            result["hint"]
                .as_str()
                .unwrap()
                .contains("browser_snapshot")
        );
        drivers.detach("t1");
        assert!(drivers.get("t1").is_none());
    }

    const PAGE: &str = "# snapshot s1\n# url \"https://shop.example.com/cart?id=7#top\"\n# title \"Cart\"\n\
- textbox \"Search\" [ref=e1]\n\
- button \"Place order\" [ref=e2]\n\
- button \"Submit\" [ref=e3]\n\
- button \"Next page\" [ref=e4]\n\
- button \"Choose file\" [ref=e5]\n\
- link \"Download invoice\" [ref=e6]\n";

    fn page() -> Page {
        let snapshot = Snapshot::parse(PAGE).unwrap();
        Page {
            url: Some(snapshot.url.clone()),
            snapshot: Some(snapshot),
            ..Page::default()
        }
    }

    fn target(name: &str, args: &Value, page: &Page) -> (approvals::ToolKind, BrowserTarget) {
        target_live(name, args, page, &Live::default())
    }

    fn target_live(
        name: &str,
        args: &Value,
        page: &Page,
        live: &Live,
    ) -> (approvals::ToolKind, BrowserTarget) {
        let call = rule_call(name, args, page, live).unwrap();
        (call.tool, call.browser.unwrap())
    }

    fn control(role: &str, name: &str) -> ControlInfo {
        ControlInfo {
            role: role.to_owned(),
            name: name.to_owned(),
            ..ControlInfo::default()
        }
    }

    #[test]
    fn each_call_is_described_for_the_rules() {
        let page = page();
        let (kind, read) = target("browser_snapshot", &json!({}), &page);
        assert_eq!(kind, approvals::ToolKind::BrowserRead);
        assert_eq!(read.host.as_deref(), Some("shop.example.com"));
        assert_eq!(read.url.as_deref(), Some("https://shop.example.com/cart"));
        let (kind, open) = target(
            "browser_open",
            &json!({"url": "https://Docs.Example.org./a?q"}),
            &page,
        );
        assert_eq!(kind, approvals::ToolKind::BrowserNavigate);
        assert_eq!(open.host.as_deref(), Some("docs.example.org"));
        let (kind, plain) = target("browser_click", &json!({"ref": "e4"}), &page);
        assert_eq!(kind, approvals::ToolKind::BrowserAct);
        assert_eq!(plain.effect, None);
        assert!(!plain.consequential);
        assert_eq!(plain.control.unwrap().name.as_deref(), Some("Next page"));
        // "Choose file" is not a file input by its line alone: a submit
        // (asked); the driver's input type makes it an upload.
        for (args, effect) in [
            (json!({"ref": "e3"}), BrowserEffect::Submit),
            (json!({"ref": "e5"}), BrowserEffect::Submit),
            (json!({"ref": "e6"}), BrowserEffect::Download),
        ] {
            let (_, act) = target("browser_click", &args, &page);
            assert_eq!(act.effect, Some(effect), "{args}");
            assert!(!act.consequential, "{args}");
        }
        let (_, typed) = target(
            "browser_type",
            &json!({"ref": "e1", "text": "shoes", "submit": true}),
            &page,
        );
        assert_eq!(typed.effect, Some(BrowserEffect::Submit));
        // A press with no focus read fails closed (finding 1).
        let (_, enter) = target("browser_press", &json!({"key": "Enter"}), &page);
        assert_eq!(enter.effect, Some(BrowserEffect::Submit));
        assert!(enter.consequential);
        let (_, order) = target("browser_click", &json!({"ref": "e2"}), &page);
        assert!(order.consequential, "a buy word");
        // Fail closed: a control the snapshot does not hold.
        let (_, unknown) = target("browser_click", &json!({"ref": "e9"}), &page);
        assert!(unknown.consequential);
        let (_, blind) = target("browser_click", &json!({"ref": "e4"}), &Page::default());
        assert!(blind.consequential && blind.host.is_none());
        // The hand-over is a question; a call that does not parse never
        // reaches the page.
        let live = Live::default();
        assert!(
            rule_call(
                "browser_request_user",
                &json!({"reason": "sign in"}),
                &page,
                &live
            )
            .is_none()
        );
        assert!(rule_call("browser_click", &json!({}), &page, &live).is_none());
    }

    /// Finding 1: a press is decided against the focused control the
    /// driver read; unknown focus fails closed, every time.
    #[test]
    fn a_press_is_decided_against_the_focused_control() {
        let page = page();
        let press = |key: &str, focused: Option<Option<ControlInfo>>| {
            target_live(
                "browser_press",
                &json!({ "key": key }),
                &page,
                &Live {
                    focused,
                    ..Live::default()
                },
            )
            .1
        };
        for key in ["Enter", "Space", "Control+Enter"] {
            let order = press(key, Some(Some(control("button", "Place order"))));
            assert!(order.consequential, "{key}");
            assert_eq!(
                order.control.and_then(|c| c.name).as_deref(),
                Some("Place order"),
                "the ask names the control"
            );
            let unknown = press(key, None);
            assert!(unknown.consequential, "{key} on an unknown focus");
        }
        let next = press("Enter", Some(Some(control("button", "Next page"))));
        assert!(!next.consequential && next.effect.is_none());
        let field = press("Enter", Some(Some(control("textbox", "Search"))));
        assert_eq!(field.effect, Some(BrowserEffect::Submit));
        assert!(
            !field.consequential,
            "a grant or a site approval may answer it"
        );
        let export = press("Enter", Some(Some(control("link", "Export CSV"))));
        assert_eq!(export.effect, Some(BrowserEffect::Download));
        let tab = press("Tab", None);
        assert!(tab.effect.is_none() && !tab.consequential, "Tab only moves");
    }

    /// Findings 2 and 3, through the real classifier: a link that downloads
    /// is a download, never an upload or a submit, whatever its words; an
    /// upload is a file input or an upload/attach control only.
    #[test]
    fn downloads_come_first_and_uploads_are_file_inputs() {
        let page = Page {
            snapshot: Some(
                Snapshot::parse(
                    "# snapshot s1\n# url \"https://shop.example.com/\"\n# title \"Shop\"\n\
- link \"Download file\" [ref=e1]\n\
- link \"Submit and download report\" [ref=e2]\n\
- link \"Export\" [ref=e3]\n\
- button \"Export CSV\" [ref=e4]\n\
- link \"Open file list\" [ref=e5]\n\
- button \"Attach\" [ref=e6]\n\
- textbox \"Resume\" [ref=e7]\n\
- link \"Invoice 7\" [ref=e8]\n",
                )
                .unwrap(),
            ),
            url: Some("https://shop.example.com/".to_owned()),
            ..Page::default()
        };
        let click = |r: &str, inspected: Option<ControlInfo>| {
            target_live(
                "browser_click",
                &json!({ "ref": r }),
                &page,
                &Live {
                    inspected,
                    ..Live::default()
                },
            )
            .1
        };
        for r in ["e1", "e2", "e3", "e4"] {
            assert_eq!(click(r, None).effect, Some(BrowserEffect::Download), "{r}");
        }
        // A link with "file" in its name is not an upload.
        assert_eq!(click("e5", None).effect, None);
        assert_eq!(click("e6", None).effect, Some(BrowserEffect::Upload));
        // A file input reads as a textbox; the driver's type says file.
        let file = ControlInfo {
            input_type: Some("file".to_owned()),
            ..control("textbox", "Resume")
        };
        assert_eq!(click("e7", Some(file)).effect, Some(BrowserEffect::Upload));
        // A link with a `download` attribute downloads, whatever its name.
        let attr = ControlInfo {
            download: true,
            ..control("link", "Invoice 7")
        };
        assert_eq!(click("e8", None).effect, None);
        assert_eq!(
            click("e8", Some(attr)).effect,
            Some(BrowserEffect::Download)
        );
    }

    /// Finding 4: the act's host is the live page's, not the last snapshot's.
    #[test]
    fn the_live_page_is_the_host_of_the_call() {
        let page = page();
        let live = Live {
            url: Some("https://Pay.Other.example/checkout?x=1".to_owned()),
            ..Live::default()
        };
        let (_, scroll) = target_live(
            "browser_scroll",
            &json!({"direction": "down"}),
            &page,
            &live,
        );
        assert_eq!(scroll.host.as_deref(), Some("pay.other.example"));
        assert_eq!(
            scroll.url.as_deref(),
            Some("https://pay.other.example/checkout")
        );
        // A driver that could not say: the agent's last view.
        let (_, read) = target("browser_snapshot", &json!({}), &page);
        assert_eq!(read.host.as_deref(), Some("shop.example.com"));
    }

    struct Pane;

    #[async_trait]
    impl BrowserDriver for Pane {
        async fn open(&self, url: &str) -> Result<PageInfo, BrowserError> {
            Ok(PageInfo {
                url: url.to_owned(),
                title: "Page".to_owned(),
                refs_stale: true,
            })
        }
        async fn snapshot(&self, _max: usize) -> Result<Snapshot, BrowserError> {
            Ok(Snapshot::parse(PAGE).unwrap())
        }
        async fn text(&self, _max: usize) -> Result<PageText, BrowserError> {
            Ok(PageText::default())
        }
        async fn screenshot(&self, _t: Option<&ElementRef>) -> Result<Screenshot, BrowserError> {
            Err(BrowserError::new(ErrorCode::Unsupported, "no"))
        }
        async fn click(&self, _t: &ElementRef, _d: bool) -> Result<PageInfo, BrowserError> {
            Ok(PageInfo {
                url: "https://shop.example.com/done".to_owned(),
                ..PageInfo::default()
            })
        }
        async fn type_text(
            &self,
            _t: &ElementRef,
            _x: &str,
            _c: bool,
            _s: bool,
        ) -> Result<PageInfo, BrowserError> {
            Ok(PageInfo::default())
        }
        async fn select(&self, _t: &ElementRef, _v: &[String]) -> Result<PageInfo, BrowserError> {
            Ok(PageInfo::default())
        }
        async fn scroll(&self, _t: &ScrollTarget) -> Result<PageInfo, BrowserError> {
            Ok(PageInfo::default())
        }
        async fn press(&self, _k: &Key) -> Result<PageInfo, BrowserError> {
            Ok(PageInfo::default())
        }
        async fn history(&self, _s: HistoryStep) -> Result<PageInfo, BrowserError> {
            Ok(PageInfo::default())
        }
        async fn wait(&self, _c: &WaitCondition, _t: Duration) -> Result<PageInfo, BrowserError> {
            Ok(PageInfo::default())
        }
        async fn request_user(&self, _r: &str) -> Result<Handback, BrowserError> {
            Err(BrowserError::new(ErrorCode::NoPerson, "nobody"))
        }
    }

    /// A pane whose live address the test sets, and whose click asks for a
    /// download the way a page's link does.
    struct Moving {
        url: Mutex<String>,
        drivers: std::sync::Weak<BrowserDrivers>,
        verdicts: Mutex<Vec<DownloadVerdict>>,
    }

    #[async_trait]
    impl BrowserDriver for Moving {
        async fn open(&self, url: &str) -> Result<PageInfo, BrowserError> {
            Pane.open(url).await
        }
        async fn snapshot(&self, max: usize) -> Result<Snapshot, BrowserError> {
            Pane.snapshot(max).await
        }
        async fn text(&self, max: usize) -> Result<PageText, BrowserError> {
            Pane.text(max).await
        }
        async fn screenshot(&self, t: Option<&ElementRef>) -> Result<Screenshot, BrowserError> {
            Pane.screenshot(t).await
        }
        async fn click(&self, _t: &ElementRef, _d: bool) -> Result<PageInfo, BrowserError> {
            if let Some(drivers) = self.drivers.upgrade() {
                let verdict = drivers.download_requested("t1");
                self.verdicts.lock().unwrap().push(verdict);
            }
            Ok(PageInfo::default())
        }
        async fn type_text(
            &self,
            t: &ElementRef,
            x: &str,
            c: bool,
            s: bool,
        ) -> Result<PageInfo, BrowserError> {
            Pane.type_text(t, x, c, s).await
        }
        async fn select(&self, t: &ElementRef, v: &[String]) -> Result<PageInfo, BrowserError> {
            Pane.select(t, v).await
        }
        async fn scroll(&self, t: &ScrollTarget) -> Result<PageInfo, BrowserError> {
            Pane.scroll(t).await
        }
        async fn press(&self, k: &Key) -> Result<PageInfo, BrowserError> {
            Pane.press(k).await
        }
        async fn history(&self, s: HistoryStep) -> Result<PageInfo, BrowserError> {
            Pane.history(s).await
        }
        async fn wait(&self, c: &WaitCondition, t: Duration) -> Result<PageInfo, BrowserError> {
            Pane.wait(c, t).await
        }
        async fn request_user(&self, r: &str) -> Result<Handback, BrowserError> {
            Pane.request_user(r).await
        }
        async fn current_url(&self) -> Result<String, BrowserError> {
            Ok(self.url.lock().unwrap().clone())
        }
    }

    fn moving_toolset() -> (Arc<BrowserDrivers>, Arc<Moving>, BrowserToolset) {
        let drivers = Arc::new(BrowserDrivers::new());
        let pane = Arc::new(Moving {
            url: Mutex::new("https://shop.example.com/cart".to_owned()),
            drivers: Arc::downgrade(&drivers),
            verdicts: Mutex::new(Vec::new()),
        });
        drivers.attach("t1", pane.clone());
        let toolset = BrowserToolset {
            thread: "t1".to_owned(),
            drivers: drivers.clone(),
            vision: false,
            session: None,
            page: SharedPage::default(),
            turn: Arc::default(),
        };
        (drivers, pane, toolset)
    }

    fn tool_of(toolset: &BrowserToolset, name: &str) -> BrowserTool {
        BrowserTool {
            spec: elitea_browser_tools::spec::find(name).unwrap(),
            thread: toolset.thread.clone(),
            drivers: toolset.drivers.clone(),
            vision: false,
            session: toolset.session.clone(),
            page: toolset.page.clone(),
            turn: toolset.turn.clone(),
        }
    }

    /// Finding 4: refs of a snapshot of another site are stale once the
    /// pane moved there; a page-level act still runs.
    #[tokio::test]
    async fn a_ref_act_on_another_site_than_the_snapshot_is_stale() {
        let (_drivers, pane, toolset) = moving_toolset();
        let snap = tool_of(&toolset, "browser_snapshot").run(&json!({})).await;
        assert_eq!(snap["status"], "ok", "{snap}");
        let same = tool_of(&toolset, "browser_click")
            .run(&json!({"ref": "e4"}))
            .await;
        assert_eq!(same["status"], "ok", "{same}");
        tool_of(&toolset, "browser_snapshot").run(&json!({})).await;
        *pane.url.lock().unwrap() = "https://evil.example/".to_owned();
        let moved = tool_of(&toolset, "browser_click")
            .run(&json!({"ref": "e4"}))
            .await;
        assert_eq!(moved["code"], "stale_ref", "{moved}");
        assert!(toolset.page.lock().unwrap().snapshot.is_none());
        let scroll = tool_of(&toolset, "browser_scroll")
            .run(&json!({"direction": "down"}))
            .await;
        assert_eq!(scroll["status"], "ok", "{scroll}");
    }

    /// Finding 3: while an agent turn drives the pane, a download starts
    /// only when the call was decided as a download; one that was not is
    /// refused, said in the result, and the same control is asked as a
    /// download next time. Outside a turn the person's downloads stand.
    #[tokio::test]
    async fn downloads_in_an_agent_turn_need_the_call_to_be_a_download() {
        let (drivers, pane, toolset) = moving_toolset();
        assert_eq!(drivers.download_requested("t1"), DownloadVerdict::Person);
        tool_of(&toolset, "browser_snapshot").run(&json!({})).await;
        // "Next page" is a plain act: a download it starts is refused.
        let plain = tool_of(&toolset, "browser_click")
            .run(&json!({"ref": "e4"}))
            .await;
        assert_eq!(plain["status"], "ok", "{plain}");
        assert!(
            plain["download"]
                .as_str()
                .is_some_and(|d| d.starts_with("refused")),
            "{plain}"
        );
        let learned = (
            "shop.example.com".to_owned(),
            "button".to_owned(),
            "Next page".to_owned(),
        );
        assert!(drivers.learned_download("t1", &learned));
        // The link that downloads is approved as one: it starts.
        tool_of(&toolset, "browser_snapshot").run(&json!({})).await;
        let download = tool_of(&toolset, "browser_click")
            .run(&json!({"ref": "e6"}))
            .await;
        assert!(download.get("download").is_none(), "{download}");
        assert_eq!(
            *pane.verdicts.lock().unwrap(),
            [DownloadVerdict::Refused, DownloadVerdict::Approved]
        );
        // Once only: a second request in the same call is refused.
        assert_eq!(drivers.download_requested("t1"), DownloadVerdict::Refused);
        // The turn ends with its toolset: the person's again.
        drop(toolset);
        assert_eq!(drivers.download_requested("t1"), DownloadVerdict::Person);
    }

    #[tokio::test]
    async fn the_page_the_agent_saw_feeds_the_next_calls_rules() {
        let drivers = Arc::new(BrowserDrivers::new());
        drivers.attach("t1", Arc::new(Pane));
        let toolset = BrowserToolset {
            thread: "t1".to_owned(),
            drivers,
            vision: false,
            session: None,
            page: SharedPage::default(),
            turn: Arc::default(),
        };
        let tool = |name: &str| BrowserTool {
            spec: elitea_browser_tools::spec::find(name).unwrap(),
            thread: toolset.thread.clone(),
            drivers: toolset.drivers.clone(),
            vision: false,
            session: None,
            page: toolset.page.clone(),
            turn: toolset.turn.clone(),
        };
        let opened = tool("browser_open")
            .run(&json!({"url": "https://shop.example.com/"}))
            .await;
        assert_eq!(opened["status"], "ok", "{opened}");
        assert_eq!(
            toolset.page.lock().unwrap().url.as_deref(),
            Some("https://shop.example.com/")
        );
        let snap = tool("browser_snapshot").run(&json!({})).await;
        assert_eq!(snap["status"], "ok", "{snap}");
        assert!(toolset.page.lock().unwrap().snapshot.is_some());
        let clicked = tool("browser_click").run(&json!({"ref": "e4"})).await;
        assert_eq!(clicked["status"], "ok", "{clicked}");
        let page = toolset.page.lock().unwrap();
        assert!(page.snapshot.is_none(), "an act forgets the controls");
        assert_eq!(page.url.as_deref(), Some("https://shop.example.com/done"));
    }

    // ── control (ADR-0048 decision 4, #1591) ─────────────────────────────

    /// A pane whose `browser_wait` never ends and whose question is the
    /// control's hand-over, as the desktop wires it.
    struct Slow(Arc<PaneControl>);

    #[async_trait]
    impl BrowserDriver for Slow {
        async fn open(&self, url: &str) -> Result<PageInfo, BrowserError> {
            Pane.open(url).await
        }
        async fn snapshot(&self, max: usize) -> Result<Snapshot, BrowserError> {
            Pane.snapshot(max).await
        }
        async fn text(&self, max: usize) -> Result<PageText, BrowserError> {
            Pane.text(max).await
        }
        async fn screenshot(&self, _t: Option<&ElementRef>) -> Result<Screenshot, BrowserError> {
            Shot.screenshot(None).await
        }
        async fn click(&self, t: &ElementRef, d: bool) -> Result<PageInfo, BrowserError> {
            Pane.click(t, d).await
        }
        async fn type_text(
            &self,
            t: &ElementRef,
            x: &str,
            c: bool,
            s: bool,
        ) -> Result<PageInfo, BrowserError> {
            Pane.type_text(t, x, c, s).await
        }
        async fn select(&self, t: &ElementRef, v: &[String]) -> Result<PageInfo, BrowserError> {
            Pane.select(t, v).await
        }
        async fn scroll(&self, t: &ScrollTarget) -> Result<PageInfo, BrowserError> {
            Pane.scroll(t).await
        }
        async fn press(&self, k: &Key) -> Result<PageInfo, BrowserError> {
            Pane.press(k).await
        }
        async fn history(&self, s: HistoryStep) -> Result<PageInfo, BrowserError> {
            Pane.history(s).await
        }
        async fn wait(&self, _c: &WaitCondition, _t: Duration) -> Result<PageInfo, BrowserError> {
            std::future::pending().await
        }
        async fn request_user(&self, reason: &str) -> Result<Handback, BrowserError> {
            match self.0.ask(reason).await {
                crate::browser_control::Answer::HandedBack(note) => Ok(Handback { note }),
                crate::browser_control::Answer::Declined => {
                    Err(BrowserError::new(ErrorCode::Declined, "declined"))
                }
            }
        }
        async fn current_url(&self) -> Result<String, BrowserError> {
            Ok("https://login.example.com/welcome?session=1".to_owned())
        }
    }

    #[derive(Default)]
    struct Notes(Mutex<Vec<String>>);

    impl NoteSink for Notes {
        fn note(&self, text: &str) -> bool {
            self.0.lock().unwrap().push(text.to_owned());
            true
        }
    }

    struct Controlled {
        drivers: Arc<BrowserDrivers>,
        control: Arc<PaneControl>,
        toolset: BrowserToolset,
        recorder: Arc<Recorder>,
        notes: Arc<Notes>,
    }

    fn controlled() -> Controlled {
        let drivers = Arc::new(BrowserDrivers::new());
        let control = drivers.control("t1");
        drivers.attach("t1", Arc::new(Slow(control.clone())));
        let recorder = Arc::new(Recorder::default());
        let notes = Arc::new(Notes::default());
        let toolset = BrowserToolset {
            thread: "t1".to_owned(),
            drivers: drivers.clone(),
            vision: false,
            session: None,
            page: SharedPage::default(),
            turn: Arc::new(TurnLink {
                recorder: Some(recorder.clone()),
                notes: Some(notes.clone()),
                drove: std::sync::atomic::AtomicBool::new(false),
            }),
        };
        Controlled {
            drivers,
            control,
            toolset,
            recorder,
            notes,
        }
    }

    impl Controlled {
        fn tool(&self, name: &str) -> BrowserTool {
            BrowserTool {
                spec: elitea_browser_tools::spec::find(name).unwrap(),
                thread: "t1".to_owned(),
                drivers: self.drivers.clone(),
                vision: false,
                session: None,
                page: self.toolset.page.clone(),
                turn: self.toolset.turn.clone(),
            }
        }

        async fn call(&self, run_id: &str, name: &str, args: Value) -> Value {
            assert!(self.recorder.tool_started(run_id, name, &args));
            self.tool(name).run_call(run_id, &args).await
        }

        fn step(&self, run_id: &str) -> Value {
            self.recorder
                .tool_calls()
                .get(run_id)
                .and_then(|entry| entry.get(BROWSER_STEP_KEY).cloned())
                .unwrap_or(Value::Null)
        }
    }

    #[tokio::test]
    async fn person_input_during_a_call_answers_user_active_within_100_ms() {
        let pane = controlled();
        let opened = pane
            .call(
                "c1",
                "browser_open",
                json!({"url": "https://shop.example.com/"}),
            )
            .await;
        assert_eq!(opened["status"], "ok", "{opened}");
        assert_eq!(pane.control.view().control, Pill::Agent);
        let waiting = pane.tool("browser_wait");
        let call = tokio::spawn(async move {
            waiting
                .run_call("c2", &json!({"text": "never", "timeout_ms": 30000}))
                .await
        });
        // Let the call reach the page, then the person clicks (a synthetic
        // input, as the page script reports a trusted one).
        tokio::time::sleep(Duration::from_millis(20)).await;
        let input_at = std::time::Instant::now();
        pane.control.person_input();
        let result = tokio::time::timeout(Duration::from_millis(100), call)
            .await
            .expect("the paused call answers within 100 ms")
            .unwrap();
        assert!(input_at.elapsed() < Duration::from_millis(100));
        assert_eq!(result["code"], "user_active", "{result}");
        assert_eq!(pane.control.view().control, Pill::User);
    }

    #[tokio::test]
    async fn while_the_person_drives_calls_are_refused_but_a_permitted_watch() {
        let pane = controlled();
        pane.call(
            "c1",
            "browser_open",
            json!({"url": "https://shop.example.com/"}),
        )
        .await;
        let snap = pane.call("c2", "browser_snapshot", json!({})).await;
        assert_eq!(snap["status"], "ok", "{snap}");
        pane.control.take_over();
        for (id, name, args) in [
            ("c3", "browser_click", json!({"ref": "e4"})),
            ("c4", "browser_snapshot", json!({})),
            ("c5", "browser_screenshot", json!({})),
            (
                "c6",
                "browser_open",
                json!({"url": "https://other.example/"}),
            ),
        ] {
            let refused = pane.call(id, name, args).await;
            assert_eq!(refused["code"], "user_in_control", "{name}: {refused}");
            assert_eq!(pane.step(id)["outcome"], "user_in_control");
        }
        // Let the agent watch: snapshots and screenshots run, acts do not.
        pane.control.set_watch(true);
        let watched = pane.call("c7", "browser_snapshot", json!({})).await;
        assert_eq!(watched["status"], "ok", "{watched}");
        let shot = pane.call("c8", "browser_screenshot", json!({})).await;
        assert_eq!(shot["status"], "ok", "{shot}");
        assert_eq!(pane.step("c8")["screenshot"]["taken"], true);
        let act = pane.call("c9", "browser_click", json!({"ref": "e4"})).await;
        assert_eq!(act["code"], "user_in_control");
        // Hand back with a note: the agent drives again; the note is the
        // person's message of the turn.
        assert!(pane.control.hand_back(Some("added the item, go on")));
        assert_eq!(*pane.notes.0.lock().unwrap(), ["added the item, go on"]);
        assert_eq!(pane.control.view().control, Pill::Agent);
        // The person used the page: the agent's refs are not the page's.
        let stale = pane
            .call("c10", "browser_click", json!({"ref": "e4"}))
            .await;
        assert_eq!(stale["code"], "stale_ref", "{stale}");
        pane.call("c11", "browser_snapshot", json!({})).await;
        let clicked = pane
            .call("c12", "browser_click", json!({"ref": "e4"}))
            .await;
        assert_eq!(clicked["status"], "ok", "{clicked}");
        // The turn ends with its toolset: the pill is Idle.
        drop(pane.toolset);
        assert_eq!(pane.control.view().control, Pill::Idle);
    }

    #[tokio::test]
    async fn request_user_hands_over_and_its_note_answers_the_call() {
        let pane = controlled();
        pane.call(
            "c1",
            "browser_open",
            json!({"url": "https://login.example.com/"}),
        )
        .await;
        let asking = pane.tool("browser_request_user");
        let question = tokio::spawn(async move {
            asking
                .run_call("c2", &json!({"reason": "Sign in with your account"}))
                .await
        });
        while !pane.control.view().asked {
            tokio::task::yield_now().await;
        }
        let view = pane.control.view();
        assert_eq!(view.control, Pill::User);
        assert_eq!(view.reason.as_deref(), Some("Sign in with your account"));
        // The person types in the page: the question is not paused by it.
        pane.control.person_input();
        pane.control.hand_back(Some("signed in"));
        let answered = question.await.unwrap();
        assert_eq!(answered["status"], "ok", "{answered}");
        assert_eq!(answered["note"], "signed in");
        // A question's note is its answer, not a second message.
        assert!(pane.notes.0.lock().unwrap().is_empty());
    }

    #[tokio::test]
    async fn step_records_name_the_page_without_query_and_typed_text_as_a_digest() {
        let pane = controlled();
        pane.call(
            "c1",
            "browser_open",
            json!({"url": "https://Shop.Example.com./cart?id=7#top"}),
        )
        .await;
        let open = pane.step("c1");
        assert_eq!(open["tool"], "browser_open");
        assert_eq!(open["outcome"], "ok");
        assert_eq!(open["host"], "shop.example.com");
        assert_eq!(open["path"], "/cart");
        pane.call("c2", "browser_snapshot", json!({})).await;
        pane.call(
            "c3",
            "browser_type",
            json!({"ref": "e1", "text": "hunter2 secret"}),
        )
        .await;
        let typed = pane.step("c3");
        assert_eq!(typed["ref"], "e1");
        assert_eq!(typed["role"], "textbox");
        assert_eq!(typed["name"], "Search");
        assert_eq!(typed["text"]["chars"], 14);
        assert!(
            typed["text"]["digest"]
                .as_str()
                .unwrap()
                .starts_with("hmac-sha256:")
        );
        assert!(!typed.to_string().contains("hunter2"));
        // The question's step names the page the person handed back.
        let asking = pane.tool("browser_request_user");
        let recorder = pane.recorder.clone();
        assert!(recorder.tool_started("c4", "browser_request_user", &json!({})));
        let question =
            tokio::spawn(async move { asking.run_call("c4", &json!({"reason": "MFA"})).await });
        while !pane.control.view().asked {
            tokio::task::yield_now().await;
        }
        pane.control.decline();
        assert_eq!(question.await.unwrap()["code"], "declined");
        let asked = pane.step("c4");
        assert_eq!(asked["outcome"], "declined");
        assert_eq!(asked["host"], "login.example.com");
        assert_eq!(asked["path"], "/welcome");
    }

    #[test]
    fn typed_text_is_redacted_in_the_recorded_inputs_too() {
        let digest = |text: &str| format!("d:{}", text.len());
        let recorded = crate::computer_tools::recorded_inputs(
            "browser_type",
            &json!({"ref": "e1", "text": "secret"}),
            Some(&digest),
        );
        assert_eq!(
            recorded,
            json!({"ref": "e1", "text_chars": 6, "text_digest": "d:6"})
        );
    }
}

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
//! `BrowserNavigate` or `BrowserAct` with its [`BrowserTarget`] (the page's
//! host, the control's role and name from the latest snapshot, and whether
//! it submits, uploads or downloads), and goes through the session's
//! approval channel: a submit is asked with "allow for this site in this
//! thread", a buy, pay, delete, send or sign control every time, plan mode
//! denies navigating and acting, and an unattended grant's
//! `browser_submits` answers a submit on its hosts. The classifier is the
//! cloud's (`classify_browser_call`, ADR-0049 decision 3), so both hosts
//! ask the same questions; it fails closed (a ref it cannot resolve asks).
//!
//! The toolset is offered only while the client policy's
//! `local_work_browser` and `local_work_browser_agent` are on (`d0::turn`);
//! the rules engine denies every call when they are off as well.

use std::collections::HashMap;
use std::sync::{Arc, Mutex};

use adk_core::{ReadonlyContext, Tool, ToolContext, Toolset};
use async_trait::async_trait;
use base64::Engine as _;
use elitea_agent_runtime::host::{
    ApprovalOutcome, ApprovalRequest, HostError, ToolProvider, ToolsetRequest,
};
use elitea_agent_runtime::image_result::{IMAGE_FIELD, IMAGE_KIND};
use elitea_agent_runtime::toolkits::{BrowserEscalation, classify_browser_call};
use elitea_browser_tools::driver::ToolOutput;
use elitea_browser_tools::{
    BrowserCall, BrowserDriver, BrowserError, ErrorCode, Snapshot, TOOLS, ToolKind, ToolSpec,
    dispatch,
};
use elitea_local_tools::approvals::{
    self, BrowserEffect, BrowserTarget, ComputerControl, PAYLOAD_KEY, ToolCall,
};
use elitea_local_tools::session::LocalSession;
use serde_json::{Value, json};

/// The toolset's name.
pub const TOOLSET_NAME: &str = "browser";

/// The live pane drivers, by thread (conversation id).
#[derive(Default)]
pub struct BrowserDrivers {
    drivers: Mutex<HashMap<String, Arc<dyn BrowserDriver>>>,
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
    }

    #[must_use]
    pub fn get(&self, thread: &str) -> Option<Arc<dyn BrowserDriver>> {
        self.lock().get(thread).cloned()
    }
}

/// What the thread's pane last showed the agent: the latest snapshot (the
/// classifier's controls) and the page's URL (the host an act is on).
#[derive(Default)]
struct Page {
    snapshot: Option<Snapshot>,
    url: Option<String>,
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

/// The rules engine's view of one call ([`ToolCall::browser`]): its kind,
/// the page's host (or the URL it opens), the control and what the act
/// starts. `None` for `browser_request_user` (a question, never ruled) and
/// a call that does not parse (it never reaches the page).
fn rule_call(name: &str, args: &Value, page: &Page) -> Option<ToolCall> {
    let call = BrowserCall::parse(name, args).ok()?;
    let tool = match call.kind() {
        ToolKind::BrowserRead => approvals::ToolKind::BrowserRead,
        ToolKind::BrowserNavigate => approvals::ToolKind::BrowserNavigate,
        ToolKind::BrowserAct => approvals::ToolKind::BrowserAct,
        ToolKind::Question => return None,
    };
    let url = match &call {
        BrowserCall::Open { url } => Some(url.clone()),
        _ => page.url.clone(),
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
        if let Some(node) =
            element.and_then(|element| page.snapshot.as_ref()?.resolve(element).ok())
        {
            target.control = Some(ComputerControl {
                role: Some(node.role.clone()),
                name: Some(node.name.clone()),
                ..ComputerControl::default()
            });
        }
        let classified = classify_browser_call(name, args, page.snapshot.as_ref());
        match classified.escalation {
            Some(BrowserEscalation::Submit) => {
                let upload = target.control.as_ref().is_some_and(|control| {
                    control.role.as_deref().is_some_and(|role| {
                        role.eq_ignore_ascii_case("file") || role.eq_ignore_ascii_case("fileupload")
                    }) || control.name.as_deref().is_some_and(|name| {
                        name.split(|c: char| !c.is_alphanumeric()).any(|word| {
                            word.eq_ignore_ascii_case("file") || word.eq_ignore_ascii_case("files")
                        })
                    })
                });
                target.effect = Some(if upload {
                    BrowserEffect::Upload
                } else {
                    BrowserEffect::Submit
                });
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
                target.effect = Some(BrowserEffect::Submit);
            }
            None => {}
        }
        if target.control.as_ref().is_some_and(|control| {
            control
                .role
                .as_deref()
                .is_some_and(|role| role.eq_ignore_ascii_case("link"))
                && control.name.as_deref().is_some_and(|name| {
                    name.split(|c: char| !c.is_alphanumeric())
                        .any(|word| word.eq_ignore_ascii_case("download"))
                })
        }) && target.effect.is_none()
        {
            target.effect = Some(BrowserEffect::Download);
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

    /// The rules engine's answer to this call (ADR-0048 decision 8), through
    /// the session's approval channel: `Ok` to run it, or the tool result
    /// the model sees instead.
    async fn approve(&self, call_id: &str, args: &Value) -> Result<(), BrowserError> {
        let Some(session) = &self.session else {
            return Ok(());
        };
        let call = {
            let page = self
                .page
                .lock()
                .unwrap_or_else(std::sync::PoisonError::into_inner);
            rule_call(self.spec.name, args, &page)
        };
        let Some(call) = call else {
            return Ok(());
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
            ApprovalOutcome::Decided { action, .. } if action == "approve" => Ok(()),
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
        } else if self.spec.kind != ToolKind::BrowserRead {
            page.snapshot = None;
        }
        if url.is_some() {
            page.url = url;
        }
    }

    async fn run_call(&self, call_id: &str, args: &Value) -> Value {
        if let Err(error) = self.approve(call_id, args).await {
            return error.to_result();
        }
        let Some(driver) = self.drivers.get(&self.thread) else {
            return BrowserError::new(
                ErrorCode::Unsupported,
                "the browser pane of this thread is not open, or the agent cannot drive it: ask the person to open it (Thread > Browser) or to reopen it",
            )
            .to_result();
        };
        let output = dispatch(driver.as_ref(), self.spec.name, args).await;
        if let ToolOutput::Json(result) = &output {
            self.observe(result);
        }
        tool_result(output, self.vision)
    }
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
            }),
        }
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

    #[test]
    fn the_host_exposes_exactly_the_contract() {
        let toolset = BrowserToolset {
            thread: "t1".to_owned(),
            drivers: Arc::new(BrowserDrivers::new()),
            vision: true,
            session: None,
            page: SharedPage::default(),
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
        }
    }

    fn target(name: &str, args: &Value, page: &Page) -> (approvals::ToolKind, BrowserTarget) {
        let call = rule_call(name, args, page).unwrap();
        (call.tool, call.browser.unwrap())
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
        for (args, effect) in [
            (json!({"ref": "e3"}), BrowserEffect::Submit),
            (json!({"ref": "e5"}), BrowserEffect::Upload),
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
        let (_, enter) = target("browser_press", &json!({"key": "Enter"}), &page);
        assert_eq!(enter.effect, Some(BrowserEffect::Submit));
        let (_, order) = target("browser_click", &json!({"ref": "e2"}), &page);
        assert!(order.consequential, "a buy word");
        // Fail closed: a control the snapshot does not hold.
        let (_, unknown) = target("browser_click", &json!({"ref": "e9"}), &page);
        assert!(unknown.consequential);
        let (_, blind) = target("browser_click", &json!({"ref": "e4"}), &Page::default());
        assert!(blind.consequential && blind.host.is_none());
        // The hand-over is a question; a call that does not parse never
        // reaches the page.
        assert!(rule_call("browser_request_user", &json!({"reason": "sign in"}), &page).is_none());
        assert!(rule_call("browser_click", &json!({}), &page).is_none());
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
        };
        let tool = |name: &str| BrowserTool {
            spec: elitea_browser_tools::spec::find(name).unwrap(),
            thread: toolset.thread.clone(),
            drivers: toolset.drivers.clone(),
            vision: false,
            session: None,
            page: toolset.page.clone(),
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
}

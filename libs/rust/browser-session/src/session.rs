//! The [`Backend`] the agent serves: one [`CdpDriver`] over the engine the
//! `start` line asks for (ADR-0049 decision 2).
//!
//! * `full`: chrome-headless-shell.
//! * `lite`: Moli, always. Only in an image that ships Moli, and only under
//!   gVisor: Moli has no process sandbox of its own, so its container is the
//!   only boundary, and a shared kernel is refused (`isolation_refused`).
//! * `auto`: Moli first. The session switches to chrome-headless-shell, once
//!   and for good, when Moli fails a tool call in a way Chrome would not
//!   ([`SwitchReason`]): a command of the call itself is answered as
//!   unsupported, times out or meets an internal engine error (background
//!   traffic never counts, [`scoped_faults`]), `browser_open` does not load
//!   in time, the engine dies or drops the connection, or a tree snapshot
//!   gives out no ref where the page's DOM shows controls (or no node at all
//!   where it shows text). The switch reopens the current URL on Chrome
//!   (an `open` is answered again with its own URL instead), keeps the session's [`Control`] (a
//!   person's take-over carries over), moves every open view to the new
//!   engine's screencast, and tells the agent and the viewer: the call's
//!   result carries `engine_switched` with the reason, and the agent line
//!   protocol sends a `notice` `engine_switched` that the supervisor relays
//!   to the view. A read call (open, snapshot, screenshot, wait) is answered
//!   again by Chrome; an act already ran on Moli and is never repeated, so
//!   its Moli result comes back with `engine_switched.repeated: false`.
//!
//! A start whose Moli does not come up under `auto` within
//! [`AUTO_MOLI_READY`] starts Chrome instead, with the same notice: the run
//! still gets a browser.

use std::future::Future;
use std::path::PathBuf;
use std::pin::Pin;
use std::sync::Arc;
use std::time::Duration;

use async_trait::async_trait;
use elitea_browser_tools::{BrowserError, ErrorCode, ToolOutput, dispatch, spec};
use serde_json::{Value, json};
use tokio::sync::{Mutex, mpsc, watch};

use crate::cdp::{CallFaults, scoped_faults};
use crate::control::{Control, Driver};
use crate::driver::{CdpDriver, PageContent};
use crate::engine::{AUTO_MOLI_READY, EngineConfig, EngineKind};
use crate::protocol::{Backend, FrameSource, Start, StartError, ViewFrame, log};
use crate::screencast::Viewer;

/// How long `browser_request_user` waits for the person. The supervisor
/// fails a call after 90 s (the agent line protocol's limit), so the
/// question is answered `declined` before that.
pub const REQUEST_USER_TIMEOUT: Duration = Duration::from_secs(80);

/// How long `auto` waits before it reads a zero-ref snapshot again on Moli,
/// so a page still rendering is not taken for one Moli cannot read.
const EMPTY_SNAPSHOT_RETRY: Duration = Duration::from_millis(500);

/// The notice the agent sends when `auto` moves from Moli to Chrome.
pub const NOTICE_ENGINE_SWITCHED: &str = "engine_switched";

/// gVisor's sentry reports a fixed kernel in `/proc/version`:
/// `Linux version <release> #1 SMP Sun Jan 10 15:06:54 PST 2016`, where the
/// release is `4.19.0-gvisor` (or `5.15.0-gvisor` with RDMA) on current
/// releases and plain `4.4.0` on old ones
/// (`pkg/sentry/kernel/version/version.go`, checked at release 20261102).
const GVISOR_BUILD: &str = "#1 SMP Sun Jan 10 15:06:54 PST 2016";

/// Whether this process runs under gVisor, by the kernel version string its
/// sentry reports. Fail-closed: a string it does not recognise is "not
/// gVisor", and `engine_sandbox: false` is then refused.
#[must_use]
pub fn under_gvisor() -> bool {
    std::fs::read_to_string("/proc/version").is_ok_and(|version| is_gvisor_version(&version))
}

/// A start refused for want of gVisor says which kernel it saw, so a gVisor
/// release that changes its `/proc/version` shows in the log instead of as
/// every session refused for no visible reason. The kernel string is not
/// run content.
fn log_not_gvisor() {
    let seen = std::fs::read_to_string("/proc/version").unwrap_or_default();
    let seen: String = seen
        .trim()
        .chars()
        .filter(|c| !c.is_control())
        .take(160)
        .collect();
    log(&format!("not_gvisor kernel={seen:?}"));
}

fn is_gvisor_version(version: &str) -> bool {
    let Some(rest) = version.trim_end().strip_prefix("Linux version ") else {
        return false;
    };
    let Some((release, build)) = rest.split_once(' ') else {
        return false;
    };
    build == GVISOR_BUILD && (release == "4.4.0" || release.ends_with("-gvisor"))
}

/// The engine binaries an image ships.
#[derive(Clone, Debug, Default)]
pub struct Engines {
    /// chrome-headless-shell (`full`).
    pub chrome: PathBuf,
    /// Moli (`lite`); `None` in an image without it, where `lite` and
    /// `auto` fail to start.
    pub moli: Option<PathBuf>,
}

/// Why `auto` left Moli.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum SwitchReason {
    /// Moli did not start.
    StartFailed,
    /// Moli answered a command as unsupported.
    Unsupported,
    /// Moli exited or dropped the connection during the call.
    EngineLost,
    /// Moli did not answer a command in time, or `browser_open` did not
    /// load in time.
    Timeout,
    /// Moli answered a command with JSON-RPC's internal error.
    EngineError,
    /// A tree snapshot gave out no ref where the page shows controls, or
    /// had no node where it shows text, on a second read too.
    EmptySnapshot,
}

impl SwitchReason {
    #[must_use]
    pub const fn code(self) -> &'static str {
        match self {
            Self::StartFailed => "start_failed",
            Self::Unsupported => "unsupported_protocol",
            Self::EngineLost => "engine_lost",
            Self::Timeout => "timeout",
            Self::EngineError => "engine_error",
            Self::EmptySnapshot => "empty_snapshot",
        }
    }
}

/// The engine policy of one session.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
enum Policy {
    Full,
    Lite,
    Auto,
}

impl Policy {
    fn parse(engine: &str) -> Option<Self> {
        match engine {
            "full" => Some(Self::Full),
            "lite" => Some(Self::Lite),
            "auto" => Some(Self::Auto),
            _ => None,
        }
    }
}

/// What the session was started with, kept for a switch.
struct Started {
    config: EngineConfig,
    /// The session's one control: a switch hands it to the new engine, so a
    /// person who took over still drives afterwards.
    control: Arc<Control>,
    policy: Policy,
}

type Launched = Pin<Box<dyn Future<Output = Result<Arc<CdpDriver>, StartError>> + Send>>;

/// Starts an engine and its driver (the real engines; tests use fakes).
type Launcher = Box<dyn Fn(EngineConfig, Arc<Control>) -> Launched + Send + Sync>;

fn engine_launcher() -> Launcher {
    Box::new(|config, control| {
        Box::pin(async move {
            CdpDriver::start(&config, control)
                .await
                .map(Arc::new)
                .map_err(|_| StartError::EngineStartFailed)
        })
    })
}

/// The calls an `auto` switch answers again on Chrome: they only read, or
/// (`browser_open`) set the page the switch reopens anyway. Every other
/// call may have acted on the page already and is never repeated.
fn repeatable(name: &str) -> bool {
    matches!(
        name,
        spec::BROWSER_OPEN | spec::BROWSER_SNAPSHOT | spec::BROWSER_SCREENSHOT | spec::BROWSER_WAIT
    )
}

/// The session behind the protocol.
pub struct Session {
    engines: Engines,
    engine_log: bool,
    /// Whether to trust `engine_sandbox: false` without gVisor (tests only).
    gvisor: fn() -> bool,
    /// The engine's driver. A watch, so an open view follows a switch.
    driver: watch::Sender<Option<Arc<CdpDriver>>>,
    started: Mutex<Option<Started>>,
    launcher: Launcher,
    /// The protocol's notices (`engine_switched`).
    notices: std::sync::Mutex<Option<mpsc::UnboundedSender<&'static str>>>,
}

impl Session {
    /// A session over chrome-headless-shell only.
    #[must_use]
    pub fn new(engine: PathBuf, engine_log: bool) -> Self {
        Self::with_engines(
            Engines {
                chrome: engine,
                moli: None,
            },
            engine_log,
        )
    }

    #[must_use]
    pub fn with_engines(engines: Engines, engine_log: bool) -> Self {
        Self {
            engines,
            engine_log,
            gvisor: under_gvisor,
            driver: watch::Sender::new(None),
            started: Mutex::new(None),
            launcher: engine_launcher(),
            notices: std::sync::Mutex::new(None),
        }
    }

    fn driver(&self) -> Option<Arc<CdpDriver>> {
        self.driver.borrow().clone()
    }

    /// The engine the session runs now, its process id and whether it is
    /// alive; `None` before `start`. For the bake-off's measurements
    /// (`bakeoff.rs`): an `auto` session reports Chrome after a switch.
    pub fn engine_state(&self) -> Option<(EngineKind, Option<u32>, bool)> {
        let driver = self.driver()?;
        Some((driver.kind(), driver.engine_pid(), driver.is_alive()))
    }

    fn notify(&self, code: &'static str) {
        log(code);
        if let Some(notices) = self
            .notices
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
            .as_ref()
        {
            let _ = notices.send(code);
        }
    }

    async fn launch(
        &self,
        config: &EngineConfig,
        control: Arc<Control>,
    ) -> Result<(Arc<CdpDriver>, String), StartError> {
        let driver = (self.launcher)(config.clone(), control).await?;
        let version = driver
            .engine_version()
            .await
            .map_err(|_| StartError::EngineStartFailed)?;
        Ok((driver, version))
    }

    /// Move an `auto` session to Chrome: start it, reopen `url` (unless
    /// `reopen` is false: the call answered again opens a page itself), and
    /// swap the driver. `None` when it could not start (the Moli driver
    /// stays).
    async fn switch(
        &self,
        url: &str,
        reason: SwitchReason,
        reopen: bool,
    ) -> Option<Arc<CdpDriver>> {
        let (config, control) = {
            let started = self.started.lock().await;
            let started = started.as_ref()?;
            if started.policy != Policy::Auto || started.config.kind != EngineKind::Moli {
                return None;
            }
            (
                EngineConfig {
                    engine: self.engines.chrome.clone(),
                    kind: EngineKind::Chrome,
                    ..started.config.clone()
                },
                Arc::clone(&started.control),
            )
        };
        let (chrome, _) = self.launch(&config, control).await.ok()?;
        if reopen && (url.starts_with("http://") || url.starts_with("https://")) {
            chrome.reopen(url).await;
        }
        // Views follow the watch to Chrome's screencast before Moli stops.
        let previous = self.driver.send_replace(Some(Arc::clone(&chrome)));
        if let Some(previous) = previous {
            previous.shutdown();
        }
        if let Some(started) = self.started.lock().await.as_mut() {
            started.config = config;
        }
        log(reason.code());
        self.notify(NOTICE_ENGINE_SWITCHED);
        Some(chrome)
    }
}

/// Whether an `auto` session must leave Moli after `name` answered
/// `output`, from the faults of the call's own commands and whether the
/// engine is still alive. Only Moli's own failures count: a page that does
/// not have what a `browser_wait` waits for, a stale ref or a refused
/// scheme fails the same way on Chrome and never switches. The empty
/// snapshot rule needs a look at the page and is [`empty_snapshot`].
fn switch_reason(
    name: &str,
    output: &ToolOutput,
    faults: CallFaults,
    alive: bool,
) -> Option<SwitchReason> {
    if faults.unsupported > 0 {
        return Some(SwitchReason::Unsupported);
    }
    if !alive || faults.closed > 0 {
        return Some(SwitchReason::EngineLost);
    }
    let result = output.result();
    if faults.timeouts > 0 || (name == spec::BROWSER_OPEN && result["code"] == "timeout") {
        return Some(SwitchReason::Timeout);
    }
    // The driver tolerates some failed commands (a child frame's tree that
    // went away): an internal error counts only when it failed the call.
    if faults.engine_errors > 0 && result["status"] == "error" {
        return Some(SwitchReason::EngineError);
    }
    None
}

/// A tree snapshot that gave out no ref, the one kind of answer the empty
/// snapshot rule looks at.
fn zero_ref_tree(name: &str, output: &ToolOutput) -> bool {
    let result = output.result();
    name == spec::BROWSER_SNAPSHOT
        && result["status"] == "ok"
        && result.get("snapshot").is_some()
        && result["refs"].as_u64() == Some(0)
}

/// Whether a zero-ref tree snapshot misses what the page's DOM shows: a
/// control (a link, a button, a field) the tree gave no ref, or
/// any text where the tree has no node at all. A page of text alone has no
/// ref on any engine, so zero refs over text is not by itself Moli's fault.
fn empty_snapshot(output: &ToolOutput, content: PageContent) -> bool {
    content.controls
        || (content.text && empty_tree(output.result()["content"].as_str().unwrap_or_default()))
}

/// A rendered snapshot with no node line (only `# ` header lines inside
/// its frame).
fn empty_tree(framed: &str) -> bool {
    let inner = elitea_browser_tools::frame::unframe(framed).map_or(framed, |(inner, _)| inner);
    !inner
        .lines()
        .any(|line| line.trim_start().starts_with("- "))
}

/// Mark `output` as answered after a switch. `repeated`: Chrome answered
/// the call again; otherwise `output` is Moli's answer to an act that is
/// not repeated, and the page the agent sees next was reopened on Chrome.
fn mark_switched(output: ToolOutput, reason: SwitchReason, repeated: bool) -> ToolOutput {
    let mark = |mut result: Value| {
        if let Some(object) = result.as_object_mut() {
            let mut switched = json!({
                "from": "lite", "to": "full", "reason": reason.code(), "repeated": repeated,
            });
            if !repeated {
                switched["note"] = json!(
                    "this call ran on lite before the switch and was not repeated; the page was reopened on full: snapshot again before the next act"
                );
            }
            object.insert("engine_switched".to_owned(), switched);
        }
        result
    };
    match output {
        ToolOutput::Json(result) => ToolOutput::Json(mark(result)),
        ToolOutput::Image {
            media_type,
            data,
            result,
        } => ToolOutput::Image {
            media_type,
            data,
            result: mark(result),
        },
    }
}

#[async_trait]
impl Backend for Session {
    async fn start(&self, start: &Start) -> Result<String, StartError> {
        let Some(policy) = Policy::parse(&start.engine) else {
            log("engine_not_admitted");
            return Err(StartError::EngineStartFailed);
        };
        let gvisor = (self.gvisor)();
        if !start.engine_sandbox && !gvisor {
            log_not_gvisor();
            return Err(StartError::IsolationRefused);
        }
        let moli = match policy {
            Policy::Full => None,
            Policy::Lite | Policy::Auto => {
                let Some(moli) = self.engines.moli.clone() else {
                    log("engine_not_in_image");
                    return Err(StartError::EngineStartFailed);
                };
                // Moli has no sandbox of its own: gVisor is the boundary.
                if !gvisor {
                    log_not_gvisor();
                    return Err(StartError::IsolationRefused);
                }
                Some(moli)
            }
        };
        // Whether a person can be asked comes with the run's `start`; the
        // one control lives as long as the session, across a switch.
        let control = Arc::new(Control::new(start.attended, REQUEST_USER_TIMEOUT));
        let (proxy, proxy_token) = match &start.proxy {
            Some((url, token)) => (Some(url.clone()), Some(token.clone())),
            None => (None, None),
        };
        let chrome = EngineConfig {
            engine: self.engines.chrome.clone(),
            kind: EngineKind::Chrome,
            proxy,
            proxy_token,
            gvisor: !start.engine_sandbox,
            width: start.width,
            height: start.height,
            engine_log: self.engine_log,
            offline: start.proxy.is_none(),
            ..EngineConfig::default()
        };
        let mut config = moli.map_or_else(
            || chrome.clone(),
            |moli| EngineConfig {
                engine: moli,
                kind: EngineKind::Moli,
                // `auto` falls back to Chrome: Moli gets a short bound, so
                // Chrome keeps most of the protocol's 60 s `ready` limit.
                moli_ready: if policy == Policy::Auto {
                    AUTO_MOLI_READY
                } else {
                    chrome.moli_ready
                },
                ..chrome.clone()
            },
        );
        let launched = match self.launch(&config, Arc::clone(&control)).await {
            Ok(launched) => Ok(launched),
            Err(_) if policy == Policy::Auto => {
                log(SwitchReason::StartFailed.code());
                config = chrome;
                let launched = self.launch(&config, Arc::clone(&control)).await;
                if launched.is_ok() {
                    self.notify(NOTICE_ENGINE_SWITCHED);
                }
                launched
            }
            Err(error) => Err(error),
        };
        let (driver, version) = launched?;
        self.driver.send_replace(Some(driver));
        *self.started.lock().await = Some(Started {
            config,
            control,
            policy,
        });
        Ok(version)
    }

    async fn call(&self, name: &str, args: &Value) -> ToolOutput {
        let Some(driver) = self.driver() else {
            return ToolOutput::Json(
                BrowserError::new(ErrorCode::Internal, "the browser is not started").to_result(),
            );
        };
        let watching_moli = driver.kind() == EngineKind::Moli
            && self
                .started
                .lock()
                .await
                .as_ref()
                .is_some_and(|started| started.policy == Policy::Auto);
        // Only the call's own commands count: screencast acks, the page
        // tracker and `Fetch.enable` on new targets run on tasks of their
        // own.
        let (mut output, faults) = scoped_faults(dispatch(driver.as_ref(), name, args)).await;
        if !watching_moli {
            return output;
        }
        let mut reason = switch_reason(name, &output, faults, driver.is_alive());
        // The page check is one more engine round trip, only for a tree
        // snapshot that gave out no ref. A page still rendering gets one
        // more read on Moli before it counts.
        if reason.is_none()
            && zero_ref_tree(name, &output)
            && empty_snapshot(&output, driver.page_content().await)
        {
            tokio::time::sleep(EMPTY_SNAPSHOT_RETRY).await;
            let (again, faults) = scoped_faults(dispatch(driver.as_ref(), name, args)).await;
            reason = switch_reason(name, &again, faults, driver.is_alive());
            if reason.is_none()
                && zero_ref_tree(name, &again)
                && empty_snapshot(&again, driver.page_content().await)
            {
                reason = Some(SwitchReason::EmptySnapshot);
            }
            output = again;
        }
        let Some(reason) = reason else {
            return output;
        };
        let (url, _) = driver.location();
        // An open is answered again with its own URL: reopening the old
        // one first would only spend the call's time.
        let reopen = name != spec::BROWSER_OPEN;
        match self.switch(&url, reason, reopen).await {
            // A read again, on Chrome: the page was reopened, so a ref call
            // starts from a fresh snapshot (`stale_ref` tells the agent).
            Some(chrome) if repeatable(name) => {
                mark_switched(dispatch(chrome.as_ref(), name, args).await, reason, true)
            }
            // An act already ran (or half ran) on Moli: never twice.
            Some(_) => mark_switched(output, reason, false),
            None => output,
        }
    }

    async fn view(&self, max_fps: u32) -> Option<Box<dyn FrameSource>> {
        let mut slot = self.driver.subscribe();
        let driver = slot.borrow_and_update().clone()?;
        let viewer = driver.screencast().subscribe(max_fps).await.ok()?;
        Some(Box::new(Frames {
            slot,
            driver,
            viewer,
            max_fps,
        }))
    }

    async fn input(&self, event: &Value) -> Result<(), &'static str> {
        let driver = self.driver().ok_or("not_started")?;
        driver
            .person_input(event)
            .await
            .map_err(|_| "input_refused")
    }

    fn notices(&self, sender: mpsc::UnboundedSender<&'static str>) {
        *self
            .notices
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner) = Some(sender);
    }

    async fn run_ended(&self) {}

    async fn stop(&self) {
        if let Some(driver) = self.driver.send_replace(None) {
            driver.shutdown();
        }
    }
}

/// A view's frames. It follows the session's driver: after an `auto`
/// switch it subscribes to Chrome's screencast, so the view goes on.
struct Frames {
    slot: watch::Receiver<Option<Arc<CdpDriver>>>,
    driver: Arc<CdpDriver>,
    viewer: Viewer,
    max_fps: u32,
}

#[async_trait]
impl FrameSource for Frames {
    async fn next(&mut self) -> Option<ViewFrame> {
        loop {
            let moved = tokio::select! {
                frame = self.viewer.next() => match frame {
                    Some(frame) => return Some(self.view_frame(frame)),
                    // The engine's stream ended: a switch, or the end.
                    None => false,
                },
                changed = self.slot.changed() => {
                    if changed.is_err() {
                        return None;
                    }
                    true
                }
            };
            let current = self.slot.borrow_and_update().clone();
            match current {
                Some(driver) if !Arc::ptr_eq(&driver, &self.driver) => {
                    self.viewer = driver.screencast().subscribe(self.max_fps).await.ok()?;
                    self.driver = driver;
                }
                _ if !moved => return None,
                _ => {}
            }
        }
    }
}

impl Frames {
    fn view_frame(&self, frame: crate::screencast::Frame) -> ViewFrame {
        let (url, title) = self.driver.location();
        ViewFrame {
            seq: frame.sequence,
            jpeg: frame.jpeg,
            width: frame.width,
            height: frame.height,
            url,
            title,
            user: self.driver.control().state().driver != Driver::Agent,
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn start(engine: &str, sandbox: bool) -> Start {
        Start {
            engine: engine.to_owned(),
            engine_sandbox: sandbox,
            max_pages: 3,
            width: 1280,
            height: 800,
            proxy: None,
            attended: true,
        }
    }

    #[tokio::test]
    async fn no_sandbox_outside_gvisor_and_unknown_engines_are_refused() {
        let mut session = Session::new("/nonexistent/engine".into(), false);
        session.gvisor = || false;
        assert_eq!(
            session.start(&start("full", false)).await,
            Err(StartError::IsolationRefused)
        );
        assert_eq!(
            session.start(&start("turbo", true)).await,
            Err(StartError::EngineStartFailed)
        );
        // A missing engine fails to start rather than hanging.
        assert_eq!(
            session.start(&start("full", true)).await,
            Err(StartError::EngineStartFailed)
        );
        let refused = session.call("browser_open", &serde_json::json!({})).await;
        assert_eq!(refused.result()["code"], "internal");
    }

    #[tokio::test]
    async fn lite_needs_moli_in_the_image_and_gvisor() {
        // An image without Moli: lite and auto do not start.
        let mut chrome_only = Session::new("/nonexistent/chrome".into(), false);
        chrome_only.gvisor = || true;
        for engine in ["lite", "auto"] {
            assert_eq!(
                chrome_only.start(&start(engine, true)).await,
                Err(StartError::EngineStartFailed),
                "{engine}"
            );
        }
        // Moli on a shared kernel: refused, even with the engine sandbox
        // on (Moli has none).
        let mut shared_kernel = Session::with_engines(
            Engines {
                chrome: "/nonexistent/chrome".into(),
                moli: Some("/nonexistent/moli".into()),
            },
            false,
        );
        shared_kernel.gvisor = || false;
        for engine in ["lite", "auto"] {
            assert_eq!(
                shared_kernel.start(&start(engine, true)).await,
                Err(StartError::IsolationRefused),
                "{engine}"
            );
        }
        // Under gVisor a Moli that does not start fails `lite`, and `auto`
        // falls back to Chrome (here missing too, so it fails as well).
        shared_kernel.gvisor = || true;
        assert_eq!(
            shared_kernel.start(&start("lite", true)).await,
            Err(StartError::EngineStartFailed)
        );
        assert_eq!(
            shared_kernel.start(&start("auto", true)).await,
            Err(StartError::EngineStartFailed)
        );
    }

    #[test]
    fn auto_switches_on_moli_faults_only() {
        let ok = ToolOutput::Json(json!({"status": "ok"}));
        let none = CallFaults::default();
        assert_eq!(switch_reason(spec::BROWSER_CLICK, &ok, none, true), None);
        let unsupported = CallFaults {
            unsupported: 1,
            ..none
        };
        assert_eq!(
            switch_reason(spec::BROWSER_CLICK, &ok, unsupported, true),
            Some(SwitchReason::Unsupported)
        );
        // The engine gone, by its process or its connection.
        assert_eq!(
            switch_reason(spec::BROWSER_SNAPSHOT, &ok, none, false),
            Some(SwitchReason::EngineLost)
        );
        let closed = CallFaults { closed: 1, ..none };
        assert_eq!(
            switch_reason(spec::BROWSER_SNAPSHOT, &ok, closed, true),
            Some(SwitchReason::EngineLost)
        );
        let timeout = ToolOutput::Json(json!({"status": "error", "code": "timeout"}));
        // An open that did not load in time is Moli's; a wait whose
        // condition never held is the page's, on any engine.
        assert_eq!(
            switch_reason(spec::BROWSER_OPEN, &timeout, none, true),
            Some(SwitchReason::Timeout)
        );
        assert_eq!(
            switch_reason(spec::BROWSER_WAIT, &timeout, none, true),
            None
        );
        let unanswered = CallFaults {
            timeouts: 1,
            ..none
        };
        assert_eq!(
            switch_reason(spec::BROWSER_WAIT, &timeout, unanswered, true),
            Some(SwitchReason::Timeout)
        );
        let internal = ToolOutput::Json(json!({"status": "error", "code": "internal"}));
        let engine_error = CallFaults {
            engine_errors: 1,
            ..none
        };
        assert_eq!(
            switch_reason(spec::BROWSER_SNAPSHOT, &internal, engine_error, true),
            Some(SwitchReason::EngineError)
        );
        // An internal error the driver got past (a child frame's tree) did
        // not fail the call.
        assert_eq!(
            switch_reason(spec::BROWSER_SNAPSHOT, &ok, engine_error, true),
            None
        );
        // A page-side refusal is not Moli's.
        let stale = ToolOutput::Json(json!({"status": "error", "code": "stale_ref"}));
        assert_eq!(switch_reason(spec::BROWSER_CLICK, &stale, none, true), None);
    }

    #[test]
    fn an_empty_snapshot_is_judged_by_the_dom() {
        let framed =
            |inner: &str| elitea_browser_tools::frame::frame_with_nonce(inner, &"a".repeat(32));
        let empty = ToolOutput::Json(json!({
            "status": "ok", "snapshot": "s1", "refs": 0, "truncated": false, "omitted": 0,
            "content": framed("# snapshot s1\n# url \"https://a.test/\"\n# title \"A\"\n"),
        }));
        let text_only = ToolOutput::Json(json!({
            "status": "ok", "snapshot": "s1", "refs": 0,
            "content": framed("# snapshot s1\n- heading \"Hello\"\n"),
        }));
        assert!(zero_ref_tree(spec::BROWSER_SNAPSHOT, &empty));
        assert!(!zero_ref_tree(spec::BROWSER_SCREENSHOT, &empty));
        let text = PageContent {
            text: true,
            controls: false,
        };
        let controls = PageContent {
            text: true,
            controls: true,
        };
        // No node at all over text: Moli lost the tree.
        assert!(empty_snapshot(&empty, text));
        // Nodes but no ref, on a page of text alone: that is the page.
        assert!(!empty_snapshot(&text_only, text));
        // Nodes but no ref where the DOM has links or fields: Moli missed
        // them (the bake-off's salesforce-developer-docs and
        // servicenow-docs, whose links live in shadow roots).
        assert!(empty_snapshot(&text_only, controls));
        // An empty page is empty on any engine.
        assert!(!empty_snapshot(&empty, PageContent::default()));
    }

    #[test]
    fn a_switched_result_says_so() {
        let marked = mark_switched(
            ToolOutput::Json(json!({"status": "ok"})),
            SwitchReason::Unsupported,
            true,
        );
        assert_eq!(
            marked.result()["engine_switched"],
            json!({"from": "lite", "to": "full", "reason": "unsupported_protocol", "repeated": true})
        );
        let act = mark_switched(
            ToolOutput::Json(json!({"status": "error", "code": "internal"})),
            SwitchReason::Unsupported,
            false,
        );
        assert_eq!(act.result()["engine_switched"]["repeated"], false);
        assert!(act.result()["engine_switched"]["note"].is_string());
        assert!(repeatable(spec::BROWSER_SNAPSHOT) && repeatable(spec::BROWSER_WAIT));
        for act in [
            spec::BROWSER_CLICK,
            spec::BROWSER_TYPE,
            spec::BROWSER_PRESS,
            spec::BROWSER_SELECT,
            spec::BROWSER_BACK,
            spec::BROWSER_REQUEST_USER,
        ] {
            assert!(!repeatable(act), "{act}");
        }
    }

    /// How a fake engine answers one method, set while a test runs.
    #[derive(Clone)]
    enum Behaviour {
        /// This result.
        Reply(Value),
        /// JSON-RPC's internal error (-32603).
        Internal,
        /// No answer at all.
        Silent,
        /// The engine drops the connection, as one that died does.
        HangUp,
    }

    type Script = Arc<std::sync::Mutex<std::collections::HashMap<String, Behaviour>>>;

    /// A fake engine over a duplex pipe. It answers what `CdpDriver::attach`
    /// and the calls under test need, refuses `unsupported` methods as Moli
    /// does (-32601), answers a method as the test sets it ([`Behaviour`]),
    /// records every method it was sent, and lets the test push events
    /// (screencast frames).
    struct FakeEngine {
        methods: Arc<std::sync::Mutex<Vec<String>>>,
        script: Script,
        push: mpsc::UnboundedSender<Value>,
    }

    impl FakeEngine {
        fn sent(&self, method: &str) -> usize {
            self.methods
                .lock()
                .unwrap()
                .iter()
                .filter(|sent| *sent == method)
                .count()
        }

        fn set(&self, method: &str, behaviour: Behaviour) {
            self.script
                .lock()
                .unwrap()
                .insert(method.to_owned(), behaviour);
        }

        /// The page moved to `url` within its document.
        fn navigated(&self, url: &str) {
            self.push
                .send(json!({
                    "method": "Page.navigatedWithinDocument", "sessionId": "S",
                    "params": { "frameId": "T", "url": url },
                }))
                .unwrap();
        }

        fn frame(&self, id: i64, byte: u8) {
            use base64::Engine as _;
            let data = base64::engine::general_purpose::STANDARD.encode([byte]);
            self.push
                .send(json!({
                    "method": "Page.screencastFrame", "sessionId": "S",
                    "params": {
                        "data": data, "sessionId": id,
                        "metadata": {"deviceWidth": 1280.0, "deviceHeight": 800.0},
                    },
                }))
                .unwrap();
        }
    }

    fn fake_engine(
        product: &'static str,
        unsupported: &'static [&'static str],
    ) -> (crate::cdp::Connection, FakeEngine) {
        use tokio::io::{AsyncBufReadExt, AsyncWriteExt, BufReader};
        let (client_write, engine_read) = tokio::io::duplex(1 << 20);
        let (mut engine_write, client_read) = tokio::io::duplex(1 << 20);
        let methods = Arc::new(std::sync::Mutex::new(Vec::new()));
        let script = Script::default();
        let (push, mut pushed) = mpsc::unbounded_channel::<Value>();
        // `None` hangs up: the writer ends, and the client reads the end.
        let (out, mut outgoing) = mpsc::unbounded_channel::<Option<Value>>();
        tokio::spawn(async move {
            loop {
                let message = tokio::select! {
                    Some(message) = pushed.recv() => message,
                    Some(message) = outgoing.recv() => match message {
                        Some(message) => message,
                        None => break,
                    },
                    else => break,
                };
                let mut bytes = serde_json::to_vec(&message).unwrap();
                bytes.push(0);
                if engine_write.write_all(&bytes).await.is_err() {
                    break;
                }
            }
        });
        let seen = Arc::clone(&methods);
        let scripted = Arc::clone(&script);
        tokio::spawn(async move {
            let mut reader = BufReader::new(engine_read);
            let mut record = Vec::new();
            while reader.read_until(0, &mut record).await.unwrap_or(0) > 0 {
                record.pop();
                let command: Value = serde_json::from_slice(&record).unwrap();
                record.clear();
                let method = command["method"].as_str().unwrap_or_default().to_owned();
                seen.lock().unwrap().push(method.clone());
                let behaviour = scripted.lock().unwrap().get(&method).cloned();
                let reply = match behaviour {
                    Some(Behaviour::Silent) => continue,
                    Some(Behaviour::HangUp) => {
                        let _ = out.send(None);
                        break;
                    }
                    Some(Behaviour::Internal) => {
                        json!({"id": command["id"], "error": {"code": -32603, "message": "Internal error"}})
                    }
                    Some(Behaviour::Reply(result)) => {
                        json!({"id": command["id"], "result": result})
                    }
                    None if unsupported.contains(&method.as_str()) => {
                        json!({"id": command["id"], "error": {"code": -32601, "message": format!("'{method}' wasn't found")}})
                    }
                    None => {
                        let result = match method.as_str() {
                            "Target.getTargets" => {
                                json!({"targetInfos": [{"type": "page", "targetId": "T"}]})
                            }
                            "Target.attachToTarget" => json!({"sessionId": "S"}),
                            "Browser.getVersion" => json!({"product": product}),
                            "Page.createIsolatedWorld" => json!({"executionContextId": 1}),
                            "Runtime.callFunctionOn" => json!({"result": {"value": {}}}),
                            "Page.navigate" => json!({"frameId": "T", "loaderId": ""}),
                            _ => json!({}),
                        };
                        json!({"id": command["id"], "result": result})
                    }
                };
                let _ = out.send(Some(reply));
            }
        });
        (
            crate::cdp::Connection::new(client_read, client_write),
            FakeEngine {
                methods,
                script,
                push,
            },
        )
    }

    /// Each launch: its control, engine and Moli ready bound.
    type Launches = Arc<std::sync::Mutex<Vec<(Arc<Control>, EngineKind, Duration)>>>;

    /// An `auto` session over two fake engines (Moli first, then Chrome),
    /// with the Moli launch's control kept for the test.
    fn auto_over_fakes(
        moli_unsupported: &'static [&'static str],
    ) -> (Session, FakeEngine, FakeEngine, Launches) {
        let (moli_cdp, moli) = fake_engine("Moli/1.1.22", moli_unsupported);
        let (chrome_cdp, chrome) = fake_engine("HeadlessChrome/140.0", &[]);
        let connections = std::sync::Mutex::new(vec![chrome_cdp, moli_cdp]);
        let controls = Arc::new(std::sync::Mutex::new(Vec::new()));
        let kept = Arc::clone(&controls);
        let mut session = Session::with_engines(
            Engines {
                chrome: "/fake/chrome".into(),
                moli: Some("/fake/moli".into()),
            },
            false,
        );
        session.gvisor = || true;
        session.launcher = Box::new(move |config, control| {
            kept.lock()
                .unwrap()
                .push((Arc::clone(&control), config.kind, config.moli_ready));
            let cdp = connections.lock().unwrap().pop();
            Box::pin(async move {
                let cdp = cdp.ok_or(StartError::EngineStartFailed)?;
                CdpDriver::attach(cdp, &config, control)
                    .await
                    .map(Arc::new)
                    .map_err(|_| StartError::EngineStartFailed)
            })
        });
        (session, moli, chrome, controls)
    }

    /// The switch keeps the view, the person's take-over and the one
    /// control, and a read (snapshot) is answered again by Chrome.
    #[tokio::test]
    async fn a_switch_keeps_the_view_and_the_take_over() {
        let (session, moli, chrome, launched) = auto_over_fakes(&["Accessibility.getFullAXTree"]);
        let (sender, mut noticed) = mpsc::unbounded_channel();
        session.notices(sender);
        let version = session.start(&start("auto", true)).await.unwrap();
        assert!(version.starts_with("moli/"), "{version}");
        let mut view = session.view(30).await.expect("a view");
        moli.frame(1, 0x11);
        let first = tokio::time::timeout(Duration::from_secs(5), view.next())
            .await
            .unwrap()
            .unwrap();
        assert_eq!(first.jpeg, vec![0x11]);
        // A person takes over on Moli, then the agent's snapshot (watchable
        // while the person drives) meets an unsupported method.
        launched.lock().unwrap()[0].0.take_over();
        let output = session.call(spec::BROWSER_SNAPSHOT, &json!({})).await;
        assert_eq!(
            output.result()["engine_switched"]["repeated"],
            true,
            "{}",
            output.result()
        );
        assert_eq!(noticed.try_recv(), Ok(NOTICE_ENGINE_SWITCHED));
        assert_eq!(session.engine_state().unwrap().0, EngineKind::Chrome);
        assert_eq!(
            chrome.sent("Accessibility.getFullAXTree"),
            1,
            "the read ran again on Chrome"
        );
        // One control across the switch: the person still drives.
        let seen = launched.lock().unwrap().clone();
        assert_eq!(seen.len(), 2);
        assert!(Arc::ptr_eq(&seen[0].0, &seen[1].0));
        // `auto` gave Moli the short ready bound, not the 20 s of `lite`.
        assert_eq!((seen[0].1, seen[0].2), (EngineKind::Moli, AUTO_MOLI_READY));
        assert_eq!(seen[1].1, EngineKind::Chrome);
        let driver = session.driver().unwrap();
        assert_ne!(driver.control().state().driver, Driver::Agent);
        // The view follows the switch to Chrome's screencast.
        let frame = async {
            loop {
                chrome.frame(2, 0x22);
                if let Ok(frame) =
                    tokio::time::timeout(Duration::from_millis(200), view.next()).await
                {
                    return frame;
                }
            }
        };
        let next = tokio::time::timeout(Duration::from_secs(5), frame)
            .await
            .expect("the view froze after the switch")
            .expect("the view ended at the switch");
        assert_eq!(next.jpeg, vec![0x22]);
        assert!(next.user, "the person still drives on Chrome");
        assert!(chrome.sent("Page.startScreencast") >= 1);
    }

    /// An act that met an unsupported method is never run again on Chrome:
    /// the agent gets Moli's answer marked `repeated: false`.
    #[tokio::test]
    async fn an_act_is_not_repeated_after_a_switch() {
        let (session, moli, chrome, _) = auto_over_fakes(&["Input.dispatchKeyEvent"]);
        session.start(&start("auto", true)).await.unwrap();
        let output = session
            .call(spec::BROWSER_PRESS, &json!({ "key": "Enter" }))
            .await;
        let switched = &output.result()["engine_switched"];
        assert_eq!(switched["repeated"], false, "{}", output.result());
        assert!(switched["note"].is_string());
        assert_eq!(session.engine_state().unwrap().0, EngineKind::Chrome);
        assert!(moli.sent("Input.dispatchKeyEvent") >= 1);
        assert_eq!(
            chrome.sent("Input.dispatchKeyEvent"),
            0,
            "the act ran twice"
        );
    }

    /// Background traffic refused as unsupported (here every screencast
    /// ack, on the screencast's own task) does not switch the session.
    #[tokio::test]
    async fn background_unsupported_traffic_does_not_switch() {
        let (session, moli, _chrome, _) = auto_over_fakes(&["Page.screencastFrameAck"]);
        session.start(&start("auto", true)).await.unwrap();
        let mut view = session.view(30).await.unwrap();
        moli.frame(1, 1);
        let _ = tokio::time::timeout(Duration::from_secs(5), view.next())
            .await
            .unwrap();
        moli.frame(2, 2);
        let _ = tokio::time::timeout(Duration::from_secs(5), view.next())
            .await
            .unwrap();
        let output = session
            .call(
                spec::BROWSER_WAIT,
                &json!({ "url": "*", "timeout_ms": 500 }),
            )
            .await;
        assert!(
            output.result().get("engine_switched").is_none(),
            "{}",
            output.result()
        );
        assert_eq!(session.engine_state().unwrap().0, EngineKind::Moli);
        assert!(moli.sent("Page.screencastFrameAck") >= 1);
    }

    /// An `auto` session over fakes, started, with every notice it sends.
    async fn started_auto() -> (
        Session,
        FakeEngine,
        FakeEngine,
        mpsc::UnboundedReceiver<&'static str>,
    ) {
        let (session, moli, chrome, _) = auto_over_fakes(&[]);
        let (sender, noticed) = mpsc::unbounded_channel();
        session.notices(sender);
        session.start(&start("auto", true)).await.unwrap();
        (session, moli, chrome, noticed)
    }

    /// A read that switched: answered again on Chrome with `reason`.
    fn assert_read_switched(
        session: &Session,
        output: &ToolOutput,
        reason: &str,
        noticed: &mut mpsc::UnboundedReceiver<&'static str>,
    ) {
        let switched = &output.result()["engine_switched"];
        assert_eq!(switched["reason"], reason, "{}", output.result());
        assert_eq!(switched["repeated"], true, "{}", output.result());
        assert_eq!(output.result()["status"], "ok", "{}", output.result());
        assert_eq!(session.engine_state().unwrap().0, EngineKind::Chrome);
        assert_eq!(noticed.try_recv(), Ok(NOTICE_ENGINE_SWITCHED));
    }

    /// Moli died mid-call (the bake-off's docs-rust-std: `snapshot:
    /// "internal"`, the engine gone afterwards): the read goes to Chrome.
    #[tokio::test]
    async fn an_engine_that_died_switches() {
        let (session, moli, chrome, mut noticed) = started_auto().await;
        moli.set("Accessibility.getFullAXTree", Behaviour::HangUp);
        let output = session
            .call(spec::BROWSER_SNAPSHOT, &json!({ "max_nodes": 100 }))
            .await;
        assert_read_switched(&session, &output, "engine_lost", &mut noticed);
        assert_eq!(chrome.sent("Accessibility.getFullAXTree"), 1);
    }

    /// An open Moli does not load in time (the bake-off's two atlassian
    /// `open: "timeout"` tasks) is answered again on Chrome, which opens the
    /// URL itself: no reopen of the old page first.
    #[tokio::test(start_paused = true)]
    async fn an_open_that_timed_out_switches() {
        let (session, moli, chrome, mut noticed) = started_auto().await;
        // Moli is on a page already, one a switch would reopen.
        moli.navigated("https://earlier.test/");
        while session.driver().unwrap().location().0.is_empty() {
            tokio::task::yield_now().await;
        }
        // A navigation that starts and never loads.
        moli.set(
            "Page.navigate",
            Behaviour::Reply(json!({"frameId": "T", "loaderId": "L1"})),
        );
        let output = session
            .call(spec::BROWSER_OPEN, &json!({ "url": "https://slow.test/" }))
            .await;
        assert_read_switched(&session, &output, "timeout", &mut noticed);
        assert_eq!(
            chrome.sent("Page.navigate"),
            1,
            "opened once, not reopened first"
        );
    }

    /// A command Moli never answers fails the call as `timeout`: a read
    /// goes to Chrome.
    #[tokio::test(start_paused = true)]
    async fn an_unanswered_command_switches() {
        let (session, moli, chrome, mut noticed) = started_auto().await;
        moli.set("Accessibility.getFullAXTree", Behaviour::Silent);
        let output = session.call(spec::BROWSER_SNAPSHOT, &json!({})).await;
        assert_read_switched(&session, &output, "timeout", &mut noticed);
        assert_eq!(chrome.sent("Accessibility.getFullAXTree"), 1);
    }

    /// A wait whose condition never holds is the page's, on any engine:
    /// no switch.
    #[tokio::test(start_paused = true)]
    async fn a_wait_that_timed_out_does_not_switch() {
        let (session, _moli, _chrome, _) = started_auto().await;
        let output = session
            .call(
                spec::BROWSER_WAIT,
                &json!({ "url": "*/never*", "timeout_ms": 1000 }),
            )
            .await;
        assert_eq!(output.result()["code"], "timeout", "{}", output.result());
        assert!(output.result().get("engine_switched").is_none());
        assert_eq!(session.engine_state().unwrap().0, EngineKind::Moli);
    }

    /// Moli's internal error on the call's own command (`-32603`) is
    /// Moli's fault: a read goes to Chrome, an act is not repeated.
    #[tokio::test]
    async fn an_internal_engine_error_switches_and_an_act_is_not_repeated() {
        let (session, moli, chrome, mut noticed) = started_auto().await;
        moli.set("Accessibility.getFullAXTree", Behaviour::Internal);
        let output = session.call(spec::BROWSER_SNAPSHOT, &json!({})).await;
        assert_read_switched(&session, &output, "engine_error", &mut noticed);
        assert_eq!(chrome.sent("Accessibility.getFullAXTree"), 1);

        let (session, moli, chrome, _) = started_auto().await;
        moli.set("Input.dispatchKeyEvent", Behaviour::Internal);
        let output = session
            .call(spec::BROWSER_PRESS, &json!({ "key": "Enter" }))
            .await;
        let switched = &output.result()["engine_switched"];
        assert_eq!(switched["reason"], "engine_error", "{}", output.result());
        assert_eq!(switched["repeated"], false);
        assert_eq!(
            chrome.sent("Input.dispatchKeyEvent"),
            0,
            "the act ran twice"
        );
        assert!(moli.sent("Input.dispatchKeyEvent") >= 1);
    }

    /// One frame's accessibility tree: a root and these children.
    fn ax_tree(children: &[(&str, &str)]) -> Value {
        let ids: Vec<String> = (0..children.len()).map(|at| format!("n{at}")).collect();
        let mut nodes = vec![json!({
            "nodeId": "root", "ignored": false,
            "role": {"type": "role", "value": "RootWebArea"},
            "name": {"type": "computedString", "value": "A"},
            "childIds": ids,
        })];
        for (at, (role, name)) in children.iter().enumerate() {
            nodes.push(json!({
                "nodeId": format!("n{at}"), "parentId": "root", "ignored": false,
                "role": {"type": "role", "value": role},
                "name": {"type": "computedString", "value": name},
                "childIds": [],
            }));
        }
        json!({ "nodes": nodes })
    }

    /// The page probe's answer.
    fn content(text: bool, controls: u64) -> Behaviour {
        Behaviour::Reply(json!({"result": {"value": {"text": text, "controls": controls}}}))
    }

    /// Moli's tree has the page's heading but none of the links its DOM
    /// holds (the bake-off's salesforce-developer-docs and servicenow-docs:
    /// 0 refs on lite, refs on full). After a second read it goes to Chrome.
    #[tokio::test]
    async fn a_tree_without_the_doms_controls_switches() {
        let (session, moli, chrome, mut noticed) = started_auto().await;
        moli.set(
            "Accessibility.getFullAXTree",
            Behaviour::Reply(ax_tree(&[("heading", "Docs")])),
        );
        moli.set("Runtime.callFunctionOn", content(true, 12));
        let output = session.call(spec::BROWSER_SNAPSHOT, &json!({})).await;
        assert_read_switched(&session, &output, "empty_snapshot", &mut noticed);
        assert_eq!(
            moli.sent("Accessibility.getFullAXTree"),
            2,
            "Moli got a second read first"
        );
        assert_eq!(chrome.sent("Accessibility.getFullAXTree"), 1);
    }

    /// A page of text alone has no ref on any engine, and a page that
    /// renders by the second read is Moli's to keep.
    #[tokio::test]
    async fn a_text_page_or_a_late_render_stays_on_moli() {
        let (session, moli, _chrome, _) = started_auto().await;
        moli.set(
            "Accessibility.getFullAXTree",
            Behaviour::Reply(ax_tree(&[("heading", "Plain")])),
        );
        moli.set("Runtime.callFunctionOn", content(true, 0));
        let output = session.call(spec::BROWSER_SNAPSHOT, &json!({})).await;
        assert!(output.result().get("engine_switched").is_none());
        assert_eq!(output.result()["refs"], 0);
        assert_eq!(session.engine_state().unwrap().0, EngineKind::Moli);

        let (session, moli, _chrome, _) = started_auto().await;
        moli.set(
            "Accessibility.getFullAXTree",
            Behaviour::Reply(ax_tree(&[])),
        );
        moli.set("Runtime.callFunctionOn", content(true, 3));
        let late = async {
            tokio::time::sleep(EMPTY_SNAPSHOT_RETRY / 2).await;
            moli.set(
                "Accessibility.getFullAXTree",
                Behaviour::Reply(ax_tree(&[("link", "Docs")])),
            );
        };
        let args = json!({});
        let (output, ()) = tokio::join!(session.call(spec::BROWSER_SNAPSHOT, &args), late);
        assert!(
            output.result().get("engine_switched").is_none(),
            "{}",
            output.result()
        );
        assert_eq!(output.result()["refs"], 1, "{}", output.result());
        assert_eq!(session.engine_state().unwrap().0, EngineKind::Moli);
    }

    #[test]
    fn gvisor_is_told_by_its_kernel_string() {
        assert!(is_gvisor_version(
            "Linux version 4.4.0 #1 SMP Sun Jan 10 15:06:54 PST 2016\n"
        ));
        // Current gVisor releases (the bake-off's runsc 20261102 among them).
        assert!(is_gvisor_version(
            "Linux version 4.19.0-gvisor #1 SMP Sun Jan 10 15:06:54 PST 2016\n"
        ));
        assert!(is_gvisor_version(
            "Linux version 5.15.0-gvisor #1 SMP Sun Jan 10 15:06:54 PST 2016\n"
        ));
        assert!(!is_gvisor_version(
            "Linux version 6.8.0-1017-azure (buildd@lcy02) #20-Ubuntu SMP Mon Oct 20 2026\n"
        ));
        // The build line alone, or the release alone, is not enough.
        assert!(!is_gvisor_version(
            "Linux version 6.8.0 #1 SMP Sun Jan 10 15:06:54 PST 2016\n"
        ));
        assert!(!is_gvisor_version("Linux version 4.19.0-gvisor #2 SMP\n"));
    }
}

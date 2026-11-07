//! The [`Backend`] the agent serves: one [`CdpDriver`] over the engine the
//! `start` line asks for (ADR-0049 decision 2).
//!
//! * `full`: chrome-headless-shell.
//! * `lite`: Moli, always. Only in an image that ships Moli, and only under
//!   gVisor: Moli has no process sandbox of its own, so its container is the
//!   only boundary, and a shared kernel is refused (`isolation_refused`).
//! * `auto`: Moli first. The session switches to chrome-headless-shell, once
//!   and for good, when Moli answers a command as unsupported or a tree
//!   snapshot comes back empty where the page has text. The switch reopens
//!   the current URL, the call that saw it is answered by Chrome, and the
//!   agent and the viewer are both told: the call's result carries
//!   `engine_switched` with the reason, and the agent line protocol sends a
//!   `notice` `engine_switched` that the supervisor relays to the view.
//!
//! A start whose Moli does not come up under `auto` starts Chrome instead,
//! with the same notice: the run still gets a browser.

use std::path::PathBuf;
use std::sync::Arc;
use std::time::Duration;

use async_trait::async_trait;
use elitea_browser_tools::{BrowserError, ErrorCode, ToolOutput, dispatch, spec};
use serde_json::{Value, json};
use tokio::sync::{Mutex, mpsc};

use crate::control::{Control, Driver};
use crate::driver::CdpDriver;
use crate::engine::{EngineConfig, EngineKind};
use crate::protocol::{Backend, FrameSource, Start, StartError, ViewFrame, log};
use crate::screencast::Viewer;

/// How long `browser_request_user` waits for the person. The supervisor
/// fails a call after 90 s (the agent line protocol's limit), so the
/// question is answered `declined` before that.
pub const REQUEST_USER_TIMEOUT: Duration = Duration::from_secs(80);

/// The notice the agent sends when `auto` moves from Moli to Chrome.
pub const NOTICE_ENGINE_SWITCHED: &str = "engine_switched";

/// gVisor's sentry reports a fixed kernel in `/proc/version`: release
/// 4.4.0 and this build line.
const GVISOR_BUILD: &str = "#1 SMP Sun Jan 10 15:06:54 PST 2016";

/// Whether this process runs under gVisor, by the kernel version string its
/// sentry reports. Fail-closed: a string it does not recognise is "not
/// gVisor", and `engine_sandbox: false` is then refused.
#[must_use]
pub fn under_gvisor() -> bool {
    std::fs::read_to_string("/proc/version").is_ok_and(|version| is_gvisor_version(&version))
}

fn is_gvisor_version(version: &str) -> bool {
    version.contains(" 4.4.0 ") && version.contains(GVISOR_BUILD)
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
    /// A tree snapshot had no node where the page has text.
    EmptySnapshot,
}

impl SwitchReason {
    #[must_use]
    pub const fn code(self) -> &'static str {
        match self {
            Self::StartFailed => "start_failed",
            Self::Unsupported => "unsupported_protocol",
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
    attended: bool,
    policy: Policy,
}

/// The session behind the protocol.
pub struct Session {
    engines: Engines,
    engine_log: bool,
    /// Whether to trust `engine_sandbox: false` without gVisor (tests only).
    gvisor: fn() -> bool,
    driver: Mutex<Option<Arc<CdpDriver>>>,
    started: Mutex<Option<Started>>,
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
            driver: Mutex::new(None),
            started: Mutex::new(None),
            notices: std::sync::Mutex::new(None),
        }
    }

    async fn driver(&self) -> Option<Arc<CdpDriver>> {
        self.driver.lock().await.clone()
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
        config: &EngineConfig,
        attended: bool,
    ) -> Result<(Arc<CdpDriver>, String), StartError> {
        // Whether a person can be asked comes with the run's `start`.
        let control = Arc::new(Control::new(attended, REQUEST_USER_TIMEOUT));
        let driver = CdpDriver::start(config, control)
            .await
            .map_err(|_| StartError::EngineStartFailed)?;
        let version = driver
            .engine_version()
            .await
            .map_err(|_| StartError::EngineStartFailed)?;
        Ok((Arc::new(driver), version))
    }

    /// Move an `auto` session to Chrome: start it, reopen `url`, and swap
    /// the driver. `None` when it could not start (the Moli driver stays).
    async fn switch(&self, url: &str, reason: SwitchReason) -> Option<Arc<CdpDriver>> {
        let config = {
            let started = self.started.lock().await;
            let started = started.as_ref()?;
            if started.policy != Policy::Auto || started.config.kind != EngineKind::Moli {
                return None;
            }
            EngineConfig {
                engine: self.engines.chrome.clone(),
                kind: EngineKind::Chrome,
                ..started.config.clone()
            }
        };
        let attended = self.started.lock().await.as_ref()?.attended;
        let (chrome, _) = Self::launch(&config, attended).await.ok()?;
        if url.starts_with("http://") || url.starts_with("https://") {
            let _ = dispatch(chrome.as_ref(), spec::BROWSER_OPEN, &json!({ "url": url })).await;
        }
        let previous = self.driver.lock().await.replace(Arc::clone(&chrome));
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
/// `output`: a rise in unsupported-protocol answers during the call, or a
/// tree snapshot with no node at all on a page with readable text.
fn switch_reason(
    name: &str,
    output: &ToolOutput,
    unsupported_rose: bool,
    page_has_text: impl FnOnce() -> bool,
) -> Option<SwitchReason> {
    if unsupported_rose {
        return Some(SwitchReason::Unsupported);
    }
    let result = output.result();
    if name == spec::BROWSER_SNAPSHOT
        && result["status"] == "ok"
        && result.get("snapshot").is_some()
        && result["refs"].as_u64() == Some(0)
        && empty_tree(result["content"].as_str().unwrap_or_default())
        && page_has_text()
    {
        return Some(SwitchReason::EmptySnapshot);
    }
    None
}

/// A rendered snapshot with no node line (only `# ` header lines inside
/// its frame).
fn empty_tree(framed: &str) -> bool {
    let inner = elitea_browser_tools::frame::unframe(framed).map_or(framed, |(inner, _)| inner);
    !inner
        .lines()
        .any(|line| line.trim_start().starts_with("- "))
}

/// Mark `output` as answered after a switch.
fn mark_switched(output: ToolOutput, reason: SwitchReason) -> ToolOutput {
    let mark = |mut result: Value| {
        if let Some(object) = result.as_object_mut() {
            object.insert(
                "engine_switched".to_owned(),
                json!({ "from": "lite", "to": "full", "reason": reason.code() }),
            );
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
                    return Err(StartError::IsolationRefused);
                }
                Some(moli)
            }
        };
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
        };
        let mut config = moli.map_or_else(
            || chrome.clone(),
            |moli| EngineConfig {
                engine: moli,
                kind: EngineKind::Moli,
                ..chrome.clone()
            },
        );
        let launched = match Self::launch(&config, start.attended).await {
            Ok(launched) => Ok(launched),
            Err(_) if policy == Policy::Auto => {
                log(SwitchReason::StartFailed.code());
                config = chrome;
                let launched = Self::launch(&config, start.attended).await;
                if launched.is_ok() {
                    self.notify(NOTICE_ENGINE_SWITCHED);
                }
                launched
            }
            Err(error) => Err(error),
        };
        let (driver, version) = launched?;
        *self.driver.lock().await = Some(driver);
        *self.started.lock().await = Some(Started {
            config,
            attended: start.attended,
            policy,
        });
        Ok(version)
    }

    async fn call(&self, name: &str, args: &Value) -> ToolOutput {
        let Some(driver) = self.driver().await else {
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
        let before = driver.unsupported_errors();
        let output = dispatch(driver.as_ref(), name, args).await;
        if !watching_moli {
            return output;
        }
        // The text check is one more engine round trip, only for a tree
        // snapshot that gave out no ref.
        let has_text = if name == spec::BROWSER_SNAPSHOT && output.result()["refs"] == 0 {
            let text = dispatch(
                driver.as_ref(),
                spec::BROWSER_SNAPSHOT,
                &json!({ "mode": "text", "max_chars": 200 }),
            )
            .await;
            page_text_present(text.result()["content"].as_str().unwrap_or_default())
        } else {
            false
        };
        let Some(reason) =
            switch_reason(name, &output, driver.unsupported_errors() > before, || {
                has_text
            })
        else {
            return output;
        };
        let (url, _) = driver.location();
        match self.switch(&url, reason).await {
            // The call again, on Chrome: the page was reopened, so a ref
            // call starts from a fresh snapshot (`stale_ref` tells the agent).
            Some(chrome) => mark_switched(dispatch(chrome.as_ref(), name, args).await, reason),
            None => output,
        }
    }

    async fn view(&self, max_fps: u32) -> Option<Box<dyn FrameSource>> {
        let driver = self.driver().await?;
        let viewer = driver.screencast().subscribe(max_fps).await.ok()?;
        Some(Box::new(Frames { driver, viewer }))
    }

    async fn input(&self, event: &Value) -> Result<(), &'static str> {
        let driver = self.driver().await.ok_or("not_started")?;
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
        if let Some(driver) = self.driver.lock().await.take() {
            driver.shutdown();
        }
    }
}

/// Whether a text-mode snapshot's content has page text past its `# url`
/// and `# title` header.
fn page_text_present(framed: &str) -> bool {
    let inner = elitea_browser_tools::frame::unframe(framed).map_or(framed, |(inner, _)| inner);
    inner
        .lines()
        .filter(|line| !line.starts_with("# "))
        .any(|line| line.chars().any(char::is_alphanumeric))
}

struct Frames {
    driver: Arc<CdpDriver>,
    viewer: Viewer,
}

#[async_trait]
impl FrameSource for Frames {
    async fn next(&mut self) -> Option<ViewFrame> {
        let frame = self.viewer.next().await?;
        let (url, title) = self.driver.location();
        Some(ViewFrame {
            seq: frame.sequence,
            jpeg: frame.jpeg,
            width: frame.width,
            height: frame.height,
            url,
            title,
            user: self.driver.control().state().driver != Driver::Agent,
        })
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
    fn auto_switches_on_unsupported_or_an_empty_tree_over_text() {
        let framed =
            |inner: &str| elitea_browser_tools::frame::frame_with_nonce(inner, &"a".repeat(32));
        let empty = ToolOutput::Json(json!({
            "status": "ok", "snapshot": "s1", "refs": 0, "truncated": false, "omitted": 0,
            "content": framed("# snapshot s1\n# url \"https://a.test/\"\n# title \"A\"\n"),
        }));
        assert_eq!(
            switch_reason(spec::BROWSER_SNAPSHOT, &empty, false, || true),
            Some(SwitchReason::EmptySnapshot)
        );
        // An empty tree of an empty page is the page's, not Moli's.
        assert_eq!(
            switch_reason(spec::BROWSER_SNAPSHOT, &empty, false, || false),
            None
        );
        let full = ToolOutput::Json(json!({
            "status": "ok", "snapshot": "s1", "refs": 0,
            "content": framed("# snapshot s1\n- heading \"Hello\"\n"),
        }));
        assert_eq!(
            switch_reason(spec::BROWSER_SNAPSHOT, &full, false, || true),
            None
        );
        let click = ToolOutput::Json(json!({"status": "error", "code": "internal"}));
        assert_eq!(
            switch_reason(spec::BROWSER_CLICK, &click, true, || false),
            Some(SwitchReason::Unsupported)
        );
        assert_eq!(
            switch_reason(spec::BROWSER_CLICK, &click, false, || true),
            None
        );
    }

    #[test]
    fn a_switched_result_says_so() {
        let marked = mark_switched(
            ToolOutput::Json(json!({"status": "ok"})),
            SwitchReason::Unsupported,
        );
        assert_eq!(
            marked.result()["engine_switched"],
            json!({"from": "lite", "to": "full", "reason": "unsupported_protocol"})
        );
        assert!(page_text_present(
            "# url \"https://a.test/\"\n# title \"A\"\nHello world"
        ));
        assert!(!page_text_present(
            "# url \"https://a.test/\"\n# title \"A\"\n"
        ));
    }

    #[test]
    fn gvisor_is_told_by_its_kernel_string() {
        assert!(is_gvisor_version(
            "Linux version 4.4.0 #1 SMP Sun Jan 10 15:06:54 PST 2016\n"
        ));
        assert!(!is_gvisor_version(
            "Linux version 6.8.0-1017-azure (buildd@lcy02) #20-Ubuntu SMP Mon Oct 20 2026\n"
        ));
    }
}

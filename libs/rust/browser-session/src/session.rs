//! The [`Backend`] the agent serves: one [`CdpDriver`] over the engine the
//! `start` line asks for.

use std::sync::Arc;
use std::time::Duration;

use async_trait::async_trait;
use elitea_browser_tools::{BrowserError, ErrorCode, ToolOutput, dispatch};
use serde_json::Value;
use tokio::sync::Mutex;

use crate::control::{Control, Driver};
use crate::driver::CdpDriver;
use crate::engine::EngineConfig;
use crate::protocol::{Backend, FrameSource, Start, StartError, ViewFrame, log};
use crate::screencast::Viewer;

/// How long `browser_request_user` waits for the person. The supervisor
/// fails a call after 90 s (the agent line protocol's limit), so the
/// question is answered `declined` before that.
pub const REQUEST_USER_TIMEOUT: Duration = Duration::from_secs(80);

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

/// The session behind the protocol.
pub struct Session {
    /// The engine binary (the image's chrome-headless-shell).
    engine: std::path::PathBuf,
    engine_log: bool,
    /// Whether to trust `engine_sandbox: false` without gVisor (tests only).
    gvisor: fn() -> bool,
    driver: Mutex<Option<Arc<CdpDriver>>>,
}

impl Session {
    #[must_use]
    pub fn new(engine: std::path::PathBuf, engine_log: bool) -> Self {
        Self {
            engine,
            engine_log,
            gvisor: under_gvisor,
            driver: Mutex::new(None),
        }
    }

    async fn driver(&self) -> Option<Arc<CdpDriver>> {
        self.driver.lock().await.clone()
    }
}

#[async_trait]
impl Backend for Session {
    async fn start(&self, start: &Start) -> Result<String, StartError> {
        if start.engine != "full" {
            // `lite` waits for the bake-off (ADR-0049 decision 9).
            log("engine_not_admitted");
            return Err(StartError::EngineStartFailed);
        }
        if !start.engine_sandbox && !(self.gvisor)() {
            return Err(StartError::IsolationRefused);
        }
        let (proxy, proxy_token) = match &start.proxy {
            Some((url, token)) => (Some(url.clone()), Some(token.clone())),
            None => (None, None),
        };
        let config = EngineConfig {
            engine: self.engine.clone(),
            proxy,
            proxy_token,
            gvisor: !start.engine_sandbox,
            width: start.width,
            height: start.height,
            engine_log: self.engine_log,
            offline: start.proxy.is_none(),
        };
        // Whether a person can be asked comes with the run's `start`.
        let control = Arc::new(Control::new(start.attended, REQUEST_USER_TIMEOUT));
        let driver = CdpDriver::start(&config, control)
            .await
            .map_err(|_| StartError::EngineStartFailed)?;
        let version = driver
            .engine_version()
            .await
            .map_err(|_| StartError::EngineStartFailed)?;
        *self.driver.lock().await = Some(Arc::new(driver));
        Ok(version)
    }

    async fn call(&self, name: &str, args: &Value) -> ToolOutput {
        match self.driver().await {
            Some(driver) => dispatch(driver.as_ref(), name, args).await,
            None => ToolOutput::Json(
                BrowserError::new(ErrorCode::Internal, "the browser is not started").to_result(),
            ),
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

    async fn run_ended(&self) {}

    async fn stop(&self) {
        if let Some(driver) = self.driver.lock().await.take() {
            driver.shutdown();
        }
    }
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
    async fn no_sandbox_outside_gvisor_and_lite_are_refused() {
        let mut session = Session::new("/nonexistent/engine".into(), false);
        session.gvisor = || false;
        assert_eq!(
            session.start(&start("full", false)).await,
            Err(StartError::IsolationRefused)
        );
        assert_eq!(
            session.start(&start("lite", true)).await,
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

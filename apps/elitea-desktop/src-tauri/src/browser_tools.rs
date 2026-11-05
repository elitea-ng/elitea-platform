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
//! What this module does NOT decide (the seam is [`gate`]): the tool
//! kinds' approvals and plan-mode rule (`BrowserRead`, `BrowserNavigate`,
//! `BrowserAct`, ADR-0048 decision 8) and the `local_work_browser*` policy
//! keys are #1590's.

use std::collections::HashMap;
use std::sync::{Arc, Mutex};

use adk_core::{ReadonlyContext, Tool, ToolContext, Toolset};
use async_trait::async_trait;
use base64::Engine as _;
use elitea_agent_runtime::host::{HostError, ToolProvider, ToolsetRequest};
use elitea_agent_runtime::image_result::{IMAGE_FIELD, IMAGE_KIND};
use elitea_browser_tools::driver::ToolOutput;
use elitea_browser_tools::{BrowserDriver, BrowserError, ErrorCode, TOOLS, ToolSpec, dispatch};
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

/// The approvals and policy seam (#1590).
///
/// TODO(#1590): the `local_work_browser` and `local_work_browser_agent`
/// policy keys decide whether this toolset is offered at all; each call's
/// [`elitea_browser_tools::ToolKind`] goes through the approval rows of
/// ADR-0048 decision 8 here (plan mode denies `BrowserNavigate` and
/// `BrowserAct`; a submit, an upload, a download and a consequential
/// control are asked; unattended grants answer `browser_submits`). Until
/// then every call runs, as the pane is the person's to open and every
/// action shows in it.
fn gate(_spec: &ToolSpec, _arguments: &Value) -> Result<(), BrowserError> {
    Ok(())
}

/// One browser tool of one thread.
struct BrowserTool {
    spec: &'static ToolSpec,
    thread: String,
    drivers: Arc<BrowserDrivers>,
    /// The model can see images: a screenshot carries its bytes.
    vision: bool,
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

    async fn execute(&self, _ctx: Arc<dyn ToolContext>, args: Value) -> adk_core::Result<Value> {
        Ok(self.run(&args).await)
    }
}

impl BrowserTool {
    async fn run(&self, args: &Value) -> Value {
        if let Err(error) = gate(self.spec, args) {
            return error.to_result();
        }
        let Some(driver) = self.drivers.get(&self.thread) else {
            return BrowserError::new(
                ErrorCode::Unsupported,
                "the browser pane of this thread is not open: ask the person to open it (Thread > Browser)",
            )
            .to_result();
        };
        tool_result(
            dispatch(driver.as_ref(), self.spec.name, args).await,
            self.vision,
        )
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
    pub fn new(thread: &str, drivers: Arc<BrowserDrivers>, vision: bool) -> Self {
        Self {
            toolset: Arc::new(BrowserToolset {
                thread: thread.to_owned(),
                drivers,
                vision,
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
}

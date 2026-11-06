//! The [`BrowserDriver`] trait and [`dispatch`], the one path from a tool
//! call to its result on both hosts.
//!
//! A driver does the engine work and nothing else: it never parses model
//! arguments, never renders a snapshot and never frames page text. Those are
//! [`dispatch`]'s, so the model sees byte-identical shapes whichever driver
//! answered. What stays with the driver, because only it knows the page:
//!
//! * **Refs.** It keeps a [`SnapshotSlot`], builds every snapshot with
//!   [`Snapshot::build`] under [`SnapshotSlot::next_id`], installs it, and
//!   resolves refs through the slot ([`SnapshotSlot::resolve_for_input`] for
//!   `type_text` and for `press` into the focused element). A navigation
//!   (open, back, forward, reload, or an act that loads a new document)
//!   calls [`SnapshotSlot::invalidate`] and reports
//!   [`PageInfo::refs_stale`]. Within one document a ref stays valid until
//!   the next snapshot, unless its element is gone, which is `stale_ref`.
//! * **Hard limits** it alone can see: a secure field
//!   ([`crate::snapshot::is_secure_field`]) and the focused element for
//!   `press`; [`crate::call::check_url`] on every navigation the page itself
//!   starts (a link, a redirect, a `window.open`), not only on
//!   `browser_open`, which [`crate::call`] already checked.
//! * **The page's host**: the domain policy and the egress allowlist
//!   (`egress_refused`).
//! * **Control and rate**: `user_active` and `user_in_control` (ADR-0048
//!   decision 4) and [`crate::rate::ActRate`] for every `BrowserAct`.
//!
//! Approvals, plan mode and audit are the host's, before and after
//! [`dispatch`].

use std::time::Duration;

use async_trait::async_trait;
use serde_json::{Value, json};

use crate::call::{BrowserCall, ElementRef, Key, ScrollTarget, SnapshotMode, WaitCondition};
use crate::error::BrowserError;
use crate::frame;
#[cfg(doc)]
use crate::snapshot::SnapshotSlot;
use crate::snapshot::{MAX_NAME_CHARS, Snapshot, collapse, cut_url};

/// Where the page is after a call.
#[derive(Clone, Debug, Default, Eq, PartialEq)]
pub struct PageInfo {
    pub url: String,
    pub title: String,
    /// A new document loaded: every ref given out before is stale.
    pub refs_stale: bool,
}

/// The page's readable text (`browser_snapshot` with `mode: "text"`).
#[derive(Clone, Debug, Default, Eq, PartialEq)]
pub struct PageText {
    pub url: String,
    pub title: String,
    pub text: String,
}

/// A screenshot: PNG or JPEG bytes.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct Screenshot {
    /// `image/png` or `image/jpeg`.
    pub media_type: &'static str,
    pub data: Vec<u8>,
    pub width: u32,
    pub height: u32,
}

/// The person handed the page back (`browser_request_user`).
#[derive(Clone, Debug, Default, Eq, PartialEq)]
pub struct Handback {
    /// Their note ("signed in, continue"), which the host also adds to the
    /// conversation as the person's message.
    pub note: Option<String>,
}

/// `browser_back`, `browser_forward` or `browser_reload`.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum HistoryStep {
    Back,
    Forward,
    Reload,
}

/// One browser page, driven by an engine. See the module docs for what a
/// driver must enforce itself.
#[async_trait]
pub trait BrowserDriver: Send + Sync {
    /// Load `url` (already scheme-checked) and wait for the document.
    async fn open(&self, url: &str) -> Result<PageInfo, BrowserError>;

    /// A new snapshot of at most `max_nodes` nodes, installed in the
    /// driver's slot.
    async fn snapshot(&self, max_nodes: usize) -> Result<Snapshot, BrowserError>;

    /// The page's readable text; [`dispatch`] cuts it to `max_chars`.
    async fn text(&self, max_chars: usize) -> Result<PageText, BrowserError>;

    /// The visible page, or the element `target` names.
    async fn screenshot(&self, target: Option<&ElementRef>) -> Result<Screenshot, BrowserError>;

    async fn click(&self, target: &ElementRef, double: bool) -> Result<PageInfo, BrowserError>;

    /// Type into a text field; `secure_field_refused` for a secure one.
    async fn type_text(
        &self,
        target: &ElementRef,
        text: &str,
        clear: bool,
        submit: bool,
    ) -> Result<PageInfo, BrowserError>;

    /// Choose options (labels or values) of a select.
    async fn select(
        &self,
        target: &ElementRef,
        values: &[String],
    ) -> Result<PageInfo, BrowserError>;

    async fn scroll(&self, target: &ScrollTarget) -> Result<PageInfo, BrowserError>;

    /// One key or chord into the focused element; `secure_field_refused`
    /// when that element is secure.
    async fn press(&self, key: &Key) -> Result<PageInfo, BrowserError>;

    async fn history(&self, step: HistoryStep) -> Result<PageInfo, BrowserError>;

    /// Until `condition` holds; `timeout` after `timeout`.
    async fn wait(
        &self,
        condition: &WaitCondition,
        timeout: Duration,
    ) -> Result<PageInfo, BrowserError>;

    /// Hand control to the person and wait for them: `no_person` in a run
    /// with nobody to ask, `declined` when they decline or time out.
    async fn request_user(&self, reason: &str) -> Result<Handback, BrowserError>;

    /// The element a `browser_press` would go to (the focused one, through
    /// shadow roots and the frames the driver can reach), for the host's
    /// rules before the press runs. `Ok(None)`: nothing has the focus (the
    /// body). A driver that cannot tell answers an error, and the host
    /// treats the press as on an unknown control (ADR-0048 decision 8: fail
    /// closed).
    async fn focused(&self) -> Result<Option<ControlInfo>, BrowserError> {
        Err(BrowserError::new(
            crate::ErrorCode::Unsupported,
            "this driver cannot read the focused element",
        ))
    }

    /// What the element `target` names is, beyond its snapshot line (an
    /// input's `type`, a link's `download` attribute), for the host's rules
    /// before an act on it runs. An error: the host decides from the
    /// snapshot alone.
    async fn inspect(&self, _target: &ElementRef) -> Result<ControlInfo, BrowserError> {
        Err(BrowserError::new(
            crate::ErrorCode::Unsupported,
            "this driver cannot inspect an element",
        ))
    }

    /// The address the page shows now (not the agent's last view of it),
    /// for the host's rules before a call runs.
    async fn current_url(&self) -> Result<String, BrowserError> {
        Err(BrowserError::new(
            crate::ErrorCode::Unsupported,
            "this driver cannot read the page's address",
        ))
    }
}

/// An element as [`BrowserDriver::focused`] and [`BrowserDriver::inspect`]
/// read it.
#[derive(Clone, Debug, Default, Eq, PartialEq)]
pub struct ControlInfo {
    /// Its role, as a snapshot gives it (`button`, `textbox`, …).
    pub role: String,
    /// Its accessible name.
    pub name: String,
    /// The `type` attribute of an `input` (`file`, `submit`, …), lowercase.
    pub input_type: Option<String>,
    /// A link with a `download` attribute.
    pub download: bool,
}

/// What a browser tool call returns to the host.
#[derive(Clone, Debug, PartialEq)]
pub enum ToolOutput {
    /// A JSON result (every tool, failures included).
    Json(Value),
    /// `browser_screenshot`: the image for the model, and its JSON result.
    Image {
        media_type: &'static str,
        data: Vec<u8>,
        result: Value,
    },
}

impl ToolOutput {
    /// The JSON result.
    #[must_use]
    pub fn result(&self) -> &Value {
        match self {
            Self::Json(result) | Self::Image { result, .. } => result,
        }
    }
}

/// Parse a tool call, run it on `driver`, and shape its result.
///
/// Every page-controlled string (URL, title, snapshot, text) is in one
/// `content` field inside a fresh nonce frame ([`frame`]). The other fields
/// are the contract's own:
///
/// | Tool | Result fields |
/// | --- | --- |
/// | navigations, acts, `browser_wait` | `refs_stale`, `content` (`# url`, `# title`) |
/// | `browser_snapshot` (tree) | `snapshot`, `refs`, `truncated`, `omitted`, `content` (the rendered snapshot) |
/// | `browser_snapshot` (text) | `truncated`, `content` (`# url`, `# title`, the text) |
/// | `browser_screenshot` | `media_type`, `width`, `height`, and the image |
/// | `browser_request_user` | `note` (the person's, not framed) |
///
/// All carry `"status": "ok"`; a failure is [`BrowserError::to_result`].
pub async fn dispatch(driver: &dyn BrowserDriver, name: &str, arguments: &Value) -> ToolOutput {
    let call = match BrowserCall::parse(name, arguments) {
        Ok(call) => call,
        Err(error) => return ToolOutput::Json(error.to_result()),
    };
    match run(driver, call).await {
        Ok(output) => output,
        Err(error) => ToolOutput::Json(error.to_result()),
    }
}

async fn run(driver: &dyn BrowserDriver, call: BrowserCall) -> Result<ToolOutput, BrowserError> {
    let page = match call {
        BrowserCall::Open { url } => driver.open(&url).await?,
        BrowserCall::Snapshot {
            mode: SnapshotMode::Tree,
            max_nodes,
            ..
        } => {
            let snapshot = driver.snapshot(max_nodes).await?;
            return Ok(ToolOutput::Json(json!({
                "status": "ok",
                "snapshot": snapshot.id.as_str(),
                "refs": snapshot.ref_count(),
                "truncated": snapshot.omitted > 0,
                "omitted": snapshot.omitted,
                "content": frame::frame(&snapshot.render())?,
            })));
        }
        BrowserCall::Snapshot {
            mode: SnapshotMode::Text,
            max_chars,
            ..
        } => {
            let text = driver.text(max_chars).await?;
            let (body, truncated) = cut_chars(&text.text, max_chars);
            let content = format!("{}{body}", page_header(&text.url, &text.title));
            return Ok(ToolOutput::Json(json!({
                "status": "ok",
                "truncated": truncated,
                "content": frame::frame(&content)?,
            })));
        }
        BrowserCall::Screenshot { target } => {
            let shot = driver.screenshot(target.as_ref()).await?;
            if !matches!(shot.media_type, "image/png" | "image/jpeg") || shot.data.is_empty() {
                return Err(BrowserError::new(
                    crate::ErrorCode::Internal,
                    "the driver returned no PNG or JPEG image",
                ));
            }
            return Ok(ToolOutput::Image {
                media_type: shot.media_type,
                result: json!({
                    "status": "ok",
                    "media_type": shot.media_type,
                    "width": shot.width,
                    "height": shot.height,
                }),
                data: shot.data,
            });
        }
        BrowserCall::Click { target, double } => driver.click(&target, double).await?,
        BrowserCall::Type {
            target,
            text,
            clear,
            submit,
        } => driver.type_text(&target, &text, clear, submit).await?,
        BrowserCall::Select { target, values } => driver.select(&target, &values).await?,
        BrowserCall::Scroll(target) => driver.scroll(&target).await?,
        BrowserCall::Press(key) => driver.press(&key).await?,
        BrowserCall::Back => driver.history(HistoryStep::Back).await?,
        BrowserCall::Forward => driver.history(HistoryStep::Forward).await?,
        BrowserCall::Reload => driver.history(HistoryStep::Reload).await?,
        BrowserCall::Wait { condition, timeout } => driver.wait(&condition, timeout).await?,
        BrowserCall::RequestUser { reason } => {
            let handback = driver.request_user(&reason).await?;
            return Ok(ToolOutput::Json(json!({
                "status": "ok",
                "note": handback.note,
            })));
        }
    };
    Ok(ToolOutput::Json(json!({
        "status": "ok",
        "refs_stale": page.refs_stale,
        "content": frame::frame(&page_header(&page.url, &page.title))?,
    })))
}

/// The `# url` and `# title` lines.
fn page_header(url: &str, title: &str) -> String {
    let title = collapse(title, MAX_NAME_CHARS);
    format!(
        "# url {}\n# title {}\n",
        Value::String(cut_url(url)),
        Value::String(title)
    )
}

fn cut_chars(text: &str, max: usize) -> (&str, bool) {
    match text.char_indices().nth(max) {
        Some((at, _)) => (&text[..at], true),
        None => (text, false),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn cuts_on_characters() {
        assert_eq!(cut_chars("héllo", 2), ("hé", true));
        assert_eq!(cut_chars("hé", 2), ("hé", false));
    }

    #[test]
    fn header_quotes_page_strings() {
        assert_eq!(
            page_header("https://a.test/\"x", " Two\n words "),
            "# url \"https://a.test/\\\"x\"\n# title \"Two words\"\n"
        );
    }

    #[test]
    fn header_cuts_the_url() {
        let long = format!(
            "https://a.test/{}",
            "a".repeat(crate::snapshot::MAX_URL_CHARS)
        );
        let header = page_header(&long, "t");
        let url_line = header.lines().next().unwrap();
        let url: String = serde_json::from_str(url_line.trim_start_matches("# url ")).unwrap();
        assert_eq!(url.chars().count(), crate::snapshot::MAX_URL_CHARS);
        assert!(url.ends_with('…'));
        let title: String = serde_json::from_str(
            header
                .lines()
                .nth(1)
                .unwrap()
                .trim_start_matches("# title "),
        )
        .unwrap();
        assert_eq!(title, "t");
        let long_title = page_header("u", &"t".repeat(MAX_NAME_CHARS + 1));
        assert!(long_title.contains(&format!("{}…\"", "t".repeat(MAX_NAME_CHARS - 1))));
    }
}

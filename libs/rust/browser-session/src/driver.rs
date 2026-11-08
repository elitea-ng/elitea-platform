//! The CDP driver: [`BrowserDriver`] over chrome-headless-shell.
//!
//! The ADR-0049 decision 4 mapping:
//!
//! | Contract | CDP |
//! | --- | --- |
//! | `snapshot` | `Accessibility.getFullAXTree` per frame + `DOM.getDocument` (for `type`/`autocomplete`) → [`crate::ax`] → [`Snapshot::build`] |
//! | `click`, `scroll`, `press` | `Input.dispatchMouseEvent`, `Input.dispatchKeyEvent` at the element's `DOM.getContentQuads` centre |
//! | `type_text` | `DOM.focus`, then `Input.insertText` |
//! | `screenshot` | `Page.captureScreenshot` (a clip for an element) |
//! | view stream | `Page.startScreencast` ([`crate::screencast`]) |
//!
//! The driver's own page scripts (focus, select, the text read) run in an
//! isolated world (`Page.createIsolatedWorld`), so the page's scripts cannot
//! see or patch them. The model never supplies a script.
//!
//! **Refs.** A ref resolves only against the latest snapshot, and only while
//! the page is the one that snapshot read: a new main-frame document, the
//! person's input, or lost engine events invalidate the slot. Within one
//! document a ref stays valid until the next snapshot, unless its element is
//! gone (`DOM.resolveNode` fails, or it is no longer connected): `stale_ref`.
//!
//! **Hard limits** (ADR-0048 decision 5): a secure field is refused on the
//! snapshot's flag *and* on the live element's `type` and `autocomplete`
//! just before typing (a page can flip `type` after the snapshot); `press`
//! checks the focused element the same way, through shadow roots and
//! frames; every `BrowserAct` passes the pane's [`ActRate`]; a navigation
//! the page starts to a refused scheme is sent back to `about:blank` and the
//! call fails with `scheme_refused` ([`crate::page`]).

use std::collections::{HashMap, HashSet};
use std::sync::{Arc, Mutex, MutexGuard, PoisonError};
use std::time::{Duration, Instant};

use async_trait::async_trait;
use base64::Engine as _;
use elitea_browser_tools::call::{
    Direction, ElementRef, Key, ScrollTarget, WaitCondition, url_matches,
};
use elitea_browser_tools::driver::{
    BrowserDriver, Handback, HistoryStep, PageInfo, PageText, Screenshot,
};
use elitea_browser_tools::rate::ActRate;
use elitea_browser_tools::snapshot::{Snapshot, SnapshotSlot, is_secure_field};
use elitea_browser_tools::{BrowserError, ErrorCode};
use serde_json::{Value, json};

use crate::ax::{self, FrameTree, Handle};
use crate::cdp::{CdpError, Connection};
use crate::control::{Answer, Control, Driver};
use crate::engine::{Engine, EngineConfig, EngineKind};
use crate::keys;
use crate::page::{self, Marks, TUNNEL_FAILED, Tracker, Watch};
use crate::screencast::Screencast;

/// How long a navigation may take to load.
pub const NAVIGATION_TIMEOUT: Duration = Duration::from_secs(30);
/// How long after an act a navigation it caused may take to start.
const NAVIGATION_GRACE: Duration = Duration::from_millis(150);
/// At most this many frames are read into one snapshot.
const MAX_FRAMES: usize = 32;
const WORLD: &str = "elitea_browser_driver";

const INSPECT: &str = r"function () {
  const el = this;
  const tag = el.tagName;
  const nonText = ['button','submit','reset','checkbox','radio','file','image','range','color','hidden'];
  const editable = el.isContentEditable || tag === 'TEXTAREA' ||
    (tag === 'INPUT' && !nonText.includes(String(el.type || '').toLowerCase()));
  return {
    type: tag === 'INPUT' ? el.type : el.getAttribute('type'),
    autocomplete: el.getAttribute('autocomplete'),
    editable: !!editable,
    disabled: !!(el.disabled || el.readOnly),
    connected: el.isConnected,
  };
}";

const PREPARE: &str = r"function (clear) {
  const el = this;
  el.focus();
  try {
    if (clear) {
      if (!el.isContentEditable && typeof el.select === 'function') { el.select(); }
      else {
        const range = document.createRange();
        range.selectNodeContents(el);
        const selection = getSelection();
        selection.removeAllRanges();
        selection.addRange(range);
      }
    } else if (!el.isContentEditable && typeof el.value === 'string') {
      const end = el.value.length;
      el.setSelectionRange(end, end);
    }
  } catch (error) {}
  return document.activeElement === el || el.contains(document.activeElement);
}";

const FOCUSED: &str = r"function () {
  let el = document.activeElement;
  for (let hops = 0; el && hops < 64; hops += 1) {
    if (el.shadowRoot && el.shadowRoot.activeElement) { el = el.shadowRoot.activeElement; continue; }
    if (el.tagName === 'IFRAME' || el.tagName === 'FRAME') {
      let inner = null;
      try { inner = el.contentDocument; } catch (error) {}
      if (inner) { el = inner.activeElement; continue; }
      return { type: null, autocomplete: null, crossFrame: true };
    }
    break;
  }
  if (!el || el === document.body) return { type: null, autocomplete: null, crossFrame: false, focused: document.hasFocus() };
  return {
    type: el.tagName === 'INPUT' ? el.type : el.getAttribute('type'),
    autocomplete: el.getAttribute('autocomplete'),
    crossFrame: false,
    focused: document.hasFocus(),
  };
}";

const SELECT: &str = r"function (values) {
  if (this.tagName !== 'SELECT') return { error: 'not_select' };
  if (this.disabled) return { error: 'disabled' };
  const options = Array.from(this.options);
  const chosen = [];
  const missing = [];
  for (const wanted of values) {
    const trimmed = wanted.trim();
    const option = options.find((o) => o.label.trim() === trimmed) ||
      options.find((o) => o.value === wanted) ||
      options.find((o) => (o.textContent || '').trim() === trimmed);
    if (option) chosen.push(option); else missing.push(wanted);
  }
  if (missing.length) return { error: 'missing', missing };
  if (!this.multiple && chosen.length > 1) return { error: 'single' };
  for (const option of options) option.selected = chosen.includes(option);
  this.dispatchEvent(new Event('input', { bubbles: true }));
  this.dispatchEvent(new Event('change', { bubbles: true }));
  return { ok: true };
}";

const CLICK: &str = "function () { this.click(); }";

const TEXT: &str = r"function (limit) {
  const body = document.body;
  const text = body ? body.innerText : '';
  return text.length > limit ? text.slice(0, limit) : text;
}";

const HAS_TEXT: &str = r"function (needle) {
  const body = document.body;
  return !!body && body.innerText.includes(needle);
}";

/// What the page shows, read from the DOM (`CdpDriver::page_content`):
/// whether its body has readable text, and whether it has controls
/// (links, buttons, fields), looking through open shadow roots too
/// (web-component pages keep their links there). The walk stops at the
/// first root with a control or after `limit` elements. A control
/// counts only when it is rendered (`getClientRects`) and not inside
/// `aria-hidden="true"`: Chrome's tree leaves both out too, so neither
/// is a ref Moli missed.
const CONTENT: &str = r#"function (limit) {
  const body = document.body;
  const text = !!body && /[\p{L}\p{N}]/u.test(body.innerText.slice(0, 4000));
  const controls = 'a[href],button,input:not([type=hidden]),select,textarea,summary,' +
    '[role=button],[role=link],[role=textbox],[role=searchbox],[role=combobox],' +
    '[role=checkbox],[role=tab],[role=menuitem],[contenteditable=""],[contenteditable=true]';
  let found = 0;
  let seen = 0;
  const roots = [document];
  while (roots.length && found === 0 && seen < limit) {
    const root = roots.pop();
    for (const el of root.querySelectorAll(controls)) {
      if (el.getClientRects().length > 0 && !el.closest('[aria-hidden="true"]')) { found += 1; break; }
    }
    const walker = document.createTreeWalker(root, NodeFilter.SHOW_ELEMENT);
    for (let el = walker.nextNode(); el && seen < limit; el = walker.nextNode()) {
      seen += 1;
      if (el.shadowRoot) roots.push(el.shadowRoot);
    }
  }
  return { text, controls: found };
}"#;

/// The most elements [`CONTENT`] walks.
const CONTENT_WALK_LIMIT: u64 = 20_000;

/// [`PageContent`] from the page script's answer (`None`: it did not
/// run) and whether the snapshot's DOM read had a control. The script sees
/// layout and `aria-hidden`, so its answer stands; the DOM count, which
/// sees neither, is a "yes" only when the script did not run (on Moli it
/// may not).
#[must_use]
pub(crate) fn page_content_of(script: Option<&Value>, in_dom: bool) -> PageContent {
    match script {
        Some(script) if script["text"].is_boolean() => PageContent {
            text: script["text"] == true,
            controls: script["controls"].as_u64().is_some_and(|found| found > 0),
        },
        _ => PageContent {
            text: false,
            controls: in_dom,
        },
    }
}

/// What `CdpDriver::page_content` found.
#[derive(Clone, Copy, Debug, Default, Eq, PartialEq)]
pub struct PageContent {
    /// The body has a letter or a digit.
    pub text: bool,
    /// The page has a control.
    pub controls: bool,
}

/// What a call's start recorded, for its end.
#[derive(Clone, Copy)]
struct Gate {
    input_epoch: u64,
    /// A watchable call while the person drives.
    watching: bool,
}

/// The ref state of the latest snapshot.
#[derive(Default)]
struct Refs {
    slot: SnapshotSlot,
    handles: Vec<Handle>,
    hosts: HashMap<String, usize>,
    /// `(documents, dom_epoch, input_epoch)` when the snapshot was read.
    taken_at: Option<(u64, u64, u64)>,
    /// Isolated worlds by `(session, frame)`, for `worlds_document`.
    worlds: HashMap<(String, String), i64>,
    worlds_document: u64,
    /// Controls in the DOM the latest snapshot read (shadow roots and
    /// frames pierced), for `auto`'s empty-snapshot rule.
    dom_controls: usize,
}

/// A browser session's one pane, driven over CDP.
pub struct CdpDriver {
    cdp: Connection,
    /// Keeps the engine alive; `None` for a driver over a foreign connection.
    engine: Mutex<Option<Engine>>,
    session: String,
    target: String,
    tracker: Tracker,
    refs: Mutex<Refs>,
    rate: Mutex<ActRate>,
    control: Arc<Control>,
    screencast: Screencast,
    width: u32,
    height: u32,
    kind: EngineKind,
}

fn lock<T>(mutex: &Mutex<T>) -> MutexGuard<'_, T> {
    mutex.lock().unwrap_or_else(PoisonError::into_inner)
}

fn engine_error(error: &CdpError) -> BrowserError {
    match error {
        CdpError::Timeout => {
            BrowserError::new(ErrorCode::Timeout, "the browser did not answer in time")
        }
        CdpError::Closed => BrowserError::new(ErrorCode::Internal, "the browser session ended"),
        CdpError::Protocol { message, .. } => BrowserError::new(
            ErrorCode::Internal,
            format!(
                "the browser refused the command: {}",
                message.chars().take(200).collect::<String>()
            ),
        ),
    }
}

impl CdpDriver {
    /// Start the engine and set up its one page.
    ///
    /// # Errors
    ///
    /// The engine did not start or did not answer.
    pub async fn start(config: &EngineConfig, control: Arc<Control>) -> Result<Self, BrowserError> {
        let (engine, cdp) = Engine::start(config).await.map_err(|error| {
            BrowserError::new(
                ErrorCode::Internal,
                format!("the browser did not start: {error}"),
            )
        })?;
        let mut driver = Self::attach(cdp, config, control).await?;
        driver.engine = Mutex::new(Some(engine));
        Ok(driver)
    }

    /// Set up the page of an engine already connected on `cdp`.
    ///
    /// # Errors
    ///
    /// The engine did not answer, or has no page.
    pub async fn attach(
        cdp: Connection,
        config: &EngineConfig,
        control: Arc<Control>,
    ) -> Result<Self, BrowserError> {
        let events = cdp.events();
        let call = |session: Option<String>, method: &'static str, params: Value| {
            let cdp = cdp.clone();
            async move {
                cdp.call(session.as_deref(), method, params)
                    .await
                    .map_err(|error| engine_error(&error))
            }
        };
        call(
            None,
            "Target.setDiscoverTargets",
            json!({ "discover": true }),
        )
        .await?;
        // Downloads go nowhere: the session has no file to give anyone.
        let _ = call(
            None,
            "Browser.setDownloadBehavior",
            json!({ "behavior": "deny" }),
        )
        .await;
        let target = first_page(&cdp).await?;
        let attached = call(
            None,
            "Target.attachToTarget",
            json!({ "targetId": target, "flatten": true }),
        )
        .await?;
        let session = attached["sessionId"]
            .as_str()
            .ok_or_else(|| BrowserError::new(ErrorCode::Internal, "the page did not attach"))?
            .to_owned();
        let tracker = Tracker::new();
        page::spawn(
            Watch {
                cdp: cdp.clone(),
                page_session: session.clone(),
                page_target: target.clone(),
                proxy_token: config.proxy_token.clone(),
            },
            tracker.clone(),
            events,
        );
        let on_page = Some(session.clone());
        if config.proxy_token.is_some() {
            call(on_page.clone(), "Fetch.enable", page::fetch_params()).await?;
        }
        for method in ["Page.enable", "Network.enable"] {
            call(on_page.clone(), method, json!({})).await?;
        }
        call(
            on_page.clone(),
            "Emulation.setDeviceMetricsOverride",
            json!({
                "width": config.width, "height": config.height,
                "deviceScaleFactor": 1, "mobile": false,
            }),
        )
        .await?;
        call(
            on_page.clone(),
            "Target.setAutoAttach",
            page::auto_attach_params(),
        )
        .await?;
        let screencast = Screencast::new(
            cdp.clone(),
            session.clone(),
            config.width,
            config.height,
            cdp.events(),
        );
        Ok(Self {
            cdp,
            engine: Mutex::new(None),
            session,
            target,
            tracker,
            refs: Mutex::new(Refs::default()),
            rate: Mutex::new(ActRate::new()),
            control,
            screencast,
            width: config.width,
            height: config.height,
            kind: config.kind,
        })
    }

    /// Which engine this driver drives.
    #[must_use]
    pub const fn kind(&self) -> EngineKind {
        self.kind
    }

    /// How many commands the engine refused as unsupported so far (`auto`
    /// switches on a rise).
    #[must_use]
    pub fn unsupported_errors(&self) -> u64 {
        self.cdp.unsupported_errors()
    }

    /// The engine's process id, while it runs.
    #[must_use]
    pub fn engine_pid(&self) -> Option<u32> {
        lock(&self.engine).as_ref().and_then(Engine::pid)
    }

    /// The view stream (#1597 relays it).
    #[must_use]
    pub fn screencast(&self) -> &Screencast {
        &self.screencast
    }

    /// The pane's control state.
    #[must_use]
    pub fn control(&self) -> &Arc<Control> {
        &self.control
    }

    /// The page's URL and title as the events last reported them (for a
    /// view frame; no engine round trip).
    #[must_use]
    pub fn location(&self) -> (String, String) {
        let state = self.tracker.lock();
        (state.url.clone(), state.title.clone())
    }

    /// Open `url` outside the agent's gate: an `auto` switch reopening the
    /// page on the new engine. It runs while a person drives too, since
    /// their take-over carries over to the new engine. Best effort: a page
    /// that does not load is reported by the call that follows.
    pub(crate) async fn reopen(&self, url: &str) {
        let before = self.tracker.marks();
        if let Ok(navigated) = self.page("Page.navigate", json!({ "url": url })).await
            && navigated["loaderId"]
                .as_str()
                .is_some_and(|loader| !loader.is_empty())
        {
            self.loaded(before, NAVIGATION_TIMEOUT).await;
        }
    }

    /// What the page shows, read from the DOM rather than the
    /// accessibility tree. `auto` asks after a tree snapshot gave out no
    /// ref: text or a control there is something the tree should have
    /// shown. Controls come from two reads: the `DOM.getDocument` the
    /// latest snapshot already made (shadow roots and frames pierced, no
    /// page script involved) and a page script, which also tells text.
    /// Outside the agent's gate, like [`Self::reopen`]; a script that does
    /// not run finds no text.
    pub(crate) async fn page_content(&self) -> PageContent {
        let in_dom = lock(&self.refs).dom_controls > 0;
        let script = self
            .main_frame(CONTENT, vec![json!(CONTENT_WALK_LIMIT)])
            .await
            .ok();
        page_content_of(script.as_ref(), in_dom)
    }

    /// How many main-frame documents the page has had: an `auto` open
    /// that timed out reached one when this rose during the call.
    pub(crate) fn documents(&self) -> u64 {
        self.tracker.marks().documents
    }

    /// The engine's product, `chrome-headless-shell/<version>` or
    /// `moli/<version>`.
    ///
    /// # Errors
    ///
    /// The engine did not answer.
    pub async fn engine_version(&self) -> Result<String, BrowserError> {
        let version = self
            .cdp
            .call(None, "Browser.getVersion", json!({}))
            .await
            .map_err(|error| engine_error(&error))?;
        let product = version["product"].as_str().unwrap_or_default();
        let number: String = product
            .rsplit('/')
            .next()
            .unwrap_or_default()
            .chars()
            .filter(|c| c.is_ascii_alphanumeric() || matches!(c, '.' | '-' | '+'))
            .take(64)
            .collect();
        Ok(format!("{}/{number}", self.kind.product()))
    }

    /// A viewer's input event (the agent line protocol's `input` `event`):
    /// the person's pointer, wheel or key, `take_over` or `hand_back`.
    /// Pointer, wheel and key pause the agent before they reach the page.
    ///
    /// # Errors
    ///
    /// An event of an unknown type, or the engine refusing it.
    pub async fn person_input(&self, event: &Value) -> Result<(), BrowserError> {
        let number = |name: &str| event[name].as_f64().unwrap_or_default();
        let modifiers = event["modifiers"].as_u64().unwrap_or_default() & 0x0f;
        let params = match event["type"].as_str() {
            Some("take_over") => {
                self.control.take_over();
                return Ok(());
            }
            Some("hand_back") => {
                let note = event["note"]
                    .as_str()
                    .filter(|note| !note.trim().is_empty())
                    .map(|note| note.chars().take(2048).collect());
                self.control.hand_back(note);
                return Ok(());
            }
            Some("pointer") => ("Input.dispatchMouseEvent", pointer_params(event)?),
            Some("wheel") => (
                "Input.dispatchMouseEvent",
                json!({
                    "type": "mouseWheel", "x": number("x"), "y": number("y"),
                    "deltaX": number("delta_x"), "deltaY": number("delta_y"), "modifiers": modifiers,
                }),
            ),
            Some("key") => ("Input.dispatchKeyEvent", key_params(event)?),
            _ => return Err(BrowserError::invalid("unknown input event")),
        };
        // The person's input pauses the agent first, then reaches the page.
        self.control.person_input();
        self.page(params.0, params.1).await.map(|_| ())
    }

    /// Stop the engine (and remove its profile).
    pub fn shutdown(&self) {
        lock(&self.engine).take();
    }

    /// Whether the engine is still running.
    pub fn is_alive(&self) -> bool {
        let engine_alive = lock(&self.engine).as_mut().is_none_or(Engine::is_running);
        engine_alive && self.cdp.is_open()
    }

    async fn call(
        &self,
        session: &str,
        method: &str,
        params: Value,
    ) -> Result<Value, BrowserError> {
        self.cdp
            .call(Some(session), method, params)
            .await
            .map_err(|error| engine_error(&error))
    }

    /// A command to the pane's page. While a navigation swaps the page's
    /// document into another process (an error page after a failed load is
    /// one), the engine briefly answers "Not attached to an active page";
    /// the session itself survives, so the command is retried for a moment.
    async fn page(&self, method: &str, params: Value) -> Result<Value, BrowserError> {
        let deadline = tokio::time::Instant::now() + PAGE_SWAP_GRACE;
        loop {
            match self
                .cdp
                .call(Some(&self.session), method, params.clone())
                .await
            {
                Err(CdpError::Protocol { message, .. })
                    if message.contains(NOT_ATTACHED) && tokio::time::Instant::now() < deadline =>
                {
                    tokio::time::sleep(Duration::from_millis(50)).await;
                }
                result => return result.map_err(|error| engine_error(&error)),
            }
        }
    }

    // ── control, rate and staleness ─────────────────────────────────────

    /// Before any call: the person must not be driving. Returns what
    /// [`Self::end`] compares.
    fn begin(&self) -> Result<Gate, BrowserError> {
        let state = self.control.state();
        if state.driver != Driver::Agent {
            return Err(BrowserError::new(
                ErrorCode::UserInControl,
                "the person is driving the page: wait for them to hand it back",
            ));
        }
        Ok(Gate {
            input_epoch: state.input_epoch,
            watching: false,
        })
    }

    /// Before `browser_snapshot` and `browser_screenshot`: the agent may
    /// watch while the person drives (the agent line protocol's watchable
    /// calls), and the person's input then does not fail the call.
    fn begin_watch(&self) -> Gate {
        let state = self.control.state();
        Gate {
            input_epoch: state.input_epoch,
            watching: state.driver != Driver::Agent,
        }
    }

    /// Before a `BrowserAct`: [`Self::begin`] and the pane's rate.
    fn begin_act(&self) -> Result<Gate, BrowserError> {
        let gate = self.begin()?;
        lock(&self.rate).admit(Instant::now())?;
        Ok(gate)
    }

    /// After a call: the person must not have used the page meanwhile.
    fn end<T>(&self, gate: Gate, result: Result<T, BrowserError>) -> Result<T, BrowserError> {
        if !gate.watching && self.control.state().input_epoch != gate.input_epoch {
            return Err(BrowserError::new(
                ErrorCode::UserActive,
                "the person used the page during this call: the agent is paused",
            ));
        }
        result
    }

    /// Drop the snapshot when the page changed under it.
    fn sync(&self) {
        let marks = self.tracker.marks();
        let input = self.control.state().input_epoch;
        let mut refs = lock(&self.refs);
        if let Some(taken) = refs.taken_at
            && taken != (marks.documents, marks.dom_epoch, input)
        {
            refs.slot.invalidate();
            refs.taken_at = None;
        }
        if refs.worlds_document != marks.documents {
            refs.worlds.clear();
            refs.worlds_document = marks.documents;
        }
    }

    fn invalidate(&self) {
        let mut refs = lock(&self.refs);
        refs.slot.invalidate();
        refs.taken_at = None;
    }

    /// The handle `target` names in the latest snapshot.
    fn handle(&self, target: &ElementRef, input: bool) -> Result<Handle, BrowserError> {
        self.sync();
        let refs = lock(&self.refs);
        let node = if input {
            refs.slot.resolve_for_input(target)?
        } else {
            refs.slot.resolve(target)?
        };
        usize::try_from(node.key)
            .ok()
            .and_then(|key| key.checked_sub(1))
            .and_then(|index| refs.handles.get(index))
            .cloned()
            .ok_or_else(|| {
                BrowserError::invalid(format!("{target} is not an element the browser can act on"))
            })
    }

    // ── isolated-world scripts ──────────────────────────────────────────

    async fn world(&self, session: &str, frame: &str, fresh: bool) -> Result<i64, BrowserError> {
        let key = (session.to_owned(), frame.to_owned());
        if !fresh && let Some(id) = lock(&self.refs).worlds.get(&key) {
            return Ok(*id);
        }
        let created = self
            .call(
                session,
                "Page.createIsolatedWorld",
                json!({ "frameId": frame, "worldName": WORLD, "grantUniveralAccess": false }),
            )
            .await
            .map_err(|_| BrowserError::stale_ref("the element's frame is gone: snapshot again"))?;
        let id = created["executionContextId"]
            .as_i64()
            .ok_or_else(|| BrowserError::new(ErrorCode::Internal, "no isolated world"))?;
        lock(&self.refs).worlds.insert(key, id);
        Ok(id)
    }

    /// Run a driver function with `this` as the element.
    async fn on_element(
        &self,
        handle: &Handle,
        function: &str,
        arguments: Vec<Value>,
    ) -> Result<Value, BrowserError> {
        let mut object = None;
        for fresh in [false, true] {
            let context = self.world(&handle.session, &handle.frame_id, fresh).await?;
            if let Ok(resolved) = self
                .call(
                    &handle.session,
                    "DOM.resolveNode",
                    json!({ "backendNodeId": handle.backend_id, "executionContextId": context }),
                )
                .await
                && let Some(id) = resolved["object"]["objectId"].as_str()
            {
                object = Some(id.to_owned());
                break;
            }
        }
        let object =
            object.ok_or_else(|| BrowserError::stale_ref("the element is gone: snapshot again"))?;
        let arguments: Vec<Value> = arguments
            .into_iter()
            .map(|value| json!({ "value": value }))
            .collect();
        let result = self
            .call(
                &handle.session,
                "Runtime.callFunctionOn",
                json!({
                    "objectId": object, "functionDeclaration": function,
                    "arguments": arguments, "returnByValue": true,
                }),
            )
            .await;
        let cdp = self.cdp.clone();
        let session = handle.session.clone();
        tokio::spawn(async move {
            let _ = cdp
                .call(
                    Some(&session),
                    "Runtime.releaseObject",
                    json!({ "objectId": object }),
                )
                .await;
        });
        script_value(&result?)
    }

    /// Run a driver function in a frame's isolated world.
    async fn in_frame(
        &self,
        session: &str,
        frame: &str,
        function: &str,
        arguments: Vec<Value>,
    ) -> Result<Value, BrowserError> {
        let arguments: Vec<Value> = arguments
            .into_iter()
            .map(|value| json!({ "value": value }))
            .collect();
        let mut last = None;
        for fresh in [false, true] {
            let context = self.world(session, frame, fresh).await?;
            match self
                .call(
                    session,
                    "Runtime.callFunctionOn",
                    json!({
                        "executionContextId": context, "functionDeclaration": function,
                        "arguments": arguments, "returnByValue": true,
                    }),
                )
                .await
            {
                Ok(result) => return script_value(&result),
                Err(error) => last = Some(error),
            }
        }
        Err(last
            .unwrap_or_else(|| BrowserError::new(ErrorCode::Internal, "the script did not run")))
    }

    async fn main_frame(
        &self,
        function: &str,
        arguments: Vec<Value>,
    ) -> Result<Value, BrowserError> {
        self.in_frame(&self.session, &self.target, function, arguments)
            .await
    }

    // ── geometry ────────────────────────────────────────────────────────

    /// The offset of `handle`'s renderer in the page's viewport: the content
    /// boxes of the `iframe`s that host it, where a host is in another
    /// renderer (an out-of-process frame).
    async fn frame_offset(&self, handle: &Handle) -> (f64, f64) {
        let (mut x, mut y) = (0.0, 0.0);
        let (mut session, mut frame) = (handle.session.clone(), handle.frame_id.clone());
        for _ in 0..ax::MAX_FRAME_DEPTH {
            if session == self.session {
                break;
            }
            let host = {
                let refs = lock(&self.refs);
                refs.hosts
                    .get(&frame)
                    .and_then(|index| refs.handles.get(*index))
                    .cloned()
            };
            let Some(host) = host else { break };
            if host.session != session
                && let Ok(model) = self
                    .call(
                        &host.session,
                        "DOM.getBoxModel",
                        json!({ "backendNodeId": host.backend_id }),
                    )
                    .await
            {
                x += model["model"]["content"][0].as_f64().unwrap_or_default();
                y += model["model"]["content"][1].as_f64().unwrap_or_default();
            }
            session = host.session;
            frame = host.frame_id;
        }
        (x, y)
    }

    /// The element's box in the viewport, scrolled into view; `None` when it
    /// has none (hidden, an `option` of a closed select).
    async fn element_box(
        &self,
        handle: &Handle,
    ) -> Result<Option<(f64, f64, f64, f64)>, BrowserError> {
        let _ = self
            .call(
                &handle.session,
                "DOM.scrollIntoViewIfNeeded",
                json!({ "backendNodeId": handle.backend_id }),
            )
            .await;
        let Ok(quads) = self
            .call(
                &handle.session,
                "DOM.getContentQuads",
                json!({ "backendNodeId": handle.backend_id }),
            )
            .await
        else {
            return Ok(None);
        };
        let best = quads["quads"]
            .as_array()
            .into_iter()
            .flatten()
            .filter_map(|quad| {
                let points: Vec<f64> = quad.as_array()?.iter().filter_map(Value::as_f64).collect();
                (points.len() == 8).then_some(points)
            })
            .map(|points| {
                let xs = [points[0], points[2], points[4], points[6]];
                let ys = [points[1], points[3], points[5], points[7]];
                let (left, right) = (min(&xs), max(&xs));
                let (top, bottom) = (min(&ys), max(&ys));
                (left, top, right - left, bottom - top)
            })
            .filter(|(_, _, width, height)| *width >= 1.0 && *height >= 1.0)
            .max_by(|a, b| (a.2 * a.3).total_cmp(&(b.2 * b.3)));
        let Some((left, top, width, height)) = best else {
            return Ok(None);
        };
        let (dx, dy) = self.frame_offset(handle).await;
        Ok(Some((left + dx, top + dy, width, height)))
    }

    async fn mouse(&self, kind: &str, x: f64, y: f64, clicks: u32) -> Result<(), BrowserError> {
        let mut params = json!({ "type": kind, "x": x, "y": y, "modifiers": 0 });
        if kind != "mouseMoved" {
            params["button"] = json!("left");
            params["buttons"] = json!(u32::from(kind == "mousePressed"));
            params["clickCount"] = json!(clicks);
        }
        self.page("Input.dispatchMouseEvent", params)
            .await
            .map(|_| ())
    }

    // ── page info and settling ──────────────────────────────────────────

    async fn title(&self) -> String {
        self.cdp
            .call(
                None,
                "Target.getTargetInfo",
                json!({ "targetId": self.target }),
            )
            .await
            .ok()
            .and_then(|info| info["targetInfo"]["title"].as_str().map(str::to_owned))
            .unwrap_or_default()
    }

    async fn info(&self, before: Marks) -> Result<PageInfo, BrowserError> {
        let after = self.tracker.marks();
        if after.scheme_refusals > before.scheme_refusals {
            self.invalidate();
            return Err(BrowserError::new(
                ErrorCode::SchemeRefused,
                "the page tried to open a refused scheme; it was stopped",
            ));
        }
        if after.egress_refusals > before.egress_refusals {
            return Err(egress_refused());
        }
        let refs_stale = after.documents != before.documents || after.dom_epoch != before.dom_epoch;
        if refs_stale {
            self.invalidate();
        }
        let url = self.tracker.lock().url.clone();
        Ok(PageInfo {
            url,
            title: self.title().await,
            refs_stale,
        })
    }

    /// After an act: if it started a navigation, wait for the new document
    /// to load (bounded; a slow page is reported as it is).
    async fn settle(&self, before: Marks) {
        let started = self
            .tracker
            .wait_until(NAVIGATION_GRACE, |state| {
                state.navigations_requested > before.navigations_requested
            })
            .await;
        if started {
            self.loaded(before, NAVIGATION_TIMEOUT).await;
        }
    }

    /// Wait until a document newer than `before` has loaded.
    async fn loaded(&self, before: Marks, timeout: Duration) -> bool {
        self.tracker
            .wait_until(timeout, |state| {
                (state.documents > before.documents
                    && state.loaded_document == state.documents
                    && !state.loading)
                    || state.scheme_refusals > before.scheme_refusals
                    || state.egress_refusals > before.egress_refusals
            })
            .await
    }

    // ── snapshot reading ────────────────────────────────────────────────

    async fn read_frames(&self) -> Result<HashMap<String, FrameTree>, BrowserError> {
        let mut elements: HashMap<String, Arc<ax::Elements>> = HashMap::new();
        let mut frames = HashMap::new();
        let mut queue = vec![(self.target.clone(), 0_usize)];
        let mut seen = HashSet::new();
        while let Some((frame, depth)) = queue.pop() {
            if frames.len() >= MAX_FRAMES
                || depth > ax::MAX_FRAME_DEPTH
                || !seen.insert(frame.clone())
            {
                continue;
            }
            let session = self
                .tracker
                .lock()
                .frame_sessions
                .get(&frame)
                .cloned()
                .unwrap_or_else(|| self.session.clone());
            let session_elements = if let Some(found) = elements.get(&session) {
                Arc::clone(found)
            } else {
                let document = self
                    .call(
                        &session,
                        "DOM.getDocument",
                        json!({ "depth": -1, "pierce": true }),
                    )
                    .await?;
                let read = Arc::new(ax::elements_of(&document["root"]));
                elements.insert(session.clone(), Arc::clone(&read));
                read
            };
            let tree = match self
                .call(
                    &session,
                    "Accessibility.getFullAXTree",
                    json!({ "frameId": frame }),
                )
                .await
            {
                Ok(tree) => tree,
                // The main frame must read; a child frame that went away is
                // left out.
                Err(error) if frame == self.target => return Err(error),
                Err(_) => continue,
            };
            let tree = FrameTree {
                session,
                nodes: tree["nodes"].as_array().cloned().unwrap_or_default(),
                elements: session_elements,
            };
            for hosted in tree.hosted_frames() {
                queue.push((hosted, depth + 1));
            }
            frames.insert(frame, tree);
        }
        Ok(frames)
    }

    async fn take_snapshot(&self, max_nodes: usize) -> Result<Snapshot, BrowserError> {
        self.sync();
        for _ in 0..3 {
            let before = self.tracker.marks();
            let input = self.control.state().input_epoch;
            let frames = self.read_frames().await?;
            if self.tracker.marks().documents != before.documents {
                // A navigation landed while the tree was read: read again.
                continue;
            }
            let converted = ax::convert(&self.target, &frames);
            // One element map per CDP session, shared by its frames.
            let mut counted = HashSet::new();
            let dom_controls = frames
                .values()
                .filter(|tree| counted.insert(tree.session.clone()))
                .map(|tree| tree.elements.values().filter(|e| e.is_control()).count())
                .sum();
            let url = self.tracker.lock().url.clone();
            let title = self.title().await;
            let mut refs = lock(&self.refs);
            let id = refs.slot.next_id();
            let snapshot = Snapshot::build(id, &url, &title, converted.roots, max_nodes);
            refs.slot.install(snapshot.clone());
            refs.handles = converted.handles;
            refs.hosts = converted.hosts;
            refs.dom_controls = dom_controls;
            refs.taken_at = Some((before.documents, before.dom_epoch, input));
            return Ok(snapshot);
        }
        Err(BrowserError::new(
            ErrorCode::Timeout,
            "the page kept navigating while it was read",
        ))
    }

    /// Whether the focused element is a secure field (in any frame).
    async fn focused_is_secure(&self) -> Result<bool, BrowserError> {
        let focused = self.main_frame(FOCUSED, Vec::new()).await?;
        if secure_info(&focused) {
            return Ok(true);
        }
        if focused["crossFrame"] != true {
            return Ok(false);
        }
        // The focus is inside an out-of-process frame: ask each of them.
        let frames: Vec<(String, String)> = self
            .tracker
            .lock()
            .frame_sessions
            .iter()
            .map(|(frame, session)| (frame.clone(), session.clone()))
            .collect();
        for (frame, session) in frames {
            match self.in_frame(&session, &frame, FOCUSED, Vec::new()).await {
                Ok(inner) if inner["focused"] == true && secure_info(&inner) => return Ok(true),
                Ok(_) => {}
                // A frame that cannot answer cannot be proved safe.
                Err(_) => return Ok(true),
            }
        }
        Ok(false)
    }

    async fn press_keys(&self, key: &Key) -> Result<(), BrowserError> {
        for event in keys::events(key) {
            self.page("Input.dispatchKeyEvent", event).await?;
        }
        Ok(())
    }

    async fn wait_for(
        &self,
        condition: &WaitCondition,
        timeout: Duration,
    ) -> Result<(), BrowserError> {
        let deadline = tokio::time::Instant::now() + timeout;
        let mut changes = self.tracker.changed();
        loop {
            let done = match condition {
                WaitCondition::Url(pattern) => url_matches(pattern, &self.tracker.lock().url),
                WaitCondition::NetworkIdle => self.tracker.lock().network_idle(Instant::now()),
                WaitCondition::Text(text) => self
                    .main_frame(HAS_TEXT, vec![Value::String(text.clone())])
                    .await
                    .is_ok_and(|found| found == true),
                WaitCondition::Element(target) => {
                    let handle = self.handle(target, false)?;
                    self.element_box(&handle).await?.is_some()
                }
            };
            if done {
                return Ok(());
            }
            if !self.cdp.is_open() {
                return Err(engine_error(&CdpError::Closed));
            }
            let step = (tokio::time::Instant::now() + Duration::from_millis(100)).min(deadline);
            let _ = tokio::time::timeout_at(step, changes.changed()).await;
            if tokio::time::Instant::now() >= deadline {
                return Err(BrowserError::new(
                    ErrorCode::Timeout,
                    "the condition did not hold before the timeout",
                ));
            }
        }
    }
}

fn min(values: &[f64; 4]) -> f64 {
    values.iter().copied().fold(f64::INFINITY, f64::min)
}

fn max(values: &[f64; 4]) -> f64 {
    values.iter().copied().fold(f64::NEG_INFINITY, f64::max)
}

fn egress_refused() -> BrowserError {
    BrowserError::new(
        ErrorCode::EgressRefused,
        "the egress proxy refused this host: it is outside the run's allowlist",
    )
}

fn secure_info(info: &Value) -> bool {
    is_secure_field(info["type"].as_str(), info["autocomplete"].as_str())
}

fn script_value(result: &Value) -> Result<Value, BrowserError> {
    if result.get("exceptionDetails").is_some() {
        return Err(BrowserError::new(
            ErrorCode::Internal,
            "the driver's page script failed",
        ));
    }
    Ok(result["result"]["value"].clone())
}

/// The first page target, waiting for the engine to make it.
async fn first_page(cdp: &Connection) -> Result<String, BrowserError> {
    for _ in 0..100 {
        let targets = cdp
            .call(None, "Target.getTargets", json!({}))
            .await
            .map_err(|error| engine_error(&error))?;
        let page = targets["targetInfos"]
            .as_array()
            .into_iter()
            .flatten()
            .find(|info| info["type"] == "page")
            .and_then(|info| info["targetId"].as_str());
        if let Some(page) = page {
            return Ok(page.to_owned());
        }
        tokio::time::sleep(Duration::from_millis(100)).await;
    }
    let created = cdp
        .call(None, "Target.createTarget", json!({ "url": "about:blank" }))
        .await
        .map_err(|error| engine_error(&error))?;
    created["targetId"]
        .as_str()
        .map(str::to_owned)
        .ok_or_else(|| BrowserError::new(ErrorCode::Internal, "the browser has no page"))
}

/// A viewer's pointer event as `Input.dispatchMouseEvent` parameters.
///
/// `button` follows `BrowserPointerEventV1`: 0 none, 1 left, 2 middle,
/// 3 right. A move with 0 is a hover; a move with a button is a drag, so
/// CDP also gets the held button in `buttons`. A press or a release must
/// name its button: one with 0 is refused, never guessed as left.
fn pointer_params(event: &Value) -> Result<Value, BrowserError> {
    let kind = match event["kind"].as_str() {
        Some("move") => "mouseMoved",
        Some("down") => "mousePressed",
        Some("up") => "mouseReleased",
        _ => return Err(BrowserError::invalid("pointer kind is move, down or up")),
    };
    // The CDP button, and its bit in CDP's `buttons`.
    let (button, held) = match event["button"].as_u64().unwrap_or_default() {
        0 if kind == "mouseMoved" => ("none", 0),
        1 => ("left", 1),
        2 => ("middle", 4),
        3 => ("right", 2),
        _ => {
            return Err(BrowserError::invalid(
                "pointer button is 1 (left), 2 (middle) or 3 (right), or 0 on a move",
            ));
        }
    };
    // After a release no button is held.
    let buttons = if kind == "mouseReleased" { 0 } else { held };
    let click_count = if kind == "mouseMoved" {
        0
    } else {
        event["click_count"].as_u64().unwrap_or(1).clamp(1, 3)
    };
    Ok(json!({
        "type": kind,
        "x": event["x"].as_f64().unwrap_or_default(),
        "y": event["y"].as_f64().unwrap_or_default(),
        "button": button, "buttons": buttons, "clickCount": click_count,
        "modifiers": event["modifiers"].as_u64().unwrap_or_default() & 0x0f,
    }))
}

/// A viewer's key event as `Input.dispatchKeyEvent` parameters.
///
/// Chromium acts on a key down only with its `windowsVirtualKeyCode`
/// (Backspace, Enter, the arrows, Tab), so every event carries the code
/// [`keys::virtual_key_code`] gives. A named key that types text (Enter
/// types `"\r"`) types it on its down unless Ctrl, Alt or Meta is held. The
/// view sends a separate `char` only for a printable character, so nothing
/// is typed twice.
fn key_params(event: &Value) -> Result<Value, BrowserError> {
    // Alt 1, Ctrl 2, Meta 4: a chord types nothing; Shift (8) alone does.
    const CHORD: u64 = 0b0111;
    let text = |name: &str| -> String {
        event[name]
            .as_str()
            .unwrap_or_default()
            .chars()
            .take(32)
            .collect()
    };
    let modifiers = event["modifiers"].as_u64().unwrap_or_default() & 0x0f;
    let key = text("key");
    let mut typed = text("text");
    if typed.is_empty() && event["kind"].as_str() == Some("down") && modifiers & CHORD == 0 {
        keys::named_text(&key).clone_into(&mut typed);
    }
    let kind = match event["kind"].as_str() {
        Some("down") if typed.is_empty() => "rawKeyDown",
        Some("down") => "keyDown",
        Some("up") => "keyUp",
        Some("char") => "char",
        _ => return Err(BrowserError::invalid("key kind is down, up or char")),
    };
    let mut params = json!({
        "type": kind, "key": key, "code": text("code"), "modifiers": modifiers,
        "windowsVirtualKeyCode": keys::virtual_key_code(&key),
    });
    if kind != "keyUp" && !typed.is_empty() {
        params["text"] = Value::String(typed.clone());
        params["unmodifiedText"] = Value::String(typed);
    }
    Ok(params)
}

/// Width and height of a PNG from its IHDR chunk.
fn png_size(data: &[u8]) -> Option<(u32, u32)> {
    if data.len() < 24 || &data[..8] != b"\x89PNG\r\n\x1a\n" || &data[12..16] != b"IHDR" {
        return None;
    }
    let width = u32::from_be_bytes(data[16..20].try_into().ok()?);
    let height = u32::from_be_bytes(data[20..24].try_into().ok()?);
    Some((width, height))
}

/// The engine's answer while a page's document is between processes.
const NOT_ATTACHED: &str = "Not attached to an active page";

/// How long a page command waits out such a swap.
const PAGE_SWAP_GRACE: Duration = Duration::from_secs(5);

#[async_trait]
impl BrowserDriver for CdpDriver {
    async fn open(&self, url: &str) -> Result<PageInfo, BrowserError> {
        let epoch = self.begin()?;
        let result = async {
            let before = self.tracker.marks();
            let navigated = self.page("Page.navigate", json!({ "url": url })).await?;
            if let Some(error) = navigated["errorText"]
                .as_str()
                .filter(|text| !text.is_empty())
            {
                self.invalidate();
                if error.contains(TUNNEL_FAILED) && self.tracker.lock().closed.eq(&false) {
                    return Err(egress_refused());
                }
                return Err(BrowserError::new(
                    ErrorCode::NavigationFailed,
                    format!(
                        "the page did not load ({})",
                        error.chars().take(80).collect::<String>()
                    ),
                ));
            }
            let same_document = navigated["loaderId"].as_str().is_none_or(str::is_empty);
            if !same_document && !self.loaded(before, NAVIGATION_TIMEOUT).await {
                self.invalidate();
                return Err(BrowserError::new(
                    ErrorCode::Timeout,
                    "the page did not finish loading in time",
                ));
            }
            self.info(before).await
        }
        .await;
        self.end(epoch, result)
    }

    async fn snapshot(&self, max_nodes: usize) -> Result<Snapshot, BrowserError> {
        let epoch = self.begin_watch();
        let result = self.take_snapshot(max_nodes).await;
        self.end(epoch, result)
    }

    async fn text(&self, max_chars: usize) -> Result<PageText, BrowserError> {
        let epoch = self.begin()?;
        let result = async {
            let limit = max_chars.saturating_add(1).saturating_mul(2);
            let text = self.main_frame(TEXT, vec![json!(limit)]).await?;
            let url = self.tracker.lock().url.clone();
            Ok(PageText {
                url,
                title: self.title().await,
                text: text.as_str().unwrap_or_default().to_owned(),
            })
        }
        .await;
        self.end(epoch, result)
    }

    async fn screenshot(&self, target: Option<&ElementRef>) -> Result<Screenshot, BrowserError> {
        let epoch = self.begin_watch();
        let result = async {
            let mut params = json!({ "format": "png", "captureBeyondViewport": false });
            if let Some(target) = target {
                let handle = self.handle(target, false)?;
                let (left, top, width, height) = self
                    .element_box(&handle)
                    .await?
                    .ok_or_else(|| BrowserError::invalid(format!("{target} is not visible")))?;
                let metrics = self.page("Page.getLayoutMetrics", json!({})).await?;
                let viewport = &metrics["cssVisualViewport"];
                params["clip"] = json!({
                    "x": left + viewport["pageX"].as_f64().unwrap_or_default(),
                    "y": top + viewport["pageY"].as_f64().unwrap_or_default(),
                    "width": width, "height": height, "scale": 1,
                });
            }
            let shot = self.page("Page.captureScreenshot", params).await?;
            let data = base64::engine::general_purpose::STANDARD
                .decode(shot["data"].as_str().unwrap_or_default())
                .map_err(|_| {
                    BrowserError::new(ErrorCode::Internal, "the screenshot did not decode")
                })?;
            let (width, height) = png_size(&data).unwrap_or((self.width, self.height));
            Ok(Screenshot {
                media_type: "image/png",
                data,
                width,
                height,
            })
        }
        .await;
        self.end(epoch, result)
    }

    async fn click(&self, target: &ElementRef, double: bool) -> Result<PageInfo, BrowserError> {
        let epoch = self.begin_act()?;
        let result = async {
            let handle = self.handle(target, false)?;
            let before = self.tracker.marks();
            if let Some((left, top, width, height)) = self.element_box(&handle).await? {
                let (x, y) = (left + width / 2.0, top + height / 2.0);
                self.mouse("mouseMoved", x, y, 0).await?;
                for clicks in if double { 1..=2 } else { 1..=1 } {
                    self.mouse("mousePressed", x, y, clicks).await?;
                    self.mouse("mouseReleased", x, y, clicks).await?;
                }
            } else {
                // No box (an option of a closed select, a visually hidden
                // control): the element's own activation.
                let connected = self.on_element(&handle, INSPECT, Vec::new()).await?;
                if connected["connected"] != true {
                    return Err(BrowserError::stale_ref(
                        "the element is gone: snapshot again",
                    ));
                }
                self.on_element(&handle, CLICK, Vec::new()).await?;
                if double {
                    self.on_element(&handle, CLICK, Vec::new()).await?;
                }
            }
            self.settle(before).await;
            self.info(before).await
        }
        .await;
        self.end(epoch, result)
    }

    async fn type_text(
        &self,
        target: &ElementRef,
        text: &str,
        clear: bool,
        submit: bool,
    ) -> Result<PageInfo, BrowserError> {
        let epoch = self.begin_act()?;
        let result = async {
            let handle = self.handle(target, true)?;
            let inspected = self.on_element(&handle, INSPECT, Vec::new()).await?;
            if inspected["connected"] != true {
                return Err(BrowserError::stale_ref(
                    "the element is gone: snapshot again",
                ));
            }
            // The live element, not only the snapshot: a page can change a
            // field's type after the snapshot was read.
            if secure_info(&inspected) {
                return Err(BrowserError::secure_field());
            }
            if inspected["editable"] != true {
                return Err(BrowserError::invalid(format!(
                    "{target} is not a text field"
                )));
            }
            if inspected["disabled"] == true {
                return Err(BrowserError::invalid(format!(
                    "{target} is disabled or read-only"
                )));
            }
            let before = self.tracker.marks();
            let focused = self
                .on_element(&handle, PREPARE, vec![json!(clear)])
                .await?;
            if focused != true {
                return Err(BrowserError::invalid(format!(
                    "{target} did not take the focus"
                )));
            }
            if !text.is_empty() {
                self.page("Input.insertText", json!({ "text": text }))
                    .await?;
            } else if clear {
                self.press_keys(&Key::parse("Backspace")?).await?;
            }
            if submit {
                self.press_keys(&Key::parse("Enter")?).await?;
            }
            self.settle(before).await;
            self.info(before).await
        }
        .await;
        self.end(epoch, result)
    }

    async fn select(
        &self,
        target: &ElementRef,
        values: &[String],
    ) -> Result<PageInfo, BrowserError> {
        let epoch = self.begin_act()?;
        let result = async {
            let handle = self.handle(target, false)?;
            let before = self.tracker.marks();
            let outcome = self
                .on_element(&handle, SELECT, vec![json!(values)])
                .await?;
            match outcome["error"].as_str() {
                None => {}
                Some("not_select") => {
                    return Err(BrowserError::invalid(format!(
                        "{target} is not a native select: click its options instead"
                    )));
                }
                Some("disabled") => {
                    return Err(BrowserError::invalid(format!("{target} is disabled")));
                }
                Some("single") => {
                    return Err(BrowserError::invalid(format!(
                        "{target} takes one option only"
                    )));
                }
                Some(_) => {
                    let missing: Vec<&str> = outcome["missing"]
                        .as_array()
                        .into_iter()
                        .flatten()
                        .filter_map(Value::as_str)
                        .collect();
                    return Err(BrowserError::invalid(format!(
                        "{target} has no option {missing:?}"
                    )));
                }
            }
            self.settle(before).await;
            self.info(before).await
        }
        .await;
        self.end(epoch, result)
    }

    async fn scroll(&self, target: &ScrollTarget) -> Result<PageInfo, BrowserError> {
        let epoch = self.begin_act()?;
        let result = async {
            let before = self.tracker.marks();
            match target {
                ScrollTarget::Element(target) => {
                    let handle = self.handle(target, false)?;
                    self.call(
                        &handle.session,
                        "DOM.scrollIntoViewIfNeeded",
                        json!({ "backendNodeId": handle.backend_id }),
                    )
                    .await
                    .map_err(|_| BrowserError::stale_ref("the element is gone: snapshot again"))?;
                }
                ScrollTarget::Page { direction, pixels } => {
                    let (width, height) = (f64::from(self.width), f64::from(self.height));
                    let page = |size: f64| pixels.map_or(size * 0.9, f64::from);
                    let (dx, dy) = match direction {
                        Direction::Up => (0.0, -page(height)),
                        Direction::Down => (0.0, page(height)),
                        Direction::Left => (-page(width), 0.0),
                        Direction::Right => (page(width), 0.0),
                    };
                    self.page(
                        "Input.dispatchMouseEvent",
                        json!({
                            "type": "mouseWheel", "x": width / 2.0, "y": height / 2.0,
                            "deltaX": dx, "deltaY": dy,
                        }),
                    )
                    .await?;
                }
            }
            self.settle(before).await;
            self.info(before).await
        }
        .await;
        self.end(epoch, result)
    }

    async fn press(&self, key: &Key) -> Result<PageInfo, BrowserError> {
        let epoch = self.begin_act()?;
        let result = async {
            if self.focused_is_secure().await? {
                return Err(BrowserError::secure_field());
            }
            let before = self.tracker.marks();
            self.press_keys(key).await?;
            self.settle(before).await;
            self.info(before).await
        }
        .await;
        self.end(epoch, result)
    }

    async fn history(&self, step: HistoryStep) -> Result<PageInfo, BrowserError> {
        let epoch = self.begin()?;
        let result = async {
            let before = self.tracker.marks();
            let before_url = self.tracker.lock().url.clone();
            if step == HistoryStep::Reload {
                self.page("Page.reload", json!({})).await?;
                if !self.loaded(before, NAVIGATION_TIMEOUT).await {
                    self.invalidate();
                    return Err(BrowserError::new(
                        ErrorCode::Timeout,
                        "the page did not reload in time",
                    ));
                }
                return self.info(before).await;
            }
            let history = self.page("Page.getNavigationHistory", json!({})).await?;
            let current = history["currentIndex"].as_i64().unwrap_or_default();
            let wanted = if step == HistoryStep::Back {
                current - 1
            } else {
                current + 1
            };
            let entry = usize::try_from(wanted)
                .ok()
                .and_then(|index| history["entries"].as_array()?.get(index).cloned())
                .ok_or_else(|| {
                    BrowserError::new(
                        ErrorCode::NavigationFailed,
                        if step == HistoryStep::Back {
                            "there is no previous page"
                        } else {
                            "there is no next page"
                        },
                    )
                })?;
            self.page(
                "Page.navigateToHistoryEntry",
                json!({ "entryId": entry["id"] }),
            )
            .await?;
            let moved = self
                .tracker
                .wait_until(NAVIGATION_TIMEOUT, |state| {
                    (state.documents > before.documents
                        && state.loaded_document == state.documents
                        && !state.loading)
                        || (state.url != before_url && !state.loading)
                        || state.scheme_refusals > before.scheme_refusals
                        || state.egress_refusals > before.egress_refusals
                })
                .await;
            if !moved {
                self.invalidate();
                return Err(BrowserError::new(
                    ErrorCode::Timeout,
                    "the page did not load in time",
                ));
            }
            self.info(before).await
        }
        .await;
        self.end(epoch, result)
    }

    async fn wait(
        &self,
        condition: &WaitCondition,
        timeout: Duration,
    ) -> Result<PageInfo, BrowserError> {
        let epoch = self.begin()?;
        let result = async {
            let before = self.tracker.marks();
            self.wait_for(condition, timeout).await?;
            self.info(before).await
        }
        .await;
        self.end(epoch, result)
    }

    async fn request_user(&self, reason: &str) -> Result<Handback, BrowserError> {
        self.begin()?;
        if !self.control.attended {
            return Err(BrowserError::new(
                ErrorCode::NoPerson,
                "nobody is watching this run: there is no person to ask",
            ));
        }
        let answer = self.control.ask(reason).await;
        // The person had the page: whatever the agent knew of it is stale.
        self.invalidate();
        match answer {
            Answer::HandedBack(note) => Ok(Handback { note }),
            Answer::Declined => Err(BrowserError::new(
                ErrorCode::Declined,
                "the person declined or did not answer",
            )),
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn png_size_reads_the_header() {
        let mut png = b"\x89PNG\r\n\x1a\n\0\0\0\rIHDR".to_vec();
        png.extend_from_slice(&1280_u32.to_be_bytes());
        png.extend_from_slice(&800_u32.to_be_bytes());
        assert_eq!(png_size(&png), Some((1280, 800)));
        assert_eq!(png_size(b"not a png at all, not at all"), None);
    }

    #[test]
    fn secure_info_follows_the_contract_rule() {
        assert!(secure_info(&json!({"type": "password"})));
        assert!(secure_info(
            &json!({"type": "text", "autocomplete": "cc-number"})
        ));
        assert!(!secure_info(
            &json!({"type": "email", "autocomplete": "email"})
        ));
    }

    #[test]
    fn pointer_buttons_follow_the_proto() {
        let params = |kind: &str, button: u64| {
            pointer_params(
                &json!({"kind": kind, "x": 1, "y": 2, "button": button, "click_count": 1}),
            )
        };
        let pressed = |value: &Value| (value["button"].clone(), value["buttons"].clone());
        assert_eq!(
            pressed(&params("down", 1).unwrap()),
            (json!("left"), json!(1))
        );
        assert_eq!(
            pressed(&params("down", 2).unwrap()),
            (json!("middle"), json!(4))
        );
        assert_eq!(
            pressed(&params("up", 3).unwrap()),
            (json!("right"), json!(0))
        );
        // A hover holds nothing; a drag holds its button.
        assert_eq!(
            pressed(&params("move", 0).unwrap()),
            (json!("none"), json!(0))
        );
        assert_eq!(
            pressed(&params("move", 1).unwrap()),
            (json!("left"), json!(1))
        );
        assert_eq!(
            pressed(&params("move", 3).unwrap()),
            (json!("right"), json!(2))
        );
        assert_eq!(params("move", 0).unwrap()["clickCount"], 0);
        // A press or release without a button is refused, not taken as left.
        assert_eq!(
            params("down", 0).unwrap_err().code(),
            ErrorCode::InvalidArgument
        );
        assert_eq!(
            params("up", 4).unwrap_err().code(),
            ErrorCode::InvalidArgument
        );
    }

    #[test]
    fn view_keys_carry_a_virtual_key_code_and_enter_types_a_return() {
        let key = |kind: &str, key: &str, code: &str, text: &str, modifiers: u64| {
            key_params(&json!({
                "kind": kind, "key": key, "code": code, "text": text, "modifiers": modifiers,
            }))
            .unwrap()
        };
        let shape = |value: &Value| {
            (
                value["type"].clone(),
                value["windowsVirtualKeyCode"].clone(),
            )
        };
        let enter = key("down", "Enter", "Enter", "", 0);
        assert_eq!(shape(&enter), (json!("keyDown"), json!(13)));
        assert_eq!(enter["text"], "\r");
        // Ctrl+Enter types nothing.
        let chord = key("down", "Enter", "Enter", "", 2);
        assert_eq!(shape(&chord), (json!("rawKeyDown"), json!(13)));
        assert!(chord.get("text").is_none());
        for (name, vk) in [
            ("Backspace", 8),
            ("Tab", 9),
            ("ArrowLeft", 37),
            ("Delete", 46),
        ] {
            let down = key("down", name, name, "", 0);
            assert_eq!(shape(&down), (json!("rawKeyDown"), json!(vk)), "{name}");
            assert!(down.get("text").is_none(), "{name}");
        }
        // A printable: a raw down with its code, then its char types it once.
        let a = key("down", "a", "KeyA", "", 0);
        assert_eq!(shape(&a), (json!("rawKeyDown"), json!(65)));
        assert!(a.get("text").is_none());
        let typed = key("char", "a", "KeyA", "a", 0);
        assert_eq!(
            (typed["type"].clone(), typed["text"].clone()),
            (json!("char"), json!("a"))
        );
        let up = key("up", "Enter", "Enter", "", 0);
        assert_eq!(shape(&up), (json!("keyUp"), json!(13)));
        assert!(up.get("text").is_none());
        assert_eq!(
            key("down", "Shift", "ShiftLeft", "", 8)["windowsVirtualKeyCode"],
            16
        );
        assert_eq!(
            key("down", "7", "Digit7", "", 0)["windowsVirtualKeyCode"],
            55
        );
    }

    #[test]
    fn a_script_exception_is_internal() {
        let error = script_value(&json!({"exceptionDetails": {"text": "x"}})).unwrap_err();
        assert_eq!(error.code(), ErrorCode::Internal);
        assert_eq!(
            script_value(&json!({"result": {"value": 3}})).unwrap(),
            json!(3)
        );
    }
}

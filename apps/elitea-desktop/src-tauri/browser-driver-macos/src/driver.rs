//! [`WebDriver`]: the contract's [`BrowserDriver`] over an [`Engine`].
//!
//! The engine is the WebKit part ([`crate::webkit::WebKitEngine`] on macOS):
//! it runs the page script in the driver's private content world, in one
//! frame, and does what only the native side can (load, history, the page
//! state, the screenshot, trusted input). Everything the contract asks a
//! driver to enforce is here, in plain Rust, and tested with a fake engine:
//!
//! * **Refs.** One [`SnapshotSlot`] per pane, a snapshot built with
//!   [`Snapshot::build`]; a node's key is its frame's place in the
//!   snapshot's frame table and the element's number in that frame's
//!   document. Every act names the document it expects (`doc`): an element
//!   number of an earlier document is `stale_ref`, and so is any ref once
//!   the top document changed.
//! * **Secure fields.** Refused twice: by the slot
//!   ([`SnapshotSlot::resolve_for_input`]) and by the page script, which
//!   reads the element itself (a field turned into a password field after
//!   the snapshot is still refused). `press` asks the script for the
//!   focused element.
//! * **Navigations.** `open` passes the host's [`NavigationCheck`] (the
//!   pane's navigation guard and domain policy) after the contract's scheme
//!   check; every page the page itself reaches is checked again with
//!   [`check_url`] when the act settles. The pane's own `on_navigation`
//!   guard refuses the load before that (`src/browser_pane.rs`).
//! * **Rate.** [`ActRate`]: ten acts a second.
//! * **Trusted input.** A click the page cancels without any effect is
//!   retried as real input ([`Engine::trusted_click`]) only when the
//!   person gave Elitea the Accessibility grant (ADR-0047); without it the
//!   model is told to hand the page to the person (`browser_request_user`).
//!
//! The control pill (`user_active`, `user_in_control`) and the hand-over
//! question are #1591's ([`HandOver`]).

use std::sync::{Arc, Mutex, MutexGuard};
use std::time::{Duration, Instant};

use async_trait::async_trait;
use elitea_browser_tools::call::{
    Direction, ElementRef, Key, Modifier, ScrollTarget, WaitCondition, check_url, url_matches,
};
use elitea_browser_tools::driver::{
    BrowserDriver, Handback, HistoryStep, PageInfo, PageText, Screenshot,
};
use elitea_browser_tools::rate::ActRate;
use elitea_browser_tools::snapshot::{Node, Snapshot, SnapshotSlot};
use elitea_browser_tools::{BrowserError, ErrorCode};
use serde_json::{Value, json};

use crate::reply::{self, RawClick, RawNode, RawPage, RawRect, RawState};

/// A frame the engine can run the script in: 0 is the top document.
pub type FrameId = u32;

/// The top document.
pub const TOP: FrameId = 0;

/// The page as the native side sees it.
#[derive(Clone, Debug, Default, Eq, PartialEq)]
pub struct EnginePage {
    pub url: String,
    pub title: String,
    pub loading: bool,
}

/// The native half of the driver.
#[async_trait]
pub trait Engine: Send + Sync {
    /// Run the page script's `op` with `args` in `frame`, in the driver's
    /// content world; the script's answer ([`reply::parse`]).
    async fn call(&self, frame: FrameId, op: &str, args: Value) -> Result<Value, BrowserError>;

    /// The frames that announced themselves to the driver world (cross-
    /// origin frames are reached only so), as `(frame, url)`, oldest first.
    async fn frames(&self) -> Vec<(FrameId, String)>;

    /// Start loading `url` in the top document.
    async fn load(&self, url: &str) -> Result<(), BrowserError>;

    /// Start a history step; `invalid_argument` when there is no page to go
    /// back or forward to.
    async fn history(&self, step: HistoryStep) -> Result<(), BrowserError>;

    async fn page(&self) -> Result<EnginePage, BrowserError>;

    /// The visible page, or `rect` of it (top viewport, CSS pixels).
    async fn screenshot(&self, rect: Option<RawRect>) -> Result<Screenshot, BrowserError>;

    /// Whether real input may be posted: the Accessibility grant
    /// (ADR-0047) is there.
    fn trusted_input(&self) -> bool;

    /// A real click at `at` (top viewport, CSS pixels), aimed at the page
    /// view only.
    async fn trusted_click(&self, at: (f64, f64), double: bool) -> Result<(), BrowserError>;
}

/// The host's verdict on a navigation the agent asks for (the pane's guard
/// and its domain policy): `scheme_refused` or `egress_refused`.
pub type NavigationCheck = Arc<dyn Fn(&str) -> Result<(), BrowserError> + Send + Sync>;

/// `browser_request_user` (#1591: the control pill and the hand-over).
#[async_trait]
pub trait HandOver: Send + Sync {
    async fn request_user(&self, reason: &str) -> Result<Handback, BrowserError>;
}

/// Until #1591: there is no hand-over yet, so the model is told to ask in
/// the conversation.
pub struct NoHandOver;

#[async_trait]
impl HandOver for NoHandOver {
    async fn request_user(&self, _reason: &str) -> Result<Handback, BrowserError> {
        Err(BrowserError::new(
            ErrorCode::Unsupported,
            "handing the page to the person is not available yet: ask them in the conversation",
        ))
    }
}

/// How long a navigation may take before `timeout`.
pub const LOAD_TIMEOUT: Duration = Duration::from_secs(30);
/// How long an act's effects (a navigation it started) are waited for.
const SETTLE_TIMEOUT: Duration = Duration::from_secs(15);
const POLL: Duration = Duration::from_millis(50);
/// Raw nodes the walk reads per kept node, at most [`MAX_RAW`].
const RAW_PER_NODE: usize = 8;
const MAX_RAW: usize = 20_000;
/// Cross-origin frames inside cross-origin frames, at most.
const MAX_FRAME_DEPTH: usize = 3;
/// A network-idle wait: no new resource for this long.
const IDLE_QUIET: Duration = Duration::from_millis(500);

/// What a ref's key points at.
#[derive(Clone, Debug, PartialEq, Eq)]
struct FrameRef {
    frame: FrameId,
    doc: String,
    /// The key of the frame element in its parent (`None` for the top).
    parent: Option<u64>,
}

#[derive(Default)]
struct State {
    slot: SnapshotSlot,
    /// The installed snapshot's frames; a key's high half indexes it.
    frames: Vec<FrameRef>,
    rate: ActRate,
}

fn key_of(frame_slot: usize, element: u64) -> u64 {
    ((frame_slot as u64) << 32) | (element & 0xffff_ffff)
}

fn split_key(key: u64) -> (usize, u64) {
    (
        usize::try_from(key >> 32).unwrap_or(usize::MAX),
        key & 0xffff_ffff,
    )
}

/// The macOS (WebKit) browser driver of one pane.
pub struct WebDriver<E: Engine> {
    engine: E,
    check: NavigationCheck,
    hand_over: Arc<dyn HandOver>,
    state: Mutex<State>,
}

impl<E: Engine> WebDriver<E> {
    #[must_use]
    pub fn new(engine: E, check: NavigationCheck, hand_over: Arc<dyn HandOver>) -> Self {
        Self {
            engine,
            check,
            hand_over,
            state: Mutex::new(State::default()),
        }
    }

    #[must_use]
    pub const fn engine(&self) -> &E {
        &self.engine
    }

    fn lock(&self) -> MutexGuard<'_, State> {
        self.state
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
    }

    fn invalidate(&self) {
        let mut state = self.lock();
        state.slot.invalidate();
        state.frames.clear();
    }

    fn admit_act(&self) -> Result<(), BrowserError> {
        self.lock().rate.admit(Instant::now())
    }

    async fn top_state(&self) -> Result<RawState, BrowserError> {
        reply::typed(self.engine.call(TOP, "state", json!({})).await?)
    }

    /// The element `target` names: its frame, document and number. Any ref
    /// is stale once the top document is not the snapshot's.
    async fn element(
        &self,
        target: &ElementRef,
        input: bool,
    ) -> Result<(FrameRef, u64), BrowserError> {
        let (frame, element, top_doc) = {
            let state = self.lock();
            let node = if input {
                state.slot.resolve_for_input(target)?
            } else {
                state.slot.resolve(target)?
            };
            let (slot, element) = split_key(node.key);
            let frame = state
                .frames
                .get(slot)
                .cloned()
                .ok_or_else(|| BrowserError::stale_ref("snapshot again"))?;
            let top_doc = state.frames.first().map(|top| top.doc.clone());
            (frame, element, top_doc)
        };
        if element == 0 {
            return Err(BrowserError::invalid("that node is text, not an element"));
        }
        if top_doc.as_deref() != Some(self.top_state().await?.doc.as_str()) {
            self.invalidate();
            return Err(BrowserError::stale_ref(
                "the page changed since the snapshot: snapshot again",
            ));
        }
        Ok((frame, element))
    }

    async fn on_element(
        &self,
        frame: &FrameRef,
        op: &str,
        element: u64,
        mut args: Value,
    ) -> Result<Value, BrowserError> {
        args["id"] = json!(element);
        args["doc"] = json!(frame.doc);
        self.engine.call(frame.frame, op, args).await
    }

    /// Where the page is, once whatever the last call started has loaded.
    /// `refs_stale` when the top document is not the snapshot's any more.
    async fn settle(&self, deadline: Duration) -> Result<PageInfo, BrowserError> {
        tokio::time::sleep(POLL).await;
        let started = Instant::now();
        let mut page = self.engine.page().await?;
        while page.loading && started.elapsed() < deadline {
            tokio::time::sleep(POLL).await;
            page = self.engine.page().await?;
        }
        if page.loading {
            return Err(BrowserError::new(
                ErrorCode::Timeout,
                "the page is still loading",
            ));
        }
        self.page_info(page).await
    }

    async fn page_info(&self, page: EnginePage) -> Result<PageInfo, BrowserError> {
        // Every page the agent ends up on passes the scheme limit, however
        // it got there (the pane's guard refuses the load before this).
        check_url(&page.url)?;
        let doc = self.top_state().await.ok().map(|state| state.doc);
        let known = self.lock().frames.first().map(|top| top.doc.clone());
        let refs_stale = known.is_some() && doc != known;
        if refs_stale {
            self.invalidate();
        }
        Ok(PageInfo {
            url: page.url,
            title: page.title,
            refs_stale,
        })
    }

    /// Wait for a navigation the driver started: until the top document is
    /// a new one and loaded, or (a page restored from the back-forward
    /// cache keeps its document) loading stopped for a moment.
    async fn navigated(&self, before: Option<String>) -> Result<PageInfo, BrowserError> {
        let started = Instant::now();
        let mut stopped_since: Option<Instant> = None;
        loop {
            tokio::time::sleep(POLL).await;
            let page = self.engine.page().await?;
            let state = self.top_state().await.ok();
            // A new document, at the address WebKit shows, with its
            // frames and resources loaded (`load` fired).
            let ready = state.as_ref().is_some_and(|state| {
                Some(&state.doc) != before.as_ref()
                    && state.url == page.url
                    && state.ready == "complete"
            });
            if !page.loading && ready {
                break;
            }
            if page.loading {
                stopped_since = None;
            } else if stopped_since.get_or_insert_with(Instant::now).elapsed()
                > Duration::from_millis(750)
            {
                if state.is_some_and(|state| Some(state.doc) != before) {
                    break;
                }
                return Err(BrowserError::new(
                    ErrorCode::NavigationFailed,
                    "the page did not load (refused, unreachable or not a web page)",
                ));
            }
            if started.elapsed() > LOAD_TIMEOUT {
                return Err(BrowserError::new(
                    ErrorCode::Timeout,
                    "the page did not finish loading",
                ));
            }
        }
        self.invalidate();
        let page = self.engine.page().await?;
        let mut info = self.page_info(page).await?;
        info.refs_stale = true;
        Ok(info)
    }

    /// The top document's walk with every cross-origin frame read in its
    /// own frame and put in place.
    async fn walk(&self, max_raw: usize) -> Result<(RawPage, Vec<FrameRef>), BrowserError> {
        let mut page: RawPage = reply::typed(
            self.engine
                .call(TOP, "snapshot", json!({ "max_raw": max_raw }))
                .await?,
        )?;
        let mut frames = vec![FrameRef {
            frame: TOP,
            doc: page.doc.clone(),
            parent: None,
        }];
        let announced = self.engine.frames().await;
        let mut used = Vec::new();
        let mut nodes = std::mem::take(&mut page.nodes);
        self.inline_frames(&mut nodes, 0, 0, &announced, &mut used, &mut frames)
            .await;
        page.nodes = nodes;
        Ok((page, frames))
    }

    /// Key every element of `nodes` (read in `frames[slot]`) with its frame
    /// slot, reading each cross-origin frame element's content in place.
    fn inline_frames<'a>(
        &'a self,
        nodes: &'a mut [RawNode],
        slot: usize,
        depth: usize,
        announced: &'a [(FrameId, String)],
        used: &'a mut Vec<FrameId>,
        frames: &'a mut Vec<FrameRef>,
    ) -> std::pin::Pin<Box<dyn std::future::Future<Output = ()> + Send + 'a>> {
        Box::pin(async move {
            for node in nodes.iter_mut() {
                let element = node.k;
                if element != 0 {
                    node.k = key_of(slot, element);
                }
                // A same-origin frame's content was read with its parent:
                // its elements are numbered in the parent's script.
                let children_slot = slot;
                if let Some(src) = node.x.take()
                    && depth < MAX_FRAME_DEPTH
                    && let Some(frame) = pick_frame(&src, announced, used)
                {
                    used.push(frame);
                    let read = self
                        .engine
                        .call(frame, "snapshot", json!({ "max_raw": MAX_RAW / 4 }))
                        .await
                        .and_then(reply::typed::<RawPage>);
                    if let Ok(mut inner) = read {
                        frames.push(FrameRef {
                            frame,
                            doc: inner.doc.clone(),
                            parent: Some(node.k),
                        });
                        let inner_slot = frames.len() - 1;
                        self.inline_frames(
                            &mut inner.nodes,
                            inner_slot,
                            depth + 1,
                            announced,
                            used,
                            frames,
                        )
                        .await;
                        node.c = inner.nodes;
                        continue;
                    }
                }
                self.inline_frames(&mut node.c, children_slot, depth, announced, used, frames)
                    .await;
            }
        })
    }

    /// `element`'s rectangle in the top viewport: its frame's own, moved by
    /// every cross-origin frame element above it.
    async fn rect_in_top(&self, frame: &FrameRef, element: u64) -> Result<RawRect, BrowserError> {
        let mut rect: RawRect =
            reply::typed(self.on_element(frame, "rect", element, json!({})).await?)?;
        let mut parent = frame.parent;
        while let Some(key) = parent {
            let (slot, element) = split_key(key);
            let Some(outer) = self.lock().frames.get(slot).cloned() else {
                break;
            };
            let at: RawRect =
                reply::typed(self.on_element(&outer, "rect", element, json!({})).await?)?;
            rect.x += at.x;
            rect.y += at.y;
            parent = outer.parent;
        }
        Ok(rect)
    }

    async fn wait_once(&self, condition: &WaitCondition) -> Result<bool, BrowserError> {
        Ok(match condition {
            WaitCondition::Element(target) => match self.element(target, false).await {
                Ok((frame, element)) => self
                    .on_element(&frame, "alive", element, json!({}))
                    .await?
                    .as_bool()
                    .unwrap_or(false),
                Err(error) if error.code() == ErrorCode::StaleRef => false,
                Err(error) => return Err(error),
            },
            WaitCondition::Text(text) => self
                .engine
                .call(TOP, "has_text", json!({ "text": text }))
                .await?
                .as_bool()
                .unwrap_or(false),
            WaitCondition::Url(pattern) => url_matches(pattern, &self.engine.page().await?.url),
            WaitCondition::NetworkIdle => false,
        })
    }

    async fn network_idle(&self, timeout: Duration) -> Result<PageInfo, BrowserError> {
        let started = Instant::now();
        let mut last = None;
        let mut quiet_since = Instant::now();
        while started.elapsed() < timeout {
            let page = self.engine.page().await?;
            let count = self.engine.call(TOP, "resources", json!({})).await.ok();
            if page.loading || count != last {
                last = count;
                quiet_since = Instant::now();
            } else if quiet_since.elapsed() >= IDLE_QUIET {
                return self.page_info(page).await;
            }
            tokio::time::sleep(POLL).await;
        }
        Err(BrowserError::new(
            ErrorCode::Timeout,
            "the network did not go idle",
        ))
    }
}

/// The announced frame a cross-origin frame element is: the first unused
/// one at exactly its address, else at its origin. Frames announce in
/// document order, and the engine forgets them at every navigation, so the
/// n-th frame element of an origin is its n-th announced frame.
fn pick_frame(src: &str, announced: &[(FrameId, String)], used: &[FrameId]) -> Option<FrameId> {
    let free = || announced.iter().filter(|(frame, _)| !used.contains(frame));
    if let Some((frame, _)) = free().find(|(_, url)| url == src) {
        return Some(*frame);
    }
    let origin = origin_of(src)?;
    free()
        .find(|(_, url)| origin_of(url).as_deref() == Some(origin.as_str()))
        .map(|(frame, _)| *frame)
}

/// `scheme://host[:port]` of an absolute URL.
fn origin_of(url: &str) -> Option<String> {
    let (scheme, rest) = url.split_once("://")?;
    let authority = rest.split(['/', '?', '#']).next()?;
    (!authority.is_empty()).then(|| format!("{}://{authority}", scheme.to_ascii_lowercase()))
}

fn modifier_name(modifier: Modifier) -> &'static str {
    match modifier {
        Modifier::Control => "Control",
        Modifier::Alt => "Alt",
        Modifier::Shift => "Shift",
        Modifier::Meta => "Meta",
    }
}

fn needs_the_person() -> BrowserError {
    BrowserError::new(
        ErrorCode::Unsupported,
        "the page ignores scripted input: ask the person to click it with browser_request_user",
    )
}

#[async_trait]
impl<E: Engine> BrowserDriver for WebDriver<E> {
    async fn open(&self, url: &str) -> Result<PageInfo, BrowserError> {
        check_url(url)?;
        (self.check)(url)?;
        let before = self.top_state().await.ok().map(|state| state.doc);
        self.invalidate();
        self.engine.load(url).await?;
        self.navigated(before).await
    }

    async fn snapshot(&self, max_nodes: usize) -> Result<Snapshot, BrowserError> {
        let max_raw = max_nodes.saturating_mul(RAW_PER_NODE).clamp(1, MAX_RAW);
        let (page, frames) = self.walk(max_raw).await?;
        let mut keys = |key: u64| key;
        let nodes: Vec<Node> = page
            .nodes
            .into_iter()
            .map(|raw| reply::to_node(raw, &mut keys))
            .collect();
        let mut state = self.lock();
        let id = state.slot.next_id();
        let snapshot = Snapshot::build(id, &page.url, &page.title, nodes, max_nodes);
        state.slot.install(snapshot.clone());
        state.frames = frames;
        Ok(snapshot)
    }

    async fn text(&self, max_chars: usize) -> Result<PageText, BrowserError> {
        let value = self
            .engine
            .call(TOP, "text", json!({ "max_chars": max_chars }))
            .await?;
        let text: TextAnswer = reply::typed(value)?;
        Ok(PageText {
            url: text.url,
            title: text.title,
            text: text.text,
        })
    }

    async fn screenshot(&self, target: Option<&ElementRef>) -> Result<Screenshot, BrowserError> {
        let rect = match target {
            Some(target) => {
                let (frame, element) = self.element(target, false).await?;
                self.on_element(&frame, "scroll_to", element, json!({}))
                    .await?;
                Some(self.rect_in_top(&frame, element).await?)
            }
            None => None,
        };
        self.engine.screenshot(rect).await
    }

    async fn click(&self, target: &ElementRef, double: bool) -> Result<PageInfo, BrowserError> {
        self.admit_act()?;
        let (frame, element) = self.element(target, false).await?;
        let answer: RawClick = reply::typed(
            self.on_element(&frame, "click", element, json!({ "double": double }))
                .await?,
        )?;
        if answer.ignored {
            if !self.engine.trusted_input() {
                return Err(needs_the_person());
            }
            let rect = self.rect_in_top(&frame, element).await?;
            let at = (rect.x + rect.width / 2.0, rect.y + rect.height / 2.0);
            self.engine.trusted_click(at, double).await?;
        }
        self.settle(SETTLE_TIMEOUT).await
    }

    async fn type_text(
        &self,
        target: &ElementRef,
        text: &str,
        clear: bool,
        submit: bool,
    ) -> Result<PageInfo, BrowserError> {
        self.admit_act()?;
        let (frame, element) = self.element(target, true).await?;
        self.on_element(
            &frame,
            "type",
            element,
            json!({ "text": text, "clear": clear, "submit": submit }),
        )
        .await?;
        self.settle(SETTLE_TIMEOUT).await
    }

    async fn select(
        &self,
        target: &ElementRef,
        values: &[String],
    ) -> Result<PageInfo, BrowserError> {
        self.admit_act()?;
        let (frame, element) = self.element(target, false).await?;
        self.on_element(&frame, "select", element, json!({ "values": values }))
            .await?;
        self.settle(SETTLE_TIMEOUT).await
    }

    async fn scroll(&self, target: &ScrollTarget) -> Result<PageInfo, BrowserError> {
        self.admit_act()?;
        match target {
            ScrollTarget::Element(target) => {
                let (frame, element) = self.element(target, false).await?;
                self.on_element(&frame, "scroll_to", element, json!({}))
                    .await?;
            }
            ScrollTarget::Page { direction, pixels } => {
                let direction = match direction {
                    Direction::Up => "up",
                    Direction::Down => "down",
                    Direction::Left => "left",
                    Direction::Right => "right",
                };
                self.engine
                    .call(
                        TOP,
                        "scroll_page",
                        json!({ "direction": direction, "pixels": pixels }),
                    )
                    .await?;
            }
        }
        self.settle(SETTLE_TIMEOUT).await
    }

    async fn press(&self, key: &Key) -> Result<PageInfo, BrowserError> {
        self.admit_act()?;
        let modifiers: Vec<&str> = key.modifiers.iter().copied().map(modifier_name).collect();
        let args = json!({ "key": key.key, "modifiers": modifiers });
        let answer = self.engine.call(TOP, "press", args.clone()).await?;
        if answer.get("in_frame") == Some(&Value::Bool(true)) {
            // The focus is in a cross-origin frame: press there.
            for (frame, _) in self.engine.frames().await {
                let focused = self.engine.call(frame, "focused", json!({})).await;
                if focused.ok().and_then(|value| value.as_bool()) == Some(true) {
                    self.engine.call(frame, "press", args).await?;
                    break;
                }
            }
        }
        self.settle(SETTLE_TIMEOUT).await
    }

    async fn history(&self, step: HistoryStep) -> Result<PageInfo, BrowserError> {
        let before = self.top_state().await.ok().map(|state| state.doc);
        self.invalidate();
        self.engine.history(step).await?;
        self.navigated(before).await
    }

    async fn wait(
        &self,
        condition: &WaitCondition,
        timeout: Duration,
    ) -> Result<PageInfo, BrowserError> {
        if *condition == WaitCondition::NetworkIdle {
            return self.network_idle(timeout).await;
        }
        let started = Instant::now();
        loop {
            if self.wait_once(condition).await? {
                return self.page_info(self.engine.page().await?).await;
            }
            if started.elapsed() >= timeout {
                return Err(BrowserError::new(
                    ErrorCode::Timeout,
                    "the condition did not hold",
                ));
            }
            tokio::time::sleep(Duration::from_millis(100).min(timeout)).await;
        }
    }

    async fn request_user(&self, reason: &str) -> Result<Handback, BrowserError> {
        self.hand_over.request_user(reason).await
    }
}

#[derive(serde::Deserialize)]
struct TextAnswer {
    url: String,
    title: String,
    text: String,
}

#[cfg(test)]
mod tests;

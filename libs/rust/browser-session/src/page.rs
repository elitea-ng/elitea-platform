//! What the engine says about the page, kept current by one event task.
//!
//! The task reads every CDP event and keeps a [`PageState`]: navigations
//! requested and committed, loads, the network requests in flight, the
//! out-of-process frames, and the hard-limit refusals the page itself caused
//! (a navigation to a refused scheme, a document the egress proxy refused).
//! It also answers what the engine asks of a client: the proxy's credentials
//! (`Fetch.authRequired`), paused requests, JavaScript dialogs, new targets
//! waiting for the debugger, and popups. The driver reads the state and
//! waits on [`Tracker::changed`] instead of polling the engine.
//!
//! Every reply the task sends runs on a task of its own, so a slow reply
//! never holds up the events behind it.

use std::collections::{HashMap, HashSet};
use std::sync::{Arc, Mutex, MutexGuard};
use std::time::{Duration, Instant};

use base64::Engine as _;
use elitea_browser_tools::call::check_url;
use serde_json::{Value, json};
use tokio::sync::{broadcast, watch};

use crate::cdp::{Connection, Event};

/// The header the egress proxy sets on every refusal
/// (`services/elitea-egress-proxy`, `RefusalHeader`).
pub const EGRESS_REFUSAL_HEADER: &str = "x-elitea-egress-refused";

/// Chromium's error when the proxy answers a `CONNECT` with anything but
/// 200: the egress proxy refusing the host (it answers 200 before it dials,
/// so an unreachable host is a closed tunnel, not this).
pub const TUNNEL_FAILED: &str = "ERR_TUNNEL_CONNECTION_FAILED";

/// How long the network must be quiet to count as idle.
pub const NETWORK_IDLE: Duration = Duration::from_millis(500);

/// The page as the events describe it.
#[derive(Debug)]
pub struct PageState {
    /// The main frame's URL (fragment included).
    pub url: String,
    /// The page's title, as the target events last reported it.
    pub title: String,
    /// Main-frame loads that started: a cross-document navigation (a link,
    /// a form, a script, ours). Same-document navigations start none.
    pub navigations_requested: u64,
    /// Main-frame documents committed: every one makes every ref stale.
    pub documents: u64,
    /// Main-frame load events.
    pub loads: u64,
    /// The document the last load event belonged to (a `documents` value).
    pub loaded_document: u64,
    /// The main frame is loading.
    pub loading: bool,
    /// Bumped when events were lost: the page may have changed in ways the
    /// state does not show, so every ref is treated as stale.
    pub dom_epoch: u64,
    /// Network requests in flight, and when that set last changed.
    pub in_flight: HashSet<String>,
    pub network_changed: Instant,
    /// Main-frame navigations the page started to a refused scheme
    /// (`check_url`); each is sent back to `about:blank`.
    pub scheme_refusals: u64,
    /// Main-frame documents the egress proxy refused: an `http` response
    /// carrying its refusal header, or an `https` tunnel it would not open.
    pub egress_refusals: u64,
    /// Main-frame document requests in flight (their request ids).
    pub document_requests: HashSet<String>,
    /// Out-of-process frames: frame id to their flattened session.
    pub frame_sessions: HashMap<String, String>,
    /// The engine stopped answering: every call fails from now on.
    pub closed: bool,
}

impl PageState {
    fn new() -> Self {
        Self {
            url: "about:blank".to_owned(),
            title: String::new(),
            navigations_requested: 0,
            documents: 0,
            loads: 0,
            loaded_document: 0,
            loading: false,
            dom_epoch: 0,
            in_flight: HashSet::new(),
            network_changed: Instant::now(),
            scheme_refusals: 0,
            egress_refusals: 0,
            document_requests: HashSet::new(),
            frame_sessions: HashMap::new(),
            closed: false,
        }
    }

    /// The network has had nothing in flight for [`NETWORK_IDLE`].
    #[must_use]
    pub fn network_idle(&self, now: Instant) -> bool {
        self.in_flight.is_empty() && now.duration_since(self.network_changed) >= NETWORK_IDLE
    }
}

/// The counters a call compares before and after itself.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct Marks {
    pub navigations_requested: u64,
    pub documents: u64,
    pub loads: u64,
    pub dom_epoch: u64,
    pub scheme_refusals: u64,
    pub egress_refusals: u64,
}

/// The shared page state and its change signal.
#[derive(Clone)]
pub struct Tracker {
    state: Arc<Mutex<PageState>>,
    version: Arc<watch::Sender<u64>>,
}

impl Tracker {
    #[must_use]
    pub fn new() -> Self {
        let (version, _) = watch::channel(0);
        Self {
            state: Arc::new(Mutex::new(PageState::new())),
            version: Arc::new(version),
        }
    }

    /// The state, locked. Never held across an await.
    pub fn lock(&self) -> MutexGuard<'_, PageState> {
        self.state
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
    }

    #[must_use]
    pub fn marks(&self) -> Marks {
        let state = self.lock();
        Marks {
            navigations_requested: state.navigations_requested,
            documents: state.documents,
            loads: state.loads,
            dom_epoch: state.dom_epoch,
            scheme_refusals: state.scheme_refusals,
            egress_refusals: state.egress_refusals,
        }
    }

    /// A receiver that wakes on every change.
    #[must_use]
    pub fn changed(&self) -> watch::Receiver<u64> {
        self.version.subscribe()
    }

    fn update(&self, change: impl FnOnce(&mut PageState)) {
        change(&mut self.lock());
        self.version.send_modify(|version| *version += 1);
    }

    /// Wait until `done` holds or `timeout` passes; whether it held.
    pub async fn wait_until(
        &self,
        timeout: Duration,
        mut done: impl FnMut(&PageState) -> bool,
    ) -> bool {
        let deadline = tokio::time::Instant::now() + timeout;
        let mut changed = self.changed();
        loop {
            {
                let state = self.lock();
                if done(&state) {
                    return true;
                }
                if state.closed {
                    return false;
                }
            }
            // Re-check at least every 100 ms: some conditions are about time
            // (network idle), not only about events.
            let step = (tokio::time::Instant::now() + Duration::from_millis(100)).min(deadline);
            if tokio::time::timeout_at(step, changed.changed())
                .await
                .is_ok_and(|r| r.is_err())
            {
                return false;
            }
            if tokio::time::Instant::now() >= deadline {
                let state = self.lock();
                return done(&state);
            }
        }
    }
}

impl Default for Tracker {
    fn default() -> Self {
        Self::new()
    }
}

/// What the event task needs to know about the session.
#[derive(Clone)]
pub struct Watch {
    pub cdp: Connection,
    /// The page's flattened session and target (its main frame id).
    pub page_session: String,
    pub page_target: String,
    /// The proxy's run token: answered on `Fetch.authRequired` from the proxy.
    pub proxy_token: Option<String>,
    /// Answer the page's dialogs (the cloud page has no person at it). Off
    /// for the desktop's page, whose person answers them.
    pub answer_dialogs: bool,
}

/// Start the event task. Subscribe before the first command whose events
/// matter: pass the receiver taken before it.
/// Proxy-auth request ids remembered (one answer per request) before the
/// set is cleared.
const MAX_ANSWERED_AUTH: usize = 4096;

pub fn spawn(watch: Watch, tracker: Tracker, mut events: broadcast::Receiver<Arc<Event>>) {
    tokio::spawn(async move {
        let mut answered_auth: HashSet<String> = HashSet::new();
        let mut popups: HashSet<String> = HashSet::new();
        loop {
            let event = match events.recv().await {
                Ok(event) => event,
                Err(broadcast::error::RecvError::Lagged(_)) => {
                    // Events were lost: assume the page changed.
                    tracker.update(|state| state.dom_epoch += 1);
                    continue;
                }
                Err(broadcast::error::RecvError::Closed) => break,
            };
            handle(&watch, &tracker, &event, &mut answered_auth, &mut popups);
        }
        tracker.update(|state| state.closed = true);
    });
}

fn reply(cdp: &Connection, session: Option<String>, method: &'static str, params: Value) {
    let cdp = cdp.clone();
    tokio::spawn(async move {
        let _ = cdp.call(session.as_deref(), method, params).await;
    });
}

#[allow(clippy::too_many_lines)]
fn handle(
    watch: &Watch,
    tracker: &Tracker,
    event: &Event,
    answered_auth: &mut HashSet<String>,
    popups: &mut HashSet<String>,
) {
    let on_page = event.session_id.as_deref() == Some(watch.page_session.as_str());
    let params = &event.params;
    let main_frame = |frame_id: &Value| frame_id.as_str() == Some(watch.page_target.as_str());
    match event.method.as_str() {
        "Page.frameStartedLoading" if on_page && main_frame(&params["frameId"]) => {
            tracker.update(|state| {
                state.navigations_requested += 1;
                state.loading = true;
            });
        }
        "Page.frameStoppedLoading" if on_page && main_frame(&params["frameId"]) => {
            tracker.update(|state| state.loading = false);
        }
        "Page.loadEventFired" if on_page => {
            tracker.update(|state| {
                state.loads += 1;
                state.loaded_document = state.documents;
                state.loading = false;
            });
        }
        "Page.frameNavigated" if on_page && main_frame(&params["frame"]["id"]) => {
            let frame = &params["frame"];
            let url = format!(
                "{}{}",
                frame["url"].as_str().unwrap_or_default(),
                frame["urlFragment"].as_str().unwrap_or_default()
            );
            // A navigation the page started is held to the scheme limit too.
            let refused = url != "about:blank" && check_url(&url).is_err();
            tracker.update(|state| {
                state.documents += 1;
                state.url.clone_from(&url);
                if refused {
                    state.scheme_refusals += 1;
                }
            });
            if refused {
                reply(
                    &watch.cdp,
                    Some(watch.page_session.clone()),
                    "Page.navigate",
                    json!({ "url": "about:blank" }),
                );
            }
        }
        "Page.navigatedWithinDocument" if on_page && main_frame(&params["frameId"]) => {
            let url = params["url"].as_str().unwrap_or_default().to_owned();
            tracker.update(|state| state.url = url);
        }
        "Page.javascriptDialogOpening" if watch.answer_dialogs => {
            // The agent cannot see a dialog: an alert or a "leave this
            // page?" is accepted, a confirm or prompt declined, so the page
            // never decides something on the agent's behalf.
            let accept = matches!(params["type"].as_str(), Some("alert" | "beforeunload"));
            reply(
                &watch.cdp,
                event.session_id.clone(),
                "Page.handleJavaScriptDialog",
                json!({ "accept": accept }),
            );
        }
        "Network.requestWillBeSent" if on_page => {
            if let Some(id) = params["requestId"].as_str() {
                let id = id.to_owned();
                let document = params["type"] == "Document" && main_frame(&params["frameId"]);
                tracker.update(|state| {
                    if document {
                        state.document_requests.insert(id.clone());
                    }
                    state.in_flight.insert(id);
                    state.network_changed = Instant::now();
                });
            }
        }
        "Network.loadingFinished" | "Network.loadingFailed" if on_page => {
            if let Some(id) = params["requestId"].as_str() {
                let tunnel_refused = event.method == "Network.loadingFailed"
                    && params["errorText"]
                        .as_str()
                        .is_some_and(|text| text.contains(TUNNEL_FAILED));
                let proxied = watch.proxy_token.is_some();
                tracker.update(|state| {
                    if state.in_flight.remove(id) {
                        state.network_changed = Instant::now();
                    }
                    if state.document_requests.remove(id) && tunnel_refused && proxied {
                        state.egress_refusals += 1;
                    }
                });
            }
        }
        "Network.responseReceived"
            if on_page
                && params["type"] == "Document"
                && main_frame(&params["frameId"])
                && has_header(&params["response"]["headers"], EGRESS_REFUSAL_HEADER) =>
        {
            tracker.update(|state| state.egress_refusals += 1);
        }
        "Fetch.requestPaused" => {
            // Paused only because request interception carries the proxy
            // authentication: every request continues unchanged.
            if let Some(id) = params["requestId"].as_str() {
                reply(
                    &watch.cdp,
                    event.session_id.clone(),
                    "Fetch.continueRequest",
                    json!({ "requestId": id }),
                );
            }
        }
        "Fetch.authRequired" => {
            let Some(id) = params["requestId"].as_str() else {
                return;
            };
            let from_proxy = params["authChallenge"]["source"] == "Proxy";
            // Bounded: a request is challenged once per proxy connection,
            // and an id is never reused within a session.
            if answered_auth.len() >= MAX_ANSWERED_AUTH {
                answered_auth.clear();
            }
            // One answer per request: a refused token is not retried, and a
            // site's own sign-in is the person's (there is no credential
            // vault, ADR-0049 decision 8).
            let response = match &watch.proxy_token {
                Some(token) if from_proxy && answered_auth.insert(id.to_owned()) => json!({
                    "response": "ProvideCredentials",
                    "username": "run",
                    "password": token,
                }),
                _ => json!({ "response": "CancelAuth" }),
            };
            reply(
                &watch.cdp,
                event.session_id.clone(),
                "Fetch.continueWithAuth",
                json!({ "requestId": id, "authChallengeResponse": response }),
            );
        }
        "Target.attachedToTarget" => {
            attached(watch, tracker, params);
        }
        "Target.detachedFromTarget" => {
            if let Some(session) = params["sessionId"].as_str() {
                tracker.update(|state| state.frame_sessions.retain(|_, s| s != session));
            }
        }
        "Target.targetCreated" | "Target.targetInfoChanged" if event.session_id.is_none() => {
            let info = &params["targetInfo"];
            if info["targetId"].as_str() == Some(watch.page_target.as_str()) {
                let title = info["title"].as_str().unwrap_or_default().to_owned();
                tracker.update(|state| state.title = title);
            } else {
                popup(watch, info, popups);
            }
        }
        "Target.targetDestroyed" if event.session_id.is_none() => {
            if let Some(id) = params["targetId"].as_str() {
                popups.remove(id);
            }
        }
        _ => {}
    }
}

fn has_header(headers: &Value, name: &str) -> bool {
    headers
        .as_object()
        .is_some_and(|headers| headers.keys().any(|key| key.eq_ignore_ascii_case(name)))
}

/// A target the page's auto-attach announced: an out-of-process frame, a
/// worker. Each is set up like the page (proxy authentication, its own
/// children attached) and then released: it waits for the debugger.
fn attached(watch: &Watch, tracker: &Tracker, params: &Value) {
    let Some(session) = params["sessionId"].as_str().map(str::to_owned) else {
        return;
    };
    let info = &params["targetInfo"];
    let is_frame = info["type"] == "iframe";
    if is_frame && let Some(frame) = info["targetId"].as_str() {
        let frame = frame.to_owned();
        let session = session.clone();
        tracker.update(|state| {
            state.frame_sessions.insert(frame, session);
        });
    }
    let cdp = watch.cdp.clone();
    let token = watch.proxy_token.is_some();
    tokio::spawn(async move {
        let session = Some(session.as_str());
        if token {
            let _ = cdp.call(session, "Fetch.enable", fetch_params()).await;
        }
        if is_frame {
            for method in ["Page.enable", "DOM.enable"] {
                let _ = cdp.call(session, method, json!({})).await;
            }
            let _ = cdp
                .call(session, "Target.setAutoAttach", auto_attach_params())
                .await;
        }
        let _ = cdp
            .call(session, "Runtime.runIfWaitingForDebugger", json!({}))
            .await;
    });
}

/// `Fetch.enable` for the proxy's authentication challenge.
#[must_use]
pub fn fetch_params() -> Value {
    json!({
        "handleAuthRequests": true,
        "patterns": [{ "urlPattern": "*", "requestStage": "Request" }],
    })
}

#[must_use]
pub fn auto_attach_params() -> Value {
    json!({ "autoAttach": true, "waitForDebuggerOnStart": true, "flatten": true })
}

/// A new page (a `window.open`, a `target=_blank` link). The session has
/// one pane: the popup is closed, and the pane follows its URL when the
/// scheme limit allows it.
fn popup(watch: &Watch, info: &Value, popups: &mut HashSet<String>) {
    let Some(id) = info["targetId"].as_str() else {
        return;
    };
    if info["type"] != "page" || id == watch.page_target {
        return;
    }
    let url = info["url"].as_str().unwrap_or_default();
    let first_sight = popups.insert(id.to_owned());
    let real_url = !url.is_empty() && url != "about:blank";
    if !real_url && !first_sight {
        return;
    }
    if real_url {
        popups.remove(id);
        if check_url(url).is_ok() {
            reply(
                &watch.cdp,
                Some(watch.page_session.clone()),
                "Page.navigate",
                json!({ "url": url }),
            );
        }
        reply(
            &watch.cdp,
            None,
            "Target.closeTarget",
            json!({ "targetId": id }),
        );
    }
}

/// `Proxy-Authorization` value for a run token (for tests and logs that
/// must recognise it, never for the command line).
#[must_use]
pub fn proxy_authorization(token: &str) -> String {
    format!(
        "Basic {}",
        base64::engine::general_purpose::STANDARD.encode(format!("run:{token}"))
    )
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn idle_needs_quiet_time() {
        let mut state = PageState::new();
        let now = Instant::now();
        state.network_changed = now;
        assert!(!state.network_idle(now));
        assert!(state.network_idle(now + NETWORK_IDLE));
        state.in_flight.insert("1".to_owned());
        assert!(!state.network_idle(now + NETWORK_IDLE * 4));
    }

    #[test]
    fn headers_match_without_case() {
        assert!(has_header(
            &json!({"X-Elitea-Egress-Refused": "host-not-allowed"}),
            EGRESS_REFUSAL_HEADER
        ));
        assert!(!has_header(
            &json!({"content-type": "text/html"}),
            EGRESS_REFUSAL_HEADER
        ));
    }

    #[test]
    fn the_proxy_credential_is_basic_run() {
        assert_eq!(proxy_authorization("t0k"), "Basic cnVuOnQwaw==");
    }

    #[tokio::test]
    async fn wait_until_wakes_on_change_and_times_out() {
        let tracker = Tracker::new();
        let waiter = tracker.clone();
        let handle = tokio::spawn(async move {
            waiter
                .wait_until(Duration::from_secs(5), |state| state.loads > 0)
                .await
        });
        tokio::time::sleep(Duration::from_millis(20)).await;
        tracker.update(|state| state.loads += 1);
        assert!(handle.await.unwrap());
        assert!(
            !tracker
                .wait_until(Duration::from_millis(50), |state| state.loads > 1)
                .await
        );
    }
}

//! The macOS driver over the vendored agent-desktop adapters.
//!
//! Every vendor type stays in this file. The snapshot path is the vendor's
//! own (`snapshot::build`: window resolution, AX read under a deadline,
//! element tokens, uniqueness proof); refs are re-numbered into our
//! snapshot-scoped form and the vendor's `RefEntry` is kept as the binding
//! handle, so resolution and stale detection are the vendor's too.
//!
//! Capture uses the vendor's own path (`/usr/sbin/screencapture` with a
//! window id or a display index) rather than screencapturekit-rs: that
//! crate's build script compiles a Swift package with `swift build`, which
//! the Linux CI and this crate's build rules do not admit, and the vendor
//! path already verifies the window identity before and after the capture.
//! Coordinate input is the vendor's own `CGEvent` code (patched to tag every
//! event, vendor/README.md).

use std::sync::Mutex;
use std::time::{Duration, Instant};

use agent_desktop_core as ad;
use agent_desktop_core::{ObservationOps, RefEntry, SystemOps};

use crate::guard::{self, HostState};
use crate::snapshot::{Binding, RawNode, RefRegistry, build_tree};
use crate::{
    Action, ActionOutcome, AppInfo, Capture, CaptureTarget, Delivery, DesktopDriver, DesktopError,
    ElementRef, Grant, KeyChord, Modifier, Node, OwnApps, Permissions, Rect, ScreenStatus,
    ScrollDirection, Snapshot, SnapshotBudget, SyntheticMarker, WaitCondition, WindowInfo,
    WindowTarget,
};

/// How many snapshots the driver keeps refs for.
const HELD_SNAPSHOTS: usize = 8;
/// The deadline of one snapshot read.
const SNAPSHOT_TIMEOUT_MS: u64 = 5_000;
/// The deadline of one action, key or capture.
const ACTION_TIMEOUT_MS: u64 = 5_000;
/// The deadline of the frontmost-app read behind the screensaver check.
const SCREEN_CHECK_TIMEOUT_MS: u64 = 1_000;
/// The poll interval of `wait`.
const WAIT_POLL: Duration = Duration::from_millis(150);

/// What a snapshot keeps beside its bindings: the window it read.
type Payload = ad::WindowInfo;

/// The macOS [`DesktopDriver`].
///
/// Before each call it checks the screen (`screen_unavailable`), before each
/// act macOS secure input (`secure_field_refused`), and on every target
/// whether it is Elitea itself (`own_window_refused`; [`OwnApps`]).
pub struct MacDriver {
    adapter: agent_desktop_macos::MacOSAdapter,
    refs: Mutex<RefRegistry<RefEntry, Payload>>,
    marker: SyntheticMarker,
    own: OwnApps,
}

impl Default for MacDriver {
    fn default() -> Self {
        Self::new()
    }
}

impl MacDriver {
    /// A driver with the default synthetic-event marker.
    #[must_use]
    pub fn new() -> Self {
        let tag = agent_desktop_macos::synthetic_event_tag();
        Self {
            adapter: agent_desktop_macos::MacOSAdapter::new(),
            refs: Mutex::new(RefRegistry::new(HELD_SNAPSHOTS)),
            marker: SyntheticMarker::new(tag).unwrap_or_else(|| unreachable!("tag is never 0")),
            own: OwnApps::default(),
        }
    }

    /// The same driver, refusing `own` as Elitea's processes. The default
    /// set is this process and `ai.elitea.desktop`; the host adds its
    /// helper processes with [`OwnApps::with_extra`].
    #[must_use]
    pub fn with_own_apps(mut self, own: OwnApps) -> Self {
        self.own = own;
        self
    }

    /// A driver that stamps `marker` into the events it posts. The tag is
    /// process-wide (one `CGEvent` field), so the last driver made wins.
    #[must_use]
    pub fn with_marker(marker: SyntheticMarker) -> Self {
        agent_desktop_macos::set_synthetic_event_tag(marker.value());
        let mut driver = Self::new();
        driver.marker = marker;
        driver
    }

    fn registry(&self) -> std::sync::MutexGuard<'_, RefRegistry<RefEntry, Payload>> {
        // A panic while holding the lock leaves only a ref list behind; refs
        // into it are still correct or stale, so the poison is ignored.
        self.refs
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
    }

    /// The window `target` names; `own_window_refused` when it is Elitea's.
    fn resolve_window(&self, target: &WindowTarget) -> Result<ad::WindowInfo, DesktopError> {
        if let WindowTarget::App(bundle_id) = target
            && self.own.is_own_bundle(bundle_id)
        {
            return Err(DesktopError::OwnWindowRefused(bundle_id.clone()));
        }
        let deadline = deadline(SNAPSHOT_TIMEOUT_MS)?;
        let window = self.resolve_window_unchecked(target, deadline)?;
        self.refuse_own_pid(window.pid, deadline, "an Elitea window")?;
        Ok(window)
    }

    fn resolve_window_unchecked(
        &self,
        target: &WindowTarget,
        deadline: ad::Deadline,
    ) -> Result<ad::WindowInfo, DesktopError> {
        match target {
            WindowTarget::Focused => self
                .adapter
                .focused_window(deadline)
                .map_err(map_err)?
                .ok_or_else(|| DesktopError::WindowNotFound("no focused window".into())),
            WindowTarget::Window(id) => self
                .vendor_windows(None, deadline)?
                .into_iter()
                .find(|w| &w.id == id)
                .ok_or_else(|| DesktopError::WindowNotFound(id.clone())),
            WindowTarget::App(bundle_id) => {
                let windows = self.vendor_windows(Some(bundle_id), deadline)?;
                windows
                    .iter()
                    .find(|w| w.state.is_focused)
                    .or_else(|| windows.first())
                    .cloned()
                    .ok_or_else(|| {
                        DesktopError::WindowNotFound(format!("no window of {bundle_id}"))
                    })
            }
        }
    }

    /// The vendor's windows, of every app or of the app with `bundle_id`.
    fn vendor_windows(
        &self,
        bundle_id: Option<&str>,
        deadline: ad::Deadline,
    ) -> Result<Vec<ad::WindowInfo>, DesktopError> {
        let app = match bundle_id {
            None => None,
            Some(bundle) => Some(self.app_by_bundle(bundle, deadline)?),
        };
        let filter = ad::WindowFilter {
            focused_only: false,
            app: app.as_ref().map(|a| a.name.clone()),
        };
        let mut windows = self
            .adapter
            .list_windows(&filter, deadline)
            .map_err(map_err)?;
        if let Some(app) = app {
            windows.retain(|w| w.pid == app.pid);
        }
        Ok(windows)
    }

    fn app_by_bundle(
        &self,
        bundle: &str,
        deadline: ad::Deadline,
    ) -> Result<ad::AppInfo, DesktopError> {
        self.adapter
            .list_apps(deadline)
            .map_err(map_err)?
            .into_iter()
            .find(|a| {
                a.bundle_id
                    .as_deref()
                    .is_some_and(|b| b.eq_ignore_ascii_case(bundle))
            })
            .ok_or_else(|| DesktopError::AppNotFound(bundle.to_owned()))
    }

    /// `own_window_refused` when the process `pid` is Elitea, by pid or by
    /// bundle id.
    fn refuse_own_pid(
        &self,
        pid: ad::ProcessId,
        deadline: ad::Deadline,
        what: &str,
    ) -> Result<(), DesktopError> {
        self.own.refuse(pid.get(), None, what)?;
        let bundle = self.bundle_of(pid, deadline);
        self.own.refuse(pid.get(), bundle.as_deref(), what)
    }

    /// The pids of every running Elitea process.
    fn own_pids(&self, deadline: ad::Deadline) -> Result<Vec<ad::ProcessId>, DesktopError> {
        Ok(self
            .adapter
            .list_apps(deadline)
            .map_err(map_err)?
            .into_iter()
            .filter(|a| self.own.is_own(a.pid.get(), a.bundle_id.as_deref()))
            .map(|a| a.pid)
            .collect())
    }

    /// `own_window_refused` when the frontmost application is Elitea: input
    /// with no element target lands there.
    fn refuse_own_frontmost(&self, deadline: ad::Deadline) -> Result<(), DesktopError> {
        if let Some(app) = self.adapter.focused_app(deadline).map_err(map_err)? {
            self.own.refuse(
                app.pid.get(),
                app.bundle_id.as_deref(),
                "Elitea is the frontmost application",
            )?;
        }
        Ok(())
    }

    fn bundle_of(&self, pid: ad::ProcessId, deadline: ad::Deadline) -> Option<String> {
        self.adapter
            .list_apps(deadline)
            .ok()?
            .into_iter()
            .find(|a| a.pid == pid)
            .and_then(|a| a.bundle_id)
    }

    /// The vendor entry and our binding of `element_ref`, with a fresh
    /// handle to the live element.
    fn resolve_ref(
        &self,
        element_ref: &ElementRef,
        deadline: ad::Deadline,
    ) -> Result<(Binding<RefEntry>, ad::NativeHandle), DesktopError> {
        let (binding, pid) = {
            let registry = self.registry();
            let (binding, window) = registry.resolve(element_ref)?;
            (binding.clone(), window.pid)
        };
        // A snapshot of an Elitea window is never taken; this holds even if
        // a pid was reused or the set of own apps grew since.
        self.refuse_own_pid(pid, deadline, "an Elitea window")?;
        let handle = ad::refs::resolve_saved_ref::resolve_saved_ref(
            &self.adapter,
            &binding.handle,
            deadline,
            None,
        )
        .map_err(|e| match e.code {
            ad::ErrorCode::StaleRef | ad::ErrorCode::ElementNotFound => {
                DesktopError::stale(element_ref, e.message)
            }
            _ => map_err(e),
        })?;
        Ok((binding, handle))
    }

    /// Refuses when the live element is a secure field, whatever the
    /// snapshot said.
    fn refuse_if_secure(
        &self,
        handle: &ad::NativeHandle,
        deadline: ad::Deadline,
    ) -> Result<(), DesktopError> {
        let state = self
            .adapter
            .get_live_state(handle, deadline)
            .map_err(map_err)?;
        if state.is_some_and(|s| s.states.iter().any(|st| st == ad::tree::state::SECURE)) {
            return Err(DesktopError::SecureFieldRefused);
        }
        Ok(())
    }

    /// Refuses when the focused element of `pid` is a secure field. macOS
    /// secure input is checked before every act ([`guard::before_act`]).
    fn refuse_if_focus_secure(&self, pid: u32, deadline: ad::Deadline) -> Result<(), DesktopError> {
        if let Some(focused) =
            agent_desktop_macos::elitea_ext::focused_element(pid, deadline).map_err(map_err)?
        {
            self.refuse_if_secure(&focused, deadline)?;
        }
        Ok(())
    }

    fn lease(deadline: ad::Deadline) -> Result<ad::InteractionLease, DesktopError> {
        // An in-process lease: the driver is the only caller in this
        // process, and the vendor's file lock under /tmp is for its CLI.
        ad::InteractionLease::guarded(deadline, ()).map_err(map_err)
    }

    fn run_action(
        &self,
        element_ref: &ElementRef,
        action: ad::Action,
        writes_text: bool,
    ) -> Result<ActionOutcome, DesktopError> {
        // Every act, whatever its kind, is refused while secure input is
        // on; the caller ran `guard::before_act` before this.
        let deadline = deadline(ACTION_TIMEOUT_MS)?;
        let (binding, handle) = self.resolve_ref(element_ref, deadline)?;
        if writes_text {
            if binding.secure {
                return Err(DesktopError::SecureFieldRefused);
            }
            self.refuse_if_secure(&handle, deadline)?;
        }
        let name = action.name();
        let lease = Self::lease(deadline)?;
        let result = ad::execute_verified_action(
            &self.adapter,
            &handle,
            ad::ActionRequest::headless(action),
            &lease,
        )
        .map_err(map_err)?;
        Ok(outcome(name, &result))
    }

    fn snapshot_inner(
        &self,
        target: &WindowTarget,
        budget: &SnapshotBudget,
    ) -> Result<Snapshot, DesktopError> {
        let window = self.resolve_window(target)?;
        let deadline = deadline(SNAPSHOT_TIMEOUT_MS)?;
        let opts = ad::TreeOptions {
            max_depth: budget.max_depth,
            include_bounds: true,
            interactive_only: false,
            compact: false,
            surface: ad::SnapshotSurface::Window,
            skeleton: false,
            force_renderer_accessibility: false,
        };
        let result = ad::snapshot::build(&self.adapter, &opts, None, Some(&window.id), deadline)
            .map_err(map_app_err)?;
        let raw = to_raw(&result.tree, &result.refmap);
        let window_info = self.to_window(&window, deadline);
        let mut registry = self.registry();
        let id = registry.next_id();
        let built = build_tree(&raw, budget, &id);
        let truncated = built.truncated || !result.complete;
        registry.insert(id.clone(), built.bindings, window);
        Ok(Snapshot {
            id,
            window: window_info,
            root: built.root,
            node_count: built.node_count,
            truncated,
        })
    }

    fn to_window(&self, w: &ad::WindowInfo, deadline: ad::Deadline) -> WindowInfo {
        WindowInfo {
            id: w.id.clone(),
            title: w.title.clone(),
            app_name: w.app.clone(),
            pid: w.pid.get(),
            bundle_id: self.bundle_of(w.pid, deadline),
            bounds: w.bounds.map(rect),
            focused: w.state.is_focused,
            minimized: w.state.minimized.unwrap_or(false),
        }
    }

    fn wait_once(&self, condition: &WaitCondition) -> Result<bool, DesktopError> {
        match condition {
            WaitCondition::Window {
                bundle_id,
                title_contains,
            } => {
                let deadline = deadline(SNAPSHOT_TIMEOUT_MS)?;
                let windows = match self.vendor_windows(Some(bundle_id), deadline) {
                    Ok(w) => w,
                    Err(DesktopError::AppNotFound(_)) => return Ok(false),
                    Err(e) => return Err(e),
                };
                Ok(windows.iter().any(|w| {
                    title_contains
                        .as_deref()
                        .is_none_or(|t| w.title.contains(t))
                }))
            }
            WaitCondition::RefState {
                element_ref,
                state,
                present,
            } => {
                let deadline = deadline(ACTION_TIMEOUT_MS)?;
                let (_, handle) = self.resolve_ref(element_ref, deadline)?;
                let live = self
                    .adapter
                    .get_live_state(&handle, deadline)
                    .map_err(map_err)?;
                let has = live.is_some_and(|s| s.states.iter().any(|st| st == state));
                Ok(has == *present)
            }
            WaitCondition::Text { target, text } => {
                let snap = self.snapshot_inner(
                    target,
                    &SnapshotBudget {
                        actionable_only: false,
                        max_nodes: 2_000,
                        ..SnapshotBudget::default()
                    },
                )?;
                Ok(contains_text(&snap.root, text))
            }
            WaitCondition::Stable { .. } => Ok(false),
        }
    }
}

impl HostState for MacDriver {
    fn screen(&self) -> ScreenStatus {
        let state = agent_desktop_macos::elitea_ext::screen_state();
        let screensaver = deadline(SCREEN_CHECK_TIMEOUT_MS)
            .ok()
            .and_then(|d| self.adapter.focused_app(d).ok().flatten())
            .is_some_and(|a| crate::is_screensaver(a.bundle_id.as_deref(), &a.name));
        ScreenStatus {
            session_present: state.session_present,
            on_console: state.on_console,
            locked: state.locked,
            screensaver,
            display_asleep: state.display_asleep,
        }
    }

    fn secure_input(&self) -> bool {
        agent_desktop_macos::elitea_ext::secure_event_input_enabled()
    }
}

impl DesktopDriver for MacDriver {
    fn permissions(&self) -> Permissions {
        let Ok(deadline) = deadline(ACTION_TIMEOUT_MS) else {
            return Permissions {
                accessibility: Grant::Unknown,
                screen_recording: Grant::Unknown,
            };
        };
        match self.adapter.permission_report(deadline) {
            Ok(r) => Permissions {
                accessibility: grant(&r.accessibility),
                screen_recording: grant(&r.screen_recording),
            },
            Err(_) => Permissions {
                accessibility: Grant::Unknown,
                screen_recording: Grant::Unknown,
            },
        }
    }

    fn list_apps(&self) -> Result<Vec<AppInfo>, DesktopError> {
        guard::before_call(self)?;
        let deadline = deadline(SNAPSHOT_TIMEOUT_MS)?;
        let focused = self
            .adapter
            .focused_app(deadline)
            .ok()
            .flatten()
            .map(|a| a.pid);
        Ok(self
            .adapter
            .list_apps(deadline)
            .map_err(map_err)?
            .into_iter()
            // Elitea itself is not listed: the agent never reads it.
            .filter(|a| !self.own.is_own(a.pid.get(), a.bundle_id.as_deref()))
            .map(|a| AppInfo {
                focused: Some(a.pid) == focused,
                name: a.name,
                pid: a.pid.get(),
                bundle_id: a.bundle_id,
            })
            .collect())
    }

    fn list_windows(&self, bundle_id: Option<&str>) -> Result<Vec<WindowInfo>, DesktopError> {
        guard::before_call(self)?;
        if let Some(bundle) = bundle_id
            && self.own.is_own_bundle(bundle)
        {
            return Err(DesktopError::OwnWindowRefused(bundle.to_owned()));
        }
        let deadline = deadline(SNAPSHOT_TIMEOUT_MS)?;
        let own = self.own_pids(deadline)?;
        let mut windows = self.vendor_windows(bundle_id, deadline)?;
        windows.retain(|w| !own.contains(&w.pid) && !self.own.is_own(w.pid.get(), None));
        Ok(windows
            .iter()
            .map(|w| self.to_window(w, deadline))
            .collect())
    }

    fn snapshot(
        &self,
        target: &WindowTarget,
        budget: &SnapshotBudget,
    ) -> Result<Snapshot, DesktopError> {
        guard::before_call(self)?;
        self.snapshot_inner(target, budget)
    }

    fn act(
        &self,
        element_ref: &ElementRef,
        action: &Action,
    ) -> Result<ActionOutcome, DesktopError> {
        action.validate()?;
        guard::before_act(self)?;
        let vendor = match action {
            Action::Press => ad::Action::Click,
            Action::SetValue { value } => ad::Action::SetValue(value.clone()),
            Action::Focus => ad::Action::SetFocus,
            Action::Select { option } => ad::Action::Select(option.clone()),
            Action::Scroll { direction, amount } => ad::Action::Scroll(
                match direction {
                    ScrollDirection::Up => ad::Direction::Up,
                    ScrollDirection::Down => ad::Direction::Down,
                    ScrollDirection::Left => ad::Direction::Left,
                    ScrollDirection::Right => ad::Direction::Right,
                },
                *amount,
            ),
            Action::Expand => ad::Action::Expand,
            Action::Collapse => ad::Action::Collapse,
            Action::Toggle => ad::Action::Toggle,
        };
        let mut out = self.run_action(element_ref, vendor, action.writes_text())?;
        action.name().clone_into(&mut out.action);
        Ok(out)
    }

    fn type_text(
        &self,
        element_ref: Option<&ElementRef>,
        text: &str,
    ) -> Result<ActionOutcome, DesktopError> {
        crate::validate_text_write(text)?;
        guard::before_act(self)?;
        if let Some(element_ref) = element_ref {
            let mut out =
                self.run_action(element_ref, ad::Action::TypeText(text.to_owned()), true)?;
            "type".clone_into(&mut out.action);
            return Ok(out);
        }
        let deadline = deadline(ACTION_TIMEOUT_MS)?;
        let app = self
            .adapter
            .focused_app(deadline)
            .map_err(map_err)?
            .ok_or_else(|| DesktopError::AppNotFound("no frontmost application".into()))?;
        let pid = app.pid.get();
        self.own.refuse(
            pid,
            app.bundle_id.as_deref(),
            "Elitea is the frontmost application",
        )?;
        self.refuse_if_focus_secure(pid, deadline)?;
        let focused = agent_desktop_macos::elitea_ext::focused_element(pid, deadline)
            .map_err(map_err)?
            .ok_or_else(|| {
                DesktopError::ActionFailed("the frontmost app has no focused element".into())
            })?;
        let lease = Self::lease(deadline)?;
        let result = ad::execute_verified_action(
            &self.adapter,
            &focused,
            ad::ActionRequest::headless(ad::Action::TypeText(text.to_owned())),
            &lease,
        )
        .map_err(map_err)?;
        Ok(outcome("type", &result))
    }

    fn key(&self, target: &WindowTarget, chord: &KeyChord) -> Result<ActionOutcome, DesktopError> {
        if crate::is_denied_chord(chord) {
            return Err(DesktopError::ChordRefused(chord_text(chord)));
        }
        guard::before_act(self)?;
        let combo = ad::KeyCombo {
            key: chord.key.clone(),
            modifiers: chord
                .modifiers
                .iter()
                .map(|m| match m {
                    Modifier::Command => ad::Modifier::Meta,
                    Modifier::Control => ad::Modifier::Ctrl,
                    Modifier::Option => ad::Modifier::Alt,
                    Modifier::Shift => ad::Modifier::Shift,
                })
                .collect(),
        };
        if self.adapter.is_blocked_combo(&combo) {
            return Err(DesktopError::ChordRefused(chord_text(chord)));
        }
        let window = self.resolve_window(target)?;
        let deadline = deadline(ACTION_TIMEOUT_MS)?;
        // A chord may land in the frontmost app rather than the window.
        self.refuse_own_frontmost(deadline)?;
        self.refuse_if_focus_secure(window.pid.get(), deadline)?;
        let lease = Self::lease(deadline)?;
        let result = self
            .adapter
            .press_key_for_window(&window, &combo, ad::InteractionPolicy::headless(), &lease)
            .map_err(map_err)?;
        Ok(outcome("key", &result))
    }

    fn capture(&self, target: &CaptureTarget) -> Result<Capture, DesktopError> {
        guard::before_call(self)?;
        let deadline = deadline(ACTION_TIMEOUT_MS)?;
        match target {
            CaptureTarget::Window { id } => {
                let window = self.resolve_window(&WindowTarget::Window(id.clone()))?;
                let image = self
                    .adapter
                    .screenshot(ad::ScreenshotTarget::ExactWindow(window.clone()), deadline)
                    .map_err(map_err)?;
                Ok(Capture {
                    width: image.width,
                    height: image.height,
                    scale: image.scale_factor,
                    region: window.bounds.map_or(
                        Rect {
                            x: 0.0,
                            y: 0.0,
                            width: 0.0,
                            height: 0.0,
                        },
                        rect,
                    ),
                    png: image.data,
                })
            }
            CaptureTarget::Region { rect: region } => {
                if !region.is_valid() || region.width < 1.0 || region.height < 1.0 {
                    return Err(DesktopError::InvalidArgs("empty or invalid region".into()));
                }
                // Refused, not masked, when any Elitea window shows in it.
                let own = self.own_pids(deadline)?;
                let own_frames: Vec<Rect> = self
                    .vendor_windows(None, deadline)?
                    .iter()
                    .filter(|w| own.contains(&w.pid) || self.own.is_own(w.pid.get(), None))
                    .filter_map(|w| w.bounds.map(rect))
                    .collect();
                guard::refuse_region(region, &own_frames)?;
                let displays = self.adapter.list_displays(deadline).map_err(map_err)?;
                let (index, display) = displays
                    .iter()
                    .enumerate()
                    .find(|(_, d)| contains(&rect(d.bounds), region))
                    .ok_or_else(|| {
                        DesktopError::InvalidArgs("the region is not inside one display".into())
                    })?;
                let image = self
                    .adapter
                    .screenshot(
                        ad::ScreenshotTarget::Display {
                            index,
                            expected: display.clone(),
                        },
                        deadline,
                    )
                    .map_err(map_err)?;
                crop(&image.data, &rect(display.bounds), region, display.scale)
            }
        }
    }

    fn wait(&self, condition: &WaitCondition, timeout: Duration) -> Result<(), DesktopError> {
        let end = Instant::now() + timeout;
        let mut previous: Option<Node> = None;
        if let WaitCondition::Window { bundle_id, .. } = condition
            && self.own.is_own_bundle(bundle_id)
        {
            return Err(DesktopError::OwnWindowRefused(bundle_id.clone()));
        }
        loop {
            // The screen can lock while the wait polls.
            guard::before_call(self)?;
            let done = if let WaitCondition::Stable { target } = condition {
                let snap = self.snapshot_inner(target, &SnapshotBudget::default())?;
                let same = previous.as_ref().is_some_and(|p| same_shape(p, &snap.root));
                previous = Some(snap.root);
                same
            } else {
                self.wait_once(condition)?
            };
            if done {
                return Ok(());
            }
            if Instant::now() >= end {
                return Err(DesktopError::Timeout("the condition did not hold".into()));
            }
            std::thread::sleep(WAIT_POLL.min(end.saturating_duration_since(Instant::now())));
        }
    }

    fn synthetic_marker(&self) -> SyntheticMarker {
        self.marker
    }

    fn forget_snapshots(&self) {
        self.registry().clear();
    }
}

fn deadline(ms: u64) -> Result<ad::Deadline, DesktopError> {
    ad::Deadline::after(ms).map_err(map_err)
}

/// Converts the vendor tree into raw nodes. A node gets a handle exactly
/// when the vendor allocated it a ref; the handle is the vendor's entry.
fn to_raw(node: &ad::AccessibilityNode, refmap: &ad::RefMap) -> RawNode<RefEntry> {
    let secure = node
        .presentation
        .states
        .iter()
        .any(|s| s == ad::tree::state::SECURE);
    let handle = node
        .ref_id
        .as_deref()
        .and_then(|id| refmap.get(id))
        .cloned();
    RawNode {
        role: node.role.clone(),
        name: node.identity.name.clone(),
        value: if secure {
            None
        } else {
            node.identity.value.clone()
        },
        description: node.identity.description.clone(),
        secure,
        states: node.presentation.states.clone(),
        actions: node.presentation.available_actions.clone(),
        bounds: node.presentation.bounds.map(rect),
        handle,
        children_cut: node.subtree_truncated
            || (node.children.is_empty() && node.children_count.is_some_and(|c| c > 0)),
        children: node.children.iter().map(|c| to_raw(c, refmap)).collect(),
    }
}

fn rect(r: ad::Rect) -> Rect {
    Rect {
        x: r.x,
        y: r.y,
        width: r.width,
        height: r.height,
    }
}

fn contains(outer: &Rect, inner: &Rect) -> bool {
    inner.x >= outer.x
        && inner.y >= outer.y
        && inner.x + inner.width <= outer.x + outer.width
        && inner.y + inner.height <= outer.y + outer.height
}

/// Crops a display capture to `region` (both in points) and re-encodes it.
fn crop(png: &[u8], display: &Rect, region: &Rect, scale: f64) -> Result<Capture, DesktopError> {
    let img = image::load_from_memory_with_format(png, image::ImageFormat::Png)
        .map_err(|e| DesktopError::Internal(format!("decode capture: {e}")))?;
    let px = |v: f64| -> u32 {
        // Points to pixels; the display is at most a few thousand points, so
        // the value fits and is never negative after the contains check.
        let v = (v * scale).round().max(0.0);
        if v >= f64::from(u32::MAX) {
            u32::MAX
        } else {
            #[allow(clippy::cast_possible_truncation, clippy::cast_sign_loss)]
            {
                v as u32
            }
        }
    };
    let x = px(region.x - display.x).min(img.width());
    let y = px(region.y - display.y).min(img.height());
    let w = px(region.width).min(img.width() - x).max(1);
    let h = px(region.height).min(img.height() - y).max(1);
    let cropped = img.crop_imm(x, y, w, h);
    let mut out = std::io::Cursor::new(Vec::new());
    cropped
        .write_to(&mut out, image::ImageFormat::Png)
        .map_err(|e| DesktopError::Internal(format!("encode capture: {e}")))?;
    Ok(Capture {
        png: out.into_inner(),
        width: w,
        height: h,
        scale,
        region: *region,
    })
}

fn contains_text(node: &Node, text: &str) -> bool {
    node.name.as_deref().is_some_and(|n| n.contains(text))
        || node.value.as_deref().is_some_and(|v| v.contains(text))
        || node.children.iter().any(|c| contains_text(c, text))
}

/// Two trees are the same when roles, names, values and states match; refs
/// differ between snapshots by construction and are ignored.
fn same_shape(a: &Node, b: &Node) -> bool {
    a.role == b.role
        && a.name == b.name
        && a.value == b.value
        && a.states == b.states
        && a.children.len() == b.children.len()
        && a.children
            .iter()
            .zip(&b.children)
            .all(|(x, y)| same_shape(x, y))
}

fn outcome(action: &str, result: &ad::ActionResult) -> ActionOutcome {
    let physical = result
        .steps
        .iter()
        .any(|s| s.mechanism() == Some(ad::StepMechanism::PhysicalSynthetic));
    let disposition = result.disposition();
    let delivery = if disposition == ad::DeliverySemantics::NotDelivered {
        Delivery::NotNeeded
    } else if physical {
        Delivery::Physical
    } else {
        Delivery::Semantic
    };
    ActionOutcome {
        action: action.to_owned(),
        delivery,
        verified: disposition == ad::DeliverySemantics::DeliveredVerified,
    }
}

fn chord_text(chord: &KeyChord) -> String {
    let mut parts: Vec<&str> = chord
        .modifiers
        .iter()
        .map(|m| match m {
            Modifier::Command => "cmd",
            Modifier::Control => "ctrl",
            Modifier::Option => "alt",
            Modifier::Shift => "shift",
        })
        .collect();
    parts.push(&chord.key);
    parts.join("+")
}

fn grant(state: &ad::PermissionState) -> Grant {
    match state {
        ad::PermissionState::Granted => Grant::Granted,
        ad::PermissionState::Denied { .. } => Grant::Denied,
        ad::PermissionState::NotRequired => Grant::NotRequired,
        ad::PermissionState::Unknown => Grant::Unknown,
    }
}

fn map_app_err(e: ad::AppError) -> DesktopError {
    match e {
        ad::AppError::Adapter(e) => map_err(e),
        other => DesktopError::Internal(other.to_string()),
    }
}

fn map_err(e: ad::AdapterError) -> DesktopError {
    use ad::ErrorCode as C;
    let msg = e.message;
    match e.code {
        C::PermDenied => DesktopError::PermissionDenied(msg),
        C::ElementNotFound | C::StaleRef | C::SnapshotNotFound => DesktopError::StaleRef {
            element_ref: String::new(),
            reason: msg,
        },
        C::AppNotFound => DesktopError::AppNotFound(msg),
        C::WindowNotFound => DesktopError::WindowNotFound(msg),
        C::ActionNotSupported | C::PlatformNotSupported => DesktopError::ActionNotSupported(msg),
        C::ActionFailed | C::AmbiguousTarget | C::AppUnresponsive => {
            DesktopError::ActionFailed(msg)
        }
        C::PolicyDenied => DesktopError::ChordRefused(msg),
        C::Timeout => DesktopError::Timeout(msg),
        C::InvalidArgs => DesktopError::InvalidArgs(msg),
        C::NotificationNotFound | C::Internal => DesktopError::Internal(msg),
    }
}

//! The macOS driver over the vendored agent-desktop adapters.
//!
//! Every vendor type stays in this file. The snapshot path is the vendor's
//! own (`snapshot::build`: window resolution, AX read under a deadline,
//! element tokens, uniqueness proof); refs are re-numbered into our
//! snapshot-scoped form and the vendor's `RefEntry` is kept as the binding
//! handle, so resolution and stale detection are the vendor's too.
//!
//! Capture uses the vendor's own path (`/usr/sbin/screencapture -o -l`,
//! one window without its shadow; a zoom crops it) rather than
//! screencapturekit-rs: that
//! crate's build script compiles a Swift package with `swift build`, which
//! the Linux CI and this crate's build rules do not admit, and the vendor
//! path already verifies the window identity before and after the capture.
//! Coordinate input is the vendor's own `CGEvent` code (patched to tag every
//! event, vendor/README.md).

use std::sync::Mutex;
use std::time::{Duration, Instant};

use agent_desktop_core as ad;
use agent_desktop_core::{InputOps, ObservationOps, RefEntry, SystemOps};

use crate::guard::{self, HostState};
use crate::snapshot::{Binding, RawNode, RefRegistry, build_tree};
use crate::{
    Action, ActionOutcome, AppInfo, Capture, CaptureTarget, ClickAt, Delivery, DesktopDriver,
    DesktopError, ElementRef, Grant, KeyChord, Modifier, Node, OpenedApp, OwnApps, Permissions,
    PointerButton, Rect, ScreenStatus, ScrollDirection, Snapshot, SnapshotBudget, SyntheticMarker,
    WaitCondition, WindowInfo, WindowTarget, bundle_matches, rect_contains, region_pixels,
    topmost_at,
};

/// How many snapshots the driver keeps refs for.
const HELD_SNAPSHOTS: usize = 8;
/// The deadline of one snapshot read.
const SNAPSHOT_TIMEOUT_MS: u64 = 5_000;
/// The deadline of one action, key or capture.
const ACTION_TIMEOUT_MS: u64 = 5_000;
/// The deadline of `open_app`: the launch and the first window.
const LAUNCH_TIMEOUT_MS: u64 = 10_000;
/// The deadline of the frontmost-app read behind the screensaver check.
const SCREEN_CHECK_TIMEOUT_MS: u64 = 1_000;
/// The poll interval of `wait`.
const WAIT_POLL: Duration = Duration::from_millis(150);

/// What a snapshot keeps beside its bindings: the window it read and the
/// bundle id of that window's application, read when the snapshot was
/// taken (`None` when the OS did not tell it).
type Payload = (ad::WindowInfo, Option<String>);

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
        if let Some(bundle_id) = target.bundle_id()
            && self.own.is_own_bundle(bundle_id)
        {
            return Err(DesktopError::OwnWindowRefused(bundle_id.to_owned()));
        }
        let deadline = deadline(SNAPSHOT_TIMEOUT_MS)?;
        let window = self.resolve_window_unchecked(target, deadline)?;
        self.refuse_own_pid(window.pid, deadline, "an Elitea window")?;
        // The window must be the named app's: a window id of another app,
        // or of an app whose bundle id cannot be read, is not found.
        if let Some(bundle_id) = target.bundle_id()
            && !bundle_matches(self.bundle_of(window.pid, deadline).as_deref(), bundle_id)
        {
            return Err(DesktopError::WindowNotFound(format!(
                "{} is not a window of {bundle_id}",
                window.id
            )));
        }
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
            WindowTarget::Window { bundle_id, id } => self
                .vendor_windows(Some(bundle_id), deadline)?
                .into_iter()
                .find(|w| &w.id == id)
                .ok_or_else(|| {
                    DesktopError::WindowNotFound(format!("{id} is not a window of {bundle_id}"))
                }),
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

    /// The frontmost application, which must be `bundle_id`: input with no
    /// element target (untargeted typing, a key chord, a coordinate click)
    /// lands there. `own_window_refused` when it is Elitea, `not_frontmost`
    /// when it is another app or none, or its bundle id cannot be read.
    fn require_frontmost(
        &self,
        bundle_id: &str,
        deadline: ad::Deadline,
    ) -> Result<ad::AppInfo, DesktopError> {
        let app = self.adapter.focused_app(deadline).map_err(map_err)?;
        if let Some(app) = &app {
            self.own.refuse(
                app.pid.get(),
                app.bundle_id.as_deref(),
                "Elitea is the frontmost application",
            )?;
        }
        let frontmost = match &app {
            None => guard::Frontmost::None,
            Some(app) => app
                .bundle_id
                .as_deref()
                .map_or(guard::Frontmost::Unknown, guard::Frontmost::Bundle),
        };
        guard::require_frontmost(frontmost, bundle_id)?;
        app.ok_or_else(|| DesktopError::NotFrontmost("no frontmost application".into()))
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
        bundle_id: &str,
        element_ref: &ElementRef,
        deadline: ad::Deadline,
    ) -> Result<(Binding<RefEntry>, ad::NativeHandle), DesktopError> {
        let (binding, pid, taken_of) = {
            let registry = self.registry();
            let (binding, (window, bundle)) = registry.resolve(element_ref)?;
            (binding.clone(), window.pid, bundle.clone())
        };
        // A snapshot of an Elitea window is never taken; this holds even if
        // a pid was reused or the set of own apps grew since.
        self.refuse_own_pid(pid, deadline, "an Elitea window")?;
        // The ref belongs to the named app, as the snapshot recorded it and
        // as the process says now: an unknown bundle id is not the named one.
        if !bundle_matches(taken_of.as_deref(), bundle_id)
            || !bundle_matches(self.bundle_of(pid, deadline).as_deref(), bundle_id)
        {
            return Err(DesktopError::stale(
                element_ref,
                format!("the ref's snapshot was not taken of {bundle_id}"),
            ));
        }
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
        bundle_id: &str,
        element_ref: &ElementRef,
        action: ad::Action,
        writes_text: bool,
    ) -> Result<ActionOutcome, DesktopError> {
        // Every act, whatever its kind, is refused while secure input is
        // on; the caller ran `guard::before_act` before this.
        let deadline = deadline(ACTION_TIMEOUT_MS)?;
        let (binding, handle) = self.resolve_ref(bundle_id, element_ref, deadline)?;
        // The menu path to a denied chord: Quit, Log Out, Shut Down, ...
        if crate::is_denied_menu_item(&binding.role, binding.name.as_deref()) {
            return Err(DesktopError::MenuItemRefused(
                binding.name.clone().unwrap_or_default(),
            ));
        }
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

    /// One snapshot of the window `target` names. With `store`, its refs
    /// are held (a `snapshot` call); without, nothing is stored and no held
    /// ref is evicted (the polls of `wait`).
    fn snapshot_inner(
        &self,
        target: &WindowTarget,
        budget: &SnapshotBudget,
        store: bool,
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
        if store {
            let bundle = window_info.bundle_id.clone();
            registry.insert(id.clone(), built.bindings, (window, bundle));
        }
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
                bundle_id,
                element_ref,
                state,
                present,
            } => {
                let deadline = deadline(ACTION_TIMEOUT_MS)?;
                let (_, handle) = self.resolve_ref(bundle_id, element_ref, deadline)?;
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
                    false,
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
    fn screen(&self) -> ScreenStatus {
        HostState::screen(self)
    }

    fn secure_input(&self) -> bool {
        HostState::secure_input(self)
    }

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
        self.snapshot_inner(target, budget, true)
    }

    fn act(
        &self,
        bundle_id: &str,
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
        let mut out = self.run_action(bundle_id, element_ref, vendor, action.writes_text())?;
        action.name().clone_into(&mut out.action);
        Ok(out)
    }

    fn type_text(
        &self,
        bundle_id: &str,
        element_ref: Option<&ElementRef>,
        text: &str,
    ) -> Result<ActionOutcome, DesktopError> {
        crate::validate_text_write(text)?;
        guard::before_act(self)?;
        if self.own.is_own_bundle(bundle_id) {
            return Err(DesktopError::OwnWindowRefused(bundle_id.to_owned()));
        }
        if let Some(element_ref) = element_ref {
            let mut out = self.run_action(
                bundle_id,
                element_ref,
                ad::Action::TypeText(text.to_owned()),
                true,
            )?;
            "type".clone_into(&mut out.action);
            return Ok(out);
        }
        let deadline = deadline(ACTION_TIMEOUT_MS)?;
        // Untargeted text lands in the frontmost app: only the named one.
        let app = self.require_frontmost(bundle_id, deadline)?;
        let pid = app.pid.get();
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
        let Some(bundle_id) = target.bundle_id() else {
            return Err(DesktopError::InvalidArgs(
                "a key chord names the application it is for".into(),
            ));
        };
        let window = self.resolve_window(target)?;
        let deadline = deadline(ACTION_TIMEOUT_MS)?;
        // A chord lands in the frontmost app rather than the window: only
        // the named one.
        self.require_frontmost(bundle_id, deadline)?;
        self.refuse_if_focus_secure(window.pid.get(), deadline)?;
        let lease = Self::lease(deadline)?;
        let result = self
            .adapter
            .press_key_for_window(&window, &combo, ad::InteractionPolicy::headless(), &lease)
            .map_err(map_err)?;
        Ok(outcome("key", &result))
    }

    fn click_at(&self, click: &ClickAt) -> Result<ActionOutcome, DesktopError> {
        click.validate()?;
        guard::before_act(self)?;
        let deadline = deadline(ACTION_TIMEOUT_MS)?;
        let window = self.resolve_window(&WindowTarget::Window {
            bundle_id: click.bundle_id.clone(),
            id: click.window_id.clone(),
        })?;
        // Physical input goes to whatever app is frontmost: only the named one.
        self.require_frontmost(&click.bundle_id, deadline)?;
        let bounds = window
            .bounds
            .map(rect)
            .ok_or_else(|| DesktopError::WindowNotFound("the window has no frame".into()))?;
        if !rect_contains(&bounds, &click.point) {
            return Err(DesktopError::InvalidArgs(
                "the point is outside the window".into(),
            ));
        }
        // The click lands on whatever window is on top at the point: refused
        // unless it is the approved one. CoreGraphics lists the windows front
        // to back, so the first that holds the point is on top.
        let own = self.own_pids(deadline)?;
        let windows: Vec<WindowInfo> = self
            .vendor_windows(None, deadline)?
            .iter()
            .filter(|w| w.state.visible != Some(false))
            .map(|w| WindowInfo {
                id: w.id.clone(),
                title: String::new(),
                app_name: String::new(),
                pid: w.pid.get(),
                bundle_id: None,
                bounds: w.bounds.map(rect),
                focused: w.state.is_focused,
                minimized: w.state.minimized.unwrap_or(false),
            })
            .collect();
        match topmost_at(&windows, &click.point) {
            Some(top) if top.id == window.id => {}
            Some(top)
                if own.iter().any(|p| p.get() == top.pid) || self.own.is_own(top.pid, None) =>
            {
                return Err(DesktopError::OwnWindowRefused(
                    "an Elitea window covers the point".into(),
                ));
            }
            _ => {
                return Err(DesktopError::OwnWindowRefused(
                    "another window covers the point: bring the window forward first".into(),
                ));
            }
        }
        let lease = Self::lease(deadline)?;
        self.adapter
            .mouse_event(
                ad::MouseEvent {
                    kind: ad::MouseEventKind::Click { count: click.count },
                    point: ad::Point {
                        x: click.point.x,
                        y: click.point.y,
                    },
                    button: match click.button {
                        PointerButton::Left => ad::MouseButton::Left,
                        PointerButton::Right => ad::MouseButton::Right,
                    },
                    modifiers: Vec::new(),
                },
                &lease,
            )
            .map_err(map_err)?;
        Ok(ActionOutcome {
            action: "click_at".to_owned(),
            delivery: Delivery::Physical,
            verified: false,
        })
    }

    fn open_app(&self, bundle_id: &str) -> Result<OpenedApp, DesktopError> {
        if self.own.is_own_bundle(bundle_id) {
            return Err(DesktopError::OwnWindowRefused(bundle_id.to_owned()));
        }
        // A bundle id, never a path or a bare name the vendor would resolve
        // through Launch Services to some other app.
        let valid = bundle_id.contains('.')
            && !bundle_id.starts_with('.')
            && !bundle_id.ends_with('.')
            && !bundle_id.contains("..")
            && bundle_id
                .chars()
                .all(|c| c.is_ascii_alphanumeric() || matches!(c, '.' | '-' | '_'));
        if !valid {
            return Err(DesktopError::InvalidArgs(format!(
                "`{bundle_id}` is not a bundle id"
            )));
        }
        guard::before_act(self)?;
        let deadline = deadline(LAUNCH_TIMEOUT_MS)?;
        let lease = Self::lease(deadline)?;
        let options = ad::system::launch_options::LaunchOptions {
            timeout_ms: LAUNCH_TIMEOUT_MS,
            activate: true,
            ..ad::system::launch_options::LaunchOptions::default()
        };
        let launched = self
            .adapter
            .launch_app(bundle_id, &options, &lease)
            .map_err(map_err)?;
        // A pid reused, or a bundle that resolves to Elitea after all.
        self.refuse_own_pid(launched.pid, deadline, "Elitea")?;
        let window = launched
            .window
            .as_ref()
            .map(|w| self.to_window(w, deadline));
        Ok(OpenedApp {
            name: launched.app,
            pid: launched.pid.get(),
            bundle_id: bundle_id.to_owned(),
            window,
        })
    }

    fn capture(&self, target: &CaptureTarget) -> Result<Capture, DesktopError> {
        guard::before_call(self)?;
        let (bundle_id, id, region) = match target {
            CaptureTarget::Window { bundle_id, id } => (bundle_id, id, None),
            CaptureTarget::WindowRegion {
                bundle_id,
                id,
                region,
            } => (bundle_id, id, Some(region)),
        };
        let window = self.resolve_window(&WindowTarget::Window {
            bundle_id: bundle_id.clone(),
            id: id.clone(),
        })?;
        let frame = window
            .bounds
            .map(rect)
            .ok_or_else(|| DesktopError::WindowNotFound("the window has no frame".into()))?;
        let deadline = deadline(ACTION_TIMEOUT_MS)?;
        // The window alone (no other window that overlaps it shows) and
        // without its shadow (`screencapture -o`), so the image is exactly
        // the frame times the scale and a pixel maps back to a point.
        let image = self
            .adapter
            .screenshot_window_frame(&window, deadline)
            .map_err(map_err)?;
        match region {
            None => Ok(Capture {
                width: image.width,
                height: image.height,
                scale: image.scale_factor,
                region: frame,
                png: image.data,
            }),
            Some(region) => {
                let (x, y, w, h) =
                    region_pixels(frame.width, frame.height, region, image.width, image.height)?;
                let png = crop(&image.data, x, y, w, h)?;
                Ok(Capture {
                    png,
                    width: w,
                    height: h,
                    scale: image.scale_factor,
                    region: Rect {
                        x: frame.x + region.x,
                        y: frame.y + region.y,
                        width: region.width,
                        height: region.height,
                    },
                })
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
                // A poll stores nothing: the held refs stay held.
                let snap = self.snapshot_inner(target, &SnapshotBudget::default(), false)?;
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

/// Crops a window capture to the pixel box (`x`, `y`, `w`, `h`) and
/// re-encodes it as PNG.
fn crop(png: &[u8], x: u32, y: u32, w: u32, h: u32) -> Result<Vec<u8>, DesktopError> {
    let img = image::load_from_memory_with_format(png, image::ImageFormat::Png)
        .map_err(|e| DesktopError::Internal(format!("decode capture: {e}")))?;
    let cropped = img.crop_imm(x, y, w, h);
    let mut out = std::io::Cursor::new(Vec::new());
    cropped
        .write_to(&mut out, image::ImageFormat::Png)
        .map_err(|e| DesktopError::Internal(format!("encode capture: {e}")))?;
    Ok(out.into_inner())
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

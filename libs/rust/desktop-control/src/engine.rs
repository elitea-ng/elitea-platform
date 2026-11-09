//! The Windows and Linux drivers (ADR-0047 decision 10): one engine over the
//! vendored agent-desktop `windows` or `linux` adapter, the same contract
//! as the macOS driver (`macos.rs`), with each OS's own rules from
//! [`crate::platform`].
//!
//! Every vendor type stays in this file and the two OS files. The snapshot
//! path is the vendor's own (`snapshot::build`); refs are re-numbered into
//! our snapshot-scoped form and the vendor's `RefEntry` is kept as the
//! binding handle, so resolution and stale detection are the vendor's too.
//!
//! What differs from macOS, and why:
//!
//! - **Application ids.** Neither OS has bundle ids. An application is
//!   named by its id from [`crate::platform::windows_app_id`] (the
//!   executable image) or [`crate::platform::linux_app_id`] (the desktop
//!   entry id), carried in [`AppInfo::bundle_id`] so the toolset, the
//!   approvals and the policy are unchanged.
//! - **The platform deny list** is applied here, in bypass mode too: a
//!   denied application is left out of the lists and refused everywhere
//!   else (`app_denied`).
//! - **The focused element.** Neither adapter exports a focused-element
//!   read. Untargeted typing and a key chord read the frontmost window's
//!   tree and refuse when its focused node is secure. When no focused node
//!   can be found (it may sit deeper than the search reads, the focus
//!   cannot be shown not to be a secure field) untargeted typing is
//!   refused, and so is a chord that inserts text (a character, paste;
//!   [`platform::chord_inserts_text`]); a navigation chord passes.
//! - **No text into File Explorer** (Windows,
//!   [`platform::text_entry_refusal`]): its address bar runs what is typed
//!   into it, so typing, setting a value and an inserting chord are refused
//!   in every `explorer.exe` window (`app_denied`); clicks, navigation keys
//!   and reads pass.
//! - **Secure input** is a macOS notion; the secure desktop (UAC, sign-in)
//!   is part of the Windows screen check instead.

use std::sync::Mutex;
use std::time::{Duration, Instant};

use agent_desktop_core as ad;

use crate::guard::{self, HostState};
use crate::platform::{self, Os};
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
/// The poll interval of `wait`.
const WAIT_POLL: Duration = Duration::from_millis(150);
/// The depth the focused-element search reads below the frontmost window.
const FOCUS_SEARCH_DEPTH: u8 = 30;

/// What a snapshot keeps beside its bindings: the window it read and the
/// id of that window's application when the snapshot was taken.
type Payload = (ad::WindowInfo, Option<String>);

/// Starts an OS's user-active watch.
/// The thread it starts holds the [`crate::input::Alive`]'s guard, so the
/// driver sees the watch die.
pub(crate) type WatchFn = fn(
    SyntheticMarker,
    crate::InputFilter,
    crate::SignalSink,
    crate::input::Alive,
) -> Result<Box<dyn crate::InputWatch>, DesktopError>;

/// The OS reads and the input watch the engine leaves to its OS file.
pub(crate) struct OsHooks {
    pub os: Os,
    /// The application id of a vendor app ([`crate::platform`]).
    pub app_id: fn(&ad::AppInfo) -> Option<String>,
    /// The screen, read live.
    pub screen: fn() -> ScreenStatus,
    /// The marker the watch reports the driver's own events with.
    pub marker: SyntheticMarker,
    /// Starts the user-active guard's watch.
    pub watch: WatchFn,
    /// The OS's own rule on one window, beyond its owner's id: on Windows
    /// a packaged app's frame is judged by the app it hosts and only
    /// Explorer's folder windows pass (`app_denied`,
    /// [`platform::windows_window_verdict`]). Linux has none.
    pub check_window: fn(&ad::WindowInfo) -> Result<(), DesktopError>,
}

/// The Windows or Linux [`DesktopDriver`]: `WindowsDriver` and
/// `LinuxDriver` are this type on their OS.
///
/// Before each call it checks the screen (`screen_unavailable`), and on
/// every target whether it is Elitea itself (`own_window_refused`) or an
/// application on the platform deny list (`app_denied`).
pub struct NativeDriver {
    adapter: Box<dyn ad::PlatformAdapter>,
    hooks: OsHooks,
    refs: Mutex<RefRegistry<ad::RefEntry, Payload>>,
    own: OwnApps,
    halt: Mutex<Option<crate::Halt>>,
    guard: crate::input::GuardState,
}

impl NativeDriver {
    pub(crate) fn with_hooks(adapter: Box<dyn ad::PlatformAdapter>, hooks: OsHooks) -> Self {
        Self {
            adapter,
            hooks,
            refs: Mutex::new(RefRegistry::new(HELD_SNAPSHOTS)),
            own: OwnApps::default(),
            halt: Mutex::new(None),
            guard: crate::input::GuardState::default(),
        }
    }

    /// The same driver, refusing `own` as the host app's processes. The
    /// default set is this process only; the host passes
    /// [`OwnApps::for_app`] with its bundle id.
    #[must_use]
    pub fn with_own_apps(mut self, own: OwnApps) -> Self {
        self.own = own;
        self
    }

    fn registry(&self) -> std::sync::MutexGuard<'_, RefRegistry<ad::RefEntry, Payload>> {
        self.refs
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
    }

    fn app_id(&self, app: &ad::AppInfo) -> Option<String> {
        (self.hooks.app_id)(app)
    }

    /// `own_window_refused` for Elitea, `app_denied` for a denied app.
    fn refuse_id(&self, id: &str) -> Result<(), DesktopError> {
        if self.hooks.os == Os::Linux
            && let Some(why) = platform::unnameable_app(id)
        {
            return Err(DesktopError::InvalidArgs(why));
        }
        if self.own.is_own_bundle(id) {
            return Err(DesktopError::OwnWindowRefused(id.to_owned()));
        }
        if platform::is_denied_app(self.hooks.os, id) {
            return Err(DesktopError::AppDenied(id.to_owned()));
        }
        Ok(())
    }

    /// `app_denied` when this OS never puts text into `id`'s windows
    /// ([`platform::text_entry_refusal`]: File Explorer on Windows).
    fn refuse_text_entry(&self, id: &str) -> Result<(), DesktopError> {
        match platform::text_entry_refusal(self.hooks.os, id) {
            Some(why) => Err(DesktopError::AppDenied(why)),
            None => Ok(()),
        }
    }

    fn apps(&self, deadline: ad::Deadline) -> Result<Vec<ad::AppInfo>, DesktopError> {
        self.adapter.list_apps(deadline).map_err(map_err)
    }

    fn id_of(&self, pid: ad::ProcessId, deadline: ad::Deadline) -> Option<String> {
        self.apps(deadline)
            .ok()?
            .into_iter()
            .find(|a| a.pid == pid)
            .and_then(|a| self.app_id(&a))
    }

    /// Whether the process `pid` is Elitea or a denied app, by pid or id.
    fn is_refused_pid(&self, pid: u32, id: Option<&str>) -> bool {
        self.own.is_own(pid, id) || id.is_some_and(|id| platform::is_denied_app(self.hooks.os, id))
    }

    /// `own_window_refused` / `app_denied` when the process `pid` is one.
    fn refuse_pid(
        &self,
        pid: ad::ProcessId,
        deadline: ad::Deadline,
        what: &str,
    ) -> Result<(), DesktopError> {
        self.own.refuse(pid.get(), None, what)?;
        match self.id_of(pid, deadline) {
            Some(id) => {
                self.own.refuse(pid.get(), Some(&id), what)?;
                self.refuse_id(&id)
            }
            None => Ok(()),
        }
    }

    fn app_by_id(&self, id: &str, deadline: ad::Deadline) -> Result<ad::AppInfo, DesktopError> {
        self.apps(deadline)?
            .into_iter()
            .find(|a| bundle_matches(self.app_id(a).as_deref(), id))
            .ok_or_else(|| DesktopError::AppNotFound(id.to_owned()))
    }

    fn vendor_windows(
        &self,
        id: Option<&str>,
        deadline: ad::Deadline,
    ) -> Result<Vec<ad::WindowInfo>, DesktopError> {
        let app = match id {
            None => None,
            Some(id) => Some(self.app_by_id(id, deadline)?),
        };
        // The windows of every app, then the app's by pid: the vendor's
        // name filter would match aliases and other names.
        let filter = ad::WindowFilter {
            focused_only: false,
            app: None,
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

    fn resolve_window(&self, target: &WindowTarget) -> Result<ad::WindowInfo, DesktopError> {
        if let Some(id) = target.bundle_id() {
            self.refuse_id(id)?;
        }
        let deadline = deadline(SNAPSHOT_TIMEOUT_MS)?;
        let window = match target {
            WindowTarget::Focused => self
                .adapter
                .focused_window(deadline)
                .map_err(map_err)?
                .ok_or_else(|| DesktopError::WindowNotFound("no focused window".into()))?,
            WindowTarget::Window { bundle_id, id } => self
                .vendor_windows(Some(bundle_id), deadline)?
                .into_iter()
                .find(|w| &w.id == id)
                .ok_or_else(|| {
                    DesktopError::WindowNotFound(format!("{id} is not a window of {bundle_id}"))
                })?,
            WindowTarget::App(bundle_id) => {
                let windows = self.vendor_windows(Some(bundle_id), deadline)?;
                windows
                    .iter()
                    .find(|w| w.state.is_focused)
                    .or_else(|| windows.first())
                    .cloned()
                    .ok_or_else(|| {
                        DesktopError::WindowNotFound(format!("no window of {bundle_id}"))
                    })?
            }
        };
        self.refuse_pid(window.pid, deadline, "a window of this app")?;
        (self.hooks.check_window)(&window)?;
        if let Some(id) = target.bundle_id()
            && !bundle_matches(self.id_of(window.pid, deadline).as_deref(), id)
        {
            return Err(DesktopError::WindowNotFound(format!(
                "{} is not a window of {id}",
                window.id
            )));
        }
        Ok(window)
    }

    /// The pids of every running Elitea or denied process.
    fn refused_pids(&self, deadline: ad::Deadline) -> Result<Vec<ad::ProcessId>, DesktopError> {
        Ok(self
            .apps(deadline)?
            .into_iter()
            .filter(|a| self.is_refused_pid(a.pid.get(), self.app_id(a).as_deref()))
            .map(|a| a.pid)
            .collect())
    }

    /// The frontmost window, whose application must be `id`.
    fn require_frontmost(
        &self,
        id: &str,
        deadline: ad::Deadline,
    ) -> Result<ad::WindowInfo, DesktopError> {
        let window = self.adapter.focused_window(deadline).map_err(map_err)?;
        let app_id = window.as_ref().and_then(|w| self.id_of(w.pid, deadline));
        if let Some(window) = &window {
            self.own.refuse(
                window.pid.get(),
                app_id.as_deref(),
                "this app is the frontmost application",
            )?;
            (self.hooks.check_window)(window)?;
        }
        let frontmost = match (&window, app_id.as_deref()) {
            (None, _) => guard::Frontmost::None,
            (Some(_), None) => guard::Frontmost::Unknown,
            (Some(_), Some(app)) => guard::Frontmost::Bundle(app),
        };
        guard::require_frontmost(frontmost, id)?;
        window.ok_or_else(|| DesktopError::NotFrontmost("no frontmost application".into()))
    }

    fn resolve_ref(
        &self,
        id: &str,
        element_ref: &ElementRef,
        deadline: ad::Deadline,
    ) -> Result<(Binding<ad::RefEntry>, ad::NativeHandle), DesktopError> {
        self.refuse_id(id)?;
        let (binding, window, taken_of) = {
            let registry = self.registry();
            let (binding, (window, app)) = registry.resolve(element_ref)?;
            (binding.clone(), window.clone(), app.clone())
        };
        let pid = window.pid;
        self.refuse_pid(pid, deadline, "a window of this app")?;
        (self.hooks.check_window)(&window)?;
        if !bundle_matches(taken_of.as_deref(), id)
            || !bundle_matches(self.id_of(pid, deadline).as_deref(), id)
        {
            return Err(DesktopError::stale(
                element_ref,
                format!("the ref's snapshot was not taken of {id}"),
            ));
        }
        let handle = ad::refs::resolve_saved_ref::resolve_saved_ref(
            self.adapter.as_ref(),
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

    fn refuse_if_secure(
        &self,
        handle: &ad::NativeHandle,
        deadline: ad::Deadline,
    ) -> Result<(), DesktopError> {
        let state = self
            .adapter
            .get_live_state(handle, deadline)
            .map_err(map_err)?;
        if state.is_some_and(|s| platform::is_secure_field(&s.role, &s.states)) {
            return Err(DesktopError::SecureFieldRefused);
        }
        Ok(())
    }

    /// The focused element of `window`, found in its tree: `secure_field_refused`
    /// when it is secure, `None` when no node reports focus.
    fn focused_in(
        &self,
        window: &ad::WindowInfo,
        deadline: ad::Deadline,
    ) -> Result<Option<RawNode<ad::RefEntry>>, DesktopError> {
        let opts = tree_options(FOCUS_SEARCH_DEPTH);
        let result = ad::snapshot::build(
            self.adapter.as_ref(),
            &opts,
            None,
            Some(&window.id),
            deadline,
        )
        .map_err(map_app_err)?;
        let raw = to_raw(&result.tree, &result.refmap);
        match focused_node(&raw) {
            Some(node) if node.secure => Err(DesktopError::SecureFieldRefused),
            found => Ok(found.cloned()),
        }
    }

    fn lease(&self, deadline: ad::Deadline) -> Result<ad::InteractionLease, DesktopError> {
        self.check_halt()?;
        ad::InteractionLease::guarded(deadline, ()).map_err(map_err)
    }

    fn check_halt(&self) -> Result<(), DesktopError> {
        match &*self
            .halt
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
        {
            Some(halt) => halt.check(),
            None => Ok(()),
        }
    }

    fn run_action(
        &self,
        id: &str,
        element_ref: &ElementRef,
        action: ad::Action,
        writes_text: bool,
    ) -> Result<ActionOutcome, DesktopError> {
        let deadline = deadline(ACTION_TIMEOUT_MS)?;
        let (binding, handle) = self.resolve_ref(id, element_ref, deadline)?;
        if crate::is_denied_menu_item(&binding.role, binding.name.as_deref()) {
            return Err(DesktopError::MenuItemRefused(
                binding.name.clone().unwrap_or_default(),
            ));
        }
        if writes_text {
            self.refuse_text_entry(id)?;
            if binding.secure {
                return Err(DesktopError::SecureFieldRefused);
            }
            self.refuse_if_secure(&handle, deadline)?;
        }
        let name = action.name();
        let lease = self.lease(deadline)?;
        let result = ad::execute_verified_action(
            self.adapter.as_ref(),
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
        store: bool,
    ) -> Result<Snapshot, DesktopError> {
        let window = self.resolve_window(target)?;
        let deadline = deadline(SNAPSHOT_TIMEOUT_MS)?;
        let opts = tree_options(budget.max_depth);
        let result = ad::snapshot::build(
            self.adapter.as_ref(),
            &opts,
            None,
            Some(&window.id),
            deadline,
        )
        .map_err(map_app_err)?;
        let raw = to_raw(&result.tree, &result.refmap);
        let window_info = self.to_window(&window, deadline);
        let mut registry = self.registry();
        let id = registry.next_id();
        let built = build_tree(&raw, budget, &id);
        let truncated = built.truncated || !result.complete;
        if store {
            let app = window_info.bundle_id.clone();
            registry.insert(id.clone(), built.bindings, (window, app));
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
            bundle_id: self.id_of(w.pid, deadline),
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

impl HostState for NativeDriver {
    fn screen(&self) -> ScreenStatus {
        (self.hooks.screen)()
    }

    fn secure_input(&self) -> bool {
        false
    }

    fn guard_dead(&self) -> bool {
        self.guard.dead()
    }
}

impl DesktopDriver for NativeDriver {
    fn screen(&self) -> ScreenStatus {
        HostState::screen(self)
    }

    fn secure_input(&self) -> bool {
        HostState::secure_input(self)
    }

    fn permissions(&self) -> Permissions {
        let unknown = Permissions {
            accessibility: Grant::Unknown,
            screen_recording: Grant::Unknown,
        };
        let Ok(deadline) = deadline(ACTION_TIMEOUT_MS) else {
            return unknown;
        };
        match self.adapter.permission_report(deadline) {
            Ok(r) => Permissions {
                accessibility: grant(&r.accessibility),
                screen_recording: grant(&r.screen_recording),
            },
            Err(_) => unknown,
        }
    }

    fn list_apps(&self) -> Result<Vec<AppInfo>, DesktopError> {
        guard::before_call(self)?;
        let deadline = deadline(SNAPSHOT_TIMEOUT_MS)?;
        let focused = self
            .adapter
            .focused_window(deadline)
            .ok()
            .flatten()
            .map(|w| w.pid);
        Ok(self
            .apps(deadline)?
            .into_iter()
            .filter_map(|a| {
                let id = self.app_id(&a);
                // Elitea and the denied apps are not listed.
                if self.is_refused_pid(a.pid.get(), id.as_deref()) {
                    return None;
                }
                Some(AppInfo {
                    focused: Some(a.pid) == focused,
                    name: a.name,
                    pid: a.pid.get(),
                    bundle_id: id,
                })
            })
            .collect())
    }

    fn list_windows(&self, bundle_id: Option<&str>) -> Result<Vec<WindowInfo>, DesktopError> {
        guard::before_call(self)?;
        if let Some(id) = bundle_id {
            self.refuse_id(id)?;
        }
        let deadline = deadline(SNAPSHOT_TIMEOUT_MS)?;
        let refused = self.refused_pids(deadline)?;
        let mut windows = self.vendor_windows(bundle_id, deadline)?;
        windows.retain(|w| {
            !refused.contains(&w.pid)
                && !self.own.is_own(w.pid.get(), None)
                && (self.hooks.check_window)(w).is_ok()
        });
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
        let mut out = self.run_action(
            bundle_id,
            element_ref,
            vendor_action(action),
            action.writes_text(),
        )?;
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
        self.refuse_id(bundle_id)?;
        self.refuse_text_entry(bundle_id)?;
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
        let window = self.require_frontmost(bundle_id, deadline)?;
        // Untargeted text needs the focused element: without one the focus
        // cannot be shown not to be a secure field.
        let entry = self
            .focused_in(&window, deadline)?
            .and_then(|node| node.handle)
            .ok_or_else(|| {
                DesktopError::ActionFailed(
                    "the focused element of the frontmost window cannot be found or take text: type into a ref instead".into(),
                )
            })?;
        let handle = ad::refs::resolve_saved_ref::resolve_saved_ref(
            self.adapter.as_ref(),
            &entry,
            deadline,
            None,
        )
        .map_err(map_err)?;
        self.refuse_if_secure(&handle, deadline)?;
        let lease = self.lease(deadline)?;
        let result = ad::execute_verified_action(
            self.adapter.as_ref(),
            &handle,
            ad::ActionRequest::headless(ad::Action::TypeText(text.to_owned())),
            &lease,
        )
        .map_err(map_err)?;
        Ok(outcome("type", &result))
    }

    fn key(&self, target: &WindowTarget, chord: &KeyChord) -> Result<ActionOutcome, DesktopError> {
        if platform::is_denied_chord_on(self.hooks.os, chord) {
            return Err(DesktopError::ChordRefused(chord_text(chord)));
        }
        guard::before_act(self)?;
        let combo = combo(chord);
        if self.adapter.is_blocked_combo(&combo) {
            return Err(DesktopError::ChordRefused(chord_text(chord)));
        }
        let Some(bundle_id) = target.bundle_id() else {
            return Err(DesktopError::InvalidArgs(
                "a key chord names the application it is for".into(),
            ));
        };
        // File Explorer's address bar runs what is typed: no inserting
        // chord reaches an Explorer window, wherever its focus is.
        if platform::chord_inserts_text(chord) {
            self.refuse_text_entry(bundle_id)?;
        }
        let window = self.resolve_window(target)?;
        let deadline = deadline(ACTION_TIMEOUT_MS)?;
        let front = self.require_frontmost(bundle_id, deadline)?;
        // A chord lands in the focused element: refused when it is secure.
        // When none is found, a chord that inserts text is refused too (the
        // focus may be a password field deeper than the search reads);
        // a navigation chord passes.
        if self.focused_in(&front, deadline)?.is_none() && platform::chord_inserts_text(chord) {
            return Err(DesktopError::ActionFailed(format!(
                "the focused element of the frontmost window cannot be found, so `{}` (it inserts text) could land in a secure field: type into a ref instead",
                chord_text(chord)
            )));
        }
        let lease = self.lease(deadline)?;
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
        // Both adapters list windows front to back (EnumWindows z-order; the
        // X11 stacking order), so the first that holds the point is on top.
        let refused = self.refused_pids(deadline)?;
        let windows: Vec<WindowInfo> = self
            .vendor_windows(None, deadline)?
            .iter()
            .filter(|w| w.state.visible != Some(false) && w.state.minimized != Some(true))
            .map(|w| WindowInfo {
                id: w.id.clone(),
                title: String::new(),
                app_name: String::new(),
                pid: w.pid.get(),
                bundle_id: None,
                bounds: w.bounds.map(rect),
                focused: w.state.is_focused,
                minimized: false,
            })
            .collect();
        match topmost_at(&windows, &click.point) {
            Some(top) if top.id == window.id => {}
            Some(top)
                if refused.iter().any(|p| p.get() == top.pid) || self.own.is_own(top.pid, None) =>
            {
                return Err(DesktopError::OwnWindowRefused(
                    "a window of this app or a denied app covers the point".into(),
                ));
            }
            _ => {
                return Err(DesktopError::OwnWindowRefused(
                    "another window covers the point: bring the window forward first".into(),
                ));
            }
        }
        let lease = self.lease(deadline)?;
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
        self.refuse_id(bundle_id)?;
        // An application id, never a path, a command line or an argument.
        if !platform::valid_app_id(self.hooks.os, bundle_id) {
            return Err(DesktopError::InvalidArgs(format!(
                "`{bundle_id}` is not an application id"
            )));
        }
        guard::before_act(self)?;
        let deadline = deadline(LAUNCH_TIMEOUT_MS)?;
        let lease = self.lease(deadline)?;
        let options = ad::system::launch_options::LaunchOptions {
            timeout_ms: LAUNCH_TIMEOUT_MS,
            activate: true,
            ..ad::system::launch_options::LaunchOptions::default()
        };
        let launched = self
            .adapter
            .launch_app(bundle_id, &options, &lease)
            .map_err(map_err)?;
        // The id resolved to Elitea or a denied app after all.
        self.refuse_pid(launched.pid, deadline, "this app")?;
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
                let png = crate::crop::crop(&image.data, x, y, w, h)?;
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
        if let WaitCondition::Window { bundle_id, .. } = condition {
            self.refuse_id(bundle_id)?;
        }
        loop {
            self.check_halt()?;
            guard::before_call(self)?;
            let done = if let WaitCondition::Stable { target } = condition {
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
        self.hooks.marker
    }

    fn forget_snapshots(&self) {
        self.registry().clear();
    }

    fn watch_input(
        &self,
        filter: crate::InputFilter,
        sink: crate::SignalSink,
    ) -> Result<Box<dyn crate::InputWatch>, DesktopError> {
        (self.hooks.watch)(self.hooks.marker, filter, sink, self.guard.start())
    }

    fn set_halt(&self, halt: Option<crate::Halt>) {
        *self
            .halt
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner) = halt;
    }
}

fn tree_options(max_depth: u8) -> ad::TreeOptions {
    ad::TreeOptions {
        max_depth,
        include_bounds: true,
        interactive_only: false,
        compact: false,
        surface: ad::SnapshotSurface::Window,
        skeleton: false,
        force_renderer_accessibility: false,
    }
}

fn vendor_action(action: &Action) -> ad::Action {
    match action {
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
    }
}

fn combo(chord: &KeyChord) -> ad::KeyCombo {
    ad::KeyCombo {
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
    }
}

fn deadline(ms: u64) -> Result<ad::Deadline, DesktopError> {
    ad::Deadline::after(ms).map_err(map_err)
}

/// Converts the vendor tree into raw nodes; the secure-field rule is
/// [`platform::is_secure_field`].
fn to_raw(node: &ad::AccessibilityNode, refmap: &ad::RefMap) -> RawNode<ad::RefEntry> {
    let secure = platform::is_secure_field(&node.role, &node.presentation.states);
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

/// The deepest node in `raw` that carries the `focused` state.
fn focused_node<H>(raw: &RawNode<H>) -> Option<&RawNode<H>> {
    raw.children
        .iter()
        .find_map(focused_node)
        .or_else(|| raw.states.iter().any(|s| s == "focused").then_some(raw))
}

fn rect(r: ad::Rect) -> Rect {
    Rect {
        x: r.x,
        y: r.y,
        width: r.width,
        height: r.height,
    }
}

fn contains_text(node: &Node, text: &str) -> bool {
    node.name.as_deref().is_some_and(|n| n.contains(text))
        || node.value.as_deref().is_some_and(|v| v.contains(text))
        || node.children.iter().any(|c| contains_text(c, text))
}

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
            Modifier::Command => "meta",
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

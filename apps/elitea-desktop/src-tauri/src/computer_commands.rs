//! Computer use for the person (ADR-0047 decisions 6 and 7, #1584): the
//! Settings > Computer use page, the session panel and its events.
//!
//! - `computer_settings_get` / `computer_settings_set`: the device switch,
//!   the organisation's policy, the live TCC grants, what the toolset offers
//!   without Screen Recording, and the remembered "always for this app"
//!   answers; `computer_app_forget` revokes one.
//! - `computer_open_privacy`: the System Settings pane of one grant (a fixed
//!   `x-apple.systempreferences:` URL per grant; nothing the page sends is
//!   put into it).
//! - `computer_session`: the session panel's state, and **Pause**,
//!   **Resume**, **Stop**.
//! - `computer://event` (`computer_session`): every change of the panel's
//!   state, to the main window and the panel.
//!
//! **The session panel** is a small window of its own (label
//! `computer-panel`, the bundled UI at `#computer-panel`): always on top, on
//! every space, not closable, minimizable or focusable on show, without a
//! Dock entry. It shows while a session is active or paused and hides only
//! when the session ends; the person cannot hide it meanwhile (decision 6).

use std::sync::Arc;

use serde::{Deserialize, Serialize};
use serde_json::{Value, json};
use tauri::{AppHandle, Emitter as _, Manager as _, State, WebviewUrl, WebviewWindowBuilder};

use crate::computer_control::{ComputerControl, SessionView};
use crate::local_commands::{IpcError, LocalState};
use crate::settings::SettingsFiles;

/// The session panel window's label (`capabilities/default.json` names its
/// webview).
pub const PANEL: &str = "computer-panel";
/// The event channel.
pub const EVENT_NAME: &str = "computer://event";
/// The panel's size, in points.
const PANEL_SIZE: (f64, f64) = (340.0, 96.0);

/// The System Settings panes of the grants (ADR-0047 decision 7).
const ACCESSIBILITY_PANE: &str =
    "x-apple.systempreferences:com.apple.preference.security?Privacy_Accessibility";
const SCREEN_RECORDING_PANE: &str =
    "x-apple.systempreferences:com.apple.preference.security?Privacy_ScreenCapture";
const INPUT_MONITORING_PANE: &str =
    "x-apple.systempreferences:com.apple.preference.security?Privacy_ListenEvent";

/// The config directory, for the device switch in `settings.json`.
pub struct ComputerConfig(pub std::path::PathBuf);

#[derive(Debug, Serialize)]
pub struct RememberedApp {
    bundle_id: String,
    /// "Always" to see it.
    sees: bool,
    /// "Always" to use it.
    acts: bool,
}

#[derive(Debug, Serialize)]
pub struct ComputerSettings {
    /// The organisation lets computer use run here (`local_work` and
    /// `local_work_computer_use`).
    policy_allows: bool,
    /// The person's device switch (off unless turned on).
    enabled: bool,
    /// The OS can run it at all (macOS).
    supported: bool,
    /// `granted`, `denied`, `unknown` or `not_required`.
    accessibility: elitea_desktop_control::Grant,
    screen_recording: elitea_desktop_control::Grant,
    /// Without Screen Recording these tools are not offered.
    screenshot_tools: Vec<&'static str>,
    stop_shortcut: &'static str,
    remembered: Vec<RememberedApp>,
}

fn setup(state: &LocalState) -> Result<&crate::computer_tools::ComputerSetup, IpcError> {
    state
        .agents
        .computer()
        .ok_or_else(|| IpcError::new("computer_unavailable", "Computer use is not set up."))
}

fn settings_of(state: &LocalState, files: &SettingsFiles) -> Result<ComputerSettings, IpcError> {
    let setup = setup(state)?;
    let policy = state.agents.local_work_policy();
    let grants = setup.host().permissions();
    let enabled = files
        .settings()
        .ok()
        .and_then(|settings| settings.computer_use)
        .unwrap_or(false);
    Ok(ComputerSettings {
        policy_allows: policy.allowed && policy.computer_use,
        enabled,
        supported: cfg!(target_os = "macos"),
        accessibility: grants.accessibility,
        screen_recording: grants.screen_recording,
        screenshot_tools: crate::computer_tools::TOOLS
            .iter()
            .filter(|spec| spec.needs_capture)
            .map(|spec| spec.name)
            .collect(),
        stop_shortcut: elitea_desktop_control::STOP_SHORTCUT,
        remembered: state
            .agents
            .remembered_apps()
            .into_iter()
            .map(|(bundle_id, sees, acts)| RememberedApp {
                bundle_id,
                sees,
                acts,
            })
            .collect(),
    })
}

/// Settings > Computer use: everything the page shows, the grants read live.
#[tauri::command(rename_all = "snake_case")]
pub async fn computer_settings_get(
    state: State<'_, LocalState>,
    config: State<'_, ComputerConfig>,
) -> Result<ComputerSettings, IpcError> {
    let files = SettingsFiles::new(config.0.clone());
    settings_of(&state, &files)
}

/// Turn the device switch on or off. On is refused while the organisation
/// does not allow computer use, or without the Accessibility grant (the
/// required one); off is always allowed and stops the next call of a
/// running turn (`computer_use_off`).
#[tauri::command(rename_all = "snake_case")]
pub async fn computer_settings_set(
    state: State<'_, LocalState>,
    config: State<'_, ComputerConfig>,
    enabled: bool,
) -> Result<ComputerSettings, IpcError> {
    let files = SettingsFiles::new(config.0.clone());
    if enabled {
        let current = settings_of(&state, &files)?;
        if !current.policy_allows {
            return Err(IpcError::new(
                "computer_policy_off",
                "Your organisation does not allow computer use.",
            ));
        }
        if current.accessibility != elitea_desktop_control::Grant::Granted {
            return Err(IpcError::new(
                "computer_accessibility_missing",
                "Grant Accessibility to Elitea first.",
            ));
        }
    }
    let mut settings = files
        .settings()
        .map_err(|e| IpcError::new("storage", e.to_string()))?;
    settings.computer_use = enabled.then_some(true);
    files
        .save_settings(&settings)
        .map_err(|e| IpcError::new("storage", e.to_string()))?;
    settings_of(&state, &files)
}

/// Revoke "always for this app" for `bundle_id`, seeing and acting, in
/// every workspace.
#[tauri::command(rename_all = "snake_case")]
pub async fn computer_app_forget(
    state: State<'_, LocalState>,
    config: State<'_, ComputerConfig>,
    bundle_id: String,
) -> Result<ComputerSettings, IpcError> {
    let bundle_id = bundle_id.trim();
    if bundle_id.is_empty() || bundle_id.len() > 255 {
        return Err(IpcError::new("computer_invalid", "Not a bundle id."));
    }
    state
        .agents
        .forget_remembered_app(bundle_id)
        .map_err(|e| IpcError::new(&e.code, e.message))?;
    let files = SettingsFiles::new(config.0.clone());
    settings_of(&state, &files)
}

#[derive(Clone, Copy, Debug, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum Grant {
    Accessibility,
    ScreenRecording,
    InputMonitoring,
}

/// The System Settings pane where the person grants `grant`.
#[must_use]
pub fn privacy_pane(grant: Grant) -> &'static str {
    match grant {
        Grant::Accessibility => ACCESSIBILITY_PANE,
        Grant::ScreenRecording => SCREEN_RECORDING_PANE,
        Grant::InputMonitoring => INPUT_MONITORING_PANE,
    }
}

/// Open the System Settings pane of one grant.
#[tauri::command(rename_all = "snake_case")]
pub async fn computer_open_privacy(app: AppHandle, grant: Grant) -> Result<(), IpcError> {
    use tauri_plugin_opener::OpenerExt as _;
    if !cfg!(target_os = "macos") {
        return Err(IpcError::new(
            "computer_unsupported",
            "Computer use runs on macOS only.",
        ));
    }
    app.opener()
        .open_url(privacy_pane(grant), None::<&str>)
        .map_err(|e| IpcError::new("os_refused", format!("could not open System Settings: {e}")))
}

#[derive(Clone, Copy, Debug, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum SessionAction {
    Get,
    Pause,
    Resume,
    Stop,
}

/// The session panel: its state, and Pause, Resume, Stop. Answers the
/// state after the action (the change is a `computer://event` too).
#[tauri::command(rename_all = "snake_case")]
pub async fn computer_session(
    state: State<'_, LocalState>,
    action: SessionAction,
) -> Result<SessionView, IpcError> {
    let control = setup(&state)?.host().control();
    apply(&control, action);
    Ok(control.view())
}

/// Do `action` on the session.
pub fn apply(control: &ComputerControl, action: SessionAction) {
    match action {
        SessionAction::Get => {}
        SessionAction::Pause => control.pause_pressed(),
        SessionAction::Resume => control.resume(),
        SessionAction::Stop => control.stop(),
    }
}

/// The session's events and panel: every change reaches the main window and
/// the panel, the panel shows or hides with the session, the menu-bar item
/// mirrors it.
pub fn wire(app: &AppHandle, control: &Arc<ComputerControl>) {
    let handle = app.clone();
    control.set_notify(Arc::new(move |view: &SessionView| {
        let payload = event_payload(view);
        for target in [crate::window::MAIN, PANEL] {
            if let Err(error) = handle.emit_to(target, EVENT_NAME, payload.clone()) {
                log::warn!(target: "computer", "could not deliver a computer event: {error}");
            }
        }
        if let Some(activity) = handle.try_state::<Arc<crate::activity::Activity>>() {
            activity.computer(view);
        }
        let shown = view.shown();
        let app = handle.clone();
        // Windows are made and shown on the main thread.
        let _ = handle.run_on_main_thread(move || show_panel(&app, shown));
    }));
}

/// A `computer://event` of kind `computer_session`.
#[must_use]
pub fn event_payload(view: &SessionView) -> Value {
    let mut payload = serde_json::to_value(view).unwrap_or_else(|_| json!({}));
    payload["kind"] = json!("computer_session");
    payload
}

fn show_panel(app: &AppHandle, shown: bool) {
    let existing = app.get_webview_window(PANEL);
    match (existing, shown) {
        (Some(window), true) => {
            let _ = window.show();
        }
        (Some(window), false) => {
            let _ = window.hide();
        }
        (None, true) => {
            if let Err(error) = build_panel(app) {
                log::warn!(target: "computer", "no session panel: {error}");
            }
        }
        (None, false) => {}
    }
}

fn build_panel(app: &AppHandle) -> tauri::Result<()> {
    let dev = cfg!(debug_assertions);
    let window = WebviewWindowBuilder::new(
        app,
        PANEL,
        WebviewUrl::App("index.html#computer-panel".into()),
    )
    .title("Elitea")
    .inner_size(PANEL_SIZE.0, PANEL_SIZE.1)
    .resizable(false)
    .maximizable(false)
    .minimizable(false)
    .closable(false)
    .always_on_top(true)
    .visible_on_all_workspaces(true)
    .skip_taskbar(true)
    .focused(false)
    .on_navigation(move |url| crate::window::is_bundled_origin(url, dev))
    .build()?;
    // Top right of the main display, out of the way of most windows.
    if let Ok(Some(monitor)) = window.primary_monitor() {
        let scale = monitor.scale_factor();
        let size = monitor.size().to_logical::<f64>(scale);
        let _ = window.set_position(tauri::LogicalPosition::new(
            size.width - PANEL_SIZE.0 - 16.0,
            40.0,
        ));
    }
    // The person cannot close it while the session runs; closing it ends
    // nothing either (the window is kept, hidden only when the session ends).
    window.on_window_event(|event| {
        if let tauri::WindowEvent::CloseRequested { api, .. } = event {
            api.prevent_close();
        }
    });
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn each_grant_opens_its_own_privacy_pane() {
        for (grant, anchor) in [
            (Grant::Accessibility, "Privacy_Accessibility"),
            (Grant::ScreenRecording, "Privacy_ScreenCapture"),
            (Grant::InputMonitoring, "Privacy_ListenEvent"),
        ] {
            let url = privacy_pane(grant);
            assert!(url.starts_with("x-apple.systempreferences:"), "{url}");
            assert!(url.ends_with(anchor), "{url}");
        }
    }

    #[test]
    fn the_panel_buttons_pause_resume_and_stop() {
        use crate::computer_control::Phase;
        let control = ComputerControl::default();
        control.begin(1);
        apply(&control, SessionAction::Get);
        assert_eq!(control.view().phase, Phase::Active);
        apply(&control, SessionAction::Pause);
        assert_eq!(control.view().phase, Phase::Paused);
        apply(&control, SessionAction::Resume);
        assert_eq!(control.view().phase, Phase::Active);
        apply(&control, SessionAction::Stop);
        assert_eq!(control.view().phase, Phase::Stopped);
        let payload = event_payload(&control.view());
        assert_eq!(payload["kind"], "computer_session");
        assert_eq!(payload["phase"], "stopped");
    }
}

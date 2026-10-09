fn main() {
    // The host commands are an explicit allowlist: with an app manifest, Tauri
    // generates one `allow-<command>` permission per command and refuses any
    // command a capability does not name (capabilities/default.json).
    tauri_build::try_build(tauri_build::Attributes::new().app_manifest(
        tauri_build::AppManifest::new().commands(&[
            "host_state",
            "host_connect",
            "host_sign_in",
            "host_sign_in_cancel",
            "host_access_token",
            "host_refresh",
            "host_sign_out",
            "host_wipe",
            "workspace_open",
            "workspace_list",
            "workspace_remove",
            "workspace_bind_project",
            "workspace_files",
            "agent_turn_start",
            "agent_turn_cancel",
            "agent_turn_status",
            "approval_respond",
            "turn_changes",
            "thread_history",
            "thread_history_delete",
            "checkpoint_restore",
            "checkpoint_preview",
            "reveal_path",
            "open_path",
            "app_platform",
            "app_ready",
            "doctor_run",
            "doctor_fix",
        ]),
    ))
    .expect("tauri build configuration is valid");
}

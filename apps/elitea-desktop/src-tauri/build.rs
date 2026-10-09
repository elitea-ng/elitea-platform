fn main() {
    // The host commands are an explicit allowlist: with an app manifest, Tauri
    // generates one `allow-<command>` permission per command and refuses any
    // command a capability does not name (capabilities/default.json).
    tauri_build::try_build(tauri_build::Attributes::new().app_manifest(
        tauri_build::AppManifest::new().commands(&[
            "host_state",
            "host_connect",
            "host_sign_in",
            "host_access_token",
            "host_refresh",
            "host_sign_out",
            "host_wipe",
        ]),
    ))
    .expect("tauri build configuration is valid");
}

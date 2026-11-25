// Prevents an extra console window on Windows in release builds.
#![cfg_attr(not(debug_assertions), windows_subsystem = "windows")]

fn main() {
    match std::env::args_os().nth(1) {
        // An editor starts the agent as `elitea-desktop --acp` and speaks ACP on
        // its stdio (src/acp); the app itself never starts in that mode.
        Some(arg) if arg == "--acp" => std::process::exit(elitea_desktop_lib::run_acp()),
        // The build's version (the release stamps it into Cargo.toml), then exit
        // before any window, tray or single-instance hand-off. A package's smoke
        // test reads it (desktop-package.yml, the Linux AppImage).
        Some(arg) if arg == "--version" => {
            println!("elitea-desktop {}", env!("CARGO_PKG_VERSION"));
        }
        _ => elitea_desktop_lib::run(),
    }
}

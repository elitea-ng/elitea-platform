// Prevents an extra console window on Windows in release builds.
#![cfg_attr(not(debug_assertions), windows_subsystem = "windows")]

fn main() {
    // An editor starts the agent as `elitea-desktop --acp` and speaks ACP on
    // its stdio (src/acp); the app itself never starts in that mode.
    if std::env::args_os().nth(1).is_some_and(|arg| arg == "--acp") {
        std::process::exit(elitea_desktop_lib::run_acp());
    }
    elitea_desktop_lib::run();
}

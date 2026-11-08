//! `elitea-browser-session idle|serve` (see the library docs and
//! `libs/proto/elitea/runtime/v1/browser_session_agent_v1.md`).
//!
//! * `idle`: the container's entrypoint. Starts nothing and waits for a
//!   signal.
//! * `serve`: run by the supervisor as an exec; the line protocol on stdin
//!   and stdout until `destroy` or the end of stdin.
//!
//! Flags (`serve`): `--engine <path>` (default
//! `/opt/chrome-headless-shell/chrome-headless-shell`), `--moli <path>`
//! (default `/opt/moli/moli` when that file exists: the `lite` image ships
//! it, the `full` image does not), `--engine-log` (the engine's stderr to
//! ours; off by default, because it can name URLs). Exit status 0 on a clean
//! end, 2 on bad arguments, 1 when the streams failed.

#[cfg(unix)]
use std::path::PathBuf;
use std::process::ExitCode;
#[cfg(unix)]
use std::sync::Arc;

#[cfg(unix)]
use elitea_browser_session::engine::{DEFAULT_ENGINE, DEFAULT_MOLI};
#[cfg(unix)]
use elitea_browser_session::protocol::{self, log};
#[cfg(unix)]
use elitea_browser_session::session::{Engines, Session};

#[cfg(unix)]
const USAGE: &str =
    "usage: elitea-browser-session idle | serve [--engine <path>] [--moli <path>] [--engine-log]";

/// The agent runs in a Linux pod; off unix there is no engine pipe.
#[cfg(not(unix))]
fn main() -> ExitCode {
    eprintln!("elitea-browser-session runs on unix only");
    ExitCode::from(2)
}

#[cfg(unix)]
#[tokio::main(flavor = "current_thread")]
async fn main() -> ExitCode {
    let mut words = std::env::args().skip(1);
    let command = words.next();
    let mut engine = PathBuf::from(DEFAULT_ENGINE);
    let mut moli = Some(PathBuf::from(DEFAULT_MOLI)).filter(|path| path.is_file());
    let mut engine_log = false;
    while let Some(flag) = words.next() {
        match flag.as_str() {
            "--engine-log" => engine_log = true,
            "--engine" | "--moli" => {
                let Some(path) = words.next() else {
                    eprintln!("{USAGE}");
                    return ExitCode::from(2);
                };
                if flag == "--engine" {
                    engine = PathBuf::from(path);
                } else {
                    moli = Some(PathBuf::from(path));
                }
            }
            _ => {
                eprintln!("{USAGE}");
                return ExitCode::from(2);
            }
        }
    }
    match command.as_deref() {
        Some("idle") => match protocol::idle().await {
            Ok(()) => ExitCode::SUCCESS,
            Err(_) => ExitCode::from(1),
        },
        Some("serve") => {
            let engines = Engines {
                chrome: engine,
                moli,
            };
            let session = Arc::new(Session::with_engines(engines, engine_log));
            let input = tokio::io::BufReader::new(tokio::io::stdin());
            if protocol::serve(session, input, tokio::io::stdout())
                .await
                .is_ok()
            {
                ExitCode::SUCCESS
            } else {
                log("output_failed");
                ExitCode::from(1)
            }
        }
        _ => {
            eprintln!("{USAGE}");
            ExitCode::from(2)
        }
    }
}

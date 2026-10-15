//! `elitea-workspace-runner unpack|serve|pack` (see the library docs).
//!
//! Flags: `--root <dir>` (default `/workspace`), `--data-dir <dir>` (default
//! `/tmp/elitea-runner`), `--policy <json>` (serve: `RunnerPolicy`),
//! `--max-files <n>`, `--max-file-bytes <n>`, `--max-total-bytes <n>`
//! (unpack and pack). Exit status 0 on success, 2 on bad arguments, 1 on a
//! refused or failed operation (its code on stderr).

use std::path::PathBuf;
use std::process::ExitCode;

use elitea_agent_runtime::workspace_archive::ArchiveLimits;
use elitea_workspace_runner::{RunnerConfig, RunnerPolicy, open_session, pack, serve, unpack};

struct Args {
    command: String,
    root: PathBuf,
    data_dir: PathBuf,
    policy: RunnerPolicy,
    limits: ArchiveLimits,
}

fn parse() -> Option<Args> {
    let mut words = std::env::args().skip(1);
    let command = words.next()?;
    let mut args = Args {
        command,
        root: PathBuf::from("/workspace"),
        data_dir: PathBuf::from("/tmp/elitea-runner"),
        policy: RunnerPolicy::default(),
        limits: ArchiveLimits {
            max_files: 20_000,
            max_file_bytes: 25 << 20,
            max_total_bytes: 8 << 30,
        },
    };
    while let Some(flag) = words.next() {
        let value = words.next()?;
        match flag.as_str() {
            "--root" => args.root = PathBuf::from(value),
            "--data-dir" => args.data_dir = PathBuf::from(value),
            "--policy" => args.policy = serde_json::from_str(&value).ok()?,
            "--max-files" => args.limits.max_files = value.parse().ok()?,
            "--max-file-bytes" => args.limits.max_file_bytes = value.parse().ok()?,
            "--max-total-bytes" => args.limits.max_total_bytes = value.parse().ok()?,
            _ => return None,
        }
    }
    Some(args)
}

fn fail(code: &str) -> ExitCode {
    eprintln!("elitea-workspace-runner: {code}");
    ExitCode::from(1)
}

#[tokio::main(flavor = "current_thread")]
async fn main() -> ExitCode {
    let Some(args) = parse() else {
        eprintln!("usage: elitea-workspace-runner unpack|serve|pack [--root <dir>] [--data-dir <dir>] [--policy <json>] [--max-files <n>] [--max-file-bytes <n>] [--max-total-bytes <n>]");
        return ExitCode::from(2);
    };
    match args.command.as_str() {
        "unpack" => match unpack(&mut std::io::stdin().lock(), &args.root, args.limits) {
            Ok(_) => ExitCode::SUCCESS,
            Err(code) => fail(&code),
        },
        "pack" => match pack(&args.root, &mut std::io::stdout().lock(), args.limits) {
            Ok(_) => ExitCode::SUCCESS,
            Err(code) => fail(&code),
        },
        "serve" => {
            let session = match open_session(&RunnerConfig {
                root: args.root,
                data_dir: args.data_dir,
                policy: args.policy,
            }) {
                Ok(session) => session,
                Err(_) => return fail("runner.workspace_unavailable"),
            };
            let input = tokio::io::BufReader::new(tokio::io::stdin());
            match serve(session, input, tokio::io::stdout()).await {
                Ok(()) => ExitCode::SUCCESS,
                Err(_) => fail("runner.stream_failed"),
            }
        }
        _ => ExitCode::from(2),
    }
}

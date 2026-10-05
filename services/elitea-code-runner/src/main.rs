//! Runs only inside the admitted sandbox container, as its main process.
//! Outer CPU/memory/PID/network/filesystem isolation remains mandatory.
#![forbid(unsafe_code)]

mod compiled_code;
mod compiled_lifecycle;
mod lifecycle;
mod native_finalization;
mod workspace_content;
mod workspace_lifecycle;

use serde::{Deserialize, Serialize};
use std::io::{Read, Write};
use std::process::Stdio;
use std::time::Duration;
use tokio::io::AsyncReadExt;
use tokio::process::Command;

const CAPTURE_LIMIT: usize = 512 * 1024;
const REQUEST_LIMIT: u64 = 64 * 1024;

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct Request {
    argv: Vec<String>,
    timeout_seconds: u64,
}

#[derive(Serialize)]
struct Receipt {
    revision: u8,
    status: &'static str,
    exit_code: Option<i32>,
    stdout: String,
    stderr: String,
}

fn validate(request: &Request) -> bool {
    // These switches are immutable supervisor launch configuration ONLY after
    // verified role-specific authority; callers cannot request/set them.
    let snapshot_role = if std::env::var_os("ELITEA_COMPILED_CODE_JOB_PURPOSE").is_some() {
        match compiled_code::enabled_purpose() {
            Ok(compiled_code::Purpose::Compile) => {
                request.argv == ["/usr/local/bin/elitea-code-rust", "--compile-snapshot"]
            }
            Ok(compiled_code::Purpose::Execute) => {
                request.argv == ["/usr/local/bin/elitea-code-rust", "--execute-snapshot"]
            }
            Err(_) => false,
        }
    } else {
        true
    };
    snapshot_role
        && !request.argv.is_empty()
        && request.argv.len() <= 64
        && request
            .argv
            .iter()
            .all(|arg| arg.len() <= 4096 && !arg.contains('\0'))
        && !request.argv[0].is_empty()
        && (1..=3600).contains(&request.timeout_seconds)
}

async fn execute(request: Request) -> Receipt {
    let mut receipt = Receipt {
        revision: 1,
        status: "launch_failed",
        exit_code: None,
        stdout: String::new(),
        stderr: String::new(),
    };
    if !validate(&request) {
        receipt.status = "invalid_request";
        return receipt;
    }
    let mut child = match Command::new(&request.argv[0])
        .args(&request.argv[1..])
        .stdin(Stdio::null())
        .stdout(Stdio::piped())
        .stderr(Stdio::piped())
        .kill_on_drop(true)
        .spawn()
    {
        Ok(child) => child,
        Err(_) => return receipt,
    };
    let (Some(mut stdout), Some(mut stderr)) = (child.stdout.take(), child.stderr.take()) else {
        let _ = child.kill().await;
        receipt.status = "capture_failed";
        return receipt;
    };
    let deadline = tokio::time::sleep(Duration::from_secs(request.timeout_seconds));
    tokio::pin!(deadline);
    let mut out = Vec::new();
    let mut err = Vec::new();
    let mut out_open = true;
    let mut err_open = true;
    let mut exit = None;
    let mut ob = [0u8; 8192];
    let mut eb = [0u8; 8192];
    receipt.status = "capture_failed";
    loop {
        if !out_open && !err_open && exit.is_some() {
            receipt.status = if exit.as_ref().is_some_and(std::process::ExitStatus::success) {
                "completed"
            } else {
                "failed"
            };
            break;
        }
        tokio::select! {
            biased;
            () = &mut deadline => { receipt.status = "timeout"; break; }
            status = child.wait(), if exit.is_none() => {
                match status { Ok(status) => exit = Some(status), Err(_) => break }
            }
            chunk = stdout.read(&mut ob), if out_open => {
                match chunk {
                    Ok(0) => out_open = false,
                    Ok(n) => {
                        if out.len() + err.len() + n > CAPTURE_LIMIT {
                            receipt.status = "output_limit"; break;
                        }
                        out.extend_from_slice(&ob[..n]);
                    }
                    Err(_) => break,
                }
            }
            chunk = stderr.read(&mut eb), if err_open => {
                match chunk {
                    Ok(0) => err_open = false,
                    Ok(n) => {
                        if out.len() + err.len() + n > CAPTURE_LIMIT {
                            receipt.status = "output_limit"; break;
                        }
                        err.extend_from_slice(&eb[..n]);
                    }
                    Err(_) => break,
                }
            }
        }
    }
    if exit.is_none() {
        let _ = tokio::time::timeout(Duration::from_secs(2), child.kill()).await;
    }
    receipt.exit_code = exit.and_then(|status| status.code());
    receipt.stdout = String::from_utf8_lossy(&out).into_owned();
    receipt.stderr = String::from_utf8_lossy(&err).into_owned();
    receipt
}

#[tokio::main(flavor = "current_thread")]
async fn main() -> Result<(), Box<dyn std::error::Error>> {
    if let Some(command) = std::env::args().nth(1) {
        lifecycle::run(&command).await?;
        return Ok(());
    }
    // The supervisor writes this file before signaling container dispatch.
    // It must never place credentials or host runtime endpoints in it.
    let mut bytes = Vec::new();
    std::fs::File::open("/workspace/.elitea-job.json")?
        .take(REQUEST_LIMIT + 1)
        .read_to_end(&mut bytes)?;
    let receipt = if bytes.len() > REQUEST_LIMIT as usize {
        Receipt {
            revision: 1,
            status: "invalid_request",
            exit_code: None,
            stdout: String::new(),
            stderr: String::new(),
        }
    } else {
        match serde_json::from_slice::<Request>(&bytes) {
            Ok(request) => execute(request).await,
            Err(_) => Receipt {
                revision: 1,
                status: "invalid_request",
                exit_code: None,
                stdout: String::new(),
                stderr: String::new(),
            },
        }
    };
    // One envelope in container logs; child stdout/stderr never bypass this cap
    // through inherited descriptors. Treat all captured content as untrusted.
    let encoded = serde_json::to_vec(&receipt)?;
    let mut output = std::io::stdout().lock();
    output.write_all(&encoded)?;
    output.write_all(b"\n")?;
    output.flush()?;
    // Container main-process exit terminates remaining descendants in its PID namespace.
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    fn command(code: &str, timeout_seconds: u64) -> Request {
        Request {
            argv: vec!["/bin/sh".into(), "-c".into(), code.into()],
            timeout_seconds,
        }
    }

    #[tokio::test]
    async fn receipt_preserves_output_and_nonzero_status() {
        let receipt = execute(command("printf result; printf diagnostic >&2; exit 7", 5)).await;
        assert_eq!(receipt.status, "failed");
        assert_eq!(receipt.exit_code, Some(7));
        assert_eq!(receipt.stdout, "result");
        assert_eq!(receipt.stderr, "diagnostic");
    }

    #[tokio::test]
    async fn output_and_time_are_bounded() {
        let receipt = execute(command("exec yes x", 5)).await;
        assert_eq!(receipt.status, "output_limit");
        assert!(receipt.stdout.len() <= CAPTURE_LIMIT);
        let receipt = execute(command("exec sleep 20", 1)).await;
        assert_eq!(receipt.status, "timeout");
    }

    #[tokio::test]
    async fn invalid_request_never_launches() {
        let receipt = execute(command("exit 0", 0)).await;
        assert_eq!(receipt.status, "invalid_request");
        assert_eq!(receipt.exit_code, None);
    }
}

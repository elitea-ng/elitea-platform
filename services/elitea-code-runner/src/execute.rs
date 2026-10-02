//! Fixed image adapter entrypoint, launched under the bounded PID-1 runner.
#![forbid(unsafe_code)]
use std::{io::Read, process::Command};

const REQUEST: &str = "/workspace/.elitea-code.json";
const LIMIT: u64 = 1024 * 1024;

fn adapter(language: &str) -> Result<&'static str, &'static str> {
    match language {
        "python" => Ok("/opt/elitea-code/python.mjs"),
        "javascript" | "typescript" => Ok("/opt/elitea-code/javascript.mjs"),
        _ => Err("This runtime image does not support the requested Code language"),
    }
}

fn command(language: &str) -> Result<Command, &'static str> {
    if language == "rust" {
        let mut command = Command::new("/usr/local/bin/elitea-code-rust");
        command.env_clear();
        return Ok(command);
    }
    let mut command = Command::new("/usr/local/bin/deno");
    command
        .env_clear()
        .env("DENO_DIR", "/opt/deno-cache")
        .env("HOME", "/workspace");
    command.args([
        "run",
        "--no-prompt",
        "--cached-only",
        "--no-config",
        "--node-modules-dir=none",
        "--frozen",
        "--lock=/opt/elitea-code/deno.lock",
        "--allow-read=/opt/elitea-code,/opt/deno-cache,/workspace",
        "--allow-write=/workspace",
        "--allow-env=NODE_DEBUG",
        "--deny-net",
        "--deny-run",
        "--deny-ffi",
        adapter(language)?,
        REQUEST,
        if language == "python" {
            "/workspace/wheels"
        } else {
            "/workspace"
        },
    ]);
    Ok(command)
}

fn uses_dependency_bundle(request: &serde_json::Value) -> Result<bool, &'static str> {
    match (
        request.get("revision").and_then(serde_json::Value::as_u64),
        request.get("dependency_bundle_sha256"),
    ) {
        (Some(1), None) => Ok(false),
        (Some(2), Some(serde_json::Value::String(digest)))
            if request.get("language").and_then(serde_json::Value::as_str) == Some("python")
                && digest.len() == 64
                && digest
                    .bytes()
                    .all(|b| b.is_ascii_digit() || (b'a'..=b'f').contains(&b)) =>
        {
            Ok(true)
        }
        _ => Err("Code request has an invalid revision or Python dependency identity"),
    }
}

fn prepare() -> Result<Command, Box<dyn std::error::Error>> {
    if std::env::args().skip(1).collect::<Vec<_>>() != [REQUEST] {
        return Err("Code launcher accepts only the fixed prepared request path".into());
    }
    let mut bytes = Vec::new();
    std::fs::File::open(REQUEST)?
        .take(LIMIT + 1)
        .read_to_end(&mut bytes)?;
    if bytes.len() as u64 > LIMIT {
        return Err("Code request exceeds its size limit".into());
    }
    let request: serde_json::Value = serde_json::from_slice(&bytes)?;
    let prepared_dependencies = uses_dependency_bundle(&request)?;
    let language = request
        .get("language")
        .and_then(serde_json::Value::as_str)
        .ok_or("Code request has no language")?;
    let command = command(language)?;
    if language == "python" && !prepared_dependencies {
        std::fs::create_dir_all("/workspace/wheels")?;
        // Image-owned assets only; never reuse another job's writable cache.
        for entry in std::fs::read_dir("/opt/elitea-wheels")? {
            let entry = entry?;
            if !entry.file_type()?.is_file() {
                return Err("Unexpected entry in immutable Python wheel cache".into());
            }
            std::fs::copy(
                entry.path(),
                std::path::Path::new("/workspace/wheels").join(entry.file_name()),
            )?;
        }
    }
    Ok(command)
}

fn main() {
    let mut command = match prepare() {
        Ok(command) => command,
        Err(error) => {
            eprintln!("Code runtime launch failed: {error}");
            std::process::exit(2);
        }
    };
    // Replacement preserves the runner's child process identity and deadline.
    #[cfg(unix)]
    {
        use std::os::unix::process::CommandExt;
        let error = command.exec();
        eprintln!("Code runtime could not start: {error}");
        std::process::exit(2);
    }
    #[cfg(not(unix))]
    {
        let _ = &mut command;
        eprintln!("Code runtime images require a Unix container");
        std::process::exit(2);
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn language_selects_only_fixed_adapters() {
        assert_eq!(adapter("python").unwrap(), "/opt/elitea-code/python.mjs");
        assert_eq!(
            adapter("typescript").unwrap(),
            adapter("javascript").unwrap()
        );
        assert!(adapter("../../bin/sh").is_err());
        assert!(adapter("rust").is_err());
        assert_eq!(
            command("rust").unwrap().get_program(),
            "/usr/local/bin/elitea-code-rust"
        );
    }
    #[test]
    fn launch_disables_network_and_subprocesses_and_uses_fixed_request() {
        let command = command("python").unwrap();
        let args: Vec<_> = command.get_args().map(|a| a.to_str().unwrap()).collect();
        for required in [
            "--deny-net",
            "--deny-run",
            "--deny-ffi",
            "--cached-only",
            "--no-config",
            "--node-modules-dir=none",
            "--frozen",
            REQUEST,
        ] {
            assert!(args.contains(&required));
        }
        assert_eq!(command.get_envs().count(), 2);
    }

    #[test]
    fn dependency_identity_cannot_downgrade_or_select_another_language() {
        let root = "a".repeat(64);
        assert!(
            !uses_dependency_bundle(&serde_json::json!({"revision":1,"language":"python"}))
                .unwrap()
        );
        assert!(uses_dependency_bundle(&serde_json::json!({"revision":2,"language":"python","dependency_bundle_sha256":root})).unwrap());
        for request in [
            serde_json::json!({"revision":1,"language":"python","dependency_bundle_sha256":root}),
            serde_json::json!({"revision":2,"language":"rust","dependency_bundle_sha256":root}),
            serde_json::json!({"revision":2,"language":"python"}),
            serde_json::json!({"revision":2,"language":"python","dependency_bundle_sha256":root.to_uppercase()}),
            serde_json::json!({"revision":2,"language":"python","dependency_bundle_sha256":format!("sha256:{root}")}),
            serde_json::json!({"revision":1,"language":"python","dependency_bundle_sha256":null}),
        ] {
            assert!(uses_dependency_bundle(&request).is_err());
        }
    }
}

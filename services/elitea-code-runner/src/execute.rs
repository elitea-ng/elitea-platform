//! Fixed image adapter entrypoint, launched under the bounded PID-1 runner.
#![forbid(unsafe_code)]
use std::process::Command;
mod code_platform_bridge;
mod code_platform_helper;
mod code_platform_launch;
mod code_platform_mailbox;
mod code_platform_signature;
mod code_prepared_extensions;
mod workspace_content;
// PID-1 owns hydration; this binary only verifies the retained workspace entry.
#[allow(dead_code)]
mod workspace_lifecycle;

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

fn base_revision(request: &serde_json::Value) -> Result<Option<u64>, &'static str> {
    code_prepared_extensions::base_revision(request).map(Some)
}

fn uses_dependency_bundle(request: &serde_json::Value) -> Result<bool, &'static str> {
    match (
        base_revision(request)?,
        request.get("dependency_bundle_sha256"),
    ) {
        (Some(1), None) if request.get("native_dependencies").is_none() => Ok(false),
        (Some(3), Some(serde_json::Value::String(digest)))
            if digest.len() == 64
                && digest
                    .bytes()
                    .all(|b| b.is_ascii_digit() || (b'a'..=b'f').contains(&b))
                && request.get("native_dependencies").is_some_and(|n| {
                    matches!(
                        (request["language"].as_str(), n["kind"].as_str()),
                        (Some("javascript" | "typescript"), Some("deno"))
                            | (Some("rust"), Some("cargo"))
                    )
                }) =>
        {
            Ok(true)
        }
        (Some(2), Some(serde_json::Value::String(digest)))
            if request.get("language").and_then(serde_json::Value::as_str) == Some("python")
                && request.get("native_dependencies").is_none()
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

fn prepare()
-> Result<(Command, Option<code_platform_bridge::LaunchBinding>), Box<dyn std::error::Error>> {
    if std::env::args().skip(1).collect::<Vec<_>>() != [REQUEST] {
        return Err("Code launcher accepts only the fixed prepared request path".into());
    }
    let bytes = code_platform_launch::read_fixed(REQUEST, LIMIT as usize)?;
    let request: serde_json::Value = serde_json::from_slice(&bytes)?;
    let prepared_dependencies = uses_dependency_bundle(&request)?;
    if let Some(binding) = request.get("workspace") {
        workspace_lifecycle::verify_workspace_entry(binding)?;
    }
    let language = request
        .get("language")
        .and_then(serde_json::Value::as_str)
        .ok_or("Code request has no language")?;
    let mut command = command(language)?;
    if request.get("native_dependencies").is_some()
        && matches!(language, "javascript" | "typescript")
    {
        let base = std::path::Path::new("/workspace/native-bundle");
        let ready = base.join("elitea-native-ready-v2.json");
        let stat = std::fs::symlink_metadata(&ready)?;
        if !stat.is_file() || stat.len() > 128 * 1024 {
            return Err("Native ready proof is missing".into());
        }
        // The adapter verifies the exact ready record and all file hashes before source execution.
        command = Command::new("/usr/local/bin/deno");
        command
            .env_clear()
            .env("DENO_DIR", "/workspace/native-bundle/cache/deno-cache")
            .env("HOME", "/workspace")
            .args([
                "run",
                "--no-prompt",
                "--cached-only",
                "--no-config",
                "--node-modules-dir=none",
                "--frozen",
                "--lock=/workspace/native-bundle/cache/elitea-javascript-lock.json",
                "--allow-import=jsr.io,registry.npmjs.org",
                "--allow-read=/opt/elitea-code,/workspace",
                "--allow-write=/workspace",
                "--allow-env=NODE_DEBUG",
                "--deny-net",
                "--deny-run",
                "--deny-ffi",
                adapter(language)?,
                REQUEST,
                "/workspace",
            ]);
    }
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
    let binding = if request["revision"] == 5 && language != "rust" {
        Some(code_platform_launch::capture(
            &bytes,
            &request,
            std::time::Duration::from_secs(
                request["timeout_seconds"]
                    .as_u64()
                    .ok_or("Code timeout is invalid")?,
            ),
            None,
        )?)
    } else {
        None
    };
    Ok((command, binding))
}

fn main() {
    if let Some(command) = std::env::args().nth(1)
        && matches!(
            command.as_str(),
            "--platform-bind" | "--platform-read" | "--platform-reply"
        )
    {
        if std::env::args().count() != 2 || code_platform_helper::run(&command).is_err() {
            eprintln!("Code platform helper requires reconciliation");
            std::process::exit(2);
        }
        return;
    }
    let (mut command, binding) = match prepare() {
        Ok(value) => value,
        Err(error) => {
            eprintln!("Code runtime launch failed: {error}");
            std::process::exit(2);
        }
    };
    if let Some(binding) = binding {
        let runtime = tokio::runtime::Builder::new_current_thread()
            .enable_all()
            .build()
            .unwrap_or_else(|_| std::process::exit(2));
        if runtime
            .block_on(code_platform_bridge::run(
                tokio::process::Command::from(command),
                binding,
            ))
            .is_err()
        {
            eprintln!("Code platform retained process requires reconciliation");
            std::process::exit(2);
        }
        return;
    }
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

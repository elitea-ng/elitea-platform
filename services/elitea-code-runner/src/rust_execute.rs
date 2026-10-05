//! Compile and execute only inside the admitted Rust runtime container.
#![forbid(unsafe_code)]
mod code_platform_bridge;
mod code_platform_launch;
mod code_platform_mailbox;
mod code_platform_signature;
mod code_prepared_extensions;
mod compiled_code;
mod compiled_compiler;
mod compiled_snapshot;
#[cfg(any(test, target_os = "linux"))]
mod native_finalization;
mod rust_native;
mod rust_profile;
mod workspace_content;
// PID-1 owns hydration; this binary only verifies the retained workspace entry.
#[allow(dead_code)]
mod workspace_lifecycle;
use std::{
    io::Read,
    path::Path,
    process::{Command, Stdio},
};
const ROOT: &str = "/workspace/rust-job";

fn run(mut command: Command) -> Result<(), Box<dyn std::error::Error>> {
    let mut child = command
        .stdout(Stdio::piped())
        .stderr(Stdio::inherit())
        .stdin(Stdio::null())
        .spawn()?;
    // Stream compiler/program diagnostics to the parent runner, without buffering.
    let captured = match child.stdout.take() {
        Some(mut output) => std::io::copy(&mut output, &mut std::io::stderr()),
        None => Err(std::io::Error::other("Code output pipe unavailable")),
    };
    if let Err(error) = captured {
        let _ = child.kill();
        let _ = child.wait();
        return Err(error.into());
    }
    if !child.wait()?.success() {
        return Err("Rust compilation or execution failed; see diagnostics".into());
    }
    Ok(())
}

fn execute(mode: Option<compiled_code::Purpose>) -> Result<(), Box<dyn std::error::Error>> {
    let workspace = Path::new("/workspace");
    let control = mode
        .map(|purpose| {
            compiled_code::load_control(workspace, purpose)
                .map_err(|_| std::io::Error::other("Rust snapshot validation failed: control"))
        })
        .transpose()?;
    let started = std::time::Instant::now();
    let mut bytes = code_platform_launch::read_fixed("/workspace/.elitea-code.json", 1024 * 1024)?;
    if bytes.len() > 1024 * 1024 {
        return Err("Code request exceeds its limit".into());
    }
    let request: serde_json::Value = serde_json::from_slice(&bytes)?;
    let base_revision = code_prepared_extensions::base_revision(&request)?;
    let broker = request.get("platform_client").is_some();
    let execution_policy = request["policy_revision"]
        .as_str()
        .ok_or("Rust execution policy is missing")?;
    let profile = rust_profile::Profile::for_execution(execution_policy, broker)
        .map_err(|_| "Rust image profile does not match its capability")?;
    if let Some(binding) = request.get("workspace") {
        workspace_lifecycle::verify_workspace_entry(binding)?;
    }
    if !matches!(base_revision, 1 | 3)
        || request["language"] != "rust"
        || !request["input"].is_object()
    {
        return Err("Invalid Rust Code request".into());
    }
    let source = request["source"].as_str().ok_or("Rust source is missing")?;
    if source.len() > 256 * 1024 {
        return Err("Rust source exceeds its limit".into());
    }
    let native = if base_revision == 3 {
        Some(
            rust_native::NativeExecution::from_bytes(&bytes)
                .map_err(|_| "Invalid supplied Cargo request")?,
        )
    } else {
        if request.get("dependency_bundle_sha256").is_some()
            || request.get("native_dependencies").is_some()
        {
            return Err("Static Rust request cannot carry supplied dependency identity".into());
        }
        None
    };
    if let Some(native) = &native
        && native
            .profile()
            .map_err(|_| "Supplied Rust profile is invalid")?
            != profile
    {
        return Err("Supplied Rust profile does not match its capability".into());
    }
    // Capture parent-only trust and actual retained-runtime binding before
    // compiler/build-script/user execution. It never enters source or argv.
    let launch = if broker && !matches!(mode, Some(compiled_code::Purpose::Compile)) {
        let seconds = request["timeout_seconds"]
            .as_u64()
            .filter(|v| (1..=3600).contains(v))
            .ok_or("Invalid Rust Code timeout")?;
        Some(code_platform_launch::capture(
            &bytes,
            &request,
            std::time::Duration::from_secs(seconds),
            control
                .as_ref()
                .map(original_compiled_request_digest)
                .transpose()?,
        )?)
    } else {
        None
    };
    let deadline = native.as_ref().map(rust_native::NativeExecution::deadline);
    let (compile_root, target, retained) = if let Some(native) = &native {
        let (root, record, target) = rust_native::execution_profile(
            Path::new("/workspace"),
            native,
            false,
            deadline.ok_or("Missing native deadline")?,
        )
        .map_err(|_| "Supplied Cargo profile is missing or changed")?;
        (root, Some(target), Some(record))
    } else {
        std::fs::create_dir(ROOT)?;
        std::fs::create_dir(format!("{ROOT}/src"))?;
        std::fs::create_dir(format!("{ROOT}/.cargo"))?;
        if broker {
            install_static_broker_profile(Path::new(ROOT))?;
        } else {
            for file in [
                "Cargo.toml",
                "Cargo.lock",
                "src/main.rs",
                ".cargo/config.toml",
            ] {
                std::fs::copy(
                    Path::new("/opt/elitea-rust").join(file),
                    Path::new(ROOT).join(file),
                )?;
            }
        }
        (std::path::PathBuf::from(ROOT), None, None)
    };
    std::fs::create_dir(format!("{ROOT}/tmp"))?;
    // Every invocation gets a fresh Cargo home. Source never supplies Cargo flags or environment.
    std::fs::create_dir(format!("{ROOT}/cargo-home"))?;
    std::fs::write(compile_root.join("src/user.rs"), source)?;
    std::fs::write(
        format!("{ROOT}/input.json"),
        serde_json::to_vec(&request["input"])?,
    )?;
    let mut compile = Command::new("/usr/local/cargo/bin/cargo");
    compile
        .current_dir(&compile_root)
        .env_clear()
        .env("PATH", "/usr/local/cargo/bin:/usr/local/bin:/usr/bin:/bin")
        .env("RUSTUP_HOME", "/usr/local/rustup")
        .env("CARGO_HOME", format!("{ROOT}/cargo-home"))
        .env("CARGO_INCREMENTAL", "0")
        .env("CARGO_TERM_COLOR", "never")
        .env("HOME", "/workspace")
        .env("TMPDIR", format!("{ROOT}/tmp"))
        .args(["build", "--locked", "--offline", "-j", "2"]);
    if let Some(target) = &target {
        compile.args(["--target", target]);
    }
    let snapshot_deadline = if mode.is_some() {
        let seconds = request["timeout_seconds"]
            .as_u64()
            .filter(|seconds| (1..=3600).contains(seconds))
            .ok_or("Invalid snapshot timeout")?;
        Some(started + std::time::Duration::from_secs(seconds))
    } else {
        None
    };
    if let Some(control) = &control {
        compiled_snapshot::verify_binding(
            control,
            &bytes,
            source,
            &request,
            &compile_root,
            target.as_deref(),
        )?;
    }
    match mode {
        None => run(compile)?,
        Some(compiled_code::Purpose::Compile) => {
            // Reap compiler and escaped build-script descendants before capture.
            let runtime = tokio::runtime::Builder::new_current_thread()
                .enable_all()
                .build()?;
            runtime.block_on(compiled_compiler::run(
                compile,
                workspace,
                snapshot_deadline.ok_or("Missing compiler deadline")?,
            ))?;
        }
        Some(compiled_code::Purpose::Execute) => {}
    }
    if let (Some(record), Some(deadline)) = (&retained, deadline) {
        rust_profile::rust_prepare_archive::verify_tree(
            &compile_root,
            &record.content,
            true,
            deadline,
        )
        .map_err(|_| "Compilation changed supplied dependency content")?;
    }
    let program_path = if let Some(target) = &target {
        compile_root
            .join("target")
            .join(target)
            .join("debug/elitea-code-job")
    } else {
        compile_root.join("target/debug/elitea-code-job")
    };
    if let (Some(compiled_code::Purpose::Compile), Some(control)) = (mode, &control) {
        compiled_code::load_control(workspace, compiled_code::Purpose::Compile)?;
        compiled_snapshot::verify_binding(
            control,
            &bytes,
            source,
            &request,
            &compile_root,
            target.as_deref(),
        )?;
        let descriptor = compiled_snapshot::capture(workspace, &program_path, control)?;
        compiled_snapshot::await_release(
            workspace,
            snapshot_deadline.ok_or("Missing publication deadline")?,
        )?;
        compiled_code::load_descriptor(workspace, control)?;
        println!(
            "{}",
            serde_json::json!({"revision":1,"compiled_artifact":descriptor})
        );
        return Ok(());
    }
    let program_path = if let Some(control) = &control {
        compiled_snapshot::cached_program(workspace, control)?
    } else {
        program_path
    };
    let mut program = Command::new(program_path);
    program
        .current_dir(ROOT)
        .env_clear()
        .env("TMPDIR", format!("{ROOT}/tmp"));
    if let Some(launch) = launch {
        let runtime = tokio::runtime::Builder::new_current_thread()
            .enable_all()
            .build()?;
        runtime.block_on(code_platform_bridge::run(
            tokio::process::Command::from(program),
            launch,
        ))?;
        // The parent bridge validated and delivered the fixed bounded result.
        if let (Some(record), Some(deadline)) = (&retained, deadline) {
            rust_profile::rust_prepare_archive::verify_tree(
                &compile_root,
                &record.content,
                true,
                deadline,
            )
            .map_err(|_| "Execution changed supplied dependency content")?;
        }
        return Ok(());
    }
    run(program)?;
    if let (Some(record), Some(deadline)) = (&retained, deadline) {
        rust_profile::rust_prepare_archive::verify_tree(
            &compile_root,
            &record.content,
            true,
            deadline,
        )
        .map_err(|_| "Execution changed supplied dependency content")?;
    }
    bytes.clear();
    std::fs::File::open(format!("{ROOT}/result.json"))?
        .take(256 * 1024 + 1)
        .read_to_end(&mut bytes)?;
    if bytes.len() > 256 * 1024 {
        return Err("Rust result exceeds 256 KiB".into());
    }
    let result: serde_json::Value = serde_json::from_slice(&bytes)?;
    println!("{}", serde_json::json!({"revision":1, "result":result}));
    Ok(())
}
// Reproduce the existing compiled Execute request contract from the original
// trusted PID-1-pinned control; this is not a new selector or authority issuer.
fn original_compiled_request_digest(control: &compiled_code::Control) -> std::io::Result<[u8; 32]> {
    control.validate(compiled_code::Purpose::Execute)?;
    if control.binding.policy_revision != "cargo-broker-execute-v1" {
        return Err(compiled_code::invalid());
    }
    #[derive(serde::Serialize)]
    struct Intent<'a> {
        revision: u8,
        purpose: &'static str,
        base_prepared_request_sha256: &'a compiled_code::ContentSha256,
        snapshot_key_sha256: &'a compiled_code::ContentSha256,
        descriptor_sha256: &'a compiled_code::ContentSha256,
    }
    let descriptor = control
        .descriptor_sha256
        .as_ref()
        .ok_or_else(compiled_code::invalid)?;
    let raw = serde_json::to_vec(&Intent {
        revision: 1,
        purpose: "execute",
        base_prepared_request_sha256: &control.binding.base_prepared_request_sha256,
        snapshot_key_sha256: &control.snapshot_key_sha256,
        descriptor_sha256: descriptor,
    })
    .map_err(|_| compiled_code::invalid())?;
    code_platform_launch::digest(
        compiled_code::domain_hash(b"elitea.sandbox.compiled-job.v1\0", &raw).as_str(),
    )
}

fn install_static_broker_profile(root: &Path) -> Result<(), Box<dyn std::error::Error>> {
    let image = Path::new("/opt/elitea-rust-platform-v1");
    compiled_code::regular_directory(image)?;
    compiled_code::regular_directory(&image.join("src"))?;
    compiled_code::regular_directory(&image.join(".cargo"))?;
    let profile = rust_profile::Profile::Broker;
    let manifest = compiled_code::read_regular(&image.join("Cargo.toml"), 1024 * 1024)?;
    if manifest != profile.template().as_bytes() {
        return Err("Static broker manifest changed".into());
    }
    std::fs::write(root.join("Cargo.toml"), manifest)?;
    for (file, expected) in profile.sources() {
        let bytes = compiled_code::read_regular(&image.join(file), 256 * 1024)?;
        if bytes != expected.as_bytes() {
            return Err("Static broker image module changed".into());
        }
        std::fs::write(root.join(file), bytes)?;
    }
    // The image owns the pinned lock/config. Snapshot binding measures both;
    // Code never supplies a path, manifest, lock or compiler argument.
    for file in ["Cargo.lock", ".cargo/config.toml"] {
        let bytes = compiled_code::read_regular(&image.join(file), 1024 * 1024)?;
        std::fs::write(root.join(file), bytes)?;
    }
    Ok(())
}

fn main() {
    let args: Vec<String> = std::env::args().skip(1).collect();
    let mode =
        match args.as_slice() {
            [] if std::env::var_os("ELITEA_COMPILED_CODE_JOB_PURPOSE").is_none() => Ok(None),
            [command] if command == "--compile-snapshot" => compiled_code::enabled_purpose()
                .and_then(|purpose| {
                    if purpose == compiled_code::Purpose::Compile {
                        Ok(Some(purpose))
                    } else {
                        Err(compiled_code::invalid())
                    }
                }),
            [command] if command == "--execute-snapshot" => compiled_code::enabled_purpose()
                .and_then(|purpose| {
                    if purpose == compiled_code::Purpose::Execute {
                        Ok(Some(purpose))
                    } else {
                        Err(compiled_code::invalid())
                    }
                }),
            _ => Err(compiled_code::invalid()),
        };
    let outcome = mode
        .map_err(Box::<dyn std::error::Error>::from)
        .and_then(execute);
    if let Err(error) = outcome {
        eprintln!("Rust Code failed: {error}");
        std::process::exit(2);
    }
}

#[cfg(test)]
mod platform_compiled_identity_tests {
    use super::*;
    fn original_execute() -> compiled_code::Control {
        let hash = compiled_code::ContentSha256::parse("a".repeat(64)).unwrap();
        let binding = compiled_code::Binding {
            revision: 1,
            reuse_policy: "snapshot_v1".into(),
            tenant_id: "tenant".into(),
            project_id: 7,
            base_prepared_request_sha256: hash.clone(),
            source_sha256: hash.clone(),
            compilation_image_digest: format!("sha256:{}", hash.as_str()),
            execution_image_digest: format!("sha256:{}", hash.as_str()),
            platform: "linux/amd64/gnu".into(),
            target: "x86_64-unknown-linux-gnu".into(),
            policy_revision: "cargo-broker-execute-v1".into(),
            cargo_manifest_sha256: hash.clone(),
            cargo_lock_sha256: hash.clone(),
            cargo_config_sha256: hash.clone(),
            vendor_sha256: hash.clone(),
            toolchain_sha256: hash.clone(),
            adapter_sha256: hash.clone(),
            wrapper_sha256: hash.clone(),
            compiler_flags_sha256: hash,
        };
        compiled_code::Control {
            revision: 1,
            snapshot_key_sha256: binding.key().unwrap(),
            binding,
            descriptor_sha256: Some(compiled_code::ContentSha256::parse("b".repeat(64)).unwrap()),
        }
    }
    #[test]
    fn compiled_broker_request_keeps_execute_digest_distinct_from_prepared_fingerprint() {
        let original = original_execute();
        let expected = code_platform_launch::digest(
            "312c8d6750b7e3bb147caf6a1894062cd0da5fb955155d61fea553280532c92c",
        )
        .unwrap();
        assert_eq!(
            original_compiled_request_digest(&original).unwrap(),
            expected
        );
        assert_ne!(
            expected,
            code_platform_launch::digest(original.binding.base_prepared_request_sha256.as_str())
                .unwrap()
        );
        let mut changed = original.clone();
        changed.descriptor_sha256 =
            Some(compiled_code::ContentSha256::parse("c".repeat(64)).unwrap());
        assert_ne!(
            original_compiled_request_digest(&changed).unwrap(),
            expected
        );
        changed = original.clone();
        changed.binding.base_prepared_request_sha256 =
            compiled_code::ContentSha256::parse("d".repeat(64)).unwrap();
        assert!(original_compiled_request_digest(&changed).is_err());
        changed.snapshot_key_sha256 = changed.binding.key().unwrap();
        assert_ne!(
            original_compiled_request_digest(&changed).unwrap(),
            expected
        );
        changed = original.clone();
        changed.binding.policy_revision = "legacy-policy".into();
        changed.snapshot_key_sha256 = changed.binding.key().unwrap();
        assert!(original_compiled_request_digest(&changed).is_err());
        changed = original;
        changed.descriptor_sha256 = None;
        assert!(original_compiled_request_digest(&changed).is_err());
    }
}

//! Fixed Rust snapshot adapter; one executable with image-owned runtime assets.
use crate::compiled_code::{
    self as contract, Binding, ContentSha256, Control, Descriptor, Purpose,
};
use serde::Serialize;
use std::os::unix::fs::{MetadataExt, OpenOptionsExt, PermissionsExt};
use std::{
    fs,
    io::{self, Read},
    path::{Path, PathBuf},
    process::{Command, Stdio},
    time::{Duration, Instant},
};

#[derive(Serialize)]
struct VendorFile {
    path: String,
    bytes: u64,
    sha256: ContentSha256,
    mode: u32,
}
fn vendor(root: &Path) -> io::Result<ContentSha256> {
    let mut directories = vec![root.to_owned()];
    let mut files = Vec::new();
    let mut raw = 0u64;
    let mut entries = 0usize;
    while let Some(directory) = directories.pop() {
        contract::regular_directory(&directory)?;
        for entry in fs::read_dir(&directory)? {
            entries += 1;
            if entries > 16_384 {
                return Err(contract::invalid());
            }
            let path = entry?.path();
            let metadata = fs::symlink_metadata(&path)?;
            if metadata.is_dir() {
                directories.push(path);
            } else if metadata.is_file() {
                let (sha256, bytes) = contract::hash_regular(&path, 32 * 1024 * 1024)?;
                raw = raw.checked_add(bytes).ok_or_else(contract::invalid)?;
                if raw > 256 * 1024 * 1024 || files.len() >= 16_384 {
                    return Err(contract::invalid());
                }
                let relative = path
                    .strip_prefix(root)
                    .map_err(|_| contract::invalid())?
                    .to_str()
                    .ok_or_else(contract::invalid)?
                    .to_owned();
                files.push(VendorFile {
                    path: relative,
                    bytes,
                    sha256,
                    mode: metadata.permissions().mode() & 0o777,
                });
            } else {
                return Err(contract::invalid());
            }
            if directories.len() > 16_384 {
                return Err(contract::invalid());
            }
        }
    }
    files.sort_by(|a, b| a.path.cmp(&b.path));
    let bytes = serde_json::to_vec(&files).map_err(|_| contract::invalid())?;
    Ok(contract::domain_hash(
        b"elitea.rust.vendor-tree.v1\0",
        &bytes,
    ))
}
fn version(program: &str) -> io::Result<Vec<u8>> {
    let mut child = Command::new(program)
        .arg("-Vv")
        .env_clear()
        .env("PATH", "/usr/local/cargo/bin:/usr/local/bin:/usr/bin:/bin")
        .env("RUSTUP_HOME", "/usr/local/rustup")
        .env("CARGO_HOME", "/usr/local/cargo")
        .stdin(Stdio::null())
        .stdout(Stdio::piped())
        .stderr(Stdio::null())
        .spawn()?;
    let mut bytes = Vec::new();
    let captured = match child.stdout.take() {
        Some(output) => output.take(8193).read_to_end(&mut bytes),
        None => Err(contract::invalid()),
    };
    if captured.is_err() || bytes.len() > 8192 {
        let _ = child.kill();
        let _ = child.wait();
        return Err(contract::invalid());
    }
    if !child.wait()?.success() {
        return Err(contract::invalid());
    }
    Ok(bytes)
}
pub(crate) fn fixed_flags(target: Option<&str>) -> io::Result<ContentSha256> {
    // Both default-profile settings and native profiles also bind their manifest.
    let bytes=serde_json::to_vec(&serde_json::json!({
        "argv":["build","--locked","--offline","-j","2"],"target":target,
        "env":{"PATH":"/usr/local/cargo/bin:/usr/local/bin:/usr/bin:/bin","RUSTUP_HOME":"/usr/local/rustup","CARGO_HOME":"/workspace/rust-job/cargo-home","CARGO_INCREMENTAL":"0","CARGO_TERM_COLOR":"never","HOME":"/workspace","TMPDIR":"/workspace/rust-job/tmp"}
    })).map_err(|_|contract::invalid())?;
    Ok(contract::domain_hash(
        b"elitea.rust.compiler-configuration.v1\0",
        &bytes,
    ))
}
fn rejected(stage: &'static str) -> io::Error {
    // Fixed operator codes only: no paths, request/source, identity or digests.
    io::Error::other(format!("Rust snapshot validation failed: {stage}"))
}
fn binding_mismatch(actual: &Binding, expected: &Binding) -> Option<&'static str> {
    macro_rules! field {
        ($name:ident) => {
            if actual.$name != expected.$name {
                return Some(concat!("binding.", stringify!($name)));
            }
        };
    }
    field!(base_prepared_request_sha256);
    field!(source_sha256);
    field!(cargo_manifest_sha256);
    field!(cargo_lock_sha256);
    field!(cargo_config_sha256);
    field!(vendor_sha256);
    field!(wrapper_sha256);
    field!(adapter_sha256);
    field!(toolchain_sha256);
    field!(compiler_flags_sha256);
    (actual != expected).then_some("binding")
}
pub(crate) fn verify_binding(
    control: &Control,
    request_bytes: &[u8],
    source: &str,
    request: &serde_json::Value,
    profile: &Path,
    target: Option<&str>,
) -> io::Result<()> {
    let binding = &control.binding;
    binding.validate().map_err(|_| rejected("binding.shape"))?;
    let image = contract::trusted_launch_value("ELITEA_COMPILED_CODE_IMAGE_DIGEST")?
        .ok_or_else(|| rejected("launch.image_missing"))?;
    let policy = contract::trusted_launch_value("ELITEA_COMPILED_CODE_POLICY_REVISION")?
        .ok_or_else(|| rejected("launch.policy_missing"))?;
    let platform = match std::env::consts::ARCH {
        "aarch64" => "linux/arm64/gnu",
        "x86_64" => "linux/amd64/gnu",
        _ => return Err(contract::invalid()),
    };
    let runtime_target = match std::env::consts::ARCH {
        "aarch64" => "aarch64-unknown-linux-gnu",
        "x86_64" => "x86_64-unknown-linux-gnu",
        _ => return Err(contract::invalid()),
    };
    if std::env::consts::OS != "linux"
        || binding.compilation_image_digest != image
        || binding.execution_image_digest != image
        || binding.policy_revision != policy
        || request["image_digest"] != image
        || request["policy_revision"] != policy
        || binding.platform != platform
        || binding.target != runtime_target
        || target.is_some_and(|target| target != runtime_target)
    {
        return Err(rejected("launch.binding"));
    }
    contract::regular_directory(profile).map_err(|_| rejected("profile.directory"))?;
    contract::regular_directory(&profile.join("src"))
        .map_err(|_| rejected("profile.source_directory"))?;
    contract::regular_directory(&profile.join(".cargo"))
        .map_err(|_| rejected("profile.config_directory"))?;
    let vendor_root = if target.is_some() {
        profile.join("vendor")
    } else {
        PathBuf::from("/opt/rust-vendor")
    };
    let mut actual: Binding = binding.clone();
    actual.base_prepared_request_sha256 = contract::prepared_fingerprint(request_bytes);
    actual.source_sha256 = ContentSha256::of(source.as_bytes());
    actual.cargo_manifest_sha256 = contract::hash_regular(&profile.join("Cargo.toml"), 1024 * 1024)
        .map_err(|_| rejected("profile.manifest_file"))?
        .0;
    actual.cargo_lock_sha256 = contract::hash_regular(&profile.join("Cargo.lock"), 1024 * 1024)
        .map_err(|_| rejected("profile.lock_file"))?
        .0;
    actual.cargo_config_sha256 =
        contract::hash_regular(&profile.join(".cargo/config.toml"), 1024 * 1024)
            .map_err(|_| rejected("profile.config_file"))?
            .0;
    actual.vendor_sha256 = vendor(&vendor_root).map_err(|_| rejected("profile.vendor_tree"))?;
    actual.wrapper_sha256 = contract::hash_regular(&profile.join("src/main.rs"), 1024 * 1024)
        .map_err(|_| rejected("profile.wrapper_file"))?
        .0;
    actual.adapter_sha256 = contract::hash_regular(
        Path::new("/usr/local/bin/elitea-code-rust"),
        contract::EXECUTABLE_LIMIT,
    )
    .map_err(|_| rejected("image.adapter_file"))?
    .0;
    let versions = serde_json::to_vec(&[
        version("/usr/local/cargo/bin/rustc").map_err(|_| rejected("image.rustc_metadata"))?,
        version("/usr/local/cargo/bin/cargo").map_err(|_| rejected("image.cargo_metadata"))?,
    ])
    .map_err(|_| contract::invalid())?;
    actual.toolchain_sha256 = contract::domain_hash(b"elitea.rust.toolchain.v1\0", &versions);
    actual.compiler_flags_sha256 = fixed_flags(target)?;
    if let Some(stage) = binding_mismatch(&actual, binding) {
        return Err(rejected(stage));
    }
    if actual.key()? != control.snapshot_key_sha256 {
        return Err(rejected("binding.snapshot_key"));
    }
    if contract::read_regular(&profile.join("src/user.rs"), 256 * 1024)
        .map_err(|_| rejected("request.source_file"))?
        != source.as_bytes()
    {
        return Err(rejected("request.source_bytes"));
    }
    if contract::read_regular(Path::new("/workspace/.elitea-code.json"), 1024 * 1024)
        .map_err(|_| rejected("request.prepared_file"))?
        != request_bytes
    {
        return Err(rejected("request.prepared_bytes"));
    }
    if contract::read_regular(Path::new("/workspace/rust-job/input.json"), 1024 * 1024)
        .map_err(|_| rejected("request.input_file"))?
        != serde_json::to_vec(&request["input"]).map_err(|_| contract::invalid())?
    {
        return Err(rejected("request.input_bytes"));
    }
    Ok(())
}
fn same_output_file(actual: &fs::Metadata, expected: &fs::Metadata) -> bool {
    actual.is_file()
        && actual.dev() == expected.dev()
        && actual.ino() == expected.ino()
        && actual.len() == expected.len()
        && actual.nlink() == expected.nlink()
        && actual.uid() == expected.uid()
        && actual.gid() == expected.gid()
        && actual.mode() == expected.mode()
        && actual.mtime() == expected.mtime()
        && actual.mtime_nsec() == expected.mtime_nsec()
        && actual.ctime() == expected.ctime()
        && actual.ctime_nsec() == expected.ctime_nsec()
}
fn cargo_alias_name(name: &std::ffi::OsStr) -> bool {
    let Some(hash) = name
        .to_str()
        .and_then(|name| name.strip_prefix("elitea_code_job-"))
    else {
        return false;
    };
    hash.len() == 16
        && hash
            .bytes()
            .all(|byte| byte.is_ascii_digit() || (b'a'..=b'f').contains(&byte))
}
fn read_compiler_output(root: &Path, program: &Path) -> io::Result<Vec<u8>> {
    let before = fs::symlink_metadata(program)?;
    if before.nlink() == 1 {
        return contract::read_regular(program, contract::EXECUTABLE_LIMIT as usize);
    }
    // The owned compiler has exited and all descendants have drained before capture.
    // Cargo links its fixed binary to one hashed executable in debug/deps on Linux.
    // Only this output layout can enter a new, single-link immutable snapshot.
    if !before.is_file()
        || before.nlink() != 2
        || before.len() == 0
        || before.len() > contract::EXECUTABLE_LIMIT
        || before.uid() != fs::symlink_metadata(root)?.uid()
        || program.file_name() != Some(std::ffi::OsStr::new("elitea-code-job"))
    {
        return Err(contract::invalid());
    }
    let debug = program.parent().ok_or_else(contract::invalid)?;
    if debug.file_name() != Some(std::ffi::OsStr::new("debug")) {
        return Err(contract::invalid());
    }
    let deps = debug.join("deps");
    contract::regular_directory(&deps)?;
    let mut alias = None;
    for (index, entry) in fs::read_dir(&deps)?.enumerate() {
        if index >= 16_384 {
            return Err(contract::invalid());
        }
        let entry = entry?;
        if !cargo_alias_name(&entry.file_name()) {
            continue;
        }
        let path = entry.path();
        let metadata = fs::symlink_metadata(&path)?;
        if !metadata.is_file() {
            return Err(contract::invalid());
        }
        if metadata.dev() == before.dev() && metadata.ino() == before.ino() {
            if alias.is_some() || !same_output_file(&metadata, &before) {
                return Err(contract::invalid());
            }
            alias = Some(path);
        }
    }
    let alias = alias.ok_or_else(contract::invalid)?;
    #[cfg(target_os = "linux")]
    let flags = 0x20000 | 0x800; // O_NOFOLLOW | O_NONBLOCK
    #[cfg(target_os = "macos")]
    let flags = 0x100 | 0x4;
    let mut file = fs::OpenOptions::new()
        .read(true)
        .custom_flags(flags)
        .open(program)?;
    if !same_output_file(&file.metadata()?, &before) {
        return Err(contract::invalid());
    }
    let mut bytes = Vec::new();
    file.by_ref()
        .take(contract::EXECUTABLE_LIMIT + 1)
        .read_to_end(&mut bytes)?;
    if bytes.len() as u64 != before.len()
        || !same_output_file(&file.metadata()?, &before)
        || !same_output_file(&fs::symlink_metadata(program)?, &before)
        || !same_output_file(&fs::symlink_metadata(&alias)?, &before)
    {
        return Err(contract::invalid());
    }
    Ok(bytes)
}
pub(crate) fn capture(root: &Path, program: &Path, control: &Control) -> io::Result<Descriptor> {
    if control.descriptor_sha256.is_some()
        || root.join(contract::CAPTURED).exists()
        || root.join(contract::RELEASE).exists()
    {
        return Err(contract::invalid());
    }
    // Reject target directory symlinks from build scripts before opening output.
    let relative = program
        .strip_prefix(root)
        .map_err(|_| contract::invalid())?;
    let mut parent = root.to_owned();
    for part in relative
        .parent()
        .ok_or_else(contract::invalid)?
        .components()
    {
        parent.push(part);
        contract::regular_directory(&parent)?;
    }
    let bytes = read_compiler_output(root, program)?;
    if bytes.is_empty() {
        return Err(contract::invalid());
    }
    let base = root.join(contract::DIRECTORY);
    fs::create_dir(&base)?;
    contract::immutable_file(&base.join(contract::EXECUTABLE), &bytes, true)?;
    let descriptor = Descriptor {
        revision: 1,
        binding: control.binding.clone(),
        snapshot_key_sha256: control.snapshot_key_sha256.clone(),
        executable_sha256: ContentSha256::of(&bytes),
        executable_bytes: bytes.len() as u64,
    };
    descriptor.validate(control)?;
    let bytes = descriptor.bytes()?;
    contract::immutable_file(&base.join(contract::DESCRIPTOR), &bytes, false)?;
    contract::immutable_file(
        &root.join(contract::CAPTURED),
        ContentSha256::of(&bytes).as_str().as_bytes(),
        false,
    )?;
    Ok(descriptor)
}
pub(crate) fn await_release(root: &Path, deadline: Instant) -> io::Result<()> {
    loop {
        if root.join(contract::RELEASE).exists() {
            if !contract::read_regular(&root.join(contract::RELEASE), 0)?.is_empty() {
                return Err(contract::invalid());
            }
            return Ok(());
        }
        if Instant::now() >= deadline {
            return Err(io::Error::new(
                io::ErrorKind::TimedOut,
                "snapshot publication release missing",
            ));
        }
        std::thread::sleep(Duration::from_millis(10));
    }
}
pub(crate) fn cached_program(root: &Path, control: &Control) -> io::Result<PathBuf> {
    control.validate(Purpose::Execute)?;
    let sha = control
        .descriptor_sha256
        .as_ref()
        .ok_or_else(contract::invalid)?;
    if contract::read_regular(&root.join(contract::READY), 64)? != sha.as_str().as_bytes() {
        return Err(contract::invalid());
    }
    contract::load_descriptor(root, control)?;
    Ok(root.join(contract::DIRECTORY).join(contract::EXECUTABLE))
}

#[cfg(test)]
mod tests {
    use super::*;
    async fn run_fixture(command: Command, allowance: Duration) {
        // A fixed trusted test program with no build scripts or child creation.
        // Production compiler process-group/adopted-child ownership is tested
        // separately on Linux; this only supplies component input/output bytes.
        let mut command = tokio::process::Command::from(command);
        let mut child = command
            .stdin(Stdio::null())
            .stdout(Stdio::null())
            .stderr(Stdio::null())
            .kill_on_drop(true)
            .spawn()
            .unwrap();
        match tokio::time::timeout(allowance, child.wait()).await {
            Ok(result) => assert!(result.unwrap().success()),
            Err(_) => {
                child.kill().await.unwrap();
                panic!("trusted snapshot fixture exceeded its deadline");
            }
        }
    }
    fn fixture_control() -> Control {
        serde_json::from_slice(include_bytes!(
            "../fixtures/compiled-snapshot-v1/compile-control.json"
        ))
        .unwrap()
    }
    #[test]
    fn diagnostic_binding_failure_has_exact_code_without_sensitive_values() {
        let expected = fixture_control().binding;
        let mut actual = expected.clone();
        let sensitive = "secret-request-and-source-sentinel";
        actual.source_sha256 = ContentSha256::of(sensitive.as_bytes());
        let stage = binding_mismatch(&actual, &expected).unwrap();
        assert_eq!(stage, "binding.source_sha256");
        let message = rejected(stage).to_string();
        assert_eq!(
            message,
            "Rust snapshot validation failed: binding.source_sha256"
        );
        assert!(!message.contains(sensitive));
        assert!(!message.contains(actual.source_sha256.as_str()));
        assert!(!message.contains(expected.source_sha256.as_str()));
        assert!(binding_mismatch(&expected, &expected).is_none());
    }
    #[test]
    fn diagnostic_file_guard_retains_rejection_without_exposing_a_path() {
        let directory = tempfile::tempdir().unwrap();
        let secret_path = directory.path().join("private-request-source-sentinel");
        let error = contract::read_regular(&secret_path, 16)
            .map_err(|_| rejected("request.source_file"))
            .unwrap_err();
        assert_eq!(
            error.to_string(),
            "Rust snapshot validation failed: request.source_file"
        );
        assert!(
            !error
                .to_string()
                .contains("private-request-source-sentinel")
        );
    }
    #[test]
    fn capture_is_fixed_single_regular_file_and_rejects_target_symlinks() {
        let root = tempfile::tempdir().unwrap();
        let other = tempfile::tempdir().unwrap();
        fs::write(other.path().join("elitea-code-job"), b"binary").unwrap();
        std::os::unix::fs::symlink(other.path(), root.path().join("target")).unwrap();
        assert!(
            capture(
                root.path(),
                &root.path().join("target/elitea-code-job"),
                &fixture_control()
            )
            .is_err()
        );
        assert!(!root.path().join(contract::CAPTURED).exists());
        fs::remove_file(root.path().join("target")).unwrap();
        fs::create_dir(root.path().join("target")).unwrap();
        let program = root.path().join("target/elitea-code-job");
        fs::write(&program, b"binary").unwrap();
        capture(root.path(), &program, &fixture_control()).unwrap();
        assert_eq!(
            fs::read_dir(root.path().join(contract::DIRECTORY))
                .unwrap()
                .count(),
            2
        );
        assert!(capture(root.path(), &program, &fixture_control()).is_err());
    }
    fn linked_cargo_output(root: &Path) -> (PathBuf, PathBuf) {
        let debug = root.join("rust-job/target/debug");
        fs::create_dir_all(debug.join("deps")).unwrap();
        let program = debug.join("elitea-code-job");
        let alias = debug.join("deps/elitea_code_job-0123456789abcdef");
        fs::write(&program, b"compiled-binary").unwrap();
        fs::hard_link(&program, &alias).unwrap();
        (program, alias)
    }
    #[test]
    fn cargo_capture_detaches_verified_alias_into_single_link_immutable_snapshot() {
        let root = tempfile::tempdir().unwrap();
        let (program, alias) = linked_cargo_output(root.path());
        assert_eq!(fs::symlink_metadata(&program).unwrap().nlink(), 2);
        // General transport, import and publication readers must still reject links.
        assert!(contract::read_regular(&program, 32).is_err());
        let control = fixture_control();
        let descriptor = capture(root.path(), &program, &control).unwrap();
        let copied = root
            .path()
            .join(contract::DIRECTORY)
            .join(contract::EXECUTABLE);
        let metadata = fs::symlink_metadata(&copied).unwrap();
        assert_eq!(metadata.nlink(), 1);
        assert_eq!(metadata.permissions().mode() & 0o777, 0o500);
        assert_ne!(
            metadata.ino(),
            fs::symlink_metadata(&program).unwrap().ino()
        );
        assert_eq!(
            contract::read_regular(&copied, 32).unwrap(),
            b"compiled-binary"
        );
        for path in [
            root.path().join(contract::CAPTURED),
            root.path()
                .join(contract::DIRECTORY)
                .join(contract::DESCRIPTOR),
        ] {
            assert_eq!(fs::symlink_metadata(path).unwrap().nlink(), 1);
        }
        // The original Cargo aliases remain linked, but cannot mutate published bytes.
        fs::write(&alias, b"changed-binary").unwrap();
        assert_eq!(fs::read(&program).unwrap(), b"changed-binary");
        assert_eq!(
            contract::read_regular(&copied, 32).unwrap(),
            b"compiled-binary"
        );
        assert_eq!(
            contract::load_descriptor(root.path(), &control)
                .unwrap()
                .bytes()
                .unwrap(),
            descriptor.bytes().unwrap()
        );
    }
    #[test]
    fn compiler_output_stability_rejects_alias_mutation_and_inode_replacement() {
        let root = tempfile::tempdir().unwrap();
        let (program, alias) = linked_cargo_output(root.path());
        let before = fs::symlink_metadata(&program).unwrap();
        assert!(same_output_file(
            &fs::symlink_metadata(&alias).unwrap(),
            &before
        ));
        fs::write(&alias, b"mutated-through-alias").unwrap();
        assert!(!same_output_file(
            &fs::symlink_metadata(&program).unwrap(),
            &before
        ));
        assert!(!same_output_file(
            &fs::symlink_metadata(&alias).unwrap(),
            &before
        ));
        fs::remove_file(&alias).unwrap();
        fs::write(&alias, b"compiled-binary").unwrap();
        assert!(!same_output_file(
            &fs::symlink_metadata(&alias).unwrap(),
            &before
        ));
    }
    #[test]
    fn cargo_capture_rejects_unknown_alias_names_and_extra_links() {
        for name in [
            "unknown",
            "elitea_code_job-0123456789abcde",
            "elitea_code_job-0123456789abcdef0",
            "elitea_code_job-0123456789abcdeF",
            "elitea_code_job-0123456789abcdef.d",
        ] {
            let root = tempfile::tempdir().unwrap();
            let (program, alias) = linked_cargo_output(root.path());
            fs::rename(&alias, alias.parent().unwrap().join(name)).unwrap();
            assert!(capture(root.path(), &program, &fixture_control()).is_err());
            assert!(!root.path().join(contract::DIRECTORY).exists());
            assert!(!root.path().join(contract::CAPTURED).exists());
        }
        let root = tempfile::tempdir().unwrap();
        let (program, _) = linked_cargo_output(root.path());
        fs::hard_link(&program, root.path().join("third-link")).unwrap();
        assert!(capture(root.path(), &program, &fixture_control()).is_err());
        assert!(!root.path().join(contract::CAPTURED).exists());
    }
    #[test]
    fn cargo_capture_rejects_source_alias_and_deps_directory_symlinks() {
        for variant in ["source", "alias", "directory"] {
            let root = tempfile::tempdir().unwrap();
            let (program, alias) = linked_cargo_output(root.path());
            match variant {
                "source" => {
                    fs::remove_file(&program).unwrap();
                    std::os::unix::fs::symlink(&alias, &program).unwrap();
                }
                "alias" => {
                    let other = root.path().join("unapproved-hard-link");
                    fs::rename(&alias, &other).unwrap();
                    std::os::unix::fs::symlink(&program, &alias).unwrap();
                }
                "directory" => {
                    let deps = alias.parent().unwrap();
                    let other = root.path().join("unapproved-deps");
                    fs::rename(deps, &other).unwrap();
                    std::os::unix::fs::symlink(&other, deps).unwrap();
                }
                _ => unreachable!(),
            }
            assert!(capture(root.path(), &program, &fixture_control()).is_err());
            assert!(!root.path().join(contract::DIRECTORY).exists());
            assert!(!root.path().join(contract::CAPTURED).exists());
        }
    }
    #[test]
    fn cargo_capture_rejects_empty_oversized_or_noncanonical_output() {
        for variant in ["empty", "oversized", "binary-name", "parent"] {
            let root = tempfile::tempdir().unwrap();
            let (mut program, _) = linked_cargo_output(root.path());
            match variant {
                "empty" => fs::write(&program, b"").unwrap(),
                "oversized" => fs::OpenOptions::new()
                    .write(true)
                    .open(&program)
                    .unwrap()
                    .set_len(contract::EXECUTABLE_LIMIT + 1)
                    .unwrap(),
                "binary-name" => {
                    let other = program.with_file_name("other-bin");
                    fs::rename(&program, &other).unwrap();
                    program = other;
                }
                "parent" => {
                    let debug = program.parent().unwrap();
                    let other = debug.with_file_name("other-profile");
                    fs::rename(debug, &other).unwrap();
                    program = other.join("elitea-code-job");
                }
                _ => unreachable!(),
            }
            assert!(capture(root.path(), &program, &fixture_control()).is_err());
            assert!(!root.path().join(contract::CAPTURED).exists());
        }
    }
    #[test]
    fn cached_execution_requires_readiness_and_rejects_replaced_executable() {
        let root = tempfile::tempdir().unwrap();
        let program = root.path().join("program");
        fs::write(&program, b"binary").unwrap();
        let control = fixture_control();
        let descriptor = capture(root.path(), &program, &control).unwrap();
        let mut execution = control;
        execution.descriptor_sha256 = Some(ContentSha256::of(&descriptor.bytes().unwrap()));
        assert!(cached_program(root.path(), &execution).is_err());
        contract::immutable_file(
            &root.path().join(contract::READY),
            execution
                .descriptor_sha256
                .as_ref()
                .unwrap()
                .as_str()
                .as_bytes(),
            false,
        )
        .unwrap();
        assert_eq!(
            cached_program(root.path(), &execution).unwrap(),
            root.path()
                .join(contract::DIRECTORY)
                .join(contract::EXECUTABLE)
        );
        let imported = root
            .path()
            .join(contract::DIRECTORY)
            .join(contract::EXECUTABLE);
        fs::set_permissions(&imported, fs::Permissions::from_mode(0o700)).unwrap();
        fs::write(imported, b"different").unwrap();
        assert!(cached_program(root.path(), &execution).is_err());
    }
    #[test]
    fn implicit_and_native_target_flags_have_distinct_identity() {
        assert_ne!(
            fixed_flags(None).unwrap(),
            fixed_flags(Some("aarch64-unknown-linux-gnu")).unwrap()
        );
    }
    #[test]
    fn vendor_identity_changes_on_content_mode_and_rejects_links() {
        let root = tempfile::tempdir().unwrap();
        let file = root.path().join("source");
        fs::write(&file, b"one").unwrap();
        let original = vendor(root.path()).unwrap();
        fs::write(&file, b"two").unwrap();
        assert_ne!(original, vendor(root.path()).unwrap());
        let content = vendor(root.path()).unwrap();
        fs::set_permissions(&file, fs::Permissions::from_mode(0o755)).unwrap();
        assert_ne!(content, vendor(root.path()).unwrap());
        std::os::unix::fs::symlink(&file, root.path().join("link")).unwrap();
        assert!(vendor(root.path()).is_err());
    }

    #[tokio::test(flavor = "current_thread")]
    async fn offline_cold_build_capture_release_and_fresh_cached_import_are_inert() {
        let compilation = tempfile::tempdir().unwrap();
        let profile = compilation.path().join("job");
        fs::create_dir(&profile).unwrap();
        fs::create_dir(profile.join("src")).unwrap();
        fs::write(
            profile.join("Cargo.toml"),
            "[package]\nname=\"elitea-code-job\"\nversion=\"0.1.0\"\nedition=\"2024\"\n",
        )
        .unwrap();
        fs::write(
            profile.join("Cargo.lock"),
            "version = 4\n\n[[package]]\nname = \"elitea-code-job\"\nversion = \"0.1.0\"\n",
        )
        .unwrap();
        fs::write(
            profile.join("src/main.rs"),
            "fn main() { std::fs::write(\"entrypoint-ran\", br#\"{\"answer\":42}\"#).unwrap(); }\n",
        )
        .unwrap();
        let target = profile.join("target");
        let mut compile = Command::new(env!("CARGO"));
        compile
            .current_dir(&profile)
            .env("CARGO_TARGET_DIR", &target)
            .env("CARGO_BUILD_JOBS", "1")
            .env("CARGO_INCREMENTAL", "0")
            .env("CARGO_PROFILE_DEV_DEBUG", "0")
            .args(["build", "--locked", "--offline", "-j", "1"]);
        // This is a component proof using the host toolchain and a synthetic binding.
        // Linux launch attestation, PID 1 role grants and export quiescence need the
        // separate real-container acceptance; no test-only authority bypass is added.
        run_fixture(compile, Duration::from_secs(20)).await;
        assert!(!profile.join("entrypoint-ran").exists());
        let control = fixture_control();
        let descriptor = capture(
            compilation.path(),
            &target.join("debug/elitea-code-job"),
            &control,
        )
        .unwrap();
        let exported = descriptor.bytes().unwrap();
        assert_eq!(
            contract::read_regular(
                &compilation
                    .path()
                    .join(contract::DIRECTORY)
                    .join(contract::DESCRIPTOR),
                contract::DESCRIPTOR_LIMIT,
            )
            .unwrap(),
            exported
        );
        contract::load_descriptor(compilation.path(), &control).unwrap();
        assert_eq!(
            await_release(compilation.path(), Instant::now())
                .unwrap_err()
                .kind(),
            io::ErrorKind::TimedOut
        );
        contract::immutable_file(&compilation.path().join(contract::RELEASE), b"", false).unwrap();
        await_release(compilation.path(), Instant::now() + Duration::from_secs(1)).unwrap();
        assert!(!profile.join("entrypoint-ran").exists());

        let execution = tempfile::tempdir().unwrap();
        let imported = execution.path().join(contract::DIRECTORY);
        fs::create_dir(&imported).unwrap();
        contract::immutable_file(&imported.join(contract::DESCRIPTOR), &exported, false).unwrap();
        let executable = contract::read_regular(
            &compilation
                .path()
                .join(contract::DIRECTORY)
                .join(contract::EXECUTABLE),
            contract::EXECUTABLE_LIMIT as usize,
        )
        .unwrap();
        contract::immutable_file(&imported.join(contract::EXECUTABLE), &executable, true).unwrap();
        let mut selected = control;
        selected.descriptor_sha256 = Some(ContentSha256::of(&exported));
        assert!(cached_program(execution.path(), &selected).is_err());
        assert!(!execution.path().join("entrypoint-ran").exists());
        contract::immutable_file(
            &execution.path().join(contract::READY),
            selected
                .descriptor_sha256
                .as_ref()
                .unwrap()
                .as_str()
                .as_bytes(),
            false,
        )
        .unwrap();
        let mut program = Command::new(cached_program(execution.path(), &selected).unwrap());
        program.current_dir(execution.path()).env_clear();
        assert!(!execution.path().join("entrypoint-ran").exists());
        run_fixture(program, Duration::from_secs(5)).await;
        assert_eq!(
            fs::read(execution.path().join("entrypoint-ran")).unwrap(),
            br#"{"answer":42}"#
        );
        assert!(!profile.join("entrypoint-ran").exists());
    }
}

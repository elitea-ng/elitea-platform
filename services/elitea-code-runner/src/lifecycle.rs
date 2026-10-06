//! Inert preparation, dispatch, and bounded content transfer. No user code runs here.
#[path = "code_prepared_extensions.rs"]
mod code_prepared_extensions;

use serde::Deserialize;
use std::{
    fs::{self, OpenOptions},
    io::{self, Read, Seek, Write},
    path::Path,
};

const INPUT_LIMIT: u64 = 8 * 1024 * 1024;
const CONTENT_HEADER_LIMIT: usize = 4096;
const CONTENT_FILE_LIMIT: u64 = 32 * 1024 * 1024;
const CONTENT_CHUNK_BYTES: usize = 64 * 1024;

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct ContentHeader {
    name: String,
    bytes: u64,
    #[serde(default, deserialize_with = "identity_field")]
    pod_uid: Option<String>,
    #[serde(default, deserialize_with = "identity_field")]
    request_digest: Option<String>,
}

#[derive(Default, Deserialize)]
#[serde(deny_unknown_fields)]
struct ReleaseHeader {
    #[serde(default, deserialize_with = "identity_field")]
    pod_uid: Option<String>,
    #[serde(default, deserialize_with = "identity_field")]
    request_digest: Option<String>,
}

fn identity_field<'de, D: serde::Deserializer<'de>>(
    deserializer: D,
) -> Result<Option<String>, D::Error> {
    String::deserialize(deserializer).map(Some)
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct Input {
    pod_uid: String,
    request_digest: String,
    code: Option<String>,
    job: Option<String>,
}

fn invalid() -> io::Error {
    io::Error::other("sandbox preparation or runtime identity is invalid")
}

fn validate(input: &Input, uid: &str, digest: &str) -> io::Result<()> {
    if uid.is_empty()
        || input.pod_uid != uid
        || input.request_digest != digest
        || digest.len() != 64
        || !digest
            .bytes()
            .all(|b| b.is_ascii_digit() || (b'a'..=b'f').contains(&b))
    {
        return Err(invalid());
    }
    Ok(())
}

fn atomic_file(root: &Path, name: &str, bytes: &[u8]) -> io::Result<()> {
    let temporary = root.join(format!("{name}.{}", std::process::id()));
    let result = (|| {
        let mut file = OpenOptions::new()
            .write(true)
            .create_new(true)
            .open(&temporary)?;
        file.write_all(bytes)?;
        file.sync_all()?;
        fs::rename(&temporary, root.join(name))
    })();
    // Never remove another helper's temporary file after a create conflict.
    if result.is_err()
        && result
            .as_ref()
            .err()
            .is_none_or(|e| e.kind() != io::ErrorKind::AlreadyExists)
    {
        let _ = fs::remove_file(&temporary);
    }
    result
}

fn ready(root: &Path, digest: &str) -> io::Result<bool> {
    match fs::File::open(root.join(".elitea-ready")) {
        Ok(file) => {
            let mut value = String::new();
            file.take(65).read_to_string(&mut value)?;
            if value != digest {
                return Err(invalid());
            }
            Ok(true)
        }
        Err(error) if error.kind() == io::ErrorKind::NotFound => Ok(false),
        Err(error) => Err(error),
    }
}

fn apply(command: &str, input: Input, root: &Path, uid: &str, digest: &str) -> io::Result<()> {
    validate(&input, uid, digest)?;
    match command {
        "--prepare" => {
            let code = input.code.as_deref().ok_or_else(invalid)?;
            let job = input.job.as_deref().ok_or_else(invalid)?;
            if code.is_empty()
                || code.len() > 1024 * 1024
                || job.is_empty()
                || job.len() > 64 * 1024
            {
                return Err(invalid());
            }
            if ready(root, digest)? {
                return Ok(());
            }
            if root.join(".elitea-dispatch").exists() {
                return Err(invalid());
            }
            atomic_file(root, ".elitea-code.json", code.as_bytes())?;
            atomic_file(root, ".elitea-job.json", job.as_bytes())?;
            atomic_file(root, ".elitea-ready", digest.as_bytes())
        }
        "--prepared" | "--dispatch" => {
            if input.code.is_some() || input.job.is_some() || !ready(root, digest)? {
                return Err(invalid());
            }
            if command == "--dispatch" {
                match OpenOptions::new()
                    .write(true)
                    .create_new(true)
                    .open(root.join(".elitea-dispatch"))
                {
                    Ok(file) => file.sync_all()?,
                    Err(error) if error.kind() == io::ErrorKind::AlreadyExists => {}
                    Err(error) => return Err(error),
                }
            }
            Ok(())
        }
        _ => Err(invalid()),
    }
}

fn content_identity(
    pod_uid: Option<&str>,
    request_digest: Option<&str>,
    expected: Option<(&str, &str)>,
) -> io::Result<()> {
    match (pod_uid, request_digest, expected) {
        (None, None, None) => Ok(()),
        (Some(pod_uid), Some(request_digest), Some((uid, digest))) => validate(
            &Input {
                pod_uid: pod_uid.into(),
                request_digest: request_digest.into(),
                code: None,
                job: None,
            },
            uid,
            digest,
        ),
        _ => Err(invalid()),
    }
}

fn header_line(input: &mut impl Read, allow_empty: bool) -> io::Result<Option<Vec<u8>>> {
    let mut line = Vec::with_capacity(CONTENT_HEADER_LIMIT);
    let mut byte = [0];
    while line.len() < CONTENT_HEADER_LIMIT {
        match input.read_exact(&mut byte) {
            Ok(()) => {
                line.push(byte[0]);
                if byte[0] == b'\n' {
                    return Ok(Some(line));
                }
            }
            Err(error)
                if error.kind() == io::ErrorKind::UnexpectedEof
                    && line.is_empty()
                    && allow_empty =>
            {
                return Ok(None);
            }
            Err(error) => return Err(error),
        }
    }
    Err(invalid())
}

fn end_of_input(input: &mut impl Read) -> io::Result<()> {
    let mut byte = [0];
    match input.read_exact(&mut byte) {
        Err(error) if error.kind() == io::ErrorKind::UnexpectedEof => Ok(()),
        Err(error) => Err(error),
        Ok(()) => Err(invalid()),
    }
}

fn content_header(
    input: &mut impl Read,
    expected: Option<(&str, &str)>,
) -> io::Result<ContentHeader> {
    let line = header_line(input, false)?.ok_or_else(invalid)?;
    let header: ContentHeader = serde_json::from_slice(&line).map_err(|_| invalid())?;
    if header.name.is_empty()
        || header.name.len() > 256
        || matches!(header.name.as_str(), "." | "..")
        || !header
            .name
            .bytes()
            .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'.' | b'_' | b'+' | b'-'))
        || header.bytes > CONTENT_FILE_LIMIT
    {
        return Err(invalid());
    }
    content_identity(
        header.pod_uid.as_deref(),
        header.request_digest.as_deref(),
        expected,
    )?;
    Ok(header)
}

fn regular_directory(directory: &Path) -> io::Result<()> {
    if !fs::symlink_metadata(directory)?.is_dir() {
        return Err(invalid());
    }
    Ok(())
}

fn regular_source(path: &Path, bytes: u64) -> io::Result<fs::File> {
    let before = fs::symlink_metadata(path)?;
    if !before.is_file() || before.len() != bytes {
        return Err(invalid());
    }
    #[cfg(any(target_os = "linux", target_os = "macos"))]
    {
        use std::os::unix::fs::{MetadataExt, OpenOptionsExt};
        // Stable OS ABI flags: O_NOFOLLOW | O_NONBLOCK. No FIFO or symlink open can block here.
        #[cfg(target_os = "linux")]
        const FLAGS: i32 = 0x20000 | 0x800;
        #[cfg(target_os = "macos")]
        const FLAGS: i32 = 0x100 | 0x4;
        let file = OpenOptions::new()
            .read(true)
            .custom_flags(FLAGS)
            .open(path)?;
        let opened = file.metadata()?;
        if !opened.is_file()
            || opened.len() != bytes
            || opened.dev() != before.dev()
            || opened.ino() != before.ino()
        {
            return Err(invalid());
        }
        Ok(file)
    }
    #[cfg(not(any(target_os = "linux", target_os = "macos")))]
    {
        // Fail on unsupported hosts instead of removing the no-follow requirement.
        Err(invalid())
    }
}

fn stream_exact(input: &mut impl Read, output: &mut impl Write, bytes: u64) -> io::Result<()> {
    let mut remaining = bytes;
    let mut buffer = [0; CONTENT_CHUNK_BYTES];
    while remaining > 0 {
        let count =
            usize::try_from(remaining.min(CONTENT_CHUNK_BYTES as u64)).map_err(|_| invalid())?;
        input.read_exact(&mut buffer[..count])?;
        output.write_all(&buffer[..count])?;
        remaining -= count as u64;
    }
    end_of_input(input)
}

fn compare_exact(left: &mut impl Read, right: &mut impl Read, bytes: u64) -> io::Result<()> {
    let mut remaining = bytes;
    let mut left_buffer = [0; CONTENT_CHUNK_BYTES];
    let mut right_buffer = [0; CONTENT_CHUNK_BYTES];
    while remaining > 0 {
        let count =
            usize::try_from(remaining.min(CONTENT_CHUNK_BYTES as u64)).map_err(|_| invalid())?;
        left.read_exact(&mut left_buffer[..count])?;
        right.read_exact(&mut right_buffer[..count])?;
        if left_buffer[..count] != right_buffer[..count] {
            return Err(io::Error::new(
                io::ErrorKind::AlreadyExists,
                "dependency content differs",
            ));
        }
        remaining -= count as u64;
    }
    end_of_input(left)?;
    end_of_input(right)
}

fn dependency_read(
    root: &Path,
    expected: Option<(&str, &str)>,
    input: &mut impl Read,
    output: &mut impl Write,
) -> io::Result<()> {
    read_dependency(&root.join("python-dependencies"), expected, input, output)
}

fn execution_dependency_read(
    root: &Path,
    expected: Option<(&str, &str)>,
    input: &mut impl Read,
    output: &mut impl Write,
) -> io::Result<()> {
    read_dependency(&root.join("wheels"), expected, input, output)
}

fn read_dependency(
    directory: &Path,
    expected: Option<(&str, &str)>,
    input: &mut impl Read,
    output: &mut impl Write,
) -> io::Result<()> {
    let header = content_header(input, expected)?;
    end_of_input(input)?;
    regular_directory(directory)?;
    let mut file = regular_source(&directory.join(header.name), header.bytes)?;
    stream_exact(&mut file, output, header.bytes)?;
    output.flush()
}

fn dependency_write(
    root: &Path,
    expected: Option<(&str, &str)>,
    input: &mut impl Read,
) -> io::Result<()> {
    let header = content_header(input, expected)?;
    write_dependency(&root.join("wheels"), header, input)
}
fn write_dependency(
    directory: &Path,
    header: ContentHeader,
    input: &mut impl Read,
) -> io::Result<()> {
    #[cfg(unix)]
    {
        use std::os::unix::fs::DirBuilderExt;
        match fs::DirBuilder::new().mode(0o700).create(directory) {
            Ok(()) => {}
            Err(error) if error.kind() == io::ErrorKind::AlreadyExists => {}
            Err(error) => return Err(error),
        }
    }
    #[cfg(not(unix))]
    fs::create_dir_all(&directory)?;
    regular_directory(directory)?;
    let target = directory.join(header.name);
    match fs::symlink_metadata(&target) {
        Ok(_) => {
            let mut existing = regular_source(&target, header.bytes)?;
            return compare_exact(input, &mut existing, header.bytes);
        }
        Err(error) if error.kind() == io::ErrorKind::NotFound => {}
        Err(error) => return Err(error),
    }
    let mut temporary = tempfile::Builder::new()
        .prefix(".elitea-dependency-")
        .tempfile_in(directory)?;
    stream_exact(input, temporary.as_file_mut(), header.bytes)?;
    temporary.as_file_mut().flush()?;
    temporary.as_file().sync_all()?;
    // NOREPLACE publication also protects concurrent retries from replacing immutable files.
    match temporary.persist_noclobber(&target) {
        Ok(_) => Ok(()),
        Err(mut error) if error.error.kind() == io::ErrorKind::AlreadyExists => {
            let mut existing = regular_source(&target, header.bytes)?;
            error.file.as_file_mut().rewind()?;
            compare_exact(error.file.as_file_mut(), &mut existing, header.bytes)
        }
        Err(error) => Err(error.error),
    }
}

fn preparation_release(
    root: &Path,
    expected: Option<(&str, &str)>,
    input: &mut impl Read,
) -> io::Result<()> {
    let header = match header_line(input, expected.is_none())? {
        Some(line) => serde_json::from_slice::<ReleaseHeader>(&line).map_err(|_| invalid())?,
        None => ReleaseHeader::default(),
    };
    content_identity(
        header.pod_uid.as_deref(),
        header.request_digest.as_deref(),
        expected,
    )?;
    end_of_input(input)?;
    regular_directory(root)?;
    let target = root.join(".elitea-python-preparation-release");
    match fs::symlink_metadata(&target) {
        Ok(metadata) if metadata.is_file() && metadata.len() == 0 => {
            regular_source(&target, 0)?;
            return Ok(());
        }
        Ok(_) => return Err(invalid()),
        Err(error) if error.kind() == io::ErrorKind::NotFound => {}
        Err(error) => return Err(error),
    }
    let temporary = tempfile::Builder::new()
        .prefix(".elitea-release-")
        .tempfile_in(root)?;
    temporary.as_file().sync_all()?;
    match temporary.persist_noclobber(&target) {
        Ok(_) => Ok(()),
        Err(error) if error.error.kind() == io::ErrorKind::AlreadyExists => {
            regular_source(&target, 0)?;
            Ok(())
        }
        Err(error) => Err(error.error),
    }
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct NativeHeader {
    revision: u8,
    kind: String,
    root: String,
    name: String,
    bytes: u64,
    #[serde(default, deserialize_with = "identity_field")]
    pod_uid: Option<String>,
    #[serde(default, deserialize_with = "identity_field")]
    request_digest: Option<String>,
}
fn native_identity(
    root: &Path,
    header: &NativeHeader,
    preparation: bool,
) -> io::Result<serde_json::Value> {
    if header.revision != 2
        || !matches!(header.kind.as_str(), "deno" | "cargo")
        || !is_digest(&header.root)
    {
        return Err(invalid());
    }
    let request: serde_json::Value = serde_json::from_reader(
        regular_source(
            &root.join(".elitea-code.json"),
            fs::symlink_metadata(root.join(".elitea-code.json"))?.len(),
        )?
        .take(1024 * 1024 + 1),
    )
    .map_err(|_| invalid())?;
    if preparation {
        let marker: serde_json::Value = serde_json::from_reader(
            fs::File::open(root.join(".elitea-native-preparation.json"))?.take(256 * 1024 + 1),
        )
        .map_err(|_| invalid())?;
        if request["revision"] != 2
            || marker["revision"] != 2
            || marker["status"] != "resolved"
            || marker["bundle"]["digest"] != header.root
            || marker["bundle"]["kind"] != header.kind
        {
            return Err(invalid());
        }
    } else if code_prepared_extensions::base_revision(&request).map_err(|_| invalid())? != 3
        || request["dependency_bundle_sha256"] != header.root
        || request["native_dependencies"]["kind"] != header.kind
    {
        return Err(invalid());
    }
    Ok(request)
}
fn is_digest(v: &str) -> bool {
    v.len() == 64
        && v.bytes()
            .all(|b| b.is_ascii_digit() || (b'a'..=b'f').contains(&b))
}
fn native_content(
    command: &str,
    root: &Path,
    expected: Option<(&str, &str)>,
    input: &mut impl Read,
    output: &mut impl Write,
) -> io::Result<()> {
    let header: NativeHeader =
        serde_json::from_slice(&header_line(input, false)?.ok_or_else(invalid)?)
            .map_err(|_| invalid())?;
    content_identity(
        header.pod_uid.as_deref(),
        header.request_digest.as_deref(),
        expected,
    )?;
    let preparation = matches!(
        command,
        "--native-dependency-read" | "--native-preparation-release"
    );
    native_identity(root, &header, preparation)?;
    let metadata = header.name == "elitea-native-bundle-v2.json";
    let ready = header.name == "elitea-native-ready-v2.json";
    if !(metadata || ready || header.name.strip_suffix(".blob").is_some_and(is_digest))
        || header.bytes
            > if metadata || ready {
                128 * 1024
            } else if header.kind == "cargo" {
                128 * 1024 * 1024
            } else {
                32 * 1024 * 1024
            }
    {
        return Err(invalid());
    }
    let base = root.join(if preparation {
        "native-dependencies"
    } else {
        "native-bundle"
    });
    if command == "--native-preparation-release" {
        if !metadata || header.bytes != 0 {
            return Err(invalid());
        }
        end_of_input(input)?;
        let path = root.join(".elitea-native-preparation-release");
        match fs::symlink_metadata(&path) {
            Ok(_) => {
                regular_source(&path, 0)?;
                return Ok(());
            }
            Err(e) if e.kind() == io::ErrorKind::NotFound => {}
            Err(e) => return Err(e),
        };
        let file = tempfile::Builder::new()
            .prefix(".native-release-")
            .tempfile_in(root)?;
        file.as_file().sync_all()?;
        return match file.persist_noclobber(&path) {
            Ok(_) => Ok(()),
            Err(e) if e.error.kind() == io::ErrorKind::AlreadyExists => {
                regular_source(&path, 0)?;
                Ok(())
            }
            Err(e) => Err(e.error),
        };
    }
    // Finalization has an asynchronous process-group owner in run().
    if command == "--native-dependency-finalize" {
        return Err(invalid());
    }
    if !base.exists() {
        fs::create_dir(&base)?;
    }
    regular_directory(&base)?;
    let directory = if metadata || ready {
        base
    } else {
        let d = base.join("objects");
        if !d.exists() {
            fs::create_dir(&d)?;
        }
        regular_directory(&d)?;
        d
    };
    let legacy = ContentHeader {
        name: header.name,
        bytes: header.bytes,
        pod_uid: None,
        request_digest: None,
    };
    match command {
        "--native-dependency-write" if !ready && !root.join(".elitea-dispatch").exists() => {
            write_dependency(&directory, legacy, input)
        }
        "--native-dependency-read" | "--native-execution-dependency-read" => {
            end_of_input(input)?;
            let mut f = regular_source(&directory.join(legacy.name), legacy.bytes)?;
            stream_exact(&mut f, output, legacy.bytes)?;
            output.flush()
        }
        _ => Err(invalid()),
    }
}
async fn finalize_native(
    root: &Path,
    expected: Option<(&str, &str)>,
    input: &mut impl Read,
) -> io::Result<()> {
    // Start one phase budget before admission and process launch.
    let deadline = std::time::Instant::now() + std::time::Duration::from_secs(27);
    let deadline_unix_ms = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map_err(|_| invalid())?
        .as_millis()
        + 27_000;
    let header: NativeHeader =
        serde_json::from_slice(&header_line(input, false)?.ok_or_else(invalid)?)
            .map_err(|_| invalid())?;
    content_identity(
        header.pod_uid.as_deref(),
        header.request_digest.as_deref(),
        expected,
    )?;
    native_identity(root, &header, false)?;
    if header.name != "elitea-native-bundle-v2.json"
        || header.bytes != 0
        || root.join(".elitea-dispatch").exists()
    {
        return Err(invalid());
    }
    end_of_input(input)?;
    let base = root.join("native-bundle");
    regular_directory(&base)?;
    let mut command = if header.kind == "deno" {
        let mut command = std::process::Command::new("/usr/local/bin/deno");
        command
            .env_clear()
            .env("DENO_DIR", "/opt/deno-cache")
            .env("HOME", "/workspace")
            .args([
                "run",
                "--cached-only",
                "--no-config",
                "--no-prompt",
                "--allow-run=/usr/local/bin/deno",
                "--allow-read=/opt/elitea-code,/opt/deno-cache,/workspace",
                "--allow-write=/workspace",
                "--allow-env",
                "--deny-net",
                "--deny-ffi",
                "/opt/elitea-code/javascript_hydration_job.mjs",
            ]);
        command
    } else {
        let mut command = std::process::Command::new("/usr/local/bin/elitea-code-rust-prepare");
        command.env_clear().arg("--hydrate");
        command
    };
    command.env(
        "ELITEA_NATIVE_FINALIZATION_DEADLINE_UNIX_MS",
        deadline_unix_ms.to_string(),
    );
    crate::native_finalization::run(command, &base, deadline).await?;
    regular_source(
        &base.join("elitea-native-ready-v2.json"),
        fs::symlink_metadata(base.join("elitea-native-ready-v2.json"))?.len(),
    )?;
    Ok(())
}
fn run_content(
    command: &str,
    root: &Path,
    expected: Option<(&str, &str)>,
    input: &mut impl Read,
    output: &mut impl Write,
) -> io::Result<()> {
    if command.starts_with("--native-") {
        return native_content(command, root, expected, input, output);
    }
    match command {
        "--dependency-read" => dependency_read(root, expected, input, output),
        "--execution-dependency-read" => execution_dependency_read(root, expected, input, output),
        "--dependency-write" => dependency_write(root, expected, input),
        "--preparation-release" => preparation_release(root, expected, input),
        _ => Err(invalid()),
    }
}

pub async fn run(command: &str) -> io::Result<()> {
    if command.starts_with("--workspace-") {
        return crate::workspace_lifecycle::run(command);
    }

    if matches!(
        command,
        "--compiled-control-write"
            | "--compiled-artifact-write"
            | "--compiled-artifact-finalize"
            | "--compiled-artifact-status"
            | "--compiled-artifact-read"
            | "--compiled-artifact-release"
    ) {
        let identity = match (
            crate::compiled_code::trusted_launch_value("ELITEA_SANDBOX_POD_UID")?,
            crate::compiled_code::trusted_launch_value("ELITEA_SANDBOX_REQUEST")?,
        ) {
            (Some(uid), Some(digest)) => Some((uid, digest)),
            (None, None) => None,
            _ => return Err(invalid()),
        };
        return crate::compiled_lifecycle::run(
            command,
            Path::new("/workspace"),
            identity
                .as_ref()
                .map(|(uid, digest)| (uid.as_str(), digest.as_str())),
            &mut io::stdin().lock(),
            &mut io::stdout().lock(),
        );
    }
    if matches!(command, "--prepared" | "--dispatch")
        && std::env::var_os("ELITEA_COMPILED_CODE_JOB_PURPOSE").is_some()
    {
        crate::compiled_lifecycle::execution_ready(Path::new("/workspace"))?;
    }
    if matches!(
        command,
        "--dependency-read"
            | "--execution-dependency-read"
            | "--dependency-write"
            | "--preparation-release"
            | "--native-dependency-read"
            | "--native-execution-dependency-read"
            | "--native-dependency-write"
            | "--native-dependency-finalize"
            | "--native-preparation-release"
    ) {
        let identity = match (
            std::env::var("ELITEA_SANDBOX_POD_UID"),
            std::env::var("ELITEA_SANDBOX_REQUEST"),
        ) {
            (Ok(uid), Ok(digest)) => Some((uid, digest)),
            (Err(std::env::VarError::NotPresent), Err(std::env::VarError::NotPresent)) => None,
            _ => return Err(invalid()),
        };
        if command == "--native-dependency-finalize" {
            return finalize_native(
                Path::new("/workspace"),
                identity
                    .as_ref()
                    .map(|(uid, digest)| (uid.as_str(), digest.as_str())),
                &mut io::stdin().lock(),
            )
            .await
            .map_err(|error| io::Error::new(error.kind(), "sandbox native finalization failed"));
        }
        return run_content(
            command,
            Path::new("/workspace"),
            identity
                .as_ref()
                .map(|(uid, digest)| (uid.as_str(), digest.as_str())),
            &mut io::stdin().lock(),
            &mut io::stdout().lock(),
        )
        .map_err(|error| io::Error::new(error.kind(), "sandbox dependency transfer failed"));
    }
    let mut bytes = Vec::new();
    io::stdin().take(INPUT_LIMIT + 1).read_to_end(&mut bytes)?;
    if bytes.len() > INPUT_LIMIT as usize {
        return Err(invalid());
    }
    let input = serde_json::from_slice(&bytes).map_err(|_| invalid())?;
    let uid = std::env::var("ELITEA_SANDBOX_POD_UID").map_err(|_| invalid())?;
    let digest = std::env::var("ELITEA_SANDBOX_REQUEST").map_err(|_| invalid())?;
    apply(command, input, Path::new("/workspace"), &uid, &digest)
}

#[cfg(test)]
mod tests {
    use super::*;
    static COUNTER: std::sync::atomic::AtomicUsize = std::sync::atomic::AtomicUsize::new(0);
    struct Directory(std::path::PathBuf);
    impl Directory {
        fn new() -> Self {
            let path = std::env::temp_dir().join(format!(
                "elitea-lifecycle-{}-{}",
                std::process::id(),
                COUNTER.fetch_add(1, std::sync::atomic::Ordering::SeqCst)
            ));
            fs::create_dir(&path).unwrap();
            Self(path)
        }
    }
    impl Drop for Directory {
        fn drop(&mut self) {
            let _ = fs::remove_dir_all(&self.0);
        }
    }
    fn input(uid: &str, prepare: bool) -> Input {
        Input {
            pod_uid: uid.into(),
            request_digest: "a".repeat(64),
            code: prepare.then(|| "private source".into()),
            job: prepare.then(|| "runner request".into()),
        }
    }
    #[test]
    fn prepare_is_inert_and_dispatch_is_repeatable_without_rewriting_code() {
        let root = Directory::new();
        let digest = "a".repeat(64);
        apply(
            "--prepare",
            input("original", true),
            &root.0,
            "original",
            &digest,
        )
        .unwrap();
        assert!(!root.0.join(".elitea-dispatch").exists());
        apply(
            "--dispatch",
            input("original", false),
            &root.0,
            "original",
            &digest,
        )
        .unwrap();
        apply(
            "--dispatch",
            input("original", false),
            &root.0,
            "original",
            &digest,
        )
        .unwrap();
        let mut retry = input("original", true);
        retry.code = Some("different".into());
        apply("--prepare", retry, &root.0, "original", &digest).unwrap();
        assert_eq!(
            fs::read_to_string(root.0.join(".elitea-code.json")).unwrap(),
            "private source"
        );
    }
    #[test]
    fn replacement_pod_and_unprepared_dispatch_are_rejected() {
        let root = Directory::new();
        let digest = "a".repeat(64);
        assert!(
            apply(
                "--prepare",
                input("original", true),
                &root.0,
                "replacement",
                &digest
            )
            .is_err()
        );
        assert!(
            apply(
                "--dispatch",
                input("original", false),
                &root.0,
                "original",
                &digest
            )
            .is_err()
        );
        assert_eq!(fs::read_dir(&root.0).unwrap().count(), 0);
    }
    #[test]
    fn invalid_ready_marker_and_oversized_input_cannot_signal_execution() {
        let root = Directory::new();
        let digest = "a".repeat(64);
        fs::write(root.0.join(".elitea-ready"), "b".repeat(64)).unwrap();
        assert!(
            apply(
                "--dispatch",
                input("original", false),
                &root.0,
                "original",
                &digest
            )
            .is_err()
        );
        fs::remove_file(root.0.join(".elitea-ready")).unwrap();
        let mut large = input("original", true);
        large.code = Some("x".repeat(1024 * 1024 + 1));
        assert!(apply("--prepare", large, &root.0, "original", &digest).is_err());
        assert!(!root.0.join(".elitea-dispatch").exists());
    }

    fn frame(name: &str, bytes: u64, payload: &[u8]) -> Vec<u8> {
        let mut frame =
            serde_json::to_vec(&serde_json::json!({ "name": name, "bytes": bytes })).unwrap();
        frame.push(b'\n');
        frame.extend_from_slice(payload);
        frame
    }
    fn transfer(command: &str, root: &Path, bytes: &[u8]) -> io::Result<Vec<u8>> {
        let mut input = io::Cursor::new(bytes);
        let mut output = Vec::new();
        run_content(command, root, None, &mut input, &mut output)?;
        Ok(output)
    }
    fn source(root: &Path, name: &str, bytes: &[u8]) {
        fs::create_dir_all(root.join("python-dependencies")).unwrap();
        fs::write(root.join("python-dependencies").join(name), bytes).unwrap();
    }
    #[test]
    fn dependency_binary_transfer_uses_fixed_roles_and_exact_framing() {
        let root = Directory::new();
        let bytes = [0, 255, b'\n', b'\r', 128];
        assert!(
            transfer(
                "--dependency-write",
                &root.0,
                &frame("pkg.whl", bytes.len() as u64, &bytes)
            )
            .unwrap()
            .is_empty()
        );
        assert_eq!(fs::read(root.0.join("wheels/pkg.whl")).unwrap(), bytes);
        assert!(!root.0.join(".elitea-ready").exists());
        assert!(!root.0.join(".elitea-dispatch").exists());
        // Read cannot select the execution destination directory.
        assert!(
            transfer(
                "--dependency-read",
                &root.0,
                &frame("pkg.whl", bytes.len() as u64, &[])
            )
            .is_err()
        );
        source(&root.0, "pkg.whl", &bytes);
        assert_eq!(
            transfer(
                "--dependency-read",
                &root.0,
                &frame("pkg.whl", bytes.len() as u64, &[])
            )
            .unwrap(),
            bytes
        );
        assert!(
            transfer(
                "--dependency-read",
                &root.0,
                &frame("pkg.whl", bytes.len() as u64, &[0])
            )
            .is_err()
        );
    }

    #[test]
    fn execution_export_reads_imported_content_without_preparation_fallback() {
        let root = Directory::new();
        let payload = b"\0\xffexecution";
        source(&root.0, "shared.whl", b"preparation");
        source(&root.0, "preparation-only.whl", b"private");
        transfer(
            "--dependency-write",
            &root.0,
            &frame("shared.whl", payload.len() as u64, payload),
        )
        .unwrap();
        assert_eq!(
            transfer(
                "--execution-dependency-read",
                &root.0,
                &frame("shared.whl", payload.len() as u64, &[]),
            )
            .unwrap(),
            payload
        );
        assert_eq!(
            transfer("--dependency-read", &root.0, &frame("shared.whl", 11, &[])).unwrap(),
            b"preparation"
        );
        assert!(
            transfer(
                "--execution-dependency-read",
                &root.0,
                &frame("preparation-only.whl", 7, &[]),
            )
            .is_err()
        );
        let metadata = br#"{"revision":1,"digest":"recorded-root"}"#;
        transfer(
            "--dependency-write",
            &root.0,
            &frame("elitea-python-bundle.json", metadata.len() as u64, metadata),
        )
        .unwrap();
        assert_eq!(
            transfer(
                "--execution-dependency-read",
                &root.0,
                &frame("elitea-python-bundle.json", metadata.len() as u64, &[]),
            )
            .unwrap(),
            metadata
        );
        assert!(
            transfer(
                "--dependency-read",
                &root.0,
                &frame("elitea-python-bundle.json", metadata.len() as u64, &[])
            )
            .is_err()
        );
    }

    #[test]
    fn execution_export_preserves_identity_framing_and_file_validation() {
        let root = Directory::new();
        transfer(
            "--dependency-write",
            &root.0,
            &frame("package.whl", 3, b"abc"),
        )
        .unwrap();
        let digest = "a".repeat(64);
        for (uid, request) in [
            ("original", digest.as_str()),
            ("replacement", digest.as_str()),
            ("original", "changed"),
        ] {
            let mut header = serde_json::to_vec(&serde_json::json!({
                "name":"package.whl", "bytes":3, "pod_uid":uid, "request_digest":request
            }))
            .unwrap();
            header.push(b'\n');
            let mut output = Vec::new();
            let result = run_content(
                "--execution-dependency-read",
                &root.0,
                Some(("original", &digest)),
                &mut header.as_slice(),
                &mut output,
            );
            if uid == "original" && request == digest {
                result.unwrap();
                assert_eq!(output, b"abc");
            } else {
                assert!(result.is_err());
                assert!(output.is_empty());
            }
        }
        for input in [
            frame("package.whl", 2, &[]),
            frame("../package.whl", 3, &[]),
            frame("package.whl", 3, b"extra"),
        ] {
            assert!(transfer("--execution-dependency-read", &root.0, &input).is_err());
        }
        #[cfg(unix)]
        {
            std::os::unix::fs::symlink(
                root.0.join("wheels/package.whl"),
                root.0.join("wheels/link.whl"),
            )
            .unwrap();
            assert!(
                transfer(
                    "--execution-dependency-read",
                    &root.0,
                    &frame("link.whl", 3, &[])
                )
                .is_err()
            );
        }
    }

    #[test]
    fn dependency_missing_wrong_length_and_unsafe_source_emit_no_bytes() {
        let root = Directory::new();
        source(&root.0, "pkg.whl", b"abc");
        for (name, bytes) in [
            ("missing.whl", 3),
            ("pkg.whl", 2),
            ("pkg.whl", 4),
            ("python-dependencies", 0),
        ] {
            let mut output = Vec::new();
            let mut input = io::Cursor::new(frame(name, bytes, &[]));
            assert!(dependency_read(&root.0, None, &mut input, &mut output).is_err());
            assert!(output.is_empty());
        }
        #[cfg(unix)]
        {
            std::os::unix::fs::symlink(
                root.0.join("python-dependencies/pkg.whl"),
                root.0.join("python-dependencies/link.whl"),
            )
            .unwrap();
            let mut output = Vec::new();
            assert!(
                dependency_read(
                    &root.0,
                    None,
                    &mut io::Cursor::new(frame("link.whl", 3, &[])),
                    &mut output
                )
                .is_err()
            );
            assert!(output.is_empty());
        }
    }

    #[test]
    fn dependency_headers_are_strict_flat_and_bounded() {
        let root = Directory::new();
        for name in [
            "",
            ".",
            "..",
            "../pkg.whl",
            "/pkg.whl",
            "x/y",
            "x\\y",
            "pkg.whl\n",
            "é.whl",
            &"x".repeat(257),
        ] {
            assert!(transfer("--dependency-write", &root.0, &frame(name, 0, &[])).is_err());
        }
        for bytes in [
            b"{\"name\":\"pkg.whl\",\"bytes\":0,\"endpoint\":\"private\"}\n".as_slice(),
            b"{\"name\":\"pkg.whl\",\"bytes\":0,\"bytes\":0}\n".as_slice(),
            b"{\"name\":\"pkg.whl\",\"bytes\":0,\"name\":\"other.whl\"}\n".as_slice(),
            b"{\"name\":\"pkg.whl\",\"bytes\":-1}\n".as_slice(),
            b"{\"name\":\"pkg.whl\",\"bytes\":1.0}\n".as_slice(),
            b"{\"name\":\"pkg.whl\",\"bytes\":0}".as_slice(),
        ] {
            assert!(transfer("--dependency-write", &root.0, bytes).is_err());
        }
        assert!(
            transfer(
                "--dependency-write",
                &root.0,
                &frame("pkg.whl", CONTENT_FILE_LIMIT + 1, &[])
            )
            .is_err()
        );
        let mut oversized = vec![b' '; CONTENT_HEADER_LIMIT];
        oversized.push(b'\n');
        assert!(transfer("--dependency-write", &root.0, &oversized).is_err());
        assert!(!root.0.join("wheels").exists());
        let mut maximum = frame("empty.whl", 0, &[]);
        maximum.pop();
        maximum.resize(CONTENT_HEADER_LIMIT - 1, b' ');
        maximum.push(b'\n');
        transfer("--dependency-write", &root.0, &maximum).unwrap();
        assert_eq!(
            fs::metadata(root.0.join("wheels/empty.whl")).unwrap().len(),
            0
        );
    }

    #[test]
    fn dependency_truncated_excess_and_existing_writes_never_replace_content() {
        let root = Directory::new();
        for bytes in [frame("pkg.whl", 4, b"abc"), frame("pkg.whl", 2, b"abc")] {
            assert!(transfer("--dependency-write", &root.0, &bytes).is_err());
            assert_eq!(fs::read_dir(root.0.join("wheels")).unwrap().count(), 0);
        }
        transfer("--dependency-write", &root.0, &frame("pkg.whl", 3, b"abc")).unwrap();
        transfer("--dependency-write", &root.0, &frame("pkg.whl", 3, b"abc")).unwrap();
        let error =
            transfer("--dependency-write", &root.0, &frame("pkg.whl", 3, b"xyz")).unwrap_err();
        assert_eq!(error.kind(), io::ErrorKind::AlreadyExists);
        for bytes in [
            frame("pkg.whl", 3, b"ab"),
            frame("pkg.whl", 3, b"abcd"),
            frame("pkg.whl", 4, b"abcd"),
        ] {
            assert!(transfer("--dependency-write", &root.0, &bytes).is_err());
            assert_eq!(fs::read(root.0.join("wheels/pkg.whl")).unwrap(), b"abc");
        }
        assert_eq!(fs::read_dir(root.0.join("wheels")).unwrap().count(), 1);
        #[cfg(unix)]
        {
            use std::os::unix::fs::{MetadataExt, PermissionsExt};
            assert_eq!(
                fs::metadata(root.0.join("wheels/pkg.whl"))
                    .unwrap()
                    .permissions()
                    .mode()
                    & 0o777,
                0o600
            );
            std::os::unix::fs::symlink(
                root.0.join("wheels/pkg.whl"),
                root.0.join("wheels/link.whl"),
            )
            .unwrap();
            assert!(
                transfer("--dependency-write", &root.0, &frame("link.whl", 3, b"abc")).is_err()
            );
            assert_ne!(
                fs::metadata(root.0.join("wheels/pkg.whl")).unwrap().ino(),
                0
            );
        }
    }

    #[test]
    fn concurrent_dependency_writes_publish_one_complete_immutable_file() {
        struct BarrierReader<'a> {
            input: io::Cursor<Vec<u8>>,
            barrier: &'a std::sync::Barrier,
            waited: bool,
        }
        impl Read for BarrierReader<'_> {
            fn read(&mut self, bytes: &mut [u8]) -> io::Result<usize> {
                let count = self.input.read(bytes)?;
                if count == 0 && !self.waited {
                    self.waited = true;
                    self.barrier.wait();
                }
                Ok(count)
            }
        }
        for payloads in [[b"abc", b"xyz"], [b"abc", b"abc"]] {
            let root = Directory::new();
            let barrier = std::sync::Barrier::new(2);
            let outcomes = std::thread::scope(|scope| {
                let handles = payloads.map(|payload| {
                    let path = &root.0;
                    let barrier = &barrier;
                    scope.spawn(move || {
                        dependency_write(
                            path,
                            None,
                            &mut BarrierReader {
                                input: io::Cursor::new(frame("pkg.whl", 3, payload)),
                                barrier,
                                waited: false,
                            },
                        )
                    })
                });
                handles.map(|handle| handle.join().unwrap())
            });
            let identical = payloads[0] == payloads[1];
            assert_eq!(
                outcomes.iter().filter(|result| result.is_ok()).count(),
                if identical { 2 } else { 1 }
            );
            if !identical {
                assert_eq!(
                    outcomes.into_iter().find_map(Result::err).unwrap().kind(),
                    io::ErrorKind::AlreadyExists
                );
            }
            let saved = fs::read(root.0.join("wheels/pkg.whl")).unwrap();
            assert!(saved == b"abc" || saved == b"xyz");
            assert_eq!(fs::read_dir(root.0.join("wheels")).unwrap().count(), 1);
        }
    }

    struct ShortReader<R> {
        input: R,
        maximum: usize,
    }
    impl<R: Read> Read for ShortReader<R> {
        fn read(&mut self, buffer: &mut [u8]) -> io::Result<usize> {
            let size = buffer.len().min(self.maximum);
            self.input.read(&mut buffer[..size])
        }
    }
    struct GeneratedReader {
        remaining: u64,
        largest_request: usize,
    }
    impl Read for GeneratedReader {
        fn read(&mut self, buffer: &mut [u8]) -> io::Result<usize> {
            self.largest_request = self.largest_request.max(buffer.len());
            let size = buffer.len().min(8191).min(self.remaining as usize);
            buffer[..size].fill(0xa5);
            self.remaining -= size as u64;
            Ok(size)
        }
    }
    #[derive(Default)]
    struct CountWriter {
        bytes: u64,
        largest_write: usize,
    }
    impl Write for CountWriter {
        fn write(&mut self, bytes: &[u8]) -> io::Result<usize> {
            self.bytes += bytes.len() as u64;
            self.largest_write = self.largest_write.max(bytes.len());
            assert!(bytes.iter().all(|byte| *byte == 0xa5));
            Ok(bytes.len())
        }
        fn flush(&mut self) -> io::Result<()> {
            Ok(())
        }
    }
    #[test]
    fn dependency_stream_handles_short_reads_with_a_fixed_chunk_bound() {
        let root = Directory::new();
        let payload = vec![0xa5; CONTENT_CHUNK_BYTES + 7];
        let mut short = ShortReader {
            input: io::Cursor::new(frame("pkg.whl", payload.len() as u64, &payload)),
            maximum: 3,
        };
        dependency_write(&root.0, None, &mut short).unwrap();
        assert_eq!(fs::read(root.0.join("wheels/pkg.whl")).unwrap(), payload);
        let mut generated = GeneratedReader {
            remaining: CONTENT_FILE_LIMIT,
            largest_request: 0,
        };
        let mut output = CountWriter::default();
        stream_exact(&mut generated, &mut output, CONTENT_FILE_LIMIT).unwrap();
        assert_eq!(output.bytes, CONTENT_FILE_LIMIT);
        assert_eq!(output.largest_write, CONTENT_CHUNK_BYTES);
        assert!(generated.largest_request <= CONTENT_CHUNK_BYTES);
        let mut frame =
            io::Cursor::new(frame("maximum.whl", CONTENT_FILE_LIMIT, &[])).chain(GeneratedReader {
                remaining: CONTENT_FILE_LIMIT,
                largest_request: 0,
            });
        dependency_write(&root.0, None, &mut frame).unwrap();
        assert_eq!(
            fs::metadata(root.0.join("wheels/maximum.whl"))
                .unwrap()
                .len(),
            CONTENT_FILE_LIMIT
        );
    }

    #[test]
    fn dependency_commands_preserve_pod_and_request_identity_fencing() {
        let root = Directory::new();
        let digest = "a".repeat(64);
        let identity = Some(("original", digest.as_str()));
        let mut valid = serde_json::to_vec(&serde_json::json!({
            "name": "pkg.whl", "bytes": 3, "pod_uid": "original", "request_digest": digest,
        }))
        .unwrap();
        valid.extend_from_slice(b"\nabc");
        let mut output = Vec::new();
        run_content(
            "--dependency-write",
            &root.0,
            identity,
            &mut io::Cursor::new(&valid),
            &mut output,
        )
        .unwrap();
        assert!(output.is_empty());
        let rejected = Directory::new();
        for supplied in [
            serde_json::json!({ "name": "x.whl", "bytes": 0 }),
            serde_json::json!({ "name": "x.whl", "bytes": 0, "pod_uid": "original" }),
            serde_json::json!({ "name": "x.whl", "bytes": 0, "request_digest": digest }),
            serde_json::json!({ "name": "x.whl", "bytes": 0, "pod_uid": null, "request_digest": null }),
            serde_json::json!({ "name": "x.whl", "bytes": 0, "pod_uid": "replacement", "request_digest": digest }),
            serde_json::json!({ "name": "x.whl", "bytes": 0, "pod_uid": "original", "request_digest": "b".repeat(64) }),
        ] {
            let mut bytes = serde_json::to_vec(&supplied).unwrap();
            bytes.push(b'\n');
            assert!(
                run_content(
                    "--dependency-write",
                    &rejected.0,
                    identity,
                    &mut io::Cursor::new(bytes),
                    &mut Vec::new()
                )
                .is_err()
            );
        }
        assert!(
            run_content(
                "--dependency-write",
                &rejected.0,
                None,
                &mut io::Cursor::new(&valid),
                &mut Vec::new()
            )
            .is_err()
        );
        assert!(!rejected.0.join("wheels").exists());
        source(&root.0, "pkg.whl", b"abc");
        let header = &valid[..valid.len() - 3];
        assert!(
            run_content(
                "--dependency-read",
                &root.0,
                Some(("replacement", digest.as_str())),
                &mut io::Cursor::new(header),
                &mut output
            )
            .is_err()
        );
        assert!(output.is_empty());
        run_content(
            "--dependency-read",
            &root.0,
            identity,
            &mut io::Cursor::new(header),
            &mut output,
        )
        .unwrap();
        assert_eq!(output, b"abc");
    }

    #[test]
    fn preparation_release_is_atomic_empty_and_identity_fenced() {
        let root = Directory::new();
        let digest = "a".repeat(64);
        let identity = Some(("original", digest.as_str()));
        assert!(
            run_content(
                "--preparation-release",
                &root.0,
                identity,
                &mut io::Cursor::new([]),
                &mut Vec::new()
            )
            .is_err()
        );
        let bytes = format!("{{\"pod_uid\":\"original\",\"request_digest\":\"{digest}\"}}\n");
        assert!(
            run_content(
                "--preparation-release",
                &root.0,
                Some(("replacement", digest.as_str())),
                &mut io::Cursor::new(bytes.as_bytes()),
                &mut Vec::new()
            )
            .is_err()
        );
        assert!(!root.0.join(".elitea-python-preparation-release").exists());
        run_content(
            "--preparation-release",
            &root.0,
            identity,
            &mut io::Cursor::new(bytes.as_bytes()),
            &mut Vec::new(),
        )
        .unwrap();
        let path = root.0.join(".elitea-python-preparation-release");
        assert_eq!(fs::metadata(&path).unwrap().len(), 0);
        assert_eq!(fs::read_dir(&root.0).unwrap().count(), 1);
        run_content(
            "--preparation-release",
            &root.0,
            identity,
            &mut io::Cursor::new(bytes.as_bytes()),
            &mut Vec::new(),
        )
        .unwrap();
        let docker = Directory::new();
        transfer("--preparation-release", &docker.0, &[]).unwrap();
        assert_eq!(
            fs::metadata(docker.0.join(".elitea-python-preparation-release"))
                .unwrap()
                .len(),
            0
        );
        assert!(transfer("--preparation-release", &docker.0, b"\n").is_err());
        fs::write(&path, b"not empty").unwrap();
        assert!(
            run_content(
                "--preparation-release",
                &root.0,
                identity,
                &mut io::Cursor::new(bytes.as_bytes()),
                &mut Vec::new()
            )
            .is_err()
        );
    }

    #[cfg(unix)]
    #[test]
    fn dependency_directory_and_release_symlinks_are_rejected() {
        let root = Directory::new();
        let other = Directory::new();
        std::os::unix::fs::symlink(&other.0, root.0.join("wheels")).unwrap();
        assert!(transfer("--dependency-write", &root.0, &frame("pkg.whl", 3, b"abc")).is_err());
        assert_eq!(fs::read_dir(&other.0).unwrap().count(), 0);
        std::os::unix::fs::symlink(&other.0, root.0.join("python-dependencies")).unwrap();
        fs::write(other.0.join("pkg.whl"), b"abc").unwrap();
        assert!(transfer("--dependency-read", &root.0, &frame("pkg.whl", 3, &[])).is_err());
        std::os::unix::fs::symlink(
            other.0.join("pkg.whl"),
            root.0.join(".elitea-python-preparation-release"),
        )
        .unwrap();
        assert!(transfer("--preparation-release", &root.0, &[]).is_err());
        assert_eq!(fs::read(other.0.join("pkg.whl")).unwrap(), b"abc");
    }
}

#[cfg(test)]
mod native_tests {
    use super::*;
    fn header(name: &str, root: &str, kind: &str, bytes: u64) -> Vec<u8> {
        let mut out = serde_json::to_vec(
            &serde_json::json!({"revision":2,"kind":kind,"root":root,"name":name,"bytes":bytes}),
        )
        .unwrap();
        out.push(b'\n');
        out
    }
    #[test]
    fn native_helper_refuses_format_root_paths_and_dispatched_imports() {
        let dir = tempfile::tempdir().unwrap();
        let root = dir.path();
        let digest = "a".repeat(64);
        fs::write(root.join(".elitea-code.json"),serde_json::to_vec(&serde_json::json!({"revision":3,"dependency_bundle_sha256":digest,"native_dependencies":{"kind":"deno"}})).unwrap()).unwrap();
        let name = format!("{}.blob", "b".repeat(64));
        let mut input = header(&name, &digest, "deno", 3);
        input.extend(b"abc");
        native_content(
            "--native-dependency-write",
            root,
            None,
            &mut input.as_slice(),
            &mut Vec::new(),
        )
        .unwrap();
        native_content(
            "--native-dependency-write",
            root,
            None,
            &mut input.as_slice(),
            &mut Vec::new(),
        )
        .unwrap();
        for (n, r, k) in [
            ("../escaped", digest.as_str(), "deno"),
            (name.as_str(), "wrong", "deno"),
            (name.as_str(), digest.as_str(), "cargo"),
        ] {
            let mut value = header(n, r, k, 3);
            value.extend(b"abc");
            assert!(
                native_content(
                    "--native-dependency-write",
                    root,
                    None,
                    &mut value.as_slice(),
                    &mut Vec::new()
                )
                .is_err()
            );
        }
        let mut changed = header(&name, &digest, "deno", 3);
        changed.extend(b"abd");
        assert!(
            native_content(
                "--native-dependency-write",
                root,
                None,
                &mut changed.as_slice(),
                &mut Vec::new()
            )
            .is_err()
        );
        fs::write(root.join(".elitea-dispatch"), b"").unwrap();
        let value = header("elitea-native-bundle-v2.json", &digest, "deno", 0);
        assert!(
            native_content(
                "--native-dependency-finalize",
                root,
                None,
                &mut value.as_slice(),
                &mut Vec::new()
            )
            .is_err()
        );
    }

    fn execution_request(revision: u64, kind: &str, digest: &str) -> serde_json::Value {
        let mut request = serde_json::json!({
            "revision": revision,
            "dependency_bundle_sha256": digest,
            "native_dependencies": {"kind": kind},
        });
        if revision == 4 {
            request["workspace"] = workspace_binding();
        }
        if revision == 5 {
            request["platform_client"] = serde_json::json!({
                "revision": 1,
                "policy_sha256": "c".repeat(64),
                "max_calls": 8,
                "max_total_bytes": 1048576,
            });
        }
        request
    }

    fn workspace_binding() -> serde_json::Value {
        serde_json::json!({
            "revision": 1,
            "selection": {
                "toolkit_id": 7,
                "toolkit_reference_sha256": "c".repeat(64),
                "repository_id": "fixture-repository",
                "commit": "d".repeat(40),
                "mode": "read",
                "include": ["src"],
            },
            "manifest_sha256": "e".repeat(64),
            "policy_sha256": "f".repeat(64),
        })
    }

    fn execution_frame(kind: &str, root: &str, uid: &str, request_digest: &str) -> Vec<u8> {
        let mut frame = serde_json::to_vec(&serde_json::json!({
            "revision": 2,
            "kind": kind,
            "root": root,
            "name": format!("{}.blob", "b".repeat(64)),
            "bytes": 3,
            "pod_uid": uid,
            "request_digest": request_digest,
        }))
        .unwrap();
        frame.push(b'\n');
        frame
    }

    #[test]
    fn native_execution_hydrates_plain_workspace_and_broker_revisions() {
        let digest = "a".repeat(64);
        let request_digest = "9".repeat(64);
        for kind in ["deno", "cargo"] {
            for (revision, workspace) in [(3, false), (4, true), (5, false), (5, true)] {
                let dir = tempfile::tempdir().unwrap();
                let mut request = execution_request(revision, kind, &digest);
                if workspace {
                    request["workspace"] = workspace_binding();
                }
                let request_bytes = serde_json::to_vec(&request).unwrap();
                fs::write(dir.path().join(".elitea-code.json"), &request_bytes).unwrap();
                let frame = execution_frame(kind, &digest, "original", &request_digest);
                let mut write_frame = frame.clone();
                write_frame.extend_from_slice(b"abc");
                let mut output = Vec::new();
                let identity = Some(("original", request_digest.as_str()));
                native_content(
                    "--native-dependency-write",
                    dir.path(),
                    identity,
                    &mut write_frame.as_slice(),
                    &mut output,
                )
                .unwrap();
                assert!(output.is_empty());
                native_content(
                    "--native-execution-dependency-read",
                    dir.path(),
                    identity,
                    &mut frame.as_slice(),
                    &mut output,
                )
                .unwrap();
                assert_eq!(output, b"abc");
                assert_eq!(
                    fs::read(dir.path().join(".elitea-code.json")).unwrap(),
                    request_bytes
                );
                assert!(!dir.path().join(".elitea-dispatch").exists());
            }
        }
    }

    #[test]
    fn native_execution_refuses_invalid_extensions_before_content_publication() {
        let digest = "a".repeat(64);
        let mut invalid_requests = Vec::new();
        let mut legacy_extension = execution_request(3, "deno", &digest);
        legacy_extension["platform_client"] = serde_json::json!({});
        invalid_requests.push(legacy_extension);
        let mut missing_workspace = execution_request(4, "deno", &digest);
        missing_workspace
            .as_object_mut()
            .unwrap()
            .remove("workspace");
        invalid_requests.push(missing_workspace);
        let mut unsafe_workspace = execution_request(4, "deno", &digest);
        unsafe_workspace["workspace"]["selection"]["include"] = serde_json::json!(["../escape"]);
        invalid_requests.push(unsafe_workspace);
        let mut null_workspace = execution_request(4, "deno", &digest);
        null_workspace["workspace"] = serde_json::Value::Null;
        invalid_requests.push(null_workspace);
        let mut missing_broker = execution_request(5, "deno", &digest);
        missing_broker
            .as_object_mut()
            .unwrap()
            .remove("platform_client");
        invalid_requests.push(missing_broker);
        let mut invalid_broker = execution_request(5, "deno", &digest);
        invalid_broker["platform_client"]["max_calls"] = serde_json::json!(0);
        invalid_requests.push(invalid_broker);
        let mut unknown_broker_field = execution_request(5, "deno", &digest);
        unknown_broker_field["platform_client"]["credential"] = serde_json::json!("fixture-only");
        invalid_requests.push(unknown_broker_field);
        let mut invalid_combination = execution_request(5, "deno", &digest);
        invalid_combination["workspace"] = serde_json::Value::Null;
        invalid_requests.push(invalid_combination);
        let mut non_native = execution_request(5, "deno", &digest);
        non_native
            .as_object_mut()
            .unwrap()
            .remove("native_dependencies");
        invalid_requests.push(non_native);
        invalid_requests.push(execution_request(6, "deno", &digest));
        let request_digest = "9".repeat(64);
        for request in invalid_requests {
            let dir = tempfile::tempdir().unwrap();
            fs::write(
                dir.path().join(".elitea-code.json"),
                serde_json::to_vec(&request).unwrap(),
            )
            .unwrap();
            let mut frame = execution_frame("deno", &digest, "original", &request_digest);
            frame.extend_from_slice(b"abc");
            let mut output = Vec::new();
            assert!(
                native_content(
                    "--native-dependency-write",
                    dir.path(),
                    Some(("original", request_digest.as_str())),
                    &mut frame.as_slice(),
                    &mut output,
                )
                .is_err()
            );
            assert!(output.is_empty());
            assert!(!dir.path().join("native-bundle").exists());
        }
    }

    #[test]
    fn native_execution_extensions_preserve_bundle_kind_and_runtime_identity_fences() {
        let digest = "a".repeat(64);
        let request_digest = "9".repeat(64);
        for revision in [3, 4, 5] {
            for (kind, root, uid, supplied_digest) in [
                ("cargo", digest.clone(), "original", request_digest.clone()),
                ("deno", "b".repeat(64), "original", request_digest.clone()),
                (
                    "deno",
                    digest.clone(),
                    "replacement",
                    request_digest.clone(),
                ),
                ("deno", digest.clone(), "original", "8".repeat(64)),
            ] {
                let dir = tempfile::tempdir().unwrap();
                fs::write(
                    dir.path().join(".elitea-code.json"),
                    serde_json::to_vec(&execution_request(revision, "deno", &digest)).unwrap(),
                )
                .unwrap();
                let mut frame = execution_frame(kind, &root, uid, &supplied_digest);
                frame.extend_from_slice(b"abc");
                let mut output = Vec::new();
                assert!(
                    native_content(
                        "--native-dependency-write",
                        dir.path(),
                        Some(("original", request_digest.as_str())),
                        &mut frame.as_slice(),
                        &mut output,
                    )
                    .is_err()
                );
                assert!(output.is_empty());
                assert!(!dir.path().join("native-bundle").exists());
            }
        }
    }
}

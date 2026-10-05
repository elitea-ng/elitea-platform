//! Fixed repository hydration. This helper never interprets user code or credentials.
use crate::workspace_content::{WorkspaceManifest, WorkspaceMode};
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};
use std::os::unix::fs::PermissionsExt as _;
use std::{
    collections::BTreeSet,
    fs::{self, File, OpenOptions},
    io::{self, Read, Write},
    path::{Path, PathBuf},
};

const ROOT: &str = "/workspace/repository";
const MANIFEST: &str = ".elitea-workspace-manifest.json";
const PROGRESS: &str = ".elitea-workspace-progress";
const READY: &str = ".elitea-workspace-ready";
const RELEASE: &str = ".elitea-workspace-release";
const LOCK: &str = ".elitea-workspace-lock";
const TEMP: &str = ".elitea-workspace-file";
const HEADER_LIMIT: usize = 4096;

#[derive(Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
struct Header {
    revision: u8,
    job_key: String,
    request_digest: String,
    manifest_sha256: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pod_uid: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    index: Option<u32>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    bytes: Option<u64>,
}
#[derive(Serialize)]
struct Probe {
    revision: u8,
    next_index: u32,
    ready: bool,
}

struct Identity<'a> {
    job: &'a str,
    request: &'a str,
    root: &'a str,
    pod: Option<&'a str>,
}
fn invalid() -> io::Error {
    io::Error::other("Code workspace hydration identity or content is invalid")
}
fn valid_digest(value: &str) -> bool {
    value.len() == 64
        && value
            .bytes()
            .all(|v| v.is_ascii_digit() || (b'a'..=b'f').contains(&v))
}

fn header(input: &mut impl Read, identity: &Identity<'_>) -> io::Result<Header> {
    let mut line = Vec::with_capacity(HEADER_LIMIT);
    let mut byte = [0];
    loop {
        if line.len() == HEADER_LIMIT {
            return Err(invalid());
        };
        input.read_exact(&mut byte)?;
        if byte[0] == b'\n' {
            break;
        };
        line.push(byte[0]);
    }
    let value: Header = serde_json::from_slice(&line).map_err(|_| invalid())?;
    if serde_json::to_vec(&value).map_err(|_| invalid())? != line
        || value.revision != 1
        || value.job_key != identity.job
        || value.request_digest != identity.request
        || value.manifest_sha256 != identity.root
        || value.pod_uid.as_deref() != identity.pod
        || !valid_digest(identity.job)
        || !valid_digest(identity.request)
        || !valid_digest(identity.root)
        || identity
            .pod
            .is_some_and(|v| v.is_empty() || v.len() > 512 || v.chars().any(char::is_whitespace))
    {
        return Err(invalid());
    };
    Ok(value)
}
fn eof(input: &mut impl Read) -> io::Result<()> {
    let mut byte = [0];
    match input.read(&mut byte)? {
        0 => Ok(()),
        _ => Err(invalid()),
    }
}
fn regular(path: &Path) -> io::Result<fs::Metadata> {
    let m = fs::symlink_metadata(path)?;
    if !m.file_type().is_file() || m.file_type().is_symlink() {
        return Err(invalid());
    };
    Ok(m)
}
fn directory(path: &Path) -> io::Result<()> {
    let m = fs::symlink_metadata(path)?;
    if !m.file_type().is_dir() || m.file_type().is_symlink() {
        return Err(invalid());
    };
    Ok(())
}
fn read_bounded(path: &Path, limit: u64) -> io::Result<Vec<u8>> {
    let metadata = regular(path)?;
    if metadata.len() > limit {
        return Err(invalid());
    };
    let mut out = Vec::new();
    File::open(path)?.take(limit + 1).read_to_end(&mut out)?;
    if out.len() as u64 != metadata.len() {
        return Err(invalid());
    };
    Ok(out)
}
fn existing_equal(path: &Path, value: &[u8]) -> io::Result<bool> {
    match read_bounded(path, value.len() as u64) {
        Ok(bytes) if bytes == value => Ok(true),
        Ok(_) => Err(invalid()),
        Err(e) if e.kind() == io::ErrorKind::NotFound => Ok(false),
        Err(e) => Err(e),
    }
}
fn immutable_file(path: &Path, value: &[u8]) -> io::Result<()> {
    if existing_equal(path, value)? {
        return Ok(());
    }
    let name = path
        .file_name()
        .and_then(|v| v.to_str())
        .ok_or_else(invalid)?;
    let temporary = path.with_file_name(format!("{name}.pending"));
    if temporary.exists() {
        regular(&temporary)?;
        fs::remove_file(&temporary)?;
    }
    let outcome = (|| {
        let mut file = OpenOptions::new()
            .write(true)
            .create_new(true)
            .open(&temporary)?;
        file.write_all(value)?;
        file.sync_all()?;
        match fs::hard_link(&temporary, path) {
            Ok(()) => {}
            Err(e) if e.kind() == io::ErrorKind::AlreadyExists => {
                if !existing_equal(path, value)? {
                    return Err(invalid());
                }
            }
            Err(e) => return Err(e),
        }
        File::open(path.parent().ok_or_else(invalid)?)?.sync_all()
    })();
    let _ = fs::remove_file(temporary);
    outcome
}
fn replace_metadata(root: &Path, name: &str, value: &[u8]) -> io::Result<()> {
    let target = root.join(name);
    if target.exists() {
        regular(&target)?;
    }
    let temporary = root.join(format!("{name}.tmp"));
    if temporary.exists() {
        regular(&temporary)?;
        fs::remove_file(&temporary)?;
    }
    immutable_file(&temporary, value)?;
    fs::rename(&temporary, &target)?;
    File::open(root)?.sync_all()
}
fn load(root: &Path, identity: &Identity<'_>) -> io::Result<WorkspaceManifest> {
    let bytes = read_bounded(&root.join(MANIFEST), 4 << 20)?;
    let manifest =
        WorkspaceManifest::from_bound_transport(&bytes, identity.root).map_err(|_| invalid())?;
    if manifest.selection().mode != WorkspaceMode::Read {
        return Err(invalid());
    };
    Ok(manifest)
}
fn progress(root: &Path, count: usize) -> io::Result<usize> {
    match read_bounded(&root.join(PROGRESS), 16) {
        Ok(bytes) => {
            let text = std::str::from_utf8(&bytes).map_err(|_| invalid())?;
            let n = text.parse::<usize>().map_err(|_| invalid())?;
            if n > count || n.to_string() != text {
                return Err(invalid());
            };
            Ok(n)
        }
        Err(e) if e.kind() == io::ErrorKind::NotFound => Ok(0),
        Err(e) => Err(e),
    }
}
fn ready(root: &Path, identity: &Identity<'_>) -> io::Result<bool> {
    existing_equal(&root.join(READY), identity.root.as_bytes())
}
fn destination(root: &Path, path: &str) -> io::Result<PathBuf> {
    directory(root)?;
    let mut current = root.to_owned();
    let mut parts = path.split('/').peekable();
    while let Some(part) = parts.next() {
        current.push(part);
        if parts.peek().is_some() {
            match fs::symlink_metadata(&current) {
                Ok(_) => directory(&current)?,
                Err(e) if e.kind() == io::ErrorKind::NotFound => {
                    fs::create_dir(&current)?;
                    directory(&current)?
                }
                Err(e) => return Err(e),
            }
        }
    }
    Ok(current)
}
fn verify_file(path: &Path, bytes: u64, expected: &str, executable: bool) -> io::Result<()> {
    let metadata = regular(path)?;
    if metadata.len() != bytes
        || metadata.permissions().mode() & 0o7777 != if executable { 0o700 } else { 0o600 }
    {
        return Err(invalid());
    };
    let mut file = File::open(path)?;
    let mut hash = Sha256::new();
    let mut buffer = [0u8; 64 << 10];
    let mut total = 0u64;
    loop {
        let n = file.read(&mut buffer)?;
        if n == 0 {
            break;
        };
        total = total.checked_add(n as u64).ok_or_else(invalid)?;
        if total > bytes {
            return Err(invalid());
        };
        hash.update(&buffer[..n]);
    }
    if total != bytes || format!("{:x}", hash.finalize()) != expected {
        return Err(invalid());
    };
    Ok(())
}
fn write_file(
    root: &Path,
    identity: &Identity<'_>,
    header: &Header,
    input: &mut impl Read,
) -> io::Result<()> {
    if ready(root, identity)? {
        return Err(invalid());
    }
    let manifest = load(root, identity)?;
    let index = usize::try_from(header.index.ok_or_else(invalid)?).map_err(|_| invalid())?;
    let entry = manifest.files().get(index).ok_or_else(invalid)?;
    if header.bytes != Some(entry.bytes) || index > progress(root, manifest.files().len())? {
        return Err(invalid());
    }
    let temporary = root.join(TEMP);
    if temporary.exists() {
        regular(&temporary)?;
        fs::remove_file(&temporary)?;
    }
    let outcome = (|| {
        let mut file = OpenOptions::new()
            .write(true)
            .create_new(true)
            .open(&temporary)?;
        let mut remaining = entry.bytes;
        let mut hash = Sha256::new();
        let mut buffer = [0u8; 64 << 10];
        while remaining > 0 {
            let n = usize::try_from(remaining.min(buffer.len() as u64)).map_err(|_| invalid())?;
            input.read_exact(&mut buffer[..n])?;
            file.write_all(&buffer[..n])?;
            hash.update(&buffer[..n]);
            remaining -= n as u64;
        }
        eof(input)?;
        if format!("{:x}", hash.finalize()) != entry.sha256 {
            return Err(invalid());
        };
        file.sync_all()?;
        file.set_permissions(fs::Permissions::from_mode(if entry.executable {
            0o700
        } else {
            0o600
        }))?;
        let target = destination(root, &entry.path)?;
        match fs::hard_link(&temporary, &target) {
            Ok(()) => {}
            Err(e) if e.kind() == io::ErrorKind::AlreadyExists => {
                verify_file(&target, entry.bytes, &entry.sha256, entry.executable)?
            }
            Err(e) => return Err(e),
        }
        File::open(target.parent().ok_or_else(invalid)?)?.sync_all()?;
        let next = progress(root, manifest.files().len())?.max(index + 1);
        replace_metadata(root, PROGRESS, next.to_string().as_bytes())
    })();
    let _ = fs::remove_file(&temporary);
    outcome
}
fn verify_tree(root: &Path, manifest: &WorkspaceManifest) -> io::Result<()> {
    let mut files = BTreeSet::new();
    let mut dirs = BTreeSet::new();
    for entry in manifest.files() {
        files.insert(entry.path.clone());
        let mut parts = Path::new(&entry.path).parent();
        while let Some(path) = parts {
            if path.as_os_str().is_empty() {
                break;
            };
            dirs.insert(path.to_owned());
            parts = path.parent();
        }
    }
    let mut pending = vec![PathBuf::new()];
    let mut seen = 0usize;
    let limit = files.len() + dirs.len() + 7;
    while let Some(relative) = pending.pop() {
        for value in fs::read_dir(root.join(&relative))? {
            let entry = value?;
            seen += 1;
            if seen > limit {
                return Err(invalid());
            };
            let path = relative.join(entry.file_name());
            let metadata = fs::symlink_metadata(entry.path())?;
            if metadata.file_type().is_symlink() {
                return Err(invalid());
            }
            if metadata.is_dir() {
                if !dirs.contains(&path) {
                    return Err(invalid());
                };
                pending.push(path)
            } else if metadata.is_file() {
                let text = path.to_str().ok_or_else(invalid)?;
                if !files.contains(text)
                    && !(relative.as_os_str().is_empty()
                        && [MANIFEST, PROGRESS, LOCK, READY, RELEASE].contains(&text))
                {
                    return Err(invalid());
                }
            } else {
                return Err(invalid());
            }
        }
    }
    for entry in manifest.files() {
        verify_file(
            &root.join(&entry.path),
            entry.bytes,
            &entry.sha256,
            entry.executable,
        )?
    }
    Ok(())
}
fn apply(
    command: &str,
    root: &Path,
    identity: &Identity<'_>,
    header: Header,
    input: &mut impl Read,
    output: &mut impl Write,
) -> io::Result<()> {
    match command {
        "--workspace-manifest" => {
            if header.index.is_some() || header.bytes.is_none_or(|n| n == 0 || n > 4 << 20) {
                return Err(invalid());
            };
            let n = header.bytes.ok_or_else(invalid)?;
            let mut bytes = Vec::new();
            input.take(n + 1).read_to_end(&mut bytes)?;
            if bytes.len() as u64 != n {
                return Err(invalid());
            };
            eof(input)?;
            let manifest = WorkspaceManifest::from_bound_transport(&bytes, identity.root)
                .map_err(|_| invalid())?;
            if manifest.selection().mode != WorkspaceMode::Read
                || bytes.len() > manifest.policy().max_manifest_bytes as usize
            {
                return Err(invalid());
            };
            immutable_file(&root.join(MANIFEST), &bytes)
        }
        "--workspace-write" => write_file(root, identity, &header, input),
        "--workspace-probe" => {
            if header.index.is_some() || header.bytes.is_some() {
                return Err(invalid());
            };
            eof(input)?;
            let manifest = load(root, identity)?;
            let next = progress(root, manifest.files().len())?;
            // The returned cursor acknowledges actual immutable original files.
            for entry in manifest.files().iter().take(next) {
                verify_file(
                    &root.join(&entry.path),
                    entry.bytes,
                    &entry.sha256,
                    entry.executable,
                )?;
            }
            if ready(root, identity)? {
                verify_tree(root, &manifest)?;
            }
            serde_json::to_writer(
                output,
                &Probe {
                    revision: 1,
                    next_index: u32::try_from(next).map_err(|_| invalid())?,
                    ready: ready(root, identity)?,
                },
            )
            .map_err(|_| invalid())
        }
        "--workspace-finalize" | "--workspace-release" => {
            if header.index.is_some() || header.bytes.is_some() {
                return Err(invalid());
            };
            eof(input)?;
            let manifest = load(root, identity)?;
            if progress(root, manifest.files().len())? != manifest.files().len() {
                return Err(invalid());
            };
            verify_tree(root, &manifest)?;
            immutable_file(&root.join(READY), manifest.root().as_bytes())?;
            if command == "--workspace-release" {
                immutable_file(&root.join(RELEASE), identity.root.as_bytes())?
            };
            Ok(())
        }
        _ => Err(invalid()),
    }
}

// Entry verification occurs before any user code or repository compiler read.
// Prepared request authority remains the caller's existing owner.
// Language entrypoints call this after PID-1 completes hydration.
#[allow(dead_code)]
pub(crate) fn verify_workspace_entry(binding: &serde_json::Value) -> io::Result<()> {
    let binding: crate::workspace_content::WorkspaceBinding =
        serde_json::from_value(binding.clone()).map_err(|_| invalid())?;
    let root = Path::new(ROOT);
    directory(root)?;
    let manifest = load(
        root,
        &Identity {
            job: "",
            request: "",
            root: &binding.manifest_sha256,
            pod: None,
        },
    )?;
    if !binding.matches(&manifest).map_err(|_| invalid())?
        || !existing_equal(&root.join(READY), manifest.root().as_bytes())?
        || !existing_equal(&root.join(RELEASE), manifest.root().as_bytes())?
    {
        return Err(invalid());
    }
    verify_tree(root, &manifest)?;
    let mut mountinfo = Vec::new();
    File::open("/proc/self/mountinfo")?
        .take(1024 * 1024 + 1)
        .read_to_end(&mut mountinfo)?;
    if !read_only_mount(&mountinfo) {
        return Err(invalid());
    };
    Ok(())
}

// Only the language entrypoint's retained workspace check reads mount metadata.
#[allow(dead_code)]
fn read_only_mount(bytes: &[u8]) -> bool {
    if bytes.len() > 1024 * 1024 {
        return false;
    };
    let Ok(text) = std::str::from_utf8(bytes) else {
        return false;
    };
    let mut found = false;
    for line in text.lines() {
        if line.len() > 4096 {
            return false;
        };
        let fields = line.split_whitespace().collect::<Vec<_>>();
        if fields.len() < 10 || !fields.contains(&"-") {
            return false;
        };
        let path = fields[4];
        if path == ROOT {
            if found || !fields[5].split(',').any(|v| v == "ro") {
                return false;
            };
            found = true;
        } else if path.starts_with(&format!("{ROOT}/")) {
            return false;
        }
    }
    found
}

fn cleanup_pending(root: &Path) -> io::Result<()> {
    for name in [
        TEMP,
        ".elitea-workspace-manifest.json.pending",
        ".elitea-workspace-progress.tmp",
        ".elitea-workspace-progress.tmp.pending",
        ".elitea-workspace-ready.pending",
        ".elitea-workspace-release.pending",
    ] {
        let path = root.join(name);
        match fs::symlink_metadata(&path) {
            Ok(_) => {
                regular(&path)?;
                fs::remove_file(path)?
            }
            Err(e) if e.kind() == io::ErrorKind::NotFound => {}
            Err(e) => return Err(e),
        }
    }
    Ok(())
}

pub fn run(command: &str) -> io::Result<()> {
    let job = std::env::var("ELITEA_CODE_WORKSPACE_JOB_KEY").map_err(|_| invalid())?;
    let request = std::env::var("ELITEA_CODE_WORKSPACE_REQUEST_DIGEST").map_err(|_| invalid())?;
    let root_digest = std::env::var("ELITEA_CODE_WORKSPACE_ROOT_SHA256").map_err(|_| invalid())?;
    let uid = std::env::var("ELITEA_SANDBOX_POD_UID").ok();
    let identity = Identity {
        job: &job,
        request: &request,
        root: &root_digest,
        pod: uid.as_deref(),
    };
    let root = Path::new(ROOT);
    directory(root)?;
    let lock_path = root.join(LOCK);
    if lock_path.exists() {
        regular(&lock_path)?;
    }
    let lock = OpenOptions::new()
        .read(true)
        .write(true)
        .create(true)
        .truncate(false)
        .open(lock_path)?;
    lock.lock()?;
    cleanup_pending(root)?;
    let mut input = io::stdin().lock();
    let header = header(&mut input, &identity)?;
    apply(
        command,
        root,
        &identity,
        header,
        &mut input,
        &mut io::stdout().lock(),
    )
}

#[cfg(test)]
#[path = "workspace_lifecycle_tests.rs"]
mod tests;

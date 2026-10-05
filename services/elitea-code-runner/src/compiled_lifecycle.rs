//! Fixed snapshot transfer roles. Supervisor admission and grants remain external.
use crate::compiled_code::{self as contract, ContentSha256, Control, Purpose};
use serde::Deserialize;
use std::{
    fs,
    io::{self, Read, Write},
    path::Path,
};

#[derive(Clone, Copy, Deserialize, PartialEq, Eq)]
#[serde(rename_all = "snake_case")]
enum Role {
    Control,
    Descriptor,
    Executable,
    Status,
    Finalize,
    Release,
}
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct Header {
    revision: u8,
    snapshot_key_sha256: ContentSha256,
    role: Role,
    bytes: u64,
    #[serde(default, deserialize_with = "contract::non_null_sha256")]
    sha256: Option<ContentSha256>,
    #[serde(default, deserialize_with = "contract::non_null_string")]
    pod_uid: Option<String>,
    #[serde(default, deserialize_with = "contract::non_null_string")]
    request_digest: Option<String>,
}
fn header(input: &mut impl Read, expected: Option<(&str, &str)>) -> io::Result<Header> {
    let mut bytes = Vec::new();
    let mut byte = [0u8; 1];
    loop {
        if bytes.len() >= 4096 || input.read(&mut byte)? != 1 {
            return Err(contract::invalid());
        }
        bytes.push(byte[0]);
        if byte[0] == b'\n' {
            break;
        }
    }
    let value: Header = serde_json::from_slice(&bytes).map_err(|_| contract::invalid())?;
    if value.revision != 1 {
        return Err(contract::invalid());
    }
    match (
        expected,
        value.pod_uid.as_deref(),
        value.request_digest.as_deref(),
    ) {
        (None, None, None) => {}
        (Some((uid, digest)), Some(actual_uid), Some(actual_digest))
            if uid == actual_uid && digest == actual_digest => {}
        _ => return Err(contract::invalid()),
    }
    Ok(value)
}
fn eof(input: &mut impl Read) -> io::Result<()> {
    let mut byte = [0u8; 1];
    if input.read(&mut byte)? != 0 {
        return Err(contract::invalid());
    }
    Ok(())
}
fn empty(header: &Header, input: &mut impl Read, role: Role) -> io::Result<()> {
    if header.role != role || header.bytes != 0 || header.sha256.is_some() {
        return Err(contract::invalid());
    }
    eof(input)
}
fn import(
    input: &mut impl Read,
    header: &Header,
    path: &Path,
    limit: u64,
    expected: &ContentSha256,
    executable: bool,
) -> io::Result<()> {
    if header.bytes == 0 || header.bytes > limit || header.sha256.as_ref() != Some(expected) {
        return Err(contract::invalid());
    }
    // One bounded file, never an archive or caller path. No publication on partial input.
    let mut bytes = Vec::with_capacity(header.bytes as usize);
    (&mut *input).take(header.bytes).read_to_end(&mut bytes)?;
    if bytes.len() as u64 != header.bytes {
        return Err(contract::invalid());
    }
    eof(input)?;
    if ContentSha256::of(&bytes) != *expected {
        return Err(contract::invalid());
    }
    contract::immutable_file(path, &bytes, executable)
}
fn undispatched(root: &Path) -> io::Result<()> {
    if root.join(".elitea-dispatch").exists() {
        return Err(contract::invalid());
    }
    Ok(())
}
fn base(root: &Path) -> io::Result<std::path::PathBuf> {
    contract::regular_directory(root)?;
    let base = root.join(contract::DIRECTORY);
    match fs::create_dir(&base) {
        Ok(()) => {}
        Err(error) if error.kind() == io::ErrorKind::AlreadyExists => {}
        Err(error) => return Err(error),
    }
    contract::regular_directory(&base)?;
    Ok(base)
}
fn captured(root: &Path, control: &Control) -> io::Result<contract::Descriptor> {
    compile_export_quiescent()?;
    let descriptor = contract::load_descriptor(root, control)?;
    let bytes = contract::read_regular(
        &root.join(contract::DIRECTORY).join(contract::DESCRIPTOR),
        contract::DESCRIPTOR_LIMIT,
    )?;
    if contract::read_regular(&root.join(contract::CAPTURED), 64)?
        != ContentSha256::of(&bytes).as_str().as_bytes()
    {
        return Err(contract::invalid());
    }
    Ok(descriptor)
}
fn compile_export_quiescent() -> io::Result<()> {
    #[cfg(target_os = "linux")]
    {
        // Workspace markers are forgeable by build scripts. Require a quiet PID
        // namespace as well: the image-owned PID 1 runner, its one fixed Rust
        // compilation adapter, and this fixed-role helper. No compiler, build
        // script, escaped process group or unreaped zombie may remain.
        if fs::read_link("/proc/1/exe")? != Path::new("/usr/local/bin/elitea-code-runner") {
            return Err(contract::invalid());
        }
        let current = std::process::id();
        let mut bytes = Vec::new();
        fs::File::open("/proc/1/task/1/children")?
            .take(4097)
            .read_to_end(&mut bytes)?;
        if bytes.len() > 4096 {
            return Err(contract::invalid());
        }
        let text = std::str::from_utf8(&bytes).map_err(|_| contract::invalid())?;
        let children: Vec<u32> = text
            .split_ascii_whitespace()
            .map(|pid| pid.parse().map_err(|_| contract::invalid()))
            .collect::<io::Result<_>>()?;
        let children: Vec<u32> = children.into_iter().filter(|pid| *pid != current).collect();
        let [owner] = children.as_slice() else {
            return Err(contract::invalid());
        };
        if *owner == 0
            || *owner == 1
            || fs::read_link(format!("/proc/{owner}/exe"))?
                != Path::new("/usr/local/bin/elitea-code-rust")
        {
            return Err(contract::invalid());
        }
        let mut argv = Vec::new();
        fs::File::open(format!("/proc/{owner}/cmdline"))?
            .take(4097)
            .read_to_end(&mut argv)?;
        if argv != b"/usr/local/bin/elitea-code-rust\0--compile-snapshot\0" {
            return Err(contract::invalid());
        }
        let mut count = 0usize;
        for entry in fs::read_dir("/proc")? {
            let entry = entry?;
            let name = entry.file_name();
            let Some(name) = name.to_str() else { continue };
            let Ok(pid) = name.parse::<u32>() else {
                continue;
            };
            count += 1;
            if count > 128 || (pid != 1 && pid != current && pid != *owner) {
                return Err(contract::invalid());
            }
        }
        Ok(())
    }
    #[cfg(not(target_os = "linux"))]
    {
        Err(contract::invalid())
    }
}
pub(crate) fn execution_ready(root: &Path) -> io::Result<()> {
    let purpose = contract::enabled_purpose()?;
    let control = contract::load_control(root, purpose)?;
    if purpose == Purpose::Compile {
        return Ok(());
    }
    let sha = control
        .descriptor_sha256
        .as_ref()
        .ok_or_else(contract::invalid)?;
    if contract::read_regular(&root.join(contract::READY), 64)? != sha.as_str().as_bytes() {
        return Err(contract::invalid());
    }
    contract::load_descriptor(root, &control)?;
    Ok(())
}
pub(crate) fn run(
    command: &str,
    root: &Path,
    expected: Option<(&str, &str)>,
    input: &mut impl Read,
    output: &mut impl Write,
) -> io::Result<()> {
    let purpose = contract::enabled_purpose()?;
    contract::regular_directory(root)?;
    let header = header(input, expected)?;
    if command == "--compiled-control-write" {
        undispatched(root)?;
        if header.role != Role::Control {
            return Err(contract::invalid());
        }
        let pinned = ContentSha256::parse(
            contract::trusted_launch_value("ELITEA_COMPILED_CODE_CONTROL_SHA256")?
                .ok_or_else(contract::invalid)?,
        )?;
        // Parse and validate before making the auxiliary control visible.
        if header.bytes == 0
            || header.bytes > contract::DESCRIPTOR_LIMIT as u64
            || header.sha256.as_ref() != Some(&pinned)
        {
            return Err(contract::invalid());
        }
        let mut bytes = Vec::new();
        (&mut *input).take(header.bytes).read_to_end(&mut bytes)?;
        eof(input)?;
        if bytes.len() as u64 != header.bytes || ContentSha256::of(&bytes) != pinned {
            return Err(contract::invalid());
        }
        let control: Control = serde_json::from_slice(&bytes).map_err(|_| contract::invalid())?;
        control.validate(purpose)?;
        if serde_json::to_vec(&control).map_err(|_| contract::invalid())? != bytes {
            return Err(contract::invalid());
        }
        if control.snapshot_key_sha256 != header.snapshot_key_sha256 {
            return Err(contract::invalid());
        }
        return contract::immutable_file(&root.join(contract::CONTROL), &bytes, false);
    }
    let control = contract::load_control(root, purpose)?;
    if header.snapshot_key_sha256 != control.snapshot_key_sha256 {
        return Err(contract::invalid());
    }
    match command {
        "--compiled-artifact-write" if purpose == Purpose::Execute => {
            undispatched(root)?;
            if root.join(contract::READY).exists() {
                return Err(contract::invalid());
            }
            let base = base(root)?;
            match header.role {
                Role::Descriptor => {
                    let expected = control
                        .descriptor_sha256
                        .as_ref()
                        .ok_or_else(contract::invalid)?;
                    // JSON validation is required before descriptor publication, not only finalize.
                    if header.bytes == 0
                        || header.bytes > contract::DESCRIPTOR_LIMIT as u64
                        || header.sha256.as_ref() != Some(expected)
                    {
                        return Err(contract::invalid());
                    }
                    let mut bytes = Vec::new();
                    (&mut *input).take(header.bytes).read_to_end(&mut bytes)?;
                    eof(input)?;
                    if bytes.len() as u64 != header.bytes || ContentSha256::of(&bytes) != *expected
                    {
                        return Err(contract::invalid());
                    }
                    let descriptor: contract::Descriptor =
                        serde_json::from_slice(&bytes).map_err(|_| contract::invalid())?;
                    descriptor.validate(&control)?;
                    if descriptor.bytes()? != bytes {
                        return Err(contract::invalid());
                    }
                    contract::immutable_file(&base.join(contract::DESCRIPTOR), &bytes, false)
                }
                Role::Executable => {
                    let bytes = contract::read_regular(
                        &base.join(contract::DESCRIPTOR),
                        contract::DESCRIPTOR_LIMIT,
                    )?;
                    if ContentSha256::of(&bytes)
                        != *control
                            .descriptor_sha256
                            .as_ref()
                            .ok_or_else(contract::invalid)?
                    {
                        return Err(contract::invalid());
                    }
                    let descriptor: contract::Descriptor =
                        serde_json::from_slice(&bytes).map_err(|_| contract::invalid())?;
                    descriptor.validate(&control)?;
                    if header.bytes != descriptor.executable_bytes {
                        return Err(contract::invalid());
                    }
                    import(
                        input,
                        &header,
                        &base.join(contract::EXECUTABLE),
                        contract::EXECUTABLE_LIMIT,
                        &descriptor.executable_sha256,
                        true,
                    )
                }
                _ => Err(contract::invalid()),
            }
        }
        "--compiled-artifact-finalize" if purpose == Purpose::Execute => {
            undispatched(root)?;
            empty(&header, input, Role::Finalize)?;
            contract::load_descriptor(root, &control)?;
            let sha = control
                .descriptor_sha256
                .as_ref()
                .ok_or_else(contract::invalid)?;
            contract::immutable_file(&root.join(contract::READY), sha.as_str().as_bytes(), false)
        }
        "--compiled-artifact-status" if purpose == Purpose::Compile => {
            empty(&header, input, Role::Status)?;
            captured(root, &control)?;
            output.write_all(&contract::read_regular(
                &root.join(contract::DIRECTORY).join(contract::DESCRIPTOR),
                contract::DESCRIPTOR_LIMIT,
            )?)
        }
        "--compiled-artifact-read" if purpose == Purpose::Compile => {
            eof(input)?;
            let descriptor = captured(root, &control)?;
            let path = match header.role {
                Role::Descriptor => root.join(contract::DIRECTORY).join(contract::DESCRIPTOR),
                Role::Executable => root.join(contract::DIRECTORY).join(contract::EXECUTABLE),
                _ => return Err(contract::invalid()),
            };
            let (sha, bytes) = contract::hash_regular(
                &path,
                if header.role == Role::Descriptor {
                    contract::DESCRIPTOR_LIMIT as u64
                } else {
                    contract::EXECUTABLE_LIMIT
                },
            )?;
            if header.bytes != bytes
                || header.sha256.as_ref() != Some(&sha)
                || (header.role == Role::Executable && sha != descriptor.executable_sha256)
            {
                return Err(contract::invalid());
            }
            let mut file = contract::regular_file(&path, bytes)?;
            io::copy(&mut file, output)?;
            Ok(())
        }
        "--compiled-artifact-release" if purpose == Purpose::Compile => {
            empty(&header, input, Role::Release)?;
            captured(root, &control)?;
            contract::immutable_file(&root.join(contract::RELEASE), b"", false)
        }
        _ => Err(contract::invalid()),
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn roles_headers_identity_and_end_of_input_are_strict() {
        let key = "a".repeat(64);
        let valid = format!(
            "{{\"revision\":1,\"snapshot_key_sha256\":\"{key}\",\"role\":\"status\",\"bytes\":0}}\n"
        );
        let parsed = header(&mut valid.as_bytes(), None).unwrap();
        empty(&parsed, &mut b"".as_slice(), Role::Status).unwrap();
        for extra in [
            "\"path\":\"../escape\"",
            "\"argv\":[\"arbitrary\"]",
            "\"bytes\":0",
            "\"pod_uid\":\"wrong\"",
        ] {
            let bad = valid.replace("}\n", &format!(",{extra}}}\n"));
            assert!(header(&mut bad.as_bytes(), None).is_err());
        }
        assert!(empty(&parsed, &mut b"extra".as_slice(), Role::Status).is_err());
        assert!(header(&mut valid.as_bytes(), Some(("uid", &key))).is_err());
        let mut oversized = vec![b' '; 4096];
        oversized.push(b'\n');
        assert!(header(&mut oversized.as_slice(), None).is_err());
    }
    #[test]
    fn partial_excess_and_symlink_imports_do_not_publish() {
        let root = tempfile::tempdir().unwrap();
        let path = root.path().join("fixed");
        let sha = ContentSha256::of(b"abc");
        let frame = Header {
            revision: 1,
            snapshot_key_sha256: sha.clone(),
            role: Role::Executable,
            bytes: 3,
            sha256: Some(sha.clone()),
            pod_uid: None,
            request_digest: None,
        };
        for input in [b"ab".as_slice(), b"abcd"] {
            let mut reader = input;
            assert!(
                import(
                    &mut reader,
                    &frame,
                    &path,
                    contract::EXECUTABLE_LIMIT,
                    &sha,
                    true
                )
                .is_err()
            );
            assert!(!path.exists());
        }
        std::os::unix::fs::symlink(root.path().join("other"), &path).unwrap();
        assert!(
            import(
                &mut b"abc".as_slice(),
                &frame,
                &path,
                contract::EXECUTABLE_LIMIT,
                &sha,
                true
            )
            .is_err()
        );
        assert!(!root.path().join("other").exists());
    }
}

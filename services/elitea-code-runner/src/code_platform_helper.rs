//! Fixed mailbox commands. Supervisor verifies the exact runtime before and after.
use super::{
    code_platform_launch::{self, LaunchMetadata},
    code_platform_mailbox::{Mailbox, MailboxIdentity},
};
use sha2::{Digest, Sha256};
use std::io::{self, Read, Write};
const REPLY_LIMIT: usize = 8 + 4096 + 8 + 2_097_152 + 65_536;
fn refused() -> io::Error {
    io::Error::other("Code platform runtime helper refused")
}

fn observed_runtime(metadata: &LaunchMetadata) -> io::Result<()> {
    if let Ok(uid) = std::env::var("ELITEA_SANDBOX_POD_UID") {
        let request = std::env::var("ELITEA_SANDBOX_REQUEST").map_err(|_| refused())?;
        if !metadata.retained_runtime_id.starts_with("kube:")
            || metadata.retained_runtime_id.split(':').count() != 4
            || metadata.retained_runtime_id.rsplit(':').next() != Some(uid.as_str())
            || request
                != metadata
                    .request_digest
                    .as_deref()
                    .unwrap_or(&metadata.prepared_sha256)
        {
            return Err(refused());
        }
    } else {
        // Docker's full immutable ID is rechecked outside this helper. Its fixed
        // kernel-mounted hostname supplies the in-container identity cross-check.
        let hostname = code_platform_launch::read_fixed("/etc/hostname", 256)?;
        let hostname = std::str::from_utf8(&hostname)
            .map_err(|_| refused())?
            .trim_end_matches('\n');
        if hostname.len() != 12
            || !hostname
                .bytes()
                .all(|v| v.is_ascii_digit() || (b'a'..=b'f').contains(&v))
            || metadata.retained_runtime_id.len() != 64
            || !metadata.retained_runtime_id.starts_with(hostname)
        {
            return Err(refused());
        }
    }
    let bytes = code_platform_launch::read_fixed("/workspace/.elitea-code.json", 1024 * 1024)?;
    let mut hash = Sha256::new();
    hash.update(b"elitea.sandbox.prepared-job.v1\0");
    hash.update((bytes.len() as u64).to_be_bytes());
    hash.update(bytes);
    let actual: [u8; 32] = hash.finalize().into();
    if actual != code_platform_launch::digest(&metadata.prepared_sha256)? {
        return Err(refused());
    }
    Ok(())
}
fn input<R: Read>(reader: &mut R) -> io::Result<(Vec<u8>, Vec<u8>)> {
    let mut lengths = [0; 8];
    reader.read_exact(&mut lengths)?;
    let header = u32::from_be_bytes(lengths[..4].try_into().map_err(|_| refused())?) as usize;
    let body = u32::from_be_bytes(lengths[4..].try_into().map_err(|_| refused())?) as usize;
    if header == 0 || header > 4096 || body > REPLY_LIMIT {
        return Err(refused());
    }
    let mut metadata = vec![0; header];
    reader.read_exact(&mut metadata)?;
    let mut reply = vec![0; body];
    reader.read_exact(&mut reply)?;
    let mut trailing = [0];
    if reader.read(&mut trailing)? != 0 {
        return Err(refused());
    }
    Ok((metadata, reply))
}
fn bind(raw: &[u8]) -> io::Result<()> {
    use rustix::fs::{self, Mode, OFlags, RenameFlags};
    let root = fs::open(
        "/workspace",
        OFlags::RDONLY | OFlags::DIRECTORY | OFlags::NOFOLLOW | OFlags::CLOEXEC,
        Mode::empty(),
    )?;
    match code_platform_launch::read_fixed(code_platform_launch::LAUNCH_PATH, 4096) {
        Ok(saved) => return if saved == raw { Ok(()) } else { Err(refused()) },
        Err(error) if error.kind() == io::ErrorKind::NotFound => {}
        Err(error) => return Err(error),
    }
    let fd = fs::openat(
        &root,
        ".elitea-platform-launch.partial",
        OFlags::WRONLY | OFlags::CREATE | OFlags::EXCL | OFlags::NOFOLLOW | OFlags::CLOEXEC,
        Mode::RUSR | Mode::WUSR,
    )?;
    let mut file = std::fs::File::from(fd);
    if !file.metadata()?.is_file() {
        return Err(refused());
    }
    file.write_all(raw)?;
    file.sync_all()?;
    fs::renameat_with(
        &root,
        ".elitea-platform-launch.partial",
        &root,
        ".elitea-platform-launch",
        RenameFlags::NOREPLACE,
    )?;
    fs::fsync(&root)?;
    Ok(())
}
pub(crate) fn run(command: &str) -> io::Result<()> {
    let (raw, reply) = input(&mut std::io::stdin().lock())?;
    let metadata = code_platform_launch::metadata(&raw)?;
    observed_runtime(&metadata)?;
    if command == "--platform-bind" {
        if !reply.is_empty() {
            return Err(refused());
        }
        bind(&raw)?;
        return Ok(());
    }
    if code_platform_launch::read_fixed(code_platform_launch::LAUNCH_PATH, 4096)? != raw {
        return Err(refused());
    }
    let identity = || {
        MailboxIdentity::from_verified_runtime(
            metadata.retained_runtime_id.clone(),
            code_platform_launch::digest(&metadata.prepared_sha256)?,
        )
    };
    let mailbox = Mailbox::open(identity()?)?;
    match command {
        "--platform-read" => {
            if !reply.is_empty() {
                return Err(refused());
            }
            if let Some(request) = mailbox.read_request(&identity()?)? {
                let mut output = std::io::stdout().lock();
                output.write_all(&request)?;
                output.flush()?;
            }
        }
        "--platform-reply" => {
            if reply.is_empty() || mailbox.read_request(&identity()?)?.is_none() {
                return Err(refused());
            }
            // Parent verifies Main's signature; this helper never changes signed bytes.
            mailbox.publish_reply(&identity()?, &reply)?;
        }
        _ => return Err(refused()),
    }
    observed_runtime(&metadata)?;
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn input_refuses_partial_oversized_and_trailing_bytes_before_access() {
        for frame in [
            vec![0; 7],
            vec![255; 8],
            vec![0, 0, 0, 1, 0, 0, 0, 0],
            vec![0, 0, 0, 1, 0, 0, 0, 0, 1, 2],
        ] {
            assert!(input(&mut frame.as_slice()).is_err());
        }
        let frame = [0, 0, 0, 1, 0, 0, 0, 1, 1, 2];
        assert_eq!(input(&mut frame.as_slice()).unwrap(), (vec![1], vec![2]));
    }
}

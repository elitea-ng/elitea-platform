//! Deterministic regular-file content for the isolated Cargo acquisition helper.
use super::{ErrorCode, PrepareError, Result, deadline_check, io_error};
use flate2::{Compression, GzBuilder};
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};
use std::{
    io::{BufRead, BufReader, Read, Write},
    path::Path,
};
use tokio::time::Instant;

const FILE_LIMIT: u64 = 32 * 1024 * 1024;
const RAW_LIMIT: u64 = 256 * 1024 * 1024;
const COMPRESSED_LIMIT: u64 = 128 * 1024 * 1024;
const FILE_COUNT_LIMIT: usize = 16_384;

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
#[serde(deny_unknown_fields)]
pub(crate) struct FileRecord {
    pub(crate) path: String,
    pub(crate) bytes: u64,
    pub(crate) sha256: String,
    pub(crate) mode: u32,
}

#[derive(Debug, Serialize, Deserialize, PartialEq, Eq)]
#[serde(deny_unknown_fields)]
pub(crate) struct ArchiveRecord {
    pub(crate) sha256: String,
    pub(crate) compressed_bytes: u64,
    pub(crate) raw_bytes: u64,
    pub(crate) files: Vec<FileRecord>,
}

pub(crate) fn digest(bytes: &[u8]) -> String {
    format!("{:x}", Sha256::digest(bytes))
}

fn content_error() -> PrepareError {
    PrepareError::new(
        ErrorCode::InvalidContent,
        "Content must use sorted portable regular files",
    )
}

fn integrity_error() -> PrepareError {
    PrepareError::new(
        ErrorCode::IntegrityMismatch,
        "Retained Cargo content changed",
    )
}

pub(crate) fn safe_path(path: &str) -> bool {
    !path.is_empty()
        && path.len() <= 255
        && !path.starts_with('/')
        && !path
            .bytes()
            .any(|byte| byte.is_ascii_control() || matches!(byte, b'\\' | b':'))
        && path
            .split('/')
            .all(|part| !part.is_empty() && part != "." && part != ".." && part.len() <= 100)
}

fn retained_path(path: &str) -> bool {
    matches!(
        path,
        "Cargo.toml"
            | "Cargo.lock"
            | ".cargo/config.toml"
            | "src/main.rs"
            | "src/user.rs"
            | "src/platform.rs"
            | "src/platform_client.rs"
            | "src/platform_pipe.rs"
    ) || path.starts_with("vendor/")
}

fn stream_digest(reader: &mut impl Read, limit: u64, deadline: Instant) -> Result<(String, u64)> {
    let mut hash = Sha256::new();
    let mut count = 0u64;
    let mut buffer = [0; 32 * 1024];
    loop {
        deadline_check(deadline)?;
        let length = reader.read(&mut buffer).map_err(io_error)?;
        if length == 0 {
            return Ok((format!("{:x}", hash.finalize()), count));
        }
        count += length as u64;
        if count > limit {
            return Err(PrepareError::new(
                ErrorCode::ContentLimit,
                "Cargo content limit exceeded",
            ));
        }
        hash.update(&buffer[..length]);
    }
}

fn gather(
    root: &Path,
    directory: &Path,
    records: &mut Vec<FileRecord>,
    payload_bytes: &mut u64,
    deadline: Instant,
) -> Result<()> {
    deadline_check(deadline)?;
    for entry in std::fs::read_dir(directory).map_err(io_error)? {
        deadline_check(deadline)?;
        let path = entry.map_err(io_error)?.path();
        let relative = path.strip_prefix(root).map_err(|_| content_error())?;
        let relative = relative.to_str().ok_or_else(content_error)?;
        if !safe_path(relative) {
            return Err(content_error());
        }
        let metadata = std::fs::symlink_metadata(&path).map_err(io_error)?;
        if metadata.file_type().is_dir() {
            gather(root, &path, records, payload_bytes, deadline)?;
        } else if metadata.file_type().is_file() {
            if !retained_path(relative)
                || metadata.len() > FILE_LIMIT
                || records.len() >= FILE_COUNT_LIMIT
            {
                return Err(PrepareError::new(
                    ErrorCode::ContentLimit,
                    "Cargo content file limit exceeded",
                ));
            }
            *payload_bytes += metadata.len();
            if *payload_bytes > RAW_LIMIT {
                return Err(PrepareError::new(
                    ErrorCode::ContentLimit,
                    "Cargo raw content limit exceeded",
                ));
            }
            let (sha256, bytes) = stream_digest(
                &mut std::fs::File::open(&path).map_err(io_error)?,
                FILE_LIMIT,
                deadline,
            )?;
            #[cfg(unix)]
            let mode = {
                use std::os::unix::fs::PermissionsExt;
                if metadata.permissions().mode() & 0o111 != 0 {
                    0o755
                } else {
                    0o644
                }
            };
            #[cfg(not(unix))]
            let mode = 0o644;
            records.push(FileRecord {
                path: relative.into(),
                bytes,
                sha256,
                mode,
            });
        } else {
            // Never follow links or retain devices, sockets, or named pipes.
            return Err(content_error());
        }
    }
    Ok(())
}

struct BoundedWriter<W> {
    inner: W,
    bytes: u64,
    limit: u64,
    deadline: Instant,
}

impl<W: Write> Write for BoundedWriter<W> {
    fn write(&mut self, bytes: &[u8]) -> std::io::Result<usize> {
        if Instant::now() >= self.deadline || self.bytes + bytes.len() as u64 > self.limit {
            return Err(std::io::Error::other(
                "Archive deadline or size limit exceeded",
            ));
        }
        let count = self.inner.write(bytes)?;
        self.bytes += count as u64;
        Ok(count)
    }
    fn flush(&mut self) -> std::io::Result<()> {
        self.inner.flush()
    }
}

fn archive_io(error: std::io::Error, deadline: Instant) -> PrepareError {
    if Instant::now() >= deadline {
        return PrepareError::new(ErrorCode::DeadlineExceeded, "Preparation deadline expired");
    }
    if error.kind() == std::io::ErrorKind::Other {
        return PrepareError::new(ErrorCode::ContentLimit, "Cargo archive size limit exceeded");
    }
    io_error(error)
}

pub(crate) fn create_archive(
    root: &Path,
    output: &Path,
    deadline: Instant,
) -> Result<ArchiveRecord> {
    let mut files = Vec::new();
    let mut payload_bytes = 0;
    gather(root, root, &mut files, &mut payload_bytes, deadline)?;
    files.sort_unstable_by(|left, right| left.path.cmp(&right.path));
    if files.windows(2).any(|pair| pair[0].path >= pair[1].path)
        || files.iter().map(|file| file.bytes).sum::<u64>() > RAW_LIMIT
    {
        return Err(content_error());
    }
    let file = std::fs::OpenOptions::new()
        .create_new(true)
        .write(true)
        .open(output)
        .map_err(io_error)?;
    let compressed = BoundedWriter {
        inner: file,
        bytes: 0,
        limit: COMPRESSED_LIMIT,
        deadline,
    };
    let gzip = GzBuilder::new()
        .mtime(0)
        .operating_system(255)
        .write(compressed, Compression::new(6));
    let raw = BoundedWriter {
        inner: gzip,
        bytes: 0,
        limit: RAW_LIMIT,
        deadline,
    };
    let mut tar = tar::Builder::new(raw);
    tar.follow_symlinks(false);
    for record in &files {
        deadline_check(deadline)?;
        let mut header = tar::Header::new_ustar();
        // USTAR avoids hidden extended metadata records. Reject unrepresentable paths.
        header.set_path(&record.path).map_err(|_| content_error())?;
        header.set_entry_type(tar::EntryType::Regular);
        header.set_size(record.bytes);
        header.set_uid(0);
        header.set_gid(0);
        header.set_mtime(0);
        header.set_mode(record.mode);
        header.set_cksum();
        let mut input = std::fs::File::open(root.join(&record.path)).map_err(io_error)?;
        tar.append(&header, &mut input)
            .map_err(|error| archive_io(error, deadline))?;
    }
    let raw = tar
        .into_inner()
        .map_err(|error| archive_io(error, deadline))?;
    let raw_bytes = raw.bytes;
    let compressed = raw
        .inner
        .finish()
        .map_err(|error| archive_io(error, deadline))?;
    compressed.inner.sync_all().map_err(io_error)?;
    let (sha256, compressed_bytes) = stream_digest(
        &mut std::fs::File::open(output).map_err(io_error)?,
        COMPRESSED_LIMIT,
        deadline,
    )?;
    Ok(ArchiveRecord {
        sha256,
        compressed_bytes,
        raw_bytes,
        files,
    })
}

struct BoundedReader<R> {
    inner: R,
    bytes: u64,
    deadline: Instant,
}

impl<R: Read> Read for BoundedReader<R> {
    fn read(&mut self, bytes: &mut [u8]) -> std::io::Result<usize> {
        if Instant::now() >= self.deadline {
            return Err(std::io::Error::other("Archive deadline exceeded"));
        }
        let length = self.inner.read(bytes)?;
        self.bytes += length as u64;
        if self.bytes > RAW_LIMIT {
            return Err(std::io::Error::other("Archive raw limit exceeded"));
        }
        Ok(length)
    }
}

/// Verify retained bytes without extraction, resolution, compilation, or execution.
/// The trusted caller must bind this record to its original preparation identity.
pub(crate) fn verify_archive(
    path: &Path,
    expected: &ArchiveRecord,
    deadline: Instant,
) -> Result<()> {
    deadline_check(deadline)?;
    if expected.files.len() > FILE_COUNT_LIMIT
        || expected.compressed_bytes > COMPRESSED_LIMIT
        || expected.raw_bytes > RAW_LIMIT
    {
        return Err(content_error());
    }
    let file_paths: std::collections::BTreeSet<_> = expected
        .files
        .iter()
        .map(|file| file.path.as_str())
        .collect();
    if expected.files.iter().any(|file| {
        !safe_path(&file.path)
            || !retained_path(&file.path)
            || file.bytes > FILE_LIMIT
            || !matches!(file.mode, 0o644 | 0o755)
            || file
                .path
                .match_indices('/')
                .any(|(index, _)| file_paths.contains(&file.path[..index]))
    }) || expected
        .files
        .windows(2)
        .any(|pair| pair[0].path >= pair[1].path)
    {
        return Err(content_error());
    }
    let metadata = std::fs::symlink_metadata(path).map_err(io_error)?;
    if !metadata.file_type().is_file() || metadata.len() != expected.compressed_bytes {
        return Err(integrity_error());
    }
    let (hash, length) = stream_digest(
        &mut std::fs::File::open(path).map_err(io_error)?,
        COMPRESSED_LIMIT,
        deadline,
    )?;
    if hash != expected.sha256 || length != expected.compressed_bytes {
        return Err(integrity_error());
    }
    let gzip = flate2::bufread::GzDecoder::new(BufReader::new(
        std::fs::File::open(path).map_err(io_error)?,
    ));
    let mut archive = tar::Archive::new(BoundedReader {
        inner: gzip,
        bytes: 0,
        deadline,
    });
    let mut count = 0;
    for entry in archive.entries().map_err(io_error)?.raw(true) {
        deadline_check(deadline)?;
        let mut entry = entry.map_err(io_error)?;
        let record = expected.files.get(count).ok_or_else(integrity_error)?;
        let path = entry.path().map_err(io_error)?;
        if entry.header().entry_type() != tar::EntryType::Regular
            || entry.header().as_ustar().is_none()
            || path.to_str() != Some(record.path.as_str())
            || entry.size() != record.bytes
            || entry.header().mode().map_err(io_error)? != record.mode
            || entry.header().uid().map_err(io_error)? != 0
            || entry.header().gid().map_err(io_error)? != 0
            || entry.header().mtime().map_err(io_error)? != 0
        {
            return Err(integrity_error());
        }
        let (hash, length) = stream_digest(&mut entry, FILE_LIMIT, deadline)?;
        if hash != record.sha256 || length != record.bytes {
            return Err(integrity_error());
        }
        count += 1;
    }
    if count != expected.files.len() {
        return Err(integrity_error());
    }
    let mut raw = archive.into_inner();
    // Finish gzip CRC validation and count the complete raw stream.
    let mut buffer = [0; 8192];
    loop {
        let length = raw
            .read(&mut buffer)
            .map_err(|error| archive_io(error, deadline))?;
        if length == 0 {
            break;
        }
        if buffer[..length].iter().any(|byte| *byte != 0) {
            return Err(integrity_error());
        }
    }
    if raw.bytes != expected.raw_bytes {
        return Err(integrity_error());
    }
    if !raw
        .inner
        .into_inner()
        .fill_buf()
        .map_err(io_error)?
        .is_empty()
    {
        return Err(integrity_error());
    }
    Ok(())
}

/// Extract only the already verified regular-file inventory into caller-owned private staging.
/// No untrusted process runs in this inert runtime. Parent checks still reject linked paths.
pub(crate) fn extract_archive(
    path: &Path,
    expected: &ArchiveRecord,
    root: &Path,
    deadline: Instant,
) -> Result<()> {
    verify_archive(path, expected, deadline)?;
    let root_metadata = std::fs::symlink_metadata(root).map_err(io_error)?;
    if !root_metadata.file_type().is_dir()
        || std::fs::read_dir(root).map_err(io_error)?.next().is_some()
    {
        return Err(content_error());
    }
    let gzip = flate2::bufread::GzDecoder::new(BufReader::new(
        std::fs::File::open(path).map_err(io_error)?,
    ));
    let mut archive = tar::Archive::new(BoundedReader {
        inner: gzip,
        bytes: 0,
        deadline,
    });
    let mut count = 0;
    for entry in archive.entries().map_err(io_error)?.raw(true) {
        deadline_check(deadline)?;
        let mut entry = entry.map_err(io_error)?;
        let record = expected.files.get(count).ok_or_else(integrity_error)?;
        if entry.header().entry_type() != tar::EntryType::Regular
            || entry.path().map_err(io_error)?.to_str() != Some(record.path.as_str())
            || entry.size() != record.bytes
        {
            return Err(integrity_error());
        }
        let mut parent = root.to_path_buf();
        let components: Vec<_> = record.path.split('/').collect();
        for part in &components[..components.len() - 1] {
            parent.push(part);
            match std::fs::create_dir(&parent) {
                Ok(()) => {}
                Err(error) if error.kind() == std::io::ErrorKind::AlreadyExists => {
                    if !std::fs::symlink_metadata(&parent)
                        .map_err(io_error)?
                        .file_type()
                        .is_dir()
                    {
                        return Err(content_error());
                    }
                }
                Err(error) => return Err(io_error(error)),
            }
        }
        let mut output = std::fs::OpenOptions::new()
            .create_new(true)
            .write(true)
            .open(root.join(&record.path))
            .map_err(io_error)?;
        let mut hash = Sha256::new();
        let mut bytes = 0u64;
        let mut buffer = [0; 32 * 1024];
        loop {
            deadline_check(deadline)?;
            let count = entry.read(&mut buffer).map_err(io_error)?;
            if count == 0 {
                break;
            }
            bytes += count as u64;
            if bytes > record.bytes {
                return Err(integrity_error());
            }
            hash.update(&buffer[..count]);
            output.write_all(&buffer[..count]).map_err(io_error)?;
        }
        if bytes != record.bytes || format!("{:x}", hash.finalize()) != record.sha256 {
            return Err(integrity_error());
        }
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt;
            output
                .set_permissions(std::fs::Permissions::from_mode(record.mode))
                .map_err(io_error)?;
        }
        output.sync_all().map_err(io_error)?;
        count += 1;
    }
    if count != expected.files.len() {
        return Err(integrity_error());
    }
    // Reopen and finish CRC/raw-length/trailing-byte validation after the streaming extraction.
    verify_archive(path, expected, deadline)
}

/// Verify retained package files after compilation without treating writable job scratch as authority.
pub(crate) fn verify_tree(
    root: &Path,
    expected: &ArchiveRecord,
    allow_user_source: bool,
    deadline: Instant,
) -> Result<()> {
    if !std::fs::symlink_metadata(root)
        .map_err(io_error)?
        .file_type()
        .is_dir()
    {
        return Err(content_error());
    }
    for record in &expected.files {
        deadline_check(deadline)?;
        if allow_user_source && record.path == "src/user.rs" {
            continue;
        }
        let mut parent = root.to_path_buf();
        let components: Vec<_> = record.path.split('/').collect();
        for part in &components[..components.len() - 1] {
            parent.push(part);
            if !std::fs::symlink_metadata(&parent)
                .map_err(io_error)?
                .file_type()
                .is_dir()
            {
                return Err(content_error());
            }
        }
        let path = root.join(&record.path);
        let metadata = std::fs::symlink_metadata(&path).map_err(io_error)?;
        if !metadata.file_type().is_file() || metadata.len() != record.bytes {
            return Err(integrity_error());
        }
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt;
            if metadata.permissions().mode() & 0o777 != record.mode {
                return Err(integrity_error());
            }
        }
        let (hash, bytes) = stream_digest(
            &mut std::fs::File::open(&path).map_err(io_error)?,
            FILE_LIMIT,
            deadline,
        )?;
        if hash != record.sha256 || bytes != record.bytes {
            return Err(integrity_error());
        }
    }
    Ok(())
}

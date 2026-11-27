//! Copy-on-write copies for the checkpoint store: `fclonefileat` on macOS
//! (APFS) and the `FICLONE` reflink on Linux (Btrfs, XFS, bcachefs, …).
//! A clone shares the source's blocks until one of them is written, so
//! storing a large unchanged document costs no extra disk and no copy.
//! Where neither works (another file system, another volume, Windows),
//! [`copy_into`] streams the bytes. Every call starts from an OPEN source
//! file (opened confined and without following a link by the caller), so
//! nothing here resolves a path inside the workspace.

use std::fs::File;
use std::io::{Read as _, Write as _};
use std::path::Path;

use sha2::{Digest as _, Sha256};

/// How a file reached the store.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum CopyMethod {
    /// A copy-on-write clone.
    Clone,
    /// The bytes, copied.
    Copy,
}

/// What [`copy_into`] stored.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct Copied {
    pub sha256: [u8; 32],
    pub len: u64,
    pub method: CopyMethod,
}

/// Clone the open file `source` to the new file `target` (which must not
/// exist). `Err` when the system or the file system cannot clone it.
///
/// # Errors
///
/// No clone on this platform or file system, or I/O.
pub fn clone_file(source: &File, target: &Path) -> std::io::Result<()> {
    #[cfg(target_os = "macos")]
    {
        let parent = target.parent().unwrap_or_else(|| Path::new("."));
        let name = target
            .file_name()
            .ok_or_else(|| std::io::Error::from(std::io::ErrorKind::InvalidInput))?;
        let dir = File::open(parent)?;
        rustix::fs::fclonefileat(
            source,
            &dir,
            name,
            rustix::fs::CloneFlags::NOFOLLOW | rustix::fs::CloneFlags::NOOWNERCOPY,
        )
        .map_err(std::io::Error::from)
    }
    #[cfg(target_os = "linux")]
    {
        let created = File::options().write(true).create_new(true).open(target)?;
        rustix::fs::ioctl_ficlone(&created, source).map_err(|error| {
            drop(created);
            let _ = std::fs::remove_file(target);
            std::io::Error::from(error)
        })
    }
    #[cfg(not(any(target_os = "macos", target_os = "linux")))]
    {
        let _ = (source, target);
        Err(std::io::Error::from(std::io::ErrorKind::Unsupported))
    }
}

/// Hash `file` from its start, at most `max_bytes` (`None` past it).
fn hash_file(file: &mut File, max_bytes: u64) -> std::io::Result<Option<(u64, [u8; 32])>> {
    let mut hasher = Sha256::new();
    let mut buffer = vec![0u8; 256 * 1024];
    let mut len = 0u64;
    loop {
        let read = file.read(&mut buffer)?;
        if read == 0 {
            break;
        }
        len += read as u64;
        if len > max_bytes {
            return Ok(None);
        }
        hasher.update(&buffer[..read]);
    }
    Ok(Some((len, hasher.finalize().into())))
}

/// Put `source`'s content at `target` (a new file), as a clone when
/// `try_clone` and the file system allows it, else as a copy, and hash
/// what was stored, so the digest always names the stored bytes (a file
/// changed meanwhile is stored as it was when cloned or read). `Ok(None)`
/// when the content is over `max_bytes` (nothing is left at `target`).
///
/// # Errors
///
/// I/O. `target` is removed on failure.
pub fn copy_into(
    source: &mut File,
    target: &Path,
    max_bytes: u64,
    try_clone: bool,
) -> std::io::Result<Option<Copied>> {
    let stored = (|| {
        if try_clone && clone_file(source, target).is_ok() {
            let mut clone = File::open(target)?;
            return Ok(
                hash_file(&mut clone, max_bytes)?.map(|(len, sha256)| Copied {
                    sha256,
                    len,
                    method: CopyMethod::Clone,
                }),
            );
        }
        let mut out = File::options().write(true).create_new(true).open(target)?;
        let mut hasher = Sha256::new();
        let mut buffer = vec![0u8; 256 * 1024];
        let mut len = 0u64;
        loop {
            let read = source.read(&mut buffer)?;
            if read == 0 {
                break;
            }
            len += read as u64;
            if len > max_bytes {
                return Ok(None);
            }
            hasher.update(&buffer[..read]);
            out.write_all(&buffer[..read])?;
        }
        out.flush()?;
        Ok(Some(Copied {
            sha256: hasher.finalize().into(),
            len,
            method: CopyMethod::Copy,
        }))
    })();
    if !matches!(stored, Ok(Some(_))) {
        let _ = std::fs::remove_file(target);
    }
    stored
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn a_copy_names_its_bytes_whether_cloned_or_copied() {
        let dir = tempfile::tempdir().unwrap();
        std::fs::write(dir.path().join("src"), b"hello world").unwrap();
        let expected: [u8; 32] = Sha256::digest(b"hello world").into();
        for try_clone in [true, false] {
            let target = dir.path().join(format!("out-{try_clone}"));
            let mut source = File::open(dir.path().join("src")).unwrap();
            let copied = copy_into(&mut source, &target, 1024, try_clone)
                .unwrap()
                .unwrap();
            assert_eq!(copied.sha256, expected);
            assert_eq!(copied.len, 11);
            assert_eq!(std::fs::read(&target).unwrap(), b"hello world");
            if !try_clone {
                assert_eq!(copied.method, CopyMethod::Copy);
            }
        }
    }

    #[test]
    fn a_failed_clone_falls_back_to_a_copy() {
        let dir = tempfile::tempdir().unwrap();
        std::fs::write(dir.path().join("src"), b"data").unwrap();
        let target = dir.path().join("out");
        // A clone onto an existing name always fails: the copy then fails
        // too (create_new), and nothing is left half-written.
        std::fs::write(&target, b"in the way").unwrap();
        let mut source = File::open(dir.path().join("src")).unwrap();
        assert!(clone_file(&source, &target).is_err());
        assert!(copy_into(&mut source, &target, 1024, true).is_err());
        // Without anything in the way, an unclonable source is copied.
        std::fs::remove_file(&target).ok();
        let mut source = File::open(dir.path().join("src")).unwrap();
        let copied = copy_into(&mut source, &target, 1024, true)
            .unwrap()
            .unwrap();
        assert_eq!(std::fs::read(&target).unwrap(), b"data");
        assert_eq!(copied.len, 4);
    }

    #[test]
    fn content_over_the_limit_is_not_stored() {
        let dir = tempfile::tempdir().unwrap();
        std::fs::write(dir.path().join("src"), vec![7u8; 100]).unwrap();
        let target = dir.path().join("out");
        for try_clone in [true, false] {
            let mut source = File::open(dir.path().join("src")).unwrap();
            assert_eq!(
                copy_into(&mut source, &target, 50, try_clone).unwrap(),
                None
            );
            assert!(!target.exists());
        }
    }
}

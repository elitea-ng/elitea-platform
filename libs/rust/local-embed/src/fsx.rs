//! Owner-only files and directories (`0700` / `0600`, created that way;
//! wider ones narrowed; symlinks and other users' refused): the model cache
//! here, and the workspace index (`elitea-local-index` re-exports these).

use std::fs;
use std::io;
use std::path::Path;

fn refused(path: &Path, why: &str) -> io::Error {
    io::Error::new(
        io::ErrorKind::PermissionDenied,
        format!("{} {why}", path.display()),
    )
}

/// Create `dir` (and any missing parent) owner-only, or narrow an existing
/// one to owner-only.
///
/// # Errors
///
/// It cannot be created, is a symlink or not a directory, or belongs to
/// another user.
pub fn create_private_dir(dir: &Path) -> io::Result<()> {
    let mut builder = fs::DirBuilder::new();
    builder.recursive(true);
    #[cfg(unix)]
    {
        use std::os::unix::fs::DirBuilderExt as _;
        builder.mode(0o700);
    }
    builder.create(dir)?;
    let meta = fs::symlink_metadata(dir)?;
    if meta.file_type().is_symlink() || !meta.is_dir() {
        return Err(refused(dir, "is not a directory"));
    }
    #[cfg(unix)]
    {
        use std::os::unix::fs::{MetadataExt as _, PermissionsExt as _};
        if meta.uid() != rustix::process::geteuid().as_raw() {
            return Err(refused(dir, "belongs to another user"));
        }
        // `create` keeps an existing directory's mode: narrow it.
        if meta.mode() & 0o077 != 0 {
            fs::set_permissions(dir, fs::Permissions::from_mode(0o700))?;
        }
    }
    Ok(())
}

/// Create `path` as an empty owner-only file, unless it exists (then
/// [`inspect_private_file`] it). Never follows a symlink.
///
/// # Errors
///
/// It cannot be created, or the existing one is refused.
pub fn create_private_file(path: &Path) -> io::Result<()> {
    let mut options = fs::OpenOptions::new();
    options.write(true).create_new(true);
    #[cfg(unix)]
    {
        use std::os::unix::fs::OpenOptionsExt as _;
        options.mode(0o600);
    }
    match options.open(path) {
        Ok(_) => Ok(()),
        Err(error) if error.kind() == io::ErrorKind::AlreadyExists => inspect_private_file(path),
        Err(error) => Err(error),
    }
}

/// Check an existing file is a regular file of this user, narrowing it to
/// `0600` when it is wider; an absent file is fine.
///
/// # Errors
///
/// It is a symlink, not a regular file, or another user's.
pub fn inspect_private_file(path: &Path) -> io::Result<()> {
    let meta = match fs::symlink_metadata(path) {
        Ok(meta) => meta,
        Err(error) if error.kind() == io::ErrorKind::NotFound => return Ok(()),
        Err(error) => return Err(error),
    };
    if meta.file_type().is_symlink() {
        return Err(refused(path, "is a symbolic link"));
    }
    if !meta.is_file() {
        return Err(refused(path, "is not a regular file"));
    }
    #[cfg(unix)]
    {
        use std::os::unix::fs::{MetadataExt as _, PermissionsExt as _};
        if meta.uid() != rustix::process::geteuid().as_raw() {
            return Err(refused(path, "belongs to another user"));
        }
        if meta.mode() & 0o077 != 0 {
            fs::set_permissions(path, fs::Permissions::from_mode(0o600))?;
        }
    }
    Ok(())
}

//! Owner-only storage for what the desktop keeps about a workspace.
//!
//! An index holds the code structure of a folder the person opened: it is
//! as private as the folder. On Unix every directory made here is `0700`
//! and every file `0600`, created that way (never narrowed after the fact,
//! so there is no window in which they are wider); an existing directory or
//! file that is wider is narrowed, and one that is a symlink, not of its
//! kind, or another user's is refused. SQLite gives its `-wal` and `-shm`
//! files the database's mode; [`inspect_private_file`] re-checks them after
//! the database is opened.
//!
//! The index never lives inside the workspace it indexes
//! ([`ensure_outside`]): the agent's own tools would read it, a checkpoint
//! would copy it, and a `git add .` would commit it.

use std::fs;
use std::io;
use std::path::{Component, Path, PathBuf};

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

/// `path` with every symlink of the part that exists resolved, and the
/// rest appended lexically: where it is, or would be once created.
///
/// # Errors
///
/// It is relative or climbs with `..` past what exists.
pub fn resolved(path: &Path) -> io::Result<PathBuf> {
    if !path.is_absolute() {
        return Err(refused(path, "is not an absolute path"));
    }
    let mut missing = Vec::new();
    let mut existing = path;
    let base = loop {
        match fs::canonicalize(existing) {
            Ok(canonical) => break canonical,
            Err(error) if error.kind() == io::ErrorKind::NotFound => {
                let Some(name) = existing.file_name() else {
                    return Err(error);
                };
                missing.push(name.to_owned());
                existing = existing.parent().ok_or(error)?;
            }
            Err(error) => return Err(error),
        }
    };
    let mut out = base;
    for name in missing.iter().rev() {
        match Path::new(name).components().next() {
            Some(Component::Normal(part)) => out.push(part),
            _ => return Err(refused(path, "is not a plain path")),
        }
    }
    Ok(out)
}

/// Refuse an index directory that is (or would be) inside the workspace
/// it indexes, both paths resolved.
///
/// # Errors
///
/// It is inside, or either path cannot be resolved.
pub fn ensure_outside(index_dir: &Path, workspace_root: &Path) -> io::Result<()> {
    let index = resolved(index_dir)?;
    let root = fs::canonicalize(workspace_root)?;
    if index.starts_with(&root) {
        return Err(refused(
            &index,
            "is inside the workspace it would index; an index is kept in the app's data folder",
        ));
    }
    Ok(())
}

#[cfg(all(test, unix))]
mod tests {
    use super::*;
    use std::os::unix::fs::PermissionsExt as _;

    fn mode(path: &Path) -> u32 {
        fs::symlink_metadata(path).unwrap().permissions().mode() & 0o777
    }

    #[test]
    fn directories_and_files_are_owner_only() {
        let root = tempfile::tempdir().unwrap();
        let dir = root.path().join("a/b/index");
        create_private_dir(&dir).unwrap();
        assert_eq!(mode(&dir), 0o700);
        assert_eq!(mode(&root.path().join("a/b")), 0o700, "parents too");
        let file = dir.join("index.sqlite");
        create_private_file(&file).unwrap();
        assert_eq!(mode(&file), 0o600);
        // Existing and wider: narrowed.
        fs::set_permissions(&dir, fs::Permissions::from_mode(0o755)).unwrap();
        fs::set_permissions(&file, fs::Permissions::from_mode(0o644)).unwrap();
        create_private_dir(&dir).unwrap();
        create_private_file(&file).unwrap();
        assert_eq!(mode(&dir), 0o700);
        assert_eq!(mode(&file), 0o600);
    }

    #[test]
    fn symlinks_are_refused() {
        let root = tempfile::tempdir().unwrap();
        let target = root.path().join("target");
        fs::write(&target, "x").unwrap();
        let link = root.path().join("index.sqlite");
        std::os::unix::fs::symlink(&target, &link).unwrap();
        assert!(create_private_file(&link).is_err());
        assert!(inspect_private_file(&link).is_err());
        let dir_link = root.path().join("dir");
        std::os::unix::fs::symlink(root.path(), &dir_link).unwrap();
        assert!(create_private_dir(&dir_link).is_err());
        assert!(inspect_private_file(&root.path().join("absent")).is_ok());
    }

    #[test]
    fn an_index_inside_the_workspace_is_refused() {
        let root = tempfile::tempdir().unwrap();
        let workspace = root.path().join("project");
        fs::create_dir(&workspace).unwrap();
        let data = root.path().join("app/workspaces/abc/index");
        assert!(ensure_outside(&data, &workspace).is_ok());
        assert!(ensure_outside(&workspace.join(".elitea/index"), &workspace).is_err());
        assert!(ensure_outside(&workspace, &workspace).is_err());
        // Through a symlink that resolves into the workspace.
        let alias = root.path().join("alias");
        std::os::unix::fs::symlink(&workspace, &alias).unwrap();
        assert!(ensure_outside(&alias.join("index"), &workspace).is_err());
        assert!(ensure_outside(Path::new("relative/index"), &workspace).is_err());
    }
}

//! Copy one projected Secret revision into canonical owner-private runtime files.
use crate::config::read_regular_file;
use std::{fs, io::Write, path::Path};

const MAX_MATERIAL_FILES: usize = 64;

#[derive(Debug, thiserror::Error)]
#[error(
    "sandbox material preparation failed; check the Secret projection, file bounds, and destination permissions"
)]
pub struct MaterialError;

/// Run in the non-root init container before any supervisor listener starts.
/// # Errors
/// Rejects paths outside one immutable projection revision and oversized material.
pub fn prepare(source: &Path, destination: &Path) -> Result<(), MaterialError> {
    if !source.is_absolute() || !destination.is_absolute() {
        return Err(MaterialError);
    }
    let source_root = source.canonicalize().map_err(|_| MaterialError)?;
    let snapshot = source
        .join("..data")
        .canonicalize()
        .map_err(|_| MaterialError)?;
    let destination_root = destination.canonicalize().map_err(|_| MaterialError)?;
    if snapshot.parent() != Some(source_root.as_path())
        || destination_root != destination
        || destination_root.starts_with(&source_root)
    {
        return Err(MaterialError);
    }
    let mut entries = Vec::new();
    let mut total = 0usize;
    for entry in fs::read_dir(&snapshot).map_err(|_| MaterialError)? {
        let entry = entry.map_err(|_| MaterialError)?;
        let name = entry.file_name();
        let name = name.to_str().ok_or(MaterialError)?;
        if name.is_empty()
            || name.len() > 128
            || name.starts_with('.')
            || !name
                .bytes()
                .all(|b| b.is_ascii_alphanumeric() || matches!(b, b'.' | b'-' | b'_'))
            || entries.len() >= MAX_MATERIAL_FILES
        {
            return Err(MaterialError);
        }
        let path = entry.path();
        if path.canonicalize().map_err(|_| MaterialError)? != path {
            return Err(MaterialError);
        }
        let bytes = read_regular_file(&path, 1024 * 1024, false, "sandbox projected material")
            .map_err(|_| MaterialError)?;
        total = total.checked_add(bytes.len()).ok_or(MaterialError)?;
        if total > 8 * 1024 * 1024 {
            return Err(MaterialError);
        }
        entries.push((name.to_owned(), zeroize::Zeroizing::new(bytes)));
    }
    if entries.is_empty() {
        return Err(MaterialError);
    }
    // Init retries must not retain keys removed from the selected Secret revision.
    // No supervisor process is running while this directory is prepared.
    for prior in fs::read_dir(destination).map_err(|_| MaterialError)? {
        let prior = prior.map_err(|_| MaterialError)?;
        if !entries
            .iter()
            .any(|(name, _)| prior.file_name() == name.as_str())
        {
            fs::remove_file(prior.path()).map_err(|_| MaterialError)?;
        }
    }
    for (name, bytes) in entries {
        let mut file = tempfile::NamedTempFile::new_in(destination).map_err(|_| MaterialError)?;
        file.write_all(&bytes).map_err(|_| MaterialError)?;
        file.as_file().sync_all().map_err(|_| MaterialError)?;
        file.persist(destination.join(name))
            .map_err(|_| MaterialError)?;
    }
    fs::File::open(destination)
        .and_then(|directory| directory.sync_all())
        .map_err(|_| MaterialError)
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::os::unix::fs::{PermissionsExt, symlink};

    fn fixture() -> (tempfile::TempDir, std::path::PathBuf, std::path::PathBuf) {
        let root = tempfile::tempdir().unwrap();
        let root_path = root.path().canonicalize().unwrap();
        let source = root_path.join("source");
        let destination = root_path.join("private");
        fs::create_dir(&source).unwrap();
        fs::create_dir(&destination).unwrap();
        let revision = source.join("..revision");
        fs::create_dir(&revision).unwrap();
        fs::write(revision.join("config.json"), b"{}").unwrap();
        fs::write(revision.join("server.key"), b"test-only-key").unwrap();
        symlink("..revision", source.join("..data")).unwrap();
        (root, source, destination)
    }

    #[test]
    fn copies_projection_into_regular_private_files_and_allows_init_retry() {
        let (_root, source, destination) = fixture();
        prepare(&source, &destination).unwrap();
        fs::write(destination.join("removed.key"), b"obsolete").unwrap();
        prepare(&source, &destination).unwrap();
        assert!(!destination.join("removed.key").exists());
        let path = destination.join("server.key");
        assert_eq!(path.canonicalize().unwrap(), path);
        assert_eq!(
            fs::metadata(&path).unwrap().permissions().mode() & 0o777,
            0o600
        );
        assert_eq!(
            read_regular_file(&path, 1024, true, "test").unwrap(),
            b"test-only-key"
        );
    }

    #[test]
    fn rejects_escaped_projection_and_nested_file_symlinks() {
        let (_root, source, destination) = fixture();
        fs::remove_file(source.join("..data")).unwrap();
        symlink(&destination, source.join("..data")).unwrap();
        assert!(prepare(&source, &destination).is_err());
        fs::remove_file(source.join("..data")).unwrap();
        symlink("..revision", source.join("..data")).unwrap();
        symlink("config.json", source.join("..revision/alias")).unwrap();
        assert!(prepare(&source, &destination).is_err());
        assert_eq!(fs::read_dir(destination).unwrap().count(), 0);
    }

    #[test]
    fn accepts_64_material_files_and_rejects_65_before_copying() {
        let (_root, source, destination) = fixture();
        let revision = source.join("..revision");
        for index in 2..64 {
            fs::write(revision.join(format!("profile-{index}.json")), b"{}").unwrap();
        }
        prepare(&source, &destination).unwrap();
        assert_eq!(fs::read_dir(&destination).unwrap().count(), 64);
        fs::write(revision.join("profile-64.json"), b"{}").unwrap();
        fs::write(destination.join("server.key"), b"retained-test-key").unwrap();
        assert!(prepare(&source, &destination).is_err());
        assert_eq!(fs::read_dir(&destination).unwrap().count(), 64);
        assert_eq!(
            fs::read(destination.join("server.key")).unwrap(),
            b"retained-test-key"
        );
        assert!(!destination.join("profile-64.json").exists());
    }
}

//! The Unix socket a sidecar listens on and its host dials.

use std::io;
use std::os::unix::fs::{FileTypeExt, PermissionsExt};
use std::path::Path;
use tokio::net::UnixListener;

/// Bind the socket the host dials.
///
/// The parent directory is created; a stale SOCKET at the path is removed,
/// anything else at the path is refused rather than deleted. The socket is
/// made world-writable: the host's container runs as another non-root user
/// and connecting needs write permission. The shared directory (an emptyDir
/// or compose volume) is the only thing that scopes who reaches it.
///
/// # Errors
///
/// Any filesystem or bind failure, or a non-socket file at the path.
pub fn bind(path: &Path) -> io::Result<UnixListener> {
    if let Some(parent) = path.parent().filter(|p| !p.as_os_str().is_empty()) {
        std::fs::create_dir_all(parent)?;
    }
    match std::fs::symlink_metadata(path) {
        Ok(meta) if meta.file_type().is_socket() => std::fs::remove_file(path)?,
        Ok(_) => {
            return Err(io::Error::new(
                io::ErrorKind::AlreadyExists,
                format!("{} exists and is not a socket", path.display()),
            ));
        }
        Err(error) if error.kind() == io::ErrorKind::NotFound => {}
        Err(error) => return Err(error),
    }
    let listener = UnixListener::bind(path)?;
    std::fs::set_permissions(path, std::fs::Permissions::from_mode(0o777))?;
    Ok(listener)
}

#[cfg(test)]
mod tests {
    use super::*;

    fn scratch(name: &str) -> std::path::PathBuf {
        let dir =
            std::env::temp_dir().join(format!("sidecar-socket-{name}-{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&dir);
        dir
    }

    #[tokio::test]
    async fn binds_in_a_fresh_directory_and_replaces_a_stale_socket() {
        let dir = scratch("stale");
        let path = dir.join("nested/engine.sock");
        let first = bind(&path).expect("first bind");
        let mode = std::fs::metadata(&path).expect("meta").permissions().mode();
        assert_eq!(mode & 0o777, 0o777);
        drop(first);
        // The socket file stays behind when a listener is dropped: binding
        // again must replace it.
        assert!(std::fs::symlink_metadata(&path).is_ok());
        let _second = bind(&path).expect("second bind");
        let _ = std::fs::remove_dir_all(&dir);
    }

    #[tokio::test]
    async fn refuses_a_regular_file_at_the_path() {
        let dir = scratch("file");
        std::fs::create_dir_all(&dir).expect("dir");
        let path = dir.join("engine.sock");
        std::fs::write(&path, b"not a socket").expect("write");
        let error = bind(&path).expect_err("refused");
        assert_eq!(error.kind(), io::ErrorKind::AlreadyExists);
        assert_eq!(std::fs::read(&path).expect("kept"), b"not a socket");
        let _ = std::fs::remove_dir_all(&dir);
    }
}

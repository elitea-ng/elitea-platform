//! The operating system layer the tools are written against (#1828).
//!
//! On macOS and Linux every item here IS the `rustix` or `std::os::unix`
//! item (a re-export): the tools behave exactly as they did before Windows
//! was ported. On Windows the same names are a safe shim over `std`
//! (`sys/windows.rs`): no `unsafe` in this crate on either system.
//!
//! What Windows gives and does not give, against the Unix behaviour:
//!
//! | Unix | Windows |
//! |---|---|
//! | Walks from a directory descriptor with `openat(O_NOFOLLOW)`: a swapped component cannot redirect an operation | Walks by path, one component at a time, each opened without following a reparse point (symlink or junction) and refused when it is one. A component swapped between the check and the use is not caught as `openat` catches it |
//! | Process groups: `kill(-pgid)` stops a command and everything it started | Job Objects with kill-on-close ([`process::Group`]): closing the job stops every process in it. A process the command starts in the moment between its own start and its assignment to the job can escape (no `CREATE_SUSPENDED` without `unsafe`). The processes end with exit code 0 |
//! | `SIGTERM`, then `SIGKILL` after a grace | No graceful stop: both close the job |
//! | Owner and mode bits on private files | Not checked: every file reads as the person's and owner-only. The files these checks guard (`hooks.json`, budgets, slash commands, rules) are read from the app's own data under the profile, whose ACL is owner-only; a repository's copy is only ever a proposal. Reading the owner SID and DACL needs `unsafe` (`GetSecurityInfo`). The read-only attribute stands for the write bits |
//! | Inode and change time (`ino`, `ctime`) for "was it replaced" stamps | `ino` is `0`; `ctime` is the later of creation and last write, so a file renamed over (new creation time) or rewritten (new write time) still changes the stamp |
//! | The OS sandbox (Seatbelt, bubblewrap, Landlock) | None ([`crate::sandbox`]): confined commands are refused unless the host allows unenforced commands |

#[cfg(unix)]
pub use rustix::{fs, io};

#[cfg(unix)]
pub mod fd {
    pub use std::os::fd::{AsFd, OwnedFd};
}

/// The `std::os::unix` extension traits (Unix), or their Windows stand-ins.
#[cfg(unix)]
pub mod os {
    pub use std::os::unix::ffi::OsStrExt;
    pub use std::os::unix::fs::{
        DirBuilderExt, MetadataExt, OpenOptionsExt, PermissionsExt, symlink,
    };
    pub use std::os::unix::process::{CommandExt, ExitStatusExt};
}

#[cfg(unix)]
pub mod process;

#[cfg(windows)]
mod windows;

#[cfg(windows)]
pub use windows::{fd, fs, io, os, process};

#[cfg(not(any(unix, windows)))]
compile_error!("elitea-local-tools supports macOS, Linux and Windows");

use std::path::{Path, PathBuf};

/// `path` canonicalised. On Windows without the `\\?\` prefix where the
/// path can be written without it, so it compares with the paths the host
/// and the person write (`C:\Users\…`).
///
/// # Errors
///
/// As [`std::fs::canonicalize`].
pub fn canonicalize(path: impl AsRef<Path>) -> std::io::Result<PathBuf> {
    #[cfg(windows)]
    {
        dunce::canonicalize(path)
    }
    #[cfg(not(windows))]
    {
        std::fs::canonicalize(path)
    }
}

/// Set the permission bits of an open file: the mode on Unix, the
/// read-only attribute (no owner write bit) on Windows.
///
/// # Errors
///
/// As [`std::fs::File::set_permissions`].
pub fn set_file_mode(file: &std::fs::File, mode: u32) -> std::io::Result<()> {
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt as _;
        file.set_permissions(std::fs::Permissions::from_mode(mode))
    }
    #[cfg(windows)]
    {
        let mut permissions = file.metadata()?.permissions();
        permissions.set_readonly(mode & 0o200 == 0);
        file.set_permissions(permissions)
    }
}

/// Set the permission bits of `path` (as [`set_file_mode`]).
///
/// # Errors
///
/// As [`std::fs::set_permissions`].
pub fn set_path_mode(path: &Path, mode: u32) -> std::io::Result<()> {
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt as _;
        std::fs::set_permissions(path, std::fs::Permissions::from_mode(mode))
    }
    #[cfg(windows)]
    {
        let mut permissions = std::fs::metadata(path)?.permissions();
        permissions.set_readonly(mode & 0o200 == 0);
        std::fs::set_permissions(path, permissions)
    }
}

/// Whether `path` is a file this system runs: an execute bit on Unix, an
/// extension of `PATHEXT` on Windows.
#[must_use]
pub fn is_executable(path: &Path) -> bool {
    let Ok(meta) = path.metadata() else {
        return false;
    };
    if !meta.is_file() {
        return false;
    }
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt as _;
        meta.permissions().mode() & 0o111 != 0
    }
    #[cfg(windows)]
    {
        path.extension()
            .and_then(|ext| ext.to_str())
            .is_some_and(|ext| {
                executable_extensions()
                    .iter()
                    .any(|known| known.eq_ignore_ascii_case(ext))
            })
    }
}

/// The files `name` in `dir` may be: itself on Unix; on Windows itself
/// when it has an extension, then `name` with each `PATHEXT` extension.
#[must_use]
pub fn program_candidates(dir: &Path, name: &str) -> Vec<PathBuf> {
    #[cfg(windows)]
    {
        let mut out = Vec::new();
        if Path::new(name).extension().is_some() {
            out.push(dir.join(name));
        }
        out.extend(
            executable_extensions()
                .iter()
                .map(|ext| dir.join(format!("{name}.{ext}"))),
        );
        out
    }
    #[cfg(not(windows))]
    {
        vec![dir.join(name)]
    }
}

/// `PATHEXT`'s extensions without the dot (`exe`, `cmd`, `bat`, `com` when
/// it is unset).
#[cfg(windows)]
fn executable_extensions() -> Vec<String> {
    let listed = std::env::var("PATHEXT").unwrap_or_default();
    let mut out: Vec<String> = listed
        .split(';')
        .map(|ext| ext.trim().trim_start_matches('.').to_ascii_lowercase())
        .filter(|ext| !ext.is_empty())
        .collect();
    if out.is_empty() {
        out = ["exe", "cmd", "bat", "com"].map(str::to_owned).to_vec();
    }
    out
}

/// Whether a program name holds a directory part (`/`, and `\` on
/// Windows): such a name is a path, never looked up on `PATH`.
#[must_use]
pub fn has_dir_part(program: &str) -> bool {
    program.contains('/') || (cfg!(windows) && program.contains('\\'))
}

/// The person's home folder from the environment: a non-empty `HOME`, and
/// on Windows else a non-empty `USERPROFILE` (Windows sets no `HOME`).
#[must_use]
pub fn home_dir() -> Option<PathBuf> {
    let var = |name: &str| std::env::var_os(name).filter(|value| !value.is_empty());
    let home = var("HOME");
    #[cfg(windows)]
    let home = home.or_else(|| var("USERPROFILE"));
    home.map(PathBuf::from)
}

/// A path from the bytes git prints (`-z`): the bytes as they are on Unix,
/// UTF-8 (lossy) on Windows, where git prints UTF-8.
#[must_use]
pub fn path_from_bytes(bytes: &[u8]) -> PathBuf {
    #[cfg(unix)]
    {
        use std::os::unix::ffi::OsStrExt as _;
        PathBuf::from(std::ffi::OsStr::from_bytes(bytes))
    }
    #[cfg(windows)]
    {
        PathBuf::from(String::from_utf8_lossy(bytes).into_owned())
    }
}

#[cfg(test)]
mod tests {
    use std::path::Path;

    #[test]
    fn a_directory_part_makes_a_path() {
        assert!(super::has_dir_part("bin/tool"));
        assert!(!super::has_dir_part("tool"));
        assert_eq!(super::has_dir_part(r"bin\tool"), cfg!(windows));
    }

    #[test]
    fn a_program_is_found_by_its_candidates() {
        let dir = tempfile::tempdir().expect("dir");
        let name = if cfg!(windows) { "tool.exe" } else { "tool" };
        let path = dir.path().join(name);
        std::fs::write(&path, b"x").expect("write");
        assert!(!super::is_executable(&dir.path().join("missing")));
        assert!(
            !super::is_executable(dir.path()),
            "a directory is no program"
        );
        super::set_path_mode(&path, 0o755).expect("mode");
        assert!(super::is_executable(&path));
        assert!(super::program_candidates(dir.path(), "tool").contains(&path));
        assert_eq!(super::path_from_bytes(b"a/b"), Path::new("a/b"));
    }

    #[test]
    fn a_file_mode_sets_read_only() {
        let dir = tempfile::tempdir().expect("dir");
        let path = dir.path().join("f");
        let file = std::fs::File::create(&path).expect("create");
        super::set_file_mode(&file, 0o444).expect("read-only");
        assert!(
            std::fs::metadata(&path)
                .expect("meta")
                .permissions()
                .readonly()
        );
        super::set_file_mode(&file, 0o644).expect("writable");
        assert!(
            !std::fs::metadata(&path)
                .expect("meta")
                .permissions()
                .readonly()
        );
    }
}

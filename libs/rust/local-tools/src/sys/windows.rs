//! The Windows half of [`crate::sys`]: the names the tools use from
//! `rustix` and `std::os::unix`, written over `std` (#1828).
//!
//! A "descriptor" ([`fd::OwnedFd`]) is a path and an open handle. The
//! `*at` functions join the name to the descriptor's path and refuse a
//! reparse point (a symlink or a junction) where the Unix call is given
//! `O_NOFOLLOW`. This keeps the Unix rules (no symlink is followed on the
//! way, a symlink is never written through), but a component swapped
//! between the check and the use is not caught as `openat` catches it: see
//! the table in [`crate::sys`].
//!
//! Owners and mode bits do not exist here. [`process::geteuid`] and every
//! file's owner are `0`, so owner checks pass; the mode a file shows is
//! owner-only (`0o600`, `0o400` read-only, `0o700` a directory), so the
//! "open to group or others" checks pass too. Files under the person's
//! profile carry the profile's owner-only ACL.

/// Mode bits as this layer reports them.
const MODE_DIR: u32 = 0o040_000;
const MODE_FILE: u32 = 0o100_000;
const MODE_LINK: u32 = 0o120_000;
const MODE_TYPE: u32 = 0o170_000;

/// `FILE_FLAG_OPEN_REPARSE_POINT`: open a symlink or junction itself.
const OPEN_REPARSE_POINT: u32 = 0x0020_0000;
/// `FILE_FLAG_BACKUP_SEMANTICS`: needed to open a directory.
const BACKUP_SEMANTICS: u32 = 0x0200_0000;

/// The mode this layer reports for `meta`.
fn synthetic_mode(meta: &std::fs::Metadata) -> u32 {
    let kind = meta.file_type();
    if kind.is_symlink() {
        MODE_LINK | 0o700
    } else if kind.is_dir() {
        MODE_DIR | 0o700
    } else if meta.permissions().readonly() {
        MODE_FILE | 0o400
    } else {
        MODE_FILE | 0o600
    }
}

/// The stand-in for a change time: the later of creation and last write.
fn change_ticks(meta: &std::fs::Metadata) -> u64 {
    use std::os::windows::fs::MetadataExt as _;
    meta.creation_time().max(meta.last_write_time())
}

/// `t` (100 ns ticks since 1601) as seconds and nanoseconds since 1970.
fn unix_time(ticks: u64) -> (i64, i64) {
    const EPOCH_DIFF_SECS: i64 = 11_644_473_600;
    let ticks = i64::try_from(ticks).unwrap_or(i64::MAX);
    let secs = ticks / 10_000_000 - EPOCH_DIFF_SECS;
    let nanos = (ticks % 10_000_000) * 100;
    (secs, nanos)
}

pub mod io {
    //! `rustix::io` on Windows: [`Errno`].

    /// An error number, as the tools match it.
    #[derive(Clone, Copy, Debug, Eq, PartialEq)]
    pub struct Errno(i32);

    /// Codes past this one carry a Windows error code.
    const RAW_BASE: i32 = 1000;

    impl Errno {
        pub const NOENT: Self = Self(2);
        pub const EXIST: Self = Self(17);
        pub const NOTDIR: Self = Self(20);
        pub const LOOP: Self = Self(62);
        pub const NOTEMPTY: Self = Self(66);
        const OTHER: Self = Self(999);

        /// The OS error code: a Windows code when there is one.
        #[must_use]
        pub fn raw_os_error(self) -> i32 {
            if self.0 >= RAW_BASE {
                self.0 - RAW_BASE
            } else {
                self.0
            }
        }
    }

    impl From<std::io::Error> for Errno {
        fn from(error: std::io::Error) -> Self {
            match error.kind() {
                std::io::ErrorKind::NotFound => Self::NOENT,
                std::io::ErrorKind::AlreadyExists => Self::EXIST,
                std::io::ErrorKind::NotADirectory => Self::NOTDIR,
                std::io::ErrorKind::DirectoryNotEmpty => Self::NOTEMPTY,
                _ => error
                    .raw_os_error()
                    .map_or(Self::OTHER, |code| Self(code.saturating_add(RAW_BASE))),
            }
        }
    }

    impl From<Errno> for std::io::Error {
        fn from(errno: Errno) -> Self {
            use std::io::ErrorKind;
            match errno {
                Errno::NOENT => ErrorKind::NotFound.into(),
                Errno::EXIST => ErrorKind::AlreadyExists.into(),
                Errno::NOTDIR => ErrorKind::NotADirectory.into(),
                Errno::NOTEMPTY => ErrorKind::DirectoryNotEmpty.into(),
                Errno::LOOP => Self::other("a symbolic link or junction"),
                Errno(code) if code >= RAW_BASE => Self::from_raw_os_error(code - RAW_BASE),
                Errno(_) => ErrorKind::Other.into(),
            }
        }
    }

    impl std::fmt::Display for Errno {
        fn fmt(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
            std::io::Error::from(*self).fmt(formatter)
        }
    }

    impl std::error::Error for Errno {}

    pub type Result<T> = std::result::Result<T, Errno>;
}

pub mod fd {
    //! `std::os::fd` on Windows: a descriptor is a path and its handle.

    use std::fs::File;
    use std::path::{Path, PathBuf};

    /// An open file or directory, and the path it was opened at.
    #[derive(Debug)]
    pub struct OwnedFd {
        pub(super) path: PathBuf,
        pub(super) file: File,
    }

    impl OwnedFd {
        /// A second descriptor of the same file.
        ///
        /// # Errors
        ///
        /// As [`File::try_clone`].
        pub fn try_clone(&self) -> std::io::Result<Self> {
            Ok(Self {
                path: self.path.clone(),
                file: self.file.try_clone()?,
            })
        }

        pub(super) fn path(&self) -> &Path {
            &self.path
        }
    }

    impl From<OwnedFd> for File {
        fn from(fd: OwnedFd) -> Self {
            fd.file
        }
    }

    /// What a descriptor argument may be.
    pub trait AsFd {
        fn as_owned_fd(&self) -> &OwnedFd;
    }

    impl AsFd for OwnedFd {
        fn as_owned_fd(&self) -> &OwnedFd {
            self
        }
    }

    impl<T: AsFd + ?Sized> AsFd for &T {
        fn as_owned_fd(&self) -> &OwnedFd {
            (**self).as_owned_fd()
        }
    }
}

pub mod fs {
    //! The `rustix::fs` calls the tools make, over `std::fs`.

    use std::fs::OpenOptions;
    use std::ops::BitOr;
    use std::os::windows::fs::{MetadataExt as _, OpenOptionsExt as _};
    use std::path::{Path, PathBuf};

    use super::fd::{AsFd, OwnedFd};
    use super::io::{Errno, Result};

    pub type RawMode = u32;

    macro_rules! flags {
        ($name:ident { $($flag:ident = $bit:expr),* $(,)? }) => {
            #[derive(Clone, Copy, Debug, Default, Eq, PartialEq)]
            pub struct $name(u32);

            impl $name {
                $(pub const $flag: Self = Self($bit);)*

                #[must_use]
                pub const fn empty() -> Self {
                    Self(0)
                }

                #[must_use]
                pub const fn bits(self) -> u32 {
                    self.0
                }

                #[must_use]
                pub const fn contains(self, other: Self) -> bool {
                    self.0 & other.0 == other.0
                }
            }

            impl BitOr for $name {
                type Output = Self;

                fn bitor(self, other: Self) -> Self {
                    Self(self.0 | other.0)
                }
            }
        };
    }

    flags!(OFlags {
        RDONLY = 0,
        WRONLY = 1,
        CREATE = 2,
        EXCL = 4,
        DIRECTORY = 8,
        NOFOLLOW = 16,
        CLOEXEC = 32,
        NONBLOCK = 64,
        TRUNC = 128,
    });

    flags!(AtFlags {
        SYMLINK_NOFOLLOW = 1,
        REMOVEDIR = 2,
    });

    flags!(Mode {
        RUSR = 0o400,
        WUSR = 0o200,
        RWXU = 0o700,
    });

    impl Mode {
        #[must_use]
        pub const fn from_raw_mode(mode: RawMode) -> Self {
            Self(mode)
        }
    }

    /// A file's kind.
    #[derive(Clone, Copy, Debug, Eq, PartialEq)]
    pub enum FileType {
        RegularFile,
        Directory,
        Symlink,
        /// Named pipes, devices: the kinds Unix names apart are one here.
        CharacterDevice,
        Unknown,
    }

    impl FileType {
        #[must_use]
        pub const fn from_raw_mode(mode: RawMode) -> Self {
            match mode & super::MODE_TYPE {
                super::MODE_FILE => Self::RegularFile,
                super::MODE_DIR => Self::Directory,
                super::MODE_LINK => Self::Symlink,
                _ => Self::Unknown,
            }
        }
    }

    /// What `fstat` and `statat` report.
    #[derive(Clone, Copy, Debug)]
    pub struct Stat {
        pub st_mode: RawMode,
        pub st_uid: u32,
        pub st_size: i64,
        pub st_mtime: i64,
        pub st_mtime_nsec: i64,
        pub st_nlink: u64,
    }

    fn stat_of(meta: &std::fs::Metadata, links: u64) -> Stat {
        let (st_mtime, st_mtime_nsec) = super::unix_time(meta.last_write_time());
        Stat {
            st_mode: super::synthetic_mode(meta),
            st_uid: 0,
            st_size: i64::try_from(meta.len()).unwrap_or(i64::MAX),
            st_mtime,
            st_mtime_nsec,
            st_nlink: links,
        }
    }

    /// The `NOFOLLOW` refusal: `LOOP` when `path` is a symlink or junction.
    fn refuse_link(path: &Path) -> Result<()> {
        match std::fs::symlink_metadata(path) {
            Ok(meta) if meta.file_type().is_symlink() => Err(Errno::LOOP),
            Ok(_) => Ok(()),
            Err(error) => Err(error.into()),
        }
    }

    /// Open `path` as `rustix::fs::open` would.
    ///
    /// # Errors
    ///
    /// `NOENT`, `EXIST` (`CREATE | EXCL`), `NOTDIR` (`DIRECTORY` on a
    /// file), `LOOP` (`NOFOLLOW` on a symlink or junction), or the OS error.
    pub fn open<P: AsRef<Path>>(path: P, flags: OFlags, _mode: Mode) -> Result<OwnedFd> {
        open_path(path.as_ref().to_path_buf(), flags)
    }

    /// Open `name` under `dir` (as [`open`]).
    ///
    /// # Errors
    ///
    /// As [`open`].
    pub fn openat<D: AsFd, P: AsRef<Path>>(
        dir: D,
        name: P,
        flags: OFlags,
        _mode: Mode,
    ) -> Result<OwnedFd> {
        open_path(dir.as_owned_fd().path().join(name), flags)
    }

    fn open_path(path: PathBuf, flags: OFlags) -> Result<OwnedFd> {
        let create = flags.contains(OFlags::CREATE);
        if flags.contains(OFlags::NOFOLLOW) && !(create && flags.contains(OFlags::EXCL)) {
            match refuse_link(&path) {
                Err(Errno::NOENT) if create => {}
                other => other?,
            }
        }
        if flags.contains(OFlags::DIRECTORY) {
            let meta = std::fs::metadata(&path)?;
            if !meta.is_dir() {
                return Err(Errno::NOTDIR);
            }
        }
        let mut options = OpenOptions::new();
        if flags.contains(OFlags::WRONLY) {
            options.write(true);
            if create && flags.contains(OFlags::EXCL) {
                options.create_new(true);
            } else if create {
                options.create(true);
            }
            if flags.contains(OFlags::TRUNC) {
                options.truncate(true);
            }
        } else {
            options.read(true);
        }
        let mut custom = BACKUP;
        if flags.contains(OFlags::NOFOLLOW) {
            custom |= super::OPEN_REPARSE_POINT;
        }
        options.custom_flags(custom);
        let file = options.open(&path)?;
        if flags.contains(OFlags::NOFOLLOW) && file.metadata()?.file_type().is_symlink() {
            return Err(Errno::LOOP);
        }
        Ok(OwnedFd { path, file })
    }

    /// Directories open only with backup semantics; files are unaffected.
    const BACKUP: u32 = super::BACKUP_SEMANTICS;

    /// The file `fd` is.
    ///
    /// # Errors
    ///
    /// The OS error.
    pub fn fstat<D: AsFd>(fd: D) -> Result<Stat> {
        let fd = fd.as_owned_fd();
        let meta = fd.file.metadata()?;
        let links =
            winapi_util::file::information(&fd.file).map_or(1, |info| info.number_of_links());
        Ok(stat_of(&meta, links))
    }

    /// The entry `name` under `dir`; a final symlink itself with
    /// `SYMLINK_NOFOLLOW`.
    ///
    /// # Errors
    ///
    /// `NOENT`, or the OS error.
    pub fn statat<D: AsFd, P: AsRef<Path>>(dir: D, name: P, flags: AtFlags) -> Result<Stat> {
        let path = dir.as_owned_fd().path().join(name);
        let meta = if flags.contains(AtFlags::SYMLINK_NOFOLLOW) {
            std::fs::symlink_metadata(&path)?
        } else {
            std::fs::metadata(&path)?
        };
        Ok(stat_of(&meta, 1))
    }

    /// Create the directory `name` under `dir`.
    ///
    /// # Errors
    ///
    /// `EXIST`, `NOENT`, or the OS error.
    pub fn mkdirat<D: AsFd, P: AsRef<Path>>(dir: D, name: P, _mode: Mode) -> Result<()> {
        std::fs::create_dir(dir.as_owned_fd().path().join(name)).map_err(Errno::from)
    }

    /// The target of the symlink or junction `name` under `dir`.
    ///
    /// # Errors
    ///
    /// `NOENT`, or the OS error (not a link).
    pub fn readlinkat<D: AsFd, P: AsRef<Path>>(
        dir: D,
        name: P,
        _reuse: Vec<u8>,
    ) -> Result<LinkTarget> {
        let target = std::fs::read_link(dir.as_owned_fd().path().join(name))?;
        Ok(LinkTarget(target.into_os_string()))
    }

    /// A link's target, as `readlinkat` gives it.
    #[derive(Debug)]
    pub struct LinkTarget(std::ffi::OsString);

    impl LinkTarget {
        /// The target as text.
        ///
        /// # Errors
        ///
        /// When it is not valid Unicode.
        pub fn to_str(&self) -> std::result::Result<&str, Errno> {
            self.0.to_str().ok_or(Errno::LOOP)
        }
    }

    /// Rename `old` under `old_dir` to `new` under `new_dir`, replacing a
    /// file there.
    ///
    /// # Errors
    ///
    /// The OS error.
    pub fn renameat<D: AsFd, P: AsRef<Path>, E: AsFd, Q: AsRef<Path>>(
        old_dir: D,
        old: P,
        new_dir: E,
        new: Q,
    ) -> Result<()> {
        std::fs::rename(
            old_dir.as_owned_fd().path().join(old),
            new_dir.as_owned_fd().path().join(new),
        )
        .map_err(Errno::from)
    }

    /// Remove `name` under `dir`: a directory (empty) with `REMOVEDIR`,
    /// otherwise a file or a link itself (never its target).
    ///
    /// # Errors
    ///
    /// `NOENT`, `NOTEMPTY`, `NOTDIR`, or the OS error.
    pub fn unlinkat<D: AsFd, P: AsRef<Path>>(dir: D, name: P, flags: AtFlags) -> Result<()> {
        let path = dir.as_owned_fd().path().join(name);
        if flags.contains(AtFlags::REMOVEDIR) {
            let meta = std::fs::symlink_metadata(&path)?;
            if !meta.is_dir() {
                return Err(Errno::NOTDIR);
            }
            return std::fs::remove_dir(&path).map_err(Errno::from);
        }
        unlink(&path)
    }

    /// Remove the file or link `path` (never a link's target).
    ///
    /// # Errors
    ///
    /// `NOENT`, or the OS error.
    pub fn unlink<P: AsRef<Path>>(path: P) -> Result<()> {
        /// `FILE_ATTRIBUTE_DIRECTORY`.
        const ATTRIBUTE_DIRECTORY: u32 = 0x10;
        let path = path.as_ref();
        let meta = std::fs::symlink_metadata(path)?;
        // A directory symlink or a junction is removed as a directory: the
        // link's own attributes say which, whether or not its target exists.
        if meta.file_type().is_symlink() && meta.file_attributes() & ATTRIBUTE_DIRECTORY != 0 {
            return std::fs::remove_dir(path).map_err(Errno::from);
        }
        std::fs::remove_file(path).map_err(Errno::from)
    }

    /// The entries of an open directory.
    pub struct Dir(std::fs::ReadDir);

    impl Dir {
        /// Read the directory `fd`.
        ///
        /// # Errors
        ///
        /// The OS error.
        pub fn read_from<D: AsFd>(fd: D) -> Result<Self> {
            Ok(Self(std::fs::read_dir(fd.as_owned_fd().path())?))
        }
    }

    impl Iterator for Dir {
        type Item = Result<DirEntry>;

        fn next(&mut self) -> Option<Self::Item> {
            self.0.next().map(|entry| {
                entry
                    .map(|entry| DirEntry(entry.file_name()))
                    .map_err(Errno::from)
            })
        }
    }

    /// One entry's name.
    pub struct DirEntry(std::ffi::OsString);

    impl DirEntry {
        #[must_use]
        pub fn file_name(&self) -> LinkTargetRef<'_> {
            LinkTargetRef(&self.0)
        }
    }

    /// A borrowed name, as `rustix`'s `&CStr` reads.
    pub struct LinkTargetRef<'a>(&'a std::ffi::OsStr);

    impl<'a> LinkTargetRef<'a> {
        /// The name as text.
        ///
        /// # Errors
        ///
        /// When it is not valid Unicode.
        pub fn to_str(&self) -> std::result::Result<&'a str, Errno> {
            self.0.to_str().ok_or(Errno::LOOP)
        }
    }
}

pub mod os {
    //! Stand-ins for the `std::os::unix` extension traits.

    use std::path::Path;

    /// `uid`, `mode`, times and inode of a file, as the Unix trait reads
    /// them (see the module documentation for what they are here).
    pub trait MetadataExt {
        fn uid(&self) -> u32;
        fn mode(&self) -> u32;
        fn ino(&self) -> u64;
        fn mtime(&self) -> i64;
        fn mtime_nsec(&self) -> i64;
        fn ctime(&self) -> i64;
        fn ctime_nsec(&self) -> i64;
    }

    impl MetadataExt for std::fs::Metadata {
        fn uid(&self) -> u32 {
            0
        }

        fn mode(&self) -> u32 {
            super::synthetic_mode(self)
        }

        fn ino(&self) -> u64 {
            0
        }

        fn mtime(&self) -> i64 {
            super::unix_time(std::os::windows::fs::MetadataExt::last_write_time(self)).0
        }

        fn mtime_nsec(&self) -> i64 {
            super::unix_time(std::os::windows::fs::MetadataExt::last_write_time(self)).1
        }

        /// No change time through `std`: the later of the creation and the
        /// last write, so a file replaced by a rename (a new creation time)
        /// or rewritten (a new write time) changes it.
        fn ctime(&self) -> i64 {
            super::unix_time(super::change_ticks(self)).0
        }

        fn ctime_nsec(&self) -> i64 {
            super::unix_time(super::change_ticks(self)).1
        }
    }

    /// `mode` on a directory builder: nothing to set here.
    pub trait DirBuilderExt {
        fn mode(&mut self, mode: u32) -> &mut Self;
    }

    impl DirBuilderExt for std::fs::DirBuilder {
        fn mode(&mut self, _mode: u32) -> &mut Self {
            self
        }
    }

    /// `mode` and `custom_flags` on open options: `O_NOFOLLOW` becomes
    /// "open a reparse point itself" (a symlink is then opened, never
    /// followed); the mode is not set here.
    pub trait OpenOptionsExt {
        fn mode(&mut self, mode: u32) -> &mut Self;
        fn custom_flags(&mut self, flags: i32) -> &mut Self;
    }

    impl OpenOptionsExt for std::fs::OpenOptions {
        fn mode(&mut self, _mode: u32) -> &mut Self {
            self
        }

        fn custom_flags(&mut self, flags: i32) -> &mut Self {
            let nofollow = super::fs::OFlags::NOFOLLOW.bits().cast_signed();
            if flags & nofollow != 0 {
                std::os::windows::fs::OpenOptionsExt::custom_flags(self, super::OPEN_REPARSE_POINT);
            }
            self
        }
    }

    /// `process_group(0)` on a command: no console window, its own
    /// console process group. What stops the whole tree is the job
    /// ([`super::process::Group`]).
    pub trait CommandExt {
        fn process_group(&mut self, group: i32) -> &mut Self;
    }

    /// `CREATE_NEW_PROCESS_GROUP | CREATE_NO_WINDOW`.
    const CREATION_FLAGS: u32 = 0x0000_0200 | 0x0800_0000;

    impl CommandExt for std::process::Command {
        fn process_group(&mut self, _group: i32) -> &mut Self {
            std::os::windows::process::CommandExt::creation_flags(self, CREATION_FLAGS)
        }
    }

    impl CommandExt for tokio::process::Command {
        fn process_group(&mut self, _group: i32) -> &mut Self {
            self.creation_flags(CREATION_FLAGS)
        }
    }

    /// No signals end a process here.
    pub trait ExitStatusExt {
        fn signal(&self) -> Option<i32>;
    }

    impl ExitStatusExt for std::process::ExitStatus {
        fn signal(&self) -> Option<i32> {
            None
        }
    }

    /// A symlink at `link` to `target`, a directory link when the target
    /// (relative to `link`'s folder) is a directory.
    ///
    /// # Errors
    ///
    /// The OS error (creating a symlink may need Developer Mode).
    pub fn symlink(target: impl AsRef<Path>, link: impl AsRef<Path>) -> std::io::Result<()> {
        let (target, link) = (target.as_ref(), link.as_ref());
        let resolved = link
            .parent()
            .map_or_else(|| target.to_path_buf(), |parent| parent.join(target));
        if resolved.is_dir() {
            std::os::windows::fs::symlink_dir(target, link)
        } else {
            std::os::windows::fs::symlink_file(target, link)
        }
    }
}

pub mod process {
    //! Process groups as Job Objects, and the ids the owner checks compare.

    use std::sync::{Arc, Mutex};

    /// A signal, as the tools name it. Both end the whole job here: there
    /// is no graceful stop.
    #[derive(Clone, Copy, Debug, Eq, PartialEq)]
    pub struct Signal(u8);

    impl Signal {
        pub const TERM: Self = Self(15);
        pub const KILL: Self = Self(9);
    }

    /// A user id: always `0` here (see the module documentation).
    #[derive(Clone, Copy, Debug, Eq, PartialEq)]
    pub struct Uid(u32);

    impl Uid {
        #[must_use]
        pub const fn as_raw(self) -> u32 {
            self.0
        }
    }

    #[must_use]
    pub const fn geteuid() -> Uid {
        Uid(0)
    }

    #[must_use]
    pub const fn getuid() -> Uid {
        Uid(0)
    }

    /// A command's process tree: a Job Object with kill-on-close the
    /// command was assigned to once it started. Signalling it (or dropping
    /// the last clone) closes the job, which ends every process in it.
    #[derive(Clone, Debug)]
    pub struct Group(Arc<Mutex<Option<win32job::Job>>>);

    impl Group {
        fn of_handle(handle: Option<std::os::windows::io::RawHandle>) -> Option<Self> {
            let handle = handle?;
            let mut info = win32job::ExtendedLimitInfo::new();
            info.limit_kill_on_job_close();
            let job = win32job::Job::create_with_limit_info(&info).ok()?;
            job.assign_process(handle as isize).ok()?;
            Some(Self(Arc::new(Mutex::new(Some(job)))))
        }

        /// The tree of a started `std` child.
        #[must_use]
        pub fn of_child(child: &std::process::Child) -> Option<Self> {
            use std::os::windows::io::AsRawHandle as _;
            Self::of_handle(Some(child.as_raw_handle()))
        }

        /// The tree of a started tokio child (`None` once it was reaped).
        #[must_use]
        pub fn of_tokio(child: &tokio::process::Child) -> Option<Self> {
            Self::of_handle(child.raw_handle())
        }

        /// The tree of a child on a pseudo-terminal (`ConPTY`).
        #[must_use]
        pub fn of_pty(child: &(dyn portable_pty::Child + Send + Sync)) -> Option<Self> {
            Self::of_handle(child.as_raw_handle())
        }

        /// End every process in the job (any signal).
        ///
        /// The job is closed, and kill-on-close ends what is in it. Every
        /// clone shares this one handle (the `Arc`), and nothing else holds
        /// one, so taking it closes the job whatever clones are alive.
        ///
        /// Known gaps (no `unsafe` here: `TerminateJobObject` and
        /// `ResumeThread` are `unsafe fn`, and this crate forbids unsafe code):
        /// * the processes end with exit code 0, not 1 as `TerminateJobObject`
        ///   would give: the callers record the stop themselves;
        /// * the child is assigned to the job after it started, not created
        ///   suspended: a process it starts in the moment between its start
        ///   and the assignment (microseconds) is outside the job.
        pub fn signal(&self, _signal: Signal) {
            let job = self
                .0
                .lock()
                .unwrap_or_else(std::sync::PoisonError::into_inner)
                .take();
            drop(job);
        }
    }
}

#[cfg(test)]
mod tests {
    use std::io::Write as _;

    use super::fs::{self, AtFlags, Mode, OFlags};
    use super::io::Errno;

    #[test]
    fn errno_round_trips_through_io_errors() {
        for errno in [Errno::NOENT, Errno::EXIST, Errno::NOTDIR, Errno::NOTEMPTY] {
            assert_eq!(Errno::from(std::io::Error::from(errno)), errno);
        }
        let raw = Errno::from(std::io::Error::from_raw_os_error(5));
        assert_eq!(raw.raw_os_error(), 5);
    }

    #[test]
    fn the_at_calls_create_read_rename_and_remove() {
        let dir = tempfile::tempdir().expect("dir");
        let root = fs::open(
            dir.path(),
            OFlags::RDONLY | OFlags::DIRECTORY,
            Mode::empty(),
        )
        .expect("root");
        fs::mkdirat(&root, "sub", Mode::RWXU).expect("mkdir");
        assert_eq!(fs::mkdirat(&root, "sub", Mode::RWXU), Err(Errno::EXIST));
        let flags = OFlags::WRONLY | OFlags::CREATE | OFlags::EXCL | OFlags::NOFOLLOW;
        let file = fs::openat(&root, "sub/a.txt", flags, Mode::RUSR | Mode::WUSR).expect("create");
        std::fs::File::from(file).write_all(b"one").expect("write");
        assert_eq!(
            fs::openat(&root, "sub/a.txt", flags, Mode::RUSR).err(),
            Some(Errno::EXIST)
        );
        assert_eq!(
            fs::openat(
                &root,
                "sub/a.txt",
                OFlags::RDONLY | OFlags::DIRECTORY,
                Mode::empty()
            )
            .err(),
            Some(Errno::NOTDIR)
        );
        let stat = fs::statat(&root, "sub/a.txt", AtFlags::SYMLINK_NOFOLLOW).expect("stat");
        assert_eq!(stat.st_size, 3);
        assert!(matches!(
            fs::FileType::from_raw_mode(stat.st_mode),
            fs::FileType::RegularFile
        ));
        let sub = fs::openat(
            &root,
            "sub",
            OFlags::RDONLY | OFlags::DIRECTORY,
            Mode::empty(),
        )
        .expect("sub");
        let names: Vec<String> = fs::Dir::read_from(&sub)
            .expect("dir")
            .map(|entry| {
                entry
                    .expect("entry")
                    .file_name()
                    .to_str()
                    .expect("utf8")
                    .to_owned()
            })
            .collect();
        assert_eq!(names, ["a.txt"]);
        drop(sub);
        fs::renameat(&root, "sub/a.txt", &root, "b.txt").expect("rename");
        assert_eq!(fs::unlinkat(&root, "sub", AtFlags::REMOVEDIR), Ok(()));
        assert_eq!(
            fs::unlinkat(&root, "b.txt", AtFlags::REMOVEDIR),
            Err(Errno::NOTDIR)
        );
        fs::unlinkat(&root, "b.txt", AtFlags::empty()).expect("unlink");
        assert_eq!(
            fs::statat(&root, "b.txt", AtFlags::empty()).err(),
            Some(Errno::NOENT)
        );
    }

    /// Ending a group ends its whole tree: `cmd` AND the `ping` it started
    /// (a grandchild), at once instead of after 30 seconds.
    ///
    /// `ping` inherits `cmd`'s stdout pipe, so the pipe reaches end of file
    /// only when both have exited: a grandchild that escaped the job would
    /// hold it open for the rest of its 30 seconds. The first line `ping`
    /// prints proves it runs before the kill. The exit code proves nothing
    /// (a job closed with kill-on-close may end its processes with 0); the
    /// callers record a stop themselves (`timed_out`, `stopping`).
    #[test]
    fn a_group_ends_its_process_tree_grandchildren_included() {
        use std::io::BufRead as _;
        use std::time::{Duration, Instant};

        let mut child = std::process::Command::new("cmd")
            .args(["/C", "ping -n 30 127.0.0.1"])
            .stdout(std::process::Stdio::piped())
            .spawn()
            .expect("spawn");
        let group = super::process::Group::of_child(&child).expect("job");
        let stdout = child.stdout.take().expect("stdout");
        let (lines, received) = std::sync::mpsc::channel::<Option<String>>();
        std::thread::spawn(move || {
            for line in std::io::BufReader::new(stdout).lines() {
                let Ok(line) = line else { break };
                if lines.send(Some(line)).is_err() {
                    return;
                }
            }
            let _ = lines.send(None); // end of file: every writer is gone
        });
        // Wait until ping answers once: the grandchild runs.
        let deadline = Instant::now() + Duration::from_secs(20);
        loop {
            let left = deadline.saturating_duration_since(Instant::now());
            match received.recv_timeout(left).expect("ping printed nothing") {
                Some(line) if line.contains("127.0.0.1") && line.contains('=') => break,
                Some(_) => {}
                None => panic!("the pipe closed before ping ran"),
            }
        }
        let started = Instant::now();
        group.signal(super::process::Signal::KILL);
        child.wait().expect("wait");
        loop {
            let left = Duration::from_secs(10).saturating_sub(started.elapsed());
            match received.recv_timeout(left) {
                Ok(Some(_)) => {}
                Ok(None) => break,
                Err(error) => panic!(
                    "ping outlived its job for {:?} ({error})",
                    started.elapsed()
                ),
            }
        }
    }
}

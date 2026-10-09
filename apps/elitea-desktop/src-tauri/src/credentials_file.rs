//! Where the device session lives: one owner-only JSON file in the app's
//! config directory, like `~/.aws/credentials` (README, "Security model").
//!
//! `~/Library/Application Support/ai.elitea.desktop/credentials.json` on
//! macOS, `$XDG_CONFIG_HOME/ai.elitea.desktop/` on Linux,
//! `%APPDATA%\ai.elitea.desktop\` on Windows. It replaced the OS keychain: a
//! debug or ad-hoc-signed build gets a new code identity on every build, and
//! macOS asked for the login password on every keychain read. The old
//! keychain item is never read (reading it is what prompts); people sign in
//! once more.
//!
//! The file holds named slots (`device-session`, `pending-revoke`), each the
//! raw JSON the slot's owner wrote. It is read ONCE per launch and kept in
//! memory; every change is written through.
//!
//! Protection, on Unix: the directory is `0700`; the file is created `0600`
//! from the start (a temp file opened with that mode, never a chmod after a
//! write), filled, fsynced and renamed over the old one in the same
//! directory, then the directory is fsynced. A file that is a symlink, is not
//! a regular file, belongs to another user, or is readable or writable by
//! group or others is REFUSED (an error, and a warning in the log): it is not
//! ours to trust or to overwrite. Its contents are never logged.

use std::collections::BTreeMap;
use std::fs;
use std::io::Write as _;
use std::path::{Path, PathBuf};
use std::sync::{Arc, Mutex};

use crate::error::HostError;
use crate::store::SecretStore;

pub const FILE_NAME: &str = "credentials.json";

type Slots = BTreeMap<String, String>;

pub struct CredentialsFile {
    dir: PathBuf,
    /// `None` until the first access reads the file.
    cache: Mutex<Option<Slots>>,
}

fn refuse(path: &Path, why: &str) -> HostError {
    log::warn!("refusing the credentials file {}: {why}", path.display());
    HostError::Credentials(format!("{} {why}", path.display()))
}

fn io_error(what: &str, error: &std::io::Error) -> HostError {
    HostError::Credentials(format!("{what}: {error}"))
}

impl CredentialsFile {
    #[must_use]
    pub fn new(dir: PathBuf) -> Arc<Self> {
        Arc::new(Self {
            dir,
            cache: Mutex::new(None),
        })
    }

    #[must_use]
    pub fn path(&self) -> PathBuf {
        self.dir.join(FILE_NAME)
    }

    /// One named slot of this file, as a [`SecretStore`].
    #[must_use]
    pub fn slot(self: &Arc<Self>, name: &'static str) -> FileSlot {
        FileSlot {
            file: self.clone(),
            name,
        }
    }

    fn read(&self) -> Result<Slots, HostError> {
        let path = self.path();
        let meta = match fs::symlink_metadata(&path) {
            Ok(meta) => meta,
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => return Ok(Slots::new()),
            Err(e) => return Err(io_error("could not inspect the credentials file", &e)),
        };
        if meta.file_type().is_symlink() {
            return Err(refuse(&path, "is a symbolic link"));
        }
        if !meta.is_file() {
            return Err(refuse(&path, "is not a regular file"));
        }
        #[cfg(unix)]
        {
            use std::os::unix::fs::MetadataExt as _;
            if meta.uid() != rustix::process::geteuid().as_raw() {
                return Err(refuse(&path, "belongs to another user"));
            }
            if meta.mode() & 0o077 != 0 {
                return Err(refuse(&path, "is readable or writable by other users"));
            }
        }
        let raw = open_no_follow(&path)
            .and_then(|mut file| {
                let mut text = String::new();
                std::io::Read::read_to_string(&mut file, &mut text).map(|_| text)
            })
            .map_err(|e| io_error("could not read the credentials file", &e))?;
        // A damaged file must not wedge sign-in: it reads as signed out (and
        // is replaced on the next sign-in).
        Ok(serde_json::from_str(&raw).unwrap_or_else(|_| {
            log::warn!("the credentials file does not parse; treating it as empty");
            Slots::new()
        }))
    }

    fn write(&self, slots: &Slots) -> Result<(), HostError> {
        let path = self.path();
        create_private_dir(&self.dir)?;
        if slots.is_empty() {
            return match fs::remove_file(&path) {
                Ok(()) => sync_dir(&self.dir),
                Err(e) if e.kind() == std::io::ErrorKind::NotFound => Ok(()),
                Err(e) => Err(io_error("could not delete the credentials file", &e)),
            };
        }
        let body = serde_json::to_vec(slots).map_err(|e| HostError::Internal(e.to_string()))?;
        let temp = self.dir.join(format!(
            ".{FILE_NAME}.{}.tmp",
            uuid::Uuid::new_v4().simple()
        ));
        let written = (|| {
            let mut file = create_private_file(&temp)?;
            file.write_all(&body)?;
            file.sync_all()?;
            fs::rename(&temp, &path)
        })();
        if let Err(e) = written {
            let _ = fs::remove_file(&temp);
            return Err(io_error("could not write the credentials file", &e));
        }
        sync_dir(&self.dir)
    }

    /// Run `change` on the slots; when it reports a change, write the result
    /// through. The cache is only updated once the write succeeded.
    fn update<T>(&self, change: impl FnOnce(&mut Slots) -> (T, bool)) -> Result<T, HostError> {
        let mut cache = self
            .cache
            .lock()
            .map_err(|_| HostError::Internal("credentials lock poisoned".into()))?;
        let mut slots = match cache.as_ref() {
            Some(slots) => slots.clone(),
            None => self.read()?,
        };
        let (value, changed) = change(&mut slots);
        if changed {
            self.write(&slots)?;
        }
        *cache = Some(slots);
        Ok(value)
    }
}

fn open_no_follow(path: &Path) -> std::io::Result<fs::File> {
    let mut options = fs::OpenOptions::new();
    options.read(true);
    #[cfg(unix)]
    {
        use std::os::unix::fs::OpenOptionsExt as _;
        options.custom_flags(rustix::fs::OFlags::NOFOLLOW.bits().cast_signed());
    }
    options.open(path)
}

fn create_private_file(path: &Path) -> std::io::Result<fs::File> {
    let mut options = fs::OpenOptions::new();
    options.write(true).create_new(true);
    #[cfg(unix)]
    {
        use std::os::unix::fs::OpenOptionsExt as _;
        options
            .mode(0o600)
            .custom_flags(rustix::fs::OFlags::NOFOLLOW.bits().cast_signed());
    }
    options.open(path)
}

fn create_private_dir(dir: &Path) -> Result<(), HostError> {
    let mut builder = fs::DirBuilder::new();
    builder.recursive(true);
    #[cfg(unix)]
    {
        use std::os::unix::fs::DirBuilderExt as _;
        builder.mode(0o700);
    }
    builder
        .create(dir)
        .map_err(|e| io_error("could not create the credentials directory", &e))?;
    // An existing directory keeps its mode under `create`: narrow it.
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt as _;
        let meta = fs::symlink_metadata(dir)
            .map_err(|e| io_error("could not inspect the credentials directory", &e))?;
        if meta.file_type().is_symlink() || !meta.is_dir() {
            return Err(refuse(dir, "is not a directory"));
        }
        if meta.permissions().mode() & 0o077 != 0 {
            fs::set_permissions(dir, fs::Permissions::from_mode(0o700))
                .map_err(|e| io_error("could not restrict the credentials directory", &e))?;
        }
    }
    Ok(())
}

fn sync_dir(dir: &Path) -> Result<(), HostError> {
    #[cfg(unix)]
    {
        fs::File::open(dir)
            .and_then(|d| d.sync_all())
            .map_err(|e| io_error("could not sync the credentials directory", &e))?;
    }
    #[cfg(not(unix))]
    let _ = dir;
    Ok(())
}

/// One named slot of the credentials file.
pub struct FileSlot {
    file: Arc<CredentialsFile>,
    name: &'static str,
}

impl SecretStore for FileSlot {
    fn load(&self) -> Result<Option<String>, HostError> {
        self.file
            .update(|slots| (slots.get(self.name).cloned(), false))
    }

    fn save(&self, secret: &str) -> Result<(), HostError> {
        self.file.update(|slots| {
            let changed = slots.get(self.name).map(String::as_str) != Some(secret);
            slots.insert(self.name.to_owned(), secret.to_owned());
            ((), changed)
        })
    }

    fn clear(&self) -> Result<(), HostError> {
        self.file
            .update(|slots| ((), slots.remove(self.name).is_some()))
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn file() -> (tempfile::TempDir, Arc<CredentialsFile>) {
        let root = tempfile::tempdir().unwrap();
        let file = CredentialsFile::new(root.path().join("ai.elitea.desktop"));
        (root, file)
    }

    #[test]
    fn slots_round_trip_and_survive_a_new_launch() {
        let (_root, file) = file();
        let session = file.slot("device-session");
        let pending = file.slot("pending-revoke");
        assert_eq!(session.load().unwrap(), None);
        session.save(r#"{"refresh_token":"r1"}"#).unwrap();
        pending.save("[]").unwrap();
        // A new launch reads what the last one wrote.
        let again = CredentialsFile::new(file.dir.clone());
        assert_eq!(
            again.slot("device-session").load().unwrap().as_deref(),
            Some(r#"{"refresh_token":"r1"}"#)
        );
        assert_eq!(
            again.slot("pending-revoke").load().unwrap().as_deref(),
            Some("[]")
        );
    }

    #[test]
    fn the_file_is_read_once_and_then_served_from_memory() {
        let (_root, file) = file();
        file.slot("device-session").save("s1").unwrap();
        let launch = CredentialsFile::new(file.dir.clone());
        assert_eq!(
            launch.slot("device-session").load().unwrap().as_deref(),
            Some("s1")
        );
        fs::remove_file(file.path()).unwrap();
        assert_eq!(
            launch.slot("device-session").load().unwrap().as_deref(),
            Some("s1")
        );
    }

    #[test]
    fn clearing_every_slot_deletes_the_file() {
        let (_root, file) = file();
        let session = file.slot("device-session");
        let pending = file.slot("pending-revoke");
        session.save("s").unwrap();
        pending.save("p").unwrap();
        session.clear().unwrap();
        assert!(file.path().exists(), "the pending revokes are still kept");
        pending.clear().unwrap();
        assert!(!file.path().exists());
        session.clear().unwrap(); // clearing an absent slot is fine
    }

    #[test]
    fn a_replace_is_atomic_and_leaves_no_temp_file() {
        let (_root, file) = file();
        let session = file.slot("device-session");
        session.save("one").unwrap();
        session.save("two").unwrap();
        let names: Vec<String> = fs::read_dir(&file.dir)
            .unwrap()
            .map(|e| e.unwrap().file_name().to_string_lossy().into_owned())
            .collect();
        assert_eq!(names, [FILE_NAME]);
        let raw = fs::read_to_string(file.path()).unwrap();
        assert_eq!(raw, r#"{"device-session":"two"}"#);
    }

    #[test]
    fn a_damaged_file_reads_as_signed_out() {
        let (_root, file) = file();
        file.slot("device-session").save("s").unwrap();
        fs::write(file.path(), "not json").unwrap();
        let launch = CredentialsFile::new(file.dir.clone());
        assert_eq!(launch.slot("device-session").load().unwrap(), None);
    }

    #[cfg(unix)]
    #[test]
    fn the_directory_and_file_are_owner_only() {
        use std::os::unix::fs::PermissionsExt as _;
        let (_root, file) = file();
        fs::create_dir_all(&file.dir).unwrap();
        fs::set_permissions(&file.dir, fs::Permissions::from_mode(0o755)).unwrap();
        file.slot("device-session").save("s").unwrap();
        let mode = |p: &Path| fs::metadata(p).unwrap().permissions().mode() & 0o777;
        assert_eq!(mode(&file.dir), 0o700);
        assert_eq!(mode(&file.path()), 0o600);
    }

    #[cfg(unix)]
    #[test]
    fn a_symlinked_file_is_refused_and_not_followed() {
        let (root, file) = file();
        fs::create_dir_all(&file.dir).unwrap();
        let target = root.path().join("elsewhere.json");
        fs::write(&target, r#"{"device-session":"planted"}"#).unwrap();
        std::os::unix::fs::symlink(&target, file.path()).unwrap();
        let err = file.slot("device-session").load().unwrap_err();
        assert!(matches!(err, HostError::Credentials(_)), "{err:?}");
        assert!(err.to_string().contains("symbolic link"));
    }

    #[cfg(unix)]
    #[test]
    fn a_file_others_can_read_is_refused() {
        use std::os::unix::fs::PermissionsExt as _;
        let (_root, file) = file();
        file.slot("device-session").save("s").unwrap();
        fs::set_permissions(file.path(), fs::Permissions::from_mode(0o644)).unwrap();
        let launch = CredentialsFile::new(file.dir.clone());
        let err = launch.slot("device-session").load().unwrap_err();
        assert!(err.to_string().contains("other users"), "{err}");
    }
}

//! Fixed mailbox access. Trusted launch and Supervisor identity checks stay outside Code.
use rustix::fs::{self, AtFlags, Mode, OFlags, RenameFlags};
use std::{
    fs::File,
    io::{self, Read, Write},
    os::fd::OwnedFd,
};

const REQUEST_LIMIT: usize = 8 + 262_144 + 65_536;
const REPLY_LIMIT: usize = 8 + 4096 + 8 + 2_097_152 + 65_536;

fn refused() -> io::Error {
    io::Error::other("Code platform mailbox refused")
}

/// Mint only after verified prepared fingerprint and retained runtime checks.
/// Docker observes the exact container ID. Kubernetes observes the exact Pod UID.
pub(crate) struct MailboxIdentity {
    runtime_id: String,
    request_sha256: [u8; 32],
}
impl MailboxIdentity {
    pub(crate) fn from_verified_runtime(
        runtime_id: String,
        request_sha256: [u8; 32],
    ) -> io::Result<Self> {
        if runtime_id.is_empty()
            || runtime_id.len() > 512
            || runtime_id.contains(['\0', '\r', '\n'])
            || request_sha256 == [0; 32]
        {
            return Err(refused());
        }
        Ok(Self {
            runtime_id,
            request_sha256,
        })
    }
    pub(crate) fn matches(&self, observed: &Self) -> bool {
        self.runtime_id == observed.runtime_id && self.request_sha256 == observed.request_sha256
    }
}

pub(crate) struct Mailbox {
    directory: OwnedFd,
    identity: MailboxIdentity,
}
impl Mailbox {
    pub(crate) fn open(identity: MailboxIdentity) -> io::Result<Self> {
        let workspace = fs::open(
            "/workspace",
            OFlags::RDONLY | OFlags::DIRECTORY | OFlags::NOFOLLOW | OFlags::CLOEXEC,
            Mode::empty(),
        )?;
        Self::at(&workspace, identity)
    }
    fn at(workspace: &OwnedFd, identity: MailboxIdentity) -> io::Result<Self> {
        match fs::mkdirat(
            workspace,
            ".elitea-platform",
            Mode::RUSR | Mode::WUSR | Mode::XUSR,
        ) {
            Ok(()) => fs::fsync(workspace)?,
            Err(error) if error == rustix::io::Errno::EXIST => {}
            Err(error) => return Err(error.into()),
        }
        let directory = fs::openat(
            workspace,
            ".elitea-platform",
            OFlags::RDONLY | OFlags::DIRECTORY | OFlags::NOFOLLOW | OFlags::CLOEXEC,
            Mode::empty(),
        )?;
        Ok(Self {
            directory,
            identity,
        })
    }
    fn check(&self, observed: &MailboxIdentity) -> io::Result<()> {
        if !self.identity.matches(observed) {
            return Err(refused());
        }
        Ok(())
    }
    fn read(&self, name: &'static str, maximum: usize) -> io::Result<Option<Vec<u8>>> {
        let fd = match fs::openat(
            &self.directory,
            name,
            OFlags::RDONLY | OFlags::NOFOLLOW | OFlags::NONBLOCK | OFlags::CLOEXEC,
            Mode::empty(),
        ) {
            Ok(fd) => fd,
            Err(error) if error == rustix::io::Errno::NOENT => return Ok(None),
            Err(error) => return Err(error.into()),
        };
        let file = File::from(fd);
        let metadata = file.metadata()?;
        if !metadata.is_file() || metadata.len() > maximum as u64 {
            return Err(refused());
        }
        let mut bytes = Vec::new();
        file.take(maximum as u64 + 1).read_to_end(&mut bytes)?;
        if bytes.len() > maximum || bytes.len() < 8 {
            return Err(refused());
        }
        Ok(Some(bytes))
    }
    fn publish(
        &self,
        temporary: &'static str,
        final_name: &'static str,
        bytes: &[u8],
        maximum: usize,
    ) -> io::Result<()> {
        if bytes.len() < 8 || bytes.len() > maximum {
            return Err(refused());
        }
        if let Some(saved) = self.read(final_name, maximum)? {
            return if saved == bytes {
                Ok(())
            } else {
                Err(refused())
            };
        }
        let fd = fs::openat(
            &self.directory,
            temporary,
            OFlags::WRONLY | OFlags::CREATE | OFlags::EXCL | OFlags::NOFOLLOW | OFlags::CLOEXEC,
            Mode::RUSR | Mode::WUSR,
        )?;
        let mut file = File::from(fd);
        if !file.metadata()?.is_file() {
            return Err(refused());
        }
        let result = (|| {
            file.write_all(bytes)?;
            file.sync_all()?;
            fs::renameat_with(
                &self.directory,
                temporary,
                &self.directory,
                final_name,
                RenameFlags::NOREPLACE,
            )?;
            fs::fsync(&self.directory)?;
            Ok(())
        })();
        if result.is_err() {
            let _ = fs::unlinkat(&self.directory, temporary, AtFlags::empty());
        }
        result
    }
    pub(crate) fn publish_request(
        &self,
        observed: &MailboxIdentity,
        bytes: &[u8],
    ) -> io::Result<()> {
        self.check(observed)?;
        self.publish("request.partial", "request.cp1", bytes, REQUEST_LIMIT)
    }
    // The Deno helper reads requests; the native Rust bridge only publishes them.
    #[allow(dead_code)]
    pub(crate) fn read_request(&self, observed: &MailboxIdentity) -> io::Result<Option<Vec<u8>>> {
        self.check(observed)?;
        self.read("request.cp1", REQUEST_LIMIT)
    }
    // The Deno helper publishes replies; the native Rust bridge only reads them.
    #[allow(dead_code)]
    pub(crate) fn publish_reply(&self, observed: &MailboxIdentity, bytes: &[u8]) -> io::Result<()> {
        self.check(observed)?;
        self.publish("reply.partial", "reply.cp1", bytes, REPLY_LIMIT)
    }
    pub(crate) fn read_reply(&self, observed: &MailboxIdentity) -> io::Result<Option<Vec<u8>>> {
        self.check(observed)?;
        self.read("reply.cp1", REPLY_LIMIT)
    }
    /// Retire only after the native pipe owner delivers the exact committed reply.
    pub(crate) fn retire(&self, observed: &MailboxIdentity) -> io::Result<()> {
        self.check(observed)?;
        // Open with NOFOLLOW and verify regular type before each unlink.
        for name in ["request.cp1", "reply.cp1"] {
            if self.read(name, REPLY_LIMIT)?.is_none() {
                return Err(refused());
            }
        }
        fs::unlinkat(&self.directory, "request.cp1", AtFlags::empty())?;
        fs::unlinkat(&self.directory, "reply.cp1", AtFlags::empty())?;
        fs::fsync(&self.directory)?;
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::os::unix::fs::symlink;
    fn identity() -> MailboxIdentity {
        MailboxIdentity::from_verified_runtime("original-runtime".into(), [1; 32]).unwrap()
    }
    fn root(path: &std::path::Path) -> OwnedFd {
        fs::open(
            path,
            OFlags::RDONLY | OFlags::DIRECTORY | OFlags::NOFOLLOW,
            Mode::empty(),
        )
        .unwrap()
    }
    #[test]
    fn refuses_linked_mailbox_and_files() {
        let workspace = tempfile::tempdir().unwrap();
        let other = tempfile::tempdir().unwrap();
        symlink(other.path(), workspace.path().join(".elitea-platform")).unwrap();
        assert!(Mailbox::at(&root(workspace.path()), identity()).is_err());
        std::fs::remove_file(workspace.path().join(".elitea-platform")).unwrap();
        let mailbox = Mailbox::at(&root(workspace.path()), identity()).unwrap();
        symlink(
            other.path().join("private"),
            workspace.path().join(".elitea-platform/request.cp1"),
        )
        .unwrap();
        assert!(mailbox.read_request(&identity()).is_err());
        assert!(mailbox.publish_request(&identity(), &[1; 8]).is_err());
    }
    #[test]
    fn identity_bounds_exact_replay_and_retirement() {
        let workspace = tempfile::tempdir().unwrap();
        let mailbox = Mailbox::at(&root(workspace.path()), identity()).unwrap();
        let wrong =
            MailboxIdentity::from_verified_runtime("replacement-runtime".into(), [1; 32]).unwrap();
        assert!(mailbox.publish_request(&wrong, &[1; 8]).is_err());
        mailbox.publish_request(&identity(), &[1; 8]).unwrap();
        mailbox.publish_request(&identity(), &[1; 8]).unwrap();
        assert!(mailbox.publish_request(&identity(), &[2; 8]).is_err());
        assert!(
            mailbox
                .publish_reply(&identity(), &vec![1; REPLY_LIMIT + 1])
                .is_err()
        );
        mailbox.publish_reply(&identity(), &[3; 8]).unwrap();
        assert_eq!(mailbox.read_request(&identity()).unwrap(), Some(vec![1; 8]));
        assert_eq!(mailbox.read_reply(&identity()).unwrap(), Some(vec![3; 8]));
        mailbox.retire(&identity()).unwrap();
        assert!(mailbox.read_request(&identity()).unwrap().is_none());
    }
}

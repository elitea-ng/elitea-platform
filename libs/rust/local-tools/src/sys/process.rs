//! Process groups on Unix: the group a command leads (`process_group(0)`),
//! signalled as a whole. [`Group`] has the same API as the Windows Job
//! Object stand-in, so the tools stop a command's tree the same way on
//! every system.

pub use rustix::process::{Signal, geteuid, getuid};

/// A command's process group: its leader's pid.
#[derive(Clone, Debug)]
pub struct Group(rustix::process::Pid);

impl Group {
    fn of_pid(pid: Option<u32>) -> Option<Self> {
        pid.and_then(|pid| i32::try_from(pid).ok())
            .and_then(rustix::process::Pid::from_raw)
            .map(Self)
    }

    /// The group of a started `std` child (spawned with `process_group(0)`).
    #[must_use]
    pub fn of_child(child: &std::process::Child) -> Option<Self> {
        Self::of_pid(Some(child.id()))
    }

    /// The group of a started tokio child (`None` once it was reaped).
    #[must_use]
    pub fn of_tokio(child: &tokio::process::Child) -> Option<Self> {
        Self::of_pid(child.id())
    }

    /// The group of a child on a pseudo-terminal (its session's leader).
    #[must_use]
    pub fn of_pty(child: &(dyn portable_pty::Child + Send + Sync)) -> Option<Self> {
        Self::of_pid(child.process_id())
    }

    /// Send `signal` to every process in the group.
    pub fn signal(&self, signal: Signal) {
        let _ = rustix::process::kill_process_group(self.0, signal);
    }
}

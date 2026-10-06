//! Own one hydration process group, deadline, cancellation path, and retry lock.
use rustix::process::{
    Pid, Signal, WaitId, WaitIdOptions, WaitOptions, kill_process_group, test_kill_process_group,
    waitid, waitpgid,
};
use std::{
    fs, io,
    os::unix::process::CommandExt,
    path::{Path, PathBuf},
    process::{Child, Command, Stdio},
    time::{Duration, Instant},
};
use tokio::signal::unix::{SignalKind, signal};

const CLEANUP_LIMIT: Duration = Duration::from_secs(2);

struct OwnedGroup {
    // Keep the unreaped group leader alive as a PID identity until group cleanup.
    _child: Child,
    group: Pid,
    lock: PathBuf,
    closed: bool,
}
impl OwnedGroup {
    fn spawn(mut command: Command, base: &Path) -> io::Result<Self> {
        #[cfg(target_os = "linux")]
        // The Linux kernel expects a boolean. rustix represents enabled as Pid::INIT.
        rustix::process::set_child_subreaper(Some(Pid::INIT))?;
        let lock = base.join(".elitea-native-finalizing");
        fs::create_dir(&lock)?;
        command
            .process_group(0)
            .stdin(Stdio::null())
            .stdout(Stdio::null())
            .stderr(Stdio::null());
        match command.spawn() {
            Ok(child) => {
                let group = Pid::from_child(&child);
                Ok(Self {
                    _child: child,
                    group,
                    lock,
                    closed: false,
                })
            }
            Err(error) => {
                fs::remove_dir(lock)?;
                Err(error)
            }
        }
    }
    fn completed(&self) -> io::Result<Option<bool>> {
        // NOWAIT prevents PID reuse before the entire owned group is killed.
        let status = waitid(
            WaitId::Pid(self.group),
            WaitIdOptions::EXITED | WaitIdOptions::NOHANG | WaitIdOptions::NOWAIT,
        )?;
        Ok(status.map(|status| status.exited() && status.exit_status() == Some(0)))
    }
    fn close(&mut self) -> io::Result<()> {
        self.closed = true;
        match kill_process_group(self.group, Signal::KILL) {
            Ok(()) | Err(rustix::io::Errno::SRCH) => {}
            Err(error) => return Err(error.into()),
        }
        // Adopt and reap helper descendants on Linux, without waiting for PID 1 exit.
        // Cleanup is synchronous so dropping the owner cannot detach a cleanup task.
        let deadline = Instant::now() + CLEANUP_LIMIT;
        let mut leader_reaped = false;
        loop {
            match waitpgid(self.group, WaitOptions::NOHANG) {
                Ok(Some((pid, _))) => {
                    leader_reaped |= pid == self.group;
                    continue;
                }
                Ok(None) => {}
                Err(rustix::io::Errno::CHILD) => leader_reaped = true,
                Err(error) => return Err(error.into()),
            }
            match test_kill_process_group(self.group) {
                Err(rustix::io::Errno::SRCH) if leader_reaped => {
                    return fs::remove_dir_all(&self.lock);
                }
                Err(rustix::io::Errno::SRCH) => {}
                Ok(()) => {}
                Err(error) => return Err(error.into()),
            }
            if Instant::now() >= deadline {
                // Keep the lock after incomplete cleanup. A retry cannot overlap this group.
                return Err(io::Error::new(
                    io::ErrorKind::TimedOut,
                    "native process cleanup incomplete",
                ));
            }
            std::thread::sleep(Duration::from_millis(5));
        }
    }
}
impl Drop for OwnedGroup {
    fn drop(&mut self) {
        if !self.closed {
            let _ = self.close();
        }
    }
}

pub(super) async fn run(command: Command, base: &Path, deadline: Instant) -> io::Result<()> {
    // Install handlers before launching a helper. SIGKILL still requires outer runtime termination.
    let mut terminate = signal(SignalKind::terminate())?;
    let mut interrupt = signal(SignalKind::interrupt())?;
    if Instant::now() >= deadline {
        return Err(io::Error::new(
            io::ErrorKind::TimedOut,
            "native finalization deadline exceeded",
        ));
    }
    let mut owner = OwnedGroup::spawn(command, base)?;
    let mut poll = tokio::time::interval(Duration::from_millis(10));
    poll.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Skip);
    let expiry = tokio::time::sleep_until(tokio::time::Instant::from_std(deadline));
    tokio::pin!(expiry);
    let result = loop {
        tokio::select! {
            biased;
            _ = terminate.recv() => break Err(io::Error::new(io::ErrorKind::Interrupted, "native finalization cancelled")),
            _ = interrupt.recv() => break Err(io::Error::new(io::ErrorKind::Interrupted, "native finalization cancelled")),
            () = &mut expiry => break Err(io::Error::new(io::ErrorKind::TimedOut, "native finalization deadline exceeded")),
            _ = poll.tick() => match owner.completed() {
                Ok(Some(true)) => break Ok(()),
                Ok(Some(false)) => break Err(io::Error::other("native hydration failed")),
                Ok(None) => {},
                Err(error) => break Err(error),
            }
        }
    };
    // Even successful helpers cannot leave detached verifier descendants.
    owner.close()?;
    result
}

#[cfg(test)]
mod tests {
    use super::*;
    fn helper(base: &Path, exit: bool) -> Command {
        let mut command = Command::new("/bin/sh");
        command
            .arg("-c")
            .arg(if exit {
                "mkdir -p .elitea-native-finalizing/staging; printf staged > .elitea-native-finalizing/staging/data; echo $$ > parent.pid; sleep 60 & echo $! > child.pid.tmp; mv child.pid.tmp child.pid; exit 0"
            } else {
                "mkdir -p .elitea-native-finalizing/staging; printf staged > .elitea-native-finalizing/staging/data; echo $$ > parent.pid; sleep 60 & echo $! > child.pid.tmp; mv child.pid.tmp child.pid; wait"
            })
            .current_dir(base);
        command
    }
    async fn child_pid(base: &Path) -> Pid {
        let deadline = tokio::time::Instant::now() + Duration::from_secs(2);
        loop {
            if let Ok(pid) = fs::read_to_string(base.join("child.pid"))
                && let Ok(raw) = pid.trim().parse()
                && let Some(pid) = Pid::from_raw(raw)
            {
                return pid;
            }
            assert!(tokio::time::Instant::now() < deadline);
            tokio::time::sleep(Duration::from_millis(5)).await;
        }
    }
    fn gone(base: &Path, child: Pid) {
        let parent = Pid::from_raw(
            fs::read_to_string(base.join("parent.pid"))
                .unwrap()
                .trim()
                .parse()
                .unwrap(),
        )
        .unwrap();
        assert_eq!(
            rustix::process::test_kill_process(parent),
            Err(rustix::io::Errno::SRCH)
        );
        assert_eq!(
            rustix::process::test_kill_process(child),
            Err(rustix::io::Errno::SRCH)
        );
        assert!(!base.join(".elitea-native-finalizing").exists());
    }
    #[tokio::test]
    async fn timeout_reaps_descendants_and_allows_same_workspace_retry() {
        let base = tempfile::tempdir().unwrap();
        let path = base.path().to_owned();
        let command = helper(&path, false);
        let task_path = path.clone();
        let task = tokio::spawn(async move {
            run(command, &task_path, Instant::now() + Duration::from_secs(1)).await
        });
        let child = child_pid(&path).await;
        assert_eq!(
            task.await.unwrap().unwrap_err().kind(),
            io::ErrorKind::TimedOut
        );
        gone(&path, child);
        fs::remove_file(path.join("child.pid")).unwrap();
        let child_command = helper(&path, true);
        let retry_path = path.clone();
        let retry = tokio::spawn(async move {
            run(
                child_command,
                &retry_path,
                Instant::now() + Duration::from_secs(2),
            )
            .await
        });
        let child = child_pid(&path).await;
        retry.await.unwrap().unwrap();
        gone(&path, child);
    }
    #[tokio::test]
    async fn owner_drop_reaps_descendants_before_join_and_retry() {
        let base = tempfile::tempdir().unwrap();
        let path = base.path().to_owned();
        let command = helper(&path, false);
        let task_path = path.clone();
        let task = tokio::spawn(async move {
            run(
                command,
                &task_path,
                Instant::now() + Duration::from_secs(60),
            )
            .await
        });
        let child = child_pid(&path).await;
        task.abort();
        assert!(task.await.unwrap_err().is_cancelled());
        gone(&path, child);
    }
    #[tokio::test]
    async fn active_owner_rejects_overlapping_finalization() {
        let base = tempfile::tempdir().unwrap();
        let path = base.path().to_owned();
        let command = helper(&path, false);
        let task_path = path.clone();
        let task = tokio::spawn(async move {
            run(
                command,
                &task_path,
                Instant::now() + Duration::from_secs(60),
            )
            .await
        });
        let child = child_pid(&path).await;
        let error = run(
            helper(&path, true),
            &path,
            Instant::now() + Duration::from_secs(1),
        )
        .await
        .unwrap_err();
        assert_eq!(error.kind(), io::ErrorKind::AlreadyExists);
        task.abort();
        assert!(task.await.unwrap_err().is_cancelled());
        gone(&path, child);
    }
}

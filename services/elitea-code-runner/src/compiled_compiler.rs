//! Compile-only ownership. Capture is permitted only after complete child reaping.
use std::{io, path::Path, process::Command, time::Instant};

#[cfg(target_os = "linux")]
fn drain_adopted_children(deadline: Instant) -> io::Result<()> {
    use rustix::process::{Pid, Signal, WaitOptions, kill_process, waitpid};
    use std::io::Read;
    use std::time::Duration;
    // native_finalization installs this process as a Linux subreaper before launch.
    // Escaped process groups are adopted here after their compiler ancestor dies.
    // Direct child PIDs cannot be recycled until this sole process lifecycle owner reaps
    // them. Do not run another child owner or waitpid loop concurrently with this.
    loop {
        let mut bytes = Vec::new();
        let mut tasks = 0usize;
        for task in std::fs::read_dir("/proc/self/task")? {
            tasks += 1;
            if tasks > 128 {
                return Err(io::Error::other("compiler task bound exceeded"));
            }
            let mut children = Vec::new();
            match std::fs::File::open(task?.path().join("children")) {
                Ok(file) => {
                    file.take(4097).read_to_end(&mut children)?;
                }
                Err(error) if error.kind() == io::ErrorKind::NotFound => continue,
                Err(error) => return Err(error),
            }
            if children.len() > 4096 || bytes.len() + children.len() > 4096 {
                return Err(io::Error::other("compiler child bound exceeded"));
            }
            bytes.extend(children);
            bytes.push(b' ');
            if bytes.len() > 4096 {
                return Err(io::Error::other("compiler child bound exceeded"));
            }
        }
        let children = std::str::from_utf8(&bytes)
            .map_err(|_| io::Error::other("invalid compiler child inventory"))?;
        let children: Vec<Pid> = children
            .split_ascii_whitespace()
            .map(|value| {
                let value = value
                    .parse::<i32>()
                    .map_err(|_| io::Error::other("invalid compiler child identity"))?;
                Pid::from_raw(value)
                    .ok_or_else(|| io::Error::other("invalid compiler child identity"))
            })
            .collect::<io::Result<_>>()?;
        if children.is_empty() {
            return Ok(());
        }
        if children.len() > 128 {
            return Err(io::Error::other("compiler child bound exceeded"));
        }
        for child in &children {
            match kill_process(*child, Signal::KILL) {
                Ok(()) | Err(rustix::io::Errno::SRCH) => {}
                Err(error) => return Err(error.into()),
            }
        }
        for child in &children {
            match waitpid(Some(*child), WaitOptions::NOHANG) {
                Ok(_) => {}
                // A child may have been reaped while its owned process group closed.
                Err(rustix::io::Errno::CHILD) => {}
                Err(error) => return Err(error.into()),
            }
        }
        if Instant::now() >= deadline {
            return Err(io::Error::new(
                io::ErrorKind::TimedOut,
                "compiler descendants remain",
            ));
        }
        std::thread::sleep(Duration::from_millis(5));
    }
}

pub(crate) async fn run(command: Command, base: &Path, deadline: Instant) -> io::Result<()> {
    #[cfg(target_os = "linux")]
    {
        let result = crate::native_finalization::run(command, base, deadline).await;
        // This closes setsid/process-group escapes as well as ordinary children.
        // Any incomplete cleanup denies artifact capture, including on success.
        drain_adopted_children(Instant::now() + std::time::Duration::from_secs(2))?;
        result
    }
    #[cfg(not(target_os = "linux"))]
    {
        let _ = (command, base, deadline);
        Err(io::Error::other(
            "snapshot compilation requires Linux child ownership",
        ))
    }
}

#[cfg(all(test, target_os = "linux"))]
mod tests {
    use super::*;
    use std::process::Child;
    use std::time::Duration;

    const CHILD_TEST: &str = "ELITEA_CODE_COMPILER_TEST_CHILD";
    const SUCCESS_TEST: &str =
        "compiled_compiler::tests::successful_compiler_cannot_leave_setsid_writer_before_capture";
    const FAILURE_TEST: &str =
        "compiled_compiler::tests::failing_compiler_also_reaps_before_returning_error";

    struct FixtureChild(Child);

    impl Drop for FixtureChild {
        fn drop(&mut self) {
            if !matches!(self.0.try_wait(), Ok(Some(_))) {
                let _ = self.0.kill();
                let _ = self.0.wait();
            }
        }
    }

    fn isolated_test(name: &str) -> bool {
        if std::env::var(CHILD_TEST).as_deref() == Ok(name) {
            // The child runs one exact fixture, with the production sole-owner model.
            let arguments: Vec<_> = std::env::args().collect();
            assert!(arguments.windows(2).any(|pair| pair == ["--exact", name]));
            return false;
        }
        // /proc-wide compiler reaping must not share a process with other fixtures.
        // Keep Cargo, hydration and finalization tests concurrent in their own process.
        let mut child = FixtureChild(
            Command::new(std::env::current_exe().unwrap())
                .args(["--exact", name, "--nocapture"])
                .env(CHILD_TEST, name)
                .spawn()
                .unwrap(),
        );
        let deadline = Instant::now() + Duration::from_secs(10);
        loop {
            if let Some(status) = child.0.try_wait().unwrap() {
                assert!(status.success(), "isolated compiler fixture failed");
                return true;
            }
            assert!(
                Instant::now() < deadline,
                "isolated compiler fixture expired"
            );
            std::thread::sleep(Duration::from_millis(5));
        }
    }

    #[tokio::test(flavor = "current_thread")]
    async fn successful_compiler_cannot_leave_setsid_writer_before_capture() {
        if isolated_test(SUCCESS_TEST) {
            return;
        }
        let root = tempfile::tempdir().unwrap();
        let mut command = Command::new("/bin/sh");
        command.current_dir(root.path()).arg("-c").arg("setsid /bin/sh -c 'echo $$ > escaped.pid; while :; do printf poison > later-output; sleep 1; done' </dev/null >/dev/null 2>&1 & while [ ! -f escaped.pid ]; do :; done; exit 0");
        run(
            command,
            root.path(),
            Instant::now() + Duration::from_secs(5),
        )
        .await
        .unwrap();
        let raw = std::fs::read_to_string(root.path().join("escaped.pid"))
            .unwrap()
            .trim()
            .parse()
            .unwrap();
        let pid = rustix::process::Pid::from_raw(raw).unwrap();
        assert_eq!(
            rustix::process::test_kill_process(pid),
            Err(rustix::io::Errno::SRCH)
        );
        assert!(
            std::fs::read_to_string("/proc/thread-self/children")
                .unwrap()
                .trim()
                .is_empty()
        );
    }
    #[tokio::test(flavor = "current_thread")]
    async fn failing_compiler_also_reaps_before_returning_error() {
        if isolated_test(FAILURE_TEST) {
            return;
        }
        let root = tempfile::tempdir().unwrap();
        let mut command = Command::new("/bin/sh");
        command.arg("-c").arg("sleep 60 & exit 7");
        assert!(
            run(
                command,
                root.path(),
                Instant::now() + Duration::from_secs(5)
            )
            .await
            .is_err()
        );
        assert!(
            std::fs::read_to_string("/proc/thread-self/children")
                .unwrap()
                .trim()
                .is_empty()
        );
    }

    #[test]
    fn compiler_fixture_preserves_another_process_owner() {
        let mut unrelated = FixtureChild(Command::new("/bin/sleep").arg("60").spawn().unwrap());
        assert!(isolated_test(SUCCESS_TEST));
        assert!(unrelated.0.try_wait().unwrap().is_none());
        assert!(
            rustix::process::test_kill_process(rustix::process::Pid::from_child(&unrelated.0))
                .is_ok()
        );
    }
}

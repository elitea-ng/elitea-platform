//! The cgroup v2 facts the native runner reads: the container's memory
//! limit (`memory.max`, the default of the worker's address-space cap) and
//! its OOM-kill count (`memory.events`, how a SIGKILL the parent did not
//! send is explained).
//!
//! Only cgroup v2 (the unified hierarchy) is read. On a host without it
//! (macOS, cgroup v1) every fact is `None` and the callers use their
//! defaults.

use std::path::{Path, PathBuf};

/// Where the kernel lists this process's cgroups.
pub const MEMBERSHIP: &str = "/proc/self/cgroup";

/// Where the unified hierarchy is mounted.
pub const MOUNT: &str = "/sys/fs/cgroup";

/// This process's cgroup v2 directory.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Cgroup {
    dir: PathBuf,
}

impl Cgroup {
    /// The cgroup of this process, when cgroup v2 is mounted.
    #[must_use]
    pub fn discover() -> Option<Self> {
        Self::discover_in(Path::new(MEMBERSHIP), Path::new(MOUNT))
    }

    /// The cgroup `membership` (a `/proc/<pid>/cgroup` file) names under
    /// `mount`. The unified hierarchy's line is `0::<path>`. In a container
    /// with its own cgroup namespace the path is `/` and the mount is the
    /// container's cgroup; otherwise the path is tried under the mount
    /// first, then the mount itself.
    #[must_use]
    pub fn discover_in(membership: &Path, mount: &Path) -> Option<Self> {
        let text = std::fs::read_to_string(membership).ok()?;
        let relative = text.lines().find_map(|line| line.strip_prefix("0::"))?;
        let relative = relative.trim().trim_start_matches('/');
        [mount.join(relative), mount.to_path_buf()]
            .into_iter()
            .find(|dir| dir.join("memory.max").is_file() || dir.join("memory.events").is_file())
            .map(|dir| Self { dir })
    }

    /// The cgroup at `dir` (tests).
    #[must_use]
    pub fn at(dir: impl Into<PathBuf>) -> Self {
        Self { dir: dir.into() }
    }

    /// `memory.max` in bytes; `None` when unlimited (`max`) or unreadable.
    #[must_use]
    pub fn memory_max(&self) -> Option<u64> {
        let text = std::fs::read_to_string(self.dir.join("memory.max")).ok()?;
        text.trim().parse::<u64>().ok()
    }

    /// The `oom_kill` count of `memory.events`; `None` when unreadable.
    #[must_use]
    pub fn oom_kills(&self) -> Option<u64> {
        let text = std::fs::read_to_string(self.dir.join("memory.events")).ok()?;
        text.lines().find_map(|line| {
            line.strip_prefix("oom_kill ")
                .and_then(|count| count.trim().parse().ok())
        })
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn root(name: &str) -> PathBuf {
        let dir = std::env::temp_dir().join(format!("dw-cgroup-{name}-{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&dir);
        std::fs::create_dir_all(&dir).unwrap_or_else(|e| panic!("{e}"));
        dir
    }

    fn write(path: &Path, text: &str) {
        if let Some(parent) = path.parent() {
            std::fs::create_dir_all(parent).unwrap_or_else(|e| panic!("{e}"));
        }
        std::fs::write(path, text).unwrap_or_else(|e| panic!("{e}"));
    }

    #[test]
    fn a_namespaced_container_reads_the_mount_itself() {
        let dir = root("ns");
        write(&dir.join("self-cgroup"), "0::/\n");
        write(&dir.join("mount/memory.max"), "4294967296\n");
        write(
            &dir.join("mount/memory.events"),
            "low 0\nhigh 0\nmax 12\noom 2\noom_kill 1\noom_group_kill 0\n",
        );
        let cgroup = Cgroup::discover_in(&dir.join("self-cgroup"), &dir.join("mount"));
        assert_eq!(cgroup, Some(Cgroup::at(dir.join("mount/"))));
        let cgroup = cgroup.unwrap_or_else(|| Cgroup::at("/nowhere"));
        assert_eq!(cgroup.memory_max(), Some(4 << 30));
        assert_eq!(cgroup.oom_kills(), Some(1));
        let _ = std::fs::remove_dir_all(&dir);
    }

    #[test]
    fn a_nested_cgroup_is_found_under_the_mount() {
        let dir = root("nested");
        write(
            &dir.join("self-cgroup"),
            "12:cpu:/ignored\n0::/kubepods/pod1/c1\n",
        );
        write(&dir.join("mount/kubepods/pod1/c1/memory.max"), "max\n");
        let cgroup = Cgroup::discover_in(&dir.join("self-cgroup"), &dir.join("mount"));
        assert_eq!(cgroup, Some(Cgroup::at(dir.join("mount/kubepods/pod1/c1"))));
        // `max` is no limit; no events file is no count.
        let cgroup = cgroup.unwrap_or_else(|| Cgroup::at("/nowhere"));
        assert_eq!(cgroup.memory_max(), None);
        assert_eq!(cgroup.oom_kills(), None);
        let _ = std::fs::remove_dir_all(&dir);
    }

    #[test]
    fn no_unified_hierarchy_is_no_cgroup() {
        let dir = root("none");
        write(&dir.join("v1"), "4:memory:/docker/abc\n");
        assert_eq!(Cgroup::discover_in(&dir.join("v1"), &dir), None);
        assert_eq!(Cgroup::discover_in(&dir.join("missing"), &dir), None);
        let _ = std::fs::remove_dir_all(&dir);
    }
}

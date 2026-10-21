//! Watching a workspace folder for changes made outside the app: the
//! person's editor, a `git pull`, a build.
//!
//! [`FolderWatcher`] subscribes to the operating system's file events for
//! the folder (`FSEvents` on macOS, inotify on Linux) and reports, at most
//! once per [`WatchConfig::min_interval`] and only after the folder has been
//! quiet for [`WatchConfig::quiet`], how many paths changed. It never reads
//! a file: what changed is found by the refresh that follows, through the
//! same confined walk as every build.
//!
//! An event only counts when the walk would list its path: not under
//! `.git`, not denied (`path_deny`, the host's deny list), and not ignored
//! by a `.gitignore` on the way to it (or `.git/info/exclude`). So a build
//! writing to an ignored `target/` or `node_modules/` refreshes nothing.
//! A changed `.gitignore` always counts (it changes the listing) and drops
//! the cached rules of its directory.

use elitea_local_tools::workspace::{Intent, Workspace, WsPath, is_protected_name};
use ignore::gitignore::{Gitignore, GitignoreBuilder};
use notify::{EventKind, RecursiveMode, Watcher as _};
use std::collections::{HashMap, HashSet};
use std::path::{Path, PathBuf};
use std::sync::Arc;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::mpsc;
use std::time::{Duration, Instant};

/// When a watcher reports.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct WatchConfig {
    /// How long the folder must be quiet before a report.
    pub quiet: Duration,
    /// The least time between two reports.
    pub min_interval: Duration,
}

impl Default for WatchConfig {
    /// 3 s quiet, 30 s between reports.
    fn default() -> Self {
        Self {
            quiet: Duration::from_secs(3),
            min_interval: Duration::from_secs(30),
        }
    }
}

/// What a watcher calls with the number of paths that changed.
pub type OnChange = Box<dyn Fn(u64) + Send + Sync>;

/// One folder's watch; dropping it stops it.
pub struct FolderWatcher {
    /// The operating system's watch (`None`: a test feeds the events).
    _watcher: Option<notify::RecommendedWatcher>,
    stop: Arc<AtomicBool>,
    thread: Option<std::thread::JoinHandle<()>>,
}

impl std::fmt::Debug for FolderWatcher {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("FolderWatcher").finish_non_exhaustive()
    }
}

impl Drop for FolderWatcher {
    fn drop(&mut self) {
        self.stop.store(true, Ordering::SeqCst);
        if let Some(thread) = self.thread.take() {
            let _ = thread.join();
        }
    }
}

/// The ignore rules on the way to a path, read once per directory.
struct Rules {
    root: PathBuf,
    /// A directory's `.gitignore` (`None`: it has none).
    by_dir: HashMap<PathBuf, Option<Gitignore>>,
    exclude: Option<Gitignore>,
}

fn matcher(dir: &Path, file: &Path) -> Option<Gitignore> {
    if !file.is_file() {
        return None;
    }
    let mut builder = GitignoreBuilder::new(dir);
    builder.add(file);
    builder.build().ok()
}

impl Rules {
    fn new(root: &Path) -> Self {
        Self {
            root: root.to_owned(),
            by_dir: HashMap::new(),
            exclude: matcher(root, &root.join(".git/info/exclude")),
        }
    }

    fn of(&mut self, dir: &Path) -> Option<&Gitignore> {
        self.by_dir
            .entry(dir.to_owned())
            .or_insert_with(|| matcher(dir, &dir.join(".gitignore")))
            .as_ref()
    }

    /// Whether `.gitignore` files (deepest first) or the exclude file
    /// ignore `path`.
    fn ignored(&mut self, path: &Path, is_dir: bool) -> bool {
        let mut dirs: Vec<PathBuf> = path
            .ancestors()
            .skip(1)
            .take_while(|dir| dir.starts_with(&self.root))
            .map(Path::to_path_buf)
            .collect();
        dirs.sort_by_key(|dir| std::cmp::Reverse(dir.components().count()));
        for dir in dirs {
            if let Some(rules) = self.of(&dir) {
                let verdict = rules.matched_path_or_any_parents(path, is_dir);
                if verdict.is_ignore() {
                    return true;
                }
                if verdict.is_whitelist() {
                    return false;
                }
            }
        }
        self.exclude
            .as_ref()
            .is_some_and(|rules| rules.matched_path_or_any_parents(path, is_dir).is_ignore())
    }

    fn forget(&mut self, dir: &Path) {
        self.by_dir.remove(dir);
    }
}

/// Whether a change at `absolute` is one the walk would see.
fn relevant(workspace: &Workspace, rules: &mut Rules, absolute: &Path) -> bool {
    let Ok(relative) = absolute.strip_prefix(workspace.root()) else {
        return false;
    };
    let Ok(path) = WsPath::from_relative(relative) else {
        return false;
    };
    if path.is_root() || path.components().iter().any(|c| is_protected_name(c)) {
        return false;
    }
    if workspace.denied_by(&path, Intent::Read).is_some() {
        return false;
    }
    if absolute
        .file_name()
        .is_some_and(|name| name == ".gitignore")
    {
        if let Some(dir) = absolute.parent() {
            rules.forget(dir);
        }
        return true;
    }
    let is_dir = std::fs::symlink_metadata(absolute).is_ok_and(|meta| meta.is_dir());
    !rules.ignored(absolute, is_dir)
}

impl FolderWatcher {
    /// Watch `workspace`'s folder, calling `on_change` (on the watcher's
    /// own thread) as [`WatchConfig`] says.
    ///
    /// # Errors
    ///
    /// The operating system refused the watch (too many watches, the
    /// folder is gone).
    pub fn start(
        workspace: Arc<Workspace>,
        config: WatchConfig,
        on_change: OnChange,
    ) -> Result<Self, String> {
        let (events, received) = mpsc::channel::<notify::Result<notify::Event>>();
        let mut watcher = notify::recommended_watcher(move |event| drop(events.send(event)))
            .map_err(|error| error.to_string())?;
        watcher
            .watch(workspace.root(), RecursiveMode::Recursive)
            .map_err(|error| error.to_string())?;
        Self::spawn(Some(watcher), workspace, config, on_change, received)
    }

    /// A watcher fed by `received` instead of the operating system (tests:
    /// events without a real folder's timing).
    #[cfg(test)]
    fn with_events(
        workspace: Arc<Workspace>,
        config: WatchConfig,
        on_change: OnChange,
        received: mpsc::Receiver<notify::Result<notify::Event>>,
    ) -> Result<Self, String> {
        Self::spawn(None, workspace, config, on_change, received)
    }

    fn spawn(
        watcher: Option<notify::RecommendedWatcher>,
        workspace: Arc<Workspace>,
        config: WatchConfig,
        on_change: OnChange,
        received: mpsc::Receiver<notify::Result<notify::Event>>,
    ) -> Result<Self, String> {
        let stop = Arc::new(AtomicBool::new(false));
        let stopped = Arc::clone(&stop);
        let thread = std::thread::Builder::new()
            .name("index-watch".into())
            .spawn(move || run(&workspace, config, &on_change, &received, &stopped))
            .map_err(|error| error.to_string())?;
        Ok(Self {
            _watcher: watcher,
            stop,
            thread: Some(thread),
        })
    }
}

/// The watcher's loop: gather relevant paths; report once quiet, spaced.
fn run(
    workspace: &Workspace,
    config: WatchConfig,
    on_change: &OnChange,
    received: &mpsc::Receiver<notify::Result<notify::Event>>,
    stop: &AtomicBool,
) {
    let mut rules = Rules::new(workspace.root());
    let mut pending: HashSet<PathBuf> = HashSet::new();
    let mut last_event = Instant::now();
    let mut last_report: Option<Instant> = None;
    // A stop is noticed within this long.
    let tick = Duration::from_millis(250);
    while !stop.load(Ordering::SeqCst) {
        let now = Instant::now();
        let wait = if pending.is_empty() {
            tick
        } else {
            let quiet_at = last_event + config.quiet;
            let spaced_at = last_report.map_or(now, |at| at + config.min_interval);
            quiet_at
                .max(spaced_at)
                .saturating_duration_since(now)
                .min(tick)
        };
        match received.recv_timeout(wait) {
            // The system dropped events (inotify `Q_OVERFLOW`, FSEvents
            // `MustScanSubDirs`; no paths): the whole folder may have
            // changed. The refresh that follows stat-walks it (never a
            // rebuild).
            Ok(Ok(event)) if event.need_rescan() => {
                pending.insert(workspace.root().to_owned());
                last_event = Instant::now();
            }
            Ok(Ok(event)) => {
                if matches!(event.kind, EventKind::Access(_)) {
                    continue;
                }
                for path in &event.paths {
                    if relevant(workspace, &mut rules, path) {
                        pending.insert(path.clone());
                        last_event = Instant::now();
                    }
                }
            }
            // An overflow or a watch error: something changed, unknown what.
            Ok(Err(_)) => {
                pending.insert(workspace.root().to_owned());
                last_event = Instant::now();
            }
            Err(mpsc::RecvTimeoutError::Timeout) => {}
            Err(mpsc::RecvTimeoutError::Disconnected) => return,
        }
        let now = Instant::now();
        if !pending.is_empty()
            && now.duration_since(last_event) >= config.quiet
            && last_report.is_none_or(|at| now.duration_since(at) >= config.min_interval)
        {
            let changed = u64::try_from(pending.len()).unwrap_or(u64::MAX);
            pending.clear();
            last_report = Some(now);
            on_change(changed);
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use notify::event::{AccessKind, Flag, ModifyKind};
    use std::sync::Mutex;

    type Reports = Arc<Mutex<Vec<(Instant, u64)>>>;

    fn fast() -> WatchConfig {
        WatchConfig {
            quiet: Duration::from_millis(300),
            min_interval: Duration::from_millis(1500),
        }
    }

    /// A folder with ignored, `.git` and denied places.
    fn folder() -> tempfile::TempDir {
        let folder = tempfile::tempdir().unwrap();
        std::fs::write(folder.path().join(".gitignore"), "target/\n*.log\n").unwrap();
        for dir in ["src", "target", ".git", "secrets"] {
            std::fs::create_dir_all(folder.path().join(dir)).unwrap();
        }
        folder
    }

    fn recorder() -> (Reports, OnChange) {
        let reports: Reports = Arc::new(Mutex::new(Vec::new()));
        let seen = Arc::clone(&reports);
        let on_change: OnChange = Box::new(move |n| seen.lock().unwrap().push((Instant::now(), n)));
        (reports, on_change)
    }

    /// A watcher fed by hand: no real file events, no FS timing.
    struct Fed {
        _folder: tempfile::TempDir,
        root: PathBuf,
        events: mpsc::Sender<notify::Result<notify::Event>>,
        reports: Reports,
        _watcher: FolderWatcher,
    }

    impl Fed {
        fn new(config: WatchConfig, path_deny: &[String]) -> Self {
            let folder = folder();
            let workspace = Arc::new(Workspace::open(folder.path(), path_deny).unwrap());
            let root = workspace.root().to_owned();
            let (events, received) = mpsc::channel();
            let (reports, on_change) = recorder();
            let watcher =
                FolderWatcher::with_events(workspace, config, on_change, received).unwrap();
            Self {
                _folder: folder,
                root,
                events,
                reports,
                _watcher: watcher,
            }
        }

        fn modified(&self, relative: &str) {
            let event = notify::Event::new(EventKind::Modify(ModifyKind::Any))
                .add_path(self.root.join(relative));
            self.events.send(Ok(event)).unwrap();
        }

        fn reports(&self) -> Vec<(Instant, u64)> {
            self.reports.lock().unwrap().clone()
        }
    }

    fn wait_for(reports: &Mutex<Vec<(Instant, u64)>>, count: usize, within: Duration) -> bool {
        let deadline = Instant::now() + within;
        while Instant::now() < deadline {
            if reports.lock().unwrap().len() >= count {
                return true;
            }
            std::thread::sleep(Duration::from_millis(20));
        }
        false
    }

    /// A burst of changes (repeats included) is one report, once the
    /// folder has been quiet, counting each path once.
    #[test]
    fn debounce_coalesces_bursts() {
        let fed = Fed::new(fast(), &[]);
        for round in 0..3 {
            for n in 0..4 {
                fed.modified(&format!("src/{n}.rs"));
            }
            if round < 2 {
                std::thread::sleep(fast().quiet / 3);
            }
        }
        let last = Instant::now();
        assert!(
            wait_for(&fed.reports, 1, Duration::from_secs(3)),
            "reported"
        );
        std::thread::sleep(fast().quiet * 2);
        let reports = fed.reports();
        assert_eq!(reports.len(), 1, "{reports:?}");
        let (at, files) = reports[0];
        assert!(
            at.duration_since(last) >= fast().quiet,
            "after the quiet period"
        );
        assert_eq!(files, 4, "each path once");
    }

    /// Changes during the minimum interval wait for it, then make one
    /// report.
    #[test]
    fn min_interval_between_refreshes() {
        let config = WatchConfig {
            quiet: Duration::from_millis(50),
            min_interval: Duration::from_millis(800),
        };
        let fed = Fed::new(config, &[]);
        fed.modified("src/a.rs");
        assert!(wait_for(&fed.reports, 1, Duration::from_secs(2)));
        for name in ["src/b.rs", "src/c.rs"] {
            fed.modified(name);
            std::thread::sleep(Duration::from_millis(100));
        }
        assert!(wait_for(&fed.reports, 2, Duration::from_secs(3)));
        std::thread::sleep(config.quiet * 4);
        let reports = fed.reports();
        assert_eq!(reports.len(), 2, "{reports:?}");
        let gap = reports[1].0.duration_since(reports[0].0);
        assert!(gap >= config.min_interval, "{gap:?}");
        assert_eq!(reports[1].1, 2, "both changes in the second report");
    }

    /// Ignored, `.git`, denied, outside and read-only events are no
    /// change; a changed `.gitignore` is one (it changes the listing).
    #[test]
    fn ignored_and_denied_paths_are_dropped() {
        let fed = Fed::new(fast(), &["secrets/**".to_owned()]);
        for (path, body) in [
            ("target/out.o", "x"),
            ("build.log", "x"),
            (".git/HEAD", "ref"),
            ("secrets/key.pem", "x"),
            ("src/main.rs", "fn main() {}"),
        ] {
            std::fs::write(fed.root.join(path), body).unwrap();
        }
        for path in ["target/out.o", "build.log", ".git/HEAD", "secrets/key.pem"] {
            fed.modified(path);
        }
        let outside = fed.root.parent().unwrap().join("elsewhere.rs");
        let event = notify::Event::new(EventKind::Modify(ModifyKind::Any)).add_path(outside);
        fed.events.send(Ok(event)).unwrap();
        let read = notify::Event::new(EventKind::Access(AccessKind::Any))
            .add_path(fed.root.join("src/main.rs"));
        fed.events.send(Ok(read)).unwrap();
        assert!(
            !wait_for(&fed.reports, 1, fast().quiet * 4),
            "{:?}",
            fed.reports()
        );
        fed.modified(".gitignore");
        fed.modified("src/main.rs");
        assert!(wait_for(&fed.reports, 1, Duration::from_secs(3)));
        assert_eq!(fed.reports()[0].1, 2, "only the two relevant paths");
    }

    /// An overflow (a rescan notice without paths, or a watch error) is a
    /// report: the host's refresh that follows stat-walks the whole folder
    /// (an incremental refresh, never a rebuild).
    #[test]
    fn overflow_triggers_stat_walk() {
        let fed = Fed::new(fast(), &[]);
        let overflow = notify::Event::new(EventKind::Other).set_flag(Flag::Rescan);
        fed.events.send(Ok(overflow)).unwrap();
        assert!(
            wait_for(&fed.reports, 1, Duration::from_secs(3)),
            "rescan reported"
        );
        assert_eq!(fed.reports()[0].1, 1, "the folder itself");

        let fed = Fed::new(fast(), &[]);
        fed.events
            .send(Err(notify::Error::generic("queue overflow")))
            .unwrap();
        assert!(
            wait_for(&fed.reports, 1, Duration::from_secs(3)),
            "error reported"
        );
    }

    /// With the operating system's events: a real change is reported, and
    /// none once the watcher is dropped.
    #[test]
    fn a_real_change_is_reported_and_dropping_stops_it() {
        let folder = folder();
        // Events from the setup settle before the watch starts.
        std::thread::sleep(Duration::from_millis(300));
        let workspace = Arc::new(Workspace::open(folder.path(), &[]).unwrap());
        let (reports, on_change) = recorder();
        let watcher = FolderWatcher::start(workspace, fast(), on_change).unwrap();
        std::fs::write(folder.path().join("src/a.rs"), "fn a() {}").unwrap();
        assert!(wait_for(&reports, 1, Duration::from_secs(5)), "reported");
        drop(watcher);
        let seen = reports.lock().unwrap().len();
        std::fs::write(folder.path().join("src/late.rs"), "fn late() {}").unwrap();
        assert!(!wait_for(&reports, seen + 1, Duration::from_secs(1)));
    }
}

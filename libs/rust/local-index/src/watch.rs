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
    _watcher: notify::RecommendedWatcher,
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
    use std::sync::Mutex;

    fn fast() -> WatchConfig {
        WatchConfig {
            quiet: Duration::from_millis(300),
            min_interval: Duration::from_millis(1500),
        }
    }

    struct Watched {
        folder: tempfile::TempDir,
        reports: Arc<Mutex<Vec<(Instant, u64)>>>,
        watcher: FolderWatcher,
    }

    fn watch(path_deny: &[String]) -> Watched {
        let folder = tempfile::tempdir().unwrap();
        std::fs::write(folder.path().join(".gitignore"), "target/\n*.log\n").unwrap();
        std::fs::create_dir_all(folder.path().join("src")).unwrap();
        std::fs::create_dir_all(folder.path().join("target")).unwrap();
        std::fs::create_dir_all(folder.path().join(".git")).unwrap();
        std::fs::create_dir_all(folder.path().join("secrets")).unwrap();
        // Events from the setup above settle before the watch starts.
        std::thread::sleep(Duration::from_millis(300));
        let workspace = Arc::new(Workspace::open(folder.path(), path_deny).unwrap());
        let reports = Arc::new(Mutex::new(Vec::new()));
        let seen = Arc::clone(&reports);
        let watcher = FolderWatcher::start(
            workspace,
            fast(),
            Box::new(move |n| seen.lock().unwrap().push((Instant::now(), n))),
        )
        .unwrap();
        Watched {
            folder,
            reports,
            watcher,
        }
    }

    fn wait_for(reports: &Mutex<Vec<(Instant, u64)>>, count: usize, within: Duration) -> bool {
        let deadline = Instant::now() + within;
        while Instant::now() < deadline {
            if reports.lock().unwrap().len() >= count {
                return true;
            }
            std::thread::sleep(Duration::from_millis(50));
        }
        false
    }

    #[test]
    fn a_change_is_reported_once_the_folder_is_quiet() {
        let w = watch(&[]);
        let written = Instant::now();
        for n in 0..3 {
            std::fs::write(w.folder.path().join(format!("src/{n}.rs")), "fn a() {}").unwrap();
        }
        assert!(wait_for(&w.reports, 1, Duration::from_secs(5)), "reported");
        let (at, files) = w.reports.lock().unwrap()[0];
        assert!(
            at.duration_since(written) >= fast().quiet,
            "after the quiet period"
        );
        assert!(files >= 3, "{files}");
    }

    #[test]
    fn ignored_denied_and_git_paths_are_not_changes() {
        let w = watch(&["secrets/**".to_owned()]);
        std::fs::write(w.folder.path().join("target/out.o"), "x").unwrap();
        std::fs::write(w.folder.path().join("build.log"), "x").unwrap();
        std::fs::write(w.folder.path().join(".git/HEAD"), "ref").unwrap();
        std::fs::write(w.folder.path().join("secrets/key.pem"), "x").unwrap();
        assert!(
            !wait_for(&w.reports, 1, Duration::from_secs(2)),
            "{:?}",
            w.reports.lock().unwrap()
        );
        // A changed .gitignore is a change (it changes what is listed).
        std::fs::write(w.folder.path().join(".gitignore"), "target/\n").unwrap();
        assert!(wait_for(&w.reports, 1, Duration::from_secs(5)));
    }

    #[test]
    fn reports_are_spaced_by_the_minimum_interval() {
        let w = watch(&[]);
        std::fs::write(w.folder.path().join("src/a.rs"), "fn a() {}").unwrap();
        assert!(wait_for(&w.reports, 1, Duration::from_secs(5)));
        std::fs::write(w.folder.path().join("src/b.rs"), "fn b() {}").unwrap();
        assert!(wait_for(&w.reports, 2, Duration::from_secs(6)));
        let reports = w.reports.lock().unwrap();
        let gap = reports[1].0.duration_since(reports[0].0);
        assert!(gap >= fast().min_interval, "{gap:?}");
    }

    #[test]
    fn dropping_the_watcher_stops_it() {
        let w = watch(&[]);
        let reports = Arc::clone(&w.reports);
        let folder = w.folder.path().to_owned();
        drop(w.watcher);
        std::fs::write(folder.join("src/late.rs"), "fn late() {}").unwrap();
        assert!(!wait_for(&reports, 1, Duration::from_secs(1)));
    }
}

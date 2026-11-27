//! What a command changed: the folder is scanned (stat only, never a file
//! read, see `elitea_local_tools::folder_scan`) before and after each
//! writing `run_command` and `run_tests`, and every file it created,
//! changed or deleted is recorded in the turn's changes with the command
//! that did it ([`Recorder::command_changed`]). A background process
//! writes after its call returns: the scan after its start is the base of
//! one more at the turn's end ([`Background::settle`]).
//!
//! The scans follow the rules of the folder's checkpoints
//! ([`Checkpoints::scan_rules`]): a git work tree's `.gitignore` is
//! honoured, so build output git does not show is not listed either, and a
//! plain folder's is not. A file the turn already recorded (a file tool
//! changed it first) keeps its entry: nothing is listed twice. Its
//! before-image is the file as the turn's checkpoint holds it. Without one
//! (or a file the checkpoint left out, or past [`MAX_BEFORE_IMAGES`] or
//! the recorder's budget) the row has the sizes only. A path the policy
//! denies reading is not listed at all.

use std::sync::Mutex;

use elitea_local_tools::checkpoint::MAX_COPY_FILES;
use elitea_local_tools::error::ErrorCode;
use elitea_local_tools::folder_scan::{self, FolderScan, ScanRules};
use elitea_local_tools::session::LocalSession;
use elitea_local_tools::workspace::{Intent, WsPath};
use serde_json::Value;

use super::recorder::{Image, MAX_IMAGE_BYTES, Recorder};

/// The most files a git work tree's scan lists. A repository checkpoints
/// through git whatever its size, so its scan is not held to a copy
/// checkpoint's [`MAX_COPY_FILES`]; `.gitignore` keeps build output out.
pub const MAX_GIT_SCAN_FILES: usize = 250_000;

/// The most files a scan under `rules` lists.
#[must_use]
pub fn scan_limit(rules: ScanRules) -> usize {
    match rules {
        ScanRules::Git => MAX_GIT_SCAN_FILES,
        ScanRules::Folder => MAX_COPY_FILES,
    }
}

/// The scan of the folder as it is now; `None` when it has more files
/// than [`scan_limit`] (its changes cannot be told apart). A turn that met
/// one such folder scans no more ([`Recorder::has_unlisted_commands`]).
#[must_use]
pub fn scan(session: &LocalSession) -> Option<FolderScan> {
    let rules = session.checkpoints().scan_rules();
    let found = folder_scan::scan(session.workspace().root(), rules, scan_limit(rules));
    (!found.truncated).then_some(found)
}

/// Whether a call of `tool` with `args` can change files: `run_command`
/// and `run_tests` unless read-only.
#[must_use]
pub fn may_write(tool: &str, args: &Value) -> bool {
    match tool {
        "run_command" => args.get("sandbox").and_then(Value::as_str) != Some("read-only"),
        "run_tests" => true,
        _ => false,
    }
}

/// The most before-images one command's changes read from the checkpoint;
/// past it (a formatter or a code generator rewriting thousands of files)
/// the rows have the sizes only, so the call is not held back.
pub const MAX_BEFORE_IMAGES: usize = 64;

/// A file known by its stat only (`None`: it did not exist).
fn opaque(stat: Option<folder_scan::FileStat>) -> Image {
    stat.map_or(Image::Absent, |stat| Image::Opaque {
        len: stat.len,
        mtime_ns: stat.mtime_ns,
        digest: None,
    })
}

/// The files of `paths` (with their stats in the scan before the command)
/// as they were before the turn: the first [`MAX_BEFORE_IMAGES`] that
/// existed then are read from the turn's checkpoint, looked up once
/// ([`elitea_local_tools::checkpoint::Checkpoints::files_at`]) while the recorder's budget lasts; every
/// other one, a file the checkpoint left out (too large, a placeholder) or
/// one it cannot read is known by its size only.
fn before_images(
    session: &LocalSession,
    recorder: &Recorder,
    paths: &[(String, Option<folder_scan::FileStat>)],
) -> Vec<Image> {
    let mut images: Vec<Image> = paths.iter().map(|(_, stat)| opaque(*stat)).collect();
    let Some(seq) = session.turn_checkpoint() else {
        return images;
    };
    if recorder.command_images_full() {
        return images;
    }
    let wanted: Vec<(usize, WsPath)> = paths
        .iter()
        .enumerate()
        .filter(|(_, (_, stat))| stat.is_some())
        .filter_map(|(at, (path, _))| {
            WsPath::from_relative(std::path::Path::new(path))
                .ok()
                .map(|ws| (at, ws))
        })
        .take(MAX_BEFORE_IMAGES)
        .collect();
    let targets: Vec<WsPath> = wanted.iter().map(|(_, ws)| ws.clone()).collect();
    let read = match session
        .checkpoints()
        .files_at(seq, &targets, MAX_IMAGE_BYTES)
    {
        Ok(read) => read,
        Err(error) => {
            log::debug!("no before-images: {}", error.message());
            return images;
        }
    };
    for ((at, _), answer) in wanted.into_iter().zip(read) {
        match answer {
            Ok(Some(bytes)) => images[at] = Image::Bytes(bytes),
            Ok(None) => {}
            Err(error) if error.code() == ErrorCode::TooLarge => {}
            Err(error) => log::debug!("no before-image of `{}`: {}", paths[at].0, error.message()),
        }
    }
    images
}

/// Record what changed between `before` and `after` (both scans of the
/// folder) as `command`'s. A path the policy denies reading is never
/// listed: neither its name nor what it held reaches the changes or the
/// history.
pub fn record(
    session: &LocalSession,
    recorder: &Recorder,
    before: &FolderScan,
    after: &FolderScan,
    command: &str,
) {
    let workspace = session.workspace();
    let changed: Vec<(String, Option<folder_scan::FileStat>)> = folder_scan::diff(before, after)
        .paths()
        .into_iter()
        .filter(|path| !recorder.knows(path))
        .filter(|path| {
            WsPath::from_relative(std::path::Path::new(path))
                .is_ok_and(|ws| workspace.denied_by(&ws, Intent::Read).is_none())
        })
        .map(|path| {
            let stat = before.files.get(&path).copied();
            (path, stat)
        })
        .collect();
    let images = before_images(session, recorder, &changed);
    for ((path, _), image) in changed.iter().zip(images) {
        // A placeholder that was there: its size is its stat's too.
        let image = if image == Image::Absent && before.placeholders.contains(path) {
            Image::Opaque {
                len: 0,
                mtime_ns: 0,
                digest: None,
            }
        } else {
            image
        };
        recorder.command_changed(path, image, command);
    }
}

/// The scans of one turn's background commands: the scan after the latest
/// one started, and the commands, for [`settle_background`].
#[derive(Default)]
pub struct Background {
    state: Mutex<Option<(FolderScan, Vec<String>)>>,
}

impl Background {
    /// A background command started; `after` is the folder as its call
    /// returned.
    pub fn started(&self, command: &str, after: FolderScan) {
        if let Ok(mut state) = self.state.lock() {
            let commands = state
                .take()
                .map(|(_, commands)| commands)
                .unwrap_or_default();
            let mut commands = commands;
            commands.push(command.to_owned());
            *state = Some((after, commands));
        }
    }

    /// A foreground command's scan after it ran: what background commands
    /// changed before it was taken is theirs, not the next command's.
    pub fn rebase(&self, session: &LocalSession, recorder: &Recorder, now: &FolderScan) {
        if let Ok(mut state) = self.state.lock()
            && let Some((base, commands)) = state.as_mut()
        {
            record(session, recorder, base, now, &label(commands));
            *base = now.clone();
        }
    }

    /// At the turn's end: what the background commands changed since.
    pub fn settle(&self, session: &LocalSession, recorder: &Recorder) {
        let Some((base, commands)) = self.state.lock().ok().and_then(|mut state| state.take())
        else {
            return;
        };
        match scan(session) {
            Some(now) => record(session, recorder, &base, &now, &label(&commands)),
            None => recorder.commands_unlisted(),
        }
    }
}

/// How a row names the background commands that may have changed it.
fn label(commands: &[String]) -> String {
    match commands {
        [one] => one.clone(),
        many => format!("{} background commands", many.len()),
    }
}

#[cfg(all(test, unix))]
mod tests {
    use std::sync::Arc;

    use super::*;

    #[test]
    fn a_command_with_a_read_only_sandbox_is_not_scanned() {
        assert!(may_write(
            "run_command",
            &serde_json::json!({"command": "make"})
        ));
        assert!(!may_write(
            "run_command",
            &serde_json::json!({"command": "ls", "sandbox": "read-only"})
        ));
        assert!(may_write("run_tests", &serde_json::json!({})));
        assert!(!may_write("read_file", &serde_json::json!({})));
    }

    #[test]
    fn the_label_names_one_background_command_or_counts_them() {
        assert_eq!(label(&["npm run dev".to_owned()]), "npm run dev");
        assert_eq!(
            label(&["a".to_owned(), "b".to_owned()]),
            "2 background commands"
        );
    }

    #[test]
    fn a_path_the_policy_denies_reading_is_never_listed() {
        use elitea_local_tools::approvals::{DeferringPrompt, MemoryChoices, WorkspaceSettings};
        use elitea_local_tools::policy::LocalWorkPolicy;
        use elitea_local_tools::session::SessionConfig;

        let base = tempfile::tempdir().expect("tempdir");
        let root = base.path().join("work");
        std::fs::create_dir_all(&root).expect("root");
        let session = LocalSession::open(SessionConfig {
            spill_dir: None,
            root: root.clone(),
            session_id: "s".to_owned(),
            policy: LocalWorkPolicy {
                allowed: true,
                path_deny: vec![".env".to_owned()],
                ..LocalWorkPolicy::default()
            },
            settings: WorkspaceSettings::default(),
            choices: Arc::new(MemoryChoices::default()),
            prompt: Arc::new(DeferringPrompt),
            data_dir: base.path().join("data"),
            shell: None,
            deny_read: Vec::new(),
            app_id: None,
            budgets: elitea_local_tools::budget::Budgets::default(),
            documents: elitea_local_tools::document::Documents::default(),
        })
        .expect("session");
        let stat = folder_scan::FileStat {
            len: 3,
            mtime_ns: 1,
            ino: 0,
            mode: 0o644,
        };
        let mut after = FolderScan::default();
        after.files.insert(".env".to_owned(), stat);
        after.files.insert("notes.txt".to_owned(), stat);
        let recorder = Recorder::default();
        record(&session, &recorder, &FolderScan::default(), &after, "make");
        assert!(recorder.knows("notes.txt"));
        assert!(!recorder.knows(".env"));
    }

    #[test]
    fn background_state_is_taken_once() {
        let background = Arc::new(Background::default());
        background.started("dev", FolderScan::default());
        background.started("watch", FolderScan::default());
        let taken = background.state.lock().unwrap().take();
        assert_eq!(
            taken.map(|(_, commands)| commands).unwrap(),
            ["dev", "watch"]
        );
    }
}

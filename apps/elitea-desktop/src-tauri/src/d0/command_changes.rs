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
//! (or a file the checkpoint left out) the row has the sizes only.

use std::sync::Mutex;

use elitea_local_tools::checkpoint::MAX_COPY_FILES;
use elitea_local_tools::error::ErrorCode;
use elitea_local_tools::folder_scan::{self, FolderScan};
use elitea_local_tools::session::LocalSession;
use elitea_local_tools::workspace::WsPath;
use serde_json::Value;

use super::recorder::{Image, MAX_IMAGE_BYTES, Recorder};

/// The scan of the folder as it is now; `None` when it has more files
/// than a checkpoint takes (its changes cannot be told apart).
#[must_use]
pub fn scan(session: &LocalSession) -> Option<FolderScan> {
    let found = folder_scan::scan(
        session.workspace().root(),
        session.checkpoints().scan_rules(),
        MAX_COPY_FILES,
    );
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

/// The file `path` as it was before the turn, given its stat in the scan
/// before the command (`None`: it did not exist then).
fn before_image(session: &LocalSession, path: &str, stat: Option<folder_scan::FileStat>) -> Image {
    let Some(stat) = stat else {
        return Image::Absent;
    };
    let opaque = Image::Opaque {
        len: stat.len,
        mtime_ns: stat.mtime_ns,
        digest: None,
    };
    let (Some(seq), Ok(ws_path)) = (
        session.turn_checkpoint(),
        WsPath::from_relative(std::path::Path::new(path)),
    ) else {
        return opaque;
    };
    match session
        .checkpoints()
        .file_at(seq, &ws_path, MAX_IMAGE_BYTES)
    {
        Ok(Some(bytes)) => Image::Bytes(bytes),
        // Left out of the checkpoint (too large, a placeholder), too large
        // to keep, or unreadable: its size only.
        Ok(None) => opaque,
        Err(error) if error.code() == ErrorCode::TooLarge => opaque,
        Err(error) => {
            log::debug!("no before-image of `{path}`: {}", error.message());
            opaque
        }
    }
}

/// Record what changed between `before` and `after` (both scans of the
/// folder) as `command`'s.
pub fn record(
    session: &LocalSession,
    recorder: &Recorder,
    before: &FolderScan,
    after: &FolderScan,
    command: &str,
) {
    let changed = folder_scan::diff(before, after);
    for path in changed.paths() {
        if recorder.knows(&path) {
            continue;
        }
        let image = before_image(session, &path, before.files.get(&path).copied());
        // A placeholder that was there: its size is its stat's too.
        let image = if image == Image::Absent && before.placeholders.contains(&path) {
            Image::Opaque {
                len: 0,
                mtime_ns: 0,
                digest: None,
            }
        } else {
            image
        };
        recorder.command_changed(&path, image, command);
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

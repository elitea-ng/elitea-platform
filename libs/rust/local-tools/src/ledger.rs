//! Read-before-write: an agent may only overwrite or edit a file whose
//! current content it has seen.
//!
//! Each session records the stamp (length, mtime, SHA-256) of every file it
//! reads, and of every file it writes itself. A write or an edit of an
//! existing file is refused when the file was never read in this session, or
//! when its content no longer hashes to the recorded stamp: someone (the
//! person, a build, another agent) changed it since, and the agent's edit
//! would be based on text that is gone. The hash decides; the mtime is only
//! reported, since editors and `touch` change it without changing content.

use std::collections::HashMap;
use std::sync::Mutex;

use crate::error::{ErrorCode, ToolError, ToolResult};
use crate::workspace::{FileStamp, WsPath};

/// The per-session record of what the agent has seen.
#[derive(Debug, Default)]
pub struct ReadLedger {
    seen: Mutex<HashMap<WsPath, FileStamp>>,
}

impl ReadLedger {
    #[must_use]
    pub fn new() -> Self {
        Self::default()
    }

    /// Record that the agent has seen `path` as `stamp` (a read, or its own
    /// write).
    pub fn record(&self, path: &WsPath, stamp: FileStamp) {
        if let Ok(mut seen) = self.seen.lock() {
            seen.insert(path.clone(), stamp);
        }
    }

    /// Forget one path (after the agent deletes it).
    pub fn forget(&self, path: &WsPath) {
        if let Ok(mut seen) = self.seen.lock() {
            seen.remove(path);
        }
    }

    /// Forget everything: after a checkpoint restore every stamp is stale.
    pub fn clear(&self) {
        if let Ok(mut seen) = self.seen.lock() {
            seen.clear();
        }
    }

    /// Whether the agent may replace `path`, whose current state is
    /// `current` (`None`: it does not exist).
    ///
    /// # Errors
    ///
    /// [`ErrorCode::StaleRead`] when an existing file was never read, or
    /// changed since; also when a file the agent read has since been deleted.
    pub fn check_fresh(&self, path: &WsPath, current: Option<&FileStamp>) -> ToolResult<()> {
        let seen = self
            .seen
            .lock()
            .map_err(|_| ToolError::new(ErrorCode::Io, "the read ledger is unavailable"))?;
        match (seen.get(path), current) {
            (None, None) => Ok(()),
            (Some(_), None) => Err(ToolError::new(
                ErrorCode::StaleRead,
                format!("`{path}` was deleted since you read it; read the folder again"),
            )),
            (None, Some(_)) => Err(ToolError::new(
                ErrorCode::StaleRead,
                format!("`{path}` exists and you have not read it; read it before changing it"),
            )),
            (Some(recorded), Some(now)) if recorded.sha256 == now.sha256 => Ok(()),
            (Some(recorded), Some(now)) => Err(ToolError::new(
                ErrorCode::StaleRead,
                format!(
                    "`{path}` changed since you read it ({} → {} bytes{}); read it again",
                    recorded.len,
                    now.len,
                    if recorded.mtime_ns == now.mtime_ns {
                        ", same mtime"
                    } else {
                        ""
                    }
                ),
            )),
        }
    }
}

#[cfg(test)]
mod tests {
    use std::path::Path;

    use super::ReadLedger;
    use crate::error::ErrorCode;
    use crate::workspace::{FileStamp, WsPath};

    fn stamp(byte: u8, mtime: i128) -> FileStamp {
        FileStamp {
            len: 1,
            mtime_ns: mtime,
            sha256: [byte; 32],
        }
    }

    #[test]
    fn the_matrix() {
        let ledger = ReadLedger::new();
        let path = WsPath::from_relative(Path::new("a.txt")).expect("path");
        assert!(
            ledger.check_fresh(&path, None).is_ok(),
            "a new file needs no read"
        );
        let unread = ledger
            .check_fresh(&path, Some(&stamp(1, 1)))
            .expect_err("unread");
        assert_eq!(unread.code(), ErrorCode::StaleRead);
        ledger.record(&path, stamp(1, 1));
        assert!(ledger.check_fresh(&path, Some(&stamp(1, 1))).is_ok());
        assert!(
            ledger.check_fresh(&path, Some(&stamp(1, 9))).is_ok(),
            "a touch is not a change"
        );
        let changed = ledger
            .check_fresh(&path, Some(&stamp(2, 1)))
            .expect_err("changed");
        assert!(changed.message().contains("same mtime"));
        let deleted = ledger.check_fresh(&path, None).expect_err("deleted");
        assert_eq!(deleted.code(), ErrorCode::StaleRead);
        ledger.clear();
        assert_eq!(
            ledger
                .check_fresh(&path, Some(&stamp(1, 1)))
                .expect_err("cleared")
                .code(),
            ErrorCode::StaleRead
        );
    }
}

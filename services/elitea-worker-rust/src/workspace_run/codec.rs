//! The bundle and commit operations the cloud executor needs, as a narrow
//! trait.
//!
//! `libs/rust/workspace-store` (the desktop unit's crate, ADR-0034 decision
//! 2) writes and verifies COMPACT git bundle v2 segments with gix. Until it
//! is on main the worker codes against [`WorkspaceCodec`]; the binding is one
//! adapter over `bundle::verify_into`, `ObjectDb::flatten` and
//! `bundle::write_compact`, plus loose-object writes for the turn's commit.
//! No codec is composed in production until then, so a cloud turn fails with
//! `workspace_run.unavailable` instead of running without its files.
//!
//! Every method runs on a blocking thread: they do file and object work.

use std::collections::BTreeMap;
use std::path::Path;

/// What the plan says the segment is; the codec verifies all of it before
/// any file is written.
#[derive(Clone, Debug, Eq, PartialEq)]
pub(crate) struct SegmentExpectation {
    pub(crate) sha256: String,
    pub(crate) bytes: u64,
    /// The refs the segment must carry, exactly.
    pub(crate) refs: BTreeMap<String, String>,
    pub(crate) max_unpacked_bytes: u64,
    pub(crate) max_files: u64,
    pub(crate) max_file_bytes: u64,
}

/// A checked-out tree.
#[derive(Clone, Debug, Eq, PartialEq)]
pub(crate) struct HydratedTree {
    pub(crate) files: u64,
    pub(crate) unpacked_bytes: u64,
}

/// The author and trailers of the turn's commit (decision 9).
#[derive(Clone, Debug, Eq, PartialEq)]
pub(crate) struct TurnCommit {
    pub(crate) parent: String,
    pub(crate) message: String,
    pub(crate) author_name: String,
    pub(crate) author_email: String,
    pub(crate) unix_seconds: i64,
}

/// A COMPACT segment written for the publish.
#[derive(Clone, Debug, Eq, PartialEq)]
pub(crate) struct BuiltSegment {
    /// The turn's new commit: the new `refs/elitea/cloud`.
    pub(crate) commit: String,
    pub(crate) bytes: Vec<u8>,
    pub(crate) sha256: String,
}

/// Why a codec operation failed. Data-free.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum CodecError {
    /// The segment is not what the plan says (digest, size, pack, refs,
    /// connectivity): nothing was written.
    Integrity,
    /// The repository uses an object format Phase 1 does not move.
    UnsupportedObjectFormat,
    /// A cap was exceeded (unpacked size, files, file size).
    TooLarge,
    /// The working tree holds something a commit cannot carry (a path that
    /// escapes, a special file).
    InvalidTree,
    Io,
}

impl CodecError {
    #[must_use]
    pub(crate) const fn code(self) -> &'static str {
        match self {
            Self::Integrity => "workspace_run.segment_integrity",
            Self::UnsupportedObjectFormat => "workspace_run.unsupported_object_format",
            Self::TooLarge => "workspace_run.workspace_too_large",
            Self::InvalidTree => "workspace_run.invalid_tree",
            Self::Io => "workspace_run.io",
        }
    }
}

/// The bundle and commit operations of one workspace run.
pub(crate) trait WorkspaceCodec: Send + Sync {
    /// Verify `segment` against `expect` into the quarantine object database
    /// at `git_dir`, then write the tree of commit `checkout` into
    /// `worktree` (an empty directory). Nothing is written to `worktree`
    /// unless every check passed.
    ///
    /// # Errors
    ///
    /// [`CodecError::Integrity`] for any mismatch; [`CodecError::TooLarge`]
    /// over a cap.
    fn hydrate(
        &self,
        segment: &[u8],
        expect: &SegmentExpectation,
        checkout: &str,
        git_dir: &Path,
        worktree: &Path,
    ) -> Result<HydratedTree, CodecError>;

    /// Commit `worktree` as the child of `commit.parent` into `git_dir`, and
    /// write a COMPACT segment carrying `refs` with `refs/elitea/cloud`
    /// moved to the new commit. `.git` in `worktree` is never read.
    ///
    /// # Errors
    ///
    /// [`CodecError::InvalidTree`] for a tree a commit cannot carry;
    /// [`CodecError::TooLarge`] over `max_segment_bytes`.
    fn commit_and_bundle(
        &self,
        git_dir: &Path,
        worktree: &Path,
        commit: &TurnCommit,
        refs: &BTreeMap<String, String>,
        max_segment_bytes: u64,
    ) -> Result<BuiltSegment, CodecError>;
}

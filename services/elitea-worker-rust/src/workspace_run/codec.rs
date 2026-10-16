//! The bundle and commit operations the cloud executor needs, as a narrow
//! trait.
//!
//! `libs/rust/workspace-store` (the desktop unit's crate, ADR-0034 decision
//! 2) writes and verifies COMPACT git bundle v2 segments with gix. Until it
//! is on main the worker codes against [`WorkspaceCodec`]; the binding is one
//! adapter over `bundle::verify_into`, `ObjectDb::flatten` and
//! `bundle::write_compact`, plus loose-object writes for the turn's commit.
//!
//! Segments move as FILES on the worker's disk (the turn's scratch
//! directory), never as one buffer: a 200 MB segment is streamed to disk as
//! it is fetched, verified and indexed from there in bounded reads, and the
//! turn's segment is written to disk and uploaded from there in chunks.
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
    /// The paths the person explicitly included although an exclusion
    /// matched them (contract 6.2), from the handoff snapshot commit's
    /// `Elitea-Included` trailers. The hydrate re-scan skips them.
    pub(crate) included_excluded: Vec<String>,
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

/// A COMPACT segment written for the publish, on disk.
#[derive(Clone, Debug, Eq, PartialEq)]
pub(crate) struct BuiltSegment {
    /// The turn's new commit: the new `refs/elitea/cloud`.
    pub(crate) commit: String,
    /// The segment file (the `out` path the codec was given).
    pub(crate) path: std::path::PathBuf,
    pub(crate) bytes: u64,
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
    /// Verify the segment FILE `segment` against `expect` into the
    /// quarantine object database at `git_dir`, then write the tree of
    /// commit `checkout` into `worktree` (an empty directory). Nothing is
    /// written to `worktree` unless every check passed. The file is read in
    /// bounded chunks, never whole.
    ///
    /// # Errors
    ///
    /// [`CodecError::Integrity`] for any mismatch; [`CodecError::TooLarge`]
    /// over a cap.
    fn hydrate(
        &self,
        segment: &Path,
        expect: &SegmentExpectation,
        checkout: &str,
        git_dir: &Path,
        worktree: &Path,
    ) -> Result<HydratedTree, CodecError>;

    /// Commit `worktree` as the child of `commit.parent` into `git_dir`, and
    /// write a COMPACT segment carrying `refs` with `refs/elitea/cloud`
    /// moved to the new commit, streamed into the file `out`. `.git` in
    /// `worktree` is never read.
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
        out: &Path,
    ) -> Result<BuiltSegment, CodecError>;

    /// The files of commit `parent` in `git_dir`, for the end-of-turn scan
    /// of changed files (contract 6.4).
    ///
    /// # Errors
    ///
    /// The commit is not in the quarantine.
    fn parent_files(
        &self,
        git_dir: &Path,
        parent: &str,
    ) -> Result<Box<dyn super::file_redact::ParentTree + Send>, CodecError>;

    /// Regenerate of the newest cloud turn (ADR-0034 decision 7): in the
    /// verified quarantine at `git_dir`, take `cloud`'s first parent P,
    /// write P's tree into `worktree` (an empty directory) and a COMPACT
    /// segment carrying `expect.refs` with `refs/elitea/cloud` moved back to
    /// P into the file `out`. Returns P and the segment; the caller publishes
    /// it through the normal compare-and-set (old `cloud`, new P) before
    /// the turn starts.
    ///
    /// # Errors
    ///
    /// `cloud` has no parent ([`CodecError::Integrity`]), or as
    /// `hydrate`/`commit_and_bundle`.
    fn rewind(
        &self,
        git_dir: &Path,
        expect: &SegmentExpectation,
        cloud: &str,
        worktree: &Path,
        max_segment_bytes: u64,
        out: &Path,
    ) -> Result<(String, BuiltSegment), CodecError>;
}

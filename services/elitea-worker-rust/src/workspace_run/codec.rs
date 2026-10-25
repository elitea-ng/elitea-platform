//! The bundle and commit operations the cloud executor needs, as a narrow
//! trait.
//!
//! `libs/rust/workspace-store` (ADR-0034 decision 2) writes and verifies
//! git bundle v2 segments with gix: `COMPACT` (the whole closure) and, in
//! Phase 2, `PUSH` (an increment over named prerequisite commits). The
//! worker codes against [`WorkspaceCodec`]; the binding (`gix_codec.rs`)
//! verifies a chain of segment files and writes the turn's segment, plus
//! loose-object writes for the turn's commit.
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
    /// `COMPACT` or `PUSH`.
    pub(crate) kind: String,
    pub(crate) sha256: String,
    pub(crate) bytes: u64,
    /// A `PUSH`'s prerequisite commits (what the chain before it must
    /// hold); empty for a `COMPACT`.
    pub(crate) prerequisites: Vec<String>,
    /// The thread's refs after the segment: a `COMPACT` carries exactly
    /// these, a `PUSH` the ones it moved (each must be one of these).
    pub(crate) refs: BTreeMap<String, String>,
    pub(crate) max_unpacked_bytes: u64,
    pub(crate) max_files: u64,
    pub(crate) max_file_bytes: u64,
}

/// One segment file of a chain and what the plan says it is.
#[derive(Clone, Debug, Eq, PartialEq)]
pub(crate) struct SegmentFile {
    pub(crate) path: std::path::PathBuf,
    pub(crate) expect: SegmentExpectation,
}

/// How large the turn's segment may be.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) struct SegmentBudget {
    /// The policy's cap on any segment.
    pub(crate) max_segment_bytes: u64,
    /// `Some(bytes)`: write a `PUSH` over the published head when it fits
    /// in `bytes`, else a `COMPACT`; `None`: a `COMPACT` (the compaction
    /// policy says the chain is long or large enough).
    pub(crate) push_bytes: Option<u64>,
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

/// A segment written for the publish, on disk.
#[derive(Clone, Debug, Eq, PartialEq)]
pub(crate) struct BuiltSegment {
    /// `COMPACT` or `PUSH`.
    pub(crate) kind: &'static str,
    /// A `PUSH`'s prerequisites; empty for a `COMPACT`.
    pub(crate) prerequisites: Vec<String>,
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
    /// Verify the chain of segment FILES `chain` (a `COMPACT`, then `PUSH`
    /// segments; each against its expectation, every `PUSH`'s
    /// prerequisites proved present before its pack is indexed) into the
    /// quarantine object database at `git_dir`, then write the tree of
    /// commit `checkout` into `worktree` (an empty directory). Nothing is
    /// written to `worktree` unless every check passed. The files are read
    /// in bounded chunks, never whole; the caps are the last segment's.
    ///
    /// # Errors
    ///
    /// [`CodecError::Integrity`] for any mismatch; [`CodecError::TooLarge`]
    /// over a cap.
    fn hydrate(
        &self,
        chain: &[SegmentFile],
        checkout: &str,
        git_dir: &Path,
        worktree: &Path,
    ) -> Result<HydratedTree, CodecError>;

    /// Commit `worktree` as the child of `commit.parent` into `git_dir`, and
    /// write the segment that moves `refs/elitea/cloud` to the new commit,
    /// streamed into the file `out`: a `PUSH` whose prerequisites are the
    /// published commits of `refs` when `budget` allows one and it fits,
    /// else a `COMPACT` carrying `refs` with the cloud ref moved. The
    /// commit is the same either way (same inputs, same object). `.git` in
    /// `worktree` is never read.
    ///
    /// # Errors
    ///
    /// [`CodecError::InvalidTree`] for a tree a commit cannot carry;
    /// [`CodecError::TooLarge`] over `budget.max_segment_bytes`.
    fn commit_and_bundle(
        &self,
        git_dir: &Path,
        worktree: &Path,
        commit: &TurnCommit,
        refs: &BTreeMap<String, String>,
        budget: SegmentBudget,
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
    /// verified quarantine at `git_dir`, take the commit P to rewind to —
    /// `to` when Main names it (the commit the regenerated turn started
    /// from; a turn that paused for input published more than one commit),
    /// else `cloud`'s first parent — write P's tree into `worktree` (an
    /// empty directory) and a COMPACT segment carrying `expect.refs` with
    /// `refs/elitea/cloud` moved back to P into the file `out`. Returns P and
    /// the segment; the caller publishes it through the normal
    /// compare-and-set (old `cloud`, new P) before the turn starts.
    ///
    /// # Errors
    ///
    /// `cloud` has no parent, or `to` is not on `cloud`'s first-parent chain
    /// ([`CodecError::Integrity`]), or as `hydrate`/`commit_and_bundle`.
    #[allow(clippy::too_many_arguments, reason = "one rewind, all of its inputs")]
    fn rewind(
        &self,
        git_dir: &Path,
        expect: &SegmentExpectation,
        cloud: &str,
        to: Option<&str>,
        worktree: &Path,
        max_segment_bytes: u64,
        out: &Path,
    ) -> Result<(String, BuiltSegment), CodecError>;
}

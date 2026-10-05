//! The clone itself: gitoxide in process, no git binary (ADR-0026
//! decision 7). The runtime image is distroless; there is no `git` to run.
//!
//! A port of `LocalRepositoryManager.get_repository_local_path` for git
//! sources:
//!
//! 1. resolve the branch head on the remote (`git ls-remote`,
//!    [`ls_remote`]) — a missing branch stops here, before any download;
//! 2. a shallow (depth 1), single-branch clone into
//!    `{scratch}/{owner_repo}_{branch}_{sha8}`;
//! 3. the identity `{repo}:{branch}:{sha8}` of the commit actually checked
//!    out.
//!
//! # Credentials
//!
//! The URL is credential-free. The `Authorization` value is handed to the
//! HTTP transport as an extra request header, in memory, for this one
//! connection; it is never written to `.git/config`, never part of a URL,
//! and never part of an error message (the transport's errors carry the
//! URL, which has no userinfo). The repository is opened ISOLATED: no
//! system, global or environment git configuration, so no credential
//! helper, `url.*.insteadOf` rewrite or `http.extraHeader` of the host
//! machine applies, and the credential callback answers "no credentials"
//! instead of prompting. No redirect is followed, with or without a
//! credential: the host connected to is always the one the allowlist
//! admitted (gitoxide's default follows the first redirect of an anonymous
//! clone, to any host). A repository that moved must be given by its new
//! URL. A credential is refused on plain `http` unless the host is a
//! loopback address (the tests' local server).
//!
//! # Memory while fetching
//!
//! Resolving the pack decodes each object in memory. An object can claim
//! any size and compress a thousandfold (512 MiB of zeros is half a
//! megabyte on the wire), so the repository is opened with
//! `gitoxide.objects.allocLimit` at `MAX_FILE_BYTES` (at least
//! [`MIN_ALLOC_LIMIT`]): a larger object, or a pack whose delta tree needs
//! more, fails the fetch before the allocation. The resolution runs on at
//! most [`PACK_THREADS`] threads, each holding a few buffers up to that
//! limit, so the fetch's peak memory is bounded by about
//! `3 × PACK_THREADS × MAX_FILE_BYTES` plus the delta tree. That bound is
//! large; the deployment control is the job's memory limit (the pod or
//! container limit), which this does not replace.
//!
//! # What the checkout can and cannot write
//!
//! gitoxide validates every tree path when it builds the index (no `..`,
//! no `.git`, no absolute path, plus the NTFS/HFS protections), and it
//! refuses to create a file below a symbolic link (a leading component
//! must be a real directory). After the checkout, [`verify_containment`]
//! checks that again from the outside: every index path is plain and
//! relative, every leading component is a directory (not a link) whose
//! canonical path is inside the clone. Symbolic links ARE checked out as
//! links (so discovery can skip and count them, as the walker does);
//! nothing in the engine follows them.
//!
//! # Not supported, deliberately
//!
//! * Submodules: not fetched (gitoxide has no submodule checkout, and the
//!   Python engine cloned without `--recurse-submodules` too).
//! * Git LFS: pointer files stay pointer files (no LFS client, and the
//!   Python image had none configured either).
//! * SSH and `git://` remotes: every derived URL is `https`.
//!
//! # Differences from the Python engine
//!
//! * A missing branch is detected by `ls-remote` and reported before
//!   cloning. Python's `ls-remote` failure was silent (`latest` in the
//!   directory name) and the clone's stderr was pattern-matched.
//! * The `ls-remote` accepts a tag of that name too, as `git clone
//!   --branch` does (Python's `ls-remote` looked only at `refs/heads/`).
//! * The directory is always fresh: the scratch directory is per job, so a
//!   leftover directory of that name is removed, not reused.
//! * Python had no clone timeout and no size limits.
//! * Python's `git clone` followed HTTP redirects; this clone follows none.

use super::egress::AdmittedTarget;
use super::identity::RepoIdentity;
use super::limits::{IngestLimits, TreeBudget, TreeStats};
use super::providers::CloneTarget;
use crate::errors::{EngineError, ErrorType};
use crate::source::py_repr;
use gix::bstr::ByteSlice;
use gix::protocol::transport::client::blocking_io::http;
use std::num::NonZeroU32;
use std::path::{Component, Path, PathBuf};
use std::sync::Arc;
use std::sync::atomic::{AtomicBool, AtomicU8, AtomicU64, Ordering};
use std::time::{Duration, Instant};

/// A finished checkout.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ClonedRepository {
    /// The working tree.
    pub path: PathBuf,
    pub identity: RepoIdentity,
    pub tree: TreeStats,
}

/// The branch head on the remote.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct RemoteHead {
    /// The full reference name that matched (`refs/heads/…` or
    /// `refs/tags/…`).
    pub reference: String,
    /// The commit (an annotated tag is peeled).
    pub commit: String,
}

/// Why a clone was interrupted.
const STOP_NONE: u8 = 0;
const STOP_CANCELLED: u8 = 1;
const STOP_DEADLINE: u8 = 2;
const STOP_BYTES: u8 = 3;

/// How often the watchdog looks at the clone.
const WATCH_INTERVAL: Duration = Duration::from_millis(100);

/// The fewest bytes one allocation of the pack resolution may take, however
/// small `MAX_FILE_BYTES` is: the delta tree of a large pack needs some.
pub const MIN_ALLOC_LIMIT: u64 = 16 << 20;

/// The most threads that resolve the pack.
pub const PACK_THREADS: usize = 4;

/// How many tree entries are admitted between two looks at the deadline
/// and the cancel flag.
const ADMIT_CHECK_EVERY: u64 = 1024;

fn runtime_error(message: String) -> EngineError {
    EngineError::new(ErrorType::Runtime, message)
}

/// The timeout error. The text carries `timeout`, which is what the
/// legacy classifier keys `timeout_error` on.
#[must_use]
pub fn timeout_error(repo: &str, branch: &str, limit: Duration) -> EngineError {
    runtime_error(format!(
        "Clone timeout: {} (branch {}) did not finish within {} seconds (ELITEA_DEEPWIKI_CLONE_TIMEOUT_SECONDS)",
        py_repr(repo),
        py_repr(branch),
        limit.as_secs_f64()
    ))
}

/// The error for a branch the remote does not have. "not found" makes it
/// `resource_not_found`, as Python's `RuntimeError` text did.
fn branch_missing(target: &CloneTarget) -> EngineError {
    runtime_error(format!(
        "Branch {} not found in repository {}. Please verify the branch name exists.",
        py_repr(target.branch()),
        py_repr(target.repo_identifier())
    ))
}

/// Map a gitoxide failure onto the messages `_clone_repository` raised,
/// in its order: authentication, then a missing repository, then the
/// generic failure with the cause.
fn classify_failure(target: &CloneTarget, error: &dyn std::error::Error) -> EngineError {
    let detail = error_chain(error);
    let repo = py_repr(target.repo_identifier());
    let lowered = detail.to_lowercase();
    if lowered.contains("http status 401")
        || lowered.contains("http status 403")
        || lowered.contains("authentication")
        || lowered.contains("no credentials were returned")
        || lowered.contains("permission denied")
    {
        return runtime_error(format!(
            "Authentication failed for repository {repo}. Please check your access token and permissions."
        ));
    }
    if lowered.contains("http status 404") {
        return runtime_error(format!(
            "Repository not found: {repo}. Please verify the repository name and your access permissions."
        ));
    }
    runtime_error(format!("Git clone failed for {repo}: {detail}"))
}

/// Every message in an error's source chain, joined.
fn error_chain(error: &dyn std::error::Error) -> String {
    let mut parts = vec![error.to_string()];
    let mut source = error.source();
    while let Some(cause) = source {
        let text = cause.to_string();
        if !parts.iter().any(|seen| seen.contains(&text)) {
            parts.push(text);
        }
        source = cause.source();
    }
    parts.join(": ")
}

fn is_loopback(host: &str) -> bool {
    let name = host
        .strip_prefix('[')
        .and_then(|rest| rest.split(']').next())
        .unwrap_or_else(|| host.split(':').next().unwrap_or_default());
    name == "localhost"
        || name
            .parse::<std::net::IpAddr>()
            .is_ok_and(|ip| ip.is_loopback())
}

/// The HTTP options for one connection: the credential as a header, and
/// nothing read from any git configuration.
fn transport_options(target: &CloneTarget) -> Result<http::Options, EngineError> {
    let mut options = http::Options::default();
    if let Some(authorization) = target.authorization() {
        if target.url().starts_with("http://") && !is_loopback(target.host()) {
            return Err(EngineError::new(
                ErrorType::Value,
                format!(
                    "Refusing to send a credential for {} over plain http",
                    py_repr(target.repo_identifier())
                ),
            ));
        }
        options
            .extra_headers
            .push(format!("Authorization: {}", authorization.expose()));
    }
    options.user_agent =
        Some(concat!("elitea-deepwiki-engine/", env!("CARGO_PKG_VERSION")).to_owned());
    // Never to another host than the admitted one (see the module docs).
    options.follow_redirects = http::options::FollowRedirects::None;
    Ok(options)
}

/// Answer every credential request with "none": never a prompt, never a
/// helper program. The credential, if any, is already in the header.
#[allow(clippy::unnecessary_wraps)]
fn no_credentials(_action: gix::credentials::helper::Action) -> gix::credentials::protocol::Result {
    Ok(None)
}

/// `git ls-remote <url> refs/heads/<branch>` (falling back to the tag of
/// that name, as `git clone --branch` does). `None` when the remote has
/// neither.
///
/// It needs a repository to hang the remote on: a bare one is created in
/// `scratch` and removed afterwards.
///
/// # Errors
///
/// The classified failure (authentication, missing repository, other).
pub fn ls_remote(
    admitted: &AdmittedTarget,
    scratch: &Path,
) -> Result<Option<RemoteHead>, EngineError> {
    let target = admitted.target();
    let options = transport_options(target)?;
    std::fs::create_dir_all(scratch)
        .map_err(|e| runtime_error(format!("Cannot create {}: {e}", scratch.display())))?;
    let probe = scratch.join(".ls-remote");
    if probe.exists() {
        std::fs::remove_dir_all(&probe)
            .map_err(|e| runtime_error(format!("Cannot clear {}: {e}", probe.display())))?;
    }
    let result = (|| -> Result<Option<RemoteHead>, EngineError> {
        let repo = gix::ThreadSafeRepository::init_opts(
            &probe,
            gix::create::Kind::Bare,
            gix::create::Options::default(),
            gix::open::Options::isolated(),
        )
        .map_err(|e| runtime_error(format!("Cannot prepare the clone: {e}")))?
        .to_thread_local();
        let heads = format!("refs/heads/{}", target.branch());
        let tags = format!("refs/tags/{}", target.branch());
        let mut specs = Vec::new();
        for spec in [&heads, &tags] {
            let parsed =
                gix::refspec::parse(spec.as_str().into(), gix::refspec::parse::Operation::Fetch)
                    .map_err(|_| branch_missing(target))?;
            specs.push(parsed.to_owned());
        }
        let remote = repo
            .remote_at(target.url())
            .map_err(|e| classify_failure(target, &e))?
            .with_fetch_tags(gix::remote::fetch::Tags::None);
        let mut connection = remote
            .connect(gix::remote::Direction::Fetch)
            .map_err(|e| classify_failure(target, &e))?;
        connection.set_credentials(no_credentials);
        connection.set_transport_options(Box::new(options));
        let (ref_map, _) = connection
            .ref_map(
                gix::progress::Discard,
                gix::remote::ref_map::Options {
                    prefix_from_spec_as_filter_on_remote: true,
                    handshake_parameters: Vec::new(),
                    extra_refspecs: specs,
                },
            )
            .map_err(|e| classify_failure(target, &e))?;
        let find = |wanted: &str| {
            ref_map.remote_refs.iter().find_map(|reference| {
                let (name, target_id, peeled) = reference.unpack();
                (name == wanted.as_bytes().as_bstr())
                    .then(|| peeled.or(target_id).map(|id| id.to_hex().to_string()))
                    .flatten()
            })
        };
        Ok(find(&heads)
            .map(|commit| RemoteHead {
                reference: heads.clone(),
                commit,
            })
            .or_else(|| {
                find(&tags).map(|commit| RemoteHead {
                    reference: tags.clone(),
                    commit,
                })
            }))
    })();
    let _ = std::fs::remove_dir_all(&probe);
    result
}

/// Total bytes of the regular files under `dir`, links not followed.
fn dir_bytes(dir: &Path) -> u64 {
    let mut total = 0u64;
    let mut pending = vec![dir.to_path_buf()];
    while let Some(current) = pending.pop() {
        let Ok(entries) = std::fs::read_dir(&current) else {
            continue;
        };
        for entry in entries.flatten() {
            let Ok(meta) = entry.path().symlink_metadata() else {
                continue;
            };
            if meta.is_dir() {
                pending.push(entry.path());
            } else {
                total = total.saturating_add(meta.len());
            }
        }
    }
    total
}

/// gitoxide's progress id for "bytes of pack data read from the remote"
/// (`gix_pack::bundle::write::ProgressId::ReadPackBytes`).
const READ_PACK_BYTES: gix::progress::Id = *b"BWRB";

/// A progress sink that only counts the pack bytes read off the wire.
///
/// The size watchdog cannot rely on the pack file's size alone: gitoxide
/// buffers a whole object before writing it, so one large blob shows on
/// disk only once it has fully arrived. The byte counter moves with every
/// read, and the fetch checks the interrupt flag on every read too.
#[derive(Clone)]
struct PackBytes {
    id: gix::progress::Id,
    received: Arc<AtomicU64>,
    step: gix::progress::StepShared,
}

impl PackBytes {
    fn new(received: Arc<AtomicU64>) -> Self {
        Self {
            id: gix::progress::UNKNOWN,
            received,
            step: Arc::default(),
        }
    }
}

impl gix::progress::Count for PackBytes {
    fn set(&self, step: gix::progress::Step) {
        self.step.store(step, Ordering::Relaxed);
    }

    fn step(&self) -> gix::progress::Step {
        self.step.load(Ordering::Relaxed)
    }

    fn inc_by(&self, step: gix::progress::Step) {
        self.step.fetch_add(step, Ordering::Relaxed);
        if self.id == READ_PACK_BYTES {
            self.received
                .fetch_add(u64::try_from(step).unwrap_or(u64::MAX), Ordering::Relaxed);
        }
    }

    fn counter(&self) -> gix::progress::StepShared {
        Arc::clone(&self.step)
    }
}

impl gix::progress::Progress for PackBytes {
    fn init(&mut self, _max: Option<gix::progress::Step>, _unit: Option<gix::progress::Unit>) {}

    fn set_name(&mut self, _name: String) {}

    fn name(&self) -> Option<String> {
        None
    }

    fn id(&self) -> gix::progress::Id {
        self.id
    }

    fn message(&self, _level: gix::progress::MessageLevel, _message: String) {}
}

impl gix::progress::NestedProgress for PackBytes {
    type SubProgress = Self;

    fn add_child(&mut self, _name: impl Into<String>) -> Self {
        Self::new(Arc::clone(&self.received))
    }

    fn add_child_with_id(&mut self, _name: impl Into<String>, id: gix::progress::Id) -> Self {
        Self {
            id,
            ..Self::new(Arc::clone(&self.received))
        }
    }
}

/// Run `work` while a watchdog interrupts it on cancellation, at the
/// deadline, or when `measure()` passes `max_bytes`.
fn watched<T>(
    interrupt: &AtomicBool,
    cancel: &AtomicBool,
    deadline: Instant,
    measure: impl Fn() -> u64 + Sync,
    max_bytes: u64,
    work: impl FnOnce() -> T,
) -> (T, u8, u64) {
    let reason = AtomicU8::new(STOP_NONE);
    let seen = AtomicU64::new(0);
    let done = AtomicBool::new(false);
    let outcome = std::thread::scope(|scope| {
        scope.spawn(|| {
            while !done.load(Ordering::Acquire) {
                let stop = if cancel.load(Ordering::Acquire) {
                    STOP_CANCELLED
                } else if Instant::now() >= deadline {
                    STOP_DEADLINE
                } else {
                    let bytes = measure();
                    seen.store(bytes, Ordering::Release);
                    if bytes > max_bytes {
                        STOP_BYTES
                    } else {
                        STOP_NONE
                    }
                };
                if stop != STOP_NONE {
                    reason.store(stop, Ordering::Release);
                    interrupt.store(true, Ordering::Release);
                    break;
                }
                std::thread::sleep(WATCH_INTERVAL);
            }
        });
        let outcome = work();
        done.store(true, Ordering::Release);
        outcome
    });
    (
        outcome,
        reason.load(Ordering::Acquire),
        seen.load(Ordering::Acquire),
    )
}

/// The error for an interrupted stage, or `None` if nothing interrupted it.
fn stop_error(
    reason: u8,
    seen: u64,
    target: &CloneTarget,
    limits: &IngestLimits,
    stage: &str,
) -> Option<EngineError> {
    match reason {
        STOP_CANCELLED => Some(EngineError::cancelled()),
        STOP_DEADLINE => Some(timeout_error(
            target.repo_identifier(),
            target.branch(),
            limits.clone_timeout,
        )),
        STOP_BYTES => {
            let mut error = limits.clone_bytes_error(target.repo_identifier(), seen);
            error.message = format!("{} (while {stage})", error.message);
            Some(error)
        }
        _ => None,
    }
}

/// Clone `admitted` into `scratch`. Blocking: call it from a blocking
/// thread ([`super::ingest`] does).
///
/// `cancel` stops the clone at its next checkpoint; the deadline is
/// `limits.clone_timeout` from now.
///
/// # Errors
///
/// A classified clone failure, a limit, the timeout, a cancellation, or a
/// containment violation.
pub fn clone_repository(
    admitted: &AdmittedTarget,
    scratch: &Path,
    limits: &IngestLimits,
    cancel: &AtomicBool,
) -> Result<ClonedRepository, EngineError> {
    let started = Instant::now();
    let deadline = started + limits.clone_timeout;
    let target = admitted.target();
    std::fs::create_dir_all(scratch)
        .map_err(|e| runtime_error(format!("Cannot create {}: {e}", scratch.display())))?;

    let head = ls_remote(admitted, scratch)?.ok_or_else(|| branch_missing(target))?;
    if Instant::now() >= deadline {
        return Err(timeout_error(
            target.repo_identifier(),
            target.branch(),
            limits.clone_timeout,
        ));
    }
    if cancel.load(Ordering::Acquire) {
        return Err(EngineError::cancelled());
    }

    let planned = RepoIdentity::new(
        target.repo_identifier(),
        target.branch(),
        head.commit.as_str(),
    );
    let destination = scratch.join(planned.directory_name());
    if destination.symlink_metadata().is_ok() {
        std::fs::remove_dir_all(&destination)
            .map_err(|e| runtime_error(format!("Cannot clear {}: {e}", destination.display())))?;
    }

    let (repo, tree) = fetch_and_checkout(target, &destination, limits, cancel, deadline)?;

    let commit = repo
        .head_id()
        .map_err(|e| classify_failure(target, &e))?
        .to_hex()
        .to_string();
    if let Err(error) = verify_containment(&repo, &destination) {
        let _ = std::fs::remove_dir_all(&destination);
        return Err(error);
    }
    drop(repo);

    let identity = RepoIdentity::new(target.repo_identifier(), target.branch(), commit);
    let path = if identity.short_commit() == planned.short_commit() {
        destination
    } else {
        // The branch moved between ls-remote and the fetch: name the clone
        // after what it holds.
        let moved = scratch.join(identity.directory_name());
        if moved.symlink_metadata().is_ok() {
            let _ = std::fs::remove_dir_all(&moved);
        }
        std::fs::rename(&destination, &moved)
            .map_err(|e| runtime_error(format!("Cannot rename {}: {e}", destination.display())))?;
        moved
    };
    tracing::info!(
        repository = target.repo_identifier(),
        branch = target.branch(),
        commit = identity.short_commit(),
        files = tree.files,
        bytes = tree.bytes,
        elapsed_ms = u64::try_from(started.elapsed().as_millis()).unwrap_or(u64::MAX),
        "cloned"
    );
    Ok(ClonedRepository {
        path,
        identity,
        tree,
    })
}

/// The fetch and the checkout into `destination`, with the tree admitted
/// in between. gitoxide removes `destination` when either step fails.
fn fetch_and_checkout(
    target: &CloneTarget,
    destination: &Path,
    limits: &IngestLimits,
    cancel: &AtomicBool,
    deadline: Instant,
) -> Result<(gix::Repository, TreeStats), EngineError> {
    let options = transport_options(target)?;
    let interrupt = AtomicBool::new(false);
    let alloc_limit = limits.max_file_bytes.max(MIN_ALLOC_LIMIT);
    let open = gix::open::Options::isolated().config_overrides([
        format!("gitoxide.objects.allocLimit={alloc_limit}"),
        format!("pack.threads={PACK_THREADS}"),
    ]);
    let mut prepare = gix::clone::PrepareFetch::new(
        target.url(),
        destination,
        gix::create::Kind::WithWorktree,
        gix::create::Options::default(),
        open,
    )
    .map_err(|e| classify_failure(target, &e))?
    .with_shallow(gix::remote::fetch::Shallow::DepthAtRemote(NonZeroU32::MIN))
    .with_ref_name(Some(target.branch()))
    .map_err(|_| branch_missing(target))?
    .configure_remote(|remote| Ok(remote.with_fetch_tags(gix::remote::fetch::Tags::None)))
    .configure_connection(move |connection| {
        connection.set_credentials(no_credentials);
        connection.set_transport_options(Box::new(options.clone()));
        Ok(())
    });

    let git_dir = destination.join(".git");
    let received = Arc::new(AtomicU64::new(0));
    let progress = PackBytes::new(Arc::clone(&received));
    let (fetched, reason, seen) = watched(
        &interrupt,
        cancel,
        deadline,
        || dir_bytes(&git_dir).max(received.load(Ordering::Relaxed)),
        limits.max_clone_bytes,
        || prepare.fetch_then_checkout(progress, &interrupt),
    );
    if let Some(error) = stop_error(reason, seen, target, limits, "fetching") {
        return Err(error);
    }
    let (mut checkout, _outcome) = fetched.map_err(|e| {
        // gitoxide's error tree (`{:#}`) names the allocation limit; its
        // `source()` chain stops above it.
        if format!("{e:#}").contains("too large to fit in memory") {
            object_too_large(target, alloc_limit)
        } else {
            classify_failure(target, &e)
        }
    })?;

    // The tree is admitted BEFORE anything is written to the working tree.
    let tree = admit_tree(
        checkout.repo(),
        target,
        limits,
        dir_bytes(&git_dir),
        cancel,
        deadline,
    )?;

    let (checked_out, reason, seen) = watched(
        &interrupt,
        cancel,
        deadline,
        || dir_bytes(destination),
        limits.max_clone_bytes,
        || checkout.main_worktree(gix::progress::Discard, &interrupt),
    );
    if let Some(error) = stop_error(reason, seen, target, limits, "checking out") {
        return Err(error);
    }
    let (repo, outcome) = checked_out.map_err(|e| classify_failure(target, &e))?;
    if !outcome.collisions.is_empty() || !outcome.errors.is_empty() {
        let _ = std::fs::remove_dir_all(destination);
        return Err(runtime_error(format!(
            "Git clone failed for {}: the checkout reported {} path collision(s) and {} error(s)",
            py_repr(target.repo_identifier()),
            outcome.collisions.len(),
            outcome.errors.len()
        )));
    }

    Ok((repo, tree))
}

/// The error for a pack the resolution refused to decode: an object (or
/// the pack's delta tree) larger than the allocation limit.
fn object_too_large(target: &CloneTarget, alloc_limit: u64) -> EngineError {
    EngineError::new(
        ErrorType::Value,
        format!(
            "The repository {} holds an object larger than this deployment accepts: over {alloc_limit} bytes once decompressed (ELITEA_DEEPWIKI_MAX_FILE_BYTES) (while fetching)",
            py_repr(target.repo_identifier())
        ),
    )
}

/// Admit the fetched commit's tree against the limits, entry by entry, as
/// the traversal visits it: the walk stops at the first limit passed, at
/// the deadline and on cancellation. A tree may list the same subtree any
/// number of times (a few objects can describe billions of files), so
/// nothing is collected first, and directories count against
/// `MAX_FILE_COUNT` too.
fn admit_tree(
    repo: &gix::Repository,
    target: &CloneTarget,
    limits: &IngestLimits,
    fetched_bytes: u64,
    cancel: &AtomicBool,
    deadline: Instant,
) -> Result<TreeStats, EngineError> {
    let fail = |e: &dyn std::error::Error| classify_failure(target, e);
    let tree = repo
        .head_commit()
        .map_err(|e| fail(&e))?
        .tree()
        .map_err(|e| fail(&e))?;
    let mut admission = Admission {
        repo,
        target,
        limits,
        budget: TreeBudget::new(limits, target.repo_identifier(), fetched_bytes),
        cancel,
        deadline,
        visited: 0,
        directories: 0,
        path: gix::bstr::BString::default(),
        queued_paths: std::collections::VecDeque::new(),
        stopped: None,
    };
    let walked = tree.traverse().breadthfirst(&mut admission);
    if let Some(error) = admission.stopped {
        return Err(error);
    }
    walked.map_err(|e| fail(&e))?;
    Ok(admission.budget.finish())
}

/// The tree walk's delegate: each entry goes to the budget as it is seen.
struct Admission<'a> {
    repo: &'a gix::Repository,
    target: &'a CloneTarget,
    limits: &'a IngestLimits,
    budget: TreeBudget<'a>,
    cancel: &'a AtomicBool,
    deadline: Instant,
    visited: u64,
    directories: u64,
    /// The path of the entry being visited, and those of the trees queued.
    path: gix::bstr::BString,
    queued_paths: std::collections::VecDeque<gix::bstr::BString>,
    /// Why the walk stopped early.
    stopped: Option<EngineError>,
}

impl Admission<'_> {
    fn stop(&mut self, error: EngineError) -> gix::traverse::tree::visit::Action {
        self.stopped = Some(error);
        std::ops::ControlFlow::Break(())
    }

    /// The deadline and the cancel flag, every [`ADMIT_CHECK_EVERY`]
    /// entries.
    fn interrupted(&mut self) -> Option<EngineError> {
        self.visited += 1;
        if !self.visited.is_multiple_of(ADMIT_CHECK_EVERY) {
            return None;
        }
        let reason = if self.cancel.load(Ordering::Acquire) {
            STOP_CANCELLED
        } else if Instant::now() >= self.deadline {
            STOP_DEADLINE
        } else {
            STOP_NONE
        };
        stop_error(reason, 0, self.target, self.limits, "admitting the tree")
    }

    fn pop_element(&mut self) {
        if let Some(position) = self.path.rfind_byte(b'/') {
            self.path.truncate(position);
        } else {
            self.path.clear();
        }
    }

    fn push_element(&mut self, name: &gix::bstr::BStr) {
        if name.is_empty() {
            return;
        }
        if !self.path.is_empty() {
            self.path.push(b'/');
        }
        self.path.extend_from_slice(name);
    }
}

impl gix::traverse::tree::Visit for Admission<'_> {
    fn pop_back_tracked_path_and_set_current(&mut self) {
        self.path = self.queued_paths.pop_back().unwrap_or_default();
    }

    fn pop_front_tracked_path_and_set_current(&mut self) {
        self.path = self.queued_paths.pop_front().unwrap_or_default();
    }

    fn push_back_tracked_path_component(&mut self, component: &gix::bstr::BStr) {
        self.push_element(component);
        self.queued_paths.push_back(self.path.clone());
    }

    fn push_path_component(&mut self, component: &gix::bstr::BStr) {
        self.push_element(component);
    }

    fn pop_path_component(&mut self) {
        self.pop_element();
    }

    fn visit_tree(
        &mut self,
        _entry: &gix::objs::tree::EntryRef<'_>,
    ) -> gix::traverse::tree::visit::Action {
        if let Some(error) = self.interrupted() {
            return self.stop(error);
        }
        self.directories += 1;
        if self.directories > self.limits.max_file_count {
            return self.stop(EngineError::new(
                ErrorType::Value,
                format!(
                    "The repository {} has more than ELITEA_DEEPWIKI_MAX_FILE_COUNT={} directories",
                    py_repr(self.target.repo_identifier()),
                    self.limits.max_file_count
                ),
            ));
        }
        std::ops::ControlFlow::Continue(true)
    }

    fn visit_nontree(
        &mut self,
        entry: &gix::objs::tree::EntryRef<'_>,
    ) -> gix::traverse::tree::visit::Action {
        if let Some(error) = self.interrupted() {
            return self.stop(error);
        }
        if entry.mode.is_commit() {
            self.budget.add_submodule();
        } else if entry.mode.is_blob_or_symlink() {
            let size = match self.repo.find_header(entry.oid) {
                Ok(header) => header.size(),
                Err(error) => return self.stop(classify_failure(self.target, &error)),
            };
            let path = self.path.to_str_lossy().into_owned();
            if let Err(error) = self.budget.add_blob(&path, size, entry.mode.is_link()) {
                return self.stop(error);
            }
        }
        std::ops::ControlFlow::Continue(true)
    }
}

/// Check, from outside gitoxide, that the checkout stayed inside `root`.
///
/// # Errors
///
/// A `RuntimeError` naming the first offending path.
pub fn verify_containment(repo: &gix::Repository, root: &Path) -> Result<(), EngineError> {
    let violation = |path: &str, why: &str| {
        runtime_error(format!(
            "The checkout of {} is unsafe: {} {why}",
            root.display(),
            py_repr(path)
        ))
    };
    let canonical_root = std::fs::canonicalize(root)
        .map_err(|e| runtime_error(format!("Cannot resolve {}: {e}", root.display())))?;
    let index = repo
        .index()
        .map_err(|e| runtime_error(format!("Cannot read the checkout index: {e}")))?;
    for entry in index.entries() {
        let raw = entry.path(&index);
        let text = raw.to_str_lossy();
        let relative = Path::new(text.as_ref());
        let plain = relative.components().all(|c| match c {
            Component::Normal(name) => !name.eq_ignore_ascii_case(".git"),
            _ => false,
        });
        if !plain || text.is_empty() {
            return Err(violation(&text, "is not a plain relative path"));
        }
        let mut current = root.to_path_buf();
        let components: Vec<_> = relative.components().collect();
        for (position, component) in components.iter().enumerate() {
            current.push(component);
            let is_last = position + 1 == components.len();
            let Ok(meta) = current.symlink_metadata() else {
                if is_last {
                    // A file the checkout skipped is not a write outside.
                    break;
                }
                return Err(violation(&text, "has a missing parent directory"));
            };
            if is_last {
                let expected_link = entry.mode == gix::index::entry::Mode::SYMLINK;
                if meta.file_type().is_symlink() != expected_link {
                    return Err(violation(&text, "was not written as the tree says"));
                }
            } else {
                if !meta.is_dir() || meta.file_type().is_symlink() {
                    return Err(violation(
                        &text,
                        "has a parent that is not a real directory",
                    ));
                }
                let canonical = std::fs::canonicalize(&current)
                    .map_err(|_| violation(&text, "has a parent that cannot be resolved"))?;
                if !canonical.starts_with(&canonical_root) {
                    return Err(violation(&text, "resolves outside the clone"));
                }
            }
        }
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::ingest::providers::ProviderType;

    fn target(url: &str, authorization: Option<&str>) -> CloneTarget {
        CloneTarget::new(
            ProviderType::GitHub,
            url.to_owned(),
            "o/r".to_owned(),
            "main".to_owned(),
            authorization.and_then(|a| crate::ingest::secret::Secret::new(a.to_owned())),
            "token",
        )
        .unwrap_or_else(|e| panic!("{e}"))
    }

    #[test]
    fn a_credential_is_refused_over_plain_http_off_loopback() {
        let remote = target("http://git.example.com/o/r.git", Some("Basic eA=="));
        assert!(transport_options(&remote).is_err());
        for local in [
            "http://127.0.0.1:9/o/r.git",
            "http://localhost:9/o/r.git",
            "http://[::1]:9/o/r.git",
        ] {
            let options = transport_options(&target(local, Some("Basic eA==")));
            assert!(
                options.is_ok_and(|o| o.extra_headers == ["Authorization: Basic eA=="]),
                "{local}"
            );
        }
        let anonymous = transport_options(&target("http://git.example.com/o/r.git", None));
        assert!(anonymous.is_ok_and(|o| o.extra_headers.is_empty()));
    }

    #[test]
    fn failures_classify_as_python_did() {
        let remote = target("https://github.com/o/r.git", None);
        let classify = |text: &str| {
            let error = classify_failure(&remote, &std::io::Error::other(text.to_owned()));
            (
                crate::errors::classify(error.error_type, &error.message),
                error.message,
            )
        };
        assert_eq!(classify("Received HTTP status 404").0, "resource_not_found");
        let (category, message) = classify("Received HTTP status 401");
        assert_eq!(category, "runtime_error");
        assert!(message.starts_with("Authentication failed"), "{message}");
        assert_eq!(classify("connection reset").0, "runtime_error");
        assert_eq!(
            crate::errors::classify(ErrorType::Runtime, &branch_missing(&remote).message),
            "resource_not_found"
        );
        assert_eq!(
            crate::errors::classify(
                ErrorType::Runtime,
                &timeout_error("o/r", "main", Duration::from_secs(5)).message
            ),
            "timeout_error"
        );
    }
}

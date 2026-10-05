//! Per-job ingest limits (ADR-0026 decision 7).
//!
//! The Python engine had none: a repository was cloned and walked whatever
//! its size. Each limit here is a setting (`src/config.rs`) with a default
//! large enough for real repositories, and exceeding one is a clear
//! `ValueError` naming the setting, before the stage that would pay for it:
//!
//! | Setting (`ELITEA_DEEPWIKI_…`) | Default | Enforced |
//! | --- | --- | --- |
//! | `MAX_CLONE_BYTES` | 2 GiB | while fetching (the pack on disk is polled and the fetch interrupted) and again before checkout (pack + every blob the checkout would write) |
//! | `MAX_FILE_COUNT` | 100 000 | before checkout, over the tree |
//! | `MAX_FILE_BYTES` | 100 MiB | before checkout, per blob (GitHub refuses larger files, so a GitHub repository never trips it) |
//! | `MAX_PARSED_BYTES` | 512 MiB | before checkout, over the blobs discovery would hand to a parser (classified and not excluded) |
//! | `CLONE_TIMEOUT_SECONDS` | 600 | over the whole ingest (ls-remote, fetch, checkout) |
//!
//! Checking the tree BEFORE checkout means an oversized repository never
//! reaches the working tree: the tree and blob headers come from the
//! fetched pack.
//!
//! The messages avoid the words the legacy classifier keys on (`download`,
//! `memory`, `not found`, `timeout`), so a limit is `invalid_input`: the
//! request named a repository this deployment will not take.

use crate::errors::{EngineError, ErrorType};
use crate::graph::discover;
use crate::source::py_repr;
use std::time::Duration;

/// The limits one ingest runs under.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct IngestLimits {
    pub max_clone_bytes: u64,
    pub max_file_count: u64,
    pub max_file_bytes: u64,
    pub max_parsed_bytes: u64,
    pub clone_timeout: Duration,
}

impl Default for IngestLimits {
    fn default() -> Self {
        Self {
            max_clone_bytes: 2 * 1024 * 1024 * 1024,
            max_file_count: 100_000,
            max_file_bytes: 100 * 1024 * 1024,
            max_parsed_bytes: 512 * 1024 * 1024,
            clone_timeout: Duration::from_mins(10),
        }
    }
}

fn limit_error(message: String) -> EngineError {
    EngineError::new(ErrorType::Value, message)
}

impl IngestLimits {
    /// The error for fetched data past `MAX_CLONE_BYTES`.
    #[must_use]
    pub fn clone_bytes_error(&self, repo: &str, seen: u64) -> EngineError {
        limit_error(format!(
            "The repository {} is larger than this deployment accepts: the clone reached {seen} bytes, over ELITEA_DEEPWIKI_MAX_CLONE_BYTES={}",
            py_repr(repo),
            self.max_clone_bytes
        ))
    }
}

/// What the tree of the checked-out commit holds.
#[derive(Debug, Clone, Copy, Default, PartialEq, Eq)]
pub struct TreeStats {
    /// Blobs (files and symbolic links) the checkout writes.
    pub files: u64,
    /// Their bytes.
    pub bytes: u64,
    /// Bytes of the blobs discovery would parse.
    pub parsed_bytes: u64,
    /// Symbolic links (written as links; never followed later).
    pub symlinks: u64,
    /// Submodule entries (never fetched: gitoxide has no submodule
    /// checkout, and the Python engine did not recurse either).
    pub submodules: u64,
}

/// Admits a commit's tree, entry by entry, against the limits.
#[derive(Debug)]
pub struct TreeBudget<'a> {
    limits: &'a IngestLimits,
    repo: &'a str,
    fetched_bytes: u64,
    stats: TreeStats,
}

impl<'a> TreeBudget<'a> {
    /// A budget for the tree of `repo`, whose fetched objects already take
    /// `fetched_bytes` on disk.
    #[must_use]
    pub fn new(limits: &'a IngestLimits, repo: &'a str, fetched_bytes: u64) -> Self {
        Self {
            limits,
            repo,
            fetched_bytes,
            stats: TreeStats::default(),
        }
    }

    /// A submodule entry.
    pub fn add_submodule(&mut self) {
        self.stats.submodules += 1;
    }

    /// A blob at repository-relative `path` of `size` bytes.
    ///
    /// # Errors
    ///
    /// A `ValueError` when this entry takes the tree past a limit.
    pub fn add_blob(&mut self, path: &str, size: u64, is_symlink: bool) -> Result<(), EngineError> {
        let limits = self.limits;
        self.stats.files += 1;
        if self.stats.files > limits.max_file_count {
            return Err(limit_error(format!(
                "The repository {} has more than ELITEA_DEEPWIKI_MAX_FILE_COUNT={} files",
                py_repr(self.repo),
                limits.max_file_count
            )));
        }
        if size > limits.max_file_bytes {
            return Err(limit_error(format!(
                "The file {} in {} is {size} bytes, over ELITEA_DEEPWIKI_MAX_FILE_BYTES={}",
                py_repr(path),
                py_repr(self.repo),
                limits.max_file_bytes
            )));
        }
        self.stats.bytes = self.stats.bytes.saturating_add(size);
        let on_disk = self.fetched_bytes.saturating_add(self.stats.bytes);
        if on_disk > limits.max_clone_bytes {
            return Err(limits.clone_bytes_error(self.repo, on_disk));
        }
        if is_symlink {
            self.stats.symlinks += 1;
        } else if is_parsed(path) {
            self.stats.parsed_bytes = self.stats.parsed_bytes.saturating_add(size);
            if self.stats.parsed_bytes > limits.max_parsed_bytes {
                return Err(limit_error(format!(
                    "The files to analyse in {} total more than ELITEA_DEEPWIKI_MAX_PARSED_BYTES={} bytes",
                    py_repr(self.repo),
                    limits.max_parsed_bytes
                )));
            }
        }
        Ok(())
    }

    /// The totals.
    #[must_use]
    pub fn finish(self) -> TreeStats {
        self.stats
    }
}

/// Whether discovery hands the file at `rel_path` to a parser: it is not
/// under an exclude pattern and it classifies as a language or a document.
#[must_use]
pub fn is_parsed(rel_path: &str) -> bool {
    !discover::is_excluded(&format!("/{rel_path}"), discover::DEFAULT_EXCLUDE_PATTERNS)
        && discover::classify(rel_path) != "unknown"
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::errors::classify;

    fn small() -> IngestLimits {
        IngestLimits {
            max_clone_bytes: 1000,
            max_file_count: 3,
            max_file_bytes: 400,
            max_parsed_bytes: 500,
            clone_timeout: Duration::from_secs(1),
        }
    }

    #[test]
    fn within_the_limits_the_tree_is_counted() {
        let limits = small();
        let mut budget = TreeBudget::new(&limits, "o/r", 100);
        assert!(budget.add_blob("src/a.py", 300, false).is_ok());
        assert!(budget.add_blob("logo.png", 10, false).is_ok());
        assert!(budget.add_blob("link.py", 5, true).is_ok());
        budget.add_submodule();
        let stats = budget.finish();
        assert_eq!(
            stats,
            TreeStats {
                files: 3,
                bytes: 315,
                parsed_bytes: 300,
                symlinks: 1,
                submodules: 1
            }
        );
    }

    #[test]
    fn each_limit_is_a_clear_invalid_input() {
        let limits = small();
        let cases: [(&[(&str, u64)], &str); 4] = [
            (
                &[("a.py", 1), ("b.py", 1), ("c.py", 1), ("d.py", 1)],
                "MAX_FILE_COUNT",
            ),
            (&[("big.bin", 401)], "MAX_FILE_BYTES"),
            (&[("a.py", 300), ("b.py", 300)], "MAX_PARSED_BYTES"),
            (
                &[("a.bin", 400), ("b.bin", 400), ("c.bin", 300)],
                "MAX_CLONE_BYTES",
            ),
        ];
        for (blobs, setting) in cases {
            let mut budget = TreeBudget::new(&limits, "o/r", 0);
            let error = blobs
                .iter()
                .find_map(|(path, size)| budget.add_blob(path, *size, false).err());
            let error = error.unwrap_or_else(|| panic!("{setting}: no error"));
            assert!(error.message.contains(setting), "{error}");
            assert_eq!(classify(error.error_type, &error.message), "invalid_input");
        }
    }

    #[test]
    fn excluded_and_unknown_files_are_not_parsed() {
        assert!(is_parsed("src/main.rs"));
        assert!(is_parsed("README.md"));
        assert!(!is_parsed("node_modules/x/index.js"));
        assert!(!is_parsed("assets/logo.png"));
    }

    #[test]
    fn the_fetched_pack_counts_towards_the_clone_size() {
        let limits = small();
        let mut budget = TreeBudget::new(&limits, "o/r", 900);
        let error = budget.add_blob("a.bin", 200, false).err();
        assert!(error.is_some_and(|e| e.message.contains("MAX_CLONE_BYTES")));
    }
}

//! What a clone is called: the repository identity and its directory.
//!
//! Python (`local_repository_manager.py`, `filesystem_indexer.py`) names
//! a clone `{repo}:{branch}:{sha8}` — the key the cache index, the graph
//! and the vector store are filed under — and puts it in the directory
//! `{owner_repo}_{branch}_{sha8}` (`repo.replace("/", "_")`). Both are
//! kept; the commit is the one actually checked out (Python's
//! `rev-parse HEAD`), not the `ls-remote` answer, so the name and the
//! content cannot disagree when a branch moves between the two.
//!
//! Deliberate difference: the directory name is made safe. Python used
//! the raw branch, so `feature/x` made a nested directory, and any other
//! character went into a path. Here every character outside
//! `A-Z a-z 0-9 . _ -` becomes `_`. The identity string keeps the raw
//! values, since it is a cache key, not a path.

use std::fmt;

/// `{repo}:{branch}:{sha8}` and the full commit behind it.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct RepoIdentity {
    repo: String,
    branch: String,
    commit: String,
}

impl RepoIdentity {
    /// An identity for `commit` (a full hex object id).
    #[must_use]
    pub fn new(
        repo: impl Into<String>,
        branch: impl Into<String>,
        commit: impl Into<String>,
    ) -> Self {
        Self {
            repo: repo.into(),
            branch: branch.into(),
            commit: commit.into(),
        }
    }

    #[must_use]
    pub fn repo(&self) -> &str {
        &self.repo
    }

    #[must_use]
    pub fn branch(&self) -> &str {
        &self.branch
    }

    /// The full commit id.
    #[must_use]
    pub fn commit(&self) -> &str {
        &self.commit
    }

    /// The first eight characters of the commit (`commit[:8]`).
    #[must_use]
    pub fn short_commit(&self) -> &str {
        self.commit.get(..8).unwrap_or(&self.commit)
    }

    /// `{owner_repo}_{branch}_{sha8}`, safe as one path component.
    #[must_use]
    pub fn directory_name(&self) -> String {
        let name = format!(
            "{}_{}_{}",
            safe_component(&self.repo),
            safe_component(&self.branch),
            self.short_commit()
        );
        // A leading dot would hide the directory; `..` cannot occur after
        // the `_` joins, but guard the first character anyway.
        if name.starts_with('.') {
            format!("_{name}")
        } else {
            name
        }
    }
}

impl fmt::Display for RepoIdentity {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(f, "{}:{}:{}", self.repo, self.branch, self.short_commit())
    }
}

fn safe_component(text: &str) -> String {
    text.chars()
        .map(|c| {
            if c.is_ascii_alphanumeric() || matches!(c, '.' | '_' | '-') {
                c
            } else {
                '_'
            }
        })
        .collect()
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_identity_is_pythons() {
        let identity = RepoIdentity::new(
            "owner/repo",
            "main",
            "0123456789abcdef0123456789abcdef01234567",
        );
        assert_eq!(identity.to_string(), "owner/repo:main:01234567");
        assert_eq!(identity.directory_name(), "owner_repo_main_01234567");
    }

    #[test]
    fn the_directory_name_is_one_safe_component() {
        let identity = RepoIdentity::new(
            "myorg/My Project/repo",
            "feature/x",
            "abcdef0123456789abcdef0123456789abcdef01",
        );
        assert_eq!(
            identity.to_string(),
            "myorg/My Project/repo:feature/x:abcdef01"
        );
        assert_eq!(
            identity.directory_name(),
            "myorg_My_Project_repo_feature_x_abcdef01"
        );
        let dotted = RepoIdentity::new(".hidden", "main", "abcdef0123");
        assert_eq!(dotted.directory_name(), "_.hidden_main_abcdef01");
    }
}

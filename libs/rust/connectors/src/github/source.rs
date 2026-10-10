//! One GitHub repository branch as a `ContentSource` (ADR-0030 decision 3).
//!
//! * **List.** The branch resolves to its tree (`branches/{b}`, falling back
//!   to `commits/{ref}` for a tag or sha), and the tree is read recursively
//!   in one call. GitHub truncates a recursive tree past its own limit
//!   (100 000 entries or 7 MB); a truncated answer is not trusted, and the
//!   listing walks the tree one directory at a time instead.
//! * **Version.** Each file's blob sha: exact, and free with the listing.
//! * **Fetch.** `git/blobs/{sha}` with the raw media type, under the
//!   document cap. A file the listing says is over the cap is refused before
//!   it is requested.
//! * Symbolic links and submodules are not documents.

use std::collections::VecDeque;
use std::sync::Arc;
use std::time::SystemTime;

use elitea_content_source::{ContentSource, Document, DocumentRef, SourceError};
use serde_json::{Map, Value};

use super::client::{GitHubRequestKind, GitHubRest, REQUEST_TIMEOUT, validate_repository};
use super::config::GitHubToolkitConfig;
use crate::egress::HostAllowlist;
use crate::source::{
    Backoff, Failure, Listed, ListingCache, SourceHttp, SourceLimits, title_of, valid_key,
};
use crate::transport::header::ACCEPT;
use crate::transport::{HeaderValue, Request, StatusCode, Transport};

const PROVIDER: &str = "GitHub";
const RAW_ACCEPT: &str = "application/vnd.github.raw+json";
const SYMLINK_MODE: &str = "120000";

/// One repository branch, listed and fetched through the shared client.
pub struct GitHubSource {
    rest: GitHubRest,
    http: SourceHttp,
    owner: String,
    repository: String,
    branch: String,
    cache: ListingCache,
}

impl GitHubSource {
    /// The configured repository at its active branch, every request through
    /// `allowlist` (fail-closed) and then `transport`.
    ///
    /// # Errors
    ///
    /// The configured origin is not on the allowlist, the repository is not
    /// `owner/name`, or the credential cannot be used.
    pub fn new(
        config: GitHubToolkitConfig,
        allowlist: HostAllowlist,
        transport: Arc<dyn Transport>,
    ) -> Result<Self, SourceError> {
        let http = SourceHttp::new(allowlist, transport);
        if !http.permits(config.base_url()) {
            return Err(Failure::Refused.into_source_error(PROVIDER, None));
        }
        let (owner, repository) = validate_repository(config.repository())
            .map(|(owner, name)| (owner.to_owned(), name.to_owned()))
            .map_err(|error| {
                Failure::Client(error.to_string()).into_source_error(PROVIDER, None)
            })?;
        let branch = config.active_branch().to_owned();
        let rest = GitHubRest::new(config, http.transport()).map_err(|error| {
            Failure::Client(error.to_string()).into_source_error(PROVIDER, None)
        })?;
        Ok(Self {
            rest,
            http,
            owner,
            repository,
            branch,
            cache: ListingCache::default(),
        })
    }

    /// Index `branch` (a branch, tag or commit) instead of the active one.
    #[must_use]
    pub fn with_branch(mut self, branch: impl Into<String>) -> Self {
        self.branch = branch.into();
        self
    }

    #[must_use]
    pub fn with_limits(mut self, limits: SourceLimits) -> Self {
        self.http = self.http.with_limits(limits);
        self
    }

    #[must_use]
    pub fn with_backoff(mut self, backoff: Backoff) -> Self {
        self.http = self.http.with_backoff(backoff);
        self
    }

    fn request(&self, path: &[&str], query: &[(&str, String)]) -> Result<Request, Failure> {
        let mut segments = vec!["repos", self.owner.as_str(), self.repository.as_str()];
        segments.extend_from_slice(path);
        self.rest
            .build_request_at(
                GitHubRequestKind::Repository,
                &segments,
                query,
                REQUEST_TIMEOUT,
                SystemTime::now(),
            )
            .map_err(|error| Failure::Client(error.to_string()))
    }

    async fn get(&self, path: &[&str], query: &[(&str, String)]) -> Result<Value, Failure> {
        let request = self.request(path, query)?;
        Ok(self.http.json(request).await?.1)
    }

    /// The branch's (or ref's) root tree sha.
    async fn tree_sha(&self) -> Result<String, Failure> {
        let branch = self.branch.as_str();
        let value = match self.get(&["branches", branch], &[]).await {
            Ok(value) => value,
            Err(Failure::Status(StatusCode::NOT_FOUND)) => {
                self.get(&["commits", branch], &[]).await?
            }
            Err(failure) => return Err(failure),
        };
        // `branches/{b}` nests the commit once more than `commits/{ref}`.
        let commit = value
            .get("commit")
            .ok_or(Failure::InvalidResponse("the branch answer has no commit"))?;
        let tree = commit
            .get("commit")
            .and_then(|inner| inner.get("tree"))
            .or_else(|| commit.get("tree"));
        tree.and_then(|tree| tree.get("sha"))
            .and_then(Value::as_str)
            .filter(|sha| valid_sha(sha))
            .map(str::to_ascii_lowercase)
            .ok_or(Failure::InvalidResponse(
                "the branch answer has no tree sha",
            ))
    }

    async fn listing(&self) -> Result<Vec<Listed>, Failure> {
        let root = self.tree_sha().await?;
        let recursive = self
            .get(&["git", "trees", &root], &[("recursive", "1".to_owned())])
            .await?;
        let mut pages = 1;
        let mut listed = Vec::new();
        if !truncated(&recursive)? {
            collect(
                &recursive,
                "",
                &mut listed,
                &mut VecDeque::new(),
                &self.http,
            )?;
            return Ok(listed);
        }
        // Truncated: walk one directory at a time.
        let mut pending = VecDeque::from([(String::new(), root)]);
        while let Some((prefix, sha)) = pending.pop_front() {
            pages += 1;
            self.http.check_pages(pages)?;
            let tree = self.get(&["git", "trees", &sha], &[]).await?;
            if truncated(&tree)? {
                return Err(Failure::InvalidResponse(
                    "one directory holds more entries than GitHub lists",
                ));
            }
            collect(&tree, &prefix, &mut listed, &mut pending, &self.http)?;
        }
        Ok(listed)
    }

    async fn listed(&self) -> Result<Arc<std::collections::BTreeMap<String, Listed>>, SourceError> {
        if let Some(listing) = self.cache.get().await {
            return Ok(listing);
        }
        self.list().await?;
        self.cache
            .get()
            .await
            .ok_or_else(|| SourceError::Unavailable(format!("{PROVIDER}: the listing is empty")))
    }

    /// Where a reader opens the file, for github.com and GitHub Enterprise
    /// (`https://host/api/v3`).
    fn uri(&self, key: &str) -> Option<String> {
        let base = self.rest.config().base_url();
        let host = base.host_str()?;
        let web = match (host, base.path().trim_end_matches('/')) {
            ("api.github.com", "") => "https://github.com".to_owned(),
            (_, "/api/v3") => format!("{}://{}", base.scheme(), base.authority()),
            _ => return None,
        };
        Some(format!(
            "{web}/{}/{}/blob/{}/{key}",
            self.owner, self.repository, self.branch
        ))
    }
}

fn valid_sha(sha: &str) -> bool {
    matches!(sha.len(), 40 | 64) && sha.bytes().all(|byte| byte.is_ascii_hexdigit())
}

fn truncated(tree: &Value) -> Result<bool, Failure> {
    match tree.get("truncated") {
        None | Some(Value::Null) => Ok(false),
        Some(Value::Bool(flag)) => Ok(*flag),
        Some(_) => Err(Failure::InvalidResponse(
            "a tree's truncated flag is not a boolean",
        )),
    }
}

/// Add a tree answer's blobs to `listed` and its subtrees to `pending`.
fn collect(
    tree: &Value,
    prefix: &str,
    listed: &mut Vec<Listed>,
    pending: &mut VecDeque<(String, String)>,
    http: &SourceHttp,
) -> Result<(), Failure> {
    let entries = tree
        .get("tree")
        .and_then(Value::as_array)
        .ok_or(Failure::InvalidResponse("a tree answer has no entries"))?;
    for entry in entries {
        let text = |name: &str| entry.get(name).and_then(Value::as_str);
        let (Some(path), Some(kind), Some(sha)) = (text("path"), text("type"), text("sha")) else {
            return Err(Failure::InvalidResponse("a tree entry is incomplete"));
        };
        let key = if prefix.is_empty() {
            path.to_owned()
        } else {
            format!("{prefix}/{path}")
        };
        if !valid_key(&key) || !valid_sha(sha) {
            return Err(Failure::InvalidResponse(
                "a tree entry has an unsafe path or sha",
            ));
        }
        match kind {
            "tree" => pending.push_back((key, sha.to_ascii_lowercase())),
            "blob" if text("mode") != Some(SYMLINK_MODE) => {
                let size = entry.get("size").and_then(Value::as_u64).unwrap_or(0);
                let sha = sha.to_ascii_lowercase();
                listed.push(Listed::new(key, sha.clone(), size, sha));
                http.check_count(listed.len())?;
            }
            // Symbolic links and submodules (`commit`) are not documents.
            _ => {}
        }
    }
    Ok(())
}

impl ContentSource for GitHubSource {
    async fn list(&self) -> Result<Vec<DocumentRef>, SourceError> {
        let listing = self
            .listing()
            .await
            .map_err(|failure| failure.into_source_error(PROVIDER, None))?;
        Ok(self.cache.store(listing).await)
    }

    async fn fetch(&self, key: &str) -> Result<Document, SourceError> {
        let listing = self.listed().await?;
        let listed = listing
            .get(key)
            .ok_or_else(|| SourceError::NotFound(key.to_owned()))?;
        let failed = |failure: Failure| failure.into_source_error(PROVIDER, Some(key));
        self.http
            .check_size(listed.reference.size)
            .map_err(failed)?;
        let mut request = self
            .request(&["git", "blobs", &listed.handle], &[])
            .map_err(failed)?;
        request
            .headers_mut()
            .insert(ACCEPT, HeaderValue::from_static(RAW_ACCEPT));
        let bytes = self.http.raw(request).await.map_err(failed)?;
        let mut reference = listed.reference.clone();
        reference.size = bytes.len() as u64;
        let mut metadata = Map::new();
        metadata.insert("sha".to_owned(), Value::String(listed.handle.clone()));
        metadata.insert("branch".to_owned(), Value::String(self.branch.clone()));
        metadata.insert(
            "repository".to_owned(),
            Value::String(format!("{}/{}", self.owner, self.repository)),
        );
        Ok(Document {
            title: title_of(key),
            uri: self.uri(key),
            reference,
            bytes,
            metadata,
        })
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::source::fixture::{Reply, Scripted};
    use serde_json::json;
    use std::time::Duration;

    const TREE: &str = "1111111111111111111111111111111111111111";
    const SUB: &str = "2222222222222222222222222222222222222222";
    const README: &str = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa";
    const CODE: &str = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb";

    fn config() -> GitHubToolkitConfig {
        let settings = json!({
            "github_configuration": {
                "base_url": "https://api.github.com",
                "access_token": "fixture-token",
            },
            "repository": "EliteaAI/demo",
            "active_branch": "main",
            "base_branch": "main",
            "selected_tools": ["read_file"],
        });
        GitHubToolkitConfig::parse(settings.as_object().expect("settings")).expect("config")
    }

    fn allow() -> HostAllowlist {
        HostAllowlist::parse(Some("api.github.com"))
    }

    fn branch() -> Value {
        json!({"name": "main", "commit": {"sha": "c", "commit": {"tree": {"sha": TREE}}}})
    }

    fn entry(path: &str, kind: &str, sha: &str, size: Option<u64>) -> Value {
        let mut entry = json!({"path": path, "type": kind, "sha": sha, "mode": "100644"});
        if let Some(size) = size {
            entry["size"] = json!(size);
        }
        entry
    }

    #[tokio::test]
    async fn a_recursive_tree_lists_blobs_with_their_sha_as_version() {
        let transport = Scripted::new(|request, _| match request.url().path() {
            "/repos/EliteaAI/demo/branches/main" => Reply::json(&branch()),
            "/repos/EliteaAI/demo/git/trees/1111111111111111111111111111111111111111" => {
                Reply::json(&json!({"truncated": false, "tree": [
                    entry("README.md", "blob", README, Some(5)),
                    entry("src", "tree", SUB, None),
                    entry("src/lib.rs", "blob", CODE, Some(9)),
                    {"path": "link", "type": "blob", "sha": CODE, "mode": "120000", "size": 3},
                    {"path": "vendor/x", "type": "commit", "sha": CODE, "mode": "160000"},
                ]}))
            }
            "/repos/EliteaAI/demo/git/blobs/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" => {
                Reply::bytes(b"# hi\n")
            }
            other => panic!("unexpected request {other}"),
        });
        let source = GitHubSource::new(config(), allow(), transport.clone()).expect("source");
        let listed = source.list().await.expect("listing");
        let keys: Vec<_> = listed
            .iter()
            .map(|d| (d.key.as_str(), d.version.as_str(), d.size))
            .collect();
        assert_eq!(keys, [("README.md", README, 5), ("src/lib.rs", CODE, 9)]);
        assert_eq!(listed[0].mime, "text/markdown");
        let document = source.fetch("README.md").await.expect("document");
        assert_eq!(document.bytes, b"# hi\n");
        assert_eq!(document.reference.version, README);
        assert_eq!(
            document.uri.as_deref(),
            Some("https://github.com/EliteaAI/demo/blob/main/README.md")
        );
        let blob = transport.requests().pop().expect("blob request");
        assert_eq!(blob.headers().get(ACCEPT).expect("accept"), RAW_ACCEPT);
        assert!(matches!(
            source.fetch("nope.md").await,
            Err(SourceError::NotFound(_))
        ));
    }

    #[tokio::test]
    async fn a_truncated_tree_is_walked_one_directory_at_a_time() {
        let transport = Scripted::new(|request, _| {
            let recursive = request.url().query() == Some("recursive=1");
            match request.url().path() {
                "/repos/EliteaAI/demo/branches/main" => Reply::json(&branch()),
                "/repos/EliteaAI/demo/git/trees/1111111111111111111111111111111111111111"
                    if recursive =>
                {
                    Reply::json(
                        &json!({"truncated": true, "tree": [entry("README.md", "blob", README, Some(5))]}),
                    )
                }
                "/repos/EliteaAI/demo/git/trees/1111111111111111111111111111111111111111" => {
                    Reply::json(&json!({"truncated": false, "tree": [
                        entry("README.md", "blob", README, Some(5)),
                        entry("src", "tree", SUB, None),
                    ]}))
                }
                "/repos/EliteaAI/demo/git/trees/2222222222222222222222222222222222222222" => {
                    Reply::json(&json!({"truncated": false, "tree": [
                        entry("lib.rs", "blob", CODE, Some(9)),
                    ]}))
                }
                other => panic!("unexpected request {other}"),
            }
        });
        let source = GitHubSource::new(config(), allow(), transport.clone()).expect("source");
        let keys: Vec<_> = source
            .list()
            .await
            .expect("listing")
            .into_iter()
            .map(|d| (d.key, d.version))
            .collect();
        assert_eq!(
            keys,
            [
                ("README.md".to_owned(), README.to_owned()),
                ("src/lib.rs".to_owned(), CODE.to_owned())
            ]
        );
        assert_eq!(transport.seen().len(), 4, "{:?}", transport.seen());
    }

    #[tokio::test]
    async fn a_tag_resolves_through_the_commits_route() {
        let transport = Scripted::new(|request, _| match request.url().path() {
            "/repos/EliteaAI/demo/branches/v1.0" => Reply::status(StatusCode::NOT_FOUND),
            "/repos/EliteaAI/demo/commits/v1.0" => {
                Reply::json(&json!({"sha": "c", "commit": {"tree": {"sha": TREE}}}))
            }
            "/repos/EliteaAI/demo/git/trees/1111111111111111111111111111111111111111" => {
                Reply::json(&json!({"truncated": false, "tree": []}))
            }
            other => panic!("unexpected request {other}"),
        });
        let source = GitHubSource::new(config(), allow(), transport)
            .expect("source")
            .with_branch("v1.0");
        assert_eq!(source.list().await.expect("listing"), []);
    }

    #[tokio::test]
    async fn a_document_over_the_cap_is_refused_before_and_while_reading() {
        let transport = Scripted::new(|request, _| match request.url().path() {
            "/repos/EliteaAI/demo/branches/main" => Reply::json(&branch()),
            "/repos/EliteaAI/demo/git/trees/1111111111111111111111111111111111111111" => {
                Reply::json(&json!({"tree": [
                    entry("big.bin", "blob", README, Some(11)),
                    entry("lies.bin", "blob", CODE, Some(4)),
                ]}))
            }
            "/repos/EliteaAI/demo/git/blobs/bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" => {
                Reply::bytes(b"more than ten bytes")
            }
            other => panic!("unexpected request {other}"),
        });
        let limits = SourceLimits {
            max_document_bytes: 10,
            ..SourceLimits::default()
        };
        let source = GitHubSource::new(config(), allow(), transport.clone())
            .expect("source")
            .with_limits(limits);
        source.list().await.expect("listing");
        let big = source.fetch("big.bin").await.expect_err("refused");
        assert!(big.to_string().contains("larger than 10 bytes"), "{big}");
        assert!(
            !transport.seen().iter().any(|seen| seen.contains(README)),
            "the oversized blob is never requested"
        );
        let lies = source.fetch("lies.bin").await.expect_err("refused");
        assert!(lies.to_string().contains("larger than 10 bytes"), "{lies}");
    }

    #[tokio::test(start_paused = true)]
    async fn a_rate_limit_is_waited_out_within_the_bound() {
        let transport = Scripted::new(|request, index| match (request.url().path(), index) {
            ("/repos/EliteaAI/demo/branches/main", 0) => Reply::rate_limited(Some("2")),
            ("/repos/EliteaAI/demo/branches/main", _) => Reply::json(&branch()),
            ("/repos/EliteaAI/demo/git/trees/1111111111111111111111111111111111111111", _) => {
                Reply::json(&json!({"tree": []}))
            }
            (other, _) => panic!("unexpected request {other}"),
        });
        let source = GitHubSource::new(config(), allow(), transport.clone()).expect("source");
        let started = tokio::time::Instant::now();
        source.list().await.expect("listing after the wait");
        assert_eq!(started.elapsed(), Duration::from_secs(2));
        assert_eq!(transport.seen().len(), 3);
    }

    #[tokio::test]
    async fn a_host_off_the_allowlist_is_refused_before_any_request() {
        let transport = Scripted::new(|_, _| panic!("no request may be sent"));
        let refused = GitHubSource::new(
            config(),
            HostAllowlist::parse(Some("gitlab.example")),
            transport.clone(),
        )
        .err()
        .expect("refused");
        assert!(
            refused.to_string().contains("egress allowlist"),
            "{refused}"
        );
        let closed = GitHubSource::new(config(), HostAllowlist::default(), transport.clone());
        assert!(closed.is_err(), "an empty allowlist refuses everything");
        assert!(transport.seen().is_empty());
    }
}

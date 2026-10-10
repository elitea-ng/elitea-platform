//! One Bitbucket repository branch as a `ContentSource` (ADR-0030
//! decision 3), on Bitbucket Server / Data Center or Bitbucket Cloud.
//!
//! * **Server.** The branch resolves to its head commit (`commits?until=`),
//!   and `browse/{dir}?at={commit}` is read one directory at a time, paged
//!   by `start` / `nextPageStart` until `isLastPage`. Each file carries its
//!   blob id (`contentId`) and size: the version is the blob id. Fetch is
//!   `raw/{path}?at={commit}` at the listed commit, so the bytes are the
//!   listed version even if the branch has moved since.
//! * **Cloud.** The branch resolves to its head commit, and `src/{commit}/
//!   {dir}/` is read one directory at a time, following the `next` link —
//!   which must stay on this repository's API resource and pass the egress
//!   allowlist. Cloud exposes no per-file blob id or last-change commit in
//!   a listing, so the version is the listed commit: every file is "changed"
//!   when the branch moves (the cheapest exact signal Cloud offers). Fetch
//!   is `src/{commit}/{path}`.
//! * Submodules and symbolic links are not documents.

use std::collections::{BTreeMap, VecDeque};
use std::sync::Arc;

use elitea_content_source::{ContentSource, Document, DocumentRef, SourceError};
use serde_json::{Map, Value};

use super::client::{BitbucketRest, Body, valid_hash};
use super::config::{BitbucketHosting, BitbucketToolkitConfig};
use crate::egress::HostAllowlist;
use crate::source::{
    Backoff, Failure, Listed, ListingCache, SourceHttp, SourceLimits, title_of, valid_key,
};
use crate::transport::header::ACCEPT;
use crate::transport::{HeaderValue, Method, Request, Transport, Url};

const PROVIDER: &str = "Bitbucket";
const SERVER_PAGE_SIZE: &str = "500";
const CLOUD_PAGE_SIZE: &str = "100";

/// One repository branch, listed and fetched through the shared client.
pub struct BitbucketSource {
    rest: BitbucketRest,
    http: SourceHttp,
    branch: String,
    cache: ListingCache,
}

impl BitbucketSource {
    /// The configured repository at its configured branch, every request
    /// through `allowlist` (fail-closed) and then `transport`.
    ///
    /// # Errors
    ///
    /// The configured origin is not on the allowlist.
    pub fn new(
        config: BitbucketToolkitConfig,
        allowlist: HostAllowlist,
        transport: Arc<dyn Transport>,
    ) -> Result<Self, SourceError> {
        let http = SourceHttp::new(allowlist, transport);
        if !http.permits(config.base_url()) {
            return Err(Failure::Refused.into_source_error(PROVIDER, None));
        }
        let branch = config.branch().to_owned();
        Ok(Self {
            rest: BitbucketRest::new(config, http.transport()),
            http,
            branch,
            cache: ListingCache::default(),
        })
    }

    /// Index `branch` instead of the configured one.
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

    fn cloud(&self) -> bool {
        self.rest.config().hosting() == BitbucketHosting::Cloud
    }

    fn get(&self, url: Url, query: &[(&str, &str)]) -> Result<Request, Failure> {
        self.rest
            .request(Method::GET, url, query, &Body::None)
            .map_err(|error| Failure::Client(error.to_string()))
    }

    fn url(&self, suffix: &[&str]) -> Result<Url, Failure> {
        self.rest
            .repo_url(suffix)
            .map_err(|error| Failure::Client(error.to_string()))
    }

    async fn listing(&self) -> Result<Vec<Listed>, Failure> {
        if self.cloud() {
            self.cloud_listing().await
        } else {
            self.server_listing().await
        }
    }

    async fn server_listing(&self) -> Result<Vec<Listed>, Failure> {
        let commit = self.server_commit().await?;
        let mut listed = Vec::new();
        let mut pending = VecDeque::from([String::new()]);
        let mut pages = 0;
        while let Some(directory) = pending.pop_front() {
            let mut start = String::from("0");
            loop {
                pages += 1;
                self.http.check_pages(pages)?;
                let mut suffix = vec!["browse"];
                suffix.extend(directory.split('/').filter(|part| !part.is_empty()));
                let request = self.get(
                    self.url(&suffix)?,
                    &[
                        ("at", commit.as_str()),
                        ("limit", SERVER_PAGE_SIZE),
                        ("start", start.as_str()),
                    ],
                )?;
                let (_, body) = self.http.json(request).await?;
                let children = body.get("children").ok_or(Failure::InvalidResponse(
                    "a directory answer has no children",
                ))?;
                let values = children
                    .get("values")
                    .and_then(Value::as_array)
                    .ok_or(Failure::InvalidResponse("a directory page has no values"))?;
                for child in values {
                    let name = child
                        .get("path")
                        .and_then(|path| path.get("toString"))
                        .and_then(Value::as_str)
                        .ok_or(Failure::InvalidResponse("a directory entry has no path"))?;
                    let key = if directory.is_empty() {
                        name.to_owned()
                    } else {
                        format!("{directory}/{name}")
                    };
                    if !valid_key(&key) {
                        return Err(Failure::InvalidResponse(
                            "a directory entry has an unsafe path",
                        ));
                    }
                    match child.get("type").and_then(Value::as_str) {
                        Some("DIRECTORY") => pending.push_back(key),
                        Some("FILE") => {
                            let blob = child
                                .get("contentId")
                                .and_then(Value::as_str)
                                .filter(|id| valid_hash(id))
                                .ok_or(Failure::InvalidResponse("a file entry has no blob id"))?;
                            let size = child.get("size").and_then(Value::as_u64).unwrap_or(0);
                            let blob = blob.to_ascii_lowercase();
                            listed.push(Listed::new(key, blob, size, commit.clone()));
                            self.http.check_count(listed.len())?;
                        }
                        // Submodules (and anything newer) are not documents.
                        _ => {}
                    }
                }
                if children.get("isLastPage").and_then(Value::as_bool) != Some(false) {
                    break;
                }
                start = children
                    .get("nextPageStart")
                    .and_then(Value::as_u64)
                    .ok_or(Failure::InvalidResponse(
                        "a directory page has no next start",
                    ))?
                    .to_string();
            }
        }
        Ok(listed)
    }

    async fn cloud_commit(&self) -> Result<String, Failure> {
        self.rest
            .branch_hash(&self.branch)
            .await
            .map(|hash| hash.to_ascii_lowercase())
            .map_err(|error| Failure::Client(error.to_string()))
    }

    /// Server: the commit the branch (or tag, or sha) points at now. Every
    /// file of the listing is then read at this commit, so a branch that
    /// moves afterwards cannot change what a fetch returns.
    async fn server_commit(&self) -> Result<String, Failure> {
        let request = self.get(
            self.url(&["commits"])?,
            &[("until", self.branch.as_str()), ("limit", "1")],
        )?;
        let (_, body) = self.http.json(request).await?;
        body.get("values")
            .and_then(Value::as_array)
            .and_then(|values| values.first())
            .and_then(|commit| commit.get("id"))
            .and_then(Value::as_str)
            .filter(|id| valid_hash(id))
            .map(str::to_ascii_lowercase)
            .ok_or(Failure::InvalidResponse(
                "the branch answer has no head commit",
            ))
    }

    async fn cloud_listing(&self) -> Result<Vec<Listed>, Failure> {
        let commit = self.cloud_commit().await?;
        let mut listed = Vec::new();
        let mut pending = VecDeque::from([String::new()]);
        let mut pages = 0;
        while let Some(directory) = pending.pop_front() {
            let mut suffix = vec!["src", commit.as_str()];
            suffix.extend(directory.split('/').filter(|part| !part.is_empty()));
            // A trailing slash asks for the directory's listing.
            suffix.push("");
            let mut next = Some(self.get(self.url(&suffix)?, &[("pagelen", CLOUD_PAGE_SIZE)])?);
            while let Some(request) = next.take() {
                pages += 1;
                self.http.check_pages(pages)?;
                let (_, body) = self.http.json(request).await?;
                let values = body
                    .get("values")
                    .and_then(Value::as_array)
                    .ok_or(Failure::InvalidResponse("a directory page has no values"))?;
                for value in values {
                    let path = value
                        .get("path")
                        .and_then(Value::as_str)
                        .ok_or(Failure::InvalidResponse("a directory entry has no path"))?;
                    if !valid_key(path) {
                        return Err(Failure::InvalidResponse(
                            "a directory entry has an unsafe path",
                        ));
                    }
                    let link = value
                        .get("attributes")
                        .and_then(Value::as_array)
                        .is_some_and(|attributes| {
                            attributes.iter().any(|attribute| {
                                matches!(attribute.as_str(), Some("link" | "subrepository"))
                            })
                        });
                    match value.get("type").and_then(Value::as_str) {
                        Some("commit_directory") => pending.push_back(path.to_owned()),
                        Some("commit_file") if !link => {
                            let size = value.get("size").and_then(Value::as_u64).unwrap_or(0);
                            listed.push(Listed::new(
                                path.to_owned(),
                                commit.clone(),
                                size,
                                commit.clone(),
                            ));
                            self.http.check_count(listed.len())?;
                        }
                        _ => {}
                    }
                }
                next = match body.get("next") {
                    None | Some(Value::Null) => None,
                    Some(Value::String(link)) => {
                        let url = self.rest.same_repository_link(link).map_err(|_| {
                            Failure::InvalidResponse("a next link leaves the repository")
                        })?;
                        Some(self.get(url, &[])?)
                    }
                    Some(_) => return Err(Failure::InvalidResponse("a next link is not a string")),
                };
            }
        }
        Ok(listed)
    }

    async fn listed(&self) -> Result<Arc<BTreeMap<String, Listed>>, SourceError> {
        self.cache
            .get_or_list(|| async {
                self.listing()
                    .await
                    .map_err(|failure| failure.into_source_error(PROVIDER, None))
            })
            .await
    }
}

impl ContentSource for BitbucketSource {
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
        let segments: Vec<&str> = key.split('/').collect();
        let mut request = if self.cloud() {
            let mut suffix = vec!["src", listed.handle.as_str()];
            suffix.extend(&segments);
            self.get(self.url(&suffix).map_err(failed)?, &[])
        } else {
            let mut suffix = vec!["raw"];
            suffix.extend(&segments);
            // The commit the listing resolved, not the branch as it is now.
            self.get(
                self.url(&suffix).map_err(failed)?,
                &[("at", listed.handle.as_str())],
            )
        }
        .map_err(failed)?;
        request
            .headers_mut()
            .insert(ACCEPT, HeaderValue::from_static("*/*"));
        let bytes = self.http.raw(request).await.map_err(failed)?;
        let mut reference = listed.reference.clone();
        reference.size = bytes.len() as u64;
        let mut metadata = Map::new();
        metadata.insert("commit".to_owned(), Value::String(listed.handle.clone()));
        if !self.cloud() {
            metadata.insert(
                "blob_id".to_owned(),
                Value::String(listed.reference.version.clone()),
            );
        }
        metadata.insert("branch".to_owned(), Value::String(self.branch.clone()));
        Ok(Document {
            title: title_of(key),
            uri: None,
            reference,
            bytes,
            metadata,
        })
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::source::fixture::{Reply, Scripted, query};
    use serde_json::json;

    const BLOB_A: &str = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa";
    const BLOB_B: &str = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb";
    const HEAD: &str = "cccccccccccccccccccccccccccccccccccccccc";

    fn config(hosting: &str) -> BitbucketToolkitConfig {
        let url = if hosting == "cloud" {
            "https://api.bitbucket.org"
        } else {
            "https://bitbucket.example/context"
        };
        let settings = json!({
            "bitbucket_configuration": {"url": url, "username": "u", "password": "p"},
            "project": "PRJ",
            "repository": "repo",
            "branch": "main",
            "cloud": hosting == "cloud",
        });
        BitbucketToolkitConfig::parse(settings.as_object().expect("settings")).expect("config")
    }

    fn file(name: &str, blob: &str, size: u64) -> Value {
        json!({"path": {"toString": name}, "type": "FILE", "contentId": blob, "size": size})
    }

    #[tokio::test]
    async fn server_walks_directories_and_pages_with_blob_ids_as_versions() {
        let transport = Scripted::new(|request, _| {
            let path = request.url().path();
            let base = "/context/rest/api/1.0/projects/PRJ/repos/repo";
            if path.strip_prefix(base) == Some("/commits") {
                assert_eq!(query(request, "until").as_deref(), Some("main"));
                return Reply::json(&json!({"values": [{"id": HEAD}]}));
            }
            assert_eq!(
                query(request, "at").as_deref(),
                Some(HEAD),
                "every read is pinned to the resolved commit"
            );
            match (path.strip_prefix(base), query(request, "start").as_deref()) {
                (Some("/browse"), Some("0")) => Reply::json(&json!({"children": {
                    "isLastPage": false, "nextPageStart": 1,
                    "values": [file("README.md", BLOB_A, 5)],
                }})),
                (Some("/browse"), Some("1")) => Reply::json(&json!({"children": {
                    "isLastPage": true,
                    "values": [
                        {"path": {"toString": "src"}, "type": "DIRECTORY"},
                        {"path": {"toString": "sub"}, "type": "SUBMODULE"},
                    ],
                }})),
                (Some("/browse/src"), Some("0")) => Reply::json(&json!({"children": {
                    "isLastPage": true, "values": [file("lib.rs", BLOB_B, 9)],
                }})),
                (Some("/raw/src/lib.rs"), None) => Reply::bytes(b"fn x() {}"),
                other => panic!("unexpected request {other:?}"),
            }
        });
        let source = BitbucketSource::new(
            config("server"),
            HostAllowlist::parse(Some("bitbucket.example")),
            transport.clone(),
        )
        .expect("source");
        let listed = source.list().await.expect("listing");
        let keys: Vec<_> = listed
            .iter()
            .map(|d| (d.key.as_str(), d.version.as_str(), d.size))
            .collect();
        assert_eq!(keys, [("README.md", BLOB_A, 5), ("src/lib.rs", BLOB_B, 9)]);
        let document = source.fetch("src/lib.rs").await.expect("document");
        assert_eq!(document.bytes, b"fn x() {}");
        let authorization = transport.requests()[0]
            .headers()
            .get("authorization")
            .cloned()
            .expect("credential");
        assert!(authorization.is_sensitive());
    }

    #[tokio::test]
    async fn server_fetch_reads_the_listed_commit_after_the_branch_moves() {
        const MOVED: &str = "dddddddddddddddddddddddddddddddddddddddd";
        let head = Arc::new(std::sync::Mutex::new(HEAD.to_owned()));
        let current = Arc::clone(&head);
        let transport = Scripted::new(move |request, _| {
            let base = "/context/rest/api/1.0/projects/PRJ/repos/repo";
            let path = request.url().path().strip_prefix(base).expect("repo path");
            let now = current.lock().expect("head").clone();
            match path {
                "/commits" => Reply::json(&json!({"values": [{"id": now}]})),
                "/browse" => {
                    assert_eq!(query(request, "at").as_deref(), Some(HEAD));
                    Reply::json(&json!({"children": {
                        "isLastPage": true, "values": [file("a.md", BLOB_A, 5)],
                    }}))
                }
                "/raw/a.md" => match query(request, "at").as_deref() {
                    Some(HEAD) => Reply::bytes(b"listed"),
                    Some(MOVED) => Reply::bytes(b"moved!"),
                    other => panic!("unexpected revision {other:?}"),
                },
                other => panic!("unexpected request {other}"),
            }
        });
        let source = BitbucketSource::new(
            config("server"),
            HostAllowlist::parse(Some("bitbucket.example")),
            transport.clone(),
        )
        .expect("source");
        let listed = source.list().await.expect("listing");
        assert_eq!(listed[0].version, BLOB_A);
        *head.lock().expect("head") = MOVED.to_owned();
        let document = source.fetch("a.md").await.expect("document");
        assert_eq!(
            document.bytes, b"listed",
            "the listed version, not the new head"
        );
        assert_eq!(document.reference.version, BLOB_A);
        assert_eq!(document.metadata["commit"], HEAD);
        assert_eq!(document.metadata["blob_id"], BLOB_A);
    }

    #[tokio::test]
    async fn cloud_follows_next_links_on_the_same_repository_only() {
        let transport = Scripted::new(|request, _| {
            let path = request.url().path();
            match (path, query(request, "page").as_deref()) {
                ("/2.0/repositories/PRJ/repo/refs/branches/main", _) => {
                    Reply::json(&json!({"name": "main", "target": {"hash": HEAD}}))
                }
                (
                    "/2.0/repositories/PRJ/repo/src/cccccccccccccccccccccccccccccccccccccccc/",
                    None,
                ) => Reply::json(&json!({
                    "values": [
                        {"type": "commit_file", "path": "README.md", "size": 5},
                        {"type": "commit_directory", "path": "docs"},
                    ],
                    "next": "https://api.bitbucket.org/2.0/repositories/PRJ/repo/src/cccccccccccccccccccccccccccccccccccccccc/?page=2",
                })),
                (
                    "/2.0/repositories/PRJ/repo/src/cccccccccccccccccccccccccccccccccccccccc/",
                    Some("2"),
                ) => Reply::json(&json!({"values": [
                    {"type": "commit_file", "path": "link", "size": 1, "attributes": ["link"]},
                ]})),
                (
                    "/2.0/repositories/PRJ/repo/src/cccccccccccccccccccccccccccccccccccccccc/docs/",
                    _,
                ) => Reply::json(&json!({
                    "values": [{"type": "commit_file", "path": "docs/guide.md", "size": 7}],
                    "next": "https://evil.example/2.0/repositories/PRJ/repo/src/x/?page=2",
                })),
                other => panic!("unexpected request {other:?}"),
            }
        });
        let source = BitbucketSource::new(
            config("cloud"),
            HostAllowlist::parse(Some("api.bitbucket.org")),
            transport.clone(),
        )
        .expect("source");
        let refused = source
            .list()
            .await
            .expect_err("a foreign next link is refused");
        assert!(
            refused.to_string().contains("leaves the repository"),
            "{refused}"
        );
        assert!(!transport.seen().iter().any(|seen| seen.contains("evil")));
    }

    #[tokio::test]
    async fn cloud_versions_are_the_listed_commit() {
        let transport = Scripted::new(|request, _| match request.url().path() {
            "/2.0/repositories/PRJ/repo/refs/branches/main" => {
                Reply::json(&json!({"target": {"hash": HEAD}}))
            }
            "/2.0/repositories/PRJ/repo/src/cccccccccccccccccccccccccccccccccccccccc/" => {
                Reply::json(
                    &json!({"values": [{"type": "commit_file", "path": "a.md", "size": 2}]}),
                )
            }
            "/2.0/repositories/PRJ/repo/src/cccccccccccccccccccccccccccccccccccccccc/a.md" => {
                Reply::bytes(b"hi")
            }
            other => panic!("unexpected request {other}"),
        });
        let source = BitbucketSource::new(
            config("cloud"),
            HostAllowlist::parse(Some("api.bitbucket.org")),
            transport,
        )
        .expect("source");
        let listed = source.list().await.expect("listing");
        assert_eq!(listed[0].version, HEAD);
        assert_eq!(source.fetch("a.md").await.expect("document").bytes, b"hi");
    }
}

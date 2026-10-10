//! One GitLab project branch as a `ContentSource` (ADR-0030 decision 3).
//!
//! * **List.** `repository/tree?recursive=true`, 100 entries a page,
//!   following the `x-next-page` cursor until GitLab stops sending one.
//! * **Version.** Each file's blob id: exact, and free with the listing.
//!   The tree does not carry sizes, so a listed size is 0 until the
//!   document is fetched (the fetched reference carries the real one).
//! * **Fetch.** `repository/blobs/{id}/raw`, under the document cap.
//! * Symbolic links and submodules are not documents.

use std::collections::BTreeMap;
use std::sync::Arc;

use elitea_content_source::{ContentSource, Document, DocumentRef, SourceError};
use serde_json::{Map, Value};

use super::config::GitLabToolkitConfig;
use super::wire::{parse_next_page, project_request};
use crate::egress::HostAllowlist;
use crate::source::{
    Backoff, Failure, Listed, ListingCache, SourceHttp, SourceLimits, title_of, valid_key,
};
use crate::transport::header::ACCEPT;
use crate::transport::{HeaderValue, Method, Request, Transport};

const PROVIDER: &str = "GitLab";
const PAGE_SIZE: &str = "100";
const SYMLINK_MODE: &str = "120000";

/// One project branch, listed and fetched through the shared wire layer.
pub struct GitLabSource {
    config: GitLabToolkitConfig,
    http: SourceHttp,
    branch: String,
    cache: ListingCache,
}

impl GitLabSource {
    /// The configured project at its configured branch, every request
    /// through `allowlist` (fail-closed) and then `transport`.
    ///
    /// # Errors
    ///
    /// The configured origin is not on the allowlist.
    pub fn new(
        config: GitLabToolkitConfig,
        allowlist: HostAllowlist,
        transport: Arc<dyn Transport>,
    ) -> Result<Self, SourceError> {
        let http = SourceHttp::new(allowlist, transport);
        if !http.permits(config.base_url()) {
            return Err(Failure::Refused.into_source_error(PROVIDER, None));
        }
        let branch = config.branch().to_owned();
        Ok(Self {
            config,
            http,
            branch,
            cache: ListingCache::default(),
        })
    }

    /// Index `branch` (a branch, tag or commit) instead of the configured one.
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

    fn request(&self, suffix: &[&str], query: &[(&str, String)]) -> Result<Request, Failure> {
        project_request(&self.config, Method::GET, suffix, query, None)
            .map_err(|error| Failure::Client(error.to_string()))
    }

    async fn listing(&self) -> Result<Vec<Listed>, Failure> {
        let mut listed = Vec::new();
        let mut page = String::from("1");
        let mut pages = 0;
        loop {
            pages += 1;
            self.http.check_pages(pages)?;
            let request = self.request(
                &["repository", "tree"],
                &[
                    ("ref", self.branch.clone()),
                    ("recursive", "true".to_owned()),
                    ("per_page", PAGE_SIZE.to_owned()),
                    ("page", page.clone()),
                ],
            )?;
            let (headers, body) = self.http.json(request).await?;
            let entries = body
                .as_array()
                .ok_or(Failure::InvalidResponse("a tree page is not a list"))?;
            for entry in entries {
                let text = |name: &str| entry.get(name).and_then(Value::as_str);
                let (Some(path), Some(kind), Some(id)) = (text("path"), text("type"), text("id"))
                else {
                    return Err(Failure::InvalidResponse("a tree entry is incomplete"));
                };
                if kind != "blob" || text("mode") == Some(SYMLINK_MODE) {
                    continue;
                }
                if !valid_key(path) || !valid_blob_id(id) {
                    return Err(Failure::InvalidResponse(
                        "a tree entry has an unsafe path or id",
                    ));
                }
                let id = id.to_ascii_lowercase();
                listed.push(Listed::new(path.to_owned(), id.clone(), 0, id));
                self.http.check_count(listed.len())?;
            }
            match parse_next_page(headers.get("x-next-page"), false).map_err(|_| {
                Failure::InvalidResponse("the next-page cursor is not a page number")
            })? {
                Some(next) => page = next.into(),
                None => return Ok(listed),
            }
        }
    }

    async fn listed(&self) -> Result<Arc<BTreeMap<String, Listed>>, SourceError> {
        if let Some(listing) = self.cache.get().await {
            return Ok(listing);
        }
        self.list().await?;
        self.cache
            .get()
            .await
            .ok_or_else(|| SourceError::Unavailable(format!("{PROVIDER}: the listing is empty")))
    }

    fn uri(&self, key: &str) -> Option<String> {
        let project = self.config.repository();
        if project.bytes().all(|byte| byte.is_ascii_digit()) {
            return None;
        }
        let base = self.config.base_url().as_str().trim_end_matches('/');
        Some(format!("{base}/{project}/-/blob/{}/{key}", self.branch))
    }
}

fn valid_blob_id(id: &str) -> bool {
    matches!(id.len(), 40 | 64) && id.bytes().all(|byte| byte.is_ascii_hexdigit())
}

impl ContentSource for GitLabSource {
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
        let mut request = self
            .request(&["repository", "blobs", &listed.handle, "raw"], &[])
            .map_err(failed)?;
        request
            .headers_mut()
            .insert(ACCEPT, HeaderValue::from_static("*/*"));
        let bytes = self.http.raw(request).await.map_err(failed)?;
        let mut reference = listed.reference.clone();
        reference.size = bytes.len() as u64;
        let mut metadata = Map::new();
        metadata.insert("blob_id".to_owned(), Value::String(listed.handle.clone()));
        metadata.insert("branch".to_owned(), Value::String(self.branch.clone()));
        metadata.insert(
            "project".to_owned(),
            Value::String(self.config.repository().to_owned()),
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
    use crate::source::fixture::{Reply, Scripted, query};
    use crate::transport::StatusCode;
    use serde_json::json;

    const A: &str = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa";
    const B: &str = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb";

    fn config() -> GitLabToolkitConfig {
        let settings = json!({
            "gitlab_configuration": {"url": "https://gitlab.example", "private_token": "glpat-fixture"},
            "repository": "group/project",
            "branch": "main",
        });
        GitLabToolkitConfig::parse(settings.as_object().expect("settings")).expect("config")
    }

    fn allow() -> HostAllowlist {
        HostAllowlist::parse(Some("gitlab.example"))
    }

    const TREE: &str = "/api/v4/projects/group%2Fproject/repository/tree";

    #[tokio::test]
    async fn pages_follow_the_next_page_cursor_and_blob_ids_are_versions() {
        let transport = Scripted::new(|request, _| {
            let path = request.url().path();
            if path == TREE {
                assert_eq!(query(request, "ref").as_deref(), Some("main"));
                assert_eq!(query(request, "recursive").as_deref(), Some("true"));
                return match query(request, "page").as_deref() {
                    Some("1") => Reply::json(&json!([
                        {"id": A, "name": "README.md", "type": "blob", "path": "README.md", "mode": "100644"},
                        {"id": B, "name": "src", "type": "tree", "path": "src", "mode": "040000"},
                    ]))
                    .header("x-next-page", "2"),
                    Some("2") => Reply::json(&json!([
                        {"id": B, "name": "lib.rs", "type": "blob", "path": "src/lib.rs", "mode": "100644"},
                        {"id": A, "name": "link", "type": "blob", "path": "link", "mode": "120000"},
                    ]))
                    .header("x-next-page", ""),
                    other => panic!("unexpected page {other:?}"),
                };
            }
            if path == format!("/api/v4/projects/group%2Fproject/repository/blobs/{B}/raw") {
                return Reply::bytes(b"fn main() {}\n");
            }
            panic!("unexpected request {path}");
        });
        let source = GitLabSource::new(config(), allow(), transport.clone()).expect("source");
        let listed = source.list().await.expect("listing");
        let keys: Vec<_> = listed
            .iter()
            .map(|d| (d.key.as_str(), d.version.as_str()))
            .collect();
        assert_eq!(keys, [("README.md", A), ("src/lib.rs", B)]);
        let document = source.fetch("src/lib.rs").await.expect("document");
        assert_eq!(document.bytes, b"fn main() {}\n");
        assert_eq!(document.reference.size, 13);
        assert_eq!(
            document.uri.as_deref(),
            Some("https://gitlab.example/group/project/-/blob/main/src/lib.rs")
        );
        let token = transport.requests()[0]
            .headers()
            .get("private-token")
            .cloned()
            .expect("token header");
        assert!(token.is_sensitive());
    }

    #[tokio::test]
    async fn a_body_over_the_cap_is_refused_by_its_declared_length() {
        let transport = Scripted::new(|request, _| {
            if request.url().path() == TREE {
                return Reply::json(&json!([
                    {"id": A, "type": "blob", "path": "big.bin", "mode": "100644"},
                ]));
            }
            Reply::bytes(b"tiny").declaring(1_000)
        });
        let source = GitLabSource::new(config(), allow(), transport)
            .expect("source")
            .with_limits(SourceLimits {
                max_document_bytes: 100,
                ..SourceLimits::default()
            });
        let refused = source.fetch("big.bin").await.expect_err("refused");
        assert!(
            refused.to_string().contains("larger than 100 bytes"),
            "{refused}"
        );
    }

    #[tokio::test]
    async fn a_listing_that_never_ends_stops_at_the_page_cap() {
        let transport = Scripted::new(|request, _| {
            let page: u32 = query(request, "page")
                .and_then(|p| p.parse().ok())
                .unwrap_or(1);
            Reply::json(&json!([])).header("x-next-page", &(page + 1).to_string())
        });
        let source = GitLabSource::new(config(), allow(), transport.clone())
            .expect("source")
            .with_limits(SourceLimits {
                max_pages: 3,
                ..SourceLimits::default()
            });
        let refused = source.list().await.expect_err("refused");
        assert!(refused.to_string().contains("page cap"), "{refused}");
        assert_eq!(transport.seen().len(), 3);
    }

    #[tokio::test(start_paused = true)]
    async fn a_rate_limit_past_the_bound_fails_as_rate_limited() {
        let transport = Scripted::new(|_, _| Reply::rate_limited(Some("3600")));
        let source = GitLabSource::new(config(), allow(), transport.clone()).expect("source");
        let refused = source.list().await.expect_err("refused");
        assert!(refused.to_string().contains("rate limited"), "{refused}");
        assert_eq!(transport.seen().len(), 1, "an hour is not waited out");

        let transport = Scripted::new(|_, _| Reply::rate_limited(None));
        let source = GitLabSource::new(config(), allow(), transport.clone())
            .expect("source")
            .with_backoff(Backoff {
                max_attempts: 3,
                ..Backoff::default()
            });
        assert!(source.list().await.is_err());
        assert_eq!(transport.seen().len(), 3, "three tries in all");
    }

    #[tokio::test]
    async fn a_missing_branch_is_an_error_not_an_empty_source() {
        let transport = Scripted::new(|_, _| Reply::status(StatusCode::NOT_FOUND));
        let source = GitLabSource::new(config(), allow(), transport).expect("source");
        assert!(source.list().await.is_err());
    }
}

//! One Azure DevOps repository branch as a `ContentSource` (ADR-0030
//! decision 3).
//!
//! * **List.** The branch's root tree id comes from `items?path=/`, then
//!   `trees/{id}?recursive=true` lists every entry with its object id and
//!   size. A `x-ms-continuationtoken` answer header is followed as a cursor
//!   (`continuationToken=`) until Azure DevOps stops sending one.
//! * **Version.** Each file's `objectId` (the git blob id): exact, and free
//!   with the listing.
//! * **Fetch.** `blobs/{objectId}?$format=octetstream`, under the document
//!   cap. A file the listing says is over the cap is refused before it is
//!   requested.
//! * Symbolic links and submodules are not documents.

use std::collections::BTreeMap;
use std::sync::Arc;

use elitea_content_source::{ContentSource, Document, DocumentRef, SourceError};
use serde_json::{Map, Value};

use super::client::{AdoClient, AdoRequest};
use super::config::AdoConnection;
use super::repos::{AdoReposToolkitConfig, AdoRepository};
use crate::egress::HostAllowlist;
use crate::source::{
    Backoff, Failure, Listed, ListingCache, SourceHttp, SourceLimits, title_of, valid_key,
};
use crate::transport::header::ACCEPT;
use crate::transport::{HeaderValue, Request, Transport};

const PROVIDER: &str = "Azure DevOps";
const GIT_API: &str = "7.1";
const CONTINUATION: &str = "x-ms-continuationtoken";
const SYMLINK_MODE: &str = "120000";

/// One repository branch, listed and fetched through the shared client.
pub struct AdoReposSource {
    client: AdoClient,
    http: SourceHttp,
    repository: AdoRepository,
    branch: String,
    cache: ListingCache,
}

impl AdoReposSource {
    /// The configured repository at its active branch, every request
    /// through `allowlist` (fail-closed) and then `transport`.
    ///
    /// # Errors
    ///
    /// The organization's host is not on the allowlist.
    pub fn new(
        config: AdoReposToolkitConfig,
        allowlist: HostAllowlist,
        transport: Arc<dyn Transport>,
    ) -> Result<Self, SourceError> {
        let (connection, repository): (AdoConnection, AdoRepository) = config.into_parts();
        let http = SourceHttp::new(allowlist, transport);
        if !http.permits(connection.organization_url()) {
            return Err(Failure::Refused.into_source_error(PROVIDER, None));
        }
        let branch = repository.active_branch.to_string();
        Ok(Self {
            client: AdoClient::new(connection, http.transport()),
            http,
            repository,
            branch,
            cache: ListingCache::default(),
        })
    }

    /// Index `branch` instead of the active one.
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

    fn build(&self, request: AdoRequest<'_>) -> Result<Request, Failure> {
        self.client
            .build(request)
            .map_err(|error| Failure::Client(error.to_string()))
    }

    /// The branch's root tree id.
    async fn root_tree(&self) -> Result<String, Failure> {
        let segments = [
            "git",
            "repositories",
            self.repository.repository_id.as_ref(),
            "items",
        ];
        let request = self.build(
            AdoRequest::get(&segments, GIT_API)
                .query("path", "/")
                .query("recursionLevel", "None")
                .query("versionDescriptor.versionType", "branch")
                .query("versionDescriptor.version", self.branch.as_str()),
        )?;
        let (_, body) = self.http.json(request).await?;
        // One item, or a one-item collection.
        let item = body
            .get("value")
            .and_then(Value::as_array)
            .and_then(|values| values.first())
            .unwrap_or(&body);
        item.get("objectId")
            .and_then(Value::as_str)
            .filter(|id| valid_object_id(id))
            .map(str::to_ascii_lowercase)
            .ok_or(Failure::InvalidResponse("the root item has no tree id"))
    }

    async fn listing(&self) -> Result<Vec<Listed>, Failure> {
        let tree = self.root_tree().await?;
        let segments = [
            "git",
            "repositories",
            self.repository.repository_id.as_ref(),
            "trees",
            tree.as_str(),
        ];
        let mut listed = Vec::new();
        let mut continuation: Option<String> = None;
        let mut pages = 0;
        loop {
            pages += 1;
            self.http.check_pages(pages)?;
            let request = self.build(
                AdoRequest::get(&segments, GIT_API)
                    .query("recursive", "true")
                    .query_opt("continuationToken", continuation.clone()),
            )?;
            let (headers, body) = self.http.json(request).await?;
            let entries = body
                .get("treeEntries")
                .and_then(Value::as_array)
                .ok_or(Failure::InvalidResponse("a tree answer has no entries"))?;
            for entry in entries {
                let text = |name: &str| entry.get(name).and_then(Value::as_str);
                let (Some(path), Some(kind), Some(id)) = (
                    text("relativePath"),
                    text("gitObjectType"),
                    text("objectId"),
                ) else {
                    return Err(Failure::InvalidResponse("a tree entry is incomplete"));
                };
                if kind != "blob" || text("mode") == Some(SYMLINK_MODE) {
                    continue;
                }
                let path = path.trim_start_matches('/');
                if !valid_key(path) || !valid_object_id(id) {
                    return Err(Failure::InvalidResponse(
                        "a tree entry has an unsafe path or id",
                    ));
                }
                let size = entry.get("size").and_then(Value::as_u64).unwrap_or(0);
                let id = id.to_ascii_lowercase();
                listed.push(Listed::new(path.to_owned(), id.clone(), size, id));
                self.http.check_count(listed.len())?;
            }
            continuation = headers
                .get(CONTINUATION)
                .and_then(|value| value.to_str().ok())
                .map(str::trim)
                .filter(|token| !token.is_empty())
                .map(ToOwned::to_owned);
            if continuation.is_none() {
                return Ok(listed);
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
}

fn valid_object_id(id: &str) -> bool {
    matches!(id.len(), 40 | 64) && id.bytes().all(|byte| byte.is_ascii_hexdigit())
}

impl ContentSource for AdoReposSource {
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
        let segments = [
            "git",
            "repositories",
            self.repository.repository_id.as_ref(),
            "blobs",
            listed.handle.as_str(),
        ];
        let mut request = self
            .build(AdoRequest::get(&segments, GIT_API).query("$format", "octetstream"))
            .map_err(failed)?;
        request
            .headers_mut()
            .insert(ACCEPT, HeaderValue::from_static("application/octet-stream"));
        let bytes = self.http.raw(request).await.map_err(failed)?;
        let mut reference = listed.reference.clone();
        reference.size = bytes.len() as u64;
        let mut metadata = Map::new();
        metadata.insert("object_id".to_owned(), Value::String(listed.handle.clone()));
        metadata.insert("branch".to_owned(), Value::String(self.branch.clone()));
        metadata.insert(
            "repository".to_owned(),
            Value::String(self.repository.repository_id.to_string()),
        );
        let uri = format!(
            "{}/{}/_git/{}?path=/{key}&version=GB{}",
            self.client.organization_text(),
            self.client.project(),
            self.repository.repository_id,
            self.branch
        );
        Ok(Document {
            title: title_of(key),
            uri: Some(uri),
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

    const ROOT: &str = "1111111111111111111111111111111111111111";
    const A: &str = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa";
    const B: &str = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb";

    fn config() -> AdoReposToolkitConfig {
        let settings = json!({
            "ado_configuration": {
                "organization_url": "https://dev.azure.com/contoso/",
                "token": "pat-fixture",
            },
            "project": "Fabrikam",
            "repository_id": "web",
            "active_branch": "main",
        });
        AdoReposToolkitConfig::parse(settings.as_object().expect("settings")).expect("config")
    }

    fn allow() -> HostAllowlist {
        HostAllowlist::parse(Some("dev.azure.com"))
    }

    fn entry(path: &str, kind: &str, id: &str, size: u64) -> Value {
        json!({"relativePath": path, "gitObjectType": kind, "objectId": id, "size": size, "mode": "100644"})
    }

    #[tokio::test]
    async fn trees_list_with_object_ids_and_follow_the_continuation_token() {
        let transport = Scripted::new(|request, _| {
            let path = request.url().path();
            assert_eq!(query(request, "api-version").as_deref(), Some(GIT_API));
            match path {
                "/contoso/Fabrikam/_apis/git/repositories/web/items" => {
                    assert_eq!(
                        query(request, "versionDescriptor.version").as_deref(),
                        Some("main")
                    );
                    Reply::json(
                        &json!({"count": 1, "value": [{"objectId": ROOT, "gitObjectType": "tree", "path": "/"}]}),
                    )
                }
                "/contoso/Fabrikam/_apis/git/repositories/web/trees/1111111111111111111111111111111111111111" => {
                    match query(request, "continuationToken").as_deref() {
                        None => Reply::json(&json!({"treeEntries": [
                            entry("README.md", "blob", A, 5),
                            entry("src", "tree", B, 0),
                        ]}))
                        .header(CONTINUATION, "next-1"),
                        Some("next-1") => Reply::json(&json!({"treeEntries": [
                            entry("src/lib.rs", "blob", B, 9),
                            entry("vendor/x", "commit", B, 0),
                        ]})),
                        other => panic!("unexpected token {other:?}"),
                    }
                }
                "/contoso/Fabrikam/_apis/git/repositories/web/blobs/bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" =>
                {
                    assert_eq!(query(request, "$format").as_deref(), Some("octetstream"));
                    Reply::bytes(b"fn x() {}")
                }
                other => panic!("unexpected request {other}"),
            }
        });
        let source = AdoReposSource::new(config(), allow(), transport.clone()).expect("source");
        let listed = source.list().await.expect("listing");
        let keys: Vec<_> = listed
            .iter()
            .map(|d| (d.key.as_str(), d.version.as_str(), d.size))
            .collect();
        assert_eq!(keys, [("README.md", A, 5), ("src/lib.rs", B, 9)]);
        let document = source.fetch("src/lib.rs").await.expect("document");
        assert_eq!(document.bytes, b"fn x() {}");
        assert_eq!(
            document.uri.as_deref(),
            Some("https://dev.azure.com/contoso/Fabrikam/_git/web?path=/src/lib.rs&version=GBmain")
        );
        let blob = transport.requests().pop().expect("blob request");
        assert_eq!(
            blob.headers().get(ACCEPT).expect("accept"),
            "application/octet-stream"
        );
        assert!(
            blob.headers()
                .get("authorization")
                .expect("credential")
                .is_sensitive()
        );
    }

    #[tokio::test]
    async fn an_oversized_listed_file_is_never_requested() {
        let transport = Scripted::new(|request, _| match request.url().path() {
            "/contoso/Fabrikam/_apis/git/repositories/web/items" => {
                Reply::json(&json!({"objectId": ROOT}))
            }
            "/contoso/Fabrikam/_apis/git/repositories/web/trees/1111111111111111111111111111111111111111" => {
                Reply::json(
                    &json!({"treeEntries": [entry("big.pdf", "blob", A, 60 * 1_024 * 1_024)]}),
                )
            }
            other => panic!("unexpected request {other}"),
        });
        let source = AdoReposSource::new(config(), allow(), transport.clone()).expect("source");
        let refused = source.fetch("big.pdf").await.expect_err("refused");
        assert!(refused.to_string().contains("larger than"), "{refused}");
        assert_eq!(transport.seen().len(), 2, "only the listing was requested");
    }
}

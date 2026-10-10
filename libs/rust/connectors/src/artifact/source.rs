//! A folder of the invoking project's artifact bucket as a `ContentSource`
//! (ADR-0030 decision 3).
//!
//! * **List.** The object API, 1 page at a time, following `next_cursor`
//!   until it is absent. A 404 on the first page is an unknown bucket and
//!   lists as empty; a 404 after a cursor leaves the listing partial and is
//!   an error (as `elitea-repo-ingest`'s folder ingest decides).
//! * **Keys.** The object key relative to the folder, held to elitea-main's
//!   key rules and to the folder it was listed under; one bad key refuses
//!   the listing. A key ending in `/` is a folder marker, not a document.
//! * **Version.** The object's `etag` when the platform sends one, else its
//!   `modified_at`. An object with neither is refused: a version that never
//!   changes would hide every later change.
//! * **Fetch.** The object's bytes, under the document cap.
//! * **Egress.** The platform host passes the same allowlist as every
//!   connector (fail-closed); a deployment names its platform host on it.

use std::collections::BTreeMap;
use std::sync::Arc;

use elitea_content_source::{ContentSource, Document, DocumentRef, SourceError};
use serde_json::{Map, Value};

use super::client::{ArtifactPlatform, valid_object_key, valid_segment};
use crate::egress::HostAllowlist;
use crate::source::{Backoff, Failure, Listed, ListingCache, SourceHttp, SourceLimits, title_of};
use crate::transport::{Request, StatusCode, Transport};

const PROVIDER: &str = "the artifact store";

/// One bucket folder, listed and fetched through the platform object API.
pub struct ArtifactSource {
    platform: ArtifactPlatform,
    bucket: String,
    /// `""` for the whole bucket, else `folder/` (with its slash).
    list_prefix: String,
    http: SourceHttp,
    cache: ListingCache,
}

impl ArtifactSource {
    /// `bucket` (and, when `prefix` is not empty, its folder `prefix`) of the
    /// platform's invoking project, every request through `allowlist`
    /// (fail-closed) and then `transport`.
    ///
    /// # Errors
    ///
    /// The platform host is not on the allowlist, the bucket is not one
    /// segment, or the folder is not a valid key.
    pub fn new(
        platform: ArtifactPlatform,
        bucket: &str,
        prefix: &str,
        allowlist: HostAllowlist,
        transport: Arc<dyn Transport>,
    ) -> Result<Self, SourceError> {
        let http = SourceHttp::new(allowlist, transport);
        if !http.permits(platform.base()) {
            return Err(Failure::Refused.into_source_error(PROVIDER, None));
        }
        if !valid_segment(bucket) {
            return Err(SourceError::Unavailable(
                "the artifact bucket name is not a single path segment".to_owned(),
            ));
        }
        let folder = prefix.trim().trim_matches('/');
        if !folder.is_empty() && !valid_object_key(folder) {
            return Err(SourceError::Unavailable(
                "the artifact folder is not a valid object key".to_owned(),
            ));
        }
        Ok(Self {
            platform,
            bucket: bucket.to_owned(),
            list_prefix: if folder.is_empty() {
                String::new()
            } else {
                format!("{folder}/")
            },
            http,
            cache: ListingCache::default(),
        })
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

    fn request(&self, key: Option<&str>, query: &[(&str, &str)]) -> Result<Request, Failure> {
        let mut url = self.platform.objects_url(&self.bucket, key);
        if !query.is_empty() {
            url.query_pairs_mut().extend_pairs(query.iter().copied());
        }
        self.platform
            .get(url)
            .map_err(|error| Failure::Client(error.to_string()))
    }

    async fn listing(&self) -> Result<Vec<Listed>, Failure> {
        let mut listed = Vec::new();
        let mut cursor = String::new();
        let mut pages = 0;
        loop {
            pages += 1;
            self.http.check_pages(pages)?;
            let mut query = Vec::new();
            if !self.list_prefix.is_empty() {
                query.push(("prefix", self.list_prefix.as_str()));
            }
            if !cursor.is_empty() {
                query.push(("cursor", cursor.as_str()));
            }
            let request = self.request(None, &query)?;
            let page = match self.http.json(request).await {
                Ok((_, page)) => page,
                // An unknown bucket holds nothing; only the first page can
                // say so.
                Err(Failure::Status(StatusCode::NOT_FOUND)) if cursor.is_empty() => {
                    return Ok(listed);
                }
                Err(failure) => return Err(failure),
            };
            let objects = match page.get("objects") {
                None | Some(Value::Null) => &[][..],
                Some(Value::Array(objects)) => objects.as_slice(),
                Some(_) => return Err(Failure::InvalidResponse("objects is not a list")),
            };
            for object in objects {
                if let Some(item) = self.object(object)? {
                    listed.push(item);
                    self.http.check_count(listed.len())?;
                }
            }
            match page.get("next_cursor") {
                None | Some(Value::Null) => return Ok(listed),
                Some(Value::String(next)) if next.is_empty() => return Ok(listed),
                Some(Value::String(next)) => cursor.clone_from(next),
                Some(_) => return Err(Failure::InvalidResponse("next_cursor is not a string")),
            }
        }
    }

    /// One listed object as a document, `None` for a folder marker.
    fn object(&self, object: &Value) -> Result<Option<Listed>, Failure> {
        let key = object
            .get("key")
            .and_then(Value::as_str)
            .ok_or(Failure::InvalidResponse("an object has no key"))?;
        if key.ends_with('/') {
            return Ok(None);
        }
        let relative = key
            .strip_prefix(self.list_prefix.as_str())
            .filter(|relative| !relative.is_empty())
            .ok_or(Failure::InvalidResponse(
                "an object is outside the listed folder",
            ))?;
        if !valid_object_key(key) {
            return Err(Failure::InvalidResponse(
                "an object key breaks the platform's key rules",
            ));
        }
        let size = match object.get("size_bytes") {
            Some(Value::Number(number)) => number.as_u64().unwrap_or(0),
            Some(Value::String(text)) => text.trim().parse().unwrap_or(0),
            _ => 0,
        };
        let text = |name: &str| {
            object
                .get(name)
                .and_then(Value::as_str)
                .map(str::trim)
                .filter(|value| !value.is_empty())
        };
        let version =
            text("etag")
                .or_else(|| text("modified_at"))
                .ok_or(Failure::InvalidResponse(
                    "an object has neither an etag nor a modified time",
                ))?;
        Ok(Some(Listed::new(
            relative.to_owned(),
            version.to_owned(),
            size,
            key.to_owned(),
        )))
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

impl ContentSource for ArtifactSource {
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
        let request = self.request(Some(&listed.handle), &[]).map_err(failed)?;
        let bytes = self.http.raw(request).await.map_err(failed)?;
        let mut reference = listed.reference.clone();
        reference.size = bytes.len() as u64;
        let mut metadata = Map::new();
        metadata.insert("bucket".to_owned(), Value::String(self.bucket.clone()));
        metadata.insert(
            "object_key".to_owned(),
            Value::String(listed.handle.clone()),
        );
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

    fn platform() -> ArtifactPlatform {
        ArtifactPlatform::new("https://platform.example", "bearer-fixture", "7").expect("platform")
    }

    fn allow() -> HostAllowlist {
        HostAllowlist::parse(Some("platform.example"))
    }

    const LIST: &str = "/api/v2/artifacts/objects/7/docs";

    #[tokio::test]
    async fn the_cursor_is_followed_and_etag_or_modified_time_is_the_version() {
        let transport = Scripted::new(|request, _| {
            let path = request.url().path();
            if path == LIST {
                assert_eq!(query(request, "prefix").as_deref(), Some("guides/"));
                return match query(request, "cursor").as_deref() {
                    None => Reply::json(&json!({
                        "objects": [
                            {"key": "guides/a.md", "size_bytes": 5, "modified_at": "2026-10-01T00:00:00Z", "etag": "\"e1\""},
                            {"key": "guides/sub/", "size_bytes": 0, "modified_at": "2026-10-01T00:00:00Z"},
                        ],
                        "next_cursor": "c2",
                    })),
                    Some("c2") => Reply::json(&json!({"objects": [
                        {"key": "guides/sub/b.pdf", "size_bytes": "9", "modified_at": "2026-10-02T00:00:00Z"},
                    ]})),
                    other => panic!("unexpected cursor {other:?}"),
                };
            }
            if path == "/api/v2/artifacts/objects/7/docs/guides/sub/b.pdf" {
                return Reply::bytes(b"%PDF-1.7\n");
            }
            panic!("unexpected request {path}");
        });
        let source =
            ArtifactSource::new(platform(), "docs", "/guides/", allow(), transport.clone())
                .expect("source");
        let listed = source.list().await.expect("listing");
        let keys: Vec<_> = listed
            .iter()
            .map(|d| (d.key.as_str(), d.version.as_str(), d.size, d.mime.as_str()))
            .collect();
        assert_eq!(
            keys,
            [
                ("a.md", "\"e1\"", 5, "text/markdown"),
                ("sub/b.pdf", "2026-10-02T00:00:00Z", 9, "application/pdf"),
            ]
        );
        let document = source.fetch("sub/b.pdf").await.expect("document");
        assert_eq!(document.bytes, b"%PDF-1.7\n");
        assert_eq!(document.title, "b.pdf");
        let bearer = transport.requests()[0]
            .headers()
            .get("authorization")
            .cloned()
            .expect("bearer");
        assert!(bearer.is_sensitive());
    }

    #[tokio::test]
    async fn an_unknown_bucket_is_empty_but_a_later_404_is_an_error() {
        let transport = Scripted::new(|_, _| Reply::status(StatusCode::NOT_FOUND));
        let source =
            ArtifactSource::new(platform(), "docs", "", allow(), transport).expect("source");
        assert_eq!(source.list().await.expect("empty"), []);

        let transport = Scripted::new(|request, _| match query(request, "cursor") {
            None => Reply::json(&json!({"objects": [], "next_cursor": "c2"})),
            Some(_) => Reply::status(StatusCode::NOT_FOUND),
        });
        let source =
            ArtifactSource::new(platform(), "docs", "", allow(), transport).expect("source");
        assert!(source.list().await.is_err());
    }

    #[tokio::test]
    async fn unsafe_or_unversioned_objects_refuse_the_listing() {
        for object in [
            json!({"key": "guides/../x.md", "size_bytes": 1, "modified_at": "t"}),
            json!({"key": "other/x.md", "size_bytes": 1, "modified_at": "t"}),
            json!({"key": "guides/x.md", "size_bytes": 1}),
        ] {
            let transport =
                Scripted::new(move |_, _| Reply::json(&json!({"objects": [object.clone()]})));
            let source = ArtifactSource::new(platform(), "docs", "guides", allow(), transport)
                .expect("source");
            assert!(source.list().await.is_err());
        }
    }

    #[tokio::test]
    async fn the_platform_host_passes_the_allowlist_too() {
        let transport = Scripted::new(|_, _| panic!("no request may be sent"));
        let refused = ArtifactSource::new(
            platform(),
            "docs",
            "",
            HostAllowlist::parse(Some("github.com")),
            transport,
        );
        assert!(refused.is_err());
    }
}

//! A folder of the invoking project's artifact bucket as a `ContentSource`
//! (ADR-0030 decision 3).
//!
//! * **List.** The object API, 1 page at a time, following `next_cursor`
//!   until it is absent. A 404 (on the first page: an unknown or unreadable
//!   bucket; after a cursor: a listing left partial) is an error naming the
//!   bucket. Only a successful answer with no objects is an empty listing,
//!   so an index run never reads a misconfigured bucket as "everything was
//!   deleted".
//! * **Keys.** The object key relative to the folder, held to elitea-main's
//!   key rules and to the folder it was listed under; one bad key refuses
//!   the listing. A key ending in `/` is a folder marker, not a document.
//! * **Version.** The object's `etag` when the platform sends one, else its
//!   `modified_at`. An object with neither is refused: a version that never
//!   changes would hide every later change.
//! * **Fetch.** The object's bytes, under the document cap, pinned to the
//!   listed version. A listed `etag` goes out as `If-Match` (a 412 is
//!   `SourceError::Changed`); elitea-main's download handler does not honour
//!   `If-Match` today but answers with the object's current `ETag` and
//!   `Last-Modified`, so the response is checked against the listed
//!   `etag` (or `modified_at`) too, and a mismatch is `Changed` rather than
//!   newer bytes handed back under the old version. A response that
//!   carries nothing to compare is refused.
//! * **Egress.** The platform host passes the same allowlist as every
//!   connector (fail-closed); a deployment names its platform host on it.

use std::collections::BTreeMap;
use std::sync::Arc;

use elitea_content_source::{ContentSource, Document, DocumentRef, SourceError};
use serde_json::{Map, Value};

use super::client::{ArtifactPlatform, valid_object_key, valid_segment};
use crate::egress::HostAllowlist;
use crate::source::{
    Backoff, Failure, Listed, ListingCache, SourceHttp, SourceLimits, Timeouts, VersionKind,
    references, title_of,
};
use crate::transport::header::{ETAG, IF_MATCH, LAST_MODIFIED};
use crate::transport::{HeaderMap, HeaderValue, Request, StatusCode, Transport};

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

    /// Connect and read-idle timeouts for this source's requests (default:
    /// 30 s to a response, 60 s without a body byte).
    #[must_use]
    pub fn with_timeouts(mut self, timeouts: Timeouts) -> Self {
        self.http = self.http.with_timeouts(timeouts);
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
                // Never an empty listing: a missing bucket (or one this
                // project cannot read) would otherwise look like every
                // document having been deleted.
                Err(Failure::Status(StatusCode::NOT_FOUND)) => {
                    return Err(Failure::Client(format!(
                        "the bucket '{}' was not found or is not readable{}",
                        self.bucket,
                        if cursor.is_empty() {
                            ""
                        } else {
                            " (it vanished mid-listing)"
                        }
                    )));
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
        let (version, kind) = match (text("etag"), text("modified_at")) {
            (Some(etag), _) => (etag, VersionKind::ETag),
            (None, Some(modified)) => (modified, VersionKind::ModifiedAt),
            (None, None) => {
                return Err(Failure::InvalidResponse(
                    "an object has neither an etag nor a modified time",
                ));
            }
        };
        Ok(Some(
            Listed::new(
                relative.to_owned(),
                version.to_owned(),
                size,
                key.to_owned(),
            )
            .versioned_by(kind),
        ))
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

    /// A listing made now (or the one already running), replacing the
    /// remembered one: what `list()` returns.
    async fn listed_fresh(&self) -> Result<Arc<BTreeMap<String, Listed>>, SourceError> {
        self.cache
            .list_shared(|| async {
                self.listing()
                    .await
                    .map_err(|failure| failure.into_source_error(PROVIDER, None))
            })
            .await
    }
}

impl ContentSource for ArtifactSource {
    async fn list(&self) -> Result<Vec<DocumentRef>, SourceError> {
        let listing = self.listed_fresh().await?;
        Ok(references(&listing))
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
        let mut request = self.request(Some(&listed.handle), &[]).map_err(failed)?;
        if listed.kind == VersionKind::ETag {
            let etag = HeaderValue::from_str(&listed.reference.version).map_err(|_| {
                failed(Failure::InvalidResponse(
                    "a listed etag is not a header value",
                ))
            })?;
            request.headers_mut().insert(IF_MATCH, etag);
        }
        let (headers, bytes) = self.http.raw_with_headers(request).await.map_err(failed)?;
        pinned_to(listed, &headers).map_err(failed)?;
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

/// Whether the response is the listed version: its `ETag` equals the
/// listed one, or its `Last-Modified` is the listed `modified_at` (to the
/// second, the HTTP date's resolution).
fn pinned_to(listed: &Listed, headers: &HeaderMap) -> Result<(), Failure> {
    let header = |name| headers.get(name).and_then(|value| value.to_str().ok());
    match listed.kind {
        VersionKind::Handle => Ok(()),
        VersionKind::ETag => match header(ETAG) {
            Some(etag) if etag.trim() == listed.reference.version => Ok(()),
            Some(_) => Err(Failure::Changed),
            None => Err(Failure::InvalidResponse(
                "the object answer has no ETag to pin the fetch to",
            )),
        },
        VersionKind::ModifiedAt => {
            let sent = header(LAST_MODIFIED)
                .and_then(|value| chrono::DateTime::parse_from_rfc2822(value.trim()).ok())
                .ok_or(Failure::InvalidResponse(
                    "the object answer has no Last-Modified to pin the fetch to",
                ))?;
            let listed_at = chrono::DateTime::parse_from_rfc3339(&listed.reference.version)
                .map_err(|_| {
                    Failure::InvalidResponse("a listed modified time is not an RFC 3339 date")
                })?;
            if sent.timestamp() == listed_at.timestamp() {
                Ok(())
            } else {
                Err(Failure::Changed)
            }
        }
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
                return Reply::bytes(b"%PDF-1.7\n")
                    .header("last-modified", "Fri, 02 Oct 2026 00:00:00 GMT");
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
    async fn a_404_is_an_error_never_an_empty_listing() {
        let transport = Scripted::new(|_, _| Reply::status(StatusCode::NOT_FOUND));
        let source =
            ArtifactSource::new(platform(), "docs", "", allow(), transport).expect("source");
        let error = source
            .list()
            .await
            .expect_err("an unknown bucket is not empty");
        assert!(
            error.to_string().contains("bucket 'docs' was not found"),
            "{error}"
        );

        // Only a successful answer with no objects is an empty listing.
        let transport = Scripted::new(|_, _| Reply::json(&json!({"objects": []})));
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

    /// A source over one object, listed with `version_field` and fetched
    /// through `fetch_reply`.
    async fn pinned(
        object: Value,
        fetch_reply: impl Fn(&Request) -> Reply + Send + Sync + 'static,
    ) -> (Arc<Scripted>, Result<Document, SourceError>) {
        let transport = Scripted::new(move |request, _| {
            if request.url().path() == LIST {
                Reply::json(&json!({"objects": [object.clone()]}))
            } else {
                fetch_reply(request)
            }
        });
        let source = ArtifactSource::new(platform(), "docs", "", allow(), transport.clone())
            .expect("source");
        source.list().await.expect("listing");
        let fetched = source.fetch("a.md").await;
        (transport, fetched)
    }

    fn etag_object() -> Value {
        json!({"key": "a.md", "size_bytes": 2, "modified_at": "2026-10-01T00:00:00Z", "etag": "\"v1\""})
    }

    #[tokio::test]
    async fn a_fetch_is_pinned_to_the_listed_etag() {
        // Matching ETag: the bytes, with If-Match on the request.
        let (transport, fetched) = pinned(etag_object(), |_| {
            Reply::bytes(b"v1").header("etag", "\"v1\"")
        })
        .await;
        assert_eq!(fetched.expect("document").bytes, b"v1");
        let sent = transport.requests().pop().expect("fetch request");
        assert_eq!(sent.headers().get("if-match").expect("if-match"), "\"v1\"");

        // A platform that honours If-Match answers 412.
        let (_, fetched) = pinned(etag_object(), |_| {
            Reply::status(StatusCode::PRECONDITION_FAILED)
        })
        .await;
        assert!(matches!(fetched, Err(SourceError::Changed(key)) if key == "a.md"));

        // One that ignores it (elitea-main today) answers the newer bytes
        // with the newer ETag: refused, not returned.
        let (_, fetched) = pinned(etag_object(), |_| {
            Reply::bytes(b"v2").header("etag", "\"v2\"")
        })
        .await;
        assert!(
            matches!(fetched, Err(SourceError::Changed(_))),
            "{fetched:?}"
        );

        // No ETag to compare: refused, and not as Changed (re-listing would
        // not help).
        let (_, fetched) = pinned(etag_object(), |_| Reply::bytes(b"v1")).await;
        let error = fetched.expect_err("unverifiable");
        assert!(matches!(error, SourceError::Unavailable(_)), "{error}");
    }

    #[tokio::test]
    async fn a_fetch_without_an_etag_is_pinned_to_the_modified_time() {
        let object =
            json!({"key": "a.md", "size_bytes": 2, "modified_at": "2026-10-01T00:00:00.250Z"});
        let (transport, fetched) = pinned(object.clone(), |_| {
            Reply::bytes(b"v1").header("last-modified", "Thu, 01 Oct 2026 00:00:00 GMT")
        })
        .await;
        assert_eq!(fetched.expect("document").bytes, b"v1");
        let sent = transport.requests().pop().expect("fetch request");
        assert!(sent.headers().get("if-match").is_none(), "no etag to match");

        let (_, fetched) = pinned(object.clone(), |_| {
            Reply::bytes(b"v2").header("last-modified", "Thu, 01 Oct 2026 00:05:00 GMT")
        })
        .await;
        assert!(
            matches!(fetched, Err(SourceError::Changed(_))),
            "{fetched:?}"
        );

        let (_, fetched) = pinned(object, |_| Reply::bytes(b"v1")).await;
        assert!(matches!(fetched, Err(SourceError::Unavailable(_))));
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

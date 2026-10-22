//! A folder of the invoking project's artifact bucket as a `ContentSource`
//! (ADR-0030 decision 3).
//!
//! * **Store.** The host reaches elitea-main's `ArtifactObjectService` on
//!   its platform channel and implements [`ArtifactObjects`] over it; a
//!   refusal comes back as [`Failure::Status`] with the HTTP status the
//!   service carried, the hop failing as [`Failure::Transport`]. Nothing
//!   else is reachable through this source.
//! * **List.** One page at a time, following `next_cursor` until it is
//!   empty. A 404 (on the first page: an unknown or unreadable bucket;
//!   after a cursor: a listing left partial) is an error naming the bucket.
//!   Only a successful answer with no objects is an empty listing, so an
//!   index run never reads a misconfigured bucket as "everything was
//!   deleted".
//! * **Keys.** The object key relative to the folder, held to elitea-main's
//!   key rules and to the folder it was listed under; one bad key refuses
//!   the listing. A key ending in `/` is a folder marker, not a document.
//! * **Version.** The object's `etag` when the store sends one, else its
//!   `modified_at`. An object with neither is refused: a version that never
//!   changes would hide every later change.
//! * **Fetch.** The object's bytes, under the document cap, pinned to the
//!   listed version: the object the store answers with must carry the
//!   listed `etag` (or `modified_at`, to the second), and a mismatch is
//!   `SourceError::Changed` rather than newer bytes handed back under the
//!   old version. An answer that carries nothing to compare is refused.

use std::collections::BTreeMap;
use std::sync::Arc;

use elitea_content_source::{ContentSource, Document, DocumentRef, SourceError};
use serde_json::{Map, Value};

use super::client::{valid_object_key, valid_segment};
use crate::source::{
    Failure, Listed, ListingCache, SourceLimits, VersionKind, references, title_of,
};

const PROVIDER: &str = "the artifact store";

/// One listed object as the store describes it.
#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct ObjectEntry {
    pub key: String,
    pub size_bytes: u64,
    /// Empty when the store sent none.
    pub etag: String,
    /// RFC 3339; empty when the store sent none.
    pub modified_at: String,
}

/// One listing page.
#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct ObjectPage {
    pub objects: Vec<ObjectEntry>,
    /// Empty on the last page.
    pub next_cursor: String,
}

/// One object read: what the store says it is, and its bytes.
#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct FetchedObject {
    pub etag: String,
    pub modified_at: String,
    pub bytes: Vec<u8>,
}

/// The invoking project's object store, as the host reaches it (the
/// bearer and project are the host's; a bucket outside that project is
/// unreachable).
#[async_trait::async_trait]
pub trait ArtifactObjects: Send + Sync {
    /// One page of `bucket` under `prefix`, resuming at `cursor` (empty:
    /// the first page).
    ///
    /// # Errors
    ///
    /// A refusal as [`Failure::Status`] (404: no such bucket), the hop as
    /// [`Failure::Transport`].
    async fn list_page(
        &self,
        bucket: &str,
        prefix: &str,
        cursor: &str,
    ) -> Result<ObjectPage, Failure>;

    /// The object `key` of `bucket`, refused past `max_bytes`.
    ///
    /// # Errors
    ///
    /// A refusal as [`Failure::Status`], the hop as [`Failure::Transport`],
    /// a larger object as [`Failure::TooLarge`].
    async fn fetch(
        &self,
        bucket: &str,
        key: &str,
        max_bytes: u64,
    ) -> Result<FetchedObject, Failure>;
}

/// One bucket folder, listed and fetched through the host's object store.
pub struct ArtifactSource {
    objects: Arc<dyn ArtifactObjects>,
    bucket: String,
    /// `""` for the whole bucket, else `folder/` (with its slash).
    list_prefix: String,
    limits: SourceLimits,
    cache: ListingCache,
}

impl ArtifactSource {
    /// `bucket` (and, when `prefix` is not empty, its folder `prefix`) of
    /// the invoking project, read through `objects`.
    ///
    /// # Errors
    ///
    /// The bucket is not one segment, or the folder is not a valid key.
    pub fn new(
        objects: Arc<dyn ArtifactObjects>,
        bucket: &str,
        prefix: &str,
    ) -> Result<Self, SourceError> {
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
            objects,
            bucket: bucket.to_owned(),
            list_prefix: if folder.is_empty() {
                String::new()
            } else {
                format!("{folder}/")
            },
            limits: SourceLimits::default(),
            cache: ListingCache::default(),
        })
    }

    #[must_use]
    pub fn with_limits(mut self, limits: SourceLimits) -> Self {
        self.limits = limits;
        self
    }

    async fn listing(&self) -> Result<Vec<Listed>, Failure> {
        let mut listed = Vec::new();
        let mut cursor = String::new();
        let mut pages = 0;
        loop {
            pages += 1;
            self.limits.check_pages(pages)?;
            let page = match self
                .objects
                .list_page(&self.bucket, &self.list_prefix, &cursor)
                .await
            {
                Ok(page) => page,
                // Never an empty listing: a missing bucket (or one this
                // project cannot read) would otherwise look like every
                // document having been deleted.
                Err(Failure::Status(status)) if status.as_u16() == 404 => {
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
            for object in &page.objects {
                if let Some(item) = self.object(object)? {
                    listed.push(item);
                    self.limits.check_count(listed.len())?;
                }
            }
            if page.next_cursor.is_empty() {
                return Ok(listed);
            }
            if page.next_cursor == cursor {
                return Err(Failure::InvalidResponse("the listing repeated its cursor"));
            }
            cursor = page.next_cursor;
        }
    }

    /// One listed object as a document, `None` for a folder marker.
    fn object(&self, object: &ObjectEntry) -> Result<Option<Listed>, Failure> {
        let key = object.key.as_str();
        if key.is_empty() {
            return Err(Failure::InvalidResponse("an object has no key"));
        }
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
        let (version, kind) = match (present(&object.etag), present(&object.modified_at)) {
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
                object.size_bytes,
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
        self.limits
            .check_size(listed.reference.size)
            .map_err(failed)?;
        let fetched = self
            .objects
            .fetch(&self.bucket, &listed.handle, self.limits.max_document_bytes)
            .await
            .map_err(failed)?;
        pinned_to(listed, &fetched).map_err(failed)?;
        let mut reference = listed.reference.clone();
        reference.size = fetched.bytes.len() as u64;
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
            bytes: fetched.bytes,
            metadata,
        })
    }
}

/// Whether the fetched object is the listed version: its `etag` equals the
/// listed one, or its `modified_at` is the listed one (to the second).
fn pinned_to(listed: &Listed, fetched: &FetchedObject) -> Result<(), Failure> {
    match listed.kind {
        VersionKind::Handle => Ok(()),
        VersionKind::ETag => match present(&fetched.etag) {
            Some(etag) if etag == listed.reference.version => Ok(()),
            Some(_) => Err(Failure::Changed),
            None => Err(Failure::InvalidResponse(
                "the object answer has no etag to pin the fetch to",
            )),
        },
        VersionKind::ModifiedAt => {
            let instant = |value: &str| {
                chrono::DateTime::parse_from_rfc3339(value).map(|time| time.timestamp())
            };
            let sent = present(&fetched.modified_at)
                .and_then(|value| instant(value).ok())
                .ok_or(Failure::InvalidResponse(
                    "the object answer has no modified time to pin the fetch to",
                ))?;
            let listed_at = instant(&listed.reference.version).map_err(|_| {
                Failure::InvalidResponse("a listed modified time is not an RFC 3339 date")
            })?;
            if sent == listed_at {
                Ok(())
            } else {
                Err(Failure::Changed)
            }
        }
    }
}

/// `value` trimmed, `None` when that is empty (the store's "not sent").
fn present(value: &str) -> Option<&str> {
    Some(value.trim()).filter(|value| !value.is_empty())
}

#[cfg(test)]
mod tests {
    use std::sync::Mutex;

    use super::*;
    use crate::transport::StatusCode;

    /// A store scripted by bucket listing pages (by cursor) and objects.
    #[derive(Default)]
    struct Store {
        pages: BTreeMap<String, Result<ObjectPage, u16>>,
        objects: BTreeMap<String, FetchedObject>,
        listed: Mutex<Vec<(String, String, String)>>,
    }

    impl Store {
        fn page(mut self, cursor: &str, page: Result<ObjectPage, u16>) -> Self {
            self.pages.insert(cursor.to_owned(), page);
            self
        }

        fn with(mut self, key: &str, etag: &str, modified_at: &str, bytes: &[u8]) -> Self {
            self.objects.insert(
                key.to_owned(),
                FetchedObject {
                    etag: etag.to_owned(),
                    modified_at: modified_at.to_owned(),
                    bytes: bytes.to_vec(),
                },
            );
            self
        }
    }

    #[async_trait::async_trait]
    impl ArtifactObjects for Store {
        async fn list_page(
            &self,
            bucket: &str,
            prefix: &str,
            cursor: &str,
        ) -> Result<ObjectPage, Failure> {
            self.listed.lock().expect("lock").push((
                bucket.to_owned(),
                prefix.to_owned(),
                cursor.to_owned(),
            ));
            match self.pages.get(cursor) {
                Some(Ok(page)) => Ok(page.clone()),
                Some(Err(status)) => Err(Failure::Status(
                    StatusCode::from_u16(*status).expect("status"),
                )),
                None => panic!("unexpected cursor {cursor:?}"),
            }
        }

        async fn fetch(
            &self,
            _bucket: &str,
            key: &str,
            max_bytes: u64,
        ) -> Result<FetchedObject, Failure> {
            let object = self
                .objects
                .get(key)
                .cloned()
                .ok_or(Failure::Status(StatusCode::NOT_FOUND))?;
            if object.bytes.len() as u64 > max_bytes {
                return Err(Failure::TooLarge(max_bytes));
            }
            Ok(object)
        }
    }

    fn entry(key: &str, size: u64, etag: &str, modified_at: &str) -> ObjectEntry {
        ObjectEntry {
            key: key.to_owned(),
            size_bytes: size,
            etag: etag.to_owned(),
            modified_at: modified_at.to_owned(),
        }
    }

    #[allow(clippy::unnecessary_wraps)] // The shape `Store::page` takes.
    fn page(objects: Vec<ObjectEntry>, next_cursor: &str) -> Result<ObjectPage, u16> {
        Ok(ObjectPage {
            objects,
            next_cursor: next_cursor.to_owned(),
        })
    }

    #[tokio::test]
    async fn the_cursor_is_followed_and_etag_or_modified_time_is_the_version() {
        let store = Arc::new(
            Store::default()
                .page(
                    "",
                    page(
                        vec![
                            entry("guides/a.md", 5, "\"e1\"", "2026-10-01T00:00:00Z"),
                            entry("guides/sub/", 0, "", "2026-10-01T00:00:00Z"),
                        ],
                        "c2",
                    ),
                )
                .page(
                    "c2",
                    page(
                        vec![entry("guides/sub/b.pdf", 9, "", "2026-10-02T00:00:00Z")],
                        "",
                    ),
                )
                .with(
                    "guides/sub/b.pdf",
                    "",
                    "2026-10-02T00:00:00.400Z",
                    b"%PDF-1.7\n",
                ),
        );
        let source = ArtifactSource::new(store.clone(), "docs", "/guides/").expect("source");
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
        assert_eq!(
            store.listed.lock().expect("lock")[1],
            ("docs".to_owned(), "guides/".to_owned(), "c2".to_owned())
        );
    }

    #[tokio::test]
    async fn a_404_is_an_error_never_an_empty_listing() {
        let store = Arc::new(Store::default().page("", Err(404)));
        let source = ArtifactSource::new(store, "docs", "").expect("source");
        let error = source
            .list()
            .await
            .expect_err("an unknown bucket is not empty");
        assert!(
            error.to_string().contains("bucket 'docs' was not found"),
            "{error}"
        );

        // Only a successful answer with no objects is an empty listing.
        let store = Arc::new(Store::default().page("", page(Vec::new(), "")));
        let source = ArtifactSource::new(store, "docs", "").expect("source");
        assert_eq!(source.list().await.expect("empty"), []);

        let store = Arc::new(
            Store::default()
                .page("", page(Vec::new(), "c2"))
                .page("c2", Err(404)),
        );
        let source = ArtifactSource::new(store, "docs", "").expect("source");
        let error = source.list().await.expect_err("partial");
        assert!(error.to_string().contains("mid-listing"), "{error}");

        // Another refusal names its status.
        let store = Arc::new(Store::default().page("", Err(403)));
        let source = ArtifactSource::new(store, "docs", "").expect("source");
        let error = source.list().await.expect_err("refused");
        assert!(error.to_string().contains("HTTP 403"), "{error}");
    }

    /// One object listed as `listed`, fetched as `fetched`.
    async fn pinned(listed: ObjectEntry, fetched: FetchedObject) -> Result<Document, SourceError> {
        let mut store = Store::default().page("", page(vec![listed.clone()], ""));
        store.objects.insert(listed.key, fetched);
        let source = ArtifactSource::new(Arc::new(store), "docs", "").expect("source");
        source.list().await.expect("listing");
        source.fetch("a.md").await
    }

    fn fetched(etag: &str, modified_at: &str, bytes: &[u8]) -> FetchedObject {
        FetchedObject {
            etag: etag.to_owned(),
            modified_at: modified_at.to_owned(),
            bytes: bytes.to_vec(),
        }
    }

    #[tokio::test]
    async fn a_fetch_is_pinned_to_the_listed_etag() {
        let listed = entry("a.md", 2, "\"v1\"", "2026-10-01T00:00:00Z");
        let document = pinned(listed.clone(), fetched("\"v1\"", "", b"v1"))
            .await
            .expect("document");
        assert_eq!(document.bytes, b"v1");

        // Newer bytes under a newer etag: refused, not returned.
        let changed = pinned(listed.clone(), fetched("\"v2\"", "", b"v2")).await;
        assert!(
            matches!(&changed, Err(SourceError::Changed(key)) if key == "a.md"),
            "{changed:?}"
        );

        // No etag to compare: refused, and not as Changed (re-listing would
        // not help).
        let error = pinned(listed, fetched("", "", b"v1"))
            .await
            .expect_err("unverifiable");
        assert!(matches!(error, SourceError::Unavailable(_)), "{error}");
    }

    #[tokio::test]
    async fn a_fetch_without_an_etag_is_pinned_to_the_modified_time() {
        let listed = entry("a.md", 2, "", "2026-10-01T00:00:00.250Z");
        let document = pinned(listed.clone(), fetched("", "2026-10-01T00:00:00Z", b"v1"))
            .await
            .expect("document");
        assert_eq!(document.bytes, b"v1");

        let changed = pinned(listed.clone(), fetched("", "2026-10-01T00:05:00Z", b"v2")).await;
        assert!(
            matches!(changed, Err(SourceError::Changed(_))),
            "{changed:?}"
        );

        let error = pinned(listed, fetched("", "", b"v1")).await;
        assert!(matches!(error, Err(SourceError::Unavailable(_))));
    }

    #[tokio::test]
    async fn a_vanished_or_oversized_object_is_reported_by_key() {
        let listed = entry("a.md", 2, "\"v1\"", "");
        let store = Store::default().page("", page(vec![listed], ""));
        let source = ArtifactSource::new(Arc::new(store), "docs", "").expect("source");
        source.list().await.expect("listing");
        assert!(matches!(
            source.fetch("a.md").await,
            Err(SourceError::NotFound(key)) if key == "a.md"
        ));

        let listed = entry("a.md", 2, "\"v1\"", "");
        let store = Store::default().page("", page(vec![listed], "")).with(
            "a.md",
            "\"v1\"",
            "",
            b"0123456789",
        );
        let source = ArtifactSource::new(Arc::new(store), "docs", "")
            .expect("source")
            .with_limits(SourceLimits {
                max_document_bytes: 4,
                ..SourceLimits::default()
            });
        source.list().await.expect("listing");
        let error = source.fetch("a.md").await.expect_err("too large");
        assert!(error.to_string().contains("larger than 4 bytes"), "{error}");
    }

    #[tokio::test]
    async fn unsafe_or_unversioned_objects_refuse_the_listing() {
        for object in [
            entry("guides/../x.md", 1, "", "t"),
            entry("other/x.md", 1, "", "t"),
            entry("guides/x.md", 1, "", ""),
            entry("", 1, "e", ""),
        ] {
            let store = Store::default().page("", page(vec![object], ""));
            let source = ArtifactSource::new(Arc::new(store), "docs", "guides").expect("source");
            assert!(source.list().await.is_err());
        }
    }

    #[tokio::test]
    async fn the_listing_caps_hold() {
        let store = Store::default()
            .page("", page(vec![entry("a", 1, "e", "")], "c2"))
            .page("c2", page(vec![entry("b", 1, "e", "")], "c3"))
            .page("c3", page(Vec::new(), ""));
        let source = ArtifactSource::new(Arc::new(store), "docs", "")
            .expect("source")
            .with_limits(SourceLimits {
                max_pages: 2,
                ..SourceLimits::default()
            });
        let error = source.list().await.expect_err("page cap");
        assert!(error.to_string().contains("page cap"), "{error}");

        let store = Store::default().page(
            "",
            page(vec![entry("a", 1, "e", ""), entry("b", 1, "e", "")], ""),
        );
        let source = ArtifactSource::new(Arc::new(store), "docs", "")
            .expect("source")
            .with_limits(SourceLimits {
                max_documents: 1,
                ..SourceLimits::default()
            });
        assert!(source.list().await.is_err());
    }

    #[test]
    fn a_bad_bucket_or_folder_is_refused() {
        let store: Arc<dyn ArtifactObjects> = Arc::new(Store::default());
        assert!(ArtifactSource::new(Arc::clone(&store), "a/b", "").is_err());
        assert!(ArtifactSource::new(store, "docs", "a/../b").is_err());
    }
}

//! Azure DevOps work items (the `ado_boards` family) as a `ContentSource`
//! (ADR-0030 I2).
//!
//! * **List.** The `wiql` query runs in windows of 2 000 ids: the user's
//!   `WHERE` is wrapped as `WHERE (…) AND [System.Id] > last`, its
//!   `ORDER BY` replaced by `ORDER BY [System.Id]` and its `ASOF` kept, so a
//!   query matching more than the 20 000 ids one WIQL answer may hold is
//!   still listed completely; a link query (`FROM WorkItemLinks`) cannot be
//!   windowed and is read in one answer, refused when that answer is at
//!   the server's ceiling (it would be truncated). The ids are then read in
//!   batches of 200 (`workitemsbatch`, `$expand=relations`) for their
//!   revision and attachments.
//! * **Version.** `System.Rev`: the revision moves on every field, relation
//!   and attachment change. The SDK compares `System.ChangedDate`.
//! * **Dependents.** Every `AttachedFile` relation is a document
//!   (`work_items/{id}/attachments/{name}`) versioned by its own identity
//!   (the attachment GUID of its URL, its size and modified date), and,
//!   with `include_comments` (on by default), the discussion is one document
//!   (`work_items/{id}/comments.md`) versioned by a hash over the comments'
//!   ids, versions and modified dates (read at listing time, so an edited
//!   comment re-indexes the discussion and an unrelated field edit does
//!   not). Both carry a `parent_key` (ADR-0030 "Dependent documents"). The
//!   SDK indexes attachments and no comments.
//! * **Comment cost.** The comments version is exact (the hash above), but
//!   reading it costs a call per work item. The version string therefore
//!   also carries what the work item looked like when it was computed:
//!   `{hash}@{CommentCount}@{ChangedDate}`. The run tells the source the
//!   versions the index already holds (`remember_versions`); a work item
//!   whose `CommentCount` and `ChangedDate` (both in the batch answer
//!   already read) still match the stored suffix reuses the stored version
//!   with no comment call, so an unchanged run makes none. Any other work
//!   item (new, a comment added, a field or comment edit that moved the
//!   change date) has its comments read, eight work items at a time. When
//!   the fresh hash equals the stored one (a field edit that left the
//!   discussion alone) the stored version is kept as it is, so the
//!   discussion is not re-indexed; the price is that such a work item has
//!   its comments read again on later runs until its discussion changes.
//! * **Fetch.** A work item is `workitems/{id}?$expand=all`, its fields
//!   sanitised (HTML to text, identities to display names, as the SDK's
//!   `sanitize`) and written as JSON, keeping `fields` when given; an
//!   attachment is its relation URL (same organization host), under the
//!   document cap; comments are paged by continuation token.

use std::collections::BTreeMap;
use std::fmt::Write as _;
use std::sync::{Arc, Mutex};

use elitea_content_source::{ContentSource, Document, DocumentRef, SourceError, content_version};
use futures_util::StreamExt as _;
use serde_json::{Map, Value, json};

use super::client::{AdoClient, AdoRequest, AdoScope};
use super::config::AdoToolkitConfig;
use super::text::{
    ImgTag, html_to_text, optional_bool, optional_text, optional_text_list, rewrite_images,
    safe_name, sanitize_field,
};
use crate::egress::HostAllowlist;
use crate::pictures::{
    self, Escape, PictureBatch, PictureStep, Reads, SourcePicture, UNREADABLE, UNSUPPORTED,
};
use crate::source::{
    Backoff, Failure, Listed, ListingCache, SourceHttp, SourceLimits, Timeouts, references,
    valid_key,
};
use crate::transport::header::ACCEPT;
use crate::transport::{HeaderValue, Method, Request, Transport, Url};

const PROVIDER: &str = "Azure DevOps";
const WIQL_API: &str = "7.1";
const WORK_ITEMS_API: &str = "7.1";
const COMMENTS_API: &str = "7.1-preview.4";
/// Ids per WIQL window; the server's own ceiling is 20 000.
const WIQL_PAGE: usize = 2_000;
/// The server's ceiling on one WIQL answer, asked for a link query.
const WIQL_CEILING: usize = 20_000;
/// Ids per `workitemsbatch` call (the server's maximum).
/// Bytes under which a download is no picture.
const MIN_PICTURE_BYTES: usize = 8;
/// Ids per `workitemsbatch` call (the server's maximum).
const BATCH: usize = 200;
/// Comments per page (the server's maximum).
const COMMENTS_PAGE: usize = 200;
/// Work items whose comments are read at once.
const COMMENT_CONCURRENCY: usize = 8;
const MAX_WIQL_BYTES: usize = 32 * 1_024;
const ATTACHED_FILE: &str = "AttachedFile";
const KEY_PREFIX: &str = "work_items";

/// The `index_data` arguments of the `ado_boards` family
/// (`ado/work_item/ado_wrapper.py::_index_tool_params`), plus
/// `include_comments` (this connector's; see the module notes).
#[allow(
    clippy::struct_excessive_bools,
    reason = "the SDK's index_data form is these flags"
)]
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct AdoBoardsIndexParams {
    pub wiql: String,
    /// The fields kept in the document; empty keeps every field.
    pub fields: Vec<String>,
    pub sanitize: bool,
    pub include_attachments: bool,
    pub include_comments: bool,
    /// Describe the `<img>` pictures of the work item's HTML fields with the
    /// chat model (the SDK's `process_images`, off by default).
    pub process_images: bool,
    /// The prompt for those descriptions (`image_description_prompt`).
    pub image_description_prompt: Option<String>,
}

impl AdoBoardsIndexParams {
    /// Read the arguments as the request carried them.
    ///
    /// # Errors
    ///
    /// No `wiql`, or an argument of the wrong type.
    pub fn from_arguments(arguments: &Map<String, Value>) -> Result<Self, String> {
        let wiql = optional_text(arguments, "wiql")?.ok_or_else(|| {
            "`wiql` is required: the query that selects the work items".to_owned()
        })?;
        if wiql.len() > MAX_WIQL_BYTES {
            return Err(format!("`wiql` is longer than {MAX_WIQL_BYTES} bytes"));
        }
        Ok(Self {
            wiql,
            fields: optional_text_list(arguments, "fields")?,
            sanitize: optional_bool(arguments, "sanitize")?.unwrap_or(true),
            include_attachments: optional_bool(arguments, "include_attachments")?.unwrap_or(true),
            include_comments: optional_bool(arguments, "include_comments")?.unwrap_or(true),
            process_images: optional_bool(arguments, "process_images")?.unwrap_or(false),
            image_description_prompt: optional_text(arguments, "image_description_prompt")?
                .filter(|prompt| !prompt.trim().is_empty()),
        })
    }
}

/// What a listed document is, beyond its bytes.
#[derive(Clone, Debug)]
enum Extra {
    WorkItem {
        id: u64,
        rev: u64,
        title: String,
        kind: String,
    },
    Comments {
        id: u64,
        title: String,
        /// The hash over the comments' ids, versions and modified dates
        /// the listing saw.
        version: String,
    },
    Attachment {
        id: u64,
        name: String,
        url: Url,
    },
}

/// The work items a query selects, listed and fetched through the shared
/// client.
pub struct AdoBoardsSource {
    client: AdoClient,
    http: SourceHttp,
    params: AdoBoardsIndexParams,
    cache: ListingCache,
    extras: Mutex<Arc<BTreeMap<String, Extra>>>,
    /// The versions the index holds, by key (see the module notes).
    known: Mutex<BTreeMap<String, String>>,
}

impl AdoBoardsSource {
    /// The configured project, every request through `allowlist`
    /// (fail-closed) and then `transport`.
    ///
    /// # Errors
    ///
    /// The organization's host is not on the allowlist.
    pub fn new(
        config: AdoToolkitConfig,
        params: AdoBoardsIndexParams,
        allowlist: HostAllowlist,
        transport: Arc<dyn Transport>,
    ) -> Result<Self, SourceError> {
        let (connection, _settings) = config.into_parts();
        let http = SourceHttp::new(allowlist, transport);
        if !http.permits(connection.organization_url()) {
            return Err(Failure::Refused.into_source_error(PROVIDER, None));
        }
        Ok(Self {
            client: AdoClient::new(connection, http.transport()),
            http,
            params,
            cache: ListingCache::default(),
            extras: Mutex::new(Arc::new(BTreeMap::new())),
            known: Mutex::new(BTreeMap::new()),
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

    #[must_use]
    pub fn with_timeouts(mut self, timeouts: Timeouts) -> Self {
        self.http = self.http.with_timeouts(timeouts);
        self
    }

    /// Tell the source the document versions the index already holds (key,
    /// version), so a listing can skip reading the comments of a work item
    /// that has not moved. Without it every listing reads them all.
    pub fn remember_versions(&self, versions: BTreeMap<String, String>) {
        *self
            .known
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner) = versions;
    }

    /// The version the index holds for `key`, if any.
    fn stored_version(&self, key: &str) -> Option<String> {
        self.known
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
            .get(key)
            .cloned()
    }

    fn build(&self, request: AdoRequest<'_>) -> Result<Request, Failure> {
        self.client
            .build(request)
            .map_err(|error| Failure::Client(error.to_string()))
    }

    /// Every id the query selects, ascending and unique.
    async fn ids(&self) -> Result<Vec<u64>, Failure> {
        let segments = ["wit", "wiql"];
        let query = &self.params.wiql;
        if is_link_query(query) {
            let request = self.build(
                AdoRequest::new(Method::POST, AdoScope::Project, &segments, WIQL_API)
                    .query("$top", WIQL_CEILING.to_string())
                    .json(json!({"query": query})),
            )?;
            let (_, body) = self.http.json(request).await?;
            let relations = body
                .get("workItemRelations")
                .and_then(Value::as_array)
                .ok_or(Failure::InvalidResponse(
                    "a link query answer has no workItemRelations",
                ))?;
            // The server returns at most `$top` relations without saying
            // it cut the answer: a full answer is a truncated one.
            if relations.len() >= WIQL_CEILING {
                return Err(Failure::InvalidResponse(
                    "the link query matches more work items than one answer holds; narrow it",
                ));
            }
            let mut ids = Vec::new();
            for relation in relations {
                for end in ["source", "target"] {
                    if let Some(id) = relation
                        .get(end)
                        .and_then(|e| e.get("id"))
                        .and_then(Value::as_u64)
                    {
                        ids.push(id);
                    }
                }
            }
            ids.sort_unstable();
            ids.dedup();
            return Ok(ids);
        }
        let mut ids: Vec<u64> = Vec::new();
        let mut after: Option<u64> = None;
        let mut pages = 0;
        loop {
            pages += 1;
            self.http.check_pages(pages)?;
            let request = self.build(
                AdoRequest::new(Method::POST, AdoScope::Project, &segments, WIQL_API)
                    .query("$top", WIQL_PAGE.to_string())
                    .json(json!({"query": windowed(query, after)})),
            )?;
            let (_, body) = self.http.json(request).await?;
            let page = body
                .get("workItems")
                .and_then(Value::as_array)
                .ok_or(Failure::InvalidResponse("a query answer has no workItems"))?;
            let mut last = None;
            for item in page {
                let id = item
                    .get("id")
                    .and_then(Value::as_u64)
                    .ok_or(Failure::InvalidResponse("a work item reference has no id"))?;
                if after.is_some_and(|after| id <= after) {
                    return Err(Failure::InvalidResponse(
                        "a query window returned an id out of order",
                    ));
                }
                ids.push(id);
                last = Some(id);
            }
            self.http.check_count(ids.len())?;
            match last {
                Some(last) if page.len() >= WIQL_PAGE => after = Some(last),
                _ => break,
            }
        }
        ids.sort_unstable();
        ids.dedup();
        Ok(ids)
    }

    #[allow(clippy::too_many_lines)] // One pass over the batch answer: item, comments, attachments.
    async fn listing(&self) -> Result<Vec<Listed>, Failure> {
        let ids = self.ids().await?;
        let mut listed = Vec::new();
        let mut extras = BTreeMap::new();
        let segments = ["wit", "workitemsbatch"];
        for batch in ids.chunks(BATCH) {
            let request = self.build(
                AdoRequest::new(Method::POST, AdoScope::Project, &segments, WORK_ITEMS_API)
                    .json(json!({"ids": batch, "$expand": "relations", "errorPolicy": "Omit"})),
            )?;
            let (_, body) = self.http.json(request).await?;
            let items = body
                .get("value")
                .and_then(Value::as_array)
                .ok_or(Failure::InvalidResponse("a batch answer has no value"))?;
            let mut refresh: Vec<(u64, String, String, Option<String>)> = Vec::new();
            for item in items {
                // `errorPolicy: Omit` answers a work item that was deleted
                // or is not visible since the query as `null`: it is simply
                // absent from the listing.
                if item.is_null() {
                    tracing::debug!(
                        "a work item selected by the query is gone or hidden; left out"
                    );
                    continue;
                }
                let id = item
                    .get("id")
                    .and_then(Value::as_u64)
                    .ok_or(Failure::InvalidResponse("a work item has no id"))?;
                let rev = item
                    .get("rev")
                    .and_then(Value::as_u64)
                    .ok_or(Failure::InvalidResponse("a work item has no rev"))?;
                let fields = item.get("fields").and_then(Value::as_object);
                let field_text = |name: &str| {
                    fields
                        .and_then(|fields| fields.get(name))
                        .and_then(Value::as_str)
                        .unwrap_or_default()
                        .to_owned()
                };
                let comments = fields
                    .and_then(|fields| fields.get("System.CommentCount"))
                    .and_then(Value::as_u64)
                    .unwrap_or(0);
                let version = rev.to_string();
                let title = field_text("System.Title");
                let kind = field_text("System.WorkItemType");
                let key = format!("{KEY_PREFIX}/{id}.json");
                extras.insert(
                    key.clone(),
                    Extra::WorkItem {
                        id,
                        rev,
                        title: title.clone(),
                        kind,
                    },
                );
                listed.push(Listed::new(key, version.clone(), 0, id.to_string()));
                if self.params.include_comments && comments > 0 {
                    let key = format!("{KEY_PREFIX}/{id}/comments.md");
                    let suffix = format!("@{comments}@{}", field_text("System.ChangedDate"));
                    let stored = self.stored_version(&key);
                    if let Some(known) = stored
                        .clone()
                        .filter(|version| is_version_for(version, &suffix))
                    {
                        extras.insert(
                            key.clone(),
                            Extra::Comments {
                                id,
                                title: title.clone(),
                                version: known.clone(),
                            },
                        );
                        listed.push(Listed::new(key, known, 0, id.to_string()));
                    } else {
                        refresh.push((id, title.clone(), suffix, stored));
                    }
                }
                if self.params.include_attachments
                    && let Some(relations) = item.get("relations").and_then(Value::as_array)
                {
                    for relation in relations {
                        if relation.get("rel").and_then(Value::as_str) != Some(ATTACHED_FILE) {
                            continue;
                        }
                        let Some(url) = relation
                            .get("url")
                            .and_then(Value::as_str)
                            .and_then(|url| Url::parse(url).ok())
                        else {
                            continue;
                        };
                        if !self.client.owns(&url) {
                            return Err(Failure::InvalidResponse(
                                "an attachment URL is not inside the organization",
                            ));
                        }
                        let attributes = relation.get("attributes").and_then(Value::as_object);
                        let name = attributes
                            .and_then(|a| a.get("name"))
                            .and_then(Value::as_str)
                            .and_then(safe_name)
                            .ok_or(Failure::InvalidResponse(
                                "an attachment relation has no usable name",
                            ))?;
                        let size = attributes
                            .and_then(|a| a.get("resourceSize"))
                            .and_then(Value::as_u64)
                            .unwrap_or(0);
                        let guid = url
                            .path_segments()
                            .and_then(|mut s| s.next_back())
                            .filter(|guid| !guid.is_empty())
                            .ok_or(Failure::InvalidResponse(
                                "an attachment URL names no attachment",
                            ))?
                            .to_owned();
                        let modified = attributes
                            .and_then(|a| a.get("resourceModifiedDate"))
                            .and_then(Value::as_str)
                            .unwrap_or_default();
                        // The attachment's own identity: its bytes never
                        // change under one GUID, so a re-upload (a new
                        // GUID) or a changed size/date is a new version,
                        // and the parent's revision does not re-index it.
                        let version = format!("{guid}:{size}:{modified}");
                        let mut key = format!("{KEY_PREFIX}/{id}/attachments/{name}");
                        if extras.contains_key(&key) {
                            key = format!("{KEY_PREFIX}/{id}/attachments/{guid}-{name}");
                        }
                        if !valid_key(&key) {
                            return Err(Failure::InvalidResponse(
                                "an attachment name is unsafe as a key",
                            ));
                        }
                        extras.insert(
                            key.clone(),
                            Extra::Attachment {
                                id,
                                name: name.clone(),
                                url,
                            },
                        );
                        listed.push(Listed::new(key, version, size, id.to_string()));
                    }
                }
                self.http.check_count(listed.len())?;
            }
            // The work items whose discussion may have moved, eight at a
            // time.
            let read: Vec<Result<(u64, String, String), Failure>> =
                futures_util::stream::iter(refresh)
                    .map(|(id, title, suffix, stored)| async move {
                        let (hash, _) = self.comments(id).await?;
                        // The discussion is what it was: keep the stored
                        // version, so the document is not rewritten.
                        let version = match stored {
                            Some(stored) if stored.split('@').next() == Some(hash.as_str()) => {
                                stored
                            }
                            _ => format!("{hash}{suffix}"),
                        };
                        Ok((id, title, version))
                    })
                    .buffer_unordered(COMMENT_CONCURRENCY)
                    .collect()
                    .await;
            for item in read {
                let (id, title, version) = item?;
                let key = format!("{KEY_PREFIX}/{id}/comments.md");
                extras.insert(
                    key.clone(),
                    Extra::Comments {
                        id,
                        title,
                        version: version.clone(),
                    },
                );
                listed.push(Listed::new(key, version, 0, id.to_string()));
            }
            self.http.check_count(listed.len())?;
        }
        *self
            .extras
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner) = Arc::new(extras);
        Ok(listed)
    }

    fn extras(&self) -> Arc<BTreeMap<String, Extra>> {
        Arc::clone(
            &self
                .extras
                .lock()
                .unwrap_or_else(std::sync::PoisonError::into_inner),
        )
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

    async fn listed_fresh(&self) -> Result<Arc<BTreeMap<String, Listed>>, SourceError> {
        self.cache
            .list_shared(|| async {
                self.listing()
                    .await
                    .map_err(|failure| failure.into_source_error(PROVIDER, None))
            })
            .await
    }

    fn work_item_url(&self, id: u64) -> String {
        format!(
            "{}/{}/_workitems/edit/{id}",
            self.client.organization_text(),
            self.client.project()
        )
    }

    /// The work item as the API answers it, pinned to the listed revision.
    async fn work_item_body(&self, id: u64, listed_rev: u64) -> Result<Value, Failure> {
        let id_text = id.to_string();
        let segments = ["wit", "workitems", id_text.as_str()];
        let request =
            self.build(AdoRequest::get(&segments, WORK_ITEMS_API).query("$expand", "all"))?;
        let (_, body) = self.http.json(request).await?;
        let rev = body
            .get("rev")
            .and_then(Value::as_u64)
            .ok_or(Failure::InvalidResponse("the work item has no rev"))?;
        if rev != listed_rev {
            return Err(Failure::Changed);
        }
        Ok(body)
    }

    /// The work item as the document. With `process_images` on and an
    /// `<img>` in a kept field, the metadata also carries the body the
    /// picture pass works from, under [`pictures::CONTEXT_KEY`]: it is the
    /// document's own, and the run takes it off before anything is stored.
    async fn work_item(
        &self,
        id: u64,
        listed_rev: u64,
    ) -> Result<(Vec<u8>, Map<String, Value>), Failure> {
        let body = self.work_item_body(id, listed_rev).await?;
        let wants_pictures = self.params.process_images
            && body
                .get("fields")
                .and_then(Value::as_object)
                .is_some_and(|fields| {
                    fields.iter().any(|(name, value)| {
                        (self.params.fields.is_empty() || self.params.fields.contains(name))
                            && value.as_str().is_some_and(|html| html.contains("<img"))
                    })
                });
        let (bytes, mut metadata) = self.render_work_item(id, &body, &mut |_, value| {
            if self.params.sanitize {
                sanitize_field(value)
            } else {
                value.clone()
            }
        })?;
        if wants_pictures {
            // Only the kept fields, as the picture pass renders them.
            let mut fields = Map::new();
            if let Some(all) = body.get("fields").and_then(Value::as_object) {
                for (name, value) in all {
                    if self.params.fields.is_empty() || self.params.fields.contains(name) {
                        fields.insert(name.clone(), value.clone());
                    }
                }
            }
            metadata.insert(
                pictures::CONTEXT_KEY.to_owned(),
                json!({"rev": body.get("rev"), "fields": fields}),
            );
        }
        Ok((bytes, metadata))
    }

    /// The document of a work item whose `body` was read. `field` turns each
    /// kept field into its value in the document.
    fn render_work_item(
        &self,
        id: u64,
        body: &Value,
        field: &mut dyn FnMut(&str, &Value) -> Value,
    ) -> Result<(Vec<u8>, Map<String, Value>), Failure> {
        let id_text = id.to_string();
        let rev = body
            .get("rev")
            .and_then(Value::as_u64)
            .ok_or(Failure::InvalidResponse("the work item has no rev"))?;
        let fields = body
            .get("fields")
            .and_then(Value::as_object)
            .ok_or(Failure::InvalidResponse("the work item has no fields"))?;
        let mut kept = Map::new();
        for (name, value) in fields {
            if !self.params.fields.is_empty() && !self.params.fields.iter().any(|f| f == name) {
                continue;
            }
            kept.insert(name.clone(), field(name, value));
        }
        let text = |name: &str| {
            fields
                .get(name)
                .and_then(Value::as_str)
                .unwrap_or_default()
                .to_owned()
        };
        let mut metadata = Map::new();
        metadata.insert("id".to_owned(), Value::String(id_text.clone()));
        metadata.insert(
            "type".to_owned(),
            Value::String(text("System.WorkItemType")),
        );
        metadata.insert("state".to_owned(), Value::String(text("System.State")));
        metadata.insert("area".to_owned(), Value::String(text("System.AreaPath")));
        metadata.insert("reason".to_owned(), Value::String(text("System.Reason")));
        metadata.insert(
            "iteration".to_owned(),
            Value::String(text("System.IterationPath")),
        );
        metadata.insert(
            "updated_on".to_owned(),
            Value::String(text("System.ChangedDate")),
        );
        metadata.insert("rev".to_owned(), Value::from(rev));
        let document = json!({"id": id, "url": self.work_item_url(id), "fields": kept});
        let bytes = serde_json::to_vec_pretty(&document)
            .map_err(|_| Failure::InvalidResponse("the work item cannot be serialised"))?;
        Ok((bytes, metadata))
    }

    /// The picture step for `key`: a work item, with `process_images`.
    #[must_use]
    pub fn picture_step(&self, key: &str) -> Option<PictureStep> {
        if !self.params.process_images {
            return None;
        }
        matches!(self.extras().get(key), Some(Extra::WorkItem { .. })).then(|| {
            PictureStep::Inline {
                prompt: self.params.image_description_prompt.clone(),
            }
        })
    }

    /// Whether a fetched work item has a picture to read: its metadata
    /// carries the picture context.
    #[must_use]
    pub fn may_have_pictures(&self, context: Option<&Value>) -> bool {
        self.params.process_images && context.is_some()
    }

    /// The pictures in the HTML fields of a work item (the SDK's field pass
    /// of `_fetch_work_item_document`): each `<img src>` that is an
    /// attachment of this organization with an image file name is read, and
    /// its description goes where the tag was, as `[image: …]` text (the
    /// default `sanitize`) or an `image-description` attribute. A picture
    /// that cannot be read leaves its `alt` text, as in the SDK. Works from
    /// the `context` `fetch` put in the document's metadata (no second
    /// request for the work item). A picture that was not read ends as it
    /// would had it been read and not described. `None`: the work item has no
    /// HTML picture.
    pub async fn pictures(
        &self,
        key: &str,
        _bytes: &[u8],
        context: Option<&Value>,
    ) -> Option<PictureBatch> {
        if !self.params.process_images {
            return None;
        }
        let extras = self.extras();
        let Some(Extra::WorkItem { id, .. }) = extras.get(key) else {
            return None;
        };
        let body = context?;
        // Pass 1: the distinct sources, in document order.
        let mut sources: Vec<String> = Vec::new();
        let fields = body.get("fields").and_then(Value::as_object)?;
        for value in fields.values() {
            if let Value::String(html) = value
                && html.contains("<img")
            {
                rewrite_images(html, &mut |tag| {
                    if let Some(src) = tag.src.as_ref()
                        && !sources.contains(src)
                    {
                        sources.push(src.clone());
                    }
                    String::new()
                });
            }
        }
        if sources.is_empty() || sources.iter().any(|src| pictures::holds_token(src)) {
            return None;
        }
        // Read them: once each, charged once each.
        let mut reads = Reads::default();
        let mut read: BTreeMap<String, bytes::Bytes> = BTreeMap::new();
        for src in &sources {
            if src.is_empty() {
                continue;
            }
            let outcome = reads.get(src, || self.read_picture(src)).await;
            if let Some(data) = outcome {
                read.insert(src.clone(), data);
            }
        }
        if read.is_empty() && !reads.has_skips() {
            return None;
        }
        // Pass 2: the document with a token for each `<img>` of a read
        // picture (each keeps its own markup and `alt`; equal bytes cost the
        // model one call through the run's cache) and the SDK's `alt` text
        // where nothing was read.
        let sanitize = self.params.sanitize;
        let mut occurrences: Vec<SourcePicture> = Vec::new();
        let rendered = self.render_work_item(*id, body, &mut |_, value| {
            let Value::String(html) = value else {
                return if sanitize {
                    sanitize_field(value)
                } else {
                    value.clone()
                };
            };
            if !html.contains("<img") {
                return if sanitize {
                    sanitize_field(value)
                } else {
                    value.clone()
                };
            }
            let rewritten = rewrite_images(html, &mut |tag| {
                let alt = tag.alt.clone().filter(|alt| !alt.is_empty());
                let fallback = if sanitize {
                    alt.map_or_else(String::new, |alt| format!("[image: {alt}]"))
                } else {
                    tag.raw.clone()
                };
                let Some(data) = tag.src.as_ref().and_then(|src| read.get(src)) else {
                    return fallback;
                };
                if occurrences.len() >= pictures::MAX_OCCURRENCES {
                    return fallback;
                }
                let token = pictures::token(occurrences.len());
                let mut picture = describe_tag(tag, token.clone(), &fallback, sanitize);
                picture.bytes = data.clone();
                if let Some(name) = tag.src.as_deref().and_then(file_name_of) {
                    picture.name = name;
                }
                occurrences.push(picture);
                token
            });
            if sanitize {
                sanitize_field(&Value::String(rewritten))
            } else {
                Value::String(rewritten)
            }
        });
        let (template, _) = rendered.ok()?;
        let mut batch = PictureBatch {
            template: String::from_utf8(template).ok()?,
            pictures: occurrences,
            skipped: reads.into_skipped(),
            ..PictureBatch::default()
        };
        batch.pictures.shrink_to_fit();
        (!batch.pictures.is_empty() || !batch.skipped.is_empty()).then_some(batch)
    }

    /// One `<img src>`: its bytes when it is an attachment of this
    /// organization with an image file name; `Err` the skip reason.
    async fn read_picture(&self, src: &str) -> Result<Vec<u8>, &'static str> {
        // `data:` pictures are inline already; a link elsewhere is not read
        // with this connector's credential.
        let Ok(url) = Url::parse(src) else {
            return Err(UNSUPPORTED);
        };
        let attachment = url
            .path_segments()
            .is_some_and(|mut segments| segments.any(|segment| segment == "attachments"));
        if !attachment || !self.client.owns(&url) {
            return Err(UNSUPPORTED);
        }
        let Some(name) = file_name_of(src) else {
            return Err(UNSUPPORTED);
        };
        if !pictures::has_image_extension(&name) {
            return Err(UNSUPPORTED);
        }
        let bytes = match self
            .attachment(&url, Some(pictures::MAX_PICTURE_BYTES))
            .await
        {
            Ok(bytes) => bytes,
            Err(failure) => return Err(pictures::skip_reason(&failure)),
        };
        // An image model takes no body this small or that is no picture.
        if bytes.len() < MIN_PICTURE_BYTES {
            return Err(UNREADABLE);
        }
        Ok(bytes)
    }

    /// The discussion: its version (a hash over every comment's id, version
    /// and modified date, in order) and the markdown, every page followed.
    async fn comments(&self, id: u64) -> Result<(String, Vec<u8>), Failure> {
        let id_text = id.to_string();
        let segments = ["wit", "workItems", id_text.as_str(), "comments"];
        let mut out = String::new();
        let mut manifest = String::new();
        let mut continuation: Option<String> = None;
        let mut pages = 0;
        loop {
            pages += 1;
            self.http.check_pages(pages)?;
            let request = self.build(
                AdoRequest::get(&segments, COMMENTS_API)
                    .query("$top", COMMENTS_PAGE.to_string())
                    .query_opt("continuationToken", continuation.take()),
            )?;
            let (_, body) = self.http.json(request).await?;
            for comment in body
                .get("comments")
                .and_then(Value::as_array)
                .ok_or(Failure::InvalidResponse("a comments page has no comments"))?
            {
                let author = comment
                    .get("createdBy")
                    .and_then(|by| by.get("displayName"))
                    .and_then(Value::as_str)
                    .unwrap_or("unknown");
                let date = comment
                    .get("createdDate")
                    .and_then(Value::as_str)
                    .unwrap_or_default();
                let text = comment
                    .get("text")
                    .and_then(Value::as_str)
                    .map(html_to_text)
                    .unwrap_or_default();
                let _ = write!(out, "## {author} ({date})\n\n{text}\n\n");
                let comment_id = comment.get("id").and_then(Value::as_u64).unwrap_or(0);
                let version = comment.get("version").and_then(Value::as_u64).unwrap_or(0);
                let modified = comment
                    .get("modifiedDate")
                    .and_then(Value::as_str)
                    .unwrap_or(date);
                let _ = writeln!(manifest, "{comment_id}:{version}:{modified}");
            }
            continuation = body
                .get("continuationToken")
                .and_then(Value::as_str)
                .filter(|token| !token.is_empty())
                .map(ToOwned::to_owned);
            if continuation.is_none() {
                return Ok((content_version(manifest.as_bytes()), out.into_bytes()));
            }
        }
    }

    async fn attachment(&self, url: &Url, within: Option<u64>) -> Result<Vec<u8>, Failure> {
        let mut url = url.clone();
        if !url.query_pairs().any(|(name, _)| name == "api-version") {
            url.query_pairs_mut()
                .append_pair("api-version", WORK_ITEMS_API);
        }
        let mut request = Request::new(Method::GET, url);
        self.client
            .authorize(&mut request)
            .map_err(|error| Failure::Client(error.to_string()))?;
        request
            .headers_mut()
            .insert(ACCEPT, HeaderValue::from_static("application/octet-stream"));
        match within {
            Some(limit) => self.http.raw_within(request, limit).await,
            None => self.http.raw(request).await,
        }
    }
}

/// The `fileName` query parameter of an attachment URL.
fn file_name_of(src: &str) -> Option<String> {
    let url = Url::parse(src).ok()?;
    url.query_pairs()
        .find(|(name, _)| name == "fileName")
        .map(|(_, value)| value.into_owned())
        .filter(|name| !name.is_empty())
}

/// What `fallback` (the tag's replacement HTML) becomes once its field is
/// sanitised, as it does for a picture that was not read.
fn sanitized_fallback(fallback: &str) -> String {
    html_to_text(&format!("<p>{fallback}</p>"))
        .trim()
        .to_owned()
}

/// The picture a tag becomes: `[image: …]` text under `sanitize`, else the
/// tag with an `image-description` attribute.
fn describe_tag(tag: &ImgTag, token: String, fallback: &str, sanitize: bool) -> SourcePicture {
    if sanitize {
        return SourcePicture {
            token,
            name: String::new(),
            bytes: bytes::Bytes::new(),
            prompt: None,
            before: "[image: ".to_owned(),
            after: "]".to_owned(),
            // The text the unread case ends as: the same sanitising, then
            // written into the serialised document (JSON text).
            fallback: pictures::json_contents(&sanitized_fallback(fallback)),
            escapes: &[Escape::Lines, Escape::Json],
        };
    }
    let open = tag.raw.trim_end_matches('>');
    let (open, close) = open
        .strip_suffix('/')
        .map_or((open, ">"), |without| (without.trim_end(), "/>"));
    SourcePicture {
        token,
        name: String::new(),
        bytes: bytes::Bytes::new(),
        prompt: None,
        before: pictures::json_contents(&format!("{open} image-description=\"")),
        after: pictures::json_contents(&format!("\"{close}")),
        fallback: pictures::json_contents(fallback),
        escapes: &[Escape::HtmlAttribute, Escape::Json],
    }
}

/// Whether `version` (`{hash}@{count}@{date}`) was computed for a work item
/// that looks as `suffix` (`@{count}@{date}`) says now.
fn is_version_for(version: &str, suffix: &str) -> bool {
    version
        .strip_suffix(suffix)
        .is_some_and(|hash| hash.len() == 64 && hash.bytes().all(|byte| byte.is_ascii_hexdigit()))
}

/// Whether a WIQL query is a link query (`FROM WorkItemLinks`).
fn is_link_query(query: &str) -> bool {
    let lower = query.to_ascii_lowercase();
    lower
        .split_whitespace()
        .collect::<Vec<_>>()
        .windows(2)
        .any(|pair| pair[0] == "from" && pair[1].trim_matches(['(', ')']) == "workitemlinks")
}

/// The query for the ids after `after`, ordered by id: the user's query
/// with its `ORDER BY` replaced by `ORDER BY [System.Id]`, its `ASOF` kept
/// last, and the window condition added to its `WHERE` as
/// `WHERE (original) AND [System.Id] > after` — in parentheses, so a
/// top-level `OR` in the original keeps its meaning.
fn windowed(query: &str, after: Option<u64>) -> String {
    let trimmed = query.trim().trim_end_matches(';').trim();
    let words = words_outside_quotes(trimmed);
    let where_at = find_words(&words, &["where"]);
    let order_at = find_words(&words, &["order", "by"]);
    let asof_at = find_words(&words, &["asof"]);
    let end = trimmed.len();
    // The `WHERE` body ends where the next clause starts.
    let tail_start = [order_at, asof_at]
        .into_iter()
        .flatten()
        .map(|(start, _)| start)
        .filter(|start| where_at.is_none_or(|(where_start, _)| *start > where_start))
        .min()
        .unwrap_or(end);
    let head = trimmed[..where_at.map_or(tail_start, |(start, _)| start)].trim_end();
    let where_body = where_at.map(|(_, after_keyword)| trimmed[after_keyword..tail_start].trim());
    let asof = asof_at.map(|(start, _)| {
        let asof_end = order_at
            .map(|(order_start, _)| order_start)
            .filter(|order_start| *order_start > start)
            .unwrap_or(end);
        trimmed[start..asof_end].trim()
    });
    let mut out = head.to_owned();
    match (where_body.filter(|body| !body.is_empty()), after) {
        (Some(body), Some(after)) => {
            let _ = write!(out, " WHERE ({body}) AND [System.Id] > {after}");
        }
        (Some(body), None) => {
            let _ = write!(out, " WHERE {body}");
        }
        (None, Some(after)) => {
            let _ = write!(out, " WHERE [System.Id] > {after}");
        }
        (None, None) => {}
    }
    out.push_str(" ORDER BY [System.Id]");
    if let Some(asof) = asof {
        let _ = write!(out, " {asof}");
    }
    out
}

/// The words of `query` outside single-quoted strings and `[bracketed]`
/// names: (start byte, end byte, lower-cased text).
fn words_outside_quotes(query: &str) -> Vec<(usize, usize, String)> {
    let mut words = Vec::new();
    let mut word_start: Option<usize> = None;
    let mut quoted = false;
    let mut bracketed = false;
    let close = |words: &mut Vec<(usize, usize, String)>, start: &mut Option<usize>, at| {
        if let Some(begin) = start.take() {
            words.push((begin, at, query[begin..at].to_ascii_lowercase()));
        }
    };
    for (index, character) in query.char_indices() {
        if quoted {
            if character == '\'' {
                quoted = false;
            }
            continue;
        }
        if bracketed {
            if character == ']' {
                bracketed = false;
            }
            continue;
        }
        match character {
            '\'' => {
                close(&mut words, &mut word_start, index);
                quoted = true;
            }
            '[' => {
                close(&mut words, &mut word_start, index);
                bracketed = true;
            }
            '(' | ')' | ',' => close(&mut words, &mut word_start, index),
            c if c.is_whitespace() => close(&mut words, &mut word_start, index),
            _ => {
                if word_start.is_none() {
                    word_start = Some(index);
                }
            }
        }
    }
    close(&mut words, &mut word_start, query.len());
    words
}

/// Where `keyword` (one or more consecutive words) first appears: the byte
/// index of its first word and the byte index after its last.
fn find_words(words: &[(usize, usize, String)], keyword: &[&str]) -> Option<(usize, usize)> {
    words
        .windows(keyword.len())
        .find(|window| {
            window
                .iter()
                .zip(keyword)
                .all(|((_, _, word), wanted)| word == wanted)
        })
        .map(|window| (window[0].0, window[window.len() - 1].1))
}

impl ContentSource for AdoBoardsSource {
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
        let extras = self.extras();
        let extra = extras
            .get(key)
            .ok_or_else(|| SourceError::NotFound(key.to_owned()))?;
        let (bytes, title, mut metadata) = match extra {
            Extra::WorkItem { id, rev, title, .. } => {
                let (bytes, metadata) = self.work_item(*id, *rev).await.map_err(failed)?;
                (bytes, title.clone(), metadata)
            }
            Extra::Comments { id, title, version } => {
                let (now, bytes) = self.comments(*id).await.map_err(failed)?;
                if version.split('@').next() != Some(now.as_str()) {
                    return Err(failed(Failure::Changed));
                }
                let mut metadata = Map::new();
                metadata.insert("id".to_owned(), Value::String(format!("{id}::comments")));
                metadata.insert("type".to_owned(), Value::String("comments".to_owned()));
                metadata.insert(
                    "parent_key".to_owned(),
                    Value::String(format!("{KEY_PREFIX}/{id}.json")),
                );
                (bytes, format!("Comments on {title}"), metadata)
            }
            Extra::Attachment { id, name, url } => {
                let bytes = self.attachment(url, None).await.map_err(failed)?;
                let mut metadata = Map::new();
                metadata.insert("id".to_owned(), Value::String(format!("{id}::{name}")));
                metadata.insert("type".to_owned(), Value::String("attachment".to_owned()));
                metadata.insert(
                    "parent_key".to_owned(),
                    Value::String(format!("{KEY_PREFIX}/{id}.json")),
                );
                (bytes, name.clone(), metadata)
            }
        };
        let id = match extra {
            Extra::WorkItem { id, .. }
            | Extra::Comments { id, .. }
            | Extra::Attachment { id, .. } => *id,
        };
        metadata.insert("work_item_id".to_owned(), Value::String(id.to_string()));
        if let Extra::WorkItem { kind, .. } = extra {
            metadata.insert("title".to_owned(), Value::String(title.clone()));
            metadata
                .entry("type".to_owned())
                .or_insert_with(|| Value::String(kind.clone()));
        }
        let mut reference = listed.reference.clone();
        reference.size = bytes.len() as u64;
        Ok(Document {
            title,
            uri: Some(self.work_item_url(id)),
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
    use std::sync::atomic::{AtomicU64, Ordering};

    const ORG: &str = "https://dev.azure.com/contoso";
    const MODIFIED: &str = "2026-09-30T12:00:00Z";

    fn config() -> AdoToolkitConfig {
        let settings = json!({
            "ado_configuration": {"organization_url": ORG, "token": "pat"},
            "project": "Fabrikam",
        });
        AdoToolkitConfig::parse(settings.as_object().expect("settings")).expect("config")
    }

    fn params(value: &Value) -> AdoBoardsIndexParams {
        AdoBoardsIndexParams::from_arguments(value.as_object().expect("object")).expect("params")
    }

    fn allow() -> HostAllowlist {
        HostAllowlist::parse(Some("dev.azure.com"))
    }

    fn body_json(request: &Request) -> Value {
        serde_json::from_slice(request.body().and_then(|b| b.as_bytes()).expect("body"))
            .expect("json")
    }

    fn work_item(id: u64, rev: u64, comments: u64, attachments: &[(&str, &str, u64)]) -> Value {
        let relations: Vec<Value> = attachments
            .iter()
            .map(|(name, guid, size)| {
                json!({"rel": "AttachedFile", "url": format!("{ORG}/p-guid/_apis/wit/attachments/{guid}"),
                       "attributes": {"id": 7, "name": name, "resourceSize": size,
                                      "resourceCreatedDate": MODIFIED, "resourceModifiedDate": MODIFIED}})
            })
            .chain(std::iter::once(json!({"rel": "System.LinkTypes.Hierarchy-Forward", "url": format!("{ORG}/_apis/wit/workItems/9")})))
            .collect();
        json!({
            "id": id, "rev": rev,
            "fields": {
                "System.Id": id, "System.Title": format!("Item {id}"), "System.WorkItemType": "Bug",
                "System.State": "Active", "System.AreaPath": "Fabrikam", "System.Reason": "New",
                "System.IterationPath": "Fabrikam\\Sprint 1", "System.ChangedDate": "2026-10-01T00:00:00Z",
                "System.CommentCount": comments,
                "System.Description": "<div>Crash on <b>start</b></div>",
                "System.AssignedTo": {"displayName": "Ada", "uniqueName": "ada@example.com"},
            },
            "relations": relations,
        })
    }

    /// A comment as the comments API returns one (`7.1-preview.4`).
    fn comment(id: u64, version: u64, author: &str, text: &str) -> Value {
        json!({"id": id, "version": version, "text": text, "workItemId": 1,
               "createdBy": {"displayName": author}, "createdDate": format!("2026-10-01T0{id}:00:00Z"),
               "modifiedBy": {"displayName": author}, "modifiedDate": format!("2026-10-01T0{version}:00:00Z")})
    }

    /// The window a query asks for: the id after which it selects, when the
    /// window condition was added the way WIQL precedence needs it — in an
    /// `AND` outside the user's parenthesised `WHERE`. A condition appended
    /// to a `WHERE` with a top-level `OR` binds to its last operand only, so
    /// the server would answer the `OR`'s other branch again from id 1: the
    /// fixture does the same.
    fn window_after(query_text: &str) -> u64 {
        let has_where = query_text.to_ascii_lowercase().contains(" where ");
        let after = query_text
            .split("[System.Id] > ")
            .nth(1)
            .and_then(|rest| rest.split_whitespace().next())
            .and_then(|n| n.parse().ok())
            .unwrap_or(0);
        if after == 0 || !has_where {
            return after;
        }
        if query_text.contains(") AND [System.Id] > ") {
            after
        } else {
            0
        }
    }

    fn reply(request: &Request, items: &[(u64, u64, u64)]) -> Reply {
        let path = request.url().path();
        match path {
            "/contoso/Fabrikam/_apis/wit/wiql" => {
                let body = body_json(request);
                let query_text = body["query"].as_str().expect("query");
                assert!(query_text.contains("ORDER BY [System.Id]"), "{query_text}");
                assert_eq!(query(request, "$top").as_deref(), Some("2000"));
                let after = window_after(query_text);
                let page: Vec<Value> = items
                    .iter()
                    .filter(|(id, _, _)| *id > after)
                    .take(WIQL_PAGE)
                    .map(|(id, _, _)| json!({"id": id}))
                    .collect();
                Reply::json(&json!({"queryType": "flat", "workItems": page}))
            }
            "/contoso/Fabrikam/_apis/wit/workitemsbatch" => {
                let body = body_json(request);
                assert_eq!(body["$expand"], "relations");
                assert_eq!(
                    body["errorPolicy"], "Omit",
                    "a vanished work item is omitted, not a failed batch"
                );
                let ids: Vec<u64> = body["ids"]
                    .as_array()
                    .expect("ids")
                    .iter()
                    .map(|v| v.as_u64().unwrap())
                    .collect();
                assert!(ids.len() <= BATCH);
                let value: Vec<Value> = items
                    .iter()
                    .filter(|(id, _, _)| ids.contains(id))
                    .map(|(id, rev, comments)| {
                        let attachments = if *id == 1 {
                            vec![("spec.pdf", "a1", 5_u64), ("spec.pdf", "a2", 6)]
                        } else {
                            vec![]
                        };
                        work_item(*id, *rev, *comments, &attachments)
                    })
                    .collect();
                Reply::json(&json!({"count": value.len(), "value": value}))
            }
            "/contoso/Fabrikam/_apis/wit/workitems/1" => {
                assert_eq!(query(request, "$expand").as_deref(), Some("all"));
                Reply::json(&work_item(1, items[0].1, items[0].2, &[]))
            }
            "/contoso/Fabrikam/_apis/wit/workItems/1/comments" => {
                match query(request, "continuationToken").as_deref() {
                    None => Reply::json(&json!({
                        "totalCount": 2, "count": 1,
                        "comments": [comment(1, 1, "Ada", "<p>First</p>")],
                        "continuationToken": "c2",
                    })),
                    Some("c2") => Reply::json(&json!({
                        "totalCount": 2, "count": 1,
                        "comments": [comment(2, 1, "Bob", "Second")],
                    })),
                    other => panic!("token {other:?}"),
                }
            }
            "/contoso/p-guid/_apis/wit/attachments/a2" => {
                assert_eq!(
                    query(request, "api-version").as_deref(),
                    Some(WORK_ITEMS_API)
                );
                assert!(
                    request
                        .headers()
                        .get("authorization")
                        .expect("pat")
                        .is_sensitive()
                );
                Reply::bytes(b"%PDF-2")
            }
            other => panic!("unexpected request {other}"),
        }
    }

    /// The listed (key, version, size) of work item 1 and its dependents.
    fn group_of_one(listed: &[DocumentRef]) -> Vec<(String, String, u64)> {
        listed
            .iter()
            .filter(|d| d.key.starts_with("work_items/1/") || d.key == "work_items/1.json")
            .map(|d| (d.key.clone(), d.version.clone(), d.size))
            .collect()
    }

    #[tokio::test]
    async fn work_items_are_windowed_batched_and_carry_comments_and_attachments_as_dependents() {
        // 2 001 work items: the first window is full, the second holds one.
        let items: Vec<(u64, u64, u64)> =
            (1..=2_001).map(|id| (id, 3, u64::from(id == 1))).collect();
        let fixture = Scripted::new(move |request, _| reply(request, &items));
        let source = AdoBoardsSource::new(
            config(),
            params(&json!({"wiql": "SELECT [System.Id] FROM WorkItems WHERE [System.TeamProject] = @project ORDER BY [System.ChangedDate] DESC"})),
            allow(),
            fixture.clone(),
        )
        .expect("source");
        let listed = source.list().await.expect("listing");
        // 2 001 items, one comments document, two attachments.
        assert_eq!(listed.len(), 2_004);
        let seen = fixture.seen();
        assert_eq!(seen.iter().filter(|s| s.contains("/wiql")).count(), 2);
        assert_eq!(
            seen.iter()
                .filter(|s| s.contains("/workitemsbatch"))
                .count(),
            11
        );
        assert_eq!(
            seen.iter().filter(|s| s.contains("/comments")).count(),
            2,
            "the discussion was read (both pages) once at listing time for its version"
        );
        let first = group_of_one(&listed);
        let comments_version = first[3].1.clone();
        let (hash, state) = comments_version.split_once('@').expect("hash@count@date");
        assert_eq!(hash.len(), 64, "a hash: {comments_version}");
        assert_eq!(
            state, "1@2026-10-01T00:00:00Z",
            "the version carries the CommentCount and ChangedDate it was read at"
        );
        assert_eq!(
            first,
            [
                ("work_items/1.json".to_owned(), "3".to_owned(), 0),
                (
                    "work_items/1/attachments/a2-spec.pdf".to_owned(),
                    format!("a2:6:{MODIFIED}"),
                    6
                ),
                (
                    "work_items/1/attachments/spec.pdf".to_owned(),
                    format!("a1:5:{MODIFIED}"),
                    5
                ),
                (
                    "work_items/1/comments.md".to_owned(),
                    comments_version.clone(),
                    0
                ),
            ]
        );
        let item = source.fetch("work_items/1.json").await.expect("item");
        let body: Value = serde_json::from_slice(&item.bytes).expect("json");
        assert_eq!(body["fields"]["System.Description"], "Crash on start");
        assert_eq!(body["fields"]["System.AssignedTo"], "Ada");
        assert_eq!(body["url"], format!("{ORG}/Fabrikam/_workitems/edit/1"));
        assert_eq!(item.metadata["type"], "Bug");
        assert_eq!(item.metadata["title"], "Item 1");
        assert_eq!(item.metadata["updated_on"], "2026-10-01T00:00:00Z");
        assert_eq!(item.reference.mime, "application/json");
        let comments = source
            .fetch("work_items/1/comments.md")
            .await
            .expect("comments");
        let text = String::from_utf8(comments.bytes).expect("utf8");
        assert!(
            text.contains("## Ada (2026-10-01T01:00:00Z)\n\nFirst"),
            "{text}"
        );
        assert!(text.contains("## Bob"), "both pages were read: {text}");
        assert_eq!(comments.metadata["parent_key"], "work_items/1.json");
        assert_eq!(comments.reference.version, comments_version);
        let attachment = source
            .fetch("work_items/1/attachments/a2-spec.pdf")
            .await
            .expect("attachment");
        assert_eq!(attachment.bytes, b"%PDF-2");
        assert_eq!(attachment.metadata["parent_key"], "work_items/1.json");
        assert_eq!(attachment.title, "spec.pdf");
    }

    /// The recorded tenant in three states: as recorded, after a field edit
    /// (rev 3 → 4), after a new comment (comment 3 added).
    fn phased_reply(request: &Request, phase: u64) -> Reply {
        let items = [(
            1_u64,
            if phase >= 1 { 4 } else { 3 },
            if phase >= 2 { 3 } else { 2 },
        )];
        if request.url().path() == "/contoso/Fabrikam/_apis/wit/workItems/1/comments" {
            let mut comments = vec![
                comment(1, 1, "Ada", "<p>First</p>"),
                comment(2, 1, "Bob", "Second"),
            ];
            if phase >= 2 {
                comments.push(comment(3, 1, "Cy", "Third"));
            }
            return Reply::json(
                &json!({"totalCount": comments.len(), "count": comments.len(), "comments": comments}),
            );
        }
        reply(request, &items)
    }

    #[tokio::test]
    async fn a_field_edit_moves_only_the_work_item_and_a_new_comment_only_the_discussion() {
        let phase = Arc::new(AtomicU64::new(0));
        let listing_at = |phase: &Arc<AtomicU64>| {
            let phase = Arc::clone(phase);
            Scripted::new(move |request, _| phased_reply(request, phase.load(Ordering::SeqCst)))
        };
        let list = |transport| async move {
            let source = AdoBoardsSource::new(
                config(),
                params(&json!({"wiql": "SELECT [System.Id] FROM WorkItems"})),
                allow(),
                transport,
            )
            .expect("source");
            group_of_one(&source.list().await.expect("listing"))
        };
        let recorded = list(listing_at(&phase)).await;
        assert_eq!(recorded.len(), 4);

        phase.store(1, Ordering::SeqCst);
        let after_field_edit = list(listing_at(&phase)).await;
        let moved: Vec<&str> = after_field_edit
            .iter()
            .zip(&recorded)
            .filter(|(now, before)| now.1 != before.1)
            .map(|(now, _)| now.0.as_str())
            .collect();
        assert_eq!(
            moved,
            ["work_items/1.json"],
            "a field edit (rev 3 → 4) moves the work item's version only"
        );

        phase.store(2, Ordering::SeqCst);
        let after_comment = list(listing_at(&phase)).await;
        let moved: Vec<&str> = after_comment
            .iter()
            .zip(&after_field_edit)
            .filter(|(now, before)| now.1 != before.1)
            .map(|(now, _)| now.0.as_str())
            .collect();
        assert_eq!(
            moved,
            ["work_items/1/comments.md"],
            "a new comment moves the discussion's version only"
        );
    }

    #[tokio::test]
    async fn a_discussion_that_moved_on_since_the_listing_is_reported_changed() {
        let phase = Arc::new(AtomicU64::new(0));
        let shared = Arc::clone(&phase);
        let fixture =
            Scripted::new(move |request, _| phased_reply(request, shared.load(Ordering::SeqCst)));
        let source = AdoBoardsSource::new(
            config(),
            params(
                &json!({"wiql": "SELECT [System.Id] FROM WorkItems", "include_attachments": false}),
            ),
            allow(),
            fixture,
        )
        .expect("source");
        source.list().await.expect("listing");
        phase.store(2, Ordering::SeqCst);
        let error = source
            .fetch("work_items/1/comments.md")
            .await
            .expect_err("changed");
        assert!(matches!(error, SourceError::Changed(_)), "{error}");
    }

    #[tokio::test]
    async fn a_query_with_a_top_level_or_is_windowed_in_parentheses() {
        // 2 001 work items match `A OR B`; without the parentheses the second
        // window's `OR B AND [System.Id] > 2000` re-selects branch A from id
        // 1, which the connector reports as an out-of-order answer.
        let items: Vec<(u64, u64, u64)> = (1..=2_001).map(|id| (id, 1, 0)).collect();
        let fixture = Scripted::new(move |request, _| reply(request, &items));
        let source = AdoBoardsSource::new(
            config(),
            params(&json!({"wiql": "SELECT [System.Id] FROM WorkItems WHERE [System.State] = 'Active' OR [System.State] = 'New' ORDER BY [System.Id] DESC", "include_attachments": false})),
            allow(),
            fixture.clone(),
        )
        .expect("source");
        let listed = source.list().await.expect("listing");
        assert_eq!(listed.len(), 2_001);
        let queries: Vec<String> = fixture
            .requests()
            .iter()
            .filter(|request| request.url().path().ends_with("/wiql"))
            .map(|request| body_json(request)["query"].as_str().expect("q").to_owned())
            .collect();
        assert_eq!(
            queries,
            [
                "SELECT [System.Id] FROM WorkItems WHERE [System.State] = 'Active' OR [System.State] = 'New' ORDER BY [System.Id]",
                "SELECT [System.Id] FROM WorkItems WHERE ([System.State] = 'Active' OR [System.State] = 'New') AND [System.Id] > 2000 ORDER BY [System.Id]",
            ]
        );
    }

    #[tokio::test]
    async fn a_work_item_that_moved_on_since_the_listing_is_reported_changed() {
        let items = vec![(1_u64, 3_u64, 0_u64)];
        let fixture = Scripted::new(move |request, _| {
            if request.url().path() == "/contoso/Fabrikam/_apis/wit/workitems/1" {
                return Reply::json(&work_item(1, 4, 0, &[]));
            }
            reply(request, &items)
        });
        let source = AdoBoardsSource::new(
            config(),
            params(&json!({"wiql": "SELECT [System.Id] FROM WorkItems", "fields": ["System.Title"], "sanitize": false, "include_attachments": false})),
            allow(),
            fixture,
        )
        .expect("source");
        let listed = source.list().await.expect("listing");
        assert_eq!(listed.len(), 1, "no comments and attachments switched off");
        let error = source
            .fetch("work_items/1.json")
            .await
            .expect_err("changed");
        assert!(matches!(error, SourceError::Changed(_)), "{error}");
    }

    #[tokio::test]
    async fn fields_filter_the_document_and_a_denied_token_is_reported() {
        let items = vec![(1_u64, 3_u64, 0_u64)];
        let fixture = Scripted::new(move |request, _| reply(request, &items));
        let source = AdoBoardsSource::new(
            config(),
            params(&json!({"wiql": "SELECT [System.Id] FROM WorkItems", "fields": ["System.Title"], "include_comments": false, "include_attachments": false})),
            allow(),
            fixture,
        )
        .expect("source");
        let item = source.fetch("work_items/1.json").await.expect("item");
        let body: Value = serde_json::from_slice(&item.bytes).expect("json");
        assert_eq!(body["fields"].as_object().expect("fields").len(), 1);
        assert_eq!(body["fields"]["System.Title"], "Item 1");

        let denied = Scripted::new(|_, _| Reply::status(StatusCode::UNAUTHORIZED));
        let source = AdoBoardsSource::new(
            config(),
            params(&json!({"wiql": "SELECT [System.Id] FROM WorkItems"})),
            allow(),
            denied,
        )
        .expect("source");
        let error = source.list().await.expect_err("401");
        assert!(error.to_string().contains("HTTP 401"), "{error}");
        assert!(
            AdoBoardsIndexParams::from_arguments(&Map::new())
                .expect_err("wiql required")
                .contains("wiql")
        );
    }

    /// A link query answer with `relations` relations, every one between
    /// work items 1 and 2.
    fn link_reply(request: &Request, relations: usize) -> Reply {
        match request.url().path() {
            "/contoso/Fabrikam/_apis/wit/wiql" => {
                assert_eq!(query(request, "$top").as_deref(), Some("20000"));
                let value: Vec<Value> = (0..relations)
                    .map(|_| json!({"rel": "System.LinkTypes.Hierarchy-Forward", "source": {"id": 1}, "target": {"id": 2}}))
                    .collect();
                Reply::json(&json!({"queryType": "oneHop", "workItemRelations": value}))
            }
            "/contoso/Fabrikam/_apis/wit/workitemsbatch" => {
                let ids: Vec<u64> = body_json(request)["ids"]
                    .as_array()
                    .expect("ids")
                    .iter()
                    .filter_map(Value::as_u64)
                    .collect();
                let value: Vec<Value> = ids.iter().map(|id| work_item(*id, 1, 0, &[])).collect();
                Reply::json(&json!({"count": value.len(), "value": value}))
            }
            other => panic!("unexpected request {other}"),
        }
    }

    #[tokio::test]
    async fn a_link_query_lists_both_ends_and_a_full_answer_is_refused_as_truncated() {
        let wiql = "SELECT [System.Id] FROM WorkItemLinks WHERE [Source].[System.TeamProject] = @project MODE (MustContain)";
        let source = AdoBoardsSource::new(
            config(),
            params(&json!({"wiql": wiql, "include_attachments": false})),
            allow(),
            Scripted::new(|request, _| link_reply(request, 3)),
        )
        .expect("source");
        let listed = source.list().await.expect("listing");
        let keys: Vec<&str> = listed.iter().map(|d| d.key.as_str()).collect();
        assert_eq!(keys, ["work_items/1.json", "work_items/2.json"]);

        // Exactly the ceiling of relations, which name only two work items:
        // the raw count says the answer was cut, the ids do not.
        let source = AdoBoardsSource::new(
            config(),
            params(&json!({"wiql": wiql})),
            allow(),
            Scripted::new(|request, _| link_reply(request, WIQL_CEILING)),
        )
        .expect("source");
        let error = source.list().await.expect_err("truncated");
        assert!(
            error
                .to_string()
                .contains("more work items than one answer holds"),
            "{error}"
        );
    }

    #[test]
    fn queries_are_windowed_without_breaking_their_where_clause() {
        assert_eq!(
            windowed(
                "SELECT [System.Id] FROM WorkItems WHERE [System.State] = 'Active' ORDER BY [System.ChangedDate] DESC",
                Some(10)
            ),
            "SELECT [System.Id] FROM WorkItems WHERE ([System.State] = 'Active') AND [System.Id] > 10 ORDER BY [System.Id]"
        );
        assert_eq!(
            windowed("select [System.Id] from WorkItems;", Some(5)),
            "select [System.Id] from WorkItems WHERE [System.Id] > 5 ORDER BY [System.Id]"
        );
        assert_eq!(
            windowed(
                "SELECT [System.Id] FROM WorkItems WHERE [System.State] = 'Active' OR [System.State] = 'New'",
                Some(7)
            ),
            "SELECT [System.Id] FROM WorkItems WHERE ([System.State] = 'Active' OR [System.State] = 'New') AND [System.Id] > 7 ORDER BY [System.Id]"
        );
        assert_eq!(
            windowed(
                "SELECT [System.Id] FROM WorkItems WHERE [System.State] = 'Active' OR [System.State] = 'New'",
                None
            ),
            "SELECT [System.Id] FROM WorkItems WHERE [System.State] = 'Active' OR [System.State] = 'New' ORDER BY [System.Id]"
        );
        // Keywords inside quotes and bracketed names are text, and
        // `ORDER   BY` may be spaced freely.
        assert_eq!(
            windowed(
                "SELECT [System.Id] FROM WorkItems WHERE [System.Title] CONTAINS 'the order by clause where' AND [Custom.Where] = 1 ORDER   BY [System.Id]",
                Some(3)
            ),
            "SELECT [System.Id] FROM WorkItems WHERE ([System.Title] CONTAINS 'the order by clause where' AND [Custom.Where] = 1) AND [System.Id] > 3 ORDER BY [System.Id]"
        );
        // ASOF stays, after the new ORDER BY, whether it followed or
        // preceded the original ORDER BY.
        assert_eq!(
            windowed(
                "SELECT [System.Id] FROM WorkItems WHERE [System.State] = 'Done' ORDER BY [System.Id] ASOF '2026-01-01'",
                Some(1)
            ),
            "SELECT [System.Id] FROM WorkItems WHERE ([System.State] = 'Done') AND [System.Id] > 1 ORDER BY [System.Id] ASOF '2026-01-01'"
        );
        assert_eq!(
            windowed(
                "SELECT [System.Id] FROM WorkItems WHERE [System.State] = 'Done' ASOF '2026-01-01' ORDER BY [System.Id]",
                None
            ),
            "SELECT [System.Id] FROM WorkItems WHERE [System.State] = 'Done' ORDER BY [System.Id] ASOF '2026-01-01'"
        );
        assert_eq!(
            windowed(
                "SELECT [System.Id] FROM WorkItems ASOF '2026-01-01'",
                Some(2)
            ),
            "SELECT [System.Id] FROM WorkItems WHERE [System.Id] > 2 ORDER BY [System.Id] ASOF '2026-01-01'"
        );
        assert!(is_link_query(
            "SELECT [System.Id] FROM workitemLinks WHERE ..."
        ));
        assert!(!is_link_query("SELECT [System.Id] FROM WorkItems"));
    }

    /// Calls for the comments of any work item.
    fn comment_calls(fixture: &Scripted) -> usize {
        fixture
            .seen()
            .iter()
            .filter(|s| s.contains("/comments"))
            .count()
    }

    #[tokio::test]
    async fn an_unchanged_run_makes_no_comment_call_and_a_moved_item_alone_is_read_again() {
        let items: Vec<(u64, u64, u64)> = vec![(1, 3, 2)];
        let wiql =
            json!({"wiql": "SELECT [System.Id] FROM WorkItems", "include_attachments": false});
        let first_items = items.clone();
        let first = Scripted::new(move |request, _| reply(request, &first_items));
        let source =
            AdoBoardsSource::new(config(), params(&wiql), allow(), first.clone()).expect("source");
        let listed = source.list().await.expect("first listing");
        assert_eq!(comment_calls(&first), 2, "a first run reads the discussion");
        let stored: BTreeMap<String, String> = listed
            .iter()
            .map(|d| (d.key.clone(), d.version.clone()))
            .collect();

        // The same tenant, the index holding what the first run listed.
        let second_items = items.clone();
        let second = Scripted::new(move |request, _| reply(request, &second_items));
        let source =
            AdoBoardsSource::new(config(), params(&wiql), allow(), second.clone()).expect("source");
        source.remember_versions(stored.clone());
        let again = source.list().await.expect("second listing");
        assert_eq!(comment_calls(&second), 0, "nothing moved: no comment call");
        assert_eq!(again, listed, "the listing is the same");

        // A comment was added: the count moved, so the discussion is read.
        let third = Scripted::new(move |request, _| phased_reply(request, 2));
        let source =
            AdoBoardsSource::new(config(), params(&wiql), allow(), third.clone()).expect("source");
        source.remember_versions(stored);
        let moved = source.list().await.expect("third listing");
        assert_eq!(comment_calls(&third), 1, "the moved item is read again");
        let version = |listing: &[DocumentRef]| {
            listing
                .iter()
                .find(|d| d.key.ends_with("comments.md"))
                .map(|d| d.version.clone())
                .expect("comments")
        };
        assert_ne!(version(&moved), version(&listed));

        // A field edit moves the change date, not the discussion: the
        // comments are read (the date is the only signal), the stored
        // version is kept.
        let edited = Scripted::new(move |request, _| {
            let answer = reply(request, &[(1, 4, 2)]);
            if request.url().path().ends_with("/workitemsbatch") {
                return Reply::json(&json!({"value": [{
                    "id": 1, "rev": 4,
                    "fields": {"System.Title": "Item 1", "System.WorkItemType": "Bug",
                               "System.CommentCount": 2, "System.ChangedDate": "2026-10-02T00:00:00Z"},
                }]}));
            }
            answer
        });
        let comments_of = |listing: &[DocumentRef]| version(listing);
        let source =
            AdoBoardsSource::new(config(), params(&wiql), allow(), edited.clone()).expect("source");
        source.remember_versions(
            listed
                .iter()
                .map(|d| (d.key.clone(), d.version.clone()))
                .collect(),
        );
        let after_edit = source.list().await.expect("fourth listing");
        assert_eq!(
            comment_calls(&edited),
            2,
            "the date moved: comments are read"
        );
        assert_eq!(
            comments_of(&after_edit),
            comments_of(&listed),
            "the discussion is unchanged: its version is kept"
        );
    }

    #[tokio::test]
    async fn a_vanished_work_item_is_omitted_and_the_rest_are_listed() {
        let fixture = Scripted::new(|request, _| {
            if request.url().path() == "/contoso/Fabrikam/_apis/wit/workitemsbatch" {
                let body = body_json(request);
                assert_eq!(body["errorPolicy"], "Omit");
                // Work item 2 was deleted after the query ran: the documented
                // `Omit` answer holds a null in its place.
                return Reply::json(&json!({"count": 3, "value": [
                    work_item(1, 3, 0, &[]), Value::Null, work_item(3, 5, 0, &[])
                ]}));
            }
            reply(request, &[(1, 3, 0), (2, 3, 0), (3, 5, 0)])
        });
        let source = AdoBoardsSource::new(
            config(),
            params(&json!({"wiql": "SELECT [System.Id] FROM WorkItems"})),
            allow(),
            fixture,
        )
        .expect("source");
        let listed = source.list().await.expect("listing");
        let keys: Vec<_> = listed.iter().map(|d| d.key.as_str()).collect();
        assert_eq!(keys, ["work_items/1.json", "work_items/3.json"]);
    }

    #[tokio::test]
    async fn an_attachment_url_outside_the_organization_gets_no_credential() {
        for foreign in [
            // Another organization on the shared host.
            "https://dev.azure.com/other/p/_apis/wit/attachments/a9",
            // A sibling whose name starts with ours.
            "https://dev.azure.com/contoso-evil/p/_apis/wit/attachments/a9",
            // Another host.
            "https://attacker.example/contoso/p/_apis/wit/attachments/a9",
        ] {
            let url = foreign.to_owned();
            let fixture = Scripted::new(move |request, _| {
                if request.url().path() == "/contoso/Fabrikam/_apis/wit/workitemsbatch" {
                    let mut item = work_item(1, 3, 0, &[]);
                    item["relations"] = json!([{"rel": "AttachedFile", "url": url,
                        "attributes": {"name": "x.pdf", "resourceSize": 1}}]);
                    return Reply::json(&json!({"count": 1, "value": [item]}));
                }
                reply(request, &[(1, 3, 0)])
            });
            let source = AdoBoardsSource::new(
                config(),
                params(&json!({"wiql": "SELECT [System.Id] FROM WorkItems"})),
                allow(),
                fixture.clone(),
            )
            .expect("source");
            let error = source.list().await.expect_err("refused");
            assert!(
                error.to_string().contains("organization"),
                "{foreign}: {error}"
            );
            assert!(
                fixture
                    .requests()
                    .iter()
                    .all(|r| r.url().path().starts_with("/contoso/Fabrikam/_apis/wit/")),
                "nothing was requested from {foreign}"
            );
        }

        // Defence in depth: the client itself never attaches the PAT to a
        // URL outside the organization.
        let (connection, _) = config().into_parts();
        let client = AdoClient::new(connection, Scripted::new(|_, _| panic!("no request")));
        for foreign in [
            "https://dev.azure.com/other/_apis/x",
            "https://dev.azure.com/contoso-evil/_apis/x",
            "http://dev.azure.com/contoso/_apis/x",
            "https://dev.azure.com:444/contoso/_apis/x",
        ] {
            let mut request = Request::new(Method::GET, Url::parse(foreign).expect("url"));
            assert!(client.authorize(&mut request).is_err(), "{foreign}");
            assert!(
                request.headers().get("authorization").is_none(),
                "{foreign}"
            );
        }
        let mut own = Request::new(
            Method::GET,
            Url::parse("https://dev.azure.com/Contoso/p/_apis/wit/attachments/a1").expect("url"),
        );
        client.authorize(&mut own).expect("inside the organization");
        assert!(own.headers().get("authorization").is_some());
    }

    const IMG_HTML: &str = r#"<div>Crash <img src="https://dev.azure.com/contoso/p-guid/_apis/wit/attachments/img1?fileName=shot.png" alt="shot"> then <img src="https://example.com/x.png" alt="ext"> and <img src="data:image/png;base64,AAAA" alt="inline"><img src="https://dev.azure.com/contoso/p-guid/_apis/wit/attachments/img2?fileName=gone.png"></div>"#;

    fn image_fixture() -> Arc<Scripted> {
        Scripted::new(|request, _| match request.url().path() {
            "/contoso/Fabrikam/_apis/wit/wiql" => Reply::json(&json!({"workItems": [{"id": 1}]})),
            "/contoso/Fabrikam/_apis/wit/workitemsbatch" => Reply::json(
                &json!({"value": [{"id": 1, "rev": 3, "fields": {"System.Title": "T", "System.CommentCount": 0}}]}),
            ),
            "/contoso/Fabrikam/_apis/wit/workitems/1" => Reply::json(&json!({
                "id": 1, "rev": 3,
                "fields": {"System.Title": "T", "System.WorkItemType": "Bug", "System.Description": IMG_HTML}
            })),
            "/contoso/p-guid/_apis/wit/attachments/img1" => Reply::bytes(b"PNGDATA-0123"),
            "/contoso/p-guid/_apis/wit/attachments/img2" => Reply::status(StatusCode::NOT_FOUND),
            other => panic!("unexpected request {other}"),
        })
    }

    #[tokio::test]
    async fn process_images_describes_img_tags_and_keeps_alt_text_for_the_rest() {
        let source = AdoBoardsSource::new(
            config(),
            params(&json!({"wiql": "SELECT [System.Id] FROM WorkItems", "process_images": true, "include_attachments": false, "include_comments": false})),
            allow(),
            image_fixture(),
        )
        .expect("source");
        source.list().await.expect("listing");
        assert!(matches!(
            source.picture_step("work_items/1.json"),
            Some(PictureStep::Inline { .. })
        ));
        let fetched = source.fetch("work_items/1.json").await.expect("fetch");
        let context = fetched.metadata.get(pictures::CONTEXT_KEY).cloned();
        assert!(source.may_have_pictures(context.as_ref()));
        let batch = source
            .pictures("work_items/1.json", &fetched.bytes, context.as_ref())
            .await
            .expect("batch");
        assert_eq!(batch.pictures.len(), 1);
        assert_eq!(&batch.pictures[0].bytes[..], b"PNGDATA-0123");
        assert_eq!(batch.pictures[0].name, "shot.png");
        assert_eq!(
            batch.skipped.get(UNSUPPORTED),
            Some(&2),
            "external and data: sources"
        );
        assert_eq!(batch.skipped.get(UNREADABLE), Some(&1), "the 404");
        let described = batch.render(&[Some("A \"crash\" dialog.\n\n  Red.")]);
        let value: Value = serde_json::from_str(&described).expect("still JSON");
        assert_eq!(
            value["fields"]["System.Description"],
            "Crash [image: A \"crash\" dialog.\nRed.] then [image: ext] and [image: inline]"
        );
        let plain: Value = serde_json::from_str(&batch.render(&[None])).expect("JSON");
        assert_eq!(
            plain["fields"]["System.Description"],
            "Crash [image: shot] then [image: ext] and [image: inline]",
            "the SDK keeps alt text where nothing was described"
        );
        // Off by default: the plain fetch is the SDK's `sanitize` without images.
        let off = AdoBoardsSource::new(
            config(),
            params(&json!({"wiql": "SELECT [System.Id] FROM WorkItems", "include_attachments": false, "include_comments": false})),
            allow(),
            image_fixture(),
        )
        .expect("source");
        off.list().await.expect("listing");
        assert!(off.picture_step("work_items/1.json").is_none());
        let fetched = off.fetch("work_items/1.json").await.expect("fetch");
        assert!(
            !fetched.metadata.contains_key(pictures::CONTEXT_KEY),
            "nothing is carried when the step is off"
        );
        assert!(!off.may_have_pictures(None));
        assert!(
            off.pictures("work_items/1.json", &fetched.bytes, None)
                .await
                .is_none()
        );
    }

    #[tokio::test]
    async fn unsanitised_fields_get_an_image_description_attribute() {
        let source = AdoBoardsSource::new(
            config(),
            params(&json!({"wiql": "SELECT [System.Id] FROM WorkItems", "process_images": true, "sanitize": false, "include_attachments": false, "include_comments": false})),
            allow(),
            image_fixture(),
        )
        .expect("source");
        source.list().await.expect("listing");
        let fetched = source.fetch("work_items/1.json").await.expect("fetch");
        let context = fetched.metadata.get(pictures::CONTEXT_KEY).cloned();
        let batch = source
            .pictures("work_items/1.json", &fetched.bytes, context.as_ref())
            .await
            .expect("batch");
        let value: Value =
            serde_json::from_str(&batch.render(&[Some("A \"x\" <b>")])).expect("still JSON");
        let html = value["fields"]["System.Description"]
            .as_str()
            .expect("html");
        assert!(
            html.contains(r#"<img src="https://dev.azure.com/contoso/p-guid/_apis/wit/attachments/img1?fileName=shot.png" alt="shot" image-description="A &quot;x&quot; &lt;b&gt;">"#),
            "{html}"
        );
        assert!(
            html.contains(r#"<img src="https://example.com/x.png" alt="ext">"#),
            "{html}"
        );
    }

    const TWICE_HTML: &str = r#"<p><img src="https://dev.azure.com/contoso/p-guid/_apis/wit/attachments/img1?fileName=shot.png" alt="first"> and <img src="https://dev.azure.com/contoso/p-guid/_apis/wit/attachments/img1?fileName=shot.png" alt="second"></p>"#;

    /// One work item whose description is `html`; the picture answers with
    /// `status` (or its bytes on 200). Counts every request.
    fn html_fixture(html: &'static str, status: StatusCode) -> Arc<Scripted> {
        Scripted::new(move |request, _| match request.url().path() {
            "/contoso/Fabrikam/_apis/wit/wiql" => Reply::json(&json!({"workItems": [{"id": 1}]})),
            "/contoso/Fabrikam/_apis/wit/workitemsbatch" => Reply::json(
                &json!({"value": [{"id": 1, "rev": 3, "fields": {"System.Title": "T", "System.CommentCount": 0}}]}),
            ),
            "/contoso/Fabrikam/_apis/wit/workitems/1" => Reply::json(&json!({
                "id": 1, "rev": 3,
                "fields": {"System.Title": "T", "System.WorkItemType": "Bug", "System.Description": html}
            })),
            "/contoso/p-guid/_apis/wit/attachments/img1" if status == StatusCode::OK => {
                Reply::bytes(b"PNGDATA-0123")
            }
            "/contoso/p-guid/_apis/wit/attachments/img1" => Reply::status(status),
            other => panic!("unexpected request {other}"),
        })
    }

    async fn pictured(
        html: &'static str,
        status: StatusCode,
    ) -> (Option<PictureBatch>, Arc<Scripted>) {
        let fixture = html_fixture(html, status);
        let source = AdoBoardsSource::new(
            config(),
            params(&json!({"wiql": "SELECT [System.Id] FROM WorkItems", "process_images": true, "include_attachments": false, "include_comments": false})),
            allow(),
            fixture.clone(),
        )
        .expect("source");
        source.list().await.expect("listing");
        let fetched = source.fetch("work_items/1.json").await.expect("fetch");
        let context = fetched.metadata.get(pictures::CONTEXT_KEY).cloned();
        let batch = source
            .pictures("work_items/1.json", &fetched.bytes, context.as_ref())
            .await;
        (batch, fixture)
    }

    #[tokio::test]
    async fn the_picture_pass_reads_no_work_item_again() {
        let (batch, fixture) = pictured(TWICE_HTML, StatusCode::OK).await;
        assert!(batch.is_some());
        let seen = fixture.seen();
        assert_eq!(
            seen.iter().filter(|s| s.contains("/workitems/1?")).count(),
            1,
            "{seen:?}"
        );
    }

    #[tokio::test]
    async fn every_img_tag_keeps_its_own_markup_and_equal_bytes_cost_one_read() {
        let (batch, fixture) = pictured(TWICE_HTML, StatusCode::OK).await;
        let batch = batch.expect("batch");
        assert_eq!(batch.pictures.len(), 2, "one picture per <img>");
        assert_ne!(batch.pictures[0].token, batch.pictures[1].token);
        assert_eq!(batch.pictures[0].bytes, batch.pictures[1].bytes);
        let seen = fixture.seen();
        assert_eq!(
            seen.iter()
                .filter(|s| s.contains("/attachments/img1"))
                .count(),
            1,
            "the file is downloaded once"
        );
        let rendered: Value =
            serde_json::from_str(&batch.render(&[Some("one"), None])).expect("JSON");
        assert_eq!(
            rendered["fields"]["System.Description"], "[image: one] and [image: second]",
            "the second keeps its own alt text"
        );
    }

    #[tokio::test]
    async fn a_read_failure_is_retryable_only_when_another_run_cures_it() {
        let reason = |status| async move {
            let (batch, _) = pictured(TWICE_HTML, status).await;
            let batch = batch.expect("a counted skip");
            assert_eq!(batch.pictures, []);
            let json = String::from_utf8(
                // The text stays as it was fetched.
                batch.template.clone().into_bytes(),
            )
            .expect("utf8");
            assert!(json.contains("System.Description"), "{json}");
            batch.skipped.into_iter().collect::<Vec<_>>()
        };
        assert_eq!(
            reason(StatusCode::SERVICE_UNAVAILABLE).await,
            [(pictures::FETCH_FAILED, 1)]
        );
        assert_eq!(
            reason(StatusCode::TOO_MANY_REQUESTS).await,
            [(pictures::RATE_LIMITED, 1)]
        );
        assert_eq!(
            reason(StatusCode::FORBIDDEN).await,
            [(pictures::FORBIDDEN, 1)]
        );
        assert_eq!(
            reason(StatusCode::UNAUTHORIZED).await,
            [(pictures::FORBIDDEN, 1)]
        );
        assert_eq!(reason(StatusCode::NOT_FOUND).await, [(UNREADABLE, 1)]);
    }

    #[tokio::test]
    async fn two_hundred_tags_of_one_big_image_share_one_buffer() {
        let five = usize::try_from(pictures::MAX_PICTURE_BYTES).unwrap();
        let tag = r#"<img src="https://dev.azure.com/contoso/p-guid/_apis/wit/attachments/img1?fileName=shot.png" alt="s">"#;
        let html: &'static str = Box::leak(tag.repeat(250).into_boxed_str());
        let fixture = Scripted::new(move |request, _| match request.url().path() {
            "/contoso/Fabrikam/_apis/wit/wiql" => Reply::json(&json!({"workItems": [{"id": 1}]})),
            "/contoso/Fabrikam/_apis/wit/workitemsbatch" => Reply::json(
                &json!({"value": [{"id": 1, "rev": 3, "fields": {"System.Title": "T", "System.CommentCount": 0}}]}),
            ),
            "/contoso/Fabrikam/_apis/wit/workitems/1" => Reply::json(&json!({
                "id": 1, "rev": 3,
                "fields": {"System.Title": "T", "System.WorkItemType": "Bug", "System.Description": html}
            })),
            "/contoso/p-guid/_apis/wit/attachments/img1" => Reply::bytes(&vec![9; five]),
            other => panic!("unexpected request {other}"),
        });
        let source = AdoBoardsSource::new(
            config(),
            params(&json!({"wiql": "SELECT [System.Id] FROM WorkItems", "process_images": true, "include_attachments": false, "include_comments": false})),
            allow(),
            fixture.clone(),
        )
        .expect("source");
        source.list().await.expect("listing");
        let fetched = source.fetch("work_items/1.json").await.expect("fetch");
        let context = fetched.metadata.get(pictures::CONTEXT_KEY).cloned();
        let batch = source
            .pictures("work_items/1.json", &fetched.bytes, context.as_ref())
            .await
            .expect("batch");
        assert_eq!(batch.pictures.len(), pictures::MAX_OCCURRENCES);
        let first = batch.pictures[0].bytes.as_ptr();
        assert!(
            batch.pictures.iter().all(|p| p.bytes.as_ptr() == first),
            "one buffer"
        );
        assert_eq!(
            fixture
                .seen()
                .iter()
                .filter(|s| s.contains("attachments/img1"))
                .count(),
            1
        );
    }

    #[tokio::test]
    async fn a_picture_that_was_read_but_not_described_reads_like_one_never_read() {
        const READ: &str = r#"<p>Look <img src="https://dev.azure.com/contoso/p-guid/_apis/wit/attachments/img1?fileName=shot.png" alt="A &amp; B &lt;x&gt;  two   spaces"> end</p>"#;
        const UNREAD: &str = r#"<p>Look <img src="https://example.com/x.png" alt="A &amp; B &lt;x&gt;  two   spaces"> end</p>"#;
        let (read, _) = pictured(READ, StatusCode::OK).await;
        let (unread, _) = pictured(UNREAD, StatusCode::OK).await;
        let read: Value = serde_json::from_str(&read.expect("read").render(&[None])).expect("JSON");
        let unread: Value =
            serde_json::from_str(&unread.expect("unread").render(&[])).expect("JSON");
        assert_eq!(
            read["fields"]["System.Description"],
            unread["fields"]["System.Description"]
        );
    }

    #[tokio::test]
    async fn nothing_about_a_work_item_outlives_its_document() {
        // The step on, a picture present: the context is in the fetched
        // document's metadata and nowhere else; with the step off, nowhere.
        let fixture = html_fixture(TWICE_HTML, StatusCode::OK);
        let source = AdoBoardsSource::new(
            config(),
            params(&json!({"wiql": "SELECT [System.Id] FROM WorkItems", "process_images": true, "include_attachments": false, "include_comments": false})),
            allow(),
            fixture,
        )
        .expect("source");
        source.list().await.expect("listing");
        let mut fetched = source.fetch("work_items/1.json").await.expect("fetch");
        assert!(fetched.metadata.remove(pictures::CONTEXT_KEY).is_some());
        // Taking it off (as the run does) leaves a source that can hand
        // nothing out: a second fetch carries its own.
        assert!(!source.may_have_pictures(fetched.metadata.get(pictures::CONTEXT_KEY)));
    }
}

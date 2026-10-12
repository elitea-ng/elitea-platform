//! Azure DevOps work items (the `ado_boards` family) as a `ContentSource`
//! (ADR-0030 I2).
//!
//! * **List.** The `wiql` query runs in windows of 2 000 ids
//!   (`AND [System.Id] > last ORDER BY [System.Id]`), so a query matching
//!   more than the 20 000 ids one WIQL answer may hold is still listed
//!   completely; a link query (`FROM WorkItemLinks`) cannot be windowed and
//!   is read in one answer. The ids are then read in batches of 200
//!   (`workitemsbatch`, `$expand=relations`) for their revision, comment
//!   count and attachments.
//! * **Version.** `{System.Rev}.{System.CommentCount}`: the revision moves
//!   on every field, relation and attachment change, the comment count on a
//!   discussion the revision may not record. The SDK compares
//!   `System.ChangedDate`.
//! * **Dependents.** Every `AttachedFile` relation is a document
//!   (`work_items/{id}/attachments/{name}`) and, with `include_comments`
//!   (on by default), the discussion is one document
//!   (`work_items/{id}/comments.md`). Both carry the parent's version and a
//!   `parent_key`, so a changed work item re-indexes its group (ADR-0030
//!   "Dependent documents"). The SDK indexes attachments and no comments.
//! * **Fetch.** A work item is `workitems/{id}?$expand=all`, its fields
//!   sanitised (HTML to text, identities to display names, as the SDK's
//!   `sanitize`) and written as JSON, keeping `fields` when given; an
//!   attachment is its relation URL (same organization host), under the
//!   document cap; comments are paged by continuation token.

use std::collections::BTreeMap;
use std::fmt::Write as _;
use std::sync::{Arc, Mutex};

use elitea_content_source::{ContentSource, Document, DocumentRef, SourceError};
use serde_json::{Map, Value, json};

use super::client::{AdoClient, AdoRequest, AdoScope};
use super::config::AdoToolkitConfig;
use super::text::{
    html_to_text, optional_bool, optional_text, optional_text_list, safe_name, sanitize_field,
};
use crate::egress::HostAllowlist;
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
const BATCH: usize = 200;
/// Comments per page (the server's maximum).
const COMMENTS_PAGE: usize = 200;
const MAX_WIQL_BYTES: usize = 32 * 1_024;
const ATTACHED_FILE: &str = "AttachedFile";
const KEY_PREFIX: &str = "work_items";

/// The `index_data` arguments of the `ado_boards` family
/// (`ado/work_item/ado_wrapper.py::_index_tool_params`), plus
/// `include_comments` (this connector's; see the module notes).
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct AdoBoardsIndexParams {
    pub wiql: String,
    /// The fields kept in the document; empty keeps every field.
    pub fields: Vec<String>,
    pub sanitize: bool,
    pub include_attachments: bool,
    pub include_comments: bool,
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
            let mut ids = Vec::new();
            for relation in body
                .get("workItemRelations")
                .and_then(Value::as_array)
                .ok_or(Failure::InvalidResponse(
                    "a link query answer has no workItemRelations",
                ))?
            {
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
            if ids.len() >= WIQL_CEILING {
                return Err(Failure::InvalidResponse(
                    "the link query matches more work items than one answer holds; narrow it",
                ));
            }
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
        let organization_host = Url::parse(self.client.organization_text())
            .ok()
            .and_then(|url| url.host_str().map(str::to_owned));
        let mut listed = Vec::new();
        let mut extras = BTreeMap::new();
        let segments = ["wit", "workitemsbatch"];
        for batch in ids.chunks(BATCH) {
            let request = self.build(
                AdoRequest::new(Method::POST, AdoScope::Project, &segments, WORK_ITEMS_API)
                    .json(json!({"ids": batch, "$expand": "relations"})),
            )?;
            let (_, body) = self.http.json(request).await?;
            let items = body
                .get("value")
                .and_then(Value::as_array)
                .ok_or(Failure::InvalidResponse("a batch answer has no value"))?;
            for item in items {
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
                let version = format!("{rev}.{comments}");
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
                    extras.insert(
                        key.clone(),
                        Extra::Comments {
                            id,
                            title: title.clone(),
                        },
                    );
                    listed.push(Listed::new(key, version.clone(), 0, id.to_string()));
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
                        if url.scheme() != "https"
                            || url.host_str().map(str::to_owned) != organization_host
                        {
                            return Err(Failure::InvalidResponse(
                                "an attachment URL is not on the organization's host",
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
                        let mut key = format!("{KEY_PREFIX}/{id}/attachments/{name}");
                        if extras.contains_key(&key) {
                            let guid = url
                                .path_segments()
                                .and_then(|mut s| s.next_back())
                                .unwrap_or("dup");
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
                        listed.push(Listed::new(key, version.clone(), size, id.to_string()));
                    }
                }
                self.http.check_count(listed.len())?;
            }
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

    /// The work item's fields as the document: filtered to `fields` when
    /// given, sanitised when asked, as pretty JSON.
    async fn work_item(
        &self,
        id: u64,
        listed_rev: u64,
    ) -> Result<(Vec<u8>, Map<String, Value>), Failure> {
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
        let fields = body
            .get("fields")
            .and_then(Value::as_object)
            .ok_or(Failure::InvalidResponse("the work item has no fields"))?;
        let mut kept = Map::new();
        for (name, value) in fields {
            if !self.params.fields.is_empty() && !self.params.fields.iter().any(|f| f == name) {
                continue;
            }
            let value = if self.params.sanitize {
                sanitize_field(value)
            } else {
                value.clone()
            };
            kept.insert(name.clone(), value);
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

    /// The discussion as markdown, every page followed.
    async fn comments(&self, id: u64) -> Result<Vec<u8>, Failure> {
        let id_text = id.to_string();
        let segments = ["wit", "workItems", id_text.as_str(), "comments"];
        let mut out = String::new();
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
            }
            continuation = body
                .get("continuationToken")
                .and_then(Value::as_str)
                .filter(|token| !token.is_empty())
                .map(ToOwned::to_owned);
            if continuation.is_none() {
                return Ok(out.into_bytes());
            }
        }
    }

    async fn attachment(&self, url: &Url) -> Result<Vec<u8>, Failure> {
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
        self.http.raw(request).await
    }
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
/// without its `ORDER BY`, the window condition added to (or as) its `WHERE`.
fn windowed(query: &str, after: Option<u64>) -> String {
    let trimmed = query.trim().trim_end_matches(';').trim();
    let lower = trimmed.to_ascii_lowercase();
    let base = match find_keyword(&lower, "order by") {
        Some(index) => trimmed[..index].trim_end(),
        None => trimmed,
    };
    let mut out = base.to_owned();
    if let Some(after) = after {
        if find_keyword(&lower[..base.len()], "where").is_some() {
            let _ = write!(out, " AND [System.Id] > {after}");
        } else {
            let _ = write!(out, " WHERE [System.Id] > {after}");
        }
    }
    out.push_str(" ORDER BY [System.Id]");
    out
}

/// The byte index of `keyword` in `lower` as a whole word (space-bounded).
fn find_keyword(lower: &str, keyword: &str) -> Option<usize> {
    let mut from = 0;
    while let Some(found) = lower[from..].find(keyword) {
        let index = from + found;
        let before = lower[..index].chars().next_back();
        let after = lower[index + keyword.len()..].chars().next();
        if before.is_none_or(char::is_whitespace) && after.is_none_or(char::is_whitespace) {
            return Some(index);
        }
        from = index + keyword.len();
    }
    None
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
            Extra::Comments { id, title } => {
                let bytes = self.comments(*id).await.map_err(failed)?;
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
                let bytes = self.attachment(url).await.map_err(failed)?;
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

    const ORG: &str = "https://dev.azure.com/contoso";

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
                       "attributes": {"name": name, "resourceSize": size}})
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

    fn reply(request: &Request, items: &[(u64, u64, u64)]) -> Reply {
        let path = request.url().path();
        match path {
            "/contoso/Fabrikam/_apis/wit/wiql" => {
                let body = body_json(request);
                let query_text = body["query"].as_str().expect("query");
                assert!(query_text.ends_with("ORDER BY [System.Id]"), "{query_text}");
                let after: u64 = query_text
                    .split("[System.Id] > ")
                    .nth(1)
                    .and_then(|rest| rest.split_whitespace().next())
                    .and_then(|n| n.parse().ok())
                    .unwrap_or(0);
                assert_eq!(query(request, "$top").as_deref(), Some("2000"));
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
                    None => Reply::json(&json!({"comments": [
                        {"text": "<p>First</p>", "createdBy": {"displayName": "Ada"}, "createdDate": "2026-10-01T01:00:00Z"}
                    ], "continuationToken": "c2"})),
                    Some("c2") => Reply::json(&json!({"comments": [
                        {"text": "Second", "createdBy": {"displayName": "Bob"}, "createdDate": "2026-10-01T02:00:00Z"}
                    ]})),
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
        let first: Vec<_> = listed
            .iter()
            .filter(|d| d.key.starts_with("work_items/1/") || d.key == "work_items/1.json")
            .map(|d| (d.key.as_str(), d.version.as_str(), d.size))
            .collect();
        assert_eq!(
            first,
            [
                ("work_items/1.json", "3.1", 0),
                ("work_items/1/attachments/a2-spec.pdf", "3.1", 6),
                ("work_items/1/attachments/spec.pdf", "3.1", 5),
                ("work_items/1/comments.md", "3.1", 0),
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
        let attachment = source
            .fetch("work_items/1/attachments/a2-spec.pdf")
            .await
            .expect("attachment");
        assert_eq!(attachment.bytes, b"%PDF-2");
        assert_eq!(attachment.metadata["parent_key"], "work_items/1.json");
        assert_eq!(attachment.title, "spec.pdf");
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

    #[test]
    fn queries_are_windowed_without_breaking_their_where_clause() {
        assert_eq!(
            windowed(
                "SELECT [System.Id] FROM WorkItems WHERE [System.State] = 'Active' ORDER BY [System.ChangedDate] DESC",
                Some(10)
            ),
            "SELECT [System.Id] FROM WorkItems WHERE [System.State] = 'Active' AND [System.Id] > 10 ORDER BY [System.Id]"
        );
        assert_eq!(
            windowed("select [System.Id] from WorkItems;", Some(5)),
            "select [System.Id] from WorkItems WHERE [System.Id] > 5 ORDER BY [System.Id]"
        );
        assert_eq!(
            windowed(
                "SELECT [System.Id] FROM WorkItems WHERE [System.Title] CONTAINS 'order by'",
                None
            ),
            "SELECT [System.Id] FROM WorkItems WHERE [System.Title] CONTAINS 'order by' ORDER BY [System.Id]"
        );
        assert!(is_link_query(
            "SELECT [System.Id] FROM workitemLinks WHERE ..."
        ));
        assert!(!is_link_query("SELECT [System.Id] FROM WorkItems"));
    }
}

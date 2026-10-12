//! Confluence as a `ContentSource` (ADR-0030 decision 3, stage I2).
//!
//! * **List.** Pages by the toolkit's space (`/rest/api/content?spaceKey=`),
//!   or — when `index_data` selects them — by `cql`, by `label` and by
//!   `page_ids`, deduplicated by id; the SDK ignores the space whenever one of
//!   those is given, and so does this connector. Every listing follows
//!   `start`/`limit` until the API stops answering with more (a Server
//!   instance caps `limit` below what is asked, so a short page is the end
//!   only when no `_links.next` is sent). There is no page cap: the SDK's
//!   `limit` and `max_pages` arguments are accepted and ignored, and the
//!   source's document cap bounds the run.
//! * **Dependents.** With `include_attachments`, each page's attachments
//!   (`/child/attachment`, filtered by `include_extensions` and
//!   `skip_extensions`) are listed as documents of their own under the
//!   page's key; with `include_comments`, so are its comments
//!   (`/child/comment`, every depth). Each dependent carries `parent_key`
//!   and has its own version, so a re-uploaded attachment or an edited
//!   comment rewrites only its own chunks. The SDK instead appends comment
//!   text to the page and gives every dependent the page's `updated_on`.
//! * **Keys and versions.** A page is `page/{id}.html`, versioned by its
//!   `version.number`; an attachment is
//!   `page/{id}/attachment/{attachment id}/{file name}` (the file name gives
//!   the media type), versioned by its `version.number`; a comment is
//!   `page/{id}/comment/{comment id}.html`, versioned by its
//!   `version.number`. The SDK keys a page by its id alone.
//! * **Fetch.** A page's body in the asked `content_format` (HTML; the
//!   worker's extraction turns it into markdown) with inline base64 images
//!   removed, pinned to the listed version: a page or comment whose
//!   `version.number` moved since the listing is `SourceError::Changed`. An
//!   attachment is downloaded through its listed `_links.download`, which
//!   names the listed version.
//! * **Restrictions.** Without `include_restricted_content`, a page with read
//!   restrictions is left out (the SDK's `is_public_page`). With it, the
//!   restriction's users and groups are recorded as the document's
//!   [`Acl::Restricted`] principals, which its dependents inherit.
//! * **Auth and limits.** The credential and custom headers come from the
//!   toolkit configuration ([`super::config`]); a 401 or 403 on a listing is
//!   an error naming the credential, never an empty listing; a 429 (or a
//!   `Retry-After`) backs off within the shared bound.

use std::collections::BTreeMap;
use std::sync::{Arc, Mutex};

use elitea_content_source::{
    Acl, ContentSource, Document, DocumentRef, Principal, PrincipalKind, SourceError,
};
use serde_json::{Map, Value};

use super::config::{ConfluenceConnectorConfig, ConfluenceIndexArguments, PROVIDER};
use crate::atlassian::{
    attachment_passes, id_of, safe_identifier, size_of, strip_base64_images, text_of,
};
use crate::egress::HostAllowlist;
use crate::source::{
    Backoff, Failure, Listed, ListingCache, SourceHttp, SourceLimits, Timeouts, references, web_url,
};
use crate::transport::{Method, Request, StatusCode, Transport, Url};

/// Results asked per listing page (Confluence caps at 100, or lower).
const PAGE_SIZE: usize = 100;
/// The media type a page or comment body is handed over as.
const HTML: &str = "text/html";

/// What the listing learnt about a document beyond its reference: the
/// metadata its fetch reports, and for an attachment the download path.
type Details = BTreeMap<String, Map<String, Value>>;

/// One Confluence toolkit's pages, attachments and comments.
pub struct ConfluenceSource {
    config: ConfluenceConnectorConfig,
    arguments: ConfluenceIndexArguments,
    http: SourceHttp,
    cache: ListingCache,
    details: Mutex<Arc<Details>>,
}

impl ConfluenceSource {
    /// The toolkit `config` indexed with the `index_data` `arguments`, every
    /// request through `allowlist` (fail-closed) and then `transport`.
    ///
    /// # Errors
    ///
    /// The Confluence host is not on the allowlist, or neither the toolkit's
    /// space nor `cql`, `label` or `page_ids` selects any page.
    pub fn new(
        config: ConfluenceConnectorConfig,
        arguments: ConfluenceIndexArguments,
        allowlist: HostAllowlist,
        transport: Arc<dyn Transport>,
    ) -> Result<Self, SourceError> {
        let http = SourceHttp::new(allowlist, transport);
        if !http.permits(config.api_root()) {
            return Err(Failure::Refused.into_source_error(PROVIDER, None));
        }
        if !arguments.selects_pages() && config.space().is_none() {
            return Err(SourceError::Unavailable(
                "Confluence: specify at least one of the toolkit's `space`, or `cql`, `label` or `page_ids`"
                    .to_owned(),
            ));
        }
        Ok(Self {
            config,
            arguments,
            http,
            cache: ListingCache::default(),
            details: Mutex::new(Arc::new(Details::new())),
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

    /// `{api_root}/rest/api/{segments...}`.
    fn api(&self, segments: &[&str]) -> Result<Url, Failure> {
        let mut all = vec!["rest", "api"];
        all.extend_from_slice(segments);
        web_url(self.config.api_root(), all).ok_or(Failure::Client(
            "the Confluence API root cannot carry a path".to_owned(),
        ))
    }

    fn get(&self, url: Url) -> Result<Request, Failure> {
        let mut request = Request::new(Method::GET, url);
        self.config
            .common()
            .authorize(&mut request)
            .map_err(|error| Failure::Client(error.to_string()))?;
        Ok(request)
    }

    /// A listing page; a refused credential is named, not folded into "HTTP
    /// 401".
    async fn json(&self, url: Url) -> Result<Value, Failure> {
        match self.http.json(self.get(url)?).await {
            Ok((_, value)) => Ok(value),
            Err(Failure::Status(status @ (StatusCode::UNAUTHORIZED | StatusCode::FORBIDDEN))) => {
                Err(Failure::Client(format!(
                    "Confluence refused the credential (HTTP {}); check the toolkit's token or API key",
                    status.as_u16()
                )))
            }
            Err(failure) => Err(failure),
        }
    }

    /// Every result of a `start`/`limit` listing at `url` (its own query
    /// kept), under the page cap.
    async fn paginate(&self, url: &Url, pages_seen: &mut usize) -> Result<Vec<Value>, Failure> {
        let mut results = Vec::new();
        let mut start = 0_usize;
        loop {
            *pages_seen += 1;
            self.http.check_pages(*pages_seen)?;
            let mut page_url = url.clone();
            page_url
                .query_pairs_mut()
                .append_pair("start", &start.to_string())
                .append_pair("limit", &PAGE_SIZE.to_string());
            let page = self.json(page_url).await?;
            let batch = match page.get("results") {
                None | Some(Value::Null) => Vec::new(),
                Some(Value::Array(items)) => items.clone(),
                Some(_) => return Err(Failure::InvalidResponse("results is not a list")),
            };
            if batch.is_empty() {
                return Ok(results);
            }
            let has_next = page
                .pointer("/_links/next")
                .is_some_and(|next| !next.is_null());
            let asked = page
                .get("limit")
                .and_then(Value::as_u64)
                .map_or(PAGE_SIZE, |limit| {
                    usize::try_from(limit).unwrap_or(PAGE_SIZE)
                });
            start += batch.len();
            let short = batch.len() < asked.min(PAGE_SIZE);
            results.extend(batch);
            self.http.check_count(results.len())?;
            if !has_next && short {
                return Ok(results);
            }
        }
    }

    /// The `expand` every page listing asks for: the version, the labels
    /// when wanted, the read restrictions when they decide inclusion or the
    /// ACL.
    fn page_expand(&self) -> String {
        let mut expand = vec!["version"];
        if self.arguments.include_labels {
            expand.push("metadata.labels");
        }
        expand.push("restrictions.read.restrictions.user");
        expand.push("restrictions.read.restrictions.group");
        expand.join(",")
    }

    /// The pages the run selects, by id, in listing order.
    async fn pages(&self, pages_seen: &mut usize) -> Result<Vec<Value>, Failure> {
        fn keep(
            by_id: &mut BTreeMap<String, Value>,
            order: &mut Vec<String>,
            page: Value,
        ) -> Result<(), Failure> {
            let id = id_of(&page, "/id").ok_or(Failure::InvalidResponse("a page has no id"))?;
            if !safe_identifier(&id) {
                return Err(Failure::InvalidResponse("a page id is not an identifier"));
            }
            if by_id.insert(id.clone(), page).is_none() {
                order.push(id);
            }
            Ok(())
        }
        let expand = self.page_expand();
        let mut by_id: BTreeMap<String, Value> = BTreeMap::new();
        let mut order: Vec<String> = Vec::new();
        if self.arguments.selects_pages() {
            if let Some(cql) = &self.arguments.cql {
                let mut url = self.api(&["content", "search"])?;
                url.query_pairs_mut()
                    .append_pair("cql", cql)
                    .append_pair("expand", &expand);
                if self.arguments.include_archived_content {
                    url.query_pairs_mut()
                        .append_pair("includeArchivedSpaces", "true");
                }
                for page in self.paginate(&url, pages_seen).await? {
                    keep(&mut by_id, &mut order, page)?;
                }
            }
            if let Some(label) = &self.arguments.label {
                let mut url = self.api(&["content", "search"])?;
                url.query_pairs_mut()
                    .append_pair(
                        "cql",
                        &format!("type=page AND label=\"{}\"", escape_cql(label)),
                    )
                    .append_pair("expand", &expand);
                for page in self.paginate(&url, pages_seen).await? {
                    keep(&mut by_id, &mut order, page)?;
                }
            }
            for id in &self.arguments.page_ids {
                // Already listed by `cql` or `label`: the SDK unions the ids.
                if by_id.contains_key(id) {
                    continue;
                }
                *pages_seen += 1;
                self.http.check_pages(*pages_seen)?;
                let mut url = self.api(&["content", id])?;
                url.query_pairs_mut().append_pair("expand", &expand);
                let page = match self.json(url).await {
                    Ok(page) => page,
                    Err(Failure::Status(StatusCode::NOT_FOUND)) => {
                        return Err(Failure::Client(format!(
                            "the Confluence page '{id}' named by `page_ids` was not found or is not readable"
                        )));
                    }
                    Err(failure) => return Err(failure),
                };
                keep(&mut by_id, &mut order, page)?;
            }
        } else {
            let space = self
                .config
                .space()
                .ok_or(Failure::Client("the toolkit names no space".to_owned()))?;
            let mut url = self.api(&["content"])?;
            url.query_pairs_mut()
                .append_pair("spaceKey", space)
                .append_pair("type", "page")
                .append_pair(
                    "status",
                    if self.arguments.include_archived_content {
                        "any"
                    } else {
                        "current"
                    },
                )
                .append_pair("expand", &expand);
            for page in self.paginate(&url, pages_seen).await? {
                keep(&mut by_id, &mut order, page)?;
            }
        }
        Ok(order
            .into_iter()
            .filter_map(|id| by_id.remove(&id))
            .collect())
    }

    async fn listing(&self) -> Result<(Vec<Listed>, Details), Failure> {
        let mut pages_seen = 0_usize;
        let mut listed = Vec::new();
        let mut details = Details::new();
        for page in self.pages(&mut pages_seen).await? {
            let id = id_of(&page, "/id").ok_or(Failure::InvalidResponse("a page has no id"))?;
            let acl = match read_restrictions(&page) {
                None => Acl::Project,
                Some(_) if !self.arguments.include_restricted_content => continue,
                Some(principals) => Acl::Restricted { principals },
            };
            let version = version_of(&page)
                .ok_or(Failure::InvalidResponse("a page carries no version number"))?;
            let key = format!("page/{id}.html");
            let mut page_details = Map::new();
            page_details.insert("id".into(), Value::String(id.clone()));
            page_details.insert("type".into(), Value::String("page".into()));
            if let Some(title) = text_of(&page, "/title") {
                page_details.insert("title".into(), Value::String(title.to_owned()));
            }
            if let Some(status) = text_of(&page, "/status") {
                page_details.insert("status".into(), Value::String(status.to_owned()));
            }
            if let Some(webui) = text_of(&page, "/_links/webui") {
                page_details.insert("url".into(), Value::String(self.page_url(webui)));
            }
            if self.arguments.include_labels {
                page_details.insert("labels".into(), labels_of(&page));
            }
            let mut item = Listed::new(key.clone(), version, 0, id.clone());
            HTML.clone_into(&mut item.reference.mime);
            item.reference.acl = acl.clone();
            listed.push(item);
            self.http.check_count(listed.len())?;
            if self.arguments.include_attachments {
                self.list_attachments(&id, &key, &acl, &mut pages_seen, &mut listed, &mut details)
                    .await?;
            }
            if self.arguments.include_comments {
                self.list_comments(&id, &key, &acl, &mut pages_seen, &mut listed, &mut details)
                    .await?;
            }
            details.insert(key, page_details);
        }
        Ok((listed, details))
    }

    #[allow(clippy::too_many_arguments)]
    async fn list_attachments(
        &self,
        page_id: &str,
        page_key: &str,
        acl: &Acl,
        pages_seen: &mut usize,
        listed: &mut Vec<Listed>,
        details: &mut Details,
    ) -> Result<(), Failure> {
        let mut url = self.api(&["content", page_id, "child", "attachment"])?;
        url.query_pairs_mut()
            .append_pair("expand", "version,metadata.labels");
        for attachment in self.paginate(&url, pages_seen).await? {
            let id = id_of(&attachment, "/id")
                .ok_or(Failure::InvalidResponse("an attachment has no id"))?;
            if !safe_identifier(&id) {
                return Err(Failure::InvalidResponse(
                    "an attachment id is not an identifier",
                ));
            }
            let title = text_of(&attachment, "/title").unwrap_or("attachment");
            if !attachment_passes(
                title,
                &self.arguments.include_extensions,
                &self.arguments.skip_extensions,
            ) {
                continue;
            }
            let download = text_of(&attachment, "/_links/download")
                .filter(|path| path.starts_with('/'))
                .ok_or(Failure::InvalidResponse(
                    "an attachment carries no download link",
                ))?;
            let size = size_of(&attachment, "/extensions/fileSize");
            let version = version_of(&attachment).unwrap_or_else(|| format!("size:{size}"));
            let key = format!("page/{page_id}/attachment/{id}/{}", key_segment(title));
            let mut item = Listed::new(key.clone(), version, size, download.to_owned());
            item.reference.acl = acl.clone();
            listed.push(item);
            self.http.check_count(listed.len())?;
            let mut attachment_details = Map::new();
            attachment_details.insert("id".into(), Value::String(id));
            attachment_details.insert("type".into(), Value::String("attachment".into()));
            attachment_details.insert("name".into(), Value::String(title.to_owned()));
            attachment_details.insert("title".into(), Value::String(title.to_owned()));
            attachment_details.insert("page_id".into(), Value::String(page_id.to_owned()));
            attachment_details.insert("parent_key".into(), Value::String(page_key.to_owned()));
            attachment_details.insert("parent_id".into(), Value::String(page_id.to_owned()));
            if size > 0 {
                attachment_details.insert("size".into(), Value::from(size));
            }
            if let Some(media_type) = text_of(&attachment, "/metadata/mediaType") {
                attachment_details
                    .insert("media_type".into(), Value::String(media_type.to_owned()));
            }
            if let Some(creator) = text_of(&attachment, "/version/by/displayName") {
                attachment_details.insert("creator".into(), Value::String(creator.to_owned()));
            }
            if let Some(when) = text_of(&attachment, "/version/when") {
                attachment_details.insert("updated".into(), Value::String(when.to_owned()));
            }
            attachment_details.insert("labels".into(), labels_of(&attachment));
            attachment_details.insert(
                "download_url".into(),
                Value::String(format!(
                    "{}{download}",
                    self.config.api_root().as_str().trim_end_matches('/')
                )),
            );
            details.insert(key, attachment_details);
        }
        Ok(())
    }

    #[allow(clippy::too_many_arguments)]
    async fn list_comments(
        &self,
        page_id: &str,
        page_key: &str,
        acl: &Acl,
        pages_seen: &mut usize,
        listed: &mut Vec<Listed>,
        details: &mut Details,
    ) -> Result<(), Failure> {
        let mut url = self.api(&["content", page_id, "child", "comment"])?;
        url.query_pairs_mut()
            .append_pair("expand", "version")
            .append_pair("depth", "all");
        for comment in self.paginate(&url, pages_seen).await? {
            let id =
                id_of(&comment, "/id").ok_or(Failure::InvalidResponse("a comment has no id"))?;
            if !safe_identifier(&id) {
                return Err(Failure::InvalidResponse(
                    "a comment id is not an identifier",
                ));
            }
            let version = version_of(&comment).ok_or(Failure::InvalidResponse(
                "a comment carries no version number",
            ))?;
            let key = format!("page/{page_id}/comment/{id}.html");
            let mut item = Listed::new(key.clone(), version, 0, id.clone());
            HTML.clone_into(&mut item.reference.mime);
            item.reference.acl = acl.clone();
            listed.push(item);
            self.http.check_count(listed.len())?;
            let mut comment_details = Map::new();
            comment_details.insert("id".into(), Value::String(id));
            comment_details.insert("type".into(), Value::String("comment".into()));
            comment_details.insert("page_id".into(), Value::String(page_id.to_owned()));
            comment_details.insert("parent_key".into(), Value::String(page_key.to_owned()));
            comment_details.insert("parent_id".into(), Value::String(page_id.to_owned()));
            if let Some(author) = text_of(&comment, "/version/by/displayName") {
                comment_details.insert("author".into(), Value::String(author.to_owned()));
            }
            if let Some(title) = text_of(&comment, "/title") {
                comment_details.insert("title".into(), Value::String(title.to_owned()));
            }
            details.insert(key, comment_details);
        }
        Ok(())
    }

    /// `_build_page_url`: Cloud page links carry `/wiki` exactly once.
    fn page_url(&self, webui_path: &str) -> String {
        let base = self.config.base_url().as_str().trim_end_matches('/');
        let mut path = webui_path.to_owned();
        if self.config.cloud() {
            if base.ends_with("/wiki") {
                if let Some(rest) = path.strip_prefix("/wiki/") {
                    path = format!("/{rest}");
                }
            } else if !path.starts_with("/wiki/") {
                path = format!("/wiki{path}");
            }
        }
        format!("{base}{path}")
    }

    async fn listed(&self) -> Result<Arc<BTreeMap<String, Listed>>, SourceError> {
        self.cache.get_or_list(|| self.list_and_remember()).await
    }

    async fn listed_fresh(&self) -> Result<Arc<BTreeMap<String, Listed>>, SourceError> {
        self.cache.list_shared(|| self.list_and_remember()).await
    }

    async fn list_and_remember(&self) -> Result<Vec<Listed>, SourceError> {
        let (listed, details) = self
            .listing()
            .await
            .map_err(|failure| failure.into_source_error(PROVIDER, None))?;
        *self
            .details
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner) = Arc::new(details);
        Ok(listed)
    }

    fn details_of(&self, key: &str) -> Map<String, Value> {
        self.details
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
            .get(key)
            .cloned()
            .unwrap_or_default()
    }

    /// A page or comment body (`content/{id}`), pinned to the listed version.
    async fn fetch_content(
        &self,
        listed: &Listed,
        body_key: &str,
        expand_more: &[&str],
    ) -> Result<(String, Value), Failure> {
        let mut url = self.api(&["content", &listed.handle])?;
        let mut expand = vec![format!("body.{body_key}"), "version".to_owned()];
        expand.extend(expand_more.iter().map(|&item| item.to_owned()));
        url.query_pairs_mut()
            .append_pair("expand", &expand.join(","));
        let (_, content) = self.http.json(self.get(url)?).await?;
        let version = version_of(&content).ok_or(Failure::InvalidResponse(
            "the content answer carries no version number",
        ))?;
        if version != listed.reference.version {
            return Err(Failure::Changed);
        }
        let body = content
            .pointer(&format!("/body/{body_key}/value"))
            .and_then(Value::as_str)
            .ok_or(Failure::InvalidResponse(
                "the content answer carries no body in the asked format",
            ))?;
        Ok((strip_base64_images(body), content))
    }
}

impl ContentSource for ConfluenceSource {
    async fn list(&self) -> Result<Vec<DocumentRef>, SourceError> {
        let listing = self.listed_fresh().await?;
        Ok(references(&listing))
    }

    #[allow(
        clippy::too_many_lines,
        reason = "one arm per document kind: page, attachment, comment"
    )]
    async fn fetch(&self, key: &str) -> Result<Document, SourceError> {
        let listing = self.listed().await?;
        let listed = listing
            .get(key)
            .ok_or_else(|| SourceError::NotFound(key.to_owned()))?;
        let failed = |failure: Failure| failure.into_source_error(PROVIDER, Some(key));
        let mut metadata = self.details_of(key);
        let kind = metadata
            .get("type")
            .and_then(Value::as_str)
            .unwrap_or("page")
            .to_owned();
        let (bytes, title) = match kind.as_str() {
            "attachment" => {
                self.http
                    .check_size(listed.reference.size)
                    .map_err(failed)?;
                let url = Url::parse(&format!(
                    "{}{}",
                    self.config.api_root().as_str().trim_end_matches('/'),
                    listed.handle
                ))
                .map_err(|_| {
                    failed(Failure::InvalidResponse(
                        "an attachment download link is not a URL",
                    ))
                })?;
                let bytes = self
                    .http
                    .raw(self.get(url).map_err(failed)?)
                    .await
                    .map_err(failed)?;
                let title = metadata
                    .get("name")
                    .and_then(Value::as_str)
                    .unwrap_or("attachment")
                    .to_owned();
                (bytes, title)
            }
            "comment" => {
                let (html, content) = self
                    .fetch_content(listed, "view", &["container"])
                    .await
                    .map_err(failed)?;
                if let Some(when) = text_of(&content, "/version/when") {
                    metadata.insert("when".into(), Value::String(when.to_owned()));
                    metadata.insert("updated_on".into(), Value::String(when.to_owned()));
                }
                let page_title = text_of(&content, "/container/title").unwrap_or("page");
                let title = format!("Comment {} on {page_title}", listed.handle);
                (html.into_bytes(), title)
            }
            _ => {
                let (html, content) = self
                    .fetch_content(
                        listed,
                        self.arguments.content_format.body_key(),
                        &["space", "metadata.labels"],
                    )
                    .await
                    .map_err(failed)?;
                if let Some(when) = text_of(&content, "/version/when") {
                    metadata.insert("when".into(), Value::String(when.to_owned()));
                    metadata.insert("updated_on".into(), Value::String(when.to_owned()));
                }
                if let Some(space) = text_of(&content, "/space/key") {
                    metadata.insert("space_key".into(), Value::String(space.to_owned()));
                }
                if self.arguments.include_labels {
                    metadata.insert("labels".into(), labels_of(&content));
                }
                metadata.insert(
                    "version".into(),
                    Value::String(listed.reference.version.clone()),
                );
                let title = text_of(&content, "/title")
                    .or_else(|| metadata.get("title").and_then(Value::as_str))
                    .unwrap_or("page")
                    .to_owned();
                (html.into_bytes(), title)
            }
        };
        let uri = metadata
            .get("url")
            .and_then(Value::as_str)
            .map(ToOwned::to_owned)
            .or_else(|| {
                metadata
                    .get("page_id")
                    .and_then(Value::as_str)
                    .map(|page_id| {
                        self.page_url(&format!("/pages/viewpage.action?pageId={page_id}"))
                    })
            });
        let mut reference = listed.reference.clone();
        reference.size = bytes.len() as u64;
        Ok(Document {
            title,
            uri,
            reference,
            bytes,
            metadata,
        })
    }
}

/// `version.number` as the document version.
fn version_of(content: &Value) -> Option<String> {
    match content.pointer("/version/number")? {
        Value::Number(number) => Some(number.to_string()),
        Value::String(text) if !text.trim().is_empty() => Some(text.trim().to_owned()),
        _ => None,
    }
}

/// The read restrictions on a page as principals: `None` when the page is
/// public (no user and no group restriction, or none expanded).
fn read_restrictions(page: &Value) -> Option<Vec<Principal>> {
    let mut principals: Vec<Principal> = Vec::new();
    let mut add = |kind: PrincipalKind, id: &str| {
        let principal = Principal {
            kind,
            id: id.to_owned(),
        };
        if !principals.contains(&principal) {
            principals.push(principal);
        }
    };
    for user in page
        .pointer("/restrictions/read/restrictions/user/results")
        .and_then(Value::as_array)
        .into_iter()
        .flatten()
    {
        if let Some(id) = text_of(user, "/accountId")
            .or_else(|| text_of(user, "/username"))
            .or_else(|| text_of(user, "/userKey"))
        {
            add(PrincipalKind::User, id);
        }
        if let Some(email) = text_of(user, "/email") {
            add(PrincipalKind::Email, email);
        }
    }
    for group in page
        .pointer("/restrictions/read/restrictions/group/results")
        .and_then(Value::as_array)
        .into_iter()
        .flatten()
    {
        if let Some(name) = text_of(group, "/name") {
            add(PrincipalKind::Group, name);
        }
    }
    (!principals.is_empty()).then_some(principals)
}

fn labels_of(content: &Value) -> Value {
    Value::Array(
        content
            .pointer("/metadata/labels/results")
            .and_then(Value::as_array)
            .into_iter()
            .flatten()
            .filter_map(|label| text_of(label, "/name"))
            .map(|name| Value::String(name.to_owned()))
            .collect(),
    )
}

/// CQL string delimiters only (`_escape_cql_query`).
fn escape_cql(value: &str) -> String {
    value.replace('\\', "\\\\").replace('"', "\\\"")
}

/// An attachment title as one key segment: no `/`, no control characters,
/// never empty.
fn key_segment(title: &str) -> String {
    let segment: String = title
        .trim()
        .chars()
        .map(|character| {
            if character == '/' || character == '\\' || character.is_control() {
                '_'
            } else {
                character
            }
        })
        .collect();
    if segment.is_empty() {
        "attachment".to_owned()
    } else {
        segment
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::source::fixture::{Reply, Scripted, query};
    use serde_json::json;
    use std::time::Duration;

    fn config(space: Option<&str>) -> ConfluenceConnectorConfig {
        let mut settings = json!({
            "confluence_configuration": {"base_url": "https://acme.atlassian.net", "token": "fixture-token"}
        })
        .as_object()
        .cloned()
        .unwrap();
        if let Some(space) = space {
            settings.insert("space".into(), Value::String(space.into()));
        }
        ConfluenceConnectorConfig::parse(&settings).expect("config")
    }

    fn arguments(value: &Value) -> ConfluenceIndexArguments {
        ConfluenceIndexArguments::parse(value.as_object().unwrap()).expect("arguments")
    }

    fn allow() -> HostAllowlist {
        HostAllowlist::parse(Some("acme.atlassian.net"))
    }

    fn page(id: &str, title: &str, version: u64) -> Value {
        json!({
            "id": id, "type": "page", "status": "current", "title": title,
            "version": {"number": version, "when": "2026-10-01T00:00:00.000Z", "by": {"displayName": "Ann"}},
            "_links": {"webui": format!("/spaces/DOCS/pages/{id}/{title}")},
            "space": {"key": "DOCS"},
            "metadata": {"labels": {"results": [{"name": "guide"}]}},
            "body": {"view": {"value": format!("<h1>{title}</h1><p>v{version}</p>")}},
        })
    }

    const CONTENT: &str = "/wiki/rest/api/content";

    #[tokio::test]
    async fn a_space_is_listed_across_pages_and_fetched_pinned_to_the_version() {
        let transport = Scripted::new(|request, _| {
            let path = request.url().path();
            if path == CONTENT {
                assert_eq!(query(request, "spaceKey").as_deref(), Some("DOCS"));
                assert_eq!(query(request, "status").as_deref(), Some("current"));
                assert_eq!(query(request, "limit").as_deref(), Some("100"));
                return match query(request, "start").as_deref() {
                    // A Server that caps the page at 2 and says there is more.
                    Some("0") => Reply::json(&json!({
                        "results": [page("1", "Alpha", 3), page("2", "Bravo", 1)],
                        "limit": 2, "size": 2, "_links": {"next": "/rest/api/content?start=2"},
                    })),
                    Some("2") => Reply::json(
                        &json!({"results": [page("3", "Charlie", 7)], "limit": 2, "size": 1}),
                    ),
                    other => panic!("unexpected start {other:?}"),
                };
            }
            if path == "/wiki/rest/api/content/3" {
                assert!(query(request, "expand").unwrap().contains("body.view"));
                return Reply::json(&page("3", "Charlie", 7));
            }
            panic!("unexpected request {path}");
        });
        let source = ConfluenceSource::new(
            config(Some("DOCS")),
            arguments(&json!({})),
            allow(),
            transport.clone(),
        )
        .expect("source");
        let listed = source.list().await.expect("listing");
        let keys: Vec<_> = listed
            .iter()
            .map(|d| (d.key.as_str(), d.version.as_str(), d.mime.as_str()))
            .collect();
        assert_eq!(
            keys,
            [
                ("page/1.html", "3", "text/html"),
                ("page/2.html", "1", "text/html"),
                ("page/3.html", "7", "text/html"),
            ]
        );
        let document = source.fetch("page/3.html").await.expect("document");
        assert_eq!(document.title, "Charlie");
        assert_eq!(document.bytes, b"<h1>Charlie</h1><p>v7</p>");
        assert_eq!(
            document.uri.as_deref(),
            Some("https://acme.atlassian.net/wiki/spaces/DOCS/pages/3/Charlie")
        );
        assert_eq!(document.metadata["id"], "3");
        assert_eq!(document.metadata["space_key"], "DOCS");
        assert_eq!(document.metadata["updated_on"], "2026-10-01T00:00:00.000Z");
        assert!(
            document.metadata.get("labels").is_none(),
            "labels only when asked"
        );
        let bearer = transport.requests()[0]
            .headers()
            .get("authorization")
            .cloned()
            .expect("bearer");
        assert!(bearer.is_sensitive());
        assert_eq!(bearer, "Bearer fixture-token");
    }

    #[tokio::test]
    async fn attachments_and_comments_are_dependents_with_their_own_versions() {
        let transport = Scripted::new(|request, _| {
            let path = request.url().path();
            match path {
                CONTENT => Reply::json(&json!({"results": [page("1", "Alpha", 3)], "size": 1})),
                "/wiki/rest/api/content/1/child/attachment" => Reply::json(&json!({"results": [
                    {"id": "att-9", "title": "Spec.PDF", "extensions": {"fileSize": 9}, "metadata": {"mediaType": "application/pdf", "labels": {"results": []}},
                     "version": {"number": 2, "when": "2026-10-02T00:00:00Z", "by": {"displayName": "Bob"}},
                     "_links": {"download": "/download/attachments/1/Spec.PDF?version=2&modificationDate=1&api=v2"}},
                    {"id": "att-10", "title": "photo.png", "extensions": {"fileSize": 5}, "metadata": {"mediaType": "image/png"},
                     "version": {"number": 1}, "_links": {"download": "/download/attachments/1/photo.png?version=1"}},
                    {"id": "att-11", "title": "notes/with slash.txt", "extensions": {"fileSize": 4}, "metadata": {"mediaType": "text/plain"},
                     "version": {"number": 1}, "_links": {"download": "/download/attachments/1/notes.txt?version=1"}},
                ], "size": 3})),
                "/wiki/rest/api/content/1/child/comment" => {
                    assert_eq!(query(request, "depth").as_deref(), Some("all"));
                    Reply::json(&json!({"results": [
                        {"id": "500", "type": "comment", "title": "Re: Alpha", "version": {"number": 4, "by": {"displayName": "Cy"}}},
                    ], "size": 1}))
                }
                "/wiki/download/attachments/1/Spec.PDF" => {
                    assert_eq!(query(request, "version").as_deref(), Some("2"));
                    Reply::bytes(b"%PDF-1.7\n")
                }
                "/wiki/rest/api/content/500" => Reply::json(&json!({
                    "id": "500", "version": {"number": 4, "when": "2026-10-03T00:00:00Z"},
                    "container": {"id": "1", "title": "Alpha"},
                    "body": {"view": {"value": "<p>Looks <b>good</b></p>"}},
                })),
                other => panic!("unexpected request {other}"),
            }
        });
        let source = ConfluenceSource::new(
            config(Some("DOCS")),
            arguments(&json!({"include_attachments": true, "include_comments": true, "skip_extensions": ["*.png"]})),
            allow(),
            transport.clone(),
        )
        .expect("source");
        let listed = source.list().await.expect("listing");
        let keys: Vec<_> = listed
            .iter()
            .map(|d| (d.key.as_str(), d.version.as_str(), d.size, d.mime.as_str()))
            .collect();
        assert_eq!(
            keys,
            [
                ("page/1.html", "3", 0, "text/html"),
                (
                    "page/1/attachment/att-11/notes_with slash.txt",
                    "1",
                    4,
                    "text/plain"
                ),
                (
                    "page/1/attachment/att-9/Spec.PDF",
                    "2",
                    9,
                    "application/pdf"
                ),
                ("page/1/comment/500.html", "4", 0, "text/html"),
            ]
        );
        let attachment = source
            .fetch("page/1/attachment/att-9/Spec.PDF")
            .await
            .expect("attachment");
        assert_eq!(attachment.bytes, b"%PDF-1.7\n");
        assert_eq!(attachment.title, "Spec.PDF");
        assert_eq!(attachment.metadata["parent_key"], "page/1.html");
        assert_eq!(attachment.metadata["parent_id"], "1");
        assert_eq!(attachment.metadata["type"], "attachment");
        assert_eq!(attachment.metadata["media_type"], "application/pdf");
        assert_eq!(attachment.metadata["creator"], "Bob");
        assert_eq!(
            attachment.metadata["download_url"],
            "https://acme.atlassian.net/wiki/download/attachments/1/Spec.PDF?version=2&modificationDate=1&api=v2"
        );
        let comment = source
            .fetch("page/1/comment/500.html")
            .await
            .expect("comment");
        assert_eq!(comment.bytes, b"<p>Looks <b>good</b></p>");
        assert_eq!(comment.title, "Comment 500 on Alpha");
        assert_eq!(comment.metadata["parent_key"], "page/1.html");
        assert_eq!(comment.metadata["author"], "Cy");
        assert_eq!(comment.metadata["updated_on"], "2026-10-03T00:00:00Z");
        assert_eq!(
            comment.uri.as_deref(),
            Some("https://acme.atlassian.net/wiki/pages/viewpage.action?pageId=1")
        );
    }

    #[tokio::test]
    async fn cql_label_and_page_ids_select_pages_and_the_space_is_ignored() {
        let transport = Scripted::new(|request, _| {
            let path = request.url().path();
            match path {
                CONTENT => panic!("the space must not be listed when pages are selected"),
                "/wiki/rest/api/content/search" => {
                    let cql = query(request, "cql").unwrap();
                    if cql == "text ~ \"roadmap\"" {
                        assert_eq!(
                            query(request, "includeArchivedSpaces").as_deref(),
                            Some("true")
                        );
                        Reply::json(
                            &json!({"results": [page("1", "Alpha", 1), page("2", "Bravo", 1)], "size": 2}),
                        )
                    } else {
                        assert_eq!(cql, "type=page AND label=\"q\\\"1\"");
                        Reply::json(
                            &json!({"results": [page("2", "Bravo", 1), page("4", "Delta", 1)], "size": 2}),
                        )
                    }
                }
                "/wiki/rest/api/content/9" => Reply::json(&page("9", "India", 2)),
                other => panic!("unexpected request {other}"),
            }
        });
        let source = ConfluenceSource::new(
            config(Some("DOCS")),
            arguments(&json!({"cql": "text ~ \"roadmap\"", "label": "q\"1", "page_ids": ["9", "2"], "include_archived_content": true})),
            allow(),
            transport,
        )
        .expect("source");
        let keys: Vec<String> = source
            .list()
            .await
            .expect("listing")
            .into_iter()
            .map(|d| d.key)
            .collect();
        assert_eq!(
            keys,
            ["page/1.html", "page/2.html", "page/4.html", "page/9.html"]
        );
    }

    #[tokio::test]
    async fn restricted_pages_are_left_out_unless_asked_and_then_carry_an_acl() {
        let restricted = || {
            let mut page = page("7", "Secret", 1);
            page["restrictions"] = json!({"read": {"restrictions": {
                "user": {"results": [{"accountId": "u-1", "email": "ann@example.com"}]},
                "group": {"results": [{"name": "leads"}]},
            }}});
            page
        };
        let transport = Scripted::new(move |request, _| {
            assert!(
                query(request, "expand")
                    .unwrap()
                    .contains("restrictions.read.restrictions.user")
            );
            Reply::json(&json!({"results": [page("1", "Alpha", 1), restricted()], "size": 2}))
        });
        let source = ConfluenceSource::new(
            config(Some("DOCS")),
            arguments(&json!({})),
            allow(),
            transport.clone(),
        )
        .expect("source");
        let keys: Vec<String> = source
            .list()
            .await
            .unwrap()
            .into_iter()
            .map(|d| d.key)
            .collect();
        assert_eq!(keys, ["page/1.html"]);

        let source = ConfluenceSource::new(
            config(Some("DOCS")),
            arguments(&json!({"include_restricted_content": true})),
            allow(),
            transport,
        )
        .expect("source");
        let listed = source.list().await.unwrap();
        assert_eq!(listed.len(), 2);
        assert_eq!(listed[0].acl, Acl::Project);
        assert_eq!(
            listed[1].acl,
            Acl::Restricted {
                principals: vec![
                    Principal {
                        kind: PrincipalKind::User,
                        id: "u-1".into()
                    },
                    Principal {
                        kind: PrincipalKind::Email,
                        id: "ann@example.com".into()
                    },
                    Principal {
                        kind: PrincipalKind::Group,
                        id: "leads".into()
                    },
                ]
            }
        );
    }

    #[tokio::test]
    async fn a_refused_credential_is_an_error_never_an_empty_listing() {
        let transport = Scripted::new(|_, _| Reply::status(StatusCode::UNAUTHORIZED));
        let source = ConfluenceSource::new(
            config(Some("DOCS")),
            arguments(&json!({})),
            allow(),
            transport,
        )
        .expect("source");
        let error = source.list().await.expect_err("401 is not empty");
        assert!(
            error
                .to_string()
                .contains("refused the credential (HTTP 401)"),
            "{error}"
        );

        let transport = Scripted::new(|_, _| Reply::json(&json!({"results": [], "size": 0})));
        let source = ConfluenceSource::new(
            config(Some("DOCS")),
            arguments(&json!({})),
            allow(),
            transport,
        )
        .expect("source");
        assert_eq!(source.list().await.expect("empty"), []);

        let transport = Scripted::new(|_, _| Reply::status(StatusCode::NOT_FOUND));
        let source = ConfluenceSource::new(
            config(None),
            arguments(&json!({"page_ids": ["404"]})),
            allow(),
            transport,
        )
        .expect("source");
        let error = source
            .list()
            .await
            .expect_err("an unknown page id fails the run");
        assert!(error.to_string().contains("page '404'"), "{error}");
    }

    #[tokio::test(start_paused = true)]
    async fn a_rate_limit_waits_for_retry_after_and_then_lists() {
        let transport = Scripted::new(|_, index| {
            if index == 0 {
                Reply::rate_limited(Some("3"))
            } else {
                Reply::json(&json!({"results": [page("1", "Alpha", 1)], "size": 1}))
            }
        });
        let source = ConfluenceSource::new(
            config(Some("DOCS")),
            arguments(&json!({})),
            allow(),
            transport.clone(),
        )
        .expect("source");
        let started = tokio::time::Instant::now();
        let listed = source.list().await.expect("after the wait");
        assert_eq!(listed.len(), 1);
        assert_eq!(started.elapsed(), Duration::from_secs(3));
        assert_eq!(transport.seen().len(), 2);

        // Still limited at the bound: the run fails, it does not hang.
        let transport = Scripted::new(|_, _| Reply::rate_limited(Some("1")));
        let source = ConfluenceSource::new(
            config(Some("DOCS")),
            arguments(&json!({})),
            allow(),
            transport,
        )
        .expect("source")
        .with_backoff(Backoff {
            max_attempts: 2,
            ..Backoff::default()
        });
        let error = source.list().await.expect_err("rate limited");
        assert!(error.to_string().contains("rate limited"), "{error}");
    }

    #[tokio::test]
    async fn a_page_that_moved_since_the_listing_is_changed_not_returned() {
        let transport = Scripted::new(|request, _| {
            if request.url().path() == CONTENT {
                Reply::json(&json!({"results": [page("1", "Alpha", 1)], "size": 1}))
            } else {
                Reply::json(&page("1", "Alpha", 2))
            }
        });
        let source = ConfluenceSource::new(
            config(Some("DOCS")),
            arguments(&json!({})),
            allow(),
            transport,
        )
        .expect("source");
        source.list().await.expect("listing");
        let fetched = source.fetch("page/1.html").await;
        assert!(
            matches!(&fetched, Err(SourceError::Changed(key)) if key == "page/1.html"),
            "{fetched:?}"
        );
        assert!(matches!(
            source.fetch("page/2.html").await,
            Err(SourceError::NotFound(_))
        ));
    }

    #[tokio::test]
    async fn selection_and_egress_are_checked_before_any_request() {
        let transport = Scripted::new(|_, _| panic!("no request may be sent"));
        assert!(
            ConfluenceSource::new(
                config(None),
                arguments(&json!({})),
                allow(),
                transport.clone()
            )
            .is_err()
        );
        assert!(
            ConfluenceSource::new(
                config(Some("DOCS")),
                arguments(&json!({})),
                HostAllowlist::parse(Some("github.com")),
                transport
            )
            .is_err()
        );
    }

    #[tokio::test]
    async fn labels_and_the_storage_format_follow_the_arguments() {
        let transport = Scripted::new(|request, _| {
            if request.url().path() == CONTENT {
                assert!(
                    query(request, "expand")
                        .unwrap()
                        .contains("metadata.labels")
                );
                Reply::json(&json!({"results": [page("1", "Alpha", 3)], "size": 1}))
            } else {
                assert!(query(request, "expand").unwrap().contains("body.storage"));
                let mut page = page("1", "Alpha", 3);
                page["body"] = json!({"storage": {"value": "<p>stored <img src=\"data:image/png;base64,AAAA\"/></p>"}});
                Reply::json(&page)
            }
        });
        let source = ConfluenceSource::new(
            config(Some("DOCS")),
            arguments(&json!({"content_format": "storage", "include_labels": true})),
            allow(),
            transport,
        )
        .expect("source");
        source.list().await.expect("listing");
        let document = source.fetch("page/1.html").await.expect("document");
        assert_eq!(
            document.bytes,
            b"<p>stored <img src=\"[Image Removed]\"/></p>"
        );
        assert_eq!(document.metadata["labels"], json!(["guide"]));
    }
}

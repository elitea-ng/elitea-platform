//! Jira as a `ContentSource` (ADR-0030 decision 3, stage I2).
//!
//! * **List.** Every issue the JQL matches (the SDK's default JQL when
//!   `index_data` names none): v3 `search/jql` with `nextPageToken`, v2
//!   `search` with `startAt`, 100 issues a page, until the API says the
//!   listing is over. There is no issue cap: `max_total_issues` is accepted
//!   and ignored, and the source's document cap bounds the run.
//! * **Dependents.** With `include_attachments`, the `attachment` field of
//!   each issue lists its attachments (less `skip_attachment_extensions`);
//!   with `include_comments`, each issue's comments are listed
//!   (`issue/{id}/comment`, paged). Each is a document of its own with
//!   `parent_key`, and its own version: an attachment is immutable in Jira
//!   (a re-upload is a new id), a comment carries `updated`. The SDK gives
//!   every dependent the issue's `updated` instead.
//! * **Keys and versions.** An issue is `issue/{id}.md` (the numeric id,
//!   stable across key renames; the SDK keys by id too), versioned by
//!   `fields.updated`; an attachment is
//!   `issue/{id}/attachment/{attachment id}/{file name}`, versioned by its
//!   id and size; a comment is `issue/{id}/comment/{comment id}.md`,
//!   versioned by `updated`.
//! * **Fetch.** An issue is read again (`issue/{id}?fields=…`) and refused
//!   as `SourceError::Changed` when `updated` moved; its document is the
//!   SDK's markdown: `# Summary`, `# Description` (ADF rendered as text on
//!   Cloud, wiki markup as written on Server) and one `# {field}` section
//!   per `fields_to_index`. A comment's body is rendered the same way. An
//!   attachment is downloaded from its `content` URL — Cloud answers with a
//!   redirect to its media host, followed once through the egress guard, so
//!   a deployment allowlists that host too — or, on Server and Data Center,
//!   from `secure/attachment/{id}/{file name}`.
//! * **Auth and limits.** The credential and custom headers come from the
//!   toolkit configuration ([`super::config`]); a 401 or 403 on a listing is
//!   an error naming the credential, never an empty listing; a 429 (or a
//!   `Retry-After`) backs off within the shared bound.

use std::collections::BTreeMap;
use std::fmt::Write as _;
use std::sync::{Arc, Mutex};

use elitea_content_source::{ContentSource, Document, DocumentRef, SourceError};
use serde_json::{Map, Value};

use super::config::{JiraApiVersion, JiraConnectorConfig, JiraIndexArguments, PROVIDER};
use crate::atlassian::{adf_text, id_of, safe_identifier, size_of, text_of};
use crate::egress::HostAllowlist;
use crate::source::{
    Backoff, Failure, Listed, ListingCache, SourceHttp, SourceLimits, Timeouts, references, web_url,
};
use crate::transport::header::LOCATION;
use crate::transport::{Method, Request, StatusCode, Transport, Url};

/// Issues (and comments) asked per page; Jira caps `maxResults` itself.
const PAGE_SIZE: usize = 100;
/// The fields the SDK always requests.
const DEFAULT_FIELDS: [&str; 9] = [
    "status",
    "summary",
    "reporter",
    "description",
    "created",
    "updated",
    "assignee",
    "project",
    "issuetype",
];

type Details = BTreeMap<String, Map<String, Value>>;

/// One Jira toolkit's issues, attachments and comments.
pub struct JiraSource {
    config: JiraConnectorConfig,
    arguments: JiraIndexArguments,
    /// The `fields` query every search and issue read sends.
    fields: String,
    http: SourceHttp,
    cache: ListingCache,
    details: Mutex<Arc<Details>>,
}

impl JiraSource {
    /// The toolkit `config` indexed with the `index_data` `arguments`, every
    /// request through `allowlist` (fail-closed) and then `transport`.
    ///
    /// # Errors
    ///
    /// The Jira host is not on the allowlist.
    pub fn new(
        config: JiraConnectorConfig,
        arguments: JiraIndexArguments,
        allowlist: HostAllowlist,
        transport: Arc<dyn Transport>,
    ) -> Result<Self, SourceError> {
        let http = SourceHttp::new(allowlist, transport);
        if !http.permits(config.base_url()) {
            return Err(Failure::Refused.into_source_error(PROVIDER, None));
        }
        let mut fields: Vec<String> = DEFAULT_FIELDS
            .iter()
            .map(|field| (*field).to_owned())
            .collect();
        for field in arguments
            .fields_to_extract
            .iter()
            .chain(arguments.fields_to_index.iter())
        {
            let lower = field.to_ascii_lowercase();
            if !fields.contains(&lower) {
                fields.push(lower);
            }
        }
        if arguments.include_attachments && !fields.iter().any(|field| field == "attachment") {
            fields.push("attachment".to_owned());
        }
        Ok(Self {
            config,
            arguments,
            fields: fields.join(","),
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

    /// `{base}/rest/api/{version}/{segments...}`.
    fn api(&self, segments: &[&str]) -> Result<Url, Failure> {
        let mut all = vec!["rest", "api", self.config.api_version().as_str()];
        all.extend_from_slice(segments);
        web_url(self.config.base_url(), all).ok_or(Failure::Client(
            "the Jira base URL cannot carry a path".to_owned(),
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

    async fn json(&self, url: Url) -> Result<Value, Failure> {
        match self.http.json(self.get(url)?).await {
            Ok((_, value)) => Ok(value),
            Err(Failure::Status(status @ (StatusCode::UNAUTHORIZED | StatusCode::FORBIDDEN))) => {
                Err(Failure::Client(format!(
                    "Jira refused the credential (HTTP {}); check the toolkit's token or API key",
                    status.as_u16()
                )))
            }
            Err(failure) => Err(failure),
        }
    }

    fn browse_url(&self, key: &str) -> String {
        format!(
            "{}/browse/{key}",
            self.config.base_url().as_str().trim_end_matches('/')
        )
    }

    /// Every issue the JQL matches, in search order.
    async fn issues(&self, pages_seen: &mut usize) -> Result<Vec<Value>, Failure> {
        let mut issues = Vec::new();
        let mut start = 0_usize;
        let mut next_token: Option<String> = None;
        let token_pagination = self.config.api_version() == JiraApiVersion::V3;
        loop {
            *pages_seen += 1;
            self.http.check_pages(*pages_seen)?;
            let mut url = if token_pagination {
                self.api(&["search", "jql"])?
            } else {
                self.api(&["search"])?
            };
            {
                let mut query = url.query_pairs_mut();
                query
                    .append_pair("jql", self.arguments.jql())
                    .append_pair("fields", &self.fields)
                    .append_pair("maxResults", &PAGE_SIZE.to_string());
                if token_pagination {
                    if let Some(token) = &next_token {
                        query.append_pair("nextPageToken", token);
                    }
                } else {
                    query.append_pair("startAt", &start.to_string());
                }
            }
            let page = self.json(url).await?;
            let batch = match page.get("issues") {
                None | Some(Value::Null) => Vec::new(),
                Some(Value::Array(items)) => items.clone(),
                Some(_) => return Err(Failure::InvalidResponse("issues is not a list")),
            };
            if batch.is_empty() {
                return Ok(issues);
            }
            start += batch.len();
            let got = batch.len();
            issues.extend(batch);
            self.http.check_count(issues.len())?;
            if token_pagination {
                let last = page.get("isLast").and_then(Value::as_bool).unwrap_or(false);
                next_token = text_of(&page, "/nextPageToken").map(ToOwned::to_owned);
                if last || next_token.is_none() {
                    return Ok(issues);
                }
            } else {
                let total = page.get("total").and_then(Value::as_u64);
                let asked = page
                    .get("maxResults")
                    .and_then(Value::as_u64)
                    .map_or(PAGE_SIZE, |asked| {
                        usize::try_from(asked).unwrap_or(PAGE_SIZE)
                    });
                let over = total.is_some_and(|total| start as u64 >= total);
                if over || (total.is_none() && got < asked.min(PAGE_SIZE)) {
                    return Ok(issues);
                }
            }
        }
    }

    async fn listing(&self) -> Result<(Vec<Listed>, Details), Failure> {
        let mut pages_seen = 0_usize;
        let mut listed = Vec::new();
        let mut details = Details::new();
        for issue in self.issues(&mut pages_seen).await? {
            let id = id_of(&issue, "/id").ok_or(Failure::InvalidResponse("an issue has no id"))?;
            if !safe_identifier(&id) {
                return Err(Failure::InvalidResponse("an issue id is not an identifier"));
            }
            let issue_key = text_of(&issue, "/key").unwrap_or(&id).to_owned();
            let version = text_of(&issue, "/fields/updated")
                .ok_or(Failure::InvalidResponse("an issue carries no `updated`"))?
                .to_owned();
            let key = format!("issue/{id}.md");
            listed.push(Listed::new(key.clone(), version, 0, id.clone()));
            self.http.check_count(listed.len())?;
            if self.arguments.include_attachments {
                self.list_attachments(&issue, &id, &issue_key, &key, &mut listed, &mut details)?;
            }
            if self.arguments.include_comments {
                self.list_comments(
                    &id,
                    &issue_key,
                    &key,
                    &mut pages_seen,
                    &mut listed,
                    &mut details,
                )
                .await?;
            }
            let mut issue_details = Map::new();
            issue_details.insert("id".into(), Value::String(id));
            issue_details.insert("issue_key".into(), Value::String(issue_key.clone()));
            issue_details.insert("type".into(), Value::String("jira_issue".into()));
            issue_details.insert("url".into(), Value::String(self.browse_url(&issue_key)));
            details.insert(key, issue_details);
        }
        Ok((listed, details))
    }

    /// The attachments the `attachment` field of `issue` lists, as documents
    /// under the issue's key.
    fn list_attachments(
        &self,
        issue: &Value,
        id: &str,
        issue_key: &str,
        key: &str,
        listed: &mut Vec<Listed>,
        details: &mut Details,
    ) -> Result<(), Failure> {
        for attachment in issue
            .pointer("/fields/attachment")
            .and_then(Value::as_array)
            .into_iter()
            .flatten()
        {
            let Some(attachment_id) = id_of(attachment, "/id") else {
                return Err(Failure::InvalidResponse("an attachment has no id"));
            };
            if !safe_identifier(&attachment_id) {
                return Err(Failure::InvalidResponse(
                    "an attachment id is not an identifier",
                ));
            }
            let filename = text_of(attachment, "/filename").unwrap_or("attachment");
            if self.arguments.skips_attachment(filename) {
                continue;
            }
            let size = size_of(attachment, "/size");
            let attachment_key = format!(
                "issue/{id}/attachment/{attachment_id}/{}",
                key_segment(filename)
            );
            listed.push(Listed::new(
                attachment_key.clone(),
                format!("{attachment_id}:{size}"),
                size,
                attachment_id.clone(),
            ));
            self.http.check_count(listed.len())?;
            let mut attachment_details = Map::new();
            attachment_details.insert(
                "id".into(),
                Value::String(format!("attach_{attachment_id}")),
            );
            attachment_details.insert("attachment_id".into(), Value::String(attachment_id));
            attachment_details.insert("type".into(), Value::String("attachment".into()));
            attachment_details.insert("filename".into(), Value::String(filename.to_owned()));
            attachment_details.insert("issue_key".into(), Value::String(issue_key.to_owned()));
            attachment_details.insert("parent_key".into(), Value::String(key.to_owned()));
            attachment_details.insert("parent_id".into(), Value::String(id.to_owned()));
            attachment_details.insert("url".into(), Value::String(self.browse_url(issue_key)));
            if let Some(mime) = text_of(attachment, "/mimeType") {
                attachment_details.insert("mimeType".into(), Value::String(mime.to_owned()));
            }
            if let Some(created) = text_of(attachment, "/created") {
                attachment_details.insert("created".into(), Value::String(created.to_owned()));
            }
            if let Some(author) = text_of(attachment, "/author/displayName")
                .or_else(|| text_of(attachment, "/author/name"))
            {
                attachment_details.insert("author".into(), Value::String(author.to_owned()));
            }
            if let Some(content) = text_of(attachment, "/content") {
                attachment_details.insert("content_url".into(), Value::String(content.to_owned()));
            }
            details.insert(attachment_key, attachment_details);
        }
        Ok(())
    }

    #[allow(clippy::too_many_arguments)]
    async fn list_comments(
        &self,
        issue_id: &str,
        issue_key: &str,
        parent_key: &str,
        pages_seen: &mut usize,
        listed: &mut Vec<Listed>,
        details: &mut Details,
    ) -> Result<(), Failure> {
        let mut start = 0_usize;
        loop {
            *pages_seen += 1;
            self.http.check_pages(*pages_seen)?;
            let mut url = self.api(&["issue", issue_id, "comment"])?;
            url.query_pairs_mut()
                .append_pair("startAt", &start.to_string())
                .append_pair("maxResults", &PAGE_SIZE.to_string());
            let page = self.json(url).await?;
            let comments = match page.get("comments") {
                None | Some(Value::Null) => Vec::new(),
                Some(Value::Array(items)) => items.clone(),
                Some(_) => return Err(Failure::InvalidResponse("comments is not a list")),
            };
            if comments.is_empty() {
                return Ok(());
            }
            start += comments.len();
            for comment in &comments {
                let id =
                    id_of(comment, "/id").ok_or(Failure::InvalidResponse("a comment has no id"))?;
                if !safe_identifier(&id) {
                    return Err(Failure::InvalidResponse(
                        "a comment id is not an identifier",
                    ));
                }
                let version = text_of(comment, "/updated")
                    .or_else(|| text_of(comment, "/created"))
                    .ok_or(Failure::InvalidResponse("a comment carries no `updated`"))?
                    .to_owned();
                let key = format!("issue/{issue_id}/comment/{id}.md");
                listed.push(Listed::new(key.clone(), version, 0, id.clone()));
                self.http.check_count(listed.len())?;
                let mut comment_details = Map::new();
                comment_details.insert("id".into(), Value::String(id));
                comment_details.insert("type".into(), Value::String("comment".into()));
                comment_details.insert("issue_key".into(), Value::String(issue_key.to_owned()));
                comment_details.insert("parent_key".into(), Value::String(parent_key.to_owned()));
                comment_details.insert("parent_id".into(), Value::String(issue_id.to_owned()));
                comment_details.insert("url".into(), Value::String(self.browse_url(issue_key)));
                details.insert(key, comment_details);
            }
            let total = page.get("total").and_then(Value::as_u64);
            if total.is_some_and(|total| start as u64 >= total)
                || total.is_none() && comments.len() < PAGE_SIZE
            {
                return Ok(());
            }
        }
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

    /// The SDK's issue document: `# Summary`, `# Description`, then one
    /// section per `fields_to_index`.
    fn render_issue(&self, fields: &Map<String, Value>) -> String {
        let mut content = format!(
            "# Summary\n{}\n\n",
            fields.get("summary").and_then(Value::as_str).unwrap_or("")
        );
        if let Some(description) = fields.get("description").filter(|value| truthy(value)) {
            let text = adf_text(description);
            if !text.trim().is_empty() {
                let _ = write!(content, "# Description\n{text}\n\n");
            }
        }
        for field in &self.arguments.fields_to_index {
            let value = fields
                .get(field)
                .or_else(|| fields.get(&field.to_ascii_lowercase()))
                .filter(|value| truthy(value));
            if let Some(value) = value {
                let _ = write!(content, "# {field}\n{}\n\n", field_text(value));
            }
        }
        content
    }

    /// An attachment's bytes: its `content` URL (Cloud; one redirect to the
    /// media host followed through the egress guard) or the Server download
    /// path.
    async fn download(
        &self,
        listed: &Listed,
        details: &Map<String, Value>,
    ) -> Result<Vec<u8>, Failure> {
        let url = match details.get("content_url").and_then(Value::as_str) {
            Some(content) => Url::parse(content)
                .map_err(|_| Failure::InvalidResponse("an attachment content link is not a URL"))?,
            None if self.config.cloud() || self.config.api_version() == JiraApiVersion::V3 => {
                self.api(&["attachment", "content", &listed.handle])?
            }
            None => {
                let filename = details
                    .get("filename")
                    .and_then(Value::as_str)
                    .unwrap_or("attachment");
                web_url(
                    self.config.base_url(),
                    ["secure", "attachment", listed.handle.as_str(), filename],
                )
                .ok_or(Failure::Client(
                    "the Jira base URL cannot carry a path".to_owned(),
                ))?
            }
        };
        let mut response = self.http.send(self.get(url.clone())?).await?;
        if response.status().is_redirection() {
            let location = response
                .headers()
                .get(LOCATION)
                .and_then(|value| value.to_str().ok())
                .ok_or(Failure::InvalidResponse("a redirect carries no Location"))?;
            let target = url
                .join(location)
                .map_err(|_| Failure::InvalidResponse("a redirect Location is not a URL"))?;
            // The media host is another origin: the credential is not
            // forwarded, the egress guard still applies.
            return self.http.raw(Request::new(Method::GET, target)).await;
        }
        if !response.status().is_success() {
            return Err(Failure::Status(response.status()));
        }
        SourceHttp::body(&mut response, self.http.limits().max_document_bytes).await
    }
}

impl ContentSource for JiraSource {
    async fn list(&self) -> Result<Vec<DocumentRef>, SourceError> {
        let listing = self.listed_fresh().await?;
        Ok(references(&listing))
    }

    #[allow(
        clippy::too_many_lines,
        reason = "one arm per document kind: issue, attachment, comment"
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
            .unwrap_or("jira_issue")
            .to_owned();
        let (bytes, title) = match kind.as_str() {
            "attachment" => {
                self.http
                    .check_size(listed.reference.size)
                    .map_err(failed)?;
                let bytes = self.download(listed, &metadata).await.map_err(failed)?;
                let title = metadata
                    .get("filename")
                    .and_then(Value::as_str)
                    .unwrap_or("attachment")
                    .to_owned();
                metadata.remove("content_url");
                (bytes, title)
            }
            "comment" => {
                let issue_id = metadata
                    .get("parent_id")
                    .and_then(Value::as_str)
                    .unwrap_or_default()
                    .to_owned();
                let url = self
                    .api(&["issue", &issue_id, "comment", &listed.handle])
                    .map_err(failed)?;
                let comment = self.json(url).await.map_err(failed)?;
                let version = text_of(&comment, "/updated")
                    .or_else(|| text_of(&comment, "/created"))
                    .ok_or_else(|| {
                        failed(Failure::InvalidResponse("a comment carries no `updated`"))
                    })?;
                if version != listed.reference.version {
                    return Err(failed(Failure::Changed));
                }
                let body = comment.get("body").map(adf_text).unwrap_or_default();
                if let Some(created) = text_of(&comment, "/created") {
                    metadata.insert("created".into(), Value::String(created.to_owned()));
                }
                metadata.insert("updated_on".into(), Value::String(version.to_owned()));
                if let Some(author) = text_of(&comment, "/author/displayName") {
                    metadata.insert("author".into(), Value::String(author.to_owned()));
                }
                let issue_key = metadata
                    .get("issue_key")
                    .and_then(Value::as_str)
                    .unwrap_or("issue");
                let title = format!("Comment {} on {issue_key}", listed.handle);
                (body.into_bytes(), title)
            }
            _ => {
                let mut url = self.api(&["issue", &listed.handle]).map_err(failed)?;
                url.query_pairs_mut().append_pair("fields", &self.fields);
                let issue = self.json(url).await.map_err(failed)?;
                let version = text_of(&issue, "/fields/updated").ok_or_else(|| {
                    failed(Failure::InvalidResponse("an issue carries no `updated`"))
                })?;
                if version != listed.reference.version {
                    return Err(failed(Failure::Changed));
                }
                let empty = Map::new();
                let fields = issue
                    .get("fields")
                    .and_then(Value::as_object)
                    .unwrap_or(&empty);
                let content = self.render_issue(fields);
                metadata.insert("updated_on".into(), Value::String(version.to_owned()));
                let text_field = |pointer: &str| {
                    fields_pointer(fields, pointer).map(|text| Value::String(text.to_owned()))
                };
                if let Some(author) = text_field("/reporter/emailAddress") {
                    metadata.insert("author".into(), author);
                }
                if let Some(status) = text_field("/status/name") {
                    metadata.insert("status".into(), status);
                }
                if let Some(created) = text_field("/created") {
                    metadata.insert("created_on".into(), created);
                }
                if let Some(project) = text_field("/project/key") {
                    metadata.insert("project".into(), project);
                }
                if let Some(issuetype) = text_field("/issuetype/name") {
                    metadata.insert("issuetype".into(), issuetype);
                }
                let title = fields
                    .get("summary")
                    .and_then(Value::as_str)
                    .filter(|summary| !summary.trim().is_empty())
                    .map_or_else(
                        || {
                            metadata
                                .get("issue_key")
                                .and_then(Value::as_str)
                                .unwrap_or("issue")
                                .to_owned()
                        },
                        ToOwned::to_owned,
                    );
                (content.into_bytes(), title)
            }
        };
        let uri = metadata
            .get("url")
            .and_then(Value::as_str)
            .map(ToOwned::to_owned);
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

fn fields_pointer<'a>(fields: &'a Map<String, Value>, pointer: &str) -> Option<&'a str> {
    let (head, rest) = pointer
        .trim_start_matches('/')
        .split_once('/')
        .unwrap_or((pointer.trim_start_matches('/'), ""));
    let value = fields.get(head)?;
    if rest.is_empty() {
        value.as_str()
    } else {
        value.pointer(&format!("/{rest}")).and_then(Value::as_str)
    }
    .map(str::trim)
    .filter(|text| !text.is_empty())
}

/// Python truthiness of a JSON value.
fn truthy(value: &Value) -> bool {
    match value {
        Value::Null => false,
        Value::Bool(flag) => *flag,
        Value::Number(number) => number.as_f64().is_some_and(|number| number != 0.0),
        Value::String(text) => !text.is_empty(),
        Value::Array(items) => !items.is_empty(),
        Value::Object(map) => !map.is_empty(),
    }
}

/// A `fields_to_index` value as text: a string as it is, a list joined by
/// `, `, an object (ADF or any other) as readable text when it is ADF and
/// compact JSON otherwise.
fn field_text(value: &Value) -> String {
    match value {
        Value::String(text) => text.clone(),
        Value::Array(items) => items
            .iter()
            .map(|item| match item {
                Value::String(text) => text.clone(),
                Value::Object(map) => map
                    .get("name")
                    .or_else(|| map.get("value"))
                    .or_else(|| map.get("displayName"))
                    .and_then(Value::as_str)
                    .map_or_else(|| item.to_string(), ToOwned::to_owned),
                other => other.to_string(),
            })
            .collect::<Vec<_>>()
            .join(", "),
        Value::Object(map) if map.get("type").and_then(Value::as_str) == Some("doc") => {
            adf_text(value)
        }
        Value::Object(map) => map
            .get("name")
            .or_else(|| map.get("value"))
            .or_else(|| map.get("displayName"))
            .and_then(Value::as_str)
            .map_or_else(|| value.to_string(), ToOwned::to_owned),
        other => other.to_string(),
    }
}

/// A file name as one key segment: no `/`, no control characters, never
/// empty.
fn key_segment(name: &str) -> String {
    let segment: String = name
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

    fn config(url: &str, extra: &Value) -> JiraConnectorConfig {
        let mut settings = extra.as_object().cloned().unwrap_or_default();
        settings.insert(
            "jira_configuration".into(),
            json!({"base_url": url, "username": "bot@example.com", "api_key": "fixture-key"}),
        );
        JiraConnectorConfig::parse(&settings).expect("config")
    }

    fn cloud() -> JiraConnectorConfig {
        config("https://acme.atlassian.net", &json!({}))
    }

    fn arguments(value: &Value) -> JiraIndexArguments {
        JiraIndexArguments::parse(value.as_object().unwrap()).expect("arguments")
    }

    fn allow() -> HostAllowlist {
        HostAllowlist::parse(Some("acme.atlassian.net,jira.corp.example,media.example"))
    }

    fn issue(id: &str, key: &str, updated: &str) -> Value {
        json!({
            "id": id, "key": key,
            "fields": {
                "summary": format!("Summary of {key}"),
                "description": {"type": "doc", "version": 1, "content": [
                    {"type": "paragraph", "content": [{"type": "text", "text": format!("Body of {key} at {updated}")}]}
                ]},
                "status": {"name": "Open"}, "reporter": {"emailAddress": "ann@example.com", "displayName": "Ann"},
                "created": "2026-09-01T00:00:00.000+0000", "updated": updated,
                "project": {"key": "PROJ"}, "issuetype": {"name": "Task"},
                "labels": ["a", "b"],
                "attachment": [
                    {"id": "9001", "filename": "spec.pdf", "size": 9, "mimeType": "application/pdf", "created": "2026-09-02T00:00:00.000+0000",
                     "author": {"displayName": "Bob"}, "content": "https://acme.atlassian.net/rest/api/3/attachment/content/9001"},
                    {"id": "9002", "filename": "shot.PNG", "size": 5, "mimeType": "image/png",
                     "content": "https://acme.atlassian.net/rest/api/3/attachment/content/9002"},
                ],
            }
        })
    }

    #[tokio::test]
    async fn v3_search_follows_the_page_token_and_the_issue_is_rendered_like_the_sdk() {
        let transport = Scripted::new(|request, _| {
            let path = request.url().path();
            match path {
                "/rest/api/3/search/jql" => {
                    assert_eq!(
                        query(request, "jql").as_deref(),
                        Some(JiraIndexArguments::DEFAULT_JQL)
                    );
                    assert_eq!(query(request, "maxResults").as_deref(), Some("100"));
                    let fields = query(request, "fields").unwrap();
                    assert!(
                        fields.contains("summary") && fields.contains("labels"),
                        "{fields}"
                    );
                    assert!(!fields.contains("attachment"));
                    match query(request, "nextPageToken").as_deref() {
                        None => Reply::json(
                            &json!({"issues": [issue("1", "PROJ-1", "u1"), issue("2", "PROJ-2", "u1")], "nextPageToken": "t2", "isLast": false}),
                        ),
                        Some("t2") => Reply::json(
                            &json!({"issues": [issue("3", "PROJ-3", "u3")], "isLast": true}),
                        ),
                        other => panic!("unexpected token {other:?}"),
                    }
                }
                "/rest/api/3/issue/3" => {
                    assert!(query(request, "fields").unwrap().contains("labels"));
                    Reply::json(&issue("3", "PROJ-3", "u3"))
                }
                other => panic!("unexpected request {other}"),
            }
        });
        let source = JiraSource::new(
            cloud(),
            arguments(&json!({"fields_to_index": ["labels"]})),
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
                ("issue/1.md", "u1", "text/markdown"),
                ("issue/2.md", "u1", "text/markdown"),
                ("issue/3.md", "u3", "text/markdown"),
            ]
        );
        let document = source.fetch("issue/3.md").await.expect("document");
        assert_eq!(
            String::from_utf8(document.bytes).unwrap(),
            "# Summary\nSummary of PROJ-3\n\n# Description\nBody of PROJ-3 at u3\n\n\n# labels\na, b\n\n"
        );
        assert_eq!(document.title, "Summary of PROJ-3");
        assert_eq!(
            document.uri.as_deref(),
            Some("https://acme.atlassian.net/browse/PROJ-3")
        );
        assert_eq!(document.metadata["id"], "3");
        assert_eq!(document.metadata["issue_key"], "PROJ-3");
        assert_eq!(document.metadata["author"], "ann@example.com");
        assert_eq!(document.metadata["status"], "Open");
        assert_eq!(document.metadata["project"], "PROJ");
        assert_eq!(document.metadata["issuetype"], "Task");
        assert_eq!(document.metadata["updated_on"], "u3");
        assert_eq!(document.metadata["type"], "jira_issue");
        let basic = transport.requests()[0]
            .headers()
            .get("authorization")
            .cloned()
            .expect("basic");
        assert!(basic.is_sensitive());
        assert!(basic.to_str().unwrap().starts_with("Basic "));
    }

    #[tokio::test]
    async fn v2_search_follows_start_at_and_total() {
        let transport = Scripted::new(|request, _| {
            assert_eq!(request.url().path(), "/jira/rest/api/2/search");
            match query(request, "startAt").as_deref() {
                // A Server that caps the page at 2 of 3.
                Some("0") => Reply::json(
                    &json!({"issues": [issue("1", "P-1", "u"), issue("2", "P-2", "u")], "startAt": 0, "maxResults": 2, "total": 3}),
                ),
                Some("2") => Reply::json(
                    &json!({"issues": [issue("3", "P-3", "u")], "startAt": 2, "maxResults": 2, "total": 3}),
                ),
                other => panic!("unexpected startAt {other:?}"),
            }
        });
        let source = JiraSource::new(
            config("https://jira.corp.example/jira", &json!({})),
            arguments(&json!({"jql": "project = P"})),
            allow(),
            transport.clone(),
        )
        .expect("source");
        let listed = source.list().await.expect("listing");
        assert_eq!(listed.len(), 3);
        assert_eq!(transport.seen().len(), 2);
    }

    #[tokio::test]
    async fn attachments_and_comments_are_dependents_with_their_own_versions() {
        let transport = Scripted::new(|request, _| {
            let path = request.url().path();
            match path {
                "/rest/api/3/search/jql" => {
                    assert!(query(request, "fields").unwrap().contains("attachment"));
                    Reply::json(&json!({"issues": [issue("1", "PROJ-1", "u1")], "isLast": true}))
                }
                "/rest/api/3/issue/1/comment" => {
                    assert_eq!(query(request, "startAt").as_deref(), Some("0"));
                    Reply::json(&json!({"comments": [
                        {"id": "77", "created": "c1", "updated": "c2", "author": {"displayName": "Cy"},
                         "body": {"type": "doc", "content": [{"type": "paragraph", "content": [{"type": "text", "text": "LGTM"}]}]}},
                    ], "startAt": 0, "maxResults": 100, "total": 1}))
                }
                "/rest/api/3/issue/1/comment/77" => Reply::json(&json!({
                    "id": "77", "created": "c1", "updated": "c2", "author": {"displayName": "Cy"},
                    "body": {"type": "doc", "content": [{"type": "paragraph", "content": [{"type": "text", "text": "LGTM"}]}]},
                })),
                "/rest/api/3/attachment/content/9001" => Reply::status(StatusCode::SEE_OTHER)
                    .header("location", "https://media.example/blob/9001?token=x"),
                "/blob/9001" => {
                    assert!(
                        request.headers().get("authorization").is_none(),
                        "no credential to the media host"
                    );
                    Reply::bytes(b"%PDF-1.7\n")
                }
                other => panic!("unexpected request {other}"),
            }
        });
        let source = JiraSource::new(
            cloud(),
            arguments(&json!({"include_attachments": true, "include_comments": true, "skip_attachment_extensions": [".png"]})),
            allow(),
            transport,
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
                ("issue/1.md", "u1", 0, "text/markdown"),
                (
                    "issue/1/attachment/9001/spec.pdf",
                    "9001:9",
                    9,
                    "application/pdf"
                ),
                ("issue/1/comment/77.md", "c2", 0, "text/markdown"),
            ]
        );
        let attachment = source
            .fetch("issue/1/attachment/9001/spec.pdf")
            .await
            .expect("attachment");
        assert_eq!(attachment.bytes, b"%PDF-1.7\n");
        assert_eq!(attachment.title, "spec.pdf");
        assert_eq!(attachment.metadata["id"], "attach_9001");
        assert_eq!(attachment.metadata["parent_key"], "issue/1.md");
        assert_eq!(attachment.metadata["parent_id"], "1");
        assert_eq!(attachment.metadata["issue_key"], "PROJ-1");
        assert_eq!(attachment.metadata["mimeType"], "application/pdf");
        assert_eq!(attachment.metadata["author"], "Bob");
        assert_eq!(attachment.metadata["type"], "attachment");
        assert!(attachment.metadata.get("content_url").is_none());
        let comment = source
            .fetch("issue/1/comment/77.md")
            .await
            .expect("comment");
        assert_eq!(comment.bytes, b"LGTM\n");
        assert_eq!(comment.title, "Comment 77 on PROJ-1");
        assert_eq!(comment.metadata["parent_key"], "issue/1.md");
        assert_eq!(comment.metadata["author"], "Cy");
        assert_eq!(comment.metadata["created"], "c1");
        assert_eq!(
            comment.uri.as_deref(),
            Some("https://acme.atlassian.net/browse/PROJ-1")
        );
    }

    #[tokio::test]
    async fn server_attachments_download_from_the_secure_path() {
        let transport = Scripted::new(|request, _| match request.url().path() {
            "/rest/api/2/search" => {
                let mut issue = issue("1", "P-1", "u");
                issue["fields"]["attachment"] = json!([{"id": "5", "filename": "a b.txt", "size": 3, "mimeType": "text/plain"}]);
                Reply::json(&json!({"issues": [issue], "total": 1, "maxResults": 100}))
            }
            "/secure/attachment/5/a%20b.txt" => Reply::bytes(b"abc"),
            other => panic!("unexpected request {other}"),
        });
        let source = JiraSource::new(
            config("https://jira.corp.example", &json!({})),
            arguments(&json!({"include_attachments": true})),
            allow(),
            transport,
        )
        .expect("source");
        source.list().await.expect("listing");
        let attachment = source
            .fetch("issue/1/attachment/5/a b.txt")
            .await
            .expect("attachment");
        assert_eq!(attachment.bytes, b"abc");
    }

    #[tokio::test]
    async fn a_refused_credential_is_an_error_never_an_empty_listing() {
        let transport = Scripted::new(|_, _| Reply::status(StatusCode::FORBIDDEN));
        let source =
            JiraSource::new(cloud(), arguments(&json!({})), allow(), transport).expect("source");
        let error = source.list().await.expect_err("403 is not empty");
        assert!(
            error
                .to_string()
                .contains("refused the credential (HTTP 403)"),
            "{error}"
        );

        let transport = Scripted::new(|_, _| Reply::json(&json!({"issues": [], "isLast": true})));
        let source =
            JiraSource::new(cloud(), arguments(&json!({})), allow(), transport).expect("source");
        assert_eq!(source.list().await.expect("empty"), []);

        let transport = Scripted::new(|_, _| panic!("no request may be sent"));
        assert!(
            JiraSource::new(
                cloud(),
                arguments(&json!({})),
                HostAllowlist::parse(Some("github.com")),
                transport
            )
            .is_err()
        );
    }

    #[tokio::test(start_paused = true)]
    async fn a_rate_limit_waits_for_retry_after_and_then_lists() {
        let transport = Scripted::new(|_, index| {
            if index == 0 {
                Reply::rate_limited(Some("2"))
            } else {
                Reply::json(&json!({"issues": [issue("1", "PROJ-1", "u1")], "isLast": true}))
            }
        });
        let source = JiraSource::new(cloud(), arguments(&json!({})), allow(), transport.clone())
            .expect("source");
        let started = tokio::time::Instant::now();
        assert_eq!(source.list().await.expect("after the wait").len(), 1);
        assert_eq!(started.elapsed(), Duration::from_secs(2));
        assert_eq!(transport.seen().len(), 2);

        let transport = Scripted::new(|_, _| Reply::rate_limited(Some("1")));
        let source = JiraSource::new(cloud(), arguments(&json!({})), allow(), transport)
            .expect("source")
            .with_backoff(Backoff {
                max_attempts: 2,
                ..Backoff::default()
            });
        assert!(
            source
                .list()
                .await
                .expect_err("rate limited")
                .to_string()
                .contains("rate limited")
        );
    }

    #[tokio::test]
    async fn an_issue_that_moved_since_the_listing_is_changed_not_returned() {
        let transport = Scripted::new(|request, _| {
            if request.url().path() == "/rest/api/3/search/jql" {
                Reply::json(&json!({"issues": [issue("1", "PROJ-1", "u1")], "isLast": true}))
            } else {
                Reply::json(&issue("1", "PROJ-1", "u2"))
            }
        });
        let source =
            JiraSource::new(cloud(), arguments(&json!({})), allow(), transport).expect("source");
        source.list().await.expect("listing");
        let fetched = source.fetch("issue/1.md").await;
        assert!(
            matches!(&fetched, Err(SourceError::Changed(key)) if key == "issue/1.md"),
            "{fetched:?}"
        );
        assert!(matches!(
            source.fetch("issue/9.md").await,
            Err(SourceError::NotFound(_))
        ));
    }

    #[test]
    fn field_values_render_like_the_sdk() {
        assert_eq!(field_text(&json!("x")), "x");
        assert_eq!(field_text(&json!(["a", {"name": "High"}, 3])), "a, High, 3");
        assert_eq!(field_text(&json!({"name": "High", "id": "1"})), "High");
        assert_eq!(field_text(&json!({"x": 1})), "{\"x\":1}");
        assert_eq!(
            field_text(
                &json!({"type": "doc", "content": [{"type": "paragraph", "content": [{"type": "text", "text": "adf"}]}]})
            ),
            "adf\n"
        );
        assert!(
            !truthy(&json!("")) && !truthy(&json!([])) && truthy(&json!("a")) && truthy(&json!(1))
        );
    }
}

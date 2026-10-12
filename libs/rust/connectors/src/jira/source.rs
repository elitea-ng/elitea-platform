//! Jira as a `ContentSource` (ADR-0030 decision 3, stage I2).
//!
//! * **List.** Every issue the JQL matches (the SDK's default JQL when
//!   `index_data` names none). Jira Cloud is searched through
//!   `search/jql` with `nextPageToken` under the configured API version
//!   (Cloud serves `/rest/api/2/search/jql` and `/rest/api/3/search/jql`;
//!   the old `search` is being removed there); Server and Data Center keep
//!   `search` with `startAt`/`total`. 100 issues a page, until the API says
//!   the listing is over. There is no issue cap: `max_total_issues` is
//!   accepted and ignored, and the source's document cap bounds the run.
//!   Each paginated listing (the search, one issue's comment overflow) has
//!   the page cap to itself.
//! * **Dependents.** With `include_attachments`, the `attachment` field of
//!   each issue lists its attachments (less `skip_attachment_extensions`);
//!   with `include_comments`, the `comment` field of each issue carries its
//!   comments — bodies included, so no comment is read again — and only an
//!   issue whose embedded page is short of `total` has the rest listed
//!   (`issue/{id}/comment`, paged), a bounded number of issues at a time.
//!   Each is a document of its own with `parent_key`, and its own version:
//!   an attachment is immutable in Jira (a re-upload is a new id), a comment
//!   carries `updated`. The SDK gives every dependent the issue's `updated`
//!   instead.
//! * **Restrictions.** An issue with a security level (`fields.security`)
//!   is left out with its dependents: the level's members are not
//!   principals this runtime can record. Fail-closed: an issue whose
//!   response lacks the `security` key is left out too when its project has
//!   an issue security scheme (asked once per project per run, and assumed
//!   when the question fails); only a project without one reads absence as
//!   unrestricted. A comment with `visibility` is left
//!   out unless `include_restricted_content`, and then a group-restricted
//!   comment is recorded as [`Acl::Restricted`] with that group; a
//!   role-restricted comment and a Service Management internal note
//!   (`jsdPublic: false`) stay out either way.
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
//!   per `fields_to_index`. A comment's document is the body the listing
//!   already holds, rendered the same way. An attachment is downloaded from
//!   the Jira origin only: its listed `content` URL when that URL is on the
//!   configured base URL's origin (scheme included), otherwise the download
//!   is refused; without one, `attachment/content/{id}` on Cloud and
//!   `secure/attachment/{id}/{file name}` on Server. Cloud answers with a
//!   redirect to its media host (`api.media.atlassian.com`), which the shared
//!   [`crate::atlassian::AttachmentDownloader`] follows without the
//!   credential, through the egress guard and only to `https` (so a
//!   deployment allowlists that host too); a redirect to a login page, back
//!   to the Jira origin after leaving it, or to any other host is refused,
//!   and so is an HTML page served as a non-HTML attachment.
//! * **Auth and limits.** The credential and custom headers come from the
//!   toolkit configuration ([`super::config`]); a 401 or 403 on a listing is
//!   an error naming the credential, never an empty listing; a 429 (or a
//!   `Retry-After`) backs off within the shared bound.

use std::collections::BTreeMap;
use std::fmt::Write as _;
use std::sync::Arc;

use elitea_content_source::{
    Acl, ContentSource, Document, DocumentRef, Principal, PrincipalKind, SourceError,
};
use futures_util::StreamExt as _;
use serde_json::{Map, Value};

use super::config::{JiraConnectorConfig, JiraIndexArguments, PROVIDER};
use crate::atlassian::{
    AttachmentDownloader, COMMENT_LISTING_CONCURRENCY, DetailedListing, Details, adf_text,
    authorized_json, id_of, key_segment, safe_identifier, same_origin, size_of, text_of,
};
use crate::egress::HostAllowlist;
use crate::source::{
    Backoff, Failure, Listed, SourceHttp, SourceLimits, Timeouts, references, web_url,
};
use crate::transport::{Method, Request, Transport, Url};

/// Issues (and comments) asked per page; Jira caps `maxResults` itself.
const PAGE_SIZE: usize = 100;
/// The fields the SDK always requests, plus `security`: an issue's security
/// level decides whether it is listed at all.
const DEFAULT_FIELDS: [&str; 10] = [
    "status",
    "summary",
    "reporter",
    "description",
    "created",
    "updated",
    "assignee",
    "project",
    "issuetype",
    "security",
];

/// One issue whose embedded comment page did not hold every comment: the
/// rest is listed from `start`.
struct CommentTail {
    issue_id: String,
    issue_key: String,
    parent_key: String,
    start: usize,
}

/// One Jira toolkit's issues, attachments and comments.
pub struct JiraSource {
    config: JiraConnectorConfig,
    arguments: JiraIndexArguments,
    /// The `fields` query every search and issue read sends.
    fields: String,
    http: SourceHttp,
    downloads: AttachmentDownloader,
    listing: DetailedListing,
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
        let downloads = AttachmentDownloader::new(config.base_url(), &allowlist);
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
        if arguments.include_comments && !fields.iter().any(|field| field == "comment") {
            fields.push("comment".to_owned());
        }
        Ok(Self {
            config,
            arguments,
            fields: fields.join(","),
            http,
            downloads,
            listing: DetailedListing::default(),
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
        authorized_json(&self.http, self.get(url)?, PROVIDER).await
    }

    fn browse_url(&self, key: &str) -> String {
        format!(
            "{}/browse/{key}",
            self.config.base_url().as_str().trim_end_matches('/')
        )
    }

    /// One search page: Cloud's `search/jql` continued by `next_token`,
    /// Server's `search` continued by `start`.
    fn search_url(&self, start: usize, next_token: Option<&str>) -> Result<Url, Failure> {
        let mut url = if self.config.cloud() {
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
            if self.config.cloud() {
                if let Some(token) = next_token {
                    query.append_pair("nextPageToken", token);
                }
            } else {
                query.append_pair("startAt", &start.to_string());
            }
        }
        Ok(url)
    }

    /// Every issue the JQL matches, in search order, under the search's own
    /// page cap.
    async fn issues(&self) -> Result<Vec<Value>, Failure> {
        let mut issues = Vec::new();
        let mut start = 0_usize;
        let mut next_token: Option<String> = None;
        let mut pages_seen = 0_usize;
        loop {
            pages_seen += 1;
            self.http.check_pages(pages_seen)?;
            let mut page = self
                .json(self.search_url(start, next_token.as_deref())?)
                .await?;
            // Taken, not cloned: a page holds up to 100 issues.
            let batch = match page.get_mut("issues").map(Value::take) {
                None | Some(Value::Null) => Vec::new(),
                Some(Value::Array(items)) => items,
                Some(_) => return Err(Failure::InvalidResponse("issues is not a list")),
            };
            if batch.is_empty() {
                return Ok(issues);
            }
            start += batch.len();
            let got = batch.len();
            issues.extend(batch);
            self.http.check_count(issues.len())?;
            if self.config.cloud() {
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
        let mut listed = Vec::new();
        let mut details = Details::new();
        let mut tails: Vec<CommentTail> = Vec::new();
        let mut schemes: BTreeMap<String, bool> = BTreeMap::new();
        for mut issue in self.issues().await? {
            let id = id_of(&issue, "/id").ok_or(Failure::InvalidResponse("an issue has no id"))?;
            if !safe_identifier(&id) {
                return Err(Failure::InvalidResponse("an issue id is not an identifier"));
            }
            // A security level restricts the issue to the level's members
            // (groups, roles, the reporter, …), which this runtime cannot
            // record as principals: the issue and its dependents are left
            // out whatever `include_restricted_content` says.
            if self.is_secured(&issue, &mut schemes).await {
                continue;
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
                let embedded = match issue
                    .pointer_mut("/fields/comment/comments")
                    .map(Value::take)
                {
                    None | Some(Value::Null) => Vec::new(),
                    Some(Value::Array(items)) => items,
                    Some(_) => return Err(Failure::InvalidResponse("comments is not a list")),
                };
                let page = issue.pointer("/fields/comment");
                self.keep_comments(&embedded, &id, &issue_key, &key, &mut listed, &mut details)?;
                let start_at = page
                    .and_then(|page| page.get("startAt"))
                    .and_then(Value::as_u64)
                    .and_then(|start| usize::try_from(start).ok())
                    .unwrap_or(0);
                let covered = start_at.saturating_add(embedded.len());
                let total = page
                    .and_then(|page| page.get("total"))
                    .and_then(Value::as_u64);
                let tail = match (page, total) {
                    // No `comment` field at all: list the comments apart.
                    (None, _) => Some(0),
                    (Some(_), Some(total)) if (covered as u64) < total => Some(covered),
                    _ => None,
                };
                if let Some(start) = tail {
                    tails.push(CommentTail {
                        issue_id: id.clone(),
                        issue_key: issue_key.clone(),
                        parent_key: key.clone(),
                        start,
                    });
                }
            }
            let mut issue_details = Map::new();
            issue_details.insert("id".into(), Value::String(id));
            issue_details.insert("issue_key".into(), Value::String(issue_key.clone()));
            issue_details.insert("type".into(), Value::String("jira_issue".into()));
            issue_details.insert("url".into(), Value::String(self.browse_url(&issue_key)));
            details.insert(key, issue_details);
        }
        // The comments the embedded pages did not hold, a bounded number of
        // issues at a time; each issue's listing has the page cap to itself.
        let mut remaining = futures_util::stream::iter(tails.into_iter().map(|tail| async move {
            let comments = self.comment_pages(&tail.issue_id, tail.start).await?;
            Ok::<_, Failure>((tail, comments))
        }))
        .buffer_unordered(COMMENT_LISTING_CONCURRENCY);
        while let Some(result) = remaining.next().await {
            let (tail, comments) = result?;
            self.keep_comments(
                &comments,
                &tail.issue_id,
                &tail.issue_key,
                &tail.parent_key,
                &mut listed,
                &mut details,
            )?;
        }
        Ok((listed, details))
    }

    /// Whether `issue` is under a security level, fail-closed (review
    /// round 2, finding 6). A level present is a level. An issue whose
    /// response lacks the `security` key (the field was not honoured) is
    /// restricted when its project has an issue security scheme, or when
    /// that cannot be established; only in a project known to have no scheme
    /// does absence mean unrestricted. `schemes` remembers the answer per
    /// project for the run.
    async fn is_secured(&self, issue: &Value, schemes: &mut BTreeMap<String, bool>) -> bool {
        match issue.pointer("/fields/security") {
            Some(Value::Null) => return false,
            Some(_) => return true,
            None => {}
        }
        let Some(project) = text_of(issue, "/fields/project/key")
            .or_else(|| text_of(issue, "/fields/project/id"))
            .filter(|project| safe_identifier(project))
        else {
            return true;
        };
        if let Some(has_scheme) = schemes.get(project) {
            return *has_scheme;
        }
        // Any failure to find out is a scheme: closed.
        let has_scheme = self
            .project_has_security_scheme(project)
            .await
            .unwrap_or(true);
        schemes.insert(project.to_owned(), has_scheme);
        has_scheme
    }

    /// Whether `project` has an issue security scheme. The scheme itself
    /// (`project/{key}/issuesecuritylevelscheme`, Cloud and Data Center)
    /// answers 200 when there is one; its administrator permission makes a
    /// 404 ambiguous (no scheme, or not allowed to see it), so a 404 is
    /// settled by the project's security levels the caller may see
    /// (`project/{key}/securitylevel`, Browse Projects): none, and no issue
    /// the caller can list can be under a level.
    async fn project_has_security_scheme(&self, project: &str) -> Result<bool, Failure> {
        let scheme = self.api(&["project", project, "issuesecuritylevelscheme"])?;
        match self.http.json(self.get(scheme)?).await {
            Ok((_, scheme)) if scheme.get("id").is_some() || scheme.get("levels").is_some() => {
                return Ok(true);
            }
            Ok(_) | Err(Failure::Status(_)) => {}
            Err(failure) => return Err(failure),
        }
        let levels = self.api(&["project", project, "securitylevel"])?;
        let (_, answer) = self.http.json(self.get(levels)?).await?;
        match answer.get("levels") {
            Some(Value::Array(levels)) => Ok(!levels.is_empty()),
            _ => Err(Failure::InvalidResponse("security levels is not a list")),
        }
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

    /// The comments of `issue_id` from `start` on (`issue/{id}/comment`,
    /// paged), under this listing's own page cap.
    async fn comment_pages(&self, issue_id: &str, start: usize) -> Result<Vec<Value>, Failure> {
        let mut comments = Vec::new();
        let mut start = start;
        let mut pages_seen = 0_usize;
        loop {
            pages_seen += 1;
            self.http.check_pages(pages_seen)?;
            let mut url = self.api(&["issue", issue_id, "comment"])?;
            url.query_pairs_mut()
                .append_pair("startAt", &start.to_string())
                .append_pair("maxResults", &PAGE_SIZE.to_string());
            let mut page = self.json(url).await?;
            let batch = match page.get_mut("comments").map(Value::take) {
                None | Some(Value::Null) => Vec::new(),
                Some(Value::Array(items)) => items,
                Some(_) => return Err(Failure::InvalidResponse("comments is not a list")),
            };
            if batch.is_empty() {
                return Ok(comments);
            }
            start += batch.len();
            let got = batch.len();
            comments.extend(batch);
            self.http.check_count(comments.len())?;
            let total = page.get("total").and_then(Value::as_u64);
            if total.is_some_and(|total| start as u64 >= total)
                || total.is_none() && got < PAGE_SIZE
            {
                return Ok(comments);
            }
        }
    }

    /// Who may read `comment`: everyone in the project, the listed group, or
    /// nobody this runtime can name (`None`: the comment is left out).
    fn comment_acl(&self, comment: &Value) -> Option<Acl> {
        // A Service Management internal note is read by the project's
        // agents, who are not principals.
        if comment.get("jsdPublic").and_then(Value::as_bool) == Some(false) {
            return None;
        }
        let visibility = match comment.get("visibility") {
            None | Some(Value::Null) => return Some(Acl::Project),
            Some(visibility) => visibility,
        };
        if !self.arguments.include_restricted_content {
            return None;
        }
        match text_of(visibility, "/type") {
            Some("group") => {
                let group =
                    text_of(visibility, "/value").or_else(|| text_of(visibility, "/identifier"))?;
                Some(Acl::Restricted {
                    principals: vec![Principal {
                        kind: PrincipalKind::Group,
                        id: group.to_owned(),
                    }],
                })
            }
            // A project role has no principal of its own.
            _ => None,
        }
    }

    /// `comments` as documents under `parent_key`, bodies rendered now so
    /// the fetch reads nothing again.
    fn keep_comments(
        &self,
        comments: &[Value],
        issue_id: &str,
        issue_key: &str,
        parent_key: &str,
        listed: &mut Vec<Listed>,
        details: &mut Details,
    ) -> Result<(), Failure> {
        for comment in comments {
            let id =
                id_of(comment, "/id").ok_or(Failure::InvalidResponse("a comment has no id"))?;
            if !safe_identifier(&id) {
                return Err(Failure::InvalidResponse(
                    "a comment id is not an identifier",
                ));
            }
            let Some(acl) = self.comment_acl(comment) else {
                continue;
            };
            let version = text_of(comment, "/updated")
                .or_else(|| text_of(comment, "/created"))
                .ok_or(Failure::InvalidResponse("a comment carries no `updated`"))?
                .to_owned();
            let key = format!("issue/{issue_id}/comment/{id}.md");
            let mut item = Listed::new(key.clone(), version.clone(), 0, id.clone());
            item.reference.acl = acl;
            listed.push(item);
            self.http.check_count(listed.len())?;
            let mut comment_details = Map::new();
            comment_details.insert("id".into(), Value::String(id));
            comment_details.insert("type".into(), Value::String("comment".into()));
            comment_details.insert("issue_key".into(), Value::String(issue_key.to_owned()));
            comment_details.insert("parent_key".into(), Value::String(parent_key.to_owned()));
            comment_details.insert("parent_id".into(), Value::String(issue_id.to_owned()));
            comment_details.insert("url".into(), Value::String(self.browse_url(issue_key)));
            comment_details.insert(
                "body".into(),
                Value::String(comment.get("body").map(adf_text).unwrap_or_default()),
            );
            if let Some(created) = text_of(comment, "/created") {
                comment_details.insert("created".into(), Value::String(created.to_owned()));
            }
            comment_details.insert("updated_on".into(), Value::String(version));
            if let Some(author) = text_of(comment, "/author/displayName") {
                comment_details.insert("author".into(), Value::String(author.to_owned()));
            }
            details.insert(key, comment_details);
        }
        Ok(())
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

    /// An attachment's bytes, from the Jira origin only: the listed
    /// `content` URL when it is on the base URL's origin (a link elsewhere
    /// is refused, so the credential never leaves the configured origin),
    /// otherwise the Cloud `attachment/content/{id}` or the Server
    /// `secure/attachment` path. Redirects go through the shared
    /// [`AttachmentDownloader`]: Cloud's to its media host without the
    /// credential, a login page refused.
    async fn download(
        &self,
        listed: &Listed,
        details: &Map<String, Value>,
    ) -> Result<Vec<u8>, Failure> {
        let url = match details.get("content_url").and_then(Value::as_str) {
            Some(content) => {
                let content = Url::parse(content).map_err(|_| {
                    Failure::InvalidResponse("an attachment content link is not a URL")
                })?;
                if !same_origin(self.config.base_url(), &content) {
                    return Err(Failure::Client(
                        "the attachment's content link is not on the Jira origin, so the download was refused (the credential is sent only there)"
                            .to_owned(),
                    ));
                }
                content
            }
            None if self.config.cloud() => self.api(&["attachment", "content", &listed.handle])?,
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
        let filename = details
            .get("filename")
            .and_then(Value::as_str)
            .unwrap_or("attachment");
        self.downloads
            .download(
                &self.http,
                url,
                |url| self.get(url),
                filename,
                details.get("mimeType").and_then(Value::as_str),
            )
            .await
    }
}

impl ContentSource for JiraSource {
    async fn list(&self) -> Result<Vec<DocumentRef>, SourceError> {
        let listing = self
            .listing
            .list_shared(PROVIDER, || self.listing())
            .await?;
        Ok(references(&listing.listing))
    }

    #[allow(
        clippy::too_many_lines,
        reason = "one arm per document kind: issue, attachment, comment"
    )]
    async fn fetch(&self, key: &str) -> Result<Document, SourceError> {
        let snapshot = self
            .listing
            .get_or_list(PROVIDER, || self.listing())
            .await?;
        let listed = snapshot
            .listing
            .get(key)
            .ok_or_else(|| SourceError::NotFound(key.to_owned()))?;
        let failed = |failure: Failure| failure.into_source_error(PROVIDER, Some(key));
        let mut metadata = snapshot.details_of(key);
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
                // The body was listed with the version: nothing is read again.
                let body = match metadata.remove("body") {
                    Some(Value::String(body)) => body,
                    _ => String::new(),
                };
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

#[cfg(test)]
mod tests {
    use super::*;
    use crate::source::fixture::{Reply, Scripted, query};
    use crate::transport::StatusCode;
    use serde_json::json;
    use std::collections::BTreeMap;
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
        HostAllowlist::parse(Some(
            "acme.atlassian.net,jira.corp.example,media.example,api.media.atlassian.com",
        ))
    }

    /// A comment as Jira Cloud's `comment` field (and `issue/{id}/comment`)
    /// lists it.
    fn comment(id: &str, updated: &str, text: &str) -> Value {
        json!({
            "self": format!("https://acme.atlassian.net/rest/api/3/issue/1/comment/{id}"),
            "id": id, "created": "c1", "updated": updated,
            "author": {"accountId": "5b10a2844c20165700ede21g", "displayName": "Cy"},
            "updateAuthor": {"accountId": "5b10a2844c20165700ede21g", "displayName": "Cy"},
            "body": {"type": "doc", "version": 1, "content": [{"type": "paragraph", "content": [{"type": "text", "text": text}]}]},
            "jsdPublic": true,
        })
    }

    /// The `comment` field: a page of comments as the issue carries it.
    #[allow(clippy::needless_pass_by_value)]
    fn comment_page(comments: Vec<Value>, total: u64) -> Value {
        json!({"comments": comments, "self": "https://acme.atlassian.net/rest/api/3/issue/1/comment",
               "maxResults": 100, "total": total, "startAt": 0})
    }

    fn issue(id: &str, key: &str, updated: &str) -> Value {
        json!({
            "id": id, "key": key, "self": format!("https://acme.atlassian.net/rest/api/3/issue/{id}"),
            "fields": {
                "summary": format!("Summary of {key}"),
                "description": {"type": "doc", "version": 1, "content": [
                    {"type": "paragraph", "content": [{"type": "text", "text": format!("Body of {key} at {updated}")}]}
                ]},
                "status": {"name": "Open"}, "reporter": {"emailAddress": "ann@example.com", "displayName": "Ann"},
                "created": "2026-09-01T00:00:00.000+0000", "updated": updated,
                "project": {"key": "PROJ"}, "issuetype": {"name": "Task"},
                "labels": ["a", "b"],
                "security": null,
                "attachment": [
                    {"id": "9001", "filename": "spec.pdf", "size": 9, "mimeType": "application/pdf", "created": "2026-09-02T00:00:00.000+0000",
                     "author": {"displayName": "Bob"}, "content": "https://acme.atlassian.net/rest/api/3/attachment/content/9001"},
                    {"id": "9002", "filename": "shot.PNG", "size": 5, "mimeType": "image/png",
                     "content": "https://acme.atlassian.net/rest/api/3/attachment/content/9002"},
                ],
                "comment": comment_page(vec![comment("77", "c2", "LGTM")], 1),
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
                        fields.contains("summary")
                            && fields.contains("labels")
                            && fields.contains("security"),
                        "{fields}"
                    );
                    assert!(!fields.contains("attachment") && !fields.contains("comment"));
                    assert!(query(request, "startAt").is_none());
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

    /// Review round 1, finding 5: Jira Cloud removed `search`; the token
    /// search is used there whatever `api_version` says (Cloud serves it
    /// under `/rest/api/2` and `/rest/api/3`), and `startAt` is never sent.
    #[tokio::test]
    async fn cloud_searches_through_search_jql_under_either_api_version() {
        let transport = Scripted::new(|request, _| {
            assert_eq!(request.url().path(), "/rest/api/2/search/jql");
            assert!(query(request, "startAt").is_none(), "{}", request.url());
            match query(request, "nextPageToken").as_deref() {
                None => Reply::json(
                    &json!({"issues": [issue("1", "PROJ-1", "u1")], "nextPageToken": "n2", "isLast": false}),
                ),
                Some("n2") => {
                    Reply::json(&json!({"issues": [issue("2", "PROJ-2", "u1")], "isLast": true}))
                }
                other => panic!("unexpected token {other:?}"),
            }
        });
        let source = JiraSource::new(
            config("https://acme.atlassian.net", &json!({"api_version": "2"})),
            arguments(&json!({})),
            allow(),
            transport.clone(),
        )
        .expect("source");
        assert_eq!(source.list().await.expect("listing").len(), 2);
        assert_eq!(transport.seen().len(), 2);
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
                    let fields = query(request, "fields").unwrap();
                    assert!(fields.contains("attachment") && fields.contains("comment"));
                    Reply::json(&json!({"issues": [issue("1", "PROJ-1", "u1")], "isLast": true}))
                }
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
        let requests_before = transport.seen().len();
        let comment = source
            .fetch("issue/1/comment/77.md")
            .await
            .expect("comment");
        assert_eq!(
            transport.seen().len(),
            requests_before,
            "the comment body came with the search: nothing is read again"
        );
        assert_eq!(comment.bytes, b"LGTM\n");
        assert_eq!(comment.title, "Comment 77 on PROJ-1");
        assert_eq!(comment.metadata["parent_key"], "issue/1.md");
        assert_eq!(comment.metadata["author"], "Cy");
        assert_eq!(comment.metadata["created"], "c1");
        assert_eq!(comment.metadata["updated_on"], "c2");
        assert!(comment.metadata.get("body").is_none());
        assert_eq!(
            comment.uri.as_deref(),
            Some("https://acme.atlassian.net/browse/PROJ-1")
        );
    }

    /// Review round 1, finding 9: comments come with the search; only an
    /// issue whose embedded page is short of `total` has the rest listed,
    /// one request per missing page and none per comment.
    #[tokio::test]
    async fn only_the_comment_overflow_is_listed_apart_and_the_call_count_shows_it() {
        let transport = Scripted::new(|request, _| {
            let path = request.url().path();
            if path == "/rest/api/3/search/jql" {
                let mut issues = Vec::new();
                for n in 1..=20_u32 {
                    let mut issue = issue(&n.to_string(), &format!("PROJ-{n}"), "u1");
                    // Every issue's page holds 1 of 2 comments; issue 20 is
                    // complete.
                    let total = if n == 20 { 1 } else { 2 };
                    issue["fields"]["comment"] =
                        comment_page(vec![comment(&format!("{n}01"), "c2", "first")], total);
                    issues.push(issue);
                }
                return Reply::json(&json!({"issues": issues, "isLast": true}));
            }
            let issue_id = path
                .strip_prefix("/rest/api/3/issue/")
                .and_then(|rest| rest.strip_suffix("/comment"))
                .unwrap_or_else(|| panic!("unexpected request {path}"));
            assert_eq!(query(request, "startAt").as_deref(), Some("1"));
            Reply::json(&json!({
                "comments": [comment(&format!("{issue_id}02"), "c3", "second")],
                "maxResults": 100, "total": 2, "startAt": 1,
            }))
        });
        let source = JiraSource::new(
            cloud(),
            arguments(&json!({"include_comments": true})),
            allow(),
            transport.clone(),
        )
        .expect("source");
        let listed = source.list().await.expect("listing");
        assert_eq!(listed.len(), 20 + 19 * 2 + 1);
        let seen = transport.seen();
        assert_eq!(
            seen.len(),
            1 + 19,
            "one search, one overflow page per short issue: {seen:?}"
        );
        assert!(
            !seen
                .iter()
                .any(|request| request.contains("/issue/20/comment")),
            "{seen:?}"
        );
        let second = source
            .fetch("issue/7/comment/702.md")
            .await
            .expect("an overflow comment");
        assert_eq!(second.bytes, b"second\n");
        assert_eq!(second.metadata["parent_key"], "issue/7.md");
        assert_eq!(transport.seen().len(), 20, "no request per comment");
    }

    /// Review round 1, finding 4: each paginated listing has the page cap
    /// to itself. Twenty one-page comment overflows pass a cap of one page;
    /// a two-page overflow does not.
    #[tokio::test]
    async fn the_page_cap_applies_per_listing_not_across_them() {
        let listing = |long_tail_for: Option<&'static str>| {
            Scripted::new(move |request, _| {
                let path = request.url().path();
                if path == "/rest/api/3/search/jql" {
                    let issues: Vec<Value> = (1..=20_u32)
                        .map(|n| {
                            let mut issue = issue(&n.to_string(), &format!("PROJ-{n}"), "u1");
                            issue["fields"]["comment"] =
                                comment_page(vec![comment(&format!("{n}01"), "c2", "first")], 2);
                            issue
                        })
                        .collect();
                    return Reply::json(&json!({"issues": issues, "isLast": true}));
                }
                let issue_id = path
                    .strip_prefix("/rest/api/3/issue/")
                    .and_then(|rest| rest.strip_suffix("/comment"))
                    .unwrap_or_else(|| panic!("unexpected request {path}"));
                let start: u64 = query(request, "startAt").unwrap().parse().unwrap();
                // The long tail says "3 in total" and hands one per page.
                let total = if Some(issue_id) == long_tail_for {
                    3
                } else {
                    2
                };
                Reply::json(&json!({
                    "comments": [comment(&format!("{issue_id}9{start}"), "c3", "more")],
                    "maxResults": 1, "total": total, "startAt": start,
                }))
            })
        };
        let one_page = SourceLimits {
            max_pages: 1,
            ..SourceLimits::default()
        };
        let source = JiraSource::new(
            cloud(),
            arguments(&json!({"include_comments": true})),
            allow(),
            listing(None),
        )
        .expect("source")
        .with_limits(one_page);
        assert_eq!(source.list().await.expect("listing").len(), 60);

        let source = JiraSource::new(
            cloud(),
            arguments(&json!({"include_comments": true})),
            allow(),
            listing(Some("7")),
        )
        .expect("source")
        .with_limits(one_page);
        let error = source.list().await.expect_err("a two-page listing");
        assert!(error.to_string().contains("page cap"), "{error}");
    }

    /// Review round 1, finding 2: a comment's `visibility` and an issue's
    /// security level restrict readers. By default both are left out; with
    /// `include_restricted_content` a group-visible comment is recorded with
    /// its group, while a role-visible comment, a Service Management
    /// internal note and a secured issue stay out.
    #[tokio::test]
    async fn restricted_comments_and_secured_issues_are_left_out_unless_expressible_and_asked() {
        let transport = Scripted::new(|_, _| {
            let mut secured = issue("2", "PROJ-2", "u1");
            secured["fields"]["security"] = json!({
                "self": "https://acme.atlassian.net/rest/api/3/securitylevel/10021",
                "id": "10021", "description": "Only the reporter and internal staff can see this issue.", "name": "Reporter Only",
            });
            let mut open = issue("1", "PROJ-1", "u1");
            let mut by_group = comment("78", "c2", "for devs");
            by_group["visibility"] = json!({"type": "group", "value": "jira-developers", "identifier": "276f955c-63d7-42c8-9520-92d01dca0625"});
            let mut by_role = comment("79", "c2", "for admins");
            by_role["visibility"] =
                json!({"type": "role", "value": "Administrators", "identifier": "Administrators"});
            let mut internal = comment("80", "c2", "internal note");
            internal["jsdPublic"] = json!(false);
            open["fields"]["comment"] = comment_page(
                vec![comment("77", "c2", "LGTM"), by_group, by_role, internal],
                4,
            );
            Reply::json(&json!({"issues": [open, secured], "isLast": true}))
        });
        let source = JiraSource::new(
            cloud(),
            arguments(&json!({"include_comments": true, "include_attachments": true})),
            allow(),
            transport.clone(),
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
            [
                "issue/1.md",
                "issue/1/attachment/9001/spec.pdf",
                "issue/1/attachment/9002/shot.PNG",
                "issue/1/comment/77.md",
            ],
            "the secured issue, its dependents and every restricted comment are out"
        );

        let source = JiraSource::new(
            cloud(),
            arguments(&json!({"include_comments": true, "include_restricted_content": true})),
            allow(),
            transport,
        )
        .expect("source");
        let listed = source.list().await.expect("listing");
        let acls: BTreeMap<&str, &Acl> = listed.iter().map(|d| (d.key.as_str(), &d.acl)).collect();
        assert_eq!(
            acls.keys().copied().collect::<Vec<_>>(),
            [
                "issue/1.md",
                "issue/1/comment/77.md",
                "issue/1/comment/78.md"
            ]
        );
        assert_eq!(acls["issue/1/comment/77.md"], &Acl::Project);
        assert_eq!(
            acls["issue/1/comment/78.md"],
            &Acl::Restricted {
                principals: vec![Principal {
                    kind: PrincipalKind::Group,
                    id: "jira-developers".into()
                }]
            }
        );
    }

    /// Review round 1, finding 1: the credential goes to the Jira origin
    /// only. A `content` link on another origin, or on the same host over
    /// plain HTTP, is refused before any request; the media-host redirect is
    /// followed without the credential and never to plain HTTP.
    #[tokio::test]
    async fn attachment_downloads_never_send_the_credential_off_the_jira_origin() {
        let transport = Scripted::new(|request, _| match request.url().path() {
            "/rest/api/3/search/jql" => {
                let mut issue = issue("1", "PROJ-1", "u1");
                issue["fields"]["attachment"] = json!([
                    {"id": "1", "filename": "foreign.txt", "size": 3, "content": "https://media.example/rest/api/3/attachment/content/1"},
                    {"id": "2", "filename": "downgrade.txt", "size": 3, "content": "http://acme.atlassian.net/rest/api/3/attachment/content/2"},
                    {"id": "3", "filename": "redirected.txt", "size": 3, "content": "https://acme.atlassian.net/rest/api/3/attachment/content/3"},
                    {"id": "4", "filename": "built.txt", "size": 3},
                ]);
                Reply::json(&json!({"issues": [issue], "isLast": true}))
            }
            "/rest/api/3/attachment/content/3" => Reply::status(StatusCode::SEE_OTHER)
                .header("location", "http://media.example/blob/3"),
            "/rest/api/3/attachment/content/4" => {
                assert!(request.headers().get("authorization").is_some());
                Reply::bytes(b"abc")
            }
            other => panic!("the credential must not reach {other}"),
        });
        let source = JiraSource::new(
            cloud(),
            arguments(&json!({"include_attachments": true})),
            allow(),
            transport.clone(),
        )
        .expect("source");
        source.list().await.expect("listing");
        let foreign = source
            .fetch("issue/1/attachment/1/foreign.txt")
            .await
            .expect_err("another origin");
        assert!(
            foreign.to_string().contains("not on the Jira origin"),
            "{foreign}"
        );
        let downgrade = source
            .fetch("issue/1/attachment/2/downgrade.txt")
            .await
            .expect_err("plain HTTP on the same host");
        assert!(
            downgrade.to_string().contains("not on the Jira origin"),
            "{downgrade}"
        );
        let redirected = source
            .fetch("issue/1/attachment/3/redirected.txt")
            .await
            .expect_err("a plain-HTTP redirect");
        assert!(
            redirected.to_string().contains("plain-HTTP location"),
            "{redirected}"
        );
        let built = source
            .fetch("issue/1/attachment/4/built.txt")
            .await
            .expect("built from the base URL");
        assert_eq!(built.bytes, b"abc");
        let seen = transport.seen();
        assert_eq!(
            seen.iter()
                .filter(|request| request.contains("attachment/content"))
                .count(),
            2,
            "{seen:?}"
        );
    }

    /// Review round 2, finding 4: a redirect to a login page, back to the
    /// Jira origin after leaving it, or to a host that is not a media host
    /// is refused, and so is an HTML page served as the file; Cloud's media
    /// host is followed without the credential.
    #[tokio::test]
    #[allow(clippy::too_many_lines)]
    async fn attachment_redirects_to_login_pages_or_back_to_the_origin_are_refused() {
        let transport = Scripted::new(|request, _| match request.url().path() {
            "/rest/api/3/search/jql" => {
                let mut issue = issue("1", "PROJ-1", "u1");
                issue["fields"]["attachment"] = json!([
                    {"id": "11", "filename": "ok.pdf", "size": 3, "mimeType": "application/pdf"},
                    {"id": "12", "filename": "login.pdf", "size": 3, "mimeType": "application/pdf"},
                    {"id": "13", "filename": "loop.pdf", "size": 3, "mimeType": "application/pdf"},
                    {"id": "14", "filename": "sso.pdf", "size": 3, "mimeType": "application/pdf"},
                    {"id": "15", "filename": "other.pdf", "size": 3, "mimeType": "application/pdf"},
                    {"id": "16", "filename": "page.html", "size": 3, "mimeType": "text/html"},
                    {"id": "17", "filename": "chain.pdf", "size": 3, "mimeType": "application/pdf"},
                ]);
                Reply::json(&json!({"issues": [issue], "isLast": true}))
            }
            "/rest/api/3/attachment/content/11" => Reply::status(StatusCode::SEE_OTHER).header(
                "location",
                "https://api.media.atlassian.com/file/aa/binary?token=t",
            ),
            "/file/aa/binary" => {
                assert!(request.headers().get("authorization").is_none());
                Reply::bytes(b"abc").header("content-type", "application/pdf")
            }
            "/rest/api/3/attachment/content/12" => Reply::status(StatusCode::FOUND).header(
                "location",
                "/login.jsp?permissionViolation=true&os_destination=%2Fx",
            ),
            "/rest/api/3/attachment/content/13" => {
                Reply::status(StatusCode::FOUND).header("location", "https://media.example/hop")
            }
            "/hop" => Reply::status(StatusCode::FOUND).header(
                "location",
                "https://acme.atlassian.net/rest/api/3/attachment/content/13",
            ),
            "/rest/api/3/attachment/content/14" => Reply::bytes(b"<html>Log in</html>")
                .header("content-type", "text/html;charset=UTF-8"),
            "/rest/api/3/attachment/content/15" => {
                Reply::status(StatusCode::FOUND).header("location", "https://evil.example/x")
            }
            "/rest/api/3/attachment/content/16" => {
                Reply::bytes(b"<p>x</p>").header("content-type", "text/html")
            }
            "/rest/api/3/attachment/content/17" => Reply::status(StatusCode::FOUND)
                .header("location", "/rest/api/3/attachment/content/17"),
            other => panic!("unexpected request {other}"),
        });
        let source = JiraSource::new(
            cloud(),
            arguments(&json!({"include_attachments": true})),
            allow(),
            transport.clone(),
        )
        .expect("source");
        source.list().await.expect("listing");
        let fetch = |key: &'static str| {
            let source = &source;
            async move { source.fetch(key).await }
        };
        assert_eq!(
            fetch("issue/1/attachment/11/ok.pdf")
                .await
                .expect("media host")
                .bytes,
            b"abc"
        );
        let login = fetch("issue/1/attachment/12/login.pdf")
            .await
            .expect_err("login");
        assert!(login.to_string().contains("login page"), "{login}");
        let back = fetch("issue/1/attachment/13/loop.pdf")
            .await
            .expect_err("back to the origin");
        assert!(back.to_string().contains("after leaving it"), "{back}");
        let sso = fetch("issue/1/attachment/14/sso.pdf")
            .await
            .expect_err("HTML for a pdf");
        assert!(sso.to_string().contains("HTML page"), "{sso}");
        let other = fetch("issue/1/attachment/15/other.pdf")
            .await
            .expect_err("other host");
        assert!(other.to_string().contains("media host"), "{other}");
        assert_eq!(
            fetch("issue/1/attachment/16/page.html")
                .await
                .expect("html")
                .bytes,
            b"<p>x</p>"
        );
        let looped = fetch("issue/1/attachment/17/chain.pdf")
            .await
            .expect_err("a loop");
        assert!(looped.to_string().contains("too many times"), "{looped}");
        let seen = transport.seen();
        assert!(
            !seen
                .iter()
                .any(|request| request.contains("login.jsp") || request.contains("evil.example")),
            "{seen:?}"
        );
        assert_eq!(
            seen.iter()
                .filter(|request| request.ends_with("/content/17"))
                .count(),
            crate::atlassian::MAX_DOWNLOAD_REDIRECTS + 1,
            "{seen:?}"
        );
    }

    /// Review round 2, finding 6: an issue whose response lacks `security`
    /// is restricted in a project with an issue security scheme, or when the
    /// scheme cannot be established; in a project without one, absence is
    /// unrestricted. The scheme is asked once per project per run.
    #[tokio::test]
    async fn an_issue_without_the_security_key_is_restricted_in_a_project_with_a_scheme() {
        fn bare(id: &str, key: &str, project: &str) -> Value {
            let mut issue = issue(id, key, "u1");
            issue["fields"].as_object_mut().unwrap().remove("security");
            issue["fields"]["project"] = json!({"key": project});
            issue
        }
        let transport = Scripted::new(|request, _| match request.url().path() {
            "/rest/api/3/search/jql" => {
                let mut with_null = bare("3", "SEC-3", "SEC");
                with_null["fields"]["security"] = Value::Null;
                let mut no_project = bare("7", "X-7", "X");
                no_project["fields"]
                    .as_object_mut()
                    .unwrap()
                    .remove("project");
                let issues = vec![
                    bare("1", "SEC-1", "SEC"),
                    bare("2", "SEC-2", "SEC"),
                    with_null,
                    bare("4", "OPEN-4", "OPEN"),
                    bare("5", "OPEN-5", "OPEN"),
                    bare("6", "ERR-6", "ERR"),
                    no_project,
                ];
                Reply::json(&json!({"issues": issues, "isLast": true}))
            }
            // A scheme the caller may read.
            "/rest/api/3/project/SEC/issuesecuritylevelscheme" => Reply::json(
                &json!({"id": 10000, "name": "Staff", "defaultSecurityLevelId": 10001, "levels": []}),
            ),
            // No scheme (or not allowed to read it), and no level visible.
            "/rest/api/3/project/OPEN/issuesecuritylevelscheme" => {
                Reply::status(StatusCode::NOT_FOUND)
            }
            "/rest/api/3/project/OPEN/securitylevel" => Reply::json(&json!({"levels": []})),
            // Cannot be established.
            "/rest/api/3/project/ERR/issuesecuritylevelscheme"
            | "/rest/api/3/project/ERR/securitylevel" => {
                Reply::status(StatusCode::INTERNAL_SERVER_ERROR)
            }
            other => panic!("unexpected request {other}"),
        });
        let source = JiraSource::new(cloud(), arguments(&json!({})), allow(), transport.clone())
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
            ["issue/3.md", "issue/4.md", "issue/5.md"],
            "SEC-1, SEC-2 (scheme, no key), ERR-6 (unknown scheme) and X-7 (no project) are out; \
             SEC-3 says `null`; OPEN has no scheme"
        );
        let seen = transport.seen();
        let asked = |path: &str| seen.iter().filter(|request| request.contains(path)).count();
        assert_eq!(
            asked("/project/SEC/issuesecuritylevelscheme"),
            1,
            "{seen:?}"
        );
        assert_eq!(
            asked("/project/OPEN/issuesecuritylevelscheme"),
            1,
            "{seen:?}"
        );
        assert_eq!(asked("/project/OPEN/securitylevel"), 1, "{seen:?}");
        assert!(transport.requests().first().is_some_and(|search| {
            query(search, "fields").is_some_and(|fields| fields.contains("security"))
        }));
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

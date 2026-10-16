//! TestRail as a `ContentSource` (ADR-0030 decision 3, stage I3).
//!
//! * **Configuration.** `testrail_configuration.{url,email,password}` (the
//!   toolkit settings the SDK and the agent-runtime family read): the
//!   instance URL (HTTPS, an instance under a path prefix is kept), the user
//!   and the password or API key, sent as Basic to that origin only.
//! * **List.** Every test case of `project_id`, 250 a page (`limit` and
//!   `offset`), following `_links.next` until it is null; a server that
//!   answers with a bare array (before TestRail 6.7) is read as one page.
//!   Suite handling is the SDK's: `get_project` says the suite mode; mode 1
//!   (a single suite) lists the project's cases directly, modes 2 and 3 list
//!   one suite (`suite_id`) or every suite (`get_suites`, paged). The
//!   `section_id` and `title_keyword` filters narrow the cases as the SDK
//!   does. The document cap counts the cases kept after those filters; the rows read are bounded by the page cap.
//! * **Dependents.** With `include_attachments`, each case's attachments
//!   (`get_attachments_for_case`, paged), less `skip_attachment_extensions`,
//!   are documents of their own with `parent_key`. A case is
//!   `case/{id}.json`, versioned by its `updated_on`; an attachment is
//!   `case/{id}/attachment/{attachment id}/{file name}`, versioned by its id
//!   and size (an attachment is immutable in TestRail). The SDK gives every
//!   attachment the case's `updated_on` instead.
//! * **Fetch.** A case's document is the case as listed (`get_cases`
//!   returns the same fields as `get_case`), as JSON, which is what the SDK
//!   indexes (`json.dumps(case)`); nothing is read again, and a case whose
//!   listed `updated_on` is not the listed version is refused as
//!   `SourceError::Changed`. An attachment is downloaded
//!   (`get_attachment/{id}`) through the shared attachment downloader: the
//!   credential goes to the configured origin only, its redirect rules
//!   apply, and the body is within the document cap.
//! * **Auth and limits.** A 401 or 403 on any call is an error naming the
//!   credential, never an empty listing (the SDK swallowed a per-suite
//!   failure and went on); a 429 backs off within the shared bound.
//!   TestRail has no per-case permission, so a case the credential can list
//!   is one the credential's role may read.

use std::sync::Arc;

use elitea_content_source::{ContentSource, Document, DocumentRef, SourceError};
use futures_util::StreamExt as _;
use serde_json::{Map, Value};
use url::Url;

use crate::atlassian::{
    AtlassianConfigError, AttachmentDownloader, COMMENT_LISTING_CONCURRENCY, DetailedListing,
    Details, authorized_json, id_of, key_segment, optional_flag, refused_credential,
    safe_identifier, size_of, text_of,
};
use crate::base_url::BasePath;
use crate::egress::HostAllowlist;
use crate::source::{Backoff, Failure, Listed, SourceHttp, SourceLimits, Timeouts, references};
use crate::testmgmt::{
    MAX_IDENTITY_BYTES, basic, extension_list, extension_skipped, https_base, identifier_argument,
    required_text, scalar_text, take_list, text_argument,
};
use crate::transport::header::{ACCEPT, AUTHORIZATION, HeaderValue};
use crate::transport::{Method, Request, Transport};

/// What the error messages call this provider.
pub const PROVIDER: &str = "TestRail";
/// TestRail's `limit` ceiling.
const PAGE_SIZE: usize = 250;

/// Invocation-scoped TestRail authority for one index run.
pub struct TestRailConnectorConfig {
    base: Url,
    authorization: HeaderValue,
}

impl TestRailConnectorConfig {
    /// Read the toolkit `settings`.
    ///
    /// # Errors
    ///
    /// A missing `testrail_configuration`, a base URL that is not HTTPS, or
    /// a missing user or password.
    pub fn parse(settings: &Map<String, Value>) -> Result<Self, AtlassianConfigError> {
        let configuration = settings
            .get("testrail_configuration")
            .and_then(Value::as_object)
            .ok_or(crate::testmgmt::refuse(
                PROVIDER,
                crate::atlassian::AtlassianConfigErrorCode::InvalidConfiguration,
            ))?;
        let base = https_base(
            PROVIDER,
            required_text(PROVIDER, configuration, "url", 2 * 1_024)?,
            BasePath::Prefix,
        )?;
        let email = required_text(PROVIDER, configuration, "email", MAX_IDENTITY_BYTES)?;
        let password = required_text(
            PROVIDER,
            configuration,
            "password",
            crate::atlassian::MAX_SECRET_BYTES,
        )?;
        Ok(Self {
            base,
            authorization: basic(PROVIDER, email, password)?,
        })
    }

    #[must_use]
    pub fn base_url(&self) -> &Url {
        &self.base
    }
}

/// The `index_data` arguments the connector honours (the SDK's
/// `_index_tool_params`, plus `title_keyword` and `section_id`, which its
/// loader reads). `chunking_tool` is the worker's.
#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct TestRailIndexArguments {
    pub project_id: String,
    pub suite_id: Option<String>,
    pub section_id: Option<String>,
    pub title_keyword: Option<String>,
    pub include_attachments: bool,
    pub skip_attachment_extensions: Vec<String>,
}

impl TestRailIndexArguments {
    /// Read the `index_data` arguments.
    ///
    /// # Errors
    ///
    /// No `project_id`, or an argument of the wrong type.
    pub fn parse(arguments: &Map<String, Value>) -> Result<Self, AtlassianConfigError> {
        let project_id = identifier_argument(PROVIDER, arguments, "project_id")?.ok_or(
            crate::testmgmt::refuse(
                PROVIDER,
                crate::atlassian::AtlassianConfigErrorCode::InvalidConfiguration,
            ),
        )?;
        Ok(Self {
            project_id,
            suite_id: identifier_argument(PROVIDER, arguments, "suite_id")?,
            section_id: identifier_argument(PROVIDER, arguments, "section_id")?,
            title_keyword: text_argument(PROVIDER, arguments, "title_keyword")?,
            include_attachments: optional_flag(arguments, "include_attachments", PROVIDER)?
                .unwrap_or_default(),
            skip_attachment_extensions: extension_list(
                PROVIDER,
                arguments,
                "skip_attachment_extensions",
            )?,
        })
    }
}

/// One TestRail project's test cases and their attachments.
pub struct TestRailSource {
    config: TestRailConnectorConfig,
    arguments: TestRailIndexArguments,
    http: SourceHttp,
    downloads: AttachmentDownloader,
    listing: DetailedListing,
}

impl TestRailSource {
    /// The toolkit `config` indexed with `arguments`, every request through
    /// `allowlist` (fail-closed) and then `transport`.
    ///
    /// # Errors
    ///
    /// The TestRail host is not on the allowlist.
    pub fn new(
        config: TestRailConnectorConfig,
        arguments: TestRailIndexArguments,
        allowlist: HostAllowlist,
        transport: Arc<dyn Transport>,
    ) -> Result<Self, SourceError> {
        let downloads = AttachmentDownloader::new(config.base_url(), &allowlist);
        let http = SourceHttp::new(allowlist, transport);
        if !http.permits(config.base_url()) {
            return Err(Failure::Refused.into_source_error(PROVIDER, None));
        }
        Ok(Self {
            config,
            arguments,
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

    /// `{base}/index.php?/api/v2/{endpoint}&{params}`: TestRail's own URL
    /// form, the route in the query. `endpoint` is built from fixed names
    /// and checked identifiers only.
    fn api(&self, endpoint: &str, params: &[(&str, String)]) -> Result<Url, Failure> {
        let mut target = format!(
            "{}/index.php?/api/v2/{endpoint}",
            self.config.base.as_str().trim_end_matches('/')
        );
        for (name, value) in params {
            target.push('&');
            target.push_str(name);
            target.push('=');
            target.extend(url::form_urlencoded::byte_serialize(value.as_bytes()));
        }
        Url::parse(&target).map_err(|_| Failure::Client("the TestRail URL is invalid".to_owned()))
    }

    fn get(&self, url: Url) -> Request {
        let mut request = Request::new(Method::GET, url);
        let headers = request.headers_mut();
        headers.insert(ACCEPT, HeaderValue::from_static("application/json"));
        headers.insert(AUTHORIZATION, self.config.authorization.clone());
        request
    }

    async fn json(&self, endpoint: &str, params: &[(&str, String)]) -> Result<Value, Failure> {
        authorized_json(&self.http, self.get(self.api(endpoint, params)?), PROVIDER).await
    }

    /// Every item of a paged TestRail list: `field` of each page until
    /// `_links.next` is null, or the whole list when the server answers with
    /// a bare array (before TestRail 6.7).
    async fn paged(
        &self,
        endpoint: &str,
        params: &[(&str, String)],
        field: &str,
    ) -> Result<Vec<Value>, Failure> {
        self.paged_where(endpoint, params, field, |_| true).await
    }

    /// Like [`Self::paged`], keeping only the items `keep` accepts. The
    /// document cap counts the kept items; the rows read are bounded by the
    /// page cap, so a narrow filter over a large project is not refused for
    /// the project's size.
    async fn paged_where(
        &self,
        endpoint: &str,
        params: &[(&str, String)],
        field: &str,
        keep: impl Fn(&Value) -> bool,
    ) -> Result<Vec<Value>, Failure> {
        let mut items: Vec<Value> = Vec::new();
        let mut offset = 0_usize;
        let mut pages_seen = 0_usize;
        loop {
            pages_seen += 1;
            self.http.check_pages(pages_seen)?;
            let mut query = params.to_vec();
            query.push(("limit", PAGE_SIZE.to_string()));
            query.push(("offset", offset.to_string()));
            let mut page = self.json(endpoint, &query).await?;
            if let Value::Array(all) = page {
                items.extend(all.into_iter().filter(|item| keep(item)));
                self.http.check_count(items.len())?;
                return Ok(items);
            }
            let batch = take_list(&mut page, field, "a TestRail page holds no list")?;
            let got = batch.len();
            items.extend(batch.into_iter().filter(|item| keep(item)));
            self.http.check_count(items.len())?;
            let more = page
                .pointer("/_links/next")
                .and_then(Value::as_str)
                .is_some_and(|next| !next.is_empty());
            if !more || got == 0 {
                return Ok(items);
            }
            offset += got;
        }
    }

    /// The suites to list cases from: none (the project's cases directly)
    /// in single-suite mode, else the named suite or every suite.
    async fn suites(&self) -> Result<Vec<Option<String>>, Failure> {
        let project = self
            .json(&format!("get_project/{}", self.arguments.project_id), &[])
            .await?;
        let mode = project
            .get("suite_mode")
            .and_then(Value::as_u64)
            .unwrap_or(1);
        if mode != 2 && mode != 3 {
            return Ok(vec![None]);
        }
        if let Some(suite) = &self.arguments.suite_id {
            return Ok(vec![Some(suite.clone())]);
        }
        let suites = self
            .paged(
                &format!("get_suites/{}", self.arguments.project_id),
                &[],
                "suites",
            )
            .await?;
        let mut ids = Vec::new();
        for suite in &suites {
            let id = id_of(suite, "/id").ok_or(Failure::InvalidResponse("a suite has no id"))?;
            if !safe_identifier(&id) {
                return Err(Failure::InvalidResponse("a suite id is not an identifier"));
            }
            ids.push(Some(id));
        }
        Ok(ids)
    }

    fn case_url(&self, id: &str) -> String {
        format!(
            "{}/index.php?/cases/view/{id}",
            self.config.base.as_str().trim_end_matches('/')
        )
    }

    async fn listing(&self) -> Result<(Vec<Listed>, Details), Failure> {
        let mut listed = Vec::new();
        let mut details = Details::new();
        let mut case_ids: Vec<String> = Vec::new();
        let keyword = self
            .arguments
            .title_keyword
            .as_deref()
            .map(str::to_lowercase);
        for suite in self.suites().await? {
            let mut params: Vec<(&str, String)> = Vec::new();
            if let Some(suite) = &suite {
                params.push(("suite_id", suite.clone()));
            }
            if let Some(section) = &self.arguments.section_id {
                params.push(("section_id", section.clone()));
            }
            let section = self.arguments.section_id.as_deref();
            let cases = self
                .paged_where(
                    &format!("get_cases/{}", self.arguments.project_id),
                    &params,
                    "cases",
                    |case| {
                        section.is_none_or(|section| {
                            id_of(case, "/section_id").as_deref() == Some(section)
                        }) && keyword.as_ref().is_none_or(|keyword| {
                            text_of(case, "/title")
                                .unwrap_or_default()
                                .to_lowercase()
                                .contains(keyword.as_str())
                        })
                    },
                )
                .await?;
            for case in cases {
                let id = id_of(&case, "/id").ok_or(Failure::InvalidResponse("a case has no id"))?;
                if !safe_identifier(&id) {
                    return Err(Failure::InvalidResponse("a case id is not an identifier"));
                }
                let key = format!("case/{id}.json");
                if details.contains_key(&key) {
                    continue;
                }
                let version = case
                    .get("updated_on")
                    .and_then(scalar_text)
                    .or_else(|| case.get("created_on").and_then(scalar_text))
                    .ok_or(Failure::InvalidResponse("a case carries no `updated_on`"))?;
                listed.push(Listed::new(key.clone(), version, 0, id.clone()));
                self.http.check_count(listed.len())?;
                let mut case_details = Map::new();
                case_details.insert("type".into(), Value::String("testrail_test_case".into()));
                if let Some(suite) = suite.as_ref().or(self.arguments.suite_id.as_ref()) {
                    case_details.insert("suite_id".into(), Value::String(suite.clone()));
                }
                // The listed case is the content: a fetch reads nothing
                // again (as the sibling connectors).
                case_details.insert("case".into(), case);
                details.insert(key, case_details);
                case_ids.push(id);
            }
        }
        if self.arguments.include_attachments {
            let mut found = futures_util::stream::iter(case_ids.into_iter().map(|id| async move {
                let attachments = self
                    .paged(
                        &format!("get_attachments_for_case/{id}"),
                        &[],
                        "attachments",
                    )
                    .await?;
                Ok::<_, Failure>((id, attachments))
            }))
            .buffer_unordered(COMMENT_LISTING_CONCURRENCY);
            while let Some(result) = found.next().await {
                let (case_id, attachments) = result?;
                self.keep_attachments(&case_id, &attachments, &mut listed, &mut details)?;
            }
        }
        Ok((listed, details))
    }

    fn keep_attachments(
        &self,
        case_id: &str,
        attachments: &[Value],
        listed: &mut Vec<Listed>,
        details: &mut Details,
    ) -> Result<(), Failure> {
        let parent_key = format!("case/{case_id}.json");
        for attachment in attachments {
            let id = id_of(attachment, "/id")
                .ok_or(Failure::InvalidResponse("an attachment has no id"))?;
            if !safe_identifier(&id) {
                return Err(Failure::InvalidResponse(
                    "an attachment id is not an identifier",
                ));
            }
            let filename = text_of(attachment, "/filename")
                .or_else(|| text_of(attachment, "/name"))
                .unwrap_or("attachment");
            if extension_skipped(&self.arguments.skip_attachment_extensions, filename) {
                continue;
            }
            let size = size_of(attachment, "/size");
            let key = format!("case/{case_id}/attachment/{id}/{}", key_segment(filename));
            listed.push(Listed::new(
                key.clone(),
                format!("{id}:{size}"),
                size,
                id.clone(),
            ));
            self.http.check_count(listed.len())?;
            let mut meta = Map::new();
            meta.insert("type".into(), Value::String("attachment".into()));
            meta.insert("id".into(), Value::String(format!("attach_{id}")));
            meta.insert("filename".into(), Value::String(filename.to_owned()));
            meta.insert(
                "entity_type".into(),
                Value::String("test_case_attachment".into()),
            );
            meta.insert("parent_key".into(), Value::String(parent_key.clone()));
            meta.insert("parent_id".into(), Value::String(case_id.to_owned()));
            meta.insert(
                "project_id".into(),
                Value::String(self.arguments.project_id.clone()),
            );
            meta.insert("url".into(), Value::String(self.case_url(case_id)));
            if let Some(extension) = filename.rsplit_once('.').map(|(_, extension)| extension) {
                meta.insert(
                    "filetype".into(),
                    Value::String(extension.to_ascii_lowercase()),
                );
            }
            if let Some(created) = attachment.get("created_on").and_then(scalar_text) {
                meta.insert("created_on".into(), Value::String(created));
            }
            if let Some(is_image) = attachment.get("is_image").and_then(Value::as_bool) {
                meta.insert("is_image".into(), Value::Bool(is_image));
            }
            details.insert(key, meta);
        }
        Ok(())
    }

    /// The SDK's per-case metadata.
    fn case_metadata(&self, case: &Value, suite: Option<&str>) -> Map<String, Value> {
        let mut meta = Map::new();
        let text = |value: Option<&Value>| value.and_then(scalar_text).unwrap_or_default();
        meta.insert(
            "project_id".into(),
            Value::String(self.arguments.project_id.clone()),
        );
        meta.insert("title".into(), Value::String(text(case.get("title"))));
        meta.insert(
            "suite_id".into(),
            Value::String(
                suite
                    .map(ToOwned::to_owned)
                    .or_else(|| case.get("suite_id").and_then(scalar_text))
                    .unwrap_or_default(),
            ),
        );
        meta.insert("id".into(), Value::String(text(case.get("id"))));
        meta.insert(
            "updated_on".into(),
            Value::String(text(case.get("updated_on"))),
        );
        let labels: Vec<Value> = case
            .get("labels")
            .and_then(Value::as_array)
            .into_iter()
            .flatten()
            .filter_map(|label| label.get("title").and_then(Value::as_str))
            .map(|title| Value::String(title.to_owned()))
            .collect();
        meta.insert("labels".into(), Value::Array(labels));
        meta.insert("type".into(), Value::String("testrail_test_case".into()));
        meta.insert("entity_type".into(), Value::String("test_case".into()));
        for (name, field) in [
            ("priority", "priority_id"),
            ("milestone", "milestone_id"),
            ("automation_type", "custom_automation_type"),
            ("section_id", "section_id"),
        ] {
            if let Some(value) = case.get(field).filter(|value| !value.is_null()) {
                meta.insert(name.into(), value.clone());
            }
        }
        if let Some(estimate) = case.get("estimate").and_then(Value::as_str) {
            meta.insert("estimate".into(), Value::String(estimate.to_owned()));
        }
        meta
    }
}

impl ContentSource for TestRailSource {
    async fn list(&self) -> Result<Vec<DocumentRef>, SourceError> {
        let listing = self
            .listing
            .list_shared(PROVIDER, || self.listing())
            .await?;
        Ok(references(&listing.listing))
    }

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
            .unwrap_or("testrail_test_case")
            .to_owned();
        let (bytes, title, uri) = if kind == "attachment" {
            self.http
                .check_size(listed.reference.size)
                .map_err(failed)?;
            let url = self
                .api(&format!("get_attachment/{}", listed.handle), &[])
                .map_err(failed)?;
            let filename = metadata
                .get("filename")
                .and_then(Value::as_str)
                .unwrap_or("attachment");
            // The shared downloader: the credential goes to the configured
            // origin only, and its redirect rules apply.
            let bytes = self
                .downloads
                .download(&self.http, url, |url| Ok(self.get(url)), filename, None)
                .await
                .map_err(|failure| failed(refused_credential(failure, PROVIDER)))?;
            let title = metadata
                .get("filename")
                .and_then(Value::as_str)
                .unwrap_or("attachment")
                .to_owned();
            let uri = metadata
                .get("url")
                .and_then(Value::as_str)
                .map(ToOwned::to_owned);
            (bytes, title, uri)
        } else {
            let Some(case) = metadata.remove("case") else {
                return Err(failed(Failure::InvalidResponse(
                    "a listed case holds no content",
                )));
            };
            let version = case
                .get("updated_on")
                .and_then(scalar_text)
                .or_else(|| case.get("created_on").and_then(scalar_text))
                .ok_or_else(|| {
                    failed(Failure::InvalidResponse("a case carries no `updated_on`"))
                })?;
            if version != listed.reference.version {
                return Err(failed(Failure::Changed));
            }
            let suite = metadata
                .get("suite_id")
                .and_then(Value::as_str)
                .map(ToOwned::to_owned);
            let content = serde_json::to_vec(&case)
                .map_err(|_| failed(Failure::InvalidResponse("a case cannot be encoded")))?;
            let title = text_of(&case, "/title")
                .map_or_else(|| format!("Case {}", listed.handle), ToOwned::to_owned);
            metadata = self.case_metadata(&case, suite.as_deref());
            (content, title, Some(self.case_url(&listed.handle)))
        };
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

#[cfg(test)]
mod tests {
    use super::*;
    use crate::source::fixture::{Reply, Scripted, query};
    use crate::transport::StatusCode;
    use serde_json::json;
    use std::time::Duration;

    fn config() -> TestRailConnectorConfig {
        TestRailConnectorConfig::parse(
            json!({"testrail_configuration": {
                "url": "https://acme.testrail.io", "email": "qa@acme.example", "password": "api-key"
            }})
            .as_object()
            .unwrap(),
        )
        .expect("config")
    }

    fn arguments(value: &Value) -> TestRailIndexArguments {
        TestRailIndexArguments::parse(value.as_object().unwrap()).expect("arguments")
    }

    fn allow() -> HostAllowlist {
        HostAllowlist::parse(Some("acme.testrail.io"))
    }

    /// A case as TestRail's `get_cases` documents it.
    fn case(id: u64, title: &str, updated_on: u64, section: u64) -> Value {
        json!({
            "id": id, "title": title, "section_id": section, "template_id": 1,
            "type_id": 7, "priority_id": 2, "milestone_id": null, "refs": null,
            "created_by": 1, "created_on": 1_700_000_000, "updated_by": 1,
            "updated_on": updated_on, "estimate": "30s", "estimate_forecast": null,
            "suite_id": 1,
            "custom_preconds": "logged in",
            "custom_steps_separated": [{"content": "Open the page", "expected": "It opens"}],
            "labels": [{"id": 1, "title": "smoke"}],
        })
    }

    fn page(cases: &[Value], next: Option<&str>, offset: u64) -> Value {
        json!({
            "offset": offset, "limit": 250, "size": cases.len(),
            "_links": {"next": next, "prev": null},
            "cases": cases,
        })
    }

    #[test]
    fn the_configuration_is_strict() {
        for bad in [
            json!({}),
            json!({"testrail_configuration": {"url": "http://acme.testrail.io", "email": "a", "password": "b"}}),
            json!({"testrail_configuration": {"url": "https://acme.testrail.io", "email": "a"}}),
            json!({"testrail_configuration": {"url": "https://acme.testrail.io", "email": "a:b", "password": "b"}}),
        ] {
            assert!(
                TestRailConnectorConfig::parse(bad.as_object().unwrap()).is_err(),
                "{bad}"
            );
        }
        assert!(
            TestRailIndexArguments::parse(&Map::new()).is_err(),
            "project_id"
        );
        let parsed = arguments(
            &json!({"project_id": 3, "suite_id": "9", "skip_attachment_extensions": ["PNG"]}),
        );
        assert_eq!(parsed.project_id, "3");
        assert_eq!(parsed.skip_attachment_extensions, vec![".png"]);
    }

    #[tokio::test]
    async fn multiple_suites_are_listed_page_by_page_and_a_case_is_the_sdk_json() {
        let transport = Scripted::new(|request, _| {
            let route = request.url().query().unwrap_or_default().to_owned();
            assert_eq!(request.url().path(), "/index.php");
            assert!(
                request
                    .headers()
                    .get("authorization")
                    .is_some_and(crate::transport::HeaderValue::is_sensitive),
                "the credential is sent, marked sensitive"
            );
            if route.starts_with("/api/v2/get_project/5") {
                Reply::json(&json!({"id": 5, "name": "Shop", "suite_mode": 3}))
            } else if route.starts_with("/api/v2/get_suites/5") {
                Reply::json(&json!({"offset": 0, "limit": 250, "size": 2,
                    "_links": {"next": null, "prev": null},
                    "suites": [{"id": 1, "name": "Master"}, {"id": 2, "name": "Other"}]}))
            } else if route.starts_with("/api/v2/get_cases/5") {
                let suite = query(request, "suite_id");
                match (suite.as_deref(), query(request, "offset").as_deref()) {
                    (Some("1"), Some("0")) => Reply::json(&page(
                        &[case(11, "Login works", 1_700_000_100, 4)],
                        Some("/api/v2/get_cases/5&limit=250&offset=1"),
                        0,
                    )),
                    (Some("1"), Some("1")) => Reply::json(&page(
                        &[case(12, "Logout works", 1_700_000_200, 4)],
                        None,
                        1,
                    )),
                    (Some("2"), Some("0")) => Reply::json(&page(
                        &[case(13, "Search works", 1_700_000_300, 5)],
                        None,
                        0,
                    )),
                    other => panic!("unexpected cases request {other:?}"),
                }
            } else {
                panic!("unexpected request {route}")
            }
        });
        let source = TestRailSource::new(
            config(),
            arguments(&json!({"project_id": "5"})),
            allow(),
            transport.clone(),
        )
        .expect("source");
        let listed = source.list().await.expect("list");
        let keys: Vec<_> = listed
            .iter()
            .map(|r| (r.key.as_str(), r.version.as_str()))
            .collect();
        assert_eq!(
            keys,
            [
                ("case/11.json", "1700000100"),
                ("case/12.json", "1700000200"),
                ("case/13.json", "1700000300"),
            ]
        );
        assert!(listed.iter().all(|r| r.mime == "application/json"));
        let requests_after_list = transport.requests().len();
        let document = source.fetch("case/12.json").await.expect("fetch");
        // The listed case is the content: a fetch asks TestRail for nothing.
        assert_eq!(transport.requests().len(), requests_after_list);
        assert_eq!(document.title, "Logout works");
        let body: Value = serde_json::from_slice(&document.bytes).expect("JSON");
        assert_eq!(
            body["custom_steps_separated"][0]["content"],
            "Open the page"
        );
        assert_eq!(document.metadata["type"], "testrail_test_case");
        assert_eq!(document.metadata["suite_id"], "1");
        assert_eq!(document.metadata["labels"], json!(["smoke"]));
        assert_eq!(document.metadata["priority"], 2);
        assert!(
            document
                .uri
                .as_deref()
                .unwrap()
                .ends_with("/index.php?/cases/view/12")
        );
        // Every request went to the configured origin.
        assert!(
            transport
                .requests()
                .iter()
                .all(|r| r.url().host_str() == Some("acme.testrail.io"))
        );
    }

    #[tokio::test]
    async fn single_suite_mode_lists_the_project_and_the_old_array_form_is_one_page() {
        let transport = Scripted::new(|request, _| {
            let route = request.url().query().unwrap_or_default().to_owned();
            if route.starts_with("/api/v2/get_project/5") {
                Reply::json(&json!({"id": 5, "suite_mode": 1}))
            } else if route.starts_with("/api/v2/get_cases/5") {
                assert!(query(request, "suite_id").is_none());
                // TestRail before 6.7: a bare array.
                Reply::json(&json!([
                    case(1, "Alpha login", 10, 1),
                    case(2, "Beta checkout", 20, 2),
                ]))
            } else {
                panic!("unexpected request {route}")
            }
        });
        let source = TestRailSource::new(
            config(),
            arguments(&json!({"project_id": "5", "title_keyword": "LOGIN"})),
            allow(),
            transport,
        )
        .expect("source");
        let listed = source.list().await.expect("list");
        assert_eq!(listed.len(), 1);
        assert_eq!(listed[0].key, "case/1.json");
    }

    #[tokio::test]
    async fn attachments_are_dependents_with_their_own_versions_and_come_from_the_origin() {
        let transport = Scripted::new(|request, _| {
            let route = request.url().query().unwrap_or_default().to_owned();
            if route.starts_with("/api/v2/get_project/") {
                Reply::json(&json!({"id": 5, "suite_mode": 1}))
            } else if route.starts_with("/api/v2/get_cases/5") {
                Reply::json(&page(&[case(11, "Login", 100, 4)], None, 0))
            } else if route.starts_with("/api/v2/get_attachments_for_case/11") {
                Reply::json(&json!({"offset": 0, "limit": 250, "size": 2,
                "_links": {"next": null, "prev": null},
                "attachments": [
                    {"id": 301, "name": "notes.txt", "filename": "notes.txt", "size": 5,
                     "created_on": 1_700_000_000, "project_id": 5, "case_id": 11, "is_image": false},
                    {"id": 302, "name": "shot.PNG", "filename": "shot.PNG", "size": 9,
                     "created_on": 1_700_000_000, "project_id": 5, "case_id": 11, "is_image": true},
                ]}))
            } else if route.starts_with("/api/v2/get_attachment/301") {
                Reply::bytes(b"hello")
            } else {
                panic!("unexpected request {route}")
            }
        });
        let source = TestRailSource::new(
            config(),
            arguments(&json!({"project_id": "5", "include_attachments": true, "skip_attachment_extensions": [".png"]})),
            allow(),
            transport.clone(),
        )
        .expect("source");
        let listed = source.list().await.expect("list");
        let by_key: std::collections::BTreeMap<_, _> =
            listed.iter().map(|r| (r.key.as_str(), r)).collect();
        assert_eq!(by_key.len(), 2, "the png is skipped: {by_key:?}");
        let attachment = by_key["case/11/attachment/301/notes.txt"];
        assert_eq!(attachment.version, "301:5");
        assert_eq!(by_key["case/11.json"].version, "100");
        let document = source
            .fetch("case/11/attachment/301/notes.txt")
            .await
            .expect("attachment");
        assert_eq!(document.bytes, b"hello");
        assert_eq!(document.metadata["parent_key"], "case/11.json");
        assert_eq!(document.metadata["entity_type"], "test_case_attachment");
        assert_eq!(document.metadata["id"], "attach_301");
    }

    #[tokio::test]
    async fn a_fetch_uses_the_listed_case_and_never_reads_get_case() {
        let transport = Scripted::new(|request, _| {
            let route = request.url().query().unwrap_or_default().to_owned();
            if route.starts_with("/api/v2/get_project/") {
                Reply::json(&json!({"suite_mode": 1}))
            } else if route.starts_with("/api/v2/get_cases/") {
                Reply::json(&page(&[case(1, "A", 10, 1), case(2, "B", 20, 1)], None, 0))
            } else {
                panic!("unexpected request {route}")
            }
        });
        let source = TestRailSource::new(
            config(),
            arguments(&json!({"project_id": "5"})),
            allow(),
            transport.clone(),
        )
        .expect("source");
        source.list().await.expect("list");
        let listed_requests = transport.requests().len();
        for key in ["case/1.json", "case/2.json"] {
            let document = source.fetch(key).await.expect("fetch");
            assert_eq!(document.reference.version, document.metadata["updated_on"]);
        }
        assert_eq!(transport.requests().len(), listed_requests);
    }

    #[tokio::test]
    async fn an_attachment_download_follows_the_shared_rules() {
        // 401/403 name the credential; a redirect off the origin to a host
        // that is not allowed is refused and never receives the credential.
        for (reply, expected) in [
            (
                StatusCode::UNAUTHORIZED,
                "refused the credential (HTTP 401)",
            ),
            (StatusCode::FORBIDDEN, "refused the credential (HTTP 403)"),
            (StatusCode::SEE_OTHER, "not an allowed"),
        ] {
            let transport = Scripted::new(move |request, _| {
                let route = request.url().query().unwrap_or_default().to_owned();
                if route.starts_with("/api/v2/get_project/") {
                    Reply::json(&json!({"suite_mode": 1}))
                } else if route.starts_with("/api/v2/get_cases/5") {
                    Reply::json(&page(&[case(11, "Login", 100, 4)], None, 0))
                } else if route.starts_with("/api/v2/get_attachments_for_case/11") {
                    Reply::json(&json!({"_links": {"next": null}, "attachments": [
                        {"id": 301, "filename": "notes.txt", "size": 5}]}))
                } else if route.starts_with("/api/v2/get_attachment/301") {
                    if reply == StatusCode::SEE_OTHER {
                        Reply::status(reply).header("location", "https://evil.example/blob")
                    } else {
                        Reply::status(reply)
                    }
                } else {
                    assert_eq!(request.url().host_str(), Some("acme.testrail.io"));
                    panic!("unexpected request {route}")
                }
            });
            let source = TestRailSource::new(
                config(),
                arguments(&json!({"project_id": "5", "include_attachments": true})),
                allow(),
                transport.clone(),
            )
            .expect("source");
            source.list().await.expect("list");
            let error = source
                .fetch("case/11/attachment/301/notes.txt")
                .await
                .expect_err("refused")
                .to_string();
            assert!(error.contains(expected), "{error}");
            assert!(
                transport
                    .requests()
                    .iter()
                    .all(|r| r.url().host_str() == Some("acme.testrail.io"))
            );
        }
    }

    #[tokio::test]
    async fn a_refused_credential_is_named_and_never_an_empty_listing() {
        for status in [StatusCode::UNAUTHORIZED, StatusCode::FORBIDDEN] {
            let transport = Scripted::new(move |_, _| Reply::status(status));
            let source = TestRailSource::new(
                config(),
                arguments(&json!({"project_id": "5"})),
                allow(),
                transport,
            )
            .expect("source");
            let error = source.list().await.expect_err("refused");
            let text = error.to_string();
            assert!(text.contains("refused the credential"), "{text}");
            assert!(!text.contains("api-key"), "{text}");
        }
        // One suite failing among several fails the listing (the SDK went on).
        let transport = Scripted::new(|request, _| {
            let route = request.url().query().unwrap_or_default().to_owned();
            if route.starts_with("/api/v2/get_project/") {
                Reply::json(&json!({"suite_mode": 3}))
            } else if route.starts_with("/api/v2/get_suites/") {
                Reply::json(&json!([{"id": 1}, {"id": 2}]))
            } else if query(request, "suite_id").as_deref() == Some("1") {
                Reply::json(&page(&[case(1, "A", 10, 1)], None, 0))
            } else {
                Reply::status(StatusCode::FORBIDDEN)
            }
        });
        let source = TestRailSource::new(
            config(),
            arguments(&json!({"project_id": "5"})),
            allow(),
            transport,
        )
        .expect("source");
        assert!(source.list().await.is_err());
    }

    #[tokio::test(start_paused = true)]
    async fn a_rate_limit_waits_for_retry_after_and_then_lists() {
        let transport = Scripted::new(|request, index| {
            if index == 0 {
                return Reply::rate_limited(Some("2"));
            }
            let route = request.url().query().unwrap_or_default().to_owned();
            if route.starts_with("/api/v2/get_project/") {
                Reply::json(&json!({"suite_mode": 1}))
            } else {
                Reply::json(&page(&[case(1, "A", 10, 1)], None, 0))
            }
        });
        let source = TestRailSource::new(
            config(),
            arguments(&json!({"project_id": "5"})),
            allow(),
            transport.clone(),
        )
        .expect("source");
        let started = tokio::time::Instant::now();
        assert_eq!(source.list().await.expect("after the wait").len(), 1);
        assert_eq!(started.elapsed(), Duration::from_secs(2));

        let transport = Scripted::new(|_, _| Reply::rate_limited(Some("1")));
        let source = TestRailSource::new(
            config(),
            arguments(&json!({"project_id": "5"})),
            allow(),
            transport,
        )
        .expect("source")
        .with_backoff(Backoff {
            max_attempts: 2,
            ..Backoff::default()
        });
        assert!(
            source
                .list()
                .await
                .expect_err("limited")
                .to_string()
                .contains("rate limited")
        );
    }

    #[tokio::test]
    async fn the_document_cap_counts_kept_cases_not_the_projects_size() {
        let transport = || {
            Scripted::new(|request, _| {
                let route = request.url().to_string();
                if route.contains("get_project/5") {
                    Reply::json(&json!({"id": 5, "suite_mode": 1}))
                } else {
                    let cases: Vec<Value> = (1..=10)
                        .map(|id| {
                            let title = if id <= 2 { "Login" } else { "Other" };
                            case(id, &format!("{title} {id}"), 1_700_000_000 + id, 4)
                        })
                        .collect();
                    Reply::json(&page(&cases, None, 0))
                }
            })
        };
        let limits = SourceLimits {
            max_documents: 3,
            ..SourceLimits::default()
        };
        let narrow = TestRailSource::new(
            config(),
            arguments(&json!({"project_id": "5", "title_keyword": "login"})),
            allow(),
            transport(),
        )
        .unwrap()
        .with_limits(limits);
        assert_eq!(narrow.list().await.unwrap().len(), 2);
        let wide = TestRailSource::new(
            config(),
            arguments(&json!({"project_id": "5"})),
            allow(),
            transport(),
        )
        .unwrap()
        .with_limits(limits);
        assert!(wide.list().await.is_err());
    }

    #[tokio::test]
    async fn a_host_off_the_allowlist_is_refused_before_any_request() {
        let transport = Scripted::new(|_, _| panic!("no request may be sent"));
        assert!(
            TestRailSource::new(
                config(),
                arguments(&json!({"project_id": "5"})),
                HostAllowlist::parse(Some("github.com")),
                transport
            )
            .is_err()
        );
    }
}

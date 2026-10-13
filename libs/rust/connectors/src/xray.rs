//! Xray Cloud as a `ContentSource` (ADR-0030 decision 3, stage I3). The
//! toolkit type is `xray_cloud`.
//!
//! * **Configuration.** `xray_configuration.{base_url,client_id,client_secret}`:
//!   the Xray Cloud origin for the region (default
//!   `https://xray.cloud.getxray.app`, HTTPS, no path), and the API key's
//!   client id and secret. The connector authenticates once per run
//!   (`POST /api/v1/authenticate`, the answer is the token as a JSON string)
//!   and sends that bearer, to the configured origin only, on every
//!   GraphQL call (`POST /api/v2/graphql`) and attachment download.
//! * **`index_data` arguments.** Exactly one of `jql` and `graphql`, as the
//!   SDK; `include_attachments`; `skip_attachment_extensions`. A `graphql`
//!   document is read-only here: one that names a `mutation` or a
//!   `subscription` is refused (an index run must not change Xray).
//! * **List.** With `jql`, the SDK's `getTests` query, 100 tests a page
//!   (`start`, `limit`) until `total` of them are in hand. With `graphql`,
//!   the one answer, read the way the SDK reads it (`data.getTests.results`,
//!   else the first list or `results` under `data`). Both carry the test's
//!   steps, so a listing holds every test's whole content; there is no test
//!   cap beyond the source's document cap. A well-formed `graphql` answer
//!   with no tests is an empty listing, as a `jql` that matches none is;
//!   only an answer with no list at all is an error. A test is `test/{issueId}.json`.
//! * **Content and version.** The SDK's structure: the manual steps (action,
//!   data, result), else the Gherkin, else the unstructured definition, plus
//!   the Jira description and, with attachments, the sorted attachment ids.
//!   This connector adds the test's Jira key and summary to the document so
//!   it can be found by them (the SDK kept them in metadata only). The
//!   version is the SHA-256 of those bytes (the SDK's 16-hex-digit prefix of
//!   the same hash), since Xray stamps nothing on a test.
//! * **Dependents.** With `include_attachments`, each step attachment
//!   (less `skip_attachment_extensions`) is a document of its own with
//!   `parent_key`, `test/{issueId}/attachment/{id}/{file name}`, versioned by
//!   its id (immutable). Its `downloadLink` is followed only when it is on the
//!   configured origin or on one of Xray's documented Cloud hosts
//!   ([`XRAY_CLOUD_HOSTS`]: global, US, EU, AU; the global host serves links
//!   for a regional tenant), never anywhere else (the SDK sent the bearer
//!   wherever the link pointed); redirects go through the shared
//!   [`crate::atlassian::AttachmentDownloader`].
//! * **Auth and limits.** A rejected client id or secret, and a 401 or 403,
//!   are errors naming the credential, never an empty listing; a 429 backs
//!   off within the shared bound.

use std::collections::BTreeSet;
use std::sync::Arc;

use elitea_content_source::{ContentSource, Document, DocumentRef, SourceError, content_version};
use serde_json::{Map, Value, json};
use url::Url;
use zeroize::Zeroizing;

use crate::atlassian::{
    AtlassianConfigError, AtlassianConfigErrorCode, AttachmentDownloader, DetailedListing, Details,
    adf_text, key_segment, optional_flag, safe_identifier, same_origin, text_of,
};
use crate::base_url::BasePath;
use crate::egress::HostAllowlist;
use crate::source::{Backoff, Failure, Listed, SourceHttp, SourceLimits, Timeouts, references};
use crate::testmgmt::{
    MAX_IDENTITY_BYTES, extension_list, extension_skipped, https_base, printable_secret, refuse,
    required_text, scalar_text, text_argument,
};
use crate::transport::header::{ACCEPT, AUTHORIZATION, CONTENT_TYPE, HeaderValue};
use crate::transport::{Method, Request, StatusCode, Transport};

pub const PROVIDER: &str = "Xray Cloud";
/// The SDK's `XrayApiWrapper._default_base_url`.
pub const DEFAULT_BASE_URL: &str = "https://xray.cloud.getxray.app";
/// Every Xray Cloud API host Xray documents (Data Residency): the global
/// endpoint and the US, EU (Frankfurt) and Australia (Sydney) regions. An
/// attachment `downloadLink` may name any of them (the global host serves
/// links for a regional tenant), and the bearer, an Xray-issued token, is
/// sent to no other host.
pub const XRAY_CLOUD_HOSTS: [&str; 4] = [
    "xray.cloud.getxray.app",
    "us.xray.cloud.getxray.app",
    "eu.xray.cloud.getxray.app",
    "au.xray.cloud.getxray.app",
];

fn is_xray_cloud_host(url: &Url) -> bool {
    url.scheme() == "https"
        && url.port().is_none()
        && url.username().is_empty()
        && url.password().is_none()
        && url.host_str().is_some_and(|host| {
            XRAY_CLOUD_HOSTS
                .iter()
                .any(|known| host.eq_ignore_ascii_case(known))
        })
}
/// Xray Cloud's `getTests` ceiling.
const PAGE_SIZE: usize = 100;

/// The SDK's `_get_tests_query`.
const GET_TESTS: &str = r#"query GetTests($jql: String!, $limit:Int!, $start: Int)
{
    getTests(jql: $jql, limit: $limit, start: $start) {
        total
        start
        limit
        results {
            issueId
            jira(fields: ["key", "summary", "description", "created", "updated", "assignee.displayName", "reporter.displayName"])
            projectId
            testType {
                name
                kind
            }
            steps {
                id
                data
                action
                result
                attachments {
                    id
                    filename
                    downloadLink
                }
            }
            preconditions(limit: $limit) {
                total
                start
                limit
                results {
                    issueId
                    jira(fields: ["key"])
                    projectId
                }
            }
            unstructured
            gherkin
        }
    }
}"#;

/// Invocation-scoped Xray Cloud authority for one index run.
pub struct XrayConnectorConfig {
    base: Url,
    client_id: Box<str>,
    client_secret: Zeroizing<String>,
}

impl XrayConnectorConfig {
    /// Read the toolkit `settings`.
    ///
    /// # Errors
    ///
    /// A missing `xray_configuration`, client id or secret, or a base URL
    /// that is not an HTTPS origin.
    pub fn parse(settings: &Map<String, Value>) -> Result<Self, AtlassianConfigError> {
        let configuration = settings
            .get("xray_configuration")
            .and_then(Value::as_object)
            .ok_or(refuse(
                PROVIDER,
                AtlassianConfigErrorCode::InvalidConfiguration,
            ))?;
        let base = match configuration.get("base_url") {
            None | Some(Value::Null) => DEFAULT_BASE_URL,
            Some(Value::String(value)) if value.trim().is_empty() => DEFAULT_BASE_URL,
            Some(Value::String(value)) => value,
            Some(_) => {
                return Err(refuse(
                    PROVIDER,
                    AtlassianConfigErrorCode::InvalidConfiguration,
                ));
            }
        };
        Ok(Self {
            base: https_base(PROVIDER, base, BasePath::OriginOnly)?,
            client_id: required_text(PROVIDER, configuration, "client_id", MAX_IDENTITY_BYTES)?
                .into(),
            client_secret: Zeroizing::new(
                printable_secret(PROVIDER, configuration, "client_secret")?.to_owned(),
            ),
        })
    }

    #[must_use]
    pub fn base_url(&self) -> &Url {
        &self.base
    }
}

/// The `index_data` arguments the connector honours.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct XrayIndexArguments {
    pub query: XrayQuery,
    pub include_attachments: bool,
    pub skip_attachment_extensions: Vec<String>,
}

/// Which tests to index.
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum XrayQuery {
    Jql(String),
    Graphql(String),
}

impl XrayIndexArguments {
    /// Read the `index_data` arguments.
    ///
    /// # Errors
    ///
    /// Neither or both of `jql` and `graphql`, a `graphql` document that is
    /// not read-only, or an argument of the wrong type.
    pub fn parse(arguments: &Map<String, Value>) -> Result<Self, AtlassianConfigError> {
        let invalid = || refuse(PROVIDER, AtlassianConfigErrorCode::InvalidConfiguration);
        let query = match (
            text_argument(PROVIDER, arguments, "jql")?,
            text_argument(PROVIDER, arguments, "graphql")?,
        ) {
            (Some(jql), None) => XrayQuery::Jql(jql),
            (None, Some(graphql)) => {
                if !read_only_graphql(&graphql) {
                    return Err(invalid());
                }
                XrayQuery::Graphql(graphql)
            }
            _ => return Err(invalid()),
        };
        Ok(Self {
            query,
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

/// Whether a GraphQL document is a read: no `mutation` or `subscription`
/// keyword outside a string or a comment.
fn read_only_graphql(document: &str) -> bool {
    let mut word = String::new();
    let mut in_string = false;
    let mut in_block_string = false;
    let mut in_comment = false;
    let characters: Vec<char> = document.chars().collect();
    let mut at = 0;
    let check = |word: &mut String| {
        let bad = word == "mutation" || word == "subscription";
        word.clear();
        bad
    };
    while at < characters.len() {
        let c = characters[at];
        if in_comment {
            if c == '\n' {
                in_comment = false;
            }
        } else if in_block_string {
            if characters[at..].starts_with(&['"', '"', '"']) {
                in_block_string = false;
                at += 2;
            }
        } else if in_string {
            if c == '\\' {
                at += 1;
            } else if c == '"' {
                in_string = false;
            }
        } else if c == '#' {
            if check(&mut word) {
                return false;
            }
            in_comment = true;
        } else if c == '"' {
            if check(&mut word) {
                return false;
            }
            if characters[at..].starts_with(&['"', '"', '"']) {
                in_block_string = true;
                at += 2;
            } else {
                in_string = true;
            }
        } else if c.is_ascii_alphanumeric() || c == '_' {
            word.push(c);
        } else if check(&mut word) {
            return false;
        }
        at += 1;
    }
    !check(&mut word)
}

/// One Xray Cloud instance's tests and their step attachments.
pub struct XraySource {
    config: XrayConnectorConfig,
    arguments: XrayIndexArguments,
    http: SourceHttp,
    allowlist: HostAllowlist,
    token: tokio::sync::Mutex<Option<Zeroizing<String>>>,
    listing: DetailedListing,
}

impl XraySource {
    /// The toolkit `config` indexed with `arguments`, every request through
    /// `allowlist` (fail-closed) and then `transport`.
    ///
    /// # Errors
    ///
    /// The Xray host is not on the allowlist.
    pub fn new(
        config: XrayConnectorConfig,
        arguments: XrayIndexArguments,
        allowlist: HostAllowlist,
        transport: Arc<dyn Transport>,
    ) -> Result<Self, SourceError> {
        let http = SourceHttp::new(allowlist.clone(), transport);
        if !http.permits(&config.base) {
            return Err(Failure::Refused.into_source_error(PROVIDER, None));
        }
        Ok(Self {
            config,
            arguments,
            http,
            allowlist,
            token: tokio::sync::Mutex::new(None),
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

    fn url(&self, segments: &[&str]) -> Result<Url, Failure> {
        crate::source::web_url(&self.config.base, segments.iter().copied()).ok_or(Failure::Client(
            "the Xray base URL cannot carry a path".to_owned(),
        ))
    }

    fn post(url: Url, body: &Value, bearer: Option<&str>) -> Result<Request, Failure> {
        let mut request = Request::new(Method::POST, url);
        let headers = request.headers_mut();
        headers.insert(ACCEPT, HeaderValue::from_static("application/json"));
        headers.insert(CONTENT_TYPE, HeaderValue::from_static("application/json"));
        if let Some(token) = bearer {
            headers.insert(AUTHORIZATION, bearer_header(token)?);
        }
        *request.body_mut() = Some(
            serde_json::to_vec(body)
                .map_err(|_| Failure::Client("the request cannot be encoded".to_owned()))?
                .into(),
        );
        Ok(request)
    }

    fn get_authorized(url: Url, token: &str) -> Result<Request, Failure> {
        let mut request = Request::new(Method::GET, url);
        request
            .headers_mut()
            .insert(AUTHORIZATION, bearer_header(token)?);
        Ok(request)
    }

    /// The bearer for this run, authenticated once.
    async fn bearer(&self) -> Result<Zeroizing<String>, Failure> {
        let mut held = self.token.lock().await;
        if let Some(token) = held.as_ref() {
            return Ok(token.clone());
        }
        let request = Self::post(
            self.url(&["api", "v1", "authenticate"])?,
            &json!({"client_id": &*self.config.client_id, "client_secret": self.config.client_secret.as_str()}),
            None,
        )?;
        let rejected = |status: StatusCode| {
            Failure::Client(format!(
                "{PROVIDER} refused the client id or secret (HTTP {}); check the toolkit's API key",
                status.as_u16()
            ))
        };
        let answer = match self.http.json(request).await {
            Ok((_, answer)) => answer,
            Err(Failure::Status(
                status @ (StatusCode::BAD_REQUEST
                | StatusCode::UNAUTHORIZED
                | StatusCode::FORBIDDEN),
            )) => return Err(rejected(status)),
            Err(other) => return Err(other),
        };
        let token =
            answer
                .as_str()
                .filter(|token| !token.is_empty())
                .ok_or(Failure::InvalidResponse(
                    "the authentication answer is not a token",
                ))?;
        let token = Zeroizing::new(token.to_owned());
        *held = Some(token.clone());
        Ok(token)
    }

    async fn graphql(&self, query: &str, variables: &Value) -> Result<Value, Failure> {
        let token = self.bearer().await?;
        let request = Self::post(
            self.url(&["api", "v2", "graphql"])?,
            &json!({"query": query, "variables": variables}),
            Some(&token),
        )?;
        let answer = crate::atlassian::authorized_json(&self.http, request, PROVIDER).await?;
        if let Some(errors) = answer
            .get("errors")
            .and_then(Value::as_array)
            .filter(|errors| !errors.is_empty())
        {
            let message = errors
                .first()
                .and_then(|error| error.get("message"))
                .and_then(Value::as_str)
                .unwrap_or("unspecified");
            let message: String = message.chars().take(200).collect();
            return Err(Failure::Client(format!(
                "{PROVIDER} rejected the query: {message}; check the JQL or GraphQL"
            )));
        }
        Ok(answer)
    }

    /// The tests the arguments select.
    async fn tests(&self) -> Result<Vec<Value>, Failure> {
        match &self.arguments.query {
            XrayQuery::Jql(jql) => {
                let mut tests: Vec<Value> = Vec::new();
                let mut pages_seen = 0_usize;
                loop {
                    pages_seen += 1;
                    self.http.check_pages(pages_seen)?;
                    let mut answer = self
                        .graphql(
                            GET_TESTS,
                            &json!({"jql": jql, "start": tests.len(), "limit": PAGE_SIZE}),
                        )
                        .await?;
                    let mut page = answer
                        .pointer_mut("/data/getTests")
                        .map(Value::take)
                        .filter(Value::is_object)
                        .ok_or(Failure::InvalidResponse("the answer holds no `getTests`"))?;
                    let total = page.get("total").and_then(Value::as_u64);
                    let results = crate::testmgmt::take_list(
                        &mut page,
                        "results",
                        "`results` is not a list",
                    )?;
                    let got = results.len();
                    tests.extend(results);
                    self.http.check_count(tests.len())?;
                    let reached = total.is_none_or(|total| tests.len() as u64 >= total);
                    if got == 0 && !reached {
                        // The reported `total` is not reached but the page
                        // is empty: never a silently short listing.
                        return Err(Failure::InvalidResponse(
                            "an empty page while the reported `total` is not reached",
                        ));
                    }
                    if got == 0 || reached {
                        return Ok(tests);
                    }
                }
            }
            XrayQuery::Graphql(document) => {
                let mut answer = self.graphql(document, &json!({})).await?;
                let Some(Value::Object(data)) = answer.get("data") else {
                    return Err(Failure::Client(format!(
                        "{PROVIDER}: the GraphQL answer holds no data"
                    )));
                };
                // `data.getTests.results`, else the first list or `results`
                // under `data`. A list, even an empty one, is a well-formed
                // answer; none at all is not.
                let escape = |name: &str| name.replace('~', "~0").replace('/', "~1");
                let pointer = if data
                    .get("getTests")
                    .and_then(|tests| tests.get("results"))
                    .is_some_and(Value::is_array)
                {
                    Some("/data/getTests/results".to_owned())
                } else {
                    data.iter().find_map(|(name, value)| match value {
                        Value::Array(_) => Some(format!("/data/{}", escape(name))),
                        Value::Object(object)
                            if object.get("results").is_some_and(Value::is_array) =>
                        {
                            Some(format!("/data/{}/results", escape(name)))
                        }
                        _ => None,
                    })
                };
                let Some(Value::Array(tests)) = pointer
                    .and_then(|pointer| answer.pointer_mut(&pointer))
                    .map(Value::take)
                else {
                    return Err(Failure::Client(format!(
                        "{PROVIDER}: the GraphQL answer holds no list of tests"
                    )));
                };
                self.http.check_count(tests.len())?;
                Ok(tests)
            }
        }
    }

    #[allow(
        clippy::too_many_lines,
        reason = "one pass: a test, its content, its metadata and its attachments"
    )]
    async fn listing(&self) -> Result<(Vec<Listed>, Details), Failure> {
        let mut listed = Vec::new();
        let mut details = Details::new();
        let mut seen: BTreeSet<String> = BTreeSet::new();
        for test in self.tests().await? {
            let issue_id = test
                .get("issueId")
                .and_then(scalar_text)
                .ok_or(Failure::InvalidResponse("a test has no `issueId`"))?;
            if !safe_identifier(&issue_id) {
                return Err(Failure::InvalidResponse(
                    "a test issueId is not an identifier",
                ));
            }
            if !seen.insert(issue_id.clone()) {
                continue;
            }
            let mut attachments: Vec<(String, String, String, String)> = Vec::new();
            if self.arguments.include_attachments {
                for step in test
                    .get("steps")
                    .and_then(Value::as_array)
                    .into_iter()
                    .flatten()
                {
                    for attachment in step
                        .get("attachments")
                        .and_then(Value::as_array)
                        .into_iter()
                        .flatten()
                    {
                        let (Some(id), Some(filename)) = (
                            attachment.get("id").and_then(scalar_text),
                            text_of(attachment, "/filename"),
                        ) else {
                            continue;
                        };
                        if !safe_identifier(&id) {
                            return Err(Failure::InvalidResponse(
                                "an attachment id is not an identifier",
                            ));
                        }
                        attachments.push((
                            id,
                            filename.to_owned(),
                            text_of(attachment, "/downloadLink")
                                .unwrap_or_default()
                                .to_owned(),
                            step.get("id").and_then(scalar_text).unwrap_or_default(),
                        ));
                    }
                }
            }
            let bytes = Self::test_content(&test, &attachments)?;
            let version = content_version(&bytes);
            let key = format!("test/{issue_id}.json");
            listed.push(Listed::new(
                key.clone(),
                version.clone(),
                bytes.len() as u64,
                issue_id.clone(),
            ));
            self.http.check_count(listed.len())?;
            details.insert(
                key.clone(),
                Self::test_metadata(&test, &issue_id, &version, &bytes),
            );
            for (id, filename, link, step_id) in attachments {
                if extension_skipped(&self.arguments.skip_attachment_extensions, &filename) {
                    continue;
                }
                let attachment_key =
                    format!("test/{issue_id}/attachment/{id}/{}", key_segment(&filename));
                if details.contains_key(&attachment_key) {
                    continue;
                }
                listed.push(Listed::new(
                    attachment_key.clone(),
                    id.clone(),
                    0,
                    id.clone(),
                ));
                self.http.check_count(listed.len())?;
                let mut meta = Map::new();
                meta.insert("type".into(), Value::String("attachment".into()));
                meta.insert("id".into(), Value::String(format!("attach_{id}")));
                meta.insert("issueId".into(), Value::String(issue_id.clone()));
                meta.insert("filename".into(), Value::String(filename));
                meta.insert(
                    "entity_type".into(),
                    Value::String("test_case_attachment".into()),
                );
                meta.insert("step_id".into(), Value::String(step_id));
                meta.insert("parent_key".into(), Value::String(key.clone()));
                meta.insert("parent_id".into(), Value::String(issue_id.clone()));
                meta.insert(
                    "source".into(),
                    Value::String(format!("xray_test_{issue_id}")),
                );
                meta.insert("doctype".into(), Value::String("xray_test".into()));
                if let Some(test_key) = test.pointer("/jira/key").and_then(Value::as_str) {
                    meta.insert("issue_key".into(), Value::String(test_key.to_owned()));
                }
                if let Some(project) = test.get("projectId").and_then(scalar_text) {
                    meta.insert("projectId".into(), Value::String(project));
                }
                meta.insert("download_link".into(), Value::String(link));
                details.insert(attachment_key, meta);
            }
        }
        Ok((listed, details))
    }

    /// The SDK's `content_structure` for one test, as JSON text.
    fn test_content(
        test: &Value,
        attachments: &[(String, String, String, String)],
    ) -> Result<Vec<u8>, Failure> {
        let kind = test
            .pointer("/testType/name")
            .and_then(Value::as_str)
            .unwrap_or_default()
            .to_lowercase();
        let mut structure = Map::new();
        let steps = test
            .get("steps")
            .and_then(Value::as_array)
            .filter(|steps| !steps.is_empty());
        if let (true, Some(steps)) = (kind == "manual", steps) {
            let steps: Vec<Value> = steps
                .iter()
                .filter_map(|step| {
                    let mut out = Map::new();
                    for field in ["action", "data", "result"] {
                        if let Some(text) = step
                            .get(field)
                            .and_then(Value::as_str)
                            .filter(|text| !text.is_empty())
                        {
                            out.insert(field.into(), Value::String(text.to_owned()));
                        }
                    }
                    (!out.is_empty()).then_some(Value::Object(out))
                })
                .collect();
            structure.insert("steps".into(), Value::Array(steps));
        } else if let (true, Some(gherkin)) = (
            kind == "cucumber",
            test.get("gherkin")
                .and_then(Value::as_str)
                .filter(|text| !text.is_empty()),
        ) {
            structure.insert("gherkin".into(), Value::String(gherkin.to_owned()));
        } else if let Some(unstructured) = test
            .get("unstructured")
            .and_then(Value::as_str)
            .filter(|text| !text.is_empty())
        {
            structure.insert(
                "unstructured".into(),
                Value::String(unstructured.to_owned()),
            );
        }
        if !structure.is_empty() && !attachments.is_empty() {
            let mut ids: Vec<&str> = attachments.iter().map(|a| a.0.as_str()).collect();
            ids.sort_unstable();
            structure.insert("attachment_ids".into(), json!(ids));
        }
        if let Some(jira) = test.get("jira").filter(|jira| jira.is_object()) {
            if let Some(key) = jira.get("key").and_then(Value::as_str) {
                structure.insert("key".into(), Value::String(key.to_owned()));
            }
            if let Some(summary) = jira.get("summary").and_then(Value::as_str) {
                structure.insert("summary".into(), Value::String(summary.to_owned()));
            }
            if let Some(description) = jira.get("description").filter(|d| !d.is_null()) {
                let rendered = match description {
                    Value::String(plain) => plain.clone(),
                    other => adf_text(other),
                };
                if !rendered.is_empty() {
                    structure.insert("description".into(), Value::String(rendered));
                }
            }
        }
        serde_json::to_vec_pretty(&Value::Object(structure))
            .map_err(|_| Failure::InvalidResponse("a test cannot be encoded"))
    }

    fn test_metadata(
        test: &Value,
        issue_id: &str,
        version: &str,
        bytes: &[u8],
    ) -> Map<String, Value> {
        let mut meta = Map::new();
        meta.insert("doctype".into(), Value::String("xray_test".into()));
        meta.insert("type".into(), Value::String("xray_test".into()));
        meta.insert("issueId".into(), Value::String(issue_id.to_owned()));
        meta.insert("id".into(), Value::String(issue_id.to_owned()));
        meta.insert("updated_on".into(), Value::String(version.to_owned()));
        if let Some(jira) = test.get("jira").filter(|jira| jira.is_object()) {
            for (name, field) in [
                ("key", "key"),
                ("summary", "summary"),
                ("created_on", "created"),
            ] {
                if let Some(text) = jira.get(field).and_then(Value::as_str) {
                    meta.insert(name.into(), Value::String(text.to_owned()));
                }
            }
            for field in ["assignee", "reporter"] {
                let person = match jira.get(field) {
                    Some(Value::String(name)) => Some(name.clone()),
                    Some(Value::Object(object)) => object
                        .get("displayName")
                        .and_then(Value::as_str)
                        .map(ToOwned::to_owned),
                    _ => None,
                };
                if let Some(person) = person.filter(|person| !person.is_empty()) {
                    meta.insert(field.into(), Value::String(person));
                }
            }
        }
        if let Some(project) = test.get("projectId").and_then(scalar_text) {
            meta.insert("projectId".into(), Value::String(project));
        }
        if let Some(name) = test.pointer("/testType/name").and_then(Value::as_str) {
            meta.insert("testType".into(), Value::String(name.to_owned()));
        }
        if let Some(kind) = test.pointer("/testType/kind").and_then(Value::as_str) {
            meta.insert("testKind".into(), Value::String(kind.to_owned()));
        }
        meta.insert(
            "content".into(),
            Value::String(String::from_utf8_lossy(bytes).into_owned()),
        );
        meta
    }
}

fn bearer_header(token: &str) -> Result<HeaderValue, Failure> {
    let mut value = HeaderValue::from_str(&format!("Bearer {token}"))
        .map_err(|_| Failure::Client("the Xray token is not a header value".to_owned()))?;
    value.set_sensitive(true);
    Ok(value)
}

impl ContentSource for XraySource {
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
            .unwrap_or("xray_test")
            .to_owned();
        let (bytes, title) = if kind == "attachment" {
            let link = metadata
                .remove("download_link")
                .and_then(|link| link.as_str().map(ToOwned::to_owned))
                .unwrap_or_default();
            let url = Url::parse(&link).map_err(|_| {
                failed(Failure::InvalidResponse(
                    "an attachment has no usable download link",
                ))
            })?;
            if !same_origin(&self.config.base, &url) && !is_xray_cloud_host(&url) {
                return Err(failed(Failure::Client(
                    "the attachment's download link is not on the Xray origin, so the download was refused (the credential is sent only there)"
                        .to_owned(),
                )));
            }
            let token = self.bearer().await.map_err(failed)?;
            let filename = metadata
                .get("filename")
                .and_then(Value::as_str)
                .unwrap_or("attachment")
                .to_owned();
            let bytes = AttachmentDownloader::new(&url, &self.allowlist)
                .download(
                    &self.http,
                    url,
                    |url| Self::get_authorized(url, &token),
                    &filename,
                    None,
                )
                .await
                .map_err(failed)?;
            (bytes, filename)
        } else {
            let content = match metadata.remove("content") {
                Some(Value::String(content)) => content,
                _ => String::new(),
            };
            let title = metadata
                .get("summary")
                .and_then(Value::as_str)
                .or_else(|| metadata.get("key").and_then(Value::as_str))
                .map_or_else(|| format!("Test {}", listed.handle), ToOwned::to_owned);
            (content.into_bytes(), title)
        };
        let mut reference = listed.reference.clone();
        reference.size = bytes.len() as u64;
        Ok(Document {
            title,
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
    use crate::source::fixture::{Reply, Scripted};
    use std::sync::Mutex;

    fn config() -> XrayConnectorConfig {
        XrayConnectorConfig::parse(
            json!({"xray_configuration": {"client_id": "CID", "client_secret": "s3cr3t-secret"}})
                .as_object()
                .unwrap(),
        )
        .expect("config")
    }

    fn arguments(value: &Value) -> XrayIndexArguments {
        XrayIndexArguments::parse(value.as_object().unwrap()).expect("arguments")
    }

    fn allow() -> HostAllowlist {
        HostAllowlist::parse(Some("xray.cloud.getxray.app"))
    }

    fn manual(issue: &str, key: &str, action: &str, attachments: &[(&str, &str)]) -> Value {
        json!({
            "issueId": issue, "projectId": "10000",
            "jira": {"key": key, "summary": format!("Summary {key}"), "description": "Desc",
                     "created": "2026-09-01T00:00:00.000Z", "updated": "2026-09-02T00:00:00.000Z",
                     "assignee": {"displayName": "Ann"}, "reporter": {"displayName": "Bob"}},
            "testType": {"name": "Manual", "kind": "Steps"},
            "steps": [{"id": format!("s-{issue}"), "action": action, "data": "d", "result": "r",
                       "attachments": attachments.iter().map(|(id, name)| json!({
                           "id": id, "filename": name,
                           "downloadLink": format!("https://xray.cloud.getxray.app/api/v2/attachments/{id}")
                       })).collect::<Vec<_>>()}],
            "preconditions": {"total": 0, "start": 0, "limit": 100, "results": []},
            "unstructured": null, "gherkin": null,
        })
    }

    struct Instance {
        tests: Vec<Value>,
    }

    fn transport(instance: &Arc<Mutex<Instance>>) -> Arc<Scripted> {
        let instance = Arc::clone(instance);
        Scripted::new(move |request, _| {
            let path = request.url().path().to_owned();
            assert_eq!(request.url().host_str(), Some("xray.cloud.getxray.app"));
            if path == "/api/v1/authenticate" {
                let body: Value =
                    serde_json::from_slice(request.body().unwrap().as_bytes().unwrap()).unwrap();
                assert_eq!(body["client_id"], "CID");
                assert!(request.headers().get("authorization").is_none());
                return Reply::json(&json!("jwt-token"));
            }
            assert_eq!(
                request
                    .headers()
                    .get("authorization")
                    .unwrap()
                    .to_str()
                    .unwrap(),
                "Bearer jwt-token"
            );
            if path == "/api/v2/graphql" {
                let body: Value =
                    serde_json::from_slice(request.body().unwrap().as_bytes().unwrap()).unwrap();
                let start =
                    usize::try_from(body["variables"]["start"].as_u64().unwrap_or(0)).unwrap();
                let tests = instance.lock().unwrap().tests.clone();
                let page: Vec<Value> = tests.iter().skip(start).take(1).cloned().collect();
                Reply::json(&json!({"data": {"getTests": {
                    "total": tests.len(), "start": start, "limit": 100, "results": page}}}))
            } else if path.starts_with("/api/v2/attachments/") {
                Reply::bytes(b"PNGDATA").header("content-type", "image/png")
            } else {
                panic!("unexpected request {path}")
            }
        })
    }

    #[test]
    fn the_configuration_and_arguments_are_strict() {
        assert_eq!(
            config().base_url().as_str(),
            "https://xray.cloud.getxray.app/"
        );
        for bad in [
            json!({}),
            json!({"xray_configuration": {"client_id": "a"}}),
            json!({"xray_configuration": {"client_id": "a", "client_secret": "b", "base_url": "http://x.example"}}),
            json!({"xray_configuration": {"client_id": "a", "client_secret": "b", "base_url": "https://x.example/path"}}),
        ] {
            assert!(
                XrayConnectorConfig::parse(bad.as_object().unwrap()).is_err(),
                "{bad}"
            );
        }
        assert!(XrayIndexArguments::parse(&Map::new()).is_err(), "neither");
        assert!(
            XrayIndexArguments::parse(json!({"jql": "a", "graphql": "{ x }"}).as_object().unwrap())
                .is_err(),
            "both"
        );
    }

    #[test]
    fn graphql_must_be_a_read() {
        for fine in [
            "query { getTests(jql: \"project = X\") { results { issueId } } }",
            "{ getTests(jql: \"summary ~ \\\"mutation\\\"\") { total } }",
            "# a mutation would go here\nquery Q { x }",
            "query { mutationCount }",
        ] {
            assert!(read_only_graphql(fine), "{fine}");
        }
        for bad in [
            "mutation { createTest(testType: {name: \"Manual\"}) { warnings } }",
            "query { x }\nmutation M { y }",
            "subscription { z }",
            "mutation{y}",
        ] {
            assert!(!read_only_graphql(bad), "{bad}");
        }
        let error =
            XrayIndexArguments::parse(json!({"graphql": "mutation { x }"}).as_object().unwrap());
        assert!(error.is_err());
    }

    #[tokio::test]
    async fn the_jql_is_paged_and_a_test_is_the_sdk_structure() {
        let instance = Arc::new(Mutex::new(Instance {
            tests: vec![
                manual("1001", "CALC-1", "Press +", &[]),
                manual("1002", "CALC-2", "Press -", &[]),
            ],
        }));
        let source = XraySource::new(
            config(),
            arguments(&json!({"jql": "project = CALC"})),
            allow(),
            transport(&instance),
        )
        .unwrap();
        let listed = source.list().await.expect("list");
        assert_eq!(
            listed.iter().map(|r| r.key.as_str()).collect::<Vec<_>>(),
            ["test/1001.json", "test/1002.json"]
        );
        let document = source.fetch("test/1002.json").await.expect("fetch");
        let body: Value = serde_json::from_slice(&document.bytes).unwrap();
        assert_eq!(body["steps"][0]["action"], "Press -");
        assert_eq!(body["description"], "Desc");
        assert_eq!(body["key"], "CALC-2");
        assert_eq!(document.metadata["doctype"], "xray_test");
        assert_eq!(document.metadata["assignee"], "Ann");
        assert_eq!(document.metadata["testType"], "Manual");
        assert!(!document.metadata.contains_key("content"));
        assert_eq!(document.title, "Summary CALC-2");
    }

    #[tokio::test]
    async fn attachments_are_dependents_downloaded_from_the_origin_only() {
        let instance = Arc::new(Mutex::new(Instance {
            tests: vec![manual(
                "1001",
                "CALC-1",
                "Press +",
                &[("a1", "shot.png"), ("a2", "run.exe")],
            )],
        }));
        let wire = transport(&instance);
        let source = XraySource::new(
            config(),
            arguments(&json!({"jql": "x", "include_attachments": true, "skip_attachment_extensions": [".EXE"]})),
            allow(),
            wire.clone(),
        )
        .unwrap();
        let listed = source.list().await.expect("list");
        let keys: Vec<&str> = listed.iter().map(|r| r.key.as_str()).collect();
        assert_eq!(keys, ["test/1001.json", "test/1001/attachment/a1/shot.png"]);
        assert_eq!(listed[1].version, "a1", "an attachment has its own version");
        let test = source.fetch("test/1001.json").await.unwrap();
        let body: Value = serde_json::from_slice(&test.bytes).unwrap();
        assert_eq!(body["attachment_ids"], json!(["a1", "a2"]));
        let attachment = source
            .fetch("test/1001/attachment/a1/shot.png")
            .await
            .unwrap();
        assert_eq!(attachment.bytes, b"PNGDATA");
        assert_eq!(attachment.metadata["parent_key"], "test/1001.json");
        assert_eq!(attachment.metadata["step_id"], "s-1001");
        assert!(!attachment.metadata.contains_key("download_link"));
        assert_eq!(
            wire.seen()
                .iter()
                .filter(|r| r.ends_with("/authenticate"))
                .count(),
            1
        );

        // A link that points anywhere else is never requested with the bearer.
        let mut hostile = manual("2001", "CALC-9", "x", &[("z1", "z.png")]);
        hostile["steps"][0]["attachments"][0]["downloadLink"] = json!("https://evil.example/steal");
        let instance = Arc::new(Mutex::new(Instance {
            tests: vec![hostile],
        }));
        let wire = transport(&instance);
        let source = XraySource::new(
            config(),
            arguments(&json!({"jql": "x", "include_attachments": true})),
            HostAllowlist::parse(Some("xray.cloud.getxray.app evil.example")),
            wire.clone(),
        )
        .unwrap();
        source.list().await.unwrap();
        let error = source
            .fetch("test/2001/attachment/z1/z.png")
            .await
            .expect_err("refused")
            .to_string();
        assert!(error.contains("not on the Xray origin"), "{error}");
        assert!(
            wire.requests()
                .iter()
                .all(|r| r.url().host_str() != Some("evil.example"))
        );
    }

    #[tokio::test]
    async fn only_a_changed_test_gets_a_new_version() {
        let instance = Arc::new(Mutex::new(Instance {
            tests: vec![
                manual("1", "A-1", "one", &[]),
                manual("2", "A-2", "two", &[]),
            ],
        }));
        let list = |instance: &Arc<Mutex<Instance>>| {
            let source = XraySource::new(
                config(),
                arguments(&json!({"jql": "x"})),
                allow(),
                transport(instance),
            )
            .unwrap();
            async move { source.list().await.unwrap() }
        };
        let before = list(&instance).await;
        assert_eq!(list(&instance).await, before);
        instance.lock().unwrap().tests[1] = manual("2", "A-2", "two, edited", &[]);
        let after = list(&instance).await;
        assert_eq!(after[0], before[0]);
        assert_ne!(after[1].version, before[1].version);
    }

    #[tokio::test]
    async fn an_empty_jql_page_before_the_reported_total_is_an_error() {
        let source = XraySource::new(
            config(),
            arguments(&json!({"jql": "x"})),
            allow(),
            Scripted::new(|request, _| {
                if request.url().path() == "/api/v1/authenticate" {
                    return Reply::json(&json!("jwt-token"));
                }
                let body: Value =
                    serde_json::from_slice(request.body().unwrap().as_bytes().unwrap()).unwrap();
                let results = if body["variables"]["start"] == 0 {
                    vec![manual("1", "A-1", "one", &[])]
                } else {
                    vec![]
                };
                Reply::json(&json!({"data": {"getTests": {
                    "total": 3, "start": 0, "limit": 100, "results": results}}}))
            }),
        )
        .unwrap();
        let text = source.list().await.expect_err("short").to_string();
        assert!(text.contains("total"), "{text}");
    }

    #[tokio::test]
    async fn graphql_errors_and_rejected_credentials_are_named() {
        // A rejected client id/secret on authenticate.
        for status in [
            StatusCode::BAD_REQUEST,
            StatusCode::UNAUTHORIZED,
            StatusCode::FORBIDDEN,
        ] {
            let source = XraySource::new(
                config(),
                arguments(&json!({"jql": "x"})),
                allow(),
                Scripted::new(move |_, _| Reply::status(status)),
            )
            .unwrap();
            let text = source.list().await.expect_err("refused").to_string();
            assert!(text.contains("refused the client id or secret"), "{text}");
            assert!(!text.contains("s3cr3t"), "{text}");
        }
        // A token that GraphQL then refuses.
        let source = XraySource::new(
            config(),
            arguments(&json!({"jql": "x"})),
            allow(),
            Scripted::new(|request, _| {
                if request.url().path().ends_with("/authenticate") {
                    Reply::json(&json!("jwt"))
                } else {
                    Reply::status(StatusCode::FORBIDDEN)
                }
            }),
        )
        .unwrap();
        assert!(
            source
                .list()
                .await
                .expect_err("403")
                .to_string()
                .contains("refused the credential")
        );
        // An invalid JQL is a 200 with `errors`: not an empty listing.
        let source = XraySource::new(
            config(),
            arguments(&json!({"jql": "garbage ("})),
            allow(),
            Scripted::new(|request, _| {
                if request.url().path().ends_with("/authenticate") {
                    Reply::json(&json!("jwt"))
                } else {
                    Reply::json(
                        &json!({"errors": [{"message": "Error in the JQL Query"}], "data": null}),
                    )
                }
            }),
        )
        .unwrap();
        let text = source.list().await.expect_err("errors").to_string();
        assert!(text.contains("Error in the JQL Query"), "{text}");
    }

    #[tokio::test]
    async fn a_graphql_document_is_read_like_the_sdk() {
        let source = XraySource::new(
            config(),
            arguments(
                &json!({"graphql": "query { getTests(jql: \"x\") { results { issueId } } }"}),
            ),
            allow(),
            Scripted::new(|request, _| {
                if request.url().path().ends_with("/authenticate") {
                    Reply::json(&json!("jwt"))
                } else {
                    Reply::json(&json!({"data": {"getTests": {"results": [
                        {"issueId": "7", "testType": {"name": "Generic"}, "unstructured": "do it"}
                    ]}}}))
                }
            }),
        )
        .unwrap();
        assert_eq!(source.list().await.unwrap().len(), 1);
        let document = source.fetch("test/7.json").await.unwrap();
        let body: Value = serde_json::from_slice(&document.bytes).unwrap();
        assert_eq!(body["unstructured"], "do it");
    }

    #[tokio::test]
    async fn a_well_formed_graphql_answer_with_no_tests_is_an_empty_listing() {
        let list = |data: Value| {
            let source = XraySource::new(
                config(),
                arguments(
                    &json!({"graphql": "query { getTests(jql: \"x\") { results { issueId } } }"}),
                ),
                allow(),
                Scripted::new(move |request, _| {
                    if request.url().path().ends_with("/authenticate") {
                        Reply::json(&json!("jwt"))
                    } else {
                        Reply::json(&json!({"data": data.clone()}))
                    }
                }),
            )
            .unwrap();
            async move { source.list().await }
        };
        for empty in [
            json!({"getTests": {"total": 0, "results": []}}),
            json!({"other": []}),
            json!({"other": {"results": []}}),
        ] {
            assert!(list(empty.clone()).await.unwrap().is_empty(), "{empty}");
        }
        for malformed in [
            json!({}),
            json!({"getTests": null}),
            json!({"x": "text"}),
            Value::Null,
        ] {
            assert!(list(malformed.clone()).await.is_err(), "{malformed}");
        }
    }

    #[tokio::test]
    async fn the_eu_region_follows_a_global_download_link_but_no_other_host() {
        let test_with_link = |link: &str| {
            let mut test = manual("1", "A-1", "go", &[("a1", "shot.png")]);
            test["steps"][0]["attachments"][0]["downloadLink"] = json!(link);
            test
        };
        let run = |link: &'static str| {
            let wire = Scripted::new(move |request, _| {
                let path = request.url().path().to_owned();
                if path == "/api/v1/authenticate" {
                    Reply::json(&json!("jwt-token"))
                } else if path == "/api/v2/graphql" {
                    assert_eq!(request.url().host_str(), Some("eu.xray.cloud.getxray.app"));
                    Reply::json(&json!({"data": {"getTests": {"total": 1, "start": 0,
                        "limit": 100, "results": [test_with_link(link)]}}}))
                } else {
                    assert_eq!(
                        request
                            .headers()
                            .get("authorization")
                            .unwrap()
                            .to_str()
                            .unwrap(),
                        "Bearer jwt-token"
                    );
                    Reply::bytes(b"PNGDATA").header("content-type", "image/png")
                }
            });
            let source = XraySource::new(
                XrayConnectorConfig::parse(
                    json!({"xray_configuration": {"base_url": "https://eu.xray.cloud.getxray.app",
                           "client_id": "CID", "client_secret": "s3cr3t-secret"}})
                    .as_object()
                    .unwrap(),
                )
                .unwrap(),
                arguments(&json!({"jql": "x", "include_attachments": true})),
                HostAllowlist::parse(Some(
                    "eu.xray.cloud.getxray.app xray.cloud.getxray.app us.xray.cloud.getxray.app evil.example",
                )),
                wire.clone(),
            )
            .unwrap();
            async move {
                source.list().await.unwrap();
                let fetched = source.fetch("test/1/attachment/a1/shot.png").await;
                (fetched, wire)
            }
        };
        let (fetched, wire) = run("https://xray.cloud.getxray.app/api/v2/attachments/a1").await;
        assert_eq!(fetched.unwrap().bytes, b"PNGDATA");
        assert!(
            wire.requests()
                .iter()
                .any(|r| r.url().host_str() == Some("xray.cloud.getxray.app"))
        );
        // Another Xray region's host is an Xray host too.
        let (fetched, _) = run("https://us.xray.cloud.getxray.app/api/v2/attachments/a1").await;
        assert_eq!(fetched.unwrap().bytes, b"PNGDATA");
        // A look-alike or a plain-HTTP link is never followed.
        for hostile in [
            "https://xray.cloud.getxray.app.evil.example/api/v2/attachments/a1",
            "https://evil.example/api/v2/attachments/a1",
            "http://xray.cloud.getxray.app/api/v2/attachments/a1",
        ] {
            let (fetched, wire) = run(hostile).await;
            assert!(fetched.is_err(), "{hostile}");
            assert!(
                wire.requests().iter().all(|r| {
                    !r.url()
                        .host_str()
                        .is_some_and(|host| host.ends_with("evil.example"))
                        && r.url().scheme() == "https"
                }),
                "{hostile}"
            );
        }
    }

    #[tokio::test]
    async fn a_rate_limit_backs_off_and_a_foreign_host_is_refused() {
        let source = XraySource::new(
            config(),
            arguments(&json!({"jql": "x"})),
            allow(),
            Scripted::new(|_, _| Reply::rate_limited(Some("1"))),
        )
        .unwrap()
        .with_backoff(Backoff {
            max_attempts: 2,
            initial: std::time::Duration::from_millis(1),
            max_wait: std::time::Duration::from_secs(5),
        });
        assert!(
            source
                .list()
                .await
                .expect_err("limited")
                .to_string()
                .contains("rate limited")
        );
        assert!(
            XraySource::new(
                config(),
                arguments(&json!({"jql": "x"})),
                HostAllowlist::parse(Some("github.com")),
                Scripted::new(|_, _| panic!("no request may be sent")),
            )
            .is_err()
        );
    }
}

//! qTest Manager as a `ContentSource` (ADR-0030 decision 3, stage I3).
//!
//! * **Configuration.** `qtest_configuration.{base_url,qtest_api_token}` and
//!   the project, `qtest_project_id` (or the legacy `project_id`), in the
//!   toolkit settings, as the SDK and the agent-runtime family read them. The
//!   token is sent as a bearer to the configured origin only.
//! * **`index_data` arguments.** `indexing_mode`: `full` (the default), every
//!   test case of the project; `module`, the test cases directly under the
//!   module named `module_name` (as `"MD-7 Master Test Suite"`, the module's
//!   `pid` and name); or `dql`, the test cases a DQL query matches.
//!   `extract_images` and `image_prompt` are accepted and ignored: reading an
//!   image needs a model (I4), so embedded images are removed from the text.
//! * **List.** `full` and `module` page `GET /api/v3/projects/{id}/test-cases`
//!   (`page`, `size` 100, `expandSteps=true`) until a page is short, as the
//!   SDK does. `dql` pages `POST /search` (`page`, `pageSize`) until the
//!   answer has no `next` link; the SDK stopped there too, and noted that DQL
//!   through the API can be incomplete for complex queries, which this
//!   connector cannot detect. A test case is `testcase/{id}.md` (the numeric
//!   id, stable across renumbering; the SDK keyed by `pid`), versioned by the
//!   SHA-256 of its `last_modified_date` (else `created_date`) together with
//!   its module's name, so a module rename re-indexes its cases (else the
//!   SHA-256 of its content). There are no dependents: qTest test-case attachments are not
//!   indexed (the SDK did not either).
//! * **Content.** The SDK's markdown: `# Test Case: {pid} - {name}`, the
//!   module, `## Description`, `## Precondition`, `## Properties`, then each
//!   step as `### Step n` with its action and expected result, HTML reduced
//!   to text. The content is kept from the listing (the list answer holds
//!   whole test cases), so a fetch reads nothing again and can never see a
//!   version other than the listed one.
//! * **Auth and limits.** A 401 or 403 is an error naming the credential,
//!   never an empty listing; a 429 backs off within the shared bound.

use std::collections::HashMap;
use std::fmt::Write as _;
use std::sync::Arc;

use elitea_content_source::{ContentSource, Document, DocumentRef, SourceError, content_version};
use serde_json::{Map, Value, json};
use url::Url;

use crate::atlassian::{
    AtlassianConfigError, AtlassianConfigErrorCode, DetailedListing, Details, authorized_json,
    id_of, strip_base64_images, text_of,
};
use crate::base_url::BasePath;
use crate::egress::HostAllowlist;
use crate::source::{
    Backoff, Failure, Listed, SourceHttp, SourceLimits, Timeouts, references, web_url,
};
use crate::testmgmt::{
    MAX_QUERY_BYTES, https_base, printable_secret, refuse, required_text, scalar_text,
    text_argument,
};
use crate::transport::header::{ACCEPT, AUTHORIZATION, CONTENT_TYPE, HeaderValue};
use crate::transport::{Method, Request, Transport};

pub const PROVIDER: &str = "qTest";
/// The SDK's `no_of_items_per_page`.
const PAGE_SIZE: usize = 100;

/// Invocation-scoped qTest authority for one index run.
pub struct QtestConnectorConfig {
    base: Url,
    token: zeroize::Zeroizing<String>,
    project_id: u64,
}

impl QtestConnectorConfig {
    /// Read the toolkit `settings`.
    ///
    /// # Errors
    ///
    /// A missing `qtest_configuration`, token or project, or a base URL that
    /// is not HTTPS.
    pub fn parse(settings: &Map<String, Value>) -> Result<Self, AtlassianConfigError> {
        let invalid = || refuse(PROVIDER, AtlassianConfigErrorCode::InvalidConfiguration);
        let configuration = settings
            .get("qtest_configuration")
            .and_then(Value::as_object)
            .ok_or_else(invalid)?;
        let base = https_base(
            PROVIDER,
            required_text(PROVIDER, configuration, "base_url", 2 * 1_024)?,
            BasePath::Prefix,
        )?;
        let token = printable_secret(PROVIDER, configuration, "qtest_api_token")?.to_owned();
        let project_id = settings
            .get("qtest_project_id")
            .or_else(|| settings.get("project_id"))
            .and_then(|value| match value {
                Value::Number(number) => number.as_u64(),
                Value::String(text) => text.trim().parse::<u64>().ok(),
                _ => None,
            })
            .filter(|project| *project > 0)
            .ok_or_else(invalid)?;
        Ok(Self {
            base,
            token: zeroize::Zeroizing::new(token),
            project_id,
        })
    }

    #[must_use]
    pub fn base_url(&self) -> &Url {
        &self.base
    }
}

/// Which test cases to index.
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum QtestMode {
    Full,
    Module(String),
    Dql(String),
}

/// The `index_data` arguments the connector honours.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct QtestIndexArguments {
    pub mode: QtestMode,
}

impl QtestIndexArguments {
    /// Read the `index_data` arguments.
    ///
    /// # Errors
    ///
    /// An unknown `indexing_mode`, a mode missing its `dql` or
    /// `module_name`, or an argument of the wrong type.
    pub fn parse(arguments: &Map<String, Value>) -> Result<Self, AtlassianConfigError> {
        let invalid = || refuse(PROVIDER, AtlassianConfigErrorCode::InvalidConfiguration);
        let dql = text_argument(PROVIDER, arguments, "dql")?;
        let module = text_argument(PROVIDER, arguments, "module_name")?;
        if dql.as_ref().is_some_and(|dql| dql.len() > MAX_QUERY_BYTES) {
            return Err(refuse(
                PROVIDER,
                AtlassianConfigErrorCode::ResourceExhausted,
            ));
        }
        let mode = match arguments.get("indexing_mode") {
            None | Some(Value::Null) => "full",
            Some(Value::String(mode)) => mode.trim(),
            Some(_) => return Err(invalid()),
        };
        Ok(Self {
            mode: match mode {
                "full" | "" => QtestMode::Full,
                "module" => QtestMode::Module(module.ok_or_else(invalid)?),
                "dql" => QtestMode::Dql(dql.ok_or_else(invalid)?),
                _ => return Err(invalid()),
            },
        })
    }
}

/// One qTest project's test cases.
pub struct QtestSource {
    config: QtestConnectorConfig,
    arguments: QtestIndexArguments,
    http: SourceHttp,
    listing: DetailedListing,
}

impl QtestSource {
    /// The toolkit `config` indexed with `arguments`, every request through
    /// `allowlist` (fail-closed) and then `transport`.
    ///
    /// # Errors
    ///
    /// The qTest host is not on the allowlist.
    pub fn new(
        config: QtestConnectorConfig,
        arguments: QtestIndexArguments,
        allowlist: HostAllowlist,
        transport: Arc<dyn Transport>,
    ) -> Result<Self, SourceError> {
        let http = SourceHttp::new(allowlist, transport);
        if !http.permits(config.base_url()) {
            return Err(Failure::Refused.into_source_error(PROVIDER, None));
        }
        Ok(Self {
            config,
            arguments,
            http,
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

    /// `{base}/api/v3/projects/{id}/{segments...}?{params}`.
    fn api(&self, segments: &[&str], params: &[(&str, String)]) -> Result<Url, Failure> {
        let project = self.config.project_id.to_string();
        let mut all = vec!["api", "v3", "projects", project.as_str()];
        all.extend_from_slice(segments);
        let mut url = web_url(&self.config.base, all).ok_or(Failure::Client(
            "the qTest base URL cannot carry a path".to_owned(),
        ))?;
        if !params.is_empty() {
            let mut query = url.query_pairs_mut();
            for (name, value) in params {
                query.append_pair(name, value);
            }
        }
        Ok(url)
    }

    fn request(&self, method: Method, url: Url) -> Result<Request, Failure> {
        let mut request = Request::new(method, url);
        let mut value = HeaderValue::from_str(&format!("Bearer {}", self.config.token.as_str()))
            .map_err(|_| Failure::Client("the qTest token is not a header value".to_owned()))?;
        value.set_sensitive(true);
        let headers = request.headers_mut();
        headers.insert(ACCEPT, HeaderValue::from_static("application/json"));
        headers.insert(AUTHORIZATION, value);
        Ok(request)
    }

    async fn json(&self, request: Request) -> Result<Value, Failure> {
        authorized_json(&self.http, request, PROVIDER).await
    }

    /// `__get_all_modules_for_project` + `_parse_modules`: every module with
    /// its full name (`"{pid} {name}"`), by id.
    async fn modules(&self) -> Result<Vec<(String, String)>, Failure> {
        let url = self.api(&["modules"], &[("expand", "descendants".to_owned())])?;
        let answer = self.json(self.request(Method::GET, url)?).await?;
        let mut out = Vec::new();
        let mut stack: Vec<&Value> = answer
            .as_array()
            .ok_or(Failure::InvalidResponse("the modules answer is not a list"))?
            .iter()
            .rev()
            .collect();
        while let Some(module) = stack.pop() {
            let id = module.get("id").and_then(scalar_text);
            if let Some(id) = id {
                let pid = text_of(module, "/pid").unwrap_or("None");
                let name = text_of(module, "/name").unwrap_or("None");
                out.push((id, format!("{pid} {name}")));
            }
            if let Some(children) = module.get("children").and_then(Value::as_array) {
                stack.extend(children.iter().rev());
            }
            self.http.check_directories(out.len())?;
        }
        Ok(out)
    }

    /// Test cases page by page: `page` from 1 until a page is short.
    async fn plain_pages(&self, parent: Option<&str>) -> Result<Vec<Value>, Failure> {
        let mut items: Vec<Value> = Vec::new();
        let mut page = 1_usize;
        loop {
            self.http.check_pages(page)?;
            let mut params = vec![
                ("page", page.to_string()),
                ("size", PAGE_SIZE.to_string()),
                ("expandSteps", "true".to_owned()),
            ];
            if let Some(parent) = parent {
                params.push(("parentId", parent.to_owned()));
            }
            let url = self.api(&["test-cases"], &params)?;
            let answer = self.json(self.request(Method::GET, url)?).await?;
            let Value::Array(batch) = answer else {
                return Err(Failure::InvalidResponse("the test cases are not a list"));
            };
            let got = batch.len();
            items.extend(batch);
            self.http.check_count(items.len())?;
            if got < PAGE_SIZE {
                return Ok(items);
            }
            page += 1;
        }
    }

    async fn dql_pages(&self, dql: &str) -> Result<Vec<Value>, Failure> {
        let mut items: Vec<Value> = Vec::new();
        let mut page = 1_usize;
        loop {
            self.http.check_pages(page)?;
            let url = self.api(
                &["search"],
                &[
                    ("appendTestSteps", "true".to_owned()),
                    ("includeExternalProperties", "true".to_owned()),
                    ("pageSize", PAGE_SIZE.to_string()),
                    ("page", page.to_string()),
                ],
            )?;
            let mut request = self.request(Method::POST, url)?;
            request
                .headers_mut()
                .insert(CONTENT_TYPE, HeaderValue::from_static("application/json"));
            *request.body_mut() = Some(
                serde_json::to_vec(
                    &json!({"object_type": "test-cases", "fields": ["*"], "query": dql}),
                )
                .map_err(|_| Failure::Client("the query cannot be encoded".to_owned()))?
                .into(),
            );
            let mut answer = self.json(request).await?;
            let batch =
                crate::testmgmt::take_list(&mut answer, "items", "the search holds no `items`")?;
            let got = batch.len();
            items.extend(batch);
            self.http.check_count(items.len())?;
            let has_next = answer
                .get("links")
                .and_then(Value::as_array)
                .is_some_and(|links| {
                    links
                        .iter()
                        .any(|link| link.get("rel").and_then(Value::as_str) == Some("next"))
                });
            if got == 0 || !has_next {
                // The search reports `total`: stopping short of it is an
                // error, never a silently short listing.
                if answer
                    .get("total")
                    .and_then(Value::as_u64)
                    .is_some_and(|total| (items.len() as u64) < total)
                {
                    return Err(Failure::InvalidResponse(
                        "the search ended before the reported `total` was reached",
                    ));
                }
                return Ok(items);
            }
            page += 1;
        }
    }

    async fn listing(&self) -> Result<(Vec<Listed>, Details), Failure> {
        let modules = self.modules().await?;
        // Built once: a project may have thousands of cases and modules.
        let names: HashMap<&str, &str> = modules
            .iter()
            .map(|(id, name)| (id.as_str(), name.as_str()))
            .collect();
        let module_name = |id: Option<String>| -> String {
            id.and_then(|id| names.get(id.as_str()).map(|name| (*name).to_owned()))
                .unwrap_or_default()
        };
        let items = match &self.arguments.mode {
            QtestMode::Full => self.plain_pages(None).await?,
            QtestMode::Module(wanted) => {
                let id = modules
                    .iter()
                    .find(|(_, name)| name == wanted)
                    .map(|(id, _)| id.clone())
                    .ok_or_else(|| {
                        Failure::Client(format!(
                            "{PROVIDER}: module '{wanted}' not found in project {}",
                            self.config.project_id
                        ))
                    })?;
                self.plain_pages(Some(&id)).await?
            }
            QtestMode::Dql(dql) => self.dql_pages(dql).await?,
        };
        let mut listed = Vec::new();
        let mut details = Details::new();
        for item in items {
            let id =
                id_of(&item, "/id").ok_or(Failure::InvalidResponse("a test case has no id"))?;
            if !crate::atlassian::safe_identifier(&id) {
                return Err(Failure::InvalidResponse(
                    "a test case id is not an identifier",
                ));
            }
            let key = format!("testcase/{id}.md");
            if details.contains_key(&key) {
                continue;
            }
            let parent = item.get("parent_id").and_then(scalar_text);
            let module = module_name(parent.clone());
            let markdown = render_test_case(&item, &module);
            // The stamp moves when the case is edited, not when its module
            // is renamed; the module name is in the rendered text, so it is
            // in the version too.
            let version = ["last_modified_date", "updated_date", "created_date"]
                .iter()
                .find_map(|field| text_of(&item, &format!("/{field}")))
                .map_or_else(
                    || content_version(markdown.as_bytes()),
                    |stamp| content_version(format!("{stamp}\n{module}").as_bytes()),
                );
            listed.push(Listed::new(
                key.clone(),
                version.clone(),
                markdown.len() as u64,
                id.clone(),
            ));
            self.http.check_count(listed.len())?;
            let mut meta = Map::new();
            meta.insert(
                "id".into(),
                item.get("pid")
                    .cloned()
                    .unwrap_or(Value::String(String::new())),
            );
            meta.insert("qtest_id".into(), Value::String(id));
            meta.insert("updated_on".into(), Value::String(version));
            meta.insert(
                "name".into(),
                item.get("name")
                    .cloned()
                    .unwrap_or(Value::String(String::new())),
            );
            meta.insert(
                "parent_id".into(),
                item.get("parent_id").cloned().unwrap_or(Value::Null),
            );
            meta.insert("module_name".into(), Value::String(module));
            meta.insert("project_id".into(), json!(self.config.project_id));
            meta.insert("type".into(), Value::String("test_case".into()));
            for property in item
                .get("properties")
                .and_then(Value::as_array)
                .into_iter()
                .flatten()
            {
                if let Some(name) = text_of(property, "/field_name") {
                    let name = name.to_lowercase().replace(' ', "_");
                    meta.entry(name).or_insert_with(|| property_value(property));
                }
            }
            if let Some(url) = text_of(&item, "/web_url") {
                meta.insert("url".into(), Value::String(url.to_owned()));
            }
            meta.insert("content".into(), Value::String(markdown));
            details.insert(key, meta);
        }
        Ok((listed, details))
    }
}

/// `__format_property_value`.
fn property_value(property: &Value) -> Value {
    let value = property
        .get("field_value")
        .filter(|value| !value.is_null())
        .cloned()
        .unwrap_or(Value::String(String::new()));
    let Some(name) = property
        .get("field_value_name")
        .and_then(Value::as_str)
        .filter(|name| !name.is_empty())
    else {
        return value;
    };
    if let Some(inner) = name
        .strip_prefix('[')
        .and_then(|name| name.strip_suffix(']'))
    {
        return Value::Array(
            inner
                .trim()
                .split(',')
                .map(str::trim)
                .filter(|part| !part.is_empty())
                .map(|part| Value::String(part.to_owned()))
                .collect(),
        );
    }
    Value::String(name.to_owned())
}

/// `_clean_html_content` with images removed: unescape once, drop embedded
/// base64 images, then every tag becomes a space (sklearn's `strip_tags`).
fn clean_html(content: &str) -> String {
    if content.is_empty() {
        return String::new();
    }
    let text = strip_base64_images(&crate::ado::text::decode_entities(content));
    let mut out = String::with_capacity(text.len());
    let mut rest = text.as_str();
    while let Some(open) = rest.find('<') {
        out.push_str(&rest[..open]);
        let after = &rest[open + 1..];
        match after.find('>') {
            Some(close) if close > 0 => {
                out.push(' ');
                rest = &after[close + 1..];
            }
            _ => {
                out.push('<');
                rest = after;
            }
        }
    }
    out.push_str(rest);
    out
}

/// `_format_test_case_as_markdown`.
fn render_test_case(item: &Value, module: &str) -> String {
    let mut lines: Vec<String> = Vec::new();
    let pid = text_of(item, "/pid").unwrap_or("Unknown");
    let name = text_of(item, "/name").unwrap_or("Untitled");
    lines.push(format!("# Test Case: {pid} - {name}"));
    lines.push(String::new());
    if !module.is_empty() {
        lines.push("## Module".to_owned());
        lines.push(module.to_owned());
        lines.push(String::new());
    }
    for (heading, field) in [
        ("Description", "description"),
        ("Precondition", "precondition"),
    ] {
        if let Some(text) = item
            .get(field)
            .and_then(Value::as_str)
            .filter(|t| !t.is_empty())
        {
            lines.push(format!("## {heading}"));
            lines.push(clean_html(text));
            lines.push(String::new());
        }
    }
    let properties = item
        .get("properties")
        .and_then(Value::as_array)
        .filter(|properties| !properties.is_empty());
    if let Some(properties) = properties {
        lines.push("## Properties".to_owned());
        for property in properties {
            let field = text_of(property, "/field_name").unwrap_or_default();
            let value = match property_value(property) {
                Value::Array(values) => values
                    .iter()
                    .map(|value| {
                        value
                            .as_str()
                            .map_or_else(|| value.to_string(), ToOwned::to_owned)
                    })
                    .collect::<Vec<_>>()
                    .join(", "),
                Value::String(text) => text,
                Value::Null => String::new(),
                other => other.to_string(),
            };
            if !field.is_empty() && !value.is_empty() {
                lines.push(format!("- **{field}**: {value}"));
            }
        }
        lines.push(String::new());
    }
    if let Some(steps) = item
        .get("test_steps")
        .and_then(Value::as_array)
        .filter(|steps| !steps.is_empty())
    {
        lines.push("## Test Steps".to_owned());
        lines.push(String::new());
        for (index, step) in steps.iter().enumerate() {
            lines.push(format!("### Step {}", index + 1));
            let action = clean_html(
                step.get("description")
                    .and_then(Value::as_str)
                    .unwrap_or_default(),
            );
            let expected = clean_html(
                step.get("expected")
                    .and_then(Value::as_str)
                    .unwrap_or_default(),
            );
            if !action.is_empty() {
                lines.push(format!("**Action:** {action}"));
            }
            if !expected.is_empty() {
                lines.push(format!("**Expected Result:** {expected}"));
            }
            lines.push(String::new());
        }
    }
    let mut text = lines.join("\n");
    // Keep the SDK's trailing layout without a trailing space.
    if !text.ends_with('\n') {
        let _ = writeln!(text);
    }
    text
}

impl ContentSource for QtestSource {
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
        let mut metadata = snapshot.details_of(key);
        let content = match metadata.remove("content") {
            Some(Value::String(content)) => content,
            _ => String::new(),
        };
        let title = metadata
            .get("name")
            .and_then(Value::as_str)
            .map_or_else(|| listed.handle.clone(), ToOwned::to_owned);
        let uri = metadata
            .get("url")
            .and_then(Value::as_str)
            .map(ToOwned::to_owned);
        let mut reference = listed.reference.clone();
        reference.size = content.len() as u64;
        Ok(Document {
            title,
            uri,
            reference,
            bytes: content.into_bytes(),
            metadata,
        })
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::source::fixture::{Reply, Scripted, query};
    use crate::transport::StatusCode;
    use std::collections::BTreeMap;

    type ByModule = BTreeMap<String, Vec<Value>>;

    fn config() -> QtestConnectorConfig {
        QtestConnectorConfig::parse(
            json!({"qtest_configuration": {"base_url": "https://acme.qtestnet.com", "qtest_api_token": "s3cr3t-tkn"},
                   "qtest_project_id": 12})
            .as_object()
            .unwrap(),
        )
        .expect("config")
    }

    fn arguments(value: &Value) -> QtestIndexArguments {
        QtestIndexArguments::parse(value.as_object().unwrap()).expect("arguments")
    }

    fn allow() -> HostAllowlist {
        HostAllowlist::parse(Some("acme.qtestnet.com"))
    }

    fn case(id: u64, parent: u64, modified: &str, step: &str) -> Value {
        json!({
            "id": id, "pid": format!("TC-{id}"), "name": format!("Case {id}"),
            "description": "<p>Check <b>login</b></p><img src=\"data:image/png;base64,AAAA\">",
            "precondition": "&lt;p&gt;logged out&lt;/p&gt;",
            "parent_id": parent, "order": 1, "created_date": "2026-09-01T00:00:00+00:00",
            "last_modified_date": modified, "web_url": format!("https://acme.qtestnet.com/p/12/portal/project#tab=testdesign&object=1&id={id}"),
            "version": "1.0",
            "properties": [
                {"field_id": 1, "field_name": "Status", "field_value": "401", "field_value_name": "New"},
                {"field_id": 2, "field_name": "Tags", "field_value": "[1,2]", "field_value_name": "[smoke, ui]"},
                {"field_id": 3, "field_name": "Note", "field_value": "free text", "field_value_name": null},
            ],
            "test_steps": [{"id": 1, "order": 1, "description": step, "expected": "<p>ok</p>"}],
        })
    }

    fn modules() -> Value {
        json!([{"id": 100, "pid": "MD-1", "name": "Root", "children": [
            {"id": 101, "pid": "MD-7", "name": "Master Test Suite", "children": []}]}])
    }

    const MODULES: &str = "/api/v3/projects/12/modules";
    const CASES: &str = "/api/v3/projects/12/test-cases";

    #[test]
    fn the_configuration_and_arguments_are_strict() {
        for bad in [
            json!({}),
            json!({"qtest_configuration": {"base_url": "https://a.example", "qtest_api_token": "t"}}),
            json!({"qtest_configuration": {"base_url": "http://a.example", "qtest_api_token": "t"}, "qtest_project_id": 1}),
            json!({"qtest_configuration": {"base_url": "https://a.example", "qtest_api_token": "a b"}, "qtest_project_id": 1}),
        ] {
            assert!(
                QtestConnectorConfig::parse(bad.as_object().unwrap()).is_err(),
                "{bad}"
            );
        }
        assert!(
            QtestConnectorConfig::parse(
                json!({"qtest_configuration": {"base_url": "https://a.example", "qtest_api_token": "t"}, "project_id": "7"})
                    .as_object()
                    .unwrap()
            )
            .is_ok(),
            "the legacy project_id"
        );
        assert_eq!(arguments(&json!({})).mode, QtestMode::Full);
        for bad in [
            json!({"indexing_mode": "dql"}),
            json!({"indexing_mode": "module"}),
            json!({"indexing_mode": "all"}),
        ] {
            assert!(
                QtestIndexArguments::parse(bad.as_object().unwrap()).is_err(),
                "{bad}"
            );
        }
    }

    #[tokio::test]
    async fn the_whole_project_is_paged_and_a_case_is_the_sdk_markdown() {
        let transport = Scripted::new(|request, _| {
            let path = request.url().path();
            assert_eq!(request.url().host_str(), Some("acme.qtestnet.com"));
            assert_eq!(
                request
                    .headers()
                    .get("authorization")
                    .unwrap()
                    .to_str()
                    .unwrap(),
                "Bearer s3cr3t-tkn"
            );
            if path == MODULES {
                assert_eq!(query(request, "expand").as_deref(), Some("descendants"));
                Reply::json(&modules())
            } else if path == CASES {
                assert_eq!(query(request, "expandSteps").as_deref(), Some("true"));
                assert_eq!(query(request, "size").as_deref(), Some("100"));
                match query(request, "page").as_deref() {
                    Some("1") => Reply::json(&Value::Array(
                        (1..=100)
                            .map(|id| case(id, 101, "2026-09-02T00:00:00+00:00", "go"))
                            .collect(),
                    )),
                    Some("2") => Reply::json(&json!([case(
                        101,
                        100,
                        "2026-09-03T00:00:00+00:00",
                        "stop"
                    )])),
                    other => panic!("unexpected page {other:?}"),
                }
            } else {
                panic!("unexpected request {path}")
            }
        });
        let source = QtestSource::new(config(), arguments(&json!({})), allow(), transport).unwrap();
        let listed = source.list().await.expect("list");
        assert_eq!(listed.len(), 101);
        assert_eq!(listed[0].key, "testcase/1.md");
        assert_eq!(listed[0].mime, "text/markdown");
        let last = listed.iter().find(|r| r.key == "testcase/101.md").unwrap();
        assert_eq!(
            last.version,
            content_version("2026-09-03T00:00:00+00:00\nMD-1 Root".as_bytes())
        );
        let document = source.fetch("testcase/101.md").await.expect("fetch");
        let text = String::from_utf8(document.bytes).unwrap();
        assert!(
            text.starts_with("# Test Case: TC-101 - Case 101\n\n## Module\nMD-1 Root\n"),
            "{text}"
        );
        assert!(text.contains("## Description\n"), "{text}");
        assert!(text.contains("Check  login"), "{text}");
        assert!(!text.contains("base64"), "{text}");
        assert!(text.contains("## Precondition\n logged out"), "{text}");
        assert!(
            text.contains("- **Status**: New\n- **Tags**: smoke, ui\n- **Note**: free text"),
            "{text}"
        );
        assert!(
            text.contains("### Step 1\n**Action:** stop\n**Expected Result:**  ok"),
            "{text}"
        );
        assert_eq!(document.metadata["module_name"], "MD-1 Root");
        assert_eq!(document.metadata["status"], "New");
        assert_eq!(document.metadata["tags"], json!(["smoke", "ui"]));
        assert_eq!(document.metadata["project_id"], 12);
        assert!(!document.metadata.contains_key("content"));
        assert_eq!(document.title, "Case 101");
    }

    #[tokio::test]
    async fn a_module_is_found_by_its_full_name_and_listed_by_its_id() {
        let mut by_module = ByModule::new();
        by_module.insert("101".into(), vec![case(5, 101, "m1", "x")]);
        let transport = Scripted::new(move |request, _| {
            let path = request.url().path();
            if path == MODULES {
                Reply::json(&modules())
            } else if path == CASES {
                assert_eq!(query(request, "parentId").as_deref(), Some("101"));
                Reply::json(&json!(by_module["101"]))
            } else {
                panic!("unexpected request {path}")
            }
        });
        let source = QtestSource::new(
            config(),
            arguments(&json!({"indexing_mode": "module", "module_name": "MD-7 Master Test Suite"})),
            allow(),
            transport,
        )
        .unwrap();
        let listed = source.list().await.unwrap();
        assert_eq!(listed.len(), 1);

        let transport = Scripted::new(|request, _| {
            assert_eq!(request.url().path(), MODULES);
            Reply::json(&modules())
        });
        let source = QtestSource::new(
            config(),
            arguments(&json!({"indexing_mode": "module", "module_name": "MD-9 Nope"})),
            allow(),
            transport,
        )
        .unwrap();
        let error = source.list().await.expect_err("missing").to_string();
        assert!(error.contains("module 'MD-9 Nope' not found"), "{error}");
    }

    #[tokio::test]
    async fn dql_pages_the_search_until_there_is_no_next_link() {
        let transport = Scripted::new(|request, _| {
            let path = request.url().path();
            if path == MODULES {
                Reply::json(&modules())
            } else if path == "/api/v3/projects/12/search" {
                assert_eq!(*request.method(), Method::POST);
                let body: Value =
                    serde_json::from_slice(request.body().unwrap().as_bytes().unwrap()).unwrap();
                assert_eq!(body["object_type"], "test-cases");
                assert_eq!(body["query"], "Status = 'New'");
                assert_eq!(query(request, "appendTestSteps").as_deref(), Some("true"));
                match query(request, "page").as_deref() {
                    Some("1") => Reply::json(&json!({"page": 1, "page_size": 100, "total": 2,
                        "items": [case(1, 101, "d1", "a")],
                        "links": [{"rel": "next", "href": "..."}]})),
                    Some("2") => Reply::json(&json!({"page": 2, "page_size": 100, "total": 2,
                        "items": [case(2, 101, "d2", "b")], "links": []})),
                    other => panic!("unexpected page {other:?}"),
                }
            } else {
                panic!("unexpected request {path}")
            }
        });
        let source = QtestSource::new(
            config(),
            arguments(&json!({"indexing_mode": "dql", "dql": "Status = 'New'"})),
            allow(),
            transport,
        )
        .unwrap();
        assert_eq!(source.list().await.unwrap().len(), 2);
    }

    #[tokio::test]
    async fn a_dql_search_ending_before_its_total_is_an_error() {
        for second in [
            // An empty second page.
            json!({"page": 2, "total": 3, "items": [], "links": [{"rel": "next"}]}),
            // No next link though 2 of 3 were read.
            json!({"page": 2, "total": 3, "items": [], "links": []}),
        ] {
            let transport = Scripted::new(move |request, _| {
                let path = request.url().path();
                if path == MODULES {
                    Reply::json(&modules())
                } else {
                    match query(request, "page").as_deref() {
                        Some("1") => Reply::json(&json!({"page": 1, "total": 3,
                            "items": [case(1, 101, "d1", "a"), case(2, 101, "d2", "b")],
                            "links": [{"rel": "next", "href": "..."}]})),
                        _ => Reply::json(&second),
                    }
                }
            });
            let source = QtestSource::new(
                config(),
                arguments(&json!({"indexing_mode": "dql", "dql": "x"})),
                allow(),
                transport,
            )
            .unwrap();
            let error = source.list().await.expect_err("short").to_string();
            assert!(error.contains("total"), "{error}");
        }
    }

    #[tokio::test]
    async fn only_an_edited_case_moves_its_version() {
        let modified = Arc::new(std::sync::Mutex::new("v1".to_owned()));
        let list = |modified: &Arc<std::sync::Mutex<String>>| {
            let modified = Arc::clone(modified);
            let transport = Scripted::new(move |request, _| {
                if request.url().path() == MODULES {
                    Reply::json(&modules())
                } else {
                    Reply::json(&json!([
                        case(1, 101, "stable", "a"),
                        case(2, 101, &modified.lock().unwrap(), "b"),
                    ]))
                }
            });
            let source =
                QtestSource::new(config(), arguments(&json!({})), allow(), transport).unwrap();
            async move { source.list().await.unwrap() }
        };
        let before = list(&modified).await;
        assert_eq!(list(&modified).await, before);
        *modified.lock().unwrap() = "v2".to_owned();
        let after = list(&modified).await;
        assert_eq!(after[0], before[0]);
        assert_ne!(after[1].version, before[1].version);
    }

    #[tokio::test]
    async fn a_module_rename_moves_its_cases_versions() {
        let list = |name: &'static str| {
            let transport = Scripted::new(move |request, _| {
                if request.url().path() == MODULES {
                    Reply::json(&json!([{"id": 101, "pid": "MD-7", "name": name, "children": []}]))
                } else {
                    Reply::json(&json!([case(1, 101, "same-stamp", "a")]))
                }
            });
            let source =
                QtestSource::new(config(), arguments(&json!({})), allow(), transport).unwrap();
            async move { source.list().await.unwrap() }
        };
        let before = list("Old name").await;
        assert_eq!(list("Old name").await, before);
        assert_ne!(list("New name").await[0].version, before[0].version);
    }

    #[tokio::test]
    async fn modules_are_bounded_by_the_directory_cap_not_the_document_cap() {
        let many: Vec<Value> = (1..=20)
            .map(|id| json!({"id": id, "pid": format!("MD-{id}"), "name": "M", "children": []}))
            .collect();
        let handler = move |request: &Request, _: usize| {
            if request.url().path() == MODULES {
                Reply::json(&Value::Array(many.clone()))
            } else {
                Reply::json(&json!([case(1, 1, "s", "a")]))
            }
        };
        // 20 modules, 1 document: a document cap of 5 must not refuse them.
        let source = QtestSource::new(
            config(),
            arguments(&json!({})),
            allow(),
            Scripted::new(handler.clone()),
        )
        .unwrap()
        .with_limits(SourceLimits {
            max_documents: 5,
            ..SourceLimits::default()
        });
        assert_eq!(source.list().await.unwrap().len(), 1);
        // ...and a directory cap below that does.
        let source = QtestSource::new(
            config(),
            arguments(&json!({})),
            allow(),
            Scripted::new(handler),
        )
        .unwrap()
        .with_limits(SourceLimits {
            max_directories: 5,
            ..SourceLimits::default()
        });
        assert!(source.list().await.is_err());
    }

    #[tokio::test]
    async fn a_refused_credential_is_named_and_a_rate_limit_backs_off() {
        for status in [StatusCode::UNAUTHORIZED, StatusCode::FORBIDDEN] {
            let source = QtestSource::new(
                config(),
                arguments(&json!({})),
                allow(),
                Scripted::new(move |_, _| Reply::status(status)),
            )
            .unwrap();
            let text = source.list().await.expect_err("refused").to_string();
            assert!(text.contains("refused the credential"), "{text}");
            assert!(!text.contains("s3cr3t"), "{text}");
        }
        let source = QtestSource::new(
            config(),
            arguments(&json!({})),
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
            QtestSource::new(
                config(),
                arguments(&json!({})),
                HostAllowlist::parse(Some("github.com")),
                Scripted::new(|_, _| panic!("no request may be sent")),
            )
            .is_err()
        );
    }
}

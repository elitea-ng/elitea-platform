//! Azure DevOps test cases (the `ado_plans` family) as a `ContentSource`
//! (ADR-0030 I2).
//!
//! * **List.** The plan's suites (`Plans/{plan}/suites`, continuation
//!   followed; `suite_ids` narrows them), then every suite's test cases
//!   (`Plans/{plan}/Suites/{suite}/TestCase`, continuation followed). The SDK
//!   reads the first page of each; here every page is read.
//! * **Version.** The test case work item's `System.Rev`, which the test
//!   case answer carries in `workItemFields` (the listing asks for that one
//!   field, and keeps only the revision: no rendered document is held).
//! * **Document.** One markdown document per (suite, test case): the title,
//!   the description as text and the steps
//!   (`Microsoft.VSTS.TCM.Steps`) as a numbered list. The SDK indexes the raw
//!   steps XML alone (`chunking_tool='xml'`); the rendered form keeps the
//!   title and description searchable. A test case in several suites is a
//!   document in each, as in the SDK. A fetch reads the work item
//!   (`wit/workitems/{id}`), refuses it as changed when its revision is not
//!   the listed one, renders it, and holds it to the document cap.
//! * **Dependents.** None: the SDK indexes no test case attachments or
//!   shared steps, and neither does this connector (follow-up).

use std::collections::BTreeMap;
use std::sync::{Arc, Mutex};

use elitea_content_source::{ContentSource, Document, DocumentRef, SourceError};
use serde_json::{Map, Value};

use super::client::{AdoClient, AdoRequest};
use super::config::AdoToolkitConfig;
use super::text::{html_to_text, optional_u64, optional_u64_list, steps_to_markdown};
use crate::egress::HostAllowlist;
use crate::source::{
    Backoff, Failure, Listed, ListingCache, SourceHttp, SourceLimits, Timeouts, references,
};
use crate::transport::{Request, Transport};

const PROVIDER: &str = "Azure DevOps";
const TEST_PLAN_API: &str = "7.1";
const CONTINUATION: &str = "x-ms-continuationtoken";
/// The work item fields asked of each test case.
const LIST_FIELDS: &str = "System.Rev";
/// The work item fields a fetch reads to render the test case.
const FETCH_FIELDS: &str = "System.Title,System.Description,System.Rev,System.State,System.ChangedDate,Microsoft.VSTS.TCM.Steps";
const WORK_ITEMS_API: &str = "7.1";
const KEY_PREFIX: &str = "plans";

/// The `index_data` arguments of the `ado_plans` family
/// (`ado/test_plan/test_plan_wrapper.py::_index_tool_params`).
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct AdoPlansIndexParams {
    pub plan_id: u64,
    /// The suites to index; empty means every suite of the plan.
    pub suite_ids: Vec<u64>,
}

impl AdoPlansIndexParams {
    /// Read the arguments as the request carried them.
    ///
    /// # Errors
    ///
    /// No `plan_id`, or an argument of the wrong type.
    pub fn from_arguments(arguments: &Map<String, Value>) -> Result<Self, String> {
        let plan_id = optional_u64(arguments, "plan_id")?
            .ok_or_else(|| "`plan_id` is required: the test plan to index".to_owned())?;
        Ok(Self {
            plan_id,
            suite_ids: optional_u64_list(arguments, "suite_ids")?,
        })
    }
}

/// One listed test case: what a fetch needs to read it again.
#[derive(Clone, Debug)]
struct Case {
    id: u64,
    rev: u64,
    suite_id: u64,
    suite_name: String,
    title: String,
}

/// The test cases of one plan, listed through the shared client.
pub struct AdoPlansSource {
    client: AdoClient,
    http: SourceHttp,
    params: AdoPlansIndexParams,
    cache: ListingCache,
    cases: Mutex<Arc<BTreeMap<String, Case>>>,
}

impl AdoPlansSource {
    /// The configured project, every request through `allowlist`
    /// (fail-closed) and then `transport`.
    ///
    /// # Errors
    ///
    /// The organization's host is not on the allowlist.
    pub fn new(
        config: AdoToolkitConfig,
        params: AdoPlansIndexParams,
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
            cases: Mutex::new(Arc::new(BTreeMap::new())),
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

    /// Every `value` of a continuation-paged collection.
    async fn collection(
        &self,
        segments: &[&str],
        extra: &[(&'static str, &str)],
        pages: &mut usize,
    ) -> Result<Vec<Value>, Failure> {
        let mut values = Vec::new();
        let mut continuation: Option<String> = None;
        loop {
            *pages += 1;
            self.http.check_pages(*pages)?;
            let mut request = AdoRequest::get(segments, TEST_PLAN_API)
                .query_opt("continuationToken", continuation.clone());
            for (name, value) in extra {
                request = request.query(name, *value);
            }
            let (headers, body) = self.http.json(self.build(request)?).await?;
            let page = body
                .get("value")
                .and_then(Value::as_array)
                .ok_or(Failure::InvalidResponse("a test plan answer has no value"))?;
            values.extend(page.iter().cloned());
            continuation = headers
                .get(CONTINUATION)
                .and_then(|value| value.to_str().ok())
                .map(str::trim)
                .filter(|token| !token.is_empty())
                .map(ToOwned::to_owned);
            if continuation.is_none() {
                return Ok(values);
            }
        }
    }

    #[allow(clippy::too_many_lines)] // One pass: suites, cases, the rendered document.
    async fn listing(&self) -> Result<Vec<Listed>, Failure> {
        let plan = self.params.plan_id.to_string();
        let mut pages = 0;
        let suites = self
            .collection(
                &["testplan", "Plans", plan.as_str(), "suites"],
                &[],
                &mut pages,
            )
            .await?;
        let mut listed = Vec::new();
        let mut kept = BTreeMap::new();
        for suite in &suites {
            let suite_id = suite
                .get("id")
                .and_then(Value::as_u64)
                .ok_or(Failure::InvalidResponse("a test suite has no id"))?;
            if !self.params.suite_ids.is_empty() && !self.params.suite_ids.contains(&suite_id) {
                continue;
            }
            let suite_name = suite
                .get("name")
                .and_then(Value::as_str)
                .unwrap_or_default()
                .to_owned();
            let suite_text = suite_id.to_string();
            let cases = self
                .collection(
                    &[
                        "testplan",
                        "Plans",
                        plan.as_str(),
                        "Suites",
                        suite_text.as_str(),
                        "TestCase",
                    ],
                    &[("witFields", LIST_FIELDS)],
                    &mut pages,
                )
                .await?;
            for case in &cases {
                let work_item = case
                    .get("workItem")
                    .and_then(Value::as_object)
                    .ok_or(Failure::InvalidResponse("a test case has no work item"))?;
                let id = work_item
                    .get("id")
                    .and_then(Value::as_u64)
                    .ok_or(Failure::InvalidResponse("a test case work item has no id"))?;
                let fields = flatten_fields(work_item.get("workItemFields"));
                let rev = fields
                    .get("System.Rev")
                    .and_then(|rev| rev.as_u64().or_else(|| rev.as_str()?.parse().ok()))
                    .ok_or(Failure::InvalidResponse(
                        "a test case carries no System.Rev",
                    ))?;
                let title = work_item
                    .get("name")
                    .and_then(Value::as_str)
                    .or_else(|| fields.get("System.Title").and_then(Value::as_str))
                    .unwrap_or_default()
                    .to_owned();
                let key = format!("{KEY_PREFIX}/{plan}/suites/{suite_id}/{id}.md");
                listed.push(Listed::new(key.clone(), rev.to_string(), 0, id.to_string()));
                kept.insert(
                    key,
                    Case {
                        id,
                        rev,
                        suite_id,
                        suite_name: suite_name.clone(),
                        title,
                    },
                );
                self.http.check_count(listed.len())?;
            }
        }
        *self
            .cases
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner) = Arc::new(kept);
        Ok(listed)
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
}

impl AdoPlansSource {
    fn work_item_url(&self, id: u64) -> String {
        format!(
            "{}/{}/_workitems/edit/{id}",
            self.client.organization_text(),
            self.client.project()
        )
    }

    /// The test case as markdown and its metadata, read from the work item
    /// now; refused when it is no longer the listed revision or is over the
    /// document cap.
    async fn render(&self, case: &Case) -> Result<(Vec<u8>, Map<String, Value>), Failure> {
        let id_text = case.id.to_string();
        let segments = ["wit", "workitems", id_text.as_str()];
        let request =
            self.build(AdoRequest::get(&segments, WORK_ITEMS_API).query("fields", FETCH_FIELDS))?;
        let (_, body) = self.http.json(request).await?;
        let rev = body
            .get("rev")
            .and_then(Value::as_u64)
            .ok_or(Failure::InvalidResponse("the test case has no rev"))?;
        if rev != case.rev {
            return Err(Failure::Changed);
        }
        let fields = body
            .get("fields")
            .and_then(Value::as_object)
            .ok_or(Failure::InvalidResponse("the test case has no fields"))?;
        let text_field = |name: &str| fields.get(name).and_then(Value::as_str);
        let description = text_field("System.Description")
            .map(html_to_text)
            .unwrap_or_default();
        let steps = text_field("Microsoft.VSTS.TCM.Steps")
            .map(steps_to_markdown)
            .unwrap_or_default();
        let mut text = format!("# {}\n\n", case.title);
        if !description.is_empty() {
            text.push_str(&description);
            text.push_str("\n\n");
        }
        if !steps.is_empty() {
            text.push_str("## Steps\n\n");
            text.push_str(&steps);
        }
        let bytes = text.into_bytes();
        self.http.check_size(bytes.len() as u64)?;
        let plan = self.params.plan_id.to_string();
        let mut metadata = Map::new();
        metadata.insert("id".to_owned(), Value::String(id_text));
        metadata.insert("type".to_owned(), Value::String("test_case".to_owned()));
        metadata.insert("plan_id".to_owned(), Value::String(plan));
        metadata.insert(
            "suite_id".to_owned(),
            Value::String(case.suite_id.to_string()),
        );
        metadata.insert(
            "suite_name".to_owned(),
            Value::String(case.suite_name.clone()),
        );
        // The ISO change date (the SDK's `updated_on`); the revision is
        // the version, not a date.
        metadata.insert(
            "updated_on".to_owned(),
            Value::String(
                text_field("System.ChangedDate")
                    .unwrap_or_default()
                    .to_owned(),
            ),
        );
        metadata.insert("rev".to_owned(), Value::from(rev));
        if let Some(state) = text_field("System.State") {
            metadata.insert("state".to_owned(), Value::String(state.to_owned()));
        }
        Ok((bytes, metadata))
    }
}

/// `workItemFields` is a list of one-key objects (`[{"System.Rev": 3}, …]`);
/// an object is accepted too.
fn flatten_fields(value: Option<&Value>) -> Map<String, Value> {
    let mut fields = Map::new();
    match value {
        Some(Value::Array(items)) => {
            for item in items {
                if let Value::Object(object) = item {
                    for (key, value) in object {
                        fields.insert(key.clone(), value.clone());
                    }
                }
            }
        }
        Some(Value::Object(object)) => fields.clone_from(object),
        _ => {}
    }
    fields
}

impl ContentSource for AdoPlansSource {
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
        let cases = Arc::clone(
            &self
                .cases
                .lock()
                .unwrap_or_else(std::sync::PoisonError::into_inner),
        );
        let case = cases
            .get(key)
            .ok_or_else(|| SourceError::NotFound(key.to_owned()))?;
        let (bytes, metadata) = self.render(case).await.map_err(failed)?;
        let mut reference = listed.reference.clone();
        reference.size = bytes.len() as u64;
        Ok(Document {
            title: case.title.clone(),
            uri: Some(self.work_item_url(case.id)),
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

    fn config() -> AdoToolkitConfig {
        let settings = json!({
            "ado_configuration": {"organization_url": "https://dev.azure.com/contoso", "token": "pat"},
            "project": "Fabrikam",
        });
        AdoToolkitConfig::parse(settings.as_object().expect("settings")).expect("config")
    }

    fn allow() -> HostAllowlist {
        HostAllowlist::parse(Some("dev.azure.com"))
    }

    /// A test case as the listing documents it: the revision requested
    /// through `witFields`, nothing else.
    fn case(id: u64, rev: u64, title: &str) -> Value {
        json!({
            "testPlan": {"id": 7}, "testSuite": {"id": 0},
            "workItem": {"id": id, "name": title, "workItemFields": [{"System.Rev": rev}]}
        })
    }

    /// The work item as `GET wit/workitems/{id}?fields=…` answers it.
    fn work_item(id: u64, rev: u64) -> Value {
        json!({
            "id": id, "rev": rev, "url": "https://dev.azure.com/contoso/_apis/wit/workItems/1",
            "fields": {
                "System.Rev": rev,
                "System.ChangedDate": "2026-09-30T12:00:00.123Z",
                "System.Description": "<p>Checks <b>login</b></p>",
                "Microsoft.VSTS.TCM.Steps": "<steps id=\"0\" last=\"1\"><step id=\"1\" type=\"ValidateStep\"><parameterizedString isformatted=\"true\">&lt;P&gt;Open&lt;/P&gt;</parameterizedString><parameterizedString isformatted=\"true\">Opens</parameterizedString><description/></step></steps>",
                "System.State": "Design",
            }
        })
    }

    fn fixture() -> Arc<Scripted> {
        Scripted::new(|request, _| {
            let path = request.url().path();
            assert_eq!(
                query(request, "api-version").as_deref(),
                Some(TEST_PLAN_API)
            );
            match path {
                "/contoso/Fabrikam/_apis/testplan/Plans/7/suites" => {
                    match query(request, "continuationToken").as_deref() {
                        None => Reply::json(&json!({"value": [{"id": 10, "name": "Root"}]}))
                            .header(CONTINUATION, "s2"),
                        Some("s2") => Reply::json(
                            &json!({"value": [{"id": 11, "name": "Login"}, {"id": 12, "name": "Empty"}]}),
                        ),
                        other => panic!("token {other:?}"),
                    }
                }
                "/contoso/Fabrikam/_apis/testplan/Plans/7/Suites/10/TestCase" => {
                    assert_eq!(query(request, "witFields").as_deref(), Some(LIST_FIELDS));
                    Reply::json(&json!({"value": [case(100, 2, "Smoke")]}))
                }
                "/contoso/Fabrikam/_apis/testplan/Plans/7/Suites/11/TestCase" => {
                    match query(request, "continuationToken").as_deref() {
                        None => Reply::json(&json!({"value": [case(100, 2, "Smoke")]}))
                            .header(CONTINUATION, "c2"),
                        Some("c2") => Reply::json(&json!({"value": [case(101, 5, "Login works")]})),
                        other => panic!("token {other:?}"),
                    }
                }
                "/contoso/Fabrikam/_apis/testplan/Plans/7/Suites/12/TestCase" => {
                    Reply::json(&json!({"value": []}))
                }
                other => match other.strip_prefix("/contoso/Fabrikam/_apis/wit/workitems/") {
                    Some(id) => {
                        assert_eq!(
                            query(request, "fields").as_deref(),
                            Some(FETCH_FIELDS),
                            "a fetch asks for the fields the document renders"
                        );
                        let id: u64 = id.parse().expect("id");
                        let rev = if id == 101 { 5 } else { 2 };
                        Reply::json(&work_item(id, rev))
                    }
                    None => panic!("unexpected request {other}"),
                },
            }
        })
    }

    #[tokio::test]
    async fn suites_and_cases_are_walked_with_continuation_and_rendered_as_markdown() {
        let transport = fixture();
        let params = AdoPlansIndexParams::from_arguments(
            json!({"plan_id": "7"}).as_object().expect("object"),
        )
        .expect("params");
        let source =
            AdoPlansSource::new(config(), params, allow(), transport.clone()).expect("source");
        let listed = source.list().await.expect("listing");
        let keys: Vec<_> = listed
            .iter()
            .map(|d| (d.key.as_str(), d.version.as_str()))
            .collect();
        assert_eq!(
            keys,
            [
                ("plans/7/suites/10/100.md", "2"),
                ("plans/7/suites/11/100.md", "2"),
                ("plans/7/suites/11/101.md", "5"),
            ],
            "a case in two suites is two documents"
        );
        let requests = transport.seen().len();
        let document = source
            .fetch("plans/7/suites/11/101.md")
            .await
            .expect("case");
        assert_eq!(
            transport.seen().len(),
            requests + 1,
            "a fetch reads the work item; the listing kept no rendered document"
        );
        let text = String::from_utf8(document.bytes).expect("utf8");
        assert_eq!(
            text,
            "# Login works\n\nChecks login\n\n## Steps\n\n1. Open\n   Expected: Opens\n"
        );
        assert_eq!(document.metadata["suite_name"], "Login");
        assert_eq!(
            document.metadata["updated_on"], "2026-09-30T12:00:00.123Z",
            "the change date, not the revision"
        );
        assert_eq!(document.metadata["rev"], 5);
        assert_eq!(document.metadata["state"], "Design");
        assert_eq!(
            document.uri.as_deref(),
            Some("https://dev.azure.com/contoso/Fabrikam/_workitems/edit/101")
        );
    }

    #[tokio::test]
    async fn suite_ids_narrow_the_listing_and_a_missing_plan_id_is_refused() {
        let params = AdoPlansIndexParams::from_arguments(
            json!({"plan_id": 7, "suite_ids": [11]})
                .as_object()
                .expect("object"),
        )
        .expect("params");
        let source = AdoPlansSource::new(config(), params, allow(), fixture()).expect("source");
        let listed = source.list().await.expect("listing");
        assert_eq!(listed.len(), 2);
        assert!(
            listed
                .iter()
                .all(|d| d.key.starts_with("plans/7/suites/11/"))
        );
        assert!(
            AdoPlansIndexParams::from_arguments(&Map::new())
                .expect_err("plan_id")
                .contains("plan_id")
        );
        let forbidden = Scripted::new(|_, _| Reply::status(StatusCode::FORBIDDEN));
        let params =
            AdoPlansIndexParams::from_arguments(json!({"plan_id": 7}).as_object().expect("object"))
                .expect("params");
        let source = AdoPlansSource::new(config(), params, allow(), forbidden).expect("source");
        let error = source.list().await.expect_err("403");
        assert!(error.to_string().contains("HTTP 403"), "{error}");
    }

    #[tokio::test]
    async fn a_test_case_over_the_document_cap_is_refused_and_a_moved_one_is_changed() {
        let params = AdoPlansIndexParams::from_arguments(
            json!({"plan_id": 7, "suite_ids": [11]})
                .as_object()
                .expect("object"),
        )
        .expect("params");
        let source = AdoPlansSource::new(config(), params.clone(), allow(), fixture())
            .expect("source")
            .with_limits(SourceLimits {
                max_document_bytes: 20,
                ..SourceLimits::default()
            });
        source.list().await.expect("listing");
        let error = source
            .fetch("plans/7/suites/11/101.md")
            .await
            .expect_err("over the cap");
        assert!(error.to_string().contains("20"), "{error}");

        // The suite lists revision 5; the work item is now at revision 6.
        let moved = Scripted::new(|request, _| {
            let path = request.url().path();
            if path.ends_with("/suites") {
                Reply::json(&json!({"value": [{"id": 11, "name": "Login"}]}))
            } else if path.ends_with("/TestCase") {
                Reply::json(&json!({"value": [case(101, 5, "Login works")]}))
            } else {
                Reply::json(&work_item(101, 6))
            }
        });
        let source = AdoPlansSource::new(config(), params, allow(), moved).expect("source");
        source.list().await.expect("listing");
        let error = source
            .fetch("plans/7/suites/11/101.md")
            .await
            .expect_err("moved on");
        assert!(matches!(error, SourceError::Changed(_)), "{error}");
    }
}

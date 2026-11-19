//! The `analyse_jira` tools against a scripted Jira and an in-memory artifact
//! store: paging on both REST versions, the changelog read in two requests,
//! the datasets written, the metrics read back from them, the bounds and the
//! refusals.

#![allow(clippy::needless_pass_by_value, clippy::unnecessary_wraps)]

use std::collections::BTreeMap;
use std::sync::{Arc, Mutex};

use adk_core::{AdkError, Tool, ToolContext};
use adk_tool::SimpleToolContext;
use async_trait::async_trait;
use serde_json::{Value, json};

use crate::toolkits::attachment_test_support::FakePlatform;
use crate::toolkits::families::analysis_core::csv;
use crate::toolkits::families::jira::client::{
    JiraClientError, JiraClientErrorCode, JiraRead, JiraRoot,
};
use crate::toolkits::families::jira::config::JiraApiVersion;

use super::catalogue::tool_args_schema;
use super::config::AnalysisSettings;
use super::extract::JiraReader;
use super::tools::{AnalyseTool, Kind, Runner, served_selection, tool_catalog};

/// One request the scripted Jira saw.
#[derive(Clone, Debug)]
struct Call {
    root: JiraRoot,
    path: Vec<String>,
    query: BTreeMap<String, String>,
}

impl Call {
    fn param(&self, name: &str) -> Option<&str> {
        self.query.get(name).map(String::as_str)
    }

    fn is(&self, path: &[&str]) -> bool {
        self.path == path
    }
}

type Handler = dyn Fn(&Call) -> Result<JiraRead, JiraClientError> + Send + Sync;

struct FakeJira {
    version: JiraApiVersion,
    access: Option<String>,
    handler: Box<Handler>,
    calls: Mutex<Vec<Call>>,
}

impl FakeJira {
    fn new(
        version: JiraApiVersion,
        handler: impl Fn(&Call) -> Result<JiraRead, JiraClientError> + Send + Sync + 'static,
    ) -> Arc<Self> {
        Arc::new(Self {
            version,
            access: None,
            handler: Box::new(handler),
            calls: Mutex::new(Vec::new()),
        })
    }

    fn refusing_access(sentence: &str) -> Arc<Self> {
        Arc::new(Self {
            version: JiraApiVersion::V3,
            access: Some(sentence.to_owned()),
            handler: Box::new(|_| Ok(JiraRead::Refused("unreachable".to_owned()))),
            calls: Mutex::new(Vec::new()),
        })
    }

    fn calls(&self) -> Vec<Call> {
        self.calls.lock().expect("calls").clone()
    }

    fn calls_to(&self, path: &[&str]) -> Vec<Call> {
        self.calls()
            .into_iter()
            .filter(|call| call.is(path))
            .collect()
    }
}

#[async_trait]
impl JiraReader for FakeJira {
    fn rest_version(&self) -> JiraApiVersion {
        self.version
    }

    async fn check_access(&self) -> Result<Option<String>, JiraClientError> {
        Ok(self.access.clone())
    }

    async fn read(
        &self,
        root: JiraRoot,
        segments: &[&str],
        query: &[(&str, &str)],
    ) -> Result<JiraRead, JiraClientError> {
        let call = Call {
            root,
            path: segments
                .iter()
                .map(|segment| (*segment).to_owned())
                .collect(),
            query: query
                .iter()
                .map(|(name, value)| ((*name).to_owned(), (*value).to_owned()))
                .collect(),
        };
        self.calls.lock().expect("calls").push(call.clone());
        (self.handler)(&call)
    }
}

fn ok(body: Value) -> Result<JiraRead, JiraClientError> {
    Ok(JiraRead::Accepted(body))
}

fn refused(message: &str) -> Result<JiraRead, JiraClientError> {
    Ok(JiraRead::Refused(message.to_owned()))
}

/// The instance's field list: `Team` is what the toolkit's team column reads.
fn fields_ok() -> Result<JiraRead, JiraClientError> {
    ok(json!([
        {"id": "customfield_10010", "name": "Team"},
        {"id": "customfield_10020", "name": "Story Points"}
    ]))
}

fn history(id: &str, when: &str, from: &str, to: &str) -> Value {
    json!({
        "id": id,
        "created": format!("{when}.000+0000"),
        "items": [
            {"field": "status", "fromString": from, "toString": to},
            {"field": "assignee", "fromString": null, "toString": "Ada"}
        ]
    })
}

/// An issue as the search returns it. `histories` is what the search expands;
/// `total` is how many the changelog has.
fn issue(
    key: &str,
    status: &str,
    created: &str,
    resolved: Option<&str>,
    histories: Vec<Value>,
    total: usize,
) -> Value {
    let number = key.trim_start_matches("ALP-");
    json!({
        "id": format!("100{number}"),
        "key": key,
        "fields": {
            "project": {"key": "ALP", "name": "Alpha"},
            "issuetype": {"name": "Story"},
            "status": {"name": status},
            "summary": format!("Summary of {key}"),
            "created": format!("{created}.000+0000"),
            "resolutiondate": resolved.map(|time| format!("{time}.000+0000")),
            "updated": format!("{created}.000+0000"),
            "labels": [],
            "components": [],
            "subtasks": [],
            "issuelinks": [],
            "fixVersions": [],
            "customfield_10010": {"value": "red"}
        },
        "changelog": {"total": total, "histories": histories}
    })
}

fn a1() -> Value {
    issue(
        "ALP-1",
        "Done",
        "2026-01-01T09:00:00",
        Some("2026-01-10T09:00:00"),
        vec![history("1", "2026-01-03T09:00:00", "To Do", "In Progress")],
        3,
    )
}

fn a2() -> Value {
    issue(
        "ALP-2",
        "Done",
        "2026-01-02T09:00:00",
        Some("2026-01-08T09:00:00"),
        vec![
            history("4", "2026-01-04T09:00:00", "To Do", "In Progress"),
            history("5", "2026-01-08T09:00:00", "In Progress", "Done"),
        ],
        2,
    )
}

fn a3() -> Value {
    issue(
        "ALP-3",
        "Done",
        "2026-01-05T09:00:00",
        Some("2026-01-06T09:00:00"),
        vec![history("6", "2026-01-06T09:00:00", "To Do", "Done")],
        1,
    )
}

fn a4() -> Value {
    issue(
        "ALP-4",
        "In Progress",
        "2026-01-07T09:00:00",
        None,
        vec![history("7", "2026-01-09T09:00:00", "To Do", "In Progress")],
        1,
    )
}

/// A Jira Cloud (REST v3, token paging) with the four issues: two closed on
/// the first page, one on the second, one open; the first issue's changelog
/// is longer than the search shows.
fn cloud() -> Arc<FakeJira> {
    FakeJira::new(JiraApiVersion::V3, |call| {
        if call.is(&["search", "jql"]) {
            let jql = call.param("jql").unwrap_or_default();
            return if jql.contains("resolved is EMPTY") {
                ok(json!({"issues": [a4()], "isLast": true}))
            } else if call.param("nextPageToken") == Some("t2") {
                ok(json!({"issues": [a3()], "isLast": true}))
            } else {
                ok(json!({"issues": [a1(), a2()], "nextPageToken": "t2"}))
            };
        }
        if call.is(&["field"]) {
            return fields_ok();
        }
        if call.is(&["issue", "ALP-1", "changelog"]) {
            return ok(json!({
                "startAt": 1, "total": 3, "isLast": true,
                "values": [
                    history("2", "2026-01-06T09:00:00", "In Progress", "In Review"),
                    history("3", "2026-01-10T09:00:00", "In Review", "Done")
                ]
            }));
        }
        refused("not found")
    })
}

fn settings() -> AnalysisSettings {
    AnalysisSettings {
        bucket: "analyse-jira".to_owned(),
        closed_status: "Done".to_owned(),
        defect_types: vec!["Defect".to_owned()],
        custom_fields: vec![("team".to_owned(), vec!["Team".to_owned()])],
        project_keys: vec!["ALP".to_owned()],
        max_issues: 5_000,
    }
}

fn runner(jira: Arc<FakeJira>, platform: Option<&Arc<FakePlatform>>) -> Arc<Runner> {
    let mut runner = Runner::new(jira, settings(), platform.map(FakePlatform::host));
    runner.page_size = 2;
    runner.retry_delay = std::time::Duration::ZERO;
    Arc::new(runner)
}

fn tool(runner: &Arc<Runner>, name: &str) -> AnalyseTool {
    let kind = Kind::ALL
        .into_iter()
        .find(|kind| kind.name() == name)
        .unwrap_or_else(|| panic!("no tool {name}"));
    AnalyseTool::new(kind, "kit", Arc::clone(runner))
}

fn context() -> Arc<dyn ToolContext> {
    Arc::new(SimpleToolContext::new("analyse-jira").with_function_call_id("call"))
}

async fn call(runner: &Arc<Runner>, name: &str, arguments: Value) -> Value {
    tool(runner, name)
        .execute(context(), arguments)
        .await
        .unwrap_or_else(|error| panic!("{name} failed: {error:?}"))
}

async fn call_text(runner: &Arc<Runner>, name: &str, arguments: Value) -> String {
    call(runner, name, arguments)
        .await
        .as_str()
        .expect("text")
        .to_owned()
}

async fn call_error(runner: &Arc<Runner>, name: &str, arguments: Value) -> AdkError {
    match tool(runner, name).execute(context(), arguments).await {
        Err(error) => error,
        Ok(value) => panic!("{name} succeeded with {value}"),
    }
}

fn issues_arguments() -> Value {
    json!({
        "closed_issues_based_on": 2,
        "resolved_after": "2026-01-01",
        "updated_after": "2026-01-01",
        "created_after": "2026-01-01",
        "add_filter": ""
    })
}

fn stored(
    platform: &FakePlatform,
    key: &str,
) -> crate::toolkits::families::analysis_core::table::Table {
    let bytes = platform
        .get("analyse-jira", key)
        .unwrap_or_else(|| panic!("no file {key}"));
    csv::read(&bytes, 10_000).expect("a CSV")
}

fn column(
    table: &crate::toolkits::families::analysis_core::table::Table,
    name: &str,
) -> Vec<String> {
    let position = table.column(name).expect("column");
    table.rows.iter().map(|row| row[position].clone()).collect()
}

#[tokio::test]
async fn an_extraction_pages_reads_the_whole_changelog_and_writes_both_datasets() {
    let jira = cloud();
    let platform = FakePlatform::new();
    let runner = runner(Arc::clone(&jira), Some(&platform));
    let text = call_text(&runner, "get_jira_issues", issues_arguments()).await;
    assert!(
        text.contains("Data has been extracted successfully."),
        "{text}"
    );
    assert!(
        text.contains("/analyse-jira/data_work_items_alp.csv"),
        "{text}"
    );
    assert!(
        text.contains("/analyse-jira/map_statuses_alp.csv"),
        "{text}"
    );

    // Both queries, the closed one in two pages, then one changelog request
    // for the issue whose changelog the search cut.
    let searches = jira.calls_to(&["search", "jql"]);
    assert_eq!(searches.len(), 3);
    assert!(
        searches[0]
            .param("jql")
            .expect("jql")
            .contains("resolved is not EMPTY")
    );
    assert_eq!(searches[0].param("maxResults"), Some("2"));
    assert_eq!(searches[0].param("expand"), Some("changelog"));
    assert!(
        searches[0]
            .param("fields")
            .expect("fields")
            .ends_with(",customfield_10010")
    );
    assert_eq!(searches[1].param("nextPageToken"), Some("t2"));
    assert!(
        searches[2]
            .param("jql")
            .expect("jql")
            .contains("resolved is EMPTY")
    );
    let changelogs = jira.calls_to(&["issue", "ALP-1", "changelog"]);
    assert_eq!(changelogs.len(), 1);
    assert_eq!(changelogs[0].param("startAt"), Some("1"));
    assert_eq!(jira.calls_to(&["field"]).len(), 1);

    let items = stored(&platform, "data_work_items_alp.csv");
    let first = items
        .rows
        .iter()
        .filter(|row| row[0] == "ALP-1")
        .map(|row| {
            let status = items.column("status_history").expect("column");
            let days = items.column("time_in_status").expect("column");
            (row[status].clone(), row[days].clone())
        })
        .collect::<Vec<_>>();
    assert_eq!(
        first,
        [
            ("To Do".to_owned(), "2".to_owned()),
            ("In Progress".to_owned(), "3".to_owned()),
            ("In Review".to_owned(), "4".to_owned()),
            ("Done".to_owned(), String::new()),
        ]
    );
    assert_eq!(column(&items, "team")[0], "red");
    assert_eq!(
        column(&items, "request_type")
            .iter()
            .filter(|kind| *kind == "open")
            .count(),
        2,
        "the open issue has two intervals"
    );
    let statuses = stored(&platform, "map_statuses_alp.csv");
    assert_eq!(
        column(&statuses, "status_raw"),
        ["To Do", "In Progress", "Done", "In Review"]
    );
    assert_eq!(column(&statuses, "add_to_cycle_time"), ["n", "y", "n", "y"]);
}

#[tokio::test]
async fn the_metrics_tool_reads_the_datasets_back_and_writes_its_result() {
    let platform = FakePlatform::new();
    let runner = runner(cloud(), Some(&platform));
    call_text(&runner, "get_jira_issues", issues_arguments()).await;

    // Lead times 9, 6 and 1 days: mean 5.333333.
    let lead = call_text(
        &runner,
        "calculate_jira_metrics",
        json!({"metric": "lead_time", "work_items_file": "/analyse-jira/data_work_items_alp.csv"}),
    )
    .await;
    assert!(
        lead.starts_with(
            "lead_time: 1 groups, written to /analyse-jira/jira_metrics_lead_time.csv"
        ),
        "{lead}"
    );
    assert!(lead.contains("Alpha,Story,ALP,red,5.333333"), "{lead}");
    let result = stored(&platform, "jira_metrics_lead_time.csv");
    assert_eq!(
        result.columns,
        [
            "project_name",
            "issue_type",
            "project_key",
            "team",
            "lead_time"
        ]
    );

    // Cycle times 7, 4 and 0 days: mean 3.666667, with two extra aggregations.
    let cycle = call_text(
        &runner,
        "calculate_jira_metrics",
        json!({
            "metric": "cycle_time",
            "work_items_file": "/analyse-jira/data_work_items_alp.csv",
            "statuses_file": "/analyse-jira/map_statuses_alp.csv",
            "aggregate_by": ["max", "median"]
        }),
    )
    .await;
    assert!(
        cycle.contains("Alpha,Story,ALP,red,3.666667,7,4"),
        "{cycle}"
    );

    // A second run never replaces the first file.
    call_text(
        &runner,
        "calculate_jira_metrics",
        json!({"metric": "lead_time", "work_items_file": "/analyse-jira/data_work_items_alp.csv"}),
    )
    .await;
    assert!(
        platform
            .get("analyse-jira", "jira_metrics_lead_time-2.csv")
            .is_some()
    );
}

#[tokio::test]
async fn a_metric_that_needs_the_statuses_file_says_so() {
    let platform = FakePlatform::new();
    let runner = runner(cloud(), Some(&platform));
    call_text(&runner, "get_jira_issues", issues_arguments()).await;
    let said = call_text(
        &runner,
        "calculate_jira_metrics",
        json!({"metric": "cycle_time", "work_items_file": "/analyse-jira/data_work_items_alp.csv"}),
    )
    .await;
    assert!(said.contains("statuses_file"), "{said}");
    let missing = call_text(
        &runner,
        "calculate_jira_metrics",
        json!({"metric": "lead_time", "work_items_file": "/analyse-jira/nothing.csv"}),
    )
    .await;
    assert_ne!(missing, "");
    let unknown = call_text(
        &runner,
        "calculate_jira_metrics",
        json!({"metric": "velocity", "work_items_file": "/analyse-jira/data_work_items_alp.csv"}),
    )
    .await;
    assert!(unknown.contains("metric must be one of"), "{unknown}");
}

#[tokio::test]
async fn a_server_jira_pages_by_offset_until_the_total() {
    let jira = FakeJira::new(JiraApiVersion::V2, |call| {
        if call.is(&["search"]) {
            let jql = call.param("jql").unwrap_or_default();
            if jql.contains("resolved is EMPTY") {
                return ok(json!({"issues": [], "total": 0}));
            }
            return match call.param("startAt") {
                Some("0") => ok(json!({"issues": [a1(), a2()], "total": 3, "maxResults": 2})),
                Some("2") => ok(json!({"issues": [a3()], "total": 3, "maxResults": 2})),
                other => panic!("unexpected startAt {other:?}"),
            };
        }
        if call.is(&["field"]) {
            return ok(json!([{"id": "customfield_10010", "name": "Team"}]));
        }
        if call.is(&["issue", "ALP-1", "changelog"]) {
            return refused("HTTP 404 Not Found");
        }
        refused("not found")
    });
    let platform = FakePlatform::new();
    let runner = runner(Arc::clone(&jira), Some(&platform));
    let text = call_text(&runner, "get_jira_issues", issues_arguments()).await;
    assert!(
        text.contains("Data has been extracted successfully."),
        "{text}"
    );
    assert!(
        text.contains("1 issues have an incomplete status history"),
        "a Jira with no changelog endpoint is reported, not hidden: {text}"
    );
    let offsets: Vec<Option<String>> = jira
        .calls_to(&["search"])
        .iter()
        .filter(|call| {
            !call
                .param("jql")
                .unwrap_or_default()
                .contains("resolved is EMPTY")
        })
        .map(|call| call.param("startAt").map(ToOwned::to_owned))
        .collect();
    assert_eq!(offsets, [Some("0".to_owned()), Some("2".to_owned())]);
}

#[tokio::test]
async fn an_extraction_over_the_issue_cap_writes_nothing() {
    let platform = FakePlatform::new();
    let mut capped = settings();
    capped.max_issues = 2;
    let mut runner = Runner::new(cloud(), capped, Some(platform.host()));
    runner.page_size = 2;
    runner.retry_delay = std::time::Duration::ZERO;
    let runner = Arc::new(runner);
    let text = call_text(&runner, "get_jira_issues", issues_arguments()).await;
    assert!(text.contains("more than 2 issues"), "{text}");
    assert!(text.contains("wrote nothing"), "{text}");
    assert!(
        platform
            .get("analyse-jira", "data_work_items_alp.csv")
            .is_none()
    );
}

#[tokio::test]
async fn no_issues_found_writes_nothing() {
    let jira = FakeJira::new(JiraApiVersion::V3, |call| {
        if call.is(&["search", "jql"]) {
            return ok(json!({"issues": [], "isLast": true}));
        }
        fields_ok()
    });
    let platform = FakePlatform::new();
    let runner = runner(jira, Some(&platform));
    let text = call_text(&runner, "get_jira_issues", issues_arguments()).await;
    assert!(text.contains("nothing was written"), "{text}");
}

#[tokio::test]
async fn a_rate_limited_page_is_retried_and_a_refused_credential_is_told() {
    let seen = Arc::new(Mutex::new(0_u32));
    let counter = Arc::clone(&seen);
    let jira = FakeJira::new(JiraApiVersion::V3, move |call| {
        if call.is(&["search", "jql"]) {
            let mut seen = counter.lock().expect("counter");
            *seen += 1;
            if *seen == 1 {
                return Err(JiraClientError::fixture(
                    JiraClientErrorCode::RateLimited,
                    true,
                ));
            }
            return ok(json!({"issues": [], "isLast": true}));
        }
        fields_ok()
    });
    let platform = FakePlatform::new();
    let retried = runner(Arc::clone(&jira), Some(&platform));
    let text = call_text(&retried, "get_jira_issues", issues_arguments()).await;
    assert!(text.contains("nothing was written"), "{text}");
    assert_eq!(
        jira.calls_to(&["search", "jql"]).len(),
        3,
        "one retry, then the open query"
    );

    let denied = runner(
        FakeJira::refusing_access("Authentication failed: Invalid username or API key."),
        Some(&platform),
    );
    let said = call_text(&denied, "get_jira_issues", issues_arguments()).await;
    assert_eq!(said, "Authentication failed: Invalid username or API key.");

    let broken = runner(
        FakeJira::new(JiraApiVersion::V3, |_| {
            Err(JiraClientError::fixture(
                JiraClientErrorCode::DependencyUnavailable,
                false,
            ))
        }),
        Some(&platform),
    );
    let error = call_error(&broken, "get_jira_issues", issues_arguments()).await;
    assert!(format!("{error:?}").contains("unavailable"), "{error:?}");
}

#[tokio::test]
async fn a_custom_field_the_instance_does_not_have_is_named() {
    let platform = FakePlatform::new();
    let mut wrong = settings();
    wrong.custom_fields = vec![
        ("team".to_owned(), vec!["Squad".to_owned()]),
        ("points".to_owned(), vec!["story points".to_owned()]),
    ];
    let mut runner = Runner::new(cloud(), wrong, Some(platform.host()));
    runner.page_size = 2;
    let runner = Arc::new(runner);
    let text = call_text(&runner, "get_jira_issues", issues_arguments()).await;
    assert!(text.contains("Squad"), "{text}");
    assert!(
        !text.contains("story points"),
        "a name matches without case: {text}"
    );
}

#[tokio::test]
async fn the_arguments_are_checked_before_jira_is_asked() {
    let jira = cloud();
    let platform = FakePlatform::new();
    let runner = runner(Arc::clone(&jira), Some(&platform));
    let mut bad_date = issues_arguments();
    bad_date["updated_after"] = json!("last week");
    assert!(
        call_text(&runner, "get_jira_issues", bad_date)
            .await
            .contains("updated_after must be a date")
    );
    let mut bad_mode = issues_arguments();
    bad_mode["closed_issues_based_on"] = json!(3);
    assert!(
        call_text(&runner, "get_jira_issues", bad_mode)
            .await
            .contains("must be 1")
    );
    let mut ordered = issues_arguments();
    ordered["add_filter"] = json!("labels = x ORDER BY created");
    assert!(
        call_text(&runner, "get_jira_issues", ordered)
            .await
            .contains("remove the ORDER BY")
    );
    let mut keys = issues_arguments();
    keys["project_keys"] = json!("A\" OR project is not EMPTY OR \"B");
    assert!(
        call_text(&runner, "get_jira_issues", keys)
            .await
            .contains("project_keys must be project keys")
    );
    let mut stray = issues_arguments();
    stray["jql"] = json!("project = X");
    call_error(&runner, "get_jira_issues", stray).await;
    call_error(&runner, "get_jira_issues", json!("not an object")).await;
    assert!(jira.calls().is_empty(), "nothing reached Jira");
}

#[tokio::test]
async fn the_count_tool_reports_projects_and_a_summary_on_both_versions() {
    let cloud_jira = FakeJira::new(JiraApiVersion::V3, |call| {
        if call.is(&["project", "search"]) {
            return ok(json!({"values": [
                {"key": "ALP", "name": "Alpha"}, {"key": "BET", "name": "Beta"}
            ], "isLast": true}));
        }
        if call.is(&["search", "jql"]) {
            return if call.param("jql").unwrap_or_default().contains("\"ALP\"") {
                ok(json!({"issues": [{"key": "ALP-1"}, {"key": "ALP-2"}], "isLast": true}))
            } else {
                ok(json!({"issues": [{"key": "BET-1"}], "isLast": true}))
            };
        }
        refused("not found")
    });
    let runner_cloud = runner(Arc::clone(&cloud_jira), None);
    let counted = call(
        &runner_cloud,
        "get_number_off_all_issues",
        json!({"project_keys": "ALP,GONE", "after_date": "2025-01-01"}),
    )
    .await;
    assert_eq!(
        counted["projects"],
        json!([{"key": "ALP", "name": "Alpha", "issues_count": 2}])
    );
    assert_eq!(counted["projects_summary"]["issues_count"], 2);
    assert_eq!(counted["projects_summary"]["projects_count"], 1);
    assert_eq!(
        counted["projects_summary"]["unavailable_projects"],
        json!(["GONE"])
    );
    assert_eq!(counted["projects_summary"]["after_date"], "2025-01-01");

    let server = FakeJira::new(JiraApiVersion::V2, |call| {
        if call.is(&["project"]) {
            return ok(json!([
                {"key": "ALP", "name": "Alpha"}, {"key": "BET", "name": "Beta"}
            ]));
        }
        if call.is(&["search"]) {
            assert_eq!(call.param("maxResults"), Some("0"));
            return ok(
                json!({"issues": [], "total": if call.param("jql").unwrap_or_default().contains("\"ALP\"") { 7 } else { 9 }}),
            );
        }
        refused("not found")
    });
    let mut all_projects = settings();
    all_projects.project_keys = Vec::new();
    let runner_server = Arc::new(Runner::new(server, all_projects, None));
    let all = call(
        &runner_server,
        "get_number_off_all_issues",
        json!({"project_keys": null, "after_date": "2025-01-01"}),
    )
    .await;
    assert_eq!(all["projects"][0]["key"], "BET", "most issues first");
    assert_eq!(all["projects_summary"]["issues_count"], 16);
    let keys: Vec<&str> = all["projects"]
        .as_array()
        .expect("projects")
        .iter()
        .map(|project| project["key"].as_str().expect("key"))
        .collect();
    assert_eq!(keys, ["BET", "ALP"]);
}

#[tokio::test]
async fn the_statuses_the_sprints_and_the_field_overview_read_their_endpoints() {
    let jira = FakeJira::new(JiraApiVersion::V3, |call| {
        if call.is(&["status"]) {
            return ok(json!([
                {"name": "Done", "statusCategory": {"name": "Done"}},
                {"name": "To Do", "statusCategory": {"name": "To Do"}},
                {"name": "Done", "statusCategory": {"name": "Done"}}
            ]));
        }
        if call.root == JiraRoot::Agile && call.is(&["board"]) {
            assert_eq!(call.param("type"), Some("scrum"));
            assert_eq!(call.param("projectKeyOrId"), Some("ALP"));
            return ok(json!({"values": [{"id": 7}], "isLast": true}));
        }
        if call.root == JiraRoot::Agile && call.is(&["board", "7", "sprint"]) {
            return ok(json!({"values": [
                {"id": 70, "name": "Sprint 1", "state": "closed",
                 "startDate": "2026-01-05T09:00:00.000Z", "endDate": "2026-01-19T09:00:00.000Z",
                 "completeDate": "2026-01-19T10:00:00.000Z", "goal": "ship"},
                {"id": 71, "name": "Sprint 2", "state": "active", "startDate": "2026-01-19T09:00:00.000Z"}
            ], "isLast": true}));
        }
        if call.is(&["search", "jql"]) {
            assert_eq!(call.param("fields"), Some("*all"));
            return ok(json!({"issues": [
                {"key": "ALP-1", "fields": {"project": {"key": "ALP"}, "summary": "a", "labels": [], "customfield_10010": {"value": "red"}, "customfield_10020": null}},
                {"key": "ALP-2", "fields": {"project": {"key": "ALP"}, "summary": "b", "labels": ["x"], "customfield_10010": null, "customfield_10020": 3}}
            ], "isLast": true}));
        }
        if call.is(&["field"]) {
            return ok(json!([
                {"id": "customfield_10010", "name": "Team"},
                {"id": "customfield_10020", "name": "Story Points"}
            ]));
        }
        refused("not found")
    });
    let platform = FakePlatform::new();
    let runner = runner(jira, Some(&platform));

    let statuses = call(&runner, "get_jira_statuses", json!({})).await;
    assert_eq!(
        statuses,
        json!([{"name": "Done", "category": "Done"}, {"name": "To Do", "category": "To Do"}])
    );

    let sprints = call_text(&runner, "get_jira_sprints", json!({})).await;
    assert!(
        sprints.contains("/analyse-jira/data_sprints_alp.csv (2 sprints from 1 boards)"),
        "{sprints}"
    );
    let table = stored(&platform, "data_sprints_alp.csv");
    assert_eq!(column(&table, "name"), ["Sprint 1", "Sprint 2"]);
    assert_eq!(column(&table, "start_date")[0], "2026-01-05 09:00:00");
    assert_eq!(column(&table, "complete_date")[1], "");

    let overview = call(
        &runner,
        "get_jira_fields_overview",
        json!({"updated_after": "2026-01-01"}),
    )
    .await;
    assert_eq!(overview["issues_analysed"], 2);
    let fields = overview["fields"].as_array().expect("fields");
    let names: Vec<&str> = fields
        .iter()
        .map(|field| field["field"].as_str().expect("name"))
        .collect();
    assert!(
        names.contains(&"Team") && names.contains(&"Story Points"),
        "{names:?}"
    );
    assert!(
        names.contains(&"summary") && names.contains(&"project"),
        "{names:?}"
    );
    let labels = fields
        .iter()
        .find(|field| field["id"] == "labels")
        .expect("labels");
    assert_eq!(labels["used_on"], 1);
    let team = fields
        .iter()
        .find(|field| field["id"] == "customfield_10010")
        .expect("team");
    assert_eq!(team["by_project"]["ALP"], "1 (50.0%)");
}

#[tokio::test]
async fn a_board_that_has_no_sprints_is_skipped_with_its_reason() {
    let jira = FakeJira::new(JiraApiVersion::V3, |call| {
        if call.is(&["board"]) {
            return refused("HTTP 400 Bad Request: The board does not support sprints");
        }
        refused("not found")
    });
    let platform = FakePlatform::new();
    let runner = runner(jira, Some(&platform));
    let text = call_text(&runner, "get_jira_sprints", json!({"project_keys": "ALP"})).await;
    assert!(text.contains("No sprints were found"), "{text}");
    assert!(text.contains("does not support sprints"), "{text}");
}

#[test]
fn the_tools_that_need_the_artifact_store_are_left_out_of_a_run_without_one() {
    let catalog = tool_catalog();
    let needing: Vec<&str> = catalog
        .iter()
        .filter(|(_, needs)| *needs)
        .map(|(name, _)| *name)
        .collect();
    assert_eq!(
        needing,
        [
            "get_jira_issues",
            "get_jira_sprints",
            "calculate_jira_metrics"
        ]
    );
    let with_store = served_selection(&[], true).expect("selection");
    assert_eq!(with_store.len(), 6);
    let without = served_selection(&[], false).expect("selection");
    let names: Vec<&str> = without.iter().map(|kind| kind.name()).collect();
    assert_eq!(
        names,
        [
            "get_number_off_all_issues",
            "get_jira_fields_overview",
            "get_jira_statuses"
        ]
    );
    assert!(served_selection(&["get_jira_issues".into()], false).is_err());
    assert!(served_selection(&["made_up".into()], true).is_err());
    let one = served_selection(&["get_jira_statuses".into()], false).expect("selection");
    assert_eq!(one.len(), 1);
}

/// What survives of upstream's tool surface, which the tools keep exactly:
/// `tests/test_jira_analysis.py` (EliteaAI/elitea-sdk, main) calls
/// `get_number_off_all_issues(project_keys, after_date)` and
/// `get_jira_issues(project_keys, closed_issues_based_on, resolved_after,
/// updated_after, created_after, add_filter)`, expects the keys `projects` and
/// `projects_summary` and the sentence "Data has been extracted successfully.".
#[tokio::test]
async fn the_reconstructed_upstream_surface_is_kept() {
    let names: Vec<&str> = tool_catalog().iter().map(|(name, _)| *name).collect();
    assert!(names.contains(&"get_number_off_all_issues"));
    assert!(names.contains(&"get_jira_issues"));

    let properties = |tool: &str| -> Vec<String> {
        let schema = tool_args_schema(tool).expect("schema");
        let mut found: Vec<String> = schema["properties"]
            .as_object()
            .expect("properties")
            .keys()
            .cloned()
            .collect();
        found.sort();
        found
    };
    assert_eq!(
        properties("get_number_off_all_issues"),
        ["after_date", "project_keys"]
    );
    assert_eq!(
        properties("get_jira_issues"),
        [
            "add_filter",
            "closed_issues_based_on",
            "created_after",
            "project_keys",
            "resolved_after",
            "updated_after"
        ]
    );
    assert_eq!(
        tool_args_schema("get_number_off_all_issues").expect("schema")["title"],
        "GetJiraFieldsArgs"
    );
    assert_eq!(
        tool_args_schema("get_jira_issues").expect("schema")["title"],
        "GetJiraIssuesArgs"
    );

    // The test's arguments, as it passes them.
    let platform = FakePlatform::new();
    let runner = runner(cloud(), Some(&platform));
    let extracted = call_text(
        &runner,
        "get_jira_issues",
        json!({
            "project_keys": "ALP",
            "closed_issues_based_on": 1,
            "resolved_after": "2025-01-01",
            "updated_after": "2025-01-01",
            "created_after": "2025-01-01",
            "add_filter": ""
        }),
    )
    .await;
    assert!(
        extracted.contains("Data has been extracted successfully."),
        "{extracted}"
    );
}

#[test]
fn every_tool_has_an_argument_schema_and_the_catalogue_lists_the_same_tools() {
    for kind in Kind::ALL {
        let schema = tool_args_schema(kind.name()).unwrap_or_else(|| panic!("{}", kind.name()));
        assert_eq!(schema["type"], "object", "{}", kind.name());
        assert!(schema["properties"].is_object(), "{}", kind.name());
    }
    assert!(tool_args_schema("search_using_jql").is_none());
}

#[test]
fn the_catalogue_entry_lists_the_tools_the_family_serves() {
    let entry = super::catalogue::analyse_jira_type_entry();
    let mut declared: Vec<String> = entry["args_schemas"]
        .as_object()
        .expect("schemas")
        .keys()
        .cloned()
        .collect();
    declared.sort();
    let mut served: Vec<String> = tool_catalog()
        .iter()
        .map(|(name, _)| (*name).to_owned())
        .collect();
    served.sort();
    assert_eq!(declared, served);
    let mut selectable: Vec<String> =
        entry["settings"]["properties"]["selected_tools"]["items"]["enum"]
            .as_array()
            .expect("enum")
            .iter()
            .map(|name| name.as_str().expect("name").to_owned())
            .collect();
    selectable.sort();
    assert_eq!(selectable, served);
    assert_eq!(
        entry["properties"]["jira_configuration"]["configuration_types"],
        json!(["jira"]),
        "the toolkit reuses the Jira credential"
    );
    assert_eq!(entry["settings"]["required"], json!(["jira_configuration"]));
}
